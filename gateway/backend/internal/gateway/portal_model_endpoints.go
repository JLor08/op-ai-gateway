// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"sort"
	"time"
)

func (s *Server) handlePortalModels(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	// The admin management surface (?manage=1) wants the UNSUPPRESSED listing so a
	// hidden/locked model stays visible + editable. Only admins get it; a non-admin
	// passing the flag just gets the normal suppressed list (the flag is ignored, no
	// 403). Everything else (chat picker, inference /v1/models) keeps Models().
	if manageModelsRequested(r) && token.HasScope("admin") {
		writeJSON(w, http.StatusOK, s.Portal.ManageModels(r.Context(), token))
		return
	}
	writeJSON(w, http.StatusOK, s.Portal.Models(r.Context(), token))
}

// manageModelsRequested reports whether the request asked for the unsuppressed
// admin management listing via ?manage=1 or ?manage=true.
func manageModelsRequested(r *http.Request) bool {
	switch r.URL.Query().Get("manage") {
	case "1", "true":
		return true
	default:
		return false
	}
}

// rankModelServers returns a 1-based live rank per mapping id for the servers that offer `model`:
// available candidates first by descending live score, then the rest (also by descending score). A
// nil resolver or a scoring error yields nil (callers keep priority 0 on every row — never fail the
// request over a ranking hiccup). Read-only: ScoreModelServers mutates no resolver state.
func (s *Server) rankModelServers(ctx context.Context, model string) map[string]int {
	if s.Resolver == nil {
		return nil
	}
	scores, err := s.Resolver.ScoreModelServers(ctx, model, time.Now())
	if err != nil {
		return nil
	}
	sort.SliceStable(scores, func(i, j int) bool {
		if scores[i].Available != scores[j].Available {
			return scores[i].Available // available first
		}
		return scores[i].Score > scores[j].Score
	})
	ranks := make(map[string]int, len(scores))
	for i, cs := range scores {
		ranks[cs.MappingID] = i + 1
	}
	return ranks
}

// handlePortalModelServers lists the servers that offer a gateway model (?name=<model>) with the
// mapping's benchmark metrics + live loaded-state + a can_load flag. gateway:use, global (mirrors
// handlePortalModels). The model name is a query param because a gateway model name may contain '/'.
func (s *Server) handlePortalModelServers(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	rows, err := s.Portal.ModelServers(r.Context(), token, name)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("model.servers_failed", "model server list failed", ""))
		return
	}
	ranks := s.rankModelServers(r.Context(), name)
	for i := range rows {
		rows[i].Priority = ranks[rows[i].MappingID]
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

// handlePortalModelServersEvents streams the model-servers list over SSE: a `snapshot` frame, then
// an `update` frame (the full recomputed list) whenever the loaded registry signals a change, with a
// 25s heartbeat. gateway:use. The recompute reads the SAME shared LoadedModelRegistry the health
// loop + agent handler write, so a load/unload anywhere re-sends the list live.
func (s *Server) handlePortalModelServersEvents(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	token, ok := s.requireWebScope(w, r, scopeGatewayUse)
	if !ok {
		return
	}
	name := r.URL.Query().Get("name")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, apierror.Response("usage.stream_unsupported", "streaming unsupported", ""))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Clear the server WriteTimeout for this long-lived response. An unsupported
	// writer (httptest recorder) returns an error we intentionally ignore.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	compute := func() []portal.ModelServerDTO {
		rows, err := s.Portal.ModelServers(r.Context(), token, name)
		if err != nil {
			return nil
		}
		ranks := s.rankModelServers(r.Context(), name)
		for i := range rows {
			rows[i].Priority = ranks[rows[i].MappingID]
		}
		return rows
	}
	if !writePerfEvent(w, flusher, "snapshot", map[string]any{"data": compute()}) {
		return
	}

	changed, unsub := s.LoadedModels.Subscribe()
	defer unsub()
	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case _, open := <-changed:
			if !open {
				return
			}
			if !writePerfEvent(w, flusher, "update", map[string]any{"data": compute()}) {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

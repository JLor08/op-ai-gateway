// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
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
		resp := s.Portal.ManageModels(r.Context(), token)
		s.injectLoadingOnCounts(r.Context(), token, resp.Data)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp := s.Portal.Models(r.Context(), token)
	s.injectLoadingOnCounts(r.Context(), token, resp.Data)
	writeJSON(w, http.StatusOK, resp)
}

// injectLoadingOnCounts fills each row's LoadingOnCount from the volatile runtime-status
// registry, mirroring injectRuntimeModelState's split for ModelServerDTO: Service.Models/
// ManageModels always leave this zero because only the gateway layer holds the
// runtime-status registry (Service.Models has no reference to it at all -- see
// modelsResponse in portal/service.go, which builds offeredOn/loadedOn purely from
// routing.Store + LoadedModelReader).
//
// The count is registry-availability-gated ONLY, deliberately NOT gated on the
// runtime_model_probe capability the way ActiveRequests/QueueDepth/MetricsProbe/
// ContextProbe are in injectRowRuntimeState: State (the loading signal) is reported by
// ANY runtime_manager agent and predates the probe feature entirely, so gating it on
// runtime_model_probe would wrongly hide "Lädt" for older agents that legitimately
// report "starting". This mirrors injectRowRuntimeState's own unconditional `row.State
// = dto.State` line one section below.
//
// For each row, ModelServers(ctx, token, row.ID) re-resolves exactly the offering rows
// the model-servers endpoint would show this same principal (same visibility +
// resource-group rules Models()/ManageModels() already applied to produce the row in
// the first place), so the count never exceeds OfferedOnCount and never leaks a server
// this principal could not otherwise see. A synthetic model-group ID matches no real
// mapping's GatewayModelName, so ModelServers returns an empty slice and the group's
// LoadingOnCount is 0 -- no per-member aggregation, out of scope for this task.
//
// Best-effort and nil-safe throughout: a nil RuntimeStatus or Routes, a ModelServers
// error for one row, a mapping with no runtime spec, or a spec with no published
// status all just leave that row's count at 0 -- never an error, and never a reason to
// fail the whole list. Each distinct ServerID's status snapshot is fetched at most once
// across the WHOLE rows loop (byServer, shared with runtimeStatusesForServer's own
// per-request cache discipline).
func (s *Server) injectLoadingOnCounts(ctx context.Context, token auth.Token, rows []portal.ModelDTO) {
	if s.RuntimeStatus == nil || s.Routes == nil || s.Portal == nil {
		return
	}
	byServer := map[string]map[string]RuntimeStatusDTO{}
	for i := range rows {
		rows[i].LoadingOnCount = s.loadingServerCount(ctx, token, byServer, rows[i].ID)
	}
}

// loadingServerCount returns the number of DISTINCT servers offering modelID (per
// ModelServers, principal-scoped like every other row on this response) whose managed
// spec's live RuntimeStatus currently reports State == "starting". byServer is the
// injectLoadingOnCounts-shared per-server status-snapshot cache (see
// runtimeStatusesForServer). A ModelServers error, or a row whose mapping has no
// runtime spec or no published status, contributes nothing -- best-effort, never a
// reason to fail the count.
func (s *Server) loadingServerCount(ctx context.Context, token auth.Token, byServer map[string]map[string]RuntimeStatusDTO, modelID string) int {
	serverRows, err := s.Portal.ModelServers(ctx, token, modelID)
	if err != nil {
		return 0
	}
	loading := map[string]struct{}{}
	for _, row := range serverRows {
		spec, ok, err := s.Routes.RuntimeSpecByMapping(ctx, row.MappingID)
		if err != nil || !ok {
			continue
		}
		statuses := s.runtimeStatusesForServer(byServer, row.ServerID)
		if dto, ok := statuses[spec.ID]; ok && dto.State == "starting" {
			loading[row.ServerID] = struct{}{}
		}
	}
	return len(loading)
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

// injectRuntimeModelState fills each row's live State/ActiveRequests/QueueDepth/MetricsProbe/
// ContextProbe from the volatile runtime-status registry, mirroring how rankModelServers' caller injects
// Priority: Service.ModelServers always leaves these zero/empty because only the
// gateway layer holds the registry + routing store needed to resolve them.
//
// Best-effort and nil-safe throughout: a nil RuntimeStatus or Routes, a mapping with no
// runtime spec, or a spec with no published status all just leave the row's zero value —
// never an error, and never a reason to fail the whole list. Each distinct ServerID's
// status snapshot is fetched at most once (statusSnapshot copies its whole per-server
// slice), then indexed by spec id so every row in that server is a cheap map lookup.
func (s *Server) injectRuntimeModelState(ctx context.Context, rows []portal.ModelServerDTO) {
	if s.RuntimeStatus == nil || s.Routes == nil {
		return
	}
	byServer := map[string]map[string]RuntimeStatusDTO{}
	for i := range rows {
		statuses := s.runtimeStatusesForServer(byServer, rows[i].ServerID)
		s.injectRowRuntimeState(ctx, &rows[i], statuses)
	}
}

// runtimeStatusesForServer returns serverID's runtime-status snapshot indexed by spec id,
// building it from s.RuntimeStatus.statusSnapshot on first use and caching the result in
// byServer so each distinct server's snapshot (a copy of its whole per-server slice) is
// fetched at most once across the whole rows loop.
func (s *Server) runtimeStatusesForServer(byServer map[string]map[string]RuntimeStatusDTO, serverID string) map[string]RuntimeStatusDTO {
	if m, ok := byServer[serverID]; ok {
		return m
	}
	m := make(map[string]RuntimeStatusDTO)
	for _, dto := range s.RuntimeStatus.statusSnapshot(serverID) {
		m[dto.SpecID] = dto
	}
	byServer[serverID] = m
	return m
}

// injectRowRuntimeState fills row's State/ActiveRequests/QueueDepth/MetricsProbe/ContextProbe
// from statuses (row's owning server's runtime-status snapshot indexed by spec id), resolving
// row's runtime spec to find the right entry. Best-effort and nil-safe: a mapping with no
// runtime spec, or a spec with no published status, just leaves the row's zero value.
func (s *Server) injectRowRuntimeState(ctx context.Context, row *portal.ModelServerDTO, statuses map[string]RuntimeStatusDTO) {
	spec, ok, err := s.Routes.RuntimeSpecByMapping(ctx, row.MappingID)
	if err != nil || !ok {
		return // best-effort: no spec for this mapping, or lookup failed — leave zero
	}
	dto, ok := statuses[spec.ID]
	if !ok {
		return
	}
	// State (the loading indicator) is valid for any runtime_manager agent, so it is
	// injected unconditionally. Active/queue, however, are only real when the reporting
	// agent declared runtime_model_probe: for a non-probing agent they default to a
	// fabricated 0, so gate their injection on the flag and otherwise leave the row's
	// counts at their zero value (same root cause as the routing metricsOK gate).
	// MetricsProbe/ContextProbe are reachability data from that same probing agent, so
	// they ride the identical gate rather than State's unconditional one.
	row.State = dto.State
	if s.AgentFeatures.Has(row.ServerID, runtimeModelProbeFeature) {
		row.ActiveRequests = dto.ActiveRequests
		row.QueueDepth = dto.QueueDepth
		row.MetricsProbe = dto.MetricsProbe
		row.ContextProbe = dto.ContextProbe
	}
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
	s.injectRuntimeModelState(r.Context(), rows)
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
		s.injectRuntimeModelState(r.Context(), rows)
		return rows
	}
	// Subscribe BEFORE the snapshot, not after. The registry's channel is
	// buffered(1) and coalescing, so a change landing while the snapshot is
	// being computed and flushed is retained and delivered as the first update
	// (at worst a redundant one carrying the same data). Subscribing afterwards
	// left a window in which a change reached no subscriber and was dropped
	// entirely, leaving the client on a stale row until the next change or the
	// 25s heartbeat.
	changed, unsub := s.LoadedModels.Subscribe()
	defer unsub()

	if !writePerfEvent(w, flusher, "snapshot", map[string]any{"data": compute()}) {
		return
	}
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

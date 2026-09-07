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
	"op-ai-gateway/internal/routing"
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
		// UNFILTERED, matching this branch's own sibling counts:
		// ManageModels is portal.modelsResponse(suppress=false), which reads
		// the token-less activeMappingViews, so its OfferedOnCount/LoadedOn
		// are deliberately not principal-filtered. Filtering only the new
		// count here would leave one column under-reporting next to
		// neighbours that count everything. See injectLoadingOnCounts.
		s.injectLoadingOnCounts(r.Context(), token, resp.Data, false)
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp := s.Portal.Models(r.Context(), token)
	// FILTERED: Models is modelsResponse(suppress=true), which builds its
	// offered/loaded counts from visibleMappingViews (the resource-group
	// AllowedServerIDs filter), so the loading count applies the same filter.
	s.injectLoadingOnCounts(r.Context(), token, resp.Data, true)
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
// filterVisibility must match the branch this is called from, because the count has to
// agree with the SIBLING counts on the same row (OfferedOnCount, LoadedOn), which the
// portal service computes per branch: the principal-facing Service.Models builds them
// from visibleMappingViews (resource-group filtered) and the admin ?manage=1
// Service.ManageModels from the token-less activeMappingViews (deliberately
// unfiltered). Passing true applies the same AllowedServerIDs filter, still
// fail-closed; passing false skips it, which is safe precisely because that branch is
// admin-scoped and every count beside it is unfiltered too. Getting this wrong is not
// a leak either way -- it is an inconsistent column, one number filtered by a rule its
// neighbours do not apply.
//
// KNOWN LIMITATION -- group rows carry no loading count. modelsResponse aggregates a
// model GROUP's offered/loaded sets over its offerable members (groupOffered/
// groupLoaded), so a group row shows those; its loading count stays 0, because
// ModelDTO carries only IsGroup and no member list, and re-deriving the offerable
// member set here would mean duplicating modelGroupOverlay's whole rule set in the
// gateway layer (member order, loaded_only, per-member suppression) -- a second copy
// of a non-trivial rule, for a yellow count on a synthetic row. A group whose member
// is mid-launch therefore shows "Lädt" on the member's own row only. Per-token
// override ALIAS rows, by contrast, need no such re-derivation and are handled: see
// loadingCountModelKey.
//
// PERFORMANCE (Task 5 review fix, "Fix round 1"): the original implementation called
// Portal.ModelServers once per DISTINCT MODEL ROW, and ModelServers internally re-walks
// the ENTIRE server/app/mapping join from scratch on every call (activeMappingViews:
// AIServers + ApplicationsByServer per active server + MappingsByApplication per active
// app, plus AllowedServerIDs and, for non-admins, ModelSettings, plus ServerOwners per
// offering server) -- an N+1 against SQLite that multiplied the base listing's ~31
// queries by the model count (30 models x ~30 joined rows =~ 930 extra round trips per
// `GET /api/portal/models`). This version INVERTS the loop via startingServersByModel:
// it walks the runtime-status registry ONCE per request (bounded by the number of
// servers that have ever published a status frame, normally a handful, most not
// "starting"), not once per model row, so the cost no longer scales with the number of
// models in the response. See startingServersByModel's doc comment for the full cost
// accounting and how the visibility guarantee ModelServers used to provide for free is
// preserved explicitly.
//
// Best-effort and nil-safe throughout: a nil RuntimeStatus, Routes or Portal, an
// AllowedServerIDs error, a spec with no matching mapping, all just leave every row's
// count at 0 -- never an error, and never a reason to fail the whole list.
func (s *Server) injectLoadingOnCounts(ctx context.Context, token auth.Token, rows []portal.ModelDTO, filterVisibility bool) {
	if s.RuntimeStatus == nil || s.Routes == nil || s.Portal == nil {
		return
	}
	startingByModel := s.startingServersByModel(ctx, token, filterVisibility)
	if len(startingByModel) == 0 {
		return
	}
	for i := range rows {
		rows[i].LoadingOnCount = len(startingByModel[loadingCountModelKey(token, rows[i], filterVisibility)])
	}
}

// loadingCountModelKey returns the model name whose starting-server set this row's
// count comes from: the row's own id, EXCEPT for a per-token override alias row,
// which takes its target's.
//
// modelsResponse's alias overlay already copies the target's whole listing payload
// (loaded/loaded_on, offered_on_count, context size, vision, is_group) onto the alias
// row -- "the alias entry is meant to look exactly like its target's row, just filed
// under a different name" -- so the loading count belongs with them; left out, the
// alias would be the one row in the listing that understates what it does.
//
// aliasesApply mirrors the branch gate modelsResponse itself uses: aliases exist ONLY
// on the principal-facing listing (applyOverrideAliases runs under suppress==true).
// The admin ?manage=1 branch shows the system's real models, so a row id there is
// always a real model name and a token whose rule happens to be NAMED like one must
// not repoint that real row's count.
func loadingCountModelKey(token auth.Token, row portal.ModelDTO, aliasesApply bool) string {
	if !aliasesApply {
		return row.ID
	}
	if rule, ok := token.ModelOverrideRules[row.ID]; ok && rule.Offer && rule.To != "" {
		return rule.To
	}
	return row.ID
}

// startingServerCandidate is one (server, spec) pair the runtime-status registry
// currently reports as State == "starting", before the AllowedServerIDs visibility
// filter and the offering check are applied.
type startingServerCandidate struct {
	serverID string
	specID   string
}

// startingServersByModel returns map[gatewayModelName]set-of-server-NAMES for every
// server currently reporting a "starting" spec for that model, restricted to servers
// that actually OFFER it and (when filterVisibility) that this principal is allowed to
// see. injectLoadingOnCounts turns each set into a plain len() lookup per row.
//
// OFFERING (the invariant loading_on_count <= offered_on_count): a "starting" spec is
// not by itself a reason to count its server. OfferedOnCount counts the servers whose
// mapping survives activeMappingViews' four conditions (server active + not unhealthy,
// application active + reachable, mapping active); a spec can be mid-launch on a
// mapping that fails any of them -- an operator disabling the mapping does not stop
// the child -- and attributing it anyway made the yellow "Lädt" count exceed the
// "Angeboten" count it is read against. offeringServerName re-applies exactly those
// conditions per starting spec, and returns the server NAME because offeredOn is a set
// of names: counting the same unit makes the loading set a literal subset of the
// offered set instead of a differently-keyed number that merely usually agrees.
//
// COST, and why it no longer scales with the model count: this takes ONE
// statusSnapshot per server known to the registry (RuntimeStatus.serverIDs(); the
// registry only ever holds servers that have published agent-managed-runtime
// telemetry, pruned by runtimeStatusRegistry.Retain as servers are deleted, so this is
// bounded by the deployment's GPU-box count, not its model count). When that pass
// finds zero "starting" specs -- the overwhelmingly common case, since a model
// finishes starting in seconds -- it returns immediately with NO further store calls
// at all. Only when at least one starting spec exists does it pay: exactly ONE
// AllowedServerIDs call for every distinct candidate server in the WHOLE request
// (never once per model), plus a fixed handful of point reads per starting spec --
// RuntimeSpecByID, MappingByID, and offeringServerName's ApplicationByID +
// AIServerByID, all primary-key lookups, not joins -- normally 0-2 of each, since only
// a handful of specs are ever mid-launch at once. Contrast the old per-row
// Portal.ModelServers call this replaces, which re-walked the full server/app/mapping
// join once per model regardless of how many (if any) servers were starting anything;
// note also that the offering check above deliberately does NOT re-walk that join
// either, which is why it costs four point reads per starting spec instead of one
// traversal per request.
//
// VISIBILITY (must not regress): the old implementation inherited its visibility
// filtering for free by going through Portal.ModelServers, which itself calls
// AllowedServerIDs under resource-group provisioning (Resource Groups Phase 2) --
// see filterAllowedModelServerRows. This inverted pass bypasses that path entirely
// (it never calls ModelServers), so it re-applies the IDENTICAL mechanism directly:
// s.Portal.AllowedServerIDs, resolved once for the whole request instead of once per
// model row. A server the principal is not allowed to use is dropped BEFORE it can
// contribute to any model's count -- counting a restricted server here would leak its
// existence to a principal who cannot otherwise see it. On an AllowedServerIDs error
// this fails CLOSED (counts nothing) rather than falling back to "count everything",
// mirroring filterAllowedModelServerRows's own failOpen=false choice for this exact
// mechanism: an under-count is a display glitch, an over-count is a visibility leak.
// filterVisibility==false skips the filter entirely (no AllowedServerIDs call at all)
// for the admin ?manage=1 branch, whose sibling counts are unfiltered by design --
// see injectLoadingOnCounts.
func (s *Server) startingServersByModel(ctx context.Context, token auth.Token, filterVisibility bool) map[string]map[string]struct{} {
	result := map[string]map[string]struct{}{}
	var candidates []startingServerCandidate
	serverIDSet := map[string]struct{}{}
	for _, serverID := range s.RuntimeStatus.serverIDs() {
		for _, dto := range s.RuntimeStatus.statusSnapshot(serverID) {
			if dto.State != "starting" {
				continue
			}
			candidates = append(candidates, startingServerCandidate{serverID: serverID, specID: dto.SpecID})
			serverIDSet[serverID] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return result
	}

	allowed := map[string]bool{}
	if filterVisibility {
		ids := make([]string, 0, len(serverIDSet))
		for id := range serverIDSet {
			ids = append(ids, id)
		}
		var err error
		allowed, err = s.Portal.AllowedServerIDs(ctx, token, ids)
		if err != nil {
			return result
		}
	}

	for _, c := range candidates {
		if filterVisibility && !allowed[c.serverID] {
			continue
		}
		spec, ok, err := s.Routes.RuntimeSpecByID(ctx, c.specID)
		if err != nil || !ok {
			continue
		}
		mapping, err := s.Routes.MappingByID(ctx, spec.MappingID)
		if err != nil {
			continue
		}
		serverName, offering := s.offeringServerName(ctx, c.serverID, mapping)
		if !offering {
			continue
		}
		set, ok := result[mapping.GatewayModelName]
		if !ok {
			set = map[string]struct{}{}
			result[mapping.GatewayModelName] = set
		}
		set[serverName] = struct{}{}
	}
	return result
}

// offeringServerName reports whether serverID currently OFFERS mapping -- the exact
// condition portal.modelsResponse's OfferedOnCount counts -- and returns the server
// NAME it would be counted under there.
//
// The offered count is built from activeMappingViews (portal/service.go), which keeps a
// mapping only when its SERVER is active and not unhealthy, its APPLICATION is active
// and currently reachable, and the MAPPING itself is active. This re-applies those
// conditions to ONE mapping with two primary-key point reads instead of re-walking the
// whole server/application/mapping join, so the cost stays bounded by the number of
// starting specs. It must be kept in step with activeMappingViews: a condition added
// there and not here would let the loading count exceed the offered count again.
//
// The reachability source is the SAME shared AppHealthRegistry the portal service reads
// through its AppHealthReader (cmd/gateway/main.go hands one registry to both), so the
// two answers cannot disagree; nil-safe, and an application never probed counts as
// reachable, exactly as portal-side.
//
// The app must also belong to the server that published the "starting" status -- a
// spec's mapping resolving to some OTHER server's application is not a mapping view at
// all, and counting it would attribute a launch to a server that is not launching it.
func (s *Server) offeringServerName(ctx context.Context, serverID string, mapping routing.ModelMapping) (string, bool) {
	if mapping.Status != routing.ServerStatusActive {
		return "", false
	}
	app, err := s.Routes.ApplicationByID(ctx, mapping.ApplicationID)
	if err != nil || app.Status != routing.ServerStatusActive || app.ServerID != serverID {
		return "", false
	}
	if !s.AppHealth.Reachable(app.ID) {
		return "", false
	}
	server, err := s.Routes.AIServerByID(ctx, serverID)
	if err != nil || server.Status != routing.ServerStatusActive || server.HealthStatus == routing.HealthUnhealthy {
		return "", false
	}
	return server.Name, true
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

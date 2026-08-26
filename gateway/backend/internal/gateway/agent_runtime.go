// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
)

// handleAgentRuntimeConfig serves ONE ServerAgent the desired agent-managed
// runtime state for its OWN server (agent-runtime-manager spec §11): which
// model processes may run, with what command line, on which GPUs, which
// pairs may be co-resident, and the per-GPU VRAM budgets.
//
// Like handleAgentProxyRoutes, the target server comes ONLY from the agent
// token (authenticateAgent's ExtractBearerSecret -> HashSecret ->
// LookupAgentToken prologue): there is no parameter, path segment, or body
// field that can redirect the lookup, so one agent can never read another
// server's runtime configuration.
//
// A conditional GET (If-None-Match against the opaque ETag, which covers the
// exact document) answers 304 with no body -- the steady state, so an idle
// fleet does not re-fetch an unchanged configuration on every poll. The ETag
// also doubles as the WS push / file-mode schema version (design spec §10).
func (s *Server) handleAgentRuntimeConfig(w http.ResponseWriter, r *http.Request) {
	// Set on EVERY response path (before method/auth checks), matching every
	// other Bearer-token agent endpoint: this traffic flows through the public
	// listener behind fronting infra by default and must never be cached there.
	w.Header().Set("Cache-Control", "no-store")
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	principal, ok := s.authenticateAgent(w, r)
	if !ok {
		return
	}
	serverID := principal.ServerID
	if s.Portal == nil {
		writeJSON(w, http.StatusOK, portal.AgentRuntimeConfigDTO{
			GPUBudgets: []portal.AgentGPUBudgetDTO{},
			Specs:      []portal.AgentRuntimeSpecDTO{},
			Coresident: [][2]string{},
		})
		return
	}
	dto, err := s.Portal.AgentRuntimeConfig(r.Context(), serverID)
	if err != nil {
		// Deliberately static: err.Error() could carry store internals.
		slog.Error("agent runtime config derivation failed", "server_id", serverID, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("runtime_config.read_failed", "could not derive runtime config", ""))
		return
	}
	if dto.GPUBudgets == nil {
		dto.GPUBudgets = []portal.AgentGPUBudgetDTO{}
	}
	if dto.Specs == nil {
		dto.Specs = []portal.AgentRuntimeSpecDTO{}
	}
	if dto.Coresident == nil {
		dto.Coresident = [][2]string{}
	}
	for i := range dto.Specs {
		if dto.Specs[i].Args == nil {
			dto.Specs[i].Args = []string{}
		}
		if dto.Specs[i].GPUs == nil {
			dto.Specs[i].GPUs = []portal.AgentRuntimeSpecGPUDTO{}
		}
	}

	etag := `"` + dto.ETag + `"`
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), dto.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

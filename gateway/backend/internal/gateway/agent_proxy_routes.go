// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
)

// handleAgentProxyRoutes serves ONE ServerAgent the desired TLS-proxy route
// topology for its OWN server (Certificates P4 Task 7): a list of
// {listen, upstream, app_id} the agent's local reverse-proxy should run.
//
// This endpoint hands the agent DATA only -- listen/upstream ports -- never a
// command; see the design's "the gateway never delivers executable commands to
// the agent" constraint.
//
// Like handleAgentCertificate, the target server comes ONLY from the agent
// token (authenticateAgent's ExtractBearerSecret -> HashSecret ->
// LookupAgentToken prologue): there is no parameter, path segment or body
// field that can redirect the lookup, so one agent can never read another
// server's topology.
//
// A conditional GET (If-None-Match against the opaque ETag, which covers the
// exact route set) answers 304 with no body -- the steady state, so an idle
// fleet does not re-fetch an unchanged topology on every poll.
//
// On the public mux this route is wrapped by the netbird_only gate (see
// routes()); on the agent mux the NetBird-bound listener itself is the
// boundary (agentSourceRefused, applied at serveWith's dispatch point).
func (s *Server) handleAgentProxyRoutes(w http.ResponseWriter, r *http.Request) {
	// Set on EVERY response path (before method/auth checks), matching
	// agent_ca.go/agent_certificates.go: this is a Bearer-token agent endpoint
	// whose default-config traffic flows through the public listener behind
	// fronting infra, and it must never be cached there.
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
		writeJSON(w, http.StatusOK, portal.AgentProxyRoutesDTO{Routes: []portal.AgentProxyRouteDTO{}})
		return
	}
	dto, err := s.Portal.AgentProxyRoutes(r.Context(), serverID)
	if err != nil {
		// Deliberately static: err.Error() could carry store internals.
		slog.Error("agent proxy routes derivation failed", "server_id", serverID, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("proxy_routes.read_failed", "could not derive proxy routes", ""))
		return
	}
	if dto.Routes == nil {
		dto.Routes = []portal.AgentProxyRouteDTO{}
	}

	etag := `"` + dto.ETag + `"`
	w.Header().Set("ETag", etag)
	if etagMatches(r.Header.Get("If-None-Match"), dto.ETag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, dto)
}

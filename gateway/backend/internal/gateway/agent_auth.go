// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"log/slog"
	"net/http"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/auth"
)

type agentPrincipal struct {
	ServerID string
	Secret   string
}

type agentListenerContextKey struct{}

func withAgentListenerContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, agentListenerContextKey{}, true)
}

func isAgentListenerRequest(ctx context.Context) bool {
	v, _ := ctx.Value(agentListenerContextKey{}).(bool)
	return v
}

func (s *Server) authenticateAgent(w http.ResponseWriter, r *http.Request) (agentPrincipal, bool) {
	secret, ok := auth.ExtractBearerSecret(r.Header.Get("Authorization"))
	if !ok {
		slog.Debug("agent request rejected: no bearer token", "remote_addr", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, apierror.Response("auth.invalid_token", "invalid bearer token", ""))
		return agentPrincipal{}, false
	}
	serverID, ok, err := s.Routes.LookupAgentToken(r.Context(), auth.HashSecret(secret))
	if err != nil {
		slog.Error("agent request: token lookup failed", "remote_addr", r.RemoteAddr, "err", err)
		writeJSON(w, http.StatusInternalServerError, apierror.Response("agent.token_lookup_failed", "token lookup failed", ""))
		return agentPrincipal{}, false
	}
	if !ok {
		slog.Warn("agent request rejected: unknown agent token", "remote_addr", r.RemoteAddr)
		writeJSON(w, http.StatusUnauthorized, apierror.Response("auth.invalid_token", "invalid bearer token", ""))
		return agentPrincipal{}, false
	}
	// Record the transport hop, but ONLY for the mesh listener. The public
	// listener path is proxied over loopback and would misrepresent the true
	// agent transport; the mesh gate must never arm on such an observation.
	if isAgentListenerRequest(r.Context()) {
		s.AgentTransport.Report(serverID, r.TLS != nil)
	}
	return agentPrincipal{ServerID: serverID, Secret: secret}, true
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"op-ai-gateway/internal/routing"
)

// AgentProxyRoutesDTO is the P4 gateway-guided TLS-proxy topology handed to a
// ServerAgent (`GET /api/agent/v1/proxy-routes`, internal/gateway's
// handleAgentProxyRoutes). It is DATA only -- listen/upstream ports and an
// opaque app id -- never a command: see the design's "the gateway never
// delivers executable commands to the agent" constraint. ETag is an opaque
// validator over the exact route set (see agentProxyRoutesDTO), so an
// unchanged topology answers a conditional GET with 304.
type AgentProxyRoutesDTO struct {
	Routes []AgentProxyRouteDTO `json:"routes"`
	ETag   string               `json:"etag"`
}

// AgentProxyRouteDTO is one desired TLS-terminating listener the agent's local
// proxy should run. Listen is the port to bind (the application's
// ProxyListenPort); Upstream is the local plaintext application it forwards
// decrypted traffic to. AppID rides along for observability/forward
// compatibility only -- the agent's routes client deliberately does not key
// its reconcile on it (an app_id change alone must never churn a listener).
type AgentProxyRouteDTO struct {
	Listen   int    `json:"listen"`
	Upstream string `json:"upstream"`
	AppID    string `json:"app_id"`
}

// AgentProxyRoutes derives the P4 proxy-route topology for serverID -- the
// server the caller's agent token is bound to. The caller (the gateway
// handler) has already resolved serverID from that token; there is
// deliberately no parameter here that could redirect the lookup.
//
// A server OUTSIDE https-auto-switch scope (httpsSwitchInScope, which is
// always false in the byte-neutral default "manual" mode) gets an EMPTY route
// list: the agent then runs no local TLS proxy at all. For an in-scope server,
// only its proxy-switch CANDIDATES get a listener -- an ENABLED app that is http
// or already proxy-switched (isProxySwitchCandidate, shared with the Task-10
// switch reconcile). A disabled or own-TLS-https app the reconcile could never
// flip is skipped, so the agent never opens a proxy port it would not use (this
// is the per-app filter Task 7 deferred to Task 10). Each candidate gets its
// ProxyListenPort assigned (Task 5's routing.AssignProxyListenPort) if it does
// not already have one, persisted immediately via UpdateApplication so the port
// is stable across calls and matches what the switch reconcile routes to.
//
// A missing/unreadable server or settings store resolves to the same safe
// empty-routes default as out-of-scope (mirrors the NetbirdOnly/
// CertHTTPSSwitchMode "reads never fail" convention elsewhere in this file) --
// an agent must never be left thinking a route exists that a transient portal
// glitch merely failed to report. A failure reading or persisting the
// server's applications IS propagated as a real error, since a partial
// port-assignment write is exactly the kind of state corruption that must
// surface rather than be silently swallowed.
func (s *Service) AgentProxyRoutes(ctx context.Context, serverID string) (AgentProxyRoutesDTO, error) {
	if serverID == "" || s.routes == nil || s.settings == nil {
		return agentProxyRoutesDTO(nil), nil
	}
	server, err := s.routes.AIServerByID(ctx, serverID)
	if err != nil {
		return agentProxyRoutesDTO(nil), nil
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return agentProxyRoutesDTO(nil), nil
	}
	if !httpsSwitchInScope(server, CertHTTPSSwitchMode(values)) {
		return agentProxyRoutesDTO(nil), nil
	}
	apps, err := s.routes.ApplicationsByServer(ctx, serverID)
	if err != nil {
		return AgentProxyRoutesDTO{}, err
	}
	base := CertProxyListenPortBase(values)
	out := make([]AgentProxyRouteDTO, 0, len(apps))
	for i, app := range apps {
		// Only real proxy-switch candidates get a listener: an ENABLED app that is
		// http or already proxy-switched (see isProxySwitchCandidate). This is the
		// per-app filter the Task-7 derivation deferred -- it previously emitted a
		// route (and burned a ProxyListenPort) for EVERY app on an in-scope server,
		// including disabled and own-TLS-https ones the switch reconcile can never
		// flip. AssignProxyListenPort still sees the FULL app set for uniqueness, so
		// skipping a non-candidate never lets its (possibly explicit) port be reused.
		if !isProxySwitchCandidate(app) {
			continue
		}
		assigned := routing.AssignProxyListenPort(apps, app, base)
		if assigned != app.ProxyListenPort {
			app.ProxyListenPort = assigned
			if err := s.routes.UpdateApplication(ctx, app); err != nil {
				return AgentProxyRoutesDTO{}, err
			}
			apps[i] = app // keep the working set authoritative for the rest of this loop
		}
		out = append(out, AgentProxyRouteDTO{
			Listen:   assigned,
			Upstream: fmt.Sprintf("http://127.0.0.1:%d", app.Port),
			AppID:    app.ID,
		})
	}
	return agentProxyRoutesDTO(out), nil
}

// agentProxyRoutesDTO wraps routes (nil normalized to empty, never a JSON
// null) with its ETag, computed over the exact JSON-encoded route set so an
// unchanged topology -- including the empty set out-of-scope/manual servers
// always get -- produces a stable validator across calls.
func agentProxyRoutesDTO(routes []AgentProxyRouteDTO) AgentProxyRoutesDTO {
	if routes == nil {
		routes = []AgentProxyRouteDTO{}
	}
	return AgentProxyRoutesDTO{Routes: routes, ETag: agentProxyRoutesETag(routes)}
}

func agentProxyRoutesETag(routes []AgentProxyRouteDTO) string {
	raw, _ := json.Marshal(routes)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

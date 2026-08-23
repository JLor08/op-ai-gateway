// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"log/slog"
	"op-ai-gateway/internal/routing"
)

// ProxyRouteStatus is one TLS-terminating proxy route a ServerAgent reported as
// ACTUALLY running (the observed counterpart to the desired route set handed out
// by AgentProxyRoutes). Listen is the local listener port; TLSActive reports
// whether that listener currently terminates TLS. It mirrors
// gateway.ProxyRouteStatus in a portal-owned shape so the switch reconcile need
// not import internal/gateway (which imports portal); cmd/gateway adapts the
// gateway registry to ProxyRouteStatusReader below.
type ProxyRouteStatus struct {
	Listen    int
	TLSActive bool
}

// ProxyRouteStatusReader exposes the most recent proxy-route status snapshot a
// server's ServerAgent reported, so the https-auto-switch reconcile can decide
// when it is safe to flip an application to proxied HTTPS. A nil reader (or no
// report for a server) yields nil -- treated as "no observation": never a
// forward, never a revert. Satisfied by an adapter over
// *gateway.AgentProxyStatusRegistry (see cmd/gateway).
type ProxyRouteStatusReader interface {
	ProxyRouteStatuses(serverID string) []ProxyRouteStatus
}

// isProxySwitchCandidate reports whether app is eligible for proxy-HTTPS
// switching: it must be ENABLED (not disabled) AND either currently http, or
// already proxy-switched (https with a non-zero ProxyListenPort). An app
// manually set to https with NO proxy port (ProxyListenPort == 0) runs its own
// TLS and is deliberately NOT a candidate -- the reconcile leaves it alone.
// AgentProxyRoutes uses the same predicate so the agent only ever opens proxy
// listeners for apps the reconcile could actually switch (never for a disabled
// or own-TLS app).
func isProxySwitchCandidate(app routing.Application) bool {
	if app.Status == routing.ServerStatusDisabled {
		return false
	}
	switch effectiveScheme(app) {
	case "http":
		return true
	case "https":
		return app.ProxyListenPort != 0
	default:
		return false
	}
}

// effectiveScheme resolves an application's scheme, defaulting an empty value to
// "http" exactly as routing.ApplicationEndpoint does, so the candidate and
// switch logic agree with how the endpoint is actually composed.
func effectiveScheme(app routing.Application) string {
	if app.Scheme == "" {
		return "http"
	}
	return app.Scheme
}

// ReconcileHTTPSSwitch runs one https-auto-switch pass over every server. What
// it does per server depends on whether that server is IN SCOPE for the
// resolved cert_https_switch_mode (httpsSwitchInScope):
//
// IN SCOPE -- the status-driven pass over its proxy-switch candidates:
//   - FORWARD: flips an http candidate to https once the agent reports its
//     ProxyListenPort listener terminating TLS (an explicit tls_active=true for
//     that exact port). Routing then targets the proxy TLS port via
//     routing.ApplicationEndpoint's proxied-HTTPS branch.
//   - REVERT: flips a proxy-switched https candidate back to http ONLY on an
//     EXPLICIT reported tls_active=false for its ProxyListenPort. A MISSING route
//     (server never reported, empty snapshot, or that port absent from the
//     snapshot) is NOT a revert -- an agent that merely went silent must never
//     tear down a working switch.
//
// OUT OF SCOPE (an "exclude"d server under auto, a server lacking "include"
// under selected, or the whole fleet in "manual" mode) -- the SCOPE-EXIT revert,
// which is UNCONDITIONAL and does NOT consult the proxy status snapshot:
//   - Every https app with a non-zero ProxyListenPort is reverted to http.
//
// The scope-exit revert is what makes a narrowing action safe: when a server
// leaves scope, AgentProxyRoutes returns an empty route set for it, so the agent
// drains and CLOSES its TLS proxy listeners. That teardown is INDISTINGUISHABLE
// from an agent going silent (both surface as a missing route, the never-revert
// case) -- so the status-driven pass alone would strand an already-switched app
// pointing routing.ApplicationEndpoint at a now-dead proxy port (connection
// refused for every request AND the health probe -> the app is dropped from
// routing, fleet-wide, on a single auto->manual toggle, with no auto-recovery).
// Because the gateway ITSELF tore the route down here, reverting is a deliberate
// gateway decision, not a guess about a silent agent -- hence unconditional. It
// is idempotent: an already-http app fails the predicate, so later ticks are
// no-ops (no flap). This is why "manual" mode must still enumerate the fleet
// (it can never forward, but it MUST be able to revert a leftover switch).
//
// Writes go through the routing application updater (persistApplicationSchemeSwitch)
// so the NetBird access-policy observer stays consistent with the routed port
// set. Reads never fail into a switch: a settings/store glitch skips this pass
// (or the affected server) and the next tick retries.
func (s *Service) ReconcileHTTPSSwitch(ctx context.Context) {
	if s.routes == nil || s.settings == nil {
		return
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		return
	}
	mode := CertHTTPSSwitchMode(values)
	// NOTE: we do NOT early-return in "manual" mode. Manual can never FORWARD, but
	// it must still enumerate the fleet to REVERT any app left proxy-switched by a
	// prior auto/selected run (scope-exit revert below); the extra AIServers read
	// in the byte-neutral default is a deliberate correctness-over-micro-opt trade.
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		return
	}
	for _, server := range servers {
		apps, err := s.routes.ApplicationsByServer(ctx, server.ID)
		if err != nil {
			// A per-server read glitch must not abort the whole fleet pass; the
			// other servers still reconcile and this one retries next tick.
			slog.Debug("https auto-switch: list applications failed", "server", server.ID, "err", err)
			continue
		}
		if !httpsSwitchInScope(server, mode) {
			s.revertScopeExit(ctx, server, apps)
			continue
		}
		statuses := s.proxyRouteStatuses(server.ID)
		for _, app := range apps {
			if !isProxySwitchCandidate(app) {
				continue
			}
			if app.ProxyListenPort == 0 {
				// No proxy port assigned yet (AgentProxyRoutes assigns it when the
				// agent first fetches its routes): nothing observable to switch on.
				continue
			}
			tlsActive, present := proxyRouteTLSActive(statuses, app.ProxyListenPort)
			if !present {
				// Missing route: never forward, never revert (revert-only-on-explicit-false).
				continue
			}
			switch {
			case tlsActive && effectiveScheme(app) == "http":
				if err := s.persistApplicationSchemeSwitch(ctx, server, app, "https"); err != nil {
					slog.Warn("https auto-switch forward failed", "app", app.ID, "err", err)
				}
			case !tlsActive && effectiveScheme(app) == "https":
				if err := s.persistApplicationSchemeSwitch(ctx, server, app, "http"); err != nil {
					slog.Warn("https auto-switch revert failed", "app", app.ID, "err", err)
				}
			}
		}
	}
}

// revertScopeExit unconditionally reverts every proxy-switched app on an
// out-of-scope server back to http (see ReconcileHTTPSSwitch's OUT OF SCOPE
// case). It deliberately does NOT read the proxy status snapshot: the gateway
// already withdrew this server's routes (AgentProxyRoutes returns []), so the
// agent has torn down the TLS listeners and the switch MUST be undone regardless
// of what a stale/absent snapshot says. The effectiveScheme=="https" &&
// ProxyListenPort!=0 predicate makes it idempotent (an already-http app, or an
// own-TLS https app with ProxyListenPort==0, is skipped) so repeated ticks do
// not flap.
func (s *Service) revertScopeExit(ctx context.Context, server routing.AIServer, apps []routing.Application) {
	for _, app := range apps {
		if effectiveScheme(app) != "https" || app.ProxyListenPort == 0 {
			continue
		}
		if err := s.persistApplicationSchemeSwitch(ctx, server, app, "http"); err != nil {
			slog.Warn("https auto-switch scope-exit revert failed", "app", app.ID, "err", err)
		}
	}
}

// proxyRouteStatuses is the nil-safe accessor for the injected proxy-status
// reader: an unwired reader (memory-driver / test deps that omit it) reports no
// observation, so the reconcile makes no change.
func (s *Service) proxyRouteStatuses(serverID string) []ProxyRouteStatus {
	if s.proxyStatus == nil {
		return nil
	}
	return s.proxyStatus.ProxyRouteStatuses(serverID)
}

// proxyRouteTLSActive finds the reported status for the given listen port.
// present=false means the snapshot carried no entry for that port (the
// "missing route" case that must never trigger a revert).
func proxyRouteTLSActive(statuses []ProxyRouteStatus, listen int) (tlsActive, present bool) {
	for _, st := range statuses {
		if st.Listen == listen {
			return st.TLSActive, true
		}
	}
	return false, false
}

// persistApplicationSchemeSwitch writes a scheme flip through the routing
// application updater and then re-derives the server's NetBird access policy --
// the same store-write + observer the interactive UpdateApplication path runs,
// minus the HTTP-only authorization/DTO concerns a background reconcile has no
// principal for. reconcileServerPolicy gates internally on the module and never
// errors; it matters here because a proxy-https switch changes the routed port
// (Port -> ProxyListenPort).
func (s *Service) persistApplicationSchemeSwitch(ctx context.Context, server routing.AIServer, app routing.Application, scheme string) error {
	app.Scheme = scheme
	app.UpdatedAt = s.clock().UTC()
	if err := s.routes.UpdateApplication(ctx, app); err != nil {
		return err
	}
	s.reconcileServerPolicy(ctx, server.ID)
	return nil
}

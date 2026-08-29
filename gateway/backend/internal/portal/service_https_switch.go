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
	// State is the agent's own proxy.RouteState for this route, relayed
	// verbatim: "pending_leaf", "invalid_upstream", "pending_bind_host",
	// "bind_failed", "active"; empty when the agent reported none. The
	// reconcile never branches on it -- TLSActive alone decides that. It is
	// carried so the operator-facing alert this file raises can say WHY the
	// listener is down instead of only that it is.
	State string
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
//   - DECLINE (never a revert): an EXPLICIT reported tls_active=false for a
//     proxy-switched https candidate's ProxyListenPort leaves the application on
//     https, logs the refusal, and surfaces it through
//     HTTPSSwitchUnreachableApps. It does NOT flip the application back to
//     plaintext http. See "no automatic downgrade" below. A MISSING route
//     (server never reported, empty snapshot, or that port absent from the
//     snapshot) is neither -- an agent that merely went silent says nothing
//     about its listener at all.
//
// NO AUTOMATIC DOWNGRADE. This arm used to revert to plaintext http, so a
// broken certificate degraded into unencrypted inference instead of an outage.
// It did so with no log line, no audit trail and nothing in the portal: the
// only evidence was the scheme itself, in a field nobody watches. That is the
// wrong trade for this system and the operator has decided against it -- an
// automatic switch to unencrypted is a security problem, not a mitigation. The
// gateway now keeps the application on https and lets it be unreachable until
// TLS works again or an operator moves it deliberately.
//
// The cost of that decision is availability, and it must be paid HONESTLY: the
// point is not to swap a silent downgrade for a silent outage, which is the
// same defect facing the other way. So the declined revert is loud in two
// places that do not depend on each other -- a Warn on EVERY pass that observes
// it (the reconcile cadence is 15 minutes by default, so this is a recurring
// reminder rather than a one-shot line that scrolls away, and it needs no
// transition state that could get stuck), and HTTPSSwitchUnreachableApps, which
// the certificates view renders as an alert naming the server, the application,
// the port, the agent's own reason for the listener being down, and what to do
// about it.
//
// Recovery needs no action here: the application was never moved, so when the
// agent's listener comes back the application simply works again. There is no
// forward switch to re-run and no window in which it is briefly plaintext.
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
			status, present := proxyRouteStatusFor(statuses, app.ProxyListenPort)
			if !present {
				// Missing route: the agent said nothing about this listener, so
				// there is nothing to forward on and nothing to declare broken.
				continue
			}
			switch {
			case status.TLSActive && effectiveScheme(app) == "http":
				if err := s.persistApplicationSchemeSwitch(ctx, server, app, "https"); err != nil {
					slog.Warn("https auto-switch forward failed", "app", app.ID, "err", err)
				}
			case !status.TLSActive && effectiveScheme(app) == "https":
				// Declined, not reverted. Logged every pass on purpose: this is
				// an outage, the operator may not have been watching when it
				// started, and a recurring line is what a 3am page is built on.
				slog.Warn("https auto-switch: refusing to downgrade an application to plaintext http; it stays https and is UNREACHABLE until TLS is restored",
					"server", server.ID,
					"server_name", server.Name,
					"app", app.ID,
					"app_type", app.Type,
					"proxy_listen_port", app.ProxyListenPort,
					"agent_route_state", status.State,
					"action", httpsSwitchUnreachableAction(status.State))
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
//
// THIS IS THE ONE AUTOMATIC MOVE TO PLAINTEXT THAT SURVIVES the
// no-automatic-downgrade policy, and it is kept deliberately rather than
// overlooked. State the difference plainly, because it is the whole
// justification:
//
//   - On a tls_active=false report NOBODY ASKED FOR ANYTHING. A leaf expired,
//     a port got taken, a file went missing. Answering an environment failure
//     by turning encryption off is precisely the security problem the policy
//     exists to prevent, and the application recovers on its own the moment
//     TLS is back, so the outage is bounded by a fixable cause.
//   - Here an OPERATOR performed an explicit portal action -- excluding the
//     server, narrowing the mode to selected, or moving the fleet to manual --
//     whose documented and only effect is "this server no longer runs the
//     gateway-guided TLS proxy". The gateway ITSELF then withdrew the routes
//     and the agent tore the listeners down. This revert is the completion of
//     the operator's own instruction, not a decision the gateway took about a
//     failure.
//
// And the alternative is worse in a way the other case is not. Left on https,
// the application points at a port that is genuinely gone, with NO path back:
// the status-driven pass does not run for an out-of-scope server, a torn-down
// route is MISSING from the snapshot rather than an explicit false, and there
// is no proxy_listen_port field in the portal UI, so the rescue is API-only and
// has to set scheme and port in one PATCH. That is a permanent outage produced
// by a routine narrowing action -- fleet-wide, on a single auto->manual toggle.
//
// What it is NOT allowed to be is silent, which until now it was: the write
// went through persistApplicationSchemeSwitch with a log line only on FAILURE.
// It says so at Warn now, every time it moves an application.
func (s *Service) revertScopeExit(ctx context.Context, server routing.AIServer, apps []routing.Application) {
	for _, app := range apps {
		if effectiveScheme(app) != "https" || app.ProxyListenPort == 0 {
			continue
		}
		if err := s.persistApplicationSchemeSwitch(ctx, server, app, "http"); err != nil {
			slog.Warn("https auto-switch scope-exit revert failed", "app", app.ID, "err", err)
			continue
		}
		slog.Warn("https auto-switch: server left https-auto-switch scope; reverting its application to plaintext http because the gateway has withdrawn the TLS proxy route it was using",
			"server", server.ID,
			"server_name", server.Name,
			"app", app.ID,
			"app_type", app.Type,
			"proxy_listen_port", app.ProxyListenPort,
			"action", "traffic to this application is now UNENCRYPTED; put the server back in https-auto-switch scope, or give the application its own TLS (set scheme https with proxy_listen_port 0)")
	}
}

// HTTPSSwitchUnreachableDTO is one application the gateway is REFUSING to
// downgrade: proxy-switched to https, on a server in https-auto-switch scope,
// whose agent explicitly reports its proxy listener not terminating TLS. The
// application is unreachable until that is fixed, and this is what says so in
// the portal.
//
// It carries no certificate material, no upstream address and no token -- an
// identity, a port, the agent's own reason, and the remedy.
type HTTPSSwitchUnreachableDTO struct {
	ServerID        string `json:"server_id"`
	ServerName      string `json:"server_name"`
	AppID           string `json:"app_id"`
	AppType         string `json:"app_type"`
	ProxyListenPort int    `json:"proxy_listen_port"`
	// RouteState is the agent's proxy.RouteState verbatim ("pending_leaf",
	// "bind_failed", "invalid_upstream", "pending_bind_host"); empty when the
	// agent reported none. Action turns it into an instruction.
	RouteState string `json:"route_state,omitempty"`
	Action     string `json:"action"`
}

// HTTPSSwitchUnreachableApps lists every application currently in that state,
// fleet-wide. Never returns nil.
//
// It is DERIVED from the same three inputs the reconcile itself reads -- the
// switch mode, the applications, and the agent's proxy-route status snapshot --
// rather than from a side table the reconcile writes. That is deliberate, and
// it is the same reasoning GatewayCARotationPendingServers gives for reusing
// gatewayTrustPropagation: a portal view and a reconcile that maintain separate
// opinions of the same condition eventually disagree, and the operator has no
// way to tell which one is lying. Here they cannot: the predicate below is the
// reconcile's DECLINE arm, spelled once.
//
// Reads never fail into a false alarm: a settings or store glitch skips the
// affected server (or the whole pass) and reports nothing for it, exactly as
// the reconcile does.
func (s *Service) HTTPSSwitchUnreachableApps(ctx context.Context) []HTTPSSwitchUnreachableDTO {
	out := []HTTPSSwitchUnreachableDTO{}
	if s.routes == nil || s.settings == nil {
		return out
	}
	values, err := s.settings.SystemSettings(ctx)
	if err != nil {
		slog.Debug("https auto-switch: unreachable-app view skipped, settings unreadable", "err", err)
		return out
	}
	mode := CertHTTPSSwitchMode(values)
	servers, err := s.routes.AIServers(ctx)
	if err != nil {
		slog.Debug("https auto-switch: unreachable-app view skipped, fleet unreadable", "err", err)
		return out
	}
	for _, server := range servers {
		// Out of scope is not this condition: revertScopeExit resolves those
		// on the same pass, so alerting on them would name a state the
		// operator cannot and need not act on.
		if !httpsSwitchInScope(server, mode) {
			continue
		}
		apps, err := s.routes.ApplicationsByServer(ctx, server.ID)
		if err != nil {
			slog.Debug("https auto-switch: unreachable-app view skipped a server", "server", server.ID, "err", err)
			continue
		}
		statuses := s.proxyRouteStatuses(server.ID)
		for _, app := range apps {
			if !isProxySwitchCandidate(app) || app.ProxyListenPort == 0 || effectiveScheme(app) != "https" {
				continue
			}
			status, present := proxyRouteStatusFor(statuses, app.ProxyListenPort)
			if !present || status.TLSActive {
				continue
			}
			out = append(out, HTTPSSwitchUnreachableDTO{
				ServerID:        server.ID,
				ServerName:      server.Name,
				AppID:           app.ID,
				AppType:         app.Type,
				ProxyListenPort: app.ProxyListenPort,
				RouteState:      status.State,
				Action:          httpsSwitchUnreachableAction(status.State),
			})
		}
	}
	return out
}

// httpsSwitchUnreachableAction turns the agent's reported RouteState into an
// instruction. "The message must say what to do, not just what happened": an
// operator reading "pending_bind_host" at 3am should not also have to find the
// agent source to learn that it means the leaf has no usable SAN.
//
// An unrecognised (or absent) state falls back to the generic remedy rather
// than to silence -- a future agent state must never produce an alert that
// says nothing.
func httpsSwitchUnreachableAction(state string) string {
	switch state {
	case "pending_leaf":
		return "the agent has no certificate installed yet: check that cert_mode is proxy with a writable cert_dir, and that this server's certificate has been issued"
	case "bind_failed":
		return "the agent could not bind the proxy port: find what else is holding it on that host, or change the application's proxy_listen_port"
	case "pending_bind_host":
		return "the agent's certificate carries no usable IP/DNS SAN, so it has no safe address to bind: re-issue this server's certificate with its mesh address"
	case "invalid_upstream":
		return "the route's upstream URL is malformed: check the application's port, and any cert_proxy_routes override on the agent"
	default:
		return "check the server-agent log on this host for why its TLS proxy listener is not serving; the application stays https and unreachable until it is, or until an operator changes it deliberately"
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

// proxyRouteStatusFor finds the reported status for the given listen port.
// present=false means the snapshot carried no entry for that port (the
// "missing route" case, which is neither a forward nor a declared failure).
// The whole status is returned, not just TLSActive, so the caller can report
// the agent's State as the reason.
func proxyRouteStatusFor(statuses []ProxyRouteStatus, listen int) (status ProxyRouteStatus, present bool) {
	for _, st := range statuses {
		if st.Listen == listen {
			return st, true
		}
	}
	return ProxyRouteStatus{}, false
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

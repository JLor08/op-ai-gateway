// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "sync"

// ProxyRouteStatus is one TLS-terminating reverse-proxy route the ServerAgent
// reported as ACTUALLY running (Certificates P4 Task 2's proxy.Manager.Status(),
// relayed over telemetry as sample.ProxyRouteSample). Listen is the local
// listener port; TLSActive reports whether that listener currently terminates
// TLS (vs. plaintext passthrough/fallback). It is the observed counterpart to
// the DESIRED route set the gateway hands the agent via handleAgentProxyRoutes
// (agent_proxy_routes.go) — the switch reconcile (Certificates P4 Task 10)
// compares the two to decide when it is safe to flip a server's public
// listener to TLS-only.
type ProxyRouteStatus struct {
	Listen    int
	TLSActive bool
	// State is the agent's own proxy.RouteState for this route, relayed
	// verbatim and never interpreted here: "pending_leaf",
	// "invalid_upstream", "pending_bind_host", "bind_failed", "active".
	// Empty means the agent did not report one. It is what turns a
	// TLSActive=false into something an operator can act on.
	State string
}

// AgentProxyStatusRegistry remembers, per server, the most recently reported
// snapshot of proxy-route statuses. It is in-memory, nil-safe, and mirrors the
// shape of AgentTransportRegistry / AgentCertReportRegistry: ONE shared
// instance written by the agent-telemetry ingest path (ingestTelemetrySample),
// read by whatever gates on it (the Task 10 switch reconcile), and pruned by
// the app-health loop.
type AgentProxyStatusRegistry struct {
	mu     sync.RWMutex
	status map[string][]ProxyRouteStatus
}

// NewAgentProxyStatusRegistry builds a fresh, empty registry.
func NewAgentProxyStatusRegistry() *AgentProxyStatusRegistry {
	return &AgentProxyStatusRegistry{status: make(map[string][]ProxyRouteStatus)}
}

// Report stamps the observed route statuses for serverID, REPLACING whatever
// was reported before — each telemetry sample is a full snapshot of the
// agent's proxy.Manager.Status(), not a delta. No-op on a nil registry or an
// empty id. A nil/empty routes stores nil (not an empty-but-non-nil slice), so
// an agent that never sends proxy_routes reports cleanly as "no routes" rather
// than as a distinguishable empty report. Otherwise routes is defensively
// copied so a caller's later mutation of its backing slice can never
// retroactively change what was stored.
//
// It returns becameTLSActive: true when a listener terminates TLS now that did
// NOT in the previous snapshot (an UPWARD tls_active edge). That edge is what
// unblocks the https-auto-switch — ReconcileHTTPSSwitch gates the public flip
// on this registry — so the ingest caller pokes an immediate switch reconcile
// on a true return, landing the flip seconds after the agent is TLS-ready
// instead of on the next timed pass (issue #104). A steady TLS-active listener,
// or one going inactive, is not an edge and returns false, so the poke fires at
// most once per activation rather than on every telemetry sample.
func (r *AgentProxyStatusRegistry) Report(serverID string, routes []ProxyRouteStatus) (becameTLSActive bool) {
	if r == nil || serverID == "" {
		return false
	}
	var cp []ProxyRouteStatus
	if len(routes) > 0 {
		cp = make([]ProxyRouteStatus, len(routes))
		copy(cp, routes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prevActive := tlsActiveListenPorts(r.status[serverID])
	for _, rt := range cp {
		if rt.TLSActive && !prevActive[rt.Listen] {
			becameTLSActive = true
			break
		}
	}
	r.status[serverID] = cp
	return becameTLSActive
}

// tlsActiveListenPorts returns the set of Listen ports whose route currently
// terminates TLS. nil for no such ports, so a lookup on the result is always
// safe (a nil-map read is false).
func tlsActiveListenPorts(routes []ProxyRouteStatus) map[int]bool {
	if len(routes) == 0 {
		return nil
	}
	set := make(map[int]bool, len(routes))
	for _, rt := range routes {
		if rt.TLSActive {
			set[rt.Listen] = true
		}
	}
	return set
}

// Status returns the last reported route statuses for serverID, or nil when
// the server has never reported (or the registry is nil). The returned slice
// is a copy so a caller can never mutate registry state through it.
func (r *AgentProxyStatusRegistry) Status(serverID string) []ProxyRouteStatus {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	routes := r.status[serverID]
	if routes == nil {
		return nil
	}
	cp := make([]ProxyRouteStatus, len(routes))
	copy(cp, routes)
	return cp
}

// Retain evicts entries for servers not present (true) in ids, bounding
// memory the same way every other agent registry does. A nil/empty ids
// (e.g. during a transient fleet-enumeration failure upstream) clears
// everything, matching the sibling registries' Retain contract.
func (r *AgentProxyStatusRegistry) Retain(ids map[string]bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.status {
		if !ids[id] {
			delete(r.status, id)
		}
	}
}

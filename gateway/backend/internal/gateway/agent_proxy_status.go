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
func (r *AgentProxyStatusRegistry) Report(serverID string, routes []ProxyRouteStatus) {
	if r == nil || serverID == "" {
		return
	}
	var cp []ProxyRouteStatus
	if len(routes) > 0 {
		cp = make([]ProxyRouteStatus, len(routes))
		copy(cp, routes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.status[serverID] = cp
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

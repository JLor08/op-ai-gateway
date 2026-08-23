// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"time"
)

// defaultAgentPresenceWindow is used when NewAgentPresenceRegistry is given <= 0.
const defaultAgentPresenceWindow = 180 * time.Second

// AgentPresenceRegistry tracks, per server, when the ServerAgent last POSTed
// telemetry. Reporting(serverID) is true iff the last report is within the
// freshness window. In-memory, nil-safe; one shared instance is written by the
// agent-ingest handler and read by the app-health loop.
type AgentPresenceRegistry struct {
	mu     sync.RWMutex
	seen   map[string]time.Time
	window time.Duration
	now    func() time.Time
}

func NewAgentPresenceRegistry(window time.Duration) *AgentPresenceRegistry {
	if window <= 0 {
		window = defaultAgentPresenceWindow
	}
	return &AgentPresenceRegistry{
		seen:   make(map[string]time.Time),
		window: window,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Report stamps the server's last-report time. No-op on nil or empty id.
func (r *AgentPresenceRegistry) Report(serverID string) {
	if r == nil || serverID == "" {
		return
	}
	r.mu.Lock()
	r.seen[serverID] = r.now()
	r.mu.Unlock()
}

// Reporting is true iff the server reported within the freshness window.
// nil registry or never-seen server -> false.
func (r *AgentPresenceRegistry) Reporting(serverID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	at, ok := r.seen[serverID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return r.now().Sub(at) <= r.window
}

// ReportingWithin is true iff the server reported within the given window.
// nil registry or never-seen server -> false. Unlike Reporting (which uses the
// registry's own fixed window), the caller supplies the effective (per-server,
// system-default-or-override) window — see routing.EffectiveAgentPresenceTimeoutSeconds.
func (r *AgentPresenceRegistry) ReportingWithin(serverID string, window time.Duration) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	at, ok := r.seen[serverID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return r.now().Sub(at) <= window
}

// ReportReactivated stamps the server's last-report time and reports whether this
// report is an inactive->active edge: the server had NO prior report, or its prior
// report is strictly older than window (it had flipped to "inactive"). Atomic under
// the lock (read-prev, stamp-now, compare). The caller supplies the effective
// (per-server, system-default-or-override) window — see
// routing.EffectiveAgentPresenceTimeoutSeconds — so the edge lines up with the
// "Agent" status column. nil registry or empty id -> false (and no stamp).
func (r *AgentPresenceRegistry) ReportReactivated(serverID string, window time.Duration) bool {
	if r == nil || serverID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, had := r.seen[serverID]
	now := r.now()
	r.seen[serverID] = now
	return !had || now.Sub(prev) > window
}

// Retain evicts entries for servers not in the live set (bounds memory).
func (r *AgentPresenceRegistry) Retain(live map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.seen {
		if _, ok := live[id]; !ok {
			delete(r.seen, id)
		}
	}
}

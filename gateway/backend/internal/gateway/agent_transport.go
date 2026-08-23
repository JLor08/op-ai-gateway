// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"time"
)

// AgentTransportReport is the pair of most-recent transport hop timestamps the
// gateway observed for a single ServerAgent-token authentication on the mesh
// listener. LastTLSAt is set only when the authenticated request arrived over
// HTTPS/WSS; LastPlainAt only when it arrived over HTTP/WS. Either half is a
// zero time when that hop has never been observed for this server.
type AgentTransportReport struct {
	LastTLSAt   time.Time
	LastPlainAt time.Time
}

// AgentTransportRegistry remembers, per server, the last time each transport
// hop was observed on the mesh agent listener. It is in-memory, nil-safe, and
// deliberately mesh-only: only authenticateAgent-writes reach it, and only from
// the mesh listener path (isAgentListenerRequest). The public listener path
// stays out — a request that already went through the fronting reverse proxy
// arrives over the loopback and would misrepresent the true agent hop.
//
// One shared instance is written by the mesh authenticate path, read by the
// portal (the certificate list's per-server transport column + the mesh gate's
// arming check), and pruned by the app-health loop.
type AgentTransportRegistry struct {
	mu   sync.RWMutex
	seen map[string]AgentTransportReport
	now  func() time.Time
}

// NewAgentTransportRegistry builds a fresh registry with a real UTC clock.
func NewAgentTransportRegistry() *AgentTransportRegistry {
	return &AgentTransportRegistry{
		seen: make(map[string]AgentTransportReport),
		now:  func() time.Time { return time.Now().UTC() },
	}
}

// Report stamps the observed hop for serverID. No-op on nil or an empty id.
// The report intentionally keeps BOTH sides: a later plain observation must not
// erase a previous TLS mark, because AnyTLSWithin still needs to see it while
// the mesh gate is disarmed.
func (r *AgentTransportRegistry) Report(serverID string, tls bool) {
	if r == nil || serverID == "" {
		return
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := r.seen[serverID]
	if tls {
		rep.LastTLSAt = now
	} else {
		rep.LastPlainAt = now
	}
	r.seen[serverID] = rep
}

// LatestTransport returns the newer of the two hop stamps as the current
// transport verdict. ok=false is reserved for a server that has never been seen
// at all — which every caller must render as "never observed", NOT as "plain".
// The registry is in-memory, so a gateway restart legitimately erases every
// observation without changing anything on any server. The method name matches
// the portal.AgentTransportReader interface so *AgentTransportRegistry
// satisfies it directly, without a wrapper.
func (r *AgentTransportRegistry) LatestTransport(serverID string) (transport string, at time.Time, ok bool) {
	if r == nil {
		return "", time.Time{}, false
	}
	r.mu.RLock()
	rep, seen := r.seen[serverID]
	r.mu.RUnlock()
	if !seen || (rep.LastTLSAt.IsZero() && rep.LastPlainAt.IsZero()) {
		return "", time.Time{}, false
	}
	if rep.LastPlainAt.After(rep.LastTLSAt) {
		return "plain", rep.LastPlainAt, true
	}
	return "tls", rep.LastTLSAt, true
}

// AnyTLSWithin reports whether ANY server in the registry was last observed on
// TLS within (now-window, now]. Used by the mesh gate's arming precondition
// (which refuses to arm cert_mesh_require_tls until at least one fresh TLS hop
// exists) and by the informed confirm dialog. window <= 0 is a permanent NO
// (nothing is within a zero-length window), which is the safe direction.
func (r *AgentTransportRegistry) AnyTLSWithin(now time.Time, window time.Duration) bool {
	if r == nil || window <= 0 {
		return false
	}
	threshold := now.Add(-window)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rep := range r.seen {
		if !rep.LastTLSAt.IsZero() && rep.LastTLSAt.After(threshold) {
			return true
		}
	}
	return false
}

// Retain evicts entries for servers not in the live set (bounds memory), the
// same contract every other agent registry follows.
func (r *AgentTransportRegistry) Retain(live map[string]struct{}) {
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

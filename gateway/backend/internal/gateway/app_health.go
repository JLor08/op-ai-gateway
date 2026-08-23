// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"sync"
	"time"
)

// The app-health registry is the reachability source for both the routing
// resolver (routing.ReachabilityChecker) and the portal model-offering / DTO
// enrichment (portal.AppHealthReader).
var (
	_ routing.ReachabilityChecker = (*AppHealthRegistry)(nil)
	_ portal.AppHealthReader      = (*AppHealthRegistry)(nil)
)

// appHealth is the last observed reachability of one application, populated by
// the background app-health probe loop. It carries only lightweight operational
// metadata (no payloads), like ActiveRequest.
type appHealth struct {
	Reachable     bool
	LastCheckedAt time.Time
	LastError     string
}

// AppHealthRegistry is a thread-safe, in-memory map of per-application
// reachability. It is held on the Server like the active-request registry:
// purely volatile (never persisted). Phase 3 consumes it as a reachability
// checker for routing and model offering. All methods are nil-safe so a bare
// *Server built in a test keeps working.
type AppHealthRegistry struct {
	mu     sync.RWMutex
	items  map[string]appHealth
	broker *usage.Broker
}

func NewAppHealthRegistry(b *usage.Broker) *AppHealthRegistry {
	return &AppHealthRegistry{items: make(map[string]appHealth), broker: b}
}

// Set records the latest reachability for an application. It pokes the usage
// broker (when present) ONLY when reachability transitions — a change from the
// previous observation, or a first observation that is unreachable — so the SSE
// stream fires on state changes rather than on every probe tick. The lenient
// cold-start default is "reachable", so a first reachable observation is not a
// transition and does not publish.
func (r *AppHealthRegistry) Set(appID string, reachable bool, at time.Time, errMsg string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	prev, existed := r.items[appID]
	r.items[appID] = appHealth{Reachable: reachable, LastCheckedAt: at, LastError: errMsg}
	r.mu.Unlock()

	var transition bool
	if !existed {
		transition = !reachable
	} else {
		transition = prev.Reachable != reachable
	}
	if transition && r.broker != nil {
		r.broker.Publish()
	}
}

// Snapshot returns a copy of the current per-application health. A nil registry
// returns nil. The returned map is safe for the caller to mutate.
func (r *AppHealthRegistry) Snapshot() map[string]appHealth {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]appHealth, len(r.items))
	for id, h := range r.items {
		out[id] = h
	}
	return out
}

// Reachable reports whether an application is currently reachable. An unknown id
// (never probed) is reported reachable — a lenient cold-start so the gateway can
// serve immediately at startup; the first probe cycle corrects it.
func (r *AppHealthRegistry) Reachable(appID string) bool {
	if r == nil {
		return true
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.items[appID]
	if !ok {
		return true
	}
	return h.Reachable
}

// ApplicationHealth returns the last observed reachability for an application
// plus whether it has ever been probed (known). An unknown id (never probed) or
// a nil registry returns (true, zero time, false) — the lenient cold-start
// default, so a caller enriching a DTO treats never-probed applications as
// reachable with no last-checked timestamp.
func (r *AppHealthRegistry) ApplicationHealth(appID string) (reachable bool, lastCheckedAt time.Time, known bool) {
	if r == nil {
		return true, time.Time{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	h, ok := r.items[appID]
	if !ok {
		return true, time.Time{}, false
	}
	return h.Reachable, h.LastCheckedAt, true
}

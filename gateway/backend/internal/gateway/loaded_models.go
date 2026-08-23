// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sort"
	"sync"
	"time"
)

// defaultAgentLoadedTTL bounds how long an agent-reported loaded-model set is
// trusted after its last telemetry POST. Past this, the gateway-poll result is
// used instead (so a dead agent stops overriding). The agent posts far more often
// than this in practice.
const defaultAgentLoadedTTL = 5 * time.Minute

type loadedEntry struct {
	models []string
	at     time.Time
}

// LoadedModelRegistry tracks, volatile + in-memory, which upstream (app) model
// names are currently LOADED. It has two sources: the gateway's own per-application
// poll (keyed by application id) and the server-agent's per-server telemetry report
// (keyed by server id). A fresh agent report takes precedence over the gateway poll
// for that server's applications (the user-chosen "both, agent wins" policy). All
// methods are nil-safe.
//
// The loaded state is surfaced by FETCH (the portal Models() / model-servers DTO
// enrichment, read on navigation/bootstrap and the chat's model poll) — NOT over the
// usage SSE (which drives only the Activity view). ADDITIONALLY the registry fans out a
// coalescing "loaded changed" signal to Subscribe()rs (used by the model-servers SSE) on
// each SetGatewayProbe/SetAgentReport write, so a live view can react without polling.
// The agent-report TTL is evaluated lazily on read.
type LoadedModelRegistry struct {
	mu       sync.RWMutex
	byApp    map[string]loadedEntry // gateway-poll, keyed by application id
	byServer map[string]loadedEntry // agent-report, keyed by server id
	subs     map[chan struct{}]struct{}
	agentTTL time.Duration
	now      func() time.Time
}

// NewLoadedModelRegistry returns an empty registry. A nil registry is a valid
// "feature off" value handled by every method.
func NewLoadedModelRegistry() *LoadedModelRegistry {
	return &LoadedModelRegistry{
		byApp:    make(map[string]loadedEntry),
		byServer: make(map[string]loadedEntry),
		subs:     make(map[chan struct{}]struct{}),
		agentTTL: defaultAgentLoadedTTL,
		now:      time.Now,
	}
}

// SetGatewayProbe records the gateway's own poll result for an application. Passing
// nil/empty clears it (the probe failed or returned nothing).
func (r *LoadedModelRegistry) SetGatewayProbe(appID string, models []string) {
	if r == nil {
		return
	}
	r.set(r.byApp, appID, models)
	r.publish()
}

// SetAgentReport records the server-agent's reported loaded models for a server.
func (r *LoadedModelRegistry) SetAgentReport(serverID string, models []string) {
	if r == nil {
		return
	}
	r.set(r.byServer, serverID, models)
	r.publish()
}

func (r *LoadedModelRegistry) set(m map[string]loadedEntry, key string, models []string) {
	if key == "" {
		return
	}
	norm := normalizeModelSet(models)
	r.mu.Lock()
	m[key] = loadedEntry{models: norm, at: r.now()}
	r.mu.Unlock()
}

// Subscribe registers a change subscriber and returns a buffered(1) coalescing channel plus an
// idempotent unsubscribe. A single receive means "loaded-state changed at least once since the last
// drain" (bursts coalesce; no change is fully lost). A nil registry returns an already-closed
// channel and a no-op unsubscribe.
func (r *LoadedModelRegistry) Subscribe() (<-chan struct{}, func()) {
	if r == nil {
		ch := make(chan struct{})
		close(ch)
		return ch, func() { /* no-op: nil registry has no subscriber map to remove from */ }
	}
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	if r.subs == nil {
		r.subs = make(map[chan struct{}]struct{})
	}
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		delete(r.subs, ch)
		r.mu.Unlock()
	}
}

// publish coalesces a "loaded changed" signal to every subscriber. Non-blocking: a subscriber whose
// buffered(1) channel already holds a pending signal simply keeps it (bursts coalesce), so the
// publisher never blocks. Subscriber channels are snapshotted under the lock, delivered outside it.
func (r *LoadedModelRegistry) publish() {
	if r == nil {
		return
	}
	r.mu.RLock()
	targets := make([]chan struct{}, 0, len(r.subs))
	for ch := range r.subs {
		targets = append(targets, ch)
	}
	r.mu.RUnlock()
	for _, ch := range targets {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// Retain drops byApp/byServer entries whose key is not in the supplied live sets,
// so entries for deleted applications/servers do not accumulate for the life of the
// process. Called each app-health cycle with the currently-existing ids. Nil sets
// evict everything of that kind; a nil registry is a no-op.
func (r *LoadedModelRegistry) Retain(liveApps, liveServers map[string]struct{}) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id := range r.byApp {
		if _, ok := liveApps[id]; !ok {
			delete(r.byApp, id)
		}
	}
	for id := range r.byServer {
		if _, ok := liveServers[id]; !ok {
			delete(r.byServer, id)
		}
	}
}

// LoadedAppModels returns the loaded upstream-model names for an application on a
// server. A FRESH agent report for the server takes precedence (agent wins); else
// the gateway-poll result for the application. A nil registry / no data returns nil.
// The returned slice is a copy the caller may keep.
func (r *LoadedModelRegistry) LoadedAppModels(appID, serverID string) []string {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if e, ok := r.byServer[serverID]; ok && len(e.models) > 0 && r.now().Sub(e.at) <= r.agentTTL {
		return append([]string(nil), e.models...)
	}
	if e, ok := r.byApp[appID]; ok {
		return append([]string(nil), e.models...)
	}
	return nil
}

// normalizeModelSet drops empties, dedups, and sorts a model-name set so the stored
// slice is canonical and change/equality checks are order-insensitive.
func normalizeModelSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

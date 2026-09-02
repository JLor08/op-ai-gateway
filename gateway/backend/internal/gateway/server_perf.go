// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"sync"
)

const (
	// serverPerfRingCap bounds the per-server in-memory sample ring. At the
	// ServerAgent's ~3s cadence this holds roughly one hour of live history.
	serverPerfRingCap = 1200
	// serverPerfSubBuffer is the per-subscriber channel buffer. A slow reader
	// simply drops samples once the buffer fills; it recovers on its next
	// reconnect snapshot.
	serverPerfSubBuffer = 256
)

// serverPerfRegistry keeps a bounded in-memory ring of the most recent rich
// telemetry samples per server and fans new samples out to live SSE
// subscribers. It is purely volatile (never persisted; the durable history
// lives in server_telemetry_samples) and mirrors activeRegistry: held on the
// Server, all methods nil-safe so a bare *Server built in a test keeps working.
type serverPerfRegistry struct {
	mu      sync.RWMutex
	rings   map[string][]routing.TelemetrySample
	subs    map[string]map[chan routing.TelemetrySample]struct{}
	ringCap int
}

// NewServerPerfRegistry builds an empty registry with the default ring cap.
func NewServerPerfRegistry() *serverPerfRegistry {
	return &serverPerfRegistry{
		rings:   map[string][]routing.TelemetrySample{},
		subs:    map[string]map[chan routing.TelemetrySample]struct{}{},
		ringCap: serverPerfRingCap,
	}
}

// publish appends a sample to the server's ring (evicting the oldest beyond the
// cap) and non-blockingly fans it out to that server's live subscribers. A nil
// registry or an empty ServerID is a no-op. Subscriber channels are snapshotted
// under the lock, then delivered outside it so a slow reader never blocks the
// publisher (its sample is dropped when its buffer is full).
func (r *serverPerfRegistry) publish(sample routing.TelemetrySample) {
	if r == nil || sample.ServerID == "" {
		return
	}
	ringCap := r.ringCap
	if ringCap <= 0 {
		ringCap = serverPerfRingCap
	}

	r.mu.Lock()
	ring := append(r.rings[sample.ServerID], sample) //nolint:gocritic // deliberately derives a new slice to check/trim against ringCap before storing it back
	if len(ring) > ringCap {
		// Copy into a fresh slice so the evicted prefix's backing array can be
		// reclaimed and the ring's capacity stays bounded.
		trimmed := make([]routing.TelemetrySample, ringCap)
		copy(trimmed, ring[len(ring)-ringCap:])
		ring = trimmed
	}
	r.rings[sample.ServerID] = ring
	targets := make([]chan routing.TelemetrySample, 0, len(r.subs[sample.ServerID]))
	for ch := range r.subs[sample.ServerID] {
		targets = append(targets, ch)
	}
	r.mu.Unlock()

	for _, ch := range targets {
		select {
		case ch <- sample:
		default:
		}
	}
}

// latest returns the most recent sample in serverID's ring. ok is false for a
// nil registry and for a server whose ring is still empty -- which is the
// state after a gateway restart, until the next agent sample arrives (at most
// ~1 s later). A caller that gates on GPU presence must therefore read "not
// ok" as "not known yet", never as "this host has no GPU"; the VRAM
// benchmark's precondition refuses in both cases on purpose, because refusing
// is the safe direction and costs the operator one retry.
func (r *serverPerfRegistry) latest(serverID string) (routing.TelemetrySample, bool) {
	if r == nil || serverID == "" {
		return routing.TelemetrySample{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ring := r.rings[serverID]
	if len(ring) == 0 {
		return routing.TelemetrySample{}, false
	}
	return ring[len(ring)-1], true
}

// subscribe atomically returns a copy of the server's current ring plus a
// channel of subsequent samples (so no sample is lost between snapshot and
// registration) and an idempotent unsubscribe. A nil registry returns a nil
// snapshot and an already-closed channel.
func (r *serverPerfRegistry) subscribe(serverID string) ([]routing.TelemetrySample, <-chan routing.TelemetrySample, func()) {
	if r == nil {
		ch := make(chan routing.TelemetrySample)
		close(ch)
		return nil, ch, func() { /* no-op: nil registry has no subscriber map to remove from */ }
	}

	ch := make(chan routing.TelemetrySample, serverPerfSubBuffer)
	r.mu.Lock()
	snap := append([]routing.TelemetrySample(nil), r.rings[serverID]...)
	if r.subs[serverID] == nil {
		r.subs[serverID] = map[chan routing.TelemetrySample]struct{}{}
	}
	r.subs[serverID][ch] = struct{}{}
	r.mu.Unlock()

	unsub := func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if set, ok := r.subs[serverID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(r.subs, serverID)
			}
		}
	}
	return snap, ch, unsub
}

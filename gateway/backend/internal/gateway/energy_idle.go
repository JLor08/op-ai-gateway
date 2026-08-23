// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"math"
	"sync"
	"time"
)

// idleTracker is a volatile, concurrency-safe per-server rolling MINIMUM of
// observed server watts over a trailing window -- an emergent estimate of a
// server's idle power draw, absent an operator-set AIServer.IdleWatts override.
// It is fed by ingestTelemetrySample (server.go) on every agent telemetry
// report, and read by the energy reconciler (reconcileEnergyEvent, in
// energy_reconciler.go) when resolving the effective idleW to pass to
// ComputeEnergy's marginal calculation.
//
// Implementation note: rather than retaining every sample within the window
// (unbounded per-server memory, and there is no existing sliding-window-
// minimum structure in this codebase to reuse), each server keeps only its
// CURRENT minimum plus the timestamp it was set at. A new sample that is lower
// than (or equal to) the tracked minimum always replaces it. A HIGHER sample is
// ignored -- UNLESS the tracked minimum has aged out of the window, in which
// case the tracker resets to the new (higher) sample, so the idle estimate CAN
// rise again after a sustained load increase (e.g. a permanent workload
// change) instead of being pinned to a stale trough forever. Critically, a
// higher-but-still-in-window sample does NOT touch the tracked minimum's
// timestamp: doing so would let a continuous string of busy samples
// "refresh" a stale minimum's age indefinitely and pin it forever, which is
// the opposite of the intended behavior. This is a deliberate O(1)
// memory/CPU-per-observation approximation of a true sliding-window minimum.
type idleTracker struct {
	mu      sync.Mutex
	window  time.Duration
	entries map[string]idleEntry
}

// idleEntry is one server's tracked minimum watts + the time it was observed.
type idleEntry struct {
	watts float64
	at    time.Time
}

// newIdleTracker returns a ready-to-use tracker. A non-positive window
// defaults to 1 hour.
func newIdleTracker(window time.Duration) *idleTracker {
	if window <= 0 {
		window = time.Hour
	}
	return &idleTracker{window: window, entries: make(map[string]idleEntry)}
}

// Observe records a new server-watts sample for serverID at now. Nil-safe (a
// nil tracker's Observe is a no-op, so a caller need not nil-check before
// calling). A non-finite (NaN/±Inf) or negative watts reading -- a
// misbehaving or absent power sensor -- is ignored rather than poisoning the
// tracked minimum.
func (t *idleTracker) Observe(serverID string, watts float64, now time.Time) {
	if t == nil || serverID == "" {
		return
	}
	if math.IsNaN(watts) || math.IsInf(watts, 0) || watts < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.entries[serverID]
	if !ok || watts <= cur.watts || now.Sub(cur.at) > t.window {
		t.entries[serverID] = idleEntry{watts: watts, at: now}
		return
	}
	// watts > cur.watts and cur is still within the window: keep the existing
	// (lower) minimum, deliberately WITHOUT refreshing its timestamp -- see the
	// type doc for why that matters.
}

// Idle returns the tracked rolling-minimum watts for serverID, or 0 if unknown
// (never observed for that server, or a nil tracker).
func (t *idleTracker) Idle(serverID string) float64 {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[serverID].watts
}

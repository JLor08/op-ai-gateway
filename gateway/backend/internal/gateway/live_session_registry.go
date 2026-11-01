// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "sync"

// liveSessionRegistry enforces at most one active live transcription session per
// route. audio.cpp serves one /live session per model at a time and nothing else
// serializes it, so the WebSocket handler takes a slot here before relaying and a
// second concurrent session on the same route is refused with a clean busy reject
// instead of colliding at the backend. It is pure mutual exclusion: no TTL, count
// or eviction, so a slot is held until its holder calls the release it was handed.
type liveSessionRegistry struct {
	mu     sync.Mutex
	active map[string]struct{}
}

func newLiveSessionRegistry() *liveSessionRegistry {
	return &liveSessionRegistry{active: make(map[string]struct{})}
}

// acquire claims the slot for routeID. When the route already has an active session
// it returns (nil, false) and the caller must refuse the new session. Otherwise it
// returns a release func and true.
//
// release frees the slot and is one-shot: only the first call acts, every later call
// is a no-op. Each acquire gets its own release, so a stale release held by an earlier
// session can never free the slot a later session has since re-acquired.
//
// The empty routeID is an ordinary key and is serialized like any other; callers
// should pass the resolved route's ID. A nil registry fails closed: it admits
// nothing, so a missing registry can never let two sessions share a backend.
func (r *liveSessionRegistry) acquire(routeID string) (release func(), ok bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, busy := r.active[routeID]; busy {
		return nil, false
	}
	r.active[routeID] = struct{}{}

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.active, routeID)
		})
	}, true
}

// held reports whether routeID currently has an active live session, WITHOUT claiming
// or changing the slot. It lets a non-live request to the same single-slot model (a
// buffered transcription or /details) refuse cleanly instead of colliding at the
// upstream, where audio.cpp's one-at-a-time behaviour would surface as an error and an
// honest loud zero. A nil registry holds nothing (it admitted nothing to begin with),
// so the check is a no-op where live transcription is not wired.
//
// This is an advisory snapshot, not a lock: a live session may start or end right after
// the read. The window is small and the fallback is the pre-existing loud zero, so the
// check only turns the common same-model collision into a clean refusal; it does not
// try to make the two paths mutually exclusive.
func (r *liveSessionRegistry) held(routeID string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, busy := r.active[routeID]
	return busy
}

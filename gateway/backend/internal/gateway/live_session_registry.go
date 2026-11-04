// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "sync"

// liveSessionRegistry enforces at most one active live transcription session per
// instance, keyed by the target's gateway instance key (routing.InstanceKey, which
// a mapping without instances has too: "<mapping_id>#0"). audio.cpp serves one
// /live session per process at a time and nothing else serializes it, so the live
// relay takes a slot here before relaying, and a session that finds its instance's
// slot taken is selected again -- onto a sibling, or into the admission queue until a
// slot frees -- or refused with a clean busy reject, never left to collide at the
// backend. N running instances of a replicable spec
// are N slots; a mapping without instances keeps its one. It is pure mutual
// exclusion: no TTL, count or eviction, so a slot is held until its holder calls
// the release it was handed. It is also the resolver's routing.LiveSlotChecker
// (LiveHeld), so a live session's selection passes over a held instance.
type liveSessionRegistry struct {
	mu sync.Mutex
	// active maps a held instance key to its holder: the number claim gave the
	// session holding it.
	active map[string]uint64
	issued uint64 // the last holder number claim gave
}

func newLiveSessionRegistry() *liveSessionRegistry {
	return &liveSessionRegistry{active: make(map[string]uint64)}
}

// acquire claims the slot for instanceKey. When the instance already has an active
// session it returns (nil, false) and the caller must not relay the new session there.
// Otherwise it returns a release func and true.
//
// release frees the slot and is one-shot: only the first call acts, every later call
// is a no-op. Each acquire gets its own release, so a stale release held by an earlier
// session can never free the slot a later session has since re-acquired.
//
// The empty key is an ordinary key and is serialized like any other; callers pass the
// resolved target's InstanceKey. A nil registry fails closed: it admits nothing, so a
// missing registry can never let two sessions share a backend.
func (r *liveSessionRegistry) acquire(instanceKey string) (release func(), ok bool) {
	release, _, ok = r.claim(instanceKey)
	return release, ok
}

// claim is acquire that also names the slot's holder: on success the number this
// claim was given, and when the slot is taken the number of the session holding it.
// Holder numbers are never reused, so a caller that finds the same slot taken twice
// can tell whether the same session is still in its way or a later one took the slot
// after it freed. A nil registry claims nothing and names holder 0.
func (r *liveSessionRegistry) claim(instanceKey string) (release func(), holder uint64, ok bool) {
	if r == nil {
		return nil, 0, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if holder, busy := r.active[instanceKey]; busy {
		return nil, holder, false
	}
	r.issued++
	mine := r.issued
	r.active[instanceKey] = mine

	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.active[instanceKey] == mine {
				delete(r.active, instanceKey)
			}
		})
	}, mine, true
}

// LiveHeld reports whether instanceKey currently has an active live session, WITHOUT
// claiming or changing the slot. The resolver asks it (routing.LiveSlotChecker) for every
// candidate of a request that needs a free live slot -- a live session and every
// transcription, buffered or /details -- in a pool that holds an instance candidate: the
// instances, and in a mixed pool the plain mappings beside them. A buffered transcription
// (or /details) also asks it for the target its resolve chose -- a mapping's one taken
// slot, or a session that started after the resolve -- to refuse cleanly instead of
// colliding at the upstream, where audio.cpp's one-at-a-time behaviour would surface as an
// error and an honest loud zero. A nil registry holds nothing (it admitted nothing to
// begin with), so the check is a no-op where live transcription is not wired.
//
// This is an advisory snapshot, not a lock: a live session may start or end right after
// the read. A live session that selected a slot which is taken before it acquires it is
// re-selected (acquireLiveSession); a buffered request falls back to the pre-existing
// loud zero. So the check turns the common collision into a better choice or a clean
// refusal; it does not try to make the paths mutually exclusive.
func (r *liveSessionRegistry) LiveHeld(instanceKey string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, busy := r.active[instanceKey]
	return busy
}

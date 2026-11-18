// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"container/list"
	"sync"
	"time"
)

const (
	// rejectedBlobGrace is how long a hash survives without being matched.
	// Context compaction makes old blobs vanish for good; a request that merely
	// lacks them (a Claude Code subagent sharing the session id) must not drop
	// them, so only elapsed time does.
	rejectedBlobGrace = time.Hour
	// rejectedBlobTTL bounds an entry's life from its first Record.
	rejectedBlobTTL = 24 * time.Hour
	// rejectedBlobCap bounds the number of (session, target, flavor) entries.
	rejectedBlobCap = 10000
)

// rejectedBlobKey scopes remembered hashes to one client session talking to
// one target account through one API flavor.
type rejectedBlobKey struct{ Session, Target, Flavor string }

type rejectedBlobEntry struct {
	key        rejectedBlobKey
	created    time.Time
	lastActive time.Time // newest lastMatched among hashes; drives Has in O(1)
	hashes     map[blobHash]time.Time
	elem       *list.Element
}

// rejectedBlobMemo remembers, per session and target, the hashes of opaque
// reasoning blobs the target rejected, so follow-up requests are filtered up
// front instead of retried. Bounded by grace, TTL and an LRU cap; only hashes
// are stored, never blob contents. Safe for concurrent use.
type rejectedBlobMemo struct {
	mu      sync.Mutex
	now     func() time.Time
	cap     int
	entries map[rejectedBlobKey]*rejectedBlobEntry
	lru     *list.List // front = most recently used
}

func newRejectedBlobMemo(now func() time.Time) *rejectedBlobMemo {
	if now == nil {
		now = time.Now
	}
	return &rejectedBlobMemo{
		now:     now,
		cap:     rejectedBlobCap,
		entries: make(map[rejectedBlobKey]*rejectedBlobEntry),
		lru:     list.New(),
	}
}

func (m *rejectedBlobMemo) remove(e *rejectedBlobEntry) {
	m.lru.Remove(e.elem)
	delete(m.entries, e.key)
}

// live returns the entry for k, dropping it when it is past TTL or when no hash
// is within grace. Caller holds mu.
func (m *rejectedBlobMemo) live(k rejectedBlobKey, now time.Time) *rejectedBlobEntry {
	e, ok := m.entries[k]
	if !ok {
		return nil
	}
	if now.Sub(e.created) > rejectedBlobTTL || now.Sub(e.lastActive) > rejectedBlobGrace {
		m.remove(e)
		return nil
	}
	return e
}

// Record adds hashes to k's set (the set only grows) and stamps them as just
// seen. An existing entry keeps its creation time, so the TTL counts from the
// first Record.
func (m *rejectedBlobMemo) Record(k rejectedBlobKey, hashes []blobHash) {
	if len(hashes) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e := m.live(k, now)
	if e == nil {
		e = &rejectedBlobEntry{key: k, created: now, hashes: make(map[blobHash]time.Time, len(hashes))}
		e.elem = m.lru.PushFront(e)
		m.entries[k] = e
		for m.lru.Len() > m.cap {
			m.remove(m.lru.Back().Value.(*rejectedBlobEntry))
		}
	} else {
		m.lru.MoveToFront(e.elem)
	}
	for _, h := range hashes {
		e.hashes[h] = now
	}
	e.lastActive = now
}

// Has reports whether k has a live entry. It is O(1) and parses nothing, so it
// gates every passthrough request.
func (m *rejectedBlobMemo) Has(k rejectedBlobKey) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.live(k, m.now()) != nil
}

// Match returns the stored hashes that are present in this request and stamps
// them last-matched. It prunes hashes past grace. Stored hashes absent from the
// request are kept until their own grace elapses.
func (m *rejectedBlobMemo) Match(k rejectedBlobKey, present []blobHash) map[blobHash]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	e := m.live(k, now)
	if e == nil {
		return nil
	}
	for h, at := range e.hashes {
		if now.Sub(at) > rejectedBlobGrace {
			delete(e.hashes, h)
		}
	}
	if len(e.hashes) == 0 {
		m.remove(e)
		return nil
	}
	var out map[blobHash]struct{}
	for _, h := range present {
		if _, ok := e.hashes[h]; ok {
			if out == nil {
				out = make(map[blobHash]struct{})
			}
			out[h] = struct{}{}
			e.hashes[h] = now
			e.lastActive = now
		}
	}
	m.lru.MoveToFront(e.elem)
	return out
}

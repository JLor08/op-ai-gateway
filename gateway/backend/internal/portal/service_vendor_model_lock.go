// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"sync"
)

// accountLocks serializes work per vendor account id. Both writers of an account's
// model rows take it: RefreshVendorAccountModels (a read of the account's prefix
// followed by a whole-set replace) and relabelVendorAccountModels (a read of the
// rows followed by a whole-set replace). Each is a read-modify-write over a
// replace-everything store call, so without it a refresh and a prefix change that
// overlap would each write from a snapshot the other had already outdated, and
// whichever wrote last would silently undo the other.
//
// The zero value is ready to use. An id's entry exists only while someone holds or
// waits for it, so the map does not grow with every account ever written.
type accountLocks struct {
	mu      sync.Mutex
	entries map[string]*accountLockEntry
}

// accountLockEntry is one id's lock: a one-slot semaphore (a channel, so a waiter
// can give up when its context ends, which a sync.Mutex cannot) and the number of
// goroutines that hold or wait for it.
type accountLockEntry struct {
	sem  chan struct{}
	refs int
}

// lock waits for id's lock and returns its unlock. It returns ctx's error, with no
// lock held, when ctx ends first.
func (l *accountLocks) lock(ctx context.Context, id string) (unlock func(), err error) {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = map[string]*accountLockEntry{}
	}
	entry := l.entries[id]
	if entry == nil {
		entry = &accountLockEntry{sem: make(chan struct{}, 1)}
		l.entries[id] = entry
	}
	entry.refs++
	l.mu.Unlock()

	select {
	case entry.sem <- struct{}{}:
		return func() {
			<-entry.sem
			l.release(id, entry)
		}, nil
	case <-ctx.Done():
		l.release(id, entry)
		return nil, ctx.Err()
	}
}

// release drops one reference to entry and forgets the id once nobody holds or
// waits for it.
func (l *accountLocks) release(id string, entry *accountLockEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(l.entries, id)
	}
}

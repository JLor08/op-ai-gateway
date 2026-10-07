// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"sync"
	"time"
)

// runtimeConfigPusher runs Server.PushRuntimeConfig as at most one worker per
// server: a document is derived only after the previous one for the server was
// enqueued, and after every notification a pass whose derive starts after it
// runs to completion. The zero value is ready to use.
//
// Two invariants follow from the map and the dirty flag. Order: a worker's
// entry is added before its goroutine starts and deleted by that goroutine in
// the exit check that finds no pass owed, both under mu, so two passes for one
// server never overlap and every connection receives the server's documents in
// derive order. Latest wins: a notification either starts a worker, whose
// first derive begins after it, or marks the running worker dirty before that
// worker's exit check, which owes one more pass whose derive starts after the
// check. Intermediate documents may be skipped; the last one always reflects
// the latest write.
//
// THE ONE-CRITICAL-SECTION RULE. run's dirty check and its delete of the
// worker from the map must stay in one critical section. Split in two, a
// notification that lands between them sets dirty on a worker that has already
// decided to exit, nothing runs the pass it owes, and the agent rests on an
// older document until its next poll. The window is a few instructions wide;
// the short rounds of TestRuntimeConfigPushBurstsKeepOrderAndLatest aim
// notifications at it.
//
// notify never waits on a derive: it takes mu, a leaf lock that nothing else is
// taken under and that is never held across a pass, and at most starts a
// goroutine. So the portal's write-path hook stays fast, and a slow store
// delays later pushes for its own server instead of letting them overtake.
type runtimeConfigPusher struct {
	mu      sync.Mutex                           // leaf lock: nothing else is taken under it, never held across a pass
	workers map[string]*runtimeConfigPushWorker  // an entry exists from a worker's start until its exit check finds no pass owed
	after   func(time.Duration) <-chan time.Time // test seam for the spacing wait; nil means time.After
}

// runtimeConfigPushWorker is one server's running worker.
type runtimeConfigPushWorker struct {
	dirty bool          // a notification arrived after the current pass began: one more pass is owed
	done  chan struct{} // closed when the worker exits (test seam: idle)
}

// notify asks for one pass for serverID whose derive starts after this call.
// An empty serverID is ignored. When the server's worker runs, notify marks it
// dirty and returns; otherwise it registers a new worker and starts it. spacing
// and pass are the new worker's; a running worker keeps its own.
func (p *runtimeConfigPusher) notify(serverID string, spacing time.Duration, pass func(serverID string) bool) {
	if serverID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.workers[serverID]; ok {
		w.dirty = true
		return
	}
	if p.workers == nil {
		p.workers = make(map[string]*runtimeConfigPushWorker)
	}
	w := &runtimeConfigPushWorker{done: make(chan struct{})}
	p.workers[serverID] = w
	go p.run(serverID, w, spacing, pass)
}

// run is one server's worker. It runs pass, and after a pass that enqueued a
// frame on at least one connection it waits spacing before it looks for more
// work, so notifications in that window coalesce into one next pass and two
// documents for the server leave at least spacing apart. A pass that sent
// nothing is not spaced. The worker exits when no notification arrived since
// its pass began; see the one-critical-section rule on runtimeConfigPusher.
func (p *runtimeConfigPusher) run(serverID string, w *runtimeConfigPushWorker, spacing time.Duration, pass func(serverID string) bool) {
	defer close(w.done)
	for {
		sent := pass(serverID)
		if sent && spacing > 0 {
			after := p.after
			if after == nil {
				after = time.After
			}
			<-after(spacing)
		}
		p.mu.Lock()
		if !w.dirty {
			delete(p.workers, serverID)
			p.mu.Unlock()
			return
		}
		w.dirty = false
		p.mu.Unlock()
	}
}

// idle returns a channel that is closed once serverID's worker has found no
// pass owed and left the map, at once when the server has no worker.
// Tests only: production code never waits for a push.
func (p *runtimeConfigPusher) idle(serverID string) <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if w, ok := p.workers[serverID]; ok {
		return w.done
	}
	done := make(chan struct{})
	close(done)
	return done
}

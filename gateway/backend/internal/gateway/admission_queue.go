// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// errAdmissionTimeout / errAdmissionFull are returned by admissionQueue.WaitForSlot. They
// ARE the exported routing sentinels (routing.ErrAdmissionQueue*) so the resolver propagates
// them straight to the HTTP layer for a 503; keep the names stable for the local call sites.
var (
	errAdmissionTimeout = routing.ErrAdmissionQueueTimeout
	errAdmissionFull    = routing.ErrAdmissionQueueFull
)

// admissionRecheckInterval is the bounded liveness re-check: how long WaitForSlot parks
// before waking the caller to re-read the live capacity even absent a release signal. The
// release signal (below) is the FAST path (sub-ms wake in the common case); this periodic
// re-check is a backstop that guarantees a free slot is never missed for longer than one
// interval, closing the edge-triggered lost-wakeup windows (a slot that frees between the
// resolver's cap-check and this enqueue, or a release coalesced onto an already-signalled
// peer). Without it, under the default unbounded (timeout<=0) wait a lost signal would hang
// a request indefinitely with a free slot idle.
const admissionRecheckInterval = 250 * time.Millisecond

// admissionQueue is a bounded, FIFO, cancellable wait-queue for the CP4 capacity admission
// control. An unpinned request whose every candidate server is at its effective concurrency
// cap parks here (WaitForSlot) until a slot frees on one of those servers (release, fed by
// activeRegistry.Remove), a bounded re-check interval elapses (a liveness backstop), the
// wait times out, the client's context is cancelled, or the queue is at maxDepth (rejected
// immediately). Volatile + nil-safe.
//
// Fairness + race-safe hand-off: waiters are held in FIFO order; a release for server X
// signals the FRONT UNSIGNALLED waiter WATCHING X — exactly one per freed slot, so two
// queued waiters never both claim the same freed slot, AND N distinct frees wake N distinct
// waiters (release skips a waiter whose signal is still buffered rather than coalescing onto
// it). A woken waiter re-checks the live cap in the resolver and, if the slot was meanwhile
// taken by an unqueued request, re-enqueues (at the back). The bounded re-check (see
// admissionRecheckInterval) is a liveness backstop for any release that races the enqueue.
type admissionQueue struct {
	mu       sync.Mutex
	maxDepth int
	recheck  time.Duration      // bounded liveness re-check interval (0 disables — tests only)
	waiters  []*admissionWaiter // FIFO
}

type admissionWaiter struct {
	servers map[string]struct{} // candidate server ids this waiter can use
	ch      chan struct{}       // buffered(1); a release sends here
}

func newAdmissionQueue(maxDepth int) *admissionQueue {
	if maxDepth <= 0 {
		maxDepth = 128
	}
	return &admissionQueue{maxDepth: maxDepth, recheck: admissionRecheckInterval}
}

// WaitForSlot parks the caller until a slot frees on one of serverIDs (returns nil => the
// caller retries selection), the bounded re-check interval elapses (also returns nil => the
// caller re-reads the live cap; a liveness backstop for a raced release), the timeout
// elapses (>0) => errAdmissionTimeout, the queue is full => errAdmissionFull (immediate, no
// wait), or ctx is done => ctx.Err(). Nil-safe: a nil queue returns errAdmissionFull.
func (q *admissionQueue) WaitForSlot(ctx context.Context, serverIDs []string, timeout time.Duration) error {
	if q == nil {
		return errAdmissionFull
	}
	set := make(map[string]struct{}, len(serverIDs))
	for _, id := range serverIDs {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	w := &admissionWaiter{servers: set, ch: make(chan struct{}, 1)}

	q.mu.Lock()
	if len(q.waiters) >= q.maxDepth {
		q.mu.Unlock()
		return errAdmissionFull
	}
	q.waiters = append(q.waiters, w)
	q.mu.Unlock()

	defer q.remove(w) // always dequeue on exit (signalled, re-check, timeout, cancel)

	var timerC <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		timerC = timer.C
	}
	// Bounded liveness re-check: wake to re-read the live capacity even without a release,
	// so a slot freed in the check->enqueue gap (or a coalesced/lost release) is never
	// missed for longer than one interval. Returning nil makes the resolver re-run
	// selectCandidate; if still at cap it re-parks. Disabled (recheck<=0) only in tests.
	var recheckC <-chan time.Time
	if q.recheck > 0 {
		rt := time.NewTimer(q.recheck)
		defer rt.Stop()
		recheckC = rt.C
	}
	select {
	case <-w.ch:
		return nil
	case <-recheckC:
		return nil // periodic re-check trigger (liveness backstop)
	case <-timerC:
		return errAdmissionTimeout
	case <-ctx.Done():
		return ctx.Err()
	}
}

// remove drops w from the FIFO (idempotent).
func (q *admissionQueue) remove(w *admissionWaiter) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, x := range q.waiters {
		if x == w {
			q.waiters = append(q.waiters[:i], q.waiters[i+1:]...)
			return
		}
	}
}

// release wakes the FRONT waiter watching serverID (a slot freed there) and DEQUEUES it in
// the same step. Dequeuing on signal is what makes the hand-off race-safe: a woken waiter is
// no longer in q.waiters, so a subsequent release for the same server can neither re-target
// it (double-signalling one waiter while another starves) nor be fooled by a waiter that has
// already consumed its buffered signal but not yet run its own deferred remove — it simply
// wakes the NEXT queued waiter. So N distinct frees on a server wake N distinct waiters, and
// each release wakes at most one. No-op if nil/empty or no waiter watches serverID. The send
// is non-blocking (buffered(1) ch, and a waiter is signalled at most once because it is
// removed here) so a slow reader never blocks the releaser (activeRegistry.Remove).
func (q *admissionQueue) release(serverID string) {
	if q == nil || serverID == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for i, w := range q.waiters {
		if _, ok := w.servers[serverID]; !ok {
			continue
		}
		select {
		case w.ch <- struct{}{}:
		default: // defensive: cannot happen (signalled at most once), never block the releaser
		}
		q.waiters = append(q.waiters[:i], q.waiters[i+1:]...) // dequeue on signal (return immediately)
		return
	}
}

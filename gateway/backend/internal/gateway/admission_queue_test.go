// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// parkedCount reports how many waiters are currently enqueued (read under the
// queue lock so it is race-safe alongside the production code paths).
func parkedCount(q *admissionQueue) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.waiters)
}

// waitForParked polls (no sleep-as-assertion) until exactly want waiters are
// enqueued, failing the test if that condition is not reached before the deadline.
// This makes release/no-release ordering deterministic: we never signal a release
// before the target waiter has actually parked.
func waitForParked(t *testing.T, q *admissionQueue, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if parkedCount(q) == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d parked waiter(s); have %d", want, parkedCount(q))
}

func TestAdmissionQueueReleaseWakesWaiter(t *testing.T) {
	q := newAdmissionQueue(8)
	done := make(chan error, 1)
	go func() {
		done <- q.WaitForSlot(context.Background(), []string{"srv1"}, time.Second)
	}()
	waitForParked(t, q, 1)

	q.release("srv1")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForSlot after release = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForSlot did not return after release")
	}
}

func TestAdmissionQueueFullRejectsImmediately(t *testing.T) {
	q := newAdmissionQueue(1)
	q.recheck = 0 // isolate the edge-triggered/full behavior from the liveness backstop
	// Park one waiter to fill the queue to maxDepth=1. Cancel it at the end.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked := make(chan error, 1)
	go func() {
		blocked <- q.WaitForSlot(ctx, []string{"srv1"}, 0)
	}()
	waitForParked(t, q, 1)

	// A second WaitForSlot must be rejected immediately (no blocking) — a long
	// timeout would hang the test if it actually parked.
	start := time.Now()
	err := q.WaitForSlot(context.Background(), []string{"srv1"}, time.Minute)
	if !errors.Is(err, errAdmissionFull) {
		t.Fatalf("full-queue WaitForSlot = %v, want errAdmissionFull", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("full-queue WaitForSlot blocked for %v, want immediate", elapsed)
	}
}

func TestAdmissionQueueTimeout(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 0 // isolate the timeout path from the liveness backstop (which returns nil)
	err := q.WaitForSlot(context.Background(), []string{"srv1"}, 20*time.Millisecond)
	if !errors.Is(err, errAdmissionTimeout) {
		t.Fatalf("WaitForSlot with no release = %v, want errAdmissionTimeout", err)
	}
	if parkedCount(q) != 0 {
		t.Fatalf("waiter not dequeued after timeout; parked=%d", parkedCount(q))
	}
}

func TestAdmissionQueueContextCancel(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 0 // isolate the ctx-cancel path from the liveness backstop
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- q.WaitForSlot(ctx, []string{"srv1"}, time.Minute)
	}()
	waitForParked(t, q, 1)

	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("WaitForSlot after cancel = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitForSlot did not return after cancel")
	}
	if parkedCount(q) != 0 {
		t.Fatalf("waiter not dequeued after cancel; parked=%d", parkedCount(q))
	}
}

func TestAdmissionQueueOneReleaseWakesExactlyOne(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 0 // isolate the one-release-one-waiter semantics from the backstop
	// Two waiters watching the SAME server. Both report on one channel so we can
	// assert exactly one wakes without depending on which enqueued first.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	woke := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			if q.WaitForSlot(ctx, []string{"srv1"}, time.Minute) == nil {
				woke <- struct{}{}
			}
		}()
	}
	waitForParked(t, q, 2)

	q.release("srv1")

	// Exactly one must wake.
	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("no waiter woke after one release")
	}
	// The other must stay parked (no second wake within a short window).
	select {
	case <-woke:
		t.Fatal("second waiter woke from a single release")
	case <-time.After(150 * time.Millisecond):
	}
	if got := parkedCount(q); got != 1 {
		t.Fatalf("after one release parked=%d, want 1 (one still waiting)", got)
	}
}

func TestAdmissionQueueReleaseWrongServerDoesNotWake(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 0 // isolate the per-server match from the backstop (which would wake it)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- q.WaitForSlot(ctx, []string{"srvA"}, time.Minute)
	}()
	waitForParked(t, q, 1)

	q.release("srvB") // no waiter watches srvB

	select {
	case err := <-done:
		t.Fatalf("waiter on srvA woke from release(srvB): err=%v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if parkedCount(q) != 1 {
		t.Fatalf("waiter unexpectedly dequeued; parked=%d", parkedCount(q))
	}
}

func TestAdmissionQueueConcurrentNoPanic(t *testing.T) {
	q := newAdmissionQueue(64)
	var wg sync.WaitGroup
	// Many concurrent waiters with short timeouts.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = q.WaitForSlot(context.Background(), []string{"srv1", "srv2"}, 10*time.Millisecond)
		}()
	}
	// Many concurrent releasers interleaved.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q.release("srv1")
			q.release("srv2")
			q.release("")
		}()
	}
	wg.Wait()
	// A nil queue must also be safe.
	var nilQ *admissionQueue
	if err := nilQ.WaitForSlot(context.Background(), []string{"srv1"}, time.Second); !errors.Is(err, errAdmissionFull) {
		t.Fatalf("nil queue WaitForSlot = %v, want errAdmissionFull", err)
	}
	nilQ.release("srv1") // must not panic
}

// TestAdmissionQueueTwoReleasesWakeTwoWaiters: N distinct frees on a server must wake N
// distinct waiters — a second release must NOT coalesce onto the already-signalled front
// waiter (the verification-caught lost-wakeup). Regression for the release scan-past fix.
func TestAdmissionQueueTwoReleasesWakeTwoWaiters(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 0 // prove the wakes come from the releases, not the backstop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	woke := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			if q.WaitForSlot(ctx, []string{"srv1"}, time.Minute) == nil {
				woke <- struct{}{}
			}
		}()
	}
	waitForParked(t, q, 2)

	// Two frees on srv1 (near-simultaneous): both waiters must wake.
	q.release("srv1")
	q.release("srv1")

	for i := 0; i < 2; i++ {
		select {
		case <-woke:
		case <-time.After(time.Second):
			t.Fatalf("only %d of 2 waiters woke from 2 releases (coalesced lost-wakeup)", i)
		}
	}
	waitForParked(t, q, 0)
}

// TestAdmissionQueueRecheckWakesWithoutRelease: the bounded liveness backstop wakes a
// parked waiter (returns nil => the resolver re-checks) even absent ANY release and with an
// unbounded (timeout<=0) wait — so a slot freed in the check->enqueue gap can never hang the
// request indefinitely. Regression for the lost-wakeup MAJOR.
func TestAdmissionQueueRecheckWakesWithoutRelease(t *testing.T) {
	q := newAdmissionQueue(8)
	q.recheck = 20 * time.Millisecond
	start := time.Now()
	// timeout 0 = unbounded; no release is ever sent. Without the backstop this blocks
	// forever; with it, WaitForSlot returns nil within ~recheck.
	err := q.WaitForSlot(context.Background(), []string{"srv1"}, 0)
	if err != nil {
		t.Fatalf("WaitForSlot with recheck backstop = %v, want nil (periodic re-check)", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("backstop took %v, want ~recheck interval", elapsed)
	}
	if parkedCount(q) != 0 {
		t.Fatalf("waiter not dequeued after backstop wake; parked=%d", parkedCount(q))
	}
}

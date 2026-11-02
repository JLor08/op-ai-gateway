// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"testing"
	"time"
)

// --- re-asking an admission when a grace window expires -----------------------
//
// The eviction grace window keeps a recently used model off the victim list, so a
// start whose ONLY way to make room is a within-grace model must wait. That Wait
// is woken by nothing the rest of the manager provides: the release that stamped
// the victim's LastUsed fired before the window opened, the housekeeping beat only
// scans and measures, and wakeOnHostSample wakes only RAM waiters. With the
// production admission wait of 0 (wait until the client disconnects) and an
// occupant that never idle-unloads (idle_timeout 0), that start would hang
// forever. The owner therefore schedules its own wake at the grace expiry
// (Decision.WaitOnGrace / owner.rescheduleGraceWake); this is the regression for
// that hang.

// TestGraceExpiryWakeAdmitsAStartBlockedOnlyByGrace reproduces the hang and
// asserts the fix, deterministically and without any host sample, release or
// Apply after the blocked start queues -- so ONLY the grace-expiry wake can
// admit it:
//
//  1. spec-a starts, serves one request and is released, so it is idle but within
//     the (short) grace window. Its idle_timeout is 0, so nothing unloads it.
//  2. spec-b is requested. With max_processes 1, admitting it needs spec-a's slot,
//     but spec-a is within grace and cannot be evicted, so spec-b waits
//     (WaitOnGrace). Its admission wait is 0, so without a wake the wait is
//     unbounded. The test confirms spec-b is still waiting -- spec-a still running
//     -- partway through the window.
//  3. Once the window expires, the owner's grace timer re-asks spec-b's admission;
//     spec-a is now evictable, so spec-b evicts it and starts. The test asserts
//     this happens well within the window + a few hundred ms, far under spec-b's
//     own (effectively unbounded) wait.
func TestGraceExpiryWakeAdmitsAStartBlockedOnlyByGrace(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	const grace = 300 * time.Millisecond

	specA := baseSpec("spec-a", "model-a")
	specA.IdleTimeoutSeconds = 0 // never idle-unload: the window is the only thing that frees it
	specB := baseSpec("spec-b", "model-b")
	specB.AdmissionWaitTimeoutSeconds = 0 // wait until the caller's ctx is done: unbounded without the wake

	m := NewManager(ManagerOptions{
		Policy:        allowlistPolicy(),
		Getenv:        func(string) string { return "" },
		EvictionGrace: grace,
	})
	t.Cleanup(m.Close)
	m.Apply(Config{
		Specs:        []Spec{specA, specB},
		MaxProcesses: 1, // only one may run, so admitting spec-b needs spec-a's slot
		Coresident:   [][2]string{{"spec-a", "spec-b"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Start spec-a, serve nothing, and release it: now idle and freshly within the
	// grace window.
	_, releaseA, err := m.EnsureRunning(ctx, "model-a")
	if err != nil {
		t.Fatalf("EnsureRunning(model-a) = %v, want it admitted", err)
	}
	releaseA() // stamps spec-a.LastUsed = now and drops in-flight to 0
	waitUntil(t, 5*time.Second, "spec-a is running", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateRunning
	})

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, release, err := m.EnsureRunning(ctx, "model-b")
		if err == nil {
			defer release()
		}
		done <- outcome{err}
		if err == nil {
			<-ctx.Done() // keep spec-b busy until the test ends
		}
	}()

	// Partway through the window spec-b must still be waiting -- spec-a is within
	// grace and must not be evicted yet. This proves spec-b genuinely blocked on
	// the window rather than taking some other immediate path.
	select {
	case o := <-done:
		t.Fatalf("EnsureRunning(model-b) returned %v within the grace window, want it still waiting (spec-a is protected)", o.err)
	case <-time.After(grace / 2):
	}
	if st := statusFor(m, "spec-a"); st == nil || st.State != StateRunning {
		t.Fatalf("spec-a = %+v partway through the window, want it still running (within grace, not yet evictable)", st)
	}

	// Nothing else has happened -- no host sample, release, exit or Apply -- so only
	// the grace-expiry wake can admit spec-b. Assert it does, promptly.
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("EnsureRunning(model-b) = %v, want it admitted once the grace window expired", o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunning(model-b) was not admitted within 5s of the grace window expiring (its own wait is unbounded): only the grace-expiry wake can admit it, and nothing else fired -- this is the liveness regression")
	}
	waitUntil(t, 5*time.Second, "spec-a evicted for spec-b", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStopped
	})
	if st := statusFor(m, "spec-b"); st == nil || st.State != StateRunning {
		t.Fatalf("spec-b = %+v, want it running after it evicted the once-protected spec-a", st)
	}
}

// TestRescheduleGraceWakeArmsOnlyForAGraceBlockedWaiter pins when the owner's
// single grace timer is armed: a spec blocked solely by the grace window
// (wantsGraceWake) AND a within-grace victim whose window is still pending. No
// waiter, a waiter that is not idle/pending, a resolved waiter, or the window off
// all leave no timer armed. White-box over the owner so the timer's presence can
// be observed directly.
func TestRescheduleGraceWakeArmsOnlyForAGraceBlockedWaiter(t *testing.T) {
	newOwner := func() *owner {
		// A running, within-grace victim: the expiry the timer would wake at.
		o, victim := newSnapshotOwner(baseSpec("spec-a", "model-a"), 4242, nil)
		o.evictionGrace = time.Hour // long, so the armed timer cannot fire during the test
		victim.lastUsed = time.Now()
		return o
	}
	pinnedWaiter := func(waiting bool) *specState {
		return &specState{spec: Spec{ID: "spec-b", Pinned: true}, state: StateStopped, waitingOnGrace: waiting}
	}

	t.Run("arms for a grace-blocked pinned waiter", func(t *testing.T) {
		o := newOwner()
		o.specs["spec-b"] = pinnedWaiter(true)
		o.rescheduleGraceWake()
		if o.graceTimer == nil {
			t.Fatal("no timer armed for a grace-blocked waiter with a within-grace victim")
		}
		cancelTimer(o.m, &o.graceTimer) // release the wg Add
	})

	t.Run("does not arm when nothing is grace-blocked", func(t *testing.T) {
		o := newOwner() // the victim is present, but nobody waits on it
		o.rescheduleGraceWake()
		if o.graceTimer != nil {
			t.Error("a timer was armed with no grace-blocked waiter")
		}
	})

	t.Run("does not arm for a spec that is not a live waiter", func(t *testing.T) {
		o := newOwner()
		// waitingOnGrace true, but the spec has a live proc -> wantsGraceWake false.
		o.specs["spec-b"] = &specState{spec: Spec{ID: "spec-b"}, state: StateRunning, proc: &runningProc{specID: "spec-b"}, waitingOnGrace: true}
		o.rescheduleGraceWake()
		if o.graceTimer != nil {
			t.Error("a timer was armed for a spec that is not a live grace waiter")
		}
	})

	t.Run("cancels the timer when the only waiter resolves", func(t *testing.T) {
		o := newOwner()
		waiter := pinnedWaiter(true)
		o.specs["spec-b"] = waiter
		o.rescheduleGraceWake()
		if o.graceTimer == nil {
			t.Fatal("precondition: expected a timer armed")
		}
		waiter.waitingOnGrace = false // it got admitted
		o.rescheduleGraceWake()
		if o.graceTimer != nil {
			t.Error("the grace timer was not cancelled after the only waiter resolved")
		}
	})

	t.Run("the grace window off never arms", func(t *testing.T) {
		o := newOwner()
		o.evictionGrace = 0
		o.specs["spec-b"] = pinnedWaiter(true)
		o.rescheduleGraceWake()
		if o.graceTimer != nil {
			t.Error("a timer was armed with the grace window off")
		}
	})
}

// TestHandleCloseCancelsTheGraceTimer pins the G2 invariant for the grace timer:
// it is registered in m.wg, which Close waits on, so handleClose must cancel it or
// Close blocks until it fires (up to a whole grace window). White-box: arm one,
// close, and confirm the handle is cleared and m.wg has been released.
func TestHandleCloseCancelsTheGraceTimer(t *testing.T) {
	o, victim := newSnapshotOwner(baseSpec("spec-a", "model-a"), 4242, nil)
	o.evictionGrace = time.Hour // long, so the timer cannot fire during the test
	victim.lastUsed = time.Now()
	// So handleClose's beginDrain -> terminateNow returns at once (no real process
	// to signal): a closed exited channel reads as "already gone".
	closed := make(chan struct{})
	close(closed)
	victim.proc.exited = closed

	// A pinned spec blocked solely by the grace window: the grace waiter.
	o.specs["spec-b"] = &specState{spec: Spec{ID: "spec-b", Pinned: true}, state: StateStopped, waitingOnGrace: true}

	o.rescheduleGraceWake()
	if o.graceTimer == nil {
		t.Fatal("precondition: a grace timer should be armed for the pinned waiter")
	}

	o.handleClose()
	if o.graceTimer != nil {
		t.Error("handleClose left the grace timer armed; Close would block on it (m.wg) until it fired")
	}

	// m.wg must now be balanced (the cancelled timer released its Add), so a Wait
	// returns. Guard it with a deadline so a leak fails the test rather than hanging.
	waitDone := make(chan struct{})
	go func() { o.m.wg.Wait(); close(waitDone) }()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("m.wg.Wait() did not return after handleClose: the grace timer's goroutine was not released")
	}
}

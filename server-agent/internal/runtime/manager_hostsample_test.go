// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- re-evaluating admission when the hardware sample refreshes ---------------
//
// The host-RAM pool is built from the host's LIVE MemAvailable, so the verdict of
// a start that is waiting on RAM can change with nothing happening to any managed
// process: the sample that follows a victim's exit is the one that finally shows
// its freed memory. Admission is otherwise re-attempted only on a release, an
// exit, an Apply or a finished start, and SetHostInfo is a bare atomic store, so
// without a wake the beneficiary of a RAM eviction was re-admitted on the
// victim's exit against the previous, stale sample, did not fit, and then waited
// for an unrelated event or its whole admission timeout.

// settleHostWake blocks until the owner has consumed every wake SetHostInfo
// posted and finished acting on it. The pending token is cleared when the owner's
// select reads it, and the handler then runs to completion on the owner before it
// serves anything else, so once the buffer is empty a Status round trip cannot
// return before that handler has.
func settleHostWake(t *testing.T, m *Manager) {
	t.Helper()
	waitUntil(t, 5*time.Second, "the owner has consumed the host-sample wake", func() bool {
		return len(m.hostWake) == 0
	})
	_ = m.Status()
}

// TestHostSampleRefreshAdmitsTheBeneficiaryOfARAMEviction drives the sequence
// that used to hang, deterministically and without waiting for any timeout:
//
//  1. spec-a (10000 MB, idle) and spec-b (6000 MB, busy) run and are measured, so
//     the host reports 4000 MB available and the pool is 4000 + 16000 - 2000 =
//     18000.
//  2. spec-c overflows it, so the idle spec-a is drained for it. The moment
//     spec-a exits, spec-c is re-admitted against the STALE sample (still 4000
//     available, while spec-a's add-back has already left the pool): the budget
//     it sees is 4000 + 6000 - 2000 = 8000, and spec-c does not fit it. It waits,
//     and the test asserts it has not started -- and has not been refused.
//  3. A fresh sample arrives that reflects the freed RAM (14000 available). That
//     alone must admit spec-c: its admission timeout is 30 s and the test gives it
//     five, so only the wake on the sample can make this pass.
//
// Two demands are driven here. 8000 is exactly the stale budget (a plain Wait),
// and 9000 exceeds it -- the case that used to be refused outright as "exceeds
// the budget on its own" against a budget understated only by the victim that had
// just left, a durable error no later sample could undo. The one-MB-over boundary
// has its own test below.
func TestHostSampleRefreshAdmitsTheBeneficiaryOfARAMEviction(t *testing.T) {
	for _, demand := range []int{8000, 9000} {
		t.Run(itoaMB(demand), func(t *testing.T) {
			runRAMEvictionBeneficiary(t, demand)
		})
	}
}

// itoaMB names a subtest after a demand in MB.
func itoaMB(mb int) string { return fmt.Sprintf("candidate_%dMB", mb) }

// TestHostSampleRefreshAdmitsAnEvictionBeneficiaryLargerThanTheStaleBudget is
// the regression for the case the first version of the test above missed: the
// candidate that is ONE MB over the stale post-eviction budget. 8000 == the
// stale budget takes the Wait path (8000 > 8000 is false); 8001 used to land in
// the terminal own-demand refusal, so the beneficiary of a legitimate eviction
// got a durable ErrNotPermitted and its pending request was failed, with a
// "exceeds the host RAM budget on its own" message that was factually wrong --
// 8001 MB fits the pool the moment the sample catches up to the victim's freed
// RAM (18000 MB).
func TestHostSampleRefreshAdmitsAnEvictionBeneficiaryLargerThanTheStaleBudget(t *testing.T) {
	runRAMEvictionBeneficiary(t, 8001)
}

// runRAMEvictionBeneficiary is the shared body: see
// TestHostSampleRefreshAdmitsTheBeneficiaryOfARAMEviction.
func runRAMEvictionBeneficiary(t *testing.T, candidateMB int) {
	t.Helper()
	skipOnWindows(t)
	shrinkTimings(t)

	specA := baseSpec("spec-a", "model-a")
	specA.RAMMB = 10000
	specB := baseSpec("spec-b", "model-b")
	specB.RAMMB = 6000
	specC := baseSpec("spec-c", "model-c")
	specC.RAMMB = candidateMB
	specC.AdmissionWaitTimeoutSeconds = 30

	m := NewManager(ManagerOptions{
		Policy:           allowlistPolicy(),
		Getenv:           func(string) string { return "" },
		HostRAMReserveMB: 2000,
	})
	t.Cleanup(m.Close)
	var heldMB sync.Map // child pid -> MB it holds, filled in as children start
	m.SetRAMMeasurer(func(pids []int) map[int]int {
		out := make(map[int]int, len(pids))
		for _, p := range pids {
			if mb, ok := heldMB.Load(p); ok {
				out[p] = mb.(int)
			}
		}
		return out
	})
	m.SetHostInfo(HostInfo{MemAvailableMB: 20000}) // an idle host
	m.Apply(Config{
		Specs:      []Spec{specA, specB, specC},
		Coresident: [][2]string{{"spec-a", "spec-b"}, {"spec-a", "spec-c"}, {"spec-b", "spec-c"}},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := func(model, specID string, holdsMB int) func() {
		t.Helper()
		_, release, err := m.EnsureRunning(ctx, model)
		if err != nil {
			t.Fatalf("EnsureRunning(%s) = %v, want it admitted", model, err)
		}
		st := statusFor(m, specID)
		if st == nil || st.PID == 0 {
			t.Fatalf("Status()[%s] = %+v, want a live child", specID, st)
		}
		heldMB.Store(st.PID, holdsMB)
		waitUntil(t, 5*time.Second, specID+" reports the RAM it holds", func() bool {
			st := statusFor(m, specID)
			return st != nil && st.MeasuredRAMMB == holdsMB
		})
		return release
	}
	start("model-a", "spec-a", 10000)() // started, measured, then idle
	releaseB := start("model-b", "spec-b", 6000)
	defer releaseB() // busy for the whole test

	// The host reports what is left once a and b hold their 16000 MB.
	m.SetHostInfo(HostInfo{MemAvailableMB: 4000})
	settleHostWake(t, m)

	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() {
		_, release, err := m.EnsureRunning(ctx, "model-c")
		if err == nil {
			defer release()
		}
		done <- outcome{err}
		if err == nil {
			<-ctx.Done() // keep it busy until the test ends
		}
	}()

	waitUntil(t, 5*time.Second, "spec-a drained for spec-c's RAM", func() bool {
		st := statusFor(m, "spec-a")
		return st != nil && st.State == StateStopped
	})
	// spec-a's exit and the re-admission of its beneficiary run inside one owner
	// handler, and Status is served only after it, so the attempt on the stale
	// sample has already been made.
	if st := statusFor(m, "spec-c"); st == nil || st.State != StateStopped {
		t.Fatalf("Status()[spec-c] = %+v after spec-a's exit on the stale sample, want it still waiting (stopped): its demand does not fit the budget that sample shows, but it must stay a waiter -- a refusal here is durable and no later sample can undo it", st)
	}
	select {
	case o := <-done:
		t.Fatalf("EnsureRunning(model-c) returned %v before any fresh sample, want it still waiting (a refused request is failed and cannot be woken)", o.err)
	default:
	}

	// The next sample shows spec-a's 10000 MB as free again.
	m.SetHostInfo(HostInfo{MemAvailableMB: 14000})

	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("EnsureRunning(model-c) = %v, want it admitted by the refreshed sample", o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("EnsureRunning(model-c) was not admitted within 5s of the sample that shows the freed RAM, want it admitted promptly (its own admission timeout is 30s): a hardware sample must re-evaluate a start waiting on RAM")
	}
	if st := statusFor(m, "spec-c"); st == nil || (st.State != StateRunning && st.State != StateStarting) {
		t.Fatalf("Status()[spec-c] = %+v, want it started", st)
	}
	if st := statusFor(m, "spec-b"); st == nil || st.State != StateRunning {
		t.Fatalf("Status()[spec-b] = %+v, want the busy spec-b untouched", st)
	}
}

// TestHostSampleRefreshOnlyReevaluatesRAMWaiters is the "no wake storm" half.
// A sample lands about once a second for the life of the agent, and re-asking
// admission runs buildSnapshot -- which calls the installed VRAM measurer, a
// subprocess on a real host -- on the owner. So a sample may re-evaluate a spec
// only when the sample can change its verdict: a start whose last Wait the RAM
// pool contributed to. Nothing queued, or a start waiting on the co-residency
// matrix, must cost nothing.
//
// The VRAM measurer is the probe: it is invoked once per buildSnapshot, and no
// spec here declares a GPU, so the housekeeping beat never calls it.
func TestHostSampleRefreshOnlyReevaluatesRAMWaiters(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	newManager := func(t *testing.T, specs []Spec, pairs [][2]string) (*Manager, *atomic.Int64) {
		t.Helper()
		var admissions atomic.Int64
		m := NewManager(ManagerOptions{
			Policy:           allowlistPolicy(),
			Getenv:           func(string) string { return "" },
			HostRAMReserveMB: 2000,
		})
		t.Cleanup(m.Close)
		m.SetMeasurer(func(pids []int) map[int]map[int]int {
			admissions.Add(1)
			return nil
		})
		m.Apply(Config{Specs: specs, Coresident: pairs})
		return m, &admissions
	}
	samples := func(m *Manager, n, availableMB int) {
		for i := 0; i < n; i++ {
			m.SetHostInfo(HostInfo{MemAvailableMB: availableMB + i})
		}
	}
	// queued runs an EnsureRunning that is expected to stay queued until the
	// returned cancel is called, and waits until the owner has evaluated it.
	queued := func(t *testing.T, m *Manager, admissions *atomic.Int64, model string) (stop func()) {
		t.Helper()
		before := admissions.Load()
		ctx, cancel := context.WithCancel(context.Background())
		finished := make(chan struct{})
		go func() {
			defer close(finished)
			if _, release, err := m.EnsureRunning(ctx, model); err == nil {
				release()
			}
		}()
		waitUntil(t, 5*time.Second, model+" evaluated by admission", func() bool {
			return admissions.Load() > before
		})
		return func() {
			cancel()
			<-finished
		}
	}

	t.Run("nothing queued", func(t *testing.T) {
		specA := baseSpec("spec-a", "model-a")
		specA.RAMMB = 1000
		m, admissions := newManager(t, []Spec{specA}, nil)
		m.SetHostInfo(HostInfo{MemAvailableMB: 100000})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, release, err := m.EnsureRunning(ctx, "model-a")
		if err != nil {
			t.Fatalf("EnsureRunning(model-a) = %v", err)
		}
		release()

		settleHostWake(t, m)
		before := admissions.Load()
		samples(m, 25, 100000)
		settleHostWake(t, m)
		if got := admissions.Load(); got != before {
			t.Errorf("%d admission evaluations after 25 samples with nothing queued, want 0: a sample must cost nothing when no start is waiting", got-before)
		}
	})

	t.Run("a start waiting on the matrix is not woken", func(t *testing.T) {
		specA := baseSpec("spec-a", "model-a")
		specA.RAMMB = 1000
		specB := baseSpec("spec-b", "model-b")
		specB.RAMMB = 1000
		specB.AdmissionWaitTimeoutSeconds = 30
		m, admissions := newManager(t, []Spec{specA, specB}, nil) // the pair is closed
		m.SetHostInfo(HostInfo{MemAvailableMB: 100000})           // RAM is plentiful
		ctx, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelA()
		_, releaseA, err := m.EnsureRunning(ctx, "model-a")
		if err != nil {
			t.Fatalf("EnsureRunning(model-a) = %v", err)
		}
		defer releaseA() // busy, so spec-b can only wait

		stop := queued(t, m, admissions, "model-b")
		defer stop()
		settleHostWake(t, m)
		before := admissions.Load()
		samples(m, 25, 100000)
		settleHostWake(t, m)
		if got := admissions.Load(); got != before {
			t.Errorf("%d admission evaluations after 25 samples for a start blocked by the co-residency matrix, want 0: no sample can change that verdict", got-before)
		}
	})

	t.Run("a start waiting on RAM is re-evaluated by every sample and admitted once it fits", func(t *testing.T) {
		specA := baseSpec("spec-a", "model-a")
		specA.RAMMB = 10000
		specB := baseSpec("spec-b", "model-b")
		specB.RAMMB = 8000
		specB.AdmissionWaitTimeoutSeconds = 30
		m, admissions := newManager(t, []Spec{specA, specB}, [][2]string{{"spec-a", "spec-b"}})
		m.SetHostInfo(HostInfo{MemAvailableMB: 20000}) // pool 18000: a alone fits
		ctx, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelA()
		_, releaseA, err := m.EnsureRunning(ctx, "model-a")
		if err != nil {
			t.Fatalf("EnsureRunning(model-a) = %v", err)
		}
		defer releaseA() // busy: nothing to evict, so spec-b waits on RAM

		// 12000 available -> pool 10000 (nothing is measured, so nothing is
		// added back): 8000 + 10000 > 10000 and spec-a is busy.
		m.SetHostInfo(HostInfo{MemAvailableMB: 12000})
		settleHostWake(t, m)
		ctxB, cancelB := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancelB()
		result := make(chan error, 1)
		go func() {
			_, release, err := m.EnsureRunning(ctxB, "model-b")
			if err == nil {
				defer release()
			}
			result <- err
			if err == nil {
				<-ctxB.Done()
			}
		}()
		waitUntil(t, 5*time.Second, "spec-b evaluated by admission", func() bool {
			return admissions.Load() >= 2
		})
		settleHostWake(t, m)

		before := admissions.Load()
		m.SetHostInfo(HostInfo{MemAvailableMB: 12001}) // still too tight
		waitUntil(t, 5*time.Second, "a sample re-evaluates the RAM-blocked spec-b", func() bool {
			return admissions.Load() > before
		})
		select {
		case err := <-result:
			t.Fatalf("EnsureRunning(model-b) returned %v on a sample that still does not fit, want it still waiting", err)
		default:
		}

		m.SetHostInfo(HostInfo{MemAvailableMB: 30000}) // pool 28000: 18000 fits
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("EnsureRunning(model-b) = %v, want it admitted once the sample shows room", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("EnsureRunning(model-b) was not admitted within 5s of a sample that shows room for it")
		}
	})
}

// TestSetHostInfoNeverBlocksTheCaller pins the collect loop's side of the wake:
// SetHostInfo runs on the agent's collect goroutine once a sample, and it must
// never wait for the owner -- not while the owner is busy, not when many samples
// pile up faster than it can read them, and not after it has stopped. The wake is
// a coalescing buffered token, so a burst is one pending wake rather than a queue.
func TestSetHostInfoNeverBlocksTheCaller(t *testing.T) {
	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }})

	push := func(n int) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			for i := 0; i < n; i++ {
				m.SetHostInfo(HostInfo{MemAvailableMB: 1000 + i})
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("SetHostInfo blocked its caller within %d pushes", n)
		}
	}
	push(20000)
	if got := len(m.hostWake); got > 1 {
		t.Errorf("%d wakes pending, want the burst coalesced into at most 1", got)
	}

	m.Close()
	push(20000) // the owner has stopped reading: still never blocks
	if got, want := m.hostInfo.Load().MemAvailableMB, 1000+20000-1; got != want {
		t.Errorf("MemAvailableMB = %d after the last push, want %d: the store must still happen", got, want)
	}
}

// --- the ceiling the terminal RAM refusal compares against --------------------

// TestSnapshotHostRAMCeiling pins the generous twin of the budget. The budget adds
// back only what the managed processes have been MEASURED to hold, which is right
// for admission arithmetic but understates the pool for a moment whenever a
// process is loading or not yet measured; the ceiling counts such a process at its
// demand floor, so the durable own-demand refusal is never issued on the strength
// of an understated budget. In steady state, when everything is measured, the two
// are equal.
func TestSnapshotHostRAMCeiling(t *testing.T) {
	const (
		reserve   = 2000
		available = 10000
	)
	build := func(t *testing.T, mutate func(o *owner)) PolicySnapshot {
		t.Helper()
		spec := baseSpec("spec-a", "model-a")
		spec.RAMMB = 6000
		o, _ := newSnapshotOwner(spec, 4242, nil)
		o.hostRAMReserveMB = reserve
		o.m.SetHostInfo(HostInfo{MemAvailableMB: available})
		if mutate != nil {
			mutate(o)
		}
		return o.buildSnapshot()
	}

	t.Run("a measured process counts the same in both", func(t *testing.T) {
		snap := build(t, func(o *owner) { o.specs["spec-a"].measuredRAM = 5000 })
		if snap.HostRAMBudgetMB != available+5000-reserve || snap.HostRAMCeilingMB != snap.HostRAMBudgetMB {
			t.Errorf("budget %d, ceiling %d with one measured process, want both %d", snap.HostRAMBudgetMB, snap.HostRAMCeilingMB, available+5000-reserve)
		}
	})
	t.Run("a measured process below its demand is not inflated", func(t *testing.T) {
		// Counting the floor here would loosen the refusal for every spec whose
		// estimate exceeds what it really uses, not just for the unmeasured.
		snap := build(t, func(o *owner) { o.specs["spec-a"].measuredRAM = 3000 })
		if snap.HostRAMCeilingMB != snap.HostRAMBudgetMB {
			t.Errorf("ceiling %d, budget %d, want them equal for a measured process whatever its demand", snap.HostRAMCeilingMB, snap.HostRAMBudgetMB)
		}
	})
	t.Run("an unmeasured process counts at its demand in the ceiling only", func(t *testing.T) {
		snap := build(t, nil) // spec-a: demand 6000, no reading
		if want := available - reserve; snap.HostRAMBudgetMB != want {
			t.Errorf("budget = %d, want %d: an unmeasured process adds nothing back", snap.HostRAMBudgetMB, want)
		}
		if want := available + 6000 - reserve; snap.HostRAMCeilingMB != want {
			t.Errorf("ceiling = %d, want %d: an unmeasured process is taken to hold its demand", snap.HostRAMCeilingMB, want)
		}
	})
	t.Run("no sample means no ceiling either", func(t *testing.T) {
		o, _ := newSnapshotOwner(baseSpec("spec-a", "model-a"), 4242, nil)
		o.hostRAMReserveMB = reserve
		if got := o.buildSnapshot().HostRAMCeilingMB; got != 0 {
			t.Errorf("ceiling = %d with no hardware sample, want 0", got)
		}
	})
}

// TestRAMFreedCreditSettlesAfterTwoSamples pins the lifecycle of the credit for a
// process that has just exited, and what rides on it. The pool loses the process
// at once (it leaves the running set, so its add-back goes) but MemAvailable only
// gains the memory back with the next samples; the sample current at the exit may
// predate it and the next may have been in flight, so only the second sample after
// it is certain to show the freed memory. Until then the CEILING counts the freed
// RAM, so a candidate that fits once the host has caught up is not refused as too
// large; after that the ceiling is the plain pool again and a candidate that
// really is too large gets the terminal verdict. The admission BUDGET never gets
// the credit, so nothing is admitted on memory the host has not shown to be free.
func TestRAMFreedCreditSettlesAfterTwoSamples(t *testing.T) {
	const (
		reserve   = 2000
		available = 4000
	)
	specA := baseSpec("spec-a", "model-a") // the process that exits: held 10000
	specA.RAMMB = 10000
	specB := baseSpec("spec-b", "model-b") // stays: held 6000
	specB.RAMMB = 6000
	o, stB := newSnapshotOwner(specB, 4243, nil)
	stB.measuredRAM = 6000
	o.hostRAMReserveMB = reserve
	stA := &specState{spec: specA, state: StateRunning, proc: &runningProc{specID: "spec-a", pid: 4242}, measuredRAM: 10000}
	o.specs["spec-a"] = stA
	o.allowedPairs = allowPairs("cand", "spec-a", "spec-b")
	o.m.SetHostInfo(HostInfo{MemAvailableMB: 20000 - 16000}) // a and b hold 16000 of 20000

	snapshot := func() PolicySnapshot { return o.buildSnapshot() }
	if s := snapshot(); s.HostRAMBudgetMB != available+16000-reserve || s.HostRAMCeilingMB != s.HostRAMBudgetMB {
		t.Fatalf("before the exit: budget %d, ceiling %d, want both %d", s.HostRAMBudgetMB, s.HostRAMCeilingMB, available+16000-reserve)
	}

	// spec-a exits. The sample is still the old one.
	o.noteRAMFreed(stA)
	stA.proc, stA.measuredRAM = nil, 0
	stale := snapshot()
	if want := available + 6000 - reserve; stale.HostRAMBudgetMB != want {
		t.Errorf("budget right after the exit = %d, want the understated %d: the freed RAM must not enter the admission arithmetic", stale.HostRAMBudgetMB, want)
	}
	if want := available + 6000 + 10000 - reserve; stale.HostRAMCeilingMB != want {
		t.Errorf("ceiling right after the exit = %d, want %d: the exited process's RAM counts as already back", stale.HostRAMCeilingMB, want)
	}

	// A candidate over the stale budget but within what the host can offer is
	// not refused; it waits for the next sample.
	within := baseSpec("cand", "model-cand")
	within.RAMMB = 9000
	assertDecision(t, "within the ceiling, over the stale budget",
		Admit(stale, within), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})

	// One sample is not enough: it may have been taken before the exit.
	o.m.SetHostInfo(HostInfo{MemAvailableMB: available})
	if s := snapshot(); s.HostRAMCeilingMB != stale.HostRAMCeilingMB {
		t.Errorf("ceiling after ONE sample = %d, want the credit still counted (%d): that sample may predate the exit", s.HostRAMCeilingMB, stale.HostRAMCeilingMB)
	}

	// The second is certain to show the freed memory (14000 available).
	o.m.SetHostInfo(HostInfo{MemAvailableMB: 20000 - 6000})
	settled := snapshot()
	if want := 14000 + 6000 - reserve; settled.HostRAMBudgetMB != want || settled.HostRAMCeilingMB != want {
		t.Errorf("after two samples: budget %d, ceiling %d, want both %d", settled.HostRAMBudgetMB, settled.HostRAMCeilingMB, want)
	}
	// ...and now a genuinely oversized candidate gets the terminal verdict,
	// against a pool that is consistent with the running set.
	tooBig := baseSpec("cand", "model-cand")
	tooBig.RAMMB = 19000
	assertDecision(t, "over the settled pool", Admit(settled, tooBig), Decision{
		Reason:  StateNotPermitted,
		Message: "spec cand: RAM demand 19000 MB exceeds the host RAM budget 18000 MB on its own",
		Evict:   []string{},
	})
	assertDecision(t, "the earlier candidate now fits", Admit(settled, within), Decision{OK: true, Evict: []string{}})
}

// TestRAMFreedCreditIsNotRecordedWithoutASample: with no MemAvailable reading
// there is no pool to understate, so an exit before the first sample records
// nothing -- and a process with no demand and no reading frees nothing to credit.
func TestRAMFreedCreditIsNotRecordedWithoutASample(t *testing.T) {
	spec := baseSpec("spec-a", "model-a")
	spec.RAMMB = 5000
	o, st := newSnapshotOwner(spec, 4242, nil)
	o.noteRAMFreed(st)
	if got := o.pendingRAMFreedMB(); got != 0 {
		t.Errorf("credit = %d before any sample, want 0", got)
	}

	o.m.SetHostInfo(HostInfo{MemAvailableMB: 8000})
	noDemand := baseSpec("spec-b", "model-b")
	o2, st2 := newSnapshotOwner(noDemand, 4243, nil)
	o2.m.SetHostInfo(HostInfo{MemAvailableMB: 8000})
	o2.noteRAMFreed(st2)
	if got := o2.pendingRAMFreedMB(); got != 0 {
		t.Errorf("credit = %d for a process with neither demand nor reading, want 0", got)
	}

	// A measured process credits what was measured, an unmeasured one its demand.
	st.measuredRAM = 7000
	o.noteRAMFreed(st)
	if got := o.pendingRAMFreedMB(); got != 7000 {
		t.Errorf("credit = %d for a measured process, want its reading 7000", got)
	}
	st.measuredRAM = 0
	o.noteRAMFreed(st) // adds to the unsettled credit
	if got := o.pendingRAMFreedMB(); got != 7000+5000 {
		t.Errorf("credit = %d after a second exit inside the window, want 12000 (7000 measured + 5000 demand)", got)
	}
}

// TestBuildSnapshotMarksRAMBootstrapInFlight pins when buildSnapshot sets
// PolicySnapshot.RAMBootstrapInFlight -- the owner-serialized interlock signal
// Admit turns into a Wait for every other RAM admission (task 4.1). It is derived
// fresh from authoritative owner state (st.state, st.spec.RAMMB, st.measuredRAM,
// and whether a RAM measurer is installed) on every call, so it can never be a
// stale flag.
//
// The window is "a bootstrapping process is still unaccounted": unknown declared
// demand AND no real measurement (needsRAMBootstrap). WITH a RAM measurer it
// holds from Starting until the first measurement lands (the brief's
// "healthy+measured"); WITHOUT one no measurement ever arrives, so it is bounded
// by Starting and clears when the process goes healthy -- by then its RAM is
// resident and the conservative budget (MemAvailable has dropped, nothing was
// added back) accounts for it, so no interlock is needed and, crucially, it can
// never hang the host.
func TestBuildSnapshotMarksRAMBootstrapInFlight(t *testing.T) {
	// A RAM measurer is "installed" by its presence alone; its result does not
	// matter here (buildSnapshot reads the stored measuredRAM, not the measurer).
	installRAM := func(o *owner) { o.m.SetRAMMeasurer(func([]int) map[int]int { return nil }) }

	cases := []struct {
		name     string
		state    State
		ramMB    int // declared demand (0 = unknown)
		measured int
		measurer bool
		want     bool
	}{
		{"starting, unknown, unmeasured: in flight", StateStarting, 0, 0, true, true},
		{"starting, unknown, unmeasured, no measurer: still in flight", StateStarting, 0, 0, false, true},
		{"running, unknown, unmeasured, measurer: still in flight until measured", StateRunning, 0, 0, true, true},
		{"running, unknown, unmeasured, NO measurer: cleared at healthy (never hangs)", StateRunning, 0, 0, false, false},
		{"running, unknown, measured: settled, not in flight", StateRunning, 0, 5000, true, false},
		{"starting, known demand: reserved by rule 6, not a bootstrap", StateStarting, 8000, 0, true, false},
		{"running, known demand: not a bootstrap", StateRunning, 8000, 4000, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := baseSpec("spec-a", "model-a")
			spec.RAMMB = tc.ramMB
			o, st := newSnapshotOwner(spec, 4242, nil)
			o.m.SetHostInfo(HostInfo{MemAvailableMB: 40000})
			st.state = tc.state
			st.measuredRAM = tc.measured
			if tc.measurer {
				installRAM(o)
			}
			if got := o.buildSnapshot().RAMBootstrapInFlight; got != tc.want {
				t.Errorf("RAMBootstrapInFlight = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("no bootstrapping process means the flag is clear", func(t *testing.T) {
		spec := baseSpec("spec-a", "model-a")
		spec.RAMMB = 6000
		o, st := newSnapshotOwner(spec, 4242, nil)
		o.m.SetHostInfo(HostInfo{MemAvailableMB: 40000})
		st.measuredRAM = 6000
		if o.buildSnapshot().RAMBootstrapInFlight {
			t.Error("a fully-accounted host must not report a bootstrap in flight")
		}
	})
}

// TestAdmitAndStartClearsTheRAMWaitFlagOnEveryEarlyReturn: waitingOnRAM selects a
// spec for a sample wake, so it must never outlive the Wait it describes -- not
// even on the returns that precede any verdict (nobody wants the spec up, or it
// is force-stopped). A stale true there would let a sample re-run admission for a
// spec with no waiter.
func TestAdmitAndStartClearsTheRAMWaitFlagOnEveryEarlyReturn(t *testing.T) {
	for name, tc := range map[string]struct {
		spec func(*Spec)
	}{
		"nobody wants it up": {spec: func(s *Spec) {}},
		"force_stopped":      {spec: func(s *Spec) { s.Pinned = true; s.AdminState = "force_stopped" }},
	} {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec("spec-a", "model-a")
			tc.spec(&spec)
			o, _ := newSnapshotOwner(spec, 4242, nil)
			st := &specState{spec: spec, state: StateStopped, waitingOnRAM: true}
			o.specs["spec-a"] = st

			o.admitAndStart("spec-a")

			if st.waitingOnRAM {
				t.Error("waitingOnRAM is still true after an early return, want it cleared")
			}
		})
	}
}

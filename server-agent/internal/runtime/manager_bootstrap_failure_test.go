// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Task 4.4: classified, single-retry failure handling for the cold-start RAM
// bootstrap. The HARD anti-goal is that the memory-pressure escalation can NEVER
// become an unbounded evict-and-retry ("C3") storm: it is bounded to ONE guarded
// eviction per stuck bootstrap, composes with the existing crash-loop backoff,
// and never loops. These tests pin the classification, the single-retry bound,
// the genuine-crash fall-through, the no-measurement termination, the counter
// reset, and that a non-bootstrap spec is unaffected.

// coresidentAllPairs builds the Config.Coresident list that makes every spec in
// ids able to run beside every other -- rule 1 (matrix compatibility) otherwise
// evicts co-located specs regardless of RAM, which would hide the behaviour
// under test. (Even RAM-only specs are matrix-gated; see policy.Admit rule 1.)
func coresidentAllPairs(ids ...string) [][2]string {
	var pairs [][2]string
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			pairs = append(pairs, [2]string{ids[i], ids[j]})
		}
	}
	return pairs
}

// oomArgs builds stub-child argv that comes up (or not) per healthDelay and then
// SIGKILLs itself after crashAfter -- the stand-in for an OOM-kill the manager's
// memory-pressure classifier reads.
func oomArgs(healthDelay, crashAfter time.Duration, invocationLog string) []string {
	args := []string{
		"-port", "${PORT}",
		"-health-delay", healthDelay.String(),
		"-crash-after", crashAfter.String(),
		"-crash-signal",
	}
	if invocationLog != "" {
		args = append(args, "-invocation-log", invocationLog)
	}
	return args
}

func stoppedCount(m *Manager, ids ...string) int {
	n := 0
	for _, id := range ids {
		if st := statusFor(m, id); st != nil && st.State == StateStopped {
			n++
		}
	}
	return n
}

func runningCount(m *Manager, ids ...string) int {
	n := 0
	for _, id := range ids {
		if st := statusFor(m, id); st != nil && st.State == StateRunning {
			n++
		}
	}
	return n
}

// TestBootstrapDeathClassification pins the pure classifier: a bootstrap death is
// memory pressure on a SIGKILL/OOM signature OR when the last RSS reached the
// near-budget fraction, and a genuine crash otherwise. It reads the exit
// signature + the last measurement, never free text.
func TestBootstrapDeathClassification(t *testing.T) {
	cases := []struct {
		name        string
		oomSignaled bool
		measuredRAM int
		budget      int
		want        bool
	}{
		{"SIGKILL with no budget/measurement is still memory pressure", true, 0, 0, true},
		{"SIGKILL overrides a tiny RSS", true, 1, 100000, true},
		{"RSS at 90% of budget is memory pressure", false, 9000, 10000, true},
		{"RSS above budget is memory pressure", false, 11000, 10000, true},
		{"RSS at 89% of budget is a genuine crash", false, 8900, 10000, false},
		{"plain non-zero exit, no measurement, no budget is a genuine crash", false, 0, 0, false},
		{"a large RSS but no budget cannot classify as pressure", false, 50000, 0, false},
		{"a tight budget but no measurement cannot classify as pressure", false, 0, 10000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bootstrapDiedOfMemoryPressure(tc.oomSignaled, tc.measuredRAM, tc.budget); got != tc.want {
				t.Errorf("bootstrapDiedOfMemoryPressure(%v, %d, %d) = %v, want %v",
					tc.oomSignaled, tc.measuredRAM, tc.budget, got, tc.want)
			}
		})
	}
}

// TestBootstrapMemoryPressureRetriesOnce is the headline anti-C3-storm test. A
// bootstrap (unknown RAM) whose child dies of memory pressure (SIGKILL) triggers
// exactly ONE evict-and-retry: the first death evicts a single idle process and
// retries the start once; a SECOND consecutive memory-pressure death goes to
// StateStartFailed -- NOT a third start, and NOT a second eviction. With two
// idle victims present, a storm would evict both; the bound evicts exactly one.
func TestBootstrapMemoryPressureRetriesOnce(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	execs := newExecCounter(t)

	victimA := baseSpec("victim-a", "model-victim-a")
	victimA.RAMMB = 500
	victimB := baseSpec("victim-b", "model-victim-b")
	victimB.RAMMB = 500

	boot := baseSpec("boot", "model-boot")
	boot.RAMMB = 0 // unknown RAM: this is a cold-start bootstrap
	// Never becomes healthy before it SIGKILLs itself: health-delay far beyond
	// the crash, crash far inside the 5s startup timeout. So every generation
	// dies of "OOM" before health, and the queued request stays pending across
	// the one guarded retry.
	boot.Args = oomArgs(10*time.Second, 40*time.Millisecond, "")
	boot.StartupTimeoutSeconds = 5
	boot.AdmissionWaitTimeoutSeconds = 10

	m.Apply(Config{
		Specs:      []Spec{victimA, victimB, boot},
		Coresident: coresidentAllPairs("victim-a", "victim-b", "boot"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Two idle victims, both evictable.
	_, relA, err := m.EnsureRunning(ctx, "model-victim-a")
	if err != nil {
		t.Fatalf("EnsureRunning(victim-a): %v", err)
	}
	relA()
	_, relB, err := m.EnsureRunning(ctx, "model-victim-b")
	if err != nil {
		t.Fatalf("EnsureRunning(victim-b): %v", err)
	}
	relB()

	// The bootstrap request: start #1 OOMs -> one evict-and-retry -> start #2
	// OOMs -> StateStartFailed, the request fails. It must never reach a third
	// start.
	_, _, err = m.EnsureRunning(ctx, "model-boot")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("EnsureRunning(model-boot) = %v, want ErrStartFailed after one evict-and-retry then a bounded failure", err)
	}

	if got := execs.count("boot"); got != 2 {
		t.Fatalf("boot exec count = %d, want exactly 2 (initial start + ONE retry) -- a third start is the C3 storm this bound exists to prevent", got)
	}
	// Exactly one victim evicted: a storm would have evicted both.
	if got := stoppedCount(m, "victim-a", "victim-b"); got != 1 {
		t.Fatalf("evicted victims = %d, want exactly 1 (the single guarded eviction); a second eviction is the storm", got)
	}
	if got := runningCount(m, "victim-a", "victim-b"); got != 1 {
		t.Fatalf("surviving running victims = %d, want exactly 1 -- the second idle process must never be evicted for the same stuck bootstrap", got)
	}
	st := statusFor(m, "boot")
	if st == nil || (st.State != StateStartFailed && st.State != StateBackoff) {
		t.Fatalf("Status()[boot].State = %+v, want start_failed or backoff after the bounded failure", st)
	}
}

// TestBootstrapMemoryPressureEvictionBoundedUnderRepeatedOOM is the thorough
// anti-storm proof: a PINNED bootstrap that SIGKILLs itself on EVERY generation
// restarts forever (rate-limited by backoff), but the one guarded eviction is
// spent once and never again -- so a second idle victim is NEVER evicted no
// matter how many times the child OOMs. That is precisely the fork-exec storm
// the bound forbids.
func TestBootstrapMemoryPressureEvictionBoundedUnderRepeatedOOM(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	victimA := baseSpec("victim-a", "model-victim-a")
	victimA.RAMMB = 500
	victimB := baseSpec("victim-b", "model-victim-b")
	victimB.RAMMB = 500

	boot := baseSpec("boot", "model-boot")
	boot.RAMMB = 0
	boot.Pinned = true // keeps restarting with no queued request
	boot.Args = oomArgs(10*time.Second, 30*time.Millisecond, "")
	boot.StartupTimeoutSeconds = 5

	m.Apply(Config{
		Specs:      []Spec{victimA, victimB, boot},
		Coresident: coresidentAllPairs("victim-a", "victim-b", "boot"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, relA, err := m.EnsureRunning(ctx, "model-victim-a")
	if err != nil {
		t.Fatalf("EnsureRunning(victim-a): %v", err)
	}
	relA()
	_, relB, err := m.EnsureRunning(ctx, "model-victim-b")
	if err != nil {
		t.Fatalf("EnsureRunning(victim-b): %v", err)
	}
	relB()

	// Let the pinned bootstrap OOM-and-restart many times over ~1.5s.
	time.Sleep(1500 * time.Millisecond)

	// However many times the bootstrap has OOM'd, only ONE victim may ever have
	// been evicted. The other must still be running.
	if got := stoppedCount(m, "victim-a", "victim-b"); got > 1 {
		t.Fatalf("evicted victims = %d after repeated bootstrap OOMs, want at most 1 -- the escalation is evicting a fresh idle process on every failure (the C3 storm)", got)
	}
	if got := runningCount(m, "victim-a", "victim-b"); got < 1 {
		t.Fatalf("surviving running victims = %d, want at least 1 -- the bounded escalation must leave the second idle process alone", got)
	}
}

// TestBootstrapMemoryPressureWithoutDemandDoesNotEvict: the escalation frees RAM
// for a RETRY, so when nothing wants the bootstrap up (it served its request,
// was released, then OOM'd with nothing queued and no pin) there is no retry to
// help and no innocent neighbour is evicted. The death is still recorded, but as
// an ordinary crash.
func TestBootstrapMemoryPressureWithoutDemandDoesNotEvict(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	victim := baseSpec("victim", "model-victim")
	victim.RAMMB = 500

	boot := baseSpec("boot", "model-boot")
	boot.RAMMB = 0
	// Healthy immediately, serves, then OOMs ~300ms later -- by which time its
	// request has been released and nothing wants it up.
	boot.Args = oomArgs(0, 300*time.Millisecond, "")

	m.Apply(Config{
		Specs:      []Spec{victim, boot},
		Coresident: coresidentAllPairs("victim", "boot"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, relV, err := m.EnsureRunning(ctx, "model-victim")
	if err != nil {
		t.Fatalf("EnsureRunning(victim): %v", err)
	}
	relV()

	_, relB, err := m.EnsureRunning(ctx, "model-boot")
	if err != nil {
		t.Fatalf("EnsureRunning(model-boot) = %v, want it to come up healthy and serve", err)
	}
	relB() // idle now: nothing wants it up when it OOMs

	// Wait for the bootstrap to OOM and leave Running.
	waitUntil(t, 5*time.Second, "the idle bootstrap OOMs and leaves running", func() bool {
		st := statusFor(m, "boot")
		return st != nil && st.State != StateRunning && st.State != StateStarting
	})

	// The victim must be untouched: no retry was wanted, so no eviction.
	if st := statusFor(m, "victim"); st == nil || st.State != StateRunning {
		t.Fatalf("Status()[victim].State = %+v, want running -- an escalation with no retry to help must evict nobody", st)
	}
}

// TestBootstrapCrashUsesBackoffNotEscalation: a plain non-zero exit (a config /
// binary error, not memory pressure) takes the EXISTING StateStartFailed
// crash-loop backoff, unchanged -- no eviction, no escalation.
func TestBootstrapCrashUsesBackoffNotEscalation(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	victim := baseSpec("victim", "model-victim")
	victim.RAMMB = 500

	boot := baseSpec("boot", "model-boot")
	boot.RAMMB = 0
	// A plain non-zero exit before health: a genuine crash, no signal, no
	// measurement, no budget -> not memory pressure.
	boot.Args = stubArgs(10*time.Second, 40*time.Millisecond, 3, "")
	boot.StartupTimeoutSeconds = 5

	m.Apply(Config{
		Specs:      []Spec{victim, boot},
		Coresident: coresidentAllPairs("victim", "boot"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, rel, err := m.EnsureRunning(ctx, "model-victim")
	if err != nil {
		t.Fatalf("EnsureRunning(victim): %v", err)
	}
	rel()

	_, _, err = m.EnsureRunning(ctx, "model-boot")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("EnsureRunning(model-boot) = %v, want ErrStartFailed via the ordinary crash path", err)
	}

	// The idle victim must be untouched: a genuine crash never evicts.
	if st := statusFor(m, "victim"); st == nil || st.State != StateRunning {
		t.Fatalf("Status()[victim].State = %+v, want running -- a genuine crash must not evict anything", st)
	}
	st := statusFor(m, "boot")
	if st == nil || (st.State != StateStartFailed && st.State != StateBackoff) {
		t.Fatalf("Status()[boot].State = %+v, want start_failed or backoff", st)
	}
	if st.LastError == nil {
		t.Fatal("Status()[boot].LastError is nil, want the crash recorded")
	}
}

// TestBootstrapNoMeasurementWithinDeadlineFails: a bootstrap that comes up
// healthy but is never measured would hold the RAMBootstrapInFlight interlock
// forever (and that interlock makes every other RAM admission Wait). After the
// bounded number of fruitless measurement beats it is forced to StateStartFailed.
func TestBootstrapNoMeasurementWithinDeadlineFails(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	// A RAM measurer that is installed but never reports a reading for the
	// bootstrap's pid: the measurement that would resolve the bootstrap never
	// arrives.
	m.SetRAMMeasurer(func(pids []int) map[int]int { return map[int]int{} })

	boot := baseSpec("boot", "model-boot")
	boot.RAMMB = 0 // unknown RAM: a cold-start bootstrap
	// Healthy immediately, never crashes -- the only thing wrong is that it is
	// never measured.
	boot.Args = stubArgs(0, 0, 0, "")
	m.Apply(Config{Specs: []Spec{boot}})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// Comes up healthy.
	_, rel, err := m.EnsureRunning(ctx, "model-boot")
	if err != nil {
		t.Fatalf("EnsureRunning(model-boot) = %v, want it to come up healthy first", err)
	}
	defer rel()

	waitUntil(t, 5*time.Second, "a never-measured healthy bootstrap is forced to a bounded failure", func() bool {
		st := statusFor(m, "boot")
		if st == nil {
			return false
		}
		return (st.State == StateStartFailed || st.State == StateBackoff) &&
			st.LastError != nil
	})
}

// TestBootstrapRetryCounterResetsOnMeasurement: a successful RAM measurement
// resolves the bootstrap and resets both the memory-pressure escalation counter
// and the no-measurement termination counter, so a later, unrelated pressure
// event earns the one guarded retry again.
func TestBootstrapRetryCounterResetsOnMeasurement(t *testing.T) {
	const pid = 7777
	spec := baseSpec("boot", "model-boot")
	spec.RAMMB = 0 // bootstrap

	o, st := newSnapshotOwner(spec, pid, nil)
	st.ramBootstrapRetries = 1
	st.ramBootstrapBeats = 4

	o.applyMeasurement(cmdMeasured{
		targets:  []measureTarget{{specID: spec.ID, proc: st.proc, pid: pid}},
		ramByPID: map[int]int{pid: 1800},
	})

	if st.measuredRAM != 1800 {
		t.Fatalf("st.measuredRAM = %d, want 1800 -- the premise (a real measurement landed) does not hold", st.measuredRAM)
	}
	if st.ramBootstrapRetries != 0 {
		t.Errorf("st.ramBootstrapRetries = %d after a successful measurement, want 0 (reset)", st.ramBootstrapRetries)
	}
	if st.ramBootstrapBeats != 0 {
		t.Errorf("st.ramBootstrapBeats = %d after a successful measurement, want 0 (reset)", st.ramBootstrapBeats)
	}
}

// TestNonBootstrapDeathDoesNotEscalate: a spec with a KNOWN RAM demand is not a
// bootstrap, so even a SIGKILL/OOM death takes the ordinary crash path -- it
// never evicts an idle process. The escalation is strictly a cold-start concern.
func TestNonBootstrapDeathDoesNotEscalate(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	victim := baseSpec("victim", "model-victim")
	victim.RAMMB = 500

	known := baseSpec("known", "model-known")
	known.RAMMB = 500 // KNOWN demand: not a bootstrap
	known.Args = oomArgs(10*time.Second, 40*time.Millisecond, "")
	known.StartupTimeoutSeconds = 5

	m.Apply(Config{
		Specs:      []Spec{victim, known},
		Coresident: coresidentAllPairs("victim", "known"),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, rel, err := m.EnsureRunning(ctx, "model-victim")
	if err != nil {
		t.Fatalf("EnsureRunning(victim): %v", err)
	}
	rel()

	_, _, err = m.EnsureRunning(ctx, "model-known")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("EnsureRunning(model-known) = %v, want ErrStartFailed via the ordinary crash path", err)
	}

	if st := statusFor(m, "victim"); st == nil || st.State != StateRunning {
		t.Fatalf("Status()[victim].State = %+v, want running -- a known-RAM spec's death must not evict (it is not a bootstrap)", st)
	}
}

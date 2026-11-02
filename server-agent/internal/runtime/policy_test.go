// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"reflect"
	"testing"
	"time"
)

// TestPairKeyCanonical asserts PairKey orders its two arguments
// lexicographically regardless of call order, so an Allowed lookup never
// cares which side is the candidate and which is the already-running
// process.
func TestPairKeyCanonical(t *testing.T) {
	if got, want := PairKey("a", "b"), ([2]string{"a", "b"}); got != want {
		t.Errorf("PairKey(a, b) = %v, want %v", got, want)
	}
	if got, want := PairKey("b", "a"), ([2]string{"a", "b"}); got != want {
		t.Errorf("PairKey(b, a) = %v, want %v", got, want)
	}
	if PairKey("a", "b") != PairKey("b", "a") {
		t.Error("PairKey must be order-independent")
	}
	if got, want := PairKey("z", "m"), ([2]string{"m", "z"}); got != want {
		t.Errorf("PairKey(z, m) = %v, want %v", got, want)
	}
	if got, want := PairKey("x", "x"), ([2]string{"x", "x"}); got != want {
		t.Errorf("PairKey(x, x) = %v, want %v", got, want)
	}
}

// t0..t3 are strictly increasing LastUsed timestamps, oldest first, shared
// across the admission table below.
var (
	t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 = t0.Add(1 * time.Hour)
	t2 = t0.Add(2 * time.Hour)
	t3 = t0.Add(3 * time.Hour)
)

func allowPairs(candidateID string, otherIDs ...string) map[[2]string]bool {
	allowed := make(map[[2]string]bool, len(otherIDs))
	for _, other := range otherIDs {
		allowed[PairKey(candidateID, other)] = true
	}
	return allowed
}

func assertDecision(t *testing.T, name string, got, want Decision) {
	t.Helper()
	if got.OK != want.OK {
		t.Errorf("%s: OK = %v, want %v", name, got.OK, want.OK)
	}
	if got.Wait != want.Wait {
		t.Errorf("%s: Wait = %v, want %v", name, got.Wait, want.Wait)
	}
	if got.WaitOnRAM != want.WaitOnRAM {
		t.Errorf("%s: WaitOnRAM = %v, want %v", name, got.WaitOnRAM, want.WaitOnRAM)
	}
	if got.Reason != want.Reason {
		t.Errorf("%s: Reason = %q, want %q", name, got.Reason, want.Reason)
	}
	if got.Message != want.Message {
		t.Errorf("%s: Message = %q, want %q", name, got.Message, want.Message)
	}
	if got.Evict == nil {
		t.Errorf("%s: Evict = nil, want a non-nil slice (empty or populated)", name)
	}
	if !reflect.DeepEqual(got.Evict, want.Evict) {
		t.Errorf("%s: Evict = %v, want %v", name, got.Evict, want.Evict)
	}
}

// TestAdmit is table-driven over PolicySnapshot x Spec -> Decision, covering
// the admission rules in
// docs/architecture/cross-cutting/agent-runtime-manager.md §5.2 "The three
// gates" (matrix, process limit, per-GPU arithmetic) and §5.3's
// unknown-VRAM precedence (case 11). Each case asserts the FULL Decision
// (OK, Wait, Reason, Message, and the exact Evict slice in order) --
// checking OK alone would pass against an implementation that evicts the
// wrong process, the wrong number of them, or in the wrong order; leaving
// Message unchecked would pass against an implementation that leaked
// disambiguating text onto the wrong path (every case in this table wants
// Message == "", the zero value, except case 11's precedence Wait -- so
// assertDecision's check of it is exercised in both directions here, not just
// in the dedicated Message tests).
func TestAdmit(t *testing.T) {
	noGPUs := Spec{ID: "cand"}

	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			// 1: empty running set, budget fits -> OK.
			name: "empty running set budget fits",
			snap: PolicySnapshot{
				Running: nil,
				Budgets: map[int]int{0: 2000},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// 2: pair not allowed, blocker idle -> Evict [blocker].
			name: "matrix disallowed idle blocker is evicted",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "blocker", InFlight: 0, LastUsed: t0},
				},
				// Allowed left empty: the (cand, blocker) pair is disallowed.
			},
			spec: noGPUs,
			want: Decision{Evict: []string{"blocker"}},
		},
		{
			// 3: pair not allowed, blocker busy -> Wait.
			name: "matrix disallowed busy blocker waits",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "blocker", InFlight: 1, LastUsed: t0},
				},
			},
			spec: noGPUs,
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// 4: pair not allowed, blocker pinned+idle -> Wait (pinned never evicted).
			name: "matrix disallowed pinned blocker waits",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "blocker", InFlight: 0, Pinned: true, LastUsed: t0},
				},
			},
			spec: noGPUs,
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// 5: pair allowed, budget exceeded on gpu 0 -> evict oldest idle
			// toucher of gpu 0 only (one eviction is enough to fit).
			name: "budget exceeded evicts oldest idle toucher",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "old", GPUs: map[int]int{0: 4000}, LastUsed: t0},
					{SpecID: "new", GPUs: map[int]int{0: 4000}, LastUsed: t1},
				},
				Budgets: map[int]int{0: 10000},
				Allowed: allowPairs("cand", "old", "new"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}}},
			want: Decision{Evict: []string{"old"}},
		},
		{
			// 6: pair allowed, budget exceeded, all busy -> Wait.
			name: "budget exceeded all busy waits",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "old", GPUs: map[int]int{0: 4000}, InFlight: 1, LastUsed: t0},
					{SpecID: "new", GPUs: map[int]int{0: 4000}, InFlight: 2, LastUsed: t1},
				},
				Budgets: map[int]int{0: 10000},
				Allowed: allowPairs("cand", "old", "new"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}}},
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// 7: disjoint GPUs never compete -> OK. Two OTHER gpus (0 and 2)
			// are already full; the candidate only touches gpu 1, which has
			// no touchers at all. An implementation that (incorrectly) sums
			// a running process's total VRAM against every budget it checks,
			// instead of only the GPUs that process actually touches, would
			// wrongly see 8000+8000+5000=21000 > budget(1)=10000 and refuse.
			name: "disjoint gpus do not compete",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "gpu0-hog", GPUs: map[int]int{0: 8000}, LastUsed: t0},
					{SpecID: "gpu2-hog", GPUs: map[int]int{2: 8000}, LastUsed: t1},
				},
				Budgets: map[int]int{0: 8000, 1: 10000, 2: 8000},
				Allowed: allowPairs("cand", "gpu0-hog", "gpu2-hog"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 1, VRAMMB: 5000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// 8: process limit reached, idle victim exists -> evict oldest.
			name: "process limit evicts oldest idle",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "old", LastUsed: t0},
					{SpecID: "new", LastUsed: t1},
				},
				MaxProcesses: 2,
				Allowed:      allowPairs("cand", "old", "new"),
			},
			spec: noGPUs,
			want: Decision{Evict: []string{"old"}},
		},
		{
			// 9: process limit 0 -> unlimited, OK.
			name: "process limit zero is unlimited",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "p1", LastUsed: t0},
					{SpecID: "p2", LastUsed: t1},
					{SpecID: "p3", LastUsed: t2},
				},
				MaxProcesses: 0,
				Allowed:      allowPairs("cand", "p1", "p2", "p3"),
			},
			spec: noGPUs,
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// 10: unknown VRAM, gpu empty -> OK (measure-alone rule).
			name: "unknown vram empty gpu is ok",
			snap: PolicySnapshot{
				Running: nil,
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// 11: unknown VRAM, gpu occupied by an idle occupant of KNOWN
			// demand -> Wait.
			//
			// CHANGED DELIBERATELY, from `Evict: []string{"occupant"}` to
			// Wait. The brief's original expectation was the eviction, and it
			// is one half of a convergence defect: rule 5 evicts an
			// unknown-demand occupant for this occupant's sake, so with rule 4
			// evicting in the other direction the pair thrashed forever (see
			// TestAdmitMixedPairConverges, and the block comment above it).
			// The operator's resolution -- known demand beats unknown demand
			// -- takes rule 4's eviction right away and leaves it the refusal
			// to share, so the candidate now waits for the idle timeout of the
			// spec whose estimate is filled in instead of drain-stopping it.
			// Note what did NOT change: case 12 below, where the same occupant
			// is pinned, is still the terminal.
			name: "unknown vram occupied by idle known occupant waits",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0},
				},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}},
			// The one Wait in this table that carries a message: the occupant
			// it may not evict need never leave on its own
			// (TestAdmitPrecedenceWaitNamesWhatItIsWaitingFor).
			want: Decision{
				Wait:    true,
				Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it",
				Evict:   []string{},
			},
		},
		{
			// 12: unknown VRAM, gpu occupied by pinned -> Reason:
			// pending_vram_unknown. Neither evictable nor resolvable by
			// waiting (a pinned process never finishes), so it is reported
			// distinctly from ordinary Wait.
			name: "unknown vram occupied by pinned is pending_vram_unknown",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 5000}, Pinned: true, LastUsed: t0},
				},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}},
			want: Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}},
		},
		{
			// 13: multi-gpu spec (tensor parallel), one gpu over budget ->
			// evicts on that gpu only, leaving the other gpu's (fitting)
			// toucher alone.
			name: "tensor parallel evicts only the overbudget gpu",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "gpu0-old", GPUs: map[int]int{0: 9000}, LastUsed: t0},
					{SpecID: "gpu1-proc", GPUs: map[int]int{1: 5000}, LastUsed: t1},
				},
				Budgets: map[int]int{0: 10000, 1: 10000},
				Allowed: allowPairs("cand", "gpu0-old", "gpu1-proc"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}, {Index: 1, VRAMMB: 3000}}},
			want: Decision{Evict: []string{"gpu0-old"}},
		},
		{
			// 14: eviction must pick the OLDEST LastUsed among idle
			// candidates. Three candidates with distinct LastUsed values so
			// an implementation that picks an arbitrary map-iteration victim
			// fails instead of passing by luck.
			name: "eviction picks globally oldest among three",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "p3-newest", LastUsed: t3},
					{SpecID: "p1-oldest", LastUsed: t1},
					{SpecID: "p2-middle", LastUsed: t2},
				},
				MaxProcesses: 3,
				Allowed:      allowPairs("cand", "p3-newest", "p1-oldest", "p2-middle"),
			},
			spec: noGPUs,
			want: Decision{Evict: []string{"p1-oldest"}},
		},
		{
			// 15a: measured VRAM in RunningProc.GPUs is what counts. A low
			// measured value fits the budget.
			name: "measured vram low value fits budget",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0},
				},
				Budgets: map[int]int{0: 10000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 2000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// 15b: the same occupant, but with a higher measured value
			// (what an inflated estimate would have reported) now overflows
			// the same budget -- proving Admit trusts the RunningProc.GPUs
			// number exactly, not a fixed or ignored placeholder.
			name: "measured vram high value overflows budget",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 9000}, LastUsed: t0},
				},
				Budgets: map[int]int{0: 10000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 2000}}},
			want: Decision{Evict: []string{"occupant"}},
		},
		{
			// 16: budget missing for a touched gpu -> that gpu is
			// unconstrained, never a ceiling of zero (which is what a Go
			// map's zero value would silently supply). A huge existing
			// occupant does not block the candidate. An explicit budget of
			// 0 must reach the same decision -- see
			// TestAdmitZeroBudgetIsUnconstrained, which asserts the two
			// cases are identical.
			name: "missing budget is unconstrained not zero",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 999999}, LastUsed: t0},
				},
				Budgets: map[int]int{}, // gpu 0 absent
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// Review finding (Important 1): every case above evicts at most
			// one victim, so the final sortOldestFirst(victims) call at the
			// end of Admit -- the one place that orders the merged,
			// map-built victim set -- is never exercised; deleting that
			// line left the whole suite green. This case forces TWO
			// victims from TWO different rules (budget evicts "oldest" for
			// gpu 0; the process-count rule, evaluated after budget's
			// eviction already lowers the deficit by one, evicts exactly
			// one more from what remains) so the merge-then-sort step is
			// actually asserted, deliberately with Running listed out of
			// LastUsed order (t2, t0, t1) so relying on input order instead
			// of sorting would also fail.
			name: "multi-rule eviction set is sorted oldest first",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "newest", LastUsed: t2}, // does not touch gpu 0
					{SpecID: "oldest", GPUs: map[int]int{0: 8000}, LastUsed: t0},
					{SpecID: "middle", LastUsed: t1}, // does not touch gpu 0
				},
				MaxProcesses: 2,
				Budgets:      map[int]int{0: 10000},
				Allowed:      allowPairs("cand", "newest", "oldest", "middle"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}}},
			want: Decision{Evict: []string{"oldest", "middle"}},
		},
		{
			// Review finding ("also worth closing"): no other case has a
			// running process spanning multiple GPUs, so summing a
			// toucher's WHOLE GPUs map instead of only the index under
			// test (a narrower bug than case 7's fully-global sum) would
			// not be caught. "multi" occupies both gpu 0 (5000) and gpu 1
			// (4000); the candidate touches only gpu 0. Using only the
			// touched index (5000) fits comfortably under budget (OK); a
			// buggy sum of the whole map (9000) would not (3000+9000 =
			// 12000 > 10000, wrongly evicting "multi").
			name: "running process spanning multiple gpus only its touched index counts",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "multi", GPUs: map[int]int{0: 5000, 1: 4000}, LastUsed: t0},
				},
				Budgets: map[int]int{0: 10000},
				Allowed: allowPairs("cand", "multi"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Admit(c.snap, c.spec)
			assertDecision(t, c.name, got, c.want)
		})
	}
}

// TestAdmitUnknownVRAMBusyBlockerWaits extends the unknown-VRAM rule beyond
// the brief's pinned/idle pair: a BUSY (non-pinned) occupant of the
// candidate's gpu can still finish on its own, so the outcome is an
// ordinary Wait, not the terminal pending_vram_unknown reason (which is
// reserved for a holder that can never leave).
func TestAdmitUnknownVRAMBusyBlockerWaits(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 5000}, InFlight: 1, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	got := Admit(snap, spec)
	// It is still the precedence block -- the occupant has a real number and
	// this candidate does not -- so it carries rule 4's message even though
	// the occupant being busy means this particular wait does resolve itself.
	assertDecision(t, "unknown vram busy blocker waits", got, Decision{
		Wait:    true,
		Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it",
		Evict:   []string{},
	})
}

// TestAdmitEmptyRunningNeverNilEvict confirms Decision.Evict is always a
// non-nil slice, even on the plain OK path with no running processes at
// all -- a consumer that marshals Decision must never see Evict as JSON
// null.
func TestAdmitEmptyRunningNeverNilEvict(t *testing.T) {
	got := Admit(PolicySnapshot{}, Spec{ID: "cand"})
	if got.Evict == nil {
		t.Fatal("Admit().Evict = nil, want non-nil empty slice")
	}
	if len(got.Evict) != 0 {
		t.Fatalf("Admit().Evict = %v, want empty", got.Evict)
	}
	if !got.OK {
		t.Fatalf("Admit() with nothing running and no constraints should be OK, got %+v", got)
	}
}

// --- Review round 1 fixes -----------------------------------------------
//
// The four tests below cover Important 2 from the task-12 review: a
// RunningProc sharing the candidate's own SpecID (a double-start race, or a
// snapshot assembled a moment too early/late) must never be treated as a
// foreign blocker or proposed as its own victim, in ANY of the four rules.
// Each test disables every OTHER rule (no budget, no process limit, no
// unknown VRAM unless under test) so a self-skip regression in that one
// rule cannot hide behind another rule reaching the same OK conclusion.

// TestAdmitNeverProposesSelfAsMatrixVictim: without the self-skip, rule 1
// finds no self-pair in Allowed (the gateway's coresident matrix never
// contains one), so an idle self would read as matrix-incompatible with
// itself and get proposed as its own eviction victim.
func TestAdmitNeverProposesSelfAsMatrixVictim(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", InFlight: 0, LastUsed: t0},
		},
	}
	got := Admit(snap, Spec{ID: "cand"})
	assertDecision(t, "self is never its own matrix victim", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitNeverWaitsOnBusySelf: the busy-self counterpart -- without the
// self-skip this would return Wait forever, since the "blocker" (itself)
// can never finish waiting on... itself.
func TestAdmitNeverWaitsOnBusySelf(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", InFlight: 5, LastUsed: t0},
		},
	}
	got := Admit(snap, Spec{ID: "cand"})
	assertDecision(t, "busy self never causes a permanent wait", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitUnknownVRAMSelfIsNotAnOccupant: rule 4's toucher scan (and its
// pinned short-circuit) must also skip self -- an already-running instance
// of the very spec being measured is not "another process in the way" of
// measuring it alone.
func TestAdmitUnknownVRAMSelfIsNotAnOccupant(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", GPUs: map[int]int{0: 5000}, Pinned: true, LastUsed: t0},
		},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	got := Admit(snap, spec)
	assertDecision(t, "self is never an unknown-vram occupant", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitSelfNotCountedTowardProcessLimit: without the self-skip, the
// process-count rule would see the sole running entry (itself) as both
// "already running" (contributing to len(Running)) AND, since it is idle
// and not otherwise in toEvict, an eviction "candidate" -- proposing to
// drain-stop the very process the caller is trying to start, to make room
// for... that same process.
func TestAdmitSelfNotCountedTowardProcessLimit(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", LastUsed: t0},
		},
		MaxProcesses: 1,
	}
	got := Admit(snap, Spec{ID: "cand"})
	assertDecision(t, "self does not count against MaxProcesses", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitSelfVRAMNotDoubleCountedAgainstBudget: without the self-skip,
// rule 3 would add the running self's own GPU usage on top of the fresh
// candidate's declared demand for the same GPU -- double-booking one
// physical process as if it were two, and proposing to evict it to make
// room for itself.
func TestAdmitSelfVRAMNotDoubleCountedAgainstBudget(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", GPUs: map[int]int{0: 9000}, LastUsed: t0},
		},
		Budgets: map[int]int{0: 10000},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}}}
	got := Admit(snap, spec)
	assertDecision(t, "self vram is not double-counted against budget", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitCandidateAloneExceedsBudgetIsNotPermitted covers Important 3:
// spec's own declared demand on a GPU already exceeds that GPU's budget,
// with nothing else running at all. No eviction can ever fix this (there
// is nothing to evict) and waiting cannot help either (nothing here
// depends on current contention), so Admit must return a durable Reason
// rather than hanging the caller in Wait until its admission-wait timeout
// on every single request.
//
// StateNotPermitted is reused rather than adding a new State: the design
// doc already defines it as "a configuration error, visible in the portal,
// not transient", which is exactly what an over-estimated VRAM demand (or
// a budget an operator shrank below it) is. Message carries the
// distinguishing detail so this VRAM-budget cause is not confused with the
// OTHER thing that produces StateNotPermitted (an agent policy refusing
// the spec's binary/directory) when both surface through the same State.
func TestAdmitCandidateAloneExceedsBudgetIsNotPermitted(t *testing.T) {
	snap := PolicySnapshot{
		Budgets: map[int]int{0: 8000},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}}}
	got := Admit(snap, spec)

	if got.OK {
		t.Fatalf("got OK = true, want a terminal Reason: %+v", got)
	}
	if got.Wait {
		t.Fatalf("got Wait = true, want a terminal Reason (this can never resolve by waiting): %+v", got)
	}
	if got.Reason != StateNotPermitted {
		t.Fatalf("Reason = %q, want %q", got.Reason, StateNotPermitted)
	}
	if got.Evict == nil || len(got.Evict) != 0 {
		t.Fatalf("Evict = %v, want a non-nil empty slice", got.Evict)
	}
	if got.Message == "" {
		t.Fatal("Message must be set to distinguish this cause from the binary/directory not_permitted case")
	}
}

// TestAdmitZeroBudgetIsUnconstrained pins the meaning the DATA MODEL
// assigns to a zero budget, which the policy previously contradicted.
// `routing.ServerGPUBudget.BudgetMB` (gateway/backend/internal/routing/
// store.go) defines `BudgetMB 0` as "no budget for this GPU" =
// unconstrained, explicitly "also true for a GPU with no row at all" -- so a
// present 0 and an absent row must produce the IDENTICAL decision, not
// merely similar ones. Admit used to read a present 0 as a real ceiling of
// zero, which made every spec with a known non-zero demand on that GPU
// terminally not_permitted: a launch spec that silently never runs, refused
// with a message about exceeding a 0 MB budget.
//
// A 0 is reachable operator input, not a hypothetical: the portal's
// SetServerGPUBudgets validates `Index < 0 || BudgetMB < 0` and therefore
// accepts 0 deliberately, and the runtime screen seeds a brand-new budget
// row at 0 MB when telemetry offers no total-memory figure (and turns a
// cleared MB input into 0). It was also the ONE place in this feature where
// 0 did not mean unbounded: MaxProcesses 0, idle_timeout_seconds 0,
// admission_wait_timeout_seconds 0, listen_port 0 and a spec GPU's vram_mb
// 0 all read as "no constraint" / "automatic".
//
// The two "still constrains" cases are load-bearing, not padding: they are
// what stops this fix from degenerating into "ignore budgets", which would
// defeat the OOM protection the whole budget feature exists for.
func TestAdmitZeroBudgetIsUnconstrained(t *testing.T) {
	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			// The headline case: a 0 budget row, nothing running at all.
			// Before the fix this was the terminal
			// "demand 9000 MB exceeds budget 0 MB on its own" refusal.
			name: "zero budget alone admits the candidate",
			snap: PolicySnapshot{
				Budgets: map[int]int{0: 0},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// Identical, decision for decision, to the absent-row case in
			// TestAdmit ("missing budget is unconstrained not zero"): a
			// huge occupant on the same GPU is neither summed against nor
			// evicted, because there is no ceiling to overflow.
			name: "zero budget matches an absent row exactly",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 999999}, LastUsed: t0},
				},
				Budgets: map[int]int{0: 0},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}},
			want: Decision{OK: true, Evict: []string{}},
		},
		{
			// A 0 on ONE index must not leak into a sibling index that
			// does carry a real ceiling -- the skip is per GPU, not a
			// whole-snapshot opt-out.
			name: "zero on one gpu leaves a sibling budget enforced",
			snap: PolicySnapshot{
				Budgets: map[int]int{0: 0, 1: 4000},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}, {Index: 1, VRAMMB: 9000}}},
			want: Decision{
				Reason:  StateNotPermitted,
				Message: "spec cand: gpu 1 demand 9000 MB exceeds budget 4000 MB on its own",
				Evict:   []string{},
			},
		},
		{
			// Still constrains, 1: a real ceiling below the candidate's own
			// demand is still the terminal refusal.
			name: "positive budget below own demand still refuses",
			snap: PolicySnapshot{
				Budgets: map[int]int{0: 8000},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}}},
			want: Decision{
				Reason:  StateNotPermitted,
				Message: "spec cand: gpu 0 demand 9000 MB exceeds budget 8000 MB on its own",
				Evict:   []string{},
			},
		},
		{
			// Still constrains, 2: a real ceiling the candidate fits under
			// alone but not alongside the occupant still evicts.
			name: "positive budget still evicts on overflow",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 6000}, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}},
			want: Decision{Evict: []string{"occupant"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, tc.spec), tc.want)
		})
	}
}

// TestSortOldestFirstTieBreaksBySpecID is the Minor fix's direct unit
// test: sortOldestFirst must be a total, deterministic order even when
// LastUsed ties, since Admit feeds it a map-iteration-ordered (i.e.
// per-run randomized) slice.
func TestSortOldestFirstTieBreaksBySpecID(t *testing.T) {
	procs := []RunningProc{
		{SpecID: "zzz", LastUsed: t0},
		{SpecID: "aaa", LastUsed: t0},
		{SpecID: "mmm", LastUsed: t0},
	}
	sortOldestFirst(procs)
	got := []string{procs[0].SpecID, procs[1].SpecID, procs[2].SpecID}
	want := []string{"aaa", "mmm", "zzz"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sortOldestFirst tie-break order = %v, want %v", got, want)
	}
}

// TestAdmitTieBreaksDeterministically exercises the tie-break end-to-end
// through Admit itself (not just the sortOldestFirst unit above): three
// idle candidates share the exact same LastUsed, and MaxProcesses forces
// evicting exactly two of the three. Without the SpecID tie-break, which
// two (and in what order) would depend on Go's map iteration order for
// toEvict, which is randomized per process run -- run with -count=20 to
// confirm this no longer flakes.
func TestAdmitTieBreaksDeterministically(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "zzz", LastUsed: t0},
			{SpecID: "aaa", LastUsed: t0},
			{SpecID: "mmm", LastUsed: t0},
		},
		MaxProcesses: 2,
		Allowed:      allowPairs("cand", "zzz", "aaa", "mmm"),
	}
	got := Admit(snap, Spec{ID: "cand"})
	assertDecision(t, "tie-break is deterministic", got, Decision{Evict: []string{"aaa", "mmm"}})
}

// --- Rule 5: an occupant whose OWN demand is unknown ---------------------
//
// Rule 4 (above) lets a spec with unknown VRAM demand start only ALONE on
// its GPUs. That guarantee used to expire the moment it was granted: both
// unknown-VRAM rules keyed on the CANDIDATE's spec, and rule 3's arithmetic
// reads an occupant's demand as `r.GPUs[g.Index]`, which for an occupant of
// unknown demand is 0 with the key PRESENT (RunningProc.GPUs holds the
// EFFECTIVE figure -- measured if there is one, else the estimate, and
// SpecGPU.VRAMMB's own doc says 0 there means "unknown demand, never a real
// zero-cost claim"). So an unknown-demand occupant was charged 0 MB and
// ignored: the next candidate was admitted onto its card no matter how
// large that occupant really was, and no matter whether it was idle, busy
// or PINNED. Both processes could then OOM -- which is the entire thing the
// VRAM budget exists to prevent.
//
// The rule is therefore symmetric with rule 4: an occupant with unknown
// demand on a GPU the candidate wants blocks sharing exactly as an unknown
// candidate does -- evicted if idle, waited on if busy or still starting,
// terminal pending_vram_unknown if pinned.
//
// Every test below leaves Budgets nil (or generous) and MaxProcesses 0 and
// opens the co-residency pair, so rules 1, 2 and 3 all pass on their own:
// the ONLY thing that can produce a non-OK decision is rule 5, and a
// regression in it cannot hide behind another rule reaching the same answer.

// TestAdmitUnknownOccupantIdleIsEvicted: the occupant's demand on the
// candidate's GPU is unknown and it is idle, so it is an ordinary eviction
// victim -- the candidate gets the card to itself, which is the only
// configuration in which either process's footprint is accounted for.
func TestAdmitUnknownOccupantIdleIsEvicted(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 0, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	got := Admit(snap, spec)
	assertDecision(t, "idle unknown-demand occupant is evicted", got, Decision{Evict: []string{"occupant"}})
}

// TestAdmitUnknownOccupantBusyWaits: a busy occupant can drain on its own,
// so the answer is the transient Wait, never the terminal reason (§5.3:
// pending_vram_unknown as a terminal reason is reserved for a holder that
// can never leave).
func TestAdmitUnknownOccupantBusyWaits(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 1, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	got := Admit(snap, spec)
	assertDecision(t, "busy unknown-demand occupant waits", got, Decision{Wait: true, Evict: []string{}})
}

// TestAdmitUnknownOccupantStartingWaits: a still-loading occupant is not
// evictable either (isEvictable's Starting clause, the C3 fix), so it also
// yields Wait rather than an eviction that would restart the fork-exec
// storm that clause exists to stop.
func TestAdmitUnknownOccupantStartingWaits(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 0, Starting: true, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	got := Admit(snap, spec)
	assertDecision(t, "starting unknown-demand occupant waits", got, Decision{Wait: true, Evict: []string{}})
}

// TestAdmitUnknownOccupantPinnedIsTerminal: a pinned occupant is never
// evicted and never finishes, so neither eviction nor waiting can ever
// resolve this -- the same durable block rule 4 already reports for a
// pinned occupant of an unknown-demand candidate's GPU, and the same
// State the portal already renders.
//
// THE MESSAGE IS THE POINT OF THIS ASSERTION, not decoration. Rule 4's
// terminal sends none because the estimate to fill in belongs to the spec
// already displaying the state; rule 5's actionable field sits on a
// DIFFERENT spec, so without a message an operator sees spec "cand" waiting
// for VRAM with nothing anywhere naming "occupant" as the thing to fix. It
// names the contested card, the occupant, and the occupant's own unknown
// index -- the three facts needed to act.
func TestAdmitUnknownOccupantPinnedIsTerminal(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	got := Admit(snap, spec)
	assertDecision(t, "pinned unknown-demand occupant is terminal", got,
		Decision{
			Reason:  StatePendingVRAMUnknown,
			Message: "spec cand: gpu 0 is held by pinned spec occupant, whose own demand on gpu 0 is unknown",
			Evict:   []string{},
		})
}

// TestAdmitOwnDemandTerminalIsReportedAheadOfPinnedUnknown pins the
// evaluation order between the two terminal causes, which used to be decided
// by which one happened to be checked first.
//
// The candidate declares 9000 MB against an 8000 MB budget -- permanent, no
// eviction and no measurement can make it fit -- while a pinned occupant of
// unknown demand also holds that card. Reporting the unknown-VRAM cause
// invites the operator to fill in a DIFFERENT spec's estimate and achieves
// nothing; reporting the budget cause names the field that is actually wrong.
// Measured on the shipped code before this fix: the 9000-vs-8000 snapshot
// answered pending_vram_unknown with an empty Message.
//
// The second case is the same ordering seen from the candidate side: an
// unknown demand on gpu 1 does not excuse an impossible declared demand on
// gpu 0, so the hoisted check outranks rule 4's short-circuit too.
func TestAdmitOwnDemandTerminalIsReportedAheadOfPinnedUnknown(t *testing.T) {
	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			name: "own demand outranks a pinned unknown occupant",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}}},
			want: Decision{
				Reason:  StateNotPermitted,
				Message: "spec cand: gpu 0 demand 9000 MB exceeds budget 8000 MB on its own",
				Evict:   []string{},
			},
		},
		{
			name: "own demand outranks the candidate's own unknown demand elsewhere",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{1: 5000}, Pinned: true, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000, 1: 20000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 9000}, {Index: 1, VRAMMB: 0}}},
			want: Decision{
				Reason:  StateNotPermitted,
				Message: "spec cand: gpu 0 demand 9000 MB exceeds budget 8000 MB on its own",
				Evict:   []string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, tc.spec), tc.want)
		})
	}
}

// TestAdmitPinnedUnknownOccupantWithClosedPairNamesTheMatrix: when the pinned
// occupant of unknown demand is one the co-residency matrix does not allow
// the candidate to sit beside anyway, the missing VRAM number is NOT the
// blocker -- filling it in leaves rule 1 refusing the pair. §5.3 reserves
// pending_vram_unknown for a block a measurement or an estimate resolves, so
// the reported cause is the closed cell, under StateNotPermitted.
//
// The outcome stays TERMINAL rather than reverting to the pre-rule-5 Wait: a
// pinned occupant can neither be evicted nor drain, so Wait queued every
// request to its admission timeout for a block that never lifts.
//
// The two controls are what stop this from becoming "a closed pair is always
// terminal": a pinned occupant of KNOWN demand behind a closed cell still
// Waits (case 4 of TestAdmit, repeated here with GPUs in play), and an OPEN
// cell still reports the unknown demand it is really blocked on.
func TestAdmitPinnedUnknownOccupantWithClosedPairNamesTheMatrix(t *testing.T) {
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	cases := []struct {
		name string
		snap PolicySnapshot
		want Decision
	}{
		{
			name: "closed pair is reported as the closed pair",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000},
				// Allowed left empty: the (cand, occupant) pair is closed.
			},
			want: Decision{
				Reason:  StateNotPermitted,
				Message: "spec cand: gpu 0 is held by pinned spec occupant, and that pair is not permitted by the co-residency matrix",
				Evict:   []string{},
			},
		},
		{
			name: "control: closed pair with a KNOWN pinned occupant still waits",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 2000}, Pinned: true, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000},
			},
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			name: "control: an open pair still reports the unknown demand",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0},
				},
				Budgets: map[int]int{0: 8000},
				Allowed: allowPairs("cand", "occupant"),
			},
			want: Decision{
				Reason:  StatePendingVRAMUnknown,
				Message: "spec cand: gpu 0 is held by pinned spec occupant, whose own demand on gpu 0 is unknown",
				Evict:   []string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, spec), tc.want)
		})
	}
}

// TestAdmitPinnedUnknownOccupantChoiceIsDeterministic: rule 5's terminal now
// NAMES the occupant it refuses on, so which of several qualifying occupants
// it picks became observable -- and PolicySnapshot.Running arrives in Go map
// order (owner.buildSnapshot ranges over o.specs), which is randomized per
// run. Each case is therefore asserted with Running in BOTH orders: the same
// unchanged host must not report a different Reason or a different spec id
// from one admission to the next.
func TestAdmitPinnedUnknownOccupantChoiceIsDeterministic(t *testing.T) {
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	openPair := RunningProc{SpecID: "aaa-open", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0}
	closedPair := RunningProc{SpecID: "zzz-closed", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t1}

	// A closed cell outranks an open one even though its spec id sorts last:
	// no VRAM number resolves a closed cell, so it is the honest cause.
	closedWins := Decision{
		Reason:  StateNotPermitted,
		Message: "spec cand: gpu 0 is held by pinned spec zzz-closed, and that pair is not permitted by the co-residency matrix",
		Evict:   []string{},
	}
	for _, order := range [][]RunningProc{
		{openPair, closedPair},
		{closedPair, openPair},
	} {
		snap := PolicySnapshot{
			Running: order,
			Budgets: map[int]int{0: 8000},
			Allowed: allowPairs("cand", "aaa-open"),
		}
		assertDecision(t, "closed pair outranks open pair ("+order[0].SpecID+" first)",
			Admit(snap, spec), closedWins)
	}

	// Same precedence on both sides: the lowest spec id, so the message is
	// stable rather than whichever the map happened to yield first.
	otherOpen := RunningProc{SpecID: "bbb-open", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t1}
	lowestID := Decision{
		Reason:  StatePendingVRAMUnknown,
		Message: "spec cand: gpu 0 is held by pinned spec aaa-open, whose own demand on gpu 0 is unknown",
		Evict:   []string{},
	}
	for _, order := range [][]RunningProc{
		{otherOpen, openPair},
		{openPair, otherOpen},
	} {
		snap := PolicySnapshot{
			Running: order,
			Budgets: map[int]int{0: 8000},
			Allowed: allowPairs("cand", "aaa-open", "bbb-open"),
		}
		assertDecision(t, "equal precedence breaks on the lowest spec id ("+order[0].SpecID+" first)",
			Admit(snap, spec), lowestID)
	}
}

// TestAdmitUnknownOccupantBlocksEveryCardItHolds is rule 5's SCOPE, and it is
// what makes it rule 4's mirror rather than a same-named cousin.
//
// Rule 4 admits an unknown-demand spec only if it is alone on ALL of its GPUs
// (specGPUIndexes), not merely on the cards whose demand is unknown. The
// occupant here was admitted under exactly that promise -- {gpu 0: unknown,
// gpu 1: 5000} -- and the candidate wants gpu 1, where the occupant's figure
// happens to be known. Rule 5 keyed on the candidate's indexes found nothing
// unknown at gpu 1 and returned OK with the occupant idle, busy AND pinned
// (measured), silently revoking on gpu 1 the aloneness rule 4 had granted one
// card over. The budget is deliberately far larger than the sum, so rule 3
// cannot reach the same answer by arithmetic and hide a regression here.
//
// The mirrored snapshot is asserted in the same test, because "mirror" is the
// claim under test -- and that claim has since been narrowed, by the operator,
// to the two rules' SUBJECT and never their outcome. Both directions still
// refuse to share the card; WHO PAYS for the refusal is now settled by "known
// demand beats unknown demand", so the mirrored case below waits where it once
// evicted. See TestAdmitUnknownCandidateNeverEvictsKnownOccupant and
// TestAdmitMixedPairConverges for why the outcome-symmetric version could not
// stand: it made every mixed pair evict in both directions.
func TestAdmitUnknownOccupantBlocksEveryCardItHolds(t *testing.T) {
	candidate := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 1, VRAMMB: 5000}}}
	budgets := map[int]int{0: 20000, 1: 20000}

	occupant := func(mod func(*RunningProc)) []RunningProc {
		r := RunningProc{SpecID: "occupant", GPUs: map[int]int{0: 0, 1: 5000}, LastUsed: t0}
		mod(&r)
		return []RunningProc{r}
	}

	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			name: "idle occupant of partly unknown demand is evicted",
			snap: PolicySnapshot{
				Running: occupant(func(r *RunningProc) {}),
				Budgets: budgets,
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: candidate,
			want: Decision{Evict: []string{"occupant"}},
		},
		{
			name: "busy occupant of partly unknown demand waits",
			snap: PolicySnapshot{
				Running: occupant(func(r *RunningProc) { r.InFlight = 1 }),
				Budgets: budgets,
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: candidate,
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			name: "pinned occupant of partly unknown demand is terminal",
			snap: PolicySnapshot{
				Running: occupant(func(r *RunningProc) { r.Pinned = true }),
				Budgets: budgets,
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: candidate,
			want: Decision{
				Reason:  StatePendingVRAMUnknown,
				Message: "spec cand: gpu 1 is held by pinned spec occupant, whose own demand on gpu 0 is unknown",
				Evict:   []string{},
			},
		},
		{
			// CHANGED DELIBERATELY, from `Evict: []string{"occupant"}` to
			// Wait: this is the exact snapshot the two rules disagreed over.
			// The candidate is unknown on gpu 0 and known on gpu 1; the
			// occupant is known and holds gpu 1. Rule 5, run the other way
			// round (the first case in this table), evicts the unknown side --
			// so as long as rule 4 also evicted here, one card with these two
			// specs on it thrashed forever. Rule 4 keeps the aloneness demand
			// (hence Wait rather than OK, and the pinned case above is still
			// terminal) and gives up the eviction, which is the operator's
			// "known demand beats unknown demand".
			name: "mirror: an unknown candidate waits for a known occupant of one of its cards",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{1: 5000}, LastUsed: t0},
				},
				Budgets: budgets,
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}, {Index: 1, VRAMMB: 5000}}},
			// The two cards in the message are different here, which is the
			// whole point of rule 4 being keyed on every card the candidate
			// contests: gpu 1 is the one held, gpu 0 the one with no number.
			want: Decision{
				Wait:    true,
				Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 1 rather than evicting it",
				Evict:   []string{},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, tc.spec), tc.want)
		})
	}
}

// --- Rule 5's placement in the pipeline ----------------------------------
//
// Rule 5's own comment asserts two ordering constraints: it must be collected
// BEFORE rule 3's arithmetic, and it must not move AFTER rule 2's
// process-count limit. Both re-orderings compile, leave every other test in
// this file green, and change the eviction set -- destroying running work
// that the shipped order spares. The two tests below fail for one re-ordering
// each, so the comment is enforced rather than merely believed.

// TestAdmitRule5MustPrecedeRule3: rule 5's victim must be queued before the
// arithmetic sums, because an unknown occupant contributes 0 to that sum and
// releases 0 when evicted -- so the arithmetic cannot make room by evicting
// it and evicts a bystander instead.
//
// "unknown-multi" holds gpu 0 with an unknown demand and gpu 1 at 6000 MB;
// the candidate wants 2000 MB on gpu 1, where the budget is 8000 MB; "known"
// holds 1000 MB on gpu 1 and is the OLDEST, so it is the arithmetic's first
// choice of victim.
//
//	shipped:  rule 5 queues unknown-multi, so rule 3 sums 2000 + 1000 = 3000,
//	          fits, and touches nothing else       -> Evict [unknown-multi]
//	mutant:   rule 3 first sums 2000 + 6000 + 1000 = 9000, over budget, and
//	          evicts the oldest idle toucher to fit -> Evict [known,
//	          unknown-multi]
//
// The mutant drain-stops "known" for nothing: rule 5 removes 6000 MB from
// that card a moment later regardless.
func TestAdmitRule5MustPrecedeRule3(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "known", GPUs: map[int]int{1: 1000}, LastUsed: t0},
			{SpecID: "unknown-multi", GPUs: map[int]int{0: 0, 1: 6000}, LastUsed: t1},
		},
		Budgets: map[int]int{1: 8000},
		Allowed: allowPairs("cand", "known", "unknown-multi"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 1, VRAMMB: 2000}}}
	assertDecision(t, "rule 5 is collected before the arithmetic", Admit(snap, spec),
		Decision{Evict: []string{"unknown-multi"}})
}

// TestAdmitRule5MustPrecedeRule2: the process-count limit runs last precisely
// so it asks only for victims the earlier rules have not already supplied.
// Move rule 5 after it and rule 2 no longer knows that "unknown" is already
// going, so it evicts a second process to make a slot that was about to be
// free anyway.
//
//	shipped:  rule 5 queues unknown; rule 2 sees 2-1 = 1 remaining against
//	          MaxProcesses 2, deficit 0                    -> Evict [unknown]
//	mutant:   rule 2 sees 2 remaining, deficit 1, and takes the oldest idle
//	          candidate; rule 5 then adds its own          -> Evict [known,
//	          unknown]
func TestAdmitRule5MustPrecedeRule2(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "known", GPUs: map[int]int{1: 4000}, LastUsed: t0},
			{SpecID: "unknown", GPUs: map[int]int{0: 0}, LastUsed: t0.Add(2 * time.Minute)},
		},
		MaxProcesses: 2,
		Budgets:      map[int]int{0: 8000, 1: 8000},
		Allowed: map[[2]string]bool{
			PairKey("cand", "known"):    true,
			PairKey("cand", "unknown"):  true,
			PairKey("known", "unknown"): true,
		},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	assertDecision(t, "rule 5 is collected before the process-count limit", Admit(snap, spec),
		Decision{Evict: []string{"unknown"}})
}

// TestAdmitUnknownOccupantOnOtherGPUIsIgnored: rule 5 is per-GPU, exactly
// like rule 3 and unlike rule 1. An occupant whose unknown demand is on a
// card the candidate never touches competes for nothing, so it must not be
// evicted, waited on, or reported -- widening this to "any unknown-demand
// process anywhere" would evict half the host on every start.
func TestAdmitUnknownOccupantOnOtherGPUIsIgnored(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{1: 0}, InFlight: 0, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
		Budgets: map[int]int{0: 8000, 1: 8000},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	got := Admit(snap, spec)
	assertDecision(t, "unknown demand on an unrelated gpu is ignored", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitKnownOccupantUnaffectedByRule5 pins that rule 5 fires ONLY on an
// unknown (0) demand: an occupant with a real, positive figure keeps going
// through rule 3's arithmetic exactly as before -- shared when the sum fits
// (including while PINNED, which rule 5 must not turn into a terminal
// refusal), evicted when it does not.
func TestAdmitKnownOccupantUnaffectedByRule5(t *testing.T) {
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}

	fits := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 2000}, InFlight: 0, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
		Budgets: map[int]int{0: 8000},
	}
	assertDecision(t, "known occupant that fits is shared with", Admit(fits, spec),
		Decision{OK: true, Evict: []string{}})

	pinnedFits := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 2000}, Pinned: true, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
		Budgets: map[int]int{0: 8000},
	}
	assertDecision(t, "pinned known occupant that fits is shared with", Admit(pinnedFits, spec),
		Decision{OK: true, Evict: []string{}})

	overBudget := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 7500}, InFlight: 0, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
		Budgets: map[int]int{0: 8000},
	}
	assertDecision(t, "known occupant over budget is evicted", Admit(overBudget, spec),
		Decision{Evict: []string{"occupant"}})
}

// TestAdmitUnknownOccupantSelfIsNotAnOccupant: the self-filter covers rule 5
// too. A RunningProc sharing the candidate's own SpecID is the very process
// being started (a double-start race, or a snapshot taken a moment too
// early/late), so its unknown demand must never make it its own eviction
// victim -- nor, when pinned, refuse the spec on the grounds that it is
// already there.
func TestAdmitUnknownOccupantSelfIsNotAnOccupant(t *testing.T) {
	idleSelf := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", GPUs: map[int]int{0: 0}, InFlight: 0, LastUsed: t0},
		},
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}
	assertDecision(t, "unknown-demand self is not its own victim", Admit(idleSelf, spec),
		Decision{OK: true, Evict: []string{}})

	pinnedSelf := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", GPUs: map[int]int{0: 0}, Pinned: true, LastUsed: t0},
		},
	}
	assertDecision(t, "unknown-demand pinned self is not a terminal block", Admit(pinnedSelf, spec),
		Decision{OK: true, Evict: []string{}})
}

// TestAdmitUnknownOnBothSidesEvictsOnce: when the CANDIDATE's demand is
// unknown too, the occupant appears exactly ONCE in the Evict list -- an
// Evict list naming the same spec twice would have the caller drain-stop it
// once and then wait forever for a second stop that never comes.
//
// The reason it can only appear once has changed, and the older one is worth
// knowing. Rules 4 and 5 both used to queue this occupant, and the guarantee
// was `toEvict`'s spec-ID key collapsing the two writes. Since "known demand
// beats unknown demand" took rule 4's eviction right away, rule 4 no longer
// queues anyone at all: this victim comes from rule 5 alone, and the
// no-duplicate property is now structural rather than deduplicated. The
// expectation is unchanged either way, which is why this test did not move
// when the rule did.
func TestAdmitUnknownOnBothSidesEvictsOnce(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 0, LastUsed: t0},
		},
		Allowed: allowPairs("cand", "occupant"),
	}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	got := Admit(snap, spec)
	assertDecision(t, "unknown on both sides evicts the occupant once", got, Decision{Evict: []string{"occupant"}})
}

// --- Known demand beats unknown demand -----------------------------------
//
// Rules 4 and 5 were symmetric in the WRONG way: each direction of a mixed
// known/unknown pair evicted the other. Measured on the revision before this
// change -- one card, both processes idle, the pair open, no budget at all:
//
//	candidate a(unknown) vs running b(known)    -> Evict [b]
//	candidate b(known)   vs running a(unknown)  -> Evict [a]
//
// so a host alternating requests between those two specs served every single
// request only after a cold load, forever, with no state in between that
// either request could be served from.
//
// The fix is a TOTAL ORDER -- known demand beats unknown demand -- and it
// costs rule 4 its eviction right. A candidate whose own demand is unknown
// still insists on having its cards to itself (that is exactly what rule 4 is
// for), but it may no longer drain-stop a correctly-configured occupant to get
// there: it BLOCKS instead, which is Wait while the occupant could still
// leave, and the unchanged terminal when it is pinned and never can. Rule 5
// keeps its eviction right untouched -- it is the half of the order that wins
// -- so the spec that loses is always the one whose estimate is missing.
//
// What the order does NOT cover is unknown vs unknown, where neither side
// outranks the other. That tie keeps its pre-existing mutual eviction and is
// recorded as a deliberate acceptance in
// docs/architecture/11-risks-and-technical-debt.md §11.4;
// TestAdmitUnknownTieStillEvictsBothWays pins it so the acceptance is visible
// in the suite instead of only in prose.

// TestAdmitUnknownCandidateNeverEvictsKnownOccupant is rule 4's half of the
// order. Every case leaves the co-residency pair open and the budgets
// generous unless the case name says otherwise, so rule 4 is the only rule
// that can produce the non-OK answer.
func TestAdmitUnknownCandidateNeverEvictsKnownOccupant(t *testing.T) {
	unknownCandidate := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	occupant := func(mod func(*RunningProc)) []RunningProc {
		r := RunningProc{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}
		mod(&r)
		return []RunningProc{r}
	}
	idle := func(*RunningProc) {}
	// Every Wait below is rule 4's, so every one of them carries rule 4's
	// message. What it says is pinned by
	// TestAdmitPrecedenceWaitNamesWhatItIsWaitingFor; here it is the marker
	// that this Wait came from the precedence and not from another rule
	// reaching the same shape.
	const waits = "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it"

	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			// THE CHANGED CASE. Before: Evict [occupant] -- an unknown-demand
			// spec drain-stopped a spec with a perfectly good estimate to get
			// the card to itself. Now: Wait, which the occupant's own idle
			// timeout resolves without destroying anything.
			name: "idle known occupant is waited for, never evicted",
			snap: PolicySnapshot{Running: occupant(idle), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Message: waits, Evict: []string{}},
		},
		{
			// Unchanged: a busy occupant was never evictable anyway.
			name: "busy known occupant still waits",
			snap: PolicySnapshot{Running: occupant(func(r *RunningProc) { r.InFlight = 1 }), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Message: waits, Evict: []string{}},
		},
		{
			// Unchanged: isEvictable's Starting clause (the C3 fix) already
			// spared a still-loading occupant.
			name: "still-loading known occupant still waits",
			snap: PolicySnapshot{Running: occupant(func(r *RunningProc) { r.Starting = true }), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Message: waits, Evict: []string{}},
		},
		{
			// Unchanged, and it is what makes Wait above the right SHAPE
			// rather than a new terminal: an occupant that can never leave is
			// still reported as the terminal it always was, by step 2's
			// short-circuit, ahead of rule 4's collection loop.
			name: "pinned known occupant is still the terminal",
			snap: PolicySnapshot{Running: occupant(func(r *RunningProc) { r.Pinned = true }), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Reason: StatePendingVRAMUnknown, Evict: []string{}},
		},
		{
			// Before: Evict [occupant], because rule 1 queued the occupant for
			// the closed cell and rule 4 was content. The order is a property
			// of the two SPECS, not of rule 4's own loop: a closed matrix cell
			// does not hand the unknown-demand candidate back the right to
			// drain-stop the known one, and blocking here converges the closed
			// mixed pair too (which evicted in both directions before).
			name: "a closed co-residency cell does not restore the eviction right",
			snap: PolicySnapshot{Running: occupant(idle), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Message: waits, Evict: []string{}},
		},
		{
			// Before: Evict [occupant] -- here from rule 3's arithmetic on the
			// candidate's OTHER, known card. Same answer as the closed cell
			// above and for the same reason: whatever independently justifies
			// the eviction, the victim is a known-demand occupant of a card
			// this candidate contests, and this candidate may not have it.
			name: "a sibling gpu over budget does not restore it either",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{1: 5000}, LastUsed: t0}},
				Budgets: map[int]int{1: 8000},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}, {Index: 1, VRAMMB: 5000}}},
			// The contested card is the sibling here, so the message names
			// gpu 1 as the one held and gpu 0 as the one with no number.
			want: Decision{
				Wait:    true,
				Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 1 rather than evicting it",
				Evict:   []string{},
			},
		},
		{
			// The order is about the cards the candidate CONTESTS. A
			// known-demand occupant of a card this candidate never touches is
			// outside rule 4 entirely and keeps being evicted by the rules
			// that own it -- here the process-count limit. Blocking it too
			// would make an unconfigured spec unable to start on an idle host.
			name: "a known occupant of an untouched gpu is still evicted by the process limit",
			snap: PolicySnapshot{
				Running:      []RunningProc{{SpecID: "elsewhere", GPUs: map[int]int{7: 5000}, LastUsed: t0}},
				MaxProcesses: 1,
				Allowed:      allowPairs("cand", "elsewhere"),
			},
			spec: unknownCandidate,
			want: Decision{Evict: []string{"elsewhere"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, tc.spec), tc.want)
		})
	}
}

// TestAdmitPrecedenceWaitNamesWhatItIsWaitingFor is the diagnostic half of
// ADR-032. The order costs the losing spec a `Wait` while an idle victim was
// plainly available, and that wait is only as short as the occupant's own
// `idle_timeout_seconds` -- which is `0` (never unload) by default, so for an
// idle, unpinned, never-again-requested occupant the candidate can queue to
// its admission timeout on every attempt, forever. A spec that will not start,
// with nothing anywhere saying why, is not an acceptable outcome, so THIS Wait
// carries a message: which spec holds which card, and that this candidate's
// own demand is the missing number.
//
// SCOPED TO THE PRECEDENCE WAIT, which the controls at the end pin. Every
// other Wait is ordinary operation -- a busy neighbour finishes, a queued
// victim drains -- and must keep saying nothing, or the portal's error column
// fills with noise on every contested start.
func TestAdmitPrecedenceWaitNamesWhatItIsWaitingFor(t *testing.T) {
	unknownCandidate := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}

	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			// The wait nothing resolves on its own: idle, unpinned, and (in
			// the manager) an idle timeout of 0.
			name: "the idle known occupant it may not evict is named, with its card",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}},
				Allowed: allowPairs("cand", "occupant"),
				Budgets: map[int]int{0: 20000},
			},
			spec: unknownCandidate,
			want: Decision{
				Wait:    true,
				Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it",
				Evict:   []string{},
			},
		},
		{
			// The candidate's unknown card and the contested card need not be
			// the same one -- rule 4 demands aloneness on ALL of its cards --
			// so the message names both.
			name: "the contested card and the unknown card are named separately",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{1: 5000}, LastUsed: t0}},
				Allowed: allowPairs("cand", "occupant"),
				Budgets: map[int]int{1: 20000},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}, {Index: 1, VRAMMB: 5000}}},
			want: Decision{
				Wait:    true,
				Message: "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 1 rather than evicting it",
				Evict:   []string{},
			},
		},
		{
			// CONTROL: a matrix block against a busy neighbour. An ordinary
			// Wait, resolved by that neighbour finishing, and it must stay
			// silent.
			name: "control: a matrix-blocked busy neighbour says nothing",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "blocker", InFlight: 1, LastUsed: t0}},
			},
			spec: Spec{ID: "cand"},
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// CONTROL: rule 5's own Wait. The candidate here has a real
			// number and the OCCUPANT is the unknown one, so this is the
			// winning side of the order merely waiting for a busy process to
			// go idle -- at which point rule 5 evicts it. Transient by
			// construction, so no message.
			name: "control: rule 5 waiting for a busy unknown occupant says nothing",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 1, LastUsed: t0}},
				Allowed: allowPairs("cand", "occupant"),
				Budgets: map[int]int{0: 20000},
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}},
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// CONTROL: the tie the order does not decide. Both sides unknown,
			// so nothing outranks anything, rule 5 owns the occupant, and the
			// pre-existing mutual eviction stands -- there is no precedence
			// block here to report.
			name: "control: the both-unknown tie is not a precedence block",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 0}, InFlight: 1, LastUsed: t0}},
				Allowed: allowPairs("cand", "occupant"),
				Budgets: map[int]int{0: 20000},
			},
			spec: unknownCandidate,
			want: Decision{Wait: true, Evict: []string{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertDecision(t, tc.name, Admit(tc.snap, tc.spec), tc.want)
		})
	}
}

// TestAdmitPrecedenceWaitMessageIsDeterministic: PolicySnapshot.Running
// arrives in Go MAP order (owner.buildSnapshot ranges over o.specs), so
// "whichever blocker the loop saw first" would name a different spec on every
// admission against an unchanged host -- and this text goes into an
// operator-facing `last_error` that the portal renders in a column. The
// tie-break is the lowest spec id, the same total order the victim ordering
// and pinnedUnknownOccupantRefusal already use.
func TestAdmitPrecedenceWaitMessageIsDeterministic(t *testing.T) {
	a := RunningProc{SpecID: "occupant-a", GPUs: map[int]int{0: 4000}, LastUsed: t1}
	b := RunningProc{SpecID: "occupant-b", GPUs: map[int]int{0: 4000}, LastUsed: t0}
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	allowed := allowPairs("cand", "occupant-a", "occupant-b")

	const want = "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant-a to leave gpu 0 rather than evicting it"
	for _, order := range [][]RunningProc{{a, b}, {b, a}} {
		got := Admit(PolicySnapshot{Running: order, Allowed: allowed, Budgets: map[int]int{0: 20000}}, spec)
		if got.Message != want {
			t.Errorf("Running order %v -> Message = %q, want %q", []string{order[0].SpecID, order[1].SpecID}, got.Message, want)
		}
	}
}

// TestAdmitMixedPairConverges is the defect itself, asserted as a property of
// the PAIR rather than of one decision: run Admit in both directions over one
// contested card and at most one of them may evict. Before the change both
// did, which is a host that never reaches a served state it can stay in; the
// two per-direction expectations are asserted as well so a regression says
// WHICH direction moved, and the final check says what that costs.
func TestAdmitMixedPairConverges(t *testing.T) {
	allowed := map[[2]string]bool{PairKey("known", "unknown"): true}
	specKnown := Spec{ID: "known", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}}
	specUnknown := Spec{ID: "unknown", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	runKnown := RunningProc{SpecID: "known", GPUs: map[int]int{0: 5000}, LastUsed: t0}
	runUnknown := RunningProc{SpecID: "unknown", GPUs: map[int]int{0: 0}, LastUsed: t0}

	knownAsCandidate := Admit(PolicySnapshot{Running: []RunningProc{runUnknown}, Allowed: allowed}, specKnown)
	unknownAsCandidate := Admit(PolicySnapshot{Running: []RunningProc{runKnown}, Allowed: allowed}, specUnknown)

	assertDecision(t, "rule 5: known demand evicts the unknown occupant", knownAsCandidate,
		Decision{Evict: []string{"unknown"}})
	assertDecision(t, "rule 4: unknown demand waits for the known occupant", unknownAsCandidate,
		Decision{
			Wait:    true,
			Message: "spec unknown: its own demand on gpu 0 is unknown, so it waits for spec known to leave gpu 0 rather than evicting it",
			Evict:   []string{},
		})

	if len(knownAsCandidate.Evict) > 0 && len(unknownAsCandidate.Evict) > 0 {
		t.Fatalf("both directions of one mixed pair evict, so alternating requests thrash forever: "+
			"known-as-candidate evicts %v, unknown-as-candidate evicts %v",
			knownAsCandidate.Evict, unknownAsCandidate.Evict)
	}
}

// TestAdmitUnknownTieStillEvictsBothWays pins the case the operator's order
// does NOT decide: two specs that are both of unknown demand do not outrank
// each other, so each still evicts the other and the pair still thrashes.
//
// THIS TEST IS NOT AN ENDORSEMENT. It records an accepted remaining case
// (docs/architecture/11-risks-and-technical-debt.md §11.4) at the one place a
// reader of Admit will look for it, so that (a) the change above is visibly
// scoped to the MIXED pair and did not quietly alter the tie, and (b) whoever
// does resolve the tie -- with a tiebreak this branch deliberately did not
// invent -- changes this expectation on purpose rather than discovering it.
func TestAdmitUnknownTieStillEvictsBothWays(t *testing.T) {
	allowed := map[[2]string]bool{PairKey("a", "b"): true}
	specA := Spec{ID: "a", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	specB := Spec{ID: "b", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	runA := RunningProc{SpecID: "a", GPUs: map[int]int{0: 0}, LastUsed: t0}
	runB := RunningProc{SpecID: "b", GPUs: map[int]int{0: 0}, LastUsed: t0}

	assertDecision(t, "unknown a evicts unknown occupant b",
		Admit(PolicySnapshot{Running: []RunningProc{runB}, Allowed: allowed}, specA),
		Decision{Evict: []string{"b"}})
	assertDecision(t, "unknown b evicts unknown occupant a",
		Admit(PolicySnapshot{Running: []RunningProc{runA}, Allowed: allowed}, specB),
		Decision{Evict: []string{"a"}})
}

// --- VRAM bootstrap: the one-shot guarded eviction (ADR-032) --------------
//
// ADR-032 is a TOTAL ORDER: known VRAM demand beats unknown, so rule 4 (the
// candidate's own demand is unknown) BLOCKS rather than evicting a
// known-demand occupant -- which is what stops a mixed pair cold-loading each
// other forever. That order is correct in steady state but leaves one gap: a
// spec that has NEVER been measured and whose operator left its estimate at 0
// can never get a card to itself while any known-demand occupant sits on it,
// so it can never capture the first measurement that would move it out of the
// unknown class at all.
//
// Task 4.3 closes that gap with ONE guarded eviction. The owner grants it
// through PolicySnapshot.VRAMBootstrapEligible, set only while the candidate
// has no measurement and has not yet spent the shot (see
// specState.vramBootstrapDone). While the grant is live, rule 4 evicts idle
// known-demand touchers to free the card -- exactly as rule 5 already treats
// unknown occupants -- and flags Decision.VRAMBootstrap so the owner burns the
// one-shot. The moment the shot is burned (or any measurement lands) the grant
// is withdrawn and rule 4 is back to the pure ADR-032 block, so the exception
// can fire at most once per spec and can never reintroduce the thrash.

// TestVRAMBootstrapOneShotEvictsToMeasure: with the grant live, a never-measured
// unknown-VRAM candidate evicts the idle known-demand occupant of its card so it
// can start alone and be measured -- the one case rule 4 otherwise only Waits on.
func TestVRAMBootstrapOneShotEvictsToMeasure(t *testing.T) {
	unknownCandidate := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}

	t.Run("one idle known occupant is evicted and the shot is flagged", func(t *testing.T) {
		snap := PolicySnapshot{
			Running:               []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}},
			Budgets:               map[int]int{0: 20000},
			Allowed:               allowPairs("cand", "occupant"),
			VRAMBootstrapEligible: true,
		}
		got := Admit(snap, unknownCandidate)
		assertDecision(t, "the never-measured candidate evicts the known occupant once", got, Decision{Evict: []string{"occupant"}})
		if !got.VRAMBootstrap {
			t.Error("the bootstrap eviction must set Decision.VRAMBootstrap so the owner can burn the one-shot")
		}
	})

	t.Run("every idle known toucher is freed so the candidate truly runs alone", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "new", GPUs: map[int]int{0: 4000}, LastUsed: t2},
				{SpecID: "old", GPUs: map[int]int{0: 4000}, LastUsed: t0},
			},
			Budgets:               map[int]int{0: 20000},
			Allowed:               allowPairs("cand", "new", "old"),
			VRAMBootstrapEligible: true,
		}
		// Aloneness is on ALL of the card's occupants, so both idle touchers go,
		// oldest LastUsed first -- the eviction order the other rules use.
		got := Admit(snap, unknownCandidate)
		assertDecision(t, "both idle touchers are evicted oldest-first", got, Decision{Evict: []string{"old", "new"}})
		if !got.VRAMBootstrap {
			t.Error("VRAMBootstrap must be set when the bootstrap evicts")
		}
	})

	t.Run("a busy known occupant is still waited for, never a partial start", func(t *testing.T) {
		// The grant is to run ALONE; a toucher that cannot be evicted (busy or
		// still loading) means the card cannot be freed, so the candidate Waits --
		// an ordinary Wait that the neighbour's own completion resolves, carrying
		// no message and NOT the ADR-032 precedence wait. The shot is not spent.
		for _, tc := range []struct {
			name string
			mod  func(*RunningProc)
		}{
			{"busy", func(r *RunningProc) { r.InFlight = 1 }},
			{"still loading", func(r *RunningProc) { r.Starting = true }},
		} {
			t.Run(tc.name, func(t *testing.T) {
				r := RunningProc{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}
				tc.mod(&r)
				snap := PolicySnapshot{
					Running:               []RunningProc{r},
					Budgets:               map[int]int{0: 20000},
					Allowed:               allowPairs("cand", "occupant"),
					VRAMBootstrapEligible: true,
				}
				got := Admit(snap, unknownCandidate)
				assertDecision(t, "a non-evictable toucher blocks the bootstrap", got, Decision{Wait: true, Evict: []string{}})
				if got.VRAMBootstrap {
					t.Error("a blocked bootstrap must not flag VRAMBootstrap: no eviction was issued, so the shot is not spent")
				}
			})
		}
	})
}

// TestVRAMBootstrapDoesNotRepeat: the one-shot cannot thrash. The identical
// contested snapshot that evicts while the grant is live reverts to the ADR-032
// Wait -- known demand beats unknown, no eviction -- the instant the grant is
// withdrawn, which the owner does the moment the shot is burned or a measurement
// lands. So rule 4 can evict for bootstrap at most once per spec, and after that
// a host alternating requests between the two specs cannot cold-load either one
// to serve the other.
func TestVRAMBootstrapDoesNotRepeat(t *testing.T) {
	spec := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	base := PolicySnapshot{
		Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}},
		Budgets: map[int]int{0: 20000},
		Allowed: allowPairs("cand", "occupant"),
	}
	const waits = "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it"

	// Grant live: the one-shot evicts the idle known occupant.
	eligible := base
	eligible.VRAMBootstrapEligible = true
	first := Admit(eligible, spec)
	assertDecision(t, "the one shot evicts the known occupant", first, Decision{Evict: []string{"occupant"}})
	if !first.VRAMBootstrap {
		t.Fatal("the one-shot eviction must flag VRAMBootstrap")
	}

	// Grant withdrawn (shot spent, or a measurement captured): the identical
	// snapshot is back to the ADR-032 precedence Wait, never a second eviction.
	spent := base // VRAMBootstrapEligible defaults to false
	second := Admit(spent, spec)
	assertDecision(t, "after the shot the same host reverts to the ADR-032 wait", second, Decision{Wait: true, Message: waits, Evict: []string{}})
	if second.VRAMBootstrap {
		t.Error("a steady-state ADR-032 wait must never flag VRAMBootstrap")
	}
}

// TestADR032SteadyStatePreserved: with no grant (VRAMBootstrapEligible false --
// the default, and what the owner gives every spec that already has a
// measurement or has already spent its shot) the total order is untouched. An
// unknown-demand candidate waits for a known occupant rather than evicting it,
// and -- crucially -- the grant never leaks onto the winning side: a
// known-demand candidate still evicts an unknown occupant (rule 5), with or
// without the flag set, and never carries VRAMBootstrap.
func TestADR032SteadyStatePreserved(t *testing.T) {
	const waits = "spec cand: its own demand on gpu 0 is unknown, so it waits for spec occupant to leave gpu 0 rather than evicting it"

	t.Run("unknown candidate still waits for an idle known occupant", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0}},
			Budgets: map[int]int{0: 20000},
			Allowed: allowPairs("cand", "occupant"),
			// VRAMBootstrapEligible defaults to false: no grant.
		}
		got := Admit(snap, Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}})
		assertDecision(t, "known beats unknown: wait, never evict", got, Decision{Wait: true, Message: waits, Evict: []string{}})
		if got.VRAMBootstrap {
			t.Error("a steady-state wait must not flag VRAMBootstrap")
		}
	})

	t.Run("a known candidate still evicts an idle unknown occupant even with the grant set", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{{SpecID: "occupant", GPUs: map[int]int{0: 0}, LastUsed: t0}},
			Budgets: map[int]int{0: 20000},
			Allowed: allowPairs("cand", "occupant"),
			// The grant is meaningless for a KNOWN candidate (rule 4 never runs),
			// but set it to prove the bootstrap never touches rule 5's eviction.
			VRAMBootstrapEligible: true,
		}
		got := Admit(snap, Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}})
		assertDecision(t, "rule 5 still evicts the unknown occupant", got, Decision{Evict: []string{"occupant"}})
		if got.VRAMBootstrap {
			t.Error("rule 5's eviction is not a bootstrap and must not flag VRAMBootstrap")
		}
	})
}

// --- Rule 6: the host-wide RAM pool --------------------------------------
//
// RAM is a host resource, not a per-card one, so unlike rule 3 (one sum per
// GPU, victims drawn only from that GPU's occupants) rule 6 keeps ONE sum over
// every running process and draws victims from ALL of them. The pool is
// PolicySnapshot.HostRAMBudgetMB -- the capacity of the managed set, a total
// like a per-GPU budget that the running processes' RAM is summed INTO -- and 0
// is unconstrained; a candidate's RAMMB of 0 is an UNKNOWN demand that this rule
// does not gate (cold-start bootstrap owns it), and a known demand above the
// whole pool is refused terminally before any of the arithmetic.
//
// Every test opens the co-residency pair, leaves Budgets nil and MaxProcesses 0,
// so rules 1, 2 and 3 all pass on their own and the ONLY thing that can produce
// a non-OK decision is rule 6 -- except where a test is explicitly about how
// rule 6 composes with one of them.

// TestAdmitRAMFitsWithinHostBudget: the candidate plus everything running fits
// the pool, so nothing is evicted and the start proceeds.
func TestAdmitRAMFitsWithinHostBudget(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "resident", RAMMB: 10000, LastUsed: t0},
		},
		HostRAMBudgetMB: 30000,
		Allowed:         allowPairs("cand", "resident"),
	}
	got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
	assertDecision(t, "10000 running + 8000 candidate fits 30000", got, Decision{OK: true, Evict: []string{}})

	// The boundary is inclusive: summing to exactly the pool still fits.
	snap.HostRAMBudgetMB = 18000
	got = Admit(snap, Spec{ID: "cand", RAMMB: 8000})
	assertDecision(t, "a sum equal to the pool fits", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitRAMEvictsIdleOldestHostWide: the sum overflows the pool, so idle
// processes are drained oldest-LastUsed first until it fits. The occupants here
// sit on GPU 1 while the candidate wants GPU 0 (with a generous VRAM budget), so
// rule 3 has nothing to say: only a HOST-wide pool can name them as victims.
func TestAdmitRAMEvictsIdleOldestHostWide(t *testing.T) {
	running := func() []RunningProc {
		return []RunningProc{
			// Listed newest-first on purpose: the order victims are drawn in
			// must come from LastUsed, never from slice order.
			{SpecID: "new", GPUs: map[int]int{1: 1000}, RAMMB: 4000, LastUsed: t1},
			{SpecID: "old", GPUs: map[int]int{1: 1000}, RAMMB: 10000, LastUsed: t0},
		}
	}
	cand := Spec{ID: "cand", RAMMB: 8000, GPUs: []SpecGPU{{Index: 0, VRAMMB: 1000}}}

	t.Run("the oldest idle process is drained and that is enough", func(t *testing.T) {
		// 8000 + 10000 + 4000 = 22000 > 16000; draining "old" leaves 12000.
		snap := PolicySnapshot{
			Running:         running(),
			Budgets:         map[int]int{0: 24000, 1: 24000},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "old", "new"),
		}
		assertDecision(t, "oldest drained first", Admit(snap, cand), Decision{Evict: []string{"old"}})
	})

	t.Run("victims keep being drawn, oldest first, until it fits", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "c", RAMMB: 6000, LastUsed: t2},
				{SpecID: "a", RAMMB: 6000, LastUsed: t0},
				{SpecID: "b", RAMMB: 6000, LastUsed: t1},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "a", "b", "c"),
		}
		// 8000 + 18000 = 26000; drain a -> 20000 (still over); drain b -> 14000.
		assertDecision(t, "two victims", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"a", "b"}})
	})

	t.Run("oldest first, not the best fit", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "small-old", RAMMB: 1000, LastUsed: t0},
				{SpecID: "big-new", RAMMB: 12000, LastUsed: t1},
			},
			HostRAMBudgetMB: 14000,
			Allowed:         allowPairs("cand", "small-old", "big-new"),
		}
		// 8000 + 13000 = 21000 > 14000: draining the oldest alone (-> 20000) is
		// not enough, so the newer one goes too, in LastUsed order.
		assertDecision(t, "order is LastUsed, not size", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"small-old", "big-new"}})
	})
}

// TestAdmitRAMWaitsWhenAllBusy: the same overflow as above, but nothing that
// could free the RAM may be evicted. That is a Wait with an EMPTY evict list --
// never a partial one, since draining only some of the processes would not
// unblock a retry and would cost a cold load for nothing.
func TestAdmitRAMWaitsWhenAllBusy(t *testing.T) {
	cases := []struct {
		name    string
		running []RunningProc
	}{
		{
			name: "every holder is busy",
			running: []RunningProc{
				{SpecID: "old", RAMMB: 10000, InFlight: 1, LastUsed: t0},
				{SpecID: "new", RAMMB: 4000, InFlight: 2, LastUsed: t1},
			},
		},
		{
			name: "the only idle holder is pinned",
			running: []RunningProc{
				{SpecID: "old", RAMMB: 10000, Pinned: true, LastUsed: t0},
				{SpecID: "new", RAMMB: 4000, InFlight: 1, LastUsed: t1},
			},
		},
		{
			// 8000 + 10000 + 4000 = 22000 > 16000. Draining the idle 4000 would
			// leave 18000, still over, because the 10000 is busy -- so no victim
			// is named at all.
			name: "the idle holder alone cannot make it fit",
			running: []RunningProc{
				{SpecID: "busy", RAMMB: 10000, InFlight: 1, LastUsed: t0},
				{SpecID: "idle", RAMMB: 4000, LastUsed: t1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := PolicySnapshot{
				Running:         tc.running,
				HostRAMBudgetMB: 16000,
				Allowed:         allowPairs("cand", "old", "new", "busy", "idle"),
			}
			assertDecision(t, tc.name, Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
		})
	}
}

// TestAdmitRAMUnconstrainedWhenBudgetZero: HostRAMBudgetMB 0 is the same
// UNCONSTRAINED sentinel a per-GPU budget uses (the host has not produced a
// MemAvailable reading yet), and a negative value is no ceiling either. Admit is
// the one place that interprets it, so both skip the rule outright: an enormous
// occupant is neither summed against nor evicted.
func TestAdmitRAMUnconstrainedWhenBudgetZero(t *testing.T) {
	for name, budget := range map[string]int{"zero": 0, "negative": -1} {
		t.Run(name, func(t *testing.T) {
			snap := PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "huge", RAMMB: 999999, LastUsed: t0},
				},
				HostRAMBudgetMB: budget,
				Allowed:         allowPairs("cand", "huge"),
			}
			got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
			assertDecision(t, name+" budget is unconstrained", got, Decision{OK: true, Evict: []string{}})
		})
	}
}

// TestAdmitRAMUnknownDemandNotGatedWithoutAGuess: a candidate whose own RAM demand
// is 0 is UNKNOWN, never "needs none". Routing an unknown demand to a first
// measurement is the cold-start bootstrap's job, and when no bootstrap guess is
// configured (RAMBootstrapGuessMB 0, the knob off) rule 6 charges it nothing, so it
// starts exactly as it did before RAM was an admission axis, however tight the pool
// looks, and it evicts nobody. (With a guess configured it is charged that guess
// instead -- see TestRAMBootstrapStartsWithoutEvictionWhenHeadroom and friends.)
func TestAdmitRAMUnknownDemandNotGatedWithoutAGuess(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "resident", RAMMB: 10000, LastUsed: t0},
		},
		HostRAMBudgetMB: 4000, // already overflowed by the resident alone
		Allowed:         allowPairs("cand", "resident"),
	}
	got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
	assertDecision(t, "unknown candidate demand is not gated without a guess", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitRAMUnknownOccupantContributesNothingAndIsNotAVictim: the occupant
// side of the same sentinel. A running process with RAMMB 0 has no known
// occupancy, so it adds 0 to the sum -- and, because draining it frees nothing
// the arithmetic can count, it is never named as a victim for RAM, even when it
// is the oldest idle process on the host.
func TestAdmitRAMUnknownOccupantContributesNothingAndIsNotAVictim(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "unknown", RAMMB: 0, LastUsed: t0},
			{SpecID: "big", RAMMB: 10000, LastUsed: t1},
		},
		HostRAMBudgetMB: 16000,
		Allowed:         allowPairs("cand", "unknown", "big"),
	}
	// 8000 + 10000 = 18000 > 16000; only "big" can help.
	got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
	assertDecision(t, "only the holder that frees RAM is drained", got, Decision{Evict: []string{"big"}})

	// And with nothing but the unknown occupant running there is nothing to
	// charge and nothing to drain: it fits.
	snap.Running = snap.Running[:1]
	got = Admit(snap, Spec{ID: "cand", RAMMB: 8000})
	assertDecision(t, "an unknown occupant alone charges nothing", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitStartingProcReservesRAM is the known-demand interlock. A process
// that is still loading is already in Running with its RAMMB, so it counts
// toward the pool exactly like a settled one -- and it is never an eviction
// victim, so a second admission cannot reclaim, or double-commit, the headroom
// the first one is in the middle of filling.
func TestAdmitStartingProcReservesRAM(t *testing.T) {
	t.Run("a starting process counts toward the sum", func(t *testing.T) {
		// 10000 + 8000 = 18000 fits 20000, but only 2000 is left once the
		// loading process is counted -- a second 8000 candidate does not.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "loading-a", RAMMB: 10000, Starting: true, LastUsed: t0},
				{SpecID: "loading-b", RAMMB: 8000, Starting: true, LastUsed: t1},
			},
			HostRAMBudgetMB: 20000,
			Allowed:         allowPairs("cand", "loading-a", "loading-b"),
		}
		got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "second admission sees the reservation", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("a starting process is never the victim", func(t *testing.T) {
		starting := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "loading", RAMMB: 10000, Starting: true, LastUsed: t0},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "loading"),
		}
		assertDecision(t, "starting holder waits", Admit(starting, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})

		// The identical snapshot with the process settled is evicted, which is
		// what proves it is Starting -- not the numbers -- that spared it.
		settled := starting
		settled.Running = []RunningProc{{SpecID: "loading", RAMMB: 10000, LastUsed: t0}}
		assertDecision(t, "settled holder is drained", Admit(settled, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"loading"}})
	})

	t.Run("a starting process still leaves room when there is room", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "loading", RAMMB: 10000, Starting: true, LastUsed: t0},
			},
			HostRAMBudgetMB: 20000,
			Allowed:         allowPairs("cand", "loading"),
		}
		assertDecision(t, "fits beside a loading process", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{OK: true, Evict: []string{}})
	})
}

// TestBootstrapConditionDetected pins the cold-start bootstrap predicates: the
// pure RAM one (specHasUnknownRAM), the RAM-and-no-measurement combination that
// routes a FIRST start to the bootstrap (needsRAMBootstrap), and the VRAM one it
// is modelled on (specHasUnknownVRAM, reused unchanged).
func TestBootstrapConditionDetected(t *testing.T) {
	t.Run("specHasUnknownRAM is exactly RAMMB == 0", func(t *testing.T) {
		if !specHasUnknownRAM(Spec{ID: "a", RAMMB: 0}) {
			t.Error("RAMMB 0 is UNKNOWN demand and must read as unknown")
		}
		if specHasUnknownRAM(Spec{ID: "a", RAMMB: 1}) {
			t.Error("a positive RAM demand is known, never unknown")
		}
		if specHasUnknownRAM(Spec{ID: "a", RAMMB: 8000}) {
			t.Error("a positive RAM demand is known, never unknown")
		}
	})

	t.Run("needsRAMBootstrap requires unknown demand AND no real measurement", func(t *testing.T) {
		cases := []struct {
			name     string
			spec     Spec
			measured int
			want     bool
		}{
			{"unknown demand, never measured", Spec{ID: "a", RAMMB: 0}, 0, true},
			{"unknown demand, a non-measurement (0) is still no measurement", Spec{ID: "a", RAMMB: 0}, 0, true},
			{"unknown demand, a non-measurement (negative) is still no measurement", Spec{ID: "a", RAMMB: 0}, -5, true},
			{"unknown demand, already measured: not a bootstrap", Spec{ID: "a", RAMMB: 0}, 4000, false},
			{"known demand, never measured: not a bootstrap", Spec{ID: "a", RAMMB: 8000}, 0, false},
			{"known demand, measured: not a bootstrap", Spec{ID: "a", RAMMB: 8000}, 4000, false},
		}
		for _, tc := range cases {
			if got := needsRAMBootstrap(tc.spec, tc.measured); got != tc.want {
				t.Errorf("%s: needsRAMBootstrap(RAMMB=%d, measured=%d) = %v, want %v", tc.name, tc.spec.RAMMB, tc.measured, got, tc.want)
			}
		}
	})

	t.Run("specHasUnknownVRAM, the axis this mirrors, is unchanged", func(t *testing.T) {
		if !specHasUnknownVRAM(Spec{ID: "a", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}) {
			t.Error("a VRAMMB 0 GPU is unknown demand")
		}
		if specHasUnknownVRAM(Spec{ID: "a", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}}) {
			t.Error("a fully-declared GPU is not unknown")
		}
		if !specHasUnknownVRAM(Spec{ID: "a", GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}, {Index: 1, VRAMMB: 0}}}) {
			t.Error("any unknown GPU makes the spec unknown-VRAM")
		}
		if specHasUnknownVRAM(Spec{ID: "a"}) {
			t.Error("a spec that declares no GPU has no unknown VRAM demand")
		}
	})
}

// TestRAMBootstrapBlocksConcurrentRAMAdmission is the interlock (task 4.1). While
// one spec's RAM cold-start bootstrap is in its unaccounted window
// (PolicySnapshot.RAMBootstrapInFlight, which buildSnapshot sets while a
// bootstrapping process is Starting+unmeasured), its TRUE RAM cost is unknown, so
// every OTHER RAM admission must Wait -- a second spec taking the apparently-free
// RAM mid-window is exactly how the bootstrap causes the OOM it exists to
// prevent. The Wait carries WaitOnRAM so the next hardware sample re-asks it, by
// which time the bootstrap has settled or left.
func TestRAMBootstrapBlocksConcurrentRAMAdmission(t *testing.T) {
	// A roomy pool with one small resident: absent the interlock every candidate
	// below fits trivially, so a Wait can only be the interlock talking.
	base := func() PolicySnapshot {
		return PolicySnapshot{
			Running:              []RunningProc{{SpecID: "resident", RAMMB: 2000, LastUsed: t0}},
			HostRAMBudgetMB:      40000,
			RAMBootstrapInFlight: true,
			Allowed:              allowPairs("cand", "resident"),
		}
	}

	t.Run("a known-demand candidate that would fit still waits", func(t *testing.T) {
		got := Admit(base(), Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "known demand waits behind the bootstrap", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("a second unknown-demand candidate waits too (bootstraps serialize)", func(t *testing.T) {
		got := Admit(base(), Spec{ID: "cand", RAMMB: 0})
		assertDecision(t, "a second bootstrap waits", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("the interlock never evicts -- it holds the host still", func(t *testing.T) {
		snap := base()
		// An idle neighbour the arithmetic could have drained: the interlock must
		// not turn into an eviction, it must Wait with an empty list.
		snap.Running = append(snap.Running, RunningProc{SpecID: "idle", RAMMB: 3000, LastUsed: t1})
		snap.Allowed = allowPairs("cand", "resident", "idle")
		got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "no eviction during the interlock", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("no RAM accounting (no sample) means no interlock", func(t *testing.T) {
		snap := base()
		snap.HostRAMBudgetMB = 0 // the host has produced no MemAvailable reading yet
		got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "an unconstrained host does not interlock", got, Decision{OK: true, Evict: []string{}})
	})

	t.Run("cleared flag lets the ordinary arithmetic through", func(t *testing.T) {
		snap := base()
		snap.RAMBootstrapInFlight = false
		got := Admit(snap, Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "no interlock, candidate fits", got, Decision{OK: true, Evict: []string{}})
	})
}

// TestRAMBootstrapStartsWithoutEvictionWhenHeadroom is the common case (task
// 4.2): an unknown-RAM candidate is charged a conservative first guess
// (snap.RAMBootstrapGuessMB) instead of nothing, and when the host has headroom
// for the guess it just starts and measures -- no eviction. Default-on RAM
// accounting must not churn the host on every new spec's first start.
func TestRAMBootstrapStartsWithoutEvictionWhenHeadroom(t *testing.T) {
	snap := PolicySnapshot{
		Running:             []RunningProc{{SpecID: "idle", RAMMB: 6000, LastUsed: t0}},
		HostRAMBudgetMB:     40000,
		RAMBootstrapGuessMB: 8000,
		Allowed:             allowPairs("cand", "idle"),
	}
	// 8000 guess + 6000 resident = 14000 <= 40000: fits, and the idle neighbour
	// that the arithmetic COULD have drained is left untouched.
	got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
	assertDecision(t, "unknown-RAM candidate admits on the guess, no eviction", got, Decision{OK: true, Evict: []string{}})

	// The guess is charged like any demand: sized up so it just fits is still OK.
	snap.HostRAMBudgetMB = 14000
	got = Admit(snap, Spec{ID: "cand", RAMMB: 0})
	assertDecision(t, "the guess summing exactly to the pool fits", got, Decision{OK: true, Evict: []string{}})
}

// TestRAMBootstrapPreEvictsUnderPressure: only when the host is genuinely too
// tight for the guess does the bootstrap pre-evict idle processes, host-wide and
// oldest-LastUsed first, until the guess fits -- the same machinery rule 6 uses
// for a known demand, just with the guess standing in for the unknown one.
func TestRAMBootstrapPreEvictsUnderPressure(t *testing.T) {
	t.Run("one idle neighbour is drained to fit the guess", func(t *testing.T) {
		snap := PolicySnapshot{
			Running:             []RunningProc{{SpecID: "idle", RAMMB: 6000, LastUsed: t0}},
			HostRAMBudgetMB:     10000,
			RAMBootstrapGuessMB: 8000,
			Allowed:             allowPairs("cand", "idle"),
		}
		// 8000 + 6000 = 14000 > 10000; draining "idle" leaves the 8000 guess alone.
		got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
		assertDecision(t, "the idle neighbour is pre-evicted for the guess", got, Decision{Evict: []string{"idle"}})
	})

	t.Run("victims are drawn oldest-first, host-wide, until the guess fits", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "new", RAMMB: 5000, LastUsed: t2},
				{SpecID: "old", RAMMB: 5000, LastUsed: t0},
				{SpecID: "mid", RAMMB: 5000, LastUsed: t1},
			},
			HostRAMBudgetMB:     16000,
			RAMBootstrapGuessMB: 8000,
			Allowed:             allowPairs("cand", "new", "old", "mid"),
		}
		// 8000 + 15000 = 23000; drain old -> 18000 (over); drain mid -> 13000 (fits).
		got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
		assertDecision(t, "oldest-first until the guess fits", got, Decision{Evict: []string{"old", "mid"}})
	})
}

// TestRAMBootstrapWaitsWhenNoIdleVictimUnderPressure is the safety crux: when the
// host is too tight for the guess AND nothing idle can be drained to make room,
// the bootstrap WAITS (WaitOnRAM) -- it never blind-starts into RAM that is not
// there, which is exactly how a cold start would OOM its neighbours.
func TestRAMBootstrapWaitsWhenNoIdleVictimUnderPressure(t *testing.T) {
	cases := []struct {
		name    string
		running []RunningProc
	}{
		{
			name: "every neighbour is busy",
			running: []RunningProc{
				{SpecID: "busy-a", RAMMB: 6000, InFlight: 1, LastUsed: t0},
				{SpecID: "busy-b", RAMMB: 4000, InFlight: 2, LastUsed: t1},
			},
		},
		{
			name: "the only roomy neighbour is pinned",
			running: []RunningProc{
				{SpecID: "pinned", RAMMB: 6000, Pinned: true, LastUsed: t0},
				{SpecID: "busy", RAMMB: 4000, InFlight: 1, LastUsed: t1},
			},
		},
		{
			name: "a neighbour still loading is never a victim",
			running: []RunningProc{
				{SpecID: "loading", RAMMB: 6000, Starting: true, LastUsed: t0},
				{SpecID: "busy", RAMMB: 4000, InFlight: 1, LastUsed: t1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := PolicySnapshot{
				Running:             tc.running,
				HostRAMBudgetMB:     10000,
				RAMBootstrapGuessMB: 8000,
				Allowed:             allowPairs("cand", "busy-a", "busy-b", "pinned", "busy", "loading"),
			}
			// 8000 guess + 10000 resident = 18000 > 10000, and nothing drainable
			// can bring it down: Wait, never a start, never a partial eviction.
			got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
			assertDecision(t, tc.name, got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
		})
	}
}

// TestRAMBootstrapGuessNeverRefusesTerminally: the guess is an inflated stand-in,
// not the spec's real demand, so a guess the pool cannot hold -- even with
// everything evicted -- must still WAIT (resolvable by a fresh sample, an estimate
// or room opening up), never the terminal own-demand refusal that a KNOWN
// oversized demand earns. Refusing a spec terminally on a number nobody measured
// would be wrong.
func TestRAMBootstrapGuessNeverRefusesTerminally(t *testing.T) {
	snap := PolicySnapshot{
		HostRAMBudgetMB:     4000,
		HostRAMCeilingMB:    4000,
		RAMBootstrapGuessMB: 8000, // alone exceeds the whole pool
		Allowed:             allowPairs("cand"),
	}
	got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
	assertDecision(t, "an oversized guess waits, never refuses", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
}

// TestRAMBootstrapGuessUnconfiguredStartsUnconstrained: with the knob off
// (RAMBootstrapGuessMB 0) an unknown-RAM candidate charges nothing and starts as
// it did before the bootstrap existed, however tight the pool -- the behaviour
// TestAdmitRAMUnknownDemandNotGatedWithoutAGuess pins, here asserted explicitly
// against the guess field.
func TestRAMBootstrapGuessUnconfiguredStartsUnconstrained(t *testing.T) {
	snap := PolicySnapshot{
		Running:             []RunningProc{{SpecID: "resident", RAMMB: 10000, LastUsed: t0}},
		HostRAMBudgetMB:     4000, // already overflowed by the resident alone
		RAMBootstrapGuessMB: 0,    // the knob is off
		Allowed:             allowPairs("cand", "resident"),
	}
	got := Admit(snap, Spec{ID: "cand", RAMMB: 0})
	assertDecision(t, "no guess configured: unknown candidate is not gated", got, Decision{OK: true, Evict: []string{}})
}

// TestRAMBootstrapComposesWithOtherRules: the guess path is the ordinary rule 6
// with the guess substituted, so it shares toEvict with the other rules -- a
// process already queued by a VRAM budget is not charged or drained twice, and the
// process-count limit asks only for what the guess did not already supply.
func TestRAMBootstrapComposesWithOtherRules(t *testing.T) {
	t.Run("a VRAM victim is not double-counted by the guess", func(t *testing.T) {
		// "occ" is both over GPU 0's budget (rule 3 evicts it) and the only RAM
		// holder. The guess must see it as already gone, not drain a second victim.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "occ", GPUs: map[int]int{0: 5000}, RAMMB: 6000, LastUsed: t0},
				{SpecID: "other", RAMMB: 3000, LastUsed: t1},
			},
			Budgets:             map[int]int{0: 5000},
			HostRAMBudgetMB:     12000,
			RAMBootstrapGuessMB: 4000,
			Allowed:             allowPairs("cand", "occ", "other"),
		}
		// candidate wants GPU 0 at 3000: 3000 + occ 5000 = 8000 > 5000 budget ->
		// rule 3 evicts occ. RAM: guess 4000 + other 3000 = 7000 (occ already gone)
		// <= 12000, so the guess needs no further victim: Evict is just [occ].
		got := Admit(snap, Spec{ID: "cand", RAMMB: 0, GPUs: []SpecGPU{{Index: 0, VRAMMB: 3000}}})
		assertDecision(t, "the VRAM victim covers the RAM too", got, Decision{Evict: []string{"occ"}})
	})
}

// TestAdmitWaitOnRAMOnlyWhenTheRAMPoolBlocked pins what Decision.WaitOnRAM
// means: rule 6 was among the causes of a Wait. It is what lets the manager
// re-ask a waiter when the hardware sample refreshes -- and ONLY then, because a
// Wait on the co-residency matrix, the process limit or a VRAM budget cannot be
// changed by a new sample, and re-asking those once a second would run the
// admission path (and its VRAM measurement) for nothing.
func TestAdmitWaitOnRAMOnlyWhenTheRAMPoolBlocked(t *testing.T) {
	t.Run("a matrix wait with RAM fitting is not a RAM wait", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "busy", RAMMB: 1000, InFlight: 1, LastUsed: t0},
			},
			HostRAMBudgetMB: 16000,
			// the (cand, busy) pair is closed
		}
		assertDecision(t, "matrix only", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, Evict: []string{}})
	})

	t.Run("a process-limit wait with RAM fitting is not a RAM wait", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "busy", RAMMB: 1000, InFlight: 1, LastUsed: t0},
			},
			MaxProcesses:    1,
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "busy"),
		}
		assertDecision(t, "process limit only", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, Evict: []string{}})
	})

	t.Run("a wait the RAM pool contributed to is flagged even when another rule also blocks", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "busy-closed", RAMMB: 100, InFlight: 1, LastUsed: t0},
				{SpecID: "busy-holder", RAMMB: 10000, InFlight: 1, LastUsed: t1},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "busy-holder"),
		}
		assertDecision(t, "matrix and RAM", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})

	t.Run("an eviction or an admission is never flagged", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "idle", RAMMB: 10000, LastUsed: t0},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "idle"),
		}
		assertDecision(t, "evict", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"idle"}})
		assertDecision(t, "admit", Admit(snap, Spec{ID: "cand", RAMMB: 1000}), Decision{OK: true, Evict: []string{}})
	})
}

// TestAdmitRAMOwnDemandExceedsBudgetIsTerminal is rule 6's own-demand half, the
// host-wide twin of TestAdmitCandidateAloneExceedsBudgetIsNotPermitted. The pool
// is a TOTAL for the managed set, so the smallest sum the arithmetic can ever
// reach is the candidate alone, with every other process gone: a known demand
// above the whole budget cannot be made to fit by any eviction, and waiting
// cannot help. It must therefore be a durable not_permitted carrying a Message,
// not a silent Wait that hangs the caller to its admission timeout on every
// request -- and not a mass eviction of the idle set for a start that still
// could not fit.
func TestAdmitRAMOwnDemandExceedsBudgetIsTerminal(t *testing.T) {
	const message = "spec cand: RAM demand 9000 MB exceeds the host RAM budget 8000 MB on its own"
	terminal := Decision{Reason: StateNotPermitted, Message: message, Evict: []string{}}

	t.Run("with nothing running", func(t *testing.T) {
		snap := PolicySnapshot{HostRAMBudgetMB: 8000}
		assertDecision(t, "candidate alone exceeds the budget", Admit(snap, Spec{ID: "cand", RAMMB: 9000}), terminal)
	})

	t.Run("idle holders are not drained for a start that cannot fit", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "idle-a", RAMMB: 3000, LastUsed: t0},
				{SpecID: "idle-b", RAMMB: 3000, LastUsed: t1},
			},
			HostRAMBudgetMB: 8000,
			Allowed:         allowPairs("cand", "idle-a", "idle-b"),
		}
		assertDecision(t, "no eviction offered", Admit(snap, Spec{ID: "cand", RAMMB: 9000}), terminal)
	})

	t.Run("the boundary is inclusive", func(t *testing.T) {
		snap := PolicySnapshot{HostRAMBudgetMB: 9000}
		assertDecision(t, "a demand equal to the budget fits", Admit(snap, Spec{ID: "cand", RAMMB: 9000}), Decision{OK: true, Evict: []string{}})
	})

	t.Run("an unknown demand is not refused", func(t *testing.T) {
		snap := PolicySnapshot{HostRAMBudgetMB: 8000}
		assertDecision(t, "RAMMB 0 is unknown, not gated", Admit(snap, Spec{ID: "cand", RAMMB: 0}), Decision{OK: true, Evict: []string{}})
	})

	t.Run("an unconstrained budget refuses nothing", func(t *testing.T) {
		snap := PolicySnapshot{HostRAMBudgetMB: 0}
		assertDecision(t, "budget 0 is unconstrained", Admit(snap, Spec{ID: "cand", RAMMB: 1_000_000}), Decision{OK: true, Evict: []string{}})
	})

	t.Run("it is reported ahead of a pinned unknown-VRAM occupant", func(t *testing.T) {
		// Without the hoist the candidate's unknown VRAM demand against a pinned
		// occupant on its card would answer pending_vram_unknown, inviting the
		// operator to fill in a number that cannot make 9000 MB fit under 8000.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "pinned", GPUs: map[int]int{0: 4000}, Pinned: true, LastUsed: t0},
			},
			HostRAMBudgetMB: 8000,
			Allowed:         allowPairs("cand", "pinned"),
		}
		spec := Spec{ID: "cand", RAMMB: 9000, GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
		assertDecision(t, "durable cause wins over the resolvable one", Admit(snap, spec), terminal)
	})

	t.Run("a VRAM own-demand refusal keeps reporting first", func(t *testing.T) {
		snap := PolicySnapshot{
			Budgets:         map[int]int{0: 4000},
			HostRAMBudgetMB: 8000,
		}
		spec := Spec{ID: "cand", RAMMB: 9000, GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}}
		want := Decision{
			Reason:  StateNotPermitted,
			Message: "spec cand: gpu 0 demand 5000 MB exceeds budget 4000 MB on its own",
			Evict:   []string{},
		}
		assertDecision(t, "a spec over both budgets reports the VRAM cause", Admit(snap, spec), want)
	})
}

// TestAdmitRAMOwnDemandRefusalComparesAgainstTheCeiling: the terminal refusal is
// measured against the most the pool could be offering (HostRAMCeilingMB), not
// against the budget, because the budget is transiently understated -- by a
// process still loading, and for a sample or two after a victim exits, which is
// exactly when the beneficiary of its eviction is re-admitted. A demand between
// the budget and the ceiling is a RAM Wait (re-asked on the next sample), never a
// refusal and never an eviction of everything for a start that cannot yet be shown
// to fit; only a demand above the ceiling is terminal.
func TestAdmitRAMOwnDemandRefusalComparesAgainstTheCeiling(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "idle", RAMMB: 3000, LastUsed: t0},
			{SpecID: "busy", RAMMB: 3000, InFlight: 1, LastUsed: t1},
		},
		HostRAMBudgetMB:  8000,
		HostRAMCeilingMB: 18000,
		Allowed:          allowPairs("cand", "idle", "busy"),
	}

	t.Run("between the budget and the ceiling it waits", func(t *testing.T) {
		got := Admit(snap, Spec{ID: "cand", RAMMB: 9000})
		assertDecision(t, "9000 MB is over the budget but within the ceiling", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})
	t.Run("the ceiling boundary is inclusive", func(t *testing.T) {
		got := Admit(snap, Spec{ID: "cand", RAMMB: 18000})
		assertDecision(t, "a demand equal to the ceiling is not refused", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})
	t.Run("above the ceiling it is terminal and names the ceiling", func(t *testing.T) {
		got := Admit(snap, Spec{ID: "cand", RAMMB: 18001})
		assertDecision(t, "18001 MB exceeds even the ceiling", got, Decision{
			Reason:  StateNotPermitted,
			Message: "spec cand: RAM demand 18001 MB exceeds the host RAM budget 18000 MB on its own",
			Evict:   []string{},
		})
	})
	t.Run("a ceiling below the budget is ignored", func(t *testing.T) {
		low := snap
		low.HostRAMCeilingMB = 100
		got := Admit(low, Spec{ID: "cand", RAMMB: 8000})
		assertDecision(t, "the budget is the floor of the limit", got, Decision{Wait: true, WaitOnRAM: true, Evict: []string{}})
	})
	t.Run("no ceiling falls back to the budget", func(t *testing.T) {
		plain := snap
		plain.HostRAMCeilingMB = 0
		got := Admit(plain, Spec{ID: "cand", RAMMB: 8001})
		assertDecision(t, "only the budget is known", got, Decision{
			Reason:  StateNotPermitted,
			Message: "spec cand: RAM demand 8001 MB exceeds the host RAM budget 8000 MB on its own",
			Evict:   []string{},
		})
	})
}

// TestAdmitRAMSelfIsNotCountedAgainstThePool: a running process sharing the
// candidate's ID is the same physical process (a double-start race, a snapshot
// taken a moment late), so the self-filter must keep it out of the sum --
// charging it on top of the candidate's own demand books one process as two.
func TestAdmitRAMSelfIsNotCountedAgainstThePool(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{
			{SpecID: "cand", RAMMB: 9000, LastUsed: t0},
		},
		HostRAMBudgetMB: 10000,
	}
	got := Admit(snap, Spec{ID: "cand", RAMMB: 9000})
	assertDecision(t, "self RAM is not double-counted", got, Decision{OK: true, Evict: []string{}})
}

// TestAdmitRAMComposesWithOtherRules: rule 6 runs after rules 1, 3, 4 and 5 and
// before rule 2, against the SAME toEvict set, so a victim another rule already
// queued is treated as gone -- its RAM is not charged again and it is not
// evicted twice -- and the process-count rule runs last and asks only for the
// victims still missing.
func TestAdmitRAMComposesWithOtherRules(t *testing.T) {
	t.Run("a VRAM victim's RAM already counts as freed", func(t *testing.T) {
		// Rule 3: GPU 0 holds 8000 + 8000 against 10000, so "gpu-hog" goes. Its
		// 10000 of RAM is then gone too: 8000 + 6000 = 14000 fits 16000, so the
		// idle "bystander" must NOT also be evicted for RAM. The bystander is
		// the OLDER of the two on purpose: charged the hog's RAM again, rule 6
		// would reach for the oldest idle process first and take it too.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "gpu-hog", GPUs: map[int]int{0: 8000}, RAMMB: 10000, LastUsed: t1},
				{SpecID: "bystander", RAMMB: 6000, LastUsed: t0},
			},
			Budgets:         map[int]int{0: 10000},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "gpu-hog", "bystander"),
		}
		spec := Spec{ID: "cand", RAMMB: 8000, GPUs: []SpecGPU{{Index: 0, VRAMMB: 8000}}}
		assertDecision(t, "no double eviction", Admit(snap, spec), Decision{Evict: []string{"gpu-hog"}})
	})

	t.Run("a matrix victim's RAM already counts as freed", func(t *testing.T) {
		// "closed" is not allowed beside the candidate (rule 1) and is idle, so it
		// is queued; with its 10000 gone, 8000 + 4000 fits 16000 and the OLDER
		// "open" process is left alone.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "closed", RAMMB: 10000, LastUsed: t1},
				{SpecID: "open", RAMMB: 4000, LastUsed: t0},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "open"),
		}
		assertDecision(t, "matrix victim frees RAM", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"closed"}})
	})

	t.Run("RAM and VRAM victims are merged oldest first", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "ram-victim", RAMMB: 12000, LastUsed: t2},
				{SpecID: "gpu-victim", GPUs: map[int]int{0: 9000}, RAMMB: 100, LastUsed: t0},
			},
			Budgets:         map[int]int{0: 10000},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "ram-victim", "gpu-victim"),
		}
		spec := Spec{ID: "cand", RAMMB: 8000, GPUs: []SpecGPU{{Index: 0, VRAMMB: 5000}}}
		// Rule 3 drains gpu-victim (frees 100 RAM); rule 6 still overflows
		// (8000 + 12000 = 20000 > 16000) and drains ram-victim.
		assertDecision(t, "both rules contribute", Admit(snap, spec), Decision{Evict: []string{"gpu-victim", "ram-victim"}})
	})

	t.Run("the process limit asks only for what RAM did not already free", func(t *testing.T) {
		// MaxProcesses 3 with three running needs ONE eviction for the new start.
		// Rule 6 drains "a" for RAM ("unknown" holds nothing it can count, so it
		// is not a RAM victim even though it is older), which already makes room
		// in the process count: rule 2 must see that and add nothing.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "unknown", RAMMB: 0, LastUsed: t0},
				{SpecID: "a", RAMMB: 10000, LastUsed: t1},
				{SpecID: "b", RAMMB: 4000, LastUsed: t2},
			},
			MaxProcesses:    3,
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "unknown", "a", "b"),
		}
		assertDecision(t, "limit satisfied by the RAM victim", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"a"}})
	})

	t.Run("the process limit still evicts when RAM fits", func(t *testing.T) {
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "a", RAMMB: 1000, LastUsed: t0},
				{SpecID: "b", RAMMB: 1000, LastUsed: t1},
			},
			MaxProcesses:    2,
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "a", "b"),
		}
		assertDecision(t, "rule 2 unaffected", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Evict: []string{"a"}})
	})

	t.Run("a blocked rule elsewhere still makes the whole decision a Wait", func(t *testing.T) {
		// RAM could be satisfied by draining the idle holder, but the busy
		// matrix-incompatible process blocks the start: Wait, no partial evict.
		snap := PolicySnapshot{
			Running: []RunningProc{
				{SpecID: "busy-closed", RAMMB: 100, InFlight: 1, LastUsed: t0},
				{SpecID: "idle-holder", RAMMB: 10000, LastUsed: t1},
			},
			HostRAMBudgetMB: 16000,
			Allowed:         allowPairs("cand", "idle-holder"),
		}
		assertDecision(t, "no partial eviction", Admit(snap, Spec{ID: "cand", RAMMB: 8000}), Decision{Wait: true, Evict: []string{}})
	})
}

// TestHostRAMPoolChargesACandidateAgainstGenuinelyFreeRAM is the reason the pool
// adds back what the managed set holds, proved through the snapshot builder AND
// Admit together. Two managed processes hold 10000 + 6000 MB, the host reports
// 12000 MB available and the reserve is 2000, so 10000 MB is genuinely free: a
// 10000 MB candidate fits without evicting anything, and a 10001 MB one needs
// exactly one victim. A pool of plain MemAvailable - reserve (also 10000) would
// charge the 16000 MB the managed set holds on top of it and evict both
// processes for the 10000 MB start that fits.
func TestHostRAMPoolChargesACandidateAgainstGenuinelyFreeRAM(t *testing.T) {
	o, st := newSnapshotOwner(baseSpec("spec-a", "model-a"), 4242, nil)
	st.spec.RAMMB = 10000
	st.measuredRAM = 10000
	addRunningRAMSpec(o, "spec-b", 6000, 6000)
	o.hostRAMReserveMB = 2000
	o.allowedPairs = allowPairs("cand", "spec-a", "spec-b")
	o.m.SetHostInfo(HostInfo{MemAvailableMB: 12000})

	snap := o.buildSnapshot()
	if got, want := snap.HostRAMBudgetMB, 26000; got != want {
		t.Fatalf("HostRAMBudgetMB = %d, want %d (12000 available + 16000 held - 2000 reserve)", got, want)
	}

	cand := baseSpec("cand", "model-cand")
	cand.RAMMB = 10000
	assertDecision(t, "10000 MB candidate against 10000 MB genuinely free", Admit(snap, cand), Decision{OK: true, Evict: []string{}})

	cand.RAMMB = 10001
	assertDecision(t, "one MB more than is free", Admit(snap, cand), Decision{Evict: []string{"spec-a"}})
}

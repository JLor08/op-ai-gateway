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
// every case task-12-brief.md lists. Each case asserts the FULL Decision
// (OK, Wait, Reason, Message, and the exact Evict slice in order) --
// checking OK alone would pass against an implementation that evicts the
// wrong process, the wrong number of them, or in the wrong order; leaving
// Message unchecked would pass against an implementation that leaked
// disambiguating text onto the wrong path (every case in this table wants
// Message == "", the zero value, so assertDecision's check of it here is
// exercised on every single case, not just the one dedicated Message test).
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
			// 11: unknown VRAM, gpu occupied by idle -> evict that proc.
			name: "unknown vram occupied by idle evicts it",
			snap: PolicySnapshot{
				Running: []RunningProc{
					{SpecID: "occupant", GPUs: map[int]int{0: 5000}, LastUsed: t0},
				},
				Allowed: allowPairs("cand", "occupant"),
			},
			spec: Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}},
			want: Decision{Evict: []string{"occupant"}},
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
			// unconstrained, never zero-budget. A huge existing occupant
			// does not block the candidate.
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
	assertDecision(t, "unknown vram busy blocker waits", got, Decision{Wait: true, Evict: []string{}})
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

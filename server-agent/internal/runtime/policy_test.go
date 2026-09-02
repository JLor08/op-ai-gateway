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
			want: Decision{Wait: true, Evict: []string{}},
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
			want: Decision{Wait: true, Evict: []string{}},
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
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// Unchanged: a busy occupant was never evictable anyway.
			name: "busy known occupant still waits",
			snap: PolicySnapshot{Running: occupant(func(r *RunningProc) { r.InFlight = 1 }), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			// Unchanged: isEvictable's Starting clause (the C3 fix) already
			// spared a still-loading occupant.
			name: "still-loading known occupant still waits",
			snap: PolicySnapshot{Running: occupant(func(r *RunningProc) { r.Starting = true }), Allowed: allowPairs("cand", "occupant"), Budgets: map[int]int{0: 20000}},
			spec: unknownCandidate,
			want: Decision{Wait: true, Evict: []string{}},
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
			want: Decision{Wait: true, Evict: []string{}},
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
			want: Decision{Wait: true, Evict: []string{}},
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
		Decision{Wait: true, Evict: []string{}})

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

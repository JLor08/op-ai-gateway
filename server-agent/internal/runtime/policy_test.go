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
	if got.Evict == nil {
		t.Errorf("%s: Evict = nil, want a non-nil slice (empty or populated)", name)
	}
	if !reflect.DeepEqual(got.Evict, want.Evict) {
		t.Errorf("%s: Evict = %v, want %v", name, got.Evict, want.Evict)
	}
}

// TestAdmit is table-driven over PolicySnapshot x Spec -> Decision, covering
// every case task-12-brief.md lists. Each case asserts the FULL Decision
// (OK, Wait, Reason, and the exact Evict slice in order) -- checking OK
// alone would pass against an implementation that evicts the wrong process,
// the wrong number of them, or in the wrong order.
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

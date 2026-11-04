// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"strings"
	"testing"
	"time"
)

// Admission of model instances (#340): siblings co-reside and are
// never each other's victims, an instance inherits its spec's co-residency
// verdicts, an instance is not evictable while a higher-index sibling runs,
// surplus instances are drawn first, and a NoEvict (burst) admission answers
// only OK or NoCapacity.

// instanceSpec is the per-instance copy of base spec id the manager admits:
// the instance key as ID, and InstanceOf naming the base for k >= 1 (instance
// 0 is the spec itself). It demands vramMB on gpu 0.
func instanceSpec(base string, k, vramMB int) Spec {
	s := Spec{ID: InstanceKey(base, k), Instance: k, GPUs: []SpecGPU{{Index: 0, VRAMMB: vramMB}}}
	if k > 0 {
		s.InstanceOf = base
	}
	return s
}

// instanceProc is a running, idle instance k of base holding vramMB on gpu 0.
func instanceProc(base string, k, vramMB int) RunningProc {
	return RunningProc{SpecID: InstanceKey(base, k), Spec: base, Instance: k, GPUs: map[int]int{0: vramMB}}
}

// assertNoCapacity checks the one shape a NoEvict admission answers when it
// cannot start into free room: NoCapacity, nothing else set, a non-nil empty
// Evict, and a Message that names the cause (wantInMessage).
func assertNoCapacity(t *testing.T, name string, got Decision, wantInMessage string) {
	t.Helper()
	if !got.NoCapacity {
		t.Errorf("%s: NoCapacity = false, want true (decision %+v)", name, got)
	}
	if got.OK || got.Wait || got.WaitOnRAM || got.WaitOnGrace || got.VRAMBootstrap || got.Reason != "" {
		t.Errorf("%s: want only NoCapacity set, got %+v", name, got)
	}
	if got.Evict == nil || len(got.Evict) != 0 {
		t.Errorf("%s: Evict = %#v, want a non-nil empty slice", name, got.Evict)
	}
	if !strings.Contains(got.Message, wantInMessage) {
		t.Errorf("%s: Message = %q, want it to name %q", name, got.Message, wantInMessage)
	}
}

// assertNotNoCapacity guards the non-NoEvict path: NoCapacity is reserved for
// NoEvict admissions and must never leak onto an ordinary decision.
func assertNotNoCapacity(t *testing.T, name string, got Decision) {
	t.Helper()
	if got.NoCapacity {
		t.Errorf("%s: NoCapacity = true without NoEvict, decision %+v", name, got)
	}
}

func TestBaseOfFallsBackToSpecID(t *testing.T) {
	if got := baseOf(RunningProc{SpecID: "S"}); got != "S" {
		t.Errorf("baseOf(no Spec) = %q, want the SpecID %q", got, "S")
	}
	if got := baseOf(RunningProc{SpecID: "S#2", Spec: "S"}); got != "S" {
		t.Errorf("baseOf(S#2 of S) = %q, want %q", got, "S")
	}
}

// TestAdmitSiblingsAreAlwaysCoResident: the gateway's matrix never holds a
// self-pair, so without the sibling short-circuit an empty Allowed would read
// as "S may not run next to S" and evict the warm sibling. Covered from both
// ends, and for an occupant built without RunningProc.Spec (today's shape),
// which is its own base.
func TestAdmitSiblingsAreAlwaysCoResident(t *testing.T) {
	cases := []struct {
		name string
		proc RunningProc
		spec Spec
	}{
		{"instance 1 next to instance 0", RunningProc{SpecID: "S", Spec: "S"}, Spec{ID: "S#1", InstanceOf: "S", Instance: 1}},
		{"instance 1 next to an instance 0 without Spec", RunningProc{SpecID: "S"}, Spec{ID: "S#1", InstanceOf: "S", Instance: 1}},
		{"instance 0 next to instance 1", RunningProc{SpecID: "S#1", Spec: "S", Instance: 1}, Spec{ID: "S"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snap := PolicySnapshot{Running: []RunningProc{tc.proc}, Allowed: map[[2]string]bool{}}
			got := Admit(snap, tc.spec)
			assertDecision(t, tc.name, got, Decision{OK: true, Evict: []string{}})
			assertNotNoCapacity(t, tc.name, got)
		})
	}
}

// TestAdmitInstanceInheritsSpecPairs: a pair is looked up by BASE ids, so an
// instance inherits its spec's verdicts from either side of the pair -- an
// open S/T cell admits S#1 next to T (and X next to T#1), a closed one evicts
// the idle occupant exactly as it would for S itself.
func TestAdmitInstanceInheritsSpecPairs(t *testing.T) {
	idleT := RunningProc{SpecID: "T", Spec: "T", LastUsed: t0}
	s1 := Spec{ID: "S#1", InstanceOf: "S", Instance: 1}

	open := PolicySnapshot{Running: []RunningProc{idleT}, Allowed: allowPairs("S", "T")}
	assertDecision(t, "candidate instance, open pair", Admit(open, s1), Decision{OK: true, Evict: []string{}})

	closed := PolicySnapshot{Running: []RunningProc{idleT}, Allowed: map[[2]string]bool{}}
	asBase := Admit(closed, Spec{ID: "S"})
	assertDecision(t, "base spec, closed pair", asBase, Decision{Evict: []string{"T"}})
	assertDecision(t, "candidate instance, closed pair", Admit(closed, s1), asBase)

	t1 := RunningProc{SpecID: "T#1", Spec: "T", Instance: 1, LastUsed: t0}
	occOpen := PolicySnapshot{Running: []RunningProc{t1}, Allowed: allowPairs("X", "T")}
	assertDecision(t, "occupant instance, open pair", Admit(occOpen, Spec{ID: "X"}), Decision{OK: true, Evict: []string{}})
	occClosed := PolicySnapshot{Running: []RunningProc{t1}, Allowed: map[[2]string]bool{}}
	assertDecision(t, "occupant instance, closed pair", Admit(occClosed, Spec{ID: "X"}), Decision{Evict: []string{"T#1"}})
}

// TestAdmitPinnedUnknownOccupantUsesBasePair: the pinned-unknown occupant
// refusal reports a closed matrix cell as not_permitted and an open one as
// pending_vram_unknown, so it must read the pair the way rule 1 does -- by base,
// with siblings always open. Looking up the raw instance key "S#1" would find no
// cell and misreport an open pair as closed.
func TestAdmitPinnedUnknownOccupantUsesBasePair(t *testing.T) {
	pinnedUnknownT := RunningProc{SpecID: "T", Spec: "T", Pinned: true, GPUs: map[int]int{0: 0}}
	snap := PolicySnapshot{Running: []RunningProc{pinnedUnknownT}, Allowed: allowPairs("S", "T")}
	got := Admit(snap, instanceSpec("S", 1, 1000))
	if got.Reason != StatePendingVRAMUnknown {
		t.Errorf("open S/T pair: Reason = %q (%s), want %q", got.Reason, got.Message, StatePendingVRAMUnknown)
	}

	pinnedUnknownSibling := RunningProc{SpecID: "S", Spec: "S", Pinned: true, GPUs: map[int]int{0: 0}}
	snap = PolicySnapshot{Running: []RunningProc{pinnedUnknownSibling}, Allowed: map[[2]string]bool{}}
	got = Admit(snap, instanceSpec("S", 1, 1000))
	if got.Reason != StatePendingVRAMUnknown {
		t.Errorf("pinned unknown sibling: Reason = %q (%s), want %q", got.Reason, got.Message, StatePendingVRAMUnknown)
	}
}

// TestAdmitSiblingNeverVictim: evicting a warm sibling to start a cold one is
// never useful, so the idle sibling that alone fills the room is not drawn by
// any resource rule -- the candidate waits instead.
func TestAdmitSiblingNeverVictim(t *testing.T) {
	s2 := instanceProc("S", 2, 6000)
	s2.RAMMB = 6000
	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want Decision
	}{
		{
			name: "per-GPU VRAM",
			snap: PolicySnapshot{Running: []RunningProc{s2}, Budgets: map[int]int{0: 10000}},
			spec: instanceSpec("S", 1, 6000),
			want: Decision{Wait: true, Evict: []string{}},
		},
		{
			name: "host RAM",
			snap: PolicySnapshot{Running: []RunningProc{s2}, HostRAMBudgetMB: 10000},
			spec: Spec{ID: "S#1", InstanceOf: "S", Instance: 1, RAMMB: 6000},
			want: Decision{Wait: true, WaitOnRAM: true, Evict: []string{}},
		},
		{
			name: "process count",
			snap: PolicySnapshot{Running: []RunningProc{s2}, MaxProcesses: 1},
			spec: Spec{ID: "S#1", InstanceOf: "S", Instance: 1},
			want: Decision{Wait: true, Evict: []string{}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Admit(tc.snap, tc.spec)
			assertDecision(t, tc.name, got, tc.want)
			assertNotNoCapacity(t, tc.name, got)
		})
	}
}

// TestAdmitLowerIndexNotEvictableWhileHigherRuns pins the ordering invariant
// (highest index leaves first): instance 0 is not a victim while instance 1
// runs, even when instance 0 is the idle one and the LRU order would pick it.
func TestAdmitLowerIndexNotEvictableWhileHigherRuns(t *testing.T) {
	budgets := map[int]int{0: 10000}
	// The instance key is listed beside its base so the matrix is open however
	// the pair is looked up: this test is about the victim gate, not rule 1.
	allowed := allowPairs("X", "S", "S#1", "T")
	x := Spec{ID: "X", GPUs: []SpecGPU{{Index: 0, VRAMMB: 4000}}}

	s0 := instanceProc("S", 0, 4000)
	s0.LastUsed = t0
	s1Busy := instanceProc("S", 1, 4000)
	s1Busy.InFlight = 1
	snap := PolicySnapshot{Running: []RunningProc{s0, s1Busy}, Budgets: budgets, Allowed: allowed}
	assertDecision(t, "idle S under busy S#1", Admit(snap, x), Decision{Wait: true, Evict: []string{}})

	// Control: the same idle S with an unrelated busy neighbour is the victim,
	// so the Wait above is the invariant and not a lack of room.
	tBusy := RunningProc{SpecID: "T", Spec: "T", InFlight: 1, GPUs: map[int]int{0: 4000}}
	snap = PolicySnapshot{Running: []RunningProc{s0, tBusy}, Budgets: budgets, Allowed: allowed}
	assertDecision(t, "idle S, no sibling", Admit(snap, x), Decision{Evict: []string{"S"}})

	// Both idle (neither marked surplus, so only the invariant decides): the
	// least-recently-used S would be drawn by LRU, but S#1 must leave first.
	s1Idle := instanceProc("S", 1, 4000)
	s1Idle.LastUsed = t3
	snap = PolicySnapshot{Running: []RunningProc{s0, s1Idle}, Budgets: budgets, Allowed: allowed}
	assertDecision(t, "both idle", Admit(snap, x), Decision{Evict: []string{"S#1"}})
}

// TestAdmitSurplusVictimsFirst: a running burst instance is drawn before any
// other idle victim -- ahead of the LRU order and ahead of the candidate's own
// role -- since its spec keeps serving from a lower sibling.
func TestAdmitSurplusVictimsFirst(t *testing.T) {
	x := Spec{ID: "X", Role: "chat", GPUs: []SpecGPU{{Index: 0, VRAMMB: 4000}}}
	budgets := map[int]int{0: 10000}
	allowed := allowPairs("X", "S", "S#1", "T") // open under either lookup, as above

	surplus := instanceProc("S", 1, 4000)
	surplus.Surplus = true
	surplus.Role = "chat"
	surplus.LastUsed = t3
	oldT := RunningProc{SpecID: "T", Spec: "T", Role: "chat", LastUsed: t0, GPUs: map[int]int{0: 4000}}

	snap := PolicySnapshot{Running: []RunningProc{oldT, surplus}, Budgets: budgets, Allowed: allowed}
	assertDecision(t, "same role", Admit(snap, x), Decision{Evict: []string{"S#1"}})

	surplus.Role = "tts" // another role than the candidate's; T keeps the candidate's
	snap = PolicySnapshot{Running: []RunningProc{oldT, surplus}, Budgets: budgets, Allowed: allowed}
	assertDecision(t, "surplus outranks role", Admit(snap, x), Decision{Evict: []string{"S#1"}})
}

func TestAdmitNoEvictRefusesInsteadOfEvicting(t *testing.T) {
	idle := RunningProc{SpecID: "idle-t", Spec: "idle-t", LastUsed: t0, GPUs: map[int]int{0: 6000}}
	snap := PolicySnapshot{
		Running: []RunningProc{idle},
		Budgets: map[int]int{0: 10000},
		Allowed: allowPairs("S", "idle-t"),
		NoEvict: true,
	}
	got := Admit(snap, instanceSpec("S", 2, 6000))
	assertNoCapacity(t, "would evict", got, "idle-t")

	snap.NoEvict = false
	assertDecision(t, "control without NoEvict", Admit(snap, instanceSpec("S", 2, 6000)), Decision{Evict: []string{"idle-t"}})
}

func TestAdmitNoEvictRefusesInsteadOfWaiting(t *testing.T) {
	busy := RunningProc{SpecID: "T", Spec: "T", InFlight: 1, GPUs: map[int]int{0: 6000}}
	snap := PolicySnapshot{
		Running: []RunningProc{busy},
		Budgets: map[int]int{0: 10000},
		Allowed: allowPairs("S", "T"),
		NoEvict: true,
	}
	assertNoCapacity(t, "would wait", Admit(snap, instanceSpec("S", 2, 6000)), "wait")
}

func TestAdmitNoEvictOKWhenFree(t *testing.T) {
	snap := PolicySnapshot{
		Running: []RunningProc{instanceProc("S", 0, 4000)},
		Budgets: map[int]int{0: 10000},
		NoEvict: true,
	}
	got := Admit(snap, instanceSpec("S", 1, 4000))
	assertDecision(t, "free room", got, Decision{OK: true, Evict: []string{}})
	assertNotNoCapacity(t, "free room", got)
}

// TestAdmitNoEvictRefusesEveryOtherAnswer: under NoEvict the only answers are
// OK and NoCapacity. Every other shape the pipeline can produce -- including a
// bootstrap eviction (whose shot must then stay unspent), a RAM or grace wait
// (whose wake flags must not leak) and the terminals -- is mapped, and the
// Message keeps naming the underlying cause.
func TestAdmitNoEvictRefusesEveryOtherAnswer(t *testing.T) {
	known := RunningProc{SpecID: "known-k", Spec: "known-k", LastUsed: t0, GPUs: map[int]int{0: 3000}}
	unknownCand := Spec{ID: "S#3", InstanceOf: "S", Instance: 3, GPUs: []SpecGPU{{Index: 0, VRAMMB: 0}}}
	cases := []struct {
		name          string
		snap          PolicySnapshot
		spec          Spec
		wantInMessage string
	}{
		{
			name: "VRAM bootstrap eviction",
			snap: PolicySnapshot{
				Running: []RunningProc{known}, Allowed: allowPairs("S", "known-k"),
				VRAMBootstrapEligible: true,
			},
			spec:          unknownCand,
			wantInMessage: "known-k",
		},
		{
			name:          "unknown-VRAM precedence wait",
			snap:          PolicySnapshot{Running: []RunningProc{known}, Allowed: allowPairs("S", "known-k")},
			spec:          unknownCand,
			wantInMessage: "rather than evicting it",
		},
		{
			name: "host RAM wait",
			snap: PolicySnapshot{
				HostRAMBudgetMB: 10000, RAMBootstrapInFlight: true,
			},
			spec:          Spec{ID: "S#3", InstanceOf: "S", Instance: 3, RAMMB: 1000},
			wantInMessage: "host RAM",
		},
		{
			name: "eviction grace wait",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "known-k", Spec: "known-k", LastUsed: t3, GPUs: map[int]int{0: 6000}}},
				Budgets: map[int]int{0: 10000}, Allowed: allowPairs("S", "known-k"),
				Now: t3.Add(time.Second), EvictionGrace: time.Minute,
			},
			spec:          instanceSpec("S", 3, 6000),
			wantInMessage: "grace",
		},
		{
			name: "pinned unknown-VRAM occupant",
			snap: PolicySnapshot{
				Running: []RunningProc{{SpecID: "P", Spec: "P", Pinned: true, GPUs: map[int]int{0: 0}}},
				Allowed: allowPairs("S", "P"),
			},
			spec:          instanceSpec("S", 3, 1000),
			wantInMessage: string(StatePendingVRAMUnknown),
		},
		{
			name:          "own demand over budget",
			snap:          PolicySnapshot{Budgets: map[int]int{0: 1000}},
			spec:          instanceSpec("S", 3, 2000),
			wantInMessage: "exceeds budget",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if plain := Admit(tc.snap, tc.spec); plain.OK {
				t.Fatalf("precondition: without NoEvict the answer must not be OK, got %+v", plain)
			}
			snap := tc.snap
			snap.NoEvict = true
			assertNoCapacity(t, tc.name, Admit(snap, tc.spec), tc.wantInMessage)
		})
	}
}

// TestAdmitInstancesCountTowardMaxProcesses: the self-filter is by instance
// key, so every running sibling is one process toward max_processes, and none
// of them is a victim for a third.
func TestAdmitInstancesCountTowardMaxProcesses(t *testing.T) {
	snap := PolicySnapshot{
		Running:      []RunningProc{{SpecID: "S", Spec: "S"}, {SpecID: "S#1", Spec: "S", Instance: 1}},
		MaxProcesses: 2,
	}
	cand := Spec{ID: "S#2", InstanceOf: "S", Instance: 2}
	assertDecision(t, "without NoEvict", Admit(snap, cand), Decision{Wait: true, Evict: []string{}})

	snap.NoEvict = true
	assertNoCapacity(t, "with NoEvict", Admit(snap, cand), "wait")
}

// TestAdmitSiblingVRAMSummed: siblings are ordinary occupants of the budget --
// a second instance fits beside the first, a third does not.
func TestAdmitSiblingVRAMSummed(t *testing.T) {
	s0 := instanceProc("S", 0, 4000) // measured
	snap := PolicySnapshot{Running: []RunningProc{s0}, Budgets: map[int]int{0: 10000}, NoEvict: true}
	assertDecision(t, "S#1 beside S", Admit(snap, instanceSpec("S", 1, 4000)), Decision{OK: true, Evict: []string{}})

	snap.Running = append(snap.Running, instanceProc("S", 1, 4000))
	assertNoCapacity(t, "S#2 beside S and S#1", Admit(snap, instanceSpec("S", 2, 4000)), "wait")
}

// TestAdmitEvictsSiblingsHighestIndexFirst: the ordering invariant discounts a
// sibling already queued to go, so when the start needs several idle instances
// of one spec gone, ONE decision evicts all of them -- drawn, and reported in
// Decision.Evict, highest index first. Without the discount only the highest
// would be evictable, the rest would block, and the Wait would carry no
// eviction at all: nothing would drain S#2, so the start would sit until idle
// unload happened to run. LastUsed is set so the oldest-first drain order alone
// would put the lower index first; the expected lists pin the override.
func TestAdmitEvictsSiblingsHighestIndexFirst(t *testing.T) {
	sBusy := RunningProc{SpecID: "S", Spec: "S", InFlight: 1, GPUs: map[int]int{0: 3000}, RAMMB: 3000}
	s1 := RunningProc{SpecID: "S#1", Spec: "S", Instance: 1, Surplus: true, LastUsed: t0, GPUs: map[int]int{0: 3000}, RAMMB: 3000}
	s2 := RunningProc{SpecID: "S#2", Spec: "S", Instance: 2, Surplus: true, LastUsed: t1, GPUs: map[int]int{0: 3000}, RAMMB: 3000}
	sIdle := RunningProc{SpecID: "S", Spec: "S", LastUsed: t0}
	s1Idle := RunningProc{SpecID: "S#1", Spec: "S", Instance: 1, Surplus: true, LastUsed: t1}
	s2Idle := RunningProc{SpecID: "S#2", Spec: "S", Instance: 2, Surplus: true, LastUsed: t2}

	cases := []struct {
		name string
		snap PolicySnapshot
		spec Spec
		want []string
	}{
		{
			name: "per-GPU VRAM",
			snap: PolicySnapshot{
				Running: []RunningProc{sBusy, s1, s2}, Budgets: map[int]int{0: 10000}, Allowed: allowPairs("X", "S"),
			},
			spec: Spec{ID: "X", GPUs: []SpecGPU{{Index: 0, VRAMMB: 7000}}},
			want: []string{"S#2", "S#1"},
		},
		{
			name: "host RAM",
			snap: PolicySnapshot{
				Running: []RunningProc{sBusy, s1, s2}, HostRAMBudgetMB: 10000, Allowed: allowPairs("X", "S"),
			},
			spec: Spec{ID: "X", RAMMB: 7000},
			want: []string{"S#2", "S#1"},
		},
		{
			name: "process count",
			snap: PolicySnapshot{
				Running: []RunningProc{sBusy, s1, s2}, MaxProcesses: 2, Allowed: allowPairs("X", "S"),
			},
			spec: Spec{ID: "X"},
			want: []string{"S#2", "S#1"},
		},
		{
			// Every instance of a closed pair must go, instance 0 included; the
			// running set lists the lowest first, so an unordered walk would
			// meet S while S#1 and S#2 still look like staying siblings.
			name: "matrix, all instances",
			snap: PolicySnapshot{Running: []RunningProc{sIdle, s1Idle, s2Idle}, Allowed: map[[2]string]bool{}},
			spec: Spec{ID: "X"},
			want: []string{"S#2", "S#1", "S"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Admit(tc.snap, tc.spec)
			assertDecision(t, tc.name, got, Decision{Evict: tc.want})
			assertNotNoCapacity(t, tc.name, got)
		})
	}
}

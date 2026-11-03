// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Manager-level tests for model instances (#340, design §5.2-§5.6): one
// specState per instance, burst instances created on demand and refused at once
// when there is no free room, protected instances started on Apply and never
// idle-unloaded, highest index first on the way down, and the per-instance
// launch resources. They use the same real stubchild process as manager_test.go.

// replicableSpec is baseSpec opted into instances, with a KNOWN host-RAM demand
// so a further instance is never refused as demand-unknown merely because the
// test did not declare one. The tests that are about unknown demand clear it.
func replicableSpec(id, model string) Spec {
	s := baseSpec(id, model)
	s.Replicable = true
	s.RAMMB = 64
	return s
}

// instanceStatus returns the Status row of instance k of specID, or nil.
// statusFor (manager_test.go) cannot tell instance rows apart: every row of a
// spec's instances carries the BASE spec ID.
func instanceStatus(m *Manager, specID string, k int) *Status {
	for _, s := range m.Status() {
		if s.SpecID == specID && s.Instance == k {
			s := s
			return &s
		}
	}
	return nil
}

// mustInstance is instanceStatus for a row the test's premise says exists: it
// fails the test with a message, rather than panicking, when the row is missing.
func mustInstance(t *testing.T, m *Manager, specID string, k int) Status {
	t.Helper()
	st := instanceStatus(m, specID, k)
	if st == nil {
		t.Fatalf("no status row for instance %d of %s; the test's premise (it exists) does not hold", k, specID)
	}
	return *st
}

// ensureModel is EnsureRunning with a bounded context, failing the test on an
// error. The release it returns must still be called by the test.
func ensureModel(t *testing.T, m *Manager, model string) (endpoint string, release func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	endpoint, release, err := m.EnsureRunning(ctx, model)
	if err != nil {
		t.Fatalf("EnsureRunning(%q): %v", model, err)
	}
	return endpoint, release
}

// ensureErr is EnsureRunning expected to fail; it returns how long the call
// took (a burst refusal must be immediate, never a queued wait) and the error.
func ensureErr(t *testing.T, m *Manager, model string) (time.Duration, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	_, release, err := m.EnsureRunning(ctx, model)
	if err == nil {
		release()
		t.Fatalf("EnsureRunning(%q) succeeded, want an error", model)
	}
	return time.Since(start), err
}

// setDrainGrace widens drainGrace AFTER shrinkTimings (whose cleanup restores
// the original) and BEFORE the Manager exists, so an in-flight request can be
// observed finishing on a draining instance instead of being cut off at the
// shrunk 200ms grace.
func setDrainGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := drainGrace
	drainGrace = d
	t.Cleanup(func() { drainGrace = orig })
}

// setIdleTick overrides idleTickInterval the same way (before the Manager).
func setIdleTick(t *testing.T, d time.Duration) {
	t.Helper()
	orig := idleTickInterval
	idleTickInterval = d
	t.Cleanup(func() { idleTickInterval = orig })
}

func TestInstancesMinStartedOnApply(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}})

	waitUntil(t, 5*time.Second, "instances 0 and 1 running after Apply alone (min_instances=2 is the protected floor)", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})

	rows := m.Status()
	if len(rows) != 2 {
		t.Fatalf("Status() has %d rows, want 2 (one per instance): %+v", len(rows), rows)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Instance < rows[j].Instance })
	for i, want := range []struct {
		instance int
		model    string
	}{{0, "m"}, {1, "m#1"}} {
		if rows[i].SpecID != "S" || rows[i].Instance != want.instance || rows[i].Model != want.model {
			t.Errorf("row %d = {SpecID:%q Instance:%d Model:%q}, want {S %d %q}: the spec id stays the BASE id, the model is the instance address",
				i, rows[i].SpecID, rows[i].Instance, rows[i].Model, want.instance, want.model)
		}
	}
	if rows[0].PID == rows[1].PID {
		t.Errorf("both instances report pid %d, want two processes", rows[0].PID)
	}
}

func TestEnsureCreatesBurstInstanceOnDemand(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})

	endpoint, release := ensureModel(t, m, "m#2")
	defer release()
	if code, body := httpEcho(t, endpoint, "hi"); code != 200 || body != "hi" {
		t.Fatalf("echo through instance 2 = (%d, %q), want (200, hi)", code, body)
	}

	st := instanceStatus(m, "S", 2)
	if st == nil || st.State != StateRunning || st.Model != "m#2" {
		t.Fatalf("instance 2 row = %+v, want running with model m#2", st)
	}
	if instanceStatus(m, "S", 1) != nil {
		t.Errorf("an instance-1 row exists; only the named instance is created (gaps are allowed)")
	}
	if s0 := instanceStatus(m, "S", 0); s0 == nil || s0.State != StateStopped {
		t.Errorf("instance 0 row = %+v, want it to exist and stay stopped (nothing asked for it)", s0)
	}
	if loaded := m.LoadedModels(); !contains(loaded, "m#2") || contains(loaded, "m") {
		t.Errorf("LoadedModels() = %v, want the running instance address m#2 and not the stopped m", loaded)
	}
}

func TestEnsureDisallowedInstanceIsNotManaged(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	capped := replicableSpec("S", "m")
	capped.MaxInstances = 2
	m.Apply(Config{Specs: []Spec{baseSpec("P", "p"), capped}})

	for _, model := range []string{"p#1", "m#2", "m#64", "m#0", "nope#1"} {
		if _, err := ensureErr(t, m, model); !errors.Is(err, ErrModelNotManaged) {
			t.Errorf("EnsureRunning(%q) = %v, want ErrModelNotManaged", model, err)
		}
	}
	if rows := m.Status(); len(rows) != 2 {
		t.Errorf("Status() = %+v, want only the two instance-0 rows: a refused address must not create state", rows)
	}

	// The cap counts instance 0, so max_instances=2 still allows instance 1.
	_, release := ensureModel(t, m, "m#1")
	release()
}

func TestBurstInstanceRefusedWhenNoFreeRoom(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	s := replicableSpec("S", "m")
	s.GPUs = []SpecGPU{{Index: 0, VRAMMB: 6000}}
	tt := baseSpec("T", "t")
	tt.GPUs = []SpecGPU{{Index: 0, VRAMMB: 3000}}
	// S + T fit; a second S only fits by evicting the idle T -- which a burst
	// instance must never do.
	m.Apply(Config{
		Specs:      []Spec{s, tt},
		GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 13000}},
		Coresident: [][2]string{{"S", "T"}},
	})

	_, relS := ensureModel(t, m, "m")
	relS()
	_, relT := ensureModel(t, m, "t")
	relT()
	stT := statusFor(m, "T")
	if stT == nil {
		t.Fatal("no status row for T after it served a request")
	}
	pidT := stT.PID

	took, err := ensureErr(t, m, "m#1")
	if !errors.Is(err, ErrInstanceNoCapacity) {
		t.Fatalf("EnsureRunning(m#1) = %v, want ErrInstanceNoCapacity (the only fit evicts T)", err)
	}
	if took > 2*time.Second {
		t.Errorf("the refusal took %s; a burst refusal must be immediate, never a queued wait", took)
	}
	if st := statusFor(m, "T"); st == nil || st.State != StateRunning || st.PID != pidT {
		t.Errorf("T = %+v, want still running as pid %d: a burst instance never evicts a neighbour", st, pidT)
	}
	if st := instanceStatus(m, "S", 1); st != nil {
		t.Errorf("instance-1 row = %+v, want none: a refused burst instance leaves no state behind", st)
	}
}

func TestBurstInstanceDemandUnknownRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		gpus  []SpecGPU
		ramMB int
	}{
		{"vram and ram unknown", []SpecGPU{{Index: 0, VRAMMB: 0}}, 0},
		{"ram unknown, no gpu", nil, 0},
		{"vram unknown, ram known", []SpecGPU{{Index: 0, VRAMMB: 0}}, 64},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipOnWindows(t)
			shrinkTimings(t)
			m := newTestManager(t, allowlistPolicy())

			spec := replicableSpec("S", "m")
			spec.GPUs = tc.gpus
			spec.RAMMB = tc.ramMB
			m.Apply(Config{Specs: []Spec{spec}})

			// Instance 0 keeps today's unknown-demand behaviour: it starts.
			_, release := ensureModel(t, m, "m")
			defer release()

			took, err := ensureErr(t, m, "m#1")
			if !errors.Is(err, ErrInstanceDemandUnknown) {
				t.Fatalf("EnsureRunning(m#1) = %v, want ErrInstanceDemandUnknown (no declared demand, no measured sibling)", err)
			}
			if took > 2*time.Second {
				t.Errorf("the refusal took %s, want it immediate", took)
			}
			if st := instanceStatus(m, "S", 1); st != nil {
				t.Errorf("instance-1 row = %+v, want none", st)
			}
		})
	}
}

func TestBurstInheritsSiblingMeasurement(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		out := make(map[int]map[int]int, len(pids))
		for _, p := range pids {
			out[p] = map[int]int{0: 3000}
		}
		return out
	})

	spec := replicableSpec("S", "m")
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}} // the operator left the estimate at 0
	budget := func(etag string, mb int) Config {
		return Config{ETag: etag, Specs: []Spec{spec}, GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: mb}}}
	}
	m.Apply(budget("v1", 6000))

	_, rel0 := ensureModel(t, m, "m")
	defer rel0()
	waitUntil(t, 5*time.Second, "instance 0 measured at 3000 MB", func() bool {
		st := instanceStatus(m, "S", 0)
		return st != nil && st.MeasuredVRAM[0] == 3000
	})

	// The inherited charge is exactly 3000 -- pinned from both sides. One MB
	// short of 3000 + 3000 refuses it, so it is charged at least 3000 ...
	m.Apply(budget("v2", 5999))
	if _, err := ensureErr(t, m, "m#1"); !errors.Is(err, ErrInstanceNoCapacity) {
		t.Fatalf("EnsureRunning(m#1) under a 5999 MB budget = %v, want ErrInstanceNoCapacity (3000 for S + the inherited 3000 does not fit)", err)
	}
	// ... and 3000 + 3000 = 6000 fits the 6000 budget exactly, so it is charged
	// at most 3000.
	m.Apply(budget("v3", 6000))
	_, rel1 := ensureModel(t, m, "m#1")
	defer rel1()

	// A third copy is charged the same inherited 3000: 9000 does not fit, and
	// a sibling is never a victim, so the burst instance is refused.
	if _, err := ensureErr(t, m, "m#2"); !errors.Is(err, ErrInstanceNoCapacity) {
		t.Fatalf("EnsureRunning(m#2) = %v, want ErrInstanceNoCapacity (charged the inherited 3000 next to two 3000s)", err)
	}
}

func TestProtectedInstanceWaitsForSiblingMeasurement(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setIdleTick(t, 40*time.Millisecond)
	m := newTestManager(t, allowlistPolicy())

	var answer atomic.Bool
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		if !answer.Load() {
			return nil
		}
		out := make(map[int]map[int]int, len(pids))
		for _, p := range pids {
			out[p] = map[int]int{0: 3000}
		}
		return out
	})

	spec := replicableSpec("S", "m")
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}}
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}})

	waitUntil(t, 5*time.Second, "instance 0 running and instance 1 waiting with a reason", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning &&
			s1 != nil && s1.LastError != nil && strings.Contains(s1.LastError.Message, "unknown")
	})
	if s1 := instanceStatus(m, "S", 1); s1 == nil || s1.State != StateStopped || s1.PID != 0 {
		t.Fatalf("instance 1 = %+v, want stopped with no process: a baseline instance waits for a measured sibling, never starts blind", s1)
	}

	answer.Store(true)
	waitUntil(t, 5*time.Second, "instance 1 started once its sibling was measured", func() bool {
		s1 := instanceStatus(m, "S", 1)
		return s1 != nil && s1.State == StateRunning
	})
}

// TestFurtherInstanceWaitsForASiblingThatIsReady: while instance 0 is still
// loading, every measurement of it -- the housekeeping beat's, and each
// admission's own synchronous one -- catches the load in progress, a fraction of
// what the loaded model holds. A further instance with an unknown demand must not
// inherit that: it would count as known, and the instance would start against
// the fraction (two copies of a model that settles at 6000 MB, admitted at
// 1200 each onto an 8000 MB GPU). It inherits only a figure taken once its
// sibling passed the health probe (§5.2, §5.4 rule 5): until then a protected
// instance waits and a burst one is refused as demand-unknown; afterwards both
// start on the loaded figure.
//
// "On the loaded figure" is pinned, not assumed: once instance 0 is ready, the
// room is first made too small for a second LOADED copy but large enough for
// one at the mid-load figure, and instance 1 must still not start (a protected
// one waits, a burst one is refused for capacity). Only when the room is
// widened to hold the loaded figure does it start.
func TestFurtherInstanceWaitsForASiblingThatIsReady(t *testing.T) {
	const loadingMB, loadedMB = 1200, 3000
	// The room once instance 0 holds its loaded 3000: tight fits a second copy
	// at 1200 (the mid-load reading) but not at 3000; roomy fits 3000.
	const tightVRAM, roomyVRAM = 5000, 8000
	const tightRAMAvailable, roomyRAMAvailable = 2000, 4000
	for _, tc := range []struct {
		name  string
		vram  bool // the unknown demand is per-GPU VRAM (else host RAM)
		burst bool // min_instances 1, so instance 1 is burst (else 2: protected)
	}{
		{"protected, vram unknown", true, false},
		{"protected, ram unknown", false, false},
		{"burst, vram unknown", true, true},
		{"burst, ram unknown", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipOnWindows(t)
			shrinkTimings(t)
			m := newTestManager(t, allowlistPolicy())

			var loaded atomic.Bool
			var ramCalls atomic.Int64
			reading := func() int {
				if loaded.Load() {
					return loadedMB
				}
				return loadingMB
			}
			spec, openGate := gatedSpec(t, "S", "m") // every instance shares the gate
			spec.Replicable = true
			spec.MinInstances = 2
			if tc.burst {
				spec.MinInstances = 1
			}
			cfg := Config{ETag: "tight"}
			if tc.vram {
				spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}}
				spec.RAMMB = 64
				cfg.GPUBudgets = []GPUBudget{{Index: 0, BudgetMB: tightVRAM}}
				m.SetMeasurer(func(pids []int) map[int]map[int]int {
					out := make(map[int]map[int]int, len(pids))
					for _, p := range pids {
						out[p] = map[int]int{0: reading()}
					}
					return out
				})
			} else {
				spec.RAMMB = 0
				m.SetHostInfo(HostInfo{MemAvailableMB: tightRAMAvailable})
				m.SetRAMMeasurer(func(pids []int) map[int]int {
					ramCalls.Add(1)
					out := make(map[int]int, len(pids))
					for _, p := range pids {
						out[p] = reading()
					}
					return out
				})
			}
			cfg.Specs = []Spec{spec}
			m.Apply(cfg)

			waitUntil(t, 5*time.Second, "instance 0 loading, with a mid-load measurement of it recorded", func() bool {
				st := instanceStatus(m, "S", 0)
				if st == nil || st.State != StateStarting {
					return false
				}
				if tc.vram {
					return st.MeasuredVRAM[0] == loadingMB
				}
				return st.MeasuredRAMMB == loadingMB
			})
			if tc.burst {
				took, err := ensureErr(t, m, "m#1")
				if !errors.Is(err, ErrInstanceDemandUnknown) {
					t.Fatalf("EnsureRunning(m#1) while instance 0 loads = %v, want ErrInstanceDemandUnknown: a mid-load reading is no demand", err)
				}
				if took > 2*time.Second {
					t.Errorf("the refusal took %s, want it immediate", took)
				}
			} else {
				// Ten housekeeping beats (30ms each) land while instance 0 loads; not
				// one of their readings may start instance 1.
				deadline := time.Now().Add(300 * time.Millisecond)
				for time.Now().Before(deadline) {
					if s1 := instanceStatus(m, "S", 1); s1 == nil || s1.State != StateStopped || s1.PID != 0 {
						t.Fatalf("instance 1 = %+v while instance 0 is still loading, want it stopped and waiting: it would start against a mid-load reading", s1)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}

			loaded.Store(true)
			openGate()
			waitUntil(t, 5*time.Second, "instance 0 ready and measured at its loaded figure", func() bool {
				st := instanceStatus(m, "S", 0)
				if st == nil || st.State != StateRunning {
					return false
				}
				if tc.vram {
					return st.MeasuredVRAM[0] == loadedMB
				}
				return st.MeasuredRAMMB == loadedMB
			})
			if !tc.vram {
				// A RAM reading is inheritable only when it was dispatched after the
				// probe passed, and the one that set MeasuredRAMMB may have been
				// dispatched just before. Measurements go out one at a time, so the
				// second call from now was dispatched after the first one returned
				// -- after instance 0 was ready -- and the third goes out only once
				// that second one has landed.
				from := ramCalls.Load()
				waitUntil(t, 5*time.Second, "a RAM reading taken once instance 0 was ready has landed", func() bool {
					return ramCalls.Load() >= from+3
				})
			}

			// The room holds a copy at the mid-load figure but not at the loaded
			// one: instance 1 must not start.
			if tc.burst {
				if _, err := ensureErr(t, m, "m#1"); !errors.Is(err, ErrInstanceNoCapacity) {
					t.Fatalf("EnsureRunning(m#1) next to a ready instance 0 = %v, want ErrInstanceNoCapacity: charged the loaded %d MB, a second copy does not fit (at the mid-load %d MB it would)", err, loadedMB, loadingMB)
				}
			} else {
				// Ten housekeeping beats, every one of them a chance to re-ask it.
				deadline := time.Now().Add(300 * time.Millisecond)
				for time.Now().Before(deadline) {
					if s1 := instanceStatus(m, "S", 1); s1 == nil || s1.State != StateStopped || s1.PID != 0 {
						t.Fatalf("instance 1 = %+v next to a ready instance 0, want it stopped: a second copy at the loaded %d MB does not fit (at the mid-load %d MB it would)", s1, loadedMB, loadingMB)
					}
					time.Sleep(5 * time.Millisecond)
				}
			}

			// Widened to hold the loaded figure: instance 1 starts.
			if tc.vram {
				cfg.ETag = "roomy"
				cfg.GPUBudgets = []GPUBudget{{Index: 0, BudgetMB: roomyVRAM}}
				m.Apply(cfg)
			} else {
				m.SetHostInfo(HostInfo{MemAvailableMB: roomyRAMAvailable})
			}
			if tc.burst {
				waitUntil(t, 5*time.Second, "the burst instance admitted once the loaded figure fits", func() bool {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, release, err := m.EnsureRunning(ctx, "m#1")
					if err != nil {
						return false
					}
					release()
					return true
				})
				return
			}
			waitUntil(t, 5*time.Second, "instance 1 started once the loaded figure fits", func() bool {
				s1 := instanceStatus(m, "S", 1)
				return s1 != nil && s1.State == StateRunning
			})
		})
	}
}

func TestIdleUnloadHighestIndexFirst(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	// A stubborn child holds every drain open for the kill grace, so the window
	// in which instance 2 is draining is wide enough to look at the others.
	setKillGrace(t, 300*time.Millisecond)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.Args = stubArgsStubborn(0)
	spec.IdleTimeoutSeconds = 1
	m.Apply(Config{Specs: []Spec{spec}})

	for _, model := range []string{"m", "m#1", "m#2"} {
		_, release := ensureModel(t, m, model)
		waitUntilChildIsServingInstance(t, m, "S", instanceOf(model))
		release()
	}

	var (
		violation     string
		sawTopDrained bool
	)
	waitUntil(t, 15*time.Second, "every instance idle-unloaded, highest index first", func() bool {
		byK := map[int]Status{}
		for _, r := range m.Status() {
			byK[r.Instance] = r
		}
		s0, has0 := byK[0]
		s1, has1 := byK[1]
		s2, has2 := byK[2]
		if has2 && s2.State == StateDraining {
			sawTopDrained = true
			if s1.State != StateRunning || s0.State != StateRunning {
				violation = "instance 2 is draining but instance 1 or 0 is not running: one scan drained more than the top instance"
			}
		}
		if has1 && s1.State != StateRunning && has2 && violation == "" {
			violation = "instance 1 stopped running while instance 2 still exists: a lower instance was unloaded before a higher one"
		}
		if has0 && s0.State != StateRunning && (has1 || has2) && violation == "" {
			violation = "instance 0 stopped running while a higher instance still exists: instance 0 must be the last to go"
		}
		return violation != "" || (has0 && s0.State == StateStopped && !has1 && !has2)
	})
	if violation != "" {
		t.Fatal(violation)
	}
	if !sawTopDrained {
		t.Fatal("never observed instance 2 draining on its own")
	}
}

// TestIdleUnloadKeepsProtected: an instance inside the min_instances floor is
// never idle-unloaded, however long it sits idle (§5.5: only k >= P unloads).
//
// The positive control is an UNRELATED spec on the same idle timeout, used
// after every protected instance, so its unload proves the scans ran past
// their idle windows too. It must not be a burst instance of the same spec: a
// running higher-index sibling shields every lower instance from idle unload
// (the ordering invariant), so it would keep instance 0 up whatever the
// min_instances guard said. The protected instances are then watched through a
// further whole idle period. min=2 covers a protected instance k >= 1, the one
// a broken guard reaches first (highest index leaves first).
func TestIdleUnloadKeepsProtected(t *testing.T) {
	for _, minInstances := range []int{1, 2} {
		t.Run(fmt.Sprintf("min_%d", minInstances), func(t *testing.T) {
			skipOnWindows(t)
			shrinkTimings(t)
			m := newTestManager(t, allowlistPolicy())

			spec := replicableSpec("S", "m")
			spec.MinInstances = minInstances
			spec.IdleTimeoutSeconds = 1
			control := baseSpec("U", "u")
			control.IdleTimeoutSeconds = 1
			m.Apply(Config{Specs: []Spec{spec, control}, Coresident: [][2]string{{"S", "U"}}})

			waitUntil(t, 5*time.Second, "every protected instance started on Apply", func() bool {
				for k := 0; k < minInstances; k++ {
					if st := instanceStatus(m, "S", k); st == nil || st.State != StateRunning {
						return false
					}
				}
				return true
			})
			pids := make([]int, minInstances)
			for k := range pids {
				_, release := ensureModel(t, m, InstanceAddress("m", k)) // a real last use, before the control's
				release()
				pids[k] = mustInstance(t, m, "S", k).PID
			}

			_, release := ensureModel(t, m, "u")
			release()
			waitUntil(t, 5*time.Second, "the unrelated control idle-unloaded on the same timeout", func() bool {
				st := statusFor(m, "U")
				return st != nil && st.State == StateStopped && st.PID == 0
			})

			// One more whole idle timeout (1s) plus ten idle scans (30ms each).
			hold := time.Now().Add(1300 * time.Millisecond)
			for {
				for k, pid := range pids {
					if st := instanceStatus(m, "S", k); st == nil || st.State != StateRunning || st.PID != pid {
						t.Fatalf("protected instance %d = %+v, want still running as pid %d: min_instances=%d protects it from idle unload",
							k, st, pid, minInstances)
					}
				}
				if time.Now().After(hold) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestBurstExitRetiresInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.IdleTimeoutSeconds = 1
	m.Apply(Config{Specs: []Spec{spec}})

	_, release := ensureModel(t, m, "m#1")
	release()
	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateRunning {
		t.Fatalf("instance 1 = %+v, want running", st)
	}
	waitUntil(t, 5*time.Second, "instance 1 retired after its idle unload", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
	if st := instanceStatus(m, "S", 0); st == nil {
		t.Fatal("the instance-0 row disappeared; instance 0 always exists")
	}
}

func TestBurstCrashKeptUntilNextApply(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	// Crashes well after it is healthy (the stub needs ~100ms to answer), so the
	// request that started it is served first.
	spec.Args = stubArgs(0, 600*time.Millisecond, 3, "")
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	_, release := ensureModel(t, m, "m#1")
	release()
	waitUntil(t, 5*time.Second, "instance 1 crashed", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateCrashed
	})
	// A burst instance is never retried: no backoff, no restart, the row
	// simply stays crashed for the operator. Observed across many backoff
	// periods (backoffBase is 60ms here).
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		st := instanceStatus(m, "S", 1)
		if st == nil || st.State != StateCrashed || st.PID != 0 {
			t.Fatalf("instance 1 = %+v, want it to stay crashed with no process (burst instances are not restarted)", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := instanceStatus(m, "S", 1); st == nil || st.LastError == nil {
		t.Errorf("crashed row = %+v, want it kept with a LastError: it is kept precisely so the operator can read why", st)
	}

	// Named again: the crashed row is retired and a fresh instance starts.
	_, release = ensureModel(t, m, "m#1")
	release()
	waitUntil(t, 5*time.Second, "instance 1 crashed again", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateCrashed
	})

	// The next Apply retires it.
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v2"})
	if st := instanceStatus(m, "S", 1); st != nil {
		t.Fatalf("instance 1 = %+v after the next Apply, want it retired", st)
	}
}

func TestApplyReplicableOffDrainsInstancesGracefully(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	_, rel0 := ensureModel(t, m, "m")
	rel0()
	pid0 := mustInstance(t, m, "S", 0).PID
	ep1, rel1 := ensureModel(t, m, "m#1") // one request in flight on instance 1

	off := spec
	off.Replicable = false
	m.Apply(Config{Specs: []Spec{off}, ETag: "v2"})

	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateDraining {
		t.Fatalf("instance 1 = %+v right after the Apply, want draining (its request is still in flight)", st)
	}
	// The in-flight request can still finish against the draining child.
	if code, body := httpEcho(t, ep1, "still here"); code != 200 || body != "still here" {
		t.Fatalf("echo through the draining instance = (%d, %q), want it served", code, body)
	}
	if _, err := ensureErr(t, m, "m#1"); !errors.Is(err, ErrModelNotManaged) {
		t.Errorf("EnsureRunning(m#1) after replicable was turned off = %v, want ErrModelNotManaged", err)
	}
	rel1()
	waitUntil(t, 5*time.Second, "instance 1 drained and removed", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
	if st := instanceStatus(m, "S", 0); st == nil || st.State != StateRunning || st.PID != pid0 {
		t.Fatalf("instance 0 = %+v, want untouched (running as pid %d)", st, pid0)
	}
}

// TestApplyMaxLoweredDrainsDisallowed: lowering max_instances drains the
// instances the new cap no longer allows, an idle one at once and one serving a
// request only once that request has finished (Review Focus 2: a push never
// cuts a response off); the instances still allowed are untouched.
func TestApplyMaxLoweredDrainsDisallowed(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MaxInstances = 4
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	_, rel1 := ensureModel(t, m, "m#1")
	rel1()
	_, rel2 := ensureModel(t, m, "m#2")
	rel2()
	ep3, rel3 := ensureModel(t, m, "m#3") // one request in flight on instance 3 across the push
	defer rel3()
	pid1 := mustInstance(t, m, "S", 1).PID

	lowered := spec
	lowered.MaxInstances = 2
	m.Apply(Config{Specs: []Spec{lowered}, ETag: "v2"})

	if st := instanceStatus(m, "S", 3); st == nil || st.State != StateDraining {
		t.Fatalf("instance 3 = %+v right after max_instances was lowered, want draining (its request is still in flight)", st)
	}
	if code, body := httpEcho(t, ep3, "still here"); code != 200 || body != "still here" {
		t.Fatalf("echo through the draining instance 3 = (%d, %q), want it served", code, body)
	}
	waitUntil(t, 5*time.Second, "the idle instance 2 drained and removed", func() bool {
		return instanceStatus(m, "S", 2) == nil
	})
	if st := instanceStatus(m, "S", 3); st == nil || st.State != StateDraining {
		t.Fatalf("instance 3 = %+v once instance 2 is gone, want still draining for its request", st)
	}
	for _, model := range []string{"m#2", "m#3"} {
		if _, err := ensureErr(t, m, model); !errors.Is(err, ErrModelNotManaged) {
			t.Errorf("EnsureRunning(%s) = %v, want ErrModelNotManaged under max_instances=2", model, err)
		}
	}

	rel3()
	waitUntil(t, 5*time.Second, "instance 3 drained and removed", func() bool {
		return instanceStatus(m, "S", 3) == nil
	})
	waitUntil(t, 5*time.Second, "instance 3's child gone", func() bool {
		return !httpGetOK(ep3, "/health")
	})
	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateRunning || st.PID != pid1 {
		t.Errorf("instance 1 = %+v, want untouched (still allowed under max 2)", st)
	}
}

// TestApplySpecRemovedDrainsInstancesGracefully: a push without the spec takes
// every instance with it, and an instance serving a request finishes it first
// (Review Focus 2). A removed spec reaches its instances k >= 1 by a route of
// its own -- the removal loop keys by BASE spec, and the further-instances loop
// leaves an instance of an absent spec to it -- so this is the one test that
// drives that route: the instance must drain rather than be cut off, dropped
// from the table while its child keeps running, or left behind.
func TestApplySpecRemovedDrainsInstancesGracefully(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	other := baseSpec("T", "t")
	m.Apply(Config{Specs: []Spec{spec, other}, ETag: "v1"})

	ep0, rel0 := ensureModel(t, m, "m")
	rel0()
	ep1, rel1 := ensureModel(t, m, "m#1") // one request in flight on instance 1 across the push
	defer rel1()

	m.Apply(Config{Specs: []Spec{other}, ETag: "v2"})

	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateDraining {
		t.Fatalf("instance 1 = %+v right after its spec was removed, want draining (its request is still in flight)", st)
	}
	if code, body := httpEcho(t, ep1, "still here"); code != 200 || body != "still here" {
		t.Fatalf("echo through the draining instance 1 = (%d, %q), want it served", code, body)
	}
	waitUntil(t, 5*time.Second, "the idle instance 0 drained and removed", func() bool {
		return instanceStatus(m, "S", 0) == nil
	})
	if st := instanceStatus(m, "S", 1); st == nil || st.State != StateDraining {
		t.Fatalf("instance 1 = %+v once instance 0 is gone, want still draining for its request", st)
	}
	for _, model := range []string{"m", "m#1", "m#2"} {
		if _, err := ensureErr(t, m, model); !errors.Is(err, ErrModelNotManaged) {
			t.Errorf("EnsureRunning(%s) after its spec was removed = %v, want ErrModelNotManaged", model, err)
		}
	}

	rel1()
	waitUntil(t, 5*time.Second, "instance 1 drained and removed", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
	waitUntil(t, 5*time.Second, "both children of the removed spec gone", func() bool {
		return !httpGetOK(ep0, "/health") && !httpGetOK(ep1, "/health")
	})
	if rows := m.Status(); len(rows) != 1 || rows[0].SpecID != "T" {
		t.Fatalf("Status() = %+v, want only T's row", rows)
	}
}

func TestApplyUnchangedSpecDoesNotRestartInstances(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	pid0, pid1 := mustInstance(t, m, "S", 0).PID, mustInstance(t, m, "S", 1).PID

	// An identical push (new ETag) and a metadata-only edit: neither may
	// bounce an instance. A drain would be visible the moment Apply returns.
	meta := spec
	meta.IdleTimeoutSeconds = 30
	for i, s := range []Spec{spec, meta} {
		m.Apply(Config{Specs: []Spec{s}, ETag: "v" + string(rune('2'+i))})
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		if s0 == nil || s1 == nil || s0.State != StateRunning || s0.PID != pid0 || s1.State != StateRunning || s1.PID != pid1 {
			t.Fatalf("push %d: instances = %+v / %+v, want both still running as pids %d/%d", i, s0, s1, pid0, pid1)
		}
	}

	// A launch-shape change restarts every instance.
	relaunch := meta
	relaunch.Args = append(stubArgs(0, 0, 0, ""), "-health-delay", "1ms")
	m.Apply(Config{Specs: []Spec{relaunch}, ETag: "v9"})
	waitUntil(t, 5*time.Second, "both instances relaunched with the new shape", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s0.PID != pid0 &&
			s1 != nil && s1.State == StateRunning && s1.PID != pid1
	})
}

func TestInstanceUsesBaseModelAndOwnConfigDir(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)

	base := t.TempDir()
	readLog := filepath.Join(t.TempDir(), "read")
	spec := replicableSpec("S", "m")
	spec.ConfigFiles = []ConfigFile{{Name: "m.txt", Content: "model=${MODEL};dir=${CONFIG_DIR}"}}
	// ${MODEL} in the path: the file exists only if the child sees the BASE name.
	spec.Args = append(stubArgs(0, 0, 0, ""), "-read-file", "${CONFIG_DIR}/${MODEL}.txt", "-read-log", readLog)

	m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }, ConfigFilesDir: base})
	t.Cleanup(m.Close)
	m.Apply(Config{Specs: []Spec{spec}})

	_, release := ensureModel(t, m, "m#1") // instance 0 never starts, so the read log is instance 1's alone
	defer release()

	instDir := filepath.Join(base, "S#1")
	want := "content:model=m;dir=" + instDir
	if got := readFileLog(t, readLog); got != want {
		t.Fatalf("instance 1's child read %q, want %q (${MODEL} is the base name, ${CONFIG_DIR} the instance's own dir)", got, want)
	}
	if _, err := os.Stat(filepath.Join(base, "S", "m.txt")); !os.IsNotExist(err) {
		t.Errorf("instance 0's config dir was written (stat err %v); instance 1 must write only its own", err)
	}
}

func TestStartupMSReported(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("S", "m") // measured for every spec, replicable or not
	spec.Args = stubArgs(300*time.Millisecond, 0, 0, "")
	m.Apply(Config{Specs: []Spec{spec}})
	if st := statusFor(m, "S"); st == nil || st.StartupMS != 0 {
		t.Fatalf("row before any start = %+v, want StartupMS 0 (unknown)", st)
	}

	_, release := ensureModel(t, m, "m")
	defer release()
	if st := statusFor(m, "S"); st == nil || st.StartupMS < 300 {
		t.Fatalf("row = %+v, want StartupMS >= 300 (the child answers /health only after 300ms)", st)
	}
}

func TestNonReplicableUnchanged(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{baseSpec("S", "m")}})
	_, release := ensureModel(t, m, "m")
	defer release()

	rows := m.Status()
	if len(rows) != 1 {
		t.Fatalf("Status() = %+v, want exactly one row", rows)
	}
	if r := rows[0]; r.SpecID != "S" || r.Instance != 0 || r.Model != "m" || r.State != StateRunning {
		t.Fatalf("row = %+v, want {SpecID:S Instance:0 Model:m running}", r)
	}
	if loaded := m.LoadedModels(); len(loaded) != 1 || loaded[0] != "m" {
		t.Fatalf("LoadedModels() = %v, want [m]", loaded)
	}
}

func TestResolveAddress(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	capped := replicableSpec("C", "c")
	capped.MaxInstances = 2
	m.Apply(Config{Specs: []Spec{
		baseSpec("L", "foo#2"), // a real model whose name merely contains '#'
		replicableSpec("F", "foo"),
		baseSpec("P", "p"),
		capped,
	}})

	for _, tc := range []struct {
		model  string
		base   string
		k      int
		wantOK bool
	}{
		{"foo#2", "foo#2", 0, true}, // the literal name wins over instance 2 of foo
		{"foo#3", "foo", 3, true},
		{"foo", "foo", 0, true},
		{"p", "p", 0, true},
		{"p#1", "", 0, false},   // not replicable
		{"c#1", "c", 1, true},   // the cap counts instance 0
		{"c#2", "", 0, false},   // k >= max_instances
		{"foo#0", "", 0, false}, // not an instance address, and no such model
		{"foo#64", "", 0, false},
		{"nope", "", 0, false},
		{"nope#1", "", 0, false},
	} {
		base, k, ok := m.ResolveAddress(tc.model)
		if ok != tc.wantOK || (ok && (base != tc.base || k != tc.k)) {
			t.Errorf("ResolveAddress(%q) = (%q, %d, %v), want (%q, %d, %v)", tc.model, base, k, ok, tc.base, tc.k, tc.wantOK)
		}
	}

	// EnsureRunning resolves by the same rule: foo#2 is L's process.
	_, release := ensureModel(t, m, "foo#2")
	defer release()
	if st := statusFor(m, "L"); st == nil || st.State != StateRunning {
		t.Fatalf("L = %+v, want running: the literal upstream foo#2 must route to L", st)
	}
	if st := instanceStatus(m, "F", 2); st != nil {
		t.Fatalf("F instance 2 = %+v, want none", st)
	}
}

// assertFooHash2IsL checks that the address foo#2 is spec L's literal model
// and nothing else: it resolves to L, no instance 2 of F exists, no other row
// reports the model foo#2, and it is not listed as loaded while L is stopped.
func assertFooHash2IsL(t *testing.T, m *Manager) {
	t.Helper()
	if base, k, ok := m.ResolveAddress("foo#2"); !ok || base != "foo#2" || k != 0 {
		t.Errorf("ResolveAddress(foo#2) = (%q, %d, %v), want (foo#2, 0, true): L's literal model", base, k, ok)
	}
	if st := instanceStatus(m, "F", 2); st != nil {
		t.Errorf("F instance 2 = %+v, want none: its address foo#2 always routes to L, so nothing could ever reach it", st)
	}
	var rows []Status
	for _, r := range m.Status() {
		if r.Model == "foo#2" {
			rows = append(rows, r)
		}
	}
	if len(rows) != 1 || rows[0].SpecID != "L" {
		t.Errorf("rows reporting the model foo#2 = %+v, want L's alone", rows)
	}
	if loaded := m.LoadedModels(); contains(loaded, "foo#2") {
		t.Errorf("LoadedModels() = %v, want no foo#2 while L is stopped", loaded)
	}
}

// TestProtectedFloorSkipsAnAddressThatIsAnotherSpecsModel: foo#2 is the
// literal upstream model of L, so it routes to L however F (upstream foo)
// replicates (an exact upstream match wins over an instance address) --
// instance 2 of F can never be addressed. F's floor
// (min_instances 3) therefore stops short of that instance instead of starting
// a protected process nobody can reach whose row claims L's model name (in
// LoadedModels, and in the router's /upstream relay, which answers from the
// first running row of that name).
func TestProtectedFloorSkipsAnAddressThatIsAnotherSpecsModel(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	f := replicableSpec("F", "foo")
	f.MinInstances = 3
	m.Apply(Config{Specs: []Spec{baseSpec("L", "foo#2"), f}, ETag: "v1"})

	waitUntil(t, 5*time.Second, "F's reachable protected instances 0 and 1 running", func() bool {
		s0, s1 := instanceStatus(m, "F", 0), instanceStatus(m, "F", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	assertFooHash2IsL(t, m)
	if st := statusFor(m, "L"); st == nil || st.State != StateStopped {
		t.Errorf("L = %+v, want stopped: nothing asked for it", st)
	}
}

// TestApplyDrainsAnInstanceWhoseAddressBecameAnotherSpecsModel: the same
// collision arriving with a push. F's protected instance 2 is serving a request
// when a push adds L, whose literal model is foo#2: from then on that address
// reaches L, so F's instance 2 drains -- its request finishing first -- is
// removed, and the floor does not bring it back on a later push.
func TestApplyDrainsAnInstanceWhoseAddressBecameAnotherSpecsModel(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	f := replicableSpec("F", "foo")
	f.MinInstances = 3
	m.Apply(Config{Specs: []Spec{f}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "F's protected instances 0, 1 and 2 running", func() bool {
		for k := 0; k < 3; k++ {
			if st := instanceStatus(m, "F", k); st == nil || st.State != StateRunning {
				return false
			}
		}
		return true
	})
	pid0, pid1 := mustInstance(t, m, "F", 0).PID, mustInstance(t, m, "F", 1).PID
	ep2, rel2 := ensureModel(t, m, "foo#2") // F's instance 2 while no spec claims the name
	defer rel2()

	withL := Config{Specs: []Spec{baseSpec("L", "foo#2"), f}, ETag: "v2"}
	m.Apply(withL)
	if st := instanceStatus(m, "F", 2); st == nil || st.State != StateDraining {
		t.Fatalf("F instance 2 = %+v right after L claimed foo#2, want draining (its request is still in flight)", st)
	}
	if code, body := httpEcho(t, ep2, "still here"); code != 200 || body != "still here" {
		t.Fatalf("echo through the draining F instance 2 = (%d, %q), want it served", code, body)
	}

	rel2()
	waitUntil(t, 5*time.Second, "F instance 2 drained and removed", func() bool {
		return instanceStatus(m, "F", 2) == nil
	})
	withL.ETag = "v3" // a further push: the floor still stops short of instance 2
	m.Apply(withL)
	assertFooHash2IsL(t, m)
	for k, pid := range []int{pid0, pid1} {
		if st := instanceStatus(m, "F", k); st == nil || st.State != StateRunning || st.PID != pid {
			t.Errorf("F instance %d = %+v, want untouched (running as pid %d)", k, st, pid)
		}
	}
}

// TestFirstHealthyProbeMeasuresEvenWhileAMeasurementIsInFlight: a start that
// becomes healthy while another measurement is still out is measured as soon as
// that one returns, not a whole housekeeping beat later -- the sibling demand a
// burst instance inherits must become known within seconds (§5.2).
func TestFirstHealthyProbeMeasuresEvenWhileAMeasurementIsInFlight(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setIdleTick(t, time.Hour) // no housekeeping beat: only start-triggered dispatches
	m := newTestManager(t, allowlistPolicy())

	entered := make(chan struct{})
	gate := make(chan struct{})
	var enterOnce, gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	t.Cleanup(openGate)
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		out := make(map[int]map[int]int, len(pids))
		for _, p := range pids {
			out[p] = map[int]int{0: 1000}
		}
		if len(pids) > 0 {
			first := false
			enterOnce.Do(func() { first = true; close(entered) })
			if first {
				<-gate // the first live measurement stays out until released
			}
		}
		return out
	})

	a := baseSpec("A", "a")
	a.GPUs = []SpecGPU{{Index: 0, VRAMMB: 500}}
	b := baseSpec("B", "b")
	b.GPUs = []SpecGPU{{Index: 0, VRAMMB: 500}}
	m.Apply(Config{Specs: []Spec{a, b}, Coresident: [][2]string{{"A", "B"}}})

	_, relA := ensureModel(t, m, "a")
	defer relA()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("A's first healthy probe dispatched no measurement")
	}
	_, relB := ensureModel(t, m, "b") // B becomes healthy while A's measurement is out
	defer relB()
	openGate()

	waitUntil(t, 5*time.Second, "B measured right after the in-flight measurement returned", func() bool {
		st := statusFor(m, "B")
		return st != nil && st.MeasuredVRAM[0] == 1000
	})
}

// instanceOf returns the instance index of a test model address.
func instanceOf(model string) int {
	_, k := ParseInstanceAddress(model)
	return k
}

// waitUntilChildIsServingInstance is waitUntilChildIsServing for one instance
// row: it blocks until the child answers /health at all, which in stubchild is
// strictly after it installed its SIGTERM disposition.
func waitUntilChildIsServingInstance(t *testing.T, m *Manager, specID string, k int) {
	t.Helper()
	waitUntil(t, 5*time.Second, "instance child is listening", func() bool {
		st := instanceStatus(m, specID, k)
		return st != nil && st.Port != 0 && httpGetOK(endpointFor(st.Port), "/health")
	})
}

// ---------------------------------------------------------------------------
// Owner-level tests: no goroutine, no child process. A generation whose exited
// channel is already closed makes beginDrain a pure state transition (terminateNow
// returns at once), so a victim choice can be read off the state it leaves.
// ---------------------------------------------------------------------------

// liveInstance builds one live, Running instance state of base for an
// owner-level test. Running means its process has passed a health probe
// (everHealthy), so a reading taken of it is a ready one a sibling inherits; a
// test about a process still loading sets StateStarting and clears everHealthy.
func liveInstance(base Spec, k int, pid int) *specState {
	exited := make(chan struct{})
	close(exited)
	spec := specForInstance(base, k)
	return &specState{
		spec:     spec,
		instance: k,
		state:    StateRunning,
		proc:     &runningProc{specID: spec.ID, pid: pid, exited: exited, everHealthy: true},
	}
}

func instanceOwner(states ...*specState) *owner {
	o := &owner{m: &Manager{}, specs: map[string]*specState{}}
	for _, st := range states {
		o.specs[st.spec.ID] = st
	}
	return o
}

func TestDemandSpecInheritsLargestSiblingMeasurement(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, GPUs: []SpecGPU{{Index: 0}, {Index: 1, VRAMMB: 5000}}}
	s0 := liveInstance(s, 0, 100)
	s0.measuredVRAM = map[int]int{0: 3000, 1: 4000}
	s0.readyVRAM, s0.measuredRAM, s0.readyRAM = s0.measuredVRAM, 2000, 2000
	s1 := liveInstance(s, 1, 101)
	s1.measuredVRAM = map[int]int{0: 3500}
	s1.readyVRAM, s1.measuredRAM, s1.readyRAM = s1.measuredVRAM, 1500, 1500
	s2 := &specState{spec: specForInstance(s, 2), instance: 2, state: StateStopped}
	other := liveInstance(Spec{ID: "T", UpstreamModel: "t", GPUs: []SpecGPU{{Index: 0}}}, 0, 200)
	other.measuredVRAM = map[int]int{0: 9000}
	other.readyVRAM, other.measuredRAM, other.readyRAM = other.measuredVRAM, 9000, 9000
	o := instanceOwner(s0, s1, s2, other)

	d := o.demandSpec(s2)
	if d.GPUs[0].VRAMMB != 3500 || d.GPUs[1].VRAMMB != 5000 || d.RAMMB != 2000 {
		t.Errorf("demand of instance 2 = gpus %+v ram %d, want gpu0 3500 (largest sibling), gpu1 5000 (declared floor wins), ram 2000", d.GPUs, d.RAMMB)
	}
	if s.GPUs[0].VRAMMB != 0 {
		t.Errorf("demandSpec wrote through to the shared GPU slice: %+v", s.GPUs)
	}
	// Instance 0 inherits too, never from itself.
	if d := o.demandSpec(s0); d.GPUs[0].VRAMMB != 3500 || d.RAMMB != 1500 {
		t.Errorf("demand of instance 0 = gpus %+v ram %d, want gpu0 3500 and ram 1500 (instance 1's figures)", d.GPUs, d.RAMMB)
	}
	// A spec that never replicates is exactly its declared spec.
	plain := liveInstance(Spec{ID: "P", UpstreamModel: "p", GPUs: []SpecGPU{{Index: 0}}}, 0, 300)
	o2 := instanceOwner(plain, other)
	if d := o2.demandSpec(plain); d.GPUs[0].VRAMMB != 0 || d.RAMMB != 0 {
		t.Errorf("demand of a non-replicable spec = %+v, want its declared (unknown) demand untouched", d)
	}
}

func TestBuildSnapshotCarriesInstanceFields(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, MinInstances: 2, GPUs: []SpecGPU{{Index: 0}}}
	s0 := liveInstance(s, 0, 100)
	s1 := liveInstance(s, 1, 101)
	s2 := liveInstance(s, 2, 102)
	plain := liveInstance(Spec{ID: "P", UpstreamModel: "p", Pinned: true}, 0, 200)
	o := instanceOwner(s0, s1, s2, plain)
	o.m.SetMeasurer(func(pids []int) map[int]map[int]int {
		// Only instance 0 has been measured; 1 and 2 inherit its figure.
		return map[int]map[int]int{100: {0: 3000}}
	})

	snap := o.buildSnapshot()
	got := map[string]RunningProc{}
	for _, r := range snap.Running {
		got[r.SpecID] = r
	}
	for _, want := range []RunningProc{
		{SpecID: "S", Spec: "S", Instance: 0, Pinned: true, Surplus: false},
		{SpecID: "S#1", Spec: "S", Instance: 1, Pinned: true, Surplus: false},
		{SpecID: "S#2", Spec: "S", Instance: 2, Pinned: false, Surplus: true},
		{SpecID: "P", Spec: "P", Instance: 0, Pinned: true, Surplus: false},
	} {
		r, ok := got[want.SpecID]
		if !ok {
			t.Fatalf("no RunningProc for %s", want.SpecID)
		}
		if r.Spec != want.Spec || r.Instance != want.Instance || r.Pinned != want.Pinned || r.Surplus != want.Surplus {
			t.Errorf("%s = {Spec:%q Instance:%d Pinned:%v Surplus:%v}, want {%q %d %v %v}",
				want.SpecID, r.Spec, r.Instance, r.Pinned, r.Surplus, want.Spec, want.Instance, want.Pinned, want.Surplus)
		}
	}
	if c := gpusChargedFor(t, snap, "S#2"); c[0] != 3000 {
		t.Errorf("instance 2 charged %v, want gpu0 3000 (its sibling's live measurement; its own is unknown)", c)
	}
}

// TestInstanceZeroInheritsProtectedSiblingMeasurement is the restart case:
// instance 0 of an unknown-demand spec comes back next to a protected (and so
// pinned) measured sibling. Charged its own declared 0 it would hit the
// pinned-unknown terminal; charged the sibling's measurement it simply starts.
func TestInstanceZeroInheritsProtectedSiblingMeasurement(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, MinInstances: 2, GPUs: []SpecGPU{{Index: 0}}}
	s0 := &specState{spec: specForInstance(s, 0), instance: 0, state: StateCrashed}
	s1 := liveInstance(s, 1, 101)
	o := instanceOwner(s0, s1)
	o.m.SetMeasurer(func(pids []int) map[int]map[int]int {
		return map[int]map[int]int{101: {0: 3000}}
	})

	snap := o.buildSnapshot()
	if dec := Admit(snap, s0.spec); dec.Reason != StatePendingVRAMUnknown {
		t.Fatalf("premise: the declared spec gives %+v, want the pinned-unknown terminal", dec)
	}
	if dec := Admit(snap, o.demandSpec(s0)); !dec.OK {
		t.Fatalf("Admit(instance 0 with inherited demand) = %+v, want OK", dec)
	}
}

// TestSiblingInheritanceIgnoresALoadInProgress: buildSnapshot measures every
// live process, a starting one included, and that reading rightly charges the
// loading process itself. But it is a fraction of what the model holds once
// loaded, so a further instance must not inherit it: its demand would count as
// known, and Admit would let it start against the fraction (§5.4 rule 5 waits
// for a measured running sibling). Once the sibling is ready, a reading counts.
func TestSiblingInheritanceIgnoresALoadInProgress(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, MinInstances: 1, RAMMB: 64, GPUs: []SpecGPU{{Index: 0}}}
	s0 := liveInstance(s, 0, 100)
	s0.state = StateStarting
	s0.proc.everHealthy = false
	s1 := &specState{spec: specForInstance(s, 1), instance: 1, state: StateStopped}
	o := instanceOwner(s0, s1)
	o.cfg = Config{GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 8000}}}
	var reading atomic.Int64
	reading.Store(1200)
	o.m.SetMeasurer(func([]int) map[int]map[int]int {
		return map[int]map[int]int{100: {0: int(reading.Load())}}
	})

	snap := o.buildSnapshot()
	if c := gpusChargedFor(t, snap, "S"); c[0] != 1200 {
		t.Fatalf("premise: the loading instance 0 is charged %v, want its own mid-load reading 1200", c)
	}
	if d := o.demandSpec(s1); !demandUnknown(d) {
		snap.NoEvict = true
		t.Fatalf("instance 1 inherits gpus %+v from a sibling still loading (Admit with NoEvict: %+v); want its demand unknown until the sibling is ready",
			d.GPUs, Admit(snap, d))
	}

	s0.state = StateRunning
	s0.proc.everHealthy = true
	reading.Store(6000)
	o.buildSnapshot()
	if d := o.demandSpec(s1); d.GPUs[0].VRAMMB != 6000 {
		t.Fatalf("instance 1 demand once its sibling is ready = %+v, want gpu0 6000 (the loaded reading)", d.GPUs)
	}
}

// TestSiblingInheritanceNeedsAMeasurementTakenOnceReady: what makes a
// sibling's reading inheritable is WHEN it was taken, not the sibling's state
// by the time it lands. A housekeeping measurement dispatched while instance 0
// was still loading can land after its first passing probe, and is still the
// load in progress; only one dispatched once the process was ready counts --
// such as the one its first healthy probe asks for (§5.2). buildSnapshot never
// reads host RAM itself, so for RAM this is the only path there is.
func TestSiblingInheritanceNeedsAMeasurementTakenOnceReady(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, MinInstances: 2, GPUs: []SpecGPU{{Index: 0}}}
	s0 := liveInstance(s, 0, 100)
	s0.state = StateStarting
	s0.proc.everHealthy = false
	s1 := &specState{spec: specForInstance(s, 1), instance: 1, state: StateStopped}
	o := instanceOwner(s0, s1)
	o.m.cmds = make(chan command, 1) // the dispatched measurement posts its answer here
	o.m.done = make(chan struct{})
	var reading atomic.Int64
	reading.Store(1200)
	o.m.SetMeasurer(func([]int) map[int]map[int]int {
		return map[int]map[int]int{100: {0: int(reading.Load())}}
	})
	o.m.SetRAMMeasurer(func([]int) map[int]int {
		return map[int]int{100: int(reading.Load())}
	})
	land := func() {
		t.Helper()
		select {
		case c := <-o.m.cmds:
			o.applyMeasurement(c.(cmdMeasured))
		case <-time.After(5 * time.Second):
			t.Fatal("the dispatched measurement never came back")
		}
	}

	o.dispatchMeasurement() // taken while instance 0 is still loading ...
	s0.state = StateRunning // ... and landing after its first passing probe
	s0.proc.everHealthy = true
	land()
	if s0.measuredVRAM[0] != 1200 || s0.measuredRAM != 1200 {
		t.Fatalf("premise: instance 0's own figures = vram %v, ram %d, want the reading recorded (1200 each)", s0.measuredVRAM, s0.measuredRAM)
	}
	if d := o.demandSpec(s1); !demandUnknown(d) || d.GPUs[0].VRAMMB != 0 || d.RAMMB != 0 {
		t.Fatalf("instance 1 inherits gpus %+v, ram %d from a reading taken mid-load; want both still unknown", d.GPUs, d.RAMMB)
	}

	reading.Store(6000)
	o.dispatchMeasurement() // taken once instance 0 is ready
	land()
	if d := o.demandSpec(s1); d.GPUs[0].VRAMMB != 6000 || d.RAMMB != 6000 {
		t.Fatalf("instance 1 demand after a reading taken once ready = gpus %+v, ram %d, want 6000 each", d.GPUs, d.RAMMB)
	}
}

// TestReadyMeasurementClearedOnExit: a ready reading describes one process,
// exactly like the measurement it is taken from (F2), so it goes when the
// process does. Kept, it would raise a sibling's demand on behalf of the
// instance's NEXT generation while that one is still loading -- possibly with a
// different launch shape, and so a different footprint.
func TestReadyMeasurementClearedOnExit(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, GPUs: []SpecGPU{{Index: 0}}}
	logs := NewLogStore(0, 0)
	s0 := &specState{
		spec:            s,
		state:           StateDraining,
		intentionalStop: true, // a clean stop: onProcExited rests it at Stopped
		measuredVRAM:    map[int]int{0: 6000},
		readyVRAM:       map[int]int{0: 6000},
		measuredRAM:     3000,
		readyRAM:        3000,
		proc:            &runningProc{specID: "S", pid: 4242, log: logs.newProc("S"), exited: make(chan struct{}), everHealthy: true},
	}
	s1 := &specState{spec: specForInstance(s, 1), instance: 1, state: StateStopped}
	o := instanceOwner(s0, s1)

	o.onProcExited(s0, nil)
	if s0.readyVRAM != nil || s0.readyRAM != 0 {
		t.Fatalf("after the exit instance 0 still holds ready figures vram %v, ram %d; want them cleared with the process", s0.readyVRAM, s0.readyRAM)
	}

	// Its next generation is still loading: nothing of the last one may count.
	exited := make(chan struct{})
	close(exited)
	s0.state, s0.proc = StateStarting, &runningProc{specID: "S", pid: 4343, exited: exited}
	if d := o.demandSpec(s1); !demandUnknown(d) {
		t.Fatalf("instance 1 inherits gpus %+v, ram %d from instance 0's previous process; want its demand unknown", d.GPUs, d.RAMMB)
	}
}

func TestEvictOneIdleForBootstrapHonoursInstanceRules(t *testing.T) {
	now := time.Now()
	s := Spec{ID: "S", UpstreamModel: "s", Replicable: true}
	tSpec := Spec{ID: "T", UpstreamModel: "t", Replicable: true}
	u := Spec{ID: "U", UpstreamModel: "u"}

	sib := liveInstance(s, 1, 11) // the beneficiary's own sibling, oldest of all
	sib.lastUsed = now.Add(-time.Hour)
	t0 := liveInstance(tSpec, 0, 20) // idle, but its higher sibling still runs
	t0.lastUsed = now.Add(-50 * time.Minute)
	t1 := liveInstance(tSpec, 1, 21) // surplus, the newest
	t1.lastUsed = now.Add(-time.Minute)
	uu := liveInstance(u, 0, 30)
	uu.lastUsed = now.Add(-40 * time.Minute)
	o := instanceOwner(sib, t0, t1, uu)

	if !o.evictOneIdleForBootstrap("S") {
		t.Fatal("found no victim")
	}
	if t1.state != StateDraining {
		t.Fatalf("victim states: S#1 %s, T %s, T#1 %s, U %s -- want T#1 (surplus first; never the beneficiary's sibling; never T while T#1 runs)",
			sib.state, t0.state, t1.state, uu.state)
	}
	for _, st := range []*specState{sib, t0, uu} {
		if st.state != StateRunning {
			t.Errorf("%s = %s, want untouched", st.spec.ID, st.state)
		}
	}

	// With T#1 busy, T is still shielded by it; U is the only legal victim.
	t1.state, t1.inFlight = StateRunning, 1
	if !o.evictOneIdleForBootstrap("S") || uu.state != StateDraining || t0.state != StateRunning || sib.state != StateRunning {
		t.Fatalf("second pick: S#1 %s, T %s, U %s -- want U", sib.state, t0.state, uu.state)
	}
}

// logBufferKeys lists the keys the manager's log store retains a buffer under.
func logBufferKeys(m *Manager) []string {
	m.logs.mu.Lock()
	defer m.logs.mu.Unlock()
	keys := make([]string, 0, len(m.logs.logs))
	for k := range m.logs.logs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TestInstanceOutputIsInTheSpecsLogView: every instance of a replicable spec
// writes into the BASE spec's one log buffer -- the one the spec's log view and
// the gateway's watch command name -- instance 0 exactly as it printed, a
// further instance k with "[#k] " opening each of its lines. So a crashed burst
// instance's output is readable there after its row has been retired, no
// buffer is ever keyed by an instance, and an Apply keeps the spec's buffer
// while the spec exists and releases it with the spec.
func TestInstanceOutputIsInTheSpecsLogView(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	// Every instance prints "stubchild: scripted crash" to stderr and exits
	// well after it became healthy, so each request is served first.
	spec.Args = stubArgs(0, 400*time.Millisecond, 3, "")
	m.Apply(Config{Specs: []Spec{spec, baseSpec("T", "t")}, ETag: "v1"})

	_, rel0 := ensureModel(t, m, "m")
	rel0()
	_, rel1 := ensureModel(t, m, "m#1")
	rel1()

	const crash = "stubchild: scripted crash"
	batch := waitForLog(t, m, "S", "both instances' crash output and two exit markers", func(b LogBatch) bool {
		return strings.Count(batchText(b), crash) == 2 && countEvents(b, logEventExited) == 2
	})
	text := batchText(batch)
	if n := strings.Count(text, "[#1] "+crash); n != 1 {
		t.Errorf("history has %d lines %q, want instance 1's crash line once, prefixed; history:\n%s", n, "[#1] "+crash, text)
	}
	unprefixed := strings.HasPrefix(text, crash) || strings.Contains(text, "\n"+crash)
	if strings.Contains(text, "[#0]") || !unprefixed {
		t.Errorf("history:\n%s\nwant instance 0's crash line unprefixed, exactly as it printed", text)
	}
	if keys := logBufferKeys(m); len(keys) != 1 || keys[0] != "S" {
		t.Fatalf("log buffers = %v, want the spec's own [S] alone: no buffer is keyed by an instance", keys)
	}
	if st := instanceStatus(m, "S", 1); st == nil || st.LastError == nil || !strings.Contains(st.LastError.StderrTail, "[#1] "+crash) {
		t.Errorf("instance 1 row = %+v, want its LastError tail to carry its own prefixed crash line", st)
	}

	// The next Apply retires the crashed burst instance -- and keeps its output,
	// because the output belongs to the spec.
	m.Apply(Config{Specs: []Spec{spec, baseSpec("T", "t")}, ETag: "v2"})
	if st := instanceStatus(m, "S", 1); st != nil {
		t.Fatalf("instance 1 = %+v after the next Apply, want it retired", st)
	}
	waitForLogText(t, m, "S", "[#1] "+crash)
	if keys := logBufferKeys(m); len(keys) != 1 || keys[0] != "S" {
		t.Fatalf("log buffers after the Apply = %v, want [S] alone", keys)
	}

	// The spec leaves, and its buffer -- every instance's output -- with it.
	m.Apply(Config{Specs: []Spec{baseSpec("T", "t")}, ETag: "v3"})
	if keys := logBufferKeys(m); len(keys) != 0 {
		t.Fatalf("log buffers after the spec was removed = %v, want none", keys)
	}
}

// ---------------------------------------------------------------------------
// Fold-in round: start order, reserved ids, re-add while draining, names freed
// by a removed spec, occupant charges, and the instance paths below.
// ---------------------------------------------------------------------------

// killInstance kills the live process of instance k of specID from outside, the
// way an OOM killer or a segfault would end it: an exit nobody asked for.
func killInstance(t *testing.T, m *Manager, specID string, k int) int {
	t.Helper()
	pid := mustInstance(t, m, specID, k).PID
	if pid == 0 {
		t.Fatalf("instance %d of %s has no process to kill", k, specID)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("find pid %d: %v", pid, err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatalf("kill pid %d: %v", pid, err)
	}
	return pid
}

// TestLessByQueueAgeOrdersInstancesNumerically: equal queue times -- above all
// the protected instances with no waiter at all -- are ordered by spec, then by
// instance index as a NUMBER. As strings S#10 sorts before S#2, and S#1 after
// nothing that would keep instance 0 first; the wake order asks the lowest
// index first, so a further instance finds its lower siblings started instead
// of being held back for them (TestProtectedInstanceStartsOnlyAfterItsLowerSiblings).
func TestLessByQueueAgeOrdersInstancesNumerically(t *testing.T) {
	t0 := time.Now()
	c := func(base string, k int, queuedAt time.Time) wakeCandidate {
		return wakeCandidate{id: InstanceKey(base, k), base: base, instance: k, queuedAt: queuedAt}
	}
	var none time.Time
	for _, tc := range []struct {
		name          string
		first, second wakeCandidate // first must sort before second
	}{
		{"instance 0 before a further instance", c("S", 0, none), c("S", 1, none)},
		{"index order, not string order", c("S", 2, none), c("S", 10, none)},
		{"index order with equal queue times", c("S", 2, t0), c("S", 10, t0)},
		{"by spec before by index", c("S", 10, none), c("T", 0, none)},
		{"an older queue time still first", c("S", 10, t0), c("S", 2, t0.Add(time.Millisecond))},
		{"a waiter still before no waiter", c("S", 10, t0), c("S", 0, none)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !lessByQueueAge(tc.first, tc.second) {
				t.Errorf("lessByQueueAge(%s, %s) = false, want true: %s sorts first", tc.first.id, tc.second.id, tc.first.id)
			}
			if lessByQueueAge(tc.second, tc.first) {
				t.Errorf("lessByQueueAge(%s, %s) = true, want false: %s sorts first", tc.second.id, tc.first.id, tc.first.id)
			}
		})
	}
}

// TestSortInstanceOrder: the order the Apply start loop asks protected instances
// in -- by spec, then by index numerically, never map or string order.
func TestSortInstanceOrder(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "s", Replicable: true}
	var states []*specState
	for _, k := range []int{10, 2, 0, 1} {
		states = append(states, &specState{spec: specForInstance(s, k), instance: k})
	}
	states = append(states, &specState{spec: Spec{ID: "T", UpstreamModel: "t"}})
	o := instanceOwner(states...)
	ids := []string{"S#10", "T", "S#2", "S", "S#1"}
	o.sortInstanceOrder(ids)
	if got, want := strings.Join(ids, " "), "S S#1 S#2 S#10 T"; got != want {
		t.Fatalf("sorted = %s, want %s", got, want)
	}
}

// TestProtectedFloorComesUpLowestIndexFirst: when the room holds fewer instances
// than min_instances asks for, the instance that gets it is instance 0. A
// further instance never gives its room back to a sibling (a sibling is never a
// victim), so one started first in map order would keep instance 0 down for as
// long as it runs -- contradicting §4, where instance 0 is the last of a spec to
// go. Eight protected instances and a process limit of one, a few rounds over:
// a start order left to the map would pick a random one of the eight.
func TestProtectedFloorComesUpLowestIndexFirst(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	for round := 0; round < 4; round++ {
		m := NewManager(ManagerOptions{Policy: allowlistPolicy(), Getenv: func(string) string { return "" }})
		t.Cleanup(m.Close)

		spec := replicableSpec("S", "m")
		spec.MinInstances = 8
		m.Apply(Config{Specs: []Spec{spec}, MaxProcesses: 1})

		var started []int
		waitUntil(t, 5*time.Second, "one protected instance started", func() bool {
			started = started[:0]
			for _, r := range m.Status() {
				if r.PID != 0 {
					started = append(started, r.Instance)
				}
			}
			return len(started) > 0
		})
		if len(started) != 1 || started[0] != 0 {
			t.Fatalf("round %d: instances with a process = %v, want [0]: the room for one goes to instance 0", round, started)
		}
		waitUntil(t, 5*time.Second, "instance 0 running", func() bool {
			st := instanceStatus(m, "S", 0)
			return st != nil && st.State == StateRunning
		})
		for k := 1; k < 8; k++ {
			if st := instanceStatus(m, "S", k); st == nil || st.PID != 0 {
				t.Fatalf("round %d: instance %d = %+v, want it waiting with no process (the room holds one)", round, k, st)
			}
		}
		m.Close()
	}
}

// TestApplyIgnoresASpecWhoseIDIsAnInstanceKey: "S#1" is the key of S's instance
// 1, so a spec with that ID -- possible only in a Config handed to Apply without
// ParseConfig, which drops it -- must not take over S's instance: not its state,
// not its process, not its address. It is ignored, and S keeps its instance.
func TestApplyIgnoresASpecWhoseIDIsAnInstanceKey(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	_, release := ensureModel(t, m, "m#1")
	defer release()
	pid1 := mustInstance(t, m, "S", 1).PID

	m.Apply(Config{Specs: []Spec{spec, baseSpec("S#1", "other")}, ETag: "v2"})

	if st := mustInstance(t, m, "S", 1); st.State != StateRunning || st.PID != pid1 || st.Model != "m#1" {
		t.Fatalf("S instance 1 = %+v after a push with a spec named S#1, want it untouched (running as pid %d, model m#1)", st, pid1)
	}
	if base, k, ok := m.ResolveAddress("m#1"); !ok || base != "m" || k != 1 {
		t.Errorf("ResolveAddress(m#1) = (%q, %d, %v), want (m, 1, true)", base, k, ok)
	}
	if _, _, ok := m.ResolveAddress("other"); ok {
		t.Errorf("ResolveAddress(other) = ok, want not managed: the spec named S#1 is ignored")
	}
	for _, r := range m.Status() {
		if r.SpecID != "S" {
			t.Errorf("row %+v, want only S's rows: the spec named S#1 is ignored", r)
		}
	}
}

// TestSpecPushedAgainWhileDrainingKeepsEveryInstance: a push without S drains
// all of its instances; a push with S again before they have exited makes S
// desired again, for EVERY instance alike. Each drain completes, and each
// instance then follows the normal rules -- here, protected, it comes straight
// back -- and the spec stays addressable throughout. Clearing the removal on
// the further instances but not on instance 0 deleted instance 0 on exit and
// left instance 1 running behind an address nothing resolved any more.
func TestSpecPushedAgainWhileDrainingKeepsEveryInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	pid0, pid1 := mustInstance(t, m, "S", 0).PID, mustInstance(t, m, "S", 1).PID
	// A request in flight on each holds both drains open across the next push.
	_, rel0 := ensureModel(t, m, "m")
	defer rel0()
	_, rel1 := ensureModel(t, m, "m#1")
	defer rel1()

	m.Apply(Config{Specs: []Spec{}, ETag: "v2"})
	for k := 0; k < 2; k++ {
		if st := mustInstance(t, m, "S", k); st.State != StateDraining {
			t.Fatalf("instance %d = %+v after its spec was removed, want draining (its request is in flight)", k, st)
		}
	}
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v3"})

	rel0()
	rel1()
	waitUntil(t, 5*time.Second, "both instances back as new processes once their drains completed", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s0.PID != pid0 &&
			s1 != nil && s1.State == StateRunning && s1.PID != pid1
	})
	for model, k := range map[string]int{"m": 0, "m#1": 1} {
		if base, gotK, ok := m.ResolveAddress(model); !ok || base != "m" || gotK != k {
			t.Errorf("ResolveAddress(%s) = (%q, %d, %v), want (m, %d, true)", model, base, gotK, ok, k)
		}
	}
	_, release := ensureModel(t, m, "m")
	release()
}

// TestRemovedSpecLeavingFreesAnInstanceAddress: L's literal model foo#2 is
// replicable F's instance-2 address, so while L exists F's floor (min 3) skips
// instance 2. When a push removes L while it serves a request, L drains -- and
// its name stays L's until it has gone. Its exit frees the name: F's protected
// instance 2 must come up then, not wait for a later push that changes the
// document.
func TestRemovedSpecLeavingFreesAnInstanceAddress(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	f := replicableSpec("F", "foo")
	f.MinInstances = 3
	coresident := [][2]string{{"F", "L"}} // L may run beside F's protected instances
	m.Apply(Config{Specs: []Spec{baseSpec("L", "foo#2"), f}, Coresident: coresident, ETag: "v1"})
	waitUntil(t, 5*time.Second, "F's reachable protected instances 0 and 1 running", func() bool {
		s0, s1 := instanceStatus(m, "F", 0), instanceStatus(m, "F", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	_, relL := ensureModel(t, m, "foo#2") // L, serving a request across the push
	defer relL()

	m.Apply(Config{Specs: []Spec{f}, ETag: "v2"})
	if st := statusFor(m, "L"); st == nil || st.State != StateDraining {
		t.Fatalf("L = %+v right after its removal, want draining (its request is in flight)", st)
	}
	if st := instanceStatus(m, "F", 2); st != nil {
		t.Fatalf("F instance 2 = %+v while L still drains, want none yet: foo#2 is still L's", st)
	}

	relL()
	waitUntil(t, 5*time.Second, "F's instance 2 running once L has gone", func() bool {
		st := instanceStatus(m, "F", 2)
		return st != nil && st.State == StateRunning
	})
	if st := statusFor(m, "L"); st != nil {
		t.Errorf("L = %+v, want its row gone", st)
	}
	if base, k, ok := m.ResolveAddress("foo#2"); !ok || base != "foo" || k != 2 {
		t.Errorf("ResolveAddress(foo#2) = (%q, %d, %v), want (foo, 2, true): the name is F's instance 2 now", base, k, ok)
	}
}

// ramChargedFor returns the host RAM a snapshot charges to specID.
func ramChargedFor(t *testing.T, snap PolicySnapshot, specID string) int {
	t.Helper()
	for _, r := range snap.Running {
		if r.SpecID == specID {
			return r.RAMMB
		}
	}
	t.Fatalf("PolicySnapshot.Running has no row for %q", specID)
	return 0
}

// TestBuildSnapshotChargesRunningSiblingsTheirOwnMeasurement: siblings are summed
// (§5.4 rule 1), each at what it holds. A running instance with its own ready
// measurement is charged that -- floored at its declared demand, never raised to
// a sibling's peak; only what an occupant has not measured of itself (a sibling
// still loading) inherits the peak. Raised to the peak, siblings measured at
// 3000 and 3500 would count as 7000, and a third copy that fits next to their
// real 6500 on a 10000 MB GPU would be refused.
func TestBuildSnapshotChargesRunningSiblingsTheirOwnMeasurement(t *testing.T) {
	s := Spec{ID: "S", UpstreamModel: "m", Replicable: true, GPUs: []SpecGPU{{Index: 0}}}

	t.Run("running siblings are each charged their own", func(t *testing.T) {
		s0 := liveInstance(s, 0, 100)
		s0.measuredRAM, s0.readyRAM = 3000, 3000
		s1 := liveInstance(s, 1, 101)
		s1.measuredRAM, s1.readyRAM = 3500, 3500
		s2 := &specState{spec: specForInstance(s, 2), instance: 2, state: StateStopped}
		o := instanceOwner(s0, s1, s2)
		o.cfg = Config{GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 10000}}}
		o.m.SetMeasurer(func([]int) map[int]map[int]int {
			return map[int]map[int]int{100: {0: 3000}, 101: {0: 3500}}
		})

		snap := o.buildSnapshot()
		for _, want := range []struct {
			id        string
			vram, ram int
		}{{"S", 3000, 3000}, {"S#1", 3500, 3500}} {
			if got := gpusChargedFor(t, snap, want.id); got[0] != want.vram {
				t.Errorf("%s charged VRAM %v, want gpu0 %d (its own measurement, not the sibling peak)", want.id, got, want.vram)
			}
			if got := ramChargedFor(t, snap, want.id); got != want.ram {
				t.Errorf("%s charged RAM %d, want %d (its own measurement, not the sibling peak)", want.id, got, want.ram)
			}
		}
		// The candidate inherits the peak, and 3000 + 3500 + 3500 fits 10000.
		d := o.demandSpec(s2)
		if d.GPUs[0].VRAMMB != 3500 || d.RAMMB != 3500 {
			t.Fatalf("candidate instance 2 demand = gpus %+v ram %d, want the sibling peak 3500 on both", d.GPUs, d.RAMMB)
		}
		snap.NoEvict = true
		if dec := Admit(snap, d); !dec.OK {
			t.Fatalf("Admit(instance 2 at 3500 next to 3000 + 3500 on a 10000 MB GPU) = %+v, want OK", dec)
		}

		// A sibling that this call did not measure is still charged its own last
		// ready figure, not the other sibling's.
		o.m.SetMeasurer(func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 3000}} })
		snap = o.buildSnapshot()
		if got := gpusChargedFor(t, snap, "S#1"); got[0] != 3500 {
			t.Errorf("S#1 charged %v when this call measured only S, want gpu0 3500 (its own last ready figure)", got)
		}
	})

	t.Run("a declared demand above the measurement stays the floor", func(t *testing.T) {
		declared := s
		declared.GPUs = []SpecGPU{{Index: 0, VRAMMB: 4000}}
		s0 := liveInstance(declared, 0, 100)
		s1 := liveInstance(declared, 1, 101)
		o := instanceOwner(s0, s1)
		o.m.SetMeasurer(func([]int) map[int]map[int]int {
			return map[int]map[int]int{100: {0: 3000}, 101: {0: 5000}}
		})
		snap := o.buildSnapshot()
		if got := gpusChargedFor(t, snap, "S"); got[0] != 4000 {
			t.Errorf("S charged %v, want gpu0 4000 (its declared demand floors its 3000 measurement)", got)
		}
		if got := gpusChargedFor(t, snap, "S#1"); got[0] != 5000 {
			t.Errorf("S#1 charged %v, want gpu0 5000 (its own measurement above the floor)", got)
		}
	})

	t.Run("a sibling still loading inherits the peak", func(t *testing.T) {
		s0 := liveInstance(s, 0, 100)
		s0.measuredRAM, s0.readyRAM = 3000, 3000
		loading := liveInstance(s, 1, 101)
		loading.state = StateStarting
		loading.proc.everHealthy = false
		loading.measuredRAM = 1200 // a mid-load reading: never a ready one
		o := instanceOwner(s0, loading)
		o.m.SetMeasurer(func([]int) map[int]map[int]int {
			return map[int]map[int]int{100: {0: 3000}, 101: {0: 1200}}
		})
		snap := o.buildSnapshot()
		if got := gpusChargedFor(t, snap, "S#1"); got[0] != 3000 {
			t.Errorf("loading S#1 charged %v, want gpu0 3000 (the ready sibling's figure, above its own mid-load 1200)", got)
		}
		if got := ramChargedFor(t, snap, "S#1"); got != 3000 {
			t.Errorf("loading S#1 charged RAM %d, want 3000 (the ready sibling's figure, above its own mid-load 1200)", got)
		}
	})
}

// TestForceStoppedDrainsEveryInstance: force_stopped applies to all of a spec's
// instances (§5.2) -- the protected ones and a running burst one drain, nothing
// restarts, and an ensure of any instance address is refused, creating nothing.
func TestForceStoppedDrainsEveryInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	spec.Args = stubArgs(0, 0, 0, invocationLog)
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	_, release := ensureModel(t, m, "m#2") // a burst instance as well
	release()

	stopped := spec
	stopped.AdminState = "force_stopped"
	m.Apply(Config{Specs: []Spec{stopped}, ETag: "v2"})
	waitUntil(t, 5*time.Second, "every instance stopped, the burst one retired", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateStopped && s0.PID == 0 &&
			s1 != nil && s1.State == StateStopped && s1.PID == 0 &&
			instanceStatus(m, "S", 2) == nil
	})
	execs := countInvocations(t, invocationLog)

	for _, model := range []string{"m", "m#1", "m#2", "m#3"} {
		if _, err := ensureErr(t, m, model); !errors.Is(err, ErrAdmissionBlocked) {
			t.Errorf("EnsureRunning(%s) on a force_stopped spec = %v, want ErrAdmissionBlocked", model, err)
		}
	}
	// Ten housekeeping beats: the protected floor must stay down.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		for k := 0; k < 2; k++ {
			if st := instanceStatus(m, "S", k); st == nil || st.State != StateStopped || st.PID != 0 {
				t.Fatalf("instance %d = %+v, want it to stay stopped while the spec is force_stopped", k, st)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rows := m.Status(); len(rows) != 2 {
		t.Errorf("Status() = %+v, want instances 0 and 1 only: a refused ensure creates nothing", rows)
	}
	if n := countInvocations(t, invocationLog); n != execs {
		t.Errorf("child invocations = %d after force_stopped, want still %d: nothing may start", n, execs)
	}
}

// TestProtectedInstanceRestartsAfterACrash: a protected instance k >= 1 is
// restarted after a crash, after a crash-loop backoff, like a pinned spec
// (§5.4 rule 7) -- unlike a burst instance, which is not.
func TestProtectedInstanceRestartsAfterACrash(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setBackoffWindow(t, 300*time.Millisecond)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	})
	pid0 := mustInstance(t, m, "S", 0).PID

	killed := killInstance(t, m, "S", 1)
	waitUntil(t, 5*time.Second, "instance 1 waiting out a crash backoff", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateBackoff && st.PID == 0
	})
	if st := mustInstance(t, m, "S", 1); st.LastError == nil {
		t.Errorf("instance 1 = %+v in backoff, want the crash recorded as its LastError", st)
	}
	waitUntil(t, 5*time.Second, "instance 1 restarted after the backoff", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateRunning && st.PID != 0 && st.PID != killed
	})
	if st := mustInstance(t, m, "S", 1); st.Restarts != 1 {
		t.Errorf("instance 1 Restarts = %d, want 1", st.Restarts)
	}
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning || st.PID != pid0 {
		t.Errorf("instance 0 = %+v, want untouched (running as pid %d)", st, pid0)
	}
}

// TestEnsureBurstInstanceWhileStarting: a request for a burst instance that is
// still starting waits for that start -- the one thing burst capacity is ever
// queued behind (§5.4 rule 6) -- and both requests are served by one process.
func TestEnsureBurstInstanceWhileStarting(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec, openGate := gatedSpec(t, "S", "m")
	spec.Replicable = true
	spec.RAMMB = 64
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first := ensureAsync(ctx, m, "m#1")
	waitUntil(t, 5*time.Second, "instance 1 starting", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateStarting
	})
	second := ensureAsync(ctx, m, "m#1")
	// The health gate holds instance 1 in Starting: a refusal would answer now.
	select {
	case r := <-second:
		t.Fatalf("EnsureRunning(m#1) while instance 1 starts answered before its start finished: %+v, want it to wait for the start", r)
	case <-time.After(300 * time.Millisecond):
	}

	openGate()
	r1, r2 := <-first, <-second
	if r1.err != nil || r2.err != nil {
		t.Fatalf("EnsureRunning(m#1) = %v / %v, want both served once the start finished", r1.err, r2.err)
	}
	defer r1.release()
	defer r2.release()
	if r1.endpoint != r2.endpoint {
		t.Errorf("endpoints %s / %s, want one process serving both", r1.endpoint, r2.endpoint)
	}
	if st := mustInstance(t, m, "S", 1); st.InFlight != 2 {
		t.Errorf("instance 1 InFlight = %d, want 2", st.InFlight)
	}
}

// TestEnsureBurstInstanceWhileDraining: a burst instance on its way out is not
// waited for -- burst capacity never queues behind a slot (§5.4 rule 6) -- so a
// request for it is refused with ErrInstanceNoCapacity at once, while the
// request it is still serving finishes.
func TestEnsureBurstInstanceWhileDraining(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	ep, release := ensureModel(t, m, "m#1") // in flight across the push
	defer release()

	relaunch := spec // a launch-shape change drains every live instance
	relaunch.Args = append(stubArgs(0, 0, 0, ""), "-health-delay", "1ms")
	m.Apply(Config{Specs: []Spec{relaunch}, ETag: "v2"})
	if st := mustInstance(t, m, "S", 1); st.State != StateDraining {
		t.Fatalf("instance 1 = %+v after a launch-shape change, want draining (its request is in flight)", st)
	}

	took, err := ensureErr(t, m, "m#1")
	if !errors.Is(err, ErrInstanceNoCapacity) {
		t.Fatalf("EnsureRunning(m#1) while instance 1 drains = %v, want ErrInstanceNoCapacity", err)
	}
	if took > 2*time.Second {
		t.Errorf("the refusal took %s, want it immediate", took)
	}
	if code, body := httpEcho(t, ep, "still here"); code != 200 || body != "still here" {
		t.Fatalf("echo through the draining instance = (%d, %q), want it served", code, body)
	}
	release()
	waitUntil(t, 5*time.Second, "instance 1 retired once drained", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
}

// TestConcurrentEnsureStartsOneBurstInstance is TestManagerConcurrentEnsureSingleStart
// for a cold instance address: many concurrent requests for m#1 create the
// instance once and start exactly one process, counted from the child side.
func TestConcurrentEnsureStartsOneBurstInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	invocationLog := filepath.Join(t.TempDir(), "invocations.log")
	spec := replicableSpec("S", "m")
	spec.Args = stubArgs(50*time.Millisecond, 0, 0, invocationLog)
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	const n = 10
	results := make([]<-chan queuedEnsure, n)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := range results {
		results[i] = ensureAsync(ctx, m, "m#1")
	}
	endpoints := map[string]bool{}
	for i, ch := range results {
		r := <-ch
		if r.err != nil {
			t.Errorf("request %d: EnsureRunning(m#1) = %v", i, r.err)
			continue
		}
		endpoints[r.endpoint] = true
		r.release()
	}
	if len(endpoints) != 1 {
		t.Errorf("endpoints = %v, want one", endpoints)
	}
	if got := countInvocations(t, invocationLog); got != 1 {
		t.Fatalf("child-side invocation count = %d, want exactly 1 (one instance, one start)", got)
	}
	if rows := m.Status(); len(rows) != 2 {
		t.Errorf("Status() = %+v, want instance 0 (never asked for) and instance 1", rows)
	}
}

// TestInstanceZeroRestartsOnItsSiblingsMeasurement drives the restart case of
// TestInstanceZeroInheritsProtectedSiblingMeasurement through the manager's own
// admission (admitAndStart): instance 0 of an unknown-VRAM spec crashes while
// its protected -- hence pinned -- sibling runs measured. Charged its declared,
// unknown demand next to that pinned occupant it would rest at
// pending_vram_unknown for good; charged the sibling's measurement it simply
// comes back.
func TestInstanceZeroRestartsOnItsSiblingsMeasurement(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		out := make(map[int]map[int]int, len(pids))
		for _, p := range pids {
			out[p] = map[int]int{0: 3000}
		}
		return out
	})

	spec := replicableSpec("S", "m")
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}} // the operator left the estimate at 0
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}, GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 8000}}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running and measured", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s0.MeasuredVRAM[0] == 3000 &&
			s1 != nil && s1.State == StateRunning && s1.MeasuredVRAM[0] == 3000
	})

	killed := killInstance(t, m, "S", 0)
	waitUntil(t, 5*time.Second, "instance 0 back after its crash", func() bool {
		st := instanceStatus(m, "S", 0)
		if st != nil && st.State == StatePendingVRAMUnknown {
			t.Fatalf("instance 0 = %+v, want it restarted on its sibling's measured 3000 MB, not blocked as unknown next to the pinned sibling", st)
		}
		return st != nil && st.State == StateRunning && st.PID != 0 && st.PID != killed
	})
}

// TestEvictOneIdleForBootstrapSparesSiblingsOfAnInstanceAndProtected covers the
// two branches TestEvictOneIdleForBootstrapHonoursInstanceRules does not: the
// beneficiary is itself an instance k >= 1 whose state exists (its siblings are
// found through it, by base spec, not by its key), and an idle instance inside
// another spec's min_instances floor is never the victim.
func TestEvictOneIdleForBootstrapSparesSiblingsOfAnInstanceAndProtected(t *testing.T) {
	now := time.Now()
	s := Spec{ID: "S", UpstreamModel: "s", Replicable: true}
	q := Spec{ID: "Q", UpstreamModel: "q", Replicable: true, MinInstances: 1}
	u := Spec{ID: "U", UpstreamModel: "u"}

	beneficiary := &specState{spec: specForInstance(s, 2), instance: 2, state: StateStartFailed}
	sib := liveInstance(s, 1, 11) // the beneficiary's sibling, the oldest of all
	sib.lastUsed = now.Add(-2 * time.Hour)
	q0 := liveInstance(q, 0, 20) // protected by Q's min_instances, the next oldest
	q0.lastUsed = now.Add(-time.Hour)
	uu := liveInstance(u, 0, 30)
	uu.lastUsed = now.Add(-time.Minute)
	o := instanceOwner(beneficiary, sib, q0, uu)

	if !o.evictOneIdleForBootstrap("S#2") {
		t.Fatal("found no victim, want U")
	}
	if uu.state != StateDraining || sib.state != StateRunning || q0.state != StateRunning {
		t.Fatalf("states: S#1 %s, Q %s, U %s -- want only U draining (never the beneficiary's sibling, never a protected instance)",
			sib.state, q0.state, uu.state)
	}
	if len(uu.drainedFor) != 1 || uu.drainedFor[0] != "S#2" {
		t.Errorf("U.drainedFor = %v, want [S#2]: the freed slot is offered to the beneficiary first", uu.drainedFor)
	}

	// With U busy, the sibling and the protected instance are all that is idle,
	// and neither is a legal victim.
	uu.state, uu.inFlight = StateRunning, 1
	if o.evictOneIdleForBootstrap("S#2") {
		t.Fatalf("found a victim: S#1 %s, Q %s -- want none", sib.state, q0.state)
	}
}

// ---------------------------------------------------------------------------
// Start order on every path: a protected further instance starts only once its
// lower siblings have (§4). The Apply start loop and the wake order ask the
// lowest index first, but crash-loop timers, a request naming a further
// instance, and an instance's own re-admission on exit ask in event order; the
// admission itself is what has to keep the order.
// ---------------------------------------------------------------------------

// startOrderOwner builds an owner for the start-order tests: the given states
// plus an idle, unrelated occupant U, co-resident with S, under a limit of
// limit processes. A candidate admitted past the start-order rule needs U's
// slot, so it EVICTS U -- beginDrain is a pure state change for U's
// already-exited generation, and nothing is ever exec'd -- while one the rule
// holds back leaves U running and untouched.
func startOrderOwner(limit int, states ...*specState) (*owner, *specState) {
	u := liveInstance(baseSpec("U", "u"), 0, 30)
	o := instanceOwner(append(states, u)...)
	o.policy = allowlistPolicy()
	o.cfg = Config{MaxProcesses: limit}
	o.allowedPairs = allowPairs("S", "U") // only the process limit makes room scarce
	return o, u
}

// restingInstance is instance k of spec with no process, resting in state.
func restingInstance(spec Spec, k int, state State) *specState {
	return &specState{spec: specForInstance(spec, k), instance: k, state: state}
}

// TestProtectedInstanceStartsOnlyAfterItsLowerSiblings: a protected instance
// k >= 1 is not admitted while a lower sibling still waits to start -- for room,
// or for its crash-loop retry -- because the room it took would never come back
// (a sibling is never a victim, §5.4 rule 3) and instance 0 would stay down while
// it runs, against §4. It waits instead, silently and with no victim drained,
// and goes as soon as every lower sibling has a process. Gaps below it (an
// instance that cannot exist) hold nothing back.
func TestProtectedInstanceStartsOnlyAfterItsLowerSiblings(t *testing.T) {
	min2 := replicableSpec("S", "m")
	min2.MinInstances = 2
	min3 := replicableSpec("S", "m")
	min3.MinInstances = 3
	starting := func(spec Spec, k int) *specState {
		st := liveInstance(spec, k, 100+k)
		st.state = StateStarting
		st.proc.everHealthy = false
		return st
	}
	for _, tc := range []struct {
		name      string
		candidate *specState
		siblings  []*specState
		limit     int
		held      bool
	}{
		{
			name:      "instance 0 still waiting for room",
			candidate: restingInstance(min2, 1, StateStopped),
			siblings:  []*specState{restingInstance(min2, 0, StateStopped)},
			limit:     1,
			held:      true,
		},
		{
			name:      "instance 0 waiting out a crash backoff",
			candidate: restingInstance(min2, 1, StateStopped),
			siblings:  []*specState{restingInstance(min2, 0, StateBackoff)},
			limit:     1,
			held:      true,
		},
		{
			name:      "instance 1 still waiting while instance 0 runs",
			candidate: restingInstance(min3, 2, StateStopped),
			siblings:  []*specState{liveInstance(min3, 0, 100), restingInstance(min3, 1, StateStopped)},
			limit:     2,
			held:      true,
		},
		{
			name:      "instance 0 already starting",
			candidate: restingInstance(min2, 1, StateStopped),
			siblings:  []*specState{starting(min2, 0)},
			limit:     2,
		},
		{
			name:      "a gap below holds nothing back",
			candidate: restingInstance(min3, 2, StateStopped),
			siblings:  []*specState{liveInstance(min3, 0, 100)},
			limit:     2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, u := startOrderOwner(tc.limit, append(tc.siblings, tc.candidate)...)
			tc.candidate.waitingOnSibling = !tc.held // rewritten by every verdict

			o.admitAndStart(tc.candidate.spec.ID)

			if tc.held {
				if u.state != StateRunning || len(u.drainedFor) != 0 || tc.candidate.proc != nil {
					t.Fatalf("U = %s, drained for %v: instance %d claimed room while a lower sibling still waits to start; want it held back, nothing evicted",
						u.state, u.drainedFor, tc.candidate.instance)
				}
				if !tc.candidate.waitingOnSibling {
					t.Error("waitingOnSibling = false, want true: the start of the lower sibling is what re-asks it")
				}
				return
			}
			if u.state != StateDraining || len(u.drainedFor) != 1 || u.drainedFor[0] != tc.candidate.spec.ID {
				t.Fatalf("U = %s, drained for %v; want U evicted for %s: every lower sibling it has holds a process, so its admission runs",
					u.state, u.drainedFor, tc.candidate.spec.ID)
			}
			if tc.candidate.waitingOnSibling {
				t.Error("waitingOnSibling = true, want false: the verdict was not the start-order wait")
			}
		})
	}
}

// TestStartOrderHoldsOnlyAProtectedInstanceBehindAWaitingSibling: the rule is
// about room a lower sibling is waiting to start in. A further instance that is
// burst capacity is never held back by it -- it only ever takes room that is
// free right now, and is refused at once otherwise (§5.4 rule 6) -- and a lower
// sibling resting at a refusal is not waiting for room: the further instance
// reaches the same verdict on its own, and a request naming it is answered
// with it at once instead of waiting out its admission timeout.
func TestStartOrderHoldsOnlyAProtectedInstanceBehindAWaitingSibling(t *testing.T) {
	queued := func(st *specState) chan ensureOutcome {
		reply := make(chan ensureOutcome, 1)
		st.pending = []*ensureWaiter{{reply: reply, queuedAt: time.Now()}}
		return reply
	}
	answer := func(t *testing.T, reply chan ensureOutcome) error {
		t.Helper()
		select {
		case out := <-reply:
			return out.err
		default:
			t.Fatal("the queued request is still waiting; want it answered at once")
			return nil
		}
	}

	t.Run("a burst instance", func(t *testing.T) {
		spec := replicableSpec("S", "m")
		spec.Pinned = true // instance 0 protected, instance 1 burst
		burst := restingInstance(spec, 1, StateStopped)
		reply := queued(burst)
		o, u := startOrderOwner(1, restingInstance(spec, 0, StateBackoff), burst)

		o.admitAndStart("S#1")

		if err := answer(t, reply); !errors.Is(err, ErrInstanceNoCapacity) {
			t.Fatalf("burst m#1 = %v, want ErrInstanceNoCapacity: burst capacity is refused at once, never held back", err)
		}
		if u.state != StateRunning || o.specs["S#1"] != nil {
			t.Errorf("U = %s, S#1 state kept = %v; want U untouched and the refused burst instance retired", u.state, o.specs["S#1"] != nil)
		}
	})

	t.Run("a lower sibling refused on its own demand", func(t *testing.T) {
		spec := replicableSpec("S", "m")
		spec.MinInstances = 2
		spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 9000}}
		s0 := restingInstance(spec, 0, StateNotPermitted)
		s1 := restingInstance(spec, 1, StateStopped)
		reply := queued(s1)
		o, u := startOrderOwner(1, s0, s1)
		o.cfg.GPUBudgets = []GPUBudget{{Index: 0, BudgetMB: 8000}}

		o.admitAndStart("S#1")

		if err := answer(t, reply); !errors.Is(err, ErrNotPermitted) {
			t.Fatalf("m#1 = %v, want ErrNotPermitted: instance 1's own verdict, the one instance 0 rests at", err)
		}
		if s1.state != StateNotPermitted || s1.waitingOnSibling || u.state != StateRunning {
			t.Errorf("instance 1 = %s (waitingOnSibling %v), U = %s; want instance 1 not_permitted on its own verdict and U untouched",
				s1.state, s1.waitingOnSibling, u.state)
		}
	})
}

// TestAQueuedFurtherInstanceDoesNotClaimRoomAheadOfInstanceZero: the wake order
// is oldest queued request first, so a request naming m#1 puts instance 1 ahead
// of instance 0, which has no waiter of its own. Instance 1 must still not claim
// the room: the one idle neighbour is evicted for instance 0 alone, and the
// request waits for instance 1's turn.
func TestAQueuedFurtherInstanceDoesNotClaimRoomAheadOfInstanceZero(t *testing.T) {
	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	s0 := restingInstance(spec, 0, StateStopped)
	s1 := restingInstance(spec, 1, StateStopped)
	s1.pending = []*ensureWaiter{{reply: make(chan ensureOutcome, 1), queuedAt: time.Now().Add(-time.Second)}}
	o, u := startOrderOwner(1, s0, s1)

	o.wakeAdmissionCandidates()

	if u.state != StateDraining || len(u.drainedFor) != 1 || u.drainedFor[0] != "S" {
		t.Fatalf("U = %s, drained for %v; want it drained for S alone: the freed room goes to instance 0 first",
			u.state, u.drainedFor)
	}
	if s1.proc != nil || len(s1.pending) != 1 {
		t.Errorf("instance 1 = %s with %d queued, want it still waiting with its request queued", s1.state, len(s1.pending))
	}
}

// TestInstanceZeroCrashDoesNotHandItsRoomToAFurtherInstance: min_instances=2
// with room for one process. Instance 0 runs and instance 1 waits. When instance
// 0 crashes, its room is free while it waits out its crash backoff -- and the
// wake that crash causes must not hand it to instance 1, which would keep it
// for good: instance 0 restarts into its room, and instance 1 keeps waiting.
func TestInstanceZeroCrashDoesNotHandItsRoomToAFurtherInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	m.Apply(Config{Specs: []Spec{spec}, MaxProcesses: 1, ETag: "v1"})
	waitUntil(t, 5*time.Second, "instance 0 running and instance 1 waiting for room", func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.PID == 0
	})

	killed := killInstance(t, m, "S", 0)
	instance1Waits := func() {
		t.Helper()
		if s1 := mustInstance(t, m, "S", 1); s1.PID != 0 {
			t.Fatalf("instance 1 = %+v took the only room after instance 0 (%+v) crashed; want the room kept for instance 0 -- a sibling never gives room back",
				s1, mustInstance(t, m, "S", 0))
		}
	}
	waitUntil(t, 5*time.Second, "instance 0 back after its crash backoff", func() bool {
		instance1Waits()
		s0 := mustInstance(t, m, "S", 0)
		return s0.State == StateRunning && s0.PID != killed
	})
	// And it stays that way across further backoff windows and housekeeping beats.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		instance1Waits()
		if s0 := mustInstance(t, m, "S", 0); s0.State != StateRunning {
			t.Fatalf("instance 0 = %+v, want it running", s0)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestFurtherInstanceStartsAlongsideTheSiblingItWaitedFor: instance 1's
// crash-loop retry falls due while instance 0 is still waiting out a longer
// one, so instance 1 waits for instance 0 (the start order). It must then
// start the moment instance 0 does -- the two load side by side when the room
// holds both -- not only once instance 0 is ready, which the shut health gate
// here postpones for as long as the test needs.
func TestFurtherInstanceStartsAlongsideTheSiblingItWaitedFor(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	// Crash-loop waits that order the two retries: instance 0's second failure
	// in a row waits 500ms, instance 1's first 250ms. Failure streaks are not
	// reset by a stable run within the test.
	origBase, origCap, origStable := backoffBase, backoffCap, stableRunThreshold
	backoffBase, backoffCap, stableRunThreshold = 250*time.Millisecond, 2*time.Second, time.Minute
	t.Cleanup(func() { backoffBase, backoffCap, stableRunThreshold = origBase, origCap, origStable })
	m := newTestManager(t, allowlistPolicy())

	gate := filepath.Join(t.TempDir(), "S.healthy")
	setGate := func(open bool) {
		t.Helper()
		var err error
		if open {
			err = os.WriteFile(gate, nil, 0o644)
		} else {
			err = os.Remove(gate)
		}
		if err != nil {
			t.Fatalf("set the health gate open=%v: %v", open, err)
		}
	}
	spec := replicableSpec("S", "m")
	spec.MinInstances = 2
	spec.Args = []string{"-port", "${PORT}", "-health-gate", gate}
	bothRunning := func() bool {
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		return s0 != nil && s0.State == StateRunning && s1 != nil && s1.State == StateRunning
	}

	setGate(true)
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})
	waitUntil(t, 5*time.Second, "both protected instances running", bothRunning)
	first := killInstance(t, m, "S", 0) // a first failure, so the next one waits twice as long
	waitUntil(t, 5*time.Second, "instance 0 back after its first crash", func() bool {
		st := instanceStatus(m, "S", 0)
		return st != nil && st.State == StateRunning && st.PID != first
	})

	setGate(false)
	k0 := killInstance(t, m, "S", 0) // second failure: 500ms
	k1 := killInstance(t, m, "S", 1) // first failure: 250ms
	waitUntil(t, 5*time.Second, "instance 1's retry held back while instance 0 still waits out its longer backoff", func() bool {
		s0, s1 := mustInstance(t, m, "S", 0), mustInstance(t, m, "S", 1)
		return s0.State == StateBackoff && s1.State == StateStopped && s1.PID == 0
	})
	waitUntil(t, 3*time.Second, "both instances starting again", func() bool {
		s0, s1 := mustInstance(t, m, "S", 0), mustInstance(t, m, "S", 1)
		return s0.PID != 0 && s0.PID != k0 && s1.PID != 0 && s1.PID != k1
	})
	if s0 := mustInstance(t, m, "S", 0); s0.State != StateStarting {
		t.Fatalf("instance 0 = %+v, want it still loading behind its shut health gate: instance 1 started alongside it, not after it", s0)
	}

	setGate(true)
	waitUntil(t, 5*time.Second, "both instances running once the gate opens", bothRunning)
}

// ---------------------------------------------------------------------------
// Plain requests (EnsureServing) and the re-asks of a protected instance held
// back behind a lower sibling.
// ---------------------------------------------------------------------------

// servingAsync is ensureAsync for EnsureServing.
func servingAsync(ctx context.Context, m *Manager, model string) <-chan queuedEnsure {
	out := make(chan queuedEnsure, 1)
	go func() {
		ep, rel, err := m.EnsureServing(ctx, model)
		out <- queuedEnsure{endpoint: ep, release: rel, err: err}
	}()
	return out
}

// servingOwner is an owner-level fixture for handleEnsure: instance 0 of spec
// resting at Stopped, the given further states, the stub child allowlisted and
// the routing index built, so a request is resolved without a run loop.
func servingOwner(spec Spec, states ...*specState) *owner {
	o := instanceOwner(append([]*specState{restingInstance(spec, 0, StateStopped)}, states...)...)
	o.policy = allowlistPolicy()
	o.rebuildUpstreamIndex()
	return o
}

// answered returns what was sent on reply, and whether anything was: a request
// the owner queued has no answer yet.
func answered(reply chan ensureOutcome) (ensureOutcome, bool) {
	select {
	case out := <-reply:
		return out, true
	default:
		return ensureOutcome{}, false
	}
}

// TestEnsureServingNeverStartsABurstInstance: a plain request (EnsureServing)
// naming a burst instance is served by it while it runs and waits for it while
// it starts -- its admission has already happened. In every other state it is
// refused at once with ErrInstanceNoCapacity, and nothing is created or
// started: starting burst capacity is an ensure's decision, and the gateway
// re-selects a running instance for the request. A burst instance resting at a
// failure -- kept only so an operator can read why, until it is named again --
// is retired by the request that names it, which creates and starts nothing; a
// draining one is left to its exit, which retires it.
func TestEnsureServingNeverStartsABurstInstance(t *testing.T) {
	spec := replicableSpec("S", "m")
	spec.AdmissionWaitTimeoutSeconds = 0 // no admission timer on a bare owner
	live := func(state State) *specState {
		st := liveInstance(spec, 1, 101)
		st.state = state
		st.proc.port = 4242
		st.proc.everHealthy = state != StateStarting
		return st
	}
	for _, tc := range []struct {
		name    string
		burst   *specState // nil: instance 1 has no state at all
		want    error      // nil: served (running) or queued (starting)
		retired bool       // the refusal retires the row
		queued  bool
	}{
		{name: "no state", want: ErrInstanceNoCapacity},
		{name: "stopped", burst: restingInstance(spec, 1, StateStopped), want: ErrInstanceNoCapacity},
		{name: "crashed", burst: restingInstance(spec, 1, StateCrashed), want: ErrInstanceNoCapacity, retired: true},
		{name: "start failed", burst: restingInstance(spec, 1, StateStartFailed), want: ErrInstanceNoCapacity, retired: true},
		{name: "not permitted", burst: restingInstance(spec, 1, StateNotPermitted), want: ErrInstanceNoCapacity, retired: true},
		{name: "draining", burst: live(StateDraining), want: ErrInstanceNoCapacity},
		{name: "starting", burst: live(StateStarting), queued: true},
		{name: "running", burst: live(StateRunning)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var states []*specState
			var before State
			if tc.burst != nil {
				states = append(states, tc.burst)
				before = tc.burst.state
			}
			o := servingOwner(spec, states...)
			reply := make(chan ensureOutcome, 1)

			o.handleEnsure(cmdEnsure{model: "m#1", reply: reply, serving: true})

			out, ok := answered(reply)
			switch {
			case tc.want != nil:
				if !ok || !errors.Is(out.err, tc.want) {
					t.Fatalf("EnsureServing(m#1) answered %v (answered at once: %v), want %v at once", out.err, ok, tc.want)
				}
				if tc.retired {
					if got := o.specs["S#1"]; got != nil {
						t.Errorf("instance 1 = %s after the refusal, want its %s row retired: it was named again", got.state, before)
					}
					break
				}
				if got := o.specs["S#1"]; got != tc.burst {
					t.Errorf("instance 1 state is %p after the refusal, want %p: a plain request creates and replaces nothing", got, tc.burst)
				}
				if tc.burst != nil && (tc.burst.state != before || len(tc.burst.pending) != 0) {
					t.Errorf("instance 1 = %s with %d queued, want it left at %s with nothing queued", tc.burst.state, len(tc.burst.pending), before)
				}
			case tc.queued:
				if ok {
					t.Fatalf("EnsureServing(m#1) answered %+v at once, want it to wait for the start in progress", out)
				}
				if len(tc.burst.pending) != 1 {
					t.Errorf("instance 1 has %d queued requests, want 1", len(tc.burst.pending))
				}
			default:
				if !ok || out.err != nil || out.endpoint != endpointFor(4242) {
					t.Fatalf("EnsureServing(m#1) = %+v (answered %v), want instance 1's endpoint %s", out, ok, endpointFor(4242))
				}
				if tc.burst.inFlight != 1 {
					t.Errorf("instance 1 InFlight = %d, want 1", tc.burst.inFlight)
				}
			}
			if s0 := o.specs["S"]; s0.state != StateStopped || s0.proc != nil || len(s0.pending) != 0 {
				t.Errorf("instance 0 = %s (process %v, %d queued), want it untouched", s0.state, s0.proc != nil, len(s0.pending))
			}
		})
	}
}

// TestEnsureServingAdmitsBaselineInstancesLikeEnsureRunning: instance 0 and a
// protected instance are baseline capacity, which any request may bring up, so
// EnsureServing hands them to admission exactly as EnsureRunning does -- the
// answer is admission's own, never a capacity refusal.
func TestEnsureServingAdmitsBaselineInstancesLikeEnsureRunning(t *testing.T) {
	t.Run("instance 0", func(t *testing.T) {
		spec := replicableSpec("S", "m")
		spec.AdmissionWaitTimeoutSeconds = 0
		o := servingOwner(spec)
		o.policy = LocalPolicy{} // allows no binary: admission answers without starting anything
		reply := make(chan ensureOutcome, 1)

		o.handleEnsure(cmdEnsure{model: "m", reply: reply, serving: true})

		if out, ok := answered(reply); !ok || !errors.Is(out.err, ErrNotPermitted) {
			t.Fatalf("EnsureServing(m) answered %v (at once: %v), want admission's own ErrNotPermitted", out.err, ok)
		}
	})
	t.Run("a protected instance", func(t *testing.T) {
		spec := replicableSpec("S", "m")
		spec.MinInstances = 2
		spec.AdmissionWaitTimeoutSeconds = 0
		s1 := restingInstance(spec, 1, StateStopped)
		o := servingOwner(spec, s1) // instance 0 still waits to start
		reply := make(chan ensureOutcome, 1)

		o.handleEnsure(cmdEnsure{model: "m#1", reply: reply, serving: true})

		if out, ok := answered(reply); ok {
			t.Fatalf("EnsureServing(m#1) answered %v at once, want it queued like EnsureRunning: instance 1 is protected", out.err)
		}
		if len(s1.pending) != 1 || !s1.waitingOnSibling {
			t.Errorf("instance 1 has %d queued (waitingOnSibling %v), want the request queued behind instance 0's start", len(s1.pending), s1.waitingOnSibling)
		}
	})
}

// TestEnsureServingServesABurstInstanceOnlyOnceItIsUp drives the same rule
// through the real manager: a plain request for a burst instance nobody started
// is refused at once and starts nothing; once an ensure has started it, a plain
// request is served by that very process. Instance 0 is started by a plain
// request as it always was.
func TestEnsureServingServesABurstInstanceOnlyOnceItIsUp(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())
	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	if _, rel, err := m.EnsureServing(ctx, "m#1"); !errors.Is(err, ErrInstanceNoCapacity) {
		if rel != nil {
			rel()
		}
		t.Fatalf("EnsureServing(m#1) with nothing started = %v, want ErrInstanceNoCapacity", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the refusal took %s, want it immediate", took)
	}
	if rows := m.Status(); len(rows) != 1 || rows[0].Instance != 0 || rows[0].State != StateStopped {
		t.Fatalf("Status() = %+v, want instance 0 alone and stopped: a plain request starts no burst instance", rows)
	}

	ep, release := ensureModel(t, m, "m#1") // what the gateway's scaler does
	release()
	got, rel, err := m.EnsureServing(ctx, "m#1")
	if err != nil {
		t.Fatalf("EnsureServing(m#1) once instance 1 runs = %v, want it served", err)
	}
	defer rel()
	if got != ep {
		t.Errorf("EnsureServing(m#1) endpoint = %s, want instance 1's %s", got, ep)
	}

	ep0, rel0, err := m.EnsureServing(ctx, "m")
	if err != nil {
		t.Fatalf("EnsureServing(m) = %v, want instance 0 started and served", err)
	}
	defer rel0()
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning || endpointFor(st.Port) != ep0 {
		t.Errorf("instance 0 = %+v, want running behind %s", st, ep0)
	}
}

// TestEnsureServingQueuesBehindAStartingBurstInstance: a burst instance an
// ensure is starting has been admitted already, so a plain request for it waits
// for that start exactly as EnsureRunning does, and is served when it finishes.
func TestEnsureServingQueuesBehindAStartingBurstInstance(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec, openGate := gatedSpec(t, "S", "m")
	spec.Replicable = true
	spec.RAMMB = 64
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ensured := ensureAsync(ctx, m, "m#1")
	waitUntil(t, 5*time.Second, "instance 1 starting", func() bool {
		st := instanceStatus(m, "S", 1)
		return st != nil && st.State == StateStarting
	})
	served := servingAsync(ctx, m, "m#1")
	select {
	case r := <-served:
		t.Fatalf("EnsureServing(m#1) while instance 1 starts answered before the start finished: %+v, want it to wait", r)
	case <-time.After(300 * time.Millisecond):
	}

	openGate()
	r1, r2 := <-ensured, <-served
	if r1.err != nil || r2.err != nil {
		t.Fatalf("EnsureRunning / EnsureServing(m#1) = %v / %v, want both served once the start finished", r1.err, r2.err)
	}
	defer r1.release()
	defer r2.release()
	if r1.endpoint != r2.endpoint {
		t.Errorf("endpoints %s / %s, want one process serving both", r1.endpoint, r2.endpoint)
	}
}

// TestSiblingCascadeAsksEachHeldInstanceOnce: four protected instances, and
// room for three of them once the busy neighbour U is gone. Instances 1-3 are
// held back behind instance 0 (lowerSiblingWaiting). When instance 0 starts,
// the cascade re-asks them: each one that starts re-asks those above it in
// turn. Instance 3, which does not fit, must be decided ONCE in that turn --
// not once more at every level the cascade unwinds through, and not once more
// by the wake that U's exit runs after the cascade -- because every ask is a
// full admission decision, and each decision calls the VRAM measurer (a
// subprocess on a real host).
func TestSiblingCascadeAsksEachHeldInstanceOnce(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setIdleTick(t, time.Hour) // no housekeeping beat: every measurer call is an admission decision or a start's measurement
	m := newTestManager(t, allowlistPolicy())
	var (
		mu    sync.Mutex
		sizes []int // how many live processes each measurer call was asked about
	)
	m.SetMeasurer(func(pids []int) map[int]map[int]int {
		mu.Lock()
		sizes = append(sizes, len(pids))
		mu.Unlock()
		return nil
	})

	u := baseSpec("U", "u")
	u.GPUs = []SpecGPU{{Index: 0, VRAMMB: 3000}}
	budget := []GPUBudget{{Index: 0, BudgetMB: 3000}}
	m.Apply(Config{Specs: []Spec{u}, GPUBudgets: budget, ETag: "v1"})
	_, releaseU := ensureModel(t, m, "u") // U busy: not evictable while this is held

	s, _ := gatedSpec(t, "S", "m") // never healthy: no start finishes, so nothing re-asks instance 3 later
	s.Replicable = true
	s.RAMMB = 64
	s.MinInstances = 4
	s.StartupTimeoutSeconds = 60
	s.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	m.Apply(Config{Specs: []Spec{u, s}, GPUBudgets: budget, ETag: "v2"})
	for k := 0; k < 4; k++ {
		if st := mustInstance(t, m, "S", k); st.PID != 0 {
			t.Fatalf("instance %d = %+v, want it waiting: the premise is that U's room is taken", k, st)
		}
	}

	releaseU() // U idle: instance 0 evicts it, and starts in the turn U's exit is handled in
	waitUntil(t, 5*time.Second, "instances 0-2 started into U's room", func() bool {
		for k := 0; k < 3; k++ {
			if st := instanceStatus(m, "S", k); st == nil || st.PID == 0 {
				return false
			}
		}
		return true
	})
	if st := mustInstance(t, m, "S", 3); st.PID != 0 {
		t.Fatalf("instance 3 = %+v, want it waiting: the budget holds three instances", st)
	}

	// Instance 3 is the only candidate decided with three processes live:
	// instances 0-2 are, U is gone, and no start finishes behind the shut gate.
	mu.Lock()
	asked := 0
	for _, n := range sizes {
		if n == 3 {
			asked++
		}
	}
	mu.Unlock()
	if asked != 1 {
		t.Fatalf("instance 3 was decided %d times, want once: by the cascade, and neither again at each level it unwound through nor by the exit's own wake in the same turn", asked)
	}
}

// TestAWakeDecidesEachInstanceOnceInATurn: eight protected instances, seven of
// them held back behind instance 0, which a pinned neighbour of unknown demand
// refuses. One release -- one owner turn -- wakes them all: instance 0 is asked
// and refused, its refusal re-asks the held instances (a refused sibling holds
// no room to wait for), and each of those is refused in turn. Every decision
// builds a snapshot, which calls the VRAM measurer (a subprocess on a real
// host), so each instance must be decided once in that turn: the wake's own
// candidate list, collected before the cascade ran, still names instances 1-7,
// and asking them again would only repeat decisions whose inputs nothing in the
// turn has changed. Each keeps its own verdict.
func TestAWakeDecidesEachInstanceOnceInATurn(t *testing.T) {
	const instances = 8
	spec := replicableSpec("S", "m")
	spec.MinInstances = instances
	spec.AdmissionWaitTimeoutSeconds = 0 // no admission timer on a bare owner
	spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	pinned := baseSpec("P", "p")
	pinned.Pinned = true
	pinned.GPUs = []SpecGPU{{Index: 0}} // its demand on gpu 0 is unknown
	p := liveInstance(pinned, 0, 50)
	states := []*specState{p}
	for k := 0; k < instances; k++ {
		st := restingInstance(spec, k, StateStopped)
		st.waitingOnSibling = k > 0 // held back behind instance 0, which waits to start
		states = append(states, st)
	}
	o := instanceOwner(states...)
	o.policy = allowlistPolicy()
	o.allowedPairs = allowPairs("S", "P")
	o.rebuildUpstreamIndex()
	decisions := 0
	o.m.SetMeasurer(func([]int) map[int]map[int]int {
		decisions++ // the owner calls it synchronously, once per snapshot
		return nil
	})

	o.handle(cmdRelease{specID: "P", proc: p.proc}) // P idles on: the release wakes every waiting start

	if decisions != instances {
		t.Fatalf("%d admission decisions in one turn, want %d: one per instance", decisions, instances)
	}
	for k := 0; k < instances; k++ {
		if st := o.specs[InstanceKey("S", k)]; st.state != StatePendingVRAMUnknown || st.waitingOnSibling {
			t.Errorf("instance %d = %s (waitingOnSibling %v), want pending_vram_unknown, its own verdict", k, st.state, st.waitingOnSibling)
		}
	}
}

// TestHeldInstanceIsReaskedWhenItsLowerSiblingIsRefused: a protected instance
// held back behind a lower sibling that waits to start (lowerSiblingWaiting) is
// no longer held once that sibling is refused -- a refused sibling waits for no
// room -- and is re-asked at that moment, so a request queued on it gets its
// own verdict now rather than at an unrelated event or its admission timeout.
// The paths below ask the lower sibling alone: its crash-loop retry, a request
// naming it, and the dry run of its start.
func TestHeldInstanceIsReaskedWhenItsLowerSiblingIsRefused(t *testing.T) {
	newSpec := func() Spec {
		spec := replicableSpec("S", "m")
		spec.MinInstances = 2
		spec.AdmissionWaitTimeoutSeconds = 0 // no admission timer on a bare owner
		return spec
	}
	for _, tc := range []struct {
		name string
		// build returns the owner holding s0 and s1, and asks s0 alone.
		build func(spec Spec, s0, s1 *specState) (o *owner, ask func())
		// spec edits the spec before the instances are derived from it.
		spec      func(*Spec)
		s0State   State
		wantState State
		wantErr   error
	}{
		{
			name:    "crash-loop retry refused by the binary allowlist",
			s0State: StateBackoff,
			build: func(_ Spec, s0, s1 *specState) (*owner, func()) {
				o := instanceOwner(s0, s1)
				o.rebuildUpstreamIndex() // policy left empty: no binary is allowed
				return o, func() { o.handleBackoffFire("S") }
			},
			wantState: StateNotPermitted,
			wantErr:   ErrNotPermitted,
		},
		{
			name:    "a request for instance 0 refused by the binary allowlist",
			s0State: StateStopped,
			build: func(_ Spec, s0, s1 *specState) (*owner, func()) {
				o := instanceOwner(s0, s1)
				o.rebuildUpstreamIndex()
				return o, func() {
					o.handleEnsure(cmdEnsure{model: "m", reply: make(chan ensureOutcome, 1)})
				}
			},
			wantState: StateNotPermitted,
			wantErr:   ErrNotPermitted,
		},
		{
			name:    "the dry run of instance 0's start fails",
			s0State: StateBackoff,
			spec: func(spec *Spec) {
				spec.Env = map[string]string{"HF_TOKEN": "${AGENT_ENV:MISSING_TOKEN}"}
			},
			build: func(_ Spec, s0, s1 *specState) (*owner, func()) {
				o := instanceOwner(s0, s1)
				o.policy = allowlistPolicy()
				o.getenv = func(string) string { return "" }
				o.rebuildUpstreamIndex()
				return o, func() { o.handleBackoffFire("S") }
			},
			wantState: StateNotPermitted,
			wantErr:   ErrNotPermitted,
		},
		{
			name:    "crash-loop retry refused next to a pinned unknown-demand occupant",
			s0State: StateBackoff,
			spec: func(spec *Spec) {
				spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
			},
			build: func(_ Spec, s0, s1 *specState) (*owner, func()) {
				pinned := baseSpec("P", "p")
				pinned.Pinned = true
				pinned.GPUs = []SpecGPU{{Index: 0}} // its demand on gpu 0 is unknown
				o := instanceOwner(s0, s1, liveInstance(pinned, 0, 50))
				o.policy = allowlistPolicy()
				o.allowedPairs = allowPairs("S", "P")
				o.rebuildUpstreamIndex()
				return o, func() { o.handleBackoffFire("S") }
			},
			wantState: StatePendingVRAMUnknown,
			wantErr:   ErrAdmissionBlocked,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec := newSpec()
			if tc.spec != nil {
				tc.spec(&spec)
			}
			s0 := restingInstance(spec, 0, tc.s0State)
			s1 := restingInstance(spec, 1, StateStopped)
			s1.waitingOnSibling = true // held back behind instance 0, which waits to start
			reply := make(chan ensureOutcome, 1)
			s1.pending = []*ensureWaiter{{reply: reply, queuedAt: time.Now()}}
			_, ask := tc.build(spec, s0, s1)

			ask()

			if s0.state != tc.wantState {
				t.Fatalf("instance 0 = %s, want %s: the premise is that it is refused", s0.state, tc.wantState)
			}
			out, ok := answered(reply)
			if !ok {
				t.Fatalf("the request queued on instance 1 is still waiting (waitingOnSibling %v); want it re-asked the moment instance 0 was refused", s1.waitingOnSibling)
			}
			if !errors.Is(out.err, tc.wantErr) {
				t.Errorf("the request queued on instance 1 = %v, want its own verdict %v", out.err, tc.wantErr)
			}
			if s1.waitingOnSibling || s1.state != tc.wantState {
				t.Errorf("instance 1 = %s (waitingOnSibling %v), want %s on its own verdict", s1.state, s1.waitingOnSibling, tc.wantState)
			}
		})
	}
}

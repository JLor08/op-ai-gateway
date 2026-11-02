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
	pidT := statusFor(m, "T").PID

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
	m.Apply(Config{Specs: []Spec{spec}, GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 6000}}})

	_, rel0 := ensureModel(t, m, "m")
	defer rel0()
	waitUntil(t, 5*time.Second, "instance 0 measured at 3000 MB", func() bool {
		st := instanceStatus(m, "S", 0)
		return st != nil && st.MeasuredVRAM[0] == 3000
	})

	// 3000 (S) + 3000 (inherited) = 6000 fits the 6000 budget exactly.
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
	if s1 := instanceStatus(m, "S", 1); s1.State != StateStopped || s1.PID != 0 {
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
func TestFurtherInstanceWaitsForASiblingThatIsReady(t *testing.T) {
	const loadingMB, loadedMB = 1200, 3000
	for _, tc := range []struct {
		name  string
		vram  bool // the unknown demand is per-GPU VRAM (else host RAM)
		burst bool // min_instances 1, so instance 1 is burst (else 2: protected)
	}{
		{"protected, vram unknown", true, false},
		{"protected, ram unknown", false, false},
		{"burst, vram unknown", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skipOnWindows(t)
			shrinkTimings(t)
			m := newTestManager(t, allowlistPolicy())

			var loaded atomic.Bool
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
			cfg := Config{}
			if tc.vram {
				spec.GPUs = []SpecGPU{{Index: 0, VRAMMB: 0}}
				spec.RAMMB = 64
				cfg.GPUBudgets = []GPUBudget{{Index: 0, BudgetMB: 8000}}
				m.SetMeasurer(func(pids []int) map[int]map[int]int {
					out := make(map[int]map[int]int, len(pids))
					for _, p := range pids {
						out[p] = map[int]int{0: reading()}
					}
					return out
				})
			} else {
				spec.RAMMB = 0
				m.SetRAMMeasurer(func(pids []int) map[int]int {
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
			waitUntil(t, 5*time.Second, "instance 0 ready", func() bool {
				st := instanceStatus(m, "S", 0)
				return st != nil && st.State == StateRunning
			})
			if tc.burst {
				_, release := ensureModel(t, m, "m#1")
				release()
				return
			}
			waitUntil(t, 5*time.Second, "instance 1 started once its sibling was measured ready", func() bool {
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
			violation = "instance 1 left running while instance 2 still exists"
		}
		if has0 && s0.State != StateRunning && (has1 || has2) && violation == "" {
			violation = "instance 0 left running while a higher instance still exists"
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
				pids[k] = instanceStatus(m, "S", k).PID
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
	if st := instanceStatus(m, "S", 1); st.LastError == nil {
		t.Errorf("crashed row has no LastError; it is kept precisely so the operator can read why")
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
	pid0 := instanceStatus(m, "S", 0).PID
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
	pid1 := instanceStatus(m, "S", 1).PID

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
	pid0, pid1 := instanceStatus(m, "S", 0).PID, instanceStatus(m, "S", 1).PID

	// An identical push (new ETag) and a metadata-only edit: neither may
	// bounce an instance. A drain would be visible the moment Apply returns.
	meta := spec
	meta.IdleTimeoutSeconds = 30
	for i, s := range []Spec{spec, meta} {
		m.Apply(Config{Specs: []Spec{s}, ETag: "v" + string(rune('2'+i))})
		s0, s1 := instanceStatus(m, "S", 0), instanceStatus(m, "S", 1)
		if s0.State != StateRunning || s0.PID != pid0 || s1.State != StateRunning || s1.PID != pid1 {
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
	if st := statusFor(m, "S"); st.StartupMS != 0 {
		t.Fatalf("StartupMS = %d before any start, want 0 (unknown)", st.StartupMS)
	}

	_, release := ensureModel(t, m, "m")
	defer release()
	if st := statusFor(m, "S"); st.StartupMS < 300 {
		t.Fatalf("StartupMS = %d, want >= 300 (the child answers /health only after 300ms)", st.StartupMS)
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
// replicates (Ruling R1) -- instance 2 of F can never be addressed. F's floor
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
	pid0, pid1 := instanceStatus(m, "F", 0).PID, instanceStatus(m, "F", 1).PID
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

// hasLogBuffer reports whether the manager's log store retains a buffer for
// key (an instance key).
func hasLogBuffer(m *Manager, key string) bool {
	m.logs.mu.Lock()
	defer m.logs.mu.Unlock()
	_, ok := m.logs.logs[key]
	return ok
}

// TestApplyRetainsLogsOfExistingInstances: an instance's output buffer is
// keyed by its instance key and kept by every Apply while the instance exists;
// once a burst instance has been retired, the next Apply releases it.
func TestApplyRetainsLogsOfExistingInstances(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := replicableSpec("S", "m")
	spec.IdleTimeoutSeconds = 1
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v1"})

	_, release := ensureModel(t, m, "m#1")
	release()
	if !hasLogBuffer(m, "S#1") {
		t.Fatal("instance 1 has no log buffer of its own under its instance key S#1")
	}
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v2"})
	if !hasLogBuffer(m, "S#1") {
		t.Fatal("an Apply dropped the log buffer of an instance that still exists")
	}

	waitUntil(t, 5*time.Second, "instance 1 retired after its idle unload", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
	m.Apply(Config{Specs: []Spec{spec}, ETag: "v3"})
	if hasLogBuffer(m, "S#1") {
		t.Fatal("the next Apply kept the log buffer of a retired instance")
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Tests for the transient stop of one instance (Manager.Stop, the router's
// POST /stop/{model}): every answer it gives, that it persists nothing, that it
// goes highest index first and never touches the protected floor, and that a
// burst instance it stops is retired once its process has exited.

// stopOwner is an owner-level fixture for stopInstance: the given states, the
// stub child allowlisted and the routing index built, so an address resolves
// without a run loop. A live state's exited channel is already closed, so
// beginDrain is a pure state transition (terminateNow returns at once).
func stopOwner(states ...*specState) *owner {
	o := instanceOwner(states...)
	o.policy = allowlistPolicy()
	o.rebuildUpstreamIndex()
	return o
}

// instanceStateOf returns the State of instance k of the base spec id, and whether the
// owner holds a state for it at all.
func instanceStateOf(o *owner, id string, k int) (State, bool) {
	st := o.specs[InstanceKey(id, k)]
	if st == nil {
		return "", false
	}
	return st.state, true
}

// TestStopInstanceAnswers covers every answer the owner gives a stop, with the
// state each one leaves behind: a refusal or a no-op changes nothing, and a
// stop that starts a drain moves exactly the named instance to draining.
func TestStopInstanceAnswers(t *testing.T) {
	repl := replicableSpec("S", "m")
	repl.MaxInstances = 4

	min2 := repl
	min2.MinInstances = 2

	pinned := baseSpec("S", "m")
	pinned.Pinned = true

	forceRunning := baseSpec("S", "m")
	forceRunning.AdminState = "force_running"

	forceStopped := repl
	forceStopped.MinInstances = 2
	forceStopped.AdminState = "force_stopped"

	starting := func(spec Spec, k int) *specState {
		st := liveInstance(spec, k, 100+k)
		st.state = StateStarting
		st.proc.everHealthy = false
		return st
	}
	draining := func(spec Spec, k int) *specState {
		st := liveInstance(spec, k, 100+k)
		st.state = StateDraining
		return st
	}

	for _, tc := range []struct {
		name      string
		states    []*specState
		model     string
		wantState StopState
		wantErr   error
		// wantAfter is the state of each instance after the stop; absent means
		// the owner must hold no state for that instance.
		wantAfter map[int]State
	}{
		{
			name:      "unknown address",
			states:    []*specState{liveInstance(repl, 0, 100)},
			model:     "ghost",
			wantErr:   ErrModelNotManaged,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "index above max_instances",
			states:    []*specState{liveInstance(repl, 0, 100)},
			model:     "m#4",
			wantErr:   ErrModelNotManaged,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "instance address of a spec that does not replicate",
			states:    []*specState{liveInstance(baseSpec("S", "m"), 0, 100)},
			model:     "m#1",
			wantErr:   ErrModelNotManaged,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "highest running instance drains",
			states:    []*specState{liveInstance(repl, 0, 100), liveInstance(repl, 1, 101), liveInstance(repl, 2, 102)},
			model:     "m#2",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateRunning, 1: StateRunning, 2: StateDraining},
		},
		{
			name:      "instance 0 of a spec that does not replicate drains",
			states:    []*specState{liveInstance(baseSpec("S", "m"), 0, 100)},
			model:     "m",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateDraining},
		},
		{
			name:      "a starting instance drains",
			states:    []*specState{liveInstance(repl, 0, 100), starting(repl, 1)},
			model:     "m#1",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateRunning, 1: StateDraining},
		},
		{
			name:      "an instance already draining is answered stopping again",
			states:    []*specState{liveInstance(repl, 0, 100), draining(repl, 1)},
			model:     "m#1",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateRunning, 1: StateDraining},
		},
		{
			name:      "an instance with no process is already stopped",
			states:    []*specState{restingInstance(repl, 0, StateStopped)},
			model:     "m",
			wantState: StopStopped,
			wantAfter: map[int]State{0: StateStopped},
		},
		{
			name:      "an allowed instance that does not exist is stopped, and is not created",
			states:    []*specState{liveInstance(repl, 0, 100)},
			model:     "m#3",
			wantState: StopStopped,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "an instance resting at a crash is stopped",
			states:    []*specState{liveInstance(repl, 0, 100), restingInstance(repl, 1, StateCrashed)},
			model:     "m#1",
			wantState: StopStopped,
			wantAfter: map[int]State{0: StateRunning, 1: StateCrashed},
		},
		{
			name:      "a lower instance is refused while a higher one runs",
			states:    []*specState{liveInstance(repl, 0, 100), liveInstance(repl, 1, 101), liveInstance(repl, 2, 102)},
			model:     "m#1",
			wantErr:   ErrInstanceNotHighest,
			wantAfter: map[int]State{0: StateRunning, 1: StateRunning, 2: StateRunning},
		},
		{
			name:      "a lower instance is refused while a higher one is still starting",
			states:    []*specState{liveInstance(repl, 0, 100), starting(repl, 1)},
			model:     "m",
			wantErr:   ErrInstanceNotHighest,
			wantAfter: map[int]State{0: StateRunning, 1: StateStarting},
		},
		{
			name:      "a lower instance is refused while a higher one is still draining",
			states:    []*specState{liveInstance(repl, 0, 100), draining(repl, 1)},
			model:     "m",
			wantErr:   ErrInstanceNotHighest,
			wantAfter: map[int]State{0: StateRunning, 1: StateDraining},
		},
		{
			name:      "a higher instance with no process does not hold a lower one",
			states:    []*specState{liveInstance(repl, 0, 100), liveInstance(repl, 1, 101), restingInstance(repl, 2, StateCrashed)},
			model:     "m#1",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateRunning, 1: StateDraining, 2: StateCrashed},
		},
		{
			name:      "an instance inside min_instances is protected",
			states:    []*specState{liveInstance(min2, 0, 100), liveInstance(min2, 1, 101)},
			model:     "m#1",
			wantErr:   ErrInstanceProtected,
			wantAfter: map[int]State{0: StateRunning, 1: StateRunning},
		},
		{
			name:      "a protected instance with no process is still refused",
			states:    []*specState{liveInstance(min2, 0, 100), restingInstance(min2, 1, StateBackoff)},
			model:     "m#1",
			wantErr:   ErrInstanceProtected,
			wantAfter: map[int]State{0: StateRunning, 1: StateBackoff},
		},
		{
			name:      "the first instance above min_instances may stop",
			states:    []*specState{liveInstance(min2, 0, 100), liveInstance(min2, 1, 101), liveInstance(min2, 2, 102)},
			model:     "m#2",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateRunning, 1: StateRunning, 2: StateDraining},
		},
		{
			name:      "a pinned spec's instance 0 is protected",
			states:    []*specState{liveInstance(pinned, 0, 100)},
			model:     "m",
			wantErr:   ErrInstanceProtected,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "a force_running spec's instance 0 is protected",
			states:    []*specState{liveInstance(forceRunning, 0, 100)},
			model:     "m",
			wantErr:   ErrInstanceProtected,
			wantAfter: map[int]State{0: StateRunning},
		},
		{
			name:      "force_stopped: a draining instance inside the floor is answered stopping",
			states:    []*specState{draining(forceStopped, 0), draining(forceStopped, 1)},
			model:     "m",
			wantState: StopStopping,
			wantAfter: map[int]State{0: StateDraining, 1: StateDraining},
		},
		{
			name:      "force_stopped: an instance with no process is stopped",
			states:    []*specState{restingInstance(forceStopped, 0, StateStopped), restingInstance(forceStopped, 1, StateStopped)},
			model:     "m#1",
			wantState: StopStopped,
			wantAfter: map[int]State{0: StateStopped, 1: StateStopped},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := stopOwner(tc.states...)
			out := o.stopInstance(tc.model)
			if !errors.Is(out.err, tc.wantErr) || (tc.wantErr == nil && out.err != nil) {
				t.Fatalf("stop(%q) error = %v, want %v", tc.model, out.err, tc.wantErr)
			}
			if out.state != tc.wantState {
				t.Errorf("stop(%q) state = %q, want %q", tc.model, out.state, tc.wantState)
			}
			for k := 0; k <= MaxInstanceIndex; k++ {
				got, has := instanceStateOf(o, "S", k)
				want, wantHas := tc.wantAfter[k]
				if has != wantHas || got != want {
					t.Errorf("instance %d after stop(%q) = (%q, held %v), want (%q, held %v)", k, tc.model, got, has, want, wantHas)
				}
			}
		})
	}
}

// TestStopInstanceRefusesARemovedSpec: a spec the last push removed is still in
// the table while it drains, but it is no longer the operator's to act on, so a
// stop of it is answered as for an unknown model.
func TestStopInstanceRefusesARemovedSpec(t *testing.T) {
	st := liveInstance(baseSpec("S", "m"), 0, 100)
	st.state = StateDraining
	st.removed = true
	o := stopOwner(st)
	if out := o.stopInstance("m"); !errors.Is(out.err, ErrModelNotManaged) {
		t.Fatalf("stop of a removed spec = %+v, want ErrModelNotManaged", out)
	}
}

// TestStopInstanceRefusedWhileClosing: once Close has begun, a stop is answered
// ErrManagerClosed and starts nothing.
func TestStopInstanceRefusedWhileClosing(t *testing.T) {
	o := stopOwner(liveInstance(baseSpec("S", "m"), 0, 100))
	o.closing = true
	if out := o.stopInstance("m"); !errors.Is(out.err, ErrManagerClosed) {
		t.Fatalf("stop while closing = %+v, want ErrManagerClosed", out)
	}
	if got, _ := instanceStateOf(o, "S", 0); got != StateRunning {
		t.Fatalf("instance 0 = %q after a refused stop, want still running", got)
	}
}

// stopModel is Manager.Stop with a bounded context, failing the test on an
// error.
func stopModel(t *testing.T, m *Manager, model string) StopState {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	state, err := m.Stop(ctx, model)
	if err != nil {
		t.Fatalf("Stop(%q): %v", model, err)
	}
	return state
}

// TestStopRetiresABurstInstanceOnceItHasExited runs on the owner goroutine
// with a real child: a stopped burst instance drains, its process exits, and
// its row leaves the table, as after an idle unload. A second stop of the same
// address is answered stopped -- the stop is idempotent -- and instance 0 is
// untouched throughout.
func TestStopRetiresABurstInstanceOnceItHasExited(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})
	for _, model := range []string{"m", "m#1"} {
		_, release := ensureModel(t, m, model)
		release()
	}
	pid0 := mustInstance(t, m, "S", 0).PID

	if got := stopModel(t, m, "m#1"); got != StopStopping {
		t.Fatalf("Stop(m#1) = %q, want stopping", got)
	}
	waitUntil(t, 5*time.Second, "the stopped burst instance retired after its exit", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
	if got := stopModel(t, m, "m#1"); got != StopStopped {
		t.Fatalf("Stop(m#1) again = %q, want stopped (idempotent)", got)
	}
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning || st.PID != pid0 {
		t.Fatalf("instance 0 = %+v, want still running as pid %d", st, pid0)
	}
}

// TestStopPersistsNothing: stopping instance 0 of an ordinary spec drains it to
// stopped and leaves its row -- instance 0 always exists -- with nothing
// recorded that keeps it down: the next request starts it again, exactly as
// after an idle unload.
func TestStopPersistsNothing(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{baseSpec("S", "m")}})
	_, release := ensureModel(t, m, "m")
	release()

	if got := stopModel(t, m, "m"); got != StopStopping {
		t.Fatalf("Stop(m) = %q, want stopping", got)
	}
	waitUntil(t, 5*time.Second, "instance 0 stopped", func() bool {
		st := instanceStatus(m, "S", 0)
		return st != nil && st.State == StateStopped && st.PID == 0
	})
	if got := stopModel(t, m, "m"); got != StopStopped {
		t.Fatalf("Stop(m) again = %q, want stopped (idempotent)", got)
	}
	// A later look, after several housekeeping beats: nothing restarted it.
	time.Sleep(150 * time.Millisecond)
	if st := mustInstance(t, m, "S", 0); st.State != StateStopped {
		t.Fatalf("instance 0 = %+v after the stop, want it to rest at stopped with no request", st)
	}

	_, release = ensureModel(t, m, "m")
	release()
	if st := mustInstance(t, m, "S", 0); st.State != StateRunning {
		t.Fatalf("instance 0 = %+v after a request, want running: a stop must not keep it down", st)
	}
}

// TestStopWaitsForInFlightRequests: a stop drains gracefully -- an instance
// serving a request stays draining until the request is released, and only
// then exits.
func TestStopWaitsForInFlightRequests(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setDrainGrace(t, 10*time.Second)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})
	_, release := ensureModel(t, m, "m")
	release()
	_, inFlight := ensureModel(t, m, "m#1") // held: one request in flight

	if got := stopModel(t, m, "m#1"); got != StopStopping {
		t.Fatalf("Stop(m#1) = %q, want stopping", got)
	}
	hold := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(hold) {
		if st := instanceStatus(m, "S", 1); st == nil || st.State != StateDraining || st.PID == 0 {
			t.Fatalf("instance 1 = %+v while a request is in flight, want draining with its process", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
	inFlight()
	waitUntil(t, 5*time.Second, "instance 1 retired once its request was released", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
}

// TestConcurrentStopsDrainOnce races many stops of one instance, and of its
// lower sibling, against each other on the owner goroutine (run under -race):
// every stop of the highest instance is answered stopping or stopped, every
// stop of the lower one is answered stopping (after the higher one left),
// stopped, or refused as not the highest -- never anything else -- and the
// burst instance is retired exactly once.
func TestConcurrentStopsDrainOnce(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	m.Apply(Config{Specs: []Spec{replicableSpec("S", "m")}})
	for _, model := range []string{"m", "m#1"} {
		_, release := ensureModel(t, m, model)
		release()
	}

	type answer struct {
		model string
		state StopState
		err   error
	}
	const n = 16
	answers := make(chan answer, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		for _, model := range []string{"m#1", "m"} {
			wg.Add(1)
			go func(model string) {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				state, err := m.Stop(ctx, model)
				answers <- answer{model: model, state: state, err: err}
			}(model)
		}
	}
	wg.Wait()
	close(answers)
	for a := range answers {
		switch {
		case a.model == "m#1" && a.err == nil && (a.state == StopStopping || a.state == StopStopped):
		case a.model == "m" && a.err == nil && (a.state == StopStopping || a.state == StopStopped):
		case a.model == "m" && errors.Is(a.err, ErrInstanceNotHighest):
		default:
			t.Errorf("Stop(%s) = (%q, %v), want stopping, stopped, or (for m) not_highest", a.model, a.state, a.err)
		}
	}
	waitUntil(t, 5*time.Second, "instance 1 retired", func() bool {
		return instanceStatus(m, "S", 1) == nil
	})
}

// TestStopAfterCloseIsRefused: a stop after Close returns ErrManagerClosed
// rather than blocking.
func TestStopAfterCloseIsRefused(t *testing.T) {
	m := newTestManager(t, allowlistPolicy())
	m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := m.Stop(ctx, "m"); !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("Stop after Close = %v, want ErrManagerClosed", err)
	}
}

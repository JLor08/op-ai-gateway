// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for the VRAM of wsl instances (#381): Windows charges all of the WSL
// VM's GPU memory to its worker process (vmwp), so a workload's figure is the
// growth of the VM's figure across its start -- one start at a time -- and
// whatever the VM holds beyond the attributed figures is charged to the GPU as
// unattributed VRAM.

// vmwpPID stands in for the WSL VM's worker process in these tests.
const vmwpPID = 7777

// vmVRAMScript is a scripted VRAM measurer: it answers the VM worker with the
// figure set last and nothing for any other pid, and records every pid list it
// is asked. hold makes its next call wait for the returned channel to close,
// answering the figure that was current when the call began. noWorker makes
// the finder it installs name no worker at all.
type vmVRAMScript struct {
	mu       sync.Mutex
	vm       map[int]int
	asked    [][]int
	gate     chan struct{}
	noWorker bool
}

func (s *vmVRAMScript) setNoWorker(none bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noWorker = none
}

func (s *vmVRAMScript) find() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noWorker {
		return nil
	}
	return []int{vmwpPID}
}

func (s *vmVRAMScript) set(vm map[int]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vm = vm
}

func (s *vmVRAMScript) hold() chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gate = make(chan struct{})
	return s.gate
}

func (s *vmVRAMScript) measure(pids []int) map[int]map[int]int {
	s.mu.Lock()
	s.asked = append(s.asked, slices.Clone(pids))
	vm, gate := maps.Clone(s.vm), s.gate
	s.gate = nil
	s.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if !slices.Contains(pids, vmwpPID) || len(vm) == 0 {
		return nil
	}
	return map[int]map[int]int{vmwpPID: vm}
}

// askedFor returns the recorded calls whose pid list holds pid.
func (s *vmVRAMScript) askedFor(pid int) [][]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out [][]int
	for _, pids := range s.asked {
		if slices.Contains(pids, pid) {
			out = append(out, pids)
		}
	}
	return out
}

// install makes s the manager's VRAM measurer and names vmwpPID as the VM's
// worker.
func (s *vmVRAMScript) install(m *Manager) {
	m.SetMeasurer(s.measure)
	m.SetWSLVMPIDs(s.find)
}

// wslVRAMOwner is a hand-driven owner with the states given, the scripted
// measurer installed, and a command buffer the tests read the measurements
// from.
func wslVRAMOwner(vm *vmVRAMScript, states ...*specState) *owner {
	m := &Manager{cmds: make(chan command, 16), done: make(chan struct{})}
	vm.install(m)
	o := &owner{m: m, specs: map[string]*specState{}}
	for _, st := range states {
		o.specs[st.spec.ID] = st
	}
	return o
}

// wslVRAMSpec is a wsl spec on GPU 0 with known demands.
func wslVRAMSpec(id string, vramMB int) Spec {
	s := wslCandidate(id, 1000)
	s.UpstreamModel = strings.ToLower(id)
	s.GPUs = []SpecGPU{{Index: 0, VRAMMB: vramMB}}
	return s
}

// liveWSL is a live generation of a wsl spec for an owner driven by hand.
func liveWSL(spec Spec, pid int, state State) *specState {
	return &specState{spec: spec, state: state, proc: &runningProc{
		specID: spec.ID, pid: pid, log: NewLogStore(0, 0).newProc(spec.ID), exited: make(chan struct{}),
		everHealthy: state == StateRunning,
		wslUnit:     "opagent-cafe0001-" + spec.ID + "-1", wslDistro: wslTestDistro,
	}}
}

// nextCmd is the next command the owner's goroutines posted.
func nextCmd(t *testing.T, o *owner) command {
	t.Helper()
	select {
	case c := <-o.m.cmds:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no command was posted")
		return nil
	}
}

// measureNow runs one measurement through the owner by hand: the dispatch,
// then its answer.
func measureNow(t *testing.T, o *owner) {
	t.Helper()
	o.dispatchMeasurement()
	handleNextMeasurement(t, o)
}

// handleNextMeasurement hands the owner its commands up to and including the
// next measurement's answer.
func handleNextMeasurement(t *testing.T, o *owner) {
	t.Helper()
	for {
		c := nextCmd(t, o)
		o.handle(c)
		if _, ok := c.(cmdMeasured); ok {
			return
		}
	}
}

// pumpOwner handles the hand-driven owner's commands until done holds.
func pumpOwner(t *testing.T, o *owner, what string, done func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !done() {
		select {
		case c := <-o.m.cmds:
			o.handle(c)
		case <-deadline:
			t.Fatalf("timed out waiting for: %s", what)
		}
	}
}

// stopHandDriven ends a hand-driven owner's live processes and waits for every
// goroutine it started.
func stopHandDriven(t *testing.T, o *owner) {
	t.Helper()
	for _, st := range o.specs {
		if st.proc != nil && st.proc.cmd != nil && st.proc.cmd.Process != nil {
			st.proc.cmd.Process.Kill() //nolint:errcheck // already gone is fine
		}
	}
	close(o.m.done)
	o.m.wg.Wait()
}

func TestSumVMVRAM(t *testing.T) {
	byPID := map[int]map[int]int{
		10:  {0: 12318},
		11:  {0: 7432, 1: 22},
		500: {0: 9999}, // a managed native process: not the VM's
	}
	if got, want := sumVMVRAM(byPID, []int{10, 11}), map[int]int{0: 19750, 1: 22}; !maps.Equal(got, want) {
		t.Errorf("sumVMVRAM = %v, want %v", got, want)
	}
	// A worker named twice is summed once; one the measurer said nothing about
	// adds nothing; a non-positive figure is no figure.
	if got, want := sumVMVRAM(map[int]map[int]int{10: {0: 100, 1: 0}}, []int{10, 10, 12}), map[int]int{0: 100}; !maps.Equal(got, want) {
		t.Errorf("sumVMVRAM with a repeated, a silent and a zero entry = %v, want %v", got, want)
	}
	if got := sumVMVRAM(nil, []int{10}); len(got) != 0 {
		t.Errorf("sumVMVRAM of no answer = %v, want empty", got)
	}
}

func TestWSLDelta(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		after, baseline, credit map[int]int
		want                    map[int]int
	}{
		{"growth", map[int]int{0: 19772}, map[int]int{0: 12340}, map[int]int{}, map[int]int{0: 7432}},
		{"an attributed exit added back", map[int]int{0: 7454}, map[int]int{0: 12340}, map[int]int{0: 12318}, map[int]int{0: 7432}},
		{"a shrink is dropped", map[int]int{0: 100, 1: 900}, map[int]int{0: 500, 1: 400}, nil, map[int]int{1: 500}},
		{"a gpu only after", map[int]int{2: 3000}, nil, nil, map[int]int{2: 3000}},
		{"no change", map[int]int{0: 22}, map[int]int{0: 22}, nil, nil},
	} {
		if got := wslDelta(tc.after, tc.baseline, tc.credit); !maps.Equal(got, tc.want) {
			t.Errorf("%s: wslDelta = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// newWSLExecOwner is a hand-driven owner that really launches wsl specs through
// the fake: the configured user's uid is already known, and nothing runs the
// owner's loop -- the test hands it each command (pumpOwner).
func newWSLExecOwner(t *testing.T, f *fakeWSL, specs ...Spec) *owner {
	t.Helper()
	pol := allowlistPolicy()
	pol.WSL = wslTestPolicy()
	m := &Manager{cmds: make(chan command, 64), done: make(chan struct{}), logs: NewLogStore(0, 0)}
	o := &owner{
		m:         m,
		policy:    pol,
		getenv:    func(string) string { return "" },
		cfg:       Config{Specs: specs},
		specs:     map[string]*specState{},
		wslRunner: f.runner(),
		wslNonce:  "cafe0001",
		wslUIDs:   map[string]int{wslTestDistro: 1000},
		logs:      m.logs,
	}
	for _, s := range specs {
		o.specs[s.ID] = &specState{spec: s, state: StateStopped}
	}
	t.Cleanup(func() { stopHandDriven(t, o) })
	return o
}

// TestWSLStartDeltaAttributesVMGrowth: a wsl start opens a window whose
// baseline is the VM's figure just before the exec, and the first measurement
// dispatched after its first passing probe closes it: the VM's growth is the
// instance's measured VRAM. A measurement that was already out when the probe
// passed (it caught the load midway) does not close it. No native spec
// declares a GPU here, and the VM worker is measured all the same; the wsl.exe
// pid never is.
func TestWSLStartDeltaAttributesVMGrowth(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	s := wslTestSpec(f, "S", "m")
	s.Pinned = true
	s.Args = stubArgs(300*time.Millisecond, 0, 0, "")
	s.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	o := newWSLExecOwner(t, f, s)
	vm := &vmVRAMScript{vm: map[int]int{0: 22}}
	vm.install(o.m)
	st := o.specs["S"]

	o.admitAndStart("S")
	if st.proc == nil {
		t.Fatalf("S did not start: state %s, last_error %+v", st.state, st.lastError)
	}
	proc := st.proc
	w := o.wslWindow
	if w == nil || w.proc != proc {
		t.Fatalf("window = %+v, want one open for S's generation", w)
	}
	if !maps.Equal(w.baseline, map[int]int{0: 22}) {
		t.Errorf("baseline = %v, want the VM's figure before the exec {0:22}", w.baseline)
	}

	// A measurement goes out while S is still loading, and is still out when
	// S's first probe passes: the measurement that probe asks for waits.
	vm.set(map[int]int{0: 5000})
	gate := vm.hold()
	o.dispatchMeasurement()
	pumpOwner(t, o, "S's first passing probe", func() bool { return proc.everHealthy })
	if !o.remeasure {
		t.Fatal("the probe's measurement was not deferred behind the outstanding one")
	}
	vm.set(map[int]int{0: 12340})
	close(gate)
	handleNextMeasurement(t, o)
	if o.wslWindow == nil {
		t.Fatalf("a measurement dispatched before the first passing probe closed the window: S measured %v", st.measuredVRAM)
	}
	pumpOwner(t, o, "the window closes", func() bool { return o.wslWindow == nil })

	if want := map[int]int{0: 12318}; !maps.Equal(st.measuredVRAM, want) || !maps.Equal(o.wslAttrib[proc], want) {
		t.Errorf("S measured %v, attributed %v, want %v", st.measuredVRAM, o.wslAttrib[proc], want)
	}
	if got := o.snapshotStatus()[0].MeasuredVRAM; !maps.Equal(got, map[int]int{0: 12318}) {
		t.Errorf("Status.MeasuredVRAM = %v, want {0:12318}", got)
	}
	if !st.vramBootstrapDone {
		t.Error("the attribution did not resolve S's VRAM bootstrap")
	}
	if len(vm.askedFor(proc.pid)) != 0 {
		t.Errorf("the wsl.exe pid %d reached the VRAM measurer: %v", proc.pid, vm.askedFor(proc.pid))
	}
	if n := len(vm.askedFor(vmwpPID)); n < 3 {
		t.Errorf("the VM worker was measured %d times, want the baseline and both measurements", n)
	}

	// An exit deletes the attribution with the process. Unpinned and drained
	// first, so the exit rests S rather than restarting it.
	st.spec.Pinned, st.state, st.intentionalStop = false, StateDraining, true
	proc.cmd.Process.Kill() //nolint:errcheck // the test's own child
	pumpOwner(t, o, "S's exit", func() bool { return st.proc == nil })
	if _, ok := o.wslAttrib[proc]; ok {
		t.Error("S's attribution outlived its process")
	}
}

// TestWSLExitDuringWindowCorrectsDelta: an attributed wsl process that exits
// during another start's window has its attribution added back, so the VM's
// shrink does not come off the new instance's figure.
func TestWSLExitDuringWindowCorrectsDelta(t *testing.T) {
	vm := &vmVRAMScript{}
	a := liveWSL(wslVRAMSpec("A", 6000), 300, StateRunning)
	b := liveWSL(wslVRAMSpec("B", 6000), 301, StateStarting)
	o := wslVRAMOwner(vm, a, b)
	o.wslAttrib = map[*runningProc]map[int]int{a.proc: {0: 12318}}
	a.measuredVRAM = map[int]int{0: 12318}
	o.wslWindow = &wslStartWindow{proc: b.proc, baseline: map[int]int{0: 12340}}

	a.state, a.intentionalStop = StateDraining, true
	o.onProcExited(a, nil)
	if o.wslWindow == nil || o.wslWindow.void || !maps.Equal(o.wslWindow.exitedCredit, map[int]int{0: 12318}) {
		t.Fatalf("window after A's exit = %+v, want it open with A's attribution as credit", o.wslWindow)
	}
	if _, ok := o.wslAttrib[a.proc]; ok {
		t.Error("A's attribution outlived its process")
	}

	b.proc.everHealthy, b.state = true, StateRunning
	vm.set(map[int]int{0: 7454})
	measureNow(t, o)
	if o.wslWindow != nil {
		t.Fatal("the window is still open after B's settling measurement")
	}
	if want := map[int]int{0: 7432}; !maps.Equal(b.measuredVRAM, want) || !maps.Equal(o.wslAttrib[b.proc], want) {
		t.Errorf("B measured %v, attributed %v, want %v", b.measuredVRAM, o.wslAttrib[b.proc], want)
	}
}

// TestWSLExitOfUnattributedProcVoidsWindow: a wsl process with no attribution
// exits during a window, so what it held cannot be added back: the window's
// instance gets no attribution and is covered by the unattributed remainder.
// The window's own process exiting closes it without one, and a native exit
// changes nothing.
func TestWSLExitOfUnattributedProcVoidsWindow(t *testing.T) {
	vm := &vmVRAMScript{}
	a := liveWSL(wslVRAMSpec("A", 6000), 300, StateRunning)
	b := liveWSL(wslVRAMSpec("B", 6000), 301, StateStarting)
	n := liveWSL(Spec{ID: "N", UpstreamModel: "n", RAMMB: 100, GPUs: []SpecGPU{{Index: 0, VRAMMB: 500}}}, 302, StateRunning)
	n.proc.wslUnit, n.proc.wslDistro = "", ""
	o := wslVRAMOwner(vm, a, b, n)
	o.wslWindow = &wslStartWindow{proc: b.proc, baseline: map[int]int{0: 12340}}

	n.state, n.intentionalStop = StateDraining, true
	o.onProcExited(n, nil)
	if w := o.wslWindow; w == nil || w.void || len(w.exitedCredit) != 0 {
		t.Fatalf("a native exit touched the window: %+v", w)
	}

	a.state, a.intentionalStop = StateDraining, true
	o.onProcExited(a, nil)
	if o.wslWindow == nil || !o.wslWindow.void {
		t.Fatalf("window after an unattributed exit = %+v, want it open and void", o.wslWindow)
	}

	b.proc.everHealthy, b.state = true, StateRunning
	vm.set(map[int]int{0: 7454})
	measureNow(t, o)
	if o.wslWindow != nil {
		t.Fatal("a void window is still open after its settling measurement")
	}
	if _, ok := o.wslAttrib[b.proc]; ok || b.measuredVRAM != nil {
		t.Errorf("B got an attribution from a void window: %v / %v", o.wslAttrib[b.proc], b.measuredVRAM)
	}
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 7454}) {
		t.Errorf("UnattributedVRAM = %v, want B's share in the remainder {0:7454}", got)
	}

	t.Run("the window's own exit closes it without an attribution", func(t *testing.T) {
		c := liveWSL(wslVRAMSpec("C", 6000), 303, StateStarting)
		o.specs["C"] = c
		o.wslWindow = &wslStartWindow{proc: c.proc, baseline: map[int]int{}}
		o.onProcExited(c, errors.New("exit status 1"))
		cancelTimer(o.m, &c.backoffTimer)
		if o.wslWindow != nil {
			t.Errorf("window = %+v after its own process exited, want closed", o.wslWindow)
		}
		if _, ok := o.wslAttrib[c.proc]; ok {
			t.Error("the window's exited process was attributed")
		}
	})
	close(o.m.done)
	o.m.wg.Wait()
}

// TestRemainderChargedAsUnattributedVRAM: what the VM holds beyond the
// attributed figures is charged to each GPU as occupied -- in the snapshot,
// from the reading taken at admission, and in rule 3's sum -- and is never a
// victim and never a reason to refuse a demand that fits its budget alone.
func TestRemainderChargedAsUnattributedVRAM(t *testing.T) {
	vm := &vmVRAMScript{vm: map[int]int{0: 20000}}
	s := liveWSL(wslVRAMSpec("S", 6000), 300, StateRunning)
	o := wslVRAMOwner(vm, s)
	o.wslAttrib = map[*runningProc]map[int]int{s.proc: {0: 12000}}

	snap := o.buildSnapshot()
	if !maps.Equal(snap.UnattributedVRAM, map[int]int{0: 8000}) {
		t.Errorf("UnattributedVRAM = %v, want {0:8000}", snap.UnattributedVRAM)
	}
	if got := runningRow(t, snap, "S").GPUs; !maps.Equal(got, map[int]int{0: 12000}) {
		t.Errorf("S charged %v, want max(attribution, demand) = {0:12000}", got)
	}
	if snap.WSLStartInFlight {
		t.Error("WSLStartInFlight with no window open")
	}
	if len(vm.askedFor(vmwpPID)) != 1 {
		t.Errorf("the admission read the VM %d times, want once", len(vm.askedFor(vmwpPID)))
	}

	t.Run("an attribution on a gpu the instance is not charged on stays in the remainder", func(t *testing.T) {
		o.wslAttrib[s.proc] = map[int]int{0: 12000, 1: 500}
		vm.set(map[int]int{0: 20000, 1: 900})
		if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 8000, 1: 900}) {
			t.Errorf("UnattributedVRAM = %v, want {0:8000 1:900}: S is charged nothing on GPU 1", got)
		}
		o.wslAttrib[s.proc] = map[int]int{0: 12000}
		vm.set(map[int]int{0: 20000})
	})

	t.Run("an open window is in the snapshot", func(t *testing.T) {
		o.wslWindow = &wslStartWindow{proc: s.proc}
		if !o.buildSnapshot().WSLStartInFlight {
			t.Error("WSLStartInFlight = false with a window open")
		}
		o.wslWindow = nil
	})

	t.Run("no wsl process live, no VM reading", func(t *testing.T) {
		proc := s.proc
		s.proc = nil
		before := len(vm.askedFor(vmwpPID))
		if got := o.buildSnapshot().UnattributedVRAM; got != nil {
			t.Errorf("UnattributedVRAM = %v with nothing wsl live, want nil", got)
		}
		if len(vm.askedFor(vmwpPID)) != before {
			t.Error("the VM worker was measured with nothing wsl live")
		}
		s.proc = proc
	})

	cand := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 0, VRAMMB: 6000}}}
	psnap := PolicySnapshot{
		Running:          []RunningProc{{SpecID: "S", GPUs: map[int]int{0: 12000}, LastUsed: t0, WSL: true}},
		Budgets:          map[int]int{0: 24000},
		UnattributedVRAM: map[int]int{0: 8000},
		Allowed:          allowPairs("cand", "S"),
	}
	// 12000 + 8000 + 6000 = 26000 > 24000: S goes, never the remainder, and
	// 8000 + 6000 then fits.
	assertDecision(t, "remainder in the sum", Admit(psnap, cand), Decision{Evict: []string{"S"}})
	busy := psnap
	busy.Running = slices.Clone(psnap.Running)
	busy.Running[0].InFlight = 1
	assertDecision(t, "remainder beside a busy holder", Admit(busy, cand), Decision{Wait: true, Evict: []string{}})
	none := psnap
	none.UnattributedVRAM = nil
	assertDecision(t, "without a remainder", Admit(none, cand), Decision{OK: true, Evict: []string{}})
	// The remainder alone leaves no room: a wait, never a refusal of a demand
	// that fits its budget by itself.
	alone := PolicySnapshot{Budgets: map[int]int{0: 24000}, UnattributedVRAM: map[int]int{0: 20000}}
	assertDecision(t, "remainder alone", Admit(alone, cand), Decision{Wait: true, Evict: []string{}})
	if _, refused := ownDemandRefusal(alone, cand); refused {
		t.Error("ownDemandRefusal refused a demand that fits its budget alone")
	}
	// A gpu the candidate does not touch is not charged.
	other := Spec{ID: "cand", GPUs: []SpecGPU{{Index: 1, VRAMMB: 6000}}}
	alone.Budgets[1] = 6000
	assertDecision(t, "remainder on another gpu", Admit(alone, other), Decision{OK: true, Evict: []string{}})
}

// TestVRAMBootstrapNotGrantedWithoutVMFinder: the one-shot VRAM bootstrap
// eviction exists to get a first measurement, so a wsl spec is granted it only
// where its VRAM can be measured: a VRAM measurer AND a way to find the VM's
// worker. A native spec still needs only the measurer.
func TestVRAMBootstrapNotGrantedWithoutVMFinder(t *testing.T) {
	ws := wslCandidate("S", 1000)
	ws.GPUs = []SpecGPU{{Index: 0}} // unknown VRAM
	ns := Spec{ID: "N", RAMMB: 1000, GPUs: []SpecGPU{{Index: 0}}}
	wst := &specState{spec: ws, state: StateStopped}
	nst := &specState{spec: ns, state: StateStopped}
	o := &owner{m: &Manager{}, specs: map[string]*specState{"S": wst, "N": nst}}

	o.m.SetMeasurer(func([]int) map[int]map[int]int { return nil })
	if o.vramBootstrapEligible(wst) {
		t.Error("wsl spec granted the VRAM bootstrap with no VM worker finder")
	}
	if !o.vramBootstrapEligible(nst) {
		t.Error("native spec denied the VRAM bootstrap with a measurer installed")
	}
	o.m.SetWSLVMPIDs(func() []int { return nil })
	if !o.vramBootstrapEligible(wst) {
		t.Error("wsl spec denied the VRAM bootstrap with a measurer and a finder installed")
	}
	o.m.SetMeasurer(nil)
	if o.vramBootstrapEligible(wst) || o.vramBootstrapEligible(nst) {
		t.Error("VRAM bootstrap granted with no measurer")
	}
}

// liveUnitVRAM is a VRAM measurer for manager-level tests: the VM worker holds
// on GPU 0, for every live unit the fake recorded, the figure perSpec gives
// its spec -- the VM's figure grows as a workload starts and shrinks as it
// stops, as on a real host.
func liveUnitVRAM(f *fakeWSL, perSpec map[string]int) func([]int) map[int]map[int]int {
	return func(pids []int) map[int]map[int]int {
		if !slices.Contains(pids, vmwpPID) {
			return nil
		}
		entries, err := os.ReadDir(f.state)
		if err != nil {
			return nil
		}
		total := 0
		for _, e := range entries {
			parts := strings.Split(e.Name(), "-") // opagent-<nonce>-<spec>-<generation>
			if len(parts) != 4 {
				continue
			}
			b, err := os.ReadFile(filepath.Join(f.state, e.Name()))
			if err != nil {
				continue
			}
			if pid, err := strconv.Atoi(string(b)); err == nil && processAlive(pid) {
				total += perSpec[parts[2]]
			}
		}
		if total == 0 {
			return nil
		}
		return map[int]map[int]int{vmwpPID: {0: total}}
	}
}

// TestWSLSecondStartWaitsForSettle: while one wsl start's window is open a
// second wsl start waits -- evicting nothing -- and it proceeds once that
// window has closed. Serialized, each start's delta is its own: started side by
// side, the first would be attributed the second's growth too.
func TestWSLSecondStartWaitsForSettle(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.SetMeasurer(liveUnitVRAM(f, map[string]int{"A": 5000, "B": 3000}))
	m.SetWSLVMPIDs(func() []int { return []int{vmwpPID} })
	a := wslTestSpec(f, "A", "a")
	a.Args = stubArgs(time.Second, 0, 0, "")
	a.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	b := wslTestSpec(f, "B", "b")
	b.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, Coresident: coresidentAllPairs("A", "B")})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	aDone := ensureAsync(ctx, m, "a")
	waitForState(t, m, "A", StateStarting)
	bDone := ensureAsync(ctx, m, "b")
	time.Sleep(200 * time.Millisecond)
	if st := statusFor(m, "A"); st.State != StateStarting {
		t.Fatalf("A is %s, want it still loading for this check", st.State)
	}
	if st := statusFor(m, "B"); st.State != StateStopped {
		t.Errorf("B is %s while A's window is open, want it waiting", st.State)
	}
	for _, c := range f.find(t, "systemd-run") {
		if strings.Contains(c.unit(), "-B-") {
			t.Errorf("B was launched while A's window was open: %s", c.unit())
		}
	}

	for model, done := range map[string]<-chan queuedEnsure{"a": aDone, "b": bDone} {
		r := <-done
		if r.err != nil {
			t.Fatalf("EnsureRunning(%s) = %v, want it to start once the window before it closed", model, r.err)
		}
		r.release()
	}
	if got := statusFor(m, "A").MeasuredVRAM; !maps.Equal(got, map[int]int{0: 5000}) {
		t.Errorf("A measured %v, want its own growth {0:5000}", got)
	}
	waitUntil(t, 5*time.Second, "B's window closes with its own growth", func() bool {
		return maps.Equal(statusFor(m, "B").MeasuredVRAM, map[int]int{0: 3000})
	})
	if st := statusFor(m, "A"); st.State != StateRunning {
		t.Errorf("A is %s, want it untouched by B's start", st.State)
	}
}

// Fix round 1 (#381 review): unreadable VM readings, the force_running wake,
// and windows only for GPU specs.

// otherLiveWSL adds a live wsl generation O beside the hand-driven exec
// owner's specs, with no real process behind it, and opens the pair.
func otherLiveWSL(o *owner, attrib map[int]int) *specState {
	other := liveWSL(wslVRAMSpec("O", 1000), 4242, StateRunning)
	o.specs["O"] = other
	o.allowedPairs = allowPairs("S", "O")
	if attrib != nil {
		o.wslAttrib = map[*runningProc]map[int]int{other.proc: attrib}
	}
	return other
}

// TestWSLWindowVoidOnUnreadableBaseline: a baseline that is detectably not
// the VM's -- no worker found while a wsl process lives in the VM, or nothing
// held while a live wsl process is attributed VRAM -- opens a void window:
// starts stay serialized behind it, but it closes without an attribution,
// rather than giving the new instance the whole VM. A VM with nothing wsl in
// it and no worker is simply empty (the first start of a stopped VM).
func TestWSLWindowVoidOnUnreadableBaseline(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	start := func(t *testing.T, setup func(o *owner, vm *vmVRAMScript)) *wslStartWindow {
		t.Helper()
		f := newFakeWSL(t)
		s := wslTestSpec(f, "S", "m")
		s.Pinned = true
		s.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
		o := newWSLExecOwner(t, f, s)
		vm := &vmVRAMScript{vm: map[int]int{0: 5000}}
		vm.install(o.m)
		setup(o, vm)
		o.admitAndStart("S")
		if o.specs["S"].proc == nil {
			t.Fatalf("S did not start: %s %+v", o.specs["S"].state, o.specs["S"].lastError)
		}
		if o.wslWindow == nil || o.wslWindow.proc != o.specs["S"].proc {
			t.Fatalf("window = %+v, want one open for S, so wsl starts stay serialized", o.wslWindow)
		}
		return o.wslWindow
	}
	t.Run("no worker while a wsl process is live", func(t *testing.T) {
		w := start(t, func(o *owner, vm *vmVRAMScript) {
			otherLiveWSL(o, nil)
			vm.setNoWorker(true)
		})
		if !w.void {
			t.Errorf("window = %+v, want it void", w)
		}
	})
	t.Run("nothing held while a live wsl process is attributed", func(t *testing.T) {
		w := start(t, func(o *owner, vm *vmVRAMScript) {
			otherLiveWSL(o, map[int]int{0: 5000})
			vm.set(nil)
		})
		if !w.void {
			t.Errorf("window = %+v, want it void", w)
		}
	})
	t.Run("a stopped VM is empty, not unreadable", func(t *testing.T) {
		w := start(t, func(_ *owner, vm *vmVRAMScript) { vm.setNoWorker(true) })
		if w.void || w.baseline != nil {
			t.Errorf("window = %+v, want an ordinary one with an empty baseline", w)
		}
	})
}

// TestWSLSettleVoidOnUnreadableReading: the settling reading is checked the
// same way, plus one rule of its own -- nothing held after a baseline that
// held something cannot be the start's growth. The window then closes with no
// attribution at all: not an empty one, which would let a later exit of this
// process during another window credit 0 instead of voiding that window.
func TestWSLSettleVoidOnUnreadableReading(t *testing.T) {
	for name, tc := range map[string]struct {
		baseline map[int]int
		reading  map[int]int
		noWorker bool
	}{
		"nothing held after a baseline that held something": {baseline: map[int]int{0: 12340}, reading: nil},
		"no worker at the settle":                           {baseline: nil, reading: map[int]int{0: 5000}, noWorker: true},
	} {
		t.Run(name, func(t *testing.T) {
			vm := &vmVRAMScript{vm: tc.reading}
			b := liveWSL(wslVRAMSpec("B", 6000), 301, StateRunning)
			o := wslVRAMOwner(vm, b)
			o.wslWindow = &wslStartWindow{proc: b.proc, baseline: tc.baseline}
			vm.setNoWorker(tc.noWorker)
			measureNow(t, o)
			if o.wslWindow != nil {
				t.Fatal("the window is still open after its settling measurement")
			}
			if a, ok := o.wslAttrib[b.proc]; ok || b.measuredVRAM != nil {
				t.Errorf("B attributed %v (entry %v), measured %v, want no attribution entry", a, ok, b.measuredVRAM)
			}
		})
	}
}

// TestWSLAdmissionFallsBackToLastGoodVMReading: a reading at admission that is
// detectably not the VM's leaves the remainder at the last good reading, so a
// failed read never shows the VM's unattributed VRAM as free; nor does a
// failed reading on the beat replace the last good one.
func TestWSLAdmissionFallsBackToLastGoodVMReading(t *testing.T) {
	vm := &vmVRAMScript{vm: map[int]int{0: 20000}}
	s := liveWSL(wslVRAMSpec("S", 6000), 300, StateRunning)
	o := wslVRAMOwner(vm, s)
	o.wslAttrib = map[*runningProc]map[int]int{s.proc: {0: 12000}}
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 8000}) {
		t.Fatalf("good reading: UnattributedVRAM = %v, want {0:8000}", got)
	}

	vm.set(nil) // nothing held, while S is attributed 12000
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 8000}) {
		t.Errorf("failed reading: UnattributedVRAM = %v, want the last good {0:8000}", got)
	}
	measureNow(t, o) // the beat reads the same failure
	vm.setNoWorker(true)
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 8000}) {
		t.Errorf("no worker: UnattributedVRAM = %v, want the last good {0:8000}", got)
	}

	vm.setNoWorker(false)
	vm.set(map[int]int{0: 15000})
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 3000}) {
		t.Errorf("good reading again: UnattributedVRAM = %v, want {0:3000}", got)
	}
}

// TestWSLWindowCloseStartsForceRunningWaiter: two force_running wsl specs and
// no request. The second waits for the first one's window; a force_running
// spec has neither a queued request nor a pin, so the ordinary event wake
// would skip it, and its start must still follow the window's close.
func TestWSLWindowCloseStartsForceRunningWaiter(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.SetMeasurer(liveUnitVRAM(f, map[string]int{"A": 5000, "B": 3000}))
	m.SetWSLVMPIDs(func() []int { return []int{vmwpPID} })
	a := wslTestSpec(f, "A", "a")
	a.AdminState = "force_running"
	a.Args = stubArgs(300*time.Millisecond, 0, 0, "")
	a.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	b := wslTestSpec(f, "B", "b")
	b.AdminState = "force_running"
	b.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, Coresident: coresidentAllPairs("A", "B")})

	waitForState(t, m, "A", StateRunning)
	waitForState(t, m, "B", StateRunning)
	waitUntil(t, 5*time.Second, "both windows close with their own growth", func() bool {
		return maps.Equal(statusFor(m, "A").MeasuredVRAM, map[int]int{0: 5000}) &&
			maps.Equal(statusFor(m, "B").MeasuredVRAM, map[int]int{0: 3000})
	})
}

// TestWSLWindowCloseNeverEvictsForAForceRunningWaiter: the re-ask a window's
// close gives a force_running waiter starts it only into free room. Here B
// could only start by evicting the idle A (one process at most), and a wake
// that evicted would have the two evict each other at load speed.
func TestWSLWindowCloseNeverEvictsForAForceRunningWaiter(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	f := newFakeWSL(t)
	m := newWSLTestManager(t, f.runner())
	m.SetMeasurer(liveUnitVRAM(f, map[string]int{"A": 5000, "B": 3000}))
	m.SetWSLVMPIDs(func() []int { return []int{vmwpPID} })
	a := wslTestSpec(f, "A", "a")
	a.AdminState = "force_running"
	a.Args = stubArgs(300*time.Millisecond, 0, 0, "")
	a.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	b := wslTestSpec(f, "B", "b")
	b.AdminState = "force_running"
	b.GPUs = []SpecGPU{{Index: 0, VRAMMB: 1000}}
	m.Apply(Config{ETag: "v1", Specs: []Spec{a, b}, MaxProcesses: 1, Coresident: coresidentAllPairs("A", "B")})

	waitUntil(t, 5*time.Second, "A's window closes", func() bool {
		return maps.Equal(statusFor(m, "A").MeasuredVRAM, map[int]int{0: 5000})
	})
	time.Sleep(200 * time.Millisecond) // several beats and wakes
	if st := statusFor(m, "A"); st.State != StateRunning {
		t.Errorf("A is %s, want it left alone", st.State)
	}
	if st := statusFor(m, "B"); st.State != StateStopped {
		t.Errorf("B is %s, want it to wait rather than evict A", st.State)
	}
}

// TestWSLCPUOnlyStartIsNotSerialized: a wsl spec that declares no GPU opens no
// window, is not held back by another start's window, and counts as holding
// no VRAM -- so its exit during a window neither voids it nor adds a credit.
func TestWSLCPUOnlyStartIsNotSerialized(t *testing.T) {
	t.Run("no window, no VM reading", func(t *testing.T) {
		skipOnWindows(t)
		shrinkTimings(t)
		f := newFakeWSL(t)
		s := wslTestSpec(f, "S", "m")
		s.Pinned = true
		o := newWSLExecOwner(t, f, s)
		vm := &vmVRAMScript{vm: map[int]int{0: 22}}
		vm.install(o.m)
		o.admitAndStart("S")
		proc := o.specs["S"].proc
		if proc == nil {
			t.Fatalf("S did not start: %s", o.specs["S"].state)
		}
		if o.wslWindow != nil {
			t.Errorf("a CPU-only wsl start opened a window: %+v", o.wslWindow)
		}
		if len(vm.askedFor(vmwpPID)) != 0 {
			t.Errorf("a CPU-only wsl start read the VM: %v", vm.askedFor(vmwpPID))
		}
		if a, ok := o.wslAttrib[proc]; !ok || len(a) != 0 {
			t.Errorf("S attribution %v (entry %v), want an empty one: it holds no VRAM", a, ok)
		}
	})

	t.Run("not held back by a window", func(t *testing.T) {
		snap := PolicySnapshot{WSLStartInFlight: true}
		cpu := wslCandidate("cand", 1000)
		assertDecision(t, "CPU-only wsl candidate", Admit(snap, cpu), Decision{OK: true, Evict: []string{}})
		gpu := wslVRAMSpec("cand", 1000)
		assertDecision(t, "GPU wsl candidate", Admit(snap, gpu), Decision{Wait: true, Evict: []string{}})
	})

	t.Run("its exit leaves the window as it was", func(t *testing.T) {
		vm := &vmVRAMScript{}
		c := liveWSL(wslCandidate("C", 1000), 300, StateRunning)
		b := liveWSL(wslVRAMSpec("B", 6000), 301, StateStarting)
		o := wslVRAMOwner(vm, c, b)
		o.wslAttrib = map[*runningProc]map[int]int{c.proc: nil}
		o.wslWindow = &wslStartWindow{proc: b.proc, baseline: map[int]int{0: 100}}
		c.state, c.intentionalStop = StateDraining, true
		o.onProcExited(c, nil)
		if w := o.wslWindow; w == nil || w.void || len(w.exitedCredit) != 0 {
			t.Errorf("window after a CPU-only exit = %+v, want it untouched", w)
		}
	})
}

// TestWSLAttributionOnlyOnDeclaredGPUs: the VM's growth on a GPU the spec
// does not declare is not the instance's figure -- another process in the VM
// may have grown there -- so it stays in the remainder instead.
func TestWSLAttributionOnlyOnDeclaredGPUs(t *testing.T) {
	vm := &vmVRAMScript{}
	b := liveWSL(wslVRAMSpec("B", 6000), 301, StateRunning)
	o := wslVRAMOwner(vm, b)
	o.wslWindow = &wslStartWindow{proc: b.proc, baseline: map[int]int{0: 100, 1: 100}}
	vm.set(map[int]int{0: 7100, 1: 2100})
	measureNow(t, o)
	if want := map[int]int{0: 7000}; !maps.Equal(o.wslAttrib[b.proc], want) || !maps.Equal(b.measuredVRAM, want) {
		t.Errorf("B attributed %v, measured %v, want only the declared GPU %v", o.wslAttrib[b.proc], b.measuredVRAM, want)
	}
	if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 100, 1: 2100}) {
		t.Errorf("UnattributedVRAM = %v, want GPU 1's growth in it {0:100 1:2100}", got)
	}
}

// Final review (#381): the last good VM reading, the settle wake and the
// open window's provisional attribution.

// TestWSLVMReadingDoesNotOutliveItsPeriod: the last good VM reading, which an
// admission falls back to when its own reading is detectably failed, belongs
// to the period of WSL activity it was taken in. Once no wsl generation is
// live it is forgotten, so a later period that begins with a CPU-only wsl
// generation -- the VM still booting, no worker found yet, which reads as
// failed -- charges no remainder from an earlier period's figure: the booting
// VM holds nothing. A measurement still out when the period ended does not
// bring that figure back either.
func TestWSLVMReadingDoesNotOutliveItsPeriod(t *testing.T) {
	// period1 runs a GPU wsl generation G attributed 12000 of the VM's 12022,
	// so the last good reading is {0:12022}.
	period1 := func(t *testing.T) (*owner, *vmVRAMScript, *specState) {
		t.Helper()
		vm := &vmVRAMScript{vm: map[int]int{0: 12022}}
		g := liveWSL(wslVRAMSpec("G", 12000), 300, StateRunning)
		o := wslVRAMOwner(vm, g)
		o.wslAttrib = map[*runningProc]map[int]int{g.proc: {0: 12000}}
		if got := o.buildSnapshot().UnattributedVRAM; !maps.Equal(got, map[int]int{0: 22}) {
			t.Fatalf("period 1: remainder %v, want {0:22}", got)
		}
		t.Cleanup(func() {
			close(o.m.done)
			o.m.wg.Wait()
		})
		return o, vm, g
	}
	// period2 starts a CPU-only wsl generation C while the VM boots: the
	// finder names no worker, and the reading counts as failed.
	period2 := func(t *testing.T, o *owner, vm *vmVRAMScript) {
		t.Helper()
		c := liveWSL(wslCandidate("C", 1000), 301, StateStarting)
		c.spec.UpstreamModel = "c"
		o.specs["C"] = c
		o.wslAttrib[c.proc] = nil
		vm.setNoWorker(true)
		if got := o.buildSnapshot().UnattributedVRAM; got != nil {
			t.Errorf("period 2, VM booting: UnattributedVRAM = %v, want nil: the booting VM holds nothing, and %v was an earlier period's", got, map[int]int{0: 12022})
		}
	}

	t.Run("an admission between the periods", func(t *testing.T) {
		o, vm, g := period1(t)
		o.noteWSLExit(g.proc)
		g.proc = nil
		vm.set(map[int]int{0: 22})
		if got := o.buildSnapshot().UnattributedVRAM; got != nil {
			t.Fatalf("between periods: remainder %v, want nil", got)
		}
		period2(t, o, vm)
	})

	t.Run("no admission between the periods", func(t *testing.T) {
		o, vm, g := period1(t)
		g.state, g.intentionalStop = StateDraining, true
		o.onProcExited(g, nil)
		vm.set(map[int]int{0: 22})
		period2(t, o, vm)
	})

	t.Run("a measurement that lands after its period ended", func(t *testing.T) {
		o, vm, g := period1(t)
		gate := vm.hold()
		o.dispatchMeasurement() // reads the VM while G is still live
		g.state, g.intentionalStop = StateDraining, true
		o.onProcExited(g, nil)
		close(gate)
		handleNextMeasurement(t, o)
		vm.set(map[int]int{0: 22})
		period2(t, o, vm)
	})
}

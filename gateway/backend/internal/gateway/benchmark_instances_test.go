// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// instanceStatus is spec specID's instance-k status row in state with pid.
func instanceStatus(specID string, k int, state string, pid int) RuntimeStatusDTO {
	return RuntimeStatusDTO{SpecID: specID, Instance: k, State: state, PID: pid}
}

// TestBenchmarkOthersStoppedSeesInstances pins the cold-start check over a
// replicable target: the target's own row is its instance-0 row, and every
// other instance of the target is an "other" like a neighbour spec -- a burst
// instance that runs holds memory the cold pass would be measured against, so
// a cold start is confirmed only once every one of them reads stopped with
// pid 0.
func TestBenchmarkOthersStoppedSeesInstances(t *testing.T) {
	cases := []struct {
		name string
		rows []RuntimeStatusDTO
		want bool
	}{
		{"another instance of the target runs", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "running", 4242),
		}, false},
		{"another instance of the target starts", []RuntimeStatusDTO{
			instanceStatus("S", 2, "starting", 0), instanceStatus("S", 0, "stopped", 0),
		}, false},
		{"every instance of the target stopped", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "stopped", 0), instanceStatus("T", 0, "stopped", 0),
		}, true},
		{"the target's instance-0 row is not read here", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 4242), instanceStatus("S", 1, "stopped", 0),
		}, true},
		{"a neighbour's burst instance runs", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("T", 0, "stopped", 0), instanceStatus("T", 3, "running", 77),
		}, false},
	}
	for _, tc := range cases {
		if got := benchmarkOthersStopped(tc.rows, "S"); got != tc.want {
			t.Errorf("%s: benchmarkOthersStopped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestBenchmarkTargetRowIsInstanceZero pins the two rows a benchmark reads
// for its target. benchmarkSpecStatus -- the target's context size, the
// stop-all's presence check and its quiet wait -- reads the spec's
// representative row (representativeRowIndex), the row every other view of
// the spec reads, so a burst instance that serves speaks for the spec while
// instance 0 is down. Residency (agentSpecResidency) asks a different
// question, whether the instance the target's plain model name reaches is
// up, so it reads the instance-0 row whatever order the agent reports its
// rows in, and never a burst instance's.
func TestBenchmarkTargetRowIsInstanceZero(t *testing.T) {
	frame := []RuntimeStatusDTO{instanceStatus("S", 1, "running", 4242), instanceStatus("S", 0, "stopped", 0)}
	row, ok := benchmarkSpecStatus(frame, "S")
	if !ok || row.Instance != 1 || row.State != "running" {
		t.Fatalf("benchmarkSpecStatus = %+v (ok %v), want the representative row, the running instance 1", row, ok)
	}
	if row, ok := benchmarkSpecStatus([]RuntimeStatusDTO{instanceStatus("S", 2, "running", 1)}, "S"); !ok || row.Instance != 2 {
		t.Fatalf("benchmarkSpecStatus = %+v (ok %v), want the only row, the running instance 2", row, ok)
	}

	srv := &Server{RuntimeStatus: NewRuntimeStatusRegistry()}
	srv.RuntimeStatus.publish("srv1", frame)
	tgt := benchmarkTarget{server: routing.AIServer{ID: "srv1"}, spec: routing.RuntimeSpec{ID: "S"}}
	if resident, known := srv.agentSpecResidency(tgt); resident || !known {
		t.Fatalf("agentSpecResidency = resident %v known %v, want instance 0's stopped row read: not resident, known", resident, known)
	}
}

// TestBenchmarkStopSelectionAwaitsTheTargetsInstanceZero pins whose state
// decides that the stop-all's quiet wait also waits for the target to read
// stopped: instance 0's, the instance the cold pass's request reaches. A
// request for an instance 0 in backoff waits behind its backoff timer, and
// that wait would land in the load time, even while a burst instance of the
// target serves and is the spec's representative row.
func TestBenchmarkStopSelectionAwaitsTheTargetsInstanceZero(t *testing.T) {
	cases := []struct {
		name string
		rows []RuntimeStatusDTO
		want string
	}{
		{"instance 0 in backoff beside a serving burst instance", []RuntimeStatusDTO{
			instanceStatus("S", 1, "running", 4242), instanceStatus("S", 0, "backoff", 0),
		}, "S"},
		{"instance 0 stopped beside a crashed burst instance", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "crashed", 0),
		}, ""},
		{"a plain target that crashed", []RuntimeStatusDTO{instanceStatus("S", 0, "crashed", 0)}, "S"},
	}
	for _, tc := range cases {
		sel, reason := benchmarkStopSelectionOf(tc.rows, "S", nil)
		if reason != "" || sel.awaitStopped != tc.want {
			t.Errorf("%s: awaitStopped = %q (reason %q), want %q", tc.name, sel.awaitStopped, reason, tc.want)
		}
		if !slices.Contains(sel.stopSet, "S") {
			t.Errorf("%s: stop set = %v, want the target in it", tc.name, sel.stopSet)
		}
	}
	if _, reason := benchmarkStopSelectionOf([]RuntimeStatusDTO{instanceStatus("T", 0, "running", 1)}, "S", nil); reason != benchmarkStopReasonNoStatus {
		t.Fatalf("a frame without the target: reason = %q, want %q", reason, benchmarkStopReasonNoStatus)
	}
}

// TestBenchmarkStopFrameVerdictCountsSpecsNotRows pins the stop-all's quiet
// wait over instance rows: the stop set names specs, and a spec is quiet when
// every one of its rows is. Counting rows instead would let two quiet rows of
// one stopped spec stand in for another spec that still runs. A target whose
// own row must read stopped is waited for on its instance-0 row, not on a
// burst instance that happens to read stopped first.
func TestBenchmarkStopFrameVerdictCountsSpecsNotRows(t *testing.T) {
	cases := []struct {
		name         string
		frame        []RuntimeStatusDTO
		stopSet      []string
		awaitStopped string
		wantDone     bool
		wantQuiet    bool
	}{
		{"two quiet rows of one spec do not cover a spec still running", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "stopped", 0), instanceStatus("T", 0, "running", 4242),
		}, []string{"S", "T"}, "", false, false},
		{"every row of every spec quiet", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "stopped", 0), instanceStatus("T", 0, "stopped", 0),
		}, []string{"S", "T"}, "", true, true},
		{"a burst instance of a stopped spec still drains", []RuntimeStatusDTO{
			instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "draining", 4242),
		}, []string{"S"}, "", false, false},
		{"the awaited target's own row is instance 0's", []RuntimeStatusDTO{
			instanceStatus("S", 1, "stopped", 0), instanceStatus("S", 0, "backoff", 0),
		}, []string{"S"}, "S", false, false},
		{"the awaited target's instance 0 reads stopped", []RuntimeStatusDTO{
			instanceStatus("S", 1, "stopped", 0), instanceStatus("S", 0, "stopped", 0),
		}, []string{"S"}, "S", true, true},
	}
	for _, tc := range cases {
		done, quiet, reason := benchmarkStopFrameVerdict(tc.frame, tc.stopSet, tc.awaitStopped)
		if done != tc.wantDone || quiet != tc.wantQuiet || reason != "" {
			t.Errorf("%s: verdict = done %v quiet %v reason %q, want done %v quiet %v and no reason", tc.name, done, quiet, reason, tc.wantDone, tc.wantQuiet)
		}
	}
}

// TestBenchmarkFormerPinsSeeTheTargetsOtherInstances pins the formerly-pinned
// check over instance rows: the target's own row is set aside, its other
// instances are not, and a spec is named once however many of its rows are
// not stopped.
func TestBenchmarkFormerPinsSeeTheTargetsOtherInstances(t *testing.T) {
	rows := []RuntimeStatusDTO{
		instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "running", 4242),
		instanceStatus("T", 0, "running", 77), instanceStatus("T", 1, "backoff", 0),
	}
	if got := benchmarkFormerPinsNotStopped(rows, []string{"S", "T"}, "S"); !slices.Equal(got, []string{"S", "T"}) {
		t.Fatalf("benchmarkFormerPinsNotStopped = %v, want [S T]: the target's burst instance counts, each spec once", got)
	}
	if got := benchmarkFormerPinsNotStopped(rows[:1], []string{"S"}, "S"); len(got) != 0 {
		t.Fatalf("benchmarkFormerPinsNotStopped = %v, want none: the target's own row is set aside", got)
	}
}

// TestVRAMIsolationWaitsForEveryInstance pins the VRAM run's reading of a
// frame over instance rows: a spec reads as having no process only when none
// of its rows has one, whatever order the rows arrive in, so a burst instance
// still running keeps the spec pending instead of being hidden by a stopped
// instance 0 that happened to come last.
func TestVRAMIsolationWaitsForEveryInstance(t *testing.T) {
	for _, frame := range [][]RuntimeStatusDTO{
		{instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "running", 4242)},
		{instanceStatus("S", 1, "running", 4242), instanceStatus("S", 0, "stopped", 0)},
	} {
		if state := vramStateBySpec(frame)["S"]; vramStateNoProcess(state) {
			t.Fatalf("vramStateBySpec(%+v)[S] = %q, want a state with a process", frame, state)
		}
		if got := vramFrameEvidence(frame, map[string]struct{}{"S": {}}, nil); len(got) != 0 {
			t.Fatalf("vramFrameEvidence(%+v) = %v, want no evidence while an instance runs", frame, got)
		}
	}
	quiet := []RuntimeStatusDTO{instanceStatus("S", 0, "stopped", 0), instanceStatus("S", 1, "crashed", 0)}
	if got := vramFrameEvidence(quiet, map[string]struct{}{"S": {}}, nil); got["S"] == "" {
		t.Fatalf("vramFrameEvidence(%+v) = %v, want S confirmed once every instance has no process", quiet, got)
	}
}

// TestVRAMIsolationLostSeesTheTargetsOtherInstances pins the end-of-run
// isolation recheck over a replicable target. The run cleared the target's
// override to load it, so the target's instance-0 row holding a process is the
// point of the run. Every other instance of the target was drained with the
// fleet, though, and one that started during the load (a protected minimum
// coming back once the override cleared, or a burst start) put its whole
// allocation into the measured delta -- so it breaks the isolation exactly as a
// sibling does, and the target is named, in whichever order the rows arrive.
func TestVRAMIsolationLostSeesTheTargetsOtherInstances(t *testing.T) {
	plan := vramRunPlanned{specIDs: []string{"S", "T"}, targetSpecID: "S"}
	cases := []struct {
		name string
		rows []RuntimeStatusDTO
		want []string
	}{
		{"another instance of the target runs", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("S", 1, "running", 7), instanceStatus("T", 0, "stopped", 0),
		}, []string{"S"}},
		{"another instance of the target runs, listed first", []RuntimeStatusDTO{
			instanceStatus("S", 1, "running", 7), instanceStatus("S", 0, "running", 5), instanceStatus("T", 0, "stopped", 0),
		}, []string{"S"}},
		{"another instance of the target starts", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("S", 2, "starting", 0), instanceStatus("T", 0, "stopped", 0),
		}, []string{"S"}},
		{"another instance of the target in a state this gateway does not know", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("S", 1, "warming", 0), instanceStatus("T", 0, "stopped", 0),
		}, []string{"S"}},
		{"every other instance of the target has no process", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("S", 1, "stopped", 0), instanceStatus("S", 2, "crashed", 0),
			instanceStatus("T", 0, "stopped", 0),
		}, nil},
		{"only the target's instance 0", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("T", 0, "stopped", 0),
		}, nil},
		{"a sibling's burst instance runs", []RuntimeStatusDTO{
			instanceStatus("S", 0, "running", 5), instanceStatus("T", 0, "stopped", 0), instanceStatus("T", 1, "running", 9),
		}, []string{"T"}},
		{"both", []RuntimeStatusDTO{
			instanceStatus("T", 1, "running", 9), instanceStatus("S", 1, "running", 7), instanceStatus("S", 0, "running", 5),
			instanceStatus("T", 0, "stopped", 0),
		}, []string{"S", "T"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{RuntimeStatus: NewRuntimeStatusRegistry()}
			srv.RuntimeStatus.publish("srv1", tc.rows)
			if got := srv.vramIsolationLost("srv1", plan); !slices.Equal(got, tc.want) {
				t.Fatalf("vramIsolationLost = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestVRAMRunRefusesANumberWhenAnotherInstanceOfTheTargetRuns is the run-level
// half: once the run clears the target's override, the agent brings up a second
// instance of the target as well, and the post-load window holds the memory of
// both. The run must not report that sum as the target's cost.
func TestVRAMRunRefusesANumberWhenAnotherInstanceOfTheTargetRuns(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		f.used0.Store((500 + 9000 + 9000) * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{SpecID: f.targetSpec, Instance: 0, State: "running", PID: 5},
			RuntimeStatusDTO{SpecID: f.targetSpec, Instance: 1, State: "running", PID: 7},
			RuntimeStatusDTO{SpecID: f.siblingSpec, State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil {
		t.Fatal("VRAM = nil, want a report carrying the reason")
	}
	if report.Inconclusive != "isolation_lost" {
		t.Fatalf("Inconclusive = %q, want %q -- delta_mb %d holds two instances of the target",
			report.Inconclusive, "isolation_lost", vramHeadlineDeltaMB(report.GPUs))
	}
	if report.Isolated {
		t.Fatal("Isolated = true while another instance of the target ran")
	}
	if got := report.IsolationEvidence[f.targetSpec]; got != "restarted_during_run" {
		t.Fatalf("IsolationEvidence[%s] = %q, want %q", f.targetSpec, got, "restarted_during_run")
	}
	for _, item := range report.GPUs {
		if item.DeltaMB != 0 {
			t.Fatalf("a contaminated run reported a delta: %#v", item)
		}
	}
}

// TestVRAMRunAcceptsTheTargetsOtherInstancesStopped: the target's other
// instances reading stopped at the end leave the isolation intact, so the
// recheck does not refuse every run of a replicable spec.
func TestVRAMRunAcceptsTheTargetsOtherInstancesStopped(t *testing.T) {
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.seedLatestSample()
	f.drive(t)
	f.provider.onStream = func() {
		f.used0.Store(21500 * oneMiB)
		f.setStatuses(
			RuntimeStatusDTO{SpecID: f.targetSpec, Instance: 1, State: "stopped"},
			RuntimeStatusDTO{SpecID: f.targetSpec, Instance: 0, State: "running", PID: 5},
			RuntimeStatusDTO{SpecID: f.siblingSpec, State: "stopped"},
		)
	}

	report := f.run(t).Results[0].VRAM
	if report == nil || report.Inconclusive != "" {
		t.Fatalf("report = %#v, want a definitive result", report)
	}
	if !report.Isolated {
		t.Fatalf("Isolated = false; evidence = %v", report.IsolationEvidence)
	}
}

// TestVRAMMeasuredFromFrameReadsInstanceZero: the agent's own per-card
// measurement of the target is instance 0's -- the instance the run loaded --
// never a figure another instance of the same spec reports under the same id,
// whichever comes first in the frame.
func TestVRAMMeasuredFromFrameReadsInstanceZero(t *testing.T) {
	at := time.Now().UTC()
	burst := RuntimeStatusDTO{
		SpecID: "S", Instance: 1, State: "running", PID: 7, MeasuredAt: at,
		GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 9100}},
	}
	own := RuntimeStatusDTO{
		SpecID: "S", Instance: 0, State: "running", PID: 5, MeasuredAt: at,
		GPUs: []RuntimeGPUStatusDTO{{Index: 0, VRAMMeasuredMB: 9000}},
	}
	for _, frame := range [][]RuntimeStatusDTO{{burst, own}, {own, burst}} {
		got := vramMeasuredFromFrame(frame, "S")
		if len(got) != 1 || got[0] != 9000 {
			t.Fatalf("vramMeasuredFromFrame(%+v) = %v, want instance 0's {0: 9000}", frame, got)
		}
	}
	if got := vramMeasuredFromFrame([]RuntimeStatusDTO{burst}, "S"); got != nil {
		t.Fatalf("vramMeasuredFromFrame(only another instance) = %v, want nil", got)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
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

// TestBenchmarkTargetRowIsInstanceZero pins which row is "the target's own"
// wherever a benchmark reads one (benchmarkSpecStatus, agentSpecResidency):
// the instance-0 row -- the instance the target's plain model name reaches --
// whatever order the agent reports its rows in, and never a burst instance's.
func TestBenchmarkTargetRowIsInstanceZero(t *testing.T) {
	frame := []RuntimeStatusDTO{instanceStatus("S", 1, "running", 4242), instanceStatus("S", 0, "stopped", 0)}
	row, ok := benchmarkSpecStatus(frame, "S")
	if !ok || row.Instance != 0 || row.State != "stopped" {
		t.Fatalf("benchmarkSpecStatus = %+v (ok %v), want the instance-0 row", row, ok)
	}
	if _, ok := benchmarkSpecStatus([]RuntimeStatusDTO{instanceStatus("S", 2, "running", 1)}, "S"); ok {
		t.Fatal("benchmarkSpecStatus found a row for S with only a burst instance reported, want none")
	}

	srv := &Server{RuntimeStatus: NewRuntimeStatusRegistry()}
	srv.RuntimeStatus.publish("srv1", frame)
	tgt := benchmarkTarget{server: routing.AIServer{ID: "srv1"}, spec: routing.RuntimeSpec{ID: "S"}}
	if resident, known := srv.agentSpecResidency(tgt); resident || !known {
		t.Fatalf("agentSpecResidency = resident %v known %v, want instance 0's stopped row read: not resident, known", resident, known)
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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// TestBenchmarkMayPreStopMarksOnlyManualSpeedAndBothRuns: only a speed or
// both run's targets may stop the server's running agent models.
func TestBenchmarkMayPreStopMarksOnlyManualSpeedAndBothRuns(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want bool
	}{
		{"speed", true}, {"both", true}, {"capacity", false}, {"vision", false},
	} {
		targets := []benchmarkTarget{{}, {}}
		benchmarkMayPreStop(targets, tc.mode)
		for i, tgt := range targets {
			if tgt.mayPreStop != tc.want {
				t.Fatalf("mode %q: target %d mayPreStop = %v, want %v", tc.mode, i, tgt.mayPreStop, tc.want)
			}
		}
	}
}

// TestStartBenchmarkLetsOnlySpeedAndBothRunsStop: the manual starter marks a
// speed run's agent targets, so the run asks whether it may stop at all (here
// it may not: the agent never declared runtime_manager, and the run says so);
// a capacity run never asks.
func TestStartBenchmarkLetsOnlySpeedAndBothRunsStop(t *testing.T) {
	const msg = "benchmark: running agent models are not stopped in this run"
	for _, tc := range []struct {
		mode string
		want int
	}{
		{"speed", 1}, {"capacity", 0},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newRefusalFixture(t)
			if status, code, _ := f.post(t, "/api/portal/applications/rf_agent/benchmark?mode="+tc.mode); status != http.StatusAccepted {
				t.Fatalf("status = %d code = %q, want 202", status, code)
			}
			f.waitFinished(t)
			n := 0
			for _, r := range f.logs.Snapshot() {
				if r.Level == "INFO" && r.Msg == msg && r.Attrs["server_id"] == baOwnedServer {
					n++
				}
			}
			if n != tc.want {
				t.Fatalf("Info %q logged %d times, want %d", msg, n, tc.want)
			}
		})
	}
}

// waitRunFinished polls until srv1's run has finished, for up to 10 s.
func waitRunFinished(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for srv.Benchmarks.Status("srv1").Running {
		if time.Now().After(deadline) {
			t.Fatal("the run did not finish within 10 s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestScheduledRunWritesNoSpecOnAResidentTarget: a scheduled run never stops
// or unpins an agent model. A resident target, pinned or not, gets no load
// time, and the mapping keeps its stored one.
func TestScheduledRunWritesNoSpecOnAResidentTarget(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		name := "unpinned"
		if pinned {
			name = "pinned"
		}
		t.Run(name, func(t *testing.T) {
			t1 := stopAllSpecOf("map1", "running", 1101)
			t1.pinned = pinned
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1}})

			if !f.srv.TriggerScheduledBenchmark(context.Background(), f.server, f.app) {
				t.Fatal("TriggerScheduledBenchmark = false, want a launched run")
			}
			waitRunFinished(t, f.srv)

			if got := f.events.with("stop ", "clear ", "lease"); len(got) != 0 {
				t.Fatalf("writes = %v, want none: a scheduled run never stops", got)
			}
			if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
				t.Fatalf("history = load %d ms, error %q; want no load time for a resident target", row.LoadTimeMS, row.Error)
			}
			if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.LoadTimeMS != 1234 {
				t.Fatalf("stored load_time_ms = %d (%v), want 1234 kept", m.LoadTimeMS, err)
			}
		})
	}
}

// TestManualSpeedRunWithForceRunningWritesNothing: an enabled force_running
// spec anywhere on the server turns the unpin and every stop off. The run
// writes no lease and no spec, so the pinned target stays pinned, and records
// no load time while that spec is not stopped.
func TestManualSpeedRunWithForceRunningWritesNothing(t *testing.T) {
	for _, tc := range []struct {
		name             string
		frState, t1State string
		frPID, t1PID     int
	}{
		{"a resident target next to a running force_running spec", "running", "running", 1201, 1202},
		{"a cold target while the force_running spec runs", "running", "stopped", 1203, 0},
		{"a cold target while the force_running spec sits in backoff", "backoff", "stopped", 0, 0},
		{"a resident target next to a stopped force_running spec", "stopped", "running", 0, 1204},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := withCapturedSlogAtTheDefaultLevel(t)
			fr := stopAllSpecOf("mapf", tc.frState, tc.frPID)
			fr.adminState = benchmarkAdminStateForceRunning
			t1 := stopAllSpecOf("map1", tc.t1State, tc.t1PID)
			t1.pinned = true
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{fr, t1}})

			f.run(t, "speed", f.targets(t, "speed", "map1"))

			if got := f.events.with("stop", "clear", "lease", "unpin", "repin"); len(got) != 0 {
				t.Fatalf("writes = %v, want none next to a force_running spec", got)
			}
			if !f.pinnedSpecs(t)["rs_map1"] {
				t.Fatal("rs_map1 after the run is not pinned, want its pin left in place")
			}
			if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
				t.Fatalf("history = load %d ms, error %q; want no load time", row.LoadTimeMS, row.Error)
			}
			if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.LoadTimeMS != 1234 {
				t.Fatalf("stored load_time_ms = %d (%v), want 1234 kept", m.LoadTimeMS, err)
			}
			found := false
			for _, r := range logs.Snapshot() {
				if r.Level == "INFO" && r.Msg == "benchmark: a launch spec carries force_running; nothing is unpinned or stopped in this run" && r.Attrs["spec_id"] == "rs_mapf" {
					found = true
				}
			}
			if !found {
				t.Fatal("no Info naming rs_mapf as the force_running spec that turned stops off")
			}
		})
	}
}

// TestBenchmarkRunStartIsolationGateLeavesThePinsInPlace: a run whose agent
// applies no runtime configuration at its start, in file mode or without
// runtime_manager, unpins nothing and stops nothing: it writes no lease and no
// spec, and the pinned target and neighbour stay pinned.
func TestBenchmarkRunStartIsolationGateLeavesThePinsInPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		gate func(f *stopAllFixture)
	}{
		{"file mode", func(f *stopAllFixture) { f.srv.RuntimeStatus.SetFileMode("srv1", true) }},
		{"no runtime_manager", func(f *stopAllFixture) { f.srv.AgentFeatures.Set("srv1", nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := withCapturedSlogAtTheDefaultLevel(t)
			t1 := stopAllSpecOf("map1", "running", 1206)
			t1.pinned = true
			n := stopAllSpecOf("mapn", "running", 1207)
			n.pinned = true
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, n}})
			tc.gate(f)

			status := f.run(t, "speed", f.targets(t, "speed", "map1"))

			if got := f.events.with("stop", "clear", "lease", "unpin", "repin"); len(got) != 0 {
				t.Fatalf("writes = %v, want none when the agent applies no runtime configuration", got)
			}
			if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn"] {
				t.Fatalf("stored pinned after the run = %v, want both pins left in place", got)
			}
			if len(status.UnpinnedSpecIDs) != 0 {
				t.Fatalf("unpinned_spec_ids = %v, want none", status.UnpinnedSpecIDs)
			}
			assertLoggedOnce(t, logs, "INFO", "benchmark: running agent models are not stopped in this run", map[string]string{"server_id": "srv1"})
		})
	}
}

// TestBenchmarkRunStartSettlesALeftoverLease: a run that may stop reconciles
// a lease an earlier run left on its server before it writes anything, and
// writes nothing when the reconcile, or the lease read, fails.
func TestBenchmarkRunStartSettlesALeftoverLease(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // keeps the run's Warn and Info lines out of the test output
	leftover := func() (stopAllSpec, *portal.BenchmarkOverrideLease) {
		x := stopAllSpecOf("mapx", "stopped", 0)
		x.adminState = vramAdminStateForceStopped
		return x, &portal.BenchmarkOverrideLease{ClearForceStopped: []string{"rs_mapx"}}
	}
	t.Run("a leftover is reconciled before the run's own writes", func(t *testing.T) {
		x, lease := leftover()
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "running", 1301)}, lease: lease})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		reconcile, runLease, stop := f.events.index("clear [rs_mapx]", 0), f.events.index("lease clear=[rs_map1] repin=[]", 0), f.events.index("stop [rs_map1]", 0)
		if reconcile < 0 || runLease < reconcile || stop < runLease {
			t.Fatalf("reconcile at %d, the run's lease at %d, its stop at %d: the leftover is cleared first (events %v)", reconcile, runLease, stop, f.events.with("clear ", "stop ", "lease"))
		}
		if got := f.docs(); !slices.Equal(got, []string{"doc []", "doc [rs_map1]", "doc []"}) {
			t.Fatalf("documents = %v, want the reconcile's, then the run's stop and clear, in write order", got)
		}
		if got := f.adminState(t, "rs_mapx"); got != "" {
			t.Fatalf("rs_mapx admin_state = %q, want the leftover cleared", got)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})
	t.Run("a leftover the reconcile cannot settle keeps the run from writing", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		x, lease := leftover()
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "running", 1302)}, lease: lease})
		f.portal.failClear["rs_mapx"] = errors.New("store: disk I/O error")

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); !slices.Equal(got, []string{"clear [rs_mapx]"}) {
			t.Fatalf("batches = %v, want only the reconcile's clear", got)
		}
		if got := f.lease(t); !slices.Equal(got.ClearForceStopped, []string{"rs_mapx"}) || len(got.Repin) != 0 {
			t.Fatalf("lease after the run = %+v, want clear_force_stopped [rs_mapx] kept", got)
		}
		assertLoggedOnce(t, logs, "INFO", "benchmark: an earlier benchmark's override lease is still owed; nothing is unpinned or stopped in this run", map[string]string{"server_id": "srv1"})
	})
	t.Run("a leftover re-pin is settled before the run reads the specs it unpins", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		x, lease := leftover()
		lease.Repin = []string{"rs_map1"} // a run that died left t1 unpinned
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "running", 1304)}, lease: lease})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		want := []string{
			"clear [rs_mapx]", "repin [rs_map1]", "lease clear=[] repin=[]", // the reconcile
			"lease clear=[] repin=[rs_map1]", "unpin [rs_map1]", // the run's unpin of what the reconcile pinned
			"lease clear=[rs_map1] repin=[rs_map1]", "stop [rs_map1]", "clear [rs_map1]", "lease clear=[] repin=[rs_map1]",
			"repin [rs_map1]", "lease clear=[] repin=[]",
		}
		if got := f.events.with("clear ", "stop ", "unpin ", "repin ", "lease"); !slices.Equal(got, want) {
			t.Fatalf("writes = %v, want %v", got, want)
		}
		// The reconcile's two writes, the unpin and even the stop follow each
		// other closely enough to share a document. The last three documents
		// are the stop's, which carries the unpin, the clear's and the re-pin's.
		waitFor(t, func() bool {
			docs, pins := f.appliedDocs()
			return slices.Contains(docs, "doc [rs_map1]") && pins[len(pins)-1] == "pins [rs_map1]"
		})
		docs, pins := f.appliedDocs()
		if len(docs) < 3 || !slices.Equal(docs[len(docs)-3:], []string{"doc [rs_map1]", "doc []", "doc []"}) || !slices.Equal(pins[len(pins)-3:], []string{"pins []", "pins []", "pins [rs_map1]"}) {
			t.Fatalf("documents = %v, pinned specs = %v: want them to end in the stop without the pin, the clear and the re-pin", docs, pins)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})
	t.Run("a failed read after the reconcile keeps the run from writing", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		x, lease := leftover()
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "running", 1305)}, lease: lease})
		f.routes.onSpecsRead = func(n int, specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
			if n == 2 {
				return nil, errors.New("store: connection reset")
			}
			return specs, nil
		}

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("clear ", "stop ", "unpin ", "repin ", "lease"); !slices.Equal(got, []string{"clear [rs_mapx]", "repin []", "lease clear=[] repin=[]"}) {
			t.Fatalf("writes = %v, want only the reconcile's", got)
		}
		found := false
		for _, r := range logs.Snapshot() {
			if r.Level == "WARN" && r.Msg == "benchmark: could not read the launch specs; nothing is unpinned or stopped in this run" {
				found = true
			}
		}
		if !found {
			t.Fatal("no Warn for the failed read after the reconcile")
		}
	})
	t.Run("a failed lease read keeps the run from writing", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 1303)}})
		f.portal.failLeaseRead = errors.New("store: connection reset")

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("stop ", "clear ", "lease"); len(got) != 0 {
			t.Fatalf("writes = %v, want none without the lease row's content", got)
		}
		found := false
		for _, r := range logs.Snapshot() {
			if r.Level == "WARN" && r.Msg == "benchmark: could not read the override lease; nothing is unpinned or stopped in this run" {
				found = true
			}
		}
		if !found {
			t.Fatal("no Warn for the failed lease read")
		}
	})
}

// TestBenchmarkRunRewritesTheLeaseAtItsEnd: a run that unpinned nothing but
// stopped something, and whose lease write after the clear failed, rewrites
// the row at its end, which releases it.
func TestBenchmarkRunRewritesTheLeaseAtItsEnd(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 1401)}})
	f.portal.failLease = func(n int) error {
		if n == 2 {
			return errors.New("store: database is locked")
		}
		return nil
	}

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	want := []string{"lease clear=[rs_map1] repin=[]", "lease! clear=[] repin=[]", "lease clear=[] repin=[]"}
	if got := f.events.with("lease"); !slices.Equal(got, want) {
		t.Fatalf("lease writes = %v, want %v", got, want)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released by the run's last write", got)
	}
	assertLoggedOnce(t, logs, "WARN", "benchmark: could not update the override lease after a clear", map[string]string{"server_id": "srv1", "err": "store: database is locked"})
}

// TestBenchmarkRunRewritesTheLeaseAtItsEndAndWarnsWhenItFails: when the
// end-of-run rewrite fails, the run says so; the row keeps what the clear's
// rewrite stored, here nothing.
func TestBenchmarkRunRewritesTheLeaseAtItsEndAndWarnsWhenItFails(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 1402)}})
	f.portal.failLease = func(n int) error {
		if n == 3 {
			return errors.New("store: database is locked")
		}
		return nil
	}

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	want := []string{"lease clear=[rs_map1] repin=[]", "lease clear=[] repin=[]", "lease! clear=[] repin=[]"}
	if got := f.events.with("lease"); !slices.Equal(got, want) {
		t.Fatalf("lease writes = %v, want %v", got, want)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released by the clear's rewrite", got)
	}
	assertLoggedOnce(t, logs, "WARN", "benchmark: could not rewrite the override lease after the run", map[string]string{"server_id": "srv1", "err": "store: database is locked"})
}

// TestBenchmarkRunStartGatesTurnStopsOff: a run whose launch specs cannot be
// read at its start writes nothing.
func TestBenchmarkRunStartGatesTurnStopsOff(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 1501)}})
	f.routes.onSpecsRead = func(n int, specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
		if n == 1 {
			return nil, errors.New("store: connection reset")
		}
		return specs, nil
	}

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	if got := f.events.with("stop ", "clear ", "lease"); len(got) != 0 {
		t.Fatalf("writes = %v, want none when the run's spec read failed", got)
	}
	found := false
	for _, r := range logs.Snapshot() {
		if r.Level == "WARN" && r.Msg == "benchmark: could not read the launch specs; nothing is unpinned or stopped in this run" {
			found = true
		}
	}
	if !found {
		t.Fatal("no Warn for the failed spec read")
	}
}

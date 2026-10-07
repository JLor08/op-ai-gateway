// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// stopAllSpecOf is a launch spec of the stop-all fixture: mapping mappingID,
// spec rs_<mappingID>, upstream model m<suffix> (map1 -> m1). Its child loads
// in 300 ms and exits in 20 ms.
func stopAllSpecOf(mappingID, state string, pid int) stopAllSpec {
	return stopAllSpec{mappingID: mappingID, fakeAgentRuntimeSpec: fakeAgentRuntimeSpec{
		id: "rs_" + mappingID, model: "m" + strings.TrimPrefix(mappingID, "map"), state: state, pid: pid,
		load: 300 * time.Millisecond, exit: 20 * time.Millisecond,
	}}
}

// stopAllLogged returns the records at level with msg that name mappingID.
func stopAllLogged(buf *logbuffer.Buffer, level, msg, mappingID string) []logbuffer.Record {
	var out []logbuffer.Record
	for _, r := range buf.Snapshot() {
		if id, _ := r.Attrs["mapping_id"].(string); r.Level == level && r.Msg == msg && id == mappingID {
			out = append(out, r)
		}
	}
	return out
}

const (
	logNotStopped   = "benchmark: running agent models not stopped; load time not measured"
	logNotConfirmed = "benchmark: the stopped agent models were not confirmed quiet; load time not measured"
	logLeaseFailed  = "benchmark: could not record the override lease; running agent models are not stopped for this target"
	logTakenOver    = "benchmark: a clear found another admin_state on a launch spec and left it alone: taken over during the run, never stopped, or already cleared"
)

// assertLoggedOnce checks that exactly one record at level with msg carries
// every attribute of want, each a string (an error attribute is captured as
// its message).
func assertLoggedOnce(t *testing.T, buf *logbuffer.Buffer, level, msg string, want map[string]string) {
	t.Helper()
	n := 0
	for _, r := range buf.Snapshot() {
		if r.Level != level || r.Msg != msg {
			continue
		}
		match := true
		for k, v := range want {
			if got, _ := r.Attrs[k].(string); got != v {
				match = false
			}
		}
		if match {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%s %q with %v logged %d times, want once (records: %v)", level, msg, want, n, buf.Snapshot())
	}
}

// assertLoggedReason checks that the stop-all logged msg at Info for
// mappingID with this reason.
func assertLoggedReason(t *testing.T, buf *logbuffer.Buffer, msg, mappingID, reason string) {
	t.Helper()
	recs := stopAllLogged(buf, "INFO", msg, mappingID)
	if len(recs) != 1 || recs[0].Attrs["reason"] != reason {
		t.Fatalf("Info %q for %s = %v, want exactly one with reason %q", msg, mappingID, recs, reason)
	}
}

// TestManualSpeedRunStopsEveryRunningModelBeforeEachTarget: under a closed
// co-residency matrix, a manual speed run over a resident target t1 and a cold
// target t2, next to an unmeasured pinned neighbour n that runs and takes
// 400 ms to exit, unpins n for the run, stops every running model before each
// target's cold pass, records each target's own load time, without n's exit
// in it, and pins n again at its end, which starts n.
func TestManualSpeedRunStopsEveryRunningModelBeforeEachTarget(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // the unpin's Warn and the re-pin's Info
	n := stopAllSpecOf("mapn", "running", 501)
	n.exit = 400 * time.Millisecond
	n.pinned = true
	t1 := stopAllSpecOf("map1", "running", 502)
	t2 := stopAllSpecOf("map2", "stopped", 0)
	t2.load = 200 * time.Millisecond
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{n, t1, t2}, closedMatrix: true, clearDelay: 150 * time.Millisecond})

	status := f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

	wantBatches := []string{"stop [rs_map1 rs_mapn]", "clear [rs_map1 rs_mapn]", "stop [rs_map1]", "clear [rs_map1]"}
	if got := f.batches(); !slices.Equal(got, wantBatches) {
		t.Fatalf("batches = %v, want %v: one stop and one clear before each target", got, wantBatches)
	}
	if got := f.portal.notifiesPerBatch(); !slices.Equal(got, []int{1, 1, 1, 1}) {
		t.Fatalf("notifications per batch = %v, want one each", got)
	}
	waitFor(t, func() bool { docs, _ := f.appliedDocs(); return len(docs) >= 6 })
	docs, pins := f.appliedDocs()
	wantDocs := []string{"doc []", "doc [rs_map1 rs_mapn]", "doc []", "doc [rs_map1]", "doc []", "doc []"}
	if !slices.Equal(docs, wantDocs) {
		t.Fatalf("documents the agent applied = %v, want exactly one per batch: %v", docs, wantDocs)
	}
	wantPins := []string{"pins []", "pins []", "pins []", "pins []", "pins []", "pins [rs_mapn]"}
	if got := pins; !slices.Equal(got, wantPins) {
		t.Fatalf("pinned specs of each document = %v, want %v: the unpin's document first, the re-pin's last", got, wantPins)
	}
	if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_mapn]", "repin [rs_mapn]"}) {
		t.Fatalf("pinned batches = %v, want n's unpin and its re-pin", got)
	}
	if got := f.portal.notifiesPerPinBatch(); !slices.Equal(got, []int{1, 1}) {
		t.Fatalf("notifications per pinned batch = %v, want one each", got)
	}
	unpinLease, unpin := f.events.index("lease clear=[] repin=[rs_mapn]", 0), f.events.index("unpin [rs_mapn]", 0)
	if unpinLease < 0 || unpinLease > unpin || unpin > f.events.index("stop [rs_map1 rs_mapn]", 0) {
		t.Fatalf("the unpin's lease at %d, the unpin at %d: the lease names n first, and n is unpinned before the first stop", unpinLease, unpin)
	}
	if repin := f.events.index("repin [rs_mapn]", 0); repin < f.events.lastIndex("chat m2 200") {
		t.Fatalf("n's re-pin at %d, t2's last chat at %d: n is pinned again after the whole run", repin, f.events.lastIndex("chat m2 200"))
	}
	for i, stop := range []string{"stop [rs_map1 rs_mapn]", "stop [rs_map1]"} {
		lease := f.events.index("lease clear="+strings.TrimPrefix(stop, "stop ")+" repin=[rs_mapn]", 0)
		if lease < 0 || lease > f.events.index(stop, 0) {
			t.Fatalf("stop %d: the lease naming its stop set at %d, the batch at %d: the lease is written first", i+1, lease, f.events.index(stop, 0))
		}
	}

	// t1: stopped and cleared before its first chat, which rides the 503
	// until the clear reaches the agent.
	first503, clear1 := f.events.index("chat m1 503", 0), f.events.index("clear [rs_map1 rs_mapn]", 0)
	if first503 < 0 || first503 < clear1 || f.events.index("chat m1 200", 0) < first503 {
		t.Fatalf("t1's first 503 at %d, its clear at %d: the clear runs before the cold pass, and the cold pass meets the 503 first", first503, clear1)
	}
	if got := f.events.count("chat m1 503"); got < 2 {
		t.Fatalf("t1's cold pass met %d 503s, want at least 2 (the clear reaches the agent 150 ms late)", got)
	}
	if got := f.events.count("chat m1 200"); got != 2 {
		t.Fatalf("t1 served %d chats, want 2 (cold and warm)", got)
	}
	if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
		t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load within 20 %%, without n's 400 ms exit", row.LoadTimeMS, row.Error)
	}

	// t2: the snapshot it starts from still shows t1 in flight, because the
	// agent sends no frame when a request ends; the stop runs on a fresh frame.
	stop2 := f.events.index("stop [rs_map1]", 0)
	if stop2 < f.events.lastIndex("chat m1 200") || stop2 > f.events.index("chat m2 200", 0) {
		t.Fatalf("t2's stop-all at %d: it must come after t1's passes and before t2's first chat", stop2)
	}
	if got := f.events.count("chat m2 503"); got != 0 {
		t.Fatalf("t2 met %d 503s, want none: t2 was not in its stop set, so its cold pass is one stream", got)
	}
	if row := f.speedRow(t, "map2"); row.Error != "" || !within(row.LoadTimeMS, t2.load, 20) {
		t.Fatalf("t2 history = load %d ms, error %q; want its own 200 ms load within 20 %%", row.LoadTimeMS, row.Error)
	}

	if !slices.Equal(status.StoppedSpecIDs, []string{"rs_map1", "rs_mapn"}) {
		t.Fatalf("stopped_spec_ids = %v, want [rs_map1 rs_mapn]", status.StoppedSpecIDs)
	}
	if !slices.Equal(status.UnpinnedSpecIDs, []string{"rs_mapn"}) || len(status.RepinFailed) != 0 {
		t.Fatalf("unpinned_spec_ids / repin_failed = %v / %v, want [rs_mapn] / []", status.UnpinnedSpecIDs, status.RepinFailed)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
	// The run restarts nothing it stopped: t1, never pinned, gets no request
	// after its passes and stays stopped. n, pinned again, starts at the
	// re-pin's Apply.
	if state, pid := f.agent.stateOf("rs_map1"); state != "stopped" || pid != 0 {
		t.Fatalf("t1 after the run = %s pid %d, want stopped", state, pid)
	}
	if state, pid := f.agent.stateOf("rs_mapn"); pid == 0 {
		t.Fatalf("n after the run = %s pid %d, want it started by the re-pin's Apply", state, pid)
	}
	if got := f.events.with("other "); len(got) != 0 {
		t.Fatalf("other requests = %v, want none (no ensure, no unload)", got)
	}
}

// TestPreStopEligibility: every stop-all check runs before anything is written.
// A failed check writes no spec and no lease entry for that target, logs its
// reason, and leaves stops on, so the next target is still stopped. The
// scenario: a running neighbour x, a resident target t1, a cold target t2.
func TestPreStopEligibility(t *testing.T) {
	// specsRead2 changes what the run's second spec read returns: t1's
	// eligibility re-read (the first is the run's own, at its start).
	specsRead2 := func(change func(specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error)) func(f *stopAllFixture) {
		return func(f *stopAllFixture) {
			f.routes.onSpecsRead = func(n int, specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
				if n != 2 {
					return specs, nil
				}
				return change(slices.Clone(specs))
			}
		}
	}
	setOn := func(id string, set func(*routing.RuntimeSpec)) func(specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
		return func(specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
			for i := range specs {
				if specs[i].ID == id {
					set(&specs[i])
				}
			}
			return specs, nil
		}
	}
	// flipFor2 sets a volatile gate on t1's eligibility read and clears it again
	// on t2's.
	flipFor2 := func(set func(f *stopAllFixture, on bool)) func(f *stopAllFixture) {
		return func(f *stopAllFixture) {
			f.routes.onSpecsRead = func(n int, specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
				switch n {
				case 2:
					set(f, true)
				case 3:
					set(f, false)
				}
				return specs, nil
			}
		}
	}
	for _, tc := range []struct {
		name     string
		agent    func(specs []stopAllSpec)
		prepare  func(f *stopAllFixture)
		noSpec   bool
		reason   string // "" means the lease-write failure, which is a Warn
		nextStop string
	}{
		{
			name:  "traffic in flight in every frame of the selection window",
			agent: func(specs []stopAllSpec) { specs[0].direct = 1 },
			prepare: func(f *stopAllFixture) {
				f.agent.onEnd = func(model string, served int) {
					if model == "m1" && served == 2 {
						f.agent.setDirect("rs_mapx", 0)
					}
				}
			},
			reason: benchmarkStopReasonInFlight, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name: "no frame in the selection window",
			prepare: func(f *stopAllFixture) {
				f.agent.freeze(true)
				f.agent.onEnd = func(model string, served int) {
					if model == "m1" && served == 2 {
						f.agent.freeze(false)
					}
				}
			},
			reason: benchmarkStopReasonNoFrame, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "file mode, reported after the run started",
			prepare: flipFor2(func(f *stopAllFixture, on bool) { f.srv.RuntimeStatus.SetFileMode("srv1", on) }),
			reason:  benchmarkStopReasonIsolationUnavailable, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name: "runtime_manager withdrawn after the run started",
			prepare: flipFor2(func(f *stopAllFixture, on bool) {
				if on {
					f.srv.AgentFeatures.Set("srv1", nil)
					return
				}
				f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager"})
			}),
			reason: benchmarkStopReasonIsolationUnavailable, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "a stop-set spec carries an override",
			prepare: specsRead2(setOn("rs_mapx", func(s *routing.RuntimeSpec) { s.AdminState = vramAdminStateForceStopped })),
			reason:  benchmarkStopReasonAdminOverride, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "a stop-set spec is pinned in the re-read",
			prepare: specsRead2(setOn("rs_mapx", func(s *routing.RuntimeSpec) { s.Pinned = true })),
			reason:  benchmarkStopReasonPinned, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "the target is pinned in the re-read, though its stale spec is not",
			prepare: specsRead2(setOn("rs_map1", func(s *routing.RuntimeSpec) { s.Pinned = true })),
			reason:  benchmarkStopReasonPinned, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "a cold target is pinned in the re-read",
			agent:   func(specs []stopAllSpec) { specs[1].state, specs[1].pid = "stopped", 0 },
			prepare: specsRead2(setOn("rs_map1", func(s *routing.RuntimeSpec) { s.Pinned = true })),
			reason:  benchmarkStopReasonPinned, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:    "a stop-set spec is disabled",
			prepare: specsRead2(setOn("rs_mapx", func(s *routing.RuntimeSpec) { s.Enabled = false })),
			reason:  benchmarkStopReasonDisabled, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name: "a stop-set spec is unknown to the store",
			prepare: specsRead2(func(specs []routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
				return slices.DeleteFunc(specs, func(s routing.RuntimeSpec) bool { return s.ID == "rs_mapx" }), nil
			}),
			reason: benchmarkStopReasonUnknownSpec, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name: "the re-read fails",
			prepare: specsRead2(func([]routing.RuntimeSpec) ([]routing.RuntimeSpec, error) {
				return nil, errors.New("store: connection reset")
			}),
			reason: benchmarkStopReasonSpecUnreadable, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name:   "the agent reports no row for the target",
			agent:  func(specs []stopAllSpec) { specs[1].unreported = true },
			reason: benchmarkStopReasonNoStatus, nextStop: "stop [rs_mapx]",
		},
		{
			name:   "the target has no launch spec",
			noSpec: true,
			reason: benchmarkStopReasonNoSpec, nextStop: "stop [rs_map1 rs_mapx]",
		},
		{
			name: "the lease write fails",
			prepare: func(f *stopAllFixture) {
				f.portal.failLease = func(n int) error {
					if n == 1 {
						return errors.New("store: database is locked")
					}
					return nil
				}
			},
			nextStop: "stop [rs_map1 rs_mapx]",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := withCapturedSlogAtTheDefaultLevel(t)
			specs := []stopAllSpec{stopAllSpecOf("mapx", "running", 601), stopAllSpecOf("map1", "running", 602), stopAllSpecOf("map2", "stopped", 0)}
			if tc.agent != nil {
				tc.agent(specs)
			}
			f := newStopAllFixture(t, stopAllOpts{specs: specs})
			if tc.prepare != nil {
				tc.prepare(f)
			}
			targets := f.targets(t, "speed", "map1", "map2")
			if tc.noSpec {
				targets[0].spec = routing.RuntimeSpec{}
			}

			f.run(t, "speed", targets)

			t1Done := f.events.lastIndex("chat m1 200")
			for i, ev := range f.events.list() {
				written := strings.HasPrefix(ev.what, "stop ") || (strings.HasPrefix(ev.what, "lease clear=") && !strings.HasPrefix(ev.what, "lease clear=[]"))
				if written && i < t1Done {
					t.Fatalf("event %q at %d, before t1's last chat at %d: nothing may be written for t1", ev.what, i, t1Done)
				}
			}
			if got := f.batches(); len(got) != 2 || got[0] != tc.nextStop {
				t.Fatalf("batches = %v, want t2's stop %q and its clear: stops stay on", got, tc.nextStop)
			}
			if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
				t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
			}
			if tc.reason == "" {
				if recs := stopAllLogged(logs, "WARN", logLeaseFailed, "map1"); len(recs) != 1 {
					t.Fatalf("Warn %q for map1 = %v, want one", logLeaseFailed, recs)
				}
				return
			}
			assertLoggedReason(t, logs, logNotStopped, "map1", tc.reason)
		})
	}
}

// TestPreStopWaitRules: what the stop wait counts as quiet, and what ends it.
func TestPreStopWaitRules(t *testing.T) {
	t.Run("no quiet frame within the bound: cleared, no load time, and stops are off", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		x := stopAllSpecOf("mapx", "running", 701)
		x.stuck = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "stopped", 0), stopAllSpecOf("map2", "stopped", 0)}})
		benchmarkStopWaitBound = 500 * time.Millisecond

		f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_mapx]", "clear [rs_mapx]"}) {
			t.Fatalf("batches = %v, want one stop and its clear, and no stop for t2", got)
		}
		for _, id := range []string{"map1", "map2"} {
			if row := f.speedRow(t, id); row.LoadTimeMS != 0 || row.Error != "" {
				t.Fatalf("%s history = load %d ms, error %q; want no load time (x still holds its pid) and no error", id, row.LoadTimeMS, row.Error)
			}
		}
		assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonStopTimeout)
	})
	t.Run("a stop-set row in backoff with pid 0 counts as quiet", func(t *testing.T) {
		x := stopAllSpecOf("mapx", "running", 702)
		x.exitTo, x.backoff = "backoff", 10*time.Second
		t1 := stopAllSpecOf("map1", "stopped", 0)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, t1}})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_mapx]", "clear [rs_mapx]"}) {
			t.Fatalf("batches = %v, want one stop and its clear", got)
		}
		if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
			t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load: x in backoff with pid 0 is quiet", row.LoadTimeMS, row.Error)
		}
	})
	t.Run("a target whose own row reads backoff is stopped and cleared", func(t *testing.T) {
		t1 := stopAllSpecOf("map1", "backoff", 0)
		t1.backoff = 3 * time.Second
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("mapx", "stopped", 0), t1}, clearDelay: 100 * time.Millisecond})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1]", "clear [rs_map1]"}) {
			t.Fatalf("batches = %v, want the target alone in its stop set", got)
		}
		if got := f.events.count("chat m1 503"); got < 1 {
			t.Fatalf("t1's cold pass met %d 503s, want at least one: it rides the 503 of its own stop", got)
		}
		if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
			t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load, without the rest of its 3 s backoff", row.LoadTimeMS, row.Error)
		}
	})
	for _, tc := range []struct {
		name  string
		state string
		pid   int
	}{
		{"a start_failed row with a pid is not quiet", "start_failed", 4242},
		{"a state this gateway does not recognize is not quiet", "hibernating", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t1 := stopAllSpecOf("map1", "stopped", 0)
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("mapx", tc.state, tc.pid), t1}})

			f.run(t, "speed", f.targets(t, "speed", "map1"))

			if got := f.batches(); !slices.Equal(got, []string{"stop [rs_mapx]", "clear [rs_mapx]"}) {
				t.Fatalf("batches = %v, want x stopped and cleared", got)
			}
			if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
				t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load", row.LoadTimeMS, row.Error)
			}
		})
	}
	t.Run("a row outside the stop set that starts ends the wait at once", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		x := stopAllSpecOf("mapx", "running", 703)
		x.exit = 1200 * time.Millisecond
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("mapy", "stopped", 0), stopAllSpecOf("map1", "stopped", 0), stopAllSpecOf("map2", "stopped", 0)}})
		started := false
		f.agent.onApply = func(doc portal.AgentRuntimeConfigDTO) {
			if !started {
				started = true
				f.agent.setState("rs_mapy", "starting", 777) // a direct client's request
			}
		}

		f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		evs := f.events.list()
		stop, clear := f.events.index("stop [rs_mapx]", 0), f.events.index("clear [rs_mapx]", 0)
		if stop < 0 || clear < 0 {
			t.Fatalf("batches = %v, want t1's stop and clear of x", f.batches())
		}
		if waited := evs[clear].at.Sub(evs[stop].at); waited >= benchmarkStopWaitBound/2 {
			t.Fatalf("the wait lasted %v, want under half its %v bound: a late start ends it at once", waited, benchmarkStopWaitBound)
		}
		if got := f.batches(); len(got) != 4 || !strings.Contains(got[2], "rs_mapy") {
			t.Fatalf("batches = %v, want t2 still stopped (a late start leaves stops on), y among its stop set", got)
		}
		assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonLateStart)
	})
}

// TestPreStopFailedClearIsTheResultError: a clear that fails on a store error
// is the target's result error, before any pass; the lease keeps naming the
// spec, and stops are off for the rest of the run.
func TestPreStopFailedClearIsTheResultError(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // keeps the run's Warn and Info lines out of the test output
	const want = "launch specs may still be force_stopped after the benchmark: rs_map1; clear the overrides in the runtime section"
	t.Run("speed", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 801), stopAllSpecOf("map2", "running", 802), stopAllSpecOf("mapx", "stopped", 0)}})
		f.portal.failClear["rs_map1"] = errors.New("store: disk I/O error")
		// A direct client starts the unmeasured x while map1's clear is written, so
		// map2's stop-all would find a running model if stops were still on.
		var once sync.Once
		f.portal.onBatch = func(kind string, _ []string) {
			if kind == "clear" {
				once.Do(func() { f.agent.setState("rs_mapx", "running", 804) })
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		if len(status.Results) != 2 || status.Results[0].Error != want {
			t.Fatalf("results = %+v, want map1's error %q", status.Results, want)
		}
		if row := f.speedRow(t, "map1"); row.Error != want || row.LoadTimeMS != 0 {
			t.Fatalf("map1 history = load %d ms, error %q; want error %q", row.LoadTimeMS, row.Error, want)
		}
		if got := f.events.count("chat m1 200") + f.events.count("chat m1 503"); got != 0 {
			t.Fatalf("map1 got %d chats, want none: no speed pass runs after a failed clear", got)
		}
		m, err := f.mem.MappingByID(context.Background(), "map1")
		if err != nil || m.GenTokensPerSecond != 50 || m.PromptTokensPerSecond != 900 || m.LoadTimeMS != 1234 {
			t.Fatalf("map1 stored = %v / %v / %d (%v), want 50 / 900 / 1234 untouched", m.GenTokensPerSecond, m.PromptTokensPerSecond, m.LoadTimeMS, err)
		}
		if got := f.lease(t); !slices.Equal(got.ClearForceStopped, []string{"rs_map1"}) || len(got.Repin) != 0 {
			t.Fatalf("lease after the run = %+v, want clear_force_stopped [rs_map1]", got)
		}
		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1 rs_map2]", "clear [rs_map1 rs_map2]"}) {
			t.Fatalf("batches = %v, want no stop for map2, though x runs: a failed clear turns stops off", got)
		}
		// Without a stop, map2 next to the running x is measured without a
		// load time.
		if row := f.speedRow(t, "map2"); row.Error != "" || row.LoadTimeMS != 0 {
			t.Fatalf("map2 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
		}
		assertLoggedOnce(t, logs, "WARN", "benchmark: could not restore an admin override", map[string]string{"spec_id": "rs_map1", "err": "store: disk I/O error"})
	})
	t.Run("both: the capacity ramp still runs and records the router's 503", func(t *testing.T) {
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 803)}})
		f.portal.failClear["rs_map1"] = errors.New("store: disk I/O error")

		status := f.run(t, "both", f.targets(t, "both", "map1"))

		if len(status.Results) != 1 || status.Results[0].Error != want {
			t.Fatalf("results = %+v, want the speed error %q, which the merge prefers", status.Results, want)
		}
		rows, err := f.mem.BenchmarkRunsByMapping(context.Background(), "map1", 10)
		if err != nil {
			t.Fatalf("BenchmarkRunsByMapping: %v", err)
		}
		var capacityErr string
		for _, r := range rows {
			if r.Kind == "capacity" {
				capacityErr = r.Error
			}
		}
		if !strings.Contains(capacityErr, "503") {
			t.Fatalf("capacity history error = %q, want the router's 503", capacityErr)
		}
	})
}

// TestPreStopPartialWriteSkipsTheWait: when not every spec of the stop set was
// written, the stop-all does not wait for the stop set: it waits only until the
// target's own row reads quiet (benchmarkAwaitSpecQuiet; t1 exits in 20 ms, x
// would take 1.2 s) and then clears what it wrote. The target has no load time
// and no error, and a stopped target's cold pass rides the 503 and succeeds.
func TestPreStopPartialWriteSkipsTheWait(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	x := stopAllSpecOf("mapx", "running", 901)
	x.exit = 1200 * time.Millisecond
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, stopAllSpecOf("map1", "running", 902)}, clearDelay: 150 * time.Millisecond})
	f.portal.conflictStop["rs_mapx"] = true

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1 rs_mapx]", "clear [rs_map1]"}) {
		t.Fatalf("batches = %v, want the stop of both and the clear of the one it wrote", got)
	}
	evs := f.events.list()
	stop, clear := f.events.index("stop [rs_map1 rs_mapx]", 0), f.events.index("clear [rs_map1]", 0)
	if waited := evs[clear].at.Sub(evs[stop].at); waited >= benchmarkStopWaitBound/2 {
		t.Fatalf("stop to clear took %v, want no wait", waited)
	}
	if got := f.events.count("chat m1 503"); got < 1 {
		t.Fatalf("t1's cold pass met %d 503s, want at least one: its own stop was written", got)
	}
	if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
		t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released: nothing was written for the conflict", got)
	}
	assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonPartialStop)
}

// TestPreStopTakenOverClearMeasuresNoLoadTime: a clear that finds somebody
// else's override on a stopped spec leaves that override alone and the
// target's start unconfirmed, without an error.
func TestPreStopTakenOverClearMeasuresNoLoadTime(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("mapx", "running", 971), stopAllSpecOf("map1", "stopped", 0)}})
	f.portal.conflictClear["rs_mapx"] = true

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
		t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
	}
	assertLoggedOnce(t, logs, "INFO", logTakenOver, map[string]string{"spec_id": "rs_mapx"})
}

// TestColdPassAfterStopWrapsTheLastError: a cold pass after the target's own
// stop that the clear never reaches ends at the load loop's bound with the
// router's last 503, wrapped.
func TestColdPassAfterStopWrapsTheLastError(t *testing.T) {
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 951)}, dropClears: true})
	oldMax := coldLoadResidentMaxWait
	coldLoadResidentMaxWait = 300 * time.Millisecond
	t.Cleanup(func() { coldLoadResidentMaxWait = oldMax })
	f.srv.streamIdleTimeout = 200 * time.Millisecond
	targets := f.targets(t, "speed", "map1")
	targets[0].app.TimeoutMS = 100

	status := f.run(t, "speed", targets)

	streamer, _ := f.srv.Provider.(provider.StreamingClient)
	target, req := benchmarkTargetReq(targets[0])
	_, _, perr := f.srv.streamOnce(context.Background(), streamer, target, req)
	if perr == nil {
		t.Fatal("a stream for the still force-stopped target succeeded, want the router's 503")
	}
	want := "cold pass after the benchmark's stop: " + perr.Error()
	if len(status.Results) != 1 || status.Results[0].Error != want {
		t.Fatalf("results = %+v, want error %q", status.Results, want)
	}
	if row := f.speedRow(t, "map1"); row.Error != want {
		t.Fatalf("history error = %q, want %q", row.Error, want)
	}
}

// TestBenchmarkRestoreIncludesAFailedWrite: a write that stored the spec row
// and then failed on its GPU rows is undone with the rest of its batch: a
// stop's force_stopped by the deferred clear, an unpin's pinned = false by the
// immediate re-pin.
func TestBenchmarkRestoreIncludesAFailedWrite(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // keeps the run's Warn and Info lines out of the test output
	t.Run("a stop whose write stored the override and then failed is cleared", func(t *testing.T) {
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("mapx", "running", 961), stopAllSpecOf("map1", "stopped", 0)}})
		f.portalRoutes.failGPUsOnce["rs_mapx"] = true

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_mapx]", "clear [rs_mapx]"}) {
			t.Fatalf("batches = %v, want the failed spec cleared too", got)
		}
		if got := f.adminState(t, "rs_mapx"); got != "" {
			t.Fatalf("rs_mapx admin_state after the run = %q, want cleared", got)
		}
		if !slices.Equal(status.StoppedSpecIDs, []string{"rs_mapx"}) {
			t.Fatalf("stopped_spec_ids = %v, want [rs_mapx]: a failed write may have stored the override", status.StoppedSpecIDs)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})
	t.Run("an unpin whose write stored pinned = false and then failed is pinned again at once", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		x := stopAllSpecOf("mapx", "running", 963)
		x.pinned = true
		t1 := stopAllSpecOf("map1", "running", 964)
		t1.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{x, t1}})
		f.portalRoutes.failGPUsOnce["rs_mapx"] = true
		var storedAtRepin []bool
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind == "repin" {
				spec, _, _ := f.mem.RuntimeSpecByID(context.Background(), "rs_mapx")
				storedAtRepin = append(storedAtRepin, spec.Pinned)
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_map1 rs_mapx]", "repin [rs_map1 rs_mapx]"}) {
			t.Fatalf("pinned batches = %v, want the unpin and one immediate re-pin of both, the failed spec included", got)
		}
		if !slices.Equal(storedAtRepin, []bool{false}) {
			t.Fatalf("rs_mapx's stored pinned at each re-pin = %v, want [false]: the failed write stored the row", storedAtRepin)
		}
		for _, id := range []string{"rs_map1", "rs_mapx"} {
			if spec, _, _ := f.mem.RuntimeSpecByID(context.Background(), id); !spec.Pinned {
				t.Fatalf("%s after the run is not pinned, want it pinned again", id)
			}
		}
		if got := f.batches(); len(got) != 0 {
			t.Fatalf("batches = %v, want none: a failed unpin turns every stop off", got)
		}
		if len(status.UnpinnedSpecIDs) != 0 || len(status.RepinFailed) != 0 || status.Error != "" {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want none: nothing stays unpinned", status.UnpinnedSpecIDs, status.RepinFailed, status.Error)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})
}

// TestPreStopWaitsForTheTargetsOwnStopBeforeItsColdPass: when the stop-all
// returns without a confirmed quiet frame (a partial stop, a late start), the
// stop may not have reached the agent yet. The cold pass waits until the
// target's own row is quiet, so a stop that lands late never fails the warm
// pass. The agent applies the stop 80 ms late, and every pass streams for
// 150 ms, so a cold pass started at once would be served by the still-running
// target and the stop would land under it.
func TestPreStopWaitsForTheTargetsOwnStopBeforeItsColdPass(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // keeps the run's Warn and Info lines out of the test output
	for _, tc := range []struct {
		name    string
		prepare func(f *stopAllFixture)
	}{
		{"a partial stop", func(f *stopAllFixture) { f.portal.conflictStop["rs_mapx"] = true }},
		{"a late start", func(f *stopAllFixture) {
			f.portal.onBatch = func(kind string, ids []string) {
				if kind == "stop" {
					f.agent.setState("rs_mapy", "starting", 991) // a direct client's request
				}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := stopAllSpecOf("mapx", "running", 981)
			x.exit = 1200 * time.Millisecond
			f := newStopAllFixture(t, stopAllOpts{
				specs:      []stopAllSpec{x, stopAllSpecOf("mapy", "stopped", 0), stopAllSpecOf("map1", "running", 982)},
				clearDelay: 150 * time.Millisecond, stopDelay: 80 * time.Millisecond, streamTime: 150 * time.Millisecond,
			})
			tc.prepare(f)

			f.run(t, "speed", f.targets(t, "speed", "map1"))

			if row := f.speedRow(t, "map1"); row.Error != "" || row.LoadTimeMS != 0 {
				t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
			}
			if first503, firstServed := f.events.index("chat m1 503", 0), f.events.index("chat m1 200", 0); first503 < 0 || firstServed < first503 {
				t.Fatalf("t1's first 503 at %d, first served chat at %d: the cold pass must start after the stop landed and ride its 503", first503, firstServed)
			}
		})
	}
}

// TestPreStopCancelledRunStillClearsAndRewritesTheLease: a run cancelled
// after its stop batch still clears the override it wrote, rewrites the lease
// after the clear, and rewrites it again at its end, each on a context that is
// not cancelled with the run. The portal double fails every call on a done
// context, as a real store does. The run is cancelled when the agent applies
// the stop, while the stop-all waits for t1, which takes 1 s to exit.
func TestPreStopCancelledRunStillClearsAndRewritesTheLease(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	t1 := stopAllSpecOf("map1", "running", 1601)
	t1.exit = time.Second
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1}})
	var once sync.Once
	f.agent.onApply = func(doc portal.AgentRuntimeConfigDTO) {
		for _, d := range doc.Specs {
			if d.AdminState == vramAdminStateForceStopped {
				once.Do(f.cancelRun)
			}
		}
	}

	f.run(t, "speed", f.targets(t, "speed", "map1"))

	if got := f.events.with("stop", "clear"); !slices.Equal(got, []string{"stop [rs_map1]", "clear [rs_map1]"}) {
		t.Fatalf("batches = %v, want the stop and a clear that reaches the store after the cancel", got)
	}
	if got := f.adminState(t, "rs_map1"); got != "" {
		t.Fatalf("rs_map1 admin_state after the cancelled run = %q, want cleared", got)
	}
	want := []string{"lease clear=[rs_map1] repin=[]", "lease clear=[] repin=[]", "lease clear=[] repin=[]"}
	if got := f.events.with("lease"); !slices.Equal(got, want) {
		t.Fatalf("lease writes = %v, want %v: the clear's rewrite and the end-of-run rewrite both reach the store", got, want)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
	assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonCanceled)
}

// TestPreStopFailedLeaseWriteLeavesNoStaleEntry: a stop-all whose lease write
// failed writes nothing and owes nothing, so the lease rows of a later
// target's stop-all, and the row the run leaves, name only what a stop batch
// wrote. A direct client's child y runs at t1's stop-all and has exited by
// t2's, so t1's stop set is not t2's.
func TestPreStopFailedLeaseWriteLeavesNoStaleEntry(t *testing.T) {
	withCapturedSlogAtTheDefaultLevel(t) // keeps the run's Warn and Info lines out of the test output
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{
		stopAllSpecOf("mapx", "running", 1701), stopAllSpecOf("mapy", "running", 1702),
		stopAllSpecOf("map1", "running", 1703), stopAllSpecOf("map2", "stopped", 0),
	}})
	f.portal.failLease = func(n int) error {
		if n != 1 {
			return nil
		}
		f.agent.setState("rs_mapy", "stopped", 0)
		return errors.New("store: database is locked")
	}

	f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

	want := []string{
		"lease! clear=[rs_map1 rs_mapx rs_mapy] repin=[]",
		"lease clear=[rs_map1 rs_mapx] repin=[]",
		"lease clear=[] repin=[]",
		"lease clear=[] repin=[]",
	}
	if got := f.events.with("lease"); !slices.Equal(got, want) {
		t.Fatalf("lease writes = %v, want %v: t1's failed entry is not carried into t2's rows", got, want)
	}
	if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1 rs_mapx]", "clear [rs_map1 rs_mapx]"}) {
		t.Fatalf("batches = %v, want only t2's stop and clear", got)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
}

// TestPreStopTargetQuietWaitRules: after a partial stop, the wait until the
// target's own row reads quiet (benchmarkAwaitSpecQuiet) runs only when the
// target's own write succeeded, and its expiry turns stops off for the rest of
// the run.
func TestPreStopTargetQuietWaitRules(t *testing.T) {
	t.Run("a target that is not quiet within the bound turns stops off", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		t1 := stopAllSpecOf("map1", "running", 1801)
		t1.exit = 1200 * time.Millisecond
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("mapx", "running", 1802), t1, stopAllSpecOf("map2", "stopped", 0)}})
		f.portal.conflictStop["rs_mapx"] = true
		benchmarkStopWaitBound = 300 * time.Millisecond

		f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1 rs_mapx]", "clear [rs_map1]"}) {
			t.Fatalf("batches = %v, want t1's stop and clear only: the expired wait turns stops off, so t2 next to the running x and t1 is not stopped", got)
		}
		assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonPartialStop)
	})
	t.Run("a target whose own stop write failed is not waited for", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		t2 := stopAllSpecOf("map2", "stopped", 0)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 1803), t2}})
		f.portal.failStopOnce["rs_map1"] = errors.New("store: disk I/O error")

		f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		wantBatches := []string{"stop [rs_map1]", "clear [rs_map1]", "stop [rs_map1]", "clear [rs_map1]"}
		if got := f.batches(); !slices.Equal(got, wantBatches) {
			t.Fatalf("batches = %v, want %v: t1's failed write never stopped it, so its row is not awaited and stops stay on for t2", got, wantBatches)
		}
		evs := f.events.list()
		stop, clear := f.events.index("stop [rs_map1]", 0), f.events.index("clear [rs_map1]", 0)
		if waited := evs[clear].at.Sub(evs[stop].at); waited >= benchmarkStopWaitBound/2 {
			t.Fatalf("t1's stop to clear took %v, want no wait for a target whose own write failed", waited)
		}
		if row := f.speedRow(t, "map2"); row.Error != "" || !within(row.LoadTimeMS, t2.load, 20) {
			t.Fatalf("t2 history = load %d ms, error %q; want its own 300 ms load within 20 %%", row.LoadTimeMS, row.Error)
		}
		// The clear of the spec the failed write never stopped meets "", not
		// force_stopped, and leaves it alone.
		assertLoggedOnce(t, logs, "INFO", logTakenOver, map[string]string{"spec_id": "rs_map1"})
	})
}

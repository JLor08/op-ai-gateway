// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"maps"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// SetBenchmarkRuntimeSpecsPinned records every batched pinned write in the
// stop-all fixture's event log ("unpin [ids]" for pinned=false, "repin [ids]"
// for pinned=true) before it reaches the service, and fails the specs that
// failUnpin and failRepin name: they are reported as Failed, and nothing is
// written for them. On a done context it records "unpin! [ids]" or
// "repin! [ids]" and reports every spec as Failed without reaching the
// service, as a real store fails the write.
func (p *stopAllPortal) SetBenchmarkRuntimeSpecsPinned(ctx context.Context, specIDs []string, expectedPinned, pinned bool) (portal.BenchmarkSpecsOutcome, error) {
	kind := "unpin"
	if pinned {
		kind = "repin"
	}
	ids := slices.Clone(specIDs)
	slices.Sort(ids)
	if err := ctx.Err(); err != nil {
		p.events.add("%s! %v", kind, ids)
		out := portal.BenchmarkSpecsOutcome{Failed: specIDs, Errs: map[string]error{}}
		for _, id := range specIDs {
			out.Errs[id] = err
		}
		return out, err
	}
	p.events.add("%s %v", kind, ids)
	p.mu.Lock()
	hook := p.onPinBatch
	p.mu.Unlock()
	if hook != nil {
		hook(kind, ids)
	}
	p.mu.Lock()
	var pass []string
	injected := map[string]error{}
	for _, id := range specIDs {
		if err := p.pinFailureLocked(kind, id); err != nil {
			injected[id] = err
			continue
		}
		pass = append(pass, id)
	}
	p.mu.Unlock()
	before := p.notifies()
	out := portal.BenchmarkSpecsOutcome{}
	var err error
	if len(pass) > 0 {
		out, err = p.API.SetBenchmarkRuntimeSpecsPinned(ctx, pass, expectedPinned, pinned)
	}
	if out.Errs == nil {
		out.Errs = map[string]error{}
	}
	for _, id := range specIDs {
		if ierr, ok := injected[id]; ok {
			out.Errs[id] = ierr
			out.Failed = append(out.Failed, id)
			if err == nil {
				err = ierr
			}
		}
	}
	p.mu.Lock()
	p.pinNotifies = append(p.pinNotifies, p.notifies()-before)
	p.mu.Unlock()
	return out, err
}

// pinFailureLocked is the error a pinned batch of kind injects for id, if
// any, and counts a re-pin failure down.
func (p *stopAllPortal) pinFailureLocked(kind, id string) error {
	if kind == "unpin" {
		return p.failUnpin[id]
	}
	n := p.failRepin[id]
	if n == 0 {
		return nil
	}
	if n > 0 {
		p.failRepin[id] = n - 1
	}
	return errors.New("store: database is locked")
}

func (p *stopAllPortal) notifiesPerPinBatch() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.pinNotifies...)
}

// appliedDocs is every document the fake agent has applied, as its "doc" and
// its "pins" event, from one read of the event log. The fake adds the two
// events of a document one after the other, so a document whose "pins" event
// is not in the log yet is left out.
func (f *stopAllFixture) appliedDocs() (docs, pins []string) {
	for _, ev := range f.events.list() {
		switch {
		case strings.HasPrefix(ev.what, "doc "):
			docs = append(docs, ev.what)
		case strings.HasPrefix(ev.what, "pins "):
			pins = append(pins, ev.what)
		}
	}
	return docs[:len(pins)], pins
}

// pinnedSpecs is the stored pinned value of every launch spec of the fixture's
// application, by spec id.
func (f *stopAllFixture) pinnedSpecs(t *testing.T) map[string]bool {
	t.Helper()
	specs, err := f.mem.RuntimeSpecsByApplication(context.Background(), f.app.ID)
	if err != nil {
		t.Fatalf("RuntimeSpecsByApplication: %v", err)
	}
	out := map[string]bool{}
	for _, spec := range specs {
		out[spec.ID] = spec.Pinned
	}
	return out
}

// unpinLogged returns the captured records at level with exactly msg.
func unpinLogged(buf *logbuffer.Buffer, level, msg string) []logbuffer.Record {
	var out []logbuffer.Record
	for _, r := range buf.Snapshot() {
		if r.Level == level && r.Msg == msg {
			out = append(out, r)
		}
	}
	return out
}

const (
	logUnpinning      = "benchmark: unpinning the server's pinned launch specs for the run; they are pinned again when it ends"
	logUnpinLease     = "benchmark: could not record the override lease; nothing is unpinned or stopped in this run"
	logUnpinFailed    = "benchmark: could not unpin every pinned launch spec; running models are not stopped in this run"
	logRepinFailed    = "benchmark: could not pin a launch spec again after the run; pin it in the runtime section"
	logRepinnedAgain  = "benchmark: pinned launch specs again after the run"
	logFormerlyPinned = "benchmark: a launch spec the run unpinned may still start by itself; load time not measured"
)

// assertFormerlyPinnedLogged checks that the run logged the formerly_pinned
// Info exactly once for mappingID, naming exactly specIDs, and neither of the
// stop-all's other two not-measured Infos for it.
func assertFormerlyPinnedLogged(t *testing.T, logs *logbuffer.Buffer, mappingID string, specIDs []string) {
	t.Helper()
	assertLoggedOnce(t, logs, "INFO", logFormerlyPinned, map[string]string{"mapping_id": mappingID, "reason": benchmarkStopReasonFormerlyPinned})
	recs := stopAllLogged(logs, "INFO", logFormerlyPinned, mappingID)
	if got, _ := recs[0].Attrs["spec_ids"].([]string); !slices.Equal(got, specIDs) {
		t.Fatalf("Info %q spec_ids = %v, want %v", logFormerlyPinned, recs[0].Attrs["spec_ids"], specIDs)
	}
	for _, msg := range []string{logNotStopped, logNotConfirmed} {
		if other := stopAllLogged(logs, "INFO", msg, mappingID); len(other) != 0 {
			t.Fatalf("Info %q = %v, want none: formerly_pinned has an Info of its own", msg, other)
		}
	}
}

// TestManualSpeedRunUnpinsEveryPinnedSpecOnTheServer: a manual speed run lifts
// every enabled pinned spec of the server's agent application for the run, the
// measured target included, in one batch that the override lease names before
// it is written; a disabled spec's pin and force_running act on nothing. The
// stop-all then stops the formerly pinned specs, the target gets its own load
// time, and the run pins exactly those specs again while it still holds the
// server, and releases the lease.
func TestManualSpeedRunUnpinsEveryPinnedSpecOnTheServer(t *testing.T) {
	t.Run("every enabled pinned spec is unpinned for the run and pinned again inside the reservation", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		tgt := stopAllSpecOf("map1", "running", 601)
		tgt.pinned = true
		n := stopAllSpecOf("mapn", "running", 602)
		n.pinned = true
		u := stopAllSpecOf("mapu", "stopped", 0)
		d := stopAllSpecOf("mapd", "stopped", 0)
		d.pinned, d.disabled, d.unreported = true, true, true // not in the document
		d.adminState = benchmarkAdminStateForceRunning        // nor is its force_running: it turns no stop off
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{tgt, n, u, d}, clearDelay: 150 * time.Millisecond})
		var duringRun BenchmarkStatus
		var pinnedDuringRun map[string]bool
		f.portal.onBatch = func(kind string, _ []string) {
			if kind == "stop" {
				duringRun, pinnedDuringRun = f.srv.Benchmarks.Status("srv1"), f.pinnedSpecs(t)
			}
		}
		var busyAtRepin []bool
		var targetPIDAtRepin int
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind == "repin" {
				busyAtRepin = append(busyAtRepin, f.srv.Benchmarks.ServerBusy("srv1"))
				_, targetPIDAtRepin = f.agent.stateOf("rs_map1")
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		both := []string{"rs_map1", "rs_mapn"}
		lease, unpin := f.events.index("lease clear=[] repin=[rs_map1 rs_mapn]", 0), f.events.index("unpin [rs_map1 rs_mapn]", 0)
		if leases := f.events.with("lease"); len(leases) == 0 || leases[0] != "lease clear=[] repin=[rs_map1 rs_mapn]" || unpin < 0 || lease > unpin {
			t.Fatalf("writes = %v: want the first lease write to name exactly the enabled pinned specs, before the unpin batch", f.events.with("lease", "unpin ", "stop "))
		}
		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_map1 rs_mapn]", "repin [rs_map1 rs_mapn]"}) {
			t.Fatalf("pinned batches = %v, want one unpin and one re-pin of exactly the enabled pinned specs", got)
		}
		if got := f.portal.notifiesPerPinBatch(); !slices.Equal(got, []int{1, 1}) {
			t.Fatalf("notifications per pinned batch = %v, want one each", got)
		}
		if !slices.Equal(busyAtRepin, []bool{true}) {
			t.Fatalf("server busy at each re-pin = %v, want [true]: the re-pin is written inside the reservation", busyAtRepin)
		}
		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1 rs_mapn]", "clear [rs_map1 rs_mapn]"}) {
			t.Fatalf("batches = %v, want the stop-all to stop both formerly pinned specs", got)
		}
		if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, tgt.load, 20) {
			t.Fatalf("target history = load %d ms, error %q; want its own 300 ms load within 20 %%", row.LoadTimeMS, row.Error)
		}
		if !slices.Equal(duringRun.UnpinnedSpecIDs, both) || pinnedDuringRun["rs_map1"] || pinnedDuringRun["rs_mapn"] || !pinnedDuringRun["rs_mapd"] {
			t.Fatalf("at the stop: unpinned_spec_ids %v, stored pinned %v; want %v unpinned and the disabled rs_mapd left pinned", duringRun.UnpinnedSpecIDs, pinnedDuringRun, both)
		}
		if got, want := f.pinnedSpecs(t), map[string]bool{"rs_map1": true, "rs_mapn": true, "rs_mapu": false, "rs_mapd": true}; !maps.Equal(got, want) {
			t.Fatalf("stored pinned after the run = %v, want %v", got, want)
		}
		if !slices.Equal(status.UnpinnedSpecIDs, both) || len(status.RepinFailed) != 0 || status.Error != "" {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want %v / [] / \"\"", status.UnpinnedSpecIDs, status.RepinFailed, status.Error, both)
		}
		if got := f.events.with("lease"); len(got) == 0 || got[len(got)-1] != "lease clear=[] repin=[]" {
			t.Fatalf("lease writes = %v, want the last one to release the lease", got)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
		if recs := unpinLogged(logs, "WARN", logUnpinning); len(recs) != 1 || recs[0].Attrs["server_id"] != "srv1" || !slices.Equal(recs[0].Attrs["spec_ids"].([]string), both) {
			t.Fatalf("Warn %q = %v, want one naming srv1 and %v", logUnpinning, recs, both)
		}
		if recs := unpinLogged(logs, "INFO", logRepinnedAgain); len(recs) != 1 || !slices.Equal(recs[0].Attrs["spec_ids"].([]string), both) {
			t.Fatalf("Info %q = %v, want one naming %v", logRepinnedAgain, recs, both)
		}

		// On the agent, after the re-pin's document: the stopped neighbour
		// starts at the Apply, the target that still runs keeps its child, and
		// the unpinned spec nothing requested stays down.
		waitFor(t, func() bool { return f.events.count("pins [rs_map1 rs_mapn]") > 0 })
		if state, pid := f.agent.stateOf("rs_mapn"); pid == 0 {
			t.Fatalf("n after the re-pin = %s pid %d, want it started", state, pid)
		}
		if _, pid := f.agent.stateOf("rs_map1"); pid == 0 || pid != targetPIDAtRepin {
			t.Fatalf("target pid after the re-pin = %d, want %d: the re-pin leaves a running child alone", pid, targetPIDAtRepin)
		}
		if state, pid := f.agent.stateOf("rs_mapu"); state != "stopped" || pid != 0 {
			t.Fatalf("rs_mapu after the run = %s pid %d, want stopped", state, pid)
		}
	})

	t.Run("a server without a pinned spec unpins nothing and still stops", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 611), stopAllSpecOf("mapu", "stopped", 0)}})

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin "); len(got) != 0 {
			t.Fatalf("pinned batches = %v, want none", got)
		}
		if got := f.events.with("lease"); len(got) == 0 || got[0] != "lease clear=[rs_map1] repin=[]" {
			t.Fatalf("lease writes = %v, want the stop's to be the first", got)
		}
		if len(status.UnpinnedSpecIDs) != 0 || !slices.Equal(status.StoppedSpecIDs, []string{"rs_map1"}) {
			t.Fatalf("unpinned / stopped = %v / %v, want [] / [rs_map1]", status.UnpinnedSpecIDs, status.StoppedSpecIDs)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS == 0 {
			t.Fatal("no load time: stops are on without a pinned spec")
		}
	})

	t.Run("an unpin batch that wrote nothing releases the lease and still stops", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		n := stopAllSpecOf("mapn", "running", 622)
		n.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 621), n}})
		// A write between the run's read and its batch leaves n unpinned, so
		// the batch's compare-and-set has nothing to unpin.
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind != "unpin" {
				return
			}
			spec, _, err := f.mem.RuntimeSpecByID(context.Background(), "rs_mapn")
			if err == nil {
				spec.Pinned = false
				err = f.mem.UpsertRuntimeSpec(context.Background(), spec)
			}
			if err != nil {
				t.Errorf("unpin rs_mapn behind the run: %v", err)
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_mapn]"}) {
			t.Fatalf("pinned batches = %v, want only the unpin: nothing was unpinned, so nothing is pinned again", got)
		}
		leases := f.events.with("lease")
		if len(leases) < 3 || !slices.Equal(leases[:3], []string{"lease clear=[] repin=[rs_mapn]", "lease clear=[] repin=[]", "lease clear=[rs_map1 rs_mapn] repin=[]"}) {
			t.Fatalf("lease writes = %v, want the unpin's, its release, then the stop's", leases)
		}
		if len(status.UnpinnedSpecIDs) != 0 || !slices.Equal(status.StoppedSpecIDs, []string{"rs_map1", "rs_mapn"}) {
			t.Fatalf("unpinned / stopped = %v / %v, want [] / [rs_map1 rs_mapn]", status.UnpinnedSpecIDs, status.StoppedSpecIDs)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})

	t.Run("a partial unpin batch owes a re-pin only for what it wrote", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		n := stopAllSpecOf("mapn", "running", 623)
		n.pinned = true
		n2 := stopAllSpecOf("mapn2", "running", 624)
		n2.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 625), n, n2}})
		// A write between the run's read and its batch leaves n unpinned, so the
		// batch's compare-and-set unpins only n2.
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind != "unpin" {
				return
			}
			spec, _, err := f.mem.RuntimeSpecByID(context.Background(), "rs_mapn")
			if err == nil {
				spec.Pinned = false
				err = f.mem.UpsertRuntimeSpec(context.Background(), spec)
			}
			if err != nil {
				t.Errorf("unpin rs_mapn behind the run: %v", err)
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_mapn rs_mapn2]", "repin [rs_mapn2]"}) {
			t.Fatalf("pinned batches = %v, want the unpin of both and the re-pin of rs_mapn2 alone", got)
		}
		leases := f.events.with("lease")
		if len(leases) < 3 || !slices.Equal(leases[:3], []string{"lease clear=[] repin=[rs_mapn rs_mapn2]", "lease clear=[] repin=[rs_mapn2]", "lease clear=[rs_map1 rs_mapn rs_mapn2] repin=[rs_mapn2]"}) {
			t.Fatalf("lease writes = %v, want the unpin's, its narrowing to rs_mapn2 right after the batch, then the stop's naming only rs_mapn2 under repin", leases)
		}
		if !slices.Equal(status.UnpinnedSpecIDs, []string{"rs_mapn2"}) {
			t.Fatalf("unpinned_spec_ids = %v, want [rs_mapn2]", status.UnpinnedSpecIDs)
		}
		if got := f.pinnedSpecs(t); got["rs_mapn"] || !got["rs_mapn2"] {
			t.Fatalf("stored pinned after the run = %v, want rs_mapn left unpinned, as the other write left it, and rs_mapn2 pinned again", got)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})

	t.Run("a lease that cannot be written unpins and stops nothing", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		tgt := stopAllSpecOf("map1", "running", 631)
		tgt.pinned = true
		n := stopAllSpecOf("mapn", "running", 632)
		n.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{tgt, n}})
		f.portal.failLease = func(n int) error {
			if n == 1 {
				return errors.New("store: database is locked")
			}
			return nil
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin ", "stop ", "clear "); len(got) != 0 {
			t.Fatalf("spec writes = %v, want none without a lease", got)
		}
		if got := f.events.with("lease"); !slices.Equal(got, []string{"lease! clear=[] repin=[rs_map1 rs_mapn]", "lease clear=[] repin=[]"}) {
			t.Fatalf("lease writes = %v, want the failed one and the rewrite at the run's end", got)
		}
		if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn"] {
			t.Fatalf("stored pinned after the run = %v, want both still pinned", got)
		}
		if len(status.UnpinnedSpecIDs) != 0 || len(status.StoppedSpecIDs) != 0 || status.Error != "" {
			t.Fatalf("unpinned / stopped / error = %v / %v / %q, want none", status.UnpinnedSpecIDs, status.StoppedSpecIDs, status.Error)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 {
			t.Fatalf("history load = %d ms, want none for a resident target without a stop", row.LoadTimeMS)
		}
		if recs := unpinLogged(logs, "WARN", logUnpinLease); len(recs) != 1 {
			t.Fatalf("Warn %q = %v, want one", logUnpinLease, recs)
		}
	})
}

// TestUnpinFailureRestoresAndStopsNothing: when the unpin batch fails for a
// spec, the run pins the written ones again at once and stops nothing. A spec
// whose immediate re-pin fails too stays named, in the status and in the
// lease, and the run's end retries it; when that fails as well, it ends in
// repin_failed and the run's error.
func TestUnpinFailureRestoresAndStopsNothing(t *testing.T) {
	three := func() []stopAllSpec {
		var specs []stopAllSpec
		for i, id := range []string{"map1", "mapn1", "mapn2"} {
			s := stopAllSpecOf(id, "running", 701+i)
			s.pinned = true
			specs = append(specs, s)
		}
		return specs
	}
	all := []string{"rs_map1", "rs_mapn1", "rs_mapn2"}

	t.Run("a spec whose re-pin keeps failing ends in repin_failed and the run error", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: three()})
		f.portal.failUnpin["rs_mapn2"] = errors.New("store: disk I/O error")
		f.portal.failRepin["rs_mapn1"] = -1
		var atFinalRepin struct {
			status BenchmarkStatus
			lease  portal.BenchmarkOverrideLease
			pinned map[string]bool
			busy   bool
		}
		repins := 0
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind != "repin" {
				return
			}
			if repins++; repins == 2 {
				atFinalRepin.status, atFinalRepin.lease = f.srv.Benchmarks.Status("srv1"), f.lease(t)
				atFinalRepin.pinned, atFinalRepin.busy = f.pinnedSpecs(t), f.srv.Benchmarks.ServerBusy("srv1")
			}
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		want := []string{"unpin [rs_map1 rs_mapn1 rs_mapn2]", "repin [rs_map1 rs_mapn1 rs_mapn2]", "repin [rs_mapn1]"}
		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, want) {
			t.Fatalf("pinned batches = %v, want %v: the unpin, the immediate re-pin of the written and the failed specs, and the re-pin at the run's end", got, want)
		}
		if got := f.batches(); len(got) != 0 {
			t.Fatalf("batches = %v, want none: a failed unpin turns every stop off", got)
		}
		if !slices.Equal(atFinalRepin.status.UnpinnedSpecIDs, []string{"rs_mapn1"}) || !slices.Equal(atFinalRepin.lease.Repin, []string{"rs_mapn1"}) || len(atFinalRepin.lease.ClearForceStopped) != 0 {
			t.Fatalf("while the run runs: unpinned_spec_ids %v, lease %+v; want both to name only rs_mapn1, the spec that stays unpinned", atFinalRepin.status.UnpinnedSpecIDs, atFinalRepin.lease)
		}
		if got := atFinalRepin.pinned; got["rs_mapn1"] || !got["rs_map1"] || !got["rs_mapn2"] || !atFinalRepin.busy {
			t.Fatalf("at the final re-pin: stored pinned %v, server busy %v; want only rs_mapn1 unpinned, inside the reservation", got, atFinalRepin.busy)
		}
		const wantErr = "launch specs may still be unpinned after the benchmark: rs_mapn1; pin them in the runtime section"
		if !slices.Equal(status.UnpinnedSpecIDs, []string{"rs_mapn1"}) || !slices.Equal(status.RepinFailed, []string{"rs_mapn1"}) || status.Error != wantErr {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want [rs_mapn1] / [rs_mapn1] / %q", status.UnpinnedSpecIDs, status.RepinFailed, status.Error, wantErr)
		}
		if got := f.lease(t); !slices.Equal(got.Repin, []string{"rs_mapn1"}) || len(got.ClearForceStopped) != 0 {
			t.Fatalf("lease after the run = %+v, want repin [rs_mapn1] for the reconciler", got)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
			t.Fatalf("history = load %d ms, error %q; want no load time for a resident target without a stop, and no error", row.LoadTimeMS, row.Error)
		}
		for _, id := range all {
			if _, pid := f.agent.stateOf(id); pid == 0 {
				t.Fatalf("%s after the run has no process, want every model left running", id)
			}
		}
		if recs := unpinLogged(logs, "WARN", logUnpinFailed); len(recs) != 1 {
			t.Fatalf("Warn %q = %v, want one", logUnpinFailed, recs)
		}
		if recs := unpinLogged(logs, "WARN", logRepinFailed); len(recs) != 1 || recs[0].Attrs["spec_id"] != "rs_mapn1" {
			t.Fatalf("Warn %q = %v, want one naming rs_mapn1", logRepinFailed, recs)
		}
	})

	t.Run("a spec whose immediate re-pin fails is pinned again when the run ends", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: three()})
		f.portal.failUnpin["rs_mapn2"] = errors.New("store: disk I/O error")
		f.portal.failRepin["rs_mapn1"] = 1

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin ", "repin "); len(got) != 3 || got[2] != "repin [rs_mapn1]" {
			t.Fatalf("pinned batches = %v, want the unpin, its immediate re-pin and repin [rs_mapn1] at the run's end", got)
		}
		if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn1"] || !got["rs_mapn2"] {
			t.Fatalf("stored pinned after the run = %v, want all three pinned", got)
		}
		if !slices.Equal(status.UnpinnedSpecIDs, []string{"rs_mapn1"}) || len(status.RepinFailed) != 0 || status.Error != "" {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want [rs_mapn1] / [] / \"\"", status.UnpinnedSpecIDs, status.RepinFailed, status.Error)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})

	t.Run("stops stay off even when every spec stays unpinned", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		tgt := stopAllSpecOf("map1", "running", 721)
		tgt.pinned = true
		n := stopAllSpecOf("mapn", "running", 722)
		n.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{tgt, n}})
		f.portalRoutes.failGPUsOnce["rs_mapn"] = true // the unpin stores pinned = false, then fails
		f.portal.failRepin["rs_map1"], f.portal.failRepin["rs_mapn"] = -1, -1

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); len(got) != 0 {
			t.Fatalf("batches = %v, want none: a failed unpin turns every stop off, whatever stays unpinned", got)
		}
		both := []string{"rs_map1", "rs_mapn"}
		const wantErr = "launch specs may still be unpinned after the benchmark: rs_map1, rs_mapn; pin them in the runtime section"
		if !slices.Equal(status.UnpinnedSpecIDs, both) || !slices.Equal(status.RepinFailed, both) || status.Error != wantErr {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want %v / %v / %q", status.UnpinnedSpecIDs, status.RepinFailed, status.Error, both, both, wantErr)
		}
		for _, id := range both {
			if _, pid := f.agent.stateOf(id); pid == 0 {
				t.Fatalf("%s after the run has no process, want it left running", id)
			}
		}
	})
}

// TestUnpinWarnsWhenItsLeaseRewriteFails: when the lease rewrite after an unpin
// batch fails, the run says so and rewrites the row at its end, which releases
// it: after a batch that wrote nothing, and after the immediate re-pin of a
// batch that failed.
func TestUnpinWarnsWhenItsLeaseRewriteFails(t *testing.T) {
	const logLeaseAfterUnpin = "benchmark: could not update the override lease after the unpin; it is rewritten when the run ends"
	failSecondLease := func(f *stopAllFixture) {
		f.portal.failLease = func(n int) error {
			if n == 2 {
				return errors.New("store: database is locked")
			}
			return nil
		}
	}
	t.Run("after a batch that wrote nothing", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		n := stopAllSpecOf("mapn", "running", 641)
		n.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 642), n}})
		failSecondLease(f)
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind != "unpin" {
				return
			}
			spec, _, err := f.mem.RuntimeSpecByID(context.Background(), "rs_mapn")
			if err == nil {
				spec.Pinned = false
				err = f.mem.UpsertRuntimeSpec(context.Background(), spec)
			}
			if err != nil {
				t.Errorf("unpin rs_mapn behind the run: %v", err)
			}
		}

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		leases := f.events.with("lease")
		if len(leases) < 3 || !slices.Equal(leases[:2], []string{"lease clear=[] repin=[rs_mapn]", "lease! clear=[] repin=[]"}) || leases[len(leases)-1] != "lease clear=[] repin=[]" {
			t.Fatalf("lease writes = %v, want the unpin's, its failed release, and a release at the run's end", leases)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
		assertLoggedOnce(t, logs, "WARN", logLeaseAfterUnpin, map[string]string{"server_id": "srv1", "err": "store: database is locked"})
	})
	t.Run("after the immediate re-pin of a batch that failed", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		t1 := stopAllSpecOf("map1", "running", 643)
		t1.pinned = true
		n := stopAllSpecOf("mapn", "running", 644)
		n.pinned = true
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, n}})
		f.portal.failUnpin["rs_mapn"] = errors.New("store: disk I/O error")
		failSecondLease(f)

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		want := []string{"lease clear=[] repin=[rs_map1 rs_mapn]", "lease! clear=[] repin=[]", "lease clear=[] repin=[]"}
		if got := f.events.with("lease"); !slices.Equal(got, want) {
			t.Fatalf("lease writes = %v, want %v", got, want)
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
		assertLoggedOnce(t, logs, "WARN", logLeaseAfterUnpin, map[string]string{"server_id": "srv1", "err": "store: database is locked"})
	})
}

// TestRepinAtTheRunsEndLeavesAnAlreadyPinnedSpecAlone: a spec whose immediate
// re-pin stored pinned = true and then failed on its GPU rows stays owed, and
// the re-pin at the run's end finds it pinned already: a conflict, which it
// logs and leaves alone, not a failure.
func TestRepinAtTheRunsEndLeavesAnAlreadyPinnedSpecAlone(t *testing.T) {
	logs := withCapturedSlogAtTheDefaultLevel(t)
	t1 := stopAllSpecOf("map1", "running", 651)
	t1.pinned = true
	n := stopAllSpecOf("mapn", "running", 652)
	n.pinned = true
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, n}})
	f.portal.failUnpin["rs_map1"] = errors.New("store: disk I/O error")
	repins := 0
	f.portal.onPinBatch = func(kind string, _ []string) {
		if kind != "repin" {
			return
		}
		if repins++; repins == 1 {
			f.portalRoutes.mu.Lock()
			f.portalRoutes.failGPUsOnce["rs_mapn"] = true
			f.portalRoutes.mu.Unlock()
		}
	}

	status := f.run(t, "speed", f.targets(t, "speed", "map1"))

	want := []string{"unpin [rs_map1 rs_mapn]", "repin [rs_map1 rs_mapn]", "repin [rs_mapn]"}
	if got := f.events.with("unpin ", "repin "); !slices.Equal(got, want) {
		t.Fatalf("pinned batches = %v, want %v", got, want)
	}
	if !slices.Equal(status.UnpinnedSpecIDs, []string{"rs_mapn"}) || len(status.RepinFailed) != 0 || status.Error != "" {
		t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want [rs_mapn] / [] / \"\"", status.UnpinnedSpecIDs, status.RepinFailed, status.Error)
	}
	if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn"] {
		t.Fatalf("stored pinned after the run = %v, want both pinned", got)
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
	assertLoggedOnce(t, logs, "INFO", "benchmark: a launch spec was already pinned again when the run ended", map[string]string{"server_id": "srv1", "spec_id": "rs_mapn"})
}

// TestPreStopJudgesTheFormerlyPinnedNeighbours: a stop-all that has nothing
// to drain sees no sign that the agent holds the lifted pins, so a spec the
// run unpinned can still be pinned on the agent and start a child by itself
// inside the cold pass. Such a stop-all confirms a cold start only when every
// formerly pinned neighbour reads stopped with pid 0. The neighbour n in
// backoff keeps its 10 s timer for the whole test.
func TestPreStopJudgesTheFormerlyPinnedNeighbours(t *testing.T) {
	backoffPinned := func() stopAllSpec {
		n := stopAllSpecOf("mapn", "backoff", 0)
		n.pinned, n.backoff = true, 10*time.Second
		return n
	}
	t.Run("an empty stop set next to a formerly pinned neighbour in backoff measures no load time", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "stopped", 0), backoffPinned()}})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); len(got) != 0 {
			t.Fatalf("batches = %v, want none: nothing had to be stopped", got)
		}
		if got := f.events.with("unpin ", "repin "); !slices.Equal(got, []string{"unpin [rs_mapn]", "repin [rs_mapn]"}) {
			t.Fatalf("pinned batches = %v, want n's unpin and its re-pin", got)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
			t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
		}
		if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.LoadTimeMS != 1234 {
			t.Fatalf("stored load_time_ms = %d (%v), want 1234 kept", m.LoadTimeMS, err)
		}
		assertFormerlyPinnedLogged(t, logs, "map1", []string{"rs_mapn"})
	})
	t.Run("a stop set without a process next to a formerly pinned neighbour in backoff measures no load time", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		t1 := stopAllSpecOf("map1", "backoff", 0)
		t1.backoff = 3 * time.Second
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, backoffPinned()}, clearDelay: 100 * time.Millisecond})

		f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1]", "clear [rs_map1]"}) {
			t.Fatalf("batches = %v, want the target alone stopped and cleared", got)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
			t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
		}
		assertFormerlyPinnedLogged(t, logs, "map1", []string{"rs_mapn"})
	})
	// A formerly pinned neighbour that reads stopped with pid 0 has nothing to
	// restart by itself, and a neighbour the run never unpinned is not judged:
	// y sits in backoff and the start is still confirmed. The target, itself
	// formerly pinned and in backoff in the second case, is not judged either.
	for _, tc := range []struct {
		name   string
		target func() stopAllSpec
	}{
		{"an empty stop set", func() stopAllSpec { return stopAllSpecOf("map1", "stopped", 0) }},
		{"a stop set without a process", func() stopAllSpec {
			t1 := stopAllSpecOf("map1", "backoff", 0)
			t1.pinned, t1.backoff = true, 3*time.Second
			return t1
		}},
	} {
		t.Run("a formerly pinned neighbour that reads stopped still confirms: "+tc.name, func(t *testing.T) {
			logs := withCapturedSlogAtTheDefaultLevel(t)
			t1 := tc.target()
			n := stopAllSpecOf("mapn", "stopped", 0)
			n.pinned = true
			y := stopAllSpecOf("mapy", "backoff", 0)
			y.backoff = 10 * time.Second
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, n, y}, clearDelay: 100 * time.Millisecond})

			f.run(t, "speed", f.targets(t, "speed", "map1"))

			if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
				t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load within 20 %%", row.LoadTimeMS, row.Error)
			}
			for _, r := range logs.Snapshot() {
				if r.Attrs["reason"] == benchmarkStopReasonFormerlyPinned {
					t.Fatalf("logged %q with reason %s, want the start confirmed", r.Msg, benchmarkStopReasonFormerlyPinned)
				}
			}
		})
	}
}

// TestBenchmarkFormerPinsNotStopped: the formerly pinned neighbours that read
// other than stopped with pid 0, judged on one frame: only the specs the run
// unpinned, never the target, sorted.
func TestBenchmarkFormerPinsNotStopped(t *testing.T) {
	row := func(id, state string, pid int) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: id, State: state, PID: pid}
	}
	for _, tc := range []struct {
		name     string
		rows     []RuntimeStatusDTO
		unpinned []string
		want     []string
	}{
		{"a neighbour that reads stopped with pid 0", []RuntimeStatusDTO{row("rs_t", "stopped", 0), row("rs_n", "stopped", 0)}, []string{"rs_n"}, nil},
		{"a neighbour that reads stopped with a pid", []RuntimeStatusDTO{row("rs_t", "stopped", 0), row("rs_n", "stopped", 4711)}, []string{"rs_n"}, []string{"rs_n"}},
		{"a neighbour in backoff", []RuntimeStatusDTO{row("rs_t", "stopped", 0), row("rs_n", "backoff", 0)}, []string{"rs_n"}, []string{"rs_n"}},
		{"the target is not judged", []RuntimeStatusDTO{row("rs_t", "backoff", 0), row("rs_n", "stopped", 0)}, []string{"rs_t", "rs_n"}, nil},
		{"a spec the run did not unpin is not judged", []RuntimeStatusDTO{row("rs_t", "stopped", 0), row("rs_y", "backoff", 0)}, []string{"rs_n"}, nil},
		{"a formerly pinned spec without a row", []RuntimeStatusDTO{row("rs_t", "stopped", 0)}, []string{"rs_n"}, nil},
		{"several, sorted", []RuntimeStatusDTO{row("rs_t", "stopped", 0), row("rs_b", "start_failed", 0), row("rs_a", "crashed", 0)}, []string{"rs_a", "rs_b"}, []string{"rs_a", "rs_b"}},
	} {
		if got := benchmarkFormerPinsNotStopped(tc.rows, tc.unpinned, "rs_t"); !slices.Equal(got, tc.want) {
			t.Errorf("%s: benchmarkFormerPinsNotStopped = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestPreStopWaitsForATargetInBackoffToReadStopped: a target whose own row
// reads backoff, start_failed or crashed is quiet, but a request for it can
// still wait behind its backoff timer until the agent resets the row. The
// quiet wait therefore also waits for the target's row to read stopped. The
// fake applies the stop 800 ms late, and the target's 400 ms backoff timer
// would end before that: a cold pass started at once would wait for the timer
// and count it as load time.
func TestPreStopWaitsForATargetInBackoffToReadStopped(t *testing.T) {
	const stopDelay = 800 * time.Millisecond
	for _, state := range []string{"backoff", "start_failed", "crashed"} {
		t.Run("a reset that lands late is waited for, and only the load is measured: "+state, func(t *testing.T) {
			withCapturedSlogAtTheDefaultLevel(t)
			t1 := stopAllSpecOf("map1", state, 0)
			t1.backoff = 400 * time.Millisecond
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1}, stopDelay: stopDelay, clearDelay: 100 * time.Millisecond})

			f.run(t, "speed", f.targets(t, "speed", "map1"))

			if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1]", "clear [rs_map1]"}) {
				t.Fatalf("batches = %v, want the target stopped and cleared", got)
			}
			if row := f.speedRow(t, "map1"); row.Error != "" || !within(row.LoadTimeMS, t1.load, 20) {
				t.Fatalf("t1 history = load %d ms, error %q; want its own 300 ms load within 20 %%, without the backoff wait", row.LoadTimeMS, row.Error)
			}
			evs := f.events.list()
			stop, clear := f.events.index("stop [rs_map1]", 0), f.events.index("clear [rs_map1]", 0)
			if waited := evs[clear].at.Sub(evs[stop].at); waited < stopDelay/2 {
				t.Fatalf("stop to clear took %v, want the wait to last until the late reset (%v)", waited, stopDelay)
			}
		})
	}
	t.Run("a target that never reads stopped within the bound ends as a stop timeout", func(t *testing.T) {
		logs := withCapturedSlogAtTheDefaultLevel(t)
		t1 := stopAllSpecOf("map1", "backoff", 0)
		t1.backoff = 10 * time.Second
		f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, stopAllSpecOf("map2", "stopped", 0)}, stopDelay: 1500 * time.Millisecond, clearDelay: 100 * time.Millisecond})
		benchmarkStopWaitBound = 500 * time.Millisecond

		f.run(t, "speed", f.targets(t, "speed", "map1", "map2"))

		if got := f.batches(); !slices.Equal(got, []string{"stop [rs_map1]", "clear [rs_map1]"}) {
			t.Fatalf("batches = %v, want t1's stop and clear only: the expired wait turns stops off, so t2 next to the running t1 is not stopped", got)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 || row.Error != "" {
			t.Fatalf("t1 history = load %d ms, error %q; want no load time and no error", row.LoadTimeMS, row.Error)
		}
		assertLoggedReason(t, logs, logNotConfirmed, "map1", benchmarkStopReasonStopTimeout)
	})
}

// TestCancelledRunStillPinsAgain: a run cancelled after its unpin batch still
// pins the specs again, each re-pin on a context that is not cancelled with
// the run. The portal double fails every call on a done context, as a real
// store does. The run is cancelled while its unpin batch is written, so its
// context is done before the first target and at every re-pin.
func TestCancelledRunStillPinsAgain(t *testing.T) {
	pinnedPair := func() []stopAllSpec {
		tgt := stopAllSpecOf("map1", "running", 731)
		tgt.pinned = true
		n := stopAllSpecOf("mapn", "running", 732)
		n.pinned = true
		return []stopAllSpec{tgt, n}
	}
	cancelAtUnpin := func(f *stopAllFixture) {
		f.portal.onPinBatch = func(kind string, _ []string) {
			if kind == "unpin" {
				f.cancelRun()
			}
		}
	}

	t.Run("the re-pin at the run's end", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: pinnedPair()})
		cancelAtUnpin(f)

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin", "repin"); !slices.Equal(got, []string{"unpin [rs_map1 rs_mapn]", "repin [rs_map1 rs_mapn]"}) {
			t.Fatalf("pinned batches = %v, want the unpin and a re-pin that reaches the store after the cancel", got)
		}
		if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn"] {
			t.Fatalf("stored pinned after the cancelled run = %v, want both pinned again", got)
		}
		if got := f.events.with("lease"); !slices.Equal(got, []string{"lease clear=[] repin=[rs_map1 rs_mapn]", "lease clear=[] repin=[]"}) {
			t.Fatalf("lease writes = %v, want the unpin's and the release at the run's end", got)
		}
		if len(status.RepinFailed) != 0 || status.Error != "canceled" {
			t.Fatalf("repin_failed / error = %v / %q, want [] / \"canceled\"", status.RepinFailed, status.Error)
		}
	})

	t.Run("the immediate re-pin of a failed unpin", func(t *testing.T) {
		withCapturedSlogAtTheDefaultLevel(t)
		f := newStopAllFixture(t, stopAllOpts{specs: pinnedPair()})
		f.portal.failUnpin["rs_mapn"] = errors.New("store: disk I/O error")
		cancelAtUnpin(f)

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if got := f.events.with("unpin", "repin"); !slices.Equal(got, []string{"unpin [rs_map1 rs_mapn]", "repin [rs_map1 rs_mapn]"}) {
			t.Fatalf("pinned batches = %v, want the unpin and an immediate re-pin that reaches the store after the cancel", got)
		}
		if got := f.pinnedSpecs(t); !got["rs_map1"] || !got["rs_mapn"] {
			t.Fatalf("stored pinned after the cancelled run = %v, want both pinned", got)
		}
		want := []string{"lease clear=[] repin=[rs_map1 rs_mapn]", "lease clear=[] repin=[]", "lease clear=[] repin=[]"}
		if got := f.events.with("lease"); !slices.Equal(got, want) {
			t.Fatalf("lease writes = %v, want %v: the restore's rewrite and the end-of-run rewrite both reach the store", got, want)
		}
		if len(status.UnpinnedSpecIDs) != 0 || len(status.RepinFailed) != 0 || status.Error != "canceled" {
			t.Fatalf("unpinned_spec_ids / repin_failed / error = %v / %v / %q, want [] / [] / \"canceled\"", status.UnpinnedSpecIDs, status.RepinFailed, status.Error)
		}
	})
}

// TestBenchmarkJoinRunError: a failed re-pin's text joins the run's error after
// "; ", and is the whole error of a run that had none.
func TestBenchmarkJoinRunError(t *testing.T) {
	for _, tc := range []struct{ runErr, msg, want string }{
		{"", "could not pin", "could not pin"},
		{"canceled", "could not pin", "canceled; could not pin"},
	} {
		if got := benchmarkJoinRunError(tc.runErr, tc.msg); got != tc.want {
			t.Errorf("benchmarkJoinRunError(%q, %q) = %q, want %q", tc.runErr, tc.msg, got, tc.want)
		}
	}
}

// TestBenchmarkStopApplicationPicksTheAgentTarget: a manual speed or both run
// marks every target mayPreStop, so the application's type alone keeps a
// non-agent target that comes first from being taken as the one whose launch
// specs the run unpins and stops; a run without a mayPreStop agent target has
// none.
func TestBenchmarkStopApplicationPicksTheAgentTarget(t *testing.T) {
	targets := []benchmarkTarget{
		{app: routing.Application{ID: "a-ollama", Type: routing.ProviderOllama}},
		{app: routing.Application{ID: "b-agent", Type: routing.ProviderServerAgent}},
	}
	if app, ok := benchmarkStopApplication(targets); ok {
		t.Fatalf("before benchmarkMayPreStop: got %q %v, want none", app.ID, ok)
	}
	benchmarkMayPreStop(targets, "speed")
	if app, ok := benchmarkStopApplication(targets); !ok || app.ID != "b-agent" {
		t.Fatalf("got %q %v, want b-agent", app.ID, ok)
	}
	if app, ok := benchmarkStopApplication(targets[:1]); ok {
		t.Fatalf("a mayPreStop non-agent target alone: got %q %v, want none", app.ID, ok)
	}
}

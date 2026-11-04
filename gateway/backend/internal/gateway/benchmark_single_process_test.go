// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A run that measures a model alone -- a manual speed or both run with its stop-all, and a
// VRAM run -- runs a replicable target as one process for as long as it holds its override
// lease: it writes replicable = false, recorded in the lease before the write, and writes it
// back when it ends. A dead run's write is undone at the next gateway start.

// SetBenchmarkRuntimeSpecsReplicable records every replicable batch of a stop-all fixture's
// run as an event, and fails the specs in failReplicableRestore when the batch restores.
func (p *stopAllPortal) SetBenchmarkRuntimeSpecsReplicable(ctx context.Context, specIDs []string, expected, replicable bool) (portal.BenchmarkSpecsOutcome, error) {
	ids := append([]string(nil), specIDs...)
	slices.Sort(ids)
	kind := "single"
	if replicable {
		kind = "replicable"
	}
	p.events.add("%s %v", kind, ids)
	p.mu.Lock()
	hook, fail := p.onReplicableBatch, p.failReplicableRestore
	if !replicable {
		p.singleBatches++
	}
	failHold := !replicable && p.failSingleFrom > 0 && p.singleBatches >= p.failSingleFrom
	p.mu.Unlock()
	if hook != nil {
		hook(kind, ids)
	}
	if failHold {
		err := errors.New("database is locked")
		out := portal.BenchmarkSpecsOutcome{Failed: ids, Errs: map[string]error{}}
		for _, id := range ids {
			out.Errs[id] = err
		}
		return out, err
	}
	if replicable && len(fail) > 0 {
		out := portal.BenchmarkSpecsOutcome{Failed: ids, Errs: map[string]error{}}
		for _, id := range ids {
			out.Errs[id] = fail[id]
		}
		return out, fail[ids[0]]
	}
	return p.API.SetBenchmarkRuntimeSpecsReplicable(ctx, specIDs, expected, replicable)
}

// makeReplicable stores spec specID as replicable with two protected
// instances and room for four.
func makeReplicable(t *testing.T, st routing.Store, specID string) {
	t.Helper()
	ctx := context.Background()
	spec, ok, err := st.RuntimeSpecByID(ctx, specID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(%s) = %v, %v", specID, ok, err)
	}
	spec.Replicable, spec.MinInstances, spec.MaxInstances, spec.ListenPort = true, 2, 4, 0
	if err := st.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("UpsertRuntimeSpec(%s): %v", specID, err)
	}
}

// storedReplicable is spec specID's stored replicable flag.
func storedReplicable(t *testing.T, st routing.Store, specID string) bool {
	t.Helper()
	spec, ok, err := st.RuntimeSpecByID(context.Background(), specID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(%s) = %v, %v", specID, ok, err)
	}
	return spec.Replicable
}

// TestManualSpeedRunMeasuresAReplicableTargetAsOneProcess: before its first stop the run
// records every enabled replicable spec of the server in the lease -- the target's and a
// neighbour's that the run does not measure alike -- and runs them as one process each, and
// after the run they are replicable again and the lease is released. A spec that is not
// replicable, measured or not, is left alone.
func TestManualSpeedRunMeasuresAReplicableTargetAsOneProcess(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		held         []string
	}{
		{"a replicable target beside a replicable neighbour", "map1", []string{"rs_map1", "rs_mapn"}},
		{"a target that is not replicable beside two replicable neighbours", "mapx", []string{"rs_map1", "rs_mapn"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map1", "running", 701), stopAllSpecOf("mapn", "stopped", 0), stopAllSpecOf("mapx", "running", 702)}})
			makeReplicable(t, f.mem, "rs_map1")
			makeReplicable(t, f.mem, "rs_mapn")
			var leaseAtFlip portal.BenchmarkOverrideLease
			var flatAtStop []string
			f.portal.onReplicableBatch = func(kind string, _ []string) {
				if kind == "single" {
					leaseAtFlip = f.lease(t)
				}
			}
			f.portal.onBatch = func(kind string, _ []string) {
				if kind == "stop" && flatAtStop == nil {
					flatAtStop = []string{}
					for _, id := range []string{"rs_map1", "rs_mapn"} {
						if !storedReplicable(t, f.mem, id) {
							flatAtStop = append(flatAtStop, id)
						}
					}
				}
			}

			status := f.run(t, "speed", f.targets(t, "speed", tc.target))

			if status.Error != "" {
				t.Fatalf("run error = %q", status.Error)
			}
			want := []string{fmt.Sprintf("single %v", tc.held), fmt.Sprintf("replicable %v", tc.held)}
			if got := f.events.with("single ", "replicable "); !slices.Equal(got, want) {
				t.Fatalf("replicable batches = %v, want %v: every replicable spec once off and once back on", got, want)
			}
			stops := f.events.with("stop ")
			if len(stops) == 0 {
				t.Fatal("the run stopped nothing")
			}
			if single, stop := f.events.index(want[0], 0), f.events.index(stops[0], 0); single < 0 || single > stop {
				t.Fatalf("events = %v, want the replicable specs run as one process before the stop", f.events.with("single ", "stop "))
			}
			if !slices.Equal(leaseAtFlip.RestoreReplicable, tc.held) {
				t.Fatalf("lease at the write = %+v, want restore_replicable %v recorded before it", leaseAtFlip, tc.held)
			}
			if !slices.Equal(flatAtStop, tc.held) {
				t.Fatalf("specs stored as one process at the stop = %v, want %v", flatAtStop, tc.held)
			}
			if !storedReplicable(t, f.mem, "rs_map1") || !storedReplicable(t, f.mem, "rs_mapn") || storedReplicable(t, f.mem, "rs_mapx") {
				t.Fatal("after the run a replicable spec is stored as not replicable, or the other one as replicable")
			}
			if got := f.lease(t); !got.Empty() {
				t.Fatalf("lease after the run = %+v, want it released", got)
			}
		})
	}
}

// TestStopAllRunHoldsAReplicableNeighbourAsOneProcess: a replicable neighbour that keeps
// protected instances (min_instances >= 1) starts them by itself whenever it has no process
// and no override -- at the stop-all's clear, inside the target's cold pass, where their load
// would land in the target's figures. So a manual speed run holds it as one process for the
// whole run: it never starts while the run measures, the target's load time is confirmed, and
// the neighbour is replicable again when the run ends, and starts then. When the run cannot
// record that hold in its lease -- here for a replicable target and the neighbour -- it writes
// nothing and stops nothing, where it used to stop the server's models with every replicable
// spec still replicable: the target is measured as it finds the server, next to the running
// neighbour, and gets no load time.
func TestStopAllRunHoldsAReplicableNeighbourAsOneProcess(t *testing.T) {
	setup := func(t *testing.T) *stopAllFixture {
		t.Helper()
		f := newStopAllFixture(t, stopAllOpts{protectedFloor: true, specs: []stopAllSpec{stopAllSpecOf("map1", "running", 751), stopAllSpecOf("mapn", "running", 752)}})
		makeReplicable(t, f.mem, "rs_mapn")
		f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
		return f
	}
	t.Run("held for the run", func(t *testing.T) {
		f := setup(t)

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if status.Error != "" || len(status.Results) != 1 || status.Results[0].Error != "" {
			t.Fatalf("status = %+v, want one result and no error", status)
		}
		restore := f.events.index("replicable [rs_mapn]", 0)
		if hold, stop := f.events.index("single [rs_mapn]", 0), f.events.index("stop [rs_map1 rs_mapn]", 0); hold < 0 || stop < 0 || hold > stop || restore < stop {
			t.Fatalf("events = %v, want the neighbour held before the stop and restored after it", f.events.with("single ", "replicable ", "stop ", "clear "))
		}
		if started := f.events.index("protected start rs_mapn", 0); started >= 0 && started < restore {
			t.Fatalf("events = %v, want the neighbour's protected instances started only once the run made it replicable again", f.events.with("single ", "replicable ", "stop ", "clear ", "protected "))
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS <= 0 {
			t.Fatalf("map1 speed row = %+v, want a confirmed load time", row)
		}
		if !storedReplicable(t, f.mem, "rs_mapn") {
			t.Fatal("after the run the neighbour is stored as not replicable")
		}
		if got := f.lease(t); !got.Empty() {
			t.Fatalf("lease after the run = %+v, want it released", got)
		}
	})
	t.Run("the lease write fails", func(t *testing.T) {
		f := setup(t)
		makeReplicable(t, f.mem, "rs_map1")
		f.portal.failLease = func(n int) error {
			if n == 1 {
				return errors.New("database is locked")
			}
			return nil
		}

		status := f.run(t, "speed", f.targets(t, "speed", "map1"))

		if status.Error != "" || len(status.Results) != 1 || status.Results[0].Error != "" {
			t.Fatalf("status = %+v, want one result and no error", status)
		}
		if got := f.events.with("single ", "replicable ", "stop ", "clear "); len(got) != 0 {
			t.Fatalf("batches = %v, want none: without the lease nothing is held and nothing stopped", got)
		}
		if row := f.speedRow(t, "map1"); row.LoadTimeMS != 0 {
			t.Fatalf("map1 speed row = %+v, want no load time next to the running neighbour", row)
		}
		if !storedReplicable(t, f.mem, "rs_map1") || !storedReplicable(t, f.mem, "rs_mapn") {
			t.Fatal("a replicable spec was stored as not replicable")
		}
	})
}

// TestBenchmarkRunsWithoutAStopKeepTheirInstances: a capacity run (the audio ramp
// included) and a scheduled run stop nothing, so they never take a replicable
// target's instances away.
func TestBenchmarkRunsWithoutAStopKeepTheirInstances(t *testing.T) {
	t1 := stopAllSpecOf("map1", "running", 711)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1}})
	makeReplicable(t, f.mem, "rs_map1")

	f.run(t, "capacity", f.targets(t, "capacity", "map1"))

	if got := f.events.with("single ", "replicable "); len(got) != 0 {
		t.Fatalf("replicable batches = %v, want none", got)
	}
	if !storedReplicable(t, f.mem, "rs_map1") {
		t.Fatal("the capacity run stored the target as not replicable")
	}
}

// TestFailedReplicableRestoreStaysInTheLease: a restore that fails at the run's end keeps
// the spec in the lease's restore_replicable for the next reconcile, and the run's error
// names it.
func TestFailedReplicableRestoreStaysInTheLease(t *testing.T) {
	t1 := stopAllSpecOf("map1", "running", 721)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1}})
	makeReplicable(t, f.mem, "rs_map1")
	f.portal.failReplicableRestore = map[string]error{"rs_map1": errors.New("database is locked")}

	status := f.run(t, "speed", f.targets(t, "speed", "map1"))

	if !strings.Contains(status.Error, "rs_map1") {
		t.Fatalf("run error = %q, want it to name rs_map1", status.Error)
	}
	if got := f.lease(t); !slices.Equal(got.RestoreReplicable, []string{"rs_map1"}) {
		t.Fatalf("lease after the run = %+v, want restore_replicable [rs_map1]", got)
	}
	if storedReplicable(t, f.mem, "rs_map1") {
		t.Fatal("the failed restore stored the target as replicable")
	}
}

// TestReconcileRestoresReplicableAfterADeadRun: the reconcile at gateway start writes
// replicable back for every spec a dead run left as one process, by compare-and-set
// against false, and drops a spec an operator turned back on and one that is gone.
func TestReconcileRestoresReplicableAfterADeadRun(t *testing.T) {
	f := newLeaseFixture(t, routing.NewMemoryStore(), portal.NewMemorySystemSettings())
	makeReplicable(t, f.routes, "rs_pin") // turned back on by an operator: a conflict
	lease := portal.BenchmarkOverrideLease{RestoreReplicable: []string{"rs_gone", "rs_idle", "rs_pin"}}
	f.seedLease(t, "srv1", lease)

	left := f.srv.reconcileBenchmarkOverrideLease(context.Background(), "srv1", lease)

	if !left.Empty() {
		t.Fatalf("left = %#v, want nothing", left)
	}
	if !storedReplicable(t, f.routes, "rs_idle") {
		t.Fatal("rs_idle is still stored as not replicable after the reconcile")
	}
	if got := f.leaseRow(t, "srv1"); got != "" {
		t.Fatalf("lease row = %q, want it released", got)
	}
}

// TestVRAMRunMeasuresAReplicableTargetAsOneProcess: a VRAM run records its replicable
// target in the lease with its drain, runs it as one process while it loads it alone, and
// makes it replicable again on its way out.
func TestVRAMRunMeasuresAReplicableTargetAsOneProcess(t *testing.T) {
	settings := newVRAMLeaseSettings()
	f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
	makeReplicable(t, f.mem, f.targetSpec)
	spec, _, err := f.mem.RuntimeSpecByID(context.Background(), f.targetSpec)
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	f.target.spec = spec
	f.seedLatestSample()
	f.drive(t)
	spy := newVRAMLeaseSpy(f, settings.row)
	var flatAtLoad bool
	f.provider.onStream = func() {
		flatAtLoad = !storedReplicable(t, f.mem, f.targetSpec)
		f.used0.Store(21500 * oneMiB)
	}

	res := f.run(t).Results[0]

	if res.Error != "" {
		t.Fatalf("res.Error = %q", res.Error)
	}
	if spy.atDrain != `{"clear_force_stopped":["rspec_sib","rspec_target"],"restore_replicable":["rspec_target"]}` {
		t.Fatalf("lease row at the drain = %q, want the target's replicable restore recorded with the drain", spy.atDrain)
	}
	if !flatAtLoad {
		t.Fatal("the target was still replicable when the run loaded it")
	}
	if !storedReplicable(t, f.mem, f.targetSpec) {
		t.Fatal("the target is stored as not replicable after the run")
	}
	if got := settings.row(); got != "" {
		t.Fatalf("lease row after the run = %q, want released", got)
	}
}

// replicableAgentEnsurer is the stop-all fixture's provider plus the agent router's ensure
// route, as an agent that declares runtime_instances answers it: an instance address m#k
// (k >= 1) of a spec whose last applied document holds it as not replicable is refused 404
// runtime.model_not_managed, the router's answer for an instance its spec does not allow;
// an address in refuse, once the agent allows it, is refused with that router code; every
// other ensure succeeds. The agent's flag per spec id follows every document it applies
// (watch), and each ensure is an event.
type replicableAgentEnsurer struct {
	provider.Client
	events *stopAllEvents
	specOf map[string]string // base model -> spec id

	mu         sync.Mutex
	replicable map[string]bool          // spec id -> the flag the agent applied
	refuse     map[string]string        // address -> router code
	delay      map[string]time.Duration // address -> how long its start takes before the ensure answers
}

func (e *replicableAgentEnsurer) LoadedModels(ctx context.Context, target routing.Target, path, format string) ([]string, error) {
	return e.Client.(provider.LoadedModelLister).LoadedModels(ctx, target, path, format)
}

func (e *replicableAgentEnsurer) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	e.events.add("ensure %s", target.ProviderModel)
	base, _, instance := strings.Cut(target.ProviderModel, "#")
	e.mu.Lock()
	allowed := e.replicable[e.specOf[base]]
	code, refused := e.refuse[target.ProviderModel]
	delay := e.delay[target.ProviderModel]
	e.mu.Unlock()
	if instance && !allowed {
		return &provider.RouterError{Code: "runtime.model_not_managed", Err: provider.ErrUnavailable}
	}
	if refused {
		return &provider.RouterError{Code: code, Err: provider.ErrUnavailable}
	}
	time.Sleep(delay)
	e.events.add("ensured %s", target.ProviderModel)
	return nil
}

// watch records the replicable flag of every spec in an applied document.
func (e *replicableAgentEnsurer) watch(doc portal.AgentRuntimeConfigDTO) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, d := range doc.Specs {
		e.replicable[d.ID] = d.Replicable
	}
}

// newBothRunAudioFixture is a stop-all fixture whose first spec, rs_map1 (model m1), is
// replicable on an agent that declares runtime_instances and serves a one-shot TTS mapping,
// followed by the specs in opts.specs, with the agent's ensure route
// (replicableAgentEnsurer) and a scripted measurement that is always five times real time.
// Every measurement is an event "audio <model> level <k> warm-up <bool>" (the speed passes
// are level 0, the ramp's warm-ups level 0 and warm-up true). The targets it returns are
// map1's alone.
func newBothRunAudioFixture(t *testing.T, opts stopAllOpts) (*stopAllFixture, *audioScript, []benchmarkTarget) {
	t.Helper()
	opts.specs = append([]stopAllSpec{stopAllSpecOf("map1", "running", 731)}, opts.specs...)
	f := newStopAllFixture(t, opts)
	makeReplicable(t, f.mem, "rs_map1") // two protected instances, room for four
	f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
	ensurer := &replicableAgentEnsurer{Client: f.srv.Provider, events: f.events, specOf: map[string]string{"m1": "rs_map1"}, replicable: map[string]bool{"rs_map1": true}, refuse: map[string]string{}, delay: map[string]time.Duration{}}
	f.srv.Provider = ensurer
	f.agent.onApply = ensurer.watch
	script := &audioScript{answer: func(audioMeasureCall, int) (audioMeasurement, error) { return audioFigures(5, 100), nil }}
	f.srv.audioMeasureSeam = func(ctx context.Context, call audioMeasureCall) (audioMeasurement, error) {
		f.events.add("audio %s level %d warm-up %v", call.target.ProviderModel, call.level, call.warmUp)
		return script.seam(ctx, call)
	}
	return f, script, bothRunAudioTargets(t, f, "map1")
}

// bothRunAudioTargets is a both run's targets for mappingIDs, each a one-shot TTS target.
func bothRunAudioTargets(t *testing.T, f *stopAllFixture, mappingIDs ...string) []benchmarkTarget {
	t.Helper()
	targets := f.targets(t, "both", mappingIDs...)
	for i := range targets {
		targets[i].audio = audioTarget{role: routing.RoleTTS}
	}
	return targets
}

// TestBothRunRampWaitsForTheFloorItsRestoreStarts: making the spec replicable again before
// its ramp also starts its protected instances (min_instances 3: m1#1 and m1#2) by
// themselves. The ramp ensures them -- each ensure returns once the instance runs, after any
// wait its start takes (m1#2 here takes 80 ms) -- before it sends anything, so no level is
// measured while a sibling of the measured process still loads, and it does not ensure them
// again at their own levels.
func TestBothRunRampWaitsForTheFloorItsRestoreStarts(t *testing.T) {
	f, _, _ := newBothRunAudioFixture(t, stopAllOpts{stopDelay: 100 * time.Millisecond})
	spec, _, err := f.mem.RuntimeSpecByID(context.Background(), "rs_map1")
	if err != nil {
		t.Fatalf("RuntimeSpecByID: %v", err)
	}
	spec.MinInstances = 3
	if err := f.mem.UpsertRuntimeSpec(context.Background(), spec); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	f.srv.Provider.(*replicableAgentEnsurer).delay["m1#2"] = 80 * time.Millisecond

	status := f.run(t, "both", bothRunAudioTargets(t, f, "map1"))

	if res := status.Results[0]; status.Error != "" || res.Error != "" || res.AudioRealtimeConcurrency != 4 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
		t.Fatalf("status = %+v, want 4 / cap and no error", status)
	}
	// An ensure the agent refuses with 404 until the restore reaches it is tried again, so an
	// address can repeat; the order of the first tries is the order the ramp asked in.
	if got := slices.Compact(f.events.with("ensure ")); !slices.Equal(got, []string{"ensure m1", "ensure m1#1", "ensure m1#2", "ensure m1#3"}) {
		t.Fatalf("ensures = %v, want the protected instances first, then m1#3", got)
	}
	firstRampCall := f.events.index("audio m1 level 0 warm-up true", 0)
	if floorUp := f.events.index("ensured m1#2", 0); floorUp < 0 || firstRampCall < 0 || floorUp > firstRampCall {
		t.Fatalf("events = %v, want the protected instances up before the ramp's first warm-up", f.events.with("replicable ", "ensure", "audio m1 "))
	}
	var later []string
	for _, ev := range f.events.list()[firstRampCall:] {
		if strings.HasPrefix(ev.what, "ensure ") {
			later = append(later, ev.what)
		}
	}
	if !slices.Equal(later, []string{"ensure m1#3"}) {
		t.Fatalf("ensures during the ramp = %v, want only m1#3: the protected instances are up already", later)
	}
}

// TestBothRunRampFailsWhenAProtectedInstanceDoesNotStart: the agent admits a protected
// instance like a baseline start -- it waits for room or evicts an idle neighbour, and never
// refuses it for room -- so an ensure of one that fails at all, whatever its code (here even
// runtime.instance_no_capacity, or a start that timed out), is a failure of the ramp, not a
// lower bound: no level is measured, no pair is stored, and the spec is replicable again and
// the lease released after the run.
func TestBothRunRampFailsWhenAProtectedInstanceDoesNotStart(t *testing.T) {
	for _, code := range []string{audioRampNoCapacityCode, "runtime.start_timeout"} {
		t.Run(code, func(t *testing.T) {
			f, _, targets := newBothRunAudioFixture(t, stopAllOpts{})
			f.srv.Provider.(*replicableAgentEnsurer).refuse["m1#1"] = code

			status := f.run(t, "both", targets)

			res := status.Results[0]
			if !strings.Contains(res.Error, "protected instances") || !strings.Contains(res.Error, code) || res.AudioConcurrencyLimit != "" {
				t.Fatalf("result = %d / %q, error %q; want the ramp failed on the protected instance", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error)
			}
			if f.events.index("audio m1 level 1 warm-up false", 0) >= 0 {
				t.Fatalf("events = %v, want no level measured", f.events.with("ensure ", "audio m1 "))
			}
			if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.AudioConcurrencyLimit != "" {
				t.Fatalf("mapping limit = %q (%v), want no pair stored", m.AudioConcurrencyLimit, err)
			}
			if !storedReplicable(t, f.mem, "rs_map1") {
				t.Fatal("after the run the spec is stored as not replicable")
			}
			if got := f.lease(t); !got.Empty() {
				t.Fatalf("lease after the run = %+v, want it released", got)
			}
		})
	}
}

// TestBothRunHoldsARampedSpecAsOneProcessForTheTargetsAfterIt: a manual both run over a
// replicable audio target A (map1) and a target B (map2) after it. A's ramp makes A
// replicable again, and its instances run (here: its row reads running again). Before B's
// stop-all the run holds A as one process again -- recorded in the lease before the write --
// or A's protected instances would start by themselves at B's clear, inside B's cold pass.
// B's load time is then confirmed, and at the end A is made replicable again and the lease
// is released. When holding A again fails, A may be replicable, so the run stops nothing
// more: B is measured as it finds the server, next to A's running process, and gets no load
// time.
func TestBothRunHoldsARampedSpecAsOneProcessForTheTargetsAfterIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failHold bool
	}{{"A is held again", false}, {"holding A again fails", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f, script, _ := newBothRunAudioFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map2", "stopped", 0)}})
			if tc.failHold {
				f.portal.failSingleFrom = 2 // the run's own first batch succeeds
			}
			var mu sync.Mutex
			coldPassesOfB := 0
			script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
				m := audioFigures(5, 100)
				if call.target.ProviderModel == "m2" && call.level == 0 && !call.warmUp {
					mu.Lock()
					coldPassesOfB++
					cold := coldPassesOfB == 1
					mu.Unlock()
					if cold {
						m.FirstByteMS = 2100
					} else {
						m.FirstByteMS = 100
					}
				}
				return m, nil
			}
			f.portal.onReplicableBatch = func(kind string, _ []string) {
				if kind == "replicable" {
					f.agent.setState("rs_map1", "running", 733) // the restore's instances run
				}
			}
			var heldAtStop, leaseNamesA bool
			stops := 0
			f.portal.onBatch = func(kind string, _ []string) {
				if kind != "stop" {
					return
				}
				if stops++; stops == 2 {
					heldAtStop = !storedReplicable(t, f.mem, "rs_map1")
					leaseNamesA = slices.Contains(f.lease(t).RestoreReplicable, "rs_map1")
				}
			}

			status := f.run(t, "both", bothRunAudioTargets(t, f, "map1", "map2"))

			if status.Error != "" || len(status.Results) != 2 {
				t.Fatalf("status = %+v, want two results and no run error", status)
			}
			a, b := status.Results[0], status.Results[1]
			if a.Error != "" || a.AudioRealtimeConcurrency != 4 || a.AudioConcurrencyLimit != routing.AudioLimitCap {
				t.Fatalf("A = %d / %q, error %q; want 4 / cap", a.AudioRealtimeConcurrency, a.AudioConcurrencyLimit, a.Error)
			}
			wantBatches := []string{"single [rs_map1]", "replicable [rs_map1]", "single [rs_map1]", "replicable [rs_map1]"}
			if got := f.events.with("single ", "replicable "); !slices.Equal(got, wantBatches) {
				t.Fatalf("replicable batches = %v, want %v: held, restored for the ramp, held again for B, restored at the end", got, wantBatches)
			}
			if hold, firstOfB := f.events.index("single [rs_map1]", 1), f.events.index("audio m2 level 0 warm-up false", 0); hold < f.events.lastIndex("ensure m1#3") || hold > firstOfB {
				t.Fatalf("events = %v, want A held again after its ramp and before B is measured", f.events.with("single ", "replicable ", "ensure ", "stop ", "audio m2 level 0"))
			}
			if tc.failHold {
				if stops != 1 || b.Error != "" || b.AudioLoadTimeMS != 0 {
					t.Fatalf("stop batches = %d, B = load %d ms, error %q; want only A's stop and no load time for B", stops, b.AudioLoadTimeMS, b.Error)
				}
			} else {
				if stops != 2 || !heldAtStop || !leaseNamesA {
					t.Fatalf("stop batches = %d; at B's stop A held as one process %v, named in the lease %v; want both", stops, heldAtStop, leaseNamesA)
				}
				if b.Error != "" || b.AudioLoadTimeMS != 2000 {
					t.Fatalf("B = load %d ms, error %q; want the confirmed 2000", b.AudioLoadTimeMS, b.Error)
				}
			}
			if !storedReplicable(t, f.mem, "rs_map1") {
				t.Fatal("after the run A is stored as not replicable")
			}
			if got := f.lease(t); !got.Empty() {
				t.Fatalf("lease after the run = %+v, want it released", got)
			}
		})
	}
}

// TestBothRunRampsAReplicableAudioTargetByInstance: a manual both run measures a replicable
// target's speed as one process, but its audio ramp is the one by instance, which the agent
// refuses while it holds the spec as not replicable. So the run makes the spec replicable
// again before the ramp -- once, and not again at its end -- and the ramp waits out the
// agent's 404 for an instance address until the document reaches it (here a slow delivery):
// the ramp reaches max_instances (cap) with one stream per instance, and the lease is
// released.
func TestBothRunRampsAReplicableAudioTargetByInstance(t *testing.T) {
	f, script, targets := newBothRunAudioFixture(t, stopAllOpts{stopDelay: 100 * time.Millisecond})

	status := f.run(t, "both", targets)

	if status.Error != "" || len(status.Results) != 1 {
		t.Fatalf("status = %+v, want one result and no run error", status)
	}
	res := status.Results[0]
	if res.Error != "" || res.AudioRealtimeConcurrency != 4 || res.AudioConcurrencyLimit != routing.AudioLimitCap {
		t.Fatalf("result = %d / %q, error %q; want 4 / cap; events %v", res.AudioRealtimeConcurrency, res.AudioConcurrencyLimit, res.Error, f.events.with("single ", "replicable ", "ensure "))
	}
	if got := f.events.with("single ", "replicable "); !slices.Equal(got, []string{"single [rs_map1]", "replicable [rs_map1]"}) {
		t.Fatalf("replicable batches = %v, want the target once off and once back on", got)
	}
	if restore, ensure := f.events.index("replicable [rs_map1]", 0), f.events.index("ensure m1#1", 0); restore < 0 || ensure < 0 || restore > ensure {
		t.Fatalf("events = %v, want the target replicable again before the ramp ensures m1#1", f.events.with("replicable ", "ensure "))
	}
	if stop := f.events.lastIndex("stop [rs_map1]"); stop < 0 || stop > f.events.index("replicable [rs_map1]", 0) {
		t.Fatalf("events = %v, want the speed part's stop before the restore", f.events.with("stop ", "replicable "))
	}
	byLevel := map[int][]string{}
	for _, c := range script.recorded() {
		if !c.warmUp && c.level > 0 {
			byLevel[c.level] = append(byLevel[c.level], c.model)
		}
	}
	for k, models := range byLevel {
		slices.Sort(models)
		if want := []string{"m1", "m1#1", "m1#2", "m1#3"}[:k]; !slices.Equal(models, want) {
			t.Fatalf("level %d streams went to %v, want one per instance %v", k, models, want)
		}
	}
	if !storedReplicable(t, f.mem, "rs_map1") {
		t.Fatal("after the run the target is stored as not replicable")
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
}

// TestBothRunRampFailsWhenItsSpecStaysOneProcess: when the restore before the ramp fails, the
// spec may still be held as one process, so the ramp fails at once -- no instance address is
// ensured and no pair is stored -- and the spec stays in the lease for the run's end, whose
// own restore fails as well and names it.
func TestBothRunRampFailsWhenItsSpecStaysOneProcess(t *testing.T) {
	f, _, targets := newBothRunAudioFixture(t, stopAllOpts{})
	f.portal.failReplicableRestore = map[string]error{"rs_map1": errors.New("database is locked")}

	status := f.run(t, "both", targets)

	if len(status.Results) != 1 {
		t.Fatalf("status = %+v, want one result", status)
	}
	res := status.Results[0]
	if !strings.Contains(res.Error, "rs_map1") || res.AudioConcurrencyLimit != "" || res.AudioRateX == 0 {
		t.Fatalf("result = %+v, want the speed figures and a ramp error naming rs_map1", res)
	}
	if got := f.events.with("ensure "); len(got) != 0 {
		t.Fatalf("ensures = %v, want none", got)
	}
	if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.AudioConcurrencyLimit != "" {
		t.Fatalf("mapping limit = %q (%v), want no pair stored", m.AudioConcurrencyLimit, err)
	}
	if got := f.lease(t); !slices.Equal(got.RestoreReplicable, []string{"rs_map1"}) {
		t.Fatalf("lease after the run = %+v, want restore_replicable [rs_map1]", got)
	}
	if !strings.Contains(status.Error, "rs_map1") {
		t.Fatalf("run error = %q, want it to name rs_map1", status.Error)
	}
}

// TestBothRunRampEndsWhenAnOperatorsFixedPortKeepsItsSpecOneProcess: while the run holds the
// target as one process, an operator gives it a fixed listen port, which a replicable spec
// cannot have. The restore before the ramp is then a conflict that leaves the operator's
// setting -- one process on that port -- and lets go of the spec, so the agent will never run
// an instance address of it. The ramp ends at once with an error that names the port: no
// instance address is ensured, no pair is stored, and the lease is released.
func TestBothRunRampEndsWhenAnOperatorsFixedPortKeepsItsSpecOneProcess(t *testing.T) {
	f, _, targets := newBothRunAudioFixture(t, stopAllOpts{})
	var once sync.Once
	f.portal.onReplicableBatch = func(kind string, _ []string) {
		if kind != "replicable" {
			return
		}
		once.Do(func() {
			spec, ok, err := f.mem.RuntimeSpecByID(context.Background(), "rs_map1")
			if err != nil || !ok {
				t.Errorf("RuntimeSpecByID = %v, %v", ok, err)
				return
			}
			spec.ListenPort = 9123
			if err := f.mem.UpsertRuntimeSpec(context.Background(), spec); err != nil {
				t.Errorf("UpsertRuntimeSpec: %v", err)
			}
		})
	}

	status := f.run(t, "both", targets)

	if len(status.Results) != 1 {
		t.Fatalf("status = %+v, want one result", status)
	}
	res := status.Results[0]
	if !strings.Contains(res.Error, "rs_map1") || !strings.Contains(res.Error, "9123") || res.AudioConcurrencyLimit != "" || res.AudioRateX == 0 {
		t.Fatalf("result = %+v, want the speed figures and a ramp error naming rs_map1 and its port 9123", res)
	}
	if got := f.events.with("ensure "); len(got) != 0 {
		t.Fatalf("ensures = %v, want none", got)
	}
	if m, err := f.mem.MappingByID(context.Background(), "map1"); err != nil || m.AudioConcurrencyLimit != "" {
		t.Fatalf("mapping limit = %q (%v), want no pair stored", m.AudioConcurrencyLimit, err)
	}
	if storedReplicable(t, f.mem, "rs_map1") {
		t.Fatal("after the run the spec is stored as replicable, want the operator's one process")
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
	if status.Error != "" {
		t.Fatalf("run error = %q, want none: the conflict owes no restore", status.Error)
	}
}

// TestBothRunRampGoesAheadWhenAnOperatorMadeItsSpecReplicableAgain: while the run holds
// target A (map1) as one process, an operator turns multiple instances back on, and the
// portal pushes that document. The restore before A's ramp is then a conflict -- the spec is
// no longer stored as the not replicable the run wrote -- but the spec read again is
// replicable (audioRampStillReplicable), so the ramp counts it as restored: it goes ahead by
// instance up to max_instances (4 / cap, one stream per instance), and afterwards holds A as
// one process again for the stop-all of B (map2) after it (audioRampOneProcessAgain),
// recorded in the lease before the write. B's load time is confirmed, and at the end A is
// replicable again, as the operator left it, and the lease is released.
func TestBothRunRampGoesAheadWhenAnOperatorMadeItsSpecReplicableAgain(t *testing.T) {
	f, script, _ := newBothRunAudioFixture(t, stopAllOpts{specs: []stopAllSpec{stopAllSpecOf("map2", "stopped", 0)}})
	var mu sync.Mutex
	coldPassesOfB := 0
	script.answer = func(call audioMeasureCall, _ int) (audioMeasurement, error) {
		m := audioFigures(5, 100)
		if call.target.ProviderModel == "m2" && call.level == 0 && !call.warmUp {
			mu.Lock()
			coldPassesOfB++
			cold := coldPassesOfB == 1
			mu.Unlock()
			if cold {
				m.FirstByteMS = 2100
			} else {
				m.FirstByteMS = 100
			}
		}
		return m, nil
	}
	var once sync.Once
	var replicableBeforeRestore bool
	f.portal.onReplicableBatch = func(kind string, _ []string) {
		if kind != "replicable" {
			return
		}
		once.Do(func() {
			ctx := context.Background()
			spec, ok, err := f.mem.RuntimeSpecByID(ctx, "rs_map1")
			if err != nil || !ok {
				t.Errorf("RuntimeSpecByID = %v, %v", ok, err)
				return
			}
			spec.Replicable = true
			if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
				t.Errorf("UpsertRuntimeSpec: %v", err)
				return
			}
			replicableBeforeRestore = storedReplicable(t, f.mem, "rs_map1")
			f.srv.PushRuntimeConfig("srv1")             // the operator's save pushes its document
			f.agent.setState("rs_map1", "running", 733) // and the spec's instances run
		})
	}
	var heldAtStop, leaseNamesA bool
	stops := 0
	f.portal.onBatch = func(kind string, _ []string) {
		if kind != "stop" {
			return
		}
		if stops++; stops == 2 {
			heldAtStop = !storedReplicable(t, f.mem, "rs_map1")
			leaseNamesA = slices.Contains(f.lease(t).RestoreReplicable, "rs_map1")
		}
	}

	status := f.run(t, "both", bothRunAudioTargets(t, f, "map1", "map2"))

	if status.Error != "" || len(status.Results) != 2 {
		t.Fatalf("status = %+v, want two results and no run error", status)
	}
	if !replicableBeforeRestore {
		t.Fatal("the operator's write did not store rs_map1 as replicable before the restore")
	}
	a, b := status.Results[0], status.Results[1]
	if a.Error != "" || a.AudioRealtimeConcurrency != 4 || a.AudioConcurrencyLimit != routing.AudioLimitCap {
		t.Fatalf("A = %d / %q, error %q; want 4 / cap: the conflicted restore of a replicable spec counts as restored", a.AudioRealtimeConcurrency, a.AudioConcurrencyLimit, a.Error)
	}
	byLevel := map[int][]string{} // A's ramp: its streams, B's aside
	for _, c := range script.recorded() {
		if !c.warmUp && c.level > 0 && strings.HasPrefix(c.model, "m1") {
			byLevel[c.level] = append(byLevel[c.level], c.model)
		}
	}
	for k := 1; k <= 4; k++ {
		models := byLevel[k]
		slices.Sort(models)
		if want := []string{"m1", "m1#1", "m1#2", "m1#3"}[:k]; !slices.Equal(models, want) {
			t.Fatalf("level %d streams went to %v, want one per instance %v", k, models, want)
		}
	}
	wantBatches := []string{"single [rs_map1]", "replicable [rs_map1]", "single [rs_map1]", "replicable [rs_map1]"}
	if got := f.events.with("single ", "replicable "); !slices.Equal(got, wantBatches) {
		t.Fatalf("replicable batches = %v, want %v: held, the conflicted restore, held again for B, restored at the end", got, wantBatches)
	}
	if hold, firstOfB := f.events.index("single [rs_map1]", 1), f.events.index("audio m2 level 0 warm-up false", 0); hold < f.events.lastIndex("ensure m1#3") || firstOfB < 0 || hold > firstOfB {
		t.Fatalf("events = %v, want A held again after its ramp and before B is measured", f.events.with("single ", "replicable ", "ensure ", "stop ", "audio m2 level 0"))
	}
	if stops != 2 || !heldAtStop || !leaseNamesA {
		t.Fatalf("stop batches = %d; at B's stop A held as one process %v, named in the lease %v; want both", stops, heldAtStop, leaseNamesA)
	}
	if b.Error != "" || b.AudioLoadTimeMS != 2000 {
		t.Fatalf("B = load %d ms, error %q; want the confirmed 2000", b.AudioLoadTimeMS, b.Error)
	}
	if !storedReplicable(t, f.mem, "rs_map1") {
		t.Fatal("after the run A is stored as not replicable, want the operator's replicable")
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
}

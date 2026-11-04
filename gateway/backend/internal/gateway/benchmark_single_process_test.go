// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
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
	p.mu.Unlock()
	if hook != nil {
		hook(kind, ids)
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
// records the replicable target in the lease and runs it as one process, and after the run
// it is replicable again and the lease is released. A target that is not replicable, and a
// neighbour that is but is not measured, are left alone.
func TestManualSpeedRunMeasuresAReplicableTargetAsOneProcess(t *testing.T) {
	t1 := stopAllSpecOf("map1", "running", 701)
	n := stopAllSpecOf("mapn", "stopped", 0)
	f := newStopAllFixture(t, stopAllOpts{specs: []stopAllSpec{t1, n}})
	makeReplicable(t, f.mem, "rs_map1")
	makeReplicable(t, f.mem, "rs_mapn")
	var leaseAtFlip portal.BenchmarkOverrideLease
	var flatAtStop bool
	f.portal.onReplicableBatch = func(kind string, _ []string) {
		if kind == "single" {
			leaseAtFlip = f.lease(t)
		}
	}
	f.portal.onBatch = func(kind string, _ []string) {
		if kind == "stop" {
			flatAtStop = !storedReplicable(t, f.mem, "rs_map1")
		}
	}

	status := f.run(t, "speed", f.targets(t, "speed", "map1"))

	if status.Error != "" {
		t.Fatalf("run error = %q", status.Error)
	}
	if got := f.events.with("single ", "replicable "); !slices.Equal(got, []string{"single [rs_map1]", "replicable [rs_map1]"}) {
		t.Fatalf("replicable batches = %v, want the target alone, once off and once back on", got)
	}
	if single, stop := f.events.index("single [rs_map1]", 0), f.events.index("stop [rs_map1]", 0); single < 0 || stop < 0 || single > stop {
		t.Fatalf("events = %v, want the target run as one process before its stop", f.events.with("single ", "stop "))
	}
	if !slices.Equal(leaseAtFlip.RestoreReplicable, []string{"rs_map1"}) {
		t.Fatalf("lease at the write = %+v, want restore_replicable [rs_map1] recorded before it", leaseAtFlip)
	}
	if !flatAtStop {
		t.Fatal("the target was still replicable when the run stopped it")
	}
	if !storedReplicable(t, f.mem, "rs_map1") || !storedReplicable(t, f.mem, "rs_mapn") {
		t.Fatal("after the run a replicable spec is stored as not replicable")
	}
	if got := f.lease(t); !got.Empty() {
		t.Fatalf("lease after the run = %+v, want it released", got)
	}
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
// every other ensure succeeds. The agent's flag per spec id follows every document it
// applies (watch), and each ensure is an event.
type replicableAgentEnsurer struct {
	provider.Client
	events *stopAllEvents
	specOf map[string]string // base model -> spec id

	mu         sync.Mutex
	replicable map[string]bool // spec id -> the flag the agent applied
}

func (e *replicableAgentEnsurer) LoadedModels(ctx context.Context, target routing.Target, path, format string) ([]string, error) {
	return e.Client.(provider.LoadedModelLister).LoadedModels(ctx, target, path, format)
}

func (e *replicableAgentEnsurer) EnsureRuntimeModel(_ context.Context, target routing.Target) error {
	e.events.add("ensure %s", target.ProviderModel)
	base, _, instance := strings.Cut(target.ProviderModel, "#")
	e.mu.Lock()
	allowed := e.replicable[e.specOf[base]]
	e.mu.Unlock()
	if instance && !allowed {
		return &provider.RouterError{Code: "runtime.model_not_managed", Err: provider.ErrUnavailable}
	}
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

// newBothRunAudioFixture is a stop-all fixture whose one spec, rs_map1 (model m1), is
// replicable on an agent that declares runtime_instances and serves a one-shot TTS mapping,
// with the agent's ensure route (replicableAgentEnsurer) and a scripted measurement that is
// always five times real time.
func newBothRunAudioFixture(t *testing.T, opts stopAllOpts) (*stopAllFixture, *audioScript, []benchmarkTarget) {
	t.Helper()
	opts.specs = []stopAllSpec{stopAllSpecOf("map1", "running", 731)}
	f := newStopAllFixture(t, opts)
	makeReplicable(t, f.mem, "rs_map1") // two protected instances, room for four
	f.srv.AgentFeatures.Set("srv1", []string{"runtime_manager", runtimeInstancesFeature})
	ensurer := &replicableAgentEnsurer{Client: f.srv.Provider, events: f.events, specOf: map[string]string{"m1": "rs_map1"}, replicable: map[string]bool{"rs_map1": true}}
	f.srv.Provider = ensurer
	f.agent.onApply = ensurer.watch
	script := &audioScript{answer: func(audioMeasureCall, int) (audioMeasurement, error) { return audioFigures(5, 100), nil }}
	f.srv.audioMeasureSeam = script.seam
	targets := f.targets(t, "both", "map1")
	targets[0].audio = audioTarget{role: routing.RoleTTS}
	return f, script, targets
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

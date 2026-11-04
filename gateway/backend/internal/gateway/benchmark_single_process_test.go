// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
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

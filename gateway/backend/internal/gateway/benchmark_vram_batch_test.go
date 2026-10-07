// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sync"
	"testing"
	"time"
)

// errVRAMInjectedGPUWrite is the store error vramGPUWriteFailStore injects.
var errVRAMInjectedGPUWrite = errors.New("store: injected GPU write failure")

// vramDrainFailedLog is the Warn vramDrain logs for each spec whose write
// failed.
const vramDrainFailedLog = "benchmark: could not force-stop a launch spec for the VRAM run"

// vramGPUWriteFailStore is a routing.Store whose SetRuntimeSpecGPUs fails once
// for each spec armed: the shape of a launch-spec write that stored its row
// and then failed on its GPU rows, so its override is stored although the
// write reports an error.
type vramGPUWriteFailStore struct {
	routing.Store
	mu    sync.Mutex
	armed map[string]bool
}

func (s *vramGPUWriteFailStore) arm(specIDs ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.armed == nil {
		s.armed = map[string]bool{}
	}
	for _, id := range specIDs {
		s.armed[id] = true
	}
}

func (s *vramGPUWriteFailStore) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	s.mu.Lock()
	fail := s.armed[specID]
	delete(s.armed, specID)
	s.mu.Unlock()
	if fail {
		return errVRAMInjectedGPUWrite
	}
	return s.Store.SetRuntimeSpecGPUs(ctx, specID, gpus)
}

// addVRAMFleetSpecs adds one enabled, unpinned launch spec without an override
// per id to the fixture's application, each on a mapping of its own, and
// reports every spec of the fleet as stopped.
func (f *vramFixture) addVRAMFleetSpecs(t *testing.T, ids ...string) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 2, 10, 0, 0, 0, time.UTC)
	statuses := []RuntimeStatusDTO{
		{SpecID: f.targetSpec, State: "stopped"},
		{SpecID: f.siblingSpec, State: "stopped"},
	}
	for _, id := range ids {
		mappingID := "map_" + id
		if err := f.mem.CreateMapping(ctx, routing.ModelMapping{
			ID: mappingID, ApplicationID: "app1", GatewayModelName: "gw-" + id, AppModelName: "up-" + id,
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateMapping(%s): %v", mappingID, err)
		}
		if err := f.mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: id, MappingID: mappingID, Enabled: true, Binary: "/usr/local/bin/llama-server",
			Args: "[]", Env: "{}", HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("UpsertRuntimeSpec(%s): %v", id, err)
		}
		statuses = append(statuses, RuntimeStatusDTO{SpecID: id, State: "stopped"})
	}
	f.setStatuses(statuses...)
}

// vramNotification is what the store held when the portal notified the
// server's agent: the admin_state of every spec of the application, and the
// server's override lease.
type vramNotification struct {
	adminStates map[string]string
	lease       portal.BenchmarkOverrideLease
}

// recordVRAMNotifications replaces the fixture's notification hook with one
// that records a vramNotification at every notification. The hook runs inside
// the writer, after its store writes, so each record is the state the
// document the notification announces is derived from. The returned function
// reports what was recorded so far.
func (f *vramFixture) recordVRAMNotifications(t *testing.T) func() []vramNotification {
	t.Helper()
	svc, ok := f.srv.Portal.(*portal.Service)
	if !ok {
		t.Fatalf("the fixture's Portal is %T, want *portal.Service", f.srv.Portal)
	}
	var mu sync.Mutex
	var got []vramNotification
	var hookErr error
	svc.SetRuntimeConfigChangedHook(func(string) {
		ctx := context.Background()
		specs, err := f.mem.RuntimeSpecsByApplication(ctx, "app1")
		leases, lerr := f.srv.Portal.BenchmarkOverrideLeases(ctx)
		mu.Lock()
		defer mu.Unlock()
		if readErr := errors.Join(err, lerr); readErr != nil {
			hookErr = errors.Join(hookErr, readErr)
			return
		}
		n := vramNotification{adminStates: map[string]string{}, lease: leases["srv1"]}
		for _, spec := range specs {
			n.adminStates[spec.ID] = spec.AdminState
		}
		got = append(got, n)
	})
	return func() []vramNotification {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if hookErr != nil {
			t.Fatalf("the notification hook could not read the store: %v", hookErr)
		}
		return append([]vramNotification(nil), got...)
	}
}

func vramAdminStatesOf(notifications []vramNotification) []map[string]string {
	out := make([]map[string]string, 0, len(notifications))
	for _, n := range notifications {
		out = append(out, n.adminStates)
	}
	return out
}

// TestVRAMDrainAndRestoreAreOneBatchEach pins that the VRAM run's drain and
// its restore each reach the agent as one document: one batched write with one
// notification after its last write. A write per spec sends one document per
// spec, most of them partial, and an agent before 0.8.1, whose sync is
// single-flight, can rest on the first, partial one even when they arrive in
// order.
//
// It also pins what the batched drain reports when it leaves specs unwritten:
// the error of the first of them in plan order, and one Warn per failed write.
func TestVRAMDrainAndRestoreAreOneBatchEach(t *testing.T) {
	fleet := []string{"rspec_sib", "rspec_target", "rspec_x1", "rspec_x2"}
	states := func(forceStopped ...string) map[string]string {
		out := map[string]string{}
		for _, id := range fleet {
			out[id] = ""
		}
		for _, id := range forceStopped {
			out[id] = vramAdminStateForceStopped
		}
		return out
	}

	t.Run("a 4-spec run notifies once for its drain and once for its restore", func(t *testing.T) {
		f := newVRAMFixture(t, vramFixtureOpts{})
		f.addVRAMFleetSpecs(t, "rspec_x1", "rspec_x2")
		f.seedLatestSample()
		f.drive(t)
		f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }
		notifications := f.recordVRAMNotifications(t)

		res := f.run(t).Results[0]
		if res.Error != "" || res.VRAM == nil || res.VRAM.Inconclusive != "" {
			t.Fatalf("result = %+v, want a definitive run", res)
		}
		want := []map[string]string{
			// the drain: one document that stops all four
			states(fleet...),
			// the target's own clear
			states("rspec_sib", "rspec_x1", "rspec_x2"),
			// the restore: one document that clears the other three
			states(),
		}
		if got := vramAdminStatesOf(notifications()); !reflect.DeepEqual(got, want) {
			t.Fatalf("admin states at each notification = %v, want %v", got, want)
		}
	})

	t.Run("a drain whose third write fails restores what it wrote and what it may have written", func(t *testing.T) {
		mem := &vramGPUWriteFailStore{Store: routing.NewMemoryStore()}
		f := newVRAMFixture(t, vramFixtureOpts{store: mem})
		f.addVRAMFleetSpecs(t, "rspec_x1", "rspec_x2")
		f.seedLatestSample()
		f.drive(t)
		notifications := f.recordVRAMNotifications(t)
		mem.arm("rspec_x1") // the third spec in the drain's sorted order

		res := f.run(t).Results[0]
		if res.Error != errVRAMInjectedGPUWrite.Error() {
			t.Fatalf("res.Error = %q, want the failed spec's own error %q", res.Error, errVRAMInjectedGPUWrite.Error())
		}
		report := res.VRAM
		if report == nil || report.Inconclusive != vramInconclusiveRunFailed {
			t.Fatalf("report = %+v, want Inconclusive %q", report, vramInconclusiveRunFailed)
		}
		if !reflect.DeepEqual(report.DrainedSpecIDs, fleet) {
			t.Fatalf("DrainedSpecIDs = %v, want %v: the batch goes on past the failure, and the failed write stored its override first", report.DrainedSpecIDs, fleet)
		}
		if len(report.RestoreFailed) != 0 || len(report.RestoreTakenOver) != 0 {
			t.Fatalf("RestoreFailed = %v, RestoreTakenOver = %v, want neither", report.RestoreFailed, report.RestoreTakenOver)
		}
		want := []map[string]string{
			// the drain, rspec_x1's stored override included
			states(fleet...),
			// the restore, which clears rspec_x1 too
			states(),
		}
		if got := vramAdminStatesOf(notifications()); !reflect.DeepEqual(got, want) {
			t.Fatalf("admin states at each notification = %v, want %v", got, want)
		}
		if got := f.allAdminStates(t); !reflect.DeepEqual(got, states()) {
			t.Fatalf("admin states after the run = %v, want every override cleared, rspec_x1's included", got)
		}
	})

	t.Run("a drain that leaves two specs unwritten reports the first one's error and warns once per failed write", testVRAMDrainLeavesTwoSpecsUnwritten)
}

// testVRAMDrainLeavesTwoSpecsUnwritten is the subtest of
// TestVRAMDrainAndRestoreAreOneBatchEach whose drain leaves two specs
// unwritten: a refused compare-and-set first, a failed write after it.
func testVRAMDrainLeavesTwoSpecsUnwritten(t *testing.T) {
	buf := withCapturedSlogAtTheDefaultLevel(t)
	ctx := context.Background()
	mem := &vramGPUWriteFailStore{Store: routing.NewMemoryStore()}
	f := newVRAMFixture(t, vramFixtureOpts{store: mem})
	f.addVRAMFleetSpecs(t, "rspec_x1", "rspec_x2", "rspec_x3")
	f.seedLatestSample()
	f.drive(t)
	plan, err := f.srv.vramRunPlan(ctx, f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	// After the plan, an override on rspec_x1 that the drain's
	// compare-and-set refuses; rspec_x2 and rspec_x3, after it in the
	// drain's sorted order, store their override and then fail.
	spec, ok, err := f.mem.RuntimeSpecByID(ctx, "rspec_x1")
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(rspec_x1) = (%v, %v)", ok, err)
	}
	spec.AdminState = vramAdminStateForceStopped
	if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("the override on rspec_x1: %v", err)
	}
	mem.arm("rspec_x2", "rspec_x3")
	run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)

	res := f.srv.Benchmarks.Status("srv1").Results[0]
	if res.Error != portal.ErrRuntimeSpecAdminStateConflict.Error() {
		t.Fatalf("res.Error = %q, want rspec_x1's own %q: the error is the first unwritten spec's in plan order", res.Error, portal.ErrRuntimeSpecAdminStateConflict.Error())
	}
	drained := []string{"rspec_sib", "rspec_target", "rspec_x2", "rspec_x3"}
	if res.VRAM == nil || !reflect.DeepEqual(res.VRAM.DrainedSpecIDs, drained) {
		t.Fatalf("report = %+v, want DrainedSpecIDs %v", res.VRAM, drained)
	}
	for _, id := range []string{"rspec_x2", "rspec_x3"} {
		assertLoggedOnce(t, buf, "WARN", vramDrainFailedLog, map[string]string{"server_id": "srv1", "spec_id": id, "err": errVRAMInjectedGPUWrite.Error()})
	}
	for _, r := range buf.Snapshot() {
		if r.Msg == vramDrainFailedLog && r.Attrs["spec_id"] == "rspec_x1" {
			t.Fatalf("the drain warned for rspec_x1, whose write the compare-and-set refused: %v", r)
		}
	}
	want := map[string]string{"rspec_sib": "", "rspec_target": "", "rspec_x1": vramAdminStateForceStopped, "rspec_x2": "", "rspec_x3": ""}
	if got := f.allAdminStates(t); !reflect.DeepEqual(got, want) {
		t.Fatalf("admin states after the run = %v, want %v: the restore clears what the drain wrote or may have written and leaves rspec_x1's override alone", got, want)
	}
}

// TestVRAMRunRecordsItsDrainInTheLeaseAfterTheDrain pins the lease write that
// follows the drain batch. Before the drain the lease names every spec the run
// planned to drain; from the batch's return on, it names exactly what the
// drain wrote or may have written, next to what an earlier run left behind. A
// spec the drain's compare-and-set refused carries an override that is not
// this run's -- here an operator's force_stopped, set after the trigger planned
// the run and before the run held the server -- and a reconcile of a lease
// that still named it would clear the operator's override.
func TestVRAMRunRecordsItsDrainInTheLeaseAfterTheDrain(t *testing.T) {
	ctx := context.Background()
	f := newVRAMFixture(t, vramFixtureOpts{})
	f.addVRAMFleetSpecs(t, "rspec_x1", "rspec_x2")
	f.seedLatestSample()
	f.drive(t)
	leftover := portal.BenchmarkOverrideLease{Repin: []string{"rspec_left_pin"}, ClearForceStopped: []string{"rspec_left_stop"}}
	if err := f.srv.Portal.SetBenchmarkOverrideLease(ctx, "srv1", leftover); err != nil {
		t.Fatalf("seed the leftover lease: %v", err)
	}
	if leases, err := f.srv.Portal.BenchmarkOverrideLeases(ctx); err != nil || !reflect.DeepEqual(leases["srv1"], leftover) {
		t.Fatalf("leftover lease read back = (%+v, %v), want %+v: the fixture's portal needs a system settings store", leases, err, leftover)
	}
	notifications := f.recordVRAMNotifications(t)

	plan, err := f.srv.vramRunPlan(ctx, f.target)
	if err != nil {
		t.Fatalf("vramRunPlan: %v", err)
	}
	spec, ok, err := f.mem.RuntimeSpecByID(ctx, "rspec_x1")
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(rspec_x1) = (%v, %v)", ok, err)
	}
	spec.AdminState = vramAdminStateForceStopped
	if err := f.mem.UpsertRuntimeSpec(ctx, spec); err != nil {
		t.Fatalf("the operator's override: %v", err)
	}
	run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatal("TryStart did not start")
	}
	f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)

	res := f.srv.Benchmarks.Status("srv1").Results[0]
	if res.Error != portal.ErrRuntimeSpecAdminStateConflict.Error() {
		t.Fatalf("res.Error = %q, want %q", res.Error, portal.ErrRuntimeSpecAdminStateConflict.Error())
	}
	drained := []string{"rspec_sib", "rspec_target", "rspec_x2"}
	if res.VRAM == nil || !reflect.DeepEqual(res.VRAM.DrainedSpecIDs, drained) {
		t.Fatalf("report = %+v, want DrainedSpecIDs %v", res.VRAM, drained)
	}
	got := notifications()
	if len(got) != 2 {
		t.Fatalf("notifications = %+v, want 2: the drain batch and the restore batch", got)
	}
	beforeDrain := portal.BenchmarkOverrideLease{
		Repin:             leftover.Repin,
		ClearForceStopped: []string{"rspec_left_stop", "rspec_sib", "rspec_target", "rspec_x1", "rspec_x2"},
	}
	if !reflect.DeepEqual(got[0].lease, beforeDrain) {
		t.Fatalf("lease at the drain's notification = %+v, want %+v: every spec the run planned to drain, next to the leftover", got[0].lease, beforeDrain)
	}
	afterDrain := portal.BenchmarkOverrideLease{
		Repin:             leftover.Repin,
		ClearForceStopped: []string{"rspec_left_stop", "rspec_sib", "rspec_target", "rspec_x2"},
	}
	if !reflect.DeepEqual(got[1].lease, afterDrain) {
		t.Fatalf("lease at the restore's notification = %+v, want %+v: from the drain's return on, the lease names what the drain wrote, next to the leftover, and never the operator's rspec_x1", got[1].lease, afterDrain)
	}
	if state := f.adminState(t, "rspec_x1"); state != vramAdminStateForceStopped {
		t.Fatalf("rspec_x1 admin_state = %q, want the operator's force_stopped left alone", state)
	}
	if leases, err := f.srv.Portal.BenchmarkOverrideLeases(ctx); err != nil || !reflect.DeepEqual(leases["srv1"], leftover) {
		t.Fatalf("lease after the run = (%+v, %v), want the leftover %+v: the restore cleared everything the run drained", leases, err, leftover)
	}
}

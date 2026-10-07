// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// vramLeaseSettings is the portal's settings store in a VRAM lease test. It can
// fail every read, fail every lease write, or store one lease write and then
// report an error (a commit whose answer was lost).
type vramLeaseSettings struct {
	*portal.MemorySystemSettings
	failRead, failWrite, storeThenFail atomic.Bool
}

func newVRAMLeaseSettings() *vramLeaseSettings {
	return &vramLeaseSettings{MemorySystemSettings: portal.NewMemorySystemSettings()}
}

func (s *vramLeaseSettings) SystemSettings(ctx context.Context) (map[string]string, error) {
	if s.failRead.Load() {
		return nil, errors.New("database is locked")
	}
	return s.MemorySystemSettings.SystemSettings(ctx)
}

func (s *vramLeaseSettings) SetSystemSetting(ctx context.Context, key, value string, now time.Time) error {
	if strings.HasPrefix(key, "benchmark_override_lease:") {
		if s.failWrite.Load() {
			return errors.New("database is locked")
		}
		if s.storeThenFail.CompareAndSwap(true, false) {
			_ = s.MemorySystemSettings.SetSystemSetting(ctx, key, value, now)
			return errors.New("connection reset after commit")
		}
	}
	return s.MemorySystemSettings.SetSystemSetting(ctx, key, value, now)
}

// row is srv1's raw lease row, read past every injected failure.
func (s *vramLeaseSettings) row() string {
	values, _ := s.MemorySystemSettings.SystemSettings(context.Background())
	return values["benchmark_override_lease:srv1"]
}

// vramClearFailRoutes fails the store write that clears the override of the
// spec ids in failClear: the restore's store error, single write or batch.
type vramClearFailRoutes struct {
	routing.Store
	failClear map[string]bool
}

func (r *vramClearFailRoutes) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	if r.failClear[spec.ID] && spec.AdminState == "" {
		return errors.New("database is locked")
	}
	return r.Store.UpsertRuntimeSpec(ctx, spec)
}

// vramClearThenFailRoutes stores the write that clears failAfterClear's
// override and then fails that same write's next step, its GPU rows: a write
// that returns an error after it stored the cleared row.
type vramClearThenFailRoutes struct {
	routing.Store
	failAfterClear string
	armed          atomic.Bool
}

func (r *vramClearThenFailRoutes) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	err := r.Store.UpsertRuntimeSpec(ctx, spec)
	if err == nil && spec.ID == r.failAfterClear && spec.AdminState == "" {
		r.armed.Store(true)
	}
	return err
}

func (r *vramClearThenFailRoutes) SetRuntimeSpecGPUs(ctx context.Context, specID string, gpus []routing.RuntimeSpecGPU) error {
	if specID == r.failAfterClear && r.armed.CompareAndSwap(true, false) {
		return errors.New("database is locked")
	}
	return r.Store.SetRuntimeSpecGPUs(ctx, specID, gpus)
}

// vramLeaseSpy is a VRAM run's portal. It reads the lease row at the run's
// first force_stopped write and at the write that clears the target's
// override, each single or batched, before the write reaches the store, and
// records every spec id any spec write names.
type vramLeaseSpy struct {
	portal.API
	row           func() string
	target        string
	mu            sync.Mutex
	drained       bool
	atDrain       string
	cleared       bool
	atTargetClear string
	touched       map[string]bool
}

func newVRAMLeaseSpy(f *vramFixture, row func() string) *vramLeaseSpy {
	spy := &vramLeaseSpy{API: f.srv.Portal, row: row, target: f.targetSpec, touched: map[string]bool{}}
	f.srv.Portal = spy
	return spy
}

func (p *vramLeaseSpy) saw(specIDs []string, expected, adminState string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range specIDs {
		p.touched[id] = true
	}
	if adminState == vramAdminStateForceStopped && !p.drained {
		p.drained = true
		p.atDrain = p.row()
	}
	if expected == vramAdminStateForceStopped && adminState == "" && slices.Contains(specIDs, p.target) && !p.cleared {
		p.cleared = true
		p.atTargetClear = p.row()
	}
}

func (p *vramLeaseSpy) SetBenchmarkRuntimeSpecAdminState(ctx context.Context, specID, expected, adminState string) (portal.RuntimeSpecDTO, error) {
	p.saw([]string{specID}, expected, adminState)
	return p.API.SetBenchmarkRuntimeSpecAdminState(ctx, specID, expected, adminState)
}

func (p *vramLeaseSpy) SetBenchmarkRuntimeSpecsAdminState(ctx context.Context, specIDs []string, expected, adminState string) (portal.BenchmarkSpecsOutcome, error) {
	p.saw(specIDs, expected, adminState)
	return p.API.SetBenchmarkRuntimeSpecsAdminState(ctx, specIDs, expected, adminState)
}

func (p *vramLeaseSpy) SetBenchmarkRuntimeSpecsPinned(ctx context.Context, specIDs []string, expected, pinned bool) (portal.BenchmarkSpecsOutcome, error) {
	p.saw(specIDs, "", "")
	return p.API.SetBenchmarkRuntimeSpecsPinned(ctx, specIDs, expected, pinned)
}

// carriedLease is a leftover row an earlier run on srv1 could not release. Its
// clear entry for rspec_sib is stale (the VRAM plan refuses an enabled spec
// that still carries an override), and spec_old_stop sorts after the drain's
// specs, so the row the run writes shows both the merge and the order.
const carriedLease = `{"repin":["spec_old_pin"],"clear_force_stopped":["rspec_sib","spec_old_stop"]}`

func seedVRAMLease(t *testing.T, settings portal.SystemSettingsStore, value string) {
	t.Helper()
	if err := settings.SetSystemSetting(context.Background(), "benchmark_override_lease:srv1", value, time.Now().UTC()); err != nil {
		t.Fatalf("seed the lease row: %v", err)
	}
}

// TestVRAMRunRecordsItsDrainInTheLease pins that a VRAM run records every
// force_stopped override it owes in the server's override lease, which the
// gateway reconciles when it starts again: the drain's set before a single
// spec is written and still when the target's override is cleared, what the
// restore still owes only after that clear, and what the restore could not
// clear after it. A leftover row is carried over unchanged and never settled
// by the run, and without a readable and writable lease nothing is drained.
func TestVRAMRunRecordsItsDrainInTheLease(t *testing.T) {
	t.Run("the row names the drain from before it until the target's clear, then the restore's set, and is released after the restore", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
		f.seedLatestSample()
		f.drive(t)
		spy := newVRAMLeaseSpy(f, settings.row)
		var atLoad string
		f.provider.onStream = func() {
			atLoad = settings.row()
			f.used0.Store(21500 * oneMiB)
		}

		res := f.run(t).Results[0]

		if res.Error != "" {
			t.Fatalf("res.Error = %q", res.Error)
		}
		if spy.atDrain != `{"clear_force_stopped":["rspec_sib","rspec_target"]}` {
			t.Fatalf("lease row at the drain = %q, want every spec the drain force-stops", spy.atDrain)
		}
		if spy.atTargetClear != `{"clear_force_stopped":["rspec_sib","rspec_target"]}` {
			t.Fatalf("lease row at the target's clear = %q, want it still naming rspec_target: the row narrows only after that clear, or a crash in between leaves the target force_stopped with no entry to clear it", spy.atTargetClear)
		}
		if atLoad != `{"clear_force_stopped":["rspec_sib"]}` {
			t.Fatalf("lease row at the load = %q, want only what the restore still owes", atLoad)
		}
		values, err := settings.SystemSettings(context.Background())
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		if got, ok := values["benchmark_override_lease:srv1"]; !ok || got != "" {
			t.Fatalf("lease row after the run = (%q, present %v), want released", got, ok)
		}
	})

	t.Run("a failed restore leaves exactly what it could not clear", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		routes := &vramClearFailRoutes{Store: routing.NewMemoryStore()}
		f := newVRAMFixture(t, vramFixtureOpts{store: routes, settings: settings})
		routes.failClear = map[string]bool{"rspec_sib": true}
		f.seedLatestSample()
		f.drive(t)
		f.provider.onStream = func() { f.used0.Store(21500 * oneMiB) }

		res := f.run(t).Results[0]

		if res.VRAM == nil || len(res.VRAM.RestoreFailed) != 1 || res.VRAM.RestoreFailed[0] != "rspec_sib" {
			t.Fatalf("report = %#v, want RestoreFailed [rspec_sib]", res.VRAM)
		}
		if got := settings.row(); got != `{"clear_force_stopped":["rspec_sib"]}` {
			t.Fatalf("lease row after the run = %q, want exactly the failed restore", got)
		}
		if state := f.adminState(t, "rspec_sib"); state != vramAdminStateForceStopped {
			t.Fatalf("rspec_sib admin_state = %q, want force_stopped (its restore failed)", state)
		}
	})

	// The run's own clear of the target can store "" and still fail. The
	// target then stays owed to the restore, whose compare-and-set finds ""
	// and reports it taken over: neither a stop that was never stored nor a
	// writer the run does not hold off, which is why the taken-over cause
	// list names the run's own clear.
	t.Run("a target clear that failed after it stored the cleared row is taken over, not failed", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		routes := &vramClearThenFailRoutes{Store: routing.NewMemoryStore()}
		f := newVRAMFixture(t, vramFixtureOpts{store: routes, settings: settings})
		routes.failAfterClear = f.targetSpec
		f.seedLatestSample()
		f.drive(t)

		res := f.run(t).Results[0]

		if res.Error != "database is locked" {
			t.Fatalf("res.Error = %q, want the clear's error", res.Error)
		}
		if res.VRAM == nil || res.VRAM.Inconclusive != vramInconclusiveRunFailed {
			t.Fatalf("report = %#v, want inconclusive %q", res.VRAM, vramInconclusiveRunFailed)
		}
		if len(res.VRAM.RestoreFailed) != 0 {
			t.Fatalf("RestoreFailed = %v, want none: the target's override is gone, so clearing it by hand names nothing", res.VRAM.RestoreFailed)
		}
		if got := res.VRAM.RestoreTakenOver; len(got) != 1 || got[0] != f.targetSpec {
			t.Fatalf("RestoreTakenOver = %v, want [%s]", got, f.targetSpec)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("%s admin_state = %q, want every override cleared", specID, state)
			}
		}
		if got := settings.row(); got != "" {
			t.Fatalf("lease row after the run = %q, want released", got)
		}
	})

	t.Run("a leftover row's two lists are carried over unchanged and never settled by the run", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		seedVRAMLease(t, settings, carriedLease)
		f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
		f.seedLatestSample()
		f.drive(t)
		spy := newVRAMLeaseSpy(f, settings.row)
		var atLoad string
		f.provider.onStream = func() {
			atLoad = settings.row()
			f.used0.Store(21500 * oneMiB)
		}

		res := f.run(t).Results[0]

		if res.Error != "" {
			t.Fatalf("res.Error = %q", res.Error)
		}
		if want := `{"repin":["spec_old_pin"],"clear_force_stopped":["rspec_sib","rspec_target","spec_old_stop"]}`; spy.atDrain != want {
			t.Fatalf("lease row at the drain = %q, want %q", spy.atDrain, want)
		}
		if want := `{"repin":["spec_old_pin"],"clear_force_stopped":["rspec_sib","spec_old_stop"]}`; atLoad != want {
			t.Fatalf("lease row at the load = %q, want %q", atLoad, want)
		}
		if got := settings.row(); got != carriedLease {
			t.Fatalf("lease row after the run = %q, want the carried-over row %q", got, carriedLease)
		}
		for _, id := range []string{"spec_old_pin", "spec_old_stop"} {
			if spy.touched[id] {
				t.Fatalf("the run wrote %s: a VRAM run never settles a leftover lease itself", id)
			}
		}
	})

	t.Run("a lease read error fails the run before anything is drained", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
		f.seedLatestSample()
		f.drive(t)
		settings.failRead.Store(true)

		res := f.run(t).Results[0]

		if res.Error != "read the benchmark override leases: database is locked" {
			t.Fatalf("res.Error = %q", res.Error)
		}
		if res.VRAM != nil {
			t.Fatalf("report = %#v, want none: the run never reached the measurement phase", res.VRAM)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("%s admin_state = %q, want nothing drained", specID, state)
			}
		}
		if got := f.notifies(); len(got) != 0 {
			t.Fatalf("notifications = %v, want none", got)
		}
	})

	t.Run("a lease write error fails the run before anything is drained, and a failed put-back is a Warn", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		defer restore()
		settings := newVRAMLeaseSettings()
		seedVRAMLease(t, settings, carriedLease)
		f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
		f.seedLatestSample()
		f.drive(t)
		settings.failWrite.Store(true)

		res := f.run(t).Results[0]

		if res.Error != "write the benchmark override lease of server srv1: database is locked" {
			t.Fatalf("res.Error = %q", res.Error)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("%s admin_state = %q, want nothing drained", specID, state)
			}
		}
		if got := settings.row(); got != carriedLease {
			t.Fatalf("lease row = %q, want the carried-over row untouched", got)
		}
		if got := leaseLogAttr(t, buf, "WARN", "vram benchmark: could not update the override lease; the row may name more than the run still owes", "err"); got != "write the benchmark override lease of server srv1: database is locked" {
			t.Fatalf("Warn err = %v", got)
		}
	})

	t.Run("a pre-drain write that stored the row and then failed is put back to what was carried over", func(t *testing.T) {
		settings := newVRAMLeaseSettings()
		seedVRAMLease(t, settings, carriedLease)
		f := newVRAMFixture(t, vramFixtureOpts{settings: settings})
		f.seedLatestSample()
		f.drive(t)
		settings.storeThenFail.Store(true)

		res := f.run(t).Results[0]

		if res.Error != "write the benchmark override lease of server srv1: connection reset after commit" {
			t.Fatalf("res.Error = %q", res.Error)
		}
		for specID, state := range f.allAdminStates(t) {
			if state != "" {
				t.Fatalf("%s admin_state = %q, want nothing drained", specID, state)
			}
		}
		if got := settings.row(); got != carriedLease {
			t.Fatalf("lease row = %q, want it put back to the carried-over row %q", got, carriedLease)
		}
	})

	t.Run("a cancelled run still releases the row", func(t *testing.T) {
		sqlStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "vram-lease-cancel.db"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		t.Cleanup(func() { _ = sqlStore.Close() })
		if err := sqlStore.Migrate(context.Background()); err != nil {
			t.Fatalf("migrate sqlite: %v", err)
		}
		f := newVRAMFixture(t, vramFixtureOpts{store: sqlStore, settings: sqlStore})
		f.seedLatestSample()
		f.drive(t)
		plan, err := f.srv.vramRunPlan(context.Background(), f.target)
		if err != nil {
			t.Fatalf("vramRunPlan: %v", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run, ok := f.srv.Benchmarks.TryStart("srv1", "vram-probe", "vram", 1, time.Now().UTC(), cancel)
		if !ok {
			t.Fatal("TryStart did not start")
		}
		f.provider.onStream = func() { cancel() }

		f.srv.runVRAMProbe(ctx, run, "srv1", f.target, plan)

		values, err := sqlStore.SystemSettings(context.Background())
		if err != nil {
			t.Fatalf("SystemSettings: %v", err)
		}
		if got, ok := values["benchmark_override_lease:srv1"]; !ok || got != "" {
			t.Fatalf("lease row after a cancelled run = (%q, present %v), want released: the row must follow the restore", got, ok)
		}
	})
}

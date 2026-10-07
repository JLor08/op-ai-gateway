// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// leaseFlakyRoutes fails every launch-spec write of the spec ids in fail,
// which is the store error a reconcile has to keep in the lease.
type leaseFlakyRoutes struct {
	routing.Store
	fail map[string]bool
}

func (r leaseFlakyRoutes) UpsertRuntimeSpec(ctx context.Context, spec routing.RuntimeSpec) error {
	if r.fail[spec.ID] {
		return errors.New("database is locked")
	}
	return r.Store.UpsertRuntimeSpec(ctx, spec)
}

// leaseRecordingPortal is the real portal.Service with every batched spec
// write and every lease write recorded in call order. leasesErr, when set,
// fails BenchmarkOverrideLeases.
type leaseRecordingPortal struct {
	portal.API
	leasesErr error
	mu        sync.Mutex
	calls     []string
}

func (p *leaseRecordingPortal) record(call string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, call)
}

func (p *leaseRecordingPortal) recorded() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

func (p *leaseRecordingPortal) BenchmarkOverrideLeases(ctx context.Context) (map[string]portal.BenchmarkOverrideLease, error) {
	if p.leasesErr != nil {
		return nil, p.leasesErr
	}
	return p.API.BenchmarkOverrideLeases(ctx)
}

func (p *leaseRecordingPortal) SetBenchmarkRuntimeSpecsAdminState(ctx context.Context, specIDs []string, expected, adminState string) (portal.BenchmarkSpecsOutcome, error) {
	p.record(fmt.Sprintf("admin_state %q->%q %v", expected, adminState, specIDs))
	return p.API.SetBenchmarkRuntimeSpecsAdminState(ctx, specIDs, expected, adminState)
}

func (p *leaseRecordingPortal) SetBenchmarkRuntimeSpecsPinned(ctx context.Context, specIDs []string, expected, pinned bool) (portal.BenchmarkSpecsOutcome, error) {
	p.record(fmt.Sprintf("pinned %v->%v %v", expected, pinned, specIDs))
	return p.API.SetBenchmarkRuntimeSpecsPinned(ctx, specIDs, expected, pinned)
}

func (p *leaseRecordingPortal) SetBenchmarkOverrideLease(ctx context.Context, serverID string, lease portal.BenchmarkOverrideLease) error {
	p.record(fmt.Sprintf("lease %s repin=%v clear_force_stopped=%v", serverID, lease.Repin, lease.ClearForceStopped))
	return p.API.SetBenchmarkOverrideLease(ctx, serverID, lease)
}

type leaseFixture struct {
	srv      *Server
	routes   routing.Store
	settings portal.SystemSettingsStore
	portal   *leaseRecordingPortal
}

// srv1Lease is what a dead run left on srv1: every spec of srv1 that a
// reconcile can meet, and rs_gone, which no longer exists.
var srv1Lease = portal.BenchmarkOverrideLease{
	Repin:             []string{"rs_gone", "rs_pin", "rs_repin_fail", "rs_unpin"},
	ClearForceStopped: []string{"rs_clear_fail", "rs_gone", "rs_idle", "rs_stop"},
}

// newLeaseFixture seeds two servers, each with one server_agent application
// and one mapping per launch spec, and wires a real portal.Service over st and
// settings behind a leaseRecordingPortal. srv1's specs cover every way a
// reconcile can find a leased spec:
//   - rs_stop: force_stopped, which the reconcile clears;
//   - rs_idle: no override, a conflict for the clear;
//   - rs_unpin: unpinned, which the reconcile pins again;
//   - rs_pin: pinned, a conflict for the re-pin;
//   - rs_clear_fail and rs_repin_fail: as rs_stop and rs_unpin, but every
//     write of theirs fails in the store.
//
// rs_gone is never created. srv2 holds rs_s2_stop, force_stopped.
func newLeaseFixture(t *testing.T, st routing.Store, settings portal.SystemSettingsStore) *leaseFixture {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	for _, server := range []struct{ id, app string }{{"srv1", "app1"}, {"srv2", "app2"}} {
		must("CreateAIServer("+server.id+")", st.CreateAIServer(ctx, routing.AIServer{ID: server.id, Name: server.id, Domain: server.id + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + server.id, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
		must("CreateApplication("+server.app+")", st.CreateApplication(ctx, routing.Application{ID: server.app, ServerID: server.id, Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	}
	for _, spec := range []struct {
		id, app, adminState string
		pinned              bool
	}{
		{"rs_stop", "app1", "force_stopped", false},
		{"rs_idle", "app1", "", false},
		{"rs_unpin", "app1", "", false},
		{"rs_pin", "app1", "", true},
		{"rs_clear_fail", "app1", "force_stopped", false},
		{"rs_repin_fail", "app1", "", false},
		{"rs_s2_stop", "app2", "force_stopped", false},
	} {
		mappingID := "map_" + spec.id
		must("CreateMapping("+mappingID+")", st.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: spec.app, GatewayModelName: "gw-" + spec.id, AppModelName: "up-" + spec.id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("UpsertRuntimeSpec("+spec.id+")", st.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: spec.id, MappingID: mappingID, Enabled: true, Binary: "/usr/local/bin/llama-server",
			Args: "[]", Env: "{}", HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			Pinned: spec.pinned, AdminState: spec.adminState, CreatedAt: now, UpdatedAt: now,
		}))
	}
	routes := leaseFlakyRoutes{Store: st, fail: map[string]bool{"rs_clear_fail": true, "rs_repin_fail": true}}
	dir := portal.NewMemoryDirectory(nil)
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Groups: dir, Usage: usage.NewRecorder(), Routes: routes, SystemSettings: settings,
		Clock: func() time.Time { return now },
	})
	rec := &leaseRecordingPortal{API: svc}
	return &leaseFixture{srv: &Server{Routes: routes, Portal: rec}, routes: st, settings: settings, portal: rec}
}

func (f *leaseFixture) seedLease(t *testing.T, serverID string, lease portal.BenchmarkOverrideLease) {
	t.Helper()
	svc := f.portal.API
	if err := svc.SetBenchmarkOverrideLease(context.Background(), serverID, lease); err != nil {
		t.Fatalf("seed the lease of %s: %v", serverID, err)
	}
}

// leaseRow is the raw lease row of serverID: the empty string when it is
// released or was never written.
func (f *leaseFixture) leaseRow(t *testing.T, serverID string) string {
	t.Helper()
	values, err := f.settings.SystemSettings(context.Background())
	if err != nil {
		t.Fatalf("SystemSettings: %v", err)
	}
	return values["benchmark_override_lease:"+serverID]
}

// specState is a spec's admin_state and pinned, as stored.
func (f *leaseFixture) specState(t *testing.T, specID string) string {
	t.Helper()
	spec, ok, err := f.routes.RuntimeSpecByID(context.Background(), specID)
	if err != nil || !ok {
		t.Fatalf("RuntimeSpecByID(%q) = (%v, %v)", specID, ok, err)
	}
	return fmt.Sprintf("admin_state=%q pinned=%v", spec.AdminState, spec.Pinned)
}

// leaseLogAttr returns the value of attribute key on the first captured record
// with exactly this level and message, and fails the test when there is none.
func leaseLogAttr(t *testing.T, buf *logbuffer.Buffer, level, msg, key string) any {
	t.Helper()
	for _, rec := range buf.Snapshot() {
		if rec.Level == level && rec.Msg == msg {
			return rec.Attrs[key]
		}
	}
	t.Fatalf("no %s record %q", level, msg)
	return nil
}

func newLeaseSQLite(t *testing.T) *store.SQLiteStore {
	t.Helper()
	sqlStore, err := store.OpenSQLite(filepath.Join(t.TempDir(), "lease.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = sqlStore.Close() })
	if err := sqlStore.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return sqlStore
}

// TestReconcileBenchmarkOverrideLeases pins the reconciler of the benchmark
// override lease: it clears and pins again exactly what a dead run recorded,
// by compare-and-set against the value that run wrote, clears before it
// re-pins, and rewrites the row to what it could not settle.
func TestReconcileBenchmarkOverrideLeases(t *testing.T) {
	t.Run("it settles each entry by compare-and-set, clears first, and keeps only the failures", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		defer restore()
		f := newLeaseFixture(t, routing.NewMemoryStore(), portal.NewMemorySystemSettings())
		f.seedLease(t, "srv1", srv1Lease)

		left := f.srv.reconcileBenchmarkOverrideLease(context.Background(), "srv1", srv1Lease)

		wantLeft := portal.BenchmarkOverrideLease{Repin: []string{"rs_repin_fail"}, ClearForceStopped: []string{"rs_clear_fail"}}
		if !reflect.DeepEqual(left, wantLeft) {
			t.Fatalf("left = %#v, want %#v", left, wantLeft)
		}
		wantCalls := []string{
			`admin_state "force_stopped"->"" [rs_clear_fail rs_gone rs_idle rs_stop]`,
			`pinned false->true [rs_gone rs_pin rs_repin_fail rs_unpin]`,
			`lease srv1 repin=[rs_repin_fail] clear_force_stopped=[rs_clear_fail]`,
		}
		if got := f.portal.recorded(); !reflect.DeepEqual(got, wantCalls) {
			t.Fatalf("calls = %q, want %q (the clear batch first, then the re-pin batch, then the row)", got, wantCalls)
		}
		for specID, want := range map[string]string{
			"rs_stop":       `admin_state="" pinned=false`,
			"rs_idle":       `admin_state="" pinned=false`,
			"rs_unpin":      `admin_state="" pinned=true`,
			"rs_pin":        `admin_state="" pinned=true`,
			"rs_clear_fail": `admin_state="force_stopped" pinned=false`,
			"rs_repin_fail": `admin_state="" pinned=false`,
		} {
			if got := f.specState(t, specID); got != want {
				t.Fatalf("%s: %s, want %s", specID, got, want)
			}
		}
		if got := f.leaseRow(t, "srv1"); got != `{"repin":["rs_repin_fail"],"clear_force_stopped":["rs_clear_fail"]}` {
			t.Fatalf("lease row = %q, want only the two failures, each in its own list", got)
		}

		for _, want := range []struct {
			level, msg string
			specIDs    []string
		}{
			{"WARN", "benchmark: cleared force_stopped overrides that a benchmark left behind", []string{"rs_stop"}},
			{"WARN", "benchmark: pinned launch specs again that a benchmark left unpinned", []string{"rs_unpin"}},
			{"WARN", "benchmark: could not clear force_stopped overrides that a benchmark left behind; they may still be force_stopped, so clear them in the runtime section", []string{"rs_clear_fail"}},
			{"WARN", "benchmark: could not pin launch specs again that a benchmark left unpinned; they may still be unpinned, so pin them in the runtime section", []string{"rs_repin_fail"}},
		} {
			if got := leaseLogAttr(t, buf, want.level, want.msg, "spec_ids"); !reflect.DeepEqual(got, want.specIDs) {
				t.Fatalf("%q spec_ids = %v, want %v", want.msg, got, want.specIDs)
			}
		}
		var dropped []string
		for _, rec := range buf.Snapshot() {
			if rec.Level == "INFO" && rec.Msg == "benchmark: dropped override lease entries that no longer apply" {
				dropped = append(dropped, fmt.Sprintf("%v gone=%v conflict=%v", rec.Attrs["list"], rec.Attrs["gone"], rec.Attrs["conflict"]))
			}
		}
		wantDropped := []string{"clear_force_stopped gone=[rs_gone] conflict=[rs_idle]", "repin gone=[rs_gone] conflict=[rs_pin]"}
		if !reflect.DeepEqual(dropped, wantDropped) {
			t.Fatalf("dropped = %q, want %q", dropped, wantDropped)
		}
	})

	t.Run("a lease it settles completely is released", func(t *testing.T) {
		f := newLeaseFixture(t, routing.NewMemoryStore(), portal.NewMemorySystemSettings())
		lease := portal.BenchmarkOverrideLease{Repin: []string{"rs_unpin"}, ClearForceStopped: []string{"rs_stop"}}
		f.seedLease(t, "srv1", lease)

		if left := f.srv.reconcileBenchmarkOverrideLease(context.Background(), "srv1", lease); !left.Empty() {
			t.Fatalf("left = %#v, want nothing", left)
		}
		if got := f.leaseRow(t, "srv1"); got != "" {
			t.Fatalf("lease row = %q, want '' (released)", got)
		}
		if got := f.specState(t, "rs_stop"); got != `admin_state="" pinned=false` {
			t.Fatalf("rs_stop: %s", got)
		}
		if got := f.specState(t, "rs_unpin"); got != `admin_state="" pinned=true` {
			t.Fatalf("rs_unpin: %s", got)
		}
	})

	t.Run("a cancelled caller context still settles the lease", func(t *testing.T) {
		sqlStore := newLeaseSQLite(t)
		f := newLeaseFixture(t, sqlStore, sqlStore)
		lease := portal.BenchmarkOverrideLease{Repin: []string{"rs_unpin"}, ClearForceStopped: []string{"rs_stop"}}
		f.seedLease(t, "srv1", lease)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		if left := f.srv.reconcileBenchmarkOverrideLease(ctx, "srv1", lease); !left.Empty() {
			t.Fatalf("left = %#v, want nothing: the reconcile's writes are restores and must not die with their caller", left)
		}
		if got := f.leaseRow(t, "srv1"); got != "" {
			t.Fatalf("lease row = %q, want '' (released)", got)
		}
		if got := f.specState(t, "rs_stop"); got != `admin_state="" pinned=false` {
			t.Fatalf("rs_stop: %s", got)
		}
		if got := f.specState(t, "rs_unpin"); got != `admin_state="" pinned=true` {
			t.Fatalf("rs_unpin: %s", got)
		}
	})

	t.Run("at gateway start every server's leftover lease is settled", func(t *testing.T) {
		_, restore := withCapturedSlog(t)
		defer restore()
		f := newLeaseFixture(t, routing.NewMemoryStore(), portal.NewMemorySystemSettings())
		f.seedLease(t, "srv2", portal.BenchmarkOverrideLease{ClearForceStopped: []string{"rs_s2_stop"}})
		f.seedLease(t, "srv1", portal.BenchmarkOverrideLease{ClearForceStopped: []string{"rs_stop"}})
		// Thirty more leases, of servers whose lease names only a spec that no
		// longer exists. Their ids sort between srv1 and srv2. With 32 leases a
		// walk in map order matches the id order with negligible probability,
		// so the call order below pins the sort on every run.
		var between []string
		for i := range 30 {
			id := fmt.Sprintf("srv1_%02d", i)
			between = append(between, id)
			f.seedLease(t, id, portal.BenchmarkOverrideLease{ClearForceStopped: []string{"rs_gone"}})
		}

		f.srv.ReconcileBenchmarkOverrideLeases(context.Background())

		for _, server := range []struct{ id, spec string }{{"srv1", "rs_stop"}, {"srv2", "rs_s2_stop"}} {
			if got := f.leaseRow(t, server.id); got != "" {
				t.Fatalf("%s lease row = %q, want '' (released)", server.id, got)
			}
			if got := f.specState(t, server.spec); got != `admin_state="" pinned=false` {
				t.Fatalf("%s: %s", server.spec, got)
			}
		}
		wantCalls := []string{
			`admin_state "force_stopped"->"" [rs_stop]`,
			`pinned false->true []`,
			`lease srv1 repin=[] clear_force_stopped=[]`,
		}
		for _, id := range between {
			wantCalls = append(wantCalls,
				`admin_state "force_stopped"->"" [rs_gone]`,
				`pinned false->true []`,
				"lease "+id+" repin=[] clear_force_stopped=[]",
			)
		}
		wantCalls = append(wantCalls,
			`admin_state "force_stopped"->"" [rs_s2_stop]`,
			`pinned false->true []`,
			`lease srv2 repin=[] clear_force_stopped=[]`,
		)
		if got := f.portal.recorded(); !reflect.DeepEqual(got, wantCalls) {
			t.Fatalf("calls = %q, want %q (server by server, in server id order)", got, wantCalls)
		}
	})

	t.Run("at gateway start a lease read error writes nothing and says so", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		defer restore()
		f := newLeaseFixture(t, routing.NewMemoryStore(), portal.NewMemorySystemSettings())
		f.seedLease(t, "srv1", portal.BenchmarkOverrideLease{ClearForceStopped: []string{"rs_stop"}})
		f.portal.leasesErr = errors.New("read the benchmark override leases: database is locked")

		f.srv.ReconcileBenchmarkOverrideLeases(context.Background())

		if got := f.portal.recorded(); len(got) != 0 {
			t.Fatalf("calls = %q, want none", got)
		}
		if got := leaseLogAttr(t, buf, "WARN", "benchmark: could not read the override leases; leftover overrides are not reconciled at this start", "err"); got != "read the benchmark override leases: database is locked" {
			t.Fatalf("err attr = %v", got)
		}
		if got := f.specState(t, "rs_stop"); got != `admin_state="force_stopped" pinned=false` {
			t.Fatalf("rs_stop: %s", got)
		}
	})
}

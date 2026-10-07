// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBuildGatewayServerReconcilesALeftoverBenchmarkOverrideLease pins the
// startup leg of the benchmark override lease: a row that a dead run left in
// the store is settled before buildGatewayServer returns, which is before any
// listener serves a request, and the call sits after the reservation hook and
// before the benchmark scheduler starts.
func TestBuildGatewayServerReconcilesALeftoverBenchmarkOverrideLease(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "gateway.db")
	seed, err := store.OpenSQLite(dbPath)
	if err != nil {
		t.Fatalf("open seed: %v", err)
	}
	if err := seed.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().UTC()
	must := func(what string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	must("server", seed.CreateAIServer(ctx, routing.AIServer{ID: "s", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://s", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must("application", seed.CreateApplication(ctx, routing.Application{ID: "a", ServerID: "s", Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable, AlwaysReachable: true, CreatedAt: now, UpdatedAt: now}))
	for _, spec := range []struct {
		id, adminState string
	}{{"rs_stop", "force_stopped"}, {"rs_unpin", ""}} {
		must("mapping", seed.CreateMapping(ctx, routing.ModelMapping{ID: "m_" + spec.id, ApplicationID: "a", GatewayModelName: "gw-" + spec.id, AppModelName: "up-" + spec.id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
		must("spec", seed.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
			ID: spec.id, MappingID: "m_" + spec.id, Enabled: true, Binary: "/usr/local/bin/llama-server",
			Args: "[]", Env: "{}", HealthPath: "/health", HealthTimeoutSeconds: 5, StartupTimeoutSeconds: 180,
			AdminState: spec.adminState, CreatedAt: now, UpdatedAt: now,
		}))
	}
	must("lease", seed.SetSystemSetting(ctx, "benchmark_override_lease:s", `{"repin":["rs_unpin"],"clear_force_stopped":["rs_stop"]}`, now))
	must("close seed", seed.Close())

	srv, cleanup, err := buildGatewayServer(config.Config{DBDriver: "sqlite", SQLitePath: dbPath, AutoMigrate: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer cleanup()

	stopped, _, err := srv.Routes.RuntimeSpecByID(ctx, "rs_stop")
	if err != nil || stopped.AdminState != "" {
		t.Fatalf("rs_stop admin_state = %q (err %v), want the leftover override cleared at start", stopped.AdminState, err)
	}
	unpinned, _, err := srv.Routes.RuntimeSpecByID(ctx, "rs_unpin")
	if err != nil || !unpinned.Pinned {
		t.Fatalf("rs_unpin pinned = %v (err %v), want it pinned again at start", unpinned.Pinned, err)
	}
	leases, err := srv.Portal.BenchmarkOverrideLeases(ctx)
	if err != nil || len(leases) != 0 {
		t.Fatalf("leases after start = (%#v, %v), want the row released", leases, err)
	}

	body := funcSource(t, "main.go", "buildGatewayServer")
	hook := strings.Index(body, "deps.SetBenchmarkReservationHook(srv.Benchmarks.ServerBusy)")
	reconcile := strings.Index(body, "srv.ReconcileBenchmarkOverrideLeases(context.Background())")
	scheduler := strings.Index(body, "srv.StartBenchmarkScheduler(")
	if hook < 0 || reconcile < hook || scheduler < reconcile {
		t.Fatalf("buildGatewayServer order: reservation hook at %d, reconcile at %d, scheduler at %d; want the reconcile after the hook and before the scheduler", hook, reconcile, scheduler)
	}
}

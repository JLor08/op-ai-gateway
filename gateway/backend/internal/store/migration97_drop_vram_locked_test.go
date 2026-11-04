// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
	"time"
)

// reinvokeMigration97 runs migration97Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that no longer has the
// column.
func reinvokeMigration97(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration97Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration97Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration97DropsVRAMLockedFromAnUpgradedDatabase runs migration 97
// against a genuine version-96 database that already holds a spec row whose
// vram_locked is set, and pins what an upgrade of a live deployment depends on:
// the column exists before the migration (migration 65 still creates it), is
// gone after it on BOTH dialects, and the rest of the row -- every other column
// the readers select -- survives the drop untouched, so the store's own
// insert/update/select statements agree with the post-97 schema.
func TestMigration97DropsVRAMLockedFromAnUpgradedDatabase(t *testing.T) {
	forEachDialectMigratedTo(t, 96, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

		before := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		if _, ok := before["vram_locked"]; !ok {
			t.Fatalf("agent_runtime_specs has no vram_locked at v96, so this test does not start from an upgrade shape; got %v", sortedColumnNames(before))
		}

		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m97", Name: "m97", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m97:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m97", ServerID: "srv_m97", Type: routing.ProviderServerAgent, Port: 8097, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m97", ApplicationID: "app_m97", GatewayModelName: "m97-model", AppModelName: "up-m97",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		// A spec written while the column existed, with the lock ON. Raw SQL on
		// purpose: UpsertRuntimeSpec no longer names vram_locked, and the whole
		// point is a row the drop has to remove the column FROM.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, enabled, binary_path, pinned, vram_locked, ram_estimate_mb, created_at, updated_at)
			 values (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"spec_m97", "map_m97", 1, "/usr/bin/llama-server", 1, 1, 4096, now, now); err != nil {
			t.Fatalf("seed v96 spec: %v", err)
		}

		// The shape checks run at version 97 itself: a later migration may add
		// columns of its own (migration 100 adds five), so "exactly one column
		// dropped" only describes 97 while nothing after it has run.
		if err := s.migrateTo(ctx, 97); err != nil {
			t.Fatalf("migrate to 97: %v", err)
		}

		after := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		if _, ok := after["vram_locked"]; ok {
			t.Fatalf("agent_runtime_specs still has vram_locked after migration 97; got %v", sortedColumnNames(after))
		}
		// Sanity: the table is still the table (a drop-everything bug would
		// satisfy the check above vacuously), and migration 97 removed exactly
		// one column.
		for _, kept := range []string{"id", "mapping_id", "enabled", "pinned", "admin_state", "set_visible_devices", "ram_estimate_mb", "ram_measured_mb"} {
			if _, ok := after[kept]; !ok {
				t.Fatalf("agent_runtime_specs lost %q, which migration 97 must not touch; got %v", kept, sortedColumnNames(after))
			}
		}
		if len(after) != len(before)-1 {
			t.Fatalf("migration 97 changed the column count from %d to %d, want exactly one column dropped", len(before), len(after))
		}

		// The rest of the ledger, so the store's statements -- which speak the
		// head's column set -- apply to the row below.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m97")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if !spec.Enabled || !spec.Pinned || spec.Binary != "/usr/bin/llama-server" || spec.RAMEstimateMB != 4096 {
			t.Fatalf("pre-migration spec must read back with every surviving column untouched, got %+v", spec)
		}

		// The store's write path agrees with the post-97 schema, and a replay of
		// the migration (a re-run over a database that no longer has the column)
		// neither errors nor disturbs the stored row.
		spec.Binary = "/usr/bin/vllm"
		spec.UpdatedAt = now.Add(time.Minute)
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert after migration 97: %v", err)
		}
		reinvokeMigration97(ctx, t, s)
		again, _, err := s.RuntimeSpecByMapping(ctx, "map_m97")
		if err != nil {
			t.Fatalf("read after replay: %v", err)
		}
		if again.Binary != "/usr/bin/vllm" || !again.Enabled || !again.Pinned || again.RAMEstimateMB != 4096 {
			t.Fatalf("replaying migration 97 disturbed a stored value: %+v", again)
		}
	})
}

// TestMigration97FreshInstallHasNoVRAMLocked: a fresh install still creates the
// column in the frozen migration 65 and drops it again at 97, so the head schema
// of a fresh database and of an upgraded one are the same. The upgrade half is
// the test above; this pins the fresh half, including that the replay is a no-op
// (dropColumnIfPresent's contract) rather than an error.
func TestMigration97FreshInstallHasNoVRAMLocked(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		cols := tableColumns(ctx, t, s, "agent_runtime_specs")
		if slices.Contains(cols, "vram_locked") {
			t.Fatalf("a freshly migrated agent_runtime_specs still has vram_locked; columns = %v", cols)
		}
		reinvokeMigration97(ctx, t, s)
		reinvokeMigration97(ctx, t, s)
		if cols := tableColumns(ctx, t, s, "agent_runtime_specs"); slices.Contains(cols, "vram_locked") {
			t.Fatalf("agent_runtime_specs has vram_locked after replaying migration 97; columns = %v", cols)
		}
	})
}

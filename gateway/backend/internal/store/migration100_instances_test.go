// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration100 runs migration100Up in its own transaction, exactly as
// the ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration100(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration100Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration100Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// migration100Columns is every column migration 100 adds to agent_runtime_specs.
var migration100Columns = []string{
	"replicable", "min_instances", "max_instances", "instance_parallelism", "startup_measured_ms",
}

// migration100Precedents maps each of migration 100's integer columns to the
// existing agent_runtime_specs column it must be declared exactly like,
// compared within one dialect (the only comparison tableColumnDecls supports).
// replicable is an integer-boolean, so it mirrors pinned, the table's bool
// convention; the three counts mirror idle_timeout_seconds, a plain
// `integer not null default 0` setting.
var migration100Precedents = map[string]string{
	"replicable":           "pinned",
	"min_instances":        "idle_timeout_seconds",
	"max_instances":        "idle_timeout_seconds",
	"instance_parallelism": "idle_timeout_seconds",
}

// assertMigration100Columns pins the shape of the five instance columns on a
// migrated database. startup_measured_ms has no int64 sibling on the table, so
// its declaration is pinned literally: ADR-005's wide `bigint not null default
// 0`, because a millisecond wall time is a Go int64 and an `integer` is int4 on
// PostgreSQL. bigint reads back verbatim from both dialects (as migration 95's
// columns do), so one literal serves both.
func assertMigration100Columns(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	decls := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
	for col, precedent := range migration100Precedents {
		got, ok := decls[col]
		if !ok {
			t.Fatalf("agent_runtime_specs has no %s column after migration 100; got %v", col, sortedColumnNames(decls))
		}
		if want := decls[precedent]; got != want {
			t.Fatalf("agent_runtime_specs.%s is declared %q, want %q (declared like %s)", col, got, want, precedent)
		}
	}
	got, ok := decls["startup_measured_ms"]
	if !ok {
		t.Fatalf("agent_runtime_specs has no startup_measured_ms column after migration 100; got %v", sortedColumnNames(decls))
	}
	if want := "bigint not null default 0"; got != want {
		t.Fatalf("agent_runtime_specs.startup_measured_ms is declared %q, want %q (ADR-005 wide type, 0 = unknown)", got, want)
	}
}

// seedMigration100Spec writes the FK chain and a spec row that names none of
// migration 100's columns. The mapping and the spec are raw SQL on purpose:
// CreateMapping and UpsertRuntimeSpec speak the head's column set, which a
// database stopped at version 99 does not have yet. pinned and the idle timeout
// are seeded non-zero so the defaults below cannot pass by reading a
// precedent's value.
func seedMigration100Spec(ctx context.Context, t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_m100", Name: "m100", Provider: routing.ProviderVLLM,
		Endpoint: "http://srv_m100:8000",
		Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{
		ID: "app_m100", ServerID: "srv_m100", Type: routing.ProviderServerAgent, Port: 8100, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	seedBaselineMapping(ctx, t, s, routing.ModelMapping{
		ID: "map_m100", ApplicationID: "app_m100", GatewayModelName: "m100-model", AppModelName: "up-m100",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	})
	mustExec(ctx, t, s, `insert into agent_runtime_specs
		(id, mapping_id, binary_path, idle_timeout_seconds, pinned, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?)`,
		"spec_m100", "map_m100", "/usr/bin/llama-server", 300, 1, now, now)
}

// assertMigration100Defaults reads the seeded spec back twice -- raw, so the DDL
// default itself is what is checked, and through the store, so the readers map
// it -- and requires every new column at its default: not replicable, no
// minimum, no maximum, parallelism 0 (= 1) and nothing measured. That default
// is the whole upgrade story: a spec that never opted in is exactly what it was.
func assertMigration100Defaults(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	var replicable, minInstances, maxInstances, parallelism, startupMS int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select replicable, min_instances, max_instances,
		instance_parallelism, startup_measured_ms from agent_runtime_specs where id = ?`), "spec_m100").Scan(
		&replicable, &minInstances, &maxInstances, &parallelism, &startupMS); err != nil {
		t.Fatalf("read the seeded spec's instance columns: %v", err)
	}
	if replicable != 0 || minInstances != 0 || maxInstances != 0 || parallelism != 0 || startupMS != 0 {
		t.Fatalf("seeded spec reads replicable=%d min=%d max=%d parallelism=%d startup=%d, want all 0 (the DDL default)",
			replicable, minInstances, maxInstances, parallelism, startupMS)
	}
	spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m100")
	if err != nil || !ok {
		t.Fatalf("read the seeded spec through the store: ok=%v err=%v", ok, err)
	}
	if spec.Replicable || spec.MinInstances != 0 || spec.MaxInstances != 0 || spec.InstanceParallelism != 0 || spec.StartupMeasuredMS != 0 {
		t.Fatalf("seeded spec must read back as a spec that never opted in, got %+v", spec)
	}
	if spec.Binary != "/usr/bin/llama-server" || !spec.Pinned || spec.IdleTimeoutSeconds != 300 {
		t.Fatalf("seeded spec's existing columns must read back untouched, got %+v", spec)
	}
}

// TestMigration100AddsInstanceColumns pins migration 100 (#340) on a fresh
// install and on an upgrade of a genuine version-99 database that already holds
// a spec, on both dialects: the five instance columns exist on
// agent_runtime_specs, declared as their precedents and ADR-005 demand, and a
// spec written before (or without naming) them reads every one at its default.
// On the upgrade the columns are also absent before the migration, writable
// through the store after it, and a replay of the migration disturbs nothing.
func TestMigration100AddsInstanceColumns(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			assertMigration100Columns(ctx, t, s)
			seedMigration100Spec(ctx, t, s, now)
			assertMigration100Defaults(ctx, t, s)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 99, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			before := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
			for _, c := range migration100Columns {
				if _, ok := before[c]; ok {
					t.Fatalf("agent_runtime_specs already has %s at v99", c)
				}
			}
			seedMigration100Spec(ctx, t, s, now)

			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration100Columns(ctx, t, s)
			assertMigration100Defaults(ctx, t, s)

			// All five are writable through the store, and a replay of the
			// migration (a re-run over a database that has them) neither errors
			// nor disturbs a stored value.
			spec, _, err := s.RuntimeSpecByMapping(ctx, "map_m100")
			if err != nil {
				t.Fatalf("read spec: %v", err)
			}
			spec.Replicable, spec.MinInstances, spec.MaxInstances, spec.InstanceParallelism = true, 1, 4, 2
			if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
				t.Fatalf("upsert with the instance settings: %v", err)
			}
			if err := s.UpdateRuntimeSpecStartupMeasured(ctx, spec.ID, 45000); err != nil {
				t.Fatalf("UpdateRuntimeSpecStartupMeasured: %v", err)
			}
			reinvokeMigration100(ctx, t, s)
			again, _, err := s.RuntimeSpecByMapping(ctx, "map_m100")
			if err != nil {
				t.Fatalf("read after replay: %v", err)
			}
			if !again.Replicable || again.MinInstances != 1 || again.MaxInstances != 4 || again.InstanceParallelism != 2 || again.StartupMeasuredMS != 45000 {
				t.Fatalf("replaying migration 100 disturbed a stored value: %+v, want replicable 1/4/2 and 45000 ms", again)
			}
		})
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration96 runs migration96Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration96(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration96Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration96Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration96AddsRuntimeSpecRAMColumns runs migration 96 against a genuine
// version-95 database that already holds a spec row, and pins what an upgrade of
// a deployment with agent-managed specs depends on: the two RAM columns do not
// exist before the migration and exist after it on BOTH dialects, on the SPEC
// table (RAM is host-scoped, so it does not belong on the per-GPU table where
// the vram_* columns live), declared exactly like the per-GPU VRAM pair they
// mirror (integer, not null, default 0 -- and 0 is the "unknown" sentinel, so
// every pre-existing row reads back as "no estimate, nothing measured yet" and
// the upgrade changes no behaviour), and a replay of the migration changes
// nothing (it is duplicate-tolerant).
func TestMigration96AddsRuntimeSpecRAMColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 95, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

		cols := []string{"ram_estimate_mb", "ram_measured_mb"}
		before := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		for _, c := range cols {
			if _, ok := before[c]; ok {
				t.Fatalf("agent_runtime_specs already has %s at v95", c)
			}
		}

		// A spec written before the columns existed. Raw SQL on purpose: at v95
		// UpsertRuntimeSpec would name columns that are not there yet, and the
		// whole point is a row the new columns have to be added UNDER.
		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m96", Name: "m96", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m96:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m96", ServerID: "srv_m96", Type: routing.ProviderServerAgent, Port: 8096, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m96", ApplicationID: "app_m96", GatewayModelName: "m96-model", AppModelName: "up-m96",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at) values (?, ?, ?, ?, ?)`),
			"spec_m96", "map_m96", "/usr/bin/llama-server", now, now); err != nil {
			t.Fatalf("seed v95 spec: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		gpuDecls := tableColumnDecls(ctx, t, s, "agent_runtime_spec_gpus")
		for c, precedent := range map[string]string{
			"ram_estimate_mb": "vram_estimate_mb",
			"ram_measured_mb": "vram_measured_mb",
		} {
			got, ok := decls[c]
			if !ok {
				t.Fatalf("agent_runtime_specs has no %s column after migration 96; got %v", c, sortedColumnNames(decls))
			}
			if want := gpuDecls[precedent]; got != want {
				t.Fatalf("agent_runtime_specs.%s is declared %q, want %q (the per-GPU %s it mirrors)", c, got, want, precedent)
			}
			if _, onGPU := gpuDecls[c]; onGPU {
				t.Fatalf("agent_runtime_spec_gpus has a %s column: RAM is host-scoped and lives on the spec row only", c)
			}
		}

		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m96")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if spec.RAMEstimateMB != 0 || spec.RAMMeasuredMB != 0 || spec.Binary != "/usr/bin/llama-server" {
			t.Fatalf("pre-migration spec must read back untouched with both RAM columns 0 (unknown), got %+v", spec)
		}

		// Both columns are writable through the store, and a replay of the
		// migration (a re-run over a database that has them) neither errors nor
		// disturbs a stored value.
		spec.RAMEstimateMB = 4096
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert with ram_estimate_mb: %v", err)
		}
		if err := s.UpdateRuntimeSpecRAMMeasured(ctx, spec.ID, 5000); err != nil {
			t.Fatalf("UpdateRuntimeSpecRAMMeasured: %v", err)
		}
		reinvokeMigration96(ctx, t, s)
		again, _, err := s.RuntimeSpecByMapping(ctx, "map_m96")
		if err != nil {
			t.Fatalf("read after replay: %v", err)
		}
		if again.RAMEstimateMB != 4096 || again.RAMMeasuredMB != 5000 {
			t.Fatalf("replaying migration 96 disturbed a stored value: estimate %d measured %d, want 4096/5000", again.RAMEstimateMB, again.RAMMeasuredMB)
		}
	})
}

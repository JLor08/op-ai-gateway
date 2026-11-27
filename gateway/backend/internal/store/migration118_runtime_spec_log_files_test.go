// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration118 runs migration118Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration118(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration118Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration118Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration118AddsRuntimeSpecLogFiles runs migration 118 against a genuine
// version-117 database that already holds a spec row, and pins what an upgrade
// of a deployment with agent-managed specs depends on (#430): the column does
// not exist before the migration, exists after it on BOTH dialects, is declared
// exactly like the table's other free-text column (binary_path: text, not
// null, empty-string default -- the ADR-005 wide type for a string value, and a
// default that hands every pre-existing row "" = no log files, not NULL), the
// pre-existing row reads back "" through the store's own readers, and a replay
// of the migration changes nothing (it is duplicate-tolerant).
func TestMigration118AddsRuntimeSpecLogFiles(t *testing.T) {
	forEachDialectMigratedTo(t, 117, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

		if _, ok := tableColumnDecls(ctx, t, s, "agent_runtime_specs")["log_files"]; ok {
			t.Fatalf("agent_runtime_specs already has log_files at v117")
		}

		// A spec written before the column existed. Raw SQL on purpose: at v117
		// UpsertRuntimeSpec would name the column that is not there yet, and
		// the whole point is a row the new column has to be added UNDER.
		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m118", Name: "m118", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m118:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineApplication(ctx, t, s, routing.Application{
			ID: "app_m118", ServerID: "srv_m118", Type: routing.ProviderServerAgent, Port: 8118, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m118", ApplicationID: "app_m118", GatewayModelName: "m118-model", AppModelName: "up-m118",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at) values (?, ?, ?, ?, ?)`),
			"spec_m118", "map_m118", "/usr/bin/llama-server", now, now); err != nil {
			t.Fatalf("seed v117 spec: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		got, ok := decls["log_files"]
		if !ok {
			t.Fatalf("agent_runtime_specs has no log_files column after migration 118; got %v", sortedColumnNames(decls))
		}
		if want := decls["binary_path"]; got != want {
			t.Fatalf("agent_runtime_specs.log_files is declared %q, want %q (the precedent free-text column on this table)", got, want)
		}

		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m118")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if spec.LogFiles != "" || spec.Binary != "/usr/bin/llama-server" {
			t.Fatalf("pre-migration spec must read back untouched with empty log_files, got %+v", spec)
		}

		// The column is writable through the store and a replay of the
		// migration (a re-run over a database that has it) neither errors nor
		// disturbs a value already stored.
		spec.LogFiles = `[{"name":"engine","path":"${CONFIG_DIR}/e.log"}]`
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert with log_files: %v", err)
		}
		reinvokeMigration118(ctx, t, s)
		again, _, err := s.RuntimeSpecByMapping(ctx, "map_m118")
		if err != nil {
			t.Fatalf("read after replay: %v", err)
		}
		if again.LogFiles != spec.LogFiles {
			t.Fatalf("replaying migration 118 disturbed a stored value: got %q, want %q", again.LogFiles, spec.LogFiles)
		}
	})
}

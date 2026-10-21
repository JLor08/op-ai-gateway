// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration93 runs migration93Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration93(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration93Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration93Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration93AddsRuntimeSpecConfigFiles runs migration 93 against a genuine
// version-92 database that already holds a spec row, and pins what an upgrade
// of a deployment with agent-managed specs depends on (#257): the column does
// not exist before the migration, exists after it on BOTH dialects, is declared
// exactly like the table's other free-text column (binary_path: text, not
// null, empty-string default -- the ADR-005 wide type for a string value, and a
// default that hands every pre-existing row "" = no config files, not NULL), the
// pre-existing row reads back "" through the store's own readers, and a replay
// of the migration changes nothing (it is duplicate-tolerant).
func TestMigration93AddsRuntimeSpecConfigFiles(t *testing.T) {
	forEachDialectMigratedTo(t, 92, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

		if _, ok := tableColumnDecls(ctx, t, s, "agent_runtime_specs")["config_files"]; ok {
			t.Fatalf("agent_runtime_specs already has config_files at v92")
		}

		// A spec written before the column existed. Raw SQL on purpose: at v92
		// UpsertRuntimeSpec would name the column that is not there yet, and
		// the whole point is a row the new column has to be added UNDER.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m93", Name: "m93", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m93:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m93", ServerID: "srv_m93", Type: routing.ProviderServerAgent, Port: 8093, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_m93", ApplicationID: "app_m93", GatewayModelName: "m93-model", AppModelName: "up-m93",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at) values (?, ?, ?, ?, ?)`),
			"spec_m93", "map_m93", "/usr/bin/llama-server", now, now); err != nil {
			t.Fatalf("seed v92 spec: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		got, ok := decls["config_files"]
		if !ok {
			t.Fatalf("agent_runtime_specs has no config_files column after migration 93; got %v", sortedColumnNames(decls))
		}
		if want := decls["binary_path"]; got != want {
			t.Fatalf("agent_runtime_specs.config_files is declared %q, want %q (the precedent free-text column on this table)", got, want)
		}

		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m93")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if spec.ConfigFiles != "" || spec.Binary != "/usr/bin/llama-server" {
			t.Fatalf("pre-migration spec must read back untouched with empty config_files, got %+v", spec)
		}

		// The column is writable through the store and a replay of the
		// migration (a re-run over a database that has it) neither errors nor
		// disturbs a value already stored.
		spec.ConfigFiles = `[{"name":"server.json","content":"x"}]`
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert with config_files: %v", err)
		}
		reinvokeMigration93(ctx, t, s)
		again, _, err := s.RuntimeSpecByMapping(ctx, "map_m93")
		if err != nil {
			t.Fatalf("read after replay: %v", err)
		}
		if again.ConfigFiles != spec.ConfigFiles {
			t.Fatalf("replaying migration 93 disturbed a stored value: got %q, want %q", again.ConfigFiles, spec.ConfigFiles)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// TestMigration113AddsRuntimeSpecProcessTreeColumn runs migration 113 over a
// genuine version-112 database holding a spec row and pins what an upgrade
// depends on: the column is absent before and present after on every dialect,
// declared with the default that makes every existing row read back as a
// direct spec (so the upgrade changes no behaviour), a job value round-trips,
// and a replay is a no-op.
func TestMigration113AddsRuntimeSpecProcessTreeColumn(t *testing.T) {
	forEachDialectMigratedTo(t, 112, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
		const want = "text not null default 'direct'"
		if _, ok := tableColumnDecls(ctx, t, s, "agent_runtime_specs")["process_tree"]; ok {
			t.Fatalf("agent_runtime_specs already has process_tree at v112")
		}

		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m113", Name: "m113", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m113:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineApplication(ctx, t, s, routing.Application{
			ID: "app_m113", ServerID: "srv_m113", Type: routing.ProviderServerAgent, Port: 8113, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m113", ApplicationID: "app_m113", GatewayModelName: "m113-model", AppModelName: "up-m113",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at) values (?, ?, ?, ?, ?)`),
			"spec_m113", "map_m113", `C:\strata\bin\strata.cmd`, now, now); err != nil {
			t.Fatalf("seed v112 spec: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		got, ok := tableColumnDecls(ctx, t, s, "agent_runtime_specs")["process_tree"]
		if !ok || !strings.EqualFold(strings.ReplaceAll(got, "::text", ""), want) {
			t.Fatalf("agent_runtime_specs.process_tree = %q (present %v), want %q", got, ok, want)
		}
		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m113")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if spec.ProcessTree != routing.ProcessTreeDirect {
			t.Fatalf("pre-migration spec must read process_tree direct, got %q", spec.ProcessTree)
		}

		spec.ProcessTree = routing.ProcessTreeJob
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration113Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration113Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		again, _, _ := s.RuntimeSpecByMapping(ctx, "map_m113")
		if again.ProcessTree != routing.ProcessTreeJob {
			t.Fatalf("replay disturbed the stored value: %q", again.ProcessTree)
		}

		// A spec built outside the portal leaves the field unset; it stores
		// the column default, never an empty string no reader treats as a mode.
		spec.ProcessTree = ""
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert unset: %v", err)
		}
		if unset, _, _ := s.RuntimeSpecByMapping(ctx, "map_m113"); unset.ProcessTree != routing.ProcessTreeDirect {
			t.Fatalf("an unset process_tree stored %q, want direct", unset.ProcessTree)
		}
	})
}

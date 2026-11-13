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

// TestMigration109AddsRuntimeSpecExecutionColumns runs migration 109 over a
// genuine version-108 database holding a spec row and pins what an upgrade
// depends on: both columns are absent before and present after on every
// dialect, declared with the defaults that make every existing row read back
// as a native spec with no distro (so the upgrade changes no behaviour), and a
// replay is a no-op.
func TestMigration109AddsRuntimeSpecExecutionColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 108, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		want := map[string]string{
			"execution_mode": "text not null default 'native'",
			"wsl_distro":     "text not null default ''",
		}
		before := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		for c := range want {
			if _, ok := before[c]; ok {
				t.Fatalf("agent_runtime_specs already has %s at v108", c)
			}
		}

		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m109", Name: "m109", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m109:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineApplication(ctx, t, s, routing.Application{
			ID: "app_m109", ServerID: "srv_m109", Type: routing.ProviderServerAgent, Port: 8109, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m109", ApplicationID: "app_m109", GatewayModelName: "m109-model", AppModelName: "up-m109",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at) values (?, ?, ?, ?, ?)`),
			"spec_m109", "map_m109", "/usr/bin/llama-server", now, now); err != nil {
			t.Fatalf("seed v108 spec: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		decls := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		for c, w := range want {
			if got, ok := decls[c]; !ok || !strings.EqualFold(strings.ReplaceAll(got, "::text", ""), w) {
				t.Fatalf("agent_runtime_specs.%s = %q (present %v), want %q", c, got, ok, w)
			}
		}
		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m109")
		if err != nil || !ok {
			t.Fatalf("read pre-migration spec: ok=%v err=%v", ok, err)
		}
		if spec.ExecutionMode != routing.ExecutionModeNative || spec.WSLDistro != "" {
			t.Fatalf("pre-migration spec must read native/\"\", got %q/%q", spec.ExecutionMode, spec.WSLDistro)
		}

		spec.ExecutionMode, spec.WSLDistro = routing.ExecutionModeWSL, "Ubuntu-24.04"
		if err := s.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration109Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration109Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		again, _, _ := s.RuntimeSpecByMapping(ctx, "map_m109")
		if again.ExecutionMode != routing.ExecutionModeWSL || again.WSLDistro != "Ubuntu-24.04" {
			t.Fatalf("replay disturbed stored values: %q/%q", again.ExecutionMode, again.WSLDistro)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func readModes(ctx context.Context, t *testing.T, s *SQLStore, table, id string) (decisions, systemOne string) {
	t.Helper()
	row := s.db.QueryRowContext(ctx, s.dl.rebind("select decisions_mode, systemone_mode from "+table+" where id = ?"), id)
	if err := row.Scan(&decisions, &systemOne); err != nil {
		t.Fatalf("read %s modes: %v", table, err)
	}
	return decisions, systemOne
}

// TestMigration107DecisionModes upgrades a genuine version-106 database that
// already holds an application and a runtime spec: both gain decisions_mode
// 'auto' and systemone_mode 'passthrough', a replay disturbs nothing (not even
// an operator-set value), and an empty value read back by the store is the
// default.
func TestMigration107DecisionModes(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	forEachDialectMigratedTo(t, 106, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m107", Name: "m107", Provider: routing.ProviderVLLM, Endpoint: "http://srv_m107:8000",
			Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineApplication(ctx, t, s, routing.Application{
			ID: "app_m107", ServerID: "srv_m107", Type: routing.ProviderServerAgent, Port: 8107, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		})
		seedBaselineMapping(ctx, t, s, routing.ModelMapping{
			ID: "map_m107", ApplicationID: "app_m107", GatewayModelName: "m107", AppModelName: "u-m107",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		})
		mustExec(ctx, t, s, `insert into agent_runtime_specs (id, mapping_id, binary_path, created_at, updated_at)
			values (?, ?, ?, ?, ?)`, "spec_m107", "map_m107", "/usr/bin/llama-server", now, now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		for _, table := range []struct{ name, id string }{{"applications", "app_m107"}, {"agent_runtime_specs", "spec_m107"}} {
			if d, so := readModes(ctx, t, s, table.name, table.id); d != "auto" || so != "passthrough" {
				t.Fatalf("%s after upgrade: decisions_mode=%q systemone_mode=%q, want auto/passthrough", table.name, d, so)
			}
		}

		// An operator's value survives a replay.
		mustExec(ctx, t, s, "update applications set decisions_mode = 'disabled' where id = ?", "app_m107")
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration107Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration107Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if d, so := readModes(ctx, t, s, "applications", "app_m107"); d != "disabled" || so != "passthrough" {
			t.Fatalf("after replay: decisions_mode=%q systemone_mode=%q, want disabled/passthrough", d, so)
		}

		// A row an older binary wrote (blank modes) reads as the defaults.
		mustExec(ctx, t, s, "update applications set decisions_mode = '', systemone_mode = '' where id = ?", "app_m107")
		mustExec(ctx, t, s, "update agent_runtime_specs set decisions_mode = '', systemone_mode = '' where id = ?", "spec_m107")
		app, err := s.ApplicationByID(ctx, "app_m107")
		if err != nil || app.DecisionsMode != routing.EndpointModeAuto || app.SystemOneMode != routing.EndpointModePassthrough {
			t.Fatalf("blank application read = %q/%q err=%v, want auto/passthrough", app.DecisionsMode, app.SystemOneMode, err)
		}
		spec, ok, err := s.RuntimeSpecByMapping(ctx, "map_m107")
		if err != nil || !ok || spec.DecisionsMode != routing.EndpointModeAuto || spec.SystemOneMode != routing.EndpointModePassthrough {
			t.Fatalf("blank spec read = %q/%q ok=%v err=%v, want auto/passthrough", spec.DecisionsMode, spec.SystemOneMode, ok, err)
		}
	})
}

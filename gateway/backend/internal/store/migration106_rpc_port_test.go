// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestMigration106MakesMemberPortsUnique pins migration 106 on an upgrade of
// a genuine version-105 database that already holds membership rows: they
// survive, a second head can no longer take an allocated port on the same
// server, and a replay disturbs nothing.
func TestMigration106MakesMemberPortsUnique(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	forEachDialectMigratedTo(t, 105, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		head := seedRPCHeadSpecAtV105(ctx, t, s, "m1", now)
		head2 := seedRPCHeadSpecAtV105(ctx, t, s, "m2", now)
		seedBinariesServer(ctx, t, s, "srv_mig", now)
		row := func(h string, port int) []routing.ClusterMember {
			return []routing.ClusterMember{{HeadSpecID: h, Position: 0, ServerID: "srv_mig", DeviceBackend: "auto", Port: port}}
		}
		if err := s.SetClusterMembers(ctx, head, row(head, 47001), nil); err != nil {
			t.Fatalf("seed at v105: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		if got, _, err := s.ClusterMembers(ctx, head); err != nil || len(got) != 1 || got[0].Port != 47001 {
			t.Fatalf("rows after upgrade = %+v err=%v", got, err)
		}
		if err := s.SetClusterMembers(ctx, head2, row(head2, 47001), nil); !errors.Is(err, ErrConflict) {
			t.Fatalf("duplicate port after upgrade: err=%v, want ErrConflict", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration106Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration106Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.SetClusterMembers(ctx, head2, row(head2, 47002), nil); err != nil {
			t.Fatalf("free port after replay: %v", err)
		}
	})
}

// seedRPCHeadSpecAtV105 is seedRPCHeadSpec for a database still at version 105:
// the store's own writers name columns migration 107 added, so the chain is
// inserted with the baseline columns only.
func seedRPCHeadSpecAtV105(ctx context.Context, t *testing.T, s *SQLStore, key string, now time.Time) string {
	t.Helper()
	seedBinariesServer(ctx, t, s, "srv_head_"+key, now)
	seedBaselineApplication(ctx, t, s, routing.Application{
		ID: "app_" + key, ServerID: "srv_head_" + key, Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	})
	seedBaselineMapping(ctx, t, s, routing.ModelMapping{
		ID: "map_" + key, ApplicationID: "app_" + key, GatewayModelName: "m-" + key, AppModelName: "u-" + key,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	})
	mustExec(ctx, t, s, `insert into agent_runtime_specs (id, mapping_id, enabled, binary_path, args, env, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?)`,
		"rspec_"+key, "map_"+key, 1, "/usr/bin/llama-server", "[]", "{}", now, now)
	return "rspec_" + key
}

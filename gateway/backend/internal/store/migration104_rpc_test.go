// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

func rpcSpecFixture(id, serverID string, now time.Time) routing.RPCMemberSpec {
	return routing.RPCMemberSpec{
		ID: id, ServerID: serverID, Enabled: true, Binary: "/opt/rpc-server", Args: "-t 8", Env: "{}",
		DeviceBackend: "auto", AllowedHeadGroups: "[]", CreatedAt: now, UpdatedAt: now,
	}
}

// reinvokeMigration104 runs migration104Up in its own transaction, as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration104(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration104Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration104Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

var migration104Tables = map[string][]string{
	"rpc_member_specs": {
		"id", "server_id", "enabled", "binary_path", "args", "env", "cache_enabled", "device_backend",
		"ram_estimate_mb", "allowed_head_groups", "created_at", "updated_at",
	},
	"rpc_member_spec_gpus": {"spec_id", "gpu_index", "position", "vram_estimate_mb"},
	"rpc_cluster_members": {
		"head_spec_id", "position", "server_id", "member_spec_id", "threads", "cache_enabled", "device_backend",
		"ram_estimate_mb", "port", "ram_measured_mb", "measured_at", "stale", "run_peak_ram_mb",
		"run_head_instance", "run_started_at",
	},
	"rpc_cluster_member_gpus": {
		"head_spec_id", "server_id", "gpu_index", "position", "vram_estimate_mb", "vram_measured_mb", "run_peak_vram_mb",
	},
	"rpc_sessions": {"head_spec_id", "instance", "member_server_id", "since", "until"},
}

func assertMigration104Tables(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	for table, cols := range migration104Tables {
		decls := tableColumnDecls(ctx, t, s, table)
		for _, col := range cols {
			if _, ok := decls[col]; !ok {
				t.Fatalf("%s has no %s column after migration 104; got %v", table, col, sortedColumnNames(decls))
			}
		}
		if len(decls) != len(cols) {
			t.Fatalf("%s has columns %v, want exactly %v", table, sortedColumnNames(decls), cols)
		}
	}
}

// TestMigration104AddsRPCClusterTables pins migration 104 on a fresh install
// and on an upgrade of a genuine version-103 database that already holds a
// server: the five tables exist with their columns, the store can write and
// read them, and a replay disturbs nothing.
func TestMigration104AddsRPCClusterTables(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			assertMigration104Tables(context.Background(), t, s)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 103, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			for table := range migration104Tables {
				var n int
				if err := s.db.QueryRowContext(ctx, "select count(*) from "+table).Scan(&n); err == nil {
					t.Fatalf("%s already exists at v103", table)
				}
			}
			seedMigration103Server(ctx, t, s, now)
			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration104Tables(ctx, t, s)

			spec := rpcSpecFixture("rpcm_up", "srv_m103", now)
			if err := s.UpsertRPCMemberSpec(ctx, spec); err != nil {
				t.Fatalf("write member spec: %v", err)
			}
			reinvokeMigration104(ctx, t, s)
			got, ok, err := s.RPCMemberSpecByID(ctx, "rpcm_up")
			if err != nil || !ok || got.Binary != spec.Binary {
				t.Fatalf("replaying migration 104 disturbed a member spec: %+v ok=%v err=%v", got, ok, err)
			}
			if _, ok, _ := s.RPCMemberSpecByID(ctx, "rpcm_up"); !ok {
				t.Fatal("member spec lost on replay")
			}
		})
	})
}

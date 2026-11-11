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

// reinvokeMigration103 runs migration103Up in its own transaction, exactly as
// the ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration103(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration103Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration103Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// migration103Columns maps every column migration 103 adds to the declaration
// it must carry, per table. The integer-booleans and the text settings use the
// same literals both dialects report back ("integer"/"text"), so one literal
// serves SQLite and PostgreSQL.
var migration103Columns = map[string]map[string]string{
	"agent_runtime_specs": {
		"cluster_enabled":  "integer not null default 0",
		"member_mode":      "text not null default 'automatic'",
		"cluster_network":  "text not null default ''",
		"rpc_transport":    "text not null default ''",
		"lan_exposure_ack": "integer not null default 0",
		"device_backend":   "text not null default 'auto'",
	},
	"ai_servers": {
		"netbird_ip":              "text not null default ''",
		"lan_address":             "text not null default ''",
		"lan_bind_only":           "integer not null default 0",
		"rpc_member_enabled":      "integer not null default 0",
		"rpc_allowed_head_groups": "text not null default '[]'",
	},
}

func assertMigration103Columns(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	for table, cols := range migration103Columns {
		decls := tableColumnDecls(ctx, t, s, table)
		for col, want := range cols {
			got, ok := decls[col]
			if !ok {
				t.Fatalf("%s has no %s column after migration 103; got %v", table, col, sortedColumnNames(decls))
			}
			if norm := normalizeDeclDefault(got); norm != want {
				t.Fatalf("%s.%s is declared %q, want %q", table, col, got, want)
			}
		}
	}
	decls := tableColumnDecls(ctx, t, s, "server_binaries")
	for _, col := range []string{
		"server_id", "path", "kind", "version", "version_override", "build", "commit",
		"version_source", "source", "allowed", "reported_at",
	} {
		if _, ok := decls[col]; !ok {
			t.Fatalf("server_binaries has no %s column; got %v", col, sortedColumnNames(decls))
		}
	}
}

// normalizeDeclDefault maps each dialect's reported declaration onto the
// literals above: SQLite upper-cases the type, PostgreSQL renders a text
// default as `'x'::text`.
func normalizeDeclDefault(decl string) string {
	return strings.ReplaceAll(strings.ToLower(decl), "::text", "")
}

func seedMigration103Server(ctx context.Context, t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	mustExec(ctx, t, s, `insert into ai_servers (id, name, provider, endpoint, status, health_status, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?)`,
		"srv_m103", "m103", routing.ProviderVLLM, "http://srv_m103:8000", routing.ServerStatusActive, routing.HealthHealthy, now, now)
}

func assertMigration103ServerDefaults(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	srv, err := s.AIServerByID(ctx, "srv_m103")
	if err != nil {
		t.Fatalf("read seeded server: %v", err)
	}
	if srv.NetbirdIP != "" || srv.LANAddress != "" || srv.LANBindOnly || srv.RPCMemberEnabled || srv.RPCAllowedHeadGroups != "[]" {
		t.Fatalf("seeded server must read the RPC defaults (none, none, false, false, []), got %+v", srv)
	}
}

// TestMigration103AddsRPCClusterColumns pins migration 103 on a fresh install
// and on an upgrade of a genuine version-102 database that already holds a
// server: the spec and server columns exist with their declared defaults, the
// server_binaries table exists, a pre-existing server reads the defaults, the
// new columns are writable through the store, and a replay disturbs nothing.
func TestMigration103AddsRPCClusterColumns(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			assertMigration103Columns(ctx, t, s)
			seedMigration103Server(ctx, t, s, now)
			assertMigration103ServerDefaults(ctx, t, s)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 102, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			for table, cols := range migration103Columns {
				before := tableColumnDecls(ctx, t, s, table)
				for col := range cols {
					if _, ok := before[col]; ok {
						t.Fatalf("%s already has %s at v102", table, col)
					}
				}
			}
			seedMigration103Server(ctx, t, s, now)
			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration103Columns(ctx, t, s)
			assertMigration103ServerDefaults(ctx, t, s)

			srv, err := s.AIServerByID(ctx, "srv_m103")
			if err != nil {
				t.Fatalf("read server: %v", err)
			}
			srv.NetbirdIP, srv.LANAddress, srv.LANBindOnly, srv.RPCMemberEnabled = "100.64.0.9", "192.168.1.9", true, true
			srv.RPCAllowedHeadGroups = `["ugrp_a"]`
			if err := s.UpdateAIServer(ctx, srv); err != nil {
				t.Fatalf("update server: %v", err)
			}
			if err := s.UpdateServerNetbirdState(ctx, srv.ID, srv.Domain, srv.NetbirdPeerID, srv.NetbirdIP, srv.NetbirdConnected); err != nil {
				t.Fatalf("update server mesh ip: %v", err)
			}
			if err := s.ReplaceAgentServerBinaries(ctx, "srv_m103", []routing.ServerBinary{
				{Path: "/opt/llama-server", Kind: "llama-server", Version: "b1", Source: "agent", ReportedAt: now},
			}); err != nil {
				t.Fatalf("write binaries: %v", err)
			}
			reinvokeMigration103(ctx, t, s)
			again, err := s.AIServerByID(ctx, "srv_m103")
			if err != nil {
				t.Fatalf("read after replay: %v", err)
			}
			if again.NetbirdIP != "100.64.0.9" || again.LANAddress != "192.168.1.9" || !again.LANBindOnly ||
				!again.RPCMemberEnabled || again.RPCAllowedHeadGroups != `["ugrp_a"]` {
				t.Fatalf("replaying migration 103 disturbed a stored value: %+v", again)
			}
			bins, err := s.ServerBinaries(ctx, "srv_m103")
			if err != nil || len(bins) != 1 {
				t.Fatalf("replaying migration 103 disturbed server_binaries: err=%v rows=%v", err, bins)
			}
		})
	})
}

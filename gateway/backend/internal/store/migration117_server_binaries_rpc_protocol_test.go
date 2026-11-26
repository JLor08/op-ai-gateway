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

// TestMigration117AddsServerBinariesRPCProtocol runs migration 117 over a
// genuine version-116 database holding an inventory row and pins what an
// upgrade depends on: the column is absent before and present after on every
// dialect with the empty-string default (every existing row reads back as
// "protocol unknown"), a reported protocol round-trips, and a replay is a
// no-op.
func TestMigration117AddsServerBinariesRPCProtocol(t *testing.T) {
	forEachDialectMigratedTo(t, 116, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		const want = "text not null default ''"
		if _, ok := tableColumnDecls(ctx, t, s, "server_binaries")["rpc_protocol"]; ok {
			t.Fatalf("server_binaries already has rpc_protocol at v116")
		}

		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m117", Name: "m117", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m117:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`
			insert into server_binaries (server_id, path, kind, version, build, "commit", version_source, source, allowed, reported_at)
			values (?, ?, ?, ?, ?, ?, ?, 'agent', ?, ?)`),
			"srv_m117", "/a/rpc-server", "rpc_server", "b1", "1", "abc", "agent", 1, now); err != nil {
			t.Fatalf("seed v116 binary: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		got, ok := tableColumnDecls(ctx, t, s, "server_binaries")["rpc_protocol"]
		if !ok || !strings.EqualFold(strings.ReplaceAll(got, "::text", ""), want) {
			t.Fatalf("server_binaries.rpc_protocol = %q (present %v), want %q", got, ok, want)
		}
		rows, err := s.ServerBinaries(ctx, "srv_m117")
		if err != nil || len(rows) != 1 {
			t.Fatalf("read pre-migration binary: rows=%v err=%v", rows, err)
		}
		if rows[0].RPCProtocol != "" {
			t.Fatalf("pre-migration row must read an empty protocol, got %q", rows[0].RPCProtocol)
		}

		rows[0].RPCProtocol = "10.0.2"
		if err := s.ReplaceAgentServerBinaries(ctx, "srv_m117", rows); err != nil {
			t.Fatalf("replace: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration117Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration117Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		again, _ := s.ServerBinaries(ctx, "srv_m117")
		if len(again) != 1 || again[0].RPCProtocol != "10.0.2" {
			t.Fatalf("replay disturbed the stored value: %+v", again)
		}
	})
}

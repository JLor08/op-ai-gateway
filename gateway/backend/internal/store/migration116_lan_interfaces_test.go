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

// TestMigration116AddsLANInterfaceColumns runs migration 116 over a genuine
// version-115 database and pins what an upgrade depends on: the three columns
// are absent before and present after with the declared defaults, a server that
// already has a lan_address resolves "manual", one without resolves "auto", and
// no server has reported interfaces yet (” = never reported).
func TestMigration116AddsLANInterfaceColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 115, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
		want := map[string]string{
			"lan_interfaces":   "text not null default ''",
			"lan_source":       "text not null default ''",
			"lan_interface_id": "text not null default ''",
		}
		before := tableColumnDecls(ctx, t, s, "ai_servers")
		for c := range want {
			if _, ok := before[c]; ok {
				t.Fatalf("ai_servers already has %s at v115", c)
			}
		}
		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m116_lan", Name: "m116-lan", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m116_lan:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})
		mustExec(ctx, t, s, `update ai_servers set lan_address = ? where id = ?`, "192.0.2.20", "srv_m116_lan")
		seedBaselineServer(ctx, t, s, routing.AIServer{
			ID: "srv_m116_none", Name: "m116-none", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m116_none:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		})

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}
		decls := tableColumnDecls(ctx, t, s, "ai_servers")
		for c, w := range want {
			if got, ok := decls[c]; !ok || !strings.EqualFold(strings.ReplaceAll(got, "::text", ""), w) {
				t.Fatalf("ai_servers.%s = %q (present %v), want %q", c, got, ok, w)
			}
		}
		for id, wantSource := range map[string]string{
			"srv_m116_lan": routing.LANSourceManual, "srv_m116_none": routing.LANSourceAuto,
		} {
			got, err := s.AIServerByID(ctx, id)
			if err != nil {
				t.Fatalf("read %s: %v", id, err)
			}
			if got.LANSource != wantSource || got.LANInterfaceID != "" || got.LANInterfaces != "" {
				t.Fatalf("%s source/interface/interfaces = %q/%q/%q, want %q/\"\"/\"\"",
					id, got.LANSource, got.LANInterfaceID, got.LANInterfaces, wantSource)
			}
		}

		// A replay is a no-op and must not rewrite an operator's later choice.
		if err := s.UpdateServerLANInterfaces(ctx, "srv_m116_none", `[]`); err != nil {
			t.Fatalf("report empty: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration116Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit replay: %v", err)
		}
		got, err := s.AIServerByID(ctx, "srv_m116_none")
		if err != nil {
			t.Fatal(err)
		}
		if got.LANInterfaces != "[]" || got.LANSource != routing.LANSourceAuto {
			t.Fatalf("replay changed data: %+v", got)
		}
	})
}

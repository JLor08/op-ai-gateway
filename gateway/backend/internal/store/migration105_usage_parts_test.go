// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

var migration105Tables = map[string][]string{
	"usage_event_hosts":        {"event_id", "server_id", "role", "created_at"},
	"usage_event_energy_parts": {"event_id", "server_id", "role", "wh", "marginal_wh", "source"},
}

func assertMigration105Tables(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	for table, cols := range migration105Tables {
		decls := tableColumnDecls(ctx, t, s, table)
		for _, col := range cols {
			if _, ok := decls[col]; !ok {
				t.Fatalf("%s has no %s column after migration 105; got %v", table, col, sortedColumnNames(decls))
			}
		}
		if len(decls) != len(cols) {
			t.Fatalf("%s has columns %v, want exactly %v", table, sortedColumnNames(decls), cols)
		}
	}
}

// TestMigration105AddsUsageHostTables pins migration 105 on a fresh install
// and on an upgrade of a genuine version-104 database that already holds a
// usage event: the tables exist, the store writes and reads them, and a
// replay disturbs nothing.
func TestMigration105AddsUsageHostTables(t *testing.T) {
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			assertMigration105Tables(context.Background(), t, s)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 104, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			for table := range migration105Tables {
				var n int
				if err := s.db.QueryRowContext(ctx, "select count(*) from "+table).Scan(&n); err == nil {
					t.Fatalf("%s already exists at v104", table)
				}
			}
			seedUsageEventForParts(t, s, "ev_up", "srv_up", 100, at)
			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration105Tables(ctx, t, s)

			host := usage.EventHost{EventID: "ev_up", ServerID: "srv_up", Role: "head", CreatedAt: at}
			if err := s.RecordEventHosts(ctx, []usage.EventHost{host}); err != nil {
				t.Fatalf("record host: %v", err)
			}
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			if err := migration105Up(ctx, tx, s.dl); err != nil {
				_ = tx.Rollback()
				t.Fatalf("migration105Up replay: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
			got, err := s.EventHosts(ctx, "ev_up")
			if err != nil || len(got) != 1 || got[0].ServerID != "srv_up" {
				t.Fatalf("replay disturbed event hosts: %+v err=%v", got, err)
			}
		})
	})
}

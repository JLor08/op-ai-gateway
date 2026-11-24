// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestMigration114CreatesVendorAccountGroupTables runs migration 114 against a
// genuine version-113 database holding one usage row and pins the upgrade: both
// group tables exist, the pre-upgrade usage row reads vendor_group_id empty, and
// replaying the migration changes nothing.
func TestMigration114CreatesVendorAccountGroupTables(t *testing.T) {
	forEachDialectMigratedTo(t, 113, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if cols := tableColumns(ctx, t, s, "usage_events"); slices.Contains(cols, "vendor_group_id") {
			t.Fatalf("usage_events already has vendor_group_id at v113: %v", cols)
		}
		if _, err := s.exec(ctx, `insert into usage_events
			(id, request_id, user_id, token_id, session_id, api_flavor, model, provider, host, status, error_code, http_status,
			 input_tokens, output_tokens, total_tokens, latency_ms, created_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"ue_pre114", "ue_pre114", "u1", "t1", "", "", "m1", "p1", "h1", "success", "", 200,
			1, 2, 3, 4, time.Now().UTC()); err != nil {
			t.Fatalf("seed pre-v114 usage row: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		groupCols := tableColumns(ctx, t, s, "vendor_account_groups")
		for _, want := range []string{
			"id", "owner_user_id", "vendor", "name", "model_prefix", "status",
			"threshold_five_hour_pct", "threshold_weekly_pct", "threshold_credits_pct", "created_at", "updated_at",
		} {
			if !slices.Contains(groupCols, want) {
				t.Errorf("vendor_account_groups lacks %q: %v", want, groupCols)
			}
		}
		memberCols := tableColumns(ctx, t, s, "vendor_account_group_members")
		for _, want := range []string{"group_id", "vendor_account_id", "position"} {
			if !slices.Contains(memberCols, want) {
				t.Errorf("vendor_account_group_members lacks %q: %v", want, memberCols)
			}
		}
		var groupID string
		if err := s.queryRow(ctx, `select vendor_group_id from usage_events where id = ?`, "ue_pre114").Scan(&groupID); err != nil {
			t.Fatalf("read vendor_group_id: %v", err)
		}
		if groupID != "" {
			t.Errorf("pre-upgrade usage row vendor_group_id = %q, want empty", groupID)
		}

		// Replay: a second run of the step changes nothing.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := migration114Up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			t.Fatalf("migration114Up replay: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

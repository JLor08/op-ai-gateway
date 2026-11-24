// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMigration110VendorAccountShareUsage runs migration 110 against a genuine
// version-109 database that already holds a vendor account and pins what the
// upgrade owes an operator: vendor_accounts gains a not-null integer
// share_usage_with_members defaulting to 0, the pre-upgrade account reads back
// with the opt-in OFF (an upgrade must expose no usage data), and the column is
// then writable.
func TestMigration110VendorAccountShareUsage(t *testing.T) {
	forEachDialectMigratedTo(t, 109, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "vendor_accounts"); slices.Contains(cols, "share_usage_with_members") {
			t.Fatalf("vendor_accounts already has share_usage_with_members at v109: %v", cols)
		}
		if err := s.CreateUser(ctx, newTestUser("u_m110", "m110@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Raw insert: CreateVendorAccount now writes share_usage_with_members, a column
		// the version-109 schema does not have yet.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into vendor_accounts
			(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, '', '', '', '', ?, ?)`),
			"va_pre110", "u_m110", routing.VendorAnthropic, routing.VendorAuthSubscription, "Pre", routing.VendorAccountStatusActive, now, now); err != nil {
			t.Fatalf("seed account: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		raw, ok := tableColumnDecls(ctx, t, s, "vendor_accounts")["share_usage_with_members"]
		if !ok {
			t.Fatal("vendor_accounts lacks share_usage_with_members after migration 110")
		}
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
			t.Errorf("share_usage_with_members = %q, want a not-null integer column defaulting to 0", raw)
		}
		got, err := s.VendorAccountByID(ctx, "va_pre110")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ShareUsageWithMembers {
			t.Error("pre-upgrade account reads ShareUsageWithMembers = true, want false (opt-in)")
		}
		if err := s.UpdateVendorAccountShareUsage(ctx, "va_pre110", true); err != nil {
			t.Fatalf("UpdateVendorAccountShareUsage: %v", err)
		}
		after, err := s.VendorAccountByID(ctx, "va_pre110")
		if err != nil || !after.ShareUsageWithMembers {
			t.Fatalf("after enabling: err=%v ShareUsageWithMembers=%v, want true", err, after.ShareUsageWithMembers)
		}
		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

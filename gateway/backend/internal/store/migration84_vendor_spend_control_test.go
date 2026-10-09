// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestMigration84AddsSpendControlColumns runs migration 84 against a genuine
// version-83 database that already holds a vendor account and a usage snapshot
// row, and pins what the upgrade owes an operator: the seven new
// vendor_account_usage columns exist with the declared shape (non-null defaults,
// a nullable spend_reset_at), and the snapshot written BEFORE the upgrade reads
// back "no spend data" -- empty strings, an unknown -1 percent, a nil reset --
// rather than failing a NOT NULL or reading as a fabricated 0 %. No backfill runs:
// the pre-upgrade row's own values are untouched. Running the ledger again must
// be a no-op (addColumnIfMissing).
func TestMigration84AddsSpendControlColumns(t *testing.T) {
	newColumns := []string{
		"spend_unit", "spend_limit", "spend_used", "spend_remaining",
		"spend_used_pct", "spend_reset_at", "credit_status",
	}
	forEachDialectMigratedTo(t, 83, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

		// The pre-upgrade schema must NOT carry the columns yet.
		cols := tableColumns(ctx, t, s, "vendor_account_usage")
		for _, c := range newColumns {
			if slices.Contains(cols, c) {
				t.Fatalf("vendor_account_usage already has %s at v83: %v", c, cols)
			}
		}

		if err := s.CreateUser(ctx, newTestUser("u_m84", "m84@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := s.db.ExecContext(ctx, s.dl.rebind(query), args...); err != nil {
				t.Fatalf("seed %q: %v", query, err)
			}
		}
		exec(`insert into vendor_accounts (id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"va_m84", "u_m84", "openai", "subscription", "Pre-upgrade", "active", "", "enc:t", now, now)
		exec(`insert into vendor_account_usage (account_id, five_hour_pct, weekly_pct, credit_balance, updated_at)
			values (?, ?, ?, ?, ?)`, "va_m84", 42.5, -1, "12.34", now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "vendor_account_usage")
		for _, c := range newColumns {
			if _, ok := decls[c]; !ok {
				t.Fatalf("vendor_account_usage lacks %s after migration 84: %v", c, sortedColumnNames(decls))
			}
		}
		for _, c := range []string{"spend_unit", "spend_limit", "spend_used", "spend_remaining", "credit_status"} {
			if d := decls[c]; !strings.Contains(d, "not null") || !strings.Contains(d, "''") {
				t.Errorf("vendor_account_usage.%s = %q, want a not-null text column defaulting to ''", c, d)
			}
		}
		if d := decls["spend_used_pct"]; !strings.Contains(d, "double precision") || !strings.Contains(d, "not null") || !strings.Contains(d, "-1") {
			t.Errorf("vendor_account_usage.spend_used_pct = %q, want a not-null double precision defaulting to -1", d)
		}
		if d := decls["spend_reset_at"]; !strings.Contains(d, " null ") || strings.Contains(d, "not null") {
			t.Errorf("vendor_account_usage.spend_reset_at = %q, want a nullable column", d)
		}

		// The pre-upgrade snapshot keeps its own values and reads "no spend data".
		got, ok, err := s.VendorAccountUsageByID(ctx, "va_m84")
		if err != nil || !ok {
			t.Fatalf("read pre-upgrade usage: ok = %v, err = %v", ok, err)
		}
		if got.FiveHourPct != 42.5 || got.WeeklyPct != -1 || got.CreditBalance != "12.34" {
			t.Fatalf("pre-upgrade usage values changed by the migration: %+v", got)
		}
		if got.SpendUnit != "" || got.SpendLimit != "" || got.SpendUsed != "" || got.SpendRemaining != "" ||
			got.CreditStatus != "" || got.SpendUsedPct != -1 || got.SpendResetAt != nil {
			t.Fatalf("pre-upgrade usage spend fields = %+v, want the unknown defaults (\"\" / -1 / nil)", got)
		}

		// Idempotent: a second pass over the ledger (and a direct re-run of the
		// step) changes nothing and does not fail on the existing columns.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second migrate: %v", err)
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := migration84Up(ctx, tx, s.dl); err != nil {
			t.Fatalf("re-running migration84Up: %v", err)
		}
	})
}

// TestVendorAccountUsageSchemaColumnsAllCovered ties the vendor_account_usage
// round-trip fixture (TestRoutingStoreVendorAccountUsageUpsertRoundTrip, which
// sets every column to a distinct value across its two upserts) to the LIVE migrated
// schema (the #84 guard): a column added to the table but not seeded there would
// read back its zero from every reader and stay green. The table has no
// integer-boolean column, so every column is seeded.
func TestVendorAccountUsageSchemaColumnsAllCovered(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		assertColumnCoverage(context.Background(), t, s, "vendor_account_usage", columnCoverage{
			bools: nil,
			seeded: []string{
				"account_id", "credit_balance", "credit_status", "five_hour_pct", "five_hour_reset_at",
				"spend_limit", "spend_remaining", "spend_reset_at", "spend_unit", "spend_used",
				"spend_used_pct", "updated_at", "weekly_pct", "weekly_reset_at",
			},
			ignored: map[string]string{},
		})
	})
}

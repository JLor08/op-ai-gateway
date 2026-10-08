// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"slices"
	"testing"
	"time"
)

// TestMigration83AddsPrefixAndDisplayNameColumns runs migration 83 against a
// genuine version-82 database that already holds a vendor account and a model
// row, and pins the two things the upgrade owes an operator: the new columns
// exist, and rows written BEFORE the upgrade read back the empty-string default
// ("no prefix", "no display name") rather than failing a NOT NULL or scanning a
// NULL. Running the ledger again must be a no-op (addColumnIfMissing).
func TestMigration83AddsPrefixAndDisplayNameColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 82, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

		// The pre-upgrade schema must NOT carry the columns yet.
		if cols := tableColumns(ctx, t, s, "vendor_accounts"); slices.Contains(cols, "model_prefix") {
			t.Fatalf("vendor_accounts already has model_prefix at v82: %v", cols)
		}
		if cols := tableColumns(ctx, t, s, "vendor_account_models"); slices.Contains(cols, "display_name") {
			t.Fatalf("vendor_account_models already has display_name at v82: %v", cols)
		}

		if err := s.CreateUser(ctx, newTestUser("u_m83", "m83@example.test", now)); err != nil {
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
			"va_m83", "u_m83", "openai", "api_key", "Pre-upgrade", "active", "enc:k", "", now, now)
		exec(`insert into vendor_account_models (account_id, gateway_model, upstream_model, api_flavor) values (?, ?, ?, ?)`,
			"va_m83", "gpt-4o", "gpt-4o", "openai")

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		if cols := tableColumns(ctx, t, s, "vendor_accounts"); !slices.Contains(cols, "model_prefix") {
			t.Fatalf("vendor_accounts lacks model_prefix after migration 83: %v", cols)
		}
		if cols := tableColumns(ctx, t, s, "vendor_account_models"); !slices.Contains(cols, "display_name") {
			t.Fatalf("vendor_account_models lacks display_name after migration 83: %v", cols)
		}

		acc, err := s.VendorAccountByID(ctx, "va_m83")
		if err != nil {
			t.Fatalf("read pre-upgrade account: %v", err)
		}
		if acc.ModelPrefix != "" {
			t.Fatalf("pre-upgrade account ModelPrefix = %q, want the empty default", acc.ModelPrefix)
		}
		models, err := s.VendorAccountModels(ctx, "va_m83")
		if err != nil || len(models) != 1 {
			t.Fatalf("read pre-upgrade models = %+v, %v", models, err)
		}
		if models[0].DisplayName != "" {
			t.Fatalf("pre-upgrade model DisplayName = %q, want the empty default", models[0].DisplayName)
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
		if err := migration83Up(ctx, tx, s.dl); err != nil {
			t.Fatalf("re-running migration83Up: %v", err)
		}
	})
}

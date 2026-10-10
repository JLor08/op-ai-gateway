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

// TestMigration86AddsVendorAccountBaseURL runs migration 86 against a genuine
// version-85 database that already holds a vendor account, and pins what the
// upgrade owes an operator: vendor_accounts gains a not-null text base_url
// defaulting to ” (the bespoke openai/anthropic accounts have no custom root),
// the pre-upgrade row reads back with an empty BaseURL rather than failing a NOT
// NULL, and running the ledger again is a no-op (addColumnIfMissing).
func TestMigration86AddsVendorAccountBaseURL(t *testing.T) {
	forEachDialectMigratedTo(t, 85, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "vendor_accounts"); slices.Contains(cols, "base_url") {
			t.Fatalf("vendor_accounts already has base_url at v85: %v", cols)
		}

		if err := s.CreateUser(ctx, newTestUser("u_m86", "m86@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Seed a pre-upgrade row that cannot name base_url.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into vendor_accounts
			(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"va_pre", "u_m86", routing.VendorOpenAI, routing.VendorAuthAPIKey, "Pre", routing.VendorAccountStatusActive,
			"enc:k", "", "", now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "vendor_accounts")
		raw, ok := decls["base_url"]
		if !ok {
			t.Fatalf("vendor_accounts lacks base_url after migration 86: %v", sortedColumnNames(decls))
		}
		// sqlite reports the declared type upper-case (TEXT), postgres lower-case.
		d := strings.ToLower(raw)
		if !strings.Contains(d, "text") || !strings.Contains(d, "not null") || !strings.Contains(d, "''") {
			t.Errorf("vendor_accounts.base_url = %q, want a not-null text column defaulting to ''", raw)
		}

		acc, err := s.VendorAccountByID(ctx, "va_pre")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if acc.BaseURL != "" {
			t.Errorf("pre-upgrade BaseURL = %q, want empty", acc.BaseURL)
		}

		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

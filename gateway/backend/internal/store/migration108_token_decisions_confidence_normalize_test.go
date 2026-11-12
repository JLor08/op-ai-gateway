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

// TestMigration108AddsTokenDecisionsConfidenceNormalize runs migration 108 against a
// genuine version-107 database that already holds an API token, and pins what
// the upgrade owes an operator: api_tokens gains a not-null integer
// decisions_confidence_normalize defaulting to 0, the pre-upgrade token reads back
// with the flag OFF (the feature is opt-in, so an upgrade must change no
// token's behavior), the column is then writable, and running the ledger again
// is a no-op (addColumnIfMissing).
func TestMigration108AddsTokenDecisionsConfidenceNormalize(t *testing.T) {
	forEachDialectMigratedTo(t, 107, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "api_tokens"); slices.Contains(cols, "decisions_confidence_normalize") {
			t.Fatalf("api_tokens already has decisions_confidence_normalize at v107: %v", cols)
		}

		if err := s.CreateUser(ctx, newTestUser("u_m108", "m108@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Seed a pre-upgrade token that cannot name decisions_confidence_normalize
		// (the store's own insert already does, so this goes in by hand).
		seedBaselineToken(ctx, t, s, "tok_pre108", "u_m108", "Pre", "hash-pre108", now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "api_tokens")
		raw, ok := decls["decisions_confidence_normalize"]
		if !ok {
			t.Fatalf("api_tokens lacks decisions_confidence_normalize after migration 108: %v", sortedColumnNames(decls))
		}
		// sqlite reports the declared type upper-case (INTEGER), postgres lower-case.
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
			t.Errorf("api_tokens.decisions_confidence_normalize = %q, want a not-null integer column defaulting to 0", raw)
		}

		got, err := s.TokenByID(ctx, "tok_pre108")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.DecisionsConfidenceNormalize {
			t.Errorf("pre-upgrade token reads DecisionsConfidenceNormalize = true, want false (opt-in)")
		}

		// The new column is writable through the store.
		got.DecisionsConfidenceNormalize = true
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateTokenMetadata(ctx, got); err != nil {
			t.Fatalf("UpdateTokenMetadata: %v", err)
		}
		after, err := s.TokenByID(ctx, "tok_pre108")
		if err != nil || !after.DecisionsConfidenceNormalize {
			t.Fatalf("after enabling: err=%v DecisionsConfidenceNormalize=%v, want true", err, after.DecisionsConfidenceNormalize)
		}

		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

// seedBaselineToken inserts an api_tokens row naming only the columns every
// schema version has (the sibling of seedBaselineApplication): CreatePlainToken
// names every column added since, which an older database lacks. The secret
// hash is the given secret verbatim; the token has no bearer use here.
func seedBaselineToken(ctx context.Context, t *testing.T, s *SQLStore, id, userID, name, secret string, now time.Time) {
	t.Helper()
	mustExec(ctx, t, s, `insert into api_tokens
		(id, user_id, name, secret_hash, secret_prefix, status, scopes, created_at, updated_at)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, userID, name, secret, secretPrefix(secret), TokenStatusActive, `["gateway:use"]`, now, now)
}

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

// TestMigration89AddsTokenClaudeEffortCompatibility runs migration 89 against a
// genuine version-88 database that already holds an API token, and pins what
// the upgrade owes an operator: api_tokens gains a not-null integer
// claude_effort_compatibility defaulting to 0, the pre-upgrade token reads back
// with the flag OFF (the feature is opt-in, so an upgrade must change no
// token's behavior), the column is then writable, and running the ledger again
// is a no-op (addColumnIfMissing).
func TestMigration89AddsTokenClaudeEffortCompatibility(t *testing.T) {
	forEachDialectMigratedTo(t, 88, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "api_tokens"); slices.Contains(cols, "claude_effort_compatibility") {
			t.Fatalf("api_tokens already has claude_effort_compatibility at v88: %v", cols)
		}

		if err := s.CreateUser(ctx, newTestUser("u_m89", "m89@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Seed a pre-upgrade token that cannot name claude_effort_compatibility
		// (the store's own insert already does, so this goes in by hand).
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into api_tokens
			(id, user_id, name, secret_hash, secret_prefix, status, scopes, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"tok_pre89", "u_m89", "Pre", "hash-pre89", "pre89-se", TokenStatusActive, `["gateway:use"]`, now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "api_tokens")
		raw, ok := decls["claude_effort_compatibility"]
		if !ok {
			t.Fatalf("api_tokens lacks claude_effort_compatibility after migration 89: %v", sortedColumnNames(decls))
		}
		// sqlite reports the declared type upper-case (INTEGER), postgres lower-case.
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
			t.Errorf("api_tokens.claude_effort_compatibility = %q, want a not-null integer column defaulting to 0", raw)
		}

		got, err := s.TokenByID(ctx, "tok_pre89")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ClaudeEffortCompatibility {
			t.Errorf("pre-upgrade token reads ClaudeEffortCompatibility = true, want false (opt-in)")
		}

		// The new column is writable through the store.
		got.ClaudeEffortCompatibility = true
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateTokenMetadata(ctx, got); err != nil {
			t.Fatalf("UpdateTokenMetadata: %v", err)
		}
		after, err := s.TokenByID(ctx, "tok_pre89")
		if err != nil || !after.ClaudeEffortCompatibility {
			t.Fatalf("after enabling: err=%v ClaudeEffortCompatibility=%v, want true", err, after.ClaudeEffortCompatibility)
		}

		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

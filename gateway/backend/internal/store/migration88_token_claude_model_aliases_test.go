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

// TestMigration88AddsTokenClaudeModelAliases runs migration 88 against a genuine
// version-87 database that already holds an API token, and pins what the upgrade
// owes an operator: api_tokens gains a not-null integer claude_model_aliases
// defaulting to 0, the pre-upgrade token reads back with the flag OFF (the
// feature is opt-in, so an upgrade must change no token's behavior), the column
// is then writable, and running the ledger again is a no-op
// (addColumnIfMissing).
func TestMigration88AddsTokenClaudeModelAliases(t *testing.T) {
	forEachDialectMigratedTo(t, 87, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "api_tokens"); slices.Contains(cols, "claude_model_aliases") {
			t.Fatalf("api_tokens already has claude_model_aliases at v87: %v", cols)
		}

		if err := s.CreateUser(ctx, newTestUser("u_m88", "m88@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		// Seed a pre-upgrade token that cannot name claude_model_aliases (the
		// store's own insert already does, so this goes in by hand).
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into api_tokens
			(id, user_id, name, secret_hash, secret_prefix, status, scopes, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"tok_pre88", "u_m88", "Pre", "hash-pre88", "pre88-se", TokenStatusActive, `["gateway:use"]`, now, now); err != nil {
			t.Fatalf("seed: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "api_tokens")
		raw, ok := decls["claude_model_aliases"]
		if !ok {
			t.Fatalf("api_tokens lacks claude_model_aliases after migration 88: %v", sortedColumnNames(decls))
		}
		// sqlite reports the declared type upper-case (INTEGER), postgres lower-case.
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
			t.Errorf("api_tokens.claude_model_aliases = %q, want a not-null integer column defaulting to 0", raw)
		}

		got, err := s.TokenByID(ctx, "tok_pre88")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ClaudeModelAliases {
			t.Errorf("pre-upgrade token reads ClaudeModelAliases = true, want false (opt-in)")
		}

		// The new column is writable through the store.
		got.ClaudeModelAliases = true
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateTokenMetadata(ctx, got); err != nil {
			t.Fatalf("UpdateTokenMetadata: %v", err)
		}
		after, err := s.TokenByID(ctx, "tok_pre88")
		if err != nil || !after.ClaudeModelAliases {
			t.Fatalf("after enabling: err=%v ClaudeModelAliases=%v, want true", err, after.ClaudeModelAliases)
		}

		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

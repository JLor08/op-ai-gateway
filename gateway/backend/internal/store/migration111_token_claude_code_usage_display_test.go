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

// TestMigration111TokenClaudeCodeUsageDisplay runs migration 111 against a
// genuine version-110 database that already holds an API token: api_tokens
// gains a not-null integer claude_code_usage_display defaulting to 0, the
// pre-upgrade token reads back with the flag OFF (opt-in: an upgrade must not
// change any token's secret shape), and running the ledger again is a no-op.
func TestMigration111TokenClaudeCodeUsageDisplay(t *testing.T) {
	forEachDialectMigratedTo(t, 110, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "api_tokens"); slices.Contains(cols, "claude_code_usage_display") {
			t.Fatalf("api_tokens already has claude_code_usage_display at v110: %v", cols)
		}
		if err := createUserAtBaseline(ctx, s, newTestUser("u_m111", "m111@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		seedBaselineToken(ctx, t, s, "tok_pre111", "u_m111", "Pre", "hash-pre111", now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "api_tokens")
		raw, ok := decls["claude_code_usage_display"]
		if !ok {
			t.Fatalf("api_tokens lacks claude_code_usage_display after migration 111: %v", sortedColumnNames(decls))
		}
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
			t.Errorf("api_tokens.claude_code_usage_display = %q, want a not-null integer column defaulting to 0", raw)
		}
		got, err := s.TokenByID(ctx, "tok_pre111")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ClaudeCodeUsageDisplay {
			t.Errorf("pre-upgrade token reads ClaudeCodeUsageDisplay = true, want false (opt-in)")
		}
		if err := s.RotateUserTokenSecret(ctx, "tok_pre111", "hash-rot111", "hash-rot", true, now.Add(time.Minute)); err != nil {
			t.Fatalf("RotateUserTokenSecret: %v", err)
		}
		if after, err := s.TokenByID(ctx, "tok_pre111"); err != nil || !after.ClaudeCodeUsageDisplay {
			t.Fatalf("after rotate: err=%v flag=%v, want true", err, after.ClaudeCodeUsageDisplay)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

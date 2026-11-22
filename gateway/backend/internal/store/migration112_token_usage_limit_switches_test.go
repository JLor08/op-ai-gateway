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

// TestMigration112TokenUsageLimitSwitches runs migration 112 against a genuine
// version-111 database holding two API tokens: api_tokens gains two not-null
// integer columns defaulting to 0, send_usage_limits is backfilled from
// claude_code_usage_display (a token that already shows the limits in Claude
// Code keeps showing them), codex_model_switch_prompt starts off everywhere,
// and running the ledger again is a no-op that keeps the values.
func TestMigration112TokenUsageLimitSwitches(t *testing.T) {
	forEachDialectMigratedTo(t, 111, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

		cols := tableColumns(ctx, t, s, "api_tokens")
		for _, c := range []string{"send_usage_limits", "codex_model_switch_prompt"} {
			if slices.Contains(cols, c) {
				t.Fatalf("api_tokens already has %s at v111: %v", c, cols)
			}
		}
		if err := s.CreateUser(ctx, newTestUser("u_m112", "m112@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		seedBaselineToken(ctx, t, s, "tok_display", "u_m112", "Display", "hash-display", now)
		seedBaselineToken(ctx, t, s, "tok_plain", "u_m112", "Plain", "hash-plain", now)
		mustExec(ctx, t, s, `update api_tokens set claude_code_usage_display = 1 where id = ?`, "tok_display")

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "api_tokens")
		for _, c := range []string{"send_usage_limits", "codex_model_switch_prompt"} {
			raw, ok := decls[c]
			if !ok {
				t.Fatalf("api_tokens lacks %s after migration 112: %v", c, sortedColumnNames(decls))
			}
			d := strings.ToLower(raw)
			if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
				t.Errorf("api_tokens.%s = %q, want a not-null integer column defaulting to 0", c, raw)
			}
		}
		check := func() {
			t.Helper()
			display, err := s.TokenByID(ctx, "tok_display")
			if err != nil {
				t.Fatalf("read tok_display: %v", err)
			}
			plain, err := s.TokenByID(ctx, "tok_plain")
			if err != nil {
				t.Fatalf("read tok_plain: %v", err)
			}
			if !display.SendUsageLimits || plain.SendUsageLimits {
				t.Errorf("SendUsageLimits display=%v plain=%v, want true/false (backfill from claude_code_usage_display)", display.SendUsageLimits, plain.SendUsageLimits)
			}
			if display.CodexModelSwitchPrompt || plain.CodexModelSwitchPrompt {
				t.Errorf("CodexModelSwitchPrompt display=%v plain=%v, want false/false", display.CodexModelSwitchPrompt, plain.CodexModelSwitchPrompt)
			}
		}
		check()
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
		check()
	})
}

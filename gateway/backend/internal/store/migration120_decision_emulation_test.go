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

// TestMigration120DecisionEmulation runs migration 120 against a genuine
// version-119 database holding a token, a vendor account and a usage row and
// pins the upgrade (#380): api_tokens.decisions_emulation,
// vendor_accounts.decision_emulation and usage_events.decision_emulated each
// arrive as a not-null integer defaulting to 0, every pre-upgrade row reads
// back OFF (opt-in; an upgrade must change no behavior), the columns are then
// writable, and replaying the ledger is a no-op.
func TestMigration120DecisionEmulation(t *testing.T) {
	forEachDialectMigratedTo(t, 119, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

		cols := []struct{ table, col string }{
			{"api_tokens", "decisions_emulation"},
			{"vendor_accounts", "decision_emulation"},
			{"usage_events", "decision_emulated"},
		}
		for _, c := range cols {
			if have := tableColumns(ctx, t, s, c.table); slices.Contains(have, c.col) {
				t.Fatalf("%s already has %s at v119: %v", c.table, c.col, have)
			}
		}
		if err := createUserAtBaseline(ctx, s, newTestUser("u_m120", "m120@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		seedBaselineToken(ctx, t, s, "tok_pre120", "u_m120", "Pre", "hash-pre120", now)
		mustExec(ctx, t, s, `insert into vendor_accounts
			(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, '', '', '', '', ?, ?)`,
			"va_pre120", "u_m120", routing.VendorOpenAI, routing.VendorAuthSubscription, "Pre", routing.VendorAccountStatusActive, now, now)
		mustExec(ctx, t, s, `insert into usage_events
			(id, request_id, user_id, token_id, session_id, api_flavor, model, provider, host, status, error_code, http_status,
			 input_tokens, output_tokens, total_tokens, latency_ms, created_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			"ue_pre120", "ue_pre120", "u_m120", "tok_pre120", "", "", "m1", "p1", "h1", "success", "", 200,
			1, 2, 3, 4, now)

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		for _, c := range cols {
			raw, ok := tableColumnDecls(ctx, t, s, c.table)[c.col]
			if !ok {
				t.Fatalf("%s lacks %s after migration 120", c.table, c.col)
			}
			d := strings.ToLower(raw)
			if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "0") {
				t.Errorf("%s.%s = %q, want a not-null integer column defaulting to 0", c.table, c.col, raw)
			}
		}

		tok, err := s.TokenByID(ctx, "tok_pre120")
		if err != nil || tok.DecisionsEmulation {
			t.Fatalf("pre-upgrade token: err=%v DecisionsEmulation=%v, want false", err, tok.DecisionsEmulation)
		}
		acc, err := s.VendorAccountByID(ctx, "va_pre120")
		if err != nil || acc.DecisionEmulation {
			t.Fatalf("pre-upgrade account: err=%v DecisionEmulation=%v, want false", err, acc.DecisionEmulation)
		}
		events := s.All()
		if len(events) != 1 || events[0].DecisionEmulated {
			t.Fatalf("pre-upgrade usage row: %+v, want one event with DecisionEmulated false", events)
		}

		if err := s.UpdateVendorAccountDecisionEmulation(ctx, "va_pre120", true); err != nil {
			t.Fatalf("UpdateVendorAccountDecisionEmulation: %v", err)
		}
		if after, err := s.VendorAccountByID(ctx, "va_pre120"); err != nil || !after.DecisionEmulation {
			t.Fatalf("after enabling: err=%v DecisionEmulation=%v, want true", err, after.DecisionEmulation)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

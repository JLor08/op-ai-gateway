// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"testing"
)

// columnCoverage classifies EVERY column of a table for assertColumnCoverage,
// so a column added by a future migration cannot reach the readers and still be
// absent from the parity fixture (issue #84). Exactly one bucket per column:
//
//   - bools:   the integer-boolean columns carried by the parity bit-pattern
//     table (applicationParityBools / aiServerParityBools /
//     runtimeSpecParityBools). These are the dangerous ones -- a bool reads back
//     0 = false, a plausible value, so a reader that drops one stays green
//     unless a fixture row seeds it TRUE. The distinguishability guards pin the
//     bit-pattern (true-somewhere + every pair distinct); this list is the SAME
//     bool set those guards name, reused so the two cannot drift.
//   - seeded:  every other column the reader-agreement fixture gives a distinct,
//     non-zero value per row, so a dropped or reordered column shows as a
//     mismatch against the field-by-field baseline.
//   - ignored: columns deliberately covered by NO fixture row, each with the
//     reason it is safe (only genuinely unread columns belong here).
type columnCoverage struct {
	bools   []string
	seeded  []string
	ignored map[string]string // column -> reason it is deliberately uncovered
}

// assertColumnCoverage is the #84 guard: it ties a table's parity fixture to its
// LIVE migrated schema. The column-parity fixtures pin column PARITY by hand --
// applicationParityBools and its names[] are checked only against each other,
// never against the schema -- so a column added to `applications` /
// `agent_runtime_specs` / `ai_servers` could reach every reader correctly and
// still never appear in a fixture row: the conformance suite stays green because
// it does not know the column exists. That is worst for the ActiveMappingsForModel
// routing join, whose memory mirror serves a new Go field from a by-value copy
// while a column missed from the SQL join's select list reads back a clean zero
// (persistence.md records the hazard).
//
// The forcing function: every column the live schema reports must fall in exactly
// one coverage bucket, or this fails naming the column. Since the bit-pattern
// bools cannot be told apart from ordinary integers by SQL type (both are
// `integer not null default 0`), the bool-vs-seeded split stays a hand list -- but
// it can no longer silently OMIT a column, because the schema is the checklist.
func assertColumnCoverage(ctx context.Context, t *testing.T, s *SQLStore, table string, cov columnCoverage) {
	t.Helper()

	bucket := map[string]string{}
	classify := func(col, name string) {
		if prev, ok := bucket[col]; ok {
			t.Fatalf("%s.%s is classified in two buckets (%s and %s): a column belongs to exactly one", table, col, prev, name)
		}
		bucket[col] = name
	}
	for _, c := range cov.bools {
		classify(c, "bools")
	}
	for _, c := range cov.seeded {
		classify(c, "seeded")
	}
	for c := range cov.ignored {
		classify(c, "ignored")
	}

	schema := tableColumns(ctx, t, s, table) // sorted names from the live migrated schema
	inSchema := map[string]bool{}
	for _, c := range schema {
		inSchema[c] = true
	}

	// A classified column that is not in the schema: a rename, a typo, or a
	// dropped column the fixture was never told about.
	for col, name := range bucket {
		if !inSchema[col] {
			t.Errorf("the %s parity fixture classifies %q as %s, but no such column exists in the migrated schema -- a rename, a typo, or a dropped column: fix the fixture", table, col, name)
		}
	}
	// THE forcing function: a schema column no bucket covers. A migration added
	// it and nothing seeds it, so it reads back its zero value from every reader
	// and a reader that silently drops it stays green.
	for _, col := range schema {
		if _, ok := bucket[col]; ok {
			continue
		}
		t.Errorf("%s.%s exists in the migrated schema but the parity fixture covers no such column. A column no fixture row seeds a distinguishing value into reads back its zero from every reader, so a reader that drops it (e.g. the ActiveMappingsForModel join) stays GREEN while live routing ignores it (issue #84). Classify it: if it is an integer-boolean, add it to the bit-pattern (bools) and widen the table; if the fixture should vary it, add it to seeded and seed a distinct value; only if it is genuinely unread, add it to ignored WITH a reason.", table, col)
	}
}

// TestUsageEventsSchemaColumnsAllCovered is the #84 guard applied to
// usage_events. Unlike applications/ai_servers/agent_runtime_specs it has no
// reader-agreement fixture pinning a bit-pattern for its one
// integer-boolean column (stream) -- this test exists purely so a future
// column that reaches neither usageEventColumns nor Record's INSERT list
// cannot stay invisible to every test in this package the way the billing
// pair itself could have.
//
// request_id is the one WRITE-only column: Record's INSERT list carries it,
// but usageEventColumns (and its textual duplicates in ByUser/All) never
// select it back, because id already identifies the row for every lookup.
// It is declared in `ignored` rather than `seeded` for that reason -- it is
// not "genuinely unread" by oversight, it is unread by design, but the
// distinction the ignored map exists to record is the same: no fixture row
// needs to seed it a distinguishing value.
func TestUsageEventsSchemaColumnsAllCovered(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		assertColumnCoverage(context.Background(), t, s, "usage_events", columnCoverage{
			bools: []string{"stream"},
			seeded: []string{
				"agent_id", "api_flavor", "billing_quantity", "billing_unit",
				"cache_write_tokens", "cached_tokens", "content_type", "created_at",
				"energy_marginal_wh", "energy_source", "energy_wh", "error_code",
				"host", "http_status", "id", "input_tokens", "latency_ms", "model",
				"output_tokens", "project_id", "project_name", "prompt_per_second",
				"provider", "provider_model", "provider_path", "req_path",
				"requested_model", "route_id", "server_name", "service_id",
				"service_name", "session_id", "session_source", "status",
				"token_id", "token_name", "tokens_per_second", "total_tokens",
				"user_id",
			},
			ignored: map[string]string{
				"request_id": "written by Record's INSERT but never selected back by any reader (id already identifies the row; usageEventColumns, ByUser and All all omit it)",
			},
		})
	})
}

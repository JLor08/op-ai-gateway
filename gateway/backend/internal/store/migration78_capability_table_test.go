// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration78 runs migration78Up in its own tx. Its callers below
// hold a database stopped at version 77 (forEachDialectMigratedTo), so this
// is normally the migration's FIRST run, against a genuine pre-78 schema
// whose legacy columns they have just seeded — mirroring
// reinvokeMigration72's shape. The migration is also idempotent (create
// table if not exists + on conflict do nothing), so calling it a second time
// over its own output is safe, which is exactly what
// TestMigration78Idempotent below does.
func reinvokeMigration78(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration78Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration78Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedMigration78Mappings creates one server, one application and one mapping
// per id, so each id can then be forced into its own pre-78 column shape.
//
// The mapping and server rows go through the PUBLIC store API, which no longer
// writes the eleven columns migration 79 dropped -- they are
// `not null default` at version 77, so an insert that omits them still lands.
// Forcing a legacy value is then the caller's own raw `update` (mustExec),
// which needs a database stopped before 79: see forEachDialectMigratedTo.
//
// The APPLICATION row does NOT, and cannot: the public store API always speaks
// the column set of the CURRENT migration head, while every caller here hands
// it a database deliberately stopped at version 77. Those two agreed only for
// as long as no migration above 77 added an applications column -- migration
// 80's responses_live_timings_enabled is the first one that does, and
// CreateApplication naming it turned this helper into
// "table applications has no column named ...". The raw insert below names the
// original column set instead, every member of which exists at every version
// this helper is ever used at, so the application row is what it always was
// here: an FK parent whose own shape is not the subject of any migration-78 or
// -79 test. Any future applications column is additive with a DDL default and
// needs no edit here. (ai_servers and model_mappings carry the same latent
// coupling through CreateAIServer / CreateMapping; the same fix applies to
// whichever of them a post-77 migration touches first.)
func seedMigration78Mappings(ctx context.Context, t *testing.T, s *SQLStore, now time.Time, ids ...string) {
	t.Helper()
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_m78", Name: "M78", Provider: routing.ProviderVLLM, Endpoint: "http://m78:8000",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	mustExec(ctx, t, s, `insert into applications (
		id, server_id, type, port, scheme, api_flavors, priority, weight,
		timeout_ms, affinity_ttl_seconds, status, created_at, updated_at
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"app_m78", "srv_m78", routing.ProviderVLLM, 8000, "http", `["openai"]`,
		1, 1, 30000, 300, routing.ServerStatusActive, now, now)
	for _, id := range ids {
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: id, ApplicationID: "app_m78", GatewayModelName: id + "-model",
			AppModelName: id + "-upstream", Status: routing.ServerStatusActive,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping %s: %v", id, err)
		}
	}
}

// capabilityRowMap reads a mapping's backfilled rows keyed by capability, via
// the store API the rest of the codebase will use.
func capabilityRowMap(ctx context.Context, t *testing.T, s *SQLStore, mappingID string) map[string]routing.CapabilityRow {
	t.Helper()
	rows, err := s.MappingCapabilities(ctx, mappingID)
	if err != nil {
		t.Fatalf("MappingCapabilities(%s): %v", mappingID, err)
	}
	out := map[string]routing.CapabilityRow{}
	for _, r := range rows {
		if _, dup := out[r.Capability]; dup {
			t.Fatalf("%s: duplicate row for capability %q: %+v", mappingID, r.Capability, rows)
		}
		out[r.Capability] = r
	}
	return out
}

// TestMigration78BackfillFromLegacyColumns asserts every row of the design's
// backfill table, INCLUDING the two cases that must produce no row at all:
// a vision_capable of 0 whose metrics_source proves no measurement happened,
// and an is_mtp of 0 (a name heuristic's false, not a measured no). Absence
// of a row IS unknown, so writing either of those as "no" would enter a guess
// into the record as a measurement. Those two rules have no other coverage
// anywhere, which is why this test had to KEEP working across migration 79.
//
// It runs against a database stopped at version 77 -- the last one that
// still has the columns -- so the seeding below stays exactly the raw SQL it
// always was, and migration 78 is exercised against the genuine historical
// schema it was written for rather than a shape reconstructed on top of the
// current one. That is also the only honest option: after 79 the columns are
// gone, and a seed rewritten to avoid them would still compile while
// silently testing nothing.
func TestMigration78BackfillFromLegacyColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 77, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		checked := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
		seedMigration78Mappings(ctx, t, s, now,
			"m_caps", "m_lp_sup", "m_lp_unsup", "m_lp_blank",
			"m_vis1_bench", "m_vis1_manual", "m_vis1_other",
			"m_vis0_bench", "m_vis0_probe", "m_mtp1", "m_mtp0", "m_extra", "m_extra_bad")

		// The auto-detected capability columns (migration 77).
		mustExec(ctx, t, s, `update model_mappings set cap_vision = 'yes', cap_video = 'no',
			cap_audio = '', cap_tools = 'yes', capabilities_checked_at = ? where id = ?`, checked, "m_caps")
		// live_progress_support's three states.
		mustExec(ctx, t, s, `update model_mappings set live_progress_support = 'supported',
			live_progress_checked_at = ? where id = ?`, checked, "m_lp_sup")
		mustExec(ctx, t, s, `update model_mappings set live_progress_support = 'unsupported' where id = ?`, "m_lp_unsup")
		mustExec(ctx, t, s, `update model_mappings set live_progress_support = '' where id = ?`, "m_lp_blank")
		// vision_capable = 1: provenance from metrics_source.
		mustExec(ctx, t, s, `update model_mappings set vision_capable = 1, metrics_source = 'vision',
			metrics_updated_at = ? where id = ?`, checked, "m_vis1_bench")
		mustExec(ctx, t, s, `update model_mappings set vision_capable = 1, metrics_source = 'manual' where id = ?`, "m_vis1_manual")
		mustExec(ctx, t, s, `update model_mappings set vision_capable = 1, metrics_source = 'benchmark' where id = ?`, "m_vis1_other")
		// vision_capable = 0: a row ONLY for a measuring provenance.
		mustExec(ctx, t, s, `update model_mappings set vision_capable = 0, metrics_source = 'vision' where id = ?`, "m_vis0_bench")
		mustExec(ctx, t, s, `update model_mappings set vision_capable = 0, metrics_source = 'probe' where id = ?`, "m_vis0_probe")
		// is_mtp's two states.
		mustExec(ctx, t, s, `update model_mappings set is_mtp = 1, metrics_updated_at = ? where id = ?`, checked, "m_mtp1")
		mustExec(ctx, t, s, `update model_mappings set is_mtp = 0 where id = ?`, "m_mtp0")
		// cap_extra: a JSON array of names, and a malformed value that must be
		// skipped without failing the migration.
		mustExec(ctx, t, s, `update model_mappings set cap_extra = ?, capabilities_checked_at = ? where id = ?`,
			`["thinking","reasoning"]`, checked, "m_extra")
		mustExec(ctx, t, s, `update model_mappings set cap_extra = ? where id = ?`, `not json at all`, "m_extra_bad")

		reinvokeMigration78(ctx, t, s)

		// cap_vision/cap_video/cap_tools -> one row each, source
		// llama_cpp_props, checked_at from capabilities_checked_at. cap_audio
		// was "" (never determined) -> no row.
		caps := capabilityRowMap(ctx, t, s, "m_caps")
		for capability, verdict := range map[string]string{"vision": "yes", "video": "no", "tools": "yes"} {
			row, ok := caps[capability]
			if !ok {
				t.Fatalf("m_caps: no %s row: %+v", capability, caps)
			}
			if row.Verdict != verdict || row.Source != routing.CapabilitySourceLlamaCppProps {
				t.Fatalf("m_caps %s row = %+v, want verdict %q source llama_cpp_props", capability, row, verdict)
			}
			if !row.CheckedAt.Equal(checked) {
				t.Fatalf("m_caps %s checked_at = %v, want %v (capabilities_checked_at)", capability, row.CheckedAt, checked)
			}
		}
		if _, present := caps["audio"]; present {
			t.Fatalf("an empty cap_audio must produce NO row (it means never determined): %+v", caps)
		}

		// live_progress_support -> a live_progress row, "" -> no row.
		if row := capabilityRowMap(ctx, t, s, "m_lp_sup")["live_progress"]; row.Verdict != "yes" ||
			row.Source != routing.CapabilitySourceLlamaCppProps || !row.CheckedAt.Equal(checked) {
			t.Fatalf("m_lp_sup live_progress row = %+v, want yes/llama_cpp_props/%v", row, checked)
		}
		if row := capabilityRowMap(ctx, t, s, "m_lp_unsup")["live_progress"]; row.Verdict != "no" {
			t.Fatalf("m_lp_unsup live_progress row = %+v, want no", row)
		}
		if row, present := capabilityRowMap(ctx, t, s, "m_lp_unsup")["live_progress"]; !present ||
			row.CheckedAt.IsZero() {
			t.Fatalf("a null live_progress_checked_at must fall back to the migration timestamp: %+v", row)
		}
		if rows := capabilityRowMap(ctx, t, s, "m_lp_blank"); len(rows) != 0 {
			t.Fatalf("an empty live_progress_support must produce NO row: %+v", rows)
		}

		// vision_capable = 1: verdict yes, provenance mapped from metrics_source.
		for id, wantSource := range map[string]string{
			"m_vis1_bench":  routing.CapabilitySourceVisionBenchmark,
			"m_vis1_manual": routing.CapabilitySourceManual,
			"m_vis1_other":  routing.CapabilitySourceLegacy,
		} {
			row, ok := capabilityRowMap(ctx, t, s, id)["vision"]
			if !ok {
				t.Fatalf("%s: no vision row", id)
			}
			if row.Verdict != routing.CapabilityYes || row.Source != wantSource {
				t.Fatalf("%s vision row = %+v, want yes/%s", id, row, wantSource)
			}
		}

		// vision_capable = 0: a "no" row ONLY for a measuring provenance.
		if row := capabilityRowMap(ctx, t, s, "m_vis0_bench")["vision"]; row.Verdict != routing.CapabilityNo ||
			row.Source != routing.CapabilitySourceVisionBenchmark {
			t.Fatalf("m_vis0_bench vision row = %+v, want no/vision_benchmark", row)
		}
		if rows := capabilityRowMap(ctx, t, s, "m_vis0_probe"); len(rows) != 0 {
			t.Fatalf("a vision_capable of 0 without a measuring provenance must produce NO row -- the column conflated \"no\" with \"never probed\": %+v", rows)
		}

		// is_mtp: 1 -> a legacy-sourced yes; 0 -> NO row (the heuristic's false
		// means "the name did not match", i.e. unknown).
		if row := capabilityRowMap(ctx, t, s, "m_mtp1")["mtp"]; row.Verdict != routing.CapabilityYes ||
			row.Source != routing.CapabilitySourceLegacy || !row.CheckedAt.Equal(checked) {
			t.Fatalf("m_mtp1 mtp row = %+v, want yes/legacy/%v", row, checked)
		}
		if rows := capabilityRowMap(ctx, t, s, "m_mtp0"); len(rows) != 0 {
			t.Fatalf("an is_mtp of 0 must produce NO row -- its false is a name heuristic's miss, not a measured no: %+v", rows)
		}

		// cap_extra: one yes row per name; a malformed value is skipped
		// silently rather than failing the migration.
		extra := capabilityRowMap(ctx, t, s, "m_extra")
		for _, name := range []string{"thinking", "reasoning"} {
			row, ok := extra[name]
			if !ok {
				t.Fatalf("m_extra: no %s row: %+v", name, extra)
			}
			if row.Verdict != routing.CapabilityYes || row.Source != routing.CapabilitySourceLlamaCppProps {
				t.Fatalf("m_extra %s row = %+v, want yes/llama_cpp_props", name, row)
			}
			if !row.CheckedAt.Equal(checked) {
				t.Fatalf("m_extra %s checked_at = %v, want %v", name, row.CheckedAt, checked)
			}
		}
		if rows := capabilityRowMap(ctx, t, s, "m_extra_bad"); len(rows) != 0 {
			t.Fatalf("a malformed cap_extra must be skipped, producing no rows and no error: %+v", rows)
		}
	})
}

// TestMigration78Idempotent proves re-running migration 78 -- and then
// finishing the ledger over its output -- neither fails nor duplicates nor
// rewrites a backfilled row: the table create is `if not exists` and every
// backfill insert carries `on conflict do nothing`.
//
// Finishing the ledger is the second half of the assertion and it is not
// incidental: it applies migration 79, which DROPS the columns these rows
// were derived from. The rows must survive that untouched -- a backfill
// whose result the very next migration erased would be worse than no
// backfill at all.
func TestMigration78Idempotent(t *testing.T) {
	forEachDialectMigratedTo(t, 77, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_idem")
		mustExec(ctx, t, s, `update model_mappings set cap_vision = 'yes', is_mtp = 1,
			live_progress_support = 'supported' where id = ?`, "m_idem")

		reinvokeMigration78(ctx, t, s)
		first := capabilityRowMap(ctx, t, s, "m_idem")
		if len(first) != 3 {
			t.Fatalf("first pass = %+v, want vision/mtp/live_progress", first)
		}
		// The same migration a second time, over its own output and with the
		// columns still in place.
		reinvokeMigration78(ctx, t, s)
		sameRows(ctx, t, s, "m_idem", first, "re-running migration 78")

		// Then the rest of the ledger: 78 again (it is not recorded yet, so
		// Migrate runs it a THIRD time) followed by 79's column drop.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("finish the ledger: %v", err)
		}
		sameRows(ctx, t, s, "m_idem", first, "finishing the ledger through migration 79")
	})
}

// sameRows fails unless mappingID's capability rows still match want exactly
// -- same set, same verdict, same source, same timestamp. Used to assert
// that a repeated or continued migration changed nothing.
func sameRows(ctx context.Context, t *testing.T, s *SQLStore, mappingID string, want map[string]routing.CapabilityRow, what string) {
	t.Helper()
	got := capabilityRowMap(ctx, t, s, mappingID)
	if len(got) != len(want) {
		t.Fatalf("%s changed the row count: %d -> %d (%+v)", what, len(want), len(got), got)
	}
	for capability, row := range want {
		again, ok := got[capability]
		if !ok {
			t.Fatalf("%s lost the %s row", what, capability)
		}
		if again.Verdict != row.Verdict || again.Source != row.Source || !again.CheckedAt.Equal(row.CheckedAt) {
			t.Fatalf("%s rewrote the %s row: %+v -> %+v", what, capability, row, again)
		}
	}
}

// TestMigration78VisionBackfillResolvesByRank is the strongest pin in this
// file, because migration 78's vision backfill is the branch's ONE
// irreversible inference: migration 79 drops both source columns in the same
// release, so a wrong verdict here can never be recomputed from anything.
//
// TWO dropped columns can speak for the `vision` row -- cap_vision (always
// the /props probe, rank 1) and vision_capable (rank 3, 2 or 1 depending on
// what metrics_source says wrote it) -- and `on conflict do nothing` means
// whichever statement runs first keeps the mapping. So the statements run in
// RANK order, and this test walks every (cap_vision rank, vision_capable
// rank) pair the two columns can present:
//
//   - 3 vs 1 and its fail-closed mirror: an operator's verdict, locked or
//     not, must SURVIVE a probe's. This is the case the pre-fix order got
//     backwards -- it inserted the probe's row first and let `do nothing`
//     discard the operator's, the exact inversion the source rank exists to
//     make impossible.
//   - 2 vs 1, both directions: a real measurement (an image actually sent
//     upstream) outranks a probe re-reading a document.
//   - 1 vs 1: cap_vision still wins over a vision_capable whose origin is
//     unattributable (`legacy`). That is the ONE pair the old
//     cap_vision-first order was actually right about, and it is preserved
//     deliberately -- a direct /props signal beats a column whose real
//     writer is unknowable.
//   - the SAME evidence with the other column empty, which must land at the
//     same rank either way. Under the old order an operator's `manual`
//     verdict became rank 3 or rank 1 depending on whether an unrelated
//     column happened to be populated.
//   - the two no-row rules, which the reordering must NOT turn into `no`
//     rows: a vision_capable of 0 whose metrics_source proves no measurement
//     happened, and (over in TestMigration78BackfillFromLegacyColumns) every
//     is_mtp of 0.
//
// The two source columns are dated DIFFERENTLY on purpose --
// capabilities_checked_at for cap_vision, metrics_updated_at for
// vision_capable -- and every case asserts the surviving row's checked_at
// too. That is what makes each case name the statement that actually wrote
// the row rather than merely agreeing with its verdict.
func TestMigration78VisionBackfillResolvesByRank(t *testing.T) {
	// Distinct instants per source column, so a row's checked_at identifies
	// which statement wrote it.
	capsAt := time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
	metricsAt := time.Date(2026, 9, 2, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		id            string
		capVision     string // '' = the column determined nothing
		visionCapable int
		metricsSource string
		wantNoRow     bool
		wantVerdict   string
		wantSource    string
		wantCheckedAt time.Time
		why           string
	}{
		{
			id: "m_r3_over_r1", capVision: "yes", visionCapable: 0, metricsSource: "manual",
			wantVerdict: routing.CapabilityNo, wantSource: routing.CapabilitySourceManual, wantCheckedAt: metricsAt,
			why: "an operator's manual NO (rank 3) must survive a probe's yes (rank 1) -- image attach must stay off for a model the operator disabled it for",
		},
		{
			id: "m_r3_over_r1_mirror", capVision: "no", visionCapable: 1, metricsSource: "manual",
			wantVerdict: routing.CapabilityYes, wantSource: routing.CapabilitySourceManual, wantCheckedAt: metricsAt,
			why: "the mirror: an operator's manual YES (rank 3) must survive a probe's no (rank 1)",
		},
		{
			id: "m_r2_over_r1", capVision: "no", visionCapable: 1, metricsSource: "vision",
			wantVerdict: routing.CapabilityYes, wantSource: routing.CapabilitySourceVisionBenchmark, wantCheckedAt: metricsAt,
			why: "the vision benchmark measured this (rank 2) -- a probe re-reading /props (rank 1) must not displace it",
		},
		{
			id: "m_r2_over_r1_mirror", capVision: "yes", visionCapable: 0, metricsSource: "vision",
			wantVerdict: routing.CapabilityNo, wantSource: routing.CapabilitySourceVisionBenchmark, wantCheckedAt: metricsAt,
			why: "the mirror: a measured no (rank 2) must survive a probe's yes (rank 1)",
		},
		{
			id: "m_r1_tie", capVision: "no", visionCapable: 1, metricsSource: "benchmark",
			wantVerdict: routing.CapabilityNo, wantSource: routing.CapabilitySourceLlamaCppProps, wantCheckedAt: capsAt,
			why: "rank 1 against rank 1: the direct /props signal beats a vision_capable whose real writer is unknowable (legacy)",
		},
		{
			id: "m_manual_alone", capVision: "", visionCapable: 1, metricsSource: "manual",
			wantVerdict: routing.CapabilityYes, wantSource: routing.CapabilitySourceManual, wantCheckedAt: metricsAt,
			why: "the SAME manual evidence with cap_vision empty must land at the same rank 3 -- a verdict's provenance cannot depend on whether an unrelated column happens to be populated",
		},
		{
			id: "m_legacy_alone", capVision: "", visionCapable: 1, metricsSource: "benchmark",
			wantVerdict: routing.CapabilityYes, wantSource: routing.CapabilitySourceLegacy, wantCheckedAt: metricsAt,
			why: "an unattributable vision_capable with no cap_vision to lose to is still inherited, as legacy",
		},
		{
			id: "m_zero_nonmeasuring_probe", capVision: "yes", visionCapable: 0, metricsSource: "probe",
			wantVerdict: routing.CapabilityYes, wantSource: routing.CapabilitySourceLlamaCppProps, wantCheckedAt: capsAt,
			why: "a vision_capable of 0 without a measuring provenance says NOTHING, so it must neither win nor turn into a no row -- the probe's verdict stands",
		},
		{
			id: "m_zero_nonmeasuring_alone", capVision: "", visionCapable: 0, metricsSource: "probe",
			wantNoRow: true,
			why:       "and with no cap_vision either, that same silent 0 must produce NO row at all: absence is how the table says unknown",
		},
	}

	forEachDialectMigratedTo(t, 77, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		ids := make([]string, 0, len(cases))
		for _, tc := range cases {
			ids = append(ids, tc.id)
		}
		seedMigration78Mappings(ctx, t, s, now, ids...)
		for _, tc := range cases {
			mustExec(ctx, t, s, `update model_mappings set cap_vision = ?, capabilities_checked_at = ?,
				vision_capable = ?, metrics_source = ?, metrics_updated_at = ? where id = ?`,
				tc.capVision, capsAt, tc.visionCapable, tc.metricsSource, metricsAt, tc.id)
		}

		reinvokeMigration78(ctx, t, s)

		for _, tc := range cases {
			row, ok := capabilityRowMap(ctx, t, s, tc.id)["vision"]
			if tc.wantNoRow {
				if ok {
					t.Errorf("%s: got vision row %+v, want NO row -- %s", tc.id, row, tc.why)
				}
				continue
			}
			if !ok {
				t.Errorf("%s: no vision row at all, want %s/%s -- %s", tc.id, tc.wantVerdict, tc.wantSource, tc.why)
				continue
			}
			if row.Verdict != tc.wantVerdict || row.Source != tc.wantSource {
				t.Errorf("%s: vision row = %s/%s, want %s/%s -- %s",
					tc.id, row.Verdict, row.Source, tc.wantVerdict, tc.wantSource, tc.why)
			}
			if !row.CheckedAt.Equal(tc.wantCheckedAt) {
				t.Errorf("%s: vision checked_at = %v, want %v -- the surviving row must carry ITS OWN column's timestamp, which is what proves %s wrote it",
					tc.id, row.CheckedAt, tc.wantCheckedAt, tc.wantSource)
			}
		}
	})
}

// TestMigration78CapabilityRowsCascadeWithTheMapping proves the table's FK is
// declared ON DELETE CASCADE on the SQL side (the MemoryStore mirror is
// covered by TestRoutingStoreDeleteApplicationAndMappingCascade).
func TestMigration78CapabilityRowsCascadeWithTheMapping(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_casc")
		if err := s.UpsertMappingCapabilities(ctx, "m_casc", []routing.CapabilityRow{
			{
				Capability: routing.CapabilityVision, Verdict: routing.CapabilityYes,
				Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
			},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := s.DeleteMapping(ctx, "m_casc"); err != nil {
			t.Fatalf("delete mapping: %v", err)
		}
		if rows := capabilityRowMap(ctx, t, s, "m_casc"); len(rows) != 0 {
			t.Fatalf("capability rows must cascade with the mapping: %+v", rows)
		}
	})
}

// TestMappingCapabilitiesForMappingsChunking drives the bulk reader past its
// 1000-id chunk boundary, so the chunking loop is exercised rather than
// merely written.
func TestMappingCapabilitiesForMappingsChunking(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		ids := make([]string, 0, 1005)
		for i := range 1005 {
			ids = append(ids, fmt.Sprintf("m_chunk_%04d", i))
		}
		// Only the first and last mapping actually exist and carry a row; the
		// rest are unknown ids, which must contribute no keys.
		seedMigration78Mappings(ctx, t, s, now, ids[0], ids[len(ids)-1])
		for _, id := range []string{ids[0], ids[len(ids)-1]} {
			if err := s.UpsertMappingCapabilities(ctx, id, []routing.CapabilityRow{
				{
					Capability: routing.CapabilityTools, Verdict: routing.CapabilityYes,
					Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
				},
			}); err != nil {
				t.Fatalf("upsert %s: %v", id, err)
			}
		}
		got, err := s.MappingCapabilitiesForMappings(ctx, ids)
		if err != nil {
			t.Fatalf("bulk read across chunks: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("bulk read = %d keys, want 2 (only the mappings with rows)", len(got))
		}
		for _, id := range []string{ids[0], ids[len(ids)-1]} {
			if len(got[id]) != 1 || got[id][0].Capability != routing.CapabilityTools {
				t.Fatalf("bulk read %s = %+v", id, got[id])
			}
		}
	})
}

// TestMappingCapabilitiesForMappingsRepeatedIDAcrossChunks pins the SQL bulk
// reader against the one input on which it used to disagree with
// routing.MemoryStore: the same mapping id named in TWO different chunks.
// The scan appends per mapping id, so the second chunk appended that
// mapping's rows a second time, while MemoryStore assigns per id and returns
// them once. Neither caller can produce a duplicate today -- both build the
// id list from distinct mapping views -- so this is about the two drivers not
// being allowed to differ on an input either of them accepts.
//
// A duplicate INSIDE one chunk cannot show it: an `in (…)` list returns each
// matching row once however often its id is named. The repeat therefore has
// to straddle the 1000-id boundary, which is what the padding below is for.
func TestMappingCapabilitiesForMappingsRepeatedIDAcrossChunks(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_dup")
		if err := s.UpsertMappingCapabilities(ctx, "m_dup", []routing.CapabilityRow{
			{
				Capability: routing.CapabilityTools, Verdict: routing.CapabilityYes,
				Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
			},
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		// m_dup first, then enough unknown ids to fill the first chunk, then
		// m_dup again -- so the two occurrences land in different queries.
		ids := []string{"m_dup"}
		for i := range capabilityBatchChunk {
			ids = append(ids, fmt.Sprintf("m_pad_%04d", i))
		}
		ids = append(ids, "m_dup")

		got, err := s.MappingCapabilitiesForMappings(ctx, ids)
		if err != nil {
			t.Fatalf("bulk read with a repeated id: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("bulk read = %d keys (%+v), want 1", len(got), got)
		}
		if len(got["m_dup"]) != 1 {
			t.Fatalf("m_dup = %+v, want exactly ONE row -- an id repeated across a chunk boundary must not append its rows twice (routing.MemoryStore assigns and returns one)", got["m_dup"])
		}
	})
}

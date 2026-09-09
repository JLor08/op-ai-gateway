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

// reinvokeMigration78 re-runs migration78Up in its own tx after the pre-78
// column values have been forced in by hand, mirroring reinvokeMigration72.
// The migration is idempotent (create table if not exists + on conflict do
// nothing), so re-running it over an already-migrated database is safe — and
// TestMigration78Idempotent below relies on exactly that.
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
func seedMigration78Mappings(ctx context.Context, t *testing.T, s *SQLStore, now time.Time, ids ...string) {
	t.Helper()
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_m78", Name: "M78", Provider: routing.ProviderVLLM, Endpoint: "http://m78:8000",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{
		ID: "app_m78", ServerID: "srv_m78", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
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

// clearCapabilityRows removes every backfilled row so a re-run of
// migration78Up has to reproduce them from the columns alone. Migrate() has
// already applied migration 78 by the time a test body runs, so without this
// the `on conflict do nothing` backfill would have nothing left to prove.
func clearCapabilityRows(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	mustExec(ctx, t, s, `delete from model_mapping_capabilities`)
}

// TestMigration78BackfillFromLegacyColumns asserts every row of the design's
// backfill table, INCLUDING the two cases that must produce no row at all:
// a vision_capable of 0 whose metrics_source proves no measurement happened,
// and an is_mtp of 0 (a name heuristic's false, not a measured no). Absence
// of a row IS unknown, so writing either of those as "no" would enter a guess
// into the record as a measurement.
func TestMigration78BackfillFromLegacyColumns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
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

		clearCapabilityRows(ctx, t, s)
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

// TestMigration78Idempotent proves re-running the whole migration ledger (and
// migration 78 by itself) neither fails nor duplicates a backfilled row: the
// table create is `if not exists` and every backfill insert carries `on
// conflict do nothing`.
func TestMigration78Idempotent(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_idem")
		mustExec(ctx, t, s, `update model_mappings set cap_vision = 'yes', is_mtp = 1,
			live_progress_support = 'supported' where id = ?`, "m_idem")

		clearCapabilityRows(ctx, t, s)
		// Migrate() is a no-op now (78 is already recorded), so run 78 itself
		// to produce the rows, then run BOTH again over the result.
		reinvokeMigration78(ctx, t, s)
		first := capabilityRowMap(ctx, t, s, "m_idem")
		if len(first) != 3 {
			t.Fatalf("first pass = %+v, want vision/mtp/live_progress", first)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		reinvokeMigration78(ctx, t, s)
		second := capabilityRowMap(ctx, t, s, "m_idem")
		if len(second) != len(first) {
			t.Fatalf("re-running the migration changed the row count: %d -> %d (%+v)", len(first), len(second), second)
		}
		for capability, row := range first {
			again, ok := second[capability]
			if !ok {
				t.Fatalf("re-run lost the %s row", capability)
			}
			if again.Verdict != row.Verdict || again.Source != row.Source || !again.CheckedAt.Equal(row.CheckedAt) {
				t.Fatalf("re-run rewrote the %s row: %+v -> %+v", capability, row, again)
			}
		}
	})
}

// TestMigration78VisionColumnDoesNotBeatCapVision pins the backfill's ORDER,
// which is load-bearing: the cap_vision statement runs FIRST, so the later
// vision_capable statement's `on conflict do nothing` leaves the newer,
// better-provenanced verdict in place. With the order reversed, a stale
// vision_capable would silently win.
func TestMigration78VisionColumnDoesNotBeatCapVision(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_both")
		// cap_vision says no (the newer probe); vision_capable says yes (the
		// column it supersedes), with a provenance that would outrank it.
		mustExec(ctx, t, s, `update model_mappings set cap_vision = 'no', vision_capable = 1,
			metrics_source = 'manual' where id = ?`, "m_both")

		clearCapabilityRows(ctx, t, s)
		reinvokeMigration78(ctx, t, s)

		row, ok := capabilityRowMap(ctx, t, s, "m_both")["vision"]
		if !ok {
			t.Fatal("m_both: no vision row")
		}
		if row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceLlamaCppProps {
			t.Fatalf("vision row = %+v, want the cap_vision verdict (no/llama_cpp_props) to win over vision_capable", row)
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

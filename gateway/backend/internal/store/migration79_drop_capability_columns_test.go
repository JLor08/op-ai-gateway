// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// droppedCapabilityColumns is migration 79's whole subject: the eleven
// model_mappings columns model_mapping_capabilities superseded. Named here
// once so every assertion below reads from the same list.
//
// model_mapping_benchmarks.vision_capable is deliberately NOT in it: that is
// a benchmark RUN's history (migration 33), one row per measurement, not a
// mapping's current verdict. TestMigration79KeepsTheBenchmarkHistoryColumn
// pins that it survives.
var droppedCapabilityColumns = []string{
	// migration 77.
	"cap_vision", "cap_video", "cap_audio", "cap_tools", "cap_extra",
	"capabilities_source", "capabilities_checked_at",
	// migration 32.
	"vision_capable",
	// the baseline (migration 1) / migration 9.
	"is_mtp",
	// migration 76.
	"live_progress_support", "live_progress_checked_at",
}

// tableColumns reads a table's column names from the database itself, per
// dialect: sqlite has pragma_table_info, postgres has information_schema.
// Reading the LIVE schema rather than the migration source is the point --
// a migration that silently did nothing would still leave the source
// looking correct.
func tableColumns(ctx context.Context, t *testing.T, s *SQLStore, table string) []string {
	t.Helper()
	q := `select name from pragma_table_info(?)`
	if s.dl.name() == "postgres" {
		q = `select column_name from information_schema.columns
			where table_schema = 'public' and table_name = ?`
	}
	rows, err := s.db.QueryContext(ctx, s.dl.rebind(q), table)
	if err != nil {
		t.Fatalf("read %s columns: %v", table, err)
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan %s column: %v", table, err)
		}
		out = append(out, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s columns: %v", table, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s reported no columns at all -- does the table exist?", table)
	}
	slices.Sort(out)
	return out
}

// TestMigration79DropsTheElevenCapabilityColumns is this task's central
// assertion: after the full migration ledger, model_mappings carries none of
// the eleven columns model_mapping_capabilities replaced. Reverting
// migration79Up alone must make exactly this test (and its two siblings
// below) fail while everything else still passes -- that is the proof that
// nothing depends on the columns any more.
func TestMigration79DropsTheElevenCapabilityColumns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		cols := tableColumns(context.Background(), t, s, "model_mappings")
		for _, dropped := range droppedCapabilityColumns {
			if slices.Contains(cols, dropped) {
				t.Fatalf("model_mappings still has %q after migration 79; columns = %v", dropped, cols)
			}
		}
		// Sanity: the table is still the table (a dropped-everything bug
		// would satisfy the loop above vacuously).
		for _, kept := range []string{"id", "application_id", "gateway_model_name", "metrics_locked", "metrics_source"} {
			if !slices.Contains(cols, kept) {
				t.Fatalf("model_mappings lost %q, which migration 79 must not touch; columns = %v", kept, cols)
			}
		}
	})
}

// TestMigration79KeepsTheBenchmarkHistoryColumn: model_mapping_benchmarks.
// vision_capable is a benchmark RUN's recorded result (migration 33), not a
// mapping's current verdict, so it is outside migration 79's subject
// entirely. A drop written against the column NAME rather than the
// (table, column) pair would take it with them.
func TestMigration79KeepsTheBenchmarkHistoryColumn(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		cols := tableColumns(context.Background(), t, s, "model_mapping_benchmarks")
		if !slices.Contains(cols, "vision_capable") {
			t.Fatalf("model_mapping_benchmarks lost vision_capable -- a benchmark run's history must survive; columns = %v", cols)
		}
	})
}

// freshlyMigratedColumns migrates a SECOND, empty database of the SAME
// dialect through the whole ledger and returns one of its tables' columns --
// the "fresh install" half of TestMigration79FreshInstallMatchesUpgradedSchema.
//
// On sqlite that is simply another temp file. On postgres there is one DSN
// and the suite's own clean slate is `drop schema public cascade`, so the
// fresh run reuses THIS connection with its schema dropped -- which
// DESTROYS s. Call it last in a subtest, never before an assertion that
// still needs s. (A postgres subtest already starts by dropping the schema,
// so leaving a freshly migrated one behind affects nothing downstream.)
func freshlyMigratedColumns(ctx context.Context, t *testing.T, s *SQLStore, table string) []string {
	t.Helper()
	if s.dl.name() == "postgres" {
		if err := dropAllTables(ctx, s); err != nil {
			t.Fatalf("drop schema for the fresh run: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("fresh Migrate: %v", err)
		}
		return tableColumns(ctx, t, s, table)
	}
	fresh, err := OpenSQLite(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("open a fresh sqlite database: %v", err)
	}
	defer fresh.Close()
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatalf("fresh Migrate: %v", err)
	}
	return tableColumns(ctx, t, fresh, table)
}

// TestMigration79FreshInstallMatchesUpgradedSchema pins the ORDER dependency
// between 78 and 79: 78 READS the eleven columns to backfill from them, 79
// drops them. A fresh install replays both against an empty table; an
// upgraded database replays them against real rows. Both must land on the
// same model_mappings shape -- and the upgrade must still be carrying the
// rows 78 backfilled, which is the half a drop could silently undo.
func TestMigration79FreshInstallMatchesUpgradedSchema(t *testing.T) {
	// The upgraded database: stop the ledger at 77 (the last version that
	// still has the columns), write a mapping whose legacy columns say
	// something, then run the rest -- 78 backfills from them, 79 drops them.
	forEachDialectMigratedTo(t, 77, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_upgrade")
		mustExec(ctx, t, s, `update model_mappings set cap_vision = 'yes', is_mtp = 1,
			live_progress_support = 'supported' where id = ?`, "m_upgrade")

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate 78+79 over the pre-78 shape: %v", err)
		}

		got := capabilityRowMap(ctx, t, s, "m_upgrade")
		for _, capability := range []string{
			routing.CapabilityVision, routing.CapabilityMTP, routing.CapabilityLiveProgress,
		} {
			if _, ok := got[capability]; !ok {
				t.Fatalf("the %s row 78 backfilled did not survive 79: %+v", capability, got)
			}
		}
		upgraded := tableColumns(ctx, t, s, "model_mappings")

		// LAST: on postgres this reuses (and wipes) s -- see the helper.
		fresh := freshlyMigratedColumns(ctx, t, s, "model_mappings")
		if !slices.Equal(fresh, upgraded) {
			t.Fatalf("fresh install and upgrade disagree on model_mappings:\n fresh    = %v\n upgraded = %v", fresh, upgraded)
		}
	})
}

// TestMigration79IsIdempotent: re-running 79 over a database it has already
// been applied to is a no-op, not an error -- the property dropColumnIfPresent
// exists for (postgres' `if exists` clause; sqlite's swallowed "no such
// column"). Migrate() itself would not re-run a recorded migration, so this
// invokes 79 directly, twice.
func TestMigration79IsIdempotent(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		for i := range 2 {
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin tx: %v", err)
			}
			if err := migration79Up(ctx, tx, s.dl); err != nil {
				_ = tx.Rollback()
				t.Fatalf("migration79Up re-run %d over an already-dropped schema: %v", i+1, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit re-run %d: %v", i+1, err)
			}
		}
		cols := tableColumns(ctx, t, s, "model_mappings")
		for _, dropped := range droppedCapabilityColumns {
			if slices.Contains(cols, dropped) {
				t.Fatalf("model_mappings still has %q; columns = %v", dropped, cols)
			}
		}
	})
}

// TestMigration79MappingRoundTripsWithoutTheColumns proves the store's own
// insert/update/select statements agree with the post-79 schema: a mapping
// created and updated through the public API round-trips, which it cannot do
// if any statement still names a dropped column.
func TestMigration79MappingRoundTripsWithoutTheColumns(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, s, now, "m_rt")

		got, err := s.MappingByID(ctx, "m_rt")
		if err != nil {
			t.Fatalf("MappingByID: %v", err)
		}
		got.ContextSize = 4096
		got.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateMapping(ctx, got); err != nil {
			t.Fatalf("UpdateMapping: %v", err)
		}
		again, err := s.MappingByID(ctx, "m_rt")
		if err != nil {
			t.Fatalf("MappingByID after update: %v", err)
		}
		if again.ContextSize != 4096 {
			t.Fatalf("round-tripped mapping = %+v, want ContextSize 4096", again)
		}
		if _, err := s.MappingsByApplication(ctx, "app_m78"); err != nil {
			t.Fatalf("MappingsByApplication: %v", err)
		}
		if _, err := s.MappingsByServer(ctx, "srv_m78"); err != nil {
			t.Fatalf("MappingsByServer: %v", err)
		}
		if _, err := s.ActiveMappingsForModel(ctx, "m_rt-model", routing.APIFlavorOpenAI); err != nil {
			t.Fatalf("ActiveMappingsForModel: %v", err)
		}
	})
}

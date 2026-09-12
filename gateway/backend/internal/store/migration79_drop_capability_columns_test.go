// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
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

// tableColumnDecls reads a table's column names AND their full declared
// shape from the database itself, per dialect: sqlite's pragma_table_info
// exposes name/type/notnull/dflt_value in one row, postgres'
// information_schema.columns calls the same four
// column_name/data_type/is_nullable/column_default. Reading the LIVE schema
// rather than the migration source is the point -- a migration that silently
// did nothing, or added a column with the wrong shape, would still leave the
// source looking correct.
//
// The shape is the TYPE plus nullability plus default, folded into one string
// ("integer not null default 0"), because the type alone does not pin what
// callers actually depend on. That string is NOT dialect-neutral and must not
// be compared across dialects: SQLite's pragma reports the canonical `INTEGER`
// for a column declared lowercase, while PostgreSQL's information_schema
// reports `integer`, so the same real DDL folds to two strings differing only
// in case. Every caller compares within ONE dialect -- a column against a
// sibling column, or a fresh install against an upgraded one on the same
// engine -- which is what makes the fold safe here; a cross-dialect comparison
// would be exactly the silent case-mismatch this helper exists to catch. A column declared
// `integer` where the precedent is `integer not null default 0` gives every
// existing row NULL instead of 0 -- same data_type, and every later Scan of
// that column behaves differently. The three together are what ADR-005's
// narrow-vs-wide hazard is really about.
//
// Fails loudly (rather than returning a zero value) if a reported type is
// empty, or if a nullability flag is a value neither dialect is supposed to
// produce: either would make the comparisons below vacuously "equal" without
// ever having compared anything.
func tableColumnDecls(ctx context.Context, t *testing.T, s *SQLStore, table string) map[string]string {
	t.Helper()
	q := `select name, type, "notnull", dflt_value from pragma_table_info(?)`
	if s.dl.name() == "postgres" {
		q = `select column_name, data_type, is_nullable, column_default from information_schema.columns
			where table_schema = 'public' and table_name = ?`
	}
	rows, err := s.db.QueryContext(ctx, s.dl.rebind(q), table)
	if err != nil {
		t.Fatalf("read %s column declarations: %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var name, typ string
		// sqlite returns notnull as 0/1 and dflt_value as NULL when there is
		// no default; postgres returns is_nullable as YES/NO and
		// column_default as NULL. NullString absorbs both, including the
		// int64-to-string conversion database/sql does for the sqlite flag.
		var notNull, dflt sql.NullString
		if err := rows.Scan(&name, &typ, &notNull, &dflt); err != nil {
			t.Fatalf("scan %s column declaration: %v", table, err)
		}
		if typ == "" {
			t.Fatalf("%s.%s reported an empty type -- the query is reading the wrong column, or the driver is not returning one", table, name)
		}
		var nullability string
		switch notNull.String {
		case "1", "NO":
			nullability = "not null"
		case "0", "YES":
			nullability = "null"
		default:
			t.Fatalf("%s.%s reported nullability %q, which is neither sqlite's 0/1 nor postgres' YES/NO -- the query is reading the wrong column", table, name, notNull.String)
		}
		def := "(no default)"
		if dflt.Valid {
			def = "default " + dflt.String
		}
		out[name] = typ + " " + nullability + " " + def
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s column declarations: %v", table, err)
	}
	if len(out) == 0 {
		t.Fatalf("%s reported no columns at all -- does the table exist?", table)
	}
	return out
}

// sortedColumnNames is tableColumnDecls' name-only view, sorted -- what most
// callers below actually want (they only care whether a column is present,
// not what shape it has).
func sortedColumnNames(decls map[string]string) []string {
	out := make([]string, 0, len(decls))
	for name := range decls {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// tableColumns reads a table's column names from the database itself, sorted.
// See tableColumnDecls for how, and for the shape-level view of the same read.
func tableColumns(ctx context.Context, t *testing.T, s *SQLStore, table string) []string {
	t.Helper()
	return sortedColumnNames(tableColumnDecls(ctx, t, s, table))
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

// TestMigration79FreshInstallMatchesUpgradedSchema pins the ORDER dependency
// between 78 and 79: 78 READS the eleven columns to backfill from them, 79
// drops them. A fresh install replays both against an empty table; an
// upgraded database replays them against real rows. Both must land on the
// same model_mappings SHAPE -- same column names AND same declared shape
// (type, nullability, default), not merely the same names -- and the upgrade
// must still be carrying the rows 78 backfilled, which is the half a drop
// could silently undo. The shape comparison is what would catch a future
// migration whose ALTER lands on a different declaration than the baseline
// CREATE would have (ADR-005's narrow-vs-wide column type hazard, and the
// nullable-with-no-default variant of it); a name-only comparison passes
// silently through that class of bug.
//
// FRESH runs FIRST, UPGRADE second, and that order is deliberate rather than
// incidental: forEachDialect already leaves s freshly migrated end-to-end, so
// "fresh" costs nothing but reading its columns before anything below
// touches the store. The upgrade half is the one that has to rebuild a
// pre-78 shape -- on postgres, by dropping the schema and replaying the
// ledger to 77 on this SAME connection, the only DSN there is, which
// destroys whatever was there before. Doing that first would leave nothing
// to compare "fresh" against, since a schema drop cannot be undone. Ordering
// it this way means no helper has to warn a future caller "call me last": the
// destructive step is simply this test's own final step, not a shared
// function's precondition.
func TestMigration79FreshInstallMatchesUpgradedSchema(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()

		// FRESH first, while s is still exactly what forEachDialect left it
		// as: the full ledger applied to an empty database.
		freshDecls := tableColumnDecls(ctx, t, s, "model_mappings")

		// UPGRADE second: rebuild a genuine pre-78 shape (the last version
		// that still has the columns), write a mapping whose legacy columns
		// say something, then run the rest -- 78 backfills from them, 79
		// drops them. Postgres has one DSN, so this reuses (and resets) s;
		// sqlite gets a second temp file instead, since a file is cheap and
		// there is no reason to destroy the one fresh already read from.
		upgrade := s
		if s.dl.name() == "postgres" {
			if err := dropAllTables(ctx, s); err != nil {
				t.Fatalf("drop schema for the upgrade run: %v", err)
			}
			if err := s.migrateTo(ctx, 77); err != nil {
				t.Fatalf("migrate to 77: %v", err)
			}
		} else {
			upgradeSQLite, err := OpenSQLite(filepath.Join(t.TempDir(), "upgrade.db"))
			if err != nil {
				t.Fatalf("open a pre-78 sqlite database: %v", err)
			}
			defer upgradeSQLite.Close()
			if err := upgradeSQLite.migrateTo(ctx, 77); err != nil {
				t.Fatalf("migrate to 77: %v", err)
			}
			upgrade = upgradeSQLite
		}

		now := time.Now().UTC().Truncate(time.Second)
		seedMigration78Mappings(ctx, t, upgrade, now, "m_upgrade")
		mustExec(ctx, t, upgrade, `update model_mappings set cap_vision = 'yes', is_mtp = 1,
			live_progress_support = 'supported' where id = ?`, "m_upgrade")

		if err := upgrade.Migrate(ctx); err != nil {
			t.Fatalf("Migrate 78+79 over the pre-78 shape: %v", err)
		}

		got := capabilityRowMap(ctx, t, upgrade, "m_upgrade")
		for _, capability := range []string{
			routing.CapabilityVision, routing.CapabilityMTP, routing.CapabilityLiveProgress,
		} {
			if _, ok := got[capability]; !ok {
				t.Fatalf("the %s row 78 backfilled did not survive 79: %+v", capability, got)
			}
		}
		upgradedDecls := tableColumnDecls(ctx, t, upgrade, "model_mappings")

		fresh := sortedColumnNames(freshDecls)
		upgraded := sortedColumnNames(upgradedDecls)
		if !slices.Equal(fresh, upgraded) {
			t.Fatalf("fresh install and upgrade disagree on model_mappings columns:\n fresh    = %v\n upgraded = %v", fresh, upgraded)
		}
		// Same names is not the same shape: a column that survived under the
		// same name but landed on a different declaration (a narrower
		// integer, a different timestamp precision, a lost NOT NULL or
		// default, ...) would pass the check above silently. Compare WITHIN
		// this dialect only -- fresh and upgraded are always the same dialect
		// here, so no cross-dialect normalisation is needed.
		for _, name := range upgraded {
			if freshDecls[name] != upgradedDecls[name] {
				t.Fatalf("fresh install and upgrade disagree on the declaration of model_mappings.%s: fresh = %q, upgraded = %q", name, freshDecls[name], upgradedDecls[name])
			}
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

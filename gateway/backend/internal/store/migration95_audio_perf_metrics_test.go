// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// reinvokeMigration95 runs migration95Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration95(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration95Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration95Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration95AddsAudioPerfMetricColumns runs migration 95 against a genuine
// version-94 database that already holds a speed-history row, and pins what an
// upgrade of a deployment with benchmark history depends on (#263): the three
// audio perf-metric columns do not exist before the migration and exist after it
// on BOTH dialects; each is declared with the wide type ADR-005 demands (bigint
// for the millisecond counts, double precision for the real-time factor) and is
// NULLABLE with no default -- a pre-existing speed/capacity/vision row has no
// audio measurement, and "never measured" must stay distinguishable from a
// measured zero, so the backfill is deliberately none: the old row reads back
// NULL in the database and zero ("not measured") through the store; a speech row
// round-trips its metrics; and a replay of the migration changes nothing (it is
// duplicate-tolerant).
func TestMigration95AddsAudioPerfMetricColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 94, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

		cols := []string{"ttft_ms", "wall_ms", "rtf"}
		before := tableColumnDecls(ctx, t, s, "model_mapping_benchmarks")
		for _, c := range cols {
			if _, ok := before[c]; ok {
				t.Fatalf("model_mapping_benchmarks already has %s at v94", c)
			}
		}

		// A speed-history row written before the columns existed. Raw SQL on
		// purpose: at v94 InsertBenchmarkRun would name columns that are not
		// there yet, and the whole point is a row the new columns are added UNDER.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m95", Name: "m95", Provider: routing.ProviderAudioCpp,
			Endpoint: "http://srv_m95:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m95", ServerID: "srv_m95", Type: routing.ProviderAudioCpp, Port: 8095, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_m95", ApplicationID: "app_m95", GatewayModelName: "m95-tts", AppModelName: "up-m95",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`insert into model_mapping_benchmarks (id, mapping_id, server_id, created_at, gen_tokens_per_second) values (?, ?, ?, ?, ?)`),
			"bmk_old", "map_m95", "srv_m95", now, 42.5); err != nil {
			t.Fatalf("seed v94 benchmark row: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		after := tableColumnDecls(ctx, t, s, "model_mapping_benchmarks")
		want := map[string]string{
			"ttft_ms": "bigint null (no default)",
			"wall_ms": "bigint null (no default)",
			"rtf":     "double precision null (no default)",
		}
		for _, c := range cols {
			got, ok := after[c]
			if !ok {
				t.Fatalf("model_mapping_benchmarks has no %s column after migration 95; got %v", c, sortedColumnNames(after))
			}
			if got != want[c] {
				t.Fatalf("model_mapping_benchmarks.%s is declared %q, want %q (ADR-005 wide type, nullable, no default)", c, got, want[c])
			}
		}
		// The float column is the same wide type the table's throughput columns use.
		if tps := after["gen_tokens_per_second"]; !strings.HasPrefix(tps, "double precision ") {
			t.Fatalf("gen_tokens_per_second is declared %q; the precedent float column should be double precision", tps)
		}

		// The pre-existing row: NULL in the database (no backfill), zero
		// ("not measured") through the store, everything else untouched.
		var ttft, wall sql.NullInt64
		var rtf sql.NullFloat64
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(
			`select ttft_ms, wall_ms, rtf from model_mapping_benchmarks where id = ?`), "bmk_old").Scan(&ttft, &wall, &rtf); err != nil {
			t.Fatalf("read the pre-migration row's new columns: %v", err)
		}
		if ttft.Valid || wall.Valid || rtf.Valid {
			t.Fatalf("pre-migration row has ttft=%v wall=%v rtf=%v, want all NULL (a backfilled zero would claim a measurement)", ttft, wall, rtf)
		}
		runs, err := s.BenchmarkRunsByMapping(ctx, "map_m95", 10)
		if err != nil || len(runs) != 1 {
			t.Fatalf("read pre-migration history: %d rows, err %v", len(runs), err)
		}
		if old := runs[0]; old.GenTokensPerSecond != 42.5 || old.Kind != "speed" || old.TTFTMS != 0 || old.WallMS != 0 || old.RTF != 0 {
			t.Fatalf("pre-migration row must read back untouched with zero audio metrics, got %+v", old)
		}

		// A speech row round-trips its metrics, and a row that measured none
		// stores NULL rather than a zero.
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map_m95", ServerID: "srv_m95", CreatedAt: now.Add(time.Minute),
			Kind: "speech", TTFTMS: 850, WallMS: 1234, RTF: 0.4125,
		}); err != nil {
			t.Fatalf("insert speech run: %v", err)
		}
		if err := s.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: "map_m95", ServerID: "srv_m95", CreatedAt: now.Add(2 * time.Minute),
			Kind: "speech", Error: "synthesis failed",
		}); err != nil {
			t.Fatalf("insert failed speech run: %v", err)
		}
		runs, err = s.BenchmarkRunsByMapping(ctx, "map_m95", 10)
		if err != nil || len(runs) != 3 {
			t.Fatalf("read history: %d rows, err %v", len(runs), err)
		}
		// Newest first: the failed speech row, the measured speech row, the old speed row.
		if f := runs[0]; f.Kind != "speech" || f.Error == "" || f.TTFTMS != 0 || f.WallMS != 0 || f.RTF != 0 {
			t.Fatalf("failed speech row = %+v, want kind=speech with its error and no metrics", f)
		}
		if m := runs[1]; m.Kind != "speech" || m.TTFTMS != 850 || m.WallMS != 1234 || m.RTF != 0.4125 {
			t.Fatalf("measured speech row = %+v, want ttft 850, wall 1234, rtf 0.4125", m)
		}
		var nulls int
		if err := s.db.QueryRowContext(ctx, `select count(*) from model_mapping_benchmarks where kind = 'speech' and error <> '' and ttft_ms is null and wall_ms is null and rtf is null`).Scan(&nulls); err != nil || nulls != 1 {
			t.Fatalf("a failed speech row must store NULL metrics: count = %d, err %v", nulls, err)
		}

		// A replay (a re-run over a database that has the columns) neither errors
		// nor disturbs a stored value.
		reinvokeMigration95(ctx, t, s)
		again, err := s.BenchmarkRunsByMapping(ctx, "map_m95", 10)
		if err != nil || len(again) != 3 || again[1].WallMS != 1234 || again[1].RTF != 0.4125 {
			t.Fatalf("replaying migration 95 disturbed the history: %+v err %v", again, err)
		}
	})
}

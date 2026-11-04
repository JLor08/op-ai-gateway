// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration101 runs migration101Up in its own transaction, exactly as
// the ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration101(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration101Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration101Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// seedBaselineMapping inserts a model_mappings row by raw SQL naming only the
// seven columns the frozen-at-v60 baseline declares with no DDL default (id,
// application_id, gateway_model_name, app_model_name, status, created_at,
// updated_at). Every column added since arrives nullable or `not null default`,
// so the insert is complete at the baseline and at every version above it --
// version-independent in both directions, and equivalent to a CreateMapping of
// the same struct with every metric at its zero value.
//
// It exists because CreateMapping speaks the column set of the CURRENT
// migration head, and since migration 101 that set names the audio metric
// columns: a test seeding a mapping on a database stopped below 101 (the
// migration-78/79 backfill tests and the per-migration tests that need a
// mapping as an FK parent) would fail with "no such column". It is the
// model_mappings half of the coupling seedMigration78Mappings already broke for
// applications at migration 80.
func seedBaselineMapping(ctx context.Context, t *testing.T, s *SQLStore, m routing.ModelMapping) {
	t.Helper()
	mustExec(ctx, t, s, `insert into model_mappings (
		id, application_id, gateway_model_name, app_model_name, status, created_at, updated_at
	) values (?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.ApplicationID, m.GatewayModelName, m.AppModelName, m.Status, m.CreatedAt, m.UpdatedAt)
}

// migration101Columns is every column migration 101 adds to model_mappings.
var migration101Columns = []string{
	"audio_rate_x", "audio_ttfa_ms", "audio_realtime_concurrency", "audio_concurrency_limit", "audio_metrics_updated_at",
}

// migration101Precedents maps the three audio columns that have a sibling of
// the same shape on model_mappings to that sibling, compared within one
// dialect: the real-time concurrency like max_concurrency (a concurrency
// count), the concurrency limit like metrics_source (a short vocabulary string,
// empty = unknown) and the audio stamp like metrics_updated_at -- the timestamp
// type the table already uses for its metrics stamp, nullable, no default.
var migration101Precedents = map[string]string{
	"audio_realtime_concurrency": "max_concurrency",
	"audio_concurrency_limit":    "metrics_source",
	"audio_metrics_updated_at":   "metrics_updated_at",
}

// migration101Literals pins the two wide columns, which have no sibling of
// their shape on the table (the text metrics are all `not null default 0`).
// They are declared exactly like migration 95's audio columns on
// model_mapping_benchmarks -- ADR-005's double precision for the factor (a Go
// float64) and bigint for the milliseconds (a Go int64), NULLABLE so "never
// measured" stays distinguishable -- and both read back verbatim from both
// dialects.
var migration101Literals = map[string]string{
	"audio_rate_x":  "double precision null (no default)",
	"audio_ttfa_ms": "bigint null (no default)",
}

// assertMigration101Columns pins the shape of the five audio metric columns on
// a migrated database.
func assertMigration101Columns(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	decls := tableColumnDecls(ctx, t, s, "model_mappings")
	for col, precedent := range migration101Precedents {
		got, ok := decls[col]
		if !ok {
			t.Fatalf("model_mappings has no %s column after migration 101; got %v", col, sortedColumnNames(decls))
		}
		if want := decls[precedent]; got != want {
			t.Fatalf("model_mappings.%s is declared %q, want %q (declared like %s)", col, got, want, precedent)
		}
	}
	for col, want := range migration101Literals {
		got, ok := decls[col]
		if !ok {
			t.Fatalf("model_mappings has no %s column after migration 101; got %v", col, sortedColumnNames(decls))
		}
		if got != want {
			t.Fatalf("model_mappings.%s is declared %q, want %q (ADR-005 wide type, nullable, no default)", col, got, want)
		}
	}
}

// seedMigration101Mapping writes the FK chain and an audio mapping row that
// names none of migration 101's columns but carries a measured load time and
// its provenance, so the checks below see that the new columns arrive without
// disturbing the text metrics. The mapping is raw SQL because CreateMapping
// names the audio columns, which a database stopped at version 100 lacks.
func seedMigration101Mapping(ctx context.Context, t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_m101", Name: "m101", Provider: routing.ProviderAudioCpp,
		Endpoint: "http://srv_m101:8000",
		Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{
		ID: "app_m101", ServerID: "srv_m101", Type: routing.ProviderAudioCpp, Port: 8101, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	seedBaselineMapping(ctx, t, s, routing.ModelMapping{
		ID: "map_m101", ApplicationID: "app_m101", GatewayModelName: "m101-tts", AppModelName: "up-m101",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	})
	mustExec(ctx, t, s, `update model_mappings set load_time_ms = ?, metrics_source = ?, metrics_updated_at = ? where id = ?`,
		1234, "benchmark", now, "map_m101")
}

// assertMigration101Defaults reads the seeded mapping back twice -- raw, so the
// DDL defaults themselves are checked (NULL for the two wide columns and the
// stamp, 0 and the empty string for the other two), and through the store,
// which reads every one of them as "unknown" -- and requires the seeded text
// metric and its provenance untouched.
func assertMigration101Defaults(ctx context.Context, t *testing.T, s *SQLStore, seededAt time.Time) {
	t.Helper()
	var rate sql.NullFloat64
	var ttfa sql.NullInt64
	var realtime int64
	var limit string
	var stamp sql.NullTime
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select audio_rate_x, audio_ttfa_ms, audio_realtime_concurrency,
		audio_concurrency_limit, audio_metrics_updated_at from model_mappings where id = ?`), "map_m101").Scan(
		&rate, &ttfa, &realtime, &limit, &stamp); err != nil {
		t.Fatalf("read the seeded mapping's audio columns: %v", err)
	}
	if rate.Valid || ttfa.Valid || stamp.Valid || realtime != 0 || limit != "" {
		t.Fatalf("seeded mapping reads rate=%v ttfa=%v realtime=%d limit=%q stamp=%v, want NULL, NULL, 0, '' and NULL (a backfilled zero would claim a measurement)",
			rate, ttfa, realtime, limit, stamp)
	}
	got, err := s.MappingByID(ctx, "map_m101")
	if err != nil {
		t.Fatalf("read the seeded mapping through the store: %v", err)
	}
	if got.AudioRateX != 0 || got.AudioTTFAMS != 0 || got.AudioRealtimeConcurrency != 0 || got.AudioConcurrencyLimit != "" || got.AudioMetricsUpdatedAt != nil {
		t.Fatalf("seeded mapping must read back with every audio metric unknown, got %+v", got)
	}
	if got.LoadTimeMS != 1234 || got.MetricsSource != "benchmark" || got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(seededAt) {
		t.Fatalf("seeded mapping's text metric and provenance must read back untouched, got %+v", got)
	}
}

// TestMigration101AddsAudioMetricColumns pins migration 101 (#340: the audio
// benchmark's figures, stored on the mapping) on a fresh install and on an
// upgrade of a genuine version-100 database that already holds an audio
// mapping, on both dialects: the five audio metric columns exist on
// model_mappings, the wide pair declared with ADR-005's types and nullable,
// the rest like their text-metric siblings; a mapping written before (or
// without naming) them reads every one as unknown, with no backfill; and on
// the upgrade the columns are absent before the migration, writable through
// UpdateMappingAudioMetrics after it, and a replay of the migration disturbs
// nothing.
func TestMigration101AddsAudioMetricColumns(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			assertMigration101Columns(ctx, t, s)
			seedMigration101Mapping(ctx, t, s, now)
			assertMigration101Defaults(ctx, t, s, now)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 100, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			before := tableColumnDecls(ctx, t, s, "model_mappings")
			for _, c := range migration101Columns {
				if _, ok := before[c]; ok {
					t.Fatalf("model_mappings already has %s at v100", c)
				}
			}
			seedMigration101Mapping(ctx, t, s, now)

			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration101Columns(ctx, t, s)
			assertMigration101Defaults(ctx, t, s, now)

			// The columns are writable through the store, and a replay of the
			// migration neither errors nor disturbs a stored value.
			if err := s.UpdateMappingAudioMetrics(ctx, "map_m101", routing.AudioMetrics{
				RateX: 3.25, TTFAMS: 420, RealtimeConcurrency: 2, ConcurrencyLimit: routing.AudioLimitPerformance,
			}, "audio_benchmark", now); err != nil {
				t.Fatalf("UpdateMappingAudioMetrics: %v", err)
			}
			reinvokeMigration101(ctx, t, s)
			again, err := s.MappingByID(ctx, "map_m101")
			if err != nil {
				t.Fatalf("read after replay: %v", err)
			}
			if again.AudioRateX != 3.25 || again.AudioTTFAMS != 420 || again.AudioRealtimeConcurrency != 2 ||
				again.AudioConcurrencyLimit != routing.AudioLimitPerformance || again.AudioMetricsUpdatedAt == nil || !again.AudioMetricsUpdatedAt.Equal(now) {
				t.Fatalf("replaying migration 101 disturbed a stored value: %+v", again)
			}
		})
	})
}

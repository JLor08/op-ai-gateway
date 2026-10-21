// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// reinvokeMigration94 runs migration94Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration94(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration94Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration94Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration94CreatesModelMappingCache runs migration 94 against a genuine
// version-93 database that already holds a mapping, and pins what an upgrade
// of a deployment with existing mappings depends on (#251): the table does not
// exist before the migration and exists after it on BOTH dialects, every
// column is declared with the shape the store's Go values need (ADR-005:
// payload is the unbounded `text`, updated_at is the dialect's timestamp, the
// two key strings default to the empty string so a row never needs NULL), the
// pre-existing mapping is untouched and reads an empty cache, the composite
// key and the cascading foreign key behave as declared, the lookup index is
// there, and a replay of the migration changes nothing (it is
// duplicate-tolerant).
func TestMigration94CreatesModelMappingCache(t *testing.T) {
	forEachDialectMigratedTo(t, 93, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

		var before int
		countTables := `select count(*) from sqlite_master where type = 'table' and name = 'model_mapping_cache'`
		if s.dl.name() == "postgres" {
			countTables = `select count(*) from information_schema.tables where table_schema = 'public' and table_name = 'model_mapping_cache'`
		}
		if err := s.db.QueryRowContext(ctx, countTables).Scan(&before); err != nil {
			t.Fatalf("look up model_mapping_cache at v93: %v", err)
		}
		if before != 0 {
			t.Fatalf("model_mapping_cache already exists at v93")
		}

		// A mapping written before the table existed: the parent the new table
		// has to hang off after the upgrade.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m94", Name: "m94", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m94:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m94", ServerID: "srv_m94", Type: routing.ProviderServerAgent, Port: 8094, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create application: %v", err)
		}
		if err := s.CreateMapping(ctx, routing.ModelMapping{
			ID: "map_m94", ApplicationID: "app_m94", GatewayModelName: "m94-model", AppModelName: "up-m94",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create mapping: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		wantTS := "timestamp"
		if s.dl.name() == "postgres" {
			// information_schema spells timestamptz out in full.
			wantTS = "timestamp with time zone"
		}
		decls := tableColumnDecls(ctx, t, s, "model_mapping_cache")
		for col, wantType := range map[string]string{
			"mapping_id": "text",
			"cache_type": "text",
			"scope_key":  "text",
			"payload":    "text",
			"updated_at": wantTS,
		} {
			raw, ok := decls[col]
			if !ok {
				t.Errorf("model_mapping_cache lacks column %s: %v", col, sortedColumnNames(decls))
				continue
			}
			d := strings.ToLower(raw)
			if !strings.HasPrefix(d, wantType+" ") {
				t.Errorf("model_mapping_cache.%s = %q, want type %q", col, raw, wantType)
			}
			if !strings.Contains(d, "not null") {
				t.Errorf("model_mapping_cache.%s = %q, want NOT NULL", col, raw)
			}
		}
		// The two key strings that may legitimately be empty default to the
		// empty string, so no row ever needs NULL (and a read never has to).
		for _, col := range []string{"scope_key", "payload"} {
			if d := decls[col]; !strings.Contains(d, "default ''") {
				t.Errorf("model_mapping_cache.%s = %q, want an empty-string default", col, decls[col])
			}
		}
		if len(decls) != 5 {
			t.Errorf("model_mapping_cache has %d columns %v, want exactly 5", len(decls), sortedColumnNames(decls))
		}

		indexQuery := `select count(*) from sqlite_master where type = 'index' and name = ?`
		if s.dl.name() == "postgres" {
			indexQuery = `select count(*) from pg_indexes where schemaname = 'public' and indexname = ?`
		}
		var n int
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(indexQuery), "idx_model_mapping_cache_mapping").Scan(&n); err != nil {
			t.Fatalf("look up idx_model_mapping_cache_mapping: %v", err)
		}
		if n != 1 {
			t.Errorf("idx_model_mapping_cache_mapping count = %d, want 1", n)
		}

		// The pre-existing mapping is untouched and reads an empty cache.
		mapping, err := s.MappingByID(ctx, "map_m94")
		if err != nil || mapping.AppModelName != "up-m94" {
			t.Fatalf("pre-migration mapping = %+v err=%v, want it untouched", mapping, err)
		}
		if rows, err := s.ModelMappingCache(ctx, "map_m94", "voices"); err != nil || len(rows) != 0 {
			t.Fatalf("fresh cache for the pre-migration mapping = %+v err=%v, want empty", rows, err)
		}

		// The composite key (mapping_id, cache_type, scope_key) makes a second
		// write for the same key an in-place replace and distinct scope keys
		// distinct rows -- driven through the store, which is what relies on it.
		for i, scope := range []string{"", "", "de"} {
			if err := s.UpsertModelMappingCache(ctx, routing.ModelMappingCacheRow{
				MappingID: "map_m94", CacheType: "voices", ScopeKey: scope,
				Payload: strings.Repeat("v", 70_000) + string(rune('a'+i)), UpdatedAt: now.Add(time.Duration(i) * time.Minute),
			}); err != nil {
				t.Fatalf("upsert #%d: %v", i, err)
			}
		}
		rows, err := s.ModelMappingCache(ctx, "map_m94", "voices")
		if err != nil || len(rows) != 2 {
			t.Fatalf("rows after three upserts over two keys = %+v err=%v, want 2", rows, err)
		}
		// A 70 kB payload must round-trip whole (text is the wide column).
		if len(rows[0].Payload) != 70_001 || !strings.HasSuffix(rows[0].Payload, "b") {
			t.Fatalf("default-scope payload is %d bytes, want the second write's 70001 bytes ending in b", len(rows[0].Payload))
		}

		// A replay of the migration (a re-run over a database that has it)
		// neither errors nor disturbs the rows already stored.
		reinvokeMigration94(ctx, t, s)
		again, err := s.ModelMappingCache(ctx, "map_m94", "voices")
		if err != nil || len(again) != 2 {
			t.Fatalf("rows after replaying migration 94 = %+v err=%v, want the same 2", again, err)
		}

		// The cascading foreign key: a row for a missing mapping is refused,
		// and deleting the mapping removes its rows.
		insert := s.dl.rebind(`insert into model_mapping_cache (mapping_id, cache_type, scope_key, payload, updated_at) values (?, ?, ?, ?, ?)`)
		if _, err := s.db.ExecContext(ctx, insert, "map_missing", "voices", "", "{}", now); err == nil {
			t.Fatal("insert for a missing mapping succeeded; mapping_id must reference model_mappings(id)")
		} else if !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("insert for a missing mapping = %v, want a foreign-key violation", err)
		}
		if err := s.DeleteMapping(ctx, "map_m94"); err != nil {
			t.Fatalf("delete mapping: %v", err)
		}
		var left int
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from model_mapping_cache where mapping_id = ?`), "map_m94").Scan(&left); err != nil {
			t.Fatalf("count cache rows: %v", err)
		}
		if left != 0 {
			t.Fatalf("model_mapping_cache rows after deleting the mapping = %d, want 0 (ON DELETE CASCADE)", left)
		}
	})
}

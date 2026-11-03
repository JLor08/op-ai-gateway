// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration102 runs migration102Up in its own transaction, exactly as
// the ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration102(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration102Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration102Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// migration102Key is the affinity key of the pin the migration-102 tests seed.
var migration102Key = routing.AffinityKey{
	APITokenID: "tok_m102", Model: "m102-model", APIFlavor: routing.APIFlavorOpenAI, SessionID: "sess_m102",
}

// assertMigration102Column pins route_affinity.instance on a migrated database:
// declared exactly like agent_runtime_specs.max_instances (migration 100), the
// instance range it indexes into, compared within one dialect --
// `integer not null default 0`, where 0 is instance 0, the address every pin
// meant before the column existed.
func assertMigration102Column(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	decls := tableColumnDecls(ctx, t, s, "route_affinity")
	got, ok := decls["instance"]
	if !ok {
		t.Fatalf("route_affinity has no instance column after migration 102; got %v", sortedColumnNames(decls))
	}
	if want := tableColumnDecls(ctx, t, s, "agent_runtime_specs")["max_instances"]; got != want {
		t.Fatalf("route_affinity.instance is declared %q, want %q (declared like agent_runtime_specs.max_instances)", got, want)
	}
}

// seedMigration102Pin writes the FK chain and a token-affinity pin that names
// no instance. The pin is raw SQL on purpose: UpsertAffinity speaks the head's
// column set, which a database stopped at version 101 does not have yet.
func seedMigration102Pin(ctx context.Context, t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	if err := s.CreateUser(ctx, newTestUser("u_m102", "m102@example.test", now)); err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := s.CreatePlainToken(ctx, TokenRecord{ID: "tok_m102", UserID: "u_m102", Name: "m102", CreatedAt: now, UpdatedAt: now}, "m102-affinity-secret"); err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := s.CreateAIServer(ctx, routing.AIServer{
		ID: "srv_m102", Name: "m102", Provider: routing.ProviderVLLM,
		Endpoint: "http://srv_m102:8000",
		Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := s.CreateApplication(ctx, routing.Application{
		ID: "app_m102", ServerID: "srv_m102", Type: routing.ProviderServerAgent, Port: 8102, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	mustExec(ctx, t, s, `insert into route_affinity (
		id, api_token_id, user_id, model, api_flavor, session_id,
		application_id, server_id, expires_at, last_used_at, created_at, updated_at, resolved_model
	) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"aff_m102", migration102Key.APITokenID, "u_m102", migration102Key.Model, migration102Key.APIFlavor, migration102Key.SessionID,
		"app_m102", "srv_m102", now.Add(time.Hour), now, now, now, "m102-resolved")
}

// assertMigration102Default reads the seeded pin back twice -- raw, so the DDL
// default itself is checked, and through the store -- and requires instance 0
// with the rest of the pin untouched: a pin from before the column existed
// keeps meaning exactly what it meant.
func assertMigration102Default(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	var instance int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select instance from route_affinity where id = ?`), "aff_m102").Scan(&instance); err != nil {
		t.Fatalf("read the seeded pin's instance: %v", err)
	}
	if instance != 0 {
		t.Fatalf("seeded pin reads instance %d, want 0 (the DDL default)", instance)
	}
	got, ok, err := s.Affinity(ctx, migration102Key)
	if err != nil || !ok {
		t.Fatalf("read the seeded pin through the store: ok=%v err=%v", ok, err)
	}
	if got.Instance != 0 || got.ServerID != "srv_m102" || got.ApplicationID != "app_m102" || got.ResolvedModel != "m102-resolved" {
		t.Fatalf("seeded pin must read back as instance 0 with everything else untouched, got %+v", got)
	}
}

// TestMigration102AddsAffinityInstance pins migration 102 (#340, design
// section 10.2) on a fresh install and on an upgrade of a genuine version-101
// database that already holds a token-affinity pin, on both dialects:
// route_affinity gains `instance`, declared like the spec instance counts it
// indexes, and a pin written before (or without naming) it reads instance 0.
// On the upgrade the column is also absent before the migration, writable
// through UpsertAffinity after it, and a replay of the migration disturbs
// nothing.
func TestMigration102AddsAffinityInstance(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	t.Run("fresh", func(t *testing.T) {
		forEachDialect(t, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			assertMigration102Column(ctx, t, s)
			seedMigration102Pin(ctx, t, s, now)
			assertMigration102Default(ctx, t, s)
		})
	})

	t.Run("upgraded", func(t *testing.T) {
		forEachDialectMigratedTo(t, 101, func(t *testing.T, s *SQLStore) {
			ctx := context.Background()
			if _, ok := tableColumnDecls(ctx, t, s, "route_affinity")["instance"]; ok {
				t.Fatalf("route_affinity already has instance at v101")
			}
			seedMigration102Pin(ctx, t, s, now)

			if err := s.Migrate(ctx); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertMigration102Column(ctx, t, s)
			assertMigration102Default(ctx, t, s)

			// The column is writable through the store (a re-pin of the same key
			// moves it), and a replay of the migration disturbs nothing.
			pin, _, err := s.Affinity(ctx, migration102Key)
			if err != nil {
				t.Fatalf("read pin: %v", err)
			}
			pin.Instance = 3
			pin.UpdatedAt = now.Add(time.Minute)
			if err := s.UpsertAffinity(ctx, pin); err != nil {
				t.Fatalf("re-pin to instance 3: %v", err)
			}
			reinvokeMigration102(ctx, t, s)
			again, ok, err := s.Affinity(ctx, migration102Key)
			if err != nil || !ok {
				t.Fatalf("read after replay: ok=%v err=%v", ok, err)
			}
			if again.Instance != 3 {
				t.Fatalf("replaying migration 102 disturbed the pinned instance: %+v, want instance 3", again)
			}
		})
	})
}

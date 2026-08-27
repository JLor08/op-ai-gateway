// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// indexExists reports whether a named index exists, on either dialect. The
// existing sqliteIndexExists only knows sqlite_master; migration 68's index
// must be asserted on postgres too (it is the leg that catches dialect-specific
// DDL, see the CI comment on the postgres service).
func indexExists(t *testing.T, s *SQLStore, name string) bool {
	t.Helper()
	q := `select count(*) from sqlite_master where type = 'index' and name = ?`
	if s.dl.name() == "postgres" {
		q = `select count(*) from pg_indexes where indexname = ?`
	}
	var count int
	if err := s.db.QueryRow(s.dl.rebind(q), name).Scan(&count); err != nil {
		t.Fatalf("index lookup (%s): %v", s.dl.name(), err)
	}
	return count == 1
}

// seedServerForSingleAgentTest creates one ai_servers row the applications
// under test can hang off.
func seedServerForSingleAgentTest(t *testing.T, s *SQLStore, id string, now time.Time) {
	t.Helper()
	if err := s.CreateAIServer(context.Background(), routing.AIServer{
		ID: id, Name: id, Domain: id + ".local", Provider: routing.ProviderVLLM,
		Endpoint: "http://" + id + ".local:8000", Status: routing.ServerStatusActive,
		HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server %s: %v", id, err)
	}
}

func serverAgentApp(id, serverID string, port int, now time.Time) routing.Application {
	return routing.Application{
		ID: id, ServerID: serverID, Type: routing.ProviderServerAgent, Port: port, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
		CreatedAt:       now, UpdatedAt: now,
	}
}

// TestConformanceSingleServerAgentPartialUniqueIndex proves migration 68's
// partial unique index is created on BOTH dialects and actually constrains what
// it claims to: a second server_agent application on the same server is
// rejected (on a DIFFERENT port, so the pre-existing unique(server_id, port)
// constraint cannot be what fires), while a second server_agent application on
// a different server, and a non-server_agent application on the same server,
// are both accepted. The last two are what makes the index PARTIAL rather than
// a plain unique(server_id) — without them a test could pass against an index
// that is simply too broad.
func TestConformanceSingleServerAgentPartialUniqueIndex(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if !indexExists(t, s, "idx_applications_single_server_agent") {
			t.Fatalf("migration 68 index missing on %s", s.dl.name())
		}
		seedServerForSingleAgentTest(t, s, "srv1", now)
		seedServerForSingleAgentTest(t, s, "srv2", now)

		if err := s.CreateApplication(ctx, serverAgentApp("app_agent1", "srv1", 9000, now)); err != nil {
			t.Fatalf("first server_agent application: %v", err)
		}
		if err := s.CreateApplication(ctx, serverAgentApp("app_agent2", "srv1", 9001, now)); err != ErrConflict {
			t.Fatalf("second server_agent application on srv1: err = %v, want ErrConflict", err)
		}
		// Different server: allowed (the invariant is per-server).
		if err := s.CreateApplication(ctx, serverAgentApp("app_agent3", "srv2", 9000, now)); err != nil {
			t.Fatalf("server_agent application on srv2: %v, want success", err)
		}
		// Same server, different type: allowed (the index is partial).
		plain := serverAgentApp("app_plain", "srv1", 8000, now)
		plain.Type = routing.ProviderVLLM
		if err := s.CreateApplication(ctx, plain); err != nil {
			t.Fatalf("vllm application on srv1: %v, want success", err)
		}
		// Retyping that application to server_agent must be refused too — the
		// UPDATE path is covered by the same index.
		plain.Type = routing.ProviderServerAgent
		plain.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateApplication(ctx, plain); err != ErrConflict {
			t.Fatalf("retype to server_agent via update: err = %v, want ErrConflict", err)
		}
	})
}

// TestConformanceSingleServerAgentMigrationToleratesExistingDuplicates pins the
// deliberate "cannot fail a migration on live data" property of migration 68:
// on a database that already holds two server_agent applications for one server
// (only reachable on a pre-invariant development database — the type is not
// writable in any released version), the migration must SKIP the index instead
// of aborting and refusing to start the gateway.
//
// It reconstructs that state by migrating fully, dropping the index, inserting
// the duplicate, un-recording version 68, and re-running Migrate.
func TestConformanceSingleServerAgentMigrationToleratesExistingDuplicates(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		seedServerForSingleAgentTest(t, s, "srv1", now)
		if err := s.CreateApplication(ctx, serverAgentApp("app_agent1", "srv1", 9000, now)); err != nil {
			t.Fatalf("first server_agent application: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, `drop index idx_applications_single_server_agent`); err != nil {
			t.Fatalf("drop index: %v", err)
		}
		if err := s.CreateApplication(ctx, serverAgentApp("app_agent2", "srv1", 9001, now)); err != nil {
			t.Fatalf("duplicate server_agent application after dropping the index: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from schema_migrations where version = ?`), 68); err != nil {
			t.Fatalf("un-record migration 68: %v", err)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate over existing duplicates must not fail, got %v", err)
		}
		if indexExists(t, s, "idx_applications_single_server_agent") {
			t.Fatalf("index was created despite existing duplicates on %s", s.dl.name())
		}
		// The skip is recorded, so the next boot does not retry it.
		var recorded int
		if err := s.db.QueryRow(s.dl.rebind(`select count(*) from schema_migrations where version = ?`), 68).Scan(&recorded); err != nil {
			t.Fatalf("read schema_migrations: %v", err)
		}
		if recorded != 1 {
			t.Fatalf("schema_migrations rows for version 68 = %d, want 1", recorded)
		}
	})
}

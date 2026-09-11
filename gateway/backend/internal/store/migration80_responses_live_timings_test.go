// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// reinvokeMigration80 re-runs migration80Up in its own tx after seeding rows,
// mirroring reinvokeMigration72: the ADD COLUMNs are duplicate-tolerant and
// the backfill UPDATE is guarded on the target column still being 0, so a
// replay is a no-op on an already-backfilled row.
func reinvokeMigration80(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration80Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration80Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// specLiveTimings reads the raw column for one spec id. Raw SQL on purpose:
// this task adds no Go struct field, so the column has no other reader yet.
func specLiveTimings(ctx context.Context, t *testing.T, s *SQLStore, specID string) int64 {
	t.Helper()
	var v int64
	q := `select responses_live_timings_enabled from agent_runtime_specs where id = ?`
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(q), specID).Scan(&v); err != nil {
		t.Fatalf("read spec column: %v", err)
	}
	return v
}

// TestMigration80AddsTheColumnToBothTablesWithThePrecedentType proves the
// column exists on BOTH tables on BOTH dialects after a plain Migrate, and
// that its whole declared shape -- type, nullability AND default, not the
// type alone -- matches an existing integer boolean on the same table.
// Comparing against a sibling column rather than a literal keeps the
// assertion dialect-independent while still catching ADR-005's
// narrow-vs-wide hazard, which is live here because the column lands on a
// table whose baseline CREATE is frozen. The nullability and default halves
// are the ones that catch a plain `integer`: same data_type, but it would
// hand every existing row NULL instead of 0 and change how every later scan
// of the column behaves.
func TestMigration80AddsTheColumnToBothTablesWithThePrecedentType(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		appCols := tableColumnDecls(ctx, t, s, "applications")
		got, ok := appCols["responses_live_timings_enabled"]
		if !ok {
			t.Fatalf("applications has no responses_live_timings_enabled column; got %v", sortedColumnNames(appCols))
		}
		if want := appCols["opportunistic_metrics_enabled"]; got != want {
			t.Fatalf("applications.responses_live_timings_enabled is declared %q, want %q (the precedent integer boolean on this table)", got, want)
		}
		specCols := tableColumnDecls(ctx, t, s, "agent_runtime_specs")
		gotSpec, ok := specCols["responses_live_timings_enabled"]
		if !ok {
			t.Fatalf("agent_runtime_specs has no responses_live_timings_enabled column; got %v", sortedColumnNames(specCols))
		}
		if want := specCols["enabled"]; gotSpec != want {
			t.Fatalf("agent_runtime_specs.responses_live_timings_enabled is declared %q, want %q", gotSpec, want)
		}
	})
}

// TestMigration80SnapshotsTheSpecFromItsParentApplication proves the
// spec-from-parent-application join, in whichever dialect's SQL is under test.
//
// Why this test has to seed a non-zero application value by hand: on a real
// upgrade BOTH columns are new and default 0, so the backfill is provably a
// no-op on every existing row. That is exactly the decided requirement (an
// upgrade changes no deployment's behaviour) -- and it is also why a wrong
// join would never be noticed without this fixture.
//
// Why the fixture needs TWO applications, one on and one off: with a single
// application row, every join path that reaches `applications` at all yields
// that row's value, so the only join bug this test could catch would be the
// no-match kind. Drop the correlation instead of breaking it -- `where m.id =
// s.mapping_id` from the postgres branch, `where m.id =
// agent_runtime_specs.mapping_id` from the sqlite subquery -- and postgres
// updates every spec from an arbitrary matching source row while sqlite's
// now-uncorrelated subquery returns the first one. Both would still set
// spec_m80 to 1. The second application, off, is what makes that observable:
// spec_m80_off must come out of the backfill still 0, so an uncorrelated join
// would have to pick the right source row for each target BY ACCIDENT to stay
// green -- and in practice it picks one row for all of them, so one of the two
// assertions fires whichever way it lands (measured: both dialects set
// spec_m80_off to 1). This matters because a shipped migration is append-only
// -- a wrong join found after release cannot be edited in place, it needs
// migration 81.
func TestMigration80SnapshotsTheSpecFromItsParentApplication(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		// Two server_agent applications means two AI servers: migration68Up's
		// partial unique index allows only one server_agent application per
		// server_id.
		for _, host := range []struct {
			serverID, appID string
			port            int
		}{{"srv_m80", "app_m80", 8091}, {"srv_m80_off", "app_m80_off", 8092}} {
			if err := s.CreateAIServer(ctx, routing.AIServer{
				ID: host.serverID, Name: host.serverID, Provider: routing.ProviderVLLM,
				Endpoint: "http://" + host.serverID + ":8000",
				Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create server %s: %v", host.serverID, err)
			}
			if err := s.CreateApplication(ctx, routing.Application{
				ID: host.appID, ServerID: host.serverID, Type: routing.ProviderServerAgent, Port: host.port, Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create app %s: %v", host.appID, err)
			}
		}
		// One mapping per spec, because agent_runtime_specs.mapping_id is
		// unique (migration65Up's create-table).
		for _, row := range []struct{ specID, mappingID, appID string }{
			{"spec_m80", "map_m80", "app_m80"},
			{"spec_m80_on", "map_m80_b", "app_m80"},
			{"spec_m80_off", "map_m80_off", "app_m80_off"},
		} {
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: row.mappingID, ApplicationID: row.appID,
				GatewayModelName: "m80-model-" + row.mappingID, AppModelName: "up-" + row.mappingID,
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %s: %v", row.mappingID, err)
			}
			if err := s.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
				ID: row.specID, MappingID: row.mappingID, Enabled: true, Binary: "/usr/bin/llama-server",
				Args: "[]", Env: "{}", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("upsert spec %s: %v", row.specID, err)
			}
		}
		// The pre-80 shape the backfill exists for: app_m80 is ON and its
		// first spec is still 0, so the snapshot has something to do.
		// app_m80_off is OFF and so is its spec -- that pair is the
		// wrong-parent control -- and the explicit zeroes are written rather
		// than left to the DDL default so the fixture states the whole
		// starting state.
		//
		// spec_m80_on starts at 1, and an earlier version of this comment
		// claimed that row is what protects the `= 0` idempotency guard. It is
		// not. Measured by dropping the guard from BOTH dialects and running
		// this whole package with the PostgreSQL DSN set: the only assertion
		// that fails is the REPLAY one below, on sqlite and on postgres alike.
		// spec_m80_on's own assertion passes unguarded, because its parent
		// (app_m80) is also 1 at the first invocation, so an unguarded
		// backfill writes that same 1 straight back -- which makes it a
		// coherence check, satisfied by the guard and by an unguarded write
		// alike, rather than a pin. What holds the guard in place is the replay
		// below, where app_m80 has been turned OFF first and only the guard
		// keeps spec_m80 at 1.
		mustExec(ctx, t, s, `update applications set responses_live_timings_enabled = 1 where id = ?`, "app_m80")
		mustExec(ctx, t, s, `update applications set responses_live_timings_enabled = 0 where id = ?`, "app_m80_off")
		mustExec(ctx, t, s, `update agent_runtime_specs set responses_live_timings_enabled = 0 where id = ?`, "spec_m80")
		mustExec(ctx, t, s, `update agent_runtime_specs set responses_live_timings_enabled = 1 where id = ?`, "spec_m80_on")
		mustExec(ctx, t, s, `update agent_runtime_specs set responses_live_timings_enabled = 0 where id = ?`, "spec_m80_off")

		reinvokeMigration80(ctx, t, s)

		if got := specLiveTimings(ctx, t, s, "spec_m80"); got != 1 {
			t.Fatalf("spec_m80 = %d, want 1 snapshotted from its parent application through mapping_id -> application_id", got)
		}
		if got := specLiveTimings(ctx, t, s, "spec_m80_on"); got != 1 {
			t.Fatalf("spec_m80_on = %d, want 1 left untouched", got)
		}
		if got := specLiveTimings(ctx, t, s, "spec_m80_off"); got != 0 {
			t.Fatalf("spec_m80_off = %d, want 0: its OWN parent application (app_m80_off) is off, so a spec that came out on means the backfill read some other application's row", got)
		}

		// Replay: a second invocation changes nothing, and an application
		// turned OFF after the first backfill does not drag its spec back.
		mustExec(ctx, t, s, `update applications set responses_live_timings_enabled = 0 where id = ?`, "app_m80")
		reinvokeMigration80(ctx, t, s)
		if got := specLiveTimings(ctx, t, s, "spec_m80"); got != 1 {
			t.Fatalf("after replay spec_m80 = %d, want 1: the backfill must be guarded on the column still being 0", got)
		}
		if got := specLiveTimings(ctx, t, s, "spec_m80_off"); got != 0 {
			t.Fatalf("after replay spec_m80_off = %d, want 0: every application is off by now, so nothing can legitimately turn it on", got)
		}
	})
}

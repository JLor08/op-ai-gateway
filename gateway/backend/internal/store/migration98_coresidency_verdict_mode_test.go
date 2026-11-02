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

// reinvokeMigration98 runs migration98Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration98(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration98Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration98Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// rawText reads one text cell by raw SQL. Raw on purpose: the migration tests
// pin the STORED value, independently of whichever Go accessor reads it.
func rawText(ctx context.Context, t *testing.T, s *SQLStore, q string, args ...any) string {
	t.Helper()
	var v string
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(q), args...).Scan(&v); err != nil {
		t.Fatalf("read %q: %v", q, err)
	}
	return v
}

// TestMigration98And99UpgradeAnExistingCoResidencyDeployment is the "no
// surprise" guarantee of the co-residency rework. It builds a genuine
// version-97 database -- the shape every deployed gateway has today: a
// default-deny allow-list of pairs and applications that know nothing about a
// mode -- migrates it to head, and pins what an operator's running servers
// depend on:
//
//   - every pre-existing pair row reads back verdict 'allow' (they WERE allowed
//     pairs; a row was the only way to say so), so the effective allow-list is
//     byte-for-byte what it was;
//   - every pre-existing application reads back coresidency_mode 'opt_in'
//     (present row -> allow, absent row -> deny: today's exact semantics), so
//     no running server changes behaviour until its operator flips the mode;
//   - an application created AFTER the upgrade is 'opt_out', the new standard.
//
// Both columns are text not null, on both dialects, and the verdict
// migration replays without disturbing a stored 'forbid'.
func TestMigration98And99UpgradeAnExistingCoResidencyDeployment(t *testing.T) {
	forEachDialectMigratedTo(t, 97, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

		before := tableColumnDecls(ctx, t, s, "agent_coresidency_rules")
		if _, ok := before["verdict"]; ok {
			t.Fatalf("agent_coresidency_rules already has verdict at v97")
		}
		beforeApps := tableColumnDecls(ctx, t, s, "applications")
		if _, ok := beforeApps["coresidency_mode"]; ok {
			t.Fatalf("applications already has coresidency_mode at v97")
		}

		// Two server_agent applications (two servers: migration68Up allows one
		// server_agent application per server) and a plain upstream
		// application, all written before the columns exist.
		for _, host := range []struct {
			serverID, appID, appType string
			port                     int
		}{
			{"srv_m98", "app_m98", routing.ProviderServerAgent, 8098},
			{"srv_m98b", "app_m98b", routing.ProviderServerAgent, 8099},
			{"srv_m98c", "app_m98c", routing.ProviderVLLM, 8100},
		} {
			if err := s.CreateAIServer(ctx, routing.AIServer{
				ID: host.serverID, Name: host.serverID, Provider: routing.ProviderVLLM,
				Endpoint: "http://" + host.serverID + ":8000",
				Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create server %s: %v", host.serverID, err)
			}
			if err := s.CreateApplication(ctx, routing.Application{
				ID: host.appID, ServerID: host.serverID, Type: host.appType, Port: host.port, Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create application %s: %v", host.appID, err)
			}
		}
		for _, mid := range []string{"map_m98_a", "map_m98_b", "map_m98_c"} {
			if err := s.CreateMapping(ctx, routing.ModelMapping{
				ID: mid, ApplicationID: "app_m98", GatewayModelName: mid + "-model", AppModelName: mid + "-up",
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping %s: %v", mid, err)
			}
		}
		// Two allowed pairs written before the verdict column existed. Raw SQL
		// on purpose: at v97 SetCoResidencyRules would name a column that is
		// not there yet, and the point is rows the new column is added UNDER.
		for _, pair := range [][2]string{{"map_m98_a", "map_m98_b"}, {"map_m98_a", "map_m98_c"}} {
			mustExec(ctx, t, s, `insert into agent_coresidency_rules (application_id, mapping_a_id, mapping_b_id, created_at) values (?, ?, ?, ?)`,
				"app_m98", pair[0], pair[1], now)
		}

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		// Shape: text, not null, on both dialects.
		after := tableColumnDecls(ctx, t, s, "agent_coresidency_rules")
		verdictDecl, ok := after["verdict"]
		if !ok {
			t.Fatalf("agent_coresidency_rules has no verdict column after migration 98; got %v", sortedColumnNames(after))
		}
		// sqlite reports the type upper-case (TEXT), postgres lower-case.
		if !strings.HasPrefix(strings.ToLower(verdictDecl), "text not null") {
			t.Fatalf("agent_coresidency_rules.verdict is declared %q, want a text not null column", verdictDecl)
		}
		afterApps := tableColumnDecls(ctx, t, s, "applications")
		modeDecl, ok := afterApps["coresidency_mode"]
		if !ok {
			t.Fatalf("applications has no coresidency_mode column after migration 99; got %v", sortedColumnNames(afterApps))
		}
		if !strings.HasPrefix(strings.ToLower(modeDecl), "text not null") {
			t.Fatalf("applications.coresidency_mode is declared %q, want a text not null column", modeDecl)
		}

		// Backfill: every pre-existing pair is an allow, and nothing was lost.
		var n int
		if err := s.db.QueryRowContext(ctx, `select count(*) from agent_coresidency_rules`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("pair rows after migration = %d (err %v), want the 2 pre-existing rows", n, err)
		}
		if err := s.db.QueryRowContext(ctx, `select count(*) from agent_coresidency_rules where verdict = 'allow'`).Scan(&n); err != nil || n != 2 {
			t.Fatalf("pre-existing rows with verdict 'allow' = %d (err %v), want both: they were allowed pairs", n, err)
		}

		// Backfill: every pre-existing application keeps today's semantics.
		for _, appID := range []string{"app_m98", "app_m98b", "app_m98c"} {
			if got := rawText(ctx, t, s, `select coresidency_mode from applications where id = ?`, appID); got != "opt_in" {
				t.Fatalf("pre-existing application %s coresidency_mode = %q, want opt_in (today's default-deny behaviour preserved)", appID, got)
			}
		}

		// New applications are opt_out.
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m98_new", Name: "srv_m98_new", Provider: routing.ProviderVLLM,
			Endpoint: "http://srv_m98_new:8000",
			Status:   routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create post-upgrade server: %v", err)
		}
		if err := s.CreateApplication(ctx, routing.Application{
			ID: "app_m98_new", ServerID: "srv_m98_new", Type: routing.ProviderServerAgent, Port: 8101, Scheme: "http",
			APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
			HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create post-upgrade application: %v", err)
		}
		if got := rawText(ctx, t, s, `select coresidency_mode from applications where id = ?`, "app_m98_new"); got != "opt_out" {
			t.Fatalf("post-upgrade application coresidency_mode = %q, want opt_out (the new standard)", got)
		}

		// Replay of migration 98 (a re-run over a database that has the column)
		// neither errors nor disturbs an operator's explicit 'forbid'.
		mustExec(ctx, t, s, `update agent_coresidency_rules set verdict = 'forbid' where mapping_b_id = ?`, "map_m98_c")
		reinvokeMigration98(ctx, t, s)
		if got := rawText(ctx, t, s, `select verdict from agent_coresidency_rules where mapping_b_id = ?`, "map_m98_c"); got != "forbid" {
			t.Fatalf("replaying migration 98 disturbed a stored verdict: got %q, want forbid", got)
		}
		if got := rawText(ctx, t, s, `select verdict from agent_coresidency_rules where mapping_b_id = ?`, "map_m98_b"); got != "allow" {
			t.Fatalf("replaying migration 98 disturbed the other pair: got %q, want allow", got)
		}
	})
}

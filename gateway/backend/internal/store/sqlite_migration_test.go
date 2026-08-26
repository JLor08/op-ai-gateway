// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)
	defer st.Close()

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("first Migrate returned %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("second Migrate returned %v", err)
	}

	for _, table := range []string{"users", "api_tokens", "usage_events", "captures", "chats", "ai_servers", "server_telemetry", "route_affinity", "server_owners", "agent_tokens", "system_settings", "user_ui_preferences"} {
		if !sqliteTableExists(t, st.db, table) {
			t.Fatalf("table %s does not exist", table)
		}
	}
	for _, index := range []string{"idx_route_affinity_lookup", "idx_server_owners_user", "idx_agent_tokens_secret_hash", "idx_captures_created"} {
		if !sqliteIndexExists(t, st.db, index) {
			t.Fatalf("index %s does not exist", index)
		}
	}
}

func TestSQLiteMigrateUpgradesLegacyModelHosts(t *testing.T) {
	ctx := context.Background()
	st := openTestSQLite(t)
	defer st.Close()

	if _, err := st.db.ExecContext(ctx, `create table model_hosts (
		id text primary key,
		name text not null,
		provider text not null,
		endpoint text not null,
		status text not null,
		health_status text not null,
		last_seen_at timestamp,
		created_at timestamp not null,
		updated_at timestamp not null
	)`); err != nil {
		t.Fatalf("create legacy model_hosts: %v", err)
	}
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if _, err := st.db.ExecContext(ctx, `insert into model_hosts (id, name, provider, endpoint, status, health_status, last_seen_at, created_at, updated_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"srv_legacy", "Legacy", "mock", "mock://legacy", "active", "unknown", nil, now, now); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("Migrate returned %v", err)
	}

	var count int
	if err := st.db.QueryRowContext(ctx, `select count(*) from ai_servers`).Scan(&count); err != nil {
		t.Fatalf("count ai_servers: %v", err)
	}
	if count != 1 {
		t.Fatalf("ai_servers count = %d, want 1 (legacy data lost)", count)
	}
	got, err := st.AIServerByID(ctx, "srv_legacy")
	if err != nil {
		t.Fatalf("AIServerByID(srv_legacy): %v", err)
	}
	if got.Name != "Legacy" || got.Domain != "" {
		t.Fatalf("migrated server = %#v, want Name=Legacy Domain=\"\"", got)
	}
	if sqliteTableExists(t, st.db, "model_hosts") {
		t.Fatalf("legacy model_hosts table still exists after migration")
	}
}

func TestMigrateCreatesServerTelemetryOnFreshInstall(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()
	if !sqliteTableExists(t, s.db, "server_telemetry") {
		t.Fatalf("server_telemetry table missing on fresh install")
	}
	if sqliteTableExists(t, s.db, "host_telemetry") {
		t.Fatalf("host_telemetry table should not exist on fresh install")
	}
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	if err := s.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Provider: routing.ProviderMock, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := s.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv_1", ReportedAt: now, LatencyMS: 42, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	got, ok, err := s.TelemetryByServer(ctx, "srv_1")
	if err != nil || !ok || got.LatencyMS != 42 {
		t.Fatalf("TelemetryByServer = %#v ok=%v err=%v", got, ok, err)
	}
}

func TestMigrateUpgradesLegacyHostTelemetry(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)
	defer s.Close()
	now := time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC)
	if _, err := s.db.ExecContext(ctx, `create table ai_servers (
		id text primary key, name text not null, domain text not null default '',
		provider text not null, endpoint text not null, status text not null,
		health_status text not null, last_seen_at timestamp,
		created_at timestamp not null, updated_at timestamp not null)`); err != nil {
		t.Fatalf("create legacy ai_servers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `create table host_telemetry (
		host_id text primary key references ai_servers(id) on delete cascade,
		reported_at timestamp not null, agent_version text not null, os text not null,
		arch text not null, cpu_load real not null, ram_used_bytes integer not null,
		ram_total_bytes integer not null, gpu_count integer not null, vram_used_bytes integer not null,
		vram_total_bytes integer not null, active_requests integer not null, queue_depth integer not null,
		latency_ms integer not null, error_rate real not null, provider_health text not null,
		capabilities text not null, raw_summary text not null, updated_at timestamp not null)`); err != nil {
		t.Fatalf("create legacy host_telemetry: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `insert into ai_servers (id, name, domain, provider, endpoint, status, health_status, last_seen_at, created_at, updated_at) values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"srv_legacy", "Legacy", "l.test", "mock", "mock://legacy", "active", "unknown", nil, now, now); err != nil {
		t.Fatalf("insert legacy server: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `insert into host_telemetry (host_id, reported_at, agent_version, os, arch, cpu_load, ram_used_bytes, ram_total_bytes, gpu_count, vram_used_bytes, vram_total_bytes, active_requests, queue_depth, latency_ms, error_rate, provider_health, capabilities, raw_summary, updated_at) values (?, ?, '', '', '', 0, 0, 0, 0, 0, 0, 0, 0, ?, 0, '{}', '{}', '{}', ?)`,
		"srv_legacy", now, 77, now); err != nil {
		t.Fatalf("insert legacy telemetry: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	got, ok, err := s.TelemetryByServer(ctx, "srv_legacy")
	if err != nil || !ok || got.LatencyMS != 77 {
		t.Fatalf("migrated telemetry = %#v ok=%v err=%v (data lost)", got, ok, err)
	}
	if sqliteTableExists(t, s.db, "host_telemetry") {
		t.Fatalf("legacy host_telemetry still exists after migration")
	}
	if _, err := s.db.ExecContext(ctx, `insert into server_telemetry (server_id, reported_at, agent_version, os, arch, cpu_load, ram_used_bytes, ram_total_bytes, gpu_count, vram_used_bytes, vram_total_bytes, active_requests, queue_depth, latency_ms, error_rate, provider_health, capabilities, raw_summary, updated_at) values (?, ?, '', '', '', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, '{}', '{}', '{}', ?)`,
		"srv_missing", now, now); err == nil {
		t.Fatalf("insert with nonexistent server_id succeeded, want foreign key violation")
	}
}

func TestMigrateDropsLegacyModelRoutesAndReworksAffinity(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `create table ai_servers (
		id text primary key, name text not null, domain text not null default '',
		provider text not null, endpoint text not null, status text not null,
		health_status text not null, last_seen_at timestamp,
		created_at timestamp not null, updated_at timestamp not null)`); err != nil {
		t.Fatalf("create ai_servers: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `create table model_routes (
		id text primary key, model text not null,
		host_id text not null references ai_servers(id) on delete cascade,
		provider_model text not null, api_flavors text not null,
		priority integer not null, weight integer not null, timeout_ms integer not null,
		affinity_ttl_seconds integer not null, retry_policy text not null, status text not null,
		created_at timestamp not null, updated_at timestamp not null)`); err != nil {
		t.Fatalf("create legacy model_routes: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `create table route_affinity (
		id text primary key, api_token_id text not null, user_id text not null,
		model text not null, api_flavor text not null, session_id text not null,
		route_id text not null references model_routes(id) on delete cascade,
		host_id text not null references ai_servers(id) on delete cascade,
		expires_at timestamp not null, last_used_at timestamp not null,
		created_at timestamp not null, updated_at timestamp not null,
		unique(api_token_id, model, api_flavor, session_id))`); err != nil {
		t.Fatalf("create legacy route_affinity: %v", err)
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if sqliteTableExists(t, s.db, "model_routes") {
		t.Fatalf("model_routes should be dropped after migration")
	}
	cols := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `pragma table_info(route_affinity)`)
	if err != nil {
		t.Fatalf("pragma table_info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan pragma: %v", err)
		}
		cols[name] = true
	}
	if !cols["application_id"] || !cols["server_id"] {
		t.Fatalf("route_affinity missing reworked columns: %v", cols)
	}
	if cols["route_id"] || cols["host_id"] {
		t.Fatalf("route_affinity still has legacy columns: %v", cols)
	}
}

func TestSQLiteMigrateCreatesCreatedAtIndex(t *testing.T) {
	st := openMigratedTestSQLite(t)
	defer st.Close()
	if !sqliteIndexExists(t, st.db, "idx_usage_events_created") {
		t.Fatalf("index idx_usage_events_created does not exist after migration")
	}
	// Idempotent: a second Migrate must not error.
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second Migrate returned %v", err)
	}
	if !sqliteIndexExists(t, st.db, "idx_usage_events_created") {
		t.Fatalf("index idx_usage_events_created missing after second Migrate")
	}
}

func TestMigrateRecordsVersionsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenSQLite(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// schema_migrations records the baseline.
	var n int
	if err := s.db.QueryRowContext(ctx, `select count(*) from schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(migrations) {
		t.Fatalf("expected %d applied, got %d", len(migrations), n)
	}
	// Second run applies nothing new (no error, count unchanged).
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var n2 int
	_ = s.db.QueryRowContext(ctx, `select count(*) from schema_migrations`).Scan(&n2)
	if n2 != n {
		t.Fatalf("second migrate changed applied count: %d -> %d", n, n2)
	}
	// A core table exists (baseline applied).
	if _, err := s.db.ExecContext(ctx, `select 1 from users limit 1`); err != nil {
		t.Fatalf("users table missing: %v", err)
	}
}

// TestMigration20BackfillPeerManaged proves migration20Up's backfill marks
// existing gateway-enrolled servers (a non-empty netbird_setup_key_id) as
// managed and leaves manually-linked servers (no setup key) at 0. It runs after
// a full Migrate (which reaches v20 and creates the queryable column) by
// inserting two rows with netbird_peer_managed=0 and re-invoking migration20Up
// directly — which is idempotent (duplicate-column ADD swallowed) and re-runs
// the backfill.
func TestMigration20BackfillPeerManaged(t *testing.T) {
	ctx := context.Background()
	st := openMigratedTestSQLite(t)
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Second)

	// A gateway-enrolled server (has a setup key) and a manually-linked one (none).
	// Both created with NetbirdPeerManaged=false so the backfill is what flips it.
	enrolled := routing.AIServer{
		ID: "srv_enrolled", Name: "Enrolled", Provider: routing.ProviderVLLM,
		Endpoint: "http://a:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		NetbirdEnabled: true, NetbirdSetupKeyID: "sk-backfill", NetbirdPeerManaged: false,
		CreatedAt: now, UpdatedAt: now,
	}
	manual := routing.AIServer{
		ID: "srv_manual", Name: "Manual", Provider: routing.ProviderVLLM,
		Endpoint: "http://b:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		NetbirdEnabled: true, NetbirdSetupKeyID: "", NetbirdPeerManaged: false,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := st.CreateAIServer(ctx, enrolled); err != nil {
		t.Fatalf("create enrolled: %v", err)
	}
	if err := st.CreateAIServer(ctx, manual); err != nil {
		t.Fatalf("create manual: %v", err)
	}

	// Re-invoke migration20Up (idempotent) inside a manual tx so its backfill runs.
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration20Up(ctx, tx, st.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration20Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got, err := st.AIServerByID(ctx, "srv_enrolled")
	if err != nil {
		t.Fatalf("by id enrolled: %v", err)
	}
	if !got.NetbirdPeerManaged {
		t.Fatalf("backfill: enrolled (setup key) managed = false, want true")
	}
	got, err = st.AIServerByID(ctx, "srv_manual")
	if err != nil {
		t.Fatalf("by id manual: %v", err)
	}
	if got.NetbirdPeerManaged {
		t.Fatalf("backfill: manual (no setup key) managed = true, want false")
	}
}

func openTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	st, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite returned %v", err)
	}
	return st
}

func openMigratedTestSQLite(t *testing.T) *SQLiteStore {
	t.Helper()
	st := openTestSQLite(t)
	if err := st.Migrate(context.Background()); err != nil {
		_ = st.Close()
		t.Fatalf("Migrate returned %v", err)
	}
	return st
}

func sqliteTableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`select count(*) from sqlite_master where type = 'table' and name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("QueryRow returned %v", err)
	}
	return count == 1
}

func sqliteIndexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow(`select count(*) from sqlite_master where type = 'index' and name = ?`, name).Scan(&count); err != nil {
		t.Fatalf("QueryRow returned %v", err)
	}
	return count == 1
}

// TestMigration65RuntimeTables proves migration 65 creates all three
// agent-runtime-manager tables in one migration: launch specs, per-GPU VRAM
// demand rows, and the co-residency matrix (Task 2 adds that repo's methods;
// the table exists from this migration on).
func TestMigration65RuntimeTables(t *testing.T) {
	s := openMigratedTestSQLite(t)
	defer s.Close()
	for _, table := range []string{"agent_runtime_specs", "agent_runtime_spec_gpus", "agent_coresidency_rules"} {
		if !sqliteTableExists(t, s.db, table) {
			t.Fatalf("table %s missing after migrate", table)
		}
	}
}

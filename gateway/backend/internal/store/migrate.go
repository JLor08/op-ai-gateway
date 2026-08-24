// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

type migration struct {
	version int
	name    string
	up      func(ctx context.Context, tx *sql.Tx, dl dialect) error
	// rawUp, when set, is used INSTEAD of up. It exists for a migration that
	// needs connection-level control a shared *sql.Tx cannot provide — e.g.
	// migration40Up's SQLite table rebuild must toggle the foreign_keys pragma
	// OUTSIDE any transaction (SQLite documents the toggle as a silent no-op
	// once BEGIN has run), on the SAME physical connection that then runs the
	// rebuild, so the pragma actually takes effect for it. rawUp manages its
	// own transaction and must record its own schema_migrations row (inside
	// that same transaction, so the DDL and the bookkeeping commit atomically —
	// mirroring the up-wrapped path below) rather than relying on the runner.
	rawUp func(ctx context.Context, s *SQLStore, version int, name string) error
}

// migrations is the ordered, forward-only migration list. Append new entries with
// the next version number; NEVER edit or reorder an already-shipped migration.
var migrations = []migration{
	{version: 1, name: "baseline", up: baselineUp},
	{version: 2, name: "user_totp", up: migration2Up},
	{version: 3, name: "server_telemetry_samples", up: migration3Up},
	{version: 4, name: "server_telemetry_bigint_bytes", up: migration4Up},
	{version: 5, name: "application_native_passthrough", up: migration5Up},
	{version: 6, name: "usage_provider_path", up: migration6Up},
	{version: 7, name: "token_model_override_map", up: migration7Up},
	{version: 8, name: "application_loaded_models", up: migration8Up},
	{version: 9, name: "model_mapping_metrics", up: migration9Up},
	{version: 10, name: "application_context_probe", up: migration10Up},
	{version: 11, name: "model_mapping_benchmarks", up: migration11Up},
	{version: 12, name: "application_benchmark_modes", up: migration12Up},
	{version: 13, name: "mapping_concurrency_capacity", up: migration13Up},
	{version: 14, name: "application_capacity_probe", up: migration14Up},
	{version: 15, name: "model_mapping_benchmarks_capacity", up: migration15Up},
	{version: 16, name: "application_admission_queue_timeout", up: migration16Up},
	{version: 17, name: "server_app_path_and_upstream_token", up: migration17Up},
	{version: 18, name: "server_netbird", up: migration18Up},
	{version: 19, name: "server_netbird_groups", up: migration19Up},
	{version: 20, name: "server_netbird_peer_managed", up: migration20Up},
	{version: 21, name: "server_netbird_policy_override", up: migration21Up},
	{version: 22, name: "model_groups", up: migration22Up},
	{version: 23, name: "server_availability_samples", up: migration23Up},
	{version: 24, name: "server_netbird_allow_ping", up: migration24Up},
	{version: 25, name: "server_netbird_ping_exclude", up: migration25Up},
	{version: 26, name: "model_group_traversal", up: migration26Up},
	{version: 27, name: "server_availability_netbird_connected", up: migration27Up},
	{version: 28, name: "server_telemetry_samples_power", up: migration28Up},
	{version: 29, name: "server_hardware", up: migration29Up},
	{version: 30, name: "server_telemetry_samples_cpu_temp", up: migration30Up},
	{version: 31, name: "server_agent_presence_timeout", up: migration31Up},
	{version: 32, name: "model_mappings_vision_capable", up: migration32Up},
	{version: 33, name: "model_mapping_benchmarks_vision_capable", up: migration33Up},
	{version: 34, name: "usage_events_energy", up: migration34Up},
	{version: 35, name: "ai_servers_energy_config", up: migration35Up},
	{version: 36, name: "model_mappings_energy_wh_per_token", up: migration36Up},
	{version: 37, name: "ai_servers_price_unit", up: migration37Up},
	{version: 38, name: "usage_events_cache_write_tokens", up: migration38Up},
	{version: 39, name: "usage_events_session_source_agent", up: migration39Up},
	{version: 40, name: "service_accounts", rawUp: migration40RawUp},
	{version: 41, name: "principal_limits", up: migration41Up},
	{version: 42, name: "route_affinity_user_id_nullable", up: migration42Up},
	{version: 43, name: "float_columns_double_precision", up: migration43Up},
	{version: 44, name: "user_groups", up: migration44Up},
	{version: 45, name: "projects", up: migration45Up},
	{version: 46, name: "projects_coupled_group", up: migration46Up},
	{version: 47, name: "session_elevation", up: migration47Up},
	{version: 48, name: "user_group_managers_permissions", up: migration48Up},
	{version: 49, name: "user_group_managers_can_manage_servers", up: migration49Up},
	{version: 50, name: "server_admin_groups", up: migration50Up},
	{version: 51, name: "user_group_managers_can_manage_services", up: migration51Up},
	{version: 52, name: "service_admin_groups", up: migration52Up},
	{version: 53, name: "user_group_managers_can_manage_resources", up: migration53Up},
	{version: 54, name: "resource_groups", up: migration54Up},
	{version: 55, name: "resource_group_provisions", up: migration55Up},
	{version: 56, name: "api_tokens_server_override", up: migration56Up},
	{version: 57, name: "certificates", up: migration57Up},
	{version: 58, name: "server_certificate_override", up: migration58Up},
	{version: 59, name: "application_proxy_listen_port", up: migration59Up},
	{version: 60, name: "server_https_switch_override", up: migration60Up},
	{version: 61, name: "usage_requested_model", up: migration61Up},
	{version: 62, name: "model_group_selection_settings", up: migration62Up},
	{version: 63, name: "token_unknown_model_redirect", up: migration63Up},
}

// Migrate creates the schema_migrations tracking table then applies, in a
// transaction each, every migration whose version has not been recorded yet.
func (s *SQLStore) Migrate(ctx context.Context) error {
	if _, err := s.exec(ctx, `create table if not exists schema_migrations (
		version integer primary key,
		name text not null,
		applied_at `+s.dl.timestampType()+` not null
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	applied := map[int]bool{}
	rows, err := s.query(ctx, `select version from schema_migrations`)
	if err != nil {
		return fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_ = rows.Close()

	ordered := append([]migration(nil), migrations...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].version < ordered[j].version })

	for _, m := range ordered {
		if applied[m.version] {
			continue
		}
		if m.rawUp != nil {
			if err := m.rawUp(ctx, s, m.version, m.name); err != nil {
				return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
			}
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", m.version, err)
		}
		if err := m.up(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migration %d (%s): %w", m.version, m.name, err)
		}
		if _, err := tx.ExecContext(ctx, s.dl.rebind(
			`insert into schema_migrations (version, name, applied_at) values (?, ?, ?)`),
			m.version, m.name, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", m.version, err)
		}
	}
	return nil
}

// execTx runs a statement inside a migration tx, rebinding placeholders.
func execTx(ctx context.Context, tx *sql.Tx, dl dialect, q string) error {
	_, err := tx.ExecContext(ctx, dl.rebind(q))
	return err
}

// addColumnIfMissing runs `alter table <table> add column <colDef>` tolerantly
// of the column already existing, using the same duplicate-tolerant pattern
// every migrationNUp in this file has copy-pasted individually (~36 call
// sites as of v60; see e.g. migration5Up, migration58Up, migration60Up):
// on postgres it rewrites the FIRST "add column " to "add column if not
// exists " (postgres supports the clause directly); on sqlite, which has no
// such clause, it runs the statement as-is and swallows the "duplicate
// column name" error sqlite returns when the column is already there (e.g.
// because a fresh DB's baseline already created it). colDef is everything
// after "add column ", e.g. `https_switch_override text not null default` plus
// an empty-string default clause (see migration60Up for the inlined version).
//
// migration61Up (usage_requested_model) is the first and only caller — every
// migrationNUp up to v60 keeps its own inlined copy of this block, and per the
// forward-only migration-ledger rule (see the `migrations` var doc comment,
// "NEVER edit or reorder an already-shipped migration") none of them are
// rewritten to call this helper retroactively; rewriting a shipped
// migration's body is exactly the kind of change that rule exists to
// prevent, even when the resulting SQL would be byte-identical. It exists so
// a NEW migration can call addColumnIfMissing(ctx, tx, dl, "table", "col
// def") instead of copy-pasting the block a 37th time. See
// TestAddColumnIfMissingSQLite for coverage.
func addColumnIfMissing(ctx context.Context, tx *sql.Tx, dl dialect, table, colDef string) error {
	stmt := "alter table " + table + " add column " + colDef
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// baselineUp builds the current schema. On sqlite it reproduces the historical
// three-phase migration (legacy renames + CREATE IF NOT EXISTS + additive ALTERs,
// swallowing the same benign errors) so existing dev DBs upgrade and get stamped
// v1. On postgres (always fresh) it runs CREATE TABLE/INDEX only, with bytea +
// timestamptz types — no renames, no additive ALTERs.
func baselineUp(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if dl.name() == "sqlite" {
		// Phase 1: legacy renames (swallow benign errors). SQLite does NOT abort a
		// transaction on a statement error, so continuing after a swallow is safe.
		for _, stmt := range []string{
			`alter table model_hosts rename to ai_servers`,
			`alter table ai_servers add column domain text not null default ''`,
			`alter table host_telemetry rename to server_telemetry`,
			`alter table server_telemetry rename column host_id to server_id`,
			`drop table if exists route_affinity`,
			`drop table if exists model_routes`,
		} {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				msg := strings.ToLower(err.Error())
				if strings.Contains(msg, "no such table") || strings.Contains(msg, "no such column") ||
					strings.Contains(msg, "duplicate column name") || strings.Contains(msg, "already exists") ||
					strings.Contains(msg, "there is already") {
					continue
				}
				return err
			}
		}
	}
	// Phase 2: CREATE TABLE / INDEX (both dialects; dialect types for blob + timestamp).
	for _, stmt := range baselineCreateStatements(dl) {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	if dl.name() == "sqlite" {
		// Phase 3: additive ALTERs for old sqlite DBs (swallow "duplicate column name").
		for _, stmt := range baselineSqliteAlters() {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
					continue
				}
				return err
			}
		}
	}
	return nil
}

// baselineCreateStatements returns the CREATE TABLE / CREATE INDEX statements
// that build the current schema, dialect-aware. This is copied verbatim from the
// pre-migrate-runner sqlite.go:Migrate "statements" block, with exactly two
// mechanical substitutions: every `timestamp`-typed column uses dl.timestampType()
// and the two `blob blob not null` columns (captures.blob, chats.blob) use
// dl.blobType(). All other column types (text/integer/real/default/references/
// unique/primary key) are identical across dialects and are left untouched.
//
// FROZEN as of migration v60 (server_https_switch_override): this function is
// a point-in-time snapshot of the schema as it stood through v60, not a
// rolling mirror of "the current schema". Earlier migrations (see migration50Up,
// migration52Up, and migration54Up's doc comments) back-ported a handful of
// columns into this function so a fresh install would have them without
// waiting on the later migration that also duplicate-tolerantly ADDs them —
// that "double-write" convention is RETIRED as of v60. Going forward, a new
// migration that adds a column must NOT also add it here, no matter how
// tempting "fresh installs get it for free" looks: a fresh install already
// gets it for free by replaying the full, duplicate-tolerant migration chain
// (v1 baseline, then v2..vN in order) — see addColumnIfMissing and Migrate's
// loop above. Back-porting into a frozen baseline only risks the baseline and
// the migration drifting out of sync with no test catching it.
func baselineCreateStatements(dl dialect) []string {
	blob := dl.blobType()
	ts := dl.timestampType()
	return []string{
		`create table if not exists users (
			id text primary key,
			email text not null unique,
			display_name text not null,
			role text not null,
			status text not null,
			preferred_language text not null,
			chat_log_communication integer not null default 0,
			chat_secret integer not null default 0,
			totp_secret text not null default '',
			totp_pending_secret text not null default '',
			totp_enabled integer not null default 0,
			totp_confirmed_at ` + ts + `,
			password_hash text not null default '',
			password_set_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		// Services (Phase 1 service accounts, migration v40) must be created
		// BEFORE api_tokens (below), which references services(id) — postgres
		// requires the target of a REFERENCES clause to already exist (sqlite is
		// lenient about forward references, but the statement list is shared).
		`create table if not exists services (
			id text primary key,
			name text not null,
			description text not null default '',
			status text not null default 'active',
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			system_group_id text not null default ''
		)`,
		`create table if not exists service_delegates (
			service_id text not null references services(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			can_manage_settings integer not null default 0,
			primary key (service_id, user_id)
		)`,
		`create table if not exists service_allowed_models (
			service_id text not null references services(id) on delete cascade,
			gateway_model_name text not null,
			primary key (service_id, gateway_model_name)
		)`,
		// api_tokens.user_id is nullable (a service token has no user; migration
		// v40 dropped the original NOT NULL) and gained service_id + kind. A
		// fresh install gets this shape straight from baselineUp; migration40Up
		// upgrades a pre-v40 database (a full SQLite table rebuild, since SQLite
		// has no ALTER COLUMN DROP NOT NULL — see migration40Up's doc comment).
		`create table if not exists api_tokens (
			id text primary key,
			user_id text references users(id) on delete cascade,
			name text not null,
			secret_hash text not null unique,
			secret_prefix text not null,
			status text not null,
			scopes text not null,
			expires_at ` + ts + `,
			last_used_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			model_override text not null default '',
			model_override_map text not null default '',
			log_communication integer not null default 0,
			secret integer not null default 0,
			service_id text references services(id) on delete cascade,
			kind text not null default 'user',
			server_override text not null default '',
			server_override_force_unreachable integer not null default 0
		)`,
		`create table if not exists usage_events (
			id text primary key,
			request_id text not null,
			user_id text not null,
			token_id text not null,
			session_id text not null,
			session_source text not null default '',
			agent_id text not null default '',
			api_flavor text not null,
			model text not null,
			route_id text not null default '',
			provider text not null,
			host text not null,
			status text not null,
			error_code text not null,
			input_tokens integer not null,
			output_tokens integer not null,
			total_tokens integer not null,
			latency_ms integer not null,
			cached_tokens integer not null default 0,
			cache_write_tokens integer not null default 0,
			prompt_per_second double precision not null default 0,
			tokens_per_second double precision not null default 0,
			http_status integer not null default 0,
			content_type text not null default '',
			req_path text not null default '',
			provider_path text not null default '',
			provider_model text not null default '',
			stream integer not null default 0,
			token_name text not null default '',
			server_name text not null default '',
			service_id text not null default '',
			service_name text not null default '',
			energy_wh double precision not null default 0,
			energy_marginal_wh double precision not null default 0,
			energy_source text not null default '',
			created_at ` + ts + ` not null
		)`,
		`create table if not exists captures (
			usage_event_id text primary key references usage_events(id) on delete cascade,
			key_version integer not null,
			blob ` + blob + ` not null,
			created_at ` + ts + ` not null,
			secret integer not null default 0
		)`,
		`create table if not exists chats (
			id text primary key,
			user_id text not null references users(id) on delete cascade,
			title text not null,
			key_version integer not null,
			blob ` + blob + ` not null,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists ai_servers (
			id text primary key,
			name text not null,
			domain text not null default '',
			server_path_suffix text not null default '',
			provider text not null,
			endpoint text not null,
			status text not null,
			health_status text not null,
			last_seen_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			netbird_enabled integer not null default 0,
			netbird_setup_key_id text not null default '',
			netbird_group_id text not null default '',
			netbird_peer_id text not null default '',
			netbird_connected integer not null default 0,
			netbird_group_ids text not null default '',
			netbird_peer_managed integer not null default 0,
			netbird_policy_override text not null default '',
			netbird_allow_ping integer not null default 0,
			netbird_ping_exclude integer not null default 0,
			agent_presence_timeout_seconds integer not null default 0,
			estimated_watts double precision not null default 0,
			idle_watts double precision not null default 0,
			price_per_kwh double precision not null default 0,
			pue double precision not null default 0,
			price_unit text not null default 'eur_cent',
			system_group_id text not null default '',
			certificate_override text not null default '',
			https_switch_override text not null default ''
		)`,
		`create table if not exists server_telemetry (
			server_id text primary key references ai_servers(id) on delete cascade,
			reported_at ` + ts + ` not null,
			agent_version text not null,
			os text not null,
			arch text not null,
			cpu_load double precision not null,
			ram_used_bytes bigint not null,
			ram_total_bytes bigint not null,
			gpu_count integer not null,
			vram_used_bytes bigint not null,
			vram_total_bytes bigint not null,
			active_requests integer not null,
			queue_depth integer not null,
			latency_ms integer not null,
			error_rate double precision not null,
			provider_health text not null,
			capabilities text not null,
			raw_summary text not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists server_hardware (
			server_id text primary key references ai_servers(id) on delete cascade,
			collected_at ` + ts + ` not null,
			report_json text not null default '',
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists server_telemetry_samples (
			id text primary key,
			server_id text not null references ai_servers(id) on delete cascade,
			reported_at ` + ts + ` not null,
			cpu_util_pct double precision not null default 0,
			mem_used_bytes bigint not null default 0,
			mem_total_bytes bigint not null default 0,
			swap_used_bytes bigint not null default 0,
			swap_total_bytes bigint not null default 0,
			load1 double precision not null default 0,
			load5 double precision not null default 0,
			load15 double precision not null default 0,
			active_requests integer not null default 0,
			queue_depth integer not null default 0,
			gpus_json text not null default '[]',
			net_json text not null default '[]',
			cpu_power_w double precision,
			system_power_w double precision,
			cpu_temp_c double precision
		)`,
		`create table if not exists server_availability_samples (
			id text primary key,
			server_id text not null references ai_servers(id) on delete cascade,
			reported_at ` + ts + ` not null,
			health text not null default '',
			reachable_count integer not null default 0,
			active_count integer not null default 0,
			agent_reporting integer not null default 0,
			netbird_connected integer not null default 0
		)`,
		`create table if not exists server_owners (
			server_id text not null references ai_servers(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			primary key (server_id, user_id)
		)`,
		`create table if not exists sessions (
			id text primary key,
			user_id text not null references users(id) on delete cascade,
			secret_hash text not null unique,
			created_at ` + ts + ` not null,
			expires_at ` + ts + ` not null,
			last_seen_at ` + ts + ` not null,
			elevated_until ` + ts + `
		)`,
		`create table if not exists set_password_tokens (
			id text primary key,
			user_id text not null references users(id) on delete cascade,
			secret_hash text not null unique,
			expires_at ` + ts + ` not null,
			used_at ` + ts + `,
			created_at ` + ts + ` not null
		)`,
		`create table if not exists applications (
			id text primary key,
			server_id text not null references ai_servers(id) on delete cascade,
			type text not null, port integer not null, scheme text not null,
			api_flavors text not null, priority integer not null, weight integer not null,
			timeout_ms integer not null, affinity_ttl_seconds integer not null,
			status text not null,
			always_reachable integer not null default 0,
			health_check_path text not null default '/v1/health',
			health_check_mode text not null default '',
			health_check_interval_seconds integer not null default 0,
			native_responses integer not null default 0,
			native_messages integer not null default 0,
			loaded_models_path text not null default '',
			loaded_models_format text not null default '',
			context_probe_path text not null default '',
			capacity_probe_path text not null default '',
			app_path_suffix text not null default '',
			api_token text not null default '',
			api_token_header text not null default '',
			benchmark_schedule_enabled integer not null default 0,
			benchmark_schedule_interval_seconds integer not null default 0,
			opportunistic_metrics_enabled integer not null default 0,
			proxy_listen_port integer not null default 0,
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null,
			unique(server_id, port)
		)`,
		`create table if not exists model_mappings (
			id text primary key,
			application_id text not null references applications(id) on delete cascade,
			gateway_model_name text not null, app_model_name text not null, status text not null,
			gen_tokens_per_second double precision not null default 0,
			prompt_tokens_per_second double precision not null default 0,
			load_time_ms integer not null default 0,
			context_size integer not null default 0,
			is_mtp integer not null default 0,
			vision_capable integer not null default 0,
			energy_wh_per_token double precision not null default 0,
			metrics_locked integer not null default 0,
			metrics_updated_at ` + ts + `,
			metrics_source text not null default '',
			max_concurrency integer not null default 0,
			recommended_concurrency integer not null default 0,
			gen_tokens_per_second_at_capacity double precision not null default 0,
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
		`create table if not exists model_mapping_benchmarks (
			id text primary key,
			mapping_id text not null references model_mappings(id) on delete cascade,
			server_id text not null,
			created_at ` + ts + ` not null,
			gen_tokens_per_second double precision not null default 0,
			prompt_tokens_per_second double precision not null default 0,
			load_time_ms integer not null default 0,
			context_size integer not null default 0,
			error text not null default '',
			vision_capable integer not null default 0
		)`,
		`create table if not exists agent_tokens (
			id text primary key,
			server_id text not null unique references ai_servers(id) on delete cascade,
			secret_hash text not null unique,
			secret_prefix text not null,
			last_used_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists route_affinity (
			id text primary key,
			api_token_id text not null references api_tokens(id) on delete cascade,
			user_id text references users(id) on delete cascade,
			model text not null,
			api_flavor text not null,
			session_id text not null,
			application_id text not null references applications(id) on delete cascade,
			server_id text not null references ai_servers(id) on delete cascade,
			expires_at ` + ts + ` not null,
			last_used_at ` + ts + ` not null,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			resolved_model text not null default '',
			unique(api_token_id, model, api_flavor, session_id)
		)`,
		`create table if not exists model_groups (
			id text primary key,
			gateway_model_name text not null,
			display_name text not null,
			status text not null,
			failover_mode text not null default 'sticky',
			traversal text not null default 'round_robin',
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
		`create table if not exists model_group_members (
			id text primary key,
			group_id text not null references model_groups(id) on delete cascade,
			member_gateway_name text not null,
			priority integer not null default 0,
			created_at ` + ts + ` not null,
			unique(group_id, member_gateway_name)
		)`,
		`create table if not exists model_settings (
			gateway_model_name text primary key,
			visibility text not null default 'shown',
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
		`create table if not exists principal_limits (
			principal_type text not null,
			principal_id text not null,
			rate_limit_requests integer not null default 0,
			rate_limit_window_seconds integer not null default 0,
			request_quota_requests integer not null default 0,
			request_quota_period text not null default '',
			token_quota_tokens bigint not null default 0,
			token_quota_period text not null default '',
			cost_budget_amount double precision not null default 0,
			cost_budget_period text not null default '',
			updated_at ` + ts + ` not null,
			primary key (principal_type, principal_id)
		)`,
		`create table if not exists system_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at ` + ts + ` NOT NULL
		)`,
		`create table if not exists user_ui_preferences (
			user_id text not null references users(id) on delete cascade,
			key text not null,
			value_json text not null,
			updated_at ` + ts + ` not null,
			primary key (user_id, key)
		)`,
		`create index if not exists idx_usage_events_user_created on usage_events(user_id, created_at)`,
		`create index if not exists idx_usage_events_created on usage_events(created_at)`,
		`create index if not exists idx_captures_created on captures(created_at)`,
		`create index if not exists idx_chats_user on chats(user_id, updated_at)`,
		`create index if not exists idx_api_tokens_hash on api_tokens(secret_hash)`,
		`create index if not exists idx_route_affinity_lookup on route_affinity(api_token_id, model, api_flavor, session_id, expires_at)`,
		`create index if not exists idx_sessions_user on sessions(user_id)`,
		`create index if not exists idx_set_password_tokens_user on set_password_tokens(user_id)`,
		`create index if not exists idx_server_owners_user on server_owners(user_id)`,
		`create index if not exists idx_model_mappings_application on model_mappings(application_id)`,
		`create index if not exists idx_agent_tokens_secret_hash on agent_tokens(secret_hash)`,
		`create index if not exists idx_server_telemetry_samples_server_reported on server_telemetry_samples(server_id, reported_at desc)`,
		`create index if not exists idx_server_availability_samples_server_reported on server_availability_samples(server_id, reported_at desc)`,
		`create index if not exists idx_model_mapping_benchmarks_mapping_created on model_mapping_benchmarks(mapping_id, created_at desc)`,
		`create index if not exists idx_model_group_members_group on model_group_members(group_id, priority, id)`,
		`create index if not exists idx_service_delegates_user on service_delegates(user_id)`,
	}
}

// baselineSqliteAlters returns the additive ALTER TABLE statements that upgrade
// pre-existing sqlite databases created before a given column existed. Copied
// verbatim from the pre-migrate-runner sqlite.go:Migrate "alters" block. These
// run sqlite-only: postgres databases are always created fresh by
// baselineCreateStatements, which already includes every one of these columns.
func baselineSqliteAlters() []string {
	return []string{
		`alter table users add column password_hash text not null default ''`,
		`alter table users add column password_set_at timestamp`,
		`alter table users add column chat_log_communication integer not null default 0`,
		`alter table users add column chat_secret integer not null default 0`,
		`alter table api_tokens add column model_override text not null default ''`,
		`alter table api_tokens add column log_communication integer not null default 0`,
		`alter table api_tokens add column secret integer not null default 0`,
		`alter table usage_events add column cached_tokens integer not null default 0`,
		`alter table usage_events add column prompt_per_second real not null default 0`,
		`alter table usage_events add column tokens_per_second real not null default 0`,
		`alter table usage_events add column http_status integer not null default 0`,
		`alter table usage_events add column content_type text not null default ''`,
		`alter table usage_events add column req_path text not null default ''`,
		`alter table usage_events add column provider_model text not null default ''`,
		`alter table usage_events add column stream integer not null default 0`,
		`alter table usage_events add column token_name text not null default ''`,
		`alter table usage_events add column server_name text not null default ''`,
		`alter table captures add column secret integer not null default 0`,
		`alter table applications add column always_reachable integer not null default 0`,
		`alter table applications add column health_check_path text not null default '/v1/health'`,
		`alter table applications add column health_check_mode text not null default ''`,
		`alter table applications add column health_check_interval_seconds integer not null default 0`,
	}
}

// migration2Up adds the TOTP columns to users. On a fresh DB the baseline
// (migration 1) already creates them, so this is a no-op there; it only does
// real work upgrading a pre-existing DB.
func migration2Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmts := []string{
		`alter table users add column totp_secret text not null default ''`,
		`alter table users add column totp_pending_secret text not null default ''`,
		`alter table users add column totp_enabled integer not null default 0`,
		`alter table users add column totp_confirmed_at ` + dl.timestampType(),
	}
	for _, stmt := range stmts {
		if dl.name() == "postgres" {
			// postgres supports IF NOT EXISTS; makes a fresh DB (baseline already
			// has the columns) a no-op while upgrading any pre-existing DB.
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		// sqlite: no IF NOT EXISTS on ADD COLUMN — swallow "duplicate column name"
		// so a fresh DB (baseline already has them) is a no-op.
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration3Up adds server_telemetry_samples. On a fresh DB the baseline
// (migration 1) already creates it, so `create table/index if not exists`
// make this a no-op there; it only does real work upgrading a pre-existing DB.
func migration3Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists server_telemetry_samples (
			id text primary key,
			server_id text not null references ai_servers(id) on delete cascade,
			reported_at ` + ts + ` not null,
			cpu_util_pct real not null default 0,
			mem_used_bytes bigint not null default 0,
			mem_total_bytes bigint not null default 0,
			swap_used_bytes bigint not null default 0,
			swap_total_bytes bigint not null default 0,
			load1 real not null default 0,
			load5 real not null default 0,
			load15 real not null default 0,
			active_requests integer not null default 0,
			queue_depth integer not null default 0,
			gpus_json text not null default '[]',
			net_json text not null default '[]'
		)`,
		`create index if not exists idx_server_telemetry_samples_server_reported on server_telemetry_samples(server_id, reported_at desc)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration4Up widens the server_telemetry byte columns from int4 to bigint on
// postgres. The v1 baseline originally declared ram_used_bytes / ram_total_bytes /
// vram_used_bytes / vram_total_bytes as `integer` (int4, max ~2.1e9), which
// overflows for a host with >2 GB RAM/VRAM — pgx then fails to encode the int64
// value ("greater than maximum value for int4") and every agent telemetry POST
// 500s. SQLite's INTEGER is already 64-bit (and does not support ALTER COLUMN
// TYPE), so this is a no-op there; fresh DBs get bigint directly from the
// corrected baseline.
func migration4Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if dl.name() != "postgres" {
		return nil
	}
	for _, col := range []string{"ram_used_bytes", "ram_total_bytes", "vram_used_bytes", "vram_total_bytes"} {
		if err := execTx(ctx, tx, dl, "alter table server_telemetry alter column "+col+" type bigint"); err != nil {
			return err
		}
	}
	return nil
}

// migration5Up adds the per-application native-passthrough flags
// (native_responses = Codex /v1/responses, native_messages = Claude Code /v1/messages).
// When set, the gateway proxies the raw client body straight to the upstream's
// native endpoint instead of translating through the internal representation. On a
// fresh DB the baseline (migration 1) already creates the columns, so this is a
// no-op there (same duplicate-tolerant pattern as migration2Up); it only does real
// work upgrading a pre-existing DB.
func migration5Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{"native_responses", "native_messages"} {
		stmt := "alter table applications add column " + col + " integer not null default 0"
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration6Up adds usage_events.provider_path — the upstream endpoint path the
// gateway called (the built-in translation's chat-completions path, or the native
// passthrough path). On a fresh DB the baseline (migration 1) already creates it,
// so this is a no-op there (same duplicate-tolerant pattern as migration5Up); it
// only does real work upgrading a pre-existing DB.
func migration6Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table usage_events add column provider_path text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration7Up adds api_tokens.model_override_map — the per-requested-model
// override map (JSON object string; model_override stays the catch-all). On a
// fresh DB the baseline (migration 1) already creates it, so this is a no-op there
// (same duplicate-tolerant pattern as migration6Up); it only does real work
// upgrading a pre-existing DB.
func migration7Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table api_tokens add column model_override_map text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration8Up adds applications.loaded_models_path + loaded_models_format — the
// optional per-application endpoint (path + response-parser hint) the gateway polls
// to learn which model(s) are currently loaded. On a fresh DB the baseline already
// creates them, so this is a no-op there (same duplicate-tolerant pattern as
// migration5Up); it only does real work upgrading a pre-existing DB.
func migration8Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{"loaded_models_path", "loaded_models_format"} {
		stmt := "alter table applications add column " + col + " text not null default ''"
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration9Up adds the per-model-mapping performance-metric columns. On a fresh
// DB the baseline (migration 1) already creates them, so this is a no-op there
// (same duplicate-tolerant pattern as migration8Up); it only does real work
// upgrading a pre-existing DB. metrics_updated_at is nullable (dl.timestampType,
// no default); every other metric column defaults to a zero "unknown" value.
func migration9Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"gen_tokens_per_second real not null default 0",
		"prompt_tokens_per_second real not null default 0",
		"load_time_ms integer not null default 0",
		"context_size integer not null default 0",
		"is_mtp integer not null default 0",
		"metrics_locked integer not null default 0",
		"metrics_updated_at " + dl.timestampType(),
		"metrics_source text not null default ''",
	}
	for _, col := range cols {
		stmt := "alter table model_mappings add column " + col
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration10Up adds applications.context_probe_path — the optional per-application
// upstream path the gateway GETs (llama.cpp /props-style) to learn the model's
// context size. On a fresh DB the baseline already creates it, so this is a no-op
// there (same duplicate-tolerant pattern as migration8Up); it only does real work
// upgrading a pre-existing DB.
func migration10Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table applications add column context_probe_path text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration11Up adds the model_mapping_benchmarks history table (one row per
// benchmarked mapping per run) + its lookup index. On a fresh DB the baseline
// (migration 1) already creates them, so `create table/index if not exists`
// make this a no-op there; it only does real work upgrading a pre-existing DB.
// Mirrors migration3Up (server_telemetry_samples). server_id has no FK on
// purpose (a run survives server churn); the mapping FK cascade is enough.
func migration11Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists model_mapping_benchmarks (
			id text primary key,
			mapping_id text not null references model_mappings(id) on delete cascade,
			server_id text not null,
			created_at ` + ts + ` not null,
			gen_tokens_per_second real not null default 0,
			prompt_tokens_per_second real not null default 0,
			load_time_ms integer not null default 0,
			context_size integer not null default 0,
			error text not null default ''
		)`,
		`create index if not exists idx_model_mapping_benchmarks_mapping_created on model_mapping_benchmarks(mapping_id, created_at desc)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration12Up adds the per-application P5 benchmark-mode columns
// (benchmark_schedule_enabled + benchmark_schedule_interval_seconds = scheduled
// benchmark mode; opportunistic_metrics_enabled = opportunistic EWMA mode). On a
// fresh DB the baseline (migration 1) already creates them, so this is a no-op
// there (same duplicate-tolerant pattern as migration8Up); it only does real work
// upgrading a pre-existing DB. All three default to 0 (feature OFF).
func migration12Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{"benchmark_schedule_enabled", "benchmark_schedule_interval_seconds", "opportunistic_metrics_enabled"} {
		stmt := "alter table applications add column " + col + " integer not null default 0"
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration13Up adds the per-model-mapping concurrency-capacity metric columns
// (max_concurrency + recommended_concurrency = concurrency capacity;
// gen_tokens_per_second_at_capacity = aggregate generation throughput at capacity).
// On a fresh DB the baseline (migration 1) already creates them, so this is a no-op
// there (same duplicate-tolerant pattern as migration9Up); it only does real work
// upgrading a pre-existing DB. All three default to 0 (unknown).
func migration13Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []struct{ name, typ string }{
		{"max_concurrency", "integer not null default 0"},
		{"recommended_concurrency", "integer not null default 0"},
		{"gen_tokens_per_second_at_capacity", "real not null default 0"},
	}
	for _, c := range cols {
		stmt := "alter table model_mappings add column " + c.name + " " + c.typ
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration14Up adds applications.capacity_probe_path — the optional per-application
// upstream path the gateway GETs to learn the saturation signal used by the capacity
// benchmark (llama.cpp /metrics Prometheus, or /props|/slots JSON). On a fresh DB the
// baseline already creates it, so this is a no-op there (same duplicate-tolerant
// pattern as migration10Up); it only does real work upgrading a pre-existing DB.
func migration14Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table applications add column capacity_probe_path text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration15Up adds the capacity-curve columns to model_mapping_benchmarks: a
// kind discriminator ("speed" default | "capacity") and an opaque capacity_curve
// JSON string (a serialized routing.CapacityReport for a capacity row, "" for a
// speed row). The v11 baseline creates the table without these columns, so this
// ALTER does real work on both fresh and pre-existing DBs (same duplicate-tolerant
// per-column pattern as migration13Up).
func migration15Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []struct{ name, typ string }{
		{"kind", "text not null default 'speed'"},
		{"capacity_curve", "text not null default ''"},
	}
	for _, c := range cols {
		stmt := "alter table model_mapping_benchmarks add column " + c.name + " " + c.typ
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration16Up adds applications.admission_queue_timeout_seconds — how long an
// unpinned request waits in the CP4 admission queue for a free concurrency slot
// (0 = wait until the client aborts). The v1 baseline creates the applications
// table without this column, so this ALTER does real work on both fresh and
// pre-existing DBs (same duplicate-tolerant single-column pattern as migration14Up).
func migration16Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table applications add column admission_queue_timeout_seconds integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration17Up adds the URL-path-suffix + per-application upstream-token columns:
// ai_servers.server_path_suffix and applications.app_path_suffix (both appended to
// the upstream origin), plus applications.api_token (a SEALED enc:/plain: upstream
// credential) and applications.api_token_header (an optional custom header name).
// On a fresh DB the baseline (migration 1) already creates them, so this is a no-op
// there (same duplicate-tolerant multi-column pattern as migration15Up); it only
// does real work upgrading a pre-existing DB. All four default to ” (feature OFF).
func migration17Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []struct{ table, name string }{
		{"ai_servers", "server_path_suffix"},
		{"applications", "app_path_suffix"},
		{"applications", "api_token"},
		{"applications", "api_token_header"},
	}
	for _, c := range cols {
		stmt := "alter table " + c.table + " add column " + c.name + " text not null default ''"
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration18Up adds the NetBird integration columns to ai_servers:
// netbird_enabled (marks the server as a NetBird peer), netbird_setup_key_id +
// netbird_group_id (so the setup key can be regenerated and the peer correlated
// via the per-server tracking group), and the synced netbird_peer_id +
// netbird_connected state. On a fresh DB the baseline (migration 1) already
// creates them, so this is a no-op there (same duplicate-tolerant per-column
// pattern as migration17Up); it only does real work upgrading a pre-existing DB.
// The two bool columns stay integer, per the dialect convention.
func migration18Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"netbird_enabled integer not null default 0",
		"netbird_setup_key_id text not null default ''",
		"netbird_group_id text not null default ''",
		"netbird_peer_id text not null default ''",
		"netbird_connected integer not null default 0",
	}
	for _, col := range cols {
		stmt := "alter table ai_servers add column " + col
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration19Up adds ai_servers.netbird_group_ids — an OPAQUE JSON string
// ([{"id","name"}]) mirroring the peer's NetBird policy groups (excluding the
// per-server tracking group). The store treats it as a dumb string (like
// capacity_curve). On a fresh DB the baseline (migration 1) already creates it,
// so this is a no-op there (same duplicate-tolerant pattern as migration18Up);
// it only does real work upgrading a pre-existing DB. Default ” (no groups yet).
func migration19Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column netbird_group_ids text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration20Up adds ai_servers.netbird_peer_managed — a provenance flag marking a
// server whose NetBird peer + setup key originated from a gateway-generated setup
// key (create hook / enroll / regenerate), so the delete-peer checkbox can be
// pre-checked; a peer linked manually via the system-admin editor stays 0. On a
// fresh DB the baseline (migration 1) already creates the column, so the ADD COLUMN
// is a no-op there (same duplicate-tolerant single-column pattern as migration19Up).
// After the column exists it runs a one-time backfill marking every already
// gateway-enrolled server (a non-empty netbird_setup_key_id) as managed, so existing
// rows are correctly pre-checked; on a fresh 0-row DB the backfill is a no-op. The
// backfill runs after a successful (or duplicate-tolerated) ADD COLUMN. The bool
// column stays integer, per the dialect convention.
func migration20Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column netbird_peer_managed integer not null default 0"
	if dl.name() == "postgres" {
		if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
			return err
		}
	} else {
		// sqlite: no IF NOT EXISTS on ADD COLUMN — swallow "duplicate column name"
		// so a fresh DB (baseline already has it) is a no-op. SQLite does NOT abort
		// the tx on a statement error, so the backfill below still runs.
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				return err
			}
		}
	}
	// Backfill: an existing gateway-enrolled server (a setup key was generated) is
	// managed. A fresh 0-row DB makes this a no-op.
	return execTx(ctx, tx, dl, "update ai_servers set netbird_peer_managed = 1 where netbird_setup_key_id <> ''")
}

// migration21Up adds ai_servers.netbird_policy_override — the per-server policy
// opt-in/opt-out override ("" default / "include" / "exclude"). On a fresh DB the
// baseline (migration 1) already creates the column, so the ADD COLUMN is a no-op
// there (same duplicate-tolerant single-column pattern as migration19Up). NO
// backfill: unlike migration20Up's provenance flag, there is no prior state to
// derive a sensible non-default value from, so every existing row keeps the
// default "" (no override) until an operator sets one explicitly.
func migration21Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column netbird_policy_override text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration22Up creates the model-group tables (model_groups + the ordered
// model_group_members + per-model model_settings) and adds route_affinity.resolved_model.
// On a fresh DB the baseline (migration 1) already creates the tables + column, so the
// `create table/index if not exists` blocks and the duplicate-tolerant ADD COLUMN make
// this a no-op there; it only does real work upgrading a pre-existing DB. model_group_members
// FK-cascades on model_groups; a member references a gateway model NAME (loose, no FK).
func migration22Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists model_groups (
			id text primary key,
			gateway_model_name text not null,
			display_name text not null,
			status text not null,
			failover_mode text not null default 'sticky',
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
		`create table if not exists model_group_members (
			id text primary key,
			group_id text not null references model_groups(id) on delete cascade,
			member_gateway_name text not null,
			priority integer not null default 0,
			created_at ` + ts + ` not null,
			unique(group_id, member_gateway_name)
		)`,
		`create index if not exists idx_model_group_members_group on model_group_members(group_id, priority, id)`,
		`create table if not exists model_settings (
			gateway_model_name text primary key,
			visibility text not null default 'shown',
			created_at ` + ts + ` not null, updated_at ` + ts + ` not null
		)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	// route_affinity.resolved_model (duplicate-tolerant, both dialects).
	col := "alter table route_affinity add column resolved_model text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration23Up adds server_availability_samples. On a fresh DB the baseline
// already creates it, so create-if-not-exists makes this a no-op there; it only
// does real work upgrading a pre-existing DB.
func migration23Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists server_availability_samples (
			id text primary key,
			server_id text not null references ai_servers(id) on delete cascade,
			reported_at ` + ts + ` not null,
			health text not null default '',
			reachable_count integer not null default 0,
			active_count integer not null default 0,
			agent_reporting integer not null default 0
		)`,
		`create index if not exists idx_server_availability_samples_server_reported on server_availability_samples(server_id, reported_at desc)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration24Up adds ai_servers.netbird_allow_ping — the per-server flag letting
// the gateway ICMP-ping this server (managed policy op-gw-ping-servers). On a fresh
// DB the baseline (migration 1) already creates the column, so the ADD COLUMN is a
// no-op there (same duplicate-tolerant single-column pattern as migration21Up). NO
// backfill: every existing row keeps the default 0 (not pingable) until an operator
// opts in. The bool column stays integer, per the dialect convention. Routing never
// reads it (absent from the candidate join).
func migration24Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column netbird_allow_ping integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration25Up adds ai_servers.netbird_ping_exclude — the per-server flag opting a
// server OUT of ping when the account-wide "all servers pingable" switch is on. On a
// fresh DB the baseline (migration 1) already creates the column, so the ADD COLUMN is
// a no-op there (same duplicate-tolerant single-column pattern as migration24Up). NO
// backfill: every existing row keeps the default 0 (not excluded). The bool column
// stays integer, per the dialect convention. Routing never reads it (absent from the
// candidate join).
func migration25Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column netbird_ping_exclude integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration26Up adds model_groups.traversal — the per-group subgroup-traversal
// strategy ("depth" | "breadth" | "round_robin") used to flatten a nested group tree
// into the failover candidate order. On a fresh DB the baseline (migration 1) already
// creates the column, so the ADD COLUMN is a no-op there (same duplicate-tolerant
// single-column pattern as migration25Up). NO backfill: every existing row (which has
// no subgroups yet) keeps the default 'round_robin' — inert until nesting is used.
func migration26Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table model_groups add column traversal text not null default 'round_robin'"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration27Up adds server_availability_samples.netbird_connected — the third
// tracked availability dimension (NetBird peer connectivity over time), folded into
// the event-sourced sample by the health loop. On a fresh DB the baseline (migration
// 1) already creates the column, so the ADD COLUMN is a no-op there (same
// duplicate-tolerant single-column pattern as migration26Up). NO backfill: every
// existing sample reads back 0 ("was not connected then"); NetBird history starts at
// deploy. The bool column stays integer, per the dialect convention.
func migration27Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table server_availability_samples add column netbird_connected integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration28Up adds the two nullable power columns to server_telemetry_samples:
// cpu_power_w (CPU package watts) and system_power_w (total system watts). Both are
// nullable `real` (NULL = "not measured", distinct from 0 W). On a fresh DB the
// baseline (migration 1) already creates them, so this is a no-op there (same
// duplicate-tolerant per-column pattern as migration12Up); it only does real work
// upgrading a pre-existing DB.
func migration28Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, col := range []string{"cpu_power_w", "system_power_w"} {
		stmt := "alter table server_telemetry_samples add column " + col + " real"
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration29Up creates the server_hardware table — the 1:1 latest hardware
// inventory per server (PK server_id, upsert-overwrite), mirroring server_telemetry.
// On a fresh DB the baseline (migration 1) already creates it, so this is a no-op
// there (create table if not exists); it only does real work upgrading a
// pre-existing DB. No serials/UUIDs/MACs are ever stored (the report JSON schema
// has no such fields). No index needed (PK lookup).
func migration29Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmt := `create table if not exists server_hardware (
		server_id    text primary key references ai_servers(id) on delete cascade,
		collected_at ` + ts + ` not null,
		report_json  text not null default '',
		updated_at   ` + ts + ` not null
	)`
	return execTx(ctx, tx, dl, stmt)
}

// migration30Up adds the nullable cpu_temp_c (CPU package °C) column to
// server_telemetry_samples. Both dialects, duplicate-tolerant (mirrors v28 power).
// On a fresh DB the baseline (migration 1) already creates it, so this is a no-op
// there; it only does real work upgrading a pre-existing DB.
func migration30Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table server_telemetry_samples add column cpu_temp_c real"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration31Up adds ai_servers.agent_presence_timeout_seconds — the per-server
// override for the ServerAgent-presence window (0 = follow the system-wide
// agent_presence_timeout_seconds setting). On a fresh DB the baseline
// (migration 1) already creates the column, so the ADD COLUMN is a no-op there
// (same duplicate-tolerant single-column pattern as migration30Up). NO
// backfill: every existing row keeps the default 0 (follow system). Routing
// never reads it (absent from the candidate join).
func migration31Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column agent_presence_timeout_seconds integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration32Up adds model_mappings.vision_capable — whether the mapping's model
// accepts image inputs (default false = unknown/no). On a fresh DB the baseline
// (migration 1) already creates the column, so the ADD COLUMN is a no-op there
// (same duplicate-tolerant single-column pattern as migration30Up/31Up). NO
// backfill: every existing row keeps the default 0.
func migration32Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table model_mappings add column vision_capable integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration33Up adds model_mapping_benchmarks.vision_capable — the definitive
// vision-acceptance verdict recorded by a kind=="vision" benchmark history row
// (default false = unused/inconclusive for any other row). On a fresh DB the
// baseline (migration 1) already creates the column, so the ADD COLUMN is a
// no-op there (same duplicate-tolerant single-column pattern as migration32Up);
// it only does real work upgrading a pre-existing DB. NO backfill: every
// existing row keeps the default 0.
func migration33Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table model_mapping_benchmarks add column vision_capable integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration34Up adds the P1 energy-attribution columns to usage_events:
// energy_wh (attributed energy for the request, watt-hours), energy_marginal_wh
// (marginal energy vs. an idle baseline, watt-hours), and energy_source (how the
// value was derived, e.g. "measured"/"estimated"; ” = unknown). This is purely
// additive storage — no computation engine exists yet, so recordUsage leaves all
// three at their zero values (no-op invariant); a later phase populates them. On
// a fresh DB the baseline (migration 1) already creates the columns, so the ADD
// COLUMN is a no-op there (same duplicate-tolerant multi-column pattern as
// migration17Up/migration18Up); it only does real work upgrading a pre-existing
// DB. NO backfill: every existing row keeps the defaults 0/0/”.
func migration34Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"energy_wh real not null default 0",
		"energy_marginal_wh real not null default 0",
		"energy_source text not null default ''",
	}
	for _, col := range cols {
		stmt := "alter table usage_events add column " + col
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration35Up adds the four P1 energy-attribution columns to ai_servers:
// estimated_watts (operator estimate of typical draw), idle_watts (operator
// estimate of idle draw), price_per_kwh (electricity price used to cost
// energy use), and pue (datacenter Power Usage Effectiveness multiplier). All
// are `real not null default 0` ("unset / use default"). This is purely
// additive storage — no engine consumes these yet, so every existing row
// keeps the defaults (no-op invariant); a later phase populates + reads them.
// On a fresh DB the baseline (migration 1) already creates the columns, so
// the ADD COLUMN is a no-op there (same duplicate-tolerant multi-column
// pattern as migration34Up/migration17Up); it only does real work upgrading
// a pre-existing DB. NO backfill: every existing row keeps the defaults
// 0/0/0/0. Routing never reads these (absent from the ActiveMappingsForModel
// candidate join).
func migration35Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"estimated_watts real not null default 0",
		"idle_watts real not null default 0",
		"price_per_kwh real not null default 0",
		"pue real not null default 0",
	}
	for _, col := range cols {
		stmt := "alter table ai_servers add column " + col
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration36Up adds model_mappings.energy_wh_per_token — the manually-entered
// per-token energy coefficient (watt-hours per generated token) used (in a
// later phase) to attribute per-request energy for this mapping; default 0
// means "unknown". This is purely additive storage — no engine consumes it
// yet, so every existing row keeps the default (no-op invariant); a later
// phase auto-calibrates it via EWMA. On a fresh DB the baseline (migration 1)
// already creates the column, so the ADD COLUMN is a no-op there (same
// duplicate-tolerant single-column pattern as migration32Up/33Up); it only
// does real work upgrading a pre-existing DB. NO backfill: every existing row
// keeps the default 0.
func migration36Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table model_mappings add column energy_wh_per_token real not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration37Up adds ai_servers.price_unit — additive DISPLAY metadata for the
// per-server electricity price (price_per_kwh, unchanged): the price VALUE stays
// canonical EUR/kWh; price_unit only tells the frontend which unit to show/enter
// it in (portal.NormalizePriceUnit governs valid values, default "eur_cent"). No
// engine/backend logic reads it yet. On a fresh DB the baseline (migration 1)
// already creates the column, so the ADD COLUMN is a no-op there (same
// duplicate-tolerant single-column pattern as migration32Up/33Up/36Up); it only
// does real work upgrading a pre-existing DB. NO backfill: every existing row
// keeps the default 'eur_cent'.
func migration37Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column price_unit text not null default 'eur_cent'"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration38Up adds usage_events.cache_write_tokens — prompt tokens WRITTEN to the
// upstream's prompt cache this turn (Anthropic cache_creation_input_tokens; 0 for
// OpenAI/Responses, which have no cache-write count). It rounds out the read/write
// split alongside cached_tokens (cache READ) so Activity can compare against
// Anthropic-style cache pricing. On a fresh DB the baseline (migration 1) already
// creates the column, so this is a no-op there (same duplicate-tolerant pattern as
// migration6Up); it only does real work upgrading a pre-existing DB. NO backfill:
// every existing row keeps the default 0.
func migration38Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table usage_events add column cache_write_tokens integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration39Up adds usage_events.session_source + agent_id — the protocol-aware
// session provenance + Claude Code subagent id surfaced in Activity. Additive,
// duplicate-tolerant (no-op on a fresh DB whose baseline already has them). NO
// backfill: existing rows keep the empty-string default.
func migration39Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, stmt := range []string{
		"alter table usage_events add column session_source text not null default ''",
		"alter table usage_events add column agent_id text not null default ''",
	} {
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration40RawUp adds the Phase-1 service-accounts schema: new services /
// service_delegates / service_allowed_models tables, api_tokens.service_id +
// .kind, api_tokens.user_id NOT NULL -> nullable (a service token has no
// user), and usage_events.service_id/.service_name for attribution.
//
// It uses rawUp (a dedicated connection) rather than the shared up(tx, dl)
// signature because making api_tokens.user_id nullable on SQLite requires a
// full table rebuild (SQLite has no ALTER COLUMN DROP NOT NULL), and that
// rebuild's DROP TABLE api_tokens is dangerous under SQLite's default
// foreign_keys=ON: route_affinity.api_token_id references api_tokens(id) on
// delete cascade, and SQLite fires that cascade — deleting every
// route_affinity row — as a side effect of DROPping the referenced table, not
// just of a DELETE. Disabling the pragma prevents that, but SQLite documents
// the toggle as a silent no-op once a transaction (BEGIN) is already open, so
// it must happen OUTSIDE any transaction, on the SAME physical connection the
// rebuild's transaction then runs on — which a shared *sql.Tx (already
// BEGINed by the runner, on a connection picked from the pool) cannot
// guarantee. rawUp instead checks out its own *sql.Conn, toggles the pragma
// on it, runs the rebuild in a transaction on that same Conn, and restores the
// pragma before returning the connection to the pool (a pooled connection
// left with foreign_keys=OFF would silently drop referential-integrity
// enforcement for every later use of that connection).
//
// Postgres needs neither the dedicated connection nor the rebuild: a single
// `alter column user_id drop not null` suffices (no DROP TABLE, no cascade
// risk), so it just runs on a normal transaction.
func migration40RawUp(ctx context.Context, s *SQLStore, version int, name string) error {
	if s.dl.name() != "sqlite" {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if err := migration40UpPostgres(ctx, tx, s.dl); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.ExecContext(ctx, s.dl.rebind(
			`insert into schema_migrations (version, name, applied_at) values (?, ?, ?)`),
			version, name, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		return tx.Commit()
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire connection for migration %d: %w", version, err)
	}
	defer func() { _ = conn.Close() }()

	// Disable FK enforcement OUTSIDE any transaction (see doc comment above),
	// and always restore it before releasing the connection back to the pool.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); err != nil {
		return fmt.Errorf("disable foreign_keys for migration %d: %w", version, err)
	}
	defer func() { _, _ = conn.ExecContext(ctx, "PRAGMA foreign_keys=ON") }()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin migration %d: %w", version, err)
	}
	if err := migration40UpSQLite(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.ExecContext(ctx, s.dl.rebind(
		`insert into schema_migrations (version, name, applied_at) values (?, ?, ?)`),
		version, name, time.Now().UTC()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("record migration %d: %w", version, err)
	}
	return tx.Commit()
}

// migration40UpPostgres runs the Phase-1 service-accounts schema change on
// postgres: purely additive CREATE TABLE/INDEX (both duplicate-tolerant via
// IF NOT EXISTS — a no-op when baselineUp already created them on a fresh
// install) plus two `add column if not exists` and one idempotent
// `drop not null` — no table rebuild, no cascade risk.
func migration40UpPostgres(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists services (
			id text primary key,
			name text not null,
			description text not null default '',
			status text not null default 'active',
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists service_delegates (
			service_id text not null references services(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			can_manage_settings integer not null default 0,
			primary key (service_id, user_id)
		)`,
		`create index if not exists idx_service_delegates_user on service_delegates(user_id)`,
		`create table if not exists service_allowed_models (
			service_id text not null references services(id) on delete cascade,
			gateway_model_name text not null,
			primary key (service_id, gateway_model_name)
		)`,
		`alter table usage_events add column if not exists service_id text not null default ''`,
		`alter table usage_events add column if not exists service_name text not null default ''`,
		`alter table api_tokens add column if not exists service_id text references services(id) on delete cascade`,
		`alter table api_tokens add column if not exists kind text not null default 'user'`,
		`alter table api_tokens alter column user_id drop not null`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration40UpSQLite runs the Phase-1 service-accounts schema change on
// SQLite. The new tables + usage_events columns are additive/duplicate-
// tolerant (a no-op when baselineUp already created them on a fresh install).
// The api_tokens change (user_id NOT NULL -> nullable, + service_id + kind)
// needs a full table rebuild (create new, insert...select, drop, rename,
// recreate the hash index) — SQLite has no ALTER COLUMN DROP NOT NULL — and
// that rebuild is SKIPPED when api_tokens already has the final shape (the
// `kind` column present), which is always true on a fresh install where
// baselineUp already built it and api_tokens is therefore guaranteed empty at
// this point in the bootstrap sequence (no HTTP endpoint has run yet), so
// re-running the rebuild there would be safe but pointlessly heavy DDL.
func migration40UpSQLite(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists services (
			id text primary key,
			name text not null,
			description text not null default '',
			status text not null default 'active',
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists service_delegates (
			service_id text not null references services(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			can_manage_settings integer not null default 0,
			primary key (service_id, user_id)
		)`,
		`create index if not exists idx_service_delegates_user on service_delegates(user_id)`,
		`create table if not exists service_allowed_models (
			service_id text not null references services(id) on delete cascade,
			gateway_model_name text not null,
			primary key (service_id, gateway_model_name)
		)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}

	for _, stmt := range []string{
		`alter table usage_events add column service_id text not null default ''`,
		`alter table usage_events add column service_name text not null default ''`,
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}

	var hasKind int
	if err := tx.QueryRowContext(ctx, `select count(*) from pragma_table_info('api_tokens') where name = 'kind'`).Scan(&hasKind); err != nil {
		return fmt.Errorf("inspect api_tokens columns: %w", err)
	}
	if hasKind > 0 {
		// baselineUp already built the final shape (fresh install) — api_tokens
		// is guaranteed empty at this point in the bootstrap sequence.
		return nil
	}

	rebuild := []string{
		`create table if not exists api_tokens_new (
			id text primary key,
			user_id text references users(id) on delete cascade,
			name text not null,
			secret_hash text not null unique,
			secret_prefix text not null,
			status text not null,
			scopes text not null,
			expires_at ` + ts + `,
			last_used_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			model_override text not null default '',
			model_override_map text not null default '',
			log_communication integer not null default 0,
			secret integer not null default 0,
			service_id text references services(id) on delete cascade,
			kind text not null default 'user'
		)`,
		`insert into api_tokens_new (
			id, user_id, name, secret_hash, secret_prefix, status, scopes,
			expires_at, last_used_at, created_at, updated_at, model_override,
			model_override_map, log_communication, secret, service_id, kind
		) select
			id, user_id, name, secret_hash, secret_prefix, status, scopes,
			expires_at, last_used_at, created_at, updated_at, model_override,
			model_override_map, log_communication, secret, NULL, 'user'
		from api_tokens`,
		`drop table api_tokens`,
		`alter table api_tokens_new rename to api_tokens`,
		`create index if not exists idx_api_tokens_hash on api_tokens(secret_hash)`,
	}
	for _, stmt := range rebuild {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration41Up adds the principal_limits table — a generic per-principal
// (service or user) optional rate/quota/budget limits config (Phase 2 of the
// service-accounts work; principal_type/principal_id form the composite primary
// key, no FK since either target table may not exist for a given row). On a
// fresh DB the baseline (migration 1) already creates it, so this is a no-op
// there (same duplicate-tolerant "create table if not exists" pattern as
// migration11Up/migration23Up); it only does real work upgrading a
// pre-existing DB. token_quota_tokens is bigint (not integer/int4) because a
// monthly token-quota sum can exceed int4 (see the int4-overflow lesson from
// server_telemetry's byte columns, migration4Up).
func migration41Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmt := `create table if not exists principal_limits (
		principal_type text not null,
		principal_id text not null,
		rate_limit_requests integer not null default 0,
		rate_limit_window_seconds integer not null default 0,
		request_quota_requests integer not null default 0,
		request_quota_period text not null default '',
		token_quota_tokens bigint not null default 0,
		token_quota_period text not null default '',
		cost_budget_amount real not null default 0,
		cost_budget_period text not null default '',
		updated_at ` + ts + ` not null,
		primary key (principal_type, principal_id)
	)`
	return execTx(ctx, tx, dl, stmt)
}

// migration42Up makes route_affinity.user_id nullable. A service token (Phase
// 1) has no user (auth.Token.UserID == "" for a service-kind token), but the
// resolver's affinity upsert (internal/routing/resolver.go) always sets
// RouteAffinity.UserID = token.UserID and the column carries `not null
// references users(id) on delete cascade`. Writing "" against a NOT NULL FK
// column is checked against the referenced table (unlike a genuine SQL
// NULL, which the FK constraint exempts) and users("") never exists, so
// every UpsertAffinity for a service-token request failed the FK check on
// both sqlite (foreign_keys=ON) and a real postgres deployment — a 502 on
// EVERY service-token inference call in production. Memory-store mode has no
// FK enforcement, so this was invisible there. The fix mirrors
// migration40Up's identical api_tokens.user_id nullable change: the FK stays
// (a real user_id must still reference an existing user), only the NOT NULL
// is dropped, and the write path switches to a genuine NULL for an empty
// UserID (nullableTokenRef, sqlite_routes.go) with a coalesce-to-empty-string
// on read so the Go-side field stays a plain string either way. The affinity
// LOOKUP key is unaffected: unique(api_token_id, model, api_flavor,
// session_id) never includes user_id, which is purely a denormalized
// attribute (+ its cascade-on-user-delete) on the row.
//
// Postgres: a single `alter column drop not null` suffices — no rebuild, no
// cascade risk, because (unlike api_tokens) NOTHING references route_affinity
// via FK (it is only ever the child/referencing side), so altering or even
// dropping it can never cascade into another table.
//
// SQLite has no `ALTER COLUMN DROP NOT NULL`, and — for the same
// nothing-references-route_affinity reason above — this does NOT need
// migration40Up's dedicated-connection foreign_keys=OFF dance (that dance
// exists solely to survive DROPping a table other tables reference; dropping
// route_affinity itself is safe under foreign_keys=ON regardless). route_
// affinity is also a pure, self-healing TTL cache: every row expires and is
// re-created on the next matching request, never authoritative data, so a
// plain drop+recreate (unlike a rebuild+backfill) is the simpler correct
// choice here — accepted, harmless cost: any in-flight affinity pin is lost
// across this one-time upgrade and re-pins on the next request. The unique
// constraint and its lookup index are recreated identically; idempotent on a
// fresh install too (baselineUp already creates the nullable shape, so this
// just drops and rebuilds the same empty table).
func migration42Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if dl.name() != "sqlite" {
		return execTx(ctx, tx, dl, `alter table route_affinity alter column user_id drop not null`)
	}
	ts := dl.timestampType()
	stmts := []string{
		`drop table if exists route_affinity`,
		`create table route_affinity (
			id text primary key,
			api_token_id text not null references api_tokens(id) on delete cascade,
			user_id text references users(id) on delete cascade,
			model text not null,
			api_flavor text not null,
			session_id text not null,
			application_id text not null references applications(id) on delete cascade,
			server_id text not null references ai_servers(id) on delete cascade,
			expires_at ` + ts + ` not null,
			last_used_at ` + ts + ` not null,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			resolved_model text not null default '',
			unique(api_token_id, model, api_flavor, session_id)
		)`,
		`create index if not exists idx_route_affinity_lookup on route_affinity(api_token_id, model, api_flavor, session_id, expires_at)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration43Up widens every float column from `real` (float32) to
// `double precision` (float64) on postgres. The v1 baseline originally declared
// these columns as `real`, which stores only ~7 significant digits — but the Go
// structs backing them are all `float64`. Writing a float64 into a `real` column
// silently rounds it, and a `sum(...)` aggregation (e.g. the /api/portal/usage/groups
// endpoint's SUM(energy_wh)) accumulates the ~1e-7 relative drift into a visibly
// wrong total (0.1 + 1.0 read back as 1.1000000014901161 instead of 1.1). SQLite's
// REAL is already 8-byte (and it does not support ALTER COLUMN TYPE), so this is a
// no-op there; fresh DBs get `double precision` directly from the corrected
// baseline. This mirrors migration4Up (the int4->bigint byte-column fix): the same
// class of postgres-only column-type latent bug, corrected in the baseline for
// fresh installs and ALTERed here for pre-existing deployments.
//
// Every one of these columns backs a Go float64 field (verified against the
// routing/usage structs); none is intentionally float32. Altering an
// already-`double precision` column (e.g. after a fresh baseline created it that
// way) is a harmless no-op, so this runs unconditionally on postgres regardless of
// which historical migration first created each column.
func migration43Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	if dl.name() != "postgres" {
		return nil
	}
	// {table, column} for every float64-backed column. Grouped by table for
	// readability; the order does not matter (each ALTER is independent).
	cols := [][2]string{
		{"usage_events", "prompt_per_second"},
		{"usage_events", "tokens_per_second"},
		{"usage_events", "energy_wh"},
		{"usage_events", "energy_marginal_wh"},
		{"ai_servers", "estimated_watts"},
		{"ai_servers", "idle_watts"},
		{"ai_servers", "price_per_kwh"},
		{"ai_servers", "pue"},
		{"server_telemetry", "cpu_load"},
		{"server_telemetry", "error_rate"},
		{"server_telemetry_samples", "cpu_util_pct"},
		{"server_telemetry_samples", "load1"},
		{"server_telemetry_samples", "load5"},
		{"server_telemetry_samples", "load15"},
		{"server_telemetry_samples", "cpu_power_w"},
		{"server_telemetry_samples", "system_power_w"},
		{"server_telemetry_samples", "cpu_temp_c"},
		{"model_mappings", "gen_tokens_per_second"},
		{"model_mappings", "prompt_tokens_per_second"},
		{"model_mappings", "energy_wh_per_token"},
		{"model_mappings", "gen_tokens_per_second_at_capacity"},
		{"model_mapping_benchmarks", "gen_tokens_per_second"},
		{"model_mapping_benchmarks", "prompt_tokens_per_second"},
		{"principal_limits", "cost_budget_amount"},
	}
	for _, c := range cols {
		stmt := "alter table " + c[0] + " alter column " + c[1] + " type double precision"
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration44Up creates the user-groups schema: a system→admin→user group tree
// (user_groups), content/peer membership rows (user_group_members), and
// promoted co-manager rows (user_group_managers) — modeled on the existing
// server_owners many-to-many pattern (mirrors migration22Up's table-create
// style). It then seeds two fixed-id default groups (system + admin, the admin
// group parented under the system group) and enrolls every EXISTING user (at
// the time this migration runs) as a member of both — so an upgrade never
// leaves a pre-existing user group-less, while a fresh install simply gets two
// empty default groups. The seed inserts are on-conflict-do-nothing so a
// re-run (or a store that somehow re-applies this migration) is safe.
func migration44Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists user_groups (
			id text primary key,
			tier text not null,
			name text not null,
			parent_group_id text references user_groups(id) on delete cascade,
			owner_user_id text references users(id) on delete set null,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create index if not exists idx_user_groups_parent on user_groups(parent_group_id)`,
		`create index if not exists idx_user_groups_owner on user_groups(owner_user_id)`,
		`create index if not exists idx_user_groups_tier on user_groups(tier)`,
		`create table if not exists user_group_members (
			group_id text not null references user_groups(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			state text not null,
			invited_by text not null default '',
			created_at ` + ts + ` not null,
			primary key (group_id, user_id)
		)`,
		`create index if not exists idx_user_group_members_user on user_group_members(user_id)`,
		`create table if not exists user_group_managers (
			group_id text not null references user_groups(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			created_at ` + ts + ` not null,
			primary key (group_id, user_id)
		)`,
		`create index if not exists idx_user_group_managers_user on user_group_managers(user_id)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	// Seed the two default groups (fixed ids -> idempotent; owner NULL).
	seed := []struct {
		id, tier, name, parent string
	}{
		{DefaultSystemGroupID, GroupTierSystem, "Standard", ""},
		{DefaultAdminGroupID, GroupTierAdmin, "Standard", DefaultSystemGroupID},
	}
	for _, g := range seed {
		var parent any
		if g.parent != "" {
			parent = g.parent
		} // else nil -> NULL
		if _, err := tx.ExecContext(ctx, dl.rebind(
			`insert into user_groups (id, tier, name, parent_group_id, owner_user_id, created_at, updated_at)
			 values (?, ?, ?, ?, NULL, ?, ?) on conflict (id) do nothing`),
			g.id, g.tier, g.name, parent, now, now); err != nil {
			return err
		}
	}
	// Seed all existing users into BOTH default groups as members. The
	// trailing "where 1=1" is load-bearing on sqlite: modernc.org/sqlite's
	// parser cannot disambiguate a bare "FROM users ON CONFLICT ..." (it
	// tries to read "on" as a join constraint on the FROM clause) unless a
	// WHERE clause intervenes between FROM and ON CONFLICT; postgres has no
	// such ambiguity and accepts it as a no-op predicate.
	for _, gid := range []string{DefaultSystemGroupID, DefaultAdminGroupID} {
		if _, err := tx.ExecContext(ctx, dl.rebind(
			`insert into user_group_members (group_id, user_id, state, invited_by, created_at)
			 select ?, id, 'member', '', ? from users where 1=1
			 on conflict (group_id, user_id) do nothing`),
			gid, now); err != nil {
			return err
		}
	}
	return nil
}

// migration45Up creates the projects schema: user-owned projects
// (projects), a project's member users (project_members), and a project's
// assigned user-groups (project_groups) — mirrors migration22Up's table-create
// style. It then additively adds `project_id` to api_tokens (nullable FK,
// ON DELETE SET NULL — a project delete detaches its tokens rather than
// deleting/disabling them) and `project_id`/`project_name` to usage_events
// (PLAIN text columns, deliberately NOT a foreign key: usage history must
// never be cascaded or nulled by a project delete — see the "CRITICAL" note
// in the design brief).
//
// None of this is duplicated into baselineCreateStatements — exactly like
// migration44Up's user_groups/user_group_members/user_group_managers, this
// migration is entirely self-sufficient and is the SOLE creator of all three
// tables on every database, fresh or upgrading. baselineCreateStatements is a
// frozen historical v1 snapshot, not a rolling mirror of the current schema;
// a fresh install runs v1..v44 first (so users AND user_groups already
// exist) and only then v45, which creates projects/project_members/
// project_groups and ALTERs api_tokens/usage_events — identical to what an
// upgrading pre-v45 database gets. The additive-column ALTERs mirror
// migration39Up's duplicate-tolerant pattern (both dialects).
func migration45Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists projects (
			id text primary key,
			name text not null,
			description text not null default '',
			owner_user_id text references users(id) on delete set null,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create index if not exists idx_projects_owner on projects(owner_user_id)`,
		`create table if not exists project_members (
			project_id text not null references projects(id) on delete cascade,
			user_id text not null references users(id) on delete cascade,
			created_at ` + ts + ` not null,
			primary key (project_id, user_id)
		)`,
		`create index if not exists idx_project_members_user on project_members(user_id)`,
		`create table if not exists project_groups (
			project_id text not null references projects(id) on delete cascade,
			group_id text not null references user_groups(id) on delete cascade,
			created_at ` + ts + ` not null,
			primary key (project_id, group_id)
		)`,
		`create index if not exists idx_project_groups_group on project_groups(group_id)`,
	}
	for _, s := range stmts {
		if err := execTx(ctx, tx, dl, s); err != nil {
			return err
		}
	}
	// Additive columns (duplicate-tolerant, both dialects) — mirror migration39Up.
	adds := []string{
		"alter table api_tokens add column project_id text references projects(id) on delete set null",
		"alter table usage_events add column project_id text not null default ''",
		"alter table usage_events add column project_name text not null default ''",
	}
	for _, col := range adds {
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, col); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration46Up adds projects.coupled_group_id (the coupled-projects feature,
// spec 2026-08-09): a nullable FK -> user_groups(id) ON DELETE SET NULL, so
// deleting the coupled group turns the project into a normal (uncoupled) one.
// Additive, duplicate-tolerant, both dialects — mirrors migration45Up's
// api_tokens.project_id ADD COLUMN.
func migration46Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	col := "alter table projects add column coupled_group_id text references user_groups(id) on delete set null"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration47Up adds sessions.elevated_until (system-admin step-up mode,
// spec 2026-08-10): a nullable timestamp — when in the future the session's
// principal carries the `system` scope. Additive, duplicate-tolerant, both
// dialects — mirrors migration46Up.
func migration47Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := "timestamp"
	if dl.name() == "postgres" {
		ts = "timestamptz"
	}
	col := "alter table sessions add column elevated_until " + ts
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration48Up adds two per-co-manager permission flags to
// user_group_managers (per-Admin-Group co-manager permissions, spec
// 2026-08-10): can_manage_users / can_manage_group, both
// `integer not null default 1` — a co-manager row that predates this
// migration (and a brand-new row inserted by SetUserGroupManager, which does
// not name these columns) both pick up the default of "full co-manager
// rights", preserving today's behavior byte-for-byte until Phase B starts
// consuming the flags. user_group_managers itself is NOT part of
// baselineCreateStatements (that table is exclusively created by
// migration44Up, an already-shipped migration this change must not edit —
// see the "NEVER edit or reorder" rule on the migrations list above); a
// fresh install runs migration44Up then this ALTER in the same pass, so a
// fresh DB ends up with the columns exactly like an upgrading one. Additive,
// duplicate-tolerant, both dialects — mirrors migration45Up's multi-column
// ALTER loop.
func migration48Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	adds := []string{
		"alter table user_group_managers add column can_manage_users integer not null default 1",
		"alter table user_group_managers add column can_manage_group integer not null default 1",
	}
	for _, col := range adds {
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, col); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration49Up adds a THIRD per-co-manager permission flag to
// user_group_managers (admin-group permissions Phase B, spec 2026-08-10):
// can_manage_servers, `integer not null default 1` — mirrors migration48Up
// exactly (same table, same default-1 rationale: a co-manager row that
// predates this migration, and a brand-new row inserted by
// SetUserGroupManager [which does not name this column either], both pick up
// the default of "full co-manager rights", so an existing co-manager keeps
// server-management capability byte-for-byte until Phase B's later tasks link
// servers to admin groups and start consuming the flag). Additive,
// duplicate-tolerant, both dialects.
func migration49Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	col := "alter table user_group_managers add column can_manage_servers integer not null default 1"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration50Up wires an ai-server to an admin group (admin-group permissions
// Phase B, spec 2026-08-10 — Task 2): a per-server system_group_id (the
// containment root — a denormalized pointer, NOT a foreign key, mirroring the
// "opaque string" treatment of the netbird_* columns; "" = an ungrouped
// legacy server) and a new server_admin_groups join table linking a server to
// the ONE OR MORE admin groups that may manage it (a co-manager with
// can_manage_servers, or the owner, of a linked admin group gets management
// rights over the server — consumed by a later task, Task 3/4; this task is
// store-layer only).
//
// ai_servers predates this table split (it is created by baselineUp, not a
// later migration), so system_group_id is threaded BOTH ways — appended to
// baselineCreateStatements' ai_servers CREATE (so a fresh DB already has the
// column) AND added here via a duplicate-tolerant ADD COLUMN (so an upgrading
// DB gets it too) — exactly like every other ai_servers column added after
// migration18Up (netbird_enabled, netbird_group_ids, ..., price_unit).
// RETIRED CONVENTION, kept here only as history: as of v60,
// baselineCreateStatements is frozen (see its doc comment) and no later
// migration back-ports a column into it this way — a fresh install instead
// gets the column by replaying the full migration chain.
//
// server_admin_groups itself is a BRAND NEW table, so — mirroring
// migration44Up's user_groups / migration45Up's project_groups — it is NOT
// duplicated into baselineCreateStatements: this migration is the SOLE
// creator of the table on every database, fresh or upgrading (a fresh
// install runs v1..v49 first, so ai_servers and user_groups already exist,
// then v50 creates server_admin_groups + adds the column — identical to what
// an upgrading pre-v50 database gets).
//
// group_id has a REAL foreign key -> user_groups(id) ON DELETE CASCADE (so
// deleting an admin group drops its server links) and server_id -> ai_servers(id)
// ON DELETE CASCADE (so deleting a server drops its group links) — both
// enforced by the SQL dialects; the routing.MemoryStore mirror (Task 2, same
// commit) can only cascade on server delete (ai_servers and server_admin_groups
// both live in routing.MemoryStore) — it has no visibility into a user-group
// delete (user_groups lives in the separate portal.MemoryDirectory), the same
// structural gap server_owners already has for a (hypothetical) user delete;
// accepted as a known, documented memory-mode limitation, consistent with the
// codebase's established "memory mode does not enforce cross-store FK cascades"
// pattern (only sqlite/postgres do).
func migration50Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	col := "alter table ai_servers add column system_group_id text not null default ''"
	if dl.name() == "postgres" {
		if err := execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1)); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, col); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	stmts := []string{
		`create table if not exists server_admin_groups (
			server_id text not null,
			group_id text not null,
			created_at ` + ts + ` not null,
			unique(server_id, group_id),
			foreign key(server_id) references ai_servers(id) on delete cascade,
			foreign key(group_id) references user_groups(id) on delete cascade
		)`,
		`create index if not exists idx_server_admin_groups_group on server_admin_groups(group_id)`,
	}
	for _, s := range stmts {
		if err := execTx(ctx, tx, dl, s); err != nil {
			return err
		}
	}
	return nil
}

// migration51Up adds a FOURTH per-co-manager permission flag to
// user_group_managers (admin-group permissions Phase C, spec 2026-08-10):
// can_manage_services, `integer not null default 1` — mirrors migration49Up
// exactly (same table, same default-1 rationale: a co-manager row that
// predates this migration, and a brand-new row inserted by
// SetUserGroupManager [which does not name this column either], both pick up
// the default of "full co-manager rights", so an existing co-manager keeps
// service-management capability byte-for-byte until Phase C's later tasks
// link services to admin groups and start consuming the flag). Additive,
// duplicate-tolerant, both dialects.
func migration51Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	col := "alter table user_group_managers add column can_manage_services integer not null default 1"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration52Up wires a service (service account) to an admin group
// (admin-group permissions Phase C, spec 2026-08-10 — Task 2): a per-service
// system_group_id (the containment root — a denormalized pointer, NOT a
// foreign key, mirroring migration50Up's ai_servers.system_group_id
// treatment; "" = an ungrouped legacy service) and a new
// service_admin_groups join table linking a service to the ONE OR MORE
// admin groups that may manage it (a co-manager with can_manage_services,
// or the owner, of a linked admin group gets management rights over the
// service — consumed by a later task; this task is store-layer only). This
// is the SERVICES analog of migration50Up's server_admin_groups.
//
// services predates this table split (it is created by baselineUp, not a
// later migration), so system_group_id is threaded BOTH ways — appended to
// baselineCreateStatements' services CREATE (so a fresh DB already has the
// column) AND added here via a duplicate-tolerant ADD COLUMN (so an
// upgrading DB gets it too) — exactly like ai_servers.system_group_id.
// RETIRED CONVENTION, kept here only as history: as of v60,
// baselineCreateStatements is frozen (see its doc comment) and no later
// migration back-ports a column into it this way.
//
// service_admin_groups itself is a BRAND NEW table, so it is NOT duplicated
// into baselineCreateStatements: this migration is the SOLE creator of the
// table on every database, fresh or upgrading (a fresh install runs v1..v51
// first, so services and user_groups already exist, then v52 creates
// service_admin_groups + adds the column — identical to what an upgrading
// pre-v52 database gets).
//
// group_id has a REAL foreign key -> user_groups(id) ON DELETE CASCADE (so
// deleting an admin group drops its service links) and service_id ->
// services(id) ON DELETE CASCADE (so deleting a service drops its group
// links) — both enforced by the SQL dialects; the routing.MemoryStore
// mirror (Task 2, same commit) can only cascade on service delete (services
// and service_admin_groups both live in routing.MemoryStore) — it has no
// visibility into a user-group delete (user_groups lives in the separate
// portal.MemoryDirectory), the same structural gap server_admin_groups
// already has; accepted as a known, documented memory-mode limitation.
func migration52Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	col := "alter table services add column system_group_id text not null default ''"
	if dl.name() == "postgres" {
		if err := execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1)); err != nil {
			return err
		}
	} else if _, err := tx.ExecContext(ctx, col); err != nil {
		if !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	stmts := []string{
		`create table if not exists service_admin_groups (
			service_id text not null,
			group_id text not null,
			created_at ` + ts + ` not null,
			unique(service_id, group_id),
			foreign key(service_id) references services(id) on delete cascade,
			foreign key(group_id) references user_groups(id) on delete cascade
		)`,
		`create index if not exists idx_service_admin_groups_group on service_admin_groups(group_id)`,
	}
	for _, s := range stmts {
		if err := execTx(ctx, tx, dl, s); err != nil {
			return err
		}
	}
	return nil
}

// migration53Up adds user_group_managers.can_manage_resources (Resource
// Groups Phase 1, Task 1) — the FIFTH per-admin-group co-manager flag,
// `integer not null default 1` — mirrors migration51Up exactly (same table,
// same default-1 rationale: a co-manager row that predates this migration,
// and a brand-new row inserted by SetUserGroupManager [which does not name
// this column either], both pick up the default of "full co-manager
// rights", so an existing co-manager keeps resource-management capability
// byte-for-byte until a later Phase 1 task links resources to admin groups
// and starts consuming the flag). Additive, duplicate-tolerant, both
// dialects.
func migration53Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	col := "alter table user_group_managers add column can_manage_resources integer not null default 1"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(col, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, col); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration54Up creates the resource_groups management structure (Resource
// Groups Phase 1, spec 2026-08-11 — Task 2): a brand-new entity, `resource_groups`
// (id, name, a system_group_id containment root mirroring
// ai_servers/services.system_group_id, status), plus TWO n:m join tables —
// `resource_group_admin_groups` (which admin groups may MANAGE a resource
// group, the same shape as service_admin_groups/server_admin_groups) and
// `resource_group_servers` (which AI-servers are MEMBERS of a resource
// group — a distinct relationship: membership, not management). A later
// task consumes both; this migration is store-layer only, routing never
// reads any of the three tables.
//
// Unlike services (which predates its admin-group join and therefore needed
// system_group_id threaded into BOTH baselineCreateStatements AND a
// duplicate-tolerant ADD COLUMN — migration52Up), resource_groups itself has
// no baseline predecessor: it is a brand-new table, so — exactly like
// service_admin_groups/server_admin_groups — ALL THREE tables here are
// created SOLELY by this migration, on every database, fresh or upgrading
// (a fresh install runs v1..v53 first, so user_groups and ai_servers already
// exist, then v54 creates resource_groups + both join tables — identical to
// what an upgrading pre-v54 database gets). None of the three is duplicated
// into baselineCreateStatements.
//
// group_id has a REAL foreign key -> user_groups(id) ON DELETE CASCADE (an
// admin group's resource-group management links are dropped when it is
// deleted) and resource_group_id -> resource_groups(id) ON DELETE CASCADE on
// BOTH join tables (deleting a resource group drops its member-server and
// managing-admin-group links). server_id has a REAL foreign key ->
// ai_servers(id) ON DELETE CASCADE (a deleted server's resource-group
// membership links are dropped too). The routing.MemoryStore mirror can
// cascade a resource-group delete (both joins live in routing.MemoryStore
// alongside resource_groups) and an AI-server delete (ai_servers also lives
// there), but has no visibility into a user-group delete (user_groups lives
// in the separate portal.MemoryDirectory) — the same structural gap
// server_admin_groups/service_admin_groups already document as an accepted,
// known memory-mode limitation.
func migration54Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists resource_groups (
			id text primary key,
			name text not null,
			system_group_id text not null default '',
			status text not null default 'active',
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create table if not exists resource_group_admin_groups (
			resource_group_id text not null,
			group_id text not null,
			created_at ` + ts + ` not null,
			unique(resource_group_id, group_id),
			foreign key(resource_group_id) references resource_groups(id) on delete cascade,
			foreign key(group_id) references user_groups(id) on delete cascade
		)`,
		`create index if not exists idx_resource_group_admin_groups_group on resource_group_admin_groups(group_id)`,
		`create table if not exists resource_group_servers (
			resource_group_id text not null,
			server_id text not null,
			created_at ` + ts + ` not null,
			unique(resource_group_id, server_id),
			foreign key(resource_group_id) references resource_groups(id) on delete cascade,
			foreign key(server_id) references ai_servers(id) on delete cascade
		)`,
		`create index if not exists idx_resource_group_servers_server on resource_group_servers(server_id)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration55Up creates resource_group_provisions (Resource Groups Phase 2
// provisioning, spec 2026-08-12 — Task 1): a polymorphic n:m join recording
// which principals a resource group's servers are "provisioned for" —
// target_kind identifies the principal TYPE (routing.ProvisionKind* — one of
// user_group/admin_group/user/service) and target_id its id. Unlike
// resource_group_admin_groups/resource_group_servers (both FK'd to a single,
// well-known table), target_id is deliberately NOT a foreign key: it points
// at whichever table target_kind names (user_groups, users, or the separate
// services table), and a store-layer FK cannot express a type-conditional
// reference. A dangling target (e.g. its user-group was deleted) simply never
// matches again — no cascade needed on that side. resource_group_id DOES
// carry a real foreign key -> resource_groups(id) ON DELETE CASCADE, so
// deleting a resource group drops every one of its provisioning rows (mirrors
// resource_group_servers/resource_group_admin_groups from migration54Up).
//
// Like resource_group_admin_groups/resource_group_servers, this table has no
// baseline predecessor — it is created SOLELY by this migration, on every
// database, fresh or upgrading (a fresh install runs v1..v54 first, so
// resource_groups already exists, then v55 creates this join table).
func migration55Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists resource_group_provisions (
			resource_group_id text not null,
			target_kind text not null,
			target_id text not null,
			created_at ` + ts + ` not null,
			unique(resource_group_id, target_kind, target_id),
			foreign key(resource_group_id) references resource_groups(id) on delete cascade
		)`,
		`create index if not exists idx_rg_provisions_kind_target
			on resource_group_provisions(target_kind, target_id)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration56Up adds api_tokens.server_override + server_override_force_unreachable
// -- the per-token AI-server override (bypasses provisioning/affinity/maintenance
// exclusion) from the server-override design. On a fresh DB the baseline already
// creates both columns (see baselineCreateStatements' api_tokens block), so this
// is a no-op there (same duplicate-tolerant add-column pattern as migration8Up);
// it only does real work upgrading a pre-existing DB.
func migration56Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	for _, stmt := range []string{
		"alter table api_tokens add column server_override text not null default ''",
		"alter table api_tokens add column server_override_force_unreachable integer not null default 0",
	} {
		if dl.name() == "postgres" {
			if err := execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1)); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
				continue
			}
			return err
		}
	}
	return nil
}

// migration57Up creates the certificates table (ACME/Let's-Encrypt design). The
// domain is the natural primary key; server_id is a nullable FK so deleting a
// server takes its certificate row with it. Times are nullable: a skipped/errored
// row has no certificate yet.
func migration57Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists certificates (
			domain text primary key,
			kind text not null,
			server_id text,
			fullchain_pem text not null default '',
			key_sealed text not null default '',
			fingerprint text not null default '',
			issuer_fingerprint text not null default '',
			not_before ` + ts + `,
			not_after ` + ts + `,
			issued_at ` + ts + `,
			status text not null default 'pending',
			last_error text not null default '',
			attempt_count integer not null default 0,
			next_attempt_at ` + ts + `,
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null,
			foreign key(server_id) references ai_servers(id) on delete cascade
		)`,
		`create index if not exists idx_certificates_server on certificates(server_id)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return nil
}

// migration58Up adds ai_servers.certificate_override — the per-server ACME
// opt-in/opt-out whose MEANING depends on the system-wide cert_server_scope
// ("" = follow the scope, "include" = manage even under scope=selected,
// "exclude" = never manage under scope=all). One 3-state column (not two
// booleans) so flipping the scope can never resurrect a stale opposite flag.
// Routing never reads it. On a fresh DB the baseline already creates the column,
// so this only does real work upgrading an existing DB.
func migration58Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column certificate_override text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration59Up adds applications.proxy_listen_port — the P4 gateway-assigned
// TLS port the agent's local proxy listens on for this application (0 = not
// yet assigned; see routing.AssignProxyListenPort). Byte-neutral: every
// existing row defaults to 0, unchanged behavior until a later task starts
// reading it. On a fresh DB the baseline already creates the column, so this
// only does real work upgrading an existing DB.
func migration59Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table applications add column proxy_listen_port integer not null default 0"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration60Up adds ai_servers.https_switch_override — P4's per-server
// https-auto-switch opt-in/opt-out whose MEANING depends on the system-wide
// cert_https_switch_mode ("" = follow the mode, "include"/"exclude" resolved
// by httpsSwitchInScope). One 3-state column (not two booleans), mirroring
// certificate_override/migration58Up exactly, so a mode flip can never
// resurrect a stale opposite flag. Routing never reads it. On a fresh DB the
// baseline already creates the column, so this only does real work upgrading
// an existing DB.
func migration60Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	stmt := "alter table ai_servers add column https_switch_override text not null default ''"
	if dl.name() == "postgres" {
		return execTx(ctx, tx, dl, strings.Replace(stmt, "add column ", "add column if not exists ", 1))
	}
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return nil
		}
		return err
	}
	return nil
}

// migration61Up adds usage_events.requested_model — the model name exactly as
// the client sent it, BEFORE resolveModelOverride applied any token model
// override (issue #7). TEXT NOT NULL DEFAULT ” so pre-existing rows read
// back "" = "unknown" (whether an override fired historically is not
// recorded), mirroring certificate_override/migration58Up's empty-default
// convention. baselineCreateStatements is FROZEN as of v60 and deliberately
// does NOT carry this column (see its doc comment: back-porting a new column
// into the frozen baseline is exactly what that policy forbids) — a fresh DB
// gets requested_model from THIS migration too, by replaying the v1 baseline
// and then v2..vN in order. First migration to call addColumnIfMissing
// instead of inlining the duplicate-tolerant add-column block.
func migration61Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	return addColumnIfMissing(ctx, tx, dl, "usage_events", "requested_model text not null default ''")
}

// migration62Up adds the model-group selection settings: serve only loaded
// members, order by measured speed, a minimum-speed floor with its fallback,
// and the climb margin. Defaults reproduce the pre-feature behavior exactly.
func migration62Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"loaded_only integer not null default 0",
		"member_order text not null default 'priority'",
		"climb_speed_margin_percent integer not null default 20",
		"min_tokens_per_second real not null default 0",
		"min_speed_fallback text not null default 'error'",
	}
	for _, col := range cols {
		if err := addColumnIfMissing(ctx, tx, dl, "model_groups", col); err != nil {
			return err
		}
	}
	return nil
}

// migration63Up adds the per-token unknown-model redirect settings and the
// last-used-model marker. Defaults reproduce the pre-feature behavior exactly:
// the redirect is off, so resolution is unchanged for every existing token.
// The model-override map needs NO migration — the column holds a JSON string
// and DecodeModelOverrideRules reads the legacy shape as rules with both
// listing switches false.
//
// ROLLBACK. Dropping back to a pre-v63 binary is safe for the four columns
// added here: they are append-only, and an older binary simply never selects
// them. api_tokens.model_override_map is the one place where a rollback can
// still lose data, and exactly one case does:
//
//   - A token whose override rows use NEITHER new listing switch loses nothing.
//     EncodeModelOverrideRules writes such rows in the legacy "name":"target"
//     string form on purpose (see its doc comment), which is byte-identical to
//     what the pre-branch encoder wrote, so the old decoder reads them back
//     unchanged. A deployment that never touches the switches is fully
//     downgradable.
//   - A token with AT LEAST ONE row using `offer` or `hide_target` loses its
//     ENTIRE override map. The pre-branch decoder unmarshals the column into
//     map[string]string and returns nil on the first object-valued row — for
//     the whole map, taking the untouched sibling rows with it — and the next
//     save under the old binary then writes "" over the column, making the loss
//     permanent. Nothing on this side can prevent that; the all-or-nothing
//     decoder lives in the binary being rolled back to.
//
// So the residual is opt-in and per token: only tokens an operator explicitly
// configured with the new switches are at risk, and the exposure is their
// per-model override rows (never the catch-all, which has its own column).
// Both cases are pinned by tests in token_override_test.go against a verbatim
// copy of the pre-branch decoder.
func migration63Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	cols := []string{
		"last_used_model text not null default ''",
		"unknown_model_redirect integer not null default 0",
		"unknown_model_redirect_blocked integer not null default 0",
		"unknown_model_fallback text not null default ''",
	}
	for _, col := range cols {
		if err := addColumnIfMissing(ctx, tx, dl, "api_tokens", col); err != nil {
			return err
		}
	}
	return nil
}

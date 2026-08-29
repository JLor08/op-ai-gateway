// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestMigrationV3TelemetrySamplesTable proves migration v3 is recorded and the
// server_telemetry_samples table (with its columns) exists on a fresh DB.
func TestMigrationV3TelemetrySamplesTable(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var recorded int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from schema_migrations where version = ?`), 3).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations v3: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("schema_migrations version 3 count = %d, want 1", recorded)
	}

	var samples int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from server_telemetry_samples`)).Scan(&samples); err != nil {
		t.Fatalf("count server_telemetry_samples: %v", err)
	}
	if samples != 0 {
		t.Fatalf("server_telemetry_samples count = %d, want 0", samples)
	}
}

// TestMigration44CreatesUserGroupTablesAndSeedsFresh proves migration v44 creates
// the user-groups schema and seeds the two fixed-id default groups (system +
// admin, admin parented under system) on a FRESH database. There are no users
// yet at migrate time, so both default groups are empty — the existing-user
// seed is proven separately by TestMigration44SeedsExistingUsersOnUpgrade.
func TestMigration44CreatesUserGroupTablesAndSeedsFresh(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()

	sys, err := s.UserGroupByID(ctx, DefaultSystemGroupID)
	if err != nil {
		t.Fatalf("default system group: %v", err)
	}
	if sys.Tier != GroupTierSystem || sys.ParentGroupID != "" || sys.OwnerUserID != "" {
		t.Fatalf("default system group wrong: %+v", sys)
	}
	adm, err := s.UserGroupByID(ctx, DefaultAdminGroupID)
	if err != nil {
		t.Fatalf("default admin group: %v", err)
	}
	if adm.Tier != GroupTierAdmin || adm.ParentGroupID != DefaultSystemGroupID || adm.OwnerUserID != "" {
		t.Fatalf("default admin group wrong: %+v", adm)
	}

	for _, gid := range []string{DefaultSystemGroupID, DefaultAdminGroupID} {
		members, err := s.UserGroupMembers(ctx, gid)
		if err != nil {
			t.Fatalf("members of %s: %v", gid, err)
		}
		if len(members) != 0 {
			t.Fatalf("group %s: got %d members on a fresh DB, want 0", gid, len(members))
		}
	}
}

// TestMigration44SeedsExistingUsersOnUpgrade proves the migration's real
// upgrade guarantee: a user that existed BEFORE migration v44 ran is enrolled
// as a member of BOTH default groups. Migrate() on a fresh temp DB already
// applies v44 (with zero users, per the test above), so — mirroring the
// existing TestMigration20BackfillPeerManaged pattern for re-proving a
// migration's data-dependent behavior — this creates users on an already-
// fully-migrated store and then re-invokes migration44Up directly inside a
// manual transaction. migration44Up's seed inserts are all
// on-conflict-do-nothing/idempotent, so re-running it is safe and exercises
// exactly the "insert into user_group_members select ... from users" seed
// against a users table that is no longer empty — the same statement an
// upgrade path runs against a database that already has real users.
func TestMigration44SeedsExistingUsersOnUpgrade(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)

	if err := s.CreateUser(ctx, User{ID: "usr_a", Email: "a@x", DisplayName: "A", Role: "system_admin", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create usr_a: %v", err)
	}
	if err := s.CreateUser(ctx, User{ID: "usr_b", Email: "b@x", DisplayName: "B", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create usr_b: %v", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration44Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration44Up: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	for _, gid := range []string{DefaultSystemGroupID, DefaultAdminGroupID} {
		members, err := s.UserGroupMembers(ctx, gid)
		if err != nil {
			t.Fatalf("members of %s: %v", gid, err)
		}
		if len(members) != 2 {
			t.Fatalf("group %s: got %d members, want 2 (usr_a, usr_b): %+v", gid, len(members), members)
		}
		for _, m := range members {
			if m.State != GroupStateMember {
				t.Fatalf("group %s member %s state=%q, want %q", gid, m.UserID, m.State, GroupStateMember)
			}
		}
	}
}

// TestMigration45ProjectsSchema proves migration v45 creates the three new
// projects tables (projects/project_members/project_groups — each round-tripped
// via a raw insert+select, with foreign_keys=ON so the FKs to users/projects/
// user_groups must actually resolve) and additively widens api_tokens with a
// nullable project_id FK and usage_events with plain (non-FK) project_id/
// project_name text columns.
func TestMigration45ProjectsSchema(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()
	now := time.Now().UTC().Truncate(time.Second)

	var recorded int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from schema_migrations where version = ?`), 45).Scan(&recorded); err != nil {
		t.Fatalf("count schema_migrations v45: %v", err)
	}
	if recorded != 1 {
		t.Fatalf("schema_migrations version 45 count = %d, want 1", recorded)
	}

	if err := s.CreateUser(ctx, User{ID: "usr_p45", Email: "p45@x", DisplayName: "P45", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create usr_p45: %v", err)
	}

	// projects: insert + read back.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into projects (id, name, description, owner_user_id, created_at, updated_at) values (?, ?, ?, ?, ?, ?)`),
		"proj_test1", "Test Project", "a project", "usr_p45", now, now); err != nil {
		t.Fatalf("insert projects: %v", err)
	}
	var gotName, gotOwner string
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select name, owner_user_id from projects where id = ?`), "proj_test1").Scan(&gotName, &gotOwner); err != nil {
		t.Fatalf("select projects: %v", err)
	}
	if gotName != "Test Project" || gotOwner != "usr_p45" {
		t.Fatalf("projects round-trip = (%q, %q), want (%q, %q)", gotName, gotOwner, "Test Project", "usr_p45")
	}

	// project_members: insert + read back (FK to projects + users must resolve).
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into project_members (project_id, user_id, created_at) values (?, ?, ?)`),
		"proj_test1", "usr_p45", now); err != nil {
		t.Fatalf("insert project_members: %v", err)
	}
	var memberCount int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from project_members where project_id = ? and user_id = ?`), "proj_test1", "usr_p45").Scan(&memberCount); err != nil {
		t.Fatalf("select project_members: %v", err)
	}
	if memberCount != 1 {
		t.Fatalf("project_members count = %d, want 1", memberCount)
	}

	// project_groups: insert + read back (FK to projects + the seeded default
	// system user_group must resolve).
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into project_groups (project_id, group_id, created_at) values (?, ?, ?)`),
		"proj_test1", DefaultSystemGroupID, now); err != nil {
		t.Fatalf("insert project_groups: %v", err)
	}
	var groupCount int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select count(*) from project_groups where project_id = ? and group_id = ?`), "proj_test1", DefaultSystemGroupID).Scan(&groupCount); err != nil {
		t.Fatalf("select project_groups: %v", err)
	}
	if groupCount != 1 {
		t.Fatalf("project_groups count = %d, want 1", groupCount)
	}

	// api_tokens gained a project_id column (nullable FK to projects).
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into api_tokens (id, user_id, name, secret_hash, secret_prefix, status, scopes, created_at, updated_at, project_id)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		"tok_p45", "usr_p45", "p45 token", "hash_p45", "pfx", TokenStatusActive, "gateway:use", now, now, "proj_test1"); err != nil {
		t.Fatalf("insert api_tokens with project_id: %v", err)
	}
	var tokenProjectID string
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select project_id from api_tokens where id = ?`), "tok_p45").Scan(&tokenProjectID); err != nil {
		t.Fatalf("select api_tokens.project_id: %v", err)
	}
	if tokenProjectID != "proj_test1" {
		t.Fatalf("api_tokens.project_id = %q, want %q", tokenProjectID, "proj_test1")
	}

	// usage_events gained project_id/project_name — plain columns, NO foreign
	// key (a project delete must never cascade/null usage history), so an
	// arbitrary value (not a real project) must be accepted as-is.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into usage_events (id, request_id, user_id, token_id, session_id, api_flavor, model, provider, host, status, error_code,
			input_tokens, output_tokens, total_tokens, latency_ms, created_at, project_id, project_name)
		 values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
		"ue_p45", "req_p45", "usr_p45", "tok_p45", "sess_p45", "openai", "gpt-x", "openai", "host", "success", "",
		0, 0, 0, 0, now, "proj_does_not_exist", "Nonexistent Project"); err != nil {
		t.Fatalf("insert usage_events with project_id/project_name: %v", err)
	}
	var ueProjectID, ueProjectName string
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(`select project_id, project_name from usage_events where id = ?`), "ue_p45").Scan(&ueProjectID, &ueProjectName); err != nil {
		t.Fatalf("select usage_events project columns: %v", err)
	}
	if ueProjectID != "proj_does_not_exist" || ueProjectName != "Nonexistent Project" {
		t.Fatalf("usage_events project columns = (%q, %q), want (%q, %q)", ueProjectID, ueProjectName, "proj_does_not_exist", "Nonexistent Project")
	}
}

// TestMigration46AddsCoupledGroupID proves migration v46 adds the nullable
// projects.coupled_group_id column on a migrated schema. Full round-trip +
// ON DELETE SET NULL + cascade behavior is covered by TestConformanceProjects.
func TestMigration46AddsCoupledGroupID(t *testing.T) {
	s := openMigratedTestSQLite(t)
	defer s.Close()
	// A migrated schema must accept a coupled_group_id write/read (nullable).
	// (Full FK SET-NULL behavior is in TestConformanceProjects.)
	var n int
	if err := s.db.QueryRow(`select count(*) from pragma_table_info('projects') where name='coupled_group_id'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("coupled_group_id column missing after migrate: n=%d err=%v", n, err)
	}
}

// TestMigration48AddsManagerPermissionColumns proves migration v48 adds
// can_manage_users/can_manage_group to user_group_managers on a migrated
// schema (both a fresh install and an upgrading database run the SAME
// migration list in order, so a fully-migrated store here stands for both —
// see migration48Up's doc comment on why user_group_managers itself cannot
// be part of baselineCreateStatements). It then proves the "Bestandsschutz"
// (existing-row protection) requirement directly at the SQL level: a raw
// INSERT that names only (group_id, user_id, created_at) — the exact shape
// of the pre-migration-v48 insert AND of SetUserGroupManager's own statement
// today — reads back true/true purely from the column DEFAULT, with no
// application code involved. This is the same guarantee a genuinely
// pre-v48 row gets when the ALTER TABLE ADD COLUMN ... DEFAULT 1 runs against
// it: SQLite and Postgres both populate every existing row with the column's
// default value as part of the ALTER, so a row that predates the migration
// and a row inserted the old way after it are populated identically.
func TestMigration48AddsManagerPermissionColumns(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()

	for _, col := range []string{"can_manage_users", "can_manage_group"} {
		var n int
		if err := s.db.QueryRow(`select count(*) from pragma_table_info('user_group_managers') where name=?`, col).Scan(&n); err != nil || n != 1 {
			t.Fatalf("column %s missing after migrate: n=%d err=%v", col, n, err)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, User{ID: "usr_m48", Email: "m48@x", DisplayName: "M48", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sys := UserGroup{ID: "ugrp_m48_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_m48_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_m48_sys", OwnerUserID: "usr_m48", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Old-shape insert: no can_manage_users/can_manage_group named at all.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into user_group_managers (group_id, user_id, created_at) values (?, ?, ?)`),
		"ugrp_m48_adm", "usr_m48", now); err != nil {
		t.Fatalf("old-shape insert: %v", err)
	}

	var canUsers, canGroup int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(
		`select can_manage_users, can_manage_group from user_group_managers where group_id = ? and user_id = ?`),
		"ugrp_m48_adm", "usr_m48").Scan(&canUsers, &canGroup); err != nil {
		t.Fatalf("select manager perms: %v", err)
	}
	if canUsers != 1 || canGroup != 1 {
		t.Fatalf("existing-shape manager row = (%d, %d), want (1, 1) from the column default", canUsers, canGroup)
	}

	// Same assertion via the public reader.
	perms, err := s.UserGroupManagerPerms(ctx, "ugrp_m48_adm")
	if err != nil || len(perms) != 1 || !perms[0].CanManageUsers || !perms[0].CanManageGroup {
		t.Fatalf("UserGroupManagerPerms = %+v err=%v, want one row true/true", perms, err)
	}
}

// TestMigration49AddsCanManageServersColumn proves migration v49 adds
// can_manage_servers to user_group_managers on a migrated schema, and that
// an existing (pre-v49-shape) row is backfilled to true purely from the
// column DEFAULT — mirroring TestMigration48AddsManagerPermissionColumns'
// "Bestandsschutz" guarantee for the THIRD co-manager permission flag
// (admin-group permissions Phase B, spec 2026-08-10). A raw INSERT naming
// only (group_id, user_id, created_at) — the exact shape of
// SetUserGroupManager's own statement, which never names can_manage_servers
// either — reads back true with no application code involved.
func TestMigration49AddsCanManageServersColumn(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()

	var n int
	if err := s.db.QueryRow(`select count(*) from pragma_table_info('user_group_managers') where name='can_manage_servers'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("can_manage_servers column missing after migrate: n=%d err=%v", n, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, User{ID: "usr_m49", Email: "m49@x", DisplayName: "M49", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sys := UserGroup{ID: "ugrp_m49_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_m49_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_m49_sys", OwnerUserID: "usr_m49", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Old-shape insert: no can_manage_users/can_manage_group/can_manage_servers named at all.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into user_group_managers (group_id, user_id, created_at) values (?, ?, ?)`),
		"ugrp_m49_adm", "usr_m49", now); err != nil {
		t.Fatalf("old-shape insert: %v", err)
	}

	var canServers int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(
		`select can_manage_servers from user_group_managers where group_id = ? and user_id = ?`),
		"ugrp_m49_adm", "usr_m49").Scan(&canServers); err != nil {
		t.Fatalf("select can_manage_servers: %v", err)
	}
	if canServers != 1 {
		t.Fatalf("existing-shape manager row can_manage_servers = %d, want 1 from the column default", canServers)
	}

	perms, err := s.UserGroupManagerPerms(ctx, "ugrp_m49_adm")
	if err != nil || len(perms) != 1 || !perms[0].CanManageServers {
		t.Fatalf("UserGroupManagerPerms = %+v err=%v, want one row can_manage_servers=true", perms, err)
	}
}

// TestMigration51AddsCanManageServicesColumn proves migration v51 adds
// can_manage_services to user_group_managers on a migrated schema, and that
// an existing (pre-v51-shape) row is backfilled to true purely from the
// column DEFAULT — mirroring TestMigration49AddsCanManageServersColumn's
// "Bestandsschutz" guarantee for the FOURTH co-manager permission flag
// (admin-group permissions Phase C, spec 2026-08-10). A raw INSERT naming
// only (group_id, user_id, created_at) — the exact shape of
// SetUserGroupManager's own statement, which never names can_manage_services
// either — reads back true with no application code involved.
func TestMigration51AddsCanManageServicesColumn(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()

	var n int
	if err := s.db.QueryRow(`select count(*) from pragma_table_info('user_group_managers') where name='can_manage_services'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("can_manage_services column missing after migrate: n=%d err=%v", n, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, User{ID: "usr_m51", Email: "m51@x", DisplayName: "M51", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sys := UserGroup{ID: "ugrp_m51_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_m51_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_m51_sys", OwnerUserID: "usr_m51", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Old-shape insert: no can_manage_users/can_manage_group/can_manage_servers/can_manage_services named at all.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into user_group_managers (group_id, user_id, created_at) values (?, ?, ?)`),
		"ugrp_m51_adm", "usr_m51", now); err != nil {
		t.Fatalf("old-shape insert: %v", err)
	}

	var canServices int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(
		`select can_manage_services from user_group_managers where group_id = ? and user_id = ?`),
		"ugrp_m51_adm", "usr_m51").Scan(&canServices); err != nil {
		t.Fatalf("select can_manage_services: %v", err)
	}
	if canServices != 1 {
		t.Fatalf("existing-shape manager row can_manage_services = %d, want 1 from the column default", canServices)
	}

	perms, err := s.UserGroupManagerPerms(ctx, "ugrp_m51_adm")
	if err != nil || len(perms) != 1 || !perms[0].CanManageServices {
		t.Fatalf("UserGroupManagerPerms = %+v err=%v, want one row can_manage_services=true", perms, err)
	}
}

// TestMigration53AddsCanManageResourcesColumn proves migration v53 adds
// can_manage_resources to user_group_managers on a migrated schema, and that
// an existing (pre-v53-shape) row is backfilled to true purely from the
// column DEFAULT — mirroring TestMigration51AddsCanManageServicesColumn's
// "Bestandsschutz" guarantee for the FIFTH co-manager permission flag
// (Resource Groups Phase 1, spec 2026-08-11). A raw INSERT naming only
// (group_id, user_id, created_at) — the exact shape of SetUserGroupManager's
// own statement, which never names can_manage_resources either — reads back
// true with no application code involved.
func TestMigration53AddsCanManageResourcesColumn(t *testing.T) {
	ctx := context.Background()
	s := openMigratedTestSQLite(t)
	defer s.Close()

	var n int
	if err := s.db.QueryRow(`select count(*) from pragma_table_info('user_group_managers') where name='can_manage_resources'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("can_manage_resources column missing after migrate: n=%d err=%v", n, err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := s.CreateUser(ctx, User{ID: "usr_m53", Email: "m53@x", DisplayName: "M53", Role: "user", Status: UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	sys := UserGroup{ID: "ugrp_m53_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := UserGroup{ID: "ugrp_m53_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_m53_sys", OwnerUserID: "usr_m53", CreatedAt: now, UpdatedAt: now}
	if err := s.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Old-shape insert: no can_manage_users/can_manage_group/can_manage_servers/can_manage_services/can_manage_resources named at all.
	if _, err := s.db.ExecContext(ctx, s.dl.rebind(
		`insert into user_group_managers (group_id, user_id, created_at) values (?, ?, ?)`),
		"ugrp_m53_adm", "usr_m53", now); err != nil {
		t.Fatalf("old-shape insert: %v", err)
	}

	var canResources int64
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(
		`select can_manage_resources from user_group_managers where group_id = ? and user_id = ?`),
		"ugrp_m53_adm", "usr_m53").Scan(&canResources); err != nil {
		t.Fatalf("select can_manage_resources: %v", err)
	}
	if canResources != 1 {
		t.Fatalf("existing-shape manager row can_manage_resources = %d, want 1 from the column default", canResources)
	}

	perms, err := s.UserGroupManagerPerms(ctx, "ugrp_m53_adm")
	if err != nil || len(perms) != 1 || !perms[0].CanManageResources {
		t.Fatalf("UserGroupManagerPerms = %+v err=%v, want one row can_manage_resources=true", perms, err)
	}
}

// TestAddColumnIfMissingSQLite proves the addColumnIfMissing helper (ST-4:
// factored out of the ~36 copy-pasted "add column tolerantly" blocks in this
// file, for NEW migrations to call instead of copy-pasting a 37th) behaves
// exactly like those inlined blocks on sqlite: the first call actually adds
// the column (and existing rows read back the column's default, exactly
// like a real ALTER TABLE ADD COLUMN), and a second call with the identical
// colDef is a no-op that returns a nil error instead of sqlite's "duplicate
// column name" — the same "already applied" case a fresh DB whose baseline
// already created the column would hit if a future migration used this
// helper.
func TestAddColumnIfMissingSQLite(t *testing.T) {
	ctx := context.Background()
	s := openTestSQLite(t)
	defer s.Close()

	if _, err := s.db.ExecContext(ctx, `create table widgets (id text primary key)`); err != nil {
		t.Fatalf("create widgets: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `insert into widgets (id) values ('w1')`); err != nil {
		t.Fatalf("insert w1: %v", err)
	}

	runAddColumn := func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if err := addColumnIfMissing(ctx, tx, s.dl, "widgets", "note text not null default 'unset'"); err != nil {
			_ = tx.Rollback()
			return err
		}
		return tx.Commit()
	}

	if err := runAddColumn(); err != nil {
		t.Fatalf("first addColumnIfMissing: %v", err)
	}

	var n int
	if err := s.db.QueryRow(`select count(*) from pragma_table_info('widgets') where name='note'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("note column missing after addColumnIfMissing: n=%d err=%v", n, err)
	}
	var note string
	if err := s.db.QueryRowContext(ctx, `select note from widgets where id = 'w1'`).Scan(&note); err != nil {
		t.Fatalf("select note: %v", err)
	}
	if note != "unset" {
		t.Fatalf("existing row note = %q, want %q (the column default)", note, "unset")
	}

	// Second call with the identical column definition must be swallowed as a
	// no-op (sqlite's "duplicate column name"), not surfaced as an error —
	// the case a fresh DB whose baseline already has the column would hit.
	if err := runAddColumn(); err != nil {
		t.Fatalf("second addColumnIfMissing (duplicate) returned %v, want nil", err)
	}

	// A genuine, non-duplicate failure (bad table name) must still surface.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	err = addColumnIfMissing(ctx, tx, s.dl, "no_such_table", "note text")
	_ = tx.Rollback()
	if err == nil {
		t.Fatalf("addColumnIfMissing on a nonexistent table returned nil error, want an error")
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
		t.Fatalf("addColumnIfMissing on a nonexistent table was mistaken for a duplicate-column error: %v", err)
	}
}

// TestMigration70BackfillsProxyExcluded proves migration 70's backfill selects
// EXACTLY the retired implicit own-TLS encoding — (scheme='https' AND
// proxy_listen_port=0) — and nothing else, on BOTH dialects.
//
// It verifies the backfill in both directions rather than asserting it: every
// stored shape is seeded, the database is then put back into its genuine
// PRE-70 state (proxy_excluded cleared to the column default 0 on every row,
// and version 70 un-stamped so Migrate has it pending again), Migrate is run,
// and every row's resulting value is checked — the ones that must flip AND the
// ones that must not.
//
// The four shapes and why each lands where it does are spelled out on
// migration70Up itself. The two extra rows here are the ones a "just backfill
// the obvious case" implementation gets wrong: a DISABLED own-TLS application
// (which must still be excluded, so re-enabling it does not silently hand it
// to the proxy) and an EMPTY-scheme row (unreachable through the API, resolving
// to http everywhere it is read, so it must stay a participant at 0).
func TestMigration70BackfillsProxyExcluded(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m70", Name: "M70", Domain: "m70.example.test", Provider: routing.ProviderVLLM,
			Endpoint: "http://m70.example.test:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}

		seeds := []struct {
			id     string
			scheme string
			port   int
			status string
			want   int64
			why    string
		}{
			{"app_m70_http_unassigned", "http", 0, routing.ServerStatusActive, 0, "plain http, no listener yet: a candidate before and after"},
			{"app_m70_http_assigned", "http", 8601, routing.ServerStatusActive, 0, "http holding a released listener (the resting state after a scope exit): still a candidate"},
			{"app_m70_https_proxied", "https", 8602, routing.ServerStatusActive, 0, "actually proxied: the reconcile still owns its scheme"},
			{"app_m70_https_own_tls", "https", 0, routing.ServerStatusActive, 1, "the retired own-TLS encoding: this is the set the flag renames"},
			{"app_m70_https_own_tls_disabled", "https", 0, routing.ServerStatusDisabled, 1, "own TLS AND disabled: keeps the participation it would have had if re-enabled"},
			{"app_m70_empty_scheme", "", 0, routing.ServerStatusActive, 0, "empty scheme resolves to http everywhere it is read, so it is a participant"},
		}
		for i, seed := range seeds {
			if err := s.CreateApplication(ctx, routing.Application{
				ID: seed.id, ServerID: "srv_m70", Type: routing.ProviderVLLM,
				Port: 9200 + i, Scheme: seed.scheme,
				APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 1, Weight: 1,
				TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: seed.status,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
				ProxyListenPort: seed.port,
				CreatedAt:       now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed %s: %v", seed.id, err)
			}
		}

		readExcluded := func(id string) int64 {
			t.Helper()
			var v int64
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(
				`select proxy_excluded from applications where id = ?`), id).Scan(&v); err != nil {
				t.Fatalf("select proxy_excluded for %s: %v", id, err)
			}
			return v
		}

		// BEFORE: put the table back into its genuine pre-70 state. Every row
		// carries the column's DEFAULT 0 — which is exactly what a database
		// migrated by an older binary holds, and what a pre-70 API client's
		// insert (which never names the column) would produce.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`update applications set proxy_excluded = 0`)); err != nil {
			t.Fatalf("clear proxy_excluded: %v", err)
		}
		for _, seed := range seeds {
			if got := readExcluded(seed.id); got != 0 {
				t.Fatalf("pre-migration %s: proxy_excluded = %d, want 0", seed.id, got)
			}
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from schema_migrations where version = ?`), 70); err != nil {
			t.Fatalf("un-stamp migration 70: %v", err)
		}

		// AFTER.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		for _, seed := range seeds {
			if got := readExcluded(seed.id); got != seed.want {
				t.Fatalf("%s: proxy_excluded = %d, want %d (%s)", seed.id, got, seed.want, seed.why)
			}
		}
		var stamped int
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(
			`select count(*) from schema_migrations where version = ?`), 70).Scan(&stamped); err != nil {
			t.Fatalf("count schema_migrations v70: %v", err)
		}
		if stamped != 1 {
			t.Fatalf("schema_migrations version 70 count = %d, want 1", stamped)
		}

		// RE-RUNNING Migrate is a no-op: version 70 is stamped, so the backfill
		// does not run again. Proven with a row the backfill WOULD have
		// flipped — an operator who deliberately puts an own-TLS application
		// back into the proxy must not have that decision undone on the next
		// boot.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`update applications set proxy_excluded = 0 where id = ?`), "app_m70_https_own_tls"); err != nil {
			t.Fatalf("re-clear own-TLS row: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate (second run): %v", err)
		}
		if got := readExcluded("app_m70_https_own_tls"); got != 0 {
			t.Fatalf("after re-running Migrate: proxy_excluded = %d, want 0 — the backfill re-ran on an already-stamped migration", got)
		}
	})
}

// TestMigration70RoundTripsThroughTheStoreReaders is the other half of the
// backfill evidence: the values migration 70 wrote must be what the store
// READERS return, not merely what a raw select shows. It re-reads the rows the
// migration test above seeded through ApplicationsByServer, so a backfill that
// wrote the right column in the wrong place would still be caught.
func TestMigration70RoundTripsThroughTheStoreReaders(t *testing.T) {
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Now().UTC().Truncate(time.Second)
		if err := s.CreateAIServer(ctx, routing.AIServer{
			ID: "srv_m70r", Name: "M70R", Domain: "m70r.example.test", Provider: routing.ProviderVLLM,
			Endpoint: "http://m70r.example.test:8000", Status: routing.ServerStatusActive,
			HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create server: %v", err)
		}
		shapes := []struct {
			id     string
			scheme string
			port   int
			want   bool
		}{
			{"app_m70r_http", "http", 0, false},
			{"app_m70r_proxied", "https", 8702, false},
			{"app_m70r_own_tls", "https", 0, true},
		}
		for i, shape := range shapes {
			if err := s.CreateApplication(ctx, routing.Application{
				ID: shape.id, ServerID: "srv_m70r", Type: routing.ProviderVLLM,
				Port: 9300 + i, Scheme: shape.scheme,
				APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive,
				HealthCheckMode: routing.HealthCheckModeAlwaysReachable,
				ProxyListenPort: shape.port, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed %s: %v", shape.id, err)
			}
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`update applications set proxy_excluded = 0`)); err != nil {
			t.Fatalf("clear proxy_excluded: %v", err)
		}
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from schema_migrations where version = ?`), 70); err != nil {
			t.Fatalf("un-stamp migration 70: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		apps, err := s.ApplicationsByServer(ctx, "srv_m70r")
		if err != nil {
			t.Fatalf("ApplicationsByServer: %v", err)
		}
		got := map[string]bool{}
		for _, app := range apps {
			got[app.ID] = app.ProxyExcluded
		}
		for _, shape := range shapes {
			if got[shape.id] != shape.want {
				t.Fatalf("%s: ApplicationsByServer reports ProxyExcluded=%v, want %v", shape.id, got[shape.id], shape.want)
			}
		}
	})
}

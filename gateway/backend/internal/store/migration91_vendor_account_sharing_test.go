// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// relationExists reports whether a table exists in the live schema, on either
// dialect. tableColumnDecls cannot answer this: it fails the test outright for
// a table with no columns, which is the very state a "not there yet at the
// previous version" precondition needs to observe.
func relationExists(ctx context.Context, t *testing.T, s *SQLStore, table string) bool {
	t.Helper()
	q := `select count(*) from sqlite_master where type = 'table' and name = ?`
	if s.dl.name() == "postgres" {
		q = `select count(*) from information_schema.tables where table_schema = 'public' and table_name = ?`
	}
	var n int
	if err := s.db.QueryRowContext(ctx, s.dl.rebind(q), table).Scan(&n); err != nil {
		t.Fatalf("look up table %s: %v", table, err)
	}
	return n == 1
}

// foreignKeyActions reads the foreign keys declared on a table from the
// database's own catalog and returns them as "<column> -> <parent>(<pcol>)" to
// the on-delete action, lower-cased ("cascade", "no action", ...). Reading the
// catalog rather than provoking a violation is what separates "the FK exists"
// from "some other constraint happened to refuse the row", and it is the only
// way to see the ON DELETE action without deleting data.
func foreignKeyActions(ctx context.Context, t *testing.T, s *SQLStore, table string) map[string]string {
	t.Helper()
	q := `select "from", "table", "to", on_delete from pragma_foreign_key_list(?)`
	if s.dl.name() == "postgres" {
		// confdeltype: a = no action, r = restrict, c = cascade, n = set null,
		// d = set default.
		q = `select a.attname, pc.relname, pa.attname,
				case c.confdeltype when 'c' then 'cascade' when 'a' then 'no action'
					when 'r' then 'restrict' when 'n' then 'set null' else 'set default' end
			from pg_constraint c
			join pg_class t on t.oid = c.conrelid
			join pg_class pc on pc.oid = c.confrelid
			join pg_attribute a on a.attrelid = c.conrelid and a.attnum = c.conkey[1]
			join pg_attribute pa on pa.attrelid = c.confrelid and pa.attnum = c.confkey[1]
			where c.contype = 'f' and t.relnamespace = 'public'::regnamespace and t.relname = ?`
	}
	rows, err := s.db.QueryContext(ctx, s.dl.rebind(q), table)
	if err != nil {
		t.Fatalf("read %s foreign keys: %v", table, err)
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var col, parent, pcol, onDelete string
		if err := rows.Scan(&col, &parent, &pcol, &onDelete); err != nil {
			t.Fatalf("scan %s foreign key: %v", table, err)
		}
		out[col+" -> "+parent+"("+pcol+")"] = strings.ToLower(onDelete)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %s foreign keys: %v", table, err)
	}
	return out
}

// indexColumnNames returns the columns a named index covers, in index order,
// read from the catalog. The existing indexExists only proves a name is
// present; this proves what it is on.
func indexColumnNames(ctx context.Context, t *testing.T, s *SQLStore, index string) []string {
	t.Helper()
	q := `select name from pragma_index_info(?) order by seqno`
	if s.dl.name() == "postgres" {
		q = `select a.attname from pg_index i
			join pg_class ic on ic.oid = i.indexrelid
			join pg_attribute a on a.attrelid = i.indrelid and a.attnum = any(i.indkey)
			where ic.relnamespace = 'public'::regnamespace and ic.relname = ?
			order by array_position(i.indkey::int2[], a.attnum)`
	}
	rows, err := s.db.QueryContext(ctx, s.dl.rebind(q), index)
	if err != nil {
		t.Fatalf("read index %s columns: %v", index, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan index %s column: %v", index, err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index %s columns: %v", index, err)
	}
	return out
}

// TestMigration91AddsVendorAccountShareColumns runs migration 91 against a
// genuine version-90 database that already holds a vendor account, and pins the
// two columns that make an account shareable (shared vendor accounts via
// resource groups, #183 Phase 2):
//
//   - share_level, `text not null default 'none'`: an account is private until
//     someone explicitly shares it, so a row that predates the migration reads
//     back 'none' purely from the column DEFAULT, with no data step;
//   - system_group_id, `text not null` defaulting to the empty string: the
//     immutable containment root, empty until the account is shared. It must be
//     declared exactly like ai_servers.system_group_id (migration 50), the
//     column it mirrors, so a drift in either direction fails here.
//
// A row inserted in the exact shape the pre-sharing store wrote, which names
// neither column, picks up the same defaults. Replaying the migration body
// itself over its own output is a no-op: a bare s.Migrate would not prove that,
// because the ledger skips an applied version without re-running it.
//
// The columns are read and written with raw SQL on purpose: this migration is
// schema only, and the store accessors that consume it arrive in later tasks.
func TestMigration91AddsVendorAccountShareColumns(t *testing.T) {
	forEachDialectMigratedTo(t, 90, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		cols := tableColumns(ctx, t, s, "vendor_accounts")
		for _, c := range []string{"share_level", "system_group_id"} {
			if slices.Contains(cols, c) {
				t.Fatalf("vendor_accounts already has %s at v90: %v", c, cols)
			}
		}

		if err := s.CreateUser(ctx, newTestUser("u_m91", "m91@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		insertAccount := func(id string) {
			t.Helper()
			// Old shape: names neither share_level nor system_group_id.
			if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into vendor_accounts
				(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at)
				values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				id, "u_m91", "openai", "api_key", id, "active", "enc:k", "", "", "", now, now); err != nil {
				t.Fatalf("old-shape account insert for %s: %v", id, err)
			}
		}
		insertAccount("va_m91_pre")

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "vendor_accounts")
		for col, wantDefault := range map[string]string{"share_level": "'none'", "system_group_id": "''"} {
			raw, ok := decls[col]
			if !ok {
				t.Fatalf("vendor_accounts lacks %s after migration 91: %v", col, sortedColumnNames(decls))
			}
			// sqlite reports the declared type upper-case (TEXT) and the default
			// verbatim; postgres lower-case with a cast on the literal
			// ('none'::text). Containing the quoted literal covers both.
			d := strings.ToLower(raw)
			if !strings.HasPrefix(d, "text ") || !strings.Contains(d, "not null") || !strings.Contains(d, "default "+wantDefault) {
				t.Errorf("vendor_accounts.%s = %q, want a not-null text column defaulting to %s", col, raw, wantDefault)
			}
		}
		// Same engine, so the folded declarations are directly comparable.
		serverDecls := tableColumnDecls(ctx, t, s, "ai_servers")
		if got, want := decls["system_group_id"], serverDecls["system_group_id"]; got != want {
			t.Errorf("vendor_accounts.system_group_id = %q, want the ai_servers.system_group_id shape %q", got, want)
		}

		insertAccount("va_m91_post")
		for _, id := range []string{"va_m91_pre", "va_m91_post"} {
			var level, root string
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(
				`select share_level, system_group_id from vendor_accounts where id = ?`), id).Scan(&level, &root); err != nil {
				t.Fatalf("select share columns for %s: %v", id, err)
			}
			if level != "none" || root != "" {
				t.Errorf("account %s = (share_level %q, system_group_id %q), want (\"none\", \"\") from the column defaults", id, level, root)
			}
		}

		// Replay the migration body itself, over data it must not disturb.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(
			`update vendor_accounts set share_level = ?, system_group_id = ? where id = ?`),
			"shared", "ugrp_m91_sys", "va_m91_pre"); err != nil {
			t.Fatalf("set share state: %v", err)
		}
		reinvokeMigration91(ctx, t, s)
		var level, root string
		if err := s.db.QueryRowContext(ctx, s.dl.rebind(
			`select share_level, system_group_id from vendor_accounts where id = ?`), "va_m91_pre").Scan(&level, &root); err != nil {
			t.Fatalf("select after replay: %v", err)
		}
		if level != "shared" || root != "ugrp_m91_sys" {
			t.Errorf("after replay account = (%q, %q), want the shared state untouched", level, root)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

// reinvokeMigration91 runs migration91Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration91(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration91Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration91Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration91CreatesVendorAccountAdminGroups runs migration 91 against a
// genuine version-90 database (where the table does not exist yet) and pins the
// vendor account's management-group join table, which mirrors
// server_admin_groups (migration 50) column for column: the same declared
// shapes, a REAL foreign key on each side declared ON DELETE CASCADE, a unique
// (vendor_account_id, group_id) pair, and a lookup index on group_id. It runs on
// both dialects, so a postgres-only type or cascade slip shows up here.
func TestMigration91CreatesVendorAccountAdminGroups(t *testing.T) {
	forEachDialectMigratedTo(t, 90, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if relationExists(ctx, t, s, "vendor_account_admin_groups") {
			t.Fatalf("vendor_account_admin_groups already exists at v90")
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "vendor_account_admin_groups")
		if got := sortedColumnNames(decls); !slices.Equal(got, []string{"created_at", "group_id", "vendor_account_id"}) {
			t.Fatalf("vendor_account_admin_groups columns = %v, want exactly [created_at group_id vendor_account_id]", got)
		}
		// Same engine, so every column must fold to the very declaration its
		// server_admin_groups counterpart has (type, nullability, default).
		sibling := tableColumnDecls(ctx, t, s, "server_admin_groups")
		for col, siblingCol := range map[string]string{
			"vendor_account_id": "server_id",
			"group_id":          "group_id",
			"created_at":        "created_at",
		} {
			if got, want := decls[col], sibling[siblingCol]; got != want {
				t.Errorf("vendor_account_admin_groups.%s = %q, want the server_admin_groups.%s shape %q", col, got, siblingCol, want)
			}
			if !strings.Contains(strings.ToLower(decls[col]), "not null") {
				t.Errorf("vendor_account_admin_groups.%s = %q, want NOT NULL", col, decls[col])
			}
		}

		wantFKs := map[string]string{
			"vendor_account_id -> vendor_accounts(id)": "cascade",
			"group_id -> user_groups(id)":              "cascade",
		}
		gotFKs := foreignKeyActions(ctx, t, s, "vendor_account_admin_groups")
		if len(gotFKs) != len(wantFKs) {
			t.Errorf("vendor_account_admin_groups foreign keys = %v, want exactly %v", gotFKs, wantFKs)
		}
		for fk, action := range wantFKs {
			if gotFKs[fk] != action {
				t.Errorf("vendor_account_admin_groups foreign key %q on delete = %q, want %q (all: %v)", fk, gotFKs[fk], action, gotFKs)
			}
		}

		if !indexExists(t, s, "idx_vendor_account_admin_groups_group") {
			t.Fatalf("idx_vendor_account_admin_groups_group does not exist")
		}
		if cols := indexColumnNames(ctx, t, s, "idx_vendor_account_admin_groups_group"); !slices.Equal(cols, []string{"group_id"}) {
			t.Errorf("idx_vendor_account_admin_groups_group covers %v, want [group_id]", cols)
		}

		// Behavior: seed one account and two admin groups, then exercise the
		// constraints the catalog claims.
		if err := s.CreateUser(ctx, newTestUser("u_m91g", "m91g@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		sys := UserGroup{ID: "ugrp_m91_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sys); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		for _, id := range []string{"ugrp_m91_a", "ugrp_m91_b"} {
			g := UserGroup{ID: id, Tier: GroupTierAdmin, Name: id, ParentGroupID: "ugrp_m91_sys", OwnerUserID: "u_m91g", CreatedAt: now, UpdatedAt: now}
			if err := s.CreateUserGroup(ctx, g); err != nil {
				t.Fatalf("create admin group %s: %v", id, err)
			}
		}
		for _, id := range []string{"va_m91_x", "va_m91_y"} {
			if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into vendor_accounts
				(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at)
				values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				id, "u_m91g", "openai", "api_key", id, "active", "enc:k", "", "", "", now, now); err != nil {
				t.Fatalf("seed account %s: %v", id, err)
			}
		}
		link := func(accountID, groupID string) error {
			_, err := s.db.ExecContext(ctx, s.dl.rebind(
				`insert into vendor_account_admin_groups (vendor_account_id, group_id, created_at) values (?, ?, ?)`),
				accountID, groupID, now)
			return err
		}
		count := func(where, arg string) int {
			t.Helper()
			var n int
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(
				`select count(*) from vendor_account_admin_groups where `+where+` = ?`), arg).Scan(&n); err != nil {
				t.Fatalf("count by %s: %v", where, err)
			}
			return n
		}

		if err := link("va_missing", "ugrp_m91_a"); err == nil || !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("link to a missing vendor account = %v, want a foreign-key violation", err)
		}
		if err := link("va_m91_x", "ugrp_missing"); err == nil || !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("link to a missing group = %v, want a foreign-key violation", err)
		}
		for _, pair := range [][2]string{{"va_m91_x", "ugrp_m91_a"}, {"va_m91_x", "ugrp_m91_b"}, {"va_m91_y", "ugrp_m91_a"}} {
			if err := link(pair[0], pair[1]); err != nil {
				t.Fatalf("link %v: %v", pair, err)
			}
		}
		if err := link("va_m91_x", "ugrp_m91_a"); err == nil || !s.dl.isUniqueViolation(err) {
			t.Fatalf("duplicate link = %v, want a unique violation", err)
		}

		// Deleting an admin group drops ITS links only.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from user_groups where id = ?`), "ugrp_m91_b"); err != nil {
			t.Fatalf("delete group: %v", err)
		}
		if n := count("group_id", "ugrp_m91_b"); n != 0 {
			t.Errorf("links for the deleted group = %d, want 0 (ON DELETE CASCADE)", n)
		}
		if n := count("vendor_account_id", "va_m91_x"); n != 1 {
			t.Errorf("links left for va_m91_x = %d, want 1 (only the deleted group's link goes)", n)
		}
		// Deleting a vendor account drops ITS links only.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from vendor_accounts where id = ?`), "va_m91_x"); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		if n := count("vendor_account_id", "va_m91_x"); n != 0 {
			t.Errorf("links for the deleted account = %d, want 0 (ON DELETE CASCADE)", n)
		}
		if n := count("vendor_account_id", "va_m91_y"); n != 1 {
			t.Errorf("links left for va_m91_y = %d, want 1 (the other account's link survives)", n)
		}

		// Replaying the migration body over a populated table is a no-op.
		reinvokeMigration91(ctx, t, s)
		if n := count("vendor_account_id", "va_m91_y"); n != 1 {
			t.Errorf("links for va_m91_y after replay = %d, want 1 (replay must not touch data)", n)
		}
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
	"strings"
	"testing"
	"time"
)

// uniqueConstraintColumns returns, for every UNIQUE constraint declared on a
// table, the columns it covers in declaration order (joined with ","), read from
// the database's own catalog. Provoking a duplicate only proves that SOME
// uniqueness rule fired; this proves it is on the pair, in the order, the
// migration declared. A bare `create unique index` would not show up here on
// postgres (it is no pg_constraint row), which is correct: the siblings declare
// the pair inline as a table constraint and this table must too.
func uniqueConstraintColumns(ctx context.Context, t *testing.T, s *SQLStore, table string) []string {
	t.Helper()
	var out []string
	if s.dl.name() == "postgres" {
		rows, err := s.db.QueryContext(ctx, s.dl.rebind(`select c.conname, a.attname
			from pg_constraint c
			join pg_class t on t.oid = c.conrelid
			join pg_attribute a on a.attrelid = c.conrelid and a.attnum = any(c.conkey)
			where c.contype = 'u' and t.relnamespace = 'public'::regnamespace and t.relname = ?
			order by c.conname, array_position(c.conkey, a.attnum)`), table)
		if err != nil {
			t.Fatalf("read %s unique constraints: %v", table, err)
		}
		defer rows.Close()
		byName := make(map[string][]string)
		var names []string
		for rows.Next() {
			var name, col string
			if err := rows.Scan(&name, &col); err != nil {
				t.Fatalf("scan %s unique constraint: %v", table, err)
			}
			if _, ok := byName[name]; !ok {
				names = append(names, name)
			}
			byName[name] = append(byName[name], col)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s unique constraints: %v", table, err)
		}
		for _, n := range names {
			out = append(out, strings.Join(byName[n], ","))
		}
		return out
	}
	// sqlite: an inline UNIQUE(...) table constraint is an auto-index whose
	// origin is 'u' (a PRIMARY KEY is 'pk', a CREATE UNIQUE INDEX is 'c').
	rows, err := s.db.QueryContext(ctx, `select name from pragma_index_list(?) where "unique" = 1 and origin = 'u' order by name`, table)
	if err != nil {
		t.Fatalf("read %s unique constraints: %v", table, err)
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scan %s unique constraint: %v", table, err)
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate %s unique constraints: %v", table, err)
	}
	_ = rows.Close()
	for _, idx := range indexes {
		out = append(out, strings.Join(indexColumnNames(ctx, t, s, idx), ","))
	}
	return out
}

// reinvokeMigration92 runs migration92Up in its own transaction, exactly as the
// ledger would, so a test can replay it over a database that already has it.
func reinvokeMigration92(ctx context.Context, t *testing.T, s *SQLStore) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	if err := migration92Up(ctx, tx, s.dl); err != nil {
		_ = tx.Rollback()
		t.Fatalf("migration92Up replay: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestMigration92CreatesResourceGroupVendorAccounts runs migration 92 against a
// genuine version-91 database (where the table does not exist yet) and pins the
// link table that records which vendor accounts are provisioned into a resource
// group (shared vendor accounts via resource groups, #183 Phase 2). It mirrors
// resource_group_servers (migration 54) column for column, vendor_accounts
// taking the place of ai_servers: the same declared shapes, a REAL foreign key
// on each side declared ON DELETE CASCADE, a unique (resource_group_id,
// vendor_account_id) pair, and a lookup index on vendor_account_id (the reverse
// direction the unique pair's implicit index does not serve). It runs on both
// dialects, so a postgres-only type or cascade slip shows up here.
//
// Everything structural is read from the live catalog (pragma_* / pg_*), not
// from DDL text, and the constraints the catalog claims are then exercised for
// real: an orphan link on either side, a duplicate pair, and a delete of a
// resource group or a vendor account that must take only ITS links with it.
func TestMigration92CreatesResourceGroupVendorAccounts(t *testing.T) {
	forEachDialectMigratedTo(t, 91, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if relationExists(ctx, t, s, "resource_group_vendor_accounts") {
			t.Fatalf("resource_group_vendor_accounts already exists at v91")
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "resource_group_vendor_accounts")
		if got := sortedColumnNames(decls); !slices.Equal(got, []string{"created_at", "resource_group_id", "vendor_account_id"}) {
			t.Fatalf("resource_group_vendor_accounts columns = %v, want exactly [created_at resource_group_id vendor_account_id]", got)
		}
		// Same engine, so every column must fold to the very declaration its
		// resource_group_servers counterpart has (type, nullability, default).
		sibling := tableColumnDecls(ctx, t, s, "resource_group_servers")
		for col, siblingCol := range map[string]string{
			"resource_group_id": "resource_group_id",
			"vendor_account_id": "server_id",
			"created_at":        "created_at",
		} {
			if got, want := decls[col], sibling[siblingCol]; got != want {
				t.Errorf("resource_group_vendor_accounts.%s = %q, want the resource_group_servers.%s shape %q", col, got, siblingCol, want)
			}
			if !strings.Contains(strings.ToLower(decls[col]), "not null") {
				t.Errorf("resource_group_vendor_accounts.%s = %q, want NOT NULL", col, decls[col])
			}
		}

		wantFKs := map[string]string{
			"resource_group_id -> resource_groups(id)": "cascade",
			"vendor_account_id -> vendor_accounts(id)": "cascade",
		}
		gotFKs := foreignKeyActions(ctx, t, s, "resource_group_vendor_accounts")
		if len(gotFKs) != len(wantFKs) {
			t.Errorf("resource_group_vendor_accounts foreign keys = %v, want exactly %v", gotFKs, wantFKs)
		}
		for fk, action := range wantFKs {
			if gotFKs[fk] != action {
				t.Errorf("resource_group_vendor_accounts foreign key %q on delete = %q, want %q (all: %v)", fk, gotFKs[fk], action, gotFKs)
			}
		}

		if got, want := uniqueConstraintColumns(ctx, t, s, "resource_group_vendor_accounts"), []string{"resource_group_id,vendor_account_id"}; !slices.Equal(got, want) {
			t.Errorf("resource_group_vendor_accounts unique constraints = %v, want exactly %v", got, want)
		}

		const idx = "idx_resource_group_vendor_accounts_vendor_account"
		if !indexExists(t, s, idx) {
			t.Fatalf("%s does not exist", idx)
		}
		if cols := indexColumnNames(ctx, t, s, idx); !slices.Equal(cols, []string{"vendor_account_id"}) {
			t.Errorf("%s covers %v, want [vendor_account_id]", idx, cols)
		}

		// Behavior: seed two resource groups and two accounts, then exercise the
		// constraints the catalog claims.
		if err := s.CreateUser(ctx, newTestUser("u_m92", "m92@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
		for _, id := range []string{"rg_m92_a", "rg_m92_b"} {
			rg := routing.ResourceGroup{ID: id, Name: id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
			if err := s.CreateResourceGroup(ctx, rg); err != nil {
				t.Fatalf("create resource group %s: %v", id, err)
			}
		}
		for _, id := range []string{"va_m92_x", "va_m92_y"} {
			if _, err := s.db.ExecContext(ctx, s.dl.rebind(`insert into vendor_accounts
				(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at)
				values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
				id, "u_m92", "openai", "api_key", id, "active", "enc:k", "", "", "", now, now); err != nil {
				t.Fatalf("seed account %s: %v", id, err)
			}
		}
		link := func(groupID, accountID string) error {
			_, err := s.db.ExecContext(ctx, s.dl.rebind(
				`insert into resource_group_vendor_accounts (resource_group_id, vendor_account_id, created_at) values (?, ?, ?)`),
				groupID, accountID, now)
			return err
		}
		count := func(where, arg string) int {
			t.Helper()
			var n int
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(
				`select count(*) from resource_group_vendor_accounts where `+where+` = ?`), arg).Scan(&n); err != nil {
				t.Fatalf("count by %s: %v", where, err)
			}
			return n
		}

		if err := link("rg_missing", "va_m92_x"); err == nil || !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("link to a missing resource group = %v, want a foreign-key violation", err)
		}
		if err := link("rg_m92_a", "va_missing"); err == nil || !s.dl.isForeignKeyViolation(err) {
			t.Fatalf("link to a missing vendor account = %v, want a foreign-key violation", err)
		}
		for _, pair := range [][2]string{{"rg_m92_a", "va_m92_x"}, {"rg_m92_a", "va_m92_y"}, {"rg_m92_b", "va_m92_x"}} {
			if err := link(pair[0], pair[1]); err != nil {
				t.Fatalf("link %v: %v", pair, err)
			}
		}
		if err := link("rg_m92_a", "va_m92_x"); err == nil || !s.dl.isUniqueViolation(err) {
			t.Fatalf("duplicate link = %v, want a unique violation", err)
		}

		// Deleting a vendor account drops ITS links only.
		if _, err := s.db.ExecContext(ctx, s.dl.rebind(`delete from vendor_accounts where id = ?`), "va_m92_y"); err != nil {
			t.Fatalf("delete account: %v", err)
		}
		if n := count("vendor_account_id", "va_m92_y"); n != 0 {
			t.Errorf("links for the deleted account = %d, want 0 (ON DELETE CASCADE)", n)
		}
		if n := count("vendor_account_id", "va_m92_x"); n != 2 {
			t.Errorf("links left for va_m92_x = %d, want 2 (only the deleted account's link goes)", n)
		}
		// Deleting a resource group drops ITS links only.
		if err := s.DeleteResourceGroup(ctx, "rg_m92_b"); err != nil {
			t.Fatalf("delete resource group: %v", err)
		}
		if n := count("resource_group_id", "rg_m92_b"); n != 0 {
			t.Errorf("links for the deleted resource group = %d, want 0 (ON DELETE CASCADE)", n)
		}
		if n := count("resource_group_id", "rg_m92_a"); n != 1 {
			t.Errorf("links left for rg_m92_a = %d, want 1 (the other group's link survives)", n)
		}
		// Both parents survive their links being dropped.
		var parents int
		if err := s.db.QueryRowContext(ctx, `select (select count(*) from resource_groups) + (select count(*) from vendor_accounts)`).Scan(&parents); err != nil {
			t.Fatalf("count parents: %v", err)
		}
		if parents != 2 { // rg_m92_a + va_m92_x
			t.Errorf("parent rows left = %d, want 2 (rg_m92_a and va_m92_x): a cascade must not run upward", parents)
		}

		// Replaying the migration body over a populated table is a no-op.
		reinvokeMigration92(ctx, t, s)
		if n := count("resource_group_id", "rg_m92_a"); n != 1 {
			t.Errorf("links for rg_m92_a after replay = %d, want 1 (replay must not touch data)", n)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

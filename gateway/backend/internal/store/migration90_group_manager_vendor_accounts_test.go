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

// TestMigration90AddsCanManageVendorAccountsColumn runs migration 90 against a
// genuine version-89 database that already holds an admin-group co-manager row,
// and pins what the upgrade owes an operator (shared vendor accounts via
// resource groups, #183 Phase 2): user_group_managers gains a not-null integer
// can_manage_vendor_accounts defaulting to 1 — the SAME declared shape as its
// sibling can_manage_resources, so a drift in either direction fails here —
// and a co-manager row that predates the migration reads back 1 (granted)
// purely from the column DEFAULT, with no data step. That "Bestandsschutz" is
// the point of default 1: an existing co-manager keeps the broad capability
// they had before this permission was split out. A row inserted in the exact
// shape of SetUserGroupManager's own statement, which never names the column,
// picks up the same default. Running the ledger again is a no-op
// (addColumnIfMissing).
//
// The column is read and written with raw SQL on purpose: this migration is
// schema only, and the store accessors that consume it arrive in later tasks.
func TestMigration90AddsCanManageVendorAccountsColumn(t *testing.T) {
	forEachDialectMigratedTo(t, 89, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

		if cols := tableColumns(ctx, t, s, "user_group_managers"); slices.Contains(cols, "can_manage_vendor_accounts") {
			t.Fatalf("user_group_managers already has can_manage_vendor_accounts at v89: %v", cols)
		}

		for _, id := range []string{"u_m90_pre", "u_m90_post"} {
			if err := s.CreateUser(ctx, newTestUser(id, id+"@example.test", now)); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
		sys := UserGroup{ID: "ugrp_m90_sys", Tier: GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, sys); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		adm := UserGroup{ID: "ugrp_m90_adm", Tier: GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_m90_sys", OwnerUserID: "u_m90_pre", CreatedAt: now, UpdatedAt: now}
		if err := s.CreateUserGroup(ctx, adm); err != nil {
			t.Fatalf("create admin group: %v", err)
		}

		insertManager := func(userID string) {
			t.Helper()
			// Old shape: only (group_id, user_id, created_at), exactly what
			// SetUserGroupManager inserts — no can_manage_* column is named.
			if _, err := s.db.ExecContext(ctx, s.dl.rebind(
				`insert into user_group_managers (group_id, user_id, created_at) values (?, ?, ?)`),
				"ugrp_m90_adm", userID, now); err != nil {
				t.Fatalf("old-shape manager insert for %s: %v", userID, err)
			}
		}
		insertManager("u_m90_pre")

		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate to head: %v", err)
		}

		decls := tableColumnDecls(ctx, t, s, "user_group_managers")
		raw, ok := decls["can_manage_vendor_accounts"]
		if !ok {
			t.Fatalf("user_group_managers lacks can_manage_vendor_accounts after migration 90: %v", sortedColumnNames(decls))
		}
		// sqlite reports the declared type upper-case (INTEGER), postgres lower-case.
		d := strings.ToLower(raw)
		if !strings.Contains(d, "int") || !strings.Contains(d, "not null") || !strings.Contains(d, "default 1") {
			t.Errorf("user_group_managers.can_manage_vendor_accounts = %q, want a not-null integer column defaulting to 1", raw)
		}
		// Same engine, so the folded declarations are directly comparable.
		if sibling := decls["can_manage_resources"]; raw != sibling {
			t.Errorf("can_manage_vendor_accounts = %q, want the sibling can_manage_resources shape %q", raw, sibling)
		}

		insertManager("u_m90_post")
		for _, userID := range []string{"u_m90_pre", "u_m90_post"} {
			var granted int64
			if err := s.db.QueryRowContext(ctx, s.dl.rebind(
				`select can_manage_vendor_accounts from user_group_managers where group_id = ? and user_id = ?`),
				"ugrp_m90_adm", userID).Scan(&granted); err != nil {
				t.Fatalf("select can_manage_vendor_accounts for %s: %v", userID, err)
			}
			if granted != 1 {
				t.Errorf("manager %s can_manage_vendor_accounts = %d, want 1 from the column default", userID, granted)
			}
		}

		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}

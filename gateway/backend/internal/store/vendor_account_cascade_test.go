// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"testing"
	"time"
)

// vendorAccountChildCount counts the rows a table holds for one account.
// table is one of the two vendor_account_* child tables, both keyed by
// account_id; it is a test-local constant, never user input.
func vendorAccountChildCount(t *testing.T, s *SQLStore, table, accountID string) int {
	t.Helper()
	var n int
	if err := s.queryRow(context.Background(), `select count(*) from `+table+` where account_id = ?`, accountID).Scan(&n); err != nil {
		t.Fatalf("count %s for %s: %v", table, accountID, err)
	}
	return n
}

// seedVendorAccountModels gives accountID one vendor_account_models row through
// the store's own writer, on any driver.
func seedVendorAccountModels(t *testing.T, s routing.Store, accountID string) {
	t.Helper()
	ctx := context.Background()
	if err := s.SetVendorAccountModels(ctx, accountID, []routing.VendorAccountModel{
		{GatewayModel: "gpt-" + accountID, UpstreamModel: "gpt-upstream", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("seed vendor_account_models for %s: %v", accountID, err)
	}
	if models, err := s.VendorAccountModels(ctx, accountID); err != nil || len(models) != 1 {
		t.Fatalf("vendor_account_models rows for %s = %v, %v before the delete, want 1", accountID, models, err)
	}
}

// seedVendorAccountUsage gives accountID one vendor_account_usage snapshot row
// through the store's own writer, on any driver.
func seedVendorAccountUsage(t *testing.T, s routing.Store, accountID string, now time.Time) {
	t.Helper()
	if err := s.UpsertVendorAccountUsage(context.Background(), routing.VendorAccountUsage{
		AccountID: accountID, FiveHourPct: -1, WeeklyPct: -1, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed vendor_account_usage for %s: %v", accountID, err)
	}
	if _, ok, err := s.VendorAccountUsageByID(context.Background(), accountID); err != nil || !ok {
		t.Fatalf("vendor_account_usage for %s: ok = %v, err = %v before the delete, want ok=true", accountID, ok, err)
	}
}

// seedVendorAccountChildren gives accountID one vendor_account_models row and one
// vendor_account_usage row, both through the store's own writers -- which is also
// exactly what the FK cascade under test has to clean up.
func seedVendorAccountChildren(t *testing.T, s *SQLStore, accountID string, now time.Time) {
	t.Helper()
	seedVendorAccountModels(t, s, accountID)
	seedVendorAccountUsage(t, s, accountID, now)
	if n := vendorAccountChildCount(t, s, "vendor_account_models", accountID); n != 1 {
		t.Fatalf("vendor_account_models rows for %s = %d before the delete, want 1", accountID, n)
	}
	if n := vendorAccountChildCount(t, s, "vendor_account_usage", accountID); n != 1 {
		t.Fatalf("vendor_account_usage rows for %s = %d before the delete, want 1", accountID, n)
	}
}

// TestRoutingStoreDeleteVendorAccountCascades pins what deleting one vendor
// account removes. On EVERY driver the account itself goes (and its sibling
// stays) and so do its vendor_account_models rows (the SQL drivers through the
// ON DELETE CASCADE FK, the memory driver by dropping its per-account model
// map), while the sibling's rows stay. On the SQL drivers its
// vendor_account_usage row goes too.
//
// The usage snapshot now cascades on EVERY driver: the SQL drivers through the
// ON DELETE CASCADE FK, the memory driver by dropping its per-account usage map
// entry (DeleteVendorAccount) -- checked below through VendorAccountUsageByID for
// memory and the raw row count for SQL.
func TestRoutingStoreDeleteVendorAccountCascades(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_vac", "vacascade@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for _, id := range []string{"va_casc", "va_casc_bystander"} {
			if err := s.CreateVendorAccount(ctx, routing.VendorAccount{
				ID: id, OwnerUserID: "u_vac", Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthAPIKey,
				Name: id, Status: routing.VendorAccountStatusActive, APIKey: "enc:k", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
		}

		// The model catalog and the usage snapshot are seeded and read through the
		// store on every driver.
		sqlStore, isSQL := s.(*SQLStore)
		if isSQL {
			seedVendorAccountChildren(t, sqlStore, "va_casc", now)
			seedVendorAccountChildren(t, sqlStore, "va_casc_bystander", now)
		} else {
			seedVendorAccountModels(t, s, "va_casc")
			seedVendorAccountModels(t, s, "va_casc_bystander")
			seedVendorAccountUsage(t, s, "va_casc", now)
			seedVendorAccountUsage(t, s, "va_casc_bystander", now)
		}

		if err := s.DeleteVendorAccount(ctx, "va_casc"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_casc"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("deleted account still readable: err = %v, want ErrNotFound", err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_casc_bystander"); err != nil {
			t.Fatalf("deleting va_casc removed the sibling account: %v", err)
		}

		// Every driver: the deleted account's model rows are gone, the
		// sibling's stay.
		if models, err := s.VendorAccountModels(ctx, "va_casc"); err != nil || len(models) != 0 {
			t.Errorf("vendor_account_models rows for the deleted account = %v, %v, want none (cascade)", models, err)
		}
		if models, err := s.VendorAccountModels(ctx, "va_casc_bystander"); err != nil || len(models) != 1 {
			t.Errorf("vendor_account_models rows for the sibling account = %v, %v, want 1 (untouched)", models, err)
		}

		// Every driver: the deleted account's usage snapshot is gone (read through
		// the store writer/reader, so the memory cascade is covered too), the
		// sibling's stays.
		if _, ok, err := s.VendorAccountUsageByID(ctx, "va_casc"); err != nil || ok {
			t.Errorf("vendor_account_usage for the deleted account: ok = %v, err = %v, want ok=false (cascade)", ok, err)
		}
		if _, ok, err := s.VendorAccountUsageByID(ctx, "va_casc_bystander"); err != nil || !ok {
			t.Errorf("vendor_account_usage for the sibling account: ok = %v, err = %v, want ok=true (untouched)", ok, err)
		}

		if !isSQL {
			return
		}
		for _, table := range []string{"vendor_account_models", "vendor_account_usage"} {
			if n := vendorAccountChildCount(t, sqlStore, table, "va_casc"); n != 0 {
				t.Errorf("%s rows for the deleted account = %d, want 0 (ON DELETE CASCADE)", table, n)
			}
			if n := vendorAccountChildCount(t, sqlStore, table, "va_casc_bystander"); n != 1 {
				t.Errorf("%s rows for the sibling account = %d, want 1 (untouched)", table, n)
			}
		}
	})
}

// TestDeleteUserCascadesVendorAccounts proves vendor_accounts.owner_user_id's
// ON DELETE CASCADE: deleting a user removes that user's accounts and, through
// them, the accounts' child rows, while another user's account survives.
//
// SQL drivers only: there is no store.DeleteUser (deleteUserForTest issues the
// raw delete, as the other FK-cascade tests do) and routing.MemoryStore has no
// user concept at all, so it holds no owner-keyed state a user delete could
// orphan -- there is no memory-store cascade to mirror or to test.
func TestDeleteUserCascadesVendorAccounts(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		for _, id := range []string{"u_vac_gone", "u_vac_stays"} {
			if err := s.CreateUser(ctx, newTestUser(id, id+"@example.test", now)); err != nil {
				t.Fatalf("create user %s: %v", id, err)
			}
		}
		accounts := map[string]string{"va_gone_1": "u_vac_gone", "va_gone_2": "u_vac_gone", "va_stays": "u_vac_stays"}
		for id, owner := range accounts {
			if err := s.CreateVendorAccount(ctx, routing.VendorAccount{
				ID: id, OwnerUserID: owner, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
				Name: id, Status: routing.VendorAccountStatusActive, OAuthTokens: "enc:t", CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create %s: %v", id, err)
			}
			seedVendorAccountChildren(t, s, id, now)
		}

		if err := deleteUserForTest(ctx, s, "u_vac_gone"); err != nil {
			t.Fatalf("delete user: %v", err)
		}

		for _, id := range []string{"va_gone_1", "va_gone_2"} {
			if _, err := s.VendorAccountByID(ctx, id); !errors.Is(err, storeerr.ErrNotFound) {
				t.Errorf("account %s of the deleted user: err = %v, want ErrNotFound", id, err)
			}
			for _, table := range []string{"vendor_account_models", "vendor_account_usage"} {
				if n := vendorAccountChildCount(t, s, table, id); n != 0 {
					t.Errorf("%s rows for %s = %d, want 0 (cascade through the account)", table, id, n)
				}
			}
		}
		if owned, err := s.VendorAccountsByOwner(ctx, "u_vac_gone"); err != nil || len(owned) != 0 {
			t.Errorf("deleted user still owns accounts: %+v, %v", owned, err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_stays"); err != nil {
			t.Errorf("another user's account was removed: %v", err)
		}
		for _, table := range []string{"vendor_account_models", "vendor_account_usage"} {
			if n := vendorAccountChildCount(t, s, table, "va_stays"); n != 1 {
				t.Errorf("%s rows for another user's account = %d, want 1 (untouched)", table, n)
			}
		}
	})
}

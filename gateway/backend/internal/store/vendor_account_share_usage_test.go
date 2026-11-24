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

// TestConformanceVendorAccountShareUsageRoundTrip pins the owner's opt-in to
// showing a subscription's usage limits to members (migration 110): an account created
// without the opt-in reads back false, UpdateVendorAccountShareUsage flips it both ways through its
// own column, and an unknown id is ErrNotFound. Runs on memory, sqlite and
// postgres.
func TestConformanceVendorAccountShareUsageRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_su", "va_su_other")

		read := func(id string) routing.VendorAccount {
			t.Helper()
			acc, err := s.VendorAccountByID(ctx, id)
			if err != nil {
				t.Fatalf("VendorAccountByID(%s): %v", id, err)
			}
			return acc
		}
		if read("va_su").ShareUsageWithMembers {
			t.Fatal("a new account reads ShareUsageWithMembers = true, want false (opt-in)")
		}
		if err := s.UpdateVendorAccountShareUsage(ctx, "va_su_missing", true); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("UpdateVendorAccountShareUsage(unknown) = %v, want ErrNotFound", err)
		}
		for _, want := range []bool{true, false, true} {
			if err := s.UpdateVendorAccountShareUsage(ctx, "va_su", want); err != nil {
				t.Fatalf("UpdateVendorAccountShareUsage(%v): %v", want, err)
			}
			if got := read("va_su").ShareUsageWithMembers; got != want {
				t.Fatalf("after setting %v the account reads %v", want, got)
			}
		}
		if read("va_su_other").ShareUsageWithMembers {
			t.Error("the writer changed a different account")
		}
		// The writer touches one column: a full-row write of a stale struct (false)
		// must not revert it, and the readers (list, by owner) must agree.
		stale := read("va_su")
		stale.ShareUsageWithMembers = false
		stale.Name = "renamed"
		if err := s.UpdateVendorAccount(ctx, stale); err != nil {
			t.Fatalf("UpdateVendorAccount: %v", err)
		}
		if !read("va_su").ShareUsageWithMembers {
			t.Error("UpdateVendorAccount reverted ShareUsageWithMembers")
		}
		list, err := s.VendorAccountsByOwner(ctx, vasOwnerID)
		if err != nil {
			t.Fatalf("VendorAccountsByOwner: %v", err)
		}
		for _, a := range list {
			if want := a.ID == "va_su"; a.ShareUsageWithMembers != want {
				t.Errorf("VendorAccountsByOwner %s ShareUsageWithMembers = %v, want %v", a.ID, a.ShareUsageWithMembers, want)
			}
		}
	})
}

// TestCreateVendorAccountPersistsShareUsage pins that a create stores the usage
// opt-in it carries (spec 10.1), both values, on every driver.
func TestCreateVendorAccountPersistsShareUsage(t *testing.T) {
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		for _, want := range []bool{true, false} {
			id := "va_su_create_off"
			if want {
				id = "va_su_create_on"
			}
			acc := newSharingAccount(id, now)
			acc.ShareUsageWithMembers = want
			if err := s.CreateVendorAccount(ctx, acc); err != nil {
				t.Fatalf("CreateVendorAccount(%s): %v", id, err)
			}
			got, err := s.VendorAccountByID(ctx, id)
			if err != nil {
				t.Fatalf("VendorAccountByID(%s): %v", id, err)
			}
			if got.ShareUsageWithMembers != want {
				t.Errorf("create with ShareUsageWithMembers=%v reads %v", want, got.ShareUsageWithMembers)
			}
		}
	})
}

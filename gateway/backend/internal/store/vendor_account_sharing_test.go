// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"reflect"
	"testing"
	"time"
)

// This file is the store-contract suite for vendor-account sharing (shared
// vendor accounts via resource groups, #183 Phase 2, Task 1.5): the
// vendor-account twins of the AI-server sharing methods --
// Set/Remove/VendorAccountAdminGroups/VendorAccountsByAdminGroups (twins of
// SetServerAdminGroup & co), UpdateVendorAccountShareLevel/SystemGroup, the
// single-transaction ClearVendorAccountSharing, and the resource-group link
// (Set/Remove/ResourceGroupVendorAccounts/ResourceGroupsByVendorAccount, twins
// of the resource_group_servers methods) -- together with the two delete
// cascades that keep the link tables free of dangling rows.
//
// Every test runs through routing.Store on MEMORY, SQLITE and POSTGRES
// (forEachRoutingStoreSeeded). share_level and system_group_id are NOT fields of
// routing.VendorAccount (no reader selects them yet), so their value cannot be
// read back through the interface: the SQL legs read the two columns straight
// off the row (vendorShareColumns), and the memory leg's equivalent state is
// asserted in package routing (TestMemoryStoreVendorAccountSharing), which can
// see MemoryStore's own fields.

const (
	vasOwnerID       = "u_vas_owner"
	vasSystemGroupID = "ugrp_vas_sys"
	vasAdminGroupA   = "ugrp_vas_a"
	vasAdminGroupB   = "ugrp_vas_b"
)

// seedVendorSharingSQL gives the SQL legs what their FKs demand and the memory
// leg does not enforce: an owning user plus a system group with two admin
// groups beneath it (vendor_account_admin_groups.group_id is a real FK).
func seedVendorSharingSQL(t *testing.T, s *SQLStore, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := s.CreateUser(ctx, newTestUser(vasOwnerID, "vas-owner@example.test", now)); err != nil {
		t.Fatalf("seed owner: %v", err)
	}
	if err := s.CreateUserGroup(ctx, UserGroup{ID: vasSystemGroupID, Tier: GroupTierSystem, Name: "VAS System", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed system group: %v", err)
	}
	for _, id := range []string{vasAdminGroupA, vasAdminGroupB} {
		if err := s.CreateUserGroup(ctx, UserGroup{
			ID: id, Tier: GroupTierAdmin, Name: id, ParentGroupID: vasSystemGroupID, OwnerUserID: vasOwnerID,
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed admin group %s: %v", id, err)
		}
	}
}

func newSharingAccount(id string, now time.Time) routing.VendorAccount {
	return routing.VendorAccount{
		ID: id, OwnerUserID: vasOwnerID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: "Sharing " + id, Status: routing.VendorAccountStatusActive, APIKey: "enc:k-" + id,
		ModelPrefix: id + "/", CreatedAt: now, UpdatedAt: now,
	}
}

func createSharingAccounts(t *testing.T, s routing.Store, now time.Time, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.CreateVendorAccount(context.Background(), newSharingAccount(id, now)); err != nil {
			t.Fatalf("create vendor account %s: %v", id, err)
		}
	}
}

func createSharingResourceGroups(t *testing.T, s routing.Store, now time.Time, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if err := s.CreateResourceGroup(context.Background(), routing.ResourceGroup{
			ID: id, Name: id, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("create resource group %s: %v", id, err)
		}
	}
}

// vendorShareColumns reads share_level and system_group_id off the
// vendor_accounts row. ok is false on the memory driver, whose state for them
// is asserted in package routing instead.
func vendorShareColumns(t *testing.T, s routing.Store, id string) (level, systemGroupID string, ok bool) {
	t.Helper()
	sqlStore, isSQL := s.(*SQLStore)
	if !isSQL {
		return "", "", false
	}
	if err := sqlStore.queryRow(context.Background(), `select share_level, system_group_id from vendor_accounts where id = ?`, id).Scan(&level, &systemGroupID); err != nil {
		t.Fatalf("read share columns of %s: %v", id, err)
	}
	return level, systemGroupID, true
}

// requireVendorShareColumns asserts the two columns on the legs that can read
// them (SQL) and is a no-op on memory.
func requireVendorShareColumns(t *testing.T, s routing.Store, id, wantLevel, wantSystemGroup, stage string) {
	t.Helper()
	level, systemGroup, ok := vendorShareColumns(t, s, id)
	if !ok {
		return
	}
	if level != wantLevel || systemGroup != wantSystemGroup {
		t.Fatalf("%s: %s share_level/system_group_id = %q/%q, want %q/%q", stage, id, level, systemGroup, wantLevel, wantSystemGroup)
	}
}

func requireIDs(t *testing.T, what string, got []string, err error, want ...string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if got == nil {
		t.Fatalf("%s = nil, want a non-nil slice", what)
	}
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

func vendorAccountIDs(accounts []routing.VendorAccount) []string {
	out := make([]string, 0, len(accounts))
	for _, a := range accounts {
		out = append(out, a.ID)
	}
	return out
}

func resourceGroupIDs(groups []routing.ResourceGroup) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		out = append(out, g.ID)
	}
	return out
}

// TestRoutingStoreVendorAccountAdminGroups is the vendor-account twin of
// TestConformanceServerAdminGroups: the idempotent upsert, the first-seen
// listing order, the FK rejections, the reverse lookup (deduped, id-ordered,
// full accounts, the empty-input short-circuit) and the removal no-op.
func TestRoutingStoreVendorAccountAdminGroups(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_ag_1", "va_ag_2")

		if got, err := s.VendorAccountAdminGroups(ctx, "va_ag_1"); err != nil || got == nil || len(got) != 0 {
			t.Fatalf("VendorAccountAdminGroups before any link = %#v, %v, want a non-nil empty slice", got, err)
		}

		// va_ag_1 -> {A, B}; a repeat link is the on-conflict-do-nothing no-op.
		for _, gid := range []string{vasAdminGroupA, vasAdminGroupA, vasAdminGroupB} {
			if err := s.SetVendorAccountAdminGroup(ctx, "va_ag_1", gid); err != nil {
				t.Fatalf("SetVendorAccountAdminGroup(va_ag_1, %s): %v", gid, err)
			}
		}
		// va_ag_2 -> {B} only, so the reverse lookup has something to filter out.
		if err := s.SetVendorAccountAdminGroup(ctx, "va_ag_2", vasAdminGroupB); err != nil {
			t.Fatalf("SetVendorAccountAdminGroup(va_ag_2, B): %v", err)
		}
		got, err := s.VendorAccountAdminGroups(ctx, "va_ag_1")
		requireIDs(t, "VendorAccountAdminGroups(va_ag_1)", got, err, vasAdminGroupA, vasAdminGroupB)

		// An unknown account is ErrNotFound on every driver; an unknown group is an
		// FK violation on the SQL legs (the memory store holds no user groups and
		// keeps the id opaque, exactly like SetServerAdminGroup).
		if err := s.SetVendorAccountAdminGroup(ctx, "va_ag_missing", vasAdminGroupA); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("SetVendorAccountAdminGroup(unknown account) = %v, want ErrNotFound", err)
		}
		if _, isSQL := s.(*SQLStore); isSQL {
			if err := s.SetVendorAccountAdminGroup(ctx, "va_ag_1", "ugrp_vas_missing"); !errors.Is(err, storeerr.ErrNotFound) {
				t.Fatalf("SetVendorAccountAdminGroup(unknown group) = %v, want ErrNotFound", err)
			}
		}

		// Reverse lookup: ANY-of semantics, deduped, id-ordered, full account rows.
		byA, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupA})
		if err != nil {
			t.Fatalf("VendorAccountsByAdminGroups(A): %v", err)
		}
		if ids := vendorAccountIDs(byA); !reflect.DeepEqual(ids, []string{"va_ag_1"}) {
			t.Fatalf("VendorAccountsByAdminGroups(A) = %v, want [va_ag_1]", ids)
		}
		if byA[0].Name != "Sharing va_ag_1" || byA[0].OwnerUserID != vasOwnerID || byA[0].APIKey != "enc:k-va_ag_1" {
			t.Fatalf("VendorAccountsByAdminGroups(A) did not return the full account row: %+v", byA[0])
		}
		byB, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupB})
		if err != nil {
			t.Fatalf("VendorAccountsByAdminGroups(B): %v", err)
		}
		if ids := vendorAccountIDs(byB); !reflect.DeepEqual(ids, []string{"va_ag_1", "va_ag_2"}) {
			t.Fatalf("VendorAccountsByAdminGroups(B) = %v, want [va_ag_1 va_ag_2]", ids)
		}
		// va_ag_1 is linked to BOTH groups and must still come back exactly once.
		byBoth, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupA, vasAdminGroupB})
		if err != nil {
			t.Fatalf("VendorAccountsByAdminGroups(A,B): %v", err)
		}
		if ids := vendorAccountIDs(byBoth); !reflect.DeepEqual(ids, []string{"va_ag_1", "va_ag_2"}) {
			t.Fatalf("VendorAccountsByAdminGroups(A,B) = %v, want [va_ag_1 va_ag_2] (deduped)", ids)
		}
		for _, in := range [][]string{{}, nil} {
			byNone, err := s.VendorAccountsByAdminGroups(ctx, in)
			if err != nil || byNone == nil || len(byNone) != 0 {
				t.Fatalf("VendorAccountsByAdminGroups(%#v) = %#v, %v, want a non-nil empty slice", in, byNone, err)
			}
		}
		if byNone, err := s.VendorAccountsByAdminGroups(ctx, []string{"ugrp_vas_unlinked"}); err != nil || len(byNone) != 0 {
			t.Fatalf("VendorAccountsByAdminGroups(unlinked group) = %+v, %v, want empty", byNone, err)
		}

		// Remove leaves the other link; a repeat remove (and an unknown account) is
		// a non-error no-op.
		if err := s.RemoveVendorAccountAdminGroup(ctx, "va_ag_1", vasAdminGroupA); err != nil {
			t.Fatalf("RemoveVendorAccountAdminGroup(A): %v", err)
		}
		if err := s.RemoveVendorAccountAdminGroup(ctx, "va_ag_1", vasAdminGroupA); err != nil {
			t.Fatalf("re-RemoveVendorAccountAdminGroup(A) (no-op): %v", err)
		}
		if err := s.RemoveVendorAccountAdminGroup(ctx, "va_ag_missing", vasAdminGroupA); err != nil {
			t.Fatalf("RemoveVendorAccountAdminGroup(unknown account) (no-op): %v", err)
		}
		got, err = s.VendorAccountAdminGroups(ctx, "va_ag_1")
		requireIDs(t, "VendorAccountAdminGroups(va_ag_1) after remove", got, err, vasAdminGroupB)
		got, err = s.VendorAccountAdminGroups(ctx, "va_ag_2")
		requireIDs(t, "VendorAccountAdminGroups(va_ag_2) (untouched)", got, err, vasAdminGroupB)

		// The SQL legs additionally cascade a USER-GROUP delete through the group_id
		// FK; the memory store cannot observe a user-group deletion (it lives in the
		// separate portal directory), exactly as for server_admin_groups.
		if sqlStore, isSQL := s.(*SQLStore); isSQL {
			if err := sqlStore.DeleteUserGroup(ctx, vasAdminGroupB); err != nil {
				t.Fatalf("DeleteUserGroup(B): %v", err)
			}
			got, err = s.VendorAccountAdminGroups(ctx, "va_ag_1")
			requireIDs(t, "VendorAccountAdminGroups(va_ag_1) after the group was deleted", got, err)
			if byB, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupB}); err != nil || len(byB) != 0 {
				t.Fatalf("VendorAccountsByAdminGroups(B) after the group was deleted = %+v, %v, want empty", byB, err)
			}
		}
	})
}

// TestRoutingStoreVendorAccountShareWriters pins the two narrow writers: an
// unknown id is ErrNotFound, the value lands in its own column, and NOTHING else
// about the account changes (the writers touch one column each, like
// UpdateServerSystemGroup, so they cannot clobber a concurrent full-row write).
func TestRoutingStoreVendorAccountShareWriters(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_sw", "va_sw_other")

		if err := s.UpdateVendorAccountShareLevel(ctx, "va_sw_missing", routing.VendorShareLevelShared); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("UpdateVendorAccountShareLevel(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.UpdateVendorAccountSystemGroup(ctx, "va_sw_missing", vasSystemGroupID); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("UpdateVendorAccountSystemGroup(unknown) = %v, want ErrNotFound", err)
		}

		// A fresh account is private with no containment root (the column DEFAULTs).
		requireVendorShareColumns(t, s, "va_sw", routing.VendorShareLevelNone, "", "fresh account")

		before, err := s.VendorAccountByID(ctx, "va_sw")
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}

		if err := s.UpdateVendorAccountShareLevel(ctx, "va_sw", routing.VendorShareLevelShared); err != nil {
			t.Fatalf("UpdateVendorAccountShareLevel(shared): %v", err)
		}
		requireVendorShareColumns(t, s, "va_sw", routing.VendorShareLevelShared, "", "after raising to shared")
		if err := s.UpdateVendorAccountSystemGroup(ctx, "va_sw", vasSystemGroupID); err != nil {
			t.Fatalf("UpdateVendorAccountSystemGroup: %v", err)
		}
		requireVendorShareColumns(t, s, "va_sw", routing.VendorShareLevelShared, vasSystemGroupID, "after the root was set")
		if err := s.UpdateVendorAccountShareLevel(ctx, "va_sw", routing.VendorShareLevelFull); err != nil {
			t.Fatalf("UpdateVendorAccountShareLevel(full): %v", err)
		}
		requireVendorShareColumns(t, s, "va_sw", routing.VendorShareLevelFull, vasSystemGroupID, "after raising to full")
		// Writing the SAME value again is still a success: the row matched.
		if err := s.UpdateVendorAccountShareLevel(ctx, "va_sw", routing.VendorShareLevelFull); err != nil {
			t.Fatalf("UpdateVendorAccountShareLevel(full) again: %v", err)
		}

		after, err := s.VendorAccountByID(ctx, "va_sw")
		if err != nil {
			t.Fatalf("VendorAccountByID after the writers: %v", err)
		}
		if !reflect.DeepEqual(normalizeVendorAccountForCompare(after), normalizeVendorAccountForCompare(before)) {
			t.Fatalf("a share writer changed a column it does not own:\n got  %+v\n want %+v", after, before)
		}
		// The sibling account is untouched.
		requireVendorShareColumns(t, s, "va_sw_other", routing.VendorShareLevelNone, "", "sibling account")
	})
}

// TestRoutingStoreResourceGroupVendorAccounts is the vendor-account twin of the
// resource_group_servers half of TestConformanceResourceGroups: the idempotent
// link, the first-seen listing, the reverse lookup, the FK rejections on both
// columns (on EVERY driver -- the memory store checks both parents), and the
// removal no-op.
func TestRoutingStoreResourceGroupVendorAccounts(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_rg_1", "va_rg_2", "va_rg_3")
		createSharingResourceGroups(t, s, now, "rg_va_1", "rg_va_2")

		got, err := s.ResourceGroupVendorAccounts(ctx, "rg_va_1")
		requireIDs(t, "ResourceGroupVendorAccounts before any link", got, err)

		// rg_va_1 -> {1,2} (a repeat of 1 is the idempotent no-op); rg_va_2 -> {1}.
		for _, accID := range []string{"va_rg_1", "va_rg_1", "va_rg_2"} {
			if err := s.SetResourceGroupVendorAccount(ctx, "rg_va_1", accID); err != nil {
				t.Fatalf("SetResourceGroupVendorAccount(rg_va_1, %s): %v", accID, err)
			}
		}
		if err := s.SetResourceGroupVendorAccount(ctx, "rg_va_2", "va_rg_1"); err != nil {
			t.Fatalf("SetResourceGroupVendorAccount(rg_va_2, va_rg_1): %v", err)
		}
		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_va_1")
		requireIDs(t, "ResourceGroupVendorAccounts(rg_va_1)", got, err, "va_rg_1", "va_rg_2")

		byAcc1, err := s.ResourceGroupsByVendorAccount(ctx, "va_rg_1")
		if err != nil {
			t.Fatalf("ResourceGroupsByVendorAccount(va_rg_1): %v", err)
		}
		if ids := resourceGroupIDs(byAcc1); !reflect.DeepEqual(ids, []string{"rg_va_1", "rg_va_2"}) {
			t.Fatalf("ResourceGroupsByVendorAccount(va_rg_1) = %v, want [rg_va_1 rg_va_2] (id-ordered)", ids)
		}
		if byAcc1[0].Name != "rg_va_1" || byAcc1[0].Status != routing.ServerStatusActive {
			t.Fatalf("ResourceGroupsByVendorAccount did not return the full resource-group row: %+v", byAcc1[0])
		}
		byAcc2, err := s.ResourceGroupsByVendorAccount(ctx, "va_rg_2")
		if err != nil {
			t.Fatalf("ResourceGroupsByVendorAccount(va_rg_2): %v", err)
		}
		if ids := resourceGroupIDs(byAcc2); !reflect.DeepEqual(ids, []string{"rg_va_1"}) {
			t.Fatalf("ResourceGroupsByVendorAccount(va_rg_2) = %v, want [rg_va_1]", ids)
		}
		if byNone, err := s.ResourceGroupsByVendorAccount(ctx, "va_rg_3"); err != nil || len(byNone) != 0 {
			t.Fatalf("ResourceGroupsByVendorAccount(unlinked) = %+v, %v, want empty", byNone, err)
		}

		// A missing resource group or a missing account is ErrNotFound on EVERY
		// driver: the SQL legs through the FK, the memory store by checking both.
		if err := s.SetResourceGroupVendorAccount(ctx, "rg_va_missing", "va_rg_1"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("SetResourceGroupVendorAccount(unknown rg) = %v, want ErrNotFound", err)
		}
		if err := s.SetResourceGroupVendorAccount(ctx, "rg_va_1", "va_rg_missing"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("SetResourceGroupVendorAccount(unknown account) = %v, want ErrNotFound", err)
		}

		// Remove leaves the other member and the other group's link; a repeat
		// remove (and an unknown group) is a non-error no-op.
		if err := s.RemoveResourceGroupVendorAccount(ctx, "rg_va_1", "va_rg_2"); err != nil {
			t.Fatalf("RemoveResourceGroupVendorAccount: %v", err)
		}
		if err := s.RemoveResourceGroupVendorAccount(ctx, "rg_va_1", "va_rg_2"); err != nil {
			t.Fatalf("re-RemoveResourceGroupVendorAccount (no-op): %v", err)
		}
		if err := s.RemoveResourceGroupVendorAccount(ctx, "rg_va_missing", "va_rg_2"); err != nil {
			t.Fatalf("RemoveResourceGroupVendorAccount(unknown rg) (no-op): %v", err)
		}
		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_va_1")
		requireIDs(t, "ResourceGroupVendorAccounts(rg_va_1) after remove", got, err, "va_rg_1")
		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_va_2")
		requireIDs(t, "ResourceGroupVendorAccounts(rg_va_2) (untouched)", got, err, "va_rg_1")

		// The link is membership of a vendor account in a resource group, NOT a
		// server link: the server-side readers see nothing of it.
		if servers, err := s.ResourceGroupServers(ctx, "rg_va_1"); err != nil || len(servers) != 0 {
			t.Fatalf("ResourceGroupServers after a vendor-account link = %v, %v, want empty", servers, err)
		}
	})
}

// TestRoutingStoreDeleteResourceGroupDropsVendorAccountLinks: deleting a
// resource group empties ITS vendor-account link set (single-key delete in
// memory, the FK cascade in SQL) and leaves the accounts and the account's
// membership of every OTHER group alone.
func TestRoutingStoreDeleteResourceGroupDropsVendorAccountLinks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_drg_1", "va_drg_2")
		createSharingResourceGroups(t, s, now, "rg_drg_gone", "rg_drg_stays")
		for _, link := range [][2]string{
			{"rg_drg_gone", "va_drg_1"}, {"rg_drg_gone", "va_drg_2"}, {"rg_drg_stays", "va_drg_1"},
		} {
			if err := s.SetResourceGroupVendorAccount(ctx, link[0], link[1]); err != nil {
				t.Fatalf("link %v: %v", link, err)
			}
		}
		got, err := s.ResourceGroupVendorAccounts(ctx, "rg_drg_gone")
		requireIDs(t, "ResourceGroupVendorAccounts before the delete", got, err, "va_drg_1", "va_drg_2")

		if err := s.DeleteResourceGroup(ctx, "rg_drg_gone"); err != nil {
			t.Fatalf("DeleteResourceGroup: %v", err)
		}

		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_drg_gone")
		requireIDs(t, "ResourceGroupVendorAccounts after the delete", got, err)
		byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_drg_1")
		if err != nil {
			t.Fatalf("ResourceGroupsByVendorAccount: %v", err)
		}
		if ids := resourceGroupIDs(byAcc); !reflect.DeepEqual(ids, []string{"rg_drg_stays"}) {
			t.Fatalf("ResourceGroupsByVendorAccount(va_drg_1) after the delete = %v, want [rg_drg_stays]", ids)
		}
		if byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_drg_2"); err != nil || len(byAcc) != 0 {
			t.Fatalf("ResourceGroupsByVendorAccount(va_drg_2) after the delete = %+v, %v, want empty", byAcc, err)
		}
		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_drg_stays")
		requireIDs(t, "ResourceGroupVendorAccounts(rg_drg_stays) (untouched)", got, err, "va_drg_1")
		for _, id := range []string{"va_drg_1", "va_drg_2"} {
			if _, err := s.VendorAccountByID(ctx, id); err != nil {
				t.Fatalf("deleting the resource group removed account %s: %v", id, err)
			}
		}

		// Re-creating a group under the deleted id (fixed ids are routine in
		// dev/e2e fixtures) must NOT inherit the old membership.
		createSharingResourceGroups(t, s, now, "rg_drg_gone")
		got, err = s.ResourceGroupVendorAccounts(ctx, "rg_drg_gone")
		requireIDs(t, "ResourceGroupVendorAccounts of a re-created group", got, err)
	})
}

// TestRoutingStoreDeleteVendorAccountDropsSharingLinks: deleting a vendor
// account empties BOTH its admin-group links (single-key in memory) and its
// resource-group links. The memory store keys the latter rgID -> accountID, so
// the cascade has to visit EVERY group: the account sits in two groups here, and
// a loop that stopped after the first would leave the other behind. The links
// are read from both directions, the bystander's links must survive, and an
// account re-created under the same id must come back unshared.
func TestRoutingStoreDeleteVendorAccountDropsSharingLinks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_dc", "va_dc_by")
		createSharingResourceGroups(t, s, now, "rg_dc_1", "rg_dc_2")
		for _, id := range []string{"va_dc", "va_dc_by"} {
			for _, gid := range []string{vasAdminGroupA, vasAdminGroupB} {
				if err := s.SetVendorAccountAdminGroup(ctx, id, gid); err != nil {
					t.Fatalf("link %s to %s: %v", id, gid, err)
				}
			}
			for _, rgID := range []string{"rg_dc_1", "rg_dc_2"} {
				if err := s.SetResourceGroupVendorAccount(ctx, rgID, id); err != nil {
					t.Fatalf("link %s to %s: %v", id, rgID, err)
				}
			}
			if err := s.UpdateVendorAccountShareLevel(ctx, id, routing.VendorShareLevelFull); err != nil {
				t.Fatalf("share %s: %v", id, err)
			}
			if err := s.UpdateVendorAccountSystemGroup(ctx, id, vasSystemGroupID); err != nil {
				t.Fatalf("root %s: %v", id, err)
			}
		}
		// Non-vacuous: both link sets are populated before the delete.
		if got, err := s.VendorAccountAdminGroups(ctx, "va_dc"); err != nil || len(got) != 2 {
			t.Fatalf("VendorAccountAdminGroups before the delete = %v, %v, want 2", got, err)
		}
		if got, err := s.ResourceGroupsByVendorAccount(ctx, "va_dc"); err != nil || len(got) != 2 {
			t.Fatalf("ResourceGroupsByVendorAccount before the delete = %+v, %v, want 2", got, err)
		}

		if err := s.DeleteVendorAccount(ctx, "va_dc"); err != nil {
			t.Fatalf("DeleteVendorAccount: %v", err)
		}

		// Admin-group links: gone, read per account and through the reverse lookup.
		got, err := s.VendorAccountAdminGroups(ctx, "va_dc")
		requireIDs(t, "VendorAccountAdminGroups after the account delete", got, err)
		byGroups, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupA, vasAdminGroupB})
		if err != nil {
			t.Fatalf("VendorAccountsByAdminGroups: %v", err)
		}
		if ids := vendorAccountIDs(byGroups); !reflect.DeepEqual(ids, []string{"va_dc_by"}) {
			t.Fatalf("VendorAccountsByAdminGroups after the account delete = %v, want only the bystander", ids)
		}
		// Resource-group links: gone from EVERY group, read from both directions.
		byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_dc")
		if err != nil || len(byAcc) != 0 {
			t.Fatalf("ResourceGroupsByVendorAccount after the account delete = %+v, %v, want empty", byAcc, err)
		}
		for _, rgID := range []string{"rg_dc_1", "rg_dc_2"} {
			got, err = s.ResourceGroupVendorAccounts(ctx, rgID)
			requireIDs(t, "ResourceGroupVendorAccounts("+rgID+") after the account delete", got, err, "va_dc_by")
		}

		// The bystander keeps everything.
		got, err = s.VendorAccountAdminGroups(ctx, "va_dc_by")
		requireIDs(t, "VendorAccountAdminGroups(bystander)", got, err, vasAdminGroupA, vasAdminGroupB)
		if byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_dc_by"); err != nil || len(byAcc) != 2 {
			t.Fatalf("ResourceGroupsByVendorAccount(bystander) = %+v, %v, want 2", byAcc, err)
		}
		requireVendorShareColumns(t, s, "va_dc_by", routing.VendorShareLevelFull, vasSystemGroupID, "bystander after the delete")
		// The groups themselves are not the account's to take down.
		for _, rgID := range []string{"rg_dc_1", "rg_dc_2"} {
			if _, err := s.ResourceGroupByID(ctx, rgID); err != nil {
				t.Fatalf("deleting the account removed resource group %s: %v", rgID, err)
			}
		}

		// Fixed ids are routine in dev/e2e fixtures: an account re-created under the
		// deleted id must inherit neither links nor share state.
		createSharingAccounts(t, s, now, "va_dc")
		got, err = s.VendorAccountAdminGroups(ctx, "va_dc")
		requireIDs(t, "VendorAccountAdminGroups of a re-created account", got, err)
		if byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_dc"); err != nil || len(byAcc) != 0 {
			t.Fatalf("ResourceGroupsByVendorAccount of a re-created account = %+v, %v, want empty", byAcc, err)
		}
		requireVendorShareColumns(t, s, "va_dc", routing.VendorShareLevelNone, "", "re-created account")
	})
}

// TestRoutingStoreClearVendorAccountSharing pins the multi-effect behavior of
// the downgrade-to-none writer: ONE call empties BOTH link sets and resets the
// level to 'none' and the containment root to the empty string -- and touches nothing else:
// not the account's other columns, not the groups, not a bystander account.
func TestRoutingStoreClearVendorAccountSharing(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachRoutingStoreSeeded(t, func(t *testing.T, s *SQLStore) { seedVendorSharingSQL(t, s, now) }, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		createSharingAccounts(t, s, now, "va_clr", "va_clr_by")
		createSharingResourceGroups(t, s, now, "rg_clr_1", "rg_clr_2")
		for _, id := range []string{"va_clr", "va_clr_by"} {
			for _, gid := range []string{vasAdminGroupA, vasAdminGroupB} {
				if err := s.SetVendorAccountAdminGroup(ctx, id, gid); err != nil {
					t.Fatalf("link %s to %s: %v", id, gid, err)
				}
			}
			for _, rgID := range []string{"rg_clr_1", "rg_clr_2"} {
				if err := s.SetResourceGroupVendorAccount(ctx, rgID, id); err != nil {
					t.Fatalf("link %s to %s: %v", id, rgID, err)
				}
			}
			if err := s.UpdateVendorAccountShareLevel(ctx, id, routing.VendorShareLevelFull); err != nil {
				t.Fatalf("share %s: %v", id, err)
			}
			if err := s.UpdateVendorAccountSystemGroup(ctx, id, vasSystemGroupID); err != nil {
				t.Fatalf("root %s: %v", id, err)
			}
		}
		// Non-vacuous: all four things Clear resets are populated.
		if got, err := s.VendorAccountAdminGroups(ctx, "va_clr"); err != nil || len(got) != 2 {
			t.Fatalf("VendorAccountAdminGroups before Clear = %v, %v, want 2", got, err)
		}
		if got, err := s.ResourceGroupsByVendorAccount(ctx, "va_clr"); err != nil || len(got) != 2 {
			t.Fatalf("ResourceGroupsByVendorAccount before Clear = %+v, %v, want 2", got, err)
		}
		requireVendorShareColumns(t, s, "va_clr", routing.VendorShareLevelFull, vasSystemGroupID, "before Clear")
		before, err := s.VendorAccountByID(ctx, "va_clr")
		if err != nil {
			t.Fatalf("VendorAccountByID before Clear: %v", err)
		}

		if err := s.ClearVendorAccountSharing(ctx, "va_clr"); err != nil {
			t.Fatalf("ClearVendorAccountSharing: %v", err)
		}

		// Effect 1: the admin-group links are gone.
		got, err := s.VendorAccountAdminGroups(ctx, "va_clr")
		requireIDs(t, "VendorAccountAdminGroups after Clear", got, err)
		if byGroups, err := s.VendorAccountsByAdminGroups(ctx, []string{vasAdminGroupA, vasAdminGroupB}); err != nil ||
			!reflect.DeepEqual(vendorAccountIDs(byGroups), []string{"va_clr_by"}) {
			t.Fatalf("VendorAccountsByAdminGroups after Clear = %+v, %v, want only the bystander", byGroups, err)
		}
		// Effect 2: the resource-group links are gone, from every group.
		if byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_clr"); err != nil || len(byAcc) != 0 {
			t.Fatalf("ResourceGroupsByVendorAccount after Clear = %+v, %v, want empty", byAcc, err)
		}
		for _, rgID := range []string{"rg_clr_1", "rg_clr_2"} {
			got, err = s.ResourceGroupVendorAccounts(ctx, rgID)
			requireIDs(t, "ResourceGroupVendorAccounts("+rgID+") after Clear", got, err, "va_clr_by")
		}
		// Effects 3 + 4: level back to 'none' and the root cleared (SQL legs read the
		// columns; the memory leg is asserted in package routing).
		requireVendorShareColumns(t, s, "va_clr", routing.VendorShareLevelNone, "", "after Clear")

		// Nothing else moved: not the account row, not the groups, not the bystander.
		after, err := s.VendorAccountByID(ctx, "va_clr")
		if err != nil {
			t.Fatalf("VendorAccountByID after Clear: %v", err)
		}
		if !reflect.DeepEqual(normalizeVendorAccountForCompare(after), normalizeVendorAccountForCompare(before)) {
			t.Fatalf("Clear changed an account column:\n got  %+v\n want %+v", after, before)
		}
		for _, rgID := range []string{"rg_clr_1", "rg_clr_2"} {
			if _, err := s.ResourceGroupByID(ctx, rgID); err != nil {
				t.Fatalf("Clear removed resource group %s: %v", rgID, err)
			}
		}
		got, err = s.VendorAccountAdminGroups(ctx, "va_clr_by")
		requireIDs(t, "VendorAccountAdminGroups(bystander) after Clear", got, err, vasAdminGroupA, vasAdminGroupB)
		if byAcc, err := s.ResourceGroupsByVendorAccount(ctx, "va_clr_by"); err != nil || len(byAcc) != 2 {
			t.Fatalf("ResourceGroupsByVendorAccount(bystander) after Clear = %+v, %v, want 2", byAcc, err)
		}
		requireVendorShareColumns(t, s, "va_clr_by", routing.VendorShareLevelFull, vasSystemGroupID, "bystander after Clear")

		// An unknown account is ErrNotFound; clearing an already-clear account is a
		// success (the downgrade is idempotent).
		if err := s.ClearVendorAccountSharing(ctx, "va_clr_missing"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("ClearVendorAccountSharing(unknown) = %v, want ErrNotFound", err)
		}
		if err := s.ClearVendorAccountSharing(ctx, "va_clr"); err != nil {
			t.Fatalf("ClearVendorAccountSharing on an already-clear account: %v", err)
		}
		requireVendorShareColumns(t, s, "va_clr", routing.VendorShareLevelNone, "", "after the second Clear")
	})
}

// installDeleteTrigger installs a BEFORE DELETE row trigger on table that runs
// the given body, with the dialect's own trigger syntax (sqlite inlines the body;
// postgres needs a plpgsql function). It returns the func that removes it again --
// called explicitly (defer), not through t.Cleanup, because the store is closed
// before cleanups run. The postgres database is reused across tests, so removal
// is not optional there.
func installDeleteTrigger(t *testing.T, s *SQLStore, name, table, sqliteBody, plpgsqlBody string) (remove func()) {
	t.Helper()
	ctx := context.Background()
	run := func(stmt string) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if s.dl.name() == "postgres" {
		fn := name + "_fn"
		run(`create or replace function ` + fn + `() returns trigger language plpgsql as $$ begin ` + plpgsqlBody + ` return old; end $$`)
		run(`create trigger ` + name + ` before delete on ` + table + ` for each row execute function ` + fn + `()`)
		return func() {
			_, _ = s.db.ExecContext(ctx, `drop trigger if exists `+name+` on `+table)
			_, _ = s.db.ExecContext(ctx, `drop function if exists `+fn+`()`)
		}
	}
	run(`create trigger ` + name + ` before delete on ` + table + ` begin ` + sqliteBody + ` end`)
	return func() { _, _ = s.db.ExecContext(ctx, `drop trigger if exists `+name) }
}

// seedFullyShared makes id a full-shared account with two admin groups and two
// resource-group links, through the store's own writers.
func seedFullyShared(t *testing.T, s *SQLStore, id string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	createSharingAccounts(t, s, now, id)
	createSharingResourceGroups(t, s, now, "rg_"+id+"_1", "rg_"+id+"_2")
	for _, gid := range []string{vasAdminGroupA, vasAdminGroupB} {
		if err := s.SetVendorAccountAdminGroup(ctx, id, gid); err != nil {
			t.Fatalf("link %s to %s: %v", id, gid, err)
		}
	}
	for _, rgID := range []string{"rg_" + id + "_1", "rg_" + id + "_2"} {
		if err := s.SetResourceGroupVendorAccount(ctx, rgID, id); err != nil {
			t.Fatalf("link %s to %s: %v", id, rgID, err)
		}
	}
	if err := s.UpdateVendorAccountShareLevel(ctx, id, routing.VendorShareLevelFull); err != nil {
		t.Fatalf("share %s: %v", id, err)
	}
	if err := s.UpdateVendorAccountSystemGroup(ctx, id, vasSystemGroupID); err != nil {
		t.Fatalf("root %s: %v", id, err)
	}
}

// TestClearVendorAccountSharingRollsBackAsOneTransaction proves the "one
// transaction" half of the contract rather than assuming it. A trigger makes the
// resource-group link delete -- the THIRD statement of Clear, after the level
// write and the admin-group delete have already run -- fail; if Clear were not a
// single transaction those earlier statements would stay committed. Everything
// must read back exactly as seeded.
func TestClearVendorAccountSharingRollsBackAsOneTransaction(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		seedVendorSharingSQL(t, s, now)
		seedFullyShared(t, s, "va_tx", now)
		defer installDeleteTrigger(t, s, "vas_boom", "resource_group_vendor_accounts",
			`select raise(abort, 'vas boom');`, `raise exception 'vas boom';`)()

		err := s.ClearVendorAccountSharing(ctx, "va_tx")
		if err == nil {
			t.Fatal("ClearVendorAccountSharing succeeded although the resource-group link delete was made to fail")
		}
		if errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("ClearVendorAccountSharing = %v, want the trigger failure, not ErrNotFound", err)
		}

		requireVendorShareColumns(t, s, "va_tx", routing.VendorShareLevelFull, vasSystemGroupID, "after the failed Clear (level write must have rolled back)")
		got, aerr := s.VendorAccountAdminGroups(ctx, "va_tx")
		requireIDs(t, "VendorAccountAdminGroups after the failed Clear (delete must have rolled back)", got, aerr, vasAdminGroupA, vasAdminGroupB)
		byAcc, rerr := s.ResourceGroupsByVendorAccount(ctx, "va_tx")
		if rerr != nil || len(byAcc) != 2 {
			t.Fatalf("ResourceGroupsByVendorAccount after the failed Clear = %+v, %v, want both links intact", byAcc, rerr)
		}
	})
}

// TestClearVendorAccountSharingSetsLevelBeforeDeletingLinks pins the ordering the
// spec makes load-bearing: share_level='none' is written FIRST, so the account
// is already owner-only by the time any link row disappears, and no read can ever
// observe a live non-'none' level over half-deleted links. Atomicity hides the
// order from every OUTSIDE reader, so the probe lives inside the transaction: a
// trigger on each link table records the account's share_level at the instant a
// link row is deleted, and every recorded value must already be 'none'.
func TestClearVendorAccountSharingSetsLevelBeforeDeletingLinks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	forEachDialect(t, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		seedVendorSharingSQL(t, s, now)
		seedFullyShared(t, s, "va_ord", now)

		if _, err := s.db.ExecContext(ctx, `create table vas_order_probe (link_table text not null, share_level text not null)`); err != nil {
			t.Fatalf("create probe table: %v", err)
		}
		defer func() { _, _ = s.db.ExecContext(ctx, `drop table if exists vas_order_probe`) }()
		defer installDeleteTrigger(t, s, "vas_probe_admin", "vendor_account_admin_groups",
			`insert into vas_order_probe select 'vendor_account_admin_groups', share_level from vendor_accounts where id = old.vendor_account_id;`,
			`insert into vas_order_probe select 'vendor_account_admin_groups', share_level from vendor_accounts where id = old.vendor_account_id;`)()
		defer installDeleteTrigger(t, s, "vas_probe_rg", "resource_group_vendor_accounts",
			`insert into vas_order_probe select 'resource_group_vendor_accounts', share_level from vendor_accounts where id = old.vendor_account_id;`,
			`insert into vas_order_probe select 'resource_group_vendor_accounts', share_level from vendor_accounts where id = old.vendor_account_id;`)()

		if err := s.ClearVendorAccountSharing(ctx, "va_ord"); err != nil {
			t.Fatalf("ClearVendorAccountSharing: %v", err)
		}

		rows, err := s.db.QueryContext(ctx, `select link_table, share_level from vas_order_probe`)
		if err != nil {
			t.Fatalf("read probe: %v", err)
		}
		defer rows.Close()
		seen := map[string]int{}
		for rows.Next() {
			var table, level string
			if err := rows.Scan(&table, &level); err != nil {
				t.Fatalf("scan probe: %v", err)
			}
			seen[table]++
			if level != routing.VendorShareLevelNone {
				t.Errorf("a %s row was deleted while share_level was still %q; Clear must write 'none' before it deletes any link", table, level)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate probe: %v", err)
		}
		// 2 admin-group links and 2 resource-group links were seeded, so the probe
		// fired for each (a probe that never fired would prove nothing).
		if seen["vendor_account_admin_groups"] != 2 || seen["resource_group_vendor_accounts"] != 2 {
			t.Fatalf("probe fired %v times, want 2 for each link table", seen)
		}
	})
}

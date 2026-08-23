// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// TestMemoryDirectoryGroups mirrors the SQLStore conformance contract in
// internal/store/conformance_test.go's testUserGroups, adapted for the
// MemoryDirectory: no FK enforcement (the memory driver never validates a
// parent_group_id/owner_user_id/group_id reference on write — consistent
// with every other MemoryDirectory Create* method, which only checks id/email
// uniqueness), so the FK-violation-to-ErrNotFound assertions from the SQL
// conformance test are intentionally NOT reproduced here. Everything else —
// round-trip, upsert semantics, the member-state filter excluding an invited
// row, managers, and the manual recursive CASCADE on delete — is asserted.
func TestMemoryDirectoryGroups(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	d := NewMemoryDirectory(nil)

	sys := store.UserGroup{ID: "ugrp_s", Tier: store.GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
	if err := d.CreateUserGroup(ctx, sys); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	adm := store.UserGroup{ID: "ugrp_a", Tier: store.GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_s", OwnerUserID: "usr_g1", CreatedAt: now, UpdatedAt: now}
	if err := d.CreateUserGroup(ctx, adm); err != nil {
		t.Fatalf("create admin group: %v", err)
	}

	// Duplicate id -> ErrConflict.
	if err := d.CreateUserGroup(ctx, adm); err != store.ErrConflict {
		t.Fatalf("create duplicate group id = %v, want ErrConflict", err)
	}

	got, err := d.UserGroupByID(ctx, "ugrp_a")
	if err != nil {
		t.Fatalf("UserGroupByID: %v", err)
	}
	if got.ParentGroupID != "ugrp_s" || got.OwnerUserID != "usr_g1" || got.Tier != store.GroupTierAdmin || got.Name != "Adm" {
		t.Fatalf("roundtrip: %+v", got)
	}
	gotSys, err := d.UserGroupByID(ctx, "ugrp_s")
	if err != nil {
		t.Fatalf("UserGroupByID(system): %v", err)
	}
	if gotSys.ParentGroupID != "" || gotSys.OwnerUserID != "" {
		t.Fatalf("system group parent/owner not empty: %+v", gotSys)
	}
	if _, err := d.UserGroupByID(ctx, "ugrp_missing"); err != store.ErrNotFound {
		t.Fatalf("UserGroupByID(missing) = %v, want ErrNotFound", err)
	}

	// Update: rename + change owner; ParentGroupID/Tier are not writable via Update.
	updated := got
	updated.Name = "Adm Renamed"
	updated.OwnerUserID = "usr_g2"
	updated.UpdatedAt = now.Add(time.Minute)
	if err := d.UpdateUserGroup(ctx, updated); err != nil {
		t.Fatalf("UpdateUserGroup: %v", err)
	}
	got, err = d.UserGroupByID(ctx, "ugrp_a")
	if err != nil || got.Name != "Adm Renamed" || got.OwnerUserID != "usr_g2" {
		t.Fatalf("after update: %+v err=%v", got, err)
	}
	if err := d.UpdateUserGroup(ctx, store.UserGroup{ID: "ugrp_missing", Name: "x", UpdatedAt: now}); err != store.ErrNotFound {
		t.Fatalf("UpdateUserGroup(missing) = %v, want ErrNotFound", err)
	}

	// ListUserGroupsByTier / ChildUserGroups.
	byTier, err := d.ListUserGroupsByTier(ctx, store.GroupTierAdmin)
	if err != nil || len(byTier) != 1 || byTier[0].ID != "ugrp_a" {
		t.Fatalf("ListUserGroupsByTier(admin) = %+v err=%v", byTier, err)
	}
	children, err := d.ChildUserGroups(ctx, "ugrp_s")
	if err != nil || len(children) != 1 || children[0].ID != "ugrp_a" {
		t.Fatalf("ChildUserGroups(ugrp_s) = %+v err=%v", children, err)
	}

	// Members: usr_g1 is a full member, usr_g2 is invited (by usr_g1).
	if err := d.SetUserGroupMember(ctx, "ugrp_a", "usr_g1", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember(member): %v", err)
	}
	if err := d.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", store.GroupStateInvited, "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupMember(invited): %v", err)
	}

	members, err := d.UserGroupMembers(ctx, "ugrp_a")
	if err != nil {
		t.Fatalf("UserGroupMembers: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("members = %d, want 2: %+v", len(members), members)
	}
	var sawInvited bool
	for _, m := range members {
		if m.UserID == "usr_g2" {
			sawInvited = true
			if m.State != store.GroupStateInvited || m.InvitedBy != "usr_g1" {
				t.Fatalf("invited member wrong: %+v", m)
			}
		}
	}
	if !sawInvited {
		t.Fatalf("invited member usr_g2 not found in %+v", members)
	}

	// Upsert (SetUserGroupMember again) changes state in place, no duplicate row.
	if err := d.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember(upsert to member): %v", err)
	}
	members, _ = d.UserGroupMembers(ctx, "ugrp_a")
	if len(members) != 2 {
		t.Fatalf("members after upsert = %d, want 2 (no duplicate row): %+v", len(members), members)
	}

	// UserGroupsForUser: usr_g1 is a member of ugrp_a (admin tier).
	gs, err := d.UserGroupsForUser(ctx, "usr_g1", store.GroupTierAdmin, store.GroupStateMember)
	if err != nil || len(gs) != 1 || gs[0].ID != "ugrp_a" {
		t.Fatalf("UserGroupsForUser(usr_g1, admin, member) = %+v err=%v", gs, err)
	}
	// Any-tier/any-state lookup (both args "") also finds it.
	gsAny, err := d.UserGroupsForUser(ctx, "usr_g1", "", "")
	if err != nil || len(gsAny) != 1 || gsAny[0].ID != "ugrp_a" {
		t.Fatalf("UserGroupsForUser(usr_g1, any, any) = %+v err=%v", gsAny, err)
	}
	// usr_g2 was upserted to "member" above, so it now DOES satisfy a member filter.
	gs2, err := d.UserGroupsForUser(ctx, "usr_g2", store.GroupTierAdmin, store.GroupStateMember)
	if err != nil || len(gs2) != 1 {
		t.Fatalf("UserGroupsForUser(usr_g2, admin, member) after upsert = %+v err=%v", gs2, err)
	}
	// Re-invite usr_g2 to prove an invited membership does NOT satisfy a
	// "member" state filter (the state discriminator is load-bearing).
	if err := d.SetUserGroupMember(ctx, "ugrp_a", "usr_g2", store.GroupStateInvited, "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupMember(re-invite): %v", err)
	}
	gs3, err := d.UserGroupsForUser(ctx, "usr_g2", store.GroupTierAdmin, store.GroupStateMember)
	if err != nil {
		t.Fatalf("UserGroupsForUser(usr_g2, admin, member) after re-invite: %v", err)
	}
	if len(gs3) != 0 {
		t.Fatalf("invited state leaked into member filter: %+v", gs3)
	}

	// RemoveUserGroupMember.
	if err := d.RemoveUserGroupMember(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("RemoveUserGroupMember: %v", err)
	}
	members, _ = d.UserGroupMembers(ctx, "ugrp_a")
	if len(members) != 1 || members[0].UserID != "usr_g1" {
		t.Fatalf("members after remove = %+v, want only usr_g1", members)
	}

	// Managers.
	if err := d.SetUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("SetUserGroupManager: %v", err)
	}
	// Idempotent re-set (on-conflict-do-nothing) does not error or duplicate.
	if err := d.SetUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("SetUserGroupManager(re-set): %v", err)
	}
	mgrs, err := d.UserGroupManagers(ctx, "ugrp_a")
	if err != nil || len(mgrs) != 1 || mgrs[0] != "usr_g2" {
		t.Fatalf("UserGroupManagers = %+v err=%v", mgrs, err)
	}
	if err := d.RemoveUserGroupManager(ctx, "ugrp_a", "usr_g2"); err != nil {
		t.Fatalf("RemoveUserGroupManager: %v", err)
	}
	mgrs, _ = d.UserGroupManagers(ctx, "ugrp_a")
	if len(mgrs) != 0 {
		t.Fatalf("managers after remove = %+v, want empty", mgrs)
	}
	// Re-add a manager so the cascade assertion below has something to prove wrong.
	if err := d.SetUserGroupManager(ctx, "ugrp_a", "usr_g1"); err != nil {
		t.Fatalf("SetUserGroupManager(re-add): %v", err)
	}

	// Cascade: deleting the system group removes the admin child + its
	// member/manager rows (a manual recursive cascade mirroring the SQL
	// ON DELETE CASCADE from user_groups.parent_group_id down to
	// user_group_members/user_group_managers via group_id).
	if err := d.DeleteUserGroup(ctx, "ugrp_s"); err != nil {
		t.Fatalf("DeleteUserGroup(system): %v", err)
	}
	if _, err := d.UserGroupByID(ctx, "ugrp_a"); err != store.ErrNotFound {
		t.Fatalf("child group not cascade-deleted: %v", err)
	}
	if m, err := d.UserGroupMembers(ctx, "ugrp_a"); err != nil || len(m) != 0 {
		t.Fatalf("members not cascaded: %+v err=%v", m, err)
	}
	if m, err := d.UserGroupManagers(ctx, "ugrp_a"); err != nil || len(m) != 0 {
		t.Fatalf("managers not cascaded: %+v err=%v", m, err)
	}
	if err := d.DeleteUserGroup(ctx, "ugrp_s"); err != store.ErrNotFound {
		t.Fatalf("DeleteUserGroup(already deleted) = %v, want ErrNotFound", err)
	}
}

// TestMemoryDirectoryGroupsCascadeMultiLevel proves the recursive part of the
// cascade: a grandchild (user-tier group under the admin group) is also
// removed when the system-tier ancestor is deleted, not just the direct
// admin-tier child.
func TestMemoryDirectoryGroupsCascadeMultiLevel(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	d := NewMemoryDirectory(nil)

	if err := d.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_s2", Tier: store.GroupTierSystem, Name: "Sys2", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create system group: %v", err)
	}
	if err := d.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_a2", Tier: store.GroupTierAdmin, Name: "Adm2", ParentGroupID: "ugrp_s2", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create admin group: %v", err)
	}
	if err := d.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_u2", Tier: store.GroupTierUser, Name: "User2", ParentGroupID: "ugrp_a2", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("create user group: %v", err)
	}
	if err := d.SetUserGroupMember(ctx, "ugrp_u2", "usr_leaf", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SetUserGroupMember: %v", err)
	}

	if err := d.DeleteUserGroup(ctx, "ugrp_s2"); err != nil {
		t.Fatalf("DeleteUserGroup(system): %v", err)
	}
	if _, err := d.UserGroupByID(ctx, "ugrp_a2"); err != store.ErrNotFound {
		t.Fatalf("direct child not deleted: %v", err)
	}
	if _, err := d.UserGroupByID(ctx, "ugrp_u2"); err != store.ErrNotFound {
		t.Fatalf("grandchild not deleted: %v", err)
	}
	if m, err := d.UserGroupMembers(ctx, "ugrp_u2"); err != nil || len(m) != 0 {
		t.Fatalf("grandchild members not cascaded: %+v err=%v", m, err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"path/filepath"
	"testing"
	"time"
)

// This file is the MEMORY-vs-SQL conformance suite for GroupStore,
// ProjectStore, and TokenRepository (ST-3). Parity between *store.SQLStore
// and *MemoryDirectory for these three contracts previously rested SOLELY on
// "mirrors SQLiteStore.X" doc comments plus the compile-time `var _`
// assertions in group_store.go/project_store.go — there was no behavioral
// test that ran the SAME inputs through both backends. Playwright e2e runs
// memory mode exclusively, so a divergence between the two would ship
// straight to production without ever failing a test.
//
// Every subtest below runs against BOTH backends, through the narrow
// GroupStore/ProjectStore/TokenRepository interface only (never
// *MemoryDirectory- or *store.SQLStore-specific methods), with identical
// seed data and identical hardcoded-expected assertions — so a divergence in
// either implementation fails its own t.Run() subtest, exactly like
// internal/store/routing_store_conformance_test.go (RT-2) does for
// routing.Store.
//
// FK handling mirrors routing_store_conformance_test.go's
// forEachRoutingStoreSeeded pattern: SQLite enforces real foreign keys on
// user_groups.parent_group_id/.owner_user_id, user_group_members.user_id,
// user_group_managers.user_id, project_members.user_id,
// project_groups.group_id, and api_tokens.user_id/.service_id/.project_id;
// *MemoryDirectory enforces NONE of them (see its own doc comments in
// memory_directory.go — "only id uniqueness on Create" — an already
// documented, intentional driver limitation, not something this suite
// re-litigates). So every seedSQL hook below creates the parent rows
// directly against the concrete *store.SQLStore BEFORE it is downcast to the
// narrow interface, and every shared subtest only ever references EXISTING
// parents — it never probes the "reject a dangling reference" edge case
// (each backend's own per-backend test suite already covers that
// separately: internal/store/conformance_test.go's testUserGroups/
// testProjects for SQL, memory_directory_groups_test.go for memory).
//
// A genuine, previously-untested divergence WAS found while building this
// suite: see TestTokenRepositoryCreatePlainTokenUniqueness below.

// newConformanceSQLStore opens a freshly migrated, temp-file-backed
// *store.SQLStore for one subtest (t.TempDir() is cleaned up by the testing
// package; the store itself is closed via t.Cleanup).
func newConformanceSQLStore(t *testing.T) *store.SQLStore {
	t.Helper()
	s, err := store.OpenSQLite(filepath.Join(t.TempDir(), "st3-store-conformance.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	return s
}

// forEachGroupStore runs run against both production GroupStore backends:
// a fresh *MemoryDirectory and a freshly migrated *store.SQLStore. seedSQL,
// when non-nil, runs against the concrete *store.SQLStore BEFORE it is
// downcast to GroupStore, to satisfy an FK the interface itself has no
// method to populate (e.g. a user row a group's owner_user_id references).
func forEachGroupStore(t *testing.T, seedSQL func(t *testing.T, s *store.SQLStore), run func(t *testing.T, gs GroupStore)) {
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryDirectory(auth.NewTokenStore()))
	})
	t.Run("sqlite", func(t *testing.T) {
		s := newConformanceSQLStore(t)
		if seedSQL != nil {
			seedSQL(t, s)
		}
		run(t, s)
	})
}

// forEachProjectStore is forEachGroupStore's ProjectStore counterpart.
func forEachProjectStore(t *testing.T, seedSQL func(t *testing.T, s *store.SQLStore), run func(t *testing.T, ps ProjectStore)) {
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryDirectory(auth.NewTokenStore()))
	})
	t.Run("sqlite", func(t *testing.T) {
		s := newConformanceSQLStore(t)
		if seedSQL != nil {
			seedSQL(t, s)
		}
		run(t, s)
	})
}

// forEachTokenStore is forEachGroupStore's TokenRepository counterpart.
func forEachTokenStore(t *testing.T, seedSQL func(t *testing.T, s *store.SQLStore), run func(t *testing.T, tr TokenRepository)) {
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryDirectory(auth.NewTokenStore()))
	})
	t.Run("sqlite", func(t *testing.T) {
		s := newConformanceSQLStore(t)
		if seedSQL != nil {
			seedSQL(t, s)
		}
		run(t, s)
	})
}

// groupProjectStore is the combined GroupStore+ProjectStore view used ONLY by
// TestProjectStoreCoupledGroupSetNullOnGroupDelete below, whose assertion
// spans both interfaces (a GroupStore delete cascading into ProjectStore
// state). Both production backends already satisfy it — see the `var _`
// assertions in group_store.go/project_store.go.
type groupProjectStore interface {
	GroupStore
	ProjectStore
}

func forEachGroupProjectStore(t *testing.T, run func(t *testing.T, gps groupProjectStore)) {
	t.Run("memory", func(t *testing.T) {
		run(t, NewMemoryDirectory(auth.NewTokenStore()))
	})
	t.Run("sqlite", func(t *testing.T) {
		run(t, newConformanceSQLStore(t))
	})
}

func containsGroupID(groups []store.UserGroup, id string) bool {
	for _, g := range groups {
		if g.ID == id {
			return true
		}
	}
	return false
}

func containsProjectID(projects []store.Project, id string) bool {
	for _, p := range projects {
		if p.ID == id {
			return true
		}
	}
	return false
}

// --- GroupStore --------------------------------------------------------

// TestGroupStoreConformance covers the GroupStore contract's core behavioral
// surface identically to internal/store/conformance_test.go's testUserGroups
// + testUserGroupManagerPerms, but through the GroupStore interface against
// BOTH backends: group CRUD (rename/reowner; ParentGroupID/Tier stay
// immutable via Update), id-uniqueness (ErrConflict — a real cross-backend
// invariant, unlike the FK checks this suite deliberately does not probe),
// membership (incl. an invited row that must NOT satisfy a "member" state
// filter), the upsert-not-duplicate semantics of SetUserGroupMember/
// SetUserGroupManager, the five independent per-manager permission flags,
// and the recursive delete cascade into membership/manager rows.
func TestGroupStoreConformance(t *testing.T) {
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		for _, id := range []string{"usr_gc1", "usr_gc2", "usr_gc3"} {
			if err := s.CreateUser(ctx, store.User{
				ID: id, Email: id + "@x.test", DisplayName: id, Role: "user",
				Status: store.UserStatusActive, PreferredLanguage: "de",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
	}

	forEachGroupStore(t, seedSQL, func(t *testing.T, gs GroupStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		sys := store.UserGroup{ID: "ugrp_gc_sys", Tier: store.GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}
		if err := gs.CreateUserGroup(ctx, sys); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		adm := store.UserGroup{ID: "ugrp_gc_adm", Tier: store.GroupTierAdmin, Name: "Adm", ParentGroupID: sys.ID, OwnerUserID: "usr_gc1", CreatedAt: now, UpdatedAt: now}
		if err := gs.CreateUserGroup(ctx, adm); err != nil {
			t.Fatalf("create admin group: %v", err)
		}

		// Duplicate id -> ErrConflict. This is id-uniqueness, not an FK check,
		// so both backends must enforce it identically.
		if err := gs.CreateUserGroup(ctx, adm); err != store.ErrConflict {
			t.Fatalf("create duplicate group id = %v, want ErrConflict", err)
		}

		got, err := gs.UserGroupByID(ctx, adm.ID)
		if err != nil {
			t.Fatalf("UserGroupByID: %v", err)
		}
		if got.ParentGroupID != sys.ID || got.OwnerUserID != "usr_gc1" || got.Tier != store.GroupTierAdmin || got.Name != "Adm" {
			t.Fatalf("roundtrip: %+v", got)
		}
		gotSys, err := gs.UserGroupByID(ctx, sys.ID)
		if err != nil || gotSys.ParentGroupID != "" || gotSys.OwnerUserID != "" {
			t.Fatalf("system group parent/owner not empty: %+v err=%v", gotSys, err)
		}
		if _, err := gs.UserGroupByID(ctx, "ugrp_gc_missing"); err != store.ErrNotFound {
			t.Fatalf("UserGroupByID(missing) = %v, want ErrNotFound", err)
		}

		// Update: rename + reowner; ParentGroupID/Tier are not writable via Update.
		updated := got
		updated.Name = "Adm Renamed"
		updated.OwnerUserID = "usr_gc2"
		updated.UpdatedAt = now.Add(time.Minute)
		if err := gs.UpdateUserGroup(ctx, updated); err != nil {
			t.Fatalf("UpdateUserGroup: %v", err)
		}
		got, err = gs.UserGroupByID(ctx, adm.ID)
		if err != nil || got.Name != "Adm Renamed" || got.OwnerUserID != "usr_gc2" || got.ParentGroupID != sys.ID {
			t.Fatalf("after update: %+v err=%v", got, err)
		}
		if err := gs.UpdateUserGroup(ctx, store.UserGroup{ID: "ugrp_gc_missing", Name: "x", UpdatedAt: now}); err != store.ErrNotFound {
			t.Fatalf("UpdateUserGroup(missing) = %v, want ErrNotFound", err)
		}

		// ListUserGroupsByTier(admin): SQLite's own migration seeds its fixed
		// DefaultAdminGroupID row at this tier (migration v44), so assert
		// PRESENCE of ours, not an exact count — mirrors testUserGroups.
		byTier, err := gs.ListUserGroupsByTier(ctx, store.GroupTierAdmin)
		if err != nil {
			t.Fatalf("ListUserGroupsByTier(admin): %v", err)
		}
		if !containsGroupID(byTier, adm.ID) {
			t.Fatalf("ListUserGroupsByTier(admin) missing %s: %+v", adm.ID, byTier)
		}
		// ChildUserGroups is scoped to OUR fresh parent id, so an exact count
		// is safe even with the seeded defaults present on the SQL side.
		children, err := gs.ChildUserGroups(ctx, sys.ID)
		if err != nil || len(children) != 1 || children[0].ID != adm.ID {
			t.Fatalf("ChildUserGroups(sys) = %+v err=%v", children, err)
		}

		// Membership: usr_gc1 is a full member, usr_gc2 is invited by usr_gc1.
		if err := gs.SetUserGroupMember(ctx, adm.ID, "usr_gc1", store.GroupStateMember, ""); err != nil {
			t.Fatalf("SetUserGroupMember(member): %v", err)
		}
		if err := gs.SetUserGroupMember(ctx, adm.ID, "usr_gc2", store.GroupStateInvited, "usr_gc1"); err != nil {
			t.Fatalf("SetUserGroupMember(invited): %v", err)
		}
		members, err := gs.UserGroupMembers(ctx, adm.ID)
		if err != nil || len(members) != 2 {
			t.Fatalf("UserGroupMembers = %+v err=%v, want 2", members, err)
		}
		var sawInvited bool
		for _, m := range members {
			if m.UserID == "usr_gc2" {
				sawInvited = true
				if m.State != store.GroupStateInvited || m.InvitedBy != "usr_gc1" {
					t.Fatalf("invited member wrong: %+v", m)
				}
			}
		}
		if !sawInvited {
			t.Fatalf("invited member usr_gc2 missing: %+v", members)
		}

		// Upsert dedup: re-set usr_gc2 to member state -- no duplicate row.
		if err := gs.SetUserGroupMember(ctx, adm.ID, "usr_gc2", store.GroupStateMember, ""); err != nil {
			t.Fatalf("SetUserGroupMember(upsert): %v", err)
		}
		members, _ = gs.UserGroupMembers(ctx, adm.ID)
		if len(members) != 2 {
			t.Fatalf("members after upsert = %d, want 2 (no dup row): %+v", len(members), members)
		}

		// UserGroupsForUser: tier+state filter, any/any lookup, and the
		// invited-does-not-satisfy-member-filter rule.
		gcOne, err := gs.UserGroupsForUser(ctx, "usr_gc1", store.GroupTierAdmin, store.GroupStateMember)
		if err != nil || len(gcOne) != 1 || gcOne[0].ID != adm.ID {
			t.Fatalf("UserGroupsForUser(gc1, admin, member) = %+v err=%v", gcOne, err)
		}
		gcAny, err := gs.UserGroupsForUser(ctx, "usr_gc1", "", "")
		if err != nil || len(gcAny) != 1 || gcAny[0].ID != adm.ID {
			t.Fatalf("UserGroupsForUser(gc1, any, any) = %+v err=%v", gcAny, err)
		}
		if err := gs.SetUserGroupMember(ctx, adm.ID, "usr_gc2", store.GroupStateInvited, "usr_gc1"); err != nil {
			t.Fatalf("re-invite gc2: %v", err)
		}
		gcInvited, err := gs.UserGroupsForUser(ctx, "usr_gc2", store.GroupTierAdmin, store.GroupStateMember)
		if err != nil || len(gcInvited) != 0 {
			t.Fatalf("invited state leaked into member filter: %+v err=%v", gcInvited, err)
		}

		if err := gs.RemoveUserGroupMember(ctx, adm.ID, "usr_gc2"); err != nil {
			t.Fatalf("RemoveUserGroupMember: %v", err)
		}
		members, _ = gs.UserGroupMembers(ctx, adm.ID)
		if len(members) != 1 || members[0].UserID != "usr_gc1" {
			t.Fatalf("members after remove = %+v, want only gc1", members)
		}

		// Managers + the five independent per-permission flags.
		if err := gs.SetUserGroupManager(ctx, adm.ID, "usr_gc2"); err != nil {
			t.Fatalf("SetUserGroupManager: %v", err)
		}
		if err := gs.SetUserGroupManager(ctx, adm.ID, "usr_gc2"); err != nil {
			t.Fatalf("SetUserGroupManager(re-set): %v", err)
		}
		mgrs, err := gs.UserGroupManagers(ctx, adm.ID)
		if err != nil || len(mgrs) != 1 || mgrs[0] != "usr_gc2" {
			t.Fatalf("UserGroupManagers = %+v err=%v", mgrs, err)
		}
		perms, err := gs.UserGroupManagerPerms(ctx, adm.ID)
		if err != nil || len(perms) != 1 || perms[0].UserID != "usr_gc2" ||
			!perms[0].CanManageUsers || !perms[0].CanManageGroup || !perms[0].CanManageServers ||
			!perms[0].CanManageServices || !perms[0].CanManageResources {
			t.Fatalf("UserGroupManagerPerms(fresh insert) = %+v err=%v, want all-true default", perms, err)
		}
		// Narrow CanManageUsers only; the other four stay true.
		if err := gs.SetUserGroupManagerPermissions(ctx, adm.ID, store.UserGroupManagerPerm{UserID: "usr_gc2", CanManageUsers: false, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != nil {
			t.Fatalf("SetUserGroupManagerPermissions(narrow users): %v", err)
		}
		perms, err = gs.UserGroupManagerPerms(ctx, adm.ID)
		if err != nil || len(perms) != 1 || perms[0].CanManageUsers || !perms[0].CanManageGroup || !perms[0].CanManageServers || !perms[0].CanManageServices || !perms[0].CanManageResources {
			t.Fatalf("UserGroupManagerPerms(after narrow) = %+v err=%v, want false/true/true/true/true", perms, err)
		}
		// Narrow CanManageResources INDEPENDENTLY of CanManageUsers.
		if err := gs.SetUserGroupManagerPermissions(ctx, adm.ID, store.UserGroupManagerPerm{UserID: "usr_gc2", CanManageUsers: false, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: false}); err != nil {
			t.Fatalf("SetUserGroupManagerPermissions(narrow resources too): %v", err)
		}
		perms, err = gs.UserGroupManagerPerms(ctx, adm.ID)
		if err != nil || len(perms) != 1 || perms[0].CanManageUsers || perms[0].CanManageResources || !perms[0].CanManageGroup {
			t.Fatalf("UserGroupManagerPerms(after independent narrow) = %+v err=%v, want false/true/true/true/false", perms, err)
		}
		// A re-set of an EXISTING manager must not reset its narrowed flags.
		if err := gs.SetUserGroupManager(ctx, adm.ID, "usr_gc2"); err != nil {
			t.Fatalf("SetUserGroupManager(re-set existing): %v", err)
		}
		perms, _ = gs.UserGroupManagerPerms(ctx, adm.ID)
		if len(perms) != 1 || perms[0].CanManageUsers || perms[0].CanManageResources {
			t.Fatalf("re-set of an existing manager reset its narrowed flags: %+v", perms)
		}
		// SetUserGroupManagerPermissions against a group that exists but has
		// no such manager row -> ErrNotFound (a zero-rows-affected update).
		if err := gs.SetUserGroupManagerPermissions(ctx, adm.ID, store.UserGroupManagerPerm{UserID: "usr_gc_no_row", CanManageUsers: true, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != store.ErrNotFound {
			t.Fatalf("SetUserGroupManagerPermissions(no manager row) = %v, want ErrNotFound", err)
		}
		if err := gs.RemoveUserGroupManager(ctx, adm.ID, "usr_gc2"); err != nil {
			t.Fatalf("RemoveUserGroupManager: %v", err)
		}
		mgrs, _ = gs.UserGroupManagers(ctx, adm.ID)
		if len(mgrs) != 0 {
			t.Fatalf("managers after remove = %+v, want empty", mgrs)
		}
		if perms, err := gs.UserGroupManagerPerms(ctx, adm.ID); err != nil || len(perms) != 0 {
			t.Fatalf("UserGroupManagerPerms(none left) = %+v err=%v, want empty (non-nil)", perms, err)
		}

		// Re-add a manager so the cascade assertion below has something to
		// prove wrong.
		if err := gs.SetUserGroupManager(ctx, adm.ID, "usr_gc3"); err != nil {
			t.Fatalf("SetUserGroupManager(re-add): %v", err)
		}

		// Cascade: deleting the system group removes the admin child + its
		// member/manager rows.
		if err := gs.DeleteUserGroup(ctx, sys.ID); err != nil {
			t.Fatalf("DeleteUserGroup(system): %v", err)
		}
		if _, err := gs.UserGroupByID(ctx, adm.ID); err != store.ErrNotFound {
			t.Fatalf("child group not cascade-deleted: %v", err)
		}
		if m, err := gs.UserGroupMembers(ctx, adm.ID); err != nil || len(m) != 0 {
			t.Fatalf("members not cascaded: %+v err=%v", m, err)
		}
		if m, err := gs.UserGroupManagers(ctx, adm.ID); err != nil || len(m) != 0 {
			t.Fatalf("managers not cascaded: %+v err=%v", m, err)
		}
		if err := gs.DeleteUserGroup(ctx, sys.ID); err != store.ErrNotFound {
			t.Fatalf("DeleteUserGroup(already deleted) = %v, want ErrNotFound", err)
		}
	})
}

// TestGroupStoreConformanceCascadeMultiLevel proves the recursive part of the
// delete cascade on BOTH backends: a grandchild (user-tier group under an
// admin group under a system group) is removed, together with its own
// membership rows, when the system-tier ancestor is deleted -- not just the
// direct admin-tier child.
func TestGroupStoreConformanceCascadeMultiLevel(t *testing.T) {
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateUser(ctx, store.User{
			ID: "usr_gcm_leaf", Email: "usr_gcm_leaf@x.test", DisplayName: "leaf", Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed leaf user: %v", err)
		}
	}

	forEachGroupStore(t, seedSQL, func(t *testing.T, gs GroupStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		if err := gs.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_gcm_sys", Tier: store.GroupTierSystem, Name: "Sys", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create system group: %v", err)
		}
		if err := gs.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_gcm_adm", Tier: store.GroupTierAdmin, Name: "Adm", ParentGroupID: "ugrp_gcm_sys", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create admin group: %v", err)
		}
		if err := gs.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_gcm_usr", Tier: store.GroupTierUser, Name: "User", ParentGroupID: "ugrp_gcm_adm", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("create user group: %v", err)
		}
		if err := gs.SetUserGroupMember(ctx, "ugrp_gcm_usr", "usr_gcm_leaf", store.GroupStateMember, ""); err != nil {
			t.Fatalf("SetUserGroupMember: %v", err)
		}

		if err := gs.DeleteUserGroup(ctx, "ugrp_gcm_sys"); err != nil {
			t.Fatalf("DeleteUserGroup(system): %v", err)
		}
		if _, err := gs.UserGroupByID(ctx, "ugrp_gcm_adm"); err != store.ErrNotFound {
			t.Fatalf("direct child not deleted: %v", err)
		}
		if _, err := gs.UserGroupByID(ctx, "ugrp_gcm_usr"); err != store.ErrNotFound {
			t.Fatalf("grandchild not deleted: %v", err)
		}
		if m, err := gs.UserGroupMembers(ctx, "ugrp_gcm_usr"); err != nil || len(m) != 0 {
			t.Fatalf("grandchild members not cascaded: %+v err=%v", m, err)
		}
	})
}

// TestGroupStoreManageableUserIDsSemantics proves, at the GroupStore
// primitive level, that both backends produce the SAME manageable-user-id
// set for the exact composition Service.ManageableUserIDs performs (see
// internal/portal/service_user_groups.go): self plus, for every admin-tier
// group the principal owns OR co-manages with CanManageUsers=true, that
// group's member-state roster. computeManageable below is a literal,
// deliberate copy of ManageableUserIDs' non-system-principal branch, built
// from GroupStore calls only, so this test would fail if
// UserGroupsForUser/UserGroupManagerPerms/UserGroupMembers ever diverged
// between backends in a way that would change ManageableUserIDs' real
// output. It stops short of constructing two full *Service instances (a
// materially larger harness) because ManageableUserIDs itself adds no
// authz-relevant logic beyond this composition.
func TestGroupStoreManageableUserIDsSemantics(t *testing.T) {
	userIDs := []string{"usr_mu_owner", "usr_mu_cmu", "usr_mu_cmg", "usr_mu_pm"}
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		for _, id := range userIDs {
			if err := s.CreateUser(ctx, store.User{
				ID: id, Email: id + "@x.test", DisplayName: id, Role: "user",
				Status: store.UserStatusActive, PreferredLanguage: "de",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
	}

	forEachGroupStore(t, seedSQL, func(t *testing.T, gs GroupStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		ag := store.UserGroup{ID: "ugrp_mu_ag", Tier: store.GroupTierAdmin, Name: "AG", OwnerUserID: "usr_mu_owner", CreatedAt: now, UpdatedAt: now}
		if err := gs.CreateUserGroup(ctx, ag); err != nil {
			t.Fatalf("create admin group: %v", err)
		}
		for _, id := range userIDs {
			if err := gs.SetUserGroupMember(ctx, ag.ID, id, store.GroupStateMember, ""); err != nil {
				t.Fatalf("add member %s: %v", id, err)
			}
		}
		// usr_mu_cmu is a co-manager WITH CanManageUsers; usr_mu_cmg is a
		// co-manager WITHOUT it (the inverse facet, proving the gate reads
		// CanManageUsers specifically). usr_mu_pm stays a plain member.
		if err := gs.SetUserGroupManager(ctx, ag.ID, "usr_mu_cmu"); err != nil {
			t.Fatalf("promote cmu: %v", err)
		}
		if err := gs.SetUserGroupManagerPermissions(ctx, ag.ID, store.UserGroupManagerPerm{UserID: "usr_mu_cmu", CanManageUsers: true, CanManageGroup: false, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != nil {
			t.Fatalf("set cmu perms: %v", err)
		}
		if err := gs.SetUserGroupManager(ctx, ag.ID, "usr_mu_cmg"); err != nil {
			t.Fatalf("promote cmg: %v", err)
		}
		if err := gs.SetUserGroupManagerPermissions(ctx, ag.ID, store.UserGroupManagerPerm{UserID: "usr_mu_cmg", CanManageUsers: false, CanManageGroup: true, CanManageServers: true, CanManageServices: true, CanManageResources: true}); err != nil {
			t.Fatalf("set cmg perms: %v", err)
		}

		computeManageable := func(principal string) map[string]bool {
			manageable := map[string]bool{principal: true}
			groups, err := gs.UserGroupsForUser(ctx, principal, store.GroupTierAdmin, store.GroupStateMember)
			if err != nil {
				t.Fatalf("UserGroupsForUser(%s): %v", principal, err)
			}
			for _, g := range groups {
				canManageUsers := g.OwnerUserID != "" && g.OwnerUserID == principal
				if !canManageUsers {
					perms, err := gs.UserGroupManagerPerms(ctx, g.ID)
					if err != nil {
						t.Fatalf("UserGroupManagerPerms(%s): %v", g.ID, err)
					}
					for _, p := range perms {
						if p.UserID == principal && p.CanManageUsers {
							canManageUsers = true
							break
						}
					}
				}
				if !canManageUsers {
					continue
				}
				members, err := gs.UserGroupMembers(ctx, g.ID)
				if err != nil {
					t.Fatalf("UserGroupMembers(%s): %v", g.ID, err)
				}
				for _, m := range members {
					if m.State == store.GroupStateMember {
						manageable[m.UserID] = true
					}
				}
			}
			return manageable
		}

		full := map[string]bool{"usr_mu_owner": true, "usr_mu_cmu": true, "usr_mu_cmg": true, "usr_mu_pm": true}
		assertSet := func(label, principal string, want map[string]bool) {
			got := computeManageable(principal)
			if len(got) != len(want) {
				t.Fatalf("%s manageable = %v, want %v", label, got, want)
			}
			for id := range want {
				if !got[id] {
					t.Fatalf("%s manageable missing %s: %v", label, id, got)
				}
			}
		}

		assertSet("owner", "usr_mu_owner", full)
		assertSet("co-manager-with-CanManageUsers", "usr_mu_cmu", full)
		assertSet("co-manager-without-CanManageUsers", "usr_mu_cmg", map[string]bool{"usr_mu_cmg": true})
		assertSet("plain-member", "usr_mu_pm", map[string]bool{"usr_mu_pm": true})
	})
}

// --- ProjectStore --------------------------------------------------------

// TestProjectStoreConformance covers the ProjectStore contract identically to
// internal/store/conformance_test.go's testProjects, through the
// ProjectStore interface against BOTH backends: project CRUD (rename,
// description change, owner transfer), id-uniqueness (ErrConflict),
// members/groups add-remove with upsert-not-duplicate semantics,
// ProjectsByOwnerOrMember's owner-vs-member-vs-neither split,
// ProjectsByGroup, the delete cascade into member/group rows, and the
// coupled-project (CoupledGroupID) round trip + CoupledProjectsByGroup.
func TestProjectStoreConformance(t *testing.T) {
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		for _, id := range []string{"usr_pc1", "usr_pc2", "usr_pc3"} {
			if err := s.CreateUser(ctx, store.User{
				ID: id, Email: id + "@x.test", DisplayName: id, Role: "user",
				Status: store.UserStatusActive, PreferredLanguage: "de",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
		if err := s.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_pc_a", Tier: store.GroupTierSystem, Name: "PGA", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed group A: %v", err)
		}
		if err := s.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_pc_b", Tier: store.GroupTierSystem, Name: "PGB", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed group B: %v", err)
		}
		if err := s.CreateUserGroup(ctx, store.UserGroup{ID: "ugrp_pc_c", Tier: store.GroupTierSystem, Name: "PGC", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed group C: %v", err)
		}
	}

	forEachProjectStore(t, seedSQL, func(t *testing.T, ps ProjectStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		proj := store.Project{ID: "proj_pc_1", Name: "Alpha", Description: "d", OwnerUserID: "usr_pc1", CreatedAt: now, UpdatedAt: now}
		if err := ps.CreateProject(ctx, proj); err != nil {
			t.Fatalf("create project: %v", err)
		}
		if err := ps.CreateProject(ctx, proj); err != store.ErrConflict {
			t.Fatalf("create duplicate project id = %v, want ErrConflict", err)
		}

		got, err := ps.ProjectByID(ctx, proj.ID)
		if err != nil || got.Name != "Alpha" || got.OwnerUserID != "usr_pc1" || got.Description != "d" {
			t.Fatalf("roundtrip: %+v err=%v", got, err)
		}
		if _, err := ps.ProjectByID(ctx, "proj_pc_missing"); err != store.ErrNotFound {
			t.Fatalf("ProjectByID(missing) = %v, want ErrNotFound", err)
		}

		// Update: rename + change description + TRANSFER ownership.
		updated := got
		updated.Name = "Alpha Renamed"
		updated.Description = "d2"
		updated.OwnerUserID = "usr_pc2"
		updated.UpdatedAt = now.Add(time.Minute)
		if err := ps.UpdateProject(ctx, updated); err != nil {
			t.Fatalf("UpdateProject (transfer): %v", err)
		}
		got, err = ps.ProjectByID(ctx, proj.ID)
		if err != nil || got.Name != "Alpha Renamed" || got.Description != "d2" || got.OwnerUserID != "usr_pc2" {
			t.Fatalf("after transfer: %+v err=%v", got, err)
		}
		if err := ps.UpdateProject(ctx, store.Project{ID: "proj_pc_missing", Name: "x", UpdatedAt: now}); err != store.ErrNotFound {
			t.Fatalf("UpdateProject(missing) = %v, want ErrNotFound", err)
		}
		// Restore owner to usr_pc1 for the ownership assertions below.
		got.OwnerUserID = "usr_pc1"
		if err := ps.UpdateProject(ctx, got); err != nil {
			t.Fatalf("UpdateProject(restore owner): %v", err)
		}

		list, err := ps.ListProjects(ctx)
		if err != nil || !containsProjectID(list, proj.ID) {
			t.Fatalf("ListProjects missing %s: %+v err=%v", proj.ID, list, err)
		}

		// Direct member usr_pc2, with upsert-not-duplicate semantics.
		if err := ps.SetProjectMember(ctx, proj.ID, "usr_pc2"); err != nil {
			t.Fatalf("SetProjectMember: %v", err)
		}
		if err := ps.SetProjectMember(ctx, proj.ID, "usr_pc2"); err != nil {
			t.Fatalf("SetProjectMember(re-set): %v", err)
		}
		members, err := ps.ProjectMembers(ctx, proj.ID)
		if err != nil || len(members) != 1 || members[0] != "usr_pc2" {
			t.Fatalf("ProjectMembers = %+v err=%v", members, err)
		}

		// Assign both groups.
		if err := ps.SetProjectGroup(ctx, proj.ID, "ugrp_pc_a"); err != nil {
			t.Fatalf("SetProjectGroup(A): %v", err)
		}
		if err := ps.SetProjectGroup(ctx, proj.ID, "ugrp_pc_b"); err != nil {
			t.Fatalf("SetProjectGroup(B): %v", err)
		}
		groups, err := ps.ProjectGroups(ctx, proj.ID)
		if err != nil || len(groups) != 2 {
			t.Fatalf("ProjectGroups = %+v err=%v, want 2", groups, err)
		}

		// ProjectsByOwnerOrMember: owner, direct member, neither.
		byOwner, err := ps.ProjectsByOwnerOrMember(ctx, "usr_pc1")
		if err != nil || !containsProjectID(byOwner, proj.ID) {
			t.Fatalf("ProjectsByOwnerOrMember(owner) = %+v err=%v", byOwner, err)
		}
		byMember, err := ps.ProjectsByOwnerOrMember(ctx, "usr_pc2")
		if err != nil || !containsProjectID(byMember, proj.ID) {
			t.Fatalf("ProjectsByOwnerOrMember(member) = %+v err=%v", byMember, err)
		}
		byNone, err := ps.ProjectsByOwnerOrMember(ctx, "usr_pc3")
		if err != nil || len(byNone) != 0 {
			t.Fatalf("ProjectsByOwnerOrMember(nonmember) = %+v err=%v, want empty", byNone, err)
		}

		byGroupA, err := ps.ProjectsByGroup(ctx, "ugrp_pc_a")
		if err != nil || len(byGroupA) != 1 || byGroupA[0].ID != proj.ID {
			t.Fatalf("ProjectsByGroup(A) = %+v err=%v", byGroupA, err)
		}

		if err := ps.RemoveProjectGroup(ctx, proj.ID, "ugrp_pc_b"); err != nil {
			t.Fatalf("RemoveProjectGroup(B): %v", err)
		}
		groups, _ = ps.ProjectGroups(ctx, proj.ID)
		if len(groups) != 1 || groups[0] != "ugrp_pc_a" {
			t.Fatalf("groups after direct remove = %+v, want only A", groups)
		}

		if err := ps.RemoveProjectMember(ctx, proj.ID, "usr_pc2"); err != nil {
			t.Fatalf("RemoveProjectMember: %v", err)
		}
		members, _ = ps.ProjectMembers(ctx, proj.ID)
		if len(members) != 0 {
			t.Fatalf("members after remove = %+v, want empty (non-nil)", members)
		}

		// Delete the project -> remaining group row cascades away too.
		if err := ps.DeleteProject(ctx, proj.ID); err != nil {
			t.Fatalf("DeleteProject: %v", err)
		}
		if _, err := ps.ProjectByID(ctx, proj.ID); err != store.ErrNotFound {
			t.Fatalf("project not deleted: %v", err)
		}
		if m, err := ps.ProjectMembers(ctx, proj.ID); err != nil || len(m) != 0 {
			t.Fatalf("members not cascaded on project delete: %+v err=%v", m, err)
		}
		if g, err := ps.ProjectGroups(ctx, proj.ID); err != nil || len(g) != 0 {
			t.Fatalf("groups not cascaded on project delete: %+v err=%v", g, err)
		}
		if err := ps.DeleteProject(ctx, proj.ID); err != store.ErrNotFound {
			t.Fatalf("DeleteProject(already deleted) = %v, want ErrNotFound", err)
		}

		// Coupled project: CoupledGroupID round trip + CoupledProjectsByGroup.
		coupled := store.Project{ID: "prj_pc_coupled", Name: "Coupled", CoupledGroupID: "ugrp_pc_c", CreatedAt: now, UpdatedAt: now}
		if err := ps.CreateProject(ctx, coupled); err != nil {
			t.Fatalf("create coupled project: %v", err)
		}
		if err := ps.SetProjectGroup(ctx, coupled.ID, "ugrp_pc_c"); err != nil {
			t.Fatalf("set coupled project group: %v", err)
		}
		gotCoupled, err := ps.ProjectByID(ctx, coupled.ID)
		if err != nil || gotCoupled.CoupledGroupID != "ugrp_pc_c" {
			t.Fatalf("coupled roundtrip: %+v err=%v", gotCoupled, err)
		}
		byGroup, err := ps.CoupledProjectsByGroup(ctx, "ugrp_pc_c")
		if err != nil || len(byGroup) != 1 || byGroup[0].ID != coupled.ID {
			t.Fatalf("CoupledProjectsByGroup = %+v err=%v", byGroup, err)
		}
	})
}

// TestProjectStoreCoupledGroupSetNullOnGroupDelete proves the cross-interface
// cascade behavior testProjects also covers for SQL alone (migration46Up):
// deleting a coupled project's group SET-NULLs Project.CoupledGroupID AND
// drops the mirror project_groups row, on BOTH backends. This needs both
// GroupStore.DeleteUserGroup and ProjectStore methods against the SAME
// backend value, hence the combined groupProjectStore interface.
func TestProjectStoreCoupledGroupSetNullOnGroupDelete(t *testing.T) {
	forEachGroupProjectStore(t, func(t *testing.T, gps groupProjectStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		grp := store.UserGroup{ID: "ugrp_pcx_c", Tier: store.GroupTierSystem, Name: "Coupled Group", CreatedAt: now, UpdatedAt: now}
		if err := gps.CreateUserGroup(ctx, grp); err != nil {
			t.Fatalf("create group: %v", err)
		}
		proj := store.Project{ID: "prj_pcx_coupled", Name: "Coupled", CoupledGroupID: grp.ID, CreatedAt: now, UpdatedAt: now}
		if err := gps.CreateProject(ctx, proj); err != nil {
			t.Fatalf("create coupled project: %v", err)
		}
		if err := gps.SetProjectGroup(ctx, proj.ID, grp.ID); err != nil {
			t.Fatalf("set project group mirror row: %v", err)
		}

		if err := gps.DeleteUserGroup(ctx, grp.ID); err != nil {
			t.Fatalf("delete coupled group: %v", err)
		}
		after, err := gps.ProjectByID(ctx, proj.ID)
		if err != nil || after.CoupledGroupID != "" {
			t.Fatalf("after group delete CoupledGroupID = %q err=%v, want empty (SET NULL)", after.CoupledGroupID, err)
		}
		if groups, err := gps.ProjectGroups(ctx, proj.ID); err != nil || len(groups) != 0 {
			t.Fatalf("project_groups mirror row not cascade-deleted: %+v err=%v", groups, err)
		}
		if left, err := gps.CoupledProjectsByGroup(ctx, grp.ID); err != nil || len(left) != 0 {
			t.Fatalf("CoupledProjectsByGroup after delete = %+v err=%v, want empty", left, err)
		}
	})
}

// --- TokenRepository -----------------------------------------------------

// TestTokenRepositoryConformance covers TokenRepository's core CRUD +
// listing surface against BOTH backends: create defaults (Status/Scopes/
// Kind), the ServerOverride/ServerOverrideForceUnreachable round trip
// (including that a token created WITHOUT an override reads back the
// empty/false defaults, never inheriting another token's values),
// TokensByUser/TokensByService/TokensByProject (incl. their empty-vs-nil
// shapes and TokensByProject("")'s never-matches-unassigned rule),
// UpdateTokenMetadata, RotateTokenSecret, and DeleteToken -- all with the
// same missing-id -> ErrNotFound contract on both backends.
func TestTokenRepositoryConformance(t *testing.T) {
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		for _, id := range []string{"usr_tc1", "usr_tc2"} {
			if err := s.CreateUser(ctx, store.User{
				ID: id, Email: id + "@x.test", DisplayName: id, Role: "user",
				Status: store.UserStatusActive, PreferredLanguage: "de",
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("seed user %s: %v", id, err)
			}
		}
		if err := s.CreateProject(ctx, store.Project{ID: "proj_tc", Name: "TC Project", OwnerUserID: "usr_tc1", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		if err := s.CreateService(ctx, routing.Service{ID: "svc_tc", Name: "TC Service", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("seed service: %v", err)
		}
	}

	forEachTokenStore(t, seedSQL, func(t *testing.T, tr TokenRepository) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		tok1 := store.TokenRecord{
			ID: "tok_tc1", UserID: "usr_tc1", Name: "primary", CreatedAt: now, UpdatedAt: now,
			ServerOverride: "srv_x", ServerOverrideForceUnreachable: true,
		}
		if err := tr.CreatePlainToken(ctx, tok1, "tc-secret-one"); err != nil {
			t.Fatalf("create token 1: %v", err)
		}
		tok2 := store.TokenRecord{ID: "tok_tc2", UserID: "usr_tc1", Name: "no-override", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second)}
		if err := tr.CreatePlainToken(ctx, tok2, "tc-secret-two"); err != nil {
			t.Fatalf("create token 2: %v", err)
		}
		tokProj := store.TokenRecord{ID: "tok_tc_proj", UserID: "usr_tc2", ProjectID: "proj_tc", CreatedAt: now.Add(2 * time.Second), UpdatedAt: now.Add(2 * time.Second)}
		if err := tr.CreatePlainToken(ctx, tokProj, "tc-secret-proj"); err != nil {
			t.Fatalf("create project token: %v", err)
		}
		tokSvc := store.TokenRecord{ID: "tok_tc_svc", ServiceID: "svc_tc", Kind: store.TokenKindService, CreatedAt: now.Add(3 * time.Second), UpdatedAt: now.Add(3 * time.Second)}
		if err := tr.CreatePlainToken(ctx, tokSvc, "tc-secret-svc"); err != nil {
			t.Fatalf("create service token: %v", err)
		}

		got, err := tr.TokenByID(ctx, tok1.ID)
		if err != nil || got.UserID != "usr_tc1" || got.Status != store.TokenStatusActive || got.Kind != store.TokenKindUser || got.Scopes != "[]" {
			t.Fatalf("TokenByID(defaults) = %+v err=%v", got, err)
		}
		if got.ServerOverride != "srv_x" || !got.ServerOverrideForceUnreachable {
			t.Fatalf("server override not persisted: %+v", got)
		}
		gotNoOverride, err := tr.TokenByID(ctx, tok2.ID)
		if err != nil || gotNoOverride.ServerOverride != "" || gotNoOverride.ServerOverrideForceUnreachable {
			t.Fatalf("token without override must default empty/false, never inherit: %+v err=%v", gotNoOverride, err)
		}
		if _, err := tr.TokenByID(ctx, "tok_tc_missing"); err != store.ErrNotFound {
			t.Fatalf("TokenByID(missing) = %v, want ErrNotFound", err)
		}

		byUser, err := tr.TokensByUser(ctx, "usr_tc1")
		if err != nil || len(byUser) != 2 || byUser[0].ID != tok2.ID || byUser[1].ID != tok1.ID {
			t.Fatalf("TokensByUser (newest first) = %+v err=%v", byUser, err)
		}
		byUserEmpty, err := tr.TokensByUser(ctx, "usr_tc_nobody")
		if err != nil || byUserEmpty == nil || len(byUserEmpty) != 0 {
			t.Fatalf("TokensByUser(nobody) = %+v err=%v, want empty non-nil", byUserEmpty, err)
		}

		byService, err := tr.TokensByService(ctx, "svc_tc")
		if err != nil || len(byService) != 1 || byService[0].ID != tokSvc.ID {
			t.Fatalf("TokensByService = %+v err=%v", byService, err)
		}
		byServiceEmpty, err := tr.TokensByService(ctx, "svc_nobody")
		if err != nil || byServiceEmpty == nil || len(byServiceEmpty) != 0 {
			t.Fatalf("TokensByService(nobody) = %+v err=%v, want empty non-nil", byServiceEmpty, err)
		}

		byProject, err := tr.TokensByProject(ctx, "proj_tc")
		if err != nil || len(byProject) != 1 || byProject[0].ID != tokProj.ID {
			t.Fatalf("TokensByProject = %+v err=%v", byProject, err)
		}
		byProjectEmptyID, err := tr.TokensByProject(ctx, "")
		if err != nil || byProjectEmptyID == nil || len(byProjectEmptyID) != 0 {
			t.Fatalf(`TokensByProject("") = %+v err=%v, want empty non-nil (never matches an unassigned token)`, byProjectEmptyID, err)
		}

		// UpdateTokenMetadata: change then clear the server override in the
		// same call shape; also flips status/scopes/project assignment.
		updated := got
		updated.ServerOverride = "srv_y"
		updated.ServerOverrideForceUnreachable = false
		updated.Status = store.TokenStatusDisabled
		updated.Scopes = `["gateway:use"]`
		updated.ProjectID = "proj_tc"
		updated.UpdatedAt = now.Add(time.Minute)
		if err := tr.UpdateTokenMetadata(ctx, updated); err != nil {
			t.Fatalf("UpdateTokenMetadata: %v", err)
		}
		afterUpdate, err := tr.TokenByID(ctx, tok1.ID)
		if err != nil || afterUpdate.ServerOverride != "srv_y" || afterUpdate.ServerOverrideForceUnreachable ||
			afterUpdate.Status != store.TokenStatusDisabled || afterUpdate.Scopes != `["gateway:use"]` || afterUpdate.ProjectID != "proj_tc" {
			t.Fatalf("after UpdateTokenMetadata: %+v err=%v", afterUpdate, err)
		}
		if err := tr.UpdateTokenMetadata(ctx, store.TokenRecord{ID: "tok_tc_missing", UpdatedAt: now}); err != store.ErrNotFound {
			t.Fatalf("UpdateTokenMetadata(missing) = %v, want ErrNotFound", err)
		}

		// RotateTokenSecret.
		if err := tr.RotateTokenSecret(ctx, tok2.ID, auth.HashSecret("tc-secret-rotated"), "tcrot-", now.Add(2*time.Minute)); err != nil {
			t.Fatalf("RotateTokenSecret: %v", err)
		}
		afterRotate, err := tr.TokenByID(ctx, tok2.ID)
		if err != nil || afterRotate.SecretPrefix != "tcrot-" {
			t.Fatalf("after rotate: %+v err=%v", afterRotate, err)
		}
		if err := tr.RotateTokenSecret(ctx, "tok_tc_missing", auth.HashSecret("x"), "x-", now); err != store.ErrNotFound {
			t.Fatalf("RotateTokenSecret(missing) = %v, want ErrNotFound", err)
		}

		// DeleteToken.
		if err := tr.DeleteToken(ctx, tokProj.ID); err != nil {
			t.Fatalf("DeleteToken: %v", err)
		}
		if _, err := tr.TokenByID(ctx, tokProj.ID); err != store.ErrNotFound {
			t.Fatalf("token not deleted: %v", err)
		}
		if err := tr.DeleteToken(ctx, tokProj.ID); err != store.ErrNotFound {
			t.Fatalf("DeleteToken(already deleted) = %v, want ErrNotFound", err)
		}
	})
}

// TestTokenRepositoryCreatePlainTokenUniqueness is the ST-3 DIVERGENCE
// DISCOVERY test.
//
// *** GENUINE MEMORY-VS-SQL DIVERGENCE FOUND (not previously covered by any
// test): store.SQLStore.CreatePlainToken enforces TWO real uniqueness
// constraints and maps either violation to store.ErrConflict:
//   - api_tokens.id is the PRIMARY KEY (duplicate id -> ErrConflict).
//   - api_tokens.secret_hash carries a UNIQUE index (duplicate secret,
//     even under a different id -> ErrConflict; see
//     internal/store/migrate.go's `secret_hash text not null unique` and
//     internal/store/sqlite_token.go's CreatePlainToken).
//
// portal.MemoryDirectory.CreatePlainToken (internal/portal/memory_directory.go)
// checks NEITHER: it unconditionally does `m.tokens[token.ID] = token`, so a
// duplicate id silently OVERWRITES the map entry (losing the original
// token's fields), and a duplicate secret hash is never rejected either --
// two tokens can end up sharing one bearer secret, which would make
// auth.TokenStore.LookupBearer non-deterministically resolve to whichever
// call happened to run last (auth.TokenStore keys its map by secret hash
// too; see internal/auth/token_store.go AddPlainToken). This is UNLIKE
// CreateUserGroup/CreateProject in the same file, which both correctly check
// id-uniqueness and return store.ErrConflict on a re-used id -- so this
// looks like an accidental gap in CreatePlainToken specifically, not the
// same documented "no FK enforcement" driver-wide policy that governs
// SetProjectMember/SetUserGroupMember/etc.
//
// Per the ST-3 constraints this suite must not fix production code, so this
// test detects the divergence and calls t.Skip with the discrepancy spelled
// out, rather than asserting one specific (currently wrong) memory-side
// behavior that a future incidental change could flip without anyone
// noticing. On the SQL backend the test asserts the real contract
// (ErrConflict) and FAILS if that ever regresses.
//
// TODO(ST-3 follow-up, separate task): decide whether
// MemoryDirectory.CreatePlainToken should gain the same id/secret-hash
// uniqueness checks (likely yes, to match the "memory mode is a faithful
// dev/e2e driver" goal used elsewhere in this file), then remove the
// t.Skip here once fixed.
func TestTokenRepositoryCreatePlainTokenUniqueness(t *testing.T) {
	seedSQL := func(t *testing.T, s *store.SQLStore) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
		if err := s.CreateUser(ctx, store.User{
			ID: "usr_tu1", Email: "usr_tu1@x.test", DisplayName: "usr_tu1", Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}

	forEachTokenStore(t, seedSQL, func(t *testing.T, tr TokenRepository) {
		ctx := context.Background()
		now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

		base := store.TokenRecord{ID: "tok_dupe_1", UserID: "usr_tu1", Name: "first", CreatedAt: now, UpdatedAt: now}
		if err := tr.CreatePlainToken(ctx, base, "tu-secret-one"); err != nil {
			t.Fatalf("create first token: %v", err)
		}

		// Duplicate ID, different secret -> ErrConflict on both backends
		// (MemoryDirectory mirrors the api_tokens PRIMARY KEY; the ST-3-flagged
		// divergence where memory silently overwrote the entry is fixed).
		dupID := store.TokenRecord{ID: "tok_dupe_1", UserID: "usr_tu1", Name: "second", CreatedAt: now, UpdatedAt: now}
		if err := tr.CreatePlainToken(ctx, dupID, "tu-secret-two"); err != store.ErrConflict {
			t.Fatalf("create token with duplicate id = %v, want ErrConflict", err)
		}

		// Duplicate secret, different ID -> ErrConflict on both backends
		// (MemoryDirectory mirrors the api_tokens.secret_hash UNIQUE index).
		dupSecret := store.TokenRecord{ID: "tok_dupe_2", UserID: "usr_tu1", Name: "third", CreatedAt: now, UpdatedAt: now}
		if err := tr.CreatePlainToken(ctx, dupSecret, "tu-secret-one"); err != store.ErrConflict {
			t.Fatalf("create token with duplicate secret = %v, want ErrConflict", err)
		}
	})
}

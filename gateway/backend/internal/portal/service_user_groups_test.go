// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// TestVisibleUserIDs builds: system_admin S, admin A, users U1,U2,U3.
// System group SG1 members {A, U1}; admin group AG1 (parent SG1) members
// {U1, U2}. Asserts:
//   - system_admin S -> all 5 users.
//   - admin A -> {A, U1} (SG1 members + self).
//   - user U1 -> {U1, U2} (AG1 members + self).
//   - user U3 (no groups) -> {U3} only.
func TestVisibleUserIDs(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())

	for _, id := range []string{"usr_s", "usr_a", "usr_u1", "usr_u2", "usr_u3"} {
		if err := dir.CreateUser(ctx, store.User{
			ID: id, Email: id + "@example.test", DisplayName: id, Role: "user",
			Status: store.UserStatusActive, PreferredLanguage: "de",
			CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
	}

	sg1 := store.UserGroup{ID: "ugrp_sg1", Tier: store.GroupTierSystem, Name: "SG1", CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(ctx, sg1); err != nil {
		t.Fatalf("create SG1: %v", err)
	}
	ag1 := store.UserGroup{ID: "ugrp_ag1", Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: "ugrp_sg1", OwnerUserID: "usr_a", CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(ctx, ag1); err != nil {
		t.Fatalf("create AG1: %v", err)
	}

	if err := dir.SetUserGroupMember(ctx, "ugrp_sg1", "usr_a", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SG1 member A: %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, "ugrp_sg1", "usr_u1", store.GroupStateMember, ""); err != nil {
		t.Fatalf("SG1 member U1: %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, "ugrp_ag1", "usr_u1", store.GroupStateMember, ""); err != nil {
		t.Fatalf("AG1 member U1: %v", err)
	}
	if err := dir.SetUserGroupMember(ctx, "ugrp_ag1", "usr_u2", store.GroupStateMember, ""); err != nil {
		t.Fatalf("AG1 member U2: %v", err)
	}

	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{Users: dir, Groups: dir, Routes: routeStore, Clock: func() time.Time { return now }})

	systemAdmin := auth.Token{UserID: "usr_s", Scopes: []string{"gateway:use", "system", "admin"}}
	admin := auth.Token{UserID: "usr_a", Scopes: []string{"gateway:use", "admin"}}
	u1 := auth.Token{UserID: "usr_u1", Scopes: []string{"gateway:use"}}
	u3 := auth.Token{UserID: "usr_u3", Scopes: []string{"gateway:use"}}

	// system_admin -> all 5.
	got, err := svc.VisibleUserIDs(ctx, systemAdmin)
	if err != nil {
		t.Fatalf("VisibleUserIDs(system_admin): %v", err)
	}
	wantAll := map[string]bool{"usr_s": true, "usr_a": true, "usr_u1": true, "usr_u2": true, "usr_u3": true}
	if len(got) != len(wantAll) {
		t.Fatalf("VisibleUserIDs(system_admin) = %v, want %v", got, wantAll)
	}
	for id := range wantAll {
		if !got[id] {
			t.Fatalf("VisibleUserIDs(system_admin) missing %s: %v", id, got)
		}
	}

	// admin A -> {A, U1}.
	got, err = svc.VisibleUserIDs(ctx, admin)
	if err != nil {
		t.Fatalf("VisibleUserIDs(admin): %v", err)
	}
	wantAdmin := map[string]bool{"usr_a": true, "usr_u1": true}
	if len(got) != len(wantAdmin) || !got["usr_a"] || !got["usr_u1"] {
		t.Fatalf("VisibleUserIDs(admin) = %v, want %v", got, wantAdmin)
	}

	// user U1 -> {U1, U2}.
	got, err = svc.VisibleUserIDs(ctx, u1)
	if err != nil {
		t.Fatalf("VisibleUserIDs(u1): %v", err)
	}
	wantU1 := map[string]bool{"usr_u1": true, "usr_u2": true}
	if len(got) != len(wantU1) || !got["usr_u1"] || !got["usr_u2"] {
		t.Fatalf("VisibleUserIDs(u1) = %v, want %v", got, wantU1)
	}

	// user U3 (no groups) -> {U3} only.
	got, err = svc.VisibleUserIDs(ctx, u3)
	if err != nil {
		t.Fatalf("VisibleUserIDs(u3): %v", err)
	}
	if len(got) != 1 || !got["usr_u3"] {
		t.Fatalf("VisibleUserIDs(u3) = %v, want {usr_u3}", got)
	}
}

// TestManageableUserIDs proves the Task 3 per-Admin-Group co-manager
// permissions model (spec 2026-08-10): system_admin S manages EVERY user;
// otherwise a principal manages themselves plus, for each admin group they
// OWN or CO-MANAGE with CanManageUsers=true, that group's full member
// roster. Builds: system group SG (members {O, CMU, CMG, PM, T1}); admin
// group AG (parent SG, owner O) with the same five as members; CMU promoted
// co-manager with {CanManageUsers:true, CanManageGroup:false}; CMG promoted
// co-manager with {CanManageUsers:false, CanManageGroup:true} (the inverse
// facet — proves the gate reads CanManageUsers specifically, not "is a
// manager at all"); PM stays a plain member (no manager row).
func TestManageableUserIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_o", "admin")
	e.createUser("usr_cmu", "admin")
	e.createUser("usr_cmg", "admin")
	e.createUser("usr_pm", "user")
	e.createUser("usr_t1", "user")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_o", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_o", "usr_cmu", "usr_cmg", "usr_pm", "usr_t1")

	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	if ag.OwnerUserID != "usr_o" {
		t.Fatalf("precondition: usr_o should own AG, got %+v", ag)
	}
	e.mustAddMembers(owner, ag.ID, "usr_cmu", "usr_cmg", "usr_pm", "usr_t1")

	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmu", true, false, true, true, true); err != nil {
		t.Fatalf("promote CMU: %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmg", false, true, true, true, true); err != nil {
		t.Fatalf("promote CMG: %v", err)
	}

	agMembers := map[string]bool{"usr_o": true, "usr_cmu": true, "usr_cmg": true, "usr_pm": true, "usr_t1": true}
	assertManageable := func(label string, principal auth.Token, want map[string]bool) {
		t.Helper()
		got, err := e.svc.ManageableUserIDs(e.ctx, principal)
		if err != nil {
			t.Fatalf("ManageableUserIDs(%s): %v", label, err)
		}
		if len(got) != len(want) {
			t.Fatalf("ManageableUserIDs(%s) = %v, want %v", label, got, want)
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("ManageableUserIDs(%s) missing %s: %v", label, id, got)
			}
		}
	}

	// system_admin -> everyone (6 users total).
	assertManageable("system_admin", sysAdmin, map[string]bool{
		"usr_s": true, "usr_o": true, "usr_cmu": true, "usr_cmg": true, "usr_pm": true, "usr_t1": true,
	})

	// owner -> AG's full roster (self already a member of it).
	assertManageable("owner", owner, agMembers)

	// co-manager WITH CanManageUsers=true -> AG's full roster (self included).
	assertManageable("co-manager-with-users", token("usr_cmu", "admin"), agMembers)

	// co-manager WITHOUT CanManageUsers (has CanManageGroup instead) -> self
	// only; a structure-management grant must not leak user-management reach.
	assertManageable("co-manager-without-users", token("usr_cmg", "admin"), map[string]bool{"usr_cmg": true})

	// plain member (no manager row at all) -> self only.
	assertManageable("plain-member", token("usr_pm"), map[string]bool{"usr_pm": true})
}

// TestManageableUserIDsUnionsAcrossGroups proves a principal who owns/
// co-manages MORE THAN ONE admin group gets the union of every such group's
// roster, not just the first one found.
func TestManageableUserIDsUnionsAcrossGroups(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_o", "admin")
	e.createUser("usr_x", "user")
	e.createUser("usr_y", "user")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_o", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_o", "usr_x", "usr_y")

	agOne := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-One", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, agOne.ID, "usr_x")
	agTwo := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-Two", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, agTwo.ID, "usr_y")

	got, err := e.svc.ManageableUserIDs(e.ctx, owner)
	if err != nil {
		t.Fatalf("ManageableUserIDs(owner): %v", err)
	}
	want := map[string]bool{"usr_o": true, "usr_x": true, "usr_y": true}
	if len(got) != len(want) {
		t.Fatalf("ManageableUserIDs(owner-of-two-groups) = %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Fatalf("ManageableUserIDs(owner-of-two-groups) missing %s: %v", id, got)
		}
	}
}

// TestManageableUserIDsIncludesSystemAdminGroupMembers proves that a
// system_admin who is a member of a managed admin group IS in a non-system
// caller's manageable set (spec 2026-08-10 follow-up, revised): the caller may
// perform the SUPPORT operations (limits / re-invite / TOTP reset) on that
// system_admin, all of which gate only on this set. The two DESTRUCTIVE
// operations (edit / deactivate) stay blocked downstream by account.UpdateUser's
// ErrForbiddenRole (covered by TestUpdateUserForbidsNonSystemActorAgainstSystemAdmin),
// so the account does NOT need to be hidden from the set.
func TestManageableUserIDsIncludesSystemAdminGroupMembers(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")   // the elevated system_admin
	e.createUser("usr_o", "admin")          // group owner (a normal admin)
	e.createUser("usr_sa2", "system_admin") // a system_admin who is a group member
	e.createUser("usr_t", "user")           // a normal member

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_o", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_o", "usr_sa2", "usr_t")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ag.ID, "usr_sa2", "usr_t")

	// A normal admin owning the group manages ALL its members, INCLUDING the
	// system_admin member (so limits/re-invite/TOTP-reset are reachable).
	got, err := e.svc.ManageableUserIDs(e.ctx, owner)
	if err != nil {
		t.Fatalf("ManageableUserIDs(owner): %v", err)
	}
	wantOwner := map[string]bool{"usr_o": true, "usr_sa2": true, "usr_t": true}
	if len(got) != len(wantOwner) {
		t.Fatalf("ManageableUserIDs(owner) = %v, want %v", got, wantOwner)
	}
	for id := range wantOwner {
		if !got[id] {
			t.Fatalf("ManageableUserIDs(owner) missing %s: %v", id, got)
		}
	}
	if !got["usr_sa2"] {
		t.Fatalf("owner must manage a system_admin group member (support ops): %v", got)
	}

	// An elevated system_admin manages every account, system_admins included.
	gotSys, err := e.svc.ManageableUserIDs(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ManageableUserIDs(system_admin): %v", err)
	}
	if !gotSys["usr_sa2"] || !gotSys["usr_s"] {
		t.Fatalf("elevated system_admin must manage system_admins: %v", gotSys)
	}
}

// --- shared test scaffolding (Tasks 6-8) ------------------------------------

type groupTestEnv struct {
	t      *testing.T
	ctx    context.Context
	dir    *MemoryDirectory
	svc    *Service
	now    time.Time
	routes *routing.MemoryStore
}

func newGroupTestEnv(t *testing.T) *groupTestEnv {
	t.Helper()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Users:  dir,
		Groups: dir,
		Routes: routeStore,
		Clock:  func() time.Time { return now },
	})
	return &groupTestEnv{t: t, ctx: context.Background(), dir: dir, svc: svc, now: now, routes: routeStore}
}

// createUser creates a user with the given role (one of "user"/"admin"/
// "system_admin") and returns it.
func (e *groupTestEnv) createUser(id, role string) store.User {
	e.t.Helper()
	u := store.User{
		ID: id, Email: id + "@example.test", DisplayName: id, Role: role,
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.dir.CreateUser(e.ctx, u); err != nil {
		e.t.Fatalf("create user %s: %v", id, err)
	}
	return u
}

// setUserStatus mutates an existing user's Status in place (e.g. simulating
// an admin disabling an account mid-test, after it was already promoted to
// manager/owner of a group).
func (e *groupTestEnv) setUserStatus(id, status string) {
	e.t.Helper()
	u, err := e.dir.UserByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("setUserStatus: load %s: %v", id, err)
	}
	u.Status = status
	if err := e.dir.UpdateUser(e.ctx, u); err != nil {
		e.t.Fatalf("setUserStatus: update %s: %v", id, err)
	}
}

// setUserRole mutates an existing user's Role in place (e.g. simulating a
// manager being downgraded from admin to a plain user after promotion).
func (e *groupTestEnv) setUserRole(id, role string) {
	e.t.Helper()
	u, err := e.dir.UserByID(e.ctx, id)
	if err != nil {
		e.t.Fatalf("setUserRole: load %s: %v", id, err)
	}
	u.Role = role
	if err := e.dir.UpdateUser(e.ctx, u); err != nil {
		e.t.Fatalf("setUserRole: update %s: %v", id, err)
	}
}

// mustAddMembers is a test-only shortcut around AddGroupMembers that fails
// the test on error (used to fixture up containment scenarios without
// duplicating the AddGroupMembers call-site everywhere). For a user-tier
// target group, AddGroupMembers (Task 10) only INVITES (state=invited) — this
// helper auto-accepts on behalf of each invitee immediately afterward, so
// every existing call site (fixtured before Task 10 introduced invitations)
// keeps producing a real member (state=member) as it always has.
func (e *groupTestEnv) mustAddMembers(actor auth.Token, groupID string, userIDs ...string) {
	e.t.Helper()
	if err := e.svc.AddGroupMembers(e.ctx, actor, groupID, userIDs); err != nil {
		e.t.Fatalf("AddGroupMembers(%v -> %s): %v", userIDs, groupID, err)
	}
	g, err := e.dir.UserGroupByID(e.ctx, groupID)
	if err != nil {
		e.t.Fatalf("mustAddMembers: reload group %s: %v", groupID, err)
	}
	if g.Tier != store.GroupTierUser {
		return
	}
	for _, uid := range userIDs {
		if err := e.svc.RespondInvitation(e.ctx, token(uid), groupID, true); err != nil {
			e.t.Fatalf("mustAddMembers: auto-accept invite %s -> %s: %v", uid, groupID, err)
		}
	}
}

func (e *groupTestEnv) mustCreateGroup(actor auth.Token, in CreateGroupInput) UserGroupDTO {
	e.t.Helper()
	dto, err := e.svc.CreateGroup(e.ctx, actor, in)
	if err != nil {
		e.t.Fatalf("CreateGroup(%+v): %v", in, err)
	}
	return dto
}

// token builds a principal token for userID with "gateway:use" plus any
// extra scopes (e.g. "admin", "system").
func token(userID string, scopes ...string) auth.Token {
	return auth.Token{UserID: userID, Scopes: append([]string{"gateway:use"}, scopes...)}
}

// --- Task 6: CRUD + authorization + parent rules + uniqueness + no-leak ----

func TestCreateGroup_SystemTier(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")

	if _, err := e.svc.CreateGroup(e.ctx, admin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"}); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("admin create system group: got %v, want ErrGroupForbidden", err)
	}

	dto, err := e.svc.CreateGroup(e.ctx, sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	if err != nil {
		t.Fatalf("system_admin create system group: %v", err)
	}
	if dto.Tier != store.GroupTierSystem || dto.Name != "SG1" || dto.OwnerUserID != "" || dto.ParentGroupID != "" {
		t.Fatalf("unexpected dto: %+v", dto)
	}
	if dto.MyRole != "" || !dto.CanManage {
		t.Fatalf("system group dto myrole/canmanage: %+v", dto)
	}

	// Case-insensitive global uniqueness among system groups.
	if _, err := e.svc.CreateGroup(e.ctx, sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "sg1"}); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("dup system group name: got %v, want ErrGroupNameConflict", err)
	}

	// Unknown tier -> ErrGroupTierInvalid; blank name -> ErrGroupNameInvalid.
	if _, err := e.svc.CreateGroup(e.ctx, sysAdmin, CreateGroupInput{Tier: "bogus", Name: "X"}); !errors.Is(err, ErrGroupTierInvalid) {
		t.Fatalf("bogus tier: got %v, want ErrGroupTierInvalid", err)
	}
	if _, err := e.svc.CreateGroup(e.ctx, sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "   "}); !errors.Is(err, ErrGroupNameInvalid) {
		t.Fatalf("blank name: got %v, want ErrGroupNameInvalid", err)
	}
}

func TestCreateGroup_AdminTier_ParentAutoAndValidation(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin1", "admin")
	e.createUser("usr_admin2", "admin")
	e.createUser("usr_plain", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin1 := token("usr_admin1", "admin")
	admin2 := token("usr_admin2", "admin")
	plain := token("usr_plain")

	sg1 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	e.mustAddMembers(sysAdmin, sg1.ID, "usr_admin1", "usr_admin2")
	e.mustAddMembers(sysAdmin, sg2.ID, "usr_admin2")

	if _, err := e.svc.CreateGroup(e.ctx, plain, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"}); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("plain user create admin group: got %v, want ErrGroupForbidden", err)
	}

	// admin1 is in exactly one system group -> auto-selected parent.
	ag1 := e.mustCreateGroup(admin1, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	if ag1.ParentGroupID != sg1.ID {
		t.Fatalf("auto parent = %s, want %s", ag1.ParentGroupID, sg1.ID)
	}
	if ag1.OwnerUserID != "usr_admin1" {
		t.Fatalf("owner = %s, want usr_admin1", ag1.OwnerUserID)
	}
	if ag1.MyRole != "owner" || !ag1.CanManage {
		t.Fatalf("creator dto: %+v", ag1)
	}

	// admin2 is in TWO system groups -> ambiguous without an explicit parent.
	if _, err := e.svc.CreateGroup(e.ctx, admin2, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("ambiguous parent: got %v, want ErrGroupParentInvalid", err)
	}
	// ... picking one of THEIR OWN system groups (sg1) works.
	ag2 := e.mustCreateGroup(admin2, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg1.ID})
	if ag2.ParentGroupID != sg1.ID {
		t.Fatalf("explicit own parent: got %s", ag2.ParentGroupID)
	}

	// admin1 tries sg2 as parent, which they are NOT a member of -> invalid.
	if _, err := e.svc.CreateGroup(e.ctx, admin1, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG3", ParentGroupID: sg2.ID}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("foreign parent (non-member admin): got %v, want ErrGroupParentInvalid", err)
	}

	// A non-existent / non-system parent id is also rejected.
	if _, err := e.svc.CreateGroup(e.ctx, admin1, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG3b", ParentGroupID: "does-not-exist"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("unknown parent: got %v, want ErrGroupParentInvalid", err)
	}

	// system_admin may pick ANY system group as parent, even one they are
	// not personally a member of.
	ag4 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG4", ParentGroupID: sg2.ID})
	if ag4.ParentGroupID != sg2.ID {
		t.Fatalf("system_admin explicit any parent: got %s", ag4.ParentGroupID)
	}

	// Uniqueness is WITHIN the parent system group: "ag1" (case-insensitive)
	// under sg1 conflicts with the existing AG1...
	if _, err := e.svc.CreateGroup(e.ctx, admin2, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "ag1", ParentGroupID: sg1.ID}); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("dup admin group name under same parent: got %v, want ErrGroupNameConflict", err)
	}
	// ... but the SAME name under a DIFFERENT parent (sg2) is fine.
	if _, err := e.svc.CreateGroup(e.ctx, sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg2.ID}); err != nil {
		t.Fatalf("same name under different parent should be allowed: %v", err)
	}
}

func TestCreateAdminGroupForAnotherOwner(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_a", "admin") // the target owner
	e.createUser("usr_u", "user")  // ineligible (not an admin)
	// SG + put the target admin in it (so the parent resolves from the owner).
	mk := func(id, name, tier, parent, owner string) {
		g := store.UserGroup{ID: id, Tier: tier, Name: name, ParentGroupID: parent, OwnerUserID: owner, CreatedAt: e.now, UpdatedAt: e.now}
		if err := e.dir.CreateUserGroup(e.ctx, g); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ugrp_sg", "SG", store.GroupTierSystem, "", "")
	if err := e.dir.SetUserGroupMember(e.ctx, "ugrp_sg", "usr_a", store.GroupStateMember, ""); err != nil {
		t.Fatalf("member a->SG: %v", err)
	}
	sys := token("usr_s", "admin", "system")

	// system_admin creates an admin group FOR usr_a (owner), parent auto = SG.
	dto, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", OwnerUserID: "usr_a"})
	if err != nil {
		t.Fatalf("create-for-another: %v", err)
	}
	if dto.OwnerUserID != "usr_a" {
		t.Fatalf("owner = %q, want usr_a", dto.OwnerUserID)
	}
	if dto.ParentGroupID != "ugrp_sg" {
		t.Fatalf("parent = %q, want ugrp_sg", dto.ParentGroupID)
	}
	// usr_a is a member; the creator (usr_s) is NOT.
	members, _ := e.dir.UserGroupMembers(e.ctx, dto.ID)
	hasA, hasS := false, false
	for _, m := range members {
		if m.State == store.GroupStateMember && m.UserID == "usr_a" {
			hasA = true
		}
		if m.State == store.GroupStateMember && m.UserID == "usr_s" {
			hasS = true
		}
	}
	if !hasA || hasS {
		t.Fatalf("membership wrong: owner-member=%v creator-member=%v (want true,false)", hasA, hasS)
	}
}

func TestCreateAdminGroupForAnotherOwnerRejections(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_a", "admin") // in SG
	e.createUser("usr_b", "admin") // in NO system group
	e.createUser("usr_u", "user")
	mk := func(id, name, tier, parent string) {
		g := store.UserGroup{ID: id, Tier: tier, Name: name, ParentGroupID: parent, CreatedAt: e.now, UpdatedAt: e.now}
		if err := e.dir.CreateUserGroup(e.ctx, g); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ugrp_sg1", "SG1", store.GroupTierSystem, "")
	mk("ugrp_sg2", "SG2", store.GroupTierSystem, "")
	if err := e.dir.SetUserGroupMember(e.ctx, "ugrp_sg1", "usr_a", store.GroupStateMember, ""); err != nil {
		t.Fatalf("member: %v", err)
	}
	if err := e.dir.SetUserGroupMember(e.ctx, "ugrp_sg2", "usr_a", store.GroupStateMember, ""); err != nil {
		t.Fatalf("member: %v", err)
	}
	sys := token("usr_s", "admin", "system")
	adm := token("usr_a", "admin")

	// a regular ADMIN cannot assign a different owner.
	if _, err := e.svc.CreateGroup(e.ctx, adm, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "X", OwnerUserID: "usr_b"}); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("regular-admin owner = %v, want ErrGroupForbidden", err)
	}
	// owner is a non-admin user.
	if _, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "X", OwnerUserID: "usr_u"}); !errors.Is(err, ErrGroupOwnerInvalid) {
		t.Fatalf("non-admin owner = %v, want ErrGroupOwnerInvalid", err)
	}
	// owner in no system group.
	if _, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "X", OwnerUserID: "usr_b"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("owner-no-sysgroup = %v, want ErrGroupParentInvalid", err)
	}
	// owner in >1 system group, blank parent -> must pick.
	if _, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "X", OwnerUserID: "usr_a"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("owner-many-sysgroups blank parent = %v, want ErrGroupParentInvalid", err)
	}
	// explicit parent that the owner IS in -> ok.
	if _, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "X", OwnerUserID: "usr_a", ParentGroupID: "ugrp_sg1"}); err != nil {
		t.Fatalf("owner explicit valid parent: %v", err)
	}
	// explicit parent the owner is NOT in (fresh SG3) -> invalid.
	mk("ugrp_sg3", "SG3", store.GroupTierSystem, "")
	if _, err := e.svc.CreateGroup(e.ctx, sys, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "Y", OwnerUserID: "usr_a", ParentGroupID: "ugrp_sg3"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("owner explicit foreign parent = %v, want ErrGroupParentInvalid", err)
	}
}

func TestCreateGroup_UserTier_ParentAutoAndValidation(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_u1", "user")
	e.createUser("usr_u2", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	u1 := token("usr_u1")
	u2 := token("usr_u2")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// Admin-group membership requires parent-SYSTEM-group membership first
	// (containment, §5.2) — a plain user CAN be a system-group member
	// (membership is role-independent), so u1/u2 join SG before AG1/AG2.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_u1", "usr_u2")
	ag1 := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	ag2 := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2"})
	e.mustAddMembers(admin, ag1.ID, "usr_u1", "usr_u2")
	e.mustAddMembers(admin, ag2.ID, "usr_u1")

	// u2 is in exactly one admin group -> auto-selected parent.
	ug := e.mustCreateGroup(u2, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	if ug.ParentGroupID != ag1.ID {
		t.Fatalf("auto parent = %s, want %s", ug.ParentGroupID, ag1.ID)
	}
	if ug.OwnerUserID != "usr_u2" {
		t.Fatalf("owner = %s, want usr_u2", ug.OwnerUserID)
	}

	// u1 is in TWO admin groups -> ambiguous without an explicit parent.
	if _, err := e.svc.CreateGroup(e.ctx, u1, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG2"}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("ambiguous parent: got %v, want ErrGroupParentInvalid", err)
	}
	ug2 := e.mustCreateGroup(u1, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG2", ParentGroupID: ag2.ID})
	if ug2.ParentGroupID != ag2.ID {
		t.Fatalf("explicit own parent: got %s", ug2.ParentGroupID)
	}

	// u2 tries ag2 as parent, which they are NOT a member of -> invalid (no
	// system_admin-style override exists for user-tier creation, per §7.3).
	if _, err := e.svc.CreateGroup(e.ctx, u2, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG3", ParentGroupID: ag2.ID}); !errors.Is(err, ErrGroupParentInvalid) {
		t.Fatalf("foreign parent: got %v, want ErrGroupParentInvalid", err)
	}

	// Uniqueness is PER OWNER (not per parent): u1 already owns "UG2" ->
	// re-using the name (any parent, any case) conflicts...
	if _, err := e.svc.CreateGroup(e.ctx, u1, CreateGroupInput{Tier: store.GroupTierUser, Name: "ug2", ParentGroupID: ag1.ID}); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("dup name per owner: got %v, want ErrGroupNameConflict", err)
	}
	// ... but a DIFFERENT owner (u2) may use the exact same name.
	if _, err := e.svc.CreateGroup(e.ctx, u2, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG2", ParentGroupID: ag1.ID}); err != nil {
		t.Fatalf("different owner reusing a name should be allowed: %v", err)
	}
}

func TestGroupManage_NoLeakOwnerVsManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_outsider", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")
	outsider := token("usr_outsider")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})

	// An outsider cannot even discover the group exists.
	if _, err := e.svc.RenameGroup(e.ctx, outsider, ag.ID, "AG2"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("outsider rename: got %v, want ErrGroupNotFound", err)
	}
	if err := e.svc.DeleteGroup(e.ctx, outsider, ag.ID); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("outsider delete: got %v, want ErrGroupNotFound", err)
	}
	if err := e.svc.AddGroupMembers(e.ctx, outsider, ag.ID, []string{"usr_mgr"}); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("outsider add-members: got %v, want ErrGroupNotFound", err)
	}

	// Promote usr_mgr (must first be a member of AG, and — admin-tier rule —
	// an admin who is also a member of the parent system group).
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("promote: %v", err)
	}

	// A manager CAN rename...
	renamed, err := e.svc.RenameGroup(e.ctx, mgr, ag.ID, "AG-Renamed")
	if err != nil {
		t.Fatalf("manager rename: %v", err)
	}
	if renamed.Name != "AG-Renamed" {
		t.Fatalf("rename did not stick: %+v", renamed)
	}
	// ...but CANNOT delete (owner-only, per spec §8).
	if err := e.svc.DeleteGroup(e.ctx, mgr, ag.ID); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("manager delete: got %v, want ErrGroupForbidden", err)
	}
	// The owner CAN delete.
	if err := e.svc.DeleteGroup(e.ctx, owner, ag.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := e.dir.UserGroupByID(e.ctx, ag.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group should be gone: %v", err)
	}
}

func TestRenameGroup_UniquenessRecheck(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")

	sg1 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})

	if _, err := e.svc.RenameGroup(e.ctx, sysAdmin, sg2.ID, "sg1"); !errors.Is(err, ErrGroupNameConflict) {
		t.Fatalf("rename to an existing (case-insensitive) name: got %v, want ErrGroupNameConflict", err)
	}
	// Renaming a group to its OWN current name (even a different case) is a
	// no-op success — the uniqueness check excludes the group itself.
	if _, err := e.svc.RenameGroup(e.ctx, sysAdmin, sg1.ID, "SG1"); err != nil {
		t.Fatalf("self rename: %v", err)
	}
}

// --- Task 7: membership add/remove + containment + cascade + candidates ----

func TestAddGroupMembers_ContainmentAdminTier(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_outside_sg", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})

	// usr_outside_sg is not (yet) a member of the parent system group.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ag.ID, []string{"usr_outside_sg"}); !errors.Is(err, ErrGroupMemberNotVisible) {
		t.Fatalf("containment violation: got %v, want ErrGroupMemberNotVisible", err)
	}
	// The AG membership must NOT have been partially applied.
	members, err := e.dir.UserGroupMembers(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == "usr_outside_sg" {
			t.Fatalf("containment-rejected user must not be added: %+v", members)
		}
	}

	// Once they join SG, the same add succeeds.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_outside_sg")
	if err := e.svc.AddGroupMembers(e.ctx, owner, ag.ID, []string{"usr_outside_sg"}); err != nil {
		t.Fatalf("add after joining parent: %v", err)
	}
}

func TestAddGroupMembers_ContainmentSystemTier(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_u", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})

	// Only a system_admin can even reach AddGroupMembers on a system group
	// (authorizeGroupManage gates it) — an admin gets the no-leak 404.
	if err := e.svc.AddGroupMembers(e.ctx, admin, sg.ID, []string{"usr_u"}); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("admin add to system group: got %v, want ErrGroupNotFound", err)
	}
	// A system_admin sees everyone (VisibleUserIDs = all) -> any user id is
	// addable.
	if err := e.svc.AddGroupMembers(e.ctx, sysAdmin, sg.ID, []string{"usr_u"}); err != nil {
		t.Fatalf("system_admin add: %v", err)
	}
}

func TestGroupMemberCandidates_AdminTier(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_in_sg_a", "user")
	e.createUser("usr_in_sg_b", "user")
	e.createUser("usr_not_in_sg", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_in_sg_a", "usr_in_sg_b")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_in_sg_a")

	cands, err := e.svc.GroupMemberCandidates(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	ids := map[string]bool{}
	for _, c := range cands {
		ids[c.ID] = true
	}
	if !ids["usr_in_sg_b"] {
		t.Fatalf("usr_in_sg_b (visible, in parent, not yet a member) should be a candidate: %+v", cands)
	}
	if ids["usr_in_sg_a"] {
		t.Fatalf("usr_in_sg_a is already a member and must not be a candidate: %+v", cands)
	}
	if ids["usr_not_in_sg"] {
		t.Fatalf("usr_not_in_sg fails containment and must not be a candidate: %+v", cands)
	}
	if ids["usr_owner"] {
		t.Fatalf("the owner (already an implicit member) must not be a candidate: %+v", cands)
	}
}

// TestRemoveMemberCascade_ThreeLevel builds SG -> AG(child of SG) ->
// UG(grandchild of AG, via AG's admin owner) with a single user, U, present
// as a member (and manager) at every level, then asserts that removing U
// from SG also removes U's member+manager rows from AG and UG.
func TestRemoveMemberCascade_ThreeLevel(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	// usr_u is role "admin" (not "user") so it is ELIGIBLE for admin-tier
	// manager promotion below (§8: an admin-group manager must be an admin);
	// it separately exercises the user-tier group as an ordinary creator.
	e.createUser("usr_u", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	u := token("usr_u", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_u")

	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_u")
	// Promote U to AG co-manager so the manager-row cascade is exercised too.
	if err := e.svc.PromoteManager(e.ctx, admin, ag.ID, "usr_u", true, true, true, true, true); err != nil {
		t.Fatalf("promote usr_u in AG: %v", err)
	}

	ug := e.mustCreateGroup(u, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	if ug.ParentGroupID != ag.ID {
		t.Fatalf("UG parent = %s, want %s", ug.ParentGroupID, ag.ID)
	}
	// U owns UG (and is thus an implicit member of it via CreateGroup).

	// Sanity: U is present everywhere before the cascade.
	for _, gid := range []string{sg.ID, ag.ID, ug.ID} {
		mem, err := e.dir.UserGroupMembers(e.ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, m := range mem {
			if m.UserID == "usr_u" && m.State == store.GroupStateMember {
				found = true
			}
		}
		if !found {
			t.Fatalf("precondition: usr_u should be a member of %s before cascade", gid)
		}
	}
	agManagers, err := e.dir.UserGroupManagers(e.ctx, ag.ID)
	if err != nil || !containsStr(agManagers, "usr_u") {
		t.Fatalf("precondition: usr_u should manage AG before cascade: %v %v", agManagers, err)
	}

	// Remove U from SG (system_admin-authorized) -> full cascade.
	if err := e.svc.RemoveGroupMember(e.ctx, sysAdmin, sg.ID, "usr_u"); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}

	for _, gid := range []string{sg.ID, ag.ID, ug.ID} {
		mem, err := e.dir.UserGroupMembers(e.ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range mem {
			if m.UserID == "usr_u" {
				t.Fatalf("usr_u should be gone from %s after cascade, found state=%s", gid, m.State)
			}
		}
	}
	agManagers, err = e.dir.UserGroupManagers(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(agManagers, "usr_u") {
		t.Fatalf("usr_u should be removed as AG manager after cascade: %v", agManagers)
	}

	// UG's owner (U) just left its own ancestry chain -> succession must
	// have run and reassigned/deleted UG rather than leaving a dangling
	// owner reference.
	ugAfter, err := e.dir.UserGroupByID(e.ctx, ug.ID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unexpected error reloading UG: %v", err)
		}
		// Deleted is an acceptable outcome (no other member/manager to
		// succeed to) — U was UG's only occupant.
	} else if ugAfter.OwnerUserID == "usr_u" {
		t.Fatalf("UG should not still be owned by the departed usr_u: %+v", ugAfter)
	}
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestRemoveGroupMember_SelfLeave(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin")

	// usr_admin is a plain member of SG (not owner/manager/system_admin) and
	// could never pass authorizeGroupManage, but MAY remove themselves.
	if err := e.svc.RemoveGroupMember(e.ctx, admin, sg.ID, "usr_admin"); err != nil {
		t.Fatalf("self-leave: %v", err)
	}
	visible, err := e.svc.memberIDSet(e.ctx, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if visible["usr_admin"] {
		t.Fatalf("usr_admin should have left SG: %v", visible)
	}
}

// --- Task 8: managers, ownership transfer, succession ----------------------

func TestPromoteManager_Eligibility(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_admin_member", "admin")
	e.createUser("usr_admin_outside_sg", "admin")
	e.createUser("usr_plain_member", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// usr_admin_member + usr_plain_member both join SG first (containment —
	// SG membership is role-independent, so a plain user CAN be an SG
	// member) so their AG membership below is added via the normal,
	// contained path. usr_admin_outside_sg deliberately does NOT join SG.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_admin_member", "usr_plain_member")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_admin_member", "usr_plain_member")
	// usr_admin_outside_sg is fixtured as an AG member DIRECTLY via the
	// store, bypassing AddGroupMembers' containment check — this state is
	// UNREACHABLE via the real API (removeMemberCascade guarantees an AG
	// member always stays an SG member), but PromoteManager carries its own
	// independent parent-SG check as defense-in-depth, and this proves it.
	if err := e.dir.SetUserGroupMember(e.ctx, ag.ID, "usr_admin_outside_sg", store.GroupStateMember, ""); err != nil {
		t.Fatalf("fixture usr_admin_outside_sg into AG: %v", err)
	}

	// Non-member -> ineligible.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_sysadmin", true, true, true, true, true); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("promote non-member: got %v, want ErrGroupCandidateInvalid", err)
	}
	// Member but a plain user (not admin) -> ineligible for an ADMIN-tier
	// group's manager seat.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_plain_member", true, true, true, true, true); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("promote plain-user member: got %v, want ErrGroupCandidateInvalid", err)
	}
	// Member, admin role, but NOT a member of the parent system group ->
	// ineligible.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_admin_outside_sg", true, true, true, true, true); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("promote admin outside parent SG: got %v, want ErrGroupCandidateInvalid", err)
	}
	// Member, admin role, AND a member of the parent system group -> ok.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_admin_member", true, true, true, true, true); err != nil {
		t.Fatalf("promote eligible admin: %v", err)
	}
	managers, err := e.dir.UserGroupManagers(e.ctx, ag.ID)
	if err != nil || !containsStr(managers, "usr_admin_member") {
		t.Fatalf("usr_admin_member should now manage AG: %v %v", managers, err)
	}
}

func TestPromoteManager_UserTierRequiresMembership(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	e.createUser("usr_stranger", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	owner := token("usr_owner")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// AG membership requires SG (parent) membership first — role-independent.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_owner", "usr_member", "usr_stranger")
	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_owner", "usr_member", "usr_stranger")

	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	// usr_stranger is never invited/added to UG.
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_stranger", true, true, true, true, true); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("promote non-member of user group: got %v, want ErrGroupCandidateInvalid", err)
	}
	e.mustAddMembers(owner, ug.ID, "usr_member")
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_member", true, true, true, true, true); err != nil {
		t.Fatalf("promote member: %v", err)
	}
}

func TestManagerActions_OwnerOnly(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_candidate", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr", "usr_candidate")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr", "usr_candidate")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}

	// A co-manager may not promote/demote/transfer.
	if err := e.svc.PromoteManager(e.ctx, mgr, ag.ID, "usr_candidate", true, true, true, true, true); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("manager promote: got %v, want ErrGroupForbidden", err)
	}
	if err := e.svc.DemoteManager(e.ctx, mgr, ag.ID, "usr_mgr"); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("manager demote: got %v, want ErrGroupForbidden", err)
	}
	if err := e.svc.TransferOwnership(e.ctx, mgr, ag.ID, "usr_mgr"); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("manager transfer: got %v, want ErrGroupForbidden", err)
	}

	// The owner may do all three.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_candidate", true, true, true, true, true); err != nil {
		t.Fatalf("owner promote: %v", err)
	}
	if err := e.svc.DemoteManager(e.ctx, owner, ag.ID, "usr_candidate"); err != nil {
		t.Fatalf("owner demote: %v", err)
	}
}

func TestTransferOwnership_MustBeCurrentManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_plain_member", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr", "usr_plain_member")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr", "usr_plain_member")

	// Not (yet) a manager -> rejected.
	if err := e.svc.TransferOwnership(e.ctx, owner, ag.ID, "usr_plain_member"); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("transfer to non-manager: got %v, want ErrGroupCandidateInvalid", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TransferOwnership(e.ctx, owner, ag.ID, "usr_mgr"); err != nil {
		t.Fatalf("transfer to a current manager: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "usr_mgr" {
		t.Fatalf("owner after transfer = %s, want usr_mgr", after.OwnerUserID)
	}
	// The old owner (usr_owner) is untouched in membership; the new owner
	// (usr_mgr) can now delete.
	if err := e.svc.DeleteGroup(e.ctx, token("usr_mgr", "admin"), ag.ID); err != nil {
		t.Fatalf("new owner delete: %v", err)
	}
}

// TestSuccession_UserTier covers §8.1's user-tier succession chain:
// oldest manager -> oldest member -> delete when nobody is left.
func TestSuccession_UserTier(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_owner", "user")
	e.createUser("usr_mgr_a", "user")
	e.createUser("usr_mgr_b", "user")
	e.createUser("usr_member_a", "user")
	e.createUser("usr_member_b", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	owner := token("usr_owner")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// AG membership requires SG (parent) membership first — role-independent.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_owner", "usr_mgr_a", "usr_mgr_b", "usr_member_a", "usr_member_b")
	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_owner", "usr_mgr_a", "usr_mgr_b", "usr_member_a", "usr_member_b")

	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	e.mustAddMembers(owner, ug.ID, "usr_mgr_a", "usr_mgr_b", "usr_member_a", "usr_member_b")
	// Promote in "oldest first" order so store insertion order == the
	// intended succession order (works around the memory store's manager
	// listing not tracking created_at order the way the SQL store does —
	// see the Task 6 report's "known concerns").
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_mgr_a", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_mgr_b", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}

	// Owner leaves -> oldest manager (usr_mgr_a) becomes owner.
	if err := e.svc.RemoveGroupMember(e.ctx, owner, ug.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "usr_mgr_a" {
		t.Fatalf("successor = %s, want usr_mgr_a (oldest manager)", after.OwnerUserID)
	}

	// New owner (usr_mgr_a) leaves too, with usr_mgr_b still a manager ->
	// usr_mgr_b succeeds.
	if err := e.svc.RemoveGroupMember(e.ctx, token("usr_mgr_a"), ug.ID, "usr_mgr_a"); err != nil {
		t.Fatalf("mgr_a self-leave: %v", err)
	}
	after, err = e.dir.UserGroupByID(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "usr_mgr_b" {
		t.Fatalf("successor = %s, want usr_mgr_b (remaining manager)", after.OwnerUserID)
	}

	// usr_mgr_b (owner, no longer any OTHER manager) leaves -> falls to the
	// oldest remaining plain member.
	if err := e.svc.RemoveGroupMember(e.ctx, token("usr_mgr_b"), ug.ID, "usr_mgr_b"); err != nil {
		t.Fatalf("mgr_b self-leave: %v", err)
	}
	after, err = e.dir.UserGroupByID(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "usr_member_a" && after.OwnerUserID != "usr_member_b" {
		t.Fatalf("successor = %s, want one of the remaining plain members", after.OwnerUserID)
	}
	lastMember := after.OwnerUserID
	other := "usr_member_a"
	if lastMember == other {
		other = "usr_member_b"
	}

	// Remove the other plain member first (not the owner) — group survives.
	// A plain admin has no authority over a user-tier group they neither own
	// nor manage, so this must be done by system_admin (or the group's own
	// current owner).
	if err := e.svc.RemoveGroupMember(e.ctx, sysAdmin, ug.ID, other); err != nil {
		t.Fatalf("remove other member: %v", err)
	}
	if _, err := e.dir.UserGroupByID(e.ctx, ug.ID); err != nil {
		t.Fatalf("group should still exist: %v", err)
	}

	// Now the owner (the sole remaining occupant) leaves -> nobody left ->
	// the group is deleted.
	if err := e.svc.RemoveGroupMember(e.ctx, token(lastMember), ug.ID, lastMember); err != nil {
		t.Fatalf("last member self-leave: %v", err)
	}
	if _, err := e.dir.UserGroupByID(e.ctx, ug.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group should be deleted once empty: %v", err)
	}
}

// TestSuccession_AdminTier_ParentSystemGroupFallback covers §8.1's THIRD
// admin-tier fallback: no manager, no eligible admin/system_admin AG member
// -> an admin/system_admin who is a member of AG's PARENT system group
// (usr_parent_admin here is deliberately never an AG member at all — only
// reachable via SG) — and never a plain user (usr_plain, AG's only other
// real member).
func TestSuccession_AdminTier_ParentSystemGroupFallback(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_plain", "user")
	e.createUser("usr_parent_admin", "admin") // in SG, never an AG member
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// AG membership requires SG membership first (role-independent), so
	// usr_plain also joins SG even though it will never be eligible for
	// admin-group ownership.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_parent_admin", "usr_plain")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	// AG's only OTHER (real) member is a plain user -> must never be picked.
	e.mustAddMembers(owner, ag.ID, "usr_plain")

	// Owner leaves; no manager, no admin/system_admin AG member -> falls
	// through to an admin/system_admin member of the PARENT system group
	// (usr_parent_admin), never the plain user.
	if err := e.svc.RemoveGroupMember(e.ctx, owner, ag.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "usr_parent_admin" {
		t.Fatalf("successor = %s, want usr_parent_admin (parent-SG admin); must never be usr_plain", after.OwnerUserID)
	}
}

// TestSuccession_AdminTier_OwnerNullThenDelete covers §8.1's terminal admin-
// tier case: when truly nobody admin-capable remains reachable (not even via
// the parent system group — the only OTHER SG member is the departing owner
// themselves, correctly excluded), owner falls to NULL and the group is KEPT
// as long as it still has a member; only once it is completely empty is it
// deleted.
func TestSuccession_AdminTier_OwnerNullThenDelete(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_plain", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	// usr_plain also joins SG (role-independent membership) so it can
	// legitimately become an AG member too.
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_plain")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_plain")

	// Owner leaves: no manager, no admin/system_admin AG member (usr_plain
	// is a plain user), and the only OTHER SG member (usr_owner itself) is
	// the departing owner and must be excluded, not reselected -> owner
	// falls to NULL. AG survives because it still has a member (usr_plain).
	if err := e.svc.RemoveGroupMember(e.ctx, owner, ag.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID != "" {
		t.Fatalf("owner should be NULL (no eligible admin left, never a plain user), got %s", after.OwnerUserID)
	}

	// Removing the last member (usr_plain) empties AG completely -> deleted.
	if err := e.svc.RemoveGroupMember(e.ctx, sysAdmin, ag.ID, "usr_plain"); err != nil {
		t.Fatalf("remove last member: %v", err)
	}
	if _, err := e.dir.UserGroupByID(e.ctx, ag.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("group should be deleted once truly empty: %v", err)
	}
}

// TestSuccession_AdminTier_SkipsDisabledManager covers the final-review fix:
// a DISABLED account must never be selected as a successor, even when it is
// otherwise the "oldest manager" (the first candidate class). usr_mgr is
// promoted to co-manager of AG while active, then disabled; when the owner
// leaves, succession must skip usr_mgr entirely (it is not merely
// deprioritized — it is ineligible) and fall through to the next candidate
// class (an active admin/system_admin AG member), never selecting the
// disabled user.
func TestSuccession_AdminTier_SkipsDisabledManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_member_admin", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr", "usr_member_admin")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr", "usr_member_admin")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}

	// Disable usr_mgr AFTER promotion (a manager promoted while active, later
	// disabled, must keep its stale manager row but become ineligible).
	e.setUserStatus("usr_mgr", store.UserStatusDisabled)

	if err := e.svc.RemoveGroupMember(e.ctx, owner, ag.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID == "usr_mgr" {
		t.Fatalf("successor must never be a disabled account, got %s", after.OwnerUserID)
	}
	if after.OwnerUserID != "usr_member_admin" {
		t.Fatalf("successor = %s, want usr_member_admin (the next eligible class, since the manager is disabled)", after.OwnerUserID)
	}
}

// TestSuccession_AdminTier_SkipsDowngradedManager covers the final-review fix:
// a manager who was an admin AT PROMOTION time but is later downgraded to a
// plain "user" role must never become an admin-group owner (spec §8.1 — a
// plain user can never own an admin group). usr_mgr is promoted while admin,
// then downgraded to role "user"; succession must skip it (both as a manager
// AND as a plain member candidate) and fall all the way through to the
// parent system group's admin-role fallback.
func TestSuccession_AdminTier_SkipsDowngradedManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_parent_admin", "admin") // in SG, never an AG member
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr", "usr_parent_admin")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}

	// Downgrade usr_mgr AFTER promotion: still active, but no longer an admin
	// role — the exact "promoted while admin, later demoted" scenario.
	e.setUserRole("usr_mgr", "user")

	if err := e.svc.RemoveGroupMember(e.ctx, owner, ag.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID == "usr_mgr" {
		t.Fatalf("a downgraded (plain-user) manager must never become owner, got %s", after.OwnerUserID)
	}
	if after.OwnerUserID != "usr_parent_admin" {
		t.Fatalf("successor = %s, want usr_parent_admin (parent-SG admin fallback, since the manager no longer qualifies)", after.OwnerUserID)
	}
}

// TestSuccession_UserTier_SkipsDisabledManager mirrors the admin-tier fix for
// the user tier: a DISABLED manager must never be selected as a user-group's
// successor, even though a user-tier group has no role restriction otherwise
// (any active user is eligible).
func TestSuccession_UserTier_SkipsDisabledManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_owner", "user")
	e.createUser("usr_mgr", "user")
	e.createUser("usr_member", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	owner := token("usr_owner")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_owner", "usr_mgr", "usr_member")
	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_owner", "usr_mgr", "usr_member")

	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	e.mustAddMembers(owner, ug.ID, "usr_mgr", "usr_member")
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}
	e.setUserStatus("usr_mgr", store.UserStatusDisabled)

	if err := e.svc.RemoveGroupMember(e.ctx, owner, ug.ID, "usr_owner"); err != nil {
		t.Fatalf("owner self-leave: %v", err)
	}
	after, err := e.dir.UserGroupByID(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.OwnerUserID == "usr_mgr" {
		t.Fatalf("successor must never be a disabled account, got %s", after.OwnerUserID)
	}
	if after.OwnerUserID != "usr_member" {
		t.Fatalf("successor = %s, want usr_member (the disabled manager must be skipped)", after.OwnerUserID)
	}
}

// TestRemoveMemberCascade_OwnerSuccessionAtEveryLevel proves the fix
// described in removeMemberCascade's doc comment: removing a user from an
// ANCESTOR group must trigger owner succession for every DESCENDANT group
// that user happens to own too, not just the top-level group the removal was
// requested on. usr_child_owner owns a leaf user-group (UG) nested under an
// admin group (AG) nested under a system group (SG); removing them from SG
// cascades their membership out of AG and UG, and since they were UG's
// owner, UG's succession must run (here: nobody left in UG -> deleted).
func TestRemoveMemberCascade_OwnerSuccessionAtEveryLevel(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_child_owner", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	childOwner := token("usr_child_owner")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_child_owner")
	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_child_owner")
	ug := e.mustCreateGroup(childOwner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	if ug.OwnerUserID != "usr_child_owner" {
		t.Fatalf("precondition: usr_child_owner should own UG: %+v", ug)
	}

	// Remove usr_child_owner from SG (top of the tree) -> cascades through
	// AG into UG, where they are ALSO the owner with nobody else present.
	if err := e.svc.RemoveGroupMember(e.ctx, sysAdmin, sg.ID, "usr_child_owner"); err != nil {
		t.Fatalf("RemoveGroupMember: %v", err)
	}
	if _, err := e.dir.UserGroupByID(e.ctx, ug.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("UG should have been deleted by cascaded owner succession, got: %v", err)
	}
}

func TestReassignGroupsOwnedBy(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2"})
	e.mustAddMembers(owner, ag1.ID, "usr_mgr")
	e.mustAddMembers(owner, ag2.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag1.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag2.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatal(err)
	}

	// Simulate a disable: the owner's OWN membership rows are left in place
	// (disable != removal), only ownership is reassigned across every group
	// they own.
	if err := e.svc.ReassignGroupsOwnedBy(e.ctx, adminToken(), "usr_owner"); err != nil {
		t.Fatalf("ReassignGroupsOwnedBy: %v", err)
	}
	for _, gid := range []string{ag1.ID, ag2.ID} {
		g, err := e.dir.UserGroupByID(e.ctx, gid)
		if err != nil {
			t.Fatal(err)
		}
		if g.OwnerUserID != "usr_mgr" {
			t.Fatalf("group %s owner after reassign = %s, want usr_mgr", gid, g.OwnerUserID)
		}
	}
	// usr_owner's own membership row in SG/AG1/AG2 is untouched (disable is
	// not a membership removal) — sanity-check via SG (still a member).
	sgMembers, err := e.svc.memberIDSet(e.ctx, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sgMembers["usr_owner"] {
		t.Fatalf("disable must not remove SG membership: %v", sgMembers)
	}
}

// --- Task 10: user-group invitations (invite/accept/decline) ---------------

// TestAddGroupMembers_UserTierRoutesToInvite proves the carried hazard is
// closed: AddGroupMembers on a USER-tier group never grants direct
// membership (state=member) — it invites (state=invited, invited_by=
// principal) — and containment is membership of the PARENT ADMIN group,
// never the generic VisibleUserIDs fallback. A user who is visible to the
// owner (they share ANOTHER admin group) but is NOT a member of THIS group's
// parent admin group must be rejected with ErrGroupNotParentMember, and must
// NOT have been added in any state (whole-batch validated, matching the
// direct path's all-or-nothing semantics).
func TestAddGroupMembers_UserTierRoutesToInvite(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_parent_member", "user")
	e.createUser("usr_sibling_member", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_parent_member", "usr_sibling_member")

	// Two SIBLING admin groups, both owned by usr_owner (so VisibleUserIDs
	// for usr_owner spans both — the exact hazard). ag1 is UG's real parent;
	// ag2 is the sibling usr_sibling_member belongs to instead.
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2"})
	e.mustAddMembers(owner, ag1.ID, "usr_parent_member")
	e.mustAddMembers(owner, ag2.ID, "usr_sibling_member")

	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag1.ID})
	if ug.ParentGroupID != ag1.ID {
		t.Fatalf("UG parent = %s, want %s", ug.ParentGroupID, ag1.ID)
	}

	// usr_sibling_member is VISIBLE to usr_owner (member of AG2, which
	// usr_owner also owns) but is NOT a member of UG's parent (AG1) -> must
	// be rejected, never the VisibleUserIDs fallback.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_sibling_member"}); !errors.Is(err, ErrGroupNotParentMember) {
		t.Fatalf("invite non-parent-member: got %v, want ErrGroupNotParentMember", err)
	}
	members, err := e.dir.UserGroupMembers(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == "usr_sibling_member" {
			t.Fatalf("containment-rejected invitee must not be added in any state: %+v", m)
		}
	}

	// usr_parent_member IS a member of UG's parent (AG1) -> invite succeeds,
	// landing as state=invited (NOT member) with invited_by=usr_owner.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_parent_member"}); err != nil {
		t.Fatalf("invite parent-member: %v", err)
	}
	members, err = e.dir.UserGroupMembers(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range members {
		if m.UserID != "usr_parent_member" {
			continue
		}
		found = true
		if m.State != store.GroupStateInvited {
			t.Fatalf("invited user's state = %q, want %q (never granted direct membership)", m.State, store.GroupStateInvited)
		}
		if m.InvitedBy != "usr_owner" {
			t.Fatalf("invited_by = %q, want usr_owner", m.InvitedBy)
		}
	}
	if !found {
		t.Fatalf("usr_parent_member should have a pending invitation row: %+v", members)
	}
	// A user-tier "member" query (state=member only) must NOT count them yet.
	memberSet, err := e.svc.memberIDSet(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if memberSet["usr_parent_member"] {
		t.Fatalf("an invited-but-not-accepted user must not count as a member: %v", memberSet)
	}
}

// TestAddGroupMembers_UserTierReinviteDoesNotDemoteMember is the regression
// for a review-caught bug: inviteGroupMembers used to upsert state=invited
// UNCONDITIONALLY, so re-naming an already-ACCEPTED member in a later
// AddGroupMembers call (e.g. a raw API retry, or re-adding the group's own
// owner — auto-enrolled as a member at creation) silently DEMOTED them
// member->invited, dropping them out of memberIDSet/visibility until they
// happened to re-accept. A re-invite of an existing member must be a no-op
// on their state.
func TestAddGroupMembers_UserTierReinviteDoesNotDemoteMember(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_a", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_a")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_a")
	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag.ID})

	// Invite usr_a, then accept -> a real member.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_a"}); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := e.svc.RespondInvitation(e.ctx, token("usr_a"), ug.ID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	members, err := e.svc.memberIDSet(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !members["usr_a"] {
		t.Fatalf("precondition: usr_a should be a member after accept: %v", members)
	}

	// Re-invite (name usr_a in AddGroupMembers again) — must NOT demote them
	// back to invited.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_a"}); err != nil {
		t.Fatalf("re-invite: %v", err)
	}
	rows, err := e.dir.UserGroupMembers(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range rows {
		if m.UserID == "usr_a" && m.State != store.GroupStateMember {
			t.Fatalf("usr_a must stay state=member across a re-invite, got state=%q", m.State)
		}
	}
	members, err = e.svc.memberIDSet(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !members["usr_a"] {
		t.Fatalf("usr_a must remain a member (visible/countable) after re-invite: %v", members)
	}
}

// TestListInvitations_OnlyTheInviteeSeesTheirOwnPending proves ListInvitations
// returns only the calling principal's own pending invitations (never a
// bystander's), each carrying the group name and inviter, and drops the row
// as soon as it is no longer state=invited.
func TestListInvitations_OnlyTheInviteeSeesTheirOwnPending(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_invitee", "user")
	e.createUser("usr_bystander", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_invitee")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_invitee")
	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag.ID})

	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_invitee"}); err != nil {
		t.Fatalf("invite: %v", err)
	}

	invitee := token("usr_invitee")
	got, err := e.svc.ListInvitations(e.ctx, invitee)
	if err != nil {
		t.Fatalf("ListInvitations(invitee): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListInvitations(invitee) = %+v, want exactly 1", got)
	}
	if got[0].GroupID != ug.ID || got[0].GroupName != "UG" || got[0].ParentGroupID != ag.ID || got[0].InvitedBy != "usr_owner" {
		t.Fatalf("unexpected invitation dto: %+v", got[0])
	}

	// A bystander (never invited anywhere) sees nothing.
	bystander := token("usr_bystander")
	got, err = e.svc.ListInvitations(e.ctx, bystander)
	if err != nil {
		t.Fatalf("ListInvitations(bystander): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ListInvitations(bystander) = %+v, want empty", got)
	}

	// Once accepted, it drops out of the pending list.
	if err := e.svc.RespondInvitation(e.ctx, invitee, ug.ID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	got, err = e.svc.ListInvitations(e.ctx, invitee)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ListInvitations(invitee) after accept = %+v, want empty", got)
	}
}

// TestRespondInvitation_AcceptDeclineAndInviteeOnly covers accept -> member,
// decline -> row removed, and that only the invitee themselves (never a
// bystander, and never the group owner acting on someone else's behalf) may
// respond.
func TestRespondInvitation_AcceptDeclineAndInviteeOnly(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_accepter", "user")
	e.createUser("usr_decliner", "user")
	e.createUser("usr_bystander", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_accepter", "usr_decliner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_accepter", "usr_decliner")
	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag.ID})

	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_accepter", "usr_decliner"}); err != nil {
		t.Fatalf("invite both: %v", err)
	}

	// A bystander (no invitation at all) can't respond.
	if err := e.svc.RespondInvitation(e.ctx, token("usr_bystander"), ug.ID, true); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("bystander respond: got %v, want ErrGroupNotFound", err)
	}
	// The group's OWNER cannot respond on someone else's behalf either — this
	// is exclusively the invitee's own action, not an owner/manager action.
	if err := e.svc.RespondInvitation(e.ctx, owner, ug.ID, true); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("owner responds for someone else: got %v, want ErrGroupNotFound", err)
	}

	// Accept -> state=member.
	if err := e.svc.RespondInvitation(e.ctx, token("usr_accepter"), ug.ID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	memberSet, err := e.svc.memberIDSet(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !memberSet["usr_accepter"] {
		t.Fatalf("accepter should now be a real member: %v", memberSet)
	}
	// Having accepted, a second accept-or-decline call finds no pending
	// invitation left (ErrGroupNotFound, same no-leak shape).
	if err := e.svc.RespondInvitation(e.ctx, token("usr_accepter"), ug.ID, false); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("re-respond after accept: got %v, want ErrGroupNotFound", err)
	}

	// Decline -> row removed entirely (not merely left invited).
	if err := e.svc.RespondInvitation(e.ctx, token("usr_decliner"), ug.ID, false); err != nil {
		t.Fatalf("decline: %v", err)
	}
	members, err := e.dir.UserGroupMembers(e.ctx, ug.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.UserID == "usr_decliner" {
			t.Fatalf("declined invitee should have no row at all: %+v", m)
		}
	}
}

// TestAddGroupMembers_SystemAdminTierUnchanged is a regression proving the
// system/admin-tier direct path is byte-identical to Task 7: an add still
// lands as state=member immediately, with no invited_by and no pending
// invitation to respond to.
func TestAddGroupMembers_SystemAdminTierUnchanged(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_direct", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	if err := e.svc.AddGroupMembers(e.ctx, sysAdmin, sg.ID, []string{"usr_owner", "usr_direct"}); err != nil {
		t.Fatalf("system-tier direct add: %v", err)
	}
	members, err := e.dir.UserGroupMembers(e.ctx, sg.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range members {
		if m.State != store.GroupStateMember || m.InvitedBy != "" {
			t.Fatalf("system-tier add must land as a direct member, no invited_by: %+v", m)
		}
	}

	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	if err := e.svc.AddGroupMembers(e.ctx, owner, ag.ID, []string{"usr_direct"}); err != nil {
		t.Fatalf("admin-tier direct add: %v", err)
	}
	members, err = e.dir.UserGroupMembers(e.ctx, ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range members {
		if m.UserID != "usr_direct" {
			continue
		}
		found = true
		if m.State != store.GroupStateMember || m.InvitedBy != "" {
			t.Fatalf("admin-tier add must land as a direct member, no invited_by: %+v", m)
		}
	}
	if !found {
		t.Fatalf("usr_direct should be an AG member: %+v", members)
	}
	// No pending invitation was ever created.
	invitations, err := e.svc.ListInvitations(e.ctx, token("usr_direct"))
	if err != nil {
		t.Fatal(err)
	}
	if len(invitations) != 0 {
		t.Fatalf("direct add must not create a pending invitation: %+v", invitations)
	}
}

// --- Task 12c: ListGroups exposes an admin's member-of system groups --------

// TestListGroups_SystemSection_AdminSeesOnlyMemberOfSystemGroups covers spec
// §7.2: a plain admin sees, as parent-options, ONLY the system groups they are
// a member of (read-only — can_manage=false); a plain user sees none; a
// system_admin still sees the full system list (unchanged, own can_manage=true).
func TestListGroups_SystemSection_AdminSeesOnlyMemberOfSystemGroups(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_user", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	user := token("usr_user")

	sg1 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	e.mustAddMembers(sysAdmin, sg1.ID, "usr_admin")

	// A plain admin, member of SG1 only, sees ONLY SG1 in the System section,
	// with can_manage=false (read-only parent-option).
	landscape, err := e.svc.ListGroups(e.ctx, admin)
	if err != nil {
		t.Fatalf("ListGroups(admin): %v", err)
	}
	if len(landscape.System) != 1 || landscape.System[0].ID != sg1.ID {
		t.Fatalf("ListGroups(admin).System = %+v, want only %s", landscape.System, sg1.ID)
	}
	if landscape.System[0].CanManage {
		t.Fatalf("ListGroups(admin).System[0].CanManage = true, want false (non-manager)")
	}

	// A plain user (no admin scope) must not see any system group, even if
	// (hypothetically) added as a member — users must not see system groups.
	if err := e.dir.SetUserGroupMember(e.ctx, sg1.ID, "usr_user", store.GroupStateMember, ""); err != nil {
		t.Fatalf("seed usr_user into SG1: %v", err)
	}
	landscape, err = e.svc.ListGroups(e.ctx, user)
	if err != nil {
		t.Fatalf("ListGroups(user): %v", err)
	}
	if len(landscape.System) != 0 {
		t.Fatalf("ListGroups(user).System = %+v, want empty", landscape.System)
	}

	// A system_admin still sees the FULL system list (SG1 + SG2), unchanged,
	// with can_manage=true for both (every system_admin manages every system
	// group, per groupDTO's system-tier branch).
	landscape, err = e.svc.ListGroups(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ListGroups(sysAdmin): %v", err)
	}
	if len(landscape.System) != 2 {
		t.Fatalf("ListGroups(sysAdmin).System = %+v, want SG1+SG2", landscape.System)
	}
	for _, g := range landscape.System {
		if !g.CanManage {
			t.Fatalf("ListGroups(sysAdmin).System %+v: CanManage = false, want true", g)
		}
	}
	seen := map[string]bool{}
	for _, g := range landscape.System {
		seen[g.ID] = true
	}
	if !seen[sg1.ID] || !seen[sg2.ID] {
		t.Fatalf("ListGroups(sysAdmin).System missing sg1/sg2: %+v", landscape.System)
	}

	// The Admin/User sections are unaffected by this change: the admin's own
	// admin-tier group (owned by them, parented under SG1) still surfaces
	// normally in the Admin section.
	ag1 := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	landscape, err = e.svc.ListGroups(e.ctx, admin)
	if err != nil {
		t.Fatalf("ListGroups(admin) after AG1: %v", err)
	}
	if len(landscape.Admin) != 1 || landscape.Admin[0].ID != ag1.ID {
		t.Fatalf("ListGroups(admin).Admin = %+v, want only %s", landscape.Admin, ag1.ID)
	}
}

// TestListGroups_AdminSeesMemberAdminGroupsAndParentLinkedUserGroups verifies
// the tier-aware visibility rules (spec 2026-08-09):
//   - a plain admin sees only the Admin-Groups they belong to, and User-Groups
//     they own/manage/are-a-member-of PLUS every User-Group under an
//     Admin-Group they belong to (read-only for the latter);
//   - a system_admin sees ALL groups at every tier.
func TestListGroups_AdminSeesMemberAdminGroupsAndParentLinkedUserGroups(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_a1", "admin")
	e.createUser("usr_a2", "admin")
	e.createUser("usr_x", "user")

	mk := func(id, name, tier, parent, owner string) {
		g := store.UserGroup{ID: id, Tier: tier, Name: name, ParentGroupID: parent, OwnerUserID: owner, CreatedAt: e.now, UpdatedAt: e.now}
		if err := e.dir.CreateUserGroup(e.ctx, g); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ugrp_sg", "SG", store.GroupTierSystem, "", "")
	mk("ugrp_ag1", "AG1", store.GroupTierAdmin, "ugrp_sg", "usr_a1")
	mk("ugrp_ag2", "AG2", store.GroupTierAdmin, "ugrp_sg", "usr_a2")
	mk("ugrp_ug1", "UG1", store.GroupTierUser, "ugrp_ag1", "usr_a1") // a1 owns
	mk("ugrp_ug2", "UG2", store.GroupTierUser, "ugrp_ag2", "usr_a2") // under AG2 -> a1 must NOT see
	mk("ugrp_ug3", "UG3", store.GroupTierUser, "ugrp_ag1", "usr_x")  // under AG1, a1 unrelated -> sees via parent

	ids := func(gs []UserGroupDTO) map[string]UserGroupDTO {
		out := make(map[string]UserGroupDTO, len(gs))
		for _, g := range gs {
			out[g.ID] = g
		}
		return out
	}

	// --- plain admin a1 ---
	a1 := token("usr_a1", "admin")
	land, err := e.svc.ListGroups(e.ctx, a1)
	if err != nil {
		t.Fatalf("ListGroups(a1): %v", err)
	}
	admin := ids(land.Admin)
	if _, ok := admin["ugrp_ag1"]; !ok {
		t.Fatalf("a1 should see AG1 in Admin, got %v", land.Admin)
	}
	if _, ok := admin["ugrp_ag2"]; ok {
		t.Fatalf("a1 must NOT see AG2 (not a member), got %v", land.Admin)
	}
	user := ids(land.User)
	if _, ok := user["ugrp_ug1"]; !ok {
		t.Fatalf("a1 should see UG1 (owned), got %v", land.User)
	}
	ug3, ok := user["ugrp_ug3"]
	if !ok {
		t.Fatalf("a1 should see UG3 via parent AG1, got %v", land.User)
	}
	if ug3.MyRole != "" || ug3.CanManage {
		t.Fatalf("UG3 (parent-linked) must be read-only for a1: my_role=%q can_manage=%v", ug3.MyRole, ug3.CanManage)
	}
	if _, ok := user["ugrp_ug2"]; ok {
		t.Fatalf("a1 must NOT see UG2 (under AG2), got %v", land.User)
	}

	// --- system_admin sees everything ---
	sys := token("usr_s", "admin", "system")
	sland, err := e.svc.ListGroups(e.ctx, sys)
	if err != nil {
		t.Fatalf("ListGroups(system): %v", err)
	}
	if len(sland.System) != 1 || sland.System[0].ID != "ugrp_sg" {
		t.Fatalf("system_admin System = %v, want [SG]", sland.System)
	}
	sadmin := ids(sland.Admin)
	if _, ok := sadmin["ugrp_ag1"]; !ok {
		t.Fatalf("system_admin should see AG1, got %v", sland.Admin)
	}
	if _, ok := sadmin["ugrp_ag2"]; !ok {
		t.Fatalf("system_admin should see AG2, got %v", sland.Admin)
	}
	suser := ids(sland.User)
	for _, want := range []string{"ugrp_ug1", "ugrp_ug2", "ugrp_ug3"} {
		if _, ok := suser[want]; !ok {
			t.Fatalf("system_admin should see %s, got %v", want, sland.User)
		}
	}
}

// TestGroupMembers_RosterAndAuthorization builds a user-tier group UG owned
// by usr_owner (parent AG, parent SG) with: the owner (auto member, is_owner),
// a promoted co-manager usr_a (state=member, is_manager), and a still-pending
// invitee usr_b (state=invited, added directly via AddGroupMembers rather
// than the mustAddMembers auto-accept helper). Asserts GroupMembers returns
// all three with the correct identity + role flags, and that a principal who
// cannot manage UG (a plain outsider) gets ErrGroupNotFound (404-no-leak).
func TestGroupMembers_RosterAndAuthorization(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_owner", "user")
	e.createUser("usr_a", "user")
	e.createUser("usr_b", "user")
	e.createUser("usr_outsider", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	admin := token("usr_admin", "admin")
	owner := token("usr_owner")
	outsider := token("usr_outsider")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_admin", "usr_owner", "usr_a", "usr_b")
	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(admin, ag.ID, "usr_owner", "usr_a", "usr_b")

	ug := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "UG"})
	e.mustAddMembers(owner, ug.ID, "usr_a") // accepted -> state=member
	if err := e.svc.PromoteManager(e.ctx, owner, ug.ID, "usr_a", true, true, true, true, true); err != nil {
		t.Fatalf("PromoteManager usr_a: %v", err)
	}
	// usr_b is invited but does NOT accept -> stays state=invited.
	if err := e.svc.AddGroupMembers(e.ctx, owner, ug.ID, []string{"usr_b"}); err != nil {
		t.Fatalf("AddGroupMembers usr_b (invite): %v", err)
	}

	roster, err := e.svc.GroupMembers(e.ctx, owner, ug.ID)
	if err != nil {
		t.Fatalf("GroupMembers(owner): %v", err)
	}
	byID := map[string]UserGroupMemberDTO{}
	for _, m := range roster {
		byID[m.UserID] = m
	}
	if len(roster) != 3 {
		t.Fatalf("GroupMembers(owner) = %+v, want 3 rows (owner+manager+invitee)", roster)
	}
	ownerRow, ok := byID["usr_owner"]
	if !ok || ownerRow.State != store.GroupStateMember || !ownerRow.IsOwner || ownerRow.IsManager {
		t.Fatalf("owner row = %+v, want state=member is_owner=true is_manager=false", ownerRow)
	}
	if ownerRow.Email == "" || ownerRow.DisplayName == "" {
		t.Fatalf("owner row missing identity: %+v", ownerRow)
	}
	managerRow, ok := byID["usr_a"]
	if !ok || managerRow.State != store.GroupStateMember || managerRow.IsOwner || !managerRow.IsManager {
		t.Fatalf("manager row = %+v, want state=member is_owner=false is_manager=true", managerRow)
	}
	inviteeRow, ok := byID["usr_b"]
	if !ok || inviteeRow.State != store.GroupStateInvited || inviteeRow.IsOwner || inviteeRow.IsManager {
		t.Fatalf("invitee row = %+v, want state=invited is_owner=false is_manager=false", inviteeRow)
	}

	// A system_admin also sees the roster (authorizeGroupManage grants them
	// access to every group).
	if _, err := e.svc.GroupMembers(e.ctx, sysAdmin, ug.ID); err != nil {
		t.Fatalf("GroupMembers(sysAdmin): %v", err)
	}

	// A principal with no owner/manager/system_admin standing on UG must get
	// the same 404-no-leak as a nonexistent group.
	if _, err := e.svc.GroupMembers(e.ctx, outsider, ug.ID); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("GroupMembers(outsider) err = %v, want ErrGroupNotFound", err)
	}
}

// TestResolveInviteAdminGroup covers the ≥co-manager gate, the mandatory rule,
// the wrong-tier / foreign rejections, and the system_admin latitude.
func TestResolveInviteAdminGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_a1", "admin")
	e.createUser("usr_a2", "admin")

	mk := func(id, name, tier, parent, owner string) {
		g := store.UserGroup{ID: id, Tier: tier, Name: name, ParentGroupID: parent, OwnerUserID: owner, CreatedAt: e.now, UpdatedAt: e.now}
		if err := e.dir.CreateUserGroup(e.ctx, g); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ugrp_sg", "SG", store.GroupTierSystem, "", "")
	mk("ugrp_ag1", "AG1", store.GroupTierAdmin, "ugrp_sg", "usr_a1") // a1 owns
	mk("ugrp_ag2", "AG2", store.GroupTierAdmin, "ugrp_sg", "usr_a2") // a1 unrelated
	mk("ugrp_ag3", "AG3", store.GroupTierAdmin, "ugrp_sg", "usr_a2") // a1 plain member
	if err := e.dir.SetUserGroupMember(e.ctx, "ugrp_ag3", "usr_a1", store.GroupStateMember, ""); err != nil {
		t.Fatalf("member a1->AG3: %v", err)
	}

	a1 := token("usr_a1", "admin")
	sys := token("usr_s", "admin", "system")

	// owner -> ok, returns parent SG.
	gid, parent, err := e.svc.ResolveInviteAdminGroup(e.ctx, a1, "ugrp_ag1")
	if err != nil || gid != "ugrp_ag1" || parent != "ugrp_sg" {
		t.Fatalf("owner resolve = (%q,%q,%v), want (ugrp_ag1, ugrp_sg, nil)", gid, parent, err)
	}
	// empty -> required.
	if _, _, err := e.svc.ResolveInviteAdminGroup(e.ctx, a1, ""); !errors.Is(err, ErrInviteAdminGroupRequired) {
		t.Fatalf("empty admin group err = %v, want ErrInviteAdminGroupRequired", err)
	}
	// foreign (a1 unrelated to AG2) -> invalid.
	if _, _, err := e.svc.ResolveInviteAdminGroup(e.ctx, a1, "ugrp_ag2"); !errors.Is(err, ErrInviteAdminGroupInvalid) {
		t.Fatalf("foreign admin group err = %v, want ErrInviteAdminGroupInvalid", err)
	}
	// plain member (not >= co-manager) -> invalid.
	if _, _, err := e.svc.ResolveInviteAdminGroup(e.ctx, a1, "ugrp_ag3"); !errors.Is(err, ErrInviteAdminGroupInvalid) {
		t.Fatalf("plain-member admin group err = %v, want ErrInviteAdminGroupInvalid", err)
	}
	// wrong tier (system group id) -> invalid.
	if _, _, err := e.svc.ResolveInviteAdminGroup(e.ctx, a1, "ugrp_sg"); !errors.Is(err, ErrInviteAdminGroupInvalid) {
		t.Fatalf("system-group id err = %v, want ErrInviteAdminGroupInvalid", err)
	}
	// system_admin -> any admin group ok.
	if gid, parent, err := e.svc.ResolveInviteAdminGroup(e.ctx, sys, "ugrp_ag2"); err != nil || gid != "ugrp_ag2" || parent != "ugrp_sg" {
		t.Fatalf("system_admin resolve = (%q,%q,%v), want (ugrp_ag2, ugrp_sg, nil)", gid, parent, err)
	}
}

// TestAddUserToAdminGroup: the new user becomes a member of BOTH the parent
// system group and the admin group.
func TestAddUserToAdminGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_new", "user")
	mkSys := store.UserGroup{ID: "ugrp_sg", Tier: store.GroupTierSystem, Name: "SG", CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.dir.CreateUserGroup(e.ctx, mkSys); err != nil {
		t.Fatalf("create SG: %v", err)
	}
	mkAg := store.UserGroup{ID: "ugrp_ag", Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: "ugrp_sg", OwnerUserID: "usr_a", CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.dir.CreateUserGroup(e.ctx, mkAg); err != nil {
		t.Fatalf("create AG: %v", err)
	}
	if err := e.svc.AddUserToAdminGroup(e.ctx, adminToken(), "usr_new", "ugrp_ag", "ugrp_sg"); err != nil {
		t.Fatalf("AddUserToAdminGroup: %v", err)
	}
	isMember := func(groupID string) bool {
		members, err := e.dir.UserGroupMembers(e.ctx, groupID)
		if err != nil {
			t.Fatalf("members %s: %v", groupID, err)
		}
		for _, m := range members {
			if m.UserID == "usr_new" && m.State == store.GroupStateMember {
				return true
			}
		}
		return false
	}
	if !isMember("ugrp_sg") {
		t.Fatalf("usr_new should be a member of the parent system group")
	}
	if !isMember("ugrp_ag") {
		t.Fatalf("usr_new should be a member of the admin group")
	}
}

func TestAdminOwnerCandidates(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_a", "admin")
	e.createUser("usr_b", "admin")
	e.createUser("usr_u", "user")
	sg := store.UserGroup{ID: "ugrp_sg", Tier: store.GroupTierSystem, Name: "SG", CreatedAt: e.now, UpdatedAt: e.now}
	if err := e.dir.CreateUserGroup(e.ctx, sg); err != nil {
		t.Fatalf("create SG: %v", err)
	}
	if err := e.dir.SetUserGroupMember(e.ctx, "ugrp_sg", "usr_a", store.GroupStateMember, ""); err != nil {
		t.Fatalf("member: %v", err)
	}
	sys := token("usr_s", "admin", "system")
	adm := token("usr_a", "admin")

	// non-system caller -> forbidden.
	if _, err := e.svc.AdminOwnerCandidates(e.ctx, adm); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("admin caller = %v, want ErrGroupForbidden", err)
	}
	// system_admin -> active admins (a,b), excludes self (s) + the user (u).
	got, err := e.svc.AdminOwnerCandidates(e.ctx, sys)
	if err != nil {
		t.Fatalf("candidates: %v", err)
	}
	ids := map[string][]GroupRefDTO{}
	for _, c := range got {
		ids[c.UserID] = c.SystemGroups
	}
	if _, ok := ids["usr_a"]; !ok {
		t.Fatalf("usr_a should be a candidate, got %v", got)
	}
	if _, ok := ids["usr_b"]; !ok {
		t.Fatalf("usr_b should be a candidate, got %v", got)
	}
	if _, ok := ids["usr_s"]; ok {
		t.Fatalf("caller usr_s must be excluded")
	}
	if _, ok := ids["usr_u"]; ok {
		t.Fatalf("regular user usr_u must be excluded")
	}
	if len(ids["usr_a"]) != 1 || ids["usr_a"][0].ID != "ugrp_sg" {
		t.Fatalf("usr_a system_groups = %v, want [SG]", ids["usr_a"])
	}
	if ids["usr_b"] == nil {
		t.Fatalf("usr_b system_groups must be non-nil (empty [], not null)")
	}
}

// TestGroupDTOResolvesOwnerName proves groupDTO surfaces the owner's display
// name (distinct from the opaque owner id) so the UI shows a name, not an id.
func TestGroupDTOResolvesOwnerName(t *testing.T) {
	e := newGroupTestEnv(t)
	// Owner with a display name that differs from the id.
	if err := e.dir.CreateUser(e.ctx, store.User{
		ID: "usr_owner", Email: "owner@example.test", DisplayName: "Alice Admin",
		Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: e.now, UpdatedAt: e.now,
	}); err != nil {
		t.Fatalf("create owner: %v", err)
	}
	mk := func(id, name, tier, parent, owner string) {
		g := store.UserGroup{ID: id, Tier: tier, Name: name, ParentGroupID: parent, OwnerUserID: owner, CreatedAt: e.now, UpdatedAt: e.now}
		if err := e.dir.CreateUserGroup(e.ctx, g); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	mk("ugrp_sg", "SG", store.GroupTierSystem, "", "")
	mk("ugrp_ag", "AG", store.GroupTierAdmin, "ugrp_sg", "usr_owner")

	sys := token("usr_s", "admin", "system")
	e.createUser("usr_s", "system_admin")
	land, err := e.svc.ListGroups(e.ctx, sys)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	var got *UserGroupDTO
	for i := range land.Admin {
		if land.Admin[i].ID == "ugrp_ag" {
			got = &land.Admin[i]
		}
	}
	if got == nil {
		t.Fatalf("AG not in admin landscape: %+v", land.Admin)
	}
	if got.OwnerUserID != "usr_owner" {
		t.Fatalf("owner id = %q, want usr_owner", got.OwnerUserID)
	}
	if got.OwnerName != "Alice Admin" {
		t.Fatalf("owner name = %q, want %q (must be the display name, not the id)", got.OwnerName, "Alice Admin")
	}
	// A system-tier / owner-less group leaves OwnerName empty.
	for _, g := range land.System {
		if g.ID == "ugrp_sg" && g.OwnerName != "" {
			t.Fatalf("system group OwnerName = %q, want empty", g.OwnerName)
		}
	}
}

// --- Task 2: flag-gated group authorization + manager permissions ---------

// TestAuthorizeGroupManage_NeedMatrix covers the per-Admin-Group co-manager
// permissions authz gate (spec 2026-08-10): the owner passes every need; a
// system-scope caller passes every need; a co-manager narrowed to
// {CanManageUsers:true, CanManageGroup:false} passes needRead/needUsers but
// gets the no-leak ErrGroupNotFound for needGroup (never a distinguishable
// forbidden); a non-member gets ErrGroupNotFound for every need.
func TestAuthorizeGroupManage_NeedMatrix(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_stranger", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	stranger := token("usr_stranger")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	// Narrow usr_mgr to CanManageUsers only (no structure permission).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("promote usr_mgr (users-only): %v", err)
	}
	mgr := token("usr_mgr", "admin")

	allNeeds := []groupManageNeed{needRead, needGroup, needUsers}

	for _, need := range allNeeds {
		if _, err := e.svc.authorizeGroupManage(e.ctx, owner, ag.ID, need); err != nil {
			t.Fatalf("owner need=%d: %v", need, err)
		}
	}
	for _, need := range allNeeds {
		if _, err := e.svc.authorizeGroupManage(e.ctx, sysAdmin, ag.ID, need); err != nil {
			t.Fatalf("system-scope need=%d: %v", need, err)
		}
	}
	if _, err := e.svc.authorizeGroupManage(e.ctx, mgr, ag.ID, needRead); err != nil {
		t.Fatalf("narrowed co-manager needRead: %v", err)
	}
	if _, err := e.svc.authorizeGroupManage(e.ctx, mgr, ag.ID, needUsers); err != nil {
		t.Fatalf("narrowed co-manager needUsers: %v", err)
	}
	if _, err := e.svc.authorizeGroupManage(e.ctx, mgr, ag.ID, needGroup); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("narrowed co-manager needGroup = %v, want ErrGroupNotFound", err)
	}
	for _, need := range allNeeds {
		if _, err := e.svc.authorizeGroupManage(e.ctx, stranger, ag.ID, need); !errors.Is(err, ErrGroupNotFound) {
			t.Fatalf("non-member need=%d = %v, want ErrGroupNotFound", need, err)
		}
	}
}

// TestPromoteManager_PersistsPermissionFlags proves PromoteManager's
// canUsers/canGroup arguments round-trip through GroupMembers'
// UserGroupMemberDTO (the DTO's flags come straight from the stored
// co-manager row PromoteManager just wrote).
func TestPromoteManager_PersistsPermissionFlags(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	var mgrRow *UserGroupMemberDTO
	for i := range roster {
		if roster[i].UserID == "usr_mgr" {
			mgrRow = &roster[i]
		}
	}
	if mgrRow == nil {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
	if !mgrRow.IsManager || !mgrRow.CanManageUsers || mgrRow.CanManageGroup {
		t.Fatalf("mgr row = %+v, want is_manager=true can_manage_users=true can_manage_group=false", mgrRow)
	}
}

// TestSetManagerPermissions_OwnerOnlyAndTargetMustBeManager covers the
// owner-only gate (a co-manager -- even one with full flags -- and a total
// stranger are both rejected, distinguishably: Forbidden vs no-leak NotFound)
// and the "target must already be a co-manager" rule (a plain member is
// ErrGroupCandidateInvalid, never silently promoted).
func TestSetManagerPermissions_OwnerOnlyAndTargetMustBeManager(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	e.createUser("usr_member", "admin")
	e.createUser("usr_stranger", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	stranger := token("usr_stranger")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr", "usr_member")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr", "usr_member")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("promote usr_mgr: %v", err)
	}
	mgr := token("usr_mgr", "admin")

	// A plain member (never promoted) is an ineligible target.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_member", true, true, true, true, true); !errors.Is(err, ErrGroupCandidateInvalid) {
		t.Fatalf("target not a manager: got %v, want ErrGroupCandidateInvalid", err)
	}
	// A co-manager (visible, but not owner) gets Forbidden -- even trying to
	// edit their OWN flags.
	if err := e.svc.SetManagerPermissions(e.ctx, mgr, ag.ID, "usr_mgr", false, false, true, true, true); !errors.Is(err, ErrGroupForbidden) {
		t.Fatalf("co-manager caller: got %v, want ErrGroupForbidden", err)
	}
	// A total stranger (not visible at all) gets the no-leak 404, distinct
	// from the co-manager's Forbidden above.
	if err := e.svc.SetManagerPermissions(e.ctx, stranger, ag.ID, "usr_mgr", false, false, true, true, true); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("stranger caller: got %v, want ErrGroupNotFound", err)
	}

	// The owner narrows usr_mgr to CanManageGroup only.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", false, true, true, true, true); err != nil {
		t.Fatalf("owner narrow: %v", err)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	found := false
	for _, m := range roster {
		if m.UserID == "usr_mgr" {
			found = true
			if m.CanManageUsers || !m.CanManageGroup {
				t.Fatalf("usr_mgr row after narrow = %+v, want can_manage_users=false can_manage_group=true", m)
			}
		}
	}
	if !found {
		t.Fatalf("usr_mgr missing from roster after narrow: %+v", roster)
	}

	// A system_admin may also widen/narrow, same as the owner.
	if err := e.svc.SetManagerPermissions(e.ctx, sysAdmin, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("system_admin widen: %v", err)
	}
}

// TestRenameGroup_RequiresCanManageGroup proves RenameGroup is gated on
// needGroup: a co-manager narrowed to CanManageUsers-only cannot rename
// (no-leak ErrGroupNotFound), and widening them to CanManageGroup lets it
// succeed.
func TestRenameGroup_RequiresCanManageGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("promote (users-only): %v", err)
	}

	if _, err := e.svc.RenameGroup(e.ctx, mgr, ag.ID, "AG-Renamed"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("rename without can_manage_group: got %v, want ErrGroupNotFound", err)
	}
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("widen: %v", err)
	}
	renamed, err := e.svc.RenameGroup(e.ctx, mgr, ag.ID, "AG-Renamed")
	if err != nil {
		t.Fatalf("rename with can_manage_group: %v", err)
	}
	if renamed.Name != "AG-Renamed" {
		t.Fatalf("rename did not stick: %+v", renamed)
	}
}

// TestResolveInviteAdminGroup_RequiresCanManageUsers proves the invite-time
// authority check is gated on needUsers: a co-manager narrowed to
// CanManageGroup-only cannot be used to resolve an admin-group invite target
// (ErrInviteAdminGroupInvalid), and widening them to CanManageUsers lets it
// succeed.
func TestResolveInviteAdminGroup_RequiresCanManageUsers(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", false, true, true, true, true); err != nil {
		t.Fatalf("promote (group-only): %v", err)
	}

	if _, _, err := e.svc.ResolveInviteAdminGroup(e.ctx, mgr, ag.ID); !errors.Is(err, ErrInviteAdminGroupInvalid) {
		t.Fatalf("resolve without can_manage_users: got %v, want ErrInviteAdminGroupInvalid", err)
	}
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("widen: %v", err)
	}
	gid, parent, err := e.svc.ResolveInviteAdminGroup(e.ctx, mgr, ag.ID)
	if err != nil || gid != ag.ID || parent != sg.ID {
		t.Fatalf("resolve with can_manage_users = (%q,%q,%v), want (%q,%q,nil)", gid, parent, err, ag.ID, sg.ID)
	}
}

// TestGroupDTO_CanManageSplitByFlag proves groupDTO's CanManage (structure
// facet) and CanManageUsers (user-assignment facet) each follow ONLY their
// own stored per-permission flag for a co-manager -- independently, never
// coupled (narrowing one must not silently widen the other).
func TestGroupDTO_CanManageSplitByFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("promote (users-only): %v", err)
	}

	g, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	dto := e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManageUsers || dto.CanManage {
		t.Fatalf("mgr dto = %+v, want my_role=manager can_manage_users=true can_manage=false", dto)
	}

	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", false, true, true, true, true); err != nil {
		t.Fatalf("narrow to group-only: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || dto.CanManageUsers || !dto.CanManage {
		t.Fatalf("mgr dto after narrow = %+v, want my_role=manager can_manage_users=false can_manage=true", dto)
	}
}

// TestPromoteManager_PersistsCanManageServers proves the THIRD co-manager
// permission flag (admin-group permissions Phase B, spec 2026-08-10) reaches
// GroupMembers' roster row via PromoteManager, exactly like
// TestPromoteManager_PersistsPermissionFlags proved for the first two.
func TestPromoteManager_PersistsCanManageServers(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// canUsers=true, canGroup=true, canServers=false -- proves the third flag
	// is independently settable at promote time (not coupled to the other
	// two).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, false, true, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	var mgrRow *UserGroupMemberDTO
	for i := range roster {
		if roster[i].UserID == "usr_mgr" {
			mgrRow = &roster[i]
		}
	}
	if mgrRow == nil {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
	if !mgrRow.IsManager || !mgrRow.CanManageUsers || !mgrRow.CanManageGroup || mgrRow.CanManageServers {
		t.Fatalf("mgr row = %+v, want is_manager=true can_manage_users=true can_manage_group=true can_manage_servers=false", mgrRow)
	}
}

// TestGroupDTO_CanManageServersSplitByFlag mirrors
// TestGroupDTO_CanManageSplitByFlag for the third facet: groupDTO's
// CanManageServers follows ONLY the co-manager's own stored CanManageServers
// flag, independent of CanManage/CanManageUsers (admin-group permissions
// Phase B, spec 2026-08-10) -- narrowing CanManageServers alone must not
// disturb the other two facets, and vice versa.
func TestGroupDTO_CanManageServersSplitByFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// Promote with all three true -- reproduces "a co-manager can do
	// everything", including CanManageServers.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("promote (full rights): %v", err)
	}

	g, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	dto := e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers {
		t.Fatalf("mgr dto (full rights) = %+v, want all three facets true", dto)
	}

	// Narrow ONLY CanManageServers -- the other two facets (structure +
	// user-assignment) must be UNCHANGED.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, true, false, true, true); err != nil {
		t.Fatalf("narrow servers-only: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || dto.CanManageServers {
		t.Fatalf("mgr dto after servers-only narrow = %+v, want can_manage=true can_manage_users=true can_manage_servers=false", dto)
	}

	// Widen CanManageServers back while narrowing CanManage (structure) --
	// proves the flags remain independently settable in both directions, and
	// GroupMembers surfaces the same state as groupDTO.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("widen servers, narrow group: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers {
		t.Fatalf("mgr dto after widen-servers/narrow-group = %+v, want can_manage=false can_manage_users=true can_manage_servers=true", dto)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	found := false
	for _, m := range roster {
		if m.UserID == "usr_mgr" {
			found = true
			if m.CanManageGroup || !m.CanManageUsers || !m.CanManageServers {
				t.Fatalf("usr_mgr roster row = %+v, want can_manage_group=false can_manage_users=true can_manage_servers=true", m)
			}
		}
	}
	if !found {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
}

// TestPromoteManager_PersistsCanManageServices proves the FOURTH co-manager
// permission flag (admin-group permissions Phase C, spec 2026-08-10) reaches
// GroupMembers' roster row via PromoteManager, exactly like
// TestPromoteManager_PersistsCanManageServers proved for the third.
func TestPromoteManager_PersistsCanManageServices(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// canUsers=true, canGroup=true, canServers=true, canServices=false --
	// proves the fourth flag is independently settable at promote time (not
	// coupled to the other three).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, false, true); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	var mgrRow *UserGroupMemberDTO
	for i := range roster {
		if roster[i].UserID == "usr_mgr" {
			mgrRow = &roster[i]
		}
	}
	if mgrRow == nil {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
	if !mgrRow.IsManager || !mgrRow.CanManageUsers || !mgrRow.CanManageGroup || !mgrRow.CanManageServers || mgrRow.CanManageServices {
		t.Fatalf("mgr row = %+v, want is_manager=true can_manage_users=true can_manage_group=true can_manage_servers=true can_manage_services=false", mgrRow)
	}
}

// TestGroupDTO_CanManageServicesSplitByFlag mirrors
// TestGroupDTO_CanManageServersSplitByFlag for the fourth facet: groupDTO's
// CanManageServices follows ONLY the co-manager's own stored
// CanManageServices flag, independent of CanManage/CanManageUsers/
// CanManageServers (admin-group permissions Phase C, spec 2026-08-10) --
// narrowing CanManageServices alone must not disturb the other three facets,
// and vice versa.
func TestGroupDTO_CanManageServicesSplitByFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// Promote with all four true -- reproduces "a co-manager can do
	// everything", including CanManageServices.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("promote (full rights): %v", err)
	}

	g, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	dto := e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || !dto.CanManageServices {
		t.Fatalf("mgr dto (full rights) = %+v, want all four facets true", dto)
	}

	// Narrow ONLY CanManageServices -- the other three facets (structure +
	// user-assignment + server-management) must be UNCHANGED.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, false, true); err != nil {
		t.Fatalf("narrow services-only: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || dto.CanManageServices {
		t.Fatalf("mgr dto after services-only narrow = %+v, want can_manage=true can_manage_users=true can_manage_servers=true can_manage_services=false", dto)
	}

	// Widen CanManageServices back while narrowing CanManage (structure) --
	// proves the flags remain independently settable in both directions, and
	// GroupMembers surfaces the same state as groupDTO.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("widen services, narrow group: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || !dto.CanManageServices {
		t.Fatalf("mgr dto after widen-services/narrow-group = %+v, want can_manage=false can_manage_users=true can_manage_servers=true can_manage_services=true", dto)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	found := false
	for _, m := range roster {
		if m.UserID == "usr_mgr" {
			found = true
			if m.CanManageGroup || !m.CanManageUsers || !m.CanManageServers || !m.CanManageServices {
				t.Fatalf("usr_mgr roster row = %+v, want can_manage_group=false can_manage_users=true can_manage_servers=true can_manage_services=true", m)
			}
		}
	}
	if !found {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
}

// TestPromoteManager_PersistsCanManageResources proves the FIFTH co-manager
// permission flag (Resource Groups Phase 1, spec 2026-08-11) reaches
// GroupMembers' roster row via PromoteManager, exactly like
// TestPromoteManager_PersistsCanManageServices proved for the fourth.
func TestPromoteManager_PersistsCanManageResources(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// canUsers=true, canGroup=true, canServers=true, canServices=true,
	// canResources=false -- proves the fifth flag is independently settable
	// at promote time (not coupled to the other four).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, false); err != nil {
		t.Fatalf("PromoteManager: %v", err)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	var mgrRow *UserGroupMemberDTO
	for i := range roster {
		if roster[i].UserID == "usr_mgr" {
			mgrRow = &roster[i]
		}
	}
	if mgrRow == nil {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
	if !mgrRow.IsManager || !mgrRow.CanManageUsers || !mgrRow.CanManageGroup || !mgrRow.CanManageServers || !mgrRow.CanManageServices || mgrRow.CanManageResources {
		t.Fatalf("mgr row = %+v, want is_manager=true can_manage_users=true can_manage_group=true can_manage_servers=true can_manage_services=true can_manage_resources=false", mgrRow)
	}
}

// TestGroupDTO_CanManageResourcesSplitByFlag mirrors
// TestGroupDTO_CanManageServicesSplitByFlag for the fifth facet: groupDTO's
// CanManageResources follows ONLY the co-manager's own stored
// CanManageResources flag, independent of CanManage/CanManageUsers/
// CanManageServers/CanManageServices (Resource Groups Phase 1, spec
// 2026-08-11) -- narrowing CanManageResources alone must not disturb the
// other four facets, and vice versa.
func TestGroupDTO_CanManageResourcesSplitByFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	mgr := token("usr_mgr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr")

	// Promote with all five true -- reproduces "a co-manager can do
	// everything", including CanManageResources.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, true); err != nil {
		t.Fatalf("promote (full rights): %v", err)
	}

	g, err := e.dir.UserGroupByID(e.ctx, ag.ID)
	if err != nil {
		t.Fatalf("load group: %v", err)
	}
	dto := e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || !dto.CanManageServices || !dto.CanManageResources {
		t.Fatalf("mgr dto (full rights) = %+v, want all five facets true", dto)
	}

	// Narrow ONLY CanManageResources -- the other four facets (structure +
	// user-assignment + server-management + service-management) must be
	// UNCHANGED.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, true, true, true, false); err != nil {
		t.Fatalf("narrow resources-only: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || !dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || !dto.CanManageServices || dto.CanManageResources {
		t.Fatalf("mgr dto after resources-only narrow = %+v, want can_manage=true can_manage_users=true can_manage_servers=true can_manage_services=true can_manage_resources=false", dto)
	}

	// Widen CanManageResources back while narrowing CanManage (structure) --
	// proves the flags remain independently settable in both directions, and
	// GroupMembers surfaces the same state as groupDTO.
	if err := e.svc.SetManagerPermissions(e.ctx, owner, ag.ID, "usr_mgr", true, false, true, true, true); err != nil {
		t.Fatalf("widen resources, narrow group: %v", err)
	}
	dto = e.svc.groupDTO(e.ctx, mgr, g)
	if dto.MyRole != "manager" || dto.CanManage || !dto.CanManageUsers || !dto.CanManageServers || !dto.CanManageServices || !dto.CanManageResources {
		t.Fatalf("mgr dto after widen-resources/narrow-group = %+v, want can_manage=false can_manage_users=true can_manage_servers=true can_manage_services=true can_manage_resources=true", dto)
	}
	roster, err := e.svc.GroupMembers(e.ctx, owner, ag.ID)
	if err != nil {
		t.Fatalf("GroupMembers: %v", err)
	}
	found := false
	for _, m := range roster {
		if m.UserID == "usr_mgr" {
			found = true
			if m.CanManageGroup || !m.CanManageUsers || !m.CanManageServers || !m.CanManageServices || !m.CanManageResources {
				t.Fatalf("usr_mgr roster row = %+v, want can_manage_group=false can_manage_users=true can_manage_servers=true can_manage_services=true can_manage_resources=true", m)
			}
		}
	}
	if !found {
		t.Fatalf("usr_mgr missing from roster: %+v", roster)
	}
}

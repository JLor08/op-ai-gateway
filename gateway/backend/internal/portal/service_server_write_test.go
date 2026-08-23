// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Task 4: server WRITE (create gate/validate/persist + linkage editing +
// candidates endpoint), spec 2026-08-10 ----------------------------------

// TestCreateServerGateRequiresManageGroupOrSystemScope proves CreateServer's
// authorization gate: a non-system principal with NO can_manage_servers reach
// into any admin group (serverManageGroupIDs empty) gets ErrServerForbidden
// -- even with a syntactically-valid AdminGroupIDs list, the gate runs FIRST
// and never even reaches request-shape validation. A system-scope principal
// is exempt from the gate (though still subject to the AdminGroupIDs
// requirement -- see TestCreateServerRejectsEmptyAdminGroupIDs).
func TestCreateServerGateRequiresManageGroupOrSystemScope(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_plain", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	plain := token("usr_plain", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	// usr_plain manages NO admin group (owns none, co-manages none) -> the
	// GATE fires before AdminGroupIDs is even inspected.
	if _, err := e.svc.CreateServer(e.ctx, plain, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{ag.ID},
	}); !errors.Is(err, ErrServerForbidden) {
		t.Fatalf("CreateServer(plain, no manage-group) = %v, want ErrServerForbidden", err)
	}

	// system scope is exempt from the gate (it still needs a valid
	// AdminGroupIDs set -- proven separately).
	if _, err := e.svc.CreateServer(e.ctx, sysAdmin, CreateServerRequest{
		Name: "S2", Domain: "s2.example.test", AdminGroupIDs: []string{ag.ID},
	}); err != nil {
		t.Fatalf("CreateServer(system) = %v, want nil", err)
	}
}

// TestCreateServerCoManagerHappyPath proves the primary success path: a
// can_manage_servers CO-MANAGER (not the owner) of an admin group creates a
// server into it; the auto system_group_id equals the group's parent, the
// server_admin_groups join row is persisted (ServerAdminGroups reads it
// back), and the returned DTO carries admin_groups + system_group_id/_name.
func TestCreateServerCoManagerHappyPath(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_cms", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	cms := token("usr_cms", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_cms")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ag.ID, "usr_cms")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cms", false, false, true, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_servers): %v", err)
	}

	dto, err := e.svc.CreateServer(e.ctx, cms, CreateServerRequest{
		Name: "GPU 1", Domain: "gpu1.example.test", AdminGroupIDs: []string{ag.ID},
	})
	if err != nil {
		t.Fatalf("CreateServer(co-manager): %v", err)
	}
	if dto.SystemGroupID != sg.ID || dto.SystemGroupName != "SG" {
		t.Fatalf("dto.SystemGroupID/Name = %q/%q, want %q/SG", dto.SystemGroupID, dto.SystemGroupName, sg.ID)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag.ID || dto.AdminGroups[0].Name != "AG" {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG}]", dto.AdminGroups, ag.ID)
	}
	// The join row is really persisted, not just reflected on the response DTO.
	linked, err := e.routes.ServerAdminGroups(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("ServerAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("ServerAdminGroups(persisted) = %v, want [%s]", linked, ag.ID)
	}
	// SystemGroupID is really persisted on the row too (not just the DTO).
	stored, err := e.routes.AIServerByID(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if stored.SystemGroupID != sg.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q", stored.SystemGroupID, sg.ID)
	}
}

// TestCreateServerRejectsEmptyAdminGroupIDs proves len(AdminGroupIDs)==0 (post
// trim/dedup) is ErrServerAdminGroupRequired for EVERY scope, including
// system -- a server always needs >=1 linked admin group.
func TestCreateServerRejectsEmptyAdminGroupIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	// usr_owner owns an admin group (satisfies the CreateServer GATE) but the
	// REQUEST supplies no groups -- the request-shape check must still fire.
	e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateServer(e.ctx, owner, CreateServerRequest{
		Name: "S", Domain: "s.example.test",
	}); !errors.Is(err, ErrServerAdminGroupRequired) {
		t.Fatalf("CreateServer(owner, no groups) = %v, want ErrServerAdminGroupRequired", err)
	}
	// A whitespace-only entry dedups/trims to empty too.
	if _, err := e.svc.CreateServer(e.ctx, owner, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{"  ", ""},
	}); !errors.Is(err, ErrServerAdminGroupRequired) {
		t.Fatalf("CreateServer(owner, blank-only groups) = %v, want ErrServerAdminGroupRequired", err)
	}
	// system scope is NOT exempt from this requirement.
	if _, err := e.svc.CreateServer(e.ctx, sysAdmin, CreateServerRequest{
		Name: "S", Domain: "s.example.test",
	}); !errors.Is(err, ErrServerAdminGroupRequired) {
		t.Fatalf("CreateServer(system, no groups) = %v, want ErrServerAdminGroupRequired", err)
	}
}

// TestCreateServerRejectsNonManageableGroup proves a NON-system caller cannot
// link a server into an admin-tier group they neither own nor co-manage
// (can_manage_servers) -- even though the group genuinely exists and is
// admin-tier -- and that a genuinely non-existent / non-admin-tier id is
// ALSO ErrServerAdminGroupInvalid (not a different sentinel).
func TestCreateServerRejectsNonManageableGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_mine", "admin")
	e.createUser("usr_theirs", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	mine := token("usr_mine", "admin")
	theirs := token("usr_theirs", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_mine", "usr_theirs")
	// mine's own group (satisfies the create GATE).
	myAG := e.mustCreateGroup(mine, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "MineAG", ParentGroupID: sg.ID})
	// A group mine does NOT manage.
	theirAG := e.mustCreateGroup(theirs, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "TheirsAG", ParentGroupID: sg.ID})

	// A group that genuinely exists (admin-tier) but is not manageable by mine.
	if _, err := e.svc.CreateServer(e.ctx, mine, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{theirAG.ID},
	}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("CreateServer(non-manageable group) = %v, want ErrServerAdminGroupInvalid", err)
	}
	// A completely unknown id.
	if _, err := e.svc.CreateServer(e.ctx, mine, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{"ugrp_ghost"},
	}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("CreateServer(unknown group) = %v, want ErrServerAdminGroupInvalid", err)
	}
	// A SYSTEM-tier group id (exists, but not admin-tier).
	if _, err := e.svc.CreateServer(e.ctx, mine, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{sg.ID},
	}); !errors.Is(err, ErrServerAdminGroupInvalid) {
		t.Fatalf("CreateServer(system-tier group id) = %v, want ErrServerAdminGroupInvalid", err)
	}
	// Sanity: mine's OWN group still works (the gate + validation both pass).
	if _, err := e.svc.CreateServer(e.ctx, mine, CreateServerRequest{
		Name: "OK", Domain: "ok.example.test", AdminGroupIDs: []string{myAG.ID},
	}); err != nil {
		t.Fatalf("CreateServer(own manageable group) = %v, want nil", err)
	}
}

// TestCreateServerRejectsDifferingParents proves that when the caller
// specifies MULTIPLE admin groups whose parent (system-tier) groups differ,
// the create is rejected as ErrServerAdminGroupParentMismatch -- containment
// requires ALL linked groups to share exactly one root.
func TestCreateServerRejectsDifferingParents(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg1 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	e.mustAddMembers(sysAdmin, sg1.ID, "usr_owner")
	e.mustAddMembers(sysAdmin, sg2.ID, "usr_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg1.ID})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg2.ID})

	if _, err := e.svc.CreateServer(e.ctx, owner, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{ag1.ID, ag2.ID},
	}); !errors.Is(err, ErrServerAdminGroupParentMismatch) {
		t.Fatalf("CreateServer(differing parents) = %v, want ErrServerAdminGroupParentMismatch", err)
	}
	// Two groups sharing the SAME parent succeed and dedup the containment
	// root to that one parent.
	ag3 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG3", ParentGroupID: sg1.ID})
	dto, err := e.svc.CreateServer(e.ctx, owner, CreateServerRequest{
		Name: "S2", Domain: "s2.example.test", AdminGroupIDs: []string{ag1.ID, ag3.ID},
	})
	if err != nil {
		t.Fatalf("CreateServer(same parent, two groups) = %v, want nil", err)
	}
	if dto.SystemGroupID != sg1.ID || len(dto.AdminGroups) != 2 {
		t.Fatalf("dto = %#v, want SystemGroupID=%s + 2 admin_groups", dto, sg1.ID)
	}
}

// TestCreateServerSystemGroupHintCrossCheck proves the system-scope
// convenience SystemGroupID cross-check (CreateServerRequest.SystemGroupID):
// a mismatching hint is rejected as ErrServerAdminGroupParentMismatch even
// though the chosen group's own containment is internally consistent, and a
// non-system caller's hint is IGNORED (never checked -- only system-scope
// pays attention to it, per validateAdminGroupIDs).
func TestCreateServerSystemGroupHintCrossCheck(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	otherSG := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "OtherSG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	// system scope: a WRONG hint is rejected.
	if _, err := e.svc.CreateServer(e.ctx, sysAdmin, CreateServerRequest{
		Name: "S", Domain: "s.example.test", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); !errors.Is(err, ErrServerAdminGroupParentMismatch) {
		t.Fatalf("CreateServer(system, wrong hint) = %v, want ErrServerAdminGroupParentMismatch", err)
	}
	// system scope: the RIGHT hint succeeds.
	if _, err := e.svc.CreateServer(e.ctx, sysAdmin, CreateServerRequest{
		Name: "S2", Domain: "s2.example.test", AdminGroupIDs: []string{ag.ID}, SystemGroupID: sg.ID,
	}); err != nil {
		t.Fatalf("CreateServer(system, correct hint) = %v, want nil", err)
	}
	// non-system caller: a WRONG hint is simply ignored (only checked under
	// system scope) -- the create still succeeds via the owner's own group.
	if _, err := e.svc.CreateServer(e.ctx, owner, CreateServerRequest{
		Name: "S3", Domain: "s3.example.test", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); err != nil {
		t.Fatalf("CreateServer(non-system, hint ignored) = %v, want nil", err)
	}
}

// TestSetServerAdminGroupsAddRemoveDelta proves the linkage editor applies
// exactly the add/remove delta: starting from {AG1}, setting {AG2} adds AG2
// and removes AG1 (both groups share SG's containment root so no other
// invariant is disturbed), and the fresh serverDTO reflects the new set.
func TestSetServerAdminGroupsAddRemoveDelta(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg.ID})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})

	srv := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_delta", Name: "Delta"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srv.ID, ag1.ID); err != nil {
		t.Fatalf("SetServerAdminGroup(seed AG1): %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, srv.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup(seed): %v", err)
	}

	dto, err := e.svc.SetServerAdminGroups(e.ctx, owner, srv.ID, []string{ag2.ID})
	if err != nil {
		t.Fatalf("SetServerAdminGroups: %v", err)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag2.ID {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG2}]", dto.AdminGroups, ag2.ID)
	}
	linked, err := e.routes.ServerAdminGroups(e.ctx, srv.ID)
	if err != nil {
		t.Fatalf("ServerAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag2.ID {
		t.Fatalf("ServerAdminGroups(after set) = %v, want [%s] (AG1 removed, AG2 added)", linked, ag2.ID)
	}

	// Adding a group already linked is idempotent (no duplicate, no error).
	dto2, err := e.svc.SetServerAdminGroups(e.ctx, owner, srv.ID, []string{ag2.ID, ag1.ID})
	if err != nil {
		t.Fatalf("SetServerAdminGroups(add AG1 back): %v", err)
	}
	if len(dto2.AdminGroups) != 2 {
		t.Fatalf("dto2.AdminGroups = %#v, want 2 entries", dto2.AdminGroups)
	}
}

// TestSetServerAdminGroupsRequiresAtLeastOne proves removing the LAST linked
// admin group (an empty groupIDs) is rejected as ErrServerAdminGroupRequired
// -- a server can never be left with zero admin groups -- and that the
// rejected call leaves the existing link untouched.
func TestSetServerAdminGroupsRequiresAtLeastOne(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	srv := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_last", Name: "Last"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srv.ID, ag.ID); err != nil {
		t.Fatalf("SetServerAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, srv.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetServerAdminGroups(e.ctx, owner, srv.ID, nil); !errors.Is(err, ErrServerAdminGroupRequired) {
		t.Fatalf("SetServerAdminGroups(empty) = %v, want ErrServerAdminGroupRequired", err)
	}
	linked, err := e.routes.ServerAdminGroups(e.ctx, srv.ID)
	if err != nil {
		t.Fatalf("ServerAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("a rejected SetServerAdminGroups must not persist, got %v", linked)
	}
}

// TestSetServerAdminGroupsContainmentEnforced proves the new set must still
// share one parent -- an owner who co-manages TWO admin groups with
// DIFFERENT parents cannot link a server to both.
func TestSetServerAdminGroupsContainmentEnforced(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg1 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG1"})
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	e.mustAddMembers(sysAdmin, sg1.ID, "usr_owner")
	e.mustAddMembers(sysAdmin, sg2.ID, "usr_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg1.ID})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg2.ID})

	srv := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_contain", Name: "Contain"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srv.ID, ag1.ID); err != nil {
		t.Fatalf("SetServerAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, srv.ID, sg1.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetServerAdminGroups(e.ctx, owner, srv.ID, []string{ag1.ID, ag2.ID}); !errors.Is(err, ErrServerAdminGroupParentMismatch) {
		t.Fatalf("SetServerAdminGroups(differing parents) = %v, want ErrServerAdminGroupParentMismatch", err)
	}
}

// TestSetServerAdminGroupsCannotRelocateContainmentRootCrossTenant proves the
// containment-root-immutability guard (fix-round-1 finding, spec non-goal
// "Kein Reparenting der System-Gruppe eines Servers ueber verschiedene
// Tenants"): a NON-system manager who owns/co-manages admin groups in TWO
// DIFFERENT tenants (AG-A under SG-A, AG-B under SG-B) cannot swap an
// already-grouped server's linked groups for ones under the OTHER tenant --
// even though the NEW set ([AG-B]) is, by itself, perfectly self-consistent
// (a single group, one parent) and would pass validateAdminGroupIDs's own
// internal "all chosen groups share one parent" check in isolation. The
// server's root must NOT move: it stays under SG-A after the rejected call.
func TestSetServerAdminGroupsCannotRelocateContainmentRootCrossTenant(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	mgr := token("usr_mgr", "admin")

	sgA := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-A"})
	sgB := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-B"})
	e.mustAddMembers(sysAdmin, sgA.ID, "usr_mgr")
	e.mustAddMembers(sysAdmin, sgB.ID, "usr_mgr")
	// usr_mgr owns (hence manages) an admin group in EACH tenant.
	agA := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-A", ParentGroupID: sgA.ID})
	agB := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-B", ParentGroupID: sgB.ID})

	srv := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_crosstenant", Name: "CrossTenant"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srv.ID, agA.ID); err != nil {
		t.Fatalf("SetServerAdminGroup(seed AG-A): %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, srv.ID, sgA.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup(seed SG-A): %v", err)
	}

	// The attack: swap AG-A for AG-B -- a self-consistent NEW set (one
	// group, one parent SG-B) that would pass validateAdminGroupIDs's
	// internal consistency check in isolation, but must still be rejected
	// because it would relocate the server's root from SG-A to SG-B.
	if _, err := e.svc.SetServerAdminGroups(e.ctx, mgr, srv.ID, []string{agB.ID}); !errors.Is(err, ErrServerAdminGroupParentMismatch) {
		t.Fatalf("SetServerAdminGroups(cross-tenant swap) = %v, want ErrServerAdminGroupParentMismatch", err)
	}

	// The root did NOT move: the server is still linked to AG-A under SG-A.
	linked, err := e.routes.ServerAdminGroups(e.ctx, srv.ID)
	if err != nil {
		t.Fatalf("ServerAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != agA.ID {
		t.Fatalf("a rejected cross-tenant swap must not persist, ServerAdminGroups = %v, want [%s]", linked, agA.ID)
	}
	stored, err := e.routes.AIServerByID(e.ctx, srv.ID)
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if stored.SystemGroupID != sgA.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q (root must not relocate)", stored.SystemGroupID, sgA.ID)
	}

	// Positive control: adding a SECOND group under the SAME tenant (AG-A2,
	// also parent SG-A) still succeeds and leaves system_group_id unchanged
	// -- the guard only blocks a DIFFERENT root, not ordinary within-tenant
	// membership changes.
	agA2 := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-A2", ParentGroupID: sgA.ID})
	dto, err := e.svc.SetServerAdminGroups(e.ctx, mgr, srv.ID, []string{agA.ID, agA2.ID})
	if err != nil {
		t.Fatalf("SetServerAdminGroups(within-tenant add) = %v, want nil", err)
	}
	if dto.SystemGroupID != sgA.ID {
		t.Fatalf("dto.SystemGroupID = %q after within-tenant add, want unchanged %q", dto.SystemGroupID, sgA.ID)
	}
	if len(dto.AdminGroups) != 2 {
		t.Fatalf("dto.AdminGroups = %#v, want 2 entries (AG-A + AG-A2)", dto.AdminGroups)
	}

	// Removing AG-A and keeping only AG-A2 (still under SG-A) also succeeds,
	// root unchanged -- an ordinary within-tenant membership swap.
	dto2, err := e.svc.SetServerAdminGroups(e.ctx, mgr, srv.ID, []string{agA2.ID})
	if err != nil {
		t.Fatalf("SetServerAdminGroups(within-tenant swap) = %v, want nil", err)
	}
	if dto2.SystemGroupID != sgA.ID {
		t.Fatalf("dto2.SystemGroupID = %q after within-tenant swap, want unchanged %q", dto2.SystemGroupID, sgA.ID)
	}
	if len(dto2.AdminGroups) != 1 || dto2.AdminGroups[0].ID != agA2.ID {
		t.Fatalf("dto2.AdminGroups = %#v, want [{%s}]", dto2.AdminGroups, agA2.ID)
	}
}

// TestSetServerAdminGroupsNonManagerNotFound proves authorizeServer gates
// SetServerAdminGroups FIRST (404-no-leak): a principal who is neither an
// owner, a can_manage_servers co-manager of a linked group, nor system-scope
// gets ErrServerNotFound -- the SAME error an unknown server id gets --
// never a validation error, even with a syntactically-perfect groupIDs body.
func TestSetServerAdminGroupsNonManagerNotFound(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_stranger", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	stranger := token("usr_stranger", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	srv := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_stranger", Name: "Stranger"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srv.ID, ag.ID); err != nil {
		t.Fatalf("SetServerAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, srv.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetServerAdminGroups(e.ctx, stranger, srv.ID, []string{ag.ID}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("SetServerAdminGroups(stranger) = %v, want ErrServerNotFound", err)
	}
	// The owner (still authorized) succeeds on the identical body, proving the
	// 404 above was authorization, not some latent request-shape problem.
	if _, err := e.svc.SetServerAdminGroups(e.ctx, owner, srv.ID, []string{ag.ID}); err != nil {
		t.Fatalf("SetServerAdminGroups(owner) = %v, want nil", err)
	}
	// A genuinely unknown server id is the SAME error.
	if _, err := e.svc.SetServerAdminGroups(e.ctx, sysAdmin, "srv_missing", []string{ag.ID}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("SetServerAdminGroups(unknown id) = %v, want ErrServerNotFound", err)
	}
}

// TestServerAdminGroupCandidatesScoped proves ServerAdminGroupCandidates:
// system-scope gets EVERY admin-tier group (incl. one the caller has zero
// relationship to); a non-system caller gets exactly the groups
// serverManageGroupIDs returns (own group + a co-managed one, NOT a group
// they are a plain member of without can_manage_servers), each carrying its
// parent id/name.
func TestServerAdminGroupCandidatesScoped(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_pm", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	cms := token("usr_cms", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_cms", "usr_pm")

	ownAG := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "OwnAG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ownAG.ID, "usr_cms", "usr_pm")
	if err := e.svc.PromoteManager(e.ctx, owner, ownAG.ID, "usr_cms", false, false, true, true, true); err != nil {
		t.Fatalf("promote usr_cms: %v", err)
	}
	// A THIRD admin group owner doesn't touch, owned by a different admin
	// entirely -- proves system sees it too.
	e.createUser("usr_other", "admin")
	otherOwner := token("usr_other", "admin")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_other")
	otherAG := e.mustCreateGroup(otherOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "OtherAG", ParentGroupID: sg.ID})

	// system -> ALL admin-tier groups (ownAG + otherAG), each with parent info.
	sysList, err := e.svc.ServerAdminGroupCandidates(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ServerAdminGroupCandidates(system): %v", err)
	}
	if len(sysList) != 2 {
		t.Fatalf("system candidates = %#v, want 2", sysList)
	}
	byID := map[string]AdminGroupCandidateDTO{}
	for _, c := range sysList {
		byID[c.ID] = c
	}
	if byID[ownAG.ID].ParentGroupID != sg.ID || byID[ownAG.ID].ParentGroupName != "SG" {
		t.Fatalf("ownAG candidate = %#v, want parent %s/SG", byID[ownAG.ID], sg.ID)
	}
	if _, ok := byID[otherAG.ID]; !ok {
		t.Fatalf("system candidates missing otherAG entirely unrelated to the caller: %#v", sysList)
	}

	// owner -> exactly {ownAG} (the group they own; otherAG absent).
	ownerList, err := e.svc.ServerAdminGroupCandidates(e.ctx, owner)
	if err != nil {
		t.Fatalf("ServerAdminGroupCandidates(owner): %v", err)
	}
	if len(ownerList) != 1 || ownerList[0].ID != ownAG.ID {
		t.Fatalf("owner candidates = %#v, want [{%s}]", ownerList, ownAG.ID)
	}

	// co-manager (can_manage_servers) -> exactly {ownAG} too.
	cmsList, err := e.svc.ServerAdminGroupCandidates(e.ctx, cms)
	if err != nil {
		t.Fatalf("ServerAdminGroupCandidates(cms): %v", err)
	}
	if len(cmsList) != 1 || cmsList[0].ID != ownAG.ID {
		t.Fatalf("cms candidates = %#v, want [{%s}]", cmsList, ownAG.ID)
	}

	// plain member (usr_pm, no manager row at all) -> empty.
	pmList, err := e.svc.ServerAdminGroupCandidates(e.ctx, token("usr_pm", "admin"))
	if err != nil {
		t.Fatalf("ServerAdminGroupCandidates(pm): %v", err)
	}
	if len(pmList) != 0 {
		t.Fatalf("plain-member candidates = %#v, want empty", pmList)
	}
}

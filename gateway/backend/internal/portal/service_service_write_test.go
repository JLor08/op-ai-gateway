// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Task 4: service WRITE (create gate/validate/persist + linkage editing +
// candidates endpoint), spec 2026-08-10 -- mirrors service_server_write_test.go
// (Phase B), keyed on can_manage_services / service_admin_groups. ----------

// TestCreateServiceGateRequiresManageGroupOrSystemScope proves CreateService's
// authorization gate: a non-system principal with NO can_manage_services
// reach into any admin group (serviceManageGroupIDs empty) gets
// ErrServiceForbidden -- even with a syntactically-valid AdminGroupIDs list,
// the gate runs FIRST and never even reaches request-shape validation. A
// system-scope principal is exempt from the gate (though still subject to
// the AdminGroupIDs requirement -- see TestCreateServiceRejectsEmptyAdminGroupIDs).
func TestCreateServiceGateRequiresManageGroupOrSystemScope(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_plain", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	plain := token("usr_plain", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	// usr_plain manages NO admin group (owns none, co-manages none) -> the
	// GATE fires before AdminGroupIDs is even inspected.
	if _, err := e.svc.CreateService(e.ctx, plain, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{ag.ID},
	}); !errors.Is(err, ErrServiceForbidden) {
		t.Fatalf("CreateService(plain, no manage-group) = %v, want ErrServiceForbidden", err)
	}

	// system scope is exempt from the gate (it still needs a valid
	// AdminGroupIDs set -- proven separately).
	if _, err := e.svc.CreateService(e.ctx, sysAdmin, CreateServiceRequest{
		Name: "S2", AdminGroupIDs: []string{ag.ID},
	}); err != nil {
		t.Fatalf("CreateService(system) = %v, want nil", err)
	}
}

// TestCreateServiceCoManagerHappyPath proves the primary success path: a
// can_manage_services CO-MANAGER (not the owner) of an admin group creates a
// service into it; the auto system_group_id equals the group's parent, the
// service_admin_groups join row is persisted (ServiceAdminGroups reads it
// back), and the returned DTO carries admin_groups + system_group_id/_name.
func TestCreateServiceCoManagerHappyPath(t *testing.T) {
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
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cms", false, false, false, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_services): %v", err)
	}

	dto, err := e.svc.CreateService(e.ctx, cms, CreateServiceRequest{
		Name: "Billing Bot", AdminGroupIDs: []string{ag.ID},
	})
	if err != nil {
		t.Fatalf("CreateService(co-manager): %v", err)
	}
	if dto.SystemGroupID != sg.ID || dto.SystemGroupName != "SG" {
		t.Fatalf("dto.SystemGroupID/Name = %q/%q, want %q/SG", dto.SystemGroupID, dto.SystemGroupName, sg.ID)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag.ID || dto.AdminGroups[0].Name != "AG" {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG}]", dto.AdminGroups, ag.ID)
	}
	// The join row is really persisted, not just reflected on the response DTO.
	linked, err := e.routes.ServiceAdminGroups(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("ServiceAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("ServiceAdminGroups(persisted) = %v, want [%s]", linked, ag.ID)
	}
	// SystemGroupID is really persisted on the row too (not just the DTO).
	stored, err := e.routes.ServiceByID(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("ServiceByID: %v", err)
	}
	if stored.SystemGroupID != sg.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q", stored.SystemGroupID, sg.ID)
	}
}

// TestCreateServiceRejectsEmptyAdminGroupIDs proves len(AdminGroupIDs)==0
// (post trim/dedup) is ErrServiceAdminGroupRequired for EVERY scope,
// including system -- a service always needs >=1 linked admin group.
func TestCreateServiceRejectsEmptyAdminGroupIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	// usr_owner owns an admin group (satisfies the CreateService GATE) but the
	// REQUEST supplies no groups -- the request-shape check must still fire.
	e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateService(e.ctx, owner, CreateServiceRequest{
		Name: "S",
	}); !errors.Is(err, ErrServiceAdminGroupRequired) {
		t.Fatalf("CreateService(owner, no groups) = %v, want ErrServiceAdminGroupRequired", err)
	}
	// A whitespace-only entry dedups/trims to empty too.
	if _, err := e.svc.CreateService(e.ctx, owner, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{"  ", ""},
	}); !errors.Is(err, ErrServiceAdminGroupRequired) {
		t.Fatalf("CreateService(owner, blank-only groups) = %v, want ErrServiceAdminGroupRequired", err)
	}
	// system scope is NOT exempt from this requirement.
	if _, err := e.svc.CreateService(e.ctx, sysAdmin, CreateServiceRequest{
		Name: "S",
	}); !errors.Is(err, ErrServiceAdminGroupRequired) {
		t.Fatalf("CreateService(system, no groups) = %v, want ErrServiceAdminGroupRequired", err)
	}
}

// TestCreateServiceRejectsNonManageableGroup proves a NON-system caller
// cannot link a service into an admin-tier group they neither own nor
// co-manage (can_manage_services) -- even though the group genuinely exists
// and is admin-tier -- and that a genuinely non-existent / non-admin-tier id
// is ALSO ErrServiceAdminGroupInvalid (not a different sentinel).
func TestCreateServiceRejectsNonManageableGroup(t *testing.T) {
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
	if _, err := e.svc.CreateService(e.ctx, mine, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{theirAG.ID},
	}); !errors.Is(err, ErrServiceAdminGroupInvalid) {
		t.Fatalf("CreateService(non-manageable group) = %v, want ErrServiceAdminGroupInvalid", err)
	}
	// A completely unknown id.
	if _, err := e.svc.CreateService(e.ctx, mine, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{"ugrp_ghost"},
	}); !errors.Is(err, ErrServiceAdminGroupInvalid) {
		t.Fatalf("CreateService(unknown group) = %v, want ErrServiceAdminGroupInvalid", err)
	}
	// A SYSTEM-tier group id (exists, but not admin-tier).
	if _, err := e.svc.CreateService(e.ctx, mine, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{sg.ID},
	}); !errors.Is(err, ErrServiceAdminGroupInvalid) {
		t.Fatalf("CreateService(system-tier group id) = %v, want ErrServiceAdminGroupInvalid", err)
	}
	// Sanity: mine's OWN group still works (the gate + validation both pass).
	if _, err := e.svc.CreateService(e.ctx, mine, CreateServiceRequest{
		Name: "OK", AdminGroupIDs: []string{myAG.ID},
	}); err != nil {
		t.Fatalf("CreateService(own manageable group) = %v, want nil", err)
	}
}

// TestCreateServiceRejectsDifferingParents proves that when the caller
// specifies MULTIPLE admin groups whose parent (system-tier) groups differ,
// the create is rejected as ErrServiceAdminGroupParentMismatch -- containment
// requires ALL linked groups to share exactly one root.
func TestCreateServiceRejectsDifferingParents(t *testing.T) {
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

	if _, err := e.svc.CreateService(e.ctx, owner, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{ag1.ID, ag2.ID},
	}); !errors.Is(err, ErrServiceAdminGroupParentMismatch) {
		t.Fatalf("CreateService(differing parents) = %v, want ErrServiceAdminGroupParentMismatch", err)
	}
	// Two groups sharing the SAME parent succeed and dedup the containment
	// root to that one parent.
	ag3 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG3", ParentGroupID: sg1.ID})
	dto, err := e.svc.CreateService(e.ctx, owner, CreateServiceRequest{
		Name: "S2", AdminGroupIDs: []string{ag1.ID, ag3.ID},
	})
	if err != nil {
		t.Fatalf("CreateService(same parent, two groups) = %v, want nil", err)
	}
	if dto.SystemGroupID != sg1.ID || len(dto.AdminGroups) != 2 {
		t.Fatalf("dto = %#v, want SystemGroupID=%s + 2 admin_groups", dto, sg1.ID)
	}
}

// TestCreateServiceSystemGroupHintCrossCheck proves the system-scope
// convenience SystemGroupID cross-check (CreateServiceRequest.SystemGroupID):
// a mismatching hint is rejected as ErrServiceAdminGroupParentMismatch even
// though the chosen group's own containment is internally consistent, and a
// non-system caller's hint is IGNORED (never checked -- only system-scope
// pays attention to it, per validateServiceAdminGroupIDs).
func TestCreateServiceSystemGroupHintCrossCheck(t *testing.T) {
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
	if _, err := e.svc.CreateService(e.ctx, sysAdmin, CreateServiceRequest{
		Name: "S", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); !errors.Is(err, ErrServiceAdminGroupParentMismatch) {
		t.Fatalf("CreateService(system, wrong hint) = %v, want ErrServiceAdminGroupParentMismatch", err)
	}
	// system scope: the RIGHT hint succeeds.
	if _, err := e.svc.CreateService(e.ctx, sysAdmin, CreateServiceRequest{
		Name: "S2", AdminGroupIDs: []string{ag.ID}, SystemGroupID: sg.ID,
	}); err != nil {
		t.Fatalf("CreateService(system, correct hint) = %v, want nil", err)
	}
	// non-system caller: a WRONG hint is simply ignored (only checked under
	// system scope) -- the create still succeeds via the owner's own group.
	if _, err := e.svc.CreateService(e.ctx, owner, CreateServiceRequest{
		Name: "S3", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); err != nil {
		t.Fatalf("CreateService(non-system, hint ignored) = %v, want nil", err)
	}
}

// TestSetServiceAdminGroupsAddRemoveDelta proves the linkage editor applies
// exactly the add/remove delta: starting from {AG1}, setting {AG2} adds AG2
// and removes AG1 (both groups share SG's containment root so no other
// invariant is disturbed), and the fresh serviceDTO reflects the new set.
func TestSetServiceAdminGroupsAddRemoveDelta(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg.ID})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})

	svc := mustCreateService(t, e, routing.Service{ID: "svc_delta", Name: "Delta"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, ag1.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup(seed AG1): %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, svc.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup(seed): %v", err)
	}

	dto, err := e.svc.SetServiceAdminGroups(e.ctx, owner, svc.ID, []string{ag2.ID})
	if err != nil {
		t.Fatalf("SetServiceAdminGroups: %v", err)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag2.ID {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG2}]", dto.AdminGroups, ag2.ID)
	}
	linked, err := e.routes.ServiceAdminGroups(e.ctx, svc.ID)
	if err != nil {
		t.Fatalf("ServiceAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag2.ID {
		t.Fatalf("ServiceAdminGroups(after set) = %v, want [%s] (AG1 removed, AG2 added)", linked, ag2.ID)
	}

	// Adding a group already linked is idempotent (no duplicate, no error).
	dto2, err := e.svc.SetServiceAdminGroups(e.ctx, owner, svc.ID, []string{ag2.ID, ag1.ID})
	if err != nil {
		t.Fatalf("SetServiceAdminGroups(add AG1 back): %v", err)
	}
	if len(dto2.AdminGroups) != 2 {
		t.Fatalf("dto2.AdminGroups = %#v, want 2 entries", dto2.AdminGroups)
	}
}

// TestSetServiceAdminGroupsRequiresAtLeastOne proves removing the LAST linked
// admin group (an empty groupIDs) is rejected as ErrServiceAdminGroupRequired
// -- a service can never be left with zero admin groups -- and that the
// rejected call leaves the existing link untouched.
func TestSetServiceAdminGroupsRequiresAtLeastOne(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	svc := mustCreateService(t, e, routing.Service{ID: "svc_last", Name: "Last"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, svc.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetServiceAdminGroups(e.ctx, owner, svc.ID, nil); !errors.Is(err, ErrServiceAdminGroupRequired) {
		t.Fatalf("SetServiceAdminGroups(empty) = %v, want ErrServiceAdminGroupRequired", err)
	}
	linked, err := e.routes.ServiceAdminGroups(e.ctx, svc.ID)
	if err != nil {
		t.Fatalf("ServiceAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("a rejected SetServiceAdminGroups must not persist, got %v", linked)
	}
}

// TestSetServiceAdminGroupsContainmentEnforced proves the new set must still
// share one parent -- an owner who co-manages TWO admin groups with
// DIFFERENT parents cannot link a service to both.
func TestSetServiceAdminGroupsContainmentEnforced(t *testing.T) {
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

	svc := mustCreateService(t, e, routing.Service{ID: "svc_contain", Name: "Contain"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, ag1.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, svc.ID, sg1.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetServiceAdminGroups(e.ctx, owner, svc.ID, []string{ag1.ID, ag2.ID}); !errors.Is(err, ErrServiceAdminGroupParentMismatch) {
		t.Fatalf("SetServiceAdminGroups(differing parents) = %v, want ErrServiceAdminGroupParentMismatch", err)
	}
}

// TestSetServiceAdminGroupsCannotRelocateContainmentRootCrossTenant proves
// the containment-root-immutability guard (mirrors Phase B fix-round-1
// `b0d54f3`, spec non-goal "Kein Reparenting der System-Gruppe eines
// Services ueber verschiedene Tenants"): a NON-system manager who
// owns/co-manages admin groups in TWO DIFFERENT tenants (AG-A under SG-A,
// AG-B under SG-B) cannot swap an already-grouped service's linked groups
// for ones under the OTHER tenant -- even though the NEW set ([AG-B]) is, by
// itself, perfectly self-consistent (a single group, one parent) and would
// pass validateServiceAdminGroupIDs's own internal "all chosen groups share
// one parent" check in isolation. The service's root must NOT move: it
// stays under SG-A after the rejected call. This guard is SCOPE-INDEPENDENT
// (fires for every principal, including system).
func TestSetServiceAdminGroupsCannotRelocateContainmentRootCrossTenant(t *testing.T) {
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

	svc := mustCreateService(t, e, routing.Service{ID: "svc_crosstenant", Name: "CrossTenant"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, agA.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup(seed AG-A): %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, svc.ID, sgA.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup(seed SG-A): %v", err)
	}

	// The attack: swap AG-A for AG-B -- a self-consistent NEW set (one
	// group, one parent SG-B) that would pass validateServiceAdminGroupIDs's
	// internal consistency check in isolation, but must still be rejected
	// because it would relocate the service's root from SG-A to SG-B.
	if _, err := e.svc.SetServiceAdminGroups(e.ctx, mgr, svc.ID, []string{agB.ID}); !errors.Is(err, ErrServiceAdminGroupParentMismatch) {
		t.Fatalf("SetServiceAdminGroups(cross-tenant swap) = %v, want ErrServiceAdminGroupParentMismatch", err)
	}

	// The root did NOT move: the service is still linked to AG-A under SG-A.
	linked, err := e.routes.ServiceAdminGroups(e.ctx, svc.ID)
	if err != nil {
		t.Fatalf("ServiceAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != agA.ID {
		t.Fatalf("a rejected cross-tenant swap must not persist, ServiceAdminGroups = %v, want [%s]", linked, agA.ID)
	}
	stored, err := e.routes.ServiceByID(e.ctx, svc.ID)
	if err != nil {
		t.Fatalf("ServiceByID: %v", err)
	}
	if stored.SystemGroupID != sgA.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q (root must not relocate)", stored.SystemGroupID, sgA.ID)
	}

	// Positive control: adding a SECOND group under the SAME tenant (AG-A2,
	// also parent SG-A) still succeeds and leaves system_group_id unchanged
	// -- the guard only blocks a DIFFERENT root, not ordinary within-tenant
	// membership changes.
	agA2 := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-A2", ParentGroupID: sgA.ID})
	dto, err := e.svc.SetServiceAdminGroups(e.ctx, mgr, svc.ID, []string{agA.ID, agA2.ID})
	if err != nil {
		t.Fatalf("SetServiceAdminGroups(within-tenant add) = %v, want nil", err)
	}
	if dto.SystemGroupID != sgA.ID {
		t.Fatalf("dto.SystemGroupID = %q after within-tenant add, want unchanged %q", dto.SystemGroupID, sgA.ID)
	}
	if len(dto.AdminGroups) != 2 {
		t.Fatalf("dto.AdminGroups = %#v, want 2 entries (AG-A + AG-A2)", dto.AdminGroups)
	}
}

// TestSetServiceAdminGroupsTokenDelegateNotFound proves authorizeServiceSettings
// gates SetServiceAdminGroups FIRST (404-no-leak): a Token-Delegate (a
// delegate lacking CanManageSettings) can re-link neither -- it gets the SAME
// ErrServiceNotFound an unrelated stranger or an unknown service id gets,
// never a validation error, even with a syntactically-perfect groupIDs body.
func TestSetServiceAdminGroupsTokenDelegateNotFound(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_full", "admin")
	e.createUser("usr_token", "admin")
	e.createUser("usr_stranger", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	full := token("usr_full", "admin")
	tokenDel := token("usr_token", "admin")
	stranger := token("usr_stranger", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_full")
	// AG is owned by usr_full (the Full-Delegate) so the positive control
	// below satisfies BOTH gates: authorizeServiceSettings (Full-Delegate)
	// AND validateServiceAdminGroupIDs's non-system manage-check
	// (serviceManageGroupIDs via ownership) -- the two are orthogonal, and
	// the Token-Delegate/stranger checks below never reach the second gate
	// at all (authorizeServiceSettings 404s them first).
	ag := e.mustCreateGroup(full, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	svc := mustCreateService(t, e, routing.Service{ID: "svc_deleg", Name: "Deleg"})
	if err := e.routes.SetServiceDelegates(e.ctx, svc.ID, []routing.ServiceDelegate{
		{UserID: "usr_full", CanManageSettings: true},
		{UserID: "usr_token", CanManageSettings: false},
	}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup(seed): %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, svc.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup(seed): %v", err)
	}

	// Token-Delegate lacks CanManageSettings -> 404, the SAME error a
	// stranger gets.
	if _, err := e.svc.SetServiceAdminGroups(e.ctx, tokenDel, svc.ID, []string{ag.ID}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("SetServiceAdminGroups(token-delegate) = %v, want ErrServiceNotFound", err)
	}
	if _, err := e.svc.SetServiceAdminGroups(e.ctx, stranger, svc.ID, []string{ag.ID}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("SetServiceAdminGroups(stranger) = %v, want ErrServiceNotFound", err)
	}
	// The Full-Delegate (settings-gated) succeeds on the identical body,
	// proving the 404s above were authorization, not some latent
	// request-shape problem.
	if _, err := e.svc.SetServiceAdminGroups(e.ctx, full, svc.ID, []string{ag.ID}); err != nil {
		t.Fatalf("SetServiceAdminGroups(full-delegate) = %v, want nil", err)
	}
	// A genuinely unknown service id is the SAME error.
	if _, err := e.svc.SetServiceAdminGroups(e.ctx, sysAdmin, "svc_missing", []string{ag.ID}); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("SetServiceAdminGroups(unknown id) = %v, want ErrServiceNotFound", err)
	}
}

// TestServiceAdminGroupCandidatesScoped proves ServiceAdminGroupCandidates:
// system-scope gets EVERY admin-tier group (incl. one the caller has zero
// relationship to); a non-system caller gets exactly the groups
// serviceManageGroupIDs returns (own group + a co-managed one, NOT a group
// they are a plain member of without can_manage_services), each carrying its
// parent id/name.
func TestServiceAdminGroupCandidatesScoped(t *testing.T) {
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
	if err := e.svc.PromoteManager(e.ctx, owner, ownAG.ID, "usr_cms", false, false, false, true, true); err != nil {
		t.Fatalf("promote usr_cms: %v", err)
	}
	// A THIRD admin group owner doesn't touch, owned by a different admin
	// entirely -- proves system sees it too.
	e.createUser("usr_other", "admin")
	otherOwner := token("usr_other", "admin")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_other")
	otherAG := e.mustCreateGroup(otherOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "OtherAG", ParentGroupID: sg.ID})

	// system -> ALL admin-tier groups (ownAG + otherAG), each with parent info.
	sysList, err := e.svc.ServiceAdminGroupCandidates(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ServiceAdminGroupCandidates(system): %v", err)
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
	ownerList, err := e.svc.ServiceAdminGroupCandidates(e.ctx, owner)
	if err != nil {
		t.Fatalf("ServiceAdminGroupCandidates(owner): %v", err)
	}
	if len(ownerList) != 1 || ownerList[0].ID != ownAG.ID {
		t.Fatalf("owner candidates = %#v, want [{%s}]", ownerList, ownAG.ID)
	}

	// co-manager (can_manage_services) -> exactly {ownAG} too.
	cmsList, err := e.svc.ServiceAdminGroupCandidates(e.ctx, cms)
	if err != nil {
		t.Fatalf("ServiceAdminGroupCandidates(cms): %v", err)
	}
	if len(cmsList) != 1 || cmsList[0].ID != ownAG.ID {
		t.Fatalf("cms candidates = %#v, want [{%s}]", cmsList, ownAG.ID)
	}

	// plain member (usr_pm, no manager row at all) -> empty.
	pmList, err := e.svc.ServiceAdminGroupCandidates(e.ctx, token("usr_pm", "admin"))
	if err != nil {
		t.Fatalf("ServiceAdminGroupCandidates(pm): %v", err)
	}
	if len(pmList) != 0 {
		t.Fatalf("plain-member candidates = %#v, want empty", pmList)
	}
}

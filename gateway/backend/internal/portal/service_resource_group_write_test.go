// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Task 4: resource-group WRITE (create gate/validate/persist + linkage
// editing + candidates endpoint), spec 2026-08-11 -- mirrors
// service_server_write_test.go (Phase B) exactly, keyed on "resource group"
// instead of "server", minus the delegate model (a resource group has no
// owner-list/delegate fallback -- admin-group linkage is the only path in,
// besides system scope; see authorizeResourceGroup). --------------------

// TestCreateResourceGroupGateRequiresManageGroupOrSystemScope proves
// CreateResourceGroup's authorization gate: a non-system principal with NO
// can_manage_resources reach into any admin group (resourceManageGroupIDs
// empty) gets ErrResourceGroupForbidden -- even with a syntactically-valid
// AdminGroupIDs list, the gate runs FIRST and never even reaches
// request-shape validation. A system-scope principal is exempt from the gate
// (though still subject to the AdminGroupIDs requirement).
func TestCreateResourceGroupGateRequiresManageGroupOrSystemScope(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_plain", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	plain := token("usr_plain", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	// usr_plain manages NO admin group (owns none, co-manages none) -> the
	// GATE fires before AdminGroupIDs is even inspected.
	if _, err := e.svc.CreateResourceGroup(e.ctx, plain, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{ag.ID},
	}); !errors.Is(err, ErrResourceGroupForbidden) {
		t.Fatalf("CreateResourceGroup(plain, no manage-group) = %v, want ErrResourceGroupForbidden", err)
	}

	// system scope is exempt from the gate (it still needs a valid
	// AdminGroupIDs set -- proven separately).
	if _, err := e.svc.CreateResourceGroup(e.ctx, sysAdmin, CreateResourceGroupRequest{
		Name: "RG2", AdminGroupIDs: []string{ag.ID},
	}); err != nil {
		t.Fatalf("CreateResourceGroup(system) = %v, want nil", err)
	}
}

// TestCreateResourceGroupCoManagerHappyPath proves the primary success path:
// a can_manage_resources CO-MANAGER (not the owner) of an admin group creates
// a resource group into it; the auto system_group_id equals the group's
// parent, the resource_group_admin_groups join row is persisted
// (ResourceGroupAdminGroups reads it back), the SystemGroupID is really
// persisted on the ROW (not just the DTO, via ResourceGroupByID), and the
// returned DTO carries admin_groups + system_group.
func TestCreateResourceGroupCoManagerHappyPath(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_cmr", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	cmr := token("usr_cmr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_cmr")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ag.ID, "usr_cmr")
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmr", false, false, false, false, true); err != nil {
		t.Fatalf("promote usr_cmr (can_manage_resources): %v", err)
	}

	dto, err := e.svc.CreateResourceGroup(e.ctx, cmr, CreateResourceGroupRequest{
		Name: "Rack A", AdminGroupIDs: []string{ag.ID},
	})
	if err != nil {
		t.Fatalf("CreateResourceGroup(co-manager): %v", err)
	}
	if dto.SystemGroup.ID != sg.ID || dto.SystemGroup.Name != "SG" {
		t.Fatalf("dto.SystemGroup = %#v, want {%s SG}", dto.SystemGroup, sg.ID)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag.ID || dto.AdminGroups[0].Name != "AG" {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG}]", dto.AdminGroups, ag.ID)
	}
	if dto.Status != routing.ServerStatusActive {
		t.Fatalf("dto.Status = %q, want active (default)", dto.Status)
	}
	// The join row is really persisted, not just reflected on the response DTO.
	linked, err := e.routes.ResourceGroupAdminGroups(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("ResourceGroupAdminGroups(persisted) = %v, want [%s]", linked, ag.ID)
	}
	// SystemGroupID is really persisted on the ROW too (not just the DTO).
	stored, err := e.routes.ResourceGroupByID(e.ctx, dto.ID)
	if err != nil {
		t.Fatalf("ResourceGroupByID: %v", err)
	}
	if stored.SystemGroupID != sg.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q", stored.SystemGroupID, sg.ID)
	}
}

// TestCreateResourceGroupRejectsEmptyAdminGroupIDs proves
// len(AdminGroupIDs)==0 (post trim/dedup) is ErrResourceGroupAdminGroupRequired
// for EVERY scope, including system -- a resource group always needs >=1
// linked admin group.
func TestCreateResourceGroupRejectsEmptyAdminGroupIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	// usr_owner owns an admin group (satisfies the CreateResourceGroup GATE)
	// but the REQUEST supplies no groups -- the request-shape check must
	// still fire.
	e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateResourceGroup(e.ctx, owner, CreateResourceGroupRequest{
		Name: "RG",
	}); !errors.Is(err, ErrResourceGroupAdminGroupRequired) {
		t.Fatalf("CreateResourceGroup(owner, no groups) = %v, want ErrResourceGroupAdminGroupRequired", err)
	}
	// A whitespace-only entry dedups/trims to empty too.
	if _, err := e.svc.CreateResourceGroup(e.ctx, owner, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{"  ", ""},
	}); !errors.Is(err, ErrResourceGroupAdminGroupRequired) {
		t.Fatalf("CreateResourceGroup(owner, blank-only groups) = %v, want ErrResourceGroupAdminGroupRequired", err)
	}
	// system scope is NOT exempt from this requirement.
	if _, err := e.svc.CreateResourceGroup(e.ctx, sysAdmin, CreateResourceGroupRequest{
		Name: "RG",
	}); !errors.Is(err, ErrResourceGroupAdminGroupRequired) {
		t.Fatalf("CreateResourceGroup(system, no groups) = %v, want ErrResourceGroupAdminGroupRequired", err)
	}
}

// TestCreateResourceGroupRejectsBlankName proves a blank (or whitespace-only)
// Name is ErrResourceGroupValidation -- checked even though the caller
// supplied a syntactically-valid, manageable AdminGroupIDs set.
func TestCreateResourceGroupRejectsBlankName(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	sysAdmin := token("usr_s", "system", "admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateResourceGroup(e.ctx, sysAdmin, CreateResourceGroupRequest{
		Name: "   ", AdminGroupIDs: []string{ag.ID},
	}); !errors.Is(err, ErrResourceGroupValidation) {
		t.Fatalf("CreateResourceGroup(blank name) = %v, want ErrResourceGroupValidation", err)
	}
}

// TestCreateResourceGroupRejectsNonManageableGroup proves a NON-system caller
// cannot link a resource group into an admin-tier group they neither own nor
// co-manage (can_manage_resources) -- even though the group genuinely exists
// and is admin-tier -- and that a genuinely non-existent / non-admin-tier id
// is ALSO ErrResourceGroupAdminGroupInvalid (not a different sentinel).
func TestCreateResourceGroupRejectsNonManageableGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_mine", "admin")
	e.createUser("usr_theirs", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	mine := token("usr_mine", "admin")
	theirs := token("usr_theirs", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_mine", "usr_theirs")
	myAG := e.mustCreateGroup(mine, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "MineAG", ParentGroupID: sg.ID})
	theirAG := e.mustCreateGroup(theirs, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "TheirsAG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateResourceGroup(e.ctx, mine, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{theirAG.ID},
	}); !errors.Is(err, ErrResourceGroupAdminGroupInvalid) {
		t.Fatalf("CreateResourceGroup(non-manageable group) = %v, want ErrResourceGroupAdminGroupInvalid", err)
	}
	if _, err := e.svc.CreateResourceGroup(e.ctx, mine, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{"ugrp_ghost"},
	}); !errors.Is(err, ErrResourceGroupAdminGroupInvalid) {
		t.Fatalf("CreateResourceGroup(unknown group) = %v, want ErrResourceGroupAdminGroupInvalid", err)
	}
	// A SYSTEM-tier group id (exists, but not admin-tier).
	if _, err := e.svc.CreateResourceGroup(e.ctx, mine, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{sg.ID},
	}); !errors.Is(err, ErrResourceGroupAdminGroupInvalid) {
		t.Fatalf("CreateResourceGroup(system-tier group id) = %v, want ErrResourceGroupAdminGroupInvalid", err)
	}
	// Sanity: mine's OWN group still works.
	if _, err := e.svc.CreateResourceGroup(e.ctx, mine, CreateResourceGroupRequest{
		Name: "OK", AdminGroupIDs: []string{myAG.ID},
	}); err != nil {
		t.Fatalf("CreateResourceGroup(own manageable group) = %v, want nil", err)
	}
}

// TestCreateResourceGroupRejectsDifferingParents proves that when the caller
// specifies MULTIPLE admin groups whose parent (system-tier) groups differ,
// the create is rejected as ErrResourceGroupAdminGroupParentMismatch.
func TestCreateResourceGroupRejectsDifferingParents(t *testing.T) {
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

	if _, err := e.svc.CreateResourceGroup(e.ctx, owner, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{ag1.ID, ag2.ID},
	}); !errors.Is(err, ErrResourceGroupAdminGroupParentMismatch) {
		t.Fatalf("CreateResourceGroup(differing parents) = %v, want ErrResourceGroupAdminGroupParentMismatch", err)
	}
	ag3 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG3", ParentGroupID: sg1.ID})
	dto, err := e.svc.CreateResourceGroup(e.ctx, owner, CreateResourceGroupRequest{
		Name: "RG2", AdminGroupIDs: []string{ag1.ID, ag3.ID},
	})
	if err != nil {
		t.Fatalf("CreateResourceGroup(same parent, two groups) = %v, want nil", err)
	}
	if dto.SystemGroup.ID != sg1.ID || len(dto.AdminGroups) != 2 {
		t.Fatalf("dto = %#v, want SystemGroup.ID=%s + 2 admin_groups", dto, sg1.ID)
	}
}

// TestCreateResourceGroupSystemGroupHintCrossCheck proves the system-scope
// convenience SystemGroupID cross-check: a mismatching hint is rejected as
// ErrResourceGroupAdminGroupParentMismatch, and a non-system caller's hint is
// IGNORED (never checked).
func TestCreateResourceGroupSystemGroupHintCrossCheck(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	otherSG := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "OtherSG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	if _, err := e.svc.CreateResourceGroup(e.ctx, sysAdmin, CreateResourceGroupRequest{
		Name: "RG", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); !errors.Is(err, ErrResourceGroupAdminGroupParentMismatch) {
		t.Fatalf("CreateResourceGroup(system, wrong hint) = %v, want ErrResourceGroupAdminGroupParentMismatch", err)
	}
	if _, err := e.svc.CreateResourceGroup(e.ctx, sysAdmin, CreateResourceGroupRequest{
		Name: "RG2", AdminGroupIDs: []string{ag.ID}, SystemGroupID: sg.ID,
	}); err != nil {
		t.Fatalf("CreateResourceGroup(system, correct hint) = %v, want nil", err)
	}
	if _, err := e.svc.CreateResourceGroup(e.ctx, owner, CreateResourceGroupRequest{
		Name: "RG3", AdminGroupIDs: []string{ag.ID}, SystemGroupID: otherSG.ID,
	}); err != nil {
		t.Fatalf("CreateResourceGroup(non-system, hint ignored) = %v, want nil", err)
	}
}

// TestSetResourceGroupAdminGroupsAddRemoveDelta proves the linkage editor
// applies exactly the add/remove delta.
func TestSetResourceGroupAdminGroupsAddRemoveDelta(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1", ParentGroupID: sg.ID})
	ag2 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_delta", Name: "Delta", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag1.ID)
	if err := e.routes.UpdateResourceGroupSystemGroup(e.ctx, rg.ID, sg.ID); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup(seed): %v", err)
	}

	dto, err := e.svc.SetResourceGroupAdminGroups(e.ctx, owner, rg.ID, []string{ag2.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupAdminGroups: %v", err)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag2.ID {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG2}]", dto.AdminGroups, ag2.ID)
	}
	linked, err := e.routes.ResourceGroupAdminGroups(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag2.ID {
		t.Fatalf("ResourceGroupAdminGroups(after set) = %v, want [%s] (AG1 removed, AG2 added)", linked, ag2.ID)
	}

	// Adding a group already linked is idempotent (no duplicate, no error).
	dto2, err := e.svc.SetResourceGroupAdminGroups(e.ctx, owner, rg.ID, []string{ag2.ID, ag1.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupAdminGroups(add AG1 back): %v", err)
	}
	if len(dto2.AdminGroups) != 2 {
		t.Fatalf("dto2.AdminGroups = %#v, want 2 entries", dto2.AdminGroups)
	}
}

// TestSetResourceGroupAdminGroupsRequiresAtLeastOne proves removing the LAST
// linked admin group (an empty groupIDs) is rejected as
// ErrResourceGroupAdminGroupRequired, and the rejected call leaves the
// existing link untouched.
func TestSetResourceGroupAdminGroupsRequiresAtLeastOne(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_last", Name: "Last", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)
	if err := e.routes.UpdateResourceGroupSystemGroup(e.ctx, rg.ID, sg.ID); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetResourceGroupAdminGroups(e.ctx, owner, rg.ID, nil); !errors.Is(err, ErrResourceGroupAdminGroupRequired) {
		t.Fatalf("SetResourceGroupAdminGroups(empty) = %v, want ErrResourceGroupAdminGroupRequired", err)
	}
	linked, err := e.routes.ResourceGroupAdminGroups(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != ag.ID {
		t.Fatalf("a rejected SetResourceGroupAdminGroups must not persist, got %v", linked)
	}
}

// TestSetResourceGroupAdminGroupsCannotRelocateContainmentRootCrossTenant
// proves the containment-root-immutability guard (scope-INDEPENDENT): a
// NON-system manager who owns/co-manages admin groups in TWO DIFFERENT
// tenants (AG-A under SG-A, AG-B under SG-B) cannot swap an
// already-grouped resource group's linked groups for ones under the OTHER
// tenant -- even though the NEW set ([AG-B]) is, by itself, perfectly
// self-consistent (a single group, one parent) and would pass
// validateResourceGroupAdminGroupIDs's own internal "all chosen groups share
// one parent" check in isolation. The root must NOT move.
func TestSetResourceGroupAdminGroupsCannotRelocateContainmentRootCrossTenant(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_mgr", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	mgr := token("usr_mgr", "admin")

	sgA := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-A"})
	sgB := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-B"})
	e.mustAddMembers(sysAdmin, sgA.ID, "usr_mgr")
	e.mustAddMembers(sysAdmin, sgB.ID, "usr_mgr")
	agA := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-A", ParentGroupID: sgA.ID})
	agB := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-B", ParentGroupID: sgB.ID})

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_crosstenant", Name: "CrossTenant", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, agA.ID)
	if err := e.routes.UpdateResourceGroupSystemGroup(e.ctx, rg.ID, sgA.ID); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup(seed SG-A): %v", err)
	}

	if _, err := e.svc.SetResourceGroupAdminGroups(e.ctx, mgr, rg.ID, []string{agB.ID}); !errors.Is(err, ErrResourceGroupAdminGroupParentMismatch) {
		t.Fatalf("SetResourceGroupAdminGroups(cross-tenant swap) = %v, want ErrResourceGroupAdminGroupParentMismatch", err)
	}

	linked, err := e.routes.ResourceGroupAdminGroups(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroups: %v", err)
	}
	if len(linked) != 1 || linked[0] != agA.ID {
		t.Fatalf("a rejected cross-tenant swap must not persist, ResourceGroupAdminGroups = %v, want [%s]", linked, agA.ID)
	}
	stored, err := e.routes.ResourceGroupByID(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupByID: %v", err)
	}
	if stored.SystemGroupID != sgA.ID {
		t.Fatalf("stored.SystemGroupID = %q, want %q (root must not relocate)", stored.SystemGroupID, sgA.ID)
	}

	// Positive control: adding a SECOND group under the SAME tenant still
	// succeeds and leaves system_group_id unchanged.
	agA2 := e.mustCreateGroup(mgr, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-A2", ParentGroupID: sgA.ID})
	dto, err := e.svc.SetResourceGroupAdminGroups(e.ctx, mgr, rg.ID, []string{agA.ID, agA2.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupAdminGroups(within-tenant add) = %v, want nil", err)
	}
	if dto.SystemGroup.ID != sgA.ID {
		t.Fatalf("dto.SystemGroup.ID = %q after within-tenant add, want unchanged %q", dto.SystemGroup.ID, sgA.ID)
	}
}

// TestSetResourceGroupAdminGroupsNonManagerNotFound proves
// authorizeResourceGroup gates SetResourceGroupAdminGroups FIRST
// (404-no-leak): a principal who is neither an owner, a can_manage_resources
// co-manager of a linked group, nor system-scope gets
// ErrResourceGroupNotFound -- the SAME error an unknown resource-group id
// gets -- never a validation error, even with a syntactically-perfect
// groupIDs body.
func TestSetResourceGroupAdminGroupsNonManagerNotFound(t *testing.T) {
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

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_stranger", Name: "Stranger", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)
	if err := e.routes.UpdateResourceGroupSystemGroup(e.ctx, rg.ID, sg.ID); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup(seed): %v", err)
	}

	if _, err := e.svc.SetResourceGroupAdminGroups(e.ctx, stranger, rg.ID, []string{ag.ID}); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("SetResourceGroupAdminGroups(stranger) = %v, want ErrResourceGroupNotFound", err)
	}
	if _, err := e.svc.SetResourceGroupAdminGroups(e.ctx, owner, rg.ID, []string{ag.ID}); err != nil {
		t.Fatalf("SetResourceGroupAdminGroups(owner) = %v, want nil", err)
	}
	if _, err := e.svc.SetResourceGroupAdminGroups(e.ctx, sysAdmin, "rgrp_missing", []string{ag.ID}); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("SetResourceGroupAdminGroups(unknown id) = %v, want ErrResourceGroupNotFound", err)
	}
}

// TestResourceGroupAdminGroupCandidatesScoped proves
// ResourceGroupAdminGroupCandidates: system-scope gets EVERY admin-tier
// group (incl. one the caller has zero relationship to); a non-system caller
// gets exactly the groups resourceManageGroupIDs returns.
func TestResourceGroupAdminGroupCandidatesScoped(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_cmr", "admin")
	e.createUser("usr_pm", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	cmr := token("usr_cmr", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_cmr", "usr_pm")

	ownAG := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "OwnAG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ownAG.ID, "usr_cmr", "usr_pm")
	if err := e.svc.PromoteManager(e.ctx, owner, ownAG.ID, "usr_cmr", false, false, false, false, true); err != nil {
		t.Fatalf("promote usr_cmr: %v", err)
	}
	e.createUser("usr_other", "admin")
	otherOwner := token("usr_other", "admin")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_other")
	otherAG := e.mustCreateGroup(otherOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "OtherAG", ParentGroupID: sg.ID})

	sysList, err := e.svc.ResourceGroupAdminGroupCandidates(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroupCandidates(system): %v", err)
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

	ownerList, err := e.svc.ResourceGroupAdminGroupCandidates(e.ctx, owner)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroupCandidates(owner): %v", err)
	}
	if len(ownerList) != 1 || ownerList[0].ID != ownAG.ID {
		t.Fatalf("owner candidates = %#v, want [{%s}]", ownerList, ownAG.ID)
	}

	cmrList, err := e.svc.ResourceGroupAdminGroupCandidates(e.ctx, cmr)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroupCandidates(cmr): %v", err)
	}
	if len(cmrList) != 1 || cmrList[0].ID != ownAG.ID {
		t.Fatalf("cmr candidates = %#v, want [{%s}]", cmrList, ownAG.ID)
	}

	pmList, err := e.svc.ResourceGroupAdminGroupCandidates(e.ctx, token("usr_pm", "admin"))
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroupCandidates(pm): %v", err)
	}
	if len(pmList) != 0 {
		t.Fatalf("plain-member candidates = %#v, want empty", pmList)
	}
}

// TestUpdateResourceGroupNameStatus proves UpdateResourceGroup: authorized
// via authorizeResourceGroup (a stranger gets ErrResourceGroupNotFound), a
// blank name is rejected, and a partial update (status only) leaves the
// unset field (name) unchanged.
func TestUpdateResourceGroupNameStatus(t *testing.T) {
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

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_upd", Name: "Original", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)

	if _, err := e.svc.UpdateResourceGroup(e.ctx, stranger, rg.ID, UpdateResourceGroupRequest{}); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("UpdateResourceGroup(stranger) = %v, want ErrResourceGroupNotFound", err)
	}

	blank := "   "
	if _, err := e.svc.UpdateResourceGroup(e.ctx, owner, rg.ID, UpdateResourceGroupRequest{Name: &blank}); !errors.Is(err, ErrResourceGroupValidation) {
		t.Fatalf("UpdateResourceGroup(blank name) = %v, want ErrResourceGroupValidation", err)
	}

	disabled := routing.ServerStatusDisabled
	dto, err := e.svc.UpdateResourceGroup(e.ctx, owner, rg.ID, UpdateResourceGroupRequest{Status: &disabled})
	if err != nil {
		t.Fatalf("UpdateResourceGroup(status only) = %v, want nil", err)
	}
	if dto.Name != "Original" {
		t.Fatalf("dto.Name = %q, want unchanged %q (name not supplied)", dto.Name, "Original")
	}
	if dto.Status != routing.ServerStatusDisabled {
		t.Fatalf("dto.Status = %q, want disabled", dto.Status)
	}

	renamed := "Renamed"
	dto2, err := e.svc.UpdateResourceGroup(e.ctx, owner, rg.ID, UpdateResourceGroupRequest{Name: &renamed})
	if err != nil {
		t.Fatalf("UpdateResourceGroup(name only) = %v, want nil", err)
	}
	if dto2.Name != "Renamed" {
		t.Fatalf("dto2.Name = %q, want Renamed", dto2.Name)
	}
	if dto2.Status != routing.ServerStatusDisabled {
		t.Fatalf("dto2.Status = %q, want unchanged disabled (status not supplied)", dto2.Status)
	}
}

// TestDeleteResourceGroupAuthzAndCascade proves DeleteResourceGroup is gated
// by authorizeResourceGroup (a stranger/unknown id gets
// ErrResourceGroupNotFound) and, once authorized, actually removes the row
// (a subsequent ResourceGroupByID errors) along with its admin-group and
// server join rows (the FK cascade).
func TestDeleteResourceGroupAuthzAndCascade(t *testing.T) {
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

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_del", Name: "Del", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)
	srv := e.mustCreateServer("srv_del", "Server Del")
	e.mustLinkResourceGroupServer(rg.ID, srv.ID)

	if err := e.svc.DeleteResourceGroup(e.ctx, stranger, rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("DeleteResourceGroup(stranger) = %v, want ErrResourceGroupNotFound", err)
	}
	// Not yet deleted -- still there for the owner.
	if err := e.svc.DeleteResourceGroup(e.ctx, owner, rg.ID); err != nil {
		t.Fatalf("DeleteResourceGroup(owner) = %v, want nil", err)
	}
	if _, err := e.routes.ResourceGroupByID(e.ctx, rg.ID); err == nil {
		t.Fatalf("ResourceGroupByID after delete = nil error, want ErrNotFound")
	}
	linked, err := e.routes.ResourceGroupAdminGroups(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupAdminGroups after delete: %v", err)
	}
	if len(linked) != 0 {
		t.Fatalf("ResourceGroupAdminGroups after delete = %v, want empty (FK cascade)", linked)
	}
	servers, err := e.routes.ResourceGroupServers(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers after delete: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("ResourceGroupServers after delete = %v, want empty (FK cascade)", servers)
	}
	// A genuinely unknown id is the same error.
	if err := e.svc.DeleteResourceGroup(e.ctx, sysAdmin, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("DeleteResourceGroup(unknown id) = %v, want ErrResourceGroupNotFound", err)
	}
}

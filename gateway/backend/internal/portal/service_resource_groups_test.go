// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
	"testing"
)

// mustCreateResourceGroup is a test-only shortcut that fails the test on
// error, mirroring mustCreateGroup/mustAddMembers's pattern.
func (e *groupTestEnv) mustCreateResourceGroup(rg routing.ResourceGroup) routing.ResourceGroup {
	e.t.Helper()
	if err := e.routes.CreateResourceGroup(e.ctx, rg); err != nil {
		e.t.Fatalf("CreateResourceGroup(%+v): %v", rg, err)
	}
	return rg
}

// mustLinkResourceGroupAdminGroup fails the test on error.
func (e *groupTestEnv) mustLinkResourceGroupAdminGroup(rgID, groupID string) {
	e.t.Helper()
	if err := e.routes.SetResourceGroupAdminGroup(e.ctx, rgID, groupID); err != nil {
		e.t.Fatalf("SetResourceGroupAdminGroup(%s, %s): %v", rgID, groupID, err)
	}
}

// mustLinkResourceGroupServer fails the test on error.
func (e *groupTestEnv) mustLinkResourceGroupServer(rgID, serverID string) {
	e.t.Helper()
	if err := e.routes.SetResourceGroupServer(e.ctx, rgID, serverID); err != nil {
		e.t.Fatalf("SetResourceGroupServer(%s, %s): %v", rgID, serverID, err)
	}
}

// mustCreateServer is a minimal AI-server fixture helper for resource-group
// tests -- routing.AIServer needs only id/name/status/timestamps to satisfy
// AIServerByID/ResourceGroupServers/serverDTO's own callers.
func (e *groupTestEnv) mustCreateServer(id, name string) routing.AIServer {
	e.t.Helper()
	srv := routing.AIServer{
		ID: id, Name: name, Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.routes.CreateAIServer(e.ctx, srv); err != nil {
		e.t.Fatalf("CreateAIServer(%s): %v", id, err)
	}
	return srv
}

// TestResourceManageGroupIDs mirrors TestServerManageGroupIDs (Phase B): an
// owner of an admin group is always included; a co-manager is included ONLY
// when their stored CanManageResources flag is true; a co-manager with every
// OTHER flag (users/group/servers/services) but not resources contributes
// nothing.
func TestResourceManageGroupIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr_with", "admin")
	e.createUser("usr_mgr_without", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr_with", "usr_mgr_without")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG"})
	e.mustAddMembers(owner, ag.ID, "usr_mgr_with", "usr_mgr_without")

	// usr_mgr_with: every OTHER flag true, resources true.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr_with", true, true, true, true, true); err != nil {
		t.Fatalf("PromoteManager(with): %v", err)
	}
	// usr_mgr_without: every OTHER flag true, resources FALSE -- proves the
	// facet is independent (mirrors CanManageServers/CanManageServices).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_mgr_without", true, true, true, true, false); err != nil {
		t.Fatalf("PromoteManager(without): %v", err)
	}

	ownerIDs, err := e.svc.resourceManageGroupIDs(e.ctx, owner)
	if err != nil {
		t.Fatalf("resourceManageGroupIDs(owner): %v", err)
	}
	if !ownerIDs[ag.ID] {
		t.Fatalf("owner resourceManageGroupIDs = %v, want %s present (owner)", ownerIDs, ag.ID)
	}

	withIDs, err := e.svc.resourceManageGroupIDs(e.ctx, token("usr_mgr_with", "admin"))
	if err != nil {
		t.Fatalf("resourceManageGroupIDs(with): %v", err)
	}
	if !withIDs[ag.ID] {
		t.Fatalf("mgr_with resourceManageGroupIDs = %v, want %s present (can_manage_resources=true)", withIDs, ag.ID)
	}

	withoutIDs, err := e.svc.resourceManageGroupIDs(e.ctx, token("usr_mgr_without", "admin"))
	if err != nil {
		t.Fatalf("resourceManageGroupIDs(without): %v", err)
	}
	if withoutIDs[ag.ID] {
		t.Fatalf("mgr_without resourceManageGroupIDs = %v, want %s ABSENT (can_manage_resources=false despite every other flag true)", withoutIDs, ag.ID)
	}
	if len(withoutIDs) != 0 {
		t.Fatalf("mgr_without resourceManageGroupIDs = %v, want empty", withoutIDs)
	}

	// A plain member (never promoted) contributes nothing either. Must be
	// added to the parent system group first -- AddGroupMembers requires the
	// invitee be VISIBLE to the actor, and an admin's visibility is scoped to
	// their own system groups' membership.
	e.createUser("usr_plain", "user")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_plain")
	e.mustAddMembers(owner, ag.ID, "usr_plain")
	plainIDs, err := e.svc.resourceManageGroupIDs(e.ctx, token("usr_plain"))
	if err != nil {
		t.Fatalf("resourceManageGroupIDs(plain): %v", err)
	}
	if len(plainIDs) != 0 {
		t.Fatalf("plain member resourceManageGroupIDs = %v, want empty", plainIDs)
	}
}

// resourceGroupAuthzFixture wires: system_admin S; admin group AG1 owned by
// usr_owner (parent system group SG); a resource group RG linked to AG1; a
// SIBLING admin group AG2 (also under SG) that is NOT linked to RG, owned by
// usr_other_owner.
type resourceGroupAuthzFixture struct {
	sysAdmin auth.Token
	owner    auth.Token
	rg       routing.ResourceGroup
	ag1ID    string
	ag2ID    string
}

func newResourceGroupAuthzFixture(e *groupTestEnv) resourceGroupAuthzFixture {
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_mgr_with", "admin")
	e.createUser("usr_mgr_without", "admin")
	e.createUser("usr_other_owner", "admin")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner", "admin")
	otherOwner := token("usr_other_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_mgr_with", "usr_mgr_without", "usr_other_owner")
	ag1 := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG1"})
	e.mustAddMembers(owner, ag1.ID, "usr_mgr_with", "usr_mgr_without")
	ag2 := e.mustCreateGroup(otherOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2"})

	if err := e.svc.PromoteManager(e.ctx, owner, ag1.ID, "usr_mgr_with", true, true, true, true, true); err != nil {
		e.t.Fatalf("PromoteManager(with): %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag1.ID, "usr_mgr_without", true, true, true, true, false); err != nil {
		e.t.Fatalf("PromoteManager(without): %v", err)
	}

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_x", Name: "RG", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag1.ID)

	return resourceGroupAuthzFixture{sysAdmin: sysAdmin, owner: owner, rg: rg, ag1ID: ag1.ID, ag2ID: ag2.ID}
}

// TestAuthorizeResourceGroup_Matrix drives the full authorization matrix
// from the task brief: system scope; owner of the linked admin group;
// can_manage_resources co-manager of the linked group; a co-manager of the
// SAME group WITHOUT the flag; a manager of a DIFFERENT (unlinked) admin
// group; and an unknown resource-group id. Every rejection must be
// ErrResourceGroupNotFound (404-no-leak), never a distinct "forbidden" error.
func TestAuthorizeResourceGroup_Matrix(t *testing.T) {
	e := newGroupTestEnv(t)
	fx := newResourceGroupAuthzFixture(e)

	if _, err := e.svc.authorizeResourceGroup(e.ctx, fx.sysAdmin, fx.rg.ID); err != nil {
		t.Fatalf("system scope: got %v, want nil", err)
	}
	if _, err := e.svc.authorizeResourceGroup(e.ctx, fx.owner, fx.rg.ID); err != nil {
		t.Fatalf("owner of linked group: got %v, want nil", err)
	}
	if _, err := e.svc.authorizeResourceGroup(e.ctx, token("usr_mgr_with", "admin"), fx.rg.ID); err != nil {
		t.Fatalf("can_manage_resources co-manager of linked group: got %v, want nil", err)
	}
	if _, err := e.svc.authorizeResourceGroup(e.ctx, token("usr_mgr_without", "admin"), fx.rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("co-manager WITHOUT can_manage_resources: got %v, want ErrResourceGroupNotFound", err)
	}
	// usr_other_owner manages AG2 (owner), which is NOT linked to rg.
	if _, err := e.svc.authorizeResourceGroup(e.ctx, token("usr_other_owner", "admin"), fx.rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("manager of a different admin group: got %v, want ErrResourceGroupNotFound", err)
	}
	if _, err := e.svc.authorizeResourceGroup(e.ctx, fx.sysAdmin, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("unknown id (even for system scope): got %v, want ErrResourceGroupNotFound", err)
	}
	// A stranger with no group relation at all gets the identical no-leak error.
	e.createUser("usr_stranger", "admin")
	if _, err := e.svc.authorizeResourceGroup(e.ctx, token("usr_stranger", "admin"), fx.rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("stranger: got %v, want ErrResourceGroupNotFound", err)
	}
}

// TestListResourceGroups_SystemSeesAll proves the system-scope branch
// enumerates every resource group unconditionally, regardless of any
// admin-group linkage.
func TestListResourceGroups_SystemSeesAll(t *testing.T) {
	e := newGroupTestEnv(t)
	fx := newResourceGroupAuthzFixture(e)
	// A second, wholly unlinked resource group -- system must still see it.
	e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_y", Name: "Unlinked", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})

	out, err := e.svc.ListResourceGroups(e.ctx, fx.sysAdmin)
	if err != nil {
		t.Fatalf("ListResourceGroups(system): %v", err)
	}
	ids := dtoIDs(out)
	sort.Strings(ids)
	want := []string{"rgrp_x", "rgrp_y"}
	if len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] {
		t.Fatalf("ListResourceGroups(system) ids = %v, want %v", ids, want)
	}
}

// TestListResourceGroups_ByAdminGroups proves the non-system branch is
// scoped to ResourceGroupsByAdminGroups(resourceManageGroupIDs), deduped by
// id: the owner + the can_manage_resources co-manager both see the linked
// resource group; a co-manager without the flag, and a manager of a
// different group entirely, see none.
func TestListResourceGroups_ByAdminGroups(t *testing.T) {
	e := newGroupTestEnv(t)
	fx := newResourceGroupAuthzFixture(e)
	// An unlinked resource group must NOT surface for a non-system caller.
	e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_y", Name: "Unlinked", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})

	out, err := e.svc.ListResourceGroups(e.ctx, fx.owner)
	if err != nil {
		t.Fatalf("ListResourceGroups(owner): %v", err)
	}
	if ids := dtoIDs(out); len(ids) != 1 || ids[0] != fx.rg.ID {
		t.Fatalf("ListResourceGroups(owner) ids = %v, want [%s]", ids, fx.rg.ID)
	}

	out, err = e.svc.ListResourceGroups(e.ctx, token("usr_mgr_with", "admin"))
	if err != nil {
		t.Fatalf("ListResourceGroups(mgr_with): %v", err)
	}
	if ids := dtoIDs(out); len(ids) != 1 || ids[0] != fx.rg.ID {
		t.Fatalf("ListResourceGroups(mgr_with) ids = %v, want [%s]", ids, fx.rg.ID)
	}

	out, err = e.svc.ListResourceGroups(e.ctx, token("usr_mgr_without", "admin"))
	if err != nil {
		t.Fatalf("ListResourceGroups(mgr_without): %v", err)
	}
	if ids := dtoIDs(out); len(ids) != 0 {
		t.Fatalf("ListResourceGroups(mgr_without) ids = %v, want empty", ids)
	}

	out, err = e.svc.ListResourceGroups(e.ctx, token("usr_other_owner", "admin"))
	if err != nil {
		t.Fatalf("ListResourceGroups(other_owner): %v", err)
	}
	if ids := dtoIDs(out); len(ids) != 0 {
		t.Fatalf("ListResourceGroups(other_owner) ids = %v, want empty", ids)
	}
}

func dtoIDs(list []ResourceGroupDTO) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.ID)
	}
	return out
}

// TestResourceGroupDTO_CarriesLinkage proves resourceGroupDTO resolves the
// linked admin groups (id+name), the containment-root system group
// (id+name), and the member servers (id+name) -- all via best-effort lookups
// that never fail the whole DTO.
func TestResourceGroupDTO_CarriesLinkage(t *testing.T) {
	e := newGroupTestEnv(t)
	fx := newResourceGroupAuthzFixture(e)
	srv1 := e.mustCreateServer("srv_1", "Server One")
	srv2 := e.mustCreateServer("srv_2", "Server Two")
	e.mustLinkResourceGroupServer(fx.rg.ID, srv1.ID)
	e.mustLinkResourceGroupServer(fx.rg.ID, srv2.ID)

	// Set the containment root (Task 3 only reads it -- write is a later
	// task's concern, so we poke the store directly here).
	sgID := e.mustGroupParent(fx.ag1ID)
	if err := e.routes.UpdateResourceGroupSystemGroup(e.ctx, fx.rg.ID, sgID); err != nil {
		t.Fatalf("UpdateResourceGroupSystemGroup: %v", err)
	}
	rg, err := e.routes.ResourceGroupByID(e.ctx, fx.rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupByID: %v", err)
	}

	dto, err := e.svc.resourceGroupDTO(e.ctx, rg)
	if err != nil {
		t.Fatalf("resourceGroupDTO: %v", err)
	}
	if dto.ID != fx.rg.ID || dto.Name != "RG" || dto.Status != routing.ServerStatusActive {
		t.Fatalf("resourceGroupDTO base fields = %+v, want id=%s name=RG status=active", dto, fx.rg.ID)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != fx.ag1ID || dto.AdminGroups[0].Name != "AG1" {
		t.Fatalf("resourceGroupDTO.AdminGroups = %+v, want [{%s AG1}]", dto.AdminGroups, fx.ag1ID)
	}
	if dto.SystemGroup.ID != sgID || dto.SystemGroup.Name != "SG" {
		t.Fatalf("resourceGroupDTO.SystemGroup = %+v, want {%s SG}", dto.SystemGroup, sgID)
	}
	gotServers := map[string]string{}
	for _, s := range dto.Servers {
		gotServers[s.ID] = s.Name
	}
	want := map[string]string{"srv_1": "Server One", "srv_2": "Server Two"}
	if len(gotServers) != len(want) || gotServers["srv_1"] != "Server One" || gotServers["srv_2"] != "Server Two" {
		t.Fatalf("resourceGroupDTO.Servers = %+v, want %v", dto.Servers, want)
	}
}

// TestResourceGroupDTO_EmptyLinkageIsNonNil proves an unlinked resource group
// (no admin groups, no system group, no servers) still returns non-nil
// slices (never JSON null), mirroring serverDTO/serviceDTO's own guarantee.
func TestResourceGroupDTO_EmptyLinkageIsNonNil(t *testing.T) {
	e := newGroupTestEnv(t)
	rg := e.mustCreateResourceGroup(routing.ResourceGroup{ID: "rgrp_bare", Name: "Bare", Status: routing.ServerStatusActive, CreatedAt: e.now, UpdatedAt: e.now})

	dto, err := e.svc.resourceGroupDTO(e.ctx, rg)
	if err != nil {
		t.Fatalf("resourceGroupDTO: %v", err)
	}
	if dto.AdminGroups == nil {
		t.Fatalf("resourceGroupDTO.AdminGroups = nil, want non-nil empty slice")
	}
	if dto.Servers == nil {
		t.Fatalf("resourceGroupDTO.Servers = nil, want non-nil empty slice")
	}
	if len(dto.AdminGroups) != 0 || len(dto.Servers) != 0 {
		t.Fatalf("resourceGroupDTO for a bare group = %+v, want empty AdminGroups/Servers", dto)
	}
	if dto.SystemGroup.ID != "" || dto.SystemGroup.Name != "" {
		t.Fatalf("resourceGroupDTO.SystemGroup = %+v, want zero value", dto.SystemGroup)
	}
}

// mustGroupParent loads groupID and returns its ParentGroupID, failing the
// test on error.
func (e *groupTestEnv) mustGroupParent(groupID string) string {
	e.t.Helper()
	g, err := e.dir.UserGroupByID(e.ctx, groupID)
	if err != nil {
		e.t.Fatalf("mustGroupParent: load %s: %v", groupID, err)
	}
	return g.ParentGroupID
}

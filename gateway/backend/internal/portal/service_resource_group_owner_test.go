// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Server-owner self-service resource-group membership (spec
// 2026-08-11-resource-groups-server-owner-self-service): a SERVER OWNER may
// enter+remove their OWN server into/from resource groups linked to an admin
// group they are a MEMBER of -- needing neither admin nor resource-management
// permission. The grant = strict ServerOwners + linked-admin-group membership +
// same-system-group (on ADD only). ----------------------------------------

// ownerFixture builds SG (system) -> AG (admin, owner usr_owner, parent SG) ->
// RG (linked to AG, so RG.SystemGroupID == SG.ID), plus a PLAIN member
// "usr_member" of AG (added to SG then AG, NOT owner/co-manager -> NOT an RG
// manager) who OWNS server srv_x under SG. Returns the pieces the tests need.
type ownerFixture struct {
	sg, ag UserGroupDTO
	rg     routing.ResourceGroup
	srvX   routing.AIServer
}

func setupOwnerFixture(e *groupTestEnv) ownerFixture {
	e.t.Helper()
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_member", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	agOwner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_member")
	ag := e.mustCreateGroup(agOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	// usr_member is a PLAIN member of AG (owner adds them) -- not owner, not a
	// can_manage_resources co-manager, so resourceManageGroupIDs(usr_member) is
	// empty and usr_member is NOT an RG manager.
	e.mustAddMembers(agOwner, ag.ID, "usr_member")

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_own", Name: "Owned RG", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)

	srvX := e.mustCreateServerWithSystemGroup("srv_x", "Server X", sg.ID)
	e.mustSetServerOwners(srvX.ID, "usr_member")
	return ownerFixture{sg: sg, ag: ag, rg: rg, srvX: srvX}
}

func mustRGServers(e *groupTestEnv, rgID string) []string {
	e.t.Helper()
	got, err := e.routes.ResourceGroupServers(e.ctx, rgID)
	if err != nil {
		e.t.Fatalf("ResourceGroupServers(%s): %v", rgID, err)
	}
	return got
}

// TestServerOwnerResourceGroupsListsEligibleWithMemberFlag: a plain-member owner
// sees the eligible RG with Member=false, joins their server, then sees Member=true.
func TestServerOwnerResourceGroupsListsEligibleWithMemberFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	member := token("usr_member")

	list, err := e.svc.ServerOwnerResourceGroups(e.ctx, member, f.srvX.ID)
	if err != nil {
		t.Fatalf("ServerOwnerResourceGroups: %v", err)
	}
	if len(list) != 1 || list[0].ID != f.rg.ID || list[0].Member {
		t.Fatalf("initial list = %+v, want [{%s Member:false}]", list, f.rg.ID)
	}

	if err := e.svc.AddServerToResourceGroup(e.ctx, member, f.srvX.ID, f.rg.ID); err != nil {
		t.Fatalf("AddServerToResourceGroup: %v", err)
	}
	if got := mustRGServers(e, f.rg.ID); len(got) != 1 || got[0] != f.srvX.ID {
		t.Fatalf("ResourceGroupServers after join = %v, want [%s]", got, f.srvX.ID)
	}

	list, err = e.svc.ServerOwnerResourceGroups(e.ctx, member, f.srvX.ID)
	if err != nil {
		t.Fatalf("ServerOwnerResourceGroups (2): %v", err)
	}
	if len(list) != 1 || list[0].ID != f.rg.ID || !list[0].Member {
		t.Fatalf("post-join list = %+v, want [{%s Member:true}]", list, f.rg.ID)
	}
}

// TestAddServerToResourceGroupNonOwnerIs404: a caller who is a MEMBER of the
// linked admin group but does NOT own the server gets ErrServerNotFound
// (404-no-leak) on both list and add, and the server is not added.
func TestAddServerToResourceGroupNonOwnerIs404(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	// usr_other is a member of AG (eligible for RG) but does NOT own srv_x.
	e.createUser("usr_other", "admin")
	e.mustAddMembers(token("usr_s", "system", "admin"), f.sg.ID, "usr_other")
	e.mustAddMembers(token("usr_owner", "admin"), f.ag.ID, "usr_other")
	other := token("usr_other")

	if _, err := e.svc.ServerOwnerResourceGroups(e.ctx, other, f.srvX.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("list as non-owner = %v, want ErrServerNotFound", err)
	}
	if err := e.svc.AddServerToResourceGroup(e.ctx, other, f.srvX.ID, f.rg.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("add as non-owner = %v, want ErrServerNotFound", err)
	}
	if got := mustRGServers(e, f.rg.ID); len(got) != 0 {
		t.Fatalf("ResourceGroupServers after rejected add = %v, want empty", got)
	}
}

// TestAddServerToResourceGroupNotMemberIs404NoLeak: an owner who is NOT a member
// of any admin group linked to the RG gets ErrResourceGroupNotFound (404-no-leak,
// indistinguishable from a non-existent group), and the RG is absent from the list.
func TestAddServerToResourceGroupNotMemberIs404NoLeak(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	// usr_stranger owns a server under SG but is a member of NO admin group.
	e.createUser("usr_stranger", "admin")
	srv2 := e.mustCreateServerWithSystemGroup("srv_2", "Server 2", f.sg.ID)
	e.mustSetServerOwners(srv2.ID, "usr_stranger")
	stranger := token("usr_stranger")

	if err := e.svc.AddServerToResourceGroup(e.ctx, stranger, srv2.ID, f.rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("add as non-member = %v, want ErrResourceGroupNotFound", err)
	}
	list, err := e.svc.ServerOwnerResourceGroups(e.ctx, stranger, srv2.ID)
	if err != nil {
		t.Fatalf("list as non-member: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list as non-member = %+v, want empty (RG not eligible)", list)
	}
}

// TestAddServerToResourceGroupCrossSystemGroupIs400: an owner-member whose server
// is in a DIFFERENT system group than the RG gets a 400 mismatch on add, and the
// RG is filtered out of the list (system groups differ).
func TestAddServerToResourceGroupCrossSystemGroupIs400(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	sysAdmin := token("usr_s", "system", "admin")
	sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG2"})
	// srv_y is owned by usr_member (member of AG under SG) but lives under SG2.
	srvY := e.mustCreateServerWithSystemGroup("srv_y", "Server Y", sg2.ID)
	e.mustSetServerOwners(srvY.ID, "usr_member")
	member := token("usr_member")

	if err := e.svc.AddServerToResourceGroup(e.ctx, member, srvY.ID, f.rg.ID); !errors.Is(err, ErrResourceGroupServerSystemGroupMismatch) {
		t.Fatalf("add cross-system-group = %v, want ErrResourceGroupServerSystemGroupMismatch", err)
	}
	list, err := e.svc.ServerOwnerResourceGroups(e.ctx, member, srvY.ID)
	if err != nil {
		t.Fatalf("list cross-system-group: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("list for a cross-system-group server = %+v, want empty (RG filtered by system group)", list)
	}
}

// TestRemoveServerFromResourceGroupOwnerCanLeaveManagerAdded: a server placed in
// the RG by a manager can be removed by its OWNER via the self-service path;
// removal is idempotent (a second remove is a no-op).
func TestRemoveServerFromResourceGroupOwnerCanLeaveManagerAdded(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	member := token("usr_member")

	// Simulate a manager (or system admin) having added srv_x to the RG.
	if err := e.routes.SetResourceGroupServer(e.ctx, f.rg.ID, f.srvX.ID); err != nil {
		t.Fatalf("seed SetResourceGroupServer: %v", err)
	}
	if got := mustRGServers(e, f.rg.ID); len(got) != 1 {
		t.Fatalf("precondition ResourceGroupServers = %v, want 1", got)
	}

	if err := e.svc.RemoveServerFromResourceGroup(e.ctx, member, f.srvX.ID, f.rg.ID); err != nil {
		t.Fatalf("RemoveServerFromResourceGroup: %v", err)
	}
	if got := mustRGServers(e, f.rg.ID); len(got) != 0 {
		t.Fatalf("ResourceGroupServers after leave = %v, want empty", got)
	}
	// Idempotent: removing again is a no-op.
	if err := e.svc.RemoveServerFromResourceGroup(e.ctx, member, f.srvX.ID, f.rg.ID); err != nil {
		t.Fatalf("second RemoveServerFromResourceGroup = %v, want nil (idempotent)", err)
	}
}

// TestServerOwnerMethodsIdempotent: adding twice yields a single membership;
// removing a non-member is a no-op.
func TestServerOwnerMethodsIdempotent(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupOwnerFixture(e)
	member := token("usr_member")

	if err := e.svc.AddServerToResourceGroup(e.ctx, member, f.srvX.ID, f.rg.ID); err != nil {
		t.Fatalf("add #1: %v", err)
	}
	if err := e.svc.AddServerToResourceGroup(e.ctx, member, f.srvX.ID, f.rg.ID); err != nil {
		t.Fatalf("add #2 (idempotent): %v", err)
	}
	if got := mustRGServers(e, f.rg.ID); len(got) != 1 || got[0] != f.srvX.ID {
		t.Fatalf("ResourceGroupServers after double add = %v, want [%s]", got, f.srvX.ID)
	}

	// srv_2 is owned by usr_member but never added -> removing it is a no-op.
	srv2 := e.mustCreateServerWithSystemGroup("srv_2b", "Server 2b", f.sg.ID)
	e.mustSetServerOwners(srv2.ID, "usr_member")
	if err := e.svc.RemoveServerFromResourceGroup(e.ctx, member, srv2.ID, f.rg.ID); err != nil {
		t.Fatalf("remove non-member = %v, want nil (idempotent)", err)
	}
}

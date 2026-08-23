// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Task 5: resource-group SERVER MEMBERSHIP (dual-manage gate +
// same-system-group containment), spec 2026-08-11 -- the one genuinely new
// authorization rule of Resource Groups Phase 1: to ENTER a server into a
// resource group the caller must (1) manage the resource group
// (authorizeResourceGroup), (2) manage the server (authorizeServer,
// service.go's existing Phase B choke-point), AND (3) the server must sit
// under the SAME system group as the resource group
// (server.SystemGroupID == rg.SystemGroupID). Removal needs only (1). -----

// mustCreateServerWithSystemGroup is a resource-group-server-test-only
// AI-server fixture: like mustCreateServer (service_resource_groups_test.go)
// but additionally stamps SystemGroupID directly on the row (routing.AIServer
// is copied by value on CreateAIServer -- see MemoryStore.CreateAIServer --
// so setting the field before the call persists it, exactly like the
// production Phase-B write path UpdateServerSystemGroup would).
func (e *groupTestEnv) mustCreateServerWithSystemGroup(id, name, systemGroupID string) routing.AIServer {
	e.t.Helper()
	srv := routing.AIServer{
		ID: id, Name: name, Status: routing.ServerStatusActive,
		SystemGroupID: systemGroupID,
		CreatedAt:     e.now, UpdatedAt: e.now,
	}
	if err := e.routes.CreateAIServer(e.ctx, srv); err != nil {
		e.t.Fatalf("CreateAIServer(%s): %v", id, err)
	}
	return srv
}

// mustSetServerOwners fails the test on error.
func (e *groupTestEnv) mustSetServerOwners(serverID string, userIDs ...string) {
	e.t.Helper()
	if err := e.routes.SetServerOwners(e.ctx, serverID, userIDs); err != nil {
		e.t.Fatalf("SetServerOwners(%s, %v): %v", serverID, userIDs, err)
	}
}

// mustLinkServerAdminGroup fails the test on error.
func (e *groupTestEnv) mustLinkServerAdminGroup(serverID, groupID string) {
	e.t.Helper()
	if err := e.routes.SetServerAdminGroup(e.ctx, serverID, groupID); err != nil {
		e.t.Fatalf("SetServerAdminGroup(%s, %s): %v", serverID, groupID, err)
	}
}

// setupResourceGroupServerFixture builds SG (system tier) -> AG (admin tier,
// owned by "usr_owner", member "usr_owner") -> RG (linked to AG, so
// RG.SystemGroupID == SG.ID). Returns the three rows.
func setupResourceGroupServerFixture(e *groupTestEnv) (sg, ag UserGroupDTO, rg routing.ResourceGroup) {
	e.t.Helper()
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg = e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner")
	ag = e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	rg = e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_srv", Name: "Servers", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)
	return sg, ag, rg
}

// TestSetResourceGroupServersDualGateRejectsUnmanagedServer proves the FIRST
// half of the dual gate: a caller who manages the resource group (owner of
// AG, linked to RG) but does NOT manage a candidate server -- neither its
// owner nor a co-manager of any admin group it is linked to -- gets
// ErrResourceGroupServerForbidden for the WHOLE call, and the server is NOT
// added. Dropping the authorizeServer check would let this unmanaged server
// slip in.
func TestSetResourceGroupServersDualGateRejectsUnmanagedServer(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	// A server under the SAME system group but owned by a stranger, with no
	// admin-group link at all -- owner has zero reach into it via
	// authorizeServer.
	e.createUser("usr_stranger", "admin")
	srv := e.mustCreateServerWithSystemGroup("srv_unmanaged", "Unmanaged", sg.ID)
	e.mustSetServerOwners(srv.ID, "usr_stranger")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv.ID}); !errors.Is(err, ErrResourceGroupServerForbidden) {
		t.Fatalf("SetResourceGroupServers(unmanaged server) = %v, want ErrResourceGroupServerForbidden", err)
	}
	members, err := e.routes.ResourceGroupServers(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("ResourceGroupServers after rejected call = %v, want empty (server must NOT be added)", members)
	}
}

// TestSetResourceGroupServersOwnerPathSucceeds proves the happy path via the
// SERVER-OWNER branch of authorizeServer: owner both manages RG and owns a
// server under the SAME system group -> the add succeeds and is persisted.
func TestSetResourceGroupServersOwnerPathSucceeds(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	srv := e.mustCreateServerWithSystemGroup("srv_owned", "Owned", sg.ID)
	e.mustSetServerOwners(srv.ID, "usr_owner")

	dto, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupServers(owner path) = %v, want nil", err)
	}
	if len(dto.Servers) != 1 || dto.Servers[0].ID != srv.ID || dto.Servers[0].Name != "Owned" {
		t.Fatalf("dto.Servers = %#v, want [{%s Owned}]", dto.Servers, srv.ID)
	}
	members, err := e.routes.ResourceGroupServers(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers: %v", err)
	}
	if len(members) != 1 || members[0] != srv.ID {
		t.Fatalf("ResourceGroupServers(persisted) = %v, want [%s]", members, srv.ID)
	}
}

// TestSetResourceGroupServersAdminGroupPathSucceeds proves the happy path via
// the ADMIN-GROUP-CO-MANAGER branch of authorizeServer (can_manage_servers,
// NOT can_manage_resources -- a distinct permission flag from the one that
// let the caller manage RG in the first place): "cmr" is a can_manage_servers
// co-manager of AG (linked to the server) and a can_manage_resources
// co-manager of AG (linked to RG) -- both via the SAME AG here, but the two
// checks are genuinely independent gates.
func TestSetResourceGroupServersAdminGroupPathSucceeds(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, ag, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	e.createUser("usr_cmr", "admin")
	sysAdmin := token("usr_s", "system", "admin")
	e.mustAddMembers(sysAdmin, sg.ID, "usr_cmr")
	e.mustAddMembers(owner, ag.ID, "usr_cmr")
	// can_manage_servers=true, can_manage_resources=true (positions 3 and 5).
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmr", false, false, true, false, true); err != nil {
		t.Fatalf("PromoteManager(cmr): %v", err)
	}
	cmr := token("usr_cmr", "admin")

	srv := e.mustCreateServerWithSystemGroup("srv_via_ag", "ViaAG", sg.ID)
	e.mustLinkServerAdminGroup(srv.ID, ag.ID)

	dto, err := e.svc.SetResourceGroupServers(e.ctx, cmr, rg.ID, []string{srv.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupServers(admin-group path) = %v, want nil", err)
	}
	if len(dto.Servers) != 1 || dto.Servers[0].ID != srv.ID {
		t.Fatalf("dto.Servers = %#v, want [{%s ...}]", dto.Servers, srv.ID)
	}
}

// TestSetResourceGroupServersRejectsSystemGroupMismatch proves the SECOND
// half of the dual gate: a server the caller genuinely OWNS (authorizeServer
// passes) but whose SystemGroupID differs from RG's is rejected as
// ErrResourceGroupServerSystemGroupMismatch, and the server is NOT added.
// Dropping this check would let a cross-system-group server slip in.
func TestSetResourceGroupServersRejectsSystemGroupMismatch(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	sysAdmin := token("usr_s", "system", "admin")
	otherSG := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "OtherSG"})
	if otherSG.ID == sg.ID {
		t.Fatalf("otherSG must differ from sg")
	}

	srv := e.mustCreateServerWithSystemGroup("srv_othersys", "OtherSys", otherSG.ID)
	e.mustSetServerOwners(srv.ID, "usr_owner")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv.ID}); !errors.Is(err, ErrResourceGroupServerSystemGroupMismatch) {
		t.Fatalf("SetResourceGroupServers(cross-system-group server) = %v, want ErrResourceGroupServerSystemGroupMismatch", err)
	}
	members, err := e.routes.ResourceGroupServers(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("ResourceGroupServers after rejected call = %v, want empty", members)
	}
}

// TestSetResourceGroupServersRejectsUngroupedServer proves an UNGROUPED
// server (SystemGroupID == "", e.g. a pre-Phase-B legacy row never assigned
// a containment root) is ALSO a system-group mismatch against a grouped RG
// -- "" never equals a non-empty rg.SystemGroupID.
func TestSetResourceGroupServersRejectsUngroupedServer(t *testing.T) {
	e := newGroupTestEnv(t)
	_, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	// mustCreateServer (service_resource_groups_test.go) leaves SystemGroupID
	// at its zero value ("").
	srv := e.mustCreateServer("srv_ungrouped", "Ungrouped")
	e.mustSetServerOwners(srv.ID, "usr_owner")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv.ID}); !errors.Is(err, ErrResourceGroupServerSystemGroupMismatch) {
		t.Fatalf("SetResourceGroupServers(ungrouped server) = %v, want ErrResourceGroupServerSystemGroupMismatch", err)
	}
}

// TestSetResourceGroupServersAddRemoveDelta proves the delta apply: only the
// newly-desired ids are added and only the no-longer-desired current ids are
// removed; re-supplying an already-linked id is idempotent (no error, no
// duplicate).
func TestSetResourceGroupServersAddRemoveDelta(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	srv1 := e.mustCreateServerWithSystemGroup("srv_d1", "D1", sg.ID)
	e.mustSetServerOwners(srv1.ID, "usr_owner")
	srv2 := e.mustCreateServerWithSystemGroup("srv_d2", "D2", sg.ID)
	e.mustSetServerOwners(srv2.ID, "usr_owner")
	srv3 := e.mustCreateServerWithSystemGroup("srv_d3", "D3", sg.ID)
	e.mustSetServerOwners(srv3.ID, "usr_owner")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv1.ID, srv2.ID}); err != nil {
		t.Fatalf("SetResourceGroupServers(seed [srv1,srv2]): %v", err)
	}

	dto, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv2.ID, srv3.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupServers(delta [srv2,srv3]): %v", err)
	}
	if len(dto.Servers) != 2 {
		t.Fatalf("dto.Servers = %#v, want 2 entries (srv1 removed, srv3 added)", dto.Servers)
	}
	gotIDs := map[string]bool{}
	for _, s := range dto.Servers {
		gotIDs[s.ID] = true
	}
	if !gotIDs[srv2.ID] || !gotIDs[srv3.ID] || gotIDs[srv1.ID] {
		t.Fatalf("dto.Servers ids = %v, want exactly {%s,%s}", gotIDs, srv2.ID, srv3.ID)
	}

	// Idempotent re-add of an already-linked id.
	dto2, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, []string{srv2.ID, srv3.ID, srv3.ID})
	if err != nil {
		t.Fatalf("SetResourceGroupServers(idempotent re-add): %v", err)
	}
	if len(dto2.Servers) != 2 {
		t.Fatalf("dto2.Servers = %#v, want still 2 entries (no duplicate)", dto2.Servers)
	}
}

// TestSetResourceGroupServersMultiMembership proves a server may belong to
// MULTIPLE resource groups at once: linking srv into rg2 must NOT remove it
// from rg1.
func TestSetResourceGroupServersMultiMembership(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, ag, rg1 := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	rg2 := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_srv2", Name: "Servers2", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rg2.ID, ag.ID)

	srv := e.mustCreateServerWithSystemGroup("srv_multi", "Multi", sg.ID)
	e.mustSetServerOwners(srv.ID, "usr_owner")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg1.ID, []string{srv.ID}); err != nil {
		t.Fatalf("SetResourceGroupServers(rg1): %v", err)
	}
	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg2.ID, []string{srv.ID}); err != nil {
		t.Fatalf("SetResourceGroupServers(rg2): %v", err)
	}

	rg1Members, err := e.routes.ResourceGroupServers(e.ctx, rg1.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers(rg1): %v", err)
	}
	if len(rg1Members) != 1 || rg1Members[0] != srv.ID {
		t.Fatalf("rg1 members after linking to rg2 = %v, want still [%s] (multi-membership must be preserved)", rg1Members, srv.ID)
	}
	rg2Members, err := e.routes.ResourceGroupServers(e.ctx, rg2.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers(rg2): %v", err)
	}
	if len(rg2Members) != 1 || rg2Members[0] != srv.ID {
		t.Fatalf("rg2 members = %v, want [%s]", rg2Members, srv.ID)
	}
}

// TestSetResourceGroupServersRemovalNeedsOnlyResourceGroupAuthz proves the
// removal path is gated SOLELY by authorizeResourceGroup: a server that was
// linked to RG via a direct store call (bypassing SetResourceGroupServers,
// so ownership is irrelevant) and is now owned by a stranger the caller
// cannot authorizeServer-reach is nonetheless successfully DROPPED when the
// caller's desired set omits it -- removal never re-checks authorizeServer.
func TestSetResourceGroupServersRemovalNeedsOnlyResourceGroupAuthz(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	owner := token("usr_owner", "admin")

	e.createUser("usr_stranger", "admin")
	srv := e.mustCreateServerWithSystemGroup("srv_strangerowned", "StrangerOwned", sg.ID)
	e.mustSetServerOwners(srv.ID, "usr_stranger")
	// Link directly via the store (owner never had authorizeServer reach into
	// this server, so this could not have happened through
	// SetResourceGroupServers itself).
	e.mustLinkResourceGroupServer(rg.ID, srv.ID)

	if _, err := e.svc.SetResourceGroupServers(e.ctx, owner, rg.ID, nil); err != nil {
		t.Fatalf("SetResourceGroupServers(remove unmanaged member) = %v, want nil", err)
	}
	members, err := e.routes.ResourceGroupServers(e.ctx, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServers: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("ResourceGroupServers after removal = %v, want empty", members)
	}
}

// TestSetResourceGroupServersNotFoundForNonManager proves
// authorizeResourceGroup gates SetResourceGroupServers FIRST (404-no-leak): a
// principal with no reach into RG at all gets ErrResourceGroupNotFound, never
// a server-side sentinel, even with a syntactically-valid server id.
func TestSetResourceGroupServersNotFoundForNonManager(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	e.createUser("usr_stranger", "admin")
	stranger := token("usr_stranger", "admin")

	srv := e.mustCreateServerWithSystemGroup("srv_x", "X", sg.ID)
	e.mustSetServerOwners(srv.ID, "usr_stranger")

	if _, err := e.svc.SetResourceGroupServers(e.ctx, stranger, rg.ID, []string{srv.ID}); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("SetResourceGroupServers(stranger) = %v, want ErrResourceGroupNotFound", err)
	}
	if _, err := e.svc.SetResourceGroupServers(e.ctx, stranger, "rgrp_missing", nil); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("SetResourceGroupServers(unknown id) = %v, want ErrResourceGroupNotFound", err)
	}
}

// TestResourceGroupServerCandidatesFilteredBySystemGroup proves
// ResourceGroupServerCandidates: authorizeResourceGroup gates it (a stranger
// gets ErrResourceGroupNotFound); a non-system caller sees exactly the
// servers THEY manage (owner union admin-group co-manager, mirroring
// ListServers) that additionally sit under RG's SAME system group -- a
// manageable server under a DIFFERENT system group is excluded, and an
// unmanaged server under the SAME system group is excluded too.
func TestResourceGroupServerCandidatesFilteredBySystemGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	sg, _, rg := setupResourceGroupServerFixture(e)
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	otherSG := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "OtherSG"})

	sameManaged := e.mustCreateServerWithSystemGroup("srv_same_managed", "SameManaged", sg.ID)
	e.mustSetServerOwners(sameManaged.ID, "usr_owner")

	otherManaged := e.mustCreateServerWithSystemGroup("srv_other_managed", "OtherManaged", otherSG.ID)
	e.mustSetServerOwners(otherManaged.ID, "usr_owner")

	e.createUser("usr_stranger", "admin")
	sameUnmanaged := e.mustCreateServerWithSystemGroup("srv_same_unmanaged", "SameUnmanaged", sg.ID)
	e.mustSetServerOwners(sameUnmanaged.ID, "usr_stranger")

	candidates, err := e.svc.ResourceGroupServerCandidates(e.ctx, owner, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServerCandidates(owner): %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != sameManaged.ID || candidates[0].Name != "SameManaged" {
		t.Fatalf("ResourceGroupServerCandidates(owner) = %#v, want [{%s SameManaged}]", candidates, sameManaged.ID)
	}

	// system-scope sees every server under the same system group,
	// unconditional on ownership/co-management.
	sysCandidates, err := e.svc.ResourceGroupServerCandidates(e.ctx, sysAdmin, rg.ID)
	if err != nil {
		t.Fatalf("ResourceGroupServerCandidates(system): %v", err)
	}
	sysIDs := map[string]bool{}
	for _, c := range sysCandidates {
		sysIDs[c.ID] = true
	}
	if !sysIDs[sameManaged.ID] || !sysIDs[sameUnmanaged.ID] || sysIDs[otherManaged.ID] {
		t.Fatalf("ResourceGroupServerCandidates(system) ids = %v, want {%s,%s} and NOT %s", sysIDs, sameManaged.ID, sameUnmanaged.ID, otherManaged.ID)
	}

	// authorizeResourceGroup gate: a stranger with no reach into rg gets
	// ErrResourceGroupNotFound.
	e.createUser("usr_stranger2", "admin")
	stranger2 := token("usr_stranger2", "admin")
	if _, err := e.svc.ResourceGroupServerCandidates(e.ctx, stranger2, rg.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("ResourceGroupServerCandidates(stranger) = %v, want ErrResourceGroupNotFound", err)
	}
}

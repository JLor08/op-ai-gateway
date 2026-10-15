// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// --- Service-side resource-group self-service (#183 Phase 0, spec §9a): a
// SERVICE MANAGER (a can_manage_services owner/co-manager of an admin group the
// service is linked to) may enter/leave their service into/from the resource
// groups linked to an admin group they are a MEMBER of. It writes the same
// resource_group_provisions rows (ProvisionKindService) the RG-side editor
// writes, one row at a time (never the whole-set replace), and needs neither
// can_manage_resources nor admin scope. ----------------------------------

// svcRGFixture builds:
//
//	SG (system) -> AG  (admin, owner usr_owner; usr_cms = co-manager with ONLY
//	                    can_manage_services; usr_plain = plain member)
//	            -> AG2 (admin, owner usr_owner2; usr_cms2 = co-manager with ONLY
//	                    can_manage_services; svcOther is linked to AG2)
//
//	svcA     linked to AG  (managed by usr_owner / usr_cms)
//	svcOther linked to AG2 (managed by usr_owner2 / usr_cms2)
//	rgReach  linked to AG  -- reachable by every member of AG
//	rgHidden linked to AG2 -- reachable only by members of AG2
type svcRGFixture struct {
	sg, ag, ag2       UserGroupDTO
	svcA, svcOther    routing.Service
	rgReach, rgHidden routing.ResourceGroup
}

func setupSvcRGFixture(e *groupTestEnv) svcRGFixture {
	e.t.Helper()
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_owner2", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_cms2", "admin")
	e.createUser("usr_plain", "admin")
	e.createUser("usr_stranger", "user")
	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	owner2 := token("usr_owner2", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_owner2", "usr_cms", "usr_cms2", "usr_plain")
	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(owner, ag.ID, "usr_cms", "usr_plain")
	ag2 := e.mustCreateGroup(owner2, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})
	e.mustAddMembers(owner2, ag2.ID, "usr_cms2")
	// can_manage_services ONLY: neither can_manage_resources nor anything else,
	// proving the self-service needs no resource-management permission.
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cms", false, false, false, true, false); err != nil {
		e.t.Fatalf("promote usr_cms: %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner2, ag2.ID, "usr_cms2", false, false, false, true, false); err != nil {
		e.t.Fatalf("promote usr_cms2: %v", err)
	}

	svcA := mustCreateService(e.t, e, routing.Service{ID: "svc_a", Name: "Service A", SystemGroupID: sg.ID})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svcA.ID, ag.ID); err != nil {
		e.t.Fatalf("link svc_a: %v", err)
	}
	svcOther := mustCreateService(e.t, e, routing.Service{ID: "svc_other", Name: "Service Other", SystemGroupID: sg.ID})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svcOther.ID, ag2.ID); err != nil {
		e.t.Fatalf("link svc_other: %v", err)
	}

	rgReach := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_reach", Name: "Reachable RG", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rgReach.ID, ag.ID)
	rgHidden := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_hidden", Name: "Hidden RG", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rgHidden.ID, ag2.ID)
	return svcRGFixture{sg: sg, ag: ag, ag2: ag2, svcA: svcA, svcOther: svcOther, rgReach: rgReach, rgHidden: rgHidden}
}

// mustServiceProvisionRGs returns the ids of every resource group provisioned
// for serviceID (ProvisionKindService).
func mustServiceProvisionRGs(e *groupTestEnv, serviceID string) []string {
	e.t.Helper()
	got, err := e.routes.ResourceGroupIDsByProvisionTargets(e.ctx, routing.ProvisionKindService, []string{serviceID})
	if err != nil {
		e.t.Fatalf("ResourceGroupIDsByProvisionTargets(%s): %v", serviceID, err)
	}
	return got
}

// TestServiceResourceGroupsListsReachableWithMemberFlag: a service manager sees
// ONLY the resource groups linked to an admin group they are a member of (the
// unreachable one is absent), joins, sees Member=true, leaves, sees Member=false.
func TestServiceResourceGroupsListsReachableWithMemberFlag(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	cms := token("usr_cms", "admin")

	list, err := e.svc.ServiceResourceGroups(e.ctx, cms, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups: %v", err)
	}
	if len(list) != 1 || list[0].ID != f.rgReach.ID || list[0].Name != f.rgReach.Name || list[0].Member {
		t.Fatalf("initial list = %+v, want [{%s Member:false}] (and no %s)", list, f.rgReach.ID, f.rgHidden.ID)
	}

	if err := e.svc.AddServiceToResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
		t.Fatalf("AddServiceToResourceGroup: %v", err)
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 1 || got[0] != f.rgReach.ID {
		t.Fatalf("service provisions after join = %v, want [%s]", got, f.rgReach.ID)
	}
	list, err = e.svc.ServiceResourceGroups(e.ctx, cms, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups (2): %v", err)
	}
	if len(list) != 1 || list[0].ID != f.rgReach.ID || !list[0].Member {
		t.Fatalf("post-join list = %+v, want [{%s Member:true}]", list, f.rgReach.ID)
	}

	if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
		t.Fatalf("RemoveServiceFromResourceGroup: %v", err)
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 0 {
		t.Fatalf("service provisions after leave = %v, want empty", got)
	}
	list, err = e.svc.ServiceResourceGroups(e.ctx, cms, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups (3): %v", err)
	}
	if len(list) != 1 || list[0].Member {
		t.Fatalf("post-leave list = %+v, want [{%s Member:false}]", list, f.rgReach.ID)
	}
}

// TestServiceResourceGroupsOwnerOfLinkedGroupIsManager: the OWNER of an admin
// group the service is linked to is a service manager too (serviceManageGroupIDs
// includes owners), without any co-manager row.
func TestServiceResourceGroupsOwnerOfLinkedGroupIsManager(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	owner := token("usr_owner", "admin")

	list, err := e.svc.ServiceResourceGroups(e.ctx, owner, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups as owner: %v", err)
	}
	if len(list) != 1 || list[0].ID != f.rgReach.ID {
		t.Fatalf("owner list = %+v, want [%s]", list, f.rgReach.ID)
	}
	if err := e.svc.AddServiceToResourceGroup(e.ctx, owner, f.svcA.ID, f.rgReach.ID); err != nil {
		t.Fatalf("AddServiceToResourceGroup as owner: %v", err)
	}
}

// TestServiceResourceGroupsJoinLeaveKeepsOtherProvisions: the self-service writes
// ONE provision row; it must never clobber the other targets the RG-side editor
// configured (the editor's PUT is a whole-set replace, this path is not).
func TestServiceResourceGroupsJoinLeaveKeepsOtherProvisions(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	cms := token("usr_cms", "admin")
	e.mustProvisionResourceGroup(f.rgReach.ID, routing.ProvisionKindUser, "usr_plain")
	e.mustProvisionResourceGroup(f.rgReach.ID, routing.ProvisionKindService, f.svcOther.ID)

	if err := e.svc.AddServiceToResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
		t.Fatalf("AddServiceToResourceGroup: %v", err)
	}
	assertProvisions := func(stage string, want map[routing.ResourceGroupProvision]bool) {
		t.Helper()
		got, err := e.routes.ResourceGroupProvisions(e.ctx, f.rgReach.ID)
		if err != nil {
			t.Fatalf("%s: ResourceGroupProvisions: %v", stage, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: provisions = %v, want %d rows", stage, got, len(want))
		}
		for _, p := range got {
			if !want[p] {
				t.Fatalf("%s: unexpected provision %+v in %v", stage, p, got)
			}
		}
	}
	assertProvisions("after join", map[routing.ResourceGroupProvision]bool{
		{Kind: routing.ProvisionKindUser, TargetID: "usr_plain"}:      true,
		{Kind: routing.ProvisionKindService, TargetID: f.svcOther.ID}: true,
		{Kind: routing.ProvisionKindService, TargetID: f.svcA.ID}:     true,
	})
	if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
		t.Fatalf("RemoveServiceFromResourceGroup: %v", err)
	}
	assertProvisions("after leave", map[routing.ResourceGroupProvision]bool{
		{Kind: routing.ProvisionKindUser, TargetID: "usr_plain"}:      true,
		{Kind: routing.ProvisionKindService, TargetID: f.svcOther.ID}: true,
	})
}

// TestServiceResourceGroupsIdempotent: joining twice yields one provision row;
// leaving a non-member is a no-op.
func TestServiceResourceGroupsIdempotent(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	cms := token("usr_cms", "admin")

	for range 2 {
		if err := e.svc.AddServiceToResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
			t.Fatalf("AddServiceToResourceGroup: %v", err)
		}
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 1 {
		t.Fatalf("provisions after double join = %v, want exactly 1", got)
	}
	for range 2 {
		if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, cms, f.svcA.ID, f.rgReach.ID); err != nil {
			t.Fatalf("RemoveServiceFromResourceGroup: %v", err)
		}
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 0 {
		t.Fatalf("provisions after double leave = %v, want empty", got)
	}
}

// TestServiceResourceGroupsNonManagerIsDenied: a caller who is a MEMBER of the
// RG's admin group but does not manage the service gets ErrServiceNotFound
// (404-no-leak, the same error a stranger gets) on list/add/remove, and nothing
// is written. Covers: a plain member, a co-manager WITHOUT can_manage_services,
// and a Full-Delegate (delegation is not can_manage_services management).
func TestServiceResourceGroupsNonManagerIsDenied(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	// usr_cmg: co-manager of AG with can_manage_group ONLY (no services flag).
	e.createUser("usr_cmg", "admin")
	e.mustAddMembers(token("usr_s", "system", "admin"), f.sg.ID, "usr_cmg")
	e.mustAddMembers(token("usr_owner", "admin"), f.ag.ID, "usr_cmg")
	if err := e.svc.PromoteManager(e.ctx, token("usr_owner", "admin"), f.ag.ID, "usr_cmg", false, true, false, false, true); err != nil {
		t.Fatalf("promote usr_cmg: %v", err)
	}
	// usr_full: Full-Delegate of svc_a AND a member of AG (so the RG is reachable).
	e.createUser("usr_full", "admin")
	e.mustAddMembers(token("usr_s", "system", "admin"), f.sg.ID, "usr_full")
	e.mustAddMembers(token("usr_owner", "admin"), f.ag.ID, "usr_full")
	if err := e.routes.SetServiceDelegates(e.ctx, f.svcA.ID, []routing.ServiceDelegate{{UserID: "usr_full", CanManageSettings: true}}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}

	for _, who := range []string{"usr_plain", "usr_cmg", "usr_full"} {
		p := token(who, "admin")
		if _, err := e.svc.ServiceResourceGroups(e.ctx, p, f.svcA.ID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s list = %v, want ErrServiceNotFound", who, err)
		}
		if err := e.svc.AddServiceToResourceGroup(e.ctx, p, f.svcA.ID, f.rgReach.ID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s add = %v, want ErrServiceNotFound", who, err)
		}
		if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, p, f.svcA.ID, f.rgReach.ID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s remove = %v, want ErrServiceNotFound", who, err)
		}
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 0 {
		t.Fatalf("provisions after rejected adds = %v, want empty", got)
	}
}

// TestServiceResourceGroupsStrangerGetsNoLeak: a caller outside every group, a
// manager of an UNRELATED service, and an unknown service id are
// indistinguishable (ErrServiceNotFound), and a rejected remove leaves an
// existing provision untouched.
func TestServiceResourceGroupsStrangerGetsNoLeak(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	e.mustProvisionResourceGroup(f.rgReach.ID, routing.ProvisionKindService, f.svcA.ID)

	cases := []struct {
		name      string
		principal string
		serviceID string
	}{
		{"stranger on real service", "usr_stranger", f.svcA.ID},
		{"manager of an unrelated service", "usr_cms2", f.svcA.ID},
		{"manager on unknown service id", "usr_cms", "svc_missing"},
	}
	for _, tc := range cases {
		p := token(tc.principal)
		if _, err := e.svc.ServiceResourceGroups(e.ctx, p, tc.serviceID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s: list = %v, want ErrServiceNotFound", tc.name, err)
		}
		if err := e.svc.AddServiceToResourceGroup(e.ctx, p, tc.serviceID, f.rgReach.ID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s: add = %v, want ErrServiceNotFound", tc.name, err)
		}
		if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, p, tc.serviceID, f.rgReach.ID); !errors.Is(err, ErrServiceNotFound) {
			t.Fatalf("%s: remove = %v, want ErrServiceNotFound", tc.name, err)
		}
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 1 || got[0] != f.rgReach.ID {
		t.Fatalf("provisions after rejected removes = %v, want [%s] untouched", got, f.rgReach.ID)
	}
}

// TestServiceResourceGroupsUnreachableRGIs404NoLeak: a service manager who is NOT
// a member of any admin group the RG is linked to gets ErrResourceGroupNotFound
// (indistinguishable from a non-existent RG id) on add/remove; the RG is absent
// from their list; and a provision that already exists on the hidden RG is
// neither shown nor removable.
func TestServiceResourceGroupsUnreachableRGIs404NoLeak(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	cms := token("usr_cms", "admin")
	// svc_a is already provisioned into the hidden RG (e.g. by a system admin).
	e.mustProvisionResourceGroup(f.rgHidden.ID, routing.ProvisionKindService, f.svcA.ID)

	if err := e.svc.AddServiceToResourceGroup(e.ctx, cms, f.svcA.ID, f.rgHidden.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("add to unreachable RG = %v, want ErrResourceGroupNotFound", err)
	}
	if err := e.svc.AddServiceToResourceGroup(e.ctx, cms, f.svcA.ID, "rgrp_missing"); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("add to missing RG = %v, want ErrResourceGroupNotFound", err)
	}
	if err := e.svc.RemoveServiceFromResourceGroup(e.ctx, cms, f.svcA.ID, f.rgHidden.ID); !errors.Is(err, ErrResourceGroupNotFound) {
		t.Fatalf("remove from unreachable RG = %v, want ErrResourceGroupNotFound", err)
	}
	list, err := e.svc.ServiceResourceGroups(e.ctx, cms, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups: %v", err)
	}
	for _, rg := range list {
		if rg.ID == f.rgHidden.ID {
			t.Fatalf("list leaks the unreachable RG: %+v", list)
		}
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 1 || got[0] != f.rgHidden.ID {
		t.Fatalf("provisions = %v, want [%s] untouched", got, f.rgHidden.ID)
	}
}

// TestServiceResourceGroupsSystemScopeSeesEveryRG: system scope bypasses the
// group gates (mirrors the server-owner path): every RG is listed and joinable.
func TestServiceResourceGroupsSystemScopeSeesEveryRG(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupSvcRGFixture(e)
	sys := token("usr_s", "system", "admin")

	list, err := e.svc.ServiceResourceGroups(e.ctx, sys, f.svcA.ID)
	if err != nil {
		t.Fatalf("ServiceResourceGroups as system: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("system list = %+v, want both resource groups", list)
	}
	// Sorted by name: "Hidden RG" < "Reachable RG".
	if list[0].ID != f.rgHidden.ID || list[1].ID != f.rgReach.ID {
		t.Fatalf("system list order = %+v, want name-sorted [%s %s]", list, f.rgHidden.ID, f.rgReach.ID)
	}
	if err := e.svc.AddServiceToResourceGroup(e.ctx, sys, f.svcA.ID, f.rgHidden.ID); err != nil {
		t.Fatalf("AddServiceToResourceGroup as system: %v", err)
	}
	if got := mustServiceProvisionRGs(e, f.svcA.ID); len(got) != 1 || got[0] != f.rgHidden.ID {
		t.Fatalf("provisions = %v, want [%s]", got, f.rgHidden.ID)
	}
}

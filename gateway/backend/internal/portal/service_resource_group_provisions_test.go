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
)

// --- Resource Groups Phase 2 -- the KEYSTONE usability predicate
// AllowedServerIDs + the resource_provisioning_enforce setting (spec
// 2026-08-12-resource-groups-phase-2-provisioning, Task 3). No role bypass:
// system-scope + owner go through the exact same logic as any other
// principal. ---------------------------------------------------------------

// TestResourceProvisioningEnforceHelper pins the pure KV reader: default
// false on an absent/blank/unparseable value, explicit true/false read back
// verbatim.
func TestResourceProvisioningEnforceHelper(t *testing.T) {
	cases := []struct {
		in   map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"resource_provisioning_enforce": ""}, false},
		{map[string]string{"resource_provisioning_enforce": "nope"}, false},
		{map[string]string{"resource_provisioning_enforce": "true"}, true},
		{map[string]string{"resource_provisioning_enforce": "false"}, false},
		{map[string]string{"resource_provisioning_enforce": "1"}, true},
	}
	for _, tc := range cases {
		if got := ResourceProvisioningEnforce(tc.in); got != tc.want {
			t.Fatalf("ResourceProvisioningEnforce(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestResourceProvisioningEnforceRoundTrip proves: default false (both the
// DTO and the Service accessor) before any write, then a true/false
// round-trip through UpdateSystemSettings persists and reads back correctly,
// and an errored settings store fails open (default false, never propagated).
func TestResourceProvisioningEnforceRoundTrip(t *testing.T) {
	settings := NewMemorySystemSettings()
	svc := NewService(ServiceDeps{SystemSettings: settings, Clock: fixedClock()})

	if dto := svc.SystemSettingsView(context.Background()); dto.ResourceProvisioningEnforce {
		t.Fatalf("default DTO ResourceProvisioningEnforce = true, want false")
	}
	if svc.ResourceProvisioningEnforce(context.Background()) {
		t.Fatalf("default accessor ResourceProvisioningEnforce = true, want false")
	}

	got, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		ResourceProvisioningEnforce: boolPtr(true),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(true): %v", err)
	}
	if !got.ResourceProvisioningEnforce {
		t.Fatalf("DTO ResourceProvisioningEnforce = false after set true, want true")
	}
	if !svc.ResourceProvisioningEnforce(context.Background()) {
		t.Fatalf("accessor ResourceProvisioningEnforce = false after set true, want true")
	}
	values, _ := settings.SystemSettings(context.Background())
	if values["resource_provisioning_enforce"] != "true" {
		t.Fatalf("stored resource_provisioning_enforce = %q, want true", values["resource_provisioning_enforce"])
	}

	got, err = svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		ResourceProvisioningEnforce: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("UpdateSystemSettings(false): %v", err)
	}
	if got.ResourceProvisioningEnforce {
		t.Fatalf("DTO ResourceProvisioningEnforce = true after set false, want false")
	}
	if svc.ResourceProvisioningEnforce(context.Background()) {
		t.Fatalf("accessor ResourceProvisioningEnforce = true after set false, want false")
	}

	// Re-fetch round-trips.
	if refetch := svc.SystemSettingsView(context.Background()); refetch.ResourceProvisioningEnforce {
		t.Fatalf("refetch ResourceProvisioningEnforce = true, want false")
	}

	// An errored store fails open (default false, never propagated).
	errSvc := NewService(ServiceDeps{SystemSettings: erroringSettings{}, Clock: fixedClock()})
	if errSvc.ResourceProvisioningEnforce(context.Background()) {
		t.Fatalf("ResourceProvisioningEnforce on errored store = true, want false (safe default)")
	}
}

// mustProvisionResourceGroup is a test-only shortcut around
// SetResourceGroupProvision that fails the test on error.
func (e *groupTestEnv) mustProvisionResourceGroup(rgID, kind, targetID string) {
	e.t.Helper()
	if err := e.routes.SetResourceGroupProvision(e.ctx, rgID, kind, targetID); err != nil {
		e.t.Fatalf("SetResourceGroupProvision(%s, %s, %s): %v", rgID, kind, targetID, err)
	}
}

// provisionFixture is the shared read-only fixture for TestAllowedServerIDs:
//
//	SG (system) -- AG (admin, member: usr_u; usr_u4 INVITED, not member)
//	           \-- AG2 (admin, member: usr_u2 only -- deliberately NOT provisioned,
//	                    so it isolates the user_group branch below from the
//	                    admin_group branch: usr_u2's ONLY path to RG_R is via UG)
//	AG2 -- UG (user, owner+member: usr_u2)
//
//	RG_R: server X is a MEMBER; provisioned for admin_group:AG, user_group:UG,
//	      user:usr_u3, service:"svc_s".
//	RG_S: server Z is a MEMBER; provisioned for admin_group:"ag_ghost" -- a
//	      target none of the test principals ever match ("provisioned but for
//	      nobody the caller matches").
//	server Y: not a member of ANY resource group (unrestricted).
type provisionFixture struct {
	rgR, rgS         routing.ResourceGroup
	srvX, srvY, srvZ routing.AIServer
	ag, ag2, ug      UserGroupDTO
}

func setupProvisionFixture(e *groupTestEnv) provisionFixture {
	e.t.Helper()
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_u", "user")
	e.createUser("usr_u2", "user")
	e.createUser("usr_u3", "user")
	e.createUser("usr_u4", "user")
	e.createUser("usr_none", "user")
	sysAdmin := token("usr_s", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_u", "usr_u2", "usr_u3", "usr_u4", "usr_none")

	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_u")
	// usr_u4 is INVITED, not a member. AddGroupMembers on an admin-tier group
	// only ever writes state=member (no invite flow at that tier -- see
	// inviteGroupMembers, user-tier only), so this fixture writes the
	// invited row directly at the store level.
	if err := e.dir.SetUserGroupMember(e.ctx, ag.ID, "usr_u4", store.GroupStateInvited, ""); err != nil {
		e.t.Fatalf("SetUserGroupMember(usr_u4, invited): %v", err)
	}

	ag2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG2", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag2.ID, "usr_u2")

	ug := e.mustCreateGroup(token("usr_u2"), CreateGroupInput{Tier: store.GroupTierUser, Name: "UG", ParentGroupID: ag2.ID})

	rgR := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_r", Name: "RG_R", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	rgS := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_s", Name: "RG_S", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})

	srvX := e.mustCreateServer("srv_x", "Server X")
	srvY := e.mustCreateServer("srv_y", "Server Y")
	srvZ := e.mustCreateServer("srv_z", "Server Z")
	e.mustLinkResourceGroupServer(rgR.ID, srvX.ID)
	e.mustLinkResourceGroupServer(rgS.ID, srvZ.ID)

	e.mustProvisionResourceGroup(rgR.ID, routing.ProvisionKindAdminGroup, ag.ID)
	e.mustProvisionResourceGroup(rgR.ID, routing.ProvisionKindUserGroup, ug.ID)
	e.mustProvisionResourceGroup(rgR.ID, routing.ProvisionKindUser, "usr_u3")
	e.mustProvisionResourceGroup(rgR.ID, routing.ProvisionKindService, "svc_s")
	e.mustProvisionResourceGroup(rgS.ID, routing.ProvisionKindAdminGroup, "ag_ghost")

	return provisionFixture{rgR: rgR, rgS: rgS, srvX: srvX, srvY: srvY, srvZ: srvZ, ag: ag, ag2: ag2, ug: ug}
}

// mustSetProvisioningEnforce fails the test on error.
func mustSetProvisioningEnforce(e *groupTestEnv, enforce bool) {
	e.t.Helper()
	if _, err := e.svc.UpdateSystemSettings(e.ctx, systemToken(), UpdateSystemSettingsRequest{
		ResourceProvisioningEnforce: boolPtr(enforce),
	}); err != nil {
		e.t.Fatalf("UpdateSystemSettings(ResourceProvisioningEnforce=%v): %v", enforce, err)
	}
}

// TestAllowedServerIDs drives the 7 cases from the design brief against the
// shared provisionFixture. The fixture's Service has NO SystemSettings store
// wired until a case explicitly needs deny mode (mustSetProvisioningEnforce),
// so every other case runs under the default opt-in mode (enforce=false).
func TestAllowedServerIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	// A settings store must be wired for the deny-mode case (3) to actually
	// persist the toggle; unset key still reads back as opt-in (false) via
	// ResourceProvisioningEnforce's own default, so every other case is
	// unaffected until case 3 flips it (and flips it back after).
	e.svc.settings = NewMemorySystemSettings()
	f := setupProvisionFixture(e)
	ids := []string{f.srvX.ID, f.srvY.ID, f.srvZ.ID}

	// 1. opt-in (enforce=false), user U provisioned via admin_group AG into
	// RG_R: X allowed (provisioned), Y allowed (unrestricted), Z NOT allowed
	// (restricted, U not a target of RG_S).
	t.Run("opt-in admin_group match", func(t *testing.T) {
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_u"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if !got[f.srvX.ID] {
			t.Errorf("X allowed = false, want true (admin_group provisioned)")
		}
		if !got[f.srvY.ID] {
			t.Errorf("Y allowed = false, want true (unrestricted)")
		}
		if got[f.srvZ.ID] {
			t.Errorf("Z allowed = true, want false (restricted, not a target)")
		}
	})

	// 2. opt-in, user with NO matching provision: X NOT allowed, Y allowed,
	// Z NOT allowed.
	t.Run("opt-in no matching provision", func(t *testing.T) {
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_none"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if got[f.srvX.ID] {
			t.Errorf("X allowed = true, want false (restricted, no matching provision)")
		}
		if !got[f.srvY.ID] {
			t.Errorf("Y allowed = false, want true (unrestricted)")
		}
		if got[f.srvZ.ID] {
			t.Errorf("Z allowed = true, want false (restricted, no matching provision)")
		}
	})

	// 3. deny (enforce=true), user U provisioned into RG_R: X allowed, Y NOT
	// allowed (deny-by-default), Z NOT allowed.
	t.Run("deny mode admin_group match", func(t *testing.T) {
		mustSetProvisioningEnforce(e, true)
		defer mustSetProvisioningEnforce(e, false) // restore opt-in for the remaining cases
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_u"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if !got[f.srvX.ID] {
			t.Errorf("X allowed = false, want true (admin_group provisioned)")
		}
		if got[f.srvY.ID] {
			t.Errorf("Y allowed = true, want false (deny-by-default)")
		}
		if got[f.srvZ.ID] {
			t.Errorf("Z allowed = true, want false (not a target)")
		}
	})

	// 4. user_group match: U2 (member of UG, UG provisioned into RG_R) -> X
	// allowed.
	t.Run("user_group match", func(t *testing.T) {
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_u2"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if !got[f.srvX.ID] {
			t.Errorf("X allowed = false, want true (user_group UG provisioned)")
		}
	})

	// 5. direct-user match: provision RG_R for `user` U3 directly -> U3 -> X
	// allowed.
	t.Run("direct-user match", func(t *testing.T) {
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_u3"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if !got[f.srvX.ID] {
			t.Errorf("X allowed = false, want true (direct user provision)")
		}
	})

	// 6. service match: provision RG_R for `service` S directly -> a
	// service-token principal (IsService, ServiceID=S) -> X allowed; a
	// DIFFERENT service -> X not allowed; the service is NEVER matched via
	// any group.
	t.Run("service match", func(t *testing.T) {
		svcS := auth.Token{Kind: "service", ServiceID: "svc_s"}
		got, err := e.svc.AllowedServerIDs(e.ctx, svcS, ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs(svc_s): %v", err)
		}
		if !got[f.srvX.ID] {
			t.Errorf("X allowed = false, want true (service svc_s directly provisioned)")
		}

		svcOther := auth.Token{Kind: "service", ServiceID: "svc_other"}
		got, err = e.svc.AllowedServerIDs(e.ctx, svcOther, ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs(svc_other): %v", err)
		}
		if got[f.srvX.ID] {
			t.Errorf("X allowed = true for svc_other, want false (not provisioned)")
		}
	})

	// 7. invited excluded: U4 is `invited` (not member) of AG -> X NOT
	// allowed.
	t.Run("invited excluded", func(t *testing.T) {
		got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_u4"), ids)
		if err != nil {
			t.Fatalf("AllowedServerIDs: %v", err)
		}
		if got[f.srvX.ID] {
			t.Errorf("X allowed = true, want false (usr_u4 is invited, not a member of AG)")
		}
	})
}

// TestAllowedServerIDsEmptyInput pins the len(serverIDs)==0 short-circuit
// (out={}, nil error) -- the first line of the function, otherwise
// untested by the table above.
func TestAllowedServerIDsEmptyInput(t *testing.T) {
	e := newGroupTestEnv(t)
	got, err := e.svc.AllowedServerIDs(e.ctx, token("usr_anyone"), nil)
	if err != nil {
		t.Fatalf("AllowedServerIDs(nil): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("AllowedServerIDs(nil) = %v, want empty map", got)
	}
}

// --- Resource Groups Phase 2 -- provisioning MANAGEMENT endpoints (Task 5,
// spec 2026-08-12-resource-groups-phase-2-provisioning): ResourceGroupProvisionsView,
// SetResourceGroupProvisions, ResourceGroupProvisionCandidates. Deliberately a
// DISTINCT fixture from provisionFixture above (which fixtures the
// PREDICATE's target side, i.e. AllowedServerIDs); this one fixtures the
// MANAGER's own authority + visibility side: who may reach the resource
// group at all (authorizeResourceGroup, via admin-group ownership), and
// whose users/groups/services that manager may validly provision it FOR
// (VisibleUserIDs / ListGroups / ListServices). -----------------------------

// provisionManagerFixture is the shared read-only fixture for
// TestResourceGroupProvisionsManagementEndpoints:
//
//	PM-SG (system) -- members: usr_pm_admin, usr_pm_peer (usr_pm_stranger is
//	  tied to NOTHING here -- not an SG member, not linked anywhere)
//	PM-AG (admin, parent PM-SG, owner+member: usr_pm_admin) -- linked to
//	  PM-RG (so usr_pm_admin manages PM-RG via PM-AG ownership)
//	PM-UG (user, parent PM-AG, owner+member: usr_pm_admin) -- visible to
//	  usr_pm_admin both directly (ownership) and via ListGroups' read-only
//	  parent-linked expansion
//	PM-RG (resource group, SystemGroupID=PM-SG, linked admin group: PM-AG)
//	PM-Service (a routing.Service, admin-group-linked to PM-AG) -- visible
//	  to usr_pm_admin via ListServices' admin-group branch
//	  (serviceManageGroupIDs' owner-of-PM-AG bypass)
type provisionManagerFixture struct {
	rg  routing.ResourceGroup
	ag  UserGroupDTO
	ug  UserGroupDTO
	svc routing.Service
}

func setupProvisionManagerFixture(e *groupTestEnv) provisionManagerFixture {
	e.t.Helper()
	e.createUser("usr_pm_sys", "system_admin")
	e.createUser("usr_pm_admin", "admin")
	e.createUser("usr_pm_peer", "user")
	e.createUser("usr_pm_stranger", "user")
	sysAdmin := token("usr_pm_sys", "system", "admin")
	admin := token("usr_pm_admin", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "PM-SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_pm_admin", "usr_pm_peer")

	ag := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "PM-AG", ParentGroupID: sg.ID})
	ug := e.mustCreateGroup(admin, CreateGroupInput{Tier: store.GroupTierUser, Name: "PM-UG", ParentGroupID: ag.ID})

	rg := e.mustCreateResourceGroup(routing.ResourceGroup{
		ID: "rgrp_pm", Name: "PM-RG", Status: routing.ServerStatusActive,
		SystemGroupID: sg.ID, CreatedAt: e.now, UpdatedAt: e.now,
	})
	e.mustLinkResourceGroupAdminGroup(rg.ID, ag.ID)

	svc := routing.Service{
		ID: "svc_pm", Name: "PM-Service", Status: routing.ServerStatusActive,
		CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.routes.CreateService(e.ctx, svc); err != nil {
		e.t.Fatalf("CreateService: %v", err)
	}
	if err := e.routes.SetServiceAdminGroup(e.ctx, svc.ID, ag.ID); err != nil {
		e.t.Fatalf("SetServiceAdminGroup: %v", err)
	}

	return provisionManagerFixture{rg: rg, ag: ag, ug: ug, svc: svc}
}

// groupRefIDs collects a []GroupRefDTO's ids into a lookup set (test helper).
func groupRefIDs(refs []GroupRefDTO) map[string]bool {
	out := make(map[string]bool, len(refs))
	for _, g := range refs {
		out[g.ID] = true
	}
	return out
}

// TestResourceGroupProvisionsManagementEndpoints drives the Task 5 endpoint
// tests (spec §Step 5) against the shared provisionManagerFixture, in
// deliberate order (later subtests depend on earlier ones' mutations, same
// shape as TestAllowedServerIDs' ordered t.Run table -- but here PUT
// actually mutates shared state, so ordering is load-bearing, not just
// organizational).
func TestResourceGroupProvisionsManagementEndpoints(t *testing.T) {
	e := newGroupTestEnv(t)
	f := setupProvisionManagerFixture(e)
	admin := token("usr_pm_admin", "admin")
	stranger := token("usr_pm_stranger")

	t.Run("GET as manager on a fresh RG is empty", func(t *testing.T) {
		got, err := e.svc.ResourceGroupProvisionsView(e.ctx, admin, f.rg.ID)
		if err != nil {
			t.Fatalf("ResourceGroupProvisionsView: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("provisions = %+v, want empty", got)
		}
	})

	t.Run("GET as a non-manager is 404 no-leak", func(t *testing.T) {
		_, err := e.svc.ResourceGroupProvisionsView(e.ctx, stranger, f.rg.ID)
		if !errors.Is(err, ErrResourceGroupNotFound) {
			t.Fatalf("ResourceGroupProvisionsView(stranger) = %v, want ErrResourceGroupNotFound", err)
		}
	})

	t.Run("candidates for a non-manager is 404 no-leak", func(t *testing.T) {
		_, err := e.svc.ResourceGroupProvisionCandidates(e.ctx, stranger, f.rg.ID)
		if !errors.Is(err, ErrResourceGroupNotFound) {
			t.Fatalf("ResourceGroupProvisionCandidates(stranger) = %v, want ErrResourceGroupNotFound", err)
		}
	})

	t.Run("candidates are scoped to the manager's OWN visible landscape", func(t *testing.T) {
		cand, err := e.svc.ResourceGroupProvisionCandidates(e.ctx, admin, f.rg.ID)
		if err != nil {
			t.Fatalf("ResourceGroupProvisionCandidates: %v", err)
		}
		users := map[string]bool{}
		for _, u := range cand.Users {
			users[u.ID] = true
		}
		if !users["usr_pm_admin"] || !users["usr_pm_peer"] {
			t.Fatalf("candidate users = %+v, want usr_pm_admin+usr_pm_peer present (PM-SG members)", cand.Users)
		}
		if users["usr_pm_stranger"] {
			t.Fatalf("candidate users = %+v, want usr_pm_stranger ABSENT (not a PM-SG member)", cand.Users)
		}
		if ugSet := groupRefIDs(cand.UserGroups); !ugSet[f.ug.ID] {
			t.Fatalf("candidate user_groups = %+v, want %s present", cand.UserGroups, f.ug.ID)
		}
		if agSet := groupRefIDs(cand.AdminGroups); !agSet[f.ag.ID] {
			t.Fatalf("candidate admin_groups = %+v, want %s present", cand.AdminGroups, f.ag.ID)
		}
		if svcSet := groupRefIDs(cand.Services); !svcSet[f.svc.ID] {
			t.Fatalf("candidate services = %+v, want %s present", cand.Services, f.svc.ID)
		}
	})

	t.Run("PUT with a target NOT visible to the caller is rejected, store unchanged", func(t *testing.T) {
		err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindUser, TargetID: "usr_pm_peer"},     // valid
			{Kind: routing.ProvisionKindUser, TargetID: "usr_pm_stranger"}, // NOT visible to admin
		})
		if !errors.Is(err, ErrResourceGroupProvisionTargetInvalid) {
			t.Fatalf("SetResourceGroupProvisions = %v, want ErrResourceGroupProvisionTargetInvalid", err)
		}
		got, gerr := e.routes.ResourceGroupProvisions(e.ctx, f.rg.ID)
		if gerr != nil {
			t.Fatalf("ResourceGroupProvisions: %v", gerr)
		}
		if len(got) != 0 {
			t.Fatalf("store mutated on a rejected PUT: %+v, want unchanged/empty", got)
		}
	})

	t.Run("PUT with a non-visible admin_group target is rejected", func(t *testing.T) {
		// PM-AG2, owned by someone else, not linked to anything usr_pm_admin
		// manages -- outside admin's own admin-group landscape.
		e.createUser("usr_pm_other", "admin")
		other := token("usr_pm_other", "admin")
		sysAdmin := token("usr_pm_sys", "system", "admin")
		sg2 := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "PM-SG2"})
		e.mustAddMembers(sysAdmin, sg2.ID, "usr_pm_other")
		ag2 := e.mustCreateGroup(other, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "PM-AG2", ParentGroupID: sg2.ID})

		err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindAdminGroup, TargetID: ag2.ID},
		})
		if !errors.Is(err, ErrResourceGroupProvisionTargetInvalid) {
			t.Fatalf("SetResourceGroupProvisions(foreign admin_group) = %v, want ErrResourceGroupProvisionTargetInvalid", err)
		}
	})

	t.Run("PUT an unrecognized kind is rejected", func(t *testing.T) {
		err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, []routing.ResourceGroupProvision{
			{Kind: "bogus_kind", TargetID: "usr_pm_peer"},
		})
		if !errors.Is(err, ErrResourceGroupProvisionTargetInvalid) {
			t.Fatalf("SetResourceGroupProvisions(unknown kind) = %v, want ErrResourceGroupProvisionTargetInvalid", err)
		}
	})

	t.Run("PUT a blank target id is rejected", func(t *testing.T) {
		err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindUser, TargetID: "   "},
		})
		if !errors.Is(err, ErrResourceGroupProvisionTargetInvalid) {
			t.Fatalf("SetResourceGroupProvisions(blank target) = %v, want ErrResourceGroupProvisionTargetInvalid", err)
		}
	})

	t.Run("PUT a valid mixed-kind set round-trips via GET", func(t *testing.T) {
		err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, []routing.ResourceGroupProvision{
			{Kind: routing.ProvisionKindUser, TargetID: "usr_pm_peer"},
			{Kind: routing.ProvisionKindUserGroup, TargetID: f.ug.ID},
			{Kind: routing.ProvisionKindAdminGroup, TargetID: f.ag.ID},
			{Kind: routing.ProvisionKindService, TargetID: f.svc.ID},
		})
		if err != nil {
			t.Fatalf("SetResourceGroupProvisions: %v", err)
		}
		got, err := e.svc.ResourceGroupProvisionsView(e.ctx, admin, f.rg.ID)
		if err != nil {
			t.Fatalf("ResourceGroupProvisionsView: %v", err)
		}
		if len(got) != 4 {
			t.Fatalf("provisions = %+v, want 4 entries", got)
		}
		byKind := make(map[string]ResourceGroupProvisionDTO, len(got))
		for _, p := range got {
			byKind[p.Kind] = p
		}
		if p := byKind[routing.ProvisionKindUser]; p.TargetID != "usr_pm_peer" || p.TargetName != "usr_pm_peer" {
			t.Fatalf("user entry = %+v, want {target_id:usr_pm_peer, target_name:usr_pm_peer}", p)
		}
		if p := byKind[routing.ProvisionKindUserGroup]; p.TargetID != f.ug.ID || p.TargetName != f.ug.Name {
			t.Fatalf("user_group entry = %+v, want {target_id:%s, target_name:%s}", p, f.ug.ID, f.ug.Name)
		}
		if p := byKind[routing.ProvisionKindAdminGroup]; p.TargetID != f.ag.ID || p.TargetName != f.ag.Name {
			t.Fatalf("admin_group entry = %+v, want {target_id:%s, target_name:%s}", p, f.ag.ID, f.ag.Name)
		}
		if p := byKind[routing.ProvisionKindService]; p.TargetID != f.svc.ID || p.TargetName != f.svc.Name {
			t.Fatalf("service entry = %+v, want {target_id:%s, target_name:%s}", p, f.svc.ID, f.svc.Name)
		}
	})

	t.Run("PUT with an empty set clears the whole (previously non-empty) set", func(t *testing.T) {
		if err := e.svc.SetResourceGroupProvisions(e.ctx, admin, f.rg.ID, nil); err != nil {
			t.Fatalf("SetResourceGroupProvisions(nil): %v", err)
		}
		got, err := e.svc.ResourceGroupProvisionsView(e.ctx, admin, f.rg.ID)
		if err != nil {
			t.Fatalf("ResourceGroupProvisionsView: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("provisions = %+v, want cleared to empty", got)
		}
	})
}

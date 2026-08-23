// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// mustCreateService is a test-only shortcut that panics the test on error;
// mirrors mustCreateAIServer (service_server_groups_test.go) for services.
func mustCreateService(t *testing.T, e *groupTestEnv, svc routing.Service) routing.Service {
	t.Helper()
	if svc.Status == "" {
		svc.Status = routing.ServerStatusActive
	}
	if svc.CreatedAt.IsZero() {
		svc.CreatedAt = e.now
	}
	if svc.UpdatedAt.IsZero() {
		svc.UpdatedAt = e.now
	}
	if err := e.routes.CreateService(e.ctx, svc); err != nil {
		t.Fatalf("CreateService(%s): %v", svc.ID, err)
	}
	return svc
}

// TestServiceManageGroupIDs proves serviceManageGroupIDs's enumeration (Phase
// C, spec 2026-08-10): builds an admin group AG (owner usr_owner) with
// members usr_cms (promoted co-manager, CanManageServices=true), usr_cmg
// (promoted co-manager, CanManageGroup=true but CanManageServices=false --
// the inverse facet, proving the gate reads CanManageServices specifically),
// and usr_pm (a plain member, no manager row at all). Mirrors
// TestServerManageGroupIDs exactly, keyed on CanManageServices.
func TestServiceManageGroupIDs(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_cmg", "admin")
	e.createUser("usr_pm", "user")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_cms", "usr_cmg", "usr_pm")

	ag := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	if ag.OwnerUserID != "usr_owner" {
		t.Fatalf("precondition: usr_owner should own AG, got %+v", ag)
	}
	e.mustAddMembers(owner, ag.ID, "usr_cms", "usr_cmg", "usr_pm")

	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cms", false, false, false, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_services only): %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmg", false, true, false, false, true); err != nil {
		t.Fatalf("promote usr_cmg (can_manage_group only): %v", err)
	}

	// owner -> {AG.ID}.
	got, err := e.svc.serviceManageGroupIDs(e.ctx, owner)
	if err != nil {
		t.Fatalf("serviceManageGroupIDs(owner): %v", err)
	}
	if len(got) != 1 || !got[ag.ID] {
		t.Fatalf("serviceManageGroupIDs(owner) = %v, want {%s}", got, ag.ID)
	}

	// co-manager WITH CanManageServices=true -> {AG.ID}.
	got, err = e.svc.serviceManageGroupIDs(e.ctx, token("usr_cms", "admin"))
	if err != nil {
		t.Fatalf("serviceManageGroupIDs(cms): %v", err)
	}
	if len(got) != 1 || !got[ag.ID] {
		t.Fatalf("serviceManageGroupIDs(cms) = %v, want {%s}", got, ag.ID)
	}

	// co-manager WITHOUT CanManageServices (has CanManageGroup instead) ->
	// empty; a structure-management grant must not leak service-management
	// reach.
	got, err = e.svc.serviceManageGroupIDs(e.ctx, token("usr_cmg", "admin"))
	if err != nil {
		t.Fatalf("serviceManageGroupIDs(cmg): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("serviceManageGroupIDs(cmg) = %v, want empty", got)
	}

	// plain member (no manager row at all) -> empty.
	got, err = e.svc.serviceManageGroupIDs(e.ctx, token("usr_pm"))
	if err != nil {
		t.Fatalf("serviceManageGroupIDs(pm): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("serviceManageGroupIDs(pm) = %v, want empty", got)
	}
}

// TestAuthorizeServiceReadMatrix proves the rewritten authorizeServiceRead's
// branches (Phase C, spec 2026-08-10) against three services: svcDelegated
// (Full-Delegate usr_full + Token-Delegate usr_token, no group link),
// svcGrouped (linked to admin group AG, no delegates), and svcUngrouped (no
// delegates, no group link -- a legacy/pre-Task-4 service).
func TestAuthorizeServiceReadMatrix(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_full", "admin")
	e.createUser("usr_token", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_cmg", "admin")
	e.createUser("usr_other", "user")
	e.createUser("usr_plain", "admin")

	sysAdmin := token("usr_s", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_full", "usr_token", "usr_cms", "usr_cmg")
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_cms", "usr_cmg")
	if err := e.svc.PromoteManager(e.ctx, sysAdmin, ag.ID, "usr_cms", false, false, false, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_services only): %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, sysAdmin, ag.ID, "usr_cmg", false, true, false, false, true); err != nil {
		t.Fatalf("promote usr_cmg (can_manage_group only): %v", err)
	}

	svcDelegated := mustCreateService(t, e, routing.Service{ID: "svc_delegated", Name: "Delegated"})
	if err := e.routes.SetServiceDelegates(e.ctx, svcDelegated.ID, []routing.ServiceDelegate{
		{UserID: "usr_full", CanManageSettings: true},
		{UserID: "usr_token", CanManageSettings: false},
	}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}
	svcGrouped := mustCreateService(t, e, routing.Service{ID: "svc_grouped", Name: "Grouped"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svcGrouped.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup: %v", err)
	}
	svcUngrouped := mustCreateService(t, e, routing.Service{ID: "svc_ungrouped", Name: "Ungrouped"})

	// 1. system -> ok for ALL THREE (unconditional bypass).
	for _, id := range []string{svcDelegated.ID, svcGrouped.ID, svcUngrouped.ID} {
		if _, err := e.svc.authorizeServiceRead(e.ctx, sysAdmin, id); err != nil {
			t.Fatalf("authorizeServiceRead(system, %s) = %v, want nil", id, err)
		}
	}

	// 2. Full-Delegate -> ok for svcDelegated; 404 for the other two.
	full := token("usr_full", "admin")
	if _, err := e.svc.authorizeServiceRead(e.ctx, full, svcDelegated.ID); err != nil {
		t.Fatalf("authorizeServiceRead(full, svcDelegated) = %v, want nil", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, full, svcGrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(full, svcGrouped) = %v, want ErrServiceNotFound", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, full, svcUngrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(full, svcUngrouped) = %v, want ErrServiceNotFound", err)
	}

	// 3. Token-Delegate (a delegate at ANY stage satisfies Read) -> ok for
	// svcDelegated; 404 for the other two.
	tok := token("usr_token", "admin")
	if _, err := e.svc.authorizeServiceRead(e.ctx, tok, svcDelegated.ID); err != nil {
		t.Fatalf("authorizeServiceRead(token-delegate, svcDelegated) = %v, want nil", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, tok, svcGrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(token-delegate, svcGrouped) = %v, want ErrServiceNotFound", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, tok, svcUngrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(token-delegate, svcUngrouped) = %v, want ErrServiceNotFound", err)
	}

	// 4. can_manage_services co-manager of a LINKED admin group -> ok for
	// svcGrouped; 404 for the other two.
	cms := token("usr_cms", "admin")
	if _, err := e.svc.authorizeServiceRead(e.ctx, cms, svcGrouped.ID); err != nil {
		t.Fatalf("authorizeServiceRead(cms, svcGrouped) = %v, want nil", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, cms, svcDelegated.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(cms, svcDelegated) = %v, want ErrServiceNotFound", err)
	}
	if _, err := e.svc.authorizeServiceRead(e.ctx, cms, svcUngrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(cms, svcUngrouped) = %v, want ErrServiceNotFound", err)
	}

	// 5. co-manager of AG WITHOUT can_manage_services (has can_manage_group
	// instead) -> 404 on svcGrouped, the SAME no-leak error a non-member gets.
	cmg := token("usr_cmg", "admin")
	if _, err := e.svc.authorizeServiceRead(e.ctx, cmg, svcGrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(cmg-without-flag, svcGrouped) = %v, want ErrServiceNotFound", err)
	}

	// 6. unlinked (ungrouped) service for a non-system, non-delegate caller
	// -> 404 no-leak (identical to a plain stranger).
	other := token("usr_other")
	if _, err := e.svc.authorizeServiceRead(e.ctx, other, svcUngrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(other, svcUngrouped) = %v, want ErrServiceNotFound", err)
	}
	// A plain admin (no system scope, no delegate row, no group link) gets
	// the SAME 404 on the ungrouped service too -- the "any admin manages
	// every service" global bypass this task removes.
	plainAdmin := token("usr_plain", "admin")
	if _, err := e.svc.authorizeServiceRead(e.ctx, plainAdmin, svcUngrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(plain-admin, svcUngrouped) = %v, want ErrServiceNotFound", err)
	}

	// A genuinely unknown service id still 404s regardless of scope.
	if _, err := e.svc.authorizeServiceRead(e.ctx, sysAdmin, "svc_missing"); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceRead(system, missing) = %v, want ErrServiceNotFound", err)
	}
}

// TestAuthorizeServiceSettingsMatrix proves the rewritten
// authorizeServiceSettings's branches (Phase C, spec 2026-08-10): a group
// grant is FULL (equivalent to a Full-Delegate) -- a can_manage_services
// co-manager of a linked admin group reaches Settings too, not just Read; a
// Token-Delegate and a co-manager WITHOUT can_manage_services both get the
// same 404 as a stranger.
func TestAuthorizeServiceSettingsMatrix(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_full", "admin")
	e.createUser("usr_token", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_cmg", "admin")

	sysAdmin := token("usr_s", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_full", "usr_token", "usr_cms", "usr_cmg")
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(sysAdmin, ag.ID, "usr_cms", "usr_cmg")
	if err := e.svc.PromoteManager(e.ctx, sysAdmin, ag.ID, "usr_cms", false, false, false, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_services only): %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, sysAdmin, ag.ID, "usr_cmg", false, true, false, false, true); err != nil {
		t.Fatalf("promote usr_cmg (can_manage_group only): %v", err)
	}

	svcDelegated := mustCreateService(t, e, routing.Service{ID: "svc_delegated", Name: "Delegated"})
	if err := e.routes.SetServiceDelegates(e.ctx, svcDelegated.ID, []routing.ServiceDelegate{
		{UserID: "usr_full", CanManageSettings: true},
		{UserID: "usr_token", CanManageSettings: false},
	}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}
	svcGrouped := mustCreateService(t, e, routing.Service{ID: "svc_grouped", Name: "Grouped"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svcGrouped.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup: %v", err)
	}

	// system -> ok.
	if _, err := e.svc.authorizeServiceSettings(e.ctx, sysAdmin, svcDelegated.ID); err != nil {
		t.Fatalf("authorizeServiceSettings(system) = %v, want nil", err)
	}

	// Full-Delegate -> ok.
	if _, err := e.svc.authorizeServiceSettings(e.ctx, token("usr_full", "admin"), svcDelegated.ID); err != nil {
		t.Fatalf("authorizeServiceSettings(full-delegate) = %v, want nil", err)
	}

	// Token-Delegate -> 404 (lacks CanManageSettings).
	if _, err := e.svc.authorizeServiceSettings(e.ctx, token("usr_token", "admin"), svcDelegated.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceSettings(token-delegate) = %v, want ErrServiceNotFound", err)
	}

	// can_manage_services group-manager -> ok (a group grant is FULL).
	if _, err := e.svc.authorizeServiceSettings(e.ctx, token("usr_cms", "admin"), svcGrouped.ID); err != nil {
		t.Fatalf("authorizeServiceSettings(cms group-manager) = %v, want nil", err)
	}

	// co-manager WITHOUT can_manage_services -> 404.
	if _, err := e.svc.authorizeServiceSettings(e.ctx, token("usr_cmg", "admin"), svcGrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceSettings(cmg-without-flag) = %v, want ErrServiceNotFound", err)
	}

	// A stranger on svcGrouped still 404s.
	if _, err := e.svc.authorizeServiceSettings(e.ctx, token("usr_stranger_2"), svcGrouped.ID); !errors.Is(err, ErrServiceNotFound) {
		t.Fatalf("authorizeServiceSettings(stranger) = %v, want ErrServiceNotFound", err)
	}
}

// TestListServicesUnion proves ListServices' scoping (Phase C, spec
// 2026-08-10): system sees every service unconditionally; a non-system
// caller sees the union of ServicesByDelegate(principal) and
// ServicesByAdminGroups(serviceManageGroupIDs), deduped, with an unrelated
// service absent from their list.
func TestListServicesUnion(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_x", "admin")
	e.createUser("usr_unrelated", "user")

	sysAdmin := token("usr_s", "system", "admin")
	x := token("usr_x", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_x")
	ag := e.mustCreateGroup(x, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	// x owns AG (auto-owner on create) -> can_manage_services via ownership,
	// no explicit PromoteManager call needed.

	svcDelegated := mustCreateService(t, e, routing.Service{ID: "svc_x_delegated", Name: "XDelegated"})
	if err := e.routes.SetServiceDelegates(e.ctx, svcDelegated.ID, []routing.ServiceDelegate{{UserID: "usr_x", CanManageSettings: true}}); err != nil {
		t.Fatalf("SetServiceDelegates: %v", err)
	}
	svcGrouped := mustCreateService(t, e, routing.Service{ID: "svc_x_grouped", Name: "XGrouped"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, svcGrouped.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup: %v", err)
	}
	svcOther := mustCreateService(t, e, routing.Service{ID: "svc_other", Name: "Other"})

	// system -> all 3, incl. the ungrouped/undelegated one.
	sysList, err := e.svc.ListServices(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ListServices(system): %v", err)
	}
	if len(sysList) != 3 {
		t.Fatalf("system sees %d, want 3: %#v", len(sysList), sysList)
	}

	// x -> exactly {svcDelegated, svcGrouped}, deduped, svcOther ABSENT.
	xList, err := e.svc.ListServices(e.ctx, x)
	if err != nil {
		t.Fatalf("ListServices(x): %v", err)
	}
	if len(xList) != 2 {
		t.Fatalf("x sees %d, want 2: %#v", len(xList), xList)
	}
	seen := map[string]bool{}
	for _, dto := range xList {
		seen[dto.ID] = true
		if dto.ID == svcOther.ID {
			t.Fatalf("x's list leaked the unrelated service: %#v", xList)
		}
	}
	if !seen[svcDelegated.ID] || !seen[svcGrouped.ID] {
		t.Fatalf("x's list = %#v, want exactly {%s, %s}", xList, svcDelegated.ID, svcGrouped.ID)
	}

	// A totally unrelated non-admin -> empty (no delegate row, no group).
	unrelated := token("usr_unrelated")
	unrelatedList, err := e.svc.ListServices(e.ctx, unrelated)
	if err != nil {
		t.Fatalf("ListServices(unrelated): %v", err)
	}
	if len(unrelatedList) != 0 {
		t.Fatalf("unrelated sees %d, want 0: %#v", len(unrelatedList), unrelatedList)
	}
}

// TestServiceDTOAdminGroupsAndSystemGroup proves serviceDTO's additive
// fields (Phase C, spec 2026-08-10): a service linked to an admin group AG
// (parent system group SG) carries admin_groups=[{AG.ID,"AG"}] and
// system_group_id/system_group_name = SG's id/name; an unlinked service reads
// back the empty defaults ([] / "" / ""); a group that vanishes between the
// link write and the DTO read is skipped (best-effort), not an error.
func TestServiceDTOAdminGroupsAndSystemGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	sysAdmin := token("usr_s", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	linked := mustCreateService(t, e, routing.Service{ID: "svc_linked", Name: "Linked"})
	if err := e.routes.SetServiceAdminGroup(e.ctx, linked.ID, ag.ID); err != nil {
		t.Fatalf("SetServiceAdminGroup: %v", err)
	}
	if err := e.routes.UpdateServiceSystemGroup(e.ctx, linked.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServiceSystemGroup: %v", err)
	}

	dto, err := e.svc.GetService(e.ctx, sysAdmin, linked.ID)
	if err != nil {
		t.Fatalf("GetService(linked): %v", err)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag.ID || dto.AdminGroups[0].Name != "AG" {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG}]", dto.AdminGroups, ag.ID)
	}
	if dto.SystemGroupID != sg.ID || dto.SystemGroupName != "SG" {
		t.Fatalf("dto.SystemGroupID/Name = %q/%q, want %q/SG", dto.SystemGroupID, dto.SystemGroupName, sg.ID)
	}

	// An unlinked service reads back the empty defaults.
	unlinked := mustCreateService(t, e, routing.Service{ID: "svc_unlinked", Name: "Unlinked"})
	dto2, err := e.svc.GetService(e.ctx, sysAdmin, unlinked.ID)
	if err != nil {
		t.Fatalf("GetService(unlinked): %v", err)
	}
	if dto2.AdminGroups == nil || len(dto2.AdminGroups) != 0 {
		t.Fatalf("dto2.AdminGroups = %#v, want non-nil empty", dto2.AdminGroups)
	}
	if dto2.SystemGroupID != "" || dto2.SystemGroupName != "" {
		t.Fatalf("dto2.SystemGroupID/Name = %q/%q, want empty/empty", dto2.SystemGroupID, dto2.SystemGroupName)
	}

	// A vanished linked group (deleted after the link write, before the
	// read) is skipped -- best-effort, never fails the whole DTO.
	if err := e.dir.DeleteUserGroup(e.ctx, ag.ID); err != nil {
		t.Fatalf("DeleteUserGroup: %v", err)
	}
	dto3, err := e.svc.GetService(e.ctx, sysAdmin, linked.ID)
	if err != nil {
		t.Fatalf("GetService(linked, after group delete): %v", err)
	}
	if len(dto3.AdminGroups) != 0 {
		t.Fatalf("dto3.AdminGroups = %#v, want empty (vanished group skipped)", dto3.AdminGroups)
	}
}

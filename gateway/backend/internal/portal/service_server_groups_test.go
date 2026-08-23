// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"testing"
)

// mustCreateAIServer is a test-only shortcut that panics the test on error;
// mirrors the other must* helpers in service_user_groups_test.go.
func mustCreateAIServer(t *testing.T, e *groupTestEnv, srv routing.AIServer) routing.AIServer {
	t.Helper()
	if srv.Status == "" {
		srv.Status = routing.ServerStatusActive
	}
	if srv.CreatedAt.IsZero() {
		srv.CreatedAt = e.now
	}
	if srv.UpdatedAt.IsZero() {
		srv.UpdatedAt = e.now
	}
	if err := e.routes.CreateAIServer(e.ctx, srv); err != nil {
		t.Fatalf("CreateAIServer(%s): %v", srv.ID, err)
	}
	return srv
}

// TestServerManageGroupIDs proves serverManageGroupIDs's enumeration (Phase B,
// spec 2026-08-10): builds an admin group AG (owner usr_owner) with members
// usr_cms (promoted co-manager, CanManageServers=true), usr_cmg (promoted
// co-manager, CanManageGroup=true but CanManageServers=false -- the inverse
// facet, proving the gate reads CanManageServers specifically), and usr_pm (a
// plain member, no manager row at all).
func TestServerManageGroupIDs(t *testing.T) {
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

	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cms", false, false, true, true, true); err != nil {
		t.Fatalf("promote usr_cms (can_manage_servers only): %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, owner, ag.ID, "usr_cmg", false, true, false, true, true); err != nil {
		t.Fatalf("promote usr_cmg (can_manage_group only): %v", err)
	}

	// owner -> {AG.ID}.
	got, err := e.svc.serverManageGroupIDs(e.ctx, owner)
	if err != nil {
		t.Fatalf("serverManageGroupIDs(owner): %v", err)
	}
	if len(got) != 1 || !got[ag.ID] {
		t.Fatalf("serverManageGroupIDs(owner) = %v, want {%s}", got, ag.ID)
	}

	// co-manager WITH CanManageServers=true -> {AG.ID}.
	got, err = e.svc.serverManageGroupIDs(e.ctx, token("usr_cms", "admin"))
	if err != nil {
		t.Fatalf("serverManageGroupIDs(cms): %v", err)
	}
	if len(got) != 1 || !got[ag.ID] {
		t.Fatalf("serverManageGroupIDs(cms) = %v, want {%s}", got, ag.ID)
	}

	// co-manager WITHOUT CanManageServers (has CanManageGroup instead) -> empty;
	// a structure-management grant must not leak server-management reach.
	got, err = e.svc.serverManageGroupIDs(e.ctx, token("usr_cmg", "admin"))
	if err != nil {
		t.Fatalf("serverManageGroupIDs(cmg): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("serverManageGroupIDs(cmg) = %v, want empty", got)
	}

	// plain member (no manager row at all) -> empty.
	got, err = e.svc.serverManageGroupIDs(e.ctx, token("usr_pm"))
	if err != nil {
		t.Fatalf("serverManageGroupIDs(pm): %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("serverManageGroupIDs(pm) = %v, want empty", got)
	}
}

// TestAuthorizeServerMatrix proves the rewritten authorizeServer's five
// branches (Phase B, spec 2026-08-10) against THREE servers: srvOwned (owned
// by usr_owner, no group link), srvGrouped (linked to admin group AG, no
// owner), and srvUngrouped (no owner, no group link -- a legacy server).
func TestAuthorizeServerMatrix(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_owner", "admin")
	e.createUser("usr_agowner", "admin")
	e.createUser("usr_cms", "admin")
	e.createUser("usr_cmg", "admin")
	e.createUser("usr_other", "user")

	sysAdmin := token("usr_s", "system", "admin")
	owner := token("usr_owner", "admin")
	agOwner := token("usr_agowner", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_owner", "usr_agowner", "usr_cms", "usr_cmg")
	// AG is owned by a DIFFERENT admin (usr_agowner) than the server owner
	// (usr_owner), so the two authorization paths (ServerOwners vs.
	// serverManageGroupIDs) stay independent in this matrix.
	ag := e.mustCreateGroup(agOwner, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddMembers(agOwner, ag.ID, "usr_cms", "usr_cmg")
	if err := e.svc.PromoteManager(e.ctx, agOwner, ag.ID, "usr_cms", false, false, true, true, true); err != nil {
		t.Fatalf("promote usr_cms: %v", err)
	}
	if err := e.svc.PromoteManager(e.ctx, agOwner, ag.ID, "usr_cmg", false, true, false, true, true); err != nil {
		t.Fatalf("promote usr_cmg: %v", err)
	}

	srvOwned := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_owned", Name: "Owned"})
	if err := e.routes.SetServerOwners(e.ctx, srvOwned.ID, []string{"usr_owner"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	srvGrouped := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_grouped", Name: "Grouped"})
	if err := e.routes.SetServerAdminGroup(e.ctx, srvGrouped.ID, ag.ID); err != nil {
		t.Fatalf("SetServerAdminGroup: %v", err)
	}
	srvUngrouped := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_ungrouped", Name: "Ungrouped"})

	// 1. system -> ok for ALL THREE (unconditional bypass).
	for _, id := range []string{srvOwned.ID, srvGrouped.ID, srvUngrouped.ID} {
		if _, err := e.svc.authorizeServer(e.ctx, sysAdmin, id); err != nil {
			t.Fatalf("authorizeServer(system, %s) = %v, want nil", id, err)
		}
	}

	// 2. ServerOwner -> ok for srvOwned; 404 for the other two (owner has no
	// group link to AG and AG isn't linked to srvOwned's siblings).
	if _, err := e.svc.authorizeServer(e.ctx, owner, srvOwned.ID); err != nil {
		t.Fatalf("authorizeServer(owner, srvOwned) = %v, want nil", err)
	}
	if _, err := e.svc.authorizeServer(e.ctx, owner, srvGrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(owner, srvGrouped) = %v, want ErrServerNotFound", err)
	}
	if _, err := e.svc.authorizeServer(e.ctx, owner, srvUngrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(owner, srvUngrouped) = %v, want ErrServerNotFound", err)
	}

	// 3. can_manage_servers co-manager of a LINKED admin group -> ok for
	// srvGrouped; 404 for the other two (not an owner, group not linked there).
	cms := token("usr_cms", "admin")
	if _, err := e.svc.authorizeServer(e.ctx, cms, srvGrouped.ID); err != nil {
		t.Fatalf("authorizeServer(cms, srvGrouped) = %v, want nil", err)
	}
	if _, err := e.svc.authorizeServer(e.ctx, cms, srvOwned.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(cms, srvOwned) = %v, want ErrServerNotFound", err)
	}
	if _, err := e.svc.authorizeServer(e.ctx, cms, srvUngrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(cms, srvUngrouped) = %v, want ErrServerNotFound", err)
	}

	// 4. co-manager of AG WITHOUT can_manage_servers (has can_manage_group
	// instead) -> 404 on srvGrouped, the SAME no-leak error a non-member gets.
	cmg := token("usr_cmg", "admin")
	if _, err := e.svc.authorizeServer(e.ctx, cmg, srvGrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(cmg-without-flag, srvGrouped) = %v, want ErrServerNotFound", err)
	}

	// 5. unlinked (ungrouped) server for a non-system, non-owner caller ->
	// 404 no-leak (identical to a plain stranger).
	other := token("usr_other")
	if _, err := e.svc.authorizeServer(e.ctx, other, srvUngrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(other, srvUngrouped) = %v, want ErrServerNotFound", err)
	}
	// A plain admin (no system scope, no ownership, no group link) gets the
	// SAME 404 on the ungrouped server too -- the "any admin manages every
	// server" global bypass this task removes.
	if _, err := e.svc.authorizeServer(e.ctx, owner, srvUngrouped.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(plain-admin, srvUngrouped) = %v, want ErrServerNotFound", err)
	}

	// A genuinely unknown server id still 404s regardless of scope (the
	// AIServerByID-first load is preserved).
	if _, err := e.svc.authorizeServer(e.ctx, sysAdmin, "srv_missing"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("authorizeServer(system, missing) = %v, want ErrServerNotFound", err)
	}
}

// TestListServersUnion proves ListServers' scoping (Phase B, spec 2026-08-10):
// system sees every server unconditionally; a non-system caller sees the union
// of ServersByAdminGroups(serverManageGroupIDs) and ServersByOwner(principal),
// deduped, with an unrelated server absent from their list.
func TestListServersUnion(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	e.createUser("usr_x", "admin")
	e.createUser("usr_unrelated", "user")

	sysAdmin := token("usr_s", "system", "admin")
	x := token("usr_x", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddMembers(sysAdmin, sg.ID, "usr_x")
	ag := e.mustCreateGroup(x, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	// x owns AG (auto-owner on create) -> can_manage_servers via ownership,
	// no explicit PromoteManager call needed.

	sOwned := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_x_owned", Name: "XOwned"})
	if err := e.routes.SetServerOwners(e.ctx, sOwned.ID, []string{"usr_x"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	sGrouped := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_x_grouped", Name: "XGrouped"})
	if err := e.routes.SetServerAdminGroup(e.ctx, sGrouped.ID, ag.ID); err != nil {
		t.Fatalf("SetServerAdminGroup: %v", err)
	}
	sOther := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_other", Name: "Other"})

	// system -> all 3, incl. the ungrouped/unowned one.
	sysList, err := e.svc.ListServers(e.ctx, sysAdmin)
	if err != nil {
		t.Fatalf("ListServers(system): %v", err)
	}
	if len(sysList.Data) != 3 {
		t.Fatalf("system sees %d, want 3: %#v", len(sysList.Data), sysList.Data)
	}

	// x -> exactly {sOwned, sGrouped}, deduped, sOther ABSENT.
	xList, err := e.svc.ListServers(e.ctx, x)
	if err != nil {
		t.Fatalf("ListServers(x): %v", err)
	}
	if len(xList.Data) != 2 {
		t.Fatalf("x sees %d, want 2: %#v", len(xList.Data), xList.Data)
	}
	seen := map[string]bool{}
	for _, dto := range xList.Data {
		seen[dto.ID] = true
		if dto.ID == sOther.ID {
			t.Fatalf("x's list leaked the unrelated server: %#v", xList.Data)
		}
	}
	if !seen[sOwned.ID] || !seen[sGrouped.ID] {
		t.Fatalf("x's list = %#v, want exactly {%s, %s}", xList.Data, sOwned.ID, sGrouped.ID)
	}

	// A totally unrelated non-admin -> empty (no ownership, no group).
	unrelated := token("usr_unrelated")
	unrelatedList, err := e.svc.ListServers(e.ctx, unrelated)
	if err != nil {
		t.Fatalf("ListServers(unrelated): %v", err)
	}
	if len(unrelatedList.Data) != 0 {
		t.Fatalf("unrelated sees %d, want 0: %#v", len(unrelatedList.Data), unrelatedList.Data)
	}
}

// TestServerDTOAdminGroupsAndSystemGroup proves serverDTO's additive fields
// (Phase B, spec 2026-08-10): a server linked to an admin group AG (parent
// system group SG) carries admin_groups=[{AG.ID,"AG"}] and
// system_group_id/system_group_name = SG's id/name; an unlinked server reads
// back the empty defaults ([] / "" / ""); a group that vanishes between the
// link write and the DTO read is skipped (best-effort), not an error.
func TestServerDTOAdminGroupsAndSystemGroup(t *testing.T) {
	e := newGroupTestEnv(t)
	e.createUser("usr_s", "system_admin")
	sysAdmin := token("usr_s", "system", "admin")

	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})

	linked := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_linked", Name: "Linked"})
	if err := e.routes.SetServerAdminGroup(e.ctx, linked.ID, ag.ID); err != nil {
		t.Fatalf("SetServerAdminGroup: %v", err)
	}
	if err := e.routes.UpdateServerSystemGroup(e.ctx, linked.ID, sg.ID); err != nil {
		t.Fatalf("UpdateServerSystemGroup: %v", err)
	}

	dto, err := e.svc.GetServer(e.ctx, sysAdmin, linked.ID)
	if err != nil {
		t.Fatalf("GetServer(linked): %v", err)
	}
	if len(dto.AdminGroups) != 1 || dto.AdminGroups[0].ID != ag.ID || dto.AdminGroups[0].Name != "AG" {
		t.Fatalf("dto.AdminGroups = %#v, want [{%s AG}]", dto.AdminGroups, ag.ID)
	}
	if dto.SystemGroupID != sg.ID || dto.SystemGroupName != "SG" {
		t.Fatalf("dto.SystemGroupID/Name = %q/%q, want %q/SG", dto.SystemGroupID, dto.SystemGroupName, sg.ID)
	}

	// An unlinked server reads back the empty defaults.
	unlinked := mustCreateAIServer(t, e, routing.AIServer{ID: "srv_unlinked", Name: "Unlinked"})
	dto2, err := e.svc.GetServer(e.ctx, sysAdmin, unlinked.ID)
	if err != nil {
		t.Fatalf("GetServer(unlinked): %v", err)
	}
	if dto2.AdminGroups == nil || len(dto2.AdminGroups) != 0 {
		t.Fatalf("dto2.AdminGroups = %#v, want non-nil empty", dto2.AdminGroups)
	}
	if dto2.SystemGroupID != "" || dto2.SystemGroupName != "" {
		t.Fatalf("dto2.SystemGroupID/Name = %q/%q, want empty/empty", dto2.SystemGroupID, dto2.SystemGroupName)
	}

	// A vanished linked group (deleted after the link write, before the read)
	// is skipped -- best-effort, never fails the whole DTO.
	if err := e.dir.DeleteUserGroup(e.ctx, ag.ID); err != nil {
		t.Fatalf("DeleteUserGroup: %v", err)
	}
	dto3, err := e.svc.GetServer(e.ctx, sysAdmin, linked.ID)
	if err != nil {
		t.Fatalf("GetServer(linked, after group delete): %v", err)
	}
	if len(dto3.AdminGroups) != 0 {
		t.Fatalf("dto3.AdminGroups = %#v, want empty (vanished group skipped)", dto3.AdminGroups)
	}
}

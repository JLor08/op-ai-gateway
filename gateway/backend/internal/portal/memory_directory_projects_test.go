// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/store"
	"testing"
	"time"
)

// TestMemoryDirectoryProjects mirrors the SQLStore conformance contract in
// internal/store/conformance_test.go's testProjects, adapted for the
// MemoryDirectory: no FK enforcement (the memory driver never validates an
// owner_user_id/project_id/user_id/group_id reference on write — consistent
// with every other MemoryDirectory Create* method, which only checks id
// uniqueness), so the FK-violation-to-ErrNotFound assertions from the SQL
// conformance test are intentionally NOT reproduced here. Everything else —
// round-trip, upsert semantics, ProjectsByOwnerOrMember/ProjectsByGroup, and
// both cascade directions (project delete -> members/groups gone; a
// user-GROUP delete -> its project_groups row gone) — is asserted.
func TestMemoryDirectoryProjects(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	d := NewMemoryDirectory(nil)

	// A user-group used for the group-assignment/cascade assertions below;
	// group MEMBERSHIP itself is not consulted by ProjectsByOwnerOrMember at
	// this layer (that composition happens in the service, see Task 3), so
	// no member is added here.
	grpA := store.UserGroup{ID: "ugrp_proj_a", Tier: store.GroupTierSystem, Name: "ProjGroupA", CreatedAt: now, UpdatedAt: now}
	if err := d.CreateUserGroup(ctx, grpA); err != nil {
		t.Fatalf("create user group A: %v", err)
	}
	grpB := store.UserGroup{ID: "ugrp_proj_b", Tier: store.GroupTierSystem, Name: "ProjGroupB", CreatedAt: now, UpdatedAt: now}
	if err := d.CreateUserGroup(ctx, grpB); err != nil {
		t.Fatalf("create user group B: %v", err)
	}

	proj := store.Project{ID: "proj_1", Name: "Alpha", Description: "d", OwnerUserID: "usr_p1", CreatedAt: now, UpdatedAt: now}
	if err := d.CreateProject(ctx, proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	// Duplicate id -> ErrConflict.
	if err := d.CreateProject(ctx, proj); err != store.ErrConflict {
		t.Fatalf("create duplicate project id = %v, want ErrConflict", err)
	}

	got, err := d.ProjectByID(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ProjectByID: %v", err)
	}
	if got.Name != "Alpha" || got.OwnerUserID != "usr_p1" || got.Description != "d" {
		t.Fatalf("roundtrip: %+v", got)
	}
	if _, err := d.ProjectByID(ctx, "proj_missing"); err != store.ErrNotFound {
		t.Fatalf("ProjectByID(missing) = %v, want ErrNotFound", err)
	}

	// Update: rename + change description + change owner.
	updated := got
	updated.Name = "Alpha Renamed"
	updated.Description = "d2"
	updated.OwnerUserID = "usr_p2"
	updated.UpdatedAt = now.Add(time.Minute)
	if err := d.UpdateProject(ctx, updated); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	got, err = d.ProjectByID(ctx, proj.ID)
	if err != nil || got.Name != "Alpha Renamed" || got.Description != "d2" || got.OwnerUserID != "usr_p2" {
		t.Fatalf("after update: %+v err=%v", got, err)
	}
	if err := d.UpdateProject(ctx, store.Project{ID: "proj_missing", Name: "x", UpdatedAt: now}); err != store.ErrNotFound {
		t.Fatalf("UpdateProject(missing) = %v, want ErrNotFound", err)
	}
	// Restore owner to usr_p1 for the ownership assertions below.
	got.OwnerUserID = "usr_p1"
	if err := d.UpdateProject(ctx, got); err != nil {
		t.Fatalf("UpdateProject(restore owner): %v", err)
	}

	// ListProjects.
	list, err := d.ListProjects(ctx)
	if err != nil || len(list) != 1 || list[0].ID != proj.ID {
		t.Fatalf("ListProjects = %+v err=%v", list, err)
	}

	// Direct member usr_p2.
	if err := d.SetProjectMember(ctx, proj.ID, "usr_p2"); err != nil {
		t.Fatalf("SetProjectMember: %v", err)
	}
	// Idempotent re-set does not error or duplicate.
	if err := d.SetProjectMember(ctx, proj.ID, "usr_p2"); err != nil {
		t.Fatalf("SetProjectMember(re-set): %v", err)
	}
	members, err := d.ProjectMembers(ctx, proj.ID)
	if err != nil || len(members) != 1 || members[0] != "usr_p2" {
		t.Fatalf("ProjectMembers = %+v err=%v", members, err)
	}

	// Assign both groups.
	if err := d.SetProjectGroup(ctx, proj.ID, grpA.ID); err != nil {
		t.Fatalf("SetProjectGroup(A): %v", err)
	}
	if err := d.SetProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("SetProjectGroup(B): %v", err)
	}
	groups, err := d.ProjectGroups(ctx, proj.ID)
	if err != nil || len(groups) != 2 {
		t.Fatalf("ProjectGroups = %+v err=%v, want 2", groups, err)
	}

	// ProjectsByOwnerOrMember: owner (usr_p1), direct member (usr_p2), and a
	// non-member/non-owner (usr_p3) that gets none.
	byOwner, err := d.ProjectsByOwnerOrMember(ctx, "usr_p1")
	if err != nil || len(byOwner) != 1 || byOwner[0].ID != proj.ID {
		t.Fatalf("ProjectsByOwnerOrMember(owner) = %+v err=%v", byOwner, err)
	}
	byMember, err := d.ProjectsByOwnerOrMember(ctx, "usr_p2")
	if err != nil || len(byMember) != 1 || byMember[0].ID != proj.ID {
		t.Fatalf("ProjectsByOwnerOrMember(member) = %+v err=%v", byMember, err)
	}
	byNone, err := d.ProjectsByOwnerOrMember(ctx, "usr_p3")
	if err != nil || len(byNone) != 0 {
		t.Fatalf("ProjectsByOwnerOrMember(nonmember) = %+v err=%v, want empty", byNone, err)
	}

	// ProjectsByGroup, both groups.
	byGroupA, err := d.ProjectsByGroup(ctx, grpA.ID)
	if err != nil || len(byGroupA) != 1 || byGroupA[0].ID != proj.ID {
		t.Fatalf("ProjectsByGroup(A) = %+v err=%v", byGroupA, err)
	}
	byGroupB, err := d.ProjectsByGroup(ctx, grpB.ID)
	if err != nil || len(byGroupB) != 1 || byGroupB[0].ID != proj.ID {
		t.Fatalf("ProjectsByGroup(B) = %+v err=%v", byGroupB, err)
	}

	// Direct RemoveProjectGroup, then re-add B so the project-delete cascade
	// assertion further below still covers >1 row.
	if err := d.RemoveProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("RemoveProjectGroup(B): %v", err)
	}
	groups, _ = d.ProjectGroups(ctx, proj.ID)
	if len(groups) != 1 || groups[0] != grpA.ID {
		t.Fatalf("groups after direct remove = %+v, want only A", groups)
	}
	if err := d.SetProjectGroup(ctx, proj.ID, grpB.ID); err != nil {
		t.Fatalf("SetProjectGroup(B re-add): %v", err)
	}

	// Deleting user-GROUP A cascades: its project_groups row disappears,
	// leaving group B and the member untouched (a manual cascade —
	// dropProjectGroupsForGroupLocked, hooked into deleteGroupCascadeLocked —
	// mirroring the SQL ON DELETE CASCADE from project_groups.group_id ->
	// user_groups(id), migration45Up).
	if err := d.DeleteUserGroup(ctx, grpA.ID); err != nil {
		t.Fatalf("DeleteUserGroup(A): %v", err)
	}
	groups, err = d.ProjectGroups(ctx, proj.ID)
	if err != nil || len(groups) != 1 || groups[0] != grpB.ID {
		t.Fatalf("ProjectGroups after group delete = %+v err=%v, want only B (cascade)", groups, err)
	}
	members, err = d.ProjectMembers(ctx, proj.ID)
	if err != nil || len(members) != 1 || members[0] != "usr_p2" {
		t.Fatalf("members should be untouched by an unrelated group delete: %+v err=%v", members, err)
	}

	// Delete the project -> member + remaining group row cascade-gone
	// (manual cascade in DeleteProject).
	if err := d.DeleteProject(ctx, proj.ID); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	if _, err := d.ProjectByID(ctx, proj.ID); err != store.ErrNotFound {
		t.Fatalf("project not deleted: %v", err)
	}
	if m, err := d.ProjectMembers(ctx, proj.ID); err != nil || len(m) != 0 {
		t.Fatalf("members not cascaded on project delete: %+v err=%v", m, err)
	}
	if g, err := d.ProjectGroups(ctx, proj.ID); err != nil || len(g) != 0 {
		t.Fatalf("groups not cascaded on project delete: %+v err=%v", g, err)
	}
	if err := d.DeleteProject(ctx, proj.ID); err != store.ErrNotFound {
		t.Fatalf("DeleteProject(already deleted) = %v, want ErrNotFound", err)
	}
}

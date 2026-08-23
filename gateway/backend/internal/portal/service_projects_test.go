// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// mustCreateUserGroupOwnedByCounter gives each call to
// mustCreateUserGroupOwnedBy a distinct system/admin parent chain (a fresh
// system_admin id + system/admin group names), so the helper is safe to call
// more than once within a single test.
var mustCreateUserGroupOwnedByCounter int

// --- shared test scaffolding ------------------------------------------------

type projectTestEnv struct {
	t   *testing.T
	ctx context.Context
	dir *MemoryDirectory
	svc *Service
	// rec is the SAME in-memory usage.Recorder wired as Usage below, exposed
	// so a test can seed usage.Event rows directly (mirrors tokenProjectTestEnv
	// in service_token_project_test.go) to exercise ListProjects' bulk
	// total-token-usage aggregation (attachProjectTotalTokens).
	rec *usage.Recorder
	now time.Time
}

func newProjectTestEnv(t *testing.T) *projectTestEnv {
	t.Helper()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	rec := usage.NewRecorder()
	svc := NewService(ServiceDeps{
		Users:    dir,
		Groups:   dir,
		Projects: dir,
		Tokens:   dir,
		// Usage is wired (a fresh in-memory recorder) so ProjectTokens' usage
		// aggregation can be exercised from this env too, without requiring
		// every test that merely constructs a Service here to special-case a
		// nil s.usage; ProjectTokens itself nil-guards s.usage regardless (see
		// TestProjectTokens_NoUsageStoreIsZeroNotPanic in
		// service_token_project_test.go), so this is purely additive.
		Usage:  rec,
		Routes: routing.NewMemoryStore(),
		Clock:  func() time.Time { return now },
	})
	return &projectTestEnv{t: t, ctx: context.Background(), dir: dir, svc: svc, rec: rec, now: now}
}

func (e *projectTestEnv) createUser(id, role string) store.User {
	e.t.Helper()
	u := store.User{
		ID: id, Email: id + "@example.test", DisplayName: id, Role: role,
		Status: store.UserStatusActive, PreferredLanguage: "de",
		CreatedAt: e.now, UpdatedAt: e.now,
	}
	if err := e.dir.CreateUser(e.ctx, u); err != nil {
		e.t.Fatalf("create user %s: %v", id, err)
	}
	return u
}

func (e *projectTestEnv) mustCreateGroup(actor auth.Token, in CreateGroupInput) UserGroupDTO {
	e.t.Helper()
	dto, err := e.svc.CreateGroup(e.ctx, actor, in)
	if err != nil {
		e.t.Fatalf("CreateGroup(%+v): %v", in, err)
	}
	return dto
}

func (e *projectTestEnv) mustAddGroupMembers(actor auth.Token, groupID string, userIDs ...string) {
	e.t.Helper()
	if err := e.svc.AddGroupMembers(e.ctx, actor, groupID, userIDs); err != nil {
		e.t.Fatalf("AddGroupMembers(%v -> %s): %v", userIDs, groupID, err)
	}
}

func (e *projectTestEnv) mustCreateProject(actor auth.Token, name, description string) ProjectDTO {
	e.t.Helper()
	dto, err := e.svc.CreateProject(e.ctx, actor, CreateProjectInput{Name: name, Description: description})
	if err != nil {
		e.t.Fatalf("CreateProject(%s): %v", name, err)
	}
	return dto
}

// mustCreateUserGroupOwnedBy creates the system+admin parent chain
// (TestAddProjectGroups_RejectsNotVisible's pattern) needed for `owner` to
// create+own a user-tier group, adds owner as a member of the freshly-created
// admin group (createUserGroup's containment rule -- the caller must be a
// MEMBER of the admin parent), then creates the user-tier group AS owner so
// owner.UserID ends up as its OwnerUserID.
func (e *projectTestEnv) mustCreateUserGroupOwnedBy(owner auth.Token, name string) UserGroupDTO {
	e.t.Helper()
	mustCreateUserGroupOwnedByCounter++
	n := mustCreateUserGroupOwnedByCounter
	sysAdminID := fmt.Sprintf("usr_sysadmin_ugob_%d", n)
	e.createUser(sysAdminID, "system_admin")
	sysAdmin := token(sysAdminID, "system", "admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: fmt.Sprintf("SG-ugob-%d", n)})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: fmt.Sprintf("AG-ugob-%d", n), ParentGroupID: sg.ID})
	e.mustAddGroupMembers(sysAdmin, sg.ID, owner.UserID)
	e.mustAddGroupMembers(sysAdmin, ag.ID, owner.UserID)
	dto, err := e.svc.CreateGroup(e.ctx, owner, CreateGroupInput{Tier: store.GroupTierUser, Name: name, ParentGroupID: ag.ID})
	if err != nil {
		e.t.Fatalf("create user group %q owned by %s: %v", name, owner.UserID, err)
	}
	return dto
}

// --- CreateProject: owner + per-owner name uniqueness -----------------------

func TestCreateProject_OwnerAndNameUniquenessPerOwner(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_a", "user")
	e.createUser("usr_b", "user")
	a := token("usr_a")
	b := token("usr_b")

	p := e.mustCreateProject(a, "Widgets", "widget stuff")
	if p.OwnerUserID != "usr_a" {
		t.Fatalf("owner = %s, want usr_a", p.OwnerUserID)
	}
	if p.MyRole != "owner" || !p.CanManage {
		t.Fatalf("creator dto: %+v", p)
	}
	if p.MemberCount != 0 || p.GroupCount != 0 {
		t.Fatalf("fresh project counts: %+v", p)
	}

	// Same owner, same name (case-insensitive) -> conflict.
	if _, err := e.svc.CreateProject(e.ctx, a, CreateProjectInput{Name: "widgets"}); !errors.Is(err, ErrProjectNameConflict) {
		t.Fatalf("dup name same owner: got %v, want ErrProjectNameConflict", err)
	}

	// Different owner, same name -> allowed (uniqueness is per-owner).
	if _, err := e.svc.CreateProject(e.ctx, b, CreateProjectInput{Name: "Widgets"}); err != nil {
		t.Fatalf("same name different owner should be allowed: %v", err)
	}
}

// --- authorizeProjectManage: owner/admin ok, else 404-no-leak ---------------

func TestAuthorizeProjectManage_OwnerAdminNoLeak(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_other", "user")
	owner := token("usr_owner")
	admin := token("usr_admin", "admin")
	other := token("usr_other")

	p := e.mustCreateProject(owner, "Secret", "")

	if _, err := e.svc.authorizeProjectManage(e.ctx, owner, p.ID); err != nil {
		t.Fatalf("owner authorize: %v", err)
	}
	if _, err := e.svc.authorizeProjectManage(e.ctx, admin, p.ID); err != nil {
		t.Fatalf("admin authorize: %v", err)
	}
	if _, err := e.svc.authorizeProjectManage(e.ctx, other, p.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("uninvolved user authorize: got %v, want ErrProjectNotFound (no-leak)", err)
	}
	if _, err := e.svc.authorizeProjectManage(e.ctx, owner, "proj_missing"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("missing project: got %v, want ErrProjectNotFound", err)
	}

	// A mere member (not owner, not admin) may not manage. Added directly at
	// the store (bypassing AddProjectMembers' VisibleUserIDs eligibility gate,
	// which is exercised separately) so this test stays focused on
	// authorizeProjectManage's own owner/admin/no-leak behavior.
	if err := e.dir.SetProjectMember(e.ctx, p.ID, "usr_other"); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if _, err := e.svc.authorizeProjectManage(e.ctx, other, p.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("member (non-manager) authorize: got %v, want ErrProjectNotFound", err)
	}
}

// --- AddProjectMembers/AddProjectGroups: eligibility gates ------------------

func TestAddProjectMembers_RejectsNotVisible(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_visible", "user")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")

	p := e.mustCreateProject(owner, "P1", "")

	// usr_stranger is not in owner's VisibleUserIDs (owner has no groups, so
	// VisibleUserIDs(owner) == {usr_owner} only) -> rejected.
	if err := e.svc.AddProjectMembers(e.ctx, owner, p.ID, []string{"usr_stranger"}); !errors.Is(err, ErrProjectMemberNotVisible) {
		t.Fatalf("add non-visible member: got %v, want ErrProjectMemberNotVisible", err)
	}
	// The whole batch is rejected -- usr_visible must NOT have been added
	// alongside the invalid usr_stranger.
	members, err := e.dir.ProjectMembers(e.ctx, p.ID)
	if err != nil || len(members) != 0 {
		t.Fatalf("partial add leaked through: %+v err=%v", members, err)
	}

	// A user in the owner's own visible set (self is always visible) works.
	if err := e.svc.AddProjectMembers(e.ctx, owner, p.ID, []string{"usr_owner"}); err != nil {
		t.Fatalf("add self as member: %v", err)
	}
}

func TestAddProjectGroups_RejectsNotVisible(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner")

	// A group the project owner cannot see: a plain "user" principal's own
	// group landscape (ListGroups) never surfaces system-tier groups at all,
	// regardless of membership -- so this is not visible to owner even though
	// it exists.
	foreignGroup := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "Foreign"})

	// A group in the owner's OWN landscape: an admin-tier group the owner is
	// a MEMBER of (containment requires the owner to first be a member of
	// that admin group's parent SYSTEM group).
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG", ParentGroupID: sg.ID})
	e.mustAddGroupMembers(sysAdmin, sg.ID, "usr_owner")
	e.mustAddGroupMembers(sysAdmin, ag.ID, "usr_owner")

	p := e.mustCreateProject(owner, "P1", "")

	if err := e.svc.AddProjectGroups(e.ctx, owner, p.ID, []string{foreignGroup.ID}); !errors.Is(err, ErrProjectGroupNotVisible) {
		t.Fatalf("add non-visible group: got %v, want ErrProjectGroupNotVisible", err)
	}
	groups, err := e.dir.ProjectGroups(e.ctx, p.ID)
	if err != nil || len(groups) != 0 {
		t.Fatalf("non-visible group leaked in: %+v err=%v", groups, err)
	}

	// A group in the owner's own landscape is assignable.
	if err := e.svc.AddProjectGroups(e.ctx, owner, p.ID, []string{ag.ID}); err != nil {
		t.Fatalf("add own-landscape group: %v", err)
	}
	groups, err = e.dir.ProjectGroups(e.ctx, p.ID)
	if err != nil || len(groups) != 1 || groups[0] != ag.ID {
		t.Fatalf("groups after valid add: %+v err=%v", groups, err)
	}
}

// --- memberProjectIDs / MyProjects: owner + direct + via-group -------------

func TestMemberProjectIDs_OwnerDirectAndViaGroup(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_sysadmin", "system_admin")
	e.createUser("usr_owner", "user")
	e.createUser("usr_direct", "user")
	e.createUser("usr_viagroup", "user")
	e.createUser("usr_outside", "user")
	sysAdmin := token("usr_sysadmin", "system", "admin")
	owner := token("usr_owner")

	pOwned := e.mustCreateProject(owner, "Owned", "")
	pDirect := e.mustCreateProject(owner, "Direct", "")
	pViaGroup := e.mustCreateProject(owner, "ViaGroup", "")

	// Seeded directly at the store (bypassing AddProjectMembers' eligibility
	// gate, exercised separately) so this test stays focused on
	// memberProjectIDs' own composition.
	if err := e.dir.SetProjectMember(e.ctx, pDirect.ID, "usr_direct"); err != nil {
		t.Fatalf("seed direct member: %v", err)
	}

	// A group the "via-group" user is a MEMBER of (state=member), assigned to
	// the third project.
	grp := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG"})
	e.mustAddGroupMembers(sysAdmin, grp.ID, "usr_viagroup")
	if err := e.dir.SetProjectGroup(e.ctx, pViaGroup.ID, grp.ID); err != nil {
		t.Fatalf("assign group to project: %v", err)
	}

	// usr_owner: owner of all three -> all three ids.
	ownerIDs, err := e.svc.memberProjectIDs(e.ctx, "usr_owner")
	if err != nil {
		t.Fatalf("memberProjectIDs(owner): %v", err)
	}
	if len(ownerIDs) != 3 || !ownerIDs[pOwned.ID] || !ownerIDs[pDirect.ID] || !ownerIDs[pViaGroup.ID] {
		t.Fatalf("memberProjectIDs(owner) = %v, want all 3", ownerIDs)
	}

	// usr_direct: direct member of pDirect only.
	directIDs, err := e.svc.memberProjectIDs(e.ctx, "usr_direct")
	if err != nil {
		t.Fatalf("memberProjectIDs(direct): %v", err)
	}
	if len(directIDs) != 1 || !directIDs[pDirect.ID] {
		t.Fatalf("memberProjectIDs(direct) = %v, want {%s}", directIDs, pDirect.ID)
	}

	// usr_viagroup: member of pViaGroup via the group's state=member row.
	viaGroupIDs, err := e.svc.memberProjectIDs(e.ctx, "usr_viagroup")
	if err != nil {
		t.Fatalf("memberProjectIDs(viagroup): %v", err)
	}
	if len(viaGroupIDs) != 1 || !viaGroupIDs[pViaGroup.ID] {
		t.Fatalf("memberProjectIDs(viagroup) = %v, want {%s}", viaGroupIDs, pViaGroup.ID)
	}

	// usr_outside: member of nothing.
	outsideIDs, err := e.svc.memberProjectIDs(e.ctx, "usr_outside")
	if err != nil {
		t.Fatalf("memberProjectIDs(outside): %v", err)
	}
	if len(outsideIDs) != 0 {
		t.Fatalf("memberProjectIDs(outside) = %v, want empty", outsideIDs)
	}

	// isProjectMember mirrors the same membership rule.
	if ok, err := e.svc.isProjectMember(e.ctx, "usr_viagroup", pViaGroup.ID); err != nil || !ok {
		t.Fatalf("isProjectMember(viagroup, pViaGroup) = %v, %v, want true, nil", ok, err)
	}
	if ok, err := e.svc.isProjectMember(e.ctx, "usr_outside", pViaGroup.ID); err != nil || ok {
		t.Fatalf("isProjectMember(outside, pViaGroup) = %v, %v, want false, nil", ok, err)
	}

	// MyProjects(direct) == the set memberProjectIDs computed for usr_direct.
	myProjects, err := e.svc.MyProjects(e.ctx, token("usr_direct"))
	if err != nil {
		t.Fatalf("MyProjects(direct): %v", err)
	}
	if len(myProjects) != 1 || myProjects[0].ID != pDirect.ID || myProjects[0].Name != "Direct" {
		t.Fatalf("MyProjects(direct) = %+v, want [{%s Direct}]", myProjects, pDirect.ID)
	}

	// MyProjects(owner) == all three, {id,name} only.
	myProjectsOwner, err := e.svc.MyProjects(e.ctx, owner)
	if err != nil {
		t.Fatalf("MyProjects(owner): %v", err)
	}
	if len(myProjectsOwner) != 3 {
		t.Fatalf("MyProjects(owner) = %+v, want 3 entries", myProjectsOwner)
	}
}

// --- Task 6: applyUsageScope project-aware widening (design spec §8) -------
//
// This is the feature's one behavior change on EXISTING surface: a
// project-scoped Activity query (group-by "project", or an exact
// project_id_exact filter) may widen a non-admin past their own-rows pin --
// but strictly to their MEMBER projects, never further, and never on a store
// error (fail-open).

// errProjectStore wraps a real ProjectStore but forces
// ProjectsByOwnerOrMember to fail -- memberProjectIDs calls it FIRST, before
// ever touching Groups, so this alone drives applyUsageScope's fail-open path
// without needing a matching GroupStore failure.
type errProjectStore struct {
	ProjectStore
	err error
}

func (e errProjectStore) ProjectsByOwnerOrMember(ctx context.Context, userID string) ([]store.Project, error) {
	return nil, e.err
}

func TestApplyUsageScope_ProjectAwareWidening(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")  // member of pAlpha only
	e.createUser("usr_outside", "user") // member of nothing
	e.createUser("usr_admin", "admin")
	owner := token("usr_owner")
	member := token("usr_member")
	outside := token("usr_outside")
	admin := token("usr_admin", "admin")

	pAlpha := e.mustCreateProject(owner, "Alpha", "")
	pBeta := e.mustCreateProject(owner, "Beta", "")
	if err := e.dir.SetProjectMember(e.ctx, pAlpha.ID, "usr_member"); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// (1) Top-level group-by-project, no exact filter: scope to the member set,
	// never wider -- UserID pin dropped (ScopeAll=true), ProjectIDs = member ids.
	q1 := usage.Query{GroupBy: "project"}
	e.svc.applyUsageScope(e.ctx, &q1, member)
	if !q1.ScopeAll || q1.UserID != "" {
		t.Fatalf("group-by-project widening = ScopeAll=%v UserID=%q, want true/''", q1.ScopeAll, q1.UserID)
	}
	if len(q1.ProjectIDs) != 1 || q1.ProjectIDs[0] != pAlpha.ID {
		t.Fatalf("group-by-project ProjectIDs = %v, want [%s]", q1.ProjectIDs, pAlpha.ID)
	}

	// (1b) A member of ZERO projects gets a non-nil EMPTY ProjectIDs (both
	// stores enforce that as "zero rows" -- never "no filter").
	q1b := usage.Query{GroupBy: "project"}
	e.svc.applyUsageScope(e.ctx, &q1b, outside)
	if !q1b.ScopeAll || q1b.UserID != "" {
		t.Fatalf("group-by-project (outsider) = ScopeAll=%v UserID=%q, want true/''", q1b.ScopeAll, q1b.UserID)
	}
	if q1b.ProjectIDs == nil || len(q1b.ProjectIDs) != 0 {
		t.Fatalf("group-by-project (outsider) ProjectIDs = %#v, want non-nil empty", q1b.ProjectIDs)
	}

	// (2) Drill-down project_id_exact = P, caller IS a member of P: full
	// visibility of P's rows, own-rows pin dropped.
	q2 := usage.Query{HasProjectIDExact: true, ProjectIDExact: pAlpha.ID}
	e.svc.applyUsageScope(e.ctx, &q2, member)
	if !q2.ScopeAll || q2.UserID != "" {
		t.Fatalf("drill-down member = ScopeAll=%v UserID=%q, want true/''", q2.ScopeAll, q2.UserID)
	}

	// (3) Drill-down project_id_exact = P, caller is NOT a member of P:
	// fallback to the own-rows pin (a non-member sees only their own P-rows).
	q3 := usage.Query{HasProjectIDExact: true, ProjectIDExact: pBeta.ID}
	e.svc.applyUsageScope(e.ctx, &q3, member)
	if q3.ScopeAll || q3.UserID != "usr_member" {
		t.Fatalf("drill-down non-member = ScopeAll=%v UserID=%q, want false/usr_member", q3.ScopeAll, q3.UserID)
	}

	// (3b) Drill-down into the EMPTY (no-project) bucket: never a member of ""
	// -> same own-rows fallback as (3), never widened.
	q3b := usage.Query{HasProjectIDExact: true, ProjectIDExact: ""}
	e.svc.applyUsageScope(e.ctx, &q3b, member)
	if q3b.ScopeAll || q3b.UserID != "usr_member" {
		t.Fatalf("drill-down empty-project bucket = ScopeAll=%v UserID=%q, want false/usr_member", q3b.ScopeAll, q3b.UserID)
	}

	// (4) A NON-project-scoped query for the same non-admin is UNCHANGED: still
	// pinned to their own rows, regardless of their project memberships.
	q4 := usage.Query{}
	e.svc.applyUsageScope(e.ctx, &q4, member)
	if q4.ScopeAll || q4.UserID != "usr_member" {
		t.Fatalf("non-project query = ScopeAll=%v UserID=%q, want false/usr_member", q4.ScopeAll, q4.UserID)
	}

	// (5) Fail-open: a memberProjectIDs STORE ERROR during a project-scoped
	// query must NEVER widen -- keep the own-rows pin, exactly like (4).
	failing := NewService(ServiceDeps{
		Users:    e.dir,
		Groups:   e.dir,
		Projects: errProjectStore{ProjectStore: e.dir, err: errors.New("boom")},
		Clock:    func() time.Time { return e.now },
	})
	q5 := usage.Query{GroupBy: "project"}
	failing.applyUsageScope(e.ctx, &q5, member)
	if q5.ScopeAll || q5.UserID != "usr_member" {
		t.Fatalf("store-error fail-open = ScopeAll=%v UserID=%q, want false/usr_member (never widen on error)", q5.ScopeAll, q5.UserID)
	}
	if q5.ProjectIDs != nil {
		t.Fatalf("store-error fail-open ProjectIDs = %v, want nil (untouched)", q5.ProjectIDs)
	}

	// (6) Admin + scope=all is UNCHANGED regardless of GroupBy -- the admin
	// branch returns before the project-scope logic is ever consulted.
	q6 := usage.Query{ScopeAll: true, GroupBy: "project"}
	e.svc.applyUsageScope(e.ctx, &q6, admin)
	if !q6.ScopeAll || q6.UserID != "" {
		t.Fatalf("admin all-scope = ScopeAll=%v UserID=%q, want true/''", q6.ScopeAll, q6.UserID)
	}
}

// --- ListProjects: owned + member-of, with my_role/can_manage --------------

func TestListProjects_OwnedAndMemberOf_RolesAndCanManage(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_member", "user")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_outside", "user")
	owner := token("usr_owner")
	member := token("usr_member")
	admin := token("usr_admin", "admin")
	outside := token("usr_outside")

	p := e.mustCreateProject(owner, "Shared", "")
	// Seeded directly at the store; AddProjectMembers' own eligibility gate is
	// exercised by TestAddProjectMembers_RejectsNotVisible.
	if err := e.dir.SetProjectMember(e.ctx, p.ID, "usr_member"); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	ownerList, err := e.svc.ListProjects(e.ctx, owner)
	if err != nil || len(ownerList) != 1 {
		t.Fatalf("ListProjects(owner) = %+v err=%v", ownerList, err)
	}
	if ownerList[0].MyRole != "owner" || !ownerList[0].CanManage {
		t.Fatalf("ListProjects(owner) dto: %+v", ownerList[0])
	}
	if ownerList[0].MemberCount != 1 {
		t.Fatalf("member count = %d, want 1", ownerList[0].MemberCount)
	}

	memberList, err := e.svc.ListProjects(e.ctx, member)
	if err != nil || len(memberList) != 1 {
		t.Fatalf("ListProjects(member) = %+v err=%v", memberList, err)
	}
	if memberList[0].MyRole != "member" || memberList[0].CanManage {
		t.Fatalf("ListProjects(member) dto: %+v", memberList[0])
	}

	// An admin sees EVERY project in ListProjects (they can manage any project),
	// even one they neither own nor belong to -- surfaced with the
	// principal-relative view MyRole="none" + CanManage=true.
	adminList, err := e.svc.ListProjects(e.ctx, admin)
	if err != nil || len(adminList) != 1 {
		t.Fatalf("ListProjects(admin) = %+v err=%v, want the one project", adminList, err)
	}
	if adminList[0].ID != p.ID || adminList[0].MyRole != "none" || !adminList[0].CanManage {
		t.Fatalf("ListProjects(admin) dto: %+v (want id=%s, my_role=none, can_manage=true)", adminList[0], p.ID)
	}
	if _, err := e.svc.authorizeProjectManage(e.ctx, admin, p.ID); err != nil {
		t.Fatalf("admin can still manage via direct id: %v", err)
	}

	// A non-admin outsider still sees NOTHING (the widening is admin-only --
	// the privacy-preserving default is unchanged for regular users).
	outsideList, err := e.svc.ListProjects(e.ctx, outside)
	if err != nil || len(outsideList) != 0 {
		t.Fatalf("ListProjects(outside) = %+v err=%v, want empty", outsideList, err)
	}
}

// TestListProjects_TotalTokens_BulkAggregation_NoLeak proves ListProjects'
// per-project TotalTokens: (1) sums a project's usage ACROSS every token +
// host that attributed to it (input+output+cached+cache-write, the same
// convention as ProjectTokenUsageTotalDTO), (2) is 0 for a project with no
// usage, (3) never mixes one project's usage into another's total (the
// no-leak property attachProjectTotalTokens's ProjectIDs scoping provides),
// and (4) is populated identically for a non-admin OWNER's narrower list as
// for an admin's full list (the total is project-wide, not viewer-scoped).
func TestListProjects_TotalTokens_BulkAggregation_NoLeak(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner_a", "user")
	e.createUser("usr_owner_b", "admin")
	ownerA := token("usr_owner_a")
	adminB := token("usr_owner_b", "admin")

	projA := e.mustCreateProject(ownerA, "Project A", "")
	projB := e.mustCreateProject(adminB, "Project B", "")
	projC := e.mustCreateProject(adminB, "Project C (no usage)", "")

	now := e.now
	// Project A: two tokens, two hosts. input+output+cached+write per row:
	// e1: 10+20+1+2=33, e2: 5+5+0+0=10 -> total 43.
	e.rec.Record(usage.Event{
		ID: "eA1", UserID: "usr_owner_a", TokenID: "tokA1", ProjectID: projA.ID, ProjectName: projA.Name,
		Host: "srv1", InputTokens: 10, OutputTokens: 20, CachedTokens: 1, CacheWriteTokens: 2, CreatedAt: now,
	})
	e.rec.Record(usage.Event{
		ID: "eA2", UserID: "usr_owner_a", TokenID: "tokA2", ProjectID: projA.ID, ProjectName: projA.Name,
		Host: "srv2", InputTokens: 5, OutputTokens: 5, CreatedAt: now.Add(time.Minute),
	})
	// Project B: one row -> total 100.
	e.rec.Record(usage.Event{
		ID: "eB1", UserID: "usr_owner_b", TokenID: "tokB1", ProjectID: projB.ID, ProjectName: projB.Name,
		Host: "srv1", InputTokens: 40, OutputTokens: 60, CreatedAt: now,
	})
	// Project C gets no usage rows at all -- must read back 0, not error.

	adminList, err := e.svc.ListProjects(e.ctx, adminB)
	if err != nil || len(adminList) != 3 {
		t.Fatalf("ListProjects(admin) = %+v err=%v, want 3 projects", adminList, err)
	}
	byID := map[string]ProjectDTO{}
	for _, d := range adminList {
		byID[d.ID] = d
	}
	if got := byID[projA.ID].TotalTokens; got != 43 {
		t.Fatalf("project A total_tokens = %d, want 43 (no leak from B/C)", got)
	}
	if got := byID[projB.ID].TotalTokens; got != 100 {
		t.Fatalf("project B total_tokens = %d, want 100 (no leak from A/C)", got)
	}
	if got := byID[projC.ID].TotalTokens; got != 0 {
		t.Fatalf("project C (no usage) total_tokens = %d, want 0", got)
	}

	// A non-admin OWNER of A (who does not see B or C at all) still gets A's
	// full project-wide total -- the same 43, not just their own rows' share.
	ownerList, err := e.svc.ListProjects(e.ctx, ownerA)
	if err != nil || len(ownerList) != 1 {
		t.Fatalf("ListProjects(owner) = %+v err=%v, want just A", ownerList, err)
	}
	if got := ownerList[0].TotalTokens; got != 43 {
		t.Fatalf("owner's view of project A total_tokens = %d, want 43", got)
	}
}

// --- TransferProject: owner-only, target must be a current member ----------

func TestTransferProject_OwnerOnly_TargetMustBeMember(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_member", "user")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")
	admin := token("usr_admin", "admin")

	p := e.mustCreateProject(owner, "Handoff", "")
	// Seeded directly at the store; AddProjectMembers' own eligibility gate is
	// exercised by TestAddProjectMembers_RejectsNotVisible.
	if err := e.dir.SetProjectMember(e.ctx, p.ID, "usr_member"); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	// A non-member target is rejected.
	if err := e.svc.TransferProject(e.ctx, owner, p.ID, "usr_stranger"); !errors.Is(err, ErrProjectTransferTargetNotMember) {
		t.Fatalf("transfer to non-member: got %v, want ErrProjectTransferTargetNotMember", err)
	}

	// An admin (who CAN manage via authorizeProjectManage) may NOT transfer --
	// deliberately narrower than "manage".
	if err := e.svc.TransferProject(e.ctx, admin, p.ID, "usr_member"); !errors.Is(err, ErrProjectForbidden) {
		t.Fatalf("admin transfer: got %v, want ErrProjectForbidden", err)
	}

	// The owner transferring to a current member succeeds.
	if err := e.svc.TransferProject(e.ctx, owner, p.ID, "usr_member"); err != nil {
		t.Fatalf("owner transfer to member: %v", err)
	}
	got, err := e.dir.ProjectByID(e.ctx, p.ID)
	if err != nil || got.OwnerUserID != "usr_member" {
		t.Fatalf("after transfer: %+v err=%v", got, err)
	}

	// The former owner (now a non-owner, non-admin) can no longer transfer.
	if err := e.svc.TransferProject(e.ctx, owner, p.ID, "usr_owner"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("former owner transfer: got %v, want ErrProjectNotFound (no-leak)", err)
	}
}

// --- Update/Delete + ProjectMembersView/Candidates (light coverage) --------

func TestUpdateProject_RenameAndDescription(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	e.mustCreateProject(owner, "Existing", "")
	p := e.mustCreateProject(owner, "Original", "orig desc")

	updated, err := e.svc.UpdateProject(e.ctx, owner, p.ID, "Renamed", "new desc")
	if err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	if updated.Name != "Renamed" || updated.Description != "new desc" {
		t.Fatalf("updated dto: %+v", updated)
	}

	// Renaming to a name already owned by the SAME owner conflicts.
	if _, err := e.svc.UpdateProject(e.ctx, owner, p.ID, "Existing", ""); !errors.Is(err, ErrProjectNameConflict) {
		t.Fatalf("rename to existing sibling name: got %v, want ErrProjectNameConflict", err)
	}

	// A blank name leaves the stored name untouched (only description changes).
	kept, err := e.svc.UpdateProject(e.ctx, owner, p.ID, "   ", "desc only")
	if err != nil {
		t.Fatalf("UpdateProject(blank name): %v", err)
	}
	if kept.Name != "Renamed" || kept.Description != "desc only" {
		t.Fatalf("blank-name update: %+v", kept)
	}
}

func TestDeleteProject_OwnerOrAdmin(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_admin", "admin")
	e.createUser("usr_stranger", "user")
	owner := token("usr_owner")
	admin := token("usr_admin", "admin")
	stranger := token("usr_stranger")

	p1 := e.mustCreateProject(owner, "P1", "")
	if err := e.svc.DeleteProject(e.ctx, stranger, p1.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("stranger delete: got %v, want ErrProjectNotFound", err)
	}
	if err := e.svc.DeleteProject(e.ctx, owner, p1.ID); err != nil {
		t.Fatalf("owner delete: %v", err)
	}
	if _, err := e.dir.ProjectByID(e.ctx, p1.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project not actually deleted: %v", err)
	}

	p2 := e.mustCreateProject(owner, "P2", "")
	if err := e.svc.DeleteProject(e.ctx, admin, p2.ID); err != nil {
		t.Fatalf("admin delete: %v", err)
	}
}

func TestProjectMembersView_And_Candidates(t *testing.T) {
	e := newProjectTestEnv(t)
	// A system_admin owner keeps the group-tier setup simple (a system_admin
	// may create a system-tier group directly, no parent hierarchy needed) --
	// the containment/eligibility rules around group tiers are already
	// covered by TestAddProjectGroups_RejectsNotVisible.
	e.createUser("usr_owner", "system_admin")
	e.createUser("usr_member", "user")
	e.createUser("usr_candidate", "user")
	e.createUser("usr_viagroup", "user")
	e.createUser("usr_invited", "user")
	owner := token("usr_owner", "system", "admin")

	p := e.mustCreateProject(owner, "P1", "")
	grp := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierSystem, Name: "G1"})
	if err := e.svc.AddProjectMembers(e.ctx, owner, p.ID, []string{"usr_member"}); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := e.svc.AddProjectGroups(e.ctx, owner, p.ID, []string{grp.ID}); err != nil {
		t.Fatalf("add group: %v", err)
	}
	// usr_viagroup is a MEMBER of the assigned group (an effective project
	// member via the group, but NOT a direct member row); usr_invited is only
	// INVITED to it (must NOT count).
	e.mustAddGroupMembers(owner, grp.ID, "usr_viagroup")
	if err := e.dir.SetUserGroupMember(e.ctx, grp.ID, "usr_invited", store.GroupStateInvited, "usr_owner"); err != nil {
		t.Fatalf("seed invited group member: %v", err)
	}

	view, err := e.svc.ProjectMembersView(e.ctx, owner, p.ID)
	if err != nil {
		t.Fatalf("ProjectMembersView: %v", err)
	}
	// The direct-members list stays direct-only (the group-resolved member is
	// NOT a direct member row).
	if len(view.Users) != 1 || view.Users[0].ID != "usr_member" {
		t.Fatalf("view.Users = %+v", view.Users)
	}
	if len(view.Groups) != 1 || view.Groups[0].ID != grp.ID || view.Groups[0].Name != "G1" {
		t.Fatalf("view.Groups = %+v", view.Groups)
	}
	// TransferCandidates = every EFFECTIVE member = the direct member UNIONED
	// with the assigned group's MEMBER-state users (invited excluded). This is
	// exactly the set TransferProject accepts, so a group-only member is offered.
	tc := map[string]bool{}
	for _, u := range view.TransferCandidates {
		tc[u.ID] = true
	}
	if !tc["usr_member"] || !tc["usr_viagroup"] {
		t.Fatalf("TransferCandidates must include the direct member AND the group-resolved member: %+v", view.TransferCandidates)
	}
	if tc["usr_invited"] {
		t.Fatalf("TransferCandidates must exclude an invited-only group member: %+v", view.TransferCandidates)
	}
	// Cross-check: every transfer candidate is actually accepted by TransferProject.
	for id := range tc {
		if id == "usr_owner" {
			continue
		}
		ok, err := e.svc.isProjectMember(e.ctx, id, p.ID)
		if err != nil || !ok {
			t.Fatalf("transfer candidate %s not accepted by isProjectMember (ok=%v err=%v)", id, ok, err)
		}
	}

	users, groups, err := e.svc.ProjectCandidates(e.ctx, owner, p.ID)
	if err != nil {
		t.Fatalf("ProjectCandidates: %v", err)
	}
	// usr_member is already a member -> excluded from candidates; usr_owner
	// (self) and usr_candidate are visible-but-not-yet-members.
	foundCandidate := false
	for _, u := range users {
		if u.ID == "usr_member" {
			t.Fatalf("current member leaked into candidates: %+v", users)
		}
		if u.ID == "usr_candidate" {
			foundCandidate = true
		}
	}
	if !foundCandidate {
		t.Fatalf("expected usr_candidate among candidates: %+v", users)
	}
	for _, g := range groups {
		if g.ID == grp.ID {
			t.Fatalf("current group leaked into candidates: %+v", groups)
		}
	}
}

// --- AddProjectMembers/AddProjectGroups: TOCTOU store.ErrNotFound remap -----

// errNotFoundProjectStore wraps a ProjectStore and forces SetProjectMember/
// SetProjectGroup to return store.ErrNotFound for one project id, simulating
// the store's FK-violation path (see SQLiteStore.SetProjectMember/
// SetProjectGroup) for a project deleted BETWEEN authorizeProjectManage's
// read and the write -- a race MemoryDirectory itself never produces (it
// enforces no FK), so this fake is the only way to drive that branch.
type errNotFoundProjectStore struct {
	ProjectStore
	failProjectID string
}

func (e *errNotFoundProjectStore) SetProjectMember(ctx context.Context, projectID, userID string) error {
	if projectID == e.failProjectID {
		return store.ErrNotFound
	}
	return e.ProjectStore.SetProjectMember(ctx, projectID, userID)
}

func (e *errNotFoundProjectStore) SetProjectGroup(ctx context.Context, projectID, groupID string) error {
	if projectID == e.failProjectID {
		return store.ErrNotFound
	}
	return e.ProjectStore.SetProjectGroup(ctx, projectID, groupID)
}

func TestAddProjectMembers_StoreNotFoundMapsToProjectNotFound(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "system_admin")
	e.createUser("usr_target", "user")
	owner := token("usr_owner", "system", "admin")
	p := e.mustCreateProject(owner, "Race", "")

	raced := NewService(ServiceDeps{
		Users:    e.dir,
		Groups:   e.dir,
		Projects: &errNotFoundProjectStore{ProjectStore: e.dir, failProjectID: p.ID},
		Routes:   routing.NewMemoryStore(),
		Clock:    func() time.Time { return e.now },
	})
	if err := raced.AddProjectMembers(e.ctx, owner, p.ID, []string{"usr_target"}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("AddProjectMembers store-not-found: got %v, want ErrProjectNotFound", err)
	}
}

func TestAddProjectGroups_StoreNotFoundMapsToProjectNotFound(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "system_admin")
	owner := token("usr_owner", "system", "admin")
	p := e.mustCreateProject(owner, "Race2", "")
	grp := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-Race"})

	raced := NewService(ServiceDeps{
		Users:    e.dir,
		Groups:   e.dir,
		Projects: &errNotFoundProjectStore{ProjectStore: e.dir, failProjectID: p.ID},
		Routes:   routing.NewMemoryStore(),
		Clock:    func() time.Time { return e.now },
	})
	if err := raced.AddProjectGroups(e.ctx, owner, p.ID, []string{grp.ID}); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("AddProjectGroups store-not-found: got %v, want ErrProjectNotFound", err)
	}
}

// --- CreateProject: coupled to a user-group (spec 2026-08-09) --------------

func TestCreateProject_CoupledToExistingGroup(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	// A user-tier group owned by usr_owner.
	grp := e.mustCreateUserGroupOwnedBy(owner, "Team")
	dto, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "P", CoupledGroupID: grp.ID})
	if err != nil {
		t.Fatalf("create coupled: %v", err)
	}
	if dto.CoupledGroupID != grp.ID || dto.CoupledGroupName != "Team" {
		t.Fatalf("coupled fields: %+v", dto)
	}
	// Owner is DERIVED from the group's owner (usr_owner), my_role=owner, can_manage.
	if dto.OwnerUserID != "usr_owner" || dto.MyRole != "owner" || !dto.CanManage {
		t.Fatalf("derived owner dto: %+v", dto)
	}
	// The coupled group is carried as a project_groups row (membership reuse).
	groups, _ := e.dir.ProjectGroups(e.ctx, dto.ID)
	if len(groups) != 1 || groups[0] != grp.ID {
		t.Fatalf("project_groups = %+v, want [%s]", groups, grp.ID)
	}
	// No individual members; stored owner is empty.
	if members, _ := e.dir.ProjectMembers(e.ctx, dto.ID); len(members) != 0 {
		t.Fatalf("coupled project has direct members: %+v", members)
	}
	stored, _ := e.dir.ProjectByID(e.ctx, dto.ID)
	if stored.OwnerUserID != "" {
		t.Fatalf("stored owner_user_id = %q, want empty (derived)", stored.OwnerUserID)
	}
}

func TestCreateProject_CoupleEligibility(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_a", "user")
	e.createUser("usr_b", "user")
	a := token("usr_a")
	b := token("usr_b")
	grpA := e.mustCreateUserGroupOwnedBy(a, "A")
	// Not the owner -> rejected.
	if _, err := e.svc.CreateProject(e.ctx, b, CreateProjectInput{Name: "X", CoupledGroupID: grpA.ID}); !errors.Is(err, ErrProjectCoupleGroupInvalid) {
		t.Fatalf("couple to a group you don't own: got %v, want ErrProjectCoupleGroupInvalid", err)
	}
	// Both coupling fields set -> ambiguous.
	if _, err := e.svc.CreateProject(e.ctx, a, CreateProjectInput{Name: "Y", CoupledGroupID: grpA.ID, CreateCoupledGroup: &NewCoupledGroup{Name: "N"}}); !errors.Is(err, ErrProjectCoupleAmbiguous) {
		t.Fatalf("both coupling fields: got %v, want ErrProjectCoupleAmbiguous", err)
	}
}

// TestCreateProject_CreateCoupledGroupBlankNameRejected is the regression test
// for fix round 1 (2026-08-09 review): the create-group coupling branch used
// to call the PRIVATE createUserGroup directly, which has NO empty-name
// guard of its own -- so a blank/whitespace create_coupled_group.name
// succeeded, leaving a permanent nameless user-tier group owned by the
// caller. The fix routes through the PUBLIC CreateGroup entry point, which
// trims + rejects an empty name (ErrGroupNameInvalid) before any tier
// dispatch -- so this must fail BEFORE creating either the group or the
// project.
func TestCreateProject_CreateCoupledGroupBlankNameRejected(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	if _, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{
		Name:               "Proj",
		CreateCoupledGroup: &NewCoupledGroup{Name: "   "},
	}); !errors.Is(err, ErrGroupNameInvalid) {
		t.Fatalf("blank create_coupled_group.name: got %v, want ErrGroupNameInvalid", err)
	}

	// No user-tier group was created -- in particular, none with an empty name.
	groups, err := e.dir.ListUserGroupsByTier(e.ctx, store.GroupTierUser)
	if err != nil {
		t.Fatalf("ListUserGroupsByTier: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("groups after rejected create = %+v, want none", groups)
	}

	// No project was created either.
	projects, err := e.svc.ListProjects(e.ctx, owner)
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(projects) != 0 {
		t.Fatalf("projects after rejected create = %+v, want none", projects)
	}
}

// TestCoupledProject_ManagementLocked is Task 3's test: membership/group/
// transfer mutations on a COUPLED project must reject with ErrProjectCoupled
// (membership is derived from the coupled group, not managed directly),
// while UpdateProject/DeleteProject stay allowed (not exercised here -- see
// the Task 3 brief) and a NORMAL (uncoupled) project must NOT be locked
// (regression).
func TestCoupledProject_MemberCountReflectsGroup(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_m2", "user")
	e.createUser("usr_inv", "user")
	owner := token("usr_owner")
	grp := e.mustCreateUserGroupOwnedBy(owner, "Team") // owner is a member-state row (count 1)
	// A second MEMBER-state user + an INVITED-only user, seeded directly on the store.
	if err := e.dir.SetUserGroupMember(e.ctx, grp.ID, "usr_m2", store.GroupStateMember, ""); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	if err := e.dir.SetUserGroupMember(e.ctx, grp.ID, "usr_inv", store.GroupStateInvited, "usr_owner"); err != nil {
		t.Fatalf("seed invited: %v", err)
	}

	dto, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "Coupled P", CoupledGroupID: grp.ID})
	if err != nil {
		t.Fatalf("create coupled: %v", err)
	}
	// A coupled project has no direct members, but member_count reports the
	// coupled group's MEMBER-state users (owner + usr_m2 = 2), NOT 0, and the
	// invited-only usr_inv is excluded.
	if dto.MemberCount != 2 {
		t.Fatalf("coupled member_count = %d, want 2 (owner + usr_m2 member-state; usr_inv invited excluded)", dto.MemberCount)
	}

	// Regression: a NORMAL project's member_count stays the direct-member count.
	np, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "Normal P"})
	if err != nil {
		t.Fatalf("create normal: %v", err)
	}
	if np.MemberCount != 0 {
		t.Fatalf("normal project member_count = %d, want 0 (no direct members)", np.MemberCount)
	}
}

func TestCoupledProject_ManagementLocked(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	e.createUser("usr_x", "user")
	owner := token("usr_owner")

	// Build the system/admin parent chain by hand (rather than via
	// mustCreateUserGroupOwnedBy) so this test retains sysAdmin + the admin
	// group id, needed below to seed usr_v into owner's VisibleUserIDs.
	sysAdmin := token("usr_sysadmin_lock", "system", "admin")
	e.createUser("usr_sysadmin_lock", "system_admin")
	sg := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierSystem, Name: "SG-lock"})
	ag := e.mustCreateGroup(sysAdmin, CreateGroupInput{Tier: store.GroupTierAdmin, Name: "AG-lock", ParentGroupID: sg.ID})
	e.mustAddGroupMembers(sysAdmin, sg.ID, "usr_owner")
	e.mustAddGroupMembers(sysAdmin, ag.ID, "usr_owner")
	grp := e.mustCreateGroup(owner, CreateGroupInput{Tier: store.GroupTierUser, Name: "Team", ParentGroupID: ag.ID})

	p, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "P", CoupledGroupID: grp.ID})
	if err != nil {
		t.Fatalf("create coupled: %v", err)
	}
	if err := e.svc.AddProjectMembers(e.ctx, owner, p.ID, []string{"usr_x"}); !errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("AddProjectMembers on coupled: got %v, want ErrProjectCoupled", err)
	}
	if err := e.svc.RemoveProjectMember(e.ctx, owner, p.ID, "usr_x"); !errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("RemoveProjectMember on coupled: got %v", err)
	}
	if err := e.svc.AddProjectGroups(e.ctx, owner, p.ID, []string{grp.ID}); !errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("AddProjectGroups on coupled: got %v", err)
	}
	if err := e.svc.RemoveProjectGroup(e.ctx, owner, p.ID, grp.ID); !errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("RemoveProjectGroup on coupled: got %v", err)
	}
	if err := e.svc.TransferProject(e.ctx, owner, p.ID, "usr_x"); !errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("TransferProject on coupled: got %v", err)
	}

	// A normal project still allows these (regression): create one and add a
	// member. Seed usr_v into owner's VisibleUserIDs (member of owner's own
	// admin group "ag") so AddProjectMembers passes its eligibility gate --
	// this way the assertion below proves the add actually SUCCEEDED (not
	// merely that it failed with some OTHER error).
	np, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "Normal"})
	if err != nil {
		t.Fatalf("create normal: %v", err)
	}
	e.createUser("usr_v", "user")
	// Containment: an admin-group member must first be a member of its
	// parent SYSTEM group (mirrors TestAddProjectGroups_RejectsNotVisible's
	// setup order for sg/ag membership above).
	e.mustAddGroupMembers(sysAdmin, sg.ID, "usr_v")
	e.mustAddGroupMembers(sysAdmin, ag.ID, "usr_v")
	if err := e.svc.AddProjectMembers(e.ctx, owner, np.ID, []string{"usr_v"}); errors.Is(err, ErrProjectCoupled) {
		t.Fatalf("normal project must not be coupled-locked: %v", err)
	} else if err != nil {
		t.Fatalf("AddProjectMembers on normal project: %v", err)
	}
}

// findGroupInLandscape scans the landscape's System/Admin/User slices for the
// group with the given id, returning a pointer into a fresh copy (or nil if
// not found).
func findGroupInLandscape(land GroupLandscapeDTO, id string) *UserGroupDTO {
	for _, sections := range [][]UserGroupDTO{land.System, land.Admin, land.User} {
		for _, g := range sections {
			if g.ID == id {
				g := g
				return &g
			}
		}
	}
	return nil
}

// --- UserGroupDTO.CoupledProjects (spec 2026-08-09, Task 4) -----------------

func TestGroupDTO_ListsCoupledProjects(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")
	grp := e.mustCreateUserGroupOwnedBy(owner, "Team")
	p, err := e.svc.CreateProject(e.ctx, owner, CreateProjectInput{Name: "Coupled P", CoupledGroupID: grp.ID})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	land, err := e.svc.ListGroups(e.ctx, owner)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	dto := findGroupInLandscape(land, grp.ID)
	if dto == nil || len(dto.CoupledProjects) != 1 || dto.CoupledProjects[0].ID != p.ID || dto.CoupledProjects[0].Name != "Coupled P" {
		t.Fatalf("coupled_projects on group dto: %+v", dto)
	}
}

// TestGroupDTO_CoupledProjectsEmptyNotNil proves a group with no coupled
// project (and a system/admin-tier group, which can never be coupled) still
// serializes coupled_projects as `[]`, not `null` -- the Go-nil-slice ->
// JSON-null -> frontend-crash trap this field was explicitly hardened
// against (spec 2026-08-09, Task 4).
func TestGroupDTO_CoupledProjectsEmptyNotNil(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner2", "user")
	owner := token("usr_owner2")
	grp := e.mustCreateUserGroupOwnedBy(owner, "Lonely")
	land, err := e.svc.ListGroups(e.ctx, owner)
	if err != nil {
		t.Fatalf("ListGroups: %v", err)
	}
	dto := findGroupInLandscape(land, grp.ID)
	if dto == nil {
		t.Fatalf("group not found in landscape")
	}
	if dto.CoupledProjects == nil {
		t.Fatalf("CoupledProjects must be non-nil (would marshal to JSON null)")
	}
	if len(dto.CoupledProjects) != 0 {
		t.Fatalf("CoupledProjects = %+v, want empty", dto.CoupledProjects)
	}
	if b, err := json.Marshal(dto); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if strings.Contains(string(b), `"coupled_projects":null`) {
		t.Fatalf("coupled_projects marshaled as null: %s", b)
	}
	// The group's own admin/system ancestors (from mustCreateUserGroupOwnedBy)
	// are system/admin tier -- confirm they too report non-nil (never
	// queried, since only user-tier groups can be coupled).
	for _, sections := range [][]UserGroupDTO{land.System, land.Admin} {
		for _, g := range sections {
			if g.CoupledProjects == nil {
				t.Fatalf("system/admin group %q CoupledProjects is nil, want []", g.Name)
			}
		}
	}
}

// --- UpdateProject: coupled-project name uniqueness is per EFFECTIVE owner
// (fix round 2, 2026-08-09 review) ------------------------------------------

// TestUpdateProject_CoupledRenameAcrossDifferentOwnersAllowed is the
// regression test for the review's Minor finding: UpdateProject's
// name-uniqueness check called projectsOwnedBy(p.OwnerUserID), and for a
// COUPLED project p.OwnerUserID=="" (derived from the group) -- so
// projectsOwnedBy("") scanned ALL ownerless/coupled projects globally,
// collapsing every distinct group-owner's coupled projects into one
// uniqueness bucket. Two DIFFERENT users, each owning their own coupled
// project, must be able to use the SAME name -- and one of them must be able
// to RENAME into the other's name, since they have different effective
// owners.
func TestUpdateProject_CoupledRenameAcrossDifferentOwnersAllowed(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_a", "user")
	e.createUser("usr_b", "user")
	a := token("usr_a")
	b := token("usr_b")

	grpA := e.mustCreateUserGroupOwnedBy(a, "TeamA")
	grpB := e.mustCreateUserGroupOwnedBy(b, "TeamB")

	pA, err := e.svc.CreateProject(e.ctx, a, CreateProjectInput{Name: "Alpha", CoupledGroupID: grpA.ID})
	if err != nil {
		t.Fatalf("create coupled A: %v", err)
	}
	if _, err := e.svc.CreateProject(e.ctx, b, CreateProjectInput{Name: "Beta", CoupledGroupID: grpB.ID}); err != nil {
		t.Fatalf("create coupled B: %v", err)
	}

	// A renames "Alpha" -> "Beta" (B's coupled project's name): different
	// effective owners (usr_a vs usr_b) -> must SUCCEED. The pre-fix code
	// would have falsely rejected this with ErrProjectNameConflict, since
	// projectsOwnedBy("") returned BOTH projects (both stored owner=="").
	updated, err := e.svc.UpdateProject(e.ctx, a, pA.ID, "Beta", "")
	if err != nil {
		t.Fatalf("rename coupled project to a DIFFERENT owner's coupled-project name: got %v, want success", err)
	}
	if updated.Name != "Beta" {
		t.Fatalf("updated name = %q, want Beta", updated.Name)
	}
}

// TestUpdateProject_CoupledRenameCollisionSameOwnerRejected strengthens the
// fix: the SAME effective owner (usr_a, via TWO coupled projects under the
// SAME group) still correctly detects a REAL collision -- proving
// effectiveOwnedProjects (not just "always allow") drives the check.
func TestUpdateProject_CoupledRenameCollisionSameOwnerRejected(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_a", "user")
	a := token("usr_a")

	grpA := e.mustCreateUserGroupOwnedBy(a, "TeamA")

	pAlpha, err := e.svc.CreateProject(e.ctx, a, CreateProjectInput{Name: "Alpha", CoupledGroupID: grpA.ID})
	if err != nil {
		t.Fatalf("create coupled Alpha: %v", err)
	}
	if _, err := e.svc.CreateProject(e.ctx, a, CreateProjectInput{Name: "Gamma", CoupledGroupID: grpA.ID}); err != nil {
		t.Fatalf("create coupled Gamma: %v", err)
	}

	if _, err := e.svc.UpdateProject(e.ctx, a, pAlpha.ID, "Gamma", ""); !errors.Is(err, ErrProjectNameConflict) {
		t.Fatalf("rename to a sibling coupled project (same effective owner): got %v, want ErrProjectNameConflict", err)
	}
}

// TestUpdateProject_NormalProjectRenameUnaffected is a no-op-invariant
// regression check: the fix must not perturb the NORMAL (uncoupled)
// project-rename path (p.CoupledGroupID=="" still takes the unchanged
// projectsOwnedBy(p.OwnerUserID) branch).
func TestUpdateProject_NormalProjectRenameUnaffected(t *testing.T) {
	e := newProjectTestEnv(t)
	e.createUser("usr_owner", "user")
	owner := token("usr_owner")

	e.mustCreateProject(owner, "Taken", "")
	p := e.mustCreateProject(owner, "Free", "")

	if _, err := e.svc.UpdateProject(e.ctx, owner, p.ID, "StillFree", ""); err != nil {
		t.Fatalf("rename normal project to a free name: %v", err)
	}
	if _, err := e.svc.UpdateProject(e.ctx, owner, p.ID, "Taken", ""); !errors.Is(err, ErrProjectNameConflict) {
		t.Fatalf("rename normal project to a same-owner sibling name: got %v, want ErrProjectNameConflict", err)
	}
}

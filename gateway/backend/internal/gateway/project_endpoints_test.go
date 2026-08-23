// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

// doGroupReqRecorder issues method/path with an optional JSON body over
// cookie and returns the raw recorder WITHOUT asserting a status -- for
// tests that need to inspect a non-2xx response (a 404-no-leak, a 403, a
// 400 ambiguous-PATCH), mirroring doGroupReq's request-building but without
// the fixed wantStatus.
func doGroupReqRecorder(t *testing.T, srv *Server, cookie *http.Cookie, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	} else {
		reader = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(cookie)
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set(csrfHeaderName, "1")
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// TestProjectEndpointsFullLifecycle drives the whole /api/portal/projects*
// surface through one coherent scenario, mirroring
// TestGroupEndpointsFullLifecycle's structure: an owner creates a project,
// adds a member and a group, the landscape (GET /projects) shows it with
// can_manage, the token-picker list (GET /projects/mine) lists it, the
// candidates endpoint excludes current members, a non-owner/non-member gets
// a 404-no-leak on the members view, the owner renames it, transfers
// ownership (owner-only), and the new owner deletes it.
func TestProjectEndpointsFullLifecycle(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_member", "member@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_candidate", "candidate@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_stranger", "stranger@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")

	ownerSession := loginSession(t, srv, "owner@example.test", "password-1")
	memberSession := loginSession(t, srv, "member@example.test", "password-1")
	strangerSession := loginSession(t, srv, "stranger@example.test", "password-1")
	adminSession := loginSession(t, srv, "admin@example.test", "password-1")
	sysadminSession := loginSession(t, srv, "sysadmin@example.test", "password-1")
	// Creating a system-tier group requires the `system` scope, now
	// conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysadminSession, "password-1")

	// --- create (201). ---
	var p portal.ProjectDTO
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/projects",
		createProjectRequest{Name: "Launch", Description: "initial"}, http.StatusCreated, &p)
	if p.Name != "Launch" || p.Description != "initial" || p.OwnerUserID != "usr_owner" {
		t.Fatalf("create project = %+v", p)
	}
	if p.MyRole != "owner" || !p.CanManage {
		t.Fatalf("create project role/manage = %+v", p)
	}

	// A group the owner can assign to the project via AddProjectGroups: an
	// ADMIN-tier group the owner is a MEMBER of (visibleGroupIDs surfaces any
	// tier the principal belongs to, via myGroups -- but creating an
	// admin-tier group needs the creator to already be in a parent SYSTEM
	// group, so seed that chain first: sysadmin creates the system group +
	// seeds admin into it, admin creates the auto-parented admin group +
	// seeds the owner into it).
	var sysGroup portal.UserGroupDTO
	doGroupReq(t, srv, sysadminSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: "system", Name: "Launch System"}, http.StatusCreated, &sysGroup)
	// Seed BOTH admin and owner into the system group: admin's own
	// AddGroupMembers eligibility gate (VisibleUserIDs) for a plain "admin"
	// scope is "members of the system groups I belong to" -- so the owner
	// must already be in this system group before admin can add them to the
	// admin-tier group below.
	doGroupReq(t, srv, sysadminSession, http.MethodPost, "/api/portal/groups/"+sysGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_admin", "usr_owner", "usr_member"}}, http.StatusOK, nil)

	var grp portal.UserGroupDTO
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: "admin", Name: "Launch Group"}, http.StatusCreated, &grp)
	// usr_member must be in the SAME admin group too: a plain "user"
	// principal's VisibleUserIDs (AddProjectMembers' eligibility gate) is
	// "members of the admin groups I belong to" -- so the owner can only add
	// a member they co-share an admin group with.
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups/"+grp.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_owner", "usr_member"}}, http.StatusOK, nil)

	// --- add a member + a group. ---
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/projects/"+p.ID+"/members",
		projectMembersRequest{UserIDs: []string{"usr_member"}}, http.StatusOK, nil)
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/projects/"+p.ID+"/groups",
		projectGroupsRequest{GroupIDs: []string{grp.ID}}, http.StatusOK, nil)

	// --- GET /projects/{id}/members shows the roster (owner/admin only). ---
	var membersView portal.ProjectMembersDTO
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/projects/"+p.ID+"/members",
		nil, http.StatusOK, &membersView)
	if len(membersView.Users) != 1 || membersView.Users[0].ID != "usr_member" {
		t.Fatalf("members view users = %+v", membersView.Users)
	}
	if len(membersView.Groups) != 1 || membersView.Groups[0].ID != grp.ID {
		t.Fatalf("members view groups = %+v", membersView.Groups)
	}

	// --- GET /projects/{id}/candidates excludes the current member. ---
	var candidates struct {
		Users  []portal.UserRefDTO  `json:"users"`
		Groups []portal.GroupRefDTO `json:"groups"`
	}
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/projects/"+p.ID+"/candidates",
		nil, http.StatusOK, &candidates)
	for _, u := range candidates.Users {
		if u.ID == "usr_member" {
			t.Fatalf("current member leaked into candidates: %+v", candidates.Users)
		}
	}
	for _, g := range candidates.Groups {
		if g.ID == grp.ID {
			t.Fatalf("current group leaked into candidates: %+v", candidates.Groups)
		}
	}

	// --- GET /projects shows the project with can_manage for the owner AND
	// for the member (role=member, can_manage=false). ---
	var landscape struct {
		Data []portal.ProjectDTO `json:"data"`
	}
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/projects", nil, http.StatusOK, &landscape)
	found := false
	for _, d := range landscape.Data {
		if d.ID == p.ID {
			found = true
			if !d.CanManage || d.MyRole != "owner" {
				t.Fatalf("owner's project in landscape = %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("owner landscape = %+v, want project %s present", landscape.Data, p.ID)
	}

	doGroupReq(t, srv, memberSession, http.MethodGet, "/api/portal/projects", nil, http.StatusOK, &landscape)
	found = false
	for _, d := range landscape.Data {
		if d.ID == p.ID {
			found = true
			if d.CanManage || d.MyRole != "member" {
				t.Fatalf("member's project in landscape = %+v", d)
			}
		}
	}
	if !found {
		t.Fatalf("member landscape = %+v, want project %s present", landscape.Data, p.ID)
	}

	// --- GET /projects/mine (the token-assign picker's slim list). ---
	var mine struct {
		Data []portal.ProjectRefDTO `json:"data"`
	}
	doGroupReq(t, srv, memberSession, http.MethodGet, "/api/portal/projects/mine", nil, http.StatusOK, &mine)
	foundMine := false
	for _, m := range mine.Data {
		if m.ID == p.ID {
			foundMine = true
		}
	}
	if !foundMine {
		t.Fatalf("member's /projects/mine = %+v, want project %s present", mine.Data, p.ID)
	}

	// --- a stranger (not owner, not member, not admin) gets 404-no-leak on
	// the owner/admin-gated members view. ---
	rec := doGroupReqRecorder(t, srv, strangerSession, http.MethodGet, "/api/portal/projects/"+p.ID+"/members", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("stranger members view = %d, want 404 no-leak, body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "project.not_found" {
		t.Fatalf("stranger members view error code = %q, want project.not_found", code)
	}

	// --- rename (PATCH {name}). ---
	var renamed portal.ProjectDTO
	doGroupReq(t, srv, ownerSession, http.MethodPatch, "/api/portal/projects/"+p.ID,
		patchProjectRequest{Name: strPtr("Launch v2")}, http.StatusOK, &renamed)
	if renamed.Name != "Launch v2" {
		t.Fatalf("renamed project = %+v", renamed)
	}

	// --- PATCH with both name and owner_user_id is ambiguous (400). ---
	rec = doGroupReqRecorder(t, srv, ownerSession, http.MethodPatch, "/api/portal/projects/"+p.ID,
		patchProjectRequest{Name: strPtr("X"), OwnerUserID: strPtr("usr_member")})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous PATCH = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if code := errorCode(t, rec); code != "project.patch_ambiguous" {
		t.Fatalf("ambiguous PATCH error code = %q, want project.patch_ambiguous", code)
	}

	// --- transfer ownership (PATCH {owner_user_id}, owner-only). An admin
	// (who CAN see/manage-view the project via authorizeProjectManage) may
	// NOT transfer it -- narrower than "manage". ---
	rec = doGroupReqRecorder(t, srv, adminSession, http.MethodPatch, "/api/portal/projects/"+p.ID,
		patchProjectRequest{OwnerUserID: strPtr("usr_member")})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin transfer = %d, want 403, body=%s", rec.Code, rec.Body.String())
	}
	doGroupReq(t, srv, ownerSession, http.MethodPatch, "/api/portal/projects/"+p.ID,
		patchProjectRequest{OwnerUserID: strPtr("usr_member")}, http.StatusOK, nil)

	// --- remove the member/group (DELETE .../members/{userId},
	// .../groups/{groupId}), by the NEW owner (usr_member). ---
	doGroupReq(t, srv, memberSession, http.MethodDelete, "/api/portal/projects/"+p.ID+"/members/usr_member",
		nil, http.StatusOK, nil)
	doGroupReq(t, srv, memberSession, http.MethodDelete, "/api/portal/projects/"+p.ID+"/groups/"+grp.ID,
		nil, http.StatusOK, nil)

	// --- delete (by the new owner). ---
	doGroupReq(t, srv, memberSession, http.MethodDelete, "/api/portal/projects/"+p.ID, nil, http.StatusOK, nil)

	rec = doGroupReqRecorder(t, srv, memberSession, http.MethodGet, "/api/portal/projects/"+p.ID+"/members", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted project members view = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestProjectRoutesMineResolvesBeforeIDSubtree proves the exact-path
// registration ordering: GET /api/portal/projects/mine must resolve to
// handlePortalProjectsMine (200 + a list), never be parsed by
// handlePortalProjectItem as a project id of "mine" (which would 405, since
// that subtree's single-id branch only accepts PATCH/DELETE).
func TestProjectRoutesMineResolvesBeforeIDSubtree(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_a", "a@example.test", "password-1", "user")
	session := loginSession(t, srv, "a@example.test", "password-1")

	var mine struct {
		Data []portal.ProjectRefDTO `json:"data"`
	}
	doGroupReq(t, srv, session, http.MethodGet, "/api/portal/projects/mine", nil, http.StatusOK, &mine)
	if mine.Data == nil {
		t.Fatalf("GET /projects/mine data = nil, want a (possibly empty) array")
	}
}

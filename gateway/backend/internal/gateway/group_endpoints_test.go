// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/apierror"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// loginSession logs in email/password (seeded via seedLoginUser) and returns
// its session cookie, for driving the /api/portal/groups* handlers exactly
// as a browser would (requireWebScope resolves the principal's scopes from
// the session, which sessionPrincipal derives from the user's Role).
func loginSession(t *testing.T, srv *Server, email, password string) *http.Cookie {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(
		`{"email":"`+email+`","password":"`+password+`"}`,
	))
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login(%s) = %d, body=%s", email, rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec.Result())
}

// doGroupReq issues method/path with an optional JSON body over cookie,
// asserts the response status, and decodes the JSON body into out (if out is
// non-nil).
func doGroupReq(t *testing.T, srv *Server, cookie *http.Cookie, method, path string, body any, wantStatus int, out any) {
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
	if rec.Code != wantStatus {
		t.Fatalf("%s %s = %d, want %d, body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	if out != nil {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: unmarshal body %s: %v", method, path, rec.Body.String(), err)
		}
	}
}

// errorCode decodes an apierror.Body from a recorded response's bytes.
func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body apierror.Body
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal error body %s: %v", rec.Body.String(), err)
	}
	return body.Error.Code
}

// TestGroupEndpointsFullLifecycle drives the whole /api/portal/groups* surface
// through one coherent scenario: a system_admin creates a system group and
// seeds an admin into it; that admin creates an admin group (auto-parented on
// their sole system group) and seeds a user + an invitee into it; the user
// creates a user group (auto-parented on their sole admin group); the owner
// invites the invitee (user-tier -> pending, not direct); the invitee lists
// their own invitations (proving the exact-path GET /groups/invitations
// resolves to ListInvitations, not the {id} subtree) and accepts; the owner
// promotes the invitee to manager, demotes them, renames the group, transfers
// ownership to the (re-promoted) invitee, and the new owner deletes it.
func TestGroupEndpointsFullLifecycle(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_invitee", "invitee@example.test", "password-1", "user")

	sysadmin := loginSession(t, srv, "sysadmin@example.test", "password-1")
	// Creating a system-tier group requires the `system` scope, now
	// conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysadmin, "password-1")
	adminSession := loginSession(t, srv, "admin@example.test", "password-1")
	ownerSession := loginSession(t, srv, "owner@example.test", "password-1")
	inviteeSession := loginSession(t, srv, "invitee@example.test", "password-1")

	// --- system_admin creates a system group (201). ---
	var sysGroup portal.UserGroupDTO
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierSystem, Name: "Sys One"}, http.StatusCreated, &sysGroup)
	if sysGroup.Tier != store.GroupTierSystem || sysGroup.Name != "Sys One" {
		t.Fatalf("system group create = %+v", sysGroup)
	}

	// Seed the admin, owner, AND invitee into the system group (direct add,
	// system tier). The owner/invitee need this too: admin-tier membership
	// requires being a member of the admin group's PARENT SYSTEM group
	// (spec §5.2 containment, mirrored in AddGroupMembers's doc comment) --
	// so before they can join the admin group below, they must already be
	// system-group members.
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups/"+sysGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_admin", "usr_owner", "usr_invitee"}}, http.StatusOK, nil)

	// --- admin creates an admin group under their (sole) system group (201). ---
	var adminGroup portal.UserGroupDTO
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierAdmin, Name: "Admin One"}, http.StatusCreated, &adminGroup)
	if adminGroup.Tier != store.GroupTierAdmin || adminGroup.ParentGroupID != sysGroup.ID {
		t.Fatalf("admin group create = %+v, want parent %s", adminGroup, sysGroup.ID)
	}
	if adminGroup.OwnerUserID != "usr_admin" {
		t.Fatalf("admin group owner = %q, want usr_admin", adminGroup.OwnerUserID)
	}

	// Seed the owner AND the invitee into the admin group (direct add, admin tier).
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups/"+adminGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_owner", "usr_invitee"}}, http.StatusOK, nil)

	// --- a plain user creates a user group under their (sole) admin group. ---
	var userGroup portal.UserGroupDTO
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierUser, Name: "User One"}, http.StatusCreated, &userGroup)
	if userGroup.Tier != store.GroupTierUser || userGroup.ParentGroupID != adminGroup.ID {
		t.Fatalf("user group create = %+v, want parent %s", userGroup, adminGroup.ID)
	}
	if userGroup.OwnerUserID != "usr_owner" || userGroup.MyRole != "owner" || !userGroup.CanManage {
		t.Fatalf("user group owner/role = %+v", userGroup)
	}

	// candidates for the user group should include the invitee (a member of
	// the parent admin group, not yet a member of the user group).
	var candidatesResp struct {
		Data []portal.UserRefDTO `json:"data"`
	}
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/groups/"+userGroup.ID+"/candidates",
		nil, http.StatusOK, &candidatesResp)
	foundInvitee := false
	for _, c := range candidatesResp.Data {
		if c.ID == "usr_invitee" {
			foundInvitee = true
		}
	}
	if !foundInvitee {
		t.Fatalf("candidates for user group = %+v, want usr_invitee present", candidatesResp.Data)
	}

	// --- invite (user tier, POST members) -- goes to pending, NOT direct. ---
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups/"+userGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_invitee"}}, http.StatusOK, nil)

	// --- GET /api/portal/groups/invitations as the invitee (route-ordering
	// proof: this exact path must resolve to ListInvitations, not be parsed
	// as group id "invitations" by the {id} subtree). ---
	var invitationsResp struct {
		Data []portal.InvitationDTO `json:"data"`
	}
	doGroupReq(t, srv, inviteeSession, http.MethodGet, "/api/portal/groups/invitations",
		nil, http.StatusOK, &invitationsResp)
	if len(invitationsResp.Data) != 1 || invitationsResp.Data[0].GroupID != userGroup.ID {
		t.Fatalf("invitee invitations = %+v, want exactly one for group %s", invitationsResp.Data, userGroup.ID)
	}
	if invitationsResp.Data[0].InvitedBy != "usr_owner" {
		t.Fatalf("invitation invited_by = %q, want usr_owner", invitationsResp.Data[0].InvitedBy)
	}

	// The owner (a different principal) must see NO invitations of their own.
	var ownerInvitations struct {
		Data []portal.InvitationDTO `json:"data"`
	}
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/groups/invitations",
		nil, http.StatusOK, &ownerInvitations)
	if len(ownerInvitations.Data) != 0 {
		t.Fatalf("owner invitations = %+v, want none", ownerInvitations.Data)
	}

	// --- accept -> membership. ---
	doGroupReq(t, srv, inviteeSession, http.MethodPost, "/api/portal/groups/"+userGroup.ID+"/accept",
		nil, http.StatusOK, nil)

	// Now the pending invitation is gone.
	doGroupReq(t, srv, inviteeSession, http.MethodGet, "/api/portal/groups/invitations",
		nil, http.StatusOK, &invitationsResp)
	if len(invitationsResp.Data) != 0 {
		t.Fatalf("invitee invitations after accept = %+v, want none", invitationsResp.Data)
	}

	// --- promote the invitee to co-manager. ---
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups/"+userGroup.ID+"/managers",
		groupManagerRequest{UserID: "usr_invitee"}, http.StatusOK, nil)

	// --- demote them again (idempotent underneath; must succeed). ---
	doGroupReq(t, srv, ownerSession, http.MethodDelete, "/api/portal/groups/"+userGroup.ID+"/managers/usr_invitee",
		nil, http.StatusOK, nil)

	// --- PATCH rename. ---
	newName := "User One Renamed"
	var renamed portal.UserGroupDTO
	doGroupReq(t, srv, ownerSession, http.MethodPatch, "/api/portal/groups/"+userGroup.ID,
		patchGroupRequest{Name: &newName}, http.StatusOK, &renamed)
	if renamed.Name != newName {
		t.Fatalf("renamed group name = %q, want %q", renamed.Name, newName)
	}

	// --- PATCH transfer ownership requires the new owner to be a CURRENT
	// manager -- re-promote first, then transfer.
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups/"+userGroup.ID+"/managers",
		groupManagerRequest{UserID: "usr_invitee"}, http.StatusOK, nil)
	newOwnerID := "usr_invitee"
	doGroupReq(t, srv, ownerSession, http.MethodPatch, "/api/portal/groups/"+userGroup.ID,
		patchGroupRequest{OwnerUserID: &newOwnerID}, http.StatusOK, nil)

	// A PATCH carrying BOTH name and owner_user_id is rejected as ambiguous.
	ambiguousName := "Nope"
	ambiguousOwner := "usr_owner"
	ambigReq := httptest.NewRequest(http.MethodPatch, "/api/portal/groups/"+userGroup.ID,
		strings.NewReader(`{"name":"`+ambiguousName+`","owner_user_id":"`+ambiguousOwner+`"}`))
	ambigReq.AddCookie(ownerSession)
	ambigReq.Header.Set(csrfHeaderName, "1")
	ambigRec := httptest.NewRecorder()
	srv.ServeHTTP(ambigRec, ambigReq)
	if ambigRec.Code != http.StatusBadRequest {
		t.Fatalf("ambiguous PATCH = %d, want 400, body=%s", ambigRec.Code, ambigRec.Body.String())
	}
	if code := errorCode(t, ambigRec); code != "group.patch_ambiguous" {
		t.Fatalf("ambiguous PATCH error code = %q, want group.patch_ambiguous", code)
	}

	// The ORIGINAL owner (usr_owner) no longer owns the group and is not a
	// manager, so they can no longer delete it -- 404 (no-leak: they can still
	// see the group as a plain member via the landscape, but authorizeGroupManage
	// only recognizes owner/manager/system_admin here).
	forbiddenDeleteReq := httptest.NewRequest(http.MethodDelete, "/api/portal/groups/"+userGroup.ID, nil)
	forbiddenDeleteReq.AddCookie(ownerSession)
	forbiddenDeleteReq.Header.Set(csrfHeaderName, "1")
	forbiddenDeleteRec := httptest.NewRecorder()
	srv.ServeHTTP(forbiddenDeleteRec, forbiddenDeleteReq)
	if forbiddenDeleteRec.Code != http.StatusNotFound {
		t.Fatalf("delete by ex-owner = %d, want 404, body=%s", forbiddenDeleteRec.Code, forbiddenDeleteRec.Body.String())
	}

	// --- the NEW owner (the invitee) deletes the group. ---
	doGroupReq(t, srv, inviteeSession, http.MethodDelete, "/api/portal/groups/"+userGroup.ID,
		nil, http.StatusOK, nil)

	// --- 404-no-leak: an outsider (a plain user with no relation to the
	// system group) cannot even discover it exists via candidates. ---
	seedLoginUser(t, dir, "usr_outsider", "outsider@example.test", "password-1", "user")
	outsiderSession := loginSession(t, srv, "outsider@example.test", "password-1")
	outsiderReq := httptest.NewRequest(http.MethodGet, "/api/portal/groups/"+sysGroup.ID+"/candidates", nil)
	outsiderReq.AddCookie(outsiderSession)
	outsiderRec := httptest.NewRecorder()
	srv.ServeHTTP(outsiderRec, outsiderReq)
	if outsiderRec.Code != http.StatusNotFound {
		t.Fatalf("outsider candidates on system group = %d, want 404, body=%s", outsiderRec.Code, outsiderRec.Body.String())
	}
	if code := errorCode(t, outsiderRec); code != "group.not_found" {
		t.Fatalf("outsider candidates error code = %q, want group.not_found", code)
	}
}

// TestGroupEndpointsListLandscape smoke-tests GET /api/portal/groups: a
// system_admin sees the system group; a plain user with no groups sees empty
// lists everywhere (never an error).
func TestGroupEndpointsListLandscape(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_lonely", "lonely@example.test", "password-1", "user")

	sysadmin := loginSession(t, srv, "sysadmin@example.test", "password-1")
	// Creating a system-tier group requires the `system` scope, now
	// conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysadmin, "password-1")
	lonely := loginSession(t, srv, "lonely@example.test", "password-1")

	var created portal.UserGroupDTO
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierSystem, Name: "Landscape Sys"}, http.StatusCreated, &created)

	var landscape portal.GroupLandscapeDTO
	doGroupReq(t, srv, sysadmin, http.MethodGet, "/api/portal/groups", nil, http.StatusOK, &landscape)
	found := false
	for _, g := range landscape.System {
		if g.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("system_admin landscape.system = %+v, want to contain %s", landscape.System, created.ID)
	}

	var lonelyLandscape portal.GroupLandscapeDTO
	doGroupReq(t, srv, lonely, http.MethodGet, "/api/portal/groups", nil, http.StatusOK, &lonelyLandscape)
	if len(lonelyLandscape.System) != 0 || len(lonelyLandscape.Admin) != 0 || len(lonelyLandscape.User) != 0 {
		t.Fatalf("lonely user landscape = %+v, want all empty (system tier invisible, no admin/user groups)", lonelyLandscape)
	}
}

// TestGroupEndpointsCreateRequiresAuth confirms the routes are behind
// requireWebScope(gateway:use) -- an unauthenticated request is rejected,
// never silently treated as anonymous.
func TestGroupEndpointsCreateRequiresAuth(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/groups", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/portal/groups = %d, want 401", rec.Code)
	}
}

// TestGroupEndpointsErrorMapping exercises the sentinel-to-HTTP-status
// mapping directly: creating a group with an invalid tier is 400
// group.tier_invalid; creating a duplicate system group name is 409
// group.name_conflict.
func TestGroupEndpointsErrorMapping(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	sysadmin := loginSession(t, srv, "sysadmin@example.test", "password-1")
	// Creating a system-tier group requires the `system` scope, now
	// conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysadmin, "password-1")

	// Invalid tier -> 400 group.tier_invalid.
	badTierReq := httptest.NewRequest(http.MethodPost, "/api/portal/groups", strings.NewReader(`{"tier":"bogus","name":"X"}`))
	badTierReq.AddCookie(sysadmin)
	badTierReq.Header.Set(csrfHeaderName, "1")
	badTierRec := httptest.NewRecorder()
	srv.ServeHTTP(badTierRec, badTierReq)
	if badTierRec.Code != http.StatusBadRequest {
		t.Fatalf("bad tier create = %d, want 400, body=%s", badTierRec.Code, badTierRec.Body.String())
	}
	if code := errorCode(t, badTierRec); code != "group.tier_invalid" {
		t.Fatalf("bad tier error code = %q, want group.tier_invalid", code)
	}

	// First system group with a given name succeeds.
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierSystem, Name: "Dup"}, http.StatusCreated, nil)

	// A second system group with the SAME name (case-insensitive) conflicts.
	dupReq := httptest.NewRequest(http.MethodPost, "/api/portal/groups", strings.NewReader(`{"tier":"system","name":"DUP"}`))
	dupReq.AddCookie(sysadmin)
	dupReq.Header.Set(csrfHeaderName, "1")
	dupRec := httptest.NewRecorder()
	srv.ServeHTTP(dupRec, dupReq)
	if dupRec.Code != http.StatusConflict {
		t.Fatalf("duplicate name create = %d, want 409, body=%s", dupRec.Code, dupRec.Body.String())
	}
	if code := errorCode(t, dupRec); code != "group.name_conflict" {
		t.Fatalf("duplicate name error code = %q, want group.name_conflict", code)
	}

	// DELETE on an unknown group id -> 404 group.not_found.
	notFoundReq := httptest.NewRequest(http.MethodDelete, "/api/portal/groups/ugrp_does_not_exist", nil)
	notFoundReq.AddCookie(sysadmin)
	notFoundReq.Header.Set(csrfHeaderName, "1")
	notFoundRec := httptest.NewRecorder()
	srv.ServeHTTP(notFoundRec, notFoundReq)
	if notFoundRec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown group = %d, want 404, body=%s", notFoundRec.Code, notFoundRec.Body.String())
	}
	if code := errorCode(t, notFoundRec); code != "group.not_found" {
		t.Fatalf("delete unknown group error code = %q, want group.not_found", code)
	}
}

// TestGroupEndpointsMethodNotAllowed pins the Allow-header/405 behavior on
// the two multi-method handlers.
func TestGroupEndpointsMethodNotAllowed(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "u1@example.test", "password-1", "user")
	session := loginSession(t, srv, "u1@example.test", "password-1")

	req := httptest.NewRequest(http.MethodDelete, "/api/portal/groups", nil)
	req.AddCookie(session)
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /api/portal/groups = %d, want 405", rec.Code)
	}
}

// TestGroupEndpointsMembersList exercises GET /api/portal/groups/{id}/members
// (Task 14b): a system_admin creates a system group, seeds an owner + a
// member; the owner lists the roster (200, both rows present with identity);
// a third principal with no owner/manager/system_admin standing on the group
// gets the same 404-no-leak as a nonexistent group (never 403). It also
// confirms POST members (add) still works on the SAME route (GET/POST share
// handlePortalGroupMembers).
func TestGroupEndpointsMembersList(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_outsider", "outsider@example.test", "password-1", "user")

	sysadmin := loginSession(t, srv, "sysadmin@example.test", "password-1")
	// Creating a system-tier group requires the `system` scope, now
	// conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysadmin, "password-1")
	adminSession := loginSession(t, srv, "admin@example.test", "password-1")
	ownerSession := loginSession(t, srv, "owner@example.test", "password-1")
	outsiderSession := loginSession(t, srv, "outsider@example.test", "password-1")

	// A system-tier group: the admin (a member) manages nothing here, but
	// system_admin does -- so system_admin both lists AND populates it.
	var sysGroup portal.UserGroupDTO
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierSystem, Name: "Members List Sys"}, http.StatusCreated, &sysGroup)
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups/"+sysGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_admin", "usr_owner"}}, http.StatusOK, nil)

	// system_admin (owner/manager equivalent for system tier) lists the roster.
	var sysRoster struct {
		Data []portal.UserGroupMemberDTO `json:"data"`
	}
	doGroupReq(t, srv, sysadmin, http.MethodGet, "/api/portal/groups/"+sysGroup.ID+"/members",
		nil, http.StatusOK, &sysRoster)
	if len(sysRoster.Data) != 2 {
		t.Fatalf("system group roster = %+v, want two member rows (usr_admin+usr_owner)", sysRoster.Data)
	}
	sysByID := map[string]portal.UserGroupMemberDTO{}
	for _, m := range sysRoster.Data {
		sysByID[m.UserID] = m
	}
	if admin, ok := sysByID["usr_admin"]; !ok || admin.State != store.GroupStateMember || admin.Email == "" {
		t.Fatalf("system group roster admin row = %+v, want state=member with identity", sysByID["usr_admin"])
	}
	if owner, ok := sysByID["usr_owner"]; !ok || owner.State != store.GroupStateMember {
		t.Fatalf("system group roster owner row = %+v, want state=member", sysByID["usr_owner"])
	}

	// A plain admin (member, not system_admin) has no standing on the system
	// group -- 404-no-leak, not 403.
	adminListReq := httptest.NewRequest(http.MethodGet, "/api/portal/groups/"+sysGroup.ID+"/members", nil)
	adminListReq.AddCookie(adminSession)
	adminListRec := httptest.NewRecorder()
	srv.ServeHTTP(adminListRec, adminListReq)
	if adminListRec.Code != http.StatusNotFound {
		t.Fatalf("admin (non-system_admin) list system-group members = %d, want 404, body=%s", adminListRec.Code, adminListRec.Body.String())
	}
	if code := errorCode(t, adminListRec); code != "group.not_found" {
		t.Fatalf("admin list error code = %q, want group.not_found", code)
	}

	// An admin-tier group owned by usr_admin (auto-parented on the system
	// group above, since usr_admin is its sole member) -- the OWNER lists it.
	var adminGroup portal.UserGroupDTO
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierAdmin, Name: "Members List Admin"}, http.StatusCreated, &adminGroup)
	doGroupReq(t, srv, adminSession, http.MethodPost, "/api/portal/groups/"+adminGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_owner"}}, http.StatusOK, nil)

	var adminRoster struct {
		Data []portal.UserGroupMemberDTO `json:"data"`
	}
	doGroupReq(t, srv, adminSession, http.MethodGet, "/api/portal/groups/"+adminGroup.ID+"/members",
		nil, http.StatusOK, &adminRoster)
	byID := map[string]portal.UserGroupMemberDTO{}
	for _, m := range adminRoster.Data {
		byID[m.UserID] = m
	}
	if owner, ok := byID["usr_admin"]; !ok || !owner.IsOwner {
		t.Fatalf("admin group roster owner row = %+v, want is_owner=true", byID["usr_admin"])
	}
	if member, ok := byID["usr_owner"]; !ok || member.IsOwner || member.IsManager {
		t.Fatalf("admin group roster member row = %+v, want plain member", byID["usr_owner"])
	}

	// usr_owner is a plain member (not owner/manager) -- also 404-no-leak.
	ownerListReq := httptest.NewRequest(http.MethodGet, "/api/portal/groups/"+adminGroup.ID+"/members", nil)
	ownerListReq.AddCookie(ownerSession)
	ownerListRec := httptest.NewRecorder()
	srv.ServeHTTP(ownerListRec, ownerListReq)
	if ownerListRec.Code != http.StatusNotFound {
		t.Fatalf("plain member list admin-group members = %d, want 404, body=%s", ownerListRec.Code, ownerListRec.Body.String())
	}

	// A total outsider gets the same 404-no-leak.
	outsiderListReq := httptest.NewRequest(http.MethodGet, "/api/portal/groups/"+adminGroup.ID+"/members", nil)
	outsiderListReq.AddCookie(outsiderSession)
	outsiderListRec := httptest.NewRecorder()
	srv.ServeHTTP(outsiderListRec, outsiderListReq)
	if outsiderListRec.Code != http.StatusNotFound {
		t.Fatalf("outsider list admin-group members = %d, want 404, body=%s", outsiderListRec.Code, outsiderListRec.Body.String())
	}
}

// TestCreateAdminGroupForAnotherOwnerInvalidCode drives POST /api/portal/groups
// with an owner_user_id an ELEVATED system_admin cannot assign (a nonexistent
// user), asserting the 400 group.owner_invalid mapping end-to-end through the
// HTTP layer (the service-level matrix lives in
// TestCreateAdminGroupForAnotherOwnerRejections).
func TestCreateAdminGroupForAnotherOwnerInvalidCode(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_s", "s@example.test", "password-123", "system_admin")
	cookie := loginAs(t, srv, "s@example.test", "password-123")
	elevateSystemAdmin(t, srv, cookie, "password-123") // enter system-admin mode
	// A non-existent/invalid owner -> 400 group.owner_invalid.
	rec := doJSON(t, srv, cookie, http.MethodPost, "/api/portal/groups",
		`{"tier":"admin","name":"AG","owner_user_id":"usr_missing"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "group.owner_invalid") {
		t.Fatalf("invalid owner = %d %s, want 400 group.owner_invalid", rec.Code, rec.Body.String())
	}
}

func TestAdminOwnerCandidatesEndpoint(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_s", "s@example.test", "password-123", "system_admin")
	seedLoginUser(t, dir, "usr_a", "a@example.test", "password-123", "admin")
	sysCookie := loginAs(t, srv, "s@example.test", "password-123")
	admCookie := loginAs(t, srv, "a@example.test", "password-123")

	// plain admin -> 403.
	if code := getStatus(t, srv, admCookie, "/api/portal/admin-owner-candidates"); code != http.StatusForbidden {
		t.Fatalf("admin candidates = %d, want 403", code)
	}
	// non-elevated system_admin -> 403 (no system scope yet).
	if code := getStatus(t, srv, sysCookie, "/api/portal/admin-owner-candidates"); code != http.StatusForbidden {
		t.Fatalf("non-elevated system_admin candidates = %d, want 403", code)
	}
	// elevated -> 200.
	elevateSystemAdmin(t, srv, sysCookie, "password-123")
	if code := getStatus(t, srv, sysCookie, "/api/portal/admin-owner-candidates"); code != http.StatusOK {
		t.Fatalf("elevated system_admin candidates = %d, want 200", code)
	}
}

// TestGroupManagerPermissionsEndToEnd drives the full per-Admin-Group
// co-manager permissions wire path (spec 2026-08-10): POST .../managers
// promotes with explicit flags (proving groupManagerRequest.CanManageUsers/
// CanManageGroup actually reach portal.Service.PromoteManager, not just the
// user_id), PATCH .../managers/{userId} narrows/widens an EXISTING
// co-manager (the NEW route), and GET .../members reflects the stored flags
// end-to-end through the HTTP layer.
func TestGroupManagerPermissionsEndToEnd(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_mgr", "mgr@example.test", "password-1", "admin")

	sysadmin := loginSession(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysadmin, "password-1")
	ownerSession := loginSession(t, srv, "owner@example.test", "password-1")

	var sysGroup portal.UserGroupDTO
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierSystem, Name: "PermsSys"}, http.StatusCreated, &sysGroup)
	doGroupReq(t, srv, sysadmin, http.MethodPost, "/api/portal/groups/"+sysGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_owner", "usr_mgr"}}, http.StatusOK, nil)

	var adminGroup portal.UserGroupDTO
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups",
		createGroupRequest{Tier: store.GroupTierAdmin, Name: "PermsAdmin"}, http.StatusCreated, &adminGroup)
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups/"+adminGroup.ID+"/members",
		groupMembersRequest{UserIDs: []string{"usr_mgr"}}, http.StatusOK, nil)

	// POST .../managers with explicit flags: CanManageUsers only.
	doGroupReq(t, srv, ownerSession, http.MethodPost, "/api/portal/groups/"+adminGroup.ID+"/managers",
		groupManagerRequest{UserID: "usr_mgr", CanManageUsers: true, CanManageGroup: false}, http.StatusOK, nil)

	type membersResp struct {
		Data []portal.UserGroupMemberDTO `json:"data"`
	}
	findMgr := func(resp membersResp) *portal.UserGroupMemberDTO {
		for i := range resp.Data {
			if resp.Data[i].UserID == "usr_mgr" {
				return &resp.Data[i]
			}
		}
		return nil
	}

	var afterPromote membersResp
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/groups/"+adminGroup.ID+"/members",
		nil, http.StatusOK, &afterPromote)
	row := findMgr(afterPromote)
	if row == nil || !row.IsManager || !row.CanManageUsers || row.CanManageGroup {
		t.Fatalf("row after POST managers = %+v, want is_manager=true can_manage_users=true can_manage_group=false", row)
	}

	// PATCH .../managers/{userId} widens usr_mgr to CanManageGroup too.
	doGroupReq(t, srv, ownerSession, http.MethodPatch, "/api/portal/groups/"+adminGroup.ID+"/managers/usr_mgr",
		groupManagerRequest{CanManageUsers: true, CanManageGroup: true}, http.StatusOK, nil)

	var afterPatch membersResp
	doGroupReq(t, srv, ownerSession, http.MethodGet, "/api/portal/groups/"+adminGroup.ID+"/members",
		nil, http.StatusOK, &afterPatch)
	row = findMgr(afterPatch)
	if row == nil || !row.CanManageUsers || !row.CanManageGroup {
		t.Fatalf("row after PATCH managers = %+v, want can_manage_users=true can_manage_group=true", row)
	}

	// PATCH on a non-manager target -> 400 group.candidate_invalid.
	patchInvalidReq := httptest.NewRequest(http.MethodPatch, "/api/portal/groups/"+adminGroup.ID+"/managers/usr_owner",
		strings.NewReader(`{"can_manage_users":true,"can_manage_group":true}`))
	patchInvalidReq.AddCookie(ownerSession)
	patchInvalidReq.Header.Set(csrfHeaderName, "1")
	patchInvalidRec := httptest.NewRecorder()
	srv.ServeHTTP(patchInvalidRec, patchInvalidReq)
	if patchInvalidRec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH non-manager target = %d, want 400, body=%s", patchInvalidRec.Code, patchInvalidRec.Body.String())
	}
	if code := errorCode(t, patchInvalidRec); code != "group.candidate_invalid" {
		t.Fatalf("PATCH non-manager target error code = %q, want group.candidate_invalid", code)
	}

	// A co-manager (usr_mgr, even with full flags now) cannot PATCH -- 403
	// Forbidden, owner/system_admin only.
	mgrSession := loginSession(t, srv, "mgr@example.test", "password-1")
	patchForbiddenReq := httptest.NewRequest(http.MethodPatch, "/api/portal/groups/"+adminGroup.ID+"/managers/usr_mgr",
		strings.NewReader(`{"can_manage_users":false,"can_manage_group":false}`))
	patchForbiddenReq.AddCookie(mgrSession)
	patchForbiddenReq.Header.Set(csrfHeaderName, "1")
	patchForbiddenRec := httptest.NewRecorder()
	srv.ServeHTTP(patchForbiddenRec, patchForbiddenReq)
	if patchForbiddenRec.Code != http.StatusForbidden {
		t.Fatalf("co-manager PATCH = %d, want 403, body=%s", patchForbiddenRec.Code, patchForbiddenRec.Body.String())
	}

	// Wrong method on .../managers/{userId} -> 405 with an Allow header.
	wrongMethodReq := httptest.NewRequest(http.MethodPost, "/api/portal/groups/"+adminGroup.ID+"/managers/usr_mgr", nil)
	wrongMethodReq.AddCookie(ownerSession)
	wrongMethodReq.Header.Set(csrfHeaderName, "1")
	wrongMethodRec := httptest.NewRecorder()
	srv.ServeHTTP(wrongMethodRec, wrongMethodReq)
	if wrongMethodRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong method on managers/{userId} = %d, want 405, body=%s", wrongMethodRec.Code, wrongMethodRec.Body.String())
	}
}

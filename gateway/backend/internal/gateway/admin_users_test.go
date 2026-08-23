// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/mail"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/totp"
	"strings"
	"testing"
	"time"
)

func loginAs(t *testing.T, srv *Server, email, password string) *http.Cookie {
	t.Helper()
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"`+email+`","password":"`+password+`"}`))
	login.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, login)
	if rec.Code != http.StatusOK {
		t.Fatalf("login %s failed: %d %s", email, rec.Code, rec.Body.String())
	}
	return sessionCookie(t, rec.Result())
}

func TestAdminCreateListUser(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	// system_admin, not a plain admin: this test exercises create+list
	// mechanics, not the group-visibility scoping (Task 5) -- a plain admin
	// with no system-group membership would not see the freshly created user
	// (by design, see TestAdminUsersListScopedForPlainAdmin), so an ELEVATED
	// system_admin (who sees everyone -- the `system` scope, now conditional
	// on System-Admin step-up mode) keeps this test's intent intact.
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "system_admin")
	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_sg", "usr_admin")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	elevateSystemAdmin(t, srv, cookie, "password-1")

	create := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{"email":"new@example.test","display_name":"New","role":"user","admin_group_id":"ugrp_ag"}`))
	create.Header.Set(csrfHeaderName, "1")
	create.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create user should be 201, got %d body=%s", createRec.Code, createRec.Body.String())
	}
	if !strings.Contains(createRec.Body.String(), "/set-password?token=") {
		t.Fatalf("create response should include an invite url, got %s", createRec.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	list.AddCookie(cookie)
	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "new@example.test") {
		t.Fatalf("list users failed: %d %s", listRec.Code, listRec.Body.String())
	}
	// No secrets or hashes leak.
	if strings.Contains(listRec.Body.String(), "password_hash") || strings.Contains(listRec.Body.String(), "secret") {
		t.Fatalf("user list must not leak credentials: %s", listRec.Body.String())
	}
}

func TestAdminCreateUserEmailsInvite(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	// A plain admin must be a member of a system group to invite (Task 5b);
	// this test's intent is the invite-email behavior, not group scoping.
	createSystemGroupForTest(t, dir, "ugrp_email_invite", "Email Invite")
	addSystemGroupMember(t, dir, "ugrp_email_invite", "usr_admin")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_email_invite", "usr_admin")
	enableSMTP(t, srv)
	rec := &recordingMailer{}
	srv.newMailer = func(mail.Config) Mailer { return rec }
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	create := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{"email":"new@example.test","display_name":"New","role":"user","admin_group_id":"ugrp_ag"}`))
	create.Header.Set(csrfHeaderName, "1")
	create.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", createRec.Code, createRec.Body.String())
	}
	var body struct {
		InviteURL  string `json:"invite_url"`
		EmailSent  bool   `json:"email_sent"`
		EmailError string `json:"email_error"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.EmailSent || body.EmailError != "" || rec.sends != 1 {
		t.Fatalf("got sent=%v err=%q sends=%d, want true/empty/1", body.EmailSent, body.EmailError, rec.sends)
	}
	if body.InviteURL == "" || rec.to != "new@example.test" {
		t.Fatalf("invite_url=%q recipient=%q", body.InviteURL, rec.to)
	}
}

func TestAdminCreateUserSMTPDisabledStillCreates(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	// A plain admin must be a member of a system group to invite (Task 5b);
	// this test's intent is the SMTP-disabled create behavior, not group
	// scoping.
	createSystemGroupForTest(t, dir, "ugrp_smtp_disabled", "SMTP Disabled")
	addSystemGroupMember(t, dir, "ugrp_smtp_disabled", "usr_admin")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_smtp_disabled", "usr_admin")
	rec := &recordingMailer{}
	srv.newMailer = func(mail.Config) Mailer { return rec }
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	create := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{"email":"new@example.test","display_name":"New","role":"user","admin_group_id":"ugrp_ag"}`))
	create.Header.Set(csrfHeaderName, "1")
	create.AddCookie(cookie)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create = %d body=%s", createRec.Code, createRec.Body.String())
	}
	if !strings.Contains(createRec.Body.String(), "/set-password?token=") || strings.Contains(createRec.Body.String(), `"email_sent":true`) || rec.sends != 0 {
		t.Fatalf("disabled create must still return the link and email_sent=false, no send: %s", createRec.Body.String())
	}
}

func TestAdminReissueInviteEmails(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	// usr_admin must manage usr_target (Task 3: item ops are now scoped to
	// ManageableUserIDs) -- owns an admin group with usr_target as a member.
	createSystemGroupForTest(t, dir, "ugrp_reissue_sg", "Reissue SG")
	createAdminGroupForTest(t, dir, "ugrp_reissue_ag", "Reissue AG", "ugrp_reissue_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_reissue_ag", "usr_target")
	enableSMTP(t, srv)
	rec := &recordingMailer{}
	srv.newMailer = func(mail.Config) Mailer { return rec }
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/usr_target/invite", nil)
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"email_sent":true`) || rec.to != "target@example.test" {
		t.Fatalf("reissue = %d body=%s recipient=%q", rr.Code, rr.Body.String(), rec.to)
	}
}

func TestAdminUsersRequireAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_user", "user@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "user@example.test", "password-1")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin should be 403, got %d", rec.Code)
	}
}

func TestAdminPatchUserLastAdminGuard(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	patch := httptest.NewRequest(http.MethodPatch, "/api/admin/users/usr_admin", strings.NewReader(`{"role":"user"}`))
	patch.Header.Set(csrfHeaderName, "1")
	patch.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, patch)
	if rec.Code != http.StatusConflict {
		t.Fatalf("demoting last admin should be 409, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminPatchSystemAdminRequiresSystemActor(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	// usr_admin must manage usr_target (Task 3: item ops are now scoped to
	// ManageableUserIDs) so the request reaches the role check under test,
	// rather than 404-no-leaking before it.
	createSystemGroupForTest(t, dir, "ugrp_patch_sysactor_sg", "Patch SysActor SG")
	createAdminGroupForTest(t, dir, "ugrp_patch_sysactor_ag", "Patch SysActor AG", "ugrp_patch_sysactor_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_patch_sysactor_ag", "usr_target")

	// A plain admin cannot promote a user to system_admin.
	adminCookie := loginAs(t, srv, "admin@example.test", "password-1")
	denied := httptest.NewRequest(http.MethodPatch, "/api/admin/users/usr_target", strings.NewReader(`{"role":"system_admin"}`))
	denied.Header.Set(csrfHeaderName, "1")
	denied.AddCookie(adminCookie)
	deniedRec := httptest.NewRecorder()
	srv.ServeHTTP(deniedRec, denied)
	if deniedRec.Code != http.StatusForbidden || !strings.Contains(deniedRec.Body.String(), "admin.system_admin_forbidden") {
		t.Fatalf("plain admin promoting to system_admin should be 403 admin.system_admin_forbidden, got %d body=%s", deniedRec.Code, deniedRec.Body.String())
	}

	// An ELEVATED system_admin can (promoting to system_admin requires the
	// `system` scope, now conditional on System-Admin step-up mode).
	sysCookie := loginAs(t, srv, "sys@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")
	allowed := httptest.NewRequest(http.MethodPatch, "/api/admin/users/usr_target", strings.NewReader(`{"role":"system_admin"}`))
	allowed.Header.Set(csrfHeaderName, "1")
	allowed.AddCookie(sysCookie)
	allowedRec := httptest.NewRecorder()
	srv.ServeHTTP(allowedRec, allowed)
	if allowedRec.Code != http.StatusOK || !strings.Contains(allowedRec.Body.String(), `"role":"system_admin"`) {
		t.Fatalf("system_admin promoting to system_admin should be 200 with role system_admin, got %d body=%s", allowedRec.Code, allowedRec.Body.String())
	}
}

// seedOwnedGroupForSuccessionTest creates a system group SG (owner/members
// managed by sysAdminID) and, hanging under it, an admin group owned by
// ownerID with memberID as a plain member -- the minimal shape owner
// succession (Task 6-8's ReassignGroupsOwnedBy) needs to have something to
// reassign. Returns the admin group's id.
func seedOwnedGroupForSuccessionTest(t *testing.T, srv *Server, sysAdminID, ownerID, memberID string) string {
	t.Helper()
	ctx := context.Background()
	sysAdmin := auth.Token{UserID: sysAdminID, Scopes: []string{"system", "admin"}}
	owner := auth.Token{UserID: ownerID, Scopes: []string{"admin"}}

	sg, err := srv.Portal.CreateGroup(ctx, sysAdmin, portal.CreateGroupInput{Tier: store.GroupTierSystem, Name: "Succession SG"})
	if err != nil {
		t.Fatalf("create system group: %v", err)
	}
	if err := srv.Portal.AddGroupMembers(ctx, sysAdmin, sg.ID, []string{ownerID, memberID}); err != nil {
		t.Fatalf("add system group members: %v", err)
	}
	ag, err := srv.Portal.CreateGroup(ctx, owner, portal.CreateGroupInput{Tier: store.GroupTierAdmin, Name: "Succession AG"})
	if err != nil {
		t.Fatalf("create admin group: %v", err)
	}
	if err := srv.Portal.AddGroupMembers(ctx, owner, ag.ID, []string{memberID}); err != nil {
		t.Fatalf("add admin group member: %v", err)
	}
	if ag.OwnerUserID != ownerID {
		t.Fatalf("precondition: %s should own the admin group, got %+v", ownerID, ag)
	}
	return ag.ID
}

// TestAdminPatchDisableUserTriggersOwnerSuccession proves the Task 11 wiring:
// disabling a user via the admin PATCH endpoint runs owner succession
// (ReassignGroupsOwnedBy) for every group they own, so a disabled owner's
// groups don't get stuck with an inactive owner.
func TestAdminPatchDisableUserTriggersOwnerSuccession(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "admin")
	// usr_member must itself carry an admin-capable role: succeedAdminGroupOwner
	// never falls back to a plain "user" member (spec §8.1) -- only a manager
	// or an admin-role member is an eligible successor.
	seedLoginUser(t, dir, "usr_member", "member@example.test", "password-1", "admin")
	groupID := seedOwnedGroupForSuccessionTest(t, srv, "usr_admin", "usr_owner", "usr_member")

	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	// The acting principal is a system_admin but not yet elevated (Task 3:
	// item ops are now scoped to ManageableUserIDs for a non-`system` caller,
	// and usr_admin has no admin-group relationship to usr_owner) -- elevate
	// so the `system` scope bypasses the gate, matching this test's intent
	// (a system_admin disabling ANY user).
	elevateSystemAdmin(t, srv, cookie, "password-1")
	patch := httptest.NewRequest(http.MethodPatch, "/api/admin/users/usr_owner", strings.NewReader(`{"status":"disabled"}`))
	patch.Header.Set(csrfHeaderName, "1")
	patch.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable user = %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := dir.UserGroupByID(context.Background(), groupID)
	if err != nil {
		t.Fatalf("UserGroupByID: %v", err)
	}
	if got.OwnerUserID != "usr_member" {
		t.Fatalf("group owner after disabling the old owner = %q, want usr_member (succession should have run)", got.OwnerUserID)
	}
}

// TestAdminPatchNonDisableUpdateDoesNotTriggerOwnerSuccession is the negative
// case: a PATCH that does NOT set status to disabled (here, a display-name
// rename) must leave group ownership untouched.
func TestAdminPatchNonDisableUpdateDoesNotTriggerOwnerSuccession(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_owner", "owner@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_member", "member@example.test", "password-1", "user")
	groupID := seedOwnedGroupForSuccessionTest(t, srv, "usr_admin", "usr_owner", "usr_member")

	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	// See the disable-test's comment above: elevate so the `system` scope
	// bypasses the Task 3 ManageableUserIDs gate.
	elevateSystemAdmin(t, srv, cookie, "password-1")
	patch := httptest.NewRequest(http.MethodPatch, "/api/admin/users/usr_owner", strings.NewReader(`{"display_name":"Renamed Owner"}`))
	patch.Header.Set(csrfHeaderName, "1")
	patch.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, patch)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename user = %d body=%s", rec.Code, rec.Body.String())
	}

	got, err := dir.UserGroupByID(context.Background(), groupID)
	if err != nil {
		t.Fatalf("UserGroupByID: %v", err)
	}
	if got.OwnerUserID != "usr_owner" {
		t.Fatalf("group owner after an unrelated rename = %q, want usr_owner unchanged (no succession should run)", got.OwnerUserID)
	}
}

func TestAdminUserTokensListsPseudoAndReal(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	// usr_admin must manage usr_target (Task 3: item ops are now scoped to
	// ManageableUserIDs) -- owns an admin group with usr_target as a member.
	createSystemGroupForTest(t, dir, "ugrp_tokens_sg", "Tokens SG")
	createAdminGroupForTest(t, dir, "ugrp_tokens_ag", "Tokens AG", "ugrp_tokens_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_tokens_ag", "usr_target")
	now := time.Now().UTC()
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{
		ID: "tok_t", UserID: "usr_target", Name: "Target Token",
		Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now,
	}, "opaigw_target_secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	cookie := loginAs(t, srv, "admin@example.test", "password-1")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/usr_target/tokens", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin user tokens = %d, body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []struct {
			ID            string `json:"id"`
			IsChatSession bool   `json:"is_chat_session"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 || body.Data[0].ID != "chat-session" || !body.Data[0].IsChatSession || body.Data[1].ID != "tok_t" {
		t.Fatalf("tokens = %#v", body.Data)
	}
	if strings.Contains(rec.Body.String(), "opaigw_target_secret") || strings.Contains(rec.Body.String(), "SecretHash") {
		t.Fatalf("must not leak secret material: %s", rec.Body.String())
	}
}

func TestAdminResetTOTP(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	optionalMode := "optional"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &optionalMode}); err != nil {
		t.Fatalf("set totp_mode: %v", err)
	}
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	// usr_admin must manage usr_target (Task 3: item ops are now scoped to
	// ManageableUserIDs) -- owns an admin group with usr_target as a member.
	createSystemGroupForTest(t, dir, "ugrp_totpreset_sg", "TOTP Reset SG")
	createAdminGroupForTest(t, dir, "ugrp_totpreset_ag", "TOTP Reset AG", "ugrp_totpreset_sg", "usr_admin")
	addSystemGroupMember(t, dir, "ugrp_totpreset_ag", "usr_target")
	adminCookie := loginAs(t, srv, "admin@example.test", "password-1")
	targetCookie := loginAs(t, srv, "target@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(targetCookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("enroll should be 200, got %d body=%s", enrollRec.Code, enrollRec.Body.String())
	}
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(targetCookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm should be 200, got %d body=%s", confirmRec.Code, confirmRec.Body.String())
	}

	// Target session works before reset.
	preCheck := httptest.NewRequest(http.MethodGet, "/api/portal/preferences", nil)
	preCheck.AddCookie(targetCookie)
	preCheckRec := httptest.NewRecorder()
	srv.ServeHTTP(preCheckRec, preCheck)
	if preCheckRec.Code != http.StatusOK {
		t.Fatalf("target session should be valid before reset, got %d body=%s", preCheckRec.Code, preCheckRec.Body.String())
	}

	reset := httptest.NewRequest(http.MethodPost, "/api/admin/users/usr_target/totp/reset", nil)
	reset.Header.Set(csrfHeaderName, "1")
	reset.AddCookie(adminCookie)
	resetRec := httptest.NewRecorder()
	srv.ServeHTTP(resetRec, reset)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("admin totp reset should be 200, got %d body=%s", resetRec.Code, resetRec.Body.String())
	}

	stored, err := dir.UserByID(context.Background(), "usr_target")
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if stored.TOTPEnabled {
		t.Fatalf("expected TOTPEnabled=false after admin reset, got true")
	}
	if stored.TOTPSecret != "" {
		t.Fatalf("expected TOTPSecret cleared after admin reset, got %q", stored.TOTPSecret)
	}

	// Target session was revoked by the reset.
	postCheck := httptest.NewRequest(http.MethodGet, "/api/portal/preferences", nil)
	postCheck.AddCookie(targetCookie)
	postCheckRec := httptest.NewRecorder()
	srv.ServeHTTP(postCheckRec, postCheck)
	if postCheckRec.Code != http.StatusUnauthorized {
		t.Fatalf("target session should be revoked after admin reset, got %d body=%s", postCheckRec.Code, postCheckRec.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	list.AddCookie(adminCookie)
	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), `"totp_enabled"`) {
		t.Fatalf("admin user list should include totp_enabled, got %d body=%s", listRec.Code, listRec.Body.String())
	}
}

func TestAdminResetTOTPRequiresAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_user", "user@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_target", "target@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "user@example.test", "password-1")
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/usr_target/totp/reset", nil)
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin should be 403, got %d", rec.Code)
	}
}

// TestAdminUserItemOpsScopedToManageableUserIDs proves the Task 3 gate in
// handleAdminUserItem: a non-`system` caller's PATCH (update)/tokens-list/
// TOTP-reset sub-routes all 404-no-leak on a target OUTSIDE their
// ManageableUserIDs, and succeed on one inside it -- checked ONCE before
// dispatching to any sub-route, so all three read identically. adminA owns
// an admin group AG containing usr_in as a member; usr_out shares no group
// relationship with adminA at all.
func TestAdminUserItemOpsScopedToManageableUserIDs(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_in", "in@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_out", "out@example.test", "password-1", "user")
	createSystemGroupForTest(t, dir, "ugrp_scope_sg", "Scope SG")
	createAdminGroupForTest(t, dir, "ugrp_scope_ag", "Scope AG", "ugrp_scope_sg", "usr_admina")
	addSystemGroupMember(t, dir, "ugrp_scope_ag", "usr_in")
	cookie := loginAs(t, srv, "admina@example.test", "password-1")

	patch := func(userID, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/users/"+userID, strings.NewReader(body))
		req.Header.Set(csrfHeaderName, "1")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	post := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set(csrfHeaderName, "1")
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	// usr_out is outside adminA's manageable set -> every sub-route 404s
	// no-leak (the same code as a genuinely nonexistent user).
	if rec := patch("usr_out", `{"display_name":"Renamed"}`); rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "admin.user_not_found") {
		t.Fatalf("PATCH on an unmanageable target should 404 no-leak, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/admin/users/usr_out/tokens"); rec.Code != http.StatusNotFound {
		t.Fatalf("GET tokens on an unmanageable target should 404, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/admin/users/usr_out/totp/reset"); rec.Code != http.StatusNotFound {
		t.Fatalf("POST totp/reset on an unmanageable target should 404, got %d body=%s", rec.Code, rec.Body.String())
	}

	// usr_in IS inside adminA's manageable set (a member of AG, which adminA
	// owns) -> the same three sub-routes succeed.
	if rec := patch("usr_in", `{"display_name":"Renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("PATCH on a manageable target should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := get("/api/admin/users/usr_in/tokens"); rec.Code != http.StatusOK {
		t.Fatalf("GET tokens on a manageable target should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	if rec := post("/api/admin/users/usr_in/totp/reset"); rec.Code != http.StatusOK {
		t.Fatalf("POST totp/reset on a manageable target should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
}

// TestAdminUsersListScopedForPlainAdmin proves the GET /api/admin/users list
// is scoped by ManageableUserIDs (Task 3: per-Admin-Group co-manager
// permissions, spec 2026-08-10) for a plain admin -- narrower than the prior
// system-group-visibility model -- while a system_admin still sees everyone.
// adminA owns an admin group AG whose members are {adminA, u1} (not u2), so
// adminA's own-scope list must contain adminA + u1 but never u2; an elevated
// system_admin's list must contain all three.
func TestAdminUsersListScopedForPlainAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_u1", "u1@example.test", "password-1", "user")
	seedLoginUser(t, dir, "usr_u2", "u2@example.test", "password-1", "user")

	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_sg", "usr_admina")
	addSystemGroupMember(t, dir, "ugrp_ag", "usr_u1")

	listAs := func(cookie *http.Cookie) []string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list users = %d body=%s", rec.Code, rec.Body.String())
		}
		var body struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		ids := make([]string, 0, len(body.Data))
		for _, u := range body.Data {
			ids = append(ids, u.ID)
		}
		return ids
	}
	contains := func(ids []string, want string) bool {
		for _, id := range ids {
			if id == want {
				return true
			}
		}
		return false
	}

	adminCookie := loginAs(t, srv, "admina@example.test", "password-1")
	adminIDs := listAs(adminCookie)
	if !contains(adminIDs, "usr_admina") || !contains(adminIDs, "usr_u1") {
		t.Fatalf("admin list should contain admina+u1, got %v", adminIDs)
	}
	if contains(adminIDs, "usr_u2") {
		t.Fatalf("admin list must NOT contain u2 (not a member of the admin's managed group), got %v", adminIDs)
	}

	// Seeing EVERYONE requires the `system` scope, now conditional on
	// System-Admin step-up mode.
	sysCookie := loginAs(t, srv, "sys@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")
	sysIDs := listAs(sysCookie)
	for _, want := range []string{"usr_sys", "usr_admina", "usr_u1", "usr_u2"} {
		if !contains(sysIDs, want) {
			t.Fatalf("elevated system_admin list should contain everyone, missing %s: got %v", want, sysIDs)
		}
	}
}

func TestAdminUserTokensRequireAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_user", "user@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "user@example.test", "password-1")
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users/usr_x/tokens", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin should be 403, got %d", rec.Code)
	}
}

// --- Task 5b: invite assigns the new user to a system group ---

// inviteResponse is the shape of a successful (201) POST /api/admin/users
// response body, trimmed to what these tests need.
type inviteResponse struct {
	User struct {
		ID    string `json:"id"`
		Email string `json:"email"`
	} `json:"user"`
}

func createSystemGroupForTest(t *testing.T, dir *portal.MemoryDirectory, id, name string) store.UserGroup {
	t.Helper()
	now := time.Now().UTC()
	g := store.UserGroup{ID: id, Tier: store.GroupTierSystem, Name: name, CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(context.Background(), g); err != nil {
		t.Fatalf("create system group %s: %v", id, err)
	}
	return g
}

// createAdminGroupForTest creates an admin-tier group owned by ownerID (so
// authorizeGroupManage treats ownerID as a co-manager for invites) with the
// given parent system group, and enrolls ownerID as a state=member row --
// mirroring the production invariant portal.Service.createAdminGroup always
// maintains (the owner is auto-added as a member at creation). This matters
// for ManageableUserIDs (Task 3), which enumerates a caller's admin groups
// via UserGroupsForUser(..., GroupStateMember): an owner absent from the
// membership table would be invisible to their own manageable-users set,
// even though authorizeGroupManage's direct OwnerUserID check would still
// admit them for group-management operations.
func createAdminGroupForTest(t *testing.T, dir *portal.MemoryDirectory, id, name, parentID, ownerID string) store.UserGroup {
	t.Helper()
	now := time.Now().UTC()
	g := store.UserGroup{ID: id, Tier: store.GroupTierAdmin, Name: name, ParentGroupID: parentID, OwnerUserID: ownerID, CreatedAt: now, UpdatedAt: now}
	if err := dir.CreateUserGroup(context.Background(), g); err != nil {
		t.Fatalf("create admin group %s: %v", id, err)
	}
	if err := dir.SetUserGroupMember(context.Background(), id, ownerID, store.GroupStateMember, ""); err != nil {
		t.Fatalf("enroll owner %s as member of %s: %v", ownerID, id, err)
	}
	return g
}

func addSystemGroupMember(t *testing.T, dir *portal.MemoryDirectory, groupID, userID string) {
	t.Helper()
	if err := dir.SetUserGroupMember(context.Background(), groupID, userID, store.GroupStateMember, ""); err != nil {
		t.Fatalf("add %s to group %s: %v", userID, groupID, err)
	}
}

// postInvite issues POST /api/admin/users with the given cookie and JSON
// body, returning the recorder for the caller to inspect.
func postInvite(t *testing.T, srv *Server, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(body))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// userEmailExists proves (via a system_admin, who sees everyone) whether a
// user with the given email was ever created -- used to confirm the "NO user
// created" half of the rejected-invite test cases.
func userEmailExists(t *testing.T, srv *Server, sysCookie *http.Cookie, email string) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	req.AddCookie(sysCookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list users (system_admin) = %d body=%s", rec.Code, rec.Body.String())
	}
	return strings.Contains(rec.Body.String(), `"email":"`+email+`"`)
}

func groupMemberIDs(t *testing.T, dir *portal.MemoryDirectory, groupID string) []string {
	t.Helper()
	members, err := dir.UserGroupMembers(context.Background(), groupID)
	if err != nil {
		t.Fatalf("group members %s: %v", groupID, err)
	}
	ids := make([]string, 0, len(members))
	for _, m := range members {
		if m.State == store.GroupStateMember {
			ids = append(ids, m.UserID)
		}
	}
	return ids
}

func containsStr(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestAdminInviteAssignsAdminGroup: a plain admin who owns an admin group can
// invite into it -> 201, and the new user is a member of BOTH that admin group
// and its parent system group, so the inviting admin still sees them.
func TestAdminInviteAssignsAdminGroup(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	addSystemGroupMember(t, dir, "ugrp_sg", "usr_admina")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_sg", "usr_admina")
	adminCookie := loginAs(t, srv, "admina@example.test", "password-1")

	rec := postInvite(t, srv, adminCookie, `{"email":"newbie@example.test","display_name":"Newbie","role":"user","admin_group_id":"ugrp_ag"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("invite into an owned admin group should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp inviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.User.ID == "" {
		t.Fatalf("expected a created user id, got %+v", resp)
	}
	if !containsStr(groupMemberIDs(t, dir, "ugrp_ag"), resp.User.ID) {
		t.Fatalf("new user %s should be a member of AG, members=%v", resp.User.ID, groupMemberIDs(t, dir, "ugrp_ag"))
	}
	if !containsStr(groupMemberIDs(t, dir, "ugrp_sg"), resp.User.ID) {
		t.Fatalf("new user %s should be a member of the parent SG, members=%v", resp.User.ID, groupMemberIDs(t, dir, "ugrp_sg"))
	}

	list := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	list.AddCookie(adminCookie)
	listRec := httptest.NewRecorder()
	srv.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), resp.User.ID) {
		t.Fatalf("the inviting admin should now see the new user: %d %s", listRec.Code, listRec.Body.String())
	}
}

// TestAdminInviteMissingAdminGroupRejected: no admin_group_id -> 400, no user.
func TestAdminInviteMissingAdminGroupRejected(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	adminCookie := loginAs(t, srv, "admina@example.test", "password-1")
	sysCookie := loginAs(t, srv, "sys@example.test", "password-1")

	rec := postInvite(t, srv, adminCookie, `{"email":"orphan@example.test","display_name":"Orphan","role":"user"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "user.admin_group_required") {
		t.Fatalf("missing admin group should be 400 user.admin_group_required, got %d %s", rec.Code, rec.Body.String())
	}
	if userEmailExists(t, srv, sysCookie, "orphan@example.test") {
		t.Fatalf("no user should have been created without an admin group")
	}
}

// TestAdminInviteForeignAdminGroupRejected: an admin who does not co-manage the
// requested admin group is refused -> 400, no user.
func TestAdminInviteForeignAdminGroupRejected(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_admina", "admina@example.test", "password-1", "admin")
	seedLoginUser(t, dir, "usr_adminb", "adminb@example.test", "password-1", "admin")
	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	createAdminGroupForTest(t, dir, "ugrp_mine", "Mine", "ugrp_sg", "usr_admina")
	createAdminGroupForTest(t, dir, "ugrp_other", "Other", "ugrp_sg", "usr_adminb")
	adminCookie := loginAs(t, srv, "admina@example.test", "password-1")
	sysCookie := loginAs(t, srv, "sys@example.test", "password-1")

	rec := postInvite(t, srv, adminCookie, `{"email":"foreign@example.test","display_name":"Foreign","role":"user","admin_group_id":"ugrp_other"}`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "user.admin_group_invalid") {
		t.Fatalf("a foreign admin group should be 400 user.admin_group_invalid, got %d %s", rec.Code, rec.Body.String())
	}
	if userEmailExists(t, srv, sysCookie, "foreign@example.test") {
		t.Fatalf("no user should have been created for a foreign admin group")
	}
}

// TestAdminInviteSystemAdminAnyGroup: a system_admin must also pick a group
// (mandatory) but may pick ANY admin group.
func TestAdminInviteSystemAdminAnyGroup(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sys", "sys@example.test", "password-1", "system_admin")
	seedLoginUser(t, dir, "usr_adminb", "adminb@example.test", "password-1", "admin")
	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_sg", "usr_adminb") // owned by someone else
	sysCookie := loginAs(t, srv, "sys@example.test", "password-1")
	// Inviting into ANY admin group (not just one the actor manages) requires
	// the `system` scope, now conditional on System-Admin step-up mode.
	elevateSystemAdmin(t, srv, sysCookie, "password-1")

	// mandatory even for system_admin.
	miss := postInvite(t, srv, sysCookie, `{"email":"nog@example.test","display_name":"NoGroup","role":"user"}`)
	if miss.Code != http.StatusBadRequest || !strings.Contains(miss.Body.String(), "user.admin_group_required") {
		t.Fatalf("system_admin without a group should be 400 user.admin_group_required, got %d %s", miss.Code, miss.Body.String())
	}
	// any admin group is allowed.
	rec := postInvite(t, srv, sysCookie, `{"email":"placed@example.test","display_name":"Placed","role":"user","admin_group_id":"ugrp_ag"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("system_admin invite into any admin group should succeed, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp inviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !containsStr(groupMemberIDs(t, dir, "ugrp_ag"), resp.User.ID) {
		t.Fatalf("new user should be a member of AG, members=%v", groupMemberIDs(t, dir, "ugrp_ag"))
	}
}

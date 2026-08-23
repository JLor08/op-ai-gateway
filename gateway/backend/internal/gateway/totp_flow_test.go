// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/totp"
	"strings"
	"testing"
	"time"
)

// TestTOTPRequiredModeFullFlow proves the full required-mode wiring end to
// end: a system_admin flips totp_mode=required, invites a user, the invitee
// sets a password (getting an enrollment payload instead of a session),
// confirms with a valid code (getting a session), /api/portal/me reflects
// totp_enabled/totp_mode, a second password-only login challenges again, and
// an admin reset forces re-enrollment on the next login.
func TestTOTPRequiredModeFullFlow(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	sysCookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")

	setMode := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"required"}`))
	setMode.Header.Set(csrfHeaderName, "1")
	setMode.AddCookie(sysCookie)
	setModeRec := httptest.NewRecorder()
	srv.ServeHTTP(setModeRec, setMode)
	if setModeRec.Code != http.StatusOK {
		t.Fatalf("PUT totp_mode=required = %d, want 200 (body=%s)", setModeRec.Code, setModeRec.Body.String())
	}

	// Admin invites a new user (mandatory admin_group_id).
	createSystemGroupForTest(t, dir, "ugrp_sg", "SG")
	createAdminGroupForTest(t, dir, "ugrp_ag", "AG", "ugrp_sg", "usr_sysadmin")
	create := httptest.NewRequest(http.MethodPost, "/api/admin/users", strings.NewReader(`{"email":"invitee@example.test","display_name":"Invitee","role":"user","admin_group_id":"ugrp_ag"}`))
	create.Header.Set(csrfHeaderName, "1")
	create.AddCookie(sysCookie)
	createRec := httptest.NewRecorder()
	srv.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create user = %d, want 201 (body=%s)", createRec.Code, createRec.Body.String())
	}
	var createBody struct {
		User struct {
			ID string `json:"id"`
		} `json:"user"`
		InviteURL string `json:"invite_url"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &createBody); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}
	userID := createBody.User.ID
	if userID == "" {
		t.Fatalf("expected non-empty user id, body=%s", createRec.Body.String())
	}
	tokenIdx := strings.Index(createBody.InviteURL, "token=")
	if tokenIdx == -1 {
		t.Fatalf("invite_url missing token param: %s", createBody.InviteURL)
	}
	inviteToken := createBody.InviteURL[tokenIdx+len("token="):]

	// Set-password: under required mode, this must return an enrollment
	// payload rather than a session cookie.
	setPassword := httptest.NewRequest(http.MethodPost, "/api/auth/set-password", strings.NewReader(`{"token":"`+inviteToken+`","password":"password-123"}`))
	setPassword.Header.Set(csrfHeaderName, "1")
	setPasswordRec := httptest.NewRecorder()
	srv.ServeHTTP(setPasswordRec, setPassword)
	if setPasswordRec.Code != http.StatusOK {
		t.Fatalf("set-password = %d, want 200 (body=%s)", setPasswordRec.Code, setPasswordRec.Body.String())
	}
	var enrollBody struct {
		TOTPEnrollmentRequired bool   `json:"totp_enrollment_required"`
		SecretBase32           string `json:"secret_base32"`
		Email                  string `json:"email"`
	}
	if err := json.Unmarshal(setPasswordRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal set-password response: %v", err)
	}
	if !enrollBody.TOTPEnrollmentRequired {
		t.Fatalf("expected totp_enrollment_required=true, body=%s", setPasswordRec.Body.String())
	}
	if enrollBody.SecretBase32 == "" {
		t.Fatalf("expected non-empty secret_base32, body=%s", setPasswordRec.Body.String())
	}
	for _, c := range setPasswordRec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Fatalf("required-mode set-password must not set a session cookie, got %+v", c)
		}
	}

	// totp.Code -> login confirms + gets a cookie.
	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"invitee@example.test","password":"password-123","totp_code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm login = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}
	inviteeCookie := sessionCookie(t, confirmRec.Result())
	if !strings.Contains(confirmRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("expected totp_enabled:true in login response, got %s", confirmRec.Body.String())
	}

	// /api/portal/me shows totp_enabled:true, totp_mode:required.
	me := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
	me.AddCookie(inviteeCookie)
	meRec := httptest.NewRecorder()
	srv.ServeHTTP(meRec, me)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me = %d, want 200 (body=%s)", meRec.Code, meRec.Body.String())
	}
	if !strings.Contains(meRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("me body missing totp_enabled:true, got %s", meRec.Body.String())
	}
	if !strings.Contains(meRec.Body.String(), `"totp_mode":"required"`) {
		t.Fatalf("me body missing totp_mode:required, got %s", meRec.Body.String())
	}

	// Second login password-only -> {"totp_required":true}, no session.
	again := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"invitee@example.test","password":"password-123"}`))
	again.Header.Set(csrfHeaderName, "1")
	againRec := httptest.NewRecorder()
	srv.ServeHTTP(againRec, again)
	if againRec.Code != http.StatusOK {
		t.Fatalf("password-only login = %d, want 200 (body=%s)", againRec.Code, againRec.Body.String())
	}
	if len(againRec.Result().Cookies()) != 0 {
		t.Fatalf("password-only login on enabled user must not set a cookie, got %v", againRec.Result().Cookies())
	}
	if !strings.Contains(againRec.Body.String(), `"totp_required":true`) {
		t.Fatalf("expected totp_required:true, got %s", againRec.Body.String())
	}

	// Admin resets TOTP for the invitee.
	reset := httptest.NewRequest(http.MethodPost, "/api/admin/users/"+userID+"/totp/reset", nil)
	reset.Header.Set(csrfHeaderName, "1")
	reset.AddCookie(sysCookie)
	resetRec := httptest.NewRecorder()
	srv.ServeHTTP(resetRec, reset)
	if resetRec.Code != http.StatusOK {
		t.Fatalf("admin totp reset = %d, want 200 (body=%s)", resetRec.Code, resetRec.Body.String())
	}

	// User must re-enroll on next login (password-only -> enrollment payload
	// again, since totp_mode is still required).
	reenroll := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"invitee@example.test","password":"password-123"}`))
	reenroll.Header.Set(csrfHeaderName, "1")
	reenrollRec := httptest.NewRecorder()
	srv.ServeHTTP(reenrollRec, reenroll)
	if reenrollRec.Code != http.StatusOK {
		t.Fatalf("post-reset password-only login = %d, want 200 (body=%s)", reenrollRec.Code, reenrollRec.Body.String())
	}
	if len(reenrollRec.Result().Cookies()) != 0 {
		t.Fatalf("post-reset enrollment login must not set a cookie, got %v", reenrollRec.Result().Cookies())
	}
	var reenrollBody struct {
		TOTPEnrollmentRequired bool   `json:"totp_enrollment_required"`
		SecretBase32           string `json:"secret_base32"`
	}
	if err := json.Unmarshal(reenrollRec.Body.Bytes(), &reenrollBody); err != nil {
		t.Fatalf("unmarshal post-reset response: %v", err)
	}
	if !reenrollBody.TOTPEnrollmentRequired {
		t.Fatalf("expected totp_enrollment_required=true after reset, body=%s", reenrollRec.Body.String())
	}
	if reenrollBody.SecretBase32 == "" {
		t.Fatalf("expected non-empty secret_base32 after reset, body=%s", reenrollRec.Body.String())
	}
}

// TestTOTPOptionalModeSelfServiceFlow proves the optional-mode self-service
// wiring end to end: enroll, confirm, then disable via the portal TOTP
// endpoints, after which login works with password only again.
func TestTOTPOptionalModeSelfServiceFlow(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	sysCookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")

	setMode := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"optional"}`))
	setMode.Header.Set(csrfHeaderName, "1")
	setMode.AddCookie(sysCookie)
	setModeRec := httptest.NewRecorder()
	srv.ServeHTTP(setModeRec, setMode)
	if setModeRec.Code != http.StatusOK {
		t.Fatalf("PUT totp_mode=optional = %d, want 200 (body=%s)", setModeRec.Code, setModeRec.Body.String())
	}

	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "a@example.test", "password-1")

	enroll := httptest.NewRequest(http.MethodPost, "/api/portal/totp/enroll", nil)
	enroll.Header.Set(csrfHeaderName, "1")
	enroll.AddCookie(cookie)
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enroll)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("enroll = %d, want 200 (body=%s)", enrollRec.Code, enrollRec.Body.String())
	}
	var enrollBody struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrollBody); err != nil {
		t.Fatalf("unmarshal enroll response: %v", err)
	}
	if enrollBody.SecretBase32 == "" {
		t.Fatalf("expected non-empty secret_base32, body=%s", enrollRec.Body.String())
	}

	code, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`{"code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirm.AddCookie(cookie)
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}
	if !strings.Contains(confirmRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("expected totp_enabled:true after confirm, got %s", confirmRec.Body.String())
	}

	code2, err := totp.Code(enrollBody.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate second code: %v", err)
	}
	disableReq, err := http.NewRequest(http.MethodDelete, "/api/portal/totp", strings.NewReader(`{"code":"`+code2+`"}`))
	if err != nil {
		t.Fatalf("new delete request: %v", err)
	}
	disableReq.Header.Set(csrfHeaderName, "1")
	disableReq.AddCookie(cookie)
	disableRec := httptest.NewRecorder()
	srv.ServeHTTP(disableRec, disableReq)
	if disableRec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200 (body=%s)", disableRec.Code, disableRec.Body.String())
	}

	// Login now works password-only again.
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("post-disable login = %d, want 200 (body=%s)", loginRec.Code, loginRec.Body.String())
	}
	_ = sessionCookie(t, loginRec.Result())
	if !strings.Contains(loginRec.Body.String(), `"totp_enabled":false`) {
		t.Fatalf("expected totp_enabled:false after disable, got %s", loginRec.Body.String())
	}
}

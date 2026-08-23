// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/totp"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

func newAuthTestServer(t *testing.T) (*Server, *portal.MemoryDirectory) {
	t.Helper()
	ts := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(ts)
	acct := account.NewService(account.Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir, SettingsVolatile: true}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, time.Now().UTC())
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore, Groups: dir, Projects: dir, SystemSettings: portal.NewMemorySystemSettings(), UIPrefs: portal.NewMemoryUIPreferences()})
	srv := New(ServerDeps{
		Tokens: ts, Usage: recorder, Portal: svc, Account: acct, Routes: routeStore,
		CookieSecure: false, SessionMaxAge: 24 * time.Hour, PublicURL: "http://localhost:8080",
	})
	return srv, dir
}

func seedLoginUser(t *testing.T, dir *portal.MemoryDirectory, id, email, password, role string) {
	t.Helper()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	now := time.Now().UTC()
	if err := dir.CreateUser(context.Background(), store.User{ID: id, Email: email, DisplayName: id, Role: role, Status: store.UserStatusActive, PreferredLanguage: "de", PasswordHash: hash, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

// getStatus issues a GET to path with cookie attached and returns the HTTP
// status code.
func getStatus(t *testing.T, srv *Server, cookie *http.Cookie, path string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec.Code
}

// doJSON issues a request with the given method/path/body, attaching cookie
// and the CSRF header (required by authenticateWeb for non-safe methods), and
// returns the recorder.
func doJSON(t *testing.T, srv *Server, cookie *http.Cookie, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set(csrfHeaderName, "1")
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// elevateSystemAdmin enters System-Admin mode for the session identified by
// cookie, using password as the step-up credential. It fails the test if
// elevation does not succeed.
func elevateSystemAdmin(t *testing.T, srv *Server, cookie *http.Cookie, password string) {
	t.Helper()
	rec := doJSON(t, srv, cookie, http.MethodPost, "/api/portal/system-admin-mode", `{"password":"`+password+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("elevate to system-admin mode = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestSessionPrincipalInheritsChatFlags(t *testing.T) {
	on := sessionPrincipal(store.User{ID: "usr_1", DisplayName: "U", Role: "user", ChatLogCommunication: true, ChatSecret: true}, false)
	if on.ID != "" {
		t.Fatalf("session principal must carry an empty token ID, got %q", on.ID)
	}
	if on.UserID != "usr_1" {
		t.Fatalf("UserID = %q, want usr_1", on.UserID)
	}
	if !on.LogCommunication || !on.Secret {
		t.Fatalf("session principal must inherit profile chat flags, got log=%v secret=%v", on.LogCommunication, on.Secret)
	}

	off := sessionPrincipal(store.User{ID: "usr_2", Role: "user"}, false)
	if off.LogCommunication || off.Secret {
		t.Fatalf("unset profile chat flags must stay false, got log=%v secret=%v", off.LogCommunication, off.Secret)
	}
}

// TestSessionPrincipalElevationGatesSystemScope proves the gating rule: a
// system_admin only carries the `system` scope when elevated; a plain admin
// never carries it regardless of the elevated flag.
func TestSessionPrincipalElevationGatesSystemScope(t *testing.T) {
	sa := store.User{ID: "u", Role: "system_admin", Status: store.UserStatusActive}
	if p := sessionPrincipal(sa, false); p.HasScope("system") {
		t.Fatalf("non-elevated system_admin must NOT have system scope")
	}
	if p := sessionPrincipal(sa, false); !p.HasScope("admin") {
		t.Fatalf("non-elevated system_admin must still have admin scope")
	}
	if p := sessionPrincipal(sa, true); !p.HasScope("system") {
		t.Fatalf("elevated system_admin must have system scope")
	}
	adm := store.User{ID: "a", Role: "admin", Status: store.UserStatusActive}
	if p := sessionPrincipal(adm, true); p.HasScope("system") {
		t.Fatalf("an admin must never get system scope, even 'elevated'")
	}
}

// TestPortalMeReflectsSystemAdminElevation guards the bug where GET
// /api/portal/me (portal.Service.CurrentUser) always returned zero-value
// system_admin_mode fields -- because that service method has no access to
// the session, only handleAuthSession/handleSystemAdminMode did. Since
// App.tsx's loadPortalData calls api.me() -> setCurrentUser on EVERY
// navigation (not just login), this silently reverted a genuinely-elevated
// session's DISPLAYED elevation to false the moment the user navigated
// anywhere, hiding the System panel / owner picker even though the backend
// session was still elevated. handlePortalMe must overlay the real session
// elevation state, exactly like handleAuthSession does.
func TestPortalMeReflectsSystemAdminElevation(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_admin", "admin@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "admin@example.test", "password-1")

	// Before elevation: /me must report system_admin_mode=false.
	rec := doJSON(t, srv, cookie, http.MethodGet, "/api/portal/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var before struct {
		SystemAdminMode          bool   `json:"system_admin_mode"`
		SystemAdminModeExpiresAt string `json:"system_admin_mode_expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &before); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if before.SystemAdminMode {
		t.Fatalf("pre-elevation /me reported system_admin_mode=true")
	}
	if before.SystemAdminModeExpiresAt != "" {
		t.Fatalf("pre-elevation /me reported a non-empty expiry: %q", before.SystemAdminModeExpiresAt)
	}

	elevateSystemAdmin(t, srv, cookie, "password-1")

	// After elevation, a FRESH GET /me (mirroring loadPortalData's refetch on
	// navigation) must still report the elevated state -- the regression this
	// guards is exactly that a refetch reverted it.
	rec = doJSON(t, srv, cookie, http.MethodGet, "/api/portal/me", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /me (post-elevation) = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var after struct {
		SystemAdminMode          bool   `json:"system_admin_mode"`
		SystemAdminModeExpiresAt string `json:"system_admin_mode_expires_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	if !after.SystemAdminMode {
		t.Fatalf("post-elevation /me still reported system_admin_mode=false")
	}
	if after.SystemAdminModeExpiresAt == "" {
		t.Fatalf("post-elevation /me reported an empty system_admin_mode_expires_at")
	}
	if _, err := time.Parse(time.RFC3339, after.SystemAdminModeExpiresAt); err != nil {
		t.Fatalf("system_admin_mode_expires_at %q is not RFC3339: %v", after.SystemAdminModeExpiresAt, err)
	}
}

// TestSystemAdminModeEndToEnd exercises the full HTTP flow: a system_admin's
// session cannot reach a system route until it enters the mode, and loses it
// again on exit.
func TestSystemAdminModeEndToEnd(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sa", "sa@example.test", "password-123", "system_admin")
	cookie := loginAs(t, srv, "sa@example.test", "password-123")

	// A system-scoped GET is refused before elevation.
	if code := getStatus(t, srv, cookie, "/api/system/settings"); code != http.StatusForbidden {
		t.Fatalf("pre-elevation system route = %d, want 403", code)
	}
	// Enter mode with the password.
	rec := doJSON(t, srv, cookie, http.MethodPost, "/api/portal/system-admin-mode", `{"password":"password-123"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("enter = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"system_admin_mode":true`) {
		t.Fatalf("enter response should report elevated: %s", rec.Body.String())
	}
	if code := getStatus(t, srv, cookie, "/api/system/settings"); code != http.StatusOK {
		t.Fatalf("post-elevation system route = %d, want 200", code)
	}
	// Leave.
	if rec := doJSON(t, srv, cookie, http.MethodDelete, "/api/portal/system-admin-mode", ``); rec.Code != http.StatusOK {
		t.Fatalf("leave = %d", rec.Code)
	}
	if code := getStatus(t, srv, cookie, "/api/system/settings"); code != http.StatusForbidden {
		t.Fatalf("post-leave system route = %d, want 403", code)
	}
}

// TestCompatRejectsInvalidSessionCookie confirms that the OpenAI chat endpoint
// now accepts portal session cookies (via requireWebScope) but still rejects an
// invalid one: an unknown cookie value fails session resolution and yields 401
// auth.session_invalid rather than being treated as an anonymous request.
func TestCompatRejectsInvalidSessionCookie(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "anything"})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("compat with an invalid cookie should be 401, got %d", rec.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if body.Error.Code != "auth.session_invalid" {
		t.Fatalf("error code = %q, want auth.session_invalid", body.Error.Code)
	}
}

func sessionCookie(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return c
		}
	}
	t.Fatal("no session cookie set")
	return nil
}

func TestLoginLogoutRoundTrip(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "admin")

	// Missing CSRF header is rejected.
	noCSRF := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	noCSRFRec := httptest.NewRecorder()
	srv.ServeHTTP(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("login without csrf should be 403, got %d", noCSRFRec.Code)
	}

	// Wrong password.
	bad := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"nope"}`))
	bad.Header.Set(csrfHeaderName, "1")
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("bad login should be 401, got %d", badRec.Code)
	}

	// Successful login sets a cookie.
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login should be 200, got %d body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	// /api/portal/me resolves via the cookie.
	me := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
	me.AddCookie(cookie)
	meRec := httptest.NewRecorder()
	srv.ServeHTTP(meRec, me)
	if meRec.Code != http.StatusOK || !strings.Contains(meRec.Body.String(), "a@example.test") {
		t.Fatalf("me via cookie failed: %d %s", meRec.Code, meRec.Body.String())
	}

	// Logout clears the session.
	logout := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	logout.Header.Set(csrfHeaderName, "1")
	logout.AddCookie(cookie)
	logoutRec := httptest.NewRecorder()
	srv.ServeHTTP(logoutRec, logout)
	if logoutRec.Code != http.StatusOK {
		t.Fatalf("logout should be 200, got %d", logoutRec.Code)
	}
	me2 := httptest.NewRequest(http.MethodGet, "/api/portal/me", nil)
	me2.AddCookie(cookie)
	me2Rec := httptest.NewRecorder()
	srv.ServeHTTP(me2Rec, me2)
	if me2Rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout should be 401, got %d", me2Rec.Code)
	}
}

func TestSetPasswordEndpoint(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	// Invite a user directly through the account service used by the server.
	acct := account.NewService(account.Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir}, account.Config{IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de"})
	_, secret, err := acct.InviteUser(context.Background(), account.InviteInput{Email: "invitee@example.test", DisplayName: "Invitee", Role: "user"}, false)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	weak := httptest.NewRequest(http.MethodPost, "/api/auth/set-password", strings.NewReader(`{"token":"`+secret+`","password":"short"}`))
	weak.Header.Set(csrfHeaderName, "1")
	weakRec := httptest.NewRecorder()
	srv.ServeHTTP(weakRec, weak)
	if weakRec.Code != http.StatusBadRequest {
		t.Fatalf("weak password should be 400, got %d", weakRec.Code)
	}

	ok := httptest.NewRequest(http.MethodPost, "/api/auth/set-password", strings.NewReader(`{"token":"`+secret+`","password":"password-123"}`))
	ok.Header.Set(csrfHeaderName, "1")
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("set-password should be 200, got %d body=%s", okRec.Code, okRec.Body.String())
	}
	_ = sessionCookie(t, okRec.Result()) // auto-login cookie present
}

func TestSetPasswordRequiredModeReturnsEnrollment(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	requiredMode := "required"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{TOTPMode: &requiredMode}); err != nil {
		t.Fatalf("set totp_mode: %v", err)
	}
	acct := account.NewService(account.Deps{Users: dir, Sessions: dir, SetPasswordTokens: dir}, account.Config{IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de"})
	_, secret, err := acct.InviteUser(context.Background(), account.InviteInput{Email: "invitee@example.test", DisplayName: "Invitee", Role: "user"}, false)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}

	ok := httptest.NewRequest(http.MethodPost, "/api/auth/set-password", strings.NewReader(`{"token":"`+secret+`","password":"password-123"}`))
	ok.Header.Set(csrfHeaderName, "1")
	okRec := httptest.NewRecorder()
	srv.ServeHTTP(okRec, ok)
	if okRec.Code != http.StatusOK {
		t.Fatalf("set-password should be 200, got %d body=%s", okRec.Code, okRec.Body.String())
	}
	var body struct {
		TOTPEnrollmentRequired bool   `json:"totp_enrollment_required"`
		Email                  string `json:"email"`
		SecretBase32           string `json:"secret_base32"`
	}
	if err := json.Unmarshal(okRec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if !body.TOTPEnrollmentRequired {
		t.Fatalf("expected totp_enrollment_required=true, body=%s", okRec.Body.String())
	}
	if body.Email != "invitee@example.test" {
		t.Fatalf("expected email invitee@example.test, got %q", body.Email)
	}
	if body.SecretBase32 == "" {
		t.Fatalf("expected non-empty secret_base32, body=%s", okRec.Body.String())
	}
	for _, c := range okRec.Result().Cookies() {
		if c.Name == sessionCookieName {
			t.Fatalf("required-mode set-password must not set a session cookie, got %+v", c)
		}
	}

	code, err := totp.Code(body.SecretBase32, time.Now().UTC())
	if err != nil {
		t.Fatalf("generate code: %v", err)
	}
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"invitee@example.test","password":"password-123","totp_code":"`+code+`"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login should be 200, got %d body=%s", loginRec.Code, loginRec.Body.String())
	}
	_ = sessionCookie(t, loginRec.Result())
	if !strings.Contains(loginRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("expected totp_enabled:true in login response, got %s", loginRec.Body.String())
	}
}

func TestChangePasswordEndpoint(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-old", "user")
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-old"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	cookie := sessionCookie(t, loginRec.Result())

	// Missing CSRF is rejected even with a valid cookie.
	noCSRF := httptest.NewRequest(http.MethodPost, "/api/portal/password", strings.NewReader(`{"current_password":"password-old","new_password":"password-new"}`))
	noCSRF.AddCookie(cookie)
	noCSRFRec := httptest.NewRecorder()
	srv.ServeHTTP(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden {
		t.Fatalf("change-password without csrf should be 403, got %d", noCSRFRec.Code)
	}

	change := httptest.NewRequest(http.MethodPost, "/api/portal/password", strings.NewReader(`{"current_password":"password-old","new_password":"password-new"}`))
	change.Header.Set(csrfHeaderName, "1")
	change.AddCookie(cookie)
	changeRec := httptest.NewRecorder()
	srv.ServeHTTP(changeRec, change)
	if changeRec.Code != http.StatusOK {
		t.Fatalf("change-password should be 200, got %d body=%s", changeRec.Code, changeRec.Body.String())
	}
}

func TestPortalTokenItemRequiresCSRFForCookieSession(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "u1@example.test", "dev-secret", "user")

	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"u1@example.test","password":"dev-secret"}`))
	loginReq.Header.Set("X-OP-CSRF", "1")
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	req := httptest.NewRequest(http.MethodPatch, "/api/portal/tokens/tok_x", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch without CSRF = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestPortalServerItemRequiresCSRFForCookieSession(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "u1@example.test", "dev-secret", "user")

	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"u1@example.test","password":"dev-secret"}`))
	loginReq.Header.Set("X-OP-CSRF", "1")
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	req := httptest.NewRequest(http.MethodPatch, "/api/portal/servers/srv_x", strings.NewReader(`{"name":"x"}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch without CSRF = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestPortalApplicationItemRequiresCSRFForCookieSession(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "u1@example.test", "dev-secret", "user")

	loginRec := httptest.NewRecorder()
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"u1@example.test","password":"dev-secret"}`))
	loginReq.Header.Set("X-OP-CSRF", "1")
	srv.ServeHTTP(loginRec, loginReq)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	req := httptest.NewRequest(http.MethodPatch, "/api/portal/applications/app_x", strings.NewReader(`{"port":8000}`))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("patch without CSRF = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
}

func TestAuthSessionEndpoint(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sess", "sess@example.test", "password-1", "admin")

	// No cookie -> 200 { authenticated: false }.
	anon := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	anonRec := httptest.NewRecorder()
	srv.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusOK {
		t.Fatalf("anon session should be 200, got %d", anonRec.Code)
	}
	var anonBody struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(anonRec.Body.Bytes(), &anonBody); err != nil {
		t.Fatalf("anon body: %v", err)
	}
	if anonBody.Authenticated {
		t.Fatalf("anon should be authenticated=false")
	}

	// Non-GET -> 405.
	post := httptest.NewRequest(http.MethodPost, "/api/auth/session", nil)
	postRec := httptest.NewRecorder()
	srv.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST session should be 405, got %d", postRec.Code)
	}

	// Login -> cookie.
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"sess@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login should be 200, got %d body=%s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	// Valid cookie -> 200 { authenticated: true, user: {...} }.
	authed := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	authed.AddCookie(cookie)
	authedRec := httptest.NewRecorder()
	srv.ServeHTTP(authedRec, authed)
	if authedRec.Code != http.StatusOK {
		t.Fatalf("authed session should be 200, got %d", authedRec.Code)
	}
	var authedBody struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Email string `json:"email"`
		} `json:"user"`
	}
	if err := json.Unmarshal(authedRec.Body.Bytes(), &authedBody); err != nil {
		t.Fatalf("authed body: %v", err)
	}
	if !authedBody.Authenticated || authedBody.User.Email != "sess@example.test" {
		t.Fatalf("authed body = %+v, want authenticated with sess@example.test", authedBody)
	}

	// Invalid cookie -> 200 { authenticated: false } and the stale cookie is cleared.
	bad := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	bad.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "bogus"})
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusOK {
		t.Fatalf("invalid-cookie session should be 200, got %d", badRec.Code)
	}
	var badBody struct {
		Authenticated bool `json:"authenticated"`
	}
	if err := json.Unmarshal(badRec.Body.Bytes(), &badBody); err != nil {
		t.Fatalf("bad body: %v", err)
	}
	if badBody.Authenticated {
		t.Fatalf("invalid cookie should be authenticated=false")
	}
	cleared := false
	for _, c := range badRec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("invalid cookie should be cleared (Max-Age<0)")
	}
}

func TestAuthSessionIncludesDefaultLanguage(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	var body struct {
		DefaultLanguage string `json:"default_language"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body: %v", err)
	}
	if body.DefaultLanguage != "de" {
		t.Fatalf("default_language = %q, want %q", body.DefaultLanguage, "de")
	}
}

func TestPortalChatSettingsUpdate(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_chat", "chatset@example.test", "password-1", "user")
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"chatset@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	// Valid update -> 200 and both profile flags persist on the user.
	put := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(`{"log_communication":true,"secret":true}`))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("chat-settings update failed: %d %s", putRec.Code, putRec.Body.String())
	}
	user, err := dir.UserByID(context.Background(), "usr_chat")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if !user.ChatLogCommunication || !user.ChatSecret {
		t.Fatalf("chat flags not persisted: %+v", user)
	}

	// Toggling back to false persists false too.
	put2 := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(`{"log_communication":false,"secret":false}`))
	put2.Header.Set(csrfHeaderName, "1")
	put2.AddCookie(cookie)
	put2Rec := httptest.NewRecorder()
	srv.ServeHTTP(put2Rec, put2)
	if put2Rec.Code != http.StatusOK {
		t.Fatalf("chat-settings reset failed: %d %s", put2Rec.Code, put2Rec.Body.String())
	}
	user2, _ := dir.UserByID(context.Background(), "usr_chat")
	if user2.ChatLogCommunication || user2.ChatSecret {
		t.Fatalf("chat flags not reset: %+v", user2)
	}

	// Non-PUT -> 405.
	post := httptest.NewRequest(http.MethodPost, "/api/portal/chat-settings", strings.NewReader(`{}`))
	post.Header.Set(csrfHeaderName, "1")
	post.AddCookie(cookie)
	postRec := httptest.NewRecorder()
	srv.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("non-PUT chat-settings should be 405, got %d", postRec.Code)
	}

	// No session -> 401.
	anon := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(`{"log_communication":true,"secret":true}`))
	anon.Header.Set(csrfHeaderName, "1")
	anonRec := httptest.NewRecorder()
	srv.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session chat-settings should be 401, got %d", anonRec.Code)
	}
}

// TestPortalLanguageUpdate proves a NON-elevated system_admin can still edit
// their OWN preferred language: handlePortalLanguage calls
// account.UpdateOwnProfile (not UpdateUser), which carries no role/status
// fields and so never trips UpdateUser's system-admin-account protection
// guard -- a self-service non-role edit is not a "system power" and must not
// require System-Admin step-up mode. Role is deliberately "system_admin"
// (not "user") specifically to exercise this: before the fix, this 200'd for
// a "user" but 500'd (account.ErrForbiddenRole) for a non-elevated
// system_admin, because their OWN role is system_admin and the actor lacked
// the `system` scope pre-elevation.
func TestPortalLanguageUpdate(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_lang", "lang@example.test", "password-1", "system_admin")
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"lang@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", loginRec.Code, loginRec.Body.String())
	}
	cookie := sessionCookie(t, loginRec.Result())

	// Valid update -> 200 and preferred_language reflected.
	put := httptest.NewRequest(http.MethodPut, "/api/portal/language", strings.NewReader(`{"language":"en"}`))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusOK || !strings.Contains(putRec.Body.String(), `"preferred_language":"en"`) {
		t.Fatalf("language update failed: %d %s", putRec.Code, putRec.Body.String())
	}

	// Invalid language -> 400.
	bad := httptest.NewRequest(http.MethodPut, "/api/portal/language", strings.NewReader(`{"language":"fr"}`))
	bad.Header.Set(csrfHeaderName, "1")
	bad.AddCookie(cookie)
	badRec := httptest.NewRecorder()
	srv.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("invalid language should be 400, got %d", badRec.Code)
	}

	// No session -> 401.
	anon := httptest.NewRequest(http.MethodPut, "/api/portal/language", strings.NewReader(`{"language":"en"}`))
	anon.Header.Set(csrfHeaderName, "1")
	anonRec := httptest.NewRecorder()
	srv.ServeHTTP(anonRec, anon)
	if anonRec.Code != http.StatusUnauthorized {
		t.Fatalf("no-session language update should be 401, got %d", anonRec.Code)
	}
}

// TestPortalChatSettingsUpdateNonElevatedSystemAdmin mirrors
// TestPortalLanguageUpdate for the sibling self-service endpoint: a
// NON-elevated system_admin PUTting their own chat capture flags must succeed
// (200), for the same UpdateOwnProfile reason.
func TestPortalChatSettingsUpdateNonElevatedSystemAdmin(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sa_chat", "sachat@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sachat@example.test", "password-1")

	put := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(`{"log_communication":true,"secret":true}`))
	put.Header.Set(csrfHeaderName, "1")
	put.AddCookie(cookie)
	putRec := httptest.NewRecorder()
	srv.ServeHTTP(putRec, put)
	if putRec.Code != http.StatusOK {
		t.Fatalf("non-elevated system_admin chat-settings update = %d, want 200 (body=%s)", putRec.Code, putRec.Body.String())
	}
	user, err := dir.UserByID(context.Background(), "usr_sa_chat")
	if err != nil {
		t.Fatalf("load user: %v", err)
	}
	if !user.ChatLogCommunication || !user.ChatSecret {
		t.Fatalf("chat flags not persisted: %+v", user)
	}
}

// TestLoginRequiredModeEnrollThenConfirm confirms that under totp_mode=required
// a not-yet-enrolled user's password-only login returns an enrollment payload
// (no session), and completing enrollment with a valid code issues a session.
func TestLoginRequiredModeEnrollThenConfirm(t *testing.T) {
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

	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")

	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login password-only = %d, want 200 (body=%s)", loginRec.Code, loginRec.Body.String())
	}
	if len(loginRec.Result().Cookies()) != 0 {
		t.Fatalf("password-only login under required mode must not set a session cookie, got %v", loginRec.Result().Cookies())
	}
	var enroll struct {
		TotpEnrollmentRequired bool   `json:"totp_enrollment_required"`
		SecretBase32           string `json:"secret_base32"`
		QRPngDataURI           string `json:"qr_png_data_uri"`
	}
	if err := json.Unmarshal(loginRec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	if !enroll.TotpEnrollmentRequired {
		t.Fatalf("totp_enrollment_required = false, want true (body=%s)", loginRec.Body.String())
	}
	if enroll.SecretBase32 == "" {
		t.Fatalf("secret_base32 empty (body=%s)", loginRec.Body.String())
	}
	if !strings.HasPrefix(enroll.QRPngDataURI, "data:image/png;base64,") {
		t.Fatalf("qr_png_data_uri = %q, want data:image/png;base64,... prefix", enroll.QRPngDataURI)
	}

	code, err := totp.Code(enroll.SecretBase32, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("login with valid totp_code = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}
	_ = sessionCookie(t, confirmRec.Result())
	if !strings.Contains(confirmRec.Body.String(), `"totp_enabled":true`) {
		t.Fatalf("body missing totp_enabled:true, got %s", confirmRec.Body.String())
	}
}

// TestLoginEnabledUserRequiresCode confirms that once a user has TOTP enabled,
// a password-only login challenges for the code (no session), and a wrong
// code is rejected with auth.totp_invalid.
func TestLoginEnabledUserRequiresCode(t *testing.T) {
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

	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")

	enrollLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	enrollLogin.Header.Set(csrfHeaderName, "1")
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enrollLogin)
	if enrollRec.Code != http.StatusOK {
		t.Fatalf("enrollment login = %d, want 200 (body=%s)", enrollRec.Code, enrollRec.Body.String())
	}
	var enroll struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	code, err := totp.Code(enroll.SecretBase32, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm enrollment = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}

	// Password-only login on an enabled user: 200 totp_required, no cookie.
	again := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	again.Header.Set(csrfHeaderName, "1")
	againRec := httptest.NewRecorder()
	srv.ServeHTTP(againRec, again)
	if againRec.Code != http.StatusOK {
		t.Fatalf("password-only login on enabled user = %d, want 200 (body=%s)", againRec.Code, againRec.Body.String())
	}
	if len(againRec.Result().Cookies()) != 0 {
		t.Fatalf("password-only login on enabled user must not set a cookie, got %v", againRec.Result().Cookies())
	}
	if !strings.Contains(againRec.Body.String(), `"totp_required":true`) {
		t.Fatalf("body = %s, want totp_required:true", againRec.Body.String())
	}

	// Wrong code -> 401 auth.totp_invalid.
	wrong := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"000000"}`))
	wrong.Header.Set(csrfHeaderName, "1")
	wrongRec := httptest.NewRecorder()
	srv.ServeHTTP(wrongRec, wrong)
	if wrongRec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong totp_code = %d, want 401 (body=%s)", wrongRec.Code, wrongRec.Body.String())
	}
	if got := decodeErrorCode(t, wrongRec.Body.Bytes()); got != "auth.totp_invalid" {
		t.Fatalf("error code = %q, want auth.totp_invalid", got)
	}
}

// TestLoginOffModeNoTOTP confirms that with the default totp_mode=off, login
// succeeds normally and the response reports totp_enabled:false and
// totp_mode:off.
func TestLoginOffModeNoTOTP(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")

	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 (body=%s)", loginRec.Code, loginRec.Body.String())
	}
	_ = sessionCookie(t, loginRec.Result())
	body := loginRec.Body.String()
	if !strings.Contains(body, `"totp_enabled":false`) {
		t.Fatalf("body = %s, want totp_enabled:false", body)
	}
	if !strings.Contains(body, `"totp_mode":"off"`) {
		t.Fatalf("body = %s, want totp_mode:off", body)
	}
}

// TestLoginEnabledUserOffModeSkipsChallenge is the FIX for a real dead-end:
// an already-enrolled user (TOTPEnabled=true) whose org later flips
// totp_mode to "off" must not be stuck behind a code challenge that the
// admin UI no longer offers a way to satisfy or clear. With mode=off, login
// succeeds password-only and issues a session directly, without a
// totp_required challenge.
func TestLoginEnabledUserOffModeSkipsChallenge(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sysadmin", "sysadmin@example.test", "password-1", "system_admin")
	sysCookie := loginAs(t, srv, "sysadmin@example.test", "password-1")
	elevateSystemAdmin(t, srv, sysCookie, "password-1")

	requiredMode := "required"
	setMode := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"`+requiredMode+`"}`))
	setMode.Header.Set(csrfHeaderName, "1")
	setMode.AddCookie(sysCookie)
	setModeRec := httptest.NewRecorder()
	srv.ServeHTTP(setModeRec, setMode)
	if setModeRec.Code != http.StatusOK {
		t.Fatalf("PUT totp_mode=required = %d, want 200 (body=%s)", setModeRec.Code, setModeRec.Body.String())
	}

	seedLoginUser(t, dir, "usr_1", "a@example.test", "password-1", "user")

	// Enroll + confirm the user under required mode.
	enrollLogin := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	enrollLogin.Header.Set(csrfHeaderName, "1")
	enrollRec := httptest.NewRecorder()
	srv.ServeHTTP(enrollRec, enrollLogin)
	var enroll struct {
		SecretBase32 string `json:"secret_base32"`
	}
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enroll); err != nil {
		t.Fatalf("Unmarshal returned %v", err)
	}
	code, err := totp.Code(enroll.SecretBase32, time.Now())
	if err != nil {
		t.Fatalf("totp.Code: %v", err)
	}
	confirm := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1","totp_code":"`+code+`"}`))
	confirm.Header.Set(csrfHeaderName, "1")
	confirmRec := httptest.NewRecorder()
	srv.ServeHTTP(confirmRec, confirm)
	if confirmRec.Code != http.StatusOK {
		t.Fatalf("confirm enrollment = %d, want 200 (body=%s)", confirmRec.Code, confirmRec.Body.String())
	}

	// Now the org flips totp_mode to off.
	offMode := "off"
	setOff := httptest.NewRequest(http.MethodPut, "/api/system/settings", strings.NewReader(`{"totp_mode":"`+offMode+`"}`))
	setOff.Header.Set(csrfHeaderName, "1")
	setOff.AddCookie(sysCookie)
	setOffRec := httptest.NewRecorder()
	srv.ServeHTTP(setOffRec, setOff)
	if setOffRec.Code != http.StatusOK {
		t.Fatalf("PUT totp_mode=off = %d, want 200 (body=%s)", setOffRec.Code, setOffRec.Body.String())
	}

	// Password-only login now succeeds directly: no totp_required challenge,
	// a session cookie is issued, and the response still reports
	// totp_enabled:true (the enrollment itself is untouched) with
	// totp_mode:off.
	login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"email":"a@example.test","password":"password-1"}`))
	login.Header.Set(csrfHeaderName, "1")
	loginRec := httptest.NewRecorder()
	srv.ServeHTTP(loginRec, login)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("password-only login in off-mode = %d, want 200 (body=%s)", loginRec.Code, loginRec.Body.String())
	}
	_ = sessionCookie(t, loginRec.Result())
	body := loginRec.Body.String()
	if strings.Contains(body, `"totp_required":true`) {
		t.Fatalf("body = %s, must NOT challenge for a code when totp_mode=off", body)
	}
	if !strings.Contains(body, `"totp_enabled":true`) {
		t.Fatalf("body = %s, want totp_enabled:true (enrollment untouched)", body)
	}
	if !strings.Contains(body, `"totp_mode":"off"`) {
		t.Fatalf("body = %s, want totp_mode:off", body)
	}
}

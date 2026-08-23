// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
)

// This file targets the auth.go request-shape/session-lifecycle branches that
// carry NEW (post-refactor) uncovered lines: each handler's own
// json.Unmarshal-into-typed-struct failure (distinct from the earlier, already
// broadly-tested readRawJSON/writeJSONDecodeError path — a syntactically
// invalid body like `{not json` fails ONE line earlier, in server.go, not
// here), plus the session/credential/method branches in
// handleSystemAdminMode. See services_endpoints_test.go's
// TestServicesEndpointsInvalidJSONReturns400 for why `[]` (a syntactically
// VALID JSON array) is the right body to reach a handler's own struct-decode
// error rather than the generic one.

func TestHandleAuthLoginInvalidJSONReturns400(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandleAuthSetPasswordInvalidJSONReturns400(t *testing.T) {
	srv, _ := newAuthTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/set-password", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalPasswordInvalidJSONReturns400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_pw", "pw@example.test", "password-old", "user")
	cookie := loginAs(t, srv, "pw@example.test", "password-old")

	req := httptest.NewRequest(http.MethodPost, "/api/portal/password", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandlePortalPasswordWrongCurrentPasswordReturns401 proves the wrong
// current password maps to 401 auth.invalid_credentials (ChangePassword's
// account.ErrInvalidCredentials), distinct from both the invalid-JSON and the
// generic-500 branches right around it.
func TestHandlePortalPasswordWrongCurrentPasswordReturns401(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_pw2", "pw2@example.test", "password-old", "user")
	cookie := loginAs(t, srv, "pw2@example.test", "password-old")

	req := httptest.NewRequest(http.MethodPost, "/api/portal/password", strings.NewReader(`{"current_password":"totally-wrong","new_password":"password-new-1"}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "auth.invalid_credentials" {
		t.Fatalf("error code = %q, want auth.invalid_credentials", code)
	}
}

func TestHandlePortalLanguageInvalidJSONReturns400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_lang", "lang@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "lang@example.test", "password-1")

	req := httptest.NewRequest(http.MethodPut, "/api/portal/language", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalChatSettingsInvalidJSONReturns400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_chatjson", "chatjson@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "chatjson@example.test", "password-1")

	req := httptest.NewRequest(http.MethodPut, "/api/portal/chat-settings", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandlePortalTOTPInvalidJSONReturns400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_totpdel", "totpdel@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "totpdel@example.test", "password-1")

	req := httptest.NewRequest(http.MethodDelete, "/api/portal/totp", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

func TestHandleTOTPConfirmInvalidJSONReturns400(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_totpconfirm", "totpconfirm@example.test", "password-1", "user")
	cookie := loginAs(t, srv, "totpconfirm@example.test", "password-1")

	req := httptest.NewRequest(http.MethodPost, "/api/portal/totp/confirm", strings.NewReader(`[]`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "request.invalid_json" {
		t.Fatalf("error code = %q, want request.invalid_json", code)
	}
}

// TestHandleSystemAdminModeBearerTokenWithoutCookieReturns401 proves
// handleSystemAdminMode requires a REAL session cookie even when the caller
// otherwise authenticates fine (a plain bearer token satisfies requireWebScope
// via requireAnyScope's authenticateWeb fallback) — there is no session to
// elevate/de-elevate without one.
func TestHandleSystemAdminModeBearerTokenWithoutCookieReturns401(t *testing.T) {
	srv := NewTestServer() // bearer token "dev-secret", scopes gateway:use+admin, no cookie ever set
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/system-admin-mode", `{"password":"whatever"}`))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "auth.session_invalid" {
		t.Fatalf("error code = %q, want auth.session_invalid", code)
	}
}

// TestHandleSystemAdminModeWrongPasswordReturns401 proves a system_admin
// entering step-up mode with the WRONG password gets 401
// auth.invalid_credentials (account.ErrInvalidCredentials), distinct from the
// not-system-admin (403) and generic-failure (500) branches around it.
func TestHandleSystemAdminModeWrongPasswordReturns401(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sa_wrong", "sawrong@example.test", "password-correct", "system_admin")
	cookie := loginAs(t, srv, "sawrong@example.test", "password-correct")

	req := httptest.NewRequest(http.MethodPost, "/api/portal/system-admin-mode", strings.NewReader(`{"password":"totally-wrong"}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "auth.invalid_credentials" {
		t.Fatalf("error code = %q, want auth.invalid_credentials", code)
	}
}

// TestHandleSystemAdminModeMethodNotAllowed proves the endpoint is
// POST/DELETE-only.
func TestHandleSystemAdminModeMethodNotAllowed(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sa_method", "samethod@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "samethod@example.test", "password-1")

	req := httptest.NewRequest(http.MethodGet, "/api/portal/system-admin-mode", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Allow"); got != "POST, DELETE" {
		t.Fatalf("Allow header = %q, want %q", got, "POST, DELETE")
	}
	if code := errorBodyOf(t, rec); code != "request.method_not_allowed" {
		t.Fatalf("error code = %q, want request.method_not_allowed", code)
	}
}

// expireSessionOnNthResolve wraps a real account.API and fails
// ResolveSessionDetail starting from its Nth call, while forwarding every
// other method (including EnterSystemAdminMode/ExitSystemAdminMode, which
// resolve the session through their OWN unexported receiver call, not through
// this wrapper) straight to the embedded real service. This reproduces —
// deterministically, without any wall-clock race — the "session became
// invalid between handleSystemAdminMode's two independent
// ResolveSessionDetail calls" scenario: the first (inside requireWebScope)
// must succeed, the second (the fresh-/me re-fetch after a successful
// elevate/de-elevate) must fail.
type expireSessionOnNthResolve struct {
	account.API
	n     int
	calls int
}

func (a *expireSessionOnNthResolve) ResolveSessionDetail(ctx context.Context, secret string) (store.User, store.Session, error) {
	a.calls++
	if a.calls >= a.n {
		return store.User{}, store.Session{}, errors.New("session invalidated mid-request (test double)")
	}
	return a.API.ResolveSessionDetail(ctx, secret)
}

// TestHandleSystemAdminModeSessionGoneBeforeRefetchReturns401 proves that if
// the session can no longer be resolved by the time handleSystemAdminMode
// re-fetches it for the fresh /me DTO (after EnterSystemAdminMode already
// succeeded), the handler responds 401 auth.session_invalid rather than
// panicking on the zero-value user/session or silently returning stale data.
func TestHandleSystemAdminModeSessionGoneBeforeRefetchReturns401(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_sa_gone", "sagone@example.test", "password-1", "system_admin")
	cookie := loginAs(t, srv, "sagone@example.test", "password-1")

	// Swap in the wrapped Account AFTER login (login/loginAs use the real
	// Account directly to establish the cookie); resolve call #2 is the
	// re-fetch at the bottom of handleSystemAdminMode (call #1 is
	// requireWebScope's own resolve, which must still succeed).
	srv.Account = &expireSessionOnNthResolve{API: srv.Account, n: 2}

	req := httptest.NewRequest(http.MethodPost, "/api/portal/system-admin-mode", strings.NewReader(`{"password":"password-1"}`))
	req.Header.Set(csrfHeaderName, "1")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != "auth.session_invalid" {
		t.Fatalf("error code = %q, want auth.session_invalid", code)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/store"
	"testing"
)

// claude_effort_compatibility is a management-only token setting: a portal
// session or an admin-scope bearer may change it, an inference-only bearer may
// not — not on a sibling token, not on its own token, and not by creating a
// token with the opt-in. These tests drive that end to end over HTTP, with a
// real login session and real bearer secrets, so the principal each request
// resolves to is the one the handlers actually build (session: no token id;
// bearer: the token's own id).

// effortTokenFixture is a plain user with a login session and the helpers the
// scenarios below share.
type effortTokenFixture struct {
	t       *testing.T
	srv     *Server
	session *http.Cookie
}

func newEffortTokenFixture(t *testing.T) *effortTokenFixture {
	t.Helper()
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_eff", "eff@example.test", "password-1", "user")
	return &effortTokenFixture{t: t, srv: srv, session: loginSession(t, srv, "eff@example.test", "password-1")}
}

// createViaSession creates a token through the cookie session (the management
// path) and returns its DTO and one-time secret.
func (f *effortTokenFixture) createViaSession(body string) portal.CreateTokenResponse {
	f.t.Helper()
	rec := doJSON(f.t, f.srv, f.session, http.MethodPost, "/api/portal/tokens", body)
	if rec.Code != http.StatusCreated {
		f.t.Fatalf("session create %s = %d, want 201, body = %s", body, rec.Code, rec.Body.String())
	}
	var resp portal.CreateTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		f.t.Fatalf("unmarshal create response: %v, body = %s", err, rec.Body.String())
	}
	return resp
}

// bearer sends a request authenticated by a token secret alone (no cookie).
func (f *effortTokenFixture) bearer(method, path, body, secret string) *httptest.ResponseRecorder {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, bearerRequest(method, path, body, secret))
	return rec
}

// listed returns the user's real tokens as the portal lists them, by id.
func (f *effortTokenFixture) listed() map[string]portal.TokenDTO {
	f.t.Helper()
	rec := doJSON(f.t, f.srv, f.session, http.MethodGet, "/api/portal/tokens", "")
	if rec.Code != http.StatusOK {
		f.t.Fatalf("list tokens = %d, body = %s", rec.Code, rec.Body.String())
	}
	var list portal.TokenListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		f.t.Fatalf("unmarshal list: %v, body = %s", err, rec.Body.String())
	}
	out := map[string]portal.TokenDTO{}
	for _, dto := range list.Data {
		if !dto.IsChatSession {
			out[dto.ID] = dto
		}
	}
	return out
}

func requireForbidden(t *testing.T, rec *httptest.ResponseRecorder, what string) {
	t.Helper()
	if rec.Code != http.StatusForbidden {
		t.Fatalf("%s = %d, want 403, body = %s", what, rec.Code, rec.Body.String())
	}
	if code := errorBodyOf(t, rec); code != portal.CodePrincipalForbidden {
		t.Fatalf("%s error code = %q, want %q", what, code, portal.CodePrincipalForbidden)
	}
}

// TestPortalTokenPatchByBearerCannotEnableClaudeEffortCompatibility: a bearer
// PATCH that turns the flag on answers 403 and leaves the token exactly as it
// was — including when the same request also carries other changes.
func TestPortalTokenPatchByBearerCannotEnableClaudeEffortCompatibility(t *testing.T) {
	f := newEffortTokenFixture(t)
	created := f.createViaSession(`{"name":"plain","scopes":["gateway:use"]}`)
	id, secret := created.Token.ID, created.Secret

	// Enabling the flag on ITSELF through its own bearer secret is refused.
	rec := f.bearer(http.MethodPatch, "/api/portal/tokens/"+id, `{"claude_effort_compatibility":true}`, secret)
	requireForbidden(t, rec, "bearer PATCH enabling the flag on itself")

	// ... and a mixed request is refused whole: nothing else is applied either.
	rec = f.bearer(http.MethodPatch, "/api/portal/tokens/"+id,
		`{"name":"renamed","log_communication":true,"claude_effort_compatibility":true}`, secret)
	requireForbidden(t, rec, "bearer PATCH enabling the flag together with other fields")

	got := f.listed()[id]
	if got.ClaudeEffortCompatibility {
		t.Fatalf("the rejected PATCH enabled the flag: %+v", got)
	}
	if got.Name != "plain" || got.LogCommunication {
		t.Fatalf("the rejected mixed PATCH partially applied (name=%q, log_communication=%v): %+v", got.Name, got.LogCommunication, got)
	}
}

// TestPortalTokenPatchByBearerCannotClearClaudeEffortCompatibility: the gate is
// about CHANGING the setting, so a bearer cannot switch an enabled flag off
// either.
func TestPortalTokenPatchByBearerCannotClearClaudeEffortCompatibility(t *testing.T) {
	f := newEffortTokenFixture(t)
	created := f.createViaSession(`{"name":"managed","scopes":["gateway:use"],"claude_effort_compatibility":true}`)
	id, secret := created.Token.ID, created.Secret
	if !f.listed()[id].ClaudeEffortCompatibility {
		t.Fatal("a session create with the opt-in did not enable the flag")
	}

	rec := f.bearer(http.MethodPatch, "/api/portal/tokens/"+id, `{"claude_effort_compatibility":false}`, secret)
	requireForbidden(t, rec, "bearer PATCH clearing the flag")
	if !f.listed()[id].ClaudeEffortCompatibility {
		t.Fatal("the rejected PATCH cleared the flag")
	}
}

// TestPortalTokenBearerCreateWithClaudeEffortCompatibilityIs403 pins the
// CreateToken handler's hand-inlined error ladder: without its own row for
// ErrPrincipalForbidden this fell through to a 500 "token could not be
// created". The create is refused as 403 and no token is written.
func TestPortalTokenBearerCreateWithClaudeEffortCompatibilityIs403(t *testing.T) {
	f := newEffortTokenFixture(t)
	created := f.createViaSession(`{"name":"caller","scopes":["gateway:use"]}`)
	before := len(f.listed())

	rec := f.bearer(http.MethodPost, "/api/portal/tokens",
		`{"name":"escalated","scopes":["gateway:use"],"claude_effort_compatibility":true}`, created.Secret)
	requireForbidden(t, rec, "bearer create with the opt-in")
	if after := len(f.listed()); after != before {
		t.Fatalf("a rejected create wrote a token: %d tokens before, %d after", before, after)
	}
}

// TestPortalTokenBearerKeepsEverythingElse: the gate is scoped to the flag. The
// same bearer can still create a token without it, edit unrelated fields, and
// toggle claude_model_aliases; an enabled flag survives all of that.
func TestPortalTokenBearerKeepsEverythingElse(t *testing.T) {
	f := newEffortTokenFixture(t)
	created := f.createViaSession(`{"name":"managed","scopes":["gateway:use"],"claude_effort_compatibility":true}`)
	id, secret := created.Token.ID, created.Secret

	rec := f.bearer(http.MethodPatch, "/api/portal/tokens/"+id,
		`{"name":"managed-renamed","log_communication":true,"claude_model_aliases":true}`, secret)
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer PATCH of unrelated fields = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	got := f.listed()[id]
	if got.Name != "managed-renamed" || !got.LogCommunication || !got.ClaudeModelAliases {
		t.Fatalf("the unrelated PATCH did not apply: %+v", got)
	}
	if !got.ClaudeEffortCompatibility {
		t.Fatalf("an unrelated bearer PATCH cleared claude_effort_compatibility: %+v", got)
	}

	rec = f.bearer(http.MethodPost, "/api/portal/tokens", `{"name":"child","scopes":["gateway:use"]}`, secret)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bearer create without the flag = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
}

// TestPortalTokenSessionMayChangeClaudeEffortCompatibility: the management
// path itself keeps working — a plain (non-admin) user's session sets and
// clears the flag on create and PATCH.
func TestPortalTokenSessionMayChangeClaudeEffortCompatibility(t *testing.T) {
	f := newEffortTokenFixture(t)
	created := f.createViaSession(`{"name":"managed","scopes":["gateway:use"],"claude_effort_compatibility":true}`)
	id := created.Token.ID
	if !f.listed()[id].ClaudeEffortCompatibility {
		t.Fatal("session create with the opt-in did not enable the flag")
	}

	for _, want := range []bool{false, true} {
		body := `{"claude_effort_compatibility":false}`
		if want {
			body = `{"claude_effort_compatibility":true}`
		}
		rec := doJSON(t, f.srv, f.session, http.MethodPatch, "/api/portal/tokens/"+id, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("session PATCH %s = %d, want 200, body = %s", body, rec.Code, rec.Body.String())
		}
		if got := f.listed()[id].ClaudeEffortCompatibility; got != want {
			t.Fatalf("after session PATCH %s the flag reads %v, want %v", body, got, want)
		}
	}
}

// TestPortalTokenAdminBearerMayChangeClaudeEffortCompatibility: a bearer that
// was minted with the admin scope is a management principal too.
func TestPortalTokenAdminBearerMayChangeClaudeEffortCompatibility(t *testing.T) {
	srv := NewTestServer() // dev-secret: gateway:use + admin

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens",
		`{"name":"admin-managed","scopes":["gateway:use"],"claude_effort_compatibility":true}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin bearer create with the opt-in = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	var created portal.CreateTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !created.Token.ClaudeEffortCompatibility {
		t.Fatalf("admin bearer create did not enable the flag: %+v", created.Token)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+created.Token.ID, `{"claude_effort_compatibility":false}`))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin bearer PATCH = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var patched portal.TokenDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if patched.ClaudeEffortCompatibility {
		t.Fatalf("admin bearer PATCH did not clear the flag: %+v", patched)
	}
}

// TestPortalTokenInferenceOnlyBearerCannotCreateWithClaudeEffortCompatibility
// uses the shared test server's dev bearer, minted WITHOUT the admin scope, as
// the inference-only principal (distinct from the login-session fixture above:
// here the very first request is already a bearer).
func TestPortalTokenInferenceOnlyBearerCannotCreateWithClaudeEffortCompatibility(t *testing.T) {
	srv := NewTestServerWithTokenScopes([]string{"gateway:use"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPost, "/api/portal/tokens",
		`{"name":"escalated","scopes":["gateway:use"],"claude_effort_compatibility":true}`))
	requireForbidden(t, rec, "inference-only bearer create with the opt-in")

	// The same bearer still creates a token that does not touch the flag.
	id := createEditableToken(t, srv, "no-flag")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, newJSONRequest(http.MethodPatch, "/api/portal/tokens/"+id, `{"claude_effort_compatibility":true}`))
	requireForbidden(t, rec, "inference-only bearer PATCH enabling the flag")
}

// TestSessionPrincipalCarriesNoTokenID pins the signal the management gate
// reads: a session/loopback principal has no token id (a bearer always has
// one). If sessionPrincipal ever started setting ID, every session would stop
// being recognised as a session and be treated like an inference bearer.
func TestSessionPrincipalCarriesNoTokenID(t *testing.T) {
	for _, role := range []string{"user", "admin", "system_admin"} {
		for _, elevated := range []bool{false, true} {
			if got := sessionPrincipal(store.User{ID: "usr_1", Role: role}, elevated); got.ID != "" {
				t.Fatalf("sessionPrincipal(role=%s, elevated=%v).ID = %q, want empty", role, elevated, got.ID)
			}
		}
	}
}

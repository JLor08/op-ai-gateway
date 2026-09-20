// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"strings"
	"testing"
)

// The images endpoint must admit the run executor's loopback principal, which
// carries no bearer token at all.
func TestInternalOrBearerAcceptsTheLoopbackPair(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user", ChatLogCommunication: true},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	tok, ok := s.authenticateInternalOrBearer(w, r)
	if !ok {
		t.Fatalf("expected the loopback pair to authenticate; status %d", w.Code)
	}
	if tok.UserID != "usr_1" || tok.ID != "" {
		t.Fatalf("unexpected principal: %+v", tok)
	}
}

// A loopback principal must never be elevated, even for a system_admin user:
// it is not an interactive session that went through the System-Admin
// step-up, and sessionPrincipal only grants the "system" scope when its
// elevated argument is true (auth.go). auth.Token has no standalone Elevated
// field, so the "system" scope is the only observable signal -- and it is
// non-vacuous here specifically because the user's role WOULD otherwise
// qualify for it.
func TestInternalOrBearerLoopbackPrincipalIsNeverElevated(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_admin": {ID: "usr_admin", DisplayName: "Sid", Role: "system_admin"},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_admin")
	w := httptest.NewRecorder()

	tok, ok := s.authenticateInternalOrBearer(w, r)
	if !ok {
		t.Fatalf("expected the loopback pair to authenticate; status %d", w.Code)
	}
	if tok.HasScope("system") {
		t.Fatal("a loopback principal must not be elevated, even for a system_admin user")
	}
}

// A wrong secret must fall through to the bearer leg WITHOUT the loopback
// branch having written anything -- and with no bearer present that leg then
// 401s. Fail-closed on each of the three loopback conditions.
func TestInternalOrBearerFallsThroughOnAWrongSecret(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user"},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "wrong")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("a wrong secret must not authenticate")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 from the bearer leg", w.Code)
	}
}

func TestInternalOrBearerFallsThroughOnAnUnknownUser(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "nobody")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("an unresolvable user id must not authenticate")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestInternalOrBearerFallsThroughWhenNoSecretIsConfigured(t *testing.T) {
	s := newInternalAuthServer("", fakeUserLookup{"usr_1": {ID: "usr_1"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("an empty configured secret must never match")
	}
}

func TestInternalOrBearerFallsThroughWhenUsersIsNil(t *testing.T) {
	s := &Server{internalAuthSecret: "s3cret"} // users nil
	r := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	if _, ok := s.authenticateInternalOrBearer(w, r); ok {
		t.Fatal("a nil user lookup must not authenticate")
	}
}

// THE 403 BODY IS PART OF THE SHIPPED CONTRACT. /v1/images/generations went
// through requireAnyScope on every release before this branch, so its
// insufficient-scope body must stay byte-identical: same code, same status
// and the same "insufficient token scope" prose. This asserts the two
// helpers' responses against each other rather than against a literal, so it
// keeps failing if either side is reworded -- and the sibling literal
// assertion below keeps it from passing vacuously should BOTH be changed to
// the same new wording.
func TestInternalOrBearerScopeRefusalMatchesRequireAnyScopeByteForByte(t *testing.T) {
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{ID: "tok_1", UserID: "usr_1", Active: true, Scopes: []string{"gateway:use"}}, "secret")
	s := &Server{
		internalAuthSecret: "s3cret",
		users:              fakeUserLookup{"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user"}},
		Tokens:             tokens,
	}

	// The loopback leg authenticates, then fails the scope check: a loopback
	// principal for a plain "user" is never elevated, so it cannot carry
	// "system".
	loopback := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	loopback.Header.Set(internalAuthHeaderName, "s3cret")
	loopback.Header.Set(internalUserHeaderName, "usr_1")
	gotW := httptest.NewRecorder()
	if _, ok := s.requireInternalOrBearerAnyScope(gotW, loopback, "system"); ok {
		t.Fatal("a non-elevated loopback principal must not satisfy the system scope")
	}

	// The same refusal from the helper this endpoint used before the branch.
	bearer := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	bearer.Header.Set("Authorization", "Bearer secret")
	wantW := httptest.NewRecorder()
	if _, ok := s.requireAnyScope(wantW, bearer, "system"); ok {
		t.Fatal("a gateway:use bearer must not satisfy the system scope")
	}

	if gotW.Code != wantW.Code {
		t.Fatalf("status = %d, want requireAnyScope's %d", gotW.Code, wantW.Code)
	}
	if gotW.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", gotW.Code)
	}
	if gotW.Body.String() != wantW.Body.String() {
		t.Fatalf("body = %q, want requireAnyScope's %q", gotW.Body.String(), wantW.Body.String())
	}
	// The literal the pre-branch endpoint shipped, pinned so the pair above
	// cannot be satisfied by renaming both at once.
	const want = `{"error":{"code":"auth.insufficient_scope","message":"insufficient token scope"}}`
	if strings.TrimSpace(gotW.Body.String()) != want {
		t.Fatalf("body = %q, want the shipped literal %q", strings.TrimSpace(gotW.Body.String()), want)
	}
}

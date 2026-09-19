// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/store"
	"testing"
)

// fakeUserLookup lets the internal-auth branch resolve a user id to a store.User
// without a real account service.
type fakeUserLookup map[string]store.User

func (f fakeUserLookup) UserByID(_ context.Context, id string) (store.User, error) {
	u, ok := f[id]
	if !ok {
		return store.User{}, errors.New("not found")
	}
	return u, nil
}

func newInternalAuthServer(secret string, users fakeUserLookup) *Server {
	return &Server{internalAuthSecret: secret, users: users}
}

func TestInternalAuthAcceptsCorrectSecret(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{
		"usr_1": {ID: "usr_1", DisplayName: "Ann", Role: "user", ChatLogCommunication: true},
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()

	tok, ok := s.authenticateWeb(w, r)
	if !ok {
		t.Fatal("expected internal auth to succeed")
	}
	if tok.UserID != "usr_1" || tok.ID != "" || !tok.HasScope("gateway:use") {
		t.Fatalf("unexpected principal: %+v", tok)
	}
	if !tok.LogCommunication {
		t.Fatal("expected capture flag copied from user profile")
	}
}

func TestInternalAuthRejectsWrongOrMissingSecret(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{"usr_1": {ID: "usr_1"}})
	for _, hdr := range []string{"", "wrong"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if hdr != "" {
			r.Header.Set(internalAuthHeaderName, hdr)
		}
		r.Header.Set(internalUserHeaderName, "usr_1")
		w := httptest.NewRecorder()
		if _, ok := s.authenticateWeb(w, r); ok {
			t.Fatalf("expected rejection for secret %q", hdr)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 fall-through for secret %q, got %d", hdr, w.Code)
		}
	}
}

func TestInternalAuthDisabledWhenNoSecret(t *testing.T) {
	s := newInternalAuthServer("", fakeUserLookup{"usr_1": {ID: "usr_1"}})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(internalAuthHeaderName, "")
	r.Header.Set(internalUserHeaderName, "usr_1")
	w := httptest.NewRecorder()
	if _, ok := s.authenticateWeb(w, r); ok {
		t.Fatal("empty secret must not authenticate")
	}
}

func TestInternalAuthFallsThroughOnUnknownUser(t *testing.T) {
	s := newInternalAuthServer("s3cret", fakeUserLookup{})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set(internalAuthHeaderName, "s3cret")
	r.Header.Set(internalUserHeaderName, "usr_missing")
	w := httptest.NewRecorder()
	if _, ok := s.authenticateWeb(w, r); ok {
		t.Fatal("unknown user must not authenticate")
	}
}

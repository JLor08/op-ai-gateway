// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

const (
	baOwnerSecret = "ba-owner-secret"
	baOtherSecret = "ba-other-secret"
	baAdminSecret = "ba-admin-secret"
	baOwnedServer = "srv_owned"
	baOtherServer = "srv_foreign"
)

// newBenchmarkActiveFixture builds a *Server with a memory route store holding two
// servers — srv_owned (owned by usr_a) and srv_foreign (owned by a distinct user
// neither token principal is) — plus plain bearer tokens for the owner (usr_a,
// gateway:use), a non-owner who owns NEITHER server (usr_b, gateway:use), and a
// SYSTEM-scope admin (usr_c, gateway:use+admin+system — Phase B, spec 2026-08-10:
// authorizeServer's blanket "any admin manages every server" bypass is gone; a
// plain "admin" token with no ownership/group link now gets the SAME no-leak
// empty result as a non-owner). Mirrors newPerfTestServer's construction.
func newBenchmarkActiveFixture(t *testing.T) *Server {
	t.Helper()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_a", Email: "a@example.test", DisplayName: "Owner A", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_b", Email: "b@example.test", DisplayName: "Other B", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_c", Email: "c@example.test", DisplayName: "Admin C", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_a", UserID: "usr_a", Name: "Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, baOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_b", UserID: "usr_b", Name: "Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, baOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_c", UserID: "usr_c", Name: "Admin Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, baAdminSecret); err != nil {
		t.Fatalf("CreatePlainToken admin: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: baOwnedServer, Name: "Owned Host", Domain: "owned.example.test", Provider: routing.ProviderMock, Endpoint: "mock://owned", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer owned: %v", err)
	}
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: baOtherServer, Name: "Foreign Host", Domain: "foreign.example.test", Provider: routing.ProviderMock, Endpoint: "mock://foreign", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer foreign: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), baOwnedServer, []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners owned: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), baOtherServer, []string{"usr_foreign"}); err != nil {
		t.Fatalf("SetServerOwners foreign: %v", err)
	}
	recorder := usage.NewRecorder()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: recorder, Routes: routeStore})
	return New(ServerDeps{
		Tokens: tokens,
		Usage:  recorder,
		Routes: routeStore,
		Portal: svc,
	})
}

func TestHandlePortalBenchmarksActive(t *testing.T) {
	s := newBenchmarkActiveFixture(t)
	// Two running benchmarks: one on the owned server, one on a foreign server.
	if _, ok := s.Benchmarks.TryStart(baOwnedServer, "server", "speed", 2, time.Now(), func() {}); !ok {
		t.Fatal("TryStart owned")
	}
	if _, ok := s.Benchmarks.TryStart(baOtherServer, "server", "speed", 1, time.Now(), func() {}); !ok {
		t.Fatal("TryStart foreign")
	}

	get := func(tok string) []BenchmarkStatus {
		req := httptest.NewRequest(http.MethodGet, "/api/portal/benchmarks/active", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
		}
		var out struct {
			Data []BenchmarkStatus `json:"data"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out.Data
	}

	owner := get(baOwnerSecret)
	if len(owner) != 1 || owner[0].ServerID != baOwnedServer {
		t.Fatalf("owner should see only their running run, got %+v", owner)
	}
	other := get(baOtherSecret)
	if len(other) != 0 {
		t.Fatalf("non-owner must see no runs (no leak), got %+v", other)
	}
	admin := get(baAdminSecret) // system-scope, see newBenchmarkActiveFixture
	if len(admin) != 2 {
		t.Fatalf("system-admin should see both runs, got %d (%+v)", len(admin), admin)
	}

	// Method gate: only GET is allowed.
	req := httptest.NewRequest(http.MethodPost, "/api/portal/benchmarks/active", nil)
	req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST should be 405, got %d", rec.Code)
	}
}

func TestHandlePortalBenchmarksActiveRequiresAuth(t *testing.T) {
	s := newBenchmarkActiveFixture(t)
	// The auth/scope gate runs before any registry read: a request with no
	// bearer (and no session) is rejected 401, never reaching the handler body.
	req := httptest.NewRequest(http.MethodGet, "/api/portal/benchmarks/active", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer should be 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

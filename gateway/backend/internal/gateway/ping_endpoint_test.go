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
	"testing"
	"time"
)

const (
	pingOwnerSecret = "ping-owner-secret"
	pingOtherSecret = "ping-other-secret"
	pingAdminSecret = "ping-admin-secret"
	pingDomainSrv   = "srv_ping_domain"
	pingNoDomainSrv = "srv_ping_nodomain"
)

// newPingFixture builds a *Server with a memory route store holding two servers
// owned by usr_a (one with a domain, one without) plus plain bearer tokens for
// the owner (usr_a, gateway:use), a non-owner who owns neither server (usr_b,
// gateway:use), and a SYSTEM-scope admin (usr_c, gateway:use+admin+system --
// Phase B, spec 2026-08-10: authorizeServer's blanket "any admin manages every
// server" bypass is gone, so a caller with no ownership/group link needs the
// unconditional system bypass here). Mirrors newBenchmarkActiveFixture's
// construction. The ping action never touches NetBird — it only reads the
// server via Portal.GetServer and runs an ICMP echo.
func newPingFixture(t *testing.T) (*Server, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_a", Email: "a@example.test", DisplayName: "Owner A", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_b", Email: "b@example.test", DisplayName: "Other B", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_c", Email: "c@example.test", DisplayName: "Admin C", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_a", UserID: "usr_a", Name: "Owner Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, pingOwnerSecret); err != nil {
		t.Fatalf("CreatePlainToken owner: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_b", UserID: "usr_b", Name: "Other Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, pingOtherSecret); err != nil {
		t.Fatalf("CreatePlainToken other: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_c", UserID: "usr_c", Name: "Admin Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, pingAdminSecret); err != nil {
		t.Fatalf("CreatePlainToken admin: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: pingDomainSrv, Name: "Domain Host", Domain: "127.0.0.1", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer domain: %v", err)
	}
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: pingNoDomainSrv, Name: "NoDomain Host", Domain: "", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer no-domain: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), pingDomainSrv, []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners domain: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), pingNoDomainSrv, []string{"usr_a"}); err != nil {
		t.Fatalf("SetServerOwners no-domain: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Users:  dir,
		Tokens: dir,
		Routes: routeStore,
		Clock:  func() time.Time { return now },
	})
	return New(ServerDeps{Tokens: tokens, Routes: routeStore, Portal: svc}), routeStore
}

// TestHandlePortalServerPing drives the owner-or-admin portal ping endpoint
// POST /api/portal/servers/{id}/ping. A real ICMP result is covered by
// internal/ping; here we assert the endpoint's own gating + response shape.
func TestHandlePortalServerPing(t *testing.T) {
	s, _ := newPingFixture(t)

	t.Run("owner token -> 200 {ok:...}", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+pingDomainSrv+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var body struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		// The real ICMP outcome (true/false) depends on the sandbox's ICMP
		// permissions; only the shape/status is asserted here.
	})

	t.Run("system-admin token -> 200 {ok:...}", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+pingDomainSrv+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingAdminSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("no-domain server -> 200 {ok:false, error}", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+pingNoDomainSrv+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		var body struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		if body.OK {
			t.Fatalf("ok = true, want false for an empty-domain server: %s", rec.Body.String())
		}
		if body.Error == "" {
			t.Fatalf("error empty, want a 'no domain' message: %s", rec.Body.String())
		}
	})

	t.Run("non-owner non-admin token -> 404 no-leak", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+pingNoDomainSrv+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingOtherSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("non-POST -> 405", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/portal/servers/"+pingNoDomainSrv+"/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unknown id -> 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/srv_missing/ping", nil)
		req.Header.Set("Authorization", "Bearer "+pingOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestHandlePortalServerPingRequiresAuth(t *testing.T) {
	s, _ := newPingFixture(t)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/"+pingNoDomainSrv+"/ping", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing bearer should be 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
}

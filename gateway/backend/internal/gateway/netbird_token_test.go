// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

// netbirdTokenFixtureNow is the fixed clock used by newNetbirdEndpointFixture
// (2026-07-25T12:00:00Z). Expiration fixtures below are chosen relative to it so
// days_remaining is an exact, non-flaky assertion.
var netbirdTokenFixtureNow = time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

// netbirdTokenFakeServer builds a fake NetBird admin API implementing exactly the
// endpoints RotateNetbirdToken/NetbirdTokenStatus call: a single current user
// "u1" with one existing token "old1" (expiring 2026-08-10, so days_remaining=16
// from the fixed fixture clock), a token-create endpoint minting "new1" (valid
// 365 days -> 2027-07-25, days_remaining=365), idempotent deletes, and a
// verify (Ping) endpoint on /api/groups whose response is controlled by
// verifyStatus (200 = verify succeeds).
func netbirdTokenFakeServer(t *testing.T, verifyStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "u1", "is_current": true}})
	})
	mux.HandleFunc("/api/users/u1/tokens", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "old1", "name": "op-gateway", "expiration_date": "2026-08-10T00:00:00Z", "last_used": "2026-08-01T00:00:00Z"},
			})
		case http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"plain_token": "nbp_new",
				"personal_access_token": map[string]any{
					"id":              "new1",
					"name":            "op-gateway",
					"expiration_date": "2027-07-25T12:00:00Z",
				},
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/users/u1/tokens/old1", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "old1", "name": "op-gateway", "expiration_date": "2026-08-10T00:00:00Z", "last_used": "2026-08-01T00:00:00Z",
			})
		case http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	mux.HandleFunc("/api/users/u1/tokens/new1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		if verifyStatus != http.StatusOK {
			w.WriteHeader(verifyStatus)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	return fake
}

// newNetbirdModuleDisabledFixture builds a *Server with the NetBird module OFF
// (no UpdateSystemSettings call at all, so NetbirdConfig's ok is false) plus a
// system-scope token, so a test can prove the token endpoints map
// ErrNetbirdModuleDisabled -> 409 without making any NetBird call.
func newNetbirdModuleDisabledFixture(t *testing.T) *Server {
	t.Helper()
	now := netbirdTokenFixtureNow
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_s", Email: "s@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_s", UserID: "usr_s", Name: "System Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, nbSystemSecret); err != nil {
		t.Fatalf("CreatePlainToken system: %v", err)
	}
	cipher, err := capture.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	svc := portal.NewService(portal.ServiceDeps{
		Users:          dir,
		Tokens:         dir,
		Routes:         routing.NewMemoryStore(),
		SystemSettings: portal.NewMemorySystemSettings(),
		Cipher:         cipher,
		Clock:          func() time.Time { return now },
	})
	return New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc})
}

// TestHandleSystemNetbirdTokenStatus: the endpoint is system-scoped + GET-only
// and returns the live NetbirdTokenStatusDTO for the account's sole token.
func TestHandleSystemNetbirdTokenStatus(t *testing.T) {
	fake := netbirdTokenFakeServer(t, http.StatusOK)
	s := newNetbirdEndpointFixture(t, fake.URL)

	// gateway:use token -> 403 (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/token-status", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use token status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, wrong method -> 405.
	req = httptest.NewRequest(http.MethodPost, "/api/system/netbird/token-status", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("system POST status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, GET -> 200 with the token status DTO.
	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/token-status", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system GET status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var dto portal.NetbirdTokenStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !dto.Known {
		t.Fatalf("known = false, want true: %+v", dto)
	}
	if dto.Name != "op-gateway" || dto.ExpirationDate != "2026-08-10T00:00:00Z" || dto.LastUsed != "2026-08-01T00:00:00Z" {
		t.Fatalf("dto = %+v, want the fake's old1 token metadata", dto)
	}
	if dto.DaysRemaining != 16 {
		t.Fatalf("days_remaining = %d, want 16 (2026-07-25T12:00 -> 2026-08-10T00:00, ceil(15.5d))", dto.DaysRemaining)
	}
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatalf("token status leaked the admin token: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdRotateToken: the endpoint is system-scoped + POST-only
// and rotates to a freshly-created, verified token, deleting the old one.
func TestHandleSystemNetbirdRotateToken(t *testing.T) {
	fake := netbirdTokenFakeServer(t, http.StatusOK)
	s := newNetbirdEndpointFixture(t, fake.URL)

	// gateway:use token -> 403 (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/rotate-token", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use token status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, wrong method -> 405.
	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/rotate-token", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("system GET status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, POST -> 200 with the rotation result.
	req = httptest.NewRequest(http.MethodPost, "/api/system/netbird/rotate-token", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system POST status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var res portal.RotateNetbirdTokenResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if res.ExpirationDate != "2027-07-25T12:00:00Z" {
		t.Fatalf("expiration_date = %q, want the new token's 2027-07-25T12:00:00Z", res.ExpirationDate)
	}
	if res.DaysRemaining != 365 {
		t.Fatalf("days_remaining = %d, want 365", res.DaysRemaining)
	}
	if !res.OldDeleted {
		t.Fatalf("old_deleted = false, want true (the fake's DELETE succeeds)")
	}
	if res.OldUnknown {
		t.Fatalf("old_unknown = true, want false (the sole existing token resolves unambiguously)")
	}
	// The new plaintext token must never appear in the response.
	if strings.Contains(rec.Body.String(), "nbp_new") {
		t.Fatalf("rotate response leaked the new plaintext token: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdRotateTokenAuthFailReturns502: when the freshly created
// token fails verification (NetBird answers 401 on the Ping/verify call), the
// endpoint maps the resulting netbird.ErrAuth to 502 (the shared
// writePortalServerError mapping), leaking neither the old nor the new token.
func TestHandleSystemNetbirdRotateTokenAuthFailReturns502(t *testing.T) {
	fake := netbirdTokenFakeServer(t, http.StatusUnauthorized)
	s := newNetbirdEndpointFixture(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/rotate-token", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netbird.auth_failed") {
		t.Fatalf("body missing code netbird.auth_failed: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "super-secret-token") || strings.Contains(rec.Body.String(), "nbp_new") {
		t.Fatalf("rotate-failure response leaked a token: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdRotateTokenModuleDisabledReturns409: when the NetBird
// module is not configured, RotateNetbirdToken returns ErrNetbirdModuleDisabled,
// mapped to 409 without making any NetBird call.
func TestHandleSystemNetbirdRotateTokenModuleDisabledReturns409(t *testing.T) {
	s := newNetbirdModuleDisabledFixture(t)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/rotate-token", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netbird.module_disabled") {
		t.Fatalf("body missing code netbird.module_disabled: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdTokenStatusModuleDisabled: the status endpoint never
// errors on an unconfigured module — it reports known:false at 200 (mirrors the
// service contract: NetbirdTokenStatus returns (DTO{Known:false}, nil)).
func TestHandleSystemNetbirdTokenStatusModuleDisabled(t *testing.T) {
	s := newNetbirdModuleDisabledFixture(t)

	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/token-status", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var dto portal.NetbirdTokenStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if dto.Known {
		t.Fatalf("known = true, want false (module unconfigured): %+v", dto)
	}
}

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
	"sync"
	"testing"
	"time"
)

// newGatewayKeyFixture builds a *Server whose Portal has the NetBird module either
// enabled (pointing at fakeURL) or disabled, plus a gateway:use token and a
// system-scope token (reusing the netbird-endpoint secrets).
func newGatewayKeyFixture(t *testing.T, fakeURL string, enabled bool) *Server {
	t.Helper()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_u", Email: "u@example.test", DisplayName: "Plain User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_s", Email: "s@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_u", UserID: "usr_u", Name: "Use Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, nbUseSecret); err != nil {
		t.Fatalf("CreatePlainToken use: %v", err)
	}
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
	if enabled {
		if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
			NetbirdEnabled: nbBoolPtr(true),
			NetbirdURL:     nbStrPtr(fakeURL),
			NetbirdGroups:  &[]string{"gateways"},
			NetbirdToken:   nbStrPtr("super-secret-token"),
		}); err != nil {
			t.Fatalf("enable netbird: %v", err)
		}
	}
	return New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc})
}

// gatewayKeyFakeNetbird is a minimal NetBird admin API supporting the mint path:
// GET /api/groups (list) + POST /api/groups (create op-gw-portal) + POST
// /api/setup-keys (return the key, capture auto_groups).
type gatewayKeyFakeNetbird struct {
	mu             sync.Mutex
	groups         []map[string]any
	lastAutoGroups []string
}

func newGatewayKeyFakeNetbird(t *testing.T) (*httptest.Server, *gatewayKeyFakeNetbird) {
	t.Helper()
	f := &gatewayKeyFakeNetbird{groups: []map[string]any{{"id": "g-mod", "name": "gateways"}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			snapshot := append([]map[string]any(nil), f.groups...)
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(snapshot)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		grp := map[string]any{"id": "g-" + body.Name, "name": body.Name}
		f.mu.Lock()
		f.groups = append(f.groups, grp)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(grp)
	})
	mux.HandleFunc("/api/setup-keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			AutoGroups []string `json:"auto_groups"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.lastAutoGroups = body.AutoGroups
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "sk-id", "key": "nbkey-secret-value"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f
}

func (f *gatewayKeyFakeNetbird) autoGroups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.lastAutoGroups...)
}

// TestHandleSystemNetbirdGatewaySetupKey: the mint endpoint is system-scoped (a
// gateway:use token is rejected 403, POST-only), and a system token gets a 200
// JSON with setup_key + netbird_setup_command whose body NEVER contains the admin
// token; the key's auto_groups holds the resolve-or-created op-gw-portal group.
func TestHandleSystemNetbirdGatewaySetupKey(t *testing.T) {
	fake, capture := newGatewayKeyFakeNetbird(t)
	s := newGatewayKeyFixture(t, fake.URL, true)

	// gateway:use token -> 403 insufficient scope (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/gateway-setup-key", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use POST status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, wrong method -> 405.
	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/gateway-setup-key", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("system GET status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, POST -> 200 with {setup_key, netbird_setup_command}.
	req = httptest.NewRequest(http.MethodPost, "/api/system/netbird/gateway-setup-key", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system POST status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// The admin token must NEVER appear anywhere in the response body.
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatalf("response leaked the admin token: %s", rec.Body.String())
	}
	var out struct {
		SetupKey            string `json:"setup_key"`
		NetbirdSetupCommand string `json:"netbird_setup_command"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if out.SetupKey != "nbkey-secret-value" {
		t.Fatalf("setup_key = %q, want the generated key", out.SetupKey)
	}
	if want := "netbird up --management-url " + fake.URL + " --setup-key nbkey-secret-value"; out.NetbirdSetupCommand != want {
		t.Fatalf("netbird_setup_command = %q, want %q", out.NetbirdSetupCommand, want)
	}
	// The key joined the resolve-or-created op-gw-portal group.
	if got := capture.autoGroups(); len(got) != 1 || got[0] != "g-op-gw-portal" {
		t.Fatalf("auto_groups = %v, want [g-op-gw-portal]", got)
	}
}

// TestHandleSystemNetbirdGatewaySetupKeyAuthFailed: when the NetBird admin API
// answers 401/403 to the mint path, CreateGatewaySetupKey returns netbird.ErrAuth,
// which the shared netbird-error→HTTP mapping turns into HTTP 502
// netbird.auth_failed (not the 500 default). The group-resolve succeeds; the
// setup-key call is the one that auth-fails, so the whole mint path is exercised.
func TestHandleSystemNetbirdGatewaySetupKeyAuthFailed(t *testing.T) {
	// A fake NetBird admin API that serves groups normally but rejects the
	// setup-key mint with 401 (the token is not accepted for that call).
	mux := http.NewServeMux()
	mux.HandleFunc("/api/groups", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "g-mod", "name": "gateways"}})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "g-" + body.Name, "name": body.Name})
	})
	mux.HandleFunc("/api/setup-keys", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)
	s := newGatewayKeyFixture(t, fake.URL, true)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/gateway-setup-key", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netbird.auth_failed") {
		t.Fatalf("body missing code netbird.auth_failed: %s", rec.Body.String())
	}
	// The admin token must NEVER appear in the error response body.
	if strings.Contains(rec.Body.String(), "super-secret-token") {
		t.Fatalf("error response leaked the admin token: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdGatewaySetupKeyModuleDisabled: with the module off, the
// mint endpoint maps ErrNetbirdModuleDisabled to 409 netbird.module_disabled.
func TestHandleSystemNetbirdGatewaySetupKeyModuleDisabled(t *testing.T) {
	s := newGatewayKeyFixture(t, "http://netbird.invalid", false)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/gateway-setup-key", nil)
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

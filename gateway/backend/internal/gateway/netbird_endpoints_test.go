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

const (
	nbUseSecret    = "nb-use-secret"
	nbSystemSecret = "nb-system-secret"
)

// newNetbirdEndpointFixture builds a *Server whose Portal has the NetBird module
// ENABLED (pointing at fakeURL — a stand-in NetBird admin API), plus two plain
// bearer tokens: a gateway:use-only token (non-system) and a system-scope token.
func newNetbirdEndpointFixture(t *testing.T, fakeURL string) *Server {
	t.Helper()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
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
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		NetbirdEnabled: nbBoolPtr(true),
		NetbirdURL:     nbStrPtr(fakeURL),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   nbStrPtr("super-secret-token"),
	}); err != nil {
		t.Fatalf("enable netbird: %v", err)
	}
	return New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc})
}

func nbBoolPtr(b bool) *bool    { return &b }
func nbStrPtr(s string) *string { return &s }

// newNetbirdDeleteFixture builds a *Server whose Portal has the NetBird module
// enabled (pointing at fakeURL) + a system-scope token, and returns the SAME
// routing store the Portal uses so a test can seed a server with peer/setup-key ids.
func newNetbirdDeleteFixture(t *testing.T, fakeURL string) (*Server, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
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
	routeStore := routing.NewMemoryStore()
	svc := portal.NewService(portal.ServiceDeps{
		Users:          dir,
		Tokens:         dir,
		Routes:         routeStore,
		SystemSettings: portal.NewMemorySystemSettings(),
		Cipher:         cipher,
		Clock:          func() time.Time { return now },
	})
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		NetbirdEnabled: nbBoolPtr(true),
		NetbirdURL:     nbStrPtr(fakeURL),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   nbStrPtr("super-secret-token"),
	}); err != nil {
		t.Fatalf("enable netbird: %v", err)
	}
	return New(ServerDeps{Tokens: tokens, Routes: routeStore, Portal: svc}), routeStore
}

// TestPortalServerDeletePeerThreadsFlag: the DELETE arm threads ?delete_peer=true
// into the service and surfaces netbird_peer_delete_failed when a NetBird cleanup
// fails; without delete_peer the flag is omitted (best-effort, row deleted either
// way).
func TestPortalServerDeletePeerThreadsFlag(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)

	// fake NetBird: DELETE /api/peers/{id} -> 500 (failure), everything else 200/404.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/peers/"):
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"message":"boom"}`))
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/setup-keys/"):
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer fake.Close()

	seedServer := func(t *testing.T, routeStore *routing.MemoryStore) {
		t.Helper()
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_del", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerID: "peer-1", NetbirdSetupKeyID: "sk-1", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
	}

	t.Run("delete_peer=true surfaces the failure flag", func(t *testing.T) {
		s, routeStore := newNetbirdDeleteFixture(t, fake.URL)
		seedServer(t, routeStore)

		req := httptest.NewRequest(http.MethodDelete, "/api/portal/servers/srv_del?delete_peer=true", nil)
		req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body struct {
			OK                      bool `json:"ok"`
			NetbirdPeerDeleteFailed bool `json:"netbird_peer_delete_failed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
		}
		if !body.OK {
			t.Fatalf("ok = false, want true")
		}
		if !body.NetbirdPeerDeleteFailed {
			t.Fatalf("netbird_peer_delete_failed = false, want true (delete_peer must thread + a DeletePeer 500 must surface): %s", rec.Body.String())
		}
	})

	t.Run("no delete_peer omits the failure flag", func(t *testing.T) {
		s, routeStore := newNetbirdDeleteFixture(t, fake.URL)
		seedServer(t, routeStore)

		req := httptest.NewRequest(http.MethodDelete, "/api/portal/servers/srv_del", nil)
		req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "netbird_peer_delete_failed") {
			t.Fatalf("response must omit netbird_peer_delete_failed when false: %s", rec.Body.String())
		}
	})
}

// TestHandleSystemNetbirdGroups: the group-list endpoint is system-scoped — a
// gateway:use-only token is rejected (403) — and a system token gets the
// {data:[{id,name}]} shape (the exact shape the frontend expects).
func TestHandleSystemNetbirdGroups(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/groups" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "g-mod", "name": "gateways"},
				{"id": "g-prod", "name": "prod"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fake.Close()
	s := newNetbirdEndpointFixture(t, fake.URL)

	// gateway:use token -> 403 insufficient scope (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/groups", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use token status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token -> 200 with {data:[{id,name}]}.
	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/groups", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system token status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 || out.Data[0].ID != "g-mod" || out.Data[0].Name != "gateways" {
		t.Fatalf("groups = %+v, want the two fake groups with id+name", out.Data)
	}
}

// TestHandleSystemNetbirdPeers: the peer-list endpoint is system-scoped — a
// gateway:use-only token is rejected (403) — a system token gets the
// {data:[{id,name,dns_label,connected}]} shape, and the response leaks ONLY those
// four fields (never the ssh/expiration internals).
func TestHandleSystemNetbirdPeers(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/peers" && r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "peer-1", "name": "gpu-box", "dns_label": "gpu-box.netbird.io", "connected": true, "ssh_enabled": true, "login_expiration_enabled": true, "inactivity_expiration_enabled": true},
				{"id": "peer-2", "name": "cpu-box", "dns_label": "cpu-box.netbird.io", "connected": false},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fake.Close()
	s := newNetbirdEndpointFixture(t, fake.URL)

	// gateway:use token -> 403 insufficient scope (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/peers", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use token status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token -> 200 with {data:[{id,name,dns_label,connected}]}.
	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/peers", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("system token status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	// The internals must never be exposed.
	for _, leak := range []string{"ssh_enabled", "login_expiration_enabled", "inactivity_expiration_enabled"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("peers payload leaked %q: %s", leak, rec.Body.String())
		}
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data) != 2 {
		t.Fatalf("peers len = %d, want 2 (%+v)", len(out.Data), out.Data)
	}
	first := out.Data[0]
	if first["id"] != "peer-1" || first["name"] != "gpu-box" || first["dns_label"] != "gpu-box.netbird.io" || first["connected"] != true {
		t.Fatalf("peers[0] = %+v, want id/name/dns_label/connected of the first fake peer", first)
	}
	// Each object carries EXACTLY the four safe fields.
	for i, p := range out.Data {
		if len(p) != 4 {
			t.Fatalf("peers[%d] has %d keys, want exactly 4 (id,name,dns_label,connected): %+v", i, len(p), p)
		}
	}
}

// TestHandleSystemServerNetbird: the linkage editor is system-scoped (a
// gateway:use token is rejected 403), PUT-only, and returns a no-leak 404 for an
// unknown server id + for a path that is not {id}/netbird.
func TestHandleSystemServerNetbird(t *testing.T) {
	s := newNetbirdEndpointFixture(t, "http://netbird.invalid")
	body := strings.NewReader(`{"netbird_enabled":true,"netbird_peer_id":"p1"}`)

	// gateway:use token -> 403 (system-scoped endpoint).
	req := httptest.NewRequest(http.MethodPut, "/api/system/servers/srv-x/netbird", body)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use PUT status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, unknown id -> 404 no-leak.
	req = httptest.NewRequest(http.MethodPut, "/api/system/servers/srv-missing/netbird", strings.NewReader(`{"netbird_enabled":true,"netbird_peer_id":"p1"}`))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("system PUT unknown id status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, wrong method -> 405.
	req = httptest.NewRequest(http.MethodGet, "/api/system/servers/srv-x/netbird", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("system GET status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}

	// system token, non-{id}/netbird path -> 404.
	req = httptest.NewRequest(http.MethodPut, "/api/system/servers/srv-x", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-netbird path status = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleSystemServerNetbirdThreadsPeerManaged (Task 2A): the PUT decodes
// netbird_peer_managed and threads it into SetServerNetbird — a body with
// netbird_peer_managed:true stores managed=true. Seeded managed=false so the write
// is mutation-proven.
func TestHandleSystemServerNetbirdThreadsPeerManaged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// A fake NetBird that 404s everything, so the synchronous reconcile no-ops fast.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fake.Close()
	s, routeStore := newNetbirdDeleteFixture(t, fake.URL)
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_pm", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: false, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPut, "/api/system/servers/srv_pm/netbird", strings.NewReader(`{"netbird_enabled":true,"netbird_peer_id":"peer-pm","netbird_peer_managed":true}`))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	stored, err := routeStore.AIServerByID(ctx, "srv_pm")
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	if !stored.NetbirdPeerManaged {
		t.Fatalf("stored NetbirdPeerManaged = false, want true (PUT must thread netbird_peer_managed)")
	}
	// The DTO also reflects it.
	if !strings.Contains(rec.Body.String(), `"netbird_peer_managed":true`) {
		t.Fatalf("response DTO missing netbird_peer_managed:true: %s", rec.Body.String())
	}
}

// TestSetupKeyGateNonManagedReturns409 (Task 2B): the setup-key endpoint on a
// server whose existing peer is NOT gateway-managed returns 409 with code
// netbird.peer_not_managed (the regenerate gate).
func TestSetupKeyGateNonManagedReturns409(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	// The gate returns before any NetBird call, so a 404-everything fake suffices.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fake.Close()
	s, routeStore := newNetbirdDeleteFixture(t, fake.URL)
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_foreign", Name: "NB", Domain: "nb.local", NetbirdEnabled: true, NetbirdPeerManaged: false, NetbirdPeerID: "peer-foreign", NetbirdGroupID: "g-track", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/portal/servers/srv_foreign/netbird/setup-key", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netbird.peer_not_managed") {
		t.Fatalf("body missing code netbird.peer_not_managed: %s", rec.Body.String())
	}
}

// TestHandlePortalNetbirdEnabled: the module-enabled flag is readable by any
// gateway:use user and returns ONLY the boolean — never the url/token/group.
func TestHandlePortalNetbirdEnabled(t *testing.T) {
	s := newNetbirdEndpointFixture(t, "http://netbird.invalid")

	// Unauthenticated -> 401 (auth gate runs first).
	req := httptest.NewRequest(http.MethodGet, "/api/portal/netbird/enabled", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no bearer status = %d, want 401", rec.Code)
	}

	// gateway:use token -> 200 with exactly {"enabled":true,"module_enabled":true,"netbird_only":false,...}.
	req = httptest.NewRequest(http.MethodGet, "/api/portal/netbird/enabled", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The payload must NOT leak any settings value.
	for _, leak := range []string{"netbird.invalid", "super-secret-token", "gateways", "netbird_url", "netbird_token", "netbird_group"} {
		if strings.Contains(body, leak) {
			t.Fatalf("enabled payload leaked %q: %s", leak, body)
		}
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 6 {
		t.Fatalf("payload has %d keys, want exactly 6 (enabled, module_enabled, netbird_only, manage_policies, effective_policy_scope, deny_by_default): %v", len(raw), raw)
	}
	if enabled, ok := raw["enabled"].(bool); !ok || !enabled {
		t.Fatalf("enabled = %v (ok=%v), want true", raw["enabled"], ok)
	}
	// module_enabled mirrors the raw netbird_enabled checkbox; the fixture is fully
	// configured (url+token) so it agrees with "enabled" here (a dedicated test below
	// exercises the case where they diverge: enabled-but-not-yet-configured).
	if checked, ok := raw["module_enabled"].(bool); !ok || !checked {
		t.Fatalf("module_enabled = %v (ok=%v), want true", raw["module_enabled"], ok)
	}
	// netbird_only reflects the runtime toggle (off in this fixture) — a plain bool.
	if only, ok := raw["netbird_only"].(bool); !ok || only {
		t.Fatalf("netbird_only = %v (ok=%v), want false", raw["netbird_only"], ok)
	}
	// The policy context: the fixture sets only netbird_enabled/url/groups/token, so
	// manage_policies is off, deny_by_default is off, and the effective scope resolves
	// from the default "auto" scope against deny=off -> "selected".
	if mp, ok := raw["manage_policies"].(bool); !ok || mp {
		t.Fatalf("manage_policies = %v (ok=%v), want false", raw["manage_policies"], ok)
	}
	if scope, ok := raw["effective_policy_scope"].(string); !ok || scope != "selected" {
		t.Fatalf("effective_policy_scope = %v (ok=%v), want \"selected\"", raw["effective_policy_scope"], ok)
	}
	if deny, ok := raw["deny_by_default"].(bool); !ok || deny {
		t.Fatalf("deny_by_default = %v (ok=%v), want false", raw["deny_by_default"], ok)
	}
}

// TestHandlePortalNetbirdEnabledCheckedBeforeConfigured proves module_enabled
// diverges from enabled: a module that is turned on but has no url/token yet
// reports enabled=false (NetbirdModuleEnabled — not fully configured/usable)
// while module_enabled=true (the raw checkbox), so the frontend can gate the
// NetBird nav item on the checkbox alone rather than needing url+token first.
func TestHandlePortalNetbirdEnabledCheckedBeforeConfigured(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_u", Email: "u@example.test", DisplayName: "Plain User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_u", UserID: "usr_u", Name: "Use Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, nbUseSecret); err != nil {
		t.Fatalf("CreatePlainToken use: %v", err)
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
	// Enable the checkbox only — no url, no token.
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		NetbirdEnabled: nbBoolPtr(true),
	}); err != nil {
		t.Fatalf("enable netbird (checkbox only): %v", err)
	}
	s := New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc})

	req := httptest.NewRequest(http.MethodGet, "/api/portal/netbird/enabled", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if enabled, ok := raw["enabled"].(bool); !ok || enabled {
		t.Fatalf("enabled = %v (ok=%v), want false (url/token not configured)", raw["enabled"], ok)
	}
	if checked, ok := raw["module_enabled"].(bool); !ok || !checked {
		t.Fatalf("module_enabled = %v (ok=%v), want true (checkbox is on)", raw["module_enabled"], ok)
	}
}

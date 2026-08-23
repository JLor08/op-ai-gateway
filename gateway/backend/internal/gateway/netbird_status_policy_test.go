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
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// newNetbirdStatusPolicyServer builds a *Server whose portal module is configured
// (URL → the fake, sealed token) with policy management on (scope "all",
// deny-by-default on, enforce off). It returns the server + the fake's policy handler
// so a test can control the ListPolicies response for the status endpoint.
func newNetbirdStatusPolicyServer(t *testing.T, policyHandler http.HandlerFunc) *Server {
	t.Helper()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal([]string{"gateway:use", "admin", "system"})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/policies", policyHandler)
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	cipher, err := capture.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	settings := portal.NewMemorySystemSettings()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock(), SystemSettings: settings, Cipher: cipher})
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		NetbirdEnabled:              boolPtr(true),
		NetbirdURL:                  strPtr(fake.URL),
		NetbirdGroups:               &[]string{"gateways"},
		NetbirdToken:                strPtr("nbtok-secret"),
		NetbirdManagePolicies:       boolPtr(true),
		NetbirdPolicyScope:          strPtr("all"),
		NetbirdDenyByDefault:        boolPtr(true),
		NetbirdDenyByDefaultEnforce: boolPtr(false),
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	return New(ServerDeps{Tokens: tokens, Usage: usage.NewRecorder(), Provider: provider.NewMock(), Routes: routeStore, Portal: svc})
}

func fetchNetbirdStatus(t *testing.T, srv *Server) (systemNetbirdStatusDTO, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/status", nil)
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var dto systemNetbirdStatusDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	return dto, rec.Body.String()
}

// TestSystemNetbirdStatusPolicyFields: the status DTO reports the policy settings
// (manage/scope/effective/deny/enforce) from settings AND managed_policy_count +
// default_policy_present/enabled + deny_by_default_drift from a fake policy list.
func TestSystemNetbirdStatusPolicyFields(t *testing.T) {
	srv := newNetbirdStatusPolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "def", "name": "Default", "enabled": true, "rules": []any{}},
			{"id": "p1", "name": "op-gw-access-a", "enabled": true, "rules": []any{}},
			{"id": "p2", "name": "op-gw-access-b", "enabled": true, "rules": []any{}},
			{"id": "px", "name": "some-other-policy", "enabled": true, "rules": []any{}},
		})
	})

	dto, body := fetchNetbirdStatus(t, srv)

	if !dto.ManagePolicies {
		t.Errorf("manage_policies = false, want true")
	}
	if dto.PolicyScope != "all" || dto.EffectivePolicyScope != "all" {
		t.Errorf("scope = %q/%q, want all/all", dto.PolicyScope, dto.EffectivePolicyScope)
	}
	if !dto.DenyByDefault || dto.DenyByDefaultEnforce {
		t.Errorf("deny/enforce = %v/%v, want true/false", dto.DenyByDefault, dto.DenyByDefaultEnforce)
	}
	if dto.ManagedPolicyCount != 2 {
		t.Errorf("managed_policy_count = %d, want 2 (op-gw-access- prefix)", dto.ManagedPolicyCount)
	}
	if !dto.DefaultPolicyPresent || !dto.DefaultPolicyEnabled {
		t.Errorf("default present/enabled = %v/%v, want true/true", dto.DefaultPolicyPresent, dto.DefaultPolicyEnabled)
	}
	if !dto.DenyByDefaultDrift {
		t.Errorf("deny_by_default_drift = false, want true (deny && present && enabled)")
	}
	if strings.Contains(body, "nbtok-secret") {
		t.Errorf("status body leaked the admin token: %s", body)
	}
}

// TestSystemNetbirdStatusNoDriftWhenDefaultDisabled: when the Default policy is
// disabled, deny_by_default_drift is false (deny is on but Default is not enabled).
func TestSystemNetbirdStatusNoDriftWhenDefaultDisabled(t *testing.T) {
	srv := newNetbirdStatusPolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "def", "name": "Default", "enabled": false, "rules": []any{}},
			{"id": "p1", "name": "op-gw-access-a", "enabled": true, "rules": []any{}},
		})
	})

	dto, _ := fetchNetbirdStatus(t, srv)

	if !dto.DefaultPolicyPresent || dto.DefaultPolicyEnabled {
		t.Errorf("default present/enabled = %v/%v, want true/false", dto.DefaultPolicyPresent, dto.DefaultPolicyEnabled)
	}
	if dto.DenyByDefaultDrift {
		t.Errorf("deny_by_default_drift = true, want false (Default disabled → no drift)")
	}
	if dto.ManagedPolicyCount != 1 {
		t.Errorf("managed_policy_count = %d, want 1", dto.ManagedPolicyCount)
	}
}

// TestSystemNetbirdStatusPolicyListError: a ListPolicies error leaves the counts zero
// and never leaks the token; the endpoint still returns 200.
func TestSystemNetbirdStatusPolicyListError(t *testing.T) {
	srv := newNetbirdStatusPolicyServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})

	dto, body := fetchNetbirdStatus(t, srv)

	if dto.ManagedPolicyCount != 0 || dto.DefaultPolicyPresent || dto.DefaultPolicyEnabled || dto.DenyByDefaultDrift {
		t.Errorf("on list error want zero counts/false flags, got count=%d present=%v enabled=%v drift=%v", dto.ManagedPolicyCount, dto.DefaultPolicyPresent, dto.DefaultPolicyEnabled, dto.DenyByDefaultDrift)
	}
	// Settings-sourced fields are still reported (they don't depend on ListPolicies).
	if !dto.ManagePolicies || dto.PolicyScope != "all" {
		t.Errorf("settings fields lost on list error: manage=%v scope=%q", dto.ManagePolicies, dto.PolicyScope)
	}
	if strings.Contains(body, "nbtok-secret") {
		t.Errorf("status body leaked the admin token: %s", body)
	}
}

// newNetbirdStatusPeerNameServer mirrors newNetbirdStatusPolicyServer but ALSO
// registers a fake `/api/peers/{id}` endpoint (peerHandler) and configures a
// selected gateway peer id, so the status endpoint's best-effort GetPeer fires.
// policyHandler may be nil (defaults to an empty policy list).
func newNetbirdStatusPeerNameServer(t *testing.T, gatewayPeerID string, peerHandler http.HandlerFunc, policyHandler http.HandlerFunc) *Server {
	t.Helper()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal([]string{"gateway:use", "admin", "system"})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}

	if policyHandler == nil {
		policyHandler = func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/policies", policyHandler)
	if peerHandler != nil {
		mux.HandleFunc("/api/peers/", peerHandler)
	}
	fake := httptest.NewServer(mux)
	t.Cleanup(fake.Close)

	cipher, err := capture.New(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	settings := portal.NewMemorySystemSettings()
	svc := portal.NewService(portal.ServiceDeps{Users: dir, Tokens: dir, Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock(), SystemSettings: settings, Cipher: cipher})
	req := portal.UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr(fake.URL),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   strPtr("nbtok-secret"),
	}
	if gatewayPeerID != "" {
		req.NetbirdGatewayPeerID = strPtr(gatewayPeerID)
	}
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, req); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	return New(ServerDeps{Tokens: tokens, Usage: usage.NewRecorder(), Provider: provider.NewMock(), Routes: routeStore, Portal: svc})
}

// TestSystemNetbirdStatusGatewayPeerName: the status DTO reports gateway_peer_name
// as the peer's CURRENT (live) NetBird name from the best-effort GetPeer — the
// same call that already sets gateway_peer_connected — and never leaks the token.
func TestSystemNetbirdStatusGatewayPeerName(t *testing.T) {
	srv := newNetbirdStatusPeerNameServer(t, "peer-1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "peer-1", "name": "op-gateway", "connected": true})
	}, nil)

	dto, body := fetchNetbirdStatus(t, srv)

	if dto.GatewayPeerName != "op-gateway" {
		t.Errorf("gateway_peer_name = %q, want op-gateway", dto.GatewayPeerName)
	}
	if strings.Contains(body, "nbtok-secret") {
		t.Errorf("status body leaked the admin token: %s", body)
	}
}

// TestSystemNetbirdStatusGatewayPeerNameEmptyOnError: a GetPeer error leaves
// gateway_peer_name empty (best-effort — never blocks the panel).
func TestSystemNetbirdStatusGatewayPeerNameEmptyOnError(t *testing.T) {
	srv := newNetbirdStatusPeerNameServer(t, "peer-1", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}, nil)

	dto, _ := fetchNetbirdStatus(t, srv)

	if dto.GatewayPeerName != "" {
		t.Errorf("gateway_peer_name = %q, want empty on GetPeer error", dto.GatewayPeerName)
	}
}

// TestSystemNetbirdStatusGatewayPeerNameEmptyOnNoPeer: with no gateway peer id
// selected, gateway_peer_name is empty (GetPeer is never called).
func TestSystemNetbirdStatusGatewayPeerNameEmptyOnNoPeer(t *testing.T) {
	srv := newNetbirdStatusPeerNameServer(t, "", nil, nil)

	dto, _ := fetchNetbirdStatus(t, srv)

	if dto.GatewayPeerName != "" {
		t.Errorf("gateway_peer_name = %q, want empty with no peer id", dto.GatewayPeerName)
	}
}

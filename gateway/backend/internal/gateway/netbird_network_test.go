// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeNetbirdAccountServer is a minimal NetBird admin-API stand-in covering
// exactly the account endpoints the /api/system/netbird/network handler needs:
// GET /api/accounts (returns a single account seeded with `initial` settings)
// and PUT /api/accounts/{id} (captures the PUT body's "settings" map into
// *putBody so a test can assert what was written). Any request's bearer token
// (the "Authorization: Token <t>" header) is captured into *gotToken so a test
// can prove which credential (stored vs. an override) actually reached NetBird.
func fakeNetbirdAccountServer(t *testing.T, initial map[string]any, putBody *map[string]any, gotToken *string) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	settings := initial
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if gotToken != nil {
			*gotToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Token ")
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "acc-1", "settings": settings},
		})
	})
	mux.HandleFunc("/api/accounts/acc-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Settings map[string]any `json:"settings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		if putBody != nil {
			*putBody = body.Settings
		}
		settings = body.Settings
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestHandleSystemNetbirdNetworkRequiresSystemScope: the endpoint is
// system-scoped — a gateway:use-only token is rejected for both GET and PUT.
func TestHandleSystemNetbirdNetworkRequiresSystemScope(t *testing.T) {
	fake := fakeNetbirdAccountServer(t, map[string]any{"dns_domain": "old.io"}, nil, nil)
	s := newNetbirdEndpointFixture(t, fake.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/network", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use GET status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPut, "/api/system/netbird/network", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use PUT status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleSystemNetbirdNetworkGet: a system token GETs the account's network
// settings; the response carries exactly the four DTO fields and never a token.
func TestHandleSystemNetbirdNetworkGet(t *testing.T) {
	fake := fakeNetbirdAccountServer(t, map[string]any{
		"dns_domain":           "gw.example.test",
		"network_range":        "100.64.0.0/10",
		"network_range_v6":     "fd00::/48",
		"ipv6_enabled_groups":  []any{"g1", "g2"},
		"peer_login_expiry_ok": true, // an unrelated setting, must never leak into the DTO
	}, nil, nil)
	s := newNetbirdEndpointFixture(t, fake.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/system/netbird/network", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, leak := range []string{"super-secret-token", "netbird_token", "\"token\""} {
		if strings.Contains(body, leak) {
			t.Fatalf("network GET response leaked %q: %s", leak, body)
		}
	}
	var dto struct {
		DNSDomain         string   `json:"dns_domain"`
		NetworkRange      string   `json:"network_range"`
		NetworkRangeV6    string   `json:"network_range_v6"`
		IPv6EnabledGroups []string `json:"ipv6_enabled_groups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.DNSDomain != "gw.example.test" {
		t.Fatalf("dns_domain = %q, want gw.example.test", dto.DNSDomain)
	}
	if dto.NetworkRange != "100.64.0.0/10" {
		t.Fatalf("network_range = %q, want 100.64.0.0/10", dto.NetworkRange)
	}
	if dto.NetworkRangeV6 != "fd00::/48" {
		t.Fatalf("network_range_v6 = %q, want fd00::/48", dto.NetworkRangeV6)
	}
	if len(dto.IPv6EnabledGroups) != 2 || dto.IPv6EnabledGroups[0] != "g1" || dto.IPv6EnabledGroups[1] != "g2" {
		t.Fatalf("ipv6_enabled_groups = %v, want [g1 g2]", dto.IPv6EnabledGroups)
	}
}

// TestHandleSystemNetbirdNetworkPut: a system token PUTs new network settings;
// the fake NetBird account is updated and the response reflects the write.
func TestHandleSystemNetbirdNetworkPut(t *testing.T) {
	var putBody map[string]any
	fake := fakeNetbirdAccountServer(t, map[string]any{"dns_domain": "old.io"}, &putBody, nil)
	s := newNetbirdEndpointFixture(t, fake.URL)

	reqBody := `{"dns_domain":"new.io","network_range":"100.64.0.0/10","network_range_v6":"","ipv6_enabled_groups":["g1"]}`
	req := httptest.NewRequest(http.MethodPut, "/api/system/netbird/network", strings.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if putBody == nil {
		t.Fatalf("PUT /api/accounts/acc-1 was never called")
	}
	if v, _ := putBody["dns_domain"].(string); v != "new.io" {
		t.Fatalf("settings[dns_domain] = %v, want new.io", putBody["dns_domain"])
	}
	body := rec.Body.String()
	if strings.Contains(body, "super-secret-token") {
		t.Fatalf("network PUT response leaked the admin token: %s", body)
	}
	var dto struct {
		DNSDomain string `json:"dns_domain"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dto.DNSDomain != "new.io" {
		t.Fatalf("response dns_domain = %q, want new.io", dto.DNSDomain)
	}
}

// TestHandleSystemNetbirdNetworkMethodNotAllowed: a method other than GET/PUT is
// rejected with 405.
func TestHandleSystemNetbirdNetworkMethodNotAllowed(t *testing.T) {
	fake := fakeNetbirdAccountServer(t, map[string]any{"dns_domain": "old.io"}, nil, nil)
	s := newNetbirdEndpointFixture(t, fake.URL)

	req := httptest.NewRequest(http.MethodDelete, "/api/system/netbird/network", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleSystemNetbirdTestUsesOverrideToken: with NO body, the test endpoint
// uses the STORED token (which the fake rejects here, simulating a wrong stored
// credential) and reports ok:false; with a body carrying an override token the
// fake accepts, it reports ok:true — proving the override token is what actually
// reaches NetBird, not the stored one.
func TestHandleSystemNetbirdTestUsesOverrideToken(t *testing.T) {
	const overrideToken = "override-secret"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/groups" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Token "+overrideToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer fake.Close()
	// newNetbirdEndpointFixture stores the token "super-secret-token", which the
	// fake above rejects (401) — so a no-body test must report ok:false.
	s := newNetbirdEndpointFixture(t, fake.URL)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/test", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-body status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var res struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OK {
		t.Fatalf("no-body ok = true, want false (the stored token must be rejected by the fake)")
	}

	// With an override token the fake accepts, the result must be ok:true.
	req = httptest.NewRequest(http.MethodPost, "/api/system/netbird/test", strings.NewReader(`{"token":"`+overrideToken+`"}`))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("override-token status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK {
		t.Fatalf("override-token ok = %v, want true (body=%s)", res.OK, rec.Body.String())
	}
	// The override secret must never be echoed back.
	if strings.Contains(rec.Body.String(), overrideToken) {
		t.Fatalf("response leaked the override token: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdTestUsesOverrideURL: the stored NetBird URL is
// unreachable (connection refused), so a no-body test reports ok:false; a body
// overriding the url to a reachable fake reports ok:true — proving the override
// URL is what actually gets pinged.
func TestHandleSystemNetbirdTestUsesOverrideURL(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/groups" {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer fake.Close()
	// Port 1 is reserved and nothing listens there -> immediate connection refused.
	s := newNetbirdEndpointFixture(t, "http://127.0.0.1:1")

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/test", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-body status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var res struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.OK {
		t.Fatalf("no-body ok = true, want false (the stored URL is unreachable)")
	}

	req = httptest.NewRequest(http.MethodPost, "/api/system/netbird/test", strings.NewReader(`{"url":"`+fake.URL+`"}`))
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("override-url status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !res.OK {
		t.Fatalf("override-url ok = %v, want true (body=%s)", res.OK, rec.Body.String())
	}
}

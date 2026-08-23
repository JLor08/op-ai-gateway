// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeNetbirdAccount is a minimal NetBird admin-API stand-in covering exactly
// the account endpoints NetbirdNetwork/SetNetbirdNetwork need: GET /api/accounts
// (returns a single account seeded with `initial` settings) and PUT
// /api/accounts/{id} (captures the PUT body's "settings" map into *putBody so a
// test can assert the whole merged map that was written, including any foreign
// keys the read-modify-write must preserve).
func fakeNetbirdAccount(t *testing.T, initial map[string]any, putBody *map[string]any) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	settings := initial
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
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

// newNetbirdNetworkTestService builds a Service with the NetBird module enabled
// and pointed at netbirdURL, wiring onDomainChanged as the OnNetbirdDomainChanged
// hook so a test can observe whether SetNetbirdNetwork fired it.
func newNetbirdNetworkTestService(t *testing.T, netbirdURL string, onDomainChanged func()) *Service {
	t.Helper()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	routeStore := routing.NewMemoryStore()
	svc := NewService(ServiceDeps{
		Users:                  dir,
		Routes:                 routeStore,
		SystemSettings:         NewMemorySystemSettings(),
		Cipher:                 newTestCipher(t),
		Clock:                  func() time.Time { return now },
		OnNetbirdDomainChanged: onDomainChanged,
	})
	if _, err := svc.UpdateSystemSettings(context.Background(), systemToken(), UpdateSystemSettingsRequest{
		NetbirdEnabled: boolPtr(true),
		NetbirdURL:     strPtr(netbirdURL),
		NetbirdGroups:  &[]string{"gateways"},
		NetbirdToken:   strPtr("nbtok"),
	}); err != nil {
		t.Fatalf("configure netbird: %v", err)
	}
	return svc
}

// newNetbirdNetworkTestServiceDisabled builds a Service with the NetBird module
// left off (no url/token configured).
func newNetbirdNetworkTestServiceDisabled(t *testing.T) *Service {
	t.Helper()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	dir := NewMemoryDirectory(auth.NewTokenStore())
	routeStore := routing.NewMemoryStore()
	return NewService(ServiceDeps{
		Users:          dir,
		Routes:         routeStore,
		SystemSettings: NewMemorySystemSettings(),
		Cipher:         newTestCipher(t),
		Clock:          func() time.Time { return now },
	})
}

// TestSetNetbirdNetworkReadModifyWritePreservesForeignKeys: the read-modify-write
// preserves every account setting SetNetbirdNetwork does not manage (here
// peer_login_expiration_enabled), writes the four managed fields, returns the
// new state, and fires the domain-change hook exactly once (dns_domain changed).
func TestSetNetbirdNetworkReadModifyWritePreservesForeignKeys(t *testing.T) {
	var putBody map[string]any
	srv := fakeNetbirdAccount(t, map[string]any{
		"dns_domain":                    "old.io",
		"network_range":                 "100.64.0.0/10",
		"peer_login_expiration_enabled": true,
	}, &putBody)

	var hookCalls int
	svc := newNetbirdNetworkTestService(t, srv.URL, func() { hookCalls++ })

	dto, err := svc.SetNetbirdNetwork(context.Background(), systemToken(), NetbirdNetworkDTO{
		DNSDomain:         "new.io",
		NetworkRange:      "100.64.0.0/10",
		NetworkRangeV6:    "fd00::/48",
		IPv6EnabledGroups: []string{"g1"},
	})
	if err != nil {
		t.Fatalf("SetNetbirdNetwork = %v, want nil", err)
	}
	if dto.DNSDomain != "new.io" {
		t.Fatalf("DNSDomain = %q, want new.io", dto.DNSDomain)
	}
	// The RETURNED DTO must echo the ipv6 groups just set (regression: the response
	// was rebuilt from the patched settings map where the value is a Go []string,
	// which netbirdSettingsStringSlice's .([]any) assertion dropped to empty — so
	// the UI's IPv6-groups field went blank right after save).
	if !reflect.DeepEqual(dto.IPv6EnabledGroups, []string{"g1"}) {
		t.Fatalf("returned IPv6EnabledGroups = %#v, want [g1]", dto.IPv6EnabledGroups)
	}
	if putBody == nil {
		t.Fatalf("PUT /api/accounts/acc-1 was never called")
	}
	if v, _ := putBody["peer_login_expiration_enabled"].(bool); !v {
		t.Fatalf("peer_login_expiration_enabled = %v, want true (foreign key must be preserved)", putBody["peer_login_expiration_enabled"])
	}
	if v, _ := putBody["dns_domain"].(string); v != "new.io" {
		t.Fatalf("settings[dns_domain] = %v, want new.io", putBody["dns_domain"])
	}
	if v, _ := putBody["network_range_v6"].(string); v != "fd00::/48" {
		t.Fatalf("settings[network_range_v6] = %v, want fd00::/48", putBody["network_range_v6"])
	}
	if got, want := putBody["ipv6_enabled_groups"], []any{"g1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("settings[ipv6_enabled_groups] = %#v, want %#v", got, want)
	}
	if hookCalls != 1 {
		t.Fatalf("domain-change hook fired %d times, want 1", hookCalls)
	}
}

// TestSetNetbirdNetworkNoDomainChangeSkipsHook: when dns_domain is unchanged the
// domain-change hook must NOT fire.
func TestSetNetbirdNetworkNoDomainChangeSkipsHook(t *testing.T) {
	srv := fakeNetbirdAccount(t, map[string]any{"dns_domain": "same.io"}, nil)
	var hookCalls int
	svc := newNetbirdNetworkTestService(t, srv.URL, func() { hookCalls++ })

	if _, err := svc.SetNetbirdNetwork(context.Background(), systemToken(), NetbirdNetworkDTO{DNSDomain: "same.io"}); err != nil {
		t.Fatalf("SetNetbirdNetwork = %v, want nil", err)
	}
	if hookCalls != 0 {
		t.Fatalf("domain-change hook fired %d times, want 0 (dns_domain unchanged)", hookCalls)
	}
}

// TestSetNetbirdNetworkInvalidCIDR: an unparseable / wrong-family CIDR is
// rejected before any NetBird call is made (no server needed).
func TestSetNetbirdNetworkInvalidCIDR(t *testing.T) {
	svc := newNetbirdNetworkTestService(t, "http://127.0.0.1:1", nil)

	if _, err := svc.SetNetbirdNetwork(context.Background(), systemToken(), NetbirdNetworkDTO{NetworkRange: "not-a-cidr"}); err == nil || !errors.Is(err, ErrNetbirdNetworkRangeInvalid) {
		t.Fatalf("SetNetbirdNetwork(bad CIDR) = %v, want ErrNetbirdNetworkRangeInvalid", err)
	}
	// A v6 value in the v4 field must also be rejected.
	if _, err := svc.SetNetbirdNetwork(context.Background(), systemToken(), NetbirdNetworkDTO{NetworkRange: "fd00::/48"}); err == nil {
		t.Fatalf("SetNetbirdNetwork(v6 in v4 field) = nil, want an error")
	}
}

// TestNetbirdNetworkModuleDisabled: with the module not configured (no
// url/token), NetbirdNetwork returns an error and never reaches NetBird.
func TestNetbirdNetworkModuleDisabled(t *testing.T) {
	svc := newNetbirdNetworkTestServiceDisabled(t)
	if _, err := svc.NetbirdNetwork(context.Background()); err == nil {
		t.Fatalf("NetbirdNetwork(module disabled) = nil, want an error")
	}
}

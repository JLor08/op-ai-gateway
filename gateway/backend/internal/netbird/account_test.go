// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package netbird

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetAccountReturnsFirstAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/accounts" {
			t.Fatalf("path = %s, want /api/accounts", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Token secret-xyz" {
			t.Fatalf("Authorization = %q, want %q", got, "Token secret-xyz")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"acc-1","domain":"example.io","settings":{"dns_domain":"nb.example.io","network_range":"100.64.0.0/10","network_range_v6":"fd00:1234::/48","ipv6_enabled_groups":["g1","g2"],"peer_login_expiration_enabled":true}}]`))
	}))
	defer srv.Close()

	acct, err := GetAccount(context.Background(), Config{URL: srv.URL, Token: "secret-xyz"}, time.Second)
	if err != nil {
		t.Fatalf("GetAccount() error = %v", err)
	}
	if acct.ID != "acc-1" {
		t.Fatalf("acct.ID = %q, want %q", acct.ID, "acc-1")
	}
	if got := acct.Settings["dns_domain"]; got != "nb.example.io" {
		t.Fatalf("acct.Settings[dns_domain] = %v, want %q", got, "nb.example.io")
	}
	// Foreign key: proves the raw map preserves unmanaged keys.
	if got := acct.Settings["peer_login_expiration_enabled"]; got != true {
		t.Fatalf("acct.Settings[peer_login_expiration_enabled] = %v, want true", got)
	}
}

func TestGetAccountEmptyListErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	_, err := GetAccount(context.Background(), Config{URL: srv.URL, Token: "t"}, time.Second)
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("GetAccount() error = %v, want ErrNoAccount", err)
	}
}

func TestUpdateAccountSettingsPutsFullSettings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Fatalf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/api/accounts/acc-1" {
			t.Fatalf("path = %s, want /api/accounts/acc-1", r.URL.Path)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		settings, ok := body["settings"].(map[string]any)
		if !ok {
			t.Fatalf("body[settings] = %v (%T), want map[string]any", body["settings"], body["settings"])
		}
		if got := settings["dns_domain"]; got != "new.io" {
			t.Fatalf("settings[dns_domain] = %v, want %q", got, "new.io")
		}
		if got := settings["peer_login_expiration_enabled"]; got != true {
			t.Fatalf("settings[peer_login_expiration_enabled] = %v, want true", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	err := UpdateAccountSettings(context.Background(), Config{URL: srv.URL, Token: "t"}, time.Second, "acc-1", map[string]any{
		"dns_domain":                    "new.io",
		"peer_login_expiration_enabled": true,
	})
	if err != nil {
		t.Fatalf("UpdateAccountSettings() error = %v", err)
	}
}

func TestUpdateAccountSettingsAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	err := UpdateAccountSettings(context.Background(), Config{URL: srv.URL, Token: "t"}, time.Second, "acc-1", map[string]any{"dns_domain": "x"})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("errors.Is(err, ErrAuth) = false, err = %v", err)
	}
}

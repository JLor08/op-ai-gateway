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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newEnrollSidecarFixture mirrors newGatewayKeyFixture but threads a NetbirdKeyFile
// into the Portal Service so the enroll-sidecar endpoint can write the minted key.
func newEnrollSidecarFixture(t *testing.T, fakeURL string, enabled bool, keyFile string) *Server {
	t.Helper()
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_s", Email: "s@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_u", UserID: "usr_s", Name: "Use Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, nbUseSecret); err != nil {
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
		NetbirdKeyFile: keyFile,
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

// TestHandleSystemNetbirdEnrollSidecarSuccess: system POST -> 200 with the minted
// key + `netbird up` command; the key is written to the shared file; the admin
// token never appears in the response body.
func TestHandleSystemNetbirdEnrollSidecarSuccess(t *testing.T) {
	fake, _ := newGatewayKeyFakeNetbird(t)
	keyFile := filepath.Join(t.TempDir(), "netbird-setup-key")
	s := newEnrollSidecarFixture(t, fake.URL, true, keyFile)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/enroll-sidecar", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
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
		t.Fatalf("setup_key = %q, want the minted key", out.SetupKey)
	}
	if want := "netbird up --management-url " + fake.URL + " --setup-key nbkey-secret-value"; out.NetbirdSetupCommand != want {
		t.Fatalf("netbird_setup_command = %q, want %q", out.NetbirdSetupCommand, want)
	}
	// The key was written to the shared file.
	data, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatalf("read key file: %v", err)
	}
	if string(data) != "nbkey-secret-value" {
		t.Fatalf("key file content = %q, want the minted key", string(data))
	}
}

// TestHandleSystemNetbirdEnrollSidecarNoKeyFile: with no key file configured the
// endpoint maps ErrNetbirdKeyFileNotConfigured to 409 netbird.key_file_not_configured.
func TestHandleSystemNetbirdEnrollSidecarNoKeyFile(t *testing.T) {
	s := newEnrollSidecarFixture(t, "http://netbird.invalid", true, "")

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/enroll-sidecar", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body=%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "netbird.key_file_not_configured") {
		t.Fatalf("body missing code netbird.key_file_not_configured: %s", rec.Body.String())
	}
}

// TestHandleSystemNetbirdEnrollSidecarScope: a gateway:use token is rejected 403,
// and the wrong method is 405 (system-scoped, POST-only).
func TestHandleSystemNetbirdEnrollSidecarScope(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "netbird-setup-key")
	s := newEnrollSidecarFixture(t, "http://netbird.invalid", true, keyFile)

	req := httptest.NewRequest(http.MethodPost, "/api/system/netbird/enroll-sidecar", nil)
	req.Header.Set("Authorization", "Bearer "+nbUseSecret)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("gateway:use POST status = %d, want 403 (body=%s)", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/system/netbird/enroll-sidecar", nil)
	req.Header.Set("Authorization", "Bearer "+nbSystemSecret)
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("system GET status = %d, want 405 (body=%s)", rec.Code, rec.Body.String())
	}
	// The key file must NOT have been written by a rejected request.
	if _, statErr := os.Stat(keyFile); !os.IsNotExist(statErr) {
		t.Fatalf("key file should not exist after rejected requests, stat err = %v", statErr)
	}
}

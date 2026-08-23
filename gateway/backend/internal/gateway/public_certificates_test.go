// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

const (
	pubCertSystemSecret = "pub-system-secret"
	pubCertUseSecret    = "pub-use-secret"
)

// publicCertExportServer builds a *Server whose real portal.Service manages the
// public domain `managedDomain` and holds a kind=public certificate row for
// `rowDomain` with rowKind. A system-scope and a gateway:use token are seeded so
// the scope gate can be exercised.
func publicCertExportServer(t *testing.T, manage bool, managedDomain, rowDomain, rowKind string) *Server {
	t.Helper()
	now := time.Date(2026, time.August, 19, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_s", Email: "s@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_s", UserID: "usr_s", Name: "System", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, pubCertSystemSecret); err != nil {
		t.Fatalf("seed system token: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_u", UserID: "usr_s", Name: "Use", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, pubCertUseSecret); err != nil {
		t.Fatalf("seed use token: %v", err)
	}
	routeStore := routing.NewMemoryStore()
	settings := portal.NewMemorySystemSettings()
	svc := portal.NewService(portal.ServiceDeps{
		Users: dir, Tokens: dir, Routes: routeStore, SystemSettings: settings,
		SettingsVolatile: true, Clock: func() time.Time { return now },
	})
	enabled := true
	mode := portal.IssuerModeSelfSigned
	if _, err := svc.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:            &enabled,
		CertIssuerMode:         &mode,
		CertManagePublicDomain: &manage,
		CertPublicDomains:      &[]string{managedDomain},
	}); err != nil {
		t.Fatalf("seed cert settings: %v", err)
	}
	if rowDomain != "" {
		if err := routeStore.UpsertCertificate(context.Background(), routing.Certificate{
			Domain: rowDomain, Kind: rowKind, Status: "active",
			FullchainPEM: "-----BEGIN CERTIFICATE-----\nPUBLICLEAF\n-----END CERTIFICATE-----\n",
			KeySealed:    "plain:PUBLIC-KEY", Fingerprint: "fp-" + rowDomain,
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour),
			IssuedAt: now, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("seed cert row: %v", err)
		}
	}
	return New(ServerDeps{Tokens: tokens, Routes: routing.NewMemoryStore(), Portal: svc})
}

func pubCertGet(t *testing.T, s *Server, path, secret string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	return rec
}

func TestPublicCertificateExportRoutesRequireSystemScope(t *testing.T) {
	s := publicCertExportServer(t, true, "pub.example.test", "pub.example.test", "public")
	for _, path := range []string{
		"/api/system/certificates/public/pub.example.test/bundle",
		"/api/system/certificates/public/pub.example.test/key",
	} {
		rec := pubCertGet(t, s, path, pubCertUseSecret)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s with gateway:use = %d, want 403", path, rec.Code)
		}
	}
}

func TestPublicCertificateBundleAndKeyEndpoints(t *testing.T) {
	s := publicCertExportServer(t, true, "pub.example.test", "pub.example.test", "public")

	bundle := pubCertGet(t, s, "/api/system/certificates/public/pub.example.test/bundle", pubCertSystemSecret)
	if bundle.Code != http.StatusOK {
		t.Fatalf("bundle status = %d, want 200 (body=%s)", bundle.Code, bundle.Body.String())
	}
	if !strings.Contains(bundle.Body.String(), "BEGIN CERTIFICATE") || strings.Contains(bundle.Body.String(), "PRIVATE KEY") {
		t.Fatalf("bundle body wrong: %q", bundle.Body.String())
	}

	key := pubCertGet(t, s, "/api/system/certificates/public/pub.example.test/key", pubCertSystemSecret)
	if key.Code != http.StatusOK {
		t.Fatalf("key status = %d, want 200 (body=%s)", key.Code, key.Body.String())
	}
	if key.Body.String() != "PUBLIC-KEY" {
		t.Fatalf("key body = %q, want the opened plaintext key", key.Body.String())
	}
	if key.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("key Cache-Control = %q, want no-store", key.Header().Get("Cache-Control"))
	}
}

func TestPublicCertificateExportMaps409And404Exactly(t *testing.T) {
	// Management off -> 409 not-managed.
	off := publicCertExportServer(t, false, "pub.example.test", "pub.example.test", "public")
	rec := pubCertGet(t, off, "/api/system/certificates/public/pub.example.test/key", pubCertSystemSecret)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "certificate.public_not_managed") {
		t.Fatalf("management off = %d body=%s, want 409 public_not_managed", rec.Code, rec.Body.String())
	}

	// Managed name, but the stored row is kind=gateway (a collision): 404, never the
	// mesh key.
	collide := publicCertExportServer(t, true, "pub.example.test", "pub.example.test", "gateway")
	key := pubCertGet(t, collide, "/api/system/certificates/public/pub.example.test/key", pubCertSystemSecret)
	if key.Code != http.StatusNotFound || !strings.Contains(key.Body.String(), "certificate.not_found") {
		t.Fatalf("kind=gateway collision key = %d body=%s, want 404 not_found (no key leak)", key.Code, key.Body.String())
	}
	if strings.Contains(key.Body.String(), "PUBLIC-KEY") {
		t.Fatal("kind=gateway collision leaked key material")
	}
}

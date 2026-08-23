// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/account"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/theme"
	"op-ai-gateway/internal/usage"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newThemeAssetTestServer builds a Server backed by a real theme.Registry
// loaded from a temp dir containing one external theme, "acme" (favicon.png
// present, no logo), and activates it as the system theme. It mirrors
// newAuthTestServer (auth_test.go) but additionally wires portal.ServiceDeps
// .Themes so PublicThemeView/ExternalThemeAsset have a real external theme
// to resolve.
func newThemeAssetTestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	themeDir := filepath.Join(dir, "acme")
	if err := os.MkdirAll(themeDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", themeDir, err)
	}
	if err := os.WriteFile(filepath.Join(themeDir, "theme.json"), []byte(`{"name":"ACME","productName":"ACME Gateway","brand":{"type":"text","text":"ACME","title":"AI Gateway"},"light":{"brandPrimary":"#123456"}}`), 0o644); err != nil {
		t.Fatalf("write theme.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(themeDir, "favicon.png"), []byte("\x89PNG\r\n\x1a\nfake-favicon-bytes"), 0o644); err != nil {
		t.Fatalf("write favicon.png: %v", err)
	}

	// A second, unrelated theme carrying an SVG logo -- the operator-supplied
	// file type the final-review Fix 3 hardening (CSP + nosniff response
	// headers) specifically targets, since an SVG can embed a <script> that
	// the raw asset endpoint (unlike the frontend's <img> render path) would
	// otherwise execute if navigated to directly.
	svgThemeDir := filepath.Join(dir, "svglogo")
	if err := os.MkdirAll(svgThemeDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", svgThemeDir, err)
	}
	if err := os.WriteFile(filepath.Join(svgThemeDir, "theme.json"), []byte(`{"name":"SVGLogo"}`), 0o644); err != nil {
		t.Fatalf("write theme.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(svgThemeDir, "logo.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), 0o644); err != nil {
		t.Fatalf("write logo.svg: %v", err)
	}

	reg, err := theme.Load(dir)
	if err != nil {
		t.Fatalf("theme.Load: %v", err)
	}
	if _, ok := reg.Get("acme"); !ok {
		t.Fatal("theme.Load did not load the acme fixture theme")
	}
	if _, ok := reg.Get("svglogo"); !ok {
		t.Fatal("theme.Load did not load the svglogo fixture theme")
	}

	ts := auth.NewTokenStore()
	mdir := portal.NewMemoryDirectory(ts)
	acct := account.NewService(account.Deps{Users: mdir, Sessions: mdir, SetPasswordTokens: mdir, SettingsVolatile: true}, account.Config{
		IdleTTL: time.Hour, MaxTTL: 24 * time.Hour, InviteTTL: 72 * time.Hour, DefaultLanguage: "de",
	})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, time.Now().UTC())
	svc := portal.NewService(portal.ServiceDeps{
		Users: mdir, Tokens: mdir, Usage: recorder, Routes: routeStore, Groups: mdir, Projects: mdir,
		SystemSettings: portal.NewMemorySystemSettings(), UIPrefs: portal.NewMemoryUIPreferences(), Themes: reg,
	})
	acmeID := "acme"
	if _, err := svc.UpdateSystemSettings(t.Context(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{Theme: &acmeID}); err != nil {
		t.Fatalf("activate acme theme: %v", err)
	}
	srv := New(ServerDeps{
		Tokens: ts, Usage: recorder, Portal: svc, Account: acct, Routes: routeStore,
		CookieSecure: false, SessionMaxAge: 24 * time.Hour, PublicURL: "http://localhost:8080",
	})
	return srv
}

// TestSystemThemePublicExternalActive confirms GET /api/system/theme reports
// source "external" with the loaded theme's data when the active theme is
// an externally loaded one -- the pre-auth login screen needs this to theme
// itself before any session exists.
func TestSystemThemePublicExternalActive(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/theme", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/system/theme = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	var body portal.ThemePublicView
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("Unmarshal returned %v (body=%s)", err, rec.Body.String())
	}
	if body.Theme != "acme" {
		t.Fatalf("theme = %q, want %q", body.Theme, "acme")
	}
	if body.Source != "external" {
		t.Fatalf("source = %q, want %q", body.Source, "external")
	}
	if body.Data == nil {
		t.Fatal("data = nil, want the loaded acme theme")
	}
	if body.Data.ID != "acme" || body.Data.Name != "ACME" {
		t.Fatalf("data = %+v, want id=acme name=ACME", body.Data)
	}
	if !body.Data.HasFavicon {
		t.Fatal("data.HasFavicon = false, want true (favicon.png present)")
	}
	if body.Data.HasLogo {
		t.Fatal("data.HasLogo = true, want false (no logo file)")
	}
	if body.Data.Light["brandPrimary"] != "#123456" {
		t.Fatalf("data.Light[brandPrimary] = %q, want #123456", body.Data.Light["brandPrimary"])
	}
	// The absolute on-disk path must never leak onto the wire.
	if raw := rec.Body.String(); strings.Contains(raw, "FaviconPath") || strings.Contains(raw, "faviconPath") {
		t.Fatalf("response leaks a favicon path: %s", raw)
	}
}

// TestSystemThemeAssetFavicon200 confirms the favicon endpoint serves the
// loaded theme's favicon bytes with an image/png content-type.
func TestSystemThemeAssetFavicon200(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/acme/favicon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/system/themes/acme/favicon = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("favicon body is empty")
	}
	// Defense in depth (final-review Fix 3): every served theme asset --
	// not just SVG logos -- carries a restrictive CSP + nosniff, so an
	// operator-supplied file can never execute script if someone navigates
	// directly to this endpoint.
	wantCSP := "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	if csp := rec.Header().Get("Content-Security-Policy"); csp != wantCSP {
		t.Fatalf("Content-Security-Policy = %q, want %q", csp, wantCSP)
	}
	if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want %q", xcto, "nosniff")
	}
}

// TestSystemThemeAssetSVGLogoHardenedHeaders confirms an SVG logo -- the
// asset type that can embed a <script>, per final-review Fix 3 -- is served
// with a restrictive Content-Security-Policy and X-Content-Type-Options:
// nosniff, neutralizing any embedded script for a caller that navigates
// directly to the endpoint rather than rendering it through an <img> tag.
func TestSystemThemeAssetSVGLogoHardenedHeaders(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/svglogo/logo", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/system/themes/svglogo/logo = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Fatalf("Content-Type = %q, want image/svg+xml", ct)
	}
	wantCSP := "default-src 'none'; style-src 'unsafe-inline'; sandbox"
	if csp := rec.Header().Get("Content-Security-Policy"); csp != wantCSP {
		t.Fatalf("Content-Security-Policy = %q, want %q", csp, wantCSP)
	}
	if xcto := rec.Header().Get("X-Content-Type-Options"); xcto != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want %q", xcto, "nosniff")
	}
}

// TestSystemThemeAssetLogoMissing404 confirms the logo endpoint 404s for a
// theme that has no logo file, rather than serving an empty/wrong body.
func TestSystemThemeAssetLogoMissing404(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/acme/logo", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/system/themes/acme/logo = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSystemThemeAssetUnknownID404 confirms an id that never loaded (no such
// theme directory) 404s rather than erroring.
func TestSystemThemeAssetUnknownID404(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/does-not-exist/favicon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/system/themes/does-not-exist/favicon = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSystemThemeAssetUnknownKind404 confirms a kind other than
// favicon/logo 404s.
func TestSystemThemeAssetUnknownKind404(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/acme/theme.json", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /api/system/themes/acme/theme.json = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSystemThemeAssetTraversalRejected404 confirms a path-traversal id
// (encoded so it survives to the handler as literal ".." path segments,
// rather than being cleaned/redirected earlier by the mux) 404s: the id is
// resolved only against the loaded registry, which the traversal segments
// can never match, so the request can never escape the themes directory.
func TestSystemThemeAssetTraversalRejected404(t *testing.T) {
	srv := newThemeAssetTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/system/themes/..%2f..%2fetc/favicon", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET traversal path = %d, want 404 (body=%s)", rec.Code, rec.Body.String())
	}
}

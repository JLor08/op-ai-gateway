// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func writeManifest(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","go_version":"go1.26","built_at":"2026-08-07T00:00:00Z","binaries":[{"os":"windows","arch":"amd64","filename":"server-agent-windows-amd64.exe","size":10,"sha256":"ab"}]}`)
	m, err := loadAgentManifest(dir)
	if err != nil {
		t.Fatalf("loadAgentManifest: %v", err)
	}
	if m.AgentVersion != "0.1.0" || len(m.Binaries) != 1 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	b, ok := m.find("windows-amd64")
	if !ok || b.Filename != "server-agent-windows-amd64.exe" {
		t.Fatalf("find windows-amd64: %+v ok=%v", b, ok)
	}
	if _, ok := m.find("linux-amd64"); ok {
		t.Fatal("find should miss linux-amd64")
	}
}

func TestLoadAgentManifestUnavailable(t *testing.T) {
	if _, err := loadAgentManifest(""); err != errAgentBinariesUnavailable {
		t.Fatalf("empty dir: want errAgentBinariesUnavailable, got %v", err)
	}
	if _, err := loadAgentManifest(t.TempDir()); err != errAgentBinariesUnavailable {
		t.Fatalf("missing file: want errAgentBinariesUnavailable, got %v", err)
	}
}

func TestAgentTargetAllowed(t *testing.T) {
	for _, ok := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64", "windows-arm64"} {
		if !agentTargetAllowed(ok) {
			t.Errorf("target %q should be allowed", ok)
		}
	}
	for _, bad := range []string{"", "linux", "linux-386", "../etc-passwd", "windows-amd64.exe", "plan9-amd64"} {
		if agentTargetAllowed(bad) {
			t.Errorf("target %q should be rejected", bad)
		}
	}
}

func TestBuildMeshDownloadBase(t *testing.T) {
	// Off unless downloadOnly AND listener active.
	if got := buildMeshDownloadBase(false, true, "100.0.0.1:8081", "gw.netbird.selfhosted"); got != "" {
		t.Errorf("downloadOnly=false: want empty, got %q", got)
	}
	if got := buildMeshDownloadBase(true, false, "100.0.0.1:8081", "gw.netbird.selfhosted"); got != "" {
		t.Errorf("listener inactive: want empty, got %q", got)
	}
	// DNS name preferred, port taken from the listener addr.
	if got := buildMeshDownloadBase(true, true, "100.0.0.1:8081", "gw.netbird.selfhosted"); got != "http://gw.netbird.selfhosted:8081" {
		t.Errorf("dns base = %q", got)
	}
	// Fallback to the listener IP:port when DNS is empty.
	if got := buildMeshDownloadBase(true, true, "100.0.0.1:8081", ""); got != "http://100.0.0.1:8081" {
		t.Errorf("fallback base = %q", got)
	}
	// A malformed listener address (no port) yields empty, never a garbled URL.
	if got := buildMeshDownloadBase(true, true, "not-a-host-port", ""); got != "" {
		t.Errorf("malformed listener addr: want empty, got %q", got)
	}
}

// newAgentBinTestServer builds a *Server with a system-scoped dev bearer token
// ("dev-secret", scopes gateway:use+admin+system) for the portal endpoints, a
// valid agent token bound to "mock-host-qwen" ("valid-agent-secret") for the
// agent-token endpoint, agentBinaryDir set to dir, and a real MemorySystemSettings
// store (netbird_agent_download_only left unset/false, agent listener inactive).
// Mirrors newAgentGateTestServer in agent_listener_test.go.
func newAgentBinTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	return newAgentBinTestServerWithDownloadOnly(t, dir, false)
}

// newAgentBinTestServerWithDownloadOnly is newAgentBinTestServer with the
// netbird_agent_download_only system setting explicitly seeded, for the gate test.
func newAgentBinTestServerWithDownloadOnly(t *testing.T, dir string, downloadOnly bool) *Server {
	t.Helper()
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	scopesJSON, err := json.Marshal([]string{"gateway:use", "admin", "system"})
	if err != nil {
		t.Fatalf("marshal scopes: %v", err)
	}
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: string(scopesJSON), CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	settings := portal.NewMemorySystemSettings()
	if downloadOnly {
		if err := settings.SetSystemSetting(context.Background(), "netbird_agent_download_only", "true", now); err != nil {
			t.Fatalf("SetSystemSetting: %v", err)
		}
	}
	svc := portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock(), SystemSettings: settings})
	srv := New(ServerDeps{Tokens: tokens, Usage: recorder, Provider: provider.NewMock(), Routes: routeStore, Portal: svc, AgentBinaryDir: dir})
	seedTestAgentToken(t, srv, "agt_test", "mock-host-qwen", "valid-agent-secret")
	return srv
}

// authAsPortalUser sets the dev bearer token seeded by newAgentBinTestServer
// (scopes gateway:use+admin+system) on req.
func authAsPortalUser(t *testing.T, req *http.Request) {
	t.Helper()
	req.Header.Set("Authorization", "Bearer dev-secret")
}

type gatewayMeshMaterialPortal struct {
	portal.API
	material      portal.GatewayMeshCertificateMaterial
	err           error
	downloadOnly  bool
	dns           string
	materialReads int
}

func (p *gatewayMeshMaterialPortal) GatewayMeshCertificate(context.Context) (portal.GatewayMeshCertificateMaterial, error) {
	p.materialReads++
	return p.material, p.err
}

func (p *gatewayMeshMaterialPortal) CertMeshRequireTLSChecked(context.Context) bool { return false }

func (p *gatewayMeshMaterialPortal) NetbirdAgentDownloadOnly(context.Context) bool {
	return p.downloadOnly
}

func (p *gatewayMeshMaterialPortal) ResolveGatewayPeerDNS(context.Context) (string, error) {
	return p.dns, nil
}

func serverWithGatewayMeshMaterial(material portal.GatewayMeshCertificateMaterial, state AgentListenerTLSState) *Server {
	srv := New(ServerDeps{Portal: &gatewayMeshMaterialPortal{material: material, dns: material.Domain}})
	srv.SetAgentListener(true, state.Address)
	srv.SetAgentListenerTLSState(state)
	return srv
}

func parseAgentConfigJSONC(t *testing.T, raw string) map[string]any {
	t.Helper()
	var kept []string
	for _, ln := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(strings.Join(kept, "\n")), &cfg); err != nil {
		t.Fatalf("config not valid JSON after comment-strip: %v", err)
	}
	return cfg
}

func TestAgentConfigMaterialUsesRuntimeMeshTLSStateAndGatewayRow(t *testing.T) {
	srv := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
	}, AgentListenerTLSState{
		Active: true, Address: "100.64.0.10:9443", Fingerprint: strings.Repeat("a", 64),
	})

	got := srv.agentConfigMaterial(context.Background(), "http://public.example.test")
	if got.GatewayURL != "https://gateway.mesh.test:9443" {
		t.Fatalf("GatewayURL = %q, want runtime mesh HTTPS URL", got.GatewayURL)
	}
	if got.CAFile != "" || got.CACacheFile != "" || got.CAPEM != "" {
		t.Fatalf("ACME material unexpectedly carried custom trust: %+v", got)
	}
}

// TestAgentConfigURLUsesTLSPortInSeparateMode pins that the generated agent
// gateway_url takes its PORT from the runtime TLS listener state's address. In
// separate mode AgentListenerTLSState().Address is the dedicated TLS bind
// (AGENT_TLS_PORT), so the URL points at the TLS port with no separate-mode branch
// in the config builder — the state already carries the right port.
func TestAgentConfigURLUsesTLSPortInSeparateMode(t *testing.T) {
	fp := strings.Repeat("c", 64)
	srv := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gw.int.example.test", Fingerprint: fp,
	}, AgentListenerTLSState{
		Active: true, Address: "100.64.0.2:8443", Fingerprint: fp,
	})
	m, ok := srv.resolveAgentConfigMaterial(context.Background(), "http://fallback")
	if !ok || m.GatewayURL != "https://gw.int.example.test:8443" {
		t.Fatalf("gateway_url = %q ok=%v, want https://gw.int.example.test:8443", m.GatewayURL, ok)
	}
}

func TestAgentDownloadBaseUsesRuntimeMeshTLSStateAndGatewayRow(t *testing.T) {
	srv := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
	}, AgentListenerTLSState{
		Active: true, Address: "100.64.0.10:9443", Fingerprint: strings.Repeat("a", 64),
	})

	if got := srv.agentDownloadBase(context.Background(), true); got != "https://gateway.mesh.test:9443" {
		t.Fatalf("agentDownloadBase = %q, want runtime mesh HTTPS URL", got)
	}
}

func TestAgentConfigMaterialFallsBackToRequestOriginUntilTLSActuallyListens(t *testing.T) {
	srv := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
	}, AgentListenerTLSState{
		Active: false, Address: "100.64.0.10:9443", Fingerprint: strings.Repeat("a", 64),
	})

	got := srv.agentConfigMaterial(context.Background(), "https://public.example.test")
	if got != (agentConfigMaterial{GatewayURL: "https://public.example.test"}) {
		t.Fatalf("material = %+v, want request-origin fallback with empty CA fields", got)
	}

	// Active can briefly describe the last successfully loaded leaf while a newer
	// database row already exists. Never generate trust for a leaf the listener
	// is not actually serving.
	mismatch := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("b", 64),
	}, AgentListenerTLSState{
		Active: true, Address: "100.64.0.10:9443", Fingerprint: strings.Repeat("a", 64),
	}).agentConfigMaterial(context.Background(), "https://public.example.test")
	if mismatch != (agentConfigMaterial{GatewayURL: "https://public.example.test"}) {
		t.Fatalf("fingerprint-mismatch material = %+v, want request-origin fallback", mismatch)
	}
}

func TestAgentConfigMaterialSelfSignedCarriesPublicBootstrapAndCache(t *testing.T) {
	const bundle = "-----BEGIN CERTIFICATE-----\nROOT-BUNDLE\n-----END CERTIFICATE-----\n"
	srv := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
		IssuerFingerprint: strings.Repeat("b", 64), CABundlePEM: bundle,
	}, AgentListenerTLSState{Active: true, Address: "[fd00::10]:8081"})

	got := srv.agentConfigMaterial(context.Background(), "http://public.example.test")
	if got.GatewayURL != "https://gateway.mesh.test:8081" {
		t.Fatalf("GatewayURL = %q", got.GatewayURL)
	}
	if got.CAFile != "" {
		t.Fatalf("CAFile = %q, generated configs must never claim an operator-managed file", got.CAFile)
	}
	if got.CACacheFile != "server-agent-ca.pem" {
		t.Fatalf("CACacheFile = %q", got.CACacheFile)
	}
	if got.CAPEM != bundle {
		t.Fatalf("CAPEM differs from root bundle: got %q want %q", got.CAPEM, bundle)
	}
}

func TestAgentConfigMaterialFollowsStoredLeafIssuerAcrossModeSwitches(t *testing.T) {
	const bundle = "-----BEGIN CERTIFICATE-----\nCURRENT-INTERNAL-ROOT\n-----END CERTIFICATE-----\n"
	state := AgentListenerTLSState{Active: true, Address: "100.64.0.10:8081"}

	// Old self-signed leaf retained after the target mode switched to ACME: its
	// stored issuer still requires the internal bootstrap bundle.
	selfSigned := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
		IssuerFingerprint: strings.Repeat("b", 64), CABundlePEM: bundle,
	}, state).agentConfigMaterial(context.Background(), "http://public.example.test")
	if selfSigned.CAPEM != bundle || selfSigned.CACacheFile != "server-agent-ca.pem" {
		t.Fatalf("retained self-signed leaf lost its trust material: %+v", selfSigned)
	}

	// Old ACME leaf retained after the target mode switched to self_signed: the
	// now-present internal bundle must not be attached to a system-root leaf.
	acme := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("c", 64), CABundlePEM: bundle,
	}, state).agentConfigMaterial(context.Background(), "http://public.example.test")
	if acme.CAPEM != "" || acme.CACacheFile != "" {
		t.Fatalf("retained ACME leaf incorrectly followed the target mode's CA: %+v", acme)
	}

	// A self-signed issuer without a readable public bundle must not advertise
	// an empty cache/bootstrap contract.
	missingBundle := serverWithGatewayMeshMaterial(portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("d", 64),
		IssuerFingerprint: strings.Repeat("e", 64),
	}, state).agentConfigMaterial(context.Background(), "http://public.example.test")
	if missingBundle.CAPEM != "" || missingBundle.CACacheFile != "" {
		t.Fatalf("missing bundle unexpectedly produced trust material: %+v", missingBundle)
	}
}

func TestHandlePortalAgentBinariesListsManifest(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries", nil)
	authAsPortalUser(t, req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got agentBinariesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (%s)", err, rec.Body.String())
	}
	if got.AgentVersion != "0.1.0" || len(got.Binaries) != 1 {
		t.Fatalf("unexpected payload: %+v", got)
	}
	if got.NetbirdAgentDownloadOnly {
		t.Errorf("netbird_agent_download_only = true, want false")
	}
	if got.AgentDownloadBase != "" {
		t.Errorf("agent_download_base = %q, want empty (listener inactive)", got.AgentDownloadBase)
	}
}

func TestHandlePortalAgentBinariesUsesRuntimeMeshTLSBase(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	srv := newAgentBinTestServer(t, dir)
	material := portal.GatewayMeshCertificateMaterial{
		Domain: "gateway.mesh.test", Fingerprint: strings.Repeat("a", 64),
	}
	materialPortal := &gatewayMeshMaterialPortal{
		API: srv.Portal, material: material, downloadOnly: true, dns: material.Domain,
	}
	srv.Portal = materialPortal
	srv.SetAgentListener(true, "100.64.0.10:9443")
	srv.SetAgentListenerTLSState(AgentListenerTLSState{
		Active: true, Address: "100.64.0.10:9443", Fingerprint: material.Fingerprint,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries", nil)
	authAsPortalUser(t, req)
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if materialPortal.materialReads != 1 {
		t.Fatalf("manifest material reads = %d, want exactly 1 coherent snapshot", materialPortal.materialReads)
	}
	var got agentBinariesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode manifest response: %v", err)
	}
	if got.AgentDownloadBase != "https://gateway.mesh.test:9443" {
		t.Fatalf("manifest agent_download_base = %q, want runtime mesh HTTPS URL", got.AgentDownloadBase)
	}
}

func TestHandlePortalAgentBinariesUnavailable(t *testing.T) {
	s := newAgentBinTestServer(t, t.TempDir()) // no manifest.json written
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries", nil)
	authAsPortalUser(t, req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !bytesContains(rec.Body.Bytes(), "agent.binaries_unavailable") {
		t.Fatalf("body = %s, want agent.binaries_unavailable", rec.Body.String())
	}
}

func TestHandlePortalAgentBinariesRequiresAuth(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries", nil)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlePortalAgentBinaryDownloadServesFile(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"windows","arch":"amd64","filename":"server-agent-windows-amd64.exe","size":3,"sha256":"deadbeef"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "server-agent-windows-amd64.exe"), []byte("MZ!"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newAgentBinTestServer(t, dir)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries/windows-amd64", nil)
	authAsPortalUser(t, req)
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="server-agent-windows-amd64.exe"` {
		t.Errorf("content-disposition = %q", got)
	}
	if got := rec.Header().Get("X-Checksum-SHA256"); got != "deadbeef" {
		t.Errorf("checksum header = %q", got)
	}
	if rec.Body.String() != "MZ!" {
		t.Errorf("body = %q", rec.Body.String())
	}
}

func TestHandlePortalAgentBinaryDownloadUnknownTarget(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries/plan9-amd64", nil)
	authAsPortalUser(t, req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandlePortalAgentBinaryDownloadTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/portal/agent-binaries/..%2f..%2fetc%2fpasswd", nil)
	authAsPortalUser(t, req)
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentDownloadServesManifestAndBinary(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	if err := os.WriteFile(filepath.Join(dir, "server-agent-linux-amd64"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newAgentBinTestServer(t, dir)

	// manifest
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/manifest", nil)
	req.Header.Set("Authorization", "Bearer valid-agent-secret")
	rec := httptest.NewRecorder()
	s.agentMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("manifest status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got agentBinariesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal manifest: %v (%s)", err, rec.Body.String())
	}
	if got.AgentVersion != "0.1.0" {
		t.Fatalf("unexpected manifest payload: %+v", got)
	}

	// binary
	req2 := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/linux-amd64", nil)
	req2.Header.Set("Authorization", "Bearer valid-agent-secret")
	rec2 := httptest.NewRecorder()
	s.agentMux.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("binary status = %d body=%s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.String() != "X" {
		t.Errorf("body = %q", rec2.Body.String())
	}
}

func TestHandleAgentDownloadServesConfig(t *testing.T) {
	// No manifest written: the config download must not depend on built binaries.
	s := newAgentBinTestServer(t, t.TempDir())
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/config", nil)
	req.Header.Set("Authorization", "Bearer valid-agent-secret")
	rec := httptest.NewRecorder()
	s.agentMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="server-agent.json"` {
		t.Errorf("content-disposition = %q", cd)
	}
	// Strip whole-line // comments (as the agent's loader does) — the remainder must
	// be valid JSON with the caller's bearer echoed into token and a derived gateway_url.
	var kept []string
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(strings.Join(kept, "\n")), &cfg); err != nil {
		t.Fatalf("config not valid JSON after comment-strip: %v", err)
	}
	if cfg["token"] != "valid-agent-secret" {
		t.Errorf("token = %v, want the caller's bearer echoed back", cfg["token"])
	}
	if cfg["transport"] != "websocket" {
		t.Errorf("transport = %v, want websocket", cfg["transport"])
	}
	if gw, _ := cfg["gateway_url"].(string); gw != "http://example.com" {
		t.Errorf("gateway_url = %v, want http://example.com (derived from the request host)", cfg["gateway_url"])
	}
	// The four Phase 2 certificate-installation keys must be present at their
	// documented defaults: "off"/empty everywhere, so an agent that never
	// touches this stanza behaves exactly as before (no-op invariant).
	if cfg["cert_mode"] != "off" {
		t.Errorf("cert_mode = %v, want \"off\"", cfg["cert_mode"])
	}
	for _, key := range []string{"cert_dir", "cert_reload_command", "cert_poll_interval"} {
		if v, _ := cfg[key].(string); v != "" {
			t.Errorf("%s = %v, want empty string", key, cfg[key])
		}
	}
}

// TestBuildAgentConfigJSONKeySet is a drift guard: the JSONC template is
// hand-duplicated in FOUR places that cannot share code (two languages, two
// Go modules) --
//
//  1. this Go backend's buildAgentConfigJSON, served over
//     /api/agent/v1/download/config;
//  2. the frontend's buildServerAgentConfig in AgentTokenSection.tsx, behind
//     the download button one row from the curl for copy 1;
//  3. the standalone server-agent module's config fixture, which defines what
//     the agent will actually read and which this package cannot import,
//     being a separate Go module;
//  4. server-agent/README.md, which documents it for the operator.
//
// This test pins the EXACT key set buildAgentConfigJSON emits against a
// maintained expectation list below. It guards THIS copy only, and cannot see
// any of the other three: copy 2 has its own equivalent pin (in
// AgentTokenSection.test.tsx), copies 3 and 4 have none. So adding, removing
// or renaming a key here fails this test, which is the prompt to go and
// update the other three by hand -- not a guarantee that they were.
func TestBuildAgentConfigJSONKeySet(t *testing.T) {
	raw := buildAgentConfigJSON(agentConfigMaterial{GatewayURL: "https://gw.example"}, "tok")
	var kept []string
	for _, ln := range strings.Split(raw, "\n") {
		if strings.HasPrefix(strings.TrimSpace(ln), "//") {
			continue
		}
		kept = append(kept, ln)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(strings.Join(kept, "\n")), &cfg); err != nil {
		t.Fatalf("config not valid JSON after comment-strip: %v\n%s", err, raw)
	}
	// Maintained by hand. When adding a key to buildAgentConfigJSON, add it
	// here too, AND to server-agent/internal/config/config.go's fileConfig
	// (+ its README table + JSONC example) AND to AgentTokenSection.tsx's
	// buildServerAgentConfig.
	want := []string{
		"gateway_url", "token", "transport", "interval", "system_report_interval",
		"metrics_url", "model_status_url", "model_status_format", "lhm_url",
		"cert_mode", "cert_dir", "cert_reload_command", "cert_poll_interval",
		"ca_file", "ca_cache_file", "ca_pem",
		// The operator-only half of the managed-runtime router bind contract:
		// the gateway supplies the router PORT, this decides its bind host,
		// and an empty value means the agent falls back to ALL INTERFACES.
		// A generated config that never mentions it leaves that decision
		// invisible to the operator who is making it.
		"runtime_router_bind",
		"tls_insecure", "verbose",
	}
	wantSet := make(map[string]bool, len(want))
	for _, k := range want {
		wantSet[k] = true
	}
	if len(cfg) != len(want) {
		t.Errorf("buildAgentConfigJSON emits %d keys, want %d (got %v)", len(cfg), len(want), sortedKeys(cfg))
	}
	for k := range cfg {
		if !wantSet[k] {
			t.Errorf("unexpected key %q in buildAgentConfigJSON output (update the expectation list in this test AND server-agent's fileConfig AND buildServerAgentConfig)", k)
		}
	}
	for _, k := range want {
		if _, ok := cfg[k]; !ok {
			t.Errorf("missing key %q from buildAgentConfigJSON output", k)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func TestHandleAgentDownloadRejectsMissingToken(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/manifest", nil)
	rec := httptest.NewRecorder()
	s.agentMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentDownloadRejectsInvalidToken(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/manifest", nil)
	req.Header.Set("Authorization", "Bearer not-the-right-secret")
	rec := httptest.NewRecorder()
	s.agentMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleAgentDownloadRejectsUnknownTarget(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServer(t, dir)
	rec := httptest.NewRecorder()
	// %2e%2e encodes the dots so net/http's ServeMux (which cleans/redirects on a
	// LITERAL ".." segment before ever reaching a handler, on Go 1.26 with a 307 to
	// the collapsed path) doesn't intercept the request — this exercises our OWN
	// agentTargetAllowed whitelist rejection inside handleAgentDownload/
	// serveAgentBinary, decoded back to "../secret" by the time it reaches r.URL.Path.
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/%2e%2e/secret", nil)
	req.Header.Set("Authorization", "Bearer valid-agent-secret") // fake LookupAgentToken accepts it
	s.agentMux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestAgentDownloadGateOnRejectsPublicServesAgent mirrors
// TestAgentGateOnRejectsPublicServesAgent (agent_listener_test.go): with
// netbird_agent_download_only ON and an agent listener active, the PUBLIC mux
// rejects /api/agent/v1/download/* with 403 netbird.only (even with a valid
// agent token — the gate runs before handleAgentDownload's own auth), while the
// NetBird agent mux (ungated) still serves it.
func TestAgentDownloadGateOnRejectsPublicServesAgent(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServerWithDownloadOnly(t, dir, true)
	s.SetAgentListener(true, "")

	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/manifest", nil)
	req.Header.Set("Authorization", "Bearer valid-agent-secret")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("public mux status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if !bytesContains(rec.Body.Bytes(), "netbird.only") {
		t.Fatalf("public mux body = %s, want netbird.only", rec.Body.String())
	}

	agentRec := httptest.NewRecorder()
	s.agentMux.ServeHTTP(agentRec, req)
	if agentRec.Code != http.StatusOK {
		t.Fatalf("agent mux (ungated) status = %d, want 200; body=%s", agentRec.Code, agentRec.Body.String())
	}
}

// TestAgentDownloadGateFailSafeNoListenerServesPublic mirrors
// TestAgentGateFailSafeNoListenerServesPublic: netbird_agent_download_only ON
// but no agent listener bound (AgentListenerActive false) leaves the public mux
// serving the download route (a UI toggle alone can never cut off ALL agents).
func TestAgentDownloadGateFailSafeNoListenerServesPublic(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServerWithDownloadOnly(t, dir, true)
	// AgentListenerActive left false.

	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/download/manifest", nil)
	req.Header.Set("Authorization", "Bearer valid-agent-secret")
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("fail-safe: public mux status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// fakePortalDNSCounter wraps a real portal.API and counts calls to
// ResolveGatewayPeerDNS, so cachedGatewayPeerDNS's TTL cache can be proven to
// single-flight the (potentially slow, up to ~15s) live NetBird GetPeer call
// across repeated agentDownloadBase invocations within the TTL window.
type fakePortalDNSCounter struct {
	portal.API
	calls int
}

func (f *fakePortalDNSCounter) CertMeshRequireTLSChecked(context.Context) bool { return false }

func (f *fakePortalDNSCounter) ResolveGatewayPeerDNS(context.Context) (string, error) {
	f.calls++
	return "gw.netbird.selfhosted", nil
}

// TestCachedGatewayPeerDNSCachesAcrossCalls is the spec-compliance regression for
// §6.1: a portal agent-binaries list GET must never trigger a live NetBird call
// per request. With netbird_agent_download_only on and the agent listener
// active, two agentDownloadBase calls in quick succession must resolve the
// gateway peer DNS only ONCE (agentDNSCacheTTL=60s far exceeds the test's
// runtime, so the second call is served from cache, not a fresh NetBird hit).
func TestCachedGatewayPeerDNSCachesAcrossCalls(t *testing.T) {
	dir := t.TempDir()
	writeManifest(t, dir, `{"schema":1,"agent_version":"0.1.0","binaries":[{"os":"linux","arch":"amd64","filename":"server-agent-linux-amd64","size":1,"sha256":"aa"}]}`)
	s := newAgentBinTestServerWithDownloadOnly(t, dir, true)
	s.SetAgentListener(true, "100.0.0.1:8081")
	fake := &fakePortalDNSCounter{API: s.Portal}
	s.Portal = fake

	ctx := context.Background()
	first := s.agentDownloadBase(ctx, true)
	second := s.agentDownloadBase(ctx, true)
	if first != second {
		t.Fatalf("agentDownloadBase not stable across calls: %q vs %q", first, second)
	}
	if fake.calls != 1 {
		t.Fatalf("ResolveGatewayPeerDNS calls = %d, want 1 (2nd call should be served from the TTL cache)", fake.calls)
	}
	if want := "http://gw.netbird.selfhosted:8081"; first != want {
		t.Fatalf("agentDownloadBase = %q, want %q", first, want)
	}
}

func bytesContains(body []byte, substr string) bool {
	return strings.Contains(string(body), substr)
}

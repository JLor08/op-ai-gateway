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
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
	"testing"
	"time"
)

const (
	certUserSecret   = "cert-user-secret"
	certAdminSecret  = "cert-admin-secret"
	certSystemSecret = "cert-system-secret"
)

// newTestServerWithACME builds a *Server (mirrors newNetbirdEndpointFixture's
// construction) whose Portal is a real portal.Service, wired with ServerDeps.
// ACMEChallenges = store, plus three plain bearer tokens: a plain user
// (gateway:use only), a non-system admin (gateway:use+admin), and a system
// admin (gateway:use+admin+system). Returns the *Server and the underlying
// *routing.MemoryStore so a test can seed a server.
func newTestServerWithACME(t *testing.T, chal certissue.ChallengeStore) (*Server, *routing.MemoryStore) {
	t.Helper()
	return newTestServerWithACMEEdgeDir(t, chal, "")
}

// newTestServerWithACMEEdgeDir is newTestServerWithACME plus the edge-certificate
// local-delivery directory. That directory is what EdgeDeliveryCapable() reads, so
// it is the ONE knob the edge key-download gate turns on: "" = the gateway cannot
// deliver locally (download allowed), a real path = it can (download refused).
func newTestServerWithACMEEdgeDir(t *testing.T, chal certissue.ChallengeStore, edgeOutputDir string) (*Server, *routing.MemoryStore) {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	tokens := auth.NewTokenStore()
	dir := portal.NewMemoryDirectory(tokens)
	dir.AddUser(store.User{ID: "usr_user", Email: "user@example.test", DisplayName: "Plain User", Role: "user", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_admin", Email: "admin@example.test", DisplayName: "Admin", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	dir.AddUser(store.User{ID: "usr_system", Email: "system@example.test", DisplayName: "System Admin", Role: "system_admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_user", UserID: "usr_user", Name: "User Token", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now}, certUserSecret); err != nil {
		t.Fatalf("CreatePlainToken user: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_admin", UserID: "usr_admin", Name: "Admin Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, certAdminSecret); err != nil {
		t.Fatalf("CreatePlainToken admin: %v", err)
	}
	if err := dir.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_system", UserID: "usr_system", Name: "System Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin","system"]`, CreatedAt: now, UpdatedAt: now}, certSystemSecret); err != nil {
		t.Fatalf("CreatePlainToken system: %v", err)
	}
	cipher, err := capture.New(strings.Repeat("cd", 32))
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
		// Certificate private keys are sealed with their OWN cipher; wiring only
		// Cipher here would leave the certificate seal path unavailable.
		CertCipher:        cipher,
		CertEdgeOutputDir: edgeOutputDir,
		Clock:             func() time.Time { return now },
	})
	srv := New(ServerDeps{Tokens: tokens, Routes: routeStore, Portal: svc, ACMEChallenges: chal})
	return srv, routeStore
}

// seedServerForACME creates a server in routeStore owned by usr_user (the
// certUserSecret principal) and returns its id.
func seedServerForACME(t *testing.T, routeStore *routing.MemoryStore) string {
	t.Helper()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	const id = "srv_cert_owned"
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: id, Name: "Owned Host", Domain: "owned.example.test", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), id, []string{"usr_user"}); err != nil {
		t.Fatalf("SetServerOwners: %v", err)
	}
	// A reporting-agent token is a PRECONDITION for the certificate reconcile to
	// want this server at all (Phase 2: no agent means no distribution path, see
	// portal.Service.serverHasAgentToken), so every fixture that expects an
	// internal server row to be issued has to mint one.
	if err := routeStore.UpsertAgentToken(context.Background(), routing.AgentToken{
		ID: "agt_cert_owned", ServerID: id, SecretPrefix: "test", CreatedAt: now, UpdatedAt: now,
	}, "hash_cert_owned"); err != nil {
		t.Fatalf("UpsertAgentToken: %v", err)
	}
	return id
}

// enableSelfSignedForTest flips the certificate module on in self_signed mode
// with a base domain, then runs one reconcile pass so an internal CA exists for
// the CA endpoint to serve.
func enableSelfSignedForTest(t *testing.T, srv *Server) {
	t.Helper()
	enabled := true
	mode := portal.IssuerModeSelfSigned
	domain := "example.test"
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:    &enabled,
		CertIssuerMode: &mode,
		CertBaseDomain: &domain,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	srv.Portal.ReconcileCertificates(context.Background())
}

func certUserRequest(t *testing.T, method, path string, body *strings.Reader) *http.Request {
	t.Helper()
	return certRequestWith(method, path, body, certUserSecret)
}

func certAdminRequest(t *testing.T, method, path string, body *strings.Reader) *http.Request {
	t.Helper()
	return certRequestWith(method, path, body, certAdminSecret)
}

func certSystemRequest(t *testing.T, method, path string, body *strings.Reader) *http.Request {
	t.Helper()
	return certRequestWith(method, path, body, certSystemSecret)
}

func certRequestWith(method, path string, body *strings.Reader, secret string) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, body)
	}
	req.Header.Set("Authorization", "Bearer "+secret)
	return req
}

func TestACMEChallengeServesStoredToken(t *testing.T) {
	store := certissue.NewMemoryChallengeStore()
	srv, _ := newTestServerWithACME(t, store)
	store.Put("tok-abc", "tok-abc.keyauth")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok-abc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "tok-abc.keyauth" {
		t.Fatalf("body = %q, want the key authorization", got)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
}

// TestACMEChallengeUnknownTokenIs404AndNoTraversal pins the two guards in
// handleACMEChallenge that run BEFORE the challenge store is ever consulted:
// an empty token (the trailing-slash request) and a token containing "/"
// (e.g. an encoded ".." traversal, which net/http's ServeMux passes through
// unmangled because it matches on the ESCAPED path but hands the handler the
// DECODED r.URL.Path). A naive test that never populates the store cannot
// tell "the guard rejected it" apart from "the store legitimately has no
// such key" -- both read as a plain 404. To make the guards load-bearing,
// this seeds a DECOY entry under the exact key each guard-less lookup would
// hit (`Get("")` for the empty token, `Get("../../etc/passwd")` for the
// traversal token) and asserts both (a) the response is still 404 and
// (b) the decoy's value never appears in the body -- so removing either
// guard flips the request to a 200 carrying the decoy's key-authorization,
// and this test fails.
func TestACMEChallengeUnknownTokenIs404AndNoTraversal(t *testing.T) {
	const (
		emptyTokenDecoy     = "leaked-empty-token-keyauth"
		traversalTokenDecoy = "leaked-traversal-token-keyauth"
	)
	store := certissue.NewMemoryChallengeStore()
	srv, _ := newTestServerWithACME(t, store)
	// Seed the exact keys a guard-less Get() would hit for the two guarded
	// paths below (the third path, "nope", is a genuinely-unknown token and
	// needs no decoy -- it exercises the plain not-found branch instead).
	store.Put("", emptyTokenDecoy)
	store.Put("../../etc/passwd", traversalTokenDecoy)

	for _, path := range []string{
		"/.well-known/acme-challenge/nope",
		"/.well-known/acme-challenge/",
		"/.well-known/acme-challenge/..%2f..%2fetc%2fpasswd",
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s -> %d, want 404", path, rec.Code)
		}
		if body := rec.Body.String(); strings.Contains(body, emptyTokenDecoy) || strings.Contains(body, traversalTokenDecoy) {
			t.Fatalf("%s -> body %q leaked a decoy key-authorization (a guard was bypassed)", path, body)
		}
	}
}

// TestACMEChallengeNilStoreIs404 pins the fail-closed default: a Server built
// with no ACMEChallenges store must never panic on the challenge route -- it
// answers a plain 404, same as an unknown token.
func TestACMEChallengeNilStoreIs404(t *testing.T) {
	srv, _ := newTestServerWithACME(t, nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok-abc", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("nil store -> %d, want 404", rec.Code)
	}
}

// TestACMEChallengeNeverOnAgentMux pins that the public challenge route is
// registered ONLY on s.mux, never on s.agentMux -- the agent listener carries
// only the agent-telemetry/stream/system-report/download routes + /healthz.
func TestACMEChallengeNeverOnAgentMux(t *testing.T) {
	store := certissue.NewMemoryChallengeStore()
	srv, _ := newTestServerWithACME(t, store)
	store.Put("tok-abc", "tok-abc.keyauth")

	rec := httptest.NewRecorder()
	srv.agentMux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/.well-known/acme-challenge/tok-abc", nil))
	// The route is simply absent from agentMux, so http.ServeMux's own
	// NotFoundHandler answers -- assert the exact 404, not merely "not 200"
	// (a loose != 200 check would also pass on an accidental 301/500).
	if rec.Code != http.StatusNotFound {
		t.Fatalf("agentMux -> %d, want 404 (the route must not be registered there)", rec.Code)
	}
}

// TestACMEChallengeRejectsNonGET pins the method gate: a POST to the challenge
// path is rejected rather than silently falling through to a GET-shaped answer.
func TestACMEChallengeRejectsNonGET(t *testing.T) {
	store := certissue.NewMemoryChallengeStore()
	srv, _ := newTestServerWithACME(t, store)
	store.Put("tok-abc", "tok-abc.keyauth")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/.well-known/acme-challenge/tok-abc", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST -> %d, want 405", rec.Code)
	}
}

func TestPortalCertificatesEnabledEndpoint(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())

	t.Run("off by default", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, certUserRequest(t, http.MethodGet, "/api/portal/certificates/enabled", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		var body struct {
			ModuleEnabled bool `json:"module_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.ModuleEnabled {
			t.Fatal("module_enabled should be false before cert_enabled is set")
		}
	})

	t.Run("reflects cert_enabled once set", func(t *testing.T) {
		enableSelfSignedForTest(t, srv)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, certUserRequest(t, http.MethodGet, "/api/portal/certificates/enabled", nil))
		var body struct {
			ModuleEnabled bool `json:"module_enabled"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.ModuleEnabled {
			t.Fatal("module_enabled should be true after cert_enabled is set")
		}
	})

	t.Run("non-GET method is rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, certUserRequest(t, http.MethodPost, "/api/portal/certificates/enabled", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST -> %d, want 405", rec.Code)
		}
	})
}

func TestCertificatesEndpointRequiresSystemScope(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	// An admin-scoped session must NOT reach a system endpoint.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodGet, "/api/system/certificates", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("system scope -> %d, want 200", rec.Code)
	}
	var body struct {
		Data []portal.CertificateDTO `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (body %s)", err, rec.Body.String())
	}
	if body.Data == nil {
		t.Fatal("data must be an empty array, never null (frontend .map)")
	}
	// A non-GET method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST -> %d, want 405", rec.Code)
	}
}

type fakePortalCertificateMesh struct {
	portal.API
	certs   []portal.CertificateDTO
	pending []portal.CertificateServerRefDTO
}

func (f fakePortalCertificateMesh) CertificatesView(context.Context) ([]portal.CertificateDTO, error) {
	return f.certs, nil
}

func (f fakePortalCertificateMesh) GatewayCARotationPendingServers(context.Context) []portal.CertificateServerRefDTO {
	return f.pending
}

// fakePortalCertificateMesh covers certPortal, the narrow portal surface
// certificates.go/public_certificates.go actually call (GW-6). It wraps a
// live srv.Portal, so this is a documentation assertion rather than a
// latent-panic check.
var _ certPortal = fakePortalCertificateMesh{}

func TestSystemCertificatesIncludesRuntimeMeshState(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = fakePortalCertificateMesh{
		API: srv.Portal,
		certs: []portal.CertificateDTO{{
			Domain: "gateway.int.example.test", Kind: "gateway", Status: "active",
			Fingerprint: strings.Repeat("d", 64),
		}},
		pending: []portal.CertificateServerRefDTO{
			{ID: "srv-z", Name: "Zulu GPU"},
			{ID: "srv-a", Name: "Alpha GPU"},
			{ID: "srv-a", Name: "Alpha GPU"},
		},
	}
	notAfter := time.Date(2027, 8, 15, 0, 0, 0, 0, time.UTC)
	srv.SetAgentListenerTLSState(AgentListenerTLSState{
		Active: true, Address: "100.64.0.1:8081", Fingerprint: strings.Repeat("a", 64), NotAfter: notAfter,
	})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data []portal.CertificateDTO `json:"data"`
		Mesh struct {
			TLSActive                bool                             `json:"tls_active"`
			Address                  string                           `json:"address"`
			Fingerprint              string                           `json:"fingerprint"`
			NotAfter                 *time.Time                       `json:"not_after"`
			CARotationPendingServers []portal.CertificateServerRefDTO `json:"ca_rotation_pending_servers"`
		} `json:"mesh"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if !body.Mesh.TLSActive || body.Mesh.Address != "100.64.0.1:8081" || body.Mesh.Fingerprint != strings.Repeat("a", 64) {
		t.Fatalf("mesh=%+v, want the runtime listener snapshot", body.Mesh)
	}
	if body.Mesh.Fingerprint == body.Data[0].Fingerprint {
		t.Fatal("mesh fingerprint must come from runtime state, not the stored gateway certificate row")
	}
	if body.Mesh.NotAfter == nil || !body.Mesh.NotAfter.Equal(notAfter) {
		t.Fatalf("mesh not_after=%v, want %s", body.Mesh.NotAfter, notAfter)
	}
	wantPending := []portal.CertificateServerRefDTO{{ID: "srv-a", Name: "Alpha GPU"}, {ID: "srv-z", Name: "Zulu GPU"}}
	if len(body.Mesh.CARotationPendingServers) != len(wantPending) {
		t.Fatalf("pending=%+v, want sorted/deduplicated %+v", body.Mesh.CARotationPendingServers, wantPending)
	}
	for i := range wantPending {
		if body.Mesh.CARotationPendingServers[i] != wantPending[i] {
			t.Fatalf("pending[%d]=%+v, want %+v", i, body.Mesh.CARotationPendingServers[i], wantPending[i])
		}
	}
}

func TestSystemCertificatesMeshNeverSerializesKeyOrPEM(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = fakePortalCertificateMesh{API: srv.Portal}
	srv.SetAgentListenerTLSState(AgentListenerTLSState{
		Active: true, Address: "100.64.0.1:8081", Fingerprint: strings.Repeat("b", 64),
		NotAfter: time.Date(2027, 8, 15, 0, 0, 0, 0, time.UTC),
	})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"private_key", "key_pem", "fullchain_pem", "ca_pem", "BEGIN CERTIFICATE", "BEGIN PRIVATE KEY"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("mesh response leaked %q: %s", forbidden, body)
		}
	}
}

func TestRenewEndpointRejectsUnknownDomain(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())

	// Insufficient scope -> 403, before the body is even read.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodPost, "/api/system/certificates/renew",
		strings.NewReader(`{"domain":"nope.example.test"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/renew",
		strings.NewReader(`{"domain":"nope.example.test"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.not_found") {
		t.Fatalf("body = %s, want the certificate.not_found code", rec.Body.String())
	}

	// A non-POST method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/renew", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", rec.Code)
	}
}

func TestServerCertificateOverrideEndpoint(t *testing.T) {
	srv, routeStore := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	id := seedServerForACME(t, routeStore) // owned by the requesting principal

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/certificate",
		strings.NewReader(`{"certificate_override":"include"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"certificate_override":"include"`) {
		t.Fatalf("DTO must report the new override, got %s", rec.Body.String())
	}

	// An unknown value is a 400, not a silent write.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/certificate",
		strings.NewReader(`{"certificate_override":"maybe"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid override -> %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cert.invalid") {
		t.Fatalf("body = %s, want the cert.invalid code", rec.Body.String())
	}

	// An unknown server id is a 404 with no hint that it exists elsewhere.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/srv-does-not-exist/certificate",
		strings.NewReader(`{"certificate_override":"include"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server -> %d, want 404", rec.Code)
	}

	// A same-owner re-save with a different valid value still succeeds.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/certificate",
		strings.NewReader(`{"certificate_override":"exclude"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner re-save -> %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// A non-PUT method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodGet, "/api/portal/servers/"+id+"/certificate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", rec.Code)
	}
}

func TestServerHTTPSSwitchOverrideEndpoint(t *testing.T) {
	srv, routeStore := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	id := seedServerForACME(t, routeStore) // owned by the requesting principal

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/https-switch",
		strings.NewReader(`{"https_switch_override":"include"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"https_switch_override":"include"`) {
		t.Fatalf("DTO must report the new override, got %s", rec.Body.String())
	}

	// An unknown value is a 400, not a silent write.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/https-switch",
		strings.NewReader(`{"https_switch_override":"maybe"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid override -> %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "cert.invalid") {
		t.Fatalf("body = %s, want the cert.invalid code", rec.Body.String())
	}

	// An unknown server id is a 404 with no hint that it exists elsewhere.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/srv-does-not-exist/https-switch",
		strings.NewReader(`{"https_switch_override":"include"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server -> %d, want 404", rec.Code)
	}

	// A same-owner re-save with a different valid value still succeeds.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+id+"/https-switch",
		strings.NewReader(`{"https_switch_override":"exclude"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner re-save -> %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// A non-PUT method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodGet, "/api/portal/servers/"+id+"/https-switch", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", rec.Code)
	}

	// An invalid bearer is rejected at the shared auth chokepoint (before any
	// method/authz branch) with 401 -- the endpoint is not accidentally public.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certRequestWith(http.MethodPut, "/api/portal/servers/"+id+"/https-switch",
		strings.NewReader(`{"https_switch_override":"include"}`), "not-a-real-secret"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid bearer -> %d, want 401", rec.Code)
	}

	// A gateway:use principal who does NOT own the server cannot modify it and is
	// not even told it exists: a foreign-owned server yields the same 404-no-leak
	// as an unknown id (authorizeServerOwner returns ErrServerNotFound).
	foreign := "srv_cert_foreign"
	fnow := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	if err := routeStore.CreateAIServer(context.Background(), routing.AIServer{ID: foreign, Name: "Foreign Host", Domain: "foreign.example.test", CreatedAt: fnow, UpdatedAt: fnow}); err != nil {
		t.Fatalf("CreateAIServer(foreign): %v", err)
	}
	if err := routeStore.SetServerOwners(context.Background(), foreign, []string{"usr_other"}); err != nil {
		t.Fatalf("SetServerOwners(foreign): %v", err)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPut, "/api/portal/servers/"+foreign+"/https-switch",
		strings.NewReader(`{"https_switch_override":"include"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner modifying a foreign server -> %d, want 404 (no existence leak)", rec.Code)
	}
}

func TestCertificateCAEndpointNeverLeaksTheKey(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	enableSelfSignedForTest(t, srv) // writes the self_signed settings + runs one reconcile

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/ca", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "BEGIN CERTIFICATE") {
		t.Fatalf("bundle missing from the response: %s", body)
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("the CA endpoint must never emit a private key")
	}

	// Admin scope is not enough.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodGet, "/api/system/certificates/ca", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}

	// A non-GET method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST -> %d, want 405", rec.Code)
	}
}

func TestCertificateCARotateRequiresSelfSignedMode(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())

	// Insufficient scope -> 403.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}

	// Default mode is acme -> rotating an internal CA makes no sense there.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 in the acme mode (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cert.invalid") {
		t.Fatalf("body = %s, want the cert.invalid code", rec.Body.String())
	}

	// self_signed mode -> rotation succeeds.
	enableSelfSignedForTest(t, srv)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("self_signed rotate -> %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}

	// A non-POST method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", rec.Code)
	}
}

// fakePortalRotateInProgress wraps a nil portal.API and makes
// RotateCertificateCA return ErrCertReconcileInProgress unconditionally. It
// isolates the GATEWAY's error-code mapping (review finding F1.2) from the
// actual certMu lock-contention mechanics, which are pinned directly, with no
// HTTP layer involved, by
// TestRotateCertificateCAFailsFastInsteadOfBlockingWhileReconcileHoldsTheLock
// in internal/portal (certMu is a private field there, unreachable from this
// package) -- driving a REAL concurrent reconcile pass here would also be
// unable to hit this branch at all: RotateCertificateCA rejects with
// ErrCertInvalid for any mode but self_signed BEFORE it ever tries the lock,
// and a self_signed reconcile pass never makes a network call to block on.
type fakePortalRotateInProgress struct {
	portal.API // embedded nil interface; only the overridden method is ever called
}

func (fakePortalRotateInProgress) RotateCertificateCA(context.Context, auth.Token) error {
	return portal.ErrCertReconcileInProgress
}

// fakePortalRotateInProgress compiles against certPortal (GW-6), the narrow
// portal surface certificates.go actually calls. It satisfies it only via
// the embedded nil portal.API for every method besides RotateCertificateCA --
// a latent runtime panic on any other certPortal call this fake was never
// meant to exercise (it is used solely to drive the CA-rotate endpoint).
var _ certPortal = fakePortalRotateInProgress{}

func TestCertificateCARotateReturns409WhenReconcileHoldsTheLock(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = fakePortalRotateInProgress{}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("rotate while a reconcile pass holds the lock -> %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "certificate.reconcile_in_progress") {
		t.Fatalf("body = %s, want the certificate.reconcile_in_progress code", rec.Body.String())
	}

	// Authorization runs BEFORE RotateCertificateCA is ever called -- the fake
	// does not accidentally bypass it.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}
}

// fakePortalRotateNeedsCertKey mirrors fakePortalRotateInProgress for the
// certificate-encryption-key refusal: on a disk-backed store without
// OP_AI_GATEWAY_CERT_ENCRYPTION_KEY the new root is generated fine but cannot be
// sealed, so RotateCertificateCA surfaces ErrCertKeyRequired. Driving that
// through a real service here is impossible (the test service is wired WITH a
// certificate cipher, and a disk-store-without-key service could not be built in
// this package), so the fake isolates the gateway's error-code mapping.
type fakePortalRotateNeedsCertKey struct {
	portal.API // embedded nil interface; only the overridden method is ever called
}

func (fakePortalRotateNeedsCertKey) RotateCertificateCA(context.Context, auth.Token) error {
	return portal.ErrCertKeyRequired
}

// fakePortalRotateNeedsCertKey compiles against certPortal (GW-6) the same
// way fakePortalRotateInProgress does -- see that assertion's comment.
var _ certPortal = fakePortalRotateNeedsCertKey{}

func TestCertificateCARotateReturns400WhenTheCertificateKeyIsMissing(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())
	srv.Portal = fakePortalRotateNeedsCertKey{}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("rotate without a certificate encryption key -> %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "system.cert_key_required") {
		t.Fatalf("body = %s, want the system.cert_key_required code", body)
	}
	// The message must name the variable the operator has to set -- an opaque
	// 500 was the whole reason for this mapping.
	if !strings.Contains(body, "OP_AI_GATEWAY_CERT_ENCRYPTION_KEY") {
		t.Fatalf("body = %s, want it to name OP_AI_GATEWAY_CERT_ENCRYPTION_KEY", body)
	}

	// Authorization still runs BEFORE RotateCertificateCA is ever called.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodPost, "/api/system/certificates/ca/rotate", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}
}

func TestReissueAllCertificatesEndpoint(t *testing.T) {
	srv, _ := newTestServerWithACME(t, certissue.NewMemoryChallengeStore())

	// Insufficient scope -> 403.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodPost, "/api/system/certificates/reissue-all", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin scope -> %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/reissue-all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.OK {
		t.Fatal("ok should be true")
	}

	// A non-POST method is rejected.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/reissue-all", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET -> %d, want 405", rec.Code)
	}
}

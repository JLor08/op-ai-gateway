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
	"strings"
	"testing"
	"time"
)

// enableEdgeForTest turns the certificate module on in the internal-CA mode AND
// turns the edge certificate on with the given names, then runs one reconcile
// pass -- so a REAL edge row with a REAL sealed private key exists. Seeding the
// row by hand would let the key endpoint's tests pass against material this
// gateway could never actually have produced.
func enableEdgeForTest(t *testing.T, srv *Server, names ...string) {
	t.Helper()
	on := true
	mode := portal.IssuerModeSelfSigned
	base := "int.example.test"
	gw := "" // no gateway peer in this fixture -> no gateway certificate is desired
	scope := "all"
	list := names
	if _, err := srv.Portal.UpdateSystemSettings(context.Background(), auth.Token{Scopes: []string{"system"}}, portal.UpdateSystemSettingsRequest{
		CertEnabled:        &on,
		CertIssuerMode:     &mode,
		CertBaseDomain:     &base,
		CertGatewayDomain:  &gw,
		CertServerScope:    &scope,
		CertEdgeEnabled:    &on,
		CertEdgeIssuerMode: &mode,
		CertEdgeNames:      &list,
	}); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	srv.Portal.ReconcileCertificates(context.Background())
}

// TestEdgeKeyEndpointRefusesWhenTheGatewayCanDeliverItself is THE security test of
// the edge endpoints: the private key may leave the process only when there is no
// safe local path for it. The fixture has a real edge row AND a writable output
// directory, so a 409 here can only come from the capability gate -- if that gate
// is removed the very same request answers 200 with the key, and this fails.
func TestEdgeKeyEndpointRefusesWhenTheGatewayCanDeliverItself(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, t.TempDir())
	enableEdgeForTest(t, srv, "edge.lan")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/key", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (the key must not leave when it can be delivered); body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "BEGIN") {
		t.Fatal("the response leaked key material")
	}
	if code := errorCodeOf(t, rec.Body.Bytes()); code != "certificate.edge_key_managed" {
		t.Fatalf("error code = %q, want certificate.edge_key_managed", code)
	}
}

// TestEdgeKeyEndpointServesAndAuditsWhenDeliveryIsImpossible is the other half of
// the gate: with NO output directory the gateway cannot hand the key to its nginx,
// so the download is the only way and is allowed -- once, audibly.
func TestEdgeKeyEndpointServesAndAuditsWhenDeliveryIsImpossible(t *testing.T) {
	buf, restore := withCapturedSlog(t)
	defer restore()

	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/key", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "BEGIN EC PRIVATE KEY") {
		t.Fatalf("body does not carry the private key: %q", body)
	}
	if strings.Contains(body, "{") {
		t.Fatalf("body must be the bare key, not a JSON envelope: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-pem-file") {
		t.Fatalf("content-type = %q, want application/x-pem-file", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store", cc)
	}

	recs := buf.Snapshot()
	if !findLogRecord(recs, "WARN", "edge certificate private key downloaded") {
		t.Fatalf("no audit line for the key download: %+v", recs)
	}
	var sawCaller bool
	for _, r := range recs {
		if !strings.Contains(r.Msg, "edge certificate private key downloaded") {
			continue
		}
		if id, ok := r.Attrs["user_id"].(string); ok && id == "usr_system" {
			sawCaller = true
		}
		// The audit line names the caller -- never the key.
		for k, v := range r.Attrs {
			if s, ok := v.(string); ok && strings.Contains(s, "BEGIN") {
				t.Fatalf("the audit line leaked key material in attr %q", k)
			}
		}
	}
	if !sawCaller {
		t.Fatal("the audit line does not identify the caller (user_id)")
	}
	assertNoSecretInLogs(t, recs, certSystemSecret)
}

// TestEdgeKeyEndpointRequiresTheSystemScope pins the scope boundary. The fixture
// deliberately has NO output directory, so the capability gate would otherwise
// ALLOW the download -- what rejects these two callers is only the scope check.
func TestEdgeKeyEndpointRequiresTheSystemScope(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")

	for name, req := range map[string]*http.Request{
		"plain user":       certUserRequest(t, http.MethodGet, "/api/system/certificates/edge/key", nil),
		"non-system admin": certAdminRequest(t, http.MethodGet, "/api/system/certificates/edge/key", nil),
		"unauthenticated":  httptest.NewRequest(http.MethodGet, "/api/system/certificates/edge/key", nil),
	} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK {
			t.Fatalf("%s: status = 200, want a rejection", name)
		}
		if strings.Contains(rec.Body.String(), "BEGIN") {
			t.Fatalf("%s: response leaked key material", name)
		}
	}
}

// TestEdgeKeyEndpointIs404WithoutAnEdgeCertificate: the download path is open
// (no output directory) but nothing has been issued, so there is nothing to hand
// out -- a 404, not an empty 200 that a caller could mistake for a key.
func TestEdgeKeyEndpointIs404WithoutAnEdgeCertificate(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/key", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestEdgeKeyEndpointIsGETOnly(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/edge/key", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "BEGIN") {
		t.Fatal("the 405 response leaked key material")
	}
}

// TestEdgeStatusEndpointReportsDeliveryAndCarriesNoKeyMaterial pins the status
// DTO's two load-bearing fields (delivery_mode + key_download_available) against
// the SAME capability the key endpoint gates on, and that the DTO is key-free.
func TestEdgeStatusEndpointReportsDeliveryAndCarriesNoKeyMaterial(t *testing.T) {
	for _, tc := range []struct {
		name             string
		dir              string
		wantMode         string
		wantKeyAvailable bool
	}{
		{"with an output directory", t.TempDir(), "local", false},
		{"without an output directory", "", "download", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newTestServerWithACMEEdgeDir(t, nil, tc.dir)
			enableEdgeForTest(t, srv, "edge.lan", "10.0.0.5")

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, needle := range []string{"BEGIN", "PRIVATE KEY", "key_sealed", "fullchain"} {
				if strings.Contains(body, needle) {
					t.Fatalf("the edge status DTO leaks %q: %s", needle, body)
				}
			}
			var dto struct {
				Enabled            bool     `json:"enabled"`
				IssuerMode         string   `json:"issuer_mode"`
				Names              []string `json:"names"`
				Domain             string   `json:"domain"`
				Status             string   `json:"status"`
				DeliveryMode       string   `json:"delivery_mode"`
				OutputDir          string   `json:"output_dir"`
				KeyDownloadAllowed bool     `json:"key_download_available"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &dto); err != nil {
				t.Fatalf("decode: %v (%s)", err, body)
			}
			if !dto.Enabled || dto.IssuerMode != portal.IssuerModeSelfSigned {
				t.Fatalf("enabled/mode = %v/%q, want true/self_signed", dto.Enabled, dto.IssuerMode)
			}
			if len(dto.Names) != 2 || dto.Names[0] != "edge.lan" || dto.Names[1] != "10.0.0.5" {
				t.Fatalf("names = %v, want the configured list in order", dto.Names)
			}
			if dto.Domain != "edge.lan" || dto.Status != "active" {
				t.Fatalf("domain/status = %q/%q, want edge.lan/active", dto.Domain, dto.Status)
			}
			if dto.DeliveryMode != tc.wantMode {
				t.Fatalf("delivery_mode = %q, want %q", dto.DeliveryMode, tc.wantMode)
			}
			if dto.KeyDownloadAllowed != tc.wantKeyAvailable {
				t.Fatalf("key_download_available = %v, want %v", dto.KeyDownloadAllowed, tc.wantKeyAvailable)
			}
			if (dto.OutputDir != "") != (tc.dir != "") {
				t.Fatalf("output_dir = %q, want it to mirror the configured directory %q", dto.OutputDir, tc.dir)
			}
		})
	}
}

// TestEdgeStatusEndpointReportsPlaintextGateObservationState pins the merge this
// handler performs on top of EdgeCertificateView: the plaintext-gate fields
// (require_https/https_observed/last_encrypted_at) are NOT part of
// portal.EdgeCertificateDTO's own computation (the observation tracker lives on
// *gateway.Server) -- if handleSystemEdgeCertificate stopped merging them in,
// this test is what would catch it.
//
// require_https is pinned in BOTH directions: unarmed (the zero value a
// deleted dto.RequireHTTPS assignment would also produce, so asserting only
// `!= false` earlier let that exact regression through undetected) and, after
// actually arming the switch, `== true` -- an armed gate that renders back as
// an unarmed switch would otherwise invite an operator to redundantly re-arm
// it.
func TestEdgeStatusEndpointReportsPlaintextGateObservationState(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")

	get := func(withHTTPSHop bool) map[string]any {
		req := certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge", nil)
		if withHTTPSHop {
			req.Header.Set("X-OP-Edge-Scheme", "https")
		}
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v (%s)", err, rec.Body.String())
		}
		return body
	}

	before := get(false)
	if before["https_observed"] != false {
		t.Fatalf("https_observed = %v before any encrypted hop, want false", before["https_observed"])
	}
	if before["require_https"] != false {
		t.Fatalf("require_https = %v, want false (never armed in this test yet)", before["require_https"])
	}
	if before["last_encrypted_at"] != nil {
		t.Fatalf("last_encrypted_at = %v, want absent before any encrypted hop", before["last_encrypted_at"])
	}

	// noteEdgeScheme runs in serveWith BEFORE the handler dispatches, so this
	// GET's own X-OP-Edge-Scheme:https header is recorded before the handler
	// reads it back -- the same request that creates the observation already
	// reports it.
	after := get(true)
	if after["https_observed"] != true {
		t.Fatalf("https_observed = %v after an encrypted hop, want true", after["https_observed"])
	}
	if after["last_encrypted_at"] == nil {
		t.Fatal("last_encrypted_at is absent after an encrypted hop")
	}
	if after["require_https"] != false {
		t.Fatalf("require_https = %v, want still false (an observation alone does not arm it)", after["require_https"])
	}

	// Now actually arm the switch (mirrors TestSystemSettingsPUTGatesArmingOnAnObservation's
	// pattern: note a fresh observation directly, then PUT with the encrypted
	// hop header present, since the switch's own write path re-checks the
	// arming precondition against the request that carries it).
	srv.edgeScheme.Note(true, time.Now())
	armReq := certSystemRequest(t, http.MethodPut, "/api/system/settings", strings.NewReader(`{"cert_edge_require_https":true}`))
	armReq.Header.Set("X-OP-Edge-Scheme", "https")
	armRec := httptest.NewRecorder()
	srv.ServeHTTP(armRec, armReq)
	if armRec.Code != http.StatusOK {
		t.Fatalf("arming PUT = %d, body %s", armRec.Code, armRec.Body.String())
	}

	armed := get(true)
	if armed["require_https"] != true {
		t.Fatalf("require_https = %v after arming, want true -- an armed gate must not render as an unarmed switch", armed["require_https"])
	}
}

// TestEdgeBundleEndpointServesPublicMaterialOnly: the bundle is fullchain + the
// internal root, and it is allowed unconditionally (public certificates).
func TestEdgeBundleEndpointServesPublicMaterialOnly(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, t.TempDir())
	enableEdgeForTest(t, srv, "edge.lan")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("the edge bundle must never contain a private key")
	}
	// The leaf plus the internal root -- the trust anchor the upstream proxy needs.
	if n := strings.Count(body, "BEGIN CERTIFICATE"); n < 2 {
		t.Fatalf("bundle has %d certificates, want the leaf plus the internal root: %s", n, body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/x-pem-file") {
		t.Fatalf("content-type = %q, want application/x-pem-file", ct)
	}
}

func TestEdgeBundleEndpointRequiresTheSystemScopeAndIs404WhenUnissued(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/bundle", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unissued status = %d, want 404", rec.Code)
	}
	enableEdgeForTest(t, srv, "edge.lan")
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodGet, "/api/system/certificates/edge/bundle", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin status = %d, want 403", rec.Code)
	}
}

// TestEdgeReissueEndpointMarksOnlyTheEdgeRowDue proves the button is scoped: the
// edge row becomes due again while an internal row keeps its active status (the
// whole point of a per-row action next to the existing "reissue everything").
func TestEdgeReissueEndpointMarksOnlyTheEdgeRowDue(t *testing.T) {
	srv, routeStore := newTestServerWithACMEEdgeDir(t, nil, "")
	serverID := seedServerForACME(t, routeStore)
	if err := routeStore.UpdateServerNetbirdLink(context.Background(), serverID, true, "peer-1"); err != nil {
		t.Fatalf("UpdateServerNetbirdLink: %v", err)
	}
	enableEdgeForTest(t, srv, "edge.lan")

	certsBefore, err := srv.Portal.CertificatesView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var internalDomain string
	for _, c := range certsBefore {
		if c.Kind != "edge" && c.Status == "active" {
			internalDomain = c.Domain
		}
	}
	if internalDomain == "" {
		t.Fatalf("fixture produced no active internal row to contrast against: %+v", certsBefore)
	}

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/edge/reissue", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	certs, err := srv.Portal.CertificatesView(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range certs {
		switch {
		case c.Kind == "edge" && c.Status != "pending":
			t.Fatalf("edge row status = %q, want pending (due again)", c.Status)
		case c.Domain == internalDomain && c.Status != "active":
			t.Fatalf("internal row %q status = %q, want it untouched (active)", c.Domain, c.Status)
		}
	}
}

func TestEdgeReissueEndpointIsPOSTOnlyAndSystemScoped(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/reissue", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certUserRequest(t, http.MethodPost, "/api/system/certificates/edge/reissue", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("plain-user status = %d, want 403", rec.Code)
	}
}

// TestEdgeReissueEndpointIs404WithoutAnEdgeRow: nothing to re-issue must not read
// as success -- the operator would wait for a change that never comes.
func TestEdgeReissueEndpointIs404WithoutAnEdgeRow(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/edge/reissue", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

func TestEdgeProxyConfigEndpointServesTextAndIsSystemScoped(t *testing.T) {
	srv, _ := newTestServerWithACMEEdgeDir(t, nil, "")
	enableEdgeForTest(t, srv, "edge.lan")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodGet, "/api/system/certificates/edge/proxy-config", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "server {") || !strings.Contains(body, "edge.lan") {
		t.Fatalf("body does not look like the generated configuration: %s", body)
	}
	if strings.Contains(body, "PRIVATE KEY") {
		t.Fatal("the generated configuration must never contain key material")
	}

	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certAdminRequest(t, http.MethodGet, "/api/system/certificates/edge/proxy-config", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin status = %d, want 403 (the configuration reveals internal names)", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, certSystemRequest(t, http.MethodPost, "/api/system/certificates/edge/proxy-config", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", rec.Code)
	}
}

// errorCodeOf reads the apierror envelope's code so a test can pin WHICH refusal
// it got, not merely the status.
func errorCodeOf(t *testing.T, body []byte) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, body)
	}
	return env.Error.Code
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/certissue"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
	"time"
)

type fakePortalAgentCA struct {
	portal.API
	bundle      string
	err         error
	netbirdOnly bool
}

func (f fakePortalAgentCA) CertificateCABundlePEM(context.Context) (string, error) {
	return f.bundle, f.err
}

func (f fakePortalAgentCA) NetbirdOnly(context.Context) bool { return f.netbirdOnly }

// CertMeshRequireTLSChecked is off here: the mesh gate reads it on every plaintext
// agent request, so a fake driven through the agent listener must answer it.
func (f fakePortalAgentCA) CertMeshRequireTLSChecked(context.Context) bool { return false }

func testAgentCAPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "agent ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func agentCATestServer(t *testing.T, p portal.API) *Server {
	t.Helper()
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt-ca", "mock-host-qwen", "ca-secret")
	srv.Portal = p
	return srv
}

func agentCARequest(secret, etag string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/ca", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	return req
}

func TestAgentCAEndpointReturnsPublicBundleETagAndNoStore(t *testing.T) {
	bundle := testAgentCAPEM(t)
	srv := agentCATestServer(t, fakePortalAgentCA{bundle: bundle})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentCARequest("ca-secret", ""))
	if rec.Code != http.StatusOK || rec.Body.String() != bundle {
		t.Fatalf("status=%d body_matches=%v body_bytes=%d want_bytes=%d", rec.Code, rec.Body.String() == bundle, rec.Body.Len(), len(bundle))
	}
	if rec.Header().Get("ETag") == "" || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/x-pem-file" {
		t.Fatalf("headers=%v", rec.Header())
	}
}

func TestAgentCAEndpointReturns304And404Contracts(t *testing.T) {
	bundle := testAgentCAPEM(t)
	srv := agentCATestServer(t, fakePortalAgentCA{bundle: bundle})
	first := httptest.NewRecorder()
	srv.ServeHTTP(first, agentCARequest("ca-secret", ""))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentCARequest("ca-secret", first.Header().Get("ETag")))
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Fatalf("304 status=%d body_bytes=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("304 Cache-Control=%q, want no-store", rec.Header().Get("Cache-Control"))
	}

	missing := agentCATestServer(t, fakePortalAgentCA{err: portal.ErrCertificateNotFound})
	rec = httptest.NewRecorder()
	missing.ServeHTTP(rec, agentCARequest("ca-secret", ""))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "certificate.ca_not_found") {
		t.Fatalf("404 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("404 Cache-Control=%q, want no-store", rec.Header().Get("Cache-Control"))
	}

	failed := agentCATestServer(t, fakePortalAgentCA{err: errors.New("secret store detail")})
	rec = httptest.NewRecorder()
	failed.ServeHTTP(rec, agentCARequest("ca-secret", ""))
	if rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "certificate.ca_read_failed") || strings.Contains(rec.Body.String(), "secret store detail") {
		t.Fatalf("500 status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("500 Cache-Control=%q, want no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestAgentCAEndpointNeverContainsPrivateMaterial(t *testing.T) {
	public := testAgentCAPEM(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	keyPEM, _ := certissue.MarshalECKeyPEM(key)
	srv := agentCATestServer(t, fakePortalAgentCA{bundle: public + keyPEM})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentCARequest("ca-secret", ""))
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") || rec.Code == http.StatusOK {
		t.Fatalf("status=%d private_material_in_response=%v body_bytes=%d", rec.Code, strings.Contains(rec.Body.String(), "PRIVATE KEY"), rec.Body.Len())
	}
}

func TestAgentCAEndpointRejectsArbitraryPrefixBeforePublicPEM(t *testing.T) {
	const sensitivePrefix = "SENSITIVE-NON-PEM-PREFIX"
	srv := agentCATestServer(t, fakePortalAgentCA{bundle: sensitivePrefix + "\n" + testAgentCAPEM(t)})
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentCARequest("ca-secret", ""))
	leaked := strings.Contains(rec.Body.String(), sensitivePrefix)
	if rec.Code == http.StatusOK || leaked {
		t.Fatalf("status=%d prefix_leaked=%v body_bytes=%d", rec.Code, leaked, rec.Body.Len())
	}
}

func TestPublicCertificateBundleOnlyRequiresStrictPEMFraming(t *testing.T) {
	first := testAgentCAPEM(t)
	second := testAgentCAPEM(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := certissue.MarshalECKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "whitespace around public certificate", raw: " \n\t" + first + "\r\n ", want: true},
		{name: "multiple public certificates", raw: first + "\n" + second, want: true},
		{name: "non-whitespace before first block", raw: "prefix\n" + first},
		{name: "non-whitespace between blocks", raw: first + "middle\n" + second},
		{name: "non-whitespace after last block", raw: first + "suffix"},
		{name: "malformed PEM before certificate", raw: "-----BEGIN NOT-A-CERTIFICATE-----\nmalformed\n" + first},
		{name: "private key block", raw: first + keyPEM},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := publicCertificateBundleOnly([]byte(tc.raw)); got != tc.want {
				t.Fatalf("accepted=%v want=%v input_bytes=%d", got, tc.want, len(tc.raw))
			}
		})
	}
}

func TestAgentCAEndpointRoutesBothMuxesAndKeepsPublicNetbirdGate(t *testing.T) {
	bundle := testAgentCAPEM(t)
	for _, tc := range []struct {
		name      string
		netbird   bool
		agentMux  bool
		wantCode  int
		wantError string
	}{
		{name: "public available", wantCode: http.StatusOK},
		{name: "agent available", agentMux: true, wantCode: http.StatusOK},
		{name: "public gated", netbird: true, wantCode: http.StatusForbidden, wantError: "netbird.only"},
		{name: "agent bypasses public gate", netbird: true, agentMux: true, wantCode: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := agentCATestServer(t, fakePortalAgentCA{bundle: bundle, netbirdOnly: tc.netbird})
			if tc.netbird {
				srv.SetAgentListener(true, "100.64.0.5:9443")
			}
			rec := httptest.NewRecorder()
			if tc.agentMux {
				srv.AgentHandler().ServeHTTP(rec, agentCARequest("ca-secret", ""))
			} else {
				srv.ServeHTTP(rec, agentCARequest("ca-secret", ""))
			}
			errorPresent := tc.wantError != "" && strings.Contains(rec.Body.String(), tc.wantError)
			if rec.Code != tc.wantCode || (tc.wantError != "" && !errorPresent) {
				t.Fatalf("status=%d want=%d error_present=%v", rec.Code, tc.wantCode, errorPresent)
			}
		})
	}
}

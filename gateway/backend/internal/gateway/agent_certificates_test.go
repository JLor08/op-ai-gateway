// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"strings"
	"testing"
)

// fakePortalAgentCert serves one canned DTO (or error) so the handler's HTTP
// behaviour can be pinned without a full certificate store.
type fakePortalAgentCert struct {
	portal.API  // embedded nil interface; only the overridden methods are called
	dto         portal.AgentCertificateDTO
	err         error
	calls       int
	netbirdOnly bool
}

func (f *fakePortalAgentCert) AgentCertificate(_ context.Context, serverID string) (portal.AgentCertificateDTO, error) {
	f.calls++
	if f.err != nil {
		return portal.AgentCertificateDTO{}, f.err
	}
	dto := f.dto
	dto.Domain = serverID + ".test"
	return dto, nil
}

func (f *fakePortalAgentCert) NetbirdOnly(context.Context) bool { return f.netbirdOnly }

func (f *fakePortalAgentCert) CertMeshRequireTLSChecked(context.Context) bool { return false }

const (
	agentCertLeafFP = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	agentCertKeyPEM = "-----BEGIN EC PRIVATE KEY-----\nFAKEKEY\n-----END EC PRIVATE KEY-----\n"
)

func agentCertTestServer(t *testing.T, fake *fakePortalAgentCert) *Server {
	t.Helper()
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_cert", "mock-host-qwen", "cert-secret")
	srv.Portal = fake
	return srv
}

func agentCertRequest(secret, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/certificate", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

func TestAgentCertificateEndpointServesMaterial(t *testing.T) {
	fake := &fakePortalAgentCert{dto: portal.AgentCertificateDTO{
		Fingerprint: agentCertLeafFP, FullchainPEM: "-----BEGIN CERTIFICATE-----\nX\n",
		KeyPEM: agentCertKeyPEM, CABundlePEM: "-----BEGIN CERTIFICATE-----\nROOT\n",
		ETag: agentCertLeafFP + "-deadbeef",
	}}
	srv := agentCertTestServer(t, fake)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["key_pem"] != agentCertKeyPEM {
		t.Fatalf("key_pem = %v, want the private key (this endpoint exists to deliver it)", body["key_pem"])
	}
	if body["fullchain_pem"] == "" || body["ca_bundle_pem"] == "" {
		t.Fatalf("body = %v, want chain + bundle", body)
	}
	if got := rec.Header().Get("ETag"); got != `"`+fake.dto.ETag+`"` {
		t.Fatalf("ETag = %q, want the quoted opaque validator", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store -- the body carries a private key and the "+
			"shipped nginx configs serve this path through a generic location /api/ block", got)
	}
}

func TestAgentCertificateEndpointConditionalGet(t *testing.T) {
	etag := agentCertLeafFP + "-deadbeef"
	cases := []struct {
		name        string
		ifNoneMatch string
		wantStatus  int
	}{
		{"exact quoted", `"` + etag + `"`, http.StatusNotModified},
		{"weak prefix", `W/"` + etag + `"`, http.StatusNotModified},
		{"comma list", `"other", "` + etag + `"`, http.StatusNotModified},
		{"unquoted", etag, http.StatusNotModified},
		{"star", "*", http.StatusNotModified},
		{"different", `"` + strings.Repeat("0", 64) + `-x"`, http.StatusOK},
		{"absent", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePortalAgentCert{dto: portal.AgentCertificateDTO{
				Fingerprint: agentCertLeafFP, FullchainPEM: "chain", KeyPEM: agentCertKeyPEM, ETag: etag,
			}}
			srv := agentCertTestServer(t, fake)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, agentCertRequest("cert-secret", tc.ifNoneMatch))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusNotModified {
				if rec.Body.Len() != 0 {
					t.Fatalf("304 carried a body: %s", rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), agentCertKeyPEM) && rec.Header().Get("ETag") == "" {
					t.Fatal("304 must still carry the ETag")
				}
			}
		})
	}
}

func TestAgentCertificateEndpointAuthAndErrors(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		srv := agentCertTestServer(t, &fakePortalAgentCert{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		fake := &fakePortalAgentCert{}
		srv := agentCertTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("not-a-token", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if fake.calls != 0 {
			t.Fatal("the portal was consulted for an unauthenticated caller")
		}
	})
	t.Run("not found", func(t *testing.T) {
		srv := agentCertTestServer(t, &fakePortalAgentCert{err: portal.ErrCertificateNotFound})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "certificate.not_found") {
			t.Fatalf("body = %s, want certificate.not_found", rec.Body.String())
		}
	})
	t.Run("cert key required", func(t *testing.T) {
		srv := agentCertTestServer(t, &fakePortalAgentCert{err: portal.ErrCertKeyRequired})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "system.cert_key_required") {
			t.Fatalf("body = %s, want system.cert_key_required (the operator must learn which variable is missing)", rec.Body.String())
		}
	})
	t.Run("opaque store error", func(t *testing.T) {
		srv := agentCertTestServer(t, &fakePortalAgentCert{err: context.DeadlineExceeded})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), context.DeadlineExceeded.Error()) {
			t.Fatalf("body leaked the underlying error: %s", rec.Body.String())
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		srv := agentCertTestServer(t, &fakePortalAgentCert{})
		req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/certificate", nil)
		req.Header.Set("Authorization", "Bearer cert-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

// The public-mux route mirrors the telemetry/stream gate exactly, including the
// fail-safe: with no agent listener bound the public path always serves, so a UI
// toggle can never cut every agent off from its certificate.
func TestAgentCertificateEndpointNetbirdOnlyGate(t *testing.T) {
	dto := portal.AgentCertificateDTO{Fingerprint: agentCertLeafFP, FullchainPEM: "chain", KeyPEM: agentCertKeyPEM, ETag: "e"}

	t.Run("listener active and netbird_only on -> 403", func(t *testing.T) {
		fake := &fakePortalAgentCert{dto: dto, netbirdOnly: true}
		srv := agentCertTestServer(t, fake)
		srv.SetAgentListener(true, "100.64.0.5:8081")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "netbird.only") {
			t.Fatalf("body = %s, want netbird.only", rec.Body.String())
		}
	})
	t.Run("no listener -> serves despite netbird_only", func(t *testing.T) {
		fake := &fakePortalAgentCert{dto: dto, netbirdOnly: true}
		srv := agentCertTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail-safe: no agent listener means the public path must serve)", rec.Code)
		}
	})
	t.Run("agent mux is never gated", func(t *testing.T) {
		fake := &fakePortalAgentCert{dto: dto, netbirdOnly: true}
		srv := agentCertTestServer(t, fake)
		srv.SetAgentListener(true, "100.64.0.5:8081")
		rec := httptest.NewRecorder()
		srv.AgentHandler().ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 on the mesh listener (body %s)", rec.Code, rec.Body.String())
		}
	})
}

// The audit trail: exactly ONE line per key actually handed out, none on a 304, and
// never the key, the chain or the token in any record.
func TestAgentCertificateEndpointAuditLogging(t *testing.T) {
	etag := agentCertLeafFP + "-deadbeef"
	dto := portal.AgentCertificateDTO{
		Fingerprint: agentCertLeafFP, FullchainPEM: "-----BEGIN CERTIFICATE-----\nCHAINBYTES\n",
		KeyPEM: agentCertKeyPEM, CABundlePEM: "rootbytes", ETag: etag,
	}

	t.Run("one line per served key, no material", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		defer restore()
		srv := agentCertTestServer(t, &fakePortalAgentCert{dto: dto})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		recs := buf.Snapshot()
		served := 0
		for _, r := range recs {
			if r.Level == "INFO" && strings.Contains(r.Msg, "agent certificate served") {
				served++
			}
		}
		if served != 1 {
			t.Fatalf("audit lines = %d, want exactly 1", served)
		}
		assertNoSecretInLogs(t, recs, "cert-secret")
		assertNoSecretInLogs(t, recs, agentCertKeyPEM)
		assertNoSecretInLogs(t, recs, "CHAINBYTES")
	})

	t.Run("no audit line on 304", func(t *testing.T) {
		buf, restore := withCapturedSlog(t)
		defer restore()
		srv := agentCertTestServer(t, &fakePortalAgentCert{dto: dto})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentCertRequest("cert-secret", `"`+etag+`"`))
		if rec.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rec.Code)
		}
		for _, r := range buf.Snapshot() {
			if r.Level == "INFO" && strings.Contains(r.Msg, "agent certificate served") {
				t.Fatal("a 304 produced an audit line although no key was handed out")
			}
		}
	})
}

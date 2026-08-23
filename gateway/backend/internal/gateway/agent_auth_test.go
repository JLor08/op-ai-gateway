// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

type agentAuthFailingStore struct {
	routing.Store
	err error
}

type agentAuthEndpoint struct {
	name    string
	method  string
	path    string
	body    string
	handler func(*Server, http.ResponseWriter, *http.Request)
}

func agentAuthEndpoints() []agentAuthEndpoint {
	return []agentAuthEndpoint{
		{name: "telemetry", method: http.MethodPost, path: "/api/agent/v1/telemetry", body: validIngestAgentBody, handler: (*Server).handleAgentTelemetry},
		{name: "system report", method: http.MethodPost, path: "/api/agent/v1/system-report", body: validSystemReportBody, handler: (*Server).handleAgentSystemReport},
		{name: "stream", method: http.MethodGet, path: "/api/agent/v1/stream", handler: (*Server).handleAgentStream},
		{name: "download", method: http.MethodGet, path: "/api/agent/v1/download/config", handler: (*Server).handleAgentDownload},
		{name: "certificate", method: http.MethodGet, path: "/api/agent/v1/certificate", handler: (*Server).handleAgentCertificate},
		{name: "ca", method: http.MethodGet, path: "/api/agent/v1/ca", handler: (*Server).handleAgentCA},
	}
}

func (s agentAuthFailingStore) LookupAgentToken(context.Context, string) (string, bool, error) {
	return "", false, s.err
}

func TestAgentAuthMissingInvalidStoreErrorAndValidPrincipal(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		srv := NewTestServer()
		rec := httptest.NewRecorder()
		_, ok := srv.authenticateAgent(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if ok || rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "auth.invalid_token") {
			t.Fatalf("ok=%v status=%d body=%s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("invalid", func(t *testing.T) {
		srv := NewTestServer()
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		rec := httptest.NewRecorder()
		_, ok := srv.authenticateAgent(rec, req)
		if ok || rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "auth.invalid_token") {
			t.Fatalf("ok=%v status=%d body=%s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("store error", func(t *testing.T) {
		srv := NewTestServer()
		srv.Routes = agentAuthFailingStore{Store: srv.Routes, err: errors.New("store unavailable")}
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer anything")
		rec := httptest.NewRecorder()
		_, ok := srv.authenticateAgent(rec, req)
		if ok || rec.Code != http.StatusInternalServerError || !strings.Contains(rec.Body.String(), "agent.token_lookup_failed") {
			t.Fatalf("ok=%v status=%d body=%s", ok, rec.Code, rec.Body.String())
		}
	})
	t.Run("valid", func(t *testing.T) {
		srv := NewTestServer()
		seedTestAgentToken(t, srv, "agt-auth", "mock-host-qwen", "auth-secret")
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer auth-secret")
		rec := httptest.NewRecorder()
		principal, ok := srv.authenticateAgent(rec, req)
		if !ok || principal.ServerID != "mock-host-qwen" || principal.Secret != "auth-secret" {
			t.Fatalf("principal=%+v ok=%v", principal, ok)
		}
	})
}

func TestAgentAuthContextMarkerOnlyOnMeshListener(t *testing.T) {
	srv := NewTestServer()
	for _, tc := range []struct {
		name   string
		public bool
		want   bool
	}{{"public", true, false}, {"mesh", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			seen := false
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = isAgentListenerRequest(r.Context())
				w.WriteHeader(http.StatusNoContent)
			})
			rec := httptest.NewRecorder()
			srv.serveWith(rec, httptest.NewRequest(http.MethodGet, "/probe", nil), h, tc.public)
			if seen != tc.want {
				t.Fatalf("mesh marker=%v, want %v", seen, tc.want)
			}
		})
	}
}

func TestAgentAuthContractsAcrossAllAgentEndpoints(t *testing.T) {
	for _, endpoint := range agentAuthEndpoints() {
		for _, authCase := range []struct {
			name       string
			header     string
			storeError bool
			status     int
			code       string
		}{
			{name: "missing", status: http.StatusUnauthorized, code: "auth.invalid_token"},
			{name: "invalid", header: "Bearer invalid-agent-secret", status: http.StatusUnauthorized, code: "auth.invalid_token"},
			{name: "store error", header: "Bearer lookup-fails", storeError: true, status: http.StatusInternalServerError, code: "agent.token_lookup_failed"},
		} {
			t.Run(endpoint.name+"/"+authCase.name, func(t *testing.T) {
				srv := NewTestServer()
				if authCase.storeError {
					srv.Routes = agentAuthFailingStore{Store: srv.Routes, err: errors.New("store unavailable")}
				}
				req := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(endpoint.body))
				if authCase.header != "" {
					req.Header.Set("Authorization", authCase.header)
				}
				rec := httptest.NewRecorder()
				endpoint.handler(srv, rec, req)
				codePresent := strings.Contains(rec.Body.String(), authCase.code)
				if rec.Code != authCase.status || !codePresent {
					t.Fatalf("status=%d want=%d code_present=%v", rec.Code, authCase.status, codePresent)
				}
				if endpoint.name == "ca" && rec.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("CA auth failure Cache-Control=%q, want no-store", rec.Header().Get("Cache-Control"))
				}
			})
		}
	}
}

func TestAgentAuthValidPrincipalDerivesServerAndOnlyConfigReturnsSecret(t *testing.T) {
	const secret = "auth-secret-must-not-escape"
	for _, endpoint := range agentAuthEndpoints() {
		t.Run(endpoint.name, func(t *testing.T) {
			srv := NewTestServer()
			seedTestAgentToken(t, srv, "agt-auth-endpoint", "mock-host-qwen", secret)
			// Certificate/CA handlers should prove authentication without needing
			// response material that could contain PEM or private keys.
			srv.Portal = nil
			req := httptest.NewRequest(endpoint.method, endpoint.path, strings.NewReader(endpoint.body))
			req.Header.Set("Authorization", "Bearer "+secret)
			rec := httptest.NewRecorder()
			endpoint.handler(srv, rec, req)
			secretPresent := strings.Contains(rec.Body.String(), secret)
			wantSecret := endpoint.name == "download"
			if secretPresent != wantSecret {
				t.Fatalf("secret_in_response=%v want=%v status=%d", secretPresent, wantSecret, rec.Code)
			}
			if (endpoint.name == "telemetry" || endpoint.name == "system report") && !strings.Contains(rec.Body.String(), `"server_id":"mock-host-qwen"`) {
				t.Fatalf("token-derived server ID missing: status=%d", rec.Code)
			}
		})
	}
}

// TestAuthenticateAgentRecordsOnlyMeshListenerTransport pins the mesh-only
// contract: a successful public-listener auth (a proxied request) must NEVER
// stamp the transport registry, and a successful mesh-listener auth must always
// stamp it -- with TLS iff r.TLS is set. The mesh gate's arming precondition
// depends on this: a proxied hop that already went through the fronting reverse
// proxy would misrepresent the true agent transport.
func TestAuthenticateAgentRecordsOnlyMeshListenerTransport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		public     bool
		tls        bool
		wantSeen   bool
		wantTLSNow bool
	}{
		{name: "public plain: no observation", public: true, tls: false, wantSeen: false},
		{name: "public tls: no observation (proxy hop, not agent hop)", public: true, tls: true, wantSeen: false},
		{name: "mesh plain: observed as plaintext", public: false, tls: false, wantSeen: true, wantTLSNow: false},
		{name: "mesh tls: observed as TLS", public: false, tls: true, wantSeen: true, wantTLSNow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := NewTestServer()
			seedTestAgentToken(t, srv, "agt-transport", "mock-host-qwen", "transport-secret")
			called := false
			h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				_, ok := srv.authenticateAgent(w, r)
				if !ok {
					t.Fatal("authenticate failed under fully valid credentials")
				}
			})
			req := httptest.NewRequest(http.MethodGet, "/probe", nil)
			req.Header.Set("Authorization", "Bearer transport-secret")
			if tc.tls {
				req.TLS = &tls.ConnectionState{}
			}
			rec := httptest.NewRecorder()
			srv.serveWith(rec, req, h, tc.public)
			if !called {
				t.Fatalf("handler was not invoked; status=%d body=%s", rec.Code, rec.Body.String())
			}
			transport, _, ok := srv.AgentTransport.LatestTransport("mock-host-qwen")
			if ok != tc.wantSeen {
				t.Fatalf("observation seen=%v, want %v (transport=%q)", ok, tc.wantSeen, transport)
			}
			if tc.wantSeen && ((transport == "tls") != tc.wantTLSNow) {
				t.Fatalf("transport=%q, want tls=%v", transport, tc.wantTLSNow)
			}
		})
	}
}

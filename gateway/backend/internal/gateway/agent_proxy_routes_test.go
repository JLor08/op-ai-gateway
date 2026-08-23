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

// fakePortalAgentProxyRoutes serves one canned DTO (or error) so the handler's
// HTTP behaviour (auth, ETag/304, mux gating) can be pinned without a full
// routing store -- the derivation itself (scope, port assignment) is unit
// tested at the portal-service layer (service_agent_proxy_routes_test.go).
type fakePortalAgentProxyRoutes struct {
	portal.API // embedded nil interface; only the overridden methods are called
	dto        portal.AgentProxyRoutesDTO
	err        error
	calls      int
	netbird    bool
}

func (f *fakePortalAgentProxyRoutes) AgentProxyRoutes(_ context.Context, _ string) (portal.AgentProxyRoutesDTO, error) {
	f.calls++
	if f.err != nil {
		return portal.AgentProxyRoutesDTO{}, f.err
	}
	return f.dto, nil
}

func (f *fakePortalAgentProxyRoutes) NetbirdOnly(context.Context) bool { return f.netbird }

// CertMeshRequireTLSChecked is off here: the mesh gate reads it on every
// plaintext agent request, so a fake driven through the agent listener must
// answer it.
func (f *fakePortalAgentProxyRoutes) CertMeshRequireTLSChecked(context.Context) bool { return false }

func agentProxyRoutesTestServer(t *testing.T, fake *fakePortalAgentProxyRoutes) *Server {
	t.Helper()
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_proxy_routes", "mock-host-qwen", "routes-secret")
	srv.Portal = fake
	return srv
}

func agentProxyRoutesRequest(secret, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/proxy-routes", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

func twoCandidateRoutesDTO() portal.AgentProxyRoutesDTO {
	return portal.AgentProxyRoutesDTO{
		Routes: []portal.AgentProxyRouteDTO{
			{Listen: 8600, Upstream: "http://127.0.0.1:8080", AppID: "app-1"},
			{Listen: 8601, Upstream: "http://127.0.0.1:8081", AppID: "app-2"},
		},
		ETag: "cafef00d",
	}
}

// TestAgentProxyRoutesEndpoint pins Certificates P4 Task 7's Step 1 contract: an
// authed agent whose server is in switch scope with two proxy-candidate apps gets
// back routes=[{listen,upstream,app_id},...] plus a stable ETag, and a repeat
// request with that ETag answers 304.
func TestAgentProxyRoutesEndpoint(t *testing.T) {
	dto := twoCandidateRoutesDTO()
	fake := &fakePortalAgentProxyRoutes{dto: dto}
	srv := agentProxyRoutesTestServer(t, fake)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body portal.AgentProxyRoutesDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2 entries", body.Routes)
	}
	if body.Routes[0].Listen != 8600 || body.Routes[0].Upstream != "http://127.0.0.1:8080" || body.Routes[0].AppID != "app-1" {
		t.Fatalf("routes[0] = %+v, want {8600 http://127.0.0.1:8080 app-1}", body.Routes[0])
	}
	if body.Routes[1].Listen != 8601 || body.Routes[1].Upstream != "http://127.0.0.1:8081" || body.Routes[1].AppID != "app-2" {
		t.Fatalf("routes[1] = %+v, want {8601 http://127.0.0.1:8081 app-2}", body.Routes[1])
	}
	if body.ETag != dto.ETag {
		t.Fatalf("etag = %q, want %q", body.ETag, dto.ETag)
	}
	wantHeader := `"` + dto.ETag + `"`
	if got := rec.Header().Get("ETag"); got != wantHeader {
		t.Fatalf("ETag header = %q, want %q", got, wantHeader)
	}

	// A repeat request carrying that ETag is unchanged -> 304, no body.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, agentProxyRoutesRequest("routes-secret", wantHeader))
	if rec2.Code != http.StatusNotModified || rec2.Body.Len() != 0 {
		t.Fatalf("304 status = %d, body_bytes = %d", rec2.Code, rec2.Body.Len())
	}
	if got := rec2.Header().Get("ETag"); got != wantHeader {
		t.Fatalf("304 must still carry the ETag; got %q", got)
	}

	if fake.calls != 2 {
		t.Fatalf("portal calls = %d, want 2", fake.calls)
	}
}

// TestAgentProxyRoutesEndpointCacheControlNoStore mirrors agent_ca_test.go's
// Cache-Control assertion: this is a Bearer-token agent endpoint whose
// default-config traffic flows through the public listener behind fronting
// infra, so EVERY response path -- success and an auth failure alike -- must
// carry Cache-Control: no-store, set before requireMethod/authenticateAgent
// run.
func TestAgentProxyRoutesEndpointCacheControlNoStore(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := &fakePortalAgentProxyRoutes{dto: twoCandidateRoutesDTO()}
		srv := agentProxyRoutesTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})
	t.Run("auth failure", func(t *testing.T) {
		srv := agentProxyRoutesTestServer(t, &fakePortalAgentProxyRoutes{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentProxyRoutesRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store even on an auth failure", got)
		}
	})
}

// TestAgentProxyRoutesEndpointEmptyScope pins the out-of-scope/manual-mode
// contract: an empty route list is a normal 200 with routes:[] (never null),
// never an error -- "the agent then runs no proxy".
func TestAgentProxyRoutesEndpointEmptyScope(t *testing.T) {
	fake := &fakePortalAgentProxyRoutes{dto: portal.AgentProxyRoutesDTO{Routes: []portal.AgentProxyRouteDTO{}, ETag: "empty"}}
	srv := agentProxyRoutesTestServer(t, fake)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"routes":[]`) {
		t.Fatalf("body = %s, want an empty (never null) routes array", rec.Body.String())
	}
}

func TestAgentProxyRoutesEndpointConditionalGet(t *testing.T) {
	etag := "deadbeefcafe"
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
		{"different", `"different-etag"`, http.StatusOK},
		{"absent", "", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePortalAgentProxyRoutes{dto: portal.AgentProxyRoutesDTO{
				Routes: []portal.AgentProxyRouteDTO{{Listen: 8600, Upstream: "http://127.0.0.1:8080", AppID: "app-1"}},
				ETag:   etag,
			}}
			srv := agentProxyRoutesTestServer(t, fake)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", tc.ifNoneMatch))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusNotModified && rec.Body.Len() != 0 {
				t.Fatalf("304 carried a body: %s", rec.Body.String())
			}
		})
	}
}

func TestAgentProxyRoutesEndpointAuthAndErrors(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		srv := agentProxyRoutesTestServer(t, &fakePortalAgentProxyRoutes{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentProxyRoutesRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		fake := &fakePortalAgentProxyRoutes{}
		srv := agentProxyRoutesTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentProxyRoutesRequest("not-a-token", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if fake.calls != 0 {
			t.Fatal("the portal was consulted for an unauthenticated caller")
		}
	})
	t.Run("opaque store error", func(t *testing.T) {
		srv := agentProxyRoutesTestServer(t, &fakePortalAgentProxyRoutes{err: context.DeadlineExceeded})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), context.DeadlineExceeded.Error()) {
			t.Fatalf("body leaked the underlying error: %s", rec.Body.String())
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		srv := agentProxyRoutesTestServer(t, &fakePortalAgentProxyRoutes{})
		req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/proxy-routes", nil)
		req.Header.Set("Authorization", "Bearer routes-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

// TestAgentProxyRoutesEndpointRoutesBothMuxesAndKeepsPublicNetbirdGate mirrors
// the sibling agent_ca.go test exactly: on the public mux the route is gated
// by netbird_only (only when an agent listener is actually active -- the
// fail-safe), and it is always reachable, ungated, on the agent mux.
func TestAgentProxyRoutesEndpointRoutesBothMuxesAndKeepsPublicNetbirdGate(t *testing.T) {
	dto := twoCandidateRoutesDTO()
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
			fake := &fakePortalAgentProxyRoutes{dto: dto, netbird: tc.netbird}
			srv := agentProxyRoutesTestServer(t, fake)
			if tc.netbird {
				srv.SetAgentListener(true, "100.64.0.5:9443")
			}
			rec := httptest.NewRecorder()
			if tc.agentMux {
				srv.AgentHandler().ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
			} else {
				srv.ServeHTTP(rec, agentProxyRoutesRequest("routes-secret", ""))
			}
			errorPresent := tc.wantError != "" && strings.Contains(rec.Body.String(), tc.wantError)
			if rec.Code != tc.wantCode || (tc.wantError != "" && !errorPresent) {
				t.Fatalf("status=%d want=%d error_present=%v", rec.Code, tc.wantCode, errorPresent)
			}
		})
	}
}

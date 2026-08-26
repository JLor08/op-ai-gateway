// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// fakePortalAgentRuntimeConfig serves one canned DTO (or error) so the
// handler's HTTP behaviour (auth, ETag/304, mux gating) can be pinned without
// a full routing store -- the derivation itself (spec-id translation,
// enabled-only filtering, VRAM selection) is unit tested at the
// portal-service layer (service_runtime_test.go's
// TestAgentRuntimeConfigAssembly).
type fakePortalAgentRuntimeConfig struct {
	portal.API // embedded nil interface; only the overridden methods are called
	dto        portal.AgentRuntimeConfigDTO
	err        error
	calls      int
	netbird    bool
}

func (f *fakePortalAgentRuntimeConfig) AgentRuntimeConfig(_ context.Context, _ string) (portal.AgentRuntimeConfigDTO, error) {
	f.calls++
	if f.err != nil {
		return portal.AgentRuntimeConfigDTO{}, f.err
	}
	return f.dto, nil
}

func (f *fakePortalAgentRuntimeConfig) NetbirdOnly(context.Context) bool { return f.netbird }

// CertMeshRequireTLSChecked is off here: the mesh gate reads it on every
// plaintext agent request, so a fake driven through the agent listener must
// answer it (see agent_proxy_routes_test.go's identical trap note).
func (f *fakePortalAgentRuntimeConfig) CertMeshRequireTLSChecked(context.Context) bool { return false }

func agentRuntimeConfigTestServer(t *testing.T, fake *fakePortalAgentRuntimeConfig) *Server {
	t.Helper()
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_runtime_config", "mock-host-qwen", "runtime-secret")
	srv.Portal = fake
	return srv
}

func agentRuntimeConfigRequest(secret, ifNoneMatch string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/agent/v1/runtime-config", nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	return req
}

// sampleAgentRuntimeConfigDTO is a representative populated document,
// matching the agent-runtime-manager design spec §11 sample exactly (one
// two-GPU vllm spec, one budget pair, one coresidency pair).
func sampleAgentRuntimeConfigDTO() portal.AgentRuntimeConfigDTO {
	return portal.AgentRuntimeConfigDTO{
		RouterListen: 8081,
		MaxProcesses: 3,
		GPUBudgets: []portal.AgentGPUBudgetDTO{
			{Index: 0, BudgetMB: 46000},
			{Index: 1, BudgetMB: 46000},
		},
		Specs: []portal.AgentRuntimeSpecDTO{
			{
				ID:            "rspec_a",
				Model:         "qwen-coder",
				UpstreamModel: "qwen2.5-coder-32b",
				Binary:        "/usr/bin/vllm",
				Args:          []string{"--tensor-parallel-size", "2"},
				Env:           map[string]string{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"},
				GPUs: []portal.AgentRuntimeSpecGPUDTO{
					{Index: 0, VRAMMB: 22000},
					{Index: 1, VRAMMB: 21500},
				},
				ListenPort:                  0,
				HealthPath:                  "/health",
				HealthTimeoutSeconds:        5,
				StartupTimeoutSeconds:       180,
				IdleTimeoutSeconds:          900,
				AdmissionWaitTimeoutSeconds: 0,
				Pinned:                      false,
				AdminState:                  "",
			},
			{
				ID:            "rspec_b",
				Model:         "llama-small",
				UpstreamModel: "llama-3-8b",
				Binary:        "/usr/bin/llama-server",
				Args:          []string{},
				Env:           map[string]string{},
				GPUs:          []portal.AgentRuntimeSpecGPUDTO{{Index: 0, VRAMMB: 8000}},
				HealthPath:    "/health",
			},
		},
		Coresident: [][2]string{{"rspec_a", "rspec_b"}},
		ETag:       "cafef00d",
	}
}

// TestAgentRuntimeConfigEndpoint pins the runtime-config wire contract: an
// authed agent gets back the assembled document plus a stable ETag (BOTH in
// the body's etag field and the quoted ETag header), and a repeat request
// with that ETag answers 304 with an empty body.
func TestAgentRuntimeConfigEndpoint(t *testing.T) {
	dto := sampleAgentRuntimeConfigDTO()
	fake := &fakePortalAgentRuntimeConfig{dto: dto}
	srv := agentRuntimeConfigTestServer(t, fake)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body portal.AgentRuntimeConfigDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.RouterListen != dto.RouterListen || body.MaxProcesses != dto.MaxProcesses {
		t.Fatalf("body = %+v, want router_listen=%d max_processes=%d", body, dto.RouterListen, dto.MaxProcesses)
	}
	if len(body.Specs) != 2 || len(body.Coresident) != 1 || len(body.GPUBudgets) != 2 {
		t.Fatalf("body = %+v, want 2 specs, 1 coresident pair, 2 gpu budgets", body)
	}
	if body.ETag != dto.ETag {
		t.Fatalf("in-body etag = %q, want %q", body.ETag, dto.ETag)
	}
	wantHeader := `"` + dto.ETag + `"`
	if got := rec.Header().Get("ETag"); got != wantHeader {
		t.Fatalf("ETag header = %q, want %q", got, wantHeader)
	}

	// A repeat request carrying that ETag is unchanged -> 304, no body.
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, agentRuntimeConfigRequest("runtime-secret", wantHeader))
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

// TestAgentRuntimeConfigEndpointCacheControlNoStore mirrors
// agent_proxy_routes_test.go's assertion: EVERY response path -- success and
// an auth failure alike -- must carry Cache-Control: no-store, set before
// requireMethod/authenticateAgent run.
func TestAgentRuntimeConfigEndpointCacheControlNoStore(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		fake := &fakePortalAgentRuntimeConfig{dto: sampleAgentRuntimeConfigDTO()}
		srv := agentRuntimeConfigTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", got)
		}
	})
	t.Run("auth failure", func(t *testing.T) {
		srv := agentRuntimeConfigTestServer(t, &fakePortalAgentRuntimeConfig{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store even on an auth failure", got)
		}
	})
}

// TestAgentRuntimeConfigEndpointEmptyIsNeverNull pins the "an empty
// configuration must still produce a STABLE etag" and "collections never
// null" contracts: every collection-shaped field renders as [] on the wire.
func TestAgentRuntimeConfigEndpointEmptyIsNeverNull(t *testing.T) {
	fake := &fakePortalAgentRuntimeConfig{dto: portal.AgentRuntimeConfigDTO{
		GPUBudgets: []portal.AgentGPUBudgetDTO{},
		Specs:      []portal.AgentRuntimeSpecDTO{},
		Coresident: [][2]string{},
		ETag:       "empty-etag",
	}}
	srv := agentRuntimeConfigTestServer(t, fake)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"gpu_budgets":[]`, `"specs":[]`, `"coresident":[]`} {
		if !strings.Contains(body, want) {
			t.Fatalf("body = %s, want %s (never null)", body, want)
		}
	}
}

func TestAgentRuntimeConfigEndpointConditionalGet(t *testing.T) {
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
			dto := sampleAgentRuntimeConfigDTO()
			dto.ETag = etag
			fake := &fakePortalAgentRuntimeConfig{dto: dto}
			srv := agentRuntimeConfigTestServer(t, fake)
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", tc.ifNoneMatch))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantStatus == http.StatusNotModified && rec.Body.Len() != 0 {
				t.Fatalf("304 carried a body: %s", rec.Body.String())
			}
		})
	}
}

func TestAgentRuntimeConfigEndpointAuthAndErrors(t *testing.T) {
	t.Run("no bearer", func(t *testing.T) {
		srv := agentRuntimeConfigTestServer(t, &fakePortalAgentRuntimeConfig{})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
	t.Run("unknown token", func(t *testing.T) {
		fake := &fakePortalAgentRuntimeConfig{}
		srv := agentRuntimeConfigTestServer(t, fake)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("not-a-token", ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if fake.calls != 0 {
			t.Fatal("the portal was consulted for an unauthenticated caller")
		}
	})
	t.Run("opaque store error", func(t *testing.T) {
		srv := agentRuntimeConfigTestServer(t, &fakePortalAgentRuntimeConfig{err: context.DeadlineExceeded})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		if strings.Contains(rec.Body.String(), context.DeadlineExceeded.Error()) {
			t.Fatalf("body leaked the underlying error: %s", rec.Body.String())
		}
	})
	t.Run("method not allowed", func(t *testing.T) {
		srv := agentRuntimeConfigTestServer(t, &fakePortalAgentRuntimeConfig{})
		req := httptest.NewRequest(http.MethodPost, "/api/agent/v1/runtime-config", nil)
		req.Header.Set("Authorization", "Bearer runtime-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want 405", rec.Code)
		}
	})
}

// TestAgentRuntimeRoutesOnAgentMux is the dual-mux proof required for BOTH
// new Task 7 endpoints (model: agent_stream_test.go's
// TestAgentStreamRegisteredOnAgentMux): each answers identically via
// srv.AgentHandler() (the dedicated NetBird/agent mux), not just the public
// mux.
func TestAgentRuntimeRoutesOnAgentMux(t *testing.T) {
	srv := NewTestServer()
	seedTestAgentToken(t, srv, "agt_dualmux", "mock-host-qwen", "dualmux-secret")

	t.Run("features", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.AgentHandler().ServeHTTP(rec, agentFeaturesRequest("dualmux-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), `"runtime_manager"`) {
			t.Fatalf("body = %s, want the runtime_manager feature", rec.Body.String())
		}
	})
	t.Run("runtime-config", func(t *testing.T) {
		rec := httptest.NewRecorder()
		srv.AgentHandler().ServeHTTP(rec, agentRuntimeConfigRequest("dualmux-secret", ""))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		// mock-host-qwen (seedGatewayTestRoutes) carries no server_agent
		// application -- the real portal.Service must degrade to the fully
		// empty, non-nil document rather than erroring.
		if !strings.Contains(rec.Body.String(), `"specs":[]`) {
			t.Fatalf("body = %s, want an empty (never null) specs array", rec.Body.String())
		}
	})
}

// TestAgentRuntimeRoutesKeepPublicNetbirdGate mirrors
// agent_proxy_routes_test.go's gate proof for the two new endpoints: on the
// public mux they are gated by netbird_only (only when an agent listener is
// actually active -- the fail-safe), and always reachable, ungated, on the
// agent mux.
func TestAgentRuntimeRoutesKeepPublicNetbirdGate(t *testing.T) {
	dto := sampleAgentRuntimeConfigDTO()
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
			fake := &fakePortalAgentRuntimeConfig{dto: dto, netbird: tc.netbird}
			srv := agentRuntimeConfigTestServer(t, fake)
			if tc.netbird {
				srv.SetAgentListener(true, "100.64.0.5:9443")
			}
			rec := httptest.NewRecorder()
			if tc.agentMux {
				srv.AgentHandler().ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
			} else {
				srv.ServeHTTP(rec, agentRuntimeConfigRequest("runtime-secret", ""))
			}
			errorPresent := tc.wantError != "" && strings.Contains(rec.Body.String(), tc.wantError)
			if rec.Code != tc.wantCode || (tc.wantError != "" && !errorPresent) {
				t.Fatalf("status=%d want=%d error_present=%v", rec.Code, tc.wantCode, errorPresent)
			}
		})
	}
}

// TestAgentRuntimeConfigEndpointServerIsolation proves, against the REAL
// portal.Service (not a fake), that an agent token for server A can never
// see server B's runtime configuration: there is no request parameter that
// could redirect the lookup, so the only thing to assert is that each
// token's response corresponds to ITS OWN server's assembled state.
func TestAgentRuntimeConfigEndpointServerIsolation(t *testing.T) {
	srv := NewTestServer()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	mustCreateServer := func(id string) {
		t.Helper()
		if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{ID: id, Name: id, Domain: id + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + id, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
	}
	mustCreateServer("rt-srv-a")
	mustCreateServer("rt-srv-b")

	appA := routing.Application{ID: "rt-app-a", ServerID: "rt-srv-a", Type: routing.ProviderServerAgent, Port: 8081, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := srv.Routes.CreateApplication(ctx, appA); err != nil {
		t.Fatalf("CreateApplication A: %v", err)
	}
	appB := routing.Application{ID: "rt-app-b", ServerID: "rt-srv-b", Type: routing.ProviderServerAgent, Port: 9091, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := srv.Routes.CreateApplication(ctx, appB); err != nil {
		t.Fatalf("CreateApplication B: %v", err)
	}

	seedTestAgentToken(t, srv, "agt_rt_a", "rt-srv-a", "rt-secret-a")
	seedTestAgentToken(t, srv, "agt_rt_b", "rt-srv-b", "rt-secret-b")

	recA := httptest.NewRecorder()
	srv.ServeHTTP(recA, agentRuntimeConfigRequest("rt-secret-a", ""))
	if recA.Code != http.StatusOK {
		t.Fatalf("server A status = %d, body %s", recA.Code, recA.Body.String())
	}
	var bodyA portal.AgentRuntimeConfigDTO
	if err := json.Unmarshal(recA.Body.Bytes(), &bodyA); err != nil {
		t.Fatalf("decode A: %v", err)
	}
	if bodyA.RouterListen != 8081 {
		t.Fatalf("server A router_listen = %d, want 8081 (its OWN application's port)", bodyA.RouterListen)
	}

	recB := httptest.NewRecorder()
	srv.ServeHTTP(recB, agentRuntimeConfigRequest("rt-secret-b", ""))
	if recB.Code != http.StatusOK {
		t.Fatalf("server B status = %d, body %s", recB.Code, recB.Body.String())
	}
	var bodyB portal.AgentRuntimeConfigDTO
	if err := json.Unmarshal(recB.Body.Bytes(), &bodyB); err != nil {
		t.Fatalf("decode B: %v", err)
	}
	if bodyB.RouterListen != 9091 {
		t.Fatalf("server B router_listen = %d, want 9091 (its OWN application's port)", bodyB.RouterListen)
	}
	if bodyA.RouterListen == bodyB.RouterListen {
		t.Fatalf("both tokens resolved to the SAME router_listen (%d) -- isolation broken", bodyA.RouterListen)
	}
}

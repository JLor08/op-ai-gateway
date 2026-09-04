// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// newServiceScopeTestServer builds a gateway test server for the service-account
// "LLM-only" auth tests (service accounts, Phase 1, Task 2). It seeds:
//   - the standard mock route "qwen-coder" via seedGatewayTestRoutes (routable on
//     BOTH the OpenAI and Anthropic api flavors, translate path only), used for the
//     three-inference-handler pass-through tests and the translate-path allowlist
//     gate tests;
//   - a second application/mapping ("native-model") with ResponsesMode=passthrough,
//     used for the native-passthrough allowlist gate test.
//
// A service token is never store-backed in Task 2 (that lands in Task 3's
// Service/ServiceToken CRUD); it is added directly to the auth.TokenStore, exactly
// how the real store-backed BearerStore will resolve one once wired — the gateway
// layer only cares that auth.LookupBearer returns an auth.Token with
// Kind=="service".
func newServiceScopeTestServer(t *testing.T) (*Server, *auth.TokenStore, *usage.Recorder) {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("seed admin token: %v", err)
	}

	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)

	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-native-svc", Name: "Native Upstream", Domain: "native-svc.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://native-svc.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed native server: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-native-svc", ServerID: "srv-native-svc", Type: routing.ProviderVLLM, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed native app: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-native-svc", ApplicationID: "app-native-svc", GatewayModelName: "native-model", AppModelName: "native-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("seed native mapping: %v", err)
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-native-svc", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("seed native telemetry: %v", err)
	}

	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
	return srv, tokens, recorder
}

// addServiceToken seeds a service token (Kind=="service") directly into tokens.
// Defaults Scopes to ["llm:invoke"] (the fixed, non-escalatable scope real service
// tokens carry) when the caller doesn't set it explicitly — a test that needs to
// probe defense-in-depth against a mis-scoped future handler passes its own
// Scopes including "gateway:use".
func addServiceToken(tokens *auth.TokenStore, secret string, tok auth.Token) {
	tok.Kind = "service"
	if tok.Scopes == nil {
		tok.Scopes = []string{"llm:invoke"}
	}
	tok.Active = true
	tokens.AddPlainToken(tok, secret)
}

func bearerRequest(method, path, body, secret string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	return req
}

// (a) A service token (scopes=["llm:invoke"], no allowlist) reaches all three
// inference handlers — i.e. it passes auth and is actually served by routing
// (200), not merely "not rejected by auth" (which a stub could fake).
func TestServiceTokenReachesAllThreeInferenceHandlers(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`},
		{"responses", "/v1/responses", `{"model":"qwen-coder","input":"hi"}`},
		{"anthropic messages", "/v1/messages", `{"model":"qwen-coder","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, bearerRequest(http.MethodPost, tt.path, tt.body, "svc-secret"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (service token must reach %s), body = %s", rec.Code, tt.path, rec.Body.String())
			}
		})
	}
}

// (b) The same service token is rejected on /api/portal/* (any route: nothing on
// the Portal/Admin/System surface accepts llm:invoke).
func TestServiceTokenRejectedOnPortalRoutes(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"portal tokens list", http.MethodGet, "/api/portal/tokens"},
		{"portal servers list", http.MethodGet, "/api/portal/servers"},
		{"portal me", http.MethodGet, "/api/portal/me"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, bearerRequest(tt.method, tt.path, "", "svc-secret"))

			if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 401 or 403 (service token must never reach %s), body = %s", rec.Code, tt.path, rec.Body.String())
			}
		})
	}
}

// (c) Defense-in-depth: even if a service token were (mistakenly, by a future
// handler bug) granted "gateway:use" — which real service-token minting never
// does — requireWebScope must still reject it on a Portal route via the
// Kind=="service" check, with 401 (not merely 403 from the ordinary scope gate,
// which this test specifically bypasses by granting the scope). This proves the
// rejection is keyed on Kind, not on scope.
func TestPortalRouteRejectsServiceKindEvenWithGatewayScope(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", Scopes: []string{"llm:invoke", "gateway:use"}})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/portal/tokens", "", "svc-secret"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (Kind==service must be rejected regardless of scope), body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "auth.invalid_token" {
		t.Fatalf("error code = %q, want auth.invalid_token", got)
	}

	// Sanity: the SAME token still reaches an inference handler (requireWebAnyScope
	// does not apply the Kind check) — proves the rejection above is Portal-route
	// specific, not a general "any service token is always invalid" regression.
	chatRec := httptest.NewRecorder()
	srv.ServeHTTP(chatRec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if chatRec.Code != http.StatusOK {
		t.Fatalf("chat completions status = %d, want 200 (must not be blocked by the portal defense-in-depth check), body = %s", chatRec.Code, chatRec.Body.String())
	}
}

// (d) Model-allowlist admission gate — translate path (chat completions, per the
// spec's shared "effective model" checkpoint; the analogous check in Responses and
// Anthropic Messages is exercised by the tables below).
func TestModelAllowlistGateTranslatePath(t *testing.T) {
	t.Run("model not in allowlist -> 403 before upstream", func(t *testing.T) {
		srv, tokens, recorder := newServiceScopeTestServer(t)
		addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", AllowedModels: []string{"some-other-model"}})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != "model.not_allowed" {
			t.Fatalf("error code = %q, want model.not_allowed", got)
		}
		if len(recorder.All()) != 0 {
			t.Fatalf("no usage event should be recorded when the gate blocks before Resolve")
		}
	})

	t.Run("empty allowlist is a no-op (every model allowed)", func(t *testing.T) {
		srv, tokens, _ := newServiceScopeTestServer(t)
		addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"}) // AllowedModels nil

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (empty allowlist must be a no-op), body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("model in allowlist -> served", func(t *testing.T) {
		srv, tokens, _ := newServiceScopeTestServer(t)
		addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", AllowedModels: []string{"qwen-coder", "other-model"}})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("user token is never affected by AllowedModels", func(t *testing.T) {
		srv, _, _ := newServiceScopeTestServer(t)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "dev-secret"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a user token has no allowlist), body = %s", rec.Code, rec.Body.String())
		}
	})

	// The allowlist is checked against the EFFECTIVE model (after ModelOverrideMap
	// is applied), not the raw client-requested model name — both directions.
	t.Run("allowlist checks the effective model, not the requested name", func(t *testing.T) {
		srv, tokens, _ := newServiceScopeTestServer(t)
		// Requesting "requested-name" maps to "qwen-coder"; the allowlist names the
		// EFFECTIVE model "qwen-coder" (not the requested name) -> must be allowed.
		addServiceToken(tokens, "svc-secret", auth.Token{
			ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc",
			ModelOverrideRules: map[string]auth.ModelOverrideRule{"requested-name": {To: "qwen-coder"}},
			AllowedModels:      []string{"qwen-coder"},
		})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"requested-name","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (allowlist matches the effective model), body = %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("allowlisting the requested name does not allow the effective (overridden) model", func(t *testing.T) {
		srv, tokens, _ := newServiceScopeTestServer(t)
		// The allowlist names the RAW requested name "requested-name" -- but the
		// override maps it to "qwen-coder", so the EFFECTIVE model is not listed.
		addServiceToken(tokens, "svc-secret-2", auth.Token{
			ID: "tok_svc2", ServiceID: "svc_2", ServiceName: "Svc2",
			ModelOverrideRules: map[string]auth.ModelOverrideRule{"requested-name": {To: "qwen-coder"}},
			AllowedModels:      []string{"requested-name"},
		})
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"requested-name","messages":[{"role":"user","content":"hi"}]}`, "svc-secret-2"))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (allowlisting the requested name must not permit the overridden effective model), body = %s", rec.Code, rec.Body.String())
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != "model.not_allowed" {
			t.Fatalf("error code = %q, want model.not_allowed", got)
		}
	})
}

// The allowlist gate also fires on /v1/responses and /v1/messages (the translate
// fallback path — tryProxyNative returns false for these since the mapped app has
// no native-passthrough flag set).
func TestModelAllowlistGateOtherTranslateEndpoints(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"responses", "/v1/responses", `{"model":"qwen-coder","input":"hi"}`},
		{"anthropic messages", "/v1/messages", `{"model":"qwen-coder","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", AllowedModels: []string{"some-other-model"}})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, bearerRequest(http.MethodPost, tt.path, tt.body, "svc-secret"))

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
			}
			if got := decodeErrorCode(t, rec.Body.Bytes()); got != "model.not_allowed" {
				t.Fatalf("error code = %q, want model.not_allowed", got)
			}
		})
	}
}

// The allowlist gate fires on the NATIVE PASSTHROUGH path too (tryProxyNative),
// before Resolve/ProxyNative is ever called — the app serving "native-model" has
// ResponsesMode=passthrough, so a plain /v1/responses request for it takes the
// native path, not the translate fallback.
func TestModelAllowlistGateNativePassthroughPath(t *testing.T) {
	t.Run("blocked before upstream", func(t *testing.T) {
		srv, tokens, recorder := newServiceScopeTestServer(t)
		addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", AllowedModels: []string{"some-other-model"}})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/responses", `{"model":"native-model","input":"hi"}`, "svc-secret"))

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
		}
		if got := decodeErrorCode(t, rec.Body.Bytes()); got != "model.not_allowed" {
			t.Fatalf("error code = %q, want model.not_allowed", got)
		}
		if len(recorder.All()) != 0 {
			t.Fatalf("no usage event should be recorded when the native-path gate blocks before Resolve")
		}
	})

	t.Run("allowed model reaches the native path", func(t *testing.T) {
		srv, tokens, _ := newServiceScopeTestServer(t)
		addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc", AllowedModels: []string{"native-model"}})

		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/responses", `{"model":"native-model","input":"hi"}`, "svc-secret"))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
		}
	})
}

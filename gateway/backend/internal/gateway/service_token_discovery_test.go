// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"testing"
)

// A service token (scopes=["llm:invoke"], no allowlist) must reach the
// model-discovery / utility read endpoints (GET /v1/models, POST
// /v1/messages/count_tokens) — spec §13: the allowlist restricts invocation,
// not discovery, and these two calls make no upstream inference call and record
// no billing, so there is nothing for the allowlist to gate. Before this fix
// both handlers gated on gateway:use only, so a service token got 403 here,
// which breaks a standard OpenAI/Anthropic client that lists models on
// startup. The Portal-route boundary (service tokens never reach /api/portal/*)
// must stay exactly as tight as before — asserted below too.
func TestServiceTokenReachesDiscoveryEndpoints(t *testing.T) {
	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"openai models list", http.MethodGet, "/v1/models", ""},
		{"anthropic count tokens", http.MethodPost, "/v1/messages/count_tokens", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, bearerRequest(tt.method, tt.path, tt.body, "svc-secret"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (service token must reach %s), body = %s", rec.Code, tt.path, rec.Body.String())
			}
		})
	}
}

// The same service token must still be rejected on /api/portal/* — proves the
// discovery-endpoint relaxation above did not loosen the Portal-route boundary
// elsewhere (requireWebScope's Kind=="service" check is untouched by this fix).
func TestServiceTokenStillRejectedOnPortalRoutesAfterDiscoveryFix(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, bearerRequest(http.MethodGet, "/api/portal/tokens", "", "svc-secret"))

	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 401 or 403 (service token must never reach /api/portal/tokens), body = %s", rec.Code, rec.Body.String())
	}
}

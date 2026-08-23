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

func TestResolveModelOverride(t *testing.T) {
	tok := auth.Token{
		ModelOverride:    "catch-all",
		ModelOverrideMap: map[string]string{"gpt-4o": "qwen-coder", "empty-target": ""},
	}
	tests := []struct {
		name      string
		token     auth.Token
		requested string
		want      string
	}{
		{"exact map entry wins over catch-all", tok, "gpt-4o", "qwen-coder"},
		{"unmapped requested falls back to catch-all", tok, "claude-sonnet", "catch-all"},
		{"empty map target is ignored -> catch-all", tok, "empty-target", "catch-all"},
		{"no map + no catch-all leaves requested unchanged", auth.Token{}, "gpt-4o", "gpt-4o"},
		{"map miss + no catch-all leaves requested unchanged", auth.Token{ModelOverrideMap: map[string]string{"x": "y"}}, "gpt-4o", "gpt-4o"},
		{"catch-all only forces every requested model", auth.Token{ModelOverride: "forced"}, "anything", "forced"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveModelOverride(tt.token, tt.requested); got != tt.want {
				t.Fatalf("resolveModelOverride(%q) = %q, want %q", tt.requested, got, tt.want)
			}
		})
	}
}

// newModelOverrideMapTestServer seeds a token with a per-model override MAP
// {"gpt-oss-20b":"qwen-coder"} and NO catch-all. "qwen-coder" is the only routable
// model (seedGatewayTestRoutes), "gpt-oss-20b" is unroutable — so a request for
// "gpt-oss-20b" succeeding + recording Model "qwen-coder" proves the map mapped it.
func newModelOverrideMapTestServer(t *testing.T) (*Server, *usage.Recorder) {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_map", UserID: "usr_dev", Name: "Map", Status: store.TokenStatusActive, Scopes: `["gateway:use"]`, CreatedAt: now, UpdatedAt: now, ModelOverrideMap: `{"gpt-oss-20b":"qwen-coder"}`}, "ovr-secret"); err != nil {
		t.Fatalf("seed token: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	srv := New(ServerDeps{Tokens: tokens, Usage: recorder, Provider: provider.NewMock(), Routes: routeStore, Portal: portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()})})
	return srv, recorder
}

// TestChatCompletionAppliesTokenModelOverrideMap verifies a token's per-model map
// remaps the requested model (mapped entry, no catch-all).
func TestChatCompletionAppliesTokenModelOverrideMap(t *testing.T) {
	srv, recorder := newModelOverrideMapTestServer(t)
	assertOverrideDroveRouting(t, srv, recorder, http.MethodPost, "/v1/chat/completions",
		`{"model":"gpt-oss-20b","messages":[{"role":"user","content":"hi"}]}`)
}

// TestModelOverrideMapPassthroughForUnmappedModel verifies that, with a map and NO
// catch-all, a requested model NOT in the map is left unchanged — so an unroutable
// model stays unroutable (502) rather than being forced. This proves the map does
// not act as a catch-all.
func TestModelOverrideMapPassthroughForUnmappedModel(t *testing.T) {
	srv, _ := newModelOverrideMapTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"unmapped-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ovr-secret")
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (unmapped model not overridden -> unroutable), body=%s", rr.Code, rr.Body.String())
	}
}

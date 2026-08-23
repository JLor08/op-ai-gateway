// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// TestProviderPathRecordedForTranslate verifies the recorded usage event carries
// the upstream provider path (the translation's chat-completions path) alongside
// the client-facing req_path, for both a chat-completions and an Anthropic
// (translated) request routed to the mock (OpenAI-compatible) provider.
func TestProviderPathRecordedForTranslate(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		body    string
		reqPath string
	}{
		{"chat completions", "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "/v1/chat/completions"},
		{"anthropic translate", "/v1/messages", `{"model":"qwen-coder","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, "/v1/messages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := NewTestServer()
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()

			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			events := srv.Usage.ByUser("usr_dev")
			if len(events) != 1 {
				t.Fatalf("usage events = %d, want 1", len(events))
			}
			e := events[0]
			if e.ReqPath != tt.reqPath {
				t.Fatalf("req_path = %q, want %q", e.ReqPath, tt.reqPath)
			}
			// The mock provider is OpenAI-compatible: the translation always calls the
			// upstream's chat-completions path, regardless of the client edge.
			if e.ProviderPath != "/v1/chat/completions" {
				t.Fatalf("provider_path = %q, want /v1/chat/completions", e.ProviderPath)
			}
		})
	}
}

// TestProviderPathEmptyOnResolveFailure verifies an unresolved request (no route)
// records an empty provider_path (no upstream was ever called).
func TestProviderPathEmptyOnResolveFailure(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"no-such-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].ProviderPath != "" {
		t.Fatalf("provider_path = %q, want empty (no upstream called)", events[0].ProviderPath)
	}
}

// TestEffectiveProviderModel verifies the provider_model value shown in Activity
// is ALWAYS the model actually sent to the upstream application: the per-model
// provider override when set, else the requested model passed through unchanged
// (never blank).
func TestEffectiveProviderModel(t *testing.T) {
	cases := []struct {
		name          string
		providerModel string
		requested     string
		want          string
	}{
		{"override wins", "upstream-qwen", "gpt-oss-20b", "upstream-qwen"},
		{"falls back to the requested model when no override", "", "gpt-oss-20b", "gpt-oss-20b"},
		{"both empty stays empty", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := effectiveProviderModel(routing.Target{ProviderModel: tc.providerModel}, tc.requested)
			if got != tc.want {
				t.Fatalf("effectiveProviderModel(%q, %q) = %q, want %q", tc.providerModel, tc.requested, got, tc.want)
			}
		})
	}
}

// sinkCaptureProvider simulates a real provider that records the translated
// upstream exchange into the capture sink threaded via context.
type sinkCaptureProvider struct{}

func (sinkCaptureProvider) Complete(ctx context.Context, _ routing.Target, _ inference.Request) (provider.Response, error) {
	if sink := provider.CaptureSinkFrom(ctx); sink != nil {
		sink.RecordRequest(
			http.Header{"Content-Type": {"application/json"}, "Authorization": {"Bearer upstream-secret"}},
			[]byte(`{"model":"up","messages":[{"role":"user","content":"hi"}]}`),
		)
		sink.RecordResponseHeaders(http.Header{"Content-Type": {"application/json"}})
		sink.WriteResponse([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
	}
	return provider.Response{Text: "pong", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}}, nil
}

// TestTranslatedCaptureStoresUpstreamExchange verifies that on the translate path,
// with capture enabled, the encrypted capture envelope carries the translated
// upstream request+response (headers+bodies), with the upstream Authorization
// header redacted.
func TestTranslatedCaptureStoresUpstreamExchange(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, sinkCaptureProvider{}, cs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)

	// The client-facing exchange is captured as before.
	if !strings.Contains(env.ReqBody, `"content":"hello"`) {
		t.Fatalf("client req body not captured: %q", env.ReqBody)
	}
	// The translated upstream exchange is now ALSO captured.
	if !strings.Contains(env.TranslatedReqBody, `"model":"up"`) {
		t.Fatalf("translated upstream request body not captured: %q", env.TranslatedReqBody)
	}
	if env.TranslatedRespBody != `{"choices":[{"message":{"content":"pong"}}]}` {
		t.Fatalf("translated upstream response body not captured: %q", env.TranslatedRespBody)
	}
	// The upstream request headers are redacted like the client ones.
	if v := env.TranslatedReqHeaders["Authorization"]; len(v) != 1 || v[0] != "[redacted]" {
		t.Fatalf("translated upstream Authorization not redacted: %v", env.TranslatedReqHeaders["Authorization"])
	}
	if env.TranslatedRespHeaders["Content-Type"][0] != "application/json" {
		t.Fatalf("translated upstream resp content-type not captured: %v", env.TranslatedRespHeaders)
	}
}

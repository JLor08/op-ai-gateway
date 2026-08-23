// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// TestUpstreamAuthReachesUpstream runs a real OpenAICompatibleClient against an httptest
// upstream and asserts the per-app upstream credential (threaded via WithUpstreamAuth) arrives
// on the inbound request for every kind of upstream call: the two inference paths (Complete +
// CompleteStream), a probe (LoadedModels) and native passthrough (ProxyNative). It also checks
// a custom header sends the raw token, and a bare ctx attaches nothing.
func TestUpstreamAuthReachesUpstream(t *testing.T) {
	var mu sync.Mutex
	var gotAuth, gotAPIKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		gotAPIKey = r.Header.Get("x-api-key")
		mu.Unlock()
		switch {
		case r.URL.Path == "/v1/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[]}`)
		case r.URL.Path == "/v1/chat/completions":
			body, _ := io.ReadAll(r.Body)
			if bytes.Contains(body, []byte(`"stream":true`)) {
				w.Header().Set("Content-Type", "text/event-stream")
				fl, _ := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
				if fl != nil {
					fl.Flush()
				}
				_, _ = io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}],"usage":{"total_tokens":1}}`)
		default: // native passthrough (e.g. /v1/responses)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	defer upstream.Close()

	client := NewOpenAICompatibleClient(http.DefaultClient)
	target := routing.Target{Endpoint: upstream.URL, ProviderModel: "up", Timeout: 5 * time.Second}
	req := inference.Request{Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hello"}}}}}

	read := func() (string, string) {
		mu.Lock()
		defer mu.Unlock()
		return gotAuth, gotAPIKey
	}
	reset := func() {
		mu.Lock()
		defer mu.Unlock()
		gotAuth, gotAPIKey = "", ""
	}

	bearerCtx := WithUpstreamAuth(context.Background(), "", "sk-1")

	t.Run("complete_default_bearer", func(t *testing.T) {
		reset()
		if _, err := client.Complete(bearerCtx, target, req); err != nil {
			t.Fatalf("Complete returned %v", err)
		}
		if auth, _ := read(); auth != "Bearer sk-1" {
			t.Fatalf("Authorization = %q, want Bearer sk-1", auth)
		}
	})

	t.Run("complete_stream_default_bearer", func(t *testing.T) {
		reset()
		streamReq := req
		streamReq.Stream = true
		err := client.CompleteStream(bearerCtx, target, streamReq, func(inference.StreamEvent) error { return nil })
		if err != nil {
			t.Fatalf("CompleteStream returned %v", err)
		}
		if auth, _ := read(); auth != "Bearer sk-1" {
			t.Fatalf("Authorization = %q, want Bearer sk-1", auth)
		}
	})

	t.Run("loaded_models_probe_bearer", func(t *testing.T) {
		reset()
		if _, err := client.LoadedModels(bearerCtx, target, "/v1/models", "openai"); err != nil {
			t.Fatalf("LoadedModels returned %v", err)
		}
		if auth, _ := read(); auth != "Bearer sk-1" {
			t.Fatalf("Authorization = %q, want Bearer sk-1", auth)
		}
	})

	t.Run("proxy_native_bearer", func(t *testing.T) {
		reset()
		resp, err := client.ProxyNative(bearerCtx, target, "/v1/responses", []byte(`{"model":"up"}`))
		if err != nil {
			t.Fatalf("ProxyNative returned %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if auth, _ := read(); auth != "Bearer sk-1" {
			t.Fatalf("Authorization = %q, want Bearer sk-1", auth)
		}
	})

	t.Run("custom_header", func(t *testing.T) {
		reset()
		ctx := WithUpstreamAuth(context.Background(), "x-api-key", "sk-2")
		if _, err := client.Complete(ctx, target, req); err != nil {
			t.Fatalf("Complete returned %v", err)
		}
		auth, apiKey := read()
		if apiKey != "sk-2" {
			t.Fatalf("x-api-key = %q, want sk-2", apiKey)
		}
		if auth != "" {
			t.Fatalf("custom header must not set Authorization, got %q", auth)
		}
	})

	t.Run("bare_ctx_no_auth", func(t *testing.T) {
		reset()
		if _, err := client.Complete(context.Background(), target, req); err != nil {
			t.Fatalf("Complete returned %v", err)
		}
		auth, apiKey := read()
		if auth != "" || apiKey != "" {
			t.Fatalf("bare ctx must attach no auth header, got Authorization=%q x-api-key=%q", auth, apiKey)
		}
	})
}

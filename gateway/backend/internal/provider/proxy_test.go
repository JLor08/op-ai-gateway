// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
)

func TestOpenAICompatibleProxyNativeForwardsRawBody(t *testing.T) {
	var gotPath, gotContentType string
	var gotBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"ok\":true}\n\n")
	}))
	defer upstream.Close()

	c := NewOpenAICompatibleClient(upstream.Client())
	target := routing.Target{Endpoint: upstream.URL, Provider: routing.ProviderVLLM}
	in := []byte(`{"model":"upstream-model","input":"hi","tools":[{"type":"function"}]}`)

	resp, err := c.ProxyNative(context.Background(), target, "/v1/responses", in)
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", gotPath)
	}
	if gotContentType != "application/json" {
		t.Fatalf("upstream content-type = %q, want application/json", gotContentType)
	}
	if string(gotBody) != string(in) {
		t.Fatalf("upstream body = %q, want the raw body byte-for-byte %q", gotBody, in)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("response content-type = %q, want text/event-stream", ct)
	}
	out, _ := io.ReadAll(resp.Body)
	if len(out) == 0 {
		t.Fatalf("empty upstream response body")
	}
}

func TestMultiplexerProxyNativeUnsupportedProvider(t *testing.T) {
	// A provider client without the NativeProxyClient capability yields a clear
	// error rather than cross-routing to the fallback.
	m := NewMultiplexer(map[string]Client{"ollama": stubClient{}}, nil)
	_, err := m.ProxyNative(context.Background(), routing.Target{Provider: "ollama"}, "/v1/responses", []byte(`{}`))
	if err == nil {
		t.Fatalf("expected an error for a non-proxy provider")
	}
}

type stubClient struct{}

func (stubClient) Complete(context.Context, routing.Target, inference.Request) (Response, error) {
	return Response{}, nil
}

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

	resp, err := c.ProxyNative(context.Background(), target, "/v1/responses", in, "application/json")
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

// ProxyNative carries the caller's content type to the upstream verbatim, so a
// multipart image-edit body reaches sd-server with its boundary intact while every
// JSON caller keeps sending application/json. Every NativeProxyClient
// implementer is exercised, because each one threads the parameter itself.
func TestProxyNativeForwardsContentType(t *testing.T) {
	const multipart = "multipart/form-data; boundary=X-OP-BOUNDARY"
	clients := []struct {
		name   string
		client func(*http.Client) NativeProxyClient
		target func(endpoint string) routing.Target
	}{
		{"openai_compatible", func(h *http.Client) NativeProxyClient { return NewOpenAICompatibleClient(h) }, func(e string) routing.Target {
			return routing.Target{Endpoint: e, Provider: routing.ProviderVLLM}
		}},
		{"openai_responses", func(h *http.Client) NativeProxyClient { return NewOpenAIResponsesClient(h) }, func(e string) routing.Target {
			return routing.Target{Endpoint: e, Provider: routing.ProviderVendorOpenAISubscription}
		}},
		{"anthropic", func(h *http.Client) NativeProxyClient { return NewAnthropicClient(h) }, func(e string) routing.Target {
			return routing.Target{Endpoint: e, Provider: routing.ProviderVendorAnthropic}
		}},
		{"gemini", func(h *http.Client) NativeProxyClient { return NewGeminiClient(h) }, func(e string) routing.Target {
			return routing.Target{Endpoint: e, Provider: routing.ProviderVendorGemini}
		}},
		{"multiplexer", func(h *http.Client) NativeProxyClient {
			return NewMultiplexer(map[string]Client{routing.ProviderVLLM: NewOpenAICompatibleClient(h)}, nil)
		}, func(e string) routing.Target {
			return routing.Target{Endpoint: e, Provider: routing.ProviderVLLM}
		}},
	}
	for _, tc := range clients {
		for _, ct := range []struct{ name, value string }{{"json", "application/json"}, {"multipart", multipart}} {
			contentType := ct.value
			t.Run(tc.name+"/"+ct.name, func(t *testing.T) {
				var gotMethod, gotContentType string
				var gotBody []byte
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					gotMethod = r.Method
					gotContentType = r.Header.Get("Content-Type")
					gotBody, _ = io.ReadAll(r.Body)
					w.WriteHeader(http.StatusOK)
				}))
				defer upstream.Close()

				in := []byte("--X-OP-BOUNDARY\r\nContent-Disposition: form-data; name=\"prompt\"\r\n\r\nhi\r\n--X-OP-BOUNDARY--\r\n")
				resp, err := tc.client(upstream.Client()).ProxyNative(context.Background(), tc.target(upstream.URL), "/v1/images/edits", in, contentType)
				if err != nil {
					t.Fatalf("ProxyNative: %v", err)
				}
				resp.Body.Close()

				if gotMethod != http.MethodPost {
					t.Fatalf("upstream method = %q, want POST", gotMethod)
				}
				if gotContentType != contentType {
					t.Fatalf("upstream content-type = %q, want %q", gotContentType, contentType)
				}
				if string(gotBody) != string(in) {
					t.Fatalf("upstream body = %q, want the raw body byte-for-byte %q", gotBody, in)
				}
			})
		}
	}
}

// An empty content type is sent as JSON, not as a blank Content-Type header. No
// caller passes one today (the gateway's proxyNative defaults it first); this pins
// the guard for the one that will.
func TestDoNativeProxyDefaultsAnEmptyContentTypeToJSON(t *testing.T) {
	var got []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Values("Content-Type")
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	resp, err := doNativeProxy(context.Background(), upstream.Client(), routing.Target{Endpoint: upstream.URL}, "/v1/responses", []byte(`{}`), "")
	if err != nil {
		t.Fatalf("doNativeProxy: %v", err)
	}
	resp.Body.Close()

	if len(got) != 1 || got[0] != jsonContentType {
		t.Fatalf("upstream Content-Type = %q, want exactly [%q]", got, jsonContentType)
	}
}

func TestMultiplexerProxyNativeUnsupportedProvider(t *testing.T) {
	// A provider client without the NativeProxyClient capability yields a clear
	// error rather than cross-routing to the fallback.
	m := NewMultiplexer(map[string]Client{"ollama": stubClient{}}, nil)
	_, err := m.ProxyNative(context.Background(), routing.Target{Provider: "ollama"}, "/v1/responses", []byte(`{}`), "application/json")
	if err == nil {
		t.Fatalf("expected an error for a non-proxy provider")
	}
}

type stubClient struct{}

func (stubClient) Complete(context.Context, routing.Target, inference.Request) (Response, error) {
	return Response{}, nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newProbeServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return ts
}

func TestProbeContext_VLLM(t *testing.T) {
	// Verified: vllm-project/vllm entrypoints/serve/engine/protocol.py ModelCard
	// carries max_model_len as a top-level field of each data[] entry.
	body := `{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"vllm","max_model_len":4096}]}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "vllm", "/v1/models")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 4096 {
		t.Errorf("context = %d, want 4096", got)
	}
}

func TestProbeContext_LlamaCpp(t *testing.T) {
	// Verified: ggml-org/llama.cpp tools/server/README.md shows n_ctx nested
	// under default_generation_settings in the /props response, not top-level.
	body := `{"default_generation_settings":{"id":0,"n_ctx":8192},"total_slots":1,"model_path":"/models/foo.gguf"}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "llama_cpp", "/props")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 8192 {
		t.Errorf("context = %d, want 8192", got)
	}
}

func TestProbeContext_TGI(t *testing.T) {
	// Verified: huggingface/text-generation-inference docs/openapi.json "Info"
	// schema carries max_total_tokens as a top-level field of the /info response.
	body := `{"model_id":"foo","max_concurrent_requests":128,"max_input_tokens":4095,"max_total_tokens":4096,"version":"2.0.0"}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "tgi", "/info")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 4096 {
		t.Errorf("context = %d, want 4096", got)
	}
}

func TestProbeContext_Ollama(t *testing.T) {
	// Verified: ollama/ollama docs/api.md "Show Model Information" shows the
	// context length under model_info as an architecture-prefixed key, e.g.
	// "llama.context_length" (not a fixed key name).
	body := `{"model_info":{"llama.context_length":8192,"llama.attention.head_count":32},"details":{"family":"llama"}}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "ollama", "/api/show")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 8192 {
		t.Errorf("context = %d, want 8192", got)
	}
}

func TestProbeContext_CustomContextLength(t *testing.T) {
	body := `{"context_length":32768,"other":"field"}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "custom", "/status")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 32768 {
		t.Errorf("context = %d, want 32768", got)
	}
}

func TestProbeContext_CustomBestEffortNCtx(t *testing.T) {
	// Best-effort scanning also picks up n_ctx / max_model_len for an unknown
	// spec type ("" resolves to the same best-effort path as "custom").
	body := `{"nested":{"n_ctx":2048}}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "", "/status")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 2048 {
		t.Errorf("context = %d, want 2048", got)
	}
}

func TestProbeContext_NoMatch(t *testing.T) {
	body := `{"foo":"bar"}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "custom", "/status")
	if err == nil {
		t.Fatalf("ProbeContext: want error for a body with no recognizable context field, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

func TestProbeContext_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "vllm", "/v1/models")
	if err == nil {
		t.Fatalf("ProbeContext: want error for a non-2xx upstream status, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

func TestProbeContext_EmptyContextPath(t *testing.T) {
	got, err := ProbeContext(context.Background(), &http.Client{}, "http://127.0.0.1:1", "vllm", "")
	if err == nil {
		t.Fatalf("ProbeContext: want error for an empty context path, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

// recordingRoundTripper is a fake http.RoundTripper that counts calls and
// always returns a fixed canned response, never touching the network.
type recordingRoundTripper struct {
	calls int
	body  string
}

func (rt *recordingRoundTripper) RoundTrip(_ *http.Request) (*http.Response, error) {
	rt.calls++
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(rt.body)),
	}, nil
}

// TestProbeContext_UsesPassedClient is FIX 2's proof: ProbeContext must issue
// its request through the CALLER-supplied client, not http.DefaultClient. The
// base URL below ("http://probe-context-uses-passed-client.invalid") is not a
// real, dialable host -- if ProbeContext fell back to http.DefaultClient (or
// any client whose Transport actually reaches the network), the request would
// fail with a DNS/dial error instead of returning the canned response, and
// the RoundTripper's call count would stay 0.
func TestProbeContext_UsesPassedClient(t *testing.T) {
	rt := &recordingRoundTripper{body: `{"data":[{"id":"m1","max_model_len":4096}]}`}
	client := &http.Client{Transport: rt}

	got, err := ProbeContext(context.Background(), client, "http://probe-context-uses-passed-client.invalid", "vllm", "/v1/models")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 4096 {
		t.Errorf("context = %d, want 4096", got)
	}
	if rt.calls != 1 {
		t.Errorf("custom client RoundTrip calls = %d, want 1 (ProbeContext must use the passed client, not http.DefaultClient)", rt.calls)
	}
}

// TestProbeContext_NilClientFallsBackToDefault proves the documented safety
// net: a nil client (an existing/incidental caller that never threads one
// through) falls back to http.DefaultClient rather than panicking.
func TestProbeContext_NilClientFallsBackToDefault(t *testing.T) {
	body := `{"data":[{"id":"m1","max_model_len":4096}]}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), nil, ts.URL, "vllm", "/v1/models")
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 4096 {
		t.Errorf("context = %d, want 4096", got)
	}
}

// TestSafeProbePath is the agent's defense-in-depth SSRF guard: only an empty
// or single-"/"-rooted relative path with no scheme/authority/whitespace is
// safe to append to the loopback base. The attack vectors (@userinfo, //
// authority, a scheme, whitespace) must all be rejected, while the legitimate
// per-type probe paths pass.
func TestSafeProbePath(t *testing.T) {
	safe := []string{"", "/metrics", "/v1/models", "/props", "/api/show", "/info", "/a/b/c?x=1"}
	for _, p := range safe {
		if !SafeProbePath(p) {
			t.Errorf("SafeProbePath(%q) = false, want true (safe relative path)", p)
		}
	}
	unsafe := []string{
		"@evil:9999/x",       // userinfo boundary re-anchors Host to the attacker
		"//evil",             // protocol-relative authority
		"//evil/metrics",     //
		"http://evil",        // absolute URL with scheme
		"https://evil/x",     //
		"metrics",            // not rooted at "/"
		"/met rics",          // interior space
		"/met\trics",         // tab
		"/met\nrics",         // newline
		"/x\x00y",            // NUL / control byte
		"file:///etc/passwd", // scheme
	}
	for _, p := range unsafe {
		if SafeProbePath(p) {
			t.Errorf("SafeProbePath(%q) = true, want false (unsafe path must be rejected)", p)
		}
	}
}

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

// TestDetectLiveProgressSupport is the decision-rule test for #51's
// agent-side detector: it pins detectLiveProgressSupport's exact
// supported/unsupported/unknown boundary. These cases are DELIBERATELY the
// same as gateway/backend/internal/provider/model_info_test.go's
// TestParseModelInfoLiveProgressSupport table, byte-for-byte where the shape
// overlaps -- the two detectLiveProgressSupport copies (one per Go module)
// must decide identically on identical input, and this shared table is what
// makes a silent drift between them show up as a failing test on EITHER
// side instead of going unnoticed.
//
// The vLLM and Ollama cases are the load-bearing ones: a vLLM app's context
// probe fetches /v1/models, an entirely different schema that was never
// asked about timings_per_token, so it must read as unknown ("") and NEVER
// as "unsupported" -- the naive simplification ("answered without the key
// -> unsupported") would silently drop live-progress support for every
// working vLLM/Ollama upstream. If either of those two cases starts
// asserting "unsupported", that regression has landed; fix
// detectLiveProgressSupport, not this test.
func TestDetectLiveProgressSupport(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"llama.cpp /props WITH the key -> supported (never inspect the value, which is always false)",
			`{"model":"m","default_generation_settings":{"n_ctx":4096,"params":{"timings_per_token":false,"n_predict":-1}}}`,
			"supported",
		},
		{
			"llama.cpp /props WITHOUT the key -> unsupported, a real verdict (an older build)",
			`{"model":"m","default_generation_settings":{"n_ctx":4096,"params":{"n_predict":-1}}}`,
			"unsupported",
		},
		{
			"a vLLM /v1/models body must NEVER be read as unsupported -- it is a different schema, not an older llama.cpp",
			`{"object":"list","data":[{"id":"facebook/opt-125m","object":"model","owned_by":"vllm","max_model_len":2048}]}`,
			"",
		},
		{
			"an Ollama /api/show body -> unknown, not unsupported",
			`{"model_info":{"general.architecture":"llama"},"parameters":"num_ctx 4096","template":"{{ .Prompt }}"}`,
			"",
		},
		{
			"a /props-shaped body with default_generation_settings but no params object at all -> unknown",
			`{"model":"m","default_generation_settings":{"n_ctx":4096}}`,
			"",
		},
		{
			"unparseable bytes -> unknown",
			`not json`,
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectLiveProgressSupport([]byte(tc.body)); got != tc.want {
				t.Fatalf("detectLiveProgressSupport(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestProbeLiveProgressSupport_Supported is the "custom"-recovery case: this
// probe GETs collector.LiveProgressProbePath ("/props") unconditionally,
// with NO specType parameter at all -- unlike ProbeContext, it never
// dispatches on the effective runtime type. A "custom"-typed child (the
// type routing.DeriveProbePaths gives no context path to at all) still gets
// probed here, and a body carrying the key still reports "supported". A real
// verdict must also report stable == true: it is safe for the caller to
// cache and never ask again for this pid.
func TestProbeLiveProgressSupport_Supported(t *testing.T) {
	ts := newProbeServer(t, `{"default_generation_settings":{"n_ctx":8192,"params":{"timings_per_token":false}}}`)

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "supported" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q", got, "supported")
	}
	if !stable {
		t.Errorf("ProbeLiveProgressSupport stable = false, want true (a real verdict is always cacheable)")
	}
}

// TestProbeLiveProgressSupport_Unsupported covers a real /props document
// from an older llama.cpp build that never added the field. Also stable:
// this is a real, unchanging verdict about this pid's build.
func TestProbeLiveProgressSupport_Unsupported(t *testing.T) {
	ts := newProbeServer(t, `{"default_generation_settings":{"n_ctx":8192,"params":{"n_predict":-1}}}`)

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "unsupported" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q", got, "unsupported")
	}
	if !stable {
		t.Errorf("ProbeLiveProgressSupport stable = false, want true (a real verdict is always cacheable)")
	}
}

// TestProbeLiveProgressSupport_NotFound is the STABLE undetermined case: a
// 404 conclusively says this route does not exist on this build, and that
// cannot change while the process behind it keeps running. verdict stays ""
// (never fabricate "unsupported" from a 404 -- an absent route is not the
// same claim as "a real /props document without the field"), but stable
// must be true so the caller stops asking.
func TestProbeLiveProgressSupport_NotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	defer ts.Close()

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a 404", got, "")
	}
	if !stable {
		t.Errorf("ProbeLiveProgressSupport stable = false, want true (a 404 is conclusive: cache it and stop asking)")
	}
}

// TestProbeLiveProgressSupport_OtherShape mirrors the gateway side's vLLM
// case at the HTTP-probe level (not just the parse-rule level covered by
// TestDetectLiveProgressSupport above): a vLLM-shaped body served back for
// this fixed "/props" GET must read as unknown, never "unsupported" -- but,
// being a well-formed body, it IS a conclusive (stable) answer: this pid is
// simply not a llama.cpp build.
func TestProbeLiveProgressSupport_OtherShape(t *testing.T) {
	ts := newProbeServer(t, `{"object":"list","data":[{"id":"m1","max_model_len":4096}]}`)

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a non-/props shape", got, "")
	}
	if !stable {
		t.Errorf("ProbeLiveProgressSupport stable = false, want true (a well-formed non-/props body is a conclusive answer)")
	}
}

// TestProbeLiveProgressSupport_TransientStatus proves a non-404 non-2xx
// status (a 503 here -- the upstream answered, but with a status that says
// nothing conclusive about whether it's ever going to be a llama.cpp /props
// document) is swallowed to verdict "" AND reported as stable == false, so
// the caller retries instead of caching a possibly-temporary state. The
// response body here is deliberately a WOULD-BE "supported" /props document:
// if ProbeLiveProgressSupport ever stopped checking the status code
// (fetchProbeBody's 2xx check is what this test pins), this exact body would
// parse as "supported" instead of "" -- without that body shape, a broken
// implementation that ignored the status entirely could still coincidentally
// return "" (an empty/error body also parses to ""), which would let this
// test pass without actually covering the status check.
func TestProbeLiveProgressSupport_TransientStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192,"params":{"timings_per_token":false}}}`))
	}))
	defer ts.Close()

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a non-2xx response, even one carrying a well-formed supported body", got, "")
	}
	if stable {
		t.Errorf("ProbeLiveProgressSupport stable = true, want false (a 503 is not conclusive -- the child may still be starting up)")
	}
}

// TestProbeLiveProgressSupport_ConnectionRefused is the TRANSIENT case named
// explicitly in the resource-usage fix: no HTTP response was ever received
// at all, which is exactly the "child may still be warming up" scenario that
// must be retried next cycle, never cached.
func TestProbeLiveProgressSupport_ConnectionRefused(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	addr := ts.URL
	ts.Close() // closed: nothing is listening on addr anymore

	got, stable := ProbeLiveProgressSupport(context.Background(), http.DefaultClient, addr)
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a refused connection", got, "")
	}
	if stable {
		t.Errorf("ProbeLiveProgressSupport stable = true, want false (a refused connection is transient -- retry, do not cache)")
	}
}

// TestProbeLiveProgressSupport_UnparseableBody is the TRANSIENT
// syntactically-invalid-JSON case: a truncated or garbled body reads the
// same as "still mid-response", not a conclusive answer, so it must not be
// cached either.
func TestProbeLiveProgressSupport_UnparseableBody(t *testing.T) {
	ts := newProbeServer(t, `{"default_generation_settings":`) // truncated mid-object

	got, stable := ProbeLiveProgressSupport(context.Background(), ts.Client(), ts.URL)
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for unparseable JSON", got, "")
	}
	if stable {
		t.Errorf("ProbeLiveProgressSupport stable = true, want false (invalid/truncated JSON is transient -- retry, do not cache)")
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

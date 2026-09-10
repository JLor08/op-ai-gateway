// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"op-ai-server-agent/internal/gwapi"
	"op-ai-server-agent/internal/sample"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// recordedProbeRequest is what newProbeServer captures about the single
// request its canned handler received: method, path, raw body, and the
// Content-Type header. It exists so a test can assert the SHAPE of the
// request the code under test issued, not just the body newProbeServer
// serves back -- see TestProbeContextRequestShapePerSpecType, which pins
// exactly this for every spec type (#54: no test in this package had ever
// asserted a probe's method, path, or body before that test existed).
//
// ContentType joined the recording in #54's fix round, and it is load-
// bearing rather than thorough: ProbeOllamaVerdicts is the FIRST caller ever
// to reach fetchProbeBodyWith's header-setting branch (every earlier caller
// passes a nil body, so that branch was unreachable from any test), and a
// real Ollama serves /api/show through gin's ShouldBindJSON, which
// DISPATCHES on the header. A regression that dropped the Set would pass
// every other assertion in this file -- method, path and body would all
// still be right -- while breaking the probe against the only server it is
// aimed at.
//
// The header is captured raw, and the assertions come in a pair: the POST
// paths must carry "application/json", and the bodiless GET rows must carry
// NOTHING. That symmetry is the point -- a mutation that set the header
// unconditionally (dropping fetchProbeBodyWith's len(body) > 0 guard) would
// satisfy a POST-only assertion, and it would put a content type on a
// request that has no content.
type recordedProbeRequest struct {
	Method      string
	Path        string
	Body        []byte
	ContentType string
}

// probeServer is a newProbeServer *httptest.Server with the last request it
// received recorded alongside it. It embeds *httptest.Server so every
// existing caller of newProbeServer keeps compiling unmodified -- ts.URL and
// ts.Client() are unchanged; only a caller that wants the request calls
// ts.lastRequest().
type probeServer struct {
	*httptest.Server

	mu   sync.Mutex
	last *recordedProbeRequest
}

// lastRequest returns the most recently recorded request, or nil if the
// server has not been hit yet.
func (p *probeServer) lastRequest() *recordedProbeRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func newProbeServer(t *testing.T, body string) *probeServer {
	t.Helper()
	ps := &probeServer{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		ps.mu.Lock()
		ps.last = &recordedProbeRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			Body:        reqBody,
			ContentType: r.Header.Get("Content-Type"),
		}
		ps.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	ps.Server = ts
	return ps
}

func TestProbeContext_VLLM(t *testing.T) {
	// Verified: vllm-project/vllm entrypoints/serve/engine/protocol.py ModelCard
	// carries max_model_len as a top-level field of each data[] entry.
	body := `{"object":"list","data":[{"id":"m1","object":"model","created":1,"owned_by":"vllm","max_model_len":4096}]}`
	ts := newProbeServer(t, body)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "vllm", "/v1/models", "")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "llama_cpp", "/props", "")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "tgi", "/info", "")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "ollama", "/api/show", "llama3")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "custom", "/status", "")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "", "/status", "")
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

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "custom", "/status", "")
	if err == nil {
		t.Fatalf("ProbeContext: want error for a body with no recognizable context field, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

// TestProbeContextRequestShapePerSpecType is the root-cause regression test
// for #54: newProbeServer's handler used to be declared
// func(w, _ *http.Request), so no test in this package had ever asserted a
// probe's method, path, or body -- only the served response body. This pins
// the request shape ProbeContext issues per spec type.
//
// The ollama row is the point of the whole exercise: /api/show is POST-only
// upstream (issue #54: since server v0.7.0 it answers a GET with a 405
// text/plain body carrying no context data at all; a 404 before that), so
// ProbeContext POSTs {"model": "<model>"} for this one type and every other
// type keeps the plain GET it always had. This row is the discriminator that
// proves the fix actually changed something: reverting ONLY the POST branch
// in ProbeContext (leaving every other type's GET path untouched) must make
// this row -- and only this row -- fail again with the exact "method = GET,
// want POST" mismatch this test caught the very first time it ran against
// unpatched code.
func TestProbeContextRequestShapePerSpecType(t *testing.T) {
	for _, tc := range []struct {
		specType        string
		path            string
		model           string
		wantMethod      string
		wantBody        string
		wantContentType string
	}{
		{"llama_cpp", "/props", "", http.MethodGet, "", ""},
		{"vllm", "/v1/models", "", http.MethodGet, "", ""},
		{"tgi", "/info", "", http.MethodGet, "", ""},
		{"custom", "/whatever", "", http.MethodGet, "", ""},
		{"ollama", "/api/show", "probe-model", http.MethodPost, `{"model":"probe-model"}`, "application/json"}, // #54: Ollama's /api/show is POST-only; ProbeContext used to send a bodyless GET, which upstream answers with a 405 (text/plain, no context data at all) since server v0.7.0 (a 404 before that). Reverting the POST branch in ProbeContext makes this row -- and only this row -- fail again.
	} {
		t.Run(tc.specType, func(t *testing.T) {
			// The response body is irrelevant here -- extraction correctness
			// per spec type is already covered by TestProbeContext_VLLM and
			// its siblings above. This test only cares about the request
			// ProbeContext issues, so ProbeContext's own return values are
			// deliberately ignored.
			ts := newProbeServer(t, `{}`)

			_, _ = ProbeContext(context.Background(), ts.Client(), ts.URL, tc.specType, tc.path, tc.model)

			got := ts.lastRequest()
			if got == nil {
				t.Fatal("server never received a request")
			}
			if got.Method != tc.wantMethod {
				t.Errorf("method = %q, want %q", got.Method, tc.wantMethod)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want %q", got.Path, tc.path)
			}
			if string(got.Body) != tc.wantBody {
				t.Errorf("body = %q, want %q", got.Body, tc.wantBody)
			}
			// The empty want on the four GET rows is an assertion, not a
			// blank: a bodiless request must carry no content type at all
			// (see recordedProbeRequest). Only the ollama row sends a body,
			// and only it may declare one.
			if got.ContentType != tc.wantContentType {
				t.Errorf("Content-Type = %q, want %q", got.ContentType, tc.wantContentType)
			}
		})
	}
}

func TestProbeContext_NonOKStatus(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer ts.Close()

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "vllm", "/v1/models", "")
	if err == nil {
		t.Fatalf("ProbeContext: want error for a non-2xx upstream status, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

func TestProbeContext_EmptyContextPath(t *testing.T) {
	got, err := ProbeContext(context.Background(), &http.Client{}, "http://127.0.0.1:1", "vllm", "", "")
	if err == nil {
		t.Fatalf("ProbeContext: want error for an empty context path, got (%d, nil)", got)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
}

// TestProbeContext_OllamaEmptyModelNoRequest pins #54's other load-bearing
// rule: an "ollama" probe with no model name issues NO request at all and
// returns the distinct ErrOllamaModelRequired. Ollama's own /api/show
// answers a modelless POST with 400 "model is required", so sending it would
// only spend a round trip to learn nothing conclusive that this local check
// does not already know -- mirroring ProbeOllamaVerdicts' identical
// empty-model short-circuit (TestProbeOllamaVerdictsEmptyModel above). The
// request-recording newProbeServer makes the "no request sent" half of this
// assertion checkable, not just inferable from the error.
func TestProbeContext_OllamaEmptyModelNoRequest(t *testing.T) {
	ts := newProbeServer(t, `{"model_info":{"llama.context_length":8192}}`)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "ollama", "/api/show", "")
	if err != ErrOllamaModelRequired {
		t.Fatalf("ProbeContext err = %v, want ErrOllamaModelRequired", err)
	}
	if got != 0 {
		t.Errorf("context = %d, want 0 on error", got)
	}
	if got := ts.lastRequest(); got != nil {
		t.Errorf("server received a request %+v, want none: an empty model must never be sent", got)
	}
}

// TestProbeContext_OllamaBlankModelNoRequest covers a whitespace-only model:
// it must be treated the same as an empty one (TrimSpace first), not sent
// upstream as a literal " " that Ollama would accept as SOME string but
// almost certainly not resolve to a real model.
func TestProbeContext_OllamaBlankModelNoRequest(t *testing.T) {
	ts := newProbeServer(t, `{}`)

	_, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "ollama", "/api/show", "   ")
	if err != ErrOllamaModelRequired {
		t.Fatalf("ProbeContext err = %v, want ErrOllamaModelRequired", err)
	}
	if got := ts.lastRequest(); got != nil {
		t.Errorf("server received a request %+v, want none: a whitespace-only model must never be sent", got)
	}
}

// TestProbeContext_OllamaModelNeedsEscaping proves the body is built with
// json.Marshal, never string concatenation: a model name carrying a
// character that needs JSON escaping (a literal '"' here) must come through
// the wire correctly escaped, not corrupt the request body.
func TestProbeContext_OllamaModelNeedsEscaping(t *testing.T) {
	ts := newProbeServer(t, `{"model_info":{"llama.context_length":4096}}`)

	got, err := ProbeContext(context.Background(), ts.Client(), ts.URL, "ollama", "/api/show", `weird"model`)
	if err != nil {
		t.Fatalf("ProbeContext: %v", err)
	}
	if got != 4096 {
		t.Errorf("context = %d, want 4096", got)
	}
	wantBody := `{"model":"weird\"model"}`
	if gotReq := ts.lastRequest(); gotReq == nil || string(gotReq.Body) != wantBody {
		t.Errorf("body = %q, want %q", gotReq.Body, wantBody)
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

	got, err := ProbeContext(context.Background(), client, "http://probe-context-uses-passed-client.invalid", "vllm", "/v1/models", "")
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

	got, err := ProbeContext(context.Background(), nil, ts.URL, "vllm", "/v1/models", "")
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
		{
			"a llama.cpp ROUTER-mode dummy /props (issue #55) -> unknown, NEVER a verdict: it is the router's own build, not the one serving this model",
			`{"role":"router","default_generation_settings":{"n_ctx":4096,"params":{"n_predict":-1}}}`,
			"",
		},
		{
			"a llama.cpp ROUTER-mode dummy that WOULD have read as supported is also unknown -- the role gate precedes the params rule",
			`{"role":"router","default_generation_settings":{"n_ctx":4096,"params":{"timings_per_token":false}}}`,
			"",
		},
		{
			"default_generation_settings present but NOT an object -> unknown (the type assertion fails)",
			`{"model":"m","default_generation_settings":"unexpected"}`,
			"",
		},
		{
			"params present but null -> unknown, never unsupported (a null is not an empty params object)",
			`{"model":"m","default_generation_settings":{"n_ctx":4096,"params":null}}`,
			"",
		},
		{
			"no model name at all, but the params object IS present -> a real verdict: the evidence rule needs no model name",
			`{"default_generation_settings":{"n_ctx":4096,"params":{"timings_per_token":false}}}`,
			"supported",
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

// TestDetectCapabilities is the decision-rule test for #49 sub-project 2's
// agent-side capability detector. These cases are DELIBERATELY the same as
// gateway/backend/internal/provider/model_info_test.go's TestDetectCapabilities
// table -- the two detectCapabilities copies (one per Go module) must decide
// identically on identical input, and this shared table is what makes a
// silent drift between them show up as a failing test on EITHER side instead
// of going unnoticed.
//
// The two "an older server" cases are the load-bearing ones: a key absent
// from a document, or a key absent from an otherwise-present nested object,
// must decode to "" (unknown), never "no" -- an older build simply predates
// that key and has not answered the question. If either starts asserting
// "no", that regression has landed; fix detectCapabilities, not this test.
func TestDetectCapabilities(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Capabilities
	}{
		{
			name: "full modalities and tool caps",
			body: `{"modalities":{"vision":true,"video":false,"audio":true},
			        "chat_template_caps":{"supports_tools":true}}`,
			want: Capabilities{Vision: "yes", Video: "no", Audio: "yes", Tools: "yes"},
		},
		{
			// An older server predates chat_template_caps (2026-01-22): tools
			// is UNKNOWN, never "no" -- it was not asked.
			name: "modalities without tool caps",
			body: `{"modalities":{"vision":true,"video":false,"audio":false}}`,
			want: Capabilities{Vision: "yes", Video: "no", Audio: "no"},
		},
		{
			// A key missing from a PRESENT modalities object is unknown too:
			// an older server predates that key (audio 2025-05-23, video
			// 2026-06-08).
			name: "partial modalities object",
			body: `{"modalities":{"vision":true}}`,
			want: Capabilities{Vision: "yes"},
		},
		{
			name: "tool caps without modalities",
			body: `{"chat_template_caps":{"supports_tools":false}}`,
			want: Capabilities{Tools: "no"},
		},
		{
			// llama.cpp router mode answers /props with its OWN build's dummy
			// (#55). Reading it as the child's evidence would be permanent
			// under the no-rewrite discipline.
			name: "router dummy yields nothing",
			body: `{"role":"router","modalities":{"vision":true},"chat_template_caps":{"supports_tools":true}}`,
			want: Capabilities{},
		},
		{name: "not a props document", body: `{"data":[{"id":"m"}]}`, want: Capabilities{}},
		{name: "invalid json", body: `{`, want: Capabilities{}},
		{name: "null body", body: `null`, want: Capabilities{}},
		{
			name: "non-bool modality values are not evidence",
			body: `{"modalities":{"vision":"yes","audio":1}}`,
			want: Capabilities{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := detectCapabilities([]byte(tc.body))
			if got.Vision != tc.want.Vision || got.Video != tc.want.Video ||
				got.Audio != tc.want.Audio || got.Tools != tc.want.Tools {
				t.Fatalf("detectCapabilities = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDetectCapabilitiesRouterGateMatchesLiveProgressGate pins the shared
// router-dummy contract from this module's side: the two module copies must
// not drift on the #55 gate, which detectLiveProgressSupport already
// carries.
//
// Equality is via reflect.DeepEqual, not ==: Capabilities carries an Extra
// []string field, which makes the struct non-comparable with == (the brief's
// literal `got != (Capabilities{})` does not compile -- "struct containing
// []string cannot be compared").
func TestDetectCapabilitiesRouterGateMatchesLiveProgressGate(t *testing.T) {
	body := []byte(`{"role":"router","modalities":{"vision":true}}`)
	if got := detectCapabilities(body); !reflect.DeepEqual(got, Capabilities{}) {
		t.Fatalf("router dummy yielded %+v", got)
	}
	if got := detectLiveProgressSupport(body); got != "" {
		t.Fatalf("sibling detector disagrees on the router gate: %q", got)
	}
}

// TestDetectOllamaCapabilities is the decision-rule test for the Ollama
// sibling of detectCapabilities (#54 project, task 1): it reads the
// "capabilities" array of a POST /api/show response body, per
// detectOllamaCapabilities's own doc comment.
//
// The load-bearing cases are the ones that look like they should assert "no"
// and deliberately do not: Ollama's own capabilities array is NOT exhaustive
// (upstream logs "unknown capabilities for model" for an empty result, the
// field is `omitempty`, a failed model-file read silently shortens the list,
// and detection is substring heuristics over the chat template), so a
// missing name can only ever mean UNKNOWN, never a denial. This function
// therefore has no way to produce "no" at all -- every field it ever writes
// is "" or "yes".
//
//   - "completion" is dropped: upstream ASSUMES it whenever a model has no
//     pooling_type, so its presence or absence carries no evidence either
//     way.
//   - "image" is Ollama's image-GENERATION capability (born as
//     CapabilityImageGeneration, backing /v1/images/generations), not
//     vision, so it must land in Extra and never touch the Vision field.
func TestDetectOllamaCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Capabilities
	}{
		{"absent field is undetermined, not denial", `{"model_info":{}}`, Capabilities{}},
		{"empty array is undetermined", `{"capabilities":[]}`, Capabilities{}},
		{
			"structured names map to structured fields", `{"capabilities":["vision","tools","audio"]}`,
			Capabilities{Vision: "yes", Tools: "yes", Audio: "yes"},
		},
		{"completion is dropped, it is an upstream assumption", `{"capabilities":["completion"]}`, Capabilities{}},
		{"image is NOT vision", `{"capabilities":["image"]}`, Capabilities{Extra: []string{"image"}}},
		{
			"unknown publisher strings are kept verbatim", `{"capabilities":["thinking","weather.v2"]}`,
			Capabilities{Extra: []string{"thinking", "weather.v2"}},
		},
		{
			"duplicates collapse, first occurrence wins", `{"capabilities":["insert","insert"]}`,
			Capabilities{Extra: []string{"insert"}},
		},
		{"a name is never a no", `{"capabilities":["tools"]}`, Capabilities{Tools: "yes"}},
		{"malformed json yields nothing rather than panicking", `not json`, Capabilities{}},
		// Each structured name gets its OWN case, so that swapping two switch
		// arms breaks a test. Reviewed and found missing: with only the
		// combined case above plus the isolated "tools" one, exchanging the
		// vision and audio arms passed the whole suite.
		{"vision alone lands in Vision", `{"capabilities":["vision"]}`, Capabilities{Vision: "yes"}},
		{"audio alone lands in Audio", `{"capabilities":["audio"]}`, Capabilities{Audio: "yes"}},
		// Video has no Ollama equivalent at all: no name maps to it, so the
		// field stays unknown even when everything else is declared.
		{
			"video is never written, Ollama has no such name",
			`{"capabilities":["vision","tools","audio","thinking"]}`,
			Capabilities{Vision: "yes", Tools: "yes", Audio: "yes", Extra: []string{"thinking"}},
		},
		// Shapes a real server can send that must resolve to "unknown"
		// rather than to a denial or a panic.
		{"a null array is undetermined", `{"capabilities":null}`, Capabilities{}},
		{"a non-array value is undetermined", `{"capabilities":"vision"}`, Capabilities{}},
		{"a whitespace-only name is skipped", `{"capabilities":["   ","tools"]}`, Capabilities{Tools: "yes"}},
		{
			"names are matched case- and whitespace-insensitively",
			`{"capabilities":[" Vision ","TOOLS"]}`,
			Capabilities{Vision: "yes", Tools: "yes"},
		},
		{
			"an unmapped name reaches Extra normalised, not byte-for-byte",
			`{"capabilities":[" Weather.V2 "]}`,
			Capabilities{Extra: []string{"weather.v2"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := detectOllamaCapabilities([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("detectOllamaCapabilities(%s) = %+v, want %+v", tc.body, got, tc.want)
			}
		})
	}
}

// captureAtTheAgentsDefaultLevel redirects the default slog logger to a
// buffer at INFO -- the level a real agent runs at (main.newLogger takes
// Debug only under --verbose) -- for the duration of the test.
//
// It exists beside captureDebug (power_logging_test.go) rather than reusing
// it, and the difference is the whole point of the tests below: a Debug
// record never reaches this buffer, exactly as it never reaches a default
// deployment's log, so a capture at Debug would pass whatever level the code
// under test happened to choose.
func captureAtTheAgentsDefaultLevel(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// ollamaCapabilitiesBody builds a valid /api/show body declaring exactly the
// given capability names, in order.
func ollamaCapabilitiesBody(names ...string) string {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, `"`+n+`"`)
	}
	return `{"capabilities":[` + strings.Join(quoted, ",") + `]}`
}

// ollamaNames builds n distinct short publisher-style capability names.
func ollamaNames(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("pub.cap%d", i))
	}
	return out
}

// ollamaNamesAtMaxLength builds n distinct capability names of EXACTLY
// maxOllamaCapabilityNameBytes bytes each -- the clamp's worst case, and the
// input the wire-cost assertion below has to be measured against. The index
// suffix keeps them distinct (the detector dedups), padding fills the rest,
// and the characters are deliberately varied rather than one repeated
// character, because a run of one character compresses inside a PostgreSQL
// index entry and would understate the real cost.
func ollamaNamesAtMaxLength(n int) []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		suffix := fmt.Sprintf(".%d", i)
		var b strings.Builder
		for j := 0; b.Len() < maxOllamaCapabilityNameBytes-len(suffix); j++ {
			b.WriteByte(alphabet[(i+j)%len(alphabet)])
		}
		b.WriteString(suffix)
		out = append(out, b.String())
	}
	return out
}

// TestOllamaCapabilityBoundsStayUnderTheirCeilings is the guard the two
// bounds did not have: it asserts the PROPERTIES they exist to satisfy, not
// their values.
//
// The distinction is the whole design of this test. `maxOllamaExtraCapabilities
// != 64` would be a change detector -- it fails for a tightening as loudly as
// for a loosening and teaches a reader nothing about why 64. What is not a
// change detector is the CEILING each number was read off, because each is a
// real property of a system outside this file:
//
//  1. THE INDEX. A capability name is half of model_mapping_capabilities'
//     primary key (mapping_id, capability), and a PostgreSQL btree index
//     tuple may not exceed 2704 bytes. Measured on real PostgreSQL with
//     incompressible names: 2600 bytes upserts fine, 2704 bytes fails with
//     "index row size 2720 exceeds btree version 4 maximum 2704" -- and
//     since UpsertMappingCapabilities is atomic, that failure takes EVERY
//     capability row of the pass with it, logged at Debug and invisible at
//     the gateway's default level.
//  2. THE FRAME. The whole verdict set travels inside one 1 MiB
//     agent->gateway WebSocket frame (gwapi.MaxWSFrameBytes), and a frame one
//     byte over it closes the connection 1009.
//
// Both assertions are one-directional by construction: TIGHTENING either
// bound only increases the margin, so this test stays silent for it. That is
// deliberate -- a test that fires when someone makes a bound safer is noise.
//
// It is the loosening direction that was completely unguarded, in a way the
// suite could not show: raising maxOllamaExtraCapabilities to 4096, or
// maxOllamaCapabilityNameBytes to 4096, passes every other test in both
// modules. The second one re-opens the atomic-drop defect above outright.
//
// The wire cost is MEASURED off the real sample.Capabilities type rather than
// estimated, so no scaffolding constant is restated here: marshalling the
// object with one empty-name entry gives the envelope, and the difference
// between one and two entries gives the exact marginal cost of a further one,
// separator included. A final check marshals the detector's real worst-case
// output and requires the decomposition to predict it to the byte, so the
// arithmetic cannot drift from the type it prices.
//
// What the frame assertion does NOT claim: that the fleet-wide total is
// bounded. It is not, and maxOllamaExtraCapabilities' own doc says so -- ~101
// worst-case children overflow the frame, because the total is a product and
// only the per-child factor is bounded here. The assertion is the floor that
// factor must keep: one frame must still carry as many worst-case children as
// this system's own per-server spec-count expectation, which is 64 in both
// modules (runtime.maxWatchedSpecs here, runtimeLogMaxWatchedSpecs on the
// gateway), argued there against this same 1 MiB ceiling.
func TestOllamaCapabilityBoundsStayUnderTheirCeilings(t *testing.T) {
	// (1) THE INDEX. Not merely "below 2704" -- comfortably below, because a
	// name shares the index tuple with the mapping_id and the whole point of
	// this bound is that no publisher string can ever approach the cliff. A
	// factor of 8 leaves this assertion satisfied up to 338 bytes (today's
	// 128 sits at a factor of 21) while still failing for any change that
	// brings the bound within an order of magnitude of a defect that drops
	// rows atomically. Measured: at today's count bound the FRAME assertion
	// below binds first, at ~227 bytes -- so this one is the guard that
	// survives a change to the count bound or to the frame arithmetic, not
	// the one that usually fires.
	const pgBtreeMaxIndexRowBytes = 2704
	const indexSafetyFactor = 8
	if got := maxOllamaCapabilityNameBytes * indexSafetyFactor; got > pgBtreeMaxIndexRowBytes {
		t.Errorf("maxOllamaCapabilityNameBytes = %d, which is only a factor of %.1f under PostgreSQL's btree index-tuple maximum of %d bytes; want at least a factor of %d (%d bytes or fewer). "+
			"A name at or near that ceiling fails UpsertMappingCapabilities ATOMICALLY, dropping every capability row of the pass, logged at Debug and invisible at the gateway's default level.",
			maxOllamaCapabilityNameBytes, float64(pgBtreeMaxIndexRowBytes)/float64(maxOllamaCapabilityNameBytes),
			pgBtreeMaxIndexRowBytes, indexSafetyFactor, pgBtreeMaxIndexRowBytes/indexSafetyFactor)
	}

	// (2) THE FRAME. Measure the wire envelope and the marginal per-entry cost
	// off the real type, then price the clamp's worst case.
	marshalLen := func(verdicts []sample.CapabilityVerdict) int {
		b, err := json.Marshal(sample.Capabilities{Verdicts: verdicts, Source: sample.CapabilitySourceOllamaAPIShow})
		if err != nil {
			t.Fatalf("json.Marshal(sample.Capabilities): %v", err)
		}
		return len(b)
	}
	entry := sample.CapabilityVerdict{Name: "", Verdict: "yes"}
	oneEntry := marshalLen([]sample.CapabilityVerdict{entry})
	perEntry := marshalLen([]sample.CapabilityVerdict{entry, entry}) - oneEntry
	if oneEntry <= 0 || perEntry <= 0 {
		t.Fatalf("measured one-entry object = %d bytes and marginal per-entry cost = %d bytes; both must be positive or this assertion measures nothing", oneEntry, perEntry)
	}

	const worstCaseChildrenOneFrameMustCarry = 64
	// oneEntry already carries the envelope and the first (empty-name) entry;
	// perEntry is the marginal cost of each further one, separator included.
	// The names themselves are then priced at the length bound.
	perChild := oneEntry + (maxOllamaExtraCapabilities-1)*perEntry + maxOllamaExtraCapabilities*maxOllamaCapabilityNameBytes
	if total := int64(perChild) * worstCaseChildrenOneFrameMustCarry; total > gwapi.MaxWSFrameBytes {
		t.Errorf("at the bounds (%d names x %d bytes) one child's capability object costs %d bytes on the wire, so %d worst-case children cost %d bytes -- over the %d-byte frame cap (gwapi.MaxWSFrameBytes). "+
			"A frame one byte over it fails the gateway's read and closes 1009, taking telemetry, the system and runtime reports, the runtime_config push and the certificate doorbell with it.",
			maxOllamaExtraCapabilities, maxOllamaCapabilityNameBytes, perChild,
			worstCaseChildrenOneFrameMustCarry, total, gwapi.MaxWSFrameBytes)
	}

	// Non-vacuity: the arithmetic above prices a hypothetical worst case, so
	// prove the detector really does produce it. The real detector, fed a
	// document at both bounds, must yield exactly maxOllamaExtraCapabilities
	// names each exactly maxOllamaCapabilityNameBytes long -- otherwise
	// perChild is priced against an input nothing can generate and both
	// assertions above are decoration.
	caps := detectOllamaCapabilities([]byte(ollamaCapabilitiesBody(ollamaNamesAtMaxLength(maxOllamaExtraCapabilities)...)))
	if len(caps.Extra) != maxOllamaExtraCapabilities {
		t.Fatalf("the detector carried %d names out of a document at the bound, want %d -- the worst case priced above is not the worst case the detector produces", len(caps.Extra), maxOllamaExtraCapabilities)
	}
	for _, name := range caps.Extra {
		if len(name) != maxOllamaCapabilityNameBytes {
			t.Fatalf("a carried name is %d bytes, want exactly %d -- the worst case priced above assumes every carried name may be at the length bound", len(name), maxOllamaCapabilityNameBytes)
		}
	}
	if got := marshalLen(func() []sample.CapabilityVerdict {
		out := make([]sample.CapabilityVerdict, 0, len(caps.Extra))
		for _, name := range caps.Extra {
			out = append(out, sample.CapabilityVerdict{Name: name, Verdict: "yes"})
		}
		return out
	}()); got != perChild {
		t.Errorf("the real worst-case object marshals to %d bytes but the assertion above priced it at %d; the measured envelope/per-entry decomposition has drifted from the type", got, perChild)
	}
}

// TestDetectOllamaCapabilitiesCarriesTheBoundIntact is the AT-THE-BOUND half
// of the clamp (I-1): a document declaring exactly maxOllamaExtraCapabilities
// names, one of them exactly maxOllamaCapabilityNameBytes long, is carried
// whole -- nothing dropped, nothing truncated, and no Warn, because nothing
// degraded. Without it a clamp off by one in the strict direction would pass
// the over-the-bound test below and quietly discard a legitimate name.
func TestDetectOllamaCapabilitiesCarriesTheBoundIntact(t *testing.T) {
	buf := captureAtTheAgentsDefaultLevel(t)

	atTheLimit := strings.Repeat("z", maxOllamaCapabilityNameBytes)
	names := append(ollamaNames(maxOllamaExtraCapabilities-1), atTheLimit)
	caps := detectOllamaCapabilities([]byte(ollamaCapabilitiesBody(names...)))

	if len(caps.Extra) != maxOllamaExtraCapabilities {
		t.Fatalf("len(Extra) = %d, want %d -- a document AT the bound must be carried whole", len(caps.Extra), maxOllamaExtraCapabilities)
	}
	if caps.Extra[len(caps.Extra)-1] != atTheLimit {
		t.Fatalf("the %d-byte name did not survive: last Extra entry = %q", maxOllamaCapabilityNameBytes, caps.Extra[len(caps.Extra)-1])
	}
	if out := buf.String(); strings.Contains(out, "level=WARN") {
		t.Fatalf("a document at the bound warned about a drop; log =\n%s", out)
	}
}

// TestDetectOllamaCapabilitiesClampsPastTheBound is the OVER-the-bound half,
// and it carries the property that makes the clamp safe rather than merely
// bounded: the three structured verdicts survive it.
//
// The document declares maxOllamaExtraCapabilities+16 publisher strings and
// puts "vision", "tools" and "audio" LAST, which is exactly the arrangement a
// `break` at the cap would lose -- the four names a consumer reasons about
// displaced by junk that arrived first. The clamp therefore stops appending
// and keeps scanning.
//
// The Warn is asserted at the agent's own default level, and it must say HOW
// MANY names went: a clamp is otherwise silent by construction, since a
// dropped verdict shows up as a missing row and a missing row is what this
// whole model already means by "unknown".
func TestDetectOllamaCapabilitiesClampsPastTheBound(t *testing.T) {
	buf := captureAtTheAgentsDefaultLevel(t)

	const over = 16
	names := append(ollamaNames(maxOllamaExtraCapabilities+over), "vision", "tools", "audio")
	caps := detectOllamaCapabilities([]byte(ollamaCapabilitiesBody(names...)))

	if len(caps.Extra) != maxOllamaExtraCapabilities {
		t.Fatalf("len(Extra) = %d, want exactly %d -- the array must be clamped, not carried", len(caps.Extra), maxOllamaExtraCapabilities)
	}
	if caps.Vision != "yes" || caps.Tools != "yes" || caps.Audio != "yes" {
		t.Fatalf("the structured verdicts were displaced by the clamp: %+v -- a hostile tail must not be able to cost a consumer the verdicts it reasons about", caps)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "ollama capability names dropped") {
		t.Fatalf("no WARN record about the clamp at the agent's default level (info); log =\n%s -- a silent clamp is indistinguishable from a probe that never ran", out)
	}
	if !strings.Contains(out, fmt.Sprintf("dropped=%d", over)) {
		t.Fatalf("the WARN record does not say how many names were dropped (want dropped=%d); log =\n%s", over, out)
	}
}

// TestDetectOllamaCapabilitiesDropsAnOverlongName pins the second bound and
// the DIRECTION of its failure: a name past maxOllamaCapabilityNameBytes is
// dropped, never truncated.
//
// Truncating would be worse than dropping rather than merely different: the
// prefix is a DIFFERENT capability, and it would be stored as a confident
// "yes" under a name nothing upstream ever declared -- while the real reason
// the length is bounded at all is that one over-long name is half of
// model_mapping_capabilities' primary key, and a btree index tuple past
// PostgreSQL's 2704-byte maximum fails the whole atomic upsert, dropping
// EVERY capability row for that mapping.
//
// The sibling name proves the drop is the long one and not the pass: "tools"
// still lands.
func TestDetectOllamaCapabilitiesDropsAnOverlongName(t *testing.T) {
	buf := captureAtTheAgentsDefaultLevel(t)

	tooLong := strings.Repeat("q", maxOllamaCapabilityNameBytes+1)
	caps := detectOllamaCapabilities([]byte(ollamaCapabilitiesBody(tooLong, "tools")))

	if len(caps.Extra) != 0 {
		t.Fatalf("Extra = %q, want empty -- an over-long name must be dropped, and a TRUNCATED one would be a different capability written as a confident yes", caps.Extra)
	}
	if caps.Tools != "yes" {
		t.Fatalf("Tools = %q, want \"yes\" -- the over-long name must cost only itself", caps.Tools)
	}
	out := buf.String()
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "dropped_over_length=1") {
		t.Fatalf("no WARN record naming the over-length drop; log =\n%s", out)
	}
}

// TestDetectOllamaCapabilitiesSkipsReservedNames pins the agent-side half of
// the reserved-name rule: "mtp" and "live_progress" declared by an /api/show
// document never reach Extra, however they are spelled.
//
// This layer is DEFENCE IN DEPTH and the test says so on purpose -- the
// load-bearing rule is the gateway's ingest boundary, which is the only one
// in the path of a buggy or hostile agent that puts the name straight into
// the verdicts it sends. What this filter buys is that an honest agent never
// puts a name on the wire the gateway would only have to drop.
//
// Why these two and not vision/tools/audio: Ollama's array is real evidence
// for those three and says nothing at all about either of these. "mtp" is
// not detected anywhere and is a display and operator-seed fact only -- the
// router's flat +30 bonus that once read it is deleted, so what a
// publisher's string would still corrupt is that display; "live_progress"
// has a dedicated wire field, and for an Ollama child that field is always
// "", so a publisher's string would not collide with the dedicated answer --
// it would BE the answer, and the router would send timings_per_token to an
// upstream that does not understand it.
//
// The sibling names prove the skip costs only itself: "vision" still lands
// as a structured verdict and "thinking" still reaches Extra.
func TestDetectOllamaCapabilitiesSkipsReservedNames(t *testing.T) {
	for _, tc := range []struct {
		name  string
		names []string
	}{
		{"as declared", []string{"mtp", "live_progress", "vision", "thinking"}},
		{"case- and whitespace-normalised first", []string{" MTP ", "Live_Progress", "vision", "thinking"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureAtTheAgentsDefaultLevel(t)
			caps := detectOllamaCapabilities([]byte(ollamaCapabilitiesBody(tc.names...)))

			if !reflect.DeepEqual(caps.Extra, []string{"thinking"}) {
				t.Fatalf("Extra = %q, want only [thinking] -- a reserved name must never be carried, and an unrelated one must still be", caps.Extra)
			}
			if caps.Vision != "yes" {
				t.Fatalf("Vision = %q, want \"yes\" -- the reserved names must cost only themselves", caps.Vision)
			}
			out := buf.String()
			if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "dropped_reserved=2") {
				t.Fatalf("no WARN record naming the two reserved drops; log =\n%s", out)
			}
		})
	}
}

// TestProbeOllamaVerdictsRefusesAnImplausiblyLargeBody bounds the INPUT, not
// just the output. Both cases send the SAME declaration ("vision"), so the
// only difference between them is the document's size:
//
//   - at the bound the verdict is read normally -- Vision "yes";
//   - one byte past it the probe reports NO verdicts at all, so the
//     capability stays unknown rather than being answered out of a document
//     that cannot be an /api/show reply.
//
// The refusal is CONCLUSIVE (stable == true) on purpose, and that is the
// existing rule rather than a new one: a well-formed body that simply is not
// the document asked for has always been conclusive here. Treating it as
// transient would re-read and re-parse a quarter-megabyte body once per
// collect cycle for the child's whole life, and do it invisibly, since the
// caller only logs a retry at Debug.
func TestProbeOllamaVerdictsRefusesAnImplausiblyLargeBody(t *testing.T) {
	// A valid /api/show body of exactly n bytes that declares "vision".
	body := func(n int) string {
		head := `{"capabilities":["vision"],"license":"`
		tail := `"}`
		return head + strings.Repeat("a", n-len(head)-len(tail)) + tail
	}

	t.Run("at the bound the verdict is read", func(t *testing.T) {
		at := body(maxOllamaShowConclusiveBodyBytes)
		if len(at) != maxOllamaShowConclusiveBodyBytes {
			t.Fatalf("test body is %d bytes, want %d", len(at), maxOllamaShowConclusiveBodyBytes)
		}
		ts := newProbeServer(t, at)
		verdicts, stable := ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "llama3")
		if !stable || verdicts.Caps.Vision != "yes" {
			t.Fatalf("ProbeOllamaVerdicts = (%+v, %v), want Vision \"yes\" and stable -- a body AT the bound is a real answer", verdicts, stable)
		}
	})

	t.Run("one byte past the bound reports nothing", func(t *testing.T) {
		buf := captureAtTheAgentsDefaultLevel(t)
		ts := newProbeServer(t, body(maxOllamaShowConclusiveBodyBytes+1))
		verdicts, stable := ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "llama3")
		if !reflect.DeepEqual(verdicts, PropsVerdicts{}) {
			t.Fatalf("ProbeOllamaVerdicts = %+v, want the zero verdict set -- an implausible document must leave every capability UNKNOWN, not answer one out of it", verdicts)
		}
		if !stable {
			t.Fatalf("stable = false, want true -- a COMPLETE body that is not this document is conclusive here, so it costs one read per pid generation instead of one per cycle")
		}
		out := buf.String()
		if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "implausibly large") {
			t.Fatalf("no WARN record about the oversized body at the agent's default level (info); log =\n%s", out)
		}
	})
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a 404", got, "")
	}
	if !stable {
		t.Errorf("ProbeLiveProgressSupport stable = false, want true (a 404 is conclusive: cache it and stop asking)")
	}
}

// TestProbeLiveProgressSupport_ConclusiveRefusals pins the OTHER conclusive
// statuses beside the 404 above (final-review finding F1). 401/403 is the
// supported `llama-server --api-key ${API_TOKEN}` shape: llama.cpp marks only
// /health and /v1/health as public, so /props answers 401/403 for a probe
// with no credential -- and the agent's probe HAS no credential
// (runtime.Status carries no token, issue #58). 405 is the same class of
// fixed, build-level fact as a 404: the route exists, but not for GET.
//
// None of the three can change while this pid lives, so all three MUST be
// stable: treating them as transient re-GETs /props on every collect cycle
// (1 s default, 250 ms floor) for the child's entire lifetime, plus one
// slog.Debug line per attempt, forever. A 5xx stays transient -- it is
// exactly the "child still warming up" case -- and is included here as the
// control that proves the classifier still distinguishes the two.
//
// The verdict must stay "" in every row either way: a refusal is never
// evidence about the build's request schema, only about its routing table.
func TestProbeLiveProgressSupport_ConclusiveRefusals(t *testing.T) {
	cases := []struct {
		status     int
		wantStable bool
		why        string
	}{
		{http.StatusUnauthorized, true, "401: /props is behind an api key this probe cannot supply -- fixed at exec time"},
		{http.StatusForbidden, true, "403: same as 401, a credential decision fixed at exec time"},
		{http.StatusMethodNotAllowed, true, "405: the route is not GETtable on this build -- as fixed as a 404"},
		{http.StatusInternalServerError, false, "500: says nothing conclusive -- the child may still be starting up"},
		{http.StatusServiceUnavailable, false, "503: same -- must be retried, never cached"},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				// A would-be "supported" document: if the status check were
				// ever dropped, this body would parse to "supported" and the
				// verdict assertion below would catch it.
				_, _ = w.Write([]byte(`{"default_generation_settings":{"n_ctx":8192,"params":{"timings_per_token":false}}}`))
			}))
			defer ts.Close()

			verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
			got := verdicts.LiveProgress
			if got != "" {
				t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for a %d", got, "", tc.status)
			}
			if stable != tc.wantStable {
				t.Errorf("ProbeLiveProgressSupport stable = %v, want %v -- %s", stable, tc.wantStable, tc.why)
			}
		})
	}
}

// TestProbePropsVerdictsFetchesOnce is the entire point of widening the
// probe (#49-2): one GET yields every verdict, so a llama_cpp child is not
// asked for /props twice (once for live progress, once for capabilities) --
// the hit counter is the assertion that matters.
func TestProbePropsVerdictsFetchesOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != LiveProgressProbePath {
			t.Errorf("probed %q, want %q", r.URL.Path, LiveProgressProbePath)
		}
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":false}},
		                        "modalities":{"vision":true,"video":false,"audio":false},
		                        "chat_template_caps":{"supports_tools":true}}`))
	}))
	defer srv.Close()

	v, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
	if !stable {
		t.Fatal("a parsed /props document must be stable")
	}
	if v.LiveProgress != "supported" {
		t.Fatalf("LiveProgress = %q, want supported", v.LiveProgress)
	}
	if v.Caps.Vision != "yes" || v.Caps.Video != "no" || v.Caps.Tools != "yes" {
		t.Fatalf("Caps = %+v", v.Caps)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("/props hits = %d, want exactly 1", got)
	}
}

// TestProbePropsVerdictsKeepsTheConclusiveSet pins the conclusive/transient
// contract as a regression anchor, restated here on the widened function:
// ProbePropsVerdicts must keep ProbeLiveProgressSupport's exact conclusive
// set ({404, 401, 403, 405}), transient/retry rule (status 0 and every other
// non-2xx, notably 5xx), and stable-empty caching -- only the return payload
// widened.
func TestProbePropsVerdictsKeepsTheConclusiveSet(t *testing.T) {
	for _, status := range []int{404, 401, 403, 405} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		v, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if !stable {
			t.Fatalf("status %d must be conclusive", status)
		}
		// reflect.DeepEqual, not ==: Capabilities carries an []string.
		if v.LiveProgress != "" || !reflect.DeepEqual(v.Caps, Capabilities{}) {
			t.Fatalf("status %d yielded verdicts: %+v", status, v)
		}
	}
	for _, status := range []int{500, 503} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
		_, stable := ProbePropsVerdicts(context.Background(), srv.Client(), srv.URL)
		srv.Close()
		if stable {
			t.Fatalf("status %d must be transient", status)
		}
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), http.DefaultClient, addr)
	got := verdicts.LiveProgress
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

	verdicts, stable := ProbePropsVerdicts(context.Background(), ts.Client(), ts.URL)
	got := verdicts.LiveProgress
	if got != "" {
		t.Errorf("ProbeLiveProgressSupport verdict = %q, want %q (unknown) for unparseable JSON", got, "")
	}
	if stable {
		t.Errorf("ProbeLiveProgressSupport stable = true, want false (invalid/truncated JSON is transient -- retry, do not cache)")
	}
}

// TestProbeOllamaVerdictsCapabilities is ProbeOllamaVerdicts' happy path: a
// 200 /api/show response carrying a capabilities array yields those
// verdicts, and stable == true -- a real /api/show document is as
// conclusive an answer as a real /props one. The point of this case is that
// LiveProgress stays "" even though Caps is populated: the two verdicts
// inside one PropsVerdicts must not be coupled to each other.
func TestProbeOllamaVerdictsCapabilities(t *testing.T) {
	ts := newProbeServer(t, `{"capabilities":["vision","tools"]}`)

	verdicts, stable := ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "llama3")
	if !stable {
		t.Errorf("stable = false, want true (a real /api/show document is conclusive)")
	}
	if verdicts.LiveProgress != "" {
		t.Errorf("LiveProgress = %q, want %q (Ollama has no live-progress surface)", verdicts.LiveProgress, "")
	}
	want := Capabilities{Vision: "yes", Tools: "yes"}
	if !reflect.DeepEqual(verdicts.Caps, want) {
		t.Errorf("Caps = %+v, want %+v", verdicts.Caps, want)
	}
}

// TestProbeOllamaVerdictsLiveProgressNeverAVerdict pins the load-bearing rule
// (#54, task 3): LiveProgress must stay "" even when the response body ALSO
// happens to carry a key detectLiveProgressSupport would read as "supported"
// on the llama.cpp side. If ProbeOllamaVerdicts ever routed its body through
// detectLiveProgressSupport (a plausible but wrong copy from
// ProbePropsVerdicts), this body would flip the verdict to "supported" -- an
// unknown must never become ANY verdict here, because Ollama exposes no such
// surface to have an opinion about, and "" is what tells the caller to write
// no row rather than a permanent false claim.
func TestProbeOllamaVerdictsLiveProgressNeverAVerdict(t *testing.T) {
	ts := newProbeServer(t, `{"capabilities":["tools"],"default_generation_settings":{"params":{"timings_per_token":false}}}`)

	verdicts, stable := ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "llama3")
	if !stable {
		t.Fatalf("stable = false, want true")
	}
	if verdicts.LiveProgress != "" {
		t.Errorf("LiveProgress = %q, want %q even though the body carries a llama.cpp-shaped live-progress key", verdicts.LiveProgress, "")
	}
}

// TestProbeOllamaVerdictsRequestShape pins the exact request
// ProbeOllamaVerdicts issues: POST /api/show, body {"model":"<model>"},
// Content-Type application/json, using the request-recording newProbeServer
// gained for #54 (see recordedProbeRequest).
//
// The header is asserted here and not merely inherited from the plumbing:
// this function is the first caller in the module's history to reach
// fetchProbeBodyWith's header-setting branch at all, and a real Ollama
// dispatches /api/show through gin's ShouldBindJSON, which reads it.
func TestProbeOllamaVerdictsRequestShape(t *testing.T) {
	ts := newProbeServer(t, `{}`)

	_, _ = ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "llama3")

	got := ts.lastRequest()
	if got == nil {
		t.Fatal("server never received a request")
	}
	if got.Method != http.MethodPost {
		t.Errorf("method = %q, want %q", got.Method, http.MethodPost)
	}
	if got.Path != "/api/show" {
		t.Errorf("path = %q, want %q", got.Path, "/api/show")
	}
	wantBody := `{"model":"llama3"}`
	if string(got.Body) != wantBody {
		t.Errorf("body = %q, want %q", got.Body, wantBody)
	}
	if got.ContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json -- Ollama's /api/show binds the body through gin's ShouldBindJSON, which dispatches on this header", got.ContentType)
	}
}

// TestProbeOllamaVerdictsEmptyModel proves an empty model sends NO request at
// all: Ollama's /api/show answers 400 "model is required" without one, so
// sending it would only spend a round trip to learn nothing conclusive.
func TestProbeOllamaVerdictsEmptyModel(t *testing.T) {
	ts := newProbeServer(t, `{"capabilities":["vision"]}`)

	verdicts, stable := ProbeOllamaVerdicts(context.Background(), ts.Client(), ts.URL, "")
	if stable {
		t.Errorf("stable = true, want false (no model name -> no conclusive answer)")
	}
	if !reflect.DeepEqual(verdicts, PropsVerdicts{}) {
		t.Errorf("verdicts = %+v, want zero value", verdicts)
	}
	if got := ts.lastRequest(); got != nil {
		t.Errorf("server received a request %+v, want none: an empty model must never be sent", got)
	}
}

// TestProbeOllamaVerdictsConclusiveRefusals mirrors
// TestProbeLiveProgressSupport_ConclusiveRefusals on the Ollama sibling:
// ProbeOllamaVerdicts reuses ProbePropsVerdicts' conclusive-status set
// verbatim, for the identical reason documented there -- 404/401/403/405 are
// fixed properties of the binary's routing table and the credential it was
// started with, both fixed at exec time.
func TestProbeOllamaVerdictsConclusiveRefusals(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusMethodNotAllowed} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) }))
			defer srv.Close()

			verdicts, stable := ProbeOllamaVerdicts(context.Background(), srv.Client(), srv.URL, "llama3")
			if !stable {
				t.Errorf("status %d: stable = false, want true", status)
			}
			if !reflect.DeepEqual(verdicts, PropsVerdicts{}) {
				t.Errorf("status %d: verdicts = %+v, want zero value", status, verdicts)
			}
		})
	}
}

// TestProbeOllamaVerdictsTransient covers the two transient cases: a 500 (the
// child may still be starting up) and a fully refused connection (status 0,
// no HTTP response at all). Neither is conclusive, so both must report
// stable == false.
func TestProbeOllamaVerdictsTransient(t *testing.T) {
	t.Run("500", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		verdicts, stable := ProbeOllamaVerdicts(context.Background(), srv.Client(), srv.URL, "llama3")
		if stable {
			t.Errorf("stable = true, want false (a 500 may just mean the child is still starting up)")
		}
		if !reflect.DeepEqual(verdicts, PropsVerdicts{}) {
			t.Errorf("verdicts = %+v, want zero value", verdicts)
		}
	})

	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		addr := srv.URL
		srv.Close() // closed: nothing is listening on addr anymore

		verdicts, stable := ProbeOllamaVerdicts(context.Background(), http.DefaultClient, addr, "llama3")
		if stable {
			t.Errorf("stable = true, want false (a refused connection is transient -- retry, do not cache)")
		}
		if !reflect.DeepEqual(verdicts, PropsVerdicts{}) {
			t.Errorf("verdicts = %+v, want zero value", verdicts)
		}
	})
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

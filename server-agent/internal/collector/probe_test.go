// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// recordedProbeRequest is what newProbeServer captures about the single
// request its canned handler received: method, path, and raw body. It exists
// so a test can assert the SHAPE of the request the code under test issued,
// not just the body newProbeServer serves back -- see
// TestProbeContextRequestShapePerSpecType, which pins exactly this for every
// spec type (#54: no test in this package had ever asserted a probe's
// method, path, or body before that test existed).
type recordedProbeRequest struct {
	Method string
	Path   string
	Body   []byte
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
		ps.last = &recordedProbeRequest{Method: r.Method, Path: r.URL.Path, Body: reqBody}
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

// TestProbeContextRequestShapePerSpecType is the root-cause regression test
// for #54: newProbeServer's handler used to be declared
// func(w, _ *http.Request), so no test in this package had ever asserted a
// probe's method, path, or body -- only the served response body. This pins
// the request shape ProbeContext issues per spec type AS IT STANDS TODAY.
//
// The ollama row is the point of the whole exercise: /api/show is POST-only
// upstream (issue #54), but ProbeContext has only ever sent GET, so this row
// pins that GET as today's (buggy) behaviour, not as intended behaviour. The
// task that fixes #54 flips this one row's wantMethod to http.MethodPost;
// until then, this test is the discriminator that proves the fix actually
// changed something -- it must fail the moment the row flips against
// unpatched code, and pass again once ProbeContext is taught to POST to
// Ollama.
func TestProbeContextRequestShapePerSpecType(t *testing.T) {
	for _, tc := range []struct {
		specType   string
		path       string
		wantMethod string
		wantBody   string
	}{
		{"llama_cpp", "/props", http.MethodGet, ""},
		{"vllm", "/v1/models", http.MethodGet, ""},
		{"tgi", "/info", http.MethodGet, ""},
		{"custom", "/whatever", http.MethodGet, ""},
		{"ollama", "/api/show", http.MethodGet, ""}, // Task 4 flips this row to POST: #54's bug is that Ollama's /api/show is POST-only and ProbeContext has always sent GET, so this pins that mistake as it exists today, not as intended behaviour.
	} {
		t.Run(tc.specType, func(t *testing.T) {
			// The response body is irrelevant here -- extraction correctness
			// per spec type is already covered by TestProbeContext_VLLM and
			// its siblings above. This test only cares about the request
			// ProbeContext issues, so ProbeContext's own return values are
			// deliberately ignored.
			ts := newProbeServer(t, `{}`)

			_, _ = ProbeContext(context.Background(), ts.Client(), ts.URL, tc.specType, tc.path)

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
		})
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

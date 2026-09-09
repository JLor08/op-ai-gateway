// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"reflect"
	"testing"
)

func TestParseModelInfoProps(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []ModelInfo
	}{
		{"nested n_ctx + model", `{"model":"deepseek-v3","default_generation_settings":{"n_ctx":131072}}`, []ModelInfo{{Name: "deepseek-v3", ContextSize: 131072}}},
		{"top-level n_ctx + model_path", `{"model_path":"/models/qwen-coder.gguf","n_ctx":32768}`, []ModelInfo{{Name: "qwen-coder.gguf", ContextSize: 32768}}},
		{"n_ctx as float", `{"model":"m","default_generation_settings":{"n_ctx":8192.0}}`, []ModelInfo{{Name: "m", ContextSize: 8192}}},
		{"no ctx -> name only (ctx 0)", `{"model":"m"}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"no model -> empty", `{"default_generation_settings":{"n_ctx":123}}`, nil},
		{"garbage -> empty", `not json`, nil},
		{"nested wins over top-level", `{"model":"m","n_ctx":4096,"default_generation_settings":{"n_ctx":9000}}`, []ModelInfo{{Name: "m", ContextSize: 9000}}},
		{"string n_ctx not coerced", `{"model":"m","default_generation_settings":{"n_ctx":"131072"}}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"dgs not an object -> falls through", `{"model":"m","default_generation_settings":"nope"}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"negative n_ctx -> 0", `{"model":"m","default_generation_settings":{"n_ctx":-5}}`, []ModelInfo{{Name: "m", ContextSize: 0}}},
		{"top-level array -> empty", `[1,2,3]`, nil},
		{
			"parseModelInfo plumbs the live-progress verdict onto the returned entry (supported)",
			`{"model":"m","default_generation_settings":{"n_ctx":100,"params":{"timings_per_token":false}}}`,
			[]ModelInfo{{Name: "m", ContextSize: 100, LiveProgressSupport: "supported"}},
		},
		{
			"parseModelInfo plumbs the live-progress verdict onto the returned entry (unsupported)",
			`{"model":"m","default_generation_settings":{"n_ctx":100,"params":{"n_predict":-1}}}`,
			[]ModelInfo{{Name: "m", ContextSize: 100, LiveProgressSupport: "unsupported"}},
		},
		{
			"parseModelInfo plumbs the capability verdict set onto the returned entry (#49-2)",
			`{"model":"m","default_generation_settings":{"n_ctx":100},"modalities":{"vision":true,"audio":false},"chat_template_caps":{"supports_tools":true}}`,
			[]ModelInfo{{Name: "m", ContextSize: 100, Caps: Capabilities{Vision: "yes", Audio: "no", Tools: "yes"}}},
		},
	}
	for _, tc := range cases {
		got := parseModelInfo([]byte(tc.body))
		if len(got) != len(tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		for i := range got {
			// reflect.DeepEqual, not == : ModelInfo now carries a Capabilities
			// field, which itself carries a []string (Extra), making both
			// non-comparable with ==.
			if !reflect.DeepEqual(got[i], tc.want[i]) {
				t.Fatalf("%s: got %+v, want %+v", tc.name, got[i], tc.want[i])
			}
		}
	}
}

// TestParseModelInfoNamelessBodyStillCarriesTheVerdict is final-review F5: a
// /props document that carries the capability evidence but NO model/model_path
// used to be dropped whole (parseModelInfo returned nil the moment no name was
// found), so a real verdict was discarded even though the evidence rule needs
// no name at all -- a capability belongs to the server BUILD, a context size to
// a MODEL. The agent-side half never had this coupling; it hands the raw body
// straight to the detector.
//
// The nameless entry must carry NO context size even though this body reports
// an n_ctx of 4096: an unnamed size cannot be attributed, and letting it through
// would weaken exactly the name matching that context attribution needs. Both
// facts are asserted numerically/exactly rather than through a "non-empty"
// check, and the two Pick* consumers are exercised on the same slice so the
// split is proven where it is actually read.
func TestParseModelInfoNamelessBodyStillCarriesTheVerdict(t *testing.T) {
	const nameless = `{"default_generation_settings":{"n_ctx":4096,"params":{"timings_per_token":false}}}`

	got := parseModelInfo([]byte(nameless))
	if len(got) != 1 {
		t.Fatalf("parseModelInfo returned %d entries, want exactly 1 -- a nameless body still proves the build's capability", len(got))
	}
	if got[0].LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q", got[0].LiveProgressSupport, "supported")
	}
	if got[0].Name != "" {
		t.Fatalf("Name = %q, want %q (the body carries no model/model_path)", got[0].Name, "")
	}
	if got[0].ContextSize != 0 {
		t.Fatalf("ContextSize = %d, want 0 -- an unnamed context size must never be reported, or it lands on whatever model was probed", got[0].ContextSize)
	}

	// The consumers: the verdict reaches a per-model pick through the
	// first-non-empty fallback, while the context pick still finds nothing.
	if v := PickModelLiveProgressSupport(got, "some-model"); v != "supported" {
		t.Fatalf("PickModelLiveProgressSupport = %q, want %q", v, "supported")
	}
	if n := PickModelContextSize(got, "some-model"); n != 0 {
		t.Fatalf("PickModelContextSize = %d, want 0", n)
	}

	// No name AND no verdict is still nil: this widening reports a nameless
	// entry only when there is something to report.
	if got := parseModelInfo([]byte(`{"default_generation_settings":{"n_ctx":4096}}`)); got != nil {
		t.Fatalf("parseModelInfo(no name, no verdict) = %+v, want nil", got)
	}
}

// TestParseModelInfoNamelessBodyWithCapabilitiesOnlyStillCarriesThem is the
// capability-detector's counterpart to
// TestParseModelInfoNamelessBodyStillCarriesTheVerdict above: a body with NO
// model/model_path AND no live-progress evidence at all (no
// default_generation_settings.params object, so detectLiveProgressSupport
// answers "" -- not merely "unsupported") but WITH modalities evidence must
// still yield a nameless entry, because a capability is exactly the same kind
// of server-BUILD property as the live-progress verdict is, and the widened
// nameless-entry condition ("a determinable live-progress verdict OR any
// determined capability") must fire on capabilities alone.
func TestParseModelInfoNamelessBodyWithCapabilitiesOnlyStillCarriesThem(t *testing.T) {
	const nameless = `{"modalities":{"vision":true}}`

	got := parseModelInfo([]byte(nameless))
	if len(got) != 1 {
		t.Fatalf("parseModelInfo returned %d entries, want exactly 1 -- a nameless body still proves the build's capability", len(got))
	}
	want := ModelInfo{Caps: Capabilities{Vision: "yes"}}
	if !reflect.DeepEqual(got[0], want) {
		t.Fatalf("got %+v, want %+v (no live-progress verdict, no name, no context size -- only the capability)", got[0], want)
	}

	if v := PickModelCapabilities(got, "some-model"); !reflect.DeepEqual(v, Capabilities{Vision: "yes"}) {
		t.Fatalf("PickModelCapabilities = %+v, want {Vision: yes}", v)
	}
}

// TestParseModelInfoLiveProgressSupport is the decision-rule test for #51: it
// pins detectLiveProgressSupport's exact supported/unsupported/unknown
// boundary, which is the single most important behavior in this feature.
//
// The case named "vLLM ... must NEVER be read as unsupported" is the
// load-bearing one. A vLLM application's context probe fetches /v1/models,
// not /props -- an entirely different schema that was never asked about
// timings_per_token. The naive simplification ("answered without the key ->
// unsupported") would mark every such body unsupported, and Task 3's decision
// rule would then silently stop sending the live-progress parameters to a
// live, working vLLM application. If this case ever starts asserting
// "unsupported", that regression has landed -- do not "fix" this test to
// match; fix detectLiveProgressSupport instead.
func TestParseModelInfoLiveProgressSupport(t *testing.T) {
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
// capability detector: each case pins the shared contract that Task 3 (the
// collector's identical copy) and Task 5 (the store write-back) both depend
// on. The two "an older server" cases are the load-bearing ones: a key
// absent from a document, or a key absent from an otherwise-present nested
// object, must decode to "" (unknown), never "no" -- an older build simply
// predates that key and has not answered the question. If either of those
// starts asserting "no", that regression has landed; fix detectCapabilities,
// not this test.
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

func TestPickModelLiveProgressSupport(t *testing.T) {
	cases := []struct {
		name  string
		infos []ModelInfo
		model string
		want  string
	}{
		{
			"name-match wins even when a different info precedes",
			[]ModelInfo{{Name: "other", LiveProgressSupport: "unsupported"}, {Name: "m", LiveProgressSupport: "supported"}},
			"m", "supported",
		},
		{
			"first-non-empty fallback when no name matches (a per-model probe reporting a divergent name)",
			[]ModelInfo{{Name: "some-basename", LiveProgressSupport: "supported"}},
			"m", "supported",
		},
		{
			"skips unknown entries",
			[]ModelInfo{{Name: "m", LiveProgressSupport: ""}, {Name: "x", LiveProgressSupport: "unsupported"}},
			"m", "unsupported",
		},
		{"empty -> unknown", nil, "m", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickModelLiveProgressSupport(tc.infos, tc.model); got != tc.want {
				t.Fatalf("PickModelLiveProgressSupport(%+v, %q) = %q, want %q", tc.infos, tc.model, got, tc.want)
			}
		})
	}
}

// TestPickModelCapabilities mirrors TestPickModelLiveProgressSupport's table
// exactly, for PickModelCapabilities. Equality is via reflect.DeepEqual, not
// ==: Capabilities carries a []string field (Extra), which is not comparable
// with ==.
func TestPickModelCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		infos []ModelInfo
		model string
		want  Capabilities
	}{
		{
			"name-match wins even when a different info precedes",
			[]ModelInfo{{Name: "other", Caps: Capabilities{Vision: "no"}}, {Name: "m", Caps: Capabilities{Vision: "yes"}}},
			"m",
			Capabilities{Vision: "yes"},
		},
		{
			"first-non-empty fallback when no name matches (a per-model probe reporting a divergent name)",
			[]ModelInfo{{Name: "some-basename", Caps: Capabilities{Tools: "yes"}}},
			"m",
			Capabilities{Tools: "yes"},
		},
		{
			"skips entries with nothing determined",
			[]ModelInfo{{Name: "m", Caps: Capabilities{}}, {Name: "x", Caps: Capabilities{Audio: "no"}}},
			"m",
			Capabilities{Audio: "no"},
		},
		{"empty -> zero value", nil, "m", Capabilities{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickModelCapabilities(tc.infos, tc.model); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PickModelCapabilities(%+v, %q) = %+v, want %+v", tc.infos, tc.model, got, tc.want)
			}
		})
	}
}

func TestProbeModelInfoClient(t *testing.T) {
	t.Run("GETs the probe path and parses props", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"model":"up","default_generation_settings":{"n_ctx":131072}}`))
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeModelInfo(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "/props")
		if err != nil {
			t.Fatalf("ProbeModelInfo err: %v", err)
		}
		if gotPath != "/props" {
			t.Fatalf("GET path = %q, want /props", gotPath)
		}
		want := ModelInfo{Name: "up", ContextSize: 131072}
		// reflect.DeepEqual, not == : see TestParseModelInfoProps for why.
		if len(got) != 1 || !reflect.DeepEqual(got[0], want) {
			t.Fatalf("ProbeModelInfo = %+v, want [%+v]", got, want)
		}
	})

	t.Run("blank probe path is a no-op with no HTTP request", func(t *testing.T) {
		hits := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()

		client := NewOpenAICompatibleClient(srv.Client())
		got, err := client.ProbeModelInfo(context.Background(), routing.Target{Provider: "openai", Endpoint: srv.URL}, "")
		if err != nil || got != nil {
			t.Fatalf("blank probe path = (%v, %v), want (nil, nil)", got, err)
		}
		if hits != 0 {
			t.Fatalf("blank probe path made %d HTTP request(s), want 0", hits)
		}
	})
}

func TestPickModelContextSize(t *testing.T) {
	cases := []struct {
		name  string
		infos []ModelInfo
		model string
		want  int
	}{
		{"name-match wins even when a different info precedes", []ModelInfo{{Name: "other", ContextSize: 1}, {Name: "m", ContextSize: 8192}}, "m", 8192},
		{"first-positive fallback when no name matches", []ModelInfo{{Name: "basename", ContextSize: 4096}}, "m", 4096},
		{"skips non-positive", []ModelInfo{{Name: "m", ContextSize: 0}, {Name: "x", ContextSize: 2048}}, "m", 2048},
		{"empty -> 0", nil, "m", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PickModelContextSize(tc.infos, tc.model); got != tc.want {
				t.Fatalf("PickModelContextSize(%+v, %q) = %d, want %d", tc.infos, tc.model, got, tc.want)
			}
		})
	}
}

func TestExpandModelPath(t *testing.T) {
	cases := []struct {
		name  string
		path  string
		model string
		want  string
	}{
		{"simple substitution", "/upstream/{model}/props", "llama-7b", "/upstream/llama-7b/props"},
		{"slash in name stays multi-segment", "/u/{model}/props", "openai/gpt-4o", "/u/openai/gpt-4o/props"},
		{"space is escaped", "/u/{model}/props", "my model", "/u/my%20model/props"},
		{"no placeholder returned unchanged", "/props", "x", "/props"},
		{"repeated placeholder", "/{model}/{model}", "m", "/m/m"},
		{"unrelated brace left literal", "/{other}/props", "m", "/{other}/props"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpandModelPath(tc.path, tc.model); got != tc.want {
				t.Fatalf("ExpandModelPath(%q, %q) = %q, want %q", tc.path, tc.model, got, tc.want)
			}
		})
	}
}

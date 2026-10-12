// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"os"
	"reflect"
	"testing"
)

// sdcppFullOptionsDoc is the shape stable-diffusion.cpp's capability document
// has when a model is loaded: the sampling defaults sit under
// defaults_by_mode.img_gen.sample_params, the CFG scale under that block's
// guidance.txt_cfg, and clip_skip, seed, negative_prompt, width, height and
// output_format directly in defaults_by_mode.img_gen.
const sdcppFullOptionsDoc = `{
  "model": {"name": "flux1-dev.safetensors", "stem": "flux1-dev"},
  "current_mode": "img_gen",
  "supported_modes": ["img_gen"],
  "limits": {"min_width": 64, "max_width": 2048, "min_height": 64, "max_height": 2048, "max_batch_count": 8},
  "samplers": ["euler", "euler_a", "heun"],
  "schedulers": ["discrete", "karras"],
  "output_formats": ["png", "jpeg", "webp"],
  "output_formats_by_mode": {"img_gen": ["png", "jpeg"]},
  "defaults_by_mode": {
    "img_gen": {
      "prompt": "",
      "negative_prompt": "blurry",
      "clip_skip": -1,
      "width": 512,
      "height": 768,
      "seed": -1,
      "batch_count": 1,
      "output_format": "png",
      "sample_params": {
        "sample_method": "euler",
        "scheduler": "karras",
        "sample_steps": 20,
        "guidance": {"txt_cfg": 7.5, "img_cfg": 1.0}
      }
    }
  }
}`

// probeSdcppImageOptions serves body from an httptest server and probes it
// through the real client, as the sibling capabilities tests do.
func probeSdcppImageOptions(t *testing.T, body string) (SdcppImageOptions, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sdcpp/v1/capabilities" {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	return NewOpenAICompatibleClient(srv.Client()).ProbeSdcppImageOptions(context.Background(), routing.Target{Endpoint: srv.URL})
}

func TestProbeSdcppImageOptions(t *testing.T) {
	t.Run("a full document parses to every field", func(t *testing.T) {
		got, err := probeSdcppImageOptions(t, sdcppFullOptionsDoc)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := SdcppImageOptions{
			Samplers:      []string{"euler", "euler_a", "heun"},
			Schedulers:    []string{"discrete", "karras"},
			OutputFormats: []string{"png", "jpeg"},
			Limits:        SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 8},
			Defaults: SdcppImageDefaults{
				Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
				Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 768,
				CFG: 7.5,
			},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("a partial document parses what it has and leaves the rest zero", func(t *testing.T) {
		got, err := probeSdcppImageOptions(t, `{"samplers":["euler"],"output_formats_by_mode":{"img_gen":["png","webp"]}}`)
		if err != nil {
			t.Fatalf("error = %v", err)
		}
		want := SdcppImageOptions{Samplers: []string{"euler"}, OutputFormats: []string{"png", "webp"}}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("a malformed document is a best-effort value, not an error", func(t *testing.T) {
		// samplers is a number and limits a string: each of those sub-fields is
		// skipped, while the readable ones beside them still come through.
		got, err := probeSdcppImageOptions(t, `{
		  "samplers": 7,
		  "limits": "wide",
		  "schedulers": ["discrete"],
		  "output_formats_by_mode": {"img_gen": ["png"]},
		  "defaults_by_mode": {"img_gen": {"width": 512, "output_format": "png"}}
		}`)
		if err != nil {
			t.Fatalf("error = %v, want nil -- only a non-JSON body is an error", err)
		}
		want := SdcppImageOptions{
			Schedulers:    []string{"discrete"},
			OutputFormats: []string{"png"},
			Defaults:      SdcppImageDefaults{Width: 512, OutputFormat: "png"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got  %+v\nwant %+v", got, want)
		}
	})

	t.Run("a body that is not JSON is an error", func(t *testing.T) {
		got, err := probeSdcppImageOptions(t, "<html>not the document</html>")
		if !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("error = %v, want ErrInvalidResponse", err)
		}
		if !reflect.DeepEqual(got, SdcppImageOptions{}) {
			t.Fatalf("got %+v, want the zero value", got)
		}
	})

	t.Run("a non-2xx status is an error even when the body is a valid document", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(sdcppFullOptionsDoc))
		}))
		defer srv.Close()
		got, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppImageOptions(context.Background(), routing.Target{Endpoint: srv.URL})
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v, want ErrUnavailable for a 503", err)
		}
		if !reflect.DeepEqual(got, SdcppImageOptions{}) {
			t.Fatalf("got %+v, want the zero value -- a failing server's body is not its answer", got)
		}
	})

	t.Run("the application's credential is attached", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("X-Api-Key")
			_, _ = w.Write([]byte(`{}`))
		}))
		defer srv.Close()
		ctx := WithUpstreamAuth(context.Background(), "X-Api-Key", "sd-secret")
		if _, err := NewOpenAICompatibleClient(srv.Client()).ProbeSdcppImageOptions(ctx, routing.Target{Endpoint: srv.URL}); err != nil {
			t.Fatalf("error = %v", err)
		}
		if gotAuth != "sd-secret" {
			t.Fatalf("X-Api-Key = %q, want sd-secret", gotAuth)
		}
	})
}

// sdcppImageOptionsCases is the options table for the stable-diffusion.cpp
// capability document. It is duplicated VERBATIM in
// server-agent/internal/collector/sdcpp_image_options_test.go, because the
// two Go modules share no package and each keeps its own copy of the rule
// (parseSdcppImageOptions here, parseSdcppImageOptions there). The two copies
// must decode identically on identical input: if a case fails, fix the
// decode, not this table, and change both tables together.
// TestSdcppImageOptionsCasesMatchTheAgentsTable below fails when the two
// declarations differ by a byte.
var sdcppImageOptionsCases = []struct {
	name string
	body string
	want SdcppImageOptions
}{
	{
		"a full document parses to every field",
		`{"model":{"name":"flux1-dev.safetensors","stem":"flux1-dev"},"current_mode":"img_gen","supported_modes":["img_gen"],"limits":{"min_width":64,"max_width":2048,"min_height":64,"max_height":2048,"max_batch_count":8},"samplers":["euler","euler_a","heun"],"schedulers":["discrete","karras"],"output_formats":["png","jpeg","webp"],"output_formats_by_mode":{"img_gen":["png","jpeg"]},"defaults_by_mode":{"img_gen":{"prompt":"","negative_prompt":"blurry","clip_skip":-1,"width":512,"height":768,"seed":-1,"batch_count":1,"output_format":"png","sample_params":{"sample_method":"euler","scheduler":"karras","sample_steps":20,"guidance":{"txt_cfg":7.5,"img_cfg":1.0}}}}}`,
		SdcppImageOptions{
			Samplers:      []string{"euler", "euler_a", "heun"},
			Schedulers:    []string{"discrete", "karras"},
			OutputFormats: []string{"png", "jpeg"},
			Limits:        SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 8},
			Defaults: SdcppImageDefaults{
				Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
				Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 768,
				CFG: 7.5,
			},
		},
	},
	{
		"a partial document parses what it has and leaves the rest zero",
		`{"samplers":["euler"],"output_formats_by_mode":{"img_gen":["png","webp"]}}`,
		SdcppImageOptions{Samplers: []string{"euler"}, OutputFormats: []string{"png", "webp"}},
	},
	{
		"a malformed document keeps its readable fields",
		`{"samplers":7,"limits":"wide","schedulers":["discrete"],"output_formats_by_mode":{"img_gen":["png"]},"defaults_by_mode":{"img_gen":{"width":512,"output_format":"png"}}}`,
		SdcppImageOptions{
			Schedulers:    []string{"discrete"},
			OutputFormats: []string{"png"},
			Defaults:      SdcppImageDefaults{Width: 512, OutputFormat: "png"},
		},
	},
	{"empty object", `{}`, SdcppImageOptions{}},
	{"valid JSON that is not an object", `[1,2]`, SdcppImageOptions{}},
	{"null", `null`, SdcppImageOptions{}},
	{"not JSON at all", `<html>502</html>`, SdcppImageOptions{}},
	{
		"one mistyped limit does not drop its siblings",
		`{"limits":{"min_width":64,"max_width":"big","max_height":2048}}`,
		SdcppImageOptions{Limits: SdcppImageLimits{MinWidth: 64, MaxHeight: 2048}},
	},
	{
		"non-string and blank entries are dropped from a list, the rest are kept trimmed",
		`{"samplers":["euler",1,"  "," heun ",null,""]}`,
		SdcppImageOptions{Samplers: []string{"euler", "heun"}},
	},
	{
		"an img_gen output-format list of the wrong type is skipped",
		`{"output_formats_by_mode":{"img_gen":"png"},"schedulers":["karras"]}`,
		SdcppImageOptions{Schedulers: []string{"karras"}},
	},
	{
		"only img_gen output formats are read",
		`{"output_formats_by_mode":{"vid_gen":["gif"]}}`,
		SdcppImageOptions{},
	},
	{
		"output_formats_by_mode of the wrong type is skipped",
		`{"output_formats_by_mode":["png"],"samplers":["euler"]}`,
		SdcppImageOptions{Samplers: []string{"euler"}},
	},
	{
		"defaults_by_mode.img_gen of the wrong type is skipped",
		`{"defaults_by_mode":{"img_gen":"png"},"samplers":["euler"]}`,
		SdcppImageOptions{Samplers: []string{"euler"}},
	},
	{
		"sample_params of the wrong type does not take the top-level defaults with it",
		`{"defaults_by_mode":{"img_gen":{"sample_params":"euler","width":512,"seed":42,"clip_skip":2}}}`,
		SdcppImageOptions{Defaults: SdcppImageDefaults{Width: 512, Seed: 42, ClipSkip: 2}},
	},
	{
		"guidance without txt_cfg leaves CFG unknown, the sampler defaults still parse",
		`{"defaults_by_mode":{"img_gen":{"sample_params":{"sample_method":"heun","sample_steps":30,"guidance":{"img_cfg":1.5}}}}}`,
		SdcppImageOptions{Defaults: SdcppImageDefaults{Sampler: "heun", Steps: 30}},
	},
	{
		"a mistyped txt_cfg is skipped",
		`{"defaults_by_mode":{"img_gen":{"sample_params":{"sample_steps":30,"guidance":{"txt_cfg":"high"}}}}}`,
		SdcppImageOptions{Defaults: SdcppImageDefaults{Steps: 30}},
	},
	{
		"the defaults are read from img_gen only",
		`{"defaults_by_mode":{"vid_gen":{"width":1,"sample_params":{"sample_steps":9}}}}`,
		SdcppImageOptions{},
	},
	{
		"default strings are trimmed but the negative prompt is left as written",
		`{"defaults_by_mode":{"img_gen":{"negative_prompt":"  blurry ","output_format":" png ","sample_params":{"sample_method":" euler ","scheduler":" karras "}}}}`,
		SdcppImageOptions{Defaults: SdcppImageDefaults{NegativePrompt: "  blurry ", OutputFormat: "png", Sampler: "euler", Scheduler: "karras"}},
	},
}

// TestParseSdcppImageOptions pins the lenient decode on its own, below the
// HTTP layer: a mistyped or absent sub-field is skipped (zero value), never
// fatal, and one bad sub-field never takes its readable neighbours with it.
// It runs the shared sdcppImageOptionsCases table, which the agent's
// collector runs against its own copy of the decode.
func TestParseSdcppImageOptions(t *testing.T) {
	for _, tc := range sdcppImageOptionsCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSdcppImageOptions([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

// agentSdcppImageOptionsCasesPath is the agent's copy of
// sdcppImageOptionsCases, relative to this package's directory, where go test
// runs it. Both Go modules live in one repository, and CI checks the whole
// repository out before it tests this module (the same arrangement as
// agentSdcppCasesPath in sdcpp_capabilities_test.go).
const agentSdcppImageOptionsCasesPath = "../../../../server-agent/internal/collector/sdcpp_image_options_test.go"

// TestSdcppImageOptionsCasesMatchTheAgentsTable holds the two copies of
// sdcppImageOptionsCases identical, the options twin of
// TestSdcppImageCasesMatchTheAgentsTable. Each copy is pinned against its own
// module's parseSdcppImageOptions, so a change to one decode together with its
// own table would pass both suites while the two rules drift apart. This reads
// the agent's test file off disk and compares the two declarations byte for
// byte. Both extracts must be non-empty: a renamed or reshaped table would
// otherwise compare nothing with nothing and pass.
func TestSdcppImageOptionsCasesMatchTheAgentsTable(t *testing.T) {
	const ownPath = "sdcpp_image_options_test.go"
	own, err := os.ReadFile(ownPath)
	if err != nil {
		t.Fatalf("read %s: %v", ownPath, err)
	}
	agent, err := os.ReadFile(agentSdcppImageOptionsCasesPath)
	if err != nil {
		t.Fatalf("read the agent's copy %s: %v (the two modules are one repository, and this check needs both)", agentSdcppImageOptionsCasesPath, err)
	}
	ownBlock := sdcppVarBlock(own, "sdcppImageOptionsCases")
	agentBlock := sdcppVarBlock(agent, "sdcppImageOptionsCases")
	if len(ownBlock) == 0 {
		t.Fatalf("no `var sdcppImageOptionsCases = ...` declaration ending in a line \"}\" found in %s", ownPath)
	}
	if len(agentBlock) == 0 {
		t.Fatalf("no `var sdcppImageOptionsCases = ...` declaration ending in a line \"}\" found in %s", agentSdcppImageOptionsCasesPath)
	}
	if !bytes.Equal(ownBlock, agentBlock) {
		t.Fatalf("sdcppImageOptionsCases differs between gateway/backend/internal/provider/sdcpp_image_options_test.go and "+
			"server-agent/internal/collector/sdcpp_image_options_test.go. The two parseSdcppImageOptions copies must decode identically: "+
			"change both tables, and both decodes, together.\n--- gateway\n%s\n--- agent\n%s", ownBlock, agentBlock)
	}
}

// TestSdcppImageOptionsDoNotTouchTheStrictVerdict pins that the options probe
// is a SEPARATE decode: a document whose option fields are all malformed still
// carries a valid supported_modes, and the untouched strict decode answers
// Image:yes for it, while the lenient probe answers a best-effort value.
func TestSdcppImageOptionsDoNotTouchTheStrictVerdict(t *testing.T) {
	const doc = `{"supported_modes":["img_gen"],"model":{"stem":"flux1-dev"},"samplers":7,"limits":"wide","defaults_by_mode":["x"],"output_formats_by_mode":3}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(doc))
	}))
	defer srv.Close()
	client := NewOpenAICompatibleClient(srv.Client())
	target := routing.Target{Endpoint: srv.URL}

	verdict, err := client.ProbeSdcppCapabilities(context.Background(), target)
	if err != nil {
		t.Fatalf("ProbeSdcppCapabilities error = %v", err)
	}
	if verdict.Image != routing.CapabilityYes || verdict.ModelStem != "flux1-dev" {
		t.Fatalf("verdict = %+v, want {Image:yes ModelStem:flux1-dev} from the untouched strict decode", verdict)
	}
	opts, err := client.ProbeSdcppImageOptions(context.Background(), target)
	if err != nil {
		t.Fatalf("ProbeSdcppImageOptions error = %v, want nil", err)
	}
	if !reflect.DeepEqual(opts, SdcppImageOptions{}) {
		t.Fatalf("options = %+v, want the zero value for an all-malformed options doc", opts)
	}

	// The strict decode keeps rejecting what it always rejected: widening the
	// options decode must not have made it lenient about supported_modes.
	if _, err := parseSdcppCapabilities([]byte(`{"supported_modes":"img_gen","samplers":["euler"]}`)); err == nil {
		t.Fatal("the strict decode must still reject a supported_modes of the wrong type")
	}
}

// TestMultiplexerDispatchesSdcppImageOptionsToOpenAICompatibleClient mirrors
// TestMultiplexerDispatchesSdcppCapabilitiesToOpenAICompatibleClient: a
// stable_diffusion_cpp target reaches the shared client's probe, a provider
// whose client lacks it answers ErrUnavailable and sends nothing.
func TestMultiplexerDispatchesSdcppImageOptionsToOpenAICompatibleClient(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(sdcppFullOptionsDoc))
	}))
	defer srv.Close()
	shared := NewOpenAICompatibleClient(srv.Client())
	mux := NewMultiplexer(map[string]Client{
		routing.ProviderVLLM:               shared,
		routing.ProviderStableDiffusionCpp: shared, // same instance, exactly as providerClients wires it
		routing.ProviderOllama:             &recordingClient{},
	}, nil)

	got, err := mux.ProbeSdcppImageOptions(context.Background(), routing.Target{Provider: routing.ProviderStableDiffusionCpp, Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("ProbeSdcppImageOptions(stable_diffusion_cpp) returned %v", err)
	}
	if got.Defaults.Sampler != "euler" || got.Limits.MaxWidth != 2048 {
		t.Fatalf("got %+v, want the parsed document", got)
	}
	if len(paths) != 1 || paths[0] != "/sdcpp/v1/capabilities" {
		t.Fatalf("upstream paths = %v, want exactly [/sdcpp/v1/capabilities]", paths)
	}

	got, err = mux.ProbeSdcppImageOptions(context.Background(), routing.Target{Provider: routing.ProviderOllama, Endpoint: srv.URL})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("ProbeSdcppImageOptions(ollama) error = %v, want ErrUnavailable", err)
	}
	if !reflect.DeepEqual(got, SdcppImageOptions{}) {
		t.Fatalf("ProbeSdcppImageOptions(ollama) = %+v, want the zero value", got)
	}
	if len(paths) != 1 {
		t.Fatalf("upstream paths = %v, want no request for a client without the capability", paths)
	}

	var nilMux *Multiplexer
	if _, err := nilMux.ProbeSdcppImageOptions(context.Background(), routing.Target{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil Multiplexer error = %v, want ErrUnavailable", err)
	}
}

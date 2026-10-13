// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// sdcppImageCases is the verdict table for the stable-diffusion.cpp
// capability document. It is duplicated VERBATIM in
// gateway/backend/internal/provider/sdcpp_capabilities_test.go, because the
// two Go modules share no package and each keeps its own copy of the rule
// (parseSdcppCapabilities there, parseSdcppCapabilities here). The two
// copies must decide identically on identical input: if a case fails, fix
// the parser, not this table, and change both tables together. The gateway's
// TestSdcppImageCasesMatchTheAgentsTable fails when the two declarations
// differ by a byte.
var sdcppImageCases = []struct {
	name      string
	body      string
	wantImage string // "yes" | "no" | ""
	wantErr   bool
}{
	{"img_gen listed", `{"supported_modes":["img_gen"]}`, "yes", false},
	{"img_gen among others", `{"supported_modes":["vid_gen","img_gen"]}`, "yes", false},
	{"img_gen padded", `{"supported_modes":[" img_gen "]}`, "yes", false},
	{"exhaustive list without img_gen", `{"supported_modes":["vid_gen"]}`, "no", false},
	{"case variant is not img_gen", `{"supported_modes":["IMG_GEN"]}`, "no", false},
	{"longer mode is not img_gen", `{"supported_modes":["img_gen_edit"]}`, "no", false},
	{"empty list is exhaustive", `{"supported_modes":[]}`, "no", false},
	{"absent list is no answer", `{"model":{"stem":"flux1-dev"}}`, "", false},
	{"null list is no answer", `{"supported_modes":null}`, "", false},
	{"unrelated fields ignored", `{"supported_modes":["img_gen"],"limits":{"max":4},"output_formats":["png"]}`, "yes", false},
	{"list of the wrong type", `{"supported_modes":"img_gen"}`, "", true},
	{"non-string entry", `{"supported_modes":[1]}`, "", true},
	{"model of the wrong type", `{"supported_modes":["img_gen"],"model":"flux1-dev"}`, "", true},
	{"stem of the wrong type", `{"supported_modes":["img_gen"],"model":{"stem":1}}`, "", true},
	{"not JSON", `<html>502</html>`, "", true},
}

func TestParseSdcppCapabilitiesTwinTable(t *testing.T) {
	for _, tc := range sdcppImageCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSdcppCapabilities([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got.image != tc.wantImage {
				t.Fatalf("image = %q, want %q", got.image, tc.wantImage)
			}
		})
	}
}

// TestProbeSdcppVerdicts pins the stable/transient split. It is the SAME
// contract as ProbePropsVerdicts and ProbeOllamaVerdicts: a readable
// document, or a status that is a fixed property of the binary (404, 401,
// 403, 405), is conclusive and may be cached for the pid's lifetime.
// Anything else (5xx above all, no response, a body that is not JSON) is
// transient and must be retried.
func TestProbeSdcppVerdicts(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// The literal, not SdcppCapabilitiesPath: comparing the constant
			// with itself would not notice the constant drifting away from
			// the path a real sd-server serves.
			if r.URL.Path != "/sdcpp/v1/capabilities" || r.Method != http.MethodGet {
				t.Errorf("request %s %s, want GET /sdcpp/v1/capabilities", r.Method, r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	cases := []struct {
		name       string
		status     int
		body       string
		wantImage  string
		wantStable bool
	}{
		{"yes", 200, `{"supported_modes":["img_gen"]}`, "yes", true},
		{"no", 200, `{"supported_modes":["vid_gen"]}`, "no", true},
		{"absent list", 200, `{"model":{"stem":"x"}}`, "", true},
		{"wrong shape is conclusive", 200, `{"supported_modes":"img_gen"}`, "", true},
		{"not JSON is transient", 200, `{"supported_modes":[`, "", false},
		{"404 is conclusive", 404, ``, "", true},
		{"401 is conclusive", 401, ``, "", true},
		{"403 is conclusive", 403, ``, "", true},
		{"405 is conclusive", 405, ``, "", true},
		{"503 is transient", 503, `{"supported_modes":["img_gen"]}`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.body)
			defer srv.Close()
			got, stable := ProbeSdcppVerdicts(context.Background(), srv.Client(), srv.URL)
			if stable != tc.wantStable || got.Caps.Image != tc.wantImage || got.LiveProgress != "" {
				t.Fatalf("got image=%q live=%q stable=%v, want image=%q live=\"\" stable=%v",
					got.Caps.Image, got.LiveProgress, stable, tc.wantImage, tc.wantStable)
			}
		})
	}

	t.Run("no response is transient", func(t *testing.T) {
		srv := serve(200, `{}`)
		url := srv.URL
		srv.Close()
		if _, stable := ProbeSdcppVerdicts(context.Background(), http.DefaultClient, url); stable {
			t.Fatal("stable = true for a closed port, want false")
		}
	})
}

// sdcppFullOptionsDoc is the shape stable-diffusion.cpp's capability document
// has when a model is loaded (the same document the gateway's own tests use):
// the sampling defaults sit under defaults_by_mode.img_gen.sample_params, the
// CFG scale under that block's guidance.txt_cfg, and clip_skip, seed,
// negative_prompt, width, height and output_format directly in
// defaults_by_mode.img_gen.
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

// TestProbeSdcppVerdictsCarriesImageOptions pins that the SAME GET that yields
// the image verdict also yields the image options, and that the two decodes
// are independent: a malformed options document never degrades the verdict,
// and an unreadable supported_modes never costs the options their readable
// fields. The options ride PropsVerdicts.ImageOptions, a pointer, so that
// "the document was read" (non-nil, possibly all zero) stays distinct from
// "no document was read" (nil).
func TestProbeSdcppVerdictsCarriesImageOptions(t *testing.T) {
	serve := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/sdcpp/v1/capabilities" {
				t.Errorf("request path = %q, want /sdcpp/v1/capabilities", r.URL.Path)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		}))
	}
	fullWant := &SdcppImageOptions{
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
	cases := []struct {
		name        string
		status      int
		body        string
		wantImage   string
		wantOptions *SdcppImageOptions
		wantStable  bool
	}{
		{"a full document yields the verdict and every option", 200, sdcppFullOptionsDoc, "yes", fullWant, true},
		{
			"malformed option fields leave the verdict untouched and keep the readable options",
			200,
			`{"supported_modes":["img_gen"],"samplers":7,"limits":"wide","defaults_by_mode":["x"],"output_formats_by_mode":3,"schedulers":["karras"]}`,
			"yes",
			&SdcppImageOptions{Schedulers: []string{"karras"}},
			true,
		},
		{
			"an all-malformed options document is a read document with zero options, and the verdict stands",
			200,
			`{"supported_modes":["img_gen"],"samplers":7,"limits":"wide","defaults_by_mode":["x"],"output_formats_by_mode":3}`,
			"yes",
			&SdcppImageOptions{},
			true,
		},
		{
			"an exhaustive list without img_gen is a no, and the options still come through",
			200,
			`{"supported_modes":["vid_gen"],"samplers":["euler"]}`,
			"no",
			&SdcppImageOptions{Samplers: []string{"euler"}},
			true,
		},
		{
			"a supported_modes of the wrong type is no verdict, but the options decode independently",
			200,
			`{"supported_modes":"img_gen","samplers":["euler"]}`,
			"",
			&SdcppImageOptions{Samplers: []string{"euler"}},
			true,
		},
		{"a document with no options is a read, zero value", 200, `{"supported_modes":["img_gen"]}`, "yes", &SdcppImageOptions{}, true},
		{"not JSON is transient and carries no options", 200, `{"supported_modes":[`, "", nil, false},
		{"404 is conclusive and carries no options", 404, ``, "", nil, true},
		{"503 is transient and carries no options", 503, sdcppFullOptionsDoc, "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := serve(tc.status, tc.body)
			defer srv.Close()
			got, stable := ProbeSdcppVerdicts(context.Background(), srv.Client(), srv.URL)
			if stable != tc.wantStable || got.Caps.Image != tc.wantImage {
				t.Fatalf("got image=%q stable=%v, want image=%q stable=%v", got.Caps.Image, stable, tc.wantImage, tc.wantStable)
			}
			if !reflect.DeepEqual(got.ImageOptions, tc.wantOptions) {
				t.Fatalf("ImageOptions = %+v, want %+v", got.ImageOptions, tc.wantOptions)
			}
		})
	}
}

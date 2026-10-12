// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"reflect"
	"testing"
)

// sdcppImageOptionsCases is the options table for the stable-diffusion.cpp
// capability document. It is duplicated VERBATIM in
// gateway/backend/internal/provider/sdcpp_image_options_test.go, because the
// two Go modules share no package and each keeps its own copy of the rule
// (parseSdcppImageOptions there, parseSdcppImageOptions here). The two copies
// must decode identically on identical input: if a case fails, fix the
// decode, not this table, and change both tables together. The gateway's
// TestSdcppImageOptionsCasesMatchTheAgentsTable fails when the two
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

// TestParseSdcppImageOptionsTwinTable runs sdcppImageOptionsCases against this
// module's parseSdcppImageOptions. The gateway runs the same table (verbatim,
// held identical by its TestSdcppImageOptionsCasesMatchTheAgentsTable) against
// its own copy of the decode, so the two copies cannot drift apart unnoticed.
func TestParseSdcppImageOptionsTwinTable(t *testing.T) {
	for _, tc := range sdcppImageOptionsCases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSdcppImageOptions([]byte(tc.body))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

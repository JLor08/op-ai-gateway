// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"encoding/json"
	"strings"
)

// SdcppImageOptions is what one stable-diffusion.cpp capability document says
// about the image-generation knobs the loaded model offers: the sampler and
// scheduler names the server accepts, the output formats img_gen can encode,
// the size and batch limits, and the server's own defaults.
//
// It is the OPTIONS view of the same document sdcppVerdicts is the VERDICT
// view of, and it is decoded separately and leniently (parseSdcppImageOptions):
// a field the document lacks or states in a shape this code cannot read is
// left at its zero value, never an error. Every numeric zero therefore means
// "unknown" -- no limit, no default -- and an empty list means "the server
// named none". Samplers and Schedulers are opaque strings, passed through as
// the server spells them.
//
// The type, its fields and its json tags are a verbatim twin of the gateway's
// provider.SdcppImageOptions.
type SdcppImageOptions struct {
	Samplers      []string           `json:"samplers,omitempty"`
	Schedulers    []string           `json:"schedulers,omitempty"`
	OutputFormats []string           `json:"output_formats,omitempty"`
	Limits        SdcppImageLimits   `json:"limits"`
	Defaults      SdcppImageDefaults `json:"defaults"`
}

// SdcppImageLimits are the size and batch bounds the server states. 0 = unknown.
type SdcppImageLimits struct {
	MinWidth, MaxWidth, MinHeight, MaxHeight, MaxBatchCount int // 0 = unknown
}

// SdcppImageDefaults are the values the server applies when a request names
// none. 0 (or "") = unknown; note that the server's own ClipSkip and Seed
// defaults are -1 ("model default" and "random"), which this type carries
// faithfully rather than mapping to zero.
type SdcppImageDefaults struct {
	Sampler, Scheduler, OutputFormat, NegativePrompt string
	Steps, ClipSkip, Seed, Width, Height             int     // 0 = unknown
	CFG                                              float64 // 0 = unknown
}

// sdcppImageOptionsDoc is the part of the capability document the options
// decode reads. It is deliberately a different struct from
// sdcppCapabilitiesDoc: that one is strict and answers the verdict question,
// this one is lenient and answers the options question.
//
// The sampling defaults are NESTED under defaults_by_mode.img_gen.sample_params
// (sample_method, sample_steps, scheduler) with the CFG scale under that
// block's guidance.txt_cfg, while clip_skip, seed, negative_prompt, width,
// height and output_format sit directly in defaults_by_mode.img_gen. Output
// formats come from output_formats_by_mode.img_gen, the list for the mode this
// code serves, not the document's mode-agnostic output_formats.
type sdcppImageOptionsDoc struct {
	Limits struct {
		MinWidth      int `json:"min_width"`
		MaxWidth      int `json:"max_width"`
		MinHeight     int `json:"min_height"`
		MaxHeight     int `json:"max_height"`
		MaxBatchCount int `json:"max_batch_count"`
	} `json:"limits"`
	Samplers            []string `json:"samplers"`
	Schedulers          []string `json:"schedulers"`
	OutputFormatsByMode struct {
		ImgGen []string `json:"img_gen"`
	} `json:"output_formats_by_mode"`
	DefaultsByMode struct {
		ImgGen struct {
			NegativePrompt string `json:"negative_prompt"`
			ClipSkip       int    `json:"clip_skip"`
			Seed           int    `json:"seed"`
			Width          int    `json:"width"`
			Height         int    `json:"height"`
			OutputFormat   string `json:"output_format"`
			SampleParams   struct {
				SampleMethod string `json:"sample_method"`
				SampleSteps  int    `json:"sample_steps"`
				Scheduler    string `json:"scheduler"`
				Guidance     struct {
					TxtCFG float64 `json:"txt_cfg"`
				} `json:"guidance"`
			} `json:"sample_params"`
		} `json:"img_gen"`
	} `json:"defaults_by_mode"`
}

// parseSdcppImageOptions derives SdcppImageOptions from a capability document,
// BEST-EFFORT: it never fails. Whatever it cannot read -- a body that is not
// JSON, a sub-field of the wrong type, an absent block -- stays at its zero
// value. Callers that must tell a non-JSON body from an empty document check
// that themselves (ProbeSdcppVerdicts does, with json.Valid).
//
// The leniency is encoding/json's own: when a value does not fit its target
// type, Unmarshal skips THAT field and completes the decode as best it can, so
// the error it returns is dropped on purpose and one mistyped sub-field never
// takes its readable neighbours with it (a mistyped list entry decodes to ""
// and is dropped by cleanSdcppStrings). TestParseSdcppImageOptionsTwinTable
// pins this.
//
// This is a DUPLICATE, on purpose, of parseSdcppImageOptions in the gateway's
// gateway/backend/internal/provider/sdcpp_image_options.go, which reads the
// same document from an external sd-server. The two are separate Go modules
// and cannot share code, the same precedent as parseSdcppCapabilities in
// sdcpp.go. The sdcppImageOptionsCases table, duplicated verbatim in both
// modules' tests, pins both copies, and the gateway's
// TestSdcppImageOptionsCasesMatchTheAgentsTable holds the two tables
// identical. Whoever changes this rule must change that copy, and both
// tables, identically.
func parseSdcppImageOptions(body []byte) SdcppImageOptions {
	var doc sdcppImageOptionsDoc
	_ = json.Unmarshal(body, &doc) // best-effort by design: see above
	img := doc.DefaultsByMode.ImgGen
	return SdcppImageOptions{
		Samplers:      cleanSdcppStrings(doc.Samplers),
		Schedulers:    cleanSdcppStrings(doc.Schedulers),
		OutputFormats: cleanSdcppStrings(doc.OutputFormatsByMode.ImgGen),
		Limits: SdcppImageLimits{
			MinWidth:      doc.Limits.MinWidth,
			MaxWidth:      doc.Limits.MaxWidth,
			MinHeight:     doc.Limits.MinHeight,
			MaxHeight:     doc.Limits.MaxHeight,
			MaxBatchCount: doc.Limits.MaxBatchCount,
		},
		Defaults: SdcppImageDefaults{
			Sampler:        strings.TrimSpace(img.SampleParams.SampleMethod),
			Scheduler:      strings.TrimSpace(img.SampleParams.Scheduler),
			OutputFormat:   strings.TrimSpace(img.OutputFormat),
			NegativePrompt: img.NegativePrompt, // the server's text, kept exactly as written
			Steps:          img.SampleParams.SampleSteps,
			ClipSkip:       img.ClipSkip,
			Seed:           img.Seed,
			Width:          img.Width,
			Height:         img.Height,
			CFG:            img.SampleParams.Guidance.TxtCFG,
		},
	}
}

// cleanSdcppStrings trims each entry and drops the blank ones, which is also
// where an entry of the wrong type ends up (it decodes to ""). It returns nil,
// not an empty slice, when nothing is left, so "none named" has one spelling.
func cleanSdcppStrings(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

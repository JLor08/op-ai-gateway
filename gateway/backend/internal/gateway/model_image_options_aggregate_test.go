// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"op-ai-gateway/internal/provider"
	"reflect"
	"testing"
)

func marshalImageOptionsDTO(t *testing.T, dto ModelImageOptionsDTO) string {
	t.Helper()
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}

func TestAggregateImageOptionsEmptyInputIsEmptyObject(t *testing.T) {
	for name, in := range map[string][]provider.SdcppImageOptions{
		"nil":          nil,
		"empty":        {},
		"one zero":     {{}},
		"several zero": {{}, {}, {}},
	} {
		t.Run(name, func(t *testing.T) {
			got := aggregateImageOptions(in)
			if !reflect.DeepEqual(got, ModelImageOptionsDTO{}) {
				t.Fatalf("aggregateImageOptions = %+v, want the zero DTO", got)
			}
			if js := marshalImageOptionsDTO(t, got); js != "{}" {
				t.Fatalf("marshalled = %s, want {}", js)
			}
		})
	}
}

func TestAggregateImageOptionsIntersectsLists(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{
			Samplers:      []string{"euler", "euler_a", "dpm++2m"},
			Schedulers:    []string{"karras", "discrete"},
			OutputFormats: []string{"png", "jpeg", "webp"},
		},
		{
			Samplers:      []string{"euler_a", "dpm++2m", "lms"},
			Schedulers:    []string{"discrete", "karras"},
			OutputFormats: []string{"webp", "png"},
		},
	})
	if want := []string{"euler_a", "dpm++2m"}; !reflect.DeepEqual(got.Samplers, want) {
		t.Fatalf("Samplers = %v, want %v", got.Samplers, want)
	}
	// The order is the FIRST non-empty list's, not the second's.
	if want := []string{"karras", "discrete"}; !reflect.DeepEqual(got.Schedulers, want) {
		t.Fatalf("Schedulers = %v, want %v (first server's order)", got.Schedulers, want)
	}
	if want := []string{"png", "webp"}; !reflect.DeepEqual(got.OutputFormats, want) {
		t.Fatalf("OutputFormats = %v, want %v (first server's order)", got.OutputFormats, want)
	}
}

func TestAggregateImageOptionsIntersectsAcrossThreeMappings(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Samplers: []string{"a", "b", "c"}},
		{Samplers: []string{"b", "c", "d"}},
		{Samplers: []string{"c", "b"}},
	})
	if want := []string{"b", "c"}; !reflect.DeepEqual(got.Samplers, want) {
		t.Fatalf("Samplers = %v, want %v", got.Samplers, want)
	}
}

func TestAggregateImageOptionsEmptyListDoesNotConstrain(t *testing.T) {
	// One server reported no samplers at all: "unknown", not "supports
	// nothing", so the other server's list passes through unconstrained.
	for name, in := range map[string][]provider.SdcppImageOptions{
		"unknown first": {
			{},
			{Samplers: []string{"euler", "euler_a"}},
		},
		"unknown last": {
			{Samplers: []string{"euler", "euler_a"}},
			{},
		},
		"unknown between": {
			{Samplers: []string{"euler", "euler_a"}},
			{Samplers: []string{}},
			{Samplers: []string{"euler_a", "euler"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := aggregateImageOptions(in)
			want := []string{"euler", "euler_a"}
			if !reflect.DeepEqual(got.Samplers, want) {
				t.Fatalf("Samplers = %v, want %v", got.Samplers, want)
			}
		})
	}
}

func TestAggregateImageOptionsListsAreIndependentPerField(t *testing.T) {
	// The unknown server constrains no field it left empty, but still
	// constrains the ones it did report.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Samplers: []string{"euler", "euler_a"}, Schedulers: []string{"karras"}},
		{Samplers: []string{"euler_a"}},
	})
	if want := []string{"euler_a"}; !reflect.DeepEqual(got.Samplers, want) {
		t.Fatalf("Samplers = %v, want %v", got.Samplers, want)
	}
	if want := []string{"karras"}; !reflect.DeepEqual(got.Schedulers, want) {
		t.Fatalf("Schedulers = %v, want %v", got.Schedulers, want)
	}
	if len(got.OutputFormats) != 0 {
		t.Fatalf("OutputFormats = %v, want none (nobody reported any)", got.OutputFormats)
	}
}

func TestAggregateImageOptionsNoListAnywhereIsOmitted(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{{}, {}})
	if got.Samplers != nil || got.Schedulers != nil || got.OutputFormats != nil {
		t.Fatalf("lists = %v/%v/%v, want all nil", got.Samplers, got.Schedulers, got.OutputFormats)
	}
	if js := marshalImageOptionsDTO(t, got); js != "{}" {
		t.Fatalf("marshalled = %s, want {}", js)
	}
}

func TestAggregateImageOptionsDisjointListsAreOmitted(t *testing.T) {
	// Two servers that share no sampler: the aggregate offers none, which
	// marshals away so the client falls back to its static superset.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Samplers: []string{"euler"}},
		{Samplers: []string{"lms"}},
	})
	if len(got.Samplers) != 0 {
		t.Fatalf("Samplers = %v, want none", got.Samplers)
	}
	if js := marshalImageOptionsDTO(t, got); js != "{}" {
		t.Fatalf("marshalled = %s, want {}", js)
	}
}

func TestAggregateImageOptionsSingleMappingPassesThrough(t *testing.T) {
	in := provider.SdcppImageOptions{
		Samplers:      []string{"euler", "euler_a"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png"},
		Limits:        provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 128, MaxHeight: 1536, MaxBatchCount: 4},
		Defaults: provider.SdcppImageDefaults{
			Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
			Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 512, CFG: 7.5,
		},
	}
	got := aggregateImageOptions([]provider.SdcppImageOptions{in})
	want := ModelImageOptionsDTO{
		Samplers:      []string{"euler", "euler_a"},
		Schedulers:    []string{"karras"},
		OutputFormats: []string{"png"},
		SizeLimits:    &SizeLimitsDTO{MinWidth: 64, MaxWidth: 2048, MinHeight: 128, MaxHeight: 1536},
		MaxBatchCount: 4,
		Defaults: &ImageDefaultsDTO{
			Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
			Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 512, CFG: 7.5,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregateImageOptions = %+v (defaults %+v, limits %+v), want %+v", got, got.Defaults, got.SizeLimits, want)
	}
}

func TestAggregateImageOptionsSizeLimitsTightestEnvelope(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Limits: provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048}},
		{Limits: provider.SdcppImageLimits{MinWidth: 128, MaxWidth: 1024, MinHeight: 32, MaxHeight: 1536}},
	})
	want := &SizeLimitsDTO{MinWidth: 128, MaxWidth: 1024, MinHeight: 64, MaxHeight: 1536}
	if !reflect.DeepEqual(got.SizeLimits, want) {
		t.Fatalf("SizeLimits = %+v, want %+v (max of mins, min of maxes)", got.SizeLimits, want)
	}
}

func TestAggregateImageOptionsSizeLimitsZeroIsUnknown(t *testing.T) {
	// A zero is "unknown" and must not win min() as a bogus 0 cap, nor
	// raise a minimum.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Limits: provider.SdcppImageLimits{MaxWidth: 1024}},
		{Limits: provider.SdcppImageLimits{MinHeight: 64}},
		{},
	})
	want := &SizeLimitsDTO{MaxWidth: 1024, MinHeight: 64}
	if !reflect.DeepEqual(got.SizeLimits, want) {
		t.Fatalf("SizeLimits = %+v, want %+v", got.SizeLimits, want)
	}
	if js := marshalImageOptionsDTO(t, got); js != `{"size_limits":{"max_width":1024,"min_height":64}}` {
		t.Fatalf("marshalled = %s, want only the known limits", js)
	}
}

func TestAggregateImageOptionsSizeLimitsAllUnknownIsNil(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{{}, {Samplers: []string{"euler"}}})
	if got.SizeLimits != nil {
		t.Fatalf("SizeLimits = %+v, want nil", got.SizeLimits)
	}
}

func TestAggregateImageOptionsNegativeLimitsAreUnknown(t *testing.T) {
	// A negative limit (malformed, or an "unlimited" marker) is unknown like a
	// zero: it never becomes a bound, whichever side of min() it would have won.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Limits: provider.SdcppImageLimits{MinWidth: -1, MaxWidth: -1, MinHeight: -64, MaxHeight: -1, MaxBatchCount: -1}},
		{Limits: provider.SdcppImageLimits{MaxWidth: 1024, MaxBatchCount: 4}},
	})
	if want := (&SizeLimitsDTO{MaxWidth: 1024}); !reflect.DeepEqual(got.SizeLimits, want) {
		t.Fatalf("SizeLimits = %+v, want %+v (negative limits ignored)", got.SizeLimits, want)
	}
	if got.MaxBatchCount != 4 {
		t.Fatalf("MaxBatchCount = %d, want 4 (negative ignored)", got.MaxBatchCount)
	}
}

func TestAggregateImageOptionsOnlyNegativeLimitsAreOmitted(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Limits: provider.SdcppImageLimits{MinWidth: -1, MaxWidth: -1, MinHeight: -1, MaxHeight: -1, MaxBatchCount: -1}},
	})
	if got.SizeLimits != nil {
		t.Fatalf("SizeLimits = %+v, want nil", got.SizeLimits)
	}
	if got.MaxBatchCount != 0 {
		t.Fatalf("MaxBatchCount = %d, want 0", got.MaxBatchCount)
	}
	if js := marshalImageOptionsDTO(t, got); js != "{}" {
		t.Fatalf("marshalled = %s, want {}", js)
	}
}

func TestAggregateImageOptionsInvertedEnvelopeDropsTheAxis(t *testing.T) {
	cases := []struct {
		name string
		in   []provider.SdcppImageOptions
		want *SizeLimitsDTO
	}{
		{
			// Two servers whose width ranges do not overlap: [1024,2048] vs [64,512].
			name: "disjoint servers, one axis",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 1024, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048}},
				{Limits: provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 512, MinHeight: 128, MaxHeight: 1024}},
			},
			want: &SizeLimitsDTO{MinHeight: 128, MaxHeight: 1024},
		},
		{
			// A single malformed document: min_width above max_width.
			name: "single malformed document, one axis",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 1024, MaxWidth: 512, MinHeight: 64, MaxHeight: 2048}},
			},
			want: &SizeLimitsDTO{MinHeight: 64, MaxHeight: 2048},
		},
		{
			name: "height axis drops, width stays",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 2048, MaxHeight: 4096}},
				{Limits: provider.SdcppImageLimits{MinHeight: 64, MaxHeight: 1024}},
			},
			want: &SizeLimitsDTO{MinWidth: 64, MaxWidth: 2048},
		},
		{
			name: "every axis drops",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 1024, MaxWidth: 512, MinHeight: 1024, MaxHeight: 512}},
			},
			want: nil,
		},
		{
			name: "every axis drops across servers",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 1024, MaxWidth: 2048, MinHeight: 1024, MaxHeight: 2048}},
				{Limits: provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 512, MinHeight: 64, MaxHeight: 512}},
			},
			want: nil,
		},
		{
			// min == max is a single valid size, not an inversion.
			name: "equal bounds are kept",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 512, MaxWidth: 512}},
			},
			want: &SizeLimitsDTO{MinWidth: 512, MaxWidth: 512},
		},
		{
			// A lone bound has nothing to contradict, however large.
			name: "a lone minimum is kept",
			in: []provider.SdcppImageOptions{
				{Limits: provider.SdcppImageLimits{MinWidth: 4096}},
				{Limits: provider.SdcppImageLimits{MaxHeight: 8}},
			},
			want: &SizeLimitsDTO{MinWidth: 4096, MaxHeight: 8},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := aggregateImageOptions(tc.in)
			if !reflect.DeepEqual(got.SizeLimits, tc.want) {
				t.Fatalf("SizeLimits = %+v, want %+v", got.SizeLimits, tc.want)
			}
			if tc.want == nil {
				if js := marshalImageOptionsDTO(t, got); js != "{}" {
					t.Fatalf("marshalled = %s, want {}", js)
				}
			}
		})
	}
}

func TestAggregateImageOptionsInvertedEnvelopeKeepsTheRest(t *testing.T) {
	// Dropping an inverted axis touches nothing else: lists, batch count and
	// defaults still aggregate.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{
			Samplers: []string{"euler"},
			Limits:   provider.SdcppImageLimits{MinWidth: 1024, MaxWidth: 512, MaxBatchCount: 4},
			Defaults: provider.SdcppImageDefaults{Sampler: "euler"},
		},
	})
	want := ModelImageOptionsDTO{
		Samplers:      []string{"euler"},
		MaxBatchCount: 4,
		Defaults:      &ImageDefaultsDTO{Sampler: "euler"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("aggregateImageOptions = %+v, want %+v", got, want)
	}
}

func TestAggregateImageOptionsMaxBatchCountIsTheMinimumKnown(t *testing.T) {
	cases := []struct {
		name string
		in   []int
		want int
	}{
		{"8 vs 4", []int{8, 4}, 4},
		{"4 vs 8", []int{4, 8}, 4},
		{"zero ignored", []int{0, 8, 0}, 8},
		{"none known", []int{0, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var in []provider.SdcppImageOptions
			for _, n := range tc.in {
				in = append(in, provider.SdcppImageOptions{Limits: provider.SdcppImageLimits{MaxBatchCount: n}})
			}
			if got := aggregateImageOptions(in).MaxBatchCount; got != tc.want {
				t.Fatalf("MaxBatchCount = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestAggregateImageOptionsDefaultsDisagreementIsOmitted(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Defaults: provider.SdcppImageDefaults{Sampler: "euler", Scheduler: "karras", Steps: 20}},
		{Defaults: provider.SdcppImageDefaults{Sampler: "euler_a", Scheduler: "karras", Steps: 30}},
	})
	want := &ImageDefaultsDTO{Scheduler: "karras"}
	if !reflect.DeepEqual(got.Defaults, want) {
		t.Fatalf("Defaults = %+v, want %+v (sampler and steps disagree, scheduler agrees)", got.Defaults, want)
	}
}

func TestAggregateImageOptionsDefaultsAgreementIsKept(t *testing.T) {
	full := provider.SdcppImageDefaults{
		Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
		Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 768, CFG: 7.5,
	}
	got := aggregateImageOptions([]provider.SdcppImageOptions{{Defaults: full}, {Defaults: full}})
	want := &ImageDefaultsDTO{
		Sampler: "euler", Scheduler: "karras", OutputFormat: "png", NegativePrompt: "blurry",
		Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 768, CFG: 7.5,
	}
	if !reflect.DeepEqual(got.Defaults, want) {
		t.Fatalf("Defaults = %+v, want %+v", got.Defaults, want)
	}
}

func TestAggregateImageOptionsDefaultsEveryFieldDisagreeingIsNil(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Defaults: provider.SdcppImageDefaults{
			Sampler: "a", Scheduler: "a", OutputFormat: "a", NegativePrompt: "a",
			Steps: 1, ClipSkip: 1, Seed: 1, Width: 1, Height: 1, CFG: 1,
		}},
		{Defaults: provider.SdcppImageDefaults{
			Sampler: "b", Scheduler: "b", OutputFormat: "b", NegativePrompt: "b",
			Steps: 2, ClipSkip: 2, Seed: 2, Width: 2, Height: 2, CFG: 2,
		}},
	})
	if got.Defaults != nil {
		t.Fatalf("Defaults = %+v, want nil when no field survives", got.Defaults)
	}
	if js := marshalImageOptionsDTO(t, got); js != "{}" {
		t.Fatalf("marshalled = %s, want {}", js)
	}
}

func TestAggregateImageOptionsDefaultsUnknownDoesNotVeto(t *testing.T) {
	// A mapping that reported no value for a default (zero / "") does not
	// disagree with one that did: only mappings that reported it vote.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Defaults: provider.SdcppImageDefaults{Sampler: "euler", Steps: 20, CFG: 7.5}},
		{},
		{Defaults: provider.SdcppImageDefaults{Sampler: "euler", CFG: 7.5}},
	})
	want := &ImageDefaultsDTO{Sampler: "euler", Steps: 20, CFG: 7.5}
	if !reflect.DeepEqual(got.Defaults, want) {
		t.Fatalf("Defaults = %+v, want %+v", got.Defaults, want)
	}
}

func TestAggregateImageOptionsDefaultsNegativeValuesAreReal(t *testing.T) {
	// The server's own ClipSkip/Seed defaults are -1, a genuine value that
	// must survive agreement (only 0 means unknown) and disagree with a
	// different known value.
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{Defaults: provider.SdcppImageDefaults{ClipSkip: -1, Seed: -1}},
		{Defaults: provider.SdcppImageDefaults{ClipSkip: -1, Seed: 42}},
	})
	want := &ImageDefaultsDTO{ClipSkip: -1}
	if !reflect.DeepEqual(got.Defaults, want) {
		t.Fatalf("Defaults = %+v, want %+v", got.Defaults, want)
	}
}

func TestAggregateImageOptionsWireShape(t *testing.T) {
	got := aggregateImageOptions([]provider.SdcppImageOptions{
		{
			Samplers: []string{"euler", "euler_a"}, Schedulers: []string{"karras"}, OutputFormats: []string{"png"},
			Limits:   provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 4},
			Defaults: provider.SdcppImageDefaults{Sampler: "euler", NegativePrompt: "blurry", Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 512, CFG: 7.5},
		},
	})
	var decoded map[string]any
	if err := json.Unmarshal([]byte(marshalImageOptionsDTO(t, got)), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"samplers":       []any{"euler", "euler_a"},
		"schedulers":     []any{"karras"},
		"output_formats": []any{"png"},
		"size_limits": map[string]any{
			"min_width": 64.0, "max_width": 2048.0, "min_height": 64.0, "max_height": 2048.0,
		},
		"max_batch_count": 4.0,
		"defaults": map[string]any{
			"sampler": "euler", "negative_prompt": "blurry", "steps": 20.0, "clip_skip": -1.0,
			"seed": -1.0, "width": 512.0, "height": 512.0, "cfg": 7.5,
		},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("wire JSON = %v, want %v", decoded, want)
	}
}

func TestAggregateImageOptionsDoesNotMutateItsInputs(t *testing.T) {
	in := []provider.SdcppImageOptions{
		{Samplers: []string{"euler", "euler_a", "dpm++2m"}, Schedulers: []string{"karras", "discrete"}},
		{Samplers: []string{"dpm++2m", "euler"}, Schedulers: []string{"discrete"}},
	}
	snapshot := []provider.SdcppImageOptions{
		{Samplers: []string{"euler", "euler_a", "dpm++2m"}, Schedulers: []string{"karras", "discrete"}},
		{Samplers: []string{"dpm++2m", "euler"}, Schedulers: []string{"discrete"}},
	}
	got := aggregateImageOptions(in)
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("inputs mutated: %+v, want %+v", in, snapshot)
	}
	// Writing to the result must not write through to a shared input either.
	got.Samplers[0] = "clobbered"
	if in[0].Samplers[0] != "euler" || in[1].Samplers[0] != "dpm++2m" {
		t.Fatalf("result aliases an input list: %+v", in)
	}
}

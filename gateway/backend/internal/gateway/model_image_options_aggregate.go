// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/provider"
	"slices"
)

// ModelImageOptionsDTO is the CLIENT wire shape of the image options one model
// offers: what the sd-servers behind the model, taken together, can honour. It
// is built by aggregateImageOptions from one provider.SdcppImageOptions per
// serving sd-server.
//
// Every field is omitempty, and the nested objects are pointers, so a part the
// servers did not state drops out of the JSON entirely. An absent list or
// object means "not stated" and tells the client to fall back to its static
// superset; it never means "none supported".
type ModelImageOptionsDTO struct {
	Samplers      []string          `json:"samplers,omitempty"`
	Schedulers    []string          `json:"schedulers,omitempty"`
	OutputFormats []string          `json:"output_formats,omitempty"`
	SizeLimits    *SizeLimitsDTO    `json:"size_limits,omitempty"`
	MaxBatchCount int               `json:"max_batch_count,omitempty"`
	Defaults      *ImageDefaultsDTO `json:"defaults,omitempty"`
}

// SizeLimitsDTO is the image size envelope every serving sd-server accepts. A
// zero bound is not stated and drops out.
type SizeLimitsDTO struct {
	MinWidth  int `json:"min_width,omitempty"`
	MaxWidth  int `json:"max_width,omitempty"`
	MinHeight int `json:"min_height,omitempty"`
	MaxHeight int `json:"max_height,omitempty"`
}

// ImageDefaultsDTO holds the default values every sd-server that stated one
// agrees on. A zero (or empty) field is not stated and drops out; ClipSkip and
// Seed carry the servers' own -1 ("model default" / "random") faithfully.
type ImageDefaultsDTO struct {
	Sampler        string  `json:"sampler,omitempty"`
	Scheduler      string  `json:"scheduler,omitempty"`
	OutputFormat   string  `json:"output_format,omitempty"`
	NegativePrompt string  `json:"negative_prompt,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	ClipSkip       int     `json:"clip_skip,omitempty"`
	Seed           int     `json:"seed,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	CFG            float64 `json:"cfg,omitempty"`
}

// aggregateImageOptions combines the options of the sd-servers serving one
// model into the single answer a client may rely on for that model, whichever
// server a request lands on. It is pure: no I/O, and the inputs, which the
// caller shares and reads from elsewhere, are never written to and never
// aliased by the result.
//
// The rule is "what every server can do", with a zero value on a mapping read
// as unknown rather than as a limit:
//
//   - Samplers, Schedulers, OutputFormats: the INTERSECTION of the non-empty
//     lists, in the order of the first non-empty one. A mapping with an empty
//     list for a field does not constrain it (unknown is not "supports
//     nothing"). If no mapping states the field, or the stated lists share no
//     value, the result is empty and the field is omitted.
//   - Size limits: the tightest envelope, the largest stated minimum and the
//     smallest stated maximum per axis. A limit of zero or below is unknown and
//     ignored. An axis whose resulting minimum exceeds its maximum is dropped
//     (both bounds), so the wire never carries min > max; if every axis drops,
//     SizeLimits is nil.
//   - MaxBatchCount: the smallest positive stated value.
//   - Defaults: a field is kept only if every mapping that states it agrees.
//
// No mappings at all yield the zero DTO, which marshals to {}.
func aggregateImageOptions(perMapping []provider.SdcppImageOptions) ModelImageOptionsDTO {
	var out ModelImageOptionsDTO
	out.Samplers = intersectOptionLists(perMapping, func(o provider.SdcppImageOptions) []string { return o.Samplers })
	out.Schedulers = intersectOptionLists(perMapping, func(o provider.SdcppImageOptions) []string { return o.Schedulers })
	out.OutputFormats = intersectOptionLists(perMapping, func(o provider.SdcppImageOptions) []string { return o.OutputFormats })

	// The builtins min and max are not values, so wrap them to pass them on.
	larger := func(a, b int) int { return max(a, b) }
	smaller := func(a, b int) int { return min(a, b) }
	limits := SizeLimitsDTO{
		MinWidth:  tightestLimit(perMapping, func(l provider.SdcppImageLimits) int { return l.MinWidth }, larger),
		MaxWidth:  tightestLimit(perMapping, func(l provider.SdcppImageLimits) int { return l.MaxWidth }, smaller),
		MinHeight: tightestLimit(perMapping, func(l provider.SdcppImageLimits) int { return l.MinHeight }, larger),
		MaxHeight: tightestLimit(perMapping, func(l provider.SdcppImageLimits) int { return l.MaxHeight }, smaller),
	}
	limits.MinWidth, limits.MaxWidth = dropInvertedAxis(limits.MinWidth, limits.MaxWidth)
	limits.MinHeight, limits.MaxHeight = dropInvertedAxis(limits.MinHeight, limits.MaxHeight)
	if limits != (SizeLimitsDTO{}) {
		out.SizeLimits = &limits
	}
	out.MaxBatchCount = tightestLimit(perMapping, func(l provider.SdcppImageLimits) int { return l.MaxBatchCount }, smaller)

	defaults := ImageDefaultsDTO{
		Sampler:        agreedDefault(perMapping, func(d provider.SdcppImageDefaults) string { return d.Sampler }),
		Scheduler:      agreedDefault(perMapping, func(d provider.SdcppImageDefaults) string { return d.Scheduler }),
		OutputFormat:   agreedDefault(perMapping, func(d provider.SdcppImageDefaults) string { return d.OutputFormat }),
		NegativePrompt: agreedDefault(perMapping, func(d provider.SdcppImageDefaults) string { return d.NegativePrompt }),
		Steps:          agreedDefault(perMapping, func(d provider.SdcppImageDefaults) int { return d.Steps }),
		ClipSkip:       agreedDefault(perMapping, func(d provider.SdcppImageDefaults) int { return d.ClipSkip }),
		Seed:           agreedDefault(perMapping, func(d provider.SdcppImageDefaults) int { return d.Seed }),
		Width:          agreedDefault(perMapping, func(d provider.SdcppImageDefaults) int { return d.Width }),
		Height:         agreedDefault(perMapping, func(d provider.SdcppImageDefaults) int { return d.Height }),
		CFG:            agreedDefault(perMapping, func(d provider.SdcppImageDefaults) float64 { return d.CFG }),
	}
	if defaults != (ImageDefaultsDTO{}) {
		out.Defaults = &defaults
	}
	return out
}

// intersectOptionLists intersects the non-empty lists field selects from the
// mappings. It returns a fresh slice (never an input's backing array) in the
// first non-empty list's order without repeats, or nil when no mapping states a
// list or the stated lists share nothing.
func intersectOptionLists(perMapping []provider.SdcppImageOptions, field func(provider.SdcppImageOptions) []string) []string {
	var out []string
	started := false
	for _, m := range perMapping {
		list := field(m)
		if len(list) == 0 {
			continue // unknown, not "supports nothing": it constrains nothing
		}
		if !started {
			started = true
			for _, v := range list {
				if !slices.Contains(out, v) {
					out = append(out, v)
				}
			}
			continue
		}
		out = slices.DeleteFunc(out, func(v string) bool { return !slices.Contains(list, v) })
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// tightestLimit folds the positive values field selects from the mappings'
// limits with pick (min or max). A limit of zero or below is unknown (a missing
// field, a malformed document, or an "unlimited" marker) and never becomes a
// bound, so it cannot reach the wire. It returns 0 when no mapping states the
// limit. This is for limits only: a default such as ClipSkip or Seed keeps -1
// as a real value (agreedDefault).
func tightestLimit(perMapping []provider.SdcppImageOptions, field func(provider.SdcppImageLimits) int, pick func(a, b int) int) int {
	tightest := 0
	for _, m := range perMapping {
		v := field(m.Limits)
		switch {
		case v <= 0:
		case tightest == 0:
			tightest = v
		default:
			tightest = pick(tightest, v)
		}
	}
	return tightest
}

// dropInvertedAxis keeps the size contract min <= max honest: when both bounds
// of an axis are stated and the minimum exceeds the maximum, whether because
// two servers' ranges do not overlap or because one document is itself
// malformed, no size fits and the axis is dropped (both bounds cleared) so the
// client falls back to its static range. An axis with at most one bound stated
// has nothing to contradict and is returned unchanged.
func dropInvertedAxis(lo, hi int) (int, int) {
	if lo > 0 && hi > 0 && lo > hi {
		return 0, 0
	}
	return lo, hi
}

// agreedDefault returns the value every mapping that states the default
// (non-zero) agrees on, and the zero value when none states it or two of them
// disagree. A mapping that left it unknown (zero) takes no part in the vote.
func agreedDefault[T comparable](perMapping []provider.SdcppImageOptions, field func(provider.SdcppImageDefaults) T) T {
	var zero, agreed T
	for _, m := range perMapping {
		v := field(m.Defaults)
		switch {
		case v == zero:
		case agreed == zero:
			agreed = v
		case v != agreed:
			return zero
		}
	}
	return agreed
}

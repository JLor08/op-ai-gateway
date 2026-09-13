// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// liveTimingsPositiveTarget is the ONE target shape that must answer true: an
// ordinary llama.cpp application whose operator opt-in is on and whose
// live-progress verdict was never determined. Every row of the table below is
// this value with exactly ONE of the five conditions changed, which is what
// makes a row a test of one condition instead of a test of the conjunction.
// (The two server_agent rows move Provider and LiveProgressSpecType together,
// because a server_agent Provider with no spec type is a DIFFERENT condition-2
// row and is listed separately.)
//
// APIFlavor is deliberately left empty. That field carries the COARSE flavor --
// routing.Resolver.Resolve fills it from NormalizeAPIFlavor, so it is "openai"
// or "anthropic" and cannot tell /v1/responses from /v1/chat/completions -- so
// a gate that read it instead of the endpoint's own flavor would be answering a
// different question. Leaving it empty makes that shortcut fail THIS row, the
// first one, rather than hiding until someone adds a chat-completions caller.
func liveTimingsPositiveTarget() routing.Target {
	return routing.Target{
		RouteID:                     "map_live_timings",
		Provider:                    routing.ProviderLlamaCPP,
		ResponsesLiveTimingsEnabled: true,
	}
}

// TestWantsResponsesLiveTimings walks the gate's five conditions with a row per
// condition FAILING ALONE, because the whole hazard of this predicate is that a
// forgotten conjunct leaves every other test in the repository green: the two
// standing "the relayed body must not grow a timings_per_token flag"
// assertions on the passthrough path hold for a fixture whose opt-in is off,
// so they stay silent for a gate that forgets the flavor, the stream or the
// veto. This table is the only thing that does not.
//
// Which row isolates what:
//
//   - condition 1 (the operator's opt-in): "the operator opt-in is off".
//   - condition 2 (the effective kind is llama.cpp): the four rows naming a
//     kind. "vLLM is not a capable kind" also pins issue #81's D6 -- the key is
//     a llama.cpp parameter that a vLLM upstream was measured accepting with a
//     200 and answering with no timings object on any frame. The two positive
//     rows are the other half of the pair: "a server_agent child whose
//     effective kind is llama.cpp" fails a gate that asks about Provider alone,
//     and "baseline" fails a gate that asks about LiveProgressSpecType alone.
//   - condition 3 (the Responses flavor): the three flavor rows. "no flavor at
//     all" fails a gate written as "not anthropic_messages"; "the coarse flavor
//     is not the endpoint's own" fails one written over NormalizeAPIFlavor; and
//     "the Anthropic Messages flavor" carries a coarse "openai" on the TARGET,
//     so it fails a gate that reads target.APIFlavor instead of the parameter.
//   - condition 4 (streaming): "a buffered request".
//   - condition 5 (the stored verdict is not an explicit negative): the veto is
//     a veto, not a requirement, so BOTH "" (the baseline) and "supported"
//     inject. Only "unsupported" refuses. The "no" row is the vocabulary
//     boundary: routing.LiveProgressSupportFromVerdict is the single producer
//     of this field and translates the capability row's routing.CapabilityNo
//     INTO "unsupported", so a raw "no" cannot reach a real Target -- the row
//     exists to fail a veto written against routing.CapabilityNo, which would
//     never refuse anything.
func TestWantsResponsesLiveTimings(t *testing.T) {
	cases := []struct {
		// name says which condition the row isolates; it is also what the
		// failure prints, so it has to read as a claim.
		name string
		// mutate applies the one change that distinguishes this row from
		// liveTimingsPositiveTarget. nil means the untouched positive.
		mutate    func(*routing.Target)
		apiFlavor string
		stream    bool
		want      bool
	}{
		// ---- the positives ----
		{
			name:      "baseline: llama.cpp application, opt-in on, verdict never determined",
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name: "a server_agent child whose effective kind is llama.cpp",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeLlamaCpp)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "a recorded positive verdict changes nothing",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = "supported" },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "the capability row's own \"no\" is not this field's vocabulary",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = routing.CapabilityNo },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "an ordinary application's spec type is never consulted",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeCustom) },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},

		// ---- one condition failing alone, per condition ----
		{
			name:      "the operator opt-in is off",
			mutate:    func(tgt *routing.Target) { tgt.ResponsesLiveTimingsEnabled = false },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "vLLM is not a capable kind",
			mutate:    func(tgt *routing.Target) { tgt.Provider = routing.ProviderVLLM },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name: "a server_agent child whose effective kind is vLLM",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeVLLM)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name: "a server_agent child with no spec at all, which resolves to custom",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeCustom)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "a server_agent target whose spec type was never filled",
			mutate:    func(tgt *routing.Target) { tgt.Provider = routing.ProviderServerAgent },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "the Anthropic Messages flavor",
			mutate:    func(tgt *routing.Target) { tgt.APIFlavor = routing.APIFlavorOpenAI },
			apiFlavor: "anthropic_messages",
			stream:    true,
			want:      false,
		},
		{
			name:      "no flavor at all",
			apiFlavor: "",
			stream:    true,
			want:      false,
		},
		{
			name:      "the coarse flavor is not the endpoint's own",
			apiFlavor: routing.APIFlavorOpenAI,
			stream:    true,
			want:      false,
		},
		{
			name:      "a buffered request",
			apiFlavor: "openai_responses",
			stream:    false,
			want:      false,
		},
		{
			name:      "a recorded rejection vetoes the opt-in",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = "unsupported" },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
	}
	for _, tc := range cases {
		target := liveTimingsPositiveTarget()
		if tc.mutate != nil {
			tc.mutate(&target)
		}
		if got := wantsResponsesLiveTimings(target, tc.apiFlavor, tc.stream); got != tc.want {
			t.Errorf("%s: wantsResponsesLiveTimings(target{provider=%q spec_type=%q enabled=%v verdict=%q coarse_flavor=%q}, %q, stream=%v) = %v, want %v",
				tc.name, target.Provider, target.LiveProgressSpecType, target.ResponsesLiveTimingsEnabled,
				target.LiveProgressSupport, target.APIFlavor, tc.apiFlavor, tc.stream, got, tc.want)
		}
	}
}

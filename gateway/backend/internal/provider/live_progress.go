// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import "op-ai-gateway/internal/routing"

// liveProgressUpstreams lists the application types verified to tolerate the two
// extra parameters this gateway adds to the STREAMING body it builds itself:
// llama.cpp's `timings_per_token` and `stream_options.continuous_usage_stats`.
// Both exist to get an EXACT mid-stream output-token count; without one, the
// portal shows "not measured" rather than a guess.
//
// The gate exists because tolerance is not universal:
//   - llama.cpp: its request schema is PULL-based (it iterates its own field list
//     and looks each name up), so a key nobody asks for is never inspected. Its
//     `stream_options` is a nested field that reads only its own subfields, so the
//     unknown `continuous_usage_stats` subkey is inert there.
//   - vLLM: OpenAIBaseModel is ConfigDict(extra="allow") and merely debug-logs
//     unknown keys; `continuous_usage_stats` is a first-class StreamOptions field
//     (inert unless `include_usage` is also set, which this client always sets).
//   - LiteLLM: FORWARDS unknown body keys downstream, packing them into
//     `extra_body` for OpenAI/Azure, which answer 400 "Unrecognized request
//     argument supplied" -- and this client turns that into an
//     unavailable-upstream failure for the ENTIRE request. Never send them there.
//
// The default is therefore OFF. A provider value that is not listed gets neither
// parameter, so a type added later is never a silent opt-in.
var liveProgressUpstreams = map[string]struct{}{
	routing.ProviderLlamaCPP:    {},
	routing.ProviderLlamaSwap:   {},
	routing.ProviderVLLM:        {},
	routing.ProviderServerAgent: {},
}

// wantsLiveProgress reports whether the streaming request body for target may
// carry the live-progress parameters.
func wantsLiveProgress(target routing.Target) bool {
	_, ok := liveProgressUpstreams[target.Provider]
	return ok
}

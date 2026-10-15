// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

// applyChatTemplateEffortPolicy confines Claude-effort translation to the backends
// the feature is scoped to -- llama.cpp and vLLM, the ones whose chat template reads
// chat_template_kwargs (see targetUsesChatTemplateKwargs). For such a target it sets
// req.ReasoningEffortViaTemplateKwargs, and the provider then sends the effort as
// chat_template_kwargs.reasoning_effort in addition to the top-level reasoning_effort,
// which is what makes a chat-template-driven backend (Qwen) actually apply it.
//
// For any OTHER resolved Anthropic-translate target it CLEARS req.ReasoningEffort.
// The effort is only ever populated on this path when the token's Claude-effort flag
// was on (compat.ParseAnthropicMessagesWithOptions reads output_config only then), so
// a set ReasoningEffort here can only have come from this feature -- clearing it
// removes exactly what the feature added and nothing the client sent. Without this,
// the feature would leak off its scope: a generic/vendor OpenAI target would receive
// a top-level reasoning_effort (openAISamplingFields), a Gemini target would get a
// thinkingConfig (geminiwire), and a Responses target would override its effort
// default -- silent behavior and cost changes on routes the operator only enabled the
// flag for Qwen on (§3 scope / §4.7 "other backends unchanged").
//
// The APIFlavor gate is what keeps a request that is NOT an Anthropic translate -- a
// direct OpenAI chat or Responses request that merely set a reasoning effort of its
// own -- out of this policy entirely, so its field is left exactly as the caller sent
// it and neither cleared nor redirected into the directive.
//
// Like applyAnthropicCachePolicy it is handed the UPSTREAM copy of the request, never
// the client-facing one, so neither the directive nor the clear leaks into what is
// echoed back or recorded in usage.
func applyChatTemplateEffortPolicy(target routing.Target, req *inference.Request) {
	if req.APIFlavor != "anthropic_messages" {
		return
	}
	if req.ReasoningEffort == "" {
		return
	}
	if !targetUsesChatTemplateKwargs(target) {
		req.ReasoningEffort = ""
		return
	}
	req.ReasoningEffortViaTemplateKwargs = true
}

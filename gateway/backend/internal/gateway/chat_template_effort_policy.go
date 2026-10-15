// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
)

// applyChatTemplateEffortPolicy sets req.ReasoningEffortViaTemplateKwargs when an
// Anthropic-translate request carries an effort AND the resolved target reads
// chat_template_kwargs (llama.cpp / vLLM, see targetUsesChatTemplateKwargs). The
// provider then sends the effort as chat_template_kwargs.reasoning_effort in
// addition to the top-level reasoning_effort, which is what makes a
// chat-template-driven backend (Qwen) actually apply it.
//
// The effort is only ever populated on this path when the token's Claude-effort
// flag was on (compat.ParseAnthropicMessagesWithOptions reads output_config only
// then), so this needs no token check of its own. The APIFlavor gate is what
// keeps a request that is NOT an Anthropic translate -- a direct OpenAI chat or
// Responses request that merely set a reasoning effort, addressed to a target
// that already understands the top-level field -- from acquiring the directive.
//
// Like applyAnthropicCachePolicy it is handed the UPSTREAM copy of the request,
// never the client-facing one, so the directive does not leak into what is
// echoed back or recorded in usage.
func applyChatTemplateEffortPolicy(target routing.Target, req *inference.Request) {
	if req.APIFlavor != "anthropic_messages" {
		return
	}
	if req.ReasoningEffort == "" {
		return
	}
	if !targetUsesChatTemplateKwargs(target) {
		return
	}
	req.ReasoningEffortViaTemplateKwargs = true
}

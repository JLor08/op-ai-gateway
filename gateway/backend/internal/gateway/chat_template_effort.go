// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "op-ai-gateway/internal/routing"

// targetUsesChatTemplateKwargs reports whether the resolved target's backend
// honors chat_template_kwargs.reasoning_effort: llama.cpp or vLLM, whether
// registered directly or launched by an agent-managed runtime. The #230
// effort-compatibility paths use it to decide where Claude Code's effort level
// may be forwarded as a chat-template kwarg instead of being dropped.
//
// The kind under test is the EFFECTIVE upstream kind, found the same way
// wantsResponsesLiveTimings finds it: target.Provider for an ordinary
// application, and target.LiveProgressSpecType -- the resolved spec's
// routing.EffectiveRuntimeSpecType, filled by routing.Resolver.targetFrom for a
// server_agent mapping only -- when Provider is the literal "server_agent",
// which says nothing about what actually serves. Both reads are load-bearing:
// a DIRECT llama.cpp or vLLM application has an empty LiveProgressSpecType, so
// testing that field alone would miss it.
//
// The comparison is deliberate string equality against exactly two kinds, not a
// substring or family match. llama_swap proxies to arbitrary downstreams, so it
// is not llama.cpp and must not be selected; ollama, TGI, custom runtimes,
// generic providers and the vendor APIs do not read the kwarg either. This is
// NOT routing.LiveTimingsCapableKind: that is a different policy (which
// upstreams accept llama.cpp's live-progress parameters) whose membership only
// coincides with this one's for llama.cpp today. The equality relies on
// routing.RuntimeSpecTypeLlamaCpp and routing.RuntimeSpecTypeVLLM spelling the
// same strings as routing.ProviderLlamaCPP and routing.ProviderVLLM, which
// TestChatTemplateKwargsKindsShareOneStringAcrossBothVocabularies pins.
func targetUsesChatTemplateKwargs(target routing.Target) bool {
	kind := target.Provider
	if target.Provider == routing.ProviderServerAgent {
		kind = target.LiveProgressSpecType
	}
	return kind == routing.ProviderLlamaCPP || kind == routing.ProviderVLLM
}

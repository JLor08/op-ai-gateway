// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// liveTimingsCapableKinds is the closed set of upstream kinds whose request
// schema is known to tolerate a live-progress request parameter, and therefore
// the set for which a NEWLY CREATED application or runtime spec gets the
// responses live-timings opt-in switched on by default.
//
// It is keyed on the string value BOTH vocabularies share: ProviderLlamaCPP ==
// "llama_cpp" == RuntimeSpecTypeLlamaCpp, and likewise ProviderVLLM == "vllm"
// == RuntimeSpecTypeVLLM. So one lookup serves an ordinary application's Type
// and a server_agent child's EffectiveRuntimeSpecType alike -- which is exactly
// the trick internal/provider's liveProgressUpstreams already relies on, and
// why TestLiveProgressUpstreamsMatchesRoutingCapableKinds pins the two sets
// together rather than letting a second hand-written list drift.
//
// Deliberately NOT listed: server_agent (not an inference server at all -- ask
// EffectiveRuntimeSpecType what actually serves), llama_swap and litellm (both
// resolve a model to an arbitrary downstream that can be api.openai.com, which
// answers 400 on an unrecognized body key), ollama, tgi, custom, mock. The
// default is OFF, so a kind added later never opts in silently.
var liveTimingsCapableKinds = map[string]struct{}{
	ProviderLlamaCPP: {},
	ProviderVLLM:     {},
}

// LiveTimingsCapableKind reports whether kind -- an Application.Type or the
// string form of an EffectiveRuntimeSpecType -- is one of the kinds above.
// Exported because the portal's create paths are its callers and they live in
// another package; it is a statement about a KIND, not a read of any stored
// flag, so nothing about it belongs to the request path.
//
// The argument must already be normalized: the lookup is a plain map hit, so it
// is case-sensitive AND whitespace-sensitive. Every stored Type has been
// through portal.normalizeApplicationType or portal.validRuntimeSpecType, both
// of which trim, so callers on the write paths have nothing left to trim.
func LiveTimingsCapableKind(kind string) bool {
	_, ok := liveTimingsCapableKinds[kind]
	return ok
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// liveTimingsCapableKinds is the closed set of upstream kinds whose
// /v1/responses implementation is known to ANSWER a live-progress request
// parameter with per-token timings, and therefore the set for which a NEWLY
// CREATED application or runtime spec gets the responses live-timings opt-in
// switched on by default.
//
// ONE member. It is keyed on the string value BOTH vocabularies share --
// ProviderLlamaCPP == "llama_cpp" == RuntimeSpecTypeLlamaCpp -- so one lookup
// serves an ordinary application's Type and a server_agent child's
// EffectiveRuntimeSpecType alike. ProviderVLLM == "vllm" == RuntimeSpecTypeVLLM
// holds in exactly the same way and is still pinned by
// TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies, because
// internal/provider's gate still needs that equality -- it just no longer buys
// a membership here.
//
// THIS SET IS NO LONGER internal/provider's liveProgressUpstreams. The two
// were one set for one reason: both were read as "does this kind TOLERATE the
// parameter?". Measured on 2026-09-12 against a live vLLM upstream serving
// /v1/responses through this gateway, a streamed request carrying
// timings_per_token produced 48 data frames and NOT ONE of them carried a
// top-level timings object -- neither the partials nor the terminal frame.
// vLLM accepts the key (its request models allow unknown fields) and does
// nothing with it. Tolerance was never the question THIS set answers;
// DELIVERY is, because a switch that is offered, defaults on for new
// applications, and provably delivers nothing is worse than one that is not
// offered. (One deployment, one build, whose build identifier was not
// recorded -- the same scope caveat the repository already applies to #80's
// measurement. Record the build before revisiting this.)
//
// liveProgressUpstreams keeps vllm on purpose: it gates a different parameter
// PAIR (timings_per_token AND stream_options.continuous_usage_stats) on a
// different endpoint (/v1/chat/completions), and continuous_usage_stats is a
// first-class vLLM field that works there. So the relationship is now "every
// kind here is also in the gate", with exactly one recorded divergence in the
// other direction. internal/provider's
// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind pins that relation,
// and that vllm is the ONLY divergence -- but it does NOT pin that vllm is
// still in the gate: remove it there and all three of that test's loops stay
// silent. What fails then is three expectations in two other tests of that
// package, TestWantsLiveProgressAllowList and
// TestWantsLiveProgressThreeLayerRule twice. Do not re-level the two lists
// into one.
//
// The surviving relation has one blind spot, named here so it is not met as a
// surprise: it spans TWO endpoints. A kind measured to ANSWER
// timings_per_token on /v1/responses while rejecting the gate's parameter pair
// on /v1/chat/completions would belong HERE and not in the gate, and the
// parity test's subset loop would report that correct configuration as an
// error. Loop 1's recorded-divergence escape hatch covers only the opposite
// direction, so such a kind needs a second exception, argued for there the way
// vllm's was.
//
// Deliberately NOT listed: vllm (above), server_agent (not an inference server
// at all -- ask EffectiveRuntimeSpecType what actually serves), llama_swap and
// litellm (both resolve a model to an arbitrary downstream that can be
// api.openai.com, which answers 400 on an unrecognized body key), ollama, tgi,
// custom, mock, and "" -- the empty spec type, which means "auto-detect from
// the binary" and is a legitimate STORED value, so it has to be answered for
// rather than assumed absent. That is all ten strings the two vocabularies
// hold between them, minus the one member above. The default is OFF, so a kind
// added later never opts in silently.
var liveTimingsCapableKinds = map[string]struct{}{
	ProviderLlamaCPP: {},
}

// LiveTimingsCapableKind reports whether kind -- an Application.Type or the
// string form of an EffectiveRuntimeSpecType -- is one of the kinds above.
// Exported because all five of its callers live in other packages, and the
// portal holds four of them -- UPDATE paths included, not creates alone.
// portal.CreateApplication asks once and uses the answer twice, to pick the
// stored default and to refuse an explicit true. portal.UpdateApplication asks
// twice: once to refuse an asserted true, and once to clear a stored true that
// an incapable retype left behind. portal.putRuntimeSpec asks in its own
// refusal arm, judging the spec's EFFECTIVE type. The fifth caller is issue
// #81's part-2 gate, gateway.wantsResponsesLiveTimings, which re-asks about the
// EFFECTIVE kind of an already-resolved target -- and that gate IS wired into
// the request path: gateway.proxyNative consults it for every native
// passthrough request, so changing this set changes what real traffic carries
// upstream, not only what the portal accepts and stores. It is still a statement
// about a KIND and reads no stored flag: every caller supplies the kind and
// combines the answer with the stored opt-in itself, which is exactly what lets
// the store stay policy-free about which kinds may hold a true.
//
// The argument must already be normalized: the lookup is a plain map hit, so it
// is case-sensitive AND whitespace-sensitive. Nothing untrimmed can reach a
// stored Type -- but the two write paths arrive there by DIFFERENT mechanisms,
// and only one of them trims inside the validator:
// portal.normalizeApplicationType trims its own input (it switches over
// strings.TrimSpace(raw), so every value it returns is already trimmed),
// while portal.validRuntimeSpecType trims nothing at all -- the trim lives in
// its caller instead: portal.putRuntimeSpec, the only function that calls it,
// binds specType from strings.TrimSpace(req.Type) BEFORE validating it and
// stores that same trimmed local as the spec's Type. Either way a caller on a
// write path has nothing left to trim.
func LiveTimingsCapableKind(kind string) bool {
	_, ok := liveTimingsCapableKinds[kind]
	return ok
}

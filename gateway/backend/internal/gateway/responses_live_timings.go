// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "op-ai-gateway/internal/routing"

// liveTimingsVerdictUnsupported is the ONE spelling of "this upstream was
// observed rejecting the live-progress parameters" that can ever reach
// routing.Target.LiveProgressSupport. That field's vocabulary is closed --
// "" (never determined), "supported", "unsupported" -- and has a single
// producer, routing.LiveProgressSupportFromVerdict, which all four writers of
// the field go through: the candidate join in both store drivers, plus the two
// dedicated keyed reads on the paths that have no candidate to join --
// routing.Resolver.resolveAffinity's and the benchmark path's
// (Server.benchmarkLiveProgressSupport). The capability ROW is spelled
// differently ("yes"/"no", routing.CapabilityYes/CapabilityNo) and is what that
// helper CONSUMES, never what it produces -- so a veto written against
// routing.CapabilityNo would compile, read plausibly, and never fire.
const liveTimingsVerdictUnsupported = "unsupported"

// wantsResponsesLiveTimings reports whether this request's outgoing body may
// carry llama.cpp's `timings_per_token`. It is a pure function of the resolved
// target, the CLIENT API flavor the dispatch layer routed on and the stream
// flag -- no clock, no store, no memo -- so the whole rule is table-testable
// (TestWantsResponsesLiveTimings), which matters because a forgotten conjunct
// here leaves the rest of the suite green.
//
// All five must hold:
//
//  1. target.ResponsesLiveTimingsEnabled -- the operator's per-endpoint opt-in,
//     resolved spec-over-application by routing.Resolver.targetFrom. It is the
//     only condition an operator controls, and the feature is off without it.
//  2. The EFFECTIVE upstream kind is one routing.LiveTimingsCapableKind
//     accepts: llama.cpp alone. `timings_per_token` is a llama.cpp parameter,
//     and a vLLM upstream was measured answering 200 to it and attaching no
//     `timings` object to any frame of the resulting stream -- offered,
//     accepted, inert. For a server_agent target the kind is
//     target.LiveProgressSpecType (the resolved spec's
//     EffectiveRuntimeSpecType), NEVER target.Provider, which is the literal
//     "server_agent" and says nothing about what actually serves; that is the
//     same substitution internal/provider's wantsLiveProgress makes for its own
//     shape clause. The kind is re-checked HERE rather than trusted from the
//     stored flag because the store is deliberately policy-free: the portal
//     refuses a true on an incapable kind, but every store path round-trips one
//     for any kind, so a restored dump or a direct write can present a true
//     this path must not act on.
//  3. The flavor is the Responses endpoint's own. proxyNative serves
//     /v1/responses and /v1/messages from one function, and /v1/messages is out
//     of scope for issue #81 -- nothing was measured about whether a llama.cpp
//     Anthropic frame carries a `timings` object at all, so the gate refuses
//     rather than guesses. The FINE flavor is a parameter rather than
//     target.APIFlavor because that field carries the COARSE value
//     (routing.Resolver.Resolve fills it from NormalizeAPIFlavor), which cannot
//     tell this endpoint from /v1/chat/completions.
//  4. The request is streaming. The flag exists to put a rate on a PARTIAL
//     frame; a buffered response has none, and proxyNative allocates no
//     live-progress counter for one.
//  5. The stored live-progress verdict is not an explicit negative. This is a
//     VETO, not a requirement. Requiring a positive verdict would make the
//     operator's switch silently dead wherever the capability probe never ran
//     -- the worst possible failure for a control that is switched on -- and
//     within condition 2's population a positive verdict allows nothing the
//     veto has not allowed already. The verdict is a /props observation about
//     the COMPLETION endpoint's parameter schema, so it is trusted to refuse
//     and not to permit.
//
// Deliberately NOT internal/provider's wantsLiveProgress, whose shape this
// resembles: that predicate's verdict layer decides in BOTH directions -- a
// "supported" verdict there OVERRIDES its shape clause and permits a kind the
// shape would have refused, where here a positive verdict permits nothing the
// veto has not allowed already -- its own set still includes vLLM (correctly --
// it gates a different parameter pair on a different endpoint, where vLLM's
// half is a first-class field), and a fourth check, its rejection memo, is
// ANDed at its only call site that a caller from here would silently drop.
func wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool {
	if !target.ResponsesLiveTimingsEnabled {
		return false
	}
	// The same literal endpointModeFor switches on for this endpoint. A rename
	// there fails this shut rather than open, which is the safe direction.
	if apiFlavor != "openai_responses" {
		return false
	}
	if !stream {
		return false
	}
	if target.LiveProgressSupport == liveTimingsVerdictUnsupported {
		return false
	}
	kind := target.Provider
	if target.Provider == routing.ProviderServerAgent {
		kind = target.LiveProgressSpecType
	}
	return routing.LiveTimingsCapableKind(kind)
}

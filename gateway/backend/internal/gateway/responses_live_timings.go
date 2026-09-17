// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
)

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
//     of scope for issue #81. This repository states in several places -- among
//     them usageScanner.usage and parsePassthroughUsage in this package, and the
//     telemetry architecture document -- that llama.cpp attaches no `timings` to
//     any Anthropic frame, and the derived-rate fallback for that shape is built
//     on it. But none of those sites cites a MEASUREMENT for it, where the same
//     document records the Responses side's attachment with a build identifier,
//     a frame count and a without-flag replay. So this exclusion rests on the
//     design's say-so rather than on a verified upstream fact: reason enough to
//     refuse, not reason enough to widen the gate later without measuring first.
//     The FINE flavor is a parameter rather than target.APIFlavor because that
//     field carries the COARSE value (routing.Resolver.Resolve fills it from
//     NormalizeAPIFlavor), which cannot tell this endpoint from
//     /v1/chat/completions.
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
// veto has not allowed already -- and its own set still includes vLLM
// (correctly -- it gates a different parameter pair on a different endpoint,
// where vLLM's half is a first-class field).
//
// Its FOURTH check, the in-process rejection memo, is ANDed at its only call
// site, and this predicate deliberately does not fold it in either: an earlier
// cut of this comment warned that "a caller from here would silently drop" it,
// and a caller did, for two releases. The fix keeps the shape and adds the
// conjunct where the precedent puts it -- liveProgressRejectedFor below, ANDed
// at proxyNative's call site -- because folding a clock-and-state lookup in here
// would cost the purity that makes TestWantsResponsesLiveTimings a statement
// about the WHOLE rule. Read the two together: this function is what the
// operator's configuration permits, the memo is what this process has observed.
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

// liveProgressRejectedFor reports whether THIS process has already seen target's
// upstream refuse the live-progress parameters -- the sixth condition on the
// injection, ANDed at proxyNative's call site rather than folded into
// wantsResponsesLiveTimings above.
//
// It is the same memo internal/provider's CompleteStream consults at
// `wantsLiveProgress(target) && !c.liveProgress.rejects(target.RouteID)`, reached
// through the exported provider.LiveProgressRejectionMemo seam, which is what
// makes the two endpoints agree: production registers ONE
// OpenAICompatibleClient under every OpenAI-compatible provider key inside one
// Multiplexer, and Multiplexer.dispatchProxyNative and dispatchCompleteStream
// resolve the same m.clients[target.Provider] entry. So a refusal this path
// records stops /v1/chat/completions asking, and one CompleteStream's retry
// recorded stops this path asking.
//
// The capability is OPTIONAL, exactly like provider.NativeProxyClient two lines
// above it at the call site: a provider that cannot remember reports "nothing
// recorded", which is the state that means "send them". So the failure mode of a
// client without the seam is the behaviour this gate had before the seam existed,
// never a refusal an operator cannot explain.
//
// Unlike the stored "unsupported" verdict (condition 5) this observation EXPIRES:
// the memo's TTL is 5 minutes, so an upstream that was replaced or reconfigured
// behind the same mapping starts being asked again without a gateway restart.
// That is the whole reason a rejection is memoized rather than persisted as a
// capability row -- a row would outlive the build that earned it.
func liveProgressRejectedFor(p provider.Client, target routing.Target) bool {
	memo, ok := p.(provider.LiveProgressRejectionMemo)
	if !ok {
		return false
	}
	return memo.LiveProgressRejected(target)
}

// recordLiveProgressRejection notes, through the same optional seam, that
// target's upstream refused a body carrying the key THIS gateway injected.
//
// Two guards belong to the caller and are stated here because getting either
// wrong is silent: it must have INJECTED (a 400 the client's own
// `timings_per_token` earned is not evidence about the gateway's key, and
// blaming it would suppress the figure for a mapping that never objected), and
// the status must be provider.SchemaRejectionStatus -- 400 or 422 alone. This
// path has no retry to discover it guessed wrong, so a 401, 404 or 429 recorded
// here would cost the mapping its live figure for the TTL over something the key
// had no part in.
func recordLiveProgressRejection(p provider.Client, target routing.Target) {
	if memo, ok := p.(provider.LiveProgressRejectionMemo); ok {
		memo.RecordLiveProgressRejection(target)
	}
}

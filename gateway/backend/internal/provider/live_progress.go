// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// liveProgressUpstreams is the "shape genuinely implies a tolerant upstream"
// clause of wantsLiveProgress's three-layer rule -- the WEAKEST of the three
// layers, consulted only when the mapping's persisted verdict
// (routing.Target.LiveProgressSupport, filled by routing.Resolver.targetFrom
// from the joined "live_progress" row in model_mapping_capabilities --
// routing.MappingCandidate.LiveProgressSupport, via
// routing.LiveProgressSupportFromVerdict; the pre-migration-78
// live_progress_support column it replaced is gone, dropped by migration 79)
// has never been determined ("").
// A recorded verdict, "supported" or "unsupported", always overrides it: that
// verdict is either an observed upstream answer or CompleteStream's own
// retry-confirmed rejection, and an observation outranks a guess about the
// application type either way. See wantsLiveProgress for the full rule.
//
// The two extra parameters this clause is about are the ones this gateway adds
// to the STREAMING body it builds itself: llama.cpp's `timings_per_token` and
// `stream_options.continuous_usage_stats`. Both exist to get an EXACT
// mid-stream output-token count; without one, the portal shows "not measured"
// rather than a guess.
//
// It is deliberately narrow, keyed on ONLY llama_cpp and vllm, because an
// application TYPE does not otherwise imply the upstream's request schema:
//   - `server_agent` is not an inference server at all. What actually serves is
//     whatever `routing.EffectiveRuntimeSpecType` resolves the spec to --
//     `llama_cpp | vllm | tgi | ollama | custom` -- so a server_agent target is
//     tested against target.LiveProgressSpecType (that resolved value, filled by
//     routing.Resolver.targetFrom), never against target.Provider, which is
//     always the literal "server_agent" and says nothing about the child. The
//     map is keyed on the same two string values either clause needs
//     (routing.ProviderLlamaCPP == "llama_cpp" == routing.RuntimeSpecTypeLlamaCpp,
//     and likewise routing.ProviderVLLM == "vllm" == routing.RuntimeSpecTypeVLLM),
//     so one lookup serves both.
//   - `llama_swap` is a proxy. It resolves each model either to a free-text `cmd`
//     (any OpenAI-compatible server) or to a `peer` at an arbitrary base URL with
//     an injected `Authorization: Bearer` -- llama-swap's own configuration example
//     uses OpenRouter. So a `llama_swap` model can terminate at api.openai.com,
//     which is exactly the case `litellm` is excluded for. Its TYPE says nothing
//     about what actually answers, so it stays off this list entirely (a
//     llama_swap mapping can still send the parameters, but only on an observed
//     "supported" verdict -- never from shape alone).
//
// What is known about the two listed kinds when they DO serve directly:
//   - llama.cpp: its request schema is PULL-based (it iterates its own field list
//     and looks each name up), so a key nobody asks for is never inspected. Its
//     `stream_options` is a nested field that reads only its own subfields, so the
//     unknown `continuous_usage_stats` subkey is inert there.
//   - vLLM: OpenAIBaseModel is ConfigDict(extra="allow") and merely debug-logs
//     unknown keys; `continuous_usage_stats` is a first-class StreamOptions field
//     (inert unless `include_usage` is also set, which this client always sets).
//   - LiteLLM: FORWARDS unknown body keys downstream, packing them into
//     `extra_body` for OpenAI/Azure, which answer 400 "Unrecognized request
//     argument supplied". Listing it would buy nothing but a guaranteed wasted
//     round trip on every request, which is why it stays off.
//
// The default is therefore OFF: a value not listed here contributes nothing to
// the shape clause, so a kind added later never opts in silently -- it just does
// not get the (advisory) number from shape alone until someone decides it
// belongs, though a per-mapping "supported" verdict can still opt it in sooner.
var liveProgressUpstreams = map[string]struct{}{
	routing.ProviderLlamaCPP: {},
	routing.ProviderVLLM:     {},
}

// wantsLiveProgress applies the three-layer rule that decides whether the
// streaming request body for target may carry the live-progress parameters,
// ordered by the quality of their evidence -- observation beats prediction,
// prediction beats guessing, guessing beats silence:
//
//  1. target.LiveProgressSupport == "unsupported": always no. This is either an
//     OBSERVED upstream rejection (CompleteStream's retry, on a 400/422) or a
//     verdict copied from one -- no shape guess outranks it.
//  2. target.LiveProgressSupport == "supported": always yes, for the same
//     reason -- the verdict overrides the shape in either direction.
//  3. target.LiveProgressSupport == "" (never determined): falls back to
//     liveProgressUpstreams -- the shape clause. Send only when target's
//     application type genuinely implies a tolerant upstream: directly for an
//     ordinary application, or via target.LiveProgressSpecType (routing.
//     EffectiveRuntimeSpecType, filled by targetFrom at no extra store cost)
//     for a server_agent child.
//
// This is only half of CompleteStream's guard: it also consults its
// liveProgressMemo (kept as a separate check at that call site, not folded in
// here), so a target whose upstream already rejected the parameters in THIS
// process is not asked again until the memo's TTL expires -- an observation
// that outranks even a persisted "supported", exactly like layer 1 above.
func wantsLiveProgress(target routing.Target) bool {
	switch target.LiveProgressSupport {
	case "supported":
		return true
	case "unsupported":
		return false
	}
	shape := target.Provider
	if target.Provider == routing.ProviderServerAgent {
		shape = target.LiveProgressSpecType
	}
	_, ok := liveProgressUpstreams[shape]
	return ok
}

// liveProgressRejectionTTL bounds how long a recorded rejection is trusted. Past
// it the parameters are tried once more, so an upstream that was replaced or
// reconfigured behind the same mapping starts reporting live progress again
// without a gateway restart. 5 minutes is the house precedent for a
// volatile-negative TTL (defaultAgentLoadedTTL, internal/gateway/loaded_models.go).
const liveProgressRejectionTTL = 5 * time.Minute

// maxLiveProgressRejections bounds the memo's entry count. Keys are mapping ids,
// so the natural size is the number of serving mappings and this cap is never
// reached in practice; it exists so nothing -- a churn of recreated mappings, a
// hostile caller -- can make an advisory cache grow without limit.
const maxLiveProgressRejections = 1024

// liveProgressMemo remembers, per serving model mapping (routing.Target.RouteID),
// that an upstream REJECTED the live-progress request parameters. It turns a
// genuinely incompatible upstream's cost from one wasted round trip per REQUEST
// into one per mapping per TTL.
//
// TWO writers and TWO readers, on two endpoints, and the second pair reaches it
// from another package through LiveProgressRejectionMemo below -- read that
// doc comment for why the sharing is sound and what it is slightly over-broad
// about. In this package: CompleteStream's retry writes it and CompleteStream's
// own guard reads it. Outside it: internal/gateway's proxyNative writes it when a
// body carrying the `timings_per_token` IT injected earns a 400/422, and reads it
// as the sixth condition on that injection. Do not "simplify" this back to one
// writer -- a passthrough refusal that only the passthrough path remembered is
// exactly the split issue #81 was reopened to close.
//
// It records ONLY the negative verdict, and that asymmetry is the property that
// makes it safe. A stale NEGATIVE costs at most a missing advisory number -- the
// portal already renders that as its shared "never measured" em-dash -- and it
// heals by itself when the TTL expires. A stale POSITIVE would be the opposite:
// it would send the parameters to an upstream that answers 400, which is the dead
// stream this whole design exists to eliminate. So no positive verdict is ever
// recorded, there is no positive entry that could go stale, and an empty memo
// always means "send them".
//
// Every method is nil-receiver-safe and takes the lock itself, so it is safe to
// share across the concurrent requests one client serves.
type liveProgressMemo struct {
	mu sync.Mutex
	// rejectedAt maps a RouteID to when its rejection was observed.
	rejectedAt map[string]time.Time
	ttl        time.Duration
	max        int
	now        func() time.Time
}

// newLiveProgressMemo returns an empty memo -- i.e. "nothing has been observed
// rejecting the parameters, so send them wherever wantsLiveProgress's
// three-layer rule says to".
func newLiveProgressMemo() *liveProgressMemo {
	return &liveProgressMemo{
		rejectedAt: make(map[string]time.Time),
		ttl:        liveProgressRejectionTTL,
		max:        maxLiveProgressRejections,
		now:        time.Now,
	}
}

// rejects reports whether routeID has an un-expired rejection on record, dropping
// the entry when it has expired. An EMPTY routeID is never memoized and always
// reports false: there is no mapping id to key on (the probe/benchmark paths and
// tests build a Target by hand), and treating "" as one shared key would let one
// upstream's rejection suppress the number everywhere.
func (m *liveProgressMemo) rejects(routeID string) bool {
	if m == nil || routeID == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.rejectedAt[routeID]
	if !ok {
		return false
	}
	if m.now().Sub(at) > m.ttl {
		delete(m.rejectedAt, routeID)
		return false
	}
	return true
}

// recordRejection notes that routeID's upstream rejected the parameters. Bounded:
// expired entries are pruned on every write, and if the map is still at its cap
// the single oldest entry is evicted. Losing an entry only restores the pre-memo
// cost of one wasted round trip on that mapping, so an eviction can never be
// worse than not memoizing at all.
func (m *liveProgressMemo) recordRejection(routeID string) {
	if m == nil || routeID == "" {
		return
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, at := range m.rejectedAt {
		if now.Sub(at) > m.ttl {
			delete(m.rejectedAt, id)
		}
	}
	if _, known := m.rejectedAt[routeID]; !known && len(m.rejectedAt) >= m.max {
		var oldestID string
		var oldestAt time.Time
		for id, at := range m.rejectedAt {
			if oldestID == "" || at.Before(oldestAt) {
				oldestID, oldestAt = id, at
			}
		}
		delete(m.rejectedAt, oldestID)
	}
	m.rejectedAt[routeID] = now
}

// LiveProgressRejectionMemo is the optional capability of remembering, per
// serving model mapping, that an upstream REFUSED the live-progress request
// parameters -- the exported seam onto the memo above. Optional in the same
// sense as NativeProxyClient (proxy.go): a caller type-asserts for it and
// carries on unchanged when the resolved client does not have it, which is why
// it is a second interface rather than a method on NativeProxyClient -- that
// one is implemented by three production types and by a test fake per behaviour
// internal/gateway needs to drive, so widening it would make every one of those
// fakes carry a method it has no use for. The count is deliberately not written
// here: it grew by one in the very commit that first cited it.
//
// It exists because the memo has TWO writers and TWO readers across a package
// boundary, and before the seam only one pair could reach it. CompleteStream
// (translate, /v1/chat/completions) records its own retry-confirmed rejection
// and consults the memo at its guard; internal/gateway's native passthrough
// (/v1/responses) injects llama.cpp's `timings_per_token` under the operator's
// opt-in, has NO retry to absorb a wrong guess, and so both needs to stop
// asking after a refusal and to be told when another endpoint already learned
// one. Since Multiplexer.dispatchProxyNative and dispatchCompleteStream resolve
// the SAME m.clients[target.Provider] entry, and production registers one
// OpenAICompatibleClient under every OpenAI-compatible provider key, a record
// made through either path is seen by both.
//
// Both methods take a routing.Target rather than a bare route id so *Multiplexer
// can implement them: it has to resolve target.Provider to a client exactly as
// its other dispatchers do. The memo itself still keys on target.RouteID alone.
//
// The asymmetry that makes sharing one memo across two endpoints safe is the
// memo's own: it records NEGATIVES only, and a stale negative costs at most a
// missing advisory number that the portal already renders as its "never
// measured" em-dash, healing by itself when the TTL expires. The two paths do
// not send the same parameter SET -- translate sends `timings_per_token` AND
// `stream_options.continuous_usage_stats`, passthrough only the former -- so a
// translate-learned rejection is very slightly over-broad for the passthrough
// path (the refusal may have been earned by the other key). That direction is
// deliberate: it loses a display figure, never a request, and the TTL undoes it.
type LiveProgressRejectionMemo interface {
	// LiveProgressRejected reports whether target's upstream has an un-expired
	// rejection on record. False for an empty RouteID and for an empty memo,
	// which is what makes "nothing recorded" mean "send them".
	LiveProgressRejected(target routing.Target) bool
	// RecordLiveProgressRejection notes that target's upstream refused the
	// parameters. A no-op for an empty RouteID: a hand-built target (probe,
	// benchmark, test) has no mapping id, and treating "" as one shared key
	// would let one upstream's refusal suppress the figure everywhere.
	RecordLiveProgressRejection(target routing.Target)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"sync"
	"time"
)

// liveProgressUpstreams lists the application types EXPECTED to tolerate the two
// extra parameters this gateway adds to the STREAMING body it builds itself:
// llama.cpp's `timings_per_token` and `stream_options.continuous_usage_stats`.
// Both exist to get an EXACT mid-stream output-token count; without one, the
// portal shows "not measured" rather than a guess.
//
// This list is a PERFORMANCE HINT, not a correctness gate. Correctness comes from
// CompleteStream's retry: an upstream that rejects the parameters is re-asked
// without them before anything has been written to the client, so a rejection
// costs one wasted round trip and one missing advisory number instead of the
// request. The list only keeps that round trip off the types known to reject.
//
// It cannot be a correctness gate, because an application TYPE does not imply the
// upstream's request schema:
//   - `server_agent` is not an inference server at all. What actually serves is
//     whatever `RuntimeSpec.Type` says -- `"" | vllm | llama_cpp | tgi | ollama |
//     custom` -- where `custom` means nobody knows and `""` (auto-detect) is the
//     value on every pre-feature row. The agent's router forwards the request body
//     byte-for-byte, so the gateway is really talking to an unknown server.
//   - `llama_swap` is a proxy. It resolves each model either to a free-text `cmd`
//     (any OpenAI-compatible server) or to a `peer` at an arbitrary base URL with
//     an injected `Authorization: Bearer` -- llama-swap's own configuration example
//     uses OpenRouter. So a `llama_swap` model can terminate at api.openai.com,
//     which is exactly the case `litellm` is excluded for.
//
// What is known about the listed types when they DO serve directly:
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
// The default is therefore OFF: a provider value that is not listed gets neither
// parameter, so a type added later never opts in silently -- it just does not get
// the (advisory) number until someone decides the round trip is worth it.
var liveProgressUpstreams = map[string]struct{}{
	routing.ProviderLlamaCPP:    {},
	routing.ProviderLlamaSwap:   {},
	routing.ProviderVLLM:        {},
	routing.ProviderServerAgent: {},
}

// wantsLiveProgress reports whether the streaming request body for target may
// carry the live-progress parameters at all, i.e. whether its application type is
// on the allow-list above. It is only half the decision: CompleteStream also
// consults its liveProgressMemo, so a target whose upstream already rejected the
// parameters is not asked again until the memo's TTL expires.
func wantsLiveProgress(target routing.Target) bool {
	_, ok := liveProgressUpstreams[target.Provider]
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
// that an upstream REJECTED the two live-progress parameters. Consulted before the
// parameters are added and written only from CompleteStream's retry path, it turns
// a genuinely incompatible upstream's cost from one wasted round trip per REQUEST
// into one per mapping per TTL.
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

// newLiveProgressMemo returns an empty memo -- i.e. "send the parameters to
// everything the allow-list permits".
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

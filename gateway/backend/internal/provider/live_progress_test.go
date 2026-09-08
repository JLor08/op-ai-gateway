// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
	"time"
)

// TestWantsLiveProgressAllowList pins the shape clause of wantsLiveProgress's
// three-layer rule — layer 3, consulted only when no verdict has been recorded
// (LiveProgressSupport == "") — over EVERY provider constant, so a value added
// later fails loudly rather than silently opting in. A server_agent target's
// OWN Provider is always the literal "server_agent" and never compared against
// the shape clause directly (see TestWantsLiveProgressThreeLayerRule for its
// LiveProgressSpecType clause instead), so it is exercised here with no spec
// type resolved — the "no shape evidence at all" case, which must stay false.
func TestWantsLiveProgressAllowList(t *testing.T) {
	cases := map[string]bool{
		routing.ProviderLlamaCPP: true,
		routing.ProviderVLLM:     true,
		// llama_swap is a proxy: its TYPE says nothing about what actually
		// answers, so it is excluded even though it can still opt in via an
		// observed "supported" verdict (layer 2).
		routing.ProviderLlamaSwap: false,
		// LiteLLM forwards unknown body keys to OpenAI/Azure, which answer 400
		// and fail the WHOLE request. This entry is the point of the test.
		routing.ProviderLiteLLM: false,
		routing.ProviderOllama:  false,
		routing.ProviderMock:    false,
		// server_agent with no resolved spec type: no shape evidence at all.
		routing.ProviderServerAgent: false,
		"":                          false,
		"something_new":             false,
	}
	for provider, want := range cases {
		if got := wantsLiveProgress(routing.Target{Provider: provider}); got != want {
			t.Fatalf("wantsLiveProgress(%q) = %v, want %v", provider, got, want)
		}
	}
}

// TestWantsLiveProgressThreeLayerRule is the table over the full three-layer
// rule: observation (the persisted verdict) beats prediction (the shape),
// prediction beats guessing (nothing at all -- unreachable here since an
// undetermined verdict + an unlisted shape is exactly "guessing loses", i.e.
// false), and the in-process rejection memo -- consulted separately at
// CompleteStream's call site, never folded into wantsLiveProgress itself --
// outranks even an observed "supported" verdict.
func TestWantsLiveProgressThreeLayerRule(t *testing.T) {
	type tc struct {
		name        string
		routeID     string
		verdict     string
		provider    string
		specType    string // only meaningful when provider == server_agent
		memoRejects bool
		want        bool
	}
	cases := []tc{
		{
			name:    "verdict supported overrides a litellm shape that would otherwise say no",
			verdict: "supported", provider: routing.ProviderLiteLLM,
			want: true,
		},
		{
			name:    "verdict unsupported overrides a llama_cpp shape that would otherwise say yes",
			verdict: "unsupported", provider: routing.ProviderLlamaCPP,
			want: false,
		},
		{name: "undetermined + llama_cpp shape implies tolerant", verdict: "", provider: routing.ProviderLlamaCPP, want: true},
		{name: "undetermined + vllm shape implies tolerant", verdict: "", provider: routing.ProviderVLLM, want: true},
		{name: "undetermined + llama_swap: the type says nothing about the child", verdict: "", provider: routing.ProviderLlamaSwap, want: false},
		{name: "undetermined + litellm forwards unknown keys downstream", verdict: "", provider: routing.ProviderLiteLLM, want: false},
		{name: "undetermined + ollama: not on the shape allow-list", verdict: "", provider: routing.ProviderOllama, want: false},
		{name: "undetermined + mock: not on the shape allow-list", verdict: "", provider: routing.ProviderMock, want: false},
		{
			name:    "undetermined + server_agent whose effective spec type is llama_cpp",
			verdict: "", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeLlamaCpp),
			want: true,
		},
		{
			name:    "undetermined + server_agent whose effective spec type is vllm",
			verdict: "", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeVLLM),
			want: true,
		},
		{
			name:    "undetermined + server_agent whose effective spec type is tgi",
			verdict: "", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeTGI),
			want: false,
		},
		{
			name:    "undetermined + server_agent whose effective spec type is ollama",
			verdict: "", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeOllama),
			want: false,
		},
		{
			name:    "undetermined + server_agent whose effective spec type is custom",
			verdict: "", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeCustom),
			want: false,
		},
		{
			// The load-bearing row: a server_agent target whose spec has
			// Type: "" (auto-detect -- the value on every pre-runtime-manager
			// row) and a llama-server binary must still decide "send". The
			// spec type is resolved here through the SAME production helper
			// (routing.EffectiveRuntimeSpecType) targetFrom calls, and it is
			// paired with a routing-package test (resolver_endpoint_mode_test.go's
			// TestTargetLiveProgressSpecTypeUsesEffectiveTypeNotRawType) that
			// pins targetFrom itself calls it instead of reading spec.Type raw.
			// If EffectiveRuntimeSpecType is swapped for the raw Type here (Type
			// is "" so the raw value would also be ""), this row fails.
			name:     "undetermined + server_agent, spec Type empty but Binary llama-server",
			verdict:  "",
			provider: routing.ProviderServerAgent,
			specType: string(routing.EffectiveRuntimeSpecType(routing.RuntimeSpec{Type: "", Binary: "llama-server"})),
			want:     true,
		},
		{
			name:    "a supported verdict overrides even a non-tolerant server_agent effective type",
			verdict: "supported", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeTGI),
			want: true,
		},
		{
			name:    "an unsupported verdict overrides even a tolerant server_agent effective type",
			verdict: "unsupported", provider: routing.ProviderServerAgent, specType: string(routing.RuntimeSpecTypeLlamaCpp),
			want: false,
		},
		{
			// Layered on top of everything above: an in-process rejection memo
			// for this RouteID beats even an OBSERVED "supported" verdict --
			// proving the ordering (observation over the memo's own more-recent
			// observation), not merely that the memo can suppress a guess.
			// Without the memo, this exact target decides true (see the
			// "verdict supported overrides..." row above using the same
			// verdict+provider combination) -- so this row is what actually
			// exercises the override, not a shape-driven false landing by luck.
			name:        "memo rejection for the RouteID outranks a supported verdict",
			routeID:     "map_supported_but_memoized",
			verdict:     "supported",
			provider:    routing.ProviderLiteLLM,
			memoRejects: true,
			want:        false,
		},
		{
			name:        "memo rejection for the RouteID outranks an undetermined verdict's tolerant shape",
			routeID:     "map_shape_but_memoized",
			verdict:     "",
			provider:    routing.ProviderLlamaCPP,
			memoRejects: true,
			want:        false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := routing.Target{
				RouteID:              c.routeID,
				Provider:             c.provider,
				LiveProgressSupport:  c.verdict,
				LiveProgressSpecType: c.specType,
			}
			decision := wantsLiveProgress(target)
			if c.memoRejects {
				memo := newLiveProgressMemo()
				memo.recordRejection(c.routeID)
				decision = decision && !memo.rejects(target.RouteID)
			}
			if decision != c.want {
				t.Fatalf("decision for %+v = %v, want %v", target, decision, c.want)
			}
		})
	}
}

// TestLiveProgressMemoRecordsOnlyTheNegativeAndExpires pins the memo's three
// semantics at once: an empty memo says "send them", a recorded rejection
// suppresses them for that mapping only, and the record expires on the TTL (a
// stale negative heals by itself, which is why only the negative direction is
// ever recorded).
func TestLiveProgressMemoRecordsOnlyTheNegativeAndExpires(t *testing.T) {
	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	m := newLiveProgressMemo()
	m.now = func() time.Time { return now }

	if m.rejects("map_a") {
		t.Fatal("empty memo rejects map_a, want false (an empty memo means send them)")
	}
	m.recordRejection("map_a")
	if !m.rejects("map_a") {
		t.Fatal("memo does not reject map_a after recordRejection")
	}
	if m.rejects("map_b") {
		t.Fatal("memo rejects map_b, want false (a rejection is per mapping)")
	}

	now = now.Add(liveProgressRejectionTTL + time.Second)
	if m.rejects("map_a") {
		t.Fatal("memo still rejects map_a past the TTL, want false")
	}
	// The expired entry is dropped on read, not merely ignored.
	if _, ok := m.rejectedAt["map_a"]; ok {
		t.Fatal("expired entry for map_a is still in the map")
	}
}

// TestLiveProgressMemoIgnoresAnEmptyRouteID proves the "" key is never memoized:
// the probe/benchmark paths build a Target by hand with no mapping id, and
// treating "" as one shared key would let a single upstream's rejection suppress
// the number for every hand-built target in the process.
func TestLiveProgressMemoIgnoresAnEmptyRouteID(t *testing.T) {
	m := newLiveProgressMemo()
	m.recordRejection("")
	if m.rejects("") {
		t.Fatal("memo rejects the empty RouteID, want false")
	}
	if len(m.rejectedAt) != 0 {
		t.Fatalf("memo holds %d entries after recording the empty RouteID, want 0", len(m.rejectedAt))
	}
}

// TestLiveProgressMemoIsBounded proves the memo cannot grow without limit: past
// its cap the oldest entry is evicted rather than the map extended. Losing an
// entry only restores the pre-memo cost of one wasted round trip, so eviction is
// always the safe direction.
func TestLiveProgressMemoIsBounded(t *testing.T) {
	base := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	now := base
	m := newLiveProgressMemo()
	m.now = func() time.Time { return now }
	m.max = 4

	for i := range 6 {
		now = base.Add(time.Duration(i) * time.Second)
		m.recordRejection("map_" + strconv.Itoa(i))
	}
	if len(m.rejectedAt) > m.max {
		t.Fatalf("memo holds %d entries, want at most %d", len(m.rejectedAt), m.max)
	}
	// The two most recent writes must have survived; the oldest must not.
	if !m.rejects("map_5") || !m.rejects("map_4") {
		t.Fatalf("recent entries evicted: %v", m.rejectedAt)
	}
	if m.rejects("map_0") {
		t.Fatal("oldest entry map_0 survived eviction")
	}
}

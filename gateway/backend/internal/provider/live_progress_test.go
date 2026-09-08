// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
	"time"
)

// TestWantsLiveProgressAllowList pins the one shared allow-list gating the two
// live-progress request parameters. It is a table over EVERY provider constant so
// a value added later fails loudly rather than silently opting in.
func TestWantsLiveProgressAllowList(t *testing.T) {
	cases := map[string]bool{
		routing.ProviderLlamaCPP:    true,
		routing.ProviderLlamaSwap:   true,
		routing.ProviderVLLM:        true,
		routing.ProviderServerAgent: true,
		// LiteLLM forwards unknown body keys to OpenAI/Azure, which answer 400
		// and fail the WHOLE request. This entry is the point of the test.
		routing.ProviderLiteLLM: false,
		routing.ProviderOllama:  false,
		routing.ProviderMock:    false,
		"":                      false,
		"something_new":         false,
	}
	for provider, want := range cases {
		if got := wantsLiveProgress(routing.Target{Provider: provider}); got != want {
			t.Fatalf("wantsLiveProgress(%q) = %v, want %v", provider, got, want)
		}
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

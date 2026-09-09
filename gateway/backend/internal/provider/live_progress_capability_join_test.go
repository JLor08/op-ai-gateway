// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestWantsLiveProgressReadsTheJoinedVerdict pins that wantsLiveProgress's
// three-layer rule is unaffected by WHERE Target.LiveProgressSupport's value
// now comes from on the request path: MappingCandidate.LiveProgressSupport,
// filled from the joined "live_progress" capability row via
// routing.LiveProgressSupportFromVerdict, rather than the
// pre-migration-78 live_progress_support column migration 79 dropped
// (TestWantsLiveProgressThreeLayerRule already pins the rule itself against
// the "" / "supported" / "unsupported" vocabulary directly).
//
// It proves the three cases the boundary conversion has to get right decide
// EXACTLY as that pre-existing vocabulary already did: a "yes" row behaves
// like a stored "supported" verdict (always true, even overriding a shape
// that would say no), a "no" row behaves like a stored "unsupported" verdict
// (always false, even overriding a shape that would say yes -- the
// three-state-to-bool-ish case a careless conversion gets wrong), and no row
// at all behaves like an empty verdict (falls through to the shape clause,
// which is exercised both ways).
func TestWantsLiveProgressReadsTheJoinedVerdict(t *testing.T) {
	cases := []struct {
		name     string
		verdict  string // routing.CapabilityYes | routing.CapabilityNo | "" (absent row)
		provider string
		want     bool
	}{
		{
			name:     "yes row always wins, like a stored supported verdict -- even over an intolerant shape",
			verdict:  routing.CapabilityYes,
			provider: routing.ProviderOllama, // shape clause alone would say false
			want:     true,
		},
		{
			name:     "no row always loses, like a stored unsupported verdict -- even over a tolerant shape",
			verdict:  routing.CapabilityNo,
			provider: routing.ProviderLlamaCPP, // shape clause alone would say true
			want:     false,
		},
		{
			name:     "absent row falls through to a tolerant shape clause, like an empty verdict",
			verdict:  "",
			provider: routing.ProviderLlamaCPP,
			want:     true,
		},
		{
			name:     "absent row falls through to an intolerant shape clause, like an empty verdict",
			verdict:  "",
			provider: routing.ProviderOllama,
			want:     false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			support := routing.LiveProgressSupportFromVerdict(c.verdict)
			target := routing.Target{Provider: c.provider, LiveProgressSupport: support}
			if got := wantsLiveProgress(target); got != c.want {
				t.Fatalf("wantsLiveProgress(verdict=%q -> support=%q, provider=%q) = %v, want %v", c.verdict, support, c.provider, got, c.want)
			}
		})
	}
}

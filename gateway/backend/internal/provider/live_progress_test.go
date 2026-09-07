// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"op-ai-gateway/internal/routing"
	"testing"
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

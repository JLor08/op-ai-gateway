// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestEffectiveDecisionsRoute(t *testing.T) {
	cases := []struct {
		name         string
		kind         string
		mode         EndpointMode
		decision, oa bool
		route        EndpointMode
		ok           bool
	}{
		{"disabled", "vllm", EndpointModeDisabled, false, false, EndpointModeDisabled, true},
		{"disabled with verdicts", "vllm", EndpointModeDisabled, true, true, EndpointModeDisabled, true},
		{"auto prefers passthrough", "vllm", EndpointModeAuto, true, true, EndpointModePassthrough, true},
		{"auto passthrough only", "vllm", EndpointModeAuto, false, true, EndpointModePassthrough, true},
		{"auto translate", "vllm", EndpointModeAuto, true, false, EndpointModeTranslate, true},
		{"auto neither", "vllm", EndpointModeAuto, false, false, "", false},
		{"blank behaves as auto (translate)", "vllm", "", true, false, EndpointModeTranslate, true},
		{"blank behaves as auto (passthrough)", "vllm", "", true, true, EndpointModePassthrough, true},
		{"blank neither", "vllm", "", false, false, "", false},
		{"translate with decision", "vllm", EndpointModeTranslate, true, false, EndpointModeTranslate, true},
		{"translate without decision", "vllm", EndpointModeTranslate, false, true, EndpointModeTranslate, false},
		{"passthrough with verdict", "vllm", EndpointModePassthrough, false, true, EndpointModePassthrough, true},
		{"passthrough without verdict", "vllm", EndpointModePassthrough, true, false, EndpointModePassthrough, false},
		{"llama_cpp auto ignores decision_openai", ProviderLlamaCPP, EndpointModeAuto, true, true, EndpointModeTranslate, true},
		{"llama_cpp auto only decision_openai", ProviderLlamaCPP, EndpointModeAuto, false, true, "", false},
		{"llama_cpp stale forced passthrough", string(RuntimeSpecTypeLlamaCpp), EndpointModePassthrough, true, true, EndpointModePassthrough, false},
		{"llama_cpp translate", ProviderLlamaCPP, EndpointModeTranslate, true, true, EndpointModeTranslate, true},
		{"vllm_sr auto ignores decision_openai", ProviderVllmSR, EndpointModeAuto, true, true, EndpointModeTranslate, true},
		{"vllm_sr blank only decision_openai", ProviderVllmSR, "", false, true, "", false},
		{"vllm_sr stale forced passthrough", string(RuntimeSpecTypeVllmSR), EndpointModePassthrough, true, true, EndpointModePassthrough, false},
		{"vllm_sr disabled", ProviderVllmSR, EndpointModeDisabled, true, true, EndpointModeDisabled, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, ok := EffectiveDecisionsRoute(tc.kind, tc.mode, tc.decision, tc.oa)
			if route != tc.route || ok != tc.ok {
				t.Fatalf("got (%q,%v), want (%q,%v)", route, ok, tc.route, tc.ok)
			}
		})
	}
}

func TestDecisionsPassthroughCapableKind(t *testing.T) {
	for kind, want := range map[string]bool{
		ProviderLlamaCPP: false, ProviderVllmSR: false,
		"vllm": true, "custom": true, "server_agent": true, "": true,
	} {
		if got := DecisionsPassthroughCapableKind(kind); got != want {
			t.Errorf("DecisionsPassthroughCapableKind(%q) = %v, want %v", kind, got, want)
		}
	}
}

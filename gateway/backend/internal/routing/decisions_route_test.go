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

func TestEffectiveSystemOneRoute(t *testing.T) {
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
		{"passthrough with decision", "vllm", EndpointModePassthrough, true, false, EndpointModePassthrough, true},
		{"passthrough without decision", "vllm", EndpointModePassthrough, false, true, EndpointModePassthrough, false},
		{"translate with decision_openai", "vllm", EndpointModeTranslate, false, true, EndpointModeTranslate, true},
		{"translate without decision_openai", "vllm", EndpointModeTranslate, true, false, EndpointModeTranslate, false},
		{"auto prefers passthrough", "vllm", EndpointModeAuto, true, true, EndpointModePassthrough, true},
		{"auto decision only", "vllm", EndpointModeAuto, true, false, EndpointModePassthrough, true},
		{"auto translates decision_openai only", "vllm", EndpointModeAuto, false, true, EndpointModeTranslate, true},
		{"auto neither", "vllm", EndpointModeAuto, false, false, "", false},
		{"blank behaves as auto (passthrough)", "vllm", "", true, true, EndpointModePassthrough, true},
		{"blank behaves as auto (translate)", "vllm", "", false, true, EndpointModeTranslate, true},
		{"blank neither", "vllm", "", false, false, "", false},
		{"llama_cpp auto ignores decision_openai", ProviderLlamaCPP, EndpointModeAuto, false, true, "", false},
		{"llama_cpp auto decision", ProviderLlamaCPP, EndpointModeAuto, true, true, EndpointModePassthrough, true},
		{"llama_cpp stale translate", string(RuntimeSpecTypeLlamaCpp), EndpointModeTranslate, true, true, EndpointModeTranslate, false},
		{"llama_cpp passthrough", ProviderLlamaCPP, EndpointModePassthrough, true, false, EndpointModePassthrough, true},
		{"llama_cpp disabled", ProviderLlamaCPP, EndpointModeDisabled, false, false, EndpointModeDisabled, true},
		{"vllm_sr auto behaves like passthrough", ProviderVllmSR, EndpointModeAuto, true, true, EndpointModePassthrough, true},
		{"vllm_sr auto only decision_openai", string(RuntimeSpecTypeVllmSR), "", false, true, "", false},
		{"vllm_sr stale translate", ProviderVllmSR, EndpointModeTranslate, true, true, EndpointModeTranslate, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			route, ok := EffectiveSystemOneRoute(tc.kind, tc.mode, tc.decision, tc.oa)
			if route != tc.route || ok != tc.ok {
				t.Fatalf("got (%q,%v), want (%q,%v)", route, ok, tc.route, tc.ok)
			}
		})
	}
}

func TestMappingServesDecisionEndpointSystemOneRoute(t *testing.T) {
	flavors := []string{APIFlavorDecisions}
	for _, tc := range []struct {
		kind string
		mode EndpointMode
		want bool
	}{
		{"vllm", EndpointModeAuto, true},
		{"vllm", "", true},
		{"vllm", EndpointModePassthrough, true},
		{"vllm", EndpointModeTranslate, true},
		{"vllm", EndpointModeDisabled, false},
		{ProviderLlamaCPP, EndpointModeAuto, true},
		{ProviderLlamaCPP, EndpointModeTranslate, false},
		{ProviderVllmSR, EndpointModeTranslate, false},
		{ProviderVllmSR, EndpointModeAuto, true},
	} {
		app := Application{Type: tc.kind, APIFlavors: flavors, SystemOneMode: tc.mode}
		if got := MappingServesDecisionEndpoint(app, EffectiveFields(app, RuntimeSpec{}, false), APIFlavorDecisions); got != tc.want {
			t.Errorf("%s/%q: served = %v, want %v", tc.kind, tc.mode, got, tc.want)
		}
	}
}

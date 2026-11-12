// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"slices"
	"testing"
)

func TestCapabilityAnyDecisionSatisfied(t *testing.T) {
	yes := func(name string) CapabilityRow { return CapabilityRow{Capability: name, Verdict: CapabilityYes} }
	no := func(name string) CapabilityRow { return CapabilityRow{Capability: name, Verdict: CapabilityNo} }
	cases := []struct {
		name     string
		rows     []CapabilityRow
		required []string
		want     bool
	}{
		{"system one decision", []CapabilityRow{yes(CapabilityDecision)}, []string{CapabilityAnyDecision}, true},
		{"openai decisions", []CapabilityRow{yes(CapabilityDecisionOpenAI)}, []string{CapabilityAnyDecision}, true},
		{"both no", []CapabilityRow{no(CapabilityDecision), no(CapabilityDecisionOpenAI)}, []string{CapabilityAnyDecision}, false},
		{"no rows", nil, []string{CapabilityAnyDecision}, false},
		{"a stored any_decision row is inert", []CapabilityRow{yes(CapabilityAnyDecision)}, []string{CapabilityAnyDecision}, false},
		{"another requirement still gates", []CapabilityRow{yes(CapabilityDecision)}, []string{CapabilityAnyDecision, CapabilityImage}, false},
		{"all requirements met", []CapabilityRow{yes(CapabilityDecision), yes(CapabilityImage)}, []string{CapabilityAnyDecision, CapabilityImage}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capabilityRowsSatisfy(tc.rows, tc.required); got != tc.want {
				t.Fatalf("capabilityRowsSatisfy(%v, %v) = %v, want %v", tc.rows, tc.required, got, tc.want)
			}
		})
	}
}

func TestDecisionsProbeSourceRanksLikeSystemOneProbe(t *testing.T) {
	if got, want := capabilitySourceRank(CapabilitySourceDecisionsProbe), capabilitySourceRank(CapabilitySourceSystemOneProbe); got != want || got != 1 {
		t.Fatalf("rank(decisions_probe) = %d, rank(systemone_probe) = %d, want both 1", got, want)
	}
}

func TestNormalizeAPIFlavorOpenAIDecisions(t *testing.T) {
	for _, in := range []string{"openai_decisions", " OpenAI_Decisions "} {
		if got := NormalizeAPIFlavor(in); got != APIFlavorDecisions {
			t.Errorf("NormalizeAPIFlavor(%q) = %q, want %q", in, got, APIFlavorDecisions)
		}
	}
	if got := NormalizeAPIFlavor("openai_chat_completions"); got != APIFlavorOpenAI {
		t.Errorf("chat flavor regressed: %q", got)
	}
}

func TestApplicationServesEndpointDecisionModes(t *testing.T) {
	app := func(typ string, decisions, systemOne EndpointMode) Application {
		return Application{Type: typ, APIFlavors: []string{APIFlavorDecisions}, DecisionsMode: decisions, SystemOneMode: systemOne}
	}
	cases := []struct {
		name                   string
		app                    Application
		wantSystemOne, wantOAI bool
	}{
		{"both served", app(ProviderLlamaCPP, EndpointModeAuto, EndpointModePassthrough), true, true},
		{"system one disabled", app(ProviderLlamaCPP, EndpointModeAuto, EndpointModeDisabled), false, true},
		{"decisions disabled", app(ProviderLlamaCPP, EndpointModeDisabled, EndpointModePassthrough), true, false},
		{"unset modes serve", app(ProviderLlamaCPP, "", ""), true, true},
		{"server_agent: dispatch decides", app(ProviderServerAgent, EndpointModeDisabled, EndpointModeDisabled), true, true},
		{"no decisions flavor", Application{Type: ProviderLlamaCPP, APIFlavors: []string{APIFlavorOpenAI}}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := applicationServesEndpoint(tc.app, APIFlavorDecisions); got != tc.wantSystemOne {
				t.Errorf("decisions (System One) = %v, want %v", got, tc.wantSystemOne)
			}
			if got := applicationServesEndpoint(tc.app, APIFlavorOpenAIDecisions); got != tc.wantOAI {
				t.Errorf("openai_decisions = %v, want %v", got, tc.wantOAI)
			}
		})
	}
}

func TestEffectiveFieldsDecisionModes(t *testing.T) {
	app := Application{Type: ProviderServerAgent, DecisionsMode: EndpointModeAuto, SystemOneMode: EndpointModePassthrough}
	spec := RuntimeSpec{DecisionsMode: EndpointModeTranslate, SystemOneMode: EndpointModeDisabled}
	got := EffectiveFields(app, spec, true)
	if got.DecisionsMode != EndpointModeTranslate || got.SystemOneMode != EndpointModeDisabled {
		t.Errorf("with a spec: got %q/%q, want translate/disabled", got.DecisionsMode, got.SystemOneMode)
	}
	got = EffectiveFields(app, RuntimeSpec{}, false)
	if got.DecisionsMode != EndpointModeAuto || got.SystemOneMode != EndpointModePassthrough {
		t.Errorf("without a spec: got %q/%q, want auto/passthrough", got.DecisionsMode, got.SystemOneMode)
	}
}

// The resolver's candidacy filter applies the decision endpoint modes too: an
// ordinary application whose mode for the request's endpoint is disabled is no
// candidate, a server_agent application is (its spec decides at dispatch).
func TestFilterServesEndpointAppliesDecisionModes(t *testing.T) {
	cand := func(id, typ string, decisions, systemOne EndpointMode) MappingCandidate {
		return MappingCandidate{Application: Application{ID: id, Type: typ, APIFlavors: []string{APIFlavorDecisions}, DecisionsMode: decisions, SystemOneMode: systemOne}}
	}
	cands := []MappingCandidate{
		cand("both", ProviderVLLM, EndpointModeAuto, EndpointModePassthrough),
		cand("no-decisions", ProviderVLLM, EndpointModeDisabled, EndpointModePassthrough),
		cand("no-systemone", ProviderVLLM, EndpointModeAuto, EndpointModeDisabled),
		cand("agent", ProviderServerAgent, EndpointModeDisabled, EndpointModeDisabled),
	}
	ids := func(cs []MappingCandidate) []string {
		out := []string{}
		for _, c := range cs {
			out = append(out, c.Application.ID)
		}
		return out
	}
	if got := ids(filterServesEndpoint(append([]MappingCandidate(nil), cands...), APIFlavorOpenAIDecisions)); !slices.Equal(got, []string{"both", "no-systemone", "agent"}) {
		t.Errorf("openai_decisions candidates = %v", got)
	}
	if got := ids(filterServesEndpoint(append([]MappingCandidate(nil), cands...), APIFlavorDecisions)); !slices.Equal(got, []string{"both", "no-decisions", "agent"}) {
		t.Errorf("decisions (System One) candidates = %v", got)
	}
}

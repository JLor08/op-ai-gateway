// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"testing"
	"time"
)

// TestDecisionRouteFilterLeavesServerAgentCandidates: a server_agent
// candidate's effective mode lives on its runtime spec, unknown before
// resolve, so it keeps the plain [any_decision] check and the post-resolve
// route decides; a stored passthrough with only decision_openai stays a
// candidate.
func TestDecisionRouteFilterLeavesServerAgentCandidates(t *testing.T) {
	oa := []CapabilityRow{{Capability: CapabilityDecisionOpenAI, Verdict: CapabilityYes}}
	cand := func(id, kind string) MappingCandidate {
		return MappingCandidate{
			Server:      AIServer{ID: "srv_" + id},
			Application: Application{ID: "app_" + id, Type: kind, SystemOneMode: EndpointModePassthrough, DecisionsMode: EndpointModeTranslate},
			Mapping:     ModelMapping{ID: id},
		}
	}
	store := fixedCapsStore{caps: map[string][]CapabilityRow{"agent": oa, "vllm": oa}}
	r := NewResolver(store, time.Now, nil)
	for _, flavor := range decisionFlavors {
		got, err := r.filterCapable(context.Background(), []MappingCandidate{cand("agent", ProviderServerAgent), cand("vllm", ProviderVLLM)}, []string{CapabilityAnyDecision}, flavor)
		if err != nil {
			t.Fatalf("filterCapable(%s): %v", flavor, err)
		}
		if len(got) != 1 || got[0].Mapping.ID != "agent" {
			t.Fatalf("filterCapable(%s) = %+v, want only the server_agent candidate", flavor, got)
		}
	}
}

// TestAffinityDecisionGateIsRouteAware: the affinity gate judges a pinned
// ordinary mapping with the same route-aware rule as a fresh candidate.
func TestAffinityDecisionGateIsRouteAware(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := seedDecisionRouteStore(t, now,
		decisionRouteApp{id: "oa", kind: ProviderVLLM, model: "dec-model", latency: 100, decisions: EndpointModeAuto, systemOne: EndpointModePassthrough, verdicts: []string{CapabilityDecisionOpenAI}},
	)
	r := NewResolver(store, func() time.Time { return now }, nil)
	app := Application{ID: "app_oa", Type: ProviderVLLM, DecisionsMode: EndpointModeAuto, SystemOneMode: EndpointModePassthrough}
	required := []string{CapabilityAnyDecision}
	if _, ok := r.affinityCapabilityVerdict(context.Background(), "map_oa", app, required, APIFlavorDecisions); ok {
		t.Error("System One: a stored passthrough without the decision verdict satisfied the affinity gate")
	}
	if _, ok := r.affinityCapabilityVerdict(context.Background(), "map_oa", app, required, APIFlavorOpenAIDecisions); !ok {
		t.Error("/v1/decisions: an auto mapping with decision_openai was refused by the affinity gate")
	}
}

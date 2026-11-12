// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
)

// TestDecisionRedirectHintIsRouteAware: the unknown-model redirect for the two
// decision endpoints names only a model the resolver would accept for THAT
// endpoint, i.e. one with a route under its effective mode (an ordinary
// application's own modes, a server_agent mapping's spec kind and modes), not
// merely a yes on either verdict.
func TestDecisionRedirectHintIsRouteAware(t *testing.T) {
	rs := routing.NewMemoryStore()
	dec := routing.CapabilityRow{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceSystemOneProbe, CheckedAt: offeringTime}
	oa := routing.CapabilityRow{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceDecisionsProbe, CheckedAt: offeringTime}
	flavors := []string{routing.APIFlavorDecisions}
	ctx := context.Background()
	setModes := func(gateway string, systemOne, decisions routing.EndpointMode) {
		t.Helper()
		app, err := rs.ApplicationByID(ctx, "app_"+gateway)
		if err != nil {
			t.Fatalf("ApplicationByID %s: %v", gateway, err)
		}
		app.SystemOneMode, app.DecisionsMode = systemOne, decisions
		if err := rs.UpdateApplication(ctx, app); err != nil {
			t.Fatalf("UpdateApplication %s: %v", gateway, err)
		}
	}
	ordinary := func(gateway string, systemOne, decisions routing.EndpointMode, rows ...routing.CapabilityRow) {
		t.Helper()
		offerSpeechModel(t, rs, routing.ProviderVLLM, flavors, nil, gateway, rows...)
		setModes(gateway, systemOne, decisions)
	}
	agent := func(gateway string, systemOne, decisions routing.EndpointMode, rows ...routing.CapabilityRow) {
		t.Helper()
		// The application's own modes are auto: only the spec's count.
		offerSpeechModel(t, rs, routing.ProviderServerAgent, flavors, nil, gateway, rows...)
		if err := rs.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "app_" + gateway + "_spec", MappingID: "map_" + gateway, Binary: "/opt/x/server", Type: string(routing.RuntimeSpecTypeVLLM), Args: "[]", Env: "{}", APIFlavors: flavors, SystemOneMode: systemOne, DecisionsMode: decisions}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", gateway, err)
		}
	}
	ordinary("auto-dec", "auto", "auto", dec)
	ordinary("auto-oa", "auto", "auto", oa)
	ordinary("s1-pass-oa", routing.EndpointModePassthrough, "auto", oa)
	ordinary("dec-pass-dec", "auto", routing.EndpointModePassthrough, dec)
	ordinary("s1-trans-dec", routing.EndpointModeTranslate, "auto", dec)
	ordinary("dec-trans-oa", "auto", routing.EndpointModeTranslate, oa)
	agent("agent-auto-oa", "auto", "auto", oa)
	agent("agent-s1-pass-oa", routing.EndpointModePassthrough, "auto", oa)
	agent("agent-dec-pass-dec", "auto", routing.EndpointModePassthrough, dec)

	svc := offerSvc(rs, nil)
	token := auth.Token{UserID: "usr_1"}
	any := []string{routing.CapabilityAnyDecision}
	cases := []struct {
		flavor string
		want   map[string]bool
	}{
		{routing.APIFlavorDecisions, map[string]bool{ // POST /v1/systemone
			"auto-dec": true, "auto-oa": true, "s1-pass-oa": false, "dec-pass-dec": true,
			"s1-trans-dec": false, "dec-trans-oa": true,
			"agent-auto-oa": true, "agent-s1-pass-oa": false, "agent-dec-pass-dec": true,
		}},
		{routing.APIFlavorOpenAIDecisions, map[string]bool{ // POST /v1/decisions
			"auto-dec": true, "auto-oa": true, "s1-pass-oa": true, "dec-pass-dec": false,
			"s1-trans-dec": true, "dec-trans-oa": false,
			"agent-auto-oa": true, "agent-s1-pass-oa": true, "agent-dec-pass-dec": false,
		}},
	}
	for _, c := range cases {
		off := svc.ModelOfferingFor(ctx, token, c.flavor, any)
		for name, want := range c.want {
			if _, got := off.Capable[name]; got != want {
				t.Errorf("%s: capable[%s] = %v, want %v", c.flavor, name, got, want)
			}
		}
	}
}

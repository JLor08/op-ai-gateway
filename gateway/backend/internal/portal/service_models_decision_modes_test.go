// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"slices"
	"testing"
)

// decisionModes is one (systemone_mode, decisions_mode) pair.
type decisionModes struct {
	systemOne, decisions routing.EndpointMode
}

// offerDecisionModel seeds one model on an application of its own whose
// application (and, with spec non-nil, runtime spec) carry the decisions
// flavor and the given modes. Both verdicts are yes, so only the modes decide.
func offerDecisionModel(t *testing.T, rs *routing.MemoryStore, appType string, app decisionModes, spec *decisionModes, gateway string) {
	t.Helper()
	ctx := context.Background()
	srvID, appID, mappingID := "srv_"+gateway, "app_"+gateway, "map_"+gateway
	flavors := []string{routing.APIFlavorDecisions}
	if appType == routing.ProviderServerAgent {
		flavors = []string{routing.APIFlavorOpenAI, routing.APIFlavorDecisions}
	}
	if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateAIServer %s: %v", srvID, err)
	}
	if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: appType, Port: 8000, Scheme: "http", APIFlavors: flavors, SystemOneMode: app.systemOne, DecisionsMode: app.decisions, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateApplication %s: %v", appID, err)
	}
	if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
		t.Fatalf("CreateMapping %s: %v", gateway, err)
	}
	rows := []routing.CapabilityRow{
		{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime},
		{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime},
	}
	if err := rs.UpsertMappingCapabilities(ctx, mappingID, rows); err != nil {
		t.Fatalf("UpsertMappingCapabilities %s: %v", mappingID, err)
	}
	if spec != nil {
		if err := rs.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: appID + "_spec", MappingID: mappingID, Binary: "/opt/vllm/vllm", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorDecisions}, SystemOneMode: spec.systemOne, DecisionsMode: spec.decisions}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", mappingID, err)
		}
	}
}

var (
	modesBothOn       = decisionModes{routing.EndpointModePassthrough, routing.EndpointModeAuto}
	modesSystemOneOff = decisionModes{routing.EndpointModeDisabled, routing.EndpointModeAuto}
	modesDecisionsOff = decisionModes{routing.EndpointModePassthrough, routing.EndpointModeDisabled}
	modesBothOff      = decisionModes{routing.EndpointModeDisabled, routing.EndpointModeDisabled}
)

// decisionModesStore seeds one model per mode pair, on an ordinary application
// and as a server_agent spec whose application says the opposite (so a rule
// reading the application instead of the spec fails), plus a group whose two
// members each serve one endpoint only.
func decisionModesStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	rs := routing.NewMemoryStore()
	offerDecisionModel(t, rs, routing.ProviderVLLM, modesSystemOneOff, nil, "plain-s1-off")
	offerDecisionModel(t, rs, routing.ProviderVLLM, modesDecisionsOff, nil, "plain-dec-off")
	offerDecisionModel(t, rs, routing.ProviderVLLM, modesBothOff, nil, "plain-both-off")
	offerDecisionModel(t, rs, routing.ProviderServerAgent, modesDecisionsOff, &modesSystemOneOff, "agent-s1-off")
	offerDecisionModel(t, rs, routing.ProviderServerAgent, modesSystemOneOff, &modesDecisionsOff, "agent-dec-off")
	offerDecisionModel(t, rs, routing.ProviderServerAgent, modesBothOn, &modesBothOff, "agent-both-off")
	offerGroup(t, rs, "grp_split", "split-group", "plain-s1-off", "plain-dec-off")
	return rs
}

// decisionModeExpectation is what each model of decisionModesStore must be.
var decisionModeExpectation = map[string]struct {
	listed, systemOne, decisions bool
}{
	"plain-s1-off":   {true, false, true},
	"plain-dec-off":  {true, true, false},
	"plain-both-off": {false, false, false},
	"agent-s1-off":   {true, false, true},
	"agent-dec-off":  {true, true, false},
	"agent-both-off": {false, false, false},
	"split-group":    {true, true, true},
}

// The listing's decisions flavor means "at least one decision endpoint is
// served", by the EFFECTIVE modes: a model with only its systemone endpoint
// disabled is still listed under decisions (POST /v1/decisions serves it), and
// an ordinary application and a server_agent spec answer alike. The Decision
// flag (what GET /v1/systemone/models lists) follows the systemone mode only.
func TestModelsListDecisionsByEitherEffectiveMode(t *testing.T) {
	svc := offerSvc(decisionModesStore(t), nil)
	byID := modelsByID(svc.Models(context.Background(), auth.Token{UserID: "usr_1"}))
	for name, want := range decisionModeExpectation {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from Models(): %#v", name, byID)
		}
		if listed := slices.Contains(got.Flavors, routing.APIFlavorDecisions); listed != want.listed {
			t.Errorf("%s: decisions in flavors = %v (%v), want %v", name, listed, got.Flavors, want.listed)
		}
		if name == "split-group" {
			continue // a group's Decision is the AND over its members
		}
		if got.Decision != want.systemOne {
			t.Errorf("%s: decision = %v, want %v (POST /v1/systemone serves it: %v)", name, got.Decision, want.systemOne, want.systemOne)
		}
	}
	if byID["split-group"].Decision {
		t.Error("split-group: decision = true, want false -- one member's systemone endpoint is disabled")
	}
}

// ModelOffering.Callable judges each decision endpoint by its own effective
// mode: the openai_decisions flavor (POST /v1/decisions) by decisions_mode,
// the decisions flavor (POST /v1/systemone) by systemone_mode. A disabled
// endpoint leaves the name Existing (the "exists but you cannot call it"
// case), never Callable.
func TestModelOfferingJudgesEachDecisionEndpointByItsOwnMode(t *testing.T) {
	ctx := context.Background()
	svc := offerSvc(decisionModesStore(t), nil)
	token := auth.Token{UserID: "usr_1"}
	decisions := svc.ModelOfferingFor(ctx, token, routing.APIFlavorOpenAIDecisions, nil)
	systemOne := svc.ModelOfferingFor(ctx, token, routing.APIFlavorDecisions, nil)
	for name, want := range decisionModeExpectation {
		if _, got := decisions.Callable[name]; got != want.decisions {
			t.Errorf("%s: callable for /v1/decisions = %v, want %v", name, got, want.decisions)
		}
		if _, got := systemOne.Callable[name]; got != want.systemOne {
			t.Errorf("%s: callable for /v1/systemone = %v, want %v", name, got, want.systemOne)
		}
		for label, off := range map[string]ModelOffering{"/v1/decisions": decisions, "/v1/systemone": systemOne} {
			if _, exists := off.Existing[name]; !exists {
				t.Errorf("%s: missing from Existing for %s -- a disabled endpoint does not unmake the name", name, label)
			}
		}
	}
}

// A model-valued token setting accepts a name that serves either decision
// endpoint: callableModelNames unions both, so a model whose only served
// decision endpoint is /v1/decisions stays a valid fallback.
func TestCallableModelNamesCoversBothDecisionEndpoints(t *testing.T) {
	svc := offerSvc(decisionModesStore(t), nil)
	callable := svc.callableModelNames(context.Background(), auth.Token{UserID: "usr_1"})
	for name, want := range decisionModeExpectation {
		if _, got := callable[name]; got != want.listed {
			t.Errorf("%s: callable model name = %v, want %v", name, got, want.listed)
		}
	}
}

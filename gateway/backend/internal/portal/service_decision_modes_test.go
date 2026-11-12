// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

func decisionModesFixture(t *testing.T) (*Service, *routing.MemoryStore, string) {
	t.Helper()
	svc, routes := newServerTestService(t, time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC))
	server := createTestServer(t, svc, "S", "s.example.test")
	return svc, routes, server.ID
}

func TestCreateApplicationDecisionModesDefaultAndValidate(t *testing.T) {
	ctx := context.Background()
	svc, _, serverID := decisionModesFixture(t)

	dto, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: routing.ProviderVLLM, Port: 8300, Scheme: "http"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if dto.DecisionsMode != "auto" || dto.SystemOneMode != "passthrough" {
		t.Fatalf("defaults = %q/%q, want auto/passthrough", dto.DecisionsMode, dto.SystemOneMode)
	}
	dto, err = svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8301, Scheme: "http", DecisionsMode: "auto", SystemOneMode: "disabled",
	})
	if err != nil || dto.DecisionsMode != "auto" || dto.SystemOneMode != "disabled" {
		t.Fatalf("explicit = %+v, %v", dto, err)
	}

	for name, req := range map[string]CreateApplicationRequest{
		"responses auto":      {ResponsesMode: "auto"},
		"messages auto":       {MessagesMode: "auto"},
		"systemone translate": {SystemOneMode: "translate"},
		"systemone auto":      {SystemOneMode: "auto"},
		"decisions bogus":     {DecisionsMode: "bogus"},
	} {
		req.Type, req.Port, req.Scheme = routing.ProviderVLLM, 8310, "http"
		if _, err := svc.CreateApplication(ctx, ownerToken(), serverID, req); !errors.Is(err, ErrApplicationEndpointModeInvalid) {
			t.Errorf("%s: err = %v, want ErrApplicationEndpointModeInvalid", name, err)
		}
	}
}

func TestApplicationDecisionsPassthroughTypeGuard(t *testing.T) {
	ctx := context.Background()
	svc, _, serverID := decisionModesFixture(t)

	// Create: refused for llama_cpp, allowed for vllm.
	_, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderLlamaCPP, Port: 8320, Scheme: "http", DecisionsMode: "passthrough",
	})
	if !errors.Is(err, ErrApplicationDecisionsPassthroughUnsupported) {
		t.Fatalf("create llama_cpp passthrough: %v", err)
	}
	if _, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8321, Scheme: "http", DecisionsMode: "passthrough",
	}); err != nil {
		t.Fatalf("create vllm passthrough: %v", err)
	}

	llama, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: routing.ProviderLlamaCPP, Port: 8322, Scheme: "http"})
	if err != nil {
		t.Fatalf("create llama_cpp: %v", err)
	}
	// Update without type: 409.
	_, err = svc.UpdateApplication(ctx, ownerToken(), llama.ID, UpdateApplicationRequest{DecisionsMode: strPtr("passthrough")})
	if !errors.Is(err, ErrApplicationDecisionsPassthroughConflict) {
		t.Fatalf("update stored llama_cpp: %v, want conflict", err)
	}
	// Update that sends the type: 400.
	_, err = svc.UpdateApplication(ctx, ownerToken(), llama.ID, UpdateApplicationRequest{Type: strPtr(routing.ProviderLlamaCPP), DecisionsMode: strPtr("passthrough")})
	if !errors.Is(err, ErrApplicationDecisionsPassthroughUnsupported) {
		t.Fatalf("update with type: %v, want unsupported", err)
	}
	// Nothing was written.
	got, err := svc.GetApplication(ctx, ownerToken(), llama.ID)
	if err != nil || got.DecisionsMode != "auto" {
		t.Fatalf("after refusals: %+v, %v", got, err)
	}

	// Type change to llama_cpp with a stored passthrough resets to auto.
	vllm, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8323, Scheme: "http", DecisionsMode: "passthrough",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	retyped, err := svc.UpdateApplication(ctx, ownerToken(), vllm.ID, UpdateApplicationRequest{Type: strPtr(routing.ProviderLlamaCPP)})
	if err != nil {
		t.Fatalf("retype: %v", err)
	}
	if retyped.DecisionsMode != "auto" {
		t.Fatalf("decisions_mode after retype = %q, want auto", retyped.DecisionsMode)
	}
}

func TestUpdateApplicationDecisionModesKeepAndValidate(t *testing.T) {
	ctx := context.Background()
	svc, _, serverID := decisionModesFixture(t)
	app, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8330, Scheme: "http", DecisionsMode: "translate", SystemOneMode: "disabled",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// An unrelated update keeps both stored modes (never blanked).
	kept, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Priority: intPtr(3)})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if kept.DecisionsMode != "translate" || kept.SystemOneMode != "disabled" {
		t.Fatalf("kept = %q/%q", kept.DecisionsMode, kept.SystemOneMode)
	}
	for name, req := range map[string]UpdateApplicationRequest{
		"empty decisions":     {DecisionsMode: strPtr("")},
		"systemone translate": {SystemOneMode: strPtr("translate")},
		"systemone auto":      {SystemOneMode: strPtr("auto")},
		"responses auto":      {ResponsesMode: strPtr("auto")},
	} {
		if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, req); !errors.Is(err, ErrApplicationEndpointModeInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	changed, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{DecisionsMode: strPtr("auto"), SystemOneMode: strPtr("passthrough")})
	if err != nil || changed.DecisionsMode != "auto" || changed.SystemOneMode != "passthrough" {
		t.Fatalf("changed = %+v, %v", changed, err)
	}
}

func TestRuntimeSpecDecisionModes(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	// Absent -> defaults.
	m1 := newMapping("dm1")
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), m1, PutRuntimeSpecRequest{Binary: "/usr/local/bin/vllm", Type: "vllm"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if dto.DecisionsMode != "auto" || dto.SystemOneMode != "passthrough" {
		t.Fatalf("defaults = %q/%q", dto.DecisionsMode, dto.SystemOneMode)
	}

	// Explicit values round-trip through PUT/GET and putRequestFromDTO.
	dto, err = svc.PutRuntimeSpec(ctx, ownerToken(), m1, PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/vllm", Type: "vllm", DecisionsMode: "passthrough", SystemOneMode: "disabled",
	})
	if err != nil {
		t.Fatalf("put explicit: %v", err)
	}
	got, err := svc.GetRuntimeSpec(ctx, ownerToken(), m1)
	if err != nil || got.DecisionsMode != "passthrough" || got.SystemOneMode != "disabled" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	req := putRequestFromDTO(got)
	if req.DecisionsMode != "passthrough" || req.SystemOneMode != "disabled" {
		t.Fatalf("putRequestFromDTO dropped the modes: %q/%q", req.DecisionsMode, req.SystemOneMode)
	}
	req.IdleTimeoutSeconds = 777
	resaved, err := svc.PutRuntimeSpec(ctx, ownerToken(), m1, req)
	if err != nil || resaved.DecisionsMode != "passthrough" || resaved.SystemOneMode != "disabled" {
		t.Fatalf("resave = %+v, %v", resaved, err)
	}

	// Invalid values.
	for name, r := range map[string]PutRuntimeSpecRequest{
		"decisions bogus":     {DecisionsMode: "bogus"},
		"systemone translate": {SystemOneMode: "translate"},
		"systemone auto":      {SystemOneMode: "auto"},
		"responses auto":      {ResponsesMode: "auto"},
	} {
		r.Binary = "/usr/local/bin/vllm"
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), m1, r); !errors.Is(err, ErrRuntimeSpecEndpointModeInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// llama_cpp (explicit or detected from the binary) refuses passthrough.
	for name, r := range map[string]PutRuntimeSpecRequest{
		"explicit type": {Binary: "/x/server", Type: "llama_cpp", DecisionsMode: "passthrough"},
		"detected":      {Binary: "/usr/local/bin/llama-server", DecisionsMode: "passthrough"},
	} {
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("ll-"+strings.ReplaceAll(name, " ", "")), r); !errors.Is(err, ErrRuntimeSpecDecisionsPassthroughUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("ll-ok"), PutRuntimeSpecRequest{
		Binary: "/usr/local/bin/llama-server", Type: "llama_cpp", DecisionsMode: "translate",
	}); err != nil {
		t.Fatalf("llama_cpp translate: %v", err)
	}
}

func TestReservedManualDecisionVerdicts(t *testing.T) {
	if _, err := normalizeCapabilityVerdicts(map[string]string{routing.CapabilityDecisionOpenAI: routing.CapabilityYes}, nil); err != nil {
		t.Fatalf("decision_openai yes: %v", err)
	}
	if _, err := normalizeCapabilityVerdicts(map[string]string{routing.CapabilityDecisionOpenAI: ""}, nil); err != nil {
		t.Fatalf("decision_openai reset: %v", err)
	}
	if _, err := normalizeCapabilityVerdicts(map[string]string{routing.CapabilityDecisionOpenAI: routing.CapabilityNo}, nil); !errors.Is(err, ErrMappingCapabilityReserved) {
		t.Fatalf("decision_openai no: %v, want reserved", err)
	}
	for _, v := range []string{routing.CapabilityYes, routing.CapabilityNo, ""} {
		if _, err := normalizeCapabilityVerdicts(map[string]string{routing.CapabilityAnyDecision: v}, nil); !errors.Is(err, ErrMappingCapabilityReserved) {
			t.Fatalf("any_decision %q: %v, want reserved", v, err)
		}
	}
}

func TestCapabilityFoldCarriesDecisionOpenAI(t *testing.T) {
	rs := routing.NewMemoryStore()
	oa := routing.CapabilityRow{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceDecisionsProbe, CheckedAt: offeringTime}
	dec := routing.CapabilityRow{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceSystemOneProbe, CheckedAt: offeringTime}
	flavors := []string{routing.APIFlavorDecisions}
	offerSpeechModel(t, rs, routing.ProviderVLLM, flavors, nil, "oa-yes", oa)
	offerSpeechModel(t, rs, routing.ProviderVLLM, flavors, nil, "oa-none")
	offerSpeechModel(t, rs, routing.ProviderVLLM, flavors, nil, "dec-only", dec)
	offerSpeechModel(t, rs, routing.ProviderVLLM, []string{routing.APIFlavorOpenAI}, nil, "oa-no-flavor", oa)
	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(context.Background(), auth.Token{UserID: "usr_1"}))
	for name, want := range map[string]bool{"oa-yes": true, "oa-none": false, "dec-only": false, "oa-no-flavor": false} {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing", name)
		}
		if got.DecisionOpenAI != want {
			t.Errorf("%s decision_openai = %v, want %v", name, got.DecisionOpenAI, want)
		}
	}
	raw, _ := json.Marshal(ModelDTO{ID: "m"})
	if !strings.Contains(string(raw), `"decision_openai":false`) {
		t.Fatalf("decision_openai is not always on the wire: %s", raw)
	}
	for _, c := range []string{routing.CapabilityDecisionOpenAI, routing.CapabilityAnyDecision} {
		if !hasCapabilityFold(c) {
			t.Errorf("hasCapabilityFold(%s) = false", c)
		}
	}
	// any_decision is satisfied by either verdict (with the flavor).
	off := svc.ModelOfferingFor(context.Background(), auth.Token{UserID: "usr_1"}, routing.APIFlavorDecisions, []string{routing.CapabilityAnyDecision})
	for name, want := range map[string]bool{"oa-yes": true, "dec-only": true, "oa-none": false, "oa-no-flavor": false} {
		if _, got := off.Capable[name]; got != want {
			t.Errorf("any_decision capable[%s] = %v, want %v", name, got, want)
		}
	}
}

func TestMappingDTODecisionsRoute(t *testing.T) {
	yes := func(c string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: c, Verdict: routing.CapabilityYes}
	}
	caps := func(names ...string) map[string]routing.CapabilityRow {
		out := map[string]routing.CapabilityRow{}
		for _, n := range names {
			out[n] = yes(n)
		}
		return out
	}
	app := func(mode routing.EndpointMode, flavors ...string) routing.Application {
		return routing.Application{Type: routing.ProviderVLLM, APIFlavors: flavors, DecisionsMode: mode}
	}
	dec := routing.CapabilityDecision
	oa := routing.CapabilityDecisionOpenAI
	cases := []struct {
		name string
		app  routing.Application
		caps map[string]routing.CapabilityRow
		want string
	}{
		{"auto + decision_openai", app("auto", "decisions"), caps(dec, oa), "passthrough"},
		{"auto + decision only", app("auto", "decisions"), caps(dec), "translate"},
		{"auto no verdict", app("auto", "decisions"), caps(), ""},
		{"disabled", app("disabled", "decisions"), caps(dec), "disabled"},
		{"no decisions flavor", app("auto", "openai"), caps(dec, oa), ""},
		{"no decisions flavor, disabled", app("disabled", "openai"), caps(dec), ""},
		{"translate without decision", app("translate", "decisions"), caps(oa), ""},
		{"passthrough with verdict", app("passthrough", "decisions"), caps(oa), "passthrough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto := mappingDTO(routing.ModelMapping{ID: "m"}, tc.caps, effectiveFlavorMarkers(tc.app, routing.RuntimeSpec{}, false))
			if dto.DecisionsRoute != tc.want {
				t.Fatalf("decisions_route = %q, want %q", dto.DecisionsRoute, tc.want)
			}
		})
	}
	// The server_agent spec wins over the application.
	agent := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{"decisions"}, DecisionsMode: "auto"}
	spec := routing.RuntimeSpec{APIFlavors: []string{"decisions"}, DecisionsMode: "disabled"}
	if got := mappingDTO(routing.ModelMapping{}, caps(dec), effectiveFlavorMarkers(agent, spec, true)).DecisionsRoute; got != "disabled" {
		t.Fatalf("spec wins: %q", got)
	}
	raw, _ := json.Marshal(mappingDTO(routing.ModelMapping{}, nil, mappingFlavorMarkers{}))
	if !strings.Contains(string(raw), `"decisions_route":""`) {
		t.Fatalf("decisions_route not always on the wire: %s", raw)
	}
}

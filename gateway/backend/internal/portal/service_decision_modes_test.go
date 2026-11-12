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
	if dto.DecisionsMode != "auto" || dto.SystemOneMode != "auto" {
		t.Fatalf("defaults = %q/%q, want auto/auto", dto.DecisionsMode, dto.SystemOneMode)
	}
	// vllm_sr defaults to auto too (auto behaves like passthrough there).
	sr, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: routing.ProviderVllmSR, Port: 8302, Scheme: "http"})
	if err != nil || sr.SystemOneMode != "auto" {
		t.Fatalf("vllm_sr default = %+v, %v", sr, err)
	}
	dto, err = svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8301, Scheme: "http", DecisionsMode: "auto", SystemOneMode: "disabled",
	})
	if err != nil || dto.DecisionsMode != "auto" || dto.SystemOneMode != "disabled" {
		t.Fatalf("explicit = %+v, %v", dto, err)
	}
	for _, mode := range []string{"auto", "passthrough", "translate"} {
		port := 8340 + len(mode)
		dto, err = svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: port, Scheme: "http", SystemOneMode: mode,
		})
		if err != nil || dto.SystemOneMode != mode {
			t.Fatalf("systemone_mode %s = %+v, %v", mode, dto, err)
		}
	}

	for name, req := range map[string]CreateApplicationRequest{
		"responses auto":  {ResponsesMode: "auto"},
		"messages auto":   {MessagesMode: "auto"},
		"systemone bogus": {SystemOneMode: "bogus"},
		"decisions bogus": {DecisionsMode: "bogus"},
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
		"empty decisions": {DecisionsMode: strPtr("")},
		"empty systemone": {SystemOneMode: strPtr("")},
		"systemone bogus": {SystemOneMode: strPtr("bogus")},
		"responses auto":  {ResponsesMode: strPtr("auto")},
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
	if dto.DecisionsMode != "auto" || dto.SystemOneMode != "auto" {
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
		"decisions bogus": {DecisionsMode: "bogus"},
		"systemone bogus": {SystemOneMode: "bogus"},
		"responses auto":  {ResponsesMode: "auto"},
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
	// A kind without an OpenAI /v1/decisions route ignores a decision_openai
	// verdict, like the gateway's handler (the same routing helper).
	for _, kind := range []string{routing.ProviderVllmSR, routing.ProviderLlamaCPP} {
		a := routing.Application{Type: kind, APIFlavors: []string{"decisions"}, DecisionsMode: "auto"}
		if got := mappingDTO(routing.ModelMapping{}, caps(dec, oa), effectiveFlavorMarkers(a, routing.RuntimeSpec{}, false)).DecisionsRoute; got != "translate" {
			t.Fatalf("%s application, decision + decision_openai: decisions_route = %q, want translate", kind, got)
		}
	}
	for name, sp := range map[string]routing.RuntimeSpec{
		"vllm_sr spec":          {Type: "vllm_sr"},
		"llama_cpp spec":        {Type: "llama_cpp"},
		"detected vllm-srun":    {Binary: "/opt/vllm-srun"},
		"detected llama-server": {Binary: "/opt/llama-server"},
	} {
		sp.APIFlavors, sp.DecisionsMode = []string{"decisions"}, "auto"
		a := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{"decisions"}, DecisionsMode: "auto"}
		if got := mappingDTO(routing.ModelMapping{}, caps(dec, oa), effectiveFlavorMarkers(a, sp, true)).DecisionsRoute; got != "translate" {
			t.Fatalf("%s: decisions_route = %q, want translate", name, got)
		}
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

// TestVllmSRRefusesDecisionsPassthrough: the vllm_sr application and spec
// types serve the decision endpoints themselves but have no native
// /v1/decisions route, so passthrough is refused like llama_cpp.
func TestVllmSRRefusesDecisionsPassthrough(t *testing.T) {
	ctx := context.Background()
	svc, _, serverID := decisionModesFixture(t)

	_, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVllmSR, Port: 8330, Scheme: "http", DecisionsMode: "passthrough",
	})
	if !errors.Is(err, ErrApplicationDecisionsPassthroughUnsupported) {
		t.Fatalf("create vllm_sr passthrough: %v", err)
	}
	sr, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: routing.ProviderVllmSR, Port: 8331, Scheme: "http"})
	if err != nil {
		t.Fatalf("create vllm_sr: %v", err)
	}
	_, err = svc.UpdateApplication(ctx, ownerToken(), sr.ID, UpdateApplicationRequest{DecisionsMode: strPtr("passthrough")})
	if !errors.Is(err, ErrApplicationDecisionsPassthroughConflict) {
		t.Fatalf("update stored vllm_sr: %v, want conflict", err)
	}

	vllm, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8332, Scheme: "http", DecisionsMode: "passthrough",
	})
	if err != nil {
		t.Fatalf("create vllm: %v", err)
	}
	retyped, err := svc.UpdateApplication(ctx, ownerToken(), vllm.ID, UpdateApplicationRequest{Type: strPtr(routing.ProviderVllmSR)})
	if err != nil || retyped.DecisionsMode != "auto" {
		t.Fatalf("retype = %+v, %v; want decisions_mode auto", retyped, err)
	}
}

func TestRuntimeSpecVllmSR(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)

	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("sr1"), PutRuntimeSpecRequest{
		Binary: "/opt/vllm-sr/.venv/bin/vllm-srun", Type: "vllm_sr", DecisionsMode: "passthrough",
	}); !errors.Is(err, ErrRuntimeSpecDecisionsPassthroughUnsupported) {
		t.Fatalf("explicit vllm_sr passthrough: %v", err)
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("sr2"), PutRuntimeSpecRequest{
		Binary: "/opt/vllm-sr/.venv/bin/vllm-srun", DecisionsMode: "passthrough",
	}); !errors.Is(err, ErrRuntimeSpecDecisionsPassthroughUnsupported) {
		t.Fatalf("detected vllm_sr passthrough: %v", err)
	}
	dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("sr3"), PutRuntimeSpecRequest{
		Binary: "/opt/vllm-sr/.venv/bin/vllm-srun", Type: "vllm_sr",
	})
	if err != nil || dto.Type != "vllm_sr" {
		t.Fatalf("vllm_sr spec = %+v, %v", dto, err)
	}
}

// TestDecisionOpenAIFoldsAreKindAware: the decision_openai listing flag and the
// any_decision offering both ignore a decision_openai verdict on a kind that
// cannot take a /v1/decisions passthrough (routing.DecisionsPassthroughCapableKind),
// judged on the application type, or on a server_agent mapping's effective
// spec type. A vllm kind with the same verdict keeps both.
func TestDecisionOpenAIFoldsAreKindAware(t *testing.T) {
	rs := routing.NewMemoryStore()
	oa := routing.CapabilityRow{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceDecisionsProbe, CheckedAt: offeringTime}
	flavors := []string{routing.APIFlavorDecisions}
	offerSpeechModel(t, rs, routing.ProviderLlamaCPP, flavors, nil, "oa-llama", oa)
	offerSpeechModel(t, rs, routing.ProviderVllmSR, flavors, nil, "oa-sr", oa)
	offerSpeechModel(t, rs, routing.ProviderVLLM, flavors, nil, "oa-vllm", oa)
	for gateway, specType := range map[string]routing.RuntimeSpecType{
		"oa-agent-llama": routing.RuntimeSpecTypeLlamaCpp,
		"oa-agent-sr":    routing.RuntimeSpecTypeVllmSR,
		"oa-agent-vllm":  routing.RuntimeSpecTypeVLLM,
	} {
		offerSpeechModel(t, rs, routing.ProviderServerAgent, flavors, flavors, gateway, oa)
		if err := rs.UpsertRuntimeSpec(context.Background(), routing.RuntimeSpec{ID: "app_" + gateway + "_spec", MappingID: "map_" + gateway, Binary: "/opt/x/server", Type: string(specType), Args: "[]", Env: "{}", APIFlavors: flavors}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", gateway, err)
		}
	}
	want := map[string]bool{
		"oa-llama": false, "oa-sr": false, "oa-vllm": true,
		"oa-agent-llama": false, "oa-agent-sr": false, "oa-agent-vllm": true,
	}
	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(context.Background(), auth.Token{UserID: "usr_1"}))
	off := svc.ModelOfferingFor(context.Background(), auth.Token{UserID: "usr_1"}, routing.APIFlavorDecisions, []string{routing.CapabilityAnyDecision})
	for name, w := range want {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from the listing", name)
		}
		if got.DecisionOpenAI != w {
			t.Errorf("%s decision_openai = %v, want %v", name, got.DecisionOpenAI, w)
		}
		if _, capable := off.Capable[name]; capable != w {
			t.Errorf("%s any_decision capable = %v, want %v", name, capable, w)
		}
	}
}

// TestApplicationSystemOneTranslateTypeGuard: systemone_mode translate needs an
// OpenAI /v1/decisions upstream, so llama_cpp and vllm_sr refuse it like they
// refuse a decisions passthrough (ADR-100): 400 when the request supplies the
// type, 409 when the stored type is kept, and a retype resets a stored
// translate to auto. A stored passthrough is kept on an unrelated update.
func TestApplicationSystemOneTranslateTypeGuard(t *testing.T) {
	ctx := context.Background()
	svc, _, serverID := decisionModesFixture(t)
	port := 8400
	for _, kind := range []string{routing.ProviderLlamaCPP, routing.ProviderVllmSR} {
		port += 10
		_, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
			Type: kind, Port: port, Scheme: "http", SystemOneMode: "translate",
		})
		if !errors.Is(err, ErrApplicationSystemOneTranslateUnsupported) {
			t.Fatalf("create %s translate: %v", kind, err)
		}
		app, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: kind, Port: port + 1, Scheme: "http"})
		if err != nil {
			t.Fatalf("create %s: %v", kind, err)
		}
		if _, err = svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{SystemOneMode: strPtr("translate")}); !errors.Is(err, ErrApplicationSystemOneTranslateConflict) {
			t.Fatalf("update stored %s: %v, want conflict", kind, err)
		}
		if _, err = svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Type: strPtr(kind), SystemOneMode: strPtr("translate")}); !errors.Is(err, ErrApplicationSystemOneTranslateUnsupported) {
			t.Fatalf("update with type %s: %v, want unsupported", kind, err)
		}
		got, err := svc.GetApplication(ctx, ownerToken(), app.ID)
		if err != nil || got.SystemOneMode != "auto" {
			t.Fatalf("after refusals: %+v, %v", got, err)
		}
		// Passthrough and disabled stay allowed.
		for _, mode := range []string{"passthrough", "disabled", "auto"} {
			if _, err = svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{SystemOneMode: strPtr(mode)}); err != nil {
				t.Fatalf("%s %s: %v", kind, mode, err)
			}
		}
	}

	vllm, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8450, Scheme: "http", SystemOneMode: "translate",
	})
	if err != nil {
		t.Fatalf("create vllm translate: %v", err)
	}
	retyped, err := svc.UpdateApplication(ctx, ownerToken(), vllm.ID, UpdateApplicationRequest{Type: strPtr(routing.ProviderVllmSR)})
	if err != nil || retyped.SystemOneMode != "auto" {
		t.Fatalf("retype resets translate: %+v, %v", retyped, err)
	}

	// A stored Phase 1 passthrough is kept across an unrelated update, and a
	// retype to an incapable kind keeps it (only translate is reset).
	legacy, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8460, Scheme: "http", SystemOneMode: "passthrough",
	})
	if err != nil {
		t.Fatalf("create legacy: %v", err)
	}
	kept, err := svc.UpdateApplication(ctx, ownerToken(), legacy.ID, UpdateApplicationRequest{Priority: intPtr(2)})
	if err != nil || kept.SystemOneMode != "passthrough" {
		t.Fatalf("kept = %+v, %v", kept, err)
	}
	kept, err = svc.UpdateApplication(ctx, ownerToken(), legacy.ID, UpdateApplicationRequest{Type: strPtr(routing.ProviderLlamaCPP)})
	if err != nil || kept.SystemOneMode != "passthrough" {
		t.Fatalf("retype keeps passthrough: %+v, %v", kept, err)
	}
}

func TestRuntimeSpecSystemOneTranslateGuard(t *testing.T) {
	ctx := context.Background()
	svc, _, _, newMapping := liveTimingsSpecFixture(t)
	for name, r := range map[string]PutRuntimeSpecRequest{
		"llama_cpp explicit": {Binary: "/x/server", Type: "llama_cpp", SystemOneMode: "translate"},
		"llama_cpp detected": {Binary: "/usr/local/bin/llama-server", SystemOneMode: "translate"},
		"vllm_sr explicit":   {Binary: "/x/server", Type: "vllm_sr", SystemOneMode: "translate"},
	} {
		if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("s1-"+strings.ReplaceAll(name, " ", "")), r); !errors.Is(err, ErrRuntimeSpecSystemOneTranslateUnsupported) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	for _, mode := range []string{"auto", "translate", "passthrough", "disabled"} {
		dto, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("s1-ok-"+mode), PutRuntimeSpecRequest{
			Binary: "/usr/local/bin/vllm", Type: "vllm", SystemOneMode: mode,
		})
		if err != nil || dto.SystemOneMode != mode {
			t.Errorf("vllm %s: %+v, %v", mode, dto, err)
		}
	}
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("s1-sr-auto"), PutRuntimeSpecRequest{
		Binary: "/x/server", Type: "vllm_sr", SystemOneMode: "auto",
	}); err != nil {
		t.Fatalf("vllm_sr auto: %v", err)
	}
}

func TestMappingDTOSystemOneRoute(t *testing.T) {
	caps := func(names ...string) map[string]routing.CapabilityRow {
		out := map[string]routing.CapabilityRow{}
		for _, n := range names {
			out[n] = routing.CapabilityRow{Capability: n, Verdict: routing.CapabilityYes}
		}
		return out
	}
	dec, oa := routing.CapabilityDecision, routing.CapabilityDecisionOpenAI
	app := func(kind string, mode routing.EndpointMode, flavors ...string) routing.Application {
		return routing.Application{Type: kind, APIFlavors: flavors, SystemOneMode: mode}
	}
	cases := []struct {
		name string
		app  routing.Application
		caps map[string]routing.CapabilityRow
		want string
	}{
		{"auto + decision", app("vllm", "auto", "decisions"), caps(dec, oa), "passthrough"},
		{"auto + decision_openai only", app("vllm", "auto", "decisions"), caps(oa), "translate"},
		{"auto no verdict", app("vllm", "auto", "decisions"), caps(), ""},
		{"blank behaves as auto", app("vllm", "", "decisions"), caps(oa), "translate"},
		{"passthrough with decision", app("vllm", "passthrough", "decisions"), caps(dec), "passthrough"},
		{"passthrough without decision", app("vllm", "passthrough", "decisions"), caps(oa), ""},
		{"translate with decision_openai", app("vllm", "translate", "decisions"), caps(oa), "translate"},
		{"translate without decision_openai", app("vllm", "translate", "decisions"), caps(dec), ""},
		{"disabled", app("vllm", "disabled", "decisions"), caps(dec), "disabled"},
		{"no decisions flavor", app("vllm", "auto", "openai"), caps(dec, oa), ""},
		{"llama_cpp ignores decision_openai", app(routing.ProviderLlamaCPP, "auto", "decisions"), caps(oa), ""},
		{"llama_cpp stale translate", app(routing.ProviderLlamaCPP, "translate", "decisions"), caps(oa), ""},
		{"vllm_sr auto passthrough", app(routing.ProviderVllmSR, "auto", "decisions"), caps(dec, oa), "passthrough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dto := mappingDTO(routing.ModelMapping{ID: "m"}, tc.caps, effectiveFlavorMarkers(tc.app, routing.RuntimeSpec{}, false))
			if dto.SystemOneRoute != tc.want {
				t.Fatalf("systemone_route = %q, want %q", dto.SystemOneRoute, tc.want)
			}
		})
	}
	// The server_agent spec wins over the application.
	agent := routing.Application{Type: routing.ProviderServerAgent, APIFlavors: []string{"decisions"}, SystemOneMode: "auto"}
	spec := routing.RuntimeSpec{APIFlavors: []string{"decisions"}, SystemOneMode: "disabled"}
	if got := mappingDTO(routing.ModelMapping{}, caps(dec), effectiveFlavorMarkers(agent, spec, true)).SystemOneRoute; got != "disabled" {
		t.Fatalf("spec wins: %q", got)
	}
	raw, _ := json.Marshal(mappingDTO(routing.ModelMapping{}, nil, mappingFlavorMarkers{}))
	if !strings.Contains(string(raw), `"systemone_route":""`) {
		t.Fatalf("systemone_route not always on the wire: %s", raw)
	}
}

// TestSystemOneDecisionFoldJudgesTheRoute: ModelDTO.Decision and the
// /v1/systemone redirect's Capable follow the System One route (ADR-100). A
// mapping with only decision_openai yes is listed under auto on a kind that can
// take /v1/decisions and is not on llama_cpp or vllm_sr; a Phase 1 row
// (passthrough, decision yes) is unchanged.
func TestSystemOneDecisionFoldJudgesTheRoute(t *testing.T) {
	rs := routing.NewMemoryStore()
	oaOnly := []routing.CapabilityRow{{Capability: routing.CapabilityDecisionOpenAI, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}}
	decOnly := []routing.CapabilityRow{{Capability: routing.CapabilityDecision, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceManual, CheckedAt: offeringTime}}
	seed := func(gateway, kind string, mode routing.EndpointMode, rows []routing.CapabilityRow) {
		t.Helper()
		ctx := context.Background()
		srv, appID, mapID := "srv_"+gateway, "app_"+gateway, "map_"+gateway
		if err := rs.CreateAIServer(ctx, routing.AIServer{ID: srv, Name: srv, Domain: srv + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatal(err)
		}
		if err := rs.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srv, Type: kind, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorDecisions}, SystemOneMode: mode, DecisionsMode: routing.EndpointModeAuto, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatal(err)
		}
		if err := rs.CreateMapping(ctx, routing.ModelMapping{ID: mapID, ApplicationID: appID, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, CreatedAt: offeringTime, UpdatedAt: offeringTime}); err != nil {
			t.Fatal(err)
		}
		if err := rs.UpsertMappingCapabilities(ctx, mapID, rows); err != nil {
			t.Fatal(err)
		}
	}
	seed("auto-vllm-oa", routing.ProviderVLLM, routing.EndpointModeAuto, oaOnly)
	seed("auto-llama-oa", routing.ProviderLlamaCPP, routing.EndpointModeAuto, oaOnly)
	seed("auto-sr-oa", routing.ProviderVllmSR, routing.EndpointModeAuto, oaOnly)
	seed("auto-vllm-dec", routing.ProviderVLLM, routing.EndpointModeAuto, decOnly)
	seed("legacy-pass-dec", routing.ProviderVLLM, routing.EndpointModePassthrough, decOnly)
	seed("pass-vllm-oa", routing.ProviderVLLM, routing.EndpointModePassthrough, oaOnly)
	seed("translate-vllm-oa", routing.ProviderVLLM, routing.EndpointModeTranslate, oaOnly)
	seed("translate-vllm-dec", routing.ProviderVLLM, routing.EndpointModeTranslate, decOnly)
	seed("stale-translate-llama-oa", routing.ProviderLlamaCPP, routing.EndpointModeTranslate, oaOnly)
	seed("disabled-vllm-dec", routing.ProviderVLLM, routing.EndpointModeDisabled, decOnly)

	want := map[string]bool{
		"auto-vllm-oa": true, "auto-llama-oa": false, "auto-sr-oa": false, "auto-vllm-dec": true,
		"legacy-pass-dec": true, "pass-vllm-oa": false, "translate-vllm-oa": true, "translate-vllm-dec": false,
		"stale-translate-llama-oa": false, "disabled-vllm-dec": false,
	}
	svc := offerSvc(rs, nil)
	byID := modelsByID(svc.Models(context.Background(), auth.Token{UserID: "usr_1"}))
	off := svc.ModelOfferingFor(context.Background(), auth.Token{UserID: "usr_1"}, routing.APIFlavorDecisions, []string{routing.CapabilityDecision})
	for name, w := range want {
		got, ok := byID[name]
		if !ok {
			t.Fatalf("%s missing from the listing", name)
		}
		if got.Decision != w {
			t.Errorf("%s decision = %v, want %v", name, got.Decision, w)
		}
		if _, capable := off.Capable[name]; capable != w {
			t.Errorf("%s systemone capable = %v, want %v", name, capable, w)
		}
	}
}

// TestBothIncapableDecisionModesReportBoth: a save that sends decisions_mode
// passthrough AND systemone_mode translate for a kind lacking /v1/decisions
// reports both refusals, not only the first. errors.Is holds for both
// sentinels, the message names both modes, and the first sentinel (decisions)
// stays the one an HTTP mapping reaches first.
func TestBothIncapableDecisionModesReportBoth(t *testing.T) {
	ctx := context.Background()
	assertBoth := func(t *testing.T, err, decisionsErr, systemOneErr error) {
		t.Helper()
		if !errors.Is(err, decisionsErr) || !errors.Is(err, systemOneErr) {
			t.Fatalf("err = %v, want both %v and %v", err, decisionsErr, systemOneErr)
		}
		if msg := err.Error(); !strings.HasPrefix(msg, decisionsErr.Error()+": ") ||
			!strings.Contains(msg, "decisions_mode passthrough") || !strings.Contains(msg, "systemone_mode translate") {
			t.Fatalf("message %q must lead with the decisions sentinel and name both modes", msg)
		}
	}

	t.Run("application create", func(t *testing.T) {
		svc, _, serverID := decisionModesFixture(t)
		_, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{
			Type: routing.ProviderLlamaCPP, Port: 8330, Scheme: "http", DecisionsMode: "passthrough", SystemOneMode: "translate",
		})
		assertBoth(t, err, ErrApplicationDecisionsPassthroughUnsupported, ErrApplicationSystemOneTranslateUnsupported)
	})
	t.Run("application update", func(t *testing.T) {
		svc, _, serverID := decisionModesFixture(t)
		llama, err := svc.CreateApplication(ctx, ownerToken(), serverID, CreateApplicationRequest{Type: routing.ProviderLlamaCPP, Port: 8331, Scheme: "http"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		_, err = svc.UpdateApplication(ctx, ownerToken(), llama.ID, UpdateApplicationRequest{
			Type: strPtr(routing.ProviderLlamaCPP), DecisionsMode: strPtr("passthrough"), SystemOneMode: strPtr("translate"),
		})
		assertBoth(t, err, ErrApplicationDecisionsPassthroughUnsupported, ErrApplicationSystemOneTranslateUnsupported)
	})
	t.Run("runtime spec", func(t *testing.T) {
		svc, _, _, newMapping := liveTimingsSpecFixture(t)
		_, err := svc.PutRuntimeSpec(ctx, ownerToken(), newMapping("both-modes"), PutRuntimeSpecRequest{
			Binary: "/x/server", Type: "llama_cpp", DecisionsMode: "passthrough", SystemOneMode: "translate",
		})
		assertBoth(t, err, ErrRuntimeSpecDecisionsPassthroughUnsupported, ErrRuntimeSpecSystemOneTranslateUnsupported)
	})
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"testing"
	"time"
)

// decisionRouteApp is one ordinary (or server_agent) decision application for
// the route-aware candidate tests: its server, application, mapping and
// verdict rows, all named after id.
type decisionRouteApp struct {
	id        string
	kind      string
	model     string
	latency   int
	decisions EndpointMode
	systemOne EndpointMode
	verdicts  []string // capability names carrying a "yes" verdict
}

// seedDecisionRouteStore seeds one server, application and mapping per app,
// every application serving the decisions flavor, and the verdict rows. The
// latency orders the scorer: the lower latency wins a tie-free selection, so
// a test puts the mapping that must NOT be chosen on the faster server.
func seedDecisionRouteStore(t *testing.T, now time.Time, apps ...decisionRouteApp) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	for _, a := range apps {
		srv, app, mapping := "srv_"+a.id, "app_"+a.id, "map_"+a.id
		if err := store.CreateAIServer(ctx, AIServer{ID: srv, Name: srv, Domain: srv + ".test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srv, err)
		}
		if err := store.CreateApplication(ctx, Application{
			ID: app, ServerID: srv, Type: a.kind, Port: 8000, Scheme: "http",
			APIFlavors: []string{APIFlavorDecisions}, Priority: 10, Weight: 50, TimeoutMS: 30000,
			AffinityTTLSeconds: 1800, Status: ServerStatusActive,
			DecisionsMode: a.decisions, SystemOneMode: a.systemOne, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("CreateApplication %s: %v", app, err)
		}
		if err := store.CreateMapping(ctx, ModelMapping{ID: mapping, ApplicationID: app, GatewayModelName: a.model, AppModelName: a.model + "-up", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", mapping, err)
		}
		if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: srv, ReportedAt: now, LatencyMS: a.latency, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry %s: %v", srv, err)
		}
		rows := make([]CapabilityRow, 0, len(a.verdicts))
		for _, name := range a.verdicts {
			rows = append(rows, CapabilityRow{Capability: name, Verdict: CapabilityYes, Source: CapabilitySourceManual, CheckedAt: now})
		}
		if len(rows) > 0 {
			if err := store.UpsertMappingCapabilities(ctx, mapping, rows); err != nil {
				t.Fatalf("UpsertMappingCapabilities %s: %v", mapping, err)
			}
		}
	}
	return store
}

// resolveDecisionMappingN resolves a decision request n times and fails on any
// error or any mapping other than want.
func resolveDecisionMappingN(t *testing.T, resolver *Resolver, model, flavor, want string, n int) {
	t.Helper()
	token := auth.Token{ID: "tok_dec", UserID: "usr_dec", Active: true}
	for i := range n {
		target, err := resolver.Resolve(context.Background(), token, decisionRequest(model, flavor))
		if err != nil {
			t.Fatalf("Resolve #%d (%s): %v", i, flavor, err)
		}
		if target.RouteID != want {
			t.Fatalf("Resolve #%d (%s) = %s, want %s", i, flavor, target.RouteID, want)
		}
	}
}

// TestSystemOneSkipsAStoredPassthroughWithoutTheDecisionVerdict pins the Phase 1
// System One choice. Migration 107 stored systemone_mode passthrough on every
// existing row, so an ordinary mapping with only a decision_openai verdict
// satisfies [any_decision] but has no route on /v1/systemone
// (EffectiveSystemOneRoute: passthrough needs the decision verdict). It must
// not be a candidate: otherwise it can win the selection against a decision
// sibling and the request ends in routing.model_not_capable after resolve.
func TestSystemOneSkipsAStoredPassthroughWithoutTheDecisionVerdict(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := seedDecisionRouteStore(t, now,
		decisionRouteApp{id: "oa", kind: ProviderVLLM, model: "dec-model", latency: 100, decisions: EndpointModeAuto, systemOne: EndpointModePassthrough, verdicts: []string{CapabilityDecisionOpenAI}},
		decisionRouteApp{id: "s1", kind: ProviderVLLM, model: "dec-model", latency: 900, decisions: EndpointModeAuto, systemOne: EndpointModePassthrough, verdicts: []string{CapabilityDecision}},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolveDecisionMappingN(t, resolver, "dec-model", APIFlavorDecisions, "map_s1", 10)
	// /v1/decisions keeps both: auto routes the decision_openai mapping by
	// passthrough, so the faster one still wins there.
	resolveDecisionMappingN(t, resolver, "dec-model", APIFlavorOpenAIDecisions, "map_oa", 3)
}

// TestDecisionsSkipsAForcedPassthroughWithoutTheOpenAIVerdict is the
// /v1/decisions twin: an ordinary mapping forced to passthrough with only a
// System One verdict has no route (EffectiveDecisionsRoute), so its auto
// sibling, which translates, is chosen.
func TestDecisionsSkipsAForcedPassthroughWithoutTheOpenAIVerdict(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := seedDecisionRouteStore(t, now,
		decisionRouteApp{id: "pt", kind: ProviderVLLM, model: "dec-model", latency: 100, decisions: EndpointModePassthrough, systemOne: EndpointModeAuto, verdicts: []string{CapabilityDecision}},
		decisionRouteApp{id: "auto", kind: ProviderVLLM, model: "dec-model", latency: 900, decisions: EndpointModeAuto, systemOne: EndpointModeAuto, verdicts: []string{CapabilityDecision}},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolveDecisionMappingN(t, resolver, "dec-model", APIFlavorOpenAIDecisions, "map_auto", 10)
	// System One serves both by passthrough, so the faster one wins there.
	resolveDecisionMappingN(t, resolver, "dec-model", APIFlavorDecisions, "map_pt", 3)
}

// TestDecisionGroupMemberWithoutARouteIsSkipped: the group walk reads the same
// candidate filter (memberPool). A first member whose only mapping has no
// System One route yields no candidate, so the walk moves on to the second.
func TestDecisionGroupMemberWithoutARouteIsSkipped(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	store := seedDecisionRouteStore(t, now,
		decisionRouteApp{id: "oa", kind: ProviderVLLM, model: "dec-a", latency: 100, decisions: EndpointModeAuto, systemOne: EndpointModePassthrough, verdicts: []string{CapabilityDecisionOpenAI}},
		decisionRouteApp{id: "s1", kind: ProviderVLLM, model: "dec-b", latency: 900, decisions: EndpointModeAuto, systemOne: EndpointModePassthrough, verdicts: []string{CapabilityDecision}},
	)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetGroupResolver(&fakeGroups{groups: map[string]fakeGroup{
		"dec-group": {members: []GroupMember{
			{ID: "gm_a", GroupID: "grp_dec", MemberGatewayName: "dec-a", Priority: 0},
			{ID: "gm_b", GroupID: "grp_dec", MemberGatewayName: "dec-b", Priority: 1},
		}},
	}})
	resolveDecisionMappingN(t, resolver, "dec-group", APIFlavorDecisions, "map_s1", 5)
}

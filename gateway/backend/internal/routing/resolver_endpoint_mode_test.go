// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

func TestTargetCarriesAppEndpointModes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	// Give both seeded apps both flavors + explicit modes.
	for _, id := range []string{"app_fast", "app_slow"} {
		app, err := store.ApplicationByID(ctx, id)
		must(t, err)
		app.APIFlavors = []string{APIFlavorOpenAI, APIFlavorAnthropic}
		app.ResponsesMode = EndpointModeTranslate
		app.MessagesMode = EndpointModePassthrough
		must(t, store.UpdateApplication(ctx, app))
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat_completions"})
	must(t, err)
	if target.ResponsesMode != EndpointModeTranslate {
		t.Fatalf("target.ResponsesMode = %q, want translate", target.ResponsesMode)
	}
	if target.MessagesMode != EndpointModePassthrough {
		t.Fatalf("target.MessagesMode = %q, want passthrough", target.MessagesMode)
	}
	if len(target.APIFlavors) != 2 {
		t.Fatalf("target.APIFlavors = %v, want the app's two flavors", target.APIFlavors)
	}
}

func TestTargetCarriesSpecModesForServerAgent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	// App level says translate/translate — the spec must WIN over this.
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI, APIFlavorAnthropic}, ResponsesMode: EndpointModeTranslate, MessagesMode: EndpointModeTranslate, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{ID: "spec", MappingID: "map", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModePassthrough, MessagesMode: EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err)
	if target.ResponsesMode != EndpointModePassthrough {
		t.Fatalf("target.ResponsesMode = %q, want passthrough (spec wins over app's translate)", target.ResponsesMode)
	}
	if target.MessagesMode != EndpointModeDisabled {
		t.Fatalf("target.MessagesMode = %q, want disabled (spec value)", target.MessagesMode)
	}
	if len(target.APIFlavors) != 1 || target.APIFlavors[0] != APIFlavorOpenAI {
		t.Fatalf("target.APIFlavors = %v, want [openai] (spec value)", target.APIFlavors)
	}
}

func TestTargetFallsBackToAppModesWhenServerAgentHasNoSpec(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModeTranslate, MessagesMode: EndpointModePassthrough, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	// No UpsertRuntimeSpec -> app values are the fallback.

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err)
	if target.ResponsesMode != EndpointModeTranslate {
		t.Fatalf("target.ResponsesMode = %q, want translate (app fallback)", target.ResponsesMode)
	}
}

func TestCandidacyExcludesResponsesDisabledOrdinaryApp(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now) // both apps: Type=mock, APIFlavors=[openai]
	for _, id := range []string{"app_fast", "app_slow"} {
		app, err := store.ApplicationByID(ctx, id)
		must(t, err)
		app.ResponsesMode = EndpointModeDisabled
		must(t, store.UpdateApplication(ctx, app))
	}
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	// Codex (openai_responses) -> no route (both ordinary apps disabled).
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "t1", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_responses"}); err != ErrNoModelRoute {
		t.Fatalf("openai_responses: err = %v, want ErrNoModelRoute", err)
	}
	// Plain chat-completions is UNAFFECTED by ResponsesMode (coarse openai gate).
	if _, err := resolver.Resolve(ctx, auth.Token{ID: "t2", UserID: "u", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat_completions"}); err != nil {
		t.Fatalf("openai_chat_completions should still route: %v", err)
	}
}

func TestCandidacyDoesNotModeGateServerAgent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	// App-level ResponsesMode=disabled, but a server_agent app must STILL be a
	// candidate — the spec is the authority, enforced later at dispatch.
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModeDisabled, MessagesMode: EndpointModeDisabled, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{ID: "spec", MappingID: "map", APIFlavors: []string{APIFlavorOpenAI}, ResponsesMode: EndpointModePassthrough, MessagesMode: EndpointModeDisabled, CreatedAt: now, UpdatedAt: now}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "t", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_responses"})
	must(t, err) // NOT ErrNoModelRoute: server_agent survives candidacy
	if target.ResponsesMode != EndpointModePassthrough {
		t.Fatalf("target.ResponsesMode = %q, want passthrough (spec)", target.ResponsesMode)
	}
}

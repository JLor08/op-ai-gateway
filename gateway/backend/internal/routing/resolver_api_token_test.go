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

func TestTargetUsesSpecTokenForServerAgentSetMode(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, APIToken: "enc:app", APITokenHeader: "Authorization", Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{
		ID: "spec", MappingID: "map",
		APITokenMode:         string(RuntimeAPITokenModeSet),
		APIToken:             "enc:spec",
		APITokenHeaderSource: string(RuntimeAPITokenHeaderSourceCustom),
		APITokenHeader:       "X-Api-Key",
		CreatedAt:            now, UpdatedAt: now,
	}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_chat_completions"})
	must(t, err)
	if target.APIToken != "enc:spec" {
		t.Fatalf("target.APIToken = %q, want the SEALED spec token %q", target.APIToken, "enc:spec")
	}
	if target.APITokenHeader != "X-Api-Key" {
		t.Fatalf("target.APITokenHeader = %q, want the spec's custom header %q", target.APITokenHeader, "X-Api-Key")
	}
}

func TestTargetOffModeOverridesAppTokenForServerAgent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	// The app HAS a token, but the spec says off -> must NOT be sent.
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, APIToken: "enc:app", APITokenHeader: "Authorization", Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	must(t, store.UpsertRuntimeSpec(ctx, RuntimeSpec{
		ID: "spec", MappingID: "map",
		APITokenMode: string(RuntimeAPITokenModeOff),
		CreatedAt:    now, UpdatedAt: now,
	}))

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_chat_completions"})
	must(t, err)
	if target.APIToken != "" {
		t.Fatalf("target.APIToken = %q, want \"\" (off overrides the app token, which was %q)", target.APIToken, "enc:app")
	}
	if target.APITokenHeader != "" {
		t.Fatalf("target.APITokenHeader = %q, want \"\" (off)", target.APITokenHeader)
	}
}

func TestTargetFallsBackToAppTokenWhenServerAgentHasNoSpec(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	must(t, store.CreateAIServer(ctx, AIServer{ID: "srv", Domain: "srv.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateApplication(ctx, Application{ID: "app", ServerID: "srv", Type: ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, APIToken: "enc:app", APITokenHeader: "Authorization", Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.CreateMapping(ctx, ModelMapping{ID: "map", ApplicationID: "app", GatewayModelName: "m", AppModelName: "m", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(t, store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv", ReportedAt: now, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}))
	// No UpsertRuntimeSpec -> app token is the fallback.

	resolver := NewResolver(store, func() time.Time { return now }, nil)
	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok", UserID: "u", Active: true}, inference.Request{Model: "m", APIFlavor: "openai_chat_completions"})
	must(t, err)
	if target.APIToken != "enc:app" {
		t.Fatalf("target.APIToken = %q, want app fallback %q", target.APIToken, "enc:app")
	}
	if target.APITokenHeader != "Authorization" {
		t.Fatalf("target.APITokenHeader = %q, want app fallback %q", target.APITokenHeader, "Authorization")
	}
}

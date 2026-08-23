// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"testing"
	"time"
)

// TestServiceModelsContextSizeSingleMapping: a model offered by a single
// mapping surfaces that mapping's context_size verbatim.
func TestServiceModelsContextSizeSingleMapping(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, ContextSize: 8192, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	byID := modelsByID(got)
	if byID["qwen-coder"].ContextSize != 8192 {
		t.Fatalf("qwen-coder context_size = %d, want 8192", byID["qwen-coder"].ContextSize)
	}
}

// TestServiceModelsContextSizeMinAcrossMappings: when the same gateway model
// name is offered by two mappings (on different servers) with DIFFERENT
// context_size values, the DTO reports the MINIMUM known (>0) value —
// conservative, so the reported context window never overstates what every
// offering server can actually serve.
func TestServiceModelsContextSizeMinAcrossMappings(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	mustServer := func(id, name string) {
		if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: id, Name: name, Domain: id + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", id, err)
		}
	}
	mustApp := func(id, srv string, port int) {
		if err := routeStore.CreateApplication(ctx, routing.Application{ID: id, ServerID: srv, Type: routing.ProviderVLLM, Port: port, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", id, err)
		}
	}
	mustMap := func(id, app, gateway string, ctxSize int) {
		if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: app, GatewayModelName: gateway, AppModelName: gateway, Status: routing.ServerStatusActive, ContextSize: ctxSize, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	mustServer("srv_a", "GPU-A")
	mustApp("app_a", "srv_a", 8000)
	mustMap("m_a", "app_a", "qwen-coder", 8192)
	mustServer("srv_b", "GPU-B")
	mustApp("app_b", "srv_b", 8000)
	mustMap("m_b", "app_b", "qwen-coder", 4096)

	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	byID := modelsByID(got)
	if byID["qwen-coder"].ContextSize != 4096 {
		t.Fatalf("qwen-coder context_size = %d, want 4096 (min of 8192 and 4096)", byID["qwen-coder"].ContextSize)
	}
}

// TestServiceModelsContextSizeUnknownWhenNoMappingReportsOne: when every
// offering mapping's context_size is 0 (unknown), the DTO reports 0 rather
// than fabricating a value.
func TestServiceModelsContextSizeUnknownWhenNoMappingReportsOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	routeStore := routing.NewMemoryStore()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv_1", Name: "S1", Domain: "s1.test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app_1", ServerID: "srv_1", Type: routing.ProviderVLLM, Port: 8000, Scheme: "https", APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "map_1", ApplicationID: "app_1", GatewayModelName: "no-ctx", AppModelName: "no-ctx", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	svc := NewService(ServiceDeps{Usage: usage.NewRecorder(), Routes: routeStore, Clock: func() time.Time { return now }})
	got := svc.Models(ctx, auth.Token{UserID: "usr_1"})
	byID := modelsByID(got)
	if byID["no-ctx"].ContextSize != 0 {
		t.Fatalf("no-ctx context_size = %d, want 0 (unknown)", byID["no-ctx"].ContextSize)
	}
}

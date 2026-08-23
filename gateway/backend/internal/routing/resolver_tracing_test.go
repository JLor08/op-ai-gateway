// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing_test

import (
	"context"
	"io"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/tracing"
	"testing"
	"time"
)

// TestResolveEmitsSpan asserts routing.Resolver.Resolve brackets its work in a
// "routing.Resolve" span, mirroring provider.Multiplexer's per-method tracing
// (see multiplexer_tracing_test.go). It lives in the EXTERNAL routing_test
// package: routing (prod) opens the span via the OTel global provider and no
// longer imports internal/tracing (the generated routing.Store decorator lives
// in package tracing and imports routing), so this test may import tracing
// without an import cycle. The store mirrors resolver_test.go's
// seededResolverStore, seeding a routable "qwen-coder" model via exported API.
func TestResolveEmitsSpan(t *testing.T) {
	logs := logbuffer.NewBuffer(20, logbuffer.LevelTrace)
	prev := slog.Default()
	slog.SetDefault(slog.New(logs.Handler(io.Discard)))
	defer slog.SetDefault(prev)
	p, _ := tracing.Setup(tracing.Options{Enabled: true, SampleRatio: 1.0}, logs)
	defer p.Shutdown(context.Background())

	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	store := seededTracingResolverStore(t, now)
	r := routing.NewResolver(store, func() time.Time { return now }, nil)

	_, err := r.Resolve(context.Background(), auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	_ = err // a successful OR benign resolve is fine; we only assert the span was emitted

	var found bool
	for _, rec := range logs.Snapshot() {
		if rec.Msg == "span" && rec.Attrs["span"] == "routing.Resolve" {
			found = true
		}
	}
	if !found {
		t.Fatalf("routing.Resolve span not emitted: %+v", logs.Snapshot())
	}
}

// seededTracingResolverStore mirrors resolver_test.go's (unexported)
// seededResolverStore using only exported routing API, so it is reachable from
// this external test package.
func seededTracingResolverStore(t *testing.T, now time.Time) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := routing.NewMemoryStore()
	servers := []struct {
		serverID, appID, mappingID string
		latency                    int
	}{
		{"srv_fast", "app_fast", "map_fast", 100},
		{"srv_slow", "app_slow", "map_slow", 900},
	}
	for _, s := range servers {
		if err := store.CreateAIServer(ctx, routing.AIServer{ID: s.serverID, Name: s.serverID, Domain: s.serverID + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		if err := store.CreateApplication(ctx, routing.Application{ID: s.appID, ServerID: s.serverID, Type: routing.ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication: %v", err)
		}
		if err := store.CreateMapping(ctx, routing.ModelMapping{ID: s.mappingID, ApplicationID: s.appID, GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if err := store.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: s.serverID, ReportedAt: now, LatencyMS: s.latency, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertTelemetry: %v", err)
		}
	}
	return store
}

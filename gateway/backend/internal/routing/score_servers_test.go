// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"testing"
	"time"
)

// seededScoreServersStore builds two AI-servers, each with one active application/mapping
// offering the SAME gateway model "m1": srv_a (higher priority) and srv_b (lower priority,
// with a max_concurrency cap). Both have fresh, error-free telemetry so both pass the
// Score viability gate; only the fakeActivity in-flight count (installed by the caller)
// distinguishes availability.
func seededScoreServersStore(t *testing.T, now time.Time) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()

	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_a", Name: "srv_a", Domain: "srv-a.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer srv_a: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_a", ServerID: "srv_a", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 20, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication app_a: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_a", ApplicationID: "app_a", GatewayModelName: "m1", AppModelName: "m1-upstream", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping map_a: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_a", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_a: %v", err)
	}

	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_b", Name: "srv_b", Domain: "srv-b.test", Status: ServerStatusActive, HealthStatus: HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer srv_b: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_b", ServerID: "srv_b", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 5, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication app_b: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_b", ApplicationID: "app_b", GatewayModelName: "m1", AppModelName: "m1-upstream", Status: ServerStatusActive, MaxConcurrency: 2, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping map_b: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_b", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry srv_b: %v", err)
	}

	return store
}

func TestScoreModelServersRanksAndFlagsCapacity(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := seededScoreServersStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// srv_b is pinned at its max_concurrency (2 in-flight >= MaxConcurrency 2) => unavailable.
	// srv_a is lightly loaded and under no cap => available.
	resolver.SetServerActivityChecker(&fakeActivity{inFlight: map[string]int{"srv_a": 0, "srv_b": 2}}, 30*time.Second)

	scores, err := resolver.ScoreModelServers(context.Background(), "m1", now)
	if err != nil {
		t.Fatalf("ScoreModelServers returned error: %v", err)
	}
	if len(scores) != 2 {
		t.Fatalf("len(scores) = %d, want 2 (%+v)", len(scores), scores)
	}

	var a, b CandidateScore
	var foundA, foundB bool
	for _, cs := range scores {
		switch cs.MappingID {
		case "map_a":
			a, foundA = cs, true
		case "map_b":
			b, foundB = cs, true
		}
	}
	if !foundA || !foundB {
		t.Fatalf("expected both map_a and map_b in scores, got %+v", scores)
	}

	if a.ServerID != "srv_a" {
		t.Fatalf("a.ServerID = %q, want srv_a", a.ServerID)
	}
	if b.ServerID != "srv_b" {
		t.Fatalf("b.ServerID = %q, want srv_b", b.ServerID)
	}
	if !a.Available {
		t.Fatalf("a.Available = false, want true (under cap, low load)")
	}
	if b.Available {
		t.Fatalf("b.Available = true, want false (at max_concurrency)")
	}
	if !(a.Score > b.Score) {
		t.Fatalf("a.Score (%v) not > b.Score (%v); srv_a has higher priority and should rank first", a.Score, b.Score)
	}
}

func TestScoreModelServersUnknownModelReturnsEmpty(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	store := seededScoreServersStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	scores, err := resolver.ScoreModelServers(context.Background(), "no-such-model", now)
	if err != nil {
		t.Fatalf("ScoreModelServers returned error: %v", err)
	}
	if len(scores) != 0 {
		t.Fatalf("len(scores) = %d, want 0 for an unknown model", len(scores))
	}
}

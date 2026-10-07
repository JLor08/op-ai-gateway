// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// benchNoTextProvider streams a completion with no text and no usage: the shape
// of a benchmark pass that produced no output.
type benchNoTextProvider struct{}

func (benchNoTextProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (benchNoTextProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{}})
}

// keepLastSeedStore returns a MemoryStore holding benchTestTarget's server,
// application and mapping. The mapping carries 50 / 900 / 1234 (gen / prompt /
// load) with the provenance of an opportunistic sample at storedAt. The
// application has neither loaded_models_path nor context_probe_path, so a speed
// run confirms no cold start and writes no context size.
func keepLastSeedStore(t *testing.T, storedAt time.Time) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: storedAt, UpdatedAt: storedAt}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: storedAt, UpdatedAt: storedAt}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	at := storedAt
	if err := mem.CreateMapping(ctx, routing.ModelMapping{
		ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive,
		GenTokensPerSecond: 50, PromptTokensPerSecond: 900, LoadTimeMS: 1234,
		MetricsSource: "opportunistic", MetricsUpdatedAt: &at,
		CreatedAt: storedAt, UpdatedAt: storedAt,
	}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return mem
}

// runKeepLastSpeed runs one speed benchmark over benchTestTarget and returns
// the mapping as stored afterwards and the run's one history row.
func runKeepLastSpeed(t *testing.T, srv *Server, mem *routing.MemoryStore) (routing.ModelMapping, routing.BenchmarkRun) {
	t.Helper()
	ctx := context.Background()
	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, time.Now().UTC(), func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "speed")
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 0)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	return got, rows[0]
}

// TestSpeedRunKeepsStoredMetricsItCouldNotMeasure pins that a speed run writes
// onto the mapping only what it measured. A run without a confirmed cold start
// has no load time, a stream without prompt timings has no prompt rate, and a
// pass with no text has no generation rate. Each of them is 0 in the history
// row and keeps the mapping's stored value, and a run that measured nothing
// keeps the stored provenance as well.
func TestSpeedRunKeepsStoredMetricsItCouldNotMeasure(t *testing.T) {
	storedAt := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	t.Run("a run that measures only the generation rate keeps the load time and the prompt rate", func(t *testing.T) {
		mem := keepLastSeedStore(t, storedAt)
		fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 42}}
		before := time.Now().UTC()
		got, row := runKeepLastSpeed(t, &Server{Provider: fake, Routes: mem}, mem)

		if got.LoadTimeMS != 1234 || got.PromptTokensPerSecond != 900 || got.GenTokensPerSecond != 42 {
			t.Fatalf("stored load / prompt / gen = %d / %v / %v, want 1234 / 900 / 42",
				got.LoadTimeMS, got.PromptTokensPerSecond, got.GenTokensPerSecond)
		}
		if got.MetricsSource != "benchmark" {
			t.Fatalf("MetricsSource = %q, want benchmark (the run measured a generation rate)", got.MetricsSource)
		}
		if got.MetricsUpdatedAt == nil || got.MetricsUpdatedAt.Before(before) {
			t.Fatalf("MetricsUpdatedAt = %v, want the run's time (not before %v)", got.MetricsUpdatedAt, before)
		}
		if row.LoadTimeMS != 0 || row.PromptTokensPerSecond != 0 || row.GenTokensPerSecond != 42 || row.Error != "" {
			t.Fatalf("history load / prompt / gen / error = %d / %v / %v / %q, want 0 / 0 / 42 / \"\"",
				row.LoadTimeMS, row.PromptTokensPerSecond, row.GenTokensPerSecond, row.Error)
		}
	})

	t.Run("a pass with no text keeps every stored metric and its provenance", func(t *testing.T) {
		mem := keepLastSeedStore(t, storedAt)
		got, row := runKeepLastSpeed(t, &Server{Provider: benchNoTextProvider{}, Routes: mem}, mem)

		if got.LoadTimeMS != 1234 || got.PromptTokensPerSecond != 900 || got.GenTokensPerSecond != 50 {
			t.Fatalf("stored load / prompt / gen = %d / %v / %v, want 1234 / 900 / 50",
				got.LoadTimeMS, got.PromptTokensPerSecond, got.GenTokensPerSecond)
		}
		if got.MetricsSource != "opportunistic" {
			t.Fatalf("MetricsSource = %q, want opportunistic (a run that measured nothing writes no provenance)", got.MetricsSource)
		}
		if got.MetricsUpdatedAt == nil || !got.MetricsUpdatedAt.Equal(storedAt) {
			t.Fatalf("MetricsUpdatedAt = %v, want %v (a run that measured nothing writes no provenance)", got.MetricsUpdatedAt, storedAt)
		}
		if row.LoadTimeMS != 0 || row.PromptTokensPerSecond != 0 || row.GenTokensPerSecond != 0 || row.Error != "" {
			t.Fatalf("history load / prompt / gen / error = %d / %v / %v / %q, want 0 / 0 / 0 / \"\"",
				row.LoadTimeMS, row.PromptTokensPerSecond, row.GenTokensPerSecond, row.Error)
		}
	})
}

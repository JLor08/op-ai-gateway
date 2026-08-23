// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// warmerSeedStore seeds srv1/app1 (serving the openai flavor, with a loaded-models probe
// path) plus one active mapping: gateway name "warm-me" -> upstream "up-warm".
func warmerSeedStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, LoadedModelsPath: "/loaded", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "warm-me", AppModelName: "up-warm", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return mem
}

// waitWarmIdle waits until name's background warm has finished (in-flight cleared AND a
// cooldown stamp recorded), or fails after the deadline — which is what a HUNG warm
// would look like.
func waitWarmIdle(t *testing.T, w *modelWarmer, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		_, running := w.inflight[name]
		_, done := w.lastWarm[name]
		w.mu.Unlock()
		if !running && done {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("warm for %q did not finish within the deadline (hung?)", name)
}

// TestModelWarmerWarmsNotLoadedModel proves Warm loads a not-resident model exactly once
// (streaming the UPSTREAM name) and dedupes a second Warm within the cooldown.
func TestModelWarmerWarmsNotLoadedModel(t *testing.T) {
	fake := newColdLister(nil) // nothing loaded
	srv := &Server{Provider: fake, Routes: warmerSeedStore(t)}
	w := newModelWarmer(srv)

	w.Warm(context.Background(), "warm-me")
	waitWarmIdle(t, w, "warm-me")

	if streamed := fake.streamedModels(); len(streamed) != 1 || streamed[0] != "up-warm" {
		t.Fatalf("streamed = %v, want exactly one [up-warm]", streamed)
	}

	// A second Warm within the cooldown must NOT spawn a second load.
	w.Warm(context.Background(), "warm-me")
	time.Sleep(40 * time.Millisecond)
	if got := fake.streamedModels(); len(got) != 1 {
		t.Fatalf("after a deduped second Warm, streamed = %v, want still exactly 1", got)
	}
}

// TestModelWarmerSkipsLoadedModel proves an already-resident model is not warmed.
func TestModelWarmerSkipsLoadedModel(t *testing.T) {
	fake := newColdLister([]string{"up-warm"}) // already resident
	srv := &Server{Provider: fake, Routes: warmerSeedStore(t)}
	w := newModelWarmer(srv)

	w.Warm(context.Background(), "warm-me")
	waitWarmIdle(t, w, "warm-me")

	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("a resident model was warmed anyway: streamed = %v", got)
	}
}

// TestModelWarmerUnknownModelNoWarm proves an unknown model (no active mapping) triggers
// no load and doesn't panic.
func TestModelWarmerUnknownModelNoWarm(t *testing.T) {
	fake := newColdLister(nil)
	srv := &Server{Provider: fake, Routes: warmerSeedStore(t)}
	w := newModelWarmer(srv)

	w.Warm(context.Background(), "does-not-exist")
	waitWarmIdle(t, w, "does-not-exist")

	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("an unknown model triggered a stream: %v", got)
	}
}

// TestModelWarmerTimeoutBoundedWedgedProvider proves a stalled upstream can't hang the
// warm goroutine forever: benchHangingProvider blocks until ctx cancel, and the streamOnce
// idle watchdog (short here) tears it down. benchHangingProvider is not a LoadedModelLister,
// so the load stream runs directly.
func TestModelWarmerTimeoutBoundedWedgedProvider(t *testing.T) {
	srv := &Server{Provider: benchHangingProvider{}, Routes: warmerSeedStore(t)}
	srv.streamIdleTimeout = 40 * time.Millisecond
	w := newModelWarmer(srv)

	w.Warm(context.Background(), "warm-me")
	waitWarmIdle(t, w, "warm-me") // fails via the deadline if the warm hangs
}

// TestModelWarmerNilSafe proves a nil warmer (and a warmer with a nil Server) are no-ops.
func TestModelWarmerNilSafe(t *testing.T) {
	var w *modelWarmer
	w.Warm(context.Background(), "anything") // must not panic

	empty := &modelWarmer{} // srv == nil
	empty.Warm(context.Background(), "anything")
}

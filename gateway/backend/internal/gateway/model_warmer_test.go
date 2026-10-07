// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/routing"
	"slices"
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

// TestModelWarmerSkipsAResidentPortalShapedAgentModel: the warmer asks a
// server_agent application in the shape the portal stores through the agent
// router's /running, so a model that already runs is not warmed.
func TestModelWarmerSkipsAResidentPortalShapedAgentModel(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	f := newFakeAgentRouter(t, fakeAgentRouterOpts{running: []string{"qwen"}})
	tgt := portalShapedAgentTarget(f)
	server, app := tgt.server, tgt.app
	server.Provider, server.Endpoint, server.Status, server.HealthStatus = routing.ProviderMock, "mock://srv1", routing.ServerStatusActive, routing.HealthHealthy
	server.CreatedAt, server.UpdatedAt, app.CreatedAt, app.UpdatedAt = now, now, now, now
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: app.ID, GatewayModelName: "warm-agent", AppModelName: "qwen", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rs_map1", MappingID: "map1", Enabled: true, Binary: "/opt/bin/llama-server", Args: "[]", Env: "{}", APIFlavors: []string{routing.APIFlavorOpenAI}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertRuntimeSpec: %v", err)
	}
	w := newModelWarmer(&Server{Provider: newFakeAgentProvider(), Routes: mem})

	w.Warm(ctx, "warm-agent")
	waitWarmIdle(t, w, "warm-agent")

	if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
		t.Fatalf("requests = %v, want exactly [GET /running]: a resident model is not warmed", got)
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

// TestModelWarmerSkipsAnImagesOnlyAgentChild: the warm call is a chat prompt,
// so a mapping that serves only images must never be warmed. Looping over the
// text flavors keeps out an application that declares only openai_images, but
// candidacy judges a server_agent application by its own flavors, so an
// agent-launched sd-server child whose spec lists only openai_images still
// arrives as a candidate under a parent that also declares openai. The warmer
// skips it by its effective flavors, and warms a text sibling of the same name
// instead when there is one.
func TestModelWarmerSkipsAnImagesOnlyAgentChild(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	for i, srvID := range []string{"srv-sd", "srv-text"} {
		if err := mem.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + srvID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer: %v", err)
		}
		appID := "app-" + srvID
		if err := mem.CreateApplication(ctx, routing.Application{ID: appID, ServerID: srvID, Type: routing.ProviderServerAgent, Port: 8081 + i, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication: %v", err)
		}
	}
	child := func(id, appID, gateway, upstream string, flavors []string) {
		t.Helper()
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: appID, GatewayModelName: gateway, AppModelName: upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
		if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "spec-" + id, MappingID: id, Binary: "/opt/bin/server", Args: "[]", Env: "{}", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", id, err)
		}
	}
	// "flux" exists only as an images-only child. "shared" exists as an
	// images-only child (sorted first, map-a) and a text child (map-b).
	child("map-flux", "app-srv-sd", "flux", "up-flux", []string{routing.APIFlavorOpenAIImages})
	child("map-a-shared", "app-srv-sd", "shared", "up-shared-sd", []string{routing.APIFlavorOpenAIImages})
	child("map-b-shared", "app-srv-text", "shared", "up-shared-text", []string{routing.APIFlavorOpenAI})

	fake := newColdLister(nil)
	w := newModelWarmer(&Server{Provider: fake, Routes: mem})

	w.Warm(ctx, "flux")
	waitWarmIdle(t, w, "flux")
	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("streamed = %v, want no chat prompt to an images-only child", got)
	}

	w.Warm(ctx, "shared")
	waitWarmIdle(t, w, "shared")
	if got := fake.streamedModels(); len(got) != 1 || got[0] != "up-shared-text" {
		t.Fatalf("streamed = %v, want exactly [up-shared-text] (the text sibling)", got)
	}
}

// warmerTwoServerStore seeds two mock servers, srv-a and srv-b, each with one
// openai application. The gateway model "twin" has a mapping on each server:
// map-a -> "up-a" on srv-a, which ActiveMappingsForModel returns first, and
// map-b -> "up-b" on srv-b. The gateway model "solo" has a single mapping,
// map-c -> "up-solo" on srv-a.
func warmerTwoServerStore(t *testing.T) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	for i, srvID := range []string{"srv-a", "srv-b"} {
		if err := mem.CreateAIServer(ctx, routing.AIServer{ID: srvID, Name: srvID, Domain: srvID + ".example.test", Provider: routing.ProviderMock, Endpoint: "mock://" + srvID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer %s: %v", srvID, err)
		}
		if err := mem.CreateApplication(ctx, routing.Application{ID: "app-" + srvID, ServerID: srvID, Type: routing.ProviderMock, Port: 8100 + i, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication %s: %v", srvID, err)
		}
	}
	for _, m := range []struct{ id, appID, gateway, upstream string }{
		{"map-a", "app-srv-a", "twin", "up-a"},
		{"map-b", "app-srv-b", "twin", "up-b"},
		{"map-c", "app-srv-a", "solo", "up-solo"},
	} {
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: m.id, ApplicationID: m.appID, GatewayModelName: m.gateway, AppModelName: m.upstream, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", m.id, err)
		}
	}
	return mem
}

// TestModelWarmerSkipsAServerWithABenchmark: routing sends no request to a
// server a benchmark holds, so the warmer must not load a model there either.
// With the first candidate's server reserved, the warm goes to the candidate
// on the other server; with the only candidate's server reserved, nothing is
// streamed.
func TestModelWarmerSkipsAServerWithABenchmark(t *testing.T) {
	reserve := func(t *testing.T, serverID string) *BenchmarkRegistry {
		t.Helper()
		reg := NewBenchmarkRegistry()
		if _, ok := reg.TryStart(serverID, "server", "speed", 1, time.Now(), func() {}); !ok {
			t.Fatalf("TryStart(%s) refused", serverID)
		}
		return reg
	}

	t.Run("the candidate on the other server is warmed", func(t *testing.T) {
		mem := warmerTwoServerStore(t)
		cands, err := mem.ActiveMappingsForModel(context.Background(), "twin", routing.APIFlavorOpenAI)
		if err != nil || len(cands) != 2 || cands[0].Server.ID != "srv-a" {
			t.Fatalf("seed: candidates = %+v, err = %v; want two, srv-a first", cands, err)
		}
		fake := newColdLister(nil)
		w := newModelWarmer(&Server{Provider: fake, Routes: mem, Benchmarks: reserve(t, "srv-a")})

		w.Warm(context.Background(), "twin")
		waitWarmIdle(t, w, "twin")

		if got := fake.streamedModels(); len(got) != 1 || got[0] != "up-b" {
			t.Fatalf("streamed = %v, want exactly [up-b] (srv-a is reserved)", got)
		}
	})

	t.Run("a single candidate on a reserved server is not warmed", func(t *testing.T) {
		fake := newColdLister(nil)
		w := newModelWarmer(&Server{Provider: fake, Routes: warmerTwoServerStore(t), Benchmarks: reserve(t, "srv-a")})

		w.Warm(context.Background(), "solo")
		waitWarmIdle(t, w, "solo")

		if got := fake.streamedModels(); len(got) != 0 {
			t.Fatalf("streamed = %v, want nothing on a reserved server", got)
		}
	})
}

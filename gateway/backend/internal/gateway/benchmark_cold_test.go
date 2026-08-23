// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// coldLister is a streaming provider that ALSO implements provider.LoadedModelLister,
// backed by a mutable loaded set guarded by a mutex. It records the req.Model of every
// stream so the sibling-swap eviction can be asserted. With swapEvicts set, a stream of
// model M evicts every OTHER model and loads M (a single-slot swapper). *coldLister does
// NOT implement provider.ModelUnloader — use *coldUnloaderProvider for that.
type coldLister struct {
	mu           sync.Mutex
	loaded       map[string]bool
	streamed     []string
	firstDelayMS int
	calls        int
	usage        inference.Usage
	swapEvicts   bool
}

func newColdLister(loaded []string) *coldLister {
	m := map[string]bool{}
	for _, n := range loaded {
		m[n] = true
	}
	return &coldLister{loaded: m, usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 42, PromptPerSecond: 900}}
}

func (c *coldLister) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (c *coldLister) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.streamed = append(c.streamed, req.Model)
	if c.swapEvicts {
		if c.loaded == nil {
			c.loaded = map[string]bool{}
		}
		for m := range c.loaded {
			if m != req.Model {
				delete(c.loaded, m)
			}
		}
		c.loaded[req.Model] = true
	}
	delay := first && c.firstDelayMS > 0
	u := c.usage
	c.mu.Unlock()
	if delay {
		time.Sleep(time.Duration(c.firstDelayMS) * time.Millisecond)
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func (c *coldLister) LoadedModels(_ context.Context, _ routing.Target, _, _ string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.loaded))
	for m := range c.loaded {
		out = append(out, m)
	}
	return out, nil
}

func (c *coldLister) isLoaded(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loaded[model]
}

func (c *coldLister) streamedModels() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.streamed...)
}

// coldUnloaderProvider adds provider.ModelUnloader to a coldLister. UnloadModel records the
// call, returns unloadResult for `unloaded`, and (when unloadResult && unloadEvicts) removes
// the model from the loaded set so a subsequent probe confirms it gone.
type coldUnloaderProvider struct {
	*coldLister
	unloadResult bool
	unloadEvicts bool
	unloadCalls  []string
}

func (c *coldUnloaderProvider) UnloadModel(_ context.Context, _ routing.Target, model string) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unloadCalls = append(c.unloadCalls, model)
	if c.unloadResult && c.unloadEvicts {
		delete(c.loaded, model)
	}
	return c.unloadResult, nil
}

func (c *coldUnloaderProvider) unloadCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.unloadCalls)
}

// shortColdBounds shrinks the cold-load poll bounds so a verify-timeout resolves fast, and
// restores them when the test finishes.
func shortColdBounds(t *testing.T, gap, maxWait time.Duration) {
	t.Helper()
	prevGap, prevMax := coldLoadPollGap, coldLoadMaxWait
	coldLoadPollGap, coldLoadMaxWait = gap, maxWait
	t.Cleanup(func() { coldLoadPollGap, coldLoadMaxWait = prevGap, prevMax })
}

// coldTestTarget mirrors benchTestTarget but with a loaded-models probe path configured (so
// ensureColdLoad has a way to observe loaded-state).
func coldTestTarget() benchmarkTarget {
	tgt := benchTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"
	return tgt
}

// coldSeedStore seeds srv1/app1 plus the given active mappings (id -> appModelName) so the
// sibling-swap path can find another active mapping on the same application.
func coldSeedStore(t *testing.T, mappings map[string]string) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, LoadedModelsPath: "/loaded", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for id, up := range mappings {
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app1", GatewayModelName: "gw-" + id, AppModelName: up, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	return mem
}

// Case 1: already loaded + explicit unload that evicts => ensureColdLoad confirms cold.
func TestEnsureColdLoadUnloadEvicts(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 2*time.Second)
	fake := &coldUnloaderProvider{coldLister: newColdLister([]string{"up-model"}), unloadResult: true, unloadEvicts: true}
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model"})}
	if !srv.ensureColdLoad(context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = false, want true (unload evicted the model)")
	}
	if fake.unloadCallCount() != 1 {
		t.Fatalf("unload calls = %d, want 1", fake.unloadCallCount())
	}
	if fake.isLoaded("up-model") {
		t.Fatalf("up-model still loaded after a successful unload")
	}
}

// Case 2: already loaded, no unloader, sibling swap evicts the target => confirms cold.
func TestEnsureColdLoadSiblingSwap(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 2*time.Second)
	fake := newColdLister([]string{"up-model"})
	fake.swapEvicts = true // streaming a sibling evicts up-model and loads the sibling
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model", "map2": "up-model-2"})}
	if !srv.ensureColdLoad(context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = false, want true (sibling swap evicted the model)")
	}
	streamed := fake.streamedModels()
	if len(streamed) != 1 || streamed[0] != "up-model-2" {
		t.Fatalf("streamed = %v, want exactly [up-model-2] (the sibling swap stream)", streamed)
	}
	if fake.isLoaded("up-model") {
		t.Fatalf("up-model still loaded after the sibling swap")
	}
}

// Case 3: not loaded initially => confirms cold immediately, no unload/stream.
func TestEnsureColdLoadAlreadyCold(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 2*time.Second)
	fake := &coldUnloaderProvider{coldLister: newColdLister(nil), unloadResult: true, unloadEvicts: true}
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model", "map2": "up-model-2"})}
	if !srv.ensureColdLoad(context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = false, want true (model was never loaded)")
	}
	if fake.unloadCallCount() != 0 {
		t.Fatalf("unload calls = %d, want 0 (nothing to evict)", fake.unloadCallCount())
	}
	if len(fake.streamedModels()) != 0 {
		t.Fatalf("streamed = %v, want none (no sibling swap needed)", fake.streamedModels())
	}
}

// Case 4: no loaded-probe configured => ensureColdLoad false, and measureMapping records
// NO load time (unknown) even when the cold pass is slower — but throughput is still set.
// This is the core anti-bogus guard.
func TestMeasureMappingNoProbeSkipsLoadTime(t *testing.T) {
	fake := newColdLister(nil)
	fake.firstDelayMS = 40 // cold pass slower than warm — would produce a bogus load time if not gated
	srv := &Server{Provider: fake}
	tgt := benchTestTarget() // app.LoadedModelsPath == "" (no probe)

	if srv.ensureColdLoad(context.Background(), tgt) {
		t.Fatalf("ensureColdLoad = true, want false (no loaded-probe configured)")
	}
	res, err := srv.measureMapping(context.Background(), tgt)
	if err != nil {
		t.Fatalf("measureMapping err = %v", err)
	}
	if res.LoadTimeMS != 0 {
		t.Fatalf("LoadTimeMS = %d, want 0 (cold unconfirmed => unknown, never a bogus value)", res.LoadTimeMS)
	}
	if res.GenTokensPerSecond <= 0 {
		t.Fatalf("GenTokensPerSecond = %v, want > 0 (throughput measured regardless of cold-confirmation)", res.GenTokensPerSecond)
	}
}

// Case 5: loaded, unloader refuses (false), no sibling mapping => cannot confirm cold.
func TestEnsureColdLoadEvictionUnavailable(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 2*time.Second)
	fake := &coldUnloaderProvider{coldLister: newColdLister([]string{"up-model"}), unloadResult: false}
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model"})}
	if srv.ensureColdLoad(context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = true, want false (unload refused, no sibling to swap)")
	}
}

// Case 6: loaded, unload returns true but never evicts, no sibling => verify-timeout => false.
func TestEnsureColdLoadVerifyTimeout(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 0) // maxWait 0 => waitModelUnloaded returns false immediately
	fake := &coldUnloaderProvider{coldLister: newColdLister([]string{"up-model"}), unloadResult: true, unloadEvicts: false}
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model"})}
	if srv.ensureColdLoad(context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = true, want false (model never left the loaded set => verify timed out)")
	}
	if fake.unloadCallCount() != 1 {
		t.Fatalf("unload calls = %d, want 1", fake.unloadCallCount())
	}
}

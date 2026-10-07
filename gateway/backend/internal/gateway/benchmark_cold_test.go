// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"slices"
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
	mu       sync.Mutex
	loaded   map[string]bool
	streamed []string
	// streamedTargets records the routing.Target of every stream, alongside
	// streamed's req.Model and under the same mutex, so a test can assert
	// what the target BUILDER put on it (the live-progress decision inputs)
	// and not only which model was asked for.
	streamedTargets []routing.Target
	firstDelayMS    int
	calls           int
	usage           inference.Usage
	swapEvicts      bool
	// loadedCalls counts LoadedModels calls, so a test can assert that the cold pass did not
	// wait on an eviction that never happened.
	loadedCalls int
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

func (c *coldLister) CompleteStream(_ context.Context, target routing.Target, req inference.Request, emit provider.StreamEmit) error {
	c.mu.Lock()
	c.calls++
	first := c.calls == 1
	c.streamed = append(c.streamed, req.Model)
	c.streamedTargets = append(c.streamedTargets, target)
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
	c.loadedCalls++
	out := make([]string, 0, len(c.loaded))
	for m := range c.loaded {
		out = append(out, m)
	}
	return out, nil
}

func (c *coldLister) loadedCallCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadedCalls
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

// streamedTargetList is streamedModels' sibling for the recorded Targets.
func (c *coldLister) streamedTargetList() []routing.Target {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]routing.Target(nil), c.streamedTargets...)
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

// coldStartConfirmed runs srv.ensureColdLoad for tgt, fails the test on an error (no case here
// writes a stop, so none may fail), and reports whether the cold start was confirmed.
func coldStartConfirmed(t *testing.T, srv *Server, ctx context.Context, tgt benchmarkTarget) bool {
	t.Helper()
	cs, err := srv.ensureColdLoad(ctx, tgt)
	if err != nil {
		t.Fatalf("ensureColdLoad err = %v, want nil", err)
	}
	return cs.confirmed
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
	if !coldStartConfirmed(t, srv, context.Background(), coldTestTarget()) {
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
	if !coldStartConfirmed(t, srv, context.Background(), coldTestTarget()) {
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
	if !coldStartConfirmed(t, srv, context.Background(), coldTestTarget()) {
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

	if coldStartConfirmed(t, srv, context.Background(), tgt) {
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
	if coldStartConfirmed(t, srv, context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = true, want false (unload refused, no sibling to swap)")
	}
	if n := fake.loadedCallCount(); n != 1 {
		t.Fatalf("loaded-models probes = %d, want 1: a refused unload is not waited on, and there is no sibling to swap to", n)
	}
}

// Case 6: loaded, unload returns true but never evicts, no sibling => verify-timeout => false.
func TestEnsureColdLoadVerifyTimeout(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 0) // maxWait 0 => waitModelUnloaded returns false immediately
	fake := &coldUnloaderProvider{coldLister: newColdLister([]string{"up-model"}), unloadResult: true, unloadEvicts: false}
	srv := &Server{Provider: fake, Routes: coldSeedStore(t, map[string]string{"map1": "up-model"})}
	if coldStartConfirmed(t, srv, context.Background(), coldTestTarget()) {
		t.Fatalf("ensureColdLoad = true, want false (model never left the loaded set => verify timed out)")
	}
	if fake.unloadCallCount() != 1 {
		t.Fatalf("unload calls = %d, want 1", fake.unloadCallCount())
	}
}

// coldSeedAgentStore seeds srv1 with a server_agent application app1 in the shape the portal
// stores (loaded_models_path empty), the given active mappings (id -> appModelName), and a
// runtime spec with flavors for each mapping in specs.
func coldSeedAgentStore(t *testing.T, mappings map[string]string, specs map[string][]string) (*routing.MemoryStore, routing.Application) {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	app := routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderServerAgent, Port: 8100, Scheme: "http", TimeoutMS: 30000, APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorOpenAIImages}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := mem.CreateApplication(ctx, app); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	for id, up := range mappings {
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: id, ApplicationID: "app1", GatewayModelName: "gw-" + id, AppModelName: up, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping %s: %v", id, err)
		}
	}
	for id, flavors := range specs {
		if err := mem.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{ID: "rs_" + id, MappingID: id, Enabled: true, Binary: "/opt/bin/server", Args: "[]", Env: "{}", HealthPath: "/health", APIFlavors: flavors, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("UpsertRuntimeSpec %s: %v", id, err)
		}
	}
	return mem, app
}

// TestBenchmarkSiblingModelSkipsAnImagesOnlySibling: the sibling swap sends its sibling a chat
// prompt, so benchmarkSiblingModel passes over a sibling that serves only images and one whose
// spec cannot be read, and picks the first text sibling after them. With no such sibling it
// picks none.
func TestBenchmarkSiblingModelSkipsAnImagesOnlySibling(t *testing.T) {
	images := []string{routing.APIFlavorOpenAIImages}
	text := []string{routing.APIFlavorOpenAI}
	t.Run("a text sibling after them", func(t *testing.T) {
		mem, app := coldSeedAgentStore(t,
			map[string]string{"map1": "up-model", "map2": "sd-model", "map3": "unread-model", "map4": "up-model-4"},
			map[string][]string{"map1": text, "map2": images, "map3": text, "map4": text})
		specs := &refusalSpecStore{Store: mem, fail: map[string]bool{"map3": true}, reads: map[string]int{}}
		srv := &Server{Routes: specs}
		tgt := coldTestTarget()
		tgt.app = app
		sib, ok := srv.benchmarkSiblingModel(context.Background(), tgt)
		if !ok || sib != "up-model-4" {
			t.Fatalf("benchmarkSiblingModel = %q, %v; want up-model-4, true: not the images-only or the unreadable sibling", sib, ok)
		}
	})
	t.Run("only an images-only sibling", func(t *testing.T) {
		mem, app := coldSeedAgentStore(t,
			map[string]string{"map1": "up-model", "map2": "sd-model"},
			map[string][]string{"map1": text, "map2": images})
		srv := &Server{Routes: mem}
		tgt := coldTestTarget()
		tgt.app = app
		if sib, ok := srv.benchmarkSiblingModel(context.Background(), tgt); ok {
			t.Fatalf("benchmarkSiblingModel = %q, true; want none: no sibling can take the swap", sib)
		}
	})
}

// TestEnsureColdLoadAgentTargetNeverSwapsOrUnloads: the cold pass never evicts a resident
// target of a server_agent application, not even through a provider that could unload it or
// swap it out through a text sibling. It cannot confirm a cold start then. The agent's runtime
// status reads every spec stopped, so the loaded set alone refuses it.
func TestEnsureColdLoadAgentTargetNeverSwapsOrUnloads(t *testing.T) {
	shortColdBounds(t, time.Millisecond, 50*time.Millisecond)
	text := []string{routing.APIFlavorOpenAI}
	mem, app := coldSeedAgentStore(t,
		map[string]string{"map1": "up-model", "map2": "up-model-2"},
		map[string][]string{"map1": text, "map2": text})
	fake := &coldUnloaderProvider{coldLister: newColdLister([]string{"up-model"}), unloadResult: true, unloadEvicts: true}
	fake.swapEvicts = true
	srv := &Server{Provider: fake, Routes: mem, RuntimeStatus: NewRuntimeStatusRegistry()}
	srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{
		{SpecID: "rs_map1", Model: "up-model", State: "stopped"},
		{SpecID: "rs_map2", Model: "up-model-2", State: "stopped"},
	})
	tgt := coldTestTarget()
	tgt.app = app
	if coldStartConfirmed(t, srv, context.Background(), tgt) {
		t.Fatal("ensureColdLoad = true, want false: a resident agent target is never evicted, so its cold start stays unconfirmed")
	}
	if n := fake.unloadCallCount(); n != 0 {
		t.Fatalf("unload calls = %d, want 0", n)
	}
	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("streamed = %v, want none: no sibling swap", got)
	}
	if !fake.isLoaded("up-model") {
		t.Fatal("up-model is no longer loaded, want it left resident")
	}
}

// TestEnsureColdLoadPortalShapedAgentTarget: the cold pass asks a server_agent application in
// the shape the portal stores through the agent router's /running, sends it nothing else, and
// confirms a cold start only for a target it knows is not resident, on a server where every
// other model of the application reads stopped in a non-empty runtime status.
func TestEnsureColdLoadPortalShapedAgentTarget(t *testing.T) {
	ctx := context.Background()
	newSrv := func() *Server {
		return &Server{Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry()}
	}
	t.Run("cold at the start", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		if !coldStartConfirmed(t, srv, ctx, portalShapedAgentTarget(f)) {
			t.Fatal("ensureColdLoad = false, want true: /running does not list qwen, and the agent reports its spec stopped")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
			t.Fatalf("requests = %v, want exactly [GET /running]: no unload, no sibling stream", got)
		}
	})
	t.Run("an empty status snapshot cannot confirm a cold start", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		if coldStartConfirmed(t, newSrv(), ctx, portalShapedAgentTarget(f)) {
			t.Fatal("ensureColdLoad = true, want false: no runtime status for the server is no evidence that nothing else runs")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
			t.Fatalf("requests = %v, want exactly [GET /running]", got)
		}
	})
	t.Run("an API-set loaded_models_path is the one asked", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		tgt := portalShapedAgentTarget(f)
		tgt.app.LoadedModelsPath = "/custom-running"
		tgt.app.LoadedModelsFormat = "llama_swap"
		if coldStartConfirmed(t, srv, ctx, tgt) {
			t.Fatal("ensureColdLoad = true, want false: the router answers /custom-running with 404, so residency is unknown")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /custom-running"}) {
			t.Fatalf("requests = %v, want exactly [GET /custom-running]", got)
		}
	})
	t.Run("a spec the agent reports starting is resident", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "starting"}})
		if coldStartConfirmed(t, srv, ctx, portalShapedAgentTarget(f)) {
			t.Fatal("ensureColdLoad = true, want false: /running does not list a starting child, but it is loading")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
			t.Fatalf("requests = %v, want exactly [GET /running]", got)
		}
	})
	t.Run("a spec the agent reports in backoff cannot confirm a cold start", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "backoff"}})
		if coldStartConfirmed(t, srv, ctx, portalShapedAgentTarget(f)) {
			t.Fatal("ensureColdLoad = true, want false: a request for a spec in backoff waits for its backoff timer, and that wait would land in the load time")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
			t.Fatalf("requests = %v, want exactly [GET /running]", got)
		}
	})
	t.Run("a failed /running leaves residency unknown", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		f.failRunning(true)
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		if coldStartConfirmed(t, srv, ctx, portalShapedAgentTarget(f)) {
			t.Fatal("ensureColdLoad = true, want false: an unanswered /running is not a cold start")
		}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
			t.Fatalf("requests = %v, want exactly [GET /running]", got)
		}
	})
	t.Run("a target without a spec is cold only on an empty server", func(t *testing.T) {
		for _, tc := range []struct {
			state string
			want  bool
		}{{"running", false}, {"stopped", true}} {
			f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
			srv := newSrv()
			srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{Model: "qwen", State: tc.state}})
			tgt := portalShapedAgentTarget(f)
			tgt.spec = routing.RuntimeSpec{}
			if got := coldStartConfirmed(t, srv, ctx, tgt); got != tc.want {
				t.Fatalf("a row without a spec id in %s: ensureColdLoad = %v, want %v: a target without a spec has no row of its own, so every row is another model's", tc.state, got, tc.want)
			}
			if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
				t.Fatalf("a row without a spec id in %s: requests = %v, want exactly [GET /running]", tc.state, got)
			}
		}
	})
	t.Run("a running row of another spec cannot confirm a cold start", func(t *testing.T) {
		other := func(state string, pid int) RuntimeStatusDTO {
			return RuntimeStatusDTO{SpecID: "rs_other", Model: "llama", State: state, PID: pid}
		}
		for _, tc := range []struct {
			name  string
			other RuntimeStatusDTO
			want  bool
		}{
			{"running", other("running", 4242), false},
			{"stopped", other("stopped", 0), true},
			{"backoff", other("backoff", 0), false},
			{"start_failed with a process", other("start_failed", 4242), false},
			{"stopped with a process id", other("stopped", 4242), false},
			{"a state this gateway does not know", other("hibernating", 0), false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
				srv := newSrv()
				srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}, tc.other})
				if got := coldStartConfirmed(t, srv, ctx, portalShapedAgentTarget(f)); got != tc.want {
					t.Fatalf("rs_other %s with pid %d: ensureColdLoad = %v, want %v: only a neighbour that reads stopped with pid 0 neither runs nor starts by itself", tc.other.State, tc.other.PID, got, tc.want)
				}
				if got := fakeAgentPaths(f.requests()); !slices.Equal(got, []string{"GET /running"}) {
					t.Fatalf("requests = %v, want exactly [GET /running]", got)
				}
			})
		}
	})
	t.Run("a blank app model name asks nothing", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
		tgt := portalShapedAgentTarget(f)
		tgt.mapping.AppModelName = " "
		srv := newSrv()
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		if coldStartConfirmed(t, srv, ctx, tgt) {
			t.Fatal("ensureColdLoad = true, want false: there is no model to ask about")
		}
		if got := f.requests(); len(got) != 0 {
			t.Fatalf("requests = %v, want none", fakeAgentPaths(got))
		}
	})
}

// TestMeasureMappingTimesTheLoadOfAColdAgentTarget: a speed pass over a server_agent application
// in the shape the portal stores records a load time when its model is cold at the start on a
// server where every other model reads stopped, none when another model runs, and none when it
// is resident, which the cold pass leaves running.
func TestMeasureMappingTimesTheLoadOfAColdAgentTarget(t *testing.T) {
	ctx := context.Background()
	t.Run("cold at the start", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}, coldDelay: 300 * time.Millisecond})
		srv := &Server{Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry()}
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		res, err := srv.measureMapping(ctx, portalShapedAgentTarget(f))
		if err != nil {
			t.Fatalf("measureMapping err = %v", err)
		}
		if res.LoadTimeMS < 250 {
			t.Fatalf("LoadTimeMS = %d, want >= 250: the cold pass waited for a 300 ms load", res.LoadTimeMS)
		}
		if res.GenTokensPerSecond != 42 || res.PromptTokensPerSecond != 900 {
			t.Fatalf("rates = %v gen / %v prompt, want 42 / 900 from the final chunk's timings", res.GenTokensPerSecond, res.PromptTokensPerSecond)
		}
		want := []string{"GET /running", "POST /v1/chat/completions", "POST /v1/chat/completions"}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
	t.Run("cold next to a running neighbour", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}, coldDelay: 300 * time.Millisecond})
		srv := &Server{Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry()}
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{
			{SpecID: "rs_map1", Model: "qwen", State: "stopped"},
			{SpecID: "rs_other", Model: "llama", State: "running", PID: 4242},
		})
		res, err := srv.measureMapping(ctx, portalShapedAgentTarget(f))
		if err != nil {
			t.Fatalf("measureMapping err = %v", err)
		}
		if res.LoadTimeMS != 0 {
			t.Fatalf("LoadTimeMS = %d, want 0: another model of the application runs, so the cold start is not one on an otherwise empty server", res.LoadTimeMS)
		}
		want := []string{"GET /running", "POST /v1/chat/completions", "POST /v1/chat/completions"}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})
	t.Run("resident at the start", func(t *testing.T) {
		f := newFakeAgentRouter(t, fakeAgentRouterOpts{running: []string{"qwen"}})
		srv := &Server{Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry()}
		srv.RuntimeStatus.publish("srv1", []RuntimeStatusDTO{{SpecID: "rs_map1", Model: "qwen", State: "stopped"}})
		res, err := srv.measureMapping(ctx, portalShapedAgentTarget(f))
		if err != nil {
			t.Fatalf("measureMapping err = %v", err)
		}
		if res.LoadTimeMS != 0 {
			t.Fatalf("LoadTimeMS = %d, want 0: a resident agent target is never evicted, so no cold start is confirmed", res.LoadTimeMS)
		}
		want := []string{"GET /running", "POST /v1/chat/completions", "POST /v1/chat/completions"}
		if got := fakeAgentPaths(f.requests()); !slices.Equal(got, want) {
			t.Fatalf("requests = %v, want %v: no unload", got, want)
		}
	})
}

// TestColdProbeTargetFloorsTheTimeout: the cold pass's loaded-state reads and its unload are
// bounded by the probe target's Timeout, so an application without a positive timeout_ms gets
// coldLoadCallTimeout instead of an unbounded call, and a positive one keeps its own.
func TestColdProbeTargetFloorsTheTimeout(t *testing.T) {
	srv := &Server{}
	tgt := benchTestTarget()
	tgt.app.TimeoutMS = 0
	if _, pt := srv.coldProbeTarget(context.Background(), tgt); pt.Timeout != coldLoadCallTimeout {
		t.Fatalf("Timeout = %v, want coldLoadCallTimeout %v for timeout_ms 0", pt.Timeout, coldLoadCallTimeout)
	}
	tgt.app.TimeoutMS = 1500
	if _, pt := srv.coldProbeTarget(context.Background(), tgt); pt.Timeout != 1500*time.Millisecond {
		t.Fatalf("Timeout = %v, want the application's 1.5s", pt.Timeout)
	}
}

// TestAgentTargetResidentReadsTheSpecRow: once /running has not listed the model,
// agentTargetResident reads the agent's status row of the target's own spec. A state with a live
// process, or any process id, is resident. Any other state than stopped cannot confirm a cold
// start, because a request for such a spec waits (backoff) or is refused (not_permitted,
// pending_vram_unknown). A stopped spec, one the agent has not reported yet, and a row of
// another spec leave the target cold.
func TestAgentTargetResidentReadsTheSpecRow(t *testing.T) {
	row := func(specID, state string, pid int) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: specID, Model: "qwen", State: state, PID: pid}
	}
	cases := []struct {
		name                    string
		rows                    []RuntimeStatusDTO
		wantResident, wantKnown bool
	}{
		{"running", []RuntimeStatusDTO{row("rs_map1", "running", 0)}, true, true},
		{"starting", []RuntimeStatusDTO{row("rs_map1", "starting", 0)}, true, true},
		{"draining", []RuntimeStatusDTO{row("rs_map1", "draining", 0)}, true, true},
		{"start_failed with a live process", []RuntimeStatusDTO{row("rs_map1", "start_failed", 4242)}, true, true},
		{"backoff", []RuntimeStatusDTO{row("rs_map1", "backoff", 0)}, false, false},
		{"not_permitted", []RuntimeStatusDTO{row("rs_map1", "not_permitted", 0)}, false, false},
		{"pending_vram_unknown", []RuntimeStatusDTO{row("rs_map1", "pending_vram_unknown", 0)}, false, false},
		{"a state this gateway does not know", []RuntimeStatusDTO{row("rs_map1", "hibernating", 0)}, false, false},
		{"stopped", []RuntimeStatusDTO{row("rs_map1", "stopped", 0)}, false, true},
		{"no row for the spec yet", nil, false, true},
		{"a running row of another spec with the same model", []RuntimeStatusDTO{row("rs_other", "running", 0)}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAgentRouter(t, fakeAgentRouterOpts{models: []string{"qwen"}})
			srv := &Server{Provider: newFakeAgentProvider(), RuntimeStatus: NewRuntimeStatusRegistry()}
			srv.RuntimeStatus.publish("srv1", tc.rows)
			resident, known := srv.agentTargetResident(context.Background(), portalShapedAgentTarget(f))
			if resident != tc.wantResident || known != tc.wantKnown {
				t.Fatalf("agentTargetResident = (%v, %v), want (%v, %v)", resident, known, tc.wantResident, tc.wantKnown)
			}
		})
	}
}

// TestBenchmarkOthersStopped: a run that stops nothing confirms a cold start only when every row
// of a non-empty status snapshot reads stopped with pid 0, except the target's own row, which
// agentSpecResidency has already read. A target without a spec has no row of its own, so every
// row counts, a row without a spec id included.
func TestBenchmarkOthersStopped(t *testing.T) {
	row := func(specID, state string, pid int) RuntimeStatusDTO {
		return RuntimeStatusDTO{SpecID: specID, State: state, PID: pid}
	}
	cases := []struct {
		name   string
		rows   []RuntimeStatusDTO
		specID string
		want   bool
	}{
		{"no snapshot", nil, "rs_map1", false},
		{"an empty snapshot", []RuntimeStatusDTO{}, "rs_map1", false},
		{"only the target's own row", []RuntimeStatusDTO{row("rs_map1", "stopped", 0)}, "rs_map1", true},
		{"the target's own row is not read here", []RuntimeStatusDTO{row("rs_map1", "running", 4242), row("rs_other", "stopped", 0)}, "rs_map1", true},
		{"every other row stopped", []RuntimeStatusDTO{row("rs_map1", "stopped", 0), row("rs_a", "stopped", 0), row("rs_b", "stopped", 0)}, "rs_map1", true},
		{"one other row running", []RuntimeStatusDTO{row("rs_map1", "stopped", 0), row("rs_a", "stopped", 0), row("rs_b", "running", 4242)}, "rs_map1", false},
		{"another row starting", []RuntimeStatusDTO{row("rs_other", "starting", 0)}, "rs_map1", false},
		{"another row draining", []RuntimeStatusDTO{row("rs_other", "draining", 4242)}, "rs_map1", false},
		{"another row in backoff", []RuntimeStatusDTO{row("rs_other", "backoff", 0)}, "rs_map1", false},
		{"another row not_permitted", []RuntimeStatusDTO{row("rs_other", "not_permitted", 0)}, "rs_map1", false},
		{"another row pending_vram_unknown", []RuntimeStatusDTO{row("rs_other", "pending_vram_unknown", 0)}, "rs_map1", false},
		{"another row start_failed with a process", []RuntimeStatusDTO{row("rs_other", "start_failed", 4242)}, "rs_map1", false},
		{"another row stopped with a process id", []RuntimeStatusDTO{row("rs_other", "stopped", 4242)}, "rs_map1", false},
		{"another row in a state this gateway does not know", []RuntimeStatusDTO{row("rs_other", "hibernating", 0)}, "rs_map1", false},
		{"no spec: a row without a spec id running", []RuntimeStatusDTO{row("", "running", 4242)}, "", false},
		{"no spec: a row without a spec id stopped", []RuntimeStatusDTO{row("", "stopped", 0)}, "", true},
		{"no spec: every row counts", []RuntimeStatusDTO{row("rs_map1", "stopped", 0), row("rs_other", "backoff", 0)}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := benchmarkOthersStopped(tc.rows, tc.specID); got != tc.want {
				t.Fatalf("benchmarkOthersStopped(%+v, %q) = %v, want %v", tc.rows, tc.specID, got, tc.want)
			}
		})
	}
}

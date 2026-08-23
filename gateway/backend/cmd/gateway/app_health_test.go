// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"errors"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// fakeHealthStore is an in-memory healthStore + settings source for the loop tests.
type fakeHealthStore struct {
	servers  []routing.AIServer
	apps     map[string][]routing.Application
	settings map[string]string

	mu              sync.Mutex
	health          map[string]string
	mappings        map[string][]routing.ModelMapping  // keyed by application id
	ctxProbeSets    int                                // count of UpdateMappingContextProbe calls
	availSamplesLog []routing.ServerAvailabilitySample // append-ordered availability samples
	failInsert      bool                               // when true, InsertServerAvailabilitySample errors
}

func (f *fakeHealthStore) AIServers(context.Context) ([]routing.AIServer, error) {
	return f.servers, nil
}

func (f *fakeHealthStore) ApplicationsByServer(_ context.Context, serverID string) ([]routing.Application, error) {
	return f.apps[serverID], nil
}

func (f *fakeHealthStore) SetServerHealth(_ context.Context, serverID, health string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.health == nil {
		f.health = map[string]string{}
	}
	f.health[serverID] = health
	return nil
}

// MappingsByApplication returns a copy of the seeded mappings for an application.
func (f *fakeHealthStore) MappingsByApplication(_ context.Context, applicationID string) ([]routing.ModelMapping, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routing.ModelMapping(nil), f.mappings[applicationID]...), nil
}

// UpdateMappingContextProbe mirrors the store contract: it stamps context_size +
// provenance ("probe") only while the mapping is unlocked; a missing or locked
// mapping is a benign no-op.
func (f *fakeHealthStore) UpdateMappingContextProbe(_ context.Context, id string, contextSize int, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ctxProbeSets++
	for appID, list := range f.mappings {
		for i := range list {
			if list[i].ID != id || list[i].MetricsLocked {
				continue
			}
			list[i].ContextSize = contextSize
			list[i].MetricsSource = "probe"
			t := at
			list[i].MetricsUpdatedAt = &t
			f.mappings[appID] = list
			return nil
		}
	}
	return nil
}

// ctxProbeSetCount returns how many times UpdateMappingContextProbe was called.
func (f *fakeHealthStore) ctxProbeSetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ctxProbeSets
}

// InsertServerAvailabilitySample records an availability sample so the sampling
// tests can assert what the health loop wrote. When failInsert is set it returns
// an error WITHOUT recording, exercising the loop's best-effort retry invariant.
func (f *fakeHealthStore) InsertServerAvailabilitySample(_ context.Context, sample routing.ServerAvailabilitySample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failInsert {
		return fmt.Errorf("insert availability sample: boom")
	}
	f.availSamplesLog = append(f.availSamplesLog, sample)
	return nil
}

// setFailInsert toggles the InsertServerAvailabilitySample failure (test helper).
func (f *fakeHealthStore) setFailInsert(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failInsert = v
}

// availSamples returns a copy of the recorded availability samples (test helper).
func (f *fakeHealthStore) availSamples() []routing.ServerAvailabilitySample {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]routing.ServerAvailabilitySample(nil), f.availSamplesLog...)
}

// mappingOf returns the current stored mapping by id (test helper).
func (f *fakeHealthStore) mappingOf(id string) (routing.ModelMapping, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, list := range f.mappings {
		for _, mp := range list {
			if mp.ID == id {
				return mp, true
			}
		}
	}
	return routing.ModelMapping{}, false
}

func (f *fakeHealthStore) SystemSettings(context.Context) (map[string]string, error) {
	return f.settings, nil
}

// AIServerByID lets the fake satisfy the extended healthStore (scoped pass lookup).
func (f *fakeHealthStore) AIServerByID(_ context.Context, id string) (routing.AIServer, error) {
	for _, s := range f.servers {
		if s.ID == id {
			return s, nil
		}
	}
	return routing.AIServer{}, fmt.Errorf("not found: %s", id)
}

func (f *fakeHealthStore) healthOf(serverID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.health[serverID]
}

// fakeProber returns nil for reachable endpoints and an error for endpoints
// marked down; it counts calls per endpoint so the retry path is observable.
type fakeProber struct {
	mu    sync.Mutex
	calls map[string]int
	down  map[string]bool
	// loaded maps an endpoint to the models the loaded-model probe should report;
	// loadedErr marks endpoints whose loaded-model probe should fail.
	loaded    map[string][]string
	loadedErr map[string]bool
	// modelInfo maps an endpoint to the model info the context probe should report;
	// modelInfoErr marks endpoints whose context probe should fail.
	modelInfo    map[string][]provider.ModelInfo
	modelInfoErr map[string]bool
	// modelInfoByPath maps a probe PATH to the model info to return (used by the
	// {model}-template context probe); when set for a path it wins over modelInfo.
	// modelInfoPaths records every probe path ProbeModelInfo was called with.
	modelInfoByPath map[string][]provider.ModelInfo
	modelInfoPaths  []string
}

var (
	_ provider.Prober            = (*fakeProber)(nil)
	_ provider.LoadedModelLister = (*fakeProber)(nil)
	_ provider.ModelInfoProber   = (*fakeProber)(nil)
)

func newFakeProber() *fakeProber {
	return &fakeProber{
		calls: map[string]int{}, down: map[string]bool{},
		loaded: map[string][]string{}, loadedErr: map[string]bool{},
		modelInfo: map[string][]provider.ModelInfo{}, modelInfoErr: map[string]bool{},
		modelInfoByPath: map[string][]provider.ModelInfo{},
	}
}

// ProbeModelInfo satisfies provider.ModelInfoProber so the context-probe pass in
// runAppHealthOnce can query this fake. It records every probe path and, when a
// per-path entry is registered, returns it (so the {model}-template pass can be
// observed); otherwise it falls back to the per-endpoint model info.
func (f *fakeProber) ProbeModelInfo(_ context.Context, target routing.Target, probePath string) ([]provider.ModelInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.modelInfoPaths = append(f.modelInfoPaths, probePath)
	if f.modelInfoErr[target.Endpoint] {
		return nil, fmt.Errorf("model-info probe failed: %s", target.Endpoint)
	}
	if infos, ok := f.modelInfoByPath[probePath]; ok {
		return infos, nil
	}
	return f.modelInfo[target.Endpoint], nil
}

// probedPath reports whether ProbeModelInfo was ever called with the given path.
func (f *fakeProber) probedPath(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.modelInfoPaths {
		if p == path {
			return true
		}
	}
	return false
}

// ctxProbeCallCount returns how many times ProbeModelInfo was called.
func (f *fakeProber) ctxProbeCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.modelInfoPaths)
}

// LoadedModels satisfies provider.LoadedModelLister so the loaded-model probe
// pass in runAppHealthOnce can query this fake.
func (f *fakeProber) LoadedModels(_ context.Context, target routing.Target, _ string, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.loadedErr[target.Endpoint] {
		return nil, fmt.Errorf("loaded probe failed: %s", target.Endpoint)
	}
	return f.loaded[target.Endpoint], nil
}

func (f *fakeProber) Probe(_ context.Context, target routing.Target, _ string) error {
	f.mu.Lock()
	f.calls[target.Endpoint]++
	fail := f.down[target.Endpoint]
	f.mu.Unlock()
	if fail {
		return fmt.Errorf("unreachable: %s", target.Endpoint)
	}
	return nil
}

func (f *fakeProber) callCount(endpoint string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[endpoint]
}

// fakeModelSyncer records SyncApplicationModelsForApp calls per application and
// can be told to fail (as if the upstream model listing errored), backing the
// model_sync branch of the loop.
type fakeModelSyncer struct {
	mu sync.Mutex
	// calls counts reconcile attempts per application.
	calls map[string]int
	// fail marks an application whose UPSTREAM listing fails
	// (portal.ErrApplicationSyncFailed) -> unreachable.
	fail map[string]bool
	// localFail marks an application whose upstream answered but the local
	// reconcile hit a store error (any non-sync-failed error) -> reachable.
	localFail map[string]bool
	// agentPresenceDefault, when > 0, is returned by
	// ActiveAgentPresenceTimeoutSeconds (a test-controllable stand-in for the
	// real *portal.Service's env-aware effective agent-presence-timeout
	// default); <= 0 (the zero value included) falls back to
	// portal.DefaultAgentPresenceTimeoutSeconds, mirroring a fresh deployment
	// that never overrode OP_AI_GATEWAY_AGENT_PRESENCE_TIMEOUT_SECONDS nor saved
	// a System Settings value.
	agentPresenceDefault int
}

var _ modelSyncer = (*fakeModelSyncer)(nil)

func newFakeModelSyncer() *fakeModelSyncer {
	return &fakeModelSyncer{calls: map[string]int{}, fail: map[string]bool{}, localFail: map[string]bool{}}
}

// ActiveAgentPresenceTimeoutSeconds stands in for (*portal.Service)'s
// env-aware effective agent-presence-timeout default.
func (f *fakeModelSyncer) ActiveAgentPresenceTimeoutSeconds(context.Context) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.agentPresenceDefault > 0 {
		return f.agentPresenceDefault
	}
	return portal.DefaultAgentPresenceTimeoutSeconds
}

func (f *fakeModelSyncer) SyncApplicationModelsForApp(_ context.Context, _ routing.AIServer, app routing.Application) (portal.SyncResultDTO, error) {
	f.mu.Lock()
	f.calls[app.ID]++
	fail := f.fail[app.ID]
	localFail := f.localFail[app.ID]
	f.mu.Unlock()
	if fail {
		return portal.SyncResultDTO{}, portal.ErrApplicationSyncFailed
	}
	if localFail {
		return portal.SyncResultDTO{}, fmt.Errorf("store write failed")
	}
	return portal.SyncResultDTO{}, nil
}

func (f *fakeModelSyncer) callCount(appID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[appID]
}

func activeApp(id, serverID string, port int) routing.Application {
	return routing.Application{
		ID: id, ServerID: serverID, Type: routing.ProviderVLLM, Port: port, Scheme: "http",
		Status: routing.ServerStatusActive, HealthCheckPath: "/v1/health",
	}
}

func newHealthTestStore(apps ...routing.Application) *fakeHealthStore {
	server := routing.AIServer{ID: "s1", Domain: "s1.local", Provider: routing.ProviderVLLM, Status: routing.ServerStatusActive}
	return &fakeHealthStore{
		servers: []routing.AIServer{server},
		apps:    map[string][]routing.Application{"s1": apps},
	}
}

func shrinkRetryGap(t *testing.T) {
	t.Helper()
	orig := appHealthRetryGap
	appHealthRetryGap = time.Millisecond
	t.Cleanup(func() { appHealthRetryGap = orig })
}

func TestRunAppHealthOnceAllReachableHealthy(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001), activeApp("a2", "s1", 8002))
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
	if !reg.Reachable("a1") || !reg.Reachable("a2") {
		t.Fatalf("apps not marked reachable")
	}
}

func TestRunAppHealthOnceRecordsLoadedModels(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.LoadedModelsPath = "/running"
	app.LoadedModelsFormat = "llama_swap"
	st := newHealthTestStore(app)
	prober := newFakeProber()
	prober.loaded["http://s1.local:8001"] = []string{"m1", "m2"}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got := loaded.LoadedAppModels("a1", "s1")
	sort.Strings(got)
	if !reflect.DeepEqual(got, []string{"m1", "m2"}) {
		t.Fatalf("loaded models = %v, want [m1 m2]", got)
	}
}

func TestRunAppHealthOnceLoadedProbeSkippedWithoutPath(t *testing.T) {
	shrinkRetryGap(t)
	// No LoadedModelsPath -> no loaded probe, registry stays empty.
	st := newHealthTestStore(activeApp("a1", "s1", 8001))
	prober := newFakeProber()
	prober.loaded["http://s1.local:8001"] = []string{"should-not-appear"}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := loaded.LoadedAppModels("a1", "s1"); got != nil {
		t.Fatalf("loaded models = %v, want nil (no status path configured)", got)
	}
}

func TestRunAppHealthOnceLoadedProbeErrorClears(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.LoadedModelsPath = "/running"
	st := newHealthTestStore(app)
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	// Seed a stale value, then make the probe fail: the failed probe must clear it.
	loaded.SetGatewayProbe("a1", []string{"stale"})
	prober.loadedErr["http://s1.local:8001"] = true

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := loaded.LoadedAppModels("a1", "s1"); got != nil {
		t.Fatalf("loaded models = %v, want nil (probe failed -> cleared)", got)
	}
}

func ctxProbeApp(id, serverID string, port int) routing.Application {
	app := activeApp(id, serverID, port)
	app.ContextProbePath = "/props"
	return app
}

func TestRunAppHealthOncePersistsContextProbe(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, ok := st.mappingOf("m1")
	if !ok {
		t.Fatalf("mapping m1 missing")
	}
	if got.ContextSize != 131072 {
		t.Fatalf("ContextSize = %d, want 131072", got.ContextSize)
	}
	if got.MetricsSource != "probe" {
		t.Fatalf("MetricsSource = %q, want %q", got.MetricsSource, "probe")
	}
}

func TestRunAppHealthOnceContextProbeSkippedWithoutPath(t *testing.T) {
	shrinkRetryGap(t)
	// No ContextProbePath -> no context probe, mapping stays at 0.
	st := newHealthTestStore(activeApp("a1", "s1", 8001))
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.ContextSize != 0 || got.MetricsSource != "" {
		t.Fatalf("mapping changed without a probe path: ContextSize=%d Source=%q", got.ContextSize, got.MetricsSource)
	}
}

func TestRunAppHealthOnceContextProbeRespectsLock(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
			Status: routing.ServerStatusActive, ContextSize: 4096, MetricsLocked: true, MetricsSource: "manual",
		}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.ContextSize != 4096 {
		t.Fatalf("locked ContextSize = %d, want 4096 (probe must not overwrite a lock)", got.ContextSize)
	}
	if got.MetricsSource != "manual" {
		t.Fatalf("locked MetricsSource = %q, want %q", got.MetricsSource, "manual")
	}
}

func TestRunAppHealthOnceContextProbeSkipsUnchangedValue(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	// The stored context ALREADY equals the probe value, with empty provenance:
	// no write must happen (else metrics_source/updated_at would churn each cycle).
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
			Status: routing.ServerStatusActive, ContextSize: 131072, MetricsSource: "", MetricsUpdatedAt: nil,
		}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.ctxProbeSetCount(); n != 0 {
		t.Fatalf("UpdateMappingContextProbe called %d times for an unchanged value, want 0", n)
	}
	got, _ := st.mappingOf("m1")
	if got.MetricsSource != "" {
		t.Fatalf("MetricsSource = %q, want %q (unchanged value must not churn provenance)", got.MetricsSource, "")
	}
	if got.MetricsUpdatedAt != nil {
		t.Fatalf("MetricsUpdatedAt = %v, want nil (unchanged value must not churn provenance)", got.MetricsUpdatedAt)
	}
}

func TestRunAppHealthOnceContextProbeIgnoresAbsurdValue(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	// 2e8 exceeds maxProbedContextSize (1e8) -> ignored, mapping untouched.
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 200_000_000}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.ContextSize != 0 || got.MetricsSource != "" {
		t.Fatalf("absurd context size was persisted: ContextSize=%d Source=%q", got.ContextSize, got.MetricsSource)
	}
}

// ctxTemplateApp builds an active app whose context-probe path carries the {model}
// placeholder, so the per-model, loaded-gated context-probe branch is exercised.
func ctxTemplateApp(id, serverID string, port int) routing.Application {
	app := activeApp(id, serverID, port)
	app.ContextProbePath = "/upstream/{model}/props"
	return app
}

func TestRunAppHealthOnceContextProbeTemplateProbesLoadedOnly(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	prober.modelInfoByPath["/upstream/m-b/props"] = []provider.ModelInfo{{Name: "m-b", ContextSize: 16384}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"}) // only m-a is loaded

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/m-a/props") {
		t.Fatalf("expected the loaded model m-a to be probed at /upstream/m-a/props")
	}
	if prober.probedPath("/upstream/m-b/props") {
		t.Fatalf("m-b is not loaded and must NOT be probed")
	}
	gotA, _ := st.mappingOf("m1")
	if gotA.ContextSize != 8192 {
		t.Fatalf("m-a ContextSize = %d, want 8192 (persisted from the per-model probe)", gotA.ContextSize)
	}
	if gotA.MetricsSource != "probe" {
		t.Fatalf("m-a MetricsSource = %q, want %q", gotA.MetricsSource, "probe")
	}
	gotB, _ := st.mappingOf("m2")
	if gotB.ContextSize != 0 {
		t.Fatalf("m-b ContextSize = %d, want 0 (not loaded -> never probed)", gotB.ContextSize)
	}
}

func TestRunAppHealthOnceContextProbeTemplateEmptyLoadedSetProbesNothing(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry() // empty: nothing known-loaded

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := prober.ctxProbeCallCount(); n != 0 {
		t.Fatalf("ProbeModelInfo called %d times with an empty loaded set, want 0", n)
	}
	if n := st.ctxProbeSetCount(); n != 0 {
		t.Fatalf("UpdateMappingContextProbe called %d times with an empty loaded set, want 0", n)
	}
	got, _ := st.mappingOf("m1")
	if got.ContextSize != 0 {
		t.Fatalf("ContextSize = %d, want 0 (nothing loaded -> no write)", got.ContextSize)
	}
}

func TestRunAppHealthOnceContextProbeTemplateAttributesDivergentReportedName(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	// The /props for m-a reports a DIFFERENT name (a path basename) — the per-model
	// probe must still attribute the value to m-a (name-match caveat sidestepped).
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "some-basename", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.ContextSize != 8192 {
		t.Fatalf("m-a ContextSize = %d, want 8192 (attributed despite a divergent reported name)", got.ContextSize)
	}
	if got.MetricsSource != "probe" {
		t.Fatalf("m-a MetricsSource = %q, want %q", got.MetricsSource, "probe")
	}
}

func TestRunAppHealthOnceContextProbeNoTemplateUsesSingleProbeNameMatch(t *testing.T) {
	shrinkRetryGap(t)
	// A path WITHOUT {model} takes the single-probe + name-match path unchanged: one
	// probe with the LITERAL path, attributed by reported Name (no loaded gating).
	app := ctxProbeApp("a1", "s1", 8001) // "/props"
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)

	// loaded is nil (as in production for this path); the single-probe branch never consults it.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/props") {
		t.Fatalf("expected the literal /props path to be probed once")
	}
	if n := prober.ctxProbeCallCount(); n != 1 {
		t.Fatalf("ProbeModelInfo called %d times, want 1 (single probe, no {model} expansion)", n)
	}
	gotA, _ := st.mappingOf("m1")
	if gotA.ContextSize != 8192 {
		t.Fatalf("m-a ContextSize = %d, want 8192 (name-matched)", gotA.ContextSize)
	}
	gotB, _ := st.mappingOf("m2")
	if gotB.ContextSize != 0 {
		t.Fatalf("m-b ContextSize = %d, want 0 (reported name did not match)", gotB.ContextSize)
	}
}

func TestRunAppHealthOncePartialDegraded(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001), activeApp("a2", "s1", 8002))
	prober := newFakeProber()
	prober.down["http://s1.local:8002"] = true
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := st.healthOf("s1"); got != routing.HealthDegraded {
		t.Fatalf("server health = %q, want %q", got, routing.HealthDegraded)
	}
	if !reg.Reachable("a1") {
		t.Fatalf("a1 should be reachable")
	}
	if reg.Reachable("a2") {
		t.Fatalf("a2 should be unreachable")
	}
}

func TestRunAppHealthOnceNoneUnhealthy(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001), activeApp("a2", "s1", 8002))
	prober := newFakeProber()
	prober.down["http://s1.local:8001"] = true
	prober.down["http://s1.local:8002"] = true
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthUnhealthy)
	}
}

func TestRunAppHealthOnceZeroActiveUnhealthy(t *testing.T) {
	shrinkRetryGap(t)
	disabled := activeApp("a1", "s1", 8001)
	disabled.Status = routing.ServerStatusDisabled
	st := newHealthTestStore(disabled)
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("server health = %q, want %q (zero active apps)", got, routing.HealthUnhealthy)
	}
	if n := prober.callCount("http://s1.local:8001"); n != 0 {
		t.Fatalf("prober called %d times for a disabled app, want 0", n)
	}
}

func TestRunAppHealthOnceRetriesBeforeUnreachable(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001))
	prober := newFakeProber()
	prober.down["http://s1.local:8001"] = true // both attempts fail
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if reg.Reachable("a1") {
		t.Fatalf("a1 should be unreachable after both attempts failed")
	}
	if n := prober.callCount("http://s1.local:8001"); n != 2 {
		t.Fatalf("prober called %d times, want 2 (initial + one retry)", n)
	}
	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthUnhealthy)
	}
}

func TestRunAppHealthOnceAlwaysReachableSkipsProbe(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.AlwaysReachable = true
	st := newHealthTestStore(app)
	prober := newFakeProber()
	prober.down["http://s1.local:8001"] = true // would fail if probed
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := prober.callCount("http://s1.local:8001"); n != 0 {
		t.Fatalf("prober called %d times for an always_reachable app, want 0", n)
	}
	if !reg.Reachable("a1") {
		t.Fatalf("always_reachable app must be reachable")
	}
	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
}

func TestRunAppHealthOnceModelSyncReachableReconciles(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.HealthCheckMode = routing.HealthCheckModeModelSync
	st := newHealthTestStore(app)
	prober := newFakeProber()
	prober.down["http://s1.local:8001"] = true // would fail if the loop probed instead of syncing
	syncer := newFakeModelSyncer()
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: syncer, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !reg.Reachable("a1") {
		t.Fatalf("model_sync app must be reachable when the listing/reconcile succeeds")
	}
	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
	if n := syncer.callCount("a1"); n != 1 {
		t.Fatalf("syncer called %d times, want 1 (one reconcile per successful cycle)", n)
	}
	if n := prober.callCount("http://s1.local:8001"); n != 0 {
		t.Fatalf("prober called %d times in model_sync mode, want 0", n)
	}
}

func TestRunAppHealthOnceModelSyncFailureRetriesThenUnreachable(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.HealthCheckMode = routing.HealthCheckModeModelSync
	st := newHealthTestStore(app)
	prober := newFakeProber()
	syncer := newFakeModelSyncer()
	syncer.fail["a1"] = true // both attempts fail (upstream listing error)
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: syncer, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if reg.Reachable("a1") {
		t.Fatalf("a1 should be unreachable after both model-sync attempts failed")
	}
	if n := syncer.callCount("a1"); n != 2 {
		t.Fatalf("syncer called %d times, want 2 (initial + one retry)", n)
	}
	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthUnhealthy)
	}
}

func TestRunAppHealthOnceModelSyncLocalErrorStaysReachable(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.HealthCheckMode = routing.HealthCheckModeModelSync
	st := newHealthTestStore(app)
	prober := newFakeProber()
	syncer := newFakeModelSyncer()
	syncer.localFail["a1"] = true // upstream answered, but a local store write failed
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: syncer, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	// A local reconcile/persistence error must NOT take a healthy upstream out
	// of routing — reachability tracks the upstream listing only.
	if !reg.Reachable("a1") {
		t.Fatalf("a1 marked unreachable on a local reconcile error; want reachable")
	}
	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
	// No retry for a local error (only upstream failures retry).
	if n := syncer.callCount("a1"); n != 1 {
		t.Fatalf("syncer called %d times, want 1 (local error does not retry)", n)
	}
}

func TestRunAppHealthOnceReturnsFinestCadence(t *testing.T) {
	shrinkRetryGap(t)

	t.Run("custom app interval narrows the wake cadence", func(t *testing.T) {
		app := activeApp("a1", "s1", 8001)
		app.HealthCheckIntervalSeconds = 5 // custom, finer than the 30s system default
		st := newHealthTestStore(app)      // nil settings -> system default (30s)
		prober := newFakeProber()
		reg := gateway.NewAppHealthRegistry(nil)

		got := (&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
		if got != 5*time.Second {
			t.Fatalf("wake interval = %v, want 5s (finest custom cadence)", got)
		}
	})

	t.Run("all-default apps follow the system interval", func(t *testing.T) {
		st := newHealthTestStore(activeApp("a1", "s1", 8001)) // interval 0 -> follow system
		st.settings = map[string]string{"health_check_interval_seconds": "20"}
		prober := newFakeProber()
		reg := gateway.NewAppHealthRegistry(nil)

		got := (&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
		if got != 20*time.Second {
			t.Fatalf("wake interval = %v, want 20s (system interval)", got)
		}
	})
}

func TestRunAppHealthOnceSkipsNotDueApp(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001)) // system default 30s cadence
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	// A FIXED clock so no time elapses between the two cycles.
	fixed := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }
	lastProbed := map[string]time.Time{}

	// First cycle probes the app once and records it reachable.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: make(map[string]availWriteState)})
	if n := prober.callCount("http://s1.local:8001"); n != 1 {
		t.Fatalf("prober called %d times on the first cycle, want 1", n)
	}
	if !reg.Reachable("a1") {
		t.Fatalf("a1 should be reachable after the first probe")
	}

	// Second cycle at the SAME clock: the 30s cadence has not elapsed, so the app
	// is not due and must NOT be probed again; reachability is retained.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: make(map[string]availWriteState)})
	if n := prober.callCount("http://s1.local:8001"); n != 1 {
		t.Fatalf("prober called %d times after a not-due cycle, want 1", n)
	}
	if !reg.Reachable("a1") {
		t.Fatalf("a1 reachability not retained across a not-due cycle")
	}
}

func TestRunAppHealthOnceProbesDueAppAfterIntervalElapses(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001)) // system default 30s cadence
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	base := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	current := base
	clock := func() time.Time { return current }
	lastProbed := map[string]time.Time{}

	// First cycle probes at t=base.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: make(map[string]availWriteState)})
	if n := prober.callCount("http://s1.local:8001"); n != 1 {
		t.Fatalf("prober called %d times on the first cycle, want 1", n)
	}

	// Advance past the 30s cadence: the app is now due and is probed again.
	current = base.Add(31 * time.Second)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: make(map[string]availWriteState)})
	if n := prober.callCount("http://s1.local:8001"); n != 2 {
		t.Fatalf("prober called %d times after the cadence elapsed, want 2 (app due)", n)
	}
}

// TestRunAppHealthOnceWritesAvailabilitySample proves the event-sourced sampling:
// a sample is written on the first observation and on every state change, deduped
// between heartbeats, and a heartbeat forces a periodic sample even when unchanged.
func TestRunAppHealthOnceWritesAvailabilitySample(t *testing.T) {
	shrinkRetryGap(t)
	// Shorten the heartbeat so the advance-the-clock case stays fast; restore after.
	origHB := availabilityHeartbeat
	availabilityHeartbeat = 100 * time.Millisecond
	defer func() { availabilityHeartbeat = origHB }()

	st := newHealthTestStore(activeApp("a1", "s1", 8001)) // reachable app on s1
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	presence := gateway.NewAgentPresenceRegistry(0)
	presence.Report("s1") // s1's agent is reporting

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	current := base
	clock := func() time.Time { return current }
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}

	// 1) First cycle: healthy + agent reporting -> exactly one sample capturing it.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	samples := st.availSamples()
	if len(samples) != 1 {
		t.Fatalf("availability samples after the first cycle = %d, want 1", len(samples))
	}
	if s0 := samples[0]; s0.Health != routing.HealthHealthy || !s0.AgentReporting || s0.ActiveCount != 1 || s0.ReachableCount != 1 {
		t.Fatalf("first sample = %+v, want Health=healthy AgentReporting=true ActiveCount=1 ReachableCount=1", s0)
	}

	// 2) Second cycle, same clock + unchanged state -> no new sample (not due, no change).
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 1 {
		t.Fatalf("availability samples after an unchanged cycle = %d, want 1 (deduped)", n)
	}

	// 3) Advance past the heartbeat with the same state -> one periodic heartbeat sample.
	current = base.Add(200 * time.Millisecond)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 2 {
		t.Fatalf("availability samples after the heartbeat = %d, want 2", n)
	}

	// 4) Agent stops reporting (state transition) BEFORE the next heartbeat is due ->
	// a new sample purely because the state changed. Evict s1 from the presence
	// registry so Reporting("s1") flips to false without waiting out the window.
	presence.Retain(map[string]struct{}{})
	current = base.Add(210 * time.Millisecond) // < heartbeat since the 200ms write
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	samples = st.availSamples()
	if len(samples) != 3 {
		t.Fatalf("availability samples after the presence transition = %d, want 3", len(samples))
	}
	if last := samples[2]; last.AgentReporting {
		t.Fatalf("transition sample AgentReporting = true, want false (agent stopped reporting)")
	}
}

// TestRunAppHealthOnceWritesNetbirdConnectedTransition proves NetBird connectivity
// rides the event-sourced availability sample: the first observation captures the
// live NetbirdConnected, and a connected->disconnected flip (health + agent held
// constant) writes a new sample purely because the NetBird dimension changed.
// Mutation guard: dropping netbirdConnected from the `changed` predicate makes the
// second cycle a no-op (no state change, heartbeat not due) -> only 1 sample -> fail;
// dropping it from the sample literal makes samples[0].NetbirdConnected false -> fail.
func TestRunAppHealthOnceWritesNetbirdConnectedTransition(t *testing.T) {
	shrinkRetryGap(t)
	st := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s1", Domain: "s1.local", Provider: routing.ProviderVLLM, Status: routing.ServerStatusActive, NetbirdPeerID: "peer-1", NetbirdConnected: true}},
		apps:    map[string][]routing.Application{"s1": {activeApp("a1", "s1", 8001)}},
	}
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	presence := gateway.NewAgentPresenceRegistry(0)

	fixed := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}

	// 1) First cycle: peer connected -> one sample capturing NetbirdConnected=true.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	samples := st.availSamples()
	if len(samples) != 1 {
		t.Fatalf("availability samples after the first cycle = %d, want 1", len(samples))
	}
	if !samples[0].NetbirdConnected {
		t.Fatalf("first sample NetbirdConnected = false, want true")
	}

	// 2) Peer disconnects (state transition), SAME clock + SAME health/agent -> a new
	// sample purely because the NetBird dimension changed.
	st.servers[0].NetbirdConnected = false
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	samples = st.availSamples()
	if len(samples) != 2 {
		t.Fatalf("availability samples after the NetBird transition = %d, want 2", len(samples))
	}
	if samples[1].NetbirdConnected {
		t.Fatalf("transition sample NetbirdConnected = true, want false (peer disconnected)")
	}
}

// TestRunAppHealthOnceAvailabilitySampleBestEffortRetry pins the best-effort
// invariant: a FAILED InsertServerAvailabilitySample must NOT advance lastAvail,
// so the next cycle re-attempts the write for the same state (not deduped away).
// (Mutation guard: moving the lastAvail update out of the success `else` — i.e.
// advancing even on an insert error — makes the retry cycle dedup instead of
// write, so the final assertion fails.)
func TestRunAppHealthOnceAvailabilitySampleBestEffortRetry(t *testing.T) {
	shrinkRetryGap(t)
	st := newHealthTestStore(activeApp("a1", "s1", 8001)) // reachable app on s1
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	presence := gateway.NewAgentPresenceRegistry(0)
	presence.Report("s1")

	fixed := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return fixed }
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}

	// 1) Insert fails: the loop attempts the write but records nothing AND must not
	// advance lastAvail (so the state is still "unseen" for the next cycle).
	st.setFailInsert(true)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 0 {
		t.Fatalf("availability samples after a failed insert = %d, want 0 (nothing recorded)", n)
	}
	if _, seen := lastAvail["s1"]; seen {
		t.Fatalf("lastAvail advanced on a failed insert; want no entry (best-effort retry)")
	}

	// 2) Insert now succeeds, SAME clock + SAME state: because the failed write did
	// not advance lastAvail, the state is still unseen -> a sample IS written (the
	// failed write was retried, not deduped away).
	st.setFailInsert(false)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 1 {
		t.Fatalf("availability samples after the retry = %d, want 1 (failed write retried, not deduped)", n)
	}
}

// TestRunAppHealthOnceAvailabilityHeartbeatInclusiveBoundary pins the `>=`
// boundary of heartbeatDue: a cycle exactly availabilityHeartbeat after the last
// write (state unchanged) MUST produce a sample. (Mutation guard: `>=`->`>` makes
// the exact-boundary cycle skip the write, so the second-sample assertion fails.)
func TestRunAppHealthOnceAvailabilityHeartbeatInclusiveBoundary(t *testing.T) {
	shrinkRetryGap(t)
	origHB := availabilityHeartbeat
	availabilityHeartbeat = 100 * time.Millisecond
	defer func() { availabilityHeartbeat = origHB }()

	st := newHealthTestStore(activeApp("a1", "s1", 8001))
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	presence := gateway.NewAgentPresenceRegistry(0)
	presence.Report("s1")

	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	current := base
	clock := func() time.Time { return current }
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}

	// 1) First cycle writes the initial sample (lastAvail.at == base).
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 1 {
		t.Fatalf("availability samples after the first cycle = %d, want 1", n)
	}

	// 2) Advance EXACTLY availabilityHeartbeat since the last write, state unchanged
	// -> the heartbeat is due at the inclusive boundary (tNow.Sub(prev.at) == hb).
	current = base.Add(availabilityHeartbeat)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presence, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 2 {
		t.Fatalf("availability samples at the exact heartbeat boundary = %d, want 2 (>= is inclusive)", n)
	}
}

// TestRunAppHealthOnceAvailabilityUsesEffectivePerServerAgentWindow proves the
// availability sample's AgentReporting is computed from the EFFECTIVE
// per-server agent-presence window (routing.EffectiveAgentPresenceTimeoutSeconds),
// not the AgentPresenceRegistry's own fixed window, AND that the system-wide
// default fed into that computation actually FLOWS from
// syncer.ActiveAgentPresenceTimeoutSeconds (the loop's live *portal.Service
// path — the same env-aware value the "Agent" status column uses), not a
// hardcoded fallback.
//
// Two independent sub-cases, each with its OWN AgentPresenceRegistry, report,
// and real sleep (real elapsed time is unavoidable here: the registry's
// internal clock is only test-injectable from within the gateway package
// itself — see agent_presence_test.go — not from this package). Each
// sub-case's registry must stay private to it: runAppHealthOnce's
// end-of-cycle agentPresence.Retain(liveServers) evicts any server not in
// THAT cycle's own store, so a registry shared across two single-server
// stores would have the second server evicted before its own cycle ran.
//
//  1. s1 has a custom, small AgentPresenceTimeoutSeconds=3 override, and the
//     fake syncer reports a LARGE system default (3600s): the per-server
//     override must still win (AgentReporting=false), proving a per-server
//     override tightens even a large syncer-sourced default.
//  2. s2 has NO override, so it must follow the fake syncer's default
//     directly. The fake now reports a SMALL default (2s), and the sleep is
//     comfortably longer than 2s but well under the hardcoded fallback
//     (15s): AgentReporting must be false. (Mutation guard: if
//     appHealthCycleConfig ignored syncer and fell back to the hardcoded
//     portal.DefaultAgentPresenceTimeoutSeconds [15s] instead, this report's
//     age would still be under 15s, so AgentReporting would read true and
//     this case fails — proving the syncer's value genuinely flows through.
//     Reverting the ReportingWithin rewire to the old fixed-window
//     agentPresence.Reporting(server.ID) call — 180s here since
//     NewAgentPresenceRegistry(0) defaults to defaultAgentPresenceWindow —
//     makes BOTH cases read AgentReporting=true, failing case 1 too.)
func TestRunAppHealthOnceAvailabilityUsesEffectivePerServerAgentWindow(t *testing.T) {
	shrinkRetryGap(t)

	// Case 1: a LARGE fake system default; s1's own small 3s override must win.
	presence1 := gateway.NewAgentPresenceRegistry(0)
	presence1.Report("s1")
	time.Sleep(3200 * time.Millisecond) // just past s1's 3s effective window

	fixed1 := time.Now().UTC()
	st1 := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s1", Domain: "s1.local", Provider: routing.ProviderVLLM, Status: routing.ServerStatusActive, AgentPresenceTimeoutSeconds: 3}},
		apps:    map[string][]routing.Application{"s1": {activeApp("a1", "s1", 8001)}},
	}
	syncer1 := newFakeModelSyncer()
	syncer1.agentPresenceDefault = 3600
	(&appHealthRunner{store: st1, prober: newFakeProber(), syncer: syncer1, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: presence1, groups: nil, settings: st1, probeTimeout: time.Second, cipher: nil, now: func() time.Time { return fixed1 }}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
	if s := st1.availSamples(); len(s) != 1 {
		t.Fatalf("s1 availability samples = %d, want 1", len(s))
	} else if s[0].AgentReporting {
		t.Fatalf("s1 (override=3, fake default=3600) AgentReporting = true, want false (the per-server 3s window must be honored, not the syncer's 3600s default)")
	}

	// Case 2: no per-server override on s2, so it follows the fake's default
	// directly; a SMALL fake default (2s) must also read false after a ~3.2s-old
	// report — proving that value actually flows from
	// syncer.ActiveAgentPresenceTimeoutSeconds, since the hardcoded 15s
	// fallback would have (wrongly) read true for the same age.
	presence2 := gateway.NewAgentPresenceRegistry(0)
	presence2.Report("s2")
	time.Sleep(3200 * time.Millisecond) // past the fake's 2s default, well under the 15s hardcoded fallback

	fixed2 := time.Now().UTC()
	st2 := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s2", Domain: "s2.local", Provider: routing.ProviderVLLM, Status: routing.ServerStatusActive}},
		apps:    map[string][]routing.Application{"s2": {activeApp("a2", "s2", 8002)}},
	}
	syncer2 := newFakeModelSyncer()
	syncer2.agentPresenceDefault = 2
	(&appHealthRunner{store: st2, prober: newFakeProber(), syncer: syncer2, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: presence2, groups: nil, settings: st2, probeTimeout: time.Second, cipher: nil, now: func() time.Time { return fixed2 }}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
	if s := st2.availSamples(); len(s) != 1 {
		t.Fatalf("s2 availability samples = %d, want 1", len(s))
	} else if s[0].AgentReporting {
		t.Fatalf("s2 (no override, fake default=2) AgentReporting = true, want false (the syncer's small default must flow through, not the hardcoded 15s fallback)")
	}
}

// TestRunAppHealthForServerScopesToOneServer proves the scoped single-server pass
// (appHealthRunner.runForServer, built on probeServer) probes and derives health
// for ONLY the requested server, touching neither another server's health nor its
// apps.
func TestRunAppHealthForServerScopesToOneServer(t *testing.T) {
	// Two active servers, each with one always_reachable app. A scoped pass for
	// srv-1 must set srv-1 healthy and touch neither srv-2's health nor its apps.
	store := &fakeHealthStore{
		servers: []routing.AIServer{
			{ID: "srv-1", Status: routing.ServerStatusActive},
			{ID: "srv-2", Status: routing.ServerStatusActive},
		},
		apps: map[string][]routing.Application{
			"srv-1": {{ID: "app-1", ServerID: "srv-1", Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable}},
			"srv-2": {{ID: "app-2", ServerID: "srv-2", Status: routing.ServerStatusActive, HealthCheckMode: routing.HealthCheckModeAlwaysReachable}},
		},
		settings: map[string]string{},
	}
	reg := gateway.NewAppHealthRegistry(nil)
	prober := newFakeProber()
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}
	now := func() time.Time { return time.Unix(5000, 0) }

	(&appHealthRunner{store: store, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, settings: store, probeTimeout: time.Second, cipher: nil, now: now}).runForServer(context.Background(), "srv-1", &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})

	if got := store.healthOf("srv-1"); got != routing.HealthHealthy {
		t.Fatalf("srv-1 health = %q, want %q", got, routing.HealthHealthy)
	}
	if got := store.healthOf("srv-2"); got != "" {
		t.Fatalf("scoped pass must not touch srv-2 health, got %q", got)
	}
	if _, ok := lastProbed["app-2"]; ok {
		t.Fatal("scoped pass must not probe srv-2's apps")
	}
}

func TestAppHealthIntervalReadsSetting(t *testing.T) {
	st := &fakeHealthStore{settings: map[string]string{"health_check_interval_seconds": "45"}}
	if got := appHealthInterval(context.Background(), st); got != 45*time.Second {
		t.Fatalf("interval = %v, want 45s", got)
	}

	absent := &fakeHealthStore{settings: map[string]string{}}
	if got := appHealthInterval(context.Background(), absent); got != appHealthDefaultInterval {
		t.Fatalf("interval (absent) = %v, want %v", got, appHealthDefaultInterval)
	}
}

func TestAppHealthIntervalFailOpenOnError(t *testing.T) {
	if got := appHealthInterval(context.Background(), errSettings{}); got != appHealthDefaultInterval {
		t.Fatalf("interval (error) = %v, want %v (fail-open)", got, appHealthDefaultInterval)
	}
}

type errSettings struct{}

func (errSettings) SystemSettings(context.Context) (map[string]string, error) {
	return nil, fmt.Errorf("boom")
}

// stubAppHealthLoop substitutes the startAppHealthLoop seam to observe that a
// deps builder starts the loop with non-nil dependencies and the production
// probe timeout, and that cleanup cancels it. It restores the original on
// t.Cleanup and returns pointers to the observed call count / timeout / cancel.
func stubAppHealthLoop(t *testing.T) (calls *int, gotTimeout *time.Duration, cancelled *bool) {
	t.Helper()
	calls = new(int)
	gotTimeout = new(time.Duration)
	cancelled = new(bool)
	orig := startAppHealthLoop
	startAppHealthLoop = func(runner *appHealthRunner, serverTrigger <-chan string) context.CancelFunc {
		*calls++
		*gotTimeout = runner.probeTimeout
		// Task 4 wired the real AgentPresence registry into all three main.go
		// drivers, so it is now asserted non-nil alongside the other deps.
		if runner.store == nil || runner.prober == nil || runner.syncer == nil || runner.registry == nil || runner.loaded == nil || runner.agents == nil || runner.groups == nil || runner.settings == nil {
			t.Errorf("startAppHealthLoop got a nil dependency: store=%v prober=%v syncer=%v registry=%v loaded=%v agents=%v groups=%v settings=%v",
				runner.store, runner.prober, runner.syncer, runner.registry, runner.loaded, runner.agents, runner.groups, runner.settings)
		}
		return func() { *cancelled = true }
	}
	t.Cleanup(func() { startAppHealthLoop = orig })
	return calls, gotTimeout, cancelled
}

func TestMemoryDepsStartsAppHealthLoopAndCleanupStopsIt(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "dev-secret")
	calls, gotTimeout, cancelled := stubAppHealthLoop(t)

	cfg := config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory", AppHealthProbeTimeout: 7 * time.Second}
	_, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	if *calls != 1 {
		t.Fatalf("startAppHealthLoop calls = %d, want 1", *calls)
	}
	if *gotTimeout != 7*time.Second {
		t.Fatalf("probe timeout = %v, want 7s (cfg.AppHealthProbeTimeout)", *gotTimeout)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup returned %v", err)
	}
	if !*cancelled {
		t.Fatal("cleanup did not cancel the app-health loop")
	}
}

func TestSqliteDepsStartsAppHealthLoopAndCleanupStopsIt(t *testing.T) {
	calls, gotTimeout, cancelled := stubAppHealthLoop(t)

	cfg := config.Config{
		Addr:                  "127.0.0.1:8080",
		DBDriver:              "sqlite",
		SQLitePath:            filepath.Join(t.TempDir(), "gateway.db"),
		AutoMigrate:           true,
		AppHealthProbeTimeout: 9 * time.Second,
	}
	_, cleanup, err := buildGatewayServer(cfg)
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	if *calls != 1 {
		t.Fatalf("startAppHealthLoop calls = %d, want 1", *calls)
	}
	if *gotTimeout != 9*time.Second {
		t.Fatalf("probe timeout = %v, want 9s (cfg.AppHealthProbeTimeout)", *gotTimeout)
	}
	// cleanup must cancel the loop AND close the store without erroring.
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup returned %v", err)
	}
	if !*cancelled {
		t.Fatal("cleanup did not cancel the app-health loop")
	}
}

func TestRunAppHealthLoopStopsOnCancel(t *testing.T) {
	st := &fakeHealthStore{settings: map[string]string{}}
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runAppHealthLoop(ctx, &appHealthRunner{store: st, prober: prober, registry: reg, settings: st, probeTimeout: time.Second, now: func() time.Time { return time.Now().UTC() }}, nil)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runAppHealthLoop did not return after context cancel")
	}
}

func TestRunAppHealthLoopProbesImmediately(t *testing.T) {
	shrinkRetryGap(t)
	// Default settings -> 30s interval, so the ticker will NOT fire during this
	// test; only the startup probe can write health within the 2s window.
	st := newHealthTestStore(activeApp("a1", "s1", 8001))
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go runAppHealthLoop(ctx, &appHealthRunner{store: st, prober: prober, registry: reg, settings: st, probeTimeout: time.Second, now: func() time.Time { return time.Now().UTC() }}, nil)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if st.healthOf("s1") == routing.HealthHealthy {
			return // immediate startup probe ran before the first interval tick
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("runAppHealthLoop did not probe immediately at startup (health not written before the first tick)")
}

// netbirdServer builds an active AI-server whose NetBird-peer flag is set as
// given, used by the netbird_only outbound-restriction tests below.
func netbirdServer(id string, netbirdEnabled bool) routing.AIServer {
	return routing.AIServer{
		ID: id, Domain: id + ".local", Provider: routing.ProviderVLLM,
		Status: routing.ServerStatusActive, NetbirdEnabled: netbirdEnabled,
	}
}

// TestRunAppHealthOnceNetbirdOnlyExcludesOffMeshServer proves the reachability
// choke point: with netbird_only ON, a server that is NOT a NetBird peer is
// forced unreachable (and never dialed), while a NetBird-enabled server is
// unaffected and probed normally.
func TestRunAppHealthOnceNetbirdOnlyExcludesOffMeshServer(t *testing.T) {
	shrinkRetryGap(t)
	st := &fakeHealthStore{
		servers: []routing.AIServer{netbirdServer("s1", false), netbirdServer("s2", true)},
		apps: map[string][]routing.Application{
			"s1": {activeApp("a1", "s1", 8001)},
			"s2": {activeApp("a2", "s2", 8002)},
		},
		settings: map[string]string{"netbird_only": "true"},
	}
	prober := newFakeProber() // both endpoints reachable if probed
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if reg.Reachable("a1") {
		t.Fatalf("off-mesh app a1 must be unreachable under netbird_only")
	}
	if !reg.Reachable("a2") {
		t.Fatalf("on-mesh app a2 must stay reachable under netbird_only")
	}
	// The off-mesh server is NEVER dialed; the on-mesh server is probed once.
	if n := prober.callCount("http://s1.local:8001"); n != 0 {
		t.Fatalf("off-mesh server probed %d times, want 0 (never dialed)", n)
	}
	if n := prober.callCount("http://s2.local:8002"); n != 1 {
		t.Fatalf("on-mesh server probed %d times, want 1", n)
	}
	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("off-mesh server health = %q, want %q", got, routing.HealthUnhealthy)
	}
	if got := st.healthOf("s2"); got != routing.HealthHealthy {
		t.Fatalf("on-mesh server health = %q, want %q", got, routing.HealthHealthy)
	}
}

// TestRunAppHealthOnceNetbirdOnlySkipsOffMeshLoadedAndContextProbe proves the
// two off-mesh guards on the loaded-models pass (`&& !offMesh`) and the
// context-probe pass (`&& !offMesh`): under netbird_only an off-mesh server's app
// that HAS both a loaded-models path AND a context-probe path is dialed by NEITHER
// pass, so the loaded registry stays empty and the context-probe upstream is never
// called. (Mutation guard: dropping `&& !offMesh` from EITHER pass makes that pass
// run for the off-mesh server -> one of the two assertions fails.)
func TestRunAppHealthOnceNetbirdOnlySkipsOffMeshLoadedAndContextProbe(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.LoadedModelsPath = "/running"
	app.LoadedModelsFormat = "llama_swap"
	app.ContextProbePath = "/props"
	st := &fakeHealthStore{
		servers:  []routing.AIServer{netbirdServer("s1", false)}, // off-mesh
		apps:     map[string][]routing.Application{"s1": {app}},
		settings: map[string]string{"netbird_only": "true"},
	}
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	// Both would be returned if either pass ran against the off-mesh server.
	prober.loaded["http://s1.local:8001"] = []string{"should-not-appear"}
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	// Loaded-models pass skipped -> the registry is never populated for the off-mesh app.
	if got := loaded.LoadedAppModels("a1", "s1"); len(got) != 0 {
		t.Fatalf("loaded models = %v, want empty (off-mesh loaded pass must be skipped under netbird_only)", got)
	}
	// Context-probe pass skipped -> the upstream context probe was never dialed.
	if n := prober.ctxProbeCallCount(); n != 0 {
		t.Fatalf("ProbeModelInfo called %d times, want 0 (off-mesh context probe must be skipped under netbird_only)", n)
	}
	// And its context mapping stays at the default (never written).
	if got, _ := st.mappingOf("m1"); got.ContextSize != 0 {
		t.Fatalf("m1 ContextSize = %d, want 0 (off-mesh -> no probe -> no write)", got.ContextSize)
	}
}

// TestRunAppHealthOnceNetbirdOnlyExcludesAlwaysReachableOffMesh proves the
// override runs BEFORE the always_reachable short-circuit: an always_reachable
// app on an off-mesh server is excluded too (moving the override after the
// short-circuit would let this app slip through as reachable).
func TestRunAppHealthOnceNetbirdOnlyExcludesAlwaysReachableOffMesh(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.AlwaysReachable = true
	st := &fakeHealthStore{
		servers:  []routing.AIServer{netbirdServer("s1", false)},
		apps:     map[string][]routing.Application{"s1": {app}},
		settings: map[string]string{"netbird_only": "true"},
	}
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if reg.Reachable("a1") {
		t.Fatalf("always_reachable off-mesh app must be excluded under netbird_only (override precedes the short-circuit)")
	}
	if got := st.healthOf("s1"); got != routing.HealthUnhealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthUnhealthy)
	}
}

// TestRunAppHealthOnceNetbirdOnlyOffKeepsOffMeshReachable is the no-op invariant:
// with netbird_only OFF (absent), an off-mesh always_reachable app stays
// reachable exactly as today.
func TestRunAppHealthOnceNetbirdOnlyOffKeepsOffMeshReachable(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.AlwaysReachable = true
	st := &fakeHealthStore{
		servers:  []routing.AIServer{netbirdServer("s1", false)},
		apps:     map[string][]routing.Application{"s1": {app}},
		settings: map[string]string{}, // netbird_only absent -> OFF
	}
	prober := newFakeProber()
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !reg.Reachable("a1") {
		t.Fatalf("with netbird_only OFF, an off-mesh always_reachable app must stay reachable (no-op)")
	}
	if got := st.healthOf("s1"); got != routing.HealthHealthy {
		t.Fatalf("server health = %q, want %q", got, routing.HealthHealthy)
	}
}

// TestRunAppHealthOnceNetbirdOnlyReadErrorFailsOpen proves the fail-OPEN on the
// settings read: an unreadable settings source is treated as netbird_only OFF so
// a settings glitch can never blackhole every off-mesh server.
func TestRunAppHealthOnceNetbirdOnlyReadErrorFailsOpen(t *testing.T) {
	shrinkRetryGap(t)
	st := &fakeHealthStore{
		servers: []routing.AIServer{netbirdServer("s1", false)},
		apps:    map[string][]routing.Application{"s1": {activeApp("a1", "s1", 8001)}},
	}
	prober := newFakeProber() // reachable if probed
	reg := gateway.NewAppHealthRegistry(nil)

	// errSettings fails every SystemSettings read: netbird_only must default OFF.
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: errSettings{}, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !reg.Reachable("a1") {
		t.Fatalf("a settings read error must fail open (netbird_only OFF); off-mesh app must not be blackholed")
	}
	if n := prober.callCount("http://s1.local:8001"); n != 1 {
		t.Fatalf("off-mesh server probed %d times, want 1 (fail-open: probed normally)", n)
	}
}

// TestNetbirdOnlyExcludesOffMeshFromRouting is the end-to-end propagation test:
// after the health loop marks an off-mesh app unreachable, the routing resolver
// (which shares the AppHealthRegistry) drops it — the on-mesh peer serves the
// shared model, and a model served ONLY by the off-mesh peer yields no healthy
// host.
func TestNetbirdOnlyExcludesOffMeshFromRouting(t *testing.T) {
	shrinkRetryGap(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	mem := routing.NewMemoryStore()

	seedServer := func(id string, netbird bool) {
		if err := mem.CreateAIServer(ctx, routing.AIServer{ID: id, Name: id, Domain: id + ".test", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, NetbirdEnabled: netbird, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateAIServer(%s): %v", id, err)
		}
	}
	seedApp := func(serverID, appID string) {
		if err := mem.CreateApplication(ctx, routing.Application{ID: appID, ServerID: serverID, Type: routing.ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateApplication(%s): %v", appID, err)
		}
	}
	seedMapping := func(appID, mappingID, gatewayModel string) {
		if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: mappingID, ApplicationID: appID, GatewayModelName: gatewayModel, AppModelName: "up", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatalf("CreateMapping(%s): %v", mappingID, err)
		}
	}
	seedServer("s1", false) // off-mesh
	seedServer("s2", true)  // on-mesh
	seedApp("s1", "a1")
	seedApp("s2", "a2")
	seedMapping("a1", "m1", "shared")      // s1 serves shared
	seedMapping("a2", "m2", "shared")      // s2 serves shared too
	seedMapping("a1", "m3", "onlyoffmesh") // ONLY the off-mesh server serves this

	prober := newFakeProber() // both endpoints reachable if probed
	settings := &fakeHealthStore{settings: map[string]string{"netbird_only": "true"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: mem, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: settings, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	resolver := routing.NewResolver(mem, clock, reg)
	tok := auth.Token{ID: "tok", UserID: "usr", Active: true}

	// The shared model must route to the on-mesh peer only (off-mesh dropped).
	target, err := resolver.Resolve(ctx, tok, inference.Request{Model: "shared", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve(shared): %v", err)
	}
	if target.ServerID != "s2" {
		t.Fatalf("shared routed to %q, want s2 (off-mesh s1 must be excluded)", target.ServerID)
	}

	// A model served ONLY by the off-mesh peer has no healthy host under netbird_only.
	if _, err := resolver.Resolve(ctx, tok, inference.Request{Model: "onlyoffmesh", APIFlavor: "openai_chat"}); !errors.Is(err, routing.ErrNoHealthyHost) {
		t.Fatalf("Resolve(onlyoffmesh) err = %v, want ErrNoHealthyHost (off-mesh only)", err)
	}
}

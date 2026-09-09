// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHealthStore is an in-memory healthStore + settings source for the loop tests.
type fakeHealthStore struct {
	servers  []routing.AIServer
	apps     map[string][]routing.Application
	settings map[string]string

	mu                sync.Mutex
	health            map[string]string
	mappings          map[string][]routing.ModelMapping  // keyed by application id
	ctxProbeSets      int                                // count of UpdateMappingContextProbe calls
	liveProgressSets  int                                // count of UpdateMappingLiveProgressSupport calls
	capabilitiesSets  int                                // count of UpdateMappingCapabilities calls
	visionCapableSets int                                // count of UpdateMappingVisionCapable calls
	availSamplesLog   []routing.ServerAvailabilitySample // append-ordered availability samples
	failInsert        bool                               // when true, InsertServerAvailabilitySample errors
	// runtimeSpecs backs RuntimeSpecsByApplication (issue #58 per-mapping
	// upstream credentials); specsErr, when set, makes the call fail instead
	// (exercising the fallback-to-app-token degrade path).
	runtimeSpecs []routing.RuntimeSpec
	specsErr     bool
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

// UpdateMappingLiveProgressSupport mirrors the store contract (#51): it stamps
// live_progress_support + live_progress_checked_at UNCONDITIONALLY -- UNLIKE
// UpdateMappingContextProbe above it carries NO metrics_locked guard, mirroring
// SQLiteStore.UpdateMappingLiveProgressSupport (a build capability, not a
// metric an operator pins numbers against).
func (f *fakeHealthStore) UpdateMappingLiveProgressSupport(_ context.Context, id, support string, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.liveProgressSets++
	for appID, list := range f.mappings {
		for i := range list {
			if list[i].ID != id {
				continue
			}
			list[i].LiveProgressSupport = support
			t := at
			list[i].LiveProgressCheckedAt = &t
			f.mappings[appID] = list
			return nil
		}
	}
	return nil
}

// liveProgressSetCount returns how many times UpdateMappingLiveProgressSupport
// was called (a call-count spy for the no-rewrite property).
func (f *fakeHealthStore) liveProgressSetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveProgressSets
}

// UpdateMappingCapabilities mirrors the store contract (#49-2): only
// non-empty verdicts are written -- a partial probe answer must never clear
// an already-stored one -- stamping CapabilitiesSource + CapabilitiesCheckedAt
// only when something was actually written, mirroring
// MemoryStore/SQLiteStore.UpdateMappingCapabilities. NO metrics_locked guard,
// matching UpdateMappingLiveProgressSupport above.
func (f *fakeHealthStore) UpdateMappingCapabilities(_ context.Context, id string, caps routing.CapabilityVerdicts, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capabilitiesSets++
	for appID, list := range f.mappings {
		for i := range list {
			if list[i].ID != id {
				continue
			}
			wrote := false
			if caps.Vision != "" {
				list[i].CapVision = caps.Vision
				wrote = true
			}
			if caps.Video != "" {
				list[i].CapVideo = caps.Video
				wrote = true
			}
			if caps.Audio != "" {
				list[i].CapAudio = caps.Audio
				wrote = true
			}
			if caps.Tools != "" {
				list[i].CapTools = caps.Tools
				wrote = true
			}
			if len(caps.Extra) > 0 {
				encoded, _ := json.Marshal(caps.Extra)
				list[i].CapExtra = string(encoded)
				wrote = true
			}
			if wrote {
				list[i].CapabilitiesSource = caps.Source
				t := at
				list[i].CapabilitiesCheckedAt = &t
			}
			f.mappings[appID] = list
			return nil
		}
	}
	return nil
}

// capabilitiesSetCount returns how many times UpdateMappingCapabilities was
// called (a call-count spy for the no-rewrite property, mirroring
// liveProgressSetCount).
func (f *fakeHealthStore) capabilitiesSetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capabilitiesSets
}

// UpdateMappingVisionCapable mirrors the store contract: sets vision_capable +
// provenance ("vision") ONLY while the mapping is unlocked (a missing or
// locked mapping is a benign no-op), mirroring
// MemoryStore/SQLiteStore.UpdateMappingVisionCapable.
func (f *fakeHealthStore) UpdateMappingVisionCapable(_ context.Context, id string, capable bool, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visionCapableSets++
	for appID, list := range f.mappings {
		for i := range list {
			if list[i].ID != id || list[i].MetricsLocked {
				continue
			}
			list[i].VisionCapable = capable
			list[i].MetricsSource = "vision"
			t := at
			list[i].MetricsUpdatedAt = &t
			f.mappings[appID] = list
			return nil
		}
	}
	return nil
}

// visionCapableSetCount returns how many times UpdateMappingVisionCapable was
// called (a call-count spy for the vision-sync convergence property).
func (f *fakeHealthStore) visionCapableSetCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.visionCapableSets
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

// RuntimeSpecsByApplication returns the seeded runtimeSpecs (ignoring appID --
// every test using this fake seeds a single application), or specsErr when set
// (the read-failure degrade-to-app-token test).
func (f *fakeHealthStore) RuntimeSpecsByApplication(_ context.Context, _ string) ([]routing.RuntimeSpec, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.specsErr {
		return nil, fmt.Errorf("runtime specs: boom")
	}
	return append([]routing.RuntimeSpec(nil), f.runtimeSpecs...), nil
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
	// modelInfoErrValue maps a probe PATH to an error ProbeModelInfo returns
	// verbatim (nil infos) -- unlike modelInfoErr (keyed by endpoint, always a
	// generic error), this lets a test inject a SPECIFIC error (e.g. a wrapped
	// provider.ErrAuthRejected) for one {model}-expanded path.
	modelInfoErrValue map[string]error
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
		modelInfoByPath:   map[string][]provider.ModelInfo{},
		modelInfoErrValue: map[string]error{},
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
	if err, ok := f.modelInfoErrValue[probePath]; ok {
		return nil, err
	}
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

// TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle
// is the no-rewrite property test (#51): a llama.cpp /props body carrying
// "supported" must be persisted on the first cycle, and a second,
// otherwise-identical cycle (a fresh cadence state, standing in for the next
// ~30s tick) must NOT reissue the write, since the stored verdict already
// matches. Without this comparison the pass would issue an UPDATE per
// application per cadence tick forever -- the same reasoning
// writeBackRuntimeContext documents at length in
// internal/gateway/agent_ingest.go. Uses a call-count spy
// (liveProgressSetCount), mirroring the repo's existing ctxProbeSetCount spy.
func TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}

	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q", got.LiveProgressSupport, "supported")
	}
	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state (as if the next tick's interval
	// had elapsed) reports the SAME verdict -- it must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times across two identical cycles, want 1 (an unchanged verdict must not be rewritten)", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateCapabilitiesPersistOnceNotAgainOnAnIdenticalCycle
// is TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle's
// counterpart for the capability write (#49-2) on the {model}-template branch:
// a llama.cpp /props body carrying modalities + chat_template_caps evidence
// must be persisted on the first cycle, and a second, otherwise-identical
// cycle must NOT reissue the write, since the stored verdicts already match --
// the same no-rewrite property applyCapabilityWrite documents. Uses the new
// call-count spy (capabilitiesSetCount), mirroring liveProgressSetCount.
func TestRunAppHealthOnceContextProbeTemplateCapabilitiesPersistOnceNotAgainOnAnIdenticalCycle(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{
		Name: "m-a", ContextSize: 8192,
		Caps: provider.Capabilities{Vision: "yes", Tools: "yes"},
	}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.CapVision != "yes" || got.CapTools != "yes" {
		t.Fatalf("CapVision=%q CapTools=%q, want yes/yes", got.CapVision, got.CapTools)
	}
	if got.CapabilitiesSource != "llama_cpp_props" {
		t.Fatalf("CapabilitiesSource = %q, want %q", got.CapabilitiesSource, "llama_cpp_props")
	}
	if n := st.capabilitiesSetCount(); n != 1 {
		t.Fatalf("UpdateMappingCapabilities called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state reports the SAME verdicts --
	// they must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilitiesSetCount(); n != 1 {
		t.Fatalf("UpdateMappingCapabilities called %d times across two identical cycles, want 1 (an unchanged capability set must not be rewritten)", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateAllEmptyCapabilitiesNeverWrite proves
// an all-empty verdict set (detection ran, determined nothing -- e.g. a body
// with no modalities/chat_template_caps objects at all) never calls
// UpdateMappingCapabilities: applyCapabilityWrite's diff stays entirely empty,
// so the write is skipped rather than issuing a pointless round trip that the
// store would have no-op'd anyway.
func TestRunAppHealthOnceContextProbeTemplateAllEmptyCapabilitiesNeverWrite(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	// No Caps set at all -- the zero value, standing in for a /props body with
	// neither modalities nor chat_template_caps.
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilitiesSetCount(); n != 0 {
		t.Fatalf("UpdateMappingCapabilities called %d times for an all-empty verdict set, want 0", n)
	}
	got, _ := st.mappingOf("m1")
	if got.CapVision != "" || got.CapVideo != "" || got.CapAudio != "" || got.CapTools != "" {
		t.Fatalf("capability fields = %+v, want all empty", got)
	}
}

// TestRunAppHealthOnceSingleProbeNamelessCapabilitiesReachEveryMapping is the
// capability-write counterpart to
// TestRunAppHealthOnceSingleProbeNamelessVerdictReachesEveryMapping: a /props
// body that carries capability evidence but no model/model_path parses to a
// NAMELESS ModelInfo (TestParseModelInfoNamelessBodyWithCapabilitiesOnlyStillCarriesThem),
// and a single-probe application has exactly ONE endpoint, so every mapping it
// owns is served by that same build and the unattributable verdict is
// genuinely theirs. Both mappings must be written -- asserted as a numeric
// count of 2, so a silent one-mapping-only regression is caught.
func TestRunAppHealthOnceSingleProbeNamelessCapabilitiesReachEveryMapping(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	// What parseModelInfo yields for a /props body with modalities but no
	// model/model_path: a capability verdict, no name, no context size.
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Caps: provider.Capabilities{Vision: "yes"}}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilitiesSetCount(); n != 2 {
		t.Fatalf("UpdateMappingCapabilities called %d times, want 2 -- a nameless verdict belongs to every mapping of this one-endpoint application", n)
	}
	for _, id := range []string{"m1", "m2"} {
		got, _ := st.mappingOf(id)
		if got.CapVision != "yes" {
			t.Fatalf("%s CapVision = %q, want %q", id, got.CapVision, "yes")
		}
		if got.ContextSize != 0 {
			t.Fatalf("%s ContextSize = %d, want 0 (a nameless entry reports no size, so nothing may be attributed)", id, got.ContextSize)
		}
	}
}

// TestRunAppHealthOnceVisionSyncConvergesEvenWhenTriStateVerdictUnchanged is
// the CONVERGENCE regression test named in the task brief: it pins the lesson
// of Task 4's first (wrong) attempt at the vision sync in
// internal/gateway/agent_ingest.go, on this package's own write-back.
//
// The mapping starts with CapVision already "yes" (matching what the probe is
// about to report -- the tri-state is UNCHANGED) but VisionCapable is
// desynced to false, as if the independent vision BENCHMARK
// (benchmark_runner.go's UpdateMappingVisionCapable call) had pinned a wrong,
// stale answer. Because the tri-state is unchanged, applyCapabilityWrite's
// diff.Vision stays "" and UpdateMappingCapabilities is never called -- but
// the vision sync must still run, because it is driven by caps.Vision (what
// THIS probe reported), not by diff.Vision (what changed). A sync driven by
// the changed-only verdict would never fire here, leaving VisionCapable stuck
// at the wrong value forever on a build whose real vision support never
// changes -- exactly the bug Task 4 shipped first.
func TestRunAppHealthOnceVisionSyncConvergesEvenWhenTriStateVerdictUnchanged(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a",
			Status: routing.ServerStatusActive,
			// Tri-state already on file, matching the incoming probe exactly.
			CapVision: "yes", CapabilitiesSource: "llama_cpp_props",
			// Desynced: a benchmark (or a stale prior write) pinned false.
			VisionCapable: false,
		}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{
		Name: "m-a", ContextSize: 8192,
		Caps: provider.Capabilities{Vision: "yes"},
	}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if !got.VisionCapable {
		t.Fatalf("VisionCapable = false, want true -- an unchanged tri-state verdict must still converge a desynced vision_capable bool")
	}
	if n := st.visionCapableSetCount(); n != 1 {
		t.Fatalf("UpdateMappingVisionCapable called %d times, want 1", n)
	}
	// The tri-state itself was unchanged, so the capability writer must not
	// have been called at all -- this is what makes the mutation (driving the
	// sync from the changed-only diff instead of the reported verdict) fail:
	// with that mutation, diff.Vision is empty and the sync above would never
	// fire, leaving VisionCapable stuck at false.
	if n := st.capabilitiesSetCount(); n != 0 {
		t.Fatalf("UpdateMappingCapabilities called %d times for an unchanged tri-state verdict, want 0", n)
	}
}

// fakeAgentBundle satisfies agentRegistryBundle for feature-gate tests:
// ReportingWithin/Retain are inert, HasFeature answers from a static map.
type fakeAgentBundle struct{ features map[string][]string }

func (f fakeAgentBundle) ReportingWithin(string, time.Duration) bool { return false }
func (f fakeAgentBundle) Retain(map[string]struct{})                 {}
func (f fakeAgentBundle) HasFeature(serverID, feature string) bool {
	for _, name := range f.features[serverID] {
		if name == feature {
			return true
		}
	}
	return false
}

// presenceBundle adapts a bare *gateway.AgentPresenceRegistry -- the shape
// every pre-existing `agents:` field below already constructs, predating
// HasFeature -- to the (now three-method) agentRegistryBundle:
// ReportingWithin/Retain promote unchanged from the embedded registry, and
// HasFeature is unconditionally false, the same fail-closed default a
// production agentRegistries with no agentFeatures registry wired already
// returns (see agentRegistries.HasFeature above).
type presenceBundle struct{ *gateway.AgentPresenceRegistry }

func (presenceBundle) HasFeature(string, string) bool { return false }

// serverAgentApp builds an active server_agent-typed app with an empty
// ContextProbePath, so the implicit-default-gate tests below start from the
// exact shape the gate is meant to recognize.
func serverAgentApp(id, serverID string, port int) routing.Application {
	app := activeApp(id, serverID, port)
	app.Type = routing.ProviderServerAgent
	return app
}

// TestRunAppHealthOnceServerAgentImplicitPropsPathProbesPerLoadedMapping
// proves the issue #58 implicit default: a server_agent application with no
// operator-set ContextProbePath, whose agent declared
// runtimeUpstreamPropsFeature, gets probed at serverAgentPropsProbePath
// (/upstream/{model}/props) per loaded mapping -- exactly like an explicit
// {model}-template ContextProbePath would. A second, otherwise-identical
// cycle must not re-issue the live-progress write (the same no-rewrite
// property TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAn-
// IdenticalCycle proves for the explicit-path case above).
func TestRunAppHealthOnceServerAgentImplicitPropsPathProbesPerLoadedMapping(t *testing.T) {
	shrinkRetryGap(t)
	app := serverAgentApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/up/props"] = []provider.ModelInfo{{Name: "up", LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"up"}) // "up" is loaded
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/up/props") {
		t.Fatalf("expected the server_agent implicit default path /upstream/up/props to be probed")
	}
	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state (as if the next tick's interval
	// had elapsed) reports the SAME verdict -- it must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times across two identical cycles, want 1 (an unchanged verdict must not be rewritten)", n)
	}
}

// TestRunAppHealthOnceServerAgentWithoutFeatureNeverProbes proves the
// fail-closed half of the gate: a server_agent application with an empty
// ContextProbePath gets NO implicit probe at all -- neither when the agent
// bundle has positive evidence the server did NOT declare the feature (an
// empty features map) nor when there is no bundle at all (nil, the shape
// every test above this one in the file already passes). Without this gate
// an agent lacking the runtime router's /upstream/{model}/props route would
// be probed anyway and answer 404 runtime.model_not_managed forever.
func TestRunAppHealthOnceServerAgentWithoutFeatureNeverProbes(t *testing.T) {
	cases := []struct {
		name   string
		agents agentRegistryBundle
	}{
		{"feature not declared", fakeAgentBundle{features: map[string][]string{}}},
		{"nil bundle", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			shrinkRetryGap(t)
			app := serverAgentApp("a1", "s1", 8001)
			st := newHealthTestStore(app)
			st.mappings = map[string][]routing.ModelMapping{
				"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
			}
			prober := newFakeProber()
			prober.modelInfoByPath["/upstream/up/props"] = []provider.ModelInfo{{Name: "up", LiveProgressSupport: "supported"}}
			reg := gateway.NewAppHealthRegistry(nil)
			loaded := gateway.NewLoadedModelRegistry()
			loaded.SetGatewayProbe("a1", []string{"up"})

			(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: tc.agents, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

			if n := prober.ctxProbeCallCount(); n != 0 {
				t.Fatalf("ProbeModelInfo called %d times without the declared feature, want 0", n)
			}
			if n := st.liveProgressSetCount(); n != 0 {
				t.Fatalf("UpdateMappingLiveProgressSupport called %d times without the declared feature, want 0", n)
			}
		})
	}
}

// TestRunAppHealthOnceServerAgentOperatorPathWins proves an operator-set
// ContextProbePath always wins over the implicit server_agent default, even
// when the agent declared the feature: the operator's path is probed, and
// the implicit default path is never touched.
func TestRunAppHealthOnceServerAgentOperatorPathWins(t *testing.T) {
	shrinkRetryGap(t)
	app := serverAgentApp("a1", "s1", 8001)
	app.ContextProbePath = "/custom/{model}/info"
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/custom/up/info"] = []provider.ModelInfo{{Name: "up", LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"up"})
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/custom/up/info") {
		t.Fatalf("expected the operator-set path /custom/up/info to be probed")
	}
	if prober.probedPath("/upstream/up/props") {
		t.Fatalf("the operator-set ContextProbePath must win -- the implicit default must never be probed")
	}
}

// TestRunAppHealthOnceServerAgentProbeCarriesSpecToken is the wire-level
// credential test (issue #58): the per-mapping context carried into the
// {model} branch's ProbeModelInfo calls is unexported, so this proves it on
// the WIRE instead, using a REAL provider client (the same OpenAI-compatible
// constructor providerClients wires for server_agent) against an httptest
// server standing in for the agent's runtime router. Mapping A resolves to
// the default Authorization: Bearer header; mapping B's spec sets a custom
// transmission header (X-Api-Key) and must carry NO Authorization header at
// all. Both differ from the APPLICATION's own token, so a regression that
// falls back to the app-level credential is caught on the wire, not just in
// an unobservable context value.
func TestRunAppHealthOnceServerAgentProbeCarriesSpecToken(t *testing.T) {
	shrinkRetryGap(t)

	type seenAuth struct{ authorization, apiKey string }
	var mu sync.Mutex
	seen := map[string]seenAuth{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen[r.URL.Path] = seenAuth{authorization: r.Header.Get("Authorization"), apiKey: r.Header.Get("X-Api-Key")}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":true}}}`))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	app := serverAgentApp("a1", "s1", port)
	// A token the pass must NEVER send once a per-mapping spec exists --
	// its presence on the wire would mean the fallback-to-app-token path
	// fired instead of SpecUpstreamAuth.
	app.APIToken = "plain:app-tok-must-not-be-used"
	st := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s1", Domain: u.Hostname(), Provider: routing.ProviderServerAgent, Status: routing.ServerStatusActive}},
		apps:    map[string][]routing.Application{"s1": {app}},
	}
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "mpA", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "a", Status: routing.ServerStatusActive},
			{ID: "mpB", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "b", Status: routing.ServerStatusActive},
		},
	}
	st.runtimeSpecs = []routing.RuntimeSpec{
		{MappingID: "mpA", APITokenMode: "set", APIToken: "plain:tok-a"},
		{MappingID: "mpB", APITokenMode: "set", APIToken: "plain:tok-b", APITokenHeaderSource: "custom", APITokenHeader: "X-Api-Key"},
	}

	prober := providerClients(0, false, nil) // real Multiplexer -> OpenAICompatibleClient for server_agent
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"a", "b"})
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	mu.Lock()
	a, b := seen["/upstream/a/props"], seen["/upstream/b/props"]
	mu.Unlock()

	if a.authorization != "Bearer tok-a" {
		t.Fatalf("mapping A Authorization = %q, want %q", a.authorization, "Bearer tok-a")
	}
	if a.apiKey != "" {
		t.Fatalf("mapping A X-Api-Key = %q, want empty", a.apiKey)
	}
	if b.apiKey != "tok-b" {
		t.Fatalf("mapping B X-Api-Key = %q, want %q", b.apiKey, "tok-b")
	}
	if b.authorization != "" {
		t.Fatalf("mapping B Authorization = %q, want empty (custom header source, no Authorization sent)", b.authorization)
	}
	if n := st.liveProgressSetCount(); n != 2 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 2", n)
	}
}

// TestRunAppHealthOnceServerAgentAuthRejectedLogsAndNeverWrites proves the
// misconfigured-token signal (issue #58): once the retry ALSO fails with
// provider.ErrAuthRejected (wrapped, as a real client's 401/403 classification
// would produce), the pass logs the distinct "check the runtime spec's API
// token" message instead of the generic probe-failed noise, and persists
// nothing.
func TestRunAppHealthOnceServerAgentAuthRejectedLogsAndNeverWrites(t *testing.T) {
	shrinkRetryGap(t)
	app := serverAgentApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoErrValue["/upstream/up/props"] = fmt.Errorf("wrapped: %w", provider.ErrAuthRejected)
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"up"})
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 0 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times on a rejected probe, want 0", n)
	}
	if !strings.Contains(buf.String(), "check the runtime spec's API token") {
		t.Fatalf("log output = %q, want it to contain the misconfigured-token signal", buf.String())
	}
}

// TestRunAppHealthOnceServerAgentSpecReadFailureFallsBackToAppToken proves the
// degrade path (issue #58): when RuntimeSpecsByApplication fails, the {model}
// pass still probes (a store hiccup must not blackhole the probe) and every
// mapping falls back to the APPLICATION's own token -- SpecUpstreamAuth's
// documented behavior for a zero-value spec, reached here via the empty map
// the read failure leaves behind.
func TestRunAppHealthOnceServerAgentSpecReadFailureFallsBackToAppToken(t *testing.T) {
	shrinkRetryGap(t)

	var mu sync.Mutex
	var gotAuth string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"default_generation_settings":{"params":{"timings_per_token":true}}}`))
	}))
	defer upstream.Close()

	u, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parse upstream url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse upstream port: %v", err)
	}

	app := serverAgentApp("a1", "s1", port)
	app.APIToken = "plain:app-tok"
	st := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s1", Domain: u.Hostname(), Provider: routing.ProviderServerAgent, Status: routing.ServerStatusActive}},
		apps:    map[string][]routing.Application{"s1": {app}},
	}
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	st.specsErr = true

	prober := providerClients(0, false, nil)
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"up"})
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	// The read failure itself must be logged (distinct from the misconfigured-
	// token signal test 2 checks): this is what makes the assertion below
	// meaningful against a REVERT to the pre-#58 pass, which never calls
	// RuntimeSpecsByApplication at all and so never logs this line either --
	// the Authorization header alone would coincidentally match in both states
	// (an app-token-only pass and a spec-read-failure fallback both send the
	// app token), so the log line is the only observable that actually
	// distinguishes "read attempted and degraded" from "never attempted".
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !strings.Contains(buf.String(), "runtime specs for app a1 failed") {
		t.Fatalf("log output = %q, want it to record the spec read failure", buf.String())
	}
	mu.Lock()
	auth := gotAuth
	mu.Unlock()
	if auth != "Bearer app-tok" {
		t.Fatalf("Authorization = %q, want %q (a spec read failure must fall back to the app token)", auth, "Bearer app-tok")
	}
	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 1 (the probe must still proceed on a spec read failure)", n)
	}
}

// TestRunAppHealthOnceNonServerAgentNeverGetsImplicitPath proves the implicit
// default is type-gated, not feature-only: a llama_swap-typed app with an
// empty ContextProbePath gets no implicit probe even when the feature is
// (nonsensically) declared for its server.
func TestRunAppHealthOnceNonServerAgentNeverGetsImplicitPath(t *testing.T) {
	shrinkRetryGap(t)
	app := activeApp("a1", "s1", 8001)
	app.Type = routing.ProviderLlamaSwap
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-up", AppModelName: "up", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/up/props"] = []provider.ModelInfo{{Name: "up", LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"up"})
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {runtimeUpstreamPropsFeature}}}

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := prober.ctxProbeCallCount(); n != 0 {
		t.Fatalf("ProbeModelInfo called %d times for a non-server_agent app with an empty ContextProbePath, want 0 (the implicit path is type-gated, not feature-only)", n)
	}
}

// TestRunAppHealthOnceUnknownLiveProgressSupportNeverPersists proves the
// global "unknown never overwrites" rule at the pass level: when the probe's
// parsed ModelInfo carries LiveProgressSupport == "" -- exactly what a vLLM
// /v1/models body, an Ollama /api/show body, or any other non-llama.cpp-/props
// shape parses to (see detectLiveProgressSupport in
// internal/provider/model_info.go) -- the pass must never call the writer at
// all. This is the guard that keeps a live vLLM application from being
// silently marked unsupported and dropped from the live-progress figure by
// Task 3's decision rule.
//
// The mapping starts with an ALREADY-STORED "supported" verdict (as if a
// previous, correctly-shaped /props probe had determined it): this is what
// makes the assertion meaningful. If the pass instead compared only "changed
// vs. unchanged" without a dedicated empty-string guard, an unknown result
// ("" != "supported") would look like a "changed" value and would overwrite
// the good verdict with unknown -- exactly the regression this test exists to
// catch. The context-size write is independent and must still land, proving
// the two writes were not accidentally coupled together.
func TestRunAppHealthOnceUnknownLiveProgressSupportNeverPersists(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
			Status: routing.ServerStatusActive, LiveProgressSupport: "supported",
		}},
	}
	prober := newFakeProber()
	// Stands in for a vLLM /v1/models response: ProbeModelInfo reports a context
	// size but LiveProgressSupport is unknown ("") -- the schema field it is
	// about does not exist for this upstream's probed endpoint at all.
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 4096}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 0 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times for an unknown verdict, want 0", n)
	}
	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (unknown must never overwrite an already-stored verdict)", got.LiveProgressSupport, "supported")
	}
	if got.ContextSize != 4096 {
		t.Fatalf("ContextSize = %d, want 4096 (context-size probing must be unaffected by the live-progress guard)", got.ContextSize)
	}
}

// TestRunAppHealthOnceSingleProbeNamelessVerdictReachesEveryMapping is the
// attribution half of final-review F5, on the single-probe (non-{model})
// branch. A /props body that carries the capability evidence but no
// model/model_path now parses to a NAMELESS ModelInfo (see
// TestParseModelInfoNamelessBodyStillCarriesTheVerdict); with the old strict
// name equality that verdict could match nothing -- an AppModelName is never
// "" in practice -- so it was determined and then thrown away.
//
// A single-probe application has exactly ONE endpoint
// (routing.ApplicationEndpoint), so every mapping it owns is served by that
// same build and the unattributable verdict is genuinely theirs. Both mappings
// must therefore be written: the count is asserted numerically at 2, so a
// silent one-mapping-only regression is caught, and the context size (0 here,
// deliberately absent from a nameless entry) must stay untouched.
func TestRunAppHealthOnceSingleProbeNamelessVerdictReachesEveryMapping(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	// What parseModelInfo yields for a /props body with the params object but
	// no model/model_path: a verdict, no name, no context size.
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 2 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 2 -- a nameless verdict belongs to every mapping of this one-endpoint application", n)
	}
	for _, id := range []string{"m1", "m2"} {
		got, _ := st.mappingOf(id)
		if got.LiveProgressSupport != "supported" {
			t.Fatalf("%s LiveProgressSupport = %q, want %q", id, got.LiveProgressSupport, "supported")
		}
		if got.ContextSize != 0 {
			t.Fatalf("%s ContextSize = %d, want 0 (a nameless entry reports no size, so nothing may be attributed)", id, got.ContextSize)
		}
	}
}

// TestRunAppHealthOnceSingleProbeNamedVerdictStaysNameMatched is the guard on
// the other side of that widening: when the probe DOES report a model name,
// the strict name equality must survive untouched. Resolving F5 by simply
// asking provider.PickModelLiveProgressSupport per mapping (whose fallback
// takes the first non-empty verdict) would have widened the NAMED case too --
// attributing one model's verdict to every sibling mapping, which for a
// literal-path llama_swap application is a claim about a different upstream
// entirely. Asserted as a numeric count of 1 plus the untouched sibling.
func TestRunAppHealthOnceSingleProbeNamedVerdictStaysNameMatched(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 1 -- a NAMED verdict must still reach only the name-matched mapping", n)
	}
	gotA, _ := st.mappingOf("m1")
	if gotA.LiveProgressSupport != "supported" {
		t.Fatalf("m-a LiveProgressSupport = %q, want %q (name-matched)", gotA.LiveProgressSupport, "supported")
	}
	gotB, _ := st.mappingOf("m2")
	if gotB.LiveProgressSupport != "" {
		t.Fatalf("m-b LiveProgressSupport = %q, want %q (the reported name did not match)", gotB.LiveProgressSupport, "")
	}
}

// TestRunAppHealthOnceLiveProgressSupportPersistsChangedVerdict proves a
// changed verdict IS persisted (the complement of the no-rewrite test above):
// the mapping starts at "supported" and the upstream now reports
// "unsupported" (e.g. after a downgrade to an older llama.cpp build).
func TestRunAppHealthOnceLiveProgressSupportPersistsChangedVerdict(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxProbeApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
			Status: routing.ServerStatusActive, LiveProgressSupport: "supported",
		}},
	}
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 4096, LiveProgressSupport: "unsupported"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "unsupported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (a changed verdict must be persisted)", got.LiveProgressSupport, "unsupported")
	}
	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 1", n)
	}
}

// TestRunAppHealthOnceLiveProgressSupportIgnoresMetricsLock proves
// UpdateMappingLiveProgressSupport is deliberately NOT gated on
// mp.MetricsLocked, unlike UpdateMappingContextProbe: a capability is not a
// metric an operator pins numbers against (see the store method's doc
// comment), so an operator locking a mapping's throughput/context-size
// figures must not also silently suppress a real, freshly-detected build
// capability.
func TestRunAppHealthOnceLiveProgressSupportIgnoresMetricsLock(t *testing.T) {
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
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 131072, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	// ContextSize respects the lock (existing, untouched behavior)...
	if got.ContextSize != 4096 {
		t.Fatalf("locked ContextSize = %d, want 4096 (probe must not overwrite a lock)", got.ContextSize)
	}
	// ...but LiveProgressSupport is written regardless.
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (must be written even when metrics are locked)", got.LiveProgressSupport, "supported")
	}
}

// TestRunAppHealthOnceContextProbeTemplatePersistsLiveProgressSupport proves
// the per-model {model}-template branch (the second write site,
// app_health.go's `if strings.Contains(app.ContextProbePath, "{model}")`
// block) also persists the live-progress verdict, not just the single-probe
// branch exercised by the tests above.
func TestRunAppHealthOnceContextProbeTemplatePersistsLiveProgressSupport(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (per-model probe must persist the verdict too)", got.LiveProgressSupport, "supported")
	}
	if n := st.liveProgressSetCount(); n != 1 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times, want 1", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateUnknownVerdictNeverOverwrites is the
// {model}-branch counterpart to
// TestRunAppHealthOnceUnknownLiveProgressSupportNeverPersists above, and it
// guards the `support != ""` half of that branch's write condition -- the half
// that keeps vLLM working. It was unguarded by any test: every {model}-branch
// fixture reported a "supported" verdict, so deleting the term left the whole
// suite green.
//
// This branch is llama-swap's DEFAULT shape, and llama-swap's `peer` mode
// resolves a model to an arbitrary base URL whose /props answers with a
// non-llama.cpp body -- i.e. verdict "". Without the term, that "" would be
// seen as a change away from the stored "supported" and would overwrite a good
// verdict with unknown, on exactly the deployment this feature exists to
// recover.
//
// Asserted as a NUMERIC call count of zero on the writer spy, not by
// inspecting the stored field: the field assertion alone would also hold if the
// writer had been called with "", since the fake would then store "" over
// "supported" -- which is precisely the regression -- so the count is what
// makes this test fail on a revert. The context-size write must still land,
// proving the probe ran at all and that the two writes are not coupled.
func TestRunAppHealthOnceContextProbeTemplateUnknownVerdictNeverOverwrites(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a",
			Status: routing.ServerStatusActive, LiveProgressSupport: "supported",
		}},
	}
	prober := newFakeProber()
	// A llama-swap `peer`-mode answer: a parseable body that yields a context
	// size but NO live-progress verdict (LiveProgressSupport left "").
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.liveProgressSetCount(); n != 0 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times for an unknown verdict, want 0 -- unknown must never overwrite a stored verdict", n)
	}
	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want the untouched %q", got.LiveProgressSupport, "supported")
	}
	if got.ContextSize != 8192 {
		t.Fatalf("ContextSize = %d, want 8192 (the context-size write is independent of the live-progress guard)", got.ContextSize)
	}
}

// TestRunAppHealthOnceContextProbeTemplateUnchangedVerdictNotRewritten guards
// the OTHER half of the same condition, `support != mp.LiveProgressSupport`, on
// the {model} branch: no {model}-branch fixture seeded a stored verdict, so
// deleting this term also left the suite green.
//
// The stored verdict and the probed one are both "supported" here, so the
// correct behavior is zero writes -- and since this pass runs on the
// application's own ~30 s cadence, losing the term means one UPDATE per loaded
// model per application per tick, forever, for a value that can only change if
// an operator swaps the upstream binary. The probe itself must still run
// (asserted), so a zero count cannot be mistaken for "the branch never
// executed".
func TestRunAppHealthOnceContextProbeTemplateUnchangedVerdictNotRewritten(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a",
			Status: routing.ServerStatusActive, LiveProgressSupport: "supported", ContextSize: 8192,
		}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/m-a/props") {
		t.Fatalf("the per-model probe must have run -- otherwise a zero write count proves nothing")
	}
	if n := st.liveProgressSetCount(); n != 0 {
		t.Fatalf("UpdateMappingLiveProgressSupport called %d times for an UNCHANGED verdict, want 0 -- this pass runs on the app's own cadence, forever", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateLockedMappingPersistsCapabilityNotContextSize
// closes a gap the Task 2 implementer correctly surfaced but was told not to
// fix: the {model}-template branch's per-mapping skip used to read
// `mp.Status != routing.ServerStatusActive || mp.MetricsLocked ||
// mp.AppModelName == ""`, which skipped a LOCKED mapping before it was ever
// probed -- so its live-progress capability could never be learned at all,
// even though UpdateMappingLiveProgressSupport itself carries no lock guard
// (a capability is a property of the upstream build, not a number an
// operator answers for). This is llama-swap's default {model} shape, so the
// hole was not theoretical.
//
// The mapping starts with a distinguishable pre-existing state on BOTH
// fields under test -- ContextSize 4096 (the probe reports a different
// 8192) and LiveProgressSupport "" (the probe reports "supported") -- so
// neither assertion can pass by an accidental "both sides already equal"
// coincidence; each requires the fix to actually run the probe and the
// store's own guard to actually block the context-size write.
func TestRunAppHealthOnceContextProbeTemplateLockedMappingPersistsCapabilityNotContextSize(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a",
			Status: routing.ServerStatusActive, MetricsLocked: true, MetricsSource: "manual",
			ContextSize: 4096,
		}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/m-a/props") {
		t.Fatalf("a locked mapping must still be probed -- its capability cannot be learned without asking")
	}
	got, _ := st.mappingOf("m1")
	if got.LiveProgressSupport != "supported" {
		t.Fatalf("LiveProgressSupport = %q, want %q (a locked mapping's capability must still be persisted)", got.LiveProgressSupport, "supported")
	}
	// The pre-existing behavior must be unchanged: the store's own
	// `and metrics_locked = 0` guard (SQLiteStore.UpdateMappingContextProbe;
	// mirrored by MemoryStore and by fakeHealthStore.UpdateMappingContextProbe
	// above) refuses the context-size write for a locked row regardless of
	// whether the probe now runs.
	if got.ContextSize != 4096 {
		t.Fatalf("locked ContextSize = %d, want 4096 (context-size probing must stay refused for a locked mapping)", got.ContextSize)
	}
	if got.MetricsSource != "manual" {
		t.Fatalf("locked MetricsSource = %q, want %q (unchanged)", got.MetricsSource, "manual")
	}
}

// TestRunAppHealthOnceContextProbeTemplateSkipsNonActiveAndEmptyModelName
// proves the {model}-template branch's other two skip terms survive the
// mp.MetricsLocked removal above: a non-active mapping and one with an empty
// upstream model name must still never be probed at all. m-a is a control
// mapping that IS probed, proving the branch runs at all rather than
// short-circuiting for some unrelated reason.
//
// m-b is seeded into the loaded set precisely so the non-active assertion is
// not vacuous: dropping the `mp.Status != routing.ServerStatusActive` term
// alone (verified by revert-testing) makes this test fail, because m-b would
// then actually reach ProbeModelInfo.
//
// The empty-AppModelName case cannot be independently forced the same way:
// gateway.LoadedModelRegistry.normalizeModelSet unconditionally drops ""
// from any seeded loaded set (see internal/gateway/loaded_models.go), so an
// empty AppModelName can never be a member of loadedSet regardless of the
// explicit `mp.AppModelName == ""` term -- the loadedSet membership check a
// few lines below already backstops it. Revert-testing confirmed this:
// removing the explicit term alone does NOT make this test fail. The
// assertion below still documents and verifies the required, observable
// behavior (no probe, no write for an empty upstream model name); the
// explicit term itself is intentional defense-in-depth against a future
// change to the loaded-set gating, not something this test path can
// independently falsify.
func TestRunAppHealthOnceContextProbeTemplateSkipsNonActiveAndEmptyModelName(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {
			{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive},
			{ID: "m2", ApplicationID: "a1", GatewayModelName: "g-b", AppModelName: "m-b", Status: routing.ServerStatusDisabled},
			{ID: "m3", ApplicationID: "a1", GatewayModelName: "g-c", AppModelName: "", Status: routing.ServerStatusActive},
		},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	prober.modelInfoByPath["/upstream/m-b/props"] = []provider.ModelInfo{{Name: "m-b", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a", "m-b"}) // seeding "" here would be a no-op: normalizeModelSet drops it

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/m-a/props") {
		t.Fatalf("control mapping m-a must be probed")
	}
	if prober.probedPath("/upstream/m-b/props") {
		t.Fatalf("a non-active (disabled) mapping must NEVER be probed")
	}
	if prober.probedPath("/upstream//props") {
		t.Fatalf("a mapping with an empty upstream model name must NEVER be probed")
	}
	if n := prober.ctxProbeCallCount(); n != 1 {
		t.Fatalf("ProbeModelInfo called %d times, want 1 (only the active, named, loaded mapping)", n)
	}
	gotB, _ := st.mappingOf("m2")
	if gotB.LiveProgressSupport != "" {
		t.Fatalf("m-b LiveProgressSupport = %q, want %q (non-active mapping must not be probed nor written)", gotB.LiveProgressSupport, "")
	}
	gotC, _ := st.mappingOf("m3")
	if gotC.LiveProgressSupport != "" {
		t.Fatalf("m-c LiveProgressSupport = %q, want %q (empty upstream model name must not be probed nor written)", gotC.LiveProgressSupport, "")
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	samples := st.availSamples()
	if len(samples) != 1 {
		t.Fatalf("availability samples after the first cycle = %d, want 1", len(samples))
	}
	if s0 := samples[0]; s0.Health != routing.HealthHealthy || !s0.AgentReporting || s0.ActiveCount != 1 || s0.ReachableCount != 1 {
		t.Fatalf("first sample = %+v, want Health=healthy AgentReporting=true ActiveCount=1 ReachableCount=1", s0)
	}

	// 2) Second cycle, same clock + unchanged state -> no new sample (not due, no change).
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 1 {
		t.Fatalf("availability samples after an unchanged cycle = %d, want 1 (deduped)", n)
	}

	// 3) Advance past the heartbeat with the same state -> one periodic heartbeat sample.
	current = base.Add(200 * time.Millisecond)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 2 {
		t.Fatalf("availability samples after the heartbeat = %d, want 2", n)
	}

	// 4) Agent stops reporting (state transition) BEFORE the next heartbeat is due ->
	// a new sample purely because the state changed. Evict s1 from the presence
	// registry so Reporting("s1") flips to false without waiting out the window.
	presence.Retain(map[string]struct{}{})
	current = base.Add(210 * time.Millisecond) // < heartbeat since the 200ms write
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
	if n := len(st.availSamples()); n != 1 {
		t.Fatalf("availability samples after the first cycle = %d, want 1", n)
	}

	// 2) Advance EXACTLY availabilityHeartbeat since the last write, state unchanged
	// -> the heartbeat is due at the inclusive boundary (tNow.Sub(prev.at) == hb).
	current = base.Add(availabilityHeartbeat)
	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: presenceBundle{presence}, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(context.Background(), &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})
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
	(&appHealthRunner{store: st1, prober: newFakeProber(), syncer: syncer1, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: presenceBundle{presence1}, groups: nil, settings: st1, probeTimeout: time.Second, cipher: nil, now: func() time.Time { return fixed1 }}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
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
	(&appHealthRunner{store: st2, prober: newFakeProber(), syncer: syncer2, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: presenceBundle{presence2}, groups: nil, settings: st2, probeTimeout: time.Second, cipher: nil, now: func() time.Time { return fixed2 }}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})
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

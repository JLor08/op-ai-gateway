// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
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

	mu               sync.Mutex
	health           map[string]string
	mappings         map[string][]routing.ModelMapping // keyed by application id
	ctxProbeSets     int                               // count of UpdateMappingContextProbe calls
	capabilityWrites int                               // count of UpsertMappingCapabilities calls
	// caps is the model_mapping_capabilities child table, keyed by mapping id
	// then capability name -- a mapping with no entry has every capability
	// UNKNOWN, which is what the absence of a row means (routing.CapabilityRow).
	caps            map[string]map[string]routing.CapabilityRow
	capsReadErr     bool                               // when true, MappingCapabilities errors
	availSamplesLog []routing.ServerAvailabilitySample // append-ordered availability samples
	failInsert      bool                               // when true, InsertServerAvailabilitySample errors
	// runtimeSpecs backs RuntimeSpecsByApplication (issue #58 per-mapping
	// upstream credentials); specsErr, when set, makes the call fail instead
	// (exercising the fallback-to-app-token degrade path).
	runtimeSpecs []routing.RuntimeSpec
	specsErr     bool
	// cache is the model_mapping_cache table (#251), keyed by mapping id then
	// by (cache type, scope key); cacheWrites counts UpsertModelMappingCache
	// calls (the no-write-amplification spy), and cacheReadErr / cacheWriteErr
	// make the corresponding call fail.
	cache         map[string]map[modelMappingCacheKey]routing.ModelMappingCacheRow
	cacheWrites   int
	cacheWritesBy map[string]int // the same count split by cache type
	cacheReadErr  bool
	cacheWriteErr bool
}

// modelMappingCacheKey is the (cache type, scope key) half of the cache table's
// primary key; the mapping id is the outer map's key.
type modelMappingCacheKey struct{ cacheType, scopeKey string }

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

// MappingCapabilities mirrors the store contract (#49-3): one mapping's
// capability rows, ordered by capability, and an EMPTY slice for a mapping
// with none -- every capability simply unknown. capsReadErr makes it fail
// instead, so a test can pin the write path's best-effort behaviour when the
// baseline cannot be read.
func (f *fakeHealthStore) MappingCapabilities(_ context.Context, mappingID string) ([]routing.CapabilityRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.capsReadErr {
		return nil, fmt.Errorf("mapping capabilities: boom")
	}
	out := make([]routing.CapabilityRow, 0, len(f.caps[mappingID]))
	for _, row := range f.caps[mappingID] {
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Capability < out[j].Capability })
	return out, nil
}

// UpsertMappingCapabilities mirrors the store contract: rows REPLACE any row
// for the same (mapping, capability), every row is validated before anything
// is written (routing.ValidateCapabilityRow -- so a caller handing over an
// empty verdict fails here exactly as it would against a real driver), and
// there is NO metrics_locked guard, matching both drivers.
func (f *fakeHealthStore) UpsertMappingCapabilities(_ context.Context, mappingID string, rows []routing.CapabilityRow) error {
	for _, r := range rows {
		if err := routing.ValidateCapabilityRow(r); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.capabilityWrites++
	if len(rows) == 0 {
		return nil
	}
	if f.caps == nil {
		f.caps = map[string]map[string]routing.CapabilityRow{}
	}
	if f.caps[mappingID] == nil {
		f.caps[mappingID] = map[string]routing.CapabilityRow{}
	}
	for _, r := range rows {
		f.caps[mappingID][r.Capability] = r
	}
	return nil
}

// capabilityWriteCount returns how many times UpsertMappingCapabilities was
// called -- the call-count spy the no-write-amplification and precedence
// assertions turn on: a probe that correctly declines to write and one that
// rewrites an identical value reach the same stored state, so only the count
// distinguishes them.
func (f *fakeHealthStore) capabilityWriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.capabilityWrites
}

// seedCapability puts one capability row on file, standing in for whatever
// established it -- an operator (manual), the vision benchmark, migration
// 78's legacy inheritance, or an earlier probe.
func (f *fakeHealthStore) seedCapability(mappingID, capability, verdict, source string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.caps == nil {
		f.caps = map[string]map[string]routing.CapabilityRow{}
	}
	if f.caps[mappingID] == nil {
		f.caps[mappingID] = map[string]routing.CapabilityRow{}
	}
	f.caps[mappingID][capability] = routing.CapabilityRow{
		Capability: capability, Verdict: verdict, Source: source,
		CheckedAt: time.Now().UTC().Add(-time.Hour),
	}
}

// capabilityOf reads one stored capability row back. ok is false when the
// capability has NO row, which is how "unknown" is expressed -- so a test
// telling "never determined" from a definitive verdict must look at the bool,
// not at an empty verdict string.
func (f *fakeHealthStore) capabilityOf(mappingID, capability string) (routing.CapabilityRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.caps[mappingID][capability]
	return row, ok
}

// assertCapability fails unless mappingID's capability row holds exactly this
// verdict and source. The SOURCE half is load-bearing: it is what the
// precedence rule reads, so a test checking only the verdict could not tell a
// probe's write from an operator's surviving one.
func (f *fakeHealthStore) assertCapability(t *testing.T, mappingID, capability, wantVerdict, wantSource string) {
	t.Helper()
	row, ok := f.capabilityOf(mappingID, capability)
	if !ok {
		t.Fatalf("%s has NO %q row, want %s/%s", mappingID, capability, wantVerdict, wantSource)
	}
	if row.Verdict != wantVerdict || row.Source != wantSource {
		t.Fatalf("%s %q row = %s/%s, want %s/%s", mappingID, capability, row.Verdict, row.Source, wantVerdict, wantSource)
	}
}

// ModelMappingCache mirrors the store contract: one mapping's cache rows of one
// cache type ordered by scope key, and an EMPTY (non-nil) slice for a mapping
// with none. cacheReadErr makes it fail instead.
func (f *fakeHealthStore) ModelMappingCache(_ context.Context, mappingID, cacheType string) ([]routing.ModelMappingCacheRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cacheReadErr {
		return nil, fmt.Errorf("model mapping cache: boom")
	}
	out := []routing.ModelMappingCacheRow{}
	for key, row := range f.cache[mappingID] {
		if key.cacheType == cacheType {
			out = append(out, row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ScopeKey < out[j].ScopeKey })
	return out, nil
}

// UpsertModelMappingCache mirrors the store contract: the row is validated
// (routing.ValidateModelMappingCacheRow) before anything is written, and it
// REPLACES any row for the same (mapping, cache type, scope key). Every call is
// counted, a failing one included, so cacheWriteCount is a spy on what the loop
// ATTEMPTED.
func (f *fakeHealthStore) UpsertModelMappingCache(_ context.Context, row routing.ModelMappingCacheRow) error {
	if err := routing.ValidateModelMappingCacheRow(row); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cacheWrites++
	if f.cacheWritesBy == nil {
		f.cacheWritesBy = map[string]int{}
	}
	f.cacheWritesBy[row.CacheType]++
	if f.cacheWriteErr {
		return fmt.Errorf("model mapping cache: boom")
	}
	if f.cache == nil {
		f.cache = map[string]map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	if f.cache[row.MappingID] == nil {
		f.cache[row.MappingID] = map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	f.cache[row.MappingID][modelMappingCacheKey{row.CacheType, row.ScopeKey}] = row
	return nil
}

// cacheWriteCount returns how many times UpsertModelMappingCache was called --
// the spy the change-detection assertions turn on: a loop that correctly skips
// an unchanged payload and one that rewrites it reach the same stored state, so
// only the count tells them apart.
func (f *fakeHealthStore) cacheWriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cacheWrites
}

// cacheWriteCountOf is cacheWriteCount for ONE cache type: the audio.cpp pass writes
// several rows per loaded tts mapping (voices, speech_language), and a test about one
// of them must not count the others.
func (f *fakeHealthStore) cacheWriteCountOf(cacheType string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cacheWritesBy[cacheType]
}

// seedVoicesCache puts a voices row on file, standing in for an earlier write.
// Its UpdatedAt is an hour old so a test can tell a rewrite from the original.
func (f *fakeHealthStore) seedVoicesCache(mappingID, payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	if f.cache[mappingID] == nil {
		f.cache[mappingID] = map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	f.cache[mappingID][modelMappingCacheKey{routing.ModelMappingCacheVoices, ""}] = routing.ModelMappingCacheRow{
		MappingID: mappingID, CacheType: routing.ModelMappingCacheVoices, Payload: payload,
		UpdatedAt: time.Now().UTC().Add(-time.Hour),
	}
}

// voicesRowOf reads one mapping's stored voices row back. ok is false when the
// mapping has NO such row, which is how "never fetched" differs from "fetched
// and empty" (a row whose payload is "[]").
func (f *fakeHealthStore) voicesRowOf(mappingID string) (routing.ModelMappingCacheRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.cache[mappingID][modelMappingCacheKey{routing.ModelMappingCacheVoices, ""}]
	return row, ok
}

// assertVoicesPayload fails unless mappingID has a voices row holding exactly
// this payload.
func (f *fakeHealthStore) assertVoicesPayload(t *testing.T, mappingID, wantPayload string) {
	t.Helper()
	row, ok := f.voicesRowOf(mappingID)
	if !ok {
		t.Fatalf("%s has NO voices row, want payload %s", mappingID, wantPayload)
	}
	if row.Payload != wantPayload {
		t.Fatalf("%s voices payload = %s, want %s", mappingID, row.Payload, wantPayload)
	}
}

// seedCacheRow puts one scope-"" cache row of any type on file, standing in for an
// earlier write (a benchmark's rate and identity, say). It is not counted as a write.
func (f *fakeHealthStore) seedCacheRow(mappingID, cacheType, payload string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cache == nil {
		f.cache = map[string]map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	if f.cache[mappingID] == nil {
		f.cache[mappingID] = map[modelMappingCacheKey]routing.ModelMappingCacheRow{}
	}
	f.cache[mappingID][modelMappingCacheKey{cacheType, ""}] = routing.ModelMappingCacheRow{
		MappingID: mappingID, CacheType: cacheType, Payload: payload, UpdatedAt: time.Now().UTC().Add(-time.Hour),
	}
}

// cachePayloadOf reads one mapping's scope-"" row of a cache type back. ok is false
// when there is no such row.
func (f *fakeHealthStore) cachePayloadOf(mappingID, cacheType string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.cache[mappingID][modelMappingCacheKey{cacheType, ""}]
	return row.Payload, ok
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
	// loadedErr marks endpoints whose loaded-model probe should fail;
	// loadedCalls counts every LoadedModels call.
	loaded      map[string][]string
	loadedErr   map[string]bool
	loadedCalls int
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
	f.loadedCalls++
	if f.loadedErr[target.Endpoint] {
		return nil, fmt.Errorf("loaded probe failed: %s", target.Endpoint)
	}
	return f.loaded[target.Endpoint], nil
}

// loadedCallCount returns how many times LoadedModels was called.
func (f *fakeProber) loadedCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadedCalls
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
// internal/gateway/agent_ingest.go. The verdict now lands as the mapping's
// routing.CapabilityLiveProgress ROW -- #49-3 gave it no writer of its own --
// so the spy is capabilityWriteCount, mirroring the repo's existing
// ctxProbeSetCount spy.
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

	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state (as if the next tick's interval
	// had elapsed) reports the SAME verdict -- it must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times across two identical cycles, want 1 (an unchanged verdict must not be rewritten)", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateCapabilitiesPersistOnceNotAgainOnAnIdenticalCycle
// is TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAnIdenticalCycle's
// counterpart for the capability write (#49-2) on the {model}-template branch:
// a llama.cpp /props body carrying modalities + chat_template_caps evidence
// must be persisted on the first cycle as one row per capability, and a
// second, otherwise-identical cycle must NOT reissue the write, since the
// stored verdicts already match -- the same no-rewrite property
// applyCapabilityWrite documents. Uses the call-count spy
// (capabilityWriteCount), which is what distinguishes "correctly declined"
// from "rewrote the same value".
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

	st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	st.assertCapability(t, "m1", routing.CapabilityTools, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state reports the SAME verdicts --
	// they must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times across two identical cycles, want 1 (an unchanged capability set must not be rewritten)", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateAllEmptyCapabilitiesNeverWrite proves
// an all-empty verdict set (detection ran, determined nothing -- e.g. a body
// with no modalities/chat_template_caps objects at all) never calls
// UpsertMappingCapabilities: probedCapabilityRows yields no rows at all, so
// applyCapabilityWrite returns before even READING the stored baseline --
// "unknown" is the absence of a row, and there is nothing to write for it.
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

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times for an all-empty verdict set, want 0", n)
	}
	for _, capability := range []string{
		routing.CapabilityVision, routing.CapabilityVideo,
		routing.CapabilityAudio, routing.CapabilityTools, routing.CapabilityLiveProgress,
	} {
		if row, ok := st.capabilityOf("m1", capability); ok {
			t.Fatalf("m1 has a %q row (%+v), want none -- every capability must stay UNKNOWN", capability, row)
		}
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

	if n := st.capabilityWriteCount(); n != 2 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 2 -- a nameless verdict belongs to every mapping of this one-endpoint application", n)
	}
	for _, id := range []string{"m1", "m2"} {
		st.assertCapability(t, id, routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
		got, _ := st.mappingOf(id)
		if got.ContextSize != 0 {
			t.Fatalf("%s ContextSize = %d, want 0 (a nameless entry reports no size, so nothing may be attributed)", id, got.ContextSize)
		}
	}
}

// capabilityProbeRunner builds a {model}-template app-health runner over one
// active mapping ("m1"/"m-a") whose /props probe reports caps, plus the store
// it writes through -- the shared fixture for the precedence tests below,
// which differ only in what is already ON FILE for that mapping.
func capabilityProbeRunner(t *testing.T, caps provider.Capabilities) (*appHealthRunner, *fakeHealthStore) {
	t.Helper()
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a", Status: routing.ServerStatusActive}},
	}
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, Caps: caps}}
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})
	runner := &appHealthRunner{
		store: st, prober: prober, syncer: nil, registry: gateway.NewAppHealthRegistry(nil),
		loaded: loaded, agents: nil, groups: nil, settings: st,
		probeTimeout: time.Second, cipher: nil, now: time.Now,
	}
	return runner, st
}

// runCapabilityCycle runs one full app-health cycle with a fresh cadence
// state -- what the ~30s tick does, and what these tests count writes across.
func runCapabilityCycle(runner *appHealthRunner) {
	runner.runOnce(context.Background(), &cycleState{
		lastProbed: map[string]time.Time{},
		lastAvail:  make(map[string]availWriteState),
	})
}

// TestRunAppHealthOnceProbeDoesNotOverwriteAManualVerdict is the operator's
// rule on this pass: a verdict a HUMAN established survives a probe reporting
// the opposite, forever, without the operator having to lock anything.
// Provenance replaces the lock -- see
// routing.MappingStore.UpsertMappingCapabilities.
//
// It matters more here than on the telemetry path: this pass re-reads the same
// /props document every ~30 seconds for the life of the deployment, so a
// missing precedence check would not merely lose the operator's answer once,
// it would keep overwriting it.
//
// Unfalsifiable by accident: the same probe response also carries an
// UNMANAGED verdict (tools), so exactly one write must fire and must carry
// only that row. A test that merely counted writes at zero would also pass if
// the write path had stopped working altogether.
func TestRunAppHealthOnceProbeDoesNotOverwriteAManualVerdict(t *testing.T) {
	runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes", Tools: "yes"})
	st.seedCapability("m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceManual)

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1 (the unmanaged tools verdict)", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceManual)
	st.assertCapability(t, "m1", routing.CapabilityTools, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestRunAppHealthOnceProbeDoesNotOverwriteABenchmarkVerdict is the same rule
// for the other authoritative source: the vision benchmark MEASURED this --
// it sent a real image to the real upstream and read the real answer -- so a
// /props re-read must not talk over it either. Unfalsifiable the same way as
// the manual sibling.
func TestRunAppHealthOnceProbeDoesNotOverwriteABenchmarkVerdict(t *testing.T) {
	runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes", Tools: "yes"})
	st.seedCapability("m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceVisionBenchmark)

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1 (the unmanaged tools verdict)", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceVisionBenchmark)
	st.assertCapability(t, "m1", routing.CapabilityTools, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
}

// TestRunAppHealthOnceProbeOverwritesItsOwnAndLegacyVerdicts is precedence's
// other half, and it is what keeps the rule from degenerating into "never
// write anything": a row whose source is a PROBE source is overwritable,
// which is how a verdict re-establishes itself after an operator swaps the
// upstream binary. legacy (migration 78's inheritance from the pre-78
// columns) counts as a probe source deliberately -- its real origin is
// unknowable, and treating a guess as authoritative would freeze it in
// forever.
func TestRunAppHealthOnceProbeOverwritesItsOwnAndLegacyVerdicts(t *testing.T) {
	t.Run("a differing llama_cpp_props row is replaced", func(t *testing.T) {
		runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes"})
		st.seedCapability("m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)

		runCapabilityCycle(runner)

		if n := st.capabilityWriteCount(); n != 1 {
			t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1 -- a probe's own earlier verdict is overwritable", n)
		}
		st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("a differing legacy row is replaced", func(t *testing.T) {
		runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes"})
		st.seedCapability("m1", routing.CapabilityVision, routing.CapabilityNo, routing.CapabilitySourceLegacy)

		runCapabilityCycle(runner)

		if n := st.capabilityWriteCount(); n != 1 {
			t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1 -- a legacy verdict's real origin is unknowable, so it must stay probe-overwritable", n)
		}
		st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})

	t.Run("an agreeing probe row is not rewritten", func(t *testing.T) {
		runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes"})
		st.seedCapability("m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)

		runCapabilityCycle(runner)

		if n := st.capabilityWriteCount(); n != 0 {
			t.Fatalf("UpsertMappingCapabilities called %d times, want 0 -- the stored verdict already agrees, and this pass runs on the app's cadence forever", n)
		}
		st.assertCapability(t, "m1", routing.CapabilityVision, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	})
}

// TestRunAppHealthOnceCapabilityReadFailureWritesNothing pins the write
// path's best-effort behaviour when the STORED baseline cannot be read: the
// precedence rule and change detection are both judged against those rows, so
// a probe with no baseline must decline rather than guess -- writing blind
// would be exactly the overwrite the operator's rule forbids. It logs and the
// cycle carries on; nothing about a store hiccup may fail the health pass.
func TestRunAppHealthOnceCapabilityReadFailureWritesNothing(t *testing.T) {
	runner, st := capabilityProbeRunner(t, provider.Capabilities{Vision: "yes"})
	st.capsReadErr = true

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times after a failed baseline read, want 0 -- writing blind could overwrite a verdict a human established", n)
	}
	if !strings.Contains(buf.String(), "capability read for mapping m1 failed") {
		t.Fatalf("log output = %q, want the capability-read failure logged", buf.String())
	}
	// The context-size write is independent and must still land, proving the
	// probe itself ran and that one failing read does not stall the pass.
	got, _ := st.mappingOf("m1")
	if got.ContextSize != 8192 {
		t.Fatalf("ContextSize = %d, want 8192 (a capability-read failure must not stall the rest of the pass)", got.ContextSize)
	}
}

// sdcppFakeProber is the file's fakeProber plus provider.SdcppCapabilitiesProber
// -- the smallest addition that lets the stable_diffusion_cpp branch of the
// probe pass be observed. Embedding keeps every capability the production
// Multiplexer also has (Probe, LoadedModels, ProbeModelInfo), so a test here
// sees the same assertions succeed or fail as production does; the existing
// fakeProber is left untouched, so no existing test gains a capability it did
// not have.
type sdcppFakeProber struct {
	*fakeProber
	sdMu sync.Mutex
	// sdVerdicts / sdErr answer ProbeSdcppCapabilities per endpoint. An
	// endpoint with an error STILL returns its verdicts beside it, so a branch
	// that ignored the error would write them -- which is what makes "an
	// error writes nothing" observable rather than true by construction.
	sdVerdicts map[string]provider.SdcppVerdicts
	sdErr      map[string]error
	// sdTargets records every target asked, in order; sdAuth the credential
	// each call carried (provider.UpstreamAuthFrom), so a test can pin that
	// the branch builds its auth context like the context-probe branch.
	sdTargets []routing.Target
	sdAuth    []provider.UpstreamAuth
	// sdOptions / sdOptErr answer ProbeSdcppImageOptions per endpoint, with
	// the same "options STILL come back beside an error" rule as sdVerdicts;
	// sdOptTargets / sdOptAuth record every options probe asked, so a test can
	// pin that it reuses the verdict probe's target and credential.
	sdOptions    map[string]provider.SdcppImageOptions
	sdOptErr     map[string]error
	sdOptTargets []routing.Target
	sdOptAuth    []provider.UpstreamAuth
}

var (
	_ provider.SdcppCapabilitiesProber = (*sdcppFakeProber)(nil)
	_ provider.SdcppImageOptionsProber = (*sdcppFakeProber)(nil)
)

func newSdcppFakeProber() *sdcppFakeProber {
	return &sdcppFakeProber{
		fakeProber: newFakeProber(),
		sdVerdicts: map[string]provider.SdcppVerdicts{},
		sdErr:      map[string]error{},
		sdOptions:  map[string]provider.SdcppImageOptions{},
		sdOptErr:   map[string]error{},
	}
}

func (f *sdcppFakeProber) ProbeSdcppCapabilities(ctx context.Context, target routing.Target) (provider.SdcppVerdicts, error) {
	f.sdMu.Lock()
	defer f.sdMu.Unlock()
	f.sdTargets = append(f.sdTargets, target)
	a, _ := provider.UpstreamAuthFrom(ctx)
	f.sdAuth = append(f.sdAuth, a)
	return f.sdVerdicts[target.Endpoint], f.sdErr[target.Endpoint]
}

func (f *sdcppFakeProber) ProbeSdcppImageOptions(ctx context.Context, target routing.Target) (provider.SdcppImageOptions, error) {
	f.sdMu.Lock()
	defer f.sdMu.Unlock()
	f.sdOptTargets = append(f.sdOptTargets, target)
	a, _ := provider.UpstreamAuthFrom(ctx)
	f.sdOptAuth = append(f.sdOptAuth, a)
	return f.sdOptions[target.Endpoint], f.sdOptErr[target.Endpoint]
}

// sdcppOptionCalls returns a copy of every target ProbeSdcppImageOptions was asked.
func (f *sdcppFakeProber) sdcppOptionCalls() []routing.Target {
	f.sdMu.Lock()
	defer f.sdMu.Unlock()
	return append([]routing.Target(nil), f.sdOptTargets...)
}

// sdcppCalls returns a copy of every target ProbeSdcppCapabilities was asked.
func (f *sdcppFakeProber) sdcppCalls() []routing.Target {
	f.sdMu.Lock()
	defer f.sdMu.Unlock()
	return append([]routing.Target(nil), f.sdTargets...)
}

// sdcppApp is an external stable-diffusion.cpp application as the portal
// creates one: the type's /v1/models health path and NO ContextProbePath,
// which is exactly why the capability probe cannot ride the context branch.
func sdcppApp(id, serverID string, port int) routing.Application {
	app := activeApp(id, serverID, port)
	app.Type = routing.ProviderStableDiffusionCpp
	app.HealthCheckPath = "/v1/models"
	return app
}

// sdcppProbeRunner builds a runner over one stable_diffusion_cpp application
// ("a1" on http://s1.local:8001) with the given mappings, whose capability
// document answers v.
func sdcppProbeRunner(t *testing.T, v provider.SdcppVerdicts, mappings ...routing.ModelMapping) (*appHealthRunner, *fakeHealthStore, *sdcppFakeProber) {
	t.Helper()
	shrinkRetryGap(t)
	st := newHealthTestStore(sdcppApp("a1", "s1", 8001))
	st.mappings = map[string][]routing.ModelMapping{"a1": mappings}
	prober := newSdcppFakeProber()
	prober.sdVerdicts["http://s1.local:8001"] = v
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}
	return runner, st, prober
}

func sdcppMapping(id, appModel string) routing.ModelMapping {
	return routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: appModel, Status: routing.ServerStatusActive}
}

// TestRunAppHealthOnceSdcppWritesImageVerdict is the branch's whole job: the
// gateway derives an external sd-server's image verdict from the server's own
// capability document, at rank 1 under source sdcpp_capabilities, so the
// images gate stops needing a manual verdict for it.
//
// Both directions are asserted, because supported_modes is exhaustive and this
// is the first source entitled to state image: no. The verdict names its model
// (model.stem), so it lands ONLY on the mapping of that name: a sibling mapping
// of another name must get no row at all -- asserted by presence, not by
// verdict, since "unknown" is the absence of a row.
func TestRunAppHealthOnceSdcppWritesImageVerdict(t *testing.T) {
	for _, verdict := range []string{routing.CapabilityYes, routing.CapabilityNo} {
		t.Run(verdict, func(t *testing.T) {
			runner, st, prober := sdcppProbeRunner(t,
				provider.SdcppVerdicts{Image: verdict, ModelStem: "flux1-dev"},
				sdcppMapping("m1", "flux1-dev"), sdcppMapping("m2", "sd-other"))

			runCapabilityCycle(runner)

			st.assertCapability(t, "m1", routing.CapabilityImage, verdict, routing.CapabilitySourceSdcppCapabilities)
			if row, ok := st.capabilityOf("m2", routing.CapabilityImage); ok {
				t.Fatalf("m2 (AppModelName sd-other) got an image row %+v -- a verdict naming flux1-dev belongs to that mapping only", row)
			}
			if n := st.capabilityWriteCount(); n != 1 {
				t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1", n)
			}
			calls := prober.sdcppCalls()
			if len(calls) != 1 || calls[0].Provider != routing.ProviderStableDiffusionCpp || calls[0].Endpoint != "http://s1.local:8001" || calls[0].Timeout != time.Second {
				t.Fatalf("sdcpp probe targets = %+v, want one {Provider:stable_diffusion_cpp Endpoint:http://s1.local:8001 Timeout:1s}", calls)
			}
			// Independent of ContextProbePath (empty for this type) and of the
			// ModelInfoProber path: the /props probe never ran.
			if n := prober.ctxProbeCallCount(); n != 0 {
				t.Fatalf("ProbeModelInfo called %d times, want 0 -- the sd branch must not depend on the context probe", n)
			}
		})
	}
}

// TestRunAppHealthOnceSdcppNamelessVerdictReachesEveryActiveMapping is the
// context branch's name rule, applied here: a document naming no model yields
// a verdict with no stem, and a stable_diffusion_cpp application has exactly
// one endpoint serving one model, so every ACTIVE mapping it owns is served
// by the build that answered. A disabled mapping is not written.
func TestRunAppHealthOnceSdcppNamelessVerdictReachesEveryActiveMapping(t *testing.T) {
	disabled := sdcppMapping("m3", "sd-disabled")
	disabled.Status = routing.ServerStatusDisabled
	runner, st, _ := sdcppProbeRunner(t,
		provider.SdcppVerdicts{Image: routing.CapabilityYes},
		sdcppMapping("m1", "flux1-dev"), sdcppMapping("m2", "sd-other"), disabled)

	runCapabilityCycle(runner)

	for _, id := range []string{"m1", "m2"} {
		st.assertCapability(t, id, routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)
	}
	if row, ok := st.capabilityOf("m3", routing.CapabilityImage); ok {
		t.Fatalf("disabled m3 got an image row %+v, want none", row)
	}
	if n := st.capabilityWriteCount(); n != 2 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 2", n)
	}
}

// TestRunAppHealthOnceSdcppUnmatchedStemIsLoggedAtDebug: a verdict naming a
// model that no active mapping of the application serves -- an sd-server whose
// model.stem differs from the model_name discovery named the mapping by --
// fails closed (no row), and one debug line names the app, the stem and the
// verdict so the case is diagnosable. A matched verdict does not log it.
func TestRunAppHealthOnceSdcppUnmatchedStemIsLoggedAtDebug(t *testing.T) {
	const msg = "no active mapping serves the model it names"
	for _, tc := range []struct {
		name, stem string
		wantLog    bool
	}{
		{"unmatched stem logs", "flux1-schnell", true},
		{"matched stem does not", "flux1-dev", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
			t.Cleanup(func() { slog.SetDefault(prev) })
			runner, st, _ := sdcppProbeRunner(t,
				provider.SdcppVerdicts{Image: routing.CapabilityYes, ModelStem: tc.stem},
				sdcppMapping("m1", "flux1-dev"))

			runCapabilityCycle(runner)

			out := buf.String()
			if got := strings.Contains(out, msg); got != tc.wantLog {
				t.Fatalf("log contains %q = %v, want %v; log:\n%s", msg, got, tc.wantLog, out)
			}
			if tc.wantLog {
				for _, want := range []string{"app_id=a1", "model_stem=" + tc.stem, "image=yes"} {
					if !strings.Contains(out, want) {
						t.Fatalf("log lacks %q; log:\n%s", want, out)
					}
				}
				if n := st.capabilityWriteCount(); n != 0 {
					t.Fatalf("UpsertMappingCapabilities called %d times, want 0 -- an unmatched verdict writes nothing", n)
				}
			}
		})
	}
}

// TestRunAppHealthOnceSdcppDoesNotOverwriteAManualVerdict pins the operator's
// precedence rule: a probe (rank 1) never overwrites a manual (rank 3)
// verdict, in either direction. One case is a stored manual "no" against a
// probe "yes"; the portal reserves (image, no) from new manual
// writes, so the case an operator can actually produce today is the other
// one -- a manual "yes" (the only enablement path before this probe existed)
// against a probe "no" -- and both are pinned.
//
// Unfalsifiable by accident, as the /props sibling is: the verdict carries no
// stem, so it also reaches a second mapping with nothing on file, and exactly
// that one write must fire. A write count of zero alone would also pass if
// the branch had stopped writing altogether.
func TestRunAppHealthOnceSdcppDoesNotOverwriteAManualVerdict(t *testing.T) {
	for _, tc := range []struct{ name, manual, probe string }{
		{"manual no survives a probe yes", routing.CapabilityNo, routing.CapabilityYes},
		{"manual yes survives a probe no", routing.CapabilityYes, routing.CapabilityNo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, _ := sdcppProbeRunner(t,
				provider.SdcppVerdicts{Image: tc.probe},
				sdcppMapping("m1", "flux1-dev"), sdcppMapping("m2", "sd-other"))
			st.seedCapability("m1", routing.CapabilityImage, tc.manual, routing.CapabilitySourceManual)

			runCapabilityCycle(runner)

			st.assertCapability(t, "m1", routing.CapabilityImage, tc.manual, routing.CapabilitySourceManual)
			st.assertCapability(t, "m2", routing.CapabilityImage, tc.probe, routing.CapabilitySourceSdcppCapabilities)
			if n := st.capabilityWriteCount(); n != 1 {
				t.Fatalf("UpsertMappingCapabilities called %d times, want exactly 1 (m2's first verdict)", n)
			}
		})
	}
}

// TestRunAppHealthOnceSdcppNeverAsksAnotherType pins that the capability
// document is asked of stable_diffusion_cpp applications only: every other
// type -- including a llama_cpp application WITH a context probe path and a
// server_agent one, whose own passes run beside this one -- is never
// asked. The sd application in the same store is the positive control: it is
// asked exactly once, so a branch that asked nobody would fail here too.
func TestRunAppHealthOnceSdcppNeverAsksAnotherType(t *testing.T) {
	shrinkRetryGap(t)
	apps := []routing.Application{sdcppApp("sd", "s1", 8000)}
	for i, typ := range []string{
		routing.ProviderVLLM, routing.ProviderLlamaCPP, routing.ProviderLlamaSwap,
		routing.ProviderLiteLLM, routing.ProviderOllama, routing.ProviderServerAgent, routing.ProviderMock,
	} {
		app := activeApp("other-"+typ, "s1", 8100+i)
		app.Type = typ
		if typ == routing.ProviderLlamaCPP {
			app.ContextProbePath = "/props"
		}
		apps = append(apps, app)
	}
	st := newHealthTestStore(apps...)
	prober := newSdcppFakeProber()
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	calls := prober.sdcppCalls()
	if len(calls) != 1 || calls[0].Provider != routing.ProviderStableDiffusionCpp || calls[0].Endpoint != "http://s1.local:8000" {
		t.Fatalf("sdcpp probe targets = %+v, want exactly the one stable_diffusion_cpp application", calls)
	}
	if !prober.probedPath("/props") {
		t.Fatalf("the llama_cpp application's context probe did not run -- the sd branch must not displace the context branch")
	}
}

// TestRunAppHealthOnceSdcppProbeErrorWritesNothing: a failed probe -- here the
// 404 an sd-server without the document answers -- states no verdict, so no
// row may be written and nothing already on file may change. The fake hands a
// "no" back beside the error, and the stored row is a same-rank "yes" the "no"
// would be allowed to overwrite, so only the error guard stands between them.
func TestRunAppHealthOnceSdcppProbeErrorWritesNothing(t *testing.T) {
	runner, st, prober := sdcppProbeRunner(t, provider.SdcppVerdicts{Image: routing.CapabilityNo}, sdcppMapping("m1", "flux1-dev"))
	prober.sdErr["http://s1.local:8001"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)
	st.seedCapability("m1", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times after a failed probe, want 0", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)
}

// TestRunAppHealthOnceSdcppFollowsTheAppCadence pins the branch's own schedule
// key ("sdcpp:"+app.ID): with the clock standing still a second cycle does not
// ask again, and once the application's interval has elapsed it does. A
// branch sharing the health probe's key (app.ID) would never be due at all --
// the health pass stamps that key first in the same cycle -- which
// TestRunAppHealthOnceSdcppWritesImageVerdict would catch; this one catches a
// branch with no schedule, asking on every cycle.
func TestRunAppHealthOnceSdcppFollowsTheAppCadence(t *testing.T) {
	runner, _, prober := sdcppProbeRunner(t, provider.SdcppVerdicts{Image: routing.CapabilityYes}, sdcppMapping("m1", "flux1-dev"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)}

	runner.runOnce(context.Background(), state)
	runner.runOnce(context.Background(), state)
	if n := len(prober.sdcppCalls()); n != 1 {
		t.Fatalf("sdcpp probe asked %d times over two cycles at one instant, want 1 -- it must follow the application's cadence", n)
	}
	if _, ok := state.lastProbed["sdcpp:a1"]; !ok {
		t.Fatalf("lastProbed = %v, want an sdcpp:a1 entry", state.lastProbed)
	}

	current = current.Add(time.Duration(portal.MaxHealthCheckIntervalSeconds) * time.Second)
	runner.runOnce(context.Background(), state)
	if n := len(prober.sdcppCalls()); n != 2 {
		t.Fatalf("sdcpp probe asked %d times after the interval elapsed, want 2", n)
	}
}

// TestRunAppHealthOnceSdcppRetriesAVerdictThatReachedNoMapping: the capability
// pass runs beside the same cycle's model_sync reconcile, which is what creates
// an sd application's mapping in the first place. A verdict that finds no
// active mapping to land on -- one naming a stem no mapping serves yet, or a
// nameless one on an application with no active mapping -- therefore leaves
// the cadence key unstamped, so the next cycle asks again instead of one full
// interval later. The clock stands still here: the second cycle is due only
// because the first did not stamp the key. Once the verdict has landed the key
// is stamped as usual, and a third cycle at the same instant does not ask.
func TestRunAppHealthOnceSdcppRetriesAVerdictThatReachedNoMapping(t *testing.T) {
	for _, tc := range []struct{ name, stem string }{
		{"named stem", "flux1-dev"},
		{"nameless verdict", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober := sdcppProbeRunner(t, provider.SdcppVerdicts{Image: routing.CapabilityYes, ModelStem: tc.stem})
			current := time.Unix(10_000, 0).UTC()
			runner.now = func() time.Time { return current }
			state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)}

			runner.runOnce(context.Background(), state)
			if n := st.capabilityWriteCount(); n != 0 {
				t.Fatalf("UpsertMappingCapabilities called %d times with no mapping, want 0", n)
			}
			if _, ok := state.lastProbed["sdcpp:a1"]; ok {
				t.Fatalf("lastProbed = %v, want no sdcpp:a1 stamp -- the verdict reached no mapping", state.lastProbed)
			}

			// The reconcile has created the mapping by the next cycle.
			st.mu.Lock()
			st.mappings["a1"] = []routing.ModelMapping{sdcppMapping("m1", "flux1-dev")}
			st.mu.Unlock()
			runner.runOnce(context.Background(), state)
			st.assertCapability(t, "m1", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)

			runner.runOnce(context.Background(), state)
			if n := len(prober.sdcppCalls()); n != 2 {
				t.Fatalf("sdcpp probe asked %d times over three cycles at one instant, want 2 -- a landed verdict is stamped", n)
			}
		})
	}
}

// TestRunAppHealthOnceSdcppCarriesTheAppCredential: the branch builds its auth
// context exactly as the context-probe branch does, so an sd-server started
// with an API key is asked with the application's own credential.
func TestRunAppHealthOnceSdcppCarriesTheAppCredential(t *testing.T) {
	shrinkRetryGap(t)
	app := sdcppApp("a1", "s1", 8001)
	app.APIToken = "plain:sd-tok"
	app.APITokenHeader = "X-Api-Key"
	st := newHealthTestStore(app)
	prober := newSdcppFakeProber()
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	prober.sdMu.Lock()
	defer prober.sdMu.Unlock()
	// UpstreamAuth carries an ExtraHeaders map and is no longer comparable with
	// ==; check the credential fields this probe cares about directly.
	if len(prober.sdAuth) != 1 || prober.sdAuth[0].Header != "X-Api-Key" || prober.sdAuth[0].Token != "sd-tok" {
		t.Fatalf("sdcpp probe credentials = %+v, want one {Header:X-Api-Key Token:sd-tok}", prober.sdAuth)
	}
}

// TestSdcppCapabilitiesPassE2EWritesImageVerdict drives the sd branch through
// the REAL production chain -- providerClients' Multiplexer, its
// OpenAI-compatible client, a MemoryStore -- against an httptest sd-server
// serving the measured capability document. The fake-prober tests above
// cannot see the one failure that matters most here: the branch type-asserts
// r.prober to provider.SdcppCapabilitiesProber and SKIPS silently when that
// fails, so a Multiplexer that stopped implementing it, or a providerClients
// that stopped routing stable_diffusion_cpp to a client that does, would
// leave every sd mapping without a verdict and every fake test green.
func TestSdcppCapabilitiesPassE2EWritesImageVerdict(t *testing.T) {
	shrinkRetryGap(t)
	var paths []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/sdcpp/v1/capabilities" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"current_mode":"img_gen","model":{"name":"flux1-dev.safetensors","stem":"flux1-dev"},"supported_modes":["img_gen"],"output_formats":["png","jpeg","webp"]}`))
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

	ctx := context.Background()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := store.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "S1", Domain: u.Hostname(), Provider: routing.ProviderStableDiffusionCpp,
		Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	// always_reachable: the sd branch is independent of the health-check mode,
	// so the only request the upstream sees is the capability document.
	if err := store.CreateApplication(ctx, routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderStableDiffusionCpp, Port: port, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAIImages}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := store.CreateMapping(ctx, routing.ModelMapping{
		ID: "m1", ApplicationID: "a1", GatewayModelName: "flux", AppModelName: "flux1-dev",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create mapping: %v", err)
	}

	(&appHealthRunner{
		store: store, prober: providerClients(0, false, nil), registry: gateway.NewAppHealthRegistry(nil),
		settings: staticSettings(nil), probeTimeout: time.Second, now: func() time.Time { return now },
	}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	rows, err := store.MappingCapabilities(ctx, "m1")
	if err != nil {
		t.Fatalf("mapping capabilities: %v", err)
	}
	image, ok := routing.CapabilityRowsByName(rows)[routing.CapabilityImage]
	if !ok || image.Verdict != routing.CapabilityYes || image.Source != routing.CapabilitySourceSdcppCapabilities || !image.CheckedAt.Equal(now) {
		t.Fatalf("image row = %+v (present=%v), want yes/sdcpp_capabilities at %v", image, ok, now)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/sdcpp/v1/capabilities" {
		t.Fatalf("upstream paths = %v, want exactly [/sdcpp/v1/capabilities]", paths)
	}
}

// sdImageOptionsFixture is a full options view of the measured capability
// document, as ProbeSdcppImageOptions would decode it.
func sdImageOptionsFixture() provider.SdcppImageOptions {
	return provider.SdcppImageOptions{
		Samplers:      []string{"euler", "euler_a", "dpm++2m"},
		Schedulers:    []string{"discrete", "karras"},
		OutputFormats: []string{"png", "jpeg", "webp"},
		Limits:        provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 4},
		Defaults: provider.SdcppImageDefaults{
			Sampler: "euler", Scheduler: "discrete", OutputFormat: "png",
			Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 512, CFG: 7,
		},
	}
}

// sdcppOptionsRunner is sdcppProbeRunner plus an image-options registry, with
// the options document answering opts.
func sdcppOptionsRunner(t *testing.T, opts provider.SdcppImageOptions, mappings ...routing.ModelMapping) (*appHealthRunner, *fakeHealthStore, *sdcppFakeProber, *gateway.AppImageOptionsRegistry) {
	t.Helper()
	runner, st, prober := sdcppProbeRunner(t, provider.SdcppVerdicts{Image: routing.CapabilityYes, ModelStem: "flux1-dev"}, mappings...)
	prober.sdOptions["http://s1.local:8001"] = opts
	reg := gateway.NewAppImageOptionsRegistry()
	runner.imageOptionsRegistry = reg
	return runner, st, prober, reg
}

// TestRunAppHealthOnceSdcppCachesImageOptions: the same pass that writes an
// external sd-server's image verdict also caches the image options its
// capability document states, keyed by application id, so the portal can
// offer the knobs that model actually accepts. The verdict write is unchanged
// beside it.
func TestRunAppHealthOnceSdcppCachesImageOptions(t *testing.T) {
	want := sdImageOptionsFixture()
	runner, st, prober, reg := sdcppOptionsRunner(t, want, sdcppMapping("m1", "flux1-dev"))

	runCapabilityCycle(runner)

	got, ok := reg.Get("a1")
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, %v, want %+v, true", got, ok, want)
	}
	st.assertCapability(t, "m1", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)
	calls := prober.sdcppOptionCalls()
	if len(calls) != 1 || calls[0].Provider != routing.ProviderStableDiffusionCpp || calls[0].Endpoint != "http://s1.local:8001" || calls[0].Timeout != time.Second {
		t.Fatalf("options probe targets = %+v, want one {Provider:stable_diffusion_cpp Endpoint:http://s1.local:8001 Timeout:1s}", calls)
	}
}

// TestRunAppHealthOnceSdcppImageOptionsDoNotNeedAMapping: the options belong to
// the application's endpoint, not to a mapping, so they are cached even when
// the verdict has no active mapping to land on (the cycle that creates the
// mapping runs beside this pass).
func TestRunAppHealthOnceSdcppImageOptionsDoNotNeedAMapping(t *testing.T) {
	want := sdImageOptionsFixture()
	runner, _, _, reg := sdcppOptionsRunner(t, want)

	runCapabilityCycle(runner)

	if got, ok := reg.Get("a1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, %v, want %+v, true", got, ok, want)
	}
}

// TestRunAppHealthOnceSdcppImageOptionsProbeErrorCachesNothing: a failed
// options probe (here a 404-style ErrUnavailable beside which the fake still
// hands options back, so only the error guard stands between them and the
// registry) leaves the application absent, does not crash the pass, and does
// not disturb the verdict write.
func TestRunAppHealthOnceSdcppImageOptionsProbeErrorCachesNothing(t *testing.T) {
	runner, st, prober, reg := sdcppOptionsRunner(t, sdImageOptionsFixture(), sdcppMapping("m1", "flux1-dev"))
	prober.sdOptErr["http://s1.local:8001"] = fmt.Errorf("%w: status 500", provider.ErrUnavailable)

	runCapabilityCycle(runner)

	if got, ok := reg.Get("a1"); ok {
		t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, true after a failed options probe, want absent", got)
	}
	if n := len(prober.sdcppOptionCalls()); n != 1 {
		t.Fatalf("options probe asked %d times, want 1 (the failure must be reached, not skipped)", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)
}

// TestRunAppHealthOnceSdcppImageOptionsKeepLastGoodOnError: a later failed
// probe does not erase what an earlier tick learned -- stale options beat
// none for a transient outage. (Deleting the application is what evicts them,
// see TestRunAppHealthOnceRetainsImageOptionsOfLiveAppsOnly.)
func TestRunAppHealthOnceSdcppImageOptionsKeepLastGoodOnError(t *testing.T) {
	want := sdImageOptionsFixture()
	runner, _, prober, reg := sdcppOptionsRunner(t, want, sdcppMapping("m1", "flux1-dev"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)}

	runner.runOnce(context.Background(), state)
	prober.sdMu.Lock()
	prober.sdOptErr["http://s1.local:8001"] = fmt.Errorf("%w: status 503", provider.ErrUnavailable)
	prober.sdMu.Unlock()
	current = current.Add(time.Duration(portal.MaxHealthCheckIntervalSeconds) * time.Second)
	runner.runOnce(context.Background(), state)

	if n := len(prober.sdcppOptionCalls()); n != 2 {
		t.Fatalf("options probe asked %d times over two due cycles, want 2", n)
	}
	if got, ok := reg.Get("a1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, %v after a failed refresh, want the earlier %+v", got, ok, want)
	}
}

// TestRunAppHealthOnceSdcppImageOptionsFollowTheVerdictOutcome pins when the
// options probe is (not) asked, relative to the verdict probe it shares a
// document with. The verdict's strict decode failing on a document it cannot
// read (ErrInvalidResponse) must not cost the options their lenient read --
// that is the point of the separate decode. A verdict probe that could not
// reach the server at all (ErrUnavailable: transport failure or non-2xx) means
// the options GET would fail the same way, so it is not asked: a down
// sd-server must not cost a second probe timeout per tick.
func TestRunAppHealthOnceSdcppImageOptionsFollowTheVerdictOutcome(t *testing.T) {
	for _, tc := range []struct {
		name      string
		verdictEr error
		wantAsked bool
	}{
		{"verdict decode error still reads options", fmt.Errorf("%w: decode sdcpp capabilities: boom", provider.ErrInvalidResponse), true},
		{"unreachable server skips options", fmt.Errorf("%w: status 404", provider.ErrUnavailable), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := sdImageOptionsFixture()
			runner, _, prober, reg := sdcppOptionsRunner(t, want, sdcppMapping("m1", "flux1-dev"))
			prober.sdErr["http://s1.local:8001"] = tc.verdictEr

			runCapabilityCycle(runner)

			asked := len(prober.sdcppOptionCalls()) > 0
			if asked != tc.wantAsked {
				t.Fatalf("options probe asked = %v, want %v", asked, tc.wantAsked)
			}
			got, ok := reg.Get("a1")
			if ok != tc.wantAsked || (ok && !reflect.DeepEqual(got, want)) {
				t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, %v, want cached=%v", got, ok, tc.wantAsked)
			}
		})
	}
}

// TestRunAppHealthOnceSdcppNilImageOptionsRegistryAsksNothing: with no registry
// to hold the answer the options GET would be pure waste, so it is not made.
// This is also what keeps every pre-existing runner (no registry) at exactly
// the one request per tick the verdict pass always made.
func TestRunAppHealthOnceSdcppNilImageOptionsRegistryAsksNothing(t *testing.T) {
	runner, _, prober := sdcppProbeRunner(t, provider.SdcppVerdicts{Image: routing.CapabilityYes}, sdcppMapping("m1", "flux1-dev"))
	prober.sdOptions["http://s1.local:8001"] = sdImageOptionsFixture()

	runCapabilityCycle(runner)

	if n := len(prober.sdcppOptionCalls()); n != 0 {
		t.Fatalf("options probe asked %d times with no registry wired, want 0", n)
	}
}

// TestRunAppHealthOnceSdcppImageOptionsFollowTheAppCadence: the options probe
// rides the verdict pass's schedule ("sdcpp:"+id), so a second cycle at the
// same instant asks neither.
func TestRunAppHealthOnceSdcppImageOptionsFollowTheAppCadence(t *testing.T) {
	runner, _, prober, _ := sdcppOptionsRunner(t, sdImageOptionsFixture(), sdcppMapping("m1", "flux1-dev"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)}

	runner.runOnce(context.Background(), state)
	runner.runOnce(context.Background(), state)
	if v, o := len(prober.sdcppCalls()), len(prober.sdcppOptionCalls()); v != 1 || o != 1 {
		t.Fatalf("over two cycles at one instant the verdict probe was asked %d times and the options probe %d, want 1 and 1", v, o)
	}
	current = current.Add(time.Duration(portal.MaxHealthCheckIntervalSeconds) * time.Second)
	runner.runOnce(context.Background(), state)
	if v, o := len(prober.sdcppCalls()), len(prober.sdcppOptionCalls()); v != 2 || o != 2 {
		t.Fatalf("after the interval elapsed the verdict probe was asked %d times and the options probe %d, want 2 and 2", v, o)
	}
}

// TestRunAppHealthOnceSdcppImageOptionsCarryTheAppCredential: the options probe
// is built with the same target and credential as the verdict probe.
func TestRunAppHealthOnceSdcppImageOptionsCarryTheAppCredential(t *testing.T) {
	shrinkRetryGap(t)
	app := sdcppApp("a1", "s1", 8001)
	app.APIToken = "plain:sd-tok"
	app.APITokenHeader = "X-Api-Key"
	st := newHealthTestStore(app)
	prober := newSdcppFakeProber()
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		imageOptionsRegistry: gateway.NewAppImageOptionsRegistry(),
		settings:             st, probeTimeout: time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	prober.sdMu.Lock()
	defer prober.sdMu.Unlock()
	if len(prober.sdOptAuth) != 1 || prober.sdOptAuth[0].Header != "X-Api-Key" || prober.sdOptAuth[0].Token != "sd-tok" {
		t.Fatalf("options probe credentials = %+v, want one {Header:X-Api-Key Token:sd-tok}", prober.sdOptAuth)
	}
	if !reflect.DeepEqual(prober.sdTargets, prober.sdOptTargets) {
		t.Fatalf("options probe targets = %+v, verdict probe targets = %+v, want identical", prober.sdOptTargets, prober.sdTargets)
	}
}

// TestRunAppHealthOnceRetainsImageOptionsOfLiveAppsOnly: an application that no
// longer exists loses its cached options at the end of a complete cycle, like
// its loaded-model entries, so the registry does not grow for the life of the
// process.
func TestRunAppHealthOnceRetainsImageOptionsOfLiveAppsOnly(t *testing.T) {
	runner, _, _, reg := sdcppOptionsRunner(t, sdImageOptionsFixture(), sdcppMapping("m1", "flux1-dev"))
	reg.Set("deleted-app", sdImageOptionsFixture())

	runCapabilityCycle(runner)

	if _, ok := reg.Get("deleted-app"); ok {
		t.Fatalf("options of an application that no longer exists survived a complete cycle")
	}
	if _, ok := reg.Get("a1"); !ok {
		t.Fatalf("options of a live application were evicted")
	}
}

// TestSdcppImageOptionsPassE2ECachesOptions drives the options probe through
// the REAL production chain (providerClients' Multiplexer, its OpenAI-
// compatible client) against an httptest sd-server serving the capability
// document. The fake-prober tests cannot see the failure that matters here:
// the branch type-asserts the prober to provider.SdcppImageOptionsProber and
// skips silently when that fails.
func TestSdcppImageOptionsPassE2ECachesOptions(t *testing.T) {
	shrinkRetryGap(t)
	var paths []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		if r.URL.Path != "/sdcpp/v1/capabilities" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"current_mode":"img_gen","model":{"name":"flux1-dev.safetensors","stem":"flux1-dev"},"supported_modes":["img_gen"],` +
			`"samplers":["euler","euler_a"],"schedulers":["discrete","karras"],"output_formats_by_mode":{"img_gen":["png","jpeg"]},` +
			`"limits":{"min_width":64,"max_width":2048,"min_height":64,"max_height":2048,"max_batch_count":4},` +
			`"defaults_by_mode":{"img_gen":{"clip_skip":-1,"seed":-1,"width":512,"height":512,"output_format":"png",` +
			`"sample_params":{"sample_method":"euler","sample_steps":20,"scheduler":"discrete","guidance":{"txt_cfg":7}}}}}`))
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

	ctx := context.Background()
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := store.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "S1", Domain: u.Hostname(), Provider: routing.ProviderStableDiffusionCpp,
		Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := store.CreateApplication(ctx, routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderStableDiffusionCpp, Port: port, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAIImages}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}

	reg := gateway.NewAppImageOptionsRegistry()
	(&appHealthRunner{
		store: store, prober: providerClients(0, false, nil), registry: gateway.NewAppHealthRegistry(nil),
		imageOptionsRegistry: reg,
		settings:             staticSettings(nil), probeTimeout: time.Second, now: func() time.Time { return now },
	}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	want := provider.SdcppImageOptions{
		Samplers:      []string{"euler", "euler_a"},
		Schedulers:    []string{"discrete", "karras"},
		OutputFormats: []string{"png", "jpeg"},
		Limits:        provider.SdcppImageLimits{MinWidth: 64, MaxWidth: 2048, MinHeight: 64, MaxHeight: 2048, MaxBatchCount: 4},
		Defaults: provider.SdcppImageDefaults{
			Sampler: "euler", Scheduler: "discrete", OutputFormat: "png",
			Steps: 20, ClipSkip: -1, Seed: -1, Width: 512, Height: 512, CFG: 7,
		},
	}
	if got, ok := reg.Get("a1"); !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("imageOptionsRegistry.Get(a1) = %+v, %v, want %+v, true", got, ok, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 2 || paths[0] != "/sdcpp/v1/capabilities" || paths[1] != "/sdcpp/v1/capabilities" {
		t.Fatalf("upstream paths = %v, want the capability document twice (verdict + options)", paths)
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
// gateway.RuntimeUpstreamPropsFeature, gets probed at
// routing.AgentRouterContextProbePath (/upstream/{model}/props) per loaded
// mapping -- exactly like an explicit {model}-template ContextProbePath
// would. A second, otherwise-identical cycle must not re-issue the
// live-progress write (the same no-rewrite property
// TestRunAppHealthOnceLiveProgressSupportPersistsOnceNotAgainOnAn-
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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

	runner := &appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/up/props") {
		t.Fatalf("expected the server_agent implicit default path /upstream/up/props to be probed")
	}
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times after the first cycle, want 1", n)
	}

	// A second cycle with a fresh cadence state (as if the next tick's interval
	// had elapsed) reports the SAME verdict -- it must not be rewritten.
	runner.runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times across two identical cycles, want 1 (an unchanged verdict must not be rewritten)", n)
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
			if n := st.capabilityWriteCount(); n != 0 {
				t.Fatalf("UpsertMappingCapabilities called %d times without the declared feature, want 0", n)
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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

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
	if n := st.capabilityWriteCount(); n != 2 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 2", n)
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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times on a rejected probe, want 0", n)
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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

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
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 1 (the probe must still proceed on a spec read failure)", n)
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
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := prober.ctxProbeCallCount(); n != 0 {
		t.Fatalf("ProbeModelInfo called %d times for a non-server_agent app with an empty ContextProbePath, want 0 (the implicit path is type-gated, not feature-only)", n)
	}
}

// TestAppHealthLoadedPassSkipsAPortalShapedAgentApp pins that the loaded pass
// reads the stored LoadedModelsPath and never the agent router's derived
// /running: a server_agent application saved in the portal (all three probe
// fields empty) gets no LoadedModels call and no gateway-poll registry write,
// even though its router would answer and its agent declares the props route.
// For such an application the agent's own report is the loaded-state truth,
// and a gateway-poll entry would outlive an empty agent report.
func TestAppHealthLoadedPassSkipsAPortalShapedAgentApp(t *testing.T) {
	shrinkRetryGap(t)
	app := serverAgentApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	prober := newFakeProber()
	prober.loaded["http://s1.local:8001"] = []string{"qwen"}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	changed, unsubscribe := loaded.Subscribe()
	defer unsubscribe()
	bundle := fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: bundle, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := prober.loadedCallCount(); n != 0 {
		t.Fatalf("LoadedModels called %d times for a portal-shaped server_agent app, want 0 (the loaded pass reads the stored field only)", n)
	}
	select {
	case <-changed:
		t.Fatalf("the loaded pass wrote the registry (SetGatewayProbe) for a portal-shaped server_agent app, want no write")
	default:
	}
	if got := loaded.LoadedAppModels("a1", "s1"); got != nil {
		t.Fatalf("loaded models = %v, want nil (no gateway-poll entry for a portal-shaped server_agent app)", got)
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
			Status: routing.ServerStatusActive,
		}},
	}
	st.seedCapability("m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	prober := newFakeProber()
	// Stands in for a vLLM /v1/models response: ProbeModelInfo reports a context
	// size but LiveProgressSupport is unknown ("") -- the schema field it is
	// about does not exist for this upstream's probed endpoint at all.
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 4096}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times for an unknown verdict, want 0", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	got, _ := st.mappingOf("m1")
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

	if n := st.capabilityWriteCount(); n != 2 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 2 -- a nameless verdict belongs to every mapping of this one-endpoint application", n)
	}
	for _, id := range []string{"m1", "m2"} {
		st.assertCapability(t, id, routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
		got, _ := st.mappingOf(id)
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

	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 1 -- a NAMED verdict must still reach only the name-matched mapping", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if row, ok := st.capabilityOf("m2", routing.CapabilityLiveProgress); ok {
		t.Fatalf("m-b has a live_progress row (%+v), want none (the reported name did not match)", row)
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
			Status: routing.ServerStatusActive,
		}},
	}
	st.seedCapability("m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	prober := newFakeProber()
	prober.modelInfo["http://s1.local:8001"] = []provider.ModelInfo{{Name: "up", ContextSize: 4096, LiveProgressSupport: "unsupported"}}
	reg := gateway.NewAppHealthRegistry(nil)

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityNo, routing.CapabilitySourceLlamaCppProps)
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 1", n)
	}
}

// TestRunAppHealthOnceLiveProgressSupportIgnoresMetricsLock proves the
// capability write is deliberately NOT gated on mp.MetricsLocked, unlike
// UpdateMappingContextProbe: a capability is not a metric an operator pins
// numbers against (see routing.MappingStore.UpsertMappingCapabilities), so an
// operator locking a mapping's throughput/context-size figures must not also
// silently suppress a real, freshly-detected build capability. What the
// operator gets instead is per-capability provenance -- a manual row a probe
// may not overwrite.
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
	// ...but the capability row is written regardless.
	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
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

	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	if n := st.capabilityWriteCount(); n != 1 {
		t.Fatalf("UpsertMappingCapabilities called %d times, want 1", n)
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
// Asserted as a NUMERIC call count of zero on the writer spy, not only by
// inspecting the stored row: with the row model an empty verdict cannot even
// be handed to the writer (routing.ValidateCapabilityRow rejects it), so the
// count is what proves the pass declined rather than that the store defended
// itself. The context-size write must still land, proving the probe ran at all
// and that the two writes are not coupled.
func TestRunAppHealthOnceContextProbeTemplateUnknownVerdictNeverOverwrites(t *testing.T) {
	shrinkRetryGap(t)
	app := ctxTemplateApp("a1", "s1", 8001)
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{
		"a1": {{
			ID: "m1", ApplicationID: "a1", GatewayModelName: "g-a", AppModelName: "m-a",
			Status: routing.ServerStatusActive,
		}},
	}
	st.seedCapability("m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	prober := newFakeProber()
	// A llama-swap `peer`-mode answer: a parseable body that yields a context
	// size but NO live-progress verdict (LiveProgressSupport left "").
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times for an unknown verdict, want 0 -- unknown must never overwrite a stored verdict", n)
	}
	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	got, _ := st.mappingOf("m1")
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
			Status: routing.ServerStatusActive, ContextSize: 8192,
		}},
	}
	st.seedCapability("m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	prober := newFakeProber()
	prober.modelInfoByPath["/upstream/m-a/props"] = []provider.ModelInfo{{Name: "m-a", ContextSize: 8192, LiveProgressSupport: "supported"}}
	reg := gateway.NewAppHealthRegistry(nil)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"m-a"})

	(&appHealthRunner{store: st, prober: prober, syncer: nil, registry: reg, loaded: loaded, agents: nil, groups: nil, settings: st, probeTimeout: time.Second, cipher: nil, now: time.Now}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	if !prober.probedPath("/upstream/m-a/props") {
		t.Fatalf("the per-model probe must have run -- otherwise a zero write count proves nothing")
	}
	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times for an UNCHANGED verdict, want 0 -- this pass runs on the app's own cadence, forever", n)
	}
}

// TestRunAppHealthOnceContextProbeTemplateLockedMappingPersistsCapabilityNotContextSize
// closes a gap the Task 2 implementer correctly surfaced but was told not to
// fix: the {model}-template branch's per-mapping skip used to read
// `mp.Status != routing.ServerStatusActive || mp.MetricsLocked ||
// mp.AppModelName == ""`, which skipped a LOCKED mapping before it was ever
// probed -- so its live-progress capability could never be learned at all,
// even though the capability writer itself carries no lock guard
// (a capability is a property of the upstream build, not a number an
// operator answers for). This is llama-swap's default {model} shape, so the
// hole was not theoretical.
//
// The mapping starts with a distinguishable pre-existing state on BOTH
// fields under test -- ContextSize 4096 (the probe reports a different
// 8192) and NO live_progress row at all (the probe reports "supported") -- so
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
	st.assertCapability(t, "m1", routing.CapabilityLiveProgress, routing.CapabilityYes, routing.CapabilitySourceLlamaCppProps)
	got, _ := st.mappingOf("m1")
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
	if row, ok := st.capabilityOf("m2", routing.CapabilityLiveProgress); ok {
		t.Fatalf("m-b has a live_progress row (%+v), want none (non-active mapping must not be probed nor written)", row)
	}
	if row, ok := st.capabilityOf("m3", routing.CapabilityLiveProgress); ok {
		t.Fatalf("m-c has a live_progress row (%+v), want none (empty upstream model name must not be probed nor written)", row)
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

// TestRunAppHealthOnceNetbirdOnlySkipsOffMeshSdcppProbe is the sibling of the
// test above for the stable_diffusion_cpp capability pass: under netbird_only
// an off-mesh server's sd application is never asked for its capability
// document, so nothing dials it. The on-mesh server's sd application is the
// positive control -- it IS asked, and gets its row -- so a branch that asked
// nobody would fail here too. (Mutation guard: moving the sd branch out of the
// `if r.prober != nil && !offMesh` block makes the off-mesh app asked.)
func TestRunAppHealthOnceNetbirdOnlySkipsOffMeshSdcppProbe(t *testing.T) {
	shrinkRetryGap(t)
	st := &fakeHealthStore{
		servers: []routing.AIServer{netbirdServer("s1", false), netbirdServer("s2", true)},
		apps: map[string][]routing.Application{
			"s1": {sdcppApp("a1", "s1", 8001)}, // off-mesh
			"s2": {sdcppApp("a2", "s2", 8002)}, // on-mesh
		},
		settings: map[string]string{"netbird_only": "true"},
		mappings: map[string][]routing.ModelMapping{
			"a1": {{ID: "m1", ApplicationID: "a1", GatewayModelName: "g1", AppModelName: "flux1-dev", Status: routing.ServerStatusActive}},
			"a2": {{ID: "m2", ApplicationID: "a2", GatewayModelName: "g2", AppModelName: "flux1-dev", Status: routing.ServerStatusActive}},
		},
	}
	prober := newSdcppFakeProber()
	for _, ep := range []string{"http://s1.local:8001", "http://s2.local:8002"} {
		prober.sdVerdicts[ep] = provider.SdcppVerdicts{Image: routing.CapabilityYes, ModelStem: "flux1-dev"}
	}

	(&appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}).runOnce(context.Background(), &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	calls := prober.sdcppCalls()
	if len(calls) != 1 || calls[0].Endpoint != "http://s2.local:8002" {
		t.Fatalf("sdcpp probe targets = %+v, want exactly the on-mesh application (off-mesh must never be dialed under netbird_only)", calls)
	}
	if row, ok := st.capabilityOf("m1", routing.CapabilityImage); ok {
		t.Fatalf("off-mesh m1 got an image row %+v, want none", row)
	}
	st.assertCapability(t, "m2", routing.CapabilityImage, routing.CapabilityYes, routing.CapabilitySourceSdcppCapabilities)
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

// TestAudioCppCapabilityRows pins the audio.cpp projector: one row PER AXIS the
// listing answered -- a speech row and a transcription row, each attributed to
// routing.CapabilitySourceAudioCppModels and stamped at the given instant, for a
// yes or a no -- and NO row for anything else.
//
// "" is the case that matters. parseAudioCppModels answers "" for everything
// the /v1/models listing does not say (the model is not named, the listing is
// empty or unreadable, the entry carries no task), and a "no" here can switch a
// model's route off, so "unknown" must be the ABSENCE of a row and never a
// written no -- on each axis independently.
func TestAudioCppCapabilityRows(t *testing.T) {
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	row := func(capability, verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: capability, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	}
	for _, tc := range []struct {
		name string
		v    provider.AudioCppVerdicts
		want []routing.CapabilityRow
	}{
		{
			name: "a tts model: speech yes beside transcription no",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{
			name: "an asr model: speech no beside transcription yes (a loaded model's task is authoritative)",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
			},
		},
		{
			name: "a model that is neither: no on both axes",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{
			name: "speech yes alone builds only the speech row",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes},
			want: []routing.CapabilityRow{row(routing.CapabilitySpeech, routing.CapabilityYes)},
		},
		{
			name: "transcription yes alone builds only the transcription row",
			v:    provider.AudioCppVerdicts{Transcription: routing.CapabilityYes},
			want: []routing.CapabilityRow{row(routing.CapabilityTranscription, routing.CapabilityYes)},
		},
		{
			name: "a streaming asr model: transcription yes beside transcription_streaming yes",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
				row(routing.CapabilityTranscriptionStreaming, routing.CapabilityYes),
			},
		},
		{
			name: "an offline asr model: transcription_streaming no is a real verdict",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
				row(routing.CapabilityTranscriptionStreaming, routing.CapabilityNo),
			},
		},
		{
			name: "a streaming verdict on a model that is not a transcriber builds no streaming row (tts)",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, TranscriptionStreaming: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{
			name: "a no on a model that is not a transcriber builds no streaming row either",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityNo, TranscriptionStreaming: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{name: "a streaming verdict with no transcription verdict builds no row", v: provider.AudioCppVerdicts{TranscriptionStreaming: routing.CapabilityYes}, want: nil},
		{
			name: "a clone model: speech_clone yes beside speech yes and transcription no",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
				row(routing.CapabilitySpeechClone, routing.CapabilityYes),
			},
		},
		{
			name: "a plain tts model: speech_clone no is a real verdict",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
				row(routing.CapabilitySpeechClone, routing.CapabilityNo),
			},
		},
		{
			name: "a speech_clone verdict on a model that is not a speech model builds no speech_clone row (asr)",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, SpeechClone: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
			},
		},
		{name: "a speech_clone verdict with no speech verdict builds no row", v: provider.AudioCppVerdicts{SpeechClone: routing.CapabilityYes}, want: nil},
		{
			name: "a speech_clone verdict that is not yes or no builds no row",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: "maybe"},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{
			name: "a streaming tts model: speech_streaming yes beside speech yes and speech_clone no",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
				row(routing.CapabilitySpeechClone, routing.CapabilityNo),
				row(routing.CapabilitySpeechStreaming, routing.CapabilityYes),
			},
		},
		{
			name: "an offline clone model: speech_streaming no is a real verdict",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes, SpeechStreaming: routing.CapabilityNo},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
				row(routing.CapabilitySpeechClone, routing.CapabilityYes),
				row(routing.CapabilitySpeechStreaming, routing.CapabilityNo),
			},
		},
		{
			name: "a speech_streaming verdict on a model that is not a speech model builds no speech_streaming row (asr)",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, SpeechStreaming: routing.CapabilityYes},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
			},
		},
		{name: "a speech_streaming verdict with no speech verdict builds no row", v: provider.AudioCppVerdicts{SpeechStreaming: routing.CapabilityYes}, want: nil},
		{
			name: "a speech_streaming verdict that is not yes or no builds no row",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechStreaming: "maybe"},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityYes),
				row(routing.CapabilityTranscription, routing.CapabilityNo),
			},
		},
		{
			name: "a transcription_streaming verdict that is not yes or no builds no row",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: "maybe"},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
			},
		},
		{
			name: "an unknown mode does not suppress the task's rows and writes no streaming row",
			v:    provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: ""},
			want: []routing.CapabilityRow{
				row(routing.CapabilitySpeech, routing.CapabilityNo),
				row(routing.CapabilityTranscription, routing.CapabilityYes),
			},
		},
		{name: "unknown on both axes builds no row", v: provider.AudioCppVerdicts{}, want: nil},
		{name: "a speech verdict that is not yes or no builds no speech row", v: provider.AudioCppVerdicts{Speech: "maybe"}, want: nil},
		{name: "a transcription verdict that is not yes or no builds no transcription row", v: provider.AudioCppVerdicts{Transcription: "maybe"}, want: nil},
		{
			name: "one axis unknown does not suppress the other",
			v:    provider.AudioCppVerdicts{Speech: "", Transcription: routing.CapabilityNo},
			want: []routing.CapabilityRow{row(routing.CapabilityTranscription, routing.CapabilityNo)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := audioCppCapabilityRows(tc.v, at)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("audioCppCapabilityRows(%+v) = %+v, want %+v", tc.v, got, tc.want)
			}
		})
	}
}

// TestAudioCppCapabilityRowsEmitSpeechStreaming pins the speech twin of the
// streaming axis on its own: a speech model's streaming verdict becomes a
// speech_streaming row, under the same source and stamp as the speech and
// transcription rows. yes and no are both real answers (the mode names how the
// model is loaded); no verdict at all -- the mode was absent, unknown or
// unreadable -- is no row, and never a written no, because the speech handler
// reads an absent row as "buffered" and a written no as a statement the model
// cannot stream.
//
// The row is the speech capability's refinement, so it is emitted only beside a
// speech yes: a model that is not a speech model (asr, music, one the listing
// gives no task) gets NO row on it, not even a no, whatever the verdict says.
func TestAudioCppCapabilityRowsEmitSpeechStreaming(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	rowOf := func(capability, verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: capability, Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	}
	speechRows := []routing.CapabilityRow{rowOf("speech", routing.CapabilityYes), rowOf("transcription", routing.CapabilityNo)}
	for _, tc := range []struct {
		name string
		v    provider.AudioCppVerdicts
		want []routing.CapabilityRow
	}{
		{"a tts model in streaming mode", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes}, append(append([]routing.CapabilityRow{}, speechRows...), rowOf("speech_streaming", routing.CapabilityYes))},
		{"a tts model in offline mode", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechStreaming: routing.CapabilityNo}, append(append([]routing.CapabilityRow{}, speechRows...), rowOf("speech_streaming", routing.CapabilityNo))},
		{"a tts model whose mode is not stated", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo}, speechRows},
		{"an asr model with a speech_streaming verdict", provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, SpeechStreaming: routing.CapabilityYes}, []routing.CapabilityRow{
			rowOf("speech", routing.CapabilityNo), rowOf("transcription", routing.CapabilityYes),
		}},
		{"a speech_streaming verdict beside no speech verdict", provider.AudioCppVerdicts{SpeechStreaming: routing.CapabilityYes}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := audioCppCapabilityRows(tc.v, at)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("audioCppCapabilityRows(%+v) = %+v, want %+v", tc.v, got, tc.want)
			}
		})
	}
}

// TestAudioCppCapabilityRowsEmitTranscriptionStreaming pins the third axis on its
// own: a transcriber's streaming verdict becomes a transcription_streaming row, under
// the same source and stamp as the speech and transcription rows. yes and no are both
// real answers (the mode names how the model is loaded); no verdict at all -- the
// mode was absent, unknown or unreadable -- is no row, and never a written no,
// because the handler reads an absent row as "buffered" and a written no as a
// statement the model cannot stream.
//
// The row is the transcription capability's refinement, so it is emitted only beside
// a transcription yes: a model that is not a transcriber (tts, clone, music, one the
// listing gives no task) gets NO row on it, not even a no, whatever the verdict says.
func TestAudioCppCapabilityRowsEmitTranscriptionStreaming(t *testing.T) {
	at := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	transcriptionRow := routing.CapabilityRow{Capability: "transcription", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	streamingRow := func(verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: "transcription_streaming", Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	}
	for _, tc := range []struct {
		name string
		v    provider.AudioCppVerdicts
		want []routing.CapabilityRow
	}{
		{"an asr model in streaming mode", provider.AudioCppVerdicts{Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityYes}, []routing.CapabilityRow{transcriptionRow, streamingRow(routing.CapabilityYes)}},
		{"an asr model in offline mode", provider.AudioCppVerdicts{Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityNo}, []routing.CapabilityRow{transcriptionRow, streamingRow(routing.CapabilityNo)}},
		{"an asr model whose mode is not stated", provider.AudioCppVerdicts{Transcription: routing.CapabilityYes}, []routing.CapabilityRow{transcriptionRow}},
		{"a tts model with mode streaming", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, TranscriptionStreaming: routing.CapabilityYes}, []routing.CapabilityRow{
			{Capability: "speech", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at},
			{Capability: "transcription", Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at},
		}},
		{"a streaming verdict beside no transcription verdict", provider.AudioCppVerdicts{TranscriptionStreaming: routing.CapabilityYes}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := audioCppCapabilityRows(tc.v, at)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("audioCppCapabilityRows(%+v) = %+v, want %+v", tc.v, got, tc.want)
			}
		})
	}
}

// TestAudioCppCapabilityRowsEmitSpeechClone pins the fourth axis on its own: a speech
// model's speech_clone verdict becomes a speech_clone row, under the same source and
// stamp as the speech and transcription rows. yes (a clone model, which needs a
// reference voice) and no (a plain tts model) are both real answers; no verdict at
// all -- no task, an unnamed model, an unreadable listing -- is no row, and never a
// written no, because the portal reads an absent row as "not known to be a clone
// model" and a written no as a statement it is a plain synthesiser.
//
// The row is the speech capability's refinement, so it is emitted only beside a speech
// yes: a model that is not a speech model (asr, music, one the listing gives no task)
// gets NO row on it, not even a no, whatever the verdict says.
func TestAudioCppCapabilityRowsEmitSpeechClone(t *testing.T) {
	at := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	speechRow := routing.CapabilityRow{Capability: "speech", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	transcriptionNoRow := routing.CapabilityRow{Capability: "transcription", Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	cloneRow := func(verdict string) routing.CapabilityRow {
		return routing.CapabilityRow{Capability: "speech_clone", Verdict: verdict, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at}
	}
	for _, tc := range []struct {
		name string
		v    provider.AudioCppVerdicts
		want []routing.CapabilityRow
	}{
		{"a clone model", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes}, []routing.CapabilityRow{speechRow, transcriptionNoRow, cloneRow(routing.CapabilityYes)}},
		{"a plain tts model", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo}, []routing.CapabilityRow{speechRow, transcriptionNoRow, cloneRow(routing.CapabilityNo)}},
		{"a speech model whose task is not stated", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo}, []routing.CapabilityRow{speechRow, transcriptionNoRow}},
		{"an asr model", provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, SpeechClone: routing.CapabilityNo}, []routing.CapabilityRow{
			{Capability: "speech", Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at},
			{Capability: "transcription", Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: at},
		}},
		{"a speech_clone verdict beside no speech verdict", provider.AudioCppVerdicts{SpeechClone: routing.CapabilityYes}, nil},
		{"the empty verdict", provider.AudioCppVerdicts{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := audioCppCapabilityRows(tc.v, at)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("audioCppCapabilityRows(%+v) = %+v, want %+v", tc.v, got, tc.want)
			}
		})
	}
}

// audioCppFakeProber is the file's fakeProber plus provider.AudioCppModelsProber
// -- the smallest addition that lets the audio.cpp branch of the probe pass be
// observed, built exactly like sdcppFakeProber (see there for why it embeds).
type audioCppFakeProber struct {
	*fakeProber
	acMu sync.Mutex
	// acVerdicts / acErr answer ProbeAudioCppModels per MODEL ID. A model with
	// an error STILL returns its verdict beside it, so a branch that ignored
	// the error would write it -- which is what makes "an error writes
	// nothing" observable rather than true by construction.
	acVerdicts map[string]provider.AudioCppVerdicts
	acErr      map[string]error
	// acTargets / acModels / acAuth record every call, in order: the target,
	// the model id the branch asked about, and the credential it carried.
	acTargets []routing.Target
	acModels  []string
	acAuth    []provider.UpstreamAuth
	// acVoices / acVoicesErr answer ListVoices per MODEL ID, the voices twin of
	// acVerdicts / acErr: a model with an error STILL returns its voices beside
	// it, so a branch that ignored the error would write them. acVoicesModels /
	// acVoicesTargets / acVoicesAuth record every ListVoices call, in order.
	acVoices        map[string][]provider.Voice
	acVoicesErr     map[string]error
	acVoicesModels  []string
	acVoicesTargets []routing.Target
	acVoicesAuth    []provider.UpstreamAuth
}

var (
	_ provider.AudioCppModelsProber = (*audioCppFakeProber)(nil)
	_ provider.AudioCppVoicesLister = (*audioCppFakeProber)(nil)
)

func newAudioCppFakeProber() *audioCppFakeProber {
	return &audioCppFakeProber{
		fakeProber:  newFakeProber(),
		acVerdicts:  map[string]provider.AudioCppVerdicts{},
		acErr:       map[string]error{},
		acVoices:    map[string][]provider.Voice{},
		acVoicesErr: map[string]error{},
	}
}

func (f *audioCppFakeProber) ListVoices(ctx context.Context, target routing.Target, model string) ([]provider.Voice, error) {
	f.acMu.Lock()
	defer f.acMu.Unlock()
	f.acVoicesTargets = append(f.acVoicesTargets, target)
	f.acVoicesModels = append(f.acVoicesModels, model)
	a, _ := provider.UpstreamAuthFrom(ctx)
	f.acVoicesAuth = append(f.acVoicesAuth, a)
	return f.acVoices[model], f.acVoicesErr[model]
}

// voicesAsked returns a copy of every model id ListVoices was asked about.
func (f *audioCppFakeProber) voicesAsked() []string {
	f.acMu.Lock()
	defer f.acMu.Unlock()
	return append([]string(nil), f.acVoicesModels...)
}

func (f *audioCppFakeProber) ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (provider.AudioCppVerdicts, error) {
	f.acMu.Lock()
	defer f.acMu.Unlock()
	f.acTargets = append(f.acTargets, target)
	f.acModels = append(f.acModels, modelID)
	a, _ := provider.UpstreamAuthFrom(ctx)
	f.acAuth = append(f.acAuth, a)
	return f.acVerdicts[modelID], f.acErr[modelID]
}

// audioCppAsked returns a copy of every model id ProbeAudioCppModels was asked about.
func (f *audioCppFakeProber) audioCppAsked() []string {
	f.acMu.Lock()
	defer f.acMu.Unlock()
	return append([]string(nil), f.acModels...)
}

// audioCppApp is a static, operator-run audio.cpp application: NO
// ContextProbePath (so the speech probe cannot ride the context branch) and NO
// runtime spec -- the deployment this type exists for, which no agent manages.
// The health path is a fixture value, not the portal form's default (/health):
// these tests fake the prober, so nothing here reads it.
func audioCppApp(id, serverID string, port int) routing.Application {
	app := activeApp(id, serverID, port)
	app.Type = routing.ProviderAudioCpp
	app.HealthCheckPath = "/v1/models"
	return app
}

// audioCppProbeRunner builds a runner over one audio_cpp application ("a1" on
// http://s1.local:8001) with the given mappings and NO runtime spec, whose
// /v1/models listing answers verdicts per model id.
func audioCppProbeRunner(t *testing.T, verdicts map[string]provider.AudioCppVerdicts, mappings ...routing.ModelMapping) (*appHealthRunner, *fakeHealthStore, *audioCppFakeProber) {
	t.Helper()
	shrinkRetryGap(t)
	st := newHealthTestStore(audioCppApp("a1", "s1", 8001))
	st.mappings = map[string][]routing.ModelMapping{"a1": mappings}
	prober := newAudioCppFakeProber()
	for model, v := range verdicts {
		prober.acVerdicts[model] = v
	}
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}
	return runner, st, prober
}

func audioCppMapping(id, appModel string) routing.ModelMapping {
	return routing.ModelMapping{ID: id, ApplicationID: "a1", GatewayModelName: "g-" + id, AppModelName: appModel, Status: routing.ServerStatusActive}
}

// TestRunAppHealthOnceAudioCppWritesSpeechAndTranscriptionVerdicts is the
// branch's whole job: the gateway reads a static audio.cpp server's own
// /v1/models listing for each of the application's mappings and writes the
// speech AND transcription verdicts it derives, at the probe rank under source
// audiocpp_models. The application has NO runtime spec -- an operator-run
// server has none -- so this also proves the probe needs none.
//
// Both directions are asserted on both axes (a loaded asr model is a real speech
// "no" and a transcription "yes"; a tts or clone model the reverse). The model
// id handed to the probe is the MAPPING's upstream name (AppModelName), which
// is what audio.cpp's listing is keyed on: the verdict is per MODEL, so every
// mapping is its own probe.
func TestRunAppHealthOnceAudioCppWritesSpeechAndTranscriptionVerdicts(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{
			"pocket_tts_english_bf16": {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo},
			"whisper_small":           {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes},
			"clone_model":             {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo},
		},
		audioCppMapping("m1", "pocket_tts_english_bf16"), audioCppMapping("m2", "whisper_small"),
		audioCppMapping("m3", "clone_model"))

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m2", routing.CapabilitySpeech, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m3", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m1", routing.CapabilityTranscription, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m2", routing.CapabilityTranscription, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m3", routing.CapabilityTranscription, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)

	asked := prober.audioCppAsked()
	sort.Strings(asked)
	if want := []string{"clone_model", "pocket_tts_english_bf16", "whisper_small"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("audio.cpp probe asked about %v, want exactly %v (each mapping's AppModelName, nothing else)", asked, want)
	}
	prober.acMu.Lock()
	defer prober.acMu.Unlock()
	for _, target := range prober.acTargets {
		if target.Provider != routing.ProviderAudioCpp || target.Endpoint != "http://s1.local:8001" || target.Timeout != time.Second {
			t.Fatalf("audio.cpp probe target = %+v, want {Provider:audio_cpp Endpoint:http://s1.local:8001 Timeout:1s}", target)
		}
	}
}

// TestRunAppHealthOnceAudioCppWritesTranscriptionStreamingVerdict: the same
// /v1/models read also states each ASR model's mode, and the probe pass stores it as
// the per-mapping transcription_streaming row the transcription handler decides
// stream-versus-buffered on -- yes for a streaming model, no for an offline one. A
// model that does not transcribe gets no such row.
// A model whose listing states no mode writes no row AND leaves the verdict
// already on file alone: an unknown mode must not turn a stored yes into a
// buffered answer.
func TestRunAppHealthOnceAudioCppWritesTranscriptionStreamingVerdict(t *testing.T) {
	runner, st, _ := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{
			"vibevoice_streaming": {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityYes},
			"whisper_offline":     {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityNo},
			"no_mode":             {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes},
			"stale_mode":          {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes},
			"tts_with_mode":       {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, TranscriptionStreaming: routing.CapabilityYes},
		},
		audioCppMapping("m1", "vibevoice_streaming"), audioCppMapping("m2", "whisper_offline"),
		audioCppMapping("m3", "no_mode"), audioCppMapping("m4", "stale_mode"), audioCppMapping("m5", "tts_with_mode"))
	st.seedCapability("m4", routing.CapabilityTranscriptionStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilityTranscriptionStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m2", routing.CapabilityTranscriptionStreaming, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
	if row, ok := st.capabilityOf("m3", routing.CapabilityTranscriptionStreaming); ok {
		t.Fatalf("m3 has a transcription_streaming row %+v, want none: its listing stated no mode", row)
	}
	st.assertCapability(t, "m4", routing.CapabilityTranscriptionStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	// A model that does not transcribe gets no streaming row, whatever a verdict says.
	if row, ok := st.capabilityOf("m5", routing.CapabilityTranscriptionStreaming); ok {
		t.Fatalf("m5 (a tts model) has a transcription_streaming row %+v, want none: the capability is the transcriber's", row)
	}
	st.assertCapability(t, "m5", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
}

// TestRunAppHealthOnceAudioCppWritesSpeechStreamingVerdict is the speech twin of
// the transcription_streaming one: the same /v1/models read also states each
// speech model's mode, and the probe pass stores it as the per-mapping
// speech_streaming row the speech handler decides stream-versus-buffered on --
// yes for a streaming model, no for an offline one. A model that does not
// synthesise speech gets no such row. A model whose listing states no mode writes
// no row AND leaves the verdict already on file alone: an unknown mode must not
// turn a stored yes into a buffered answer.
func TestRunAppHealthOnceAudioCppWritesSpeechStreamingVerdict(t *testing.T) {
	runner, st, _ := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{
			"vibevoice_tts":     {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityYes},
			"pocket_tts":        {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo, SpeechStreaming: routing.CapabilityNo},
			"no_mode":           {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo},
			"stale_mode":        {Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo},
			"asr_with_tts_mode": {Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, SpeechStreaming: routing.CapabilityYes},
		},
		audioCppMapping("m1", "vibevoice_tts"), audioCppMapping("m2", "pocket_tts"),
		audioCppMapping("m3", "no_mode"), audioCppMapping("m4", "stale_mode"), audioCppMapping("m5", "asr_with_tts_mode"))
	st.seedCapability("m4", routing.CapabilitySpeechStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilitySpeechStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.assertCapability(t, "m2", routing.CapabilitySpeechStreaming, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
	if row, ok := st.capabilityOf("m3", routing.CapabilitySpeechStreaming); ok {
		t.Fatalf("m3 has a speech_streaming row %+v, want none: its listing stated no mode", row)
	}
	st.assertCapability(t, "m4", routing.CapabilitySpeechStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	// A model that does not synthesise speech gets no speech_streaming row, whatever a verdict says.
	if row, ok := st.capabilityOf("m5", routing.CapabilitySpeechStreaming); ok {
		t.Fatalf("m5 (an asr model) has a speech_streaming row %+v, want none: the capability is the synthesiser's", row)
	}
	st.assertCapability(t, "m5", routing.CapabilityTranscription, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
}

// TestRunAppHealthOnceAudioCppNeverReadsRuntimeSpecs pins that the gate is the
// application's TYPE alone: a static audio.cpp server has no runtime spec, so
// the branch must not depend on reading one. A spec read that fails -- which
// the old server_agent gate treated as "probe nothing" -- changes nothing here.
func TestRunAppHealthOnceAudioCppNeverReadsRuntimeSpecs(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: routing.CapabilityYes}},
		audioCppMapping("m1", "pocket_tts"))
	st.specsErr = true

	runCapabilityCycle(runner)

	if got, want := prober.audioCppAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio.cpp probe asked about %v, want %v -- it must not depend on a runtime spec", got, want)
	}
	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
}

// TestRunAppHealthOnceAudioCppUnknownVerdictWritesNothing: an empty verdict --
// the model is not in the listing, which names LOADED models only -- is no
// answer, not a "no". Nothing may be written and the verdict already on file
// must survive, or one tick with the model unloaded would switch its speech
// route off.
func TestRunAppHealthOnceAudioCppUnknownVerdictWritesNothing(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: ""}},
		audioCppMapping("m1", "pocket_tts"))
	st.seedCapability("m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)

	runCapabilityCycle(runner)

	if n := len(prober.audioCppAsked()); n != 1 {
		t.Fatalf("audio.cpp probe asked %d times, want 1 (the probe must have run for this to mean anything)", n)
	}
	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times for an unknown verdict, want 0", n)
	}
	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
}

// TestRunAppHealthOnceAudioCppProbeErrorWritesNothing: a failed probe states
// no verdict. The fake hands a "no" back beside the error and the stored row is
// a same-rank "yes" that "no" would be allowed to overwrite, so only the error
// guard stands between them.
func TestRunAppHealthOnceAudioCppProbeErrorWritesNothing(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: routing.CapabilityNo}},
		audioCppMapping("m1", "pocket_tts"))
	prober.acErr["pocket_tts"] = fmt.Errorf("%w: status 503", provider.ErrUnavailable)
	st.seedCapability("m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times after a failed probe, want 0", n)
	}
	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
}

// TestRunAppHealthOnceAudioCppPrecedence pins the two halves of the shared
// write rule for this source: an operator's (manual) verdict is never talked
// over by the probe, however often it re-reads the listing; and the probe's OWN
// earlier row is replaced when the loaded model's task changes, in either
// direction -- the source is bidirectional.
func TestRunAppHealthOnceAudioCppPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		seedVerdict, seedSource     string
		probe, wantVerdict, wantSrc string
	}{
		{"manual yes survives a probe no", routing.CapabilityYes, routing.CapabilitySourceManual, routing.CapabilityNo, routing.CapabilityYes, routing.CapabilitySourceManual},
		{"manual no survives a probe yes", routing.CapabilityNo, routing.CapabilitySourceManual, routing.CapabilityYes, routing.CapabilityNo, routing.CapabilitySourceManual},
		{"a probed yes is replaced by a probed no", routing.CapabilityYes, routing.CapabilitySourceAudioCppModels, routing.CapabilityNo, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels},
		{"a probed no is replaced by a probed yes", routing.CapabilityNo, routing.CapabilitySourceAudioCppModels, routing.CapabilityYes, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, _ := audioCppProbeRunner(t,
				map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: tc.probe}},
				audioCppMapping("m1", "pocket_tts"))
			st.seedCapability("m1", routing.CapabilitySpeech, tc.seedVerdict, tc.seedSource)

			runCapabilityCycle(runner)

			st.assertCapability(t, "m1", routing.CapabilitySpeech, tc.wantVerdict, tc.wantSrc)
		})
	}
}

// TestRunAppHealthOnceAudioCppSkipsDisabledAndNamelessMappings: only an active
// mapping with an upstream model name has anything to ask the listing about.
func TestRunAppHealthOnceAudioCppSkipsDisabledAndNamelessMappings(t *testing.T) {
	disabled := audioCppMapping("m2", "whisper_small")
	disabled.Status = routing.ServerStatusDisabled
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{
			"pocket_tts":    {Speech: routing.CapabilityYes},
			"whisper_small": {Speech: routing.CapabilityYes},
		},
		audioCppMapping("m1", "pocket_tts"), disabled, audioCppMapping("m3", ""))

	runCapabilityCycle(runner)

	if got, want := prober.audioCppAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio.cpp probe asked about %v, want %v", got, want)
	}
	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	for _, id := range []string{"m2", "m3"} {
		if _, ok := st.capabilityOf(id, routing.CapabilitySpeech); ok {
			t.Fatalf("%s has a speech row, want none", id)
		}
	}
}

// TestRunAppHealthOnceAudioCppNeverAsksAnotherType pins the gate: the listing
// is asked of audio_cpp applications only, and every other type is never
// asked -- including a server_agent application whose mapping carries an
// audio_cpp runtime spec, the shape the previous gate keyed on. That path stays
// deferred (the agent router's /v1/models carries no task, so it could yield no
// verdict), and a stale spec must not get an agent-managed mapping probed here.
// The audio application in the same store is the positive control: a branch
// that asked nobody would fail here too.
func TestRunAppHealthOnceAudioCppNeverAsksAnotherType(t *testing.T) {
	shrinkRetryGap(t)
	apps := []routing.Application{audioCppApp("audio", "s1", 8000)}
	mappings := map[string][]routing.ModelMapping{
		"audio": {{ID: "ma", ApplicationID: "audio", GatewayModelName: "g-ma", AppModelName: "pocket_tts", Status: routing.ServerStatusActive}},
	}
	prober := newAudioCppFakeProber()
	prober.acVerdicts["pocket_tts"] = provider.AudioCppVerdicts{Speech: routing.CapabilityYes}
	var specs []routing.RuntimeSpec
	for i, typ := range []string{
		routing.ProviderVLLM, routing.ProviderLlamaCPP, routing.ProviderLlamaSwap,
		routing.ProviderLiteLLM, routing.ProviderOllama, routing.ProviderServerAgent,
		routing.ProviderStableDiffusionCpp, routing.ProviderMock,
	} {
		app := activeApp("other-"+typ, "s1", 8100+i)
		app.Type = typ
		apps = append(apps, app)
		id, model := "mo-"+typ, "other-model-"+typ
		mappings[app.ID] = []routing.ModelMapping{{ID: id, ApplicationID: app.ID, GatewayModelName: "g-" + id, AppModelName: model, Status: routing.ServerStatusActive}}
		// An audio_cpp spec on every one of them: the old kind gate alone would
		// accept it, so only the type gate keeps these from being probed.
		specs = append(specs, routing.RuntimeSpec{ID: "sp-" + id, MappingID: id, Type: string(routing.RuntimeSpecTypeAudioCpp)})
		prober.acVerdicts[model] = provider.AudioCppVerdicts{Speech: routing.CapabilityYes}
	}
	st := newHealthTestStore(apps...)
	st.mappings = mappings
	st.runtimeSpecs = specs
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil),
		settings: st, probeTimeout: time.Second, now: time.Now,
	}

	runCapabilityCycle(runner)

	if got, want := prober.audioCppAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("audio.cpp probe asked about %v, want exactly %v", got, want)
	}
	prober.acMu.Lock()
	defer prober.acMu.Unlock()
	if len(prober.acTargets) != 1 || prober.acTargets[0].Provider != routing.ProviderAudioCpp || prober.acTargets[0].Endpoint != "http://s1.local:8000" {
		t.Fatalf("audio.cpp probe targets = %+v, want exactly the one audio_cpp application", prober.acTargets)
	}
}

// TestRunAppHealthOnceAudioCppFollowsTheAppCadence pins the branch's own
// schedule key ("audiocpp:"+app.ID): with the clock standing still a second
// cycle does not ask again, and once the application's interval has elapsed it
// does. A branch sharing another pass's key would never be due at all (that
// pass stamps it first in the same cycle), and one with no schedule would ask
// on every cycle.
func TestRunAppHealthOnceAudioCppFollowsTheAppCadence(t *testing.T) {
	runner, _, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: routing.CapabilityYes}},
		audioCppMapping("m1", "pocket_tts"))
	current := time.Unix(10_000, 0).UTC()
	runner.now = func() time.Time { return current }
	state := &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)}

	runner.runOnce(context.Background(), state)
	runner.runOnce(context.Background(), state)
	if n := len(prober.audioCppAsked()); n != 1 {
		t.Fatalf("audio.cpp probe asked %d times over two cycles at one instant, want 1 -- it must follow the application's cadence", n)
	}
	if _, ok := state.lastProbed["audiocpp:a1"]; !ok {
		t.Fatalf("lastProbed = %v, want an audiocpp:a1 entry", state.lastProbed)
	}

	current = current.Add(time.Duration(portal.MaxHealthCheckIntervalSeconds) * time.Second)
	runner.runOnce(context.Background(), state)
	if n := len(prober.audioCppAsked()); n != 2 {
		t.Fatalf("audio.cpp probe asked %d times after the interval elapsed, want 2", n)
	}
}

// TestRunAppHealthOnceAudioCppCarriesTheAppCredential: the branch builds its
// auth context as the sdcpp branch does -- off the APPLICATION's own sealed
// token, since a static server has no runtime spec to hold a per-mapping one --
// so an audio.cpp server started with an API key is asked with it, for every
// mapping.
func TestRunAppHealthOnceAudioCppCarriesTheAppCredential(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t, nil,
		audioCppMapping("m1", "model-a"), audioCppMapping("m2", "model-b"))
	app := st.apps["s1"][0]
	app.APIToken = "plain:app-tok"
	app.APITokenHeader = "X-App-Key"
	st.apps["s1"][0] = app

	runCapabilityCycle(runner)

	prober.acMu.Lock()
	defer prober.acMu.Unlock()
	if len(prober.acAuth) != 2 {
		t.Fatalf("audio.cpp probe asked %d times, want 2 (one per mapping)", len(prober.acAuth))
	}
	for i, a := range prober.acAuth {
		// UpstreamAuth carries an ExtraHeaders map and is not comparable with ==.
		if a.Header != "X-App-Key" || a.Token != "app-tok" {
			t.Errorf("credential for %s = {Header:%q Token:%q}, want {Header:X-App-Key Token:app-tok}", prober.acModels[i], a.Header, a.Token)
		}
	}
}

// TestRunAppHealthOnceAudioCppAuthRejectedLogsAndNeverWrites is the
// misconfigured-token signal (issue #58) on this branch: a 401/403 means the
// gateway's own credential for the application is wrong, not that the model's
// capability is unknown, so it is logged where an operator sees it, distinct
// from the debug-level noise of an unreachable server, and writes nothing.
func TestRunAppHealthOnceAudioCppAuthRejectedLogsAndNeverWrites(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": {Speech: routing.CapabilityNo}},
		audioCppMapping("m1", "pocket_tts"))
	prober.acErr["pocket_tts"] = fmt.Errorf("wrapped: %w", provider.ErrAuthRejected)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	runCapabilityCycle(runner)

	if n := st.capabilityWriteCount(); n != 0 {
		t.Fatalf("UpsertMappingCapabilities called %d times on a rejected probe, want 0", n)
	}
	if !strings.Contains(buf.String(), "check the application's API token") {
		t.Fatalf("log output = %q, want the misconfigured-token signal", buf.String())
	}
}

// audioCppLoadedTTS is the verdict of a plain text-to-speech model the listing
// states as loaded RIGHT NOW: the one shape whose voices the audio.cpp pass
// fetches and caches (#251).
func audioCppLoadedTTS() provider.AudioCppVerdicts {
	return provider.AudioCppVerdicts{
		Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo,
		SpeechClone: routing.CapabilityNo, Loaded: true,
	}
}

// voiceList builds the lister's answer for the given names, in the given order.
func voiceList(names ...string) []provider.Voice {
	out := make([]provider.Voice, 0, len(names))
	for _, n := range names {
		out = append(out, provider.Voice{Name: n})
	}
	return out
}

// TestRunAppHealthOnceAudioCppCachesVoicesOfALoadedTTSModel is the branch's
// whole job for the voice picker (#251): the audio.cpp pass observes a mapping's
// model LOADED and tts, asks the server's voice listing, and stores the names on
// the mapping as a NORMALIZED cache row (deduplicated, sorted -- the server's
// order is cosmetic), so the picker can offer them even after the model is
// unloaded.
//
// The listing is asked with the mapping's AppModelName, the server's own target
// and the application's credential, exactly like the /v1/models probe beside it.
func TestRunAppHealthOnceAudioCppCachesVoicesOfALoadedTTSModel(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
		audioCppMapping("m1", "pocket_tts"))
	prober.acVoices["pocket_tts"] = voiceList("demo_2_man", "demo_1_man", "demo_2_man")
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return at }
	app := st.apps["s1"][0]
	app.APIToken = "plain:app-tok"
	app.APITokenHeader = "X-App-Key"
	st.apps["s1"][0] = app

	runCapabilityCycle(runner)

	if got, want := prober.voicesAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("voices listing asked about %v, want %v (the mapping's AppModelName, once)", got, want)
	}
	row, ok := st.voicesRowOf("m1")
	if !ok {
		t.Fatal("m1 has no voices row, want one")
	}
	want := routing.ModelMappingCacheRow{
		MappingID: "m1", CacheType: "voices", ScopeKey: "",
		Payload: `["demo_1_man","demo_2_man"]`, UpdatedAt: at,
	}
	if row != want {
		t.Fatalf("voices row = %+v, want %+v", row, want)
	}
	prober.acMu.Lock()
	defer prober.acMu.Unlock()
	if len(prober.acVoicesTargets) != 1 {
		t.Fatalf("voices listing called %d times, want 1", len(prober.acVoicesTargets))
	}
	if target := prober.acVoicesTargets[0]; target.Provider != routing.ProviderAudioCpp || target.Endpoint != "http://s1.local:8001" || target.Timeout != time.Second {
		t.Fatalf("voices target = %+v, want {Provider:audio_cpp Endpoint:http://s1.local:8001 Timeout:1s}", target)
	}
	// UpstreamAuth carries an ExtraHeaders map and is not comparable with ==.
	if a := prober.acVoicesAuth[0]; a.Header != "X-App-Key" || a.Token != "app-tok" {
		t.Fatalf("voices credential = {Header:%q Token:%q}, want {Header:X-App-Key Token:app-tok}", a.Header, a.Token)
	}
}

// TestRunAppHealthOnceAudioCppVoicesWriteIsChangeDetected: the pass runs every
// health tick, so a steady server must cost the cache no write. The listing is
// still asked each tick (that is how a change is noticed); only an unchanged
// NORMALIZED payload suppresses the write, so a server that merely re-orders or
// repeats its names does not flap the row, and a real change does write.
func TestRunAppHealthOnceAudioCppVoicesWriteIsChangeDetected(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
		audioCppMapping("m1", "pocket_tts"))
	prober.acVoices["pocket_tts"] = voiceList("b", "a")

	runCapabilityCycle(runner)
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 1 {
		t.Fatalf("cache writes after the first tick = %d, want 1", n)
	}
	st.assertVoicesPayload(t, "m1", `["a","b"]`)

	runCapabilityCycle(runner)
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 1 {
		t.Fatalf("cache writes after an identical second tick = %d, want still 1 (change detection)", n)
	}

	prober.acVoices["pocket_tts"] = voiceList("a", "b", "a")
	runCapabilityCycle(runner)
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 1 {
		t.Fatalf("cache writes after a re-ordered, repeated listing = %d, want still 1 (the payload is normalized before the compare)", n)
	}

	prober.acVoices["pocket_tts"] = voiceList("a", "b", "c")
	runCapabilityCycle(runner)
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 2 {
		t.Fatalf("cache writes after a changed listing = %d, want 2", n)
	}
	st.assertVoicesPayload(t, "m1", `["a","b","c"]`)

	if got := len(prober.voicesAsked()); got != 4 {
		t.Fatalf("voices listing asked %d times over four ticks, want 4 (it is asked every tick; only the write is skipped)", got)
	}
}

// TestAudioCppVoicesWanted pins the verdict gate of the static voices and speech
// language steps on its own: a model the listing states as LOADED right now and as a
// speech model -- (speech, yes), whatever its (speech_clone) verdict. The clone verdict
// is not a term (#301): audio.cpp's voices listing is task-independent, so a clone
// model's configured voice_presets are worth caching, and an empty listing is what
// keeps a reference-only clone hidden downstream.
func TestAudioCppVoicesWanted(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	for _, tc := range []struct {
		name string
		v    provider.AudioCppVerdicts
		want bool
	}{
		{"a loaded clone model", provider.AudioCppVerdicts{Loaded: true, Speech: yes, SpeechClone: yes}, true},
		{"a loaded plain tts model", provider.AudioCppVerdicts{Loaded: true, Speech: yes, SpeechClone: no}, true},
		{"a loaded speech model whose clone verdict is unknown", provider.AudioCppVerdicts{Loaded: true, Speech: yes}, true},
		{"a speech model that is not loaded", provider.AudioCppVerdicts{Loaded: false, Speech: yes}, false},
		{"a clone model that is not loaded", provider.AudioCppVerdicts{Loaded: false, Speech: yes, SpeechClone: yes}, false},
		{"a loaded model that is not a speech model", provider.AudioCppVerdicts{Loaded: true, Speech: no}, false},
		{"a loaded model whose task is not stated", provider.AudioCppVerdicts{Loaded: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := audioCppVoicesWanted(tc.v); got != tc.want {
				t.Fatalf("audioCppVoicesWanted(%+v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// TestAudioCppSpeechRateWantedStillExcludesClone guards the one gate that must NOT follow
// audioCppVoicesWanted when it admits clone models (#301): the speech rate benchmark
// synthesizes with a plain request, which a clone model refuses without a reference voice,
// and streaming clone models are out of scope here. A loaded, streaming clone model is
// therefore still not a rate target, while the same listing as a plain tts model is.
func TestAudioCppSpeechRateWantedStillExcludesClone(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	clone := provider.AudioCppVerdicts{Loaded: true, Speech: yes, SpeechClone: yes, SpeechStreaming: yes}
	if audioCppSpeechRateWanted(clone) {
		t.Fatalf("audioCppSpeechRateWanted(%+v) = true, want false: a clone model is never rate-benchmarked", clone)
	}
	plain := provider.AudioCppVerdicts{Loaded: true, Speech: yes, SpeechClone: no, SpeechStreaming: yes}
	if !audioCppSpeechRateWanted(plain) {
		t.Fatalf("audioCppSpeechRateWanted(%+v) = false, want true (the control: a plain streaming tts model is a target)", plain)
	}
}

// TestRunAppHealthOnceAudioCppVoicesGate pins WHICH mappings are asked: only a
// model the listing states as LOADED right now (the server answers for the
// model it has loaded, so asking for any other would attribute another model's
// voices to this one) and only a speech one -- (speech, yes), whatever its
// (speech_clone) verdict (#301: the listing is task-independent, and a clone model
// with configured voice_presets lists them). Everything else is never fetched and
// never written, however the rest of the verdict reads.
func TestRunAppHealthOnceAudioCppVoicesGate(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	for _, tc := range []struct {
		name      string
		v         provider.AudioCppVerdicts
		wantFetch bool
	}{
		{"a loaded tts model", provider.AudioCppVerdicts{Speech: yes, Transcription: no, SpeechClone: no, Loaded: true}, true},
		{"a loaded speech model whose clone verdict is unknown", provider.AudioCppVerdicts{Speech: yes, Transcription: no, Loaded: true}, true},
		{"a tts model that is not loaded", provider.AudioCppVerdicts{Speech: yes, Transcription: no, SpeechClone: no}, false},
		{"a loaded clone model", provider.AudioCppVerdicts{Speech: yes, Transcription: no, SpeechClone: yes, Loaded: true}, true},
		{"a clone model that is not loaded", provider.AudioCppVerdicts{Speech: yes, Transcription: no, SpeechClone: yes}, false},
		{"a loaded asr model", provider.AudioCppVerdicts{Speech: no, Transcription: yes, Loaded: true}, false},
		{"a loaded model that is no speech and no transcription model", provider.AudioCppVerdicts{Speech: no, Transcription: no, Loaded: true}, false},
		{"a loaded model whose task the listing does not state", provider.AudioCppVerdicts{Loaded: true}, false},
		{"a model the listing does not name", provider.AudioCppVerdicts{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober := audioCppProbeRunner(t,
				map[string]provider.AudioCppVerdicts{"pocket_tts": tc.v},
				audioCppMapping("m1", "pocket_tts"))
			prober.acVoices["pocket_tts"] = voiceList("a")

			runCapabilityCycle(runner)

			asked := prober.voicesAsked()
			if tc.wantFetch {
				if want := []string{"pocket_tts"}; !reflect.DeepEqual(asked, want) {
					t.Fatalf("voices listing asked about %v, want %v", asked, want)
				}
				st.assertVoicesPayload(t, "m1", `["a"]`)
				return
			}
			if len(asked) != 0 {
				t.Fatalf("voices listing asked about %v, want no request", asked)
			}
			if row, ok := st.voicesRowOf("m1"); ok {
				t.Fatalf("m1 has a voices row %+v, want none", row)
			}
			if n := st.cacheWriteCount(); n != 0 {
				t.Fatalf("cache written %d times, want 0", n)
			}
		})
	}
}

// TestRunAppHealthOnceAudioCppVoicesAreAttributedToTheLoadedMappingOnly: one
// server, several mappings, two loaded speech models (a plain tts one and a clone
// one) and one unloaded sibling. Each loaded model's voices belong to ITS mapping and
// to no other: the unloaded sibling is never asked, and the clone model's presets do
// not land on the plain tts mapping or the reverse (#301: a loaded clone model is
// asked like any other loaded speech model).
func TestRunAppHealthOnceAudioCppVoicesAreAttributedToTheLoadedMappingOnly(t *testing.T) {
	cloneLoaded := provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes, Loaded: true}
	idleTTS := audioCppLoadedTTS()
	idleTTS.Loaded = false
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{
			"tts_loaded": audioCppLoadedTTS(), "tts_idle": idleTTS, "clone_loaded": cloneLoaded,
		},
		audioCppMapping("m1", "tts_loaded"), audioCppMapping("m2", "tts_idle"), audioCppMapping("m3", "clone_loaded"))
	for _, model := range []string{"tts_loaded", "tts_idle", "clone_loaded"} {
		prober.acVoices[model] = voiceList("v_" + model)
	}

	runCapabilityCycle(runner)

	asked := prober.voicesAsked()
	sort.Strings(asked)
	if want := []string{"clone_loaded", "tts_loaded"}; !reflect.DeepEqual(asked, want) {
		t.Fatalf("voices listing asked about %v, want %v", asked, want)
	}
	st.assertVoicesPayload(t, "m1", `["v_tts_loaded"]`)
	st.assertVoicesPayload(t, "m3", `["v_clone_loaded"]`)
	if row, ok := st.voicesRowOf("m2"); ok {
		t.Fatalf("m2 has a voices row %+v, want none", row)
	}
}

// TestRunAppHealthOnceAudioCppVoicesCloneModel pins what a loaded clone model gets
// cached (#301): the voice_presets names audio.cpp lists for it, normalized like any
// other model's, and a genuine empty list -- written, not skipped -- for a
// reference-only clone with no presets, because "fetched, no presets" is the fact that
// keeps it hidden downstream, which is not the same as "never asked" (no row).
func TestRunAppHealthOnceAudioCppVoicesCloneModel(t *testing.T) {
	cloneLoaded := provider.AudioCppVerdicts{Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityYes, Loaded: true}
	for _, tc := range []struct {
		name   string
		voices []provider.Voice
		want   string
	}{
		{"a clone model with presets", voiceList("preset_b", "preset_a"), `["preset_a","preset_b"]`},
		{"a reference-only clone model", []provider.Voice{}, `[]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober := audioCppProbeRunner(t,
				map[string]provider.AudioCppVerdicts{"clone_tts": cloneLoaded},
				audioCppMapping("m1", "clone_tts"))
			prober.acVoices["clone_tts"] = tc.voices

			runCapabilityCycle(runner)

			if got, want := prober.voicesAsked(), []string{"clone_tts"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("voices listing asked about %v, want %v", got, want)
			}
			st.assertVoicesPayload(t, "m1", tc.want)
		})
	}
}

// TestRunAppHealthOnceAudioCppVoicesFetchErrorKeepsTheRow is last-known-good: a
// failed listing (unreachable, non-2xx, an undecodable body) says nothing about
// the model's voices, so it writes nothing and the row on file survives. The
// fake hands voices back beside the error, so only the error guard stands
// between them and the row.
func TestRunAppHealthOnceAudioCppVoicesFetchErrorKeepsTheRow(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
		audioCppMapping("m1", "pocket_tts"))
	prober.acVoices["pocket_tts"] = voiceList("would_overwrite")
	prober.acVoicesErr["pocket_tts"] = fmt.Errorf("%w: status 503", provider.ErrUnavailable)
	st.seedVoicesCache("m1", `["old_a","old_b"]`)

	runCapabilityCycle(runner)

	if n := len(prober.voicesAsked()); n != 1 {
		t.Fatalf("voices listing asked %d times, want 1 (the fetch must have run for this to mean anything)", n)
	}
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 0 {
		t.Fatalf("cache written %d times after a failed listing, want 0", n)
	}
	st.assertVoicesPayload(t, "m1", `["old_a","old_b"]`)
}

// TestRunAppHealthOnceAudioCppVoicesGenuineEmptyListingIsWritten: a listing that
// SUCCEEDS with no voices is the truth for a model that has none, so it is
// written -- over an earlier non-empty row, and as the first row for a mapping
// with none (an absent row means "never asked", which "fetched, nothing to
// offer" is not). The empty payload is itself change-detected.
func TestRunAppHealthOnceAudioCppVoicesGenuineEmptyListingIsWritten(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed string // "" = no row on file
	}{
		{"as the first row", ""},
		{"over an earlier non-empty row", `["old"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober := audioCppProbeRunner(t,
				map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
				audioCppMapping("m1", "pocket_tts"))
			prober.acVoices["pocket_tts"] = []provider.Voice{} // a genuine empty, nil error
			if tc.seed != "" {
				st.seedVoicesCache("m1", tc.seed)
			}

			runCapabilityCycle(runner)

			st.assertVoicesPayload(t, "m1", `[]`)
			if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 1 {
				t.Fatalf("cache written %d times, want 1", n)
			}

			runCapabilityCycle(runner)
			if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 1 {
				t.Fatalf("cache written %d times after a second empty listing, want still 1", n)
			}
		})
	}
}

// TestRunAppHealthOnceAudioCppVoicesBackOffAfterAnError: a server that does not
// serve the listing (an older audio.cpp, a proxy without the route) must not be
// asked once per mapping per tick for as long as the model stays loaded. A
// failed listing starts a back-off for THAT mapping: within it the listing is
// not asked; once it has elapsed the next tick asks again, and a success (or the
// model leaving the loaded set) clears it, so a reload retries at once.
func TestRunAppHealthOnceAudioCppVoicesBackOffAfterAnError(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
		audioCppMapping("m1", "pocket_tts"))
	current := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return current }
	prober.acVoicesErr["pocket_tts"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)
	asked := func() int { return len(prober.voicesAsked()) }

	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times after the first tick, want 1", n)
	}

	current = current.Add(audioCppVoicesBackoff / 2)
	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times inside the back-off, want still 1", n)
	}

	current = current.Add(audioCppVoicesBackoff)
	runCapabilityCycle(runner)
	if n := asked(); n != 2 {
		t.Fatalf("asked %d times after the back-off elapsed, want 2", n)
	}
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 0 {
		t.Fatalf("cache written %d times while every listing failed, want 0", n)
	}

	// The server starts answering: the success is written and clears the
	// back-off, so the following ticks ask again without waiting.
	delete(prober.acVoicesErr, "pocket_tts")
	prober.acVoices["pocket_tts"] = voiceList("a")
	current = current.Add(audioCppVoicesBackoff + time.Second)
	runCapabilityCycle(runner)
	runCapabilityCycle(runner)
	if n := asked(); n != 4 {
		t.Fatalf("asked %d times after the server recovered, want 4 (a success clears the back-off)", n)
	}
	st.assertVoicesPayload(t, "m1", `["a"]`)

	// An unload clears a fresh back-off too: the reload is asked at once.
	prober.acVoicesErr["pocket_tts"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)
	runCapabilityCycle(runner) // fails, starts a back-off
	before := asked()
	unloaded := audioCppLoadedTTS()
	unloaded.Loaded = false
	prober.acMu.Lock()
	prober.acVerdicts["pocket_tts"] = unloaded
	prober.acMu.Unlock()
	runCapabilityCycle(runner)
	prober.acMu.Lock()
	prober.acVerdicts["pocket_tts"] = audioCppLoadedTTS()
	prober.acMu.Unlock()
	runCapabilityCycle(runner)
	if n := asked(); n != before+1 {
		t.Fatalf("asked %d times after an unload and reload, want %d (the reload retries inside the old back-off window)", n, before+1)
	}
}

// audioCppModelsOnlyProber is a prober that reads /v1/models but implements no
// voices listing -- every provider client but the OpenAI-compatible one.
type audioCppModelsOnlyProber struct {
	*fakeProber
	inner *audioCppFakeProber
}

func (p *audioCppModelsOnlyProber) ProbeAudioCppModels(ctx context.Context, target routing.Target, modelID string) (provider.AudioCppVerdicts, error) {
	return p.inner.ProbeAudioCppModels(ctx, target, modelID)
}

// TestRunAppHealthOnceAudioCppVoicesWithoutAListerStillWritesVerdicts: a prober
// that cannot list voices skips the voices step, silently, and the capability
// verdicts the same pass derives are unaffected -- as is the speech language fact,
// which rides the verdict and asks the server for nothing.
func TestRunAppHealthOnceAudioCppVoicesWithoutAListerStillWritesVerdicts(t *testing.T) {
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"pocket_tts": audioCppLoadedTTS()},
		audioCppMapping("m1", "pocket_tts"))
	runner.prober = &audioCppModelsOnlyProber{fakeProber: prober.fakeProber, inner: prober}

	runCapabilityCycle(runner)

	st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	if row, ok := st.voicesRowOf("m1"); ok {
		t.Fatalf("m1 has a voices row %+v, want none: nothing could list voices", row)
	}
	if n := st.cacheWriteCountOf(routing.ModelMappingCacheVoices); n != 0 {
		t.Fatalf("cache written %d times, want 0", n)
	}
	if _, ok := st.speechLanguageRowOf("m1"); !ok {
		t.Fatal("m1 has no speech_language row, want one: the step needs no voices lister")
	}
}

// TestRunAppHealthOnceAudioCppVoicesStoreFailuresAreBestEffort: the voices cache
// is a convenience on the side of the health loop, so a cache read or write that
// fails is logged and the pass carries on -- the capability verdicts and the
// next mapping are unaffected.
func TestRunAppHealthOnceAudioCppVoicesStoreFailuresAreBestEffort(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*fakeHealthStore)
	}{
		{"a failing cache read", func(st *fakeHealthStore) { st.cacheReadErr = true }},
		{"a failing cache write", func(st *fakeHealthStore) { st.cacheWriteErr = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober := audioCppProbeRunner(t,
				map[string]provider.AudioCppVerdicts{"tts_a": audioCppLoadedTTS(), "tts_b": audioCppLoadedTTS()},
				audioCppMapping("m1", "tts_a"), audioCppMapping("m2", "tts_b"))
			prober.acVoices["tts_a"], prober.acVoices["tts_b"] = voiceList("a"), voiceList("b")
			tc.setup(st)
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			runCapabilityCycle(runner)

			if n := len(prober.voicesAsked()); n != 2 {
				t.Fatalf("voices listing asked %d times, want 2 (one failing mapping must not stop the next)", n)
			}
			st.assertCapability(t, "m1", routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
			if !strings.Contains(buf.String(), "voices") {
				t.Fatalf("log output = %q, want the failure logged", buf.String())
			}
		})
	}
}

// TestWriteVoicesCache pins the shared writer (the audio.cpp pass here, the
// agent-child pass later): it stores a NORMALIZED payload -- names deduplicated
// and sorted bytewise, blanks dropped, the rest verbatim because the match
// against the server is exact -- as a JSON array, reports whether it wrote, and
// writes only when the normalized payload differs from the row on file.
func TestWriteVoicesCache(t *testing.T) {
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	newRunner := func() (*appHealthRunner, *fakeHealthStore) {
		st := newHealthTestStore()
		return &appHealthRunner{store: st, now: func() time.Time { return at }}, st
	}

	t.Run("normalizes: dedupe, sort, drop blanks, keep names verbatim", func(t *testing.T) {
		r, st := newRunner()
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("b", "A", "a", "b", "", "  ", ` q"x `))
		if err != nil || !changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (true, nil)", changed, err)
		}
		// Bytewise order: "A" < "a" < "b", and the padded, quoted name stays as is.
		st.assertVoicesPayload(t, "m1", `[" q\"x ","A","a","b"]`)
		row, _ := st.voicesRowOf("m1")
		if row.CacheType != "voices" || row.ScopeKey != "" || !row.UpdatedAt.Equal(at) {
			t.Fatalf("row = %+v, want cache type voices, empty scope key, UpdatedAt %v", row, at)
		}
	})

	t.Run("an unchanged payload writes nothing and reports changed=false", func(t *testing.T) {
		r, st := newRunner()
		if _, err := r.writeVoicesCache(context.Background(), "m1", voiceList("a", "b")); err != nil {
			t.Fatal(err)
		}
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("b", "a", "b"))
		if err != nil || changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (false, nil)", changed, err)
		}
		if n := st.cacheWriteCount(); n != 1 {
			t.Fatalf("cache written %d times, want 1", n)
		}
	})

	t.Run("a different payload replaces the row", func(t *testing.T) {
		r, st := newRunner()
		st.seedVoicesCache("m1", `["a"]`)
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("a", "b"))
		if err != nil || !changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (true, nil)", changed, err)
		}
		st.assertVoicesPayload(t, "m1", `["a","b"]`)
	})

	t.Run("no voices at all is the empty array, never null", func(t *testing.T) {
		for name, in := range map[string][]provider.Voice{"nil": nil, "empty": {}, "only blanks": voiceList("", " ")} {
			r, st := newRunner()
			changed, err := r.writeVoicesCache(context.Background(), "m1", in)
			if err != nil || !changed {
				t.Fatalf("%s: writeVoicesCache = (%v, %v), want (true, nil)", name, changed, err)
			}
			st.assertVoicesPayload(t, "m1", `[]`)
		}
	})

	t.Run("a legacy or foreign payload for the same voices is rewritten once to the canonical form", func(t *testing.T) {
		r, st := newRunner()
		st.seedVoicesCache("m1", `[ "a", "b" ]`)
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("a", "b"))
		if err != nil || !changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (true, nil)", changed, err)
		}
		st.assertVoicesPayload(t, "m1", `["a","b"]`)
	})

	t.Run("a cache read failure is an error and writes nothing", func(t *testing.T) {
		r, st := newRunner()
		st.cacheReadErr = true
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("a"))
		if err == nil || changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (false, error)", changed, err)
		}
		if n := st.cacheWriteCount(); n != 0 {
			t.Fatalf("cache written %d times after a failed read, want 0", n)
		}
	})

	t.Run("a cache write failure is an error and not a change", func(t *testing.T) {
		r, st := newRunner()
		st.cacheWriteErr = true
		changed, err := r.writeVoicesCache(context.Background(), "m1", voiceList("a"))
		if err == nil || changed {
			t.Fatalf("writeVoicesCache = (%v, %v), want (false, error)", changed, err)
		}
	})
}

// speechLanguageRowOf reads one mapping's stored speech_language row back. ok is
// false when the mapping has NO such row, which is how "never observed loaded"
// (unknown) differs from "observed, and it declares no language param" (a row
// whose payload says accepts:false).
func (f *fakeHealthStore) speechLanguageRowOf(mappingID string) (routing.ModelMappingCacheRow, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.cache[mappingID][modelMappingCacheKey{routing.ModelMappingCacheSpeechLanguage, ""}]
	return row, ok
}

// TestCacheAudioCppSpeechParams pins the static pass's speech-language step for ONE
// mapping: a model observed LOADED and plain tts has the `language` fact its
// /v1/models verdict carried cached on the mapping (routing.ModelMappingCacheSpeechLanguage),
// change-only; any other model has NO row written and a row already on file left alone.
// The step reads nothing of its own -- the fact rides the verdict -- so there is no
// fetch to fail and no back-off.
func TestCacheAudioCppSpeechParams(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	newRunner := func() (*appHealthRunner, *fakeHealthStore) {
		st := newHealthTestStore()
		return &appHealthRunner{store: st, now: func() time.Time { return at }}, st
	}
	mp := audioCppMapping("m1", "qwen3_tts")
	withLanguage := func(v provider.AudioCppVerdicts, accepts bool, def string) provider.AudioCppVerdicts {
		v.AcceptsLanguage, v.LanguageDefault = accepts, def
		return v
	}

	t.Run("a loaded plain tts model that accepts language is cached with its default", func(t *testing.T) {
		r, st := newRunner()
		r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(audioCppLoadedTTS(), true, "zh"))
		row, ok := st.speechLanguageRowOf("m1")
		if !ok {
			t.Fatal("m1 has no speech_language row, want one")
		}
		want := routing.ModelMappingCacheRow{
			MappingID: "m1", CacheType: "speech_language", ScopeKey: "",
			Payload: `{"accepts":true,"default":"zh"}`, UpdatedAt: at,
		}
		if row != want {
			t.Fatalf("speech_language row = %+v, want %+v", row, want)
		}
		if n := st.cacheWriteCount(); n != 1 {
			t.Fatalf("cache written %d times, want 1", n)
		}
	})

	t.Run("a loaded clone model has its language fact cached too", func(t *testing.T) {
		// #301: the language step rides audioCppVoicesWanted, which no longer excludes a
		// clone model, so a clone model that accepts language is offered the control as well.
		r, st := newRunner()
		v := withLanguage(audioCppLoadedTTS(), true, "de")
		v.SpeechClone = routing.CapabilityYes
		r.cacheAudioCppSpeechParams(context.Background(), mp, v)
		if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":true,"default":"de"}` {
			t.Fatalf("speech_language payload = %s, want the clone model's fact {accepts:true,default:de}", got)
		}
	})

	t.Run("a second identical tick writes nothing", func(t *testing.T) {
		r, st := newRunner()
		v := withLanguage(audioCppLoadedTTS(), true, "zh")
		r.cacheAudioCppSpeechParams(context.Background(), mp, v)
		r.cacheAudioCppSpeechParams(context.Background(), mp, v)
		if n := st.cacheWriteCount(); n != 1 {
			t.Fatalf("cache written %d times over two identical ticks, want 1", n)
		}
	})

	t.Run("a loaded plain tts model that declares no language param is a real no", func(t *testing.T) {
		r, st := newRunner()
		r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(audioCppLoadedTTS(), false, ""))
		row, ok := st.speechLanguageRowOf("m1")
		if !ok || row.Payload != `{"accepts":false,"default":""}` {
			t.Fatalf("speech_language row = %+v (present=%v), want a row {accepts:false,default:\"\"}: accepts:false is a stated fact, not an absence", row, ok)
		}
	})

	t.Run("a changed fact replaces the row", func(t *testing.T) {
		r, st := newRunner()
		st.seedCacheRow("m1", routing.ModelMappingCacheSpeechLanguage, routing.SpeechLanguagePayload(true, "auto"))
		r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(audioCppLoadedTTS(), true, "zh"))
		if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":true,"default":"zh"}` {
			t.Fatalf("speech_language payload = %s, want the new default zh", got)
		}
	})

	// Every model audioCppVoicesWanted refuses writes NO row, and a row already on file
	// (the fact learned while the model was loaded) stays: an unload does not retract it.
	notWanted := []struct {
		name string
		v    provider.AudioCppVerdicts
	}{
		{"an unloaded model", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, SpeechClone: routing.CapabilityNo, Loaded: false}},
		{"a clone model that is not loaded", provider.AudioCppVerdicts{Speech: routing.CapabilityYes, SpeechClone: routing.CapabilityYes, Loaded: false}},
		{"an asr model", provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, Loaded: true}},
		{"a model whose speech verdict is unknown", provider.AudioCppVerdicts{Loaded: true}},
	}
	for _, tc := range notWanted {
		t.Run(tc.name+" writes no row", func(t *testing.T) {
			r, st := newRunner()
			r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(tc.v, true, "zh"))
			if row, ok := st.speechLanguageRowOf("m1"); ok {
				t.Fatalf("speech_language row = %+v, want none", row)
			}
			if n := st.cacheWriteCount(); n != 0 {
				t.Fatalf("cache written %d times, want 0", n)
			}
		})
		t.Run(tc.name+" leaves the row on file alone", func(t *testing.T) {
			r, st := newRunner()
			st.seedCacheRow("m1", routing.ModelMappingCacheSpeechLanguage, routing.SpeechLanguagePayload(false, ""))
			r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(tc.v, true, "zh"))
			if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":false,"default":""}` {
				t.Fatalf("speech_language payload = %s, want the seeded last-known row untouched", got)
			}
			if n := st.cacheWriteCount(); n != 0 {
				t.Fatalf("cache written %d times, want 0", n)
			}
		})
	}

	t.Run("a store failure is logged and does not panic", func(t *testing.T) {
		for name, setup := range map[string]func(*fakeHealthStore){
			"read":  func(st *fakeHealthStore) { st.cacheReadErr = true },
			"write": func(st *fakeHealthStore) { st.cacheWriteErr = true },
		} {
			r, st := newRunner()
			setup(st)
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })
			r.cacheAudioCppSpeechParams(context.Background(), mp, withLanguage(audioCppLoadedTTS(), true, "zh"))
			if !strings.Contains(buf.String(), "speech language") {
				t.Fatalf("%s failure: log output = %q, want it logged", name, buf.String())
			}
		}
	})
}

// TestWriteSpeechLanguageCache pins the writer the static pass and a later agent pass
// share: a canonical routing.SpeechLanguagePayload row at scope "", written only when it
// differs from the row on file, with changed reporting whether it wrote.
func TestWriteSpeechLanguageCache(t *testing.T) {
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	newRunner := func() (*appHealthRunner, *fakeHealthStore) {
		st := newHealthTestStore()
		return &appHealthRunner{store: st, now: func() time.Time { return at }}, st
	}

	t.Run("writes the canonical payload and reports a change", func(t *testing.T) {
		r, st := newRunner()
		changed, err := r.writeSpeechLanguageCache(context.Background(), "m1", true, "auto")
		if err != nil || !changed {
			t.Fatalf("writeSpeechLanguageCache = (%v, %v), want (true, nil)", changed, err)
		}
		row, ok := st.speechLanguageRowOf("m1")
		if !ok || row.Payload != routing.SpeechLanguagePayload(true, "auto") || row.ScopeKey != "" || !row.UpdatedAt.Equal(at) {
			t.Fatalf("row = %+v (present=%v), want the canonical payload at scope \"\" stamped %v", row, ok, at)
		}
	})

	t.Run("an unchanged fact writes nothing and reports changed=false", func(t *testing.T) {
		r, st := newRunner()
		if _, err := r.writeSpeechLanguageCache(context.Background(), "m1", false, ""); err != nil {
			t.Fatal(err)
		}
		changed, err := r.writeSpeechLanguageCache(context.Background(), "m1", false, "")
		if err != nil || changed {
			t.Fatalf("writeSpeechLanguageCache = (%v, %v), want (false, nil)", changed, err)
		}
		if n := st.cacheWriteCount(); n != 1 {
			t.Fatalf("cache written %d times, want 1", n)
		}
	})

	t.Run("a payload in another layout for the same fact is rewritten once to the canonical one", func(t *testing.T) {
		r, st := newRunner()
		st.seedCacheRow("m1", routing.ModelMappingCacheSpeechLanguage, `{ "accepts": true, "default": "zh" }`)
		changed, err := r.writeSpeechLanguageCache(context.Background(), "m1", true, "zh")
		if err != nil || !changed {
			t.Fatalf("writeSpeechLanguageCache = (%v, %v), want (true, nil)", changed, err)
		}
		if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":true,"default":"zh"}` {
			t.Fatalf("payload = %s, want the canonical form", got)
		}
	})

	t.Run("a cache read failure is an error and writes nothing", func(t *testing.T) {
		r, st := newRunner()
		st.cacheReadErr = true
		changed, err := r.writeSpeechLanguageCache(context.Background(), "m1", true, "zh")
		if err == nil || changed {
			t.Fatalf("writeSpeechLanguageCache = (%v, %v), want (false, error)", changed, err)
		}
		if n := st.cacheWriteCount(); n != 0 {
			t.Fatalf("cache written %d times after a failed read, want 0", n)
		}
	})

	t.Run("a cache write failure is an error and not a change", func(t *testing.T) {
		r, st := newRunner()
		st.cacheWriteErr = true
		changed, err := r.writeSpeechLanguageCache(context.Background(), "m1", true, "zh")
		if err == nil || changed {
			t.Fatalf("writeSpeechLanguageCache = (%v, %v), want (false, error)", changed, err)
		}
	})
}

// TestRunAppHealthOnceAudioCppCachesSpeechLanguage drives the step through the whole
// static pass (the wiring, not just the step): the loaded plain tts model's mapping gets
// its language fact from the same /v1/models verdict read, a second tick with nothing
// changed does not rewrite the row, and a mapping whose model is not a loaded plain tts
// one gets none.
func TestRunAppHealthOnceAudioCppCachesSpeechLanguage(t *testing.T) {
	loadedLang := audioCppLoadedTTS()
	loadedLang.AcceptsLanguage, loadedLang.LanguageDefault = true, "zh"
	idle := loadedLang
	idle.Loaded = false
	runner, st, _ := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"qwen3_tts": loadedLang, "idle_tts": idle},
		audioCppMapping("m1", "qwen3_tts"), audioCppMapping("m2", "idle_tts"))
	first := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return first }

	runCapabilityCycle(runner)

	row, ok := st.speechLanguageRowOf("m1")
	if !ok || row.Payload != `{"accepts":true,"default":"zh"}` || !row.UpdatedAt.Equal(first) {
		t.Fatalf("m1 speech_language row = %+v (present=%v), want {accepts:true,default:zh} stamped %v", row, ok, first)
	}
	if row, ok := st.speechLanguageRowOf("m2"); ok {
		t.Fatalf("m2 (model not loaded) speech_language row = %+v, want none", row)
	}

	runner.now = func() time.Time { return first.Add(time.Minute) }
	runCapabilityCycle(runner)
	if row, _ := st.speechLanguageRowOf("m1"); !row.UpdatedAt.Equal(first) {
		t.Fatalf("m1 speech_language row restamped to %v on an unchanged tick, want it left at %v", row.UpdatedAt, first)
	}
}

// TestRunAppHealthOnceAudioCppFailedProbeLeavesSpeechLanguageAlone: a /v1/models read that
// FAILS states nothing, so the pass moves on before the language step and the row already
// on file (the last fact learned while the model was loaded) is left exactly as it was.
// The fake returns a loaded accepts:true verdict BESIDE the error, so a pass that ignored
// the error would overwrite the seeded accepts:false row and be caught.
func TestRunAppHealthOnceAudioCppFailedProbeLeavesSpeechLanguageAlone(t *testing.T) {
	v := audioCppLoadedTTS()
	v.AcceptsLanguage, v.LanguageDefault = true, "zh"
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"qwen3_tts": v},
		audioCppMapping("m1", "qwen3_tts"))
	prober.acErr["qwen3_tts"] = errors.New("probe: connection refused")
	seeded := routing.SpeechLanguagePayload(false, "")
	st.seedCacheRow("m1", routing.ModelMappingCacheSpeechLanguage, seeded)

	runCapabilityCycle(runner)

	if got := len(prober.audioCppAsked()); got != 1 {
		t.Fatalf("audio.cpp probe asked %d times, want 1 (the failed read must have happened)", got)
	}
	if got, ok := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); !ok || got != seeded {
		t.Fatalf("speech_language payload = %q (present=%v), want the seeded %s left untouched", got, ok, seeded)
	}
}

// TestAudioCppModelsPassE2EWritesSpeechAndTranscriptionVerdicts drives the audio.cpp branch
// through the REAL production chain -- providerClients' Multiplexer, its
// OpenAI-compatible client, a MemoryStore -- against an httptest server serving
// a /v1/models listing in the shape measured against audiocpp_server v0.9.1.
// The fake-prober tests above cannot see the one failure that matters most
// here: the branch type-asserts r.prober to provider.AudioCppModelsProber and
// SKIPS silently when that fails, so a Multiplexer that stopped implementing
// it, or a providerClients that stopped routing audio_cpp to a client that
// does, would leave every audio mapping without a verdict and every fake test
// green. The application is a static audio_cpp one with no runtime spec, the
// deployment the type exists for.
//
// The same chain carries the voices cache (#251): the one LOADED tts model's
// voices come from the server's /v1/audio/voices and land, normalized, on its
// mapping's model_mapping_cache row -- and on no other mapping's: the loaded asr
// model and the model the listing does not name are never asked. The unloaded
// entry of the real listing shape (loaded:false) is part of the fixture, so a
// pass that ignored `loaded` would ask for its voices and fail the path check.
func TestAudioCppModelsPassE2EWritesSpeechAndTranscriptionVerdicts(t *testing.T) {
	shrinkRetryGap(t)
	var paths []string
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch r.URL.Path {
		case "/v1/models":
		case "/v1/audio/voices":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"voices":["demo_2_man","demo_1_man","demo_2_man"]}`))
			return
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[` +
			`{"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"/m/pocket","params":[{"name":"language","type":"string","default":"auto"}]},` +
			`{"id":"whisper_small","object":"model","owned_by":"engine","family":"whisper","task":"asr","mode":"offline","loaded":true,"path":"/m/whisper"},` +
			`{"id":"idle_tts","object":"model","owned_by":"engine","family":"pocket_tts","task":"tts","mode":"offline","loaded":false,"path":"/m/idle","params":[{"name":"language","type":"string","default":"auto"}]}]}`))
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

	ctx := context.Background()
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	store := routing.NewMemoryStore()
	if err := store.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "S1", Domain: u.Hostname(), Provider: routing.ProviderAudioCpp,
		Endpoint: upstream.URL, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server: %v", err)
	}
	// always_reachable: the audio branch is independent of the health-check
	// mode, so the only request the upstream sees is the model listing.
	if err := store.CreateApplication(ctx, routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderAudioCpp, Port: port, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 1, Weight: 1,
		TimeoutMS: 30000, AffinityTTLSeconds: 300, Status: routing.ServerStatusActive,
		HealthCheckMode: routing.HealthCheckModeAlwaysReachable, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create application: %v", err)
	}
	for _, m := range []routing.ModelMapping{
		{ID: "m1", ApplicationID: "a1", GatewayModelName: "tts", AppModelName: "pocket_tts_english_bf16", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "m2", ApplicationID: "a1", GatewayModelName: "asr", AppModelName: "whisper_small", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "m3", ApplicationID: "a1", GatewayModelName: "idle", AppModelName: "not_loaded", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
		{ID: "m4", ApplicationID: "a1", GatewayModelName: "tts-idle", AppModelName: "idle_tts", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now},
	} {
		if err := store.CreateMapping(ctx, m); err != nil {
			t.Fatalf("create mapping %s: %v", m.ID, err)
		}
	}

	(&appHealthRunner{
		store: store, prober: providerClients(0, false, nil), registry: gateway.NewAppHealthRegistry(nil),
		settings: staticSettings(nil), probeTimeout: time.Second, now: func() time.Time { return now },
	}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: make(map[string]availWriteState)})

	capabilityRow := func(mappingID, capability string) (routing.CapabilityRow, bool) {
		rows, err := store.MappingCapabilities(ctx, mappingID)
		if err != nil {
			t.Fatalf("mapping capabilities %s: %v", mappingID, err)
		}
		row, ok := routing.CapabilityRowsByName(rows)[capability]
		return row, ok
	}
	speech := func(mappingID string) (routing.CapabilityRow, bool) {
		return capabilityRow(mappingID, routing.CapabilitySpeech)
	}
	transcription := func(mappingID string) (routing.CapabilityRow, bool) {
		return capabilityRow(mappingID, routing.CapabilityTranscription)
	}
	if row, ok := speech("m1"); !ok || row.Verdict != routing.CapabilityYes || row.Source != routing.CapabilitySourceAudioCppModels || !row.CheckedAt.Equal(now) {
		t.Fatalf("m1 speech row = %+v (present=%v), want yes/audiocpp_models at %v", row, ok, now)
	}
	if row, ok := speech("m2"); !ok || row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceAudioCppModels {
		t.Fatalf("m2 speech row = %+v (present=%v), want no/audiocpp_models", row, ok)
	}
	if row, ok := speech("m3"); ok {
		t.Fatalf("m3 speech row = %+v, want none -- a model absent from the listing is unknown, never a no", row)
	}
	if row, ok := transcription("m1"); !ok || row.Verdict != routing.CapabilityNo || row.Source != routing.CapabilitySourceAudioCppModels {
		t.Fatalf("m1 transcription row = %+v (present=%v), want no/audiocpp_models", row, ok)
	}
	if row, ok := transcription("m2"); !ok || row.Verdict != routing.CapabilityYes || row.Source != routing.CapabilitySourceAudioCppModels || !row.CheckedAt.Equal(now) {
		t.Fatalf("m2 transcription row = %+v (present=%v), want yes/audiocpp_models at %v", row, ok, now)
	}
	if row, ok := transcription("m3"); ok {
		t.Fatalf("m3 transcription row = %+v, want none -- a model absent from the listing is unknown, never a no", row)
	}
	// The voices of the one loaded tts model are cached on ITS mapping,
	// normalized (sorted, deduplicated); no other mapping has a row.
	voicesPayload := func(mappingID string) (string, bool) {
		rows, err := store.ModelMappingCache(ctx, mappingID, routing.ModelMappingCacheVoices)
		if err != nil {
			t.Fatalf("model mapping cache %s: %v", mappingID, err)
		}
		if len(rows) == 0 {
			return "", false
		}
		if len(rows) != 1 || rows[0].ScopeKey != "" || !rows[0].UpdatedAt.Equal(now) {
			t.Fatalf("%s voices rows = %+v, want exactly one with an empty scope key stamped %v", mappingID, rows, now)
		}
		return rows[0].Payload, true
	}
	if payload, ok := voicesPayload("m1"); !ok || payload != `["demo_1_man","demo_2_man"]` {
		t.Fatalf("m1 voices payload = %q (present=%v), want [\"demo_1_man\",\"demo_2_man\"]", payload, ok)
	}
	for _, id := range []string{"m2", "m3", "m4"} {
		if payload, ok := voicesPayload(id); ok {
			t.Fatalf("%s has a voices row %q, want none -- only the loaded tts model's mapping is cached", id, payload)
		}
	}
	// The speech language fact rides the same listing read (#282): the loaded tts
	// model's mapping carries what its params declare, and no other mapping has a row --
	// the unloaded tts entry declares the same param and is still left out.
	languageRows := func(mappingID string) []routing.ModelMappingCacheRow {
		rows, err := store.ModelMappingCache(ctx, mappingID, routing.ModelMappingCacheSpeechLanguage)
		if err != nil {
			t.Fatalf("model mapping cache %s: %v", mappingID, err)
		}
		return rows
	}
	wantLanguage := routing.ModelMappingCacheRow{
		MappingID: "m1", CacheType: routing.ModelMappingCacheSpeechLanguage, ScopeKey: "",
		Payload: `{"accepts":true,"default":"auto"}`, UpdatedAt: now,
	}
	if rows := languageRows("m1"); len(rows) != 1 || rows[0] != wantLanguage {
		t.Fatalf("m1 speech_language rows = %+v, want exactly [%+v]", rows, wantLanguage)
	}
	for _, id := range []string{"m2", "m3", "m4"} {
		if rows := languageRows(id); len(rows) != 0 {
			t.Fatalf("%s speech_language rows = %+v, want none -- only the loaded tts model's mapping is cached", id, rows)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	voiceRequests := 0
	for _, p := range paths {
		switch p {
		case "/v1/models":
		case "/v1/audio/voices":
			voiceRequests++
		default:
			t.Fatalf("upstream paths = %v, want only /v1/models and /v1/audio/voices", paths)
		}
	}
	if len(paths) == 0 {
		t.Fatal("the upstream was never asked for /v1/models")
	}
	if voiceRequests != 1 {
		t.Fatalf("the upstream was asked for /v1/audio/voices %d times (paths %v), want 1: only the loaded tts model's", voiceRequests, paths)
	}
}

// agentVoicesRunner builds a runner over ONE server_agent application ("a1" on
// http://s1.local:8001) whose agent declares `features`, with the given
// mappings, each of them a fully ELIGIBLE agent-launched audio.cpp child for the
// agent-child voices pass (#251): a runtime spec of kind audio_cpp, the agent's
// own plain-tts verdicts (speech yes, speech_clone no) and its model loaded.
// The app has an app-level credential, so a pass that fell back to it instead of
// the spec's is visible. A test breaks exactly one gate on the returned parts and
// runs a cycle: that is what keeps each case's reason for not fetching
// unambiguous.
func agentVoicesRunner(t *testing.T, features []string, mappings ...routing.ModelMapping) (*appHealthRunner, *fakeHealthStore, *audioCppFakeProber, *gateway.LoadedModelRegistry) {
	t.Helper()
	shrinkRetryGap(t)
	app := serverAgentApp("a1", "s1", 8001)
	app.APIToken = "plain:app-tok"
	app.APITokenHeader = "X-App-Key"
	st := newHealthTestStore(app)
	st.mappings = map[string][]routing.ModelMapping{"a1": mappings}
	loaded := gateway.NewLoadedModelRegistry()
	models := make([]string, 0, len(mappings))
	for _, mp := range mappings {
		st.runtimeSpecs = append(st.runtimeSpecs, routing.RuntimeSpec{MappingID: mp.ID, Enabled: true, Type: string(routing.RuntimeSpecTypeAudioCpp)})
		st.seedCapability(mp.ID, routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
		st.seedCapability(mp.ID, routing.CapabilitySpeechClone, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
		models = append(models, mp.AppModelName)
	}
	loaded.SetGatewayProbe("a1", models)
	prober := newAudioCppFakeProber()
	runner := &appHealthRunner{
		store: st, prober: prober, registry: gateway.NewAppHealthRegistry(nil), loaded: loaded,
		agents:   fakeAgentBundle{features: map[string][]string{"s1": features}},
		settings: st, probeTimeout: time.Second, now: time.Now,
	}
	return runner, st, prober, loaded
}

// agentVoicesFlag is the agent's declared feature set that makes the voices
// route reachable: the one flag the pass is gated on.
var agentVoicesFlag = []string{gateway.RuntimeVoicesFeature}

// TestRunAppHealthOnceVoicesAgentFetchesThroughTheRouterAndWrites is the pass's
// whole job (#251): a RUNNING agent-launched audio.cpp child that is a plain tts
// model, on a server whose agent declared runtime_voices, has its voices fetched
// through the agent router's GET /upstream/<model>/v1/audio/voices and written,
// normalized, to the mapping's cache row.
//
// The lister is aimed at <router base>/upstream/<model> -- the target's Endpoint,
// to which the lister appends /v1/audio/voices -- with the application's
// server_agent provider, the probe timeout, and the mapping's upstream model
// name. The router is the only way to reach the child (it listens on a loopback
// port the gateway cannot dial), and the route is Status-only, so a cold child
// answers 404 instead of being started.
func TestRunAppHealthOnceVoicesAgentFetchesThroughTheRouterAndWrites(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
	prober.acVoices["pocket_tts"] = voiceList("demo_2_man", "demo_1_man", "demo_2_man")
	at := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return at }

	runCapabilityCycle(runner)

	if got, want := prober.voicesAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("voices listing asked about %v, want %v", got, want)
	}
	target := prober.acVoicesTargets[0]
	if target.Provider != routing.ProviderServerAgent || target.Endpoint != "http://s1.local:8001/upstream/pocket_tts" || target.Timeout != time.Second {
		t.Fatalf("voices target = %+v, want {Provider:server_agent Endpoint:http://s1.local:8001/upstream/pocket_tts Timeout:1s}", target)
	}
	row, ok := st.voicesRowOf("m1")
	if !ok {
		t.Fatal("m1 has no voices row, want one")
	}
	want := routing.ModelMappingCacheRow{
		MappingID: "m1", CacheType: "voices", ScopeKey: "",
		Payload: `["demo_1_man","demo_2_man"]`, UpdatedAt: at,
	}
	if row != want {
		t.Fatalf("voices row = %+v, want %+v", row, want)
	}
}

// TestRunAppHealthOnceVoicesAgentEscapesTheModelInTheRouterPath: the model name
// is a PATH segment of the router URL, so it is escaped segment by segment (a
// slash-bearing name stays a multi-segment path), exactly as the context probe
// expands {model}.
func TestRunAppHealthOnceVoicesAgentEscapesTheModelInTheRouterPath(t *testing.T) {
	runner, _, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "org/my model"))
	prober.acVoices["org/my model"] = voiceList("a")

	runCapabilityCycle(runner)

	if len(prober.acVoicesTargets) != 1 {
		t.Fatalf("voices listing called %d times, want 1", len(prober.acVoicesTargets))
	}
	if got, want := prober.acVoicesTargets[0].Endpoint, "http://s1.local:8001/upstream/org/my%20model"; got != want {
		t.Fatalf("voices endpoint = %q, want %q", got, want)
	}
	if got, want := prober.voicesAsked(), []string{"org/my model"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("voices listing asked about %v, want %v (the raw upstream model name)", got, want)
	}
}

// TestRunAppHealthOnceVoicesAgentWithoutTheFlagNeverFetches is the fail-closed
// gate: an agent with no positive evidence of the voices route is NEVER asked,
// because an older router never serves the route and answers a 404
// (runtime.upstream_endpoint_not_allowed from 0.7.0 on, runtime.model_not_managed
// before 0.7.0), so asking could not succeed and would only add a silent no-op
// GET per child per tick. "No evidence"
// is an agent that declared other features only, one that declared none, a
// server with no agent registry entry at all, and no registry bundle. The
// control case, with the flag, proves the setup is otherwise fetch-capable.
func TestRunAppHealthOnceVoicesAgentWithoutTheFlagNeverFetches(t *testing.T) {
	for _, tc := range []struct {
		name     string
		agents   agentRegistryBundle
		wantAsks int
	}{
		{"control: the flag is declared", fakeAgentBundle{features: map[string][]string{"s1": agentVoicesFlag}}, 1},
		{"only another feature is declared", fakeAgentBundle{features: map[string][]string{"s1": {gateway.RuntimeUpstreamPropsFeature}}}, 0},
		{"no feature is declared", fakeAgentBundle{features: map[string][]string{"s1": {}}}, 0},
		{"the server has no agent entry", fakeAgentBundle{features: map[string][]string{}}, 0},
		{"the flag is declared by another server's agent", fakeAgentBundle{features: map[string][]string{"s2": agentVoicesFlag}}, 0},
		{"no registry bundle", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober, _ := agentVoicesRunner(t, nil, audioCppMapping("m1", "pocket_tts"))
			runner.agents = tc.agents
			prober.acVoices["pocket_tts"] = voiceList("a")

			runCapabilityCycle(runner)

			if got := len(prober.voicesAsked()); got != tc.wantAsks {
				t.Fatalf("voices listing asked %d times, want %d", got, tc.wantAsks)
			}
			if tc.wantAsks == 0 {
				if row, ok := st.voicesRowOf("m1"); ok {
					t.Fatalf("m1 has a voices row %+v, want none", row)
				}
				if n := st.cacheWriteCount(); n != 0 {
					t.Fatalf("cache written %d times, want 0", n)
				}
			} else {
				st.assertVoicesPayload(t, "m1", `["a"]`)
			}
		})
	}
}

// TestRunAppHealthOnceVoicesAgentGate pins WHICH mappings are asked, one gate
// broken per case against a fully eligible child (the control). The route is
// Status-only, so a request for a model that is not running would only 404 --
// and a gateway that fetched on a guess is how a cold child would get asked at
// all -- hence loaded is strict, and only a speech audio.cpp child has voices
// to offer: (speech, yes), from the verdicts the agent reports for the child,
// whatever its (speech_clone) verdict -- a clone child's configured voice_presets
// are listed too (#301), the same rule as the static server's pass.
func TestRunAppHealthOnceVoicesAgentGate(t *testing.T) {
	deleteCap := func(st *fakeHealthStore, capability string) { delete(st.caps["m1"], capability) }
	for _, tc := range []struct {
		name      string
		breakGate func(st *fakeHealthStore, loaded *gateway.LoadedModelRegistry)
		want      bool
	}{
		{"control: a running plain tts audio.cpp child", func(*fakeHealthStore, *gateway.LoadedModelRegistry) {}, true},
		{"the clone verdict is unknown", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			deleteCap(st, routing.CapabilitySpeechClone)
		}, true},
		{"the model is a clone model (its configured presets are listed too, #301)", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.seedCapability("m1", routing.CapabilitySpeechClone, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
		}, true},
		{"the spec names no type but an audio.cpp binary", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.runtimeSpecs[0].Type, st.runtimeSpecs[0].Binary = "", "/opt/audio.cpp/audiocpp_server"
		}, true},
		{"nothing is loaded", func(_ *fakeHealthStore, loaded *gateway.LoadedModelRegistry) { loaded.SetGatewayProbe("a1", nil) }, false},
		{"another model is loaded", func(_ *fakeHealthStore, loaded *gateway.LoadedModelRegistry) {
			loaded.SetGatewayProbe("a1", []string{"some_other_model"})
		}, false},
		{"the speech verdict is no", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.seedCapability("m1", routing.CapabilitySpeech, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
		}, false},
		{"the speech verdict is unknown", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) { deleteCap(st, routing.CapabilitySpeech) }, false},
		{"the spec is a llama.cpp one", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.runtimeSpecs[0].Type = string(routing.RuntimeSpecTypeLlamaCpp)
		}, false},
		{"the spec names no type and an unrelated binary", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.runtimeSpecs[0].Type, st.runtimeSpecs[0].Binary = "", "/usr/local/bin/my-server"
		}, false},
		{"the mapping has no runtime spec", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) { st.runtimeSpecs = nil }, false},
		{"the mapping is not active", func(st *fakeHealthStore, _ *gateway.LoadedModelRegistry) {
			st.mappings["a1"][0].Status = routing.ServerStatusDisabled
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober, loaded := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
			prober.acVoices["pocket_tts"] = voiceList("a")
			tc.breakGate(st, loaded)

			runCapabilityCycle(runner)

			asked := prober.voicesAsked()
			if tc.want {
				if !reflect.DeepEqual(asked, []string{"pocket_tts"}) {
					t.Fatalf("voices listing asked about %v, want [pocket_tts]", asked)
				}
				st.assertVoicesPayload(t, "m1", `["a"]`)
				return
			}
			if len(asked) != 0 {
				t.Fatalf("voices listing asked about %v, want no request", asked)
			}
			if row, ok := st.voicesRowOf("m1"); ok {
				t.Fatalf("m1 has a voices row %+v, want none", row)
			}
		})
	}
}

// TestAgentVoiceProbeSpec pins the per-mapping eligibility decision of the agent voices
// pass on its own: a running, active audio.cpp child whose stored verdicts say (speech,
// yes) is asked, a clone child included (#301: the listing is task-independent, so its
// configured voice_presets are worth caching), and the stored rows come back with the spec
// for the speech rate step, which still excludes a clone child itself. Every other shape
// is refused and has its back-offs cleared when it left the eligible set.
func TestAgentVoiceProbeSpec(t *testing.T) {
	yes, no := routing.CapabilityYes, routing.CapabilityNo
	for _, tc := range []struct {
		name          string
		speech, clone string // "" = no row
		want          bool
	}{
		{"a plain tts child", yes, no, true},
		{"a clone child", yes, yes, true},
		{"a speech child whose clone verdict is unknown", yes, "", true},
		{"a child that is not a speech model", no, no, false},
		{"a child whose speech verdict is unknown", "", yes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mp := audioCppMapping("m1", "pocket_tts")
			runner, st, _, _ := agentVoicesRunner(t, agentVoicesFlag, mp)
			for capability, verdict := range map[string]string{routing.CapabilitySpeech: tc.speech, routing.CapabilitySpeechClone: tc.clone} {
				if verdict == "" {
					delete(st.caps["m1"], capability)
					continue
				}
				st.seedCapability("m1", capability, verdict, routing.CapabilitySourceAudioCppModels)
			}
			specByMapping := map[string]routing.RuntimeSpec{"m1": st.runtimeSpecs[0]}
			loadedSet := map[string]struct{}{"pocket_tts": {}}
			runner.voicesBackoff.start("m1", time.Now(), time.Now().Add(time.Hour))

			spec, rows, ask := runner.agentVoiceProbeSpec(context.Background(), mp, specByMapping, loadedSet)

			if ask != tc.want {
				t.Fatalf("agentVoiceProbeSpec ask = %v, want %v", ask, tc.want)
			}
			if tc.want {
				if spec.MappingID != "m1" {
					t.Fatalf("spec = %+v, want m1's runtime spec", spec)
				}
				if got := routing.CapabilityRowsByName(rows)[routing.CapabilitySpeechClone].Verdict; got != tc.clone {
					t.Fatalf("returned rows carry speech_clone %q, want %q (the rate step reads them)", got, tc.clone)
				}
				if !runner.voicesBackoff.active("m1", time.Now()) {
					t.Fatal("an eligible mapping had its voices back-off cleared, want it left alone")
				}
				return
			}
			if runner.voicesBackoff.active("m1", time.Now()) {
				t.Fatal("an ineligible mapping kept its voices back-off, want it cleared so a reload is asked at once")
			}
		})
	}
}

// TestRunAppHealthOnceVoicesAgentOnlyAsksTheLoadedMapping: two audio.cpp
// children of one application, one running and one cold. The cold one is never
// asked -- the router would not start it, but asking is still a wasted request
// that could one day be a start -- and gets no row; the running one is.
func TestRunAppHealthOnceVoicesAgentOnlyAsksTheLoadedMapping(t *testing.T) {
	runner, st, prober, loaded := agentVoicesRunner(t, agentVoicesFlag,
		audioCppMapping("m1", "tts_running"), audioCppMapping("m2", "tts_cold"))
	loaded.SetGatewayProbe("a1", []string{"tts_running"})
	prober.acVoices["tts_running"], prober.acVoices["tts_cold"] = voiceList("r"), voiceList("c")

	runCapabilityCycle(runner)

	if got, want := prober.voicesAsked(), []string{"tts_running"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("voices listing asked about %v, want %v", got, want)
	}
	st.assertVoicesPayload(t, "m1", `["r"]`)
	if row, ok := st.voicesRowOf("m2"); ok {
		t.Fatalf("the cold child's mapping has a voices row %+v, want none", row)
	}
}

// TestRunAppHealthOnceVoicesAgentCredentialsComeFromTheSpec: each mapping's
// fetch carries the credential SpecUpstreamAuth resolves for ITS spec (issue
// #58), opened from its sealed form, because the router forwards whatever the
// request carries to the child verbatim and a child protected by its own api key
// would otherwise answer 401. The app's own token is the fallback only for a
// spec in "app" mode (and the zero value); "off" sends nothing at all.
func TestRunAppHealthOnceVoicesAgentCredentialsComeFromTheSpec(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag,
		audioCppMapping("m-set", "tts_set"), audioCppMapping("m-custom", "tts_custom"),
		audioCppMapping("m-off", "tts_off"), audioCppMapping("m-app", "tts_app"))
	for i := range st.runtimeSpecs {
		switch st.runtimeSpecs[i].MappingID {
		case "m-set":
			st.runtimeSpecs[i].APITokenMode, st.runtimeSpecs[i].APIToken = "set", "plain:set-tok"
		case "m-custom":
			st.runtimeSpecs[i].APITokenMode, st.runtimeSpecs[i].APIToken = "set", "plain:custom-tok"
			st.runtimeSpecs[i].APITokenHeaderSource, st.runtimeSpecs[i].APITokenHeader = "custom", "X-Spec-Key"
		case "m-off":
			st.runtimeSpecs[i].APITokenMode = "off"
		}
	}
	for _, model := range []string{"tts_set", "tts_custom", "tts_off", "tts_app"} {
		prober.acVoices[model] = voiceList("v")
	}

	runCapabilityCycle(runner)

	prober.acMu.Lock()
	got := map[string]provider.UpstreamAuth{}
	for i, model := range prober.acVoicesModels {
		got[model] = prober.acVoicesAuth[i]
	}
	prober.acMu.Unlock()
	want := map[string]provider.UpstreamAuth{
		"tts_set":    {Header: "X-App-Key", Token: "set-tok"},
		"tts_custom": {Header: "X-Spec-Key", Token: "custom-tok"},
		"tts_off":    {},
		"tts_app":    {Header: "X-App-Key", Token: "app-tok"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("voices credentials per model = %+v, want %+v", got, want)
	}
}

// TestRunAppHealthOnceVoicesAgentColdChildKeepsTheRowAndBacksOff: the route
// answers 404 runtime.model_not_running for a child that stopped between the
// agent's loaded report and this GET (an idle unload, a crash). That says nothing
// about the model's voices, so it is a FAILED listing: the row on file -- the last
// known good, which the picker offers while the model is unloaded -- survives,
// and a back-off opens so the pass does not ask again every tick. Once the window
// has elapsed it asks again; a success is written and clears it. The fake hands
// voices back beside the error, so only the error guard stands between them and
// the row.
func TestRunAppHealthOnceVoicesAgentColdChildKeepsTheRowAndBacksOff(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
	current := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return current }
	prober.acVoices["pocket_tts"] = voiceList("would_overwrite")
	prober.acVoicesErr["pocket_tts"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)
	st.seedVoicesCache("m1", `["old_a","old_b"]`)
	asked := func() int { return len(prober.voicesAsked()) }

	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times after the first tick, want 1", n)
	}
	if !runner.voicesBackoff.active("m1", current) {
		t.Fatal("no back-off is open after a failed listing")
	}
	st.assertVoicesPayload(t, "m1", `["old_a","old_b"]`)

	current = current.Add(audioCppVoicesBackoff / 2)
	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times inside the back-off, want still 1", n)
	}

	current = current.Add(audioCppVoicesBackoff)
	runCapabilityCycle(runner)
	if n := asked(); n != 2 {
		t.Fatalf("asked %d times after the back-off elapsed, want 2", n)
	}
	if n := st.cacheWriteCount(); n != 0 {
		t.Fatalf("cache written %d times while every listing failed, want 0", n)
	}
	st.assertVoicesPayload(t, "m1", `["old_a","old_b"]`)

	// The child answers again: the success is written over the old row and
	// clears the back-off, so the next tick asks without waiting.
	delete(prober.acVoicesErr, "pocket_tts")
	current = current.Add(audioCppVoicesBackoff + time.Second)
	runCapabilityCycle(runner)
	runCapabilityCycle(runner)
	if n := asked(); n != 4 {
		t.Fatalf("asked %d times after the child recovered, want 4 (a success clears the back-off)", n)
	}
	st.assertVoicesPayload(t, "m1", `["would_overwrite"]`)
}

// TestRunAppHealthOnceVoicesAgentUnloadClearsTheBackoff: a child that leaves the
// loaded set while a back-off is open starts clean on its next load, so a reload
// (a new child, possibly a newer audio.cpp that does serve the listing) is asked
// at once instead of waiting out the window of the child before it.
func TestRunAppHealthOnceVoicesAgentUnloadClearsTheBackoff(t *testing.T) {
	runner, _, prober, loaded := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
	prober.acVoicesErr["pocket_tts"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)

	runCapabilityCycle(runner) // fails, opens a back-off
	runCapabilityCycle(runner) // inside it: not asked
	if n := len(prober.voicesAsked()); n != 1 {
		t.Fatalf("asked %d times, want 1", n)
	}

	loaded.SetGatewayProbe("a1", nil)
	runCapabilityCycle(runner) // unloaded: never asked, back-off cleared
	loaded.SetGatewayProbe("a1", []string{"pocket_tts"})
	runCapabilityCycle(runner)
	if n := len(prober.voicesAsked()); n != 2 {
		t.Fatalf("asked %d times after an unload and reload, want 2 (the reload retries inside the old back-off window)", n)
	}
}

// TestRunAppHealthOnceVoicesAgentWritePolicy: the same write policy as the
// static server's pass -- a SUCCESSFUL listing is written, a genuine empty one
// included ("the model has no voices" is a fact the picker should know), once:
// a steady child costs a read per tick and no write.
func TestRunAppHealthOnceVoicesAgentWritePolicy(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
	prober.acVoices["pocket_tts"] = []provider.Voice{} // a genuine empty, nil error
	st.seedVoicesCache("m1", `["old"]`)

	runCapabilityCycle(runner)
	runCapabilityCycle(runner)

	st.assertVoicesPayload(t, "m1", `[]`)
	if n := st.cacheWriteCount(); n != 1 {
		t.Fatalf("cache written %d times over two empty listings, want 1", n)
	}
	if n := len(prober.voicesAsked()); n != 2 {
		t.Fatalf("voices listing asked %d times over two ticks, want 2 (asked every tick, only the write is skipped)", n)
	}

	prober.acVoices["pocket_tts"] = voiceList("b", "a", "b")
	runCapabilityCycle(runner)
	st.assertVoicesPayload(t, "m1", `["a","b"]`)
}

// TestRunAppHealthOnceVoicesAgentFailsClosedOnStoreReadErrors: the pass cannot
// tell what kind of child a mapping is without its runtime spec, nor whether it
// is a plain tts one without its capability rows, so a failed read of either is
// "not known to be eligible": nothing is asked.
func TestRunAppHealthOnceVoicesAgentFailsClosedOnStoreReadErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		setup   func(*fakeHealthStore)
		wantLog string // "" = the failure is only debug-logged
	}{
		{"the runtime specs cannot be read", func(st *fakeHealthStore) { st.specsErr = true }, "runtime specs for app a1 failed"},
		{"the capability rows cannot be read", func(st *fakeHealthStore) { st.capsReadErr = true }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
			prober.acVoices["pocket_tts"] = voiceList("a")
			tc.setup(st)
			var buf bytes.Buffer
			log.SetOutput(&buf)
			t.Cleanup(func() { log.SetOutput(os.Stderr) })

			runCapabilityCycle(runner)

			if asked := prober.voicesAsked(); len(asked) != 0 {
				t.Fatalf("voices listing asked about %v, want no request", asked)
			}
			if n := st.cacheWriteCount(); n != 0 {
				t.Fatalf("cache written %d times, want 0", n)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log output = %q, want it to contain %q", buf.String(), tc.wantLog)
			}
		})
	}
}

// TestRunAppHealthOnceVoicesAgentStoreWriteFailureIsBestEffort: a cache write
// that fails is logged and the pass carries on to the next mapping.
func TestRunAppHealthOnceVoicesAgentStoreWriteFailureIsBestEffort(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag,
		audioCppMapping("m1", "tts_a"), audioCppMapping("m2", "tts_b"))
	prober.acVoices["tts_a"], prober.acVoices["tts_b"] = voiceList("a"), voiceList("b")
	st.cacheWriteErr = true
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	runCapabilityCycle(runner)

	if n := len(prober.voicesAsked()); n != 2 {
		t.Fatalf("voices listing asked %d times, want 2 (one failing write must not stop the next mapping)", n)
	}
	if !strings.Contains(buf.String(), "voices") {
		t.Fatalf("log output = %q, want the failure logged", buf.String())
	}
}

// TestRunAppHealthOnceVoicesAgentWithoutAListerIsSkipped: a prober that cannot
// list voices skips the pass silently.
func TestRunAppHealthOnceVoicesAgentWithoutAListerIsSkipped(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "pocket_tts"))
	runner.prober = &audioCppModelsOnlyProber{fakeProber: prober.fakeProber, inner: prober}

	runCapabilityCycle(runner)

	if n := st.cacheWriteCount(); n != 0 {
		t.Fatalf("cache written %d times, want 0", n)
	}
}

// TestRunAppHealthOnceVoicesAgentOnTheWire runs the pass through the REAL client
// chain -- the Multiplexer's server_agent client against an httptest server
// standing in for the agent router -- which the fake-lister tests above cannot
// see: the router URL the pass builds, the credential reaching the wire, and the
// route being a plain GET that is never anything else. Two children of one
// application: one answers its voices, one is cold and answers the router's
// 404, which keeps its stored row and opens its back-off. The router is
// asserted to have seen exactly the two voices GETs: no request that could start
// a child, no /props, no /running.
func TestRunAppHealthOnceVoicesAgentOnTheWire(t *testing.T) {
	shrinkRetryGap(t)
	type seenReq struct{ method, path, authorization, apiKey string }
	var mu sync.Mutex
	var seen []seenReq
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, seenReq{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/upstream/tts_warm/v1/audio/voices":
			_, _ = w.Write([]byte(`{"voices":["demo_2_man","demo_1_man"]}`))
		case "/upstream/tts_cold/v1/audio/voices":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"runtime.model_not_running","message":"model is not running"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer router.Close()
	u, err := url.Parse(router.URL)
	if err != nil {
		t.Fatalf("parse router url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse router port: %v", err)
	}

	app := serverAgentApp("a1", "s1", port)
	app.HealthCheckMode = routing.HealthCheckModeAlwaysReachable // the only requests the router sees are the voices ones
	app.APIToken = "plain:app-tok-must-not-be-used"
	st := &fakeHealthStore{
		servers: []routing.AIServer{{ID: "s1", Domain: u.Hostname(), Provider: routing.ProviderServerAgent, Status: routing.ServerStatusActive}},
		apps:    map[string][]routing.Application{"s1": {app}},
		mappings: map[string][]routing.ModelMapping{"a1": {
			audioCppMapping("m-warm", "tts_warm"), audioCppMapping("m-cold", "tts_cold"),
		}},
		runtimeSpecs: []routing.RuntimeSpec{
			{MappingID: "m-warm", Type: "audio_cpp", APITokenMode: "set", APIToken: "plain:tok-warm"},
			{MappingID: "m-cold", Type: "audio_cpp", APITokenMode: "set", APIToken: "plain:tok-cold", APITokenHeaderSource: "custom", APITokenHeader: "X-Api-Key"},
		},
	}
	for _, id := range []string{"m-warm", "m-cold"} {
		st.seedCapability(id, routing.CapabilitySpeech, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
		st.seedCapability(id, routing.CapabilitySpeechClone, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
	}
	st.seedVoicesCache("m-cold", `["last_known_good"]`)
	loaded := gateway.NewLoadedModelRegistry()
	loaded.SetGatewayProbe("a1", []string{"tts_warm", "tts_cold"})
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	runner := &appHealthRunner{
		store: st, prober: providerClients(0, false, nil), registry: gateway.NewAppHealthRegistry(nil), loaded: loaded,
		agents:   fakeAgentBundle{features: map[string][]string{"s1": agentVoicesFlag}},
		settings: st, probeTimeout: time.Second, now: func() time.Time { return now },
	}

	runCapabilityCycle(runner)

	mu.Lock()
	got := append([]seenReq(nil), seen...)
	mu.Unlock()
	sort.Slice(got, func(i, j int) bool { return got[i].path < got[j].path })
	want := []seenReq{
		{http.MethodGet, "/upstream/tts_cold/v1/audio/voices", "", "tok-cold"},
		{http.MethodGet, "/upstream/tts_warm/v1/audio/voices", "Bearer tok-warm", ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("router saw %+v, want exactly %+v", got, want)
	}
	st.assertVoicesPayload(t, "m-warm", `["demo_1_man","demo_2_man"]`)
	st.assertVoicesPayload(t, "m-cold", `["last_known_good"]`)
	if n := st.cacheWriteCount(); n != 1 {
		t.Fatalf("cache written %d times, want 1 (the warm child only)", n)
	}
	if !runner.voicesBackoff.active("m-cold", now) || runner.voicesBackoff.active("m-warm", now) {
		t.Fatalf("back-off cold=%v warm=%v, want cold=true warm=false", runner.voicesBackoff.active("m-cold", now), runner.voicesBackoff.active("m-warm", now))
	}
}

// ---- the agent-launched child's speech-language pull (#282) ----

// agentSpeechParamsFlag declares BOTH router flags the agent audio pass depends
// on: runtime_voices (the pass itself) and runtime_model_params (the params
// pull folded into it). The two arrive together in practice -- the feature
// registry is append-only, so runtime_model_params (0.13.0) implies
// runtime_voices (0.11.0) -- which is why the params pull rides the voices-gated
// pass under its OWN flag.
var agentSpeechParamsFlag = []string{gateway.RuntimeVoicesFeature, gateway.RuntimeModelParamsFeature}

// TestRunAppHealthOnceSpeechParamsAgentPullsThroughTheRouterAndWrites is the
// params pull's whole job (#282): a RUNNING agent-launched audio.cpp child that
// is a plain tts model, on a server whose agent declared runtime_model_params,
// has its /v1/models listing pulled through the agent router's Status-only
// GET /upstream/<model>/v1/models and the `language` fact it carries
// (AcceptsLanguage, LanguageDefault) written to the mapping's speech_language
// cache row. It is the twin of the static server's speech-params step, through
// the route the gateway reaches an agent child by -- the same target the voices
// pass builds, with the per-mapping spec credential attached.
func TestRunAppHealthOnceSpeechParamsAgentPullsThroughTheRouterAndWrites(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentSpeechParamsFlag, audioCppMapping("m1", "pocket_tts"))
	prober.acVerdicts["pocket_tts"] = provider.AudioCppVerdicts{AcceptsLanguage: true, LanguageDefault: "zh"}
	at := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return at }

	runCapabilityCycle(runner)

	if got, want := prober.audioCppAsked(), []string{"pocket_tts"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("models probe asked about %v, want %v", got, want)
	}
	target := prober.acTargets[0]
	if target.Provider != routing.ProviderServerAgent || target.Endpoint != "http://s1.local:8001/upstream/pocket_tts" || target.Timeout != time.Second {
		t.Fatalf("models probe target = %+v, want {Provider:server_agent Endpoint:http://s1.local:8001/upstream/pocket_tts Timeout:1s}", target)
	}
	if got, want := prober.acAuth[0], (provider.UpstreamAuth{Header: "X-App-Key", Token: "app-tok"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("models probe credential = %+v, want the spec's %+v", got, want)
	}
	row, ok := st.speechLanguageRowOf("m1")
	if !ok {
		t.Fatal("m1 has no speech_language row, want one")
	}
	want := routing.ModelMappingCacheRow{
		MappingID: "m1", CacheType: "speech_language", ScopeKey: "",
		Payload: `{"accepts":true,"default":"zh"}`, UpdatedAt: at,
	}
	if row != want {
		t.Fatalf("speech_language row = %+v, want %+v", row, want)
	}
}

// TestRunAppHealthOnceAgentCloneChildCachesVoicesAndLanguageButIsNeverBenchmarked (#301):
// a RUNNING agent-launched clone child rides the same pass as a plain tts one -- its
// configured voice_presets are cached and its language fact pulled -- but the speech rate
// step still refuses it, even when it is stored as streaming, because streaming clone
// models are out of scope.
func TestRunAppHealthOnceAgentCloneChildCachesVoicesAndLanguageButIsNeverBenchmarked(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentSpeechParamsFlag, audioCppMapping("m1", "clone_tts"))
	st.seedCapability("m1", routing.CapabilitySpeechClone, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	st.seedCapability("m1", routing.CapabilitySpeechStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	prober.acVoices["clone_tts"] = voiceList("preset_b", "preset_a")
	prober.acVerdicts["clone_tts"] = provider.AudioCppVerdicts{AcceptsLanguage: true, LanguageDefault: "de"}
	rec := &speechRateRecorder{}
	runner.speechRate = rec.trigger

	runCapabilityCycle(runner)

	st.assertVoicesPayload(t, "m1", `["preset_a","preset_b"]`)
	if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":true,"default":"de"}` {
		t.Fatalf("speech_language payload = %s, want the clone child's fact {accepts:true,default:de}", got)
	}
	if calls := rec.started(); len(calls) != 0 {
		t.Fatalf("the speech rate benchmark was triggered %d times (%+v) for a clone child, want 0", len(calls), calls)
	}
}

// TestRunAppHealthOnceSpeechParamsAgentWithoutTheFlagNeverPulls is the
// fail-closed gate on the params route, separate from the voices gate: an agent
// that declared runtime_voices but NOT runtime_model_params runs the voices pass
// yet is NEVER asked for /v1/models, because an older router answers a 404
// (runtime.upstream_endpoint_not_allowed) for the route, so the pull could not
// succeed and would only add silent no-op traffic each tick. No request is made
// and no speech_language row is written. The control case, with the params flag,
// proves the setup is otherwise pull-capable.
func TestRunAppHealthOnceSpeechParamsAgentWithoutTheFlagNeverPulls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		features  []string
		wantPulls int
	}{
		{"control: the params flag is declared", agentSpeechParamsFlag, 1},
		{"only the voices flag is declared", agentVoicesFlag, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, st, prober, _ := agentVoicesRunner(t, tc.features, audioCppMapping("m1", "pocket_tts"))
			prober.acVerdicts["pocket_tts"] = provider.AudioCppVerdicts{AcceptsLanguage: true, LanguageDefault: "zh"}

			runCapabilityCycle(runner)

			if got := len(prober.audioCppAsked()); got != tc.wantPulls {
				t.Fatalf("models probe asked %d times, want %d", got, tc.wantPulls)
			}
			if tc.wantPulls == 0 {
				if row, ok := st.speechLanguageRowOf("m1"); ok {
					t.Fatalf("m1 has a speech_language row %+v, want none (the params route was not pulled)", row)
				}
			} else if _, ok := st.speechLanguageRowOf("m1"); !ok {
				t.Fatal("m1 has no speech_language row, want one")
			}
		})
	}
}

// TestRunAppHealthOnceSpeechParamsAgentFailedReadLeavesLastGoodAndBacksOff: the
// route answers 404 for a child that stopped between the agent's loaded report
// and this GET, which says nothing about the model's params, so it is a FAILED
// read -- the speech_language row on file (the last fact learned while the model
// was loaded) survives, and a back-off opens so the pull does not retry every
// tick. Once the window elapses it asks again; a success is written over the row
// and clears the back-off. The fake hands a verdict back beside the error, so
// only the error guard stands between it and the row.
func TestRunAppHealthOnceSpeechParamsAgentFailedReadLeavesLastGoodAndBacksOff(t *testing.T) {
	runner, st, prober, _ := agentVoicesRunner(t, agentSpeechParamsFlag, audioCppMapping("m1", "pocket_tts"))
	current := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	runner.now = func() time.Time { return current }
	prober.acVerdicts["pocket_tts"] = provider.AudioCppVerdicts{AcceptsLanguage: true, LanguageDefault: "would_overwrite"}
	prober.acErr["pocket_tts"] = fmt.Errorf("%w: status 404", provider.ErrUnavailable)
	seeded := routing.SpeechLanguagePayload(false, "")
	st.seedCacheRow("m1", routing.ModelMappingCacheSpeechLanguage, seeded)
	asked := func() int { return len(prober.audioCppAsked()) }

	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times after the first tick, want 1", n)
	}
	if !runner.speechParamsBackoff.active("m1", current) {
		t.Fatal("no back-off is open after a failed params read")
	}
	if got, ok := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); !ok || got != seeded {
		t.Fatalf("speech_language payload = %q (present=%v), want the seeded %s left untouched", got, ok, seeded)
	}

	current = current.Add(audioCppVoicesBackoff / 2)
	runCapabilityCycle(runner)
	if n := asked(); n != 1 {
		t.Fatalf("asked %d times inside the back-off, want still 1", n)
	}

	// The child answers again past the window: the success is written over the
	// seeded row and clears the back-off, so the next tick asks without waiting.
	delete(prober.acErr, "pocket_tts")
	prober.acVerdicts["pocket_tts"] = provider.AudioCppVerdicts{AcceptsLanguage: true, LanguageDefault: "auto"}
	current = current.Add(audioCppVoicesBackoff + time.Second)
	runCapabilityCycle(runner)
	if n := asked(); n != 2 {
		t.Fatalf("asked %d times after the back-off elapsed, want 2", n)
	}
	if runner.speechParamsBackoff.active("m1", current) {
		t.Fatal("back-off still open after a successful read, want it cleared")
	}
	if got, _ := st.cachePayloadOf("m1", routing.ModelMappingCacheSpeechLanguage); got != `{"accepts":true,"default":"auto"}` {
		t.Fatalf("speech_language payload = %s, want the recovered {accepts:true,default:auto}", got)
	}
}

// ---- the speech rate trigger (#263) ----
//
// The health loop's part in streamed speech billing: a streaming speech mapping needs
// its output byte rate cached before a client streams, so a tick that finds one without
// -- or whose loaded model is no longer the one the rate was measured under -- asks the
// gateway to run the speech rate benchmark. The loop only ASKS (it never synthesizes);
// the recorder below stands in for gateway.Server.TriggerSpeechRateBenchmark, whose own
// behavior (the registry's one-run slot, the idle gate, what is written) is pinned in
// the gateway package.

// speechRateCall is one start of the speech rate benchmark the health loop asked for.
type speechRateCall struct{ serverID, appID, mappingID, identity string }

// speechRateRecorder records every trigger and answers started (true) unless told it
// did not start one (a busy server, the idle gate).
type speechRateRecorder struct {
	mu         sync.Mutex
	calls      []speechRateCall
	notStarted bool
}

func (r *speechRateRecorder) trigger(_ context.Context, server routing.AIServer, app routing.Application, mp routing.ModelMapping, identity string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, speechRateCall{server.ID, app.ID, mp.ID, identity})
	return !r.notStarted
}

func (r *speechRateRecorder) started() []speechRateCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]speechRateCall(nil), r.calls...)
}

func (r *speechRateRecorder) setNotStarted(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notStarted = v
}

// streamingTTSVerdicts is the listing of a LOADED plain tts model in streaming mode at
// path /m/a: the one shape that makes a mapping a target of the speech rate benchmark.
var streamingTTSVerdicts = provider.AudioCppVerdicts{
	Speech: routing.CapabilityYes, Transcription: routing.CapabilityNo, SpeechClone: routing.CapabilityNo,
	SpeechStreaming: routing.CapabilityYes, Loaded: true, Path: "/m/a",
}

// speechRateEnv is a static audio_cpp application ("a1" on server "s1") with one
// mapping "m1" whose listing states streamingTTSVerdicts, a recording trigger and a
// clock the test moves. Every tick is one full health cycle.
type speechRateEnv struct {
	runner *appHealthRunner
	st     *fakeHealthStore
	prober *audioCppFakeProber
	rec    *speechRateRecorder
	clock  time.Time
}

func newSpeechRateEnv(t *testing.T) *speechRateEnv {
	t.Helper()
	runner, st, prober := audioCppProbeRunner(t,
		map[string]provider.AudioCppVerdicts{"tts_stream": streamingTTSVerdicts}, audioCppMapping("m1", "tts_stream"))
	env := &speechRateEnv{runner: runner, st: st, prober: prober, rec: &speechRateRecorder{}, clock: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	runner.speechRate = env.rec.trigger
	runner.now = func() time.Time { return env.clock }
	return env
}

func (e *speechRateEnv) tick() { runCapabilityCycle(e.runner) }

func (e *speechRateEnv) advance(d time.Duration) { e.clock = e.clock.Add(d) }

// setListing edits what the server's listing says about the mapping's model.
func (e *speechRateEnv) setListing(mutate func(*provider.AudioCppVerdicts)) {
	e.prober.acMu.Lock()
	defer e.prober.acMu.Unlock()
	v := e.prober.acVerdicts["tts_stream"]
	mutate(&v)
	e.prober.acVerdicts["tts_stream"] = v
}

// benchmarkCompletes stands in for a successful speech rate benchmark: the rate and
// the identity it was measured under, as gateway.runSpeechTarget writes them.
func (e *speechRateEnv) benchmarkCompletes(rate, identity string) {
	e.st.seedCacheRow("m1", routing.ModelMappingCacheOutputByteRate, rate)
	if identity != "" {
		e.st.seedCacheRow("m1", routing.ModelMappingCacheOutputByteRateModel, identity)
	}
}

func (e *speechRateEnv) wantCalls(t *testing.T, when string, want ...speechRateCall) {
	t.Helper()
	got := e.rec.started()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: speech rate benchmark started %+v, want %+v", when, got, want)
	}
}

func (e *speechRateEnv) ratePayload() (string, bool) {
	return e.st.cachePayloadOf("m1", routing.ModelMappingCacheOutputByteRate)
}

func (e *speechRateEnv) identityPayload() (string, bool) {
	return e.st.cachePayloadOf("m1", routing.ModelMappingCacheOutputByteRateModel)
}

var speechRateCallA = speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/a"}

// TestHealthTriggersSpeechRateBenchmarkOnce is the trigger's whole job: a streaming
// speech mapping with no cached rate gets the benchmark started, ONCE. The loop only
// asks -- it writes neither the rate nor the identity -- and it does not ask again on
// the ticks while the benchmark is out (the back-off), nor once the rate is cached for
// the model that is loaded.
func TestHealthTriggersSpeechRateBenchmarkOnce(t *testing.T) {
	env := newSpeechRateEnv(t)

	env.tick()
	env.wantCalls(t, "first tick, no rate cached", speechRateCallA)
	if payload, ok := env.ratePayload(); ok {
		t.Fatalf("the health loop wrote a rate row %q itself, want none: it only asks the gateway to benchmark", payload)
	}
	if payload, ok := env.identityPayload(); ok {
		t.Fatalf("the health loop wrote an identity row %q itself, want none: the benchmark records it with the rate", payload)
	}

	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "second tick, the benchmark has not cached a rate yet", speechRateCallA)

	env.benchmarkCompletes("88200", "/m/a")
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "third tick, the rate is now cached for the loaded model", speechRateCallA)
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "fourth tick", speechRateCallA)
	if payload, _ := env.ratePayload(); payload != "88200" {
		t.Fatalf("rate payload = %q, want the benchmark's 88200 untouched", payload)
	}
}

// TestHealthReBenchmarksOnModelIdentityChange is Review Focus 5: the model loaded
// behind one mapping changes (the listing states another file for the same id), which
// no rename invalidation sees. The cached rate is then of a different model, so the
// loop (1) stops it billing -- the rate is invalidated at once, and a streamed call
// bills the honest audio_bytes -- and (2) starts the benchmark to derive the new one.
// While the identity is unchanged nothing happens, tick after tick.
func TestHealthReBenchmarksOnModelIdentityChange(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.benchmarkCompletes("88200", "/m/a")

	env.tick()
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "the rate is cached for the loaded model")

	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/b" })
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "the loaded model's file changed", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/b"})
	payload, ok := env.ratePayload()
	if !ok || payload != "" {
		t.Fatalf("rate row after the identity change = (%q, present %v), want the stale 88200 invalidated to an empty payload", payload, ok)
	}
	if _, usable := routing.ParseOutputByteRate(payload); usable {
		t.Fatal("the invalidated payload still parses as a rate: billing would keep using the stale one")
	}

	// The benchmark completes for the new model; nothing more is asked.
	env.benchmarkCompletes("48000", "/m/b")
	for i := 0; i < 2; i++ {
		env.advance(30 * time.Second)
		env.tick()
	}
	env.wantCalls(t, "after the benchmark re-derived the rate", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/b"})
}

// A cached rate whose identity differs triggers at once even inside the failure
// back-off an earlier trigger opened: the back-off is for retrying a benchmark that
// did not produce a rate, and a changed identity is a different model, not a retry.
func TestHealthSpeechRateIdentityChangeIgnoresAnOpenBackoff(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.tick() // no rate: triggered, back-off open
	env.benchmarkCompletes("88200", "/m/a")
	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/b" })
	env.advance(time.Second) // far inside the back-off window

	env.tick()
	env.wantCalls(t, "identity changed inside the back-off", speechRateCallA, speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/b"})
}

// The trigger's gate: a mapping is a target only while its model is observed LOADED,
// plain (not a clone) and streaming. Each case below breaks exactly one of those on an
// otherwise eligible mapping with no rate, and none of them may start the benchmark or
// touch the cache.
func TestHealthSpeechRateGateRefusesIneligibleMappings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		listed func(*provider.AudioCppVerdicts)
	}{
		{"the model is not loaded", func(v *provider.AudioCppVerdicts) { v.Loaded = false }},
		{"speech_streaming is no (an offline model)", func(v *provider.AudioCppVerdicts) { v.SpeechStreaming = routing.CapabilityNo }},
		{"speech_streaming is unknown", func(v *provider.AudioCppVerdicts) { v.SpeechStreaming = "" }},
		{"a clone model, which the benchmark cannot synthesize", func(v *provider.AudioCppVerdicts) { v.SpeechClone = routing.CapabilityYes }},
		{"a transcriber", func(v *provider.AudioCppVerdicts) {
			*v = provider.AudioCppVerdicts{Speech: routing.CapabilityNo, Transcription: routing.CapabilityYes, TranscriptionStreaming: routing.CapabilityYes, Loaded: true, Path: "/m/a"}
		}},
		{"a model that is no speech model", func(v *provider.AudioCppVerdicts) { v.Speech = routing.CapabilityNo }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newSpeechRateEnv(t)
			env.setListing(tc.listed)
			env.tick()
			env.wantCalls(t, "an ineligible mapping")
			if _, ok := env.ratePayload(); ok {
				t.Fatal("a rate row appeared for an ineligible mapping")
			}
			if _, ok := env.identityPayload(); ok {
				t.Fatal("an identity row appeared for an ineligible mapping")
			}
		})
	}
}

// A probe that fails, an inactive mapping and a mapping the listing does not name carry
// no verdict, so none of them is a target either.
func TestHealthSpeechRateGateNeedsAVerdictFromTheListing(t *testing.T) {
	t.Run("the listing probe failed", func(t *testing.T) {
		env := newSpeechRateEnv(t)
		env.prober.acErr["tts_stream"] = fmt.Errorf("%w: boom", provider.ErrUnavailable)
		env.tick()
		env.wantCalls(t, "a failed probe")
	})
	t.Run("the listing does not name the model", func(t *testing.T) {
		env := newSpeechRateEnv(t)
		env.setListing(func(v *provider.AudioCppVerdicts) { *v = provider.AudioCppVerdicts{} })
		env.tick()
		env.wantCalls(t, "an unnamed model")
	})
	t.Run("the mapping is inactive", func(t *testing.T) {
		env := newSpeechRateEnv(t)
		mp := audioCppMapping("m1", "tts_stream")
		mp.Status = routing.ServerStatusDisabled
		env.st.mappings = map[string][]routing.ModelMapping{"a1": {mp}}
		env.tick()
		env.wantCalls(t, "an inactive mapping")
	})
}

// A listing that does not state a model path gives the loop nothing to compare: it
// still starts the benchmark for a mapping with no rate (the identity is then simply
// empty), but a cached rate is never re-triggered on a guess -- an unstated identity is
// no statement that the model changed.
func TestHealthSpeechRateWithoutAStatedPath(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "" })

	env.tick()
	env.wantCalls(t, "no rate, no stated path", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: ""})

	env.benchmarkCompletes("88200", "")
	env.advance(time.Hour)
	env.tick()
	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/a" })
	env.advance(time.Hour)
	env.tick()
	env.wantCalls(t, "a cached rate with no recorded identity", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: ""})
	if payload, ok := env.ratePayload(); !ok || payload != "88200" {
		t.Fatalf("rate row = (%q, %v), want the cached 88200 untouched", payload, ok)
	}
}

// A rate with no recorded identity is one a MANUAL benchmark cached (it knows none).
// The loop adopts the identity it observes for it -- recording it, with no benchmark --
// so a LATER change of the loaded model is detected from then on.
func TestHealthSpeechRateAdoptsTheIdentityOfARateThatHasNone(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.benchmarkCompletes("88200", "")

	env.tick()
	env.wantCalls(t, "a manual rate is not re-benchmarked")
	if payload, ok := env.identityPayload(); !ok || payload != "/m/a" {
		t.Fatalf("identity row = (%q, %v), want the observed /m/a adopted", payload, ok)
	}
	if payload, _ := env.ratePayload(); payload != "88200" {
		t.Fatalf("rate payload = %q, want 88200 untouched by the adoption", payload)
	}

	n := env.st.cacheWriteCount()
	env.advance(30 * time.Second)
	env.tick()
	if got := env.st.cacheWriteCount(); got != n {
		t.Fatalf("cache written %d more times on a steady tick, want 0: adoption happens once", got-n)
	}

	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/b" })
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "the model changed after the adoption", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/b"})
}

// A benchmark that does not produce a rate (it failed, or the server was never idle) is
// retried, but not every tick: the back-off holds the trigger off for
// speechRateRetryBackoff, then asks again. A trigger that did NOT start anything (the
// gateway answered false: a busy server, an idle gate) opens no back-off, so it is asked
// again on the very next tick.
func TestHealthSpeechRateRetriesAFailedBenchmarkAfterTheBackoff(t *testing.T) {
	env := newSpeechRateEnv(t)

	env.tick()
	env.advance(speechRateRetryBackoff / 2)
	env.tick()
	env.wantCalls(t, "inside the back-off", speechRateCallA)

	env.advance(speechRateRetryBackoff)
	env.tick()
	env.wantCalls(t, "after the back-off", speechRateCallA, speechRateCallA)
}

func TestHealthSpeechRateNotStartedIsAskedAgainNextTick(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.rec.setNotStarted(true)

	env.tick()
	env.advance(time.Second)
	env.tick()
	env.wantCalls(t, "the gateway started nothing", speechRateCallA, speechRateCallA)
	if env.runner.speechRateBackoff.active("m1", env.clock) {
		t.Fatal("a trigger that started nothing opened a back-off")
	}

	env.rec.setNotStarted(false)
	env.advance(time.Second)
	env.tick()
	env.wantCalls(t, "the gateway started it at last", speechRateCallA, speechRateCallA, speechRateCallA)
	env.advance(time.Second)
	env.tick()
	env.wantCalls(t, "the started run is in flight", speechRateCallA, speechRateCallA, speechRateCallA)
}

// A mapping that leaves the eligible set (the model unloaded, say) forgets its
// back-off, so a reload is asked at once, like the voices back-off.
func TestHealthSpeechRateBackoffClearsWhenTheModelLeavesTheLoadedSet(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.tick()
	env.wantCalls(t, "first", speechRateCallA)

	env.setListing(func(v *provider.AudioCppVerdicts) { v.Loaded = false })
	env.advance(time.Second)
	env.tick()
	if env.runner.speechRateBackoff.active("m1", env.clock) {
		t.Fatal("the back-off survived the model leaving the loaded set")
	}

	env.setListing(func(v *provider.AudioCppVerdicts) { v.Loaded = true })
	env.advance(time.Second)
	env.tick()
	env.wantCalls(t, "after a reload", speechRateCallA, speechRateCallA)
}

// A rate payload billing cannot parse is no rate: the loop starts the benchmark, which
// overwrites it, exactly as for a missing row. (The invalidation writes such a payload.)
func TestHealthSpeechRateAnUnusableRateIsNoRate(t *testing.T) {
	for _, payload := range []string{"", "0", "fast", "-5"} {
		t.Run(fmt.Sprintf("payload %q", payload), func(t *testing.T) {
			env := newSpeechRateEnv(t)
			env.benchmarkCompletes(payload, "/m/a")
			env.tick()
			env.wantCalls(t, "an unusable cached rate", speechRateCallA)
		})
	}
}

// With no trigger wired (a runner built without the gateway Server, or before it is
// bound) the pass skips the whole step: no panic, no cache read or write beyond what the
// voices step does.
func TestHealthSpeechRateWithoutATriggerDoesNothing(t *testing.T) {
	env := newSpeechRateEnv(t)
	env.runner.speechRate = nil
	env.benchmarkCompletes("88200", "/m/a")
	env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/b" })

	env.tick()
	if payload, _ := env.ratePayload(); payload != "88200" {
		t.Fatalf("rate payload = %q, want 88200: without a trigger the loop must not invalidate a rate it cannot re-derive", payload)
	}
	if payload, _ := env.identityPayload(); payload != "/m/a" {
		t.Fatalf("identity payload = %q, want /m/a untouched", payload)
	}
}

// Store failures are best-effort: a cache the loop cannot read is neither a "no rate" to
// trigger on nor a license to invalidate.
func TestHealthSpeechRateStoreFailuresAreBestEffort(t *testing.T) {
	t.Run("a cache read failure triggers nothing", func(t *testing.T) {
		env := newSpeechRateEnv(t)
		env.st.cacheReadErr = true
		env.tick()
		env.wantCalls(t, "an unreadable cache")
	})
	t.Run("a failed invalidation still starts the benchmark", func(t *testing.T) {
		env := newSpeechRateEnv(t)
		env.benchmarkCompletes("88200", "/m/a")
		env.setListing(func(v *provider.AudioCppVerdicts) { v.Path = "/m/b" })
		env.st.cacheWriteErr = true
		env.tick()
		env.wantCalls(t, "the invalidation write failed", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: "/m/b"})
	})
}

// ---- the agent-launched child ----

// agentSpeechRateRunner is agentVoicesRunner for the speech rate trigger: ONE running
// agent-launched audio.cpp child "m1" with the agent's own plain-tts verdicts and a
// stored speech_streaming yes, a recording trigger and a movable clock.
func agentSpeechRateRunner(t *testing.T) *speechRateEnv {
	t.Helper()
	runner, st, prober, _ := agentVoicesRunner(t, agentVoicesFlag, audioCppMapping("m1", "tts_stream"))
	st.seedCapability("m1", routing.CapabilitySpeechStreaming, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
	env := &speechRateEnv{runner: runner, st: st, prober: prober, rec: &speechRateRecorder{}, clock: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	runner.speechRate = env.rec.trigger
	runner.now = func() time.Time { return env.clock }
	return env
}

func (e *speechRateEnv) setSpec(mutate func(*routing.RuntimeSpec)) {
	e.st.mu.Lock()
	defer e.st.mu.Unlock()
	mutate(&e.st.runtimeSpecs[0])
}

// An agent-launched child that is running, plain and streaming gets the same trigger as
// a static server's model. The gateway cannot read a child's own listing, so the identity
// it records is a fingerprint of what the gateway launches (agentSpecIdentity): a launch
// spec edited to another model is the hot swap of an agent child.
func TestHealthSpeechRateAgentChild(t *testing.T) {
	env := agentSpeechRateRunner(t)
	env.setSpec(func(sp *routing.RuntimeSpec) { sp.Binary, sp.Args = "audio-server", `["--model","/m/a.gguf"]` })
	identityA := agentSpecIdentity(routing.RuntimeSpec{Binary: "audio-server", Args: `["--model","/m/a.gguf"]`})

	env.tick()
	env.wantCalls(t, "a running streaming child with no rate", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: identityA})
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "inside the back-off", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: identityA})

	env.benchmarkCompletes("88200", identityA)
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "the rate is cached for the launched model", speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: identityA})

	env.setSpec(func(sp *routing.RuntimeSpec) { sp.Args = `["--model","/m/b.gguf"]` })
	identityB := agentSpecIdentity(routing.RuntimeSpec{Binary: "audio-server", Args: `["--model","/m/b.gguf"]`})
	env.advance(30 * time.Second)
	env.tick()
	env.wantCalls(t, "the launch spec now names another model",
		speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: identityA},
		speechRateCall{serverID: "s1", appID: "a1", mappingID: "m1", identity: identityB})
	if payload, ok := env.ratePayload(); !ok || payload != "" {
		t.Fatalf("rate row = (%q, %v), want the stale rate invalidated", payload, ok)
	}
}

func TestHealthSpeechRateAgentChildGate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		breakIt func(*speechRateEnv)
	}{
		{"speech_streaming is no", func(e *speechRateEnv) {
			e.st.seedCapability("m1", routing.CapabilitySpeechStreaming, routing.CapabilityNo, routing.CapabilitySourceAudioCppModels)
		}},
		{"speech_streaming is unknown", func(e *speechRateEnv) {
			e.st.mu.Lock()
			delete(e.st.caps["m1"], routing.CapabilitySpeechStreaming)
			e.st.mu.Unlock()
		}},
		{"a clone model", func(e *speechRateEnv) {
			e.st.seedCapability("m1", routing.CapabilitySpeechClone, routing.CapabilityYes, routing.CapabilitySourceAudioCppModels)
		}},
		{"the child is not running", func(e *speechRateEnv) {
			e.runner.loaded.SetGatewayProbe("a1", nil)
		}},
		{"the spec is not an audio.cpp one", func(e *speechRateEnv) {
			e.setSpec(func(sp *routing.RuntimeSpec) { sp.Type = "llama_cpp" })
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := agentSpeechRateRunner(t)
			tc.breakIt(env)
			env.tick()
			env.wantCalls(t, "an ineligible child")
		})
	}
}

// agentSpecIdentity fingerprints what the gateway launches: the launch spec's binary,
// arguments, environment, config files and working directory. A change to any of them
// can change which model the child serves; a change to anything else (ports, timeouts,
// pinning, admin state) cannot, and must not cost a benchmark.
func TestAgentSpecIdentity(t *testing.T) {
	base := routing.RuntimeSpec{
		ID: "rs1", MappingID: "m1", Binary: "audio-server", Args: `["--model","/m/a.gguf"]`, Env: `{"K":"${AGENT_ENV:K}"}`,
		ConfigFiles: `[{"name":"c.json","content":"{}"}]`, WorkDir: "/srv", Type: "audio_cpp",
	}
	id := agentSpecIdentity(base)
	if id == "" {
		t.Fatal("agentSpecIdentity is empty for a spec that launches a model")
	}
	if again := agentSpecIdentity(base); again != id {
		t.Fatalf("agentSpecIdentity is not stable: %q then %q", id, again)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*routing.RuntimeSpec)
	}{
		{"binary", func(s *routing.RuntimeSpec) { s.Binary = "other-server" }},
		{"args", func(s *routing.RuntimeSpec) { s.Args = `["--model","/m/b.gguf"]` }},
		{"env", func(s *routing.RuntimeSpec) { s.Env = `{"K":"${AGENT_ENV:OTHER}"}` }},
		{"config files", func(s *routing.RuntimeSpec) { s.ConfigFiles = `[]` }},
		{"work dir", func(s *routing.RuntimeSpec) { s.WorkDir = "/other" }},
	} {
		t.Run("a different "+tc.name+" is a different identity", func(t *testing.T) {
			changed := base
			tc.mutate(&changed)
			if got := agentSpecIdentity(changed); got == id {
				t.Fatalf("agentSpecIdentity did not change with the %s", tc.name)
			}
		})
	}
	t.Run("fields that do not pick the model are not part of it", func(t *testing.T) {
		same := base
		same.ID, same.ListenPort, same.HealthPath, same.IdleTimeoutSeconds = "rs2", 9100, "/other", 300
		same.Pinned, same.AdminState, same.Enabled = true, "force_running", true
		same.APITokenMode, same.APIToken = "set", "plain:x"
		if got := agentSpecIdentity(same); got != id {
			t.Fatalf("agentSpecIdentity = %q for a spec differing only in port/timeouts/pin/admin/token fields, want %q", got, id)
		}
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a := routing.RuntimeSpec{Binary: "ab", Args: "c"}
		b := routing.RuntimeSpec{Binary: "a", Args: "bc"}
		if agentSpecIdentity(a) == agentSpecIdentity(b) {
			t.Fatal("agentSpecIdentity conflates (binary ab, args c) with (binary a, args bc)")
		}
	})
}

// TestBuildGatewayServerBindsTheSpeechRateTrigger drives the real production wiring: the
// app-health runner is built before the gateway Server exists, so its trigger is bound
// late (ServerDeps.SetSpeechRateTrigger, called by buildGatewayServer once srv exists).
// A runner whose trigger never reached the Server would see every call refused and no
// mapping would ever get its rate automatically, with every unit test green. The call
// below is accepted only by a Server that is bound: it reserves the server and starts a
// speech run.
func TestBuildGatewayServerBindsTheSpeechRateTrigger(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "dev-secret")
	var runner *appHealthRunner
	orig := startAppHealthLoop
	startAppHealthLoop = func(r *appHealthRunner, serverTrigger <-chan string) context.CancelFunc {
		runner = r
		return func() {}
	}
	t.Cleanup(func() { startAppHealthLoop = orig })

	srv, cleanup, err := buildGatewayServer(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "memory", AppHealthProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if runner == nil || runner.speechRate == nil {
		t.Fatal("the app-health loop was started with no speech rate trigger")
	}

	ctx := context.Background()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	// A closed port: the run the trigger starts fails fast on connection refused.
	server := routing.AIServer{ID: "s1", Name: "S1", Domain: "127.0.0.1", Provider: routing.ProviderAudioCpp, Endpoint: "http://127.0.0.1:1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}
	app := routing.Application{ID: "a1", ServerID: "s1", Type: routing.ProviderAudioCpp, Port: 1, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAIAudio}, Priority: 1, Weight: 1, TimeoutMS: 1000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	mp := routing.ModelMapping{ID: "m1", ApplicationID: "a1", GatewayModelName: "tts", AppModelName: "tts_stream", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}
	if err := srv.Routes.CreateAIServer(ctx, server); err != nil {
		t.Fatalf("create server: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, mp); err != nil {
		t.Fatalf("create mapping: %v", err)
	}
	if err := srv.Routes.UpsertMappingCapabilities(ctx, "m1", []routing.CapabilityRow{
		{Capability: routing.CapabilitySpeech, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
		{Capability: routing.CapabilitySpeechClone, Verdict: routing.CapabilityNo, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
		{Capability: routing.CapabilitySpeechStreaming, Verdict: routing.CapabilityYes, Source: routing.CapabilitySourceAudioCppModels, CheckedAt: now},
	}); err != nil {
		t.Fatalf("seed capabilities: %v", err)
	}

	if !runner.speechRate(ctx, server, app, mp, "/m/a") {
		t.Fatal("the runner's trigger was refused by a Server with an eligible mapping: it is not bound to srv.TriggerSpeechRateBenchmark")
	}
	if st := srv.Benchmarks.Status("s1"); st.Mode != "speech" || st.Scope != "speech-rate" {
		t.Fatalf("benchmark status after the trigger = %+v, want a speech-rate run on s1", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for srv.Benchmarks.Status("s1").Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

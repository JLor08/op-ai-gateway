// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"sync"
	"testing"
	"time"
)

// capProvider is a streaming provider for capacity-ramp tests. It (a) streams a fixed short
// response with a per-call latency the test can make depend on the OBSERVED concurrency
// level, and (b) implements provider.MemoryProber so ProbeServerMemory reports the CURRENT
// in-flight concurrency — RequestsProcessing = live in-flight count, RequestsDeferred =
// max(0, in-flight - fakeSlots), TotalSlots = fakeSlots. This lets the during-burst sampler
// observe a real peak while the level's N requests are live (a post-drain read would see an
// idle server). The observed level is the peak in-flight count during a short "gather"
// window, which — because the ramp fires exactly n requests concurrently per level and waits
// for all before the next — equals the ramp's current level n.
type capProvider struct {
	mu       sync.Mutex
	inflight int

	gather       time.Duration
	genTPS       float64
	outputTokens int
	fakeSlots    int                           // TotalSlots reported by ProbeServerMemory (0 = unknown)
	slotsProbe   bool                          // /slots-style probe: processing=min(inflight,slots), deferred=-1 (unknown, as a real /slots leaves it)
	latencyFor   func(level int) time.Duration // nil => 0
	errFor       func(level int) error         // nil => no error
}

func (p *capProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *capProvider) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	p.mu.Lock()
	p.inflight++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight--
		p.mu.Unlock()
	}()
	// Gather window: let all concurrent peers of this level arrive so the observed
	// in-flight count reflects the true level.
	if p.gather > 0 {
		if !sleepCtx(ctx, p.gather) {
			return ctx.Err()
		}
	}
	p.mu.Lock()
	level := p.inflight
	p.mu.Unlock()
	if p.errFor != nil {
		if err := p.errFor(level); err != nil {
			return err
		}
	}
	if p.latencyFor != nil {
		if lat := p.latencyFor(level); lat > 0 {
			if !sleepCtx(ctx, lat) {
				return ctx.Err()
			}
		}
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	u := inference.Usage{OutputTokens: p.outputTokens, TokensPerSecond: p.genTPS}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func (p *capProvider) ProbeServerMemory(_ context.Context, _ routing.Target, _, _ string) (provider.ServerMemory, error) {
	p.mu.Lock()
	inflight := p.inflight
	p.mu.Unlock()
	if p.slotsProbe {
		// A real llama.cpp /slots probe reports occupied vs total slots but NOT a deferred
		// count, so ProbeServerMemory leaves RequestsDeferred at its -1 "unknown" sentinel.
		// Occupancy saturates at the slot count (extra requests queue, they don't add slots).
		processing := inflight
		if processing > p.fakeSlots {
			processing = p.fakeSlots
		}
		return provider.ServerMemory{
			RequestsProcessing: processing,
			RequestsDeferred:   -1, // unknown — the clamp's target case
			TotalSlots:         p.fakeSlots,
			OK:                 true,
		}, nil
	}
	deferred := inflight - p.fakeSlots
	if deferred < 0 {
		deferred = 0
	}
	return provider.ServerMemory{
		RequestsProcessing: inflight,
		RequestsDeferred:   deferred,
		TotalSlots:         p.fakeSlots,
		OK:                 true,
	}, nil
}

// capFakeStore embeds a routing.Store (a memory store) but overrides TelemetryByServer so a
// test can drive the per-read telemetry sample (VRAM/RAM + an advancing ReportedAt).
type capFakeStore struct {
	routing.Store
	mu    sync.Mutex
	calls int
	fn    func(call int) (routing.ServerTelemetry, bool)
}

func (f *capFakeStore) TelemetryByServer(_ context.Context, _ string) (routing.ServerTelemetry, bool, error) {
	f.mu.Lock()
	f.calls++
	c := f.calls
	f.mu.Unlock()
	t, ok := f.fn(c)
	return t, ok, nil
}

// TestCapacityRampStopsOnVRAMMargin: agent telemetry crosses the safety margin when the ramp
// probes level 4, so the ramp stops with MaxConcurrency==2 and NEVER escalates to 4+.
func TestCapacityRampStopsOnVRAMMargin(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := &capFakeStore{
		Store: routing.NewMemoryStore(),
		fn: func(c int) (routing.ServerTelemetry, bool) {
			// calls 1..3 (t0 + levels 1,2): 50% VRAM free (>= 10 margin). call 4 (level 4
			// check): 95% used -> 5% free (< 10 margin) -> STOP.
			used := int64(50)
			if c >= 4 {
				used = 95
			}
			return routing.ServerTelemetry{
				ServerID:       "srv1",
				ReportedAt:     base.Add(time.Duration(c) * time.Second),
				VRAMUsedBytes:  used,
				VRAMTotalBytes: 100,
				RAMUsedBytes:   10,
				RAMTotalBytes:  100,
			}, true
		},
	}
	srv := &Server{
		Provider:               &capProvider{genTPS: 40, outputTokens: 20},
		Routes:                 store,
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	res, err := srv.measureMappingCapacity(context.Background(), benchTestTarget(), nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 2 {
		t.Fatalf("MaxConcurrency = %d, want 2 (VRAM margin crossed at level 4)", res.MaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (agent telemetry governed the ramp)")
	}
}

// TestCapacityRampStopsOnSaturation: the during-burst sampler observes the upstream begin
// QUEUEING while the level's requests are IN FLIGHT — with fakeSlots=3 (a non-power-of-2 slot
// count), levels 1,2 stay under capacity (deferred=0, processing<slots) but level 4 peaks at
// 4 in-flight, so deferred=1>0 -> queue-stop (discard level 4). Because the queue-stop was the
// SOLE reason and the probe reported total_slots=3, that authoritative -np ceiling is reported
// as the max (the server served all 3 slots and queued the 4th), so MaxConcurrency==3 (NOT the
// last power-of-2 that fit, 2). The gather window keeps all N requests live longer than the
// sampler interval so the peak is guaranteed to be observed mid-burst (a post-drain read would
// see an idle server and never fire).
func TestCapacityRampStopsOnSaturation(t *testing.T) {
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       400 * time.Millisecond, // keep the burst live past the 250ms sampler interval
		fakeSlots:    3,
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 routing.NewMemoryStore(), // no telemetry => agentOK false
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "/metrics" // enable the upstream probe
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 3 {
		t.Fatalf("MaxConcurrency = %d, want 3 (level 4 queued -> reported total_slots=3, the -np ceiling)", res.MaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (upstream probe governed the ramp)")
	}
}

// TestCapacityRampSingleSlotServesOne: a llama.cpp --parallel 1 server (the DEFAULT) reports
// total_slots=1. Level 1 fills that single slot (processing=1>=total=1) but queues NOTHING
// (deferred=0), so that level IS the slot ceiling and was served fine — it must be COUNTED
// (MaxConcurrency==1), not discarded. Before the fix the all-slots-busy stop discarded level 1
// -> MaxConcurrency==0 -> errBenchmarkCapacityUnavailable -> nothing persisted (a healthy
// single-slot server wrongly reporting "no capacity"). This proves the ramp counts the ceiling
// AND persists it via UpdateMappingCapacityMetrics.
func TestCapacityRampSingleSlotServesOne(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       400 * time.Millisecond, // observe the single-slot peak mid-burst
		fakeSlots:    1,                      // llama.cpp --parallel 1 (the default)
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 mem,
		Benchmarks:             NewBenchmarkRegistry(),
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "/metrics" // enable the upstream probe
	run, ok := srv.Benchmarks.TryStart("srv1", "mapping", "capacity", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	res := srv.measureCapacityTarget(ctx, tgt, run, "srv1")
	if res.Error != "" {
		t.Fatalf("measureCapacityTarget Error = %q, want none (single slot served one request)", res.Error)
	}
	if res.MaxConcurrency != 1 {
		t.Fatalf("MaxConcurrency = %d, want 1 (single slot served one request with no queue)", res.MaxConcurrency)
	}
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MaxConcurrency != 1 {
		t.Fatalf("persisted MaxConcurrency = %d, want 1 (UpdateMappingCapacityMetrics must run)", got.MaxConcurrency)
	}
	if got.MetricsSource != "capacity" {
		t.Fatalf("MetricsSource = %q, want capacity (persisted a viable single-slot level)", got.MetricsSource)
	}
}

// TestCapacityRampPowerOfTwoSlotsExact: a server with total_slots=4 (a power of 2). The ramp
// escalates 1,2,4; at level 4 all 4 slots are busy (processing=4>=total=4) with NOTHING queued
// (deferred=0), so level 4 IS the slot ceiling and was served fine -> MaxConcurrency==4. Before
// the fix the all-slots-busy stop discarded level 4 -> MaxConcurrency==2, a 2x under-report of
// a server that served 4 concurrent requests with no queue.
func TestCapacityRampPowerOfTwoSlotsExact(t *testing.T) {
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       400 * time.Millisecond, // observe every level's full peak mid-burst
		fakeSlots:    4,
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 routing.NewMemoryStore(), // no telemetry => agentOK false
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "/metrics" // enable the upstream probe
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 4 {
		t.Fatalf("MaxConcurrency = %d, want 4 (all 4 slots served with no queue -> the -np ceiling)", res.MaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (upstream probe governed the ramp)")
	}
}

// TestCapacityRampSlotsProbeNonPowerOfTwoExact: a real llama.cpp /slots probe reports total_slots
// but NOT a deferred count, so RequestsDeferred stays -1 (unknown) => peakDeferred<=0 is always
// true and the queue-stop can never fire. With total_slots=3 (a non-power-of-2 slot count) the
// ramp escalates 1,2,4; at level 4 occupancy saturates at 3 (processing=min(4,3)=3>=total=3) with
// no deferred signal, so atSlotCeiling fires and level 4 is COUNTED as good — but 4 OVERSHOT the
// true -np of 3. The clamp must pull MaxConcurrency back down to the reported slot ceiling (3), not
// leave it at the last power-of-2 level (4). Before the clamp this recorded lastGood=4 (over-report);
// after, MaxConcurrency==3 exactly. This is the real-/slots case the queue-stop path (deferred>0)
// never reaches.
func TestCapacityRampSlotsProbeNonPowerOfTwoExact(t *testing.T) {
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       400 * time.Millisecond, // observe the saturated peak (processing=3) mid-burst
		fakeSlots:    3,
		slotsProbe:   true, // /slots leaves RequestsDeferred=-1 (unknown)
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 routing.NewMemoryStore(), // no telemetry => agentOK false
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "/slots" // enable the upstream probe
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 3 {
		t.Fatalf("MaxConcurrency = %d, want 3 (level 4 overshot total_slots=3 with deferred unknown; clamp to the true -np)", res.MaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (upstream probe governed the ramp)")
	}
}

// TestCapacityRampRAMMarginCPUOnly: a CPU-only llama.cpp host has NO GPU (VRAMTotalBytes=0), so
// RAM is the relevant memory. The agent reports RAM crossing the safety margin at the level-4
// check -> the memory guard must fire (MaxConcurrency==2). Before the fix agentOK required
// VRAMTotalBytes>0, so on a CPU-only host agentOK was false, the memory guard never ran, and
// the ramp escalated to the configured cap (8 here) on the latency fallback alone.
func TestCapacityRampRAMMarginCPUOnly(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := &capFakeStore{
		Store: routing.NewMemoryStore(),
		fn: func(c int) (routing.ServerTelemetry, bool) {
			// CPU-only: NO GPU (VRAMTotalBytes=0). RAM 50% free through t0 + levels 1,2
			// (calls 1..3), then 95% used = 5% free (< 10 margin) at the level-4 check (c>=4).
			ramUsed := int64(50)
			if c >= 4 {
				ramUsed = 95
			}
			return routing.ServerTelemetry{
				ServerID:       "srv1",
				ReportedAt:     base.Add(time.Duration(c) * time.Second),
				VRAMUsedBytes:  0,
				VRAMTotalBytes: 0, // no GPU -> the old agentOK (VRAM>0) would be false
				RAMUsedBytes:   ramUsed,
				RAMTotalBytes:  100,
			}, true
		},
	}
	srv := &Server{
		Provider:               &capProvider{genTPS: 40, outputTokens: 20}, // flat latency
		Routes:                 store,
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 8, // pre-fix (agentOK=false) would ramp to this cap
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "" // no upstream probe -> RAM margin is the only ceiling
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 2 {
		t.Fatalf("MaxConcurrency = %d, want 2 (RAM margin crossed at level 4 on a CPU-only host)", res.MaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (RAM telemetry governed the ramp on a CPU-only host)")
	}
}

// TestCapacityRampStopsOnLatencyCollapseWithAgent: an agent reports flat, never-breached
// VRAM (the common llama.cpp case — VRAM is pre-allocated so the margin guard never trips),
// there is NO upstream probe path, and per-request latency collapses (> 4x L1) at level 8.
// The additive stop-chain evaluates latency-collapse EVEN WHEN an agent is present (the old
// mutually-exclusive switch never checked latency once agentOK was true, so the ramp ran to
// the configured cap). Expect the ramp to stop at level 8 -> MaxConcurrency==4 (< the cap).
func TestCapacityRampStopsOnLatencyCollapseWithAgent(t *testing.T) {
	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	store := &capFakeStore{
		Store: routing.NewMemoryStore(),
		fn: func(c int) (routing.ServerTelemetry, bool) {
			// Flat 50% VRAM/90% RAM free at every read (never below the 10% margin), with a
			// strictly-advancing ReportedAt so waitFreshTelemetry always sees a fresh sample.
			return routing.ServerTelemetry{
				ServerID:       "srv1",
				ReportedAt:     base.Add(time.Duration(c) * time.Second),
				VRAMUsedBytes:  50,
				VRAMTotalBytes: 100,
				RAMUsedBytes:   10,
				RAMTotalBytes:  100,
			}, true
		},
	}
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       40 * time.Millisecond, // reliable level detection through level 8
		latencyFor: func(level int) time.Duration {
			if level >= 8 {
				return 300 * time.Millisecond // L8 mean ~= 340ms > 4x L1 (~60ms) -> collapse
			}
			return 20 * time.Millisecond // L1..L4 mean ~= 60ms
		},
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 store,
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 16, // without the latency check the ramp would run to 16
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "" // no upstream probe -> latency-collapse is the only ceiling
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 4 {
		t.Fatalf("MaxConcurrency = %d, want 4 (latency collapse at level 8, checked despite the agent)", res.MaxConcurrency)
	}
	if res.MaxConcurrency >= srv.capacityMaxConcurrency {
		t.Fatalf("MaxConcurrency = %d, want < cap %d (latency must stop the ramp before the cap)", res.MaxConcurrency, srv.capacityMaxConcurrency)
	}
	if !res.MemoryObserved {
		t.Fatalf("MemoryObserved = false, want true (agent telemetry governed the ramp)")
	}
}

// TestCapacityRampStopsOnError: a request failing at level 4 (any 5xx/error surfaced by
// streamOnce) stops the ramp at MaxConcurrency==2 — stop-condition (i).
func TestCapacityRampStopsOnError(t *testing.T) {
	prov := &capProvider{
		genTPS:       40,
		outputTokens: 20,
		gather:       15 * time.Millisecond, // ensure all level-4 requests observe level 4
		errFor: func(level int) error {
			if level >= 4 {
				return context.DeadlineExceeded // any error
			}
			return nil
		},
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 routing.NewMemoryStore(), // no telemetry => latency/error fallback
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = ""
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 2 {
		t.Fatalf("MaxConcurrency = %d, want 2 (errors at level 4)", res.MaxConcurrency)
	}
	// CP2b: the level that tripped the stop (the last recorded level, concurrency 4)
	// carries a non-empty StopReason ("error").
	if len(res.Levels) == 0 {
		t.Fatalf("len(Levels) = 0, want the attempted levels recorded")
	}
	last := res.Levels[len(res.Levels)-1]
	if last.StopReason != "error" {
		t.Fatalf("last level StopReason = %q, want error", last.StopReason)
	}
	if last.Concurrency != 4 {
		t.Fatalf("last level Concurrency = %d, want 4 (the level that errored)", last.Concurrency)
	}
}

// TestCapacityRampWarmupErrorNoPersist: a totally-unavailable upstream (warm pass errors)
// yields an error result with MaxConcurrency==0, so measureCapacityTarget must NOT persist
// any capacity metrics (the OOM-safe "no viable level => write nothing" rule).
func TestCapacityRampWarmupErrorNoPersist(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	// errFor returns an error at every level (>=1), so the warm pass itself fails.
	prov := &capProvider{errFor: func(int) error { return context.DeadlineExceeded }}
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: prov, Routes: mem, Benchmarks: reg, capacityMaxConcurrency: 4, capacitySettle: 20 * time.Millisecond}

	run, ok := reg.TryStart("srv1", "mapping", "capacity", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	res := srv.measureCapacityTarget(ctx, benchTestTarget(), run, "srv1")
	if res.Error == "" {
		t.Fatalf("measureCapacityTarget Error empty, want a warm-pass error")
	}
	if res.MaxConcurrency != 0 {
		t.Fatalf("MaxConcurrency = %d, want 0 on a failed warm pass", res.MaxConcurrency)
	}
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MaxConcurrency != 0 || got.MetricsSource == "capacity" {
		t.Fatalf("capacity metrics persisted on a failed run: max=%d source=%q, want no persist", got.MaxConcurrency, got.MetricsSource)
	}
}

// TestCapacityRampFallbackNoAgent: no agent telemetry AND no upstream probe path -> the ramp
// runs on the latency-collapse fallback and, with flat latency, escalates until it hits the
// configured ceiling. MemoryObserved must be false.
func TestCapacityRampFallbackNoAgent(t *testing.T) {
	srv := &Server{
		Provider:               &capProvider{genTPS: 50, outputTokens: 20}, // no latency => flat
		Routes:                 routing.NewMemoryStore(),                   // no telemetry => agentOK false
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 4, // ceiling
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = "" // no upstream probe
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 4 {
		t.Fatalf("MaxConcurrency = %d, want 4 (capped at capacityMaxConcurrency)", res.MaxConcurrency)
	}
	if res.MemoryObserved {
		t.Fatalf("MemoryObserved = true, want false (no agent + no probe => latency fallback)")
	}
	if res.GenTokensPerSecondAtCapacity != 50 {
		t.Fatalf("GenTokensPerSecondAtCapacity = %v, want 50", res.GenTokensPerSecondAtCapacity)
	}
	// CP2b: every attempted level (1,2,4 — capped at 4) is recorded, first level is
	// concurrency 1, and no level tripped a stop (flat latency, no agent/probe, capped).
	if len(res.Levels) != 3 {
		t.Fatalf("len(Levels) = %d, want 3 (attempted 1,2,4)", len(res.Levels))
	}
	if res.Levels[0].Concurrency != 1 {
		t.Fatalf("Levels[0].Concurrency = %d, want 1", res.Levels[0].Concurrency)
	}
	if got := []int{res.Levels[0].Concurrency, res.Levels[1].Concurrency, res.Levels[2].Concurrency}; got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("recorded concurrency levels = %v, want [1 2 4]", got)
	}
	for i, lv := range res.Levels {
		if lv.StopReason != "" {
			t.Fatalf("Levels[%d].StopReason = %q, want empty (no stop, capped)", i, lv.StopReason)
		}
	}
}

// TestCapacityRampLatencyKnee: latency stays low through level 2, rises above 1.5x L1 at
// level 4, and collapses (> 4x L1) at level 16 while MaxConcurrency reaches 8. The
// recommended concurrency is the last level within the 1.5x knee (level 2, < MaxConcurrency).
func TestCapacityRampLatencyKnee(t *testing.T) {
	prov := &capProvider{
		genTPS:       50,
		outputTokens: 20,
		gather:       20 * time.Millisecond,
		latencyFor: func(level int) time.Duration {
			switch {
			case level <= 2:
				return 30 * time.Millisecond // L1,L2: measured ~50ms
			case level <= 8:
				return 120 * time.Millisecond // L4,L8: ~140ms (> 1.5xL1, <= 4xL1)
			default:
				return 600 * time.Millisecond // L16: ~620ms (> 4xL1) -> collapse
			}
		},
	}
	srv := &Server{
		Provider:               prov,
		Routes:                 routing.NewMemoryStore(), // no telemetry => latency fallback
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 64,
		capacitySettle:         20 * time.Millisecond,
	}
	tgt := benchTestTarget()
	tgt.app.CapacityProbePath = ""
	res, err := srv.measureMappingCapacity(context.Background(), tgt, nil)
	if err != nil {
		t.Fatalf("measureMappingCapacity err = %v", err)
	}
	if res.MaxConcurrency != 8 {
		t.Fatalf("MaxConcurrency = %d, want 8 (collapse at level 16)", res.MaxConcurrency)
	}
	if res.RecommendedConcurrency != 2 {
		t.Fatalf("RecommendedConcurrency = %d, want 2 (last level within 1.5x L1)", res.RecommendedConcurrency)
	}
	if res.RecommendedConcurrency >= res.MaxConcurrency {
		t.Fatalf("RecommendedConcurrency (%d) must be < MaxConcurrency (%d)", res.RecommendedConcurrency, res.MaxConcurrency)
	}
}

// TestCapacityRampPersists: a capacity run (mode="capacity") distills + persists the three
// headline scalars via UpdateMappingCapacityMetrics (metrics_source="capacity").
func TestCapacityRampPersists(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	reg := NewBenchmarkRegistry()
	srv := &Server{
		Provider:               &capProvider{genTPS: 50, outputTokens: 20}, // flat latency
		Routes:                 mem,
		Benchmarks:             reg,
		capacityVRAMMarginPct:  10,
		capacityMaxConcurrency: 2, // ramp reaches 2 then stops at the cap
		capacitySettle:         20 * time.Millisecond,
	}
	run, ok := reg.TryStart("srv1", "mapping", "capacity", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "capacity")

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MaxConcurrency != 2 {
		t.Fatalf("persisted MaxConcurrency = %d, want 2", got.MaxConcurrency)
	}
	if got.RecommendedConcurrency != 2 {
		t.Fatalf("persisted RecommendedConcurrency = %d, want 2", got.RecommendedConcurrency)
	}
	if got.GenTokensPerSecondAtCapacity != 50 {
		t.Fatalf("persisted GenTokensPerSecondAtCapacity = %v, want 50", got.GenTokensPerSecondAtCapacity)
	}
	if got.MetricsSource != "capacity" {
		t.Fatalf("MetricsSource = %q, want capacity", got.MetricsSource)
	}
}

// TestBenchmarkModeSpeedUnchanged: mode="speed" runs ONLY the speed path (metrics_source
// stays "benchmark", no capacity scalars written). A junk mode via the HTTP mode-parse
// helper is rejected 400.
func TestBenchmarkModeSpeedUnchanged(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: &capProvider{genTPS: 55, outputTokens: 20}, Routes: mem, Benchmarks: reg, capacityMaxConcurrency: 4, capacitySettle: 20 * time.Millisecond}
	run, ok := reg.TryStart("srv1", "mapping", "speed", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "speed")

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MetricsSource != "benchmark" {
		t.Fatalf("MetricsSource = %q, want benchmark (speed path only)", got.MetricsSource)
	}
	if got.GenTokensPerSecond != 55 {
		t.Fatalf("GenTokensPerSecond = %v, want 55 (speed path ran)", got.GenTokensPerSecond)
	}
	if got.MaxConcurrency != 0 || got.RecommendedConcurrency != 0 || got.GenTokensPerSecondAtCapacity != 0 {
		t.Fatalf("capacity scalars written on a speed run: max=%d rec=%d gen=%v, want all 0", got.MaxConcurrency, got.RecommendedConcurrency, got.GenTokensPerSecondAtCapacity)
	}

	// Junk mode via the HTTP mode-parse helper -> 400 benchmark.mode_invalid.
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/map1/benchmark?mode=junk", nil)
	if mode, ok := parseBenchmarkMode(rec, r); ok {
		t.Fatalf("parseBenchmarkMode(junk) ok = true (mode %q), want false", mode)
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("junk mode HTTP status = %d, want 400", rec.Code)
	}

	// Empty mode -> "speed"; explicit modes pass through.
	for in, want := range map[string]string{"": "speed", "speed": "speed", "capacity": "capacity", "both": "both", "vision": "vision"} {
		rec2 := httptest.NewRecorder()
		q := ""
		if in != "" {
			q = "?mode=" + in
		}
		r2 := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/map1/benchmark"+q, nil)
		got, ok := parseBenchmarkMode(rec2, r2)
		if !ok || got != want {
			t.Fatalf("parseBenchmarkMode(%q) = (%q,%v), want (%q,true)", in, got, ok, want)
		}
	}
}

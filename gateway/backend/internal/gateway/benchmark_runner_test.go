// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// benchFakeProvider is a minimal streaming provider for runner tests. It emits one
// text delta then a Completed event carrying a configurable usage. A per-call
// counter lets it behave differently on the cold (call #1) vs warm (call #2)
// request so load-time measurement can be exercised.
type benchFakeProvider struct {
	calls        int
	firstDelayMS int // sleep before the first delta on call #1 (simulates a cold load)
	usage        inference.Usage
}

func (f *benchFakeProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (f *benchFakeProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	f.calls++
	if f.calls == 1 && f.firstDelayMS > 0 {
		time.Sleep(time.Duration(f.firstDelayMS) * time.Millisecond)
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	u := f.usage
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func benchTestTarget() benchmarkTarget {
	return benchmarkTarget{
		server:  routing.AIServer{ID: "srv1", Domain: "host.example.test"},
		app:     routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000},
		mapping: routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive},
	}
}

func TestBenchmarkMeasureMappingUpstreamRates(t *testing.T) {
	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 42, PromptPerSecond: 900}}
	srv := &Server{Provider: fake}
	res, err := srv.measureMapping(context.Background(), benchTestTarget())
	if err != nil {
		t.Fatalf("measureMapping err = %v", err)
	}
	if res.GenTokensPerSecond != 42 {
		t.Fatalf("GenTokensPerSecond = %v, want 42", res.GenTokensPerSecond)
	}
	if res.PromptTokensPerSecond != 900 {
		t.Fatalf("PromptTokensPerSecond = %v, want 900", res.PromptTokensPerSecond)
	}
	if res.MappingID != "map1" || res.GatewayModelName != "gw-model" {
		t.Fatalf("result identity = %q/%q, want map1/gw-model", res.MappingID, res.GatewayModelName)
	}
}

func TestBenchmarkMeasureMappingLoadTimeFromColdWarm(t *testing.T) {
	// A load time is recorded only when a cold start is CONFIRMED. Configure a loaded-probe
	// (empty loaded set => ensureColdLoad confirms cold immediately) so the cold-minus-warm
	// delta is emitted; the firstDelayMS makes the cold pass reliably slower than the warm.
	fake := newColdLister(nil)
	fake.firstDelayMS = 40
	fake.usage = inference.Usage{OutputTokens: 20, TokensPerSecond: 42}
	srv := &Server{Provider: fake}
	tgt := benchTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"
	res, err := srv.measureMapping(context.Background(), tgt)
	if err != nil {
		t.Fatalf("measureMapping err = %v", err)
	}
	if res.LoadTimeMS <= 0 {
		t.Fatalf("LoadTimeMS = %d, want > 0 (cold call slower than warm)", res.LoadTimeMS)
	}
}

func TestBenchmarkMeasureMappingDerivesRateWhenUpstreamSilent(t *testing.T) {
	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 10}} // TokensPerSecond: 0
	srv := &Server{Provider: fake}
	res, err := srv.measureMapping(context.Background(), benchTestTarget())
	if err != nil {
		t.Fatalf("measureMapping err = %v", err)
	}
	if res.GenTokensPerSecond <= 0 {
		t.Fatalf("GenTokensPerSecond = %v, want > 0 (wall-clock fallback)", res.GenTokensPerSecond)
	}
}

func TestBenchmarkMeasureMappingNoStreaming(t *testing.T) {
	srv := &Server{Provider: nonStreamingProvider{}}
	if _, err := srv.measureMapping(context.Background(), benchTestTarget()); err != errBenchmarkNoStreaming {
		t.Fatalf("measureMapping err = %v, want errBenchmarkNoStreaming", err)
	}
}

// nonStreamingProvider implements provider.Client but not StreamingClient.
type nonStreamingProvider struct{}

func (nonStreamingProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func TestBenchmarkRunPersistsAndFinishes(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
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

	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55, PromptPerSecond: 1200}}
	srv := &Server{Provider: fake, Routes: mem}

	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}

	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "speed")

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.GenTokensPerSecond != 55 {
		t.Fatalf("persisted GenTokensPerSecond = %v, want 55", got.GenTokensPerSecond)
	}
	if got.PromptTokensPerSecond != 1200 {
		t.Fatalf("persisted PromptTokensPerSecond = %v, want 1200", got.PromptTokensPerSecond)
	}
	if got.MetricsSource != "benchmark" {
		t.Fatalf("MetricsSource = %q, want benchmark", got.MetricsSource)
	}

	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy true after run, want cleared (finish ran)")
	}
	if st := reg.Status("srv1"); st.Running {
		t.Fatalf("status Running true after run, want false")
	} else if st.Error != "" {
		t.Fatalf("status Error = %q, want empty", st.Error)
	} else if st.Done != 1 {
		t.Fatalf("status Done = %d, want 1", st.Done)
	}
}

// benchProbingProvider is a streaming fake that ALSO implements
// provider.ModelInfoProber, so runBenchmark's context re-probe path is exercised.
type benchProbingProvider struct {
	benchFakeProvider
	probeName    string
	probeContext int
	probedPath   string // records the path passed to the most recent ProbeModelInfo call
}

// ProbeModelInfo records the path it was called with. The benchmark re-probe issues one
// probe per target strictly sequentially inside the single-goroutine runBenchmark loop, so
// a plain (unguarded) field is race-free here.
func (f *benchProbingProvider) ProbeModelInfo(_ context.Context, _ routing.Target, path string) ([]provider.ModelInfo, error) {
	f.probedPath = path
	return []provider.ModelInfo{{Name: f.probeName, ContextSize: f.probeContext}}, nil
}

func TestBenchmarkRunReprobeKeepsBenchmarkSource(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, ContextProbePath: "/props", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	fake := &benchProbingProvider{
		benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55, PromptPerSecond: 1200}},
		probeName:         "up-model",
		probeContext:      131072,
	}
	srv := &Server{Provider: fake, Routes: mem}

	// The target's app must carry ContextProbePath so runBenchmark triggers the probe.
	tgt := benchTestTarget()
	tgt.app.ContextProbePath = "/props"

	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{tgt}, "speed")

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MetricsSource != "benchmark" {
		t.Fatalf("MetricsSource = %q, want benchmark (benchmark write must land LAST, after the probe)", got.MetricsSource)
	}
	if got.ContextSize != 131072 {
		t.Fatalf("ContextSize = %d, want 131072 (probed context must survive the benchmark write)", got.ContextSize)
	}
	if got.GenTokensPerSecond != 55 {
		t.Fatalf("GenTokensPerSecond = %v, want 55", got.GenTokensPerSecond)
	}
}

// TestBenchmarkRunReprobeExpandsModelTemplate: a {model} ContextProbePath is expanded with
// the mapping's upstream name (so a per-model props endpoint is queried) and the returned
// context size is attributed DIRECTLY to the mapping — even when the reported model name
// diverges from AppModelName (the benchmark just warmed this exact model, so it is resident).
func TestBenchmarkRunReprobeExpandsModelTemplate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, ContextProbePath: "/upstream/{model}/props", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "m-a", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	fake := &benchProbingProvider{
		benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55, PromptPerSecond: 1200}},
		probeName:         "some-basename", // DIVERGENT reported name — must not defeat direct attribution
		probeContext:      8192,
	}
	srv := &Server{Provider: fake, Routes: mem}

	tgt := benchTestTarget()
	tgt.app.ContextProbePath = "/upstream/{model}/props"
	tgt.mapping.AppModelName = "m-a"

	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{tgt}, "speed")

	if fake.probedPath != "/upstream/m-a/props" {
		t.Fatalf("probed path = %q, want /upstream/m-a/props (the {model} template must be expanded)", fake.probedPath)
	}
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.ContextSize != 8192 {
		t.Fatalf("ContextSize = %d, want 8192 (direct attribution despite a divergent reported name)", got.ContextSize)
	}
}

// benchHangingProvider is a streaming provider whose CompleteStream blocks until its
// context is cancelled and NEVER emits — modelling a stalled upstream (2xx then silent,
// or a cold load that never produces a first token). The only thing that ends it is the
// idle watchdog's ctx cancel.
type benchHangingProvider struct{}

func (benchHangingProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (benchHangingProvider) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, _ provider.StreamEmit) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestBenchmarkRunHungUpstreamSelfTerminates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
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

	srv := &Server{Provider: benchHangingProvider{}, Routes: mem}
	srv.streamIdleTimeout = 40 * time.Millisecond // watchdog fires fast

	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}

	done := make(chan struct{})
	go func() {
		srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "speed")
		close(done)
	}()

	select {
	case <-done:
		// runBenchmark returned -> the watchdog self-terminated the hung stream.
	case <-time.After(2 * time.Second):
		t.Fatalf("runBenchmark did not return within 2s against a hung upstream (watchdog did not fire)")
	}

	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy still true after a hung run — server-busy leaked (finish did not run)")
	}
	st := reg.Status("srv1")
	if st.Running {
		t.Fatalf("status Running still true after a hung run")
	}
	if len(st.Results) != 1 || st.Results[0].Error == "" {
		t.Fatalf("expected one result carrying an error, got %+v", st.Results)
	}

	// No metrics were persisted for the hung mapping.
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.MetricsSource != "" || got.GenTokensPerSecond != 0 {
		t.Fatalf("hung mapping should have no persisted metrics, got source=%q gen=%v", got.MetricsSource, got.GenTokensPerSecond)
	}
}

// TestBenchmarkRunPublishesProgress asserts the runner fans a progress frame out to
// a live subscriber after each measured mapping (Results accumulating Done 1 -> 2)
// plus a terminal frame with Running==false once the run finishes.
func TestBenchmarkRunPublishesProgress(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping map1: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map2", ApplicationID: "app1", GatewayModelName: "gw-model-2", AppModelName: "up-model-2", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping map2: %v", err)
	}

	reg := NewBenchmarkRegistry()
	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}}
	srv := &Server{Provider: fake, Routes: mem, Benchmarks: reg}

	// Subscribe BEFORE the run so no frame is missed.
	_, ch, unsub := reg.Subscribe("srv1")
	defer unsub()

	run, ok := reg.TryStart("srv1", "server", "speed", 2, now, func() {})
	if !ok {
		t.Fatalf("TryStart failed")
	}

	tgt1 := benchTestTarget() // map1
	tgt2 := benchTestTarget()
	tgt2.mapping.ID = "map2"
	tgt2.mapping.GatewayModelName = "gw-model-2"
	tgt2.mapping.AppModelName = "up-model-2"

	// Run in a goroutine + read each frame with a timeout so a dropped frame can't
	// hang the test (the subscriber channel is never closed on the happy path).
	go srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{tgt1, tgt2}, "speed")

	readFrame := func() BenchmarkStatus {
		t.Helper()
		select {
		case st := <-ch:
			return st
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for a progress frame")
			return BenchmarkStatus{}
		}
	}

	f1 := readFrame()
	if f1.Done != 1 || !f1.Running || len(f1.Results) != 1 {
		t.Fatalf("frame 1 = %#v, want Done 1, Running true, 1 result", f1)
	}
	f2 := readFrame()
	if f2.Done != 2 || !f2.Running || len(f2.Results) != 2 {
		t.Fatalf("frame 2 = %#v, want Done 2, Running true, 2 results", f2)
	}
	f3 := readFrame()
	if f3.Running || f3.Done != 2 || len(f3.Results) != 2 {
		t.Fatalf("terminal frame = %#v, want Running false, Done 2, 2 results", f3)
	}
	if f3.Error != "" {
		t.Fatalf("terminal frame Error = %q, want empty", f3.Error)
	}
}

func TestBenchmarkRunCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mem := routing.NewMemoryStore()
	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}}
	srv := &Server{Provider: fake, Routes: mem}
	reg := NewBenchmarkRegistry()
	run, ok := reg.TryStart("srv1", "owner", "speed", 1, time.Now(), func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "speed")
	st := reg.Status("srv1")
	if st.Running {
		t.Fatalf("status Running true after canceled run, want false")
	}
	if st.Error != "canceled" {
		t.Fatalf("status Error = %q, want canceled", st.Error)
	}
}

// TestCapacityRunWritesHistoryCurve: a capacity-mode run appends exactly ONE
// benchmark-history row of kind "capacity" whose CapacityCurve unmarshals to a
// routing.CapacityReport with a non-empty per-level curve (CP2b). It also proves a
// capacity run writes NO speed-history row (the mode != "capacity" guard).
func TestCapacityRunWritesHistoryCurve(t *testing.T) {
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
		capacityMaxConcurrency: 4, // ramp 1,2,4 then cap
		capacitySettle:         20 * time.Millisecond,
	}
	run, ok := reg.TryStart("srv1", "mapping", "capacity", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "capacity")

	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	// A capacity run writes exactly ONE history row of kind "capacity" and NO speed row
	// (the mode != "capacity" guard in runBenchmark).
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want exactly 1 (one capacity row, no speed row)", len(rows))
	}
	curve := rows[0]
	if curve.Kind != "capacity" {
		t.Fatalf("history row Kind = %q, want capacity", curve.Kind)
	}
	if curve.CapacityCurve == "" {
		t.Fatalf("capacity row CapacityCurve empty, want the marshaled report")
	}
	var report routing.CapacityReport
	if err := json.Unmarshal([]byte(curve.CapacityCurve), &report); err != nil {
		t.Fatalf("CapacityCurve does not unmarshal to CapacityReport: %v", err)
	}
	if len(report.Levels) == 0 {
		t.Fatalf("report.Levels empty, want the per-level curve recorded")
	}
	if report.Levels[0].Concurrency != 1 {
		t.Fatalf("report.Levels[0].Concurrency = %d, want 1", report.Levels[0].Concurrency)
	}
	if report.MaxConcurrency != 4 {
		t.Fatalf("report.MaxConcurrency = %d, want 4 (capped)", report.MaxConcurrency)
	}
}

// bothSplitProvider succeeds for the SPEED benchmark (MaxTokens 64) and errors for
// the CAPACITY ramp (MaxTokens == capacityRampMaxTokens, 128), so a "both" run has a
// successful speed measurement together with a failed capacity ramp — the exact
// combination that must NOT mislabel the speed-history row with the capacity error.
type bothSplitProvider struct{}

func (bothSplitProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (bothSplitProvider) CompleteStream(_ context.Context, _ routing.Target, req inference.Request, emit provider.StreamEmit) error {
	if req.MaxTokens == capacityRampMaxTokens {
		return errors.New("capacity upstream boom")
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	u := inference.Usage{OutputTokens: 20, TokensPerSecond: 55, PromptPerSecond: 1200}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &u})
}

func benchSeedStore(t *testing.T, mem *routing.MemoryStore, now time.Time) {
	t.Helper()
	ctx := context.Background()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
}

// TestBothModeSpeedRowNotMislabeledByCapacityError: in a "both" run where the speed
// benchmark SUCCEEDS but the capacity ramp FAILS, the speed-history row must record an
// empty error (its own outcome), NOT the capacity error the run merges into the live
// status. (Regression for the CP2b verification's confirmed both-mode mislabel finding.)
func TestBothModeSpeedRowNotMislabeledByCapacityError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	benchSeedStore(t, mem, now)

	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: bothSplitProvider{}, Routes: mem, Benchmarks: reg, capacityMaxConcurrency: 4, capacitySettle: 10 * time.Millisecond}
	run, ok := reg.TryStart("srv1", "mapping", "both", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "both")

	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	var speedRow, capRow *routing.BenchmarkRun
	for i := range rows {
		if rows[i].Kind == "capacity" {
			capRow = &rows[i]
		} else {
			speedRow = &rows[i]
		}
	}
	if speedRow == nil || capRow == nil {
		t.Fatalf("want one speed + one capacity history row, got %+v", rows)
	}
	if speedRow.Error != "" {
		t.Fatalf("speed-history row Error = %q, want empty (a capacity failure must not mislabel the speed row)", speedRow.Error)
	}
	if capRow.Error == "" {
		t.Fatalf("capacity-history row Error empty, want the capacity failure recorded")
	}
	// Speed metrics distilled onto the mapping; capacity NOT distilled (failed ramp).
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.GenTokensPerSecond != 55 {
		t.Fatalf("speed metric not distilled: GenTokensPerSecond = %v, want 55", got.GenTokensPerSecond)
	}
	if got.MaxConcurrency != 0 {
		t.Fatalf("capacity must NOT be distilled on a failed ramp, got MaxConcurrency %d", got.MaxConcurrency)
	}
	// The live/poll status still surfaces the capacity error (merged) for visibility.
	st := reg.Status("srv1")
	if len(st.Results) != 1 || st.Results[0].Error == "" {
		t.Fatalf("merged status should surface the capacity error, got %+v", st.Results)
	}
}

// TestCapacityCurveSanitizesNonFiniteRates: a misbehaving upstream reporting a NaN/±Inf
// tok/s must not (a) break json.Marshal into a silently-empty stored curve, nor (b)
// poison the distilled routing metric. cleanFloat coerces non-finite rates to 0.
// (Regression for the CP2b verification's plausible NaN-curve finding.)
func TestCapacityCurveSanitizesNonFiniteRates(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	benchSeedStore(t, mem, now)

	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: &capProvider{genTPS: math.NaN(), outputTokens: 20}, Routes: mem, Benchmarks: reg, capacityMaxConcurrency: 4, capacitySettle: 10 * time.Millisecond}
	run, ok := reg.TryStart("srv1", "mapping", "capacity", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "capacity")

	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want 1", len(rows))
	}
	if rows[0].CapacityCurve == "" {
		t.Fatalf("CapacityCurve empty — a NaN rate broke json.Marshal (sanitize failed)")
	}
	var report routing.CapacityReport
	if err := json.Unmarshal([]byte(rows[0].CapacityCurve), &report); err != nil {
		t.Fatalf("CapacityCurve does not unmarshal (non-finite leaked into JSON): %v", err)
	}
	if report.GenTokensPerSecondAtCapacity != 0 {
		t.Fatalf("GenTokensPerSecondAtCapacity = %v, want 0 (NaN sanitized)", report.GenTokensPerSecondAtCapacity)
	}
	for _, lv := range report.Levels {
		if math.IsNaN(lv.AggregateTokensPerSecond) || math.IsInf(lv.AggregateTokensPerSecond, 0) ||
			math.IsNaN(lv.PerRequestTokensPerSecond) || math.IsInf(lv.PerRequestTokensPerSecond, 0) {
			t.Fatalf("level %d carries a non-finite rate: %+v", lv.Concurrency, lv)
		}
	}
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if math.IsNaN(got.GenTokensPerSecondAtCapacity) || math.IsInf(got.GenTokensPerSecondAtCapacity, 0) {
		t.Fatalf("distilled GenTokensPerSecondAtCapacity is non-finite: %v", got.GenTokensPerSecondAtCapacity)
	}
}

// TestVisionRunWritesHistoryRow: a vision-mode run appends exactly ONE
// benchmark-history row of kind "vision" carrying the definitive verdict, and
// distills vision_capable onto the mapping. It also proves a vision run writes NO
// speed-history row (the mode != "vision" guard).
func TestVisionRunWritesHistoryRow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	benchSeedStore(t, mem, now)

	svc := portal.NewService(portal.ServiceDeps{SystemSettings: portal.NewMemorySystemSettings()})
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: visionFakeProvider{answer: "a photo"}, Routes: mem, Portal: svc, Benchmarks: reg}
	run, ok := reg.TryStart("srv1", "mapping", "vision", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "vision")

	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want exactly 1 (one vision row, no speed row)", len(rows))
	}
	row := rows[0]
	if row.Kind != "vision" {
		t.Fatalf("history row Kind = %q, want vision", row.Kind)
	}
	if !row.VisionCapable {
		t.Fatalf("history row VisionCapable = false, want true (upstream accepted the image)")
	}
	if row.Error != "" {
		t.Fatalf("history row Error = %q, want empty (a definitive verdict)", row.Error)
	}

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if !got.VisionCapable {
		t.Fatalf("mapping VisionCapable = false, want true (distilled from the definitive verdict)")
	}
}

// TestVisionRunInconclusiveWritesHistoryRowWithError: a vision run whose probe is
// inconclusive (the baseline text call fails, so no verdict can be reached) still
// appends a kind="vision" history row — with VisionCapable false and the error
// recorded — and does NOT touch the mapping's vision_capable column.
func TestVisionRunInconclusiveWritesHistoryRowWithError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	benchSeedStore(t, mem, now)

	svc := portal.NewService(portal.ServiceDeps{SystemSettings: portal.NewMemorySystemSettings()})
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: visionFakeProvider{failText: true}, Routes: mem, Portal: svc, Benchmarks: reg}
	run, ok := reg.TryStart("srv1", "mapping", "vision", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	srv.runBenchmark(ctx, run, "srv1", []benchmarkTarget{benchTestTarget()}, "vision")

	rows, err := mem.BenchmarkRunsByMapping(ctx, "map1", 50)
	if err != nil {
		t.Fatalf("BenchmarkRunsByMapping: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("history rows = %d, want exactly 1", len(rows))
	}
	row := rows[0]
	if row.Kind != "vision" {
		t.Fatalf("history row Kind = %q, want vision", row.Kind)
	}
	if row.VisionCapable {
		t.Fatalf("history row VisionCapable = true, want false (inconclusive)")
	}
	if row.Error == "" {
		t.Fatalf("history row Error empty, want the inconclusive-probe error recorded")
	}

	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.VisionCapable {
		t.Fatalf("mapping VisionCapable = true, want false (an inconclusive verdict must not be distilled)")
	}
}

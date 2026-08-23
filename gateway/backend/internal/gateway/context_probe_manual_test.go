// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// ctxProbeSeedStore seeds srv1/app1/map1 (app carries the given context-probe path;
// the mapping starts with context_size 0 so a "no persist" assertion is meaningful).
func ctxProbeSeedStore(t *testing.T, probePath, appModel string) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock, Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http", TimeoutMS: 30000, ContextProbePath: probePath, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: appModel, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	return mem
}

// TestRunContextProbeReportsWithoutPersist: the happy path — runContextProbe warm-loads then
// probes, publishes a TERMINAL status (Running=false) carrying results[0].context_size == the
// fake's probeContext, frees the server (ServerBusy false), and writes NOTHING to the store
// (the mapping's stored context_size stays 0 — this op must not persist).
func TestRunContextProbeReportsWithoutPersist(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	mem := ctxProbeSeedStore(t, "/props", "up-model")

	fake := &benchProbingProvider{
		benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}},
		probeName:         "up-model",
		probeContext:      8192,
	}
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: fake, Routes: mem, Benchmarks: reg}

	run, ok := reg.TryStart("srv1", "context-probe", "context", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()
	tgt.app.ContextProbePath = "/props"

	srv.runContextProbe(ctx, run, "srv1", tgt)

	st := reg.Status("srv1")
	if st.Running {
		t.Fatalf("status Running true after probe, want false (finish ran)")
	}
	if len(st.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(st.Results))
	}
	if st.Results[0].ContextSize != 8192 {
		t.Fatalf("results[0].ContextSize = %d, want 8192 (reported probed context)", st.Results[0].ContextSize)
	}
	if st.Results[0].Error != "" {
		t.Fatalf("results[0].Error = %q, want empty", st.Results[0].Error)
	}
	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy true after probe, want cleared (finish freed the server)")
	}
	// The op must NOT persist: the stored mapping's context/metrics are untouched.
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.ContextSize != 0 {
		t.Fatalf("stored ContextSize = %d, want 0 (probe must not persist)", got.ContextSize)
	}
	if got.MetricsSource != "" {
		t.Fatalf("stored MetricsSource = %q, want empty (probe must not persist)", got.MetricsSource)
	}
}

// TestRunContextProbeExpandsModelTemplate: a {model} context-probe path is expanded with the
// mapping's upstream name (so a per-model endpoint is queried) and the returned context is
// reported — even when the reported model name diverges (PickModelContextSize's first-positive
// fallback). Still no store write.
func TestRunContextProbeExpandsModelTemplate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	mem := ctxProbeSeedStore(t, "/upstream/{model}/props", "m-a")

	fake := &benchProbingProvider{
		benchFakeProvider: benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}},
		probeName:         "some-basename", // divergent reported name — first-positive still applies
		probeContext:      4096,
	}
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: fake, Routes: mem, Benchmarks: reg}

	run, ok := reg.TryStart("srv1", "context-probe", "context", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()
	tgt.app.ContextProbePath = "/upstream/{model}/props"
	tgt.mapping.AppModelName = "m-a"

	srv.runContextProbe(ctx, run, "srv1", tgt)

	if fake.probedPath != "/upstream/m-a/props" {
		t.Fatalf("probed path = %q, want /upstream/m-a/props (the {model} template must be expanded)", fake.probedPath)
	}
	st := reg.Status("srv1")
	if len(st.Results) != 1 || st.Results[0].ContextSize != 4096 {
		t.Fatalf("results = %+v, want one result with ContextSize 4096", st.Results)
	}
	got, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}
	if got.ContextSize != 0 {
		t.Fatalf("stored ContextSize = %d, want 0 (probe must not persist)", got.ContextSize)
	}
}

// ctxProbeErrStreamer is a streaming provider whose CompleteStream returns an error immediately,
// modelling a failed warm-load. It does NOT implement ModelInfoProber (the probe is never reached).
type ctxProbeErrStreamer struct{}

func (ctxProbeErrStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (ctxProbeErrStreamer) CompleteStream(context.Context, routing.Target, inference.Request, provider.StreamEmit) error {
	return errors.New("warm-load boom")
}

// TestRunContextProbeWarmLoadError: a failed warm-load stream yields a result carrying the error
// and context_size 0, finishes the run (Running=false), and frees the server (ServerBusy false),
// so a failed probe never permanently reserves the server.
func TestRunContextProbeWarmLoadError(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	mem := ctxProbeSeedStore(t, "/props", "up-model")

	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: ctxProbeErrStreamer{}, Routes: mem, Benchmarks: reg}

	run, ok := reg.TryStart("srv1", "context-probe", "context", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()
	tgt.app.ContextProbePath = "/props"

	srv.runContextProbe(ctx, run, "srv1", tgt)

	st := reg.Status("srv1")
	if st.Running {
		t.Fatalf("status Running true after failed probe, want false")
	}
	if len(st.Results) != 1 || st.Results[0].Error == "" {
		t.Fatalf("results = %+v, want one result carrying an error", st.Results)
	}
	if st.Results[0].ContextSize != 0 {
		t.Fatalf("results[0].ContextSize = %d, want 0 on a failed warm-load", st.Results[0].ContextSize)
	}
	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy true after a failed probe, want cleared (finish freed the server)")
	}
}

// TestStartContextProbeIdleGate exercises the two 409 paths of the HTTP handler: a server already
// reserved (TryStart fails, no Release of the pre-existing run) and a server with in-flight traffic
// (idle-gate refuses AND Releases the just-taken reservation).
func TestStartContextProbeIdleGate(t *testing.T) {
	s := newBenchmarkActiveFixture(t)
	// Seed an active mapping under the owned server so the mapping scope authorizes.
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	if err := s.Routes.CreateApplication(ctx, routing.Application{ID: "cp_app", ServerID: baOwnedServer, Type: routing.ProviderMock, Port: 8200, Scheme: "http", TimeoutMS: 30000, ContextProbePath: "/props", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "cp_map", ApplicationID: "cp_app", GatewayModelName: "gw-cp", AppModelName: "up-cp", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	post := func() int {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/cp_map/probe-context", nil)
		req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}

	// Case A: server already reserved => TryStart fails => 409, and the pre-existing
	// reservation must NOT be released (the handler does not Release on the TryStart-fail path).
	if _, ok := s.Benchmarks.TryStart(baOwnedServer, "server", "speed", 1, now, func() {}); !ok {
		t.Fatalf("pre-reserve TryStart failed")
	}
	if code := post(); code != http.StatusConflict {
		t.Fatalf("already-reserved: status = %d, want 409", code)
	}
	if !s.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatalf("pre-existing reservation was released on the TryStart-fail path, want it preserved")
	}
	s.Benchmarks.Release(baOwnedServer) // clear for the next case

	// Case B: server free but has an in-flight request => idle-gate refuses => 409, and the
	// just-taken reservation IS released (ServerBusy false afterward).
	s.Active.Add(ActiveRequest{ID: "inflight-1", ServerName: "Owned Host", ServerID: baOwnedServer})
	if code := post(); code != http.StatusConflict {
		t.Fatalf("in-use: status = %d, want 409", code)
	}
	if s.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatalf("reservation not released on the in-use path, want ServerBusy false")
	}
}

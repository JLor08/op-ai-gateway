// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestRunLoadModelHappyPath: a streaming provider that yields a token → runLoadModel finishes
// with results[0].Loaded == true, Running == false, and the server freed (ServerBusy false). No
// loaded-model registry is wired, so the reflection step is a no-op (must not panic).
func TestRunLoadModelHappyPath(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fake := &benchFakeProvider{usage: inference.Usage{OutputTokens: 20, TokensPerSecond: 55}}
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: fake, Benchmarks: reg}

	run, ok := reg.TryStart("srv1", "load", "load", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()

	srv.runLoadModel(ctx, run, "srv1", tgt)

	st := reg.Status("srv1")
	if st.Running {
		t.Fatalf("status Running true after load, want false (finish ran)")
	}
	if len(st.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(st.Results))
	}
	if !st.Results[0].Loaded {
		t.Fatalf("results[0].Loaded = false, want true (load confirmed)")
	}
	if st.Results[0].Error != "" {
		t.Fatalf("results[0].Error = %q, want empty", st.Results[0].Error)
	}
	if fake.calls != 1 {
		t.Fatalf("provider CompleteStream calls = %d, want 1 (a load stream)", fake.calls)
	}
	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy true after load, want cleared (finish freed the server)")
	}
}

// TestRunLoadModelAlreadyLoadedShortCircuits: when the loaded-model probe reports the model already
// resident, runLoadModel does NOT stream (no wasted load) but still reports Loaded == true.
func TestRunLoadModelAlreadyLoadedShortCircuits(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// coldLister reports "up-model" already loaded; LoadedModelsPath is set so modelResident probes.
	fake := newColdLister([]string{"up-model"})
	reg := NewBenchmarkRegistry()
	srv := &Server{Provider: fake, Benchmarks: reg}

	run, ok := reg.TryStart("srv1", "load", "load", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"

	srv.runLoadModel(ctx, run, "srv1", tgt)

	if got := fake.streamedModels(); len(got) != 0 {
		t.Fatalf("CompleteStream called %d time(s) (%v), want 0 (model already resident)", len(got), got)
	}
	st := reg.Status("srv1")
	if len(st.Results) != 1 || !st.Results[0].Loaded {
		t.Fatalf("results = %+v, want one result with Loaded=true", st.Results)
	}
	if reg.ServerBusy("srv1") {
		t.Fatalf("ServerBusy true after short-circuit load, want cleared")
	}
}

// TestRunLoadModelReflectsLoaded: after a real (non-short-circuit) load, reflectLoadedAfterLoad
// re-probes the app's loaded set and writes it to the gateway-poll registry, so LoadedAppModels
// immediately returns the model AND a subscriber is signalled.
func TestRunLoadModelReflectsLoaded(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	// swapEvicts=true: a stream of the target model loads it (initially empty => modelResident false).
	fake := newColdLister(nil)
	fake.swapEvicts = true
	reg := NewBenchmarkRegistry()
	loaded := NewLoadedModelRegistry()
	srv := &Server{Provider: fake, Benchmarks: reg, LoadedModels: loaded}

	ch, unsub := loaded.Subscribe()
	defer unsub()

	run, ok := reg.TryStart("srv1", "load", "load", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}
	tgt := benchTestTarget()
	tgt.app.LoadedModelsPath = "/loaded"

	srv.runLoadModel(ctx, run, "srv1", tgt)

	if got := fake.streamedModels(); len(got) != 1 || got[0] != "up-model" {
		t.Fatalf("streamed models = %v, want one load of up-model", got)
	}
	// The registry now reflects the loaded model for the app on that server.
	got := loaded.LoadedAppModels(tgt.app.ID, tgt.server.ID)
	if len(got) != 1 || got[0] != "up-model" {
		t.Fatalf("LoadedAppModels(%q,%q) = %v, want [up-model]", tgt.app.ID, tgt.server.ID, got)
	}
	// The change fanned out to the subscriber (buffered(1), already delivered synchronously).
	select {
	case <-ch:
	default:
		t.Fatalf("subscriber not signalled after reflectLoadedAfterLoad wrote the loaded set")
	}
	st := reg.Status("srv1")
	if len(st.Results) != 1 || !st.Results[0].Loaded {
		t.Fatalf("results = %+v, want one Loaded=true result", st.Results)
	}
}

// TestRunLoadModelHungUpstreamFrees: a stalled upstream (CompleteStream blocks, never emits)
// self-terminates via the idle watchdog and frees the server — a load action never permanently
// reserves a server behind a wedged upstream.
func TestRunLoadModelHungUpstreamFrees(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	srv := &Server{Provider: benchHangingProvider{}, Benchmarks: NewBenchmarkRegistry()}
	srv.streamIdleTimeout = 40 * time.Millisecond // watchdog fires fast

	run, ok := srv.Benchmarks.TryStart("srv1", "load", "load", 1, now, func() {})
	if !ok {
		t.Fatalf("TryStart did not start")
	}

	done := make(chan struct{})
	go func() {
		srv.runLoadModel(ctx, run, "srv1", benchTestTarget())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("runLoadModel did not return within 2s against a hung upstream (watchdog did not fire)")
	}

	if srv.Benchmarks.ServerBusy("srv1") {
		t.Fatalf("ServerBusy still true after a hung load — server-busy leaked (finish did not run)")
	}
	st := srv.Benchmarks.Status("srv1")
	if st.Running {
		t.Fatalf("status Running still true after a hung load")
	}
	if len(st.Results) != 1 || st.Results[0].Error == "" {
		t.Fatalf("expected one result carrying an error, got %+v", st.Results)
	}
	if st.Results[0].Loaded {
		t.Fatalf("results[0].Loaded = true after a failed load, want false")
	}
}

// TestStartLoadModel exercises the HTTP handler: 202 when idle, 409 (already_running) when the
// server is already reserved, 409 (server_in_use) when there is in-flight traffic, and 404 for
// an unknown/unauthorized mapping (no existence leak).
func TestStartLoadModel(t *testing.T) {
	s := newBenchmarkActiveFixture(t)
	// A real streaming provider so the launched background load is a clean, in-memory no-network run.
	s.Provider = &benchFakeProvider{usage: inference.Usage{OutputTokens: 5, TokensPerSecond: 10}}

	ctx := context.Background()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	if err := s.Routes.CreateApplication(ctx, routing.Application{ID: "ld_app", ServerID: baOwnedServer, Type: routing.ProviderMock, Port: 8300, Scheme: "http", TimeoutMS: 30000, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := s.Routes.CreateMapping(ctx, routing.ModelMapping{ID: "ld_map", ApplicationID: "ld_app", GatewayModelName: "gw-ld", AppModelName: "up-ld", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	post := func(mappingID string) int {
		req := httptest.NewRequest(http.MethodPost, "/api/portal/mappings/"+mappingID+"/load", nil)
		req.Header.Set("Authorization", "Bearer "+baOwnerSecret)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		return rec.Code
	}

	// Unknown mapping => AuthorizeBenchmarkScope errors => 404 (no leak).
	if code := post("does_not_exist"); code != http.StatusNotFound {
		t.Fatalf("unknown mapping: status = %d, want 404", code)
	}

	// Already reserved => TryStart fails => 409 already_running; the pre-existing reservation is
	// NOT released.
	if _, ok := s.Benchmarks.TryStart(baOwnedServer, "server", "speed", 1, now, func() {}); !ok {
		t.Fatalf("pre-reserve TryStart failed")
	}
	if code := post("ld_map"); code != http.StatusConflict {
		t.Fatalf("already-reserved: status = %d, want 409", code)
	}
	if !s.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatalf("pre-existing reservation released on the TryStart-fail path, want it preserved")
	}
	s.Benchmarks.Release(baOwnedServer)

	// In-flight traffic => idle-gate refuses => 409 server_in_use; the just-taken reservation IS
	// released.
	s.Active.Add(ActiveRequest{ID: "inflight-1", ServerName: "Owned Host", ServerID: baOwnedServer})
	if code := post("ld_map"); code != http.StatusConflict {
		t.Fatalf("in-use: status = %d, want 409", code)
	}
	if s.Benchmarks.ServerBusy(baOwnedServer) {
		t.Fatalf("reservation not released on the in-use path, want ServerBusy false")
	}
	s.Active.Remove("inflight-1")

	// Idle => 202 Accepted (the background load runs on the in-memory fake provider).
	if code := post("ld_map"); code != http.StatusAccepted {
		t.Fatalf("idle: status = %d, want 202", code)
	}
}

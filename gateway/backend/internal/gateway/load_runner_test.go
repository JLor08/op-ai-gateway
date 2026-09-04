// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// unavailableThenOKProvider returns provider.ErrUnavailable (a cold-load 503) on
// the first failFirst CompleteStream calls, then streams a token -- modelling a
// server that answers 503 while a large model is still loading.
type unavailableThenOKProvider struct {
	calls     int
	failFirst int
}

func (p *unavailableThenOKProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *unavailableThenOKProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	p.calls++
	if p.calls <= p.failFirst {
		// A 503 -> ErrUpstreamStarting, the one status the load retry waits on.
		return fmt.Errorf("%w: upstream status 503", provider.ErrUpstreamStarting)
	}
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "ok"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted})
}

// unavailable404Provider fails with a NON-503 ErrUnavailable (a bad model name,
// say) -- the same error class the swapper's 503 belongs to, but NOT retryable.
type unavailable404Provider struct{ calls int }

func (p *unavailable404Provider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *unavailable404Provider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, _ provider.StreamEmit) error {
	p.calls++
	return fmt.Errorf("%w: upstream status 404", provider.ErrUnavailable)
}

// plainErrProvider fails CompleteStream with a NON-unavailable error, to prove
// the cold-load retry does not swallow genuine failures.
type plainErrProvider struct{ calls int }

func (p *plainErrProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *plainErrProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, _ provider.StreamEmit) error {
	p.calls++
	return errors.New("boom: not an availability problem")
}

func shortenColdLoadRetry(t *testing.T, gap, budget time.Duration) {
	t.Helper()
	oldGap, oldBudget := coldLoadPollGap, coldLoadResidentMaxWait
	coldLoadPollGap, coldLoadResidentMaxWait = gap, budget
	t.Cleanup(func() { coldLoadPollGap, coldLoadResidentMaxWait = oldGap, oldBudget })
}

// TestEnsureResidentRetriesColdLoad503: a server that answers 503 three times
// while loading, then succeeds, must NOT fail the run -- the load is retried
// until it becomes servable.
func TestEnsureResidentRetriesColdLoad503(t *testing.T) {
	shortenColdLoadRetry(t, time.Millisecond, 2*time.Second)
	fake := &unavailableThenOKProvider{failFirst: 3}
	srv := &Server{Provider: fake}

	_, _, err := srv.ensureResidentForRun(context.Background(), benchTestTarget())
	if err != nil {
		t.Fatalf("ensureResidentForRun err = %v, want nil (retried past the cold-load 503)", err)
	}
	if fake.calls != 4 {
		t.Fatalf("CompleteStream calls = %d, want 4 (three 503s then a success)", fake.calls)
	}
}

// TestEnsureResidentGivesUpAfterColdLoadBudget: an upstream that never leaves
// 503 still fails -- but only after the budget, and only after it was retried.
func TestEnsureResidentGivesUpAfterColdLoadBudget(t *testing.T) {
	shortenColdLoadRetry(t, time.Millisecond, 40*time.Millisecond)
	fake := &unavailableThenOKProvider{failFirst: 1 << 30} // always 503
	srv := &Server{Provider: fake}

	_, _, err := srv.ensureResidentForRun(context.Background(), benchTestTarget())
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("ensureResidentForRun err = %v, want a wrapped provider.ErrUnavailable", err)
	}
	if fake.calls < 2 {
		t.Fatalf("CompleteStream calls = %d, want at least 2 (the load was retried before giving up)", fake.calls)
	}
}

// TestEnsureResidentDoesNotRetryGenuineError: a non-availability error is
// returned immediately, unretried, so a real failure is never masked as loading.
func TestEnsureResidentDoesNotRetryGenuineError(t *testing.T) {
	shortenColdLoadRetry(t, time.Millisecond, 2*time.Second)
	fake := &plainErrProvider{}
	srv := &Server{Provider: fake}

	_, _, err := srv.ensureResidentForRun(context.Background(), benchTestTarget())
	if err == nil {
		t.Fatalf("ensureResidentForRun err = nil, want the genuine error")
	}
	if errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("err = %v, want a NON-availability error passed through", err)
	}
	if fake.calls != 1 {
		t.Fatalf("CompleteStream calls = %d, want exactly 1 (no retry on a genuine error)", fake.calls)
	}
}

// TestEnsureResidentDoesNotRetryNon503Unavailable: a 404 (or any non-503) is
// ErrUnavailable but NOT ErrUpstreamStarting, so it fails fast -- otherwise a
// bad model name / crashed server would be retried for the whole budget, and a
// mid-load OOM crash would be re-driven into a loop.
func TestEnsureResidentDoesNotRetryNon503Unavailable(t *testing.T) {
	shortenColdLoadRetry(t, time.Millisecond, 2*time.Second)
	fake := &unavailable404Provider{}
	srv := &Server{Provider: fake}

	_, _, err := srv.ensureResidentForRun(context.Background(), benchTestTarget())
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("err = %v, want a wrapped provider.ErrUnavailable (the 404)", err)
	}
	if errors.Is(err, provider.ErrUpstreamStarting) {
		t.Fatalf("a 404 must NOT be tagged ErrUpstreamStarting: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("CompleteStream calls = %d, want exactly 1 (a non-503 unavailable is not retried)", fake.calls)
	}
}

// TestEnsureResidentReturnsCtxErrorOnCancel: a cancelled run returns ctx.Err(),
// not a stale 503, even mid-retry.
func TestEnsureResidentReturnsCtxErrorOnCancel(t *testing.T) {
	shortenColdLoadRetry(t, time.Millisecond, 2*time.Second)
	fake := &unavailableThenOKProvider{failFirst: 1 << 30} // always 503
	srv := &Server{Provider: fake}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := srv.ensureResidentForRun(ctx, benchTestTarget())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (a cancelled run must not report the stale 503)", err)
	}
}

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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// --- The benchmark path's live-progress verdict ----------------------------
//
// benchmarkTargetReq used to read the mapping's live_progress_support COLUMN.
// Migration 79 dropped it, and the tempting way to satisfy the compiler --
// delete the assignment -- would have frozen every benchmark stream's verdict
// at "" (never determined) forever, silently. Not a fail-closed default
// either: an upstream already detected as "unsupported" would then receive
// the live-progress parameters on every stream of every run, pay a 400 plus a
// retry for each, and never be able to memoize the rejection (the benchmark
// path sets RouteID "" deliberately, and an empty RouteID is never memoized).
// A capacity run is 4 levels x 16 concurrent = 64 streams.
//
// So the value comes from the mapping's capability ROW instead, resolved once
// per target. These tests fail if it is frozen: each seeds a verdict that is
// non-empty and DISTINCT from the zero value, so nothing can pass by
// accident.

// benchLiveProgressStore seeds srv1/app1/map1 in a MemoryStore and, when
// verdict is non-empty, gives map1 that live_progress capability row.
func benchLiveProgressStore(t *testing.T, verdict string) *routing.MemoryStore {
	t.Helper()
	ctx := context.Background()
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{
		ID: "srv1", Name: "Host", Domain: "host.example.test", Provider: routing.ProviderMock,
		Endpoint: "mock://srv1", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{
		ID: "app1", ServerID: "srv1", Type: routing.ProviderMock, Port: 8100, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, TimeoutMS: 30000,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{
		ID: "map1", ApplicationID: "app1", GatewayModelName: "gw-model", AppModelName: "up-model",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if verdict == "" {
		return mem
	}
	if err := mem.UpsertMappingCapabilities(ctx, "map1", []routing.CapabilityRow{{
		Capability: routing.CapabilityLiveProgress, Verdict: verdict,
		Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now,
	}}); err != nil {
		t.Fatalf("UpsertMappingCapabilities: %v", err)
	}
	return mem
}

// TestBenchmarkLiveProgressSupportReadsTheCapabilityRow: a stored "no" row
// must read back as "unsupported", the verdict whose loss costs a 400 plus a
// retry per stream. Asserted on the "no" direction specifically -- a
// three-state-to-bool-ish conversion that collapsed "no" into the
// zero/unknown value would be invisible on the "yes" case.
func TestBenchmarkLiveProgressSupportReadsTheCapabilityRow(t *testing.T) {
	for _, tc := range []struct {
		verdict string
		want    string
	}{
		{routing.CapabilityNo, "unsupported"},
		{routing.CapabilityYes, "supported"},
	} {
		t.Run(tc.verdict, func(t *testing.T) {
			srv := &Server{Routes: benchLiveProgressStore(t, tc.verdict)}
			if got := srv.benchmarkLiveProgressSupport(context.Background(), "map1"); got != tc.want {
				t.Fatalf("benchmarkLiveProgressSupport for a %q row = %q, want %q", tc.verdict, got, tc.want)
			}
		})
	}
}

// TestBenchmarkLiveProgressSupportNoRowIsUnknown: no row means never
// determined, which is "" -- the same reading an absent row produces
// everywhere else. A nil store is the same answer without a dereference.
func TestBenchmarkLiveProgressSupportNoRowIsUnknown(t *testing.T) {
	srv := &Server{Routes: benchLiveProgressStore(t, "")}
	if got := srv.benchmarkLiveProgressSupport(context.Background(), "map1"); got != "" {
		t.Fatalf("verdict for a mapping with no row = %q, want \"\" (never determined)", got)
	}
	nilStore := &Server{Routes: nil}
	if got := nilStore.benchmarkLiveProgressSupport(context.Background(), "map1"); got != "" {
		t.Fatalf("verdict with no store = %q, want \"\"", got)
	}
}

// capabilityReadErrorStore is a routing.Store whose MappingCapabilities
// always fails, so the degradation path can be exercised rather than merely
// reasoned about. Every other method is the embedded store's own.
type capabilityReadErrorStore struct {
	*routing.MemoryStore
}

func (capabilityReadErrorStore) MappingCapabilities(context.Context, string) ([]routing.CapabilityRow, error) {
	return nil, errors.New("capability read failed")
}

// TestBenchmarkLiveProgressSupportReadErrorDegradesToUnknown: a store error
// degrades to "" rather than propagating. A benchmark/probe/load/warm run
// must not fail over a request-parameter hint, and "" is the value an absent
// row would have produced anyway -- the same best-effort rule
// benchmarkSpecFor applies to a spec lookup.
func TestBenchmarkLiveProgressSupportReadErrorDegradesToUnknown(t *testing.T) {
	srv := &Server{Routes: capabilityReadErrorStore{benchLiveProgressStore(t, routing.CapabilityYes)}}
	if got := srv.benchmarkLiveProgressSupport(context.Background(), "map1"); got != "" {
		t.Fatalf("verdict after a read error = %q, want \"\" (best-effort degradation)", got)
	}
}

// TestBenchmarkTargetForFillsTheLiveProgressVerdict covers the CONSTRUCTION
// site, which is the half a unit test of the reader alone would miss: the
// four benchmark/probe/load/vram endpoint handlers and the scheduler all
// build their targets through benchmarkTargetFor, so a verdict resolved
// correctly but never attached would still leave every one of them at "".
// Routing every one of those sites through this single constructor is what
// makes one test enough.
func TestBenchmarkTargetForFillsTheLiveProgressVerdict(t *testing.T) {
	ctx := context.Background()
	mem := benchLiveProgressStore(t, routing.CapabilityNo)
	srv := &Server{Routes: mem}

	server, err := mem.AIServerByID(ctx, "srv1")
	if err != nil {
		t.Fatalf("AIServerByID: %v", err)
	}
	app, err := mem.ApplicationByID(ctx, "app1")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	mapping, err := mem.MappingByID(ctx, "map1")
	if err != nil {
		t.Fatalf("MappingByID: %v", err)
	}

	tgt := srv.benchmarkTargetFor(ctx, server, app, mapping)
	if tgt.liveProgressSupport != "unsupported" {
		t.Fatalf("benchmarkTargetFor liveProgressSupport = %q, want %q -- a verdict left at \"\" makes every benchmark stream pay a 400 plus a retry, forever", tgt.liveProgressSupport, "unsupported")
	}
	// And it reaches the Target the streams actually carry.
	target, _ := benchmarkTargetReq(tgt)
	if target.LiveProgressSupport != "unsupported" {
		t.Fatalf("built Target LiveProgressSupport = %q, want %q", target.LiveProgressSupport, "unsupported")
	}
}

// TestModelWarmerCarriesLiveProgressVerdictFromTheCandidate covers the SIXTH
// construction site, the model warmer -- the one that does not go through
// benchmarkTargetFor, because it already holds a routing.MappingCandidate
// whose LiveProgressSupport came out of ActiveMappingsForModel's join. This
// drives the real warm path end to end and asserts on the Target the
// provider was actually handed, so "took it from the candidate" is proven
// rather than assumed.
func TestModelWarmerCarriesLiveProgressVerdictFromTheCandidate(t *testing.T) {
	ctx := context.Background()
	mem := benchLiveProgressStore(t, routing.CapabilityNo)
	// The warmer resolves candidates by GATEWAY model name and skips a model
	// that is already resident, so the app needs no loaded-models path here
	// (an absent path makes the residency probe a no-op) and nothing is
	// seeded as loaded.
	fake := newColdLister(nil)
	srv := &Server{Provider: fake, Routes: mem}
	w := newModelWarmer(srv)

	w.Warm(ctx, "gw-model")
	waitWarmIdle(t, w, "gw-model")

	targets := fake.streamedTargetList()
	if len(targets) != 1 {
		t.Fatalf("streamed targets = %d, want exactly 1 (%+v)", len(targets), targets)
	}
	if targets[0].LiveProgressSupport != "unsupported" {
		t.Fatalf("warm stream LiveProgressSupport = %q, want %q -- the warmer must carry the candidate's joined verdict", targets[0].LiveProgressSupport, "unsupported")
	}
}

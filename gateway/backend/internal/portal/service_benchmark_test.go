// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestAuthorizeBenchmarkScope covers each scope's happy path (owner sees the
// expected views), the RBAC not-found collapse for a non-owner, the empty-scope
// ErrBenchmarkNoModels case, and an unknown scope.
func TestAuthorizeBenchmarkScope(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	svc, _ := newServerTestService(t, now)
	ctx := context.Background()

	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	// Two mappings: one active, one disabled. App/server scopes must keep only
	// the active one; the mapping scope benchmarks a single mapping regardless.
	activeMap, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "gw-active", AppModelName: "up-active", Status: routing.ServerStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateMapping active: %v", err)
	}
	disabledMap, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "gw-disabled", AppModelName: "up-disabled", Status: routing.ServerStatusDisabled,
	})
	if err != nil {
		t.Fatalf("CreateMapping disabled: %v", err)
	}

	// --- mapping scope: a single mapping regardless of status ---
	srv, views, err := svc.AuthorizeBenchmarkScope(ctx, ownerToken(), "mapping", disabledMap.ID)
	if err != nil {
		t.Fatalf("mapping scope (owner): %v", err)
	}
	if srv.ID != server.ID {
		t.Fatalf("mapping scope server = %q, want %q", srv.ID, server.ID)
	}
	if len(views) != 1 || views[0].Mapping.ID != disabledMap.ID {
		t.Fatalf("mapping scope views = %#v", views)
	}

	// --- application scope: only the ACTIVE mapping ---
	srv, views, err = svc.AuthorizeBenchmarkScope(ctx, ownerToken(), "application", app.ID)
	if err != nil {
		t.Fatalf("application scope (owner): %v", err)
	}
	if srv.ID != server.ID || len(views) != 1 || views[0].Mapping.ID != activeMap.ID {
		t.Fatalf("application scope views = %#v (server %q)", views, srv.ID)
	}

	// --- server scope: only the ACTIVE mapping across active apps ---
	srv, views, err = svc.AuthorizeBenchmarkScope(ctx, ownerToken(), "server", server.ID)
	if err != nil {
		t.Fatalf("server scope (owner): %v", err)
	}
	if srv.ID != server.ID || len(views) != 1 || views[0].Mapping.ID != activeMap.ID {
		t.Fatalf("server scope views = %#v (server %q)", views, srv.ID)
	}
	if views[0].App.ID != app.ID || views[0].Server.ID != server.ID {
		t.Fatalf("server scope view app/server = %#v", views[0])
	}

	// --- RBAC: a non-owner/non-admin gets the matching not-found sentinel ---
	if _, _, err := svc.AuthorizeBenchmarkScope(ctx, otherToken(), "mapping", activeMap.ID); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("mapping scope (other) err = %v, want ErrMappingNotFound", err)
	}
	if _, _, err := svc.AuthorizeBenchmarkScope(ctx, otherToken(), "application", app.ID); !errors.Is(err, ErrApplicationNotFound) {
		t.Fatalf("application scope (other) err = %v, want ErrApplicationNotFound", err)
	}
	if _, _, err := svc.AuthorizeBenchmarkScope(ctx, otherToken(), "server", server.ID); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("server scope (other) err = %v, want ErrServerNotFound", err)
	}

	// --- empty scope: an app with no active mappings -> ErrBenchmarkNoModels ---
	emptyApp, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8001, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication empty: %v", err)
	}
	if _, _, err := svc.AuthorizeBenchmarkScope(ctx, ownerToken(), "application", emptyApp.ID); !errors.Is(err, ErrBenchmarkNoModels) {
		t.Fatalf("empty application scope err = %v, want ErrBenchmarkNoModels", err)
	}

	// --- unknown scope ---
	if _, _, err := svc.AuthorizeBenchmarkScope(ctx, ownerToken(), "bogus", app.ID); !errors.Is(err, ErrBenchmarkScopeInvalid) {
		t.Fatalf("unknown scope err = %v, want ErrBenchmarkScopeInvalid", err)
	}
}

// TestMappingBenchmarks covers the per-mapping history read: the owner sees the
// runs newest-first, and a non-owner gets the not-found sentinel (no leak).
func TestMappingBenchmarks(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()

	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "gw", AppModelName: "up", Status: routing.ServerStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := routeStore.InsertBenchmarkRun(ctx, routing.BenchmarkRun{
			MappingID: mapping.ID, ServerID: server.ID,
			CreatedAt: now.Add(time.Duration(i) * time.Minute), GenTokensPerSecond: float64(20 + i),
		}); err != nil {
			t.Fatalf("InsertBenchmarkRun %d: %v", i, err)
		}
	}

	// Owner: sees both runs, newest-first (gen 21 leads gen 20).
	runs, err := svc.MappingBenchmarks(ctx, ownerToken(), mapping.ID, 50)
	if err != nil {
		t.Fatalf("MappingBenchmarks (owner): %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	if runs[0].GenTokensPerSecond != 21 || runs[0].MappingID != mapping.ID {
		t.Fatalf("newest run = %#v", runs[0])
	}

	// Non-owner: not-found sentinel (no existence leak).
	if _, err := svc.MappingBenchmarks(ctx, otherToken(), mapping.ID, 50); !errors.Is(err, ErrMappingNotFound) {
		t.Fatalf("MappingBenchmarks (other) err = %v, want ErrMappingNotFound", err)
	}
}

// TestMappingBenchmarksCapacityDecode covers the DTO layer's handling of the
// kind + decoded capacity report: a well-formed capacity curve decodes onto
// Capacity, a malformed curve tolerantly yields Capacity==nil (no error), and a
// plain row reads back Kind=="speed" with no capacity.
func TestMappingBenchmarksCapacityDecode(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()

	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "gw", AppModelName: "up", Status: routing.ServerStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	const goodCurve = `{"max_concurrency":8,"recommended_concurrency":4,"gen_tokens_per_second_at_capacity":42.5,"memory_observed":true,"levels":[{"concurrency":1,"per_request_tokens_per_second":50},{"concurrency":2,"per_request_tokens_per_second":40,"stop_reason":"latency"}]}`

	// Distinct ServerIDs identify each run in the returned slice; distinct
	// CreatedAt keeps the newest-first ordering deterministic.
	runs := []routing.BenchmarkRun{
		{MappingID: mapping.ID, ServerID: "srv-good", CreatedAt: now.Add(2 * time.Minute), Kind: "capacity", CapacityCurve: goodCurve},
		{MappingID: mapping.ID, ServerID: "srv-bad", CreatedAt: now.Add(1 * time.Minute), Kind: "capacity", CapacityCurve: "{not json"},
		{MappingID: mapping.ID, ServerID: "srv-speed", CreatedAt: now, GenTokensPerSecond: 20},
	}
	for i, r := range runs {
		if err := routeStore.InsertBenchmarkRun(ctx, r); err != nil {
			t.Fatalf("InsertBenchmarkRun %d: %v", i, err)
		}
	}

	got, err := svc.MappingBenchmarks(ctx, ownerToken(), mapping.ID, 50)
	if err != nil {
		t.Fatalf("MappingBenchmarks: %v", err)
	}
	byServer := make(map[string]BenchmarkRunDTO, len(got))
	for _, r := range got {
		byServer[r.ServerID] = r
	}
	if len(byServer) != 3 {
		t.Fatalf("expected 3 distinct runs, got %d (%#v)", len(byServer), got)
	}

	// Well-formed capacity row: decoded onto Capacity.
	good := byServer["srv-good"]
	if good.Kind != "capacity" {
		t.Fatalf("good.Kind = %q, want capacity", good.Kind)
	}
	if good.Capacity == nil {
		t.Fatalf("good.Capacity = nil, want decoded report")
	}
	if good.Capacity.MaxConcurrency != 8 || good.Capacity.RecommendedConcurrency != 4 {
		t.Fatalf("good.Capacity headline = %#v", good.Capacity)
	}
	if good.Capacity.GenTokensPerSecondAtCapacity != 42.5 || !good.Capacity.MemoryObserved {
		t.Fatalf("good.Capacity scalars = %#v", good.Capacity)
	}
	if len(good.Capacity.Levels) != 2 {
		t.Fatalf("good.Capacity.Levels = %#v", good.Capacity.Levels)
	}
	if good.Capacity.Levels[1].StopReason != "latency" {
		t.Fatalf("good.Capacity.Levels[1].StopReason = %q, want latency", good.Capacity.Levels[1].StopReason)
	}

	// Malformed capacity row: tolerant -> Kind=="capacity", Capacity==nil, no error.
	bad := byServer["srv-bad"]
	if bad.Kind != "capacity" {
		t.Fatalf("bad.Kind = %q, want capacity", bad.Kind)
	}
	if bad.Capacity != nil {
		t.Fatalf("bad.Capacity = %#v, want nil (malformed curve)", bad.Capacity)
	}

	// Plain speed row: empty stored kind -> "speed", no capacity.
	speed := byServer["srv-speed"]
	if speed.Kind != "speed" {
		t.Fatalf("speed.Kind = %q, want speed", speed.Kind)
	}
	if speed.Capacity != nil {
		t.Fatalf("speed.Capacity = %#v, want nil", speed.Capacity)
	}
}

// TestMappingBenchmarksVRAMDecode covers the DTO layer's handling of the
// kind=="vram" history row: a well-formed per-GPU payload decodes onto VRAM, a
// malformed one tolerantly yields VRAM==nil (no error, mirroring the capacity
// curve), the SAME payload stored on a row of any other kind is NOT decoded
// (the column is read for one kind only), and a decoded report never serves a
// JSON-null gpus array.
func TestMappingBenchmarksVRAMDecode(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newServerTestService(t, now)
	ctx := context.Background()

	server := createTestServer(t, svc, "S", "s.example.test")
	app, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{
		GatewayModelName: "gw", AppModelName: "up", Status: routing.ServerStatusActive,
	})
	if err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	const goodPayload = `{"isolated":true,"isolation_evidence":{"spec1":"stopped_after_write"},` +
		`"drained_spec_ids":["spec1"],"restore_failed":["spec2"],` +
		`"gpus":[{"index":1,"fingerprint":"GPU-abc","fingerprint_kind":"uuid","baseline_used_mb":700,` +
		`"delta_mb":22000,"measured_mb":21900,"attributable":true},` +
		`{"index":2,"fingerprint":"Radeon Pro W7900","fingerprint_kind":"name_total","unified_memory":true,` +
		`"baseline_used_mb":100,"delta_mb":900}]}`

	// Distinct ServerIDs identify each run in the returned slice; distinct
	// CreatedAt keeps the newest-first ordering deterministic.
	runs := []routing.BenchmarkRun{
		{MappingID: mapping.ID, ServerID: "srv-good", CreatedAt: now.Add(4 * time.Minute), Kind: "vram", VRAMJSON: goodPayload},
		{MappingID: mapping.ID, ServerID: "srv-bad", CreatedAt: now.Add(3 * time.Minute), Kind: "vram", VRAMJSON: "{not json"},
		{MappingID: mapping.ID, ServerID: "srv-inconclusive", CreatedAt: now.Add(2 * time.Minute), Kind: "vram", VRAMJSON: `{"inconclusive":"below_floor"}`},
		{MappingID: mapping.ID, ServerID: "srv-otherkind", CreatedAt: now.Add(time.Minute), Kind: "capacity", VRAMJSON: goodPayload},
		{MappingID: mapping.ID, ServerID: "srv-speed", CreatedAt: now, GenTokensPerSecond: 20},
	}
	for i, r := range runs {
		if err := routeStore.InsertBenchmarkRun(ctx, r); err != nil {
			t.Fatalf("InsertBenchmarkRun %d: %v", i, err)
		}
	}

	got, err := svc.MappingBenchmarks(ctx, ownerToken(), mapping.ID, 50)
	if err != nil {
		t.Fatalf("MappingBenchmarks: %v", err)
	}
	byServer := make(map[string]BenchmarkRunDTO, len(got))
	for _, r := range got {
		byServer[r.ServerID] = r
	}
	if len(byServer) != 5 {
		t.Fatalf("expected 5 distinct runs, got %d (%#v)", len(byServer), got)
	}

	// Well-formed VRAM row: decoded onto VRAM, every field carried.
	good := byServer["srv-good"]
	if good.Kind != "vram" {
		t.Fatalf("good.Kind = %q, want vram", good.Kind)
	}
	if good.VRAM == nil {
		t.Fatal("good.VRAM = nil, want a decoded report")
	}
	if !good.VRAM.Isolated || good.VRAM.Inconclusive != "" {
		t.Fatalf("good.VRAM headline = %#v", good.VRAM)
	}
	if good.VRAM.IsolationEvidence["spec1"] != "stopped_after_write" {
		t.Fatalf("good.VRAM.IsolationEvidence = %#v", good.VRAM.IsolationEvidence)
	}
	if len(good.VRAM.DrainedSpecIDs) != 1 || good.VRAM.DrainedSpecIDs[0] != "spec1" {
		t.Fatalf("good.VRAM.DrainedSpecIDs = %#v", good.VRAM.DrainedSpecIDs)
	}
	if len(good.VRAM.RestoreFailed) != 1 || good.VRAM.RestoreFailed[0] != "spec2" {
		t.Fatalf("good.VRAM.RestoreFailed = %#v", good.VRAM.RestoreFailed)
	}
	if len(good.VRAM.GPUs) != 2 {
		t.Fatalf("good.VRAM.GPUs = %#v", good.VRAM.GPUs)
	}
	first := good.VRAM.GPUs[0]
	if first.Index != 1 || first.Fingerprint != "GPU-abc" || first.FingerprintKind != "uuid" ||
		first.BaselineUsedMB != 700 || first.DeltaMB != 22000 || first.MeasuredMB != 21900 || !first.Attributable {
		t.Fatalf("good.VRAM.GPUs[0] = %#v", first)
	}
	second := good.VRAM.GPUs[1]
	if second.Index != 2 || second.FingerprintKind != "name_total" || !second.UnifiedMemory ||
		second.DeltaMB != 900 || second.MeasuredMB != 0 || second.Attributable {
		t.Fatalf("good.VRAM.GPUs[1] = %#v", second)
	}

	// Malformed payload: tolerant -> Kind=="vram", VRAM==nil, no error.
	bad := byServer["srv-bad"]
	if bad.Kind != "vram" || bad.VRAM != nil {
		t.Fatalf("bad row = %#v, want kind=vram with VRAM nil (malformed payload)", bad)
	}

	// An inconclusive row decodes, carries its reason, and serves gpus as an
	// empty array rather than a JSON null.
	inconclusive := byServer["srv-inconclusive"]
	if inconclusive.VRAM == nil {
		t.Fatal("inconclusive.VRAM = nil, want a decoded report carrying the reason")
	}
	if inconclusive.VRAM.Inconclusive != "below_floor" {
		t.Fatalf("inconclusive.VRAM.Inconclusive = %q, want below_floor", inconclusive.VRAM.Inconclusive)
	}
	if inconclusive.VRAM.GPUs == nil {
		t.Fatal("inconclusive.VRAM.GPUs = nil, want a non-nil empty slice (a nil marshals as JSON null)")
	}

	// The payload column is read for kind=="vram" ONLY: the same bytes on a
	// capacity row are not decoded, and nothing there claims a VRAM result.
	otherKind := byServer["srv-otherkind"]
	if otherKind.Kind != "capacity" {
		t.Fatalf("otherKind.Kind = %q, want capacity", otherKind.Kind)
	}
	if otherKind.VRAM != nil {
		t.Fatalf("otherKind.VRAM = %#v, want nil (the column is decoded for kind==vram only)", otherKind.VRAM)
	}

	// Plain speed row: no VRAM report at all.
	if speed := byServer["srv-speed"]; speed.Kind != "speed" || speed.VRAM != nil {
		t.Fatalf("speed row = %#v, want kind=speed with VRAM nil", speed)
	}
}

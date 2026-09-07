// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// --- Task 13 (P4a) end-to-end: telemetry ingest actually steers routing ---
//
// Every other test in this package exercises ingest and routing in isolation:
// the ingest tests above assert what lands in the store/registries, and
// internal/routing's resolver tests assert scoring given an already-primed
// RuntimeModelStateChecker fake. Neither proves the two halves are actually
// WIRED TOGETHER on a real *Server -- that a telemetry sample arriving through
// ingestTelemetrySample changes what that SAME server's live resolver
// (Server.Resolver, built with newRuntimeModelStateChecker over
// Server.RuntimeStatus in New -- see server.go) decides for a real Resolve
// call. These two tests close that gap.
//
// idleModelRoutingServerID is a second server offering the SAME gateway
// model ("qwen-coder") as the "mock-host-comp" fixture seedGatewayTestRoutes
// already wires up (application app_mock_comp / mapping route_mock_qwen,
// AppModelName "qwen-coder", latency 100ms). This second server is seeded
// with markedly worse per-server telemetry (latency 900ms), the same
// asymmetry (and the same scoring constants) TestResolverPerModelMetricsOverridePerServerTelemetry
// in internal/routing/resolver_runtime_state_test.go uses for srv_fast (100ms)
// vs srv_slow (900ms): raw scores 1230 vs 1070, so absent any live per-model
// signal "mock-host-comp" wins outright, and a reported active=10/queue=5 on
// its OWN "qwen-coder" runtime drops its merged score to 880 -- below the
// idle server's untouched 1070 -- flipping the winner.
const idleModelRoutingServerID = "mock-host-comp-idle"

// seedIdleModelRoutingServer creates idleModelRoutingServerID with an
// application + mapping offering the SAME gateway model as "mock-host-comp"
// ("qwen-coder") but a distinct AppModelName (so it never coincidentally
// matches a runtime-status entry meant for "mock-host-comp"), and telemetry
// deliberately worse (900ms vs 100ms) than "mock-host-comp"'s so the baseline
// (no live per-model signal) resolve favors "mock-host-comp".
//
// It also REFRESHES "mock-host-comp"'s own telemetry to "now" (real time):
// seedGatewayTestRoutes stamps it with a fixed 2026-07-10 ReportedAt, and
// Server.Resolver runs on the real wall clock (routing.NewResolver's default
// when New passes a nil clock -- see server.go), so left alone that seed
// telemetry is always far older than Score's 2-minute staleness window and
// would eat a flat -500 staleTelemetryPenalty regardless of anything this
// test ingests, masking the very scoring asymmetry it exists to exercise.
func seedIdleModelRoutingServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := srv.Routes.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: "mock-host-comp", ReportedAt: now, LatencyMS: 100, ErrorRate: 0,
		ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("refresh mock-host-comp telemetry: %v", err)
	}
	if err := srv.Routes.CreateAIServer(ctx, routing.AIServer{
		ID: idleModelRoutingServerID, Name: idleModelRoutingServerID, Domain: idleModelRoutingServerID + ".example.test",
		Provider: routing.ProviderMock, Endpoint: "mock://" + idleModelRoutingServerID,
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create idle server: %v", err)
	}
	if err := srv.Routes.CreateApplication(ctx, routing.Application{
		ID: "app_" + idleModelRoutingServerID, ServerID: idleModelRoutingServerID, Type: routing.ProviderMock, Port: 8100, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800,
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create idle application: %v", err)
	}
	if err := srv.Routes.CreateMapping(ctx, routing.ModelMapping{
		ID: "map_" + idleModelRoutingServerID, ApplicationID: "app_" + idleModelRoutingServerID,
		GatewayModelName: "qwen-coder", AppModelName: "qwen-coder-idle",
		Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create idle mapping: %v", err)
	}
	if err := srv.Routes.UpsertTelemetry(ctx, routing.ServerTelemetry{
		ServerID: idleModelRoutingServerID, ReportedAt: now, LatencyMS: 900, ErrorRate: 0,
		ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert idle telemetry: %v", err)
	}
}

// TestIngestTelemetrySampleSteersRoutingDecision is the positive half: a
// live telemetry sample declaring runtime_model_probe and reporting
// "mock-host-comp"'s own "qwen-coder" runtime heavily loaded (active=10,
// queue=5) must flip an ACTUAL Resolve call on the SAME server away from
// "mock-host-comp" (the normally-better server) to the idle one -- proving
// the ingested per-model load reaches the resolver's scoring, not merely the
// store or a registry nobody reads.
func TestIngestTelemetrySampleSteersRoutingDecision(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	seedIdleModelRoutingServer(t, srv)

	// Empty token bypasses affinity (Resolver.Resolve only reads/writes
	// affinity when token.ID != ""), so repeated resolves below exercise
	// plain scoring each time rather than a cached prior pick.
	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	baseline, err := srv.Resolver.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve (baseline): %v", err)
	}
	if baseline.ServerID != "mock-host-comp" {
		t.Fatalf("baseline target.ServerID = %q, want mock-host-comp (better raw score, no live per-model signal yet)", baseline.ServerID)
	}

	// latency_ms is repeated explicitly (matching the 100ms the baseline just
	// scored on) so this sample's own per-server telemetry write does not
	// itself perturb the raw score -- the ONLY thing changing between the
	// baseline and post-ingest resolves is the live per-model active/queue
	// overlay.
	body := `{"host":{"cpu_util_pct":1},"latency_ms":100,"capabilities":{"features":["runtime_model_probe"]},` +
		`"runtimes":[{"spec_id":"probe_comp","model":"qwen-coder","state":"running","active_requests":10,"queue_depth":5}]}`
	ireq, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-comp", ireq, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	after, err := srv.Resolver.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve (after ingest): %v", err)
	}
	if after.ServerID != idleModelRoutingServerID {
		t.Fatalf("target.ServerID = %q after ingesting mock-host-comp's live overload, want %s -- the ingested per-model load must steer routing to the idle server", after.ServerID, idleModelRoutingServerID)
	}
}

// TestIngestTelemetrySampleWithoutProbeCapabilityDoesNotSteerRouting is the
// contrast: the SAME reported active/queue for the SAME runtime, but the
// sample does NOT declare runtime_model_probe. The runtime-status registry is
// still populated unconditionally (ingestTelemetrySample publishes it
// regardless of capability), but runtimeModelStateChecker's metricsOK is
// gated on the reporting agent's declared feature set
// (Server.AgentFeatures), so a fabricated/unreliable active=10/queue=5 from a
// non-probing agent must NOT be scored -- proving the routing-side gate this
// package's earlier fix added, end to end, not just at the
// resolver-with-a-fake level internal/routing already covers.
func TestIngestTelemetrySampleWithoutProbeCapabilityDoesNotSteerRouting(t *testing.T) {
	srv := NewTestServer()
	ctx := context.Background()
	seedIdleModelRoutingServer(t, srv)

	req := inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"}

	baseline, err := srv.Resolver.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve (baseline): %v", err)
	}
	if baseline.ServerID != "mock-host-comp" {
		t.Fatalf("baseline target.ServerID = %q, want mock-host-comp", baseline.ServerID)
	}

	// Deliberately the SAME active_requests/queue_depth (and latency_ms) as
	// the steering test above, but with no "capabilities" object at all -- no
	// runtime_model_probe declared for this sample.
	body := `{"host":{"cpu_util_pct":1},"latency_ms":100,` +
		`"runtimes":[{"spec_id":"probe_comp_noprobe","model":"qwen-coder","state":"running","active_requests":10,"queue_depth":5}]}`
	ireq, raw := ingestReq(t, body)
	if err := srv.ingestTelemetrySample(ctx, "mock-host-comp", ireq, raw); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	// The registry still recorded the report (publish is unconditional) --
	// what must NOT happen is the resolver trusting its metrics.
	snap, _, unsub := srv.RuntimeStatus.subscribe("mock-host-comp")
	unsub()
	if len(snap) != 1 || snap[0].ActiveRequests != 10 || snap[0].QueueDepth != 5 {
		t.Fatalf("runtime-status snapshot = %#v, want the report recorded regardless of capability (metricsOK gates the RESOLVER, not the registry)", snap)
	}

	after, err := srv.Resolver.Resolve(ctx, auth.Token{}, req)
	if err != nil {
		t.Fatalf("Resolve (after ingest, no probe capability): %v", err)
	}
	if after.ServerID != "mock-host-comp" {
		t.Fatalf("target.ServerID = %q after a non-probing agent reported its model overloaded, want mock-host-comp unchanged -- a fabricated/unreliable metric from an agent that never declared runtime_model_probe must not steer routing", after.ServerID)
	}
}

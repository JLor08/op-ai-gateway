// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// TestRunAppHealthOnceAgentPresencePropagatesToAvailabilitySample is the
// cross-cutting integration test for the availability-history feature's headline
// "was the ServerAgent reporting" capability. It proves the agent-presence path
// propagates end-to-end against a REAL store: the SAME *gateway.AgentPresenceRegistry
// that the agent-ingest handler stamps via Report(serverID) is passed to the
// health loop, and the availability sample the loop writes carries
// AgentReporting == true.
//
// Per-task tests exercised the sampling logic with a NIL presence registry
// (-> AgentReporting always false); none drove the true path with a real store,
// so a regression where the loop stopped reading the shared registry would have
// gone unnoticed. This closes that gap.
//
// Mutation-proof: the committed form calls Report on the shared registry passed
// to runAppHealthOnce. If Report is NOT called (or a different / nil registry is
// passed) Reporting(serverID) is false, so the sample's AgentReporting is false
// and the final assertion fails — confirming the test genuinely depends on the
// shared-instance, agent-reported path (demonstrated during development).
func TestRunAppHealthOnceAgentPresencePropagatesToAvailabilitySample(t *testing.T) {
	shrinkRetryGap(t)
	ctx := context.Background()
	// A fixed clock so the written sample's ReportedAt is deterministic and the
	// read window below straddles it. (The AgentPresenceRegistry keeps its own
	// real internal clock; Report -> run happens within the freshness window, so
	// Reporting is true regardless of this loop clock.)
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return base }

	// A REAL store (not the fake health store) so the sample round-trips through
	// InsertServerAvailabilitySample -> ServerAvailabilitySamples exactly as in
	// production. Seed one active server + one active, reachable application + a
	// mapping so deriveServerHealth yields "healthy" (all active apps reachable).
	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "s1", Domain: "s1.test",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := mem.CreateApplication(ctx, routing.Application{
		ID: "a1", ServerID: "s1", Type: routing.ProviderMock, Port: 8000, Scheme: "http",
		APIFlavors: []string{routing.APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000,
		Status: routing.ServerStatusActive, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := mem.CreateMapping(ctx, routing.ModelMapping{
		ID: "m1", ApplicationID: "a1", GatewayModelName: "gpt", AppModelName: "up",
		Status: routing.ServerStatusActive, CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}

	prober := newFakeProber() // the seeded app is reachable (endpoint not marked down)
	// Settings source: netbird_only absent -> OFF, default cadence (the memory store
	// is not a healthSettings source; mirror the netbird routing test's split).
	settings := &fakeHealthStore{settings: map[string]string{}}

	// The SAME AgentPresenceRegistry the agent-ingest handler writes and the health
	// loop reads. Report(s1) simulates a fresh ServerAgent telemetry POST.
	agentPresence := gateway.NewAgentPresenceRegistry(180 * time.Second)
	agentPresence.Report("s1")

	reg := gateway.NewAppHealthRegistry(nil)
	lastProbed := map[string]time.Time{}
	lastAvail := map[string]availWriteState{}

	// Run one cycle passing the SAME agentPresence instance and the real store.
	(&appHealthRunner{store: mem, prober: prober, syncer: nil, registry: reg, loaded: nil, agents: agentPresence, groups: nil, settings: settings, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(ctx, &cycleState{lastProbed: lastProbed, lastAvail: lastAvail})

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	samples, err := mem.ServerAvailabilitySamples(ctx, "s1", from, to, 20000)
	if err != nil {
		t.Fatalf("ServerAvailabilitySamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("availability samples = %d, want 1 (one write on the first observation)", len(samples))
	}
	got := samples[0]
	if got.Health != routing.HealthHealthy {
		t.Fatalf("sample Health = %q, want %q (one active reachable app)", got.Health, routing.HealthHealthy)
	}
	if !got.AgentReporting {
		t.Fatalf("sample AgentReporting = false, want true (the shared AgentPresenceRegistry stamped via Report must propagate to the written sample)")
	}
}

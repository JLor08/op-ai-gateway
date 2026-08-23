// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/gateway"
	"op-ai-gateway/internal/routing"
	"os"
	"strings"
	"testing"
	"time"
)

// The cert-report registry has to be BOUNDED, and the only place that knows which
// servers are still live is this loop's end-of-cycle Retain. This drives the real
// runAppHealthOnce against a real store with a real bundle and proves the wiring:
// a deleted server's report is evicted while a live one survives.
//
// Mutation-proof: dropping `a.certReports.Retain(live)` from agentRegistries.Retain
// (or passing a bundle without the registry) leaves the stale entry and fails here.
func TestRunAppHealthOnceRetainsCertReportsForLiveServersOnly(t *testing.T) {
	shrinkRetryGap(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return base }

	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "s1", Domain: "s1.test",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	certReports := gateway.NewAgentCertReportRegistry()
	const fp = "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666aaaa7777bbbb8888"
	certReports.Report("s1", gateway.AgentCertReport{Fingerprint: fp, Mode: "files"})
	certReports.Report("deleted-server", gateway.AgentCertReport{Fingerprint: fp, Mode: "files"})

	agents := agentRegistries{
		presence:    gateway.NewAgentPresenceRegistry(180 * time.Second),
		certReports: certReports,
	}
	settings := &fakeHealthStore{settings: map[string]string{}}

	(&appHealthRunner{store: mem, prober: newFakeProber(), syncer: nil, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: agents, groups: nil, settings: settings, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: map[string]availWriteState{}})

	if _, ok := certReports.Get("s1"); !ok {
		t.Fatal("the live server's cert report was evicted")
	}
	if _, ok := certReports.Get("deleted-server"); ok {
		t.Fatal("a deleted server's cert report survived the cycle -- Retain is not wired for the cert-report registry")
	}
}

// A nil bundle (what most tests pass) must stay a no-op rather than a nil-interface
// panic, so the accessors keep the pre-existing nil-safe behaviour of the registry
// pointers they replaced.
func TestAgentRegistryBundleNilIsANoOp(t *testing.T) {
	if reportingWithin(nil, "s1", time.Minute) {
		t.Fatal("a nil bundle reported presence")
	}
	retainAgents(nil, map[string]struct{}{}) // must not panic
}

// A partially-wired bundle must degrade, not panic: both fields are nil-safe pointers.
func TestAgentRegistriesPartiallyWiredIsSafe(t *testing.T) {
	var agents agentRegistryBundle = agentRegistries{}
	if reportingWithin(agents, "s1", time.Minute) {
		t.Fatal("an empty bundle reported presence")
	}
	retainAgents(agents, map[string]struct{}{}) // must not panic
}

// The registry is a per-process singleton with THREE consumers (the agent-ingest
// path via ServerDeps, the portal read side via ServiceDeps, and the app-health
// loop's Retain) and must be wired in ALL THREE driver paths -- postgres is the
// production default, so a gap there is invisible in every local run.
//
// Since CMP-1, memoryDeps/sqliteDeps/postgresDeps share ONE body
// (buildRuntime, reached directly by memoryDeps and via sqlDeps by
// sqliteDeps/postgresDeps) instead of each inlining this wiring separately,
// so there is now exactly one portal.NewService call site rather than three;
// each driver's call chain into that shared body is pinned at the bottom.
func TestAllThreeDriversWireAgentCertReports(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	drivers := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if drivers != 1 {
		t.Fatalf("found %d portal.NewService call sites, expected 1 (buildRuntime, shared by memory/sqlite/postgres)", drivers)
	}
	checks := []struct {
		what    string
		snippet string
		why     string
	}{
		{
			"constructed", "agentCertReports := gateway.NewAgentCertReportRegistry()",
			"an unwired driver path has no registry at all",
		},
		{
			"given to the portal", "AgentCertReports: agentCertReports,",
			"the certificate list + the CA-rotation brake would read nothing",
		},
		{
			"given to the gateway server", "AgentCertReports:                agentCertReports,",
			"agent reports would be dropped on the floor",
		},
		{
			"given to the app-health loop", "certReports: agentCertReports,",
			"the registry would grow unbounded (Retain never reaches it)",
		},
	}
	for _, c := range checks {
		if got := strings.Count(body, c.snippet); got != drivers {
			t.Fatalf("the cert-report registry is %s in %d of %d driver paths -- %s", c.what, got, drivers, c.why)
		}
	}
	assertAllDriversReachBuildRuntime(t)
}

// TestAllThreeDriversShareAndRetainAgentTransportRegistry mirrors the
// cert-report contract for the transport registry (T7). One instance is
// constructed, given to the portal (read side), the gateway server (write
// side inside authenticateAgent) and the app-health loop (Retain) in every
// driver path; a gap in any of them silently breaks the corresponding
// consumer.
func TestAllThreeDriversShareAndRetainAgentTransportRegistry(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	drivers := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if drivers != 1 {
		t.Fatalf("found %d portal.NewService call sites, expected 1 (buildRuntime, shared by memory/sqlite/postgres)", drivers)
	}
	checks := []struct {
		what    string
		snippet string
		why     string
	}{
		{
			"constructed", "agentTransport := gateway.NewAgentTransportRegistry()",
			"an unwired driver path has no transport registry at all",
		},
		{
			"given to the portal", "AgentTransport: agentTransport,",
			"the certificate list transport column + the mesh gate arming read nothing",
		},
		{
			"given to the gateway server", "AgentTransport:                  agentTransport,",
			"authenticateAgent's mesh-listener stamp is dropped on the floor",
		},
		{
			"given to the app-health loop", "transport: agentTransport",
			"the transport registry would grow unbounded (Retain never reaches it)",
		},
	}
	for _, c := range checks {
		if got := strings.Count(body, c.snippet); got != drivers {
			t.Fatalf("the transport registry is %s in %d of %d driver paths -- %s", c.what, got, drivers, c.why)
		}
	}
	assertAllDriversReachBuildRuntime(t)
}

// TestRunAppHealthOnceRetainsTransportForLiveServersOnly proves that the shared
// registry survives a real app-health cycle: a live server keeps its observation,
// a deleted server loses it. If the app_health.go Retain fan-out ever drops
// transport, this fails.
func TestRunAppHealthOnceRetainsTransportForLiveServersOnly(t *testing.T) {
	shrinkRetryGap(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return base }

	mem := routing.NewMemoryStore()
	if err := mem.CreateAIServer(ctx, routing.AIServer{
		ID: "s1", Name: "s1", Domain: "s1.test",
		Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
		CreatedAt: base, UpdatedAt: base,
	}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}

	transport := gateway.NewAgentTransportRegistry()
	transport.Report("s1", true)
	transport.Report("deleted-server", false)

	agents := agentRegistries{
		presence:    gateway.NewAgentPresenceRegistry(180 * time.Second),
		certReports: gateway.NewAgentCertReportRegistry(),
		transport:   transport,
	}
	settings := &fakeHealthStore{settings: map[string]string{}}

	(&appHealthRunner{store: mem, prober: newFakeProber(), syncer: nil, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: agents, groups: nil, settings: settings, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: map[string]availWriteState{}})

	if _, _, ok := transport.LatestTransport("s1"); !ok {
		t.Fatal("the live server's transport observation was evicted")
	}
	if _, _, ok := transport.LatestTransport("deleted-server"); ok {
		t.Fatal("a deleted server's transport observation survived the cycle -- Retain is not wired for the transport registry")
	}
}

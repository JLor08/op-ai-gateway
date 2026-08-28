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

// The runtime-status registry (agent-runtime-manager Task 9) has to be
// BOUNDED like every other per-server ServerAgent registry, and the only
// place that knows which servers are still live is this loop's end-of-cycle
// Retain. This drives the real runOnce against a real store with a real
// bundle and proves the wiring: a deleted server's file-mode flag is evicted
// while a live one survives -- mirrors
// TestRunAppHealthOnceRetainsCertReportsForLiveServersOnly exactly.
//
// Mutation-proof: dropping the `if a.runtimeStatus != nil { a.runtimeStatus.
// Retain(live) }` branch from agentRegistries.Retain (or passing a bundle
// without the registry) leaves the stale entry and fails here.
func TestRunAppHealthOnceRetainsRuntimeStatusForLiveServersOnly(t *testing.T) {
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

	runtimeStatus := gateway.NewRuntimeStatusRegistry()
	runtimeStatus.SetFileMode("s1", true)
	runtimeStatus.SetFileMode("deleted-server", true)

	agents := agentRegistries{
		presence:      gateway.NewAgentPresenceRegistry(180 * time.Second),
		runtimeStatus: runtimeStatus,
	}
	settings := &fakeHealthStore{settings: map[string]string{}}

	(&appHealthRunner{store: mem, prober: newFakeProber(), syncer: nil, registry: gateway.NewAppHealthRegistry(nil), loaded: nil, agents: agents, groups: nil, settings: settings, probeTimeout: time.Second, cipher: nil, now: clock}).runOnce(ctx, &cycleState{lastProbed: map[string]time.Time{}, lastAvail: map[string]availWriteState{}})

	if !runtimeStatus.IsFileMode("s1") {
		t.Fatal("the live server's file-mode flag was evicted")
	}
	if runtimeStatus.IsFileMode("deleted-server") {
		t.Fatal("a deleted server's file-mode flag survived the cycle -- Retain is not wired for the runtime-status registry")
	}
}

// A bundle with NO runtimeStatus set (the interface field's zero value, nil)
// must stay a no-op through the nil check, not panic -- unlike a nil POINTER
// on the sibling fields, a nil INTERFACE cannot forward a call to a
// nil-safe receiver.
func TestAgentRegistriesRetainNilRuntimeStatusIsSafe(t *testing.T) {
	agents := agentRegistries{presence: gateway.NewAgentPresenceRegistry(time.Minute)}
	agents.Retain(map[string]struct{}{"s1": {}}) // must not panic
}

// TestAllThreeDriversWireRuntimeStatus mirrors
// TestAllThreeDriversWireAgentCertReports's source-scan shape: the registry
// is a per-process singleton with TWO consumers (ServerDeps.RuntimeStatus,
// the write/read side the agent-ingest path and the portal SSE handler
// share, and the app-health loop's agentRegistries bundle) and must be the
// SAME instance in both, or the SSE-visible state would silently diverge
// from the pruned state.
func TestAllThreeDriversWireRuntimeStatus(t *testing.T) {
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
			"constructed", "runtimeStatus := gateway.NewRuntimeStatusRegistry()",
			"an unwired driver path has no registry at all",
		},
		{
			"given to the gateway server", "RuntimeStatus:                   runtimeStatus,",
			"the SSE stream and PushRuntimeConfig's file-mode gate would see nothing",
		},
		{
			"given to the app-health loop", "runtimeStatus: runtimeStatus",
			"the registry would grow unbounded (Retain never reaches it)",
		},
	}
	for _, c := range checks {
		if got := strings.Count(body, c.snippet); got != drivers {
			t.Fatalf("the runtime-status registry is %s in %d of %d driver paths -- %s", c.what, got, drivers, c.why)
		}
	}
}

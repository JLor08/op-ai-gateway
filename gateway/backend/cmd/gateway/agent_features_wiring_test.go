// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/config"
	"testing"
	"time"
)

// The agent-features registry has to be BOUNDED to the live server set like
// every other per-server ServerAgent registry, and the only place that knows
// which servers are still live is the app-health loop's end-of-cycle Retain.
// A correct Retain method is NOT enough on its own: gateway.New silently
// default-constructs its own instance when ServerDeps.AgentFeatures is nil,
// so a missed wiring line would leave production pruning an instance nothing
// ever writes to while the Server's own instance grew forever (exactly the
// trap NewRuntimeStatusRegistry's doc comment describes).
//
// This is therefore a SAME-INSTANCE test, not a Retain test: it drives the
// real production wiring (buildGatewayServer -> memoryDeps -> buildRuntime),
// captures the agentRegistries bundle the loop was actually started with via
// the startAppHealthLoop seam, WRITES through the registry the *gateway.Server
// holds, and PRUNES through the bundle. The write can only become invisible
// if both sides hold the one same object.
//
// Mutation-proof, each of these independently fails it: dropping
// `AgentFeatures: agentFeatures` from buildRuntime's ServerDeps (the Server
// would default-construct a different instance), dropping `agentFeatures:
// agentFeatures` from the agentRegistries literal, dropping the
// `a.agentFeatures.Retain(live)` fan-out in agentRegistries.Retain, or
// dropping the Retain method body.
func TestBuildGatewayServerPrunesTheSameAgentFeaturesRegistryTheServerWritesTo(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "dev-secret")

	// Capture the bundle production hands the loop. startAppHealthLoop is
	// called synchronously from buildRuntime, so no synchronization is needed.
	var bundle agentRegistryBundle
	orig := startAppHealthLoop
	startAppHealthLoop = func(runner *appHealthRunner, serverTrigger <-chan string) context.CancelFunc {
		bundle = runner.agents
		return func() {}
	}
	t.Cleanup(func() { startAppHealthLoop = orig })

	srv, cleanup, err := buildGatewayServer(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "memory", AppHealthProbeTimeout: time.Second,
	})
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	if bundle == nil {
		t.Fatal("the app-health loop was started with no agent-registry bundle")
	}
	if srv.AgentFeatures == nil {
		t.Fatal("the gateway Server carries no agent-features registry")
	}

	// The write side: exactly what ingestTelemetrySample does on the Server.
	srv.AgentFeatures.Set("live-server", []string{"runtime_manager"})
	srv.AgentFeatures.Set("deleted-server", []string{"runtime_manager"})

	// The prune side: exactly what runOnce does at the end of a full cycle.
	bundle.Retain(map[string]struct{}{"live-server": {}})

	if !srv.AgentFeatures.Has("live-server", "runtime_manager") {
		t.Fatal("a LIVE server's declared features were evicted by the end-of-cycle prune")
	}
	if srv.AgentFeatures.Has("deleted-server", "runtime_manager") {
		t.Fatal("a deleted server's declared features survived the end-of-cycle prune -- " +
			"the registry cmd/gateway prunes is not the instance the gateway Server writes to")
	}
}

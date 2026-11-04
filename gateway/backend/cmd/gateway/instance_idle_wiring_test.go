// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"op-ai-gateway/internal/config"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// The idle timeout pushed for a replicable runtime spec is derived under two
// operator settings (config.InstanceIdleFactor and InstanceIdleFloorSeconds,
// OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR / _FLOOR_SECONDS). portal.NewService
// defaults a ServiceDeps value it was never handed, so a missed wiring line in
// buildRuntime would leave every operator's setting silently ignored: the
// document would keep pushing the defaults and nothing would fail. So this
// drives the real production deps builder (memoryDeps -> buildRuntime, the
// call every DB driver ends in) and reads the configured derivation back out
// of the runtime-config document the agent fetches. The derivation itself is
// pinned in the portal package (TestEffectiveIdleTimeout), and so is the
// agent-feature gate (TestAgentRuntimeDocInstanceFieldsOnlyForAnAgentThatRunsInstances).
func TestBuildRuntimeWiresTheInstanceIdleSettings(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	// The app-health loop prunes the agent-features registry to the servers it
	// last listed, which can be before this test created its server; a cycle
	// landing between the Set below and the read would drop the declaration.
	// The loop is not what this test is about, so it does not run.
	orig := startAppHealthLoop
	startAppHealthLoop = func(*appHealthRunner, <-chan string) context.CancelFunc { return func() {} }
	t.Cleanup(func() { startAppHealthLoop = orig })
	cases := []struct {
		name   string
		factor float64
		floor  int
		want   int
	}{
		// startup 20 s: factor 2 gives 40 s, above the floor 30.
		{"configured factor and floor", 2, 30, 40},
		// The same spec under factor 1 and floor 90 is held at the floor.
		{"a configured floor above the derived value", 1, 90, 90},
		// Unset, as config.Load never leaves them: the portal defaults 5 / 120.
		{"unset values fall back to the defaults", 0, 0, 120},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps, cleanup, err := memoryDeps(config.Config{
				Addr: "127.0.0.1:8080", DBDriver: "memory",
				InstanceIdleFactor: tc.factor, InstanceIdleFloorSeconds: tc.floor,
			})
			if err != nil {
				t.Fatalf("memoryDeps returned %v", err)
			}
			t.Cleanup(func() { _ = cleanup() })
			ctx := context.Background()
			now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
			const serverID, appID, mappingID, specID = "srv_instances", "app_instances", "map_instances", "rspec_instances"
			if err := deps.Routes.CreateAIServer(ctx, routing.AIServer{
				ID: serverID, Name: serverID, Domain: serverID + ".example.test", Provider: routing.ProviderMock,
				Endpoint: "mock://" + serverID, Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy,
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create server: %v", err)
			}
			if err := deps.Routes.CreateApplication(ctx, routing.Application{
				ID: appID, ServerID: serverID, Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http",
				APIFlavors: []string{routing.APIFlavorOpenAI}, Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create application: %v", err)
			}
			if err := deps.Routes.CreateMapping(ctx, routing.ModelMapping{
				ID: mappingID, ApplicationID: appID, GatewayModelName: "tts", AppModelName: "tts",
				Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("create mapping: %v", err)
			}
			if err := deps.Routes.UpsertRuntimeSpec(ctx, routing.RuntimeSpec{
				ID: specID, MappingID: mappingID, Enabled: true, Binary: "/usr/local/bin/llama-server",
				Args: "[]", Env: "{}", Replicable: true, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatalf("upsert spec: %v", err)
			}
			if err := deps.Routes.UpdateRuntimeSpecStartupMeasured(ctx, specID, 20000); err != nil {
				t.Fatalf("seed measured startup: %v", err)
			}
			// The derived value goes only to an agent that runs instances, as
			// recorded by the ingest in the registry the portal reads -- which
			// also pins that buildRuntime hands the portal that same registry.
			deps.AgentFeatures.Set(serverID, []string{"runtime_manager", portal.AgentFeatureRuntimeInstances})

			doc, err := deps.Portal.AgentRuntimeConfig(ctx, serverID)
			if err != nil {
				t.Fatalf("AgentRuntimeConfig: %v", err)
			}
			if len(doc.Specs) != 1 {
				t.Fatalf("document specs = %d, want 1", len(doc.Specs))
			}
			if got := doc.Specs[0].IdleTimeoutSeconds; got != tc.want {
				t.Fatalf("pushed idle_timeout_seconds = %d, want %d (factor %v, floor %d, startup 20 s)", got, tc.want, tc.factor, tc.floor)
			}
		})
	}
}

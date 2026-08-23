// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"testing"
	"time"
)

// TestServerOverrideNoOpWhenUnset confirms the no-op invariant: req.ServerOverrideID == ""
// resolves EXACTLY as today (reusing the TestResolverRoutesToMappingBackedTarget fixture +
// assertions) — the force-branch guard at the top of Resolve must never fire.
func TestServerOverrideNoOpWhenUnset(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("Resolve returned %v", err)
	}
	if target.ServerID != "srv_fast" || target.RouteID != "map_fast" || target.Provider != ProviderMock || target.ProviderModel != "qwen2.5" {
		t.Fatalf("target = %#v, want the unmodified srv_fast/map_fast target", target)
	}
}

// TestServerOverrideForcesToServerBypassingProvisioningAndAffinity is the core proof: two
// servers offer qwen-coder; a ProvisioningGate blocks srv_slow, and a route affinity is
// pinned to srv_fast. A req.ServerOverrideID = srv_slow request must STILL land on
// srv_slow — proving the force-branch bypasses BOTH the provisioning gate and affinity
// (neither filterProvisioned nor resolveAffinity is consulted by resolveServerOverride).
//
// Mutation check: routing the override through the normal candidate/affinity/provisioning
// path (instead of the short-circuit at the top of Resolve) would either return the
// srv_fast pin or ErrNoModelRoute (gate blocks srv_slow) — this test fails either way.
func TestServerOverrideForcesToServerBypassingProvisioningAndAffinity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)
	// The gate blocks srv_slow (server B) entirely; only srv_fast (A) is provisioned.
	resolver.SetProvisioningGate(fakeGate{allow: map[string]bool{"srv_fast": true}})
	token := auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true}

	// Establish an affinity pin to srv_fast (A) via a normal (non-override) resolve; the
	// gate allows srv_fast so this succeeds and pins the session there.
	pinned, err := resolver.Resolve(ctx, token, inference.Request{Model: "qwen-coder", APIFlavor: "openai_chat"})
	if err != nil {
		t.Fatalf("priming Resolve (establish the A pin): %v", err)
	}
	if pinned.ServerID != "srv_fast" {
		t.Fatalf("priming Resolve landed on %q, want srv_fast (the pin this test needs)", pinned.ServerID)
	}

	// Now force to srv_slow (B) — the gate still blocks it, and the token still carries
	// the srv_fast pin. The override must win over both.
	target, err := resolver.Resolve(ctx, token, inference.Request{
		Model:            "qwen-coder",
		APIFlavor:        "openai_chat",
		ServerOverrideID: "srv_slow",
	})
	if err != nil {
		t.Fatalf("Resolve with ServerOverrideID=srv_slow returned %v, want a served target", err)
	}
	if target.ServerID != "srv_slow" {
		t.Fatalf("target.ServerID = %q, want srv_slow (forced, despite the provisioning-gate block AND the srv_fast affinity pin)", target.ServerID)
	}
	if target.RouteID != "map_slow" {
		t.Fatalf("target.RouteID = %q, want map_slow", target.RouteID)
	}
}

// maintenanceOverrideStore seeds a single server/app/mapping offering qwen-coder with the
// given server status + health status, mirroring singleCapCandidateStore.
func maintenanceOverrideStore(t *testing.T, now time.Time, status, health string) *MemoryStore {
	t.Helper()
	ctx := context.Background()
	store := NewMemoryStore()
	if err := store.CreateAIServer(ctx, AIServer{ID: "srv_over", Name: "over", Domain: "over.test", Status: status, HealthStatus: health, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := store.CreateApplication(ctx, Application{ID: "app_over", ServerID: "srv_over", Type: ProviderMock, Port: 8000, Scheme: "http", APIFlavors: []string{APIFlavorOpenAI}, Priority: 10, Weight: 50, TimeoutMS: 30000, AffinityTTLSeconds: 1800, Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := store.CreateMapping(ctx, ModelMapping{ID: "map_over", ApplicationID: "app_over", GatewayModelName: "qwen-coder", AppModelName: "qwen2.5", Status: ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if err := store.UpsertTelemetry(ctx, ServerTelemetry{ServerID: "srv_over", ReportedAt: now, LatencyMS: 100, ErrorRate: 0, ProviderHealth: "{}", Capabilities: "{}", RawSummary: "{}", UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return store
}

// TestServerOverrideAllowsMaintenanceServer: a server in ServerStatusMaintenance is a
// candidate serverSelectable would EXCLUDE (it requires ServerStatusActive) — the override
// must still route to it, proving the maintenance-status exclusion is bypassed.
//
// Mutation check: calling serverSelectable (or equivalently gating on
// server.Status == ServerStatusActive) inside resolveServerOverride, instead of the
// disabled-only + health-only checks, makes this test fail.
func TestServerOverrideAllowsMaintenanceServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := maintenanceOverrideStore(t, now, ServerStatusMaintenance, HealthHealthy)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
		Model:            "qwen-coder",
		APIFlavor:        "openai_chat",
		ServerOverrideID: "srv_over",
	})
	if err != nil {
		t.Fatalf("Resolve returned %v, want the maintenance server to be routed via override", err)
	}
	if target.ServerID != "srv_over" || target.RouteID != "map_over" {
		t.Fatalf("target = %#v, want srv_over/map_over", target)
	}
}

// TestServerOverrideRejectsDisabledServer: a disabled server is refused even via the
// override — ErrServerOverrideServerUnavailable, never routed.
func TestServerOverrideRejectsDisabledServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	store := maintenanceOverrideStore(t, now, ServerStatusDisabled, HealthHealthy)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
		Model:            "qwen-coder",
		APIFlavor:        "openai_chat",
		ServerOverrideID: "srv_over",
	})
	if !errors.Is(err, ErrServerOverrideServerUnavailable) {
		t.Fatalf("Resolve err = %v, want ErrServerOverrideServerUnavailable", err)
	}
}

// TestServerOverrideUnhealthyGatedByForce drives BOTH reachability signals
// resolveServerOverride consults — server.HealthStatus and the ReachabilityChecker — and
// proves ServerOverrideForceUnreachable is what lets each one through.
func TestServerOverrideUnhealthyGatedByForce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

	t.Run("unhealthy server", func(t *testing.T) {
		store := maintenanceOverrideStore(t, now, ServerStatusActive, HealthUnhealthy)
		resolver := NewResolver(store, func() time.Time { return now }, nil)

		_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
			Model:            "qwen-coder",
			APIFlavor:        "openai_chat",
			ServerOverrideID: "srv_over",
		})
		if !errors.Is(err, ErrServerOverrideServerUnavailable) {
			t.Fatalf("unforced Resolve err = %v, want ErrServerOverrideServerUnavailable", err)
		}

		target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
			Model:                          "qwen-coder",
			APIFlavor:                      "openai_chat",
			ServerOverrideID:               "srv_over",
			ServerOverrideForceUnreachable: true,
		})
		if err != nil {
			t.Fatalf("forced Resolve returned %v, want the unhealthy server to be routed", err)
		}
		if target.ServerID != "srv_over" {
			t.Fatalf("target.ServerID = %q, want srv_over", target.ServerID)
		}
	})

	t.Run("unreachable application (checker-driven)", func(t *testing.T) {
		store := maintenanceOverrideStore(t, now, ServerStatusActive, HealthHealthy)
		checker := fakeReachability{unreachable: map[string]bool{"app_over": true}}
		resolver := NewResolver(store, func() time.Time { return now }, checker)

		_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
			Model:            "qwen-coder",
			APIFlavor:        "openai_chat",
			ServerOverrideID: "srv_over",
		})
		if !errors.Is(err, ErrServerOverrideServerUnavailable) {
			t.Fatalf("unforced Resolve err = %v, want ErrServerOverrideServerUnavailable", err)
		}

		target, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
			Model:                          "qwen-coder",
			APIFlavor:                      "openai_chat",
			ServerOverrideID:               "srv_over",
			ServerOverrideForceUnreachable: true,
		})
		if err != nil {
			t.Fatalf("forced Resolve returned %v, want the unreachable-app server to be routed", err)
		}
		if target.ServerID != "srv_over" {
			t.Fatalf("target.ServerID = %q, want srv_over", target.ServerID)
		}
	})
}

// TestServerOverrideModelNotOnServer: the requested model has no live mapping on the named
// server (it doesn't exist in the store at all here) -> ErrServerOverrideModelUnavailable,
// never ErrServerOverrideServerUnavailable (a distinct failure mode: "the server can't be
// used" vs "the server doesn't serve this model").
func TestServerOverrideModelNotOnServer(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	// qwen-coder IS offered by srv_fast/srv_slow in this store, but NOT by srv_missing.
	store := seededResolverStore(t, now)
	resolver := NewResolver(store, func() time.Time { return now }, nil)

	_, err := resolver.Resolve(ctx, auth.Token{}, inference.Request{
		Model:            "qwen-coder",
		APIFlavor:        "openai_chat",
		ServerOverrideID: "srv_missing",
	})
	if !errors.Is(err, ErrServerOverrideModelUnavailable) {
		t.Fatalf("Resolve err = %v, want ErrServerOverrideModelUnavailable", err)
	}
}

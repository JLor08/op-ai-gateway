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

// TestAuthorizeRuntimeActionReturnsTheSpecAndItsScope: the owner of the
// mapping's server gets the mapping, its application and server, and the
// stored spec -- what the gateway's start and stop act on.
func TestAuthorizeRuntimeActionReturnsTheSpecAndItsScope(t *testing.T) {
	svc, _, serverID, spec := benchmarkSpecFixture(t)
	scope, err := svc.AuthorizeRuntimeAction(context.Background(), ownerToken(), spec.MappingID)
	if err != nil {
		t.Fatalf("AuthorizeRuntimeAction: %v", err)
	}
	if scope.Server.ID != serverID || scope.Mapping.ID != spec.MappingID || scope.Application.ID != scope.Mapping.ApplicationID {
		t.Fatalf("scope = server %q mapping %q app %q, want server %q mapping %q", scope.Server.ID, scope.Mapping.ID, scope.Application.ID, serverID, spec.MappingID)
	}
	if scope.Spec.ID != spec.ID || scope.Spec.StartupTimeoutSeconds != 240 {
		t.Fatalf("spec = %q (startup %d), want %q (startup 240)", scope.Spec.ID, scope.Spec.StartupTimeoutSeconds, spec.ID)
	}
}

// TestAuthorizeRuntimeActionRefusals pins the refusals in their order:
// a caller with no claim to the mapping learns nothing (404-no-leak, before
// the benchmark gate), a benchmark run holding the server refuses like the
// spec PUT, and a mapping without a spec has nothing to start or stop.
func TestAuthorizeRuntimeActionRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("stranger", func(t *testing.T) {
		svc, _, serverID, spec := benchmarkSpecFixture(t)
		svc.SetBenchmarkReservationHook(func(id string) bool { return id == serverID })
		if _, err := svc.AuthorizeRuntimeAction(ctx, otherToken(), spec.MappingID); !errors.Is(err, ErrMappingNotFound) {
			t.Fatalf("err = %v, want ErrMappingNotFound", err)
		}
	})
	t.Run("benchmark holds the server", func(t *testing.T) {
		svc, _, serverID, spec := benchmarkSpecFixture(t)
		svc.SetBenchmarkReservationHook(func(id string) bool { return id == serverID })
		if _, err := svc.AuthorizeRuntimeAction(ctx, ownerToken(), spec.MappingID); !errors.Is(err, ErrRuntimeSpecServerBenchmarking) {
			t.Fatalf("err = %v, want ErrRuntimeSpecServerBenchmarking", err)
		}
	})
	t.Run("no spec", func(t *testing.T) {
		now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app := seedServerAgentApplication(t, routeStore, server.ID, now)
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "bare", AppModelName: "bare"})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if _, err := svc.AuthorizeRuntimeAction(ctx, ownerToken(), mapping.ID); !errors.Is(err, ErrRuntimeSpecNotFound) {
			t.Fatalf("err = %v, want ErrRuntimeSpecNotFound", err)
		}
	})
	t.Run("not an agent application", func(t *testing.T) {
		now := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app := seedServerAgentApplication(t, routeStore, server.ID, now)
		app.Type = routing.ProviderVLLM
		if err := routeStore.UpdateApplication(ctx, app); err != nil {
			t.Fatalf("UpdateApplication: %v", err)
		}
		mapping, err := svc.CreateMapping(ctx, ownerToken(), app.ID, CreateMappingRequest{GatewayModelName: "plain", AppModelName: "plain"})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}
		if _, err := svc.AuthorizeRuntimeAction(ctx, ownerToken(), mapping.ID); !errors.Is(err, ErrRuntimeSpecNotServerAgent) {
			t.Fatalf("err = %v, want ErrRuntimeSpecNotServerAgent", err)
		}
	})
}

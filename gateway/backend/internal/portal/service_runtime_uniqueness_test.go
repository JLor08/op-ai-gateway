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

// TestCreateApplicationSecondServerAgentRejected pins the "at most one
// server_agent application per AI server" invariant on the create path.
func TestCreateApplicationSecondServerAgentRejected(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http",
	}); err != nil {
		t.Fatalf("first server_agent create: %v, want success", err)
	}
	// A DIFFERENT port, so the pre-existing unique(server_id, port) constraint
	// cannot be what rejects this -- the new invariant must.
	_, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9001, Scheme: "http",
	})
	if !errors.Is(err, ErrServerAgentApplicationExists) {
		t.Fatalf("second server_agent create: err = %v, want ErrServerAgentApplicationExists", err)
	}

	// A non-server_agent application on the same server is unaffected.
	if _, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	}); err != nil {
		t.Fatalf("vllm create alongside a server_agent application: %v, want success", err)
	}

	// A server_agent application on a DIFFERENT server is unaffected (the
	// invariant is per-server, not global).
	other := createTestServer(t, svc, "S2", "s2.example.test")
	if _, err := svc.CreateApplication(ctx, ownerToken(), other.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http",
	}); err != nil {
		t.Fatalf("server_agent create on a second server: %v, want success", err)
	}
}

// TestUpdateApplicationRetypeToServerAgentRejected pins the same invariant on
// the update path: retyping an existing non-server_agent application to
// server_agent is the easy way past a create-only gate.
func TestUpdateApplicationRetypeToServerAgentRejected(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	seedServerAgentApplication(t, routeStore, server.ID, now)

	plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication(vllm): %v", err)
	}
	retype := routing.ProviderServerAgent
	if _, err := svc.UpdateApplication(ctx, ownerToken(), plain.ID, UpdateApplicationRequest{Type: &retype}); !errors.Is(err, ErrServerAgentApplicationExists) {
		t.Fatalf("retype to server_agent: err = %v, want ErrServerAgentApplicationExists", err)
	}
	// The rejected update must not have been persisted.
	reloaded, err := svc.GetApplication(ctx, ownerToken(), plain.ID)
	if err != nil {
		t.Fatalf("GetApplication: %v", err)
	}
	if reloaded.Type != routing.ProviderVLLM {
		t.Fatalf("type after a rejected retype = %q, want vllm", reloaded.Type)
	}
}

// TestUpdateApplicationServerAgentSelfRetypeAllowed guards the obvious
// self-collision regression: the gate must exclude the application being
// updated, so a no-op "type = server_agent" PATCH on the server's OWN
// server_agent application still succeeds, as does any unrelated field edit
// on it.
func TestUpdateApplicationServerAgentSelfRetypeAllowed(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	app := seedServerAgentApplication(t, routeStore, server.ID, now)

	same := routing.ProviderServerAgent
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Type: &same}); err != nil {
		t.Fatalf("re-setting the same type on the server's own server_agent app: %v, want success", err)
	}
	port := 9100
	if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Port: &port}); err != nil {
		t.Fatalf("port edit on a server_agent app (no type in the request): %v, want success", err)
	}
}

// TestAgentRuntimeConfigDeterminismRestsOnUniqueness is the pin the A1 brief
// asks for: AgentRuntimeConfig takes the FIRST server_agent application it
// finds and breaks, which is only well-defined because at most one can
// exist. The test drives that dependency explicitly -- it creates the one
// allowed server_agent application, proves a second one is refused, and then
// shows the derived document is the one belonging to that single
// application. If the uniqueness gate were removed, a second application
// could be created and this assertion would become order-dependent (ids are
// random hex and the store orders by id).
func TestAgentRuntimeConfigDeterminismRestsOnUniqueness(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _ := newServerTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	first, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http",
	})
	if err != nil {
		t.Fatalf("CreateApplication(server_agent): %v", err)
	}
	if _, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9001, Scheme: "http",
	}); !errors.Is(err, ErrServerAgentApplicationExists) {
		t.Fatalf("second server_agent create: err = %v, want ErrServerAgentApplicationExists", err)
	}
	cfg, err := svc.AgentRuntimeConfig(ctx, server.ID)
	if err != nil {
		t.Fatalf("AgentRuntimeConfig: %v", err)
	}
	if cfg.RouterListen != first.Port {
		t.Fatalf("cfg.RouterListen = %d, want %d (the single server_agent application's port)", cfg.RouterListen, first.Port)
	}
}

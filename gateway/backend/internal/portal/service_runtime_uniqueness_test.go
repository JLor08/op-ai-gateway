// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"sync/atomic"
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

// raceWindowRoutingStore reproduces the TOCTOU window
// serverAgentApplicationExistsOnServer's own doc comment describes: the
// service gate reads, releases, and only then writes, so a concurrent writer
// can land a server_agent application in between. Reads hide the server's
// existing server_agent applications until a write is attempted -- exactly
// what the LOSING request of that race observes -- while the store underneath
// keeps enforcing the invariant, so the write fails with store.ErrConflict.
//
// One shape covers every driver: memory rejects the write through
// MemoryStore.serverAgentApplicationExistsLocked, sqlite/postgres through
// migration 68's partial unique index, and all three surface the identical
// opaque store.ErrConflict -- which is why the portal's classification of it
// cannot depend on the driver, and why testing it over the memory store is
// not a memory-only assertion.
type raceWindowRoutingStore struct {
	routing.Store
	hideServerAgent atomic.Bool
}

func (r *raceWindowRoutingStore) ApplicationsByServer(ctx context.Context, serverID string) ([]routing.Application, error) {
	apps, err := r.Store.ApplicationsByServer(ctx, serverID)
	if err != nil || !r.hideServerAgent.Load() {
		return apps, err
	}
	visible := make([]routing.Application, 0, len(apps))
	for _, app := range apps {
		if app.Type != routing.ProviderServerAgent {
			visible = append(visible, app)
		}
	}
	return visible, nil
}

// CreateApplication and UpdateApplication close the window: by the time the
// losing request reaches the store, the racing writer has committed, so every
// read after that point -- the classification read included -- sees the truth.
func (r *raceWindowRoutingStore) CreateApplication(ctx context.Context, app routing.Application) error {
	r.hideServerAgent.Store(false)
	return r.Store.CreateApplication(ctx, app)
}

func (r *raceWindowRoutingStore) UpdateApplication(ctx context.Context, app routing.Application) error {
	r.hideServerAgent.Store(false)
	return r.Store.UpdateApplication(ctx, app)
}

// openRaceWindow swaps svc's routing store for one that hides the server's
// existing server_agent applications from the service-level gate, and returns
// the wrapper so a test can re-open the window.
func openRaceWindow(svc *Service, routeStore routing.Store) *raceWindowRoutingStore {
	race := &raceWindowRoutingStore{Store: routeStore}
	race.hideServerAgent.Store(true)
	svc.routes = race
	return race
}

// TestServerAgentWriteConflictReportsTheHonestCode is M7: when the invariant
// is enforced by the STORE rather than by the service gate -- the race the
// gate cannot close, on any of the three drivers -- the portal must still
// name the condition that actually holds. Before this was classified, both
// paths answered ErrApplicationConflict ("application.port_conflict":
// "application port already in use") on a request where no port collided.
//
// The third subtest is the guard against over-classifying: when the request
// really does collide on a port, the port code must still win, even though
// the server_agent condition holds as well.
func TestServerAgentWriteConflictReportsTheHonestCode(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		seedServerAgentApplication(t, routeStore, server.ID, now)
		openRaceWindow(svc, routeStore)

		// Port 9001, while the hidden server_agent application holds 9000: no
		// port collides, so a port_conflict answer here is simply false.
		_, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderServerAgent, Port: 9001, Scheme: "http",
		})
		if !errors.Is(err, ErrServerAgentApplicationExists) {
			t.Fatalf("create that lost the race: err = %v, want ErrServerAgentApplicationExists", err)
		}
	})

	t.Run("retype", func(t *testing.T) {
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		seedServerAgentApplication(t, routeStore, server.ID, now)
		plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("CreateApplication(vllm): %v", err)
		}
		openRaceWindow(svc, routeStore)

		retype := routing.ProviderServerAgent
		if _, err := svc.UpdateApplication(ctx, ownerToken(), plain.ID, UpdateApplicationRequest{Type: &retype}); !errors.Is(err, ErrServerAgentApplicationExists) {
			t.Fatalf("retype that lost the race: err = %v, want ErrServerAgentApplicationExists", err)
		}
	})

	t.Run("a real port collision still reports the port", func(t *testing.T) {
		svc, routeStore := newServerTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		agent := seedServerAgentApplication(t, routeStore, server.ID, now)
		openRaceWindow(svc, routeStore)

		_, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderServerAgent, Port: agent.Port, Scheme: "http",
		})
		if !errors.Is(err, ErrApplicationConflict) {
			t.Fatalf("create colliding on port %d: err = %v, want ErrApplicationConflict", agent.Port, err)
		}
	})
}

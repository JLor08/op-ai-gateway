// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sync"
	"testing"
	"time"
)

// newRuntimeNotifyTestService is newServerTestService plus a recording
// runtime-config-changed hook, and a snapshot accessor for what it recorded.
// The three pre-existing hook tests in service_runtime_test.go each build
// their own directory/store/service inline to pass OnRuntimeConfigChanged
// through ServiceDeps; SetRuntimeConfigChangedHook (the same field, exposed
// as a setter for the gateway's construction order) lets these tests reuse
// the standard fixture instead of copying that block four more times.
//
// The mutex is not ceremony: the hook's production implementation
// (gateway.Server.PushRuntimeConfig) is documented as "synchronous but
// guaranteed fast" and is called from the write path, so recording under a
// lock keeps the test honest under -race even though nothing here is
// concurrent today.
func newRuntimeNotifyTestService(t *testing.T, now time.Time) (*Service, *routing.MemoryStore, func() []string) {
	t.Helper()
	svc, routeStore := newServerTestService(t, now)
	var mu sync.Mutex
	var calls []string
	svc.SetRuntimeConfigChangedHook(func(serverID string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, serverID)
	})
	return svc, routeStore, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// TestCreateApplicationServerAgentFiresRuntimeChangedHook is the case the e2e
// runtime suite tripped over: AgentRuntimeConfig derives router_listen from
// the server's server_agent application, so creating that application changes
// the document the agent must act on -- and before this, nothing told the
// agent, leaving the freshly created application reading unhealthy (its router
// unbound) until the agent's 60 s poll backstop.
func TestCreateApplicationServerAgentFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, _, calls := newRuntimeNotifyTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")

	if _, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderServerAgent, Port: 9000, Scheme: "http",
	}); err != nil {
		t.Fatalf("CreateApplication(server_agent): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
		t.Fatalf("calls after creating the server_agent application = %#v, want exactly [%q]", got, server.ID)
	}

	// The gate is on the type, not on "an application was written": a plain
	// upstream application is no part of the runtime-config document, so it
	// must not push one.
	if _, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	}); err != nil {
		t.Fatalf("CreateApplication(vllm): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
		t.Fatalf("calls after also creating a vllm application = %#v, want still exactly [%q]", got, server.ID)
	}
}

// TestUpdateApplicationServerAgentFiresRuntimeChangedHook covers the two
// cases that make a naive "if the incoming type is server_agent" gate wrong,
// plus the two negatives that keep the gate from degenerating into "notify on
// every application write".
func TestUpdateApplicationServerAgentFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("retype to server_agent", func(t *testing.T) {
		svc, _, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("CreateApplication(vllm): %v", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after creating the vllm application = %#v, want none", got)
		}

		retype := routing.ProviderServerAgent
		if _, err := svc.UpdateApplication(ctx, ownerToken(), plain.ID, UpdateApplicationRequest{Type: &retype}); err != nil {
			t.Fatalf("retype to server_agent: %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after retyping to server_agent = %#v, want exactly [%q]", got, server.ID)
		}
	})

	// The case a currentType-only gate misses. The agent owns a router and a
	// set of managed processes derived from THIS application; once it is no
	// longer a server_agent application the agent's desired state is the empty
	// document, and it can only learn that if the write that emptied it
	// notified.
	t.Run("retype away from server_agent", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		// Seeded at the store layer so the create itself does not fire the
		// hook -- the assertion below is then about the UPDATE alone.
		app := seedServerAgentApplication(t, routeStore, server.ID, now)

		retype := routing.ProviderVLLM
		if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Type: &retype}); err != nil {
			t.Fatalf("retype away from server_agent: %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after retyping away from server_agent = %#v, want exactly [%q]", got, server.ID)
		}
	})

	// The deliberate over-notify: weight is no part of AgentRuntimeConfig, and
	// this still notifies. Cheap and idempotent (the agent re-fetches and its
	// driver applies only on a real change), versus a "relevant fields"
	// allow-list that would silently rot as the derivation grows.
	t.Run("edit that touches no runtime-relevant field", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		app := seedServerAgentApplication(t, routeStore, server.ID, now)

		weight := 7
		if _, err := svc.UpdateApplication(ctx, ownerToken(), app.ID, UpdateApplicationRequest{Weight: &weight}); err != nil {
			t.Fatalf("weight edit on the server_agent application: %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after a weight-only edit = %#v, want exactly [%q]", got, server.ID)
		}
	})

	t.Run("edit on an application that neither was nor becomes server_agent", func(t *testing.T) {
		svc, _, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("CreateApplication(vllm): %v", err)
		}

		weight := 7
		if _, err := svc.UpdateApplication(ctx, ownerToken(), plain.ID, UpdateApplicationRequest{Weight: &weight}); err != nil {
			t.Fatalf("weight edit on the vllm application: %v", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after editing a vllm application = %#v, want none", got)
		}
	})

	// A write that never happened must not announce a change: the uniqueness
	// gate (ErrServerAgentApplicationExists) returns before the store write,
	// and the notification sits after it.
	t.Run("rejected retype does not notify", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
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
			t.Fatalf("second server_agent via retype: err = %v, want ErrServerAgentApplicationExists", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after a rejected retype = %#v, want none", got)
		}
	})
}

// TestDeleteApplicationServerAgentFiresRuntimeChangedHook: deleting the
// server_agent application collapses AgentRuntimeConfig to the empty document
// (its "no server_agent application" case), which the agent must act on by
// tearing its router and every managed process down.
func TestDeleteApplicationServerAgentFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	agent := seedServerAgentApplication(t, routeStore, server.ID, now)
	plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication(vllm): %v", err)
	}

	if err := svc.DeleteApplication(ctx, ownerToken(), plain.ID); err != nil {
		t.Fatalf("DeleteApplication(vllm): %v", err)
	}
	if got := calls(); len(got) != 0 {
		t.Fatalf("calls after deleting a vllm application = %#v, want none", got)
	}

	if err := svc.DeleteApplication(ctx, ownerToken(), agent.ID); err != nil {
		t.Fatalf("DeleteApplication(server_agent): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
		t.Fatalf("calls after deleting the server_agent application = %#v, want exactly [%q]", got, server.ID)
	}
}

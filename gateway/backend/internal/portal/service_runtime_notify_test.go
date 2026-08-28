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
	return svc, routeStore, recordRuntimeChanged(svc)
}

// recordRuntimeChanged installs the recording hook on an ALREADY-built
// service and returns the snapshot accessor. Split out of
// newRuntimeNotifyTestService so the model-sync case, which needs
// newServerTestServiceWithLister's ModelLister, can record the same way
// without a second fixture constructor.
func recordRuntimeChanged(svc *Service) func() []string {
	var mu sync.Mutex
	var calls []string
	svc.SetRuntimeConfigChangedHook(func(serverID string) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, serverID)
	})
	return func() []string {
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

// --- Mapping write paths (row 3 of THE RULE) ---------------------------------

// TestCreateMappingFiresRuntimeChangedHookForServerAgent: a mapping under the
// server_agent application is a runtime-config input (its two model-name
// fields are a spec's model/upstream_model), so its write paths notify. The
// gate is the OWNING APPLICATION's type -- a mapping write on an ordinary
// upstream application is no part of any runtime-config document.
func TestCreateMappingFiresRuntimeChangedHookForServerAgent(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
	server := createTestServer(t, svc, "S", "s.example.test")
	// Seeded at the store layer so the application create itself does not fire
	// the hook: every assertion below is then about the MAPPING write alone.
	agent := seedServerAgentApplication(t, routeStore, server.ID, now)
	plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
		Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
	})
	if err != nil {
		t.Fatalf("CreateApplication(vllm): %v", err)
	}

	if _, err := svc.CreateMapping(ctx, ownerToken(), agent.ID, CreateMappingRequest{
		GatewayModelName: "qwen", AppModelName: "qwen",
	}); err != nil {
		t.Fatalf("CreateMapping(server_agent app): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
		t.Fatalf("calls after a mapping create on the server_agent application = %#v, want exactly [%q]", got, server.ID)
	}

	if _, err := svc.CreateMapping(ctx, ownerToken(), plain.ID, CreateMappingRequest{
		GatewayModelName: "llama", AppModelName: "llama",
	}); err != nil {
		t.Fatalf("CreateMapping(vllm app): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
		t.Fatalf("calls after also creating a mapping on the vllm application = %#v, want still exactly [%q]", got, server.ID)
	}
}

// TestUpdateMappingFiresRuntimeChangedHookForServerAgent covers the
// operator-visible case that motivated this fix (a rename rewrites the spec's
// model/upstream_model, and until the agent hears about it the new gateway
// model name 404s at its router while the old one still routes), the
// deliberate over-notify, and the two negatives.
func TestUpdateMappingFiresRuntimeChangedHookForServerAgent(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	// The 404-for-a-minute case: gateway_model_name IS the document's
	// specs[].model.
	t.Run("gateway model rename", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		agent := seedServerAgentApplication(t, routeStore, server.ID, now)
		mapping, err := svc.CreateMapping(ctx, ownerToken(), agent.ID, CreateMappingRequest{
			GatewayModelName: "qwen", AppModelName: "qwen",
		})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}

		renamed := "qwen-32b"
		if _, err := svc.UpdateMapping(ctx, ownerToken(), mapping.ID, UpdateMappingRequest{GatewayModelName: &renamed}); err != nil {
			t.Fatalf("rename the gateway model: %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID, server.ID}) {
			t.Fatalf("calls after create+rename = %#v, want exactly two pushes for %q", got, server.ID)
		}
	})

	// The deliberate over-notify, mirroring the application row's weight-only
	// edit: metrics_locked is no part of AgentRuntimeConfig, and this still
	// notifies -- a "relevant fields" allow-list would be a second copy of the
	// derivation that rots the moment it grows a field.
	t.Run("edit that touches no runtime-relevant field", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		agent := seedServerAgentApplication(t, routeStore, server.ID, now)
		mapping, err := svc.CreateMapping(ctx, ownerToken(), agent.ID, CreateMappingRequest{
			GatewayModelName: "qwen", AppModelName: "qwen",
		})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}

		locked := true
		if _, err := svc.UpdateMapping(ctx, ownerToken(), mapping.ID, UpdateMappingRequest{MetricsLocked: &locked}); err != nil {
			t.Fatalf("metrics_locked edit: %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID, server.ID}) {
			t.Fatalf("calls after create+metrics_locked edit = %#v, want exactly two pushes for %q", got, server.ID)
		}
	})

	t.Run("mapping on an ordinary application", func(t *testing.T) {
		svc, _, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("CreateApplication(vllm): %v", err)
		}
		mapping, err := svc.CreateMapping(ctx, ownerToken(), plain.ID, CreateMappingRequest{
			GatewayModelName: "qwen", AppModelName: "qwen",
		})
		if err != nil {
			t.Fatalf("CreateMapping: %v", err)
		}

		renamed := "qwen-32b"
		if _, err := svc.UpdateMapping(ctx, ownerToken(), mapping.ID, UpdateMappingRequest{GatewayModelName: &renamed}); err != nil {
			t.Fatalf("rename the gateway model: %v", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after mapping create+rename on a vllm application = %#v, want none", got)
		}
	})

	// A write that never happened must not announce a change: the gateway-name
	// conflict returns before the store write, and the notification sits after
	// it.
	t.Run("rejected rename does not notify", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		agent := seedServerAgentApplication(t, routeStore, server.ID, now)
		// Store-seeded so the two creates do not fire the hook: the assertion
		// below is then "a rejected update fires NOTHING", with no baseline to
		// subtract.
		mapping := seedMapping(t, routeStore, agent.ID, "qwen", now)
		seedMapping(t, routeStore, agent.ID, "llama", now)

		taken := "llama"
		if _, err := svc.UpdateMapping(ctx, ownerToken(), mapping.ID, UpdateMappingRequest{GatewayModelName: &taken}); !errors.Is(err, ErrMappingGatewayNameConflict) {
			t.Fatalf("rename onto a taken gateway name: err = %v, want ErrMappingGatewayNameConflict", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after a rejected rename = %#v, want none", got)
		}
	})
}

// TestDeleteMappingFiresRuntimeChangedHookForServerAgent: the store cascades a
// deleted mapping's runtime spec, its GPU rows and its co-residency pairs, so
// deleting a mapping under the server_agent application removes a whole spec
// from the document. DeleteMapping must therefore resolve the owning
// application's type BEFORE the row is gone.
func TestDeleteMappingFiresRuntimeChangedHookForServerAgent(t *testing.T) {
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
	plainMapping, err := svc.CreateMapping(ctx, ownerToken(), plain.ID, CreateMappingRequest{
		GatewayModelName: "llama", AppModelName: "llama",
	})
	if err != nil {
		t.Fatalf("CreateMapping(vllm app): %v", err)
	}
	agentMapping, err := svc.CreateMapping(ctx, ownerToken(), agent.ID, CreateMappingRequest{
		GatewayModelName: "qwen", AppModelName: "qwen",
	})
	if err != nil {
		t.Fatalf("CreateMapping(server_agent app): %v", err)
	}
	// A real enabled spec, so the delete really does drop a specs[] entry
	// rather than only exercising the gate. PutRuntimeSpec notifies itself
	// (pre-existing call site), hence the running count below.
	if _, err := svc.PutRuntimeSpec(ctx, ownerToken(), agentMapping.ID, PutRuntimeSpecRequest{
		Enabled: true, Binary: "/usr/local/bin/llama-server", Args: []string{"--model", "/models/q.gguf"},
	}); err != nil {
		t.Fatalf("PutRuntimeSpec: %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID, server.ID}) {
		t.Fatalf("calls after the mapping create + spec put = %#v, want exactly two pushes for %q", got, server.ID)
	}

	if err := svc.DeleteMapping(ctx, ownerToken(), plainMapping.ID); err != nil {
		t.Fatalf("DeleteMapping(vllm app): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID, server.ID}) {
		t.Fatalf("calls after deleting a vllm application's mapping = %#v, want still exactly two", got)
	}

	if err := svc.DeleteMapping(ctx, ownerToken(), agentMapping.ID); err != nil {
		t.Fatalf("DeleteMapping(server_agent app): %v", err)
	}
	if got := calls(); !reflect.DeepEqual(got, []string{server.ID, server.ID, server.ID}) {
		t.Fatalf("calls after deleting the server_agent application's mapping = %#v, want exactly three pushes for %q", got, server.ID)
	}
}

// TestSyncApplicationModelsFiresRuntimeChangedHookForServerAgent covers the
// FOURTH mapping write path -- the manual "Sync models" button and the
// background model_sync probe loop both reach reconcileApplicationModels,
// which creates and disables mappings under whatever application it is given,
// server_agent included. Notified for the same reason as the three explicit
// paths, and gated the same way (owning application type), plus the negative
// that a reconcile which wrote NOTHING announces nothing.
func TestSyncApplicationModelsFiresRuntimeChangedHookForServerAgent(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("sync that writes on the server_agent application", func(t *testing.T) {
		lister := &fakeLister{models: []string{"qwen", "llama"}}
		svc, routeStore := newServerTestServiceWithLister(t, now, lister)
		calls := recordRuntimeChanged(svc)
		server := createTestServer(t, svc, "S", "s.example.test")
		agent := seedServerAgentApplication(t, routeStore, server.ID, now)

		result, err := svc.SyncApplicationModels(ctx, ownerToken(), agent.ID)
		if err != nil {
			t.Fatalf("SyncApplicationModels: %v", err)
		}
		if result.Added != 2 {
			t.Fatalf("result = %#v, want Added:2", result)
		}
		// ONE push for the whole reconcile, not one per created mapping.
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after a sync that added two mappings = %#v, want exactly [%q]", got, server.ID)
		}

		// Nothing written -> nothing announced. This is not a field gate: it is
		// the same "after the successful store write" discipline as every other
		// call site, applied to a path that may make no write at all.
		result, err = svc.SyncApplicationModels(ctx, ownerToken(), agent.ID)
		if err != nil {
			t.Fatalf("second SyncApplicationModels: %v", err)
		}
		if result.Added != 0 || result.Disabled != 0 || result.Conflicted != 0 || result.Unchanged != 2 {
			t.Fatalf("second result = %#v, want Unchanged:2 only", result)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after a no-op re-sync = %#v, want still exactly [%q]", got, server.ID)
		}
	})

	t.Run("sync on an ordinary application", func(t *testing.T) {
		lister := &fakeLister{models: []string{"qwen", "llama"}}
		svc, _ := newServerTestServiceWithLister(t, now, lister)
		calls := recordRuntimeChanged(svc)
		server := createTestServer(t, svc, "S", "s.example.test")
		plain, err := svc.CreateApplication(ctx, ownerToken(), server.ID, CreateApplicationRequest{
			Type: routing.ProviderVLLM, Port: 8000, Scheme: "https",
		})
		if err != nil {
			t.Fatalf("CreateApplication(vllm): %v", err)
		}

		result, err := svc.SyncApplicationModels(ctx, ownerToken(), plain.ID)
		if err != nil {
			t.Fatalf("SyncApplicationModels: %v", err)
		}
		if result.Added != 2 {
			t.Fatalf("result = %#v, want Added:2", result)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after syncing a vllm application = %#v, want none", got)
		}
	})
}

// --- The AI server row (row 1 of THE RULE) -----------------------------------

// TestUpdateServerFiresRuntimeChangedHook: RuntimeMaxProcesses is the
// document's max_processes and UpdateServer is the only path that writes it.
// The notification is UNCONDITIONAL for the server -- neither gated on
// req.RuntimeMaxProcesses being set (that is the rejected "relevant fields"
// allow-list) nor on the server actually having a server_agent application
// (PushRuntimeConfig already fail-closes on "no runtime_manager agent
// connected" more cheaply and more accurately). Both of those deliberate
// choices are asserted here, so a later attempt to add either gate fails a
// test instead of silently narrowing the rule.
func TestUpdateServerFiresRuntimeChangedHook(t *testing.T) {
	now := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("runtime max processes", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		seedServerAgentApplication(t, routeStore, server.ID, now)

		limit := 3
		if _, err := svc.UpdateServer(ctx, adminToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &limit}); err != nil {
			t.Fatalf("UpdateServer(runtime_max_processes): %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after a runtime_max_processes edit = %#v, want exactly [%q]", got, server.ID)
		}
	})

	// The deliberate over-notify on the server row: a rename is no part of the
	// document and still notifies. Also the deliberate NON-gate on having a
	// server_agent application -- this server has none.
	t.Run("unrelated field on a server with no server_agent application", func(t *testing.T) {
		svc, _, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")

		name := "S renamed"
		if _, err := svc.UpdateServer(ctx, adminToken(), server.ID, UpdateServerRequest{Name: &name}); err != nil {
			t.Fatalf("UpdateServer(name): %v", err)
		}
		if got := calls(); !reflect.DeepEqual(got, []string{server.ID}) {
			t.Fatalf("calls after a rename = %#v, want exactly [%q]", got, server.ID)
		}
	})

	// A rejected write never announces a change: the limit validation returns
	// before UpdateAIServer, and the notification sits after it.
	t.Run("rejected update does not notify", func(t *testing.T) {
		svc, routeStore, calls := newRuntimeNotifyTestService(t, now)
		server := createTestServer(t, svc, "S", "s.example.test")
		seedServerAgentApplication(t, routeStore, server.ID, now)

		bad := -1
		if _, err := svc.UpdateServer(ctx, adminToken(), server.ID, UpdateServerRequest{RuntimeMaxProcesses: &bad}); !errors.Is(err, ErrServerRuntimeLimitInvalid) {
			t.Fatalf("negative runtime_max_processes: err = %v, want ErrServerRuntimeLimitInvalid", err)
		}
		if got := calls(); len(got) != 0 {
			t.Fatalf("calls after a rejected server update = %#v, want none", got)
		}
	})
}

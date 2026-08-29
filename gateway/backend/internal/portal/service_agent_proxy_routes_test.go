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

// mustCreateSwitchTestServer creates a minimal AI server for the P4 proxy-route
// derivation tests, with HTTPSSwitchOverride set directly (bypassing the portal
// setter, mirroring mustCreateNetbirdServer's direct-store-write convention).
func mustCreateSwitchTestServer(t *testing.T, svc *Service, ctx context.Context, id, override string) {
	t.Helper()
	now := time.Now().UTC()
	if err := svc.routes.CreateAIServer(ctx, routing.AIServer{
		ID: id, Name: id, Domain: id + ".int.example.test", Provider: "vllm", Status: "active",
		HTTPSSwitchOverride: override, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create server %s: %v", id, err)
	}
}

// mustCreateSwitchTestApp creates a minimal Application directly in the store
// (bypassing portal validation, like storeServerCert does for certificates).
// proxyListenPort 0 means "not yet assigned".
func mustCreateSwitchTestApp(t *testing.T, svc *Service, ctx context.Context, id, serverID string, port, proxyListenPort int) routing.Application {
	t.Helper()
	now := time.Now().UTC()
	app := routing.Application{
		ID: id, ServerID: serverID, Type: "openai_compatible", Port: port, Scheme: "http",
		Status: routing.ServerStatusActive, Priority: 1, Weight: 1,
		ProxyListenPort: proxyListenPort,
		CreatedAt:       now, UpdatedAt: now,
	}
	if err := svc.routes.CreateApplication(ctx, app); err != nil {
		t.Fatalf("create app %s: %v", id, err)
	}
	return app
}

func setHTTPSSwitchMode(t *testing.T, svc *Service, ctx context.Context, mode string) {
	t.Helper()
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &mode}); err != nil {
		t.Fatalf("set https switch mode %q: %v", mode, err)
	}
}

// TestAgentProxyRoutesDerivesAssignsAndPersistsPorts is the portal half of
// Certificates P4 Task 7's Step 1 contract: an in-scope server's two
// applications each get a ProxyListenPort auto-assigned (lowest free >= the
// configured base, unique per server) and PERSISTED, so a second derivation
// call is stable (idempotent assignment, unchanged ETag) and matches what the
// later switch reconcile (Task 10) will route to.
func TestAgentProxyRoutesDerivesAssignsAndPersistsPorts(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-1", "srv-a", 8080, 0)
	mustCreateSwitchTestApp(t, svc, ctx, "app-2", "srv-a", 8081, 0)

	dto, err := svc.AgentProxyRoutes(ctx, "srv-a")
	if err != nil {
		t.Fatalf("AgentProxyRoutes: %v", err)
	}
	if len(dto.Routes) != 2 {
		t.Fatalf("routes = %+v, want 2 entries", dto.Routes)
	}
	byApp := map[string]AgentProxyRouteDTO{}
	for _, r := range dto.Routes {
		byApp[r.AppID] = r
	}
	if r, ok := byApp["app-1"]; !ok || r.Listen != 8600 || r.Upstream != "http://127.0.0.1:8080" {
		t.Fatalf("app-1 route = %+v (present=%v), want {8600 http://127.0.0.1:8080}", r, ok)
	}
	if r, ok := byApp["app-2"]; !ok || r.Listen != 8601 || r.Upstream != "http://127.0.0.1:8081" {
		t.Fatalf("app-2 route = %+v (present=%v), want {8601 http://127.0.0.1:8081}", r, ok)
	}

	// Persisted: re-reading the applications shows the assigned ports.
	stored1, err := svc.routes.ApplicationByID(ctx, "app-1")
	if err != nil || stored1.ProxyListenPort != 8600 {
		t.Fatalf("stored app-1 ProxyListenPort = %d (err=%v), want 8600", stored1.ProxyListenPort, err)
	}
	stored2, err := svc.routes.ApplicationByID(ctx, "app-2")
	if err != nil || stored2.ProxyListenPort != 8601 {
		t.Fatalf("stored app-2 ProxyListenPort = %d (err=%v), want 8601", stored2.ProxyListenPort, err)
	}

	// Stable across a second call: same route set -> same ETag.
	dto2, err := svc.AgentProxyRoutes(ctx, "srv-a")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if dto2.ETag != dto.ETag {
		t.Fatalf("etag changed across an unchanged route set: %q vs %q", dto2.ETag, dto.ETag)
	}
}

// TestAgentProxyRoutesManualModeAndOutOfScopeReturnEmptyWithoutAssigningPorts
// pins the byte-neutral default: "manual" mode (never explicitly set) and an
// explicitly-excluded server under "auto" both yield an empty route list AND
// leave every application's ProxyListenPort untouched -- the agent then runs
// no local TLS proxy at all, and no port is silently consumed.
func TestAgentProxyRoutesManualModeAndOutOfScopeReturnEmptyWithoutAssigningPorts(t *testing.T) {
	svc, ctx := certEnv(t)
	mustCreateSwitchTestServer(t, svc, ctx, "srv-b", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-b1", "srv-b", 9000, 0)

	dto, err := svc.AgentProxyRoutes(ctx, "srv-b")
	if err != nil {
		t.Fatalf("manual mode: %v", err)
	}
	if len(dto.Routes) != 0 {
		t.Fatalf("manual mode routes = %+v, want empty", dto.Routes)
	}
	stored, err := svc.routes.ApplicationByID(ctx, "app-b1")
	if err != nil || stored.ProxyListenPort != 0 {
		t.Fatalf("manual mode must not assign a port: %+v (err=%v)", stored, err)
	}

	// auto mode, but this server is explicitly excluded (opt-out).
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	if err := svc.routes.UpdateServerHTTPSSwitchOverride(ctx, "srv-b", "exclude"); err != nil {
		t.Fatalf("exclude override: %v", err)
	}
	dto, err = svc.AgentProxyRoutes(ctx, "srv-b")
	if err != nil {
		t.Fatalf("excluded server: %v", err)
	}
	if len(dto.Routes) != 0 {
		t.Fatalf("excluded server routes = %+v, want empty", dto.Routes)
	}
	stored, err = svc.routes.ApplicationByID(ctx, "app-b1")
	if err != nil || stored.ProxyListenPort != 0 {
		t.Fatalf("excluded server must not assign a port: %+v (err=%v)", stored, err)
	}
}

// TestAgentProxyRoutesSelectedModeRequiresInclude covers the third mode: only
// a server explicitly overridden "include" is in scope.
func TestAgentProxyRoutesSelectedModeRequiresInclude(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "selected")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-e", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-e1", "srv-e", 7000, 0)

	if dto, err := svc.AgentProxyRoutes(ctx, "srv-e"); err != nil || len(dto.Routes) != 0 {
		t.Fatalf("selected mode, no override: routes=%+v err=%v, want empty", dto.Routes, err)
	}

	if err := svc.routes.UpdateServerHTTPSSwitchOverride(ctx, "srv-e", "include"); err != nil {
		t.Fatalf("include override: %v", err)
	}
	dto, err := svc.AgentProxyRoutes(ctx, "srv-e")
	if err != nil {
		t.Fatalf("selected mode, included: %v", err)
	}
	if len(dto.Routes) != 1 {
		t.Fatalf("routes = %+v, want 1 entry", dto.Routes)
	}
}

// TestAgentProxyRoutesUnknownOrEmptyServerReturnsEmptyNoError pins the
// GENUINELY-empty cases: a server that does not exist (store.ErrNotFound), or
// an empty serverID, is the same safe empty default as out-of-scope -- never
// an error. It is the other half of
// TestAgentProxyRoutesPropagatesTransientServerAndSettingsReadFailures below,
// and the reason that fix discriminates on store.ErrNotFound rather than
// propagating every AIServerByID error.
func TestAgentProxyRoutesUnknownOrEmptyServerReturnsEmptyNoError(t *testing.T) {
	svc, ctx := certEnv(t)
	if dto, err := svc.AgentProxyRoutes(ctx, "does-not-exist"); err != nil || len(dto.Routes) != 0 {
		t.Fatalf("unknown server = %+v, err=%v, want empty/no error", dto.Routes, err)
	}
	if dto, err := svc.AgentProxyRoutes(ctx, ""); err != nil || len(dto.Routes) != 0 {
		t.Fatalf("empty serverID = %+v, err=%v, want empty/no error", dto.Routes, err)
	}
}

// TestAgentProxyRoutesUsesConfiguredPortBase confirms cert_proxy_listen_port_base
// (not the hardcoded default) drives auto-assignment.
func TestAgentProxyRoutesUsesConfiguredPortBase(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	base := 9500
	if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertProxyListenPortBase: &base}); err != nil {
		t.Fatalf("set port base: %v", err)
	}
	mustCreateSwitchTestServer(t, svc, ctx, "srv-c", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-c1", "srv-c", 7000, 0)

	dto, err := svc.AgentProxyRoutes(ctx, "srv-c")
	if err != nil {
		t.Fatalf("AgentProxyRoutes: %v", err)
	}
	if len(dto.Routes) != 1 || dto.Routes[0].Listen != 9500 {
		t.Fatalf("routes = %+v, want listen=9500", dto.Routes)
	}
}

// TestAgentProxyRoutesPreservesExplicitProxyListenPort confirms an
// already-assigned port (whether auto-assigned earlier or set explicitly) is
// never reassigned/churned, even when it falls outside the configured base.
func TestAgentProxyRoutesPreservesExplicitProxyListenPort(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-d", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-d1", "srv-d", 7000, 9999)

	dto, err := svc.AgentProxyRoutes(ctx, "srv-d")
	if err != nil {
		t.Fatalf("AgentProxyRoutes: %v", err)
	}
	if len(dto.Routes) != 1 || dto.Routes[0].Listen != 9999 {
		t.Fatalf("routes = %+v, want the pre-assigned listen=9999 preserved", dto.Routes)
	}
}

// TestAgentProxyRoutesPropagatesTransientServerAndSettingsReadFailures is the
// proxy-route half of C1, decided separately from it rather than inherited by
// silence.
//
// This method used to answer BOTH reads' failures with the safe-empty route
// list and err == nil, on NetbirdOnly's "reads never fail" convention. That
// convention belongs to accessors whose signature has no error channel at all
// (NetbirdOnly returns a bare bool, so a glitch has to become SOME value);
// this one returns an error, its only caller already answers a non-nil error
// with 500, and the agent's Fetch already keeps its current routes on any
// non-200. Collapsing here therefore throws away the one signal every layer
// downstream is built to handle.
//
// What the collapse costs, concretely -- an EMPTY route list is not "the
// agent runs no local TLS proxy", it is a TEARDOWN of the ones it is running:
//
//   - Driver.SyncRoutes applies the empty set (only a fetch ERROR keeps the
//     current routes), and Manager.reconcileLocked closes every listener no
//     longer desired -- proxy_test.go's TestManagerDrainsRemovedRoute pins
//     exactly that: the dropped listener stops accepting and leaves Status().
//   - Every app already proxy-switched has scheme https + ProxyListenPort, so
//     routing.ApplicationEndpoint points at the port just closed: connection
//     refused for every request AND for the health probe.
//   - ReconcileHTTPSSwitch cannot undo it. The torn-down route is MISSING from
//     the status snapshot, not explicitly tls_active=false, and a missing route
//     is deliberately never a revert (an agent that merely went silent must not
//     lose a working switch). The unconditional scope-exit revert does not
//     apply either: the server is still in scope -- the collapse happened
//     BEFORE the scope test.
//   - Recovery waits for the agent's next certificate-poll tick, which is the
//     cadence SyncRoutes rides: 6h on the WebSocket transport, 15m on POST.
//
// store.ErrNotFound stays the genuinely-empty case, exactly as in
// AgentRuntimeConfig: no such server row really does mean "no routes".
func TestAgentProxyRoutesPropagatesTransientServerAndSettingsReadFailures(t *testing.T) {
	t.Run("server read failure", func(t *testing.T) {
		transient := errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
		svc, ctx := certEnv(t)
		setHTTPSSwitchMode(t, svc, ctx, "auto")
		mustCreateSwitchTestServer(t, svc, ctx, "srv-g0", "")
		mustCreateSwitchTestApp(t, svc, ctx, "app-g0", "srv-g0", 7000, 0)

		svc.routes = &failAIServerByIDStore{Store: svc.routes, err: transient}
		dto, err := svc.AgentProxyRoutes(ctx, "srv-g0")
		if err == nil {
			t.Fatalf("AgentProxyRoutes returned dto = %+v with a nil error; a transient server read failure must be propagated, never collapsed into the empty route list", dto)
		}
		if !errors.Is(err, transient) {
			t.Fatalf("err = %v, want it to wrap %v", err, transient)
		}
	})

	t.Run("settings read failure", func(t *testing.T) {
		transient := errors.New("settings down")
		svc, ctx := certEnv(t)
		setHTTPSSwitchMode(t, svc, ctx, "auto")
		mustCreateSwitchTestServer(t, svc, ctx, "srv-g", "")
		mustCreateSwitchTestApp(t, svc, ctx, "app-g1", "srv-g", 7000, 0)

		svc.settings = &failSystemSettingsReadAt{SystemSettingsStore: svc.settings, failAt: 1, failErr: transient}
		dto, err := svc.AgentProxyRoutes(ctx, "srv-g")
		if err == nil {
			t.Fatalf("AgentProxyRoutes returned dto = %+v with a nil error; a transient settings read failure must be propagated, never collapsed into the empty route list", dto)
		}
		if !errors.Is(err, transient) {
			t.Fatalf("err = %v, want it to wrap %v", err, transient)
		}
	})
}

// failApplicationsByServerStore injects an ApplicationsByServer failure onto an
// otherwise-real routing.Store, mirroring failCertificatesStore.
type failApplicationsByServerStore struct {
	routing.Store
	err error
}

func (s *failApplicationsByServerStore) ApplicationsByServer(context.Context, string) ([]routing.Application, error) {
	return nil, s.err
}

// failUpdateApplicationStore injects an UpdateApplication failure onto an
// otherwise-real routing.Store.
type failUpdateApplicationStore struct {
	routing.Store
	err error
}

func (s *failUpdateApplicationStore) UpdateApplication(context.Context, routing.Application) error {
	return s.err
}

// TestAgentProxyRoutesPropagatesApplicationReadAndWriteFailures confirms a
// genuine store failure while listing or persisting applications is
// propagated as a real error -- unlike a missing server/settings glitch, a
// failed port-assignment write is exactly the kind of state corruption that
// must surface rather than be silently swallowed into an empty route list.
func TestAgentProxyRoutesPropagatesApplicationReadAndWriteFailures(t *testing.T) {
	t.Run("applications read failure", func(t *testing.T) {
		routes := routing.NewMemoryStore()
		ctx := context.Background()
		now := time.Now().UTC()
		if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-f", Name: "srv-f", Domain: "srv-f.int.example.test", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		failing := &failApplicationsByServerStore{Store: routes, err: errors.New("db down")}
		svc := NewService(ServiceDeps{Routes: failing, SystemSettings: NewMemorySystemSettings(), SettingsVolatile: true})
		mode := "auto"
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &mode}); err != nil {
			t.Fatalf("set mode: %v", err)
		}
		if _, err := svc.AgentProxyRoutes(ctx, "srv-f"); err == nil {
			t.Fatal("want the applications-read error propagated, got nil")
		}
	})

	t.Run("update application (port persist) failure", func(t *testing.T) {
		routes := routing.NewMemoryStore()
		ctx := context.Background()
		now := time.Now().UTC()
		if err := routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-h", Name: "srv-h", Domain: "srv-h.int.example.test", Status: "active", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		if err := routes.CreateApplication(ctx, routing.Application{
			ID: "app-h1", ServerID: "srv-h", Type: "openai_compatible", Port: 7000, Scheme: "http",
			Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		failing := &failUpdateApplicationStore{Store: routes, err: errors.New("write failed")}
		svc := NewService(ServiceDeps{Routes: failing, SystemSettings: NewMemorySystemSettings(), SettingsVolatile: true})
		mode := "auto"
		if _, err := svc.UpdateSystemSettings(ctx, systemToken(), UpdateSystemSettingsRequest{CertHTTPSSwitchMode: &mode}); err != nil {
			t.Fatalf("set mode: %v", err)
		}
		if _, err := svc.AgentProxyRoutes(ctx, "srv-h"); err == nil {
			t.Fatal("want the port-persist error propagated, got nil")
		}
	})
}

// TestAgentProxyRoutesSkipsExcludedApplicationEntirely is the route-publication
// half of the per-application opt-out, and it asserts the ABSENCE of a write,
// not merely the absence of a route.
//
// One clause in isProxySwitchCandidate stops all of it: no route published (so
// no listener opened), and no AssignProxyListenPort call (so no port assigned
// and no UpdateApplication write). The write matters independently — a version
// that filtered the ROUTE but still assigned the port would look correct in the
// response and quietly churn the row on every agent fetch, forever.
//
// It also pins that the released port is deliberately REUSABLE: the sibling
// draws exactly the number the excluded application gave up. That is a
// behaviour change from "a non-candidate's port stays reserved against every
// sibling forever", and it is intended, so it is asserted rather than left to
// be rediscovered as a bug.
func TestAgentProxyRoutesSkipsExcludedApplicationEntirely(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-a", "")

	// The excluded application HELD 8600 before the operator excluded it; the
	// exclusion cleared it to 0 (the invariant), so 8600 is free again.
	excluded := mustCreateSwitchTestApp(t, svc, ctx, "app-excluded", "srv-a", 8080, 0)
	excluded.ProxyExcluded = true
	excluded.UpdatedAt = excluded.UpdatedAt.Add(time.Minute)
	if err := svc.routes.UpdateApplication(ctx, excluded); err != nil {
		t.Fatalf("mark excluded: %v", err)
	}
	before, err := svc.routes.ApplicationByID(ctx, "app-excluded")
	if err != nil {
		t.Fatalf("read app-excluded: %v", err)
	}
	mustCreateSwitchTestApp(t, svc, ctx, "app-sibling", "srv-a", 8081, 0)

	// Repeated fetches, like a real agent's poll: a per-fetch write would show
	// up as a moved UpdatedAt even if the first pass happened to look clean.
	for fetch := 0; fetch < 3; fetch++ {
		dto, err := svc.AgentProxyRoutes(ctx, "srv-a")
		if err != nil {
			t.Fatalf("AgentProxyRoutes (fetch %d): %v", fetch, err)
		}
		if len(dto.Routes) != 1 {
			t.Fatalf("fetch %d: routes = %+v, want exactly the sibling's", fetch, dto.Routes)
		}
		if dto.Routes[0].AppID != "app-sibling" {
			t.Fatalf("fetch %d: route published for %q, want app-sibling", fetch, dto.Routes[0].AppID)
		}
		if dto.Routes[0].Listen != 8600 {
			t.Fatalf("fetch %d: sibling listen = %d, want 8600 -- the port the excluded application released must return to the free pool",
				fetch, dto.Routes[0].Listen)
		}
	}

	after, err := svc.routes.ApplicationByID(ctx, "app-excluded")
	if err != nil {
		t.Fatalf("read app-excluded (after): %v", err)
	}
	if after.ProxyListenPort != 0 {
		t.Fatalf("excluded application was assigned ProxyListenPort = %d, want 0", after.ProxyListenPort)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("excluded application was WRITTEN by the routes derivation: UpdatedAt %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}
	if after.ProxyExcluded != true || after.Scheme != "http" {
		t.Fatalf("excluded application changed shape: %+v", after)
	}
}

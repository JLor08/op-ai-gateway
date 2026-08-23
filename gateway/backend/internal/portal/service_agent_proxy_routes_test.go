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

// TestAgentProxyRoutesUnknownOrEmptyServerReturnsEmptyNoError mirrors the
// "reads never fail" convention used elsewhere in this file (NetbirdOnly,
// CertHTTPSSwitchMode): a server that does not exist, or an empty serverID,
// is the same safe empty default as out-of-scope -- never an error.
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

// TestAgentProxyRoutesSettingsReadFailureIsSafeEmpty mirrors NetbirdOnly's
// nil-safe convention: a settings-store read glitch must never be reported to
// the agent as a hard error, only as the same safe empty default as
// out-of-scope.
func TestAgentProxyRoutesSettingsReadFailureIsSafeEmpty(t *testing.T) {
	svc, ctx := certEnv(t)
	setHTTPSSwitchMode(t, svc, ctx, "auto")
	mustCreateSwitchTestServer(t, svc, ctx, "srv-g", "")
	mustCreateSwitchTestApp(t, svc, ctx, "app-g1", "srv-g", 7000, 0)

	svc.settings = &failSystemSettingsReadAt{SystemSettingsStore: svc.settings, failAt: 1, failErr: errors.New("settings down")}
	dto, err := svc.AgentProxyRoutes(ctx, "srv-g")
	if err != nil {
		t.Fatalf("settings read failure must be a safe empty default, not an error: %v", err)
	}
	if len(dto.Routes) != 0 {
		t.Fatalf("routes = %+v, want empty on a settings read failure", dto.Routes)
	}
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

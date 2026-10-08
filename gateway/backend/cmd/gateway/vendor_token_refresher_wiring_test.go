// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"path/filepath"
	"strings"
	"testing"
)

// The vendor model discovery needs the gateway Server's locked token refresh to
// renew an expired subscription token (a portal-side refresh would race the
// dispatch for the single-use refresh token). The Server exists only after the
// portal Service, so the wiring is a pair, like SetBenchmarkReservationHook: the
// driver builders must hand the Service's setter forward on gateway.ServerDeps,
// and buildGatewayServer must call it with the built Server's refresher. Dropping
// either half leaves "refresh models" silently unable to refresh an expired token,
// with every Go test of the pieces still green.
func TestDriverDepsHandTheVendorTokenRefresherSetterForward(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "")
	deps, cleanup, err := memoryDeps(config.Config{Addr: "127.0.0.1:8080", DBDriver: "memory"})
	if err != nil {
		t.Fatalf("memoryDeps returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })
	if deps.SetVendorTokenRefresher == nil {
		t.Fatal("memoryDeps left ServerDeps.SetVendorTokenRefresher nil -- buildGatewayServer has no setter to call once the Server exists")
	}

	sqliteDepsValue, sqliteCleanup, err := sqliteDeps(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "gateway.db"), AutoMigrate: true,
	})
	if err != nil {
		t.Fatalf("sqliteDeps returned %v", err)
	}
	t.Cleanup(func() { _ = sqliteCleanup() })
	if sqliteDepsValue.SetVendorTokenRefresher == nil {
		t.Fatal("sqliteDeps left ServerDeps.SetVendorTokenRefresher nil")
	}

	// postgresDeps needs a live DSN, so the shared body all drivers funnel into is
	// pinned in source instead (mirrors TestPostgresDepsWiresTheCertCipher).
	runtimeBody := funcSource(t, "main.go", "buildRuntime")
	if !containsWired(runtimeBody, "SetVendorTokenRefresher: portalService.SetVendorTokenRefresher,") {
		t.Fatal("buildRuntime does not hand portalService.SetVendorTokenRefresher forward as ServerDeps.SetVendorTokenRefresher")
	}
	assertAllDriversReachBuildRuntime(t)
}

func TestBuildGatewayServerWiresTheServersTokenRefresherIntoThePortal(t *testing.T) {
	body := funcSource(t, "main.go", "buildGatewayServer")
	const call = "deps.SetVendorTokenRefresher(srv.RefreshVendorSubscriptionTokens)"
	if !containsWired(body, call) {
		t.Fatalf("buildGatewayServer does not invoke %s -- the portal could never refresh an expired subscription token for a model discovery", call)
	}
	if !containsWired(body, "if deps.SetVendorTokenRefresher != nil {") {
		t.Fatal("buildGatewayServer must call the setter nil-safely (a Server built without the portal setter has nothing to wire)")
	}
	newIdx, callIdx := strings.Index(body, "gateway.New(deps)"), strings.Index(body, call)
	if newIdx < 0 || callIdx < newIdx {
		t.Fatalf("the refresher wiring (at %d) must come AFTER gateway.New (at %d): srv must exist before its method can be referenced", callIdx, newIdx)
	}
	// It must be wired before the Server starts serving anything that can run a
	// discovery: before the scheduler and the other background loops start.
	if sched := strings.Index(body, "srv.StartBenchmarkScheduler("); sched < callIdx {
		t.Fatalf("the refresher wiring (at %d) must come before the benchmark scheduler starts (at %d)", callIdx, sched)
	}
}

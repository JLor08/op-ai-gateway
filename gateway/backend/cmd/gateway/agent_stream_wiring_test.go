// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"os"
	"strings"
	"testing"
)

// TestAllThreeDriversWireAgentStreamsIntoCertificateIssuedHook is the source-scan
// completeness guard for the Phase 2 distribution doorbell -- the counterpart to
// cert_trigger_test.go's TestAllThreeDriversWireCertSettingsChangeTrigger. Postgres
// is the DEFAULT production driver and needs a live server, so it has no
// behavioural test here; a registry wired in only some driver paths would leave a
// certificate reconcile silently unable to notify any agent in the unwired
// one(s) (the agent would simply fall back to its own poll/reconnect cadence,
// with no error anywhere to reveal the gap).
//
// This pins the wiring by SOURCE, the same technique the cert-trigger guard
// uses: the ONE portal.NewService call site must pass the SAME agentStreams
// instance as OnCertificateIssued that the same gateway.ServerDeps literal
// carries as AgentStreams -- so the side that registers a server's open
// connections and the side that pushes to them can never be two different
// registries.
//
// Since CMP-1, memoryDeps/sqliteDeps/postgresDeps share ONE body
// (buildRuntime, reached directly by memoryDeps and via sqlDeps by
// sqliteDeps/postgresDeps) instead of each inlining this wiring separately,
// so there is now exactly one portal.NewService call site rather than three;
// this test additionally pins each driver's call chain into that shared body
// so a driver that stopped reaching it would still be caught.
func TestAllThreeDriversWireAgentStreamsIntoCertificateIssuedHook(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)

	newServiceCalls := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if newServiceCalls != 1 {
		t.Fatalf("found %d portal.NewService call sites, expected 1 (buildRuntime, shared by memory/sqlite/postgres)", newServiceCalls)
	}

	if got := strings.Count(body, "agentStreams := gateway.NewAgentStreamRegistry()"); got != 1 {
		t.Fatalf("gateway.NewAgentStreamRegistry() is constructed %d times, want 1 (buildRuntime)", got)
	}
	if !containsWired(body, "OnCertificateIssued: agentStreams.NotifyCertUpdate,") {
		t.Fatal("OnCertificateIssued is not wired to agentStreams.NotifyCertUpdate -- a certificate " +
			"reconcile can never notify a waiting agent, and nothing would ever surface that gap " +
			"(the agent's own poll/reconnect masks it)")
	}
	if !containsWired(body, "AgentStreams: agentStreams,") {
		t.Fatal("gateway.ServerDeps.AgentStreams is not set to the SAME agentStreams instance " +
			"constructed above -- if it diverges from the instance passed as OnCertificateIssued, " +
			"the push side and the connection-registration side would be two different registries")
	}

	assertAllDriversReachBuildRuntime(t)
}

// TestBuildGatewayServerWiresRuntimeConfigChangedHookForAllDrivers is Task 8's
// wiring completeness guard, the counterpart to
// TestAllThreeDriversWireAgentStreamsIntoCertificateIssuedHook above for the
// runtime-config PUSH path. Unlike the cert-update wiring (a plain function
// value the portal Service can be constructed with directly), the
// runtime-config hook needs the gateway Server itself
// (Server.PushRuntimeConfig), which does not exist until AFTER
// gateway.New(deps) returns -- but portalService (needed to call its own
// exported setter, Service.SetRuntimeConfigChangedHook) only exists inside
// buildRuntime, before it is wrapped into ServerDeps.Portal. Neither side can
// wire the other in at its own construction time, so the setter itself is
// handed forward as a plain ServerDeps field
// (gateway.ServerDeps.SetRuntimeConfigChangedHook) and invoked once
// buildGatewayServer has a live srv. See that field's doc comment
// (internal/gateway/server.go) for the full construction-order rationale.
//
// This is a wiring pair a future refactor could drop EITHER half of
// independently, so this test pins both:
//  1. buildRuntime must assign the field from the SAME portalService it
//     wraps into ServerDeps.Portal a few lines above/below.
//  2. buildGatewayServer must actually invoke it (nil-safely) AFTER
//     gateway.New.
//
// buildRuntime is the ONE shared body all three drivers funnel into
// (assertAllDriversReachBuildRuntime below), and buildGatewayServer is the
// ONE function every driver's own switch statement falls through to the
// SAME gateway.New(deps) call from, so pinning both there covers all three
// drivers by construction, not by per-driver repetition.
func TestBuildGatewayServerWiresRuntimeConfigChangedHookForAllDrivers(t *testing.T) {
	runtimeBody := funcSource(t, "main.go", "buildRuntime")
	if !containsWired(runtimeBody, "SetRuntimeConfigChangedHook: portalService.SetRuntimeConfigChangedHook,") {
		t.Fatal("buildRuntime does not hand portalService.SetRuntimeConfigChangedHook forward as " +
			"ServerDeps.SetRuntimeConfigChangedHook -- without it there is no setter left for " +
			"buildGatewayServer to call once the gateway Server exists")
	}

	serverBody := funcSource(t, "main.go", "buildGatewayServer")
	if got := strings.Count(serverBody, "gateway.New(deps)"); got != 1 {
		t.Fatalf("buildGatewayServer calls gateway.New(deps) %d times, want exactly 1 (the ONE call site all three drivers share)", got)
	}
	if !containsWired(serverBody, "deps.SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)") {
		t.Fatal("buildGatewayServer does not invoke deps.SetRuntimeConfigChangedHook with " +
			"srv.PushRuntimeConfig -- a runtime-spec write can never push to a connected agent, and " +
			"nothing would ever surface that gap (the agent's own poll cadence masks it)")
	}
	newIdx := strings.Index(serverBody, "gateway.New(deps)")
	hookIdx := strings.Index(serverBody, "deps.SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)")
	if newIdx < 0 || hookIdx < 0 || hookIdx < newIdx {
		t.Fatal("the SetRuntimeConfigChangedHook invocation must come AFTER gateway.New constructs " +
			"srv -- srv must exist before PushRuntimeConfig, a method on it, can be referenced")
	}

	for _, want := range []string{
		`deps, cleanup, err = memoryDeps(cfg)`,
		`deps, cleanup, err = sqliteDeps(cfg)`,
		`deps, cleanup, err = postgresDeps(cfg)`,
	} {
		if !containsWired(serverBody, want) {
			t.Fatalf("buildGatewayServer's driver switch is missing %q -- a driver that never reaches "+
				"the shared gateway.New(deps)/SetRuntimeConfigChangedHook wiring above would never get "+
				"the runtime-config push either", want)
		}
	}

	assertAllDriversReachBuildRuntime(t)
}

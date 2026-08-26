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
// runtime-config PUSH path. Unlike that cert-update wiring (a plain function
// value the portal Service can be constructed with directly), the
// runtime-config hook needs the gateway Server itself
// (Server.PushRuntimeConfig) -- which cmd/gateway builds AFTER the portal
// Service (deps.Portal wraps it for ServerDeps) -- so it cannot be wired at
// portal.NewService construction time; see Service.
// SetRuntimeConfigChangedHook's doc for why it is an exported setter instead,
// called here once srv exists.
//
// buildGatewayServer is the ONE function every OP_AI_GATEWAY_DB_DRIVER branch
// reaches (its own switch dispatches to memoryDeps/sqliteDeps/postgresDeps
// and then falls through, unconditionally, to the SAME gateway.New(deps)
// call), so pinning the wiring there -- AFTER that call -- covers all three
// drivers by construction; a driver whose path skipped gateway.New would
// already fail the count check below.
func TestBuildGatewayServerWiresRuntimeConfigChangedHookForAllDrivers(t *testing.T) {
	body := funcSource(t, "main.go", "buildGatewayServer")

	if got := strings.Count(body, "gateway.New(deps)"); got != 1 {
		t.Fatalf("buildGatewayServer calls gateway.New(deps) %d times, want exactly 1 (the ONE call site all three drivers share)", got)
	}
	if !containsWired(body, "portal.UnwrapService(srv.Portal)") {
		t.Fatal("buildGatewayServer does not recover the concrete *portal.Service from srv.Portal via " +
			"portal.UnwrapService -- without it there is nothing left to call " +
			"SetRuntimeConfigChangedHook on once gateway.New has wrapped it")
	}
	if !containsWired(body, "SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)") {
		t.Fatal("SetRuntimeConfigChangedHook is not wired to srv.PushRuntimeConfig -- a runtime-spec " +
			"write can never push to a connected agent, and nothing would ever surface that gap " +
			"(the agent's own poll cadence masks it)")
	}
	newIdx := strings.Index(body, "gateway.New(deps)")
	hookIdx := strings.Index(body, "SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)")
	if newIdx < 0 || hookIdx < 0 || hookIdx < newIdx {
		t.Fatal("SetRuntimeConfigChangedHook must be wired AFTER gateway.New constructs srv -- srv must " +
			"exist before PushRuntimeConfig, a method on it, can be referenced")
	}

	for _, want := range []string{
		`deps, cleanup, err = memoryDeps(cfg)`,
		`deps, cleanup, err = sqliteDeps(cfg)`,
		`deps, cleanup, err = postgresDeps(cfg)`,
	} {
		if !containsWired(body, want) {
			t.Fatalf("buildGatewayServer's driver switch is missing %q -- a driver that never reaches "+
				"the shared gateway.New(deps)/SetRuntimeConfigChangedHook wiring above would never get "+
				"the runtime-config push either", want)
		}
	}
}

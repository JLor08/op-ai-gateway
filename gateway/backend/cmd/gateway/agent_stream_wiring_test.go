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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"op-ai-gateway/internal/config"
	"os"
	"strings"
	"testing"
)

// Since CMP-1, memoryDeps/sqliteDeps/postgresDeps share ONE body
// (buildRuntime, reached directly by memoryDeps and via sqlDeps by
// sqliteDeps/postgresDeps) instead of each inlining this wiring separately,
// so there is now exactly one portal.NewService call site rather than three;
// this test additionally pins each driver's call chain into that shared body
// so a driver that stopped reaching it would still be caught.
func TestAllThreeDriversWireAgentBindHost(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	body := string(src)
	drivers := strings.Count(body, "portal.NewService(portal.ServiceDeps{")
	if drivers != 1 {
		t.Fatalf("found %d portal.NewService call sites, expected 1 (buildRuntime, shared by memory/sqlite/postgres)", drivers)
	}
	if got := strings.Count(body, "AgentBindHost: explicitAgentBindHost(cfg)"); got != drivers {
		t.Fatalf("AgentBindHost is wired through explicitAgentBindHost in %d of %d driver paths", got, drivers)
	}

	assertAllDriversReachBuildRuntime(t)

	for _, tc := range []struct {
		name string
		addr string
		want string
	}{
		{name: "empty", addr: "", want: ""},
		{name: "trimmed IPv4", addr: " 100.64.0.10:8081 ", want: "100.64.0.10"},
		{name: "IPv6", addr: "[fd00::10]:8081", want: "fd00::10"},
		{name: "hostname remains a host", addr: "gateway.mesh.test:8081", want: "gateway.mesh.test"},
		{name: "invalid", addr: "missing-port", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := explicitAgentBindHost(config.Config{AgentAddr: tc.addr}); got != tc.want {
				t.Fatalf("explicitAgentBindHost(%q) = %q, want %q", tc.addr, got, tc.want)
			}
		})
	}
}

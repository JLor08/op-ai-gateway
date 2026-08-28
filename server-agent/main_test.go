// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import "testing"

// TestIsLoopbackHost pins the startup advisory's predicate. It only decides
// whether a warning is printed -- never how anything routes -- so the cost of
// being wrong is a missing or a spurious line, not a misdirected request. It
// still has to be right about the shapes an operator actually writes into
// runtime_router_bind.
func TestIsLoopbackHost(t *testing.T) {
	loopback := []string{"127.0.0.1", "127.0.0.53", "localhost", "LocalHost", "::1", "::ffff:127.0.0.1"}
	for _, host := range loopback {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	other := []string{"", "0.0.0.0", "10.4.0.7", "192.168.1.10", "fd00::1", "agent.mesh.internal", "localhost.evil.com", "not-an-ip"}
	for _, host := range other {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

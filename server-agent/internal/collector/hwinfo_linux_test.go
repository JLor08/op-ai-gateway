// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"context"
	"net/http"
	"op-ai-server-agent/internal/sample"
	"testing"
)

func TestLinuxMemoryModulesFromHelper(t *testing.T) {
	origEuid := helperGeteuid
	t.Cleanup(func() { helperGeteuid = origEuid })
	helperGeteuid = func() int { return 1000 }

	var path string
	socket := serveHelperSocket(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"modules":[{"locator":"DIMM_A1","size_bytes":17179869184}]}`))
	}))
	got := linuxMemoryModules(context.Background(), HardwareOptions{HelperSocket: socket})
	eq(t, HelperMemoryModulesPath, path)
	eq(t, []sample.MemoryModule{{Locator: "DIMM_A1", SizeBytes: 17179869184}}, got)

	// A missing helper leaves the modules empty (RAM total only).
	if got := linuxMemoryModules(context.Background(), HardwareOptions{HelperSocket: socket + ".missing"}); got != nil {
		t.Fatalf("missing helper: got %#v, want nil", got)
	}
	// Without a socket the unprivileged agent does not try.
	if got := linuxMemoryModules(context.Background(), HardwareOptions{}); got != nil {
		t.Fatalf("no socket: got %#v, want nil", got)
	}
}

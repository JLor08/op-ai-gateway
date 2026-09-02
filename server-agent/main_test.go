// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

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

// TestMeasurerWiringGoesThroughTheSelector pins the one line of this file that
// is platform-critical.
//
// On Windows, collector.NewNvidiaComputeApps is not a no-op but a hazard:
// nvidia-smi reports `[N/A]` for per-process memory under WDDM, which parses
// to 0, and a measured 0 OVERRIDES the operator's VRAM estimate in
// buildSnapshot -- so every managed model is charged 0 MB and the GPU budget
// looks free. collector.NewVRAMMeasurer is the platform split that keeps that
// measurer away from Windows, and calling the compute-apps constructor
// directly from here would route straight around it.
//
// CI compiles nothing for Windows, so no build or test on a runner can catch
// the mistake. Reading this file's own AST can. Mode 0 leaves comments
// unattached, so the prose above does not count as a reference.
func TestMeasurerWiringGoesThroughTheSelector(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	called := make(map[string]bool)
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			called[id.Name] = true
		}
		return true
	})
	if called["NewNvidiaComputeApps"] {
		t.Error("main.go calls collector.NewNvidiaComputeApps directly -- it must go through collector.NewVRAMMeasurer, which keeps the compute-apps measurer (and its measured zeros) off Windows")
	}
	if !called["NewVRAMMeasurer"] {
		t.Error("main.go does not call collector.NewVRAMMeasurer -- the runtime manager would be left with no measurer at all")
	}
}

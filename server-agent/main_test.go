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

// TestGatewayFeaturesWiringReachesTheAgent pins the one line of main.go that
// turns the stable-diffusion.cpp capability probe on. The agent reads a nil
// Deps.GatewayFeatures as "the gateway declares nothing", which is what an
// older gateway expects, so without the line every production agent would
// silently never probe a stable_diffusion_cpp child, and every other test
// would stay green: nothing else runs main(). The agent must also be handed
// the SAME features client the runtime driver polls, so both read one
// gateway's answers through one ETag-conditional client.
//
// Read from this file's AST, like TestMeasurerWiringGoesThroughTheSelector
// above: main() builds everything inline, and extracting the assembly just to
// call it from here would move far more code than the one wire it pins.
func TestGatewayFeaturesWiringReachesTheAgent(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	ident := func(e ast.Expr) string {
		if id, ok := e.(*ast.Ident); ok {
			return id.Name
		}
		return ""
	}
	// calls reports whether e is a call of runtimectl.<name>; proxy has a
	// NewDriver of its own.
	calls := func(e ast.Expr, name string) bool {
		c, ok := e.(*ast.CallExpr)
		if !ok {
			return false
		}
		sel, ok := c.Fun.(*ast.SelectorExpr)
		return ok && ident(sel.X) == "runtimectl" && sel.Sel.Name == name
	}
	var built, polled, wired []string
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != 1 || len(n.Rhs) != 1 {
				return true
			}
			if calls(n.Rhs[0], "NewFeaturesClient") {
				built = append(built, ident(n.Lhs[0]))
			}
			if sel, ok := n.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "GatewayFeatures" {
				wired = append(wired, ident(n.Rhs[0]))
			}
		case *ast.KeyValueExpr:
			if ident(n.Key) == "GatewayFeatures" {
				wired = append(wired, ident(n.Value))
			}
		case *ast.CallExpr:
			// runtimectl.NewDriver(mgr, src, features, reporter, bindHost)
			if calls(n, "NewDriver") && len(n.Args) >= 3 {
				polled = append(polled, ident(n.Args[2]))
			}
		}
		return true
	})
	if len(built) != 1 || built[0] == "" {
		t.Fatalf("main.go builds the features client as %q, want exactly one runtimectl.NewFeaturesClient assigned to a variable", built)
	}
	if len(polled) != 1 || polled[0] != built[0] {
		t.Fatalf("runtimectl.NewDriver is handed %q as its features client, want %q", polled, built[0])
	}
	if len(wired) != 1 || wired[0] != built[0] {
		t.Fatalf("agent.Deps.GatewayFeatures is set from %q in main.go, want exactly %q, the client the runtime driver polls -- "+
			"without it the agent reads the gateway as declaring nothing and never probes a stable_diffusion_cpp child", wired, built[0])
	}
}

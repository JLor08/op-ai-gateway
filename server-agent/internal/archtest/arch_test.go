// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package archtest holds ArchUnit-style dependency-rule tests for the
// op-ai-server-agent module. It contains only _test.go files (see
// TestArchtestPackageIsTestOnly) -- it exists purely to be run by
// `go test ./...`, never imported.
//
// Two kinds of rule live here:
//
//  1. TestAllowlistFrozen compares the CURRENT module-internal import graph
//     against the checked-in allowedDeps literal below. Any import edge
//     that is not already in the allowlist fails the test, naming the
//     exact new edge and the allowedDeps entry to edit.
//
//  2. TestForbiddenEdges asserts specific import directions/leaf
//     properties that must never exist, independent of the allowlist
//     above -- these encode the load-bearing parts of the architecture
//     (mainly: gwapi and certfiles are stdlib-only transport/cert-path
//     contracts, and trust/proxy/certinstall must not depend on each
//     other).
package archtest

import "testing"

// allowedDeps is the checked-in allowlist of op-ai-server-agent
// module-internal import edges. Keys are package paths relative to the
// module root (e.g. "internal/agent"), or mainPkgKey ("main") for the
// module-root main package; values are the OTHER module packages that
// package's PRODUCTION (non-test) code is currently allowed to import.
//
// CONTRACT:
//   - This map must equal reality. TestAllowlistFrozen fails in both
//     directions: a new import not listed here, or a listed import that no
//     longer exists.
//   - To add a new module-internal import edge: add the target to the
//     source package's slice below, in the SAME change that introduces the
//     import.
//   - Do not add edges speculatively. Only list what a package actually
//     imports today.
//   - Leaf packages (no module-internal imports) are still listed with an
//     empty slice, so the map documents the full package universe.
//
// Generated from `go list -f '{{join .Imports "\n"}}' <pkg>` for every
// package under ./internal/... and the module root, filtered to
// op-ai-server-agent/*, on 2026-08-21.
var allowedDeps = map[string][]string{
	mainPkgKey: {
		"internal/agent",
		"internal/certinstall",
		"internal/client",
		"internal/collector",
		"internal/config",
		"internal/proxy",
		"internal/runtime",
		"internal/trust",
	},
	"internal/agent": {
		"internal/certinstall",
		"internal/collector",
		"internal/config",
		"internal/proxy",
		"internal/runtime",
		"internal/sample",
	},
	"internal/certfiles": {},
	"internal/certinstall": {
		"internal/certfiles",
		"internal/gwapi",
	},
	"internal/client": {
		"internal/gwapi",
		"internal/sample",
	},
	"internal/collector": {
		"internal/sample",
	},
	"internal/config": {},
	"internal/gwapi":  {},
	"internal/proxy": {
		"internal/certfiles",
		"internal/gwapi",
	},
	"internal/runtime": {
		"internal/gwapi",
	},
	"internal/sample": {},
	"internal/trust": {
		"internal/certfiles",
		"internal/gwapi",
	},
}

// TestAllowlistFrozen freezes the current module-internal import graph. Any
// new import edge that is not already in allowedDeps fails the test until
// a developer consciously adds it.
func TestAllowlistFrozen(t *testing.T) {
	graph := loadInternalGraph(t)

	// Forward direction: every edge that exists today must be allowed.
	for pkg, imports := range graph {
		allowed := make(map[string]bool, len(allowedDeps[pkg]))
		for _, a := range allowedDeps[pkg] {
			allowed[a] = true
		}
		for _, imp := range imports {
			if !allowed[imp] {
				t.Errorf(
					"NEW import edge %s -> %s is not in the allowedDeps allowlist "+
						"(server-agent/internal/archtest/arch_test.go). "+
						"If this edge is intentional, add %q to allowedDeps[%q]; "+
						"otherwise remove the import from %s.",
					pkg, imp, imp, pkg, pkg,
				)
			}
		}
	}

	// Reverse direction: every allowlist entry must still reflect reality,
	// so removed imports (or renamed/removed packages) don't linger.
	for pkg, allowed := range allowedDeps {
		actual, ok := graph[pkg]
		if !ok {
			t.Errorf("allowedDeps[%q] refers to a package that no longer exists; remove the entry", pkg)
			continue
		}
		actualSet := make(map[string]bool, len(actual))
		for _, a := range actual {
			actualSet[a] = true
		}
		for _, a := range allowed {
			if !actualSet[a] {
				t.Errorf(
					"allowedDeps[%q] lists %q but %s no longer imports it in production code; "+
						"remove it from the allowlist so it stays accurate",
					pkg, a, pkg,
				)
			}
		}
	}
}

// TestForbiddenEdges asserts explicit forbidden import directions and leaf
// properties that encode load-bearing architectural decisions in
// op-ai-server-agent. Unlike TestAllowlistFrozen, these rules are not meant
// to move just because the allowlist above is edited.
//
// Note: "nothing imports package main" is not asserted here because Go's
// compiler already makes it impossible to import a package declared as
// `package main` -- it is enforced automatically, not by this test suite.
func TestForbiddenEdges(t *testing.T) {
	pkgs := loadPackages(t)
	graph := loadInternalGraph(t)

	t.Run("gwapi and certfiles are stdlib-only leaves", func(t *testing.T) {
		// internal/gwapi is the agent<->gateway transport contract and
		// internal/certfiles is the shared cert-path leaf; both must stay
		// free of any dependency (module-internal or third-party) so every
		// other package can depend on them without dragging anything else
		// in.
		for _, pkg := range []string{"internal/gwapi", "internal/certfiles"} {
			info, ok := pkgs[pkg]
			if !ok {
				t.Fatalf("package %q not found in loaded packages", pkg)
			}
			for _, imp := range info.All {
				if isModulePkg(imp) {
					t.Errorf("%s must not import other module packages (found %s); it is a leaf transport/cert-path contract", pkg, imp)
					continue
				}
				if !isStdlib(imp) {
					t.Errorf("%s must be stdlib-only (leaf transport/cert-path contract), found third-party import %s", pkg, imp)
				}
			}
		}
	})

	t.Run("collector does not import proxy, trust, client or agent", func(t *testing.T) {
		toSet := map[string]bool{
			"internal/proxy":  true,
			"internal/trust":  true,
			"internal/client": true,
			"internal/agent":  true,
		}
		for _, e := range graph["internal/collector"] {
			if toSet[e] {
				t.Errorf("forbidden edge internal/collector -> %s: collector reports sampled data upward and must not depend on the proxy/trust/client/agent orchestration layers", e)
			}
		}
	})

	t.Run("sample imports nothing module-internal", func(t *testing.T) {
		// Verified against `go list -f '{{join .Imports "\n"}}'` before
		// encoding: internal/sample has no module-internal edge today, so
		// it is held to the strict leaf rule.
		if edges := graph["internal/sample"]; len(edges) != 0 {
			t.Errorf("internal/sample is documented as a leaf package (no module-internal imports) but imports %v", edges)
		}
	})

	t.Run("trust/proxy/certinstall do not import each other", func(t *testing.T) {
		// internal/trust, internal/proxy and internal/certinstall each
		// depend on the shared leaves internal/certfiles and
		// internal/gwapi, but must not depend on EACH OTHER: certinstall
		// installs a trust anchor into the OS trust store, trust manages
		// the CA/cert material, and proxy terminates TLS using it. Keeping
		// them independent means none of the three needs to know how the
		// other two do their job.
		group := []string{"internal/trust", "internal/proxy", "internal/certinstall"}
		groupSet := make(map[string]bool, len(group))
		for _, p := range group {
			groupSet[p] = true
		}
		for _, from := range group {
			for _, e := range graph[from] {
				if groupSet[e] {
					t.Errorf("forbidden edge %s -> %s: trust/proxy/certinstall must not import each other", from, e)
				}
			}
		}
	})
}

// TestArchtestPackageIsTestOnly asserts that internal/archtest contains no
// non-test Go files. It exists purely to run architecture tests and must
// never itself be an importable production package.
func TestArchtestPackageIsTestOnly(t *testing.T) {
	assertPackageIsTestOnly(t)
}

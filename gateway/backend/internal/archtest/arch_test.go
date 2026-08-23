// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package archtest holds ArchUnit-style dependency-rule tests for the
// op-ai-gateway module. It contains only _test.go files (see
// TestArchtestPackageIsTestOnly) -- it exists purely to be run by
// `go test ./...`, never imported.
//
// Two kinds of rule live here:
//
//  1. TestAllowlistFrozen compares the CURRENT internal->internal (and
//     internal->cmd) import graph against the checked-in allowedDeps
//     literal below. Any import edge that is not already in the allowlist
//     fails the test, with a message naming the exact new edge and the
//     allowedDeps entry to edit. This does not judge whether an edge is
//     good architecture -- it only forces every new edge to be a conscious,
//     reviewed change instead of an accidental one.
//
//  2. TestForbiddenEdges asserts specific import directions that must never
//     exist, independent of the allowlist above. These encode the parts of
//     the architecture considered load-bearing even if allowedDeps is
//     loosened later.
package archtest

import "testing"

// allowedDeps is the checked-in allowlist of op-ai-gateway internal/cmd
// import edges. Keys are package paths relative to the module root (e.g.
// "internal/routing", "cmd/gateway"); values are the OTHER internal/cmd
// packages that package's PRODUCTION (non-test) code is currently allowed
// to import.
//
// CONTRACT:
//   - This map must equal reality. TestAllowlistFrozen fails in both
//     directions: a new import not listed here, or a listed import that no
//     longer exists.
//   - To add a new internal->internal import edge: add the target to the
//     source package's slice below, in the SAME change that introduces the
//     import. That is the whole point of this test -- it forces a
//     reviewer's eye onto every new edge in the dependency graph.
//   - Do not add edges speculatively. Only list what a package actually
//     imports today.
//   - Leaf packages (no internal imports) are still listed with an empty
//     slice, so the map documents the full package universe.
//
// Generated from `go list -f '{{join .Imports "\n"}}' <pkg>` for every
// package under ./internal/... and ./cmd/..., filtered to op-ai-gateway/*,
// on 2026-08-21.
var allowedDeps = map[string][]string{
	"cmd/gateway": {
		"internal/account",
		"internal/auth",
		"internal/capture",
		"internal/certissue",
		"internal/config",
		"internal/gateway",
		"internal/logbuffer",
		"internal/netbird",
		"internal/portal",
		"internal/provider",
		"internal/routing",
		"internal/store",
		"internal/theme",
		"internal/tracing",
		"internal/usage",
	},
	"internal/account": {
		"internal/auth",
		"internal/capture",
		"internal/store",
		"internal/totp",
	},
	"internal/apierror":  {},
	"internal/archtest":  {},
	"internal/auth":      {},
	"internal/capture":   {},
	"internal/certissue": {},
	"internal/compat": {
		"internal/inference",
	},
	"internal/config": {},
	"internal/gateway": {
		"internal/account",
		"internal/apierror",
		"internal/auth",
		"internal/capture",
		"internal/certissue",
		"internal/compat",
		"internal/gateway/visionassets",
		"internal/inference",
		"internal/logbuffer",
		"internal/mail",
		"internal/netbird",
		"internal/ping",
		"internal/portal",
		"internal/provider",
		"internal/routing",
		"internal/store",
		"internal/totp",
		"internal/tracing",
		"internal/usage",
	},
	"internal/gateway/visionassets": {},
	"internal/inference":            {},
	"internal/logbuffer":            {},
	"internal/mail":                 {},
	"internal/netbird":              {},
	"internal/ping":                 {},
	"internal/portal": {
		"internal/auth",
		"internal/capture",
		"internal/certissue",
		"internal/netbird",
		"internal/provider",
		"internal/routing",
		"internal/store",
		"internal/theme",
		"internal/usage",
	},
	"internal/provider": {
		"internal/inference",
		"internal/routing",
		"internal/tracing",
	},
	"internal/routing": {
		"internal/auth",
		"internal/inference",
		"internal/storeerr",
	},
	"internal/store": {
		"internal/auth",
		"internal/routing",
		"internal/storeerr",
		"internal/usage",
	},
	"internal/storeerr": {},
	"internal/theme":    {},
	"internal/totp":     {},
	"internal/tracing": {
		"internal/logbuffer",
		"internal/routing",
	},
	"internal/usage": {},
}

// TestAllowlistFrozen freezes the current internal import graph. Any new
// internal->internal (or internal->cmd) import edge that is not already in
// allowedDeps fails the test until a developer consciously adds it.
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
						"(gateway/backend/internal/archtest/arch_test.go). "+
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

// TestForbiddenEdges asserts explicit forbidden import directions that
// encode load-bearing architectural decisions in op-ai-gateway. Unlike
// TestAllowlistFrozen, these rules are not meant to move just because the
// allowlist above is edited -- they are the invariants the allowlist is not
// allowed to erode.
func TestForbiddenEdges(t *testing.T) {
	graph := loadInternalGraph(t)

	// forbid fails if any package in from imports any package in to.
	forbid := func(t *testing.T, from, to []string, reason string) {
		t.Helper()
		toSet := make(map[string]bool, len(to))
		for _, p := range to {
			toSet[p] = true
		}
		for _, f := range from {
			for _, e := range graph[f] {
				if toSet[e] {
					t.Errorf("forbidden edge %s -> %s: %s", f, e, reason)
				}
			}
		}
	}

	// allowOnlyAmongInternal fails if pkg imports any op-ai-gateway
	// internal/cmd package other than those listed in allowed.
	allowOnlyAmongInternal := func(t *testing.T, pkg string, allowed []string, reason string) {
		t.Helper()
		allowedSet := make(map[string]bool, len(allowed))
		for _, p := range allowed {
			allowedSet[p] = true
		}
		for _, e := range graph[pkg] {
			if !allowedSet[e] {
				t.Errorf("forbidden edge %s -> %s: %s (only %v allowed among internal packages)", pkg, e, reason, allowed)
			}
		}
	}

	t.Run("nothing under internal imports cmd", func(t *testing.T) {
		// The composition root (cmd/gateway) wires the whole application
		// together at the top of the graph; it must stay a pure consumer.
		// If anything under internal/ imported cmd/gateway, main() could no
		// longer be assumed to be the single place assembling the app.
		var internalPkgs []string
		for pkg := range graph {
			if pkg == "internal/archtest" {
				continue // this test package, obviously fine either way
			}
			if len(pkg) >= len("internal/") && pkg[:len("internal/")] == "internal/" {
				internalPkgs = append(internalPkgs, pkg)
			}
		}
		forbid(t, internalPkgs, []string{"cmd/gateway"},
			"cmd/gateway is the composition root; nothing under internal/ may depend on it")
	})

	t.Run("routing is domain-core", func(t *testing.T) {
		// internal/store depends on internal/routing (routing types flow
		// into persistence), never the reverse. internal/portal, gateway
		// and compat sit above routing in the graph and must not be
		// depended on by it either.
		forbid(t, []string{"internal/routing"},
			[]string{"internal/store", "internal/portal", "internal/gateway", "internal/compat"},
			"internal/routing is domain-core; store/portal/gateway/compat depend on routing, never the reverse")
	})

	t.Run("store does not import the http-facing layers", func(t *testing.T) {
		// internal/store is a persistence layer. internal/portal (admin
		// API) and internal/gateway (proxy/serving path) are consumers of
		// store, not dependencies of it.
		forbid(t, []string{"internal/store"},
			[]string{"internal/portal", "internal/gateway"},
			"internal/store is a persistence layer; portal and gateway consume it, they are not consumed by it")
	})

	t.Run("portal does not import gateway or compat", func(t *testing.T) {
		// internal/portal (admin API) must not reach into internal/gateway
		// (the serving/proxy path) or internal/compat (flavor translation)
		// -- those are gateway's concerns, not portal's.
		forbid(t, []string{"internal/portal"},
			[]string{"internal/gateway", "internal/compat"},
			"internal/portal must not depend on internal/gateway or internal/compat; those are the serving path's concerns")
	})

	t.Run("compat stays contained to inference", func(t *testing.T) {
		// internal/compat performs cross-flavor request/response
		// translation and must stay contained: among internal packages it
		// may depend only on internal/inference's shared types, never reach
		// into store/portal/gateway/routing/etc.
		allowOnlyAmongInternal(t, "internal/compat", []string{"internal/inference"},
			"internal/compat (flavor translation) must depend on nothing internal except internal/inference")
	})

	t.Run("tracing does not import its own decorated packages", func(t *testing.T) {
		// Documented cycle-dodge: internal/portal, internal/account and
		// internal/provider each have their OTel tracing decorators living
		// IN those packages (via the OTel global), specifically so
		// internal/tracing never needs to import them back.
		forbid(t, []string{"internal/tracing"},
			[]string{"internal/portal", "internal/account", "internal/provider"},
			"tracing decorators for portal/account/provider live in those packages via the OTel global; internal/tracing must not import them back")
	})

	t.Run("leaf packages import nothing internal", func(t *testing.T) {
		// Verified against `go list -f '{{join .Imports "\n"}}'` for each of
		// these before encoding: none of them has a legit internal edge
		// today, so all six are held to the strict leaf rule rather than
		// added to an exception list.
		leaves := []string{
			"internal/inference",
			"internal/usage",
			"internal/apierror",
			"internal/storeerr",
			"internal/theme",
			"internal/auth",
		}
		for _, leaf := range leaves {
			if edges := graph[leaf]; len(edges) != 0 {
				t.Errorf("%s is documented as a leaf package (no internal imports) but imports %v", leaf, edges)
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

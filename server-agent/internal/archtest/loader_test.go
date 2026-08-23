// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package archtest

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// modulePath is the Go module path for the server agent.
const modulePath = "op-ai-server-agent"

// mainPkgKey is the graph key used for the module-root package (package
// main, at the repo root of server-agent), since it has no "internal/"
// prefix to strip.
const mainPkgKey = "main"

// pkgInfo holds both the filtered (module-internal-only) and the raw (all)
// direct imports of one package, keyed by its module-relative path.
type pkgInfo struct {
	// Internal is the sorted set of other op-ai-server-agent packages this
	// package imports from its production (non-test) source files.
	Internal []string
	// All is the sorted set of every direct import (stdlib, third-party,
	// and module-internal) from its production (non-test) source files.
	All []string
}

// loadPackages loads every package in the op-ai-server-agent module and
// returns, for each package (keyed by its path relative to the module
// root, e.g. "internal/gwapi", or mainPkgKey for the module-root main
// package), its direct PRODUCTION (non-test) imports.
//
// Test-file imports are deliberately excluded: Tests is left false, so
// packages.Load only compiles the non-test variant of each package and
// pkg.Imports reflects only what its *.go (non-_test.go) files import.
func loadPackages(t *testing.T) map[string]pkgInfo {
	t.Helper()

	cfg := &packages.Config{
		Mode:  packages.NeedName | packages.NeedImports | packages.NeedFiles,
		Tests: false,
	}
	pkgs, err := packages.Load(cfg, modulePath+"/...")
	if err != nil {
		t.Fatalf("packages.Load(%s/...): %v", modulePath, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("packages.Load(%s/...) reported %d error(s); see stderr above", modulePath, n)
	}

	result := make(map[string]pkgInfo)
	for _, pkg := range pkgs {
		if !isModulePkg(pkg.PkgPath) {
			continue
		}
		key := relPath(pkg.PkgPath)
		var internal, all []string
		for imp := range pkg.Imports {
			all = append(all, imp)
			if isModulePkg(imp) {
				internal = append(internal, relPath(imp))
			}
		}
		sort.Strings(internal)
		sort.Strings(all)
		result[key] = pkgInfo{Internal: internal, All: all}
	}
	return result
}

// loadInternalGraph is a convenience wrapper over loadPackages that returns
// just the module-internal import edges, keyed the same way.
func loadInternalGraph(t *testing.T) map[string][]string {
	t.Helper()
	pkgs := loadPackages(t)
	graph := make(map[string][]string, len(pkgs))
	for k, v := range pkgs {
		graph[k] = v.Internal
	}
	return graph
}

// isModulePkg reports whether pkgPath is op-ai-server-agent itself or one
// of its sub-packages (as opposed to a third-party or stdlib import).
func isModulePkg(pkgPath string) bool {
	return pkgPath == modulePath || strings.HasPrefix(pkgPath, modulePath+"/")
}

// isStdlib reports whether importPath looks like a standard-library import
// path: stdlib import paths never contain a "." in their first path
// segment (e.g. "context", "net/http"), whereas third-party module paths
// do (e.g. "golang.org/x/tools/go/packages"). isModulePkg has already been
// used to exclude our own module's paths (which also lack a dot) before
// this heuristic is applied.
func isStdlib(importPath string) bool {
	first := importPath
	if i := strings.Index(importPath, "/"); i >= 0 {
		first = importPath[:i]
	}
	return !strings.Contains(first, ".")
}

// relPath strips the module path prefix, e.g.
// "op-ai-server-agent/internal/gwapi" -> "internal/gwapi". The bare module
// path (the module-root main package) maps to mainPkgKey.
func relPath(pkgPath string) string {
	if pkgPath == modulePath {
		return mainPkgKey
	}
	return strings.TrimPrefix(pkgPath, modulePath+"/")
}

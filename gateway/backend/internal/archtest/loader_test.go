// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package archtest

import (
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// modulePath is the Go module path for op-ai-gateway.
const modulePath = "op-ai-gateway"

// loadInternalGraph loads every package in the op-ai-gateway module and
// returns, for each internal/cmd package (keyed by its path relative to the
// module root, e.g. "internal/routing" or "cmd/gateway"), the sorted set of
// other op-ai-gateway internal/cmd packages it imports from its PRODUCTION
// (non-test) source files.
//
// Test-file imports are deliberately excluded: Tests is left false, so
// packages.Load only compiles the non-test variant of each package and
// pkg.Imports reflects only what the package's *.go (non-_test.go) files
// import. This means an architecturally-forbidden import used only in a
// _test.go helper will not trip these tests -- by design, these tests
// freeze the PRODUCTION dependency graph.
func loadInternalGraph(t *testing.T) map[string][]string {
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

	graph := make(map[string][]string)
	for _, pkg := range pkgs {
		if !isModulePkg(pkg.PkgPath) {
			continue
		}
		key := relPath(pkg.PkgPath)
		var edges []string
		for imp := range pkg.Imports {
			if !isModulePkg(imp) {
				continue
			}
			edges = append(edges, relPath(imp))
		}
		sort.Strings(edges)
		graph[key] = edges
	}
	return graph
}

// loadProductionImports returns the sorted import paths of one op-ai-gateway package's
// PRODUCTION (non-test) source files -- every import, standard library and third-party
// included, not only the module's own -- for the rules that constrain more than the
// internal graph. pkg is relative to the module root, e.g. "internal/benchaudio".
func loadProductionImports(t *testing.T, pkg string) []string {
	t.Helper()

	cfg := &packages.Config{Mode: packages.NeedName | packages.NeedImports, Tests: false}
	pkgs, err := packages.Load(cfg, modulePath+"/"+pkg)
	if err != nil {
		t.Fatalf("packages.Load(%s): %v", pkg, err)
	}
	if n := packages.PrintErrors(pkgs); n > 0 {
		t.Fatalf("packages.Load(%s) reported %d error(s); see stderr above", pkg, n)
	}
	if len(pkgs) != 1 {
		t.Fatalf("packages.Load(%s) = %d packages, want exactly one", pkg, len(pkgs))
	}
	imports := make([]string, 0, len(pkgs[0].Imports))
	for imp := range pkgs[0].Imports {
		imports = append(imports, imp)
	}
	sort.Strings(imports)
	return imports
}

// isStdlibPkg reports whether pkgPath is a standard-library package: the go command's
// rule, a path whose first element has no dot -- except this module's own, whose path
// ("op-ai-gateway") has none either.
func isStdlibPkg(pkgPath string) bool {
	first, _, _ := strings.Cut(pkgPath, "/")
	return !strings.Contains(first, ".") && !isModulePkg(pkgPath)
}

// isModulePkg reports whether pkgPath is op-ai-gateway itself or one of its
// internal/cmd sub-packages (as opposed to a third-party or stdlib import).
func isModulePkg(pkgPath string) bool {
	return pkgPath == modulePath || strings.HasPrefix(pkgPath, modulePath+"/")
}

// relPath strips the module path prefix, e.g.
// "op-ai-gateway/internal/routing" -> "internal/routing".
func relPath(pkgPath string) string {
	return strings.TrimPrefix(pkgPath, modulePath+"/")
}

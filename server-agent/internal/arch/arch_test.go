// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package arch_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	// modulePath is the import prefix of the server-agent module.
	modulePath = "github.com/your-org/onprem-ai-gateway/server-agent"
	// siblingModulePath is the other Go module in this repository. The two
	// modules must stay import-isolated.
	siblingModulePath = "github.com/your-org/onprem-ai-gateway/gateway/backend"
)

// skippedDirs are directory names that never contain module packages.
var skippedDirs = map[string]bool{
	".git":         true,
	"bin":          true,
	"dist":         true,
	"node_modules": true,
}

// pkgImports captures the direct imports of one package directory.
type pkgImports struct {
	relDir  string
	main    bool
	imports map[string]bool
}

// sortedImports returns the package imports in a stable order.
func sortedImports(pkg *pkgImports) []string {
	imports := make([]string, 0, len(pkg.imports))
	for imported := range pkg.imports {
		imports = append(imports, imported)
	}
	sort.Strings(imports)
	return imports
}

// moduleRoot walks up from the test working directory to the module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("determine working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

// loadPackages scans the module and maps each package directory (relative
// to the module root, slash-separated) to its direct imports and main flag.
func loadPackages(t *testing.T) map[string]*pkgImports {
	t.Helper()
	root := moduleRoot(t)
	fset := token.NewFileSet()
	packages := map[string]*pkgImports{}

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && skippedDirs[entry.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return fmt.Errorf("parse %s: %w", path, parseErr)
		}
		relDir, relErr := filepath.Rel(root, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		relDir = filepath.ToSlash(relDir)
		pkg := packages[relDir]
		if pkg == nil {
			pkg = &pkgImports{relDir: relDir, imports: map[string]bool{}}
			packages[relDir] = pkg
		}
		if !strings.HasSuffix(path, "_test.go") && file.Name.Name == "main" {
			pkg.main = true
		}
		for _, spec := range file.Imports {
			imported, unquoteErr := strconv.Unquote(spec.Path.Value)
			if unquoteErr != nil {
				return unquoteErr
			}
			pkg.imports[imported] = true
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("scan module: %v", walkErr)
	}
	return packages
}

// isStdlib reports whether an import path points into the standard library.
// Standard library first path elements never contain a dot.
func isStdlib(imported string) bool {
	first := imported
	if index := strings.Index(imported, "/"); index >= 0 {
		first = imported[:index]
	}
	return !strings.Contains(first, ".")
}

// TestPublicPackagesOnlyDependOnStdlibAndPublicPackages enforces the
// layering of the public API surface: pkg/* may only depend on the standard
// library and other public packages, never on internal or command code.
func TestPublicPackagesOnlyDependOnStdlibAndPublicPackages(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		if !strings.HasPrefix(pkg.relDir, "pkg/") {
			continue
		}
		for _, imported := range sortedImports(pkg) {
			if isStdlib(imported) || strings.HasPrefix(imported, modulePath+"/pkg/") {
				continue
			}
			t.Errorf("package %s imports %s: public packages may only depend on the standard library and other public packages", pkg.relDir, imported)
		}
	}
}

// TestOnlyCommandPackagesDeclareMain enforces that executable entry points
// live under cmd/ and that every package under cmd/ is a main package.
func TestOnlyCommandPackagesDeclareMain(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		inCmd := strings.HasPrefix(pkg.relDir, "cmd/")
		if pkg.main && !inCmd {
			t.Errorf("package %s declares main outside of cmd/", pkg.relDir)
		}
		if inCmd && !pkg.main {
			t.Errorf("package %s is under cmd/ but does not declare main", pkg.relDir)
		}
	}
}

// TestModuleBoundaryAgainstSiblingModule enforces that the server-agent
// module never imports the gateway backend module.
func TestModuleBoundaryAgainstSiblingModule(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		for _, imported := range sortedImports(pkg) {
			if strings.HasPrefix(imported, siblingModulePath) {
				t.Errorf("package %s imports %s: the repository modules must stay import-isolated", pkg.relDir, imported)
			}
		}
	}
}

// TestNoExternalDependencies enforces the zero external dependency policy:
// every import must be the standard library or this module.
func TestNoExternalDependencies(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		for _, imported := range sortedImports(pkg) {
			if isStdlib(imported) || strings.HasPrefix(imported, modulePath) {
				continue
			}
			t.Errorf("package %s imports %s: this module must not depend on external modules", pkg.relDir, imported)
		}
	}
}

// TestReportingCollectorStaysTransportFree enforces that the telemetry
// collector is transport-agnostic: HTTP delivery stays in cmd/agent.
func TestReportingCollectorStaysTransportFree(t *testing.T) {
	for _, pkg := range loadPackages(t) {
		if pkg.relDir != "pkg/reporting" {
			continue
		}
		for _, imported := range sortedImports(pkg) {
			if strings.HasPrefix(imported, "net/") {
				t.Errorf("package %s imports %s: the reporting collector must not depend on networking", pkg.relDir, imported)
			}
		}
	}
}

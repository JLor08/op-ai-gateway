// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package archtest

import (
	"os"
	"strings"
	"testing"
)

// assertPackageIsTestOnly scans the archtest package's own directory and
// fails the test if it contains any .go file that is not a _test.go file.
// This package must stay pure-test: it is loaded by loadInternalGraph like
// any other package, and it must never become a production dependency
// something else could import.
func assertPackageIsTestOnly(t *testing.T) {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	entries, err := os.ReadDir(wd)
	if err != nil {
		t.Fatalf("os.ReadDir(%s): %v", wd, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go") {
			t.Errorf("internal/archtest must contain only _test.go files (pure test package), found non-test file %q", name)
		}
	}
}

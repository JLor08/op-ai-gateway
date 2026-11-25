// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"os"
	"path/filepath"
	"testing"
)

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func TestWriteTreeCreatesAllThreeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTree(dir, 42); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, DomainDir)
	if got := readFile(t, filepath.Join(d, "name")); got != "package-0\n" {
		t.Errorf("name = %q", got)
	}
	if got := readFile(t, filepath.Join(d, "max_energy_range_uj")); got != "9223372036854775807\n" {
		t.Errorf("max_energy_range_uj = %q", got)
	}
	if got := readFile(t, filepath.Join(d, "energy_uj")); got != "42\n" {
		t.Errorf("energy_uj = %q", got)
	}
}

func TestWriteTreeLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	for _, v := range []uint64{1, 2} {
		if err := WriteTree(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, DomainDir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{"energy_uj", "max_energy_range_uj", "name"}
	if len(names) != len(want) {
		t.Fatalf("entries = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entries = %v, want %v", names, want)
		}
	}
}

func TestReadCounterRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTree(dir, 7_000_000); err != nil {
		t.Fatal(err)
	}
	if got := ReadCounter(dir); got != 7_000_000 {
		t.Errorf("ReadCounter = %d", got)
	}
}

func TestReadCounterCorruptOrMissingIsZero(t *testing.T) {
	dir := t.TempDir()
	if got := ReadCounter(dir); got != 0 {
		t.Errorf("missing: %d", got)
	}
	d := filepath.Join(dir, DomainDir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"", "garbage\n"} {
		if err := os.WriteFile(filepath.Join(d, "energy_uj"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := ReadCounter(dir); got != 0 {
			t.Errorf("content %q: %d", content, got)
		}
	}
}

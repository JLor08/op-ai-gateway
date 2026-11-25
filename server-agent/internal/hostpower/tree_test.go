// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"os"
	"path/filepath"
	"strings"
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
	if got := readFile(t, filepath.Join(d, "max_energy_range_uj")); got != "0\n" {
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

// symlinkOrSkip creates a symlink, skipping the test where the OS refuses
// (Windows without the privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

func replaceWithSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.RemoveAll(link); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, target, link)
}

func TestWriteTreeRefusesDomainSymlinkToAnotherVM(t *testing.T) {
	out := t.TempDir()
	vm100, vm101 := filepath.Join(out, "100"), filepath.Join(out, "101")
	for _, d := range []string{vm100, vm101} {
		if err := WriteTree(d, 5); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{
		filepath.Join("..", "101", DomainDir), // relative
		filepath.Join(vm101, DomainDir),       // absolute
	} {
		replaceWithSymlink(t, target, filepath.Join(vm100, DomainDir))
		if err := WriteTree(vm100, 999); err == nil {
			t.Errorf("target %s: WriteTree followed an escaping symlink", target)
		}
		if got := readFile(t, filepath.Join(vm101, DomainDir, "energy_uj")); got != "5\n" {
			t.Errorf("target %s: other VM energy_uj = %q, want 5", target, got)
		}
		if got := ReadCounter(vm100); got != 0 {
			t.Errorf("target %s: ReadCounter followed the symlink: %d", target, got)
		}
	}
}

func TestWriteTreeCreatesNothingThroughSymlinkOutsideOut(t *testing.T) {
	out, elsewhere := t.TempDir(), t.TempDir()
	vm := filepath.Join(out, "100")
	if err := os.MkdirAll(vm, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, elsewhere, filepath.Join(vm, DomainDir))
	if err := WriteTree(vm, 7); err == nil {
		t.Error("WriteTree followed a symlink outside the VM dir")
	}
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("created outside the VM dir: %v", entries)
	}
}

func TestEnergyFileSymlinkOutsideIsNeitherReadNorWritten(t *testing.T) {
	out, elsewhere := t.TempDir(), t.TempDir()
	vm := filepath.Join(out, "100")
	if err := WriteTree(vm, 1); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(elsewhere, "counter")
	if err := os.WriteFile(foreign, []byte("123456789\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replaceWithSymlink(t, foreign, filepath.Join(vm, DomainDir, "energy_uj"))
	if got := ReadCounter(vm); got != 0 {
		t.Errorf("ReadCounter = %d, want 0 (escaping symlink)", got)
	}
	// rename replaces the link itself, so the write succeeds inside the VM dir
	// and the foreign file stays untouched.
	if err := WriteTree(vm, 42); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, foreign); got != "123456789\n" {
		t.Errorf("foreign file = %q, modified through the symlink", got)
	}
	if got := readFile(t, filepath.Join(vm, DomainDir, "energy_uj")); got != "42\n" {
		t.Errorf("energy_uj = %q", got)
	}
}

func TestSymlinkInsideVMDirIsHarmless(t *testing.T) {
	vm := filepath.Join(t.TempDir(), "100")
	if err := os.MkdirAll(filepath.Join(vm, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "real", filepath.Join(vm, DomainDir))
	if err := WriteTree(vm, 9); err != nil {
		t.Fatalf("in-dir symlink: %v", err)
	}
	if got := readFile(t, filepath.Join(vm, "real", "energy_uj")); got != "9\n" {
		t.Errorf("energy_uj = %q", got)
	}
	if got := ReadCounter(vm); got != 9 {
		t.Errorf("ReadCounter = %d", got)
	}
}

func TestWriteTreeRefusesSymlinkedVMDir(t *testing.T) {
	out, elsewhere := t.TempDir(), t.TempDir()
	symlinkOrSkip(t, elsewhere, filepath.Join(out, "100"))
	if err := WriteTree(filepath.Join(out, "100"), 1); err == nil {
		t.Error("WriteTree accepted a symlinked VM dir")
	}
	if entries, _ := os.ReadDir(elsewhere); len(entries) != 0 {
		t.Errorf("created outside: %v", entries)
	}
}

func TestRemoveStaleTemps(t *testing.T) {
	out, elsewhere := t.TempDir(), t.TempDir()
	vm := filepath.Join(out, "100")
	if err := WriteTree(vm, 3); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(vm, DomainDir)
	for _, n := range []string{".energy_uj.123", ".name.abc", ".max_energy_range_uj.x", ".keep", "notes"} {
		if err := os.WriteFile(filepath.Join(d, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A second VM whose domain dir escapes: its target must stay untouched.
	stray := filepath.Join(elsewhere, ".energy_uj.999")
	if err := os.WriteFile(stray, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, "101"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, elsewhere, filepath.Join(out, "101", DomainDir))

	if err := RemoveStaleTemps(out); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	want := []string{".keep", "energy_uj", "max_energy_range_uj", "name", "notes"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", names, want)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("removed through an escaping symlink: %v", err)
	}
}

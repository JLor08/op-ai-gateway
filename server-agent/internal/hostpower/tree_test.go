// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// within fails the test when fn does not return within 2 s (a blocked read).
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked for more than 2s")
	}
}

func mkfifoOrSkip(t *testing.T, p string) {
	t.Helper()
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	if err := mkfifo(p); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
}

func TestWriteTreeReplacesFIFOWithoutBlocking(t *testing.T) {
	vm := filepath.Join(t.TempDir(), "100")
	if err := WriteTree(vm, 1); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(vm, DomainDir, "name")
	mkfifoOrSkip(t, name)
	var err error
	within(t, func() { err = WriteTree(vm, 2) })
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(name)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("name after WriteTree: %v %v, want a regular file", fi, err)
	}
	if got := readFile(t, name); got != "package-0\n" {
		t.Errorf("name = %q", got)
	}
}

func TestWriteTreeReplacesOversizedStaticFile(t *testing.T) {
	vm := filepath.Join(t.TempDir(), "100")
	if err := WriteTree(vm, 1); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(vm, DomainDir, "max_energy_range_uj")
	if err := os.WriteFile(p, []byte(strings.Repeat("0", maxTreeFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteTree(vm, 2); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, p); got != "0\n" {
		t.Errorf("max_energy_range_uj = %q", got)
	}
}

func TestReadCounterRejectsOversizedFile(t *testing.T) {
	vm := filepath.Join(t.TempDir(), "100")
	if err := WriteTree(vm, 1); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(vm, DomainDir, "energy_uj")
	if err := os.WriteFile(p, []byte(strings.Repeat("1", maxTreeFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadCounter(vm); got != 0 {
		t.Errorf("ReadCounter = %d, want 0 (oversized)", got)
	}
	// At the cap it is still read (and then rejected only if unparsable).
	if err := os.WriteFile(p, []byte("123\n"+strings.Repeat(" ", maxTreeFileBytes-4)), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadCounter(vm); got != 123 {
		t.Errorf("ReadCounter at the cap = %d, want 123", got)
	}
}

func TestReadCounterFIFOReturnsZeroWithoutBlocking(t *testing.T) {
	vm := filepath.Join(t.TempDir(), "100")
	if err := WriteTree(vm, 1); err != nil {
		t.Fatal(err)
	}
	mkfifoOrSkip(t, filepath.Join(vm, DomainDir, "energy_uj"))
	var got uint64
	within(t, func() { got = ReadCounter(vm) })
	if got != 0 {
		t.Errorf("ReadCounter = %d, want 0", got)
	}
}

// TestNonblockOpenOfFIFODoesNotBlock covers the Lstat-to-open race in
// readSmall, which cannot be staged deterministically: a FIFO swapped in after
// the Lstat is opened with the same flags readSmall uses, so the open returns
// at once and the post-open Stat sees a non-regular file.
func TestNonblockOpenOfFIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	mkfifoOrSkip(t, filepath.Join(dir, "x"))
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	within(t, func() {
		f, err := root.OpenFile("x", os.O_RDONLY|oNonblock, 0)
		if err != nil {
			t.Errorf("open: %v", err)
			return
		}
		defer f.Close()
		if fi, err := f.Stat(); err != nil || fi.Mode().IsRegular() {
			t.Errorf("Stat after open = %v, %v; want a non-regular file", fi, err)
		}
	})
}

func TestRemoveStaleTempsSkipsFIFODomain(t *testing.T) {
	out := t.TempDir()
	vm := filepath.Join(out, "100")
	if err := os.MkdirAll(vm, 0o755); err != nil {
		t.Fatal(err)
	}
	mkfifoOrSkip(t, filepath.Join(vm, DomainDir))
	var err error
	within(t, func() { err = RemoveStaleTemps(out) })
	if err != nil {
		t.Fatal(err)
	}
}

func TestWriteTreeWritesWindowsTree(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTree(dir, 42); err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(dir, WindowsDir, WindowsDomainDir)
	for name, want := range map[string]string{
		"name":                "package-0\n",
		"max_energy_range_uj": "0\n",
		"energy_uj":           "42\n",
	} {
		if got := readFile(t, filepath.Join(d, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestWindowsTreeHasNoColonNames(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTree(dir, 1); err != nil {
		t.Fatal(err)
	}
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, WindowsDir), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		n++
		if strings.Contains(d.Name(), ":") {
			t.Errorf("entry %s contains a colon", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 5 {
		t.Errorf("walked %d entries, want the windows dir, the domain dir and three files", n)
	}
}

// assertWindowsEscapeContained checks that WriteTree failed on a broken
// Windows subtree, left outside untouched and still wrote the Linux tree.
func assertWindowsEscapeContained(t *testing.T, dir, outside string) {
	t.Helper()
	if err := WriteTree(dir, 42); err == nil {
		t.Error("WriteTree followed an escaping Windows symlink")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("created outside: %v", entries)
	}
	if got := readFile(t, filepath.Join(dir, DomainDir, "energy_uj")); got != "42\n" {
		t.Errorf("Linux energy_uj = %q, want 42", got)
	}
}

func TestWindowsSubtreeSymlinkEscapesNothing(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	symlinkOrSkip(t, outside, filepath.Join(dir, WindowsDir))
	assertWindowsEscapeContained(t, dir, outside)
}

func TestWindowsDomainSymlinkEscapesNothing(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, WindowsDir), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, outside, filepath.Join(dir, WindowsDir, WindowsDomainDir))
	assertWindowsEscapeContained(t, dir, outside)
}

func TestRemoveStaleTempsSweepsWindowsTree(t *testing.T) {
	out := t.TempDir()
	vm := filepath.Join(out, "100")
	if err := WriteTree(vm, 3); err != nil {
		t.Fatal(err)
	}
	dirs := []string{filepath.Join(vm, DomainDir), filepath.Join(vm, WindowsDir, WindowsDomainDir)}
	for _, d := range dirs {
		if err := os.WriteFile(filepath.Join(d, ".energy_uj.abc"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveStaleTemps(out); err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		if _, err := os.Stat(filepath.Join(d, ".energy_uj.abc")); !os.IsNotExist(err) {
			t.Errorf("%s: stale temp not removed (err=%v)", d, err)
		}
		if got := readFile(t, filepath.Join(d, "energy_uj")); got != "3\n" {
			t.Errorf("%s: energy_uj = %q", d, got)
		}
	}
}

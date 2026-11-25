// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package hostpower

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDiscoverVMsSortedNumericOnly(t *testing.T) {
	root := t.TempDir()
	slice := filepath.Join(root, "qemu.slice")
	for _, n := range []string{"101.scope", "100.scope", "99.scope", "foo.scope", "notascope"} {
		if err := os.MkdirAll(filepath.Join(slice, n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A regular file named like a scope is ignored.
	if err := os.WriteFile(filepath.Join(slice, "102.scope"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := DiscoverVMs(root)
	want := []string{"99", "100", "101"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiscoverVMs = %v, want %v", got, want)
	}
}

func TestDiscoverVMsNoSlice(t *testing.T) {
	if got := DiscoverVMs(t.TempDir()); len(got) != 0 {
		t.Errorf("DiscoverVMs = %v, want empty", got)
	}
}

func TestReadVMUsageUsec(t *testing.T) {
	root := t.TempDir()
	scope := filepath.Join(root, "qemu.slice", "100.scope")
	if err := os.MkdirAll(scope, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, "cpu.stat"), []byte("usage_usec 555\nuser_usec 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, ok := ReadVMUsageUsec(root, "100"); !ok || v != 555 {
		t.Errorf("got %d, %v", v, ok)
	}
	if _, ok := ReadVMUsageUsec(root, "101"); ok {
		t.Error("missing scope should be !ok")
	}
}

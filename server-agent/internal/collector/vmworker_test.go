// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"runtime"
	"slices"
	"testing"
)

// TestFilterVMWorkerPIDs: the VM worker is matched by its exact image name,
// in any case (Windows file names are case-insensitive), and nothing that
// merely starts with it -- vmmem is the VM's memory process, not its worker,
// and holds none of its GPU memory (Phase 0, B1). The answer is sorted.
func TestFilterVMWorkerPIDs(t *testing.T) {
	got := filterVMWorkerPIDs(map[int]string{10: "vmwp.exe", 11: "VMWP.EXE", 12: "vmmem", 13: "vmwp.exe.bak"})
	if want := []int{10, 11}; !slices.Equal(got, want) {
		t.Errorf("filterVMWorkerPIDs = %v, want %v", got, want)
	}
	if got := filterVMWorkerPIDs(map[int]string{4: "System", 12: "vmmem"}); len(got) != 0 {
		t.Errorf("no VM worker: filterVMWorkerPIDs = %v, want none", got)
	}
	if got := filterVMWorkerPIDs(map[int]string{0: "vmwp.exe"}); len(got) != 0 {
		t.Errorf("pid 0 is the idle process, never a VM worker: filterVMWorkerPIDs = %v", got)
	}
}

// TestNewWSLVMWorkerPIDsOffWindows: there is no WSL VM off Windows, so no
// finder -- the manager then never reads a VM figure at all.
func TestNewWSLVMWorkerPIDsOffWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows finder enumerates the real process table")
	}
	if NewWSLVMWorkerPIDs() != nil {
		t.Error("NewWSLVMWorkerPIDs() off Windows is non-nil, want nil")
	}
}

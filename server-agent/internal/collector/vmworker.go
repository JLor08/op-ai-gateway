// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"slices"
	"strings"
)

// vmWorkerImage is the image name of the Hyper-V VM worker process, the one
// Windows charges a WSL2 VM's GPU memory to: every guest process with a GPU
// context appears in the PDH \GPU Process Memory counters as an instance named
// after this process's pid (Phase 0, B1/B2b of #381). vmmem, the VM's memory
// process, holds none of it.
const vmWorkerImage = "vmwp.exe"

// filterVMWorkerPIDs returns, sorted, the pids in procs (pid -> image name)
// whose image is the VM worker. Windows file names are case-insensitive, so
// the name is compared that way; it must match whole, since a process merely
// named like the worker is someone else's. pid 0, the idle process, is never
// one. This is the pure half of NewWSLVMWorkerPIDs, tested on every platform;
// the process table it filters is read by the Windows-only half.
//
// EVERY worker is returned, not the WSL VM's alone. Each Hyper-V VM has a
// worker of its own, and nothing in a process listing tells the WSL VM's apart
// (open question 1 of #381). The manager sums them all, so another VM's GPU
// memory is charged as part of the WSL VM's unattributed remainder: admission
// then sees less free VRAM than there is, never more.
func filterVMWorkerPIDs(procs map[int]string) []int {
	var out []int
	for pid, name := range procs {
		if pid > 0 && strings.EqualFold(name, vmWorkerImage) {
			out = append(out, pid)
		}
	}
	slices.Sort(out)
	return out
}

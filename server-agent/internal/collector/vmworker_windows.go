// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

import (
	"errors"
	"log/slog"
	"unsafe"

	"golang.org/x/sys/windows"
)

// NewWSLVMWorkerPIDs returns the finder runtime.Manager.SetWSLVMPIDs takes:
// the pids of every running Hyper-V VM worker (vmwp.exe), which the manager
// hands the VRAM measurer beside its own processes' pids, because Windows
// charges all of the WSL VM's GPU memory to that worker (#381). See
// filterVMWorkerPIDs for why every worker is returned.
//
// The process table is read with a Toolhelp snapshot, which names each
// process's image without opening the process: the worker runs as the VM's
// own virtual account, which an unelevated agent could not open. One snapshot
// is a single in-process syscall, cheap enough for the admission path that
// calls it. A snapshot that fails answers no worker and logs at debug level,
// like a failed PDH read: the VM figure then reads as empty for that one
// reading.
func NewWSLVMWorkerPIDs() func() []int {
	return vmWorkerPIDs
}

func vmWorkerPIDs() []int {
	procs, err := processImageNames()
	if err != nil {
		slog.Debug("vm worker process listing failed", "err", err)
		return nil
	}
	return filterVMWorkerPIDs(procs)
}

// processImageNames is the syscall half: every process in a Toolhelp
// snapshot, pid -> image file name (no path).
func processImageNames() (map[int]string, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // status ignored: nothing to do about a failed close

	e := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	out := make(map[int]string)
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		out[int(e.ProcessID)] = windows.UTF16ToString(e.ExeFile[:])
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	return out, nil
}

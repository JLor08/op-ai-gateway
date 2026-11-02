// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

import (
	"log/slog"
	"math"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This file is the Windows half of the per-process host-RAM measurer
// (runtime.Manager.SetRAMMeasurer's func shape: the manager's own live
// managed-pid set in, pid -> MB out). Its non-Windows counterpart is
// ram_measurer_other.go.
//
// THE METRIC IS THE PRIVATE WORKING SET, not the plain working set. On
// Windows the working set (what gopsutil reports as RSS, and what the other
// platforms' RSS corresponds to) also counts every SHAREABLE page the process
// has resident -- each loaded DLL's code, mapped system files -- and those are
// charged once to every process that maps them. The private working set is the
// part of the working set that belongs to this process alone, and it is the
// number Task Manager's "Memory (active private working set)" column shows.
// That makes it the closer analogue of what the Unix RSS measures for the
// purpose this number serves, which is host-RAM PLACEMENT: how much physical
// memory does starting or evicting this one process add or free? A model
// server's weights and KV cache are private (or mmap'd file pages it alone
// maps), while the CUDA/ggml DLL footprint is shared system overhead that
// summing plain working sets across co-resident models would count repeatedly.
// This mirrors the VRAM measurer's own Windows/other split: the platform's
// first-choice mechanism (nvidia-smi, RSS) is wrong or unavailable under
// Windows' memory model, so Windows gets the metric that is right for it.
//
// MECHANISM: GetProcessMemoryInfo (psapi) with the PROCESS_MEMORY_COUNTERS_EX2
// layout, which adds PrivateWorkingSetSize to the older PROCESS_MEMORY_COUNTERS
// fields, per pid, through a PROCESS_QUERY_LIMITED_INFORMATION handle (the
// weakest access right that works, and one the agent always holds on the model
// children it spawned itself). It is a direct per-process read: no counter
// instance names to join back to pids (the `\Process(*)\Working Set - Private`
// PDH counter is keyed by IMAGE NAME with a "#N" suffix, so mapping it to a pid
// needs a second counter and a join), no subprocess, no shell. It runs on the
// manager's off-owner measurement goroutine, so a handful of in-process
// syscalls per beat is far below anything that matters. It follows the same
// system-DLL-only loading rule and up-front export resolution as the VRAM
// measurer (nvidia_pdh_windows.go, which carries the reasoning).
//
// EX2 IS WINDOWS 10 VERSION 1809 AND LATER. On an older build GetProcessMemoryInfo
// either fails or leaves the EX2-only field at 0; both read as "this pid could
// not be measured" below and are skipped, so such a host falls back to the
// operator's RAM estimate exactly as a host with no measurer does -- never to a
// wrong number and never to a 0 (see measureProcessPrivateWorkingSet).
//
// This file is the SYSCALL half and is reviewed rather than tested: CI
// (ubuntu-latest) never compiles, vets or runs it. `GOOS=windows go vet ./...`
// compiles it on any machine, and the layout assertions below turn a wrong
// struct into a compile error there instead of a silently garbled reading.
//
// Same contract as the non-Windows measurer (ram_measurer_other.go): a pid it
// cannot read is ABSENT, never present with 0; nothing measurable returns nil.

// psapi.dll loads from System32 only (windows.NewLazySystemDLL), the same rule
// and the same reason as pdh.dll in nvidia_pdh_windows.go: a bare-name load
// starts at the application directory, and this DLL runs in the process that
// spawns the model children and holds the gateway mTLS client identity.
var (
	psapiDLL                 = windows.NewLazySystemDLL("psapi.dll")
	procGetProcessMemoryInfo = psapiDLL.NewProc("GetProcessMemoryInfo")
)

// processMemoryCountersEx2 mirrors PROCESS_MEMORY_COUNTERS_EX2 (psapi.h), the
// buffer GetProcessMemoryInfo fills:
//
//	DWORD   cb;                          //  0
//	DWORD   PageFaultCount;              //  4
//	SIZE_T  PeakWorkingSetSize;          //  8
//	SIZE_T  WorkingSetSize;              // 16
//	SIZE_T  QuotaPeakPagedPoolUsage;     // 24
//	SIZE_T  QuotaPagedPoolUsage;         // 32
//	SIZE_T  QuotaPeakNonPagedPoolUsage;  // 40
//	SIZE_T  QuotaNonPagedPoolUsage;      // 48
//	SIZE_T  PagefileUsage;               // 56
//	SIZE_T  PeakPagefileUsage;           // 64
//	SIZE_T  PrivateUsage;                // 72
//	SIZE_T  PrivateWorkingSetSize;       // 80
//	ULONG64 SharedCommitUsage;           // 88   (total 96)
//
// Only Cb (which the caller must set to the structure's size) and
// PrivateWorkingSetSize are read or written here, so everything between them
// and the trailing SharedCommitUsage is blank padding that keeps the offsets
// honest. SIZE_T is pointer-sized, hence uintptr, which is also why a 32-bit
// Windows target fails the assertions below rather than mis-reading the
// struct -- the same intended outcome as the PDH structs' (8-byte pointer
// assumed for amd64 and arm64, the only Windows binaries the agent ships).
type processMemoryCountersEx2 struct {
	Cb                    uint32
	_                     [76]byte // PageFaultCount .. PrivateUsage, offsets 4..80
	PrivateWorkingSetSize uintptr
	_                     uint64 // SharedCommitUsage
}

// Compile-time layout assertions, in the form and for the reason set out above
// the PDH structs' block in nvidia_pdh_windows.go: `uint(x - N)` and
// `uint(N - x)` together are a compile error unless x == N exactly. The total
// size is pinned because the blank fields cannot be asserted by name, and each
// field that IS named gets its offset and width. A wrong cb is not harmless:
// GetProcessMemoryInfo writes cb bytes, so an over-stated size would let the
// kernel write past the Go struct.
const (
	_ = uint(unsafe.Sizeof(processMemoryCountersEx2{}) - 96)
	_ = uint(96 - unsafe.Sizeof(processMemoryCountersEx2{}))
	_ = uint(unsafe.Offsetof(processMemoryCountersEx2{}.Cb) - 0)
	_ = uint(0 - unsafe.Offsetof(processMemoryCountersEx2{}.Cb))
	_ = uint(unsafe.Sizeof(processMemoryCountersEx2{}.Cb) - 4)
	_ = uint(4 - unsafe.Sizeof(processMemoryCountersEx2{}.Cb))
	_ = uint(unsafe.Offsetof(processMemoryCountersEx2{}.PrivateWorkingSetSize) - 80)
	_ = uint(80 - unsafe.Offsetof(processMemoryCountersEx2{}.PrivateWorkingSetSize))
	_ = uint(unsafe.Sizeof(processMemoryCountersEx2{}.PrivateWorkingSetSize) - 8)
	_ = uint(8 - unsafe.Sizeof(processMemoryCountersEx2{}.PrivateWorkingSetSize))
)

// NewRAMMeasurer returns the Windows per-process private-working-set measurer
// for runtime.Manager.SetRAMMeasurer, or nil when this host cannot support it
// (the psapi export is missing). nil leaves the manager reporting no measured
// RAM, exactly as it behaves with no measurer installed.
func NewRAMMeasurer() func(pids []int) map[int]int {
	// Resolve the one export up front, so a host without it gets NO measurer
	// rather than one that panics on its first call: a LazyProc resolves
	// lazily and panics from Call when the export is absent, which on the
	// manager's measurement goroutine would take the agent down.
	if err := procGetProcessMemoryInfo.Find(); err != nil {
		slog.Debug("windows process RAM measurer unavailable", "proc", procGetProcessMemoryInfo.Name, "err", err)
		return nil
	}
	return measureProcessPrivateWorkingSet
}

// measureProcessPrivateWorkingSet is the func(pids []int) map[int]int
// NewRAMMeasurer hands out. See NewRAMMeasurer for the metric and contract.
//
// A pid it cannot open or read (exited since the manager listed it, or access
// denied) is skipped, and so is one whose private working set comes back 0 or
// rounds to under 1 MB: the manager reads a present key as a measurement, and a
// measured 0 means "unknown", not "needs no RAM" -- see runtime.realRAM.
func measureProcessPrivateWorkingSet(pids []int) map[int]int {
	var out map[int]int
	for _, pid := range pids {
		bytes, ok := privateWorkingSetBytes(pid)
		if !ok {
			continue
		}
		mb := int(int64(bytes) / bytesPerMB)
		if mb <= 0 {
			continue
		}
		if out == nil {
			out = make(map[int]int, len(pids))
		}
		out[pid] = mb
	}
	return out
}

// privateWorkingSetBytes reads one process's private working set in bytes.
// (0, false) for anything that is not a usable reading; none of those is a
// fault, so none is logged above debug level, and this one logs nothing --
// the exit race would make a per-pid line noise on every beat.
//
// pid <= 0 is rejected before the OS is asked: pid 0 is the System Idle
// Process, whose "memory" is not any managed model's.
func privateWorkingSetBytes(pid int) (uint64, bool) {
	if pid <= 0 || pid > math.MaxUint32 {
		return 0, false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // status ignored: nothing to do about a failed close

	c := processMemoryCountersEx2{Cb: uint32(unsafe.Sizeof(processMemoryCountersEx2{}))}
	// BOOL return: 0 is failure. On success the kernel has written c.Cb bytes
	// into c, which is why the struct's size is pinned above.
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&c)), uintptr(c.Cb)); r == 0 {
		return 0, false
	}
	if c.PrivateWorkingSetSize == 0 {
		return 0, false // pre-EX2 build left the field unwritten: see NewRAMMeasurer
	}
	return uint64(c.PrivateWorkingSetSize), true
}

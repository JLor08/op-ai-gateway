// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package collector

import (
	"log/slog"
	"math"
	"os"

	"github.com/shirou/gopsutil/v4/process"
)

// NewRAMMeasurer returns the per-process host-RAM measurer for every platform
// except Windows (runtime.Manager.SetRAMMeasurer's func shape: the manager's
// own live managed-pid set in, pid -> resident MB out), or nil when this host
// cannot read process memory at all.
//
// THE METRIC IS RSS, the resident set size gopsutil reads from the OS process
// table (/proc/<pid>/statm on Linux, the task info on macOS). RSS counts the
// pages a process currently has in physical memory, including file-backed
// pages it has mapped -- and for a model server that is the right thing to
// count: llama.cpp and its relatives mmap the weights, so the bulk of a model's
// host-RAM footprint is exactly resident mmap'd file pages that a
// private-memory metric would miss. The accepted caveat is that such pages are
// page cache the kernel could reclaim under pressure, so RSS can overstate what
// a process strictly NEEDS; for admission that errs on the safe side (a model
// is charged for what it occupies), which is the direction the VRAM budget
// errs in as well. The Windows file measures private working set instead, for
// the reason given there.
//
// Contract, matching the VRAM measurer (measurer_other.go) except that RAM has
// no per-GPU axis, so the answer is one number per pid:
//
//   - a pid the measurer could not read (exited between the manager's list and
//     this call, or not readable by the agent) is ABSENT from the result,
//     never present with 0. The manager reads a present key as a measurement,
//     and a measured 0 means "unknown", not "needs no RAM" -- see
//     runtime.realRAM, which enforces the same rule at the near end of the
//     loop. A process resident in under 1 MB rounds to 0 and is dropped for the
//     same reason.
//   - nothing measurable at all returns nil, not an empty map.
//   - it is stateless and so safe to call from the manager's off-owner
//     measurement goroutine while another call is in flight.
//
// nil means "measurement is unavailable on this host", decided once, here: the
// measurer must be able to read the agent's OWN process, which is the cheapest
// proof that the OS exposes per-process memory to this binary at all (a
// container without /proc mounted is the realistic failure). nil leaves the
// manager reporting no measured RAM, exactly as it behaves with no measurer.
//
// Follows the measurer_windows.go / measurer_other.go pattern: one exported
// name, one implementation per platform, the choice made by the build tag.
func NewRAMMeasurer() func(pids []int) map[int]int {
	if _, ok := processRSSBytes(os.Getpid()); !ok {
		slog.Debug("process RAM measurer unavailable: cannot read this process's own RSS")
		return nil
	}
	return measureProcessRSS
}

// measureProcessRSS is the func(pids []int) map[int]int NewRAMMeasurer hands
// out. See NewRAMMeasurer for the contract.
func measureProcessRSS(pids []int) map[int]int {
	var out map[int]int
	for _, pid := range pids {
		rss, ok := processRSSBytes(pid)
		if !ok {
			continue
		}
		mb := int(int64(rss) / bytesPerMB)
		if mb <= 0 {
			continue // under 1 MB: "unknown", not "needs none" -- see NewRAMMeasurer
		}
		if out == nil {
			out = make(map[int]int, len(pids))
		}
		out[pid] = mb
	}
	return out
}

// processRSSBytes reads one pid's resident set size in bytes. (0, false) for a
// pid that is not a real process id or that the OS will not report on, which is
// the normal outcome of the exit race and never worth more than a skip.
//
// pid <= 0 is rejected before the OS is asked: on some platforms gopsutil
// resolves pid 0 to the kernel itself and returns its memory, which would be
// charged to whichever spec the caller meant. A pid above MaxInt32 is not a pid
// on any non-Windows platform and would wrap when narrowed to gopsutil's int32.
func processRSSBytes(pid int) (uint64, bool) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, false
	}
	p, err := process.NewProcess(int32(pid))
	if err != nil {
		return 0, false
	}
	mi, err := p.MemoryInfo()
	if err != nil || mi == nil {
		return 0, false
	}
	return mi.RSS, true
}

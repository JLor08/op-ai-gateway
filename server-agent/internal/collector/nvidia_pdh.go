// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"regexp"
	"strconv"
	"strings"
)

// This file is the PURE half of the Windows per-process VRAM measurer: the
// counter-instance name grammar, the pci.bus_id grammar, and the aggregation
// into the nested map runtime.Manager.SetMeasurer expects. It carries NO build
// tag on purpose, exactly like wmi_map.go next to hwinfo_windows.go: CI runs
// on ubuntu-latest and never builds, vets or tests GOOS=windows, so anything
// behind `//go:build windows` is verified by review alone. Everything that can
// be wrong about a number -- a swapped LUID field, a hex/decimal mix-up, a
// mis-summed multi-GPU model, a 0 escaping as if it were a measurement -- lives
// here, where a unit test on Linux exercises the very code Windows runs. Only
// the syscalls themselves are in nvidia_pdh_windows.go.
//
// Why this measurer exists at all: on Windows the WDDM driver model puts the
// OS, not the NVIDIA driver, in charge of GPU memory, so
// `nvidia-smi --query-compute-apps=...,used_memory` reports `[N/A]`. The chain
// used instead was proven end to end on a 3-GPU host (driver 610.62):
//
//  1. PDH counter `\GPU Process Memory(*)\Dedicated Usage`, whose instances are
//     named `pid_<PID>_luid_0x<HighPart>_0x<LowPart>_phys_<N>` -> bytes per PID
//     per adapter LUID.
//  2. `D3DKMTOpenAdapterFromLuid` + `D3DKMTQueryAdapterInfo` -> that adapter's
//     PCI address.
//  3. `nvidia-smi --query-gpu=index,pci.bus_id` -> the same PCI address -> the
//     GPU index specs and budgets are written in terms of.

// nvidiaPCIBusIDFields is the ordered --query-gpu field list the PDH measurer
// requests: the GPU index, and the PCI address that is the only thing a
// D3DKMT-resolved adapter and an nvidia-smi GPU have in common (compute-apps'
// UUID is unavailable from D3DKMT, and D3DKMT's LUID is unavailable from
// nvidia-smi). parseNvidiaPCIIndexCSV assumes exactly this column order.
// Deliberately its own constant, independent of nvidiaQueryFields and of the
// measurer-only nvidiaGPUIndexFields: three queries with three parsers, none of
// which may be changed in step with the others.
const nvidiaPCIBusIDFields = "index,pci.bus_id"

// bytesPerMB converts the PDH counter's BYTES to the MB the measurer contract
// (pid -> gpuIndex -> MB) and every spec's VRAMMB estimate are written in.
const bytesPerMB int64 = 1024 * 1024

// pdhLUID is a Windows display-adapter LUID as it appears in a `GPU Process
// Memory` counter instance name and in D3DKMT_OPENADAPTERFROMLUID: a locally
// unique id, stable for the life of a boot, that identifies one adapter. The
// field types mirror the C LUID (DWORD LowPart, LONG HighPart) so the Windows
// half can pass them straight through.
type pdhLUID struct {
	HighPart int32
	LowPart  uint32
}

// pdhProcessMemory is one `\GPU Process Memory(*)\Dedicated Usage` counter
// instance: how many bytes of dedicated VRAM one process holds on one physical
// segment of one adapter.
type pdhProcessMemory struct {
	PID  int
	LUID pdhLUID
	// Phys is the adapter's physical memory segment. It is parsed but never
	// interpreted: a process holds VRAM across several segments of the same
	// adapter, and the measurement wanted is the per-GPU total, so
	// attributePDHDedicated sums the segments away. Kept as a field because
	// dropping it would make two distinct instances look identical.
	Phys           int
	DedicatedBytes int64
}

// pdhInstanceRe is the counter-instance grammar. Case-insensitive because
// nothing documents the casing (Microsoft does not document the grammar at
// all -- it was read off a live host), and deliberately NOT anchored at the
// end: the probe run matched every instance on that machine with this
// expression, and a future Windows build that appends a further segment should
// still yield the PID and LUID it does report rather than nothing.
//
// The two hex fields are HighPart FIRST. That ordering is a measured fact, not
// a documented one, and it is the single mistake this file could make with no
// visible symptom: the swapped reading yields a LUID that D3DKMT simply refuses
// to open, which is indistinguishable from the perfectly normal case of an
// adapter with no NVIDIA GPU behind it, so the measurer would report nothing
// and look like "unsupported hardware" forever.
var pdhInstanceRe = regexp.MustCompile(`(?i)^pid_(\d+)_luid_0x([0-9a-f]+)_0x([0-9a-f]+)_phys_(\d+)`)

// parsePDHInstanceName parses one counter instance name. The returned struct's
// DedicatedBytes is left at 0 -- the counter VALUE arrives separately, out of
// the PDH item struct, and only the Windows half has it.
//
// ok is false for anything that does not match the grammar, including numbers
// too large for their type. Every unexpected shape is SKIPPED: a wrong PID
// would charge another process's VRAM to a managed model, and a wrong LUID
// would charge the wrong GPU, so guessing is strictly worse than reporting
// nothing and letting the manager fall back to the operator's estimate.
func parsePDHInstanceName(name string) (pdhProcessMemory, bool) {
	m := pdhInstanceRe.FindStringSubmatch(name)
	if m == nil {
		return pdhProcessMemory{}, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil {
		return pdhProcessMemory{}, false
	}
	high, err := strconv.ParseUint(m[2], 16, 32)
	if err != nil {
		return pdhProcessMemory{}, false
	}
	low, err := strconv.ParseUint(m[3], 16, 32)
	if err != nil {
		return pdhProcessMemory{}, false
	}
	phys, err := strconv.Atoi(m[4])
	if err != nil {
		return pdhProcessMemory{}, false
	}
	return pdhProcessMemory{
		PID:  pid,
		LUID: pdhLUID{HighPart: int32(high), LowPart: uint32(low)},
		Phys: phys,
	}, true
}

// pciAddress is a PCI bus location, the join key between an adapter LUID and an
// nvidia-smi GPU index. Deliberately a struct of plain numbers rather than a
// formatted string: nvidia-smi writes `00000000:21:00.0` in HEX while D3DKMT
// reports the same location as three decimal ints, so comparing the two as text
// would silently never match (bus 0x21 vs bus 33). The PCI domain is not part
// of it -- D3DKMT_ADAPTERADDRESS does not report one.
type pciAddress struct {
	Bus      uint32
	Device   uint32
	Function uint32
}

// parseNvidiaBusID parses nvidia-smi's pci.bus_id
// (`<domain>:<bus>:<device>.<function>`, hex) into the comparable form. The
// domain is accepted and discarded; a bus_id without one parses too. ok is
// false for `[N/A]`, an empty field, or any other shape -- an address that
// cannot be parsed simply never joins to an adapter, which costs a measurement,
// never a wrong one.
func parseNvidiaBusID(s string) (pciAddress, bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 {
		return pciAddress{}, false
	}
	devFn := strings.Split(parts[len(parts)-1], ".")
	if len(devFn) != 2 {
		return pciAddress{}, false
	}
	bus, err := strconv.ParseUint(parts[len(parts)-2], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	dev, err := strconv.ParseUint(devFn[0], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	fn, err := strconv.ParseUint(devFn[1], 16, 32)
	if err != nil {
		return pciAddress{}, false
	}
	return pciAddress{Bus: uint32(bus), Device: uint32(dev), Function: uint32(fn)}, true
}

// parseNvidiaPCIIndexCSV parses `nvidia-smi --query-gpu=index,pci.bus_id
// --format=csv,noheader,nounits` output into a PCI-address -> GPU-index map.
// Rows with fewer than 2 fields, or an unparseable bus_id, are skipped --
// parseNvidiaCSV's own tolerance for a malformed line. Reuses
// splitNvidiaCSVRows and naInt (nvidia.go) so this module keeps exactly one
// nvidia-smi CSV reader.
//
// pci.bus_id is safe to carry through a comma-split: its form is
// `00000000:21:00.0` -- colons and a dot, never a comma.
func parseNvidiaPCIIndexCSV(data []byte) map[pciAddress]int {
	out := make(map[pciAddress]int)
	for _, row := range splitNvidiaCSVRows(data) {
		if len(row) < 2 {
			continue
		}
		addr, ok := parseNvidiaBusID(row[1])
		if !ok {
			continue
		}
		out[addr] = naInt(row[0])
	}
	return out
}

// attributePDHDedicated is the aggregation the measurer contract needs: filter
// the counter instances to the manager's own PIDs, resolve each instance's
// adapter LUID to a GPU index, sum the adapter's physical segments, and convert
// bytes to MB. The result is map[pid]map[gpuIndex]MB; nil means nothing was
// measured.
//
// A standalone pure function, following attributeComputeApps (nvidia.go) for
// the same reason: the headline tests call the SAME code the production path
// runs, instead of re-implementing the filter/sum/convert loop in the test body
// where a real bug -- wrong filter direction, a dropped GPU index, segments not
// summed, bytes reported as MB -- would leave them green regardless.
//
// THE RULE THIS FUNCTION EXISTS TO ENFORCE: a (pid, gpu) whose summed usage is
// 0 MB is DROPPED, not reported as 0. runtime/manager.go buildSnapshot reads
// `if v, ok := byGPU[g.Index]; ok`, so a present key is authoritative and a
// measured 0 overrides the operator's VRAM estimate -- the GPU budget then
// looks entirely free and co-residency admission loses the OOM protection it
// exists for. That is the live bug on Windows today (nvidia-smi's `[N/A]` ->
// naInt -> 0 -> a non-nil map of zeros), and this measurer must not reproduce
// it in a new shape. An absent key means "not measured", which falls back to
// the estimate; an estimate is the safe direction, because it is never smaller
// than reality by accident.
func attributePDHDedicated(instances []pdhProcessMemory, luidToIndex map[pdhLUID]int, pids []int) map[int]map[int]int {
	wanted := make(map[int]bool, len(pids))
	for _, p := range pids {
		wanted[p] = true
	}

	// Sum in BYTES first, then convert once per (pid, gpu). Converting each
	// instance would round every physical segment down separately and lose up
	// to a MB per segment.
	sums := make(map[int]map[int]int64)
	for _, in := range instances {
		if !wanted[in.PID] {
			continue // not one of the manager's own managed children
		}
		if in.DedicatedBytes <= 0 {
			continue // nothing to add, and a nonsense negative must not REDUCE a real segment
		}
		idx, ok := luidToIndex[in.LUID]
		if !ok {
			// An adapter that could not be resolved to an NVIDIA GPU: a
			// software/render adapter, an iGPU, or a stale counter instance.
			// Normal, and observed on the probe host -- skip it rather than
			// guess an index (index 0 being the guess a zero value would make).
			continue
		}
		if sums[in.PID] == nil {
			sums[in.PID] = make(map[int]int64)
		}
		sums[in.PID][idx] += in.DedicatedBytes
	}

	var out map[int]map[int]int
	for pid, byGPU := range sums {
		for idx, bytes := range byGPU {
			mb := int(bytes / bytesPerMB)
			if mb <= 0 {
				continue // see THE RULE above: a 0 must never reach the manager
			}
			if out == nil {
				out = make(map[int]map[int]int)
			}
			if out[pid] == nil {
				out[pid] = make(map[int]int)
			}
			out[pid][idx] = mb
		}
	}
	return out
}

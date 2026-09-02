// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

// NewVRAMMeasurer selects the per-process, per-GPU VRAM measurer for Windows:
// the PDH one, or none at all.
//
// The compute-apps measurer is deliberately NOT an option here, and not merely
// because it would report nothing useful. Under the WDDM driver model the OS,
// not the NVIDIA driver, owns GPU memory, so
// `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory` reports `[N/A]`
// for used_memory. naInt maps that to 0, so attributeComputeApps returns a
// NON-NIL map[pid]map[gpuIndex]0 -- and runtime/manager.go buildSnapshot reads
// `if v, ok := byGPU[g.Index]; ok`, where a present key wins. Every managed
// model on the host would be charged 0 MB, the GPU budget would look entirely
// free, and co-residency admission would lose exactly the OOM protection it
// exists for. (Only the TCC driver model restores per-process memory
// reporting, and TCC disables display output and is unavailable on most
// GeForce -- not an option for these hosts.)
//
// So the choice on Windows is PDH or nothing, and nothing is strictly safer
// than a measured 0: no measurer means the manager falls back to each spec's
// operator-entered VRAM estimate, while a measured 0 overrides it.
// newNvidiaPDHMeasurer already returns nil when the host cannot support the
// PDH path, which makes this the whole selector.
func NewVRAMMeasurer() func(pids []int) map[int]map[int]int {
	return newNvidiaPDHMeasurer()
}

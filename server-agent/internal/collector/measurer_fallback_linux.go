// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

// fallbackVRAMMeasurer is the Linux choice behind NewVRAMMeasurer: nvidia-smi
// compute-apps when present, else KFD sysfs, else nil.
func fallbackVRAMMeasurer() func(pids []int) map[int]map[int]int {
	return pickLinuxVRAMMeasurer(NewNvidiaComputeApps(), NewKFDMeasurer())
}

// pickLinuxVRAMMeasurer returns the first non-nil measurer. NVIDIA comes first
// so that every NVIDIA host, including a mixed NVIDIA/AMD one, keeps exactly
// the measurer it had; the manager installs a single measurer, so a mixed host
// measures its NVIDIA GPUs and leaves AMD specs on their estimates.
func pickLinuxVRAMMeasurer(nvidia, kfd func(pids []int) map[int]map[int]int) func(pids []int) map[int]map[int]int {
	if nvidia != nil {
		return nvidia
	}
	return kfd
}

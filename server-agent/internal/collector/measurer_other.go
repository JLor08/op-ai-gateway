// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package collector

// NewVRAMMeasurer selects the per-process, per-GPU VRAM measurer for every
// platform except Windows: the nvidia-smi compute-apps one first, on Linux the
// KFD sysfs one next, and nil when neither source exists.
//
// On Linux and macOS `nvidia-smi --query-compute-apps` reports real
// per-process memory (the WDDM problem is Windows-only), so it keeps priority
// wherever nvidia-smi exists -- a mixed NVIDIA/AMD host included -- and every
// NVIDIA host is unchanged. An AMD Linux host has no nvidia-smi, and there the
// kernel's KFD sysfs supplies the same per-process figures. The per-platform
// order lives in fallbackVRAMMeasurer (measurer_fallback_linux.go and
// measurer_fallback_other.go). The Windows-only PDH path is not merely unused
// here, it cannot build: it calls pdh.dll and gdi32.dll directly.
//
// Follows the hwinfo_windows.go / hwinfo_other.go pattern: one exported name,
// one implementation per platform, and the platform decision made by the
// build tag rather than by a runtime.GOOS branch that would have to compile
// both halves everywhere.
func NewVRAMMeasurer() func(pids []int) map[int]map[int]int {
	return fallbackVRAMMeasurer()
}

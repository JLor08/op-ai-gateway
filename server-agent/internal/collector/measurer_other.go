// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package collector

// NewVRAMMeasurer selects the per-process, per-GPU VRAM measurer for every
// platform except Windows: the nvidia-smi compute-apps one wherever nvidia-smi
// exists, and on Linux an AMD one (DRM fdinfo, else KFD sysfs) beside it, so a
// mixed NVIDIA/AMD host measures both vendors. Measure is nil when no source
// exists.
//
// On Linux and macOS `nvidia-smi --query-compute-apps` reports real
// per-process memory (the WDDM problem is Windows-only), so every NVIDIA host
// keeps exactly the measurer it had; its answers now carry the vendor. The
// per-platform assembly lives in fallbackVRAMMeasurer
// (measurer_fallback_linux.go and measurer_fallback_other.go). The Windows-only PDH path is not merely unused
// here, it cannot build: it calls pdh.dll and gdi32.dll directly.
//
// Follows the hwinfo_windows.go / hwinfo_other.go pattern: one exported name,
// one implementation per platform, and the platform decision made by the
// build tag rather than by a runtime.GOOS branch that would have to compile
// both halves everywhere.
func NewVRAMMeasurer() VRAMMeasurer {
	return fallbackVRAMMeasurer()
}

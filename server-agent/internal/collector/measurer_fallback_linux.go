// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

// fallbackVRAMMeasurer is the Linux measurer behind NewVRAMMeasurer: NVIDIA
// compute-apps AND an AMD measurer side by side, so a mixed host measures both
// vendors (linuxVRAMMeasurer composes them). For AMD the DRM fdinfo measurer
// is chosen when the self-probe finds drm-memory-vram in this kernel's amdgpu
// fdinfo (it covers ROCm and Vulkan), else KFD sysfs (ROCm only), else none.
// Both AMD measurers answer only while the AMD collector's PCI join is
// published (currentAMDIndex), which is read at every call.
func fallbackVRAMMeasurer() VRAMMeasurer {
	var fdinfo, kfd func(pids []int) map[int]map[int]int
	if probeDRMFdinfo() {
		fdinfo = fdinfoMeasurer{procRoot: linuxProcRoot, index: currentAMDIndex, groupOf: getpgidOwner}.measure
	} else {
		kfd = NewKFDMeasurer()
	}
	return linuxVRAMMeasurer(NewNvidiaComputeApps(), fdinfo, kfd, func() bool { return currentAMDIndex() != nil })
}

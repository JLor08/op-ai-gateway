// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

// VRAMMeasurer is the host's per-process, per-GPU VRAM measurement, built once
// by NewVRAMMeasurer.
//
// Measure answers runtime.Manager.SetMeasurer's question -- pid -> GPU key ->
// MiB for the group each pid leads -- and is nil when the host has no source
// at all (the operator's estimates stand). A GPU absent from an answer is "not
// measured", never 0: a present key overrides the operator's estimate.
//
// Measurable reports, per vendor, the backends whose per-process VRAM Measure
// can see (`cuda`, `rocm`, `vulkan`); the hardware report carries it so the
// gateway does not assume measurability. It is never nil and is evaluated at
// each call, because the AMD part depends on the collector's PCI join, which
// can appear or go after construction.
type VRAMMeasurer struct {
	Measure    func(pids []int) map[int]map[GPUKey]int
	Measurable func() map[string][]string
}

// vendorMeasurer is one vendor's measurer, which answers by that vendor's GPU
// index alone.
type vendorMeasurer struct {
	vendor  string
	measure func(pids []int) map[int]map[int]int
}

// composeMeasurers runs every non-nil part and merges the answers per pid.
// The keys never collide because they carry the vendor, so a mixed host
// measures both vendors at once, and one vendor returning nothing (an
// nvidia-smi timeout) costs the other nothing. It returns a nil measurer when
// no part is non-nil, and a nil answer when every part answered nil or empty.
func composeMeasurers(parts ...vendorMeasurer) func(pids []int) map[int]map[GPUKey]int {
	var live []vendorMeasurer
	for _, p := range parts {
		if p.measure != nil {
			live = append(live, p)
		}
	}
	if len(live) == 0 {
		return nil
	}
	return func(pids []int) map[int]map[GPUKey]int {
		var out map[int]map[GPUKey]int
		for _, p := range live {
			for pid, byIndex := range p.measure(pids) {
				for idx, mib := range byIndex {
					if out == nil {
						out = make(map[int]map[GPUKey]int)
					}
					if out[pid] == nil {
						out[pid] = make(map[GPUKey]int)
					}
					out[pid][GPUKey{Vendor: p.vendor, Index: idx}] = mib
				}
			}
		}
		return out
	}
}

// Backend lists per source. NVIDIA's measurers (Linux compute-apps, Windows
// PDH) see CUDA and Vulkan processes alike; AMD's DRM fdinfo sees every
// amdgpu client (ROCm and Vulkan), KFD only processes that opened /dev/kfd.
func nvidiaMeasurable() []string        { return []string{"cuda", "vulkan"} }
func amdFdinfoMeasurable() []string     { return []string{"rocm", "vulkan"} }
func amdKFDMeasurable() []string        { return []string{"rocm"} }
func noMeasurable() map[string][]string { return map[string][]string{} }

// nvidiaVRAMMeasurer is the single-source shape of Windows (PDH) and of every
// platform that is neither Windows nor Linux (compute-apps): NVIDIA or none.
func nvidiaVRAMMeasurer(nvidia func(pids []int) map[int]map[int]int) VRAMMeasurer {
	if nvidia == nil {
		return VRAMMeasurer{Measurable: noMeasurable}
	}
	return VRAMMeasurer{
		Measure:    composeMeasurers(vendorMeasurer{VendorNVIDIA, nvidia}),
		Measurable: func() map[string][]string { return map[string][]string{VendorNVIDIA: nvidiaMeasurable()} },
	}
}

// linuxVRAMMeasurer assembles the Linux measurer from its sources: NVIDIA
// compute-apps, and for AMD the DRM fdinfo measurer when the kernel reports
// drm-memory-vram, else KFD. KFD is a fallback only, never added to fdinfo:
// both read the same allocations. Every AMD answer and the AMD measurability
// exist only while amdJoined holds -- without the collector's PCI join an AMD
// index would be a guess, and a guess is never measured against.
func linuxVRAMMeasurer(nvidia, fdinfo, kfd func(pids []int) map[int]map[int]int, amdJoined func() bool) VRAMMeasurer {
	amd, amdBackends := fdinfo, amdFdinfoMeasurable
	if amd == nil {
		amd, amdBackends = kfd, amdKFDMeasurable
	}
	var gatedAMD func(pids []int) map[int]map[int]int
	if amd != nil {
		gatedAMD = func(pids []int) map[int]map[int]int {
			if !amdJoined() {
				return nil
			}
			return amd(pids)
		}
	}
	return VRAMMeasurer{
		Measure: composeMeasurers(vendorMeasurer{VendorNVIDIA, nvidia}, vendorMeasurer{VendorAMD, gatedAMD}),
		Measurable: func() map[string][]string {
			out := noMeasurable()
			if nvidia != nil {
				out[VendorNVIDIA] = nvidiaMeasurable()
			}
			if amd != nil && amdJoined() {
				out[VendorAMD] = amdBackends()
			}
			return out
		},
	}
}

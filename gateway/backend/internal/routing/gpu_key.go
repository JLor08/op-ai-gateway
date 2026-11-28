// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"slices"
	"strconv"
)

// The GPU vendors a GPU key names (#434, mixed NVIDIA + AMD hosts). They are
// the wire values of every `vendor` field and of the gpu_vendor columns. The
// empty string is the legacy vendor: "the host's only vendor", what old
// agents report and what rows written before migration 119 hold.
const (
	GPUVendorNVIDIA = "nvidia"
	GPUVendorAMD    = "amd"
	GPUVendorApple  = "apple"
)

// gpuVendorOrder is the fixed order LegacyGPUVendor picks from: the order in
// which today's agent picks "the" vendor of a host.
var gpuVendorOrder = []string{GPUVendorNVIDIA, GPUVendorAMD, GPUVendorApple}

// GPUKey names one GPU of a host: its vendor and its vendor-local index (the
// nvidia-smi index, the ROCr ordinal, the Apple device index). One index may
// exist once per vendor, so the index alone no longer names a card.
type GPUKey struct {
	Vendor string
	Index  int
}

// String renders the key for humans: "NVIDIA 0", "AMD 1", "Apple 0", and
// "GPU 0" for the legacy empty vendor. An unknown vendor is shown verbatim.
func (k GPUKey) String() string {
	label := k.Vendor
	switch k.Vendor {
	case GPUVendorNVIDIA:
		label = "NVIDIA"
	case GPUVendorAMD:
		label = "AMD"
	case GPUVendorApple:
		label = "Apple"
	case "":
		label = "GPU"
	}
	return label + " " + strconv.Itoa(k.Index)
}

// ValidGPUVendor reports whether v is a vendor a GPU row may carry: the
// legacy empty vendor or one of the three known ones, exactly as spelled.
func ValidGPUVendor(v string) bool {
	return v == "" || v == GPUVendorNVIDIA || v == GPUVendorAMD || v == GPUVendorApple
}

// LegacyGPUVendor is the vendor the empty vendor stands for on a host that
// reports vendors: the first present of nvidia, amd, apple, whatever the
// order of vendors; "" when none of them is present.
func LegacyGPUVendor(vendors []string) string {
	for _, v := range gpuVendorOrder {
		if slices.Contains(vendors, v) {
			return v
		}
	}
	return ""
}

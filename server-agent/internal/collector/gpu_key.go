// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

// GPU vendor wire values. They are the same strings as GPUCollector.Name()
// for the three GPU collectors, and the empty string means "the host's only
// vendor" on legacy wire data.
const (
	VendorNVIDIA = "nvidia"
	VendorAMD    = "amd"
	VendorApple  = "apple"
)

// GPUKey identifies one GPU on a host. A bare index is ambiguous once an
// NVIDIA and an AMD card share a machine (both number from 0), so the vendor
// is part of the key.
type GPUKey struct {
	Vendor string
	Index  int
}

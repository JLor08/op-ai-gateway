// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestLegacyGPUVendor(t *testing.T) {
	for _, tc := range []struct {
		vendors []string
		want    string
	}{
		{[]string{"amd", "nvidia"}, "nvidia"},
		{[]string{"apple"}, "apple"},
		{[]string{"apple", "amd"}, "amd"},
		{[]string{"", "amd", "amd"}, "amd"},
		{[]string{"intel"}, ""},
		{nil, ""},
	} {
		if got := LegacyGPUVendor(tc.vendors); got != tc.want {
			t.Errorf("LegacyGPUVendor(%q) = %q, want %q", tc.vendors, got, tc.want)
		}
	}
}

func TestValidGPUVendor(t *testing.T) {
	for _, v := range []string{"", GPUVendorNVIDIA, GPUVendorAMD, GPUVendorApple} {
		if !ValidGPUVendor(v) {
			t.Errorf("ValidGPUVendor(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"NVIDIA", "intel", " amd", "nvidia "} {
		if ValidGPUVendor(v) {
			t.Errorf("ValidGPUVendor(%q) = true, want false", v)
		}
	}
}

func TestGPUKeyString(t *testing.T) {
	for _, tc := range []struct {
		key  GPUKey
		want string
	}{
		{GPUKey{GPUVendorNVIDIA, 0}, "NVIDIA 0"},
		{GPUKey{GPUVendorAMD, 1}, "AMD 1"},
		{GPUKey{GPUVendorApple, 0}, "Apple 0"},
		{GPUKey{"", 2}, "GPU 2"},
		{GPUKey{"intel", 3}, "intel 3"},
	} {
		if got := tc.key.String(); got != tc.want {
			t.Errorf("%#v.String() = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestRuntimeSpecGPUKey(t *testing.T) {
	g := RuntimeSpecGPU{SpecID: "s", GPUVendor: GPUVendorAMD, GPUIndex: 1, Position: 3}
	if got := g.Key(); got != (GPUKey{GPUVendorAMD, 1}) {
		t.Fatalf("Key() = %#v", got)
	}
}

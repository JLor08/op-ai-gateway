// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
)

// TestGPUKeyTextRoundTrip: a GPU-keyed map marshals to a JSON object keyed by
// "vendor:index" (Status.MeasuredVRAM stays an object on the wire) and reads
// back to the same keys.
func TestGPUKeyTextRoundTrip(t *testing.T) {
	b, err := json.Marshal(map[GPUKey]int{{GPUVendorAMD, 1}: 3462})
	if err != nil || string(b) != `{"amd:1":3462}` {
		t.Fatalf("marshal = %s, %v; want {\"amd:1\":3462}", b, err)
	}
	var back map[GPUKey]int
	if err := json.Unmarshal(b, &back); err != nil || back[GPUKey{GPUVendorAMD, 1}] != 3462 {
		t.Fatalf("unmarshal = %v, %v", back, err)
	}
	if got := (GPUKey{GPUVendorNVIDIA, 0}).String(); got != "nvidia:0" {
		t.Errorf("String() = %q, want nvidia:0", got)
	}
}

// TestGPUKeyUnmarshalTextRejectsMalformed: only "vendor:index" with a known
// (or empty) vendor and a non-negative index is a key.
func TestGPUKeyUnmarshalTextRejectsMalformed(t *testing.T) {
	for _, s := range []string{"0", "nvidia", "intel:0", "nvidia:x", "amd:-1", "nvidia:"} {
		var k GPUKey
		if err := k.UnmarshalText([]byte(s)); err == nil {
			t.Errorf("UnmarshalText(%q) = %+v, want an error", s, k)
		}
	}
	var k GPUKey
	if err := k.UnmarshalText([]byte(":2")); err != nil || k != (GPUKey{Index: 2}) {
		t.Errorf("UnmarshalText(\":2\") = %+v, %v; want the empty-vendor key", k, err)
	}
}

// TestResolveLegacyGPUVendors: an empty vendor becomes the host's legacy
// vendor, an explicit one is kept, and the input is never written.
func TestResolveLegacyGPUVendors(t *testing.T) {
	cfg := Config{
		GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 1}},
		Specs:      []Spec{{ID: "a", GPUs: []SpecGPU{{Index: 0}, {Index: 1, Vendor: GPUVendorAMD}}}},
	}
	got := resolveLegacyGPUVendors(cfg, GPUVendorNVIDIA)
	want := []SpecGPU{{Index: 0, Vendor: GPUVendorNVIDIA}, {Index: 1, Vendor: GPUVendorAMD}}
	if !reflect.DeepEqual(got.Specs[0].GPUs, want) || got.GPUBudgets[0].Vendor != GPUVendorNVIDIA {
		t.Fatalf("resolved = %+v", got)
	}
	if cfg.Specs[0].GPUs[0].Vendor != "" || cfg.GPUBudgets[0].Vendor != "" {
		t.Fatalf("input was written: %+v", cfg)
	}
}

// TestResolveLegacyGPUVendorsOnAGPULessHostIsANoOp: with no legacy vendor (a
// CPU-only host) there is nothing to resolve to, and the document is returned
// as it came.
func TestResolveLegacyGPUVendorsOnAGPULessHostIsANoOp(t *testing.T) {
	cfg := Config{Specs: []Spec{{ID: "a", GPUs: []SpecGPU{{Index: 0}}}}}
	if got := resolveLegacyGPUVendors(cfg, GPUVendorNone); !reflect.DeepEqual(got, cfg) {
		t.Fatalf("resolved = %+v, want the input unchanged", got)
	}
}

// The topology's vendors are the GPUs the agent reported (HostInfo), not the
// GPU tools on PATH: an NVIDIA-only box with rocm-smi installed is not mixed
// once its GPUs are known. Before the first host info detection stands.
func TestTopologyVendorsFollowReportedGPUs(t *testing.T) {
	o := &owner{m: &Manager{}, gpuVendor: GPUVendorNVIDIA, gpuVendors: GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}}
	if !o.topology().Mixed() {
		t.Fatal("before any host info: detection says nvidia+amd, want mixed")
	}
	o.m.SetHostInfo(HostInfo{GPUTotalsMB: map[GPUKey]int{{GPUVendorNVIDIA, 0}: 24000, {GPUVendorNVIDIA, 1}: 24000}})
	if got := o.topology().Vendors; !slices.Equal(got, GPUVendors{GPUVendorNVIDIA}) {
		t.Fatalf("vendors with nvidia-only GPUs = %v, want [nvidia]", got)
	}
	// A vendor known only from the Vulkan map counts too.
	o.m.SetHostInfo(HostInfo{
		GPUTotalsMB: map[GPUKey]int{{GPUVendorAMD, 0}: 16000},
		Vulkan:      VulkanMap{Ordinals: map[GPUKey]int{{GPUVendorNVIDIA, 0}: 0}, DeviceCount: 2},
	})
	if got := o.topology().Vendors; !slices.Equal(got, GPUVendors{GPUVendorNVIDIA, GPUVendorAMD}) {
		t.Fatalf("vendors = %v, want [nvidia amd] in precedence order", got)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"reflect"
	"testing"
)

// The fakes below stand in for the per-vendor measurers. The AMD figures are
// the fixtures' (fdinfo-amdgpu-hip.txt: 3461 MiB on card 1; kfd-proc-hip.txt:
// vram_19306 3766919168 B = 3592 MiB for the same process).
var (
	joined     = func() bool { return true }
	nvStub     = func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 7874}} }
	fdinfoStub = func([]int) map[int]map[int]int { return map[int]map[int]int{100: {1: 3461}} }
	kfdStub    = func([]int) map[int]map[int]int { return map[int]map[int]int{100: {1: 3592}} }
)

func TestComposeMeasurersMergesVendors(t *testing.T) {
	nv := func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 7874}} }
	amd := func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 3323}, 200: {1: 3461}} }
	got := composeMeasurers(vendorMeasurer{VendorNVIDIA, nv}, vendorMeasurer{VendorAMD, amd})([]int{100, 200})
	want := map[int]map[GPUKey]int{100: {{VendorNVIDIA, 0}: 7874, {VendorAMD, 0}: 3323}, 200: {{VendorAMD, 1}: 3461}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// TestComposeMeasurersOneVendorFailingKeepsTheOther: one vendor's hiccup (an
// nvidia-smi timeout) must not cost the other vendor its measurement, and when
// every vendor returns nothing the composite returns nil -- no measurement,
// never an empty-but-present map.
func TestComposeMeasurersOneVendorFailingKeepsTheOther(t *testing.T) {
	none := func([]int) map[int]map[int]int { return nil }
	amd := func([]int) map[int]map[int]int { return map[int]map[int]int{200: {1: 3461}} }
	got := composeMeasurers(vendorMeasurer{VendorNVIDIA, none}, vendorMeasurer{VendorAMD, amd})([]int{100, 200})
	if want := (map[int]map[GPUKey]int{200: {{VendorAMD, 1}: 3461}}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := composeMeasurers(vendorMeasurer{VendorNVIDIA, none}, vendorMeasurer{VendorAMD, none})([]int{100}); got != nil {
		t.Fatal(got)
	}
	empty := func([]int) map[int]map[int]int { return map[int]map[int]int{100: {}} }
	if got := composeMeasurers(vendorMeasurer{VendorAMD, empty})([]int{100}); got != nil {
		t.Fatalf("an empty per-pid map is no measurement: %v", got)
	}
}

func TestComposeMeasurersSkipsNilParts(t *testing.T) {
	if composeMeasurers() != nil || composeMeasurers(vendorMeasurer{VendorNVIDIA, nil}) != nil {
		t.Fatal("no part (or only nil parts) must compose to a nil measurer")
	}
	got := composeMeasurers(vendorMeasurer{VendorNVIDIA, nil}, vendorMeasurer{VendorAMD, fdinfoStub})([]int{100})
	if want := (map[int]map[GPUKey]int{100: {{VendorAMD, 1}: 3461}}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestLinuxMeasurerPrefersFdinfoOverKFD(t *testing.T) {
	m := linuxVRAMMeasurer(nil, fdinfoStub, kfdStub, joined)
	if !reflect.DeepEqual(m.Measurable(), map[string][]string{VendorAMD: {"rocm", "vulkan"}}) {
		t.Fatal(m.Measurable())
	}
}

// TestLinuxMeasurerNeverAddsKFDToFdinfo: KFD and fdinfo read the same
// allocations (KFD's figure is ~130 MiB higher), so adding them would charge
// a ROCm process twice.
func TestLinuxMeasurerNeverAddsKFDToFdinfo(t *testing.T) {
	m := linuxVRAMMeasurer(nil, fdinfoStub, kfdStub, joined)
	if got, want := m.Measure([]int{100}), (map[int]map[GPUKey]int{100: {{VendorAMD, 1}: 3461}}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestLinuxMeasurerKFDFallbackMeasurability(t *testing.T) {
	m := linuxVRAMMeasurer(nvStub, nil, kfdStub, joined)
	want := map[string][]string{VendorNVIDIA: {"cuda", "vulkan"}, VendorAMD: {"rocm"}}
	if !reflect.DeepEqual(m.Measurable(), want) {
		t.Fatal(m.Measurable())
	}
	got := m.Measure([]int{100})
	if wantM := (map[int]map[GPUKey]int{100: {{VendorNVIDIA, 0}: 7874, {VendorAMD, 1}: 3592}}); !reflect.DeepEqual(got, wantM) {
		t.Fatal(got)
	}
}

// TestLinuxMeasurerNoAMDWithoutJoin: without the collector's PCI join the
// AMD indices the measurer would produce are a guess, so there is neither an
// AMD measurement nor an AMD measurability claim. The join is read at every
// call: it can appear (and go) after construction.
func TestLinuxMeasurerNoAMDWithoutJoin(t *testing.T) {
	for name, amd := range map[string][2]func([]int) map[int]map[int]int{
		"fdinfo": {fdinfoStub, nil},
		"kfd":    {nil, kfdStub},
	} {
		t.Run(name, func(t *testing.T) {
			ok := false
			m := linuxVRAMMeasurer(nvStub, amd[0], amd[1], func() bool { return ok })
			if _, has := m.Measurable()[VendorAMD]; has {
				t.Fatal(m.Measurable())
			}
			for _, byGPU := range m.Measure([]int{100}) {
				for k := range byGPU {
					if k.Vendor == VendorAMD {
						t.Fatal("amd measured without a join")
					}
				}
			}
			ok = true
			if _, has := m.Measurable()[VendorAMD]; !has {
				t.Fatalf("the join appeared, the AMD measurability did not: %v", m.Measurable())
			}
			if got := m.Measure([]int{100}); len(got[100]) != 2 {
				t.Fatalf("the join appeared, the AMD measurement did not: %v", got)
			}
		})
	}
}

func TestLinuxMeasurerNone(t *testing.T) {
	if m := linuxVRAMMeasurer(nil, nil, nil, joined); m.Measure != nil || m.Measurable == nil || len(m.Measurable()) != 0 {
		t.Fatal("no source: want a nil Measure and an empty (non-nil) Measurable")
	}
}

// TestNvidiaVRAMMeasurer is the Windows and non-Linux shape: one NVIDIA
// source, or none.
func TestNvidiaVRAMMeasurer(t *testing.T) {
	if m := nvidiaVRAMMeasurer(nil); m.Measure != nil || m.Measurable == nil || len(m.Measurable()) != 0 {
		t.Fatal("no source: want a nil Measure and an empty Measurable")
	}
	m := nvidiaVRAMMeasurer(nvStub)
	if !reflect.DeepEqual(m.Measurable(), map[string][]string{VendorNVIDIA: {"cuda", "vulkan"}}) {
		t.Fatal(m.Measurable())
	}
	if got, want := m.Measure([]int{100}), (map[int]map[GPUKey]int{100: {{VendorNVIDIA, 0}: 7874}}); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

// TestMeasurableReturnsFreshSlices: a caller that edits the list it got must
// not change what the next hardware refresh reports.
func TestMeasurableReturnsFreshSlices(t *testing.T) {
	m := linuxVRAMMeasurer(nvStub, fdinfoStub, nil, joined)
	m.Measurable()[VendorNVIDIA][0] = "bogus"
	m.Measurable()[VendorAMD][0] = "bogus"
	if want := (map[string][]string{VendorNVIDIA: {"cuda", "vulkan"}, VendorAMD: {"rocm", "vulkan"}}); !reflect.DeepEqual(m.Measurable(), want) {
		t.Fatal(m.Measurable())
	}
}

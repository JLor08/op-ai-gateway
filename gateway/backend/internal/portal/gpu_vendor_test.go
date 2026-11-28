// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"errors"
	"testing"
)

// TestNormalizeGPUVendor pins the save-time rule for one GPU's vendor:
// lower-cased, one of the wire values, refused for an agent known to lack
// gpu_vendor_identity, and an empty vendor filled with the stored legacy
// vendor for an agent that declares the flag.
func TestNormalizeGPUVendor(t *testing.T) {
	cases := []struct {
		name           string
		vendor         string
		flagged, known bool
		legacy, want   string
		wantErr        error
	}{
		{name: "upper case is lowered", vendor: "AMD", flagged: true, known: true, want: "amd"},
		{name: "surrounding space is trimmed", vendor: " Nvidia ", flagged: true, known: true, want: "nvidia"},
		{name: "unknown vendor", vendor: "intel", flagged: true, known: true, wantErr: ErrRuntimeSpecGPUVendorInvalid},
		{name: "unknown vendor before the flag check", vendor: "intel", known: true, wantErr: ErrRuntimeSpecGPUVendorInvalid},
		{name: "vendor for an unflagged agent", vendor: "amd", known: true, wantErr: ErrRuntimeSpecGPUVendorUnsupported},
		{name: "foreign vendor for an unflagged agent", vendor: "amd", known: true, legacy: "nvidia", wantErr: ErrRuntimeSpecGPUVendorUnsupported},
		{name: "legacy vendor for an unflagged agent is kept", vendor: "NVIDIA", known: true, legacy: "nvidia", want: "nvidia"},
		{name: "vendor for an agent not heard from", vendor: "amd", want: "amd"},
		{name: "empty for an unflagged agent stays empty", vendor: "", known: true, legacy: "nvidia", want: ""},
		{name: "empty for a flagged agent is the legacy vendor", vendor: "", flagged: true, known: true, legacy: "nvidia", want: "nvidia"},
		{name: "empty without a legacy vendor stays empty", vendor: "", flagged: true, known: true, want: ""},
		{name: "explicit vendor beats the legacy one", vendor: "amd", flagged: true, known: true, legacy: "nvidia", want: "amd"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeGPUVendor(tc.vendor, tc.flagged, tc.known, tc.legacy)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("vendor = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPushedGPUVendor pins which GPU rows a runtime document carries for an
// agent with and without gpu_vendor_identity, and with which vendor.
func TestPushedGPUVendor(t *testing.T) {
	cases := []struct {
		name     string
		vendor   string
		flagged  bool
		legacy   string
		wantOut  string
		wantKeep bool
	}{
		{name: "flagged keeps the vendor", vendor: "amd", flagged: true, legacy: "nvidia", wantOut: "amd", wantKeep: true},
		{name: "flagged keeps an empty vendor", vendor: "", flagged: true, wantOut: "", wantKeep: true},
		{name: "unflagged keeps an empty row", vendor: "", legacy: "nvidia", wantOut: "", wantKeep: true},
		{name: "unflagged keeps the legacy vendor's row without its vendor", vendor: "nvidia", legacy: "nvidia", wantOut: "", wantKeep: true},
		{name: "unflagged drops a foreign vendor's row", vendor: "amd", legacy: "nvidia", wantOut: "", wantKeep: false},
		{name: "unflagged without a legacy vendor keeps only empty rows", vendor: "nvidia", wantOut: "", wantKeep: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, keep := pushedGPUVendor(tc.vendor, tc.flagged, tc.legacy)
			if out != tc.wantOut || keep != tc.wantKeep {
				t.Fatalf("pushedGPUVendor(%q, %v, %q) = (%q, %v), want (%q, %v)", tc.vendor, tc.flagged, tc.legacy, out, keep, tc.wantOut, tc.wantKeep)
			}
		})
	}
}

// TestResolveSpecBackend pins the portal's mirror of the agent's per-spec
// backend rule (resolveDevicePrefix): auto resolves by the spec's own GPU
// vendors, a native backend that cannot drive one of them is a mismatch,
// and SYCL cannot span NVIDIA and AMD.
func TestResolveSpecBackend(t *testing.T) {
	for _, tc := range []struct {
		backend      string
		vendors      []string
		want         string
		wantMismatch bool
	}{
		{"auto", []string{"nvidia"}, "cuda", false},
		{"auto", []string{"amd"}, "rocm", false},
		{"auto", []string{"apple"}, "metal", false},
		{"", []string{"amd", "amd"}, "rocm", false},
		{"auto", []string{"nvidia", "amd"}, "vulkan", false},
		{"auto", nil, "auto", false},
		{"auto", []string{"", ""}, "auto", false},
		{"auto", []string{"", "amd"}, "rocm", false},
		{"cuda", []string{"nvidia", "amd"}, "cuda", true},
		{"rocm", []string{"nvidia"}, "rocm", true},
		{"metal", []string{"amd"}, "metal", true},
		{"cuda", []string{"nvidia", ""}, "cuda", false},
		{"cuda", nil, "cuda", false},
		{"vulkan", []string{"nvidia", "amd"}, "vulkan", false},
		{"sycl", []string{"nvidia", "amd"}, "sycl", true},
		{"sycl", []string{"amd"}, "sycl", false},
		{" ROCm ", []string{"amd"}, "rocm", false},
	} {
		got, mismatch := resolveSpecBackend(tc.backend, tc.vendors)
		if got != tc.want || mismatch != tc.wantMismatch {
			t.Errorf("resolveSpecBackend(%q, %v) = (%q, %v), want (%q, %v)", tc.backend, tc.vendors, got, mismatch, tc.want, tc.wantMismatch)
		}
	}
}

// TestGPULooksNVIDIAPrefersVendor: a reported vendor decides; the name
// heuristic is only for vendor-less reports of older agents.
func TestGPULooksNVIDIAPrefersVendor(t *testing.T) {
	if gpuLooksNVIDIA("amd", "NVIDIA GeForce RTX 4090", "00000000:01:00.0") {
		t.Fatal("vendor amd with an NVIDIA name looks NVIDIA")
	}
	if !gpuLooksNVIDIA("nvidia", "AMD-branded board", "") {
		t.Fatal("vendor nvidia with an AMD-looking name does not look NVIDIA")
	}
	if gpuLooksNVIDIA("apple", "Apple M2", "00000000:01:00.0") {
		t.Fatal("vendor apple with a bus id looks NVIDIA")
	}
	if !gpuLooksNVIDIA("", "H100 PCIe", "00000000:41:00.0") {
		t.Fatal("vendor-less keyword-less name with a bus id: heuristic not applied")
	}
	if gpuLooksAMD("nvidia", "AMD Radeon RX 7900") {
		t.Fatal("vendor nvidia with an AMD name looks AMD")
	}
	if !gpuLooksAMD("AMD", "Some Card") {
		t.Fatal("vendor AMD does not look AMD")
	}
	if !gpuLooksAMD("", "Radeon PRO W7900") {
		t.Fatal("vendor-less Radeon: heuristic not applied")
	}
}

// TestGPUMeasurable: a reported vram_measurable list decides for the
// resolved backend; without one, today's kfdHost rule.
func TestGPUMeasurable(t *testing.T) {
	listed := hostGPU{vendor: "amd", kfdHost: true, measurable: []string{"rocm"}, hasMeasurable: true}
	if !gpuMeasurable(listed, "rocm") || gpuMeasurable(listed, "vulkan") || gpuMeasurable(listed, "auto") {
		t.Fatalf("listed [rocm]: rocm %v vulkan %v auto %v", gpuMeasurable(listed, "rocm"), gpuMeasurable(listed, "vulkan"), gpuMeasurable(listed, "auto"))
	}
	none := hostGPU{vendor: "amd", kfdHost: true, hasMeasurable: true}
	if gpuMeasurable(none, "rocm") {
		t.Fatal("a vendor-reporting agent listing nothing is measurable")
	}
	legacy := hostGPU{kfdHost: true}
	if !gpuMeasurable(legacy, "rocm") || !gpuMeasurable(legacy, "auto") || gpuMeasurable(legacy, "vulkan") {
		t.Fatal("kfdHost rule changed for reports without the list")
	}
	if gpuMeasurable(hostGPU{}, "rocm") {
		t.Fatal("non-kfd host without the list is measurable")
	}
}

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

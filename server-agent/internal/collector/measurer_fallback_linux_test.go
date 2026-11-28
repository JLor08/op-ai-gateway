// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"os"
	"testing"
)

// TestLinuxFallbackGatesAMDOnTheCollectorJoin pins the production wiring of
// the join gate on any runner: while the AMD collector has published no PCI
// join, the Linux measurer claims no AMD measurability and measures no AMD
// GPU, whatever sources this host has. The pure assembly is covered with
// fakes by the linuxVRAMMeasurer tests.
func TestLinuxFallbackGatesAMDOnTheCollectorJoin(t *testing.T) {
	prev := amdIndex.Load()
	t.Cleanup(func() { amdIndex.Store(prev) })
	amdIndex.Store(nil)

	m := fallbackVRAMMeasurer()
	if m.Measurable == nil {
		t.Fatal("Measurable is nil; it must always be callable")
	}
	if got, ok := m.Measurable()[VendorAMD]; ok {
		t.Fatalf("Measurable()[amd] = %v without a join, want absent", got)
	}
	if m.Measure == nil {
		return
	}
	for _, byGPU := range m.Measure([]int{os.Getpid()}) {
		for k := range byGPU {
			if k.Vendor == VendorAMD {
				t.Fatalf("measured %v without a join", k)
			}
		}
	}
}

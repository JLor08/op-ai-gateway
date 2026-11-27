// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"testing"
)

// fakeGPUCollector returns a fixed GPU with a driver version.
type fakeGPUCollector struct{}

func (fakeGPUCollector) Name() string    { return "fake" }
func (fakeGPUCollector) Available() bool { return true }
func (fakeGPUCollector) Collect(context.Context) ([]sample.GPU, error) {
	return []sample.GPU{{Index: 0, Name: "FakeGPU", UUID: "GPU-fake", DriverVersion: "1.2.3", MemTotalBytes: 8 << 30}}, nil
}

func TestCollectHardware(t *testing.T) {
	r := CollectHardware(context.Background(), []GPUCollector{fakeGPUCollector{}}, "9.9.9", HardwareOptions{})
	if r.AgentVersion != "9.9.9" {
		t.Fatalf("agent_version = %q", r.AgentVersion)
	}
	if r.Arch == "" {
		t.Fatal("arch must be set from runtime.GOARCH")
	}
	// gopsutil returns logical CPU count on every supported OS (incl. the test host).
	if r.CPU.LogicalThreads <= 0 {
		t.Fatalf("logical_threads = %d, want > 0", r.CPU.LogicalThreads)
	}
	if len(r.GPUs) != 1 || r.GPUs[0].Name != "FakeGPU" || r.GPUs[0].DriverVersion != "1.2.3" {
		t.Fatalf("gpus = %#v", r.GPUs)
	}
	if r.GPUs[0].MemoryTotalBytes != 8<<30 {
		t.Fatalf("gpu vram mapping wrong: %d", r.GPUs[0].MemoryTotalBytes)
	}
	// GPUs slice is always non-nil (Normalize).
	if r.GPUs == nil {
		t.Fatal("GPUs must be non-nil")
	}
}

func TestHelperMemoryModulesNeeded(t *testing.T) {
	origGOOS, origEuid := helperGOOS, helperGeteuid
	t.Cleanup(func() { helperGOOS, helperGeteuid = origGOOS, origEuid })
	withModules := sample.SystemReport{}
	withModules.Memory.Modules = []sample.MemoryModule{{Locator: "DIMM_A1"}}
	cases := []struct {
		name   string
		goos   string
		euid   int
		socket string
		report sample.SystemReport
		want   bool
	}{
		{"linux unprivileged, no modules", "linux", 1000, "/s", sample.SystemReport{}, true},
		{"modules already present", "linux", 1000, "/s", withModules, false},
		{"root reads natively", "linux", 0, "/s", sample.SystemReport{}, false},
		{"no socket", "linux", 1000, "", sample.SystemReport{}, false},
		{"not linux", "darwin", 1000, "/s", sample.SystemReport{}, false},
	}
	for _, tc := range cases {
		helperGOOS, helperGeteuid = tc.goos, func() int { return tc.euid }
		if got := HelperMemoryModulesNeeded(tc.report, HardwareOptions{HelperSocket: tc.socket}); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// fixedGPUCollector returns the GPUs it was given.
type fixedGPUCollector struct{ gpus []sample.GPU }

func (fixedGPUCollector) Name() string    { return "fixed" }
func (fixedGPUCollector) Available() bool { return true }
func (f fixedGPUCollector) Collect(context.Context) ([]sample.GPU, error) {
	return f.gpus, nil
}

func TestCollectHardwareCopiesVendor(t *testing.T) {
	c := fixedGPUCollector{gpus: []sample.GPU{{Index: 1, Vendor: VendorAMD, PCIBusID: "0000:02:00.0"}}}
	r := CollectHardware(context.Background(), []GPUCollector{c}, "x", HardwareOptions{})
	if len(r.GPUs) != 1 || r.GPUs[0].Vendor != VendorAMD || r.GPUs[0].PCIBusID != "0000:02:00.0" {
		t.Fatalf("%+v", r.GPUs)
	}
}

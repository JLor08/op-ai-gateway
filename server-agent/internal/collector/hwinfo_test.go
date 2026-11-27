// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"slices"
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
	r := CollectHardware(context.Background(), []GPUCollector{fakeGPUCollector{}}, "9.9.9", HardwareOptions{Vulkan: noVulkan})
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
	r := CollectHardware(context.Background(), []GPUCollector{c}, "x", HardwareOptions{Vulkan: noVulkan})
	if len(r.GPUs) != 1 || r.GPUs[0].Vendor != VendorAMD || r.GPUs[0].PCIBusID != "0000:02:00.0" {
		t.Fatalf("%+v", r.GPUs)
	}
}

func noVulkan(context.Context) ([]VulkanDevice, bool) { return nil, false }

func vulkanFromFixture(t *testing.T, name string) func(context.Context) ([]VulkanDevice, bool) {
	return func(context.Context) ([]VulkanDevice, bool) {
		return parseVulkaninfo([]byte(readTestdata(t, name))), true
	}
}

func TestCollectHardwareFillsMeasurabilityAndVulkanOrdinal(t *testing.T) {
	opts := HardwareOptions{
		VRAMMeasurable: func() map[string][]string { return map[string][]string{"amd": {"rocm", "vulkan"}} },
		Vulkan:         vulkanFromFixture(t, "vulkaninfo-amd-2gpu.txt"),
	}
	c := fixedGPUCollector{gpus: []sample.GPU{{Index: 1, Vendor: VendorAMD, PCIBusID: "0000:02:00.0"}}}
	r := CollectHardware(context.Background(), []GPUCollector{c}, "x", opts)
	g := r.GPUs[0]
	if g.VulkanOrdinal == nil || *g.VulkanOrdinal != 1 || !slices.Equal(g.VRAMMeasurable, []string{"rocm", "vulkan"}) || r.VulkanDeviceCount != 3 {
		t.Fatalf("%+v count=%d", g, r.VulkanDeviceCount)
	}
}

func TestCollectHardwareWithoutVulkaninfo(t *testing.T) {
	opts := HardwareOptions{
		Vulkan:         noVulkan,
		VRAMMeasurable: func() map[string][]string { return map[string][]string{"nvidia": {"cuda"}} },
	}
	c := fixedGPUCollector{gpus: []sample.GPU{{Index: 1, Vendor: VendorAMD, PCIBusID: "0000:02:00.0"}}}
	r := CollectHardware(context.Background(), []GPUCollector{c}, "x", opts)
	if g := r.GPUs[0]; g.VulkanOrdinal != nil || g.VRAMMeasurable != nil || r.VulkanDeviceCount != 0 {
		t.Fatalf("%+v count=%d", g, r.VulkanDeviceCount)
	}
}

func TestCollectHardwareNVIDIAVulkanCount(t *testing.T) {
	opts := HardwareOptions{Vulkan: vulkanFromFixture(t, "vulkaninfo-nvidia-2gpu.txt")}
	c := fixedGPUCollector{gpus: []sample.GPU{
		{Index: 0, Vendor: VendorNVIDIA, PCIBusID: "0000:01:00.0"},
		{Index: 1, Vendor: VendorNVIDIA, PCIBusID: "0000:02:00.0"},
	}}
	r := CollectHardware(context.Background(), []GPUCollector{c}, "x", opts)
	if r.VulkanDeviceCount != 2 {
		t.Fatalf("count = %d", r.VulkanDeviceCount)
	}
	for i, g := range r.GPUs {
		if g.VulkanOrdinal == nil || *g.VulkanOrdinal != i {
			t.Fatalf("gpu %d: %+v", i, g)
		}
	}
}

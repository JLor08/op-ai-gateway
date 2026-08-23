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
	r := CollectHardware(context.Background(), []GPUCollector{fakeGPUCollector{}}, "9.9.9")
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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package agent

import (
	"context"
	"op-ai-server-agent/internal/collector"
	"op-ai-server-agent/internal/config"
	runtimectl "op-ai-server-agent/internal/runtime"
	"op-ai-server-agent/internal/sample"
	"sync"
	"testing"
	"time"
)

// lateGPUCollector reports no GPUs for its first empty calls, then gpus: an
// amdgpu driver that finishes initializing its cards seconds after the agent
// started and collected its inventory.
type lateGPUCollector struct {
	mu    sync.Mutex
	calls int
	empty int
	gpus  []sample.GPU
}

func (*lateGPUCollector) Name() string    { return "amd" }
func (*lateGPUCollector) Available() bool { return true }
func (c *lateGPUCollector) Collect(context.Context) ([]sample.GPU, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= c.empty {
		return nil, nil
	}
	return c.gpus, nil
}

func (c *lateGPUCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func setGPUInventoryRetryInterval(t *testing.T, d time.Duration) {
	t.Helper()
	orig := gpuInventoryRetryInterval
	gpuInventoryRetryInterval = d
	t.Cleanup(func() { gpuInventoryRetryInterval = orig })
}

var twoAMDCards = []sample.GPU{
	{Vendor: collector.VendorAMD, Index: 0, Name: "Radeon", MemTotalBytes: 16 << 30},
	{Vendor: collector.VendorAMD, Index: 1, Name: "Radeon", MemTotalBytes: 16 << 30},
}

// The boot race: the startup inventory finds no cards, telemetry a moment
// later finds two. The agent re-collects, re-sends the report once, and stops
// once the inventory covers what telemetry sees.
func TestGPUInventoryRecollectsWhenTelemetrySeesMissingGPUs(t *testing.T) {
	setMemoryRetryInterval(t, 5*time.Millisecond)
	setGPUInventoryRetryInterval(t, 5*time.Millisecond)
	frp := &fakeReporterPoster{}
	gpu := &lateGPUCollector{empty: 1, gpus: twoAMDCards}
	cfg := config.Config{Interval: 5 * time.Millisecond, SystemReportInterval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: frp, GPUs: []collector.GPUCollector{gpu}})

	runAgentFor(t, a, func() bool { return frp.reportCount() >= 2 })

	if n := frp.reportCount(); n != 2 {
		t.Fatalf("system reports = %d, want 2 (startup + one after the re-collect, then no more)", n)
	}
	frp.mu.Lock()
	first, last := frp.reports[0], frp.reports[1]
	frp.mu.Unlock()
	if len(first.GPUs) != 0 {
		t.Fatalf("startup report gpus = %+v, want none (the race)", first.GPUs)
	}
	if len(last.GPUs) != 2 || last.GPUs[0].Vendor != collector.VendorAMD || last.GPUs[1].Index != 1 {
		t.Fatalf("re-sent report gpus = %+v, want both AMD cards", last.GPUs)
	}
	if len(a.hw.GPUs) != 2 {
		t.Fatalf("cached inventory gpus = %d, want 2", len(a.hw.GPUs))
	}
}

// A host whose inventory already matches telemetry never re-collects.
func TestGPUInventoryNoRecollectWhenConsistent(t *testing.T) {
	setMemoryRetryInterval(t, 5*time.Millisecond)
	setGPUInventoryRetryInterval(t, time.Millisecond)
	frp := &fakeReporterPoster{}
	gpu := &lateGPUCollector{gpus: twoAMDCards}
	cfg := config.Config{Interval: 5 * time.Millisecond, SystemReportInterval: time.Hour}
	a := NewFromDeps(cfg, Deps{Poster: frp, GPUs: []collector.GPUCollector{gpu}})

	runAgentFor(t, a, func() bool { return gpu.count() >= 5 })

	if n := frp.reportCount(); n != 1 {
		t.Fatalf("system reports = %d, want 1 (startup only)", n)
	}
}

// When the re-collect keeps missing the cards telemetry sees, the agent gives
// up after gpuInventoryRetryMax attempts instead of re-collecting forever.
func TestGPUInventoryRecollectIsBounded(t *testing.T) {
	setGPUInventoryRetryInterval(t, 0)
	frp := &fakeReporterPoster{}
	gpu := &lateGPUCollector{empty: 1 << 30}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: frp, GPUs: []collector.GPUCollector{gpu}})
	a.hw = &sample.SystemReport{}
	a.telemetryGPUs = []runtimectl.GPUKey{{Vendor: runtimectl.GPUVendorAMD, Index: 0}}

	for range 3 * gpuInventoryRetryMax {
		a.refreshInventoryForMissingGPUs(context.Background())
	}

	if n := gpu.count(); n != gpuInventoryRetryMax {
		t.Fatalf("inventory re-collects = %d, want %d", n, gpuInventoryRetryMax)
	}
	if n := frp.reportCount(); n != 0 {
		t.Fatalf("system reports = %d, want 0 (nothing new was found)", n)
	}
}

// Re-collects are spaced by gpuInventoryRetryInterval, so a mismatch seen on
// every 1 s telemetry tick does not re-run the whole inventory each second.
func TestGPUInventoryRecollectIsRateLimited(t *testing.T) {
	setGPUInventoryRetryInterval(t, time.Hour)
	gpu := &lateGPUCollector{empty: 1 << 30}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: &fakeReporterPoster{}, GPUs: []collector.GPUCollector{gpu}})
	a.hw = &sample.SystemReport{}
	a.telemetryGPUs = []runtimectl.GPUKey{{Vendor: runtimectl.GPUVendorAMD, Index: 0}}

	for range 5 {
		a.refreshInventoryForMissingGPUs(context.Background())
	}

	if n := gpu.count(); n != 1 {
		t.Fatalf("inventory re-collects = %d, want 1 within one interval", n)
	}
}

// The re-collected inventory keeps the memory modules the helper retry filled
// in, when the fresh collect comes without them.
func TestGPUInventoryRecollectKeepsHelperMemoryModules(t *testing.T) {
	setGPUInventoryRetryInterval(t, 0)
	frp := &fakeReporterPoster{}
	gpu := &lateGPUCollector{gpus: twoAMDCards[:1]}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: frp, GPUs: []collector.GPUCollector{gpu}})
	mods := []sample.MemoryModule{{Locator: "DIMM_A1", SizeBytes: 16 << 30}}
	hw := sample.SystemReport{}
	hw.Memory.Modules = mods
	a.hw = &hw
	a.telemetryGPUs = []runtimectl.GPUKey{{Vendor: runtimectl.GPUVendorAMD, Index: 0}}

	a.refreshInventoryForMissingGPUs(context.Background())

	if len(a.hw.GPUs) != 1 || frp.reportCount() != 1 {
		t.Fatalf("gpus = %+v, reports = %d; want the card and one report", a.hw.GPUs, frp.reportCount())
	}
	if len(a.hw.Memory.Modules) != 1 || a.hw.Memory.Modules[0].Locator != "DIMM_A1" {
		t.Fatalf("modules = %+v, want the helper's DIMM_A1 kept", a.hw.Memory.Modules)
	}
}

// A re-collect that loses a card the cached inventory had (its collector
// timed out while the driver was still initializing the other card) must not
// replace the cache, even though it is missing a different number of GPUs.
func TestGPUInventoryRecollectNeverDropsCachedGPUs(t *testing.T) {
	setGPUInventoryRetryInterval(t, 0)
	frp := &fakeReporterPoster{}
	gpu := &lateGPUCollector{empty: 1 << 30}
	a := NewFromDeps(config.Config{Interval: time.Hour}, Deps{Poster: frp, GPUs: []collector.GPUCollector{gpu}})
	a.hw = &sample.SystemReport{GPUs: []sample.GPUInfo{{Vendor: collector.VendorAMD, Index: 0}}}
	a.telemetryGPUs = []runtimectl.GPUKey{
		{Vendor: runtimectl.GPUVendorAMD, Index: 0},
		{Vendor: runtimectl.GPUVendorAMD, Index: 1},
	}

	a.refreshInventoryForMissingGPUs(context.Background())

	if len(a.hw.GPUs) != 1 || frp.reportCount() != 0 {
		t.Fatalf("gpus = %+v, reports = %d; want the cached amd:0 kept and nothing sent", a.hw.GPUs, frp.reportCount())
	}
}

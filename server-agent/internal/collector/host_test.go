// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// TestHostCollectorReal exercises the real gopsutil host collector on the CI
// hardware (no mocks): two collects with a short gap, then sanity-check the
// derived host metrics.
func TestHostCollectorReal(t *testing.T) {
	h := NewHostCollector()
	ctx := context.Background()

	if _, err := h.Collect(ctx); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	time.Sleep(50 * time.Millisecond)

	host, err := h.Collect(ctx)
	if err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if host == nil {
		t.Fatal("Collect returned nil host")
	}

	if host.MemTotalBytes <= 0 {
		t.Errorf("MemTotalBytes = %d, want > 0", host.MemTotalBytes)
	}
	if host.MemUsedBytes < 0 || host.MemUsedBytes > host.MemTotalBytes {
		t.Errorf("MemUsedBytes = %d, want in [0, %d]", host.MemUsedBytes, host.MemTotalBytes)
	}

	// CPUUtilPct is now the MEAN of the per-core values, so it is in [0,100].
	if host.CPUUtilPct < 0 || host.CPUUtilPct > 100 {
		t.Errorf("CPUUtilPct = %f, want in [0, 100] (mean of cores)", host.CPUUtilPct)
	}
	// Per-core utilization: one entry per logical CPU, each in [0,100].
	if len(host.CPUCores) != runtime.NumCPU() {
		t.Errorf("len(CPUCores) = %d, want %d (NumCPU)", len(host.CPUCores), runtime.NumCPU())
	}
	for i, v := range host.CPUCores {
		if v < 0 || v > 100 {
			t.Errorf("CPUCores[%d] = %f, want in [0, 100]", i, v)
		}
	}

	if len(host.Net) < 1 {
		t.Fatalf("len(Net) = %d, want >= 1", len(host.Net))
	}
	if host.Net[0].Name != "total" {
		t.Errorf("Net[0].Name = %q, want %q", host.Net[0].Name, "total")
	}
}

// TestCollectHostSetsMemAvailable pins that the host sample carries the live
// available-RAM figure (gopsutil VirtualMemoryStat.Available -- MemAvailable on
// Linux, free+inactive/purgeable on macOS, the standby+free pool on Windows),
// not just used/total. RAM admission budgets against what is genuinely
// obtainable right now, which is not total-minus-used on a host whose page
// cache is large. It runs against the real collector on the CI hardware, like
// TestHostCollectorReal.
func TestCollectHostSetsMemAvailable(t *testing.T) {
	host, err := NewHostCollector().Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if host.MemAvailableBytes <= 0 {
		t.Errorf("MemAvailableBytes = %d, want > 0", host.MemAvailableBytes)
	}
	if host.MemAvailableBytes > host.MemTotalBytes {
		t.Errorf("MemAvailableBytes = %d, want <= MemTotalBytes %d", host.MemAvailableBytes, host.MemTotalBytes)
	}
}

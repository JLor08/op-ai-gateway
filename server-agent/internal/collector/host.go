// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// hostCollector reads host CPU/memory/swap/load/network metrics via gopsutil.
type hostCollector struct{}

// NewHostCollector returns a real gopsutil-backed HostCollector. It primes the
// cpu.Percent delta window once so the first Collect returns a real reading
// instead of an initial zero.
func NewHostCollector() HostCollector {
	// Seed the delta window (per-core); the first real Collect then measures
	// against it. gopsutil keys the delta on the (percpu) call shape, so seed
	// with the same percpu=true used in Collect.
	_, _ = cpu.Percent(0, true)
	return &hostCollector{}
}

// Collect gathers a host snapshot. It never fails the whole collect for one
// missing metric: an unsupported subsystem (e.g. load average) degrades to 0
// while the rest of the host is still populated.
func (c *hostCollector) Collect(ctx context.Context) (*sample.Host, error) {
	h := &sample.Host{}

	// One per-core call: CPUCores is the per-core slice; the aggregate CPUUtilPct
	// is their mean (a second cpu.Percent call would see ~0 elapsed and return 0).
	if pct, err := cpu.PercentWithContext(ctx, 0, true); err == nil && len(pct) > 0 {
		cores := make([]float64, len(pct))
		sum := 0.0
		for i, v := range pct {
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			cores[i] = v
			sum += v
		}
		h.CPUCores = cores
		h.CPUUtilPct = sum / float64(len(cores))
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		h.MemUsedBytes = int64(vm.Used)
		h.MemTotalBytes = int64(vm.Total)
	}

	if sw, err := mem.SwapMemoryWithContext(ctx); err == nil && sw != nil {
		h.SwapUsedBytes = int64(sw.Used)
		h.SwapTotalBytes = int64(sw.Total)
	}

	// Load average is unsupported on some platforms; default to 0 on error.
	if avg, err := load.AvgWithContext(ctx); err == nil && avg != nil {
		h.Load1 = avg.Load1
		h.Load5 = avg.Load5
		h.Load15 = avg.Load15
	}

	if counters, err := psnet.IOCountersWithContext(ctx, false); err == nil && len(counters) > 0 {
		agg := counters[0]
		h.Net = []sample.Net{{
			Name:    "total",
			RxBytes: int64(agg.BytesRecv),
			TxBytes: int64(agg.BytesSent),
		}}
	}

	return h, nil
}

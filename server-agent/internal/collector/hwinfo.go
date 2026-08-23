// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// This file collects a static hardware inventory (CPU/RAM/OS via gopsutil + GPUs
// via the detected GPU collectors + per-OS mainboard/BIOS/DIMMs). It is build-tag-
// free; the per-OS mainboard/BIOS/DIMM detail comes from platformHardware, defined
// in hwinfo_{linux,windows,darwin,other}.go.
package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// hwCollectTimeout bounds each external source (a GPU CLI, system_profiler,
// dmidecode) so a wedged tool cannot hang collection. Matches the telemetry
// collectors' per-source budget.
const hwCollectTimeout = 2 * time.Second

// CollectHardware gathers a static SystemReport: CPU/RAM/OS from gopsutil, per-OS
// mainboard/BIOS/DIMMs from platformHardware, and GPUs from the detected GPU
// collectors (mapped to GPUInfo, including driver_version). Best-effort: any
// missing source leaves its field blank; the report is always produced.
func CollectHardware(ctx context.Context, gpuCollectors []GPUCollector, agentVersion string) sample.SystemReport {
	r := sample.SystemReport{
		CollectedAt:  time.Now().UTC(),
		AgentVersion: agentVersion,
		OS:           runtime.GOOS,
		Arch:         runtime.GOARCH,
		GPUs:         []sample.GPUInfo{},
	}

	if infos, err := cpu.InfoWithContext(ctx); err == nil && len(infos) > 0 {
		r.CPU.Model = strings.TrimSpace(infos[0].ModelName)
		r.CPU.Vendor = strings.TrimSpace(infos[0].VendorID)
		r.CPU.BaseMHz = infos[0].Mhz
	}
	// cpu.Counts(false)=physical cores, cpu.Counts(true)=logical threads. Do NOT
	// use InfoStat.Cores (that is cores-per-package, not a total).
	if n, err := cpu.CountsWithContext(ctx, false); err == nil && n > 0 {
		r.CPU.PhysicalCores = n
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil && n > 0 {
		r.CPU.LogicalThreads = n
	}

	if vm, err := mem.VirtualMemoryWithContext(ctx); err == nil && vm != nil {
		r.Memory.TotalBytes = int64(vm.Total)
	}

	if hi, err := host.InfoWithContext(ctx); err == nil && hi != nil {
		if plat := strings.TrimSpace(hi.Platform + " " + hi.PlatformVersion); strings.TrimSpace(plat) != "" {
			r.OS = plat
		}
		r.Kernel = strings.TrimSpace(hi.KernelVersion)
	}
	if hn, err := os.Hostname(); err == nil {
		r.Hostname = hn
	}

	board, bios, modules := platformHardware(ctx)
	r.Mainboard = board
	r.BIOS = bios
	if len(modules) > 0 {
		r.Memory.Modules = modules
	}

	for _, g := range gpuCollectors {
		cctx, cancel := context.WithTimeout(ctx, hwCollectTimeout)
		gpus, err := g.Collect(cctx)
		cancel()
		if err != nil {
			continue
		}
		for _, gp := range gpus {
			r.GPUs = append(r.GPUs, sample.GPUInfo{
				Index:            gp.Index,
				Name:             gp.Name,
				UUID:             gp.UUID,
				DriverVersion:    gp.DriverVersion,
				MemoryTotalBytes: gp.MemTotalBytes,
			})
		}
	}

	r.Normalize()
	return r
}

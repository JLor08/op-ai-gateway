// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build darwin

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"os/exec"
)

// platformHardware shells out to system_profiler (CGO-free) for mainboard/BIOS and
// per-DIMM detail. Apple Silicon reports unified memory (no modules). Best-effort.
func platformHardware(ctx context.Context) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	var board sample.Mainboard
	var bios sample.BIOS
	if out, err := runSystemProfiler(ctx, "SPHardwareDataType"); err == nil {
		board, bios = parseSPHardware(out)
	}
	var modules []sample.MemoryModule
	if out, err := runSystemProfiler(ctx, "SPMemoryDataType"); err == nil {
		modules = parseSPMemory(out)
	}
	return board, bios, modules
}

func runSystemProfiler(ctx context.Context, dataType string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, hwCollectTimeout)
	defer cancel()
	return exec.CommandContext(cctx, "system_profiler", "-json", dataType).Output()
}

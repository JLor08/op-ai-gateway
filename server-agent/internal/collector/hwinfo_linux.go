// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"os"
	"os/exec"
)

// defaultDMIRoot is the Linux SMBIOS/DMI sysfs directory (all 0444 world-readable).
const defaultDMIRoot = "/sys/class/dmi/id"

// platformHardware reads mainboard/BIOS from sysfs DMI and per-DIMM detail from
// dmidecode when running as root (else RAM total only). Best-effort.
func platformHardware(ctx context.Context) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	board, bios := newDMIReader(defaultDMIRoot).read()
	return board, bios, linuxMemoryModules(ctx)
}

// linuxMemoryModules returns per-DIMM detail via `dmidecode -t memory`, but only
// when euid==0 (dmidecode reads /dev/mem, which needs root) AND dmidecode is on
// PATH. Otherwise nil (total-only, documented best-effort). Never errors.
func linuxMemoryModules(ctx context.Context) []sample.MemoryModule {
	if os.Geteuid() != 0 {
		return nil
	}
	if _, err := exec.LookPath("dmidecode"); err != nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, hwCollectTimeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "dmidecode", "-t", "memory").Output()
	if err != nil {
		return nil
	}
	return parseDmidecodeMemory(out)
}

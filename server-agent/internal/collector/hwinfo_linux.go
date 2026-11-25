// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
	"os"
)

// defaultDMIRoot is the Linux SMBIOS/DMI sysfs directory (all 0444 world-readable).
const defaultDMIRoot = "/sys/class/dmi/id"

// platformHardware reads mainboard/BIOS from sysfs DMI and per-DIMM detail from
// the raw SMBIOS table when running as root (else RAM total only). Best-effort.
func platformHardware(ctx context.Context) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	board, bios := newDMIReader(defaultDMIRoot).read()
	return board, bios, linuxMemoryModules(ctx)
}

// linuxMemoryModules returns per-DIMM detail by parsing the raw SMBIOS table,
// which only root can read. Otherwise nil (total-only, documented best-effort).
// Never errors.
func linuxMemoryModules(_ context.Context) []sample.MemoryModule {
	if os.Geteuid() != 0 {
		return nil
	}
	mods, err := ReadSMBIOSMemoryModules(DefaultDMITablePath)
	if err != nil {
		return nil
	}
	return mods
}

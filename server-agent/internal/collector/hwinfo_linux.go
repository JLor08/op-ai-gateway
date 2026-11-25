// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
)

// defaultDMIRoot is the Linux SMBIOS/DMI sysfs directory (all 0444 world-readable).
const defaultDMIRoot = "/sys/class/dmi/id"

// platformHardware reads mainboard/BIOS from sysfs DMI and per-DIMM detail from
// the raw SMBIOS table (root) or the privileged helper (else RAM total only).
// Best-effort.
func platformHardware(ctx context.Context, opts HardwareOptions) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	board, bios := newDMIReader(defaultDMIRoot).read()
	return board, bios, linuxMemoryModules(ctx, opts)
}

// linuxMemoryModules returns per-DIMM detail. Root parses the raw SMBIOS table
// itself; an unprivileged agent asks the privileged helper at opts.HelperSocket
// within hwCollectTimeout. Otherwise, or on any failure, nil (total-only,
// documented best-effort). Never errors.
func linuxMemoryModules(ctx context.Context, opts HardwareOptions) []sample.MemoryModule {
	if helperGeteuid() == 0 {
		mods, err := ReadSMBIOSMemoryModules(DefaultDMITablePath)
		if err != nil {
			return nil
		}
		return mods
	}
	if !helperEnabled(opts.HelperSocket) {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, hwCollectTimeout)
	defer cancel()
	mods, err := FetchHelperMemoryModules(cctx, NewHelperHTTPClient(opts.HelperSocket))
	if err != nil || len(mods) == 0 {
		return nil
	}
	return mods
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux && !windows && !darwin

package collector

import (
	"context"
	"op-ai-server-agent/internal/sample"
)

// platformHardware is a no-op on unsupported OSes: CPU/RAM/OS still come from
// gopsutil in CollectHardware; only mainboard/BIOS/DIMM detail is unavailable.
func platformHardware(_ context.Context) (sample.Mainboard, sample.BIOS, []sample.MemoryModule) {
	return sample.Mainboard{}, sample.BIOS{}, nil
}

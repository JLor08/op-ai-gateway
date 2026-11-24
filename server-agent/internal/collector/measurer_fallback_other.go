// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows && !linux

package collector

// fallbackVRAMMeasurer is the choice behind NewVRAMMeasurer on every platform
// that is neither Windows nor Linux: nvidia-smi compute-apps, or nil. KFD is a
// Linux kernel interface, so there is no second source here.
func fallbackVRAMMeasurer() func(pids []int) map[int]map[int]int {
	return NewNvidiaComputeApps()
}

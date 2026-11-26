// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package helper

import "golang.org/x/sys/unix"

// monotonicNS returns the system-wide CLOCK_MONOTONIC time in nanoseconds: the
// same clock for every process on the host, unlike Go's process-relative
// monotonic reading. 0 if the clock cannot be read (the agent then falls back
// to its own clock).
func monotonicNS() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return uint64(ts.Nano()) //nolint:gosec // G115: CLOCK_MONOTONIC is never negative
}

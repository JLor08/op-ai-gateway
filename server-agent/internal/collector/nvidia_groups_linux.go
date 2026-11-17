// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"math"
	"syscall"
)

// groupNvidiaComputeApps uses the same process-group boundary as runtime
// termination: each managed root is its group's leader. Detached groups are
// deliberately excluded, just as they are from the manager's group signals.
func groupNvidiaComputeApps(rows []nvidiaComputeAppRow) []nvidiaComputeAppRow {
	out := rows[:0]
	for _, row := range rows {
		if row.PID <= 0 || row.PID > math.MaxInt32 {
			continue
		}
		pgid, err := syscall.Getpgid(row.PID)
		if err != nil {
			continue // exited or inaccessible: no measurement, never a guessed owner
		}
		row.PID = pgid
		out = append(out, row)
	}
	return out
}

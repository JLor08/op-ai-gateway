// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package collector

// groupNvidiaComputeApps charges a process_tree "job" member's rows to its
// job's root (#424), the Windows counterpart of the Linux process-group
// grouping; a process in no managed job keeps its own rows. Windows measures
// VRAM through PDH (measurer_windows.go), which applies the same attribution;
// this keeps the compute-apps path consistent should it ever be used here.
func groupNvidiaComputeApps(rows []nvidiaComputeAppRow) []nvidiaComputeAppRow {
	return snapshotProcessTrees().computeAppsToRoots(rows)
}

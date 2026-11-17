// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !linux

package collector

// Other platforms retain their existing per-process attribution.
func groupNvidiaComputeApps(rows []nvidiaComputeAppRow) []nvidiaComputeAppRow {
	return rows
}

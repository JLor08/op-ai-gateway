// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package collector

// NewWSLVMWorkerPIDs returns nil off Windows: there is no WSL VM, so there is
// no VM worker whose GPU memory could be read, and the manager never takes a
// VM figure (runtime.Manager.SetWSLVMPIDs).
func NewWSLVMWorkerPIDs() func() []int {
	return nil
}

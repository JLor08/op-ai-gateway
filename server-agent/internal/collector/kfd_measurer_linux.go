// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build linux

package collector

import (
	"math"
	"os"
	"path/filepath"
	"syscall"
)

// NewKFDMeasurer returns the KFD sysfs per-process VRAM measurer, or nil when
// the host has no KFD GPU to measure (no amdgpu compute driver, or only CPU
// topology nodes). Nil leaves the operator's VRAM estimates standing, exactly
// as on any host without a measurer. It is the AMD fallback: NewVRAMMeasurer
// uses it only when the DRM fdinfo self-probe fails (probeDRMFdinfo), and
// never adds it to fdinfo values.
func NewKFDMeasurer() func(pids []int) map[int]map[int]int {
	ordinals, err := kfdGPUOrdinals(kfdRoot)
	if err != nil || len(ordinals) == 0 {
		return nil
	}
	if st, err := os.Stat(filepath.Join(kfdRoot, "proc")); err != nil || !st.IsDir() {
		return nil
	}
	return kfdMeasurer{root: kfdRoot, groupOf: getpgidOwner}.measure
}

// getpgidOwner maps a pid to its process group, the boundary runtime
// termination signals; an exited or inaccessible pid has no owner rather than
// a guessed one.
func getpgidOwner(pid int) (int, bool) {
	if pid <= 0 || pid > math.MaxInt32 {
		return 0, false
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return 0, false
	}
	return pgid, true
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"math"

	"golang.org/x/sys/windows"
)

// diskFreeMB is the space in MB available to the calling user on the volume
// holding path.
func diskFreeMB(path string) (int, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, &total, &free); err != nil {
		return 0, err
	}
	return int(min(avail>>20, math.MaxInt32)), nil
}

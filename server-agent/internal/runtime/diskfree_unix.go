// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"math"

	"golang.org/x/sys/unix"
)

// diskFreeMB is the space in MB available to an unprivileged writer on the
// volume holding path.
func diskFreeMB(path string) (int, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	mb := st.Bavail * uint64(st.Bsize) >> 20 //nolint:gosec // Bsize is a positive block size
	return int(min(mb, math.MaxInt32)), nil
}

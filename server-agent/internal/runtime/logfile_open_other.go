// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"os"
	"syscall"
)

// openInRoot opens target.Rel read-only inside root for openLogFile. A
// pre-open Root.Stat refuses a target that is not a regular file before
// open(2) can run a device's open side effects; the authoritative check is
// the fstat of the opened descriptor. O_NONBLOCK makes the open itself never
// wait (a FIFO swapped in after the stat would otherwise block until a writer
// appears); it is cleared once the descriptor is known to be a regular file.
// O_NOCTTY keeps a terminal from becoming the agent's controlling terminal.
// Both calls go through root, so neither can leave it (openLogFile's doc).
func openInRoot(root *os.Root, target logFileTarget) (*os.File, error) {
	fi, err := root.Stat(target.Rel)
	if err != nil {
		return nil, classifyInRootErr(target, err)
	}
	if !fi.Mode().IsRegular() {
		return nil, logOpenRefusal(target, "is not a regular file")
	}
	f, err := root.OpenFile(target.Rel, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, classifyInRootErr(target, err)
	}
	if ofi, err := f.Stat(); err != nil || !ofi.Mode().IsRegular() {
		_ = f.Close()
		return nil, logOpenRefusal(target, "is not a regular file")
	}
	if err := clearNonblock(f); err != nil {
		_ = f.Close()
		return nil, openErr(err)
	}
	return f, nil
}

// clearNonblock puts f's descriptor back into blocking mode. Through
// SyscallConn, not Fd, so the os.File's own view of the descriptor stays
// consistent (a regular file is never registered with the poller).
func clearNonblock(f *os.File) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.SetNonblock(int(fd), false) }); err != nil {
		return err
	}
	return serr
}

// openedLinkCount is the hard-link count of the file f is open on (fstat).
func openedLinkCount(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return statNlink(fi)
}

func statNlink(fi os.FileInfo) (uint64, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, os.ErrInvalid
	}
	return uint64(st.Nlink), nil //nolint:unconvert // Nlink is narrower than uint64 on some GOOS
}

// isReparsePoint is a Windows notion; elsewhere a link is ModeSymlink.
func isReparsePoint(os.FileInfo) bool { return false }

// statLogFileName observes rel through root with ONE Root.Stat: its last step
// is an fstatat(AT_SYMLINK_NOFOLLOW) on the final non-link name the root walk
// reached (a link below the root is followed only if it stays below it), so
// identity and link count come from the same stat. The descriptor's identity
// cannot change, so comparing it from a separate fstat is sound.
func statLogFileName(root *os.Root, f *os.File, rel string) (nameBinding, error) {
	ofi, err := f.Stat()
	if err != nil {
		return nameBinding{}, err
	}
	fi, err := root.Stat(rel)
	if err != nil {
		return nameBinding{}, err
	}
	n, err := statNlink(fi)
	if err != nil {
		return nameBinding{}, err
	}
	return nameBinding{same: os.SameFile(ofi, fi), regular: fi.Mode().IsRegular(), nlink: n}, nil
}

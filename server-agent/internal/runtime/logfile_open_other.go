// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"syscall"
)

// openLogFileHandle opens target.OpenPath read-only for openLogFile and
// returns the file with its final path. A pre-open stat refuses a target that
// is not a regular file before open(2) can run a device's open side effects;
// the authoritative check is openLogFile's fstat of the opened descriptor.
// O_NONBLOCK makes the open itself never wait (a FIFO swapped in after the
// stat would otherwise block until a writer appears); it is cleared once the
// descriptor is known to be a regular file. O_NOCTTY keeps a terminal from
// becoming the agent's controlling terminal.
func openLogFileHandle(target logFileTarget) (*os.File, string, error) {
	fi, err := os.Stat(target.OpenPath)
	if err != nil {
		return nil, "", openErr(err)
	}
	if !fi.Mode().IsRegular() {
		return nil, "", logOpenRefusal(target, "", "is not a regular file")
	}
	f, err := os.OpenFile(target.OpenPath, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", openErr(err)
	}
	if ofi, err := f.Stat(); err != nil || !ofi.Mode().IsRegular() {
		_ = f.Close()
		return nil, "", logOpenRefusal(target, "", "is not a regular file")
	}
	if err := clearNonblock(f); err != nil {
		_ = f.Close()
		return nil, "", openErr(err)
	}
	final, err := openedFinalPath(f, target.OpenPath)
	if err != nil {
		_ = f.Close()
		return nil, "", logOpenRefusal(target, "", "has a final path that cannot be determined")
	}
	return f, final, nil
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

// openedFinalPath is the final path of the file f is open on. On Linux it is
// the kernel's own answer for the descriptor (/proc/self/fd/N), so no swap of
// the path after the open can make it name a different file. Elsewhere (and
// on a Linux host without /proc) it is filepath.EvalSymlinks of the path,
// bound to the descriptor by os.SameFile: if the path was swapped between the
// open and the resolution, the resolved path names another file and is
// refused instead of vouching for the wrong one.
func openedFinalPath(f *os.File, openPath string) (string, error) {
	if goruntime.GOOS == "linux" {
		if p, err := fdPath(f); err == nil && filepath.IsAbs(p) {
			return p, nil
		}
	}
	final, err := filepath.EvalSymlinks(openPath)
	if err != nil {
		return "", err
	}
	ofi, err := f.Stat()
	if err != nil {
		return "", err
	}
	rfi, err := os.Lstat(final)
	if err != nil {
		return "", err
	}
	if !os.SameFile(ofi, rfi) {
		return "", os.ErrInvalid
	}
	return final, nil
}

// fdPath reads /proc/self/fd/N for f's descriptor.
func fdPath(f *os.File) (string, error) {
	rc, err := f.SyscallConn()
	if err != nil {
		return "", err
	}
	var p string
	var rerr error
	if err := rc.Control(func(fd uintptr) {
		p, rerr = os.Readlink("/proc/self/fd/" + strconv.FormatUint(uint64(fd), 10))
	}); err != nil {
		return "", err
	}
	return p, rerr
}

// resolveLogRoot resolves an allowed root the way the opened file's final
// path is resolved, so a root that is itself a symlink compares correctly.
func resolveLogRoot(root string) (string, error) {
	return filepath.EvalSymlinks(root)
}

// openedLinkCount is the hard-link count of the file f is open on (fstat).
func openedLinkCount(f *os.File) (uint64, error) {
	fi, err := f.Stat()
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, os.ErrInvalid
	}
	return uint64(st.Nlink), nil //nolint:unconvert // Nlink is narrower than uint64 on some GOOS
}

// isReparsePoint is a Windows notion; elsewhere a link is ModeSymlink.
func isReparsePoint(os.FileInfo) bool { return false }

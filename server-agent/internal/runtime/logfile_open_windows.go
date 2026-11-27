// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"os"
	"syscall"

	"golang.org/x/sys/windows"
)

// openInRoot opens target.Rel read-only inside root for openLogFile.
//
// Share mode (§5.2 step 4): Root.OpenFile reaches internal/syscall/windows
// Openat, which calls NtCreateFile with ShareAccess
// FILE_SHARE_READ|FILE_SHARE_WRITE|FILE_SHARE_DELETE for every open (Go 1.26,
// src/internal/syscall/windows/at_windows.go), so the server can still write,
// rename and delete the file while the agent tails it; there is no flag to
// ask for less. Each component is opened relative to its parent's handle with
// OBJ_DONT_REPARSE, so a junction or symlink is never crossed silently: os.Root
// reads it and follows it only if it stays below the root (a junction's
// target is absolute, so it never does).
//
// O_RDONLY does not exclude directories there (no FILE_NON_DIRECTORY_FILE),
// so the opened handle must be a disk file (GetFileType: not a pipe, a
// console or a device) whose stat is a regular file. os.Root also refuses
// reserved device names such as NUL.
func openInRoot(root *os.Root, target logFileTarget) (*os.File, error) {
	f, err := root.OpenFile(target.Rel, os.O_RDONLY, 0)
	if err != nil {
		return nil, classifyInRootErr(target, err)
	}
	if t, err := windows.GetFileType(windows.Handle(f.Fd())); err != nil || t != windows.FILE_TYPE_DISK {
		_ = f.Close()
		return nil, logOpenRefusal(target, "is not a regular file")
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, logOpenRefusal(target, "is not a regular file")
	}
	return f, nil
}

// openedLinkCount is the hard-link count of the file f is open on
// (GetFileInformationByHandle's NumberOfLinks).
func openedLinkCount(f *os.File) (uint64, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil {
		return 0, err
	}
	return uint64(info.NumberOfLinks), nil
}

// isReparsePoint reports FILE_ATTRIBUTE_REPARSE_POINT (a symlink, a junction,
// a mount point) on an os.Lstat or Root.Lstat result.
func isReparsePoint(fi os.FileInfo) bool {
	d, ok := fi.Sys().(*syscall.Win32FileAttributeData)
	return ok && d != nil && d.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0
}

// statLogFileName observes rel through root once. Root.Lstat's FileInfo
// carries no link count on Windows, so rel is opened again through the same
// root (the same component-wise, link-checked walk as the open itself) and ONE
// GetFileInformationByHandle on that handle yields the attributes, the
// identity (volume serial and file index, compared with the descriptor's,
// which cannot change) and NumberOfLinks.
func statLogFileName(root *os.Root, f *os.File, rel string) (nameBinding, error) {
	var opened windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &opened); err != nil {
		return nameBinding{}, err
	}
	nf, err := root.OpenFile(rel, os.O_RDONLY, 0)
	if err != nil {
		return nameBinding{}, err
	}
	defer func() { _ = nf.Close() }()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(nf.Fd()), &info); err != nil {
		return nameBinding{}, err
	}
	return nameBinding{
		same: info.VolumeSerialNumber == opened.VolumeSerialNumber &&
			info.FileIndexHigh == opened.FileIndexHigh && info.FileIndexLow == opened.FileIndexLow,
		regular: info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0,
		nlink:   uint64(info.NumberOfLinks),
	}, nil
}

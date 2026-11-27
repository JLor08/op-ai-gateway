// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

// logFileShareMode lets the server write, rename and delete the file while the
// agent holds it open for tailing (§5.2 step 4).
const logFileShareMode = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE | windows.FILE_SHARE_DELETE

// The GetFinalPathNameByHandle flags (fileapi.h), which x/sys/windows does not
// define: a normalized name with its drive letter. Both are 0, the default.
const (
	fileNameNormalized = 0x0 // FILE_NAME_NORMALIZED
	volumeNameDOS      = 0x0 // VOLUME_NAME_DOS
)

// openLogFileHandle opens target.OpenPath read-only with full sharing for
// openLogFile and returns the file with its final path. CreateFile without
// FILE_FLAG_BACKUP_SEMANTICS cannot open a directory; GetFileType refuses
// every handle that is not a disk file (a pipe, a console, a reserved device
// name such as NUL or CON) before anything reads from it. The final path is
// the handle's own (GetFinalPathNameByHandle), so it reflects every symlink
// and junction actually followed and cannot be raced by a later swap.
func openLogFileHandle(target logFileTarget) (*os.File, string, error) {
	p16, err := windows.UTF16PtrFromString(target.OpenPath)
	if err != nil {
		return nil, "", logOpenRefusal(target, "", "is not a valid Windows path")
	}
	h, err := windows.CreateFile(p16, windows.GENERIC_READ, logFileShareMode, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, "", openErr(err)
	}
	if t, err := windows.GetFileType(h); err != nil || t != windows.FILE_TYPE_DISK {
		_ = windows.CloseHandle(h)
		return nil, "", logOpenRefusal(target, "", "is not a regular file")
	}
	final, err := finalPathByHandle(h)
	if err != nil {
		_ = windows.CloseHandle(h)
		return nil, "", logOpenRefusal(target, "", "has a final path that cannot be determined")
	}
	return os.NewFile(uintptr(h), target.OpenPath), final, nil
}

// resolveLogRoot resolves an allowed root through the same API as the opened
// file (a directory handle and GetFinalPathNameByHandle), so a root that is a
// symlink or junction, or spelled with another case, compares against the
// file's final path in the same normalized form. filepath.EvalSymlinks would
// not do: it no longer follows mount points (junctions) on Windows.
func resolveLogRoot(root string) (string, error) {
	p16, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p16, windows.FILE_READ_ATTRIBUTES, logFileShareMode, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = windows.CloseHandle(h) }()
	return finalPathByHandle(h)
}

// finalPathByHandle is the normalized DOS path of h, with the \\?\ prefix
// GetFinalPathNameByHandle adds stripped (\\?\UNC\srv\share becomes
// \\srv\share, which openLogFile then refuses as a UNC path).
func finalPathByHandle(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), fileNameNormalized|volumeNameDOS)
		if err != nil {
			return "", err
		}
		if n < uint32(len(buf)) {
			return stripExtendedPathPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n)
	}
}

func stripExtendedPathPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	if rest, ok := strings.CutPrefix(p, `\\?\`); ok {
		return rest
	}
	return p
}

// fileIdentity would be (volume serial, file index) on Windows, but an
// os.FileInfo does not expose them: its Sys() is a
// *syscall.Win32FileAttributeData, and os keeps the volume and index private.
// ok is therefore always false here; a caller detecting rotation uses
// os.SameFile, which compares exactly those fields on Windows.
func fileIdentity(os.FileInfo) (ino, dev uint64, ok bool) {
	return 0, 0, false
}

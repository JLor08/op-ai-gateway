// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Package wslentry is the grammar of a WSL allowlist entry,
// wsl://<distro>/<absolute Linux path>, shared by every package that reads the
// agent's runtime_allowed_binaries / runtime_allowed_dirs: the config startup
// warnings, the runtime policy and the binary inventory. It is a stdlib-only
// leaf package -- it imports nothing else from this module -- so config can
// use the grammar without importing the runtime.
package wslentry

import (
	"regexp"
	"strings"
)

// Scheme prefixes an allowlist entry that names a binary (or a directory)
// inside a WSL2 distribution: wsl://<distro>/<absolute Linux path>, e.g.
// wsl://Ubuntu-24.04/home/ai/vllm-env/bin/vllm. The scheme is exactly
// lowercase; WSL:// or wsl:/ is not a WSL entry.
const Scheme = "wsl://"

// DistroPattern is what a WSL distribution name may look like: ASCII letters,
// digits, '.', '_' and '-', 1 to 64 characters, the first a letter or a digit.
// WSL's own names (Ubuntu-24.04, Debian, docker-desktop) fit, and nothing that
// could be read as an option (a leading '-' after wsl.exe -d), a path or a
// shell word does. The gateway's runtimeSpecWSLDistroPattern is the same rule.
var DistroPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidDistro reports whether name is an acceptable WSL distribution name
// (DistroPattern).
func ValidDistro(name string) bool {
	return DistroPattern.MatchString(name)
}

// Is reports whether entry carries the wsl:// scheme, well-formed or not. A
// native allowlist ignores every such entry, so a malformed one can never be
// misread as a native path.
func Is(entry string) bool {
	return strings.HasPrefix(entry, Scheme)
}

// Parse splits a well-formed wsl:// entry into its distro and Linux path. The
// distro is everything up to the first "/" after the scheme and must match
// DistroPattern; the Linux path is the rest INCLUDING that "/", with no NUL and
// no backslash, and is returned verbatim (no cleaning), exactly as a native
// entry is compared. ok is false for anything else.
func Parse(entry string) (distro, linuxPath string, ok bool) {
	if !Is(entry) {
		return "", "", false
	}
	rest := entry[len(Scheme):]
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return "", "", false
	}
	distro, linuxPath = rest[:i], rest[i:]
	if !ValidDistro(distro) || strings.ContainsAny(linuxPath, "\x00\\") {
		return "", "", false
	}
	return distro, linuxPath, true
}

// LooksLike reports whether entry is a failed attempt at a WSL entry: its
// lowercased form starts with "wsl:" but Parse rejects it. That covers a
// malformed wsl:// entry, a wrong-case WSL:// one, and a wsl:/ or wsl:x typo. It
// is the one look-alike rule shared by the fail-closed work_dir containment
// (the runtime policy) and the startup warnings in the config package.
func LooksLike(entry string) bool {
	if len(entry) < 4 || !strings.EqualFold(entry[:4], "wsl:") {
		return false
	}
	_, _, ok := Parse(entry)
	return !ok
}

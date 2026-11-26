// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build unix

package hostpower

import "syscall"

func mkfifo(path string) error { return syscall.Mkfifo(path, 0o644) }

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// killSelf SIGKILLs this process: the kernel's OOM-kill signature
// (-crash-signal).
func killSelf() {
	_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
}

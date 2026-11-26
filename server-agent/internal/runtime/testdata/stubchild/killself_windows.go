// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package main

import "os"

// killSelf terminates this process the hard way. Windows has no SIGKILL; the
// -crash-signal classification it stands in for does not exist there either
// (killedBySIGKILL is always false), so this only has to end the process
// without running any exit path of its own.
func killSelf() {
	p, err := os.FindProcess(os.Getpid())
	if err == nil {
		_ = p.Kill()
	}
	os.Exit(1)
}

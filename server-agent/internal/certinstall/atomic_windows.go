// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package certinstall

import (
	"context"
	"os/exec"
	"syscall"
)

// buildHookCommand runs command on Windows via a raw CmdLine handed straight
// to CreateProcess, bypassing os/exec's own argument quoting -- which would
// otherwise mangle a composed command line (pipes, redirects, quoted paths
// with spaces). This is the same technique the os/exec package itself
// documents for exactly this problem (see syscall.SysProcAttr.CmdLine): when
// CmdLine is non-empty it REPLACES the command line os/exec would otherwise
// build from Args, so command is passed through verbatim.
func buildHookCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "/C " + command}
	return cmd
}

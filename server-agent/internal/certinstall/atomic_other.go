// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package certinstall

import (
	"context"
	"os/exec"
)

// buildHookCommand runs command through a shell on unix-like systems, exactly
// as an operator would type it at a prompt (pipes, redirects, and quoting all
// work), matching the documented "sh -c <cmd>" contract.
func buildHookCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}

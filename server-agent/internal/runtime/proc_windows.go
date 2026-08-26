// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import "os/exec"

// setProcGroup is a no-op on Windows: syscall.SysProcAttr has no Setpgid
// field there, and (see terminateGroup below) this platform has no
// graceful group-signal equivalent for an arbitrary child process anyway.
//
// KNOWN LIMITATION: the agent-managed model runtime is kill-only on
// Windows, not graceful-drain-then-kill -- terminateGroup and killGroup are
// therefore identical here. Windows is a real build target for
// server-agent (gateway/deploy/build-agents.sh cross-compiles it), so this
// file exists to keep `go build` working for that target, but CI
// (.github/workflows/ci.yml) only runs ubuntu-latest, so this platform's
// process-management path is never exercised by tests. A future task
// should add a real graceful-shutdown path (e.g. CREATE_NEW_PROCESS_GROUP +
// GenerateConsoleCtrlEvent) if Windows operators need clean model-server
// shutdown rather than a hard kill.
func setProcGroup(cmd *exec.Cmd) {}

// terminateGroup has no graceful equivalent on this platform (see the
// package doc above), so it goes straight to killGroup.
func terminateGroup(cmd *exec.Cmd) error {
	return killGroup(cmd)
}

// killGroup forcibly terminates the child process. This does not reach
// grandchildren a model server may have spawned (no process-group
// isolation on this platform, per setProcGroup above).
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

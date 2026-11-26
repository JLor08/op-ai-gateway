// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import "os/exec"

// startNativeProc starts a native spec's process and returns the control that
// signals it. Off Windows, process_tree "job" (#424) changes nothing: the
// process group setProcGroup already made is what holds the tree here, so a job
// spec starts and stops exactly as a direct one.
func startNativeProc(cmd *exec.Cmd, spec Spec) (procControl, error) {
	_ = spec
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return nativeControl{cmd: cmd}, nil
}

// StartKillableTree starts cmd as the leader of its own process group and
// returns a func that SIGKILLs the whole group, so a wrapper's grandchildren
// die with it. For a short-lived probe of a binary (the agent's rpc-server
// protocol probe) that must not leave anything behind; the kill is safe to
// call more than once.
func StartKillableTree(cmd *exec.Cmd) (kill func(), err error) {
	setProcGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return func() { _ = killGroup(cmd) }, nil
}

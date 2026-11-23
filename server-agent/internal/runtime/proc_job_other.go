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

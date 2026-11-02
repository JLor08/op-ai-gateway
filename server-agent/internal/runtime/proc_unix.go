// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"errors"
	"os/exec"
	"syscall"
)

// killedBySIGKILL reports whether err -- a cmd.Wait() error -- describes a
// process the kernel terminated with SIGKILL. That is the Linux OOM killer's
// signature (and a `kill -9`), and it is the memory-pressure signal the
// cold-start RAM bootstrap classifier reads (task 4.4, see
// bootstrapDiedOfMemoryPressure). It is deliberately narrow -- SIGKILL only, not
// any signal -- because a SIGTERM is a graceful stop, and the agent's OWN
// SIGKILL escalation never reaches the classifier (it rides the intentional-stop
// branch of onProcExited). A nil error (clean exit), a non-exec error, or a
// plain non-zero exit code all return false: the process was not signal-killed.
func killedBySIGKILL(err error) bool {
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	ws, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	return ws.Signaled() && ws.Signal() == syscall.SIGKILL
}

// setProcGroup makes cmd (once started) the leader of its own process
// group, so a single group-signal (see signalGroup) reaches the whole
// tree a model server may spawn (helper processes, workers) rather than
// only the immediate child.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateGroup asks cmd's process group to exit gracefully (SIGTERM).
func terminateGroup(cmd *exec.Cmd) error {
	return signalGroup(cmd, syscall.SIGTERM)
}

// killGroup forces cmd's process group to exit immediately (SIGKILL), used
// after killGrace has elapsed since terminateGroup, or directly when a
// child never became healthy and there is nothing to gracefully drain.
func killGroup(cmd *exec.Cmd) error {
	return signalGroup(cmd, syscall.SIGKILL)
}

// signalGroup signals the negative PID (the process-group convention --
// see setProcGroup's Setpgid, which makes the child's PID its own group
// leader's PGID). ESRCH (the group is already gone) is not an error worth
// surfacing: the manager's own goal -- the process being gone -- is
// already achieved.
func signalGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if err != nil && errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

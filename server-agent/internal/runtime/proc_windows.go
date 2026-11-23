// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import "os/exec"

// killedBySIGKILL is always false on Windows: there is no POSIX signal delivery
// and no OOM-killer-sends-SIGKILL convention, so the cold-start RAM bootstrap
// classifier (task 4.4) has no kill-signal signature to read here. A Windows
// host therefore classifies a bootstrap death as memory pressure ONLY through
// the RSS-near-budget signature (bootstrapDiedOfMemoryPressure), which needs no
// signal. The parameter is unused, kept so the two builds share one signature.
func killedBySIGKILL(err error) bool {
	_ = err
	return false
}

// setProcGroup is a no-op on Windows: syscall.SysProcAttr has no Setpgid
// field there. How a native child's tree is held is the spec's process_tree
// (#424), chosen in startNativeProc:
//
//   - direct (the default) is kill-only: terminateGroup and killGroup below are
//     identical, a stop is TerminateProcess on the child alone, and a
//     grandchild the child spawned is not reached. There is no graceful drain
//     on this path.
//   - job is opt-in (proc_job_windows.go): the child starts suspended in its
//     own process group, is assigned to a kill-on-close job object and only
//     then resumed; terminate delivers CTRL_BREAK_EVENT to its group and kill,
//     after the ordinary kill grace, is TerminateJobObject on the whole tree.
//     A failed Ctrl-Break falls through to that kill at once.
//
// A wsl spec (#381) is neither: its workload is stopped gracefully from inside
// the distro, through its systemd scope (wslControl), and the kill below is
// only that path's backstop on wsl.exe; process_tree is inert there.
//
// CI (.github/workflows/ci.yml) runs ubuntu-latest only, so none of this
// platform's process control is exercised there; the job mode's tests
// (proc_job_windows_test.go) run on a Windows host.
func setProcGroup(cmd *exec.Cmd) {}

// terminateGroup is the direct mode's stop: there is no graceful group signal
// for an arbitrary child without the job mode's own process group and console
// (see setProcGroup above), so it goes straight to killGroup.
func terminateGroup(cmd *exec.Cmd) error {
	return killGroup(cmd)
}

// killGroup forcibly terminates the child process. In direct mode this does
// not reach grandchildren a model server may have spawned; process_tree "job"
// is the mode that does (setProcGroup above).
func killGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

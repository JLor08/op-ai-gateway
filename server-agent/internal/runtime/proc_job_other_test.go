// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build !windows

package runtime

import (
	"os/exec"
	"testing"
	"time"
)

// TestStartNativeProcJobIsTheProcessGroupOffWindows pins the controller ruling
// that process_tree "job" is a Windows mechanism (#424): everywhere else a job
// spec starts exactly like a direct one -- its own process group, signalled
// through nativeControl -- so SIGTERM still reaches the tree.
func TestStartNativeProcJobIsTheProcessGroupOffWindows(t *testing.T) {
	for _, tree := range []string{"", ProcessTreeDirect, ProcessTreeJob} {
		t.Run("tree="+tree, func(t *testing.T) {
			cmd := exec.Command("sleep", "30")
			setProcGroup(cmd)
			ctl, err := startNativeProc(cmd, Spec{ID: "s", ProcessTree: tree})
			if err != nil {
				t.Fatalf("startNativeProc: %v", err)
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			if _, ok := ctl.(nativeControl); !ok {
				t.Fatalf("control = %T, want nativeControl", ctl)
			}
			if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
				t.Fatal("the child must lead its own process group, as a direct spec's does")
			}
			if err := ctl.terminate(); err != nil {
				t.Fatalf("terminate: %v", err)
			}
			select {
			case <-done:
			case <-time.After(killGrace):
				_ = ctl.kill()
				t.Fatal("SIGTERM to the group did not end the child within killGrace")
			}
		})
	}
}

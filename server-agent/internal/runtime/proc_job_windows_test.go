// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These tests exercise the process_tree "job" control (#424) against real
// processes and so run on a Windows host only. TestMain builds no helper on
// Windows (the manager suite self-skips there), so they build stubchild
// themselves.

// consolelessHelperEnv, when set, makes TestJobConsolelessHelper run: the
// parent test re-runs this test binary as a DETACHED_PROCESS -- no console, as
// the agent has none when it runs as a service -- with the stubchild path in
// this variable.
const consolelessHelperEnv = "RUNTIME_JOB_CONSOLELESS_HELPER_STUBCHILD"

var (
	winStubOnce sync.Once
	winStubPath string
	winStubErr  error
)

// windowsStubchild builds testdata/stubchild once per test binary.
func windowsStubchild(t *testing.T) string {
	t.Helper()
	if p := os.Getenv(consolelessHelperEnv); p != "" {
		return p
	}
	winStubOnce.Do(func() {
		dir, err := os.MkdirTemp("", "runtime-job-stubchild-*")
		if err != nil {
			winStubErr = err
			return
		}
		winStubPath = filepath.Join(dir, "stubchild.exe")
		out, err := exec.Command("go", "build", "-o", winStubPath, "./testdata/stubchild").CombinedOutput()
		if err != nil {
			winStubErr = errors.New(err.Error() + ": " + string(out))
		}
	})
	if winStubErr != nil {
		t.Fatalf("build stubchild: %v", winStubErr)
	}
	return winStubPath
}

// lockedBuffer is a concurrency-safe log writer standing in for procLog.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// jobRun is one stubchild started in job mode, reaped the way waitForExit
// reaps a generation: Wait, then close.
type jobRun struct {
	ctl  *jobControl
	cmd  *exec.Cmd
	out  *lockedBuffer
	done chan error
}

func startStubInJob(t *testing.T, args ...string) *jobRun {
	t.Helper()
	cmd := exec.Command(windowsStubchild(t), append([]string{"-port", "0"}, args...)...)
	out := &lockedBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	ctl, err := startInJob(cmd)
	if err != nil {
		t.Fatalf("startInJob: %v", err)
	}
	r := &jobRun{ctl: ctl, cmd: cmd, out: out, done: make(chan error, 1)}
	go func() {
		err := cmd.Wait()
		_ = ctl.close()
		r.done <- err
	}()
	t.Cleanup(func() {
		_ = ctl.kill()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Errorf("stubchild %d was not reaped after the cleanup kill", cmd.Process.Pid)
		}
	})
	return r
}

var grandchildPIDLine = regexp.MustCompile(`stubchild: grandchild pid=(\d+)`)

// grandchildPID waits for the child to report the PID of its -spawn-grandchild.
func (r *jobRun) grandchildPID(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := grandchildPIDLine.FindStringSubmatch(r.out.String()); m != nil {
			pid, _ := strconv.Atoi(m[1])
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the child never reported its grandchild; output: %q", r.out.String())
	return 0
}

// waitExited reports whether the child was reaped within d.
func (r *jobRun) waitExited(d time.Duration) bool {
	select {
	case <-r.done:
		return true
	case <-time.After(d):
		return false
	}
}

// processGoneWithin reports whether process pid has exited (or no longer
// exists) within d.
func processGoneWithin(pid int, d time.Duration) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // no such process any more
	}
	defer windows.CloseHandle(h) //nolint:errcheck // test helper
	ev, err := windows.WaitForSingleObject(h, uint32(d/time.Millisecond))
	return err == nil && ev == windows.WAIT_OBJECT_0
}

// TestJobPidsHoldChildAndGrandchild: the child is in the job before it runs a
// single instruction, so the grandchild it spawns is in the job too.
func TestJobPidsHoldChildAndGrandchild(t *testing.T) {
	r := startStubInJob(t, "-spawn-grandchild")
	gc := r.grandchildPID(t)
	pids := r.ctl.pids()
	if !slices.Contains(pids, r.cmd.Process.Pid) || !slices.Contains(pids, gc) {
		t.Fatalf("pids() = %v, want both the child %d and the grandchild %d", pids, r.cmd.Process.Pid, gc)
	}
}

// TestJobTerminateEndsTreeViaCtrlBreak: terminate is a real Ctrl-Break to the
// child's group, which a cooperative child and its grandchild die of within
// the kill grace -- without the kill.
func TestJobTerminateEndsTreeViaCtrlBreak(t *testing.T) {
	r := startStubInJob(t, "-spawn-grandchild")
	gc := r.grandchildPID(t)
	// terminate falls through to the kill on a failed delivery, which would
	// end the tree too; record the delivery's own result to tell them apart.
	deliver := r.ctl.ctrlBreak
	var deliverErr error
	r.ctl.ctrlBreak = func(pid int) error { deliverErr = deliver(pid); return deliverErr }
	if err := r.ctl.terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if deliverErr != nil {
		t.Fatalf("Ctrl-Break delivery failed (agent console: %v): %v", agentHasConsole(), deliverErr)
	}
	if !r.waitExited(killGrace) {
		t.Fatalf("the child outlived killGrace after Ctrl-Break (agent console: %v)", agentHasConsole())
	}
	if !processGoneWithin(gc, killGrace) {
		t.Fatalf("grandchild %d outlived the child's Ctrl-Break stop", gc)
	}
}

// TestJobTerminateFallsThroughToKillWhenCtrlBreakFails: a failed delivery is
// not a stop that never happens -- terminate kills the whole job at once.
func TestJobTerminateFallsThroughToKillWhenCtrlBreakFails(t *testing.T) {
	r := startStubInJob(t, "-spawn-grandchild", "-ignore-ctrlbreak")
	gc := r.grandchildPID(t)
	r.ctl.ctrlBreak = func(int) error { return errors.New("injected delivery failure") }
	if err := r.ctl.terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if !r.waitExited(killGrace) {
		t.Fatal("a failed Ctrl-Break must fall through to the job kill, but the child is still running")
	}
	if !processGoneWithin(gc, killGrace) {
		t.Fatalf("grandchild %d survived the fall-through kill", gc)
	}
}

// TestJobTerminateBoundsAHangingCtrlBreak: a delivery that hangs costs the
// owner at most ctrlBreakTimeout and then reads as failed.
func TestJobTerminateBoundsAHangingCtrlBreak(t *testing.T) {
	r := startStubInJob(t, "-ignore-ctrlbreak")
	release := make(chan struct{})
	defer close(release)
	r.ctl.ctrlBreak = func(int) error { <-release; return nil }
	start := time.Now()
	if err := r.ctl.terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if took := time.Since(start); took > ctrlBreakTimeout+500*time.Millisecond {
		t.Fatalf("terminate blocked %v, want at most about %v", took, ctrlBreakTimeout)
	}
	if !r.waitExited(killGrace) {
		t.Fatal("a hanging Ctrl-Break must fall through to the job kill")
	}
}

// TestJobKillEndsATreeThatIgnoresCtrlBreak: the escalation after the kill
// grace reaches a tree that survives Ctrl-Break.
func TestJobKillEndsATreeThatIgnoresCtrlBreak(t *testing.T) {
	r := startStubInJob(t, "-spawn-grandchild", "-ignore-ctrlbreak")
	gc := r.grandchildPID(t)
	if err := r.ctl.terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if r.waitExited(time.Second) {
		t.Fatal("a child that ignores Ctrl-Break must still be running after terminate")
	}
	if err := r.ctl.kill(); err != nil {
		t.Fatalf("kill: %v", err)
	}
	if !r.waitExited(killGrace) {
		t.Fatal("kill must end the child")
	}
	if !processGoneWithin(gc, killGrace) {
		t.Fatalf("grandchild %d survived the job kill", gc)
	}
}

// TestJobChildExitingAloneTakesTheGrandchildWithIt: when the root exits by
// itself, the job is closed and kill-on-close ends the leftover descendant --
// which also releases the stdout pipe it inherited, so the reap completes.
func TestJobChildExitingAloneTakesTheGrandchildWithIt(t *testing.T) {
	r := startStubInJob(t, "-spawn-grandchild", "-ignore-ctrlbreak", "-crash-after", "1s")
	gc := r.grandchildPID(t)
	if !r.waitExited(15 * time.Second) {
		t.Fatal("the child's own exit was never reaped (a grandchild holding the stdout pipe keeps cmd.Wait open)")
	}
	if err := r.ctl.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !processGoneWithin(gc, killGrace) {
		t.Fatalf("grandchild %d outlived its job", gc)
	}
	if got := r.ctl.pids(); got != nil {
		t.Fatalf("pids() after close = %v, want nil", got)
	}
	if err := r.ctl.kill(); err != nil {
		t.Fatalf("kill after close must be a no-op, got %v", err)
	}
}

// TestJobStdoutReachesTheLogWriter: job mode (suspended start, own console
// when the agent has none) keeps stdio redirected to the log writer.
func TestJobStdoutReachesTheLogWriter(t *testing.T) {
	r := startStubInJob(t, "-print", `hello from the job child\n`)
	deadline := time.Now().Add(10 * time.Second)
	for !bytes.Contains([]byte(r.out.String()), []byte("hello from the job child")) {
		if time.Now().After(deadline) {
			t.Fatalf("the child's stdout never reached the log writer; got %q", r.out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStartNativeProcHonoursJobOnlyForJobSpecs: direct keeps today's
// kill-only control, job gets the job control.
func TestStartNativeProcHonoursJobOnlyForJobSpecs(t *testing.T) {
	for tree, want := range map[string]string{"": "nativeControl", ProcessTreeDirect: "nativeControl", ProcessTreeJob: "jobControl"} {
		cmd := exec.Command(windowsStubchild(t), "-port", "0")
		ctl, err := startNativeProc(cmd, Spec{ID: "s", ProcessTree: tree})
		if err != nil {
			t.Fatalf("tree %q: startNativeProc: %v", tree, err)
		}
		got := "nativeControl"
		if _, ok := ctl.(*jobControl); ok {
			got = "jobControl"
		}
		_ = ctl.kill()
		_ = cmd.Wait()
		if c, ok := ctl.(procCloser); ok {
			_ = c.close()
		}
		if got != want {
			t.Errorf("tree %q: control = %s, want %s", tree, got, want)
		}
	}
}

// TestJobCtrlBreakFromAConsolelessAgent runs the Ctrl-Break stop the way a
// service agent does it: with no console of its own, so the child gets a
// hidden console and delivery goes through AttachConsole. It re-runs this
// test binary as a DETACHED_PROCESS and checks that the helper passed.
func TestJobCtrlBreakFromAConsolelessAgent(t *testing.T) {
	if os.Getenv(consolelessHelperEnv) != "" {
		t.Skip("running as the consoleless helper")
	}
	stub := windowsStubchild(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestJobConsolelessHelper$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), consolelessHelperEnv+"="+stub)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("consoleless helper failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("--- PASS: TestJobConsolelessHelper")) {
		t.Fatalf("consoleless helper did not pass:\n%s", out)
	}
}

// TestJobConsolelessHelper is TestJobCtrlBreakFromAConsolelessAgent's
// detached half; it skips unless that test started it.
func TestJobConsolelessHelper(t *testing.T) {
	if os.Getenv(consolelessHelperEnv) == "" {
		t.Skip("only runs under TestJobCtrlBreakFromAConsolelessAgent")
	}
	if agentHasConsole() {
		t.Fatal("the helper was meant to run without a console")
	}
	r := startStubInJob(t, "-spawn-grandchild")
	gc := r.grandchildPID(t)
	deliver := r.ctl.ctrlBreak
	var deliverErr error
	r.ctl.ctrlBreak = func(pid int) error { deliverErr = deliver(pid); return deliverErr }
	if err := r.ctl.terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	if deliverErr != nil {
		t.Fatalf("Ctrl-Break delivery through the child's console failed: %v", deliverErr)
	}
	if !r.waitExited(killGrace) {
		t.Fatal("the child outlived killGrace after a Ctrl-Break through its own console")
	}
	if !processGoneWithin(gc, killGrace) {
		t.Fatalf("grandchild %d outlived the child's Ctrl-Break stop", gc)
	}
	if agentHasConsole() {
		t.Fatal("the agent must be detached from the child's console again after delivery")
	}
}

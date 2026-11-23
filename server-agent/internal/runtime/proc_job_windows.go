// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

//go:build windows

package runtime

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ctrlBreakTimeout bounds how long terminate waits, on the owner goroutine, for
// a Ctrl-Break delivery (callWithin). A delivery that takes longer reads as a
// failed one and the job is killed at once.
const ctrlBreakTimeout = time.Second

// The console calls golang.org/x/sys/windows does not wrap. All three are
// documented kernel32 exports present on every Windows the agent supports.
var (
	modKernel32                = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole          = modKernel32.NewProc("AttachConsole")
	procFreeConsole            = modKernel32.NewProc("FreeConsole")
	procSetConsoleCtrlHandler  = modKernel32.NewProc("SetConsoleCtrlHandler")
	procGetConsoleProcessList  = modKernel32.NewProc("GetConsoleProcessList")
	errNoThreadOfSuspendedProc = errors.New("no thread of the suspended child was found")
)

// consoleMu serialises the agent's console attachment: a process has at most
// one console, so two Ctrl-Break deliveries through AttachConsole must not
// interleave.
var consoleMu sync.Mutex

// startNativeProc starts a native spec's process and returns the control that
// signals it. A process_tree "job" spec (#424) runs in a kill-on-close job
// object (startInJob); every other native spec starts exactly as before --
// kill-only, see proc_windows.go.
func startNativeProc(cmd *exec.Cmd, spec Spec) (procControl, error) {
	if spec.ProcessTreeMode() != ProcessTreeJob {
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		return nativeControl{cmd: cmd}, nil
	}
	c, err := startInJob(cmd)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// jobControl holds a process_tree "job" generation (#424): the child runs in
// its own process group inside a job object the agent created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so every process it spawns is in the job
// too (nested jobs, Windows 8+, keep a job the child creates for itself inside
// ours).
//
// terminate delivers CTRL_BREAK_EVENT to the child's process group -- the
// Windows counterpart of SIGTERM to the unix process group -- and falls
// through to kill when that cannot be delivered. kill is TerminateJobObject:
// the whole tree, at once. close gives the job handle back, which kills
// whatever is still in it; it runs as soon as the root process exits (the
// exit watcher startInJob starts) and again, harmlessly, when waitForExit has
// reaped it. Closing at the root's exit rather than at the reap matters: a
// descendant that inherited the child's stdout keeps the pipe open, and
// cmd.Wait does not return until the pipe closes.
//
// mu guards job: once close has run, job is 0 and kill, close and pids do
// nothing, so no call can reach a handle value Windows has since handed out
// again.
type jobControl struct {
	cmd *exec.Cmd
	pid int

	mu  sync.Mutex
	job windows.Handle

	// ctrlBreak delivers CTRL_BREAK_EVENT to the process group pid leads. A
	// field so a test can make delivery fail or hang.
	ctrlBreak func(pid int) error
}

// startProcess and waitForExit find a job generation's close and pids through
// these interfaces; a signature drift would silently skip both.
var (
	_ procCloser     = (*jobControl)(nil)
	_ procTreeLister = (*jobControl)(nil)
)

// startInJob starts cmd suspended, in a new process group and -- when the
// agent has no console of its own (a service) -- in a hidden console of its
// own, so Ctrl-Break has a console to travel through; stdio stays redirected
// to whatever cmd.Stdout/Stderr are, exactly as for a direct spec. It assigns
// the child to a new kill-on-close job and only then resumes it, so no
// grandchild can be spawned outside the job.
//
// On any failure after the process exists, startInJob kills and reaps it
// itself and returns the error: the caller treats it as a failed exec.
func startInJob(cmd *exec.Cmd) (*jobControl, error) {
	ownConsole := !agentHasConsole()
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED | windows.CREATE_NEW_PROCESS_GROUP
	if ownConsole {
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_CONSOLE
		cmd.SysProcAttr.HideWindow = true
	}

	job, err := newKillOnCloseJob()
	if err != nil {
		return nil, fmt.Errorf("runtime: process_tree job: create the job object: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = windows.CloseHandle(job)
		return nil, err
	}
	pid := cmd.Process.Pid
	fail := func(step string, err error) (*jobControl, error) {
		// TerminateProcess works on a suspended process; the reap releases
		// the pipes os/exec opened, since no caller will Wait on this cmd.
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("runtime: process_tree job: %s: %w", step, err)
	}

	// The handle the exit watcher waits on, opened while the child is
	// suspended (cmd.Process keeps the PID from being reused meanwhile).
	waitHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return fail("open the child for its exit watch", err)
	}
	if err := assignToJob(job, pid); err != nil {
		_ = windows.CloseHandle(waitHandle)
		return fail("assign the child to its job", err)
	}
	if err := resumeProcess(pid); err != nil {
		_ = windows.CloseHandle(waitHandle)
		return fail("resume the child", err)
	}

	c := &jobControl{cmd: cmd, pid: pid, job: job, ctrlBreak: ctrlBreakSharedConsole}
	if ownConsole {
		c.ctrlBreak = ctrlBreakViaAttach
	}
	go c.closeWhenExited(waitHandle)
	return c, nil
}

// closeWhenExited waits for the root process to exit, then closes the job,
// which kills every descendant still in it. It ends when the process does,
// which kill guarantees.
func (c *jobControl) closeWhenExited(h windows.Handle) {
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about a failed close of a wait handle
	if _, err := windows.WaitForSingleObject(h, windows.INFINITE); err != nil {
		slog.Warn("runtime: waiting for a job root process failed; its job closes at the reap", "pid", c.pid, "error", err)
		return
	}
	if err := c.close(); err != nil {
		slog.Debug("runtime: closing a job object failed", "pid", c.pid, "error", err)
	}
}

// terminate asks the child's process group to stop (CTRL_BREAK_EVENT). A
// delivery that fails, or does not return within ctrlBreakTimeout, is logged
// and falls through to kill at once: the stop goes on, only less gracefully.
func (c *jobControl) terminate() error {
	err := callWithin(ctrlBreakTimeout, func() error { return c.ctrlBreak(c.pid) })
	if err == nil {
		return nil
	}
	slog.Info("runtime: Ctrl-Break could not be delivered; killing the job instead", "pid", c.pid, "error", err)
	return c.kill()
}

// kill ends every process in the job (TerminateJobObject, exit code 1). After
// close it does nothing: the job, and so every process in it, is gone.
func (c *jobControl) kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	return windows.TerminateJobObject(c.job, 1)
}

// close gives the job handle back. It is the last handle to the job, so
// kill-on-close ends any process still in it. Safe to call more than once.
func (c *jobControl) close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	err := windows.CloseHandle(c.job)
	c.job = 0
	return err
}

// jobPIDListCap is how many process IDs pids reads from the job at most. A
// model server's tree is a handful of processes; a job holding more reports
// the first jobPIDListCap.
const jobPIDListCap = 1024

// jobProcessIDList is JOBOBJECT_BASIC_PROCESS_ID_LIST with a fixed-size array:
// two DWORDs, then ULONG_PTRs, which Go lays out exactly as C does on both
// 386 and amd64 (the array starts at offset 8).
type jobProcessIDList struct {
	assigned uint32
	inList   uint32
	ids      [jobPIDListCap]uintptr
}

// pids lists the processes currently in the job: the root and every
// descendant still alive. nil once the job is closed or when the query fails.
func (c *jobControl) pids() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.job == 0 {
		return nil
	}
	var list jobProcessIDList
	err := windows.QueryInformationJobObject(c.job, windows.JobObjectBasicProcessIdList,
		uintptr(unsafe.Pointer(&list)), uint32(unsafe.Sizeof(list)), nil)
	if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
		return nil
	}
	n := min(int(list.inList), jobPIDListCap)
	out := make([]int, 0, n)
	for _, id := range list.ids[:n] {
		out = append(out, int(id))
	}
	return out
}

// newKillOnCloseJob creates an unnamed job object whose processes all die
// when its last handle is closed -- by close, or by the agent's own exit, so a
// job child never outlives the agent that holds it.
func newKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

// assignToJob puts process pid into job.
func assignToJob(job windows.Handle, pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h) //nolint:errcheck // nothing to do about a failed close
	return windows.AssignProcessToJobObject(job, h)
}

// resumeProcess resumes the threads of a process created with
// CREATE_SUSPENDED. os.StartProcess closes the primary thread's handle before
// it returns, so the thread is found again through a ToolHelp thread snapshot
// (documented, unlike ntdll's NtResumeProcess). A suspended new process has
// exactly its primary thread; should anything have injected another one,
// ResumeThread on a running thread changes nothing, so resuming every thread
// of the process is safe.
func resumeProcess(pid int) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap) //nolint:errcheck // nothing to do about a failed close
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &entry); err == nil; err = windows.Thread32Next(snap, &entry) {
		if entry.OwnerProcessID != uint32(pid) {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if oerr != nil {
			return fmt.Errorf("open thread %d: %w", entry.ThreadID, oerr)
		}
		_, rerr := windows.ResumeThread(th)
		_ = windows.CloseHandle(th)
		if rerr != nil {
			return fmt.Errorf("resume thread %d: %w", entry.ThreadID, rerr)
		}
		resumed++
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return err
	}
	if resumed == 0 {
		return errNoThreadOfSuspendedProc
	}
	return nil
}

// agentHasConsole reports whether the agent is attached to a console: run
// from a terminal it is (and a job child shares that console); as a service
// it is not, and the child then gets a hidden console of its own.
// GetConsoleProcessList returns 0 without a console.
func agentHasConsole() bool {
	var ids [1]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 1)
	return n != 0
}

// ctrlBreakSharedConsole delivers CTRL_BREAK_EVENT to the process group pid
// leads, when the agent and the child share the agent's console. The agent is
// not in that group, so the event reaches only the child's tree.
func ctrlBreakSharedConsole(pid int) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
}

// ctrlBreakViaAttach delivers CTRL_BREAK_EVENT to the process group pid leads
// when the child has a console of its own (the agent has none): the agent
// attaches to the child's console for the duration of the call -- a console
// control event can only be generated on the caller's own console -- and
// detaches again. Ctrl-C handling is switched off around it as a guard; the
// event itself targets only the child's group, which the agent is not in.
// Serialised by consoleMu.
func ctrlBreakViaAttach(pid int) error {
	consoleMu.Lock()
	defer consoleMu.Unlock()
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("attach to the child's console: %w", err)
	}
	guarded := false
	if r, _, _ := procSetConsoleCtrlHandler.Call(0, 1); r != 0 {
		guarded = true
	}
	sendErr := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(pid))
	if r, _, err := procFreeConsole.Call(); r == 0 {
		// The event is on its way regardless; a later delivery's
		// AttachConsole fails and falls through to the kill.
		slog.Warn("runtime: detaching from a job child's console failed", "pid", pid, "error", err)
	}
	if guarded {
		procSetConsoleCtrlHandler.Call(0, 0) //nolint:errcheck // restoring the default; nothing to do on failure
	}
	if sendErr != nil {
		return fmt.Errorf("generate the Ctrl-Break event: %w", sendErr)
	}
	return nil
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive a REAL child process through the manager, because the
// property that matters is one no unit test of the store can establish: that
// what a live process writes to its own stdout/stderr reaches the spec's
// buffer, and is still there after that process is gone.

// waitForLog polls the spec's retained history until ok accepts a scrollback
// snapshot of it, then returns that snapshot. Polling rather than sleeping: the
// child writes on its own schedule, os/exec copies on its own goroutines, and
// the exit marker is recorded later still, on the manager's owner goroutine
// once cmd.Wait has returned.
//
// That last part is why the predicate is a parameter rather than a substring:
// a test that waited only for the OUTPUT would routinely observe the history
// before the exit marker was in it -- which is exactly how this helper's first
// version failed under -race, where the gap between the two widens.
func waitForLog(t *testing.T, m *Manager, specID string, what string, ok func(LogBatch) bool) LogBatch {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last LogBatch
	for {
		m.Logs().SetWatch([]string{specID})
		for _, b := range m.Logs().Drain() {
			if b.SpecID != specID || !b.Scrollback {
				continue
			}
			last = b
			if ok(b) {
				return b
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spec %q retained history never satisfied %s; last snapshot: %+v", specID, what, last)
		}
		// Drop the watch so the next round takes a fresh scrollback snapshot
		// rather than only the (already consumed) live delta.
		m.Logs().SetWatch(nil)
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForLogText is waitForLog for the common case: the history contains want.
func waitForLogText(t *testing.T, m *Manager, specID, want string) LogBatch {
	t.Helper()
	return waitForLog(t, m, specID, "output containing "+want, func(b LogBatch) bool {
		return strings.Contains(batchText(b), want)
	})
}

// hasEvent reports whether the snapshot carries a marker of this kind.
func hasEvent(b LogBatch, event string) bool {
	for _, e := range b.Entries {
		if e.Event == event {
			return true
		}
	}
	return false
}

// TestManagerRetainsCrashedProcessOutput is the acceptance case in miniature:
// a model process writes to stderr and dies, and the operator -- arriving
// afterwards, which is the normal case -- can still read what it printed.
//
// Before T3 this was impossible by construction: the buffer was a field of
// runningProc, and onProcExited set st.proc = nil, so everything but the
// ~2 KiB copied into LastError.StderrTail was garbage the moment it became
// interesting.
func TestManagerRetainsCrashedProcessOutput(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-crash", "crash-model")
	// The stub child prints "stubchild: scripted crash" to STDERR and exits
	// with the given code -- both halves of the requirement in one run.
	spec.Args = stubArgs(0, 150*time.Millisecond, 3, "")
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	// Wait for the CRASH TO BE PROCESSED, not merely for its output to have
	// been copied: the exit marker is recorded on the owner goroutine after
	// cmd.Wait returns, so the two are separated by real time.
	batch := waitForLog(t, m, "spec-crash", "the crash output plus its exit marker", func(b LogBatch) bool {
		return strings.Contains(batchText(b), "stubchild: scripted crash") && hasEvent(b, logEventExited)
	})

	// And the boundary markers must carry the real pid and exit code: that is
	// what turns a wall of text into "attempt N ended like this".
	var sawStarted, sawExited bool
	for _, e := range batch.Entries {
		switch e.Event {
		case logEventStarted:
			sawStarted = true
			if e.PID <= 0 {
				t.Errorf("started marker has pid %d, want the real child pid", e.PID)
			}
		case logEventExited:
			sawExited = true
			if e.ExitCode != 3 {
				t.Errorf("exited marker exit_code = %d, want 3 (the child's real exit code)", e.ExitCode)
			}
		}
	}
	if !sawStarted || !sawExited {
		t.Errorf("markers: started=%v exited=%v, want both", sawStarted, sawExited)
	}
}

// TestManagerRetainsTheWHOLEOfACrashedProcessOutput is the same requirement as
// the test above, stated as a NUMBER rather than as a substring match, because
// the substring match would also pass against the ~2 KiB tail that was all the
// predecessor kept.
//
// A process that prints eleven kilobytes of configuration and then dies during
// load is the shape of the case this feature exists for: the failure line is at
// the end, and the reason is halfway up. Against the commit this branch starts
// from the recoverable amount is exactly 2048 bytes (measured), because the
// buffer was a field of runningProc and onProcExited cleared st.proc.
func TestManagerRetainsTheWHOLEOfACrashedProcessOutput(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs /bin/sh to emit a controlled volume of output")
	}
	shrinkTimings(t)
	m := NewManager(ManagerOptions{
		Policy: LocalPolicy{AllowedBinaries: []string{"/bin/sh"}},
		Getenv: func(string) string { return "" },
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-volume", "volume-model")
	spec.Binary = "/bin/sh"
	spec.Args = []string{"-c", "i=0; while [ $i -lt 400 ]; do echo line-$i-XXXXXXXXXXXXXXXXXXXX; i=$((i+1)); done; exit 1"}
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	deadline := time.Now().Add(5 * time.Second)
	var best int
	for {
		m.Logs().SetWatch([]string{"spec-volume"})
		for _, b := range m.Logs().Drain() {
			if b.SpecID != "spec-volume" || !b.Scrollback {
				continue
			}
			if got := len(batchText(b)); got > best {
				best = got
			}
		}
		if best >= 10000 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("recoverable output after the crash = %d bytes, want the ~11000 the process printed", best)
		}
		m.Logs().SetWatch(nil)
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManagerLogRetentionIsNotWrittenToDisk is the load-bearing safety
// assertion, made against a live process rather than argued: nothing the child
// printed appears anywhere under the agent's working directory or its temp
// dir. Captured output can contain prompt text, and the entire feature is
// affordable only because it is never persisted.
func TestManagerLogRetentionIsNotWrittenToDisk(t *testing.T) {
	shrinkTimings(t)

	// A private HOME/TMPDIR/working directory for this test, so "nothing was
	// written" is a statement about a tree nothing else touches.
	sandbox := t.TempDir()
	t.Setenv("TMPDIR", sandbox)
	t.Setenv("HOME", sandbox)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(sandbox); err != nil {
		t.Fatalf("chdir sandbox: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	m := newTestManager(t, allowlistPolicy())
	// A distinctive needle, planted in the child's STDERR by the simplest
	// mechanism that needs no cooperation from the child at all: an unknown
	// flag makes Go's flag package print "flag provided but not defined:
	// -<needle>" and exit. So this exercises the stderr half of the capture
	// specifically, and if any byte of it were ever persisted, this string is
	// what would show up in the file that held it.
	const needle = "op-ai-gateway-log-needle-9d3f"
	spec := baseSpec("spec-needle", "needle-model")
	spec.Args = []string{"-" + needle}
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	// Wait until the output has actually been captured, so this is not a race
	// that passes because nothing happened yet.
	waitForLogText(t, m, "spec-needle", needle)

	var offenders []string
	err = filepath.WalkDir(sandbox, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not evidence of a write
		}
		if strings.Contains(d.Name(), needle) {
			offenders = append(offenders, path+" (filename)")
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr // same
		}
		if strings.Contains(string(raw), needle) {
			offenders = append(offenders, path+" (contents)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sandbox: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("managed-process output reached disk at %v -- it may contain prompt text and must never be persisted", offenders)
	}
}

// TestManagerLogRetentionFollowsSpecRemoval: deleting a spec releases its
// buffer. There is no row left in the portal to open a log view on, so keeping
// it would be memory held for something that no longer exists.
func TestManagerLogRetentionFollowsSpecRemoval(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-gone", "gone-model")
	spec.Args = stubArgs(0, 100*time.Millisecond, 1, "")
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})
	waitForLogText(t, m, "spec-gone", "stubchild: scripted crash")

	// The operator deletes the spec.
	m.Apply(Config{ETag: "e2", MaxProcesses: 1, Specs: []Spec{}})

	m.Logs().SetWatch([]string{"spec-gone"})
	for _, b := range m.Logs().Drain() {
		if b.SpecID == "spec-gone" && batchText(b) != "" {
			t.Fatalf("a deleted spec kept its retained output: %q", batchText(b))
		}
	}
}

// TestManagerLastErrorStderrTailStillWorks guards the pre-existing consumer of
// this buffer through the move from process-scoped to spec-scoped storage: the
// crash report must still carry the failing generation's own tail, and only
// its own.
func TestManagerLastErrorStderrTailStillWorks(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-tail", "tail-model")
	spec.Args = stubArgs(0, 100*time.Millisecond, 7, "")
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		var last *LastError
		for _, st := range m.Status() {
			if st.SpecID == "spec-tail" {
				last = st.LastError
			}
		}
		if last != nil && strings.Contains(last.StderrTail, "stubchild: scripted crash") {
			if last.ExitCode != 7 {
				t.Errorf("LastError.ExitCode = %d, want 7", last.ExitCode)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("LastError.StderrTail never carried the crash output; last = %+v", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

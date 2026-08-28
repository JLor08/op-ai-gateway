// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
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

// --- the resolved launch command, against a live process -------------------

// waitForCommand polls the spec's retained history until the newest
// command-carrying marker in a scrollback snapshot satisfies ok, then returns
// that ENTRY -- marker and command together, since the pid the command belongs
// to lives on the marker. Polling for the same reason waitForLog does: the
// child, os/exec's copying goroutines and the manager's owner goroutine all move
// on their own schedules.
func waitForCommand(t *testing.T, m *Manager, specID, what string, ok func(LogEntry) bool) LogEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last LogEntry
	for {
		m.Logs().SetWatch([]string{specID})
		for _, b := range m.Logs().Drain() {
			if b.SpecID != specID || !b.Scrollback {
				continue
			}
			// The NEWEST opening marker: that is the generation whose output is
			// at the tail, and the one a caller asking "what is running" means.
			for _, e := range b.Entries {
				if e.Command != nil {
					last = e
				}
			}
			if ok(last) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("spec %q never reported a command satisfying %s; last: %+v", specID, what, last)
		}
		m.Logs().SetWatch(nil)
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManagerReportsTheCommandItActuallyExecuted is the acceptance case: a real
// child is launched from a spec written as a TEMPLATE, and what the operator
// gets back is the resolved command -- the ephemeral ${PORT} as a number, and
// the real pid of the process whose output sits underneath it.
//
// Against the commit this branch starts from nothing retained the resolved argv
// at all: ExpandPlaceholders built it, exec.Command consumed it, and it was
// dropped.
func TestManagerReportsTheCommandItActuallyExecuted(t *testing.T) {
	shrinkTimings(t)
	// A getenv that actually defines PATH, so the reported environment is the
	// real agent-provided base block rather than an empty one -- "the complete
	// environment the child received" is one of the four things this reports,
	// and an all-empty getenv would make that assertion vacuous.
	m := NewManager(ManagerOptions{
		Policy: allowlistPolicy(),
		Getenv: func(k string) string {
			if k == "PATH" {
				return "/usr/bin:/bin"
			}
			return ""
		},
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-cmd", "cmd-model")
	spec.Args = append(stubArgs(0, 0, 0, ""), "-alias", "${MODEL}")
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	marker := waitForCommand(t, m, "spec-cmd", "a started marker carrying a command", func(e LogEntry) bool {
		return e.Command != nil && e.Event == logEventStarted && e.PID > 0
	})
	cmd := marker.Command

	if cmd.Binary != stubchildPath {
		t.Errorf("binary = %q, want %q", cmd.Binary, stubchildPath)
	}
	joined := strings.Join(cmd.Args, " ")
	if strings.Contains(joined, "${PORT}") || strings.Contains(joined, "${MODEL}") {
		t.Fatalf("args = %q still carry a template placeholder -- the operator would be reading what they typed, not what ran", joined)
	}
	if !strings.Contains(joined, "-alias cmd-model") {
		t.Errorf("args = %q, want ${MODEL} resolved to the spec's upstream_model", joined)
	}
	// The port must be the one the process is actually listening on, which is
	// also what the status row reports -- if these two ever disagree the panel
	// is describing a different process.
	st := statusFor(m, "spec-cmd")
	if st == nil || st.Port == 0 {
		t.Fatalf("status = %+v, want a running process with a port", st)
	}
	if !strings.Contains(joined, "-port "+strconv.Itoa(st.Port)) {
		t.Errorf("args = %q, want the resolved port %d that the status row reports", joined, st.Port)
	}
	if marker.PID != st.PID {
		t.Errorf("marker pid = %d, status pid = %d -- the command must belong to the generation whose output is being read", marker.PID, st.PID)
	}
	// The environment is the complete, agent-built block the child received.
	if v, ok := envValue(cmd.Env, "PATH"); !ok || v != "/usr/bin:/bin" {
		t.Errorf("env PATH = %q (present=%v), want the minimal base environment the child was actually given", v, ok)
	}
}

// TestManagerReportsEachGenerationsOwnCommand: ${PORT} is resolved afresh on
// every start, so across a crash loop the command DIFFERS between attempts. With
// the command on each generation's opening marker, the buffer shows every
// attempt's own -- attribution by construction rather than by a rule about which
// one to display, and the per-attempt difference is visible instead of collapsed
// into "the latest".
func TestManagerReportsEachGenerationsOwnCommand(t *testing.T) {
	shrinkTimings(t)
	m := newTestManager(t, allowlistPolicy())

	spec := baseSpec("spec-loop", "loop-model")
	spec.Args = stubArgs(0, 80*time.Millisecond, 3, "")
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	// Read the markers from ONE snapshot: two snapshots would let a restart
	// slip between the reads.
	var batch LogBatch
	deadline := time.Now().Add(15 * time.Second)
	for {
		m.Logs().SetWatch([]string{"spec-loop"})
		for _, b := range m.Logs().Drain() {
			if b.SpecID != "spec-loop" || !b.Scrollback {
				continue
			}
			if countEvents(b, logEventStarted) >= 2 {
				batch = b
			}
		}
		if batch.SpecID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the crash loop never produced two generations in one snapshot")
		}
		m.Logs().SetWatch(nil)
		time.Sleep(20 * time.Millisecond)
	}

	var seenPorts, seenPIDs []string
	for _, e := range batch.Entries {
		if e.Event != logEventStarted {
			continue
		}
		if e.Command == nil {
			t.Fatalf("a started marker carries no command: %+v", e)
		}
		if e.PID <= 0 {
			t.Errorf("started marker pid = %d, want the real child pid", e.PID)
		}
		args := strings.Join(e.Command.Args, " ")
		if strings.Contains(args, "${PORT}") {
			t.Fatalf("marker for pid %d reports the template, not the command: %q", e.PID, args)
		}
		seenPIDs = append(seenPIDs, strconv.Itoa(e.PID))
		for i, a := range e.Command.Args {
			if a == "-port" && i+1 < len(e.Command.Args) {
				seenPorts = append(seenPorts, e.Command.Args[i+1])
			}
		}
	}
	if len(seenPorts) < 2 {
		t.Fatalf("found %d per-generation commands, want one per attempt (ports %v)", len(seenPorts), seenPorts)
	}
	// Each attempt grabbed its own ephemeral port, so the commands must differ
	// -- which is exactly what a single "latest command" view cannot show.
	if seenPorts[0] == seenPorts[len(seenPorts)-1] {
		t.Errorf("every attempt reports port %s; the resolved port should differ per generation (pids %v)", seenPorts[0], seenPIDs)
	}
}

// TestManagerReportsTheCommandOfAFailedExec: a spec whose exec fails prints
// nothing at all, so the resolved command is the ONLY content the log view has
// -- and it is exactly the case where the difference between the template and
// what ran is most likely to BE the bug (a binary or work_dir that is not what
// the operator thinks).
func TestManagerReportsTheCommandOfAFailedExec(t *testing.T) {
	shrinkTimings(t)
	missing := filepath.Join(t.TempDir(), "not-installed")
	m := NewManager(ManagerOptions{
		Policy: LocalPolicy{AllowedBinaries: []string{missing}},
		Getenv: func(string) string { return "" },
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-noexec", "noexec-model")
	spec.Binary = missing
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	marker := waitForCommand(t, m, "spec-noexec", "the attempted command of a failed exec", func(e LogEntry) bool {
		return e.Command != nil
	})
	if marker.Event != logEventStartFailed {
		t.Errorf("event = %q, want %q: a marker claiming output begins here would be a lie", marker.Event, logEventStartFailed)
	}
	if marker.PID != 0 {
		t.Errorf("marker pid = %d, want 0: the exec failed, so no process ever existed", marker.PID)
	}
	if marker.Command.Binary != missing {
		t.Errorf("command binary = %q, want the binary the failed exec attempted (%q)", marker.Command.Binary, missing)
	}
}

// TestManagerResolvedCommandMasksASecretButNotThePort is the security property
// end to end, through the real launch path rather than against the masking
// helper: a ${AGENT_ENV:NAME} secret placed in an ARGUMENT is masked in what the
// agent retains, while the resolved port beside it stays visible.
//
// It also asserts the store never held the plaintext at all -- masking happens
// where the substitution happens, so there is nothing for a later bug in the
// store, the drain or the frame to leak.
func TestManagerResolvedCommandMasksASecretButNotThePort(t *testing.T) {
	shrinkTimings(t)
	const secret = "op-ai-gateway-argv-secret-7c1a"
	m := NewManager(ManagerOptions{
		Policy: allowlistPolicy(),
		Getenv: func(k string) string {
			if k == "MODEL_TOKEN" {
				return secret
			}
			return ""
		},
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-secret", "secret-model")
	spec.Args = append(stubArgs(0, 0, 0, ""), "-alias", "${AGENT_ENV:MODEL_TOKEN}")
	spec.Env = map[string]string{"MODEL_TOKEN": "${AGENT_ENV:MODEL_TOKEN}"}
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	cmd := waitForCommand(t, m, "spec-secret", "a started marker carrying a command", func(e LogEntry) bool {
		return e.Command != nil && e.PID > 0
	}).Command

	joined := strings.Join(cmd.Args, " ") + " " + strings.Join(cmd.Env, " ")
	if strings.Contains(joined, secret) {
		t.Fatalf("the retained command carries the resolved secret: %q", joined)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "-alias ${AGENT_ENV:MODEL_TOKEN}") {
		t.Errorf("args = %v, want the masked argument to read as its own placeholder", cmd.Args)
	}
	if v, ok := envValue(cmd.Env, "MODEL_TOKEN"); !ok || v != "${AGENT_ENV:MODEL_TOKEN}" {
		t.Errorf("env MODEL_TOKEN = %q (present=%v), want the placeholder, with the key intact", v, ok)
	}
	if !cmd.Masked {
		t.Error("Masked = false although a secret was masked")
	}
	st := statusFor(m, "spec-secret")
	if st == nil || st.Port == 0 {
		t.Fatalf("status = %+v, want a running process", st)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "-port "+strconv.Itoa(st.Port)) {
		t.Errorf("args = %v, want the resolved port still visible beside the mask -- masking must not cost the panel its usefulness", cmd.Args)
	}
}

// TestManagerResolvedCommandIsNotWrittenToDisk mirrors
// TestManagerLogRetentionIsNotWrittenToDisk for the command: it is a resolved
// argv and environment, which is closer to user data than status ever was, and
// it has exactly as little business on disk as the output does.
//
// Two needles, because they fail differently: one in a plain ARGUMENT (which
// the retained command shows, so a persisting bug would write it out verbatim)
// and one behind ${AGENT_ENV:...} (which the retained command masks, so finding
// it anywhere means the masking was bypassed rather than merely persisted).
func TestManagerResolvedCommandIsNotWrittenToDisk(t *testing.T) {
	shrinkTimings(t)

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

	const argNeedle = "op-ai-gateway-argv-needle-2e8b"
	const secretNeedle = "op-ai-gateway-secret-needle-5a4d"
	m := NewManager(ManagerOptions{
		Policy: allowlistPolicy(),
		Getenv: func(k string) string {
			if k == "MODEL_TOKEN" {
				return secretNeedle
			}
			return ""
		},
	})
	t.Cleanup(m.Close)

	spec := baseSpec("spec-argv-needle", "argv-needle-model")
	spec.Args = append(stubArgs(0, 0, 0, ""), "-alias", argNeedle)
	spec.Env = map[string]string{"MODEL_TOKEN": "${AGENT_ENV:MODEL_TOKEN}"}
	spec.Pinned = true
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	// Wait until the command has actually been retained, so this is not a race
	// that passes because nothing happened yet.
	cmd := waitForCommand(t, m, "spec-argv-needle", "the retained command", func(e LogEntry) bool {
		return e.Command != nil && e.PID > 0
	}).Command
	if !strings.Contains(strings.Join(cmd.Args, " "), argNeedle) {
		t.Fatalf("args = %v, want the argument needle: this test proves nothing if the value was never retained", cmd.Args)
	}

	var offenders []string
	err = filepath.WalkDir(sandbox, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not evidence of a write
		}
		for _, needle := range []string{argNeedle, secretNeedle} {
			if strings.Contains(d.Name(), needle) {
				offenders = append(offenders, path+" (filename)")
				return nil
			}
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil //nolint:nilerr // same
		}
		for _, needle := range []string{argNeedle, secretNeedle} {
			if strings.Contains(string(raw), needle) {
				offenders = append(offenders, path+" (contents)")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk sandbox: %v", err)
	}
	if len(offenders) != 0 {
		t.Fatalf("the resolved launch command reached disk at %v -- argv and environment are never persisted, exactly like the output", offenders)
	}
}

// countEvents counts markers of one kind in a snapshot.
func countEvents(b LogBatch, event string) int {
	n := 0
	for _, e := range b.Entries {
		if e.Event == event {
			n++
		}
	}
	return n
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests drive a REAL child through the manager with a declared log file
// (#430, design §5.3, §7, §9): the stub child appends to the file
// (-log-file/-log-text/-log-exit-text) and the manager's tailer must carry its
// text into the spec's "engine" stream, per generation, and into last_error.

// engineLogPath is where every test's spec declares its log file.
const engineLogPath = "${CONFIG_DIR}/engine.log"

// setLogFileGrace sets the manager's post-exit tailer grace for one test. It
// must be called BEFORE the manager is built: the restore is a Cleanup, and
// Cleanups run last-registered-first, so it then runs after the manager's
// Close, once nothing reads the var any more.
func setLogFileGrace(t *testing.T, d time.Duration) {
	t.Helper()
	orig := logFileGrace
	logFileGrace = d
	t.Cleanup(func() { logFileGrace = orig })
}

// newLogFileManager is newTestManager with a config_files_dir, which is what
// ${CONFIG_DIR} resolves under.
func newLogFileManager(t *testing.T, policy LocalPolicy, configDir string) *Manager {
	t.Helper()
	m := NewManager(ManagerOptions{Policy: policy, Getenv: func(string) string { return "" }, ConfigFilesDir: configDir})
	t.Cleanup(m.Close)
	return m
}

// logFileSpec is baseSpec declaring the log file "engine" at engineLogPath and
// running the child with extra arguments after the standard ones. The child
// writes to the same path, so what it appends is what the tailer reads.
func logFileSpec(id, model string, extra ...string) Spec {
	spec := baseSpec(id, model)
	spec.Args = append(spec.Args, "-log-file", engineLogPath)
	spec.Args = append(spec.Args, extra...)
	spec.LogFiles = []LogFile{{Name: "engine", Path: engineLogPath}}
	spec.Pinned = true
	return spec
}

// streamRecords copies the retained records of one stream of specID, with
// their generations: the generation is not on the wire (LogEntry), and it is
// exactly what these tests have to tell apart.
func streamRecords(m *Manager, specID, stream string) []logRecord {
	s := m.logs
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.logs[specID]
	if l == nil || l.streams[stream] == nil {
		return nil
	}
	return slices.Clone(l.streams[stream].recs)
}

// genText is the text generation gen recorded on the records, in order.
func genText(recs []logRecord, gen uint64) string {
	var b strings.Builder
	for _, r := range recs {
		if r.gen == gen && r.event == "" {
			b.WriteString(r.text)
		}
	}
	return b.String()
}

// startedGens returns the generations and pids of the process stream's started
// markers, oldest first.
func startedGens(m *Manager, specID string) (gens []uint64, pids []int) {
	for _, r := range streamRecords(m, specID, "") {
		if r.event == logEventStarted {
			gens = append(gens, r.gen)
			pids = append(pids, r.pid)
		}
	}
	return gens, pids
}

// waitForStreamEvent waits until the stream of specID holds the event, and
// returns the snapshot it was seen in.
func waitForStreamEvent(t *testing.T, m *Manager, specID, stream, event string) LogBatch {
	t.Helper()
	return waitForLog(t, m, specID, stream+" event "+event, func(b LogBatch) bool {
		for _, e := range b.Entries {
			if e.Stream == stream && e.Event == event {
				return true
			}
		}
		return false
	})
}

// waitForLastError polls Status until ok accepts the spec's last_error.
func waitForLastError(t *testing.T, m *Manager, specID, what string, ok func(*LastError) bool) *LastError {
	t.Helper()
	var got *LastError
	waitUntil(t, 10*time.Second, what, func() bool {
		if s := statusFor(m, specID); s != nil && s.LastError != nil && ok(s.LastError) {
			got = s.LastError
			return true
		}
		return false
	})
	return got
}

// TestManagerTailsConfigDirLogFile: what the child appends to its declared
// ${CONFIG_DIR} file reaches the spec's "engine" stream, opened by a
// file_opened marker, and never the process stream.
func TestManagerTailsConfigDirLogFile(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newLogFileManager(t, allowlistPolicy(), t.TempDir())

	spec := logFileSpec("spec-lf", "lf-model", "-log-text", `engine says hello\n`)
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	b := waitForLog(t, m, "spec-lf", "engine text", func(b LogBatch) bool {
		return strings.Contains(streamText([]LogBatch{b}, "engine"), "engine says hello\n")
	})
	if !hasEvent(b, logEventFileOpened) {
		t.Errorf("engine stream has no %s marker: %+v", logEventFileOpened, b.Entries)
	}
	if strings.Contains(streamText([]LogBatch{b}, ""), "engine says hello") {
		t.Errorf("file text leaked into the process stream: %q", streamText([]LogBatch{b}, ""))
	}
}

// TestManagerCreatesConfigDirWithoutConfigFiles: a spec with a ${CONFIG_DIR}
// log file but no config_files still gets its instance directory before exec,
// so the server can create its log there.
func TestManagerCreatesConfigDirWithoutConfigFiles(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	base := t.TempDir()
	m := newLogFileManager(t, allowlistPolicy(), base)

	spec := logFileSpec("spec-mkdir", "mkdir-model", "-log-text", `created\n`)
	if len(spec.ConfigFiles) != 0 {
		t.Fatal("the spec must declare no config_files")
	}
	dir := filepath.Join(base, "spec-mkdir")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("instance dir exists before the start: %v", err)
	}
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	// The child exits (log.Fatal) when it cannot create the file, so the text
	// arriving proves the directory was there at exec.
	waitForLog(t, m, "spec-mkdir", "engine text", func(b LogBatch) bool {
		return strings.Contains(streamText([]LogBatch{b}, "engine"), "created\n")
	})
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("instance dir %s: %v, %v; want a real directory", dir, fi, err)
	}
}

// TestManagerRemovesConfigDirLogBeforeExec: a ${CONFIG_DIR} log file left by an
// earlier run is removed before exec, so a generation's stream reads only its
// own file from offset 0 -- the first generation (the file on disk before the
// agent started it) and the second (the file its predecessor wrote) alike.
func TestManagerRemovesConfigDirLogBeforeExec(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "spec-rm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "spec-rm", "engine.log"), []byte("left over from before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newLogFileManager(t, allowlistPolicy(), base)

	spec := logFileSpec("spec-rm", "rm-model", "-log-text", `run {pid}\n`)
	spec.Args = append(spec.Args, "-crash-after", "300ms", "-exit-code", "1")
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	var gens []uint64
	var pids []int
	waitUntil(t, 10*time.Second, "two generations with their engine text", func() bool {
		gens, pids = startedGens(m, "spec-rm")
		if len(gens) < 2 {
			return false
		}
		recs := streamRecords(m, "spec-rm", "engine")
		return strings.Contains(genText(recs, gens[1]), "run "+strconv.Itoa(pids[1])+"\n")
	})
	recs := streamRecords(m, "spec-rm", "engine")
	for i := range 2 {
		want := "run " + strconv.Itoa(pids[i]) + "\n"
		if got := genText(recs, gens[i]); got != want {
			t.Errorf("generation %d engine text = %q, want exactly its own %q", i+1, got, want)
		}
		first, ok := openingRecordOf(recs, gens[i])
		if !ok || first.event != logEventFileOpened {
			t.Errorf("generation %d engine stream starts with %+v, want %s", i+1, first, logEventFileOpened)
		}
	}
}

// openingRecordOf is the record generation gen's file stream opens with: its
// first record other than file_waiting. A ${CONFIG_DIR} file is removed before
// exec, so its tailer usually starts before the server has created it again
// and records file_waiting first; that status is not the generation's text,
// and the generation boundary is the file_opened that follows it.
func openingRecordOf(recs []logRecord, gen uint64) (logRecord, bool) {
	for _, r := range recs {
		if r.gen == gen && r.event != logEventFileWaiting {
			return r, true
		}
	}
	return logRecord{}, false
}

// TestManagerLogFileTailsInLastError: a crash's last_error gains the end of the
// declared file once the tailer's grace is over -- at most 2048 bytes, ending
// with the server's last line -- while stderr_tail keeps process output only.
func TestManagerLogFileTailsInLastError(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	// No restart: the tails come from the grace running out, not from a next
	// generation ending it.
	backoffBase, backoffCap = time.Minute, time.Minute
	setLogFileGrace(t, 300*time.Millisecond)
	m := newLogFileManager(t, allowlistPolicy(), t.TempDir())

	filler := strings.Repeat(`engine filler line 0123456789\n`, 150) // ~4.5 KiB
	spec := logFileSpec("spec-tails", "tails-model",
		"-health-delay", "1m", "-crash-after", "300ms", "-exit-code", "1",
		"-log-text", filler, "-log-exit-text", `engine last line\n`)
	spec.StartupTimeoutSeconds = 30
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	le := waitForLastError(t, m, "spec-tails", "last_error with log_file_tails", func(le *LastError) bool {
		return len(le.LogFileTails) > 0
	})
	if len(le.LogFileTails) != 1 || le.LogFileTails[0].Name != "engine" {
		t.Fatalf("log_file_tails = %+v, want one entry named engine", le.LogFileTails)
	}
	tail := le.LogFileTails[0].Tail
	if !strings.HasSuffix(tail, "engine last line\n") {
		t.Errorf("tail does not end with the server's last line: ...%q", tail[max(0, len(tail)-80):])
	}
	if len(tail) > 2048 || len(tail) < 2000 {
		t.Errorf("tail is %d bytes, want the last 2048", len(tail))
	}
	if le.Failures != 1 || !strings.Contains(le.Message, "before becoming healthy") {
		t.Errorf("tails attached to %+v, want the crash's own last_error", le)
	}
	if strings.Contains(le.StderrTail, "engine") || !strings.Contains(le.StderrTail, "scripted crash") {
		t.Errorf("stderr_tail = %q, want process output only", le.StderrTail)
	}
}

// TestManagerLogFileTailsInStartTimeoutError: a generation that never becomes
// healthy is killed by the owner for its startup timeout -- an INTENTIONAL
// exit, whose last_error was recorded before the kill -- and that last_error
// still gains the file's tails: the never-healthy server (a long load that
// dies, or hangs) is the case a file log matters most for.
func TestManagerLogFileTailsInStartTimeoutError(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	backoffBase, backoffCap = time.Minute, time.Minute
	setLogFileGrace(t, 300*time.Millisecond)
	m := newLogFileManager(t, allowlistPolicy(), t.TempDir())

	spec := logFileSpec("spec-timeout", "timeout-model",
		"-health-delay", "1m", "-log-text", `engine loading\nengine still loading\n`)
	spec.StartupTimeoutSeconds = 1
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	le := waitForLastError(t, m, "spec-timeout", "the start timeout's last_error with log_file_tails", func(le *LastError) bool {
		return len(le.LogFileTails) > 0
	})
	if !strings.Contains(le.Message, "startup timed out") {
		t.Fatalf("tails attached to %+v, want the start timeout's own last_error", le)
	}
	if len(le.LogFileTails) != 1 || le.LogFileTails[0].Name != "engine" || le.LogFileTails[0].Tail != "engine loading\nengine still loading\n" {
		t.Errorf("log_file_tails = %+v, want the engine file ending with its last line", le.LogFileTails)
	}
}

// TestManagerLogFileTailsNotAttachedToALaterError: when last_error has been
// replaced by the time a generation's tails are ready, the tails are dropped --
// they never describe the later error. Here the next start attempt fails
// before exec (its binary is gone), which replaces last_error in the same turn
// that ends the crashed generation's grace.
func TestManagerLogFileTailsNotAttachedToALaterError(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setLogFileGrace(t, 500*time.Millisecond)
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "stubchild-copy")
	raw, err := os.ReadFile(stubchildPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, raw, 0o755); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "exit-now")
	m := newLogFileManager(t, LocalPolicy{AllowedBinaries: []string{bin}}, t.TempDir())

	spec := logFileSpec("spec-later", "later-model",
		"-health-delay", "1m", "-exit-when", gate, "-exit-code", "1",
		"-log-text", `engine before the crash\n`)
	spec.Binary = bin
	spec.StartupTimeoutSeconds = 30
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	waitForLog(t, m, "spec-later", "engine text", func(b LogBatch) bool {
		return strings.Contains(streamText([]LogBatch{b}, "engine"), "engine before the crash")
	})
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The crash is recorded, then replaced by the failed restart.
	waitForLastError(t, m, "spec-later", "a later last_error than the crash", func(le *LastError) bool {
		return le.Failures >= 2 || !strings.Contains(le.Message, "before becoming healthy") && le.Message != ""
	})
	deadline := time.Now().Add(3 * logFileGrace)
	for time.Now().Before(deadline) {
		if s := statusFor(m, "spec-later"); s != nil && s.LastError != nil && len(s.LastError.LogFileTails) > 0 {
			t.Fatalf("a later last_error carries the crashed generation's tails: %+v", s.LastError)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleLogFileTailsAttachesOnlyToItsOwnError pins the identity rule at the
// owner: the tails land on a COPY of the very last_error they were cut for, and
// on nothing else.
func TestHandleLogFileTailsAttachesOnlyToItsOwnError(t *testing.T) {
	crash := &LastError{Message: "crash", Failures: 1}
	st := &specState{lastError: crash}
	o := &owner{m: &Manager{transitions: make(chan struct{}, 1)}, specs: map[string]*specState{"s": st}}
	tails := []LogFileTail{{Name: "engine", Tail: "last\n"}}

	o.handleLogFileTails(cmdLogFileTails{key: "s", le: &LastError{Message: "crash", Failures: 1}, tails: tails})
	if st.lastError != crash || crash.LogFileTails != nil {
		t.Fatalf("tails attached to an equal but different last_error: %+v", st.lastError)
	}

	o.handleLogFileTails(cmdLogFileTails{key: "s", le: crash, tails: tails})
	if st.lastError == crash || crash.LogFileTails != nil {
		t.Fatal("the last_error handed out was modified in place instead of replaced by a copy")
	}
	if st.lastError.Message != "crash" || st.lastError.Failures != 1 || !slices.Equal(st.lastError.LogFileTails, tails) {
		t.Fatalf("lastError = %+v, want the crash with the tails", st.lastError)
	}

	o.handleLogFileTails(cmdLogFileTails{key: "gone", le: crash, tails: tails}) // no state: a no-op
}

// TestManagerFastCrashLoopKeepsGenerationsApart is Review Focus 3: generation
// N crashes, N+1 starts within N's grace and recreates the ${CONFIG_DIR} file
// under the same name. N's final lines still arrive and attach to N's own
// last_error, N+1 starts from offset 0 of its own file with its own
// file_opened, and N's stream never holds N+1's text.
//
// The grace is raised to 30 s, far beyond the test's deadlines: N's tails can
// then only arrive in time because N+1's start ended N's grace (EndGrace).
func TestManagerFastCrashLoopKeepsGenerationsApart(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	setLogFileGrace(t, 30*time.Second)
	dir := t.TempDir()
	gate := filepath.Join(dir, "exit-now")
	never := filepath.Join(dir, "never-healthy")
	m := newLogFileManager(t, allowlistPolicy(), t.TempDir())

	// Never healthy (the health gate is never created), so N+1 stays starting
	// and N's last_error stays the current one; only the generation that
	// consumes the exit gate exits.
	spec := logFileSpec("spec-loop", "loop-model",
		"-health-gate", never, "-exit-when", gate, "-exit-code", "1",
		"-log-text", `start {pid}\n`, "-log-exit-text", `last {pid}\n`)
	spec.StartupTimeoutSeconds = 30
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	var gens []uint64
	var pids []int
	waitUntil(t, 10*time.Second, "generation N's engine text", func() bool {
		gens, pids = startedGens(m, "spec-loop")
		return len(gens) == 1 && strings.Contains(genText(streamRecords(m, "spec-loop", "engine"), gens[0]), "start ")
	})
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	le := waitForLastError(t, m, "spec-loop", "N's last_error with its tails", func(le *LastError) bool {
		return len(le.LogFileTails) > 0
	})
	pidN := strconv.Itoa(pids[0])
	if le.Failures != 1 || len(le.LogFileTails) != 1 || le.LogFileTails[0].Tail != "start "+pidN+"\nlast "+pidN+"\n" {
		t.Fatalf("N's last_error = %+v, want failure 1 with exactly N's lines", le)
	}

	waitUntil(t, 10*time.Second, "generation N+1's engine text", func() bool {
		gens, pids = startedGens(m, "spec-loop")
		return len(gens) == 2 && strings.Contains(genText(streamRecords(m, "spec-loop", "engine"), gens[1]), "start ")
	})
	recs := streamRecords(m, "spec-loop", "engine")
	pidN1 := strconv.Itoa(pids[1])
	if got := genText(recs, gens[0]); got != "start "+pidN+"\nlast "+pidN+"\n" {
		t.Errorf("N's engine text = %q, want N's lines only", got)
	}
	if got := genText(recs, gens[1]); got != "start "+pidN1+"\n" {
		t.Errorf("N+1's engine text = %q, want its own file from offset 0", got)
	}
	if first, ok := openingRecordOf(recs, gens[1]); !ok || first.event != logEventFileOpened {
		t.Errorf("N+1's engine stream starts with %+v, want %s", first, logEventFileOpened)
	}
	if s := statusFor(m, "spec-loop"); s == nil || s.State != StateStarting {
		t.Errorf("N+1 is not starting: %+v", s)
	}
}

// TestManagerRefusedLogFileMarksNotPermitted: a declared file the path policy
// refuses marks file_not_permitted on its stream, and the start goes ahead.
func TestManagerRefusedLogFileMarksNotPermitted(t *testing.T) {
	skipOnWindows(t)
	shrinkTimings(t)
	m := newLogFileManager(t, allowlistPolicy(), t.TempDir())

	spec := baseSpec("spec-refused", "refused-model")
	spec.Pinned = true
	spec.LogFiles = []LogFile{{Name: "engine", Path: filepath.Join(t.TempDir(), "outside.log")}}
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	waitForStreamEvent(t, m, "spec-refused", "engine", logEventFileNotPermitted)
	waitForState(t, m, "spec-refused", StateRunning)
}

// TestManagerLogFileRetentionIsNotWrittenToDisk is
// TestManagerLogRetentionIsNotWrittenToDisk for a log file: the agent reads the
// server's file into RAM only, so the needle the child writes into it is found
// on disk in that file and nowhere else.
func TestManagerLogFileRetentionIsNotWrittenToDisk(t *testing.T) {
	skipOnWindows(t)
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

	base := filepath.Join(sandbox, "config-files")
	m := newLogFileManager(t, allowlistPolicy(), base)
	const needle = "op-ai-gateway-logfile-needle-5c1e"
	spec := logFileSpec("spec-lf-needle", "lf-needle-model", "-log-text", needle+`\n`)
	m.Apply(Config{ETag: "e1", MaxProcesses: 1, Specs: []Spec{spec}})

	waitForLog(t, m, "spec-lf-needle", "the needle on the engine stream", func(b LogBatch) bool {
		return strings.Contains(streamText([]LogBatch{b}, "engine"), needle)
	})

	own := filepath.Join(base, "spec-lf-needle", "engine.log")
	var offenders []string
	sawOwn := false
	err = filepath.WalkDir(sandbox, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable entry is not evidence of a write
		}
		if strings.Contains(d.Name(), needle) {
			offenders = append(offenders, path+" (filename)")
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil || !strings.Contains(string(raw), needle) {
			return nil //nolint:nilerr // same
		}
		if path == own {
			sawOwn = true
			return nil
		}
		offenders = append(offenders, path+" (contents)")
		return nil
	})
	if err != nil {
		t.Fatalf("walk sandbox: %v", err)
	}
	if !sawOwn {
		t.Fatalf("the needle is not in the server's own log file %s; the walk proves nothing", own)
	}
	if len(offenders) != 0 {
		t.Fatalf("log-file text reached disk outside the server's own file at %v", offenders)
	}
}

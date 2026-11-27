// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tailerHarness is one tailer under test: a store, the file stream it writes
// into, and the file's path. Every wait polls with a generous deadline, so the
// tests do not depend on scheduling: Interval is 5 ms, but nothing asserts on
// how many polls ran.
type tailerHarness struct {
	t      *testing.T
	store  *LogStore
	tailer *fileTailer
}

const (
	tailerTestSpec   = "s1"
	tailerTestStream = "e"
	tailerWait       = 10 * time.Second
)

// newTailerTarget resolves ${CONFIG_DIR}/name for spec s1 under a fresh
// config_files_dir, through the real policy, and returns the target and the
// file's path (not created).
func newTailerTarget(t *testing.T, name string) (logFileTarget, string, string) {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "cfg")
	inst := filepath.Join(cfgDir, tailerTestSpec)
	if err := os.MkdirAll(inst, 0o755); err != nil {
		t.Fatal(err)
	}
	return openTarget(t, cfgDir, nil, "${CONFIG_DIR}/"+name), filepath.Join(inst, name), dir
}

// testTailerConfig is the brief's test configuration: Interval 5 ms, Grace
// 50 ms, production caps unless a test narrows them.
func testTailerConfig(target logFileTarget, log *procLog) tailerConfig {
	return tailerConfig{
		Target:    target,
		Log:       log,
		Interval:  5 * time.Millisecond,
		Grace:     50 * time.Millisecond,
		TickBytes: logFileTickBytes,
		MaxLine:   logFileMaxLine,
	}
}

func startHarness(t *testing.T, target logFileTarget, mod func(*tailerConfig)) *tailerHarness {
	t.Helper()
	s := newTestLogStore(t, 1<<20, 0)
	p := s.newProc(tailerTestSpec)
	cfg := testTailerConfig(target, p.ForStream(tailerTestStream))
	if mod != nil {
		mod(&cfg)
	}
	h := &tailerHarness{t: t, store: s}
	h.tailer = startFileTailer(cfg)
	t.Cleanup(func() { h.stop() })
	return h
}

// stop stops the tailer and waits for it to finish, failing on a hang.
func (h *tailerHarness) stop() {
	h.t.Helper()
	select {
	case <-h.tailer.Stop():
	case <-time.After(tailerWait):
		h.t.Fatal("tailer did not finish after Stop")
	}
}

func (h *tailerHarness) recs() []logRecord {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	l := h.store.logs[tailerTestSpec]
	if l == nil || l.streams[tailerTestStream] == nil {
		return nil
	}
	return slices.Clone(l.streams[tailerTestStream].recs)
}

func (h *tailerHarness) text() string { return ringText(h.store, tailerTestSpec, tailerTestStream) }

func (h *tailerHarness) events() []string {
	var out []string
	for _, r := range h.recs() {
		if r.event != "" {
			out = append(out, r.event)
		}
	}
	return out
}

func (h *tailerHarness) dropped() int64 {
	var n int64
	for _, r := range h.recs() {
		n += r.dropped
	}
	return n
}

// flat renders the stream as text with each event as [event], so a test can
// assert the exact order of text and markers.
func (h *tailerHarness) flat() string {
	var b strings.Builder
	for _, r := range h.recs() {
		if r.event != "" {
			b.WriteString("[" + r.event + "]")
		}
		b.WriteString(r.text)
	}
	return b.String()
}

// waitFor polls cond until it holds or the deadline passes.
func (h *tailerHarness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(tailerWait)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; stream = %q", what, h.flat())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *tailerHarness) waitText(want string) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("text %q", want), func() bool { return strings.Contains(h.text(), want) })
}

func (h *tailerHarness) waitEvent(ev string) {
	h.t.Helper()
	h.waitFor("event "+ev, func() bool { return slices.Contains(h.events(), ev) })
}

// waitDone waits for the tailer to end on its own (binary, not permitted),
// without Stop.
func (h *tailerHarness) waitDone() {
	h.t.Helper()
	select {
	case <-h.tailer.done:
	case <-time.After(tailerWait):
		h.t.Fatalf("tailer did not stop on its own; stream = %q", h.flat())
	}
}

func appendFile(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestTailerAppend(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "one\n")
	h := startHarness(t, target, nil)
	h.waitText("one\n")
	appendFile(t, path, "two\n")
	appendFile(t, path, "three")
	h.waitText("three")
	h.stop()
	if got, want := h.flat(), "["+logEventFileOpened+"]one\ntwo\nthree"; got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestTailerStartAtEOFSkipsOldContent(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "old run\n")
	h := startHarness(t, target, func(c *tailerConfig) { c.StartAtEOF = true })
	// file_opened is recorded when startFileTailer returns: the offset is
	// fixed as of the start, so what is appended now is this run's.
	if evs := h.events(); !slices.Equal(evs, []string{logEventFileOpened}) {
		t.Fatalf("events right after start = %v, want [file_opened]", evs)
	}
	appendFile(t, path, "new run\n")
	h.waitText("new run\n")
	h.stop()
	if got := h.text(); got != "new run\n" {
		t.Fatalf("text = %q, want only the content appended after the start", got)
	}
}

// TestTailerStartAtEOFReadsAFileCreatedLater: a file that did not exist at the
// start is this generation's own, so it is read from offset 0.
func TestTailerStartAtEOFReadsAFileCreatedLater(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	h := startHarness(t, target, func(c *tailerConfig) { c.StartAtEOF = true })
	h.waitEvent(logEventFileWaiting)
	mustWrite(t, path, "first line\n")
	h.waitText("first line\n")
}

func TestTailerMissingThenCreated(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	var opens atomic.Int64
	h := startHarness(t, target, func(c *tailerConfig) {
		c.Open = func(tg logFileTarget) (*os.File, error) { opens.Add(1); return openLogFile(tg) }
	})
	h.waitEvent(logEventFileWaiting)
	// Let it poll the missing file several times: file_waiting is recorded
	// once, not per poll.
	h.waitFor("several polls of the missing file", func() bool { return opens.Load() >= 5 })
	mustWrite(t, path, "hello\n")
	h.waitText("hello\n")
	h.stop()
	if got, want := h.flat(), "["+logEventFileWaiting+"]["+logEventFileOpened+"]hello\n"; got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestTailerMissingForever(t *testing.T) {
	target, _, _ := newTailerTarget(t, "engine.log")
	h := startHarness(t, target, nil)
	h.waitEvent(logEventFileWaiting)
	h.stop()
	if got, want := h.events(), []string{logEventFileWaiting, logEventFileMissing}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTailerTruncate(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "one\ntwo\n")
	h := startHarness(t, target, nil)
	h.waitText("two\n")
	if err := os.WriteFile(path, []byte("x\n"), 0o600); err != nil { // O_TRUNC, shorter than the offset
		t.Fatal(err)
	}
	h.waitText("x\n")
	h.stop()
	if got, want := h.flat(), "["+logEventFileOpened+"]one\ntwo\n["+logEventFileTruncated+"]x\n"; got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestTailerRotateRename(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "a\n")
	h := startHarness(t, target, nil)
	h.waitText("a\n")
	appendFile(t, path, "b\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	// While no new file exists the rotation is in progress: the tailer keeps
	// reading the handle it holds, so the old file's last lines arrive.
	appendFile(t, path+".1", "c\n")
	h.waitText("c\n")
	mustWrite(t, path, "d\n")
	h.waitText("d\n")
	h.stop()
	if got, want := h.flat(), "["+logEventFileOpened+"]a\nb\nc\n["+logEventFileRotated+"]d\n"; got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

// TestTailerRotateToEscapingSymlinkIsRefused is Review Focus 1: a rotation
// that swaps the file for a link leading out of ${CONFIG_DIR} is refused on
// the reopen, the outside file's content never reaches the ring, and the
// tailer stops with file_not_permitted.
func TestTailerRotateToEscapingSymlinkIsRefused(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege on Windows")
	}
	target, path, dir := newTailerTarget(t, "engine.log")
	outside := filepath.Join(dir, "outside", "shadow")
	mustWrite(t, outside, "root:SECRET-HASH\n")
	mustWrite(t, path, "inside\n")
	h := startHarness(t, target, nil)
	h.waitText("inside\n")
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, outside, path)
	h.waitDone()
	if strings.Contains(h.text(), "SECRET") {
		t.Fatalf("the outside file's content reached the ring: %q", h.text())
	}
	if got, want := h.events(), []string{logEventFileOpened, logEventFileNotPermitted}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	h.stop()
	if strings.Contains(h.text(), "SECRET") {
		t.Fatalf("the outside file's content reached the ring after Stop: %q", h.text())
	}
}

// TestTailerFirstOpenRefusedStops: a file that is an escaping link from the
// start is refused once and the tailer ends -- no waiting, no missing.
func TestTailerFirstOpenRefusedStops(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("creating symlinks needs a privilege on Windows")
	}
	target, path, dir := newTailerTarget(t, "engine.log")
	outside := filepath.Join(dir, "outside", "shadow")
	mustWrite(t, outside, "SECRET\n")
	mustSymlink(t, outside, path)
	var opens atomic.Int64
	h := startHarness(t, target, func(c *tailerConfig) {
		c.Open = func(tg logFileTarget) (*os.File, error) { opens.Add(1); return openLogFile(tg) }
	})
	h.waitDone()
	h.stop()
	if got, want := h.events(), []string{logEventFileNotPermitted}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if n := opens.Load(); n != 1 {
		t.Fatalf("Open called %d times, want 1: a refused first open is final", n)
	}
	if h.text() != "" {
		t.Fatalf("text = %q, want none", h.text())
	}
}

// TestTailerReopenRefusalIsRetriedBriefly: a rotation by link()+unlink()
// briefly leaves the new file with two links, which the policy refuses. The
// tailer retries such a reopen refusal on the next polls and carries on when it
// clears; it gives up only after logFileReopenRefusals consecutive refusals.
func TestTailerReopenRefusalIsRetriedBriefly(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "a\n")
	var refuse atomic.Int64
	var calls atomic.Int64
	h := startHarness(t, target, func(c *tailerConfig) {
		c.Open = func(tg logFileTarget) (*os.File, error) {
			calls.Add(1)
			if refuse.Load() > 0 {
				refuse.Add(-1)
				return nil, fmt.Errorf("%w: test refusal", errLogFileNotPermitted)
			}
			return openLogFile(tg)
		}
	})
	h.waitText("a\n")
	refuse.Store(logFileReopenRefusals - 1)
	h.waitFor("the refusals to be consumed", func() bool { return refuse.Load() == 0 })
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, "b\n")
	h.waitText("b\n")
	h.stop()
	if got, want := h.flat(), "["+logEventFileOpened+"]a\n["+logEventFileRotated+"]b\n"; got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}

	// Now a lasting refusal: exactly logFileReopenRefusals attempts, then
	// file_not_permitted, and no further Open call.
	target2, path2, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path2, "a\n")
	calls.Store(0)
	var lasting atomic.Bool
	h2 := startHarness(t, target2, func(c *tailerConfig) {
		c.Open = func(tg logFileTarget) (*os.File, error) {
			if lasting.Load() {
				calls.Add(1)
				return nil, fmt.Errorf("%w: test refusal", errLogFileNotPermitted)
			}
			return openLogFile(tg)
		}
	})
	h2.waitText("a\n")
	lasting.Store(true)
	h2.waitDone()
	if n := calls.Load(); n != logFileReopenRefusals {
		t.Fatalf("refused reopens = %d, want %d", n, logFileReopenRefusals)
	}
	if got, want := h2.events(), []string{logEventFileOpened, logEventFileNotPermitted}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTailerLongLineCut(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	long := strings.Repeat("x", 100) + strings.Repeat("y", 50)
	mustWrite(t, path, "short\n"+long+"\nnext\n")
	h := startHarness(t, target, func(c *tailerConfig) { c.MaxLine = 100 })
	h.waitText("next\n")
	// A long line split across two reads is cut at the same length.
	appendFile(t, path, strings.Repeat("z", 60))
	h.waitText(strings.Repeat("z", 60))
	appendFile(t, path, strings.Repeat("w", 60)+"\nend\n")
	h.waitText("end\n")
	h.stop()
	want := "[" + logEventFileOpened + "]short\n" + strings.Repeat("x", 100) + "\n[" + logEventFileLineCut + "]next\n" +
		strings.Repeat("z", 60) + strings.Repeat("w", 40) + "\n[" + logEventFileLineCut + "]end\n"
	if got := h.flat(); got != want {
		t.Fatalf("stream = %q, want %q", got, want)
	}
}

func TestTailerBinaryStops(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "hello\n")
	h := startHarness(t, target, nil)
	h.waitText("hello\n")
	blob := make([]byte, 64<<10)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	appendFile(t, path, string(blob))
	h.waitDone()
	appendFile(t, path, "after\n")
	h.stop()
	got := h.flat()
	if !strings.HasPrefix(got, "["+logEventFileOpened+"]hello\n") || !strings.HasSuffix(got, "["+logEventFileBinary+"]") {
		t.Fatalf("stream = %q, want hello, then file_binary last", got)
	}
	if strings.Contains(got, "after") || len(got) > 1024 {
		t.Fatalf("stream = %d bytes %q, want tailing stopped at the blob", len(got), got)
	}
}

// TestTailerColourLogIsNotBinary: an ordinary UTF-8 log with ANSI colour codes
// is text, however long, and its ESC bytes are shown escaped.
func TestTailerColourLogIsNotBinary(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	line := "\x1b[32mINFO\x1b[0m größe=12 ✓ \x1b[1;31mwarn\x1b[0m\tdone\r\n"
	content := strings.Repeat(line, 200)
	mustWrite(t, path, content)
	h := startHarness(t, target, nil)
	escaped := strings.ReplaceAll(content, "\x1b", `\x1b`)
	h.waitFor("the whole colour log", func() bool { return h.text() == escaped })
	h.stop()
	if evs := h.events(); slices.Contains(evs, logEventFileBinary) {
		t.Fatalf("a colour log was flagged binary: %v", evs)
	}
}

// TestTailerEscapesControlAndInvalidBytes: invalid UTF-8 becomes U+FFFD and
// control bytes other than \t \n \r become \xNN, so nothing a terminal or a
// renderer would interpret reaches the ring.
func TestTailerEscapesControlAndInvalidBytes(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "a\x00b\x07c\x1b[31mred\x7f\t\r\n\xff\xfeé\n")
	h := startHarness(t, target, nil)
	want := `a\x00b\x07c\x1b[31mred\x7f` + "\t\r\n��é\n"
	h.waitText(want)
	h.stop()
	if got := h.text(); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

// TestTailerRuneSplitAcrossReadsStaysWhole: a multi-byte character whose
// bytes arrive in two reads is not replaced.
func TestTailerRuneSplitAcrossReadsStaysWhole(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	r := []byte("é")
	mustWrite(t, path, "x"+string(r[:1]))
	h := startHarness(t, target, nil)
	h.waitText("x")
	appendFile(t, path, string(r[1:])+"\n")
	h.waitText("é\n")
	h.stop()
	if got := h.text(); got != "xé\n" {
		t.Fatalf("text = %q, want %q", got, "xé\n")
	}
}

func TestTailerRateCapCountsDropped(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	var content bytes.Buffer
	for i := 0; content.Len() < 1<<20; i++ {
		fmt.Fprintf(&content, "line %07d\n", i)
	}
	data := content.Bytes()[:1<<20]
	mustWrite(t, path, string(data))
	const tick = 4096
	h := startHarness(t, target, func(c *tailerConfig) { c.TickBytes = tick })
	h.waitFor("the first tick", func() bool { return h.dropped() > 0 && len(h.text()) > 0 })
	h.stop()
	if got := h.text(); got != string(data[len(data)-tick:]) {
		t.Fatalf("text = %d bytes, want the newest %d bytes of the backlog", len(got), tick)
	}
	if got, want := h.dropped(), int64(len(data)-tick); got != want {
		t.Fatalf("dropped = %d, want %d", got, want)
	}
	// The gap sits where the bytes were skipped: before the text.
	recs := h.recs()
	gapAt, textAt := -1, -1
	for i, r := range recs {
		if r.dropped > 0 && gapAt < 0 {
			gapAt = i
		}
		if r.text != "" && textAt < 0 {
			textAt = i
		}
	}
	if gapAt < 0 || textAt < gapAt {
		t.Fatalf("records = %+v, want the gap at or before the first text", recs)
	}
}

// TestTailerRateCapIsPerTick: a writer that keeps the file growing past the
// cap is read at most TickBytes per poll; everything else is counted.
func TestTailerRateCapIsPerTick(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "")
	const tick = 1024
	var reads atomic.Int64
	h := startHarness(t, target, func(c *tailerConfig) {
		c.TickBytes = tick
		c.Open = func(tg logFileTarget) (*os.File, error) { reads.Add(1); return openLogFile(tg) }
	})
	h.waitEvent(logEventFileOpened)
	chunk := strings.Repeat("0123456789abcde\n", 512) // 8 KiB
	total := 0
	for range 8 {
		appendFile(t, path, chunk)
		total += len(chunk)
	}
	h.waitFor("all bytes accounted", func() bool { return int64(len(h.text()))+h.dropped() == int64(total) })
	h.stop()
	// Every poll probes Open once; text can be at most tick per poll.
	if polls := reads.Load(); int64(len(h.text())) > polls*tick {
		t.Fatalf("text = %d bytes over %d polls, above %d per poll", len(h.text()), polls, tick)
	}
	if h.dropped() == 0 {
		t.Fatal("no bytes counted as dropped")
	}
}

func TestTailerGraceCatchesFinalLines(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	mustWrite(t, path, "running\n")
	h := startHarness(t, target, func(c *tailerConfig) { c.Grace = time.Second })
	h.waitText("running\n")
	done := h.tailer.Stop()
	appendFile(t, path, "crash: final words\n")
	select {
	case <-done:
	case <-time.After(tailerWait):
		t.Fatal("tailer did not finish")
	}
	if !strings.Contains(h.text(), "crash: final words\n") {
		t.Fatalf("text = %q, want the line written during the grace", h.text())
	}
	// Stop is idempotent and returns the same, closed channel.
	select {
	case <-h.tailer.Stop():
	default:
		t.Fatal("a second Stop returned an open channel")
	}
}

func TestTailerMasksSecrets(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	m := maskerFor(t, Spec{Args: []string{"${AGENT_ENV:HF_TOKEN}"}}, map[string]string{"HF_TOKEN": "hf_abcdefgh123"})
	s := newTestLogStore(t, 1<<20, 0)
	p := s.newInstanceProcMasked(tailerTestSpec, 0, m)
	mustWrite(t, path, "token=hf_abcd")
	h := &tailerHarness{t: t, store: s}
	h.tailer = startFileTailer(testTailerConfig(target, p.ForStream(tailerTestStream)))
	t.Cleanup(func() { h.stop() })
	h.waitText("token=")
	assertRingClean(t, s, "hf_abcd")
	appendFile(t, path, "efgh123 end\n")
	h.waitText(" end\n")
	assertRingClean(t, s, "hf_abcd")
	if got, want := h.text(), "token=${AGENT_ENV:HF_TOKEN} end\n"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}

	// A possible secret start held back at Stop is flushed, not lost.
	appendFile(t, path, "tail hf_ab")
	h.stop()
	if got, want := h.text(), "token=${AGENT_ENV:HF_TOKEN} end\ntail hf_ab"; got != want {
		t.Fatalf("text after Stop = %q, want %q", got, want)
	}
}

// TestTailerStopIsIdempotent: Stop returns the same channel every time.
func TestTailerStopIsIdempotent(t *testing.T) {
	target, _, _ := newTailerTarget(t, "engine.log")
	h := startHarness(t, target, nil)
	a, b := h.tailer.Stop(), h.tailer.Stop()
	if a != b {
		t.Fatal("Stop returned different channels")
	}
	h.stop()
}

// TestTailerOpenErrorIsWaiting: an open error other than not-exist and
// not-permitted is retried quietly; before the first open it reads as waiting.
func TestTailerOpenErrorIsWaiting(t *testing.T) {
	target, path, _ := newTailerTarget(t, "engine.log")
	var fail atomic.Bool
	fail.Store(true)
	h := startHarness(t, target, func(c *tailerConfig) {
		c.Open = func(tg logFileTarget) (*os.File, error) {
			if fail.Load() {
				return nil, errors.New("runtime: open log file: permission denied")
			}
			return openLogFile(tg)
		}
	})
	h.waitEvent(logEventFileWaiting)
	mustWrite(t, path, "ok\n")
	fail.Store(false)
	h.waitText("ok\n")
	h.stop()
	if got, want := h.events(), []string{logEventFileWaiting, logEventFileOpened}; !slices.Equal(got, want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTailerConstants(t *testing.T) {
	if logFileTailInterval != 250*time.Millisecond || logFileTailGrace != 2*time.Second ||
		logFileTickBytes != 64<<10 || logFileMaxLine != 16<<10 {
		t.Fatalf("tailer constants = %v %v %d %d, want 250ms 2s 64KiB 16KiB",
			logFileTailInterval, logFileTailGrace, logFileTickBytes, logFileMaxLine)
	}
}

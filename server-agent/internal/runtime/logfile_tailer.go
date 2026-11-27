// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package runtime

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

// This file is the log-file tailer (#430, design §7): one goroutine per
// generation and declared file, which follows the file by polling and writes
// its text into the generation's file stream (procLog.ForStream). It never
// opens or stats the file by path: every open goes through the path policy's
// anchored open (openLogFile, §5.2), and everything else is asked of the
// handle that open returned.
//
// Each poll:
//
//  1. Probes the path with cfg.Open. This is how the tailer sees a rotation:
//     the probe's handle is compared with the held one by os.SameFile (inode
//     and device on Unix, file id and volume serial on Windows). Re-running
//     the whole policy on every poll, instead of a cheaper stat of the path,
//     is deliberate: a plain os.Stat(OpenPath) would re-resolve every parent
//     directory outside the os.Root anchor -- exactly what openLogFile exists
//     to avoid -- and a root-anchored stat would still need the full open
//     before a rotated file could be read. With the probe, the file a
//     rotation leaves behind the name has passed every check before a byte
//     of it is read, and a swap for an escaping link (Review Focus 1) is seen
//     on the very next poll. The cost is one root open, one file open and two
//     stats per file per 250 ms (at most four files per spec).
//  2. Reads the held handle: size below the offset is a truncation
//     (file_truncated, read from 0); otherwise the bytes past the offset, at
//     most TickBytes per poll.
//  3. If the probe found another file, the held handle has just been read to
//     its end, so it is closed and the new one is read from offset 0
//     (file_rotated).
//
// Open outcomes (the controller's rulings):
//
//   - not exist (also errLogFileVanished): before the first successful open,
//     file_waiting once, and keep polling; after one, a rotation in progress
//     -- the held handle is read on.
//   - not permitted: before the first successful open, file_not_permitted and
//     the tailer ends. After one, it is retried on the next polls, because a
//     rotation by link()+unlink() briefly gives the new file two links; only
//     logFileReopenRefusals consecutive refusals end the tailer. The policy
//     logs each refusal itself, so a refused name is opened at most that many
//     times: never once per poll for the rest of the generation.
//   - any other error: retried quietly, logged once per change of state; it
//     reads as waiting before the first open.
//
// Text: the bytes are made safe before they are written -- invalid UTF-8
// becomes U+FFFD and every control byte except \t \n \r is shown as \xNN
// (the ANSI ESC byte included, as \x1b) -- so nothing a terminal or a
// renderer would interpret reaches the ring; the portal renders text, never
// HTML. A character split across two reads is held back (at most three bytes)
// and completed by the next read. A line longer than MaxLine bytes is cut:
// its first MaxLine bytes and a newline are written, file_line_cut is
// recorded, and the rest up to and including the next \n is discarded. Masking
// (§8) happens in procLog.Write, so the tailer flushes the stream's masking
// carry (procLog.Flush) before every marker, before a gap and at the end:
// bytes on the two sides of a discontinuity can never form one value.
//
// Binary: see looksBinary.
//
// Rate: at most TickBytes are read per poll (64 KiB per 250 ms = 256 KiB/s).
// When more is waiting, the OLDER excess is skipped and counted with
// LogStore.gap, and the newest TickBytes are read: the skipped bytes are then
// reported exactly where they were ("missing immediately before" the text
// that follows), and the lines a crashing server wrote last are the ones that
// are kept. A rotated file's remainder and the new file share one poll's
// budget; a new file the budget could not reach at all waits for the next
// poll, where its backlog is capped the same way.
//
// Known limit: a truncation followed by a rewrite past the old offset within
// one poll is indistinguishable from an append (copytruncate's classic race);
// the text is then read from the old offset on.

// The tailer's production values (global constraints).
const (
	logFileTailInterval = 250 * time.Millisecond
	logFileTailGrace    = 2 * time.Second
	logFileTickBytes    = 64 << 10
	logFileMaxLine      = 16 << 10
	// logFileReopenRefusals is how many consecutive polls a reopen may be
	// refused before the tailer gives up with file_not_permitted.
	logFileReopenRefusals = 3
	// logFileReadChunk is the read buffer of one tailer.
	logFileReadChunk = 32 << 10
)

// The binary heuristic (looksBinary): judged over a window of at least
// binaryWindowBytes read bytes, binary when more than binaryPercent of them
// are suspicious.
const (
	binaryWindowBytes = 512
	binaryPercent     = 30
)

// tailerConfig is one tailer's input. Zero Interval, Grace, TickBytes, MaxLine
// and a nil Open take the production values; a nil Log records nothing.
type tailerConfig struct {
	Target     logFileTarget
	Log        *procLog      // the stream sibling (ForStream(Target.Name))
	StartAtEOF bool          // external path: true; ${CONFIG_DIR}: false
	Interval   time.Duration // 250 ms
	Grace      time.Duration // 2 s
	TickBytes  int           // 64 KiB = 256 KiB/s at 250 ms
	MaxLine    int           // 16 KiB
	Open       func(logFileTarget) (*os.File, error)
}

// fileTailer follows one declared file of one generation. Everything but the
// two channels and the once is owned by the tailer's goroutine (before it
// starts, by startFileTailer).
type fileTailer struct {
	cfg      tailerConfig
	stop     chan struct{}
	stopOnce sync.Once
	done     chan struct{}

	f      *os.File    // the held handle; nil before the first open
	fi     os.FileInfo // f's stat at open: its identity for os.SameFile
	off    int64       // the next byte of f to read
	buf    []byte      // the read buffer
	opened bool        // a file was opened at least once
	ended  bool        // a final status (binary, not permitted) was recorded
	waited bool        // file_waiting was recorded
	// refusals counts consecutive refused reopens; openErr is the other
	// open error last logged ("" when the last open was not one).
	refusals int
	openErr  string

	// The text pipeline (see the file comment).
	lineLen    int     // bytes of the current line written so far
	discarding bool    // a cut line: discard to the next \n
	tail       [4]byte // an incomplete character held back from the last read
	tailN      int
	out        []byte // sanitised text not yet written
	winTotal   int    // looksBinary's window
	winSuspect int
}

// startFileTailer opens the file once (so StartAtEOF means "as of the start")
// and starts the tailer's goroutine. A first open the policy refuses ends the
// tailer at once: Stop then returns a closed channel.
func startFileTailer(cfg tailerConfig) *fileTailer {
	if cfg.Interval <= 0 {
		cfg.Interval = logFileTailInterval
	}
	if cfg.Grace <= 0 {
		cfg.Grace = logFileTailGrace
	}
	if cfg.TickBytes <= 0 {
		cfg.TickBytes = logFileTickBytes
	}
	if cfg.MaxLine <= 0 {
		cfg.MaxLine = logFileMaxLine
	}
	if cfg.Open == nil {
		cfg.Open = openLogFile
	}
	if cfg.Log == nil {
		cfg.Log = &procLog{}
	}
	t := &fileTailer{cfg: cfg, stop: make(chan struct{}), done: make(chan struct{})}
	if f := t.probe(); f != nil {
		t.adopt(f, cfg.StartAtEOF)
	}
	if t.ended {
		t.finish()
		close(t.done)
		return t
	}
	go t.run()
	return t
}

// Stop starts the grace and returns a channel that is closed once the grace
// has elapsed, the last read is done and the stream was flushed. It is safe to
// call more than once and returns the same channel every time; the tailer's
// goroutine has exited when it is closed.
func (t *fileTailer) Stop() <-chan struct{} {
	t.stopOnce.Do(func() { close(t.stop) })
	return t.done
}

func (t *fileTailer) run() {
	defer close(t.done)
	ticker := time.NewTicker(t.cfg.Interval)
	defer ticker.Stop()
	stop := t.stop
	var grace <-chan time.Time
	var timer *time.Timer
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		t.poll()
		if t.ended {
			t.finish()
			return
		}
		select {
		case <-ticker.C:
		case <-stop:
			stop = nil
			timer = time.NewTimer(t.cfg.Grace)
			grace = timer.C
		case <-grace:
			t.poll()
			t.finish()
			return
		}
	}
}

// poll is one step of the loop (see the file comment).
func (t *fileTailer) poll() {
	nf := t.probe()
	if t.ended {
		return
	}
	if t.f == nil {
		if nf != nil {
			t.adopt(nf, false)
		}
		if t.f != nil {
			budget := t.cfg.TickBytes
			t.read(&budget)
		}
		return
	}
	if nf != nil && t.isHeld(nf) {
		_ = nf.Close()
		nf = nil
	}
	budget := t.cfg.TickBytes
	t.read(&budget)
	if t.ended {
		if nf != nil {
			_ = nf.Close()
		}
		return
	}
	if nf == nil {
		return
	}
	_ = t.f.Close()
	t.f = nil
	t.mark(logEventFileRotated)
	if !t.adopt(nf, false) {
		return
	}
	if budget > 0 {
		t.read(&budget)
	}
}

// probe runs cfg.Open and classifies its outcome (see the file comment). It
// returns the opened file, or nil.
func (t *fileTailer) probe() *os.File {
	f, err := t.cfg.Open(t.cfg.Target)
	switch {
	case err == nil:
		t.refusals, t.openErr = 0, ""
		return f
	case errors.Is(err, errLogFileNotPermitted):
		t.openErr = ""
		t.refusals++
		if !t.opened || t.refusals >= logFileReopenRefusals {
			t.end(logEventFileNotPermitted)
		}
	case errors.Is(err, fs.ErrNotExist):
		t.refusals, t.openErr = 0, ""
		t.waiting()
	default:
		t.refusals = 0
		if msg := err.Error(); msg != t.openErr {
			t.openErr = msg
			slog.Warn("runtime: log file cannot be opened; retrying", "spec", t.cfg.Log.specID, "log_file", t.cfg.Target.Name, "err", err)
		}
		t.waiting()
	}
	return nil
}

// waiting records file_waiting the first time the file is found absent before
// any open.
func (t *fileTailer) waiting() {
	if !t.opened && !t.waited {
		t.waited = true
		t.mark(logEventFileWaiting)
	}
}

// adopt makes f the held handle, from its end when atEOF. The first adopted
// file records file_opened. It reports false (and closes f) when f cannot be
// stat'ed: without its identity a rotation could not be told apart.
func (t *fileTailer) adopt(f *os.File, atEOF bool) bool {
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return false
	}
	t.f, t.fi, t.off = f, fi, 0
	if atEOF {
		t.off = fi.Size()
	}
	if !t.opened {
		t.opened = true
		t.mark(logEventFileOpened)
	}
	return true
}

// isHeld reports whether nf is the file already held.
func (t *fileTailer) isHeld(nf *os.File) bool {
	fi, err := nf.Stat()
	return err == nil && os.SameFile(t.fi, fi)
}

// read reads the held handle from the offset towards its end, at most
// *budget bytes, and charges what it read to *budget.
func (t *fileTailer) read(budget *int) {
	fi, err := t.f.Stat()
	if err != nil {
		return
	}
	size := fi.Size()
	if size < t.off {
		t.mark(logEventFileTruncated)
		t.off = 0
	}
	avail := size - t.off
	if avail <= 0 || *budget <= 0 {
		return
	}
	if limit := int64(*budget); avail > limit {
		t.boundary()
		t.cfg.Log.store.gap(t.cfg.Log, avail-limit)
		t.off += avail - limit
		avail = limit
	}
	if t.buf == nil {
		t.buf = make([]byte, logFileReadChunk)
	}
	for avail > 0 && !t.ended {
		n, err := t.f.ReadAt(t.buf[:min(avail, int64(len(t.buf)))], t.off)
		if n > 0 {
			t.off += int64(n)
			avail -= int64(n)
			*budget -= n
			t.feed(t.buf[:n])
		}
		if err != nil {
			break
		}
	}
	t.writeOut()
}

// feed runs one read through the binary check and the line cut, into t.out.
func (t *fileTailer) feed(b []byte) {
	if t.looksBinary(b) {
		t.end(logEventFileBinary)
		return
	}
	for len(b) > 0 {
		if t.discarding {
			i := indexNewline(b)
			if i < 0 {
				return
			}
			b = b[i+1:]
			t.discarding, t.lineLen = false, 0
			continue
		}
		seg := b
		nl := indexNewline(b)
		if nl >= 0 {
			seg = b[:nl+1]
		}
		content := len(seg)
		if nl >= 0 {
			content--
		}
		if t.lineLen+content > t.cfg.MaxLine {
			keep := t.cfg.MaxLine - t.lineLen
			t.sanitize(b[:keep])
			t.sanitize([]byte{'\n'})
			t.mark(logEventFileLineCut)
			t.discarding = true
			b = b[keep:]
			continue
		}
		t.sanitize(seg)
		t.lineLen += content
		if nl >= 0 {
			t.lineLen = 0
		}
		b = b[len(seg):]
	}
}

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}

// looksBinary is the binary heuristic. A byte is suspicious when it is a
// byte of invalid UTF-8, NUL, DEL, or a C0 control byte that text does not
// use: \t \n \r \f \v \b and ESC are text (ESC opens the ANSI colour and
// cursor sequences ordinary server logs are full of), so a coloured UTF-8 log
// scores zero. Suspicious and total bytes accumulate over reads until at least
// binaryWindowBytes were seen; the window is then judged -- more than
// binaryPercent suspicious is binary -- and restarted. A binary file scores
// far above the threshold (random bytes: about 60 %), text far below; the
// window keeps a few stray bytes in a short read from deciding, and a text
// file that turns into a blob later is still caught. When b completes a
// binary window none of b is written. Known limit: UTF-16 text (half NUL)
// reads as binary.
func (t *fileTailer) looksBinary(b []byte) bool {
	t.winTotal += len(b)
	t.winSuspect += suspiciousBytes(b)
	if t.winTotal < binaryWindowBytes {
		return false
	}
	binary := t.winSuspect*100 > t.winTotal*binaryPercent
	t.winTotal, t.winSuspect = 0, 0
	return binary
}

func suspiciousBytes(b []byte) int {
	n := 0
	for i := 0; i < len(b); {
		c := b[i]
		if c < utf8.RuneSelf {
			if (c < 0x20 && !isTextControl(c)) || c == 0x7f {
				n++
			}
			i++
			continue
		}
		if !utf8.FullRune(b[i:]) {
			break // a character the next read completes
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			n++
		}
		i += size
	}
	return n
}

func isTextControl(c byte) bool {
	switch c {
	case '\t', '\n', '\r', '\f', '\v', '\b', 0x1b:
		return true
	}
	return false
}

const hexDigits = "0123456789abcdef"

// sanitize appends b to t.out as safe text (see the file comment), holding
// back a trailing incomplete character.
func (t *fileTailer) sanitize(b []byte) {
	data := b
	if t.tailN > 0 {
		data = append(t.tail[:t.tailN:t.tailN], b...)
		t.tailN = 0
	}
	for i := 0; i < len(data); {
		c := data[i]
		if c < utf8.RuneSelf {
			if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
				t.out = append(t.out, '\\', 'x', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				t.out = append(t.out, c)
			}
			i++
			continue
		}
		if !utf8.FullRune(data[i:]) {
			t.tailN = copy(t.tail[:], data[i:])
			return
		}
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			t.out = utf8.AppendRune(t.out, utf8.RuneError)
		} else {
			t.out = append(t.out, data[i:i+size]...)
		}
		i += size
	}
}

// writeOut writes the sanitised text gathered so far.
func (t *fileTailer) writeOut() {
	if len(t.out) > 0 {
		_, _ = t.cfg.Log.Write(t.out)
		t.out = t.out[:0]
	}
}

// boundary ends the text before a discontinuity (a marker, a gap, the end): a
// held-back incomplete character becomes U+FFFD, the text is written, the
// stream's masking carry is flushed, and the line starts afresh.
func (t *fileTailer) boundary() {
	if t.tailN > 0 {
		t.tailN = 0
		t.out = utf8.AppendRune(t.out, utf8.RuneError)
	}
	t.writeOut()
	t.cfg.Log.Flush()
	t.lineLen, t.discarding = 0, false
}

// mark records ev on the stream after a boundary.
func (t *fileTailer) mark(ev string) {
	t.boundary()
	t.cfg.Log.Mark(ev)
}

// end records the final status ev; the loop then finishes.
func (t *fileTailer) end(ev string) {
	t.mark(ev)
	t.ended = true
}

// finish ends the tailer: the last text is written and flushed, a file that
// never appeared (and was not refused) is file_missing, and the handle is
// closed.
func (t *fileTailer) finish() {
	if !t.opened && !t.ended {
		t.mark(logEventFileMissing)
	}
	t.boundary()
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
}

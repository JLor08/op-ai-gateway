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
//     logFileReopenRefusals consecutive refusals end the tailer -- after the
//     held handle was read to its end, so its last lines are kept. The policy
//     logs each refusal itself, so a refused name is opened at most that many
//     times: never once per poll for the rest of the generation.
//   - any other error: retried quietly; it reads as waiting before the first
//     open. Each distinct error is logged once per tailer (at most
//     logFileOpenErrLogs of them), however often it comes and goes.
//
// Stop and the grace: Stop starts the grace (2 s, the crash case). The first
// poll after Stop is the LAST probe of the path; from then on the grace only
// drains the handle already held, to its end, and never opens or adopts
// another file. In a crash loop the next generation starts within this
// generation's grace and recreates a ${CONFIG_DIR} file under the same name
// (§5.3 removes the old name, this tailer's handle reads on); that file and
// its text belong to the next generation's own tailer, never to this one.
// EndGrace cuts the grace short with one final drain: the next generation's
// start calls it on the previous generation's tailer of the same name.
//
// Text: the bytes are made safe before they are written -- invalid UTF-8
// becomes U+FFFD and every control byte except \t \n \r is shown as \xNN
// (the ANSI ESC byte included, as \x1b) -- so nothing a terminal or a
// renderer would interpret reaches the ring; the portal renders text, never
// HTML. A character split across two reads is held back (at most three bytes)
// and completed by the next read. A line longer than MaxLine bytes is cut:
// its first MaxLine bytes are written, then a newline THE TAILER INSERTS (it
// is not in the file; it ends the cut line, so the marker and the next line
// stand on their own lines and a further instance's next line gets its
// prefix), then file_line_cut is recorded, and the rest of the line up to and
// including its \n in the file is discarded.
//
// Masking (§8) happens in procLog.Write, which holds back a possible secret
// start until the next bytes decide it. At a NATURAL boundary -- a rotation,
// a truncation, the end -- the tailer resolves that carry (procLog.Flush)
// before the marker: the stream really ends there. At a cut the tailer makes
// ITSELF in the middle of the text -- a line cut, a skip past the rate cap,
// the stop at a binary blob -- the bytes after the cut are not the
// continuation, and resolving the carry would write the first part of a
// secret straddling the cut in clear; there the carry is DISCARDED (procLog.DiscardCarry) and counted as dropped. After
// a skip, reading resumes at the next line start (the bytes up to and
// including the next \n are dropped and counted too), so the end of a secret
// the skip cut through is never written either.
//
// Binary: see looksBinary.
//
// Rate: at most TickBytes are read per poll (64 KiB per 250 ms = 256 KiB/s).
// When more is waiting, the OLDER excess is skipped and counted with
// LogStore.gap, and the newest TickBytes are read (from the first line start
// in them): the skipped bytes are then reported exactly where they were
// ("missing immediately before" the text that follows), and the lines a
// crashing server wrote last are the ones that are kept. A rotated file's remainder and the new file share one poll's
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
	// logFileOpenErrLogs bounds the distinct open errors one tailer logs.
	logFileOpenErrLogs = 4
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
// channels and the onces is owned by the tailer's goroutine (before it
// starts, by startFileTailer).
type fileTailer struct {
	cfg      tailerConfig
	stop     chan struct{}
	stopOnce sync.Once
	endGrace chan struct{} // EndGrace
	endOnce  sync.Once
	done     chan struct{}

	stopping   bool // Stop was seen: the next poll is the last probe
	probesDone bool // the grace: drain the held handle only
	giveUp     bool // the last reopen refusal: drain, then file_not_permitted

	f      *os.File    // the held handle; nil before the first open
	fi     os.FileInfo // f's stat at open: its identity for os.SameFile
	off    int64       // the next byte of f to read
	buf    []byte      // the read buffer
	opened bool        // a file was opened at least once
	ended  bool        // a final status (binary, not permitted) was recorded
	waited bool        // file_waiting was recorded
	// refusals counts consecutive refused reopens; loggedErrs are the other
	// open errors already logged.
	refusals   int
	loggedErrs map[string]bool

	// The text pipeline (see the file comment).
	lineLen    int     // bytes of the current line written so far
	discarding bool    // a cut line: discard to the next \n
	resync     bool    // after a skip: drop to the next \n (counted)
	pendingGap int64   // dropped bytes not yet recorded (LogStore.gap)
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
	t := &fileTailer{cfg: cfg, stop: make(chan struct{}), endGrace: make(chan struct{}), done: make(chan struct{})}
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

// EndGrace ends the tailer without waiting out the grace: one final drain of
// the held handle to its current end (capped like any poll), then the flush,
// the close, and the channel Stop returns is closed. It does not wait for
// that; Stop's channel does. Safe before or after Stop, and more than once.
// Called before Stop it ends the tailer just the same, without the last
// probe of the path.
func (t *fileTailer) EndGrace() {
	t.endOnce.Do(func() { close(t.endGrace) })
}

// finished reports whether the tailer has ended: the channel Stop returns is
// closed.
func (t *fileTailer) finished() bool {
	select {
	case <-t.done:
		return true
	default:
		return false
	}
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
			t.stopping = true
			timer = time.NewTimer(t.cfg.Grace)
			grace = timer.C
		case <-grace:
			t.drain()
			t.finish()
			return
		case <-t.endGrace:
			t.drain()
			t.finish()
			return
		}
	}
}

// poll is one step of the loop (see the file comment).
func (t *fileTailer) poll() {
	t.noteEndGraceBeforeProbe()
	if t.probesDone {
		t.drain()
		return
	}
	if t.stopping {
		t.probesDone = true // this is the last probe
	}
	nf := t.probe()
	if t.ended {
		return
	}
	if t.f == nil && nf != nil {
		t.adopt(nf, false)
		nf = nil
	}
	if nf != nil && t.isHeld(nf) {
		_ = nf.Close()
		nf = nil
	}
	budget := t.cfg.TickBytes
	if t.f != nil {
		t.read(&budget)
	}
	if t.endAfterRead(nf) {
		return
	}
	if nf == nil {
		return
	}
	t.rotate(nf, &budget)
}

// noteEndGraceBeforeProbe is poll's first step: once stopping, and while the
// post-Stop probe is still to be made, an EndGrace that has already come marks
// the probes done without making it.
func (t *fileTailer) noteEndGraceBeforeProbe() {
	if t.stopping && !t.probesDone {
		// EndGrace may have come before the Stop the loop saw first: the
		// grace is then over before it began, and the post-Stop probe is
		// not made (EndGrace's contract).
		select {
		case <-t.endGrace:
			t.probesDone = true
		default:
		}
	}
}

// endAfterRead is poll's step after the held handle was read: when the
// tailer has ended or given up, the newly opened file nf (if any) is closed,
// a give-up ends the tailer with file_not_permitted, and it reports true --
// the poll is over. Otherwise it changes nothing and reports false.
func (t *fileTailer) endAfterRead(nf *os.File) bool {
	if !t.ended && !t.giveUp {
		return false
	}
	if nf != nil {
		_ = nf.Close()
	}
	if !t.ended {
		t.end(logEventFileNotPermitted)
	}
	return true
}

// rotate is poll's last step: nf is a different file at the path than the
// held one, which has been read up to this poll's budget. The held handle is
// closed, file_rotated recorded, nf adopted from offset 0, and what is left
// of *budget read from it.
func (t *fileTailer) rotate(nf *os.File, budget *int) {
	_ = t.f.Close()
	t.f = nil
	t.mark(logEventFileRotated)
	if !t.adopt(nf, false) {
		return
	}
	if *budget > 0 {
		t.read(budget)
	}
}

// probe runs cfg.Open and classifies its outcome (see the file comment). It
// returns the opened file, or nil.
func (t *fileTailer) probe() *os.File {
	f, err := t.cfg.Open(t.cfg.Target)
	switch {
	case err == nil:
		t.refusals = 0
		return f
	case errors.Is(err, errLogFileNotPermitted):
		t.refusals++
		switch {
		case !t.opened:
			t.end(logEventFileNotPermitted)
		case t.refusals >= logFileReopenRefusals:
			t.giveUp = true // poll drains the held handle first
		}
	case errors.Is(err, fs.ErrNotExist):
		t.refusals = 0
		t.waiting()
	default:
		t.refusals = 0
		t.logOpenErr(err)
		t.waiting()
	}
	return nil
}

// logOpenErr logs an open error that is neither not-exist nor a refusal, once
// per distinct error and tailer, and at most logFileOpenErrLogs of them: an
// error that comes and goes (a lock taken and released) is logged once, not
// on every return.
func (t *fileTailer) logOpenErr(err error) {
	msg := err.Error()
	if t.loggedErrs[msg] || len(t.loggedErrs) >= logFileOpenErrLogs {
		return
	}
	if t.loggedErrs == nil {
		t.loggedErrs = map[string]bool{}
	}
	t.loggedErrs[msg] = true
	slog.Warn("runtime: log file cannot be opened; retrying", "spec", t.cfg.Log.specID, "log_file", t.cfg.Target.Name, "err", err)
}

// drain reads the held handle, if any, for one poll's budget.
func (t *fileTailer) drain() {
	if t.f != nil {
		budget := t.cfg.TickBytes
		t.read(&budget)
	}
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
		t.cut(avail - limit)
		t.off += avail - limit
		avail = limit
		t.resync = !t.atLineStart()
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

// cut is a cut the tailer makes itself in the middle of the text, skipping
// skipped bytes: the text so far is written, the incomplete character and
// the masking carry it ends in are discarded, and all of it is counted as
// dropped (recorded with the next text or at the next boundary). See the file
// comment for why the carry is discarded rather than flushed.
func (t *fileTailer) cut(skipped int64) {
	t.writeOut()
	t.pendingGap += skipped + int64(t.tailN) + int64(t.cfg.Log.DiscardCarry())
	t.tailN = 0
	t.lineLen, t.discarding = 0, false
}

// atLineStart reports whether t.off is the start of a line: offset 0, or the
// byte before it is a \n. An unreadable byte counts as mid-line.
func (t *fileTailer) atLineStart() bool {
	if t.off == 0 {
		return true
	}
	var b [1]byte
	n, _ := t.f.ReadAt(b[:], t.off-1)
	return n == 1 && b[0] == '\n'
}

// feed runs one read through the binary check and the line cut, into t.out.
func (t *fileTailer) feed(b []byte) {
	if t.looksBinary(b) {
		// A cut the tailer makes: the blob is not the continuation of a
		// possible secret start held in the carry, so the carry is dropped
		// and counted (file comment), never flushed by the end's boundary.
		t.cut(0)
		t.end(logEventFileBinary)
		return
	}
	for len(b) > 0 {
		switch {
		case t.resync:
			b = t.skipToNextLine(b)
		case t.discarding:
			b = t.discardRestOfLine(b)
		default:
			b = t.feedLine(b)
		}
	}
}

// skipToNextLine is feed's step after a skip (t.resync): everything up to and
// including the next \n is dropped and counted, and resync ends there. It
// returns the rest of b; without a \n all of b is dropped and counted, and
// resync continues into the next read.
func (t *fileTailer) skipToNextLine(b []byte) []byte {
	i := indexNewline(b)
	if i < 0 {
		t.pendingGap += int64(len(b))
		return b[len(b):]
	}
	t.pendingGap += int64(i + 1)
	t.resync = false
	return b[i+1:]
}

// discardRestOfLine is feed's step after a line cut (t.discarding): the rest
// of the cut line, up to and including its \n, is dropped (the line_cut
// marker already told the reader), and the next line starts afresh. It
// returns the rest of b; without a \n all of b is dropped and discarding
// continues into the next read.
func (t *fileTailer) discardRestOfLine(b []byte) []byte {
	i := indexNewline(b)
	if i < 0 {
		return b[len(b):]
	}
	t.discarding, t.lineLen = false, 0
	return b[i+1:]
}

// feedLine is feed's ordinary step: it sanitises b up to and including its
// first \n (all of b without one) and returns the rest. A line whose content
// would grow past cfg.MaxLine is cut there instead: its first part is
// written, the masking carry dropped and counted, a newline inserted,
// file_line_cut recorded, and the rest of the line discarded.
func (t *fileTailer) feedLine(b []byte) []byte {
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
		t.writeOut()
		// A cut the tailer makes: a secret straddling it must not be
		// resolved in clear (file comment), so the carry is dropped.
		t.pendingGap += int64(t.cfg.Log.DiscardCarry())
		t.sanitize([]byte{'\n'}) // the inserted newline (file comment)
		t.mark(logEventFileLineCut)
		t.discarding = true
		return b[keep:]
	}
	t.sanitize(seg)
	t.lineLen += content
	if nl >= 0 {
		t.lineLen = 0
	}
	return b[len(seg):]
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
			t.out = appendSafeASCII(t.out, c)
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

// appendSafeASCII appends the ASCII byte c to out as safe text: \t, \n, \r
// and every printable byte as they are, any other C0 control byte and DEL as
// a \xNN escape (lower-case hex).
func appendSafeASCII(out []byte, c byte) []byte {
	if (c < 0x20 && c != '\t' && c != '\n' && c != '\r') || c == 0x7f {
		return append(out, '\\', 'x', hexDigits[c>>4], hexDigits[c&0xf])
	}
	return append(out, c)
}

// writeOut writes the sanitised text gathered so far, after the dropped
// bytes it follows.
func (t *fileTailer) writeOut() {
	if len(t.out) > 0 {
		t.writeGap()
		_, _ = t.cfg.Log.Write(t.out)
		t.out = t.out[:0]
	}
}

// writeGap records the dropped bytes not yet recorded, at this position.
func (t *fileTailer) writeGap() {
	if t.pendingGap > 0 {
		t.cfg.Log.store.gap(t.cfg.Log, t.pendingGap)
		t.pendingGap = 0
	}
}

// boundary ends the text at a natural discontinuity (a marker, the end): a
// held-back incomplete character becomes U+FFFD, the text and any dropped
// count are written, the stream's masking carry is flushed (at a line cut it
// was already discarded, so there is nothing left to flush), and the line
// starts afresh.
func (t *fileTailer) boundary() {
	if t.tailN > 0 {
		t.tailN = 0
		t.out = utf8.AppendRune(t.out, utf8.RuneError)
	}
	t.writeOut()
	t.writeGap()
	t.cfg.Log.Flush()
	t.lineLen, t.discarding, t.resync = 0, false, false
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
// never appeared (and was not refused) is file_missing, the handle is
// closed and the read buffer released.
func (t *fileTailer) finish() {
	if !t.opened && !t.ended {
		t.mark(logEventFileMissing)
	}
	t.boundary()
	if t.f != nil {
		_ = t.f.Close()
		t.f = nil
	}
	// A finished tailer can stay parked on specState.graceTailers until the
	// next generation starts; it must not pin its read buffer meanwhile.
	t.buf = nil
}

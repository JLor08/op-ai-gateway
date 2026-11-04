// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"io"
	"sync"
)

// maxLiveReplayBytes bounds the PCM a bearer live session keeps for a replay
// (liveFeed). The refusals a replay answers -- an instance gone -- arrive as soon as
// the agent reads the request line, so a session keeps a few hundred milliseconds of
// audio at most; the bound only stops an attempt that never answers from growing it.
// 2 MiB is a minute of 16 kHz mono s16le, five seconds of 48 kHz stereo f32le.
const maxLiveReplayBytes = 2 << 20

// liveFeed carries a bearer live session's PCM from the client's request body into the
// request body of the session's current upstream attempt, a pipe per attempt. While
// the current attempt may still be re-selected (an instance k >= 1 that may turn out to
// be gone) it also keeps every byte it sent, so the next attempt can be sent the
// session's audio from its first byte: the transport may have written PCM into the
// refused attempt before its refusal arrived, and that audio is not lost.
//
// One goroutine writes (liveBodyCopyLoop: write, then end once); the relay switches
// attempts (restart), stops keeping (accept) and tears down (close). The pipe, its
// generation and what was kept change together under mu, so a chunk the copy loop
// wrote into an attempt that has since been replaced is known to travel in the replay.
type liveFeed struct {
	mu      sync.Mutex
	pr      *io.PipeReader
	pw      *io.PipeWriter
	gen     int
	keeping bool
	kept    []byte
	// ended is nil while the client streams, then io.EOF for a clean end of input or
	// errLiveClientGone for a body cut short; a later attempt's pipe is ended the same.
	ended error
}

// newLiveFeed returns a feed whose first attempt keeps its audio for a replay when
// replayable.
func newLiveFeed(replayable bool) *liveFeed {
	pr, pw := io.Pipe()
	return &liveFeed{pr: pr, pw: pw, keeping: replayable}
}

// body is the current attempt's upstream request body.
func (f *liveFeed) body() io.Reader {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pr
}

// write sends p to the current attempt, keeping it first while the attempt may be
// replayed; a session whose audio outgrows maxLiveReplayBytes stops keeping, and can
// no longer be replayed. ok is false once the feed is torn down. A write the relay
// interrupted by switching attempts is ok: p was kept, and travels in the replay.
func (f *liveFeed) write(p []byte) (ok bool) {
	f.mu.Lock()
	if f.keeping {
		if len(f.kept)+len(p) > maxLiveReplayBytes {
			f.keeping, f.kept = false, nil
		} else {
			f.kept = append(f.kept, p...)
		}
	}
	pw, gen := f.pw, f.gen
	f.mu.Unlock()
	if _, err := pw.Write(p); err != nil {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.gen != gen
	}
	return true
}

// end records how the client's body ended (io.EOF for a clean end, anything else for
// a truncated one) and ends the current attempt's body the same way: a clean close
// becomes the terminating chunk, a truncation aborts the body without one.
func (f *liveFeed) end(err error) {
	f.mu.Lock()
	f.ended = err
	pw := f.pw
	f.mu.Unlock()
	endLivePipe(pw, err)
}

func endLivePipe(pw *io.PipeWriter, err error) {
	if err == io.EOF {
		_ = pw.Close()
		return
	}
	_ = pw.CloseWithError(errLiveClientGone)
}

// restart begins the next attempt and returns its request body: everything the feed
// kept, then whatever the client sends from now on (ended as the client's body ended,
// if it already has). The abandoned attempt's body is torn down, which releases a write
// blocked in it. ok is false, and nothing changes, when the feed kept nothing it could
// replay. The new attempt is the last that can be replayed, so the feed stops keeping.
func (f *liveFeed) restart() (io.Reader, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.keeping {
		return nil, false
	}
	replay := f.kept
	f.keeping, f.kept = false, nil
	old := f.pr
	f.pr, f.pw = io.Pipe()
	f.gen++
	_ = old.CloseWithError(errLiveRelayDone)
	if f.ended != nil {
		endLivePipe(f.pw, f.ended)
	}
	return io.MultiReader(bytes.NewReader(replay), f.pr), true
}

// accept tells the feed the current attempt was accepted: nothing will be replayed, so
// what was kept is dropped and nothing more is kept.
func (f *liveFeed) accept() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keeping, f.kept = false, nil
}

// close tears the current attempt's body down from the relay's side, which unblocks
// both the transport's read of it and a copy-loop write blocked in it.
func (f *liveFeed) close() {
	f.mu.Lock()
	pr := f.pr
	f.mu.Unlock()
	_ = pr.CloseWithError(errLiveRelayDone)
}

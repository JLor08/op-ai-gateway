// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// readAllAsync drains r on a goroutine of its own.
func readAllAsync(r io.Reader) <-chan []byte {
	out := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(r)
		out <- b
	}()
	return out
}

// A restarted attempt is sent everything the abandoned one was, then the rest,
// each byte once, ended as the client's body ended.
func TestLiveFeedReplaysTheSessionIntoTheNextAttempt(t *testing.T) {
	f := newLiveFeed(true)
	first := readAllAsync(f.body())
	if !f.write([]byte("ab")) || !f.write([]byte("cd")) {
		t.Fatal("write to the first attempt failed")
	}
	body, ok := f.restart()
	if !ok {
		t.Fatal("restart = false, want a replay")
	}
	if got := <-first; string(got) != "abcd" {
		t.Fatalf("first attempt got %q, want abcd before it was torn down", got)
	}
	next := readAllAsync(body)
	if !f.write([]byte("ef")) {
		t.Fatal("write to the next attempt failed")
	}
	f.end(io.EOF)
	if got := <-next; string(got) != "abcdef" {
		t.Fatalf("next attempt got %q, want abcdef", got)
	}
	if _, ok := f.restart(); ok {
		t.Fatal("a second restart replayed, want the next attempt to be the last")
	}
}

// A client that ended before the restart ends the next attempt too: cleanly
// after a clean end, truncated after a truncated one.
func TestLiveFeedRestartAfterTheClientEnded(t *testing.T) {
	for _, end := range []error{io.EOF, errLiveClientGone} {
		f := newLiveFeed(true)
		go func() { _, _ = io.Copy(io.Discard, f.body()) }()
		f.write([]byte("pcm"))
		f.end(end)
		body, ok := f.restart()
		if !ok {
			t.Fatal("restart = false, want a replay")
		}
		got, err := io.ReadAll(body)
		if string(got) != "pcm" {
			t.Fatalf("end %v: next attempt got %q, want pcm", end, got)
		}
		if wantErr := end != io.EOF; (err != nil) != wantErr || (wantErr && !errors.Is(err, errLiveClientGone)) {
			t.Fatalf("end %v: next attempt ended with %v", end, err)
		}
	}
}

// A feed that may not be replayed, one accepted, and one whose audio outgrew
// the bound keep nothing and do not restart.
func TestLiveFeedKeepsOnlyWhatItMayReplay(t *testing.T) {
	notReplayable := newLiveFeed(false)
	if _, ok := notReplayable.restart(); ok {
		t.Fatal("a feed for an attempt that is never re-selected restarted")
	}
	accepted := newLiveFeed(true)
	accepted.accept()
	if _, ok := accepted.restart(); ok {
		t.Fatal("an accepted feed restarted")
	}
	big := newLiveFeed(true)
	go func() { _, _ = io.Copy(io.Discard, big.body()) }()
	big.write(bytes.Repeat([]byte{1}, maxLiveReplayBytes))
	big.write([]byte{2})
	if _, ok := big.restart(); ok {
		t.Fatal("a feed past maxLiveReplayBytes restarted")
	}
}

// A torn-down feed refuses writes, so the copy loop ends.
func TestLiveFeedCloseEndsTheCopy(t *testing.T) {
	f := newLiveFeed(false)
	f.close()
	if f.write([]byte("x")) {
		t.Fatal("write after close = ok, want the copy loop told to stop")
	}
}

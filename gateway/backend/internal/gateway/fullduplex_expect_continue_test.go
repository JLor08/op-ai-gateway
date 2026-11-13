// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestFullDuplexExpectContinueHeaderFlush is the toolchain canary for issue #387, reduced to
// net/http alone: it is the pattern the live-transcription relay depends on, without the relay.
// A full-duplex handler reads the request body on its own goroutine (the relay's
// liveBodyCopyLoop) while it writes and flushes the response header (relayBearerLiveSSE), and
// the client sent Expect: 100-continue (the voice/live loopback) and waits for the response
// before it sends more body.
//
// On Go 1.26.10 and older the header flush does not touch the request body's mutex, so the
// client gets the response at once. On Go 1.27 (golang/go commit f5c3217500dc) and on
// go1.26.11, which backported it, chunkWriter.writeHeader calls
// body.bodyRemains(), which waits for the mutex the parked body read holds, and the response
// never arrives while the client waits: this test then fails after its deadline. That is why
// toolchain_guard_go127_test.go keeps this package from compiling on Go 1.27. Removing those
// guards for a fixed Go release must leave this test green.
func TestFullDuplexExpectContinueHeaderFlush(t *testing.T) {
	firstRead := make(chan struct{})
	bodyDone := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			t.Errorf("EnableFullDuplex: %v", err)
			return
		}
		go func() {
			defer close(bodyDone)
			br := bufio.NewReader(r.Body)
			if _, err := br.ReadByte(); err != nil {
				return
			}
			close(firstRead)
			// Keep reading: this read parks on the wire while the client waits for the response.
			_, _ = io.Copy(io.Discard, br)
		}()
		select {
		case <-firstRead:
		case <-r.Context().Done():
			return
		}
		// Give the body reader time to park in its next read before the header goes out.
		time.Sleep(50 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: first\n\n")
		http.NewResponseController(w).Flush()
		<-bodyDone
	}))
	defer ts.Close()

	pr, pw := io.Pipe()
	// Release the parked body read on any exit, so a deadlocked server goroutine can finish
	// and ts.Close does not hang.
	defer pw.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL, pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Expect", "100-continue")

	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		done <- result{resp, err}
	}()

	// Send one chunk once the server asked for the body, then wait for the response: the
	// client's write blocks until the transport, after the 100 Continue, reads the pipe.
	if _, err := pw.Write([]byte("pcm")); err != nil {
		t.Fatalf("write first body chunk: %v", err)
	}

	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("request: %v", res.err)
		}
		defer res.resp.Body.Close()
		if res.resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", res.resp.StatusCode)
		}
		line, err := bufio.NewReader(res.resp.Body).ReadString('\n')
		if err != nil || line != "data: first\n" {
			t.Fatalf("first SSE line = %q, %v; want %q", line, err, "data: first\n")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the full-duplex response header did not reach a client that sent Expect: 100-continue " +
			"while the server's body read was parked: the net/http regression of issue #387 (Go 1.27, go1.26.11+)")
	}
}

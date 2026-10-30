// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These tests pin the Expect: 100-continue ordering of the bearer live-transcription relay
// (issue #333). The voice loopback POSTs with Expect: 100-continue and holds its streaming PCM
// body until it sees a 100 Continue or a final response. Go's HTTP server sends that 100
// Continue by itself on the FIRST read of the request body, which is the relay's body-copy
// goroutine's first read -- scheduled independently of the upstream dial. If the relay writes
// the final SSE 200 before that read, a Go client receives the final response ahead of 100
// Continue and ABORTS its body: the PCM is never sent, the relay loud-zeroes (pcm_bytes=0) and
// the voice turn fails with stream_error.
//
// The tests hold the copy goroutine back through the package-level liveBodyCopyStartHook seam,
// so none of them may run in parallel.

// holdLiveBodyCopy delays every body-copy goroutine's first read by d for the test's lifetime:
// the scheduling under CPU contention in which the upstream's accept (and so the SSE 200) wins
// the race against the first body read. Call it BEFORE building the server under test:
// cleanups run last-in first-out, so the hook is reset only after the server has shut down and
// joined every copy goroutine.
func holdLiveBodyCopy(t *testing.T, d time.Duration) {
	t.Helper()
	liveBodyCopyStartHook = func() { time.Sleep(d) }
	t.Cleanup(func() { liveBodyCopyStartHook = nil })
}

// A voice turn whose loopback body-copy goroutine reads LATE (the upstream has already
// accepted) must still deliver its PCM: the relay may not send the SSE 200 ahead of 100
// Continue, or the loopback client aborts its body and the turn dies with stream_error. The
// turn here must succeed outright -- the child receives the PCM, answers a partial, and returns
// the clean final after push_end -- with no error frame anywhere on the way.
func TestBearerLiveExpect100DelayedFirstReadStillDeliversVoicePCM(t *testing.T) {
	holdLiveBodyCopy(t, 250*time.Millisecond)

	child := newFakeVoiceASRChild(t) // no acceptGate: the child accepts the instant it is dialled
	child.finals = []string{"late first read"}
	h := newVoiceTurnHarness(t, child, voiceTurnOpts{timing: turnGraceTiming()})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dial(t, ctx)
	readVoiceUntilState(t, ctx, conn, voiceStateListening)

	// The PCM must reach the child (a partial is the child's per-chunk answer), and an error
	// frame on the way fails the test: stream_error is exactly the aborted-body symptom.
	sendVoicePCM(t, ctx, conn, 320)
	readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRPartial)

	sendVoiceControl(t, ctx, conn, "push_end")
	final := readVoiceUntilFailingOnError(t, ctx, conn, voiceDownASRFinal)
	if final.Text != "late first read" {
		t.Fatalf("asr_final text = %q, want the child's clean final %q", final.Text, "late first read")
	}

	if conns, bytes := child.snapshot(); conns != 1 || bytes != 320 {
		t.Fatalf("child saw conns=%d bytes=%d, want 1 connection that received the 320 PCM bytes", conns, bytes)
	}
	if got := child.cleanEnds(); got != 1 {
		t.Fatalf("child clean end-of-input count = %d, want 1 (the body must end with the terminating chunk, not an abort)", got)
	}
}

// readResponseStatuses reads response heads off a raw connection up to and including the first
// FINAL (non-1xx) one, returning every status code in arrival order, informational ones too.
func readResponseStatuses(t *testing.T, br *bufio.Reader) []int {
	t.Helper()
	var codes []int
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading a response status line (statuses so far %v): %v", codes, err)
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/1.") {
			t.Fatalf("not a status line: %q", line)
		}
		code, err := strconv.Atoi(fields[1])
		if err != nil {
			t.Fatalf("status line %q: %v", line, err)
		}
		codes = append(codes, code)
		for { // the header block, up to the blank line
			h, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("reading response headers (statuses so far %v): %v", codes, err)
			}
			if strings.TrimRight(h, "\r\n") == "" {
				break
			}
		}
		if code >= 200 {
			return codes
		}
	}
}

// requireLiveAccessLogStatus waits for the access-log line of the live request and asserts the
// status it recorded. The line is written when the handler returns, after the client has seen
// the end of the stream, hence the poll.
func requireLiveAccessLogStatus(t *testing.T, logs *logbuffer.Buffer, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range logs.Snapshot() {
			if r.Msg == "http request" && r.Attrs["path"] == "/v1/audio/transcriptions/live" {
				if got := fmt.Sprint(r.Attrs["status"]); got != strconv.Itoa(want) {
					t.Fatalf("access log status = %s, want %d (the informational 100 Continue must not be recorded as the response status)", got, want)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("no access-log line for the live request")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The wire order, observed without a Go client in the way (whose abort is only the
// consequence): a client that sent Expect: 100-continue must see a 100 Continue BEFORE the
// final 200, even though the copy goroutine's first body read -- what makes Go's server send
// its own 100 Continue -- is held back until after the upstream has accepted. A client that
// did NOT send Expect has no 100 Continue to be ordered and must get none: its 200 is the only
// response, and it arrives with no PCM sent yet (such a client, like audio.cpp's own /live
// clients, may wait for the response headers before it streams a byte). Either way the access
// log records the 200 as the response status: the 100 Continue is informational.
func TestBearerLiveExpect100SendsContinueBeforeTheFinalResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expect bool
	}{
		{"with Expect: 100-continue", true},
		{"without Expect", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holdLiveBodyCopy(t, 250*time.Millisecond)
			logs, restoreLogs := withCapturedSlog(t)
			defer restoreLogs()
			child := newMockLiveChild(t)
			srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
			ts := httptest.NewServer(srv)
			defer ts.Close()

			conn, err := net.Dial("tcp", ts.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

			head := "POST /v1/audio/transcriptions/live?" + bearerLiveQuery("asr-model") + " HTTP/1.1\r\n" +
				"Host: gateway.test\r\nAuthorization: Bearer dev-secret\r\n" +
				"Content-Type: application/octet-stream\r\nTransfer-Encoding: chunked\r\n"
			if tc.expect {
				head += "Expect: 100-continue\r\n"
			}
			if _, err := io.WriteString(conn, head+"\r\n"); err != nil {
				t.Fatalf("write request head: %v", err)
			}
			br := bufio.NewReader(conn)

			codes := readResponseStatuses(t, br)
			if tc.expect {
				// 100 Continue first, then the 200; a repeated 100 in between is legal (the
				// first body read may add the server's own after ours), a 200 ahead of the
				// first 100 is the bug.
				if len(codes) < 2 || codes[0] != http.StatusContinue || codes[len(codes)-1] != http.StatusOK {
					t.Fatalf("response statuses = %v, want 100 Continue before the final 200", codes)
				}
				for _, c := range codes[:len(codes)-1] {
					if c != http.StatusContinue {
						t.Fatalf("response statuses = %v, want only 100 Continue ahead of the 200", codes)
					}
				}
			} else if len(codes) != 1 || codes[0] != http.StatusOK {
				t.Fatalf("response statuses = %v, want just the 200 (an unsolicited 100 Continue was sent)", codes)
			}

			// The session must then run to its clean end: PCM up as one chunk, the terminating
			// chunk, and the upstream's done event down.
			pcm := string(make([]byte, 320))
			if _, err := io.WriteString(conn, strconv.FormatInt(int64(len(pcm)), 16)+"\r\n"+pcm+"\r\n0\r\n\r\n"); err != nil {
				t.Fatalf("write pcm body: %v", err)
			}
			for sawDone := false; !sawDone; {
				line, err := br.ReadString('\n')
				if err != nil {
					t.Fatalf("the session never delivered transcript.text.done: %v", err)
				}
				sawDone = strings.Contains(line, "transcript.text.done")
			}
			_ = conn.Close()
			requireLiveAccessLogStatus(t, logs, http.StatusOK)
		})
	}
}

// postExpect100 opens a chunked POST to the bearer live endpoint WITH Expect: 100-continue, the
// way the voice loopback does, on a body the caller drives through the returned pipe. Do returns
// once the relay's final response arrives.
func postExpect100(ctx context.Context, ts *httptest.Server) (*io.PipeWriter, <-chan expect100Result) {
	pr, pw := io.Pipe()
	out := make(chan expect100Result, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/audio/transcriptions/live?"+bearerLiveQuery("asr-model"), pr)
		if err != nil {
			out <- expect100Result{err: err}
			return
		}
		req.Header.Set("Authorization", "Bearer dev-secret")
		req.Header.Set("Expect", "100-continue")
		resp, err := ts.Client().Do(req)
		out <- expect100Result{resp: resp, err: err}
	}()
	return pw, out
}

type expect100Result struct {
	resp *http.Response
	err  error
}

// An Expect: 100-continue turn whose request body ends without a single PCM byte must still
// complete: the handshake finishes, the upstream sees a clean end-of-input and answers done +
// [DONE], the per-route slot is freed and one zero-quantity row is billed -- nothing hangs. The
// copy goroutine's first read is held back, and the body is closed both at once (the transport
// sends it as a zero-length request) and after a pause (it is sent chunked, so the relay reads
// a terminating chunk only).
func TestBearerLiveExpect100EmptyBodyCompletesWithoutHanging(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeAfter time.Duration
	}{
		{"closed at once", 0},
		{"closed after the headers are sent", 350 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holdLiveBodyCopy(t, 100*time.Millisecond)
			child := newMockLiveChild(t)
			srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
			ts := httptest.NewServer(srv)
			defer ts.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pw, out := postExpect100(ctx, ts)
			if tc.closeAfter > 0 {
				time.Sleep(tc.closeAfter)
			}
			_ = pw.Close() // an empty body: the first read the relay issues is the EOF

			res := <-out
			if res.err != nil {
				t.Fatalf("an empty-bodied Expect: 100-continue session never completed its handshake: %v", res.err)
			}
			defer res.resp.Body.Close()
			if res.resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", res.resp.StatusCode)
			}
			// The upstream saw a clean end-of-input with no PCM, so it ends its SSE with done +
			// [DONE]; the stream therefore reaches EOF instead of hanging.
			br := bufio.NewReader(res.resp.Body)
			sawDone := false
			for {
				line, err := br.ReadString('\n')
				if strings.Contains(line, "transcript.text.done") {
					sawDone = true
				}
				if err != nil {
					break
				}
			}
			if !sawDone {
				t.Fatal("the empty session never received transcript.text.done: the body's EOF did not reach the upstream as a clean end")
			}
			requireOneBearerLiveRow(t, srv, usage.BillingUnitAudioSecond, 0)
			deadline := time.Now().Add(5 * time.Second)
			for {
				if release, ok := srv.liveSessions.acquire("route-asr"); ok {
					release()
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the per-route live slot was never released after the empty session ended")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

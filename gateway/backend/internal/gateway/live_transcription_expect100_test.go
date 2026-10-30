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
	"net/http/httptrace"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/usage"
	"strconv"
	"strings"
	"sync/atomic"
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

// expect100Call is one in-flight Expect: 100-continue POST made by startExpect100.
type expect100Call struct {
	done chan struct{} // closed once Do has returned
	resp *http.Response
	err  error
	// waited records that the client genuinely HELD its body for the handshake (httptrace's
	// Wait100Continue), got100 that a 100 Continue then reached it. Without them an Expect
	// header the client never honours would leave the handshake untested.
	waited, got100 atomic.Bool
}

// startExpect100 opens a chunked POST to the bearer live endpoint WITH Expect: 100-continue, the
// way the voice loopback does, on a body the caller drives through the returned pipe. The client
// has a real ExpectContinueTimeout (a zero one, httptest's default Transport, sends the body at
// once and never waits for a 100), so it holds the body until the relay sends a 100 Continue or
// a final response. Do returns once the relay's final response arrives.
func startExpect100(ctx context.Context, t *testing.T, ts *httptest.Server) (*io.PipeWriter, *expect100Call) {
	t.Helper()
	pr, pw := io.Pipe()
	call := &expect100Call{done: make(chan struct{})}
	tr := &http.Transport{ExpectContinueTimeout: 5 * time.Second}
	t.Cleanup(tr.CloseIdleConnections)
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		Wait100Continue: func() { call.waited.Store(true) },
		Got100Continue:  func() { call.got100.Store(true) },
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/audio/transcriptions/live?"+bearerLiveQuery("asr-model"), pr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Expect", "100-continue")
	go func() {
		defer close(call.done)
		call.resp, call.err = (&http.Client{Transport: tr}).Do(req)
	}()
	return pw, call
}

// An Expect: 100-continue turn whose request body ends without a single PCM byte must still
// complete: the client holds its (empty) body for the handshake, the relay's 100 Continue
// releases it, the upstream sees a clean end-of-input and answers done + [DONE], the per-route
// slot is freed and one zero-quantity row is billed -- nothing hangs. The copy goroutine's first
// read is held back, so it is the relay's own 100 Continue that has to release the client (an
// aborted body would end the session without a done event). A POST body from an io.Pipe is
// always sent chunked, so the relay reads only the terminating chunk; the pipe is closed either
// as the request starts or after the client has already been waiting for the 100 Continue.
func TestBearerLiveExpect100EmptyBodyCompletesWithoutHanging(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeAfter time.Duration
	}{
		{"body closed as the request starts", 0},
		{"body closed while the client waits", 350 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			holdLiveBodyCopy(t, 100*time.Millisecond)
			child := newMockLiveChild(t)
			srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
			ts := httptest.NewServer(srv)
			defer ts.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			pw, call := startExpect100(ctx, t, ts)
			if tc.closeAfter > 0 {
				time.Sleep(tc.closeAfter)
			}
			_ = pw.Close() // an empty body: the first read the relay issues is the EOF

			<-call.done
			if call.err != nil {
				t.Fatalf("the empty Expect: 100-continue session failed before its 200: %v", call.err)
			}
			defer call.resp.Body.Close()
			if !call.waited.Load() || !call.got100.Load() {
				t.Fatalf("client waited for 100 Continue = %v, received it = %v; both must hold or the handshake was not exercised", call.waited.Load(), call.got100.Load())
			}
			if call.resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", call.resp.StatusCode)
			}
			// The upstream saw a clean end-of-input with no PCM, so it ends its SSE with done +
			// [DONE]; the stream therefore reaches EOF instead of hanging.
			br := bufio.NewReader(call.resp.Body)
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

// requestExpectsContinue must say yes exactly where net/http's server would send its own
// automatic 100 Continue (the relay owes the client one ahead of the 200 only then, and must not
// invent one elsewhere). The unit cases pin the edges of that condition, the differential test
// below checks them against the real server.
func TestBearerLiveExpect100RequestExpectsContinue(t *testing.T) {
	for _, tc := range []struct {
		name          string
		expect        []string // the Expect header lines, in order
		contentLength int64    // -1 = chunked
		proto         string   // "" = HTTP/1.1
		want          bool
	}{
		{"plain token, chunked", []string{"100-continue"}, -1, "", true},
		{"mixed case", []string{"100-Continue"}, -1, "", true},
		{"upper case", []string{"100-CONTINUE"}, -1, "", true},
		{"declared length", []string{"100-continue"}, 320, "", true},
		{"comma list, token last", []string{"foo, 100-continue"}, -1, "", true},
		{"comma list, token first", []string{"100-continue,foo"}, -1, "", true},
		{"space separated", []string{"100-continue foo"}, -1, "", true},
		{"space separated, token last", []string{"foo 100-continue"}, -1, "", true},
		{"tab separated", []string{"100-continue\tfoo"}, -1, "", true},
		{"no Expect header", nil, -1, "", false},
		{"other expectation", []string{"foo"}, -1, "", false},
		{"token is a prefix of a longer word", []string{"100-continued"}, -1, "", false},
		{"token is a suffix of a longer word", []string{"x100-continue"}, -1, "", false},
		{"declared empty body (Content-Length: 0)", []string{"100-continue"}, 0, "", false},
		{"HTTP/1.0", []string{"100-continue"}, 320, "HTTP/1.0", false},
		{"HTTP/2", []string{"100-continue"}, -1, "HTTP/2.0", true},
		{"net/http reads only the first Expect line (token first)", []string{"100-continue", "foo"}, -1, "", true},
		{"net/http reads only the first Expect line (token second)", []string{"foo", "100-continue"}, -1, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions/live", nil)
			r.ContentLength = tc.contentLength
			switch tc.proto {
			case "HTTP/1.0":
				r.Proto, r.ProtoMajor, r.ProtoMinor = tc.proto, 1, 0
			case "HTTP/2.0":
				r.Proto, r.ProtoMajor, r.ProtoMinor = tc.proto, 2, 0
			}
			for _, v := range tc.expect {
				r.Header.Add("Expect", v)
			}
			if got := requestExpectsContinue(r); got != tc.want {
				t.Fatalf("requestExpectsContinue = %v, want %v", got, tc.want)
			}
		})
	}
}

// The same condition against net/http itself: a plain handler asks requestExpectsContinue and
// then reads the body, and a raw client that has already sent the whole request (as a client does
// once its ExpectContinueTimeout runs out) sees whether the server answered the first read with
// its own 100 Continue. The two must agree for every shape, so the relay's explicit 100 Continue
// is owed exactly where the server would have sent one. (Shapes net/http refuses with 417 before
// any handler runs are not reachable here and are covered by the unit cases.)
func TestBearerLiveExpect100RequestExpectsContinueMatchesNetHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		proto  string
		expect string
		body   string // the framing headers and the body, verbatim
	}{
		{"chunked", "HTTP/1.1", "100-continue", "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"chunked, mixed case", "HTTP/1.1", "100-Continue", "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"comma list", "HTTP/1.1", "foo, 100-continue", "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"space separated", "HTTP/1.1", "100-continue foo", "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"tab separated", "HTTP/1.1", "100-continue\tfoo", "Transfer-Encoding: chunked\r\n\r\n0\r\n\r\n"},
		{"declared length", "HTTP/1.1", "100-continue", "Content-Length: 5\r\n\r\nhello"},
		{"declared empty body", "HTTP/1.1", "100-continue", "Content-Length: 0\r\n\r\n"},
		{"HTTP/1.0", "HTTP/1.0", "100-continue", "Content-Length: 5\r\n\r\nhello"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got.Store(requestExpectsContinue(r))
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			conn, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			req := "POST / " + tc.proto + "\r\nHost: gateway.test\r\nExpect: " + tc.expect + "\r\n" + tc.body
			if _, err := io.WriteString(conn, req); err != nil {
				t.Fatalf("write request: %v", err)
			}
			codes := readResponseStatuses(t, bufio.NewReader(conn))
			serverSent100 := len(codes) > 0 && codes[0] == http.StatusContinue
			if serverSent100 != got.Load() {
				t.Fatalf("net/http sent an automatic 100 Continue = %v (statuses %v) but requestExpectsContinue = %v", serverSent100, codes, got.Load())
			}
		})
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The portal's live-transcription WebSocket (GET /api/portal/transcription-live) is a
// thin adapter: after the ticket is redeemed it loops the session back to the
// gateway's OWN /v1/audio/transcriptions/live, so the portal mic exercises the very
// endpoint a programmatic bearer client hits. These tests stand up the whole chain --
// browser WebSocket -> in-process loopback -> /v1/audio/transcriptions/live -> a mock
// full-duplex /live child -- and assert that resolution, admission, the per-route
// guard and the SINGLE usage row all happen at /v1 (as the ticket's loopback user),
// never a second time at the portal.

// liveWSProtocol is the fixed subprotocol the browser offers first; the ticket is
// the second offered value. Must match the handler's constant.
const liveWSProtocol = "op-live-transcription"

// mockLiveChild is a full-duplex /live echo child: it reads the chunked PCM request
// body and, for each chunk, emits a transcript.text.delta carrying the chunk's byte
// count, so a test can prove deltas arrive live (before the next chunk). When the
// body ends with a clean io.EOF (the terminating chunk the gateway sends on
// end-of-input) it emits transcript.text.done + [DONE]; a body cut WITHOUT that
// terminator (a non-EOF read error) ends silently -- the truncated contract.
type mockLiveChild struct {
	server *httptest.Server

	mu       sync.Mutex
	gotPath  string
	gotQuery string
	gotBytes int
	// emitPartial, when set, makes the child emit a transcript.text.partial before
	// the done event, to exercise the partial relay.
	emitPartial bool
	// emitError, when set, makes the child emit an SSE error event instead of
	// reading the body (a mid-stream failure).
	emitError bool
	// statusCode, when non-zero and not 2xx, makes the child answer that HTTP status
	// with a short error body instead of upgrading to the SSE stream (an upstream
	// error, e.g. the agent's 404 model_not_managed / 503 admission_blocked).
	statusCode int
}

func newMockLiveChild(t *testing.T) *mockLiveChild {
	t.Helper()
	c := &mockLiveChild{}
	c.server = httptest.NewServer(http.HandlerFunc(c.serve))
	t.Cleanup(c.server.Close)
	return c
}

func (c *mockLiveChild) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.gotPath = r.URL.Path
	c.gotQuery = r.URL.RawQuery
	emitPartial := c.emitPartial
	emitError := c.emitError
	statusCode := c.statusCode
	c.mu.Unlock()

	rc := http.NewResponseController(w)
	// Full-duplex so the server may write the response while the request body is still
	// incoming -- what a real audio.cpp /live upstream does, and what lets a non-2xx
	// answer (below) reach the client without the server first draining an open body.
	_ = rc.EnableFullDuplex()

	// A non-2xx upstream answer: report the status with a short error body and do NOT
	// upgrade to the SSE stream. Flush so the client's full-duplex request (whose body
	// is still open) receives the response headers promptly.
	if statusCode != 0 && (statusCode < 200 || statusCode >= 300) {
		w.WriteHeader(statusCode)
		fmt.Fprint(w, `{"error":{"message":"model not managed"}}`)
		_ = rc.Flush()
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	if emitError {
		fmt.Fprint(w, "data: {\"type\":\"error\",\"error\":{\"message\":\"backend blew up\"}}\n\n")
		_ = rc.Flush()
		return
	}

	buf := make([]byte, 4096)
	total := 0
	var readErr error
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			total += n
			fmt.Fprintf(w, "data: {\"type\":\"transcript.text.delta\",\"delta\":\"+%d\"}\n\n", n)
			_ = rc.Flush()
		}
		if err != nil {
			readErr = err
			break
		}
	}
	c.mu.Lock()
	c.gotBytes = total
	c.mu.Unlock()

	// A clean terminating chunk reads as io.EOF; an aborted body reads as a
	// non-EOF error and emits no done event (truncated).
	if readErr != io.EOF {
		return
	}
	if emitPartial {
		fmt.Fprint(w, "data: {\"type\":\"transcript.text.partial\",\"text\":\"provisional\"}\n\n")
		_ = rc.Flush()
	}
	fmt.Fprintf(w, "data: {\"type\":\"transcript.text.done\",\"text\":\"final-%d\"}\n\n", total)
	fmt.Fprint(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

func (c *mockLiveChild) query() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gotQuery
}

func (c *mockLiveChild) path() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gotPath
}

// newLiveLoopbackTestServer builds the full loopback chain's gateway: the
// transcription-capable routing fixtures (asr-model -> route-asr -> whisper_x,
// streaming yes; asr-offline, streaming no), the dev user usr_dev, the internal
// trusted-loopback secret and the live ticket store -- all pointed at the mock /live
// child. selfBaseURL is wired to the returned httptest server so the portal
// WebSocket's loopback is a real in-process HTTP call to the gateway's own address.
func newLiveLoopbackTestServer(t *testing.T, child *mockLiveChild) (*Server, *httptest.Server) {
	t.Helper()
	srv, _ := newTranscriptionCapableTestServer(t, child.server.URL)
	srv.SetBaseContext(context.Background())
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	srv.selfBaseURL = ts.URL
	return srv, ts
}

// mintLiveModelTicket mints a ticket the portal WebSocket will loop back with: the
// resolved model NAME (the loopback re-resolves it at /v1), the PCM params, and the
// user whose trusted-loopback identity the loopback replays as. Unlike the old direct
// relay, the ticket carries no live upstream target -- /v1 resolves it fresh.
func mintLiveModelTicket(t *testing.T, srv *Server, model string, sampleRate, channels int, format, language string) string {
	t.Helper()
	ticket, err := srv.liveTickets.Mint(liveTicketClaims{
		Target:     routing.Target{Model: model},
		SampleRate: sampleRate,
		Channels:   channels,
		Format:     format,
		Language:   language,
		UserID:     "usr_dev",
	})
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	return ticket
}

func dialLive(t *testing.T, ctx context.Context, httpURL, ticket string, extra http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	subs := []string{liveWSProtocol}
	if ticket != "" {
		subs = append(subs, ticket)
	}
	return websocket.Dial(ctx, wsURL(httpURL, "/api/portal/transcription-live"), &websocket.DialOptions{
		Subprotocols: subs,
		HTTPHeader:   extra,
	})
}

func readLiveMsg(t *testing.T, ctx context.Context, conn *websocket.Conn) (liveDownMsg, error) {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		return liveDownMsg{}, err
	}
	if typ != websocket.MessageText {
		t.Fatalf("expected a text down-message, got %v", typ)
	}
	var m liveDownMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode down-message %q: %v", data, err)
	}
	return m, nil
}

// readErrorFrame reads down-frames until the first error frame and returns it, or reports
// false when the socket ended without one.
func readErrorFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) (liveDownMsg, bool) {
	t.Helper()
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil {
			return liveDownMsg{}, false
		}
		if m.Type == "error" {
			return m, true
		}
	}
}

// requireForwardedRefusal asserts an error frame carries /v1's refusal: a non-empty code
// (wantCode when it is non-empty) and a message that is /v1's own, not the generic
// fallback the relay used before it forwarded them.
func requireForwardedRefusal(t *testing.T, m liveDownMsg, wantCode string) {
	t.Helper()
	if m.Code == "" || (wantCode != "" && m.Code != wantCode) {
		t.Fatalf("error frame code = %q, want %q (the /v1 code must reach the browser)", m.Code, wantCode)
	}
	if m.Message == "" || m.Message == "live transcription upstream error" {
		t.Fatalf("error frame message = %q, want /v1's own message, not the generic fallback", m.Message)
	}
}

func sendEnd(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		t.Fatalf("send end: %v", err)
	}
}

// readUntilDone drains down-frames until the done message (or the socket ends).
func readUntilDone(t *testing.T, ctx context.Context, conn *websocket.Conn) bool {
	t.Helper()
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil {
			return false
		}
		if m.Type == "done" {
			return true
		}
	}
}

// requireLiveLoopbackRow polls for the single recorded usage row and asserts it was
// recorded at the /v1 relay (ReqPath), billed as it is, and attributed to the loopback
// user -- a bare session principal: UserID set, TokenID/ServiceID/ProjectID all empty.
// That is the proof the portal session produced exactly one row, at the canonical
// relay, not a second one at the portal transport.
func requireLiveLoopbackRow(t *testing.T, srv *Server, wantQuantity float64) usage.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := srv.Usage.All()
		if len(rows) == 1 {
			ev := rows[0]
			if ev.BillingUnit != usage.BillingUnitAudioSecond || ev.BillingQuantity != wantQuantity {
				t.Fatalf("billing = %v %q, want %v %q", ev.BillingQuantity, ev.BillingUnit, wantQuantity, usage.BillingUnitAudioSecond)
			}
			if ev.ReqPath != "/v1/audio/transcriptions/live" {
				t.Fatalf("ReqPath = %q, want /v1/audio/transcriptions/live (the row must be recorded at the canonical relay, not the portal transport)", ev.ReqPath)
			}
			if ev.UserID != "usr_dev" || ev.TokenID != "" {
				t.Fatalf("UserID/TokenID = %q/%q, want usr_dev and no token (the bare loopback session principal)", ev.UserID, ev.TokenID)
			}
			if ev.ServiceID != "" || ev.ProjectID != "" {
				t.Fatalf("ServiceID/ProjectID = %q/%q, want empty (the loopback user has no service/project)", ev.ServiceID, ev.ProjectID)
			}
			if ev.RouteID != "route-asr" {
				t.Fatalf("RouteID = %q, want route-asr", ev.RouteID)
			}
			return ev
		}
		if len(rows) > 1 {
			t.Fatalf("expected exactly one usage row, got %d", len(rows))
		}
		if time.Now().After(deadline) {
			t.Fatalf("no usage row recorded within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLiveTranscriptionLoopbackRelaysDeltasLive proves the valid ticket upgrades (the
// fixed subprotocol is selected) and that a PCM frame up produces a transcript delta
// down -- through the whole browser -> loopback -> /v1 -> child chain -- before the
// next frame is sent (no buffering).
func TestLiveTranscriptionLoopbackRelaysDeltasLive(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "en")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	if conn.Subprotocol() != liveWSProtocol {
		t.Fatalf("subprotocol = %q, want %q", conn.Subprotocol(), liveWSProtocol)
	}

	// One PCM frame up -> a delta comes back before we send the next frame.
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 1600)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
	m, err := readLiveMsg(t, ctx, conn)
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}
	if m.Type != "delta" || m.Text == "" {
		t.Fatalf("first message = %+v, want a non-empty delta", m)
	}

	// The loopback re-resolved asr-model to the direct child and forwarded the mapped
	// name + the PCM triple (billing-rate == forwarded-rate), proving the /v1 relay --
	// not the portal transport -- built the upstream request.
	if child.path() != "/v1/audio/transcriptions/live" {
		t.Fatalf("child path = %q, want /v1/audio/transcriptions/live", child.path())
	}
	if !strings.Contains(child.query(), "model=whisper_x") || !strings.Contains(child.query(), "sample_rate=16000") {
		t.Fatalf("forwarded query %q lacks model=whisper_x and the rate triple", child.query())
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestLiveTranscriptionLoopbackCleanEndBillsOnceAtV1 proves end-of-input -> terminating
// chunk -> clean done -> ONE usage row, recorded at /v1 and attributed to the loopback
// user, never a second row at the portal. 32000 bytes at 16 kHz mono s16le (32000 B/s)
// is 1.0 second.
func TestLiveTranscriptionLoopbackCleanEndBillsOnceAtV1(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 32000)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn); err != nil {
		t.Fatalf("read delta: %v", err)
	}
	sendEnd(t, ctx, conn)
	if !readUntilDone(t, ctx, conn) {
		t.Fatal("never received a done message")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	ev := requireLiveLoopbackRow(t, srv, 1.0)
	if ev.Status != "success" || ev.HTTPStatus == 0 {
		t.Fatalf("usage row status/httpStatus = %q/%d, want success and a non-zero status", ev.Status, ev.HTTPStatus)
	}
}

// TestLiveTranscriptionLoopbackTruncatedLoudZero proves a WS close WITHOUT end-of-input
// truncates the loopback body, so /v1 loud-zeros: exactly one row, quantity 0, still
// status success (the upstream answered 2xx; only delivery was cut).
func TestLiveTranscriptionLoopbackTruncatedLoudZero(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 16000)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn); err != nil {
		t.Fatalf("read delta: %v", err)
	}
	// Drop WITHOUT end-of-input.
	_ = conn.CloseNow()

	ev := requireLiveLoopbackRow(t, srv, 0)
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (truncated is success + loud zero, not an error row)", ev.Status)
	}
}

// TestLiveTranscriptionLoopbackRelaysPartial proves a transcript.text.partial event
// relays through the chain as its typed partial down-message.
func TestLiveTranscriptionLoopbackRelaysPartial(t *testing.T) {
	child := newMockLiveChild(t)
	child.emitPartial = true
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 320)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn); err != nil {
		t.Fatalf("read delta: %v", err)
	}
	sendEnd(t, ctx, conn)

	sawPartial := false
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil {
			break
		}
		if m.Type == "partial" {
			sawPartial = true
			if m.Text == "" {
				t.Fatalf("partial carried no text")
			}
		}
		if m.Type == "done" {
			break
		}
	}
	if !sawPartial {
		t.Fatal("never received a partial message")
	}
}

// TestLiveTranscriptionLoopbackErrorEventRelayed proves a mid-stream SSE error event
// relays as a typed error down-message and the /v1 relay loud-zeros it (status success,
// the upstream answered 2xx before the error).
func TestLiveTranscriptionLoopbackErrorEventRelayed(t *testing.T) {
	child := newMockLiveChild(t)
	child.emitError = true
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	sawError := false
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil {
			break
		}
		if m.Type == "error" {
			sawError = true
			if m.Message == "" {
				t.Fatalf("error message empty")
			}
			break
		}
	}
	if !sawError {
		t.Fatal("never received an error message")
	}
	ev := requireLiveLoopbackRow(t, srv, 0)
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (a post-200 SSE error is success + loud zero)", ev.Status)
	}
}

// TestLiveTranscriptionLoopbackNonStreamingModelErrorFrame proves the /v1 gate surfaces
// to the browser: a model whose transcription_streaming verdict is not yes is refused at
// /v1 (409), the loopback sees the non-2xx, and the WebSocket relays a typed error
// down-frame. The refusal is a gate, not a relay, so NO usage row is recorded.
func TestLiveTranscriptionLoopbackNonStreamingModelErrorFrame(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-offline", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// The frame carries /v1's own stable code and message (not a generic sentence), so
	// the browser can tell "this model cannot stream" from every other failure.
	refusal, sawError := readErrorFrame(t, ctx, conn)
	if !sawError {
		t.Fatal("never received an error message for a non-streaming model")
	}
	requireForwardedRefusal(t, refusal, liveTranscriptionNotStreaming)
	// A gate refusal never bills and never reaches the child.
	if child.path() != "" {
		t.Fatalf("the child was contacted (%q) for a non-streaming model", child.path())
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(srv.Usage.All()); n != 0 {
		t.Fatalf("usage rows = %d, want 0 for a /v1 gate refusal", n)
	}
}

// TestLiveTranscriptionLoopbackBusyRejectsSecondSession proves the per-route session
// guard now lives at /v1: a second concurrent portal session on the same route is
// refused there (503 busy) and surfaced to the browser as an error down-frame, not a
// silent collision.
func TestLiveTranscriptionLoopbackBusyRejectsSecondSession(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Session 1 holds the route's slot: established once it has exchanged a frame (its
	// /v1 relay has acquired the slot by the time the first delta comes back).
	conn1, _, err := dialLive(t, ctx, ts.URL, mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", ""), nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer conn1.CloseNow()
	if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, 320)); err != nil {
		t.Fatalf("session1 pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn1); err != nil {
		t.Fatalf("session1 delta: %v", err)
	}

	// Session 2 on the same route: its /v1 loopback is refused 503 busy -> the WebSocket
	// relays an error down-frame.
	conn2, _, err := dialLive(t, ctx, ts.URL, mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", ""), nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.CloseNow()
	refusal, sawError := readErrorFrame(t, ctx, conn2)
	if !sawError {
		t.Fatal("session 2 never received a busy error frame")
	}
	// The busy code reaches the browser intact: it is what makes the portal show "the
	// model is busy" and not retry, instead of a generic failure.
	requireForwardedRefusal(t, refusal, liveBearerBusyCode)
}

// TestLiveTranscriptionLoopbackAdmitsOnceAtV1NotAtMint is the no-double-admit invariant:
// with the ticket user rate-limited to ONE admission, the ticket mint never spends the
// budget (admit=false: both mints return 200), the first portal session's /v1 loopback
// spends it (admission #1 -> success + one usage row), and a second session's /v1
// loopback is the SECOND admission -> denied 429 -> an error down-frame and no extra
// usage row. Had the mint admitted, session 1 would already have been the second tick
// and failed.
func TestLiveTranscriptionLoopbackAdmitsOnceAtV1NotAtMint(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	// usr_dev may admit exactly once per hour.
	store := newFakePrincipalStore()
	store.setConfig(Principal{Type: routing.PrincipalTypeUser, ID: "usr_dev"}, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 3600})
	srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Two mints through the real endpoint: both 200. If the mint admitted, the second
	// would be the (denied) second tick -- so two 200s prove the mint spends nothing.
	ticket1 := mintTicketViaEndpoint(t, srv, "asr-model")
	ticket2 := mintTicketViaEndpoint(t, srv, "asr-model")

	// Session 1: /v1 admission #1 -> succeeds end to end.
	conn1, _, err := dialLive(t, ctx, ts.URL, ticket1, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, 32000)); err != nil {
		t.Fatalf("session1 pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn1); err != nil {
		t.Fatalf("session1 delta: %v", err)
	}
	sendEnd(t, ctx, conn1)
	if !readUntilDone(t, ctx, conn1) {
		t.Fatal("session 1 never finished: /v1 denied the FIRST admission, so the mint must have already spent it")
	}
	_ = conn1.Close(websocket.StatusNormalClosure, "")

	// Session 2: /v1 admission #2 -> denied 429 -> error down-frame.
	conn2, _, err := dialLive(t, ctx, ts.URL, ticket2, nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer conn2.CloseNow()
	refusal, sawError := readErrorFrame(t, ctx, conn2)
	if !sawError {
		t.Fatal("session 2 was not denied: /v1 did not admit, so admission is not enforced at the one relay")
	}
	// The denial's own code and message are forwarded too (whatever the 429 states), not
	// collapsed to the generic upstream-error sentence.
	requireForwardedRefusal(t, refusal, "")

	// Exactly one usage row overall: session 1's clean session. Session 2 was denied
	// before any relay (no row), and the portal transport records none.
	ev := requireLiveLoopbackRow(t, srv, 1.0)
	if ev.Status != "success" {
		t.Fatalf("row status = %q, want success", ev.Status)
	}
}

// mintTicketViaEndpoint mints a ticket through the real POST
// /api/portal/transcription-live-ticket (admit=false), authenticated by the dev bearer,
// and returns the opaque ticket. A non-200 fails the test.
func mintTicketViaEndpoint(t *testing.T, srv *Server, model string) string {
	t.Helper()
	body := fmt.Sprintf(`{"model":%q,"sample_rate":16000,"channels":1,"sample_format":"s16le"}`, model)
	req := httptest.NewRequest(http.MethodPost, "/api/portal/transcription-live-ticket", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("mint endpoint status = %d, want 200 (the mint must NOT admit); body = %s", rec.Code, rec.Body.String())
	}
	var resp liveTicketResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ticket response %q: %v", rec.Body.Bytes(), err)
	}
	if resp.Ticket == "" {
		t.Fatalf("mint endpoint returned an empty ticket: %s", rec.Body.String())
	}
	return resp.Ticket
}

// TestLiveTranscriptionMissingTicketNoUpgrade proves a missing ticket is refused with
// an HTTP error and no upgrade.
func TestLiveTranscriptionMissingTicketNoUpgrade(t *testing.T) {
	child := newMockLiveChild(t)
	_, ts := newLiveLoopbackTestServer(t, child)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := dialLive(t, ctx, ts.URL, "", nil)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("expected the upgrade to be refused without a ticket")
	}
	if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		t.Fatalf("status = %v, want 401/403", resp)
	}
}

// TestLiveTranscriptionReplayedTicketRefused proves a ticket is single-use: the second
// upgrade with the same ticket is refused with no upgrade.
func TestLiveTranscriptionReplayedTicketRefused(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn1, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	defer conn1.CloseNow()

	conn2, resp, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err == nil {
		_ = conn2.CloseNow()
		t.Fatal("expected the replayed ticket to be refused")
	}
	if resp == nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		t.Fatalf("replay status = %v, want 401/403", resp)
	}
}

// TestLiveTranscriptionCrossOriginRefused proves a cross-origin upgrade is refused by
// coder/websocket's default (same-origin) Accept, even with a valid ticket.
func TestLiveTranscriptionCrossOriginRefused(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Origin", "https://evil.example")
	conn, resp, err := dialLive(t, ctx, ts.URL, ticket, hdr)
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("expected a cross-origin upgrade to be refused")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin status = %v, want 403", resp)
	}
}

// TestLiveTranscriptionReadLimitEnforced proves SetReadLimit caps an inbound frame: a
// frame over the cap closes the connection with StatusMessageTooBig, before the PCM is
// ever relayed to the loopback.
func TestLiveTranscriptionReadLimitEnforced(t *testing.T) {
	child := newMockLiveChild(t)
	srv, ts := newLiveLoopbackTestServer(t, child)

	ticket := mintLiveModelTicket(t, srv, "asr-model", 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	// A client is allowed to WRITE a large frame; the SERVER's read limit rejects it.
	conn.SetReadLimit(1 << 21)
	oversize := make([]byte, maxLiveFrameBytes+1)
	_ = conn.Write(ctx, websocket.MessageBinary, oversize)
	_, _, rerr := conn.Read(ctx)
	if websocket.CloseStatus(rerr) != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %v (err %v), want StatusMessageTooBig", websocket.CloseStatus(rerr), rerr)
	}
}

// TestLiveRefusalFrame pins how a non-2xx /v1 body becomes the browser's error frame: the
// gateway's envelope yields its code and message; other shapes yield what they state
// (never a code); anything unreadable falls back to the generic message with no code.
func TestLiveRefusalFrame(t *testing.T) {
	const generic = "live transcription upstream error"
	cases := []struct {
		name        string
		body        string
		wantMessage string
		wantCode    string
	}{
		{"gateway envelope", `{"error":{"code":"transcription.live_busy","message":"busy now","request_id":"req_1"}}`, "busy now", "transcription.live_busy"},
		{"envelope without a code", `{"error":{"message":"no code here"}}`, "no code here", ""},
		{"upstream one-line string error", `{"error":"model not managed"}`, "model not managed", ""},
		{"upstream flat message", `{"message":"admission blocked"}`, "admission blocked", ""},
		{"code that is not a string", `{"error":{"code":503,"message":"numeric"}}`, "numeric", ""},
		{"code that is not a plausible code", `{"error":{"code":"` + strings.Repeat("x", maxLiveRefusalCodeBytes+1) + `","message":"long"}}`, "long", ""},
		{"empty body", ``, generic, ""},
		{"not json", `<html>502 Bad Gateway</html>`, generic, ""},
		{"json without an error", `{"ok":false}`, generic, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := liveRefusalFrame(strings.NewReader(tc.body))
			want := liveDownMsg{Type: "error", Message: tc.wantMessage, Code: tc.wantCode}
			if got != want {
				t.Fatalf("liveRefusalFrame(%q) = %+v, want %+v", tc.body, got, want)
			}
		})
	}
}

// TestLiveRefusalFrameBoundsTheBody proves an oversized refusal body is read only up to
// the bound: a body whose JSON is complete within the bound is parsed, and one whose
// envelope would only close past it falls back rather than being buffered whole.
func TestLiveRefusalFrameBoundsTheBody(t *testing.T) {
	huge := `{"error":{"code":"x.y","message":"` + strings.Repeat("a", maxLiveRefusalBodyBytes) + `"}}`
	got := liveRefusalFrame(strings.NewReader(huge))
	if got.Message != "live transcription upstream error" || got.Code != "" {
		t.Fatalf("liveRefusalFrame(oversized) = %+v, want the generic fallback with no code", got)
	}
}

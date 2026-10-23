// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/usage"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

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
	// gotHeader is a copy of the inbound request headers, so a test can assert which
	// upstream credential (if any) the gateway attached.
	gotHeader http.Header
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
	c.gotHeader = r.Header.Clone()
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

// header returns the first value of the named inbound request header ("" when absent).
func (c *mockLiveChild) header(name string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gotHeader.Get(name)
}

// newLiveTestServer builds a test Server whose native provider is a real
// OpenAI-compatible client (which implements NativeDuplexProxyClient), pointed by
// the seeded target at the mock child.
func newLiveTestServer(t *testing.T) *Server {
	t.Helper()
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	srv.Provider = provider.NewOpenAICompatibleClient(http.DefaultClient)
	return srv
}

func liveTarget(child *mockLiveChild, providerKind string) routing.Target {
	return routing.Target{
		RouteID:       "route_live",
		ServerID:      "srv_live",
		Provider:      providerKind,
		Endpoint:      child.server.URL,
		Model:         "whisper",
		ProviderModel: "whisper-1",
		APIFlavor:     apiFlavorSpeech,
	}
}

func mintLiveTicket(t *testing.T, srv *Server, target routing.Target, sampleRate, channels int, format, language string) string {
	t.Helper()
	return mintLiveTicketFor(t, srv, liveTicketClaims{
		Target:     target,
		SampleRate: sampleRate,
		Channels:   channels,
		Format:     format,
		Language:   language,
		UserID:     "usr_dev",
		TokenID:    "tok_dev",
	})
}

// mintLiveTicketFor mints a ticket for exactly the given claims, for a test that needs a
// field mintLiveTicket does not set (the billing attribution).
func mintLiveTicketFor(t *testing.T, srv *Server, claims liveTicketClaims) string {
	t.Helper()
	ticket, err := srv.liveTickets.Mint(claims)
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

func sendEnd(t *testing.T, ctx context.Context, conn *websocket.Conn) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"end"}`)); err != nil {
		t.Fatalf("send end: %v", err)
	}
}

// requireLiveUsageRow polls for the single recorded usage row and asserts its unit,
// quantity and attribution.
func requireLiveUsageRow(t *testing.T, srv *Server, wantUnit string, wantQuantity float64) usage.Event {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		rows := srv.Usage.All()
		if len(rows) == 1 {
			ev := rows[0]
			if ev.BillingUnit != wantUnit || ev.BillingQuantity != wantQuantity {
				t.Fatalf("billing = %v %q, want %v %q", ev.BillingQuantity, ev.BillingUnit, wantQuantity, wantUnit)
			}
			if ev.RouteID != "route_live" || ev.UserID != "usr_dev" || ev.TokenID != "tok_dev" {
				t.Fatalf("attribution = route=%q user=%q token=%q, want route_live/usr_dev/tok_dev", ev.RouteID, ev.UserID, ev.TokenID)
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

// TestLiveTranscriptionHappyPathRelaysDeltasLive proves the valid ticket upgrades
// (the fixed subprotocol is selected) and that a PCM frame up produces a delta down
// before the next frame is sent -- no buffering.
func TestLiveTranscriptionHappyPathRelaysDeltasLive(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, routing.ProviderServerAgent), 16000, 1, "s16le", "en")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	// Agent path: model rides in the URL path, NOT the query.
	if child.path() != "/live/whisper-1" {
		t.Fatalf("child path = %q, want /live/whisper-1", child.path())
	}
	if strings.Contains(child.query(), "model=") {
		t.Fatalf("agent child query %q must not carry model= (the agent adds it)", child.query())
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// TestLiveTranscriptionCleanEndBills proves end-of-input -> terminating chunk ->
// clean done -> billed pcmBytes/byteRate (a DISTINCT test from the truncated one).
func TestLiveTranscriptionCleanEndBills(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	// 16000 * 1 * 2 = 32000 bytes/sec. Send 32000 bytes -> 1.0 second.
	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 32000)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}
	// Drain the delta.
	if _, err := readLiveMsg(t, ctx, conn); err != nil {
		t.Fatalf("read delta: %v", err)
	}
	sendEnd(t, ctx, conn)

	// Read until the done message arrives.
	sawDone := false
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil {
			break
		}
		if m.Type == "done" {
			sawDone = true
			break
		}
	}
	if !sawDone {
		t.Fatal("never received a done message")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	ev := requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 1.0)
	// Billing-rate == forwarded-rate: the direct child's query carries the same triple.
	if !strings.Contains(child.query(), "sample_rate=16000") || !strings.Contains(child.query(), "sample_format=s16le") {
		t.Fatalf("forwarded query %q lacks the billed rate triple", child.query())
	}
	if ev.HTTPStatus == 0 {
		t.Fatalf("usage row has no HTTP status")
	}
	// Direct path: model rides in the query.
	if child.path() != "/v1/audio/transcriptions/live" {
		t.Fatalf("direct child path = %q", child.path())
	}
	if !strings.Contains(child.query(), "model=whisper-1") {
		t.Fatalf("direct child query %q must carry model=whisper-1", child.query())
	}
}

// TestLiveTranscriptionTruncatedLoudZero proves a WS close WITHOUT end-of-input is a
// truncated session -> loud zero (quantity 0), a DISTINCT test from the clean one.
func TestLiveTranscriptionTruncatedLoudZero(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	ev := requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 0)
	// A 200-then-truncated session is deliberately success + loud zero (the upstream
	// answered 2xx; only the delivery was cut). It must NOT be over-classified as an
	// error row -- that is reserved for a non-2xx upstream answer.
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (truncated is success + loud zero, not an error row)", ev.Status)
	}
}

// attributedLiveClaims is a 16 kHz mono s16le session on a direct child whose ticket was
// minted by a token carrying a service and a project (the attribution the ticket endpoint
// copies off the authenticated token).
func attributedLiveClaims(child *mockLiveChild) liveTicketClaims {
	return liveTicketClaims{
		Target:      liveTarget(child, "audio_cpp"),
		SampleRate:  16000,
		Channels:    1,
		Format:      "s16le",
		UserID:      "usr_dev",
		TokenID:     "tok_dev",
		ServiceID:   "svc_live",
		ServiceName: "Nightly Batch",
		ProjectID:   "proj_live",
		ProjectName: "Call Notes",
	}
}

// requireLiveRowAttribution fails unless the row carries the service and project of
// attributedLiveClaims.
func requireLiveRowAttribution(t *testing.T, ev usage.Event) {
	t.Helper()
	if ev.ServiceID != "svc_live" || ev.ServiceName != "Nightly Batch" || ev.ProjectID != "proj_live" || ev.ProjectName != "Call Notes" {
		t.Fatalf("service/project = %q/%q/%q/%q, want svc_live/Nightly Batch/proj_live/Call Notes",
			ev.ServiceID, ev.ServiceName, ev.ProjectID, ev.ProjectName)
	}
}

// TestLiveTranscriptionCleanRowIsServiceAndProjectAttributed proves the billed row of a
// clean session carries the service and project the ticket was minted with, as the buffered
// transcription row does -- the WebSocket has no principal of its own, so the ticket is
// where the attribution travels.
func TestLiveTranscriptionCleanRowIsServiceAndProjectAttributed(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicketFor(t, srv, attributedLiveClaims(child))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	for {
		m, err := readLiveMsg(t, ctx, conn)
		if err != nil || m.Type == "done" {
			break
		}
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")

	requireLiveRowAttribution(t, requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 1.0))
}

// TestLiveTranscriptionLoudZeroRowIsServiceAndProjectAttributed proves a truncated session's
// loud-zero row is attributed too: the zero is a real, visible row and belongs to the same
// service and project as a clean one.
func TestLiveTranscriptionLoudZeroRowIsServiceAndProjectAttributed(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicketFor(t, srv, attributedLiveClaims(child))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

	requireLiveRowAttribution(t, requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 0))
}

// TestLiveTranscriptionUnattributedTicketLeavesServiceAndProjectEmpty is the control: a
// ticket minted by a token with no service or project (a plain portal session) bills a row
// with none, so the attribution above comes from the claims and from nowhere else.
func TestLiveTranscriptionUnattributedTicketLeavesServiceAndProjectEmpty(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	_ = conn.CloseNow()

	ev := requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 0)
	if ev.ServiceID != "" || ev.ServiceName != "" || ev.ProjectID != "" || ev.ProjectName != "" {
		t.Fatalf("service/project = %q/%q/%q/%q, want all empty for an unattributed ticket",
			ev.ServiceID, ev.ServiceName, ev.ProjectID, ev.ProjectName)
	}
}

// TestLiveTranscriptionBusyRejectsSecondSession proves a second concurrent session on
// the same route gets the busy close code, and that a new one connects after release.
func TestLiveTranscriptionBusyRejectsSecondSession(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	target := liveTarget(child, "audio_cpp")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Session 1: holds the slot.
	conn1, _, err := dialLive(t, ctx, ts.URL, mintLiveTicket(t, srv, target, 16000, 1, "s16le", ""), nil)
	if err != nil {
		t.Fatalf("dial 1: %v", err)
	}
	// Make sure session 1 is fully established (slot acquired) by exchanging a frame.
	if err := conn1.Write(ctx, websocket.MessageBinary, make([]byte, 320)); err != nil {
		t.Fatalf("session1 pcm: %v", err)
	}
	if _, err := readLiveMsg(t, ctx, conn1); err != nil {
		t.Fatalf("session1 delta: %v", err)
	}

	// Session 2: same route -> busy close.
	conn2, _, err := dialLive(t, ctx, ts.URL, mintLiveTicket(t, srv, target, 16000, 1, "s16le", ""), nil)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	_, _, err = conn2.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Fatalf("session 2 close status = %v (err %v), want StatusTryAgainLater", websocket.CloseStatus(err), err)
	}
	_ = conn2.CloseNow()

	// Release session 1.
	sendEnd(t, ctx, conn1)
	for {
		if _, err := readLiveMsg(t, ctx, conn1); err != nil {
			break
		}
	}
	_ = conn1.Close(websocket.StatusNormalClosure, "")

	// Session 3: the slot is free again.
	deadline := time.Now().Add(3 * time.Second)
	for {
		conn3, _, err := dialLive(t, ctx, ts.URL, mintLiveTicket(t, srv, target, 16000, 1, "s16le", ""), nil)
		if err == nil {
			// Confirm it is live, not immediately busy-closed.
			if werr := conn3.Write(ctx, websocket.MessageBinary, make([]byte, 320)); werr == nil {
				if _, rerr := readLiveMsg(t, ctx, conn3); rerr == nil {
					_ = conn3.Close(websocket.StatusNormalClosure, "")
					return
				}
			}
			_ = conn3.CloseNow()
		}
		if time.Now().After(deadline) {
			t.Fatal("session 3 never connected after release")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestLiveTranscriptionMissingTicketNoUpgrade proves a missing ticket is refused with
// an HTTP error and no upgrade.
func TestLiveTranscriptionMissingTicketNoUpgrade(t *testing.T) {
	srv := newLiveTestServer(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()
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
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
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
// frame over the cap closes the connection with StatusMessageTooBig.
func TestLiveTranscriptionReadLimitEnforced(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	// The server closes with StatusMessageTooBig once it reads the oversized frame.
	_, _, rerr := conn.Read(ctx)
	if websocket.CloseStatus(rerr) != websocket.StatusMessageTooBig {
		t.Fatalf("close status = %v (err %v), want StatusMessageTooBig", websocket.CloseStatus(rerr), rerr)
	}
}

// TestLiveTranscriptionRelaysPartialAndError proves the partial and error SSE events
// relay as their typed down-messages.
func TestLiveTranscriptionRelaysPartialAndError(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	child.emitPartial = true
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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

// TestLiveTranscriptionErrorEventRelayed proves a mid-stream SSE error event relays as
// a typed error down-message and bills a loud zero.
func TestLiveTranscriptionErrorEventRelayed(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	child.emitError = true
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	ev := requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 0)
	// A post-200 mid-stream SSE error event is deliberately success + loud zero (the
	// upstream answered 2xx), distinct from the non-2xx-upstream status="error" row.
	if ev.Status != "success" {
		t.Fatalf("status = %q, want success (a post-200 SSE error is success + loud zero, not an error row)", ev.Status)
	}
}

// TestLiveTranscriptionNon2xxUpstreamErrorRow proves a non-2xx UPSTREAM answer is
// recorded as an ERROR row (status "error" + an upstream.<code>) with a loud-zero
// quantity -- never a self-contradictory status="success" row that hides a real
// backend failure while HTTPStatus carries the 4xx.
func TestLiveTranscriptionNon2xxUpstreamErrorRow(t *testing.T) {
	srv := newLiveTestServer(t)
	child := newMockLiveChild(t)
	child.statusCode = http.StatusNotFound
	ts := httptest.NewServer(srv)
	defer ts.Close()

	ticket := mintLiveTicket(t, srv, liveTarget(child, "audio_cpp"), 16000, 1, "s16le", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// The gateway's chunked upstream request sends its headers up front, so the
	// upstream exchange begins as soon as the request is dispatched (the first PCM
	// frame follows immediately in the mic flow). A PCM frame is sent here to mirror
	// that flow; the child answers its non-2xx status and the relay reports it.
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 320)); err != nil {
		t.Fatalf("send pcm: %v", err)
	}

	// The handler relays a typed error down-message on a non-2xx upstream.
	sawError := false
	for {
		m, rerr := readLiveMsg(t, ctx, conn)
		if rerr != nil {
			break
		}
		if m.Type == "error" {
			sawError = true
			break
		}
	}
	if !sawError {
		t.Fatal("never received an error message on a non-2xx upstream")
	}

	ev := requireLiveUsageRow(t, srv, usage.BillingUnitAudioSecond, 0)
	if ev.Status != "error" {
		t.Fatalf("status = %q, want error", ev.Status)
	}
	if ev.ErrorCode != "upstream.404" {
		t.Fatalf("errorCode = %q, want upstream.404", ev.ErrorCode)
	}
	if ev.HTTPStatus != http.StatusNotFound {
		t.Fatalf("httpStatus = %d, want 404", ev.HTTPStatus)
	}
}

// TestLiveTranscriptionAppliesUpstreamCredential proves the live relay attaches the
// per-mapping upstream credential to its upstream request -- exactly as the buffered
// native path does -- so a mapping whose child needs a token (a direct audio_cpp
// server with an API key, or an agent-managed child launched with ${API_TOKEN}) is
// reachable live. The ticket carries a sealed target.APIToken; the real
// OpenAI-compatible client (through the real upstreamAuthCtx -> applyUpstreamAuth
// chain) must deliver it to the child, and a token-less target must send none.
func TestLiveTranscriptionAppliesUpstreamCredential(t *testing.T) {
	const token = "live-upstream-token"
	cases := []struct {
		name         string
		providerKind string
		apiToken     string
		tokenHeader  string
		wantHeader   string // the header expected to carry the credential ("" = none)
		wantValue    string
		wantAbsent   []string // headers that must NOT be sent
	}{
		{
			name:         "direct audio_cpp default bearer",
			providerKind: "audio_cpp",
			apiToken:     token,
			wantHeader:   "Authorization",
			wantValue:    "Bearer " + token,
			wantAbsent:   []string{"X-Api-Key"},
		},
		{
			name:         "agent-managed target default bearer",
			providerKind: routing.ProviderServerAgent,
			apiToken:     token,
			wantHeader:   "Authorization",
			wantValue:    "Bearer " + token,
		},
		{
			name:         "custom credential header",
			providerKind: "audio_cpp",
			apiToken:     token,
			tokenHeader:  "X-Api-Key",
			wantHeader:   "X-Api-Key",
			wantValue:    token,
			wantAbsent:   []string{"Authorization"},
		},
		{
			name:         "token-less target sends no credential",
			providerKind: "audio_cpp",
			wantAbsent:   []string{"Authorization", "X-Api-Key"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newLiveTestServer(t)
			cipher := newDispatchCipher(t)
			srv.Cipher = cipher
			child := newMockLiveChild(t)
			ts := httptest.NewServer(srv)
			defer ts.Close()

			target := liveTarget(child, tc.providerKind)
			if tc.apiToken != "" {
				sealed, err := capture.SealSecret(cipher, false, tc.apiToken)
				if err != nil {
					t.Fatalf("seal token: %v", err)
				}
				target.APIToken = sealed
				target.APITokenHeader = tc.tokenHeader
			}
			ticket := mintLiveTicket(t, srv, target, 16000, 1, "s16le", "")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, _, err := dialLive(t, ctx, ts.URL, ticket, nil)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()

			// A round trip (PCM up, delta down) proves the child received and handled the
			// upstream request, so its recorded headers are the ones the relay sent.
			if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 320)); err != nil {
				t.Fatalf("send pcm: %v", err)
			}
			if _, err := readLiveMsg(t, ctx, conn); err != nil {
				t.Fatalf("read delta: %v", err)
			}

			if tc.wantHeader != "" {
				if got := child.header(tc.wantHeader); got != tc.wantValue {
					t.Fatalf("upstream %s = %q, want %q (the per-mapping credential never reached the live upstream)", tc.wantHeader, got, tc.wantValue)
				}
			}
			for _, name := range tc.wantAbsent {
				if got := child.header(name); got != "" {
					t.Fatalf("upstream %s = %q, want none", name, got)
				}
			}
		})
	}
}

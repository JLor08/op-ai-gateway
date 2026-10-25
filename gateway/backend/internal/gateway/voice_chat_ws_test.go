// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The Voice Chat WebSocket (GET /api/portal/voice-chat) is the ticket-gated, same-origin
// transport of the hands-free ASR -> LLM -> TTS loop. This file covers the part that is
// independent of the loop itself: the pre-upgrade ticket redeem and its opaque refusal,
// the upgrade and subprotocol, the wire frames, and the single writer. The state machine
// that rides on it is exercised by its own tests.

// voiceWSHarness is a full-stack gateway behind an httptest server, so every test goes
// through Server.ServeHTTP and the real mux (a route that is not registered, or one a
// middleware swallows, fails here). returned receives one value each time a request
// handler returns, which is how a test observes "the session tore down".
type voiceWSHarness struct {
	srv      *Server
	url      string
	returned chan struct{}
}

func newVoiceWSHarness(t *testing.T) *voiceWSHarness {
	t.Helper()
	h := &voiceWSHarness{srv: newTestServer(t), returned: make(chan struct{}, 16)}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.srv.ServeHTTP(w, r)
		select {
		case h.returned <- struct{}{}:
		default:
		}
	}))
	t.Cleanup(ts.Close)
	h.url = ts.URL
	return h
}

// mint returns a fresh, valid ticket from the server's own store.
func (h *voiceWSHarness) mint(t *testing.T) string {
	t.Helper()
	return mustMintVoiceTicket(t, h.srv.voiceTickets, sampleVoiceTicketClaims())
}

// dial opens the voice WebSocket the way the browser does: the fixed subprotocol first,
// the ticket second. An empty ticket offers the fixed subprotocol alone.
func (h *voiceWSHarness) dial(t *testing.T, ctx context.Context, ticket string, hdr http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	subs := []string{"op-voice-chat"}
	if ticket != "" {
		subs = append(subs, ticket)
	}
	return websocket.Dial(ctx, wsURL(h.url, "/api/portal/voice-chat"), &websocket.DialOptions{
		Subprotocols: subs,
		HTTPHeader:   hdr,
	})
}

// dialOK dials with a fresh valid ticket and fails the test if the upgrade is refused.
func (h *voiceWSHarness) dialOK(t *testing.T, ctx context.Context) *websocket.Conn {
	t.Helper()
	conn, _, err := h.dial(t, ctx, h.mint(t), nil)
	if err != nil {
		t.Fatalf("dial with a valid ticket: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

// requireHandlerReturns waits for one request handler to return, i.e. the session ended.
func (h *voiceWSHarness) requireHandlerReturns(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.returned:
	case <-time.After(5 * time.Second):
		t.Fatalf("the handler did not return after %s: the session leaked", what)
	}
}

func readVoiceFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) (voiceDownMsg, []byte) {
	t.Helper()
	typ, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read a down-frame: %v", err)
	}
	if typ != websocket.MessageText {
		t.Fatalf("down-frame type = %v, want text", typ)
	}
	var m voiceDownMsg
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode down-frame %q: %v", data, err)
	}
	return m, data
}

func sendVoiceControl(t *testing.T, ctx context.Context, conn *websocket.Conn, typ string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf(`{"type":%q}`, typ))); err != nil {
		t.Fatalf("send control %q: %v", typ, err)
	}
}

// requireNoUpgrade asserts a dial was refused before any upgrade with the given status.
func requireNoUpgrade(t *testing.T, conn *websocket.Conn, resp *http.Response, err error, wantStatus int) {
	t.Helper()
	if err == nil {
		_ = conn.CloseNow()
		t.Fatal("the upgrade succeeded, want it refused")
	}
	if resp == nil || resp.StatusCode != wantStatus {
		t.Fatalf("status = %v, want %d with no upgrade (err %v)", resp, wantStatus, err)
	}
}

// refusalOf captures the externally observable part of a refused upgrade: everything a
// caller can compare to tell one refusal from another.
type refusalOf struct {
	Status      int
	Body        string
	ContentType string
	Auth        string
}

func captureRefusal(t *testing.T, resp *http.Response) refusalOf {
	t.Helper()
	if resp == nil {
		t.Fatal("no HTTP response to capture")
	}
	body, _ := io.ReadAll(resp.Body)
	return refusalOf{
		Status:      resp.StatusCode,
		Body:        string(body),
		ContentType: resp.Header.Get("Content-Type"),
		Auth:        resp.Header.Get("WWW-Authenticate"),
	}
}

// TestVoiceWSRejectsMissingTicket: an upgrade that carries no ticket is refused 401 and
// is never upgraded, whether it offers no subprotocol at all or only the fixed one.
func TestVoiceWSRejectsMissingTicket(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	conn, resp, err := h.dial(t, ctx, "", nil)
	requireNoUpgrade(t, conn, resp, err, http.StatusUnauthorized)

	conn, resp, err = websocket.Dial(ctx, wsURL(h.url, "/api/portal/voice-chat"), nil)
	requireNoUpgrade(t, conn, resp, err, http.StatusUnauthorized)
}

// TestVoiceWSRejectsRedeemedTicket: a ticket is single-use, so the second upgrade with
// the same ticket is refused 401 with no upgrade.
func TestVoiceWSRejectsRedeemedTicket(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticket := h.mint(t)
	first, _, err := h.dial(t, ctx, ticket, nil)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	defer first.CloseNow()

	conn, resp, err := h.dial(t, ctx, ticket, nil)
	requireNoUpgrade(t, conn, resp, err, http.StatusUnauthorized)
}

// TestVoiceWSRefusesEveryTicketFailureWithOneOpaqueResponse is the Task-1 carry-forward:
// a forged, a foreign-keyed, an expired and a replayed ticket (and a missing one) must
// be indistinguishable on the wire, or a caller holding a captured ticket could probe
// which of the three failures it hit. The SPECIFIC cause still reaches the operator's
// log, at the level a default deployment keeps.
func TestVoiceWSRefusesEveryTicketFailureWithOneOpaqueResponse(t *testing.T) {
	type refusalCase struct {
		name      string
		wantCause string // "" = no ticket was presented, so there is no cause to name
		// ticket prepares the ticket to present, given the harness.
		ticket func(t *testing.T, ctx context.Context, h *voiceWSHarness) string
	}
	cases := []refusalCase{
		{"missing", "", func(*testing.T, context.Context, *voiceWSHarness) string { return "" }},
		{"forged", "invalid", func(*testing.T, context.Context, *voiceWSHarness) string { return "bm90LWEtdGlja2V0LWF0LWFsbA" }},
		{"foreign key", "invalid", func(t *testing.T, _ context.Context, _ *voiceWSHarness) string {
			return mustMintVoiceTicket(t, newVoiceTicketStore(), sampleVoiceTicketClaims())
		}},
		{"expired", "expired", func(t *testing.T, _ context.Context, h *voiceWSHarness) string {
			store := h.srv.voiceTickets
			store.now = func() time.Time { return time.Now().Add(-time.Hour) }
			ticket := mustMintVoiceTicket(t, store, sampleVoiceTicketClaims())
			store.now = time.Now
			return ticket
		}},
		{"replayed", "replayed", func(t *testing.T, ctx context.Context, h *voiceWSHarness) string {
			ticket := h.mint(t)
			first, _, err := h.dial(t, ctx, ticket, nil)
			if err != nil {
				t.Fatalf("first dial: %v", err)
			}
			t.Cleanup(func() { _ = first.CloseNow() })
			return ticket
		}},
	}

	h := newVoiceWSHarness(t)
	var want *refusalOf
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := withCapturedSlogAtTheDefaultLevel(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			conn, resp, err := h.dial(t, ctx, tc.ticket(t, ctx, h), nil)
			requireNoUpgrade(t, conn, resp, err, http.StatusUnauthorized)

			got := captureRefusal(t, resp)
			if got.Body == "" {
				t.Fatal("the refusal has an empty body, want the one opaque sentence")
			}
			if want == nil {
				want = &got
			} else if got != *want {
				t.Fatalf("refusal = %+v, want it identical to every other ticket failure: %+v", got, *want)
			}
			for _, leak := range []string{"invalid", "expired", "replay", "redeemed", "forged", "missing"} {
				if strings.Contains(strings.ToLower(got.Body), leak) {
					t.Fatalf("the refusal body %q names the cause (%q), want it opaque", got.Body, leak)
				}
			}

			if tc.wantCause == "" {
				return
			}
			var cause any
			for _, rec := range logs.Snapshot() {
				if rec.Level == "WARN" && rec.Msg == "voice chat: ticket refused" {
					cause = rec.Attrs["cause"]
				}
			}
			if cause != tc.wantCause {
				t.Fatalf("logged cause = %v, want %q (records: %+v)", cause, tc.wantCause, logs.Snapshot())
			}
		})
	}
}

// TestVoiceWSDoesNotLogTheTicket: the ticket is a bearer credential for the 30 seconds it
// lives; it must never reach a log line, refused or accepted.
func TestVoiceWSDoesNotLogTheTicket(t *testing.T) {
	h := newVoiceWSHarness(t)
	logs := withCapturedSlogAtTheDefaultLevel(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ticket := h.mint(t)
	conn, _, err := h.dial(t, ctx, ticket, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	replay, resp, err := h.dial(t, ctx, ticket, nil)
	requireNoUpgrade(t, replay, resp, err, http.StatusUnauthorized)

	for _, rec := range logs.Snapshot() {
		if strings.Contains(rec.Msg, ticket) || strings.Contains(fmt.Sprint(rec.Attrs), ticket) {
			t.Fatalf("a log record carries the ticket: %+v", rec)
		}
	}
}

// TestVoiceWSAcceptsAndSendsInitialState: a valid ticket upgrades the socket on the FIXED
// subprotocol (the opaque ticket is never the negotiated one) and the first down-frame is
// the state frame the UI keys its listening indicator on.
func TestVoiceWSAcceptsAndSendsInitialState(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	if got := conn.Subprotocol(); got != "op-voice-chat" {
		t.Fatalf("negotiated subprotocol = %q, want the fixed op-voice-chat, never the ticket", got)
	}

	m, raw := readVoiceFrame(t, ctx, conn)
	if m.Type != "state" || m.State != "listening" {
		t.Fatalf("first down-frame = %s, want a state frame in state listening", raw)
	}
	// Exact bytes: no stray zero-valued field (seq, rate, text, ...) rides a state frame.
	if string(raw) != `{"type":"state","state":"listening"}` {
		t.Fatalf("state frame = %s, want exactly {\"type\":\"state\",\"state\":\"listening\"}", raw)
	}
}

// TestVoiceWSRejectsNonGET: the route is an upgrade endpoint, GET only.
func TestVoiceWSRejectsNonGET(t *testing.T) {
	h := newVoiceWSHarness(t)
	resp, err := http.Post(h.url+"/api/portal/voice-chat", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
}

// TestVoiceWSCrossOriginRefused: the ticket is not the only guard. A browser upgrade
// cannot carry the CSRF header, so coder/websocket's default same-origin check is the
// origin defense, and it refuses a cross-origin page even holding a valid ticket.
func TestVoiceWSCrossOriginRefused(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	hdr := http.Header{}
	hdr.Set("Origin", "https://evil.example")
	conn, resp, err := h.dial(t, ctx, h.mint(t), hdr)
	requireNoUpgrade(t, conn, resp, err, http.StatusForbidden)
}

// TestVoiceWSReadLimitEnforced: SetReadLimit caps an inbound frame; one past the cap
// closes the connection with StatusMessageTooBig instead of buffering it.
func TestVoiceWSReadLimitEnforced(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	// A client may WRITE a large frame; the SERVER's read limit is what rejects it.
	conn.SetReadLimit(1 << 21)
	_ = conn.Write(ctx, websocket.MessageBinary, make([]byte, maxLiveFrameBytes+1))
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the initial state frame
		}
		if websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
			t.Fatalf("close status = %v (err %v), want StatusMessageTooBig", websocket.CloseStatus(err), err)
		}
		break
	}
	h.requireHandlerReturns(t, "an oversize frame")
}

// TestVoiceWSAcceptsAFrameUpToTheLiveFrameCap: the cap is maxLiveFrameBytes, not the
// websocket library's much smaller default (32 KiB), which is what an unset limit would
// silently leave in force. A frame well past that default, under the cap, is delivered
// and the session carries on.
func TestVoiceWSAcceptsAFrameUpToTheLiveFrameCap(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 256<<10)); err != nil {
		t.Fatalf("send a 256 KiB frame: %v", err)
	}
	sendVoiceControl(t, ctx, conn, "bye")
	for {
		_, _, err := conn.Read(ctx)
		if err == nil {
			continue // the initial state frame
		}
		if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
			t.Fatalf("close status = %v (err %v), want StatusNormalClosure: a frame under the cap must not end the session", websocket.CloseStatus(err), err)
		}
		break
	}
	h.requireHandlerReturns(t, "bye after a large frame")
}

// TestVoiceWSByeClosesCleanly: the bye control ends the session with a proper close
// handshake (1000), not an abrupt drop, and leaves no handler behind.
func TestVoiceWSByeClosesCleanly(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	if m, raw := readVoiceFrame(t, ctx, conn); m.Type != "state" {
		t.Fatalf("first down-frame = %s, want a state frame", raw)
	}
	sendVoiceControl(t, ctx, conn, "bye")

	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("after bye the close status = %v (err %v), want StatusNormalClosure", websocket.CloseStatus(err), err)
	}
	h.requireHandlerReturns(t, "bye")
}

// TestVoiceWSClientDropTearsDown: a client that vanishes without a bye still ends the
// session, with the handler (and so its writer goroutine) gone.
func TestVoiceWSClientDropTearsDown(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	if m, raw := readVoiceFrame(t, ctx, conn); m.Type != "state" {
		t.Fatalf("first down-frame = %s, want a state frame", raw)
	}
	_ = conn.CloseNow()
	h.requireHandlerReturns(t, "a client drop")
}

// TestVoiceWSIgnoresFramesItDoesNotUnderstand: PCM, garbage and unknown or not yet
// handled controls neither kill nor wedge the session; it still answers a bye cleanly.
func TestVoiceWSIgnoresFramesItDoesNotUnderstand(t *testing.T) {
	h := newVoiceWSHarness(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn := h.dialOK(t, ctx)
	if m, raw := readVoiceFrame(t, ctx, conn); m.Type != "state" {
		t.Fatalf("first down-frame = %s, want a state frame", raw)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, make([]byte, 3200)); err != nil {
		t.Fatalf("send PCM: %v", err)
	}
	for _, junk := range []string{`not json`, `{"type":"nope"}`, `{"type":"end"}`, `[]`} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(junk)); err != nil {
			t.Fatalf("send %q: %v", junk, err)
		}
	}
	for _, ctl := range []string{"push_start", "push_end", "cancel"} {
		sendVoiceControl(t, ctx, conn, ctl)
	}
	sendVoiceControl(t, ctx, conn, "bye")

	_, _, err := conn.Read(ctx)
	if websocket.CloseStatus(err) != websocket.StatusNormalClosure {
		t.Fatalf("close status = %v (err %v), want StatusNormalClosure: the session must survive the ignored frames", websocket.CloseStatus(err), err)
	}
	h.requireHandlerReturns(t, "bye after ignored frames")
}

// TestVoiceTicketRefusalCause pins the log vocabulary for the three redeem sentinels.
func TestVoiceTicketRefusalCause(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{errVoiceTicketInvalid, "invalid"},
		{errVoiceTicketExpired, "expired"},
		{errVoiceTicketReplayed, "replayed"},
		{fmt.Errorf("wrapped: %w", errVoiceTicketReplayed), "replayed"},
		{errors.New("something else"), "unknown"},
	}
	for _, tc := range cases {
		if got := voiceTicketRefusalCause(tc.err); got != tc.want {
			t.Errorf("voiceTicketRefusalCause(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestExtractVoiceTicket(t *testing.T) {
	cases := []struct{ header, want string }{
		{"op-voice-chat, tkt", "tkt"},
		{"tkt, op-voice-chat", "tkt"},
		{"op-voice-chat,tkt", "tkt"},
		{"  op-voice-chat  ,   tkt  ", "tkt"},
		{"tkt", "tkt"},
		{"op-voice-chat", ""},
		{"", ""},
		{" , ,", ""},
	}
	for _, tc := range cases {
		if got := extractVoiceTicket(tc.header); got != tc.want {
			t.Errorf("extractVoiceTicket(%q) = %q, want %q", tc.header, got, tc.want)
		}
	}
}

func TestParseVoiceUpControl(t *testing.T) {
	valid := []string{"push_start", "push_end", "cancel", "bye"}
	for _, typ := range valid {
		got, ok := parseVoiceUpControl([]byte(fmt.Sprintf(`{"type":%q}`, typ)))
		if !ok || got.Type != typ {
			t.Errorf("parseVoiceUpControl(%q) = (%+v, %v), want (%q, true)", typ, got, ok, typ)
		}
	}
	// Unknown extra fields are tolerated (a newer client), whitespace is JSON's own.
	for _, raw := range []string{`{"type":"bye","reason":"user left"}`, ` { "type" : "bye" } `, "{\"type\":\"bye\"}\n"} {
		if got, ok := parseVoiceUpControl([]byte(raw)); !ok || got.Type != "bye" {
			t.Errorf("parseVoiceUpControl(%q) = (%+v, %v), want (bye, true)", raw, got, ok)
		}
	}

	invalid := map[string]string{
		"empty":            ``,
		"not json":         `not json`,
		"empty object":     `{}`,
		"empty type":       `{"type":""}`,
		"wrong case":       `{"type":"BYE"}`,
		"live end":         `{"type":"end"}`,
		"unknown type":     `{"type":"nope"}`,
		"down type":        `{"type":"state"}`,
		"numeric type":     `{"type":5}`,
		"null type":        `{"type":null}`,
		"null":             `null`,
		"array":            `[]`,
		"bare string":      `"bye"`,
		"number":           `42`,
		"truncated":        `{"type":"bye"`,
		"trailing garbage": `{"type":"bye"} x`,
	}
	for name, raw := range invalid {
		if got, ok := parseVoiceUpControl([]byte(raw)); ok {
			t.Errorf("%s: parseVoiceUpControl(%q) = (%+v, true), want it rejected", name, raw, got)
		}
	}
}

// TestVoiceDownMsgWireShapes pins the JSON of every down-frame (spec 5.2): which fields
// ride which frame, and that a zero-valued field never leaks onto one that does not own
// it. audio_delta is the subtle one: its seq starts at 0 and must still be on the wire.
func TestVoiceDownMsgWireShapes(t *testing.T) {
	cases := []struct {
		msg  voiceDownMsg
		want string
	}{
		{voiceDownMsg{Type: voiceDownState, State: voiceStateListening}, `{"type":"state","state":"listening"}`},
		{voiceDownMsg{Type: voiceDownState, State: voiceStateIdle}, `{"type":"state","state":"idle"}`},
		{voiceDownMsg{Type: voiceDownState, State: voiceStateThinking}, `{"type":"state","state":"thinking"}`},
		{voiceDownMsg{Type: voiceDownState, State: voiceStateSpeaking}, `{"type":"state","state":"speaking"}`},
		{voiceDownMsg{Type: voiceDownState, State: voiceStateDraining}, `{"type":"state","state":"draining"}`},
		{voiceDownMsg{Type: voiceDownASRPartial, Text: "hallo"}, `{"type":"asr_partial","text":"hallo"}`},
		{voiceDownMsg{Type: voiceDownASRFinal, Text: "hallo welt", MessageID: "msg-1"}, `{"type":"asr_final","text":"hallo welt","message_id":"msg-1"}`},
		{voiceDownMsg{Type: voiceDownAnswerDelta, Text: "Hi"}, `{"type":"answer_delta","text":"Hi"}`},
		{voiceDownMsg{Type: voiceDownAnswerDone, MessageID: "msg-2"}, `{"type":"answer_done","message_id":"msg-2"}`},
		{voiceDownMsg{Type: voiceDownAnswerReset}, `{"type":"answer_reset"}`},
		{voiceDownMsg{Type: voiceDownAudioDelta, Seq: 0, Rate: 24000, Audio: "AAA="}, `{"type":"audio_delta","seq":0,"rate":24000,"audio":"AAA="}`},
		{voiceDownMsg{Type: voiceDownAudioDelta, Seq: 7, Rate: 24000, Audio: "AQI="}, `{"type":"audio_delta","seq":7,"rate":24000,"audio":"AQI="}`},
		{voiceDownMsg{Type: voiceDownAudioDone}, `{"type":"audio_done"}`},
		{voiceDownMsg{Type: voiceDownError, Code: "runtime.admission_blocked", Message: "no room"}, `{"type":"error","code":"runtime.admission_blocked","message":"no room"}`},
	}
	for _, tc := range cases {
		got, err := json.Marshal(tc.msg)
		if err != nil {
			t.Fatalf("marshal %+v: %v", tc.msg, err)
		}
		var gotMap, wantMap map[string]any
		if err := json.Unmarshal(got, &gotMap); err != nil {
			t.Fatalf("decode own output %s: %v", got, err)
		}
		if err := json.Unmarshal([]byte(tc.want), &wantMap); err != nil {
			t.Fatalf("bad expectation %s: %v", tc.want, err)
		}
		if !reflect.DeepEqual(gotMap, wantMap) {
			t.Errorf("wire of %+v = %s, want %s", tc.msg, got, tc.want)
		}

		// And the other direction: what the browser reads back is what was sent.
		var back voiceDownMsg
		if err := json.Unmarshal(got, &back); err != nil {
			t.Fatalf("decode %s: %v", got, err)
		}
		if back != tc.msg {
			t.Errorf("round trip of %+v = %+v", tc.msg, back)
		}
	}
}

// fakeVoiceSocket is the writer's socket double: it records what the writer does to the
// connection and fails on demand, so each branch of run is reachable without a network.
type fakeVoiceSocket struct {
	mu            sync.Mutex
	writeErr      error
	pingErr       error
	writes        [][]byte
	pings         int
	closeNowCalls int
	closes        []websocket.StatusCode
}

func (f *fakeVoiceSocket) Write(_ context.Context, typ websocket.MessageType, p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if typ != websocket.MessageText {
		return fmt.Errorf("fake: unexpected message type %v", typ)
	}
	if f.writeErr != nil {
		return f.writeErr
	}
	f.writes = append(f.writes, append([]byte(nil), p...))
	return nil
}

func (f *fakeVoiceSocket) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pings++
	return f.pingErr
}

func (f *fakeVoiceSocket) Close(code websocket.StatusCode, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes = append(f.closes, code)
	return nil
}

func (f *fakeVoiceSocket) CloseNow() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeNowCalls++
	return nil
}

func (f *fakeVoiceSocket) snapshot() (writes []string, pings, closeNow int, closes []websocket.StatusCode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writes {
		writes = append(writes, string(w))
	}
	return writes, f.pings, f.closeNowCalls, append([]websocket.StatusCode(nil), f.closes...)
}

// startVoiceWriter runs a voiceWriter over sock with the given ping interval and returns
// it, the handler context it cancels on a reap, and a channel closed when run returns.
func startVoiceWriter(sock voiceSocket, pingInterval time.Duration) (*voiceWriter, context.Context, context.CancelFunc, <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	w := newVoiceWriter(sock, cancel)
	w.pingInterval = pingInterval
	w.pingTimeout = 200 * time.Millisecond
	w.writeTimeout = 200 * time.Millisecond
	returned := make(chan struct{})
	go func() {
		w.run(ctx)
		close(returned)
	}()
	return w, ctx, cancel, returned
}

func requireWriterReturns(t *testing.T, returned <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatalf("the writer did not return after %s: its goroutine would leak", what)
	}
}

func TestNewVoiceWriterUsesTheAgentStreamTimings(t *testing.T) {
	w := newVoiceWriter(&fakeVoiceSocket{}, func() {})
	if w.pingInterval != agentStreamPingInterval || w.pingTimeout != agentStreamPingTimeout || w.writeTimeout != agentStreamWriteTimeout {
		t.Fatalf("timings = %v/%v/%v, want the agent stream's %v/%v/%v",
			w.pingInterval, w.pingTimeout, w.writeTimeout,
			agentStreamPingInterval, agentStreamPingTimeout, agentStreamWriteTimeout)
	}
	if cap(w.out) != voiceOutboundQueue {
		t.Fatalf("queue capacity = %d, want voiceOutboundQueue (%d)", cap(w.out), voiceOutboundQueue)
	}
}

// TestVoiceWriterWritesFramesInOrderThenFlushesOnCancel: frames reach the socket in the
// order they were sent, a frame still queued when the context ends is flushed (the final
// state or error of a session must not be lost), and the connection is then closed
// abruptly: the read loop holds the read mutex, so a close handshake would stall.
func TestVoiceWriterWritesFramesInOrderThenFlushesOnCancel(t *testing.T) {
	sock := &fakeVoiceSocket{}
	w, ctx, cancel, returned := startVoiceWriter(sock, time.Hour)

	for _, state := range []string{voiceStateListening, voiceStateThinking, voiceStateSpeaking} {
		w.send(ctx, voiceDownMsg{Type: voiceDownState, State: state})
	}
	cancel()
	requireWriterReturns(t, returned, "the context was cancelled")

	writes, _, closeNow, closes := sock.snapshot()
	want := []string{
		`{"type":"state","state":"listening"}`,
		`{"type":"state","state":"thinking"}`,
		`{"type":"state","state":"speaking"}`,
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v, in order and none lost", writes, want)
	}
	if closeNow != 1 || len(closes) != 0 {
		t.Fatalf("CloseNow = %d, Close = %v; want one abrupt close and no handshake", closeNow, closes)
	}
}

// TestVoiceWriterClosesNormallyWhenAsked: closeNormally flushes what is queued and then
// performs the close handshake with 1000 instead of the abrupt close.
func TestVoiceWriterClosesNormallyWhenAsked(t *testing.T) {
	sock := &fakeVoiceSocket{}
	w, ctx, _, returned := startVoiceWriter(sock, time.Hour)

	w.send(ctx, voiceDownMsg{Type: voiceDownState, State: voiceStateIdle})
	w.closeNormally()
	requireWriterReturns(t, returned, "closeNormally")

	writes, _, closeNow, closes := sock.snapshot()
	if want := []string{`{"type":"state","state":"idle"}`}; !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v flushed before the close", writes, want)
	}
	if !reflect.DeepEqual(closes, []websocket.StatusCode{websocket.StatusNormalClosure}) || closeNow != 0 {
		t.Fatalf("Close = %v, CloseNow = %d; want exactly one 1000 handshake and no abrupt close", closes, closeNow)
	}
	if ctx.Err() == nil {
		t.Fatal("closeNormally did not cancel the handler context")
	}
}

// TestVoiceWriterReapsOnWriteFailure: a failed data write reaps the connection AND
// cancels the handler context, never a bare return that would strand the relay.
func TestVoiceWriterReapsOnWriteFailure(t *testing.T) {
	sock := &fakeVoiceSocket{writeErr: errors.New("boom")}
	w, ctx, _, returned := startVoiceWriter(sock, time.Hour)

	w.send(ctx, voiceDownMsg{Type: voiceDownState, State: voiceStateListening})
	requireWriterReturns(t, returned, "a write failure")

	if _, _, closeNow, closes := sock.snapshot(); closeNow != 1 || len(closes) != 0 {
		t.Fatalf("CloseNow = %d, Close = %v; want exactly one reap", closeNow, closes)
	}
	if ctx.Err() == nil {
		t.Fatal("a dead peer must cancel the handler context so the whole relay tears down")
	}
}

// TestVoiceWriterReapsOnPingFailure: the keepalive reaps a peer that stopped ponging.
func TestVoiceWriterReapsOnPingFailure(t *testing.T) {
	sock := &fakeVoiceSocket{pingErr: errors.New("no pong")}
	_, ctx, _, returned := startVoiceWriter(sock, 2*time.Millisecond)

	requireWriterReturns(t, returned, "a ping failure")

	if _, pings, closeNow, _ := sock.snapshot(); pings < 1 || closeNow != 1 {
		t.Fatalf("pings = %d, CloseNow = %d; want a ping and then exactly one reap", pings, closeNow)
	}
	if ctx.Err() == nil {
		t.Fatal("a dead peer must cancel the handler context so the whole relay tears down")
	}
}

// TestVoiceWriterSurvivesAnIdleSessionAcrossManyPings: a session with no data traffic
// (the user is silent) outlives many keepalive cycles as long as each ping succeeds.
func TestVoiceWriterSurvivesAnIdleSessionAcrossManyPings(t *testing.T) {
	sock := &fakeVoiceSocket{}
	_, ctx, cancel, returned := startVoiceWriter(sock, 2*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, pings, _, _ := sock.snapshot(); pings >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the writer did not ping an idle session")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-returned:
		t.Fatal("the writer returned while every ping succeeded")
	default:
	}
	if _, _, closeNow, _ := sock.snapshot(); closeNow != 0 || ctx.Err() != nil {
		t.Fatalf("a healthy idle session was reaped (CloseNow = %d, ctx err = %v)", closeNow, ctx.Err())
	}
	cancel()
	requireWriterReturns(t, returned, "a clean cancel")
}

// TestVoiceWriterSendNeverBlocksPastTeardown: a sender that outruns a stalled writer
// blocks only until the context ends, so a producer can never outlive the session.
func TestVoiceWriterSendNeverBlocksPastTeardown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	w := newVoiceWriter(&fakeVoiceSocket{}, cancel) // never run: nothing drains the queue
	for range voiceOutboundQueue {
		w.send(ctx, voiceDownMsg{Type: voiceDownState, State: voiceStateListening})
	}

	done := make(chan struct{})
	go func() {
		w.send(ctx, voiceDownMsg{Type: voiceDownState, State: voiceStateIdle}) // the queue is full
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("send returned although the queue was full and the context live: a frame was dropped")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("send stayed blocked after the context ended")
	}
}

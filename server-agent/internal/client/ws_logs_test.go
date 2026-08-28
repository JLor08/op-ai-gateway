// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// TestLogWatchFramePayloadDelivered proves the gateway->agent
// runtime_log_config command reaches LogWatchUpdates() byte-for-byte. Without
// this direction there is no way to ask an agent for logs at all, which is why
// the POST transport -- which has no server->agent channel and therefore no
// LogWatchUpdates -- can never stream them.
func TestLogWatchFramePayloadDelivered(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up

	g.push(t, []byte(`{"type":"runtime_log_config","data":{"spec_ids":["spec-a","spec-b"]}}`))
	select {
	case got := <-s.LogWatchUpdates():
		if string(got) != `{"spec_ids":["spec-a","spec-b"]}` {
			t.Fatalf("LogWatchUpdates() = %s, want the frame's exact data field", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the runtime_log_config frame did not reach LogWatchUpdates()")
	}
}

// TestLogWatchHasNoConnectWake is a deliberate ASYMMETRY with
// RuntimeUpdates(), and it is worth pinning because the obvious "make it look
// like its sibling" edit would break it.
//
// A nil on RuntimeUpdates() means "resync over HTTP" -- a safe, idempotent
// fallback for a DOCUMENT the agent can re-fetch. A command has no such
// fallback: there is nothing to re-fetch, so a connect-hook nil could only
// mean "watch nothing", and it would race readLoop's first real command for
// the same latest-wins slot. The gateway restates the authoritative set on
// every new connection instead, so nothing has to be guessed here.
func TestLogWatchHasNoConnectWake(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up
	// The runtime-config channel DOES get its connect wake...
	drainInitialRuntimeConfigWake(t, s)
	// ...and the log-watch channel deliberately does not.
	select {
	case got := <-s.LogWatchUpdates():
		t.Fatalf("LogWatchUpdates() produced %s on connect; a command must come from the gateway, never be invented locally", got)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestLogWatchUpdatesLatestWins: each command states the FULL desired set, so
// a superseded one must never survive over a newer one -- the same drain-then-
// send discipline RuntimeUpdates() needs, for the same reason.
func TestLogWatchUpdatesLatestWins(t *testing.T) {
	g, up := newServerPusher()
	ts := httptest.NewServer(g.handler(up))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	<-up

	g.push(t, []byte(`{"type":"runtime_log_config","data":{"spec_ids":["old"]}}`))
	g.push(t, []byte(`{"type":"runtime_log_config","data":{"spec_ids":["new"]}}`))
	time.Sleep(300 * time.Millisecond)

	var got json.RawMessage
	select {
	case got = <-s.LogWatchUpdates():
	case <-time.After(time.Second):
		t.Fatal("expected a coalesced pending command after the burst")
	}
	if string(got) != `{"spec_ids":["new"]}` {
		t.Fatalf("latest-wins violated: got %s, want the newest command", got)
	}
	select {
	case extra := <-s.LogWatchUpdates():
		t.Fatalf("a two-command burst must coalesce into one, got a second: %s", extra)
	default:
	}
}

// frameCollector is a test gateway that records the typed frames the agent
// sends it, so a test can assert what actually went on the wire.
type frameCollector struct {
	mu     sync.Mutex
	frames []streamFrame
	seen   chan struct{}
}

func newFrameCollector() *frameCollector {
	return &frameCollector{seen: make(chan struct{}, 32)}
}

func (c *frameCollector) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var f streamFrame
			if err := wsjson.Read(r.Context(), conn, &f); err != nil {
				return
			}
			c.mu.Lock()
			c.frames = append(c.frames, f)
			c.mu.Unlock()
			select {
			case c.seen <- struct{}{}:
			default:
			}
		}
	}
}

func (c *frameCollector) byType(frameType string) []streamFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []streamFrame
	for _, f := range c.frames {
		if f.Type == frameType {
			out = append(out, f)
		}
	}
	return out
}

// TestPostRuntimeLogWritesTheFrameVerbatim: the payload is already-marshaled
// managed-process output, and this transport must put it on the wire
// byte-for-byte rather than re-marshaling something it does not understand.
func TestPostRuntimeLogWritesTheFrameVerbatim(t *testing.T) {
	c := newFrameCollector()
	ts := httptest.NewServer(c.handler())
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	payload := json.RawMessage(`{"spec_id":"spec-a","entries":[{"text":"hello\n"}]}`)
	if err := s.PostRuntimeLog(context.Background(), payload); err != nil {
		t.Fatalf("PostRuntimeLog: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		if got := c.byType("runtime_log"); len(got) == 1 {
			if string(got[0].Data) != string(payload) {
				t.Fatalf("runtime_log data = %s, want %s verbatim", got[0].Data, payload)
			}
			return
		}
		select {
		case <-c.seen:
		case <-deadline:
			t.Fatalf("no runtime_log frame arrived; frames seen: %+v", c.byType("runtime_log"))
		}
	}
}

// TestPostRuntimeLogIsNotResentOnReconnect is the one place this poster
// deliberately differs from PostSystemReport/PostRuntimeReport. Those cache
// their payload and restate it on every reconnect because they carry current
// STATE. A log frame carries output that has already happened; replaying it
// would duplicate history in front of the operator, and the agent's scrollback
// on the next subscribe is the one authoritative replay.
func TestPostRuntimeLogIsNotResentOnReconnect(t *testing.T) {
	c := newFrameCollector()
	ts := httptest.NewServer(c.handler())
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()
	s.cleanMin, s.cleanMax = time.Millisecond, 2*time.Millisecond
	s.backoffBase, s.backoffCap = time.Millisecond, 2*time.Millisecond

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := s.PostRuntimeLog(context.Background(), json.RawMessage(`{"spec_id":"a","entries":[]}`)); err != nil {
		t.Fatalf("PostRuntimeLog: %v", err)
	}
	// Force a reconnect and drive a fresh Post over the new connection, which
	// is what triggers the cached-report resends.
	s.dropConn(s.currentConn(), false)
	deadline := time.Now().Add(3 * time.Second)
	for s.currentConn() == nil {
		if err := s.Post(context.Background(), wsTestSample()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("never reconnected")
		}
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	if got := c.byType("runtime_log"); len(got) != 1 {
		t.Fatalf("runtime_log frames = %d, want exactly the one that was posted (no resend on reconnect)", len(got))
	}
}

// TestPostRuntimeLogWithNoConnectionDoesNotDial: log output is worth nothing
// once it is late, and the command that asked for it only exists on a live
// connection. Unlike the report posters this must not open one.
func TestPostRuntimeLogWithNoConnectionDoesNotDial(t *testing.T) {
	var dials int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		dials++
		mu.Unlock()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		_ = conn.CloseNow()
	}))
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.PostRuntimeLog(context.Background(), json.RawMessage(`{"spec_id":"a","entries":[]}`)); err == nil {
		t.Fatal("PostRuntimeLog with no connection returned nil, want the backing-off sentinel")
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 0 {
		t.Fatalf("PostRuntimeLog dialled %d times; a log frame must never open a connection", dials)
	}
}

// TestPostRuntimeLogEmptyPayloadIsANoOp: Drain returning nothing must not
// produce an empty frame on the wire.
func TestPostRuntimeLogEmptyPayloadIsANoOp(t *testing.T) {
	c := newFrameCollector()
	ts := httptest.NewServer(c.handler())
	defer ts.Close()
	s := newTestWSSender(t, wsHTTPToWS(ts.URL))
	defer s.Close()

	if err := s.Post(context.Background(), wsTestSample()); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := s.PostRuntimeLog(context.Background(), nil); err != nil {
		t.Fatalf("PostRuntimeLog(nil) = %v, want nil", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := c.byType("runtime_log"); len(got) != 0 {
		t.Fatalf("an empty drain produced %d frames, want none", len(got))
	}
}

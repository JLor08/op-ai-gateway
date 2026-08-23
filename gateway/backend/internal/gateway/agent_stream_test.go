// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func wsURL(httpURL, path string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http") + path
}

// dialAgentStream dials the stream endpoint with a bearer secret.
func dialAgentStream(t *testing.T, ctx context.Context, url, secret string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	hdr := http.Header{}
	if secret != "" {
		hdr.Set("Authorization", "Bearer "+secret)
	}
	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: hdr})
}

func TestAgentStreamIngestsTelemetryFrame(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	frame := streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":42},"gpus":[{"index":0,"name":"RTX","util_pct":10,"mem_used_bytes":1,"mem_total_bytes":2}]}`)}
	if err := wsjson.Write(ctx, conn, frame); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case s := <-ch:
		if s.ServerID != "mock-host-qwen" || s.CPUUtilPct != 42 {
			t.Fatalf("fanned = %+v", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no fanned-out sample")
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func TestAgentStreamSkipsBadFrameKeepsConnection(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Invalid payload (negative cpu_util_pct) -> skipped, connection stays open.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":-1}}`)}); err != nil {
		t.Fatalf("write bad: %v", err)
	}
	// A valid frame after the bad one still ingests.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":7}}`)}); err != nil {
		t.Fatalf("write good: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 7 {
			t.Fatalf("cpu = %v, want 7 (bad frame must have been skipped)", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection dropped after a bad frame")
	}
}

// TestAgentStreamUnknownFrameTypeKeepsConnection covers the forward-compat default
// branch in handleAgentStream's read-loop switch (f.Type not "telemetry"): an
// unrecognized frame is silently ignored (Debug-logged only), the connection is
// NOT closed, and a subsequent valid telemetry frame still ingests normally.
func TestAgentStreamUnknownFrameTypeKeepsConnection(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// An unknown frame type is a no-op: not ingested, and does not close the stream.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "ping-of-the-future", Data: json.RawMessage(`{"anything":true}`)}); err != nil {
		t.Fatalf("write unknown: %v", err)
	}
	// A valid telemetry frame right after it still ingests -- proving the connection
	// (and the read loop) survived the unknown type.
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":13}}`)}); err != nil {
		t.Fatalf("write telemetry: %v", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 13 {
			t.Fatalf("cpu = %v, want 13 (telemetry after an unknown frame type must still ingest)", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("connection dropped or telemetry not ingested after an unknown frame type")
	}
}

func TestAgentStreamRejectsMissingBearer(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	_, resp, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "")
	if err == nil {
		t.Fatal("dial succeeded without a bearer; want handshake rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401 (no upgrade)", resp)
	}
}

// TestAgentStreamRejectsUnknownBearer covers the OTHER unauthorized branch in
// handleAgentStream's auth prologue: a syntactically valid, NON-EMPTY bearer that
// does not match any registered agent token (LookupAgentToken ok=false) is rejected
// with 401 before any upgrade -- distinct from TestAgentStreamRejectsMissingBearer,
// which covers the no-bearer-at-all branch.
func TestAgentStreamRejectsUnknownBearer(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	_, resp, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "not-a-registered-secret")
	if err == nil {
		t.Fatal("dial succeeded with an unregistered bearer; want handshake rejection")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %v, want 401 (no upgrade)", resp)
	}
}

func TestAgentStreamCloseOnServerMismatch(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	_ = wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"server_id":"other-host","host":{"cpu_util_pct":1}}`)})
	_, _, readErr := conn.Read(ctx)
	if websocket.CloseStatus(readErr) != websocket.StatusPolicyViolation {
		t.Fatalf("close status = %v, want PolicyViolation; err=%v", websocket.CloseStatus(readErr), readErr)
	}
}

func TestAgentStreamGracefulShutdownSendsGoingAway(t *testing.T) {
	srv := NewTestServer()
	baseCtx, cancel := context.WithCancel(context.Background())
	srv.SetBaseContext(baseCtx)
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	ts := httptest.NewServer(srv)
	defer ts.Close()
	conn, _, err := dialAgentStream(t, context.Background(), wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	cancel() // simulate SIGTERM: cancel the server shutdown context.
	// Bounded so a watcher regression (no GoingAway ever sent) fails fast and
	// locally instead of hanging until the package's overall test timeout.
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	_, _, readErr := conn.Read(readCtx)
	if websocket.CloseStatus(readErr) != websocket.StatusGoingAway {
		t.Fatalf("close status = %v, want GoingAway; err=%v", websocket.CloseStatus(readErr), readErr)
	}
}

func TestAgentStreamListenerContextCancelClosesOnlyThatListenersStream(t *testing.T) {
	srv := NewTestServer()
	// The process stays alive throughout: only one listener's child context is
	// cancelled, exactly as an agent-listener rebind does.
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws_listener", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()

	listenerCtx, cancelListener := context.WithCancel(context.Background())
	mesh := httptest.NewUnstartedServer(srv.AgentHandler())
	mesh.Config.BaseContext = func(net.Listener) context.Context { return listenerCtx }
	mesh.Start()
	defer mesh.Close()
	public := httptest.NewServer(srv)
	defer public.Close()

	meshConn, _, err := dialAgentStream(t, context.Background(), wsURL(mesh.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial mesh stream: %v", err)
	}
	defer meshConn.CloseNow()
	publicConn, _, err := dialAgentStream(t, context.Background(), wsURL(public.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial public stream: %v", err)
	}
	defer publicConn.CloseNow()

	cancelListener()
	readCtx, readCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer readCancel()
	_, _, readErr := meshConn.Read(readCtx)
	if websocket.CloseStatus(readErr) != websocket.StatusGoingAway {
		t.Fatalf("mesh close status = %v, want GoingAway after listener rebind; err=%v", websocket.CloseStatus(readErr), readErr)
	}

	// Cancelling the mesh listener must not touch a stream accepted by the public
	// listener: its request context has a different, still-live base context.
	if err := wsjson.Write(context.Background(), publicConn, streamFrame{
		Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":17}}`),
	}); err != nil {
		t.Fatalf("public stream was closed by mesh listener cancel: %v", err)
	}
	select {
	case sample := <-ch:
		if sample.CPUUtilPct != 17 {
			t.Fatalf("public sample cpu = %v, want 17", sample.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("public stream stopped ingesting after mesh listener cancel")
	}
}

func TestAgentStreamSurvivesPastServerReadTimeout(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	_, ch, unsub := srv.ServerPerf.subscribe("mock-host-qwen")
	defer unsub()
	// A short server ReadTimeout that hijack must clear for a long-lived WS.
	ts := httptest.NewUnstartedServer(srv)
	ts.Config.ReadTimeout = 250 * time.Millisecond
	ts.Config.WriteTimeout = 250 * time.Millisecond
	ts.Start()
	defer ts.Close()

	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()

	// Idle well past the ReadTimeout (no frames, no pings in this window), then write.
	time.Sleep(600 * time.Millisecond)
	if err := wsjson.Write(ctx, conn, streamFrame{Type: "telemetry", Data: json.RawMessage(`{"host":{"cpu_util_pct":9}}`)}); err != nil {
		t.Fatalf("write after idle: %v (connection was killed by ReadTimeout)", err)
	}
	select {
	case s := <-ch:
		if s.CPUUtilPct != 9 {
			t.Fatalf("cpu = %v, want 9", s.CPUUtilPct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame not ingested after idle; connection likely killed by ReadTimeout")
	}
}

func TestAgentStreamRegisteredOnAgentMux(t *testing.T) {
	srv := NewTestServer()
	srv.SetBaseContext(context.Background())
	seedTestAgentToken(t, srv, "agt_ws", "mock-host-qwen", "agent-secret")
	ts := httptest.NewServer(srv.AgentHandler())
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := dialAgentStream(t, ctx, wsURL(ts.URL, "/api/agent/v1/stream"), "agent-secret")
	if err != nil {
		t.Fatalf("dial via agent mux: %v", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

func TestWriteStreamFrame(t *testing.T) {
	// Exercises the future gateway->agent write helper end-to-end.
	type payload struct {
		Msg string `json:"msg"`
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer c.CloseNow()
		_ = writeStreamFrame(r.Context(), c, "hello", payload{Msg: "hi"})
	})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	ctx := context.Background()
	conn, _, err := websocket.Dial(ctx, wsURL(ts.URL, "/"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.CloseNow()
	var f streamFrame
	if err := wsjson.Read(ctx, conn, &f); err != nil {
		t.Fatalf("read: %v", err)
	}
	if f.Type != "hello" {
		t.Fatalf("type = %q, want hello", f.Type)
	}
	var p payload
	if err := json.Unmarshal(f.Data, &p); err != nil || p.Msg != "hi" {
		t.Fatalf("data = %s (err %v)", f.Data, err)
	}
}

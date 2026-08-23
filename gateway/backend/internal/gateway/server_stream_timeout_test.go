// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// blockingStreamer emits one text delta, then blocks until its context is done,
// simulating an upstream that stalls with no further tokens.
type blockingStreamer struct{}

func (blockingStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (blockingStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "partial"}); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

// pacedStreamer emits n text deltas spaced by gap, then completes.
type pacedStreamer struct {
	n   int
	gap time.Duration
}

func (pacedStreamer) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p pacedStreamer) CompleteStream(ctx context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	for i := 0; i < p.n; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.gap):
		}
		if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "tok"}); err != nil {
			return err
		}
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: p.n, TotalTokens: 1 + p.n}})
}

// trickleReader releases its pieces one Read at a time, sleeping gap before each, so the
// request body takes len(pieces)*gap to arrive on the wire.
type trickleReader struct {
	pieces []string
	gap    time.Duration
}

func (tr *trickleReader) Read(p []byte) (int, error) {
	if len(tr.pieces) == 0 {
		return 0, io.EOF
	}
	time.Sleep(tr.gap)
	n := copy(p, tr.pieces[0])
	tr.pieces[0] = tr.pieces[0][n:]
	if tr.pieces[0] == "" {
		tr.pieces = tr.pieces[1:]
	}
	return n, nil
}

func streamRequest() *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	return req
}

// A stalled upstream ends the stream with an in-band idle-timeout error frame.
func TestOpenAIChatStreamIdleTimeoutFires(t *testing.T) {
	srv := newStreamTestServerWithProviderAndIdle(blockingStreamer{}, 40*time.Millisecond)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, streamRequest())

	body := rec.Body.String()
	sawIdle := false
	for _, chunk := range sseDataChunks(t, body) {
		if errObj, ok := chunk["error"].(map[string]any); ok {
			if code, _ := errObj["code"].(string); code == "provider.stream_idle_timeout" {
				sawIdle = true
			}
		}
	}
	if !sawIdle {
		t.Fatalf("missing provider.stream_idle_timeout frame, body = %s", body)
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 || events[0].ErrorCode != "provider.stream_idle_timeout" || events[0].Status != "error" {
		t.Fatalf("usage = %#v, want one error event coded provider.stream_idle_timeout", events)
	}
}

// Activity within the idle window keeps the stream alive to completion.
func TestOpenAIChatStreamIdleTimeoutResets(t *testing.T) {
	// 5 tokens, 12ms apart (~60ms total) under a 60ms idle window: each token resets
	// the watchdog, so the full stream completes with no error frame. The window is
	// held comfortably above the per-token gap to avoid CI flakiness under load.
	srv := newStreamTestServerWithProviderAndIdle(pacedStreamer{n: 5, gap: 12 * time.Millisecond}, 60*time.Millisecond)
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, streamRequest())

	body := rec.Body.String()
	if !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("missing terminal [DONE], body = %s", body)
	}
	if strings.Contains(body, "provider.stream_idle_timeout") {
		t.Fatalf("unexpected idle-timeout frame, body = %s", body)
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 || events[0].Status != "success" {
		t.Fatalf("usage = %#v, want one success event", events)
	}
}

// A disconnected client ends the stream silently (no idle frame) and is recorded
// as provider.client_disconnected.
func TestOpenAIChatStreamClientDisconnect(t *testing.T) {
	srv := newStreamTestServerWithProviderAndIdle(blockingStreamer{}, time.Hour) // watchdog won't fire
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := streamRequest().WithContext(ctx)

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond) // let the first delta emit, then "disconnect"
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}
	if strings.Contains(rec.Body.String(), "provider.stream_idle_timeout") {
		t.Fatalf("client disconnect must not emit an idle-timeout frame, body = %s", rec.Body.String())
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 || events[0].ErrorCode != "provider.client_disconnected" {
		t.Fatalf("usage = %#v, want provider.client_disconnected", events)
	}
}

// Over a real connection (where SetWriteDeadline is honored), the idle-timeout
// frame must still reach the client — the last chunk's write deadline must be
// re-armed before the terminal frame is written.
func TestOpenAIChatStreamIdleTimeoutFrameReachesClient(t *testing.T) {
	srv := newStreamTestServerWithProviderAndIdle(blockingStreamer{}, 40*time.Millisecond)
	httpSrv := &http.Server{Handler: srv}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	body := strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), "provider.stream_idle_timeout") {
		t.Fatalf("idle-timeout frame did not reach client over a real conn, body = %s", raw)
	}
}

// TestStreamSurvivesShortServerWriteTimeout: with the idle watchdog DISABLED (idle=0,
// so writeChunk's armWrite is a no-op), a ~360ms stream over an http.Server whose
// WriteTimeout is 100ms is delivered in full ONLY because liftInferenceDeadlines cleared
// the connection write deadline. Remove the lift and the 2nd chunk (~120ms) hits the
// elapsed 100ms deadline and the stream truncates.
func TestStreamSurvivesShortServerWriteTimeout(t *testing.T) {
	srv := newStreamTestServerWithProviderAndIdle(pacedStreamer{n: 6, gap: 60 * time.Millisecond}, 0) // idle=0: watchdog OFF
	httpSrv := &http.Server{Handler: srv, WriteTimeout: 100 * time.Millisecond}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	body := strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer dev-secret")
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body) // a truncated stream yields a read error or short body; assert on count below
	got := strings.Count(string(raw), `"content":"tok"`)
	if got != 6 {
		t.Fatalf("received %d token chunks, want 6 (stream truncated by 100ms WriteTimeout without the lift), body = %s", got, raw)
	}
	if !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("missing terminal [DONE], body = %s", raw)
	}
}

// TestSlowUploadSurvivesShortServerReadTimeout guards the READ half of the lift: a body
// that trickles in over ~600ms against an http.Server whose ReadTimeout is 150ms is read
// in full and returns 200 ONLY because liftInferenceDeadlines cleared the read deadline
// (readRawJSONUnlimited runs after the lift). Remove the lift and the body read is cut at
// ~150ms, the JSON decode fails, and the request does not return 200. Non-streaming
// (stream:false) so the exchange exercises the read path + a buffered 200.
func TestSlowUploadSurvivesShortServerReadTimeout(t *testing.T) {
	srv := newStreamTestServerWithProviderAndIdle(provider.NewMock(), 0)
	httpSrv := &http.Server{Handler: srv, ReadTimeout: 150 * time.Millisecond}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() { _ = httpSrv.Serve(ln) }()
	defer httpSrv.Close()

	body := &trickleReader{pieces: []string{
		`{"model":"qwen-coder","messages":[{"role":"user","content":"`,
		strings.Repeat("a", 2000),
		`x`,
		`"}],"stream":false}`,
	}, gap: 150 * time.Millisecond}
	req, _ := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/chat/completions", body)
	req.Header.Set("Authorization", "Bearer dev-secret")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request failed (slow upload cut by read deadline without the lift?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (slow upload cut without the lift), body = %s", resp.StatusCode, raw)
	}
}

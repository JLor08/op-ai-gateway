// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/provider"
	"strings"
	"testing"
	"time"
)

func TestStreamSuccessCapturesRawSSE(t *testing.T) {
	cs := &fakeCaptureStore{}
	// use the mock streaming provider
	srv := newCaptureTestServer(t, mockStreamer{}, cs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	saved, ok := cs.last()
	if !ok {
		t.Fatal("no streaming capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	// The tee copies every emitted frame verbatim -> capture == full client SSE.
	if env.RespBody != rec.Body.String() {
		t.Fatalf("captured SSE != client SSE:\n cap=%q\n cli=%q", env.RespBody, rec.Body.String())
	}
	if !strings.Contains(env.RespBody, "data: [DONE]") {
		t.Fatalf("captured SSE missing [DONE]: %q", env.RespBody)
	}
	if env.RespHeaders["Content-Type"][0] != "text/event-stream" {
		t.Fatalf("resp content-type not captured: %v", env.RespHeaders)
	}
	if env.Truncated {
		t.Fatal("small stream wrongly marked truncated")
	}
}

func TestStreamProviderErrorCapturesErrorFrame(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, erroringStreamer{}, cs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if !strings.Contains(env.RespBody, "partial") {
		t.Fatalf("partial delta not captured: %q", env.RespBody)
	}
	if !strings.Contains(env.RespBody, "provider.unavailable") {
		t.Fatalf("error frame not captured: %q", env.RespBody)
	}
	if !strings.Contains(env.RespBody, "data: [DONE]") {
		t.Fatalf("[DONE] not captured: %q", env.RespBody)
	}
}

func TestStreamCaptureTruncates(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, pacedStreamer{n: 50, gap: 0}, cs)
	srv.captureMaxBytes = 64 // force truncation far below the full stream size
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	// The client still gets the full stream regardless of the capture cap.
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("client stream truncated (must not be): %s", rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	env := decryptCapture(t, saved.Blob)
	if !env.Truncated {
		t.Fatalf("capture not marked truncated (cap=64, resp=%d bytes)", len(env.RespBody))
	}
	if len(env.RespBody) > srv.captureMaxBytes {
		t.Fatalf("captured resp body %d exceeds cap %d", len(env.RespBody), srv.captureMaxBytes)
	}
}

func TestStreamCaptureRAMFallbackWhenCipherNil(t *testing.T) {
	// SP-C+ P4: a nil cipher is no longer fail-closed — it is the RAM-fallback
	// signal. The streaming path must still capture end to end, storing plain
	// gzip (KeyVersion 0, never sealed) instead of dropping the capture.
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, mockStreamer{}, cs)
	srv.Cipher = nil // RAM fallback: no key configured
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("stream should still complete: %s", rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no streaming capture saved in RAM fallback (nil cipher)")
	}
	if saved.KeyVersion != 0 {
		t.Fatalf("KeyVersion = %d, want 0 (RAM fallback, never sealed)", saved.KeyVersion)
	}
	// Blob is plain gzip (never AES-GCM sealed) — gunzip directly, no cipher.
	zr, err := gzip.NewReader(bytes.NewReader(saved.Blob))
	if err != nil {
		t.Fatalf("blob is not plain gzip (unexpected Seal?): %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	var env captureEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	// The tee copies every emitted frame verbatim -> capture == full client SSE.
	if env.RespBody != rec.Body.String() {
		t.Fatalf("captured resp body != client bytes:\n cap=%q\n cli=%q", env.RespBody, rec.Body.String())
	}
}

// The capture write must use a detached context: after a client disconnect
// r.Context() is canceled, yet SaveCapture must still run on a live context.
func TestStreamCaptureUsesDetachedContextOnDisconnect(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, blockingStreamer{}, cs) // emits one delta then blocks
	ctx, cancel := context.WithCancel(context.Background())
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer dev-secret")

	done := make(chan struct{})
	go func() {
		srv.ServeHTTP(rec, req)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel() // client disconnects; r.Context() is now canceled

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after disconnect")
	}

	saved, ok := cs.last()
	if !ok {
		t.Fatal("capture not saved after client disconnect")
	}
	if saved.UsageEventID == "" {
		t.Fatal("capture missing event id")
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	// persistCapture hands SaveCapture a bounded, detached context
	// (context.WithTimeout(context.Background(), 5s)); its `defer cancel()` cancels
	// that context once the synchronous write returns, so an after-the-fact Err() is
	// always non-nil and cannot distinguish detached from request-derived (matches
	// TestPersistCaptureUsesDetachedContext). The bounded ~5s deadline is the
	// observable proof the write used the detached context rather than the canceled
	// r.Context() (which carries no deadline).
	deadline, ok := cs.sawCtx[len(cs.sawCtx)-1].Deadline()
	if !ok {
		t.Fatal("capture used a context without a deadline; want the bounded detached (5s) context, not r.Context()")
	}
	if d := time.Until(deadline); d <= 0 || d > 5*time.Second {
		t.Fatalf("capture write deadline %v out of expected (0, 5s] bound (must be detached/live)", d)
	}
}

// mockStreamer is provider.NewMock() under a local name for streaming capture tests.
type mockStreamer = provider.Mock

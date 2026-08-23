// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"op-ai-gateway/internal/capture"
	"testing"
	"time"
)

// decryptCapture reverses Seal→gzip→marshal to inspect a stored envelope. Shared
// by the non-stream/stream capture tests.
func decryptCapture(t *testing.T, blob []byte) captureEnvelope {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	gzBlob, err := cipher.Open(blob)
	if err != nil {
		t.Fatalf("cipher.Open: %v", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gzBlob))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	plain, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip read: %v", err)
	}
	var env captureEnvelope
	if err := json.Unmarshal(plain, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	return env
}

func TestRedactCaptureHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("Authorization", "Bearer secret")
	h.Set("Cookie", "op_ai_gateway_session=abc")
	h.Set("X-OP-CSRF", "1")
	h.Set("X-OP-Run-As-Token", "tok_123")
	h.Set("Content-Type", "application/json")
	h.Add("X-Custom", "a")
	h.Add("X-Custom", "b")

	got := redactCaptureHeaders(h)

	// net/http canonicalizes X-OP-CSRF -> X-Op-Csrf etc.; redaction is
	// case-insensitive and REPLACES the value with a marker while keeping the key.
	for _, name := range []string{"Authorization", "Cookie", "X-Op-Csrf", "X-Op-Run-As-Token"} {
		vs, ok := got[name]
		if !ok {
			t.Fatalf("sensitive header %q must be kept with a redaction marker, got dropped: %v", name, got)
		}
		if len(vs) != 1 || vs[0] != "[redacted]" {
			t.Fatalf("sensitive header %q = %v, want [\"[redacted]\"]", name, vs)
		}
	}
	if got["Content-Type"][0] != "application/json" {
		t.Fatalf("Content-Type = %v, want preserved", got["Content-Type"])
	}
	if len(got["X-Custom"]) != 2 {
		t.Fatalf("X-Custom = %v, want two values preserved", got["X-Custom"])
	}
}

func TestBuildCaptureEnvelopeTruncates(t *testing.T) {
	in := &captureInput{
		ReqHeaders:  http.Header{"Authorization": {"Bearer x"}, "X-Keep": {"v"}},
		RawReq:      []byte("0123456789"),
		RespHeaders: http.Header{"Content-Type": {"application/json"}},
		RespBody:    []byte("abcdefghij"),
	}
	env := buildCaptureEnvelope(in, 4)
	if env.ReqBody != "0123" {
		t.Fatalf("ReqBody = %q, want 0123", env.ReqBody)
	}
	if env.RespBody != "abcd" {
		t.Fatalf("RespBody = %q, want abcd", env.RespBody)
	}
	if !env.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if v := env.ReqHeaders["Authorization"]; len(v) != 1 || v[0] != "[redacted]" {
		t.Fatalf("Authorization must be redacted to the marker, got %v", env.ReqHeaders["Authorization"])
	}
	if env.RespHeaders["Content-Type"][0] != "application/json" {
		t.Fatalf("resp Content-Type not preserved: %v", env.RespHeaders)
	}
}

func TestBuildCaptureEnvelopeNotTruncated(t *testing.T) {
	env := buildCaptureEnvelope(&captureInput{RawReq: []byte("hi"), RespBody: []byte("yo")}, 1024)
	if env.Truncated {
		t.Fatal("Truncated = true, want false for small bodies")
	}
}

func TestPersistCaptureRoundTrip(t *testing.T) {
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv := &Server{Captures: cs, Cipher: cipher, captureMaxBytes: defaultCaptureMaxBytes}

	srv.persistCapture("req_test_1", &captureInput{
		ReqHeaders:  http.Header{"Authorization": {"Bearer secret"}, "X-Keep": {"v"}},
		RawReq:      []byte(`{"model":"qwen-coder"}`),
		RespHeaders: http.Header{"Content-Type": {"application/json"}},
		RespBody:    []byte(`{"ok":true}`),
	})

	saved, ok := cs.last()
	if !ok {
		t.Fatal("SaveCapture not called")
	}
	if saved.UsageEventID != "req_test_1" {
		t.Fatalf("UsageEventID = %q, want req_test_1", saved.UsageEventID)
	}
	if saved.KeyVersion != capture.KeyVersion {
		t.Fatalf("KeyVersion = %d, want %d", saved.KeyVersion, capture.KeyVersion)
	}
	env := decryptCapture(t, saved.Blob)
	if env.ReqBody != `{"model":"qwen-coder"}` {
		t.Fatalf("ReqBody = %q", env.ReqBody)
	}
	if env.RespBody != `{"ok":true}` {
		t.Fatalf("RespBody = %q", env.RespBody)
	}
	if v := env.ReqHeaders["Authorization"]; len(v) != 1 || v[0] != "[redacted]" {
		t.Fatalf("Authorization not redacted to the marker in stored capture: %v", env.ReqHeaders["Authorization"])
	}
	if env.ReqHeaders["X-Keep"][0] != "v" {
		t.Fatalf("X-Keep not preserved: %v", env.ReqHeaders)
	}
}

func TestPersistCaptureSwallowsStoreError(t *testing.T) {
	cipher, _ := capture.New(testCaptureKey)
	cs := &fakeCaptureStore{saveErr: errors.New("disk full")}
	srv := &Server{Captures: cs, Cipher: cipher, captureMaxBytes: defaultCaptureMaxBytes}
	// Must not panic or propagate: persistCapture is fire-and-forget.
	srv.persistCapture("req_test_2", &captureInput{RawReq: []byte("x"), RespBody: []byte("y")})
	if _, ok := cs.last(); !ok {
		t.Fatal("SaveCapture should still have been attempted")
	}
}

func TestPersistCaptureUsesDetachedContext(t *testing.T) {
	cipher, _ := capture.New(testCaptureKey)
	cs := &fakeCaptureStore{}
	srv := &Server{Captures: cs, Cipher: cipher, captureMaxBytes: defaultCaptureMaxBytes}
	srv.persistCapture("req_test_3", &captureInput{RawReq: []byte("x"), RespBody: []byte("y")})
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if len(cs.sawCtx) != 1 {
		t.Fatalf("SaveCapture calls = %d, want 1", len(cs.sawCtx))
	}
	// persistCapture must hand SaveCapture a bounded, detached context
	// (context.WithTimeout(context.Background(), 5s)) so a slow/hung write cannot
	// block the post-response path forever. A plain context.Background() has NO
	// deadline, so the presence of a ~5s deadline is exactly what distinguishes
	// the specified bounded write from an unbounded one. (Err() is intentionally
	// not asserted: persistCapture's `defer cancel()` legitimately cancels the
	// context once the write returns, so an after-the-fact Err() is always
	// non-nil and would not tell us whether the timeout was applied.)
	deadline, ok := cs.sawCtx[0].Deadline()
	if !ok {
		t.Fatal("capture write context has no deadline; want a bounded (5s) detached context")
	}
	if d := time.Until(deadline); d <= 0 || d > 5*time.Second {
		t.Fatalf("capture write deadline %v out of expected (0, 5s] bound (must be detached/live)", d)
	}
}

func TestPersistCapturePlainGzipWhenNoCipher(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := &Server{Captures: cs, Cipher: nil, captureMaxBytes: defaultCaptureMaxBytes}

	srv.persistCapture("req_test_ram", &captureInput{
		RawReq:      []byte(`{"model":"qwen-coder"}`),
		RespBody:    []byte(`{"ok":true}`),
		OwnerUserID: "usr_dev",
		APIFlavor:   "openai_chat_completions",
		HTTPStatus:  200,
	})

	saved, ok := cs.last()
	if !ok {
		t.Fatal("SaveCapture not called")
	}
	if saved.KeyVersion != 0 {
		t.Fatalf("KeyVersion = %d, want 0 (RAM fallback, no seal)", saved.KeyVersion)
	}
	if saved.OwnerUserID != "usr_dev" || saved.APIFlavor != "openai_chat_completions" || saved.HTTPStatus != 200 {
		t.Fatalf("owner/flavor/status = %q/%q/%d", saved.OwnerUserID, saved.APIFlavor, saved.HTTPStatus)
	}
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
		t.Fatalf("unmarshal: %v", err)
	}
	if env.ReqBody != `{"model":"qwen-coder"}` || env.RespBody != `{"ok":true}` {
		t.Fatalf("envelope = %#v", env)
	}
}

func TestPersistCaptureFillsOwnerFlavorStatusWithCipher(t *testing.T) {
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	srv := &Server{Captures: cs, Cipher: cipher, captureMaxBytes: defaultCaptureMaxBytes}

	srv.persistCapture("req_test_owner", &captureInput{
		RawReq:      []byte("x"),
		RespBody:    []byte("y"),
		OwnerUserID: "usr_owner",
		APIFlavor:   "anthropic_messages",
		HTTPStatus:  201,
	})

	saved, ok := cs.last()
	if !ok {
		t.Fatal("SaveCapture not called")
	}
	if saved.OwnerUserID != "usr_owner" || saved.APIFlavor != "anthropic_messages" || saved.HTTPStatus != 201 {
		t.Fatalf("owner/flavor/status = %q/%q/%d", saved.OwnerUserID, saved.APIFlavor, saved.HTTPStatus)
	}
	if saved.KeyVersion != capture.KeyVersion {
		t.Fatalf("KeyVersion = %d, want %d (sealed path)", saved.KeyVersion, capture.KeyVersion)
	}
}

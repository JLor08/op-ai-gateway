// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// newCaptureTestServer wires a fake capture store + a real cipher and a bearer
// token whose LogCommunication flag is set, so the capture pipeline runs end to
// end. Shared by the non-stream and stream capture tests.
func newCaptureTestServer(t *testing.T, prov provider.Client, captures CaptureStore) *Server {
	t.Helper()
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID:               "tok_dev",
		UserID:           "usr_dev",
		Name:             "Dev Token",
		Active:           true,
		Scopes:           []string{"gateway:use", "admin"},
		LogCommunication: true,
	}, "dev-secret")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
		Captures: captures,
		Cipher:   cipher,
	})
}

// erroringComplete fails on the non-streaming Complete path.
type erroringComplete struct{}

func (erroringComplete) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, fmt.Errorf("%w: boom", provider.ErrUnavailable)
}

func TestNonStreamSuccessCapture(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, provider.NewMock(), cs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("X-Trace", "keep-me")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	// The dev token carries no secret flag, so the capture is non-secret.
	if saved.Secret {
		t.Fatalf("saved.Secret = true, want false (dev token has no secret flag)")
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 || saved.UsageEventID != events[0].ID {
		t.Fatalf("capture id %q != usage event id", saved.UsageEventID)
	}
	env := decryptCapture(t, saved.Blob)
	if !strings.Contains(env.ReqBody, `"content":"hello"`) {
		t.Fatalf("req body not captured: %q", env.ReqBody)
	}
	if !strings.Contains(env.RespBody, "Mock response for qwen-coder: hello") {
		t.Fatalf("resp body not captured: %q", env.RespBody)
	}
	// resp_body must be the exact client bytes (serialize-once).
	if env.RespBody != rec.Body.String() {
		t.Fatalf("captured resp body != client bytes:\n cap=%q\n cli=%q", env.RespBody, rec.Body.String())
	}
	if env.ReqHeaders["X-Trace"][0] != "keep-me" {
		t.Fatalf("non-sensitive header dropped: %v", env.ReqHeaders)
	}
	if v := env.ReqHeaders["Authorization"]; len(v) != 1 || v[0] != "[redacted]" {
		t.Fatalf("Authorization not redacted to the marker in capture: %v", env.ReqHeaders["Authorization"])
	}
	if env.RespHeaders["Content-Type"][0] != "application/json" {
		t.Fatalf("resp content-type not captured: %v", env.RespHeaders)
	}
}

func TestNonStreamProviderErrorCapture(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, erroringComplete{}, cs)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body = %s", rec.Code, rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved on error path")
	}
	env := decryptCapture(t, saved.Blob)
	if !strings.Contains(env.RespBody, "provider.unavailable") {
		t.Fatalf("error body not captured: %q", env.RespBody)
	}
	if env.RespBody != rec.Body.String() {
		t.Fatalf("captured error body != client bytes")
	}
}

func TestNonStreamCaptureInheritsTokenSecret(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, provider.NewMock(), cs)
	// A secret-flagged token (LogCommunication on so the capture runs).
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_secret", UserID: "usr_dev", Name: "Secret", Active: true, Scopes: []string{"gateway:use", "admin"}, LogCommunication: true, Secret: true}, "secret-secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer secret-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if !saved.Secret {
		t.Fatalf("saved.Secret = false, want true (inherited from the token secret flag at write time)")
	}
}

func TestNonStreamNoCaptureWhenFlagOff(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, provider.NewMock(), cs)
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_off", UserID: "usr_dev", Name: "Off", Active: true, Scopes: []string{"gateway:use", "admin"}, LogCommunication: false}, "off-secret")
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer off-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := cs.last(); ok {
		t.Fatal("capture written despite LogCommunication=false")
	}
}

func TestNonStreamNoCaptureWhenGloballyDisabled(t *testing.T) {
	cs := &fakeCaptureStore{}
	srv := newCaptureTestServer(t, provider.NewMock(), cs)
	srv.CaptureEnabled = func() bool { return false }
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if _, ok := cs.last(); ok {
		t.Fatal("capture written despite CaptureEnabled hook returning false")
	}
}

func TestNonStreamSuccessCaptureRAMFallback(t *testing.T) {
	// RAM fallback: NO cipher wired, a real in-RAM MemoryCaptureStore as Captures.
	// The write path must still capture end to end — KeyVersion 0, plain gzip,
	// readable back through Captures.Capture — instead of failing closed.
	mem := store.NewMemoryCaptureStore(0)
	tokens := auth.NewTokenStore()
	tokens.AddPlainToken(auth.Token{
		ID:               "tok_dev",
		UserID:           "usr_dev",
		Name:             "Dev Token",
		Active:           true,
		Scopes:           []string{"gateway:use", "admin"},
		LogCommunication: true,
	}, "dev-secret")
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory := portal.NewMemoryDirectory(auth.NewTokenStore())
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	seedGatewayTestRoutes(routeStore, now)
	srv := New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: provider.NewMock(),
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock(), Captures: mem}),
		Captures: mem,
		Cipher:   nil, // RAM fallback: no cipher configured.
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want exactly 1", len(events))
	}
	id := events[0].ID

	row, err := mem.Capture(context.Background(), id)
	if err != nil {
		t.Fatalf("Capture(%q): %v", id, err)
	}
	if row.KeyVersion != 0 {
		t.Fatalf("KeyVersion = %d, want 0 (RAM fallback, never sealed)", row.KeyVersion)
	}
	// Blob is plain gzip (never AES-GCM sealed) — gunzip directly, no cipher.
	zr, err := gzip.NewReader(bytes.NewReader(row.Blob))
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
	if !strings.Contains(env.ReqBody, `"content":"hello"`) {
		t.Fatalf("req body not captured: %q", env.ReqBody)
	}
	// resp_body must be the exact client bytes (serialize-once).
	if env.RespBody != rec.Body.String() {
		t.Fatalf("captured resp body != client bytes:\n cap=%q\n cli=%q", env.RespBody, rec.Body.String())
	}
}

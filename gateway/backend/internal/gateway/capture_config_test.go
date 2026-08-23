// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/store"
	"sync"
	"testing"
)

// fakeCaptureStore is the shared in-memory CaptureStore double for the phase-4
// tests: it records every SaveCapture call, the context it was handed, and an
// optional injected error. (P3's server_test.go defines a separate trivial
// stubCaptureStore for its nil-guard test; the two names do not collide.)
type fakeCaptureStore struct {
	mu      sync.Mutex
	saved   []store.Capture
	sawCtx  []context.Context
	saveErr error
}

func (f *fakeCaptureStore) SaveCapture(ctx context.Context, c store.Capture) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sawCtx = append(f.sawCtx, ctx)
	f.saved = append(f.saved, c)
	return f.saveErr
}

func (f *fakeCaptureStore) last() (store.Capture, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.saved) == 0 {
		return store.Capture{}, false
	}
	return f.saved[len(f.saved)-1], true
}

// testCaptureKey is 64 hex chars == 32 bytes (AES-256).
const testCaptureKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var _ CaptureStore = (*store.MemoryCaptureStore)(nil)

func TestCapturingEnabledGate(t *testing.T) {
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	cases := []struct {
		name  string
		srv   *Server
		token auth.Token
		want  bool
	}{
		{"all set", &Server{Captures: cs, Cipher: cipher}, auth.Token{LogCommunication: true}, true},
		{"flag off", &Server{Captures: cs, Cipher: cipher}, auth.Token{LogCommunication: false}, false},
		{"no store", &Server{Cipher: cipher}, auth.Token{LogCommunication: true}, false},
		{"no cipher (RAM fallback, still captures)", &Server{Captures: cs}, auth.Token{LogCommunication: true}, true},
		{"capture_enabled hook off", &Server{Captures: cs, Cipher: cipher, CaptureEnabled: func() bool { return false }}, auth.Token{LogCommunication: true}, false},
		{"capture_enabled hook on", &Server{Captures: cs, Cipher: cipher, CaptureEnabled: func() bool { return true }}, auth.Token{LogCommunication: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.srv.capturingEnabled(tc.token); got != tc.want {
				t.Fatalf("capturingEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCapturingEnabledOverrideGate(t *testing.T) {
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	cs := &fakeCaptureStore{}
	yes := func(v bool) func() bool { return func() bool { return v } }
	cases := []struct {
		name            string
		captureEnabled  bool
		captureOverride bool
		logComm         bool
		withStore       bool
		want            bool
	}{
		{"enabled, no override, opted-in", true, false, true, true, true},
		{"enabled, no override, opted-out", true, false, false, true, false},
		{"enabled, override forces opted-out on", true, true, false, true, true},
		{"kill switch beats override", false, true, true, true, false},
		{"override on but no store", true, true, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{Cipher: cipher, CaptureEnabled: yes(tc.captureEnabled), CaptureOverride: yes(tc.captureOverride)}
			if tc.withStore {
				srv.Captures = cs
			}
			if got := srv.capturingEnabled(auth.Token{LogCommunication: tc.logComm}); got != tc.want {
				t.Fatalf("capturingEnabled = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewDefaultsCaptureMaxBytes(t *testing.T) {
	if srv := New(ServerDeps{}); srv.captureMaxBytes != defaultCaptureMaxBytes {
		t.Fatalf("captureMaxBytes = %d, want default %d", srv.captureMaxBytes, defaultCaptureMaxBytes)
	}
	if srv := New(ServerDeps{CaptureMaxBytes: 42}); srv.captureMaxBytes != 42 {
		t.Fatalf("captureMaxBytes = %d, want 42", srv.captureMaxBytes)
	}
}

func TestCaptureEnabledDefaultsToTrueWhenHookNil(t *testing.T) {
	srv := &Server{}
	if !srv.captureEnabled() {
		t.Fatalf("captureEnabled() = false, want true when CaptureEnabled hook is nil (default-on)")
	}
}

func TestCaptureEnabledUsesHookWhenSet(t *testing.T) {
	srv := &Server{CaptureEnabled: func() bool { return false }}
	if srv.captureEnabled() {
		t.Fatalf("captureEnabled() = true, want false — the hook returned false")
	}
}

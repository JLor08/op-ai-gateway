// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// upstreamAuthTestKey is a 32-byte (64 hex char) test encryption key.
var upstreamAuthTestKey = strings.Repeat("ab", 32)

// TestUpstreamAuthCtxSealedTokenDecrypts proves a sealed (enc:) per-app token is
// decrypted and attached to the ctx as the default "Authorization: Bearer" credential.
func TestUpstreamAuthCtxSealedTokenDecrypts(t *testing.T) {
	cipher, err := capture.New(upstreamAuthTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	sealed, err := capture.SealSecret(cipher, false, "sk-9")
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	if !strings.HasPrefix(sealed, "enc:") {
		t.Fatalf("expected an enc: sealed token, got %q", sealed)
	}
	s := &Server{Cipher: cipher}
	ctx := s.upstreamAuthCtx(context.Background(), routing.Target{APIToken: sealed})
	auth, ok := provider.UpstreamAuthFrom(ctx)
	if !ok || auth.Token != "sk-9" {
		t.Fatalf("UpstreamAuthFrom = %+v ok=%v, want token sk-9", auth, ok)
	}
	if auth.Header != "" {
		t.Fatalf("Header = %q, want empty (default Bearer)", auth.Header)
	}
}

// TestUpstreamAuthCtxCustomHeader proves a custom header name rides the ctx.
func TestUpstreamAuthCtxCustomHeader(t *testing.T) {
	cipher, err := capture.New(upstreamAuthTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	sealed, _ := capture.SealSecret(cipher, false, "sk-7")
	s := &Server{Cipher: cipher}
	ctx := s.upstreamAuthCtx(context.Background(), routing.Target{APIToken: sealed, APITokenHeader: "x-api-key"})
	auth, ok := provider.UpstreamAuthFrom(ctx)
	if !ok || auth.Token != "sk-7" || auth.Header != "x-api-key" {
		t.Fatalf("UpstreamAuthFrom = %+v ok=%v, want token sk-7 header x-api-key", auth, ok)
	}
}

// TestUpstreamAuthCtxEmptyTokenUnchanged proves an unauthenticated app threads nothing.
func TestUpstreamAuthCtxEmptyTokenUnchanged(t *testing.T) {
	s := &Server{Cipher: nil}
	base := context.Background()
	ctx := s.upstreamAuthCtx(base, routing.Target{APIToken: ""})
	if ctx != base {
		t.Fatal("empty token must return ctx unchanged")
	}
	if _, ok := provider.UpstreamAuthFrom(ctx); ok {
		t.Fatal("no auth expected for an empty token")
	}
}

// TestUpstreamAuthCtxEncWithoutCipherFailsOpen proves the fail-open path: an enc:
// value with no cipher (keyless disk / memory driver) decrypt-errors, so the ctx is
// returned unchanged, no header, no panic — the upstream will simply 401.
func TestUpstreamAuthCtxEncWithoutCipherFailsOpen(t *testing.T) {
	s := &Server{Cipher: nil}
	base := context.Background()
	ctx := s.upstreamAuthCtx(base, routing.Target{APIToken: "enc:not-decryptable"})
	if ctx != base {
		t.Fatal("fail-open: an enc token without a cipher must return ctx unchanged")
	}
	if _, ok := provider.UpstreamAuthFrom(ctx); ok {
		t.Fatal("no auth expected on the fail-open path")
	}
}

// TestUpstreamAuthCtxPlainTokenNoCipher proves the volatile (memory) store's plain:
// tokens open with a nil cipher (the memory driver passes nil).
func TestUpstreamAuthCtxPlainTokenNoCipher(t *testing.T) {
	s := &Server{Cipher: nil}
	ctx := s.upstreamAuthCtx(context.Background(), routing.Target{APIToken: "plain:sk-mem"})
	auth, ok := provider.UpstreamAuthFrom(ctx)
	if !ok || auth.Token != "sk-mem" {
		t.Fatalf("UpstreamAuthFrom = %+v ok=%v, want token sk-mem", auth, ok)
	}
}

// TestUpstreamAuthCtxReachesUpstreamComplete is a gateway->provider end-to-end: a
// resolved target carrying a sealed token, decrypted via upstreamAuthCtx, sends the
// decrypted Bearer credential to the upstream on a real Complete call.
func TestUpstreamAuthCtxReachesUpstreamComplete(t *testing.T) {
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer up.Close()

	cipher, err := capture.New(upstreamAuthTestKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	sealed, _ := capture.SealSecret(cipher, false, "sk-9")
	s := &Server{Cipher: cipher}
	target := routing.Target{Provider: "openai", Endpoint: up.URL, Model: "m", APIToken: sealed}

	client := provider.NewOpenAICompatibleClient(up.Client())
	ctx := s.upstreamAuthCtx(context.Background(), target)
	req := inference.Request{
		Model:    "m",
		Messages: []inference.Message{{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}}},
	}
	if _, err := client.Complete(ctx, target, req); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if gotAuth != "Bearer sk-9" {
		t.Fatalf("upstream Authorization = %q, want Bearer sk-9", gotAuth)
	}
}

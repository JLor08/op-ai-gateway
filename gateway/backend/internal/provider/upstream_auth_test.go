// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package provider

import (
	"context"
	"net/http"
	"testing"
)

func TestApplyUpstreamAuthDefaultBearer(t *testing.T) {
	ctx := WithUpstreamAuth(context.Background(), "", "sk-1")
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	applyUpstreamAuth(ctx, req)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-1" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer sk-1")
	}
}

func TestApplyUpstreamAuthCustomHeader(t *testing.T) {
	ctx := WithUpstreamAuth(context.Background(), "x-api-key", "sk-2")
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	applyUpstreamAuth(ctx, req)
	if got := req.Header.Get("x-api-key"); got != "sk-2" {
		t.Fatalf("x-api-key = %q, want %q", got, "sk-2")
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("custom header must not also set Authorization, got %q", got)
	}
}

func TestApplyUpstreamAuthEmptyTokenNoHeader(t *testing.T) {
	// An empty token leaves ctx unchanged and sets no header.
	base := context.Background()
	ctx := WithUpstreamAuth(base, "x-api-key", "   ")
	if ctx != base {
		t.Fatalf("empty token should leave ctx unchanged")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	applyUpstreamAuth(ctx, req)
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Fatalf("no header expected for empty token, got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("no Authorization expected for empty token, got %q", got)
	}
}

// TestApplyUpstreamAuthExtraHeadersWithBearer proves a subscription-shaped
// context (bearer via empty header + static extra headers) attaches BOTH the
// Authorization bearer and every extra header.
func TestApplyUpstreamAuthExtraHeadersWithBearer(t *testing.T) {
	extra := map[string]string{"anthropic-version": "2023-06-01", "anthropic-beta": "oauth-2025-04-20"}
	ctx := WithUpstreamAuthHeaders(context.Background(), "", "oauth-access", extra)
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	applyUpstreamAuth(ctx, req)
	if got := req.Header.Get("Authorization"); got != "Bearer oauth-access" {
		t.Fatalf("Authorization = %q, want Bearer oauth-access", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q", got)
	}
	if got := req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q", got)
	}
}

// TestApplyUpstreamAuthExtraHeadersOnly proves a context carrying ONLY extra
// headers (no token) still attaches them and sets no credential header.
func TestApplyUpstreamAuthExtraHeadersOnly(t *testing.T) {
	base := context.Background()
	ctx := WithUpstreamAuthHeaders(base, "", "", map[string]string{"anthropic-beta": "oauth-2025-04-20"})
	if ctx == base {
		t.Fatal("extra headers should be carried even with an empty token")
	}
	if _, ok := UpstreamAuthFrom(ctx); !ok {
		t.Fatal("UpstreamAuthFrom should be ok when extra headers are present")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://example/", nil)
	applyUpstreamAuth(ctx, req)
	if got := req.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("no Authorization expected for an extra-headers-only context, got %q", got)
	}
}

// TestWithUpstreamAuthHeadersEmptyUnchanged proves an empty token with no extra
// headers leaves ctx untouched (the unauthenticated no-op).
func TestWithUpstreamAuthHeadersEmptyUnchanged(t *testing.T) {
	base := context.Background()
	if ctx := WithUpstreamAuthHeaders(base, "x-api-key", "   ", nil); ctx != base {
		t.Fatal("empty token + no extra headers must leave ctx unchanged")
	}
}

func TestUpstreamAuthFromOkSemantics(t *testing.T) {
	// Bare ctx → not ok.
	if _, ok := UpstreamAuthFrom(context.Background()); ok {
		t.Fatalf("bare ctx should not carry upstream auth")
	}
	// With a token → ok, trimmed header + raw token.
	ctx := WithUpstreamAuth(context.Background(), "  x-api-key  ", "sk-3")
	a, ok := UpstreamAuthFrom(ctx)
	if !ok {
		t.Fatalf("expected ok for a set token")
	}
	if a.Header != "x-api-key" {
		t.Fatalf("header should be trimmed: got %q", a.Header)
	}
	if a.Token != "sk-3" {
		t.Fatalf("token = %q, want %q", a.Token, "sk-3")
	}
}

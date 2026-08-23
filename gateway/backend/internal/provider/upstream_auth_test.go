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

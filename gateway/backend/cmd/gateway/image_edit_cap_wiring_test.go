// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/config"
	"strings"
	"testing"
	"time"
)

// The multipart read cap of POST /v1/images/edits is an operator setting
// (config.MaxImageEditBodyBytes, OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES). The
// gateway package defaults a ServerDeps value it was never handed, so a missed
// wiring line in buildRuntime would leave every operator's setting silently
// ignored -- the endpoint would keep its default and nothing would fail. So this
// drives the real production wiring and asks the one thing an operator observes:
// is a body past THEIR cap refused, and one under it not.
func TestBuildGatewayServerHonoursTheConfiguredImageEditBodyCap(t *testing.T) {
	t.Setenv("OP_AI_GATEWAY_DEV_TOKEN", "dev-secret")
	orig := startAppHealthLoop
	startAppHealthLoop = func(*appHealthRunner, <-chan string) context.CancelFunc { return func() {} }
	t.Cleanup(func() { startAppHealthLoop = orig })

	srv, cleanup, err := buildGatewayServer(config.Config{
		Addr: "127.0.0.1:8080", DBDriver: "memory", AppHealthProbeTimeout: time.Second,
		MaxImageEditBodyBytes: 2048,
	})
	if err != nil {
		t.Fatalf("buildGatewayServer returned %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	post := func(size int) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/edits", bytes.NewReader(bytes.Repeat([]byte("x"), size)))
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		req.Header.Set("Authorization", "Bearer dev-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	over := post(4096)
	if over.Code != http.StatusRequestEntityTooLarge || !strings.Contains(over.Body.String(), "request.body_too_large") {
		t.Fatalf("a 4096-byte body under a 2048-byte cap: status = %d, body = %s, want the 413 request.body_too_large", over.Code, over.Body.String())
	}
	// Under the cap the size gate lets it through to the next check (this is not a
	// well-formed form, so that is a 400 -- but not a 413).
	under := post(1024)
	if under.Code == http.StatusRequestEntityTooLarge {
		t.Fatalf("a 1024-byte body under a 2048-byte cap was refused as too large: %s", under.Body.String())
	}
}

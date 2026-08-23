// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package main

import (
	"context"
	"net/http"
	"testing"
)

func TestNewHTTPServerUsesBaseContext(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKeyForTest{}, "sentinel")
	srv := newHTTPServer(ctx, ":0", http.NotFoundHandler())
	if srv.BaseContext == nil {
		t.Fatal("BaseContext is nil; graceful shutdown cannot cancel WS handlers")
	}
	got := srv.BaseContext(nil)
	if got.Value(ctxKeyForTest{}) != "sentinel" {
		t.Fatalf("BaseContext did not return the provided context")
	}
	if srv.ReadTimeout == 0 || srv.WriteTimeout == 0 {
		t.Fatalf("timeouts must be preserved (read=%v write=%v)", srv.ReadTimeout, srv.WriteTimeout)
	}
}

type ctxKeyForTest struct{}

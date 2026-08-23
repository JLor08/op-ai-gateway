// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
	"time"
)

// The usage event ID and every SSE chunk id must be the same hoisted value, and
// the frame-id format (req_...) must be unchanged.
func TestStreamSharesEventIDWithSSEChunks(t *testing.T) {
	srv := NewTestServer()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"ping"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()

	srv.ServeHTTP(rec, req)

	events := srv.Usage.ByUser("usr_dev")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	eventID := events[0].ID
	if !strings.HasPrefix(eventID, "req_") {
		t.Fatalf("event id %q lost the req_ frame-id format", eventID)
	}
	chunks := sseDataChunks(t, rec.Body.String())
	if len(chunks) == 0 {
		t.Fatal("no SSE chunks")
	}
	for _, chunk := range chunks {
		if chunk["id"] != eventID {
			t.Fatalf("chunk id = %v, want shared event id %q", chunk["id"], eventID)
		}
	}
}

// recordUsage stamps the event with the id it is given, not a freshly minted one.
func TestRecordUsageUsesPassedID(t *testing.T) {
	srv := NewTestServer()
	srv.recordUsage(time.Now(), auth.Token{UserID: "usr_x"}, inference.Request{Model: "m"}, routing.Target{}, provider.Response{}, "", "success", usageMeta{}, "req_explicit_id", nil)
	events := srv.Usage.ByUser("usr_x")
	if len(events) != 1 || events[0].ID != "req_explicit_id" {
		t.Fatalf("event = %#v, want ID req_explicit_id", events)
	}
}

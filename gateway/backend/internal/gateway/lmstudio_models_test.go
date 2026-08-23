// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"reflect"
	"strings"
	"testing"
)

func TestLMStudioModelsFromDTOs(t *testing.T) {
	in := []portal.ModelDTO{
		{ID: "loaded-model", ContextSize: 8192, Loaded: true},
		{ID: "cold-model", ContextSize: 4096, Loaded: false},
		{ID: "unknown-ctx", ContextSize: 0, Loaded: false},
		{ID: "loaded-unknown-ctx", ContextSize: 0, Loaded: true},
	}
	got := lmStudioModelsFromDTOs(in)

	if len(got) != 4 {
		t.Fatalf("len = %d, want 4", len(got))
	}
	want0 := map[string]any{
		"id": "loaded-model", "object": "model", "type": "llm", "state": "loaded",
		"max_context_length": 8192, "loaded_context_length": 8192,
	}
	if !reflect.DeepEqual(got[0], want0) {
		t.Fatalf("got[0] = %#v, want %#v", got[0], want0)
	}
	want1 := map[string]any{
		"id": "cold-model", "object": "model", "type": "llm", "state": "not-loaded",
		"max_context_length": 4096,
	}
	if !reflect.DeepEqual(got[1], want1) {
		t.Fatalf("got[1] = %#v, want %#v", got[1], want1)
	}
	if _, ok := got[2]["max_context_length"]; ok {
		t.Fatalf("unknown context must omit max_context_length: %#v", got[2])
	}
	if got[2]["state"] != "not-loaded" {
		t.Fatalf("got[2] state = %v", got[2]["state"])
	}
	want3 := map[string]any{
		"id": "loaded-unknown-ctx", "object": "model", "type": "llm", "state": "loaded",
	}
	if !reflect.DeepEqual(got[3], want3) {
		t.Fatalf("got[3] = %#v, want %#v", got[3], want3)
	}
	if _, ok := got[3]["max_context_length"]; ok {
		t.Fatalf("loaded model with unknown context must omit max_context_length: %#v", got[3])
	}
	if _, ok := got[3]["loaded_context_length"]; ok {
		t.Fatalf("loaded model with unknown context must omit loaded_context_length: %#v", got[3])
	}
}

func TestLMStudioModelsEndpointRequiresAuth(t *testing.T) {
	srv := newCaptureTestServer(t, provider.NewMock(), &fakeCaptureStore{})
	req := httptest.NewRequest(http.MethodGet, "/api/v0/models", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 401/403", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v0/models", nil)
	req2.Header.Set("Authorization", "Bearer dev-secret")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec2.Code, rec2.Body.String())
	}
	if !strings.Contains(rec2.Body.String(), `"object":"list"`) {
		t.Fatalf("body missing list envelope: %s", rec2.Body.String())
	}
}

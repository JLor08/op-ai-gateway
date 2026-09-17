// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postImages sends an images request through the real mux, the way
// active_requests_test.go drives the server.
func postImages(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// The endpoint runs the ONE existing gate. A model with no image verdict is
// refused with the capability code -- not served, and not mislabelled as an
// unknown model.
func TestImagesRefusesIncapableModel(t *testing.T) {
	srv := NewTestServer() // seedGatewayTestRoutes seeds qwen-coder with no image verdict

	rec := postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "routing.model_not_capable") {
		t.Fatalf("body = %s, want the capability code, not an unknown-model code", rec.Body.String())
	}
}

// An images request has no messages, so inference.Request.Validate would reject
// every one of them. The handler validates its own shape instead, and asserting
// the EXACT code (rather than only the 400 status, or only the absence of the
// chat validator's own code) is what proves validateImagesRequest itself ran --
// a 400 could otherwise come from any number of unrelated causes, and nothing
// on this path ever calls Validate to produce request.messages_required in the
// first place, so a negative check for it would never fail either way.
func TestImagesRejectsMissingPrompt(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"model":"qwen-coder","n":1}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "images.prompt_required") {
		t.Fatalf("body = %s, want images.prompt_required", rec.Body.String())
	}
}

// The routing model comes from our own mapping via the same tolerant JSON probe
// every native-passthrough endpoint uses. sd-server itself IGNORES the model
// field -- one process serves one model -- so a body with no model must not
// resolve to something arbitrary. Asserting the exact code (not just the 400
// status) is what pins this to validateImagesRequest's own model check rather
// than some other 400.
func TestImagesRejectsMissingModel(t *testing.T) {
	srv := NewTestServer()

	rec := postImages(t, srv, `{"prompt":"a cat"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a body with no model; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "images.model_required") {
		t.Fatalf("body = %s, want images.model_required", rec.Body.String())
	}
}

// The usage row carries the endpoint's own path. This exercises the REFUSAL
// path (qwen-coder has no image verdict, see TestImagesRefusesIncapableModel):
// relayImages records a usage event for that terminal rejection too, exactly
// like the admission-queue and endpoint-disabled branches in
// native_passthrough.go already do. On a resolve failure target is the zero
// value, so ProviderPath is "" here (upstreamPath returns "" immediately for
// an empty target.Provider) -- this test only pins ReqPath and confirms
// ProviderPath is NOT the chat default; it does NOT exercise upstreamPath's
// own apiFlavorImages branch (that never runs against a zero target), which
// is why TestUpstreamPathImagesFlavorBypassesModeAndProviderFallbacks in
// native_passthrough_test.go pins that branch directly instead.
func TestImagesUsageRowCarriesItsOwnPath(t *testing.T) {
	srv := NewTestServer()

	postImages(t, srv, `{"model":"qwen-coder","prompt":"a cat","n":1}`)

	events := srv.Usage.All()
	if len(events) == 0 {
		t.Fatal("the refusal recorded no usage event")
	}
	got := events[len(events)-1]
	if got.ReqPath != "/v1/images/generations" {
		t.Fatalf("ReqPath = %q, want /v1/images/generations", got.ReqPath)
	}
	if got.ProviderPath == "/v1/chat/completions" {
		t.Fatal("ProviderPath fell through to the chat default: upstreamPath needs its own case")
	}
}

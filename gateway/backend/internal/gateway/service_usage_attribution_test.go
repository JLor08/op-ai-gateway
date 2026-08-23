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

// TestRecordUsageAttributesServiceIDAndName proves recordUsage stamps the usage
// event with the resolved token's ServiceID/ServiceName (service accounts, Phase
// 1 §4.4) — the same attribution TokenID/TokenName already carry, sourced from
// the SAME token argument, so no extra store round-trip is needed.
func TestRecordUsageAttributesServiceIDAndName(t *testing.T) {
	srv := NewTestServer()
	tok := auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Nightly Batch", Kind: "service"}
	srv.recordUsage(time.Now(), tok, inference.Request{Model: "m"}, routing.Target{}, provider.Response{}, "", "success", usageMeta{}, "req_svc_attr", nil)

	events := srv.Usage.All()
	idx := -1
	for i, e := range events {
		if e.ID == "req_svc_attr" {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("event req_svc_attr not recorded; all events = %#v", events)
	}
	if got := events[idx]; got.ServiceID != "svc_1" || got.ServiceName != "Nightly Batch" {
		t.Fatalf("recorded ServiceID/ServiceName = %q/%q, want svc_1/Nightly Batch", got.ServiceID, got.ServiceName)
	}
}

// TestRecordUsageServiceAttributionEmptyForUserToken is the no-op-invariant
// regression: a plain user token (no Kind/ServiceID) must record ServiceID/
// ServiceName as "" — the overwhelming existing case, byte-identical to
// pre-feature behavior.
func TestRecordUsageServiceAttributionEmptyForUserToken(t *testing.T) {
	srv := NewTestServer()
	srv.recordUsage(time.Now(), auth.Token{ID: "tok_user", UserID: "usr_x"}, inference.Request{Model: "m"}, routing.Target{}, provider.Response{}, "", "success", usageMeta{}, "req_user_attr", nil)

	events := srv.Usage.ByUser("usr_x")
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if events[0].ServiceID != "" || events[0].ServiceName != "" {
		t.Fatalf("user-token event ServiceID/ServiceName = %q/%q, want empty/empty", events[0].ServiceID, events[0].ServiceName)
	}
}

// TestPortalUsageActiveIncludesServiceAttribution proves the /api/portal/usage/
// active DTO surfaces service_id/service_name for a service-attributed in-flight
// row, and that an ordinary (non-service) row still reads back empty — exercising
// the activeRequestDTO wiring independent of exactly which handler populated
// ActiveRequest.
func TestPortalUsageActiveIncludesServiceAttribution(t *testing.T) {
	srv, dir := newAuthTestServer(t)
	seedLoginUser(t, dir, "usr_adm", "adm@example.test", "password-1", "admin")

	base := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	srv.Active.Add(ActiveRequest{ID: "req_svc", ServiceID: "svc_1", ServiceName: "Nightly Batch", Model: "m1", APIFlavor: "openai", ReqPath: "/v1/chat/completions", StartedAt: base})
	srv.Active.Add(ActiveRequest{ID: "req_plain", UserID: "usr_adm", TokenID: "tok_p", TokenName: "Plain", Model: "m2", APIFlavor: "openai", ReqPath: "/v1/chat/completions", StartedAt: base.Add(time.Second)})

	admCookie := loginCookie(t, srv, "adm@example.test", "password-1")
	rows := getActive(t, srv, admCookie, "all")
	byID := map[string]activeRequestDTO{}
	for _, row := range rows {
		byID[row.ID] = row
	}
	if got := byID["req_svc"]; got.ServiceID != "svc_1" || got.ServiceName != "Nightly Batch" {
		t.Fatalf("req_svc DTO service fields = %q/%q, want svc_1/Nightly Batch (%#v)", got.ServiceID, got.ServiceName, got)
	}
	if got := byID["req_plain"]; got.ServiceID != "" || got.ServiceName != "" {
		t.Fatalf("req_plain (non-service) DTO service fields = %q/%q, want empty/empty (%#v)", got.ServiceID, got.ServiceName, got)
	}
}

// addServiceTokenToStreamServer seeds a service token (Kind=="service") directly
// into the token store backing a newStreamTestServerWithProvider server, mirroring
// addServiceToken (service_token_scope_test.go, Task 2) but for the blocking-
// provider test servers built in this package which don't expose their
// *auth.TokenStore separately — Server.Tokens is the SAME instance under the
// auth.BearerStore interface, so the concrete type assertion reaches it.
func addServiceTokenToStreamServer(t *testing.T, srv *Server, secret string, tok auth.Token) {
	t.Helper()
	ts, ok := srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("srv.Tokens is %T, want *auth.TokenStore", srv.Tokens)
	}
	tok.Kind = "service"
	if tok.Scopes == nil {
		tok.Scopes = []string{"llm:invoke"}
	}
	tok.Active = true
	ts.AddPlainToken(tok, secret)
}

// TestActiveRegistryTracksServiceTokenInFlightNonStream drives a real
// non-streaming inference request through the HTTP layer, authenticated with a
// service token, and proves the in-flight ActiveRequest carries the resolved
// token's ServiceID/ServiceName (server.go's non-stream Active.Add call site) —
// mirrors TestActiveRegistryTracksLiveRequest but for a service principal.
func TestActiveRegistryTracksServiceTokenInFlightNonStream(t *testing.T) {
	prov := newBlockingProvider()
	srv := newStreamTestServerWithProvider(prov)
	addServiceTokenToStreamServer(t, srv, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Nightly Batch"})

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer svc-secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		close(reqDone)
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider Complete was never entered")
	}

	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	if snap[0].ServiceID != "svc_1" || snap[0].ServiceName != "Nightly Batch" {
		t.Fatalf("in-flight row service attribution = %q/%q, want svc_1/Nightly Batch (%#v)", snap[0].ServiceID, snap[0].ServiceName, snap[0])
	}

	close(prov.release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish after release")
	}
	if snap := srv.Active.Snapshot(); len(snap) != 0 {
		t.Fatalf("post-completion snapshot = %d rows, want 0 (%#v)", len(snap), snap)
	}
}

// TestActiveRegistryTracksServiceTokenInFlightStream is the streaming-path sibling
// of TestActiveRegistryTracksServiceTokenInFlightNonStream (server.go's stream
// Active.Add call site).
func TestActiveRegistryTracksServiceTokenInFlightStream(t *testing.T) {
	prov := newBlockingProvider()
	srv := newStreamTestServerWithProvider(prov)
	addServiceTokenToStreamServer(t, srv, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Nightly Batch"})

	reqDone := make(chan struct{})
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/openai/v1/chat/completions",
			strings.NewReader(`{"model":"qwen-coder","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer svc-secret")
		rec := httptest.NewRecorder() // ResponseRecorder IS an http.Flusher
		srv.ServeHTTP(rec, req)
		close(reqDone)
	}()

	select {
	case <-prov.started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider CompleteStream was never entered")
	}

	snap := srv.Active.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("in-flight stream snapshot = %d rows, want 1 (%#v)", len(snap), snap)
	}
	if !snap[0].Stream {
		t.Fatalf("streaming path must register Stream=true: %#v", snap[0])
	}
	if snap[0].ServiceID != "svc_1" || snap[0].ServiceName != "Nightly Batch" {
		t.Fatalf("in-flight stream row service attribution = %q/%q, want svc_1/Nightly Batch (%#v)", snap[0].ServiceID, snap[0].ServiceName, snap[0])
	}

	close(prov.release)
	select {
	case <-reqDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stream request did not finish after release")
	}
	if snap := srv.Active.Snapshot(); len(snap) != 0 {
		t.Fatalf("post-stream snapshot = %d rows, want 0 (%#v)", len(snap), snap)
	}
}

// TestServiceTokenNativePassthroughAttributesUsage proves a service-token request
// served over native passthrough (native_passthrough.go's recordUsage call, which
// shares its Active.Add call site's same token-derived fields) still carries the
// ServiceID/ServiceName attribution through to the persisted usage event — the
// native path is a separate code path from the four translate-handler sites, so
// this closes the coverage gap the blocking-provider tests above (translate path
// only) leave open.
func TestServiceTokenNativePassthroughAttributesUsage(t *testing.T) {
	srv, tokens, recorder := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Nightly Batch"})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"native-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer svc-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	all := recorder.All()
	if len(all) != 1 {
		t.Fatalf("usage events = %d, want 1 (%#v)", len(all), all)
	}
	if all[0].ServiceID != "svc_1" || all[0].ServiceName != "Nightly Batch" {
		t.Fatalf("native-passthrough usage event service attribution = %q/%q, want svc_1/Nightly Batch (%#v)", all[0].ServiceID, all[0].ServiceName, all[0])
	}
}

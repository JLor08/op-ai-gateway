// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
	"time"
)

const windowsLocalModel = "gw-model"

// windowsProvider answers the vendor-Anthropic target with a scripted subscription
// response and every other target as a local model: a configurable status and header
// set on the native path, a plain completion on the translate path.
type windowsProvider struct {
	mu          sync.Mutex
	vendorHdr   http.Header
	localStatus int
	localHdr    http.Header
}

func (p *windowsProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{Text: "hi", Usage: inference.Usage{InputTokens: 1, OutputTokens: 1}}, nil
}

func (p *windowsProvider) CompleteStream(_ context.Context, _ routing.Target, _ inference.Request, emit provider.StreamEmit) error {
	if err := emit(inference.StreamEvent{Type: inference.StreamEventTextDelta, Text: "hi"}); err != nil {
		return err
	}
	return emit(inference.StreamEvent{Type: inference.StreamEventCompleted, Usage: &inference.Usage{InputTokens: 1, OutputTokens: 1}})
}

func (p *windowsProvider) ProxyNative(_ context.Context, target routing.Target, _ string, _ []byte, _ string) (*provider.ProxyResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	status, hdr := p.localStatus, p.localHdr
	if target.Provider == routing.ProviderVendorAnthropic {
		status, hdr = http.StatusOK, p.vendorHdr
	}
	h := hdr.Clone()
	if h == nil {
		h = http.Header{}
	}
	h.Set("Content-Type", jsonContentType)
	body := `{"id":"msg_1","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`
	if status >= 400 {
		body = `{"type":"error","error":{"type":"api_error","message":"boom"}}`
	}
	return &provider.ProxyResponse{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}, nil
}

type windowsFixture struct {
	srv  *Server
	prov *windowsProvider
}

// newWindowsFixture builds a gateway with one Anthropic subscription account owned by
// usr_va_a (served as subMessagesModel) and one local model (windowsLocalModel) whose
// Messages endpoint runs in localMode.
func newWindowsFixture(t *testing.T, localMode routing.EndpointMode) *windowsFixture {
	t.Helper()
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	prov := &windowsProvider{localStatus: http.StatusOK}
	srv.Provider = prov
	seedSubscriptionAnthropicRoutableAccount(t, routes, cipher, subMessagesAccountID, "usr_va_a", vendorauth.TokenSet{
		AccessToken: subMessagesAccessToken, RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct",
	})
	seedWindowsLocalModel(t, routes, localMode)
	enableVendorAccountsFlag(t, srv)
	return &windowsFixture{srv: srv, prov: prov}
}

func seedWindowsLocalModel(t *testing.T, routes *routing.MemoryStore, mode routing.EndpointMode) {
	t.Helper()
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(routes.CreateAIServer(ctx, routing.AIServer{ID: "srv-local", Name: "Local", Domain: "local.example.test", Provider: routing.ProviderVLLM, Endpoint: "http://local.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}))
	must(routes.CreateApplication(ctx, routing.Application{ID: "app-local", ServerID: "srv-local", Type: routing.ProviderServerAgent, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModeTranslate, MessagesMode: mode, CreatedAt: now, UpdatedAt: now}))
	must(routes.CreateMapping(ctx, routing.ModelMapping{ID: "route-local", ApplicationID: "app-local", GatewayModelName: windowsLocalModel, AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}))
	must(routes.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-local", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}))
}

func subscriptionWindowHeaders() http.Header {
	now := time.Now()
	h := windowHeaders(now, time.Hour, 24*time.Hour)
	h.Set("Retry-After", "30") // never to be re-attached
	return h
}

func (f *windowsFixture) post(t *testing.T, secret, path, model string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`
	if path == "/v1/chat/completions" {
		body = `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]`
	}
	if stream {
		body += `,"stream":true`
	}
	body += `}`
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.ServeHTTP(rec, req)
	return rec
}

func (f *windowsFixture) observeViaVendor(t *testing.T, secret string) {
	t.Helper()
	f.prov.vendorHdr = subscriptionWindowHeaders()
	if rec := f.post(t, secret, "/v1/messages", subMessagesModel, false); rec.Code != http.StatusOK {
		t.Fatalf("vendor request status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func requireWindowsAttached(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	if h.Get(rlp+"status") != "allowed" || h.Get(rlp+"5h-utilization") != "0.42" || h.Get(rlp+"7d-utilization") != "0.10" || h.Get(rlp+"5h-reset") == "" {
		t.Fatalf("subscription windows not attached: %v", h)
	}
	if h.Get("Retry-After") != "" {
		t.Fatalf("Retry-After attached: %v", h)
	}
}

func requireNoUnified(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), rlp) {
			t.Fatalf("unexpected unified header %s: %v", k, rec.Header())
		}
	}
}

func TestWindowsObservedFromVendorThenAttachedOnLocalPassthrough(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeViaVendor(t, vaOwnerSecret)

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("local status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireWindowsAttached(t, rec)

	other := f.post(t, vaOtherSecret, "/v1/messages", windowsLocalModel, false)
	if other.Code != http.StatusOK {
		t.Fatalf("other principal status = %d, body = %s", other.Code, other.Body.String())
	}
	requireNoUnified(t, other)
}

func TestWindowsAttachedOnTranslateStreamAndBuffered(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "buffered", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			f := newWindowsFixture(t, routing.EndpointModeTranslate)
			f.observeViaVendor(t, vaOwnerSecret)

			rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireWindowsAttached(t, rec)

			requireNoUnified(t, f.post(t, vaOtherSecret, "/v1/messages", windowsLocalModel, stream))
		})
	}
}

func TestWindowsNotAttachedOnError(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeViaVendor(t, vaOwnerSecret)
	f.prov.localStatus = http.StatusInternalServerError

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	requireNoUnified(t, rec)
}

func TestWindowsNotAttachedOverFreshHeaders(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeViaVendor(t, vaOwnerSecret)
	// The local upstream reports its own (different) figures; the gateway is the
	// owner of the account on that target, so they are relayed unmodified.
	own := http.Header{}
	own.Set(rlp+"status", "allowed_warning")
	own.Set(rlp+"5h-utilization", "0.91")
	f.prov.localHdr = own

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if got := rec.Header().Values(rlp + "status"); len(got) != 1 || got[0] != "allowed_warning" {
		t.Fatalf("status header = %v, want the upstream's own only", got)
	}
	if rec.Header().Get(rlp+"5h-utilization") != "0.91" {
		t.Fatalf("5h utilization = %q, want 0.91", rec.Header().Get(rlp+"5h-utilization"))
	}
	if rec.Header().Get(rlp+"7d-utilization") != "" {
		t.Fatalf("cached 7d window mixed into fresh headers: %v", rec.Header())
	}
}

func TestWindowsNotObservedForNonEntitled(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.srv.Portal = &usageManagerPortal{API: f.srv.Portal, ok: false}
	member := auth.Token{ID: "tok_va_b", UserID: "usr_va_b", Active: true}
	target := routing.Target{Provider: routing.ProviderVendorAnthropic, VendorAccountID: subMessagesAccountID, VendorAccountOwnerUserID: "usr_va_a"}

	f.srv.observeRateLimitWindows(member, target, subscriptionWindowHeaders())

	rec := f.post(t, vaOtherSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoUnified(t, rec)

	// Control: the same call for an entitled caller (the owner) is recorded.
	f.srv.observeRateLimitWindows(auth.Token{ID: "tok_va_a", UserID: "usr_va_a", Active: true}, target, subscriptionWindowHeaders())
	requireWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false))
}

func TestChatCompletionsGetNoWindows(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModeTranslate)
	f.observeViaVendor(t, vaOwnerSecret)
	for _, stream := range []bool{false, true} {
		rec := f.post(t, vaOwnerSecret, "/v1/chat/completions", windowsLocalModel, stream)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%v status = %d, body = %s", stream, rec.Code, rec.Body.String())
		}
		requireNoUnified(t, rec)
	}
}

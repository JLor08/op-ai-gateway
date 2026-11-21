// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

func rateLimitSrcHeaders() http.Header {
	h := http.Header{}
	h.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.82")
	h.Add("Anthropic-Ratelimit-Unified-Status", "allowed")
	h.Add("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
	h.Set("Retry-After", "37")
	h.Set("X-Should-Retry", "false")
	h.Set("Request-Id", "req_1")
	h.Set("Anthropic-Organization-Id", "org")
	h.Set("Anthropic-Ratelimit-Requests-Limit", "50")
	return h
}

func TestForwardRateLimitHeadersAllowlist(t *testing.T) {
	src := rateLimitSrcHeaders()

	full := http.Header{}
	forwardRateLimitHeaders(full, src, true)
	want := http.Header{}
	want.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.82")
	want.Add("Anthropic-Ratelimit-Unified-Status", "allowed")
	want.Add("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
	want.Set("Retry-After", "37")
	want.Set("X-Should-Retry", "false")
	if len(full) != len(want) {
		t.Fatalf("includeUnified=true copied %v, want exactly %v", full, want)
	}
	for k, vs := range want {
		got := full.Values(k)
		if strings.Join(got, ",") != strings.Join(vs, ",") {
			t.Fatalf("header %s = %v, want %v", k, got, vs)
		}
	}

	min := http.Header{}
	forwardRateLimitHeaders(min, src, false)
	if len(min) != 2 || min.Get("Retry-After") != "37" || min.Get("X-Should-Retry") != "false" {
		t.Fatalf("includeUnified=false copied %v, want only Retry-After and X-Should-Retry", min)
	}
}

type usageManagerPortal struct {
	portal.API
	mu    sync.Mutex
	calls int
	ok    bool
	err   error
}

func (p *usageManagerPortal) VendorAccountUsageManager(context.Context, auth.Token, string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.ok, p.err
}

func TestRateLimitFiguresEntitled(t *testing.T) {
	member := auth.Token{ID: "tok_m", UserID: "usr_m", Active: true}
	vendor := routing.Target{VendorAccountID: "acc_1", VendorAccountOwnerUserID: "usr_owner"}
	cases := []struct {
		name   string
		token  auth.Token
		target routing.Target
		portal *usageManagerPortal // nil = nil Portal
		want   bool
	}{
		{"self-hosted target", member, routing.Target{}, nil, true},
		{"owner", auth.Token{UserID: "usr_owner"}, vendor, nil, true},
		{"opt-in", member, routing.Target{VendorAccountID: "acc_1", VendorAccountOwnerUserID: "usr_owner", VendorShareUsageWithMembers: true}, nil, true},
		{"portal says true", member, vendor, &usageManagerPortal{ok: true}, true},
		{"portal says false", member, vendor, &usageManagerPortal{ok: false}, false},
		{"portal error", member, vendor, &usageManagerPortal{ok: true, err: errors.New("boom")}, false},
		{"nil portal", member, vendor, nil, false},
		{"empty user id never matches an empty owner", auth.Token{}, routing.Target{VendorAccountID: "acc_1"}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := &Server{}
			if tc.portal != nil {
				srv.Portal = tc.portal
			}
			if got := srv.rateLimitFiguresEntitled(context.Background(), tc.token, tc.target); got != tc.want {
				t.Fatalf("entitled = %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("second call within the TTL is cached", func(t *testing.T) {
		p := &usageManagerPortal{ok: true}
		srv := &Server{Portal: p}
		for i := 0; i < 3; i++ {
			if !srv.rateLimitFiguresEntitled(context.Background(), member, vendor) {
				t.Fatalf("call %d: not entitled", i)
			}
		}
		if p.calls != 1 {
			t.Fatalf("portal calls = %d, want 1", p.calls)
		}
		other := routing.Target{VendorAccountID: "acc_2", VendorAccountOwnerUserID: "usr_owner"}
		srv.rateLimitFiguresEntitled(context.Background(), member, other)
		if p.calls != 2 {
			t.Fatalf("portal calls after another account = %d, want 2 (key includes the account)", p.calls)
		}
	})
}

// headerProxyProvider answers each ProxyNative call from a script of (status, headers,
// body); the last answer repeats.
type headerAnswer struct {
	status int
	header http.Header
	body   string
}

type headerProxyProvider struct {
	mu      sync.Mutex
	answers []headerAnswer
}

func (p *headerProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *headerProxyProvider) CompleteStream(context.Context, routing.Target, inference.Request, provider.StreamEmit) error {
	return nil
}

func (p *headerProxyProvider) ProxyNative(context.Context, routing.Target, string, []byte, string) (*provider.ProxyResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.answers[0]
	if len(p.answers) > 1 {
		p.answers = p.answers[1:]
	}
	return &provider.ProxyResponse{StatusCode: a.status, Header: a.header.Clone(), Body: io.NopCloser(strings.NewReader(a.body))}, nil
}

const rlMessagesBody = `{"model":"gw-model","max_tokens":8,"messages":[{"role":"user","content":"hi"}]}`

func relayRateLimit(srv *Server, target routing.Target, flavor string, endpoint sessionEndpoint, rel func(*nativeRelay)) *httptest.ResponseRecorder {
	path := "/v1/messages"
	if endpoint == endpointResponses {
		path = "/v1/responses"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(rlMessagesBody))
	rec := httptest.NewRecorder()
	nr := nativeRelay{
		token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true},
		target:   target,
		path:     path,
		raw:      []byte(rlMessagesBody),
		pfReq:    inference.Request{Model: "gw-model", RequestedModel: "gw-model", APIFlavor: flavor},
		endpoint: endpoint,
	}
	if rel != nil {
		rel(&nr)
	}
	srv.proxyNative(rec, req, nr)
	return rec
}

func assertRateLimitHeaders(t *testing.T, rec *httptest.ResponseRecorder, unified bool) {
	t.Helper()
	h := rec.Header()
	if h.Get("Retry-After") != "37" || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("Retry-After/X-Should-Retry = %q/%q, want 37/false (headers %v)", h.Get("Retry-After"), h.Get("X-Should-Retry"), h)
	}
	if unified {
		if h.Get("Anthropic-Ratelimit-Unified-5h-Utilization") != "0.82" || len(h.Values("Anthropic-Ratelimit-Unified-Status")) != 2 {
			t.Fatalf("unified family missing: %v", h)
		}
	} else if len(h.Values("Anthropic-Ratelimit-Unified-5h-Utilization"))+len(h.Values("Anthropic-Ratelimit-Unified-Status")) != 0 {
		t.Fatalf("unified family forwarded to a non-entitled client: %v", h)
	}
	for _, name := range []string{"Request-Id", "Anthropic-Organization-Id", "Anthropic-Ratelimit-Requests-Limit"} {
		if v := h.Get(name); v != "" {
			t.Fatalf("%s = %q leaked to the client", name, v)
		}
	}
}

func TestNativeMessagesForwardsRateLimitHeaders(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		status                  int
	}{
		{"sse", "text/event-stream", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", 200},
		{"json", jsonContentType, `{"id":"msg_1","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, 200},
		{"429", jsonContentType, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := rateLimitSrcHeaders()
			h.Set("Content-Type", tc.contentType)
			prov := &headerProxyProvider{answers: []headerAnswer{{status: tc.status, header: h, body: tc.body}}}
			srv := newNativeProxyTestServer(prov, false, true)
			rec := relayRateLimit(srv, routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}, inference.APIFlavorAnthropicMessages, endpointMessages, nil)
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("status %d body %q, want %d %q", rec.Code, rec.Body.String(), tc.status, tc.body)
			}
			assertRateLimitHeaders(t, rec, true)
		})
	}
}

func TestNativeMessagesRateLimitHeadersNonEntitled(t *testing.T) {
	h := rateLimitSrcHeaders()
	h.Set("Content-Type", jsonContentType)
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 429, header: h, body: `{"type":"error"}`}}}
	srv := newNativeProxyTestServer(prov, false, true)
	srv.Portal = &usageManagerPortal{API: srv.Portal, ok: false}
	target := routing.Target{Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acc_x", VendorAccountOwnerUserID: "usr_other", ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
	rec := relayRateLimit(srv, target, inference.APIFlavorAnthropicMessages, endpointMessages, nil)
	assertRateLimitHeaders(t, rec, false)
}

func TestNativeResponsesDoesNotForwardRateLimitHeaders(t *testing.T) {
	h := rateLimitSrcHeaders()
	h.Set("Content-Type", jsonContentType)
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 200, header: h, body: `{"output_text":"hi"}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	rec := relayRateLimit(srv, routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}, inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	for _, name := range []string{"Retry-After", "X-Should-Retry", "Anthropic-Ratelimit-Unified-Status", "Anthropic-Ratelimit-Unified-5h-Utilization"} {
		if v := rec.Header().Get(name); v != "" {
			t.Fatalf("%s = %q forwarded on the Responses flavor", name, v)
		}
	}
}

// The instance-gone re-selection cannot be driven through the whole resolver with
// the existing helpers cheaply, so this pins the behaviour it protects: headers come
// only from the response written to the client (the final attempt), never from an
// earlier attempt the relay discarded.
func TestNativeMessagesReselectionDropsFirstAttemptHeaders(t *testing.T) {
	gone := http.Header{"Retry-After": []string{"99"}, "Content-Type": []string{jsonContentType}}
	final := http.Header{"Retry-After": []string{"1"}, "Content-Type": []string{jsonContentType}}
	first := &headerProxyProvider{answers: []headerAnswer{{status: 409, header: gone, body: `{"error":{"code":"runtime.instance_no_capacity","message":"full"}}`}}}
	second := &headerProxyProvider{answers: []headerAnswer{{status: 200, header: final, body: `{"id":"msg_1","type":"message","content":[]}`}}}
	srv := newNativeProxyTestServer(first, false, true)
	base := routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
	lost := base
	lost.InstanceKey = "inst#1"

	var rec *httptest.ResponseRecorder
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(rlMessagesBody))
	rec = httptest.NewRecorder()
	nr := nativeRelay{
		token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true},
		target:   lost,
		path:     "/v1/messages",
		raw:      []byte(rlMessagesBody),
		pfReq:    inference.Request{Model: "gw-model", RequestedModel: "gw-model", APIFlavor: inference.APIFlavorAnthropicMessages},
		endpoint: endpointMessages,
	}
	nr.reselect = func(routing.Target, error) {
		srv.Provider = second
		again := nr
		again.target = base
		again.reselect = nil
		srv.proxyNative(rec, req, again)
	}
	srv.proxyNative(rec, req, nr)

	if got := rec.Header().Values("Retry-After"); len(got) != 1 || got[0] != "1" {
		t.Fatalf("Retry-After = %v, want only the final attempt's 1", got)
	}
}

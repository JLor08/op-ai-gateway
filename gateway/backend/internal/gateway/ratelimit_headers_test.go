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
	"time"
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

type slowPortal struct {
	usageManagerPortal
}

func (p *slowPortal) VendorAccountUsageManager(ctx context.Context, _ auth.Token, _ string) (bool, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-ctx.Done()
	return true, ctx.Err()
}

func TestRateLimitFiguresEntitledFailureModes(t *testing.T) {
	member := auth.Token{ID: "tok_m", UserID: "usr_m", Active: true}
	vendor := routing.Target{VendorAccountID: "acc_1", VendorAccountOwnerUserID: "usr_owner"}

	t.Run("error cached within the negative TTL", func(t *testing.T) {
		p := &usageManagerPortal{ok: true, err: errors.New("boom")}
		srv := &Server{Portal: p}
		for i := 0; i < 3; i++ {
			if srv.rateLimitFiguresEntitled(context.Background(), member, vendor) {
				t.Fatalf("call %d entitled on a portal error", i)
			}
		}
		if p.calls != 1 {
			t.Fatalf("portal calls = %d, want 1 (error cached)", p.calls)
		}
	})

	t.Run("slow portal times out not entitled", func(t *testing.T) {
		p := &slowPortal{}
		srv := &Server{Portal: p}
		start := time.Now()
		if srv.rateLimitFiguresEntitled(context.Background(), member, vendor) {
			t.Fatal("entitled on a timed-out lookup")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("lookup took %v, want it bounded near %v", d, rateLimitEntitlementTimeout)
		}
		srv.rateLimitFiguresEntitled(context.Background(), member, vendor)
		if p.calls != 1 {
			t.Fatalf("portal calls = %d, want 1 (timeout cached)", p.calls)
		}
	})
}

func TestNativeMessagesWithoutUnifiedHeadersNeverConsultsPortal(t *testing.T) {
	h := http.Header{"Retry-After": []string{"37"}, "X-Should-Retry": []string{"false"}, "Content-Type": []string{jsonContentType}}
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 429, header: h, body: `{"type":"error"}`}}}
	srv := newNativeProxyTestServer(prov, false, true)
	p := &usageManagerPortal{API: srv.Portal, ok: true}
	srv.Portal = p
	target := routing.Target{Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acc_x", VendorAccountOwnerUserID: "usr_other", ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
	rec := relayRateLimit(srv, target, inference.APIFlavorAnthropicMessages, endpointMessages, nil)
	if p.calls != 0 {
		t.Fatalf("portal consulted %d times for a response with no unified headers", p.calls)
	}
	if rec.Header().Get("Retry-After") != "37" || rec.Header().Get("X-Should-Retry") != "false" {
		t.Fatalf("retry headers not forwarded: %v", rec.Header())
	}
}

func TestForwardRateLimitHeadersAllowlist(t *testing.T) {
	src := rateLimitSrcHeaders()

	full := http.Header{}
	forwardRateLimitHeaders(full, src, isUnifiedRateLimitHeader)
	want := http.Header{}
	want.Set("Anthropic-Ratelimit-Unified-5h-Utilization", "0.82")
	want.Add("Anthropic-Ratelimit-Unified-Status", "allowed")
	want.Add("Anthropic-Ratelimit-Unified-Status", "allowed_warning")
	want.Set("Retry-After", "37")
	want.Set("X-Should-Retry", "false")
	if len(full) != len(want) {
		t.Fatalf("gated unified copied %v, want exactly %v", full, want)
	}
	for k, vs := range want {
		got := full.Values(k)
		if strings.Join(got, ",") != strings.Join(vs, ",") {
			t.Fatalf("header %s = %v, want %v", k, got, vs)
		}
	}

	only := http.Header{}
	forwardRateLimitHeaders(only, src, nil)
	if len(only) != 2 || only.Get("Retry-After") != "37" || only.Get("X-Should-Retry") != "false" {
		t.Fatalf("nil gate copied %v, want only Retry-After and X-Should-Retry", only)
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
		token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true, SendUsageLimits: true},
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

func TestNativeResponsesDoesNotForwardUnifiedHeaders(t *testing.T) {
	h := rateLimitSrcHeaders()
	h.Set("Content-Type", jsonContentType)
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 200, header: h, body: `{"output_text":"hi"}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	rec := relayRateLimit(srv, routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}, inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	for _, name := range []string{"Anthropic-Ratelimit-Unified-Status", "Anthropic-Ratelimit-Unified-5h-Utilization"} {
		if v := rec.Header().Get(name); v != "" {
			t.Fatalf("%s = %q forwarded on the Responses flavor", name, v)
		}
	}
	if rec.Header().Get("Retry-After") != "37" {
		t.Fatalf("Retry-After = %q, want 37 relayed on the Responses flavor", rec.Header().Get("Retry-After"))
	}
}

func TestIsCodexRateLimitHeader(t *testing.T) {
	for _, n := range []string{
		"x-codex-primary-used-percent", "X-Codex-Secondary-Reset-At", "x-codex-primary-window-minutes",
		"x-codex-limit-name", "x-codex-other-primary-used-percent", "x-codex-other-limit-name",
		"x-codex-credits-has-credits", "x-codex-credits-unlimited", "x-codex-credits-balance",
		"x-codex-active-limit", "x-codex-rate-limit-reached-type", "x-codex-promo-message",
	} {
		if !isCodexRateLimitHeader(n) {
			t.Errorf("%s not in the family, want in", n)
		}
	}
	for _, n := range []string{
		"x-codex-turn-state", "x-codex-beta-features", "x-request-id", "x-ratelimit-remaining-requests",
		"x-other-primary-used-percent", "anthropic-ratelimit-unified-status", "retry-after", "",
	} {
		if isCodexRateLimitHeader(n) {
			t.Errorf("%s in the family, want out", n)
		}
	}
	if hasCodexRateLimitHeader(http.Header{"X-Codex-Turn-State": {"x"}}) {
		t.Error("turn-state alone counted as a family header")
	}
	if !hasCodexRateLimitHeader(http.Header{"x-codex-limit-name": {"x"}}) {
		t.Error("non-canonical family header not detected")
	}
}

func codexRateLimitSrcHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Codex-Primary-Used-Percent", "42.5")
	h.Set("X-Codex-Primary-Window-Minutes", "300")
	h.Set("X-Codex-Primary-Reset-At", "1795290566")
	h.Set("X-Codex-Secondary-Used-Percent", "17")
	h.Set("X-Codex-Credits-Balance", "0")
	h.Set("X-Codex-Turn-State", "ts_1")
	h.Set("X-Request-Id", "req_1")
	h.Set("X-Ratelimit-Remaining-Requests", "50")
	h.Set("Retry-After", "37")
	h.Set("X-Should-Retry", "false")
	h.Set("Anthropic-Ratelimit-Unified-Status", "allowed")
	return h
}

func assertCodexHeaders(t *testing.T, rec *httptest.ResponseRecorder, family bool) {
	t.Helper()
	h := rec.Header()
	if h.Get("Retry-After") != "37" || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("Retry-After/X-Should-Retry = %q/%q, want 37/false (headers %v)", h.Get("Retry-After"), h.Get("X-Should-Retry"), h)
	}
	want := map[string]string{
		"X-Codex-Primary-Used-Percent": "42.5", "X-Codex-Primary-Window-Minutes": "300",
		"X-Codex-Primary-Reset-At": "1795290566", "X-Codex-Secondary-Used-Percent": "17", "X-Codex-Credits-Balance": "0",
	}
	for k, v := range want {
		got, present := h[k]
		if family && (len(got) != 1 || got[0] != v) {
			t.Fatalf("%s = %v, want [%s] (headers %v)", k, got, v, h)
		}
		if !family && present {
			t.Fatalf("%s forwarded to a non-entitled client: %v", k, h)
		}
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Request-Id", "X-Ratelimit-Remaining-Requests", "Anthropic-Ratelimit-Unified-Status"} {
		if v := h.Get(name); v != "" {
			t.Fatalf("%s = %q leaked to the client", name, v)
		}
	}
}

func TestNativeResponsesForwardsCodexRateLimitHeaders(t *testing.T) {
	cases := []struct {
		name, contentType, body string
		status                  int
	}{
		{"sse", "text/event-stream", "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\"}}\n\n", 200},
		{"json", jsonContentType, `{"output_text":"hi"}`, 200},
		{"429", jsonContentType, `{"error":{"type":"rate_limit_error","message":"slow down"}}`, 429},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := codexRateLimitSrcHeaders()
			h.Set("Content-Type", tc.contentType)
			prov := &headerProxyProvider{answers: []headerAnswer{{status: tc.status, header: h, body: tc.body}}}
			srv := newNativeProxyTestServer(prov, true, false)
			rec := relayRateLimit(srv, routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}, inference.APIFlavorOpenAIResponses, endpointResponses, nil)
			if rec.Code != tc.status || rec.Body.String() != tc.body {
				t.Fatalf("status %d body %q, want %d %q", rec.Code, rec.Body.String(), tc.status, tc.body)
			}
			assertCodexHeaders(t, rec, true)
		})
	}
}

func codexVendorTarget(owner string) routing.Target {
	return routing.Target{Provider: routing.ProviderVendorOpenAISubscription, VendorAccountID: "acc_x", VendorAccountOwnerUserID: owner, ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
}

func TestNativeResponsesCodexHeadersNonEntitled(t *testing.T) {
	h := codexRateLimitSrcHeaders()
	h.Set("Content-Type", jsonContentType)
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 429, header: h, body: `{"error":{}}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	srv.Portal = &usageManagerPortal{API: srv.Portal, ok: false}
	rec := relayRateLimit(srv, codexVendorTarget("usr_other"), inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	assertCodexHeaders(t, rec, false)
}

func TestNativeResponsesCodexHeadersEntitledOwner(t *testing.T) {
	h := codexRateLimitSrcHeaders()
	h.Set("Content-Type", jsonContentType)
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 429, header: h, body: `{"error":{}}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	srv.Portal = &usageManagerPortal{API: srv.Portal, ok: false}
	rec := relayRateLimit(srv, codexVendorTarget("usr_dev"), inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	assertCodexHeaders(t, rec, true)
}

func TestNativeResponsesWithoutCodexHeadersNeverConsultsPortal(t *testing.T) {
	h := http.Header{"Retry-After": []string{"37"}, "X-Should-Retry": []string{"false"}, "X-Codex-Turn-State": []string{"ts"}, "Content-Type": []string{jsonContentType}}
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 429, header: h, body: `{"error":{}}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	p := &usageManagerPortal{API: srv.Portal, ok: true}
	srv.Portal = p
	rec := relayRateLimit(srv, codexVendorTarget("usr_other"), inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	if p.calls != 0 {
		t.Fatalf("portal consulted %d times for a response with no family headers", p.calls)
	}
	if rec.Header().Get("Retry-After") != "37" || rec.Header().Get("X-Should-Retry") != "false" {
		t.Fatalf("retry headers not forwarded: %v", rec.Header())
	}
	if rec.Header().Get("X-Codex-Turn-State") != "" {
		t.Fatalf("turn state leaked: %v", rec.Header())
	}
}

func TestNativeResponsesCodexHeaderValuesAndCase(t *testing.T) {
	h := http.Header{"Content-Type": []string{jsonContentType}}
	h["x-codex-primary-used-percent"] = []string{"9"} // direct assignment: non-canonical key
	h["X-Codex-Limit-Name"] = []string{"codex", "codex_other"}
	prov := &headerProxyProvider{answers: []headerAnswer{{status: 200, header: h, body: `{"output_text":"hi"}`}}}
	srv := newNativeProxyTestServer(prov, true, false)
	rec := relayRateLimit(srv, routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}, inference.APIFlavorOpenAIResponses, endpointResponses, nil)
	got := rec.Header()
	if v := got["X-Codex-Primary-Used-Percent"]; len(v) != 1 || v[0] != "9" {
		t.Fatalf("canonical used-percent = %v, want [9] (headers %v)", v, got)
	}
	if v := got["X-Codex-Limit-Name"]; len(v) != 2 || v[0] != "codex" || v[1] != "codex_other" {
		t.Fatalf("limit-name = %v, want both values", v)
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

func TestNativeResponsesReselectionDropsFirstAttemptHeaders(t *testing.T) {
	gone := http.Header{"Retry-After": []string{"99"}, "X-Codex-Primary-Used-Percent": []string{"99"}, "Content-Type": []string{jsonContentType}}
	final := http.Header{"Retry-After": []string{"1"}, "X-Codex-Primary-Used-Percent": []string{"5"}, "Content-Type": []string{jsonContentType}}
	first := &headerProxyProvider{answers: []headerAnswer{{status: 409, header: gone, body: `{"error":{"code":"runtime.instance_no_capacity","message":"full"}}`}}}
	second := &headerProxyProvider{answers: []headerAnswer{{status: 200, header: final, body: `{"output_text":"hi"}`}}}
	srv := newNativeProxyTestServer(first, false, true)
	base := routing.Target{ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
	lost := base
	lost.InstanceKey = "inst#1"

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(rlMessagesBody))
	rec := httptest.NewRecorder()
	nr := nativeRelay{
		token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true, SendUsageLimits: true},
		target:   lost,
		path:     "/v1/responses",
		raw:      []byte(rlMessagesBody),
		pfReq:    inference.Request{Model: "gw-model", RequestedModel: "gw-model", APIFlavor: inference.APIFlavorOpenAIResponses},
		endpoint: endpointResponses,
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
	if got := rec.Header().Values("X-Codex-Primary-Used-Percent"); len(got) != 1 || got[0] != "5" {
		t.Fatalf("X-Codex-Primary-Used-Percent = %v, want only the final attempt's 5", got)
	}
}

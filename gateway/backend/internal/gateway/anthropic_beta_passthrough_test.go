// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestMergeAnthropicBeta pins the pure merge: the order-stable, de-duplicated
// union of the target's static anthropic-beta (first) and the client's
// comma-separated tokens (in the order sent), whitespace-trimmed, with empty
// tokens dropped.
func TestMergeAnthropicBeta(t *testing.T) {
	cases := []struct {
		name   string
		static string
		client []string
		want   string
	}{
		{name: "both empty", static: "", client: nil, want: ""},
		{name: "empty client slice and blank static", static: "  ", client: []string{}, want: ""},
		{name: "empty static: just the client's", static: "", client: []string{"foo,bar"}, want: "foo,bar"},
		{name: "empty client: just the static", static: "oauth-2025-04-20", client: nil, want: "oauth-2025-04-20"},
		{name: "static first, then the client's", static: "oauth-2025-04-20", client: []string{"baz"}, want: "oauth-2025-04-20,baz"},
		{name: "client repeats the static: no duplicate", static: "oauth-2025-04-20", client: []string{"oauth-2025-04-20,baz"}, want: "oauth-2025-04-20,baz"},
		{name: "client sends the static last: still first once", static: "oauth-2025-04-20", client: []string{"baz, oauth-2025-04-20"}, want: "oauth-2025-04-20,baz"},
		{name: "order of the client's tokens is kept", static: "", client: []string{"zeta,alpha,mid"}, want: "zeta,alpha,mid"},
		{name: "duplicates inside the client header", static: "", client: []string{"a,b,a,c,b"}, want: "a,b,c"},
		{name: "whitespace around tokens is trimmed", static: " s1 ", client: []string{"  a , b\t,\tc  "}, want: "s1,a,b,c"},
		{name: "empty tokens are dropped", static: "", client: []string{",a,, ,b,"}, want: "a,b"},
		{name: "multiple header lines are merged in order", static: "", client: []string{"a,b", "c", "b,d"}, want: "a,b,c,d"},
		{name: "a static list is split too", static: "s1, s2", client: []string{"s2,c"}, want: "s1,s2,c"},
		{name: "tokens are case-sensitive", static: "", client: []string{"Foo,foo"}, want: "Foo,foo"},
		{name: "only separators and blanks", static: " , ", client: []string{" , ,", ""}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mergeAnthropicBeta(tc.static, tc.client); got != tc.want {
				t.Fatalf("mergeAnthropicBeta(%q, %q) = %q, want %q", tc.static, tc.client, got, tc.want)
			}
		})
	}
}

// TestAnthropicPassthroughHeaderOverrides pins the gate: a client anthropic-beta
// becomes an upstream override ONLY for an Anthropic passthrough target; an
// OpenAI target, an Anthropic translate target and a request without a beta
// header get none (nil: nothing changes).
func TestAnthropicPassthroughHeaderOverrides(t *testing.T) {
	hdr := http.Header{}
	hdr.Add("Anthropic-Beta", "foo")
	hdr.Add("Anthropic-Beta", "bar")
	anthropicPassthrough := routing.Target{Provider: routing.ProviderVendorAnthropic, MessagesMode: routing.EndpointModePassthrough}

	if got := anthropicPassthroughHeaderOverrides(anthropicPassthrough, hdr); !reflect.DeepEqual(got, map[string]string{"anthropic-beta": "foo,bar"}) {
		t.Fatalf("api-key anthropic passthrough: overrides = %v, want anthropic-beta=foo,bar", got)
	}

	// A subscription-shaped target: its static oauth beta is kept, the client's added.
	sub := anthropicPassthrough
	sub.ExtraHeaders = map[string]string{"anthropic-beta": "oauth-2025-04-20"}
	if got := anthropicPassthroughHeaderOverrides(sub, hdr); !reflect.DeepEqual(got, map[string]string{"anthropic-beta": "oauth-2025-04-20,foo,bar"}) {
		t.Fatalf("subscription-shaped anthropic passthrough: overrides = %v, want the union with the static oauth beta", got)
	}
	// ... and the static map is left exactly as it was.
	if !reflect.DeepEqual(sub.ExtraHeaders, map[string]string{"anthropic-beta": "oauth-2025-04-20"}) {
		t.Fatalf("the target's ExtraHeaders were mutated: %v", sub.ExtraHeaders)
	}

	for name, target := range map[string]routing.Target{
		"openai passthrough":         {Provider: routing.ProviderVendorOpenAI, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough},
		"openai subscription":        {Provider: routing.ProviderVendorOpenAISubscription, ResponsesMode: routing.EndpointModePassthrough},
		"self-hosted passthrough":    {Provider: routing.ProviderVLLM, MessagesMode: routing.EndpointModePassthrough},
		"anthropic translate":        {Provider: routing.ProviderVendorAnthropic, MessagesMode: routing.EndpointModeTranslate},
		"anthropic, no mode":         {Provider: routing.ProviderVendorAnthropic},
		"anthropic, endpoint closed": {Provider: routing.ProviderVendorAnthropic, MessagesMode: routing.EndpointModeDisabled},
	} {
		if got := anthropicPassthroughHeaderOverrides(target, hdr); got != nil {
			t.Fatalf("%s: overrides = %v, want none (the client's beta must not be forwarded there)", name, got)
		}
	}

	// No client beta (or one made only of blanks): nothing to forward, so the
	// request keeps exactly the static headers it had.
	for _, h := range []http.Header{nil, {}, {"Anthropic-Beta": {""}}, {"Anthropic-Beta": {" , "}}} {
		if got := anthropicPassthroughHeaderOverrides(sub, h); got != nil {
			t.Fatalf("client header %v: overrides = %v, want none", h, got)
		}
	}
}

// betaPassthroughHarness is a real *Server whose provider is the Multiplexer
// the daemon builds for vendor accounts (the real Anthropic and OpenAI-compatible
// clients, over the stub's HTTP client), so a request driven through proxyNative
// reaches the stub exactly as it would reach api.anthropic.com / api.openai.com.
type betaPassthroughHarness struct {
	srv    *Server
	routes *routing.MemoryStore
	cipher *capture.Cipher
	stub   *anthropicPlatformMessagesStub
}

func newBetaPassthroughHarness(t *testing.T) *betaPassthroughHarness {
	t.Helper()
	stub := newAnthropicPlatformMessagesStub(t)
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorAnthropic: provider.NewAnthropicClient(stub.srv.Client()),
		routing.ProviderVendorOpenAI:    provider.NewOpenAICompatibleClient(stub.srv.Client()),
	}, provider.NewMock())
	return &betaPassthroughHarness{srv: srv, routes: routes, cipher: cipher, stub: stub}
}

// relay drives the REAL proxyNative for target with the given client headers (the
// client's own bearer is deliberately in there: it must never go upstream) and
// returns the recorded client-facing response.
func (h *betaPassthroughHarness) relay(t *testing.T, target routing.Target, apiFlavor string, endpoint sessionEndpoint, raw string, clientHeader http.Header) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/messages"
	if apiFlavor == "openai_responses" {
		path = "/v1/responses"
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	for name, vals := range clientHeader {
		for _, v := range vals {
			req.Header.Add(name, v)
		}
	}
	rec := httptest.NewRecorder()
	h.srv.proxyNative(rec, req, nativeRelay{
		token:    auth.Token{ID: "tok_beta", UserID: "usr_va_a", Active: true},
		target:   target,
		path:     upstreamPath(target, apiFlavor),
		raw:      []byte(raw),
		pfReq:    inference.Request{Model: target.Model, RequestedModel: target.Model, APIFlavor: apiFlavor, Stream: true},
		endpoint: endpoint,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxyNative status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	return rec
}

func (h *betaPassthroughHarness) apiKeyAnthropicTarget(t *testing.T) routing.Target {
	t.Helper()
	target := resolveAPIKeyAnthropicTarget(t, h.cipher, "anthropic_messages", h.stub.srv.URL)
	if target.Provider != routing.ProviderVendorAnthropic || target.MessagesMode != routing.EndpointModePassthrough || target.Subscription {
		t.Fatalf("resolver-built target = %+v, want a non-subscription Anthropic Messages passthrough target", target)
	}
	return target
}

// subscriptionPassthroughTarget is the Anthropic SUBSCRIPTION target shape with
// the Messages passthrough mode on and an account holding a live OAuth token. The
// resolver does not produce this combination yet, so it is built by hand.
func (h *betaPassthroughHarness) subscriptionPassthroughTarget(t *testing.T) routing.Target {
	t.Helper()
	seedSubscriptionAccount(t, h.routes, h.cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
		AccessToken: "live-access", RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-1",
	})
	target := subscriptionTarget("acc_sub", h.stub.srv.URL)
	target.MessagesMode = routing.EndpointModePassthrough
	return target
}

func betaHeader(values ...string) http.Header {
	h := http.Header{}
	for _, v := range values {
		h.Add("anthropic-beta", v)
	}
	return h
}

// TestAnthropicAPIKeyPassthroughForwardsTheClientsBeta proves the client's
// anthropic-beta reaches the upstream through the real proxyNative on an api-key
// Messages passthrough, next to the opened x-api-key, exactly one gateway-pinned
// anthropic-version (the client's own version is NOT forwarded) and the body
// verbatim but for the model.
func TestAnthropicAPIKeyPassthroughForwardsTheClientsBeta(t *testing.T) {
	h := newBetaPassthroughHarness(t)
	target := h.apiKeyAnthropicTarget(t)

	client := betaHeader("foo, bar")
	client.Set("anthropic-version", "1999-01-01") // never forwarded

	rec := h.relay(t, target, "anthropic_messages", endpointMessages, apiKeyMessagesBody, client)

	assertAnthropicAPIKeyPassthroughWire(t, h.stub, rec.Body.Bytes(), apiKeyMessagesBody, "foo,bar")
	if got := h.stub.gotHeader.Values("anthropic-beta"); len(got) != 1 {
		t.Fatalf("anthropic-beta header lines = %q, want exactly one", got)
	}
}

// TestAnthropicAPIKeyPassthroughMergesRepeatedBetaHeaders: a client that sends
// the header on several lines (and repeats a token) still produces ONE upstream
// header carrying each token once, in the order sent.
func TestAnthropicAPIKeyPassthroughMergesRepeatedBetaHeaders(t *testing.T) {
	h := newBetaPassthroughHarness(t)
	rec := h.relay(t, h.apiKeyAnthropicTarget(t), "anthropic_messages", endpointMessages, apiKeyMessagesBody,
		betaHeader("interleaved-thinking-2025-05-14, foo", "foo", "bar"))

	assertAnthropicAPIKeyPassthroughWire(t, h.stub, rec.Body.Bytes(), apiKeyMessagesBody, "interleaved-thinking-2025-05-14,foo,bar")
}

// TestAnthropicAPIKeyPassthroughWithoutClientBetaSendsNone pins the unchanged
// behavior: a client that sends no anthropic-beta gets none upstream (an api-key
// target has no static one).
func TestAnthropicAPIKeyPassthroughWithoutClientBetaSendsNone(t *testing.T) {
	h := newBetaPassthroughHarness(t)
	rec := h.relay(t, h.apiKeyAnthropicTarget(t), "anthropic_messages", endpointMessages, apiKeyMessagesBody, nil)

	assertAnthropicAPIKeyPassthroughWire(t, h.stub, rec.Body.Bytes(), apiKeyMessagesBody, "")
	if _, present := h.stub.gotHeader["Anthropic-Beta"]; present {
		t.Fatalf("anthropic-beta present upstream: %q, want no such header", h.stub.gotHeader.Values("anthropic-beta"))
	}
}

// TestAnthropicSubscriptionShapedPassthroughMergesTheClientsBetaWithOAuth: a
// subscription target carries a static anthropic-beta: oauth-2025-04-20 (the OAuth
// opt-in its bearer needs). The client's tokens are unioned with it -- the static
// token kept, never duplicated -- while the OAuth bearer and the pinned
// anthropic-version still go out, and the shared target is left untouched.
func TestAnthropicSubscriptionShapedPassthroughMergesTheClientsBetaWithOAuth(t *testing.T) {
	cases := []struct {
		name   string
		client http.Header
		want   string
	}{
		{name: "client adds a token", client: betaHeader("baz"), want: "oauth-2025-04-20,baz"},
		{name: "client also sends the oauth token", client: betaHeader("oauth-2025-04-20, baz"), want: "oauth-2025-04-20,baz"},
		{name: "client sends the oauth token last", client: betaHeader("baz", "oauth-2025-04-20"), want: "oauth-2025-04-20,baz"},
		{name: "client sends nothing", client: nil, want: "oauth-2025-04-20"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBetaPassthroughHarness(t)
			target := h.subscriptionPassthroughTarget(t)
			staticBefore := copyStringMap(target.ExtraHeaders)

			h.relay(t, target, "anthropic_messages", endpointMessages, apiKeyMessagesBody, tc.client)

			if got := h.stub.gotHeader.Values("anthropic-beta"); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("anthropic-beta = %q, want exactly [%q]", got, tc.want)
			}
			if got := h.stub.gotHeader.Get("Authorization"); got != "Bearer live-access" {
				t.Fatalf("Authorization = %q, want the OAuth bearer (the client's own bearer must not go upstream)", got)
			}
			if got := h.stub.gotHeader.Values("anthropic-version"); len(got) != 1 || got[0] != "2023-06-01" {
				t.Fatalf("anthropic-version = %q, want exactly [2023-06-01]", got)
			}
			if !reflect.DeepEqual(target.ExtraHeaders, staticBefore) {
				t.Fatalf("the shared target's ExtraHeaders were mutated: %v, was %v", target.ExtraHeaders, staticBefore)
			}
		})
	}
}

// TestAnthropicPassthroughBetaOverrideReplacesAnyCaseOfTheStaticHeader: a target
// whose static header was stored under another spelling (Anthropic-Beta) must not
// end up with BOTH spellings in the ctx -- applyUpstreamAuth would Set them in map
// order and the upstream would see whichever came last. Exactly one merged value
// goes out.
func TestAnthropicPassthroughBetaOverrideReplacesAnyCaseOfTheStaticHeader(t *testing.T) {
	h := newBetaPassthroughHarness(t)
	target := h.subscriptionPassthroughTarget(t)
	target.ExtraHeaders = map[string]string{"anthropic-version": "2023-06-01", "Anthropic-Beta": "oauth-2025-04-20"}

	// Repeat to defeat map-iteration luck.
	for i := 0; i < 20; i++ {
		h.relay(t, target, "anthropic_messages", endpointMessages, apiKeyMessagesBody, betaHeader("baz"))
		if got := h.stub.gotHeader.Values("anthropic-beta"); len(got) != 1 || got[0] != "oauth-2025-04-20,baz" {
			t.Fatalf("run %d: anthropic-beta = %q, want exactly [oauth-2025-04-20,baz]", i, got)
		}
	}
	if !reflect.DeepEqual(target.ExtraHeaders, map[string]string{"anthropic-version": "2023-06-01", "Anthropic-Beta": "oauth-2025-04-20"}) {
		t.Fatalf("the shared target's ExtraHeaders were mutated: %v", target.ExtraHeaders)
	}
}

// TestOpenAIPassthroughNeverForwardsAnthropicBeta: the client's anthropic-beta is
// an Anthropic-only header. An OpenAI api-key Responses passthrough must not carry
// it upstream, whatever the client sent.
func TestOpenAIPassthroughNeverForwardsAnthropicBeta(t *testing.T) {
	h := newBetaPassthroughHarness(t)
	target := resolveAPIKeyOpenAITarget(t, h.cipher, "openai_responses", h.stub.srv.URL)
	if target.Provider != routing.ProviderVendorOpenAI || target.ResponsesMode != routing.EndpointModePassthrough {
		t.Fatalf("resolver-built target = %+v, want an OpenAI Responses passthrough target", target)
	}

	h.relay(t, target, "openai_responses", endpointResponses, apiKeyResponsesBody, betaHeader("foo, bar"))

	if h.stub.gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", h.stub.gotPath)
	}
	if got := h.stub.gotHeader.Values("anthropic-beta"); len(got) != 0 {
		t.Fatalf("anthropic-beta = %q reached an OpenAI target, want absent", got)
	}
	if got := h.stub.gotHeader.Get("Authorization"); got != "Bearer "+apiKeyPlainKey {
		t.Fatalf("Authorization = %q, want the opened OpenAI key (credentials unchanged)", got)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	apiKeyAnthropicAccountID = "acc_apikey_anthropic"
	apiKeyAnthropicPlainKey  = "sk-ant-test-opened-key"
	apiKeyAnthropicOwnerID   = "usr_apikey_anthropic_owner"
	apiKeyAnthropicModel     = "claude-sonnet"
	apiKeyAnthropicUpstream  = "claude-sonnet-4-5-20250929"
)

// seedAPIKeyAnthropicAccount stores one ACTIVE Anthropic api-key vendor account
// (the key sealed with cipher) serving apiKeyAnthropicModel -> apiKeyAnthropicUpstream
// in routes, owned by ownerID.
func seedAPIKeyAnthropicAccount(t *testing.T, routes *routing.MemoryStore, cipher *capture.Cipher, ownerID string, now time.Time) {
	t.Helper()
	sealed, err := capture.SealSecret(cipher, false, apiKeyAnthropicPlainKey)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	ctx := context.Background()
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: apiKeyAnthropicAccountID, OwnerUserID: ownerID, Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthAPIKey,
		Name: apiKeyAnthropicAccountID, Status: routing.VendorAccountStatusActive, APIKey: sealed, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routes.SetVendorAccountModels(ctx, apiKeyAnthropicAccountID, []routing.VendorAccountModel{
		{AccountID: apiKeyAnthropicAccountID, GatewayModel: apiKeyAnthropicModel, UpstreamModel: apiKeyAnthropicUpstream, APIFlavor: routing.APIFlavorAnthropic},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

// resolveAPIKeyAnthropicTarget resolves a request through the REAL routing.Resolver
// against a store holding one ACTIVE Anthropic api-key vendor account, and returns
// the target the dispatch layer would receive. endpoint replaces the target's
// Endpoint (a struct copy, so only the destination moves) so a test can point the
// otherwise production-shaped target at an httptest stub. Going through the
// resolver rather than hand-building the target keeps these tests honest about what
// production actually produces.
func resolveAPIKeyAnthropicTarget(t *testing.T, cipher *capture.Cipher, fineFlavor, endpoint string) routing.Target {
	t.Helper()
	store := routing.NewMemoryStore()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	seedAPIKeyAnthropicAccount(t, store, cipher, apiKeyAnthropicOwnerID, now)
	resolver := routing.NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, nil) // flag on, mode defaults to vendor_first

	target, err := resolver.Resolve(context.Background(), auth.Token{ID: "tok_apikey_anthropic", UserID: apiKeyAnthropicOwnerID, Active: true}, inference.Request{Model: apiKeyAnthropicModel, APIFlavor: fineFlavor})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", fineFlavor, err)
	}
	target.Endpoint = endpoint
	return target
}

// anthropicPlatformMessagesStub is an httptest stand-in for api.anthropic.com's
// /v1/messages. It records the inbound path + headers + body and answers with a
// multi-frame Messages SSE stream (including a ping event, an SSE comment line and
// the terminal usage frame) so the relay can be checked byte-for-byte.
type anthropicPlatformMessagesStub struct {
	srv       *httptest.Server
	gotPath   string
	gotHeader http.Header
	gotBody   []byte
	sse       string
}

func newAnthropicPlatformMessagesStub(t *testing.T) *anthropicPlatformMessagesStub {
	t.Helper()
	st := &anthropicPlatformMessagesStub{
		sse: "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_platform\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n" +
			"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
			": keep-alive\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hm\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.gotPath = r.URL.Path
		st.gotHeader = r.Header.Clone()
		st.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, st.sse)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

// apiKeyMessagesBody is a rich Messages body the compat translate parser would
// mangle (drops the `system` block array with its cache_control, `tools`,
// `thinking`, `metadata`, ...). Passthrough must forward every field verbatim, the
// model aside. It deliberately carries no Claude-Code system block: the gateway
// must NOT inject the masquerade the subscription translate client prepends.
const apiKeyMessagesBody = `{"model":"claude-sonnet","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"Be terse.","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"name":"shell","description":"run","input_schema":{"type":"object"}}],"thinking":{"type":"enabled","budget_tokens":2048},"metadata":{"user_id":"u1"}}`

// TestAnthropicAPIKeyMessagesDispatchIsLosslessPassthrough proves the full serving
// path for an anthropic_messages request to an ANTHROPIC API-KEY vendor account:
// the request reaches the upstream at /v1/messages with the OPENED key as
// x-api-key, an anthropic-version header (guaranteed by the client), NO OAuth
// bearer / beta header and NO Claude-Code masquerade block, the body verbatim
// except for the rewritten model, and the Messages SSE relayed byte-for-byte.
func TestAnthropicAPIKeyMessagesDispatchIsLosslessPassthrough(t *testing.T) {
	stub := newAnthropicPlatformMessagesStub(t)
	cipher := newDispatchCipher(t)
	s := &Server{Cipher: cipher, Routes: routing.NewMemoryStore()}
	target := resolveAPIKeyAnthropicTarget(t, cipher, "anthropic_messages", stub.srv.URL)

	// The seam tryProxyNative uses: passthrough is selected by the resolved target.
	path, mode := endpointModeFor(target, "anthropic_messages")
	if mode != routing.EndpointModePassthrough || path != "/v1/messages" {
		t.Fatalf("endpointModeFor = (%q, %q), want (/v1/messages, passthrough)", path, mode)
	}
	if !targetServesFlavor(target, "anthropic_messages") {
		t.Fatal("the api-key target must serve the anthropic flavor, or tryProxyNative disables the endpoint")
	}

	// Build the upstream request exactly as proxyNative would: the real auth
	// context, the real path seam and the real model rewrite, relayed through the
	// real Anthropic client the Multiplexer registers for vendor_anthropic.
	ctx := s.upstreamAuthCtx(context.Background(), target)
	upstreamBody := rewriteModelField([]byte(apiKeyMessagesBody), target.ProviderModel)
	resp, err := provider.NewAnthropicClient(stub.srv.Client()).ProxyNative(ctx, target, upstreamPath(target, "anthropic_messages"), upstreamBody)
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	relayed, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	assertAnthropicAPIKeyPassthroughWire(t, stub, relayed, apiKeyMessagesBody, "")
}

// assertAnthropicAPIKeyPassthroughWire checks what the stub upstream saw for an
// api-key Messages passthrough: path, credential, version header, no OAuth/
// masquerade, the body verbatim but for the model, and the SSE relayed verbatim.
// wantBeta is the exact anthropic-beta value the upstream must have received (the
// client's own tokens, merged): empty means the header must be absent.
func assertAnthropicAPIKeyPassthroughWire(t *testing.T, stub *anthropicPlatformMessagesStub, relayed []byte, clientBody, wantBeta string) {
	t.Helper()
	if stub.gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", stub.gotPath)
	}
	// The sealed key is OPENED and sent as x-api-key; the sealed form never goes out.
	if got := stub.gotHeader.Get("x-api-key"); got != apiKeyAnthropicPlainKey {
		t.Fatalf("x-api-key = %q, want the opened key", got)
	}
	for name, vals := range stub.gotHeader {
		for _, v := range vals {
			if strings.Contains(v, "enc:") {
				t.Fatalf("the sealed (enc:) key leaked to the upstream in header %s", name)
			}
		}
	}
	// anthropic-version is mandatory on api.anthropic.com; the client guarantees it
	// and pins the exact value, so a wrong default (or an inbound client's own version
	// leaking through) fails here as well as in the provider tests.
	if got := stub.gotHeader.Values("anthropic-version"); len(got) != 1 || got[0] != "2023-06-01" {
		t.Fatalf("anthropic-version = %v, want exactly [2023-06-01]", got)
	}
	// An api-key target is no subscription target: no OAuth bearer, and no OAuth
	// beta -- the only anthropic-beta it may carry is the client's own, forwarded.
	if got := stub.gotHeader.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want absent on an api-key passthrough (no OAuth bearer)", got)
	}
	switch got := stub.gotHeader.Values("anthropic-beta"); {
	case wantBeta == "" && len(got) != 0:
		t.Fatalf("anthropic-beta = %q, want absent (the client sent none, and the oauth opt-in is subscription-only)", got)
	case wantBeta != "" && (len(got) != 1 || got[0] != wantBeta):
		t.Fatalf("anthropic-beta = %q, want exactly [%q] (the client's tokens, forwarded)", got, wantBeta)
	}

	// The body is the client's, verbatim, except for the model field.
	var sent, want map[string]any
	if err := json.Unmarshal(stub.gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v: %s", err, stub.gotBody)
	}
	if err := json.Unmarshal([]byte(clientBody), &want); err != nil {
		t.Fatalf("fixture body not JSON: %v", err)
	}
	if sent["model"] != apiKeyAnthropicUpstream {
		t.Fatalf("upstream model = %v, want %s (rewritten to the bare upstream slug)", sent["model"], apiKeyAnthropicUpstream)
	}
	want["model"] = apiKeyAnthropicUpstream
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("upstream body differs from the client's beyond the model field:\n got: %s\nwant: %v", stub.gotBody, want)
	}
	if strings.Contains(string(stub.gotBody), "You are Claude Code") {
		t.Fatalf("the Claude-Code masquerade block was injected into an api-key passthrough body: %s", stub.gotBody)
	}

	// The Messages SSE is relayed byte-for-byte.
	if string(relayed) != stub.sse {
		t.Fatalf("relayed body = %q, want the upstream SSE verbatim", relayed)
	}
	if strings.Contains(string(relayed), apiKeyAnthropicPlainKey) {
		t.Fatal("the key leaked into the relayed stream")
	}
}

// TestAnthropicAPIKeyTranslateFlavorsDoNotSelectMessagesPassthrough pins the scope
// of the Messages passthrough: it is chosen by the resolved target and only for an
// anthropic_messages request, so a chat or Responses request to the same Anthropic
// api-key account resolves a target whose Messages mode is zero (translate). Each
// subtest asserts the guarantee that flavor's dispatch actually depends on:
//
//   - openai_responses reaches tryProxyNative, which reads the Responses mode via
//     endpointModeFor(target, "openai_responses"); an empty mode means it hands the
//     request to the translate dispatch.
//   - chat completions never reaches tryProxyNative (it has no native endpoint and
//     is always translated), so the only guarantee to pin is that the resolved
//     target carries no Messages passthrough mode.
func TestAnthropicAPIKeyTranslateFlavorsDoNotSelectMessagesPassthrough(t *testing.T) {
	cipher := newDispatchCipher(t)
	for _, flavor := range []string{"openai_chat_completions", "openai_responses"} {
		t.Run(flavor, func(t *testing.T) {
			target := resolveAPIKeyAnthropicTarget(t, cipher, flavor, "https://api.anthropic.com")
			if flavor == "openai_responses" {
				if path, mode := endpointModeFor(target, flavor); mode != "" {
					t.Fatalf("endpointModeFor(%s) = (%q, %q), want an empty mode (translate)", flavor, path, mode)
				}
			}
			if target.MessagesMode != "" {
				t.Fatalf("MessagesMode = %q, want zero (translate): a %s request must never select the Messages passthrough", target.MessagesMode, flavor)
			}
		})
	}
}

// rewriteHostTransport sends every request to the stub instead of the host the
// request names, recording that host, so the REAL production target (Endpoint
// https://api.anthropic.com) can be exercised without a code seam for the base URL.
type rewriteHostTransport struct {
	stub     *url.URL
	gotHosts []string
}

func (rt *rewriteHostTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.gotHosts = append(rt.gotHosts, r.URL.Scheme+"://"+r.URL.Host)
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rt.stub.Scheme
	r2.URL.Host = rt.stub.Host
	return http.DefaultTransport.RoundTrip(r2)
}

// TestAnthropicAPIKeyMessagesEndToEndThroughTheGateway drives the WHOLE path: an
// owner's POST /v1/messages (bearer-authenticated) against a real *Server whose
// resolver finds the owner's Anthropic api-key vendor account (flag on), through
// tryProxyNative -> proxyNative -> the Multiplexer -> the real AnthropicClient,
// which calls api.anthropic.com (redirected to a stub by the HTTP transport only).
// The client gets the upstream SSE back byte-for-byte; the upstream sees the
// opened x-api-key, an anthropic-version, no OAuth/masquerade, the verbatim body
// but for the model; and the usage row records the vendor account.
func TestAnthropicAPIKeyMessagesEndToEndThroughTheGateway(t *testing.T) {
	stub := newAnthropicPlatformMessagesStub(t)
	stubURL, err := url.Parse(stub.srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	transport := &rewriteHostTransport{stub: stubURL}

	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorAnthropic: provider.NewAnthropicClient(&http.Client{Transport: transport}),
	}, provider.NewMock())
	seedAPIKeyAnthropicAccount(t, routes, cipher, "usr_va_a", time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	enableVendorAccountsFlag(t, srv)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(apiKeyMessagesBody))
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	req.Header.Set("Content-Type", "application/json")
	// The client's own anthropic-version is never forwarded (the gateway pins it);
	// its anthropic-beta is.
	req.Header.Set("anthropic-version", "1999-01-01")
	req.Header.Set("anthropic-beta", "client-beta-1")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if len(transport.gotHosts) != 1 || transport.gotHosts[0] != "https://api.anthropic.com" {
		t.Fatalf("upstream hosts = %v, want exactly one call to https://api.anthropic.com", transport.gotHosts)
	}
	assertAnthropicAPIKeyPassthroughWire(t, stub, rec.Body.Bytes(), apiKeyMessagesBody, "client-beta-1")
	if got := stub.gotHeader.Get("anthropic-version"); got == "1999-01-01" {
		t.Fatalf("anthropic-version = %q: the inbound client's header must not be forwarded", got)
	}

	events := srv.Usage.ByUser("usr_va_a")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].AccountID != apiKeyAnthropicAccountID {
		t.Fatalf("usage AccountID = %q, want %s (vendor account attribution on the passthrough path)", events[0].AccountID, apiKeyAnthropicAccountID)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

const (
	subMessagesAccountID   = "acc_sub_anthropic_e2e"
	subMessagesAccessToken = "sub-e2e-live-access-token"
	subMessagesModel       = "claude-sonnet"
	subMessagesUpstream    = "claude-sonnet-4-5-20250929"
	// claudeCodeLine is the exact first system block the OAuth Messages path needs
	// (the provider's claudeCodeSystemPrompt, spelled out here on purpose so a
	// change to the constant shows up as a failing wire assertion).
	claudeCodeLine = "You are Claude Code, Anthropic's official CLI for Claude."
)

// seedSubscriptionAnthropicRoutableAccount stores one ACTIVE Anthropic SUBSCRIPTION
// vendor account owned by ownerID, holding the OAuth tokens ts sealed with cipher,
// and serving subMessagesModel -> subMessagesUpstream.
func seedSubscriptionAnthropicRoutableAccount(t *testing.T, routes *routing.MemoryStore, cipher *capture.Cipher, accountID, ownerID string, ts vendorauth.TokenSet) {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(cipher, false, ts)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: accountID, OwnerUserID: ownerID, Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription,
		Name: accountID, Status: routing.VendorAccountStatusActive, OAuthTokens: sealed, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routes.SetVendorAccountModels(ctx, accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: subMessagesModel, UpstreamModel: subMessagesUpstream, APIFlavor: routing.APIFlavorAnthropic},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

// resolveSubscriptionAnthropicTarget resolves a request through the REAL
// routing.Resolver against routes (which must hold an ACTIVE Anthropic subscription
// account owned by ownerID) and returns the target the dispatch layer would
// receive. endpoint replaces the target's Endpoint (a struct copy, so only the
// destination moves). Going through the resolver rather than hand-building the
// target keeps the tests honest about what production actually produces.
func resolveSubscriptionAnthropicTarget(t *testing.T, routes *routing.MemoryStore, ownerID, fineFlavor, endpoint string) routing.Target {
	t.Helper()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	resolver := routing.NewResolver(routes, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, nil) // flag on, mode defaults to vendor_first

	target, err := resolver.Resolve(context.Background(), auth.Token{ID: "tok_sub_anthropic", UserID: ownerID, Active: true}, inference.Request{Model: subMessagesModel, APIFlavor: fineFlavor})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", fineFlavor, err)
	}
	target.Endpoint = endpoint
	return target
}

// subscriptionMessagesE2E is a real *Server whose resolver finds the owner's
// Anthropic SUBSCRIPTION vendor account (flag on) and whose HTTP transport
// redirects api.anthropic.com to a stub: only the destination is faked, every
// layer in between is the production one.
type subscriptionMessagesE2E struct {
	srv       *Server
	stub      *anthropicPlatformMessagesStub
	transport *rewriteHostTransport
}

func newSubscriptionMessagesE2E(t *testing.T) *subscriptionMessagesE2E {
	t.Helper()
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
	seedSubscriptionAnthropicRoutableAccount(t, routes, cipher, subMessagesAccountID, "usr_va_a", vendorauth.TokenSet{
		AccessToken: subMessagesAccessToken, RefreshToken: "sub-e2e-live-refresh", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-e2e",
	})
	enableVendorAccountsFlag(t, srv)
	return &subscriptionMessagesE2E{srv: srv, stub: stub, transport: transport}
}

// post drives the whole gateway: the owner's bearer-authenticated POST /v1/messages
// with the client's own anthropic-version (never forwarded) and anthropic-beta.
func (e *subscriptionMessagesE2E) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "1999-01-01")
	req.Header.Set("anthropic-beta", "client-beta-1")
	rec := httptest.NewRecorder()
	e.srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if len(e.transport.gotHosts) != 1 || e.transport.gotHosts[0] != "https://api.anthropic.com" {
		t.Fatalf("upstream hosts = %v, want exactly one call to https://api.anthropic.com", e.transport.gotHosts)
	}
	return rec
}

// assertSubscriptionMessagesPassthroughWire checks what the stub upstream saw for a
// subscription Messages passthrough: path, the OAuth bearer (and no x-api-key),
// exactly one gateway-pinned anthropic-version, the merged anthropic-beta, the
// upstream `system` array being exactly wantSystem (the Claude-Code block once,
// first, and the client's own blocks after it as written), the rest of the client's
// body verbatim but for the model; and the SSE relayed byte-for-byte.
func assertSubscriptionMessagesPassthroughWire(t *testing.T, stub *anthropicPlatformMessagesStub, relayed []byte, clientBody string, wantSystem []any) {
	t.Helper()
	if stub.gotPath != "/v1/messages" {
		t.Fatalf("upstream path = %q, want /v1/messages", stub.gotPath)
	}
	// The OAuth bearer is the account's OPENED access token. The client's own
	// gateway bearer must never go upstream, and a subscription has no x-api-key.
	if got := stub.gotHeader.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+subMessagesAccessToken {
		t.Fatalf("Authorization = %q, want exactly [Bearer <the resolved OAuth access token>]", got)
	}
	if got := stub.gotHeader.Values("x-api-key"); len(got) != 0 {
		t.Fatalf("x-api-key = %q, want absent: a subscription authenticates with the bearer", got)
	}
	for name, vals := range stub.gotHeader {
		for _, v := range vals {
			if strings.Contains(v, "enc:") || strings.Contains(v, vaOwnerSecret) {
				t.Fatalf("a sealed or gateway credential leaked to the upstream in header %s", name)
			}
		}
	}
	// anthropic-version is gateway-pinned, exactly once: neither the client's own
	// (1999-01-01) nor a duplicate from the static target header + the client default.
	if got := stub.gotHeader.Values("anthropic-version"); len(got) != 1 || got[0] != "2023-06-01" {
		t.Fatalf("anthropic-version = %v, want exactly [2023-06-01]", got)
	}
	// The target's static oauth beta first, the client's tokens after it, once each.
	if got := stub.gotHeader.Values("anthropic-beta"); len(got) != 1 || got[0] != "oauth-2025-04-20,client-beta-1" {
		t.Fatalf("anthropic-beta = %q, want exactly [oauth-2025-04-20,client-beta-1]", got)
	}

	var sent, want map[string]any
	if err := json.Unmarshal(stub.gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v: %s", err, stub.gotBody)
	}
	if err := json.Unmarshal([]byte(clientBody), &want); err != nil {
		t.Fatalf("fixture body not JSON: %v", err)
	}
	if sent["model"] != subMessagesUpstream {
		t.Fatalf("upstream model = %v, want %s (rewritten to the bare upstream slug)", sent["model"], subMessagesUpstream)
	}
	// The Claude-Code block is the FIRST system block and appears exactly once; the
	// client's own blocks follow it untouched (cache_control included).
	if !reflect.DeepEqual(sent["system"], wantSystem) {
		t.Fatalf("upstream system = %v, want %v", sent["system"], wantSystem)
	}
	if n := strings.Count(string(stub.gotBody), claudeCodeLine); n != 1 {
		t.Fatalf("the Claude-Code line occurs %d times in the upstream body, want exactly once: %s", n, stub.gotBody)
	}
	// Everything else (thinking, messages, tools, metadata, ...) is the client's.
	want["model"] = subMessagesUpstream
	want["system"] = wantSystem
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("upstream body differs from the client's beyond the model field and the injected block:\n got: %s\nwant: %v", stub.gotBody, want)
	}

	// The Messages SSE is relayed byte-for-byte, with no credential in it.
	if string(relayed) != stub.sse {
		t.Fatalf("relayed body = %q, want the upstream SSE verbatim", relayed)
	}
	if strings.Contains(string(relayed), subMessagesAccessToken) {
		t.Fatal("the OAuth access token leaked into the relayed stream")
	}
}

// assertSubscriptionMessagesUsage checks the single usage row the request left: the
// vendor account is attributed, as on every other vendor passthrough.
func assertSubscriptionMessagesUsage(t *testing.T, e *subscriptionMessagesE2E) {
	t.Helper()
	events := e.srv.Usage.ByUser("usr_va_a")
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].AccountID != subMessagesAccountID {
		t.Fatalf("usage AccountID = %q, want %s (vendor account attribution on the passthrough path)", events[0].AccountID, subMessagesAccountID)
	}
}

// TestAnthropicSubscriptionMessagesEndToEndThroughTheGateway drives the WHOLE path
// of #188: an owner's POST /v1/messages (bearer-authenticated) against a real
// *Server whose resolver finds the owner's Anthropic SUBSCRIPTION vendor account
// (flag on), through tryProxyNative -> proxyNative -> the Multiplexer -> the real
// AnthropicClient, which calls api.anthropic.com (redirected to a stub by the HTTP
// transport only). The client's body carries `thinking` and a `system` array with
// cache_control, none of which the translate path could carry, and an
// anthropic-beta header. The upstream sees the OAuth bearer, one anthropic-version,
// the merged oauth+client beta, the Claude-Code block injected as the first system
// block with the client's own block (and its cache_control) after it, and the rest
// of the body verbatim; the client gets the SSE back byte-for-byte; and the usage
// row records the vendor account.
func TestAnthropicSubscriptionMessagesEndToEndThroughTheGateway(t *testing.T) {
	e := newSubscriptionMessagesE2E(t)

	rec := e.post(t, apiKeyMessagesBody)

	// The client sent ONE system block (with cache_control) and no Claude-Code line:
	// the gateway puts the bare line in front of it and keeps the block as written.
	wantSystem := []any{
		map[string]any{"type": "text", "text": claudeCodeLine},
		map[string]any{"type": "text", "text": "Be terse.", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	assertSubscriptionMessagesPassthroughWire(t, e.stub, rec.Body.Bytes(), apiKeyMessagesBody, wantSystem)
	assertSubscriptionMessagesUsage(t, e)
}

// subscriptionMessagesBodyWithClaudeCodeBlock is a body from a real Claude Code
// client: it ALREADY sends the Claude-Code line as the first system block (with a
// cache_control of its own), followed by its working-directory block.
const subscriptionMessagesBodyWithClaudeCodeBlock = `{"model":"claude-sonnet","max_tokens":1024,"stream":true,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral"}},{"type":"text","text":"Working dir: /repo","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"thinking":{"type":"enabled","budget_tokens":2048}}`

// TestAnthropicSubscriptionMessagesEndToEndDoesNotDoubleTheClaudeCodeBlock is the
// idempotence case through the whole gateway: a client that already sends the
// Claude-Code block as system[0] gets it relayed ONCE, in place, not preceded by a
// second copy; everything else about the request is the same as above.
func TestAnthropicSubscriptionMessagesEndToEndDoesNotDoubleTheClaudeCodeBlock(t *testing.T) {
	e := newSubscriptionMessagesE2E(t)

	rec := e.post(t, subscriptionMessagesBodyWithClaudeCodeBlock)

	// The expected upstream system is the client's own array, unchanged: the line
	// once at the front WITH the client's own cache_control (an injected bare block
	// would have dropped it), then the client's working-directory block.
	wantSystem := []any{
		map[string]any{"type": "text", "text": claudeCodeLine, "cache_control": map[string]any{"type": "ephemeral"}},
		map[string]any{"type": "text", "text": "Working dir: /repo", "cache_control": map[string]any{"type": "ephemeral"}},
	}
	assertSubscriptionMessagesPassthroughWire(t, e.stub, rec.Body.Bytes(), subscriptionMessagesBodyWithClaudeCodeBlock, wantSystem)
	assertSubscriptionMessagesUsage(t, e)
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/logbuffer"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// anthropicMessagesStub is an httptest stand-in for api.anthropic.com's
// /v1/messages that records the inbound headers + body and returns a minimal
// valid Messages response.
type anthropicMessagesStub struct {
	srv       *httptest.Server
	gotHeader http.Header
	gotBody   []byte
}

func newAnthropicMessagesStub(t *testing.T) *anthropicMessagesStub {
	t.Helper()
	st := &anthropicMessagesStub{}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st.gotHeader = r.Header.Clone()
		st.gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

// subscriptionTarget builds the hand-shaped target the routing resolver produces
// for an Anthropic subscription account, pointed at the given messages endpoint.
func subscriptionTarget(accountID, endpoint string) routing.Target {
	return routing.Target{
		RouteID:         "vendor:" + accountID + ":claude-sonnet",
		Provider:        routing.ProviderVendorAnthropic,
		Endpoint:        endpoint,
		Model:           "claude-sonnet",
		ProviderModel:   "claude-3-7-sonnet",
		Timeout:         30 * time.Second,
		APIFlavor:       routing.APIFlavorAnthropic,
		VendorAccountID: accountID,
		Subscription:    true,
		Masquerade:      routing.MasqueradeClaudeCode,
		ExtraHeaders: map[string]string{
			"anthropic-version": "2023-06-01",
			"anthropic-beta":    "oauth-2025-04-20",
		},
		APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
	}
}

func newDispatchCipher(t *testing.T) *capture.Cipher {
	t.Helper()
	cipher, err := capture.New(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	return cipher
}

func seedSubscriptionAccount(t *testing.T, store *routing.MemoryStore, cipher *capture.Cipher, id, vendor string, ts vendorauth.TokenSet) string {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(cipher, false, ts)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	now := time.Now().UTC()
	if err := store.CreateVendorAccount(context.Background(), routing.VendorAccount{
		ID: id, OwnerUserID: "u1", Vendor: vendor, AuthType: routing.VendorAuthSubscription,
		Name: id, Status: routing.VendorAccountStatusActive, OAuthTokens: sealed, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	return sealed
}

func dispatchReq() inference.Request {
	return inference.Request{
		Model: "claude-sonnet",
		Messages: []inference.Message{
			{Role: inference.RoleSystem, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "Be terse."}}},
			{Role: inference.RoleUser, Content: []inference.ContentPart{{Type: inference.ContentText, Text: "hi"}}},
		},
	}
}

// TestSubscriptionDispatchAttachesBearerHeadersAndMasquerade proves the full
// serving path for an un-stale subscription token: the Authorization bearer, the
// two OAuth headers, and the Claude-Code masquerade first system block all reach
// api.anthropic.com, and no token leaks into the upstream response.
func TestSubscriptionDispatchAttachesBearerHeadersAndMasquerade(t *testing.T) {
	stub := newAnthropicMessagesStub(t)
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
		AccessToken: "live-access", RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-1",
	})
	s := &Server{Cipher: cipher, Routes: store}
	target := subscriptionTarget("acc_sub", stub.srv.URL)

	ctx := s.upstreamAuthCtx(context.Background(), target)
	client := provider.NewAnthropicClient(stub.srv.Client())
	resp, err := client.Complete(ctx, target, dispatchReq())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if got := stub.gotHeader.Get("Authorization"); got != "Bearer live-access" {
		t.Fatalf("Authorization = %q, want Bearer live-access", got)
	}
	if got := stub.gotHeader.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want 2023-06-01", got)
	}
	if got := stub.gotHeader.Get("anthropic-beta"); got != "oauth-2025-04-20" {
		t.Fatalf("anthropic-beta = %q, want oauth-2025-04-20", got)
	}
	// The masquerade first system block must be exactly the Claude-Code line.
	var body struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if err := json.Unmarshal(stub.gotBody, &body); err != nil {
		t.Fatalf("system is not an array of blocks: %v: %s", err, stub.gotBody)
	}
	if len(body.System) == 0 || body.System[0].Text != "You are Claude Code, Anthropic's official CLI for Claude." {
		t.Fatalf("first system block = %+v, want the exact Claude-Code line", body.System)
	}

	// Security: the access/refresh token must not appear in the upstream response.
	if strings.Contains(resp.Text, "live-access") || strings.Contains(resp.Text, "live-refresh") {
		t.Fatal("a token leaked into the upstream response text")
	}
}

// TestSubscriptionDispatchRefreshesResealsAndPersists proves a near-expiry token
// is refreshed under the lock, resealed, and persisted (the stored OAuthTokens
// changes and now opens to the refreshed access token), and the refreshed bearer
// is what reaches the upstream.
func TestSubscriptionDispatchRefreshesResealsAndPersists(t *testing.T) {
	stub := newAnthropicMessagesStub(t)
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(10 * time.Second), // within the 2m buffer => stale
		AccountID: "acct-7", PlanType: "max",
	})

	// An OAuth token endpoint that rotates to a fresh token set.
	var sentRefresh string
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &payload)
		sentRefresh = payload["refresh_token"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer oauth.Close()
	ep := vendorauth.DefaultAnthropicEndpoints()
	ep.TokenURL = oauth.URL
	s := &Server{Cipher: cipher, Routes: store, vendorAnthropicEndpoints: ep}
	target := subscriptionTarget("acc_sub", stub.srv.URL)

	ctx := s.upstreamAuthCtx(context.Background(), target)
	if _, err := provider.NewAnthropicClient(stub.srv.Client()).Complete(ctx, target, dispatchReq()); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	if sentRefresh != "stale-refresh" {
		t.Fatalf("refresh_token sent to the token endpoint = %q, want stale-refresh", sentRefresh)
	}
	if got := stub.gotHeader.Get("Authorization"); got != "Bearer fresh-access" {
		t.Fatalf("Authorization = %q, want Bearer fresh-access (the refreshed token)", got)
	}

	// The stored OAuthTokens changed and now opens to the refreshed set, with the
	// account id carried forward.
	acc, err := store.VendorAccountByID(context.Background(), "acc_sub")
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.OAuthTokens == originalSealed {
		t.Fatal("stored OAuthTokens did not change after a refresh")
	}
	if !strings.HasPrefix(acc.OAuthTokens, "enc:") {
		t.Fatalf("stored OAuthTokens is not sealed enc:, got prefix %q", firstPrefix(acc.OAuthTokens))
	}
	reopened, err := vendorauth.OpenTokenSet(cipher, acc.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	if reopened.AccessToken != "fresh-access" || reopened.RefreshToken != "fresh-refresh" {
		t.Fatalf("persisted tokens = %q/%q, want fresh-access/fresh-refresh", "<redacted>", "<redacted>")
	}
	if reopened.AccountID != "acct-7" {
		t.Fatalf("persisted AccountID = %q, want acct-7 (carried forward)", reopened.AccountID)
	}
	// Account stays active across a successful refresh.
	if acc.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status = %q, want active after a successful refresh", acc.Status)
	}
}

// TestSubscriptionDispatchRejectionMarksNeedsReconnect proves a dead refresh
// token flips the account to needs_reconnect and the request proceeds WITHOUT a
// bearer (no panic), so the upstream sees no Authorization.
func TestSubscriptionDispatchRejectionMarksNeedsReconnect(t *testing.T) {
	stub := newAnthropicMessagesStub(t)
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "dead-refresh",
		ExpiresAt: time.Now().Add(-time.Second), // expired
	})

	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer oauth.Close()
	ep := vendorauth.DefaultAnthropicEndpoints()
	ep.TokenURL = oauth.URL
	s := &Server{Cipher: cipher, Routes: store, vendorAnthropicEndpoints: ep}
	target := subscriptionTarget("acc_sub", stub.srv.URL)

	ctx := s.upstreamAuthCtx(context.Background(), target)

	// No bearer is carried, but the static headers still are.
	auth, ok := provider.UpstreamAuthFrom(ctx)
	if !ok {
		t.Fatal("expected the extra headers to still be carried")
	}
	if auth.Token != "" {
		t.Fatal("a bearer must NOT be carried after a refresh rejection")
	}
	if auth.ExtraHeaders["anthropic-beta"] != "oauth-2025-04-20" {
		t.Fatalf("extra headers lost after rejection: %+v", auth.ExtraHeaders)
	}

	// The account is flipped to needs_reconnect, and its tokens are UNCHANGED
	// (the narrow status writer touches only status).
	acc, err := store.VendorAccountByID(context.Background(), "acc_sub")
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("status = %q, want needs_reconnect", acc.Status)
	}
	if acc.OAuthTokens != originalSealed {
		t.Fatal("status flip must not rewrite the OAuth tokens column")
	}

	// The request still dispatches cleanly (no panic); the upstream sees no bearer.
	if _, err := provider.NewAnthropicClient(stub.srv.Client()).Complete(ctx, target, dispatchReq()); err != nil {
		t.Fatalf("Complete after rejection: %v", err)
	}
	if got := stub.gotHeader.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty after a refresh rejection", got)
	}
	if got := stub.gotHeader.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("anthropic-version = %q, want it still attached", got)
	}
}

// TestSubscriptionDispatchConcurrentRefreshSingleFlights proves the per-account
// lock serializes concurrent dispatches for one account so the (often single-use)
// refresh token is spent at most once: with a stale token, N concurrent
// upstreamAuthCtx calls hit the token endpoint exactly once and all end up with
// the refreshed bearer. Run under -race, this also pins the lock discipline.
func TestSubscriptionDispatchConcurrentRefreshSingleFlights(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(5 * time.Second), // stale
	})

	var refreshCalls int32
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&refreshCalls, 1)
		// A small delay widens the window two unserialized refreshers would race in.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer oauth.Close()
	ep := vendorauth.DefaultAnthropicEndpoints()
	ep.TokenURL = oauth.URL
	s := &Server{Cipher: cipher, Routes: store, vendorAnthropicEndpoints: ep}
	target := subscriptionTarget("acc_sub", "http://unused.example")

	const n = 8
	done := make(chan string, n)
	for i := 0; i < n; i++ {
		go func() {
			ctx := s.upstreamAuthCtx(context.Background(), target)
			auth, _ := provider.UpstreamAuthFrom(ctx)
			done <- auth.Token
		}()
	}
	for i := 0; i < n; i++ {
		if got := <-done; got != "fresh-access" {
			t.Fatalf("concurrent dispatch bearer = %q, want fresh-access", got)
		}
	}
	if got := atomic.LoadInt32(&refreshCalls); got != 1 {
		t.Fatalf("token endpoint called %d times, want exactly 1 (per-account single-flight)", got)
	}
}

func firstPrefix(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i+1]
	}
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// --- OpenAI subscription (Milestone 5b) ------------------------------------

// chatGPTBackendStub is an httptest stand-in for chatgpt.com/backend-api/codex's
// Responses endpoint. It records the inbound path + headers + body and returns a
// minimal Responses SSE stream so the relay can be checked byte-for-byte.
type chatGPTBackendStub struct {
	srv       *httptest.Server
	gotPath   string
	gotHeader http.Header
	gotBody   []byte
	sse       string
}

func newChatGPTBackendStub(t *testing.T) *chatGPTBackendStub {
	t.Helper()
	st := &chatGPTBackendStub{sse: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_oai\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n"}
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

// openAISubscriptionTarget builds the hand-shaped target the routing resolver
// produces for an OpenAI subscription account (vendorSubscriptionOpenAITarget),
// pointed at the given backend endpoint. Endpoint is the stub URL so the relay is
// local; the production value is https://chatgpt.com/backend-api/codex.
func openAISubscriptionTarget(accountID, endpoint string) routing.Target {
	return routing.Target{
		RouteID:         "vendor:" + accountID + ":gpt-5-codex",
		Provider:        routing.ProviderVendorOpenAISubscription,
		Endpoint:        endpoint,
		Model:           "gpt-5-codex",
		ProviderModel:   "gpt-5-codex-upstream",
		Timeout:         30 * time.Second,
		APIFlavor:       routing.APIFlavorOpenAI,
		VendorAccountID: accountID,
		Subscription:    true,
		ExtraHeaders: map[string]string{
			"OpenAI-Beta": "responses=experimental",
			"originator":  "codex_cli_rs",
		},
		APIFlavors:    []string{routing.APIFlavorOpenAI},
		ResponsesMode: routing.EndpointModePassthrough,
	}
}

// openAIResponsesBody is a rich Codex-style Responses body (the translate parser
// would drop `tools`; passthrough must forward it verbatim, model aside).
const openAIResponsesBody = `{"model":"gpt-5-codex","stream":true,"input":"hi","tools":[{"type":"function","name":"shell"}]}`

// TestEndpointModeForOpenAISubscriptionResponsesPath pins the path seam: only an
// OpenAI SUBSCRIPTION target (the explicit Subscription flag, not VendorAccountID)
// resolves the Responses path to /responses (the ChatGPT backend's own path, joined
// onto the .../codex endpoint); every other Responses target -- an api-key OpenAI
// vendor target that now ALSO carries a VendorAccountID, and a self-hosted one --
// keeps the OpenAI-platform /v1/responses. upstreamPath agrees, so the usage
// ProviderPath matches what the upstream was actually called with.
func TestEndpointModeForOpenAISubscriptionResponsesPath(t *testing.T) {
	sub := openAISubscriptionTarget("acc_sub_oai", "https://chatgpt.com/backend-api/codex")
	if path, mode := endpointModeFor(sub, "openai_responses"); path != "/responses" || mode != routing.EndpointModePassthrough {
		t.Fatalf("endpointModeFor(openai subscription) = (%q, %q), want (/responses, passthrough)", path, mode)
	}
	if got := upstreamPath(sub, "openai_responses"); got != "/responses" {
		t.Fatalf("upstreamPath(openai subscription) = %q, want /responses", got)
	}

	// An API-KEY OpenAI vendor target, built by the REAL resolver for an
	// openai_responses request: Provider vendor_openai and a VendorAccountID (usage
	// attribution) but Subscription=false and ResponsesMode passthrough (lossless
	// native passthrough to api.openai.com). It must use the stock /v1/responses --
	// NOT the Codex backend's bare /responses. This is the M6a decoupling guard:
	// keying on VendorAccountID here would have conflated it with a subscription
	// target. upstreamPath agrees (the usage label is the path actually called).
	apiKey := resolveAPIKeyOpenAITarget(t, newDispatchCipher(t), "openai_responses", "https://api.openai.com")
	if apiKey.Subscription || apiKey.VendorAccountID == "" || apiKey.Provider != routing.ProviderVendorOpenAI {
		t.Fatalf("resolver-built api-key target = %+v, want a non-subscription vendor_openai target carrying a VendorAccountID", apiKey)
	}
	if path, mode := endpointModeFor(apiKey, "openai_responses"); path != "/v1/responses" || mode != routing.EndpointModePassthrough {
		t.Fatalf("endpointModeFor(api-key openai vendor) = (%q, %q), want (/v1/responses, passthrough)", path, mode)
	}
	if got := upstreamPath(apiKey, "openai_responses"); got != "/v1/responses" {
		t.Fatalf("upstreamPath(api-key openai vendor) = %q, want /v1/responses (passthrough, not the chat-completions translate path)", got)
	}

	// The same account reached over a chat request still TRANSLATES: the resolver
	// leaves ResponsesMode zero on the target (asserted directly -- endpointModeFor
	// only has a case for the Responses/Messages flavors, so it would say nothing
	// about a chat target), so upstreamPath keeps the OpenAI-compatible client's
	// chat-completions endpoint.
	chat := resolveAPIKeyOpenAITarget(t, newDispatchCipher(t), "openai_chat_completions", "https://api.openai.com")
	if chat.ResponsesMode != "" {
		t.Fatalf("api-key openai vendor + chat: ResponsesMode = %q, want zero (translate)", chat.ResponsesMode)
	}
	if got := upstreamPath(chat, "openai_chat_completions"); got != "/v1/chat/completions" {
		t.Fatalf("upstreamPath(api-key openai vendor, chat) = %q, want /v1/chat/completions (translate)", got)
	}

	// A self-hosted Responses target (no VendorAccountID, no Subscription) keeps
	// /v1/responses even in passthrough mode.
	plain := routing.Target{Provider: routing.ProviderVLLM, ResponsesMode: routing.EndpointModePassthrough}
	if path, _ := endpointModeFor(plain, "openai_responses"); path != "/v1/responses" {
		t.Fatalf("endpointModeFor(plain responses) = %q, want /v1/responses", path)
	}
}

// TestOpenAISubscriptionDispatchAttachesBearerAccountIDAndHeaders proves the full
// serving path for an un-stale OpenAI subscription token: the request reaches the
// ChatGPT backend at /responses with the bearer, the chatgpt-account-id header, the
// two static Codex headers, and the model rewritten to the upstream name; the
// Responses SSE is relayed verbatim and no token leaks.
func TestOpenAISubscriptionDispatchAttachesBearerAccountIDAndHeaders(t *testing.T) {
	stub := newChatGPTBackendStub(t)
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_sub_oai", routing.VendorOpenAI, vendorauth.TokenSet{
		AccessToken: "live-access", RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-oai-1", PlanType: "pro",
	})
	s := &Server{Cipher: cipher, Routes: store}
	target := openAISubscriptionTarget("acc_sub_oai", stub.srv.URL)

	// Build the upstream request exactly as proxyNative would: the real path seam
	// and the real model rewrite, so this exercises production code, not a fixture.
	ctx := s.upstreamAuthCtx(context.Background(), target)
	path := upstreamPath(target, "openai_responses")
	upstreamBody := rewriteModelField([]byte(openAIResponsesBody), target.ProviderModel)
	resp, err := provider.NewOpenAIResponsesClient(stub.srv.Client()).ProxyNative(ctx, target, path, upstreamBody)
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	relayed, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if stub.gotPath != "/responses" {
		t.Fatalf("upstream path = %q, want /responses", stub.gotPath)
	}
	if got := stub.gotHeader.Get("Authorization"); got != "Bearer live-access" {
		t.Fatalf("Authorization = %q, want Bearer live-access", got)
	}
	if got := stub.gotHeader.Get("Chatgpt-Account-Id"); got != "acct-oai-1" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-oai-1", got)
	}
	if got := stub.gotHeader.Get("Openai-Beta"); got != "responses=experimental" {
		t.Fatalf("Openai-Beta = %q, want responses=experimental", got)
	}
	if got := stub.gotHeader.Get("Originator"); got != "codex_cli_rs" {
		t.Fatalf("Originator = %q, want codex_cli_rs", got)
	}
	// Model rewritten to the upstream name; every other field preserved verbatim.
	var probe struct {
		Model string          `json:"model"`
		Tools json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(stub.gotBody, &probe); err != nil {
		t.Fatalf("upstream body not JSON: %v: %s", err, stub.gotBody)
	}
	if probe.Model != "gpt-5-codex-upstream" {
		t.Fatalf("upstream model = %q, want gpt-5-codex-upstream (rewritten)", probe.Model)
	}
	if !strings.Contains(string(probe.Tools), "shell") {
		t.Fatalf("upstream body dropped the tools field: %s", stub.gotBody)
	}
	// The Responses SSE is relayed byte-for-byte.
	if string(relayed) != stub.sse {
		t.Fatalf("relayed body = %q, want the upstream SSE verbatim", relayed)
	}
	// Security: no token leaks into the relayed stream or the captured headers.
	if strings.Contains(string(relayed), "live-access") || strings.Contains(string(relayed), "live-refresh") {
		t.Fatal("a token leaked into the relayed stream")
	}
	if strings.Contains(strings.Join(stub.gotHeader["Chatgpt-Account-Id"], ","), "live-") {
		t.Fatal("a token leaked into a captured header")
	}
}

// TestOpenAISubscriptionDispatchRefreshesResealsAndPersists proves a near-expiry
// OpenAI token is refreshed via the FORM-ENCODED OpenAI endpoint (vendorOpenAIEndpoints),
// resealed and persisted, the account id carried forward, and the refreshed bearer
// plus that account id reach the ChatGPT backend.
func TestOpenAISubscriptionDispatchRefreshesResealsAndPersists(t *testing.T) {
	stub := newChatGPTBackendStub(t)
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub_oai", routing.VendorOpenAI, vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(10 * time.Second), // within the 2m buffer => stale
		AccountID: "acct-oai-7", PlanType: "pro",
	})

	// A FORM-ENCODED OpenAI token endpoint (not JSON — the Anthropic shape) that
	// rotates the token set and carries NO id_token, so the account id must be
	// carried forward from the stored set.
	var sentRefresh, sentGrant, gotContentType string
	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		sentRefresh = r.PostForm.Get("refresh_token")
		sentGrant = r.PostForm.Get("grant_type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`)
	}))
	defer oauth.Close()
	ep := vendorauth.DefaultOpenAIEndpoints()
	ep.TokenURL = oauth.URL
	s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: ep}
	target := openAISubscriptionTarget("acc_sub_oai", stub.srv.URL)

	ctx := s.upstreamAuthCtx(context.Background(), target)
	if _, err := provider.NewOpenAIResponsesClient(stub.srv.Client()).ProxyNative(ctx, target, upstreamPath(target, "openai_responses"), []byte(openAIResponsesBody)); err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}

	if gotContentType != "application/x-www-form-urlencoded" {
		t.Fatalf("token endpoint Content-Type = %q, want form-encoded (OpenAI shape)", gotContentType)
	}
	if sentGrant != "refresh_token" || sentRefresh != "stale-refresh" {
		t.Fatalf("token form = grant_type=%q refresh_token=%q, want refresh_token/stale-refresh", sentGrant, sentRefresh)
	}
	if got := stub.gotHeader.Get("Authorization"); got != "Bearer fresh-access" {
		t.Fatalf("Authorization = %q, want Bearer fresh-access (refreshed)", got)
	}
	if got := stub.gotHeader.Get("Chatgpt-Account-Id"); got != "acct-oai-7" {
		t.Fatalf("Chatgpt-Account-Id = %q, want acct-oai-7 (carried forward)", got)
	}

	// The stored OAuthTokens changed and now opens to the refreshed set.
	acc, err := store.VendorAccountByID(context.Background(), "acc_sub_oai")
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.OAuthTokens == originalSealed {
		t.Fatal("stored OAuthTokens did not change after a refresh")
	}
	reopened, err := vendorauth.OpenTokenSet(cipher, acc.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	if reopened.AccessToken != "fresh-access" || reopened.RefreshToken != "fresh-refresh" {
		t.Fatal("persisted tokens are not the refreshed set")
	}
	if reopened.AccountID != "acct-oai-7" {
		t.Fatalf("persisted AccountID = %q, want acct-oai-7 (carried forward)", reopened.AccountID)
	}
	if acc.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status = %q, want active after a successful refresh", acc.Status)
	}
}

// TestSubscriptionDispatchUnknownVendorServesNoBearer proves resolveSubscriptionBearer
// is FAIL-CLOSED: a subscription account with an unknown vendor value serves no
// bearer and no account id, so its sealed token can never be sent to the wrong
// vendor's token endpoint. A near-expiry token makes the point sharper — a
// default-based branch would have tried to refresh it against one vendor's host.
func TestSubscriptionDispatchUnknownVendorServesNoBearer(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_sub_x", "mystery-vendor", vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "stale-refresh",
		ExpiresAt: time.Now().Add(10 * time.Second), // stale: a default branch would refresh
		AccountID: "acct-x",
	})
	s := &Server{Cipher: cipher, Routes: store}

	access, accountID, ok := s.resolveSubscriptionBearer(context.Background(), "acc_sub_x")
	if ok || access != "" || accountID != "" {
		t.Fatalf("resolveSubscriptionBearer(unknown vendor) = (%q, %q, %v), want (\"\", \"\", false) — fail-closed", access, accountID, ok)
	}
	// The account is left untouched (no needs_reconnect flip, no refresh attempted).
	acc, err := store.VendorAccountByID(context.Background(), "acc_sub_x")
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status = %q, want active (an unknown vendor is not a dead refresh token)", acc.Status)
	}
}

// TestOpenAISubscriptionDispatchRejectionMarksNeedsReconnect proves a dead OpenAI
// refresh token flips the account to needs_reconnect and the request proceeds
// WITHOUT a bearer or an account id (no panic); the static Codex headers still ride.
func TestOpenAISubscriptionDispatchRejectionMarksNeedsReconnect(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub_oai", routing.VendorOpenAI, vendorauth.TokenSet{
		AccessToken: "stale-access", RefreshToken: "dead-refresh",
		ExpiresAt: time.Now().Add(-time.Second), // expired
	})

	oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
	}))
	defer oauth.Close()
	ep := vendorauth.DefaultOpenAIEndpoints()
	ep.TokenURL = oauth.URL
	s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: ep}
	target := openAISubscriptionTarget("acc_sub_oai", "http://unused.example")

	ctx := s.upstreamAuthCtx(context.Background(), target)

	auth, ok := provider.UpstreamAuthFrom(ctx)
	if !ok {
		t.Fatal("expected the static Codex headers to still be carried")
	}
	if auth.Token != "" {
		t.Fatal("a bearer must NOT be carried after a refresh rejection")
	}
	if auth.ExtraHeaders["chatgpt-account-id"] != "" {
		t.Fatalf("no chatgpt-account-id must be carried after a rejection, got %q", auth.ExtraHeaders["chatgpt-account-id"])
	}
	if auth.ExtraHeaders["OpenAI-Beta"] != "responses=experimental" {
		t.Fatalf("static Codex headers lost after rejection: %+v", auth.ExtraHeaders)
	}

	acc, err := store.VendorAccountByID(context.Background(), "acc_sub_oai")
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	if acc.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("status = %q, want needs_reconnect", acc.Status)
	}
	if acc.OAuthTokens != originalSealed {
		t.Fatal("status flip must not rewrite the OAuth tokens column")
	}
}

// failingTokenWriteStore fails the narrow OAuth-token writer, so a refreshed set
// cannot be persisted.
type failingTokenWriteStore struct {
	routing.Store
}

func (failingTokenWriteStore) SetVendorAccountOAuthTokens(context.Context, string, string) error {
	return errors.New("simulated store failure")
}

// warnRecords returns the WARN-level records whose message contains substr.
func warnRecords(recs []logbuffer.Record, substr string) []logbuffer.Record {
	var out []logbuffer.Record
	for _, r := range recs {
		if r.Level == "WARN" && strings.Contains(r.Msg, substr) {
			out = append(out, r)
		}
	}
	return out
}

// TestResolveSubscriptionBearerLogsGenuineFailuresAtWarn pins the level, not just
// the behavior: the gateway's default log level is info, so a failed or rejected
// refresh logged at Debug would leave a subscription account that silently stops
// serving with nothing in any log a default deployment keeps. The capture runs at
// INFO for that reason. Every record names the account and never a token.
func TestResolveSubscriptionBearerLogsGenuineFailuresAtWarn(t *testing.T) {
	const staleRefresh = "stale-refresh-secret"
	staleTokens := vendorauth.TokenSet{
		AccessToken: "stale-access-secret", RefreshToken: staleRefresh,
		ExpiresAt: time.Now().Add(-time.Second), // expired => a refresh is attempted
	}
	oauthWith := func(status int, body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
		}))
	}
	assertNoSecrets := func(t *testing.T, recs []logbuffer.Record) {
		t.Helper()
		for _, secret := range []string{"stale-access-secret", staleRefresh, "fresh-access-secret", "fresh-refresh-secret"} {
			assertNoSecretInLogs(t, recs, secret)
		}
	}

	t.Run("refresh rejection warns and flips needs_reconnect", func(t *testing.T) {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		oauth := oauthWith(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"refresh token revoked"}`)
		defer oauth.Close()
		store := routing.NewMemoryStore()
		cipher := newDispatchCipher(t)
		seedSubscriptionAccount(t, store, cipher, "acc_warn_rej", routing.VendorOpenAI, staleTokens)
		ep := vendorauth.DefaultOpenAIEndpoints()
		ep.TokenURL = oauth.URL
		s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: ep}

		if _, _, ok := s.resolveSubscriptionBearer(context.Background(), "acc_warn_rej"); ok {
			t.Fatal("a rejected refresh must serve no bearer")
		}
		recs := buf.Snapshot()
		got := warnRecords(recs, "refresh rejected")
		if len(got) != 1 || got[0].Attrs["account"] != "acc_warn_rej" {
			t.Fatalf("want exactly one WARN naming the account for the rejection at the default level (info); records = %+v", recs)
		}
		assertNoSecrets(t, recs)
	})

	t.Run("transient refresh failure warns", func(t *testing.T) {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		oauth := oauthWith(http.StatusInternalServerError, `{"error":"server_error"}`)
		defer oauth.Close()
		store := routing.NewMemoryStore()
		cipher := newDispatchCipher(t)
		seedSubscriptionAccount(t, store, cipher, "acc_warn_5xx", routing.VendorOpenAI, staleTokens)
		ep := vendorauth.DefaultOpenAIEndpoints()
		ep.TokenURL = oauth.URL
		s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: ep}

		if _, _, ok := s.resolveSubscriptionBearer(context.Background(), "acc_warn_5xx"); ok {
			t.Fatal("a failed refresh must serve no bearer")
		}
		recs := buf.Snapshot()
		if got := warnRecords(recs, "refresh failed"); len(got) != 1 || got[0].Attrs["account"] != "acc_warn_5xx" {
			t.Fatalf("want exactly one WARN naming the account for the failed refresh; records = %+v", recs)
		}
		assertNoSecrets(t, recs)
		acc, err := store.VendorAccountByID(context.Background(), "acc_warn_5xx")
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		if acc.Status != routing.VendorAccountStatusActive {
			t.Fatalf("status = %q, want active (a transient failure is not a dead refresh token)", acc.Status)
		}
	})

	t.Run("persist failure warns but still serves the refreshed bearer", func(t *testing.T) {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		oauth := oauthWith(http.StatusOK, `{"access_token":"fresh-access-secret","refresh_token":"fresh-refresh-secret","expires_in":3600}`)
		defer oauth.Close()
		store := routing.NewMemoryStore()
		cipher := newDispatchCipher(t)
		seedSubscriptionAccount(t, store, cipher, "acc_warn_persist", routing.VendorOpenAI, staleTokens)
		ep := vendorauth.DefaultOpenAIEndpoints()
		ep.TokenURL = oauth.URL
		s := &Server{Cipher: cipher, Routes: failingTokenWriteStore{Store: store}, vendorOpenAIEndpoints: ep}

		access, _, ok := s.resolveSubscriptionBearer(context.Background(), "acc_warn_persist")
		if !ok || access != "fresh-access-secret" {
			t.Fatalf("resolveSubscriptionBearer = (%q, %v), want the refreshed bearer served even when persisting failed", access, ok)
		}
		recs := buf.Snapshot()
		if got := warnRecords(recs, "persist failed"); len(got) != 1 || got[0].Attrs["account"] != "acc_warn_persist" {
			t.Fatalf("want exactly one WARN naming the account for the failed persist; records = %+v", recs)
		}
		assertNoSecrets(t, recs)
	})

	t.Run("unknown vendor stays quiet at the default level", func(t *testing.T) {
		buf := withCapturedSlogAtTheDefaultLevel(t)
		store := routing.NewMemoryStore()
		cipher := newDispatchCipher(t)
		seedSubscriptionAccount(t, store, cipher, "acc_quiet", "mystery-vendor", staleTokens)
		s := &Server{Cipher: cipher, Routes: store}

		if _, _, ok := s.resolveSubscriptionBearer(context.Background(), "acc_quiet"); ok {
			t.Fatal("an unknown vendor must serve no bearer")
		}
		if recs := buf.Snapshot(); len(recs) != 0 {
			t.Fatalf("the fail-closed unknown-vendor path must not log above Debug; records = %+v", recs)
		}
	})
}

// TestUpstreamAuthCtxKeysOnSubscriptionFlagNotAccountID is the M6a decoupling
// regression guard. The subscription-bearer trigger moved off target.VendorAccountID
// (now carried by EVERY vendor target for usage attribution) onto the explicit
// target.Subscription flag. This pins both halves so the move cannot silently
// regress M5a/M5b:
//
//   - An API-KEY vendor target (VendorAccountID set, Subscription=false) uses its
//     sealed APIToken and attempts NO subscription-bearer resolution -- proven by a
//     decoy subscription account sharing the same id with a dead refresh token:
//     were resolution attempted, it would flip that account to needs_reconnect; it
//     stays active, and the upstream credential is the decrypted api key.
//   - A SUBSCRIPTION target (Subscription=true) still resolves the OAuth bearer
//     from the account's sealed tokens, carrying no api key.
func TestUpstreamAuthCtxKeysOnSubscriptionFlagNotAccountID(t *testing.T) {
	t.Run("api_key_target_uses_sealed_token_and_skips_subscription_resolution", func(t *testing.T) {
		cipher := newDispatchCipher(t)
		store := routing.NewMemoryStore()
		// A decoy SUBSCRIPTION account with the SAME id as the api-key target's
		// VendorAccountID, carrying a dead refresh token and an expired access token.
		// If upstreamAuthCtx wrongly routed on VendorAccountID it would try to refresh
		// this and flip it to needs_reconnect; it must not.
		seedSubscriptionAccount(t, store, cipher, "acc_vendor", routing.VendorAnthropic, vendorauth.TokenSet{
			AccessToken: "stale-access", RefreshToken: "dead-refresh", ExpiresAt: time.Now().Add(-time.Hour),
		})
		oauth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_grant"}`)
		}))
		defer oauth.Close()
		ep := vendorauth.DefaultAnthropicEndpoints()
		ep.TokenURL = oauth.URL
		s := &Server{Cipher: cipher, Routes: store, vendorAnthropicEndpoints: ep}

		sealedKey, err := capture.SealSecret(cipher, false, "sk-vendor-key")
		if err != nil {
			t.Fatalf("SealSecret: %v", err)
		}
		// The hand-built api-key vendor target: an account id (usage attribution) AND
		// a sealed APIToken, Subscription deliberately false.
		target := routing.Target{
			RouteID:         "vendor:acc_vendor:gpt-4o",
			Provider:        routing.ProviderVendorOpenAI,
			Endpoint:        "https://api.openai.com",
			Model:           "gpt-4o",
			ProviderModel:   "gpt-4o",
			Timeout:         30 * time.Second,
			APIFlavor:       routing.APIFlavorOpenAI,
			APIToken:        sealedKey,
			VendorAccountID: "acc_vendor",
			Subscription:    false,
		}

		ctx := s.upstreamAuthCtx(context.Background(), target)
		auth, ok := provider.UpstreamAuthFrom(ctx)
		if !ok {
			t.Fatal("expected the sealed api-key credential to be carried")
		}
		if auth.Token != "sk-vendor-key" {
			t.Fatalf("Token = %q, want the decrypted api key (the APIToken path)", auth.Token)
		}
		// No subscription resolution ran: the decoy account stays active.
		acc, err := store.VendorAccountByID(context.Background(), "acc_vendor")
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		if acc.Status != routing.VendorAccountStatusActive {
			t.Fatalf("status = %q, want active -- an api-key target must not trigger subscription-bearer resolution", acc.Status)
		}
	})

	t.Run("subscription_target_resolves_oauth_bearer", func(t *testing.T) {
		cipher := newDispatchCipher(t)
		store := routing.NewMemoryStore()
		seedSubscriptionAccount(t, store, cipher, "acc_sub", routing.VendorAnthropic, vendorauth.TokenSet{
			AccessToken: "live-access", RefreshToken: "live-refresh", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct-1",
		})
		s := &Server{Cipher: cipher, Routes: store}
		target := subscriptionTarget("acc_sub", "https://api.anthropic.com")

		ctx := s.upstreamAuthCtx(context.Background(), target)
		auth, ok := provider.UpstreamAuthFrom(ctx)
		if !ok {
			t.Fatal("expected an upstream credential for a subscription target")
		}
		if auth.Token != "live-access" {
			t.Fatalf("Token = %q, want the resolved OAuth bearer live-access", auth.Token)
		}
	})
}

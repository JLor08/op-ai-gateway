// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
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

func seedSubscriptionAccount(t *testing.T, store *routing.MemoryStore, cipher *capture.Cipher, id string, ts vendorauth.TokenSet) string {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(cipher, false, ts)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	now := time.Now().UTC()
	if err := store.CreateVendorAccount(context.Background(), routing.VendorAccount{
		ID: id, OwnerUserID: "u1", Vendor: routing.VendorAnthropic, AuthType: routing.VendorAuthSubscription,
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
	seedSubscriptionAccount(t, store, cipher, "acc_sub", vendorauth.TokenSet{
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
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub", vendorauth.TokenSet{
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
	originalSealed := seedSubscriptionAccount(t, store, cipher, "acc_sub", vendorauth.TokenSet{
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
	seedSubscriptionAccount(t, store, cipher, "acc_sub", vendorauth.TokenSet{
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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	apiKeyAccountID = "acc_apikey_oai"
	apiKeyPlainKey  = "sk-test-opened-key"
	apiKeyOwnerID   = "usr_apikey_owner"
)

// resolveAPIKeyOpenAITarget resolves a request through the REAL routing.Resolver
// against a store holding one ACTIVE OpenAI api-key vendor account (key sealed with
// cipher) serving gpt-4o, and returns the target the dispatch layer would receive.
// endpoint replaces the target's Endpoint (a struct copy, so only the destination
// moves) so a test can point the otherwise production-shaped target at an httptest
// stub. Going through the resolver rather than hand-building the target keeps these
// tests honest about what production actually produces.
func resolveAPIKeyOpenAITarget(t *testing.T, cipher *capture.Cipher, fineFlavor, endpoint string) routing.Target {
	t.Helper()
	sealed, err := capture.SealSecret(cipher, false, apiKeyPlainKey)
	if err != nil {
		t.Fatalf("SealSecret: %v", err)
	}
	ctx := context.Background()
	store := routing.NewMemoryStore()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	if err := store.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: apiKeyAccountID, OwnerUserID: apiKeyOwnerID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthAPIKey,
		Name: apiKeyAccountID, Status: routing.VendorAccountStatusActive, APIKey: sealed, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := store.SetVendorAccountModels(ctx, apiKeyAccountID, []routing.VendorAccountModel{
		{AccountID: apiKeyAccountID, GatewayModel: "gpt-4o", UpstreamModel: "gpt-4o-2024-08-06", APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	resolver := routing.NewResolver(store, func() time.Time { return now }, nil)
	resolver.SetVendorAccountAccessors(func() bool { return true }, nil) // flag on, mode defaults to vendor_first

	target, err := resolver.Resolve(ctx, auth.Token{ID: "tok_apikey", UserID: apiKeyOwnerID, Active: true}, inference.Request{Model: "gpt-4o", APIFlavor: fineFlavor})
	if err != nil {
		t.Fatalf("Resolve(%s): %v", fineFlavor, err)
	}
	target.Endpoint = endpoint
	return target
}

// openAIPlatformResponsesStub is an httptest stand-in for api.openai.com's
// /v1/responses. It records the inbound path + headers + body and answers with a
// multi-frame Responses SSE stream (including an SSE comment line and the terminal
// usage frame) so the relay can be checked byte-for-byte.
type openAIPlatformResponsesStub struct {
	srv       *httptest.Server
	gotPath   string
	gotHeader http.Header
	gotBody   []byte
	sse       string
}

func newOpenAIPlatformResponsesStub(t *testing.T) *openAIPlatformResponsesStub {
	t.Helper()
	st := &openAIPlatformResponsesStub{
		sse: ": keep-alive\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_platform\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n",
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

// apiKeyResponsesBody is a rich Responses body the translate parser would mangle
// (drops `tools`, `reasoning`, `previous_response_id`, ...). Passthrough must
// forward every field verbatim, the model aside. It deliberately omits `store`:
// the gateway must NOT inject store:false the way the subscription translate
// client does.
const apiKeyResponsesBody = `{"model":"gpt-4o","stream":true,"input":[{"role":"user","content":"hi"}],"tools":[{"type":"function","name":"shell","parameters":{"type":"object"}}],"reasoning":{"effort":"high"},"previous_response_id":"resp_prev","metadata":{"k":"v"}}`

// TestOpenAIAPIKeyResponsesDispatchIsLosslessPassthrough proves the full serving
// path for an openai_responses request to an OpenAI API-KEY vendor account: the
// request reaches the upstream at /v1/responses (not /v1/chat/completions) with the
// OPENED key as Authorization: Bearer, no ChatGPT-backend headers, the body verbatim
// except for the rewritten model, and the Responses SSE relayed byte-for-byte.
func TestOpenAIAPIKeyResponsesDispatchIsLosslessPassthrough(t *testing.T) {
	stub := newOpenAIPlatformResponsesStub(t)
	cipher := newDispatchCipher(t)
	s := &Server{Cipher: cipher, Routes: routing.NewMemoryStore()}
	target := resolveAPIKeyOpenAITarget(t, cipher, "openai_responses", stub.srv.URL)

	// The seam tryProxyNative uses: passthrough is selected by the resolved target.
	path, mode := endpointModeFor(target, "openai_responses")
	if mode != routing.EndpointModePassthrough || path != "/v1/responses" {
		t.Fatalf("endpointModeFor = (%q, %q), want (/v1/responses, passthrough)", path, mode)
	}
	if !targetServesFlavor(target, "openai_responses") {
		t.Fatal("the api-key target must serve the openai flavor, or tryProxyNative disables the endpoint")
	}

	// Build the upstream request exactly as proxyNative would: the real auth
	// context, the real path seam and the real model rewrite, relayed through the
	// real OpenAI-compatible client the Multiplexer registers for vendor_openai.
	ctx := s.upstreamAuthCtx(context.Background(), target)
	upstreamBody := rewriteModelField([]byte(apiKeyResponsesBody), target.ProviderModel)
	resp, err := provider.NewOpenAICompatibleClient(stub.srv.Client()).ProxyNative(ctx, target, upstreamPath(target, "openai_responses"), upstreamBody)
	if err != nil {
		t.Fatalf("ProxyNative: %v", err)
	}
	relayed, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if stub.gotPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", stub.gotPath)
	}
	// The sealed key is OPENED and sent as a Bearer; the sealed form never goes out.
	if got := stub.gotHeader.Get("Authorization"); got != "Bearer "+apiKeyPlainKey {
		t.Fatalf("Authorization = %q, want Bearer <the opened key>", got)
	}
	if strings.Contains(stub.gotHeader.Get("Authorization"), "enc:") {
		t.Fatal("the sealed (enc:) key leaked to the upstream")
	}
	// An api-key target carries none of the ChatGPT-backend (subscription) headers.
	for _, h := range []string{"Chatgpt-Account-Id", "Openai-Beta", "Originator"} {
		if got := stub.gotHeader.Get(h); got != "" {
			t.Fatalf("%s = %q, want absent on an api-key passthrough", h, got)
		}
	}

	// The body is the client's, verbatim, except for the model field.
	var sent, want map[string]any
	if err := json.Unmarshal(stub.gotBody, &sent); err != nil {
		t.Fatalf("upstream body not JSON: %v: %s", err, stub.gotBody)
	}
	if err := json.Unmarshal([]byte(apiKeyResponsesBody), &want); err != nil {
		t.Fatalf("fixture body not JSON: %v", err)
	}
	if sent["model"] != "gpt-4o-2024-08-06" {
		t.Fatalf("upstream model = %v, want gpt-4o-2024-08-06 (rewritten to the bare upstream slug)", sent["model"])
	}
	want["model"] = "gpt-4o-2024-08-06"
	if !reflect.DeepEqual(sent, want) {
		t.Fatalf("upstream body differs from the client's beyond the model field:\n got: %s\nwant: %v", stub.gotBody, want)
	}
	if _, forced := sent["store"]; forced {
		t.Fatalf("upstream body carries a store field %v the client never sent (verbatim relay must not force store:false)", sent["store"])
	}

	// The Responses SSE is relayed byte-for-byte.
	if string(relayed) != stub.sse {
		t.Fatalf("relayed body = %q, want the upstream SSE verbatim", relayed)
	}
	if strings.Contains(string(relayed), apiKeyPlainKey) {
		t.Fatal("the key leaked into the relayed stream")
	}
}

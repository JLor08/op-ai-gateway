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

// openAIPacedResponsesStub is an httptest stand-in for api.openai.com's
// /v1/responses that serves a reasoning model's stream the way a real one
// arrives: the first reasoning-summary delta, then -- after a real gap, because
// the usage scanner's generation window is measured between arrivals -- the text
// delta and the terminal response.completed. Like the real OpenAI platform it
// carries no `timings` on any frame.
func newOpenAIPacedResponsesStub(t *testing.T, gap time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"hm\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(gap)
		_, _ = io.WriteString(w,
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n"+
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_paced\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestOpenAIAPIKeyResponsesPassthroughRecordsTheDerivedRateOnTheUsageRow proves
// the #182 headline end to end for a vendor Responses passthrough: the REAL
// proxyNative relays a resolver-built api-key OpenAI target's stream, and the
// recorded usage row carries a gateway-derived tokens/s (OpenAI reports none),
// attributed to the vendor account. The upstream pauses for gap between the first
// (reasoning) delta and the rest, which gives the scanner a generation window
// comfortably wider than minGatewayRateWindow; the exact figures are pinned by the
// scanner tests, so this one only proves the rate reaches the recorded row.
func TestOpenAIAPIKeyResponsesPassthroughRecordsTheDerivedRateOnTheUsageRow(t *testing.T) {
	// Generous against the 50ms window floor: the scanner measures between the two
	// READ times, and a loaded machine can delay the first read by tens of ms.
	const gap = 300 * time.Millisecond
	upstream := newOpenAIPacedResponsesStub(t, gap)
	srv, _, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	srv.Provider = provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorOpenAI: provider.NewOpenAICompatibleClient(upstream.Client()),
	}, provider.NewMock())
	target := resolveAPIKeyOpenAITarget(t, cipher, "openai_responses", upstream.URL)
	if target.VendorAccountID != apiKeyAccountID || target.ResponsesMode != routing.EndpointModePassthrough {
		t.Fatalf("resolver-built target = %+v, want a vendor Responses passthrough target for %s", target, apiKeyAccountID)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(apiKeyResponsesBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+vaOwnerSecret)
	rec := httptest.NewRecorder()
	srv.proxyNative(rec, req, nativeRelay{
		token:    auth.Token{ID: "tok_rate", UserID: "usr_va_a", Active: true},
		target:   target,
		path:     upstreamPath(target, "openai_responses"),
		raw:      []byte(apiKeyResponsesBody),
		pfReq:    inference.Request{Model: target.Model, RequestedModel: target.Model, APIFlavor: "openai_responses", Stream: true},
		endpoint: endpointResponses,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("proxyNative status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}

	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	ev := events[0]
	if ev.AccountID != apiKeyAccountID || ev.OutputTokens != 6 {
		t.Fatalf("usage row = account %q / output %d, want %s / 6", ev.AccountID, ev.OutputTokens, apiKeyAccountID)
	}
	// Only "a derived rate was recorded" is asserted here. The exact values are
	// pinned deterministically by the scanner tests (controlled timestamps); this
	// test's real-time window is T_read(chunk 2) - T_read(chunk 1), which shrinks
	// under scheduler/CPU load, so any upper bound derived from the upstream's
	// sleep would be a flake. The > 0 is still meaningful end to end: the stream's
	// only content before the pause is a reasoning-summary delta, so without
	// reasoning counted as content the window would collapse to the single chunk
	// carrying the text delta and the terminal frame, below the floor, and the
	// rate would be 0.
	if ev.TokensPerSecond <= 0 {
		t.Fatalf("recorded TokensPerSecond = %v, want > 0 (a gateway-derived rate on the vendor row; the window opens at the reasoning delta)", ev.TokensPerSecond)
	}
}

// TestSelfHostedResponsesPassthroughRecordsNoDerivedRate is the negative control
// through the same proxyNative wiring: a self-hosted (non-vendor) target relaying
// the same paced stream with no `timings` records rate 0 -- the vendor flag the
// scanner is built with must be false there.
func TestSelfHostedResponsesPassthroughRecordsNoDerivedRate(t *testing.T) {
	prov := pacedNativeProxyProvider{
		pieces: []string{
			"event: response.reasoning_summary_text.delta\n" + `data: {"type":"response.reasoning_summary_text.delta","delta":"hm"}` + "\n\n",
			"event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","delta":"Hello"}` + "\n\n" +
				"event: response.completed\n" + `data: {"type":"response.completed","response":{"id":"r","usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}` + "\n\n",
		},
		gap: 80 * time.Millisecond,
	}
	srv := newNativeProxyTestServer(prov, true, false)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	events := srv.Usage.All()
	if len(events) != 1 {
		t.Fatalf("usage events = %d, want 1", len(events))
	}
	if events[0].OutputTokens != 6 {
		t.Fatalf("recorded OutputTokens = %d, want 6", events[0].OutputTokens)
	}
	if events[0].TokensPerSecond != 0 {
		t.Fatalf("recorded TokensPerSecond = %v, want 0 (a self-hosted Responses stream without upstream timings stays unrated)", events[0].TokensPerSecond)
	}
}

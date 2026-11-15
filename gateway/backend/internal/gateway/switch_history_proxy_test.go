// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/inference"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strings"
	"sync"
	"testing"
)

// The recorded upstream answers of the provider-switch probes (design doc probe
// table): O4 from the ChatGPT backend, A2 from Anthropic.
const (
	probeO4Body = `{"error":{"message":"The encrypted content for item rs_1 could not be verified.","type":"invalid_request_error","param":null,"code":"invalid_encrypted_content"}}`
	probeA2Body = "{\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"messages.1.content.0: Invalid `signature` in `thinking` block\"}}"
	switchOKSSE = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_x\",\"usage\":{\"input_tokens\":3,\"output_tokens\":7,\"total_tokens\":10}}}\n\n"
)

type scriptedAnswer struct {
	status int
	body   string
}

// scriptedProxyProvider answers ProxyNative from a queue (a 200 SSE once the
// queue is empty) and records every body it was handed.
type scriptedProxyProvider struct {
	mu      sync.Mutex
	answers []scriptedAnswer
	bodies  [][]byte
}

func (p *scriptedProxyProvider) Complete(context.Context, routing.Target, inference.Request) (provider.Response, error) {
	return provider.Response{}, nil
}

func (p *scriptedProxyProvider) ProxyNative(_ context.Context, _ routing.Target, _ string, body []byte, _ string) (*provider.ProxyResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.bodies = append(p.bodies, append([]byte(nil), body...))
	a := scriptedAnswer{status: http.StatusOK, body: switchOKSSE}
	if len(p.answers) > 0 {
		a, p.answers = p.answers[0], p.answers[1:]
	}
	ct := "text/event-stream"
	if a.status != http.StatusOK {
		ct = jsonContentType
	}
	return &provider.ProxyResponse{
		StatusCode: a.status,
		Header:     http.Header{"Content-Type": []string{ct}},
		Body:       io.NopCloser(strings.NewReader(a.body)),
	}, nil
}

func (p *scriptedProxyProvider) calls() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([][]byte(nil), p.bodies...)
}

var (
	switchOpenAITarget = routing.Target{Provider: routing.ProviderVendorOpenAI, VendorAccountID: "acc_oai_b", ServerID: "srv-native", Model: "gw-model", ProviderModel: "upstream-model"}
	switchLocalTarget  = routing.Target{Provider: routing.ProviderVLLM, ServerID: "srv-native", InstanceKey: "0", Model: "gw-model", ProviderModel: "upstream-model"}
)

// relaySwitch runs one native passthrough through proxyNative for target and
// returns the client's answer.
func relaySwitch(t *testing.T, srv *Server, target routing.Target, flavor string, endpoint sessionEndpoint, raw []byte, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	path := "/v1/responses"
	if endpoint == endpointMessages {
		path = "/v1/messages"
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	rec := httptest.NewRecorder()
	srv.proxyNative(rec, req, nativeRelay{
		token:    auth.Token{ID: "tok_dev", UserID: "usr_dev", Active: true, LogCommunication: true},
		target:   target,
		path:     path,
		raw:      raw,
		pfReq:    inference.Request{Model: target.Model, RequestedModel: target.Model, APIFlavor: flavor, Stream: true},
		endpoint: endpoint,
	})
	return rec
}

func codexSession(id string) http.Header {
	return http.Header{"session_id": []string{id}}
}

func TestProxyNativeStripsMintedReasoningBeforeSend(t *testing.T) {
	prov := &scriptedProxyProvider{}
	srv := newNativeProxyTestServer(prov, true, false)
	cs := &fakeCaptureStore{}
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv.Captures, srv.Cipher = cs, cipher

	// A gateway-minted reasoning item: no readable text, empty encrypted_content.
	client := `{"model":"gw-model","stream":true,"input":[{"role":"user","content":"hi"},{"type":"reasoning","summary":[],"encrypted_content":""},{"role":"user","content":"next"}]}`
	raw := []byte(client)
	rec := relaySwitch(t, srv, switchLocalTarget, inference.APIFlavorOpenAIResponses, endpointResponses, raw, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := prov.calls()
	if len(calls) != 1 {
		t.Fatalf("provider calls = %d, want 1", len(calls))
	}
	if bytes.Contains(calls[0], []byte(`"reasoning"`)) {
		t.Fatalf("upstream body still carries the minted reasoning item: %s", calls[0])
	}
	if !bytes.Contains(calls[0], []byte(`"next"`)) || !bytes.Contains(calls[0], []byte(`"upstream-model"`)) {
		t.Fatalf("upstream body lost the rest of the request: %s", calls[0])
	}
	if string(raw) != client {
		t.Fatalf("client body was written to: %s", raw)
	}
	saved, ok := cs.last()
	if !ok {
		t.Fatal("no capture saved")
	}
	if env := decryptCapture(t, saved.Blob); env.ReqBody != client {
		t.Fatalf("captured request = %q, want the client's own bytes %q", env.ReqBody, client)
	}
}

// codexSwitchBody is a Codex history replayed after a switch to account B: it
// carries the reasoning item account A produced.
const codexSwitchBody = `{"model":"gw-model","stream":true,"input":[{"role":"user","content":"hi"},{"type":"reasoning","summary":[{"type":"summary_text","text":"t"}],"encrypted_content":"gAAAA-from-account-a"},{"role":"user","content":"next"}]}`

func TestProxyNativeRetriesOnceOnForeignReasoningRefusal(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)

	rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), codexSession("sess-retry"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != switchOKSSE {
		t.Fatalf("client body = %q, want the retry's answer only", rec.Body.String())
	}
	calls := prov.calls()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	if !bytes.Contains(calls[0], []byte("gAAAA-from-account-a")) {
		t.Fatalf("first attempt must be sent optimistically with the blob: %s", calls[0])
	}
	if bytes.Contains(calls[1], []byte(`"reasoning"`)) {
		t.Fatalf("retry body still carries a reasoning item: %s", calls[1])
	}
	if !bytes.Contains(calls[1], []byte(`"next"`)) {
		t.Fatalf("retry body lost the rest of the history: %s", calls[1])
	}
	if events := srv.Usage.All(); len(events) != 1 {
		t.Fatalf("usage rows = %d, want exactly 1", len(events))
	}
}

func TestProxyNativeRetriesAnthropicSignatureRefusal(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeA2Body}}}
	srv := newNativeProxyTestServer(prov, false, true)
	body := `{"model":"gw-model","stream":true,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig-from-a"}]},{"role":"user","content":"next"}]}`

	rec := relaySwitch(t, srv, switchLocalTarget, inference.APIFlavorAnthropicMessages, endpointMessages, []byte(body), http.Header{"x-claude-code-session-id": []string{"cc-1"}})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := prov.calls()
	if len(calls) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(calls))
	}
	if bytes.Contains(calls[1], []byte("sig-from-a")) || bytes.Contains(calls[1], []byte(`"assistant"`)) {
		t.Fatalf("retry body still carries the thinking turn: %s", calls[1])
	}
	if events := srv.Usage.All(); len(events) != 1 {
		t.Fatalf("usage rows = %d, want exactly 1", len(events))
	}
}

func TestProxyNativeFollowUpFilteredWithoutFailedAttempt(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)
	relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), codexSession("sess-follow"))
	before := len(prov.calls())

	followUp := `{"model":"gw-model","stream":true,"input":[{"role":"user","content":"hi"},{"type":"reasoning","summary":[],"encrypted_content":"gAAAA-from-account-a"},{"role":"user","content":"next"},{"type":"reasoning","summary":[],"encrypted_content":"gAAAA-from-account-b"},{"role":"user","content":"more"}]}`
	rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(followUp), codexSession("sess-follow"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	calls := prov.calls()[before:]
	if len(calls) != 1 {
		t.Fatalf("follow-up provider calls = %d, want 1", len(calls))
	}
	if bytes.Contains(calls[0], []byte("gAAAA-from-account-a")) {
		t.Fatalf("follow-up still carries the rejected blob: %s", calls[0])
	}
	if !bytes.Contains(calls[0], []byte("gAAAA-from-account-b")) {
		t.Fatalf("follow-up lost the new target's own blob: %s", calls[0])
	}
}

func TestProxyNativeDoesNotRetryUnrelatedErrors(t *testing.T) {
	for _, a := range []scriptedAnswer{
		{http.StatusBadRequest, `{"error":{"message":"max_tokens is too large","type":"invalid_request_error","param":"max_tokens","code":null}}`},
		{http.StatusTooManyRequests, `{"error":{"message":"rate limited","type":"rate_limit_error","code":"rate_limit_exceeded"}}`},
		{http.StatusInternalServerError, `{"error":{"message":"boom","code":"invalid_encrypted_content"}}`},
	} {
		prov := &scriptedProxyProvider{answers: []scriptedAnswer{a}}
		srv := newNativeProxyTestServer(prov, true, false)

		rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), codexSession("sess-other"))

		if n := len(prov.calls()); n != 1 {
			t.Fatalf("%d: provider calls = %d, want 1", a.status, n)
		}
		if rec.Code != a.status || rec.Body.String() != a.body {
			t.Fatalf("%d: client got %d %q, want the upstream answer verbatim", a.status, rec.Code, rec.Body.String())
		}
	}
}

func TestProxyNativeRelaysSecondFailureWithoutLooping(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}, {http.StatusBadRequest, probeO4Body}, {http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)

	rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), codexSession("sess-loop"))

	if n := len(prov.calls()); n != 2 {
		t.Fatalf("provider calls = %d, want 2", n)
	}
	if rec.Code != http.StatusBadRequest || rec.Body.String() != probeO4Body {
		t.Fatalf("client got %d %q, want the second 400 verbatim", rec.Code, rec.Body.String())
	}
	if events := srv.Usage.All(); len(events) != 1 {
		t.Fatalf("usage rows = %d, want exactly 1", len(events))
	}
}

// A refusal for a body with no opaque blob left to remove is relayed, not retried
// with the identical body.
func TestProxyNativeDoesNotRetryIdenticalBody(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)
	body := `{"model":"gw-model","stream":true,"input":[{"role":"user","content":"hi"}]}`

	rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(body), codexSession("sess-same"))

	if n := len(prov.calls()); n != 1 {
		t.Fatalf("provider calls = %d, want 1", n)
	}
	if rec.Code != http.StatusBadRequest || rec.Body.String() != probeO4Body {
		t.Fatalf("client got %d %q, want the 400 verbatim", rec.Code, rec.Body.String())
	}
}

func TestProxyNativeNoSessionIDRetriesEachTime(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}, {http.StatusOK, switchOKSSE}, {http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)

	for i := range 2 {
		rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, body = %s", i, rec.Code, rec.Body.String())
		}
	}
	if n := len(prov.calls()); n != 4 {
		t.Fatalf("provider calls = %d, want 4 (no memo without a session id)", n)
	}
}

// A body that needs nothing reaches the provider exactly as rewriteModelField
// leaves it, on both flavors and target classes.
func TestProxyNativeUntouchedBodyIsByteIdentical(t *testing.T) {
	for _, tc := range []struct {
		target   routing.Target
		flavor   string
		endpoint sessionEndpoint
		body     string
	}{
		{switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, `{"model":"gw-model", "stream":true,"input":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high"}}`},
		{switchLocalTarget, inference.APIFlavorAnthropicMessages, endpointMessages, `{"model":"gw-model", "stream":true,"messages":[{"role":"user","content":"hi"}]}`},
	} {
		prov := &scriptedProxyProvider{}
		srv := newNativeProxyTestServer(prov, true, true)
		relaySwitch(t, srv, tc.target, tc.flavor, tc.endpoint, []byte(tc.body), codexSession("s"))
		want := rewriteModelField([]byte(tc.body), tc.target.ProviderModel)
		if calls := prov.calls(); len(calls) != 1 || !bytes.Equal(calls[0], want) {
			t.Fatalf("%s: upstream body = %q, want %q", tc.flavor, calls, want)
		}
	}
}

// A Server built without New has no memo: steps 1 and 2 still run, nothing panics.
func TestProxyNativeNilMemoStillRetries(t *testing.T) {
	prov := &scriptedProxyProvider{answers: []scriptedAnswer{{http.StatusBadRequest, probeO4Body}}}
	srv := newNativeProxyTestServer(prov, true, false)
	srv.switchMemo = nil

	rec := relaySwitch(t, srv, switchOpenAITarget, inference.APIFlavorOpenAIResponses, endpointResponses, []byte(codexSwitchBody), codexSession("sess-nil"))

	if rec.Code != http.StatusOK || len(prov.calls()) != 2 {
		t.Fatalf("status = %d, calls = %d, want 200 after one retry", rec.Code, len(prov.calls()))
	}
}

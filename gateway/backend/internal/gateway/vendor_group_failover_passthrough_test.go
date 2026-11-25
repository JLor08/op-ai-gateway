// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// keyedVendorAnswer is what keyedVendorStub answers one api key with: status (0 =
// 200) with header and body.
type keyedVendorAnswer struct {
	status int
	header http.Header
	body   string
}

// keyedVendorStub stands in for a fixed-endpoint vendor (api.anthropic.com,
// api.openai.com) that several accounts share: it tells the accounts apart by the
// api key the request carries (x-api-key or the Bearer), counts the calls per key
// and answers each key as scripted.
type keyedVendorStub struct {
	srv     *httptest.Server
	mu      sync.Mutex
	calls   map[string]int
	answers map[string]keyedVendorAnswer
}

func newKeyedVendorStub(t *testing.T, answers map[string]keyedVendorAnswer) *keyedVendorStub {
	t.Helper()
	st := &keyedVendorStub{calls: map[string]int{}, answers: answers}
	st.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		st.mu.Lock()
		st.calls[key]++
		a, ok := st.answers[key]
		st.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		for k, vs := range a.header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		if a.status != 0 {
			w.WriteHeader(a.status)
		}
		_, _ = io.WriteString(w, a.body)
	}))
	t.Cleanup(st.srv.Close)
	return st
}

func (st *keyedVendorStub) callsFor(key string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.calls[key]
}

func (st *keyedVendorStub) total() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	n := 0
	for _, c := range st.calls {
		n += c
	}
	return n
}

// newPassthroughGroupServer is the group test server with the real Anthropic and
// OpenAI-compatible clients, both sending every request to stub whatever host the
// production target names (rewriteHostTransport).
func newPassthroughGroupServer(t *testing.T, stub *keyedVendorStub) (*Server, *routing.MemoryStore, func(id, vendor, prefix, key string)) {
	t.Helper()
	stubURL, err := url.Parse(stub.srv.URL)
	if err != nil {
		t.Fatalf("parse stub URL: %v", err)
	}
	client := &http.Client{Transport: &rewriteHostTransport{stub: stubURL}}
	srv, routes, cipher := newVendorGroupTestServer(t, provider.NewMultiplexer(map[string]provider.Client{
		routing.ProviderVendorAnthropic: provider.NewAnthropicClient(client),
		routing.ProviderVendorOpenAI:    provider.NewOpenAICompatibleClient(client),
	}, provider.NewMock()))
	seed := func(id, vendor, prefix, key string) {
		seedVendorGroupAccount(t, routes, cipher, id, vendor, "", prefix, key)
	}
	return srv, routes, seed
}

const (
	vgMessagesSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_b\",\"usage\":{\"input_tokens\":4,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"from B\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":6}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	vgResponsesSSE = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"from B\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_b\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6,\"total_tokens\":10}}}\n\n"
)

func vgMessagesBody(model string) string {
	return `{"model":"` + model + `","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`
}

func vgResponsesBody(model string) string {
	return `{"model":"` + model + `","stream":true,"input":"hi"}`
}

// anthropicLimitAnswer is a 429 as api.anthropic.com sends it: retry-after, the
// unified headers of the rejected window, and a rate_limit_error naming who.
func anthropicLimitAnswer(who, retryAfter string, reset time.Time) keyedVendorAnswer {
	return keyedVendorAnswer{
		status: http.StatusTooManyRequests,
		header: http.Header{
			"Content-Type":                               {"application/json"},
			"Retry-After":                                {retryAfter},
			"Anthropic-Ratelimit-Unified-Status":         {"rejected"},
			"Anthropic-Ratelimit-Unified-5h-Utilization": {"1.0"},
			"Anthropic-Ratelimit-Unified-5h-Reset":       {strconv.FormatInt(reset.Unix(), 10)},
			"Anthropic-Ratelimit-Unified-Reset":          {strconv.FormatInt(reset.Unix(), 10)},
		},
		body: `{"type":"error","error":{"type":"rate_limit_error","message":"limit of ` + who + `"}}`,
	}
}

func sseAnswer(body string) keyedVendorAnswer {
	return keyedVendorAnswer{header: http.Header{"Content-Type": {"text/event-stream"}}, body: body}
}

// TestPassthroughMessagesFailsOverOnVendor429: a Claude Code /v1/messages request
// routed to a group whose first member answers 429 is relayed to the second
// member, and the client gets that member's stream byte for byte. The refusing
// member got exactly one call, is cooled down for the model until its retry-after
// and has its rejected window scraped; the one usage row is the serving member's.
func TestPassthroughMessagesFailsOverOnVendor429(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-ant-a": anthropicLimitAnswer("A", "5", time.Now().Add(time.Hour)),
		"sk-ant-b": sseAnswer(vgMessagesSSE),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seed("acc_b", routing.VendorAnthropic, "b/", "sk-ant-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a", "acc_b")

	before := time.Now()
	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody(vgGroupModel))

	if rec.Code != http.StatusOK || rec.Body.String() != vgMessagesSSE {
		t.Fatalf("status = %d body = %q, want 200 with member 2's stream verbatim", rec.Code, rec.Body.String())
	}
	if stub.callsFor("sk-ant-a") != 1 || stub.callsFor("sk-ant-b") != 1 {
		t.Fatalf("upstream calls = %v, want one per member", stub.calls)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Fatalf("Retry-After = %q: the refused attempt's headers must not reach the client", rec.Header().Get("Retry-After"))
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
	until, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now())
	if !ok || until.Before(before.Add(4*time.Second)) || until.After(time.Now().Add(6*time.Second)) {
		t.Fatalf("member 1 cooldown = %s (active %v), want about now+5s (retry-after before the unified reset)", until, ok)
	}
	if u, found, err := routes.VendorAccountUsageByID(context.Background(), "acc_a"); err != nil || !found || u.FiveHourPct != 100 {
		t.Fatalf("member 1 usage = %+v found=%v err=%v, want the rejected window scraped (100 %%)", u, found, err)
	}
}

// TestPassthroughCooldownPublicEndIgnoresTheScrapedReset: a refusal without a
// Retry-After cools the member down until the scraped unified reset for routing, but
// its public end -- the one a borrower who is not entitled to the account's usage is
// shown -- is the 60 s default, never that reset.
func TestPassthroughCooldownPublicEndIgnoresTheScrapedReset(t *testing.T) {
	reset := time.Now().Add(time.Hour).Truncate(time.Second)
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-ant-a": anthropicLimitAnswer("A", "", reset),
		"sk-ant-b": sseAnswer(vgMessagesSSE),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seed("acc_b", routing.VendorAnthropic, "b/", "sk-ant-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a", "acc_b")

	before := time.Now()
	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody(vgGroupModel))

	if rec.Code != http.StatusOK || rec.Body.String() != vgMessagesSSE {
		t.Fatalf("status = %d body = %q, want 200 with member 2's stream verbatim", rec.Code, rec.Body.String())
	}
	if until, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now()); !ok || !until.Equal(reset) {
		t.Fatalf("routing cooldown = %s (active %v), want the scraped unified reset %s", until, ok, reset)
	}
	public := srv.VendorLimitCooldowns().ActivePublic("acc_a", time.Now())
	if len(public) != 1 || public[0].UpstreamModel != vgUpstream {
		t.Fatalf("public cooldowns = %+v, want one for %s", public, vgUpstream)
	}
	if got := public[0].Until; got.Before(before.Add(routing.DefaultVendorLimitCooldown)) || got.After(time.Now().Add(routing.DefaultVendorLimitCooldown)) {
		t.Fatalf("public cooldown end = %s, want about now+%s (the default, never the scraped reset)", got, routing.DefaultVendorLimitCooldown)
	}
}

// TestPassthroughResponsesFailsOverOnVendor429 is the Codex (/v1/responses) twin,
// over OpenAI api-key accounts.
func TestPassthroughResponsesFailsOverOnVendor429(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-oai-a": {status: http.StatusTooManyRequests, header: http.Header{"Retry-After": {"9"}}, body: `{"error":{"code":"insufficient_quota"}}`},
		"sk-oai-b": sseAnswer(vgResponsesSSE),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorOpenAI, "a/", "sk-oai-a")
	seed("acc_b", routing.VendorOpenAI, "b/", "sk-oai-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAI, "grp/", "acc_a", "acc_b")

	rec := vgPost(t, srv, "/v1/responses", vgResponsesBody(vgGroupModel))

	if rec.Code != http.StatusOK || rec.Body.String() != vgResponsesSSE {
		t.Fatalf("status = %d body = %q, want 200 with member 2's stream verbatim", rec.Code, rec.Body.String())
	}
	if stub.callsFor("sk-oai-a") != 1 || stub.callsFor("sk-oai-b") != 1 {
		t.Fatalf("upstream calls = %v, want one per member", stub.calls)
	}
	assertOneVendorUsageRow(t, srv, "acc_b", vgGroupID)
	if _, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now()); !ok {
		t.Fatal("member 1 has no limit cooldown after its 429")
	}
}

// TestPassthroughAllMembers429RelaysLast (Review Focus 5): every member answers
// 429. Each is called exactly once -- the exclusion grows with every attempt -- and
// the client gets the last member's answer as it came (status, retry-after, body),
// with one usage row.
func TestPassthroughAllMembers429RelaysLast(t *testing.T) {
	reset := time.Now().Add(time.Hour)
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-ant-a": anthropicLimitAnswer("A", "11", reset),
		"sk-ant-b": anthropicLimitAnswer("B", "12", reset),
		"sk-ant-c": anthropicLimitAnswer("C", "13", reset),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seed("acc_b", routing.VendorAnthropic, "b/", "sk-ant-b")
	seed("acc_c", routing.VendorAnthropic, "c/", "sk-ant-c")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a", "acc_b", "acc_c")

	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody(vgGroupModel))

	if rec.Code != http.StatusTooManyRequests || rec.Body.String() != anthropicLimitAnswer("C", "13", reset).body {
		t.Fatalf("status = %d body = %q, want the third member's 429 verbatim", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "13" {
		t.Fatalf("Retry-After = %q, want the last member's 13", got)
	}
	for _, key := range []string{"sk-ant-a", "sk-ant-b", "sk-ant-c"} {
		if stub.callsFor(key) != 1 {
			t.Fatalf("upstream calls = %v, want exactly one per member", stub.calls)
		}
	}
	if stub.total() != 3 {
		t.Fatalf("upstream calls = %d, want 3", stub.total())
	}
	assertOneVendorUsageRow(t, srv, "acc_c", vgGroupID)
}

// TestPassthroughSingleAccount429Unchanged: an account reached by its own name (no
// group) is relayed as today -- one upstream call, the 429 as it came, no cooldown.
func TestPassthroughSingleAccount429Unchanged(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-ant-a": anthropicLimitAnswer("A", "5", time.Now().Add(time.Hour)),
		"sk-ant-b": sseAnswer(vgMessagesSSE),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seed("acc_b", routing.VendorAnthropic, "b/", "sk-ant-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a", "acc_b")

	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody("a/"+vgUpstream))

	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "limit of A") || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status = %d retry-after = %q body = %q, want A's 429 relayed", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if stub.callsFor("sk-ant-a") != 1 || stub.total() != 1 {
		t.Fatalf("upstream calls = %v, want exactly one, to A", stub.calls)
	}
	assertOneVendorUsageRow(t, srv, "acc_a", "")
	if _, ok := srv.VendorLimitCooldowns().Until("acc_a", vgUpstream, time.Now()); ok {
		t.Fatal("a single-account request must not set a group cooldown")
	}
}

// TestPassthroughVendorFailoverDeclinesOnShadowedAccount: the group's prefix ("")
// shadows its first member's own prefix (""), and the only other member is cooling
// down. After member 1's 429 the re-resolve finds no group member left and the
// resolver's own-account step -- which ignores the exclusion -- returns member 1
// itself, outside the group. That is no failover: member 1 is not called again and
// the client gets its 429 as it came.
func TestPassthroughVendorFailoverDeclinesOnShadowedAccount(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{
		"sk-ant-a": anthropicLimitAnswer("A", "5", time.Now().Add(time.Hour)),
		"sk-ant-b": sseAnswer(vgMessagesSSE),
	})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "", "sk-ant-a")
	seed("acc_b", routing.VendorAnthropic, "b/", "sk-ant-b")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "", "acc_a", "acc_b")
	srv.VendorLimitCooldowns().Set("acc_b", vgUpstream, time.Now().Add(time.Hour), time.Now().Add(time.Hour))

	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody(vgUpstream))

	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), "limit of A") || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("status = %d retry-after = %q body = %q, want A's own 429 relayed", rec.Code, rec.Header().Get("Retry-After"), rec.Body.String())
	}
	if stub.callsFor("sk-ant-a") != 1 || stub.total() != 1 {
		t.Fatalf("upstream calls = %v, want exactly one, to A (never A again outside the group, never B)", stub.calls)
	}
	assertOneVendorUsageRow(t, srv, "acc_a", vgGroupID)
}

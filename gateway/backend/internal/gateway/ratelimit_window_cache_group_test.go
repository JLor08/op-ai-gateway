// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// A response served by a vendor group member that reports no limit windows of its
// own (an api-key reserve) must not carry the principal's cached subscription
// windows -- those belong to a member the group skipped -- and the cached entry is
// dropped, so the client neither shows nor blocks itself on them. A response from
// outside a group keeps #407's behaviour.

// vgSubscriptionAccount is the account the seeded cache entries claim to come from:
// a subscription member the group no longer routes to.
const vgSubscriptionAccount = "acc_sub"

// vgUsageLimitsToken switches the group token's usage-limit opt-in (#420) on and
// returns it with its principal key.
func vgUsageLimitsToken(t *testing.T, srv *Server) (auth.Token, string) {
	t.Helper()
	store := srv.Tokens.(*auth.TokenStore)
	tok, ok := store.LookupBearer("Bearer " + vgSecret)
	if !ok {
		t.Fatal("group token not found")
	}
	tok.SendUsageLimits = true
	store.UpdateToken(tok)
	key, ok := principalKey(tok)
	if !ok {
		t.Fatal("group token has no principal")
	}
	return tok, key
}

func hasCachedWindows(c *rateLimitWindowCache, key string) bool {
	_, ok := c.liveEntry(key, time.Now())
	return ok
}

func TestWindowCacheDropRemovesTheEntryWhateverItsAccount(t *testing.T) {
	now := time.Now()
	c := newRateLimitWindowCache(10)
	c.Observe("user:a", "acct1", windowHeaders(now, time.Hour, 24*time.Hour), now)
	c.ObserveCodex("user:b", "acct2", codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	c.Drop("user:a")
	c.Drop("user:nobody")

	if hasCachedWindows(c, "user:a") {
		t.Fatal("user:a's entry survived Drop")
	}
	if !hasCachedWindows(c, "user:b") {
		t.Fatal("Drop of another principal removed user:b's entry")
	}
}

// TestAttachRateLimitWindowsByTarget pins the rule at the attach seam: a target
// from outside a group (a local model, a single account) gets the cached windows
// as before; a group target gets nothing and drops the entry.
func TestAttachRateLimitWindowsByTarget(t *testing.T) {
	srv := &Server{rateLimitWindows: newRateLimitWindowCache(10), codexRateLimitWindows: newRateLimitWindowCache(10)}
	tok := auth.Token{ID: "tok_w", UserID: "usr_w", Active: true, SendUsageLimits: true}
	key, _ := principalKey(tok)
	now := time.Now()
	srv.rateLimitWindows.Observe(key, vgSubscriptionAccount, windowHeaders(now, time.Hour, 24*time.Hour), now)
	srv.codexRateLimitWindows.ObserveCodex(key, vgSubscriptionAccount, codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	local := routing.Target{RouteID: "route-local", Provider: routing.ProviderServerAgent}
	dst := http.Header{}
	srv.attachRateLimitWindows(tok, local, dst)
	srv.attachCodexRateLimitWindows(tok, local, dst)
	if dst.Get(rlp+"5h-utilization") != "0.42" || dst.Get("X-Codex-Primary-Used-Percent") != "42.5" {
		t.Fatalf("local-model response lost the cached windows: %v", dst)
	}

	grouped := routing.Target{RouteID: "vendor:acc_key:m", Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acc_key", VendorGroupID: vgGroupID}
	dst = http.Header{}
	srv.attachRateLimitWindows(tok, grouped, dst)
	srv.attachCodexRateLimitWindows(tok, grouped, dst)
	if len(dst) != 0 {
		t.Fatalf("group-served response got cached windows: %v", dst)
	}
	if hasCachedWindows(srv.rateLimitWindows, key) || hasCachedWindows(srv.codexRateLimitWindows, key) {
		t.Fatal("the principal's cached entries survived a group member without windows")
	}
}

// TestRateLimitWindowsDroppedForGroupMemberWithoutWindows: Claude Code over
// passthrough, served by an Anthropic api-key group member whose response carries
// no unified headers.
func TestRateLimitWindowsDroppedForGroupMemberWithoutWindows(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{"sk-ant-a": sseAnswer(vgMessagesSSE)})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a")
	_, key := vgUsageLimitsToken(t, srv)
	now := time.Now()
	srv.rateLimitWindows.Observe(key, vgSubscriptionAccount, windowHeaders(now, time.Hour, 24*time.Hour), now)

	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody(vgGroupModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	requireNoUnified(t, rec)
	if hasCachedWindows(srv.rateLimitWindows, key) {
		t.Fatal("the principal's cached windows survived a group member without windows")
	}
}

// TestRateLimitWindowsStillAttachedOutsideGroup: the same api-key account reached
// by its own name (no group) keeps #407's behaviour -- the cached windows ride along
// and stay cached.
func TestRateLimitWindowsStillAttachedOutsideGroup(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{"sk-ant-a": sseAnswer(vgMessagesSSE)})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorAnthropic, "a/", "sk-ant-a")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorAnthropic, "grp/", "acc_a")
	_, key := vgUsageLimitsToken(t, srv)
	now := time.Now()
	srv.rateLimitWindows.Observe(key, vgSubscriptionAccount, windowHeaders(now, time.Hour, 24*time.Hour), now)

	rec := vgPost(t, srv, "/v1/messages", vgMessagesBody("a/"+vgUpstream))

	if rec.Code != http.StatusOK || rec.Header().Get(rlp+"5h-utilization") != "0.42" {
		t.Fatalf("status = %d headers = %v, want the cached windows attached", rec.Code, rec.Header())
	}
	if !hasCachedWindows(srv.rateLimitWindows, key) {
		t.Fatal("a response from outside a group dropped the cached windows")
	}
}

// TestRateLimitWindowsDroppedOnTranslatedGroupResponse: /v1/messages translated to
// an OpenAI-compatible group member, buffered and streamed.
func TestRateLimitWindowsDroppedOnTranslatedGroupResponse(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "buffered", true: "stream"}[stream], func(t *testing.T) {
			srv := newCompatGroupServer(t, newChatVendorStub(t, "A", 0, ""), newChatVendorStub(t, "B", 0, ""))
			_, key := vgUsageLimitsToken(t, srv)
			now := time.Now()
			srv.rateLimitWindows.Observe(key, vgSubscriptionAccount, windowHeaders(now, time.Hour, 24*time.Hour), now)
			body := `{"model":"` + vgGroupModel + `","max_tokens":8,"messages":[{"role":"user","content":"hi"}]`
			if stream {
				body += `,"stream":true`
			}

			rec := vgPost(t, srv, "/v1/messages", body+`}`)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			requireNoUnified(t, rec)
			if hasCachedWindows(srv.rateLimitWindows, key) {
				t.Fatal("the principal's cached windows survived a translated group response")
			}
		})
	}
}

// TestCodexWindowsDroppedForGroupMemberWithoutWindows is the Codex twin over
// passthrough: an OpenAI api-key group member answers /v1/responses without any
// x-codex-* header.
func TestCodexWindowsDroppedForGroupMemberWithoutWindows(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{"sk-oai-a": sseAnswer(vgResponsesSSE)})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorOpenAI, "a/", "sk-oai-a")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAI, "grp/", "acc_a")
	_, key := vgUsageLimitsToken(t, srv)
	now := time.Now()
	srv.codexRateLimitWindows.ObserveCodex(key, vgSubscriptionAccount, codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	rec := vgPost(t, srv, "/v1/responses", vgResponsesBody(vgGroupModel))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodex(t, rec)
	if hasCachedWindows(srv.codexRateLimitWindows, key) {
		t.Fatal("the principal's cached Codex windows survived a group member without windows")
	}
}

func TestCodexWindowsStillAttachedOutsideGroup(t *testing.T) {
	stub := newKeyedVendorStub(t, map[string]keyedVendorAnswer{"sk-oai-a": sseAnswer(vgResponsesSSE)})
	srv, routes, seed := newPassthroughGroupServer(t, stub)
	seed("acc_a", routing.VendorOpenAI, "a/", "sk-oai-a")
	seedVendorGroup(t, routes, vgGroupID, routing.VendorOpenAI, "grp/", "acc_a")
	_, key := vgUsageLimitsToken(t, srv)
	now := time.Now()
	srv.codexRateLimitWindows.ObserveCodex(key, vgSubscriptionAccount, codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	rec := vgPost(t, srv, "/v1/responses", vgResponsesBody("a/"+vgUpstream))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	requireCodexWindowsAttached(t, rec, "42.5")
	if !hasCachedWindows(srv.codexRateLimitWindows, key) {
		t.Fatal("a response from outside a group dropped the cached Codex windows")
	}
}

// TestCodexWindowsDroppedOnTranslatedGroupResponse: /v1/responses translated to an
// OpenAI-compatible group member.
func TestCodexWindowsDroppedOnTranslatedGroupResponse(t *testing.T) {
	srv := newCompatGroupServer(t, newChatVendorStub(t, "A", 0, ""), newChatVendorStub(t, "B", 0, ""))
	_, key := vgUsageLimitsToken(t, srv)
	now := time.Now()
	srv.codexRateLimitWindows.ObserveCodex(key, vgSubscriptionAccount, codexWindowHeaders(now, time.Hour, 24*time.Hour), now)

	rec := vgPost(t, srv, "/v1/responses", `{"model":"`+vgGroupModel+`","input":"hi"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodex(t, rec)
	if hasCachedWindows(srv.codexRateLimitWindows, key) {
		t.Fatal("the principal's cached Codex windows survived a translated group response")
	}
}

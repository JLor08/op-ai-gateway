// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"testing"
	"time"
)

// --- principalFor: pure resolution logic (design spec §3) -----------------

// TestPrincipalForServiceToken proves a service token (Kind=="service")
// resolves to the SERVICE principal keyed on its ServiceID.
func TestPrincipalForServiceToken(t *testing.T) {
	tok := auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_1", ServiceName: "Svc"}
	p, ok := principalFor(tok)
	if !ok {
		t.Fatalf("principalFor(service token) ok = false, want true")
	}
	if p != (Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}) {
		t.Fatalf("principalFor(service token) = %+v, want {service svc_1}", p)
	}
}

// TestPrincipalForUserToken proves an ordinary user-owned API token resolves
// to the USER principal keyed on UserID.
func TestPrincipalForUserToken(t *testing.T) {
	tok := auth.Token{ID: "tok_usr", UserID: "usr_1"}
	p, ok := principalFor(tok)
	if !ok {
		t.Fatalf("principalFor(user token) ok = false, want true")
	}
	if p != (Principal{Type: routing.PrincipalTypeUser, ID: "usr_1"}) {
		t.Fatalf("principalFor(user token) = %+v, want {user usr_1}", p)
	}
}

// TestPrincipalForSessionPseudoToken proves the session/chat-loopback pseudo-
// principal (a token with NO ID -- it is not a real bearer token row -- but a
// UserID, exactly how the session-authenticated web path and the internal
// trusted-loopback background-chat-run path populate auth.Token) resolves to
// the SAME user principal a real API token for that user would.
func TestPrincipalForSessionPseudoToken(t *testing.T) {
	tok := auth.Token{ID: "", UserID: "usr_1"}
	p, ok := principalFor(tok)
	if !ok {
		t.Fatalf("principalFor(session pseudo-token) ok = false, want true")
	}
	if p != (Principal{Type: routing.PrincipalTypeUser, ID: "usr_1"}) {
		t.Fatalf("principalFor(session pseudo-token) = %+v, want {user usr_1}", p)
	}
}

// TestPrincipalForNeitherServiceNorUser proves a token with neither a service
// id nor a user id (should not occur for any token that passed auth in
// practice) skips principal-limit enforcement entirely, rather than resolving
// to some degenerate zero-value principal that could accidentally alias every
// such token onto one shared bucket.
func TestPrincipalForNeitherServiceNorUser(t *testing.T) {
	_, ok := principalFor(auth.Token{ID: "tok_x"})
	if ok {
		t.Fatalf("principalFor(token with no service/user id) ok = true, want false")
	}
}

// TestPrincipalForNoStacking proves "kein Stacking" (design spec §3): a token
// that is (hypothetically -- real service-token minting never sets UserID)
// BOTH a service token AND carries a UserID resolves to the SERVICE principal
// ONLY. If IsService() and the UserID branch were ever both consulted (e.g. a
// future refactor that changed the early return to a fallthrough), this would
// catch it immediately.
func TestPrincipalForNoStacking(t *testing.T) {
	tok := auth.Token{ID: "tok_svc", Kind: "service", ServiceID: "svc_1", UserID: "usr_should_be_ignored"}
	p, ok := principalFor(tok)
	if !ok {
		t.Fatalf("principalFor ok = false, want true")
	}
	if p != (Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}) {
		t.Fatalf("principalFor(service+user token) = %+v, want the SERVICE principal only (no stacking)", p)
	}
}

// --- writeLimitDenied: HTTP status/code/header mapping (design spec §8) ---

func TestWriteLimitDeniedRate(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLimitDenied(rec, "rate", 7*time.Second)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "limit.rate_limited" {
		t.Fatalf("error code = %q, want limit.rate_limited", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
}

// TestWriteLimitDeniedRateSubSecondFloor proves a sub-second retryAfter (e.g.
// a rate window boundary computed a few milliseconds out) still reports a
// meaningful (>=1s) Retry-After rather than "0", which would tell the client
// to retry with no delay at all.
func TestWriteLimitDeniedRateSubSecondFloor(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLimitDenied(rec, "rate", 150*time.Millisecond)
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1 (floor)", got)
	}
}

func TestWriteLimitDeniedRequestQuota(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLimitDenied(rec, "request_quota", 0)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "limit.request_quota_exceeded" {
		t.Fatalf("error code = %q, want limit.request_quota_exceeded", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want absent for a non-rate reason", got)
	}
}

func TestWriteLimitDeniedTokenQuota(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLimitDenied(rec, "token_quota", 0)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "limit.token_quota_exceeded" {
		t.Fatalf("error code = %q, want limit.token_quota_exceeded", got)
	}
}

func TestWriteLimitDeniedCostBudget(t *testing.T) {
	rec := httptest.NewRecorder()
	writeLimitDenied(rec, "cost_budget", 0)
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402", rec.Code)
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "limit.cost_budget_exceeded" {
		t.Fatalf("error code = %q, want limit.cost_budget_exceeded", got)
	}
	if got := rec.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want absent for a cost-budget denial", got)
	}
}

// --- Full-stack wiring: Admit before Resolve, Record in recordUsage -------
//
// These reuse newServiceScopeTestServer (service_token_scope_test.go), which
// already builds a full *Server (real routing/resolver, provider.NewMock()
// upstream) with two routable models -- "qwen-coder" (translate path) and
// "native-model" (native passthrough, ResponsesMode=passthrough). New(...)
// always installs a default s.Limiter (backed by the routing.MemoryStore
// those helpers use, which is a genuine no-op store -- see the "CARRY" note in
// docs/superpowers/sdd/2026-08-08-principal-limits/progress.md); each test
// below that needs the limiter to actually deny REPLACES srv.Limiter with one
// backed by a fakePrincipalStore (from principal_limits_test.go) so the
// exact-threshold behavior is deterministic and independent of the store
// driver.

// TestPrincipalLimiterNoopIntegration is the no-op regression the design spec
// (§10) requires: with NO principal_limits configured for anyone (the default
// s.Limiter New() installs, backed by the real routing.MemoryStore -- exactly
// what every OTHER gateway test in this package exercises), every one of the
// three inference handlers, for BOTH a service token and a plain user token,
// behaves exactly as it did before this feature existed: served, 200.
func TestPrincipalLimiterNoopIntegration(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   string
		secret string
	}{
		{"chat completions, service token", "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"},
		{"responses, service token", "/v1/responses", `{"model":"qwen-coder","input":"hi"}`, "svc-secret"},
		{"anthropic messages, service token", "/v1/messages", `{"model":"qwen-coder","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, "svc-secret"},
		{"chat completions, user token", "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "dev-secret"},
		{"native passthrough, service token", "/v1/responses", `{"model":"native-model","input":"hi"}`, "svc-secret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, bearerRequest(http.MethodPost, tt.path, tt.body, tt.secret))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (no principal limits configured must be a full no-op), body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestPrincipalLimiterRateLimitDenies drives a real request through
// handleOpenAIChat with a 1-request/60s rate limit configured for the calling
// service principal: the first request is served, the immediately-following
// second request is denied 429 with Retry-After -- all BEFORE Resolve (the
// resolve-error/no-route usage event that a genuine Resolve attempt would
// leave behind never appears, since the model is perfectly routable).
func TestPrincipalLimiterRateLimitDenies(t *testing.T) {
	srv, tokens, recorder := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
	fakeStore.setConfig(p, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body = %s", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.rate_limited" {
		t.Fatalf("error code = %q, want limit.rate_limited", got)
	}
	if got := rec2.Header().Get("Retry-After"); got == "" {
		t.Fatalf("Retry-After header missing on a rate-limit denial")
	}
	// The denied second request must never have reached Resolve/the upstream:
	// exactly one usage event (the first, served, request) is recorded.
	if n := len(recorder.All()); n != 1 {
		t.Fatalf("recorded usage events = %d, want 1 (the denied request must not record a second event)", n)
	}
}

// TestPrincipalLimiterRateLimitDeniesNativePassthrough proves the SAME Admit
// gate applies on the tryProxyNative path (native-model, ResponsesMode=
// passthrough) -- the choke point the brief calls out as a separate call site
// from the three translate handlers.
func TestPrincipalLimiterRateLimitDeniesNativePassthrough(t *testing.T) {
	srv, tokens, recorder := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
	fakeStore.setConfig(p, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, "/v1/responses", `{"model":"native-model","input":"hi"}`, "svc-secret"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, "/v1/responses", `{"model":"native-model","input":"hi"}`, "svc-secret"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429, body = %s", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.rate_limited" {
		t.Fatalf("error code = %q, want limit.rate_limited", got)
	}
	if n := len(recorder.All()); n != 1 {
		t.Fatalf("recorded usage events = %d, want 1 (the denied native-passthrough request must not record a second event)", n)
	}
}

// TestPrincipalLimiterRateLimitCountsOnceOnResponsesNonNativeFallback is the
// regression for a reviewer-caught Critical: /v1/responses and /v1/messages
// both attempt native passthrough (tryProxyNative) BEFORE falling back to
// their own translate handler when the resolved application is NOT
// native-passthrough-enabled -- the common, default-off case (here,
// "qwen-coder" via app_mock_comp, whose ResponsesMode/MessagesMode are both
// left at the zero value, i.e. translate; see seedGatewayTestRoutes). Both call sites call
// admitPrincipal at their own pre-Resolve choke point, but they are the SAME
// client request: admitPrincipal must run Limiter.Admit at MOST once for it.
// Before the fix, tryProxyNative's admitPrincipal call consumed the rate
// bucket, then fell through (native passthrough not enabled) to the translate
// handler's admitPrincipal call, which consumed it AGAIN -- so a principal
// with RateRequests:1 got 429 on its very first, ordinary, non-native request.
func TestPrincipalLimiterRateLimitCountsOnceOnResponsesNonNativeFallback(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"responses", "/v1/responses", `{"model":"qwen-coder","input":"hi"}`},
		{"anthropic messages", "/v1/messages", `{"model":"qwen-coder","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, tokens, _ := newServiceScopeTestServer(t)
			addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

			fakeStore := newFakePrincipalStore()
			p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
			fakeStore.setConfig(p, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
			now := utc(2026, 3, 15, 12, 0, 0)
			srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

			rec1 := httptest.NewRecorder()
			srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, tt.path, tt.body, "svc-secret"))
			if rec1.Code != http.StatusOK {
				t.Fatalf("first request (non-native fallback) status = %d, want 200 -- admission must count exactly once for one client request even though tryProxyNative also ran, body = %s", rec1.Code, rec1.Body.String())
			}

			rec2 := httptest.NewRecorder()
			srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, tt.path, tt.body, "svc-secret"))
			if rec2.Code != http.StatusTooManyRequests {
				t.Fatalf("second request status = %d, want 429, body = %s", rec2.Code, rec2.Body.String())
			}
			if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.rate_limited" {
				t.Fatalf("error code = %q, want limit.rate_limited", got)
			}
		})
	}
}

// TestPrincipalLimiterRequestQuotaDeniesAfterRecord is the required
// "Record after a successful response increments so the next request is
// denied" integration: the first request is served UNDER quota (aggregate 0 <
// 1); recordUsage's post-response Record call must bump the in-memory
// aggregate cache to 1 with NO further store read, so the immediately-
// following second request is denied purely from that in-memory bump (proven
// by leaving the fake store's own stored aggregate at its stale 0 the whole
// time -- if Record were a no-op, or recordUsage never called it, the second
// request would incorrectly see the stale under-quota value and be served).
func TestPrincipalLimiterRequestQuotaDeniesAfterRecord(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
	fakeStore.setConfig(p, routing.LimitConfig{RequestQuota: 1, RequestQuotaPeriod: "day"})
	fakeStore.setAggregate(p, 0, 0, 0) // left untouched for the rest of the test
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 (Record must have bumped the request count), body = %s", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.request_quota_exceeded" {
		t.Fatalf("error code = %q, want limit.request_quota_exceeded", got)
	}
}

// TestPrincipalLimiterTokenQuotaDeniesAfterRecord proves the TOKENS argument
// Record receives is really the served response's total token count (not a
// stub/zero): a TokenQuota threshold below what the mock provider's single
// response produces is crossed by the FIRST request's Record call alone, so
// the second request is denied purely from that bump (the fake store's own
// aggregate again stays at its stale 0 the whole test).
func TestPrincipalLimiterTokenQuotaDeniesAfterRecord(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
	// The mock provider's response to "hi" produces well over 1 token (see
	// mockText in internal/provider/mock.go: input + echoed-response word
	// count), so a threshold of 1 is guaranteed to be crossed by one request.
	fakeStore.setConfig(p, routing.LimitConfig{TokenQuota: 1, TokenQuotaPeriod: "day"})
	fakeStore.setAggregate(p, 0, 0, 0)
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429 (Record must have bumped the token count from the served response's real usage), body = %s", rec2.Code, rec2.Body.String())
	}
	if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.token_quota_exceeded" {
		t.Fatalf("error code = %q, want limit.token_quota_exceeded", got)
	}
}

// TestPrincipalLimiterCostBudgetDenies proves the 402 cost-budget mapping via
// the STORE aggregate path (Admit's own UsageAggregateSince-backed check,
// independent of Record -- which always contributes 0 cost at record time;
// see recordUsage's Record call site for why that is the correct, non-
// approximate value there): a pre-existing aggregate cost already at the
// budget denies the very first request.
func TestPrincipalLimiterCostBudgetDenies(t *testing.T) {
	srv, tokens, _ := newServiceScopeTestServer(t)
	addServiceToken(tokens, "svc-secret", auth.Token{ID: "tok_svc", ServiceID: "svc_1", ServiceName: "Svc"})

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeService, ID: "svc_1"}
	fakeStore.setConfig(p, routing.LimitConfig{CostBudget: 5.0, CostBudgetPeriod: "month"})
	fakeStore.setAggregate(p, 0, 0, 5.0)
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "svc-secret"))
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("status = %d, want 402, body = %s", rec.Code, rec.Body.String())
	}
	if got := decodeErrorCode(t, rec.Body.Bytes()); got != "limit.cost_budget_exceeded" {
		t.Fatalf("error code = %q, want limit.cost_budget_exceeded", got)
	}
}

// TestPrincipalLimiterAppliesToUserToken proves the limiter is not
// service-token-only: a plain user token (no ServiceID) is gated against its
// OWN user-principal limits exactly the same way (admins are not exempt,
// design spec §3). "dev-secret"/"usr_dev" is the seeded admin user token from
// newServiceScopeTestServer.
func TestPrincipalLimiterAppliesToUserToken(t *testing.T) {
	srv, _, _ := newServiceScopeTestServer(t)

	fakeStore := newFakePrincipalStore()
	p := Principal{Type: routing.PrincipalTypeUser, ID: "usr_dev"}
	fakeStore.setConfig(p, routing.LimitConfig{RateRequests: 1, RateWindowSeconds: 60})
	now := utc(2026, 3, 15, 12, 0, 0)
	srv.Limiter = NewPrincipalLimiter(fakeStore, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec1 := httptest.NewRecorder()
	srv.ServeHTTP(rec1, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "dev-secret"))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200, body = %s", rec1.Code, rec1.Body.String())
	}

	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, bearerRequest(http.MethodPost, "/v1/chat/completions", `{"model":"qwen-coder","messages":[{"role":"user","content":"hi"}]}`, "dev-secret"))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request (user token, admin) status = %d, want 429 -- admins are not exempt from principal limits", rec2.Code)
	}
	if got := decodeErrorCode(t, rec2.Body.Bytes()); got != "limit.rate_limited" {
		t.Fatalf("error code = %q, want limit.rate_limited", got)
	}
}

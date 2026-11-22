// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strings"
	"testing"
)

// setSendUsageLimits sets the owner token's usage-limit opt-in (#420) in the
// gateway's bearer store; the next request resolves the new value.
func (f *windowsFixture) setSendUsageLimits(t *testing.T, on bool) {
	t.Helper()
	f.setSendUsageLimitsFor(t, vaOwnerSecret, on)
}

// setSendUsageLimitsFor is setSendUsageLimits for the token behind secret.
func (f *windowsFixture) setSendUsageLimitsFor(t *testing.T, secret string, on bool) {
	t.Helper()
	store, ok := f.srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("bearer store is %T, want *auth.TokenStore", f.srv.Tokens)
	}
	tok, ok := store.LookupBearer("Bearer " + secret)
	if !ok {
		t.Fatal("token not found")
	}
	tok.SendUsageLimits = on
	store.UpdateToken(tok)
}

func requireNoUnifiedResult(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k := range rec.Result().Header {
		if strings.HasPrefix(strings.ToLower(k), rlp) {
			t.Fatalf("unexpected unified header %s: %v", k, rec.Result().Header)
		}
	}
}

func TestMessagesOptOutRelaysNoUnifiedHeaders(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.prov.vendorHdr = subscriptionWindowHeaders()
	f.prov.vendorHdr.Set("X-Should-Retry", "true")

	rec := f.post(t, vaOwnerSecret, "/v1/messages", subMessagesModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoUnifiedResult(t, rec)
	h := rec.Result().Header
	if h.Get("Retry-After") != "30" || h.Get("X-Should-Retry") != "true" {
		t.Fatalf("retry headers not relayed: %v", h)
	}
}

func TestMessagesOptOutAttachesNoCachedWindows(t *testing.T) {
	cases := []struct {
		name   string
		mode   routing.EndpointMode
		stream bool
	}{
		{"passthrough", routing.EndpointModePassthrough, false},
		{"translate buffered", routing.EndpointModeTranslate, false},
		{"translate stream", routing.EndpointModeTranslate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsFixture(t, tc.mode)
			f.observeViaVendor(t, vaOwnerSecret)
			requireWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, tc.stream))

			f.setSendUsageLimits(t, false)
			rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, tc.stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireNoUnifiedResult(t, rec)
		})
	}
}

func TestMessagesOptOutObservesNothing(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.observeViaVendor(t, vaOwnerSecret)

	f.setSendUsageLimits(t, true)
	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoUnifiedResult(t, rec)
}

func TestMessagesOptOutNoPrincipalStanding(t *testing.T) {
	cases := []struct {
		name   string
		mode   routing.EndpointMode
		model  string
		stream bool
	}{
		{"native", routing.EndpointModePassthrough, windowsLocalModel, false},
		{"translate buffered", routing.EndpointModeTranslate, windowsLocalModel, false},
		{"translate stream", routing.EndpointModeTranslate, windowsLocalModel, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWindowsFixture(t, tc.mode)
			f.setSendUsageLimits(t, false)
			f.capPrincipal()
			rec := f.post(t, vaOwnerSecret, "/v1/messages", tc.model, tc.stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireNoUnifiedResult(t, rec)
		})
	}
}

// blockOwner exhausts usr_va_a's token quota.
func (f *windowsFixture) blockOwner() {
	p := Principal{Type: routing.PrincipalTypeUser, ID: "usr_va_a"}
	store := newFakePrincipalStore()
	store.setConfig(p, routing.LimitConfig{TokenQuota: 100, TokenQuotaPeriod: "day"})
	store.setAggregate(p, 0, 100, 0)
	f.srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{})
}

func TestMessagesBlockOptOut(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.blockOwner()

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	h := rec.Result().Header
	if h.Get("Retry-After") == "" || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("retry headers missing: %v", h)
	}
	requireNoUnifiedResult(t, rec)
	body := rec.Body.String()
	if !strings.Contains(body, `"billing_error"`) || !strings.Contains(body, "token quota reached · resets ") ||
		!strings.Contains(body, "ask your gateway administrator to raise it") {
		t.Fatalf("billing_error body changed: %s", body)
	}
}

func TestMessagesOptInUnchanged(t *testing.T) {
	// Native vendor relay.
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.prov.vendorHdr = subscriptionWindowHeaders()
	rec := f.post(t, vaOwnerSecret, "/v1/messages", subMessagesModel, false)
	if h := rec.Result().Header; h.Get(rlp+"status") != "allowed" || h.Get(rlp+"5h-utilization") != "0.42" || h.Get("Retry-After") != "30" {
		t.Fatalf("relay changed: %v", h)
	}
	// Cache attach.
	requireWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false))
	// Standing.
	f.capPrincipal()
	requireStandingAttached(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false))
	// Translate standing.
	for _, stream := range []bool{false, true} {
		g := newWindowsFixture(t, routing.EndpointModeTranslate)
		g.capPrincipal()
		requireStandingAttached(t, g.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, stream))
	}
	// Block headers.
	f.blockOwner()
	blocked := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	h := blocked.Result().Header
	if blocked.Code != http.StatusTooManyRequests || h.Get(rlp+"status") != "rejected" || h.Get(rlp+"overage-disabled-reason") != "org_spend_cap_reached" || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("block changed: %d %v", blocked.Code, h)
	}
}

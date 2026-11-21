// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/routing"
	"strconv"
	"testing"
)

// capPrincipal gives usr_va_a a 1000 tokens/day quota with 800 used, so every
// admitted request reports the token quota at 80 %.
func (f *windowsFixture) capPrincipal() {
	p := Principal{Type: routing.PrincipalTypeUser, ID: "usr_va_a"}
	store := newFakePrincipalStore()
	store.setConfig(p, routing.LimitConfig{TokenQuota: 1000, TokenQuotaPeriod: "day"})
	store.setAggregate(p, 0, 800, 0)
	f.srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{})
}

func requireStandingAttached(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Result().Header
	if h.Get(rlp+"overage-utilization") != "0.80" || h.Get(rlp+"overage-surpassed-threshold") != "0.75" || h.Get(rlp+"status") != "allowed_warning" {
		t.Fatalf("principal standing not attached: %v", h)
	}
	if h.Get(rlp+"representative-claim") != "overage" || h.Get(rlp+"overage-status") != "allowed_warning" {
		t.Fatalf("overage window not described: %v", h)
	}
}

func TestStandingAttachedOnLocalPassthrough(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireStandingAttached(t, rec)
}

func TestStandingAttachedOnTranslateStreamAndBuffered(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "buffered", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			f := newWindowsFixture(t, routing.EndpointModeTranslate)
			f.capPrincipal()

			rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireStandingAttached(t, rec)
		})
	}
}

func TestStandingWithSubscriptionRelay(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.prov.vendorHdr = subscriptionWindowHeaders()
	f.prov.vendorHdr.Set(rlp+"overage-status", "rejected")

	rec := f.post(t, vaOwnerSecret, "/v1/messages", subMessagesModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireStandingAttached(t, rec)
	h := rec.Result().Header
	if h.Get(rlp+"5h-utilization") != "0.42" || h.Get(rlp+"7d-utilization") != "0.10" || h.Get(rlp+"5h-reset") == "" {
		t.Fatalf("subscription windows not kept: %v", h)
	}
	if got := h.Get(rlp + "overage-status"); got != "allowed_warning" {
		t.Fatalf("overage-status = %q, want the gateway's own (the upstream's rejected is gone)", got)
	}

	// #407 is unchanged: the cache observed the upstream set, not the overlay. The
	// follow-up runs without limits, so any gateway field on it would have come out
	// of the cache.
	f.srv.Limiter = NewPrincipalLimiter(newFakePrincipalStore(), PrincipalLimiterOptions{})
	local := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if local.Code != http.StatusOK {
		t.Fatalf("local status = %d, body = %s", local.Code, local.Body.String())
	}
	lh := local.Result().Header
	if lh.Get(rlp+"5h-utilization") != "0.42" || lh.Get(rlp+"7d-utilization") != "0.10" {
		t.Fatalf("cached windows not attached: %v", lh)
	}
	if got := lh.Get(rlp + "representative-claim"); got != "five_hour" {
		t.Fatalf("representative-claim = %q, want the upstream's five_hour", got)
	}
	for _, name := range []string{"overage-utilization", "overage-status", "overage-reset", "overage-surpassed-threshold"} {
		if got := lh.Get(rlp + name); got != "" {
			t.Fatalf("%s = %q leaked from the overlay into the cache: %v", name, got, lh)
		}
	}
}

func TestStandingWithCachedWindows(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeViaVendor(t, vaOwnerSecret)
	f.capPrincipal()

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireStandingAttached(t, rec)
	h := rec.Result().Header
	if h.Get(rlp+"5h-utilization") != "0.42" || h.Get(rlp+"7d-utilization") != "0.10" {
		t.Fatalf("cached windows missing: %v", h)
	}
}

func TestStandingNotAttachedToOtherFlavors(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModeTranslate)
	f.capPrincipal()
	for _, stream := range []bool{false, true} {
		rec := f.post(t, vaOwnerSecret, "/v1/chat/completions", windowsLocalModel, stream)
		if rec.Code != http.StatusOK {
			t.Fatalf("stream=%v status = %d, body = %s", stream, rec.Code, rec.Body.String())
		}
		requireNoUnified(t, rec)
	}
}

func TestStandingAbsentWithoutLimits(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	store := newFakePrincipalStore()
	f.srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{})

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoUnified(t, rec)

	// With windows cached, #407's headers are all there is.
	f.observeViaVendor(t, vaOwnerSecret)
	rec = f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	requireWindowsAttached(t, rec)
	if rec.Header().Get(rlp+"representative-claim") != "five_hour" || rec.Header().Get(rlp+"overage-utilization") != "" {
		t.Fatalf("gateway headers present without limits: %v", rec.Header())
	}
}

func TestStandingNotAttachedOnError(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.prov.localStatus = http.StatusInternalServerError

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	requireNoUnified(t, rec)
}

// TestStandingResetIsPeriodEnd pins the reset the overlay reports to the end of the
// limiter's current UTC day, independent of the subscription windows' resets.
func TestStandingResetIsPeriodEnd(t *testing.T) {
	f := newWindowsFixture(t, routing.EndpointModePassthrough)
	p := Principal{Type: routing.PrincipalTypeUser, ID: "usr_va_a"}
	store := newFakePrincipalStore()
	store.setConfig(p, routing.LimitConfig{TokenQuota: 1000, TokenQuotaPeriod: "day"})
	now := utc(2026, 11, 21, 13, 30, 0)
	f.srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{Now: clockAt(&now)})

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	want := strconv.FormatInt(utc(2026, 11, 22, 0, 0, 0).Unix(), 10)
	if got := rec.Header().Get(rlp + "reset"); got != want {
		t.Fatalf("reset = %q, want %s", got, want)
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// setCodexModelSwitchPrompt sets the owner token's codex_model_switch_prompt switch
// (#420) in the gateway's bearer store; the next request resolves the new value.
func (f *windowsFixture) setCodexModelSwitchPrompt(t *testing.T, on bool) {
	t.Helper()
	store, ok := f.srv.Tokens.(*auth.TokenStore)
	if !ok {
		t.Fatalf("bearer store is %T, want *auth.TokenStore", f.srv.Tokens)
	}
	tok, ok := store.LookupBearer("Bearer " + vaOwnerSecret)
	if !ok {
		t.Fatal("token not found")
	}
	tok.CodexModelSwitchPrompt = on
	store.UpdateToken(tok)
}

// codexHeaderNames returns the sorted lower-case x-codex-* header names of h.
func codexHeaderNames(h http.Header) []string {
	var out []string
	for k := range h {
		if strings.HasPrefix(strings.ToLower(k), codexRateLimitPrefix) {
			out = append(out, strings.ToLower(k))
		}
	}
	sort.Strings(out)
	return out
}

func requireNoCodexResult(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if names := codexHeaderNames(rec.Result().Header); len(names) != 0 {
		t.Fatalf("unexpected Codex headers %v: %v", names, rec.Result().Header)
	}
}

func TestCodexOptOutRelaysNoFamily(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	h := codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	h.Set("x-codex-credits-has-credits", "true")
	h.Set("x-codex-credits-unlimited", "false")
	h.Set("x-codex-promo-message", "vendor promo")
	h.Set("X-Should-Retry", "true")
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusOK, h
	f.cprov.mu.Unlock()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodexResult(t, rec)
	got := rec.Result().Header
	if got.Get("Retry-After") != "30" || got.Get("X-Should-Retry") != "true" {
		t.Fatalf("retry headers not relayed: %v", got)
	}

	// Positive control: the same response relays the family with the switch on.
	f.setSendUsageLimits(t, true)
	rec = f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false)
	if got := rec.Result().Header; got.Get("X-Codex-Primary-Used-Percent") != "42.5" || got.Get("X-Codex-Credits-Has-Credits") != "true" {
		t.Fatalf("opt-in relay changed: %v", got)
	}
}

func TestCodexOptOutAttachesNoCachedWindows(t *testing.T) {
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
			f := newCodexWindowsFixture(t, tc.mode)
			f.observeViaVendor(t, vaOwnerSecret)
			if n := len(f.srv.codexRateLimitWindows.entries); n != 1 {
				t.Fatalf("cache entries = %d, want 1 (positive control)", n)
			}
			requireCodexWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, tc.stream), "42.5")

			f.setSendUsageLimits(t, false)
			rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, tc.stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireNoCodexResult(t, rec)
		})
	}
}

func TestCodexOptOutObservesNothing(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.observeViaVendor(t, vaOwnerSecret)
	if n := len(f.srv.codexRateLimitWindows.entries); n != 0 {
		t.Fatalf("opted-out token cached %d entries", n)
	}

	f.setSendUsageLimits(t, true)
	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodexResult(t, rec)

	// Positive control: with the switch on the observation is recorded.
	f.observeViaVendor(t, vaOwnerSecret)
	if n := len(f.srv.codexRateLimitWindows.entries); n != 1 {
		t.Fatalf("opted-in token cached %d entries, want 1", n)
	}
}

func TestCodexOptOutNoPrincipalStanding(t *testing.T) {
	cases := []struct {
		name   string
		mode   routing.EndpointMode
		stream bool
	}{
		{"native", routing.EndpointModePassthrough, false},
		{"translate buffered", routing.EndpointModeTranslate, false},
		{"translate stream", routing.EndpointModeTranslate, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCodexWindowsFixture(t, tc.mode)
			f.capPrincipal()
			requireCodexStandingAttached(t, f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, tc.stream))

			f.setSendUsageLimits(t, false)
			rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, tc.stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireNoCodexResult(t, rec)
		})
	}
}

// requireCodexBlockBody asserts the exact usage_limit_reached body of blockOwner's token quota.
func requireCodexBlockBody(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var body codexLimitBlockedBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := codexLimitBlockedBody{Error: codexLimitBlockedError{
		Type:     "usage_limit_reached",
		Code:     "limit.token_quota_exceeded",
		Message:  body.Error.Message,
		ResetsAt: body.Error.ResetsAt,
	}}
	if !reflect.DeepEqual(body, want) || body.Error.ResetsAt <= 0 ||
		!strings.HasPrefix(body.Error.Message, "token quota reached · resets ") ||
		!strings.HasSuffix(body.Error.Message, "ask your gateway administrator to raise it") {
		t.Fatalf("usage_limit_reached body changed: %s", rec.Body.String())
	}
}

func TestCodexBlockOptOut(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.blockOwner()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	h := rec.Result().Header
	if h.Get("Retry-After") == "" || h.Get("X-Should-Retry") != "false" {
		t.Fatalf("retry headers missing: %v", h)
	}
	if want := []string{"x-codex-promo-message"}; !reflect.DeepEqual(codexHeaderNames(h), want) {
		t.Fatalf("x-codex-* headers = %v, want only %v", codexHeaderNames(h), want)
	}
	if h.Get("X-Codex-Promo-Message") != "OP AI Gateway token quota reached; ask your gateway administrator to raise it" {
		t.Fatalf("promo = %q", h.Get("X-Codex-Promo-Message"))
	}
	requireCodexBlockBody(t, rec)

	// Positive control: opted in, the primary window and credits flags come back.
	f.setSendUsageLimits(t, true)
	h = f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false).Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "100" || h.Get("X-Codex-Credits-Has-Credits") != "true" {
		t.Fatalf("opt-in block changed: %v", h)
	}
}

func TestCodexModelSwitchPromptDropsCreditsFlag(t *testing.T) {
	credits := []string{"x-codex-credits-has-credits", "x-codex-credits-unlimited"}
	// Both switches on: window but no credits flags, on the success standing and the block.
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setCodexModelSwitchPrompt(t, true)
	f.capPrincipal()
	h := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false).Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "80" || h.Get("X-Codex-Limit-Name") != "OP AI Gateway token quota" {
		t.Fatalf("standing window missing: %v", h)
	}
	for _, c := range credits {
		if _, ok := h[http.CanonicalHeaderKey(c)]; ok {
			t.Fatalf("standing carries %s: %v", c, h)
		}
	}
	g := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
	g.setCodexModelSwitchPrompt(t, true)
	g.capPrincipal()
	for _, stream := range []bool{false, true} {
		h := g.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, stream).Result().Header
		if h.Get("X-Codex-Primary-Used-Percent") != "80" {
			t.Fatalf("translate stream=%v standing window missing: %v", stream, h)
		}
		for _, c := range credits {
			if _, ok := h[http.CanonicalHeaderKey(c)]; ok {
				t.Fatalf("translate stream=%v standing carries %s: %v", stream, c, h)
			}
		}
	}
	f.blockOwner()
	h = f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false).Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "100" {
		t.Fatalf("block window missing: %v", h)
	}
	for _, c := range credits {
		if _, ok := h[http.CanonicalHeaderKey(c)]; ok {
			t.Fatalf("block carries %s: %v", c, h)
		}
	}

	// Only send_usage_limits on: the flags are written as before.
	f.setCodexModelSwitchPrompt(t, false)
	h = f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false).Result().Header
	if h.Get("X-Codex-Credits-Has-Credits") != "true" || h.Get("X-Codex-Credits-Unlimited") != "false" {
		t.Fatalf("block credits flags changed: %v", h)
	}
	f.capPrincipal()
	requireCodexStandingAttached(t, f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false))
}

func TestCodexModelSwitchPromptWithoutSendIsMoot(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, false)
	f.setCodexModelSwitchPrompt(t, true)

	f.capPrincipal()
	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodexResult(t, rec)

	f.blockOwner()
	rec = f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d", rec.Code)
	}
	if want := []string{"x-codex-promo-message"}; !reflect.DeepEqual(codexHeaderNames(rec.Result().Header), want) {
		t.Fatalf("x-codex-* headers = %v, want only %v", codexHeaderNames(rec.Result().Header), want)
	}
	requireCodexBlockBody(t, rec)
}

func TestCodexVendorCreditsUntouchedByPromptSwitch(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setCodexModelSwitchPrompt(t, true)
	h := codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	h.Set("x-codex-credits-has-credits", "false")
	h.Set("x-codex-credits-unlimited", "true")
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusOK, h
	f.cprov.mu.Unlock()

	got := f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false).Result().Header
	if got.Get("X-Codex-Credits-Has-Credits") != "false" || got.Get("X-Codex-Credits-Unlimited") != "true" ||
		got.Get("X-Codex-Credits-Balance") != "5" || got.Get("X-Codex-Primary-Used-Percent") != "42.5" {
		t.Fatalf("vendor credits changed: %v", got)
	}
}

func TestWriteCodexLimitBlockedHeaderSets(t *testing.T) {
	r := codexTokenReading(1000, 1000)
	promo := "x-codex-promo-message"
	window := []string{"x-codex-primary-reset-at", "x-codex-primary-used-percent", "x-codex-primary-window-minutes"}
	credits := []string{"x-codex-credits-has-credits", "x-codex-credits-unlimited"}
	all := func(parts ...[]string) []string {
		var out []string
		for _, p := range parts {
			out = append(out, p...)
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name        string
		send, offer bool
		want        []string
	}{
		{"send off", false, false, []string{promo}},
		{"send off popup on", false, true, []string{promo}},
		{"send on popup off", true, false, all([]string{promo}, window, credits)},
		{"send on popup on", true, true, all([]string{promo}, window)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeCodexLimitBlocked(rec, r, codexTestNow, tc.send, tc.offer)
			if rec.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d", rec.Code)
			}
			if got := codexHeaderNames(rec.Header()); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("x-codex-* = %v, want %v", got, tc.want)
			}
			if rec.Header().Get("Retry-After") != "21600" || rec.Header().Get("X-Should-Retry") != "false" {
				t.Fatalf("retry headers = %v", rec.Header())
			}
			var body codexLimitBlockedBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Error.Type != "usage_limit_reached" || body.Error.Message != limitBlockedMessage(r) || body.Error.ResetsAt != codexTestReset.Unix() {
				t.Fatalf("body = %+v", body)
			}
		})
	}
}

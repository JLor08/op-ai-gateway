// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"strconv"
	"strings"
	"testing"
	"time"
)

func requireCodexStandingAttached(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "80" || h.Get("X-Codex-Primary-Window-Minutes") != "1440" ||
		h.Get("X-Codex-Limit-Name") != "OP AI Gateway token quota" || h.Get("X-Codex-Credits-Has-Credits") != "true" {
		t.Fatalf("Codex standing not attached: %v", h)
	}
}

func requireCodexStandingOK(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCodexStandingAttachedOnLocalPassthrough(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireCodexStandingAttached(t, rec)
}

func TestCodexStandingAttachedOnTranslateStreamAndBuffered(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
	f.capPrincipal()

	for _, stream := range []bool{false, true} {
		rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, stream)
		requireCodexStandingOK(t, rec)
		requireCodexStandingAttached(t, rec)
	}
}

func TestCodexStandingYieldsToVendorFigures(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusOK, codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	f.cprov.mu.Unlock()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false)
	requireCodexStandingOK(t, rec)
	h := rec.Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "42.5" || h.Get("X-Codex-Limit-Name") != "codex" || h.Get("X-Codex-Credits-Has-Credits") != "" {
		t.Fatalf("vendor figures not preserved: %v", h)
	}
}

func requireNoCodexSecondary(t *testing.T, h http.Header) {
	t.Helper()
	if h.Get("X-Codex-Secondary-Used-Percent") != "" || h.Get("X-Codex-Secondary-Reset-At") != "" {
		t.Fatalf("cached secondary window survived the takeover: %v", h)
	}
}

func TestCodexStandingTakesOverCachedWindows(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.observeViaVendor(t, vaOwnerSecret)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireCodexStandingAttached(t, rec)
	requireNoCodexSecondary(t, rec.Result().Header)
}

func TestCodexStandingTakesOverCachedWindowsOnTranslate(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
	f.capPrincipal()
	f.observeViaVendor(t, vaOwnerSecret)

	for _, stream := range []bool{false, true} {
		rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, stream)
		requireCodexStandingOK(t, rec)
		requireCodexStandingAttached(t, rec)
		requireNoCodexSecondary(t, rec.Result().Header)
	}
}

func TestCodexStandingYieldsToCachedWindowsBelowThreshold(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipalAt(740)
	f.observeViaVendor(t, vaOwnerSecret)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireCodexWindowsAttached(t, rec, "42.5")
	h := rec.Result().Header
	if got := h.Get("X-Codex-Limit-Name"); got != "codex" {
		t.Fatalf("limit name = %q, want codex", got)
	}
	if got := h.Get("X-Codex-Credits-Has-Credits"); got != "" {
		t.Fatalf("credits header = %q, want none", got)
	}
}

func TestCodexStandingYieldsToCachedWindowsBelowThresholdOnTranslate(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
	f.capPrincipalAt(740)
	f.observeViaVendor(t, vaOwnerSecret)

	for _, stream := range []bool{false, true} {
		rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, stream)
		requireCodexStandingOK(t, rec)
		requireCodexWindowsAttached(t, rec, "42.5")
		h := rec.Result().Header
		if got := h.Get("X-Codex-Limit-Name"); got != "codex" {
			t.Fatalf("stream=%v limit name = %q, want codex", stream, got)
		}
		if got := h.Get("X-Codex-Credits-Has-Credits"); got != "" {
			t.Fatalf("stream=%v credits header = %q, want none", stream, got)
		}
	}
}

// observeWithExtraFamily records the default family plus an x-codex-other-* family
// in the owner's window cache through one vendor request.
func (f *codexWindowsFixture) observeWithExtraFamily(t *testing.T) {
	t.Helper()
	now := time.Now()
	h := codexWindowHeaders(now, time.Hour, 24*time.Hour)
	h.Set("x-codex-other-primary-used-percent", "12")
	h.Set("x-codex-other-primary-window-minutes", "60")
	h.Set("x-codex-other-primary-reset-at", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	h.Set("x-codex-other-limit-name", "other")
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusOK, h
	f.cprov.mu.Unlock()
	if rec := f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false); rec.Code != http.StatusOK {
		t.Fatalf("vendor request status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func TestCodexTakeoverDropsCachedExtraFamily(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeWithExtraFamily(t)
	f.capPrincipal()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireCodexStandingAttached(t, rec)
	for k := range rec.Result().Header {
		if strings.HasPrefix(strings.ToLower(k), "x-codex-other-") {
			t.Fatalf("cached extra family survived the takeover: %v", rec.Result().Header)
		}
	}
}

func TestCodexBelowThresholdKeepsCachedExtraFamily(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeWithExtraFamily(t)
	f.capPrincipalAt(740)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	if got := rec.Result().Header.Get("X-Codex-Other-Primary-Used-Percent"); got != "12" {
		t.Fatalf("cached extra family = %q, want 12: %v", got, rec.Result().Header)
	}
}

func TestCodexTakeoverWithModelSwitchPrompt(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.observeViaVendor(t, vaOwnerSecret)
	f.setCodexModelSwitchPrompt(t, true)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	h := rec.Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != "80" || h.Get("X-Codex-Limit-Name") != "OP AI Gateway token quota" {
		t.Fatalf("takeover not written: %v", h)
	}
	if h.Get("X-Codex-Credits-Has-Credits") != "" || h.Get("X-Codex-Credits-Unlimited") != "" {
		t.Fatalf("credits headers present with the model-switch prompt on: %v", h)
	}
	requireNoCodexSecondary(t, h)
}

func TestCodexStandingForNonEntitledVendorCaller(t *testing.T) {
	mf, accountID := vaMemberFixture(t, routing.VendorShareLevelShared)
	srv := mf.srv
	const vendorModel, memberSecret = "shared-responses-model", "member-codex-standing-secret"
	if err := mf.routes.SetVendorAccountModels(context.Background(), accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: vendorModel, UpstreamModel: vendorModel, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_cs_member", UserID: "usr_va_b", Active: true, SendUsageLimits: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: accountID}}}}, memberSecret)
	cprov := &codexWindowsProvider{windowsProvider: windowsProvider{localStatus: http.StatusOK}, vendorStatus: http.StatusOK, vendorHdr: codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)}
	srv.Provider = cprov
	p := Principal{Type: routing.PrincipalTypeUser, ID: "usr_va_b"}
	store := newFakePrincipalStore()
	store.setConfig(p, routing.LimitConfig{TokenQuota: 1000, TokenQuotaPeriod: "day"})
	store.setAggregate(p, 0, 800, 0)
	srv.Limiter = NewPrincipalLimiter(store, PrincipalLimiterOptions{})
	f := &windowsFixture{srv: srv, prov: &cprov.windowsProvider}

	rec := f.post(t, memberSecret, "/v1/responses", vendorModel, false)
	requireCodexStandingOK(t, rec)
	if got := rec.Result().Header.Get("X-Codex-Primary-Used-Percent"); got == "42.5" {
		t.Fatalf("vendor figures leaked to a non-entitled caller: %v", rec.Result().Header)
	}
	requireCodexStandingAttached(t, rec)
}

func TestCodexStandingNotAttachedOnError(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.prov.localStatus = http.StatusInternalServerError

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireNoCodex(t, rec)
}

func TestCodexStandingAbsentWithoutLimits(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireNoCodex(t, rec)
}

func TestCodexStandingNotOnMessages(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()

	rec := f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), codexRateLimitPrefix) {
			t.Fatalf("unexpected Codex header %s: %v", k, rec.Header())
		}
	}
}

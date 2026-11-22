// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
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

func TestCodexStandingYieldsToCachedWindows(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.capPrincipal()
	f.observeViaVendor(t, vaOwnerSecret)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireCodexStandingOK(t, rec)
	requireCodexWindowsAttached(t, rec, "42.5")
	if got := rec.Result().Header.Get("X-Codex-Limit-Name"); got != "codex" {
		t.Fatalf("limit name = %q, want codex", got)
	}
}

func TestCodexStandingYieldsToCachedWindowsOnTranslate(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
	f.capPrincipal()
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

func TestCodexStandingForNonEntitledVendorCaller(t *testing.T) {
	mf, accountID := vaMemberFixture(t, routing.VendorShareLevelShared)
	srv := mf.srv
	const vendorModel, memberSecret = "shared-responses-model", "member-codex-standing-secret"
	if err := mf.routes.SetVendorAccountModels(context.Background(), accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: vendorModel, UpstreamModel: vendorModel, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_cs_member", UserID: "usr_va_b", Active: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: accountID}}}}, memberSecret)
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

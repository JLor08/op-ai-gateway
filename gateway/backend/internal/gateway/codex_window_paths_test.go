// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"testing"
	"time"
)

const (
	codexWindowsAccountID = "acc_sub_openai_windows"
	codexWindowsModel     = "oai/gpt-5.5"
	codexWindowsUpstream  = "gpt-5.5"
)

// seedSubscriptionOpenAIRoutableAccount stores one ACTIVE OpenAI SUBSCRIPTION vendor
// account owned by ownerID, serving codexWindowsModel -> codexWindowsUpstream. It is the
// OpenAI analog of seedSubscriptionAnthropicRoutableAccount.
func seedSubscriptionOpenAIRoutableAccount(t *testing.T, routes *routing.MemoryStore, cipher *capture.Cipher, accountID, ownerID string, ts vendorauth.TokenSet) {
	t.Helper()
	sealed, err := vendorauth.SealTokenSet(cipher, false, ts)
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if err := routes.CreateVendorAccount(ctx, routing.VendorAccount{
		ID: accountID, OwnerUserID: ownerID, Vendor: routing.VendorOpenAI, AuthType: routing.VendorAuthSubscription,
		Name: accountID, Status: routing.VendorAccountStatusActive, OAuthTokens: sealed, ModelPrefix: "oai/", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("CreateVendorAccount: %v", err)
	}
	if err := routes.SetVendorAccountModels(ctx, accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: codexWindowsModel, UpstreamModel: codexWindowsUpstream, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
}

// codexWindowsProvider answers OpenAI vendor targets with a scripted response and every
// other target as a local model (windowsProvider's behavior).
type codexWindowsProvider struct {
	windowsProvider
	vendorStatus int
	vendorHdr    http.Header
}

func (p *codexWindowsProvider) ProxyNative(ctx context.Context, target routing.Target, path string, body []byte, ct string) (*provider.ProxyResponse, error) {
	if !routing.IsOpenAIVendorProvider(target.Provider) {
		return p.windowsProvider.ProxyNative(ctx, target, path, body, ct)
	}
	p.mu.Lock()
	status, hdr := p.vendorStatus, p.vendorHdr.Clone()
	p.mu.Unlock()
	if hdr == nil {
		hdr = http.Header{}
	}
	hdr.Set("Content-Type", jsonContentType)
	payload := `{"id":"resp_1","output_text":"hi"}`
	if status >= 400 {
		payload = `{"error":{"type":"rate_limit_error","message":"slow down"}}`
	}
	return &provider.ProxyResponse{StatusCode: status, Header: hdr, Body: io.NopCloser(strings.NewReader(payload))}, nil
}

type codexWindowsFixture struct {
	*windowsFixture
	cprov  *codexWindowsProvider
	routes *routing.MemoryStore
}

func newCodexWindowsFixture(t *testing.T, localResponsesMode routing.EndpointMode) *codexWindowsFixture {
	t.Helper()
	srv, routes, _ := newVendorAccountSettingsTestServer(t, true)
	cipher := newDispatchCipher(t)
	srv.Cipher = cipher
	cprov := &codexWindowsProvider{windowsProvider: windowsProvider{localStatus: http.StatusOK}, vendorStatus: http.StatusOK}
	srv.Provider = cprov
	seedSubscriptionOpenAIRoutableAccount(t, routes, cipher, codexWindowsAccountID, "usr_va_a", vendorauth.TokenSet{
		AccessToken: "codex-windows-access", RefreshToken: "r", ExpiresAt: time.Now().Add(time.Hour), AccountID: "acct",
	})
	seedWindowsLocalModel(t, routes, routing.EndpointModeTranslate)
	if localResponsesMode != routing.EndpointModeTranslate {
		app, err := routes.ApplicationByID(context.Background(), "app-local")
		if err != nil {
			t.Fatalf("ApplicationByID: %v", err)
		}
		app.ResponsesMode = localResponsesMode
		if err := routes.UpdateApplication(context.Background(), app); err != nil {
			t.Fatalf("UpdateApplication: %v", err)
		}
	}
	enableVendorAccountsFlag(t, srv)
	f := &codexWindowsFixture{windowsFixture: &windowsFixture{srv: srv, prov: &cprov.windowsProvider}, cprov: cprov, routes: routes}
	// The owner's token opts into usage-limit signals (#420), as the scenarios of
	// this file assume; usage_limit_optin_codex_test.go flips it off.
	f.setSendUsageLimits(t, true)
	return f
}

func (f *codexWindowsFixture) observeViaVendor(t *testing.T, secret string) {
	t.Helper()
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusOK, codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	f.cprov.mu.Unlock()
	if rec := f.post(t, secret, "/v1/responses", codexWindowsModel, false); rec.Code != http.StatusOK {
		t.Fatalf("vendor request status = %d, body = %s", rec.Code, rec.Body.String())
	}
}

func requireCodexWindowsAttached(t *testing.T, rec *httptest.ResponseRecorder, wantPrimary string) {
	t.Helper()
	h := rec.Result().Header
	if h.Get("X-Codex-Primary-Used-Percent") != wantPrimary || h.Get("X-Codex-Secondary-Used-Percent") != "17" ||
		h.Get("X-Codex-Primary-Reset-At") == "" || h.Get("X-Codex-Limit-Name") != "codex" {
		t.Fatalf("Codex windows not attached (want primary %s): %v", wantPrimary, h)
	}
	if h.Get("X-Codex-Credits-Balance") != "" || h.Get("X-Codex-Active-Limit") != "" || h.Get("Retry-After") != "" {
		t.Fatalf("non-window headers attached: %v", h)
	}
}

func requireNoCodex(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	for k := range rec.Header() {
		if strings.HasPrefix(strings.ToLower(k), codexRateLimitPrefix) {
			t.Fatalf("unexpected Codex header %s: %v", k, rec.Header())
		}
	}
}

func TestCodexWindowsObservedFromVendorThenAttachedOnLocalPassthrough(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.observeViaVendor(t, vaOwnerSecret)

	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("local status = %d, body = %s", rec.Code, rec.Body.String())
	}
	requireCodexWindowsAttached(t, rec, "42.5")

	other := f.post(t, vaOtherSecret, "/v1/responses", windowsLocalModel, false)
	if other.Code != http.StatusOK {
		t.Fatalf("other principal status = %d, body = %s", other.Code, other.Body.String())
	}
	requireNoCodex(t, other)
}

func TestCodexWindowsAttachedOnTranslateStreamAndBuffered(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := map[bool]string{false: "buffered", true: "stream"}[stream]
		t.Run(name, func(t *testing.T) {
			f := newCodexWindowsFixture(t, routing.EndpointModeTranslate)
			f.observeViaVendor(t, vaOwnerSecret)

			rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, stream)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			requireCodexWindowsAttached(t, rec, "42.5")

			requireNoCodex(t, f.post(t, vaOtherSecret, "/v1/responses", windowsLocalModel, stream))
		})
	}
}

func TestCodexWindowsNotAttachedOnErrorOrOverFresh(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
		f.observeViaVendor(t, vaOwnerSecret)
		f.prov.localStatus = http.StatusInternalServerError

		rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rec.Code)
		}
		requireNoCodex(t, rec)
	})
	t.Run("fresh", func(t *testing.T) {
		f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
		f.observeViaVendor(t, vaOwnerSecret)
		own := http.Header{}
		own.Set("x-codex-primary-used-percent", "5")
		f.prov.localHdr = own

		rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
		h := rec.Result().Header
		if got := h.Values("X-Codex-Primary-Used-Percent"); len(got) != 1 || got[0] != "5" {
			t.Fatalf("primary used-percent = %v, want the upstream's own [5]", got)
		}
		if h.Get("X-Codex-Secondary-Used-Percent") != "" || h.Get("X-Codex-Limit-Name") != "" {
			t.Fatalf("cached windows mixed into fresh headers: %v", h)
		}
	})
}

// A member's real vendor /v1/responses requests drive recordUsage's observe call: with
// the owner's opt-in on, the member's windows are cached (and shown on a local-model
// answer); after the owner opts out, the next vendor response from that account makes
// recordUsage drop the member's entry, so the member's local answers carry none. Before
// the opt-in, the member's vendor responses cache nothing.
func TestCodexWindowsNotObservedForNonEntitledAndForgottenOnRevocation(t *testing.T) {
	mf, accountID := vaMemberFixture(t, routing.VendorShareLevelShared)
	srv := mf.srv
	const vendorModel, memberSecret = "shared-responses-model", "member-codex-windows-secret"
	if err := mf.routes.SetVendorAccountModels(context.Background(), accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: vendorModel, UpstreamModel: vendorModel, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	seedWindowsLocalModel(t, mf.routes, routing.EndpointModeTranslate)
	app, err := mf.routes.ApplicationByID(context.Background(), "app-local")
	if err != nil {
		t.Fatalf("ApplicationByID: %v", err)
	}
	app.ResponsesMode = routing.EndpointModePassthrough
	if err := mf.routes.UpdateApplication(context.Background(), app); err != nil {
		t.Fatalf("UpdateApplication: %v", err)
	}
	srv.Tokens.(*auth.TokenStore).AddPlainToken(auth.Token{ID: "tok_cw_member", UserID: "usr_va_b", Active: true, SendUsageLimits: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: accountID}}}}, memberSecret)
	cprov := &codexWindowsProvider{windowsProvider: windowsProvider{localStatus: http.StatusOK}, vendorStatus: http.StatusOK, vendorHdr: codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)}
	srv.Provider = cprov
	f := &windowsFixture{srv: srv, prov: &cprov.windowsProvider}

	vendorCall := func() {
		t.Helper()
		if rec := f.post(t, memberSecret, "/v1/responses", vendorModel, false); rec.Code != http.StatusOK {
			t.Fatalf("vendor request status = %d, body = %s", rec.Code, rec.Body.String())
		}
	}
	localCall := func() *httptest.ResponseRecorder {
		t.Helper()
		rec := f.post(t, memberSecret, "/v1/responses", windowsLocalModel, false)
		if rec.Code != http.StatusOK {
			t.Fatalf("local request status = %d, body = %s", rec.Code, rec.Body.String())
		}
		return rec
	}
	setSharing := func(on bool) {
		t.Helper()
		body := map[bool]string{true: `{"enabled":true}`, false: `{"enabled":false}`}[on]
		if rec := vaDo(t, srv, http.MethodPut, shareUsagePath(accountID), vaSystemSecret, body); rec.Code != http.StatusOK {
			t.Fatalf("share-usage PUT = %d, body = %s", rec.Code, rec.Body.String())
		}
	}

	// Opt-in off: the member's vendor response is not observed.
	vendorCall()
	requireNoCodex(t, localCall())

	// Opt-in on: observed, shown on the next local answer.
	setSharing(true)
	vendorCall()
	requireCodexWindowsAttached(t, localCall(), "42.5")

	// Opt-in off again: the next vendor response drops the member's entry.
	setSharing(false)
	vendorCall()
	requireNoCodex(t, localCall())
}

func TestCodexWindowsObservedFrom429(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	h := codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	h.Set("x-codex-primary-used-percent", "100")
	f.cprov.mu.Lock()
	f.cprov.vendorStatus, f.cprov.vendorHdr = http.StatusTooManyRequests, h
	f.cprov.mu.Unlock()

	rec := f.post(t, vaOwnerSecret, "/v1/responses", codexWindowsModel, false)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("vendor status = %d, want 429", rec.Code)
	}
	if got := rec.Result().Header.Values("X-Codex-Primary-Used-Percent"); len(got) != 1 || got[0] != "100" {
		t.Fatalf("429 primary used-percent = %v, want its own [100]", got)
	}

	requireCodexWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false), "100")
}

func TestCodexWindowsNeedAPrincipal(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	target := routing.Target{Provider: routing.ProviderVendorOpenAISubscription}
	f.srv.observeCodexRateLimitWindows(auth.Token{ID: "t", SendUsageLimits: true}, target, codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour))
	if n := len(f.srv.codexRateLimitWindows.entries); n != 0 {
		t.Fatalf("a token without a principal cached %d entries", n)
	}
}

func TestMessagesGetNoCodexWindowsAndResponsesGetNoUnified(t *testing.T) {
	f := newCodexWindowsFixture(t, routing.EndpointModePassthrough)
	f.setSendUsageLimits(t, true) // the Messages half needs the Claude Code opt-in (#420)
	f.observeViaVendor(t, vaOwnerSecret)
	owner := auth.Token{ID: "tok_va_a", UserID: "usr_va_a", Active: true, SendUsageLimits: true}
	anthropic := routing.Target{Provider: routing.ProviderVendorAnthropic, VendorAccountID: "acc_anthropic", VendorAccountOwnerUserID: "usr_va_a"}
	f.srv.observeRateLimitWindows(owner, anthropic, subscriptionWindowHeaders())

	requireNoCodex(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false))
	rec := f.post(t, vaOwnerSecret, "/v1/responses", windowsLocalModel, false)
	requireNoUnified(t, rec)
	requireCodexWindowsAttached(t, rec, "42.5")
	requireWindowsAttached(t, f.post(t, vaOwnerSecret, "/v1/messages", windowsLocalModel, false))
}

// Through the real request path, a translated /v1/chat/completions answer from an
// OpenAI vendor account is captured without the Codex family for a member the owner
// did not opt in, and with it for the owner. The account is shared to the member
// through a resource group (the analog of the Anthropic capture test).
func TestTranslatedCaptureRedactsCodexHeadersThroughRequestPath(t *testing.T) {
	f, accountID := vaMemberFixture(t, routing.VendorShareLevelShared)
	srv := f.srv
	const gatewayModel, ownerSecret, memberSecret = "shared-chat-model", "owner-codex-capture-secret", "member-codex-capture-secret"
	if err := f.routes.SetVendorAccountModels(context.Background(), accountID, []routing.VendorAccountModel{
		{AccountID: accountID, GatewayModel: gatewayModel, UpstreamModel: gatewayModel, APIFlavor: routing.APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("SetVendorAccountModels: %v", err)
	}
	tokens := srv.Tokens.(*auth.TokenStore)
	tokens.AddPlainToken(auth.Token{ID: "tok_cap_owner", UserID: "usr_va_sys", Active: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{All: true}}, ownerSecret)
	tokens.AddPlainToken(auth.Token{ID: "tok_cap_member", UserID: "usr_va_b", Active: true, Scopes: []string{"gateway:use"}, VendorAccess: auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: accountID}}}}, memberSecret)

	cs := &fakeCaptureStore{}
	srv.Captures = cs
	cipher, err := capture.New(testCaptureKey)
	if err != nil {
		t.Fatalf("capture.New: %v", err)
	}
	srv.Cipher = cipher
	srv.CaptureOverride = func() bool { return true }
	hdr := codexWindowHeaders(time.Now(), time.Hour, 24*time.Hour)
	hdr.Set("Content-Type", "application/json")
	srv.Provider = captureSinkVendorProvider{respHeaders: hdr}

	capturedHeaders := func(secret string) http.Header {
		t.Helper()
		cs.mu.Lock()
		before := len(cs.saved)
		cs.mu.Unlock()
		body := `{"model":"` + gatewayModel + `","messages":[{"role":"user","content":"hi"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
		}
		cs.mu.Lock()
		defer cs.mu.Unlock()
		if len(cs.saved) != before+1 {
			t.Fatalf("captures saved = %d, want %d", len(cs.saved), before+1)
		}
		return http.Header(decryptCapture(t, cs.saved[len(cs.saved)-1].Blob).TranslatedRespHeaders)
	}

	member := capturedHeaders(memberSecret)
	if hasCodexRateLimitHeader(member) {
		t.Fatalf("non-entitled capture kept the Codex family: %v", member)
	}
	if member.Get("Content-Type") != "application/json" {
		t.Fatalf("unrelated header lost: %v", member)
	}
	if owner := capturedHeaders(ownerSecret); !hasCodexRateLimitHeader(owner) {
		t.Fatalf("owner capture lost the Codex family: %v", owner)
	}
}

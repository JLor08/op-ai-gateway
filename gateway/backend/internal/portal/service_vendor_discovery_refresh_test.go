// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	refreshTestOldAccess   = "tok-old-access-DO-NOT-ECHO-1c2d"
	refreshTestFreshAccess = "tok-fresh-access-DO-NOT-ECHO-7e8f"
	refreshTestRefresh     = "tok-refresh-DO-NOT-ECHO-5a6b"
)

// fakeTokenRefresher stands in for the gateway's locked refresher: it records
// every account it was asked about and, when fresh is set, persists that token set
// the way the real one does (reseal + write), so a re-read sees the new token.
type fakeTokenRefresher struct {
	t          *testing.T
	svc        *Service
	routeStore *routing.MemoryStore

	mu    sync.Mutex
	calls []string
	// fresh is the token set persisted on each call; nil persists nothing (a
	// refresher that "succeeds" without having refreshed anything).
	fresh *vendorauth.TokenSet
	err   error
}

func (f *fakeTokenRefresher) refresh(_ context.Context, accountID string) error {
	f.mu.Lock()
	f.calls = append(f.calls, accountID)
	fresh, err := f.fresh, f.err
	f.mu.Unlock()
	if err != nil {
		return err
	}
	if fresh != nil {
		setDiscoveryTokens(f.t, f.svc, f.routeStore, accountID, *fresh)
	}
	return nil
}

func (f *fakeTokenRefresher) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// installTokenRefresher wires a fake refresher into svc. It persists a fresh,
// unexpired token set (fresh access token, same refresh token, account id kept).
func installTokenRefresher(t *testing.T, svc *Service, routeStore *routing.MemoryStore) *fakeTokenRefresher {
	t.Helper()
	f := &fakeTokenRefresher{t: t, svc: svc, routeStore: routeStore}
	f.fresh = &vendorauth.TokenSet{
		AccessToken: refreshTestFreshAccess, RefreshToken: refreshTestRefresh,
		ExpiresAt: discoveryTestNow.Add(time.Hour), AccountID: discoveryTestAccount,
	}
	svc.vendorDiscovery.tokenRefresher = f.refresh
	return f
}

// expiredSubscription is a subscription account of vendor holding an access token
// that is past its expiry with a refresh token that can renew it.
func expiredSubscription(t *testing.T, svc *Service, routeStore *routing.MemoryStore, vendor, prefix string) VendorAccountDTO {
	t.Helper()
	acc := createTestVendorAccount(t, svc, ownerToken(), CreateVendorAccountRequest{
		Vendor: vendor, AuthType: routing.VendorAuthSubscription, Name: "Stale", ModelPrefix: prefix,
	})
	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{
		AccessToken: refreshTestOldAccess, RefreshToken: refreshTestRefresh,
		ExpiresAt: discoveryTestNow.Add(-time.Hour), AccountID: discoveryTestAccount,
	})
	return acc
}

// --- the token refresher seam --------------------------------------------------------

// THE fix for "refresh models" on an expired-but-refreshable subscription: the
// service asks the gateway's locked refresher to renew the token, RE-READS the
// account and discovers with the fresh token, and the rows are written.
func TestRefreshVendorAccountModelsRefreshesAnExpiredTokenThenDiscovers(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"), discovered("gpt-6.1-sol", "GPT-6.1 Sol"))
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "chatgpt/")

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshOK || res.Discovered != 2 {
		t.Fatalf("result = %+v, want ok / 2", res)
	}
	if got := refresher.recorded(); !reflect.DeepEqual(got, []string{acc.ID}) {
		t.Fatalf("refresher calls = %v, want exactly one for %s", got, acc.ID)
	}
	// The vendor was asked with the FRESH token, never the expired one.
	if call := fake.onlyCall(t); call.credential != refreshTestFreshAccess || call.accountID != discoveryTestAccount {
		t.Fatalf("discoverer call = %+v, want the refreshed access token and the account id", call)
	}
	want := []routing.VendorAccountModel{
		modelRow(acc.ID, "chatgpt/gpt-6-luna", "gpt-6-luna", routing.APIFlavorOpenAI, "GPT-6 Luna"),
		modelRow(acc.ID, "chatgpt/gpt-6.1-sol", "gpt-6.1-sol", routing.APIFlavorOpenAI, "GPT-6.1 Sol"),
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("stored rows = %+v, want the discovered rows %+v", got, want)
	}
	if len(dto.Models) != 2 {
		t.Fatalf("dto models = %+v, want the discovered models", dto.Models)
	}
	for _, secret := range []string{refreshTestOldAccess, refreshTestFreshAccess, refreshTestRefresh} {
		requireNoSubstring(t, res, secret)
		requireNoSubstring(t, dto, secret)
	}
}

// requireNoSubstring fails if v's JSON form contains secret.
func requireNoSubstring(t *testing.T, v any, secret string) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %T: %v", v, err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("%T leaks %q: %s", v, secret, raw)
	}
}

// Anthropic subscription tokens are opaque but carry an expiry from expires_in, so
// the same refresh-then-discover path applies to them.
func TestRefreshVendorAccountModelsRefreshesAnExpiredAnthropicToken(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.ok(kindAnthropicSubscription, discovered("claude-opus-5", "Claude Opus 5"))
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorAnthropic, "")

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	if len(refresher.recorded()) != 1 {
		t.Fatalf("refresher calls = %v, want one", refresher.recorded())
	}
	if call := fake.onlyCall(t); call.kind != kindAnthropicSubscription || call.credential != refreshTestFreshAccess {
		t.Fatalf("call = %+v, want the Anthropic fetcher with the refreshed token", call)
	}
}

// A token that is still valid is never refreshed: the refresher (which spends a
// single-use refresh token) runs only for a token already past its expiry.
func TestRefreshVendorAccountModelsDoesNotRefreshAValidToken(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Fresh")
	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{
		AccessToken: refreshTestOldAccess, RefreshToken: refreshTestRefresh, ExpiresAt: discoveryTestNow.Add(time.Hour),
	})

	if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	if got := refresher.recorded(); len(got) != 0 {
		t.Fatalf("refresher calls = %v, want none for an unexpired token", got)
	}
	if call := fake.onlyCall(t); call.credential != refreshTestOldAccess {
		t.Fatalf("call = %+v, want the stored token used as is", call)
	}
}

// An expired token with no refresh token can never be renewed, so the refresher is
// not asked; the stored token is simply tried (it may still be accepted).
func TestRefreshVendorAccountModelsDoesNotRefreshWithoutARefreshToken(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "No refresh token")
	setDiscoveryTokens(t, svc, routeStore, acc.ID, vendorauth.TokenSet{
		AccessToken: refreshTestOldAccess, ExpiresAt: discoveryTestNow.Add(-time.Hour),
	})

	if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("result = %+v, err = %v, want ok", res, err)
	}
	if got := refresher.recorded(); len(got) != 0 {
		t.Fatalf("refresher calls = %v, want none without a refresh token", got)
	}
	if call := fake.onlyCall(t); call.credential != refreshTestOldAccess {
		t.Fatalf("call = %+v, want the stored token tried", call)
	}
}

// With NO refresher wired the behaviour is the fail-soft one: Unverifiable, no
// fetch, rows kept. The portal must NOT refresh by itself (the gateway's lock is
// what prevents two parties burning one rotating refresh token).
func TestRefreshVendorAccountModelsWithoutARefresherStaysUnverifiable(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	if svc.vendorDiscovery.tokenRefresher != nil {
		t.Fatal("the test service must have no refresher unless a test installs one")
	}
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v (fail-soft must not be an error)", err)
	}
	if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 || !strings.Contains(res.Detail, "expired") {
		t.Fatalf("result = %+v, want unverifiable explaining the expired token", res)
	}
	fake.requireNoCalls(t, "for an expired token with no refresher")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
	if len(dto.Models) != len(before) {
		t.Fatalf("dto models = %+v, want the kept rows", dto.Models)
	}
}

// A refresher that fails (the refresh token was rejected, the vendor was
// unreachable, ...) is fail-soft too: Unverifiable, nothing fetched, rows kept,
// and the failure text never reaches the result.
func TestRefreshVendorAccountModelsWhenTheRefresherFailsStaysUnverifiable(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	refresher.err = errors.New("refresh rejected for " + refreshTestRefresh)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v (a failing refresher is fail-soft)", err)
	}
	if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 || res.Detail == "" {
		t.Fatalf("result = %+v, want unverifiable with a detail", res)
	}
	if len(refresher.recorded()) != 1 {
		t.Fatalf("refresher calls = %v, want one", refresher.recorded())
	}
	fake.requireNoCalls(t, "after a failed refresh")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
	requireNoSubstring(t, res, refreshTestRefresh)
	requireNoSubstring(t, dto, refreshTestRefresh)
}

// A refresher that reports success but left the stored token expired (it could not
// persist the refreshed set) must not lead to a fetch with the dead token.
func TestRefreshVendorAccountModelsWhenTheTokenIsStillExpiredAfterTheRefresher(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	refresher.fresh = nil // "succeeds" and persists nothing
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v", err)
	}
	if res.Status != VendorRefreshUnverifiable || res.Detail == "" {
		t.Fatalf("result = %+v, want unverifiable", res)
	}
	fake.requireNoCalls(t, "with a token that is still expired")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
}

// The refresh leaves the stored token set unreadable: that is the credential
// error, not a verdict, and nothing is fetched.
func TestRefreshVendorAccountModelsWhenTheRefreshedTokensCannotBeOpened(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		_ = refresher.refresh(ctx, id)
		row, err := routeStore.VendorAccountByID(ctx, id)
		if err != nil {
			return err
		}
		row.OAuthTokens = "enc:!!!not-base64"
		return routeStore.UpdateVendorAccount(ctx, row)
	}

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountCredentialUnreadable) {
		t.Fatalf("err = %v, want ErrVendorAccountCredentialUnreadable", err)
	}
	fake.requireNoCalls(t, "with an unreadable token set")
}

// The account is deleted while the refresher runs: reported as not found.
func TestRefreshVendorAccountModelsWhenTheAccountVanishesDuringTheRefresh(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		return routeStore.DeleteVendorAccount(ctx, id)
	}

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("err = %v, want ErrVendorAccountNotFound", err)
	}
	fake.requireNoCalls(t, "for a deleted account")
}

// The gateway's refresher flips the account to needs_reconnect when the vendor
// rejects the refresh token. The 200 the portal answers must report THAT status
// (not the "active" the account had when the request loaded it), and say to
// reconnect instead of suggesting a retry that can only fail again.
func TestRefreshVendorAccountModelsReportsTheStatusARejectedRefreshSet(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	before := storedModels(t, routeStore, acc.ID)
	if acc.Status != routing.VendorAccountStatusActive {
		t.Fatalf("precondition: status = %q, want active", acc.Status)
	}
	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		// What resolveSubscriptionBearer does on a rejected refresh token.
		if err := routeStore.SetVendorAccountStatus(ctx, id, routing.VendorAccountStatusNeedsReconnect); err != nil {
			t.Errorf("SetVendorAccountStatus: %v", err)
		}
		return errors.New("refresh rejected")
	}

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("RefreshVendorAccountModels: %v (fail-soft must not be an error)", err)
	}
	if dto.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("dto status = %q, want needs_reconnect (the status the refresher just set, not the stale active)", dto.Status)
	}
	if res.Status != VendorRefreshUnverifiable || res.Discovered != 0 || !strings.Contains(res.Detail, "reconnect") || strings.Contains(res.Detail, "try again") {
		t.Fatalf("result = %+v, want unverifiable telling the user to reconnect, not to try again", res)
	}
	fake.requireNoCalls(t, "after a rejected refresh")
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, before) {
		t.Fatalf("rows changed to %+v", got)
	}
	if len(dto.Models) != len(before) {
		t.Fatalf("dto models = %+v, want the kept rows", dto.Models)
	}
}

// Every fail-soft answer is built from the account as it is NOW: a refresher that
// fails without touching the status leaves it active (and suggests a retry), and an
// account deleted during the refresh is reported as not found rather than rendered
// from the stale copy.
func TestRefreshVendorAccountModelsFailSoftAnswerIsBuiltFromTheCurrentAccount(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	refresher := installTokenRefresher(t, svc, routeStore)
	refresher.err = errors.New("vendor unreachable")
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	dto, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil || dto.Status != routing.VendorAccountStatusActive || !strings.Contains(res.Detail, "try again") {
		t.Fatalf("dto = %+v, result = %+v, err = %v, want active with a retry hint", dto, res, err)
	}

	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		_ = routeStore.DeleteVendorAccount(ctx, id)
		return errors.New("vendor unreachable")
	}
	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("deleted during the refresh: err = %v, want ErrVendorAccountNotFound", err)
	}
}

// A refused principal never reaches the refresher either (it would otherwise let a
// stranger spend the owner's refresh token).
func TestRefreshVendorAccountModelsRefusedPrincipalNeverRefreshesTheToken(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), otherToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("err = %v, want ErrVendorAccountNotFound", err)
	}
	if _, _, err := svc.RefreshVendorAccountModels(context.Background(), systemToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("system scope: err = %v, want ErrVendorAccountNotFound", err)
	}
	if got := refresher.recorded(); len(got) != 0 {
		t.Fatalf("refresher calls = %v, want none for a refused principal", got)
	}
}

// Connecting with a pasted token that is already expired (and a refresh token that
// can renew it) used to leave the static seed; with the refresher the connect-time
// discovery refreshes first and serves the real models at once.
func TestConnectVendorAccountImportWithAnExpiredTokenDiscoversThroughTheRefresher(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.ok(kindOpenAISubscription, discovered("gpt-6-luna", "GPT-6 Luna"))
	refresher := installTokenRefresher(t, svc, routeStore)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{
		AccessToken: refreshTestOldAccess, RefreshToken: refreshTestRefresh, ExpiresAt: svc.clock().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if !dto.SubscriptionConnected || len(dto.Models) != 1 || dto.Models[0].GatewayModel != "gpt-6-luna" {
		t.Fatalf("dto = %+v, want a connected account serving the discovered model", dto)
	}
	if len(refresher.recorded()) != 1 {
		t.Fatalf("refresher calls = %v, want one", refresher.recorded())
	}
	if call := fake.onlyCall(t); call.credential != refreshTestFreshAccess {
		t.Fatalf("call = %+v, want the refreshed token", call)
	}
}

func TestNewServiceInjectsAndReplacesTheVendorTokenRefresher(t *testing.T) {
	if NewService(ServiceDeps{}).vendorDiscovery.tokenRefresher != nil {
		t.Fatal("default refresher is set, want nil (nil-safe: no refresher means fail-soft)")
	}
	var called string
	svc := NewService(ServiceDeps{VendorTokenRefresher: func(_ context.Context, id string) error {
		called = id
		return nil
	}})
	if svc.vendorDiscovery.tokenRefresher == nil {
		t.Fatal("the injected refresher was dropped")
	}
	_ = svc.vendorDiscovery.tokenRefresher(context.Background(), "va_1")
	if called != "va_1" {
		t.Fatalf("injected refresher saw %q, want va_1", called)
	}

	// The construction-order setter (the gateway Server exists only after the
	// Service) replaces it, and nil clears it.
	var replaced string
	svc.SetVendorTokenRefresher(func(_ context.Context, id string) error {
		replaced = id
		return nil
	})
	_ = svc.vendorDiscovery.tokenRefresher(context.Background(), "va_2")
	if replaced != "va_2" {
		t.Fatalf("replacement refresher saw %q, want va_2", replaced)
	}
	svc.SetVendorTokenRefresher(nil)
	if svc.vendorDiscovery.tokenRefresher != nil {
		t.Fatal("SetVendorTokenRefresher(nil) must clear the refresher")
	}
}

// --- the refresh is never cancelled mid-flight -----------------------------------------

// A refresh token is single-use: if the vendor rotated it and the call or the
// persist is then cut off, only the dead old token is left and the account breaks
// on its next use. So the refresher runs on a context that neither a client
// disconnect nor the connect-time bound can cancel, under a bound of its own, and
// the discovery stops WAITING for it when its own context ends instead.
func TestRefreshVendorAccountModelsNeverCancelsATokenRefreshInFlight(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	fresh := vendorauth.TokenSet{AccessToken: refreshTestFreshAccess, RefreshToken: refreshTestRefresh, ExpiresAt: discoveryTestNow.Add(time.Hour), AccountID: discoveryTestAccount}

	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var ctxErrAtPersist error
	var hasOwnBound bool
	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		defer close(finished)
		_, hasOwnBound = ctx.Deadline()
		close(entered)
		<-release
		// The vendor has rotated the token and the persist is about to run.
		ctxErrAtPersist = ctx.Err()
		setDiscoveryTokens(t, svc, routeStore, id, fresh)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		res RefreshResult
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		_, res, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID)
		got <- outcome{res, err}
	}()
	waitClosed(t, entered, "the refresher to start")
	cancel() // the client goes away mid-refresh

	// The discovery gives up waiting (fail-soft, nothing fetched) ...
	select {
	case o := <-got:
		if o.err != nil && !errors.Is(o.err, context.Canceled) {
			t.Fatalf("err = %v, want fail-soft or the context's own error", o.err)
		}
		if o.err == nil && o.res.Status != VendorRefreshUnverifiable {
			t.Fatalf("result = %+v, want unverifiable", o.res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the discovery kept waiting on a refresh after its own context ended")
	}
	fake.requireNoCalls(t, "after the discovery stopped waiting")

	// ... but the refresh itself was NOT cancelled and still persists the rotation.
	close(release)
	waitClosed(t, finished, "the refresher to finish")
	if ctxErrAtPersist != nil {
		t.Fatalf("the refresher's context was cancelled mid-flight: %v (a rotated single-use token would be lost)", ctxErrAtPersist)
	}
	if !hasOwnBound {
		t.Fatal("the refresher ran without a bound of its own")
	}
	if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); ts.AccessToken != refreshTestFreshAccess {
		t.Fatalf("stored access token = %q, want the rotated one persisted", ts.AccessToken)
	}
}

// Same at connect time: the connect-time bound ends the discovery's wait, never the
// refresh, and the connect still succeeds with the seeded models.
func TestConnectVendorAccountImportBoundDoesNotCancelATokenRefresh(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	svc.vendorDiscovery.connectTimeout = 100 * time.Millisecond
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	seed := storedModels(t, routeStore, acc.ID)
	fresh := vendorauth.TokenSet{AccessToken: refreshTestFreshAccess, RefreshToken: refreshTestRefresh, ExpiresAt: svc.clock().Add(time.Hour)}

	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var ctxErrAtPersist error
	svc.vendorDiscovery.tokenRefresher = func(ctx context.Context, id string) error {
		defer close(finished)
		close(entered)
		<-release
		ctxErrAtPersist = ctx.Err()
		setDiscoveryTokens(t, svc, routeStore, id, fresh)
		return nil
	}

	type outcome struct {
		dto VendorAccountDTO
		err error
	}
	got := make(chan outcome, 1)
	go func() {
		dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{
			AccessToken: refreshTestOldAccess, RefreshToken: refreshTestRefresh, ExpiresAt: svc.clock().Add(-time.Hour),
		})
		got <- outcome{dto, err}
	}()
	waitClosed(t, entered, "the refresher to start")
	select {
	case o := <-got:
		if o.err != nil || !o.dto.SubscriptionConnected || len(o.dto.Models) != len(seed) {
			t.Fatalf("connect = %+v, %v, want a connected account with the seeded models", o.dto, o.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the connect waited on a slow refresh past its discovery bound")
	}

	close(release)
	waitClosed(t, finished, "the refresher to finish")
	if ctxErrAtPersist != nil {
		t.Fatalf("the connect-time bound cancelled the refresh in flight: %v", ctxErrAtPersist)
	}
	if _, ts := storedTokenSet(t, routeStore, svc, acc.ID); ts.AccessToken != refreshTestFreshAccess {
		t.Fatalf("stored access token = %q, want the rotated one persisted", ts.AccessToken)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, seed) {
		t.Fatalf("stored rows = %+v, want the seed kept", got)
	}
}

// A refresher that panics must not take the process down (it runs off the request
// goroutine); the discovery treats it as a failed refresh.
func TestRefreshVendorAccountModelsSurvivesAPanickingRefresher(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := expiredSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	svc.vendorDiscovery.tokenRefresher = func(context.Context, string) error { panic("boom") }

	_, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
	if err != nil || res.Status != VendorRefreshUnverifiable {
		t.Fatalf("result = %+v, err = %v, want a fail-soft unverifiable", res, err)
	}
	fake.requireNoCalls(t, "after a failed refresh")
}

// --- connect-time discovery bound ------------------------------------------------------

func TestNewServiceBoundsConnectTimeDiscoveryToFiveSeconds(t *testing.T) {
	if got := NewService(ServiceDeps{}).vendorDiscovery.connectTimeout; got != 5*time.Second {
		t.Fatalf("connect-time discovery timeout = %v, want 5s", got)
	}
}

// A vendor that hangs adds at most the connect-time bound to a connect: the fetch
// is cancelled when the bound passes and the connect still succeeds with the seeded
// models.
func TestConnectVendorAccountImportBoundsAHangingDiscovery(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	svc.vendorDiscovery.connectTimeout = 100 * time.Millisecond
	var sawDeadline bool
	svc.vendorDiscovery.discoverers.OpenAISubscription = func(ctx context.Context, _ *http.Client, _, _, _ string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		_, sawDeadline = ctx.Deadline()
		select {
		case <-ctx.Done():
		case <-time.After(30 * time.Second):
			t.Error("the hanging fetch was never cancelled")
		}
		return nil, vendorauth.DiscoveryUnverifiable
	}
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	seed := storedModels(t, routeStore, acc.ID)

	start := time.Now()
	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("connect took %v, want it bounded by the connect-time discovery timeout", elapsed)
	}
	if !sawDeadline {
		t.Fatal("the discovery fetch ran without a deadline")
	}
	if !dto.SubscriptionConnected || len(dto.Models) != len(seed) {
		t.Fatalf("dto = %+v, want a connected account with the seeded models", dto)
	}
	if got := storedModels(t, routeStore, acc.ID); !reflect.DeepEqual(got, seed) {
		t.Fatalf("stored rows = %+v, want the seed kept", got)
	}
}

// The explicit refresh is not given the short connect-time bound: only the connect
// flows are.
func TestRefreshVendorAccountModelsHasNoConnectTimeBound(t *testing.T) {
	svc, routeStore, _ := newDiscoveryTestService(t)
	var hasDeadline bool
	svc.vendorDiscovery.discoverers.OpenAISubscription = func(ctx context.Context, _ *http.Client, _, _, _ string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		_, hasDeadline = ctx.Deadline()
		return []vendorauth.DiscoveredModel{discovered("gpt-6-luna", "x")}, vendorauth.DiscoveryOK
	}
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")

	if _, res, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID); err != nil || res.Status != VendorRefreshOK {
		t.Fatalf("result = %+v, err = %v", res, err)
	}
	if hasDeadline {
		t.Fatal("the explicit refresh was given a deadline; only connect-time discovery is bounded")
	}
}

// When the connect-time discovery ends in an error the connect still answers with
// the account, and it is the account as it is NOW (here: a status flipped meanwhile),
// not the copy taken before the discovery ran.
func TestConnectDiscoveryFailureAnswersWithTheCurrentAccount(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	svc.vendorDiscovery.discoverers.OpenAISubscription = func(context.Context, *http.Client, string, string, string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus) {
		// The status changes while the vendor call is in flight, then the write fails.
		if err := routeStore.SetVendorAccountStatus(context.Background(), acc.ID, routing.VendorAccountStatusNeedsReconnect); err != nil {
			t.Errorf("SetVendorAccountStatus: %v", err)
		}
		svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("db down")}
		return []vendorauth.DiscoveredModel{discovered("gpt-6-luna", "x")}, vendorauth.DiscoveryOK
	}

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v (a failing discovery must not fail the connect)", err)
	}
	if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("dto = %+v, want the connected account with its CURRENT status needs_reconnect", dto)
	}
}

// --- log hygiene -------------------------------------------------------------------------

// corruptAfterConnectStore makes the account's token blob unreadable once the
// connect has stored its tokens, so the connect-time discovery fails with
// ErrVendorAccountCredentialUnreadable. The blob (and so the cause chain of the
// open error) is built from the tokens themselves.
type corruptAfterConnectStore struct {
	routing.Store
	mu    sync.Mutex
	armed bool
	blob  string
}

func (c *corruptAfterConnectStore) UpdateVendorAccount(ctx context.Context, acc routing.VendorAccount) error {
	err := c.Store.UpdateVendorAccount(ctx, acc)
	if err == nil {
		c.mu.Lock()
		c.armed = true
		c.mu.Unlock()
	}
	return err
}

func (c *corruptAfterConnectStore) VendorAccountByID(ctx context.Context, id string) (routing.VendorAccount, error) {
	acc, err := c.Store.VendorAccountByID(ctx, id)
	c.mu.Lock()
	armed := c.armed
	c.mu.Unlock()
	if err == nil && armed {
		acc.OAuthTokens = c.blob
	}
	return acc, err
}

// A connect whose discovery fails on an unreadable credential logs ONE fixed line:
// no token, and not the cause chain either (a decode error of the stored blob can
// quote part of what it could not decode).
func TestConnectDiscoveryFailureOnAnUnreadableCredentialLogsAFixedLine(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	svc.routes = &corruptAfterConnectStore{Store: routeStore, blob: "plain:" + connectTestAccess + connectTestRefresh}
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	var dto VendorAccountDTO
	logs := captureSlog(t, func() {
		var err error
		dto, err = svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
		if err != nil {
			t.Fatalf("ConnectVendorAccountImport: %v (discovery must not fail the connect)", err)
		}
	})
	if !dto.SubscriptionConnected {
		t.Fatalf("dto = %+v, want a connected account", dto)
	}
	if logs == "" {
		t.Fatal("nothing was logged for the failed discovery")
	}
	for _, leak := range []string{connectTestAccess, connectTestRefresh, "plain:", "vendorauth", "open vendor account", "decode token set", "invalid character"} {
		if strings.Contains(logs, leak) {
			t.Fatalf("logs contain %q (a credential or the cause chain): %s", leak, logs)
		}
	}
	if !strings.Contains(logs, "stored credential could not be read") || !strings.Contains(logs, acc.ID) {
		t.Fatalf("logs = %s, want the fixed unreadable-credential line naming the account", logs)
	}
	fake.requireNoCalls(t, "with an unreadable credential")
}

// A discovery that fails some other way still logs its error (that is how an
// operator finds a store outage), and that line carries no token either.
func TestConnectDiscoveryStoreFailureStillLogsItsCauseWithoutATokenAnywhere(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	fake := installFakeVendorDiscoverers(svc)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	svc.routes = failSetModelsStore{Store: routeStore, err: errors.New("db down")}

	logs := captureSlog(t, func() {
		if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh}); err != nil {
			t.Fatalf("ConnectVendorAccountImport: %v", err)
		}
	})
	if !strings.Contains(logs, "db down") {
		t.Fatalf("logs = %s, want the store failure logged", logs)
	}
	for _, leak := range []string{connectTestAccess, connectTestRefresh} {
		if strings.Contains(logs, leak) {
			t.Fatalf("logs contain %q: %s", leak, logs)
		}
	}
}

// --- per-account write single-flight ----------------------------------------------------

// blockingSetModelsStore blocks the FIRST SetVendorAccountModels of blockID until
// release is closed, signalling entered when it is blocked, so a test can hold one
// writer inside its critical section while another one tries to enter.
type blockingSetModelsStore struct {
	routing.Store
	blockID string
	entered chan struct{}
	release chan struct{}

	mu      sync.Mutex
	blocked bool
}

func newBlockingSetModelsStore(store routing.Store, blockID string) *blockingSetModelsStore {
	return &blockingSetModelsStore{Store: store, blockID: blockID, entered: make(chan struct{}), release: make(chan struct{})}
}

func (b *blockingSetModelsStore) SetVendorAccountModels(ctx context.Context, id string, rows []routing.VendorAccountModel) error {
	b.mu.Lock()
	block := id == b.blockID && !b.blocked
	if block {
		b.blocked = true
	}
	b.mu.Unlock()
	if block {
		close(b.entered)
		<-b.release
	}
	return b.Store.SetVendorAccountModels(ctx, id, rows)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitForPrefix(t *testing.T, routeStore *routing.MemoryStore, id, prefix string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if row, err := routeStore.VendorAccountByID(context.Background(), id); err == nil && row.ModelPrefix == prefix {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for the stored prefix %q", prefix)
}

// requireStillRunning fails if done fires within a short window: the operation is
// expected to be waiting on the per-account lock.
func requireStillRunning(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s finished (err = %v) while the other writer held the account's lock", what, err)
	case <-time.After(150 * time.Millisecond):
	}
}

// A refresh that is writing and a prefix change that arrives meanwhile cannot lose
// each other's update: the relabel waits for the refresh's write and then
// re-labels the rows it wrote, so the rows end up under the NEW prefix.
func TestRefreshThenRelabelOfOneAccountAreSingleFlight(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "old/")
	gate := newBlockingSetModelsStore(routeStore, acc.ID)
	svc.routes = gate
	ctx := context.Background()

	refreshDone := make(chan error, 1)
	go func() {
		_, _, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID)
		refreshDone <- err
	}()
	waitClosed(t, gate.entered, "the refresh to reach its write")

	updateDone := make(chan error, 1)
	go func() {
		_, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("new/")})
		updateDone <- err
	}()
	waitForPrefix(t, routeStore, acc.ID, "new/")
	requireStillRunning(t, updateDone, "the re-label")

	close(gate.release)
	for name, done := range map[string]chan error{"refresh": refreshDone, "update": updateDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never finished", name)
		}
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"new/gpt-6-luna"}) {
		t.Fatalf("gateway models = %v, want the discovered model under the new prefix", got)
	}
}

// The other order: a re-label that is writing and a refresh that arrives meanwhile.
// Without the lock the re-label's (older) rows would overwrite the refresh's.
func TestRelabelThenRefreshOfOneAccountAreSingleFlight(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "old/")
	gate := newBlockingSetModelsStore(routeStore, acc.ID)
	svc.routes = gate
	ctx := context.Background()

	updateDone := make(chan error, 1)
	go func() {
		_, err := svc.UpdateVendorAccount(ctx, ownerToken(), acc.ID, UpdateVendorAccountRequest{ModelPrefix: strPtr("new/")})
		updateDone <- err
	}()
	waitClosed(t, gate.entered, "the re-label to reach its write")

	refreshDone := make(chan error, 1)
	go func() {
		_, _, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID)
		refreshDone <- err
	}()
	requireStillRunning(t, refreshDone, "the refresh")

	close(gate.release)
	for name, done := range map[string]chan error{"refresh": refreshDone, "update": updateDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s never finished", name)
		}
	}
	if got := gatewayModels(storedModels(t, routeStore, acc.ID)); !reflect.DeepEqual(got, []string{"new/gpt-6-luna"}) {
		t.Fatalf("gateway models = %v, want the discovered model (not the re-labelled seed) under the new prefix", got)
	}
}

// The lock is per ACCOUNT: a write held on one account does not make another
// account's refresh wait.
func TestModelWriteLockIsPerAccount(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	held := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	other := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	gate := newBlockingSetModelsStore(routeStore, held.ID)
	svc.routes = gate
	ctx := context.Background()

	heldDone := make(chan error, 1)
	go func() {
		_, _, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), held.ID)
		heldDone <- err
	}()
	waitClosed(t, gate.entered, "the first refresh to reach its write")

	otherDone := make(chan error, 1)
	go func() {
		_, _, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), other.ID)
		otherDone <- err
	}()
	select {
	case err := <-otherDone:
		if err != nil {
			t.Fatalf("the other account's refresh: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the other account's refresh waited on an account it does not share")
	}
	close(gate.release)
	if err := <-heldDone; err != nil {
		t.Fatalf("the held refresh: %v", err)
	}
}

// A writer that is waiting on the lock gives up when its context ends, instead of
// waiting on a stuck writer for ever.
func TestModelWriteLockWaitHonoursTheContext(t *testing.T) {
	svc, routeStore, fake := newDiscoveryTestService(t)
	fake.okSlugs(kindOpenAISubscription, "gpt-6-luna")
	acc := connectedSubscription(t, svc, routeStore, routing.VendorOpenAI, "")
	gate := newBlockingSetModelsStore(routeStore, acc.ID)
	svc.routes = gate

	holder := make(chan error, 1)
	go func() {
		_, _, err := svc.RefreshVendorAccountModels(context.Background(), ownerToken(), acc.ID)
		holder <- err
	}()
	waitClosed(t, gate.entered, "the first refresh to reach its write")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := svc.RefreshVendorAccountModels(ctx, ownerToken(), acc.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's deadline error while the lock is held", err)
	}
	close(gate.release)
	if err := <-holder; err != nil {
		t.Fatalf("the holder: %v", err)
	}
}

func TestAccountLocksSerializeOneKeyAndForgetIdleKeys(t *testing.T) {
	var locks accountLocks
	ctx := context.Background()

	unlockA, err := locks.lock(ctx, "a")
	if err != nil {
		t.Fatalf("lock a: %v", err)
	}
	// Another key is independent.
	unlockB, err := locks.lock(ctx, "b")
	if err != nil {
		t.Fatalf("lock b: %v", err)
	}
	unlockB()

	acquired := make(chan struct{})
	go func() {
		unlock, err := locks.lock(ctx, "a")
		if err != nil {
			t.Errorf("second lock a: %v", err)
			return
		}
		close(acquired)
		unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("a second holder entered while the first still held the key")
	case <-time.After(100 * time.Millisecond):
	}
	unlockA()
	waitClosed(t, acquired, "the second holder to enter after the unlock")

	// Idle keys are forgotten, so the map does not grow with every account ever
	// written.
	deadline := time.Now().Add(5 * time.Second)
	for {
		locks.mu.Lock()
		n := len(locks.entries)
		locks.mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock entries = %d after every holder released, want 0", n)
		}
		time.Sleep(time.Millisecond)
	}

	// A cancelled waiter does not leak its reference: the key is forgotten once
	// the holder lets go.
	unlock, err := locks.lock(ctx, "c")
	if err != nil {
		t.Fatalf("lock c: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := locks.lock(cancelled, "c"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: err = %v, want context.Canceled", err)
	}
	unlock()
	locks.mu.Lock()
	n := len(locks.entries)
	locks.mu.Unlock()
	if n != 0 {
		t.Fatalf("lock entries = %d after a cancelled waiter and the release, want 0", n)
	}
}

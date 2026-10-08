// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	refresherStaleAccess   = "stale-access-DO-NOT-ECHO-1a"
	refresherStaleRefresh  = "stale-refresh-DO-NOT-ECHO-2b"
	refresherFreshAccess   = "fresh-access-DO-NOT-ECHO-3c"
	refresherFreshRefresh  = "fresh-refresh-DO-NOT-ECHO-4d"
	refresherOpenAIAccount = "acct-refresher-7"
)

// openAITokenEndpoint is a form-encoded OpenAI token endpoint that rotates the
// token set on every call and counts the calls and the refresh tokens it saw.
type openAITokenEndpoint struct {
	srv   *httptest.Server
	calls int32

	mu   sync.Mutex
	sent []string
}

func newOpenAITokenEndpoint(t *testing.T, status int, body string) *openAITokenEndpoint {
	t.Helper()
	e := &openAITokenEndpoint{}
	e.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&e.calls, 1)
		_ = r.ParseForm()
		e.mu.Lock()
		e.sent = append(e.sent, r.PostForm.Get("refresh_token"))
		e.mu.Unlock()
		// A small delay widens the window two unserialized refreshers would race in.
		time.Sleep(20 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(e.srv.Close)
	return e
}

func (e *openAITokenEndpoint) endpoints() vendorauth.Endpoints {
	ep := vendorauth.DefaultOpenAIEndpoints()
	ep.TokenURL = e.srv.URL
	return ep
}

func (e *openAITokenEndpoint) callCount() int { return int(atomic.LoadInt32(&e.calls)) }

func freshTokenBody() string {
	return `{"access_token":"` + refresherFreshAccess + `","refresh_token":"` + refresherFreshRefresh + `","expires_in":3600}`
}

func expiredOpenAITokens() vendorauth.TokenSet {
	return vendorauth.TokenSet{
		AccessToken: refresherStaleAccess, RefreshToken: refresherStaleRefresh,
		ExpiresAt: time.Now().Add(-time.Hour), AccountID: refresherOpenAIAccount, PlanType: "plus",
	}
}

func storedTokens(t *testing.T, store *routing.MemoryStore, cipher *capture.Cipher, id string) (routing.VendorAccount, vendorauth.TokenSet) {
	t.Helper()
	acc, err := store.VendorAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	ts, err := vendorauth.OpenTokenSet(cipher, acc.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	return acc, ts
}

// RefreshVendorSubscriptionTokens is the portal's handle on the gateway's LOCKED
// refresh: an expired token set is refreshed against the account's own vendor
// endpoint, resealed and persisted, and the error is nil.
func TestRefreshVendorSubscriptionTokensRefreshesAndPersistsAnExpiredTokenSet(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_oai", routing.VendorOpenAI, expiredOpenAITokens())
	oauth := newOpenAITokenEndpoint(t, http.StatusOK, freshTokenBody())
	s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: oauth.endpoints()}

	if err := s.RefreshVendorSubscriptionTokens(context.Background(), "acc_oai"); err != nil {
		t.Fatalf("RefreshVendorSubscriptionTokens: %v", err)
	}
	if got := oauth.callCount(); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}
	acc, ts := storedTokens(t, store, cipher, "acc_oai")
	if ts.AccessToken != refresherFreshAccess || ts.RefreshToken != refresherFreshRefresh || ts.AccountID != refresherOpenAIAccount {
		t.Fatal("the persisted token set is not the refreshed one (with the account id carried forward)")
	}
	if acc.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status = %q, want active", acc.Status)
	}
}

// A token that is not near expiry is left alone: the refresher costs nothing and
// spends no refresh token.
func TestRefreshVendorSubscriptionTokensLeavesAValidTokenSetAlone(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	valid := expiredOpenAITokens()
	valid.ExpiresAt = time.Now().Add(time.Hour)
	original := seedSubscriptionAccount(t, store, cipher, "acc_oai", routing.VendorOpenAI, valid)
	oauth := newOpenAITokenEndpoint(t, http.StatusOK, freshTokenBody())
	s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: oauth.endpoints()}

	if err := s.RefreshVendorSubscriptionTokens(context.Background(), "acc_oai"); err != nil {
		t.Fatalf("RefreshVendorSubscriptionTokens: %v", err)
	}
	if oauth.callCount() != 0 {
		t.Fatalf("token endpoint called %d times, want none for a valid token", oauth.callCount())
	}
	if acc, _ := store.VendorAccountByID(context.Background(), "acc_oai"); acc.OAuthTokens != original {
		t.Fatal("a valid token set was rewritten")
	}
}

// Every way the refresh cannot deliver a usable token is an error, and the error
// names no credential. A rejected refresh token also flips the account to
// needs_reconnect, exactly as a request would.
func TestRefreshVendorSubscriptionTokensReportsEveryFailureWithoutACredential(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		body          string
		wantReconnect bool
	}{
		{"refresh token rejected", http.StatusUnauthorized, `{"error":"invalid_grant"}`, true},
		{"token endpoint down", http.StatusInternalServerError, `{"error":"server_error"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cipher := newDispatchCipher(t)
			store := routing.NewMemoryStore()
			original := seedSubscriptionAccount(t, store, cipher, "acc_oai", routing.VendorOpenAI, expiredOpenAITokens())
			oauth := newOpenAITokenEndpoint(t, tc.status, tc.body)
			s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: oauth.endpoints()}

			err := s.RefreshVendorSubscriptionTokens(context.Background(), "acc_oai")
			if err == nil {
				t.Fatal("RefreshVendorSubscriptionTokens = nil, want an error when no usable token was had")
			}
			for _, secret := range []string{refresherStaleAccess, refresherStaleRefresh, refresherFreshAccess, refresherFreshRefresh} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks a token: %v", err)
				}
			}
			acc, _ := store.VendorAccountByID(context.Background(), "acc_oai")
			if acc.OAuthTokens != original {
				t.Fatal("a failed refresh rewrote the stored token set")
			}
			if got := acc.Status == routing.VendorAccountStatusNeedsReconnect; got != tc.wantReconnect {
				t.Fatalf("status = %q, needs_reconnect = %v, want %v", acc.Status, got, tc.wantReconnect)
			}
		})
	}
}

func TestRefreshVendorSubscriptionTokensForAnUnknownAccountOrWithoutAStoreIsAnError(t *testing.T) {
	cipher := newDispatchCipher(t)
	s := &Server{Cipher: cipher, Routes: routing.NewMemoryStore()}
	if err := s.RefreshVendorSubscriptionTokens(context.Background(), "va_missing"); err == nil {
		t.Fatal("an unknown account must be an error")
	}
	if err := (&Server{}).RefreshVendorSubscriptionTokens(context.Background(), "va_x"); err == nil {
		t.Fatal("a Server without a routing store must be an error")
	}
}

// The portal's refresh and the dispatch share ONE lock per account: with a stale
// token, a burst of portal refreshes and dispatch bearer lookups hits the token
// endpoint exactly once, so the single-use refresh token is spent once. (This is
// why the portal must go through the gateway's refresher instead of refreshing by
// itself.) Run under -race it also pins the lock discipline.
func TestRefreshVendorSubscriptionTokensSharesTheDispatchRefreshLock(t *testing.T) {
	cipher := newDispatchCipher(t)
	store := routing.NewMemoryStore()
	seedSubscriptionAccount(t, store, cipher, "acc_oai", routing.VendorOpenAI, expiredOpenAITokens())
	oauth := newOpenAITokenEndpoint(t, http.StatusOK, freshTokenBody())
	s := &Server{Cipher: cipher, Routes: store, vendorOpenAIEndpoints: oauth.endpoints()}

	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	bearers := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- s.RefreshVendorSubscriptionTokens(context.Background(), "acc_oai")
		}()
		go func() {
			defer wg.Done()
			access, _, _ := s.resolveSubscriptionBearer(context.Background(), "acc_oai")
			bearers <- access
		}()
	}
	wg.Wait()
	close(errs)
	close(bearers)
	for err := range errs {
		if err != nil {
			t.Fatalf("RefreshVendorSubscriptionTokens: %v", err)
		}
	}
	for access := range bearers {
		if access != refresherFreshAccess {
			t.Fatalf("dispatch bearer = %q, want the refreshed token", access)
		}
	}
	if got := oauth.callCount(); got != 1 {
		t.Fatalf("token endpoint called %d times, want exactly 1 (shared per-account lock)", got)
	}
}

// THE end-to-end fix, wired the way cmd/gateway wires it: the portal Service takes
// the Server's refresher through its setter (the Server exists only after the
// Service), and "refresh models" on an expired-but-refreshable subscription then
// refreshes the token under the gateway's lock, discovers with the FRESH token and
// stores the real models. Without the refresher the same request is a fail-soft
// "unverifiable" that keeps the seed.
func TestModelsRefreshEndpointRefreshesAnExpiredSubscriptionTokenThroughTheGatewayRefresher(t *testing.T) {
	cipher := newDispatchCipher(t)
	oauth := newOpenAITokenEndpoint(t, http.StatusOK, freshTokenBody())
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{
		{Slug: "gpt-6-luna", DisplayName: "GPT-6 Luna"}, {Slug: "gpt-6.1-sol", DisplayName: "GPT-6.1 Sol"},
	}}
	srv, store, _ := newVendorAccountSettingsTestServerWithDeps(t, false, func(deps *portal.ServiceDeps) {
		deps.VendorDiscoverers = disc.discoverers()
		deps.Cipher = cipher
	})
	enableVendorAccountsFlag(t, srv)
	srv.Cipher = cipher
	srv.vendorOpenAIEndpoints = oauth.endpoints()

	created := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", vaOwnerSecret,
		`{"vendor":"openai","auth_type":"subscription","name":"ChatGPT Plus","model_prefix":"chatgpt/"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d, body = %s", created.Code, created.Body.String())
	}
	acc := vaDecode(t, created)
	sealed, err := vendorauth.SealTokenSet(cipher, false, expiredOpenAITokens())
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	row, err := store.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	row.OAuthTokens = sealed
	if err := store.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}
	seed := acc.Models

	// No refresher wired: fail-soft, nothing fetched, the seed kept.
	rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh without a refresher = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := vaDecodeRefresh(t, rec.Body.Bytes())
	if body.Refresh.Status != portal.VendorRefreshUnverifiable || !reflect.DeepEqual(body.Account.Models, seed) {
		t.Fatalf("without a refresher: refresh = %+v, models = %+v, want unverifiable with the seed kept", body.Refresh, body.Account.Models)
	}
	if len(disc.credentials()) != 0 || oauth.callCount() != 0 {
		t.Fatalf("without a refresher: discovery saw %v and the token endpoint was called %d times, want neither", disc.credentials(), oauth.callCount())
	}

	// Wired exactly as cmd/gateway does it.
	srv.Portal.(*portal.Service).SetVendorTokenRefresher(srv.RefreshVendorSubscriptionTokens)
	rec = vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, body = %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{refresherStaleAccess, refresherStaleRefresh, refresherFreshAccess, refresherFreshRefresh} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("refresh response leaks a token: %s", rec.Body.String())
		}
	}
	body = vaDecodeRefresh(t, rec.Body.Bytes())
	if body.Refresh.Status != portal.VendorRefreshOK || body.Refresh.Discovered != 2 {
		t.Fatalf("refresh = %+v, want ok / 2", body.Refresh)
	}
	wantModels := []string{"chatgpt/gpt-6-luna", "chatgpt/gpt-6.1-sol"}
	var gotModels []string
	for _, m := range body.Account.Models {
		gotModels = append(gotModels, m.GatewayModel)
	}
	if !reflect.DeepEqual(gotModels, wantModels) {
		t.Fatalf("account models = %v, want %v", gotModels, wantModels)
	}
	if got := disc.credentials(); !reflect.DeepEqual(got, []string{refresherFreshAccess}) {
		t.Fatalf("discovery saw %v, want exactly the REFRESHED access token", got)
	}
	if got := oauth.callCount(); got != 1 {
		t.Fatalf("token endpoint called %d times, want 1", got)
	}
	// The refreshed token set was persisted by the gateway's refresher.
	if _, ts := storedTokens(t, store, cipher, acc.ID); ts.AccessToken != refresherFreshAccess || ts.RefreshToken != refresherFreshRefresh {
		t.Fatal("the refreshed token set was not persisted")
	}
}

// The vendor rejecting the refresh token flips the account to needs_reconnect (in
// the gateway's refresher); the 200 answer of "refresh models" reports that status
// and says to reconnect, instead of the stale "active" the account had when the
// request loaded it. Nothing is fetched and the seeded models are kept.
func TestModelsRefreshEndpointReportsNeedsReconnectAfterARejectedRefreshToken(t *testing.T) {
	cipher := newDispatchCipher(t)
	oauth := newOpenAITokenEndpoint(t, http.StatusUnauthorized, `{"error":"invalid_grant"}`)
	disc := &vaDiscovery{status: vendorauth.DiscoveryOK, models: []vendorauth.DiscoveredModel{{Slug: "gpt-6-luna", DisplayName: "x"}}}
	srv, store, _ := newVendorAccountSettingsTestServerWithDeps(t, false, func(deps *portal.ServiceDeps) {
		deps.VendorDiscoverers = disc.discoverers()
		deps.Cipher = cipher
	})
	enableVendorAccountsFlag(t, srv)
	srv.Cipher = cipher
	srv.vendorOpenAIEndpoints = oauth.endpoints()
	srv.Portal.(*portal.Service).SetVendorTokenRefresher(srv.RefreshVendorSubscriptionTokens)

	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")
	sealed, err := vendorauth.SealTokenSet(cipher, false, expiredOpenAITokens())
	if err != nil {
		t.Fatalf("SealTokenSet: %v", err)
	}
	row, err := store.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	row.OAuthTokens = sealed
	if err := store.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}

	rec := vaDo(t, srv, http.MethodPost, vaRefreshPath(acc.ID), vaOwnerSecret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := vaDecodeRefresh(t, rec.Body.Bytes())
	if body.Account.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("account status = %q, want needs_reconnect (the refresh token was rejected)", body.Account.Status)
	}
	if body.Refresh.Status != portal.VendorRefreshUnverifiable || !strings.Contains(body.Refresh.Detail, "reconnect") {
		t.Fatalf("refresh = %+v, want unverifiable telling the user to reconnect", body.Refresh)
	}
	if got := vaDecode(t, vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, "")); got.Status != routing.VendorAccountStatusNeedsReconnect {
		t.Fatalf("stored status = %q, want needs_reconnect", got.Status)
	}
	if len(disc.credentials()) != 0 {
		t.Fatalf("discovery saw %v, want no fetch with a token that could not be refreshed", disc.credentials())
	}
	for _, secret := range []string{refresherStaleAccess, refresherStaleRefresh} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("refresh response leaks a token: %s", rec.Body.String())
		}
	}
}

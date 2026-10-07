// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	connectTestAccess  = "sk-ant-oat01-access-do-not-echo"
	connectTestRefresh = "sk-ant-ort01-refresh-do-not-echo"
)

// vendorTokenStub is an httptest vendor token endpoint. It records every request
// body (JSON for Anthropic, form-encoded for OpenAI, both flattened to
// name -> value) and answers with the configured status and body.
type vendorTokenStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	calls  []map[string]string
	// hook, when set, runs inside the request handler before the answer is sent
	// (used to act "while the vendor call is in flight").
	hook func(r *http.Request)
}

func newVendorTokenStub(t *testing.T) *vendorTokenStub {
	t.Helper()
	stub := &vendorTokenStub{
		status: http.StatusOK,
		body:   `{"access_token":"` + connectTestAccess + `","refresh_token":"` + connectTestRefresh + `","expires_in":3600,"scope":"user:inference"}`,
	}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got := map[string]string{}
		if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			_ = json.Unmarshal(raw, &got)
		} else if form, err := url.ParseQuery(string(raw)); err == nil {
			for k := range form {
				got[k] = form.Get(k)
			}
		}
		stub.mu.Lock()
		stub.calls = append(stub.calls, got)
		status, body, hook := stub.status, stub.body, stub.hook
		stub.mu.Unlock()
		if hook != nil {
			hook(r)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *vendorTokenStub) respond(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *vendorTokenStub) onRequest(hook func(r *http.Request)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hook = hook
}

func (s *vendorTokenStub) requests() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.calls...)
}

// anthropicEndpoints / openAIEndpoints are the default vendor endpoints re-pointed
// at the stub, with a recognisable authorize host.
func (s *vendorTokenStub) anthropicEndpoints() vendorauth.Endpoints {
	ep := vendorauth.DefaultAnthropicEndpoints()
	ep.AuthorizeURL = "https://authorize.anthropic.test/oauth/authorize"
	ep.TokenURL = s.srv.URL + "/v1/oauth/token"
	return ep
}

func (s *vendorTokenStub) openAIEndpoints() vendorauth.Endpoints {
	ep := vendorauth.DefaultOpenAIEndpoints()
	ep.AuthorizeURL = "https://authorize.openai.test/oauth/authorize"
	ep.TokenURL = s.srv.URL + "/oauth/token"
	return ep
}

// newVendorConnectTestService is the vendor-account test service (flag ON,
// volatile "plain:" sealing) with its vendor endpoints pointed at one stub.
func newVendorConnectTestService(t *testing.T) (*Service, *routing.MemoryStore, *vendorTokenStub) {
	t.Helper()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	stub := newVendorTokenStub(t)
	svc.vendorConnect.anthropic = stub.anthropicEndpoints()
	svc.vendorConnect.openai = stub.openAIEndpoints()
	return svc, routeStore, stub
}

func createSubscriptionAccount(t *testing.T, svc *Service, principal auth.Token, vendor, name string) VendorAccountDTO {
	t.Helper()
	return createTestVendorAccount(t, svc, principal, CreateVendorAccountRequest{Vendor: vendor, AuthType: routing.VendorAuthSubscription, Name: name})
}

func storedTokenSet(t *testing.T, routeStore *routing.MemoryStore, svc *Service, id string) (routing.VendorAccount, vendorauth.TokenSet) {
	t.Helper()
	row, err := routeStore.VendorAccountByID(context.Background(), id)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	ts, err := vendorauth.OpenTokenSet(svc.cipher, row.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	return row, ts
}

func pendingConnect(t *testing.T, svc *Service, id string) pendingVendorConnect {
	t.Helper()
	svc.vendorConnect.mu.Lock()
	defer svc.vendorConnect.mu.Unlock()
	p, ok := svc.vendorConnect.pending[id]
	if !ok {
		t.Fatalf("no pending connect for %s", id)
	}
	return p
}

func pendingCount(svc *Service) int {
	svc.vendorConnect.mu.Lock()
	defer svc.vendorConnect.mu.Unlock()
	return len(svc.vendorConnect.pending)
}

// connectTestJWT is an unsigned three-segment JWT carrying the ChatGPT account
// facts under the https://api.openai.com/auth claim, the shape of both the
// OpenAI id_token and (as the Codex CLI stores it) the access token.
func connectTestJWT(t *testing.T, accountID, plan string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		vendorauth.OpenAIAuthClaimNamespace: map[string]any{vendorauth.OpenAIClaimAccountID: accountID, vendorauth.OpenAIClaimPlanType: plan},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

// --- import ---------------------------------------------------------------

func TestConnectVendorAccountImportSealsTheTokenSet(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if acc.SubscriptionConnected {
		t.Fatal("a fresh subscription account must start unconnected")
	}
	expires := time.Date(2026, 10, 8, 9, 30, 0, 0, time.UTC)

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: "  " + connectTestAccess + "\n", RefreshToken: " " + connectTestRefresh + " ", ExpiresAt: expires})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive || dto.AuthType != routing.VendorAuthSubscription || dto.APIKeySet {
		t.Fatalf("dto = %+v, want a connected active subscription account without an api key", dto)
	}
	raw, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal dto: %v", err)
	}
	for _, secret := range []string{connectTestAccess, connectTestRefresh} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("dto JSON leaks a token: %s", raw)
		}
	}

	row, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if !strings.HasPrefix(row.OAuthTokens, "plain:") || strings.Contains(row.OAuthTokens[len("plain:"):], "  ") {
		t.Fatalf("stored token blob = %q, want the sealed plain: envelope", row.OAuthTokens)
	}
	if ts.AccessToken != connectTestAccess || ts.RefreshToken != connectTestRefresh || !ts.ExpiresAt.Equal(expires) {
		t.Fatalf("stored token set = %v (expires %v), want trimmed access/refresh and the given expiry", ts, ts.ExpiresAt)
	}
	if row.APIKey != "" {
		t.Fatalf("api key = %q, want it untouched (empty)", row.APIKey)
	}
}

func TestConnectVendorAccountImportSealsWithTheCipher(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestServiceWithCipher(t, now, newTestCipher(t), false)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	row, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if !strings.HasPrefix(row.OAuthTokens, "enc:") || strings.Contains(row.OAuthTokens, connectTestAccess) {
		t.Fatalf("stored token blob = %q, want an enc: envelope without the plaintext token", row.OAuthTokens)
	}
	if ts.AccessToken != connectTestAccess || ts.RefreshToken != "" || !ts.ExpiresAt.IsZero() {
		t.Fatalf("token set = %v, want the access token only (refresh and expiry are optional)", ts)
	}
}

// An imported OpenAI access token is itself a JWT carrying the account id the
// dispatch needs (the chatgpt-account-id header), so import fills AccountID and
// PlanType from it and yields the same token set the code-paste path does.
func TestConnectVendorAccountImportFillsTheOpenAIAccountFromTheAccessToken(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	jwt := connectTestJWT(t, "acct-123", "plus")

	imported := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Imported")
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), imported.ID, ConnectVendorAccountImportRequest{AccessToken: jwt, RefreshToken: connectTestRefresh}); err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	_, importedSet := storedTokenSet(t, routeStore, svc, imported.ID)
	if importedSet.AccessToken != jwt || importedSet.AccountID != "acct-123" || importedSet.PlanType != "plus" {
		t.Fatalf("imported token set = %v, want the access token with account_id acct-123 and plan plus", importedSet)
	}

	// The code-paste path for the same identity stores the same account facts.
	pasted := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Pasted")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), pasted.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	stub.respond(http.StatusOK, `{"access_token":"`+jwt+`","refresh_token":"`+connectTestRefresh+`","id_token":"`+jwt+`","expires_in":3600}`)
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), pasted.ID, "openai-code"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, pastedSet := storedTokenSet(t, routeStore, svc, pasted.ID)
	if pastedSet.AccountID != importedSet.AccountID || pastedSet.PlanType != importedSet.PlanType {
		t.Fatalf("import (%v) and code-paste (%v) must agree on the account facts", importedSet, pastedSet)
	}
}

// The claim read is best-effort: an OpenAI token that is not a JWT imports fine
// with no account id, and an Anthropic token is never inspected.
func TestConnectVendorAccountImportTolerantAccountClaims(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)

	opaque := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Opaque")
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), opaque.ID, ConnectVendorAccountImportRequest{AccessToken: "sk-not-a-jwt"}); err != nil {
		t.Fatalf("import of a non-JWT OpenAI token: %v", err)
	}
	if _, ts := storedTokenSet(t, routeStore, svc, opaque.ID); ts.AccessToken != "sk-not-a-jwt" || ts.AccountID != "" || ts.PlanType != "" {
		t.Fatalf("token set = %v, want the token with no account facts", ts)
	}

	anthropic := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude")
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), anthropic.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestJWT(t, "acct-123", "plus")}); err != nil {
		t.Fatalf("import on an anthropic account: %v", err)
	}
	if _, ts := storedTokenSet(t, routeStore, svc, anthropic.ID); ts.AccountID != "" || ts.PlanType != "" {
		t.Fatalf("anthropic token set = %v, want OpenAI claims left alone", ts)
	}
}

func TestConnectVendorAccountImportReactivatesAnAccountThatNeedsReconnect(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	row.Status = routing.VendorAccountStatusNeedsReconnect
	if err := routeStore.UpdateVendorAccount(context.Background(), row); err != nil {
		t.Fatalf("UpdateVendorAccount: %v", err)
	}

	dto, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess, RefreshToken: connectTestRefresh})
	if err != nil {
		t.Fatalf("ConnectVendorAccountImport: %v", err)
	}
	if dto.Status != routing.VendorAccountStatusActive {
		t.Fatalf("status = %q, want active after a reconnect", dto.Status)
	}
}

func TestConnectVendorAccountImportValidation(t *testing.T) {
	svc, routeStore, _ := newVendorConnectTestService(t)
	sub := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	apiKeyAcc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), sub.ID, ConnectVendorAccountImportRequest{AccessToken: "  \n", RefreshToken: connectTestRefresh}); !errors.Is(err, ErrVendorAccountConnectTokenRequired) {
		t.Fatalf("blank access token: err = %v, want ErrVendorAccountConnectTokenRequired", err)
	}
	if _, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), apiKeyAcc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Fatalf("api_key account: err = %v, want ErrVendorAccountNotSubscription", err)
	}
	for _, id := range []string{sub.ID, apiKeyAcc.ID} {
		row, err := routeStore.VendorAccountByID(context.Background(), id)
		if err != nil {
			t.Fatalf("VendorAccountByID: %v", err)
		}
		if row.OAuthTokens != "" {
			t.Fatalf("account %s stored tokens %q after a refused import", id, row.OAuthTokens)
		}
	}
}

func TestConnectVendorAccountImportOnAKeylessDiskStoreIsAKeyRequiredError(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestServiceWithCipher(t, now, nil, false)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")

	_, err := svc.ConnectVendorAccountImport(context.Background(), ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess})
	if !errors.Is(err, ErrVendorAccountConnectKeyRequired) || !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectKeyRequired wrapping capture.ErrKeyRequired", err)
	}
	row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
	if err != nil || row.OAuthTokens != "" {
		t.Fatalf("row = %+v, %v, want nothing persisted (never plaintext)", row, err)
	}
}

// --- begin ----------------------------------------------------------------

// A store that cannot seal a token set is found out before the user signs in at
// the vendor, not after: begin fails fast with the key-required error and keeps
// no pending state.
func TestBeginVendorAccountConnectOnAKeylessDiskStoreFailsFast(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	svc.settingsVolatile = false // a disk-backed store ...
	svc.cipher = nil             // ... with no encryption key

	_, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID)
	if !errors.Is(err, ErrVendorAccountConnectKeyRequired) || !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectKeyRequired wrapping capture.ErrKeyRequired", err)
	}
	if pendingCount(svc) != 0 {
		t.Fatal("a refused begin must not store a pending entry")
	}
}

func TestBeginVendorAccountConnectAnthropic(t *testing.T) {
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")

	authorizeURL, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("BeginVendorAccountConnect: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", authorizeURL, err)
	}
	ep := stub.anthropicEndpoints()
	if got := u.Scheme + "://" + u.Host + u.Path; got != ep.AuthorizeURL {
		t.Fatalf("authorize url base = %q, want the injected %q", got, ep.AuthorizeURL)
	}
	q := u.Query()
	want := map[string]string{
		"response_type":                   "code",
		"client_id":                       ep.ClientID,
		"redirect_uri":                    ep.RedirectURI,
		"scope":                           ep.Scopes,
		"code_challenge":                  vendorauth.PKCEChallenge(pending.verifier),
		"code_challenge_method":           "S256",
		"state":                           pending.state,
		vendorauth.AnthropicParamShowCode: vendorauth.AnthropicParamShowCodeValue,
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("authorize param %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if pending.verifier == "" || pending.state == "" || pending.vendor != routing.VendorAnthropic {
		t.Fatalf("pending = %+v, want verifier, state and the anthropic vendor", pending)
	}
	if strings.Contains(authorizeURL, pending.verifier) {
		t.Fatalf("authorize url %q must carry the challenge, never the verifier", authorizeURL)
	}
}

func TestBeginVendorAccountConnectOpenAI(t *testing.T) {
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	authorizeURL, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("BeginVendorAccountConnect: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)
	u, err := url.Parse(authorizeURL)
	if err != nil {
		t.Fatalf("authorize url %q: %v", authorizeURL, err)
	}
	ep := stub.openAIEndpoints()
	if got := u.Scheme + "://" + u.Host + u.Path; got != ep.AuthorizeURL {
		t.Fatalf("authorize url base = %q, want the injected %q", got, ep.AuthorizeURL)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":                      ep.ClientID,
		"redirect_uri":                   ep.RedirectURI,
		"code_challenge":                 vendorauth.PKCEChallenge(pending.verifier),
		"state":                          pending.state,
		vendorauth.OpenAIParamOriginator: vendorauth.OpenAIOriginator,
		vendorauth.OpenAIParamCodexCLISimplifiedFlow: "true",
	}
	for k, v := range want {
		if q.Get(k) != v {
			t.Errorf("authorize param %s = %q, want %q", k, q.Get(k), v)
		}
	}
	if pending.vendor != routing.VendorOpenAI {
		t.Fatalf("pending vendor = %q, want openai", pending.vendor)
	}
}

func TestBeginVendorAccountConnectAgainReplacesThePendingEntry(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")

	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	first := pendingConnect(t, svc, acc.ID)
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	second := pendingConnect(t, svc, acc.ID)
	if first.state == second.state || first.verifier == second.verifier || pendingCount(svc) != 1 {
		t.Fatalf("first = %+v, second = %+v, count = %d, want one fresh entry replacing the first", first, second, pendingCount(svc))
	}
}

func TestBeginVendorAccountConnectPrunesExpiredEntries(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _, _ := newVendorConnectTestService(t)
	stale := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Stale")
	fresh := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "Fresh")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), stale.ID); err != nil {
		t.Fatalf("begin stale: %v", err)
	}
	svc.clock = func() time.Time { return now.Add(vendorConnectPendingTTL + time.Second) }
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), fresh.ID); err != nil {
		t.Fatalf("begin fresh: %v", err)
	}
	if pendingCount(svc) != 1 {
		t.Fatalf("pending count = %d, want the expired entry pruned", pendingCount(svc))
	}
	pendingConnect(t, svc, fresh.ID)
}

func TestBeginVendorAccountConnectRefusesAnAPIKeyAccount(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Fatalf("err = %v, want ErrVendorAccountNotSubscription", err)
	}
	if pendingCount(svc) != 0 {
		t.Fatal("a refused begin must not store a pending entry")
	}
}

// --- complete -------------------------------------------------------------

func TestCompleteVendorAccountConnectAnthropicCodeAndState(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)

	dto, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "  the-code#"+pending.state+"\n")
	if err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive || dto.AuthType != routing.VendorAuthSubscription {
		t.Fatalf("dto = %+v, want a connected active subscription account", dto)
	}
	if raw, _ := json.Marshal(dto); strings.Contains(string(raw), connectTestAccess) || strings.Contains(string(raw), connectTestRefresh) {
		t.Fatalf("dto JSON leaks a token: %s", raw)
	}

	calls := stub.requests()
	if len(calls) != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", len(calls))
	}
	ep := stub.anthropicEndpoints()
	for k, v := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "the-code",
		"state":         pending.state,
		"code_verifier": pending.verifier,
		"redirect_uri":  ep.RedirectURI,
		"client_id":     ep.ClientID,
	} {
		if calls[0][k] != v {
			t.Errorf("exchange %s = %q, want %q", k, calls[0][k], v)
		}
	}

	row, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if ts.AccessToken != connectTestAccess || ts.RefreshToken != connectTestRefresh || ts.ExpiresAt.IsZero() {
		t.Fatalf("stored token set = %v, want the exchanged tokens with an expiry", ts)
	}
	if row.Status != routing.VendorAccountStatusActive || row.AuthType != routing.VendorAuthSubscription {
		t.Fatalf("row status/auth_type = %q/%q", row.Status, row.AuthType)
	}
	if pendingCount(svc) != 0 {
		t.Fatal("the pending entry must be cleared after a successful connect")
	}
}

// A bare code (no "#state") is exchanged with the pending entry's own state.
func TestCompleteVendorAccountConnectRawCodeUsesThePendingState(t *testing.T) {
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)

	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "raw-code"); err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	calls := stub.requests()
	if len(calls) != 1 || calls[0]["code"] != "raw-code" || calls[0]["state"] != pending.state || calls[0]["code_verifier"] != pending.verifier {
		t.Fatalf("exchange = %v, want the raw code with the pending state and verifier", calls)
	}
}

func TestCompleteVendorAccountConnectOpenAI(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)
	idToken := connectTestJWT(t, "acct-123", "plus")
	stub.respond(http.StatusOK, `{"access_token":"`+connectTestAccess+`","refresh_token":"`+connectTestRefresh+`","id_token":"`+idToken+`","expires_in":3600}`)

	dto, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "openai-code")
	if err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	if !dto.SubscriptionConnected {
		t.Fatalf("dto = %+v, want connected", dto)
	}
	calls := stub.requests()
	ep := stub.openAIEndpoints()
	if len(calls) != 1 {
		t.Fatalf("token endpoint calls = %d, want 1", len(calls))
	}
	for k, v := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "openai-code",
		"code_verifier": pending.verifier,
		"redirect_uri":  ep.RedirectURI,
		"client_id":     ep.ClientID,
	} {
		if calls[0][k] != v {
			t.Errorf("exchange %s = %q, want %q", k, calls[0][k], v)
		}
	}
	_, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if ts.AccessToken != connectTestAccess || ts.AccountID != "acct-123" || ts.PlanType != "plus" {
		t.Fatalf("stored token set = %v, want the exchanged tokens with the account id and plan", ts)
	}
}

// OpenAI's redirect lands on a dead loopback URL; users paste that whole URL.
func TestCompleteVendorAccountConnectAcceptsAPastedCallbackURL(t *testing.T) {
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)

	pasted := "http://localhost:1455/auth/callback?code=url-code&scope=openid&state=" + url.QueryEscape(pending.state)
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, pasted); err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	if calls := stub.requests(); len(calls) != 1 || calls[0]["code"] != "url-code" {
		t.Fatalf("exchange = %v, want the code extracted from the pasted URL", calls)
	}
}

func TestCompleteVendorAccountConnectRejectsAMismatchedState(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	pending := pendingConnect(t, svc, acc.ID)

	_, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "the-code#not-the-issued-state")
	if !errors.Is(err, ErrVendorAccountConnectState) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectState", err)
	}
	if calls := stub.requests(); len(calls) != 0 {
		t.Fatalf("a state mismatch must not reach the vendor, got %v", calls)
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored after a state mismatch: %q", row.OAuthTokens)
	}
	// The user can still paste the right value: the pending entry survives.
	if got := pendingConnect(t, svc, acc.ID); got.state != pending.state {
		t.Fatal("a state mismatch must keep the pending entry")
	}
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "the-code#"+pending.state); err != nil {
		t.Fatalf("retry with the right state: %v", err)
	}
}

func TestCompleteVendorAccountConnectVendorRejectionKeepsTheAccountUntouched(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	stub.respond(http.StatusUnauthorized, `{"error":{"type":"invalid_grant","message":"code expired"}}`)

	_, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "bad-code")
	if !errors.Is(err, ErrVendorAccountConnectRejected) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectRejected", err)
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored after a rejection: %q", row.OAuthTokens)
	}
	// A typo is recoverable: the pending entry stays, so a corrected paste works.
	stub.respond(http.StatusOK, `{"access_token":"`+connectTestAccess+`","expires_in":60}`)
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "good-code"); err != nil {
		t.Fatalf("retry after a rejection: %v", err)
	}
}

func TestCompleteVendorAccountConnectUpstreamFailures(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
	}{
		"server error":   {http.StatusInternalServerError, `{"error":"server_error"}`},
		"rate limited":   {http.StatusTooManyRequests, `{"error":"rate_limited"}`},
		"unusable reply": {http.StatusOK, `not json`},
	} {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, stub := newVendorConnectTestService(t)
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
			if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
				t.Fatalf("begin: %v", err)
			}
			stub.respond(tc.status, tc.body)
			_, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code")
			if !errors.Is(err, ErrVendorAccountConnectUpstream) || errors.Is(err, ErrVendorAccountConnectRejected) {
				t.Fatalf("err = %v, want ErrVendorAccountConnectUpstream (and not a rejection)", err)
			}
			if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
				t.Fatalf("tokens stored after an upstream failure: %q", row.OAuthTokens)
			}
		})
	}
}

func TestCompleteVendorAccountConnectWithoutAPendingEntry(t *testing.T) {
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")

	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code"); !errors.Is(err, ErrVendorAccountConnectState) {
		t.Fatalf("never begun: err = %v, want ErrVendorAccountConnectState", err)
	}
	if len(stub.requests()) != 0 {
		t.Fatal("a complete without a begin must not reach the vendor")
	}
}

func TestCompleteVendorAccountConnectAfterTheTTL(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}

	// One second short of the TTL the entry is still usable.
	svc.clock = func() time.Time { return now.Add(vendorConnectPendingTTL - time.Second) }
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code"); err != nil {
		t.Fatalf("complete inside the TTL: %v", err)
	}

	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	calls := len(stub.requests())
	svc.clock = func() time.Time { return now.Add(2 * vendorConnectPendingTTL) }
	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code"); !errors.Is(err, ErrVendorAccountConnectState) {
		t.Fatalf("complete after the TTL: err = %v, want ErrVendorAccountConnectState", err)
	}
	if len(stub.requests()) != calls {
		t.Fatal("an expired pending entry must not reach the vendor")
	}
	if pendingCount(svc) != 0 {
		t.Fatal("an expired pending entry must be dropped when it is looked up")
	}
}

// The authorization code is single-use, so complete must not spend it on a vendor
// exchange whose result could not be stored: the seal probe runs BEFORE the
// exchange. (Here the store lost its key after begin.)
func TestCompleteVendorAccountConnectDoesNotSpendTheCodeWhenTheTokensCannotBeSealed(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	svc.settingsVolatile = false
	svc.cipher = nil

	_, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code")
	if !errors.Is(err, ErrVendorAccountConnectKeyRequired) || !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectKeyRequired wrapping capture.ErrKeyRequired", err)
	}
	if calls := stub.requests(); len(calls) != 0 {
		t.Fatalf("the vendor was called %d time(s); the one-time code must not be spent on an unstorable result", len(calls))
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored without a key: %q", row.OAuthTokens)
	}
}

func TestCompleteVendorAccountConnectRequiresACode(t *testing.T) {
	svc, _, _ := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, pasted := range []string{"", "   \n", "#only-a-state", "http://localhost:1455/auth/callback?error=access_denied"} {
		if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, pasted); !errors.Is(err, ErrVendorAccountConnectCodeRequired) {
			t.Fatalf("paste %q: err = %v, want ErrVendorAccountConnectCodeRequired", pasted, err)
		}
	}
}

// Completing re-reads the account after the (slow) vendor round trip, so a rename
// made meanwhile is not overwritten by the stale copy loaded before it.
func TestCompleteVendorAccountConnectKeepsAConcurrentRename(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	// The rename lands while the vendor call is in flight.
	stub.onRequest(func(r *http.Request) {
		newName := "Renamed meanwhile"
		if _, err := svc.UpdateVendorAccount(r.Context(), ownerToken(), acc.ID, UpdateVendorAccountRequest{Name: &newName}); err != nil {
			t.Errorf("rename: %v", err)
		}
	})

	if _, err := svc.CompleteVendorAccountConnect(context.Background(), ownerToken(), acc.ID, "a-code"); err != nil {
		t.Fatalf("CompleteVendorAccountConnect: %v", err)
	}
	row, _ := storedTokenSet(t, routeStore, svc, acc.ID)
	if row.Name != "Renamed meanwhile" {
		t.Fatalf("name = %q, want the concurrent rename kept", row.Name)
	}
}

// --- authorization and the master flag -------------------------------------

func TestVendorAccountConnectIsOwnerOnly(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	if _, err := svc.BeginVendorAccountConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("owner begin: %v", err)
	}

	ctx := context.Background()
	for label, principal := range map[string]auth.Token{
		"another user":       otherToken(),
		"system scope":       systemToken(),
		"no user identity":   {},
		"another user (adm)": {UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
	} {
		if _, err := svc.ConnectVendorAccountImport(ctx, principal, acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s import: err = %v, want ErrVendorAccountNotFound", label, err)
		}
		if _, err := svc.BeginVendorAccountConnect(ctx, principal, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s begin: err = %v, want ErrVendorAccountNotFound", label, err)
		}
		if _, err := svc.CompleteVendorAccountConnect(ctx, principal, acc.ID, "a-code"); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s complete: err = %v, want ErrVendorAccountNotFound", label, err)
		}
	}
	if _, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountNotFound) {
		t.Fatalf("unknown id: err = %v, want ErrVendorAccountNotFound", err)
	}
	if len(stub.requests()) != 0 {
		t.Fatal("a refused principal must not reach the vendor")
	}
	if row, _ := routeStore.VendorAccountByID(ctx, acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored by a refused principal: %q", row.OAuthTokens)
	}
	// The refused principals did not consume the owner's pending entry.
	pendingConnect(t, svc, acc.ID)
}

func TestVendorAccountConnectRefusesWhileTheMasterFlagIsOff(t *testing.T) {
	svc, routeStore, stub := newVendorConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")
	setVendorAccountsEnabled(t, svc, false)

	ctx := context.Background()
	if _, err := svc.ConnectVendorAccountImport(ctx, ownerToken(), acc.ID, ConnectVendorAccountImportRequest{AccessToken: connectTestAccess}); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("import: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("begin: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, err := svc.CompleteVendorAccountConnect(ctx, ownerToken(), acc.ID, "a-code"); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("complete: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, err := svc.BeginVendorAccountConnect(ctx, ownerToken(), "va_missing"); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("begin on an unknown id: err = %v, want ErrVendorAccountsDisabled (a disabled area confirms nothing)", err)
	}
	if pendingCount(svc) != 0 || len(stub.requests()) != 0 {
		t.Fatal("a disabled module must neither store pending state nor reach the vendor")
	}
	if row, _ := routeStore.VendorAccountByID(ctx, acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored while disabled: %q", row.OAuthTokens)
	}
}

// --- wiring ----------------------------------------------------------------

func TestNewServiceDefaultsAndInjectsTheVendorOAuthEndpoints(t *testing.T) {
	defaults := NewService(ServiceDeps{})
	if defaults.vendorConnect.anthropic != vendorauth.DefaultAnthropicEndpoints() || defaults.vendorConnect.openai != vendorauth.DefaultOpenAIEndpoints() {
		t.Fatalf("default endpoints = %+v / %+v, want the vendorauth defaults", defaults.vendorConnect.anthropic, defaults.vendorConnect.openai)
	}
	if defaults.vendorConnect.client == nil || defaults.vendorConnect.client.Timeout <= 0 {
		t.Fatalf("default client = %+v, want one with a timeout", defaults.vendorConnect.client)
	}

	anthropic := vendorauth.DefaultAnthropicEndpoints()
	anthropic.TokenURL = "http://127.0.0.1:1/anthropic"
	openAI := vendorauth.DefaultOpenAIEndpoints()
	openAI.TokenURL = "http://127.0.0.1:1/openai"
	client := &http.Client{Timeout: time.Second}
	injected := NewService(ServiceDeps{VendorAnthropicEndpoints: anthropic, VendorOpenAIEndpoints: openAI, VendorHTTPClient: client})
	if injected.vendorConnect.anthropic != anthropic || injected.vendorConnect.openai != openAI || injected.vendorConnect.client != client {
		t.Fatal("the injected endpoints / http client were not used")
	}
}

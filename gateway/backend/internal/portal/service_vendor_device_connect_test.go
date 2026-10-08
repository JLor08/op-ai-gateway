// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/capture"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
	"time"
)

// vendorDeviceStub is an httptest OpenAI deviceauth host for the portal device
// flow: it answers the usercode, deviceauth-token and oauth-token (exchange)
// paths with configurable status/body and records how often each was hit and the
// last poll body. The exchange defaults to the shared connect test tokens.
type vendorDeviceStub struct {
	srv *httptest.Server

	mu             sync.Mutex
	usercodeStatus int
	usercodeBody   string
	tokenStatus    int
	tokenBody      string
	exchangeStatus int
	exchangeBody   string
	usercodeCalls  int
	tokenCalls     int
	exchangeCalls  int
	lastTokenBody  map[string]string
}

func newVendorDeviceStub(t *testing.T) *vendorDeviceStub {
	t.Helper()
	stub := &vendorDeviceStub{
		usercodeStatus: http.StatusOK,
		usercodeBody:   `{"device_auth_id":"dev-abc","user_code":"WXYZ-1234","interval":"5"}`,
		// Default: the user has not approved yet, so the first poll is pending.
		tokenStatus:    http.StatusForbidden,
		tokenBody:      `{"error":"authorization_pending"}`,
		exchangeStatus: http.StatusOK,
		exchangeBody:   `{"access_token":"` + connectTestAccess + `","refresh_token":"` + connectTestRefresh + `","expires_in":3600}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		stub.usercodeCalls++
		status, body := stub.usercodeStatus, stub.usercodeBody
		stub.mu.Unlock()
		writeDeviceJSON(w, status, body)
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		decoded := map[string]string{}
		_ = json.Unmarshal(raw, &decoded)
		stub.mu.Lock()
		stub.tokenCalls++
		stub.lastTokenBody = decoded
		status, body := stub.tokenStatus, stub.tokenBody
		stub.mu.Unlock()
		writeDeviceJSON(w, status, body)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		stub.exchangeCalls++
		status, body := stub.exchangeStatus, stub.exchangeBody
		stub.mu.Unlock()
		writeDeviceJSON(w, status, body)
	})
	stub.srv = httptest.NewServer(mux)
	t.Cleanup(stub.srv.Close)
	return stub
}

func writeDeviceJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *vendorDeviceStub) setToken(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenStatus, s.tokenBody = status, body
}

func (s *vendorDeviceStub) setUsercode(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usercodeStatus, s.usercodeBody = status, body
}

func (s *vendorDeviceStub) setExchange(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchangeStatus, s.exchangeBody = status, body
}

func (s *vendorDeviceStub) counts() (usercode, token, exchange int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usercodeCalls, s.tokenCalls, s.exchangeCalls
}

func (s *vendorDeviceStub) endpoints() vendorauth.Endpoints {
	ep := vendorauth.DefaultOpenAIEndpoints()
	ep.AuthorizeURL = "https://authorize.openai.test/oauth/authorize"
	ep.TokenURL = s.srv.URL + "/oauth/token"
	ep.DeviceUsercodeURL = s.srv.URL + "/api/accounts/deviceauth/usercode"
	ep.DeviceTokenURL = s.srv.URL + "/api/accounts/deviceauth/token"
	// DeviceVerificationURL / DeviceCallbackRedirect keep the auth.openai.com
	// defaults: a display string and an exchange parameter, neither fetched here.
	return ep
}

// newVendorDeviceConnectTestService is the vendor-account test service (flag ON,
// volatile "plain:" sealing) with the OpenAI endpoints pointed at one device stub.
func newVendorDeviceConnectTestService(t *testing.T) (*Service, *routing.MemoryStore, *vendorDeviceStub) {
	t.Helper()
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, routeStore := newVendorAccountTestService(t, now)
	stub := newVendorDeviceStub(t)
	svc.vendorConnect.openai = stub.endpoints()
	return svc, routeStore, stub
}

func pendingDeviceConnect(t *testing.T, svc *Service, id string) pendingVendorDeviceConnect {
	t.Helper()
	svc.vendorDeviceConnect.mu.Lock()
	defer svc.vendorDeviceConnect.mu.Unlock()
	p, ok := svc.vendorDeviceConnect.pending[id]
	if !ok {
		t.Fatalf("no pending device connect for %s", id)
	}
	return p
}

// --- begin ----------------------------------------------------------------

func TestBeginVendorAccountDeviceConnectStoresPendingAndReturnsUserCode(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	userCode, verificationURL, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("BeginVendorAccountDeviceConnect: %v", err)
	}
	if userCode != "WXYZ-1234" {
		t.Fatalf("userCode = %q, want WXYZ-1234", userCode)
	}
	if verificationURL != vendorauth.OpenAIDeviceVerificationURL {
		t.Fatalf("verificationURL = %q, want %q", verificationURL, vendorauth.OpenAIDeviceVerificationURL)
	}
	pending := pendingDeviceConnect(t, svc, acc.ID)
	if pending.deviceAuthID != "dev-abc" || pending.userCode != "WXYZ-1234" {
		t.Fatalf("pending = %+v, want the device_auth_id and user_code from the vendor", pending)
	}
	if usercode, _, _ := stub.counts(); usercode != 1 {
		t.Fatalf("usercode calls = %d, want 1", usercode)
	}
}

func TestBeginVendorAccountDeviceConnectOnAKeylessDiskStoreFailsFast(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	svc.settingsVolatile = false // a disk-backed store ...
	svc.cipher = nil             // ... with no encryption key

	_, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if !errors.Is(err, ErrVendorAccountConnectKeyRequired) || !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectKeyRequired wrapping capture.ErrKeyRequired", err)
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("a refused begin must not store a pending entry")
	}
	if usercode, _, _ := stub.counts(); usercode != 0 {
		t.Fatal("a keyless store must fail before the vendor round trip")
	}
}

func TestBeginVendorAccountDeviceConnectAgainReplacesThePendingEntry(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("first begin: %v", err)
	}
	stub.setUsercode(http.StatusOK, `{"device_auth_id":"dev-second","user_code":"AAAA-0000","interval":"5"}`)
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if got := pendingDeviceConnect(t, svc, acc.ID); got.deviceAuthID != "dev-second" || svc.vendorDeviceConnect.count() != 1 {
		t.Fatalf("pending = %+v, count = %d, want one fresh entry replacing the first", got, svc.vendorDeviceConnect.count())
	}
}

func TestBeginVendorAccountDeviceConnectUpstreamFailure(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	stub.setUsercode(http.StatusInternalServerError, `{"error":"server_error"}`)

	_, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if !errors.Is(err, ErrVendorAccountConnectUpstream) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectUpstream", err)
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("a failed begin must not store a pending entry")
	}
}

// --- poll -----------------------------------------------------------------

func TestPollVendorAccountDeviceConnectPending(t *testing.T) {
	svc, routeStore, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}

	connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil || connected {
		t.Fatalf("poll = (%v, %v), want (false, nil) while still pending", connected, err)
	}
	// The pending entry survives so the next poll continues, and the poll used the
	// stored device_auth_id / user_code.
	pending := pendingDeviceConnect(t, svc, acc.ID)
	if _, token, exchange := stub.counts(); token != 1 || exchange != 0 {
		t.Fatalf("token/exchange calls = %d/%d, want 1/0", token, exchange)
	}
	if stub.lastTokenBody["device_auth_id"] != pending.deviceAuthID || stub.lastTokenBody["user_code"] != pending.userCode {
		t.Fatalf("poll body = %v, want the stored device_auth_id/user_code", stub.lastTokenBody)
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored while still pending: %q", row.OAuthTokens)
	}
}

func TestPollVendorAccountDeviceConnectAuthorized(t *testing.T) {
	svc, routeStore, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	idToken := connectTestJWT(t, "acct-dev", "pro")
	stub.setToken(http.StatusOK, `{"authorization_code":"auth-code","code_challenge":"chal","code_verifier":"ver"}`)
	stub.setExchange(http.StatusOK, `{"access_token":"`+connectTestAccess+`","refresh_token":"`+connectTestRefresh+`","id_token":"`+idToken+`","expires_in":3600}`)

	connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil || !connected {
		t.Fatalf("poll = (%v, %v), want (true, nil) once authorized", connected, err)
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("a successful device connect must clear the pending entry")
	}
	if _, token, exchange := stub.counts(); token != 1 || exchange != 1 {
		t.Fatalf("token/exchange calls = %d/%d, want 1/1", token, exchange)
	}

	// The account is now a connected, active subscription with the account facts,
	// and the DTO shows subscription_connected without leaking a token.
	dto, err := svc.GetVendorAccount(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("GetVendorAccount: %v", err)
	}
	if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive || dto.AuthType != routing.VendorAuthSubscription {
		t.Fatalf("dto = %+v, want a connected active subscription account", dto)
	}
	if raw, _ := json.Marshal(dto); strings.Contains(string(raw), connectTestAccess) || strings.Contains(string(raw), connectTestRefresh) {
		t.Fatalf("dto JSON leaks a token: %s", raw)
	}
	row, ts := storedTokenSet(t, routeStore, svc, acc.ID)
	if ts.AccessToken != connectTestAccess || ts.RefreshToken != connectTestRefresh || ts.AccountID != "acct-dev" || ts.PlanType != "pro" {
		t.Fatalf("stored token set = %v, want the exchanged tokens with the account facts", ts)
	}
	if !strings.HasPrefix(row.OAuthTokens, "plain:") {
		t.Fatalf("stored blob = %q, want a sealed envelope", row.OAuthTokens)
	}
}

func TestPollVendorAccountDeviceConnectWithoutABegin(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	if _, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountDeviceConnectState) {
		t.Fatalf("err = %v, want ErrVendorAccountDeviceConnectState", err)
	}
	if _, token, _ := stub.counts(); token != 0 {
		t.Fatal("a poll without a begin must not reach the vendor")
	}
}

func TestPollVendorAccountDeviceConnectAfterTheTTL(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	svc.clock = func() time.Time { return now.Add(2 * vendorDeviceConnectPendingTTL) }

	if _, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountDeviceConnectState) {
		t.Fatalf("err = %v, want ErrVendorAccountDeviceConnectState after the TTL", err)
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("an expired pending entry must be dropped when it is looked up")
	}
	if _, token, _ := stub.counts(); token != 0 {
		t.Fatal("an expired pending entry must not reach the vendor")
	}
}

// A poll error keeps or clears the pending entry by KIND: a transient upstream
// error (5xx, 429) is retryable and KEEPS the entry, so a single blip during the
// ~15-minute unattended poll loop does not abort the authorization; a genuine
// rejection CLEARS it. The failure is a poll error here and an exchange error in
// TestPollVendorAccountDeviceConnectExchangeFailure.
func TestPollVendorAccountDeviceConnectPollErrorKeepsPendingOnlyWhenTransient(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		body        string
		wantErr     error
		wantNotErr  error
		wantCleared bool
	}{
		"server error keeps pending":   {http.StatusInternalServerError, `{"error":"server_error"}`, ErrVendorAccountConnectUpstream, ErrVendorAccountConnectRejected, false},
		"rate limited keeps pending":   {http.StatusTooManyRequests, `{"error":"rate_limited"}`, ErrVendorAccountConnectUpstream, ErrVendorAccountConnectRejected, false},
		"rejection clears the pending": {http.StatusUnauthorized, `{"error":"invalid_grant"}`, ErrVendorAccountConnectRejected, nil, true},
	} {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, stub := newVendorDeviceConnectTestService(t)
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
			if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
				t.Fatalf("begin: %v", err)
			}
			stub.setToken(tc.status, tc.body)

			connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
			if connected || !errors.Is(err, tc.wantErr) {
				t.Fatalf("poll = (%v, %v), want (false, %v)", connected, err, tc.wantErr)
			}
			if tc.wantNotErr != nil && errors.Is(err, tc.wantNotErr) {
				t.Fatalf("err = %v, must not also be %v", err, tc.wantNotErr)
			}
			if gotCleared := svc.vendorDeviceConnect.count() == 0; gotCleared != tc.wantCleared {
				t.Fatalf("pending cleared = %v, want %v (a transient error must keep pending, a rejection must clear it)", gotCleared, tc.wantCleared)
			}
			if !tc.wantCleared {
				// A kept entry is retryable: a following authorized poll connects it.
				pendingDeviceConnect(t, svc, acc.ID)
				idToken := connectTestJWT(t, "acct-dev", "pro")
				stub.setToken(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"ver"}`)
				stub.setExchange(http.StatusOK, `{"access_token":"`+connectTestAccess+`","id_token":"`+idToken+`","expires_in":3600}`)
				if ok, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil || !ok {
					t.Fatalf("retry after a transient error = (%v, %v), want (true, nil)", ok, err)
				}
			}
			if !tc.wantCleared {
				return
			}
			if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
				t.Fatalf("tokens stored after a rejection: %q", row.OAuthTokens)
			}
		})
	}
}

// An exchange failure follows the same keep-or-clear rule as a poll error: a
// rejection (invalid_grant) clears the pending entry, a transient upstream failure
// keeps it for the next poll to retry.
func TestPollVendorAccountDeviceConnectExchangeFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		status      int
		body        string
		wantErr     error
		wantCleared bool
	}{
		"rejection clears": {http.StatusBadRequest, `{"error":"invalid_grant"}`, ErrVendorAccountConnectRejected, true},
		"transient keeps":  {http.StatusBadGateway, `{"error":"bad_gateway"}`, ErrVendorAccountConnectUpstream, false},
	} {
		t.Run(name, func(t *testing.T) {
			svc, routeStore, stub := newVendorDeviceConnectTestService(t)
			acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
			if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
				t.Fatalf("begin: %v", err)
			}
			stub.setToken(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"ver"}`)
			stub.setExchange(tc.status, tc.body)

			connected, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
			if connected || !errors.Is(err, tc.wantErr) {
				t.Fatalf("poll = (%v, %v), want (false, %v)", connected, err, tc.wantErr)
			}
			if gotCleared := svc.vendorDeviceConnect.count() == 0; gotCleared != tc.wantCleared {
				t.Fatalf("pending cleared = %v, want %v", gotCleared, tc.wantCleared)
			}
			if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
				t.Fatalf("tokens stored after an exchange failure: %q", row.OAuthTokens)
			}
		})
	}
}

// The authorization code a poll returns is single-use, so a poll must not spend
// it on an exchange whose result cannot be sealed: the seal probe runs BEFORE the
// poll. Here the store lost its key after begin.
func TestPollVendorAccountDeviceConnectDoesNotPollWhenTheTokensCannotBeSealed(t *testing.T) {
	svc, routeStore, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("begin: %v", err)
	}
	stub.setToken(http.StatusOK, `{"authorization_code":"auth-code","code_verifier":"ver"}`)
	svc.settingsVolatile = false
	svc.cipher = nil

	_, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if !errors.Is(err, ErrVendorAccountConnectKeyRequired) || !errors.Is(err, capture.ErrKeyRequired) {
		t.Fatalf("err = %v, want ErrVendorAccountConnectKeyRequired wrapping capture.ErrKeyRequired", err)
	}
	if _, token, exchange := stub.counts(); token != 0 || exchange != 0 {
		t.Fatalf("token/exchange calls = %d/%d, want 0/0; the one-time code must not be spent", token, exchange)
	}
	// The pending entry stays, so fixing the key lets a later poll succeed.
	pendingDeviceConnect(t, svc, acc.ID)
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored without a key: %q", row.OAuthTokens)
	}
}

// --- vendor / auth-type / authorization / master flag ----------------------

func TestVendorAccountDeviceConnectRejectsANonOpenAIAccount(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	// Point the anthropic endpoints at the stub too, so a leaked request would be
	// observable; it must not happen.
	svc.vendorConnect.anthropic = stub.endpoints()
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorAnthropic, "Claude Max")

	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountDeviceUnsupported) {
		t.Fatalf("begin: err = %v, want ErrVendorAccountDeviceUnsupported", err)
	}
	if _, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountDeviceUnsupported) {
		t.Fatalf("poll: err = %v, want ErrVendorAccountDeviceUnsupported", err)
	}
	if usercode, token, _ := stub.counts(); usercode != 0 || token != 0 {
		t.Fatal("a non-openai account must not reach the vendor")
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("a refused begin must not store a pending entry")
	}
}

func TestVendorAccountDeviceConnectRejectsANonSubscriptionAccount(t *testing.T) {
	svc, _, _ := newVendorDeviceConnectTestService(t)
	acc := createTestVendorAccount(t, svc, ownerToken(), apiKeyAccountRequest("Key account"))

	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Fatalf("begin: err = %v, want ErrVendorAccountNotSubscription", err)
	}
	if _, err := svc.PollVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountNotSubscription) {
		t.Fatalf("poll: err = %v, want ErrVendorAccountNotSubscription", err)
	}
}

func TestVendorAccountDeviceConnectIsOwnerOnly(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	if _, _, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID); err != nil {
		t.Fatalf("owner begin: %v", err)
	}

	ctx := context.Background()
	for label, principal := range map[string]auth.Token{
		"another user":       otherToken(),
		"system scope":       systemToken(),
		"no user identity":   {},
		"another user (adm)": {UserID: "usr_admin", Scopes: []string{"gateway:use", "admin"}},
	} {
		if _, _, err := svc.BeginVendorAccountDeviceConnect(ctx, principal, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s begin: err = %v, want ErrVendorAccountNotFound", label, err)
		}
		if _, err := svc.PollVendorAccountDeviceConnect(ctx, principal, acc.ID); !errors.Is(err, ErrVendorAccountNotFound) {
			t.Fatalf("%s poll: err = %v, want ErrVendorAccountNotFound", label, err)
		}
	}
	if _, token, _ := stub.counts(); token != 0 {
		t.Fatal("a refused principal must not reach the vendor")
	}
	// The refused principals did not consume the owner's pending entry.
	pendingDeviceConnect(t, svc, acc.ID)
}

func TestVendorAccountDeviceConnectRefusesWhileTheMasterFlagIsOff(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")
	setVendorAccountsEnabled(t, svc, false)

	ctx := context.Background()
	if _, _, err := svc.BeginVendorAccountDeviceConnect(ctx, ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("begin: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if _, err := svc.PollVendorAccountDeviceConnect(ctx, ownerToken(), acc.ID); !errors.Is(err, ErrVendorAccountsDisabled) {
		t.Fatalf("poll: err = %v, want ErrVendorAccountsDisabled", err)
	}
	if svc.vendorDeviceConnect.count() != 0 {
		t.Fatal("a disabled module must not store pending state")
	}
	if usercode, token, _ := stub.counts(); usercode != 0 || token != 0 {
		t.Fatal("a disabled module must not reach the vendor")
	}
}

// The verification URL and poll body never carry a secret, and a begin that
// points at a hand-built endpoint still produces a usable verification URL.
func TestBeginVendorAccountDeviceConnectVerificationURLIsOverridable(t *testing.T) {
	svc, _, stub := newVendorDeviceConnectTestService(t)
	ep := stub.endpoints()
	ep.DeviceVerificationURL = "https://auth.example.test/codex/device"
	svc.vendorConnect.openai = ep
	acc := createSubscriptionAccount(t, svc, ownerToken(), routing.VendorOpenAI, "ChatGPT Plus")

	_, verificationURL, err := svc.BeginVendorAccountDeviceConnect(context.Background(), ownerToken(), acc.ID)
	if err != nil {
		t.Fatalf("BeginVendorAccountDeviceConnect: %v", err)
	}
	if verificationURL != "https://auth.example.test/codex/device" {
		t.Fatalf("verificationURL = %q, want the overridden endpoint", verificationURL)
	}
}

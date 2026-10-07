// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
)

// vaDeviceStub is an httptest OpenAI deviceauth host for the HTTP-level device
// connect tests. It serves the usercode, deviceauth-token and oauth-token
// (exchange) paths with configurable status/body.
type vaDeviceStub struct {
	srv *httptest.Server

	mu             sync.Mutex
	usercodeStatus int
	usercodeBody   string
	tokenStatus    int
	tokenBody      string
	exchangeStatus int
	exchangeBody   string
}

func newVADeviceStub(t *testing.T) *vaDeviceStub {
	t.Helper()
	stub := &vaDeviceStub{
		usercodeStatus: http.StatusOK,
		usercodeBody:   `{"device_auth_id":"dev-http","user_code":"HTTP-1234","interval":"5"}`,
		tokenStatus:    http.StatusForbidden,
		tokenBody:      `{"error":"authorization_pending"}`,
		exchangeStatus: http.StatusOK,
		exchangeBody:   `{"access_token":"` + vaConnectAccess + `","refresh_token":"` + vaConnectRefresh + `","expires_in":3600}`,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		status, body := stub.usercodeStatus, stub.usercodeBody
		stub.mu.Unlock()
		vaDeviceWrite(w, status, body)
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		status, body := stub.tokenStatus, stub.tokenBody
		stub.mu.Unlock()
		vaDeviceWrite(w, status, body)
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		stub.mu.Lock()
		status, body := stub.exchangeStatus, stub.exchangeBody
		stub.mu.Unlock()
		vaDeviceWrite(w, status, body)
	})
	stub.srv = httptest.NewServer(mux)
	t.Cleanup(stub.srv.Close)
	return stub
}

func vaDeviceWrite(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (s *vaDeviceStub) setToken(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenStatus, s.tokenBody = status, body
}

func (s *vaDeviceStub) setExchange(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.exchangeStatus, s.exchangeBody = status, body
}

// newVendorDeviceConnectTestServer is a vendor-account server (flag ON) with the
// OpenAI endpoints -- token and deviceauth paths -- pointed at one device stub.
func newVendorDeviceConnectTestServer(t *testing.T) (*Server, *routing.MemoryStore, *vaDeviceStub) {
	t.Helper()
	stub := newVADeviceStub(t)
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(deps *portal.ServiceDeps) {
		openAI := vendorauth.DefaultOpenAIEndpoints()
		openAI.AuthorizeURL = "https://authorize.openai.test/oauth/authorize"
		openAI.TokenURL = stub.srv.URL + "/oauth/token"
		openAI.DeviceUsercodeURL = stub.srv.URL + "/api/accounts/deviceauth/usercode"
		openAI.DeviceTokenURL = stub.srv.URL + "/api/accounts/deviceauth/token"
		deps.VendorOpenAIEndpoints = openAI
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore, stub
}

func vaDeviceJWT(t *testing.T, accountID, plan string) string {
	t.Helper()
	claims, err := json.Marshal(map[string]any{
		vendorauth.OpenAIAuthClaimNamespace: map[string]any{vendorauth.OpenAIClaimAccountID: accountID, vendorauth.OpenAIClaimPlanType: plan},
	})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"
}

func TestVendorAccountDeviceConnectBeginAndPoll(t *testing.T) {
	srv, routeStore, stub := newVendorDeviceConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")

	// Begin returns the user code and the verification page URL, no token.
	beginRec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "device/begin"), vaOwnerSecret, "")
	if beginRec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, want 200, body = %s", beginRec.Code, beginRec.Body.String())
	}
	var begin map[string]string
	if err := json.Unmarshal(beginRec.Body.Bytes(), &begin); err != nil {
		t.Fatalf("unmarshal begin: %v (%s)", err, beginRec.Body.String())
	}
	if begin["user_code"] != "HTTP-1234" || begin["verification_url"] != vendorauth.OpenAIDeviceVerificationURL {
		t.Fatalf("begin = %v, want the user_code and verification URL", begin)
	}

	// A poll while the user has not approved yet is {connected:false}.
	pollRec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "device/poll"), vaOwnerSecret, "")
	if pollRec.Code != http.StatusOK {
		t.Fatalf("poll status = %d, want 200, body = %s", pollRec.Code, pollRec.Body.String())
	}
	if !strings.Contains(pollRec.Body.String(), `"connected":false`) {
		t.Fatalf("pending poll body = %s, want connected=false", pollRec.Body.String())
	}

	// Once the vendor authorizes, a poll connects the account and reports true.
	idToken := vaDeviceJWT(t, "acct-http", "plus")
	stub.setToken(http.StatusOK, `{"authorization_code":"auth","code_challenge":"c","code_verifier":"v"}`)
	stub.setExchange(http.StatusOK, `{"access_token":"`+vaConnectAccess+`","refresh_token":"`+vaConnectRefresh+`","id_token":"`+idToken+`","expires_in":3600}`)
	doneRec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "device/poll"), vaOwnerSecret, "")
	if doneRec.Code != http.StatusOK || !strings.Contains(doneRec.Body.String(), `"connected":true`) {
		t.Fatalf("authorized poll = %d %s, want 200 connected=true", doneRec.Code, doneRec.Body.String())
	}
	for _, secret := range []string{vaConnectAccess, vaConnectRefresh} {
		if strings.Contains(doneRec.Body.String(), secret) {
			t.Fatalf("poll response leaks a token: %s", doneRec.Body.String())
		}
	}

	// A plain GET now shows the subscription connected, still without a token.
	get := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, "")
	if get.Code != http.StatusOK || !vaDecode(t, get).SubscriptionConnected || strings.Contains(get.Body.String(), vaConnectAccess) {
		t.Fatalf("GET after connect = %d %s, want subscription_connected without a token", get.Code, get.Body.String())
	}
	row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	ts, err := vendorauth.OpenTokenSet(nil, row.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	if ts.AccountID != "acct-http" || ts.PlanType != "plus" {
		t.Fatalf("stored token set = %v, want the account facts from the exchange", ts)
	}
}

func TestVendorAccountDeviceConnectBeginRejectsNonOpenAI(t *testing.T) {
	srv, _, _ := newVendorDeviceConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "device/begin"), vaOwnerSecret, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("begin status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "vendor_account.device_not_supported" {
		t.Fatalf("code = %q, want vendor_account.device_not_supported", code)
	}
}

func TestVendorAccountDeviceConnectPollWithoutBegin(t *testing.T) {
	srv, _, _ := newVendorDeviceConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "device/poll"), vaOwnerSecret, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("poll status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if code := perfErrorCode(t, rec.Body.Bytes()); code != "vendor_account.device_connect_state" {
		t.Fatalf("code = %q, want vendor_account.device_connect_state", code)
	}
}

func TestVendorAccountDeviceConnectIsOwnerOnly(t *testing.T) {
	srv, _, _ := newVendorDeviceConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")

	for _, action := range []string{"device/begin", "device/poll"} {
		rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, action), vaOtherSecret, "")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s as another user = %d, want 404, body = %s", action, rec.Code, rec.Body.String())
		}
		if code := perfErrorCode(t, rec.Body.Bytes()); code != portal.CodeVendorAccountNotFound {
			t.Fatalf("%s code = %q, want %q", action, code, portal.CodeVendorAccountNotFound)
		}
	}
}

func TestVendorAccountDeviceConnectRejectsNonPost(t *testing.T) {
	srv, _, _ := newVendorDeviceConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")

	for _, action := range []string{"device/begin", "device/poll"} {
		rec := vaDo(t, srv, http.MethodGet, vaConnectPath(acc.ID, action), vaOwnerSecret, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405, body = %s", action, rec.Code, rec.Body.String())
		}
	}
}

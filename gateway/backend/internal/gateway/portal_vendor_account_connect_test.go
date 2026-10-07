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
	"net/url"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/vendorauth"
	"strings"
	"sync"
	"testing"
)

const (
	vaConnectAccess  = "sk-ant-oat01-http-access-do-not-echo"
	vaConnectRefresh = "sk-ant-ort01-http-refresh-do-not-echo"
)

// vaVendorStub is an httptest vendor token endpoint: it records the exchange
// bodies (JSON or form, flattened) and answers with the configured status/body.
type vaVendorStub struct {
	srv *httptest.Server

	mu     sync.Mutex
	status int
	body   string
	calls  []map[string]string
}

func newVAVendorStub(t *testing.T) *vaVendorStub {
	t.Helper()
	stub := &vaVendorStub{
		status: http.StatusOK,
		body:   `{"access_token":"` + vaConnectAccess + `","refresh_token":"` + vaConnectRefresh + `","expires_in":3600}`,
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
		status, body := stub.status, stub.body
		stub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(stub.srv.Close)
	return stub
}

func (s *vaVendorStub) respond(status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.body = status, body
}

func (s *vaVendorStub) requests() []map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]string(nil), s.calls...)
}

// newVendorConnectTestServer is newVendorAccountTestServer (flag ON, volatile
// RAM-mode sealing) with BOTH vendors' OAuth endpoints pointed at the stub.
func newVendorConnectTestServer(t *testing.T) (*Server, *routing.MemoryStore, *vaVendorStub) {
	t.Helper()
	stub := newVAVendorStub(t)
	srv, routeStore, _ := newVendorAccountSettingsTestServerWithDeps(t, true, func(deps *portal.ServiceDeps) {
		anthropic := vendorauth.DefaultAnthropicEndpoints()
		anthropic.AuthorizeURL = "https://authorize.anthropic.test/oauth/authorize"
		anthropic.TokenURL = stub.srv.URL + "/v1/oauth/token"
		deps.VendorAnthropicEndpoints = anthropic
		openAI := vendorauth.DefaultOpenAIEndpoints()
		openAI.AuthorizeURL = "https://authorize.openai.test/oauth/authorize"
		openAI.TokenURL = stub.srv.URL + "/oauth/token"
		deps.VendorOpenAIEndpoints = openAI
	})
	enableVendorAccountsFlag(t, srv)
	return srv, routeStore, stub
}

func vaCreateSubscription(t *testing.T, srv *Server, secret, vendor, name string) portal.VendorAccountDTO {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPost, "/api/portal/vendor-accounts", secret, `{"vendor":"`+vendor+`","auth_type":"subscription","name":"`+name+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create subscription status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	return vaDecode(t, rec)
}

func vaConnectPath(id, action string) string {
	return "/api/portal/vendor-accounts/" + id + "/connect/" + action
}

func vaBegin(t *testing.T, srv *Server, secret, id string) (authorizeURL string, state string) {
	t.Helper()
	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(id, "begin"), secret, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("begin status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal begin: %v (%s)", err, rec.Body.String())
	}
	u, err := url.Parse(body["authorize_url"])
	if err != nil || body["authorize_url"] == "" {
		t.Fatalf("authorize_url = %q (%v), want a URL", body["authorize_url"], err)
	}
	return body["authorize_url"], u.Query().Get("state")
}

func TestVendorAccountConnectImportEndpoint(t *testing.T) {
	srv, routeStore, _ := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret,
		`{"access_token":" `+vaConnectAccess+` ","refresh_token":"`+vaConnectRefresh+`","expires_at":"2026-10-08T09:30:00Z"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{vaConnectAccess, vaConnectRefresh} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("import response leaks a token: %s", rec.Body.String())
		}
	}
	dto := vaDecode(t, rec)
	if !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive || dto.AuthType != routing.VendorAuthSubscription || dto.ID != acc.ID {
		t.Fatalf("dto = %+v, want the connected active subscription account", dto)
	}

	row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	ts, err := vendorauth.OpenTokenSet(nil, row.OAuthTokens)
	if err != nil {
		t.Fatalf("OpenTokenSet: %v", err)
	}
	if ts.AccessToken != vaConnectAccess || ts.RefreshToken != vaConnectRefresh || ts.ExpiresAt.IsZero() {
		t.Fatalf("stored token set = %v, want the imported tokens with their expiry", ts)
	}
	if strings.Contains(row.OAuthTokens, vaConnectAccess) && !strings.HasPrefix(row.OAuthTokens, "plain:") {
		t.Fatalf("stored blob %q is not a sealed envelope", row.OAuthTokens)
	}

	// The subscription flag now shows on a plain GET, still without any token.
	get := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, "")
	if get.Code != http.StatusOK || !vaDecode(t, get).SubscriptionConnected || strings.Contains(get.Body.String(), vaConnectAccess) {
		t.Fatalf("GET after import = %d %s, want subscription_connected without a token", get.Code, get.Body.String())
	}
}

func TestVendorAccountConnectImportEndpointOptionalFields(t *testing.T) {
	srv, _, _ := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret, `{"access_token":"`+vaConnectAccess+`"}`)
	if rec.Code != http.StatusOK || !vaDecode(t, rec).SubscriptionConnected {
		t.Fatalf("import with only an access token = %d %s, want 200 connected", rec.Code, rec.Body.String())
	}
}

func TestVendorAccountConnectImportEndpointValidation(t *testing.T) {
	srv, routeStore, _ := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")
	keyAcc := vaCreate(t, srv, vaOwnerSecret, "Key account")

	for _, tc := range []struct {
		name       string
		id         string
		body       string
		wantStatus int
		wantCode   string
	}{
		{"missing access token", acc.ID, `{"refresh_token":"r"}`, http.StatusBadRequest, "vendor_account.connect_token_required"},
		{"blank access token", acc.ID, `{"access_token":"  "}`, http.StatusBadRequest, "vendor_account.connect_token_required"},
		{"api_key account", keyAcc.ID, `{"access_token":"` + vaConnectAccess + `"}`, http.StatusBadRequest, "vendor_account.not_subscription"},
		{"malformed expires_at", acc.ID, `{"access_token":"` + vaConnectAccess + `","expires_at":"tomorrow"}`, http.StatusBadRequest, "request.invalid_json"},
		{"not json", acc.ID, `{"access_token":`, http.StatusBadRequest, "request.invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := vaDo(t, srv, http.MethodPost, vaConnectPath(tc.id, "import"), vaOwnerSecret, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if strings.Contains(rec.Body.String(), vaConnectAccess) {
				t.Fatalf("error body echoes the token: %s", rec.Body.String())
			}
		})
	}
	for _, id := range []string{acc.ID, keyAcc.ID} {
		if row, _ := routeStore.VendorAccountByID(context.Background(), id); row.OAuthTokens != "" {
			t.Fatalf("account %s stored tokens after refused imports: %q", id, row.OAuthTokens)
		}
	}
}

func TestVendorAccountConnectBeginAndCompleteAnthropic(t *testing.T) {
	srv, routeStore, stub := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	authorizeURL, state := vaBegin(t, srv, vaOwnerSecret, acc.ID)
	u, _ := url.Parse(authorizeURL)
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://authorize.anthropic.test/oauth/authorize" {
		t.Fatalf("authorize url base = %q, want the injected endpoint", got)
	}
	q := u.Query()
	if q.Get("client_id") != vendorauth.AnthropicClientID || q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || state == "" || q.Get("code") != "true" {
		t.Fatalf("authorize query = %v, want client id, S256 challenge, state and code=true", q)
	}

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, `{"code":"  pasted-code#`+state+`\n"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("complete status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	for _, secret := range []string{vaConnectAccess, vaConnectRefresh} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("complete response leaks a token: %s", rec.Body.String())
		}
	}
	if dto := vaDecode(t, rec); !dto.SubscriptionConnected || dto.Status != routing.VendorAccountStatusActive {
		t.Fatalf("dto = %+v, want connected and active", dto)
	}

	calls := stub.requests()
	if len(calls) != 1 || calls[0]["code"] != "pasted-code" || calls[0]["state"] != state || calls[0]["code_verifier"] == "" || calls[0]["grant_type"] != "authorization_code" {
		t.Fatalf("exchange = %v, want the pasted code, the issued state and a verifier", calls)
	}
	if strings.Contains(authorizeURL, calls[0]["code_verifier"]) {
		t.Fatal("the authorize URL must not carry the PKCE verifier")
	}
	row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID)
	ts, err := vendorauth.OpenTokenSet(nil, row.OAuthTokens)
	if err != nil || ts.AccessToken != vaConnectAccess || ts.RefreshToken != vaConnectRefresh {
		t.Fatalf("stored token set = %v, %v, want the exchanged tokens", ts, err)
	}

	// The pending entry is single-use: completing again needs a new begin.
	again := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, `{"code":"pasted-code"}`)
	if again.Code != http.StatusBadRequest || perfErrorCode(t, again.Body.Bytes()) != "vendor_account.connect_state" {
		t.Fatalf("second complete = %d %s, want 400 vendor_account.connect_state", again.Code, again.Body.String())
	}
}

func TestVendorAccountConnectBeginAndCompleteOpenAI(t *testing.T) {
	srv, _, stub := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")

	authorizeURL, state := vaBegin(t, srv, vaOwnerSecret, acc.ID)
	u, _ := url.Parse(authorizeURL)
	if got := u.Scheme + "://" + u.Host + u.Path; got != "https://authorize.openai.test/oauth/authorize" {
		t.Fatalf("authorize url base = %q, want the injected endpoint", got)
	}
	if u.Query().Get(vendorauth.OpenAIParamOriginator) != vendorauth.OpenAIOriginator {
		t.Fatalf("authorize query = %v, want the codex originator", u.Query())
	}

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, `{"code":"openai-code"}`)
	if rec.Code != http.StatusOK || !vaDecode(t, rec).SubscriptionConnected {
		t.Fatalf("complete = %d %s, want 200 connected", rec.Code, rec.Body.String())
	}
	calls := stub.requests()
	if len(calls) != 1 || calls[0]["code"] != "openai-code" || calls[0]["code_verifier"] == "" {
		t.Fatalf("exchange = %v, want the pasted code and a verifier", calls)
	}
	if state == "" {
		t.Fatal("begin must issue a state")
	}
}

func TestVendorAccountConnectCompleteErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		stubStatus int
		stubBody   string
		pasteState string // "" = use the issued state
		body       string // overrides the generated body when set
		wantStatus int
		wantCode   string
	}{
		{name: "mismatched state", pasteState: "not-the-issued-state", wantStatus: http.StatusBadRequest, wantCode: "vendor_account.connect_state"},
		{name: "vendor rejects the code (401)", stubStatus: http.StatusUnauthorized, stubBody: `{"error":"invalid_grant"}`, wantStatus: http.StatusBadRequest, wantCode: "vendor_account.connect_rejected"},
		{name: "vendor rejects the code (invalid_grant)", stubStatus: http.StatusBadRequest, stubBody: `{"error":"invalid_grant"}`, wantStatus: http.StatusBadRequest, wantCode: "vendor_account.connect_rejected"},
		{name: "vendor unavailable", stubStatus: http.StatusBadGateway, stubBody: `oops`, wantStatus: http.StatusBadGateway, wantCode: "vendor_account.connect_upstream_failed"},
		{name: "unusable vendor reply", stubStatus: http.StatusOK, stubBody: `{}`, wantStatus: http.StatusBadGateway, wantCode: "vendor_account.connect_upstream_failed"},
		{name: "empty code", body: `{"code":"  "}`, wantStatus: http.StatusBadRequest, wantCode: "vendor_account.connect_code_required"},
		{name: "missing code field", body: `{}`, wantStatus: http.StatusBadRequest, wantCode: "vendor_account.connect_code_required"},
		{name: "invalid json", body: `{"code":`, wantStatus: http.StatusBadRequest, wantCode: "request.invalid_json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, routeStore, stub := newVendorConnectTestServer(t)
			acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")
			_, state := vaBegin(t, srv, vaOwnerSecret, acc.ID)
			if tc.stubStatus != 0 {
				stub.respond(tc.stubStatus, tc.stubBody)
			}
			body := tc.body
			if body == "" {
				paste := state
				if tc.pasteState != "" {
					paste = tc.pasteState
				}
				body = `{"code":"a-code#` + paste + `"}`
			}

			rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if code := perfErrorCode(t, rec.Body.Bytes()); code != tc.wantCode {
				t.Fatalf("code = %q, want %q", code, tc.wantCode)
			}
			if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
				t.Fatalf("tokens stored after a failed complete: %q", row.OAuthTokens)
			}
			// A vendor-error body is never relayed (it could echo request data).
			if strings.Contains(rec.Body.String(), "oops") {
				t.Fatalf("response relays the vendor body: %s", rec.Body.String())
			}
		})
	}
}

func TestVendorAccountConnectCompleteWithoutBegin(t *testing.T) {
	srv, _, stub := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, `{"code":"a-code"}`)
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.connect_state" {
		t.Fatalf("complete without begin = %d %s, want 400 vendor_account.connect_state", rec.Code, rec.Body.String())
	}
	if len(stub.requests()) != 0 {
		t.Fatal("complete without begin must not reach the vendor")
	}
}

func TestVendorAccountConnectEndpointsRefuseAnAPIKeyAccount(t *testing.T) {
	srv, _, _ := newVendorConnectTestServer(t)
	keyAcc := vaCreate(t, srv, vaOwnerSecret, "Key account")
	for action, body := range map[string]string{"begin": "", "complete": `{"code":"x"}`} {
		rec := vaDo(t, srv, http.MethodPost, vaConnectPath(keyAcc.ID, action), vaOwnerSecret, body)
		if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.not_subscription" {
			t.Fatalf("%s on an api_key account = %d %s, want 400 vendor_account.not_subscription", action, rec.Code, rec.Body.String())
		}
	}
}

// A user who does not own the account gets the same 404 as an unknown id on all
// three endpoints (including a system-scope principal: the write is owner-only),
// and an owner's pending connect is not consumable by a stranger.
func TestVendorAccountConnectEndpointsAreOwnerOnly(t *testing.T) {
	srv, routeStore, stub := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")
	_, state := vaBegin(t, srv, vaOwnerSecret, acc.ID)

	for _, secret := range []string{vaOtherSecret, vaSystemSecret} {
		for _, tc := range []struct{ action, body string }{
			{"import", `{"access_token":"` + vaConnectAccess + `"}`},
			{"begin", ""},
			{"complete", `{"code":"a-code#` + state + `"}`},
		} {
			rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, tc.action), secret, tc.body)
			if rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
				t.Fatalf("%s as %s = %d %s, want 404 %s", tc.action, secret, rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
			}
		}
	}
	if rec := vaDo(t, srv, http.MethodPost, vaConnectPath("va_missing", "begin"), vaOwnerSecret, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("begin on an unknown id = %d, want 404", rec.Code)
	}
	if len(stub.requests()) != 0 {
		t.Fatal("a refused principal must not reach the vendor")
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored by a refused principal: %q", row.OAuthTokens)
	}
	// The owner's pending entry survived the strangers' attempts.
	if rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "complete"), vaOwnerSecret, `{"code":"a-code#`+state+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("owner complete = %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestVendorAccountConnectEndpointsRouting(t *testing.T) {
	srv, _, _ := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	// Only POST is allowed on the three actions.
	for _, action := range []string{"import", "begin", "complete"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			rec := vaDo(t, srv, method, vaConnectPath(acc.ID, action), vaOwnerSecret, "")
			if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != http.MethodPost {
				t.Fatalf("%s %s = %d (Allow %q), want 405 Allow: POST", method, action, rec.Code, rec.Header().Get("Allow"))
			}
		}
	}
	// Unknown shapes under the item are the item's own 404.
	for _, path := range []string{
		"/api/portal/vendor-accounts/" + acc.ID + "/connect",
		"/api/portal/vendor-accounts/" + acc.ID + "/connect/",
		"/api/portal/vendor-accounts/" + acc.ID + "/connect/nope",
		"/api/portal/vendor-accounts/" + acc.ID + "/connect/import/extra",
		"/api/portal/vendor-accounts/" + acc.ID + "/other",
		"/api/portal/vendor-accounts/connect/begin",
	} {
		rec := vaDo(t, srv, http.MethodPost, path, vaOwnerSecret, `{}`)
		if rec.Code != http.StatusNotFound || perfErrorCode(t, rec.Body.Bytes()) != portal.CodeVendorAccountNotFound {
			t.Fatalf("POST %s = %d %s, want 404 %s", path, rec.Code, rec.Body.String(), portal.CodeVendorAccountNotFound)
		}
	}
	// The plain item routes still work.
	if rec := vaDo(t, srv, http.MethodGet, "/api/portal/vendor-accounts/"+acc.ID, vaOwnerSecret, ""); rec.Code != http.StatusOK {
		t.Fatalf("GET item = %d, want 200", rec.Code)
	}
	// Authentication comes first.
	if rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "begin"), "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous begin = %d, want 401", rec.Code)
	}
}

// A disk-backed store with no encryption key cannot seal a token set; the
// import and begin are a 400 about the subscription (not the api-key message) and persists
// nothing -- never plaintext.
func TestVendorAccountConnectImportEndpointOnAKeylessDiskStore(t *testing.T) {
	srv, routeStore := newVendorAccountTestServer(t, false)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "anthropic", "Claude Max")

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret, `{"access_token":"`+vaConnectAccess+`"}`)
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.connect_key_required" {
		t.Fatalf("import on a keyless disk store = %d %s, want 400 vendor_account.connect_key_required", rec.Code, rec.Body.String())
	}
	// The code-paste flow fails fast too, before the user signs in at the vendor.
	rec = vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "begin"), vaOwnerSecret, "")
	if rec.Code != http.StatusBadRequest || perfErrorCode(t, rec.Body.Bytes()) != "vendor_account.connect_key_required" {
		t.Fatalf("begin on a keyless disk store = %d %s, want 400 vendor_account.connect_key_required", rec.Code, rec.Body.String())
	}
	if row, _ := routeStore.VendorAccountByID(context.Background(), acc.ID); row.OAuthTokens != "" {
		t.Fatalf("tokens stored without a key: %q", row.OAuthTokens)
	}
}

// An imported OpenAI access token is a JWT; its ChatGPT account facts land in the
// stored token set (the dispatch needs the account id), never in the response.
func TestVendorAccountConnectImportEndpointReadsTheOpenAIAccountClaims(t *testing.T) {
	srv, routeStore, _ := newVendorConnectTestServer(t)
	acc := vaCreateSubscription(t, srv, vaOwnerSecret, "openai", "ChatGPT Plus")
	claims, err := json.Marshal(map[string]any{
		vendorauth.OpenAIAuthClaimNamespace: map[string]any{vendorauth.OpenAIClaimAccountID: "acct-http-1", vendorauth.OpenAIClaimPlanType: "plus"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jwt := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".sig"

	rec := vaDo(t, srv, http.MethodPost, vaConnectPath(acc.ID, "import"), vaOwnerSecret, `{"access_token":"`+jwt+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("import status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "acct-http-1") || strings.Contains(rec.Body.String(), jwt) {
		t.Fatalf("import response leaks token material: %s", rec.Body.String())
	}
	row, err := routeStore.VendorAccountByID(context.Background(), acc.ID)
	if err != nil {
		t.Fatalf("VendorAccountByID: %v", err)
	}
	ts, err := vendorauth.OpenTokenSet(nil, row.OAuthTokens)
	if err != nil || ts.AccountID != "acct-http-1" || ts.PlanType != "plus" {
		t.Fatalf("stored token set = %v, %v, want account_id acct-http-1 and plan plus", ts, err)
	}
}

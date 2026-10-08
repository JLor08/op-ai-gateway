// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deviceStub is an httptest OpenAI deviceauth host. It routes the three device
// paths (usercode, token, oauth/token exchange) to one handler per path and
// records the decoded request body of each call, so a test can assert the body
// without the handler repeating the decode. The default endpoints are re-pointed
// at it.
type deviceStub struct {
	srv *httptest.Server

	usercode func(w http.ResponseWriter, body map[string]any)
	token    func(w http.ResponseWriter, body map[string]any)
	exchange func(w http.ResponseWriter, form map[string]string)

	lastUsercode map[string]any
	lastToken    map[string]any
	lastExchange map[string]string
}

func newDeviceStub(t *testing.T) *deviceStub {
	t.Helper()
	ds := &deviceStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		ds.lastUsercode = decodeJSONBody(t, r)
		if ds.usercode != nil {
			ds.usercode(w, ds.lastUsercode)
		}
	})
	mux.HandleFunc("/api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		ds.lastToken = decodeJSONBody(t, r)
		if ds.token != nil {
			ds.token(w, ds.lastToken)
		}
	})
	mux.HandleFunc("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		form := map[string]string{}
		for k := range r.PostForm {
			form[k] = r.PostForm.Get(k)
		}
		ds.lastExchange = form
		if ds.exchange != nil {
			ds.exchange(w, form)
		}
	})
	ds.srv = httptest.NewServer(mux)
	t.Cleanup(ds.srv.Close)
	return ds
}

func decodeJSONBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	if got := r.Header.Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	raw, _ := io.ReadAll(r.Body)
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("decode body %q: %v", raw, err)
		}
	}
	return out
}

func (ds *deviceStub) endpoints() Endpoints {
	ep := DefaultOpenAIEndpoints()
	ep.DeviceUsercodeURL = ds.srv.URL + "/api/accounts/deviceauth/usercode"
	ep.DeviceTokenURL = ds.srv.URL + "/api/accounts/deviceauth/token"
	ep.TokenURL = ds.srv.URL + "/oauth/token"
	// DeviceVerificationURL and DeviceCallbackRedirect keep their auth.openai.com
	// defaults: they are a display string and an exchange parameter, never fetched.
	return ep
}

// --- start ----------------------------------------------------------------

func TestOpenAIDeviceStart(t *testing.T) {
	ds := newDeviceStub(t)
	ds.usercode = func(w http.ResponseWriter, _ map[string]any) {
		// interval as a STRING, the shape the Codex CLI deserializes.
		writeJSON(w, 200, `{"device_auth_id":"dev-1","user_code":"ABCD-1234","interval":"5"}`)
	}
	deviceAuthID, userCode, verificationURL, interval, err := OpenAIDeviceStart(context.Background(), http.DefaultClient, ds.endpoints())
	if err != nil {
		t.Fatalf("OpenAIDeviceStart: %v", err)
	}
	if deviceAuthID != "dev-1" || userCode != "ABCD-1234" {
		t.Fatalf("got (%q, %q), want (dev-1, ABCD-1234)", deviceAuthID, userCode)
	}
	if interval != 5*time.Second {
		t.Fatalf("interval = %v, want 5s", interval)
	}
	if verificationURL != OpenAIDeviceVerificationURL {
		t.Fatalf("verificationURL = %q, want %q", verificationURL, OpenAIDeviceVerificationURL)
	}
	if ds.lastUsercode["client_id"] != OpenAIClientID {
		t.Fatalf("usercode body = %v, want client_id %q", ds.lastUsercode, OpenAIClientID)
	}
	if len(ds.lastUsercode) != 1 {
		t.Fatalf("usercode body = %v, want only client_id", ds.lastUsercode)
	}
}

func TestOpenAIDeviceStartNumericInterval(t *testing.T) {
	ds := newDeviceStub(t)
	ds.usercode = func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, 200, `{"device_auth_id":"dev-1","user_code":"CODE","interval":7}`)
	}
	_, _, _, interval, err := OpenAIDeviceStart(context.Background(), http.DefaultClient, ds.endpoints())
	if err != nil {
		t.Fatalf("OpenAIDeviceStart: %v", err)
	}
	if interval != 7*time.Second {
		t.Fatalf("interval = %v, want 7s (a numeric interval must parse too)", interval)
	}
}

func TestOpenAIDeviceStartUserCodeAliasAndAbsentInterval(t *testing.T) {
	ds := newDeviceStub(t)
	ds.usercode = func(w http.ResponseWriter, _ map[string]any) {
		// "usercode" alias, and no interval at all (the CLI defaults it to 0).
		writeJSON(w, 200, `{"device_auth_id":"dev-2","usercode":"ALT-CODE"}`)
	}
	deviceAuthID, userCode, _, interval, err := OpenAIDeviceStart(context.Background(), http.DefaultClient, ds.endpoints())
	if err != nil {
		t.Fatalf("OpenAIDeviceStart: %v", err)
	}
	if deviceAuthID != "dev-2" || userCode != "ALT-CODE" {
		t.Fatalf("got (%q, %q), want (dev-2, ALT-CODE)", deviceAuthID, userCode)
	}
	if interval != 0 {
		t.Fatalf("interval = %v, want 0 when absent", interval)
	}
}

func TestOpenAIDeviceStartMissingFieldsIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no device_auth_id": `{"user_code":"X"}`,
		"no user_code":      `{"device_auth_id":"dev"}`,
		"not json":          `not json`,
		"empty object":      `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			ds := newDeviceStub(t)
			ds.usercode = func(w http.ResponseWriter, _ map[string]any) { writeJSON(w, 200, body) }
			_, _, _, _, err := OpenAIDeviceStart(context.Background(), http.DefaultClient, ds.endpoints())
			if !errors.Is(err, ErrBadTokenResponse) {
				t.Fatalf("err = %v, want ErrBadTokenResponse", err)
			}
		})
	}
}

func TestOpenAIDeviceStartNon2xxIsAStatusError(t *testing.T) {
	ds := newDeviceStub(t)
	ds.usercode = func(w http.ResponseWriter, _ map[string]any) { writeJSON(w, 500, `{"error":"server_error"}`) }
	_, _, _, _, err := OpenAIDeviceStart(context.Background(), http.DefaultClient, ds.endpoints())
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 {
		t.Fatalf("err = %v, want a *StatusError for HTTP 500", err)
	}
}

// --- poll -----------------------------------------------------------------

func TestOpenAIDevicePollPending(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		ds := newDeviceStub(t)
		ds.token = func(w http.ResponseWriter, _ map[string]any) {
			writeJSON(w, status, `{"error":"authorization_pending"}`)
		}
		code, verifier, pending, err := OpenAIDevicePoll(context.Background(), http.DefaultClient, ds.endpoints(), "dev-1", "ABCD")
		if err != nil {
			t.Fatalf("status %d: unexpected error %v", status, err)
		}
		if !pending || code != "" || verifier != "" {
			t.Fatalf("status %d: got pending=%v code=%q verifier=%q, want pending with no code", status, pending, code, verifier)
		}
		if ds.lastToken["device_auth_id"] != "dev-1" || ds.lastToken["user_code"] != "ABCD" {
			t.Fatalf("status %d: token body = %v, want device_auth_id/user_code", status, ds.lastToken)
		}
	}
}

func TestOpenAIDevicePollAuthorized(t *testing.T) {
	ds := newDeviceStub(t)
	ds.token = func(w http.ResponseWriter, _ map[string]any) {
		writeJSON(w, 200, `{"authorization_code":"auth-code","code_challenge":"chal","code_verifier":"ver"}`)
	}
	code, verifier, pending, err := OpenAIDevicePoll(context.Background(), http.DefaultClient, ds.endpoints(), "dev-1", "ABCD")
	if err != nil {
		t.Fatalf("OpenAIDevicePoll: %v", err)
	}
	if pending {
		t.Fatal("a 2xx answer must not report pending")
	}
	if code != "auth-code" || verifier != "ver" {
		t.Fatalf("got (%q, %q), want (auth-code, ver)", code, verifier)
	}
}

func TestOpenAIDevicePollMissingFieldsIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"no authorization_code": `{"code_verifier":"ver"}`,
		"no code_verifier":      `{"authorization_code":"auth-code"}`,
		"not json":              `garbled`,
	} {
		t.Run(name, func(t *testing.T) {
			ds := newDeviceStub(t)
			ds.token = func(w http.ResponseWriter, _ map[string]any) { writeJSON(w, 200, body) }
			_, _, pending, err := OpenAIDevicePoll(context.Background(), http.DefaultClient, ds.endpoints(), "d", "c")
			if pending {
				t.Fatal("a malformed 2xx body must not report pending")
			}
			if !errors.Is(err, ErrBadTokenResponse) {
				t.Fatalf("err = %v, want ErrBadTokenResponse", err)
			}
		})
	}
}

func TestOpenAIDevicePollTerminalError(t *testing.T) {
	cases := map[string]struct {
		status       int
		wantRejected bool
	}{
		"500 upstream":  {http.StatusInternalServerError, false},
		"429 upstream":  {http.StatusTooManyRequests, false},
		"401 rejected":  {http.StatusUnauthorized, true},
		"400 bad input": {http.StatusBadRequest, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ds := newDeviceStub(t)
			ds.token = func(w http.ResponseWriter, _ map[string]any) { writeJSON(w, tc.status, `{"error":"nope"}`) }
			_, _, pending, err := OpenAIDevicePoll(context.Background(), http.DefaultClient, ds.endpoints(), "dev-1", "SECRET-USER-CODE")
			if pending {
				t.Fatalf("status %d must be terminal, not pending", tc.status)
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Fatalf("err = %v, want a *StatusError for HTTP %d", err, tc.status)
			}
			if got := errors.Is(err, ErrAuthRejected); got != tc.wantRejected {
				t.Fatalf("errors.Is(ErrAuthRejected) = %v, want %v", got, tc.wantRejected)
			}
			if strings.Contains(err.Error(), "SECRET-USER-CODE") {
				t.Fatalf("error leaks the user code: %v", err)
			}
		})
	}
}

// --- exchange -------------------------------------------------------------

func TestExchangeOpenAIDeviceCode(t *testing.T) {
	idToken := stubJWT(t, openAIClaims("acct-dev", "pro"))
	ds := newDeviceStub(t)
	ds.exchange = func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 200, `{"access_token":"at","refresh_token":"rt","id_token":"`+idToken+`","expires_in":3600,"scope":"openid"}`)
	}
	ts, err := ExchangeOpenAIDeviceCode(context.Background(), http.DefaultClient, ds.endpoints(), " auth-code ", "the-verifier")
	if err != nil {
		t.Fatalf("ExchangeOpenAIDeviceCode: %v", err)
	}
	if ts.AccessToken != "at" || ts.RefreshToken != "rt" || ts.AccountID != "acct-dev" || ts.PlanType != "pro" || ts.ExpiresAt.IsZero() {
		t.Fatalf("TokenSet = %+v, want the exchanged tokens with the account facts", ts)
	}
	// The device exchange uses the deviceauth redirect, never the loopback one.
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "auth-code",
		"redirect_uri":  OpenAIDeviceCallbackRedirect,
		"client_id":     OpenAIClientID,
		"code_verifier": "the-verifier",
	}
	for k, v := range want {
		if ds.lastExchange[k] != v {
			t.Errorf("exchange form[%s] = %q, want %q", k, ds.lastExchange[k], v)
		}
	}
	if len(ds.lastExchange) != len(want) {
		t.Errorf("exchange form = %v, want exactly %v", ds.lastExchange, want)
	}
}

func TestExchangeOpenAIDeviceCodeErrorCarriesNoSecret(t *testing.T) {
	ds := newDeviceStub(t)
	ds.exchange = func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 400, `{"error":"invalid_grant","error_description":"bad"}`)
	}
	_, err := ExchangeOpenAIDeviceCode(context.Background(), http.DefaultClient, ds.endpoints(), "SECRET-DEVICE-CODE", "SECRET-VERIFIER")
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected for invalid_grant", err)
	}
	for _, secret := range []string{"SECRET-DEVICE-CODE", "SECRET-VERIFIER"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks a secret %q: %v", secret, err)
		}
	}
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// stubJWT builds an unsigned three-segment JWT carrying claims, the shape the
// OpenAI token endpoint returns for id_token (the library never verifies it).
func stubJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + "."
}

func openAIClaims(accountID, plan string) map[string]any {
	return map[string]any{
		"sub":                    "user-1",
		OpenAIAuthClaimNamespace: map[string]any{OpenAIClaimAccountID: accountID, OpenAIClaimPlanType: plan},
	}
}

// openAIStub starts an httptest server for both the token and the device
// endpoints and returns the default OpenAI endpoints re-pointed at it. handler
// receives the request path and the parsed form body.
func openAIStub(t *testing.T, handler func(w http.ResponseWriter, path string, form url.Values)) Endpoints {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		handler(w, r.URL.Path, r.PostForm)
	}))
	t.Cleanup(srv.Close)
	ep := DefaultOpenAIEndpoints()
	ep.TokenURL = srv.URL + "/oauth/token"
	ep.DeviceAuthorizeURL = srv.URL + "/oauth/device/code"
	return ep
}

func expectForm(t *testing.T, got url.Values, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("form = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("form[%s] = %q, want %q", k, got.Get(k), v)
		}
	}
}

func TestBuildOpenAIAuthorizeURL(t *testing.T) {
	ep := DefaultOpenAIEndpoints()
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	raw := BuildOpenAIAuthorizeURL(ep, verifier, "state-xyz")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL does not parse: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != ep.AuthorizeURL {
		t.Fatalf("base = %q, want %q", got, ep.AuthorizeURL)
	}
	want := map[string]string{
		"response_type":              "code",
		"client_id":                  OpenAIClientID,
		"redirect_uri":               OpenAIRedirectURI,
		"scope":                      OpenAIScopes,
		"code_challenge":             challenge,
		"code_challenge_method":      "S256",
		"state":                      "state-xyz",
		"id_token_add_organizations": "true",
		"codex_cli_simplified_flow":  "true",
		"originator":                 "codex_cli_rs",
	}
	q := u.Query()
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("query %s = %q, want %q", k, got, v)
		}
	}
	if len(q) != len(want) {
		t.Errorf("query has %d params, want exactly %d: %v", len(q), len(want), q)
	}
	if strings.Contains(raw, verifier) {
		t.Errorf("authorize URL must carry the challenge, never the verifier")
	}
}

func TestBuildOpenAIAuthorizeURLKeepsCodexParamsOnHandBuiltEndpoints(t *testing.T) {
	ep := Endpoints{ClientID: "cid", AuthorizeURL: "http://127.0.0.1:9/authorize", RedirectURI: "http://127.0.0.1:9/cb", Scopes: "openid"}
	q := mustQuery(t, BuildOpenAIAuthorizeURL(ep, "v", "s"))
	for _, k := range []string{"id_token_add_organizations", "codex_cli_simplified_flow", "originator"} {
		if q.Get(k) == "" {
			t.Errorf("hand-built endpoints lost the %s parameter", k)
		}
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u.Query()
}

func TestExchangeOpenAICode(t *testing.T) {
	idToken := stubJWT(t, openAIClaims("acct-42", "plus"))
	ep := openAIStub(t, func(w http.ResponseWriter, path string, form url.Values) {
		if path != "/oauth/token" {
			t.Errorf("path = %s, want the token endpoint", path)
		}
		expectForm(t, form, map[string]string{
			"grant_type":    "authorization_code",
			"code":          "the-code",
			"redirect_uri":  OpenAIRedirectURI,
			"client_id":     OpenAIClientID,
			"code_verifier": "the-verifier",
		})
		writeJSON(w, 200, `{"access_token":"at","refresh_token":"rt","id_token":"`+idToken+`","expires_in":3600,"scope":"openid profile"}`)
	})
	before := time.Now()
	ts, err := ExchangeOpenAICode(context.Background(), http.DefaultClient, ep, "the-code", "the-verifier")
	after := time.Now()
	if err != nil {
		t.Fatalf("ExchangeOpenAICode: %v", err)
	}
	if ts.AccessToken != "at" || ts.RefreshToken != "rt" || ts.AccountID != "acct-42" || ts.PlanType != "plus" || ts.Scope != "openid profile" {
		t.Fatalf("TokenSet = %+v", ts)
	}
	if ts.ExpiresAt.Before(before.Add(time.Hour)) || ts.ExpiresAt.After(after.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %v, want now+1h", ts.ExpiresAt)
	}
}

func TestExchangeOpenAICodeFallsBackToAccessTokenClaims(t *testing.T) {
	// No id_token claim, but the access token is itself a JWT with the
	// https://api.openai.com/auth object.
	accessJWT := stubJWT(t, openAIClaims("acct-from-access", "pro"))
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		writeJSON(w, 200, `{"access_token":"`+accessJWT+`","refresh_token":"rt","expires_in":"3600"}`)
	})
	ts, err := ExchangeOpenAICode(context.Background(), http.DefaultClient, ep, "c", "v")
	if err != nil {
		t.Fatalf("ExchangeOpenAICode: %v", err)
	}
	if ts.AccountID != "acct-from-access" || ts.PlanType != "pro" {
		t.Fatalf("TokenSet = %+v, want claims recovered from the access token", ts)
	}
	if ts.ExpiresAt.IsZero() {
		t.Fatalf("a numeric-string expires_in must still set ExpiresAt")
	}
}

func TestExchangeOpenAICodeWithoutClaimsLeavesIdentityEmpty(t *testing.T) {
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		writeJSON(w, 200, `{"access_token":"opaque","id_token":"garbled","expires_in":60}`)
	})
	ts, err := ExchangeOpenAICode(context.Background(), http.DefaultClient, ep, "c", "v")
	if err != nil {
		t.Fatalf("a token response without claims must still succeed: %v", err)
	}
	if ts.AccessToken != "opaque" || ts.AccountID != "" || ts.PlanType != "" {
		t.Fatalf("TokenSet = %+v", ts)
	}
}

func TestParseOpenAIIDTokenClaims(t *testing.T) {
	enc := base64.RawURLEncoding.EncodeToString
	jwtOf := func(payload string) string { return "h." + enc([]byte(payload)) + ".s" }
	cases := []struct {
		name        string
		token       string
		wantAccount string
		wantPlan    string
	}{
		{"valid", stubJWT(t, openAIClaims("a1", "plus")), "a1", "plus"},
		{"padded base64 segment", "h." + base64.URLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"a2"}}`)) + ".s", "a2", ""},
		{"only plan", jwtOf(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"team"}}`), "", "team"},
		{"empty string", "", "", ""},
		{"not a jwt", "garbled", "", ""},
		{"two segments", "a.b", "", ""},
		{"four segments", "a.b.c.d", "", ""},
		{"payload not base64", "h.!!!notbase64!!!.s", "", ""},
		{"payload not json", jwtOf(`not json`), "", ""},
		{"payload json null", jwtOf(`null`), "", ""},
		{"payload json array", jwtOf(`[1,2]`), "", ""},
		{"namespace missing", jwtOf(`{"sub":"x"}`), "", ""},
		{"namespace is a string", jwtOf(`{"https://api.openai.com/auth":"nope"}`), "", ""},
		{"namespace is null", jwtOf(`{"https://api.openai.com/auth":null}`), "", ""},
		{"claims of the wrong type", jwtOf(`{"https://api.openai.com/auth":{"chatgpt_account_id":7,"chatgpt_plan_type":["x"]}}`), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			account, plan := parseOpenAIIDTokenClaims(tc.token)
			if account != tc.wantAccount || plan != tc.wantPlan {
				t.Fatalf("got (%q, %q), want (%q, %q)", account, plan, tc.wantAccount, tc.wantPlan)
			}
		})
	}
}

func TestRefreshOpenAI(t *testing.T) {
	t.Run("re-issues tokens and identity", func(t *testing.T) {
		idToken := stubJWT(t, openAIClaims("acct-9", "pro"))
		ep := openAIStub(t, func(w http.ResponseWriter, _ string, form url.Values) {
			expectForm(t, form, map[string]string{"grant_type": "refresh_token", "refresh_token": "old-r", "client_id": OpenAIClientID})
			writeJSON(w, 200, `{"access_token":"new-a","refresh_token":"new-r","id_token":"`+idToken+`","expires_in":3600}`)
		})
		ts, err := RefreshOpenAI(context.Background(), http.DefaultClient, ep, "old-r")
		if err != nil {
			t.Fatalf("RefreshOpenAI: %v", err)
		}
		if ts.AccessToken != "new-a" || ts.RefreshToken != "new-r" || ts.AccountID != "acct-9" || ts.PlanType != "pro" || ts.ExpiresAt.IsZero() {
			t.Fatalf("TokenSet = %+v", ts)
		}
	})
	t.Run("response without refresh token or id_token keeps the old refresh token", func(t *testing.T) {
		ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
			writeJSON(w, 200, `{"access_token":"new-a","expires_in":3600}`)
		})
		ts, err := RefreshOpenAI(context.Background(), http.DefaultClient, ep, "keep-me")
		if err != nil {
			t.Fatalf("RefreshOpenAI: %v", err)
		}
		if ts.RefreshToken != "keep-me" || ts.AccountID != "" {
			t.Fatalf("TokenSet = %+v; identity must be left for the caller to carry forward", ts)
		}
	})
}

func TestOpenAIErrorMapping(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantRejected bool
	}{
		{"401", 401, `{"error":{"message":"bad","type":"invalid_request_error","code":"token_expired"}}`, true},
		{"403", 403, `forbidden`, true},
		{"400 invalid_grant", 400, `{"error":"invalid_grant","error_description":"refresh token reused"}`, true},
		{"400 invalid_request", 400, `{"error":"invalid_request"}`, false},
		{"429", 429, `{"error":"rate_limit"}`, false},
		{"503", 503, `unavailable`, false},
	}
	calls := map[string]func(ep Endpoints) error{
		"exchange": func(ep Endpoints) error {
			_, err := ExchangeOpenAICode(context.Background(), http.DefaultClient, ep, "SECRET-CODE", "v")
			return err
		},
		"refresh": func(ep Endpoints) error {
			_, err := RefreshOpenAI(context.Background(), http.DefaultClient, ep, "SECRET-REFRESH")
			return err
		},
	}
	for _, tc := range cases {
		for name, call := range calls {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { writeJSON(w, tc.status, tc.body) })
				err := call(ep)
				if err == nil {
					t.Fatal("want an error")
				}
				if got := errors.Is(err, ErrAuthRejected); got != tc.wantRejected {
					t.Fatalf("errors.Is(ErrAuthRejected) = %v, want %v (err: %v)", got, tc.wantRejected, err)
				}
				var se *StatusError
				if !errors.As(err, &se) || se.Status != tc.status || !strings.Contains(err.Error(), "HTTP") {
					t.Fatalf("error is not a *StatusError for HTTP %d: %v", tc.status, err)
				}
				for _, secret := range []string{"SECRET-CODE", "SECRET-REFRESH"} {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("error leaks request secret %q: %v", secret, err)
					}
				}
			})
		}
	}
}

func TestOpenAIDeviceAuthorize(t *testing.T) {
	ep := openAIStub(t, func(w http.ResponseWriter, path string, form url.Values) {
		if path != "/oauth/device/code" {
			t.Errorf("path = %s, want the device-authorize endpoint", path)
		}
		expectForm(t, form, map[string]string{"client_id": OpenAIClientID, "scope": OpenAIScopes})
		writeJSON(w, 200, `{"device_code":"dev-1","user_code":"ABCD-EFGH","verification_uri":"https://auth.example/activate","interval":7,"expires_in":600}`)
	})
	device, user, uri, interval, expiresIn, err := OpenAIDeviceAuthorize(context.Background(), http.DefaultClient, ep)
	if err != nil {
		t.Fatalf("OpenAIDeviceAuthorize: %v", err)
	}
	if device != "dev-1" || user != "ABCD-EFGH" || uri != "https://auth.example/activate" || interval != 7 || expiresIn != 600 {
		t.Fatalf("got (%q, %q, %q, %d, %d)", device, user, uri, interval, expiresIn)
	}
}

func TestOpenAIDeviceAuthorizeTolerance(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantURI      string
		wantInterval int
		wantExpires  int
	}{
		{"missing interval and expiry get RFC defaults", `{"device_code":"d","user_code":"u","verification_uri":"https://v"}`, "https://v", 5, 900},
		{"verification_url spelling", `{"device_code":"d","user_code":"u","verification_url":"https://v2","interval":3,"expires_in":60}`, "https://v2", 3, 60},
		{"verification_uri_complete when nothing else", `{"device_code":"d","user_code":"u","verification_uri_complete":"https://v3?c=u","interval":"4","expires_in":"90"}`, "https://v3?c=u", 4, 90},
		{"verification_uri wins over complete", `{"device_code":"d","user_code":"u","verification_uri":"https://v","verification_uri_complete":"https://v?c=u"}`, "https://v", 5, 900},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { writeJSON(w, 200, tc.body) })
			_, _, uri, interval, expiresIn, err := OpenAIDeviceAuthorize(context.Background(), http.DefaultClient, ep)
			if err != nil {
				t.Fatalf("OpenAIDeviceAuthorize: %v", err)
			}
			if uri != tc.wantURI || interval != tc.wantInterval || expiresIn != tc.wantExpires {
				t.Fatalf("got (%q, %d, %d), want (%q, %d, %d)", uri, interval, expiresIn, tc.wantURI, tc.wantInterval, tc.wantExpires)
			}
		})
	}
}

func TestOpenAIDeviceAuthorizeErrors(t *testing.T) {
	t.Run("401 is auth rejected", func(t *testing.T) {
		ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { writeJSON(w, 401, `{"error":"invalid_client"}`) })
		_, _, _, _, _, err := OpenAIDeviceAuthorize(context.Background(), http.DefaultClient, ep)
		if !errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want ErrAuthRejected", err)
		}
	})
	t.Run("404 means the device flow is not offered", func(t *testing.T) {
		ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { writeJSON(w, 404, `{}`) })
		_, _, _, _, _, err := OpenAIDeviceAuthorize(context.Background(), http.DefaultClient, ep)
		var se *StatusError
		if !errors.As(err, &se) || se.Status != 404 || errors.Is(err, ErrAuthRejected) {
			t.Fatalf("err = %v, want a plain *StatusError for 404", err)
		}
	})
	for name, body := range map[string]string{
		"no device_code":       `{"user_code":"u","verification_uri":"https://v"}`,
		"no user_code":         `{"device_code":"d","verification_uri":"https://v"}`,
		"no verification page": `{"device_code":"d","user_code":"u"}`,
		"not json":             `<<<`,
	} {
		t.Run(name, func(t *testing.T) {
			ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { writeJSON(w, 200, body) })
			_, _, _, _, _, err := OpenAIDeviceAuthorize(context.Background(), http.DefaultClient, ep)
			if !errors.Is(err, ErrBadTokenResponse) {
				t.Fatalf("err = %v, want ErrBadTokenResponse", err)
			}
		})
	}
}

// fastPolling shrinks the poll timing knobs for a test and restores them.
func fastPolling(t *testing.T) {
	t.Helper()
	oldStep, oldWindow := slowDownStep, maxDevicePollWindow
	slowDownStep, maxDevicePollWindow = time.Millisecond, 5*time.Second
	t.Cleanup(func() { slowDownStep, maxDevicePollWindow = oldStep, oldWindow })
}

func TestPollOpenAIDeviceTokenPendingThenSuccess(t *testing.T) {
	fastPolling(t)
	idToken := stubJWT(t, openAIClaims("acct-dev", "plus"))
	var polls atomic.Int32
	ep := openAIStub(t, func(w http.ResponseWriter, path string, form url.Values) {
		if path != "/oauth/token" {
			t.Errorf("path = %s, want the token endpoint", path)
		}
		expectForm(t, form, map[string]string{
			"grant_type":  "urn:ietf:params:oauth:grant-type:device_code",
			"device_code": "dev-1",
			"client_id":   OpenAIClientID,
		})
		if polls.Add(1) == 1 {
			writeJSON(w, 400, `{"error":"authorization_pending"}`)
			return
		}
		writeJSON(w, 200, `{"access_token":"at","refresh_token":"rt","id_token":"`+idToken+`","expires_in":3600}`)
	})
	ts, err := PollOpenAIDeviceToken(context.Background(), http.DefaultClient, ep, "dev-1", time.Millisecond)
	if err != nil {
		t.Fatalf("PollOpenAIDeviceToken: %v", err)
	}
	if got := polls.Load(); got != 2 {
		t.Fatalf("token endpoint polled %d times, want 2 (pending, then success)", got)
	}
	if ts.AccessToken != "at" || ts.RefreshToken != "rt" || ts.AccountID != "acct-dev" || ts.PlanType != "plus" || ts.ExpiresAt.IsZero() {
		t.Fatalf("TokenSet = %+v", ts)
	}
}

func TestPollOpenAIDeviceTokenSlowDownBacksOffAndContinues(t *testing.T) {
	fastPolling(t)
	slowDownStep = 20 * time.Millisecond
	var polls atomic.Int32
	var stamps [3]time.Time
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		n := polls.Add(1)
		if n <= 3 {
			stamps[n-1] = time.Now()
		}
		switch n {
		case 1:
			writeJSON(w, 400, `{"error":"slow_down"}`)
		case 2:
			writeJSON(w, 400, `{"error":"authorization_pending"}`)
		default:
			writeJSON(w, 200, `{"access_token":"at","expires_in":60}`)
		}
	})
	ts, err := PollOpenAIDeviceToken(context.Background(), http.DefaultClient, ep, "dev", time.Millisecond)
	if err != nil {
		t.Fatalf("PollOpenAIDeviceToken: %v", err)
	}
	if ts.AccessToken != "at" || polls.Load() != 3 {
		t.Fatalf("TokenSet = %+v after %d polls", ts, polls.Load())
	}
	// After slow_down the interval grows by slowDownStep and stays grown.
	if gap := stamps[1].Sub(stamps[0]); gap < 20*time.Millisecond {
		t.Errorf("gap after slow_down = %v, want >= 20ms", gap)
	}
	if gap := stamps[2].Sub(stamps[1]); gap < 20*time.Millisecond {
		t.Errorf("interval must stay backed off, gap = %v", gap)
	}
}

func TestPollOpenAIDeviceTokenPendingMayArriveAsForbidden(t *testing.T) {
	// Some deployments answer "not yet" with 403 + an authorization_pending
	// code; that must not be mistaken for a credential rejection.
	fastPolling(t)
	var polls atomic.Int32
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		if polls.Add(1) == 1 {
			writeJSON(w, 403, `{"error":"authorization_pending"}`)
			return
		}
		writeJSON(w, 200, `{"access_token":"at"}`)
	})
	if _, err := PollOpenAIDeviceToken(context.Background(), http.DefaultClient, ep, "d", time.Millisecond); err != nil {
		t.Fatalf("err = %v, want the poll to continue past a 403 pending", err)
	}
}

func TestPollOpenAIDeviceTokenTerminalAnswers(t *testing.T) {
	fastPolling(t)
	cases := []struct {
		name     string
		status   int
		body     string
		wantIs   error
		wantAuth bool
	}{
		{"expired_token", 400, `{"error":"expired_token"}`, ErrDeviceCodeExpired, false},
		{"access_denied", 400, `{"error":"access_denied"}`, ErrDeviceCodeDenied, false},
		{"invalid_grant", 400, `{"error":"invalid_grant"}`, ErrAuthRejected, true},
		{"401", 401, `{}`, ErrAuthRejected, true},
		{"server error ends the poll", 500, `oops`, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
				polls.Add(1)
				writeJSON(w, tc.status, tc.body)
			})
			ts, err := PollOpenAIDeviceToken(context.Background(), http.DefaultClient, ep, "d", time.Millisecond)
			if err == nil {
				t.Fatalf("got %+v, want an error", ts)
			}
			if ts != (TokenSet{}) {
				t.Fatalf("an error must return the zero TokenSet, got %+v", ts)
			}
			if polls.Load() != 1 {
				t.Fatalf("polled %d times, want the poll to stop at the first terminal answer", polls.Load())
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("err = %v, want %v", err, tc.wantIs)
			}
			if tc.wantIs == nil {
				var se *StatusError
				if !errors.As(err, &se) || se.Status != tc.status {
					t.Fatalf("err = %v, want a *StatusError for HTTP %d", err, tc.status)
				}
			}
			if errors.Is(err, ErrAuthRejected) != tc.wantAuth {
				t.Fatalf("errors.Is(ErrAuthRejected) = %v, want %v (err: %v)", !tc.wantAuth, tc.wantAuth, err)
			}
		})
	}
}

func TestPollOpenAIDeviceTokenContextCancellation(t *testing.T) {
	fastPolling(t)
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		writeJSON(w, 400, `{"error":"authorization_pending"}`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := PollOpenAIDeviceToken(ctx, http.DefaultClient, ep, "d", 5*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's context.DeadlineExceeded", err)
	}
	if errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("a caller deadline must not masquerade as a vendor-side expiry")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("poll did not stop promptly on cancellation")
	}
}

func TestPollOpenAIDeviceTokenCanceledBeforeFirstPoll(t *testing.T) {
	fastPolling(t)
	var polls atomic.Int32
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { polls.Add(1) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := PollOpenAIDeviceToken(ctx, http.DefaultClient, ep, "d", time.Hour)
	if !errors.Is(err, context.Canceled) || polls.Load() != 0 {
		t.Fatalf("err = %v after %d polls, want context.Canceled and no request", err, polls.Load())
	}
}

func TestPollOpenAIDeviceTokenHardCapEndsAnUnboundedPoll(t *testing.T) {
	fastPolling(t)
	maxDevicePollWindow = 60 * time.Millisecond
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) {
		writeJSON(w, 400, `{"error":"authorization_pending"}`)
	})
	_, err := PollOpenAIDeviceToken(context.Background(), http.DefaultClient, ep, "d", 5*time.Millisecond)
	if !errors.Is(err, ErrDeviceCodeExpired) {
		t.Fatalf("err = %v, want ErrDeviceCodeExpired once the poll window is exhausted", err)
	}
}

func TestPollOpenAIDeviceTokenNonPositiveIntervalIsFloored(t *testing.T) {
	// A zero interval must not busy-loop; it is floored to the RFC default. The
	// context deadline cuts the wait short, so no request is ever made.
	var polls atomic.Int32
	ep := openAIStub(t, func(w http.ResponseWriter, _ string, _ url.Values) { polls.Add(1) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := PollOpenAIDeviceToken(ctx, http.DefaultClient, ep, "d", 0)
	if !errors.Is(err, context.DeadlineExceeded) || polls.Load() != 0 {
		t.Fatalf("err = %v after %d polls, want the default interval to apply", err, polls.Load())
	}
}

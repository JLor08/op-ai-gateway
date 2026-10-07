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
	"net/url"
	"strings"
	"testing"
	"time"
)

// anthropicStub starts an httptest token endpoint and returns the default
// Anthropic endpoints re-pointed at it. handler sees the decoded JSON body.
func anthropicStub(t *testing.T, handler func(w http.ResponseWriter, body map[string]string)) (Endpoints, *int) {
	t.Helper()
	calls := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		raw, _ := io.ReadAll(r.Body)
		body := map[string]string{}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body is not a flat JSON object: %v: %s", err, raw)
		}
		handler(w, body)
	}))
	t.Cleanup(srv.Close)
	ep := DefaultAnthropicEndpoints()
	ep.TokenURL = srv.URL + "/v1/oauth/token"
	return ep, calls
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func TestBuildAnthropicAuthorizeURL(t *testing.T) {
	ep := DefaultAnthropicEndpoints()
	verifier, challenge, err := GeneratePKCE()
	if err != nil {
		t.Fatal(err)
	}
	raw := BuildAnthropicAuthorizeURL(ep, verifier, "state-xyz")
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("authorize URL does not parse: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != ep.AuthorizeURL {
		t.Fatalf("base = %q, want %q", got, ep.AuthorizeURL)
	}
	want := map[string]string{
		"response_type":         "code",
		"client_id":             AnthropicClientID,
		"redirect_uri":          AnthropicRedirectURI,
		"scope":                 AnthropicScopes,
		"code_challenge":        challenge,
		"code_challenge_method": "S256",
		"state":                 "state-xyz",
		"code":                  "true",
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

func TestBuildAnthropicAuthorizeURLHonoursOverriddenEndpoints(t *testing.T) {
	ep := Endpoints{
		ClientID: "cid", AuthorizeURL: "http://127.0.0.1:9/authorize?keep=1",
		RedirectURI: "http://127.0.0.1:9/cb", Scopes: "a b",
	}
	u, err := url.Parse(BuildAnthropicAuthorizeURL(ep, "v", "s"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "127.0.0.1:9" || q.Get("keep") != "1" || q.Get("client_id") != "cid" ||
		q.Get("redirect_uri") != "http://127.0.0.1:9/cb" || q.Get("scope") != "a b" || q.Get("code") != "true" {
		t.Fatalf("overrides not honoured: %s", u)
	}
}

func TestExchangeAnthropicCode(t *testing.T) {
	ep, calls := anthropicStub(t, func(w http.ResponseWriter, body map[string]string) {
		want := map[string]string{
			"grant_type":    "authorization_code",
			"code":          "the-code",
			"redirect_uri":  AnthropicRedirectURI,
			"client_id":     AnthropicClientID,
			"code_verifier": "the-verifier",
		}
		if len(body) != len(want) {
			t.Errorf("body = %v, want exactly %v", body, want)
		}
		for k, v := range want {
			if body[k] != v {
				t.Errorf("body[%s] = %q, want %q", k, body[k], v)
			}
		}
		writeJSON(w, 200, `{"token_type":"Bearer","access_token":"sk-ant-oat01-A","refresh_token":"sk-ant-ort01-R","expires_in":28800,"scope":"user:inference user:profile"}`)
	})
	before := time.Now()
	ts, err := ExchangeAnthropicCode(context.Background(), http.DefaultClient, ep, "the-code", "the-verifier")
	after := time.Now()
	if err != nil {
		t.Fatalf("ExchangeAnthropicCode: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("token endpoint called %d times, want 1", *calls)
	}
	if ts.AccessToken != "sk-ant-oat01-A" || ts.RefreshToken != "sk-ant-ort01-R" || ts.Scope != "user:inference user:profile" {
		t.Fatalf("TokenSet = %+v", ts)
	}
	if ts.ExpiresAt.Before(before.Add(28800*time.Second)) || ts.ExpiresAt.After(after.Add(28800*time.Second)) {
		t.Fatalf("ExpiresAt = %v, want now+28800s (between %v and %v)", ts.ExpiresAt, before, after)
	}
}

func TestExchangeAnthropicCodeTrimsPastedCode(t *testing.T) {
	ep, _ := anthropicStub(t, func(w http.ResponseWriter, body map[string]string) {
		if body["code"] != "the-code" {
			t.Errorf("code = %q, want it trimmed", body["code"])
		}
		writeJSON(w, 200, `{"access_token":"A","expires_in":60}`)
	})
	if _, err := ExchangeAnthropicCode(context.Background(), nil, ep, "  the-code\n", "v"); err != nil {
		t.Fatalf("ExchangeAnthropicCode: %v", err)
	}
}

func TestRefreshAnthropic(t *testing.T) {
	t.Run("rotated refresh token is returned", func(t *testing.T) {
		ep, _ := anthropicStub(t, func(w http.ResponseWriter, body map[string]string) {
			want := map[string]string{"grant_type": "refresh_token", "refresh_token": "old-r", "client_id": AnthropicClientID}
			if len(body) != len(want) {
				t.Errorf("body = %v, want exactly %v", body, want)
			}
			for k, v := range want {
				if body[k] != v {
					t.Errorf("body[%s] = %q, want %q", k, body[k], v)
				}
			}
			writeJSON(w, 200, `{"access_token":"new-a","refresh_token":"new-r","expires_in":3600}`)
		})
		ts, err := RefreshAnthropic(context.Background(), http.DefaultClient, ep, "old-r")
		if err != nil {
			t.Fatalf("RefreshAnthropic: %v", err)
		}
		if ts.AccessToken != "new-a" || ts.RefreshToken != "new-r" || ts.ExpiresAt.IsZero() {
			t.Fatalf("TokenSet = %+v", ts)
		}
	})
	t.Run("a response without a refresh token keeps the one used", func(t *testing.T) {
		ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) {
			writeJSON(w, 200, `{"access_token":"new-a","expires_in":3600}`)
		})
		ts, err := RefreshAnthropic(context.Background(), http.DefaultClient, ep, "keep-me")
		if err != nil {
			t.Fatalf("RefreshAnthropic: %v", err)
		}
		if ts.RefreshToken != "keep-me" {
			t.Fatalf("RefreshToken = %q, want the original carried forward", ts.RefreshToken)
		}
	})
}

func TestAnthropicErrorMapping(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		body         string
		wantRejected bool
		wantContains []string
	}{
		{"401", 401, `{"error":"invalid_token"}`, true, []string{"HTTP 401"}},
		{"403", 403, `{"error":{"type":"permission_error","message":"nope"}}`, true, []string{"HTTP 403", "permission_error"}},
		{"400 invalid_grant", 400, `{"error":"invalid_grant","error_description":"Refresh token not found or invalid"}`, true, []string{"HTTP 400", "invalid_grant"}},
		{"400 nested invalid_grant", 400, `{"type":"error","error":{"type":"invalid_grant","message":"revoked"}}`, true, []string{"HTTP 400", "invalid_grant"}},
		{"400 other", 400, `{"error":"invalid_request","error_description":"bad body"}`, false, []string{"HTTP 400", "invalid_request"}},
		{"429", 429, `{"error":"rate_limited"}`, false, []string{"HTTP 429"}},
		{"500 non-json", 500, `<html>oops</html>`, false, []string{"HTTP 500"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) { writeJSON(w, tc.status, tc.body) })
			ts, err := ExchangeAnthropicCode(context.Background(), http.DefaultClient, ep, "SECRET-CODE", "SECRET-VERIFIER")
			if err == nil {
				t.Fatalf("got %+v, want an error", ts)
			}
			if ts != (TokenSet{}) {
				t.Fatalf("an error must return the zero TokenSet, got %+v", ts)
			}
			if got := errors.Is(err, ErrAuthRejected); got != tc.wantRejected {
				t.Fatalf("errors.Is(ErrAuthRejected) = %v, want %v (err: %v)", got, tc.wantRejected, err)
			}
			for _, s := range tc.wantContains {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not mention %q", err, s)
				}
			}
			var se *StatusError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Errorf("error is not a *StatusError carrying HTTP %d: %v", tc.status, err)
			}
			for _, secret := range []string{"SECRET-CODE", "SECRET-VERIFIER"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("error leaks request secret %q: %v", secret, err)
				}
			}
		})
	}
}

func TestRefreshAnthropicRejectedIsAuthRejected(t *testing.T) {
	ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) { writeJSON(w, 401, `{}`) })
	_, err := RefreshAnthropic(context.Background(), http.DefaultClient, ep, "r")
	if !errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want ErrAuthRejected", err)
	}
}

func TestAnthropicMalformedSuccessResponses(t *testing.T) {
	for name, body := range map[string]string{
		"no access token": `{"refresh_token":"r","expires_in":1}`,
		"not json":        `<<<`,
		"empty":           ``,
	} {
		t.Run(name, func(t *testing.T) {
			ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) { writeJSON(w, 200, body) })
			_, err := ExchangeAnthropicCode(context.Background(), http.DefaultClient, ep, "c", "v")
			if !errors.Is(err, ErrBadTokenResponse) {
				t.Fatalf("err = %v, want ErrBadTokenResponse", err)
			}
		})
	}
}

func TestAnthropicMissingExpiresInLeavesExpiryUnknown(t *testing.T) {
	ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) {
		writeJSON(w, 200, `{"access_token":"A"}`)
	})
	ts, err := ExchangeAnthropicCode(context.Background(), http.DefaultClient, ep, "c", "v")
	if err != nil {
		t.Fatal(err)
	}
	if !ts.ExpiresAt.IsZero() {
		t.Fatalf("ExpiresAt = %v, want zero (unknown) when expires_in is absent", ts.ExpiresAt)
	}
}

func TestAnthropicHonoursContextCancellation(t *testing.T) {
	ep, _ := anthropicStub(t, func(w http.ResponseWriter, _ map[string]string) { writeJSON(w, 200, `{"access_token":"A"}`) })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ExchangeAnthropicCode(ctx, http.DefaultClient, ep, "c", "v")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestAnthropicTransportErrorIsNotAuthRejected(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	ep := DefaultAnthropicEndpoints()
	ep.TokenURL = srv.URL
	srv.Close() // nothing listening any more → connection refused
	_, err := RefreshAnthropic(context.Background(), http.DefaultClient, ep, "r")
	if err == nil || errors.Is(err, ErrAuthRejected) {
		t.Fatalf("err = %v, want a plain transport error", err)
	}
}

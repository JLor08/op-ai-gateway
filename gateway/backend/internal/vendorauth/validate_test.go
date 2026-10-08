// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// probeSecret is a distinctive credential value; every test asserts it never
// shows up in a CredentialCheck.
const probeSecret = "sk-test-DO-NOT-LEAK-0123456789abcdef"

type recordedRequest struct {
	Method string
	Host   string
	Path   string
	Body   string
	Header http.Header
}

// probeRecorder remembers every request the stub server saw.
type probeRecorder struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

func (p *probeRecorder) add(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reqs = append(p.reqs, recordedRequest{
		Method: r.Method, Host: r.Host, Path: r.URL.Path, Body: string(body), Header: r.Header.Clone(),
	})
}

func (p *probeRecorder) all() []recordedRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedRequest(nil), p.reqs...)
}

// rewriteTransport sends every request to target regardless of the URL's host,
// but keeps the request's Host header, so the validators run against their real
// vendor URLs (constants, never overridden) while the test still asserts the
// exact host and path they asked for.
type rewriteTransport struct{ target *url.URL }

func (rt rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme = rt.target.Scheme
	r2.URL.Host = rt.target.Host
	return http.DefaultTransport.RoundTrip(r2)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// newProbeClient starts an httptest server running respond and returns an
// http.Client whose every request lands on it, plus the recorder of what it saw.
func newProbeClient(t *testing.T, respond http.HandlerFunc) (*http.Client, *probeRecorder) {
	t.Helper()
	rec := &probeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: rewriteTransport{target: target}}, rec
}

func respondWith(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, body)
	}
}

type probeCase struct {
	name string
	call func(ctx context.Context, c *http.Client, cred string) CredentialCheck
	host string
	path string
	// headers are the exact request headers the probe must send for cred.
	headers func(cred string) map[string]string
	// absent are headers the probe must NOT send.
	absent []string
	// forbidden is the verdict for HTTP 403.
	forbidden CredentialStatus
}

func probeCases() []probeCase {
	return []probeCase{
		{
			name: "openai subscription",
			call: ValidateOpenAISubscription,
			host: "chatgpt.com",
			path: "/backend-api/wham/accounts/check",
			headers: func(cred string) map[string]string {
				return map[string]string{"Authorization": "Bearer " + cred}
			},
			absent:    []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta"},
			forbidden: StatusUnverifiable,
		},
		{
			name: "anthropic subscription",
			call: ValidateAnthropicSubscription,
			host: "api.anthropic.com",
			path: "/api/oauth/profile",
			headers: func(cred string) map[string]string {
				return map[string]string{
					"Authorization":     "Bearer " + cred,
					"Anthropic-Beta":    "oauth-2025-04-20",
					"Anthropic-Version": "2023-06-01",
				}
			},
			absent: []string{"X-Api-Key"},
			// A `claude setup-token` token has scope user:inference, not
			// user:profile, so the profile endpoint legitimately answers 403.
			forbidden: StatusValid,
		},
		{
			name: "openai api key",
			call: ValidateOpenAIAPIKey,
			host: "api.openai.com",
			path: "/v1/models",
			headers: func(cred string) map[string]string {
				return map[string]string{"Authorization": "Bearer " + cred}
			},
			absent:    []string{"X-Api-Key", "Anthropic-Version", "Anthropic-Beta"},
			forbidden: StatusUnverifiable,
		},
		{
			name: "anthropic api key",
			call: ValidateAnthropicAPIKey,
			host: "api.anthropic.com",
			path: "/v1/models",
			headers: func(cred string) map[string]string {
				return map[string]string{"X-Api-Key": cred, "Anthropic-Version": "2023-06-01"}
			},
			absent:    []string{"Authorization", "Anthropic-Beta"},
			forbidden: StatusUnverifiable,
		},
	}
}

func TestValidatorsRequestShape(t *testing.T) {
	for _, tc := range probeCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, respondWith(http.StatusOK, `{}`))
			tc.call(context.Background(), client, probeSecret)

			reqs := rec.all()
			if len(reqs) != 1 {
				t.Fatalf("requests = %d, want exactly 1", len(reqs))
			}
			got := reqs[0]
			if got.Method != http.MethodGet {
				t.Errorf("method = %s, want GET", got.Method)
			}
			if got.Host != tc.host {
				t.Errorf("host = %q, want %q", got.Host, tc.host)
			}
			if got.Path != tc.path {
				t.Errorf("path = %q, want %q", got.Path, tc.path)
			}
			if got.Body != "" {
				t.Errorf("body = %q, want none (the probe is model-independent)", got.Body)
			}
			for name, want := range tc.headers(probeSecret) {
				if v := got.Header.Get(name); v != want {
					t.Errorf("header %s = %q, want %q", name, v, want)
				}
			}
			for _, name := range tc.absent {
				if v := got.Header.Get(name); v != "" {
					t.Errorf("header %s = %q, want it absent", name, v)
				}
			}
		})
	}
}

func TestValidatorsStatusMapping(t *testing.T) {
	for _, tc := range probeCases() {
		t.Run(tc.name, func(t *testing.T) {
			want := map[int]CredentialStatus{
				http.StatusOK:                  StatusValid,
				http.StatusNoContent:           StatusValid,
				http.StatusUnauthorized:        StatusInvalid,
				http.StatusForbidden:           tc.forbidden,
				http.StatusBadRequest:          StatusUnverifiable,
				http.StatusNotFound:            StatusUnverifiable,
				http.StatusRequestTimeout:      StatusUnverifiable,
				http.StatusTooManyRequests:     StatusUnverifiable,
				http.StatusInternalServerError: StatusUnverifiable,
				http.StatusBadGateway:          StatusUnverifiable,
				http.StatusServiceUnavailable:  StatusUnverifiable,
			}
			for code, wantStatus := range want {
				client, _ := newProbeClient(t, respondWith(code, `{}`))
				got := tc.call(context.Background(), client, probeSecret)
				if got.Status != wantStatus {
					t.Errorf("HTTP %d: status = %v, want %v (detail %q)", code, got.Status, wantStatus, got.Detail)
				}
				if got.Detail == "" {
					t.Errorf("HTTP %d: Detail is empty, want a status phrase", code)
				}
			}
		})
	}
}

func TestValidateAnthropicSubscriptionForbiddenMeansScopeNotInvalid(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusForbidden,
		`{"type":"error","error":{"type":"permission_error","message":"OAuth token does not meet scope requirement user:profile"}}`))
	got := ValidateAnthropicSubscription(context.Background(), client, probeSecret)
	if got.Status != StatusValid {
		t.Fatalf("status = %v, want valid (a setup-token token lacks user:profile)", got.Status)
	}
	if !strings.Contains(got.Detail, "403") || !strings.Contains(got.Detail, "permission_error") {
		t.Errorf("Detail = %q, want it to say the profile endpoint answered 403 permission_error", got.Detail)
	}
	if got.AccountID != "" || got.PlanType != "" {
		t.Errorf("AccountID/PlanType = %q/%q, want empty (only the OpenAI subscription validator fills them)", got.AccountID, got.PlanType)
	}
}

func TestValidatorsSurfaceVendorErrorCode(t *testing.T) {
	tests := []struct {
		name   string
		call   func(ctx context.Context, c *http.Client, cred string) CredentialCheck
		status int
		body   string
		want   string
	}{
		{
			name: "openai nested code wins over type", call: ValidateOpenAIAPIKey, status: http.StatusUnauthorized,
			body: `{"error":{"message":"Incorrect API key provided: ` + probeSecret + `","type":"invalid_request_error","code":"invalid_api_key"}}`,
			want: "invalid_api_key",
		},
		{
			name: "anthropic nested type", call: ValidateAnthropicAPIKey, status: http.StatusUnauthorized,
			body: `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`,
			want: "authentication_error",
		},
		{
			name: "anthropic rate limit on an unverifiable answer", call: ValidateAnthropicAPIKey, status: http.StatusTooManyRequests,
			body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			want: "rate_limit_error",
		},
		{
			name: "oauth style string error", call: ValidateAnthropicSubscription, status: http.StatusUnauthorized,
			body: `{"error":"invalid_token","error_description":"The access token expired"}`,
			want: "invalid_token",
		},
		{
			name: "chatgpt backend nested code", call: ValidateOpenAISubscription, status: http.StatusUnauthorized,
			body: `{"error":{"code":"token_expired","message":"expired"}}`,
			want: "token_expired",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(tc.status, tc.body))
			got := tc.call(context.Background(), client, probeSecret)
			if !strings.Contains(got.Detail, tc.want) {
				t.Errorf("Detail = %q, want it to carry the vendor code %q", got.Detail, tc.want)
			}
			if !strings.Contains(got.Detail, "HTTP") {
				t.Errorf("Detail = %q, want it to name the HTTP status", got.Detail)
			}
			if strings.Contains(got.Detail, "Incorrect API key") || strings.Contains(got.Detail, "invalid x-api-key") {
				t.Errorf("Detail = %q, must not echo the vendor's free-text message", got.Detail)
			}
		})
	}
}

func TestValidatorsIgnoreUnusableErrorBodies(t *testing.T) {
	bodies := map[string]string{
		"html":                `<html><body>502 Bad Gateway</body></html>`,
		"empty":               ``,
		"json without error":  `{"detail":"nope"}`,
		"error is a number":   `{"error":42}`,
		"code is not a token": `{"error":{"code":"Incorrect API key provided: abc def","type":"x y"}}`,
		"code too long":       `{"error":{"code":"` + strings.Repeat("a", 200) + `"}}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusUnauthorized, body))
			got := ValidateOpenAIAPIKey(context.Background(), client, probeSecret)
			if got.Status != StatusInvalid {
				t.Errorf("status = %v, want invalid", got.Status)
			}
			if strings.Contains(got.Detail, "Incorrect") || strings.Contains(got.Detail, "aaaa") || strings.Contains(got.Detail, "<html") {
				t.Errorf("Detail = %q, must not carry body text that is not a short error code", got.Detail)
			}
		})
	}
}

func TestValidatorsNeverLeakCredential(t *testing.T) {
	// A hostile or careless vendor that echoes the credential in every field of
	// its answer must still not get it into Detail.
	echo := func(status int) http.HandlerFunc {
		return respondWith(status, `{"error":{"message":"bad key `+probeSecret+`","type":"`+probeSecret+`","code":"`+probeSecret+`"},`+
			`"error_description":"`+probeSecret+`","default_account_id":"acc-1","accounts":[{"id":"acc-1","plan_type":"plus"}]}`)
	}
	statuses := []int{
		http.StatusOK, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusTooManyRequests, http.StatusInternalServerError,
	}
	for _, tc := range probeCases() {
		for _, status := range statuses {
			client, _ := newProbeClient(t, echo(status))
			got := tc.call(context.Background(), client, probeSecret)
			if strings.Contains(got.Detail, probeSecret) {
				t.Errorf("%s / HTTP %d: Detail %q contains the credential", tc.name, status, got.Detail)
			}
			if strings.Contains(got.AccountID, probeSecret) || strings.Contains(got.PlanType, probeSecret) {
				t.Errorf("%s / HTTP %d: AccountID/PlanType contain the credential", tc.name, status)
			}
		}
	}
}

func TestValidatorsTransportFailuresAreUnverifiable(t *testing.T) {
	for _, tc := range probeCases() {
		t.Run(tc.name+"/client timeout", func(t *testing.T) {
			release := make(chan struct{})
			t.Cleanup(func() { close(release) })
			client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			client.Timeout = 50 * time.Millisecond
			got := tc.call(context.Background(), client, probeSecret)
			if got.Status != StatusUnverifiable {
				t.Fatalf("status = %v, want unverifiable (detail %q)", got.Status, got.Detail)
			}
			if !strings.Contains(got.Detail, "timed out") {
				t.Errorf("Detail = %q, want it to say the request timed out", got.Detail)
			}
		})

		t.Run(tc.name+"/context deadline", func(t *testing.T) {
			client, _ := newProbeClient(t, func(_ http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			got := tc.call(ctx, client, probeSecret)
			if got.Status != StatusUnverifiable || !strings.Contains(got.Detail, "timed out") {
				t.Errorf("got %v %q, want unverifiable and a timed-out phrase", got.Status, got.Detail)
			}
		})

		t.Run(tc.name+"/context canceled", func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, `{}`))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			got := tc.call(ctx, client, probeSecret)
			if got.Status != StatusUnverifiable || !strings.Contains(got.Detail, "canceled") {
				t.Errorf("got %v %q, want unverifiable and a canceled phrase", got.Status, got.Detail)
			}
		})

		t.Run(tc.name+"/transport error", func(t *testing.T) {
			// The transport error text deliberately embeds the credential: the
			// verdict must not repeat error text, only a fixed phrase.
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("dial failed for " + probeSecret)
			})}
			got := tc.call(context.Background(), client, probeSecret)
			if got.Status != StatusUnverifiable {
				t.Fatalf("status = %v, want unverifiable", got.Status)
			}
			if strings.Contains(got.Detail, probeSecret) {
				t.Errorf("Detail %q contains the credential", got.Detail)
			}
			if got.Detail == "" {
				t.Error("Detail is empty, want a status phrase")
			}
		})

		t.Run(tc.name+"/connection refused", func(t *testing.T) {
			srv := httptest.NewServer(http.NotFoundHandler())
			target, _ := url.Parse(srv.URL)
			srv.Close() // nothing listens on target any more
			client := &http.Client{Transport: rewriteTransport{target: target}}
			got := tc.call(context.Background(), client, probeSecret)
			if got.Status != StatusUnverifiable {
				t.Errorf("status = %v, want unverifiable (detail %q)", got.Status, got.Detail)
			}
		})
	}
}

func TestValidatorsDoNotFollowRedirects(t *testing.T) {
	// x-api-key is not stripped by net/http on a cross-host redirect, so a
	// redirect must end the probe rather than carry the credential elsewhere.
	for _, tc := range probeCases() {
		t.Run(tc.name, func(t *testing.T) {
			client, rec := newProbeClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.example.test/steal", http.StatusFound)
			})
			got := tc.call(context.Background(), client, probeSecret)
			if n := len(rec.all()); n != 1 {
				t.Errorf("requests = %d, want 1 (the redirect must not be followed)", n)
			}
			if got.Status != StatusUnverifiable {
				t.Errorf("status = %v, want unverifiable for a redirect answer", got.Status)
			}
		})
	}
}

func TestValidatorsLeaveCallersClientUntouched(t *testing.T) {
	client, _ := newProbeClient(t, respondWith(http.StatusOK, `{}`))
	client.Timeout = 7 * time.Second
	for _, tc := range probeCases() {
		tc.call(context.Background(), client, probeSecret)
	}
	if client.CheckRedirect != nil {
		t.Error("the validators mutated the caller's http.Client.CheckRedirect")
	}
	if client.Timeout != 7*time.Second {
		t.Errorf("client.Timeout = %v, want it untouched", client.Timeout)
	}
}

func TestValidateOpenAISubscriptionExtractsAccount(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantID   string
		wantPlan string
	}{
		{
			name:   "default account picks its own plan",
			body:   `{"default_account_id":"acc-2","accounts":[{"id":"acc-1","plan_type":"free"},{"id":"acc-2","plan_type":"pro"}]}`,
			wantID: "acc-2", wantPlan: "pro",
		},
		{
			name:   "no default falls back to the first account",
			body:   `{"accounts":[{"id":"acc-1","plan_type":"plus"},{"id":"acc-2","plan_type":"pro"}]}`,
			wantID: "acc-1", wantPlan: "plus",
		},
		{
			name:   "empty default falls back to the first account",
			body:   `{"default_account_id":"","accounts":[{"id":"acc-7","plan_type":"team"}]}`,
			wantID: "acc-7", wantPlan: "team",
		},
		{
			name:   "default not among the accounts keeps the id, not another account's plan",
			body:   `{"default_account_id":"acc-9","accounts":[{"id":"acc-1","plan_type":"plus"}]}`,
			wantID: "acc-9", wantPlan: "",
		},
		{
			name:   "default without an accounts list",
			body:   `{"default_account_id":"acc-3"}`,
			wantID: "acc-3", wantPlan: "",
		},
		{
			name:   "accounts of an unexpected shape still yield the default id",
			body:   `{"default_account_id":"acc-4","accounts":{"acc-4":{"plan_type":"pro"}}}`,
			wantID: "acc-4", wantPlan: "",
		},
		{
			name:   "matching account without a plan",
			body:   `{"default_account_id":"acc-5","accounts":[{"id":"acc-5"}]}`,
			wantID: "acc-5", wantPlan: "",
		},
		{name: "no accounts at all", body: `{"accounts":[]}`, wantID: "", wantPlan: ""},
		{name: "empty object", body: `{}`, wantID: "", wantPlan: ""},
		{name: "not json", body: `<html>ok</html>`, wantID: "", wantPlan: ""},
		{name: "empty body", body: ``, wantID: "", wantPlan: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client, _ := newProbeClient(t, respondWith(http.StatusOK, tc.body))
			got := ValidateOpenAISubscription(context.Background(), client, probeSecret)
			if got.Status != StatusValid {
				t.Errorf("status = %v, want valid (a body that does not parse never changes the verdict)", got.Status)
			}
			if got.AccountID != tc.wantID {
				t.Errorf("AccountID = %q, want %q", got.AccountID, tc.wantID)
			}
			if got.PlanType != tc.wantPlan {
				t.Errorf("PlanType = %q, want %q", got.PlanType, tc.wantPlan)
			}
		})
	}
}

func TestOnlyAnOpenAISubscriptionAcceptanceFillsAccount(t *testing.T) {
	const body = `{"default_account_id":"acc-1","accounts":[{"id":"acc-1","plan_type":"plus"}]}`
	for _, tc := range probeCases() {
		for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
			client, _ := newProbeClient(t, respondWith(status, body))
			got := tc.call(context.Background(), client, probeSecret)
			if got.AccountID != "" || got.PlanType != "" {
				t.Errorf("%s / HTTP %d: AccountID/PlanType = %q/%q, want empty on a non-accepting answer", tc.name, status, got.AccountID, got.PlanType)
			}
		}
		if tc.name == "openai subscription" {
			continue
		}
		client, _ := newProbeClient(t, respondWith(http.StatusOK, body))
		got := tc.call(context.Background(), client, probeSecret)
		if got.AccountID != "" || got.PlanType != "" {
			t.Errorf("%s: AccountID/PlanType = %q/%q, want empty (only the OpenAI subscription validator fills them)", tc.name, got.AccountID, got.PlanType)
		}
	}
}

func TestCredentialStatus(t *testing.T) {
	var unset CredentialCheck
	for _, s := range []CredentialStatus{StatusValid, StatusInvalid, StatusUnverifiable} {
		if unset.Status == s {
			t.Errorf("the zero CredentialCheck reads as %v; an unset check must never look like a verdict", s)
		}
	}
	want := map[CredentialStatus]string{
		StatusValid:        "valid",
		StatusInvalid:      "invalid",
		StatusUnverifiable: "unverifiable",
		unset.Status:       "unknown",
	}
	for s, str := range want {
		if got := s.String(); got != str {
			t.Errorf("CredentialStatus(%d).String() = %q, want %q", int(s), got, str)
		}
	}
}

func TestVendorValidationURLsAreTheDocumentedOnes(t *testing.T) {
	want := map[string]string{
		OpenAIAccountsCheckURL:   "https://chatgpt.com/backend-api/wham/accounts/check",
		AnthropicOAuthProfileURL: "https://api.anthropic.com/api/oauth/profile",
		OpenAIModelsURL:          "https://api.openai.com/v1/models",
		AnthropicModelsURL:       "https://api.anthropic.com/v1/models",
	}
	for got, w := range want {
		if got != w {
			t.Errorf("constant = %q, want %q", got, w)
		}
	}
}

func TestAccessTokenExpiry(t *testing.T) {
	const exp = int64(1893456000) // 2030-01-01T00:00:00Z
	wantTime := time.Unix(exp, 0).UTC()

	valid := map[string]string{
		"integer exp":                stubJWT(t, map[string]any{"sub": "u", "exp": exp}),
		"fractional exp truncates":   stubJWT(t, map[string]any{"exp": float64(exp) + 0.9}),
		"alongside the openai claim": stubJWT(t, map[string]any{"exp": exp, OpenAIAuthClaimNamespace: map[string]any{OpenAIClaimAccountID: "a"}}),
	}
	for name, token := range valid {
		t.Run(name, func(t *testing.T) {
			got, ok := AccessTokenExpiry(token)
			if !ok {
				t.Fatal("ok = false, want true")
			}
			if !got.Equal(wantTime) {
				t.Errorf("expiry = %v, want %v", got, wantTime)
			}
			if got.Location() != time.UTC {
				t.Errorf("location = %v, want UTC", got.Location())
			}
		})
	}

	t.Run("surrounding whitespace and padded payload are tolerated", func(t *testing.T) {
		token := stubJWT(t, map[string]any{"exp": exp})
		parts := strings.Split(token, ".")
		padded := parts[0] + "." + parts[1] + strings.Repeat("=", (4-len(parts[1])%4)%4) + "." + parts[2]
		got, ok := AccessTokenExpiry("  " + padded + "\n")
		if !ok || !got.Equal(wantTime) {
			t.Errorf("got %v, %v; want %v, true", got, ok, wantTime)
		}
	})

	invalid := map[string]string{
		"empty":                   "",
		"opaque token":            "sk-ant-oat01-abcdef",
		"two segments":            "aaa.bbb",
		"four segments":           "a.b.c.d",
		"payload is not base64":   "aaa.!!!.ccc",
		"payload is not json":     "aaa.bm90IGpzb24.ccc", // "not json"
		"payload is a json array": "aaa.WzEsMiwzXQ.ccc",  // [1,2,3]
		"no exp claim":            stubJWT(t, map[string]any{"sub": "u"}),
		"exp is a string":         stubJWT(t, map[string]any{"exp": "1893456000"}),
		"exp is null":             stubJWT(t, map[string]any{"exp": nil}),
		"exp is a bool":           stubJWT(t, map[string]any{"exp": true}),
		"exp is zero":             stubJWT(t, map[string]any{"exp": 0}),
		"exp is negative":         stubJWT(t, map[string]any{"exp": -5}),
		"exp is absurdly large":   stubJWT(t, map[string]any{"exp": 1e30}),
	}
	for name, token := range invalid {
		t.Run(name, func(t *testing.T) {
			got, ok := AccessTokenExpiry(token)
			if ok || !got.IsZero() {
				t.Errorf("got %v, %v; want the zero time and false", got, ok)
			}
		})
	}
}

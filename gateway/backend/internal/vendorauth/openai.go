// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrDeviceCodeExpired means the device code can no longer be redeemed: the
	// vendor answered expired_token, or the poll window ran out. Start over with
	// OpenAIDeviceAuthorize.
	ErrDeviceCodeExpired = errors.New("vendorauth: device code expired")

	// ErrDeviceCodeDenied means the user declined the authorization (access_denied).
	ErrDeviceCodeDenied = errors.New("vendorauth: device authorization denied")
)

const (
	// deviceCodeGrantType is the RFC 8628 grant_type for redeeming a device code.
	deviceCodeGrantType = "urn:ietf:params:oauth:grant-type:device_code"

	// defaultDeviceInterval and defaultDeviceExpiresIn are the RFC 8628 defaults
	// applied when the vendor omits interval (5s) or expires_in (RFC: required;
	// 15 minutes here is only a fallback for a non-conforming answer).
	defaultDeviceInterval  = 5 * time.Second
	defaultDeviceExpiresIn = 900
)

// slowDownStep is how much PollOpenAIDeviceToken lengthens its interval on a
// slow_down answer (RFC 8628 §3.5). A variable only so tests can shrink it.
var slowDownStep = 5 * time.Second

// maxDevicePollWindow caps how long one PollOpenAIDeviceToken call polls
// regardless of the caller's context, so a caller that forgets a deadline cannot
// poll forever. A variable only so tests can shrink it.
var maxDevicePollWindow = 15 * time.Minute

// BuildOpenAIAuthorizeURL returns the URL the user opens to sign in to
// auth.openai.com and approve the gateway: the standard OAuth+PKCE parameters
// (S256 challenge derived from verifier, never the verifier itself) plus the
// Codex-specific id_token_add_organizations, codex_cli_simplified_flow and
// originator parameters. The registered redirect is a loopback URL the gateway
// cannot listen on, so the user copies the code from the failed redirect back by
// hand; verifying state and extracting code is the CALLER's job.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func BuildOpenAIAuthorizeURL(ep Endpoints, verifier, state string) string {
	q := authorizeQuery(ep, verifier, state)
	q.Set(OpenAIParamIDTokenAddOrganizations, "true")
	q.Set(OpenAIParamCodexCLISimplifiedFlow, "true")
	q.Set(OpenAIParamOriginator, OpenAIOriginator)
	return appendQuery(ep.AuthorizeURL, q)
}

// ExchangeOpenAICode trades an authorization code (plus the PKCE verifier) for
// tokens by POSTing a form-encoded body to ep.TokenURL (the standard OAuth
// choice; the Codex CLI may use JSON, VERIFY LIVE). The account id and plan are
// read from the id_token's https://api.openai.com/auth claim. ExpiresAt is
// time.Now() plus the response's expires_in. A 401/403 or OAuth invalid_grant
// answer matches ErrAuthRejected.
func ExchangeOpenAICode(ctx context.Context, httpClient *http.Client, ep Endpoints, code, verifier string) (TokenSet, error) {
	return openAIToken(ctx, httpClient, ep, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {strings.TrimSpace(code)},
		"redirect_uri":  {ep.RedirectURI},
		"client_id":     {ep.ClientID},
		"code_verifier": {verifier},
	}, "")
}

// RefreshOpenAI exchanges a refresh token for a fresh TokenSet. When the vendor
// does not rotate the refresh token the result keeps refreshToken. AccountID and
// PlanType are filled only if the response carries an id_token; otherwise the
// CALLER must carry them forward from the previous TokenSet. A dead refresh
// token matches ErrAuthRejected (the account needs reconnecting).
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func RefreshOpenAI(ctx context.Context, httpClient *http.Client, ep Endpoints, refreshToken string) (TokenSet, error) {
	return openAIToken(ctx, httpClient, ep, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {ep.ClientID},
	}, refreshToken)
}

// OpenAIDeviceAuthorize starts an RFC 8628 device-code login: it POSTs the
// client id and scopes to ep.DeviceAuthorizeURL and returns the device code to
// poll with, the short user code to show, the page the user opens to enter it,
// the minimum poll interval in seconds (default 5) and the code lifetime in
// seconds (default 900).
//
// UNVERIFIED: the endpoint, and whether the Codex client id may use the device
// flow at all, are guesses; see OpenAIDeviceAuthorizeURL. A 404 here most likely
// means the flow is not offered, in which case fall back to the
// BuildOpenAIAuthorizeURL code-paste flow.
func OpenAIDeviceAuthorize(ctx context.Context, httpClient *http.Client, ep Endpoints) (deviceCode, userCode, verificationURI string, interval int, expiresIn int, err error) {
	status, body, err := postForm(ctx, httpClient, ep.DeviceAuthorizeURL, url.Values{
		"client_id": {ep.ClientID},
		"scope":     {ep.Scopes},
	})
	if err != nil {
		return "", "", "", 0, 0, err
	}
	if status < 200 || status > 299 {
		return "", "", "", 0, 0, newStatusError(status, body)
	}
	var resp struct {
		DeviceCode              string      `json:"device_code"`
		UserCode                string      `json:"user_code"`
		VerificationURI         string      `json:"verification_uri"`
		VerificationURL         string      `json:"verification_url"`
		VerificationURIComplete string      `json:"verification_uri_complete"`
		Interval                json.Number `json:"interval"`
		ExpiresIn               json.Number `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", "", "", 0, 0, fmt.Errorf("%w: device authorization: %v", ErrBadTokenResponse, err)
	}
	verification := firstNonEmpty(resp.VerificationURI, resp.VerificationURL, resp.VerificationURIComplete)
	if resp.DeviceCode == "" || resp.UserCode == "" || verification == "" {
		return "", "", "", 0, 0, fmt.Errorf("%w: device authorization lacks a device_code, user_code or verification page", ErrBadTokenResponse)
	}
	interval = positiveInt(resp.Interval, int(defaultDeviceInterval/time.Second))
	expiresIn = positiveInt(resp.ExpiresIn, defaultDeviceExpiresIn)
	return resp.DeviceCode, resp.UserCode, verification, interval, expiresIn, nil
}

// PollOpenAIDeviceToken redeems a device code by polling ep.TokenURL every
// interval (waiting one interval before the first request, since the user has
// only just been shown the code). authorization_pending keeps polling;
// slow_down also lengthens the interval by 5s (RFC 8628 §3.5). It returns
// ErrDeviceCodeExpired when the vendor says expired_token or the poll window
// (15 minutes, a hard ceiling independent of the caller) runs out, and
// ErrDeviceCodeDenied on access_denied. A caller-side cancellation or deadline
// returns that context error, so derive a context from OpenAIDeviceAuthorize's
// expiresIn (context.WithTimeout) to stop exactly when the code does. Any other
// answer ends the poll with a *StatusError (the caller may call again with the
// same device code); success parses like ExchangeOpenAICode.
//
// UNVERIFIED: see OpenAIDeviceAuthorizeURL.
func PollOpenAIDeviceToken(ctx context.Context, httpClient *http.Client, ep Endpoints, deviceCode string, interval time.Duration) (TokenSet, error) {
	if interval <= 0 {
		interval = defaultDeviceInterval
	}
	polling, cancel := context.WithTimeout(ctx, maxDevicePollWindow)
	defer cancel()
	form := url.Values{
		"grant_type":  {deviceCodeGrantType},
		"device_code": {deviceCode},
		"client_id":   {ep.ClientID},
	}
	for {
		if err := sleepContext(polling, interval); err != nil {
			return TokenSet{}, pollInterrupted(ctx)
		}
		status, body, err := postForm(polling, httpClient, ep.TokenURL, form)
		if err != nil {
			if polling.Err() != nil {
				return TokenSet{}, pollInterrupted(ctx)
			}
			return TokenSet{}, err
		}
		if status >= 200 && status <= 299 {
			return openAITokenSet(status, body, "")
		}
		se := newStatusError(status, body)
		// Judge by the OAuth error code before the HTTP status: some deployments
		// signal "not yet" with a 403, which must not read as a rejection.
		switch se.Code {
		case "authorization_pending":
		case "slow_down":
			interval += slowDownStep
		case "expired_token":
			return TokenSet{}, ErrDeviceCodeExpired
		case "access_denied":
			return TokenSet{}, ErrDeviceCodeDenied
		default:
			return TokenSet{}, se
		}
	}
}

// pollInterrupted explains why the poll loop stopped on its own context: the
// caller's cancellation or deadline wins; otherwise our window ran out.
func pollInterrupted(caller context.Context) error {
	if err := caller.Err(); err != nil {
		return err
	}
	return ErrDeviceCodeExpired
}

// sleepContext waits d or until ctx is done.
func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// postForm sends form as an application/x-www-form-urlencoded body.
func postForm(ctx context.Context, c *http.Client, endpoint string, form url.Values) (int, []byte, error) {
	return post(ctx, c, endpoint, "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
}

// openAIToken posts form to the token endpoint and maps the answer to a TokenSet.
func openAIToken(ctx context.Context, httpClient *http.Client, ep Endpoints, form url.Values, fallbackRefresh string) (TokenSet, error) {
	status, body, err := postForm(ctx, httpClient, ep.TokenURL, form)
	if err != nil {
		return TokenSet{}, err
	}
	return openAITokenSet(status, body, fallbackRefresh)
}

// openAITokenSet maps a token-endpoint answer to a TokenSet. fallbackRefresh is
// kept when the response issues no new refresh token.
func openAITokenSet(status int, body []byte, fallbackRefresh string) (TokenSet, error) {
	tr, err := parseTokenResponse(status, body)
	if err != nil {
		return TokenSet{}, err
	}
	accountID, planType := parseOpenAIIDTokenClaims(tr.IDToken)
	if accountID == "" || planType == "" {
		// The access token is itself a JWT carrying the same claim object; use
		// it for whatever the id_token did not supply.
		accessAccount, accessPlan := parseOpenAIIDTokenClaims(tr.AccessToken)
		accountID = firstNonEmpty(accountID, accessAccount)
		planType = firstNonEmpty(planType, accessPlan)
	}
	return TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: firstNonEmpty(tr.RefreshToken, fallbackRefresh),
		ExpiresAt:    tr.expiresAt(time.Now()),
		AccountID:    accountID,
		PlanType:     planType,
		Scope:        tr.Scope,
	}, nil
}

// parseOpenAIIDTokenClaims reads chatgpt_account_id and chatgpt_plan_type from
// the https://api.openai.com/auth claim of a JWT (id_token or access token). It
// decodes the payload without verifying the signature: the token came straight
// from the token endpoint over TLS and is used only to label the account, never
// to authenticate anyone. Tolerant by design: a missing, malformed or
// unexpectedly typed token yields empty strings and never panics.
func parseOpenAIIDTokenClaims(idToken string) (accountID, planType string) {
	parts := strings.Split(strings.TrimSpace(idToken), ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", ""
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	auth, _ := claims[OpenAIAuthClaimNamespace].(map[string]any)
	accountID, _ = auth[OpenAIClaimAccountID].(string)
	planType, _ = auth[OpenAIClaimPlanType].(string)
	return accountID, planType
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// positiveInt returns n as an int when it is a positive number, else fallback.
func positiveInt(n json.Number, fallback int) int {
	f, err := n.Float64()
	if err != nil || f < 1 {
		return fallback
	}
	return int(f)
}

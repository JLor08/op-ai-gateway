// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Why there is no device-code login here (a note for a future device-UI
// milestone, not a TODO for this file): the Codex CLI's device flow is NOT the
// generic RFC 8628 grant, so an RFC 8628 client cannot talk to it. Per a review
// of the Codex CLI source (codex-rs/login/src/device_code_auth.rs; not
// re-verified live), it is a bespoke protocol against the issuer:
//
//  1. POST {issuer}/api/accounts/deviceauth/usercode answers
//     {device_auth_id, user_code, interval}.
//  2. The user opens the verification page {issuer}/codex/device and enters
//     user_code.
//  3. Poll POST {issuer}/api/accounts/deviceauth/token with
//     {device_auth_id, user_code}; HTTP 403 or 404 means "not yet", and a 2xx
//     answer carries authorization_code, code_challenge and code_verifier.
//  4. Finish with a normal authorization-code exchange (ExchangeOpenAICode)
//     using those values and redirect_uri={issuer}/deviceauth/callback.
//
// OpenAI subscription connect uses the code-paste flow below instead
// (BuildOpenAIAuthorizeURL, then ExchangeOpenAICode).

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

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file holds the OpenAI code-paste connect flow (BuildOpenAIAuthorizeURL,
// then ExchangeOpenAICode) and the shared token/claim helpers. The bespoke Codex
// device-code login -- a separate, optional connect method that also works
// remotely -- lives in openai_device.go.

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

// OpenAIClaimsFromJWT reads the ChatGPT account id and plan type from the
// https://api.openai.com/auth claim of an OpenAI JWT (an id_token or an access
// token), without verifying its signature. It is the exported face of the
// claim parsing the code exchange and the refresh use, for a caller that holds
// only a pasted access token (an import) and so never saw the id_token: the
// access token is itself a JWT carrying the same claim object. Tolerant by
// design: a token that is not a JWT, or carries no such claim, yields empty
// strings, never an error.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func OpenAIClaimsFromJWT(token string) (accountID, planType string) {
	return parseOpenAIIDTokenClaims(token)
}

// parseOpenAIIDTokenClaims reads chatgpt_account_id and chatgpt_plan_type from
// the https://api.openai.com/auth claim of a JWT (id_token or access token). It
// decodes the payload without verifying the signature: the token came straight
// from the token endpoint over TLS and is used only to label the account, never
// to authenticate anyone. Tolerant by design: a missing, malformed or
// unexpectedly typed token yields empty strings and never panics.
func parseOpenAIIDTokenClaims(idToken string) (accountID, planType string) {
	claims, ok := jwtClaims(idToken)
	if !ok {
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

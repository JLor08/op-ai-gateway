// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// BuildAnthropicAuthorizeURL returns the URL the user opens to sign in to
// claude.ai and approve the gateway. It carries the S256 challenge derived from
// verifier (never the verifier itself) and state; the callback page then shows a
// "code#state" string for the user to paste back. Splitting that string and
// verifying state against the one issued here is the CALLER's job: pass the
// code part to ExchangeAnthropicCode.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func BuildAnthropicAuthorizeURL(ep Endpoints, verifier, state string) string {
	q := authorizeQuery(ep, verifier, state)
	q.Set(AnthropicParamShowCode, AnthropicParamShowCodeValue)
	return appendQuery(ep.AuthorizeURL, q)
}

// ExchangeAnthropicCode trades an authorization code for tokens by POSTing a JSON
// body to ep.TokenURL. verifier is the PKCE verifier whose challenge went into
// the authorize URL, and state is the state value from the pasted "code#state"
// string (the caller splits it and checks it against the state it issued; it is
// sent on so a vendor that wants it in the exchange body gets it). code and
// state are whitespace-trimmed as pasted input. ExpiresAt is time.Now() plus the
// response's expires_in. A 401/403 or OAuth invalid_grant answer matches
// ErrAuthRejected.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func ExchangeAnthropicCode(ctx context.Context, httpClient *http.Client, ep Endpoints, code, verifier, state string) (TokenSet, error) {
	return anthropicToken(ctx, httpClient, ep, map[string]string{
		"grant_type":    "authorization_code",
		"code":          strings.TrimSpace(code),
		"state":         strings.TrimSpace(state),
		"redirect_uri":  ep.RedirectURI,
		"client_id":     ep.ClientID,
		"code_verifier": verifier,
	}, "")
}

// RefreshAnthropic exchanges a refresh token for a fresh TokenSet. When the
// vendor does not rotate the refresh token the returned set keeps refreshToken,
// so the result can replace the stored blob as is. A dead refresh token matches
// ErrAuthRejected (the account needs reconnecting).
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func RefreshAnthropic(ctx context.Context, httpClient *http.Client, ep Endpoints, refreshToken string) (TokenSet, error) {
	return anthropicToken(ctx, httpClient, ep, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     ep.ClientID,
	}, refreshToken)
}

// anthropicToken posts payload as JSON and maps the answer to a TokenSet;
// fallbackRefresh is kept when the response issues no new refresh token.
func anthropicToken(ctx context.Context, httpClient *http.Client, ep Endpoints, payload map[string]string, fallbackRefresh string) (TokenSet, error) {
	status, body, err := postJSON(ctx, httpClient, ep.TokenURL, payload)
	if err != nil {
		return TokenSet{}, err
	}
	tr, err := parseTokenResponse(status, body)
	if err != nil {
		return TokenSet{}, err
	}
	refresh := tr.RefreshToken
	if refresh == "" {
		refresh = fallbackRefresh
	}
	return TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: refresh,
		ExpiresAt:    tr.expiresAt(time.Now()),
		Scope:        tr.Scope,
	}, nil
}

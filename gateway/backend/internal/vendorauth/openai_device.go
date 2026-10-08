// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// OpenAI Codex device-code login: the CLI's BESPOKE "deviceauth" protocol, NOT
// the generic RFC 8628 device grant. Reverse-engineered from the Codex CLI source
// (codex-rs/login/src/device_code_auth.rs). It is an OPTIONAL connect method that
// also works for a remote gateway: no browser callback reaches the gateway; the
// gateway polls the vendor until the user has approved the code. Three steps:
//
//  1. OpenAIDeviceStart   -> device_auth_id, user_code, a verification page URL.
//  2. OpenAIDevicePoll    -> one poll; 403/404 is "not yet", a 2xx carries the
//     authorization_code and its PKCE code_verifier. The CALLER loops on the
//     interval so it, not this package, owns the cadence and the overall timeout.
//  3. ExchangeOpenAIDeviceCode -> a normal authorization-code exchange with the
//     deviceauth redirect_uri, yielding a TokenSet.
//
// Every URL is read from Endpoints (see constants.go) so a live operator can
// correct a rotated path and the unit tests can point them at an httptest server.
//
// REVERSE-ENGINEERED / VERIFY LIVE.

// OpenAIDeviceStart begins a device login: it POSTs {"client_id"} to
// ep.DeviceUsercodeURL and reads device_auth_id, user_code (also accepted under
// the "usercode" key) and the poll interval from the JSON answer. The returned
// verificationURL is ep.DeviceVerificationURL, the fixed page the user opens to
// type the code (it is NOT a field of the response). The interval is tolerant of
// a JSON number or a numeric string and defaults to 0 when absent. A response
// missing device_auth_id or user_code, or that is not JSON, is ErrBadTokenResponse;
// a non-2xx answer is a *StatusError that carries no request body.
func OpenAIDeviceStart(ctx context.Context, httpClient *http.Client, ep Endpoints) (deviceAuthID, userCode, verificationURL string, interval time.Duration, err error) {
	status, body, err := postJSON(ctx, httpClient, ep.DeviceUsercodeURL, map[string]string{"client_id": ep.ClientID})
	if err != nil {
		return "", "", "", 0, err
	}
	if status < 200 || status > 299 {
		return "", "", "", 0, newStatusError(status, body)
	}
	var resp struct {
		DeviceAuthID string      `json:"device_auth_id"`
		UserCode     string      `json:"user_code"`
		UserCodeAlt  string      `json:"usercode"`
		Interval     json.Number `json:"interval"`
	}
	if jsonErr := json.Unmarshal(body, &resp); jsonErr != nil {
		return "", "", "", 0, fmt.Errorf("%w: %v", ErrBadTokenResponse, jsonErr)
	}
	userCode = firstNonEmpty(resp.UserCode, resp.UserCodeAlt)
	if resp.DeviceAuthID == "" || userCode == "" {
		return "", "", "", 0, fmt.Errorf("%w: device start missing device_auth_id or user_code", ErrBadTokenResponse)
	}
	return resp.DeviceAuthID, userCode, ep.DeviceVerificationURL, intervalSeconds(resp.Interval), nil
}

// OpenAIDevicePoll performs ONE poll of ep.DeviceTokenURL with
// {device_auth_id, user_code}. HTTP 403 or 404 means the user has not finished
// yet, reported as pending=true with no error (the caller sleeps the interval and
// polls again). A 2xx answer returns the authorization_code and its PKCE
// code_verifier. A 2xx answer missing either is ErrBadTokenResponse; any other
// status is a *StatusError (401/403 matches ErrAuthRejected via Is). Nothing it
// returns on error carries the request body.
func OpenAIDevicePoll(ctx context.Context, httpClient *http.Client, ep Endpoints, deviceAuthID, userCode string) (authorizationCode, codeVerifier string, pending bool, err error) {
	status, body, err := postJSON(ctx, httpClient, ep.DeviceTokenURL, map[string]string{
		"device_auth_id": deviceAuthID,
		"user_code":      userCode,
	})
	if err != nil {
		return "", "", false, err
	}
	if status == http.StatusForbidden || status == http.StatusNotFound {
		return "", "", true, nil
	}
	if status < 200 || status > 299 {
		return "", "", false, newStatusError(status, body)
	}
	// The 2xx answer also carries code_challenge, which this flow does not need
	// (it exchanges the code with the returned code_verifier); it is left unparsed.
	var resp struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	if jsonErr := json.Unmarshal(body, &resp); jsonErr != nil {
		return "", "", false, fmt.Errorf("%w: %v", ErrBadTokenResponse, jsonErr)
	}
	if resp.AuthorizationCode == "" || resp.CodeVerifier == "" {
		return "", "", false, fmt.Errorf("%w: device poll missing authorization_code or code_verifier", ErrBadTokenResponse)
	}
	return resp.AuthorizationCode, resp.CodeVerifier, false, nil
}

// ExchangeOpenAIDeviceCode trades the device authorization code (plus the PKCE
// code_verifier the poll returned) for a TokenSet. It is the standard OpenAI
// authorization-code exchange and differs from ExchangeOpenAICode only in the
// redirect_uri: the deviceauth callback (ep.DeviceCallbackRedirect), not the
// loopback one. The account id and plan are read from the id_token / access token
// as elsewhere; a 401/403 or invalid_grant answer matches ErrAuthRejected.
func ExchangeOpenAIDeviceCode(ctx context.Context, httpClient *http.Client, ep Endpoints, authorizationCode, codeVerifier string) (TokenSet, error) {
	return openAIToken(ctx, httpClient, ep, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {strings.TrimSpace(authorizationCode)},
		"redirect_uri":  {ep.DeviceCallbackRedirect},
		"client_id":     {ep.ClientID},
		"code_verifier": {codeVerifier},
	}, "")
}

// intervalSeconds reads a tolerant poll interval (a JSON number or a numeric
// string) as seconds. An absent, non-numeric or non-positive value is 0, which
// the caller treats as "use its own default".
func intervalSeconds(n json.Number) time.Duration {
	if n == "" {
		return 0
	}
	secs, err := n.Float64()
	if err != nil || secs <= 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

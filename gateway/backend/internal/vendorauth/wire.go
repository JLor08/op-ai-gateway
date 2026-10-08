// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	// ErrAuthRejected marks a vendor refusal of the credentials or grant: HTTP
	// 401/403 from the token endpoint, or an OAuth invalid_grant /
	// invalid_client / unauthorized_client error. For a refresh this means the
	// refresh token is dead (revoked, expired or rotated away) and the account
	// needs reconnecting; transient failures (network, 5xx, 429) never match.
	ErrAuthRejected = errors.New("vendorauth: vendor rejected the credentials")

	// ErrBadTokenResponse marks a 2xx answer from a vendor OAuth endpoint that is not
	// usable: not JSON, or a token response without an access_token.
	ErrBadTokenResponse = errors.New("vendorauth: malformed OAuth response")
)

// StatusError is a non-2xx answer from a vendor OAuth endpoint. It never
// carries the request body or the raw response body, only the HTTP status and
// the (truncated) OAuth error code and description, so it is safe to log.
// errors.Is(err, ErrAuthRejected) reports whether the vendor refused the
// credentials.
type StatusError struct {
	// Status is the HTTP status code.
	Status int
	// Code is the OAuth error code ("invalid_grant", "authorization_pending"),
	// or the error type of an Anthropic-style nested error object; may be empty.
	Code string
	// Description is the vendor's human-readable detail; may be empty.
	Description string
}

func (e *StatusError) Error() string {
	msg := fmt.Sprintf("vendorauth: token endpoint returned HTTP %d", e.Status)
	if e.Code != "" {
		msg += ": " + e.Code
	}
	if e.Description != "" {
		msg += fmt.Sprintf(" (%q)", e.Description)
	}
	return msg
}

// Is makes errors.Is(err, ErrAuthRejected) true for credential refusals.
func (e *StatusError) Is(target error) bool {
	if target != ErrAuthRejected {
		return false
	}
	switch e.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	switch e.Code {
	case "invalid_grant", "invalid_client", "unauthorized_client":
		return true
	}
	return false
}

const (
	// maxResponseBytes caps what is read from a vendor endpoint; real token
	// responses are a few KiB.
	maxResponseBytes = 1 << 20
	// maxErrorText truncates a vendor-supplied error description.
	maxErrorText = 200
)

// defaultHTTPClient backs a nil *http.Client so the flows never use a client
// without a timeout.
var defaultHTTPClient = &http.Client{Timeout: 30 * time.Second}

// authorizeQuery returns the standard OAuth 2.0 + PKCE authorization-request
// parameters for ep; the vendor builders add their own extras on top.
func authorizeQuery(ep Endpoints, verifier, state string) url.Values {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", ep.ClientID)
	q.Set("redirect_uri", ep.RedirectURI)
	q.Set("scope", ep.Scopes)
	q.Set("code_challenge", PKCEChallenge(verifier))
	q.Set("code_challenge_method", PKCEMethodS256)
	q.Set("state", state)
	return q
}

// appendQuery adds q to base, which may already carry a query string.
func appendQuery(base string, q url.Values) string {
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// postJSON sends payload as a JSON body and returns the status and body.
func postJSON(ctx context.Context, c *http.Client, endpoint string, payload any) (int, []byte, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("vendorauth: encode request: %w", err)
	}
	return post(ctx, c, endpoint, "application/json", bytes.NewReader(raw))
}

func post(ctx context.Context, c *http.Client, endpoint, contentType string, body io.Reader) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("vendorauth: build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	return send(c, req)
}

// send performs req with c (a nil c falls back to defaultHTTPClient) and returns
// the status and the body, capped at maxResponseBytes.
func send(c *http.Client, req *http.Request) (int, []byte, error) {
	return sendCapped(c, req, maxResponseBytes)
}

// sendCapped is send with an explicit body limit: at most limit bytes of the body
// are read, and a longer body is cut off there (so a caller that parses it sees a
// truncated document). send's limit is maxResponseBytes; a response that
// legitimately outgrows it, such as a model catalog, asks for its own.
func sendCapped(c *http.Client, req *http.Request, limit int64) (int, []byte, error) {
	if c == nil {
		c = defaultHTTPClient
	}
	resp, err := c.Do(req)
	if err != nil {
		// *url.Error names the method and URL, never the request body.
		return 0, nil, fmt.Errorf("vendorauth: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return 0, nil, fmt.Errorf("vendorauth: read response: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// newStatusError builds a StatusError from a non-2xx response. It understands
// the standard OAuth shape {"error":"code","error_description":"..."} and the
// Anthropic nested shape {"error":{"type":"code","message":"..."}}; anything
// else leaves Code and Description empty.
func newStatusError(status int, body []byte) *StatusError {
	se := &StatusError{Status: status}
	var env struct {
		Error            json.RawMessage `json:"error"`
		ErrorDescription string          `json:"error_description"`
		Message          string          `json:"message"`
	}
	if json.Unmarshal(body, &env) != nil {
		return se
	}
	var code string
	if json.Unmarshal(env.Error, &code) == nil {
		se.Code = code
	} else {
		var nested struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(env.Error, &nested) == nil {
			se.Code = nested.Type
			if se.Code == "" {
				se.Code = nested.Code
			}
			se.Description = nested.Message
		}
	}
	if env.ErrorDescription != "" {
		se.Description = env.ErrorDescription
	}
	if se.Description == "" {
		se.Description = env.Message
	}
	se.Code = clip(se.Code)
	se.Description = clip(se.Description)
	return se
}

func clip(s string) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) > maxErrorText {
		return s[:maxErrorText]
	}
	return s
}

// tokenResponse is the union of the token-endpoint fields both vendors return.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	Scope        string `json:"scope"`
	// ExpiresIn is seconds; json.Number tolerates a number or a numeric string.
	ExpiresIn json.Number `json:"expires_in"`
}

// parseTokenResponse turns a token-endpoint answer into a tokenResponse: a
// non-2xx status becomes a *StatusError, an unusable 2xx body ErrBadTokenResponse.
func parseTokenResponse(status int, body []byte) (tokenResponse, error) {
	if status < 200 || status > 299 {
		return tokenResponse{}, newStatusError(status, body)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return tokenResponse{}, fmt.Errorf("%w: %v", ErrBadTokenResponse, err)
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, fmt.Errorf("%w: no access_token", ErrBadTokenResponse)
	}
	return tr, nil
}

// expiresAt converts a relative expires_in (seconds) into an absolute time; an
// absent or non-positive value leaves the expiry unknown (zero).
func (tr tokenResponse) expiresAt(now time.Time) time.Time {
	secs, err := tr.ExpiresIn.Float64()
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(secs * float64(time.Second)))
}

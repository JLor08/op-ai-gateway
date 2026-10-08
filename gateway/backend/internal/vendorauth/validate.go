// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// This file holds the model-independent credential validation probes: one cheap
// GET per credential kind that tells a working credential from a dead one without
// naming a model, so a wrong catalog entry can never masquerade as an auth
// failure. The URLs and the two subscription probes' REVERSE-ENGINEERED status
// live in constants.go (VERIFY LIVE).
//
// Classification is deliberately uniform and conservative: HTTP 2xx means Valid,
// HTTP 401 means Invalid, and everything else (403, 404, 408, 429, 5xx, a
// redirect, a timeout, any transport error) means Unverifiable, so an unreachable
// or confused vendor warns but is never reported as a bad credential. The one
// exception is the Anthropic SUBSCRIPTION probe, where 403 also means Valid: a
// token minted by `claude setup-token` has scope user:inference but not
// user:profile and so legitimately gets a 403 from the profile endpoint.
//
// None of the probes ever logs or returns the credential. CredentialCheck.Detail
// is a fixed status phrase plus, when the response body carries one, the vendor's
// short error code or type; the vendor's free-text message is never echoed (it
// may quote the key), and neither is any transport error text.

// CredentialStatus is the verdict of one validation probe.
type CredentialStatus int

// The zero CredentialStatus is deliberately none of these, so a CredentialCheck
// that was never filled in cannot read as a verdict.
const (
	// StatusValid means the vendor accepted the credential.
	StatusValid CredentialStatus = iota + 1
	// StatusInvalid means the vendor definitively rejected the credential (HTTP 401).
	StatusInvalid
	// StatusUnverifiable means the probe could not reach a clean verdict (vendor
	// unreachable, rate limited, an unexpected answer). It is NOT a statement
	// about the credential and must never be treated as one.
	StatusUnverifiable
)

// String returns "valid", "invalid" or "unverifiable" ("unknown" for a value that
// is none of them).
func (s CredentialStatus) String() string {
	switch s {
	case StatusValid:
		return "valid"
	case StatusInvalid:
		return "invalid"
	case StatusUnverifiable:
		return "unverifiable"
	default:
		return "unknown"
	}
}

// CredentialCheck is the outcome of a validation probe.
type CredentialCheck struct {
	// Status is the verdict.
	Status CredentialStatus
	// Detail is a short human-readable status phrase, for example "credential
	// rejected (HTTP 401: invalid_api_key)". It never contains the credential.
	Detail string
	// AccountID and PlanType are filled ONLY by ValidateOpenAISubscription on an
	// accepted credential, and only when the response names them: the ChatGPT
	// account the token belongs to (the value sent back as chatgpt-account-id) and
	// its plan. A caller whose pasted token carried no chatgpt_account_id claim can
	// backfill it from here.
	AccountID string
	PlanType  string
}

const (
	// maxErrorCodeLen bounds a vendor error code echoed into Detail.
	maxErrorCodeLen = 64

	phraseAccepted        = "credential accepted"
	phraseAcceptedNoScope = "credential accepted, profile scope not granted"
	phraseRejected        = "credential rejected"
	phraseUnverifiable    = "could not verify"
)

// ValidateOpenAISubscription checks a ChatGPT (Codex) subscription access token
// with GET OpenAIAccountsCheckURL. On acceptance it also returns the account id
// (default_account_id, falling back to the first accounts[].id) and the matching
// plan_type; a body that does not parse never changes the verdict, it just leaves
// those fields empty.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func ValidateOpenAISubscription(ctx context.Context, httpClient *http.Client, accessToken string) CredentialCheck {
	check, body := runProbe(ctx, httpClient, probeSpec{
		url:     OpenAIAccountsCheckURL,
		headers: map[string]string{"Authorization": "Bearer " + accessToken},
		secret:  accessToken,
	})
	if check.Status == StatusValid {
		check.AccountID, check.PlanType = parseAccountsCheck(body)
	}
	return check
}

// ValidateAnthropicSubscription checks a Claude (Pro/Max) subscription access
// token with GET AnthropicOAuthProfileURL. HTTP 403 counts as Valid: a
// `claude setup-token` token lacks the user:profile scope this endpoint wants but
// is a working credential.
//
// REVERSE-ENGINEERED / VERIFY LIVE.
func ValidateAnthropicSubscription(ctx context.Context, httpClient *http.Client, accessToken string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url: AnthropicOAuthProfileURL,
		headers: map[string]string{
			"Authorization":     "Bearer " + accessToken,
			"anthropic-beta":    AnthropicBeta,
			"anthropic-version": AnthropicAPIVersion,
		},
		secret:           accessToken,
		forbiddenIsValid: true,
	})
	return check
}

// ValidateOpenAIAPIKey checks an OpenAI API key with GET OpenAIModelsURL.
func ValidateOpenAIAPIKey(ctx context.Context, httpClient *http.Client, apiKey string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url:     OpenAIModelsURL,
		headers: map[string]string{"Authorization": "Bearer " + apiKey},
		secret:  apiKey,
	})
	return check
}

// ValidateAnthropicAPIKey checks an Anthropic API key with GET AnthropicModelsURL.
func ValidateAnthropicAPIKey(ctx context.Context, httpClient *http.Client, apiKey string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url: AnthropicModelsURL,
		headers: map[string]string{
			"x-api-key":         apiKey,
			"anthropic-version": AnthropicAPIVersion,
		},
		secret: apiKey,
	})
	return check
}

// probeSpec describes one validation GET.
type probeSpec struct {
	url     string
	headers map[string]string
	// secret is the credential in headers; it is never allowed into a Detail.
	secret string
	// forbiddenIsValid makes HTTP 403 mean Valid (the Anthropic subscription probe).
	forbiddenIsValid bool
}

// runProbe sends the GET and classifies the answer. It also returns the response
// body for a caller that reads more than the verdict from it (nil on a transport
// failure). The caller's httpClient owns the timeout: it is not changed here, and
// a nil client falls back to defaultHTTPClient (30s).
func runProbe(ctx context.Context, httpClient *http.Client, p probeSpec) (CredentialCheck, []byte) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		// The error text is not echoed: a fixed phrase is all a Detail may carry.
		return unverifiable(phraseUnverifiable + ": request could not be built"), nil
	}
	req.Header.Set("Accept", "application/json")
	for name, value := range p.headers {
		req.Header.Set(name, value)
	}
	status, body, err := send(withoutRedirects(httpClient), req)
	if err != nil {
		return unverifiable(transportPhrase(err)), nil
	}
	return classify(status, body, p), body
}

// withoutRedirects returns a copy of c that returns a 3xx answer instead of
// following it. net/http strips only Authorization (not x-api-key) when a
// redirect leaves the host, so following one could carry the credential to
// another place; none of these endpoints redirect, so a 3xx is an unexpected
// answer and ends up Unverifiable. The caller's client is left untouched.
func withoutRedirects(c *http.Client) *http.Client {
	if c == nil {
		c = defaultHTTPClient
	}
	cc := *c
	cc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cc
}

func unverifiable(detail string) CredentialCheck {
	return CredentialCheck{Status: StatusUnverifiable, Detail: detail}
}

// transportPhrase maps a failure to reach the vendor to a fixed phrase. The
// error's own text is dropped on purpose.
func transportPhrase(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return phraseUnverifiable + ": request canceled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return phraseUnverifiable + ": request timed out"
	default:
		return phraseUnverifiable + ": request to the vendor failed"
	}
}

// classify applies the uniform status rule from the file comment.
func classify(httpStatus int, body []byte, p probeSpec) CredentialCheck {
	switch {
	case httpStatus >= http.StatusOK && httpStatus < http.StatusMultipleChoices:
		return CredentialCheck{Status: StatusValid, Detail: describe(phraseAccepted, httpStatus, "", p.secret)}
	case httpStatus == http.StatusUnauthorized:
		return CredentialCheck{Status: StatusInvalid, Detail: describe(phraseRejected, httpStatus, vendorErrorCode(body), p.secret)}
	case httpStatus == http.StatusForbidden && p.forbiddenIsValid:
		return CredentialCheck{Status: StatusValid, Detail: describe(phraseAcceptedNoScope, httpStatus, vendorErrorCode(body), p.secret)}
	default:
		return CredentialCheck{Status: StatusUnverifiable, Detail: describe(phraseUnverifiable, httpStatus, vendorErrorCode(body), p.secret)}
	}
}

// describe builds a Detail: the phrase, the HTTP status and, when present, the
// vendor error code. A code that contains the credential is dropped.
func describe(phrase string, httpStatus int, code, secret string) string {
	if code != "" && secret != "" && strings.Contains(code, secret) {
		code = ""
	}
	if code == "" {
		return fmt.Sprintf("%s (HTTP %d)", phrase, httpStatus)
	}
	return fmt.Sprintf("%s (HTTP %d: %s)", phrase, httpStatus, code)
}

// vendorErrorCode extracts the vendor's short error code from a non-2xx body,
// or "" when there is none. It understands the OpenAI shape
// {"error":{"code":"invalid_api_key","type":"..."}} (code preferred over type),
// the Anthropic shape {"error":{"type":"authentication_error"}} and the OAuth
// shape {"error":"invalid_token"}. Only a short identifier-like token is
// accepted: free text (a message that may quote the key) yields "".
func vendorErrorCode(body []byte) string {
	var env struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil || len(env.Error) == 0 {
		return ""
	}
	var asString string
	if json.Unmarshal(env.Error, &asString) == nil {
		if isErrorCode(asString) {
			return asString
		}
		return ""
	}
	var obj map[string]any
	if json.Unmarshal(env.Error, &obj) != nil {
		return ""
	}
	for _, key := range []string{"code", "type"} {
		if s, ok := obj[key].(string); ok && isErrorCode(s) {
			return s
		}
	}
	return ""
}

// isErrorCode reports whether s looks like a machine error code
// (invalid_api_key, authentication_error, rate_limit_exceeded): non-empty, at
// most maxErrorCodeLen long, and made only of letters, digits and _ . : -.
func isErrorCode(s string) bool {
	if s == "" || len(s) > maxErrorCodeLen {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '.', r == ':', r == '-':
		default:
			return false
		}
	}
	return true
}

// parseAccountsCheck reads the ChatGPT account id and plan from an accounts/check
// body: default_account_id (falling back to the first accounts[].id) and the
// plan_type of that account. Best effort: a body that is not JSON, or has
// fields of the wrong type, yields whatever decoded and otherwise empty strings.
func parseAccountsCheck(body []byte) (accountID, planType string) {
	var resp struct {
		DefaultAccountID string `json:"default_account_id"`
		Accounts         []struct {
			ID       string `json:"id"`
			PlanType string `json:"plan_type"`
		} `json:"accounts"`
	}
	// A decode error (wrong field type, not JSON) is deliberately ignored:
	// json.Unmarshal keeps every field it could decode, and the verdict never
	// depends on this body.
	_ = json.Unmarshal(body, &resp)

	accountID = resp.DefaultAccountID
	if accountID == "" && len(resp.Accounts) > 0 {
		accountID = resp.Accounts[0].ID
	}
	if accountID == "" {
		return "", ""
	}
	for _, a := range resp.Accounts {
		if a.ID == accountID {
			return accountID, a.PlanType
		}
	}
	return accountID, ""
}

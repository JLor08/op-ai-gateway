// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/capture"
	"strings"
	"time"
)

// TokenSet is the credential blob stored (sealed) for a subscription vendor
// account. It is what the OAuth flows return and what token refresh replaces.
type TokenSet struct {
	// AccessToken is the bearer token presented to the vendor API.
	AccessToken string `json:"access_token"`
	// RefreshToken re-issues the access token; may be empty when the vendor did
	// not issue one (a pasted access-only token).
	RefreshToken string `json:"refresh_token,omitempty"`
	// ExpiresAt is when AccessToken stops working; the zero time means unknown.
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	// AccountID is the vendor-side account identifier (OpenAI: the id_token's
	// chatgpt_account_id, sent back as the chatgpt-account-id header).
	AccountID string `json:"account_id,omitempty"`
	// PlanType is the subscription plan the vendor reports (OpenAI:
	// chatgpt_plan_type, for example "plus" or "pro").
	PlanType string `json:"plan_type,omitempty"`
	// Scope is the space-separated scope list the vendor granted, if reported.
	Scope string `json:"scope,omitempty"`
}

// NeedsRefresh reports whether the access token has expired or will within
// buffer of now. A zero ExpiresAt (unknown expiry) never needs refresh: there
// is nothing to compare, and refreshing blindly on every use would burn the
// refresh token.
func (ts TokenSet) NeedsRefresh(now time.Time, buffer time.Duration) bool {
	return !ts.ExpiresAt.IsZero() && now.Add(buffer).After(ts.ExpiresAt)
}

const (
	// jwtClaimExpiry is the registered "exp" claim (RFC 7519 section 4.1.4): the
	// expiry as a NumericDate, seconds since the Unix epoch.
	jwtClaimExpiry = "exp"
	// maxJWTExpiry is 9999-12-31T23:59:59Z; a larger "exp" is not a date.
	maxJWTExpiry = 253402300799
)

// AccessTokenExpiry reads the expiry of a JWT access token from its numeric
// "exp" claim, without verifying the signature (like OpenAIClaimsFromJWT, it is
// used to schedule a refresh, never to authenticate anyone). It returns the
// expiry in UTC, or the zero time and false when the token is not a JWT or its
// exp is missing, not a number, not positive or not a plausible date. A
// pasted access token (an import) carries no expires_in, so this lets
// TokenSet.NeedsRefresh fire for it. Anthropic access tokens are opaque, not
// JWTs, and so always yield false.
func AccessTokenExpiry(token string) (time.Time, bool) {
	claims, ok := jwtClaims(token)
	if !ok {
		return time.Time{}, false
	}
	exp, _ := claims[jwtClaimExpiry].(float64)
	if exp <= 0 || exp > maxJWTExpiry {
		return time.Time{}, false
	}
	return time.Unix(int64(exp), 0).UTC(), true
}

// jwtClaims decodes the payload (claims) segment of a JWT without verifying its
// signature. Tolerant by design: anything that is not three dot-separated
// segments around a base64url JSON object yields (nil, false), never an error or
// a panic.
func jwtClaims(token string) (map[string]any, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return nil, false
	}
	var claims map[string]any
	if json.Unmarshal(payload, &claims) != nil || claims == nil {
		return nil, false
	}
	return claims, true
}

// String redacts the secrets so a stray %v / %s of a TokenSet cannot spill a
// bearer or refresh token into a log. (TokenSet deliberately has no MarshalJSON:
// sealing needs the real fields; structured loggers are covered by LogValue.)
func (ts TokenSet) String() string {
	return fmt.Sprintf("TokenSet{access:%s refresh:%s expires_at:%s account_id:%q plan_type:%q scope:%q}",
		redacted(ts.AccessToken), redacted(ts.RefreshToken), ts.ExpiresAt.Format(time.RFC3339),
		ts.AccountID, ts.PlanType, ts.Scope)
}

// GoString makes %#v redact the same way String does.
func (ts TokenSet) GoString() string { return ts.String() }

// LogValue implements slog.LogValuer so a log/slog handler (JSON or text) logs
// the redacted summary instead of reflecting over the real token fields.
func (ts TokenSet) LogValue() slog.Value { return slog.StringValue(ts.String()) }

func redacted(secret string) string {
	if secret == "" {
		return "unset"
	}
	return "[redacted]"
}

// SealTokenSet JSON-encodes ts and seals it with capture.SealSecret, so it
// follows the repository's at-rest scheme: "enc:" with a cipher, "plain:" on a
// volatile (RAM-only) store, and capture.ErrKeyRequired (never plaintext) on a
// disk store without a key.
func SealTokenSet(cipher *capture.Cipher, volatile bool, ts TokenSet) (string, error) {
	raw, err := json.Marshal(ts)
	if err != nil {
		return "", fmt.Errorf("vendorauth: encode token set: %w", err)
	}
	sealed, err := capture.SealSecret(cipher, volatile, string(raw))
	if err != nil {
		return "", fmt.Errorf("vendorauth: seal token set: %w", err)
	}
	return sealed, nil
}

// OpenTokenSet reverses SealTokenSet. The empty string (no tokens stored yet)
// yields the zero TokenSet and a nil error; any failure returns the zero
// TokenSet so a half-decoded credential is never used.
func OpenTokenSet(cipher *capture.Cipher, sealed string) (TokenSet, error) {
	if sealed == "" {
		return TokenSet{}, nil
	}
	raw, err := capture.OpenSecret(cipher, sealed)
	if err != nil {
		return TokenSet{}, fmt.Errorf("vendorauth: open token set: %w", err)
	}
	var ts TokenSet
	if err := json.Unmarshal([]byte(raw), &ts); err != nil {
		return TokenSet{}, fmt.Errorf("vendorauth: decode token set: %w", err)
	}
	return ts, nil
}

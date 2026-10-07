// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"op-ai-gateway/internal/capture"
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

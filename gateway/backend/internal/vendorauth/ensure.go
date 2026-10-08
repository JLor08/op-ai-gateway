// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
	"time"
)

// EnsureFresh returns a usable access token for an Anthropic subscription
// account, refreshing it only when it is at or within buffer of its expiry.
//
//   - When ts does not need refreshing (NeedsRefresh is false — a token with a
//     comfortable expiry, or an unknown/zero expiry which never refreshes), it
//     returns (ts, false, nil): the stored blob is handed back untouched and the
//     caller persists nothing.
//   - Otherwise it exchanges ts.RefreshToken for a fresh TokenSet via
//     RefreshAnthropic and returns (fresh, true, nil). The refresh response only
//     carries access/refresh/expiry/scope, so AccountID and PlanType are carried
//     FORWARD from the old ts whenever the response omits them — a refresh must
//     not blank the account id a vendor needs on later requests.
//   - A refresh failure returns the error unchanged; a dead refresh token surfaces
//     as ErrAuthRejected (via *StatusError.Is), which tells the caller to mark the
//     account needs_reconnect. Transient failures (network, 5xx, 429) do NOT match
//     ErrAuthRejected, so the caller leaves the account active and retries later.
func EnsureFresh(ctx context.Context, httpClient *http.Client, ep Endpoints, ts TokenSet, buffer time.Duration) (TokenSet, bool, error) {
	return ensureFresh(ts, buffer, func(refreshToken string) (TokenSet, error) {
		return RefreshAnthropic(ctx, httpClient, ep, refreshToken)
	})
}

// EnsureFreshOpenAI is the OpenAI (Codex ChatGPT-subscription) analogue of
// EnsureFresh (Milestone 5b): same staleness rule and same identity-carry-forward
// contract, but it exchanges the refresh token through RefreshOpenAI (form-encoded
// body) against the OpenAI token endpoint. Carrying AccountID/PlanType forward
// matters here too: a refresh response without an id_token leaves RefreshOpenAI's
// AccountID empty, and the chatgpt-account-id the dispatch sends on every request
// must survive a refresh.
//
// CRITICAL: never call this with Anthropic endpoints (or EnsureFresh with OpenAI
// endpoints) — an OpenAI refresh token must not be sent to the Anthropic token
// endpoint and vice versa.
func EnsureFreshOpenAI(ctx context.Context, httpClient *http.Client, ep Endpoints, ts TokenSet, buffer time.Duration) (TokenSet, bool, error) {
	return ensureFresh(ts, buffer, func(refreshToken string) (TokenSet, error) {
		return RefreshOpenAI(ctx, httpClient, ep, refreshToken)
	})
}

// ensureFresh is the vendor-agnostic core shared by EnsureFresh and
// EnsureFreshOpenAI: a non-stale token is returned untouched (no refresh call),
// and a stale one is exchanged through refresh, with the vendor-side identity
// fields (AccountID/PlanType) carried forward from the old ts whenever the refresh
// response omits them. The only thing that varies between vendors is the refresh
// func, so that is all this takes.
func ensureFresh(ts TokenSet, buffer time.Duration, refresh func(refreshToken string) (TokenSet, error)) (TokenSet, bool, error) {
	if !ts.NeedsRefresh(time.Now(), buffer) {
		return ts, false, nil
	}
	fresh, err := refresh(ts.RefreshToken)
	if err != nil {
		return TokenSet{}, false, err
	}
	// Carry the vendor-side identity fields forward: a refresh response that omits
	// them would otherwise strip the account id (and plan) the stored blob has held
	// since connect.
	if fresh.AccountID == "" {
		fresh.AccountID = ts.AccountID
	}
	if fresh.PlanType == "" {
		fresh.PlanType = ts.PlanType
	}
	return fresh, true, nil
}

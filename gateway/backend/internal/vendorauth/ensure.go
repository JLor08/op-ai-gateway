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
//
// The OpenAI subscription variant is Milestone 5b; this function is Anthropic-only.
func EnsureFresh(ctx context.Context, httpClient *http.Client, ep Endpoints, ts TokenSet, buffer time.Duration) (TokenSet, bool, error) {
	if !ts.NeedsRefresh(time.Now(), buffer) {
		return ts, false, nil
	}
	fresh, err := RefreshAnthropic(ctx, httpClient, ep, ts.RefreshToken)
	if err != nil {
		return TokenSet{}, false, err
	}
	// Carry the vendor-side identity fields forward: RefreshAnthropic leaves them
	// empty, and losing them would strip the account id (and plan) the stored blob
	// has held since connect.
	if fresh.AccountID == "" {
		fresh.AccountID = ts.AccountID
	}
	if fresh.PlanType == "" {
		fresh.PlanType = ts.PlanType
	}
	return fresh, true, nil
}

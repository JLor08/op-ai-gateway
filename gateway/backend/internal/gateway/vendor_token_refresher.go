// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"errors"
)

// errVendorSubscriptionRefreshFailed is the portal-facing outcome of a refresh that
// delivered no usable token. It is a fixed, credential-free message: the cause (a
// rejected refresh token, an unreachable vendor, an unreadable sealed set) is
// logged by resolveSubscriptionBearer, which also flips a rejected account to
// needs_reconnect.
var errVendorSubscriptionRefreshFailed = errors.New("the subscription access token could not be refreshed")

// RefreshVendorSubscriptionTokens renews the OAuth access token of the
// subscription account accountID when it is at or near expiry, and persists the
// renewed token set. It is the portal's handle on the gateway's own locked refresh
// (portal.ServiceDeps.VendorTokenRefresher, wired by cmd/gateway through
// portal.Service.SetVendorTokenRefresher): the model discovery uses it to get a
// usable token for an expired-but-refreshable account without ever refreshing by
// itself, which could race the dispatch for the single-use refresh token. It is
// resolveSubscriptionBearer, so it runs under the same per-account lock, refreshes
// against the account's own vendor endpoint and flips a rejected account to
// needs_reconnect; the bearer it resolves is discarded, the persisted token set is
// the result. A token that is not near expiry is left as it is.
//
// A nil error says the refresh produced a bearer, not that it was persisted (a
// reseal or store failure is logged and the bearer served for that request only):
// the caller re-reads the account to see what is stored. Any failure to produce a
// bearer is errVendorSubscriptionRefreshFailed, which names no credential.
func (s *Server) RefreshVendorSubscriptionTokens(ctx context.Context, accountID string) error {
	if access, _, ok := s.resolveSubscriptionBearer(ctx, accountID); !ok || access == "" {
		return errVendorSubscriptionRefreshFailed
	}
	return nil
}

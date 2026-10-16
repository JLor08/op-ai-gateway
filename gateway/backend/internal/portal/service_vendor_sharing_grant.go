// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"log/slog"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"sort"
)

// VendorAccountsForRouting is the routing-time GRANT of shared vendor accounts (#183,
// spec 2026-10-15 section 8): the vendor accounts the principal may route to, in the
// order the resolver serves them. It is the concrete routing.VendorAccountEnumerator
// (the gateway adapter delegates here through the portal.API interface) and the one
// source the model listing is built on, so what is advertised is what is routable.
//
// The answer is the principal's OWN accounts, emitted FIRST in id order, then the
// SHARED accounts, in id order. An account is available iff it is owned, or
//
//   - it is `shared` or `full` by its EFFECTIVE level (effectiveVendorAccountShareLevel),
//     read live from the row in this very call, so a downgrade, a pause (the master
//     flag off: effective `none`) or a raise never waits on anything: the link's
//     existence is NECESSARY, never sufficient, and a `none` account whose resource-group
//     link survived (a torn downgrade) grants nothing;
//   - a SUBSCRIPTION account additionally needs vendor_subscription_sharing_enabled
//     (link creation does not look at that flag, so this is the only place it bites);
//   - AND it is linked to a resource group the principal is PROVISIONED into
//     (provisionedRGsForPrincipal: a service matches only a direct service provision,
//     a user matches its user, user-group and admin-group provisions).
//
// This is NOT the server model. It does not read resource_provisioning_enforce and has
// no open pool: an account in no group, or in a group nobody is provisioned into, is
// reachable by its owner alone.
//
// One fail direction: the owner's own accounts are ALWAYS returned, and only the
// shared arm fails closed. A store error while computing the shared candidates denies
// them and still returns the own accounts with no error (unlike AllowedServerIDs,
// which fails open), so routing and the listing agree even in the error window. Only a
// failure to read the principal's OWN accounts is reported, as the error it is: the
// caller (the resolver) fails the request as it always has, and nothing shared leaks.
//
// It decides REACH only. Whether a token may use a shared account (the per-account
// opt-in on the token) and which accounts a principal MANAGES are other questions,
// asked elsewhere.
func (s *Service) VendorAccountsForRouting(ctx context.Context, principal auth.Token) ([]routing.VendorAccount, error) {
	own, err := s.ownVendorAccountsForRouting(ctx, principal)
	if err != nil {
		return nil, err
	}
	shared, err := s.sharedVendorAccountsForRouting(ctx, principal, own)
	if err != nil {
		// Fail closed on the shared arm ONLY: deny the shared candidates, keep own.
		slog.WarnContext(ctx, "shared vendor accounts denied: the shared candidates could not be computed; only the principal's own accounts are routable", "err", err)
		return own, nil
	}
	return append(own, shared...), nil
}

// ownVendorAccountsForRouting is the own block: every account the principal owns,
// whatever its level, id-sorted (the store orders by id). A principal with no user
// identity (a service) owns nothing and costs no store read.
func (s *Service) ownVendorAccountsForRouting(ctx context.Context, principal auth.Token) ([]routing.VendorAccount, error) {
	if principal.UserID == "" {
		return nil, nil
	}
	return s.routes.VendorAccountsByOwner(ctx, principal.UserID)
}

// sharedVendorAccountsForRouting is the shared arm: the accounts linked to a resource
// group the principal is provisioned into that are routable by the live-level
// predicate (vendorAccountSharedRoutable), id-sorted, excluding the accounts own
// already holds (an account the principal owns is listed once, as their own).
//
// With the master sharing flag off it is empty and is not even computed: sharing is
// off in most deployments and this runs on every routed request. The flag is read
// once for that shortcut and again, through effectiveVendorAccountShareLevel, per
// account; a flip between the two can only ever DENY (the per-account read answers
// `none`), never grant. Both reads, and the subscription flag's, go through the 5 s
// invalidatable cache of the sharing switches (vendorAccountSharingEnabledCached,
// service_vendor_sharing_flags.go), so this hot path costs no settings SELECT of its
// own, sharing on or off.
func (s *Service) sharedVendorAccountsForRouting(ctx context.Context, principal auth.Token, own []routing.VendorAccount) ([]routing.VendorAccount, error) {
	if !s.vendorAccountSharingEnabledCached(ctx) {
		return nil, nil
	}
	rgIDs, err := s.provisionedRGsForPrincipal(ctx, principal)
	if err != nil {
		return nil, err
	}
	candidateIDs, err := s.vendorAccountIDsInResourceGroups(ctx, rgIDs, own)
	if err != nil {
		return nil, err
	}
	subscriptionSharing := s.vendorSubscriptionSharingEnabledCached(ctx)
	shared := make([]routing.VendorAccount, 0, len(candidateIDs))
	for _, id := range candidateIDs {
		acc, err := s.routes.VendorAccountByID(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Deleted between the link read and this read: nothing to grant.
				continue
			}
			return nil, err
		}
		if s.vendorAccountSharedRoutable(ctx, acc, subscriptionSharing) {
			shared = append(shared, acc)
		}
	}
	return shared, nil
}

// vendorAccountIDsInResourceGroups returns the ids of every vendor account linked to
// any of rgIDs, deduplicated (an account linked into two granted groups is one
// candidate), without the accounts in skip, in id order. It reads LINKS only: every
// candidate still has to pass the live-level predicate.
func (s *Service) vendorAccountIDsInResourceGroups(ctx context.Context, rgIDs map[string]bool, skip []routing.VendorAccount) ([]string, error) {
	seen := make(map[string]bool, len(skip))
	for _, acc := range skip {
		seen[acc.ID] = true
	}
	var ids []string
	for rgID := range rgIDs {
		linked, err := s.routes.ResourceGroupVendorAccounts(ctx, rgID)
		if err != nil {
			return nil, err
		}
		for _, id := range linked {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// vendorAccountSharedRoutable is the live predicate of the shared arm: acc is
// `shared` or `full` by its effective level, and, if it is not an api_key account,
// subscription sharing is on. Anything that is not an api_key account is treated as
// a subscription (deny by default: an unrecognised auth type must not slip past the
// flag), mirroring checkVendorAccountSubscriptionRaise.
func (s *Service) vendorAccountSharedRoutable(ctx context.Context, acc routing.VendorAccount, subscriptionSharing bool) bool {
	if !vendorAccountSharedLevel(s.effectiveVendorAccountShareLevel(ctx, acc)) {
		return false
	}
	return acc.AuthType == routing.VendorAuthAPIKey || subscriptionSharing
}

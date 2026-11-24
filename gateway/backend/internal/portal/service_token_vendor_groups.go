// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"slices"
	"strings"
)

// claimVendorAccessGroups is the group half of validateVendorAccess (spec 5.1). Every
// listed group entry must name a group the principal owns (a foreign, unknown or
// blank id is the same "invalid", no existence leak), be listed once and carry a valid
// override prefix; under all + include_groups an entry naming an own group is
// covered by the switch and, like a covered account entry, ignored.
//
// The names are then claimed per opted-in group: the policy is rendered as the token
// it will become and run through advertisedVendorGroupModels, the listing's own group
// step, so a group claims exactly prefix + upstream over its usable members' models
// and the save-time check cannot drift from what is advertised and routed. Members
// are recorded so that a clash between a group and one of its own members is not a
// conflict (the group shadows it); any other shared name is.
func (s *Service) claimVendorAccessGroups(ctx context.Context, principal auth.Token, byID map[string]routing.VendorAccount, d *VendorAccessDTO, check *vendorAccessCheck) error {
	covered := d.All && d.IncludeGroups
	entries := make([]auth.VendorAccessGroupEntry, 0, len(d.Groups))
	seen := make(map[string]struct{}, len(d.Groups))
	for _, e := range d.Groups {
		id := strings.TrimSpace(e.GroupID)
		g, err := s.routes.VendorAccountGroupByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && (principal.UserID == "" || g.OwnerUserID != principal.UserID)) {
			return fmt.Errorf("%w: unknown group %q", ErrTokenVendorAccessInvalid, id)
		}
		if err != nil {
			return err
		}
		if covered {
			continue
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("%w: group %q listed more than once", ErrTokenVendorAccessInvalid, id)
		}
		seen[id] = struct{}{}
		entry := auth.VendorAccessGroupEntry{GroupID: id}
		if e.PrefixOverride != nil && e.PrefixOverride.Enabled {
			prefix, err := normalizeVendorAccountModelPrefix(e.PrefixOverride.Value)
			if err != nil {
				return fmt.Errorf("%w: group %q: %w", ErrTokenVendorAccessInvalid, id, err)
			}
			entry.OverrideEnabled, entry.OverridePrefix = true, prefix
		}
		entries = append(entries, entry)
	}
	check.subscriptionsOn = s.VendorSubscriptionsEnabled(ctx)
	if len(entries) == 0 && !covered {
		return nil
	}
	token := auth.Token{UserID: principal.UserID, VendorAccess: auth.VendorAccess{
		All: d.All, IncludeGroups: d.IncludeGroups, Groups: entries,
	}}
	claims, loaded := s.advertisedVendorGroupModels(ctx, token, slices.Collect(maps.Values(byID)), check.subscriptionsOn)
	if !loaded {
		slog.Warn("portal: token vendor-access group collision check ran on an incomplete group listing; the routing backstop still applies")
	}
	for _, c := range claims {
		if check.groupMembers[c.GroupID] == nil {
			check.groupMembers[c.GroupID] = make(map[string]struct{})
		}
		check.groupMembers[c.GroupID][c.Account.ID] = struct{}{}
	}
	for _, c := range claims {
		for _, m := range c.Models {
			if err := check.claim(m.GatewayModel, c.GroupID); err != nil {
				return err
			}
			check.groupNames[m.GatewayModel] = c.GroupID
		}
	}
	return nil
}

// checkOwnAccountsAgainstGroups is the backstop of `all + include_groups`: all makes
// every active own account of the principal an advertised source under its own
// prefix, next to the groups the switch pulls in, so an own account that is NOT a
// member of a group yet serves one of the group's names is a conflict (group save
// already prevents it). Own accounts clashing with each other stay as tolerated under
// all as ever; only the clash with a group is looked at.
func (s *Service) checkOwnAccountsAgainstGroups(ctx context.Context, principal auth.Token, check *vendorAccessCheck) error {
	if len(check.groupNames) == 0 {
		return nil
	}
	ids := make([]string, 0, len(check.byID))
	for id, acc := range check.byID {
		if routing.TokenOwnsVendorAccount(principal, acc) && acc.Status == routing.VendorAccountStatusActive &&
			(acc.AuthType != routing.VendorAuthSubscription || check.subscriptionsOn) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		acc := check.byID[id]
		models, err := s.routes.VendorAccountModels(ctx, id)
		if err != nil {
			slog.Warn("portal: token vendor-access collision check skipped for an account whose models could not be read; the routing backstop still applies",
				"account_id", id, "error", err)
			continue
		}
		for _, m := range models {
			name := acc.ModelPrefix + m.UpstreamModel
			if gid, hit := check.groupNames[name]; hit && !check.shadows(gid, id) {
				return fmt.Errorf("%w: %q is served by both %q and %q", ErrTokenVendorAccessConflict, name, gid, id)
			}
		}
	}
	return nil
}

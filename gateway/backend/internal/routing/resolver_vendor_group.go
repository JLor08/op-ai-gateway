// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import (
	"context"
	"fmt"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/inference"
	"slices"
	"strings"
	"time"
)

// VendorUsagePuller asks for a fresh usage snapshot of a vendor account. The
// implementation throttles; the resolver may call it on every request that
// skips a member for a limit whose reset it does not know. The resolver calls it
// synchronously on the resolve path, so an implementation MUST NOT block: the
// throttle check must be cheap and any vendor call must run asynchronously.
type VendorUsagePuller interface {
	RequestVendorUsagePull(accountID string)
}

// resolveVendorGroup is the group step of resolveVendorAccount: it runs before
// the single-account loop, so a group shadows its own members' names. For each
// of the token's own active groups (id order) whose prefix starts the request
// model it builds one candidate per member, in member order (never relying on
// stored position values), and lets SelectVendorGroupMember pick. A group with
// no usable candidate (every member unreachable, disabled, without the model, or
// excluded, or all blocked while fallback is off) yields no match and the next
// group, then the account loop, runs. reachable is the grant's answer
// (routableVendorAccounts), so a revoked share drops its member here.
//
// The listing (portal advertisedVendorAccountModels) uses the same group and
// member rules, minus thresholds and cooldowns, so advertised names stay
// routable.
func (r *Resolver) resolveVendorGroup(ctx context.Context, token auth.Token, req inference.Request, apiFlavor string, reachable []VendorAccount) (Target, bool, error) {
	if token.UserID == "" {
		return Target{}, false, nil
	}
	groups, err := r.store.VendorAccountGroupsByOwner(ctx, token.UserID)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor groups: %w", err)
	}
	if len(groups) == 0 {
		return Target{}, false, nil
	}
	byID := make(map[string]VendorAccount, len(reachable))
	for _, acc := range reachable {
		byID[acc.ID] = acc
	}
	decision := isVendorDecisionRequest(req, apiFlavor)
	now := r.clock()
	for _, g := range groups {
		if g.Status != VendorAccountGroupStatusActive {
			continue
		}
		prefix, allowed := TokenVendorGroupPrefix(token, g)
		if !allowed || !strings.HasPrefix(req.Model, prefix) {
			continue
		}
		upstream := strings.TrimPrefix(req.Model, prefix)
		t, ok, err := r.resolveVendorGroupMember(ctx, g, upstream, req, apiFlavor, decision, byID, now)
		if err != nil || ok {
			return t, ok, err
		}
	}
	return Target{}, false, nil
}

func (r *Resolver) resolveVendorGroupMember(ctx context.Context, g VendorAccountGroup, upstream string, req inference.Request, apiFlavor string, decision bool, reachable map[string]VendorAccount, now time.Time) (Target, bool, error) {
	members, err := r.store.VendorAccountGroupMembers(ctx, g.ID)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor group members: %w", err)
	}
	cands := make([]VendorGroupCandidate, len(members))
	targets := make([]Target, len(members))
	for i, m := range members {
		cands[i].AccountID = m.AccountID
		acc, ok := reachable[m.AccountID]
		if !ok || acc.Vendor != g.Vendor || !r.vendorAccountUsable(acc, apiFlavor, decision) ||
			slices.Contains(req.ExcludeVendorAccountIDs, acc.ID) {
			continue
		}
		models, err := r.store.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			return Target{}, false, fmt.Errorf("resolve vendor group member models: %w", err)
		}
		for _, row := range models {
			if row.UpstreamModel != upstream {
				continue
			}
			if t, ok := vendorAccountModelTarget(acc, row, req, apiFlavor); ok {
				targets[i] = t
				cands[i].Usable = true
			}
			break
		}
		if !cands[i].Usable {
			continue
		}
		cands[i].ThresholdsApply = acc.OwnerUserID == g.OwnerUserID || acc.ShareUsageWithMembers
		if u, ok, err := r.store.VendorAccountUsageByID(ctx, acc.ID); err != nil {
			return Target{}, false, fmt.Errorf("resolve vendor group member usage: %w", err)
		} else if ok {
			cands[i].Usage, cands[i].HasUsage = u, true
		}
		if until, ok := r.vendorCooldowns.Until(acc.ID, upstream, now); ok {
			cands[i].CooldownUntil = until
		}
	}
	th := g.Thresholds()
	r.requestVendorUsagePulls(cands, th, now)
	idx, ok := SelectVendorGroupMember(cands, th, now, len(req.ExcludeVendorAccountIDs) == 0)
	if !ok {
		return Target{}, false, nil
	}
	t := targets[idx]
	t.VendorGroupID = g.ID
	return t, true, nil
}

// requestVendorUsagePulls asks the puller for a fresh snapshot of every usable
// member blocked by a threshold whose reset time is unknown: without it the
// block would never be known to lift.
func (r *Resolver) requestVendorUsagePulls(cands []VendorGroupCandidate, th VendorGroupThresholds, now time.Time) {
	if r.vendorPuller == nil {
		return
	}
	for _, c := range cands {
		if !c.Usable {
			continue
		}
		for _, b := range VendorGroupMemberBlocks(c, th, now) {
			if b.Category != VendorBlockCooldown && b.FreeAt == nil {
				r.vendorPuller.RequestVendorUsagePull(c.AccountID)
				break
			}
		}
	}
}

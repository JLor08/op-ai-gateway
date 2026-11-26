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
// (routableVendorAccounts), so a revoked share drops its member here. A token that
// can use no group (TokenUsesVendorGroups) returns at once without reading the
// groups, so a failing group read never fails a resolve that never needed groups.
//
// The listing (portal advertisedVendorAccountModels) uses the same group and
// member rules, minus thresholds and cooldowns, so advertised names stay
// routable.
func (r *Resolver) resolveVendorGroup(ctx context.Context, token auth.Token, req inference.Request, apiFlavor string, reachable []VendorAccount) (Target, bool, error) {
	if !TokenUsesVendorGroups(token) {
		return Target{}, false, nil
	}
	groups, err := r.store.VendorAccountGroupsByOwner(ctx, token.UserID)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor groups: %w", err)
	}
	if len(groups) == 0 {
		return Target{}, false, nil
	}
	vr := vendorGroupRequest{
		req: req, apiFlavor: apiFlavor, decision: isVendorDecisionRequest(req, apiFlavor),
		reachable: make(map[string]VendorAccount, len(reachable)), now: r.clock(),
	}
	for _, acc := range reachable {
		vr.reachable[acc.ID] = acc
	}
	for _, g := range groups {
		if g.Status != VendorAccountGroupStatusActive {
			continue
		}
		prefix, allowed := TokenVendorGroupPrefix(token, g)
		if !allowed || !strings.HasPrefix(req.Model, prefix) {
			continue
		}
		t, ok, err := r.resolveVendorGroupMember(ctx, g, strings.TrimPrefix(req.Model, prefix), vr)
		if err != nil || ok {
			return t, ok, err
		}
	}
	return Target{}, false, nil
}

// vendorGroupRequest is what the group step resolves one request against: the
// request, its coarse flavor and whether it is a decision request
// (isVendorDecisionRequest), the grant's reachable accounts by id, and the resolve's
// clock reading.
type vendorGroupRequest struct {
	req       inference.Request
	apiFlavor string
	decision  bool
	reachable map[string]VendorAccount
	now       time.Time
}

// resolveVendorGroupMember builds one candidate per member of g, in member order,
// for the request's upstream model and lets SelectVendorGroupMember pick; the
// all-blocked fallback is off on a failover pass (a non-empty exclusion list). No
// pick is (Target{}, false, nil): the group does not serve the request.
func (r *Resolver) resolveVendorGroupMember(ctx context.Context, g VendorAccountGroup, upstream string, vr vendorGroupRequest) (Target, bool, error) {
	members, err := r.store.VendorAccountGroupMembers(ctx, g.ID)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor group members: %w", err)
	}
	cands := make([]VendorGroupCandidate, len(members))
	targets := make([]Target, len(members))
	for i, m := range members {
		if cands[i], targets[i], err = r.vendorGroupCandidate(ctx, g, m.AccountID, upstream, vr); err != nil {
			return Target{}, false, err
		}
	}
	th := g.Thresholds()
	r.requestVendorUsagePulls(cands, th, vr.now)
	idx, ok := SelectVendorGroupMember(cands, th, vr.now, len(vr.req.ExcludeVendorAccountIDs) == 0)
	if !ok {
		return Target{}, false, nil
	}
	t := targets[idx]
	t.VendorGroupID = g.ID
	return t, true, nil
}

// vendorGroupCandidate is member accountID of g as the selector sees it, with the
// target it would serve the request on. The member is Usable only when it passes
// spec 5.3 rules 1 to 3 -- reachable, of the group's vendor, usable for the request
// (vendorAccountUsable), not excluded, and serving (upstream, dialect); a usable
// member then carries its usage snapshot and its cooldown for upstream.
func (r *Resolver) vendorGroupCandidate(ctx context.Context, g VendorAccountGroup, accountID, upstream string, vr vendorGroupRequest) (VendorGroupCandidate, Target, error) {
	cand := VendorGroupCandidate{AccountID: accountID}
	acc, ok := vr.reachable[accountID]
	if !ok || acc.Vendor != g.Vendor || !r.vendorAccountUsable(acc, vr.apiFlavor, vr.decision) ||
		slices.Contains(vr.req.ExcludeVendorAccountIDs, acc.ID) {
		return cand, Target{}, nil
	}
	t, serves, err := r.vendorGroupMemberTarget(ctx, acc, upstream, vr)
	if err != nil || !serves {
		return cand, Target{}, err
	}
	cand.Usable = true
	cand.ThresholdsApply = acc.OwnerUserID == g.OwnerUserID || acc.ShareUsageWithMembers
	if u, found, err := r.store.VendorAccountUsageByID(ctx, acc.ID); err != nil {
		return cand, Target{}, fmt.Errorf("resolve vendor group member usage: %w", err)
	} else if found {
		cand.Usage, cand.HasUsage = u, true
	}
	if until, cooling := r.vendorCooldowns.Until(acc.ID, upstream, vr.now); cooling {
		cand.CooldownUntil = until
	}
	return cand, t, nil
}

// vendorGroupMemberTarget is the target member acc serves the request's upstream
// model on, from its model rows (the account path's target builder); false when it
// does not serve that model on the request's dialect.
func (r *Resolver) vendorGroupMemberTarget(ctx context.Context, acc VendorAccount, upstream string, vr vendorGroupRequest) (Target, bool, error) {
	models, err := r.store.VendorAccountModels(ctx, acc.ID)
	if err != nil {
		return Target{}, false, fmt.Errorf("resolve vendor group member models: %w", err)
	}
	for _, row := range models {
		if row.UpstreamModel == upstream {
			t, ok := vendorAccountModelTarget(acc, row, vr.req, vr.apiFlavor)
			return t, ok, nil
		}
	}
	return Target{}, false, nil
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

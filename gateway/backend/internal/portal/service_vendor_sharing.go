// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package portal

import (
	"context"
	"errors"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"strings"
)

// The share-level lifecycle of a vendor account (spec 2026-10-15 sections 6, 7, 11
// and 13): raising an account none -> shared -> full and lowering it again.
//
//   - RAISE (RaiseVendorAccountShareLevel) is owner + a CURRENT admin role, on an
//     owned account, and always carries >= 1 management group.
//   - LOWER (LowerVendorAccountShareLevel) is the CREDENTIAL gate's shape: the owner
//     always, a CM at `full` only.
//   - SetVendorAccountShareLevel is the endpoint's single entry: it compares the
//     requested level with the stored one and runs the one or the other.
//
// Every one of them is refused with ErrVendorAccountSharingDisabled while the master
// sharing flag is off; that guard (requireVendorAccountSharingEnabled) is shared with
// the management-group and resource-group methods that follow.

// SetVendorAccountShareLevelRequest is the body of PUT
// /api/portal/vendor-accounts/{id}/share-level. Level is the TARGET level (none,
// shared or full). AdminGroupIDs are the management groups: required (>= 1) when the
// request RAISES the account, ignored when it lowers it.
type SetVendorAccountShareLevelRequest struct {
	Level         string   `json:"level"`
	AdminGroupIDs []string `json:"admin_group_ids"`
}

// vendorShareLevelRank orders the share levels: none < shared < full. A level the
// service does not know (a corrupt row) ranks with `none`: private, fail closed.
func vendorShareLevelRank(level string) int {
	switch level {
	case routing.VendorShareLevelFull:
		return 2
	case routing.VendorShareLevelShared:
		return 1
	}
	return 0
}

// normalizeVendorAccountShareLevel trims a requested level and checks it is one of
// none, shared or full (case-sensitive, like every other enumerated field here).
func normalizeVendorAccountShareLevel(raw string) (string, error) {
	switch level := strings.TrimSpace(raw); level {
	case routing.VendorShareLevelNone, routing.VendorShareLevelShared, routing.VendorShareLevelFull:
		return level, nil
	default:
		return "", ErrVendorAccountShareLevelInvalid
	}
}

// SetVendorAccountShareLevel moves an account to req.Level: a request for a level
// ABOVE the stored one is RaiseVendorAccountShareLevel, anything else (a lower level,
// or the level the account already has) is LowerVendorAccountShareLevel. Both run
// their own authorization against the account they load themselves, so a level
// that changed between this method's read and theirs can only make a call fail
// (ErrVendorAccountShareLevelUnchanged), never run under the wrong gate.
//
// The order is: the sharing flags (global, so they reveal nothing about an account),
// then the level's shape (account-independent, so a 400 reveals nothing either), then
// the account. A stranger and an unknown id are both ErrVendorAccountNotFound.
func (s *Service) SetVendorAccountShareLevel(ctx context.Context, principal auth.Token, id string, req SetVendorAccountShareLevelRequest) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountSharingEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	target, err := normalizeVendorAccountShareLevel(req.Level)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if vendorShareLevelRank(target) > vendorShareLevelRank(acc.ShareLevel) {
		return s.RaiseVendorAccountShareLevel(ctx, principal, id, target, req.AdminGroupIDs)
	}
	return s.LowerVendorAccountShareLevel(ctx, principal, id, target)
}

// RaiseVendorAccountShareLevel raises an account to `shared` or `full` (spec
// sections 6 and 7). It needs, in this order:
//
//  1. vendor_account_sharing_enabled (ErrVendorAccountSharingDisabled, a 409);
//  2. the OWNER, who must be an admin RIGHT NOW (authorizeVendorAccountRaise): a
//     non-owner is ErrVendorAccountNotFound unless they can already see the account (a
//     CM), who is ErrVendorAccountForbidden; an owner who is not an admin is
//     ErrVendorAccountForbidden. System scope is a stranger, as for every write;
//  3. a target level above the stored one (else ErrVendorAccountShareLevelUnchanged);
//  4. for a SUBSCRIPTION account (anything not api_key), the cap and the flag: never
//     `full` (ErrVendorAccountSubscriptionShareCapped), and only while
//     vendor_account_sharing_enabled AND vendor_subscription_sharing_enabled are on
//     (ErrVendorSubscriptionSharingDisabled);
//  5. >= 1 management group (validateVendorAccountAdminGroupIDs): each an existing
//     admin-tier group, for a non-system caller one they manage vendor accounts
//     through, all under one parent, which is the containment root. While the account
//     is already shared the root is immutable: groups under another parent are
//     ErrVendorAccountRootMismatch.
//
// The carried group set REPLACES the account's stored one (additions first, so the
// account never has zero groups mid-write). The writes are ordered so that every
// failure leaves the account PRIVATE or unchanged, never half-shared: root, then
// groups, then the share level LAST. Until the level moves, every gate and every
// listing reads the account as `none`, so a link or a root left behind by a failed
// raise grants nothing and is overwritten by the retry.
//
// Returns the account's credential-free view.
func (s *Service) RaiseVendorAccountShareLevel(ctx context.Context, principal auth.Token, id, level string, adminGroupIDs []string) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountSharingEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if err := s.authorizeVendorAccountRaise(ctx, principal, acc); err != nil {
		return VendorAccountDTO{}, err
	}
	target, err := normalizeVendorAccountShareLevel(level)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	current := vendorShareLevelRank(acc.ShareLevel)
	if vendorShareLevelRank(target) <= current {
		return VendorAccountDTO{}, ErrVendorAccountShareLevelUnchanged
	}
	if err := s.checkVendorAccountSubscriptionRaise(ctx, acc, target); err != nil {
		return VendorAccountDTO{}, err
	}
	groupIDs, rootID, err := s.validateVendorAccountAdminGroupIDs(ctx, principal, adminGroupIDs)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if rootID == "" {
		// An admin group always has a parent system group; a root-less account would
		// escape the containment checks every later step relies on.
		return VendorAccountDTO{}, ErrVendorAccountAdminGroupInvalid
	}
	if current > 0 && acc.SystemGroupID != "" && rootID != acc.SystemGroupID {
		return VendorAccountDTO{}, ErrVendorAccountRootMismatch
	}
	if rootID != acc.SystemGroupID {
		if err := s.routes.UpdateVendorAccountSystemGroup(ctx, acc.ID, rootID); err != nil {
			return VendorAccountDTO{}, notFoundAsVendorAccountNotFound(err)
		}
	}
	if err := s.replaceVendorAccountAdminGroups(ctx, acc.ID, groupIDs); err != nil {
		return VendorAccountDTO{}, err
	}
	if err := s.routes.UpdateVendorAccountShareLevel(ctx, acc.ID, target); err != nil {
		return VendorAccountDTO{}, notFoundAsVendorAccountNotFound(err)
	}
	return s.vendorAccountDTOByID(ctx, acc.ID)
}

// LowerVendorAccountShareLevel lowers an account to `shared` (from `full`) or to
// `none` (spec sections 6 and 7). It is the CREDENTIAL gate's row: the owner always --
// ownership is all it takes, so an owner can un-share after losing the admin role --
// and a CM only at `full`; a CM of a `shared` account is ErrVendorAccountForbidden,
// everyone else ErrVendorAccountNotFound. It deliberately does NOT look at the
// subscription flag: an account can always be un-shared.
//
// Lowering to `none` is ClearVendorAccountSharing: in one transaction the level goes
// to none FIRST, then every management-group link and resource-group link is deleted
// and the root cleared. It always runs, even for an account that already reads
// `none`, so it is idempotent and also scrubs a leftover link. Lowering full ->
// shared changes only the level; the groups, the root and the links stay.
//
// A target that is not below the stored level (and is not `none`) is
// ErrVendorAccountShareLevelUnchanged. Like the raise, ErrVendorAccountSharingDisabled
// while the sharing flag is off.
func (s *Service) LowerVendorAccountShareLevel(ctx context.Context, principal auth.Token, id, level string) (VendorAccountDTO, error) {
	if err := s.requireVendorAccountSharingEnabled(ctx); err != nil {
		return VendorAccountDTO{}, err
	}
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	if err := s.authorizeVendorAccountCredential(ctx, principal, acc); err != nil {
		return VendorAccountDTO{}, err
	}
	target, err := normalizeVendorAccountShareLevel(level)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	switch {
	case target == routing.VendorShareLevelNone:
		err = s.routes.ClearVendorAccountSharing(ctx, acc.ID)
	case vendorShareLevelRank(target) < vendorShareLevelRank(acc.ShareLevel):
		err = s.routes.UpdateVendorAccountShareLevel(ctx, acc.ID, target)
	default:
		return VendorAccountDTO{}, ErrVendorAccountShareLevelUnchanged
	}
	if err != nil {
		return VendorAccountDTO{}, notFoundAsVendorAccountNotFound(err)
	}
	return s.vendorAccountDTOByID(ctx, acc.ID)
}

// authorizeVendorAccountRaise is the gate of RAISING a level: the owner, who must be
// an admin right now (spec 6: raise is owner+admin, not the operate or credential
// gate). The two denials differ on purpose, exactly like the credential gate's:
//
//   - a caller who can already SEE the account (a CM of a shared/full account) but is
//     not its owner, and an owner who is no longer an admin, get
//     ErrVendorAccountForbidden (403): the account is no secret to them;
//   - everyone else (a stranger, system scope, a co-manager without the flag, a
//     principal with no user identity) gets ErrVendorAccountNotFound (404-no-leak),
//     identical to an unknown id.
func (s *Service) authorizeVendorAccountRaise(ctx context.Context, principal auth.Token, acc routing.VendorAccount) error {
	if !isVendorAccountOwner(principal, acc) {
		if err := s.authorizeVendorAccountOperate(ctx, principal, acc); err != nil {
			return err
		}
		return ErrVendorAccountForbidden
	}
	admin, err := s.principalIsCurrentAdmin(ctx, principal)
	if err != nil {
		return err
	}
	if !admin {
		return ErrVendorAccountForbidden
	}
	return nil
}

// principalIsCurrentAdmin reports whether principal is an admin RIGHT NOW, on two
// independent counts that must both hold: the token carries the admin scope (so an
// inference-only bearer of an admin cannot manage anything) AND the user's CURRENT
// role in the store is admin or system_admin (so a token minted before a demotion
// stops working at once). A user who no longer exists is no admin.
func (s *Service) principalIsCurrentAdmin(ctx context.Context, principal auth.Token) (bool, error) {
	if principal.UserID == "" || !isAdmin(principal) {
		return false, nil
	}
	user, err := s.users.UserByID(ctx, principal.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return isAdminRole(user.Role), nil
}

// checkVendorAccountSubscriptionRaise applies spec section 11 to a raise: a
// SUBSCRIPTION account (the OAuth token set is the credential) may be raised to
// `shared` only, never `full`, and only while vendor_account_sharing_enabled AND
// vendor_subscription_sharing_enabled are both on. Anything that is not an api_key
// account is treated as a subscription (deny by default: an unrecognised auth type
// must not slip past the cap). The cap is a property of the request and answers
// before the flag, which is a property of the system.
func (s *Service) checkVendorAccountSubscriptionRaise(ctx context.Context, acc routing.VendorAccount, target string) error {
	if acc.AuthType == routing.VendorAuthAPIKey {
		return nil
	}
	if target == routing.VendorShareLevelFull {
		return ErrVendorAccountSubscriptionShareCapped
	}
	return s.requireVendorSubscriptionSharingEnabled(ctx)
}

// validateVendorAccountAdminGroupIDs validates the management-group set of a vendor
// account: the vendor-account counterpart of validateAdminGroupIDs (server) and
// validateServiceAdminGroupIDs, over the same validateAdminGroupScope. >= 1 id after
// trimming and de-duplicating (ErrVendorAccountAdminGroupRequired); each an existing
// admin-tier group (ErrVendorAccountAdminGroupInvalid) which, for a non-system
// principal, they manage vendor accounts through (vendorAccountManageGroupIDs); all
// under one parent (ErrVendorAccountAdminGroupParentMismatch), which is returned as
// the containment root. A Service with no group store can validate nothing, so every
// group is invalid.
func (s *Service) validateVendorAccountAdminGroupIDs(ctx context.Context, principal auth.Token, rawIDs []string) ([]string, string, error) {
	if s.groups == nil {
		for _, raw := range rawIDs {
			if strings.TrimSpace(raw) != "" {
				return nil, "", ErrVendorAccountAdminGroupInvalid
			}
		}
		return nil, "", ErrVendorAccountAdminGroupRequired
	}
	return s.validateAdminGroupScope(ctx, principal, rawIDs, "", s.vendorAccountManageGroupIDs, adminGroupSentinels{
		Required:       ErrVendorAccountAdminGroupRequired,
		Invalid:        ErrVendorAccountAdminGroupInvalid,
		ParentMismatch: ErrVendorAccountAdminGroupParentMismatch,
	})
}

// replaceVendorAccountAdminGroups makes want (already validated: >= 1 id, no
// duplicates) the account's whole management-group set: the missing links are added
// FIRST and the surplus ones removed after, so the account has at least one group at
// every step (mirrors SetServerAdminGroups). It is the one write path of the
// management-group set; the later assignment endpoint reuses it.
func (s *Service) replaceVendorAccountAdminGroups(ctx context.Context, accountID string, want []string) error {
	current, err := s.routes.VendorAccountAdminGroups(ctx, accountID)
	if err != nil {
		return notFoundAsVendorAccountNotFound(err)
	}
	currentSet := make(map[string]bool, len(current))
	for _, gid := range current {
		currentSet[gid] = true
	}
	wantSet := make(map[string]bool, len(want))
	for _, gid := range want {
		wantSet[gid] = true
		if currentSet[gid] {
			continue
		}
		if err := s.routes.SetVendorAccountAdminGroup(ctx, accountID, gid); err != nil {
			return notFoundAsVendorAccountNotFound(err)
		}
	}
	for _, gid := range current {
		if wantSet[gid] {
			continue
		}
		if err := s.routes.RemoveVendorAccountAdminGroup(ctx, accountID, gid); err != nil {
			return notFoundAsVendorAccountNotFound(err)
		}
	}
	return nil
}

// vendorAccountDTOByID re-reads the account (the write may have moved columns the
// caller's copy does not have) and renders its credential-free view.
func (s *Service) vendorAccountDTOByID(ctx context.Context, id string) (VendorAccountDTO, error) {
	acc, err := s.loadVendorAccount(ctx, id)
	if err != nil {
		return VendorAccountDTO{}, err
	}
	return s.vendorAccountDTO(ctx, acc)
}

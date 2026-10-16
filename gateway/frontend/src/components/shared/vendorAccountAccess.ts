// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { VendorAccount, VendorAccountShareLevel } from '../../api';

// What the viewer of a vendor account may be OFFERED, derived from the DTO the
// backend rendered for them (spec 2026-10-15 sections 6 and 10). The backend is
// the gate -- it answers 403 for every operation below that the viewer may not do
// -- so this only keeps the portal from inviting a request that is certain to be
// refused. It mirrors the spec's authorization table:
//
//   | operation                          | none  | shared         | full           |
//   | operate (rename, status, prefix)   | owner | owner + CM     | owner + CM     |
//   | test / refresh / usage pull        | owner | owner + CM (1) | owner + CM (1) |
//   | credential (connect, key, delete)  | owner | owner          | owner + CM     |
//   | management groups                  | -     | owner          | owner + CM     |
//   | raise                              | owner + admin (until full)                |
//   | lower                              | -     | owner          | owner + CM     |
//
//   (1) a SUBSCRIPTION account: owner only -- the three open the owner's single-use
//   OAuth token set and can rotate it (spec 11).
//
// There are three viewers. The DTO carries no "is a co-manager" bit and does not
// need one: `is_owner` is the owner; a non-owner who is handed a share level
// (`shared` / `full`) reads the management view, which only a co-manager (or a
// system-scope reader) gets; and a MEMBER, who can only route to the account
// through a resource group, reads the minimal view whose share_level is withheld
// ("", not `none`). Anything that is not a known level on somebody else's account
// is therefore a member, so a level this build does not know can only widen
// nothing (deny by default).

export type VendorAccountTier = 'owner' | 'manager' | 'member';

export type VendorAccountAccess = {
  tier: VendorAccountTier;
  // The stored level; `none` for a member, whose level is withheld.
  level: VendorAccountShareLevel;
  // Rename, status and model prefix (the PATCH's operate gate).
  canOperate: boolean;
  // Connect / reconnect, the api key, delete (the credential gate).
  canManageCredential: boolean;
  // Connection test, models refresh, usage refresh and the on-view usage pull: the
  // calls that open the stored credential. Owner-only on a subscription account.
  canRunUpstreamOps: boolean;
  // Replace the management groups.
  canEditAdminGroups: boolean;
  // The levels the viewer may raise the account to, lowest first. Never `full` for a
  // subscription account.
  raiseTargets: Exclude<VendorAccountShareLevel, 'none'>[];
  // The levels the viewer may lower the account to, highest first.
  lowerTargets: Exclude<VendorAccountShareLevel, 'full'>[];
  // Whether the share area has anything to show: the level and its actions.
  showSharePanel: boolean;
};

function isShareLevel(level: string): level is VendorAccountShareLevel {
  return level === 'none' || level === 'shared' || level === 'full';
}

// A member reaches nothing of the account's management. A function, not a shared
// constant, so no caller can mutate the arrays another one reads.
function memberAccess(): VendorAccountAccess {
  return {
    tier: 'member',
    level: 'none',
    canOperate: false,
    canManageCredential: false,
    canRunUpstreamOps: false,
    canEditAdminGroups: false,
    raiseTargets: [],
    lowerTargets: [],
    showSharePanel: false,
  };
}

// The levels an owner-admin may raise `account` to (empty for anyone else). Only an
// owner who is an admin may raise; a subscription account is shared at most as
// `shared`, never `full`.
function vendorAccountRaiseTargets(
  owner: boolean,
  viewerIsAdmin: boolean,
  level: VendorAccountShareLevel,
  full: boolean,
  apiKey: boolean,
): VendorAccountAccess['raiseTargets'] {
  if (!owner || !viewerIsAdmin) return [];
  const targets: VendorAccountAccess['raiseTargets'] = [];
  if (level === 'none') targets.push('shared');
  if (!full && apiKey) targets.push('full');
  return targets;
}

// The levels the viewer may lower `account` to, highest first. Lowering needs only
// ownership at `shared` (an owner must be able to un-share after losing the admin
// role) and ownership or co-management at `full`.
function vendorAccountLowerTargets(
  owner: boolean,
  shared: boolean,
  full: boolean,
): VendorAccountAccess['lowerTargets'] {
  if (!(owner ? shared : full)) return [];
  const targets: VendorAccountAccess['lowerTargets'] = [];
  if (full) targets.push('shared');
  targets.push('none');
  return targets;
}

/**
 * What `viewerIsAdmin` (the portal user's CURRENT admin or system_admin role) may do
 * with `account`. Only an owner who is an admin may raise the level; lowering needs
 * only ownership at `shared` (an owner must be able to un-share after losing the
 * admin role) and ownership or co-management at `full`.
 */
export function vendorAccountAccess(
  account: Pick<VendorAccount, 'auth_type' | 'share_level' | 'is_owner'>,
  viewerIsAdmin: boolean,
): VendorAccountAccess {
  const owner = account.is_owner;
  if (!owner && !isShareLevel(account.share_level)) return memberAccess();

  const level: VendorAccountShareLevel = isShareLevel(account.share_level)
    ? account.share_level
    : 'none';
  const shared = level === 'shared' || level === 'full';
  const full = level === 'full';
  const apiKey = account.auth_type === 'api_key';

  const raiseTargets = vendorAccountRaiseTargets(owner, viewerIsAdmin, level, full, apiKey);
  const lowerTargets = vendorAccountLowerTargets(owner, shared, full);

  return {
    tier: owner ? 'owner' : 'manager',
    level,
    canOperate: owner || shared,
    canManageCredential: owner || full,
    canRunUpstreamOps: (owner || shared) && (owner || apiKey),
    canEditAdminGroups: (owner && shared) || full,
    raiseTargets,
    lowerTargets,
    showSharePanel: raiseTargets.length > 0 || shared,
  };
}

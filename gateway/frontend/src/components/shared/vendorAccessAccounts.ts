// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { VendorAccessAccountDTO, VendorAccount } from '../../api';
import type { Translation } from './types';
import { isValidModelPrefix, normalizeModelPrefix } from './vendorInputs';

// What a token's per-account vendor opt-in (`vendor_provider_access`) may OFFER,
// shared by the user-token form (TokenList) and the service-token create form
// (ServiceTokensSection). The policy names accounts by id; an OWN account is
// covered by `all`, a SHARED one only by its explicit per-account entry (spec
// 2026-10-15 sections 2.3 and 8).
//
// The account list api.vendorAccounts() is own ∪ managed ∪ member, rendered FOR the
// viewer, and a row cannot say whether the TOKEN's principal can route to the
// account: a co-manager who is also provisioned into the account's resource group
// reads the management view (the widest tier), the same row a co-manager who is
// not reads. So the rule below declines only what is CERTAINLY not reachable (an
// account that is shared with nobody) and offers the rest; the save-time check
// (an unreachable id is a 400 portal.token_vendor_access_invalid) and the routing
// grant, re-evaluated on every request, remain the gate.

export type VendorTokenPrincipal = 'user' | 'service';

type Offerable = Pick<VendorAccount, 'share_level' | 'is_owner'>;

/**
 * Splits the viewer's account list into what a token of `principal` may opt into:
 * `own` (the accounts the token's own user owns, which `all` covers) and `shared`
 * (everything else that could be reached through a resource group). List order is
 * kept.
 *
 *   - user token: the principal is the owning user, so an own account is `own`
 *     whatever its level, and anybody else's account is `shared` unless its level
 *     is `none` (a member's minimal row withholds the level, which is not `none`).
 *   - service token: a service owns nothing, so there is no `own`; every account is
 *     `shared` for it, even the creator's own, but an account at level `none` is
 *     shared with nobody and grants a service nothing.
 */
export function offerableVendorAccounts<T extends Offerable>(
  accounts: readonly T[],
  principal: VendorTokenPrincipal,
): { own: T[]; shared: T[] } {
  const own: T[] = [];
  const shared: T[] = [];
  for (const acc of accounts) {
    if (principal === 'user' && acc.is_owner) own.push(acc);
    else if (acc.share_level !== 'none') shared.push(acc);
  }
  return { own, shared };
}

/**
 * The label of an account in the opt-in lists. A shared account carries its owner's
 * name, which tells two same-named accounts of different owners apart; the name is
 * owner-supplied text and is only ever rendered as text.
 */
export function vendorAccountOptionLabel(
  acc: Pick<VendorAccount, 'name' | 'is_owner' | 'shared_by'>,
  t: Translation,
): string {
  return acc.is_owner || !acc.shared_by
    ? acc.name
    : `${acc.name} (${t.vendorAccountSharedBy} ${acc.shared_by})`;
}

// One vendor account (by its stable id) a token is opted in to, plus the form state
// of its optional prefix override. The override is ON only while overrideEnabled;
// prefix is then the value that replaces the account's own model prefix for this
// token ("" = no prefix, the bare original model names).
export type VendorAccessEntryState = {
  accountId: string;
  overrideEnabled: boolean;
  prefix: string;
};

/** The form state of one stored policy entry; an override counts only when enabled. */
export function vendorAccessEntryState(entry: VendorAccessAccountDTO): VendorAccessEntryState {
  return {
    accountId: entry.account_id,
    overrideEnabled: entry.prefix_override?.enabled === true,
    prefix: entry.prefix_override?.value ?? '',
  };
}

/**
 * The wire entry of a form entry: an override that is off omits prefix_override
 * entirely (never enabled:false), an enabled one sends its trimmed value ("" = no
 * prefix).
 */
export function vendorAccessEntryDTO(entry: VendorAccessEntryState): VendorAccessAccountDTO {
  return {
    account_id: entry.accountId,
    ...(entry.overrideEnabled
      ? { prefix_override: { enabled: true, value: normalizeModelPrefix(entry.prefix) } }
      : {}),
  };
}

/** An enabled override with a malformed value blocks the save; a disabled one is never sent. */
export function vendorAccessEntryInvalid(entry: VendorAccessEntryState): boolean {
  return entry.overrideEnabled && !isValidModelPrefix(entry.prefix);
}

/** Adds the account with its override off, or removes it when it is already in. */
export function toggleVendorAccessEntry(
  entries: readonly VendorAccessEntryState[],
  accountId: string,
): VendorAccessEntryState[] {
  return entries.some((entry) => entry.accountId === accountId)
    ? entries.filter((entry) => entry.accountId !== accountId)
    : [...entries, { accountId, overrideEnabled: false, prefix: '' }];
}

/** Patches the one entry of `accountId`. */
export function patchVendorAccessEntry(
  entries: readonly VendorAccessEntryState[],
  accountId: string,
  patch: Partial<VendorAccessEntryState>,
): VendorAccessEntryState[] {
  return entries.map((entry) => (entry.accountId === accountId ? { ...entry, ...patch } : entry));
}

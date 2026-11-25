// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { Box, Checkbox, FormControlLabel, Typography } from '@mui/material';
import type { VendorAccessDTO, VendorAccount, VendorAccountGroup } from '../../api';
import type { Translation } from './types';
import { CheckboxGroup } from './CheckboxGroup';
import { VendorAccessEntryGroups } from './VendorAccessEntryGroups';
import {
  offerableVendorAccounts,
  patchVendorAccessEntry,
  toggleVendorAccessEntry,
  vendorAccessEntryDTO,
  vendorAccessEntryInvalid,
  vendorAccessEntryState,
  vendorAccessGroupEntryDTO,
  vendorAccessGroupEntryState,
  vendorAccountOptionLabel,
  vendorGroupOptionLabel,
  type VendorAccessEntryState,
} from './vendorAccessAccounts';

// The provider-access ("Anbieter-Zugriff") editor shared by the token form and the
// chat-session card. Controlled and fetch-free: the owner holds the draft and loads
// the account and group lists (null = not loaded yet, so nothing can be judged stale).
//
// all = every account the caller OWNS, also future ones (never a shared account);
// accounts is the explicit per-account opt-in list, which is also how a SHARED
// account is named. includeGroups = every group the caller owns (only meaningful
// together with all); groups is the explicit per-group list (entry.accountId then
// carries the GROUP id).
export type VendorAccessDraft = {
  all: boolean;
  includeGroups: boolean;
  accounts: VendorAccessEntryState[];
  groups: VendorAccessEntryState[];
};

export function emptyVendorAccessDraft(): VendorAccessDraft {
  return { all: false, includeGroups: false, accounts: [], groups: [] };
}

/** The draft of a stored policy; a missing one (the strict default) reads as no access. */
export function vendorAccessDraftFromDTO(dto: VendorAccessDTO | undefined): VendorAccessDraft {
  return {
    all: dto?.all ?? false,
    includeGroups: dto?.include_groups ?? false,
    accounts: (dto?.accounts ?? []).map(vendorAccessEntryState),
    groups: (dto?.groups ?? []).map(vendorAccessGroupEntryState),
  };
}

// The opt-in entries the request carries. Once this open's account list has
// loaded, an opt-in for an account the list no longer offers (deleted, or no longer
// shared with the caller) is dropped: the backend rejects an account the owner
// cannot reach (400 portal.token_vendor_access_invalid) and the editor has no way
// to show, let alone remove, it. Under all the caller's OWN accounts are covered by
// the switch, so an entry naming one is dropped too (the backend ignores it anyway:
// all wins); what stays are the shared accounts, which all never covers. Without a
// loaded list nothing can be judged, so every entry stays.
function sentEntries(
  draft: VendorAccessDraft,
  accounts: readonly VendorAccount[] | null,
): VendorAccessEntryState[] {
  if (accounts === null) return draft.accounts;
  const offerable = offerableVendorAccounts(accounts, 'user');
  const ownIds = new Set(offerable.own.map((acc) => acc.id));
  const offeredIds = new Set([...ownIds, ...offerable.shared.map((acc) => acc.id)]);
  return draft.accounts.filter(
    (entry) => offeredIds.has(entry.accountId) && !(draft.all && ownIds.has(entry.accountId)),
  );
}

// A group that no longer exists is dropped once the list has loaded.
function sentGroups(
  draft: VendorAccessDraft,
  groups: readonly VendorAccountGroup[] | null,
): VendorAccessEntryState[] {
  if (groups === null) return draft.groups;
  const ids = new Set(groups.map((group) => group.id));
  return draft.groups.filter((entry) => ids.has(entry.accountId));
}

/**
 * The vendor_access request value: { all: true } for "all own providers" with the
 * explicit shared-account entries next to it when there are any; otherwise the
 * explicit opt-in list, always as an object (an empty list is the explicit "no
 * providers" policy, never null). An override that is off omits prefix_override
 * entirely (never enabled:false), an enabled one sends its trimmed value.
 */
export function buildVendorAccessDTO(
  draft: VendorAccessDraft,
  accounts: readonly VendorAccount[] | null,
  groups: readonly VendorAccountGroup[] | null,
): VendorAccessDTO {
  const accountDTOs = sentEntries(draft, accounts).map(vendorAccessEntryDTO);
  const groupDTOs = sentGroups(draft, groups).map(vendorAccessGroupEntryDTO);
  const groupFields = groupDTOs.length > 0 ? { groups: groupDTOs } : {};
  if (!draft.all) return { all: false, accounts: accountDTOs, ...groupFields };
  return {
    all: true,
    ...(draft.includeGroups ? { include_groups: true } : {}),
    ...(accountDTOs.length > 0 ? { accounts: accountDTOs } : {}),
    ...groupFields,
  };
}

/**
 * An enabled per-account prefix override with a malformed value blocks the save; a
 * disabled override is never sent, so its value is not judged, and neither is an
 * entry that is not sent at all.
 */
export function vendorAccessDraftInvalid(
  draft: VendorAccessDraft,
  accounts: readonly VendorAccount[] | null,
  groups: readonly VendorAccountGroup[] | null,
): boolean {
  return [...sentEntries(draft, accounts), ...sentGroups(draft, groups)].some(
    vendorAccessEntryInvalid,
  );
}

/**
 * `idPrefix` namespaces the DOM ids of one form (heading and per-entry groups).
 * Account names are vendor/owner-supplied text: rendered as text only. `disabled`
 * freezes every control (the chat card does so while the stored policy is unknown).
 */
export function VendorAccessEditor({
  value,
  onChange,
  accounts,
  groups,
  t,
  idPrefix,
  labels,
  disabled = false,
}: Readonly<{
  value: VendorAccessDraft;
  onChange: (next: VendorAccessDraft) => void;
  accounts: VendorAccount[] | null;
  groups: VendorAccountGroup[] | null;
  t: Translation;
  idPrefix: string;
  labels: { heading: string; note: string };
  disabled?: boolean;
}>) {
  const offerable = offerableVendorAccounts(accounts ?? [], 'user');
  // Under all the caller's own accounts are covered by the switch, so only the
  // shared ones are listed for explicit opt-in; otherwise both groups are. The
  // opted-in accounts follow in the account list's own order, so each one's
  // override controls sit in a stable place.
  const ownOptions = value.all ? [] : offerable.own;
  const sharedOptions = offerable.shared;
  const accountOption = (acc: VendorAccount) => ({
    value: acc.id,
    label: vendorAccountOptionLabel(acc, t),
  });
  const entryById = new Map(value.accounts.map((entry) => [entry.accountId, entry]));
  const optedInAccounts = [...ownOptions, ...sharedOptions].flatMap((acc) => {
    const entry = entryById.get(acc.id);
    return entry ? [{ id: acc.id, label: vendorAccountOptionLabel(acc, t), entry }] : [];
  });
  const groupOptions = (groups ?? []).map((group) => ({
    value: group.id,
    label: vendorGroupOptionLabel(group),
  }));
  const groupEntryById = new Map(value.groups.map((entry) => [entry.accountId, entry]));
  const optedInGroups = (groups ?? []).flatMap((group) => {
    const entry = groupEntryById.get(group.id);
    return entry ? [{ id: group.id, label: vendorGroupOptionLabel(group), entry }] : [];
  });
  const headingId = `${idPrefix}-vendor-access-heading`;
  const toggleAccount = (id: string) =>
    onChange({ ...value, accounts: toggleVendorAccessEntry(value.accounts, id) });
  const toggleGroup = (id: string) =>
    onChange({ ...value, groups: toggleVendorAccessEntry(value.groups, id) });

  return (
    // A native fieldset: `disabled` reaches every nested input without threading
    // the flag through the shared checkbox groups.
    <Box
      component="fieldset"
      role="group"
      aria-labelledby={headingId}
      disabled={disabled}
      sx={{ display: 'grid', gap: 1, m: 0, p: 0, border: 0, minWidth: 0 }}
    >
      <Typography id={headingId} variant="subtitle2">
        {labels.heading}
      </Typography>
      <Typography variant="caption" color="text.secondary">
        {labels.note}
      </Typography>
      <FormControlLabel
        control={
          <Checkbox
            checked={value.all}
            disabled={disabled}
            onChange={(e) =>
              onChange({
                ...value,
                all: e.target.checked,
                includeGroups: e.target.checked ? value.includeGroups : false,
              })
            }
          />
        }
        label={t.tokenVendorAccessAll}
      />
      <Box sx={{ pl: 3 }}>
        <FormControlLabel
          control={
            <Checkbox
              checked={value.all && value.includeGroups}
              disabled={disabled || !value.all}
              onChange={(e) => onChange({ ...value, includeGroups: e.target.checked })}
            />
          }
          label={t.tokenVendorAccessIncludeGroups}
        />
      </Box>
      {value.all && (
        <Typography variant="caption" color="text.secondary">
          {t.tokenVendorAccessAllHint}
        </Typography>
      )}
      {!value.all && accounts !== null && ownOptions.length + sharedOptions.length === 0 && (
        <Typography variant="body2" color="text.secondary">
          {t.tokenVendorAccessNone}
        </Typography>
      )}
      {ownOptions.length > 0 && (
        <CheckboxGroup
          legend={t.tokenVendorAccessAccountsLabel}
          options={ownOptions.map(accountOption)}
          selected={value.accounts.map((entry) => entry.accountId)}
          onToggle={toggleAccount}
        />
      )}
      {sharedOptions.length > 0 && (
        <Box sx={{ display: 'grid', gap: 0.5 }}>
          <CheckboxGroup
            legend={t.tokenVendorAccessSharedLabel}
            options={sharedOptions.map(accountOption)}
            selected={value.accounts.map((entry) => entry.accountId)}
            onToggle={toggleAccount}
          />
          <Typography variant="caption" color="text.secondary">
            {t.tokenVendorAccessSharedHint}
          </Typography>
        </Box>
      )}
      <VendorAccessEntryGroups
        items={optedInAccounts}
        idPrefix={`${idPrefix}-va`}
        onChange={(id, patch) =>
          onChange({ ...value, accounts: patchVendorAccessEntry(value.accounts, id, patch) })
        }
        t={t}
      />
      {groupOptions.length > 0 && (
        <CheckboxGroup
          legend={t.tokenVendorAccessGroupsLabel}
          options={groupOptions}
          selected={value.groups.map((entry) => entry.accountId)}
          onToggle={toggleGroup}
        />
      )}
      <VendorAccessEntryGroups
        items={optedInGroups}
        idPrefix={`${idPrefix}-vg`}
        onChange={(id, patch) =>
          onChange({ ...value, groups: patchVendorAccessEntry(value.groups, id, patch) })
        }
        t={t}
      />
    </Box>
  );
}

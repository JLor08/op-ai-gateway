// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { VendorAccountUsage } from './vendorAccounts';
import { type Fetcher, request } from './transport';

// Vendor account groups: an ordered failover group of one user's accounts at a
// single vendor (spec 2026-11-24, sections 9 and 10). Field names mirror
// portal.VendorAccountGroupDTO and friends field-for-field
// (internal/portal/service_vendor_groups.go); the routes and error codes are
// gateway.handlePortalVendorGroups* (internal/gateway/portal_vendor_group_endpoints.go).

// The thresholds are percentages used (1..100); a window switches the group to the
// next member once it reaches its threshold. Default 99.
export type VendorGroupThresholds = {
  five_hour_pct: number;
  weekly_pct: number;
  credits_pct: number;
};

// The computed state of a member: `active` is the one routing would pick,
// `available` is usable but behind it, `over_threshold` is past a threshold,
// `cooldown` answered a limit error recently, `not_usable` cannot serve (see reason).
export type VendorGroupMemberState =
  'active' | 'available' | 'over_threshold' | 'cooldown' | 'not_usable';

// Why a not_usable member cannot serve ("" for every other state).
export type VendorGroupMemberReason =
  '' | 'disabled' | 'needs_reconnect' | 'subscriptions_disabled' | 'not_reachable';

export type VendorGroupCooldown = { model: string; until: string };

export type VendorAccountGroupMember = {
  account_id: string;
  account_name: string;
  auth_type: string;
  // The owner's display name for a member somebody else owns, "" otherwise.
  owner_name: string;
  // 1-based index in member order.
  position: number;
  state: VendorGroupMemberState;
  reason: VendorGroupMemberReason | string;
  // Percent used of the binding window and when it frees up; absent when the group
  // owner may not see the member's usage.
  pct?: number;
  free_at?: string;
  cooldowns: VendorGroupCooldown[];
  usage?: VendorAccountUsage;
};

export type VendorAccountGroup = {
  id: string;
  vendor: string;
  name: string;
  model_prefix: string;
  status: string;
  thresholds: VendorGroupThresholds;
  // The member routing would pick ignoring the model, "" when none is usable.
  active_member_id: string;
  members: VendorAccountGroupMember[];
  created_at: string;
  updated_at: string;
};

// POST body. status defaults to active and each omitted threshold to 99; the
// vendor is fixed here and cannot be changed later.
export type CreateVendorAccountGroupRequest = {
  name: string;
  vendor: string;
  model_prefix?: string;
  status?: string;
  threshold_five_hour_pct?: number;
  threshold_weekly_pct?: number;
  threshold_credits_pct?: number;
  account_ids?: string[];
};

// PATCH body: omitted fields keep their stored value. The members are replaced
// through setVendorAccountGroupMembers.
export type UpdateVendorAccountGroupRequest = {
  name?: string;
  model_prefix?: string;
  status?: string;
  threshold_five_hour_pct?: number;
  threshold_weekly_pct?: number;
  threshold_credits_pct?: number;
};

export function vendorAccountGroupsApi(fetcher: Fetcher) {
  const base = '/api/portal/vendor-account-groups';
  const item = (id: string) => `${base}/${encodeURIComponent(id)}`;
  return {
    vendorAccountGroups: () => request<{ groups: VendorAccountGroup[] }>(fetcher, base),
    vendorAccountGroup: (id: string) => request<VendorAccountGroup>(fetcher, item(id)),
    createVendorAccountGroup: (body: CreateVendorAccountGroupRequest) =>
      request<VendorAccountGroup>(fetcher, base, { method: 'POST', body }),
    updateVendorAccountGroup: (id: string, body: UpdateVendorAccountGroupRequest) =>
      request<VendorAccountGroup>(fetcher, item(id), { method: 'PATCH', body }),
    deleteVendorAccountGroup: (id: string) =>
      request<{ ok: boolean }>(fetcher, item(id), { method: 'DELETE' }),
    // Replaces the member list; the order of accountIds is the member order.
    setVendorAccountGroupMembers: (id: string, accountIds: string[]) =>
      request<VendorAccountGroup>(fetcher, `${item(id)}/members`, {
        method: 'PUT',
        body: { account_ids: accountIds },
      }),
  };
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';
import type { PortalToken } from './tokens';

export type AdminUser = {
  id: string;
  email: string;
  display_name: string;
  role: string;
  status: string;
  preferred_language: string;
  created_at: string;
  totp_enabled: boolean;
};

export type CreateUserRequest = {
  email: string;
  display_name: string;
  role: string;
  preferred_language?: string;
  // The admin group the new user must be assigned to (spec:
  // 2026-08-09-group-visibility-admin-group-invite-design.md). Mandatory for
  // every actor; the backend adds the user to this group and its parent
  // system group.
  admin_group_id: string;
};

export type InviteResponse = {
  user: AdminUser;
  invite_url: string;
  email_sent: boolean;
  email_error?: string;
};

export type UpdateUserRequest = {
  display_name?: string;
  role?: string;
  status?: string;
  preferred_language?: string;
};

// Principal Limits (Phase 2, design spec §7.3): a principal (Service or User)
// rate/quota/budget configuration. Every field's zero value ("" for a period)
// means that specific limit is off — mirrors the backend's
// portal.LimitConfigDTO exactly (field-for-field, same JSON tags).
export type LimitPeriod = '' | 'hour' | 'day' | 'week' | 'month';

export type LimitConfig = {
  rate_requests: number;
  rate_window_seconds: number;
  request_quota: number;
  request_quota_period: LimitPeriod;
  token_quota: number;
  token_quota_period: LimitPeriod;
  cost_budget: number;
  cost_budget_period: LimitPeriod;
};

// Every limit off — the shared "clear all limits" / "nothing configured"
// value, reused by both ServicesView (create-form default) and UsersView.
export const EMPTY_LIMIT_CONFIG: LimitConfig = {
  rate_requests: 0,
  rate_window_seconds: 0,
  request_quota: 0,
  request_quota_period: '',
  token_quota: 0,
  token_quota_period: '',
  cost_budget: 0,
  cost_budget_period: '',
};

// A principal's CURRENT calendar-period usage against each of its configured
// limits (read-only; mirrors portal.LimitUsageDTO). A field for a limit with
// no period configured is always 0.
export type LimitUsage = {
  requests_this_period: number;
  tokens_this_period: number;
  cost_this_period: number;
};

// GET/PUT /api/portal/admin/users/{id}/limits response shape.
export type UserLimitsDTO = {
  limits: LimitConfig;
  usage: LimitUsage;
};

export function usersApi(fetcher: Fetcher) {
  return {
    adminUsers: () => request<{ data: AdminUser[] }>(fetcher, '/api/admin/users'),
    userTokens: (userId: string) =>
      request<{ data: PortalToken[] }>(
        fetcher,
        `/api/admin/users/${encodeURIComponent(userId)}/tokens`,
      ),
    createUser: (body: CreateUserRequest) =>
      request<InviteResponse>(fetcher, '/api/admin/users', { method: 'POST', body }),
    updateUser: (id: string, body: UpdateUserRequest) =>
      request<AdminUser>(fetcher, `/api/admin/users/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body,
      }),
    reinviteUser: (id: string) =>
      request<InviteResponse>(fetcher, `/api/admin/users/${encodeURIComponent(id)}/invite`, {
        method: 'POST',
      }),
    // Principal Limits (Phase 2, design spec §7.2): admin-only, no self-service
    // path exists on the backend.
    userLimits: (userId: string) =>
      request<UserLimitsDTO>(
        fetcher,
        `/api/portal/admin/users/${encodeURIComponent(userId)}/limits`,
      ),
    setUserLimits: (userId: string, body: LimitConfig) =>
      request<UserLimitsDTO>(
        fetcher,
        `/api/portal/admin/users/${encodeURIComponent(userId)}/limits`,
        {
          method: 'PUT',
          body,
        },
      ),
  };
}

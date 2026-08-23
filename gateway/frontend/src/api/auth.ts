// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

export type CurrentUser = {
  id: string;
  email: string;
  display_name: string;
  role: string;
  preferred_language: string;
  totp_enabled: boolean;
  totp_mode: string;
  // System-admin step-up mode: a system_admin session starts as a plain admin
  // (system scope withheld) until it elevates via enterSystemAdminMode. Every
  // other role always reads system_admin_mode=false.
  system_admin_mode: boolean;
  system_admin_mode_expires_at: string;
  system_admin_mode_require_password: boolean;
};

// A freshly generated, still-pending TOTP secret shown once during enrollment.
export type TotpEnrollment = {
  secret_base32: string;
  otpauth_uri: string;
  qr_png_data_uri: string;
};

// /api/auth/login: either the user (success), a code challenge, or a forced
// enrollment (required mode, not yet enrolled). Discriminate on the flag keys.
export type LoginResponse =
  CurrentUser | { totp_required: true } | ({ totp_enrollment_required: true } & TotpEnrollment);

// /api/auth/set-password: the user (success) or forced enrollment. The
// enrollment variant echoes `email` so the confirm login has both factors.
export type SetPasswordResponse =
  CurrentUser | ({ totp_enrollment_required: true; email: string } & TotpEnrollment);

export function authApi(fetcher: Fetcher) {
  return {
    login: (email: string, password: string, totpCode?: string) =>
      request<LoginResponse>(fetcher, '/api/auth/login', {
        method: 'POST',
        body: { email, password, ...(totpCode ? { totp_code: totpCode } : {}) },
      }),
    logout: () => request<{ ok: boolean }>(fetcher, '/api/auth/logout', { method: 'POST' }),
    session: () =>
      request<{ authenticated: boolean; user?: CurrentUser; default_language: string }>(
        fetcher,
        '/api/auth/session',
      ),
    setPassword: (token: string, password: string) =>
      request<SetPasswordResponse>(fetcher, '/api/auth/set-password', {
        method: 'POST',
        body: { token, password },
      }),
    changePassword: (currentPassword: string, newPassword: string) =>
      request<{ ok: boolean }>(fetcher, '/api/portal/password', {
        method: 'POST',
        body: { current_password: currentPassword, new_password: newPassword },
      }),
    updatePreferredLanguage: (language: string) =>
      request<CurrentUser>(fetcher, '/api/portal/language', { method: 'PUT', body: { language } }),
    updateChatSettings: (body: { log_communication: boolean; secret: boolean }) =>
      request<CurrentUser>(fetcher, '/api/portal/chat-settings', { method: 'PUT', body }),
    me: () => request<CurrentUser>(fetcher, '/api/portal/me'),
    // System-admin step-up: enter (optionally re-entering the password) or leave
    // the elevated system-admin mode; both return the fresh CurrentUser.
    enterSystemAdminMode: (password?: string) =>
      request<CurrentUser>(fetcher, '/api/portal/system-admin-mode', {
        method: 'POST',
        body: { password: password ?? '' },
      }),
    exitSystemAdminMode: () =>
      request<CurrentUser>(fetcher, '/api/portal/system-admin-mode', {
        method: 'DELETE',
        body: {},
      }),
    // Per-user UI preferences (generic KV; value is arbitrary JSON owned by the caller).
    preferences: () => request<Record<string, unknown>>(fetcher, '/api/portal/preferences'),
    setPreference: (key: string, value: unknown) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/preferences/${encodeURIComponent(key)}`, {
        method: 'PUT',
        body: value,
      }),
    totpEnroll: () =>
      request<TotpEnrollment>(fetcher, '/api/portal/totp/enroll', { method: 'POST' }),
    totpConfirm: (code: string) =>
      request<CurrentUser>(fetcher, '/api/portal/totp/confirm', { method: 'POST', body: { code } }),
    totpDisable: (code: string) =>
      request<{ ok: boolean }>(fetcher, '/api/portal/totp', { method: 'DELETE', body: { code } }),
    adminResetTotp: (userId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/admin/users/${encodeURIComponent(userId)}/totp/reset`,
        { method: 'POST' },
      ),
  };
}

// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

// Vendor accounts ("Anbieter"): a per-user external AI vendor account -- a
// plain API key today, a consumer-subscription OAuth connection later. Every
// authenticated user manages only their OWN accounts (the backend answers 404,
// never 403, for somebody else's id). Field names mirror
// portal.VendorAccountDTO / CreateVendorAccountRequest /
// UpdateVendorAccountRequest exactly, field-for-field
// (internal/portal/service_vendor_accounts.go); the routes and response shapes
// are gateway.handlePortalVendorAccounts / handlePortalVendorAccountItem
// (internal/gateway/portal_vendor_account_endpoints.go).
//
// Credentials are WRITE-ONLY: the DTO carries only the api_key_set /
// subscription_connected booleans, never the secret.
//
// A subscription account is attached to a consumer subscription in one of
// three ways, all owner-only POSTs on the item path (handlePortalVendorAccountItem,
// internal/gateway/portal_vendor_account_endpoints.go; service side in
// internal/portal/service_vendor_connect.go): connect/import takes tokens the
// user already holds, connect/begin + connect/complete run the OAuth
// code-paste flow (begin returns the vendor sign-in URL, the user pastes the
// code the vendor shows back into complete), and -- OpenAI only --
// connect/device/begin + connect/device/poll run the device-code flow
// (internal/portal/service_vendor_device_connect.go). The import and
// code-paste successes answer the credential-free VendorAccount with
// subscription_connected=true; the device poll answers only {connected}, so the
// caller re-reads the account (GET .../{id}).
export type VendorAccountVendor = 'openai' | 'anthropic';
export type VendorAccountAuthType = 'api_key' | 'subscription';
// needs_reconnect is system-managed (a failed subscription token refresh); a
// user can only ever set active or disabled.
export type VendorAccountStatus = 'active' | 'disabled' | 'needs_reconnect';

// One gateway model an account serves -- mirrors portal.VendorAccountModelDTO.
// Always [] until the routing milestone seeds the per-vendor catalog.
export type VendorAccountModel = {
  gateway_model: string;
  upstream_model: string;
  api_flavor: string;
};

// The latest rate-limit snapshot the gateway scraped off an account's upstream
// responses -- mirrors portal.VendorAccountUsageDTO. PERCENTAGES ONLY: neither
// vendor exposes an absolute cap, so there is no "N of M". A percentage is
// 0..100, or -1 when that window has never been observed (unknown, NOT 0 % used);
// a reset time is null when the vendor sent none; credit_balance is the vendor's
// raw credit string ("" = none).
export type VendorAccountUsage = {
  five_hour_pct: number;
  five_hour_reset_at: string | null;
  weekly_pct: number;
  weekly_reset_at: string | null;
  credit_balance: string;
  updated_at: string;
};

export type VendorAccount = {
  id: string;
  vendor: VendorAccountVendor;
  auth_type: VendorAccountAuthType;
  name: string;
  status: VendorAccountStatus;
  // The write-only secret sentinels: true once an api key (resp. a subscription
  // token set) is stored. The value itself is never returned.
  api_key_set: boolean;
  subscription_connected: boolean;
  models: VendorAccountModel[];
  // The rate-limit snapshot. ONLY the single-account read (GET .../{id}, i.e.
  // vendorAccount(id)) carries it, and only once the gateway has scraped one;
  // the list and the create/update/connect responses omit it.
  usage?: VendorAccountUsage;
  created_at: string;
  updated_at: string;
};

// POST /api/portal/vendor-accounts body. status defaults to active when
// omitted/empty; api_key is only meaningful (and only accepted) for an api_key
// account -- the backend rejects one on a subscription account with
// vendor_account.api_key_not_allowed.
export type CreateVendorAccountRequest = {
  vendor: string;
  auth_type: string;
  name: string;
  status?: string;
  api_key?: string;
};

// PATCH /api/portal/vendor-accounts/{id} body -- pointer-semantics on the
// backend (omitted = keep the stored value). vendor and auth_type are
// immutable. api_key is the write-only secret sentinel: omitted keeps the
// stored key, the exact empty string "" clears it, any other value replaces it
// (a whitespace-only value is rejected with vendor_account.api_key_invalid).
export type UpdateVendorAccountRequest = {
  name?: string;
  status?: string;
  api_key?: string;
};

// POST .../connect/import body -- mirrors portal.ConnectVendorAccountImportRequest.
// Both tokens are WRITE-ONLY secrets. Only access_token is required; the
// refresh token and expires_at (an RFC 3339 time; omitted = unknown) are
// optional.
export type ConnectVendorAccountImportRequest = {
  access_token: string;
  refresh_token?: string;
  expires_at?: string;
};

// POST .../connect/begin response: the vendor sign-in URL the portal opens in
// a new tab.
export type VendorAccountConnectBegin = { authorize_url: string };

// POST .../connect/device/begin response: the public pairing code the user
// types in at the vendor's verification page, and that page's URL. The
// user_code is NOT a secret (it only pairs a browser approval with this
// pending login), so it is meant to be shown.
export type VendorAccountDeviceConnectBegin = { user_code: string; verification_url: string };

// POST .../connect/device/poll response: false while the user has not approved
// the code yet, true once the account is connected. Never carries a token.
export type VendorAccountDeviceConnectPoll = { connected: boolean };

export function vendorAccountsApi(fetcher: Fetcher) {
  return {
    // The vendor-accounts MASTER flag (system setting vendor_accounts_enabled,
    // off by default). Portal-scoped (any authenticated user, gateway:use) and
    // boolean-only -- no routing mode or other setting leaks -- so it gates the
    // "Anbieter" nav item / view for every user the way netbirdEnabled's
    // module_enabled and certificatesEnabled gate theirs. It answers while the
    // module is off; every other vendor-account call then answers 409
    // vendor_accounts.module_disabled.
    vendorAccountsEnabled: () =>
      request<{ module_enabled: boolean }>(fetcher, '/api/portal/vendor-accounts/enabled'),
    // The caller's OWN accounts, oldest first -- system scope does not widen
    // the list (the page is a personal one and the DTO carries no owner).
    vendorAccounts: () =>
      request<{ data: VendorAccount[] }>(fetcher, '/api/portal/vendor-accounts'),
    // One account of the caller (404 for anybody else's id).
    vendorAccount: (id: string) =>
      request<VendorAccount>(fetcher, `/api/portal/vendor-accounts/${encodeURIComponent(id)}`),
    createVendorAccount: (body: CreateVendorAccountRequest) =>
      request<VendorAccount>(fetcher, '/api/portal/vendor-accounts', { method: 'POST', body }),
    updateVendorAccount: (id: string, body: UpdateVendorAccountRequest) =>
      request<VendorAccount>(fetcher, `/api/portal/vendor-accounts/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body,
      }),
    deleteVendorAccount: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/vendor-accounts/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    // Subscription connect, path 1: attach tokens the user already holds. The
    // backend does not probe them (the first real request validates them).
    connectVendorAccountImport: (id: string, body: ConnectVendorAccountImportRequest) =>
      request<VendorAccount>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/connect/import`,
        { method: 'POST', body },
      ),
    // Subscription connect, path 2 / step 1: start the OAuth code-paste flow.
    // No request body; a second begin for the same account replaces the first.
    beginVendorAccountConnect: (id: string) =>
      request<VendorAccountConnectBegin>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/connect/begin`,
        { method: 'POST' },
      ),
    // Subscription connect, path 2 / step 2: `code` is whatever the user pasted
    // back -- a bare code, "code#state", or a whole callback URL; the backend
    // parses all three. A refused paste keeps the pending connect, so the
    // caller can simply retry.
    completeVendorAccountConnect: (id: string, code: string) =>
      request<VendorAccount>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/connect/complete`,
        { method: 'POST', body: { code } },
      ),
    // Subscription connect, path 3 (OpenAI accounts only; any other vendor is
    // vendor_account.device_not_supported): start the device-code login. No
    // request body; a second begin for the same account replaces the first.
    beginVendorAccountDeviceConnect: (id: string) =>
      request<VendorAccountDeviceConnectBegin>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/connect/device/begin`,
        { method: 'POST' },
      ),
    // Path 3 / step 2: ONE backend poll of the begun login, called by the portal
    // on an interval. {connected:false} (HTTP 200) while the user has not
    // approved yet. A TRANSIENT vendor failure is a 502
    // vendor_account.connect_upstream_failed that the backend answers WITHOUT
    // dropping the pending login -- the caller keeps polling. A vendor refusal
    // is a 400 vendor_account.connect_rejected (the pending login is gone: begin
    // again). No request body.
    pollVendorAccountDeviceConnect: (id: string) =>
      request<VendorAccountDeviceConnectPoll>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/connect/device/poll`,
        { method: 'POST' },
      ),
  };
}

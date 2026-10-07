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
  };
}

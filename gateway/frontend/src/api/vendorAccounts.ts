// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { AdminGroupCandidate } from './groups';
import { type Fetcher, request } from './transport';

// Vendor accounts ("Anbieter"): a per-user external AI vendor account -- a
// plain API key today, a consumer-subscription OAuth connection later. Every
// authenticated user manages their OWN accounts; an owner may also share one
// (share_level, see below), and then its co-managers read and operate it and
// the members of a resource group it is linked to read a minimal view. For a
// stranger the backend answers 404, never 403, for the id. Field names mirror
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
//
// openai and anthropic have fixed hosts and either auth type; the other six
// vendors (xai, openrouter, kilo, google, the native-dialect google_native and
// the user-supplied openai_compatible "Custom") are reached through a
// configurable root URL and are api-key only (the backend answers
// vendor_account.auth_type_invalid for a subscription one). google_native speaks
// Google's own generateContent API rather than the OpenAI wire format the rest use.
export type VendorAccountVendor =
  | 'openai'
  | 'anthropic'
  | 'xai'
  | 'openrouter'
  | 'kilo'
  | 'google'
  | 'google_native'
  | 'openai_compatible';
export type VendorAccountAuthType = 'api_key' | 'subscription';
// needs_reconnect is system-managed (a failed subscription token refresh); a
// user can only ever set active or disabled.
export type VendorAccountStatus = 'active' | 'disabled' | 'needs_reconnect';
// How far an account is shared (portal.VendorAccountDTO.ShareLevel): `none` is
// personal (owner-only), `shared` lets the co-managers of its management groups
// read and operate it, `full` also delegates its credential and sharing to them.
// The backend reports the STORED level and an unrecognised stored value as `none`.
export type VendorAccountShareLevel = 'none' | 'shared' | 'full';

// One gateway model an account serves -- mirrors portal.VendorAccountModelDTO.
// gateway_model is the id a client requests at the gateway: the account's
// model_prefix glued onto the vendor's own id (upstream_model), which is what
// the vendor is still asked for. display_name is the vendor's human-readable
// name for the model, "" when it supplied none. It is TEXT FROM THE VENDOR (it
// may hold < > " &), so it is only ever rendered as text, never as markup.
export type VendorAccountModel = {
  gateway_model: string;
  upstream_model: string;
  api_flavor: string;
  display_name: string;
};

// The latest usage snapshot of a vendor account, scraped off its upstream responses
// and/or pulled from the vendor -- mirrors portal.VendorAccountUsageDTO. The
// rate-limit windows are PERCENTAGES ONLY: neither OpenAI nor Anthropic exposes an
// absolute window cap, so there is no "N of M" for them. A percentage is 0..100, or
// -1 when that window has never been observed (unknown, NOT 0 % used); a reset time
// is null when the vendor sent none; credit_balance is the vendor's raw credit
// string ("" = none; Kilo's balance).
//
// The absolute amounts ride in the spend fields and credit_status (always present
// on the wire), which a Business plan's spend control and an OpenRouter key's USD
// limit both fill: spend_unit (e.g. "credit", "USD") and the vendor's raw
// spend_limit / spend_used / spend_remaining strings (kept verbatim, never parsed),
// spend_used_pct (0..100, or -1 = unknown, like the windows), spend_reset_at (null =
// unknown) and credit_status ("unlimited" | "has_credits" | "none", "" = unknown;
// an OpenRouter key without a cap is "unlimited" with the spend strings empty). An
// account with none of this reads "" / -1 / null for all of them.
export type VendorAccountUsage = {
  five_hour_pct: number;
  five_hour_reset_at: string | null;
  weekly_pct: number;
  weekly_reset_at: string | null;
  credit_balance: string;
  spend_unit: string;
  spend_limit: string;
  spend_used: string;
  spend_remaining: string;
  spend_used_pct: number;
  spend_reset_at: string | null;
  credit_status: string;
  updated_at: string;
};

export type VendorAccount = {
  id: string;
  vendor: VendorAccountVendor;
  auth_type: VendorAccountAuthType;
  name: string;
  status: VendorAccountStatus;
  // The account's optional model-id namespace ("" = none): every model it
  // serves is listed and requested as model_prefix + the vendor's model id.
  // Valid shape: at most 64 characters of [A-Za-z0-9._~:/@+-], no "..".
  model_prefix: string;
  // The upstream root URL of a configurable-root account (a preset's default
  // root, or the one the user typed); "" for openai and anthropic, whose hosts
  // are fixed. Always on the wire. IMMUTABLE: fixed at creation and never sent
  // on an update.
  base_url: string;
  // The write-only secret sentinels: true once an api key (resp. a subscription
  // token set) is stored. The value itself is never returned.
  api_key_set: boolean;
  subscription_connected: boolean;
  models: VendorAccountModel[];
  // The rate-limit snapshot. The single-account read (GET .../{id}, i.e.
  // vendorAccount(id)) and the list carry it, and only once the gateway has
  // scraped one; the create/update/connect responses omit it.
  usage?: VendorAccountUsage;
  created_at: string;
  updated_at: string;
  // Sharing (spec 2026-10-15 sections 6 and 10). The DTO is rendered FOR the
  // viewer, and it is credential-free for every viewer: a co-manager (CM) of a
  // shared account receives exactly these fields with is_owner false, never the
  // owner's secret (only the api_key_set / subscription_connected booleans above).
  //
  // A MEMBER -- somebody who can only route to the account through a resource
  // group -- reads a MINIMAL view of it: share_level "" (withheld, which is NOT
  // `none`), admin_groups [], system_group_id / system_group_name "", base_url "",
  // no usage, each model's upstream_model "" and both timestamps the zero time
  // ("0001-01-01T00:00:00Z"). is_owner is false and shared_by names the owner.
  // vendorAccountAccess (components/shared) is the one place that tells the views
  // apart; nothing renders a withheld field.
  share_level: VendorAccountShareLevel | '';
  // The management groups (id + name; always an array, [] when the account is not
  // shared) and their parent system group, the containment root ("" / "" when
  // there is none; the name is a best-effort lookup).
  admin_groups: { id: string; name: string }[];
  system_group_id: string;
  system_group_name: string;
  // Whether the viewer owns the account; shared_by is the owner's display name
  // for a viewer who does not ("" for the owner, or when the name is unknown).
  is_owner: boolean;
  shared_by: string;
};

// One resource group the OWNER of a shared account may enter it into (spec
// 2026-10-15 section 9): id + name + whether the account is currently in it.
// Mirrors portal.VendorAccountResourceGroupDTO. Drives the owner's "Resource
// groups" join/leave section in the Anbieter detail; never exposes any other
// server, account or target of the resource group.
export type VendorAccountResourceGroup = { id: string; name: string; member: boolean };

// POST /api/portal/vendor-accounts body. status defaults to active when
// omitted/empty; api_key is only meaningful (and only accepted) for an api_key
// account -- the backend rejects one on a subscription account with
// vendor_account.api_key_not_allowed. model_prefix is optional (omitted = none);
// a malformed one is a 400 vendor_account.model_prefix_invalid. base_url is the
// root URL of a configurable-root account (xai, openrouter, kilo, google,
// google_native, openai_compatible): omitted = the preset's default root, required for
// openai_compatible (400 vendor_account.base_url_required), and a non-https URL
// or one carrying credentials, a query or a fragment is a 400
// vendor_account.base_url_invalid. The backend ignores it for openai and
// anthropic. It exists on create only -- UpdateVendorAccountRequest has no
// base_url, because the root cannot be changed once the account exists.
export type CreateVendorAccountRequest = {
  vendor: string;
  auth_type: string;
  name: string;
  status?: string;
  api_key?: string;
  model_prefix?: string;
  base_url?: string;
};

// PATCH /api/portal/vendor-accounts/{id} body -- pointer-semantics on the
// backend (omitted = keep the stored value). vendor and auth_type are
// immutable. api_key is the write-only secret sentinel: omitted keeps the
// stored key, the exact empty string "" clears it, any other value replaces it
// (a whitespace-only value is rejected with vendor_account.api_key_invalid).
// model_prefix follows the same pointer semantics: omitted keeps the stored
// prefix, "" clears it, any other value replaces it (a malformed one is a 400
// vendor_account.model_prefix_invalid). Sending it -- even unchanged -- also
// re-labels the account's model rows to the stored prefix, which heals rows a
// failed earlier re-label left behind.
export type UpdateVendorAccountRequest = {
  name?: string;
  status?: string;
  api_key?: string;
  model_prefix?: string;
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

// POST .../check response -- mirrors portal.VendorConnectionCheck: the verdict of
// asking the vendor whether the account's stored credential is accepted. It is
// about the CREDENTIAL only, never about whether a particular model is served,
// so a refused chat with a `valid` verdict points at the model, not the login.
// `unverifiable` is no statement about the credential: the vendor could not be
// reached or gave no clear answer, or there was nothing to test (no credential
// stored yet). `detail` is a short English status phrase (the vendor's HTTP
// status and error code, or what was missing), token-free by construction: a
// technical aid for the user, not a message to localize. checked_at is an
// RFC 3339 instant.
export type VendorConnectionStatus = 'valid' | 'invalid' | 'unverifiable';
export type VendorConnectionCheck = {
  status: VendorConnectionStatus;
  detail: string;
  checked_at: string;
};

// POST .../models/refresh response -- mirrors vendorAccountModelsRefreshResponse:
// the credential-free account as it now serves its models, and what the refresh
// did. `ok`: the vendor served a usable list and it replaced the account's models
// (`discovered` of them). `unverifiable`: no usable list could be had (the vendor
// could not be reached, refused, or listed nothing usable), so the models are
// UNCHANGED and `discovered` is 0; it is no statement about the credential, and
// `account.status` reads needs_reconnect when a rejected refresh token is why.
// `detail` is a short English phrase, token-free by construction: a technical
// aid for the user, not a message to localize.
export type VendorModelsRefreshStatus = 'ok' | 'unverifiable';
export type VendorModelsRefresh = {
  status: VendorModelsRefreshStatus;
  discovered: number;
  detail: string;
};
export type VendorAccountModelsRefresh = {
  account: VendorAccount;
  refresh: VendorModelsRefresh;
};

// POST .../usage/refresh -- mirrors portal.VendorUsageRefreshResult and the
// gateway's {usage, refresh} answer. `status` is what to key off (never the raw
// English `detail`): ok = the vendor was asked and the snapshot stored; fresh = the
// server's recent-pull TTL skipped the call (the stored snapshot is returned, the
// vendor was NOT called); unverifiable = the vendor could not be asked or answered
// unusably (the stored snapshot is kept); unsupported = the account has no active
// usage pull (only a connected OpenAI subscription and an OpenRouter or Kilo api
// key have one). `usage` is the same snapshot the single-account read carries, or
// null when none is stored.
export type VendorUsageRefreshStatus = 'ok' | 'fresh' | 'unverifiable' | 'unsupported';
export type VendorUsageRefresh = {
  status: VendorUsageRefreshStatus;
  detail: string;
};
export type VendorAccountUsageRefresh = {
  usage: VendorAccountUsage | null;
  refresh: VendorUsageRefresh;
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
    //
    // subscriptions_enabled mirrors the narrower vendor_subscriptions_enabled
    // system switch (on by default) the same way, so every user's Anbieter page
    // can flag its subscription accounts without the system-scoped settings read.
    // Optional on the wire: a response without it means the switch is on.
    vendorAccountsEnabled: () =>
      request<{ module_enabled: boolean; subscriptions_enabled?: boolean }>(
        fetcher,
        '/api/portal/vendor-accounts/enabled',
      ),
    // The accounts the caller OWNS, then the ones they manage as a co-manager of a
    // shared account, then the ones they can only route to as a member (each arm
    // oldest first; a member row is the minimal view -- see VendorAccount). System
    // scope does not widen the list (the page is a personal one).
    vendorAccounts: () =>
      request<{ data: VendorAccount[] }>(fetcher, '/api/portal/vendor-accounts'),
    // One account the caller may read, in the view their tier earns -- the owner's,
    // a co-manager's (key-free, is_owner false) or a member's minimal one (404 for a
    // stranger's id).
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
    // Moves an account to `level` (PUT .../share-level; the share-level lifecycle of
    // spec 2026-10-15 section 7). The backend compares it with the stored level and
    // runs the one or the other: a RAISE (none -> shared -> full) is owner + a current
    // admin role and always carries >= 1 management group in `adminGroupIds` (a
    // subscription account only to `shared`); a LOWER (to `shared` from `full`, or to
    // `none`) is the credential gate -- the owner always, a CM at `full` -- and ignores
    // the groups, so the portal leaves them out. Lowering to `none` also removes the
    // management groups, the resource-group links and the containment root. Answers
    // the credential-free account as the caller now reads it. 409
    // vendor_accounts.sharing_disabled while the sharing switch is off (it is by
    // default); a stranger gets the 404 of an unknown id, a caller who can see the
    // account but may not do this a 403.
    setVendorAccountShareLevel: (
      id: string,
      level: VendorAccountShareLevel,
      adminGroupIds?: string[],
    ) =>
      request<VendorAccount>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/share-level`,
        {
          method: 'PUT',
          body: adminGroupIds === undefined ? { level } : { level, admin_group_ids: adminGroupIds },
        },
      ),
    // Replaces the management groups of an account that is already shared (PUT
    // .../admin-groups): the owner at `shared`, the owner or a CM at `full`. >= 1
    // group, each one the caller may name, all under the account's immutable
    // containment root (400 vendor_account.root_mismatch otherwise). Answers the
    // credential-free account. Mirrors setServerAdminGroups.
    setVendorAccountAdminGroups: (id: string, groupIds: string[]) =>
      request<VendorAccount>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/admin-groups`,
        {
          method: 'PUT',
          body: { admin_group_ids: groupIds },
        },
      ),
    // The admin-tier groups the caller may name as management groups: system scope ->
    // every admin-tier group; anyone else -> the groups they manage vendor accounts
    // through (can_manage_vendor_accounts), so a plain user gets []. Each carries its
    // parent system group, the account's future containment root. Drives the raise
    // picker and the management-group editor; mirrors serverAdminGroupCandidates.
    vendorAccountAdminGroupCandidates: () =>
      request<{ data: AdminGroupCandidate[] }>(
        fetcher,
        '/api/portal/vendor-account-admin-group-candidates',
      ).then((r) => r.data),
    // Owner self-service resource-group link (spec 2026-10-15 section 9; the
    // vendor-account analogue of the server-owner trio in api/servers.ts): the
    // OWNER of a `shared` or `full` account lists the resource groups linked to an
    // admin group they are a MEMBER of and whose root is the account's (each with a
    // `member` flag) and joins/leaves their OWN account. Neither admin nor
    // resource-management permission required. A caller who is not the owner gets
    // the 403/404 of the account, a private account a 400
    // vendor_account.not_shared; a join under another root is a 400
    // resource_group.vendor_account_system_group_mismatch. Join and leave are
    // idempotent and write only this account's own link row. All three are a 409
    // while the vendor-accounts module or the sharing switch is off.
    vendorAccountResourceGroups: (id: string) =>
      request<{ data: VendorAccountResourceGroup[] }>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/resource-groups`,
      ).then((r) => r.data),
    joinVendorAccountResourceGroup: (id: string, rgId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/resource-groups/${encodeURIComponent(rgId)}`,
        { method: 'PUT' },
      ),
    leaveVendorAccountResourceGroup: (id: string, rgId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/resource-groups/${encodeURIComponent(rgId)}`,
        { method: 'DELETE' },
      ),
    // The explicit "test connection": the backend opens the account's stored
    // credential and asks the vendor whether it is accepted, answering 200 with
    // the credential-free verdict whichever way it came out (a rejected credential
    // is an `invalid` verdict, NOT an error). Owner-only (404 for anybody else's
    // id); an unexpected failure is a 500 vendor_account.check_failed. No request
    // body.
    testConnection: (id: string) =>
      request<VendorConnectionCheck>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/check`,
        { method: 'POST' },
      ),
    // The explicit "refresh models": the gateway asks the vendor which models
    // the account's stored credential can use and, on a usable answer, replaces
    // the account's models with it (under the account's prefix). Fail-soft: a
    // vendor that cannot be asked is still a 200 with refresh.status
    // "unverifiable" and the models untouched. Owner-only (404 for anybody
    // else's id); a stored credential that cannot be opened is a 409
    // vendor_account.credential_unreadable, an unexpected failure a 500
    // vendor_account.refresh_failed. No request body.
    refreshModels: (id: string) =>
      request<VendorAccountModelsRefresh>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/models/refresh`,
        { method: 'POST' },
      ),
    // The explicit / on-view "refresh usage": the gateway asks the vendor for the
    // account's current limits and stores them, WITHOUT touching the models. Only a
    // connected OpenAI subscription and an OpenRouter or Kilo api key have such an
    // active pull; any other account answers refresh.status "unsupported". Without
    // `force` the server skips the vendor call while its last pull is younger than
    // its TTL (5 min; status "fresh"), which is what the on-view call relies on;
    // `{force: true}` (?force=1) bypasses the TTL for the explicit button. Fail-soft
    // like the models refresh: a vendor that cannot be asked is a 200 with
    // "unverifiable". Owner-only (404 for anybody
    // else's id); 409 vendor_account.credential_unreadable for a stored credential
    // that cannot be opened, 500 vendor_account.usage_refresh_failed otherwise. No
    // request body.
    refreshUsage: (id: string, opts: { force?: boolean } = {}) =>
      request<VendorAccountUsageRefresh>(
        fetcher,
        `/api/portal/vendor-accounts/${encodeURIComponent(id)}/usage/refresh${opts.force ? '?force=1' : ''}`,
        { method: 'POST' },
      ),
    // Subscription connect, path 1: attach tokens the user already holds. The
    // backend probes the access token once before it stores anything: a token the
    // vendor definitively rejects is a 400 vendor_account.connect_invalid_credentials
    // (nothing stored). The portal's file picker fills this same request from a
    // credential file it parsed in the browser.
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

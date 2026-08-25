// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

// One model_override_map entry on the wire: `to` is the gateway model the
// requested-model key resolves to; `offer` lists the key itself as an
// offered model name (inheriting `to`'s API flavors); `hide_target` removes
// `to`'s own name from the offered list. The plain-string row form is no
// longer accepted by the backend (400 portal.token_model_override_invalid).
export type ModelOverrideEntry = {
  to: string;
  offer: boolean;
  hide_target: boolean;
};

export type PortalToken = {
  id: string;
  name: string;
  secret_prefix: string;
  status: 'active' | 'disabled' | 'expired';
  scopes: string[];
  expires_at: string | null;
  last_used_at: string | null;
  created_at: string;
  // model_override is the CATCH-ALL override (applied to any requested model with
  // no entry in model_override_map). "" = no catch-all.
  model_override: string;
  // model_override_map maps a requested model name -> the gateway model to use
  // instead (takes precedence over the catch-all). Absent/empty = no per-model
  // overrides.
  model_override_map?: Record<string, ModelOverrideEntry>;
  // server_override forces every request on this token onto one specific
  // AI-server the owner manages (absent/"" = no override, the common case —
  // the backend DTO field is `omitempty`); server_override_force_unreachable,
  // when true, routes there even if the resolver deems it disabled/unreachable
  // (else that request 502s rather than falling back elsewhere). Always
  // false/absent whenever server_override is absent/"".
  server_override?: string;
  server_override_force_unreachable?: boolean;
  // last_used_model is the gateway model or group name this token last routed
  // a request to. READ-ONLY: it appears on no request type and must never be
  // sent back in a create/update body (the backend ignores it there anyway).
  last_used_model?: string;
  // The unknown-model redirect: a requested model this token cannot route to
  // is served by last_used_model, or else by unknown_model_fallback, instead
  // of failing. unknown_model_redirect_blocked widens "unknown" from "does
  // not exist at all" to "exists but this token cannot call it". Both
  // sub-settings are always off/empty whenever unknown_model_redirect is
  // false.
  unknown_model_redirect?: boolean;
  unknown_model_redirect_blocked?: boolean;
  unknown_model_fallback?: string;
  log_communication: boolean;
  secret: boolean;
  is_chat_session: boolean;
  deletable: boolean;
  // Projects (spec: 2026-08-08-projects-design.md §6). project_id attributes
  // the token to a project the owner is a member of; project_name is the
  // project's current display name ("" whenever project_id is empty or the
  // project no longer exists) — mirrors portal.Service.PortalToken.
  project_id?: string;
  project_name?: string;
};

export type CreateTokenRequest = {
  name: string;
  scopes: string[];
  model_override?: string;
  model_override_map?: Record<string, ModelOverrideEntry>;
  // Optional server override (see PortalToken.server_override); self-healed
  // server-side against the owner's current server-manage rights, never
  // rejected outright.
  server_override?: string;
  server_override_force_unreachable?: boolean;
  // The unknown-model redirect (see PortalToken). Deliberately NO
  // last_used_model here: a fresh token has never routed a request, and the
  // marker is written by the inference path only, never by a client.
  unknown_model_redirect?: boolean;
  unknown_model_redirect_blocked?: boolean;
  unknown_model_fallback?: string;
  log_communication?: boolean;
  secret?: boolean;
  // Optional project attribution (§6); "" = no project. Membership-checked
  // server-side (403 token.project_not_member if the caller isn't a member).
  project_id?: string;
};

export type CreateTokenResponse = {
  token: PortalToken;
  secret: string;
};

export type UpdateTokenRequest = {
  name?: string;
  scopes?: string[];
  status?: 'active' | 'disabled';
  model_override?: string;
  model_override_map?: Record<string, ModelOverrideEntry>;
  // Omitted = keep the current value (still re-validated server-side on every
  // update); "" clears the override; a server id replaces it.
  server_override?: string;
  server_override_force_unreachable?: boolean;
  // The unknown-model redirect (see PortalToken). Omitted = keep the current
  // value. Deliberately no last_used_model — it is read-only, see above.
  unknown_model_redirect?: boolean;
  unknown_model_redirect_blocked?: boolean;
  unknown_model_fallback?: string;
  log_communication?: boolean;
  secret?: boolean;
  // nil (omitted) = keep the current project attribution; "" = clear it;
  // a project id = reassign (membership-checked server-side).
  project_id?: string;
};

export function tokensApi(fetcher: Fetcher) {
  return {
    tokens: () => request<{ data: PortalToken[] }>(fetcher, '/api/portal/tokens'),
    createToken: (body: CreateTokenRequest) =>
      request<CreateTokenResponse>(fetcher, '/api/portal/tokens', { method: 'POST', body }),
    rotateToken: (id: string) =>
      request<CreateTokenResponse>(fetcher, `/api/portal/tokens/${encodeURIComponent(id)}/rotate`, {
        method: 'POST',
      }),
    updateToken: (id: string, body: UpdateTokenRequest) =>
      request<PortalToken>(fetcher, `/api/portal/tokens/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body,
      }),
    deleteToken: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/tokens/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
  };
}

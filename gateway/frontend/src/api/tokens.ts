// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';

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
  model_override_map?: Record<string, string>;
  // server_override forces every request on this token onto one specific
  // AI-server the owner manages (absent/"" = no override, the common case —
  // the backend DTO field is `omitempty`); server_override_force_unreachable,
  // when true, routes there even if the resolver deems it disabled/unreachable
  // (else that request 502s rather than falling back elsewhere). Always
  // false/absent whenever server_override is absent/"".
  server_override?: string;
  server_override_force_unreachable?: boolean;
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
  model_override_map?: Record<string, string>;
  // Optional server override (see PortalToken.server_override); self-healed
  // server-side against the owner's current server-manage rights, never
  // rejected outright.
  server_override?: string;
  server_override_force_unreachable?: boolean;
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
  model_override_map?: Record<string, string>;
  // Omitted = keep the current value (still re-validated server-side on every
  // update); "" clears the override; a server id replaces it.
  server_override?: string;
  server_override_force_unreachable?: boolean;
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

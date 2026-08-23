// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request } from './transport';
import type { AdminGroupCandidate } from './groups';
import type { LimitConfig, LimitUsage } from './users';

// Service Accounts (Phase 1): a Service is managed like an AI-Server (admins
// create; admins + delegates manage) and owns any number of service-tokens
// restricted to llm:invoke (inference only).
export type ServiceStatus = 'active' | 'disabled';

export type ServiceDelegate = {
  user_id: string;
  user_name: string;
  can_manage_settings: boolean;
};

// The create/update wire shape for one delegate entry (no user_name — the
// backend resolves display names server-side).
export type ServiceDelegateInput = {
  user_id: string;
  can_manage_settings: boolean;
};

export type PortalService = {
  id: string;
  name: string;
  description: string;
  status: ServiceStatus;
  delegates: ServiceDelegate[];
  // Always non-null; [] = every gateway model is allowed (the default).
  allowed_models: string[];
  token_count: number;
  created_at: string;
  updated_at: string;
  // Principal Limits (Phase 2): optional rate/quota/budget configuration for
  // this service, plus its current-calendar-period usage (read-only).
  limits: LimitConfig;
  limits_usage: LimitUsage;
  // Admin-group linkage (Phase C, spec 2026-08-10): the service's
  // containment basis (service_admin_groups, migration v52). Always a
  // non-nil slice ([] for an ungrouped legacy service). system_group_id/
  // _name are the containment root every linked admin group must share (""
  // when ungrouped; system_group_name is a best-effort lookup, empty if the
  // group vanished). Mirrors PortalServer.admin_groups/system_group_id/
  // system_group_name exactly.
  admin_groups: { id: string; name: string }[];
  system_group_id: string;
  system_group_name: string;
};

export type CreateServiceRequest = {
  name: string;
  description?: string;
  status?: string;
  delegates?: ServiceDelegateInput[];
  allowed_models?: string[];
  limits?: LimitConfig;
  // AdminGroupIDs: the admin-tier group(s) the new service is linked to
  // (Phase C, spec 2026-08-10) -- mandatory for EVERY caller, including
  // system_admin; the backend rejects an empty set with
  // service.admin_group_required. Every chosen group must share one parent
  // (system-tier) group, which becomes the service's system_group_id.
  // Mirrors CreateServerRequest.admin_group_ids.
  admin_group_ids?: string[];
  // SystemGroupID: an optional system-admin convenience cross-check -- when
  // set, every chosen admin_group_ids entry's parent must equal it, or the
  // create is rejected (service.admin_group_parent_mismatch).
  system_group_id?: string;
};

// Pointer-semantics on the backend: an omitted field keeps its current value;
// a supplied (possibly empty) delegates/allowed_models array REPLACES it wholesale.
// limits, when present, REPLACES the whole config (an all-zero LimitConfig
// clears every limit — see EMPTY_LIMIT_CONFIG).
export type UpdateServiceRequest = {
  name?: string;
  description?: string;
  status?: string;
  delegates?: ServiceDelegateInput[];
  allowed_models?: string[];
  limits?: LimitConfig;
};

// A Service-Account token: mirrors PortalToken but carries service_id instead
// of a user, and NEVER a secret value (only secret_prefix).
export type ServiceTokenDTO = {
  id: string;
  service_id: string;
  name: string;
  secret_prefix: string;
  status: string;
  scopes: string[];
  expires_at: string | null;
  last_used_at: string | null;
  created_at: string;
  model_override: string;
  model_override_map?: Record<string, string>;
  log_communication: boolean;
  secret: boolean;
};

export type CreateServiceTokenRequest = {
  name: string;
  expires_at?: string | null;
  model_override?: string;
  model_override_map?: Record<string, string>;
  log_communication?: boolean;
  secret?: boolean;
};

// The one-time reveal response for both create and rotate.
export type CreateServiceTokenResponse = {
  token: ServiceTokenDTO;
  secret: string;
};

export function servicesApi(fetcher: Fetcher) {
  return {
    // Service Accounts (Phase 1): admin-created, delegate-managed principals
    // restricted to LLM inference. Mirrors the server endpoints' shape.
    services: () => request<{ data: PortalService[] }>(fetcher, '/api/portal/services'),
    createService: (body: CreateServiceRequest) =>
      request<PortalService>(fetcher, '/api/portal/services', { method: 'POST', body }),
    updateService: (id: string, body: UpdateServiceRequest) =>
      request<PortalService>(fetcher, `/api/portal/services/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body,
      }),
    deleteService: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/services/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    // The admin-tier groups the caller may create/link a service into (Phase
    // C, spec 2026-08-10): system scope -> every admin-tier group; anyone
    // else -> the groups they own or co-manage with can_manage_services.
    // Drives the create-service / linkage-editor picker. Mirrors
    // serverAdminGroupCandidates exactly.
    serviceAdminGroupCandidates: () =>
      request<{ data: AdminGroupCandidate[] }>(
        fetcher,
        '/api/portal/service-admin-group-candidates',
      ).then((r) => r.data),
    // Replaces a service's linked admin-group set (Phase C, spec 2026-08-10).
    // >=1 group required; every chosen group must share one parent
    // (system-tier) group. Mirrors setServerAdminGroups exactly.
    setServiceAdminGroups: (id: string, groupIds: string[]) =>
      request<PortalService>(
        fetcher,
        `/api/portal/services/${encodeURIComponent(id)}/admin-groups`,
        {
          method: 'PUT',
          body: { admin_group_ids: groupIds },
        },
      ),
    serviceTokens: (serviceId: string) =>
      request<{ data: ServiceTokenDTO[] }>(
        fetcher,
        `/api/portal/services/${encodeURIComponent(serviceId)}/tokens`,
      ),
    createServiceToken: (serviceId: string, body: CreateServiceTokenRequest) =>
      request<CreateServiceTokenResponse>(
        fetcher,
        `/api/portal/services/${encodeURIComponent(serviceId)}/tokens`,
        {
          method: 'POST',
          body,
        },
      ),
    rotateServiceToken: (serviceId: string, tokenId: string) =>
      request<CreateServiceTokenResponse>(
        fetcher,
        `/api/portal/services/${encodeURIComponent(serviceId)}/tokens/${encodeURIComponent(tokenId)}/rotate`,
        { method: 'POST' },
      ),
    deleteServiceToken: (serviceId: string, tokenId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/services/${encodeURIComponent(serviceId)}/tokens/${encodeURIComponent(tokenId)}`,
        { method: 'DELETE' },
      ),
  };
}

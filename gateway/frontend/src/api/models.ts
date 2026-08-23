// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, request, subscribeSSE } from './transport';

export type ApplicationType = 'ollama' | 'vllm' | 'llama_cpp' | 'llama_swap' | 'litellm';
export type ApplicationScheme = 'http' | 'https';
export type ApplicationStatus = 'active' | 'disabled';

export type ApplicationHealthMode = 'always_reachable' | 'health_path' | 'model_sync';

export type PortalApplication = {
  id: string;
  server_id: string;
  type: ApplicationType;
  port: number;
  scheme: ApplicationScheme;
  endpoint: string;
  // Optional path suffix appended to the application URL (empty = none).
  app_path_suffix: string;
  // Whether an upstream API token is stored (the token itself is never returned).
  api_token_set: boolean;
  // Header the token is sent under; empty = "Authorization: Bearer <token>".
  api_token_header: string;
  api_flavors: string[];
  priority: number;
  weight: number;
  timeout_ms: number;
  affinity_ttl_seconds: number;
  admission_queue_timeout_seconds: number;
  status: ApplicationStatus;
  always_reachable: boolean;
  health_check_path: string;
  health_check_mode: ApplicationHealthMode;
  // 0 = follow the system-wide health_check_interval_seconds; > 0 = custom cadence.
  health_check_interval_seconds: number;
  // Native passthrough: proxy Codex (/v1/responses) resp. Claude Code
  // (/v1/messages) raw to the upstream instead of translating.
  native_responses: boolean;
  native_messages: boolean;
  // Optional upstream status path the gateway polls to learn which models are
  // loaded (empty disables it); format selects the parser (auto/openai/
  // llama_swap/llama_cpp).
  loaded_models_path: string;
  loaded_models_format: string;
  // Optional upstream path GET to learn context size (llama.cpp /props); empty = off.
  context_probe_path: string;
  // Auto-update metrics: run a benchmark on a schedule + learn speed opportunistically
  // from real traffic. interval 0 = off/unset.
  benchmark_schedule_enabled: boolean;
  benchmark_schedule_interval_seconds: number;
  opportunistic_metrics_enabled: boolean;
  // Gateway-managed TLS proxy-listen port (P4 HTTPS switch); 0 = not yet
  // assigned (the gateway auto-assigns it). Not user-editable in the portal UI.
  proxy_listen_port: number;
  reachable: boolean;
  last_checked_at: string | null;
  created_at: string;
};

export type PortalApplicationListResponse = { data: PortalApplication[] };

export type CreateApplicationRequest = {
  type: ApplicationType;
  port: number;
  scheme: ApplicationScheme;
  api_flavors?: string[];
  priority?: number;
  weight?: number;
  timeout_ms?: number;
  affinity_ttl_seconds?: number;
  admission_queue_timeout_seconds?: number;
  status?: ApplicationStatus;
  always_reachable?: boolean;
  health_check_path?: string;
  health_check_mode?: ApplicationHealthMode;
  health_check_interval_seconds?: number;
  native_responses?: boolean;
  native_messages?: boolean;
  loaded_models_path?: string;
  loaded_models_format?: string;
  context_probe_path?: string;
  app_path_suffix?: string;
  // Write-only upstream API token: omit = none, a value = set it.
  api_token?: string;
  api_token_header?: string;
  benchmark_schedule_enabled?: boolean;
  benchmark_schedule_interval_seconds?: number;
  opportunistic_metrics_enabled?: boolean;
  // Gateway-managed; omit to auto-assign (0).
  proxy_listen_port?: number;
};

export type UpdateApplicationRequest = {
  type?: ApplicationType;
  port?: number;
  scheme?: ApplicationScheme;
  api_flavors?: string[];
  priority?: number;
  weight?: number;
  timeout_ms?: number;
  affinity_ttl_seconds?: number;
  admission_queue_timeout_seconds?: number;
  status?: ApplicationStatus;
  always_reachable?: boolean;
  health_check_path?: string;
  health_check_mode?: ApplicationHealthMode;
  health_check_interval_seconds?: number;
  native_responses?: boolean;
  native_messages?: boolean;
  loaded_models_path?: string;
  loaded_models_format?: string;
  context_probe_path?: string;
  app_path_suffix?: string;
  // Write-only upstream API token: omit = keep the stored token, "" = clear, a value = replace.
  api_token?: string;
  api_token_header?: string;
  benchmark_schedule_enabled?: boolean;
  benchmark_schedule_interval_seconds?: number;
  opportunistic_metrics_enabled?: boolean;
  // Gateway-managed; omit to keep the stored value.
  proxy_listen_port?: number;
};

export type PortalModelMapping = {
  id: string;
  application_id: string;
  gateway_model_name: string;
  app_model_name: string;
  status: ApplicationStatus;
  created_at: string;
  gen_tokens_per_second: number;
  prompt_tokens_per_second: number;
  load_time_ms: number;
  context_size: number;
  is_mtp: boolean;
  vision_capable: boolean;
  energy_wh_per_token: number;
  metrics_locked: boolean;
  metrics_source: string;
  metrics_updated_at?: string | null;
  max_concurrency: number;
  recommended_concurrency: number;
  gen_tokens_per_second_at_capacity: number;
};

export type PortalMappingListResponse = { data: PortalModelMapping[] };

export type CreateMappingRequest = {
  gateway_model_name: string;
  app_model_name: string;
  status?: ApplicationStatus;
  gen_tokens_per_second?: number;
  prompt_tokens_per_second?: number;
  load_time_ms?: number;
  context_size?: number;
  is_mtp?: boolean;
  vision_capable?: boolean;
  energy_wh_per_token?: number;
  metrics_locked?: boolean;
  max_concurrency?: number;
  recommended_concurrency?: number;
  gen_tokens_per_second_at_capacity?: number;
};

export type UpdateMappingRequest = {
  gateway_model_name?: string;
  app_model_name?: string;
  status?: ApplicationStatus;
  gen_tokens_per_second?: number;
  prompt_tokens_per_second?: number;
  load_time_ms?: number;
  context_size?: number;
  is_mtp?: boolean;
  vision_capable?: boolean;
  energy_wh_per_token?: number;
  metrics_locked?: boolean;
  max_concurrency?: number;
  recommended_concurrency?: number;
  gen_tokens_per_second_at_capacity?: number;
};

export type SyncResult = {
  added: number;
  disabled: number;
  unchanged: number;
  conflicted: number;
};

// A model's global visibility (model_settings.visibility). "shown" is listed +
// directly requestable; "hidden" is unlisted but still directly requestable;
// "locked" is unlisted AND only reachable via a group.
export type ModelVisibility = 'shown' | 'hidden' | 'locked';

// One ordered member of a model group: a gateway model NAME (loose reference).
// Array order in PortalModelGroup.members is the priority (index 0 = highest).
export type ModelGroupMember = { member_gateway_name: string };

// A priority-failover model group offered to clients as a synthetic model.
// Requesting the group name routes to the first available member. Visibility is
// NOT a member attribute — it lives on the model (see setModelVisibility).
export type PortalModelGroup = {
  id: string;
  gateway_model_name: string;
  display_name: string;
  status: ApplicationStatus;
  failover_mode: string; // "sticky" | "climb_up"
  // The group NAME's own visibility (a group name is itself a gateway_model_name).
  visibility: ModelVisibility;
  members: ModelGroupMember[];
  // Subgroup-traversal strategy ("depth" | "breadth" | "round_robin", default
  // "round_robin") governing the order in which a member subgroup's own models
  // are flattened into this group's failover candidate list.
  traversal: string;
};

export type PortalModelGroupListResponse = { data: PortalModelGroup[] };

export type CreateModelGroupRequest = {
  gateway_model_name: string;
  display_name?: string;
  status?: ApplicationStatus;
  failover_mode?: string;
  visibility?: ModelVisibility;
  members: ModelGroupMember[];
  traversal?: string;
};

// Partial update (omitted fields left unchanged server-side; a present `members`
// replaces the whole ordered set).
export type UpdateModelGroupRequest = {
  gateway_model_name?: string;
  display_name?: string;
  status?: ApplicationStatus;
  failover_mode?: string;
  visibility?: ModelVisibility;
  members?: ModelGroupMember[];
  traversal?: string;
};

export type PortalRoute = {
  id?: string;
  model: string;
  provider: string;
  host: string;
  status: 'active' | 'watch' | 'standby';
};

export type ModelOption = {
  id: string;
  display_name: string;
  flavors: string[];
  // True when this model is currently loaded on at least one reachable
  // application (requestable without waiting for a load). loaded_on names the
  // servers where it is loaded.
  loaded?: boolean;
  loaded_on?: string[];
  // Count of reachable applications that currently OFFER this model (a real model
  // via its active mappings, or a group via its offerable members). 0 = unknown.
  offered_on_count?: number;
  // The model's global visibility (default "shown"). Only meaningful for a real
  // model; a group row (is_group) has no visibility control.
  visibility?: ModelVisibility;
  // True when this "model" is actually a model group (synthetic offered name).
  is_group?: boolean;
  // True when this model — and, for a group, EVERY offered member — is
  // vision-capable (accepts image input). The backend AND-aggregates this
  // across all offering mappings/members (fail-closed).
  vision?: boolean;
};

export type ModelServerRow = {
  server_id: string;
  server_name: string;
  application_id: string;
  mapping_id: string;
  loaded: boolean;
  can_load: boolean;
  gen_tokens_per_second: number;
  prompt_tokens_per_second: number;
  load_time_ms: number;
  context_size: number;
  max_concurrency: number;
  recommended_concurrency: number;
  gen_tokens_per_second_at_capacity: number;
  is_mtp: boolean;
  // True when this (server, mapping) accepts image input, per the backend's
  // vision_probe_mode detection.
  vision_capable?: boolean;
  metrics_source: string;
  metrics_updated_at?: string | null;
  // Live 1-based rank among this model's offering servers (0 = unknown/unranked).
  priority: number;
};

// One (model, server) a model GROUP can serve, with a live rank across the whole
// group's flattened candidate list (see api.modelGroupServers).
export type GroupModelServerRow = ModelServerRow & { model: string };

export function modelsApi(fetcher: Fetcher) {
  return {
    models: () => request<{ data: ModelOption[] }>(fetcher, '/api/portal/models'),
    // manageModels returns the UNSUPPRESSED model listing (admin only): hidden/
    // locked models are INCLUDED so the management UI can revert them or add them
    // to a group. Feeds ModelList + ModelGroupSection, never the chat picker.
    manageModels: () => request<{ data: ModelOption[] }>(fetcher, '/api/portal/models?manage=1'),
    // The servers offering a gateway model, with per-server benchmark metrics + live loaded-state +
    // a can_load flag. Global (gateway:use); name is a query param (a gateway model name may hold '/').
    modelServers: (name: string) =>
      request<{ data: ModelServerRow[] }>(
        fetcher,
        `/api/portal/model-servers?name=${encodeURIComponent(name)}`,
      ).then((r) => r.data),
    // Every (model, server) a model GROUP can serve (its flattened members), with a
    // live rank across the whole group's candidate list. Global (gateway:use); no
    // SSE — the group-detail view polls this like the model-detail Prio column.
    modelGroupServers: (name: string) =>
      request<{ data: GroupModelServerRow[] }>(
        fetcher,
        `/api/portal/model-group-servers?name=${encodeURIComponent(name)}`,
      ).then((r) => r.data),
    // Subscribe to a model's server-offering list over SSE. Sends a `snapshot` frame on connect,
    // then an `update` frame (the full recomputed list) on each loaded-state change. Mirrors
    // subscribeServerPerf; a malformed frame is swallowed.
    subscribeModelServers: (
      name: string,
      onData: (rows: ModelServerRow[]) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) => {
      const handle = (e: MessageEvent) => {
        try {
          const parsed = JSON.parse(e.data) as { data?: ModelServerRow[] };
          onData(parsed.data ?? []);
        } catch {
          // ignore a malformed frame
        }
      };
      return subscribeSSE(
        `/api/portal/model-servers/events?name=${encodeURIComponent(name)}`,
        { snapshot: handle, update: handle },
        { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') },
      );
    },
    applications: (serverId: string) =>
      request<PortalApplicationListResponse>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/applications`,
      ),
    createApplication: (serverId: string, body: CreateApplicationRequest) =>
      request<PortalApplication>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/applications`,
        {
          method: 'POST',
          body,
        },
      ),
    application: (aid: string) =>
      request<PortalApplication>(fetcher, `/api/portal/applications/${encodeURIComponent(aid)}`),
    updateApplication: (aid: string, body: UpdateApplicationRequest) =>
      request<PortalApplication>(fetcher, `/api/portal/applications/${encodeURIComponent(aid)}`, {
        method: 'PATCH',
        body,
      }),
    deleteApplication: (aid: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/applications/${encodeURIComponent(aid)}`, {
        method: 'DELETE',
      }),
    syncApplicationModels: (aid: string) =>
      request<SyncResult>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(aid)}/sync-models`,
        {
          method: 'POST',
        },
      ),
    mappings: (aid: string) =>
      request<PortalMappingListResponse>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(aid)}/mappings`,
      ),
    createMapping: (aid: string, body: CreateMappingRequest) =>
      request<PortalModelMapping>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(aid)}/mappings`,
        {
          method: 'POST',
          body,
        },
      ),
    updateMapping: (mid: string, body: UpdateMappingRequest) =>
      request<PortalModelMapping>(fetcher, `/api/portal/mappings/${encodeURIComponent(mid)}`, {
        method: 'PATCH',
        body,
      }),
    deleteMapping: (mid: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/mappings/${encodeURIComponent(mid)}`, {
        method: 'DELETE',
      }),
    // Model groups (admin-scoped): priority-failover synthetic models. Members are
    // gateway model names ordered by priority. A PUT replaces the whole group
    // (including its ordered members); DELETE removes it.
    modelGroups: () => request<PortalModelGroupListResponse>(fetcher, '/api/portal/model-groups'),
    createModelGroup: (body: CreateModelGroupRequest) =>
      request<PortalModelGroup>(fetcher, '/api/portal/model-groups', { method: 'POST', body }),
    updateModelGroup: (id: string, body: UpdateModelGroupRequest) =>
      request<PortalModelGroup>(fetcher, `/api/portal/model-groups/${encodeURIComponent(id)}`, {
        method: 'PUT',
        body,
      }),
    deleteModelGroup: (id: string) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/model-groups/${encodeURIComponent(id)}`, {
        method: 'DELETE',
      }),
    // Set a model's global visibility (shown | hidden | locked). Admin-scoped;
    // the {name} path segment is URL-encoded (a gateway name may contain a slash).
    setModelVisibility: (name: string, visibility: ModelVisibility) =>
      request<{ ok: boolean }>(fetcher, `/api/portal/model-settings/${encodeURIComponent(name)}`, {
        method: 'PUT',
        body: { visibility },
      }),
  };
}

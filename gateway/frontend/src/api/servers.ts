// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { type Fetcher, PortalApiError, buildQueryString, request, subscribeSSE } from './transport';
import type { AdminGroupCandidate } from './groups';

export type ServerOwner = {
  id: string;
  email: string;
  display_name: string;
};

export type ServerStatus = 'active' | 'disabled' | 'maintenance';
export type ServerHealthStatus = 'healthy' | 'degraded' | 'unhealthy' | 'unknown';
// Live-derived ServerAgent presence: "active" (reporting within the effective
// window) / "inactive" (a token is configured but not currently reporting) /
// "unconfigured" (no ServerAgent token generated for this server yet).
export type AgentStatus = 'active' | 'inactive' | 'unconfigured';

// Currency unit for cost display + per-server/system-default price entry.
// Mirrors src/currency.ts's CurrencyUnit; re-declared here (not imported) so
// this data layer stays import-free, matching the rest of the file.
export type CurrencyUnit = 'eur' | 'eur_cent' | 'usd' | 'usd_cent';

export type PortalServer = {
  id: string;
  name: string;
  domain: string;
  // Optional path suffix appended to the server URL (empty = none).
  server_path_suffix: string;
  status: ServerStatus;
  health_status: ServerHealthStatus;
  owners: ServerOwner[];
  last_seen_at: string | null;
  created_at: string;
  // NetBird integration. netbird_enabled marks the server as a NetBird peer;
  // netbird_peer_id/netbird_connected surface the enrolled peer + its live link
  // state (both empty/false until the sync loop resolves the peer).
  netbird_enabled: boolean;
  netbird_setup_key_id: string;
  netbird_group_id: string;
  netbird_peer_id: string;
  netbird_connected: boolean;
  // The peer's NetBird POLICY group membership (excluding the tracking group,
  // which the sync mirror already filters out). Always a slice ([] when none).
  netbird_group_ids: { id: string; name: string }[];
  // Provenance: true when the peer/setup-key originated from a gateway-generated
  // setup key (create hook / enroll / regenerate). Pre-checks the "also delete
  // peer" box on server delete; a manually-linked peer stays false.
  netbird_peer_managed: boolean;
  // Per-server policy-management opt-in/opt-out override: "" (follow the
  // effective scope), "include" (opt in — meaningful in "selected" scope), or
  // "exclude" (opt out — meaningful in "all" scope).
  netbird_policy_override: string;
  // Per-server ICMP ping-allow: when on (and NetBird-enabled), the gateway can
  // ping this server (contributes to the op-gw-ping-servers destination set).
  netbird_allow_ping: boolean;
  // Per-server ICMP ping-EXCLUDE: the opt-out counterpart, meaningful when
  // "Alle Server pingbar" is on system-wide (removes this server from the
  // op-gw-ping-servers destination set). Mutually exclusive with allow_ping.
  netbird_ping_exclude: boolean;
  // Live-derived ServerAgent presence, computed from the effective (per-server
  // or system-default) agent_presence_timeout_seconds window.
  agent_status: AgentStatus;
  // Per-server agent-presence-timeout override (seconds); 0 = follow the
  // system-wide agent_presence_timeout_seconds setting.
  agent_presence_timeout_seconds: number;
  // Energy-attribution config (purely additive — no engine consumes these
  // yet). All default 0 = "unset / use default".
  estimated_watts: number;
  idle_watts: number;
  price_per_kwh: number;
  pue: number;
  // Currency unit price_per_kwh is entered/displayed in; defaults to "eur_cent"
  // server-side, so this is always present (never undefined).
  price_unit: CurrencyUnit;
  // Admin-group linkage (Phase B, spec 2026-08-10): the server's containment
  // basis (server_admin_groups, migration v50). Always a non-nil slice ([]
  // for an ungrouped legacy server). system_group_id/_name are the
  // containment root every linked admin group must share ("" when ungrouped;
  // system_group_name is a best-effort lookup, empty if the group vanished).
  admin_groups: { id: string; name: string }[];
  system_group_id: string;
  system_group_name: string;
  // Per-server certificate-management opt-in/opt-out override (certificates
  // feature): "" (follow cert_server_scope), "include" (opt in — meaningful
  // in "selected" scope), or "exclude" (opt out — meaningful in "all"
  // scope). Optional so existing PortalServer literal fixtures across the
  // test suite compile unchanged.
  certificate_override?: string;
  // Per-server https-auto-switch opt-in/opt-out override (P4): "" (follow
  // cert_https_switch_mode), "include" (opt in — meaningful in "selected"
  // mode), or "exclude" (opt out — meaningful in "auto" mode). Optional so
  // existing PortalServer literal fixtures across the test suite compile
  // unchanged.
  https_switch_override?: string;
  // Managed-runtime-only restriction (Task 6, migration 66; surfaced to the
  // portal in the agent-runtime-manager feature): true when this server only
  // accepts server_agent applications (CreateApplication otherwise rejects a
  // non-server_agent type with ErrServerManagedRuntimeOnly / the
  // application.managed_runtime_only error code). Always present on the real
  // wire DTO (no omitempty server-side); optional here so existing
  // PortalServer literal fixtures across the test suite compile unchanged
  // (mirrors certificate_override/https_switch_override above).
  managed_runtime_only?: boolean;
  // RuntimeMaxProcesses caps how many agent-managed runtime processes may run
  // concurrently on this server (0 = unlimited; Task 6, migration 66 --
  // declared alongside managed_runtime_only server-side). Drives the "server
  // limits" tab's process-limit field (agent-runtime-manager feature, Task
  // 21). Always present on the real wire DTO (no omitempty --
  // ServerDTO.RuntimeMaxProcesses in service.go); optional here so existing
  // PortalServer literal fixtures across the test suite compile unchanged
  // (mirrors managed_runtime_only above).
  runtime_max_processes?: number;
};

// CreateServer response = the server DTO plus the display-once setup key and a
// best-effort create-hook error (both present only on create, never on the
// list/get DTO).
export type CreateServerResponse = PortalServer & {
  netbird_setup_key?: string;
  // Display-once ready-to-paste `netbird up …` command (contains the setup key);
  // present only when a setup key was generated. Never persisted.
  netbird_setup_command?: string;
  netbird_error?: string;
};

export type AgentConfigMaterial = {
  gateway_url: string;
  ca_file: string;
  ca_cache_file: string;
  ca_pem: string;
};

export type AgentTokenDTO = {
  exists: boolean;
  secret_prefix?: string;
  last_used_at?: string | null;
  created_at?: string | null;
  updated_at?: string | null;
  config: AgentConfigMaterial;
  agent_download_base: string;
};

export type GenerateAgentTokenResponse = { secret: string; token: AgentTokenDTO };

export type AgentBinaryEntry = {
  os: string;
  arch: string;
  filename: string;
  size: number;
  sha256: string;
};
export type AgentBinaries = {
  agent_version: string;
  go_version: string;
  built_at: string;
  binaries: AgentBinaryEntry[];
  netbird_agent_download_only: boolean;
  agent_download_base: string;
};

export type CreateServerRequest = {
  name: string;
  domain: string;
  server_path_suffix?: string;
  status?: string;
  owner_ids?: string[];
  netbird_enabled?: boolean;
  netbird_policy_override?: string;
  // 0/omitted = follow the system-wide agent_presence_timeout_seconds.
  agent_presence_timeout_seconds?: number;
  // Energy-attribution config (purely additive — no engine consumes these
  // yet). 0/omitted = unset / use default.
  estimated_watts?: number;
  idle_watts?: number;
  price_per_kwh?: number;
  pue?: number;
  price_unit?: CurrencyUnit;
  // AdminGroupIDs: the admin-tier group(s) the new server is linked to
  // (Phase B, spec 2026-08-10) -- mandatory for EVERY caller, including
  // system_admin; the backend rejects an empty set with
  // server.admin_group_required. Every chosen group must share one parent
  // (system-tier) group, which becomes the server's system_group_id.
  admin_group_ids: string[];
  // SystemGroupID: an optional system-admin convenience cross-check -- when
  // set, every chosen admin_group_ids entry's parent must equal it, or the
  // create is rejected (server.admin_group_parent_mismatch).
  system_group_id?: string;
  // Restrict the new server to server_agent applications (Task 6, migration 66;
  // the server form's checkbox, issue #25). Mirrors the Go
  // CreateServerRequest.ManagedRuntimeOnly `*bool`, but create has no
  // "leave unchanged" case: a brand-new row's column defaults to false, so nil
  // and false land in the same place. The form therefore always states it. The
  // reason to accept it here at all is provisioning: an operator who wants a
  // managed-only server should not have to create it and then PATCH it.
  managed_runtime_only?: boolean;
};

export type UpdateServerRequest = {
  name?: string;
  domain?: string;
  server_path_suffix?: string;
  status?: string;
  owner_ids?: string[];
  netbird_enabled?: boolean;
  // 0 = follow the system-wide agent_presence_timeout_seconds; > 0 = custom.
  agent_presence_timeout_seconds?: number;
  // Energy-attribution config (purely additive — no engine consumes these
  // yet). 0 = unset / use default.
  estimated_watts?: number;
  idle_watts?: number;
  price_per_kwh?: number;
  pue?: number;
  price_unit?: CurrencyUnit;
  // The "server limits" tab's process-limit field (agent-runtime-manager
  // feature, Task 21): caps concurrent agent-managed runtime processes; a
  // supplied 0 resets to unlimited (mirrors the Go
  // UpdateServerRequest.RuntimeMaxProcesses `*int` -- undefined here means
  // "leave unchanged", matching every other optional field in this request).
  // Must be >= 0 (else server.runtime_limit_invalid).
  runtime_max_processes?: number;
  // Restrict this server to server_agent applications (Task 6, migration 66;
  // the server form's checkbox, issue #25). Mirrors the Go
  // UpdateServerRequest.ManagedRuntimeOnly `*bool` -- and here, unlike on
  // create, ALL THREE states are distinct and reachable: undefined = "leave
  // unchanged", true = restrict, false = LIFT the restriction. The distinction
  // is not decorative. UpdateServer applies `if req.ManagedRuntimeOnly != nil`,
  // so a PATCH sent for some unrelated reason -- a rename, a status change --
  // that carries a `false` it never meant to state silently clears the
  // operator's policy and returns an ordinary 200. Any sender must therefore
  // leave this key OUT unless it is deliberately answering the question; see
  // submitEdit in ServerList.tsx, which compares against the value the form was
  // seeded with rather than sending its checkbox state unconditionally.
  managed_runtime_only?: boolean;
};

// One resource group a server OWNER may enter their server into (spec
// 2026-08-11-resource-groups-server-owner-self-service): id + name + whether the
// server is currently a member. Mirrors portal.ServerResourceGroupDTO. Drives the
// per-server "Ressourcengruppen" join/leave sub-view in ServerList; never exposes
// any other server of the resource group.
export type ServerResourceGroup = { id: string; name: string; member: boolean };

// Per-server live performance telemetry. Mirrors the backend
// perfGPUDTO/perfNetDTO/perfPointDTO/perfHistoryDTO (perf_endpoints.go) JSON tags
// byte-for-byte.
export type PerfGPU = {
  index: number;
  name: string;
  uuid: string;
  util_pct: number;
  mem_used_bytes: number;
  mem_total_bytes: number;
  temp_c: number;
  vram_temp_c: number;
  power_w: number;
  fan_pct: number;
};
export type PerfNet = { name: string; rx_bytes: number; tx_bytes: number };
export type PerfPoint = {
  t: string;
  cpu_util_pct: number;
  cpu_cores: number[];
  mem_used_bytes: number;
  mem_total_bytes: number;
  swap_used_bytes: number;
  swap_total_bytes: number;
  load1: number;
  load5: number;
  load15: number;
  active_requests: number;
  queue_depth: number;
  gpus: PerfGPU[];
  net: PerfNet[];
  // Nullable host power watts (CPU package + total system). null = not measured.
  cpu_power_w: number | null;
  system_power_w: number | null;
  // Nullable CPU package temperature in degrees Celsius. null = not measured.
  cpu_temp_c: number | null;
};
export type PerfHistory = { points: PerfPoint[]; from: string; to: string };

// One point in a server's availability history: the derived health state plus
// whether the ServerAgent was reporting at that time. `health` is "" only for a
// pre-persist sample; a persisted point is one of the ServerHealthStatus values.
export type AvailabilityPoint = {
  t: string;
  health: ServerHealthStatus | '';
  reachable_count: number;
  active_count: number;
  agent_reporting: boolean;
  // True when the server's linked NetBird peer was connected at this time
  // (netbird_connected column). Rendered only when the server has a peer linked.
  netbird_connected: boolean;
  // True when this point's RAW predecessor was more than the backend gap floor
  // (10 min) away — an observer gap. The interval leading into a gap_before point
  // is painted "unknown" (the gateway was not sampling) rather than held.
  gap_before: boolean;
};
export type AvailabilityHistory = { points: AvailabilityPoint[]; from: string; to: string };

// Static hardware inventory reported by a server's ServerAgent (GET /hardware).
export type HardwareCPU = {
  model: string;
  vendor: string;
  physical_cores: number;
  logical_threads: number;
  base_mhz: number;
};
export type HardwareMemoryModule = {
  locator?: string;
  size_bytes: number;
  type?: string;
  speed_mhz?: number;
};
export type HardwareMemory = { total_bytes: number; modules?: HardwareMemoryModule[] };
export type HardwareMainboard = { vendor: string; product: string; version: string };
export type HardwareBIOS = { vendor: string; version: string };
// pci_bus_id is the card's PCI address ("00000000:65:00.0"), NVIDIA only and
// omitted by every other collector and by any agent older than this field --
// so every consumer must render without it. It is a display/disambiguation
// aid: on the 4x/8x identical-card hosts that are the normal AI-server build
// it is the only handle that maps to a physical slot and survives index
// renumbering across reboots. It is deliberately NOT an identity -- GPU rows,
// budgets and admission all key on `index`.
export type HardwareGPU = {
  index: number;
  name: string;
  uuid?: string;
  driver_version?: string;
  memory_total_bytes: number;
  pci_bus_id?: string;
};
export type HardwareReport = {
  collected_at: string;
  agent_version: string;
  os: string;
  arch: string;
  kernel?: string;
  hostname?: string;
  cpu: HardwareCPU;
  memory: HardwareMemory;
  mainboard: HardwareMainboard;
  bios: HardwareBIOS;
  gpus: HardwareGPU[];
};
// `report` is `| null` as well as optional for the same reason as
// RuntimeReport.report (api/runtime.ts): `report,omitempty` on a
// `json.RawMessage` only omits an EMPTY blob, and hardware_endpoints.go writes
// an empty stored ReportJSON out as the JSON literal `null`. Absent, `null`
// and an object are all on the wire.
export type HardwareResponse = {
  available: boolean;
  collected_at?: string;
  updated_at?: string;
  report?: HardwareReport | null;
};

// A gateway model one specific server offers (see api.serverModels), used to
// narrow the model dropdown once a server override is selected (token or chat).
export type ServerModelOption = { id: string; display_name: string };

// One benchmarked mapping's measured metrics (mirrors the Go BenchmarkResult).
export type BenchmarkResult = {
  mapping_id: string;
  gateway_model_name: string;
  gen_tokens_per_second: number;
  prompt_tokens_per_second: number;
  load_time_ms: number;
  context_size?: number;
  max_concurrency?: number;
  recommended_concurrency?: number;
  gen_tokens_per_second_at_capacity?: number;
  vision_capable?: boolean;
  error?: string;
};

// The per-server benchmark run status (mirrors the Go BenchmarkStatus). A POST
// start returns this with running=true; the status GET polls it to completion.
export type BenchmarkStatus = {
  running: boolean;
  server_id: string;
  scope: string;
  started_at?: string;
  total: number;
  done: number;
  mode?: string;
  current_concurrency?: number;
  error?: string;
  results?: BenchmarkResult[];
};

// One level of a capacity ramp (mirrors the Go CapacityLevelDTO).
export type CapacityLevelDTO = {
  concurrency: number;
  aggregate_tokens_per_second: number;
  per_request_tokens_per_second: number;
  mean_latency_ms: number;
  successes: number;
  errors: number;
  vram_free_pct?: number;
  ram_free_pct?: number;
  requests_deferred?: number;
  requests_processing?: number;
  total_slots?: number;
  stop_reason?: string;
};

// A decoded capacity report attached to a kind:"capacity" benchmark run
// (mirrors the Go CapacityReportDTO).
export type CapacityReportDTO = {
  max_concurrency: number;
  recommended_concurrency: number;
  gen_tokens_per_second_at_capacity: number;
  memory_observed: boolean;
  levels?: CapacityLevelDTO[];
};

// One persisted benchmark run for a mapping (mirrors the Go BenchmarkRunDTO).
// Returned newest-first from GET /api/portal/mappings/{id}/benchmarks.
export type BenchmarkRunDTO = {
  id: string;
  mapping_id: string;
  server_id: string;
  created_at: string;
  gen_tokens_per_second: number;
  prompt_tokens_per_second: number;
  load_time_ms: number;
  context_size: number;
  error: string;
  kind: string;
  capacity?: CapacityReportDTO;
  // Present only for a kind==="vision" row, carrying that run's verdict (false for
  // both a definitive "not capable" AND an inconclusive probe — check `error`,
  // non-empty only when inconclusive, to tell the two apart).
  vision_capable?: boolean;
};

// The optional `?mode=...` query suffix shared by the three benchmark-start
// endpoints below.
function benchmarkModeSuffix(mode?: string): string {
  return mode ? `?mode=${encodeURIComponent(mode)}` : '';
}

export function serversApi(fetcher: Fetcher) {
  return {
    // GET the decimated per-server performance history window.
    serverPerfHistory: (id: string, window: string) =>
      request<PerfHistory>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(id)}/perf${buildQueryString({ window })}`,
      ),
    // GET the transition-reduced per-server availability history window.
    serverAvailability: (id: string, window: string) =>
      request<AvailabilityHistory>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(id)}/availability${buildQueryString({ window })}`,
      ),
    // GET the latest static hardware inventory for a server (available:false when
    // no ServerAgent has reported yet).
    serverHardware: (id: string) =>
      request<HardwareResponse>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}/hardware`),
    // Subscribe to a server's live performance SSE. The stream sends a `snapshot`
    // frame ({points}) on connect, then a `sample` frame (one PerfPoint) per
    // publish. Mirrors subscribeActivity (withCredentials, named-event listeners,
    // exp-backoff reconnect, idempotent unsubscribe) but parses each payload; a
    // malformed frame is swallowed rather than thrown out of the handler.
    subscribeServerPerf: (
      id: string,
      onSnapshot: (points: PerfPoint[]) => void,
      onSample: (point: PerfPoint) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) =>
      subscribeSSE(
        `/api/portal/servers/${encodeURIComponent(id)}/perf/events`,
        {
          snapshot: (e) => {
            try {
              const parsed = JSON.parse(e.data) as { points?: PerfPoint[] };
              onSnapshot(parsed.points ?? []);
            } catch {
              // ignore a malformed frame
            }
          },
          sample: (e) => {
            try {
              onSample(JSON.parse(e.data) as PerfPoint);
            } catch {
              // ignore a malformed frame
            }
          },
        },
        { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') },
      ),
    servers: () => request<{ data: PortalServer[] }>(fetcher, '/api/portal/servers'),
    createServer: (body: CreateServerRequest) =>
      request<CreateServerResponse>(fetcher, '/api/portal/servers', { method: 'POST', body }),
    updateServer: (id: string, body: UpdateServerRequest) =>
      request<PortalServer>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}`, {
        method: 'PATCH',
        body,
      }),
    deleteServer: (id: string, deletePeer?: boolean) =>
      request<{ ok: boolean; netbird_peer_delete_failed?: boolean }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(id)}${deletePeer ? '?delete_peer=true' : ''}`,
        { method: 'DELETE' },
      ),
    // The admin-tier groups the caller may create/link a server into (Phase B,
    // spec 2026-08-10): system scope -> every admin-tier group; anyone else ->
    // the groups they own or co-manage with can_manage_servers. Drives the
    // create-server / linkage-editor picker.
    serverAdminGroupCandidates: () =>
      request<{ data: AdminGroupCandidate[] }>(
        fetcher,
        '/api/portal/server-admin-group-candidates',
      ).then((r) => r.data),
    // Replaces a server's linked admin-group set (Phase B, spec 2026-08-10).
    // >=1 group required; every chosen group must share one parent
    // (system-tier) group.
    setServerAdminGroups: (id: string, groupIds: string[]) =>
      request<PortalServer>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}/admin-groups`, {
        method: 'PUT',
        body: { admin_group_ids: groupIds },
      }),
    // Server-owner self-service resource-group membership (spec
    // 2026-08-11-resource-groups-server-owner-self-service): a server OWNER lists
    // the resource groups linked to an admin group they are a MEMBER of (each with
    // a `member` flag) and joins/leaves their OWN server. Neither admin nor
    // resource-management permission required.
    serverResourceGroups: (serverId: string) =>
      request<{ data: ServerResourceGroup[] }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/resource-groups`,
      ).then((r) => r.data),
    joinResourceGroup: (serverId: string, rgId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/resource-groups/${encodeURIComponent(rgId)}`,
        { method: 'PUT' },
      ),
    leaveResourceGroup: (serverId: string, rgId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/resource-groups/${encodeURIComponent(rgId)}`,
        { method: 'DELETE' },
      ),
    agentTokenStatus: (serverId: string) =>
      request<AgentTokenDTO>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/agent-token`,
      ),
    generateAgentToken: (serverId: string) =>
      request<GenerateAgentTokenResponse>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/agent-token`,
        {
          method: 'POST',
        },
      ),
    revokeAgentToken: (serverId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/agent-token`,
        {
          method: 'DELETE',
        },
      ),
    agentBinaries: () => request<AgentBinaries>(fetcher, '/api/portal/agent-binaries'),
    downloadAgentBinary: async (target: string): Promise<Blob> => {
      const response = await fetcher(`/api/portal/agent-binaries/${encodeURIComponent(target)}`, {
        credentials: 'include',
      });
      if (!response.ok) {
        const payload = await response.json().catch(() => null);
        const error =
          payload && typeof payload === 'object'
            ? (payload as { error?: { code?: string; message?: string } }).error
            : undefined;
        throw new PortalApiError(
          response.status,
          error?.code ?? 'request.failed',
          error?.message ?? 'download failed',
        );
      }
      return response.blob();
    },
    // The distinct gateway models a specific server offers, gated on the caller
    // MANAGING that server (404-no-leak for a non-manager or unknown id). Used to
    // narrow the model dropdown once a server override (token/chat) is selected.
    serverModels: (serverId: string) =>
      request<{ data: ServerModelOption[] }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/models`,
      ).then((r) => r.data),
    // Load a mapping's model on its server (owner/admin). 202 + BenchmarkStatus (running=true); poll
    // benchmarkStatus(server_id) to completion, then read results[0]. A 409 = server in use / a run
    // already running. Does not persist.
    loadModel: (mappingId: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(mappingId)}/load`,
        {
          method: 'POST',
        },
      ),
    // Benchmark runners. Each POST starts a per-server run and returns 202 with the
    // initial BenchmarkStatus (running=true, carrying server_id for the status poll);
    // benchmarkStatus polls that run to completion (running=false).
    benchmarkMapping: (id: string, mode?: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(id)}/benchmark${benchmarkModeSuffix(mode)}`,
        { method: 'POST' },
      ),
    benchmarkApplication: (id: string, mode?: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(id)}/benchmark${benchmarkModeSuffix(mode)}`,
        { method: 'POST' },
      ),
    benchmarkServer: (id: string, mode?: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(id)}/benchmark${benchmarkModeSuffix(mode)}`,
        { method: 'POST' },
      ),
    benchmarkStatus: (serverId: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/benchmark/status`,
      ),
    // Manual context-size probe: warm-loads the model + reads its context. 202 + initial
    // BenchmarkStatus (running=true); poll benchmarkStatus(serverId) to completion, then read
    // results[].context_size. Does NOT persist — the caller fills the form field.
    probeMappingContext: (id: string) =>
      request<BenchmarkStatus>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(id)}/probe-context`,
        { method: 'POST' },
      ),
    // GET the per-mapping benchmark run history (newest-first).
    mappingBenchmarks: (mappingId: string) =>
      request<{ data: BenchmarkRunDTO[] }>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(mappingId)}/benchmarks`,
      ).then((r) => r.data),
    // GET the running benchmarks the caller may see (one per server) for the live list indicator.
    activeBenchmarks: () =>
      request<{ data: BenchmarkStatus[] }>(fetcher, '/api/portal/benchmarks/active').then(
        (r) => r.data,
      ),
    // Subscribe to a server's live benchmark SSE. The stream sends a `snapshot`
    // frame on connect, then a `progress` frame per publish — BOTH carry the full
    // BenchmarkStatus JSON, so both drive onStatus. Mirrors subscribeServerPerf
    // (withCredentials, named-event listeners, exp-backoff reconnect, idempotent
    // unsubscribe); a malformed frame is swallowed rather than thrown out of the
    // handler. This is the LIVE VIEW ONLY — the status poll remains the completion
    // authority (an SSE terminal frame can be dropped under buffer pressure).
    subscribeBenchmark: (
      serverId: string,
      onStatus: (status: BenchmarkStatus) => void,
      onConn?: (status: 'open' | 'error') => void,
    ): (() => void) => {
      const handle = (e: MessageEvent) => {
        try {
          onStatus(JSON.parse(e.data) as BenchmarkStatus);
        } catch {
          // ignore a malformed frame
        }
      };
      return subscribeSSE(
        `/api/portal/servers/${encodeURIComponent(serverId)}/benchmark/events`,
        { snapshot: handle, progress: handle },
        { onOpen: () => onConn?.('open'), onError: () => onConn?.('error') },
      );
    },
    // Owner/admin-scoped dedicated save for the server edit form's "Energy & cost"
    // section: a full-replace of the four energy columns, independent of the main
    // server PATCH. Returns the fresh server DTO.
    setServerEnergy: (
      id: string,
      estimatedWatts: number,
      idleWatts: number,
      pricePerKwh: number,
      pue: number,
      priceUnit: CurrencyUnit,
    ) =>
      request<PortalServer>(fetcher, `/api/portal/servers/${encodeURIComponent(id)}/energy`, {
        method: 'PUT',
        body: {
          estimated_watts: estimatedWatts,
          idle_watts: idleWatts,
          price_per_kwh: pricePerKwh,
          pue: pue,
          price_unit: priceUnit,
        },
      }),
  };
}

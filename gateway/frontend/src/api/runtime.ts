// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

// Agent-managed runtime domain module (agent-runtime-manager feature, portal
// Task 19): mapping-scoped launch specs, application-scoped co-residency +
// warnings, server-scoped GPU budgets + the file-mode report view, and the
// live per-server runtime-status SSE stream. Every shape here mirrors the Go
// DTOs in gateway/backend/internal/portal/service_runtime.go and the SSE
// payload in gateway/backend/internal/gateway/{runtime_registry,
// portal_runtime_endpoints}.go byte-for-byte -- see those files, not this
// comment, as the source of truth if the two ever drift.

import { type Fetcher, request, subscribeSSE } from './transport';

// One per-GPU VRAM demand row on a runtime spec. vram_estimate_mb is
// operator-owned (round-tripped from a PUT verbatim); vram_measured_mb is
// agent-owned and read-only here -- the agent's telemetry write-back sets it
// after the process actually starts, and a PUT's value for it is always
// ignored server-side (see PutRuntimeSpecRequest below).
export interface RuntimeSpecGPU {
  index: number;
  vram_estimate_mb: number;
  vram_measured_mb: number;
}

// A mapping's agent-managed launch spec (GET/PUT/DELETE
// /api/portal/mappings/{id}/runtime-spec). `configured: false` means the
// mapping has no spec row yet -- the only signal for "not configured"; every
// other field is then a zero value (gpus/args/env still non-nil empty), and
// `id` is absent (the backend's `omitempty`).
export interface RuntimeSpec {
  configured: boolean;
  id?: string;
  mapping_id: string;
  enabled: boolean;
  binary: string;
  args: string[];
  env: Record<string, string>;
  work_dir: string;
  listen_port: number;
  health_path: string;
  health_timeout_seconds: number;
  startup_timeout_seconds: number;
  idle_timeout_seconds: number;
  admission_wait_timeout_seconds: number;
  pinned: boolean;
  admin_state: string;
  vram_locked: boolean;
  gpus: RuntimeSpecGPU[];
}

// A full-document upsert of a runtime spec: every field is applied verbatim
// (after backend validation/defaulting), never merged against the stored
// row. The one exception is each GPU entry's vram_measured_mb, which the
// backend ALWAYS ignores on write (agent-owned) even though the request
// shape still carries the field -- see RuntimeSpec.gpus / PutRuntimeSpec's
// VRAM ownership rule in service_runtime.go.
export type PutRuntimeSpecRequest = Omit<RuntimeSpec, 'configured' | 'id' | 'mapping_id'>;

// An application's allowed co-residency pairs (its own mappings only), each
// pair canonically ordered (pairs[i][0] < pairs[i][1]). Always a non-nil
// array, even when empty.
export interface CoResidency {
  pairs: [string, string][];
}

// One per-GPU VRAM budget row on a server. expected_uuid/expected_name are a
// purely descriptive drift detector snapshotted server-side from live
// telemetry -- never client-writable, on create or on later PUTs.
export interface GPUBudget {
  index: number;
  budget_mb: number;
  expected_uuid: string;
  expected_name: string;
}

// One agent-managed process's last failure, as published to the live SSE
// stream. Volatile only (never persisted) -- see runtimeStatusRegistry's doc
// in runtime_registry.go for why, including stderr_tail.
export interface RuntimeError {
  message: string;
  at: string;
  exit_code: number;
  failures: number;
  stderr_tail?: string;
}

// One agent-managed model process's live state, as published on the
// `snapshot`/`update` SSE stream (GET .../runtime/events). Deliberately has
// NO gpu field: measured VRAM reaches the UI through the spec's own
// gpus[].vram_measured_mb after the write-back, never through this stream.
export interface RuntimeStatus {
  spec_id: string;
  model: string;
  state: string;
  since: string;
  pid?: number;
  port?: number;
  in_flight: number;
  restarts: number;
  last_error?: RuntimeError;
}

// The parsed content of a file-mode agent's runtime report, nested under
// RuntimeReport.report (see below) -- mirrors the agent's upward
// `agentRuntimeReport` wire struct (gateway/backend/internal/gateway/
// agent_runtime.go): which config source produced it, when it was
// (re)loaded, any parse error from that load, and the sanitized (env values
// masked) effective config. `config` is intentionally untyped: it is an
// opaque passthrough of the agent-runtime-config document shape (the same
// one AgentRuntimeConfigDTO describes), not something this foundation layer
// needs to interpret field-by-field.
export interface RuntimeReportContent {
  source: string;
  collected_at: string;
  parse_error?: string;
  config: unknown;
}

// The GET /api/portal/servers/{id}/runtime/report body: the operator-facing,
// read-only view of a file-mode agent's latest reported runtime
// configuration. `available: false` means no report has ever been stored
// (not an error) -- mirrors the hardware panel's HardwareResponse shape
// (api/servers.ts) exactly: available/collected_at?/updated_at?/report?.
// agent_version/agent_features are read from the server's LATEST telemetry
// row regardless of whether a runtime report was ever stored (so a
// feature-mismatch banner needs no new endpoint) and are always present
// (never omitted), unlike the report-derived fields.
export interface RuntimeReport {
  available: boolean;
  collected_at?: string;
  updated_at?: string;
  report?: RuntimeReportContent;
  agent_version: string;
  agent_features: string[];
}

export function runtimeApi(fetcher: Fetcher) {
  return {
    runtimeSpec: (mappingId: string) =>
      request<RuntimeSpec>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`,
      ),
    putRuntimeSpec: (mappingId: string, body: PutRuntimeSpecRequest) =>
      request<RuntimeSpec>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`,
        {
          method: 'PUT',
          body,
        },
      ),
    deleteRuntimeSpec: (mappingId: string) =>
      request<{ ok: boolean }>(
        fetcher,
        `/api/portal/mappings/${encodeURIComponent(mappingId)}/runtime-spec`,
        { method: 'DELETE' },
      ),
    runtimeCoresidency: (appId: string) =>
      request<CoResidency>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(appId)}/runtime/coresidency`,
      ),
    putRuntimeCoresidency: (appId: string, body: CoResidency) =>
      request<CoResidency>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(appId)}/runtime/coresidency`,
        { method: 'PUT', body },
      ),
    runtimeWarnings: (appId: string) =>
      request<{ warnings: string[] }>(
        fetcher,
        `/api/portal/applications/${encodeURIComponent(appId)}/runtime/warnings`,
      ),
    gpuBudgets: (serverId: string) =>
      request<{ budgets: GPUBudget[] }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/gpu-budgets`,
      ),
    putGpuBudgets: (serverId: string, body: { budgets: GPUBudget[] }) =>
      request<{ budgets: GPUBudget[] }>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/gpu-budgets`,
        { method: 'PUT', body },
      ),
    runtimeReport: (serverId: string) =>
      request<RuntimeReport>(
        fetcher,
        `/api/portal/servers/${encodeURIComponent(serverId)}/runtime/report`,
      ),
    // Subscribe to a server's live agent-managed-runtime-status SSE. The
    // stream sends a `snapshot` frame on connect, then a full-replacement
    // `update` frame per publish -- both wrapped as {runtimes: [...]}
    // (runtimeStatusEventDTO in portal_runtime_endpoints.go), NOT {data:
    // [...]} like the model-servers/perf streams. Mirrors subscribeServerPerf
    // (withCredentials, named-event listeners, exp-backoff reconnect,
    // idempotent unsubscribe); a malformed frame is swallowed.
    subscribeRuntimeStatus: (
      serverId: string,
      onData: (rows: RuntimeStatus[]) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) => {
      const handle = (e: MessageEvent) => {
        try {
          const parsed = JSON.parse(e.data) as { runtimes?: RuntimeStatus[] };
          onData(parsed.runtimes ?? []);
        } catch {
          // ignore a malformed frame
        }
      };
      return subscribeSSE(
        `/api/portal/servers/${encodeURIComponent(serverId)}/runtime/events`,
        { snapshot: handle, update: handle },
        { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') },
      );
    },
  };
}

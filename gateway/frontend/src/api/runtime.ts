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
  // Turns the spec's GPU list from a declaration into an enforcement: the
  // agent sets the vendor-appropriate visibility variable
  // (CUDA_VISIBLE_DEVICES on NVIDIA, ROCR_VISIBLE_DEVICES on AMD; nothing on
  // Apple or a host with no recognised GPU stack) for this spec's child, from
  // this spec's own HOST gpu indices. Two combinations the backend refuses:
  // on with an empty `gpus` (an empty visibility value hides EVERY card), and
  // on together with one of those variables hand-set in `env`.
  set_visible_devices: boolean;
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

// One GPU's measured VRAM as carried on THIS status frame (mirrors the Go
// RuntimeGPUStatusDTO). Only a strictly positive measurement appears: 0 means
// unknown throughout the runtime feature and is dropped on ingest.
export interface RuntimeGPUStatus {
  index: number;
  vram_measured_mb: number;
}

// One agent-managed model process's live state, as published on the
// `snapshot`/`update` SSE stream (GET .../runtime/events).
//
// `gpus` + `measured_at` are a WATERMARK and are omitted together: a frame that
// measured nothing (no measurer on the host, or a spec with no live process)
// carries neither, so a timestamp never appears with nothing to be fresh about.
// `measured_at` is the GATEWAY's arrival time for the frame that carried the
// measurement, not the agent's self-reported `reported_at`. This is the only
// place a measurement's AGE is available: the spec's own
// gpus[].vram_measured_mb (from the write-back) stays the durable value but
// carries no timestamp and is not rewritten when it does not change, so reading
// it back tells you nothing about when it was taken.
export interface RuntimeStatus {
  spec_id: string;
  model: string;
  state: string;
  since: string;
  pid?: number;
  port?: number;
  in_flight: number;
  restarts: number;
  gpus?: RuntimeGPUStatus[];
  measured_at?: string;
  last_error?: RuntimeError;
}

// One unit of a managed model process's output on the live log stream
// (GET .../runtime/logs?spec_id=…). Mirrors RuntimeLogEntryDTO
// (gateway/backend/internal/gateway/runtime_logs.go), which in turn mirrors the
// agent's own runtime.LogEntry.
//
// Exactly one of `text` and `event` is meaningful per entry:
//
//  - `text` is verbatim process output (stdout and stderr interleaved as the
//    process produced them). NEVER render it as HTML.
//  - `event` is a structural boundary between two runs of the same spec, from
//    a CLOSED set the backend allow-lists: 'started' (with `pid`), 'exited'
//    (with `exit_code`), and 'start_failed' -- a generation whose exec itself
//    failed, so it has no pid, will produce no output, and has no exit code
//    coming. The portal owns the wording, which is why the set is closed -- an
//    agent must not be able to put free text where the operator reads a
//    portal-authored sentence.
//
// `command` appears ONLY on an opening marker ('started' / 'start_failed') and
// carries that generation's resolved launch command. Render it as part of that
// marker's block: it describes the process whose output follows, and only that
// one.
//
// `dropped_bytes` is the overflow marker: N bytes the process printed are
// missing immediately BEFORE this entry. It is produced wherever output can be
// lost (the agent's retention buffer, the agent's send queue, the gateway's
// per-subscriber queue) and deliberately does not say which -- the reader only
// needs to know the gap is there. It must always be rendered: a gap shown as
// silence is a lie about what the process printed, and silence is what the
// operator is trying to interpret.
export interface RuntimeLogEntry {
  pid?: number;
  at?: string;
  text?: string;
  dropped_bytes?: number;
  event?: string;
  exit_code?: number;
  command?: RuntimeLogCommand;
}

// The RESOLVED launch command of ONE generation: what the agent actually
// exec'd, with every `${PORT}`, `${MODEL}`, `${HOST_GPU_IDS}` and
// `${AGENT_ENV:NAME}` already substituted. Mirrors RuntimeLogCommandDTO
// (gateway/backend/internal/gateway/runtime_logs.go), which mirrors the agent's
// runtime.ResolvedCommand — and that file
// (server-agent/internal/runtime/command.go) is where the masking rule is
// documented in full.
//
// It arrives on the generation's OPENING MARKER entry and carries no pid of its
// own: the marker has one. That is what makes attribution structural rather
// than a rule — a crash loop shows each attempt's own command beside its own
// output, including the fact that `${PORT}` differed between attempts.
//
// What a renderer must get right, because each of these is a way to lie to the
// operator:
//
//  - **Render it with its marker, never on its own.** A command detached from
//    the marker it arrived on would be a claim about which generation it
//    describes, and that claim is exactly what this design removed the need for.
//  - `masked` and `env_redacted` are two DIFFERENT reasons for withholding,
//    one flag each, independent, and both can be set on the same command. Say
//    each one in its own words rather than hoping the operator recognises a
//    mask — and never say one when the other is what happened:
//      - `masked: true` — at least one value was replaced by its own
//        `${AGENT_ENV:NAME}` placeholder. The placeholder IS the mask: it is
//        unmistakably not a value, and it names a variable on the AI server the
//        operator can go and check.
//      - `env_redacted: true` — the agent takes its specs from a local file, so
//        at least one value the spec's own `env` sets was withheld in full (key
//        intact, value replaced by a mask). There is no placeholder and nothing
//        to look up: the value is in the operator's own document.
//    The masking scope is **this reported command**, and nothing wider. A model
//    server that prints its own argv or environment at startup puts the
//    resolved value into the stream this command rides on, and into
//    `last_error.stderr_tail`; nothing agent-side can mask what the child chose
//    to print. Any wording here must keep that scope.
//  - `truncated: true` means arguments or env entries are MISSING. State it,
//    on the same reasoning as `dropped_bytes`: a shortened list rendered as a
//    complete one is a lie about what ran.
//  - **An opening marker can be evicted** from the agent's bounded buffer, so
//    output can arrive with no command anywhere before it. That must be stated,
//    not left to read as "there was no command" — same rule as `dropped_bytes`.
//  - There is deliberately NO copy affordance, and adding one would be a
//    regression, not a feature. Even unmasked, this is not a runnable command
//    line: `env` REPLACES the environment rather than adding to it, the port
//    was ephemeral and is stale by the time anyone reads it, and a
//    `set_visible_devices` child renumbers its GPUs from zero. A copy button
//    would promise reproduction and deliver a broken paste.
//
// `binary`, `args`, `work_dir` and `env` are agent/operator-authored strings.
// Render them as text; NEVER as HTML.
export interface RuntimeLogCommand {
  binary?: string;
  args?: string[];
  work_dir?: string;
  env?: string[];
  masked?: boolean;
  env_redacted?: boolean;
  truncated?: boolean;
}

// One `log` SSE frame: the entries an agent flushed for one spec.
//
// `scrollback: true` marks the one-shot replay of the agent's retained history
// that a subscribe produces. The view must RESET on it, not append: an agent
// reconnect delivers a fresh scrollback, and appending would duplicate the
// history. An EMPTY scrollback is itself an answer -- "the agent's buffer holds
// nothing", which is what an agent restart leaves behind -- and must be
// rendered as such rather than looking like "nothing has arrived yet".
//
// The resolved launch command travels inside `entries`, on each generation's
// opening marker (`RuntimeLogEntry.command`), not as a field here: a batch is a
// time-slice that can span two generations, and a command belongs to exactly
// one.
export interface RuntimeLogBatch {
  spec_id: string;
  scrollback?: boolean;
  entries: RuntimeLogEntry[];
}

// Whether a live log stream is possible for this server right now, from the
// `status` SSE frame. Three states because an empty log window has three
// causes needing three different things from the operator:
//
//  - 'streaming'   the agent is connected and understands the request, so
//                  silence from here on genuinely means the process is quiet;
//  - 'unsupported' the agent is connected but does not declare the
//                  runtime_logs feature -- an older binary that will never
//                  answer. Tell the operator to update it;
//  - 'offline'     no live agent connection at all, so there is nothing to ask
//                  over. Also the permanent state of an agent configured with
//                  the POST transport, which has no gateway->agent direction.
export type RuntimeLogState = 'streaming' | 'unsupported' | 'offline';

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
//
// `report` is `| null` as well as optional, and that is not defensive padding:
// `report,omitempty` on a `json.RawMessage` omits the field only when the blob
// is EMPTY, and the builder never leaves it empty on an existing row -- an
// empty stored ReportJSON is written out as the JSON literal `null`
// (service_runtime.go's ServerRuntimeReportView). So all three of absent,
// `null` and an object are on the wire, and a type naming only two of them is
// a lie the `?.` at every read site silently covers for.
export interface RuntimeReport {
  available: boolean;
  collected_at?: string;
  updated_at?: string;
  report?: RuntimeReportContent | null;
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
    // Subscribe to ONE managed process's live stdout+stderr. The stream sends
    // a `status` frame first (can this even work -- see RuntimeLogState), then
    // a `log` frame per agent flush: the agent's retained scrollback, then live
    // output.
    //
    // Opening this subscription is what MAKES the agent stream: the gateway
    // asks it to start on the first viewer and to stop when the last one
    // leaves. So the returned unsubscribe is not merely cleanup -- calling it
    // is what turns the stream back off, and failing to call it leaves an agent
    // producing output nobody is reading.
    //
    // Mirrors subscribeRuntimeStatus (withCredentials, named-event listeners,
    // exp-backoff reconnect, idempotent unsubscribe); a malformed frame is
    // swallowed.
    subscribeRuntimeLogs: (
      serverId: string,
      specId: string,
      onBatch: (batch: RuntimeLogBatch) => void,
      onState: (state: RuntimeLogState) => void,
      onStatus?: (status: 'open' | 'error') => void,
    ): (() => void) => {
      return subscribeSSE(
        `/api/portal/servers/${encodeURIComponent(serverId)}/runtime/logs?spec_id=${encodeURIComponent(specId)}`,
        {
          status: (e: MessageEvent) => {
            try {
              const parsed = JSON.parse(e.data) as { state?: RuntimeLogState };
              if (parsed.state) onState(parsed.state);
            } catch {
              // ignore a malformed frame
            }
          },
          log: (e: MessageEvent) => {
            try {
              const parsed = JSON.parse(e.data) as RuntimeLogBatch;
              // `entries` is non-nil on the wire, but a frame that lost it is
              // still usable as a scrollback RESET signal -- which is the one
              // thing the view must not miss.
              onBatch({ ...parsed, entries: parsed.entries ?? [] });
            } catch {
              // ignore a malformed frame
            }
          },
        },
        { onOpen: () => onStatus?.('open'), onError: () => onStatus?.('error') },
      );
    },
  };
}

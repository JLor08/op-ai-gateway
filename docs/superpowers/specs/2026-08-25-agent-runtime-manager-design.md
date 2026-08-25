# Agent Runtime Manager — Design Spec

Date: 2026-08-25
Status: approved in brainstorming; awaiting written-spec review
Scope: sub-projects T1 (agent runtime manager + gateway data model/endpoints) and
T2 (portal runtime admin) of the llama-swap-replacement effort.

## 0. Sub-project decomposition (context)

| | Sub-project | Status |
|---|---|---|
| T1 | Agent runtime manager: process supervisor, local request router, desired-state sync, co-residency enforcement, status reporting; gateway data model + agent endpoints | **this spec** |
| T2 | Portal runtime admin: spec CRUD, lower-triangle matrix UI, live process view, entry wiring | **this spec** |
| T3 | Log streaming: stdout/stderr agent→gateway, per-process SSE log viewer in the portal | later, own spec (`runtime_logs` feature flag reserved) |
| T4 | Gateway routing integration: cold-load-aware deadlines, idle/preload intelligence, matrix as capacity signal, benchmark-watchdog decoupling, double-load fix | later, own spec |

T1+T2 together are a complete llama-swap replacement: on-demand loading happens
inside the agent (request for model X → not loaded → admission check → start →
wait healthy → proxy), exactly as llama-swap does today. T4 is the later
optimization where the gateway contributes its routing intelligence.

## 1. Goals and non-goals

**Goals**

- The server-agent starts/stops the applications that load and serve models
  (llama.cpp server, vLLM, …), several in parallel, and routes each incoming
  request to the right child process.
- Which model may be co-resident with which other model is an explicit
  operator decision (lower-triangle pair matrix) backed by per-GPU VRAM
  arithmetic and a process-count limit.
- The gateway portal gains a dedicated runtime admin for this, entered via a
  new application type and an optional per-server switch.
- Models visibly report *loading* and *last-load-failed*, not just *loaded*.
- Gateway and agent negotiate supported features at connect time; only
  mutually supported features activate. The agent version is bumped on every
  agent change.
- The model-to-command configuration can alternatively live in a local file on
  the AI server; then the agent reports it upward and the portal shows it
  read-only.

**Non-goals (deliberately out of T1+T2)**

- Streaming process logs to the gateway/portal (T3). T1 only keeps a local
  ring buffer and attaches a bounded stderr tail to load errors.
- Gateway-side cold-load-aware deadlines, preloading, benchmark-watchdog
  decoupling, the cross-server double-load fix (T4).
- Per-model "loading" badges for classic (non-managed) llama-swap
  applications: today's loaded-model parser flattens llama-swap's `state`
  field away; extending it is a separate small step.
- Resolver changes. The gateway routing pipeline treats the managed runtime
  as one ordinary application with ordinary mappings.

## 2. Decisions (with rationale)

1. **Request path: one router port on the agent.** The agent listens on a
   single port, reads the `model` field, ensures the process runs, and
   reverse-proxies. Children bind loopback-only. For the gateway this is one
   `Application` row per server; routing, TLS proxy, firewall/mesh surface all
   stay unchanged. Trade-off accepted: double forwarding; the agent is a
   single point of failure for the server.
2. **Command ownership: gateway specification + agent-local policy.** The full
   launch specification (binary, args, env, port, health, timeouts) lives in
   the gateway DB and is portal-maintained. The agent enforces a local policy
   from its own config: a binary allowlist and permitted work/model
   directories. **An empty allowlist starts nothing** (with a distinct,
   visible status reason). No shell interpreter — direct exec with an argv
   array; the agent runs as an unprivileged user. The server operator controls
   *what* may run at all; the gateway decides *when and how*.
3. **Matrix + per-GPU VRAM budget + process limit.** The pairwise matrix alone
   cannot prevent three pairwise-compatible models from exceeding VRAM
   together. All three gates must pass (see §5).
4. **Eviction: idle first, busy waits.** Evict processes serving no request
   (oldest-idle first); never evict pinned ones. If blocked by busy processes,
   the request queues up to a configurable timeout, then fails with a stable
   error. Stops always drain: in-flight responses/streams finish first.
5. **Portal entry: application type + server switch.** New application type
   `server_agent`; its model view opens the runtime admin instead of the
   mapping form. A server flag `managed_runtime_only` additionally blocks
   creating other applications and jumps straight into the runtime admin.
   Classic applications may coexist on a server (migration path: llama-swap
   and managed runtime run side by side).
6. **Authorization: exactly like model-mapping writes.** `system` scope, the
   server owner list, or admin-group delegation with `can_manage_servers`;
   plain `admin` is not sufficient. No new RBAC rule — the hard boundary is
   the agent-local allowlist.
7. **Multi-GPU from day one.** Budgets and spec VRAM demands are per GPU
   index. Disjoint GPUs do not compete; tensor-parallel specs list several
   GPUs with per-GPU demands.
8. **Feature negotiation via named flags, never version comparison.**
   Behavior gates on string-equal feature names present on *both* sides. The
   version number is for humans (display, debugging).
9. **Realtime over WS via full-config push; declarative admin overrides.**
   Manual start/stop are desired-state overrides (`none | force_running |
   force_stopped`) stored in the DB — never fire-once commands (a command sent
   during a reconnect would be silently lost). WS-connected agents receive the
   *full config as frame payload* (zero extra round trips); POST agents poll.
   State transitions trigger an immediate out-of-band telemetry sample so
   portal badges flip without waiting for the tick.
10. **Config source per agent: `gateway` (default) or `file`.** In file mode
    the agent reads the identical JSON document from a local file, reports the
    effective config upward (env **values redacted**, keys visible), and the
    portal renders read-only — including hiding start/stop: whoever owns the
    config owns the operations.

## 3. Architecture

### 3.1 Agent: new package `internal/runtime`

Modeled on `internal/proxy` (focused files, imports only `internal/gwapi`
among module-internal packages; archtest allowlist updated in the same
change).

| File | Responsibility |
|---|---|
| `manager.go` | Owns child processes. Reconciles against desired state with the proxy manager's generation discipline (a late process exit never clobbers a successor). Per-spec `RuntimeState` (§7). |
| `policy.go` | Admission decision: matrix pairs, per-GPU budgets, process limit, eviction victim selection. Pure functions over snapshots — the most heavily tested part. |
| `router.go` | The single listener: health path, `/running` (llama-swap shape), model-routed reverse proxy (§6). |
| `config_client.go` | `GET /api/agent/v1/runtime-config` with `If-None-Match` (mirrors `proxy.RoutesClient`); persists the last good document atomically on disk as a start-time fallback. In file mode: reads/watches the local file instead (same schema, same validation, same reconciler — the mode is a source switch, not a second code path). |
| `policy_local.go` | The agent-side boundary: binary allowlist + permitted directories from `OP_AGENT_RUNTIME_*`. |
| `logs.go` | Bounded per-process ring buffer over stdout/stderr. Local-only in T1; the capture point exists from the start so T3 does not rebuild process startup. |

### 3.2 Agent: integration seams (no new transport)

1. `internal/client/ws.go`: a third doorbell/payload channel `RuntimeUpdates()`
   — buffered(1), non-blocking send, coalescing, like `certUpdates` — plus the
   `runtime_config` frame payload handling (§9).
2. `internal/agent/agent.go`: `Deps.RuntimeDriver` with `Sync(ctx)` +
   `Status()`, exactly like `Deps.ProxyDriver`. `nil` ⇒ feature entirely
   absent: no sample field, no behavior change (the established no-op
   invariant).
3. `internal/sample/sample.go`: `Runtimes []RuntimeSample` (`omitempty`); the
   manager fills the existing `LoadedModels` **authoritatively** (only truly
   loaded models — `starting` does not count).

### 3.3 Gateway

| Place | Change |
|---|---|
| `internal/routing/store.go` | New domain types `RuntimeSpec` (exactly one per `ModelMapping`), `RuntimeSpecGPU`, `CoResidencyRule`, `ServerGPUBudget`, `ServerRuntimeReport`. New `AIServer` fields (`RuntimeMaxProcesses`, `ManagedRuntimeOnly`). New `Application.Type` value `server_agent`. |
| `internal/store/` | Repos for all three drivers + migrations 65–67 (§4). |
| `internal/portal/` | CRUD + validation; authorization identical to mapping writes. |
| `internal/gateway/` | Agent endpoints `GET /api/agent/v1/runtime-config` (ETag), `GET /api/agent/v1/features`, `POST /api/agent/v1/runtime-report`; WS frames `runtime_config` (gw→agent) and `runtime_report` (agent→gw); portal endpoints incl. `GET /api/portal/servers/{id}/runtime/events` (SSE); volatile runtime-status registry. |
| `internal/routing/` | **Nothing.** The resolver sees an ordinary application with mappings. |
| `cmd/gateway/main.go` | `server_agent` → `provider.NewOpenAICompatibleClient` (the router port speaks the OpenAI dialect, like `llama_swap`/`vllm`/`llama_cpp`/`litellm`). |

### 3.4 Data flow

- **Desired state:** portal write → DB → WS `runtime_config` push (or POST
  poll) → `Manager.Reconcile`. Doorbell lost ⇒ the poll cadence catches up;
  gateway down ⇒ the agent runs on its disk copy.
- **Actual state:** manager snapshot → sample (1 s + immediate on transition)
  → gateway volatile registry → portal SSE.
- **Request:** client → gateway (routing unchanged) → agent router port →
  `EnsureRunning` → loopback child.

## 4. Data model

All new columns follow ADR-005 lessons: float columns are `double precision`
from the start; all VRAM values are **MB as `integer`** (2^31 MB ≈ 2 PB — the
int4-overflow class of bug cannot recur).

### 4.1 `agent_runtime_specs` — one launch spec per mapping

`mapping_id` unique, `on delete cascade` (mapping deleted ⇒ spec gone).

| Column | Purpose |
|---|---|
| `enabled` | default **off**: a half-finished row must trigger nothing |
| `binary`, `args`, `work_dir` | `args` is an opaque JSON-array string (the `netbird_group_ids` pattern) |
| `env` | JSON object, **no secrets** (§4.6) |
| `listen_port` | `0` = agent picks a free loopback port (the normal case) |
| `health_path`, `health_timeout_seconds`, `startup_timeout_seconds` | readiness/failure criteria |
| `idle_timeout_seconds` | `0` = never unload |
| `admission_wait_timeout_seconds` | how long a request may queue when blocked by busy/pinned processes; `0` = wait until the client disconnects (mirrors the gateway's `admission_queue_timeout_seconds` semantics) |
| `pinned` | starts with the agent, never evicted |
| `admin_state` | `'' (= none) \| force_running \| force_stopped` (decision 9) |
| `vram_locked` | measured values do not overwrite operator values (mirrors `metrics_locked`), per spec — an operator thinks "pin this model's numbers", not "pin GPU 2" |

### 4.2 `agent_runtime_spec_gpus` — per-GPU VRAM demand

PK `(spec_id, gpu_index)`; `vram_estimate_mb` (operator), `vram_measured_mb`
(agent measurement). A model may span GPUs unevenly (tensor parallel).

### 4.3 `agent_coresidency_rules` — the lower-triangle matrix

PK `(application_id, mapping_a_id, mapping_b_id)` with the canonical order
`mapping_a_id < mapping_b_id` enforced at write time — one row per unordered
pair, double-occupancy structurally impossible. **Row present = pair
allowed.** No `allowed` column: "not co-resident" is the default
*structurally* (exactly today's llama-swap behavior until someone opens a
cell), and the table stays small. Accepted trade-off: an explicit "forbidden"
is indistinguishable from "never considered" and has no `updated_at`. The
diagonal stays empty: multiple instances of one spec are a concurrency
question (`max_concurrency` on the mapping), not co-residency.

### 4.4 `ai_server_gpu_budgets` — per-GPU budgets

PK `(server_id, gpu_index)`; `budget_mb`, plus `expected_uuid`,
`expected_name` snapshotted at creation. Telemetry already reports live
index/UUID/name per GPU; a mismatch surfaces as a **warning, never a
blocker** (a driver update renumbering cards must not take the server out of
service). AMD (`cardN`, no UUID) and Apple (always index 0, unified memory)
skip the UUID check entirely rather than warn falsely.

### 4.5 Server columns and report table

- `ai_servers.runtime_max_processes` (0 = unlimited),
  `ai_servers.managed_runtime_only` (portal entry switch).
- `server_runtime_reports`: 1:1 per server, upsert-overwrite, validated opaque
  JSON blob — mirrors `server_hardware` exactly. Holds the file-mode report
  (§10).

### 4.6 Secrets: never in the gateway

`env` values are referential: `{"HF_TOKEN": "${AGENT_ENV:HF_TOKEN}"}`. The
placeholder resolves from the **agent's own process environment/config file**.
The gateway never stores or transports a secret; the portal cannot leak one;
the no-plaintext-secrets rule needs no new exception. Accepted cost: a secret
must exist on the AI server; the portal cannot set it.

### 4.7 Migrations

- **65 `agent_runtime_manager`**: `agent_runtime_specs`,
  `agent_runtime_spec_gpus`, `agent_coresidency_rules`.
- **66 `server_runtime_limits`**: `ai_server_gpu_budgets` + the two
  `ai_servers` columns.
- **67 `server_runtime_reports`**: the report table.

Split so a binary rollback survives each half independently. Append-only; 64
is the last shipped migration.

## 5. Admission rule

Spec S may start next to the running set R iff **all three** hold:

1. For every `r ∈ R` a matrix row exists for the pair (S, r). The matrix
   expresses *intent* and covers non-VRAM constraints (PCIe bandwidth, system
   RAM, CPU); the arithmetic is the *veto*. Both must pass.
2. `|R| + 1 ≤ runtime_max_processes` (0 = unlimited).
3. For **every GPU g that S touches**: Σ VRAM over S and all `r ∈ R` touching
   g ≤ `budget(g)`. Disjoint GPUs do not compete.

**Unknown VRAM: measure instead of guessing.** A spec with unknown demand on
GPU g may start only **alone on g** (state `pending_vram_unknown` otherwise,
with the reason visible in the portal). The agent then measures the real
usage (`nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory` — exact,
because the agent knows its own child PIDs; AMD `rocm-smi --showpids`
best-effort; Apple unified memory: operator estimates stay authoritative and
the portal says so) and writes it back. "Unknown" is thus a self-resolving
transient, not a permanent hole in the OOM protection. This is deliberately
**not** fail-open — fail-open here would hollow out exactly the protection the
VRAM budget was chosen for. Measurement is not a feature flag (flags negotiate
protocol, not hardware); an agent without a measurement path simply omits the
field.

## 6. Runtime behavior

### 6.1 Router port

| Path | Behavior |
|---|---|
| health path (as configured on the application) | `200` while the router itself is up. Reachability means "the router accepts", not "a model is warm" — otherwise a cold server falls out of routing and can never warm up. **Never blocks during loads** (§8: this property is why llama-swap escapes the health-flip trap and a direct llama.cpp does not). |
| `/running` (llama-swap shape) | currently loaded upstream names; the gateway's existing `LoadedModelsFormat: "llama_swap"` detection keeps working unchanged — a second, independent source for the same truth besides telemetry. Never blocks. |
| everything else | model-routed forwarding: path, method, headers unchanged, to the child. |

The request body is buffered (bounded) to read `model` — resolved against
`app_model_name` (that is what the gateway sends upstream:
`resolver.go` sets `ProviderModel = mapping.AppModelName` and the provider
uses it). The response is **never** buffered: `httputil.ReverseProxy` with
`FlushInterval: -1` so SSE tokens pass through as they are produced.

### 6.2 Request flow

1. `model` → spec via `app_model_name`. None or not `enabled` ⇒
   `runtime.model_not_managed`.
2. Running and healthy ⇒ forward.
3. Not running ⇒ admission rule (§5) against the running set:
   - free ⇒ start, wait for health (≤ `startup_timeout_seconds`), forward;
   - blocked ⇒ pick eviction victims (idle only, oldest-idle first, never
     pinned), drain-stop them, re-evaluate;
   - everything blocking is busy or pinned ⇒ queue the request, re-evaluate on
     every completion, fail at the spec's `admission_wait_timeout_seconds`
     (`0` = wait until the client disconnects).

### 6.3 Serialized admission owner

**All admission decisions run through one serialized owner** — a
state machine in a single goroutine, not a mutex around scattered checks. Two
concurrent requests independently computing "still fits" and both starting is
the most likely severe bug in this feature; computing and starting must be one
indivisible operation. Only forwarding is concurrent (it touches in-flight
counters, never the state machine).

### 6.4 Stop, crash, idle

- Drain-stop: no new requests for the spec; in-flight ones finish (bounded
  drain grace, cf. `proxy.shutdownGrace`); then SIGTERM, after a grace period
  SIGKILL.
- Child dies mid-request ⇒ `runtime.upstream_gone`; the spec enters crash
  backoff (base/cap/stable-reset, the WS sender's discipline) so a
  misconfigured model cannot busy-loop the machine.
- Idle ticker unloads specs with `idle_timeout_seconds > 0`, no in-flight
  request, no activity for the duration. Never pinned ones.

### 6.5 Stable error codes (agent-emitted, gateway envelope shape)

The agent reproduces the `{"error":{"code","message"}}` envelope (the module
imports nothing from the gateway; `ws.go` already mirrors the WS envelope the
same way).

| Code | HTTP | Meaning |
|---|---|---|
| `runtime.model_not_managed` | 404 | no active launch spec for this model |
| `runtime.start_failed` | 502 | process exited or never became healthy |
| `runtime.start_timeout` | 504 | `startup_timeout_seconds` elapsed (distinct diagnosis) |
| `runtime.admission_blocked` | 503 | no slot freed within the wait window |
| `runtime.not_permitted` | 502 | agent policy refused binary/directory — a configuration error, visible in the portal, not transient |
| `runtime.upstream_gone` | 502 | child died during the request |

## 7. Visible load lifecycle

Per-spec state machine (the user-facing form of `RuntimeState`):

`stopped`, `starting` (**loading** — process up, health not yet green),
`running` (loaded), `draining`, `backoff` (crash loop wait),
`start_failed` / `crashed` (**last load failed**), `pending_vram_unknown`,
`not_permitted`.

- **`last_error` is cleared only by the next successful start**, not by state
  changes: a spec can be `stopped` and still show "last load attempt failed,
  yesterday 14:32, exit code 1". Content: message, timestamp, exit code,
  failure count, and a **bounded stderr tail (~2 KB)** — error context (the
  one `CUDA error: out of memory` line), deliberately not T3 log streaming.
- Transport: `RuntimeSample` on the 1 s sample (`state`, `since`, `pid`,
  `port`, `in_flight`, `restarts`, `last_error`) + an immediate coalesced
  sample on every transition.
- Gateway-side storage: **volatile RAM only** (like active requests), never
  the DB. Deliberate: stderr tails can contain prompt fragments from chatty
  model servers, and the capture policy forbids such content at rest outside
  opt-in capture. A gateway restart forgets the status; the next sample (≤1 s)
  repopulates it; the agent itself retains `last_error` until it restarts.
- Routing unchanged: the flat `loaded_models` list carries only truly loaded
  models — `starting` does not count (prefer-loaded must not route to a model
  that cannot answer yet).

## 8. Timeout behavior

Background: a verified 125-finding inventory of every timer on the path
client → nginx → gateway → upstream (2026-08-25, workflow run; durable facts
below). The two primary cold-load/TTFT killers today:

| Path | Timer | Default | Client sees |
|---|---|---|---|
| non-streaming (all upstream types) | `Target.Timeout` = application `timeout_ms`, a **total** deadline, never reset (`provider/openai_compatible.go:35`, `ollama.go:34`, buffered native `native_passthrough.go:221`) | 30 s | `502 provider.timeout` on every cold load > 30 s |
| streaming | idle watchdog `OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT` (`stream_session.go:132`, `native_passthrough.go:217`) | 120 s | translate: 200 + terminal frame `provider.stream_idle_timeout`; native pre-headers: `502 provider.unavailable` (mislabeled; fix spun off separately) |

A long time-to-first-token (large prefill) is the same failure class as a
cold load: a silent window with no bytes. Secondary traps: the 3 s health
probe flips a blocking app unreachable after one failed cycle (whole server
if it is the only app); the benchmark's own 120 s watchdog means models
loading > ~2 min never get a `load_time_ms`; `warmCallTimeout` (60 s,
hardcoded) defeats climb-up warming for large models (fix spun off);
swap-protection routes a concurrent same-model request to a second server
(double load).

**T1 measures**

1. Router health/`/running` never block (§6.1) — inherits the llama-swap
   property that avoids the health-flip trap by construction.
2. Write-time validation: an application's `timeout_ms` must exceed the
   largest `startup_timeout_seconds` among its mappings, else a visible portal
   warning. Necessary because the gateway timer keeps running while the agent
   router holds the request — T1 alone does not heal the 30 s case. The
   `server_agent` application type gets a `timeout_ms` default of
   **600 000 ms (10 min)** instead of 30 000.
3. Streaming heartbeats: for a request with `"stream": true` whose model needs
   a start (or whose child has not produced bytes yet), the router commits
   `200` + SSE headers once admission succeeds, emits `: keepalive` comment
   lines every 10 s during the silent window, then splices the child's
   stream. If the start fails or the child errors, the router emits a
   terminal SSE error event carrying the §6.5 code instead. Accepted
   trade-off (stated, not hidden): once heartbeats began, a non-200 upstream
   status can no longer be relayed as an HTTP status — it arrives as an
   in-stream error event, the same shape the gateway's own translate path
   uses for mid-stream failures. The heartbeat verifiably re-arms the
   native-path watchdog (byte-based re-arm, `native_passthrough.go:314` — the
   path Codex and Claude Code use) and nginx (3600 s, uncritical anyway).
4. Honest limit, documented: heartbeats do not help the translate watchdog
   (event-based reset; SSE comments are skipped by the scanner) nor the
   non-streaming total deadline. The real fix — deadlines computed from
   `load_time_ms` and `prompt_tokens_per_second` × prompt estimate, using the
   authoritative loaded-state the agent now provides — is T4, together with
   benchmark-watchdog decoupling and the double-load vector.

Immediate operational relief (config-only, independent of T1): raise
`timeout_ms` on applications serving large models; raise
`OP_AI_GATEWAY_STREAM_IDLE_TIMEOUT` (cost: real hangs detected later).

## 9. Feature negotiation and agent versioning

**Named feature flags decide; version numbers explain.** Behavior hangs
exclusively on string-equal feature names, never on version comparison
(fragile under forks/backports). One new flag in T1: `runtime_manager`. T3
reserves `runtime_logs`. One flag per shipped capability, not per plan.

- **Agent → gateway:** the sample's existing (today always empty)
  `capabilities` object: `{"features":["runtime_manager"],"agent_version":"0.2.0"}`.
  No wire change; both transports carry it already; the gateway already
  persists it (`server_telemetry.capabilities`).
- **Gateway → agent:** `GET /api/agent/v1/features`, ETag-conditional (the
  `ca`/`proxy-routes` shape), fetched at startup and on every WS reconnect.
  Deliberately no hello frame: works identically for POST and WS agents, is
  cacheable, needs no connection state machine.
- **Intersection:** a feature is active iff both lists contain its name.
  Deterministic, computed independently on both sides.
  - agent sends no/empty `features` ⇒ gateway reads ∅ (old agent behaves as
    today);
  - agent gets 404 on `/features` ⇒ agent reads ∅ (old gateway unharmed);
  - unknown names are ignored (the WS read loop's existing forward-compat
    contract).
- **Visibility:** if a runtime config exists but the agent lacks
  `runtime_manager`, the server's agent panel and the runtime admin say so
  explicitly (reported version, declared features, active features, and what
  the gateway wanted but the agent cannot do). Never a silent no-op.
- **Wiring:** `main.go` wires the runtime driver only when `runtime_manager`
  is mutually active; otherwise `Deps.RuntimeDriver` stays `nil` (full no-op
  invariant).
- **Versioning:** `agent.Version` bumps on every agent change — MINOR for a
  new feature flag, PATCH otherwise. A registry
  (`internal/agent/features.go`: `{Name, Since}`) plus a test enforcing valid
  SemVer, `Since ≤ Version`, unique snake_case names — forgetting the bump
  with a new flag fails a test (the frozen-allowlist principle). Honest
  limit: a forgotten PATCH bump after a bugfix is not machine-detectable;
  it becomes an AGENTS.md rule and PR-checklist item, not a fake gate.

## 10. Realtime channel and config source

### 10.1 WS full-config push (decision 9)

- After every runtime write the gateway sends WS-connected agents that
  declared `runtime_manager` a **`runtime_config` frame whose `data` is the
  complete document + its ETag**. The agent applies it straight from the
  frame — zero extra round trips. Full document, not a delta: every frame is
  self-contained and idempotent, last-wins; the agent applies iff the ETag
  differs from its current one.
- `GET /runtime-config` remains the resync/fallback path: startup, every
  reconnect (the `certUpdates` pattern), POST-only agents, disk fallback.
  Missed frames are harmless by construction. Old agents discard unknown
  frame types (verified contract, `ws.go`).
- Pure command frames were considered and rejected: a command sent during a
  reconnect is lost, demanding acks/retries/dedup — while a persisted desired
  state must exist anyway for resync. If a genuinely imperative action ever
  appears, the frame namespace supports `runtime_command` behind its own
  feature flag; every T1 action (start, stop, restart-as-sequence) is
  state-shaped.
- Reverse direction latency: state transitions trigger an immediate coalesced
  sample (§7), so a portal click feels like network latency, not tick cadence.

### 10.2 Config source: `gateway` | `file` (decision 10)

- `OP_AGENT_RUNTIME_SOURCE = gateway | file` (default `gateway`);
  `OP_AGENT_RUNTIME_CONFIG` points at the local file in file mode.
- The file uses **exactly the runtime-config JSON schema** (§11): one parser,
  one validation, one reconciler; the mode is a source switch. Hot reload via
  mtime poll on the existing cadence; a broken file keeps the last good
  config running and reports the parse error.
- The binary allowlist applies in both modes (one enforcement path).
- **Upward report (file mode):** the agent reports the effective config —
  SystemReport-style: on start, on file change, on WS reconnect; POST
  transport also on the 30-min cadence as backstop. WS frame
  `runtime_report`, POST `POST /api/agent/v1/runtime-report`; sent only when
  the gateway declared `runtime_manager`. **Env values are redacted before
  sending** (keys visible, values masked) — a local file may legitimately
  contain plaintext secrets, and they must never reach the gateway. The
  report carries `runtime_source`, load timestamp, and any parse error.
  Stored in `server_runtime_reports` (§4.5).
- **Portal in file mode:** read-only rendering of specs/matrix/budgets from
  the report, banner "configuration managed locally on the AI server",
  **no start/stop buttons** (the admin override is part of the gateway
  document, which file mode does not consume; a dead button is worse than
  none). Live status/badges/`last_error` work unchanged (they ride the
  sample). If gateway-side specs exist for a file-mode server, the portal
  marks them explicitly as ineffective (the §9 visibility principle). The
  agent discards incoming `runtime_config` frames in file mode; the gateway
  stops sending once the report tells it the mode — doubly harmless.

## 11. The runtime-config contract

`GET /api/agent/v1/runtime-config` (agent-token bearer, ETag over a canonical
serialization — the ETag is the version counter; also the WS frame payload
and the file-mode schema):

```json
{
  "router_listen": 8081,
  "max_processes": 3,
  "gpu_budgets": [{"index": 0, "budget_mb": 46000}, {"index": 1, "budget_mb": 46000}],
  "specs": [
    { "id": "…", "model": "qwen-coder", "upstream_model": "qwen2.5-coder-32b",
      "binary": "/usr/bin/vllm", "args": ["--tensor-parallel-size", "2"],
      "env": { "HF_TOKEN": "${AGENT_ENV:HF_TOKEN}" },
      "gpus": [{"index": 0, "vram_mb": 22000}, {"index": 1, "vram_mb": 21500}],
      "listen_port": 0, "health_path": "/health", "health_timeout_seconds": 5,
      "startup_timeout_seconds": 180, "idle_timeout_seconds": 900,
      "admission_wait_timeout_seconds": 0,
      "pinned": false, "admin_state": "" }
  ],
  "coresident": [["spec-a", "spec-b"]]
}
```

## 12. Portal UI (T2)

### 12.1 Navigation

No new top-level view (no `views.tsx` entry ⇒ no new RBAC surface; gates come
from the existing server authorization server-side). The runtime admin lives
in the drill-down `ServerList → ApplicationSection → RuntimeAdminSection`: a
`server_agent` application's model level renders `RuntimeAdminSection`
instead of `MappingSection` (own breadcrumb, like the others).
`managed_runtime_only` jumps straight there and blocks creating other
applications with an explanation.

### 12.2 `RuntimeAdminSection` — four areas

1. **Launch specs**: mappings with spec (model, binary, GPUs+VRAM, idle
   timeout, pinned, enabled), status badge (§7); one form maintains
   mapping + spec together.
2. **Triangle matrix**: rows 2…n, columns 1…n−1, only cells below the
   diagonal; click toggles the pair. Tooltip shows the pair's VRAM sum per
   GPU against the budget (advisory — the agent's arithmetic vetoes anyway).
   Wide matrices scroll in their own container.
3. **Server limits**: per-GPU budgets prefilled from live telemetry
   (`MemTotalBytes`, name, UUID), process limit, UUID-drift warning.
4. **Live status**: processes with state, PID, port, in-flight, uptime,
   restarts, `last_error` incl. stderr tail; start/stop as admin-override
   buttons with a visible chip ("manually stopped"); restart as the
   force_stopped → stopped → clear sequence driven by the UI. SSE via
   `GET /api/portal/servers/{id}/runtime/events` (snapshot on connect, then
   frames — the `benchmark/events` pattern).

All new strings in German and English together (`i18n.ts`; build-enforced
parity). README screenshots regenerated per the AGENTS.md rule.

## 13. Testing strategy

| Layer | What |
|---|---|
| agent `policy.go` | the bulk: table-driven tests over pure functions — matrix rules, per-GPU arithmetic, process limit, unknown-VRAM-alone, victim selection, admin overrides |
| agent `manager.go` | reconciliation with the re-exec helper pattern (test binary as child): start/health/drain/crash/backoff, generation discipline |
| agent `router.go` | httptest upstreams: model extraction, unbuffered streaming, heartbeats, error envelopes, `/running` shape |
| agent WS/sample | `RuntimeUpdates()` + `runtime_config` payload handling (mirrors `certUpdates` tests); `RuntimeSample`; archtest allowlist for `internal/runtime` in the same change |
| gateway store | conformance suite (memory/sqlite/postgres) for the new tables + migration tests; verify with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` |
| gateway portal/agent API | CRUD validation (canonical pair order, timeout warning), authorization-as-mapping, runtime-config ETag, push-after-write, feature intersection incl. 404, runtime-report ingest (opaque-blob validation; the env-value redaction test lives agent-side, where redaction happens) |
| frontend | vitest per new component: matrix toggle, badges, override chips, file-mode read-only |
| e2e | new suite `e2e:runtime` modeled on `e2e:agent` (which already builds and spawns the real agent with a fake `nvidia-smi`): portal creates a spec pointing at a tiny stub model-server binary → agent starts it → request flows gateway → router port → stub → badge goes green. The full circle. |

## 14. Documentation plan

New cross-cutting doc `docs/architecture/cross-cutting/agent-runtime-manager.md`
plus same-branch updates to `05-building-block-view.md`, `06-runtime-view.md`,
`reference/api-surface.md`, `reference/data-model.md`,
`reference/config-env.md`, and the AGENTS.md version-bump rule. The durable
timeout-inventory facts land in `compatibility-and-inference.md` (or the new
doc) as part of §8's documentation.

## 15. Risks and accepted trade-offs

- Agent is a single point of failure for its server's managed models
  (accepted; classic applications can coexist during migration).
- Portal admins with server-management rights effectively choose what runs on
  AI servers — bounded by the agent-local allowlist (empty = nothing starts).
- Pairwise matrix + arithmetic cannot express every co-residency constraint
  (e.g. exotic interconnect contention); the matrix's intent role covers the
  known ones.
- Volatile runtime status forgets `last_error` on agent restart (accepted;
  honest, documented).
- File mode offers no remote operations by design.
- A forgotten PATCH version bump is not machine-detectable (process rule
  only).

# Runtime-Spec Type + per-model context size & metrics — design

Status: approved in brainstorming (user: "passt"). Date: 2026-09-07.

## 1. Goal

When the **server-agent loads models** (per-mapping runtime specs → the agent's
router → a child model server per spec, loopback-only), give the gateway a
per-model **context size** and per-model **live metrics** (active/queued
requests). The enabler is a new **`RuntimeSpec.Type`** (`vllm` | `llama_cpp` |
`tgi` | `ollama` | `custom`) from which the per-type probe endpoints and the
metric/field extraction are derived, with per-spec overrides.

## 2. Why — the current gap

- **Context size today** is per-*application*: an app has an optional
  `context_probe_path`; the **gateway** GETs it and stores per-mapping
  `context_size` (`UpdateMappingContextProbe`, provenance `probe`). It feeds
  routing (`requestFitsContext`) and the portal. For a `server_agent` app the
  "endpoint" is the **router** (one port, many models) — a single app-level
  probe cannot yield a per-model context.
- **Metrics today** are one agent-wide `OP_AGENT_METRICS_URL` the agent scrapes
  (vLLM/llama.cpp auto-detected → `active_requests`/`queue_depth` in the
  telemetry `Sample`). It is a **single** endpoint with **no model
  distinction**.
- **The blocker:** a `server_agent`'s child model servers run **loopback-only**
  (`127.0.0.1:<ListenPort>` behind the router). The **gateway cannot reach
  them**; only the **agent** can (it already does, for `health_path` checks).

**Therefore:** the agent — which owns the child processes and reaches their
loopback endpoints — probes each child's context size and scrapes each child's
metrics, and reports both **per model** in its telemetry sample. The gateway
stores them per mapping, exactly like today's probe values.

## 3. Design

### 3.1 `RuntimeSpec.Type`

New per-spec field `Type` ∈ { `vllm`, `llama_cpp`, `tgi`, `ollama`, `custom` }.
It describes the **child backend** the spec's `binary` runs. (This is orthogonal
to `Application.Type`, which is always `server_agent` for a spec's owning app.)

- **Effective type / detection.** An empty stored `Type` means "auto": the
  effective type is **detected from `binary`** (basename, case-insensitive
  substring): `vllm` → `vllm`; `llama-server` / `llama_cpp` / `llama.cpp` →
  `llama_cpp`; `text-generation-launcher` / `tgi` → `tgi`; `ollama` → `ollama`;
  **no match → `custom`** (the fallback). The operator can override by storing
  an explicit `Type` in the portal.
- **Migration default.** Migration backfills existing specs' `Type` to `''`
  (auto) — so the effective type is detected from each spec's `binary` on the
  fly, and behaviour only changes once a probe/scrape is actually wired
  (§3.5). No forced re-typing on upgrade.
- **Future leverage.** The user's intent: once a spec is typed, *other* fields
  can derive from it too (e.g. the API-token feature's structural
  unsupported-backend banner, which today has to guess the child backend
  because no typed field exists). Out of scope for this feature, but `Type` is
  the foundation.

llama_swap / litellm are proxies/routers, not a single-model child — they are
**not** distinct spec types (an operator running one as a spec's binary picks
`custom`).

### 3.2 Per-type derivation + overrides

The type derives three things per backend: the **metrics path**, the **context
probe path**, and **how to extract** the context number from the response.

| Type | metrics path | context path | context field | status |
|---|---|---|---|---|
| `vllm` | `/metrics` (`vllm:num_requests_running` / `_waiting`) | `/v1/models` | `data[].max_model_len` | vLLM metrics **verified**; context path/field **to verify** against source at impl |
| `llama_cpp` | `/metrics` (`llamacpp:requests_processing` / `_deferred`) | `/props` | `default_generation_settings.n_ctx` (or `n_ctx`) | metrics **verified** (PR #45); context path/field **to verify** |
| `tgi` | `/metrics` (candidate: `tgi_batch_current_size` / `tgi_queue_size`) | `/info` | candidate: `max_total_tokens` | **candidate, verify** against text-generation-inference source at impl |
| `ollama` | (candidate: none native — Ollama has no Prometheus `/metrics`) | `/api/show` (candidate) | candidate: `model_info["*.context_length"]` | **candidate, verify**; metrics may be unavailable for ollama |
| `custom` | operator-set `metrics_path` (generic scrape: vLLM/llama.cpp names auto-detected, else 0) | operator-set `context_probe_path` | best-effort: first present of `n_ctx` / `max_model_len` / `context_length` in the JSON | — |

- The exact `tgi`/`ollama` endpoints and JSON fields are **verified against the
  upstream source during implementation** (as done for llama.cpp in PR #45),
  not asserted from memory. The metric-name auto-detection already merged
  (`scrape.go`) is extended with each family's names.
- **Overrides.** New optional per-spec fields `metrics_path` and
  `context_probe_path`: empty ⇒ derive from the effective type; a value ⇒ use it
  verbatim (still with the type's extraction rules; for `custom`, best-effort
  extraction). This is "configurable but auto-derivable from the type."
- If a type has no metrics or no context mechanism (e.g. ollama metrics), that
  signal is simply absent (0 / unknown) — never an error.

### 3.3 Agent-side probing

The agent already holds each spec's `Model`, `Binary`, `ListenPort`,
`HealthPath` (wire `runtime.Spec`) and reaches the child on loopback for health
checks. Add to the wire spec: `Type`, `MetricsPath`, `ContextProbePath` (the
gateway sends the **effective** type + resolved paths, or the agent resolves
from the type — see §7 for where derivation runs). Then:

- **Context size — once, on load, cached.** Context is static per loaded model.
  When a child is healthy/first-scraped, the agent GETs
  `http://127.0.0.1:<ListenPort><context_path>`, extracts the number per the
  type's rule, and caches it for that child's lifetime (re-probed on a
  restart/reload). A failed probe leaves it unknown (0), retried on the next
  cycle until known.
- **Metrics — per telemetry cycle.** Each cycle, for every loaded child, the
  agent GETs `http://127.0.0.1:<ListenPort><metrics_path>` and reads the
  active/queued counters using the merged auto-detect (§3.2). Reuses the
  existing `collector` scrape machinery, now **per child** instead of one URL.
- **Reporting — per model.** The telemetry `Sample` gains a per-model list, e.g.
  `models: [{ model, context_size, active_requests, queue_depth }]` (exact
  shape in §4). The existing agent-wide `active_requests`/`queue_depth` from
  `OP_AGENT_METRICS_URL` remain for the single-external-server case (§3.6).

### 3.4 Gateway ingest → routing + portal

On telemetry ingest, the gateway maps each reported model to its mapping (by
`gateway_model_name` / `app_model_name`) and stores:

- **Context size** per mapping (`UpdateMappingContextProbe`-equivalent, or the
  same call with provenance `agent`). Feeds `requestFitsContext` and the portal
  `context_size` display — identical downstream to today's gateway probe, just
  a different source.
- **Metrics** per mapping: `ActiveRequests` / `QueueDepth` per model. Routing's
  load-aware scoring uses the **per-model** value as the more precise source;
  the existing **per-server** aggregation is derived from the per-model values
  (sum across the server's models). (User-approved: per-model replaces the
  coarse per-server aggregation as the source of truth.)

### 3.5 Multi-model & mixed-type (the point)

Because each spec has its own child on its own `ListenPort` and its own `Type`,
the agent scrapes each child independently with the right metric names. So:

- **N models loaded at once** → N children → N ports → N independent per-model
  scrapes. No aggregation ambiguity.
- **Mixed types** (a vLLM model and a llama.cpp model live at the same time) →
  each spec's `Type` tells the agent how to read *that* child. Fully supported.

This is the concrete fix for the single-`OP_AGENT_METRICS_URL` limitation.

### 3.6 Relationship to existing mechanisms

- **`OP_AGENT_METRICS_URL`** stays — it is the "one external inference server,
  agent not loading models" path. For a `server_agent` that loads models, the
  per-spec per-model scrape (§3.3) is the source; the single URL is redundant
  there.
- **App-level `context_probe_path`** stays for **non-`server_agent`** apps
  (gateway-side probe, unchanged). `server_agent` mappings now get their context
  from the per-spec agent probe instead.

## 4. Data model, wire, and sample

- **Migration (additive, append-only, mirrors prior migrations).** New columns
  on `agent_runtime_specs`: `type text not null default ''` (values
  `''|vllm|llama_cpp|tgi|ollama|custom`; `''` = auto-detect), `metrics_path text
  not null default ''`, `context_probe_path text not null default ''`.
  `routing.RuntimeSpec` gains `Type`, `MetricsPath`, `ContextProbePath` strings.
  A `routing.RuntimeSpecType` enum-type file (mirrors `runtime_api_token_mode.go`).
- **Per-mapping metrics + context storage.** Context reuses the existing
  per-mapping `context_size` + provenance. Per-mapping live metrics
  (active/queue) may reuse existing per-server telemetry keyed by mapping, or a
  small per-mapping telemetry addition — decided in the plan against the current
  telemetry store shape.
- **Agent wire `runtime.Spec`** gains `Type`, `MetricsPath`, `ContextProbePath`
  (json `type` / `metrics_path` / `context_probe_path`).
- **Telemetry `Sample`** gains a per-model array (json `models`), each entry:
  `model`, `context_size`, `active_requests`, `queue_depth`. Nil/empty is a
  valid empty array (mirrors `LoadedModels`).

## 5. Capability negotiation + version

New agent capability flag (e.g. `runtime_model_probe`, Since the next MINOR),
`const Version` bump. The gateway only trusts/asks for per-model probe data from
an agent that declares it; an older agent simply omits the new `models` array
(graceful — the gateway falls back to today's behaviour). Follows the append-only
`agent.Features` rules and `TestFeatureRegistry` (Since ≤ Version).

## 6. Portal

On the runtime-spec editor: a **Type** select (`Auto (aus Binary erkennen)` /
`vLLM` / `llama.cpp` / `TGI` / `Ollama` / `Eigen (custom)`); when `custom` (or as
optional overrides for a known type) the `metrics_path` / `context_probe_path`
fields. Read-only display of the **detected** effective type when `Auto`, and of
the resolved probe paths, so the operator sees what will be used. The
per-mapping **context size** and **live active/queue** are surfaced in the
runtime/mapping views (context_size already shown; add the live metrics). i18n
de + en (parity compile-enforced).

## 7. Open implementation decisions (resolved in the plan)

- **Where derivation runs:** gateway resolves the effective type + paths and
  sends concrete values on the wire (agent stays dumb), **or** the gateway sends
  `Type` and the agent derives paths. Recommendation: **gateway-side derivation**
  (single source of truth, the agent already receives resolved config; keeps the
  agent's new surface minimal — receive paths, GET, extract, report). The
  extraction *rule* per type still lives agent-side (it parses the child's JSON).
- **Extraction rules location:** a small per-type table in the agent (context
  JSON field(s) per type) since the agent parses the response. Verified against
  upstream sources at impl.
- **Metrics store shape:** per-mapping vs per-server-keyed-by-mapping — against
  the current `routing` telemetry store.

## 8. Security & edge cases

- All probes are **loopback** (`127.0.0.1`), agent-local — no new external
  surface. Short timeouts (mirror the existing 5s scrape / health checks);
  failures logged + skipped, never fatal to the telemetry loop.
- Context re-probed on child restart/reload (cache keyed by child lifetime).
- A `custom` spec with no paths set: no metrics/context (0/unknown), no error.
- No secrets involved (this is read-only telemetry). Independent of the
  API-token feature, though both touch `runtime.Spec` + the wire.

## 9. Out of scope

- Deriving *other* spec fields from `Type` (banner, GPU hints, etc.) — future;
  `Type` is only the foundation here.
- Per-request/label-split metrics from a single multi-model child (the
  server_agent model is one child per spec = one model; label splitting is
  unnecessary).
- Replacing `OP_AGENT_METRICS_URL` or the app-level `context_probe_path` (both
  retained for their non-server_agent cases).

## 10. Components touched (for the plan)

1. Store/migration: `agent_runtime_specs.type` + `metrics_path` +
   `context_probe_path`; `routing.RuntimeSpec` fields; `RuntimeSpecType` type;
   sqlite/pg CRUD; conformance.
2. Routing/portal: type detection-from-binary helper; per-type derivation
   (paths + which metric names); resolve effective type + paths for the wire
   push; per-mapping context + metrics ingest → routing + DTOs.
3. Agent (`server-agent`): wire `Spec` fields; per-child context probe (once,
   cached) + per-child metrics scrape (per cycle) via the extended `collector`;
   per-model `Sample.models`; capability flag + Version; verify tgi/ollama
   endpoints against source.
4. Gateway ingest: decode `Sample.models` → per-mapping context + metrics.
5. Scoring: per-model active/queue as the source; per-server aggregation derived.
6. Frontend: Type select + path overrides + detected-type/paths display + live
   metrics display; i18n de/en.
7. Docs: agent-runtime-manager, telemetry-usage-observability, data-model,
   api-surface, config-env, ADR.
8. Full verification (Postgres + Sonar + version rule + frontend format:check);
   cleanup + PR.

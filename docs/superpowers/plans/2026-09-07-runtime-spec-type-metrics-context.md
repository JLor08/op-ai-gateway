# RuntimeSpec Type + per-model context size & metrics — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** For server_agent-loaded models, give the gateway per-model **context size** and per-model **live metrics** (active/queued), plus surface the already-reported per-model **load state** in the Models view and use it in routing — all keyed off a new **`RuntimeSpec.Type`** (`vllm`/`llama_cpp`/`tgi`/`ollama`/`custom`) that derives the probe endpoints.

**Architecture:** The agent owns each child model server (loopback-only), so it probes each child's context (once, cached) and scrapes its metrics (per cycle) using the type-derived paths, and reports them on the **existing** per-runtime telemetry channel (`sample.RuntimeSample` → `agentRuntimeSample` → `RuntimeStatusDTO`, already carrying per-model `State`). The gateway resolves the effective type + paths and pushes them in the runtime config; on ingest it maps each sample's `SpecID` → mapping and stores per-mapping context + metrics for routing and the portal. This mirrors the already-merged **API-token feature** (migration 74, `RuntimeAPITokenMode`, the wire field, the `runtime_api_token` capability flag, the portal mode-select) almost one-for-one — read that code and mirror it for every mechanical parallel; write fresh code only for the type-derivation, the agent prober, the ingest mapping, and the scoring signal.

**Tech Stack:** Go 1.x (gateway `gateway/backend`, agent `server-agent`), modernc sqlite + Postgres, React/TS portal (`gateway/frontend`), the `collector` scrape package (extended in PR #45).

## Global Constraints

Every task's requirements implicitly include this section. Values are verbatim and load-bearing.

- **Migration 75** on `agent_runtime_specs`, additive + append-only, mirroring `store/migrate.go` `migration74Up` (`addColumnIfMissing`, aborts boot on failure, replays cleanly). Three new columns:
  - `type text not null default ''` — one of `''|vllm|llama_cpp|tgi|ollama|custom` (`''` = auto-detect from binary).
  - `metrics_path text not null default ''` — override; empty = derive from type.
  - `context_probe_path text not null default ''` — override; empty = derive from type.
- `routing.RuntimeSpec` (in `routing/store.go`) gains `Type string`, `MetricsPath string`, `ContextProbePath string`.
- New file `routing/runtime_spec_type.go` (mirror `routing/runtime_api_token_mode.go`): type `RuntimeSpecType string` + consts `RuntimeSpecTypeVLLM="vllm"`, `RuntimeSpecTypeLlamaCpp="llama_cpp"`, `RuntimeSpecTypeTGI="tgi"`, `RuntimeSpecTypeOllama="ollama"`, `RuntimeSpecTypeCustom="custom"`.
- **Detection from binary** (`''` type): basename(binary) lowercased, first substring match → `vllm` | `llama_cpp` (matches `llama-server`/`llama_cpp`/`llama.cpp`) | `tgi` (matches `text-generation-launcher`/`tgi`) | `ollama`; **no match → `custom`**.
- **Per-type derivation** (metrics path + which metric names; context path + extraction field):
  - `vllm`: metrics `/metrics` (`vllm:num_requests_running`/`_waiting` — already in `scrape.go`); context `/v1/models` → `data[].max_model_len` (VERIFY path/field at Task 8).
  - `llama_cpp`: metrics `/metrics` (`llamacpp:requests_processing`/`_deferred` — already merged, PR #45); context `/props` → `default_generation_settings.n_ctx` (or `n_ctx`) (VERIFY at Task 8).
  - `tgi`: candidate metrics `/metrics` (`tgi_batch_current_size`/`tgi_queue_size`); context `/info` → `max_total_tokens`. **VERIFY against text-generation-inference source at Task 8** (WebFetch), do NOT hardcode from memory.
  - `ollama`: candidate context `/api/show` → `model_info` context_length; **metrics may be unavailable (no native `/metrics`)**. VERIFY at Task 8.
  - `custom`: operator-set `metrics_path`/`context_probe_path`; metrics via the generic scrape (auto-detects known families, else 0); context best-effort — first present of `n_ctx`/`max_model_len`/`context_length` in the JSON.
- Override precedence: a non-empty `metrics_path`/`context_probe_path` on the spec wins; empty ⇒ derive from the effective type.
- **Wire**: `server-agent` `runtime.Spec` and the gateway push DTO `AgentRuntimeSpecDTO` gain `Type`, `MetricsPath`, `ContextProbePath` (json `type`/`metrics_path`/`context_probe_path`), carrying the **resolved effective** type + paths (gateway-side derivation — the agent receives concrete paths + the type for extraction).
- **Extend the EXISTING per-runtime channel** (no new array): `sample.RuntimeSample` (agent), `agentRuntimeSample` (gateway ingest), and `RuntimeStatusDTO` gain `ContextSize int`, `ActiveRequests int`, `QueueDepth int` (json `context_size`/`active_requests`/`queue_depth`; additive, omitempty-safe like the neighbours). `State` already exists — do NOT add a new state field.
- **Agent probing**: per child (keyed by `ListenPort`), context probed ONCE and cached for the child's lifetime (re-probe on restart), metrics scraped per telemetry cycle, via the extended `collector`. Loopback only, short timeout (mirror the existing 5s), failures logged + skipped (never fatal to telemetry).
- **Capability**: new flag `runtime_model_probe` in `agent/features.go` (Since `0.6.0`); bump `agent/agent.go` `const Version` `0.5.0` → `0.6.0` (MINOR; `TestFeatureRegistry` enforces Since ≤ Version). The probe fields are additive/omitempty-safe (an agent with `runtime_manager` but no probing sends 0 → graceful).
- **Ingest**: each `agentRuntimeSample.SpecID` resolves to its mapping; store per-mapping context via `UpdateMappingContextProbe` (provenance `agent`) and per-mapping metrics in the routing telemetry store.
- **Routing/scoring**: per-model active/queue is the source; per-server aggregation is derived (sum over the server's models). The existing `StateStarting` is a positive "soon-available" hint (prefer an already-starting instance over a second cold-start). Conservative — complements the cold-load 503 retry path.
- **Portal**: RuntimeSpec editor Type select (default `Auto`) + `metrics_path`/`context_probe_path` overrides + read-only detected-type/resolved-paths display. **Models → Details**: per-model state (loading indicator) + context size + live metrics. **Application editor**: for a `server_agent`-type app, **disable and clear** `context_probe_path`, `capacity_probe_path`, `loaded_models_path`, `loaded_models_format`. i18n de + en (parity compile-enforced). Frontend uses snake_case JSON keys directly (no camelCase mapper), mirroring the api-token frontend.
- **Health-check** app fields are NOT disabled for server_agent — verify (server_agent liveness = agent-presence) but do not change liveness behavior in this feature.
- **Branching/PR (AGENTS.md)**: work only in this worktree; never commit to `main`. `docs/superpowers/**` deleted before the PR. Frontend CI runs `npm run format:check` — run it before finishing.

---

## File Structure

**Gateway backend (`gateway/backend/internal`):**
- `routing/runtime_spec_type.go` *(new)* — the enum + `DetectRuntimeSpecType(binary)` + the per-type derivation helper `DeriveProbePaths(effectiveType, metricsOverride, contextOverride) (metricsPath, contextPath string)`.
- `routing/store.go` *(modify)* — `RuntimeSpec` gains 3 fields.
- `store/migrate.go` + sqlite CRUD *(modify)* — migration 75 + the 3 columns (every `visible_devices_mode`/`api_token_mode` CRUD site).
- `portal/service_runtime.go` *(modify)* — DTO + put-request fields + validation; wire push (`AgentRuntimeSpecDTO`) resolve effective type + paths.
- `gateway/agent_ingest.go` *(modify)* — `agentRuntimeSample` + `RuntimeStatusDTO` new fields; ingest → per-mapping context + metrics.
- `routing/scorer.go` + `resolver.go` *(modify)* — per-model metrics source + `StateStarting` hint.
- `portal/service_applications.go` *(modify, minimal)* — no hard change; the disable is portal-side (Task 14).

**Agent (`server-agent/internal`):**
- `runtime/types.go` *(modify)* — wire `Spec` gains `Type`/`MetricsPath`/`ContextProbePath`.
- `collector/scrape.go` + a new `collector/probe.go` *(modify/new)* — per-child metrics (extend names) + context probe with per-type extraction.
- `runtime/manager.go` + `sample/sample.go` *(modify)* — fill `RuntimeSample.ContextSize`/`ActiveRequests`/`QueueDepth`.
- `agent/features.go` + `agent/agent.go` *(modify)* — flag + Version.

**Frontend (`gateway/frontend/src`):** `api/runtime.ts` + `api/models.ts` (types), `components/RuntimeAdminSection.tsx` (Type select + Models-details), `components/ApplicationSection.tsx` (server_agent disable), `i18n.ts`.

**Docs (`docs/architecture`):** agent-runtime-manager, telemetry-usage-observability, data-model, api-surface, config-env, ADR.

---

## Task 1: Store — migration 75, `RuntimeSpecType`, RuntimeSpec fields, CRUD

**Files:** Create `routing/runtime_spec_type.go`; modify `routing/store.go` (RuntimeSpec), `store/migrate.go` (migration 75 after `migration74Up`), the sqlite CRUD sites (find with `grep -rn "api_token_mode" gateway/backend/internal/store`); Test: the store conformance round-trip that covers `api_token_mode`.

**Interfaces produced:** `routing.RuntimeSpecType` + the 5 consts; `routing.RuntimeSpec.{Type,MetricsPath,ContextProbePath}` (string); migration `75` named `runtime_spec_type_probe`.

- [ ] **Step 1: Failing round-trip test** — extend the conformance test: persist a spec with `Type:"vllm"`, `MetricsPath:"/m"`, `ContextProbePath:"/c"`, read it back, assert equality; plus a raw-insert row asserting the three columns default to `''`.
- [ ] **Step 2: Run to fail** — `cd gateway/backend && go test ./internal/store/ -run <ConformanceTest> -v`. Expect FAIL (unknown fields / missing columns).
- [ ] **Step 3: Enum file** — create `routing/runtime_spec_type.go` mirroring `runtime_api_token_mode.go`'s header/comment style, with `type RuntimeSpecType string` and consts `RuntimeSpecTypeVLLM="vllm"`, `RuntimeSpecTypeLlamaCpp="llama_cpp"`, `RuntimeSpecTypeTGI="tgi"`, `RuntimeSpecTypeOllama="ollama"`, `RuntimeSpecTypeCustom="custom"`. Doc: `''` = auto-detect from binary.
- [ ] **Step 4: RuntimeSpec fields** — in `routing/store.go` next to `APITokenMode` add `Type string`, `MetricsPath string`, `ContextProbePath string` with doc comments.
- [ ] **Step 5: Migration 75** — add `{version: 75, name: "runtime_spec_type_probe", up: migration75Up}` after the 74 entry, and `migration75Up` mirroring `migration74Up`: three `addColumnIfMissing(ctx,tx,dl,"agent_runtime_specs","type text not null default ''")` etc.
- [ ] **Step 6: CRUD** — every INSERT/UPDATE/SELECT/scan naming `api_token_mode` also names `type`, `metrics_path`, `context_probe_path`, same order, bound to the new fields.
- [ ] **Step 7: Run to pass** — `cd gateway/backend && go test ./internal/store/ ./internal/routing/`. Expect PASS.
- [ ] **Step 8: Commit** — `feat(store): migration 75 + RuntimeSpec type/metrics_path/context_probe_path`.

---

## Task 2: Routing — detection + per-type derivation helpers

**Files:** `routing/runtime_spec_type.go` (add helpers); Test: `routing/runtime_spec_type_test.go`.

**Interfaces produced:**
- `routing.DetectRuntimeSpecType(binary string) RuntimeSpecType` — basename-substring detection; no match → `RuntimeSpecTypeCustom`.
- `routing.EffectiveRuntimeSpecType(spec RuntimeSpec) RuntimeSpecType` — `spec.Type` if non-empty, else `DetectRuntimeSpecType(spec.Binary)`.
- `routing.DeriveProbePaths(t RuntimeSpecType, metricsOverride, contextOverride string) (metricsPath, contextPath string)` — override wins; else the per-type default paths (Global Constraints table).

- [ ] **Step 1: Failing test** — table cases: `DetectRuntimeSpecType("/usr/bin/vllm")==vllm`; `.../llama-server==llama_cpp`; `.../text-generation-launcher==tgi`; `.../ollama==ollama`; `.../my-thing==custom`. `EffectiveRuntimeSpecType` with explicit type overrides detection. `DeriveProbePaths(vllm,"","")==("/metrics","/v1/models")`; `(llama_cpp,"","")==("/metrics","/props")`; override `DeriveProbePaths(vllm,"/x","/y")==("/x","/y")`; `(custom,"","")==("","")`.
- [ ] **Step 2: Run to fail.** `go test ./internal/routing/ -run TestRuntimeSpecType -v`. FAIL.
- [ ] **Step 3: Implement** the three helpers. Detection: `strings.ToLower(filepath.Base(binary))`, ordered `strings.Contains` checks. Derivation: a `switch t` returning the default `(metrics,context)` per the table, with override params taking precedence when non-empty. (tgi/ollama default paths are the candidates from the constraints — Task 8 verifies/adjusts the exact strings; leave a `// TODO(Task 8): verify tgi/ollama paths against upstream` only if the value is still unconfirmed at that point, else set the verified value.)
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(routing): RuntimeSpec type detection + per-type probe-path derivation`.

---

## Task 3: Portal — RuntimeSpec DTO + validation for type/paths

**Files:** `portal/service_runtime.go` (`RuntimeSpecDTO`, `PutRuntimeSpecRequest`, validation ~ the api-token validator); Test: the runtime-spec DTO/validation test.

**Interfaces produced:** DTO read fields `type`, `metrics_path`, `context_probe_path`, plus read-only `effective_type` (the resolved type when `''`) + `resolved_metrics_path` / `resolved_context_probe_path` echoes (so the portal shows what will be used). PutRequest fields `type`, `metrics_path`, `context_probe_path`. Sentinel `ErrRuntimeSpecTypeInvalid` (`runtime_spec.type_invalid`).

- [ ] **Step 1: Failing test** — invalid type (`"bogus"`) → `ErrRuntimeSpecTypeInvalid`; valid types + `''` accepted; DTO echoes `effective_type` = detected when `type==''` (e.g. binary `llama-server` → `llama_cpp`) and the resolved paths.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — add the DTO/PutRequest fields (mirror the api-token DTO block); a `validRuntimeSpecType` helper (empty OR one of the five); wire `effective_type`/resolved-path echoes via `routing.EffectiveRuntimeSpecType` + `DeriveProbePaths` in the DTO assembly. Normalize empty type as `''` (auto). No path-shape validation beyond trimming (paths are freeform).
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(portal): runtime-spec type + probe-path DTO/validation + resolved echoes`.

---

## Task 4: Portal — push resolved type + paths to the agent

**Files:** `portal/service_runtime.go` (`AgentRuntimeSpecDTO` + `AgentRuntimeConfig` builder, the same site the api-token `resolvePushToken` set `specDTO.APIToken`); Test: the push-builder test.

**Interfaces produced:** `AgentRuntimeSpecDTO` gains `Type string \`json:"type"\``, `MetricsPath string \`json:"metrics_path"\``, `ContextProbePath string \`json:"context_probe_path"\``, carrying the **resolved effective** type + paths.

- [ ] **Step 1: Failing test** — a spec `Type:""`, binary `llama-server` → pushed `Type=="llama_cpp"`, `MetricsPath=="/metrics"`, `ContextProbePath=="/props"`. A spec with overrides → pushed overrides. A `custom` spec with no paths → pushed `Type=="custom"`, empty paths.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — add the 3 fields to `AgentRuntimeSpecDTO`; in the `AgentRuntimeConfig` spec loop (where `specDTO.APIToken` is set), set `et := routing.EffectiveRuntimeSpecType(spec); mp, cp := routing.DeriveProbePaths(et, spec.MetricsPath, spec.ContextProbePath); specDTO.Type = string(et); specDTO.MetricsPath = mp; specDTO.ContextProbePath = cp`.
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(portal): push resolved runtime-spec type + probe paths to the agent`.

---

## Task 5: Gateway endpoint — map the type sentinel → 400

**Files:** `gateway/portal_runtime_endpoints.go` (the `portalRuntimeSpecErrRows` table); Test: the endpoint test.

- [ ] **Step 1: Failing test** — PUT a spec with `type:"bogus"` → HTTP 400, code `runtime_spec.type_invalid`.
- [ ] **Step 2: Run to fail.** FAIL (500).
- [ ] **Step 3: Implement** — add `{err: portal.ErrRuntimeSpecTypeInvalid, status: 400, code: "runtime_spec.type_invalid", msg: "..."}` to `portalRuntimeSpecErrRows`.
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(gateway): map runtime_spec.type_invalid to 400`.

---

## Task 6: Agent — wire `Spec` fields

**Files:** `server-agent/internal/runtime/types.go` (`Spec`, near `APIToken`); Test: a policy/expand test that a wire spec round-trips the new fields (or a manager test that reads them).

**Interfaces produced:** `runtime.Spec.{Type, MetricsPath, ContextProbePath}` (json `type`/`metrics_path`/`context_probe_path`).

- [ ] **Step 1: Failing test** — unmarshal a config JSON with `"type":"llama_cpp","metrics_path":"/metrics","context_probe_path":"/props"` into `runtime.Spec`; assert the fields.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — add the 3 string fields with json tags to `runtime.Spec`.
- [ ] **Step 4: Run to pass.** `cd server-agent && go test ./internal/runtime/`. PASS.
- [ ] **Step 5: Commit** — `feat(agent): wire runtime.Spec type + probe paths`.

---

## Task 7: Agent — sample fields (`RuntimeSample` context/metrics)

**Files:** `server-agent/internal/sample/sample.go` (`RuntimeSample`); Test: sample round-trip test.

**Interfaces produced:** `sample.RuntimeSample.{ContextSize, ActiveRequests, QueueDepth}` (json `context_size`/`active_requests`/`queue_depth`).

- [ ] **Step 1: Failing test** — marshal a `RuntimeSample` with the 3 fields set, unmarshal, assert; assert an entry without them marshals them as 0 (they are NOT omitempty — they are always-present counters, mirroring `InFlight`/`Restarts`).
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — add the 3 int fields (json, not omitempty, like `InFlight`/`Restarts`).
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(agent): RuntimeSample context_size + active/queue fields`.

---

## Task 8: Agent — per-child prober (context once + metrics per cycle); VERIFY tgi/ollama

**Files:** Create `server-agent/internal/collector/probe.go`; modify `collector/scrape.go` (extend metric-name detection for tgi/ollama); Test: `collector/probe_test.go`.

**Interfaces produced:** `collector.ProbeContext(ctx, baseURL, specType, contextPath string) (int, error)` — GET `baseURL+contextPath`, extract the context number per `specType`'s JSON rule (vllm `data[].max_model_len`; llama_cpp `n_ctx`; tgi `max_total_tokens`; ollama `context_length`; custom/unknown → best-effort first-present of `n_ctx`/`max_model_len`/`context_length`). The metrics scrape stays `collector.NewScraper` (extended names).

- [ ] **Step 0: VERIFY tgi/ollama endpoints** — before hardcoding, WebFetch the upstream sources for the exact names/paths (as done for llama.cpp): text-generation-inference `/metrics` (queue/batch gauges) + `/info` (`max_total_tokens`); Ollama `/api/show` (`model_info` context_length key) and whether it exposes `/metrics`. Record the verified values in the code comments. Adjust Task 2's `DeriveProbePaths` tgi/ollama defaults + `scrape.go`'s metric-name set to the verified strings.
- [ ] **Step 1: Failing tests** — `ProbeContext` against an httptest server returning: a vLLM `/v1/models` body → `max_model_len`; a llama.cpp `/props` body → `n_ctx`; a tgi `/info` body → the verified field; a `custom` body with `context_length` → that value; a body with none → error/0. Extend the scrape test: a tgi `/metrics` body → active/queue from the verified tgi names.
- [ ] **Step 2: Run to fail.** `cd server-agent && go test ./internal/collector/ -v`. FAIL.
- [ ] **Step 3: Implement** — `probe.go` with `ProbeContext` (json.Unmarshal into a small typed/`map[string]any` shape per type; tolerant — missing key → 0/err). Extend `scrape.go`'s auto-detect with the verified tgi/ollama running/waiting names (mirror the existing `firstPresent` list).
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(agent): per-child context probe + tgi/ollama metric names (verified against upstream)`.

---

## Task 9: Agent — wire the prober into the runtime manager's sample

**Files:** `server-agent/internal/runtime/manager.go` (where `RuntimeSample` is built per cycle); Test: a manager test with a fake child metrics/context server.

**Interfaces consumed:** `collector.ProbeContext` + `collector.NewScraper` (Task 8); each spec's resolved `Type`/`MetricsPath`/`ContextProbePath` (Task 6) + `ListenPort`.

- [ ] **Step 1: Failing test** — a running spec whose child (httptest on the spec's port) serves `/metrics` + context → the built `RuntimeSample` has `ContextSize`/`ActiveRequests`/`QueueDepth` filled; context is probed ONCE across two cycles (cache); a failing probe leaves 0 and does not error the cycle.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — when building each running spec's `RuntimeSample`: base URL `http://127.0.0.1:<ListenPort>`; scrape metrics each cycle (`NewScraper(base+MetricsPath).Scrape`), set active/queue; probe context once (cache keyed by specID+PID or child start time; re-probe when PID/port changes), set `ContextSize`. Short timeout; errors → keep 0, log at debug. Only for `StateRunning` children (a `StateStarting` child has no healthy endpoint yet → 0/loading).
- [ ] **Step 4: Run to pass.** PASS (whole `internal/runtime` + `internal/collector` green).
- [ ] **Step 5: Commit** — `feat(agent): fill RuntimeSample context/metrics from per-child probes`.

---

## Task 10: Agent — capability flag + Version

**Files:** `agent/features.go`, `agent/agent.go`; Test: `TestFeatureRegistry`.

- [ ] **Step 1: Test** — `Features` contains `{Name:"runtime_model_probe", Since:"0.6.0"}`; `Version=="0.6.0"`; registry invariant holds.
- [ ] **Step 2: Run to fail.** FAIL at 0.5.0.
- [ ] **Step 3: Implement** — append `{Name:"runtime_model_probe", Since:"0.6.0"}` (doc comment: agent probes each managed child for context size + live metrics and reports them in RuntimeSample); bump `const Version = "0.5.0"` → `"0.6.0"`.
- [ ] **Step 4: Run to pass.** `cd server-agent && go test ./internal/agent/`. PASS.
- [ ] **Step 5: Commit** — `feat(agent): capability flag runtime_model_probe; Version 0.6.0`.

---

## Task 11: Gateway ingest — decode the new fields + surface in the status DTO

**Files:** `gateway/agent_ingest.go` (`agentRuntimeSample`, `RuntimeStatusDTO`, `runtimeStatusDTOsFromSamples`); the `RuntimeStatusDTO` may be in `portal`/`gateway` — find it; Test: the ingest/status test.

**Interfaces produced:** `agentRuntimeSample.{ContextSize,ActiveRequests,QueueDepth}` (json) → `RuntimeStatusDTO.{ContextSize,ActiveRequests,QueueDepth}`.

- [ ] **Step 1: Failing test** — ingest a sample with a runtime carrying the 3 fields → the resulting `RuntimeStatusDTO` carries them.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — add the 3 json fields to `agentRuntimeSample`; map them in `runtimeStatusDTOsFromSamples` to the (new) `RuntimeStatusDTO` fields (mirror how `InFlight`/`Restarts` flow).
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(gateway): ingest per-runtime context/metrics into RuntimeStatusDTO`.

---

## Task 12: Gateway ingest — store per-mapping context + metrics

**Files:** `gateway/agent_ingest.go` (the ingest path); `routing` store (per-mapping metrics — reuse the telemetry store or add a per-mapping write); Test: ingest→store test.

**Interfaces consumed:** each `agentRuntimeSample.SpecID` → the spec → its `MappingID`; `store.UpdateMappingContextProbe(ctx, mappingID, contextSize, at)`; the telemetry store for per-mapping active/queue.

- [ ] **Step 1: Failing test** — ingest a runtime sample with `SpecID` (→ known mapping), `ContextSize:8192`, `ActiveRequests:2`, `QueueDepth:1` → the mapping's `ContextSize` is 8192 (provenance `agent`) and per-mapping active/queue is stored; the per-server telemetry equals the sum across the server's runtimes.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — in ingest, for each runtime sample resolve `SpecID`→spec→mapping (the spec is the `AgentRuntimeSpecDTO.ID` the gateway pushed; load via the store); call `UpdateMappingContextProbe` when `ContextSize>0`; write per-mapping active/queue; derive the per-server aggregate (sum) so the existing per-server telemetry stays populated. Guard on the `runtime_model_probe` feature (only trust these when declared; else leave today's behaviour).
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(gateway): store per-mapping context + metrics from runtime samples`.

---

## Task 13: Routing — per-model metrics source + `StateStarting` soon-available hint

**Files:** `routing/scorer.go` + `routing/resolver.go`; Test: scorer/resolver tests.

- [ ] **Step 1: Failing test** — (a) a mapping with per-model active/queue scores using the per-model values (not only per-server); (b) two candidates for the same model, one `StateStarting` (loading) and one not-yet-started: the resolver prefers the already-starting one over triggering a fresh cold-start (assert the chosen candidate / that no second start is requested). Keep it conservative — a `StateRunning` candidate still beats a `StateStarting` one.
- [ ] **Step 2: Run to fail.** FAIL.
- [ ] **Step 3: Implement** — feed the per-mapping active/queue into the scorer's `Route.{ActiveRequests,QueueDepth}` where a per-model value exists (fall back to per-server otherwise); add a small preference for a `StateStarting` instance of the requested model over cold-starting another (a soft bonus / tie-break, NOT overriding a ready instance). Reuse the runtime-status registry the gateway already holds for state.
- [ ] **Step 4: Run to pass.** PASS.
- [ ] **Step 5: Commit** — `feat(routing): per-model load metrics + prefer an already-loading instance`.

---

## Task 14: Frontend — RuntimeSpec Type UI + Models-details + app-editor disable

**Files:** `gateway/frontend/src/api/runtime.ts` + `api/models.ts` (types); `components/RuntimeAdminSection.tsx` (Type select + overrides + detected display + Models-details state/context/metrics); `components/ApplicationSection.tsx` (server_agent disable+clear); `i18n.ts`. Test: the component tests.

- [ ] **Step 1: Failing component tests** — (a) the spec editor shows a Type select (default `Auto`), the override fields, and the detected-effective-type/resolved-paths read-only display; (b) Models → Details shows a `loading` indicator for a `StateStarting`/`loading` runtime and the context size + live active/queue; (c) the application editor, for `type==='server_agent'`, renders `context_probe_path`/`capacity_probe_path`/`loaded_models_path`/`loaded_models_format` as disabled and clears them on save.
- [ ] **Step 2: Run to fail.** `cd gateway/frontend && npm test -- <tests>`. FAIL.
- [ ] **Step 3: Implement** — add the snake_case type fields to `RuntimeSpec`/`PutRuntimeSpecRequest` (mirror `api_token_mode`); the `RuntimeStatus`/model DTO gains `context_size`/`active_requests`/`queue_depth` (+ reuse the existing `state`). Build the Type `<SelectField>` + overrides + detected display (mirror the api-token mode select). In the Models → Details view, show state (loading indicator on `StateStarting`) + context + metrics. In `ApplicationSection`, when `type==='server_agent'` disable the four probe fields and set them to `''` in the submit payload. All strings in de + en.
- [ ] **Step 4: Run gates.** `npm run format:check && npm run lint && npm run build && npm test`. All green (fix formatting with `npm run format` if needed).
- [ ] **Step 5: Commit** — `feat(frontend): runtime-spec Type UI + Models-details state/metrics + server_agent probe-field disable`.

---

## Task 15: Docs

**Files:** `docs/architecture/cross-cutting/agent-runtime-manager.md`, `.../telemetry-usage-observability.md`, `reference/data-model.md`, `reference/api-surface.md`, `reference/config-env.md`, a new ADR.

- [ ] **Step 1** — agent-runtime-manager: the RuntimeSpec `type` + per-type probe derivation; the per-child context/metrics probing on the existing RuntimeSample channel; the `runtime_model_probe` flag + Version 0.6.0.
- [ ] **Step 2** — telemetry-usage-observability: RuntimeSample now carries per-model context/active/queue; ingest → per-mapping context (provenance `agent`) + metrics → routing/portal; `OP_AGENT_METRICS_URL` remains the single-external case.
- [ ] **Step 3** — data-model: migration 75 columns + defaults + the `''`=auto rationale.
- [ ] **Step 4** — api-surface: RuntimeSpec DTO `type`/`metrics_path`/`context_probe_path` + `effective_type`/resolved echoes + `runtime_spec.type_invalid`; RuntimeStatus new fields.
- [ ] **Step 5** — config-env / applications doc: for server_agent the app probe fields are disabled.
- [ ] **Step 6** — ADR: reuse the existing per-runtime channel; RuntimeSpec.Type as the derivation foundation; gateway-side derivation; loading = existing StateStarting.
- [ ] **Step 7** — `bash scripts/check-docs.sh` passes. **Commit** — `docs: runtime-spec type + per-model context/metrics`.

---

## Task 16: Full verification, cleanup, PR

- [ ] **Step 1** — backend `cd gateway/backend && go test ./...` (sqlite).
- [ ] **Step 2** — Postgres leg (per AGENTS.md / `OP_AI_GATEWAY_TEST_POSTGRES_DSN`) — exercises migration 75.
- [ ] **Step 3** — `cd server-agent && go test ./...` (incl. `TestFeatureRegistry` at 0.6.0).
- [ ] **Step 4** — frontend `npm run format:check && npm run lint && npm run build && npm test`.
- [ ] **Step 5** — Sonar gate (`make sonar-gate` + `make sonar-findings`/`sonar-branch-findings`, per the local-Sonar memory) — judge new-code/branch-attributed findings only; fix any attributed to this branch.
- [ ] **Step 6** — leak/consistency grep + `check-docs.sh`.
- [ ] **Step 7** — `git rm -r docs/superpowers && commit`.
- [ ] **Step 8** — push a real branch (`git branch --show-current` non-empty) + open the PR against `main` with a summary (Type + derivation, per-model context/metrics on the existing channel, loading-state reuse + routing hint, server_agent probe-field disable, capability 0.6.0).

---

## Self-Review

**Spec coverage** (§ → task): §3.1 Type/detection → T1,T2; §3.2 derivation/overrides → T2 (+T8 verify tgi/ollama); §3.3 agent probing → T8,T9; §3.4 ingest→routing/portal → T11,T12,T13; §3.5 multi/mixed → inherent (per-child T9); §3.6 disable app probes + OP_AGENT_METRICS_URL retained → T14 (+ note in T15); §3.7 state reuse + Models-details + routing → T13,T14; §4 migration/wire/RuntimeSample → T1,T4,T6,T7,T11; §5 capability → T10; §6 portal → T3,T14; §7 gateway-side derivation → T4; §8 security (loopback, timeouts) → T8,T9; §10 components → all tasks; docs → T15; verification → T16.

**Placeholder scan:** No "TBD"/"handle edge cases". The tgi/ollama exact strings are a bounded, explicit **verification step** (T8 Step 0, WebFetch) feeding T2 — not a vague placeholder. Mechanical tasks name the exact mirror target (api-token migration 74 / `runtime_api_token_mode.go` / the mode-select) with the exact values.

**Type consistency:** `RuntimeSpecType` + consts (T1) used by `DetectRuntimeSpecType`/`EffectiveRuntimeSpecType`/`DeriveProbePaths` (T2), consumed by the push (T4) and the agent prober (T8/T9). `RuntimeSpec.{Type,MetricsPath,ContextProbePath}` (T1) ↔ `AgentRuntimeSpecDTO`/`runtime.Spec` json `type`/`metrics_path`/`context_probe_path` (T4/T6). `RuntimeSample.{ContextSize,ActiveRequests,QueueDepth}` (T7) ↔ `agentRuntimeSample`/`RuntimeStatusDTO` (T11) ↔ per-mapping store (T12) ↔ scorer (T13). `runtime_model_probe`/0.6.0 (T10) gates ingest trust (T12). Frontend snake_case fields (T14) match the DTO json tags.

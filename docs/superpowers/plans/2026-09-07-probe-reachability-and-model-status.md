# Probe Reachability + Model Runtime-Status Presentation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface, per managed child, whether its `/metrics` and context probe are reachable (so a forgotten `--metrics` is visible); stop showing a misleading `0` when a value isn't actually measured; consolidate the model-detail "loaded"/"live-status" columns into one tri-state "Status"; and add a yellow "Lädt" (loading) count to the model overview.

**Architecture:** The agent already probes each `StateRunning` child per cycle. It now also derives a three-state reachability per probe and reports it on the **existing** `RuntimeSample` telemetry channel; the gateway threads it to `RuntimeStatusDTO` + `ModelServerDTO`; the frontend derives the admin "Probes" indicators, the "—"-instead-of-0 gate, the merged "Status" column, and (via a new backend `loading_on_count`) the overview "Lädt" column. Design: `docs/superpowers/specs/2026-09-07-probe-reachability-and-model-status-design.md`.

**Tech Stack:** Go (server-agent + gateway backend, dual-driver store), React/TS portal (Vitest + @testing-library).

## Global Constraints

- Probe-state string values are EXACTLY `ok` | `unreachable` | `na`; `""` = not reported (legacy agent, non-running child). New wire fields are plain `string` (NOT omitempty), mirroring the existing `RuntimeSample` result fields.
- Reuse the existing per-runtime `RuntimeSample` channel; do NOT add a new array. Everything is best-effort and MUST NOT ever reject/fail a telemetry sample.
- All per-model-probe consumption on the gateway is gated on the `runtime_model_probe` capability, exactly like the existing active/queue injection (`injectRuntimeModelState`).
- snake_case JSON everywhere, no camelCase mapper: `metrics_probe`, `context_probe`, `loading_on_count`.
- Frontend: reuse `components/shared/runtimeState.ts` (`runtimeStateBadge`/`runtimeStateLabel`); assert `data-status` on StatusChips (per `StatusChip.tsx`); add every i18n key to BOTH `de` and `en` (tsc enforces identical key sets); run `npm run format:check` before finishing.
- Part 2 (richer telemetry: live t/s, cache stats, modality auto-detect) is OUT OF SCOPE — tracked in issue #49. Do not implement it here.

---

### Task 1: Agent — `RuntimeSample` probe-state fields

**Files:**
- Modify: `server-agent/internal/sample/sample.go` (the `RuntimeSample` struct, ~lines 123-134, where `ContextSize`/`ActiveRequests`/`QueueDepth` live)
- Test: `server-agent/internal/sample/sample_test.go`

**Interfaces — Produces:** `sample.RuntimeSample.{MetricsProbe, ContextProbe}` (`string`, json `metrics_probe` / `context_probe`).

- [ ] **Step 1: Failing test** — extend the RuntimeSample round-trip test: marshal a sample with `MetricsProbe: "ok"`, `ContextProbe: "unreachable"`, unmarshal, assert both; assert a zero-value sample marshals them as present empty strings (they are NOT omitempty, mirroring `MetricsProbe`-siblings).
- [ ] **Step 2: Run to fail** — `cd server-agent && go test ./internal/sample/ -run RuntimeSample -v`. FAIL (unknown fields).
- [ ] **Step 3: Implement** — add after `QueueDepth`:
  ```go
  MetricsProbe string `json:"metrics_probe"`
  ContextProbe string `json:"context_probe"`
  ```
  gofmt-align the tags.
- [ ] **Step 4: Run to pass** — `go test ./internal/sample/`. PASS.
- [ ] **Step 5: Commit** — `feat(agent): RuntimeSample metrics_probe + context_probe fields`.

---

### Task 2: Agent — compute the three-state in `probeRuntimeChild`

**Files:**
- Modify: `server-agent/internal/agent/agent.go` (`probeRuntimeChild`, ~lines 1044-1103)
- Test: `server-agent/internal/agent/agent_test.go`

**Interfaces — Consumes:** Task 1's fields. **Produces:** each cycle's `RuntimeSample` has `metrics_probe`/`context_probe` set for a `StateRunning` child.

Rules (set on `rs`):
- **metrics_probe**: `MetricsPath == ""` → `"na"`; `MetricsPath != ""` but `!SafeProbePath` OR the scrape errored → `"unreachable"`; scrape succeeded → `"ok"`.
- **context_probe**: `ContextProbePath == ""` → `"na"`; `!SafeProbePath` OR probe error OR non-positive size → `"unreachable"`; cache hit OR probe size>0 → `"ok"`.
- Set ONLY within `probeRuntimeChild` (already only called for running children). Never change the best-effort/non-fatal behaviour.

- [ ] **Step 1: Failing tests** — mirror the existing probe tests (a running child whose httptest server serves `/metrics` + a context body → `rs.MetricsProbe=="ok"`, `rs.ContextProbe=="ok"`; a child with `MetricsPath` set but the endpoint refusing → `"unreachable"`, context still `"ok"`; a child with empty `MetricsPath`/`ContextProbePath` → `"na"` for the empty one; a context probe returning size 0 → `context_probe=="unreachable"`). Assert the exact strings.
- [ ] **Step 2: Run to fail** — `cd server-agent && go test ./internal/agent/ -run RuntimeProbe -v`. FAIL.
- [ ] **Step 3: Implement** — thread the state through the existing branches of `probeRuntimeChild` (set `rs.MetricsProbe` in the metrics block incl. the empty/unsafe cases; set `rs.ContextProbe` in the context block incl. empty/unsafe/err/size≤0/cache-hit/ok). Keep it a local computation; no new deps.
- [ ] **Step 4: Run to pass** — `go test ./internal/agent/ ./internal/collector/`. PASS.
- [ ] **Step 5: Commit** — `feat(agent): derive metrics/context probe reachability (ok/unreachable/na)`.

---

### Task 3: Gateway wire — `agentRuntimeSample` + `RuntimeStatusDTO`

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`agentRuntimeSample` struct ~line 159-170; `runtimeStatusDTOsFromSamples` ~line 223-235)
- Modify: `gateway/backend/internal/gateway/runtime_registry.go` (`RuntimeStatusDTO` ~line 45-56)
- Test: `gateway/backend/internal/gateway/agent_runtime_status_ingest_test.go`

**Interfaces — Produces:** `RuntimeStatusDTO.{MetricsProbe, ContextProbe}` (json `metrics_probe`/`context_probe`).

- [ ] **Step 1: Failing test** — extend the runtime-status ingest test: a sample runtime carrying `metrics_probe:"ok"`, `context_probe:"unreachable"` → the resulting `RuntimeStatusDTO` carries both. (These tags MUST match `sample.RuntimeSample`.)
- [ ] **Step 2: Run to fail** — `cd gateway/backend && go test ./internal/gateway/ -run RuntimeStatus`. FAIL.
- [ ] **Step 3: Implement** — add `MetricsProbe`/`ContextProbe string` (json `metrics_probe`/`context_probe`) after `QueueDepth` on BOTH `agentRuntimeSample` and `RuntimeStatusDTO`; map them in `runtimeStatusDTOsFromSamples`. Not omitempty.
- [ ] **Step 4: Run to pass** — `go test ./internal/gateway/`. PASS.
- [ ] **Step 5: Commit** — `feat(gateway): ingest probe reachability into RuntimeStatusDTO`.

---

### Task 4: Gateway — inject probe reachability onto `ModelServerDTO`

**Files:**
- Modify: `gateway/backend/internal/portal/service_model_servers.go` (`ModelServerDTO`, add the fields — near `State`/`ActiveRequests`/`QueueDepth`)
- Modify: `gateway/backend/internal/gateway/portal_model_endpoints.go` (`injectRuntimeModelState`)
- Test: `gateway/backend/internal/gateway/model_servers_endpoint_test.go`

**Interfaces — Produces:** `ModelServerDTO.{MetricsProbe, ContextProbe}` injected from the registry, gated on `runtime_model_probe`.

- [ ] **Step 1: Failing test** — extend the injection test (both the plain GET and the SSE case): with `runtime_model_probe` declared and a runtime status carrying the probe states, the `ModelServerDTO` row carries `metrics_probe`/`context_probe`; without the capability, they stay `""`.
- [ ] **Step 2: Run to fail** — `go test ./internal/gateway/ -run ModelServers`. FAIL.
- [ ] **Step 3: Implement** — add `MetricsProbe`/`ContextProbe string` (json `metrics_probe`/`context_probe`) to `ModelServerDTO` (doc them as gateway-injected, like `State`); in `injectRuntimeModelState`, set them from the joined `RuntimeStatusDTO` alongside `State`, under the SAME `s.AgentFeatures.Has(serverID, "runtime_model_probe")` gate the active/queue injection uses. (State is already injected unconditionally; the probe states go with the metrics gate.)
- [ ] **Step 4: Run to pass** — `go test ./internal/gateway/ ./internal/portal/`. PASS.
- [ ] **Step 5: Commit** — `feat(gateway): inject probe reachability into the model-servers list`.

---

### Task 5: Gateway — `loading_on_count` on the model overview DTO

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (the model-overview DTO — `LoadedOn`/`OfferedOnCount` at ~lines 960-963; the builder that sets `dto.OfferedOnCount = len(offeredOn[id])` at ~line 2255)
- Test: the `Service.Models` test (find it — grep `func TestModels`/`OfferedOnCount` under `internal/portal/*_test.go`)

**Interfaces — Produces:** the model-overview DTO gains `LoadingOnCount int json:"loading_on_count"` = number of servers offering the model whose managed spec is currently `starting`.

- [ ] **Step 0: Discovery** — read the `Service.Models` builder around lines 2080-2260 to learn how `loaded_on`/`offeredOn` are sourced, and whether the model-overview endpoint has a gateway-layer post-processing/injection step (analogous to `handlePortalModelServers`'s `Priority` injection at `portal_model_endpoints.go:88-91`). Decide the source of the `starting` count: the `RuntimeStatus` registry (`statusSnapshot(serverID)` → `RuntimeStatusDTO.State == "starting"`). If `Service.Models` has no access to that registry, compute `loading_on_count` at the gateway layer (mirror the `Priority`/model-servers injection) rather than in the portal service. Record the chosen seam in the task report.
- [ ] **Step 1: Failing test** — seed two servers offering one model, one with its spec `starting` in the runtime registry, `runtime_model_probe` declared → the model's DTO reports `loading_on_count == 1`. Without the capability / no starting server → `0`.
- [ ] **Step 2: Run to fail** — FAIL.
- [ ] **Step 3: Implement** — add `LoadingOnCount int json:"loading_on_count"` to the DTO (doc: gateway-injected/computed, like OfferedOnCount); compute the `starting` count per model from the registry at the seam chosen in Step 0, gated on `runtime_model_probe`. Best-effort: registry absent → 0.
- [ ] **Step 4: Run to pass** — `go test ./internal/gateway/ ./internal/portal/`. PASS.
- [ ] **Step 5: Commit** — `feat(gateway): loading_on_count (servers currently loading a model)`.

---

### Task 6: Frontend — `RuntimeAdminSection` "Probes" column

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeStatus` type, ~lines 177-189 — add `metrics_probe`/`context_probe`)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (columns array ~2918, add the column near `live_status` ~2992)
- Modify: `gateway/frontend/src/i18n.ts` (de+en)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`

- [ ] **Step 1: Failing test** — the mapping live-status table renders a "Probes" column: for a mapping whose live status has `metrics_probe:"unreachable"`, `context_probe:"ok"`, a metrics chip with `data-status` for "unreachable" and a context chip for "ok" render; for `"na"` a neutral chip; nothing renders when both are `""`.
- [ ] **Step 2: Run to fail** — `cd gateway/frontend && npm test -- RuntimeAdminSection`. FAIL.
- [ ] **Step 3: Implement** — add `metrics_probe`/`context_probe: string` to `RuntimeStatus`. Add a small helper mapping a probe state → StatusChip `status` (`ok`→success/active, `unreachable`→a warn/error status, `na`→standby) + label. Add a `probes` column: two labeled chips (M / C) from the live status joined to the mapping (reuse the existing `statusForMapping`/`statusBySpecId` join). i18n keys for the column label, the M/C prefixes, and a tooltip ("metrics/context endpoint not reachable — check the server arguments").
- [ ] **Step 4: Gates** — `npm run format:check && npm run lint && npm run build && npm test`. Green.
- [ ] **Step 5: Commit** — `feat(frontend): probe-reachability column in the runtime live-status table`.

---

### Task 7: Frontend — `ModelServersSection` merged "Status" + "—" gate

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (`ModelServerRow` — add `metrics_probe`/`context_probe`)
- Modify: `gateway/frontend/src/components/ModelServersSection.tsx` (replace the `loaded` (137-147) + `state` (159-168) columns with one `status` column; gate `active` (175), `queue` (185), `context` (213) on the probe state)
- Modify: `gateway/frontend/src/i18n.ts` (de+en)
- Test: `gateway/frontend/src/components/ModelServersSection.test.tsx`

- [ ] **Step 1: Failing tests** — (a) the "Status" column shows "Geladen" for `state:"running"`, "Lädt" for `state:"starting"`, "Nicht Geladen" otherwise, and (state `""`) falls back to the `loaded` boolean; the old separate "Geladen"/"Live-Status" columns are gone. (b) `active`/`queue` render `—` when `metrics_probe != "ok"` and the number (incl. a real `0`) when `"ok"`; `context` renders `—` when `context_probe != "ok"`, the size when `"ok"`. Assert per-column via cell position and specific values.
- [ ] **Step 2: Run to fail** — `npm test -- ModelServersSection`. FAIL.
- [ ] **Step 3: Implement** — add the two probe fields to `ModelServerRow`. Replace the two columns with one `status` column (label `t.tableModelStatus` new) using `runtimeStateBadge`/`runtimeStateLabel` for running/starting and the `loaded` fallback for empty state; add i18n for the tri-state labels ("Nicht Geladen"/"Lädt"/"Geladen" — reuse existing loaded/loading labels where they exist). Gate the three numeric columns' `render` on the respective probe state (helper: `probeOk(state) => state === "ok"`), showing `—` otherwise.
- [ ] **Step 4: Gates** — `npm run format:check && npm run lint && npm run build && npm test`. Green.
- [ ] **Step 5: Commit** — `feat(frontend): consolidated Status column + probe-gated metrics in the model detail`.

---

### Task 8: Frontend — `ModelList` "Lädt" column

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (`ModelOption` — add `loading_on_count`)
- Modify: `gateway/frontend/src/components/ModelList.tsx` (insert a column between `offered` (127-133) and `loaded` (139-145))
- Modify: `gateway/frontend/src/i18n.ts` (de+en)
- Test: `gateway/frontend/src/components/ModelList.test.tsx`

- [ ] **Step 1: Failing test** — a model with `loading_on_count: 2` renders a "Lädt" StatusChip with `data-status` "watch" (yellow) and label "2", positioned between "Angeboten" and "Geladen"; `loading_on_count: 0` renders nothing in that cell (mirror the `offered`/`loaded` shown-only-when-count>0 pattern).
- [ ] **Step 2: Run to fail** — `npm test -- ModelList`. FAIL.
- [ ] **Step 3: Implement** — add `loading_on_count: number` to `ModelOption`; insert a `loading` column between `offered` and `loaded`: `StatusChip status="watch" label={String(m.loading_on_count)}` shown only when `> 0` (mirror the loaded column); i18n key `tableModelLoading` = "Lädt" (de) / "Loading" (en).
- [ ] **Step 4: Gates** — `npm run format:check && npm run lint && npm run build && npm test`. Green.
- [ ] **Step 5: Commit** — `feat(frontend): yellow "Lädt" (loading) count column in the model overview`.

---

### Task 9: Docs

**Files:** `docs/architecture/cross-cutting/agent-runtime-manager.md`, `.../telemetry-usage-observability.md`, `reference/api-surface.md` (and the model-views section if one exists).

- [ ] **Step 1** — agent-runtime-manager: the per-probe reachability three-state (`metrics_probe`/`context_probe`: ok/unreachable/na) on the `RuntimeSample` channel; only for running children; best-effort.
- [ ] **Step 2** — telemetry-usage-observability + api-surface: the new `RuntimeStatusDTO`/`ModelServerDTO` fields; the model-overview `loading_on_count`; the UI semantics (admin "Probes" indicators; "—" instead of `0` when a probe isn't `ok`; the consolidated "Status" tri-state; the "Lädt" count). Note part 2 (#49) as the follow-up for richer telemetry.
- [ ] **Step 3** — `bash scripts/check-docs.sh` passes. **Commit** — `docs: probe reachability + model runtime-status presentation`.

---

### Task 10: Verification, cleanup, PR

- [ ] **Step 1** — `cd gateway/backend && go test ./...` (sqlite).
- [ ] **Step 2** — Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (no migration here, but the store CRUD/DTO paths still exercise both dialects).
- [ ] **Step 3** — `cd server-agent && go test ./...`.
- [ ] **Step 4** — frontend `npm run format:check && npm run lint && npm run build && npm test`.
- [ ] **Step 5** — Sonar gate (`make sonar-gate` + `make sonar-findings` + `make sonar-branch-findings` + `make sonar-down`); judge by branch-attributed findings only; fix any attributed to this branch, re-scan to 0.
- [ ] **Step 6** — leak/consistency grep + `check-docs.sh`.
- [ ] **Step 7** — `git rm -r docs/superpowers && commit`.
- [ ] **Step 8** — push the branch + open the PR against `main` with a summary (probe reachability three-state + admin indicators; "—" instead of 0; consolidated Status column; "Lädt" count). Give the commit(s) a real body so the squash description is populated.

---

## Self-Review

**Spec coverage:** A (agent state T1/T2, wire T3, ModelServerDTO T4, admin column T6, "—" gate T7) ✓; B (Status merge T7) ✓; C (backend `loading_on_count` T5, frontend column T8) ✓; docs T9; verification T10.

**Placeholder scan:** No TBD/"handle edge cases". T5 Step 0 is a bounded discovery (the one genuine unknown: the layer that can reach the `starting` state for the overview), not a vague placeholder.

**Type consistency:** `metrics_probe`/`context_probe` string with values `ok`/`unreachable`/`na` flow `RuntimeSample` (T1) → `agentRuntimeSample`/`RuntimeStatusDTO` (T3) → `ModelServerDTO` (T4) → frontend `RuntimeStatus`/`ModelServerRow` (T6/T7), snake_case identical at every hop. `loading_on_count` (T5) → `ModelOption` (T8). Gating on `runtime_model_probe` matches the existing active/queue injection.

# vllm-sr Phase 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve decision models from vLLM Semantic Router: an operator-run Engine-mode instance as application type `vllm_sr`, and the agent-launched worker `vllm-srun` as runtime spec type `vllm_sr`, with an api_version warning.

**Architecture:** Two new type strings plumbed through the existing type switches (validation, detection, probe paths, discovery, passthrough guard, frontend defaults). The agent learns to read vllm-sr's context limit, queue gauge and api_version (additive sample field, PATCH bump); the gateway compares the major with the pinned one and warns in the portal.

**Tech Stack:** Go (gateway/backend, server-agent), React + TypeScript + Vitest (gateway/frontend).

**Spec:** `docs/superpowers/specs/2026-11-12-vllm-sr-phase3-design.md` (authority). Pointers: `docs/superpowers/specs/2026-11-12-vllm-sr-inventory.md`. Contract: `docs/superpowers/specs/2026-11-12-vllm-sr-contract.md`.

## Global Constraints

- Branch `feat/vllm-sr-373-phase3`; never commit to `main`. Commit messages in English, conventional style, `(#373)` suffix, no attribution lines.
- No LAN host names in repo text. Code and docs never cite `docs/superpowers/`; cite ADR-099.
- Type strings exactly `vllm_sr` (application type and runtime spec type share it); pinned commit `4ec449f358ce7805228fb341bdc55ea5211707fd`; supported api major `5`.
- No migration. Server agent: PATCH bump `0.18.1` → `0.18.2` (check main's current version before the PR); no new agent feature.
- Go: golangci-lint v2.15.0 clean per module (`gateway/backend`, `server-agent`); each new function's cognitive complexity ≤ 15. Frontend: `npm test`, `npm run build`, `npm run lint`, `npm run format:check`.
- Known local-only failures, not to chase: `TestVoice*` / `TestLiveTranscriptionLoopback*`; `make lint-docs` agent-extractor line on macOS (#377).

## Review Focus

1. A spec whose binary is `/opt/vllm-sr/.venv/bin/vllm-srun` with type Auto — must detect as `vllm_sr`, never `vllm` (else it gets `decision_openai` probes and vLLM metrics names).
2. An application of type `vllm_sr` saved with `decisions_mode: passthrough` via the API (not the form) — 400; an existing app retyped to `vllm_sr` with a stored passthrough — reset to `auto`.
3. An `api_version` like `"5.1.0"`, `"6.0.0"`, `"5"`, `""`, `"garbage"` — warn only on a parseable major ≠ 5; never on unknown.
4. A vllm-sr `/v1/models` body where the first `data[]` entry lacks `limits.max_input_tokens` but a later one has it.
5. A translate request to a vllm-sr upstream answering 5.0-shaped 422 `{detail:[{loc,msg,type}],code:"question_failed",…}` and 529 — the client gets `decisions.upstream_error` with the upstream status and the `msg` text.

---

### Task 1: Gateway types, detection, discovery and guards

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`, `internal/routing/runtime_spec_type.go`, `internal/portal/service_applications.go`, `internal/portal/service_runtime.go`, `internal/portal/service.go` (`viewRuntimeType`), `internal/provider/openai_compatible.go` (`modelDiscoveryFor`), `cmd/gateway/main.go` (`providerClients`), `internal/gateway/portal_runtime_endpoints.go` (type error text), allowlist doc comments that count type strings
- Test: `internal/routing/runtime_spec_type_test.go`, `internal/portal/service_applications_test.go`, `internal/portal/service_decision_modes_test.go`, `internal/portal/service_runtime_test.go` (warning), `internal/provider/openai_compatible_test.go` (discovery), `internal/provider/live_progress_kind_parity_test.go`, `internal/gateway/portal_application_endpoint_mode_test.go`, `internal/gateway/portal_runtime_endpoints_test.go`, `cmd/gateway/app_health_decisions_probe_test.go`

**Interfaces:**
- Produces: `routing.ProviderVllmSR = "vllm_sr"`; `routing.RuntimeSpecTypeVllmSR RuntimeSpecType = "vllm_sr"`; `routing.VllmSRSupportedAPIMajor = 5` (doc comment names the pinned commit); RuntimeWarning string `api_flavors_text_on_vllm_sr`.

- [ ] **Step 1: Failing tests**
  - `TestDetectRuntimeSpecTypeVllmSR`: `vllm-srun`, `/opt/vllm-sr/.venv/bin/vllm-srun`, `vllm_srun`, `vllm-sr` → `vllm_sr`; `vllm`, `/usr/bin/vllm`, `python -m vllm.entrypoints...` stay `vllm`.
  - `TestDeriveProbePathsVllmSR`: (`/metrics`, `/v1/models`).
  - App create with type `vllm_sr` accepted, timeout default 600000; spec type `vllm_sr` accepted; the type-invalid error text lists `vllm_sr`.
  - Passthrough refused: app create `vllm_sr` + passthrough → 400 `application.decisions_passthrough_unsupported`; update keeping stored `vllm_sr` → 409; retype to `vllm_sr` with stored passthrough → `auto`; spec `vllm_sr` + passthrough → 400.
  - Discovery: `ListModels` for a `vllm_sr` target GETs `/v1/systemone/models` and decodes `{"object":"list","data":[{"id":"d3-mini","object":"model","api":"systemone"}]}`.
  - Probe scope: a `vllm_sr` app and a `vllm_sr` spec (explicit and Auto-detected from `vllm-srun`) never get the `decision_openai` probe; they do get the System One probe.
  - RuntimeWarnings: a `vllm_sr` spec with flavors `[openai, decisions]` yields `api_flavors_text_on_vllm_sr`; `[decisions]` yields none.
  - `viewRuntimeType` maps an app of type `vllm_sr` to spec type `vllm_sr`.
- [ ] **Step 2: Run** `go test ./internal/routing/ ./internal/portal/ ./internal/provider/ ./cmd/gateway/ -run 'VllmSR|Detect|DeriveProbe|Decision|Discovery|RuntimeWarning|Kind'` — FAIL.
- [ ] **Step 3: Implement** (inventory "Application type" and "Runtime spec type").
- [ ] **Step 4: Run** the packages plus `go test ./internal/gateway/ -run 'Portal|Runtime|Application'` — PASS; lint clean.
- [ ] **Step 5: Commit** `feat(routing): vllm_sr application and runtime spec types (#373)`

### Task 2: Server agent — context, queue gauge, api_version

**Files:**
- Modify: `server-agent/internal/collector/probe.go` (`extractContext` + a vllm-sr extractor; return the api_version alongside, or a separate reader of the same body), `server-agent/internal/collector/scrape.go` (waiting metric `vllm_srun_queue_depth`), `server-agent/internal/sample/sample.go` (`RuntimeAPIVersion string json:"runtime_api_version,omitempty"` on the runtime sample), `server-agent/internal/agent/agent.go` (record it for `vllm_sr` children from the context probe; skip `probeRuntimeChildProps` for `vllm_sr`; `Version` 0.18.2 with a history line), docs comments that list per-type behavior
- Test: `server-agent/internal/collector/probe_test.go`, `scrape_test.go`, `server-agent/internal/agent/*_test.go`

**Interfaces:**
- Consumes: wire `type: "vllm_sr"`, `context_probe_path: "/v1/models"`, `metrics_path: "/metrics"` (Task 1's DeriveProbePaths).
- Produces: runtime sample JSON field `runtime_api_version` (omitted when unknown).

- [ ] **Step 1: Failing tests:** `extractContext("vllm_sr", body)` with `data[0]` lacking limits and `data[1].limits.max_input_tokens: 32768` → 32768; without any → best-effort fallback; scrape of a body with `vllm_srun_queue_depth 3` → waiting 3, running absent; a `vllm_sr` child's sample carries `runtime_api_version: "5.0.0"` from the probed `/v1/models`; a `vllm` child's sample does not; no `/props` request for a `vllm_sr` child.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ ./internal/agent/ ./internal/sample/` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./...` in server-agent — PASS; lint clean.
- [ ] **Step 5: Commit** `feat(agent): read vllm-sr's input limit, queue depth and api_version (#373)`

### Task 3: Gateway — api_version ingest, warning, 5.0 error shapes

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go`, `internal/gateway/runtime_registry.go`, `internal/portal/service_model_servers.go` (row fields), a small helper `routing.VllmSRAPIVersionUnsupported(version string) bool` (or in portal), the ingest log
- Test: ingest/registry tests, `internal/portal/service_model_servers_test.go`, `internal/gateway/decisions_handler_test.go` (5.0-shaped 422 and 529 through translate)

**Interfaces:**
- Consumes: Task 2's `runtime_api_version`; Task 1's `VllmSRSupportedAPIMajor`.
- Produces: model-servers row JSON `runtime_api_version` (string, omitted/empty when unknown) and `runtime_api_version_unsupported` (bool).

- [ ] **Step 1: Failing tests:** helper table (`5.0.0`→false, `5.1.0`→false, `5`→false, `6.0.0`→true, `4.0.0`→true, `""`→false, `garbage`→false); ingest stores and the row exposes both fields for a `vllm_sr` runtime; a non-`vllm_sr` runtime never reports unsupported; one Warn per change into an unsupported major (not per sample); translate against an upstream answering 422 `{"detail":[{"loc":["body","questions","q0"],"msg":"type_unsupported","type":"value_error"}],"code":"question_failed"}` → 422 `decisions.upstream_error` whose message contains `type_unsupported`; 529 → 529 `decisions.upstream_error`.
- [ ] **Step 2: Run** focused `go test` — FAIL (the error-shape tests may already pass; then they pin it).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/gateway/ -skip 'TestVoice|TestLiveTranscriptionLoopback'`, `./internal/portal/`, `./internal/routing/` — PASS; lint clean.
- [ ] **Step 5: Commit** `feat(gateway): warn when a vllm-sr worker reports an unsupported api_version (#373)`

### Task 4: Portal frontend

**Files:**
- Modify: `gateway/frontend/src/api/models.ts`, `api/runtime.ts`, the model-servers row type, `components/ApplicationSection.tsx`, `components/shared/applicationTypeDefaults.ts`, `components/shared/decisionsModes.ts`, `components/RuntimeAdminSection.tsx`, `components/shared/runtimeSpecTypeFlavors.ts`, the model-servers table component (warning chip), `i18n.ts`
- Test: the matching tests named in the inventory (liveTimings.test.ts exhaustive records, applicationTypeDefaults.test.ts, runtimeSpecTypeFlavors.test.ts, ApplicationSection.test.tsx, RuntimeAdminSection.test.tsx, decisionsModes tests, i18n.test.ts, the model-servers table test)

**Interfaces:**
- Consumes: type string `vllm_sr`; row fields `runtime_api_version`, `runtime_api_version_unsupported`; RuntimeWarning `api_flavors_text_on_vllm_sr`.

- [ ] **Step 1: Failing tests:** `vllm_sr` in the app type options with defaults (port 8899, flavors `[decisions]`, responses/messages `disabled`, decisions `auto`, systemone `passthrough`, health path `/v1/systemone/models`, timeout 600000) and its type note; spec type option `vllm_sr` with flavor defaults `[decisions]`, both coding modes disabled, decisions `auto`, and its type note (launch template, pinned commit, install, loopback, startup-timeout hint); passthrough not offered for `vllm_sr` (app and spec), a stored passthrough becomes `auto` on retype; the warning chip shows the version when `runtime_api_version_unsupported`; the RuntimeWarning message renders; DE+EN i18n keys.
- [ ] **Step 2: Run** `npx vitest run <touched tests>` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test`, `npm run build`, `npm run lint`, `npm run format:check` — PASS.
- [ ] **Step 5: Commit** `feat(portal): vllm_sr application and runtime spec types with the api_version warning (#373)`

### Task 5: Documentation and ADR-099

**Files:**
- Modify: docs listed in the inventory "Docs" section; `docs/architecture/09-architecture-decisions.md` (ADR-099 per spec §8, Context/Decision/Rejected/Consequence like ADR-097/098; "Amended by" notes where the repo convention applies); `docs/architecture/cross-cutting/compatibility-and-inference.md` (§3.9/§3.10: vllm-sr as a backend; the 5.0 error envelope and 529; the Engine 30 s deadline); `agent-runtime-manager.md` §3.4 (type, detection order, probe paths, context field, queue gauge, api_version), the agent version history if documented, README/AGENTS server lists, glossary entry `vllm_sr`.
- [ ] **Step 1:** Write every claim from the code as merged in Tasks 1–4, and the pinned commit, the supported major, and the "not verified live" list (spec §7).
- [ ] **Step 2: Run** `make lint-docs` — only the #377 line and the branch-local plan reference may remain.
- [ ] **Step 3: Commit** `docs(architecture): vLLM Semantic Router types and ADR-099 (#373)`

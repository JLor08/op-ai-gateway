# Strata Support Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve Strata through the gateway as application type and agent-managed runtime spec type `strata`, with a run-config import, a main-last GPU placeholder and an opt-in Windows process-tree mode that stops and measures the whole process tree.

**Architecture:** Strata is a llama.cpp-like OpenAI/Anthropic server, so the type reuses the shared OpenAI-compatible client and the existing passthrough modes (structural checklist: PR #379 `vllm_sr`). The run config travels as an agent-written config file (ADR-066). Two agent capabilities are new: placeholder `${HOST_GPU_IDS_MAIN_LAST}` and `process_tree = job` (Windows job object + Ctrl-Break + tree-wide RAM/VRAM attribution), negotiated by feature flags.

**Tech Stack:** Go (gateway `gateway/backend`, agent `server-agent`, `golang.org/x/sys/windows`), React/TypeScript/Vite portal (`gateway/frontend`), vitest, SQLite/Postgres/memory stores.

**Spec:** `docs/superpowers/specs/2026-11-22-strata-support-design.md` (issue #424). Touchpoint map (outside repo): `…/side-session-notes/local_29e28c94-…/gateway-integration-touchpoints.md` — implementers of Tasks 1–3 read it; it lists every type-list site with line hints.

## Global Constraints

- Type string is exactly `strata` for both `routing.ProviderStrata` and `routing.RuntimeSpecTypeStrata`.
- Agent `Version` goes 0.20.1 → **0.21.0** once (Task 7), flags `runtime_gpu_main_last` and `runtime_process_tree_job`, `Since: "0.21.0"`. Rebase onto main's version before finalizing.
- Migration **113** `runtime_spec_process_tree`: `agent_runtime_specs.process_tree text not null default 'direct'`; all three stores; postgres leg run with `OP_AI_GATEWAY_TEST_POSTGRES_DSN`.
- `process_tree` values: `direct` (default) | `job`. Error codes: 400 `runtime_spec.process_tree_invalid`, 400 `runtime_spec.process_tree_unsupported`.
- No secret in config-file content; Strata key only via `env` `STRATA_API_KEY=${API_TOKEN}`.
- No host names, Windows user names or keys in repo text; fixture paths use `C:\Users\<user>\…`.
- i18n: every new key in DE and EN together.
- Existing specs/applications behave byte-for-byte as before (`direct` default, no new defaults on existing types).
- Live timings stay off for `strata`.
- Pre-PR gates: per Go module `golangci-lint fmt --diff` and `golangci-lint run`, `go test ./...`; agent also `GOOS=windows go vet ./...` and `GOOS=windows go build ./...`; portal `npm test`, `npm run build`, `npm run lint`, `npm run format:check`; `make lint-docs`; `make sonar-scan` + branch findings.

## Review Focus

1. Strata share listener returns 404 on `/props`, `/metrics`, `/health` → app stays healthy, no error badge, context from `/v1/models` (Task 2 test).
2. `${HOST_GPU_IDS_MAIN_LAST}` with one GPU, with duplicates (`2,2,0`), and with no GPUs (hard error) (Task 6 tests).
3. Pasted run config containing `api_key` with a value, or invalid JSON, or missing `args` → invalid JSON / missing `exe` / non-array `args` rejected inline with nothing stored; `api_key` dropped with an inline notice (Task 5 tests).
4. `job` spec saved for an agent on Linux / without the flag → 400 `process_tree_unsupported`; agent not yet reported → saved with warning (Task 7 tests).
5. Ctrl-Break cannot be delivered (no console, wrong account) → stop still completes within `killGrace` via job kill, no hang (Task 8 test with injected failing delivery).

---

## Phase 1 — Application type `strata`

### Task 1: Routing kinds and provider client

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (Provider consts), `runtime_spec_type.go` (const, `DetectRuntimeSpecType`, `DeriveProbePaths`), `model_role.go` (`DeriveRole`), `decisions_route.go` (`DecisionsPassthroughCapableKind`), `live_timings.go` (doc "not listed" enumeration only)
- Modify: `gateway/backend/cmd/gateway/main.go` (`providerClients`: `routing.ProviderStrata: openAICompatible`)
- Modify: `gateway/frontend/src/components/shared/decisionsModes.ts` (mirror decisions exclusion)
- Test: `routing/runtime_spec_type_test.go`, `model_role_test.go`, `decisions_route_test.go`, `live_timings_test.go`, `internal/provider/live_progress_kind_parity_test.go`, `decisionsModes.test.ts`

**Interfaces:**
- Produces: `routing.ProviderStrata = "strata"`, `routing.RuntimeSpecTypeStrata RuntimeSpecType = "strata"`.

- [ ] **Step 1: Failing tests**
  - `TestDetectRuntimeSpecTypeStrata`: `DetectRuntimeSpecType(`C:\…\bin\strata.cmd`)` and `/home/u/.local/bin/strata` → `RuntimeSpecTypeStrata`; `strata-vision.exe` too; `python.exe` → `custom`; existing vllm/llama rows unchanged.
  - `TestDeriveProbePathsStrata`: metrics `/metrics`, context probe `/props`.
  - `DeriveRole` table row: strata → `RoleChat`.
  - `DecisionsPassthroughCapableKind("strata") == false`.
  - Add both strata strings to the `kinds` slice in `TestLiveProgressUpstreamsCoverEveryRoutingCapableKind` and assert not live-progress/live-timings capable.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/routing/ ./internal/provider/` → FAIL (undefined).
- [ ] **Step 3: Implement** the constants (doc comment: shares the OpenAI-compatible client; upstream pinned `Niko1221/Strata@c6f6427`, 0.2.17/0.1.63), detection case placed so `strata` is not shadowed and does not shadow, probe paths, role, decisions exclusion, `providerClients` entry, `decisionsModes.ts` predicates.
- [ ] **Step 4: Run** the same tests plus `npx vitest run src/components/shared/decisionsModes.test.ts` → PASS.
- [ ] **Step 5: Commit** `feat(routing): strata provider and runtime spec kinds (#424)`.

### Task 2: Application type in portal service, frontend and probes

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go` (`normalizeApplicationType`), `service.go` (`viewRuntimeType`)
- Modify: `gateway/backend/cmd/gateway/app_health.go` only if the test below shows a 404 on the context/props probe or metrics scrape marks the app degraded
- Modify: `gateway/frontend/src/api/models.ts`, `components/ApplicationSection.tsx` (options, type note), `components/shared/applicationTypeDefaults.ts`, `components/shared/liveTimings.ts` keys if typed, `i18n.ts` (`applicationTypeStrataNote` DE+EN)
- Test: `portal/service_applications_test.go` (all-valid-types list), `cmd/gateway/app_health_strata_test.go` (new), `applicationTypeDefaults.test.ts`, `ApplicationSection.test.tsx`, `liveTimings.test.ts`, `i18n.test.ts`

**Interfaces:**
- Consumes: Task 1 constants.
- Produces: frontend `ApplicationType` includes `'strata'`; defaults `{port: 8080, scheme: 'http', flavors: ['openai','anthropic'], responsesMode: 'passthrough', messagesMode: 'passthrough', healthPath: '/v1/models', loadedModelsPath: '/v1/models', loadedModelsFormat: 'openai', contextProbePath: '', timeoutMs: <as llama_cpp>}`.

- [ ] **Step 1: Failing tests**
  - Go: create application type `strata` succeeds; appears in the accept-all list.
  - Go `TestStrataShareListenerHealthy` (httptest server mimicking the share: `/v1/models` requires `Authorization: Bearer k` → 200 with the measured body `{"object":"list","data":[{"id":"unsloth-q8_0","owned_by":"strata","status":{"value":"loaded"},"architecture":{"input_modalities":["text","image"]},"context_length":262144}]}`; 401 without; everything else 404): app with `APIToken "k"` → healthy, model `unsloth-q8_0` discovered, context 262144 recorded, no error state.
  - Frontend: defaults entry equals the object above; ApplicationSection offers `strata`, prefills defaults, shows the note; i18n block for strata keys (exist both locales, EN ≠ DE, non-empty).
- [ ] **Step 2: Run** `go test ./internal/portal/ ./cmd/gateway/ -run 'Strata|AllValidTypes'` and `npx vitest run` on the touched test files → FAIL.
- [ ] **Step 3: Implement.** If discovery does not read `context_length` from `/v1/models` today, extend the OpenAI `/v1/models` parser to take `context_length` (fallback `meta.n_ctx`) as context for any type — the builder verifies whether a generic path already exists (`provider/loaded_models.go`, `app_health.go` model sync) and reuses it. Health probe must send `APIToken`; if it does not, fix it here and note it in the ADR. Type note text (EN; DE equivalent): "Strata (Niko1221/Strata). Works with `strata serve` and with the desktop app's LAN share (app port + 1). Set the Strata API key as the application token — the share requires it, and a keyless Strata refuses host names. One model per Strata process. Older Strata releases answer 404 on /v1/responses: set the Responses mode to translate."
- [ ] **Step 4: Run** tests → PASS; `npm run format:check`.
- [ ] **Step 5: Commit** `feat(portal): strata application type (#424)`.

### Task 3: Media guard decision and Anthropic usage pin

**Files:**
- Modify (only if Strata reads local paths): `gateway/backend/internal/gateway/inference_resolve.go` (`mediaURLsRefused`)
- Test: `internal/gateway/native_passthrough_strata_usage_test.go` (new); `inference_resolve_test.go` if guarded

- [ ] **Step 1:** Read upstream `terminal/serve/` image handling at `c6f6427` (`git clone --depth 1` into a fresh empty dir under `/tmp`, read only). Decide: guard iff it opens `file://` or local paths. Record the finding (file:line) in the commit body.
- [ ] **Step 2: Failing test** `TestStrataMessagesPassthroughUsageSumsCacheFields`: passthrough `/v1/messages` to a stub returning usage `{"input_tokens":1,"output_tokens":20,"cache_creation_input_tokens":4,"cache_read_input_tokens":53}` (non-stream) and the same in a `message_delta` SSE (stream) → recorded input 58, cached 53, output 20. If guarded: `chat.media_url_unsupported` for a `file:///` image on a strata target. (The usage test should pass immediately — it pins existing behaviour; if it fails, that is a real bug to fix here.)
- [ ] **Step 3: Run / implement / run** → PASS.
- [ ] **Step 4: Commit** `test(gateway): pin Strata Anthropic usage accounting (#424)`.

## Phase 2 — Runtime spec type `strata` (gateway + portal)

### Task 4: Spec type validation and portal template

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`validRuntimeSpecType` + doc count), `internal/gateway/portal_runtime_endpoints.go` (400 message list)
- Modify: `gateway/frontend/src/api/runtime.ts` (type union), `components/RuntimeAdminSection.tsx` (type option, `runtimeSpecTypeNote`, template prefill on choosing `strata`), `i18n.ts` (`runtimeSpecTypeStrata`, `runtimeSpecTypeStrataNote`, API-token help row)
- Modify: `server-agent/internal/agent/agent.go` probe skip list only if `/props` absence would error (it exists on `strata serve`; leave default); `server-agent/internal/collector/probe.go` `extractContext` case `"strata"` → `extractLlamaCppContext` with fallback best-effort
- Test: `portal_runtime_endpoints_test.go`, `service_runtime_test.go` (accepts strata), `collector/probe_test.go`, `RuntimeAdminSection.test.tsx`, `i18n.test.ts`

**Interfaces:**
- Produces: frontend `strataSpecTemplate(): Partial<RuntimeSpecDraft>` in `components/shared/strataTemplate.ts` returning
  `{binaryHint: '%LOCALAPPDATA%\\Strata\\terminal\\current\\bin\\strata.cmd', args: ['serve','--config','${CONFIG_DIR}/strata.json','--host','0.0.0.0','--port','${PORT}','--gpu','${HOST_GPU_IDS_MAIN_LAST}','--output','jsonl','--shutdown-timeout','3'], env: {STRATA_API_KEY: '${API_TOKEN}'}, setVisibleDevices: false, processTree: 'job', startupTimeoutSeconds: 960, healthPath: '/health', flavors: ['openai','anthropic'], responsesMode: 'passthrough', messagesMode: 'passthrough'}`; pinned/idle/admission fields as the llama.cpp template. Bind address: if the llama.cpp/sglang templates use `127.0.0.1` with the agent proxy, use the same and adjust the args literal; the test pins whichever is chosen. `processTree` is wired in Task 12; until then the template omits it.

- [ ] **Step 1: Failing tests** — spec PUT with `type: strata` → 200; 400 message lists `strata`; choosing type `strata` in a new-spec form prefills exactly the template; note visible; `extractContext("strata", <props body n_ctx 262144>)` → 262144; i18n keys both locales.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** If the template's startup timeout trips `timeout_ms_below_startup_timeout` for the linked mapping, mirror how the llama.cpp template avoids it (spec §4.2).
- [ ] **Step 4: Run** → PASS; format:check.
- [ ] **Step 5: Commit** `feat(runtime): strata runtime spec type and portal template (#424)`.

### Task 5: Run-config import

**Files:**
- Create: `gateway/frontend/src/components/shared/strataRunConfig.ts`, `strataRunConfig.test.ts`, fixture `gateway/frontend/src/components/shared/__fixtures__/strata-launch-q8_0.json` (the owner's file shape, paths `C:\\Users\\<user>\\…`, no key)
- Modify: `components/RuntimeAdminSection.tsx` (Import action in the strata form), `i18n.ts`

**Interfaces:**
- Produces:
  ```ts
  export type StrataImport =
    | { ok: true; fileContent: string; gpuOrder: number[]; startupTimeoutSeconds: number;
        upstreamModel?: string; warnings: ('layer_split_positional' | 'api_key_dropped')[] }
    | { ok: false; error: 'invalid_json' | 'missing_exe' | 'missing_args' };
  export function importStrataRunConfig(text: string): StrataImport;
  ```

- [ ] **Step 1: Failing tests** (fixture-based):
  - drop list removed (`args_written`, `port`, `host`, `open_browser`, `gpus_asked`, `gpu`, `gpu_roles`, `gpu_cards`, `gpu_order`, every `ui_*`, `api_key`); kept `exe`, `args`, `cwd`, `tokenizer`, `log`, `lib_dirs`, `vision`, `layer_split`, `vram_reserve_mib`, `engine_ready_s`, and an unknown key `future_x`.
  - `model_name === "${MODEL}"`, `gpu_order === "as_given"`; output is pretty JSON (2-space).
  - `gpuOrder` from roles `{0:helper,1:helper,2:main}` → `[2,0,1]`; from legacy `gpu: [0,1,2]` without roles → `[2,0,1]`; single `gpu: 1` → `[1]`.
  - `startupTimeoutSeconds`: `engine_ready_s 1845` → 1905; absent → 960; `300` → 960.
  - `upstreamModel` = original `model_name`.
  - `api_key: "x"` → `ok: true`, key absent from `fileContent`, `warnings` contains `'api_key_dropped'`.
  - invalid JSON / no `exe` / `args` not an array → matching error.
  - `layer_split: "24"` → warning `layer_split_positional`.
- [ ] **Step 2: Run** `npx vitest run src/components/shared/strataRunConfig.test.ts` → FAIL.
- [ ] **Step 3: Implement** the pure function; wire the Import dialog to fill the config-files facet (`strata.json`), GPU rows (order), startup timeout and empty model fields; show warnings/errors inline (i18n DE+EN).
- [ ] **Step 4: Run** → PASS; component test: paste fixture → form has file `strata.json`, GPU rows `2,0,1`.
- [ ] **Step 5: Commit** `feat(portal): import a strata launch run config into a runtime spec (#424)`.

## Phase 3 — Agent capabilities and `process_tree`

### Task 6: Placeholder `${HOST_GPU_IDS_MAIN_LAST}`, flags and version

**Files:**
- Modify: `server-agent/internal/runtime/policy_local.go` (resolver next to `${HOST_GPU_IDS}`; args, env and config-file content expander), `internal/agent/features.go`, `internal/agent/agent.go` (`Version = "0.21.0"`), `features_test.go` pins
- Modify: `gateway/backend/internal/gateway/agent_features.go` (both names), portal placeholder hint list (`RuntimeAdminSection.tsx` / `i18n.ts`), `api/runtime.ts` if placeholders are enumerated
- Test: `runtime/policy_local_test.go` (or the existing placeholder test file), `agent/features_test.go`, `gateway/agent_features_test.go`

**Interfaces:**
- Produces: `func hostGPUIDsMainLast(gpus []int) string` (declared order, dedup first-wins, first moved last, comma-joined); flag consts `FeatureRuntimeGPUMainLast = "runtime_gpu_main_last"`, `FeatureRuntimeProcessTreeJob = "runtime_process_tree_job"` (both agent and gateway copies).

- [ ] **Step 1: Failing tests** — `[2,0,1]`→`"0,1,2"`; `[1]`→`"1"`; `[2,2,0]`→`"0,2"`; `[]` → expansion error `not_permitted` naming the token; resolves in args, env and config-file content; near-miss `${HOST_GPU_IDS_MAIN_LAS}` passes literally in content and errors per existing near-miss rules in args if applicable; features list contains both flags `Since "0.21.0"`; `Version == "0.21.0"`.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ ./internal/agent/` → FAIL.
- [ ] **Step 3: Implement.** The `runtime_process_tree_job` flag is declared here but only advertised on Windows (Task 8 sets the gate; until then declare it `Since` and gate `Advertised()`/equivalent on `runtime.GOOS == "windows"` if the features mechanism supports per-OS flags — follow how `runtime_exec_wsl` is gated).
- [ ] **Step 4: Run** → PASS; also `GOOS=windows go vet ./...`.
- [ ] **Step 5: Commit** `feat(agent): main-last GPU placeholder and 0.21.0 feature flags (#424)`.

### Task 7: `process_tree` field — gateway store, API, push

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (migration 113), `sqlite_runtime.go` (+ postgres/memory paths as #390 did for `execution_mode`), `store/runtime_spec_column_parity_test.go`
- Create: `gateway/backend/internal/routing/process_tree.go` (`type ProcessTree string`, `ProcessTreeDirect`, `ProcessTreeJob`, `NormalizeProcessTree(string) (ProcessTree, bool)`), `store/migration113_runtime_spec_process_tree_test.go`
- Modify: `routing/store.go` (`RuntimeSpec.ProcessTree`), `portal/service_runtime.go` (read/write, validation, refusal), `gateway/agent_runtime.go` (push field `process_tree`), `gateway/portal_runtime_endpoints.go` (error rows)
- Test: `portal/service_runtime_process_tree_test.go` (new)

**Interfaces:**
- Produces: spec JSON field `process_tree` (`"direct"|"job"`, empty → `direct`); agent DTO field `process_tree`.

- [ ] **Step 1: Failing tests**
  - migration 113: existing rows read `direct`; sqlite, memory and postgres (DSN set).
  - PUT `process_tree: "bogus"` → 400 `runtime_spec.process_tree_invalid`.
  - PUT `job` for an agent whose last report has OS `linux` → 400 `runtime_spec.process_tree_unsupported`; Windows agent without `runtime_process_tree_job` → same 400; Windows agent with flag → 200; agent never reported → 200 + warning code `process_tree_agent_unknown` in `RuntimeWarnings`.
  - change `direct`→`job` on a running spec classifies as launch-affecting.
  - push DTO carries `process_tree`.
- [ ] **Step 2: Run** `go test ./internal/store/ ./internal/portal/ ./internal/gateway/ -run 'ProcessTree|Migration113|ColumnParity'` (with and without `OP_AI_GATEWAY_TEST_POSTGRES_DSN`) → FAIL.
- [ ] **Step 3: Implement** following the `execution_mode` (#390, migration 109) plumbing exactly.
- [ ] **Step 4: Run** → PASS (postgres leg included).
- [ ] **Step 5: Commit** `feat(runtime): process_tree spec field with Windows job mode negotiation (#424)`.

### Task 8: Agent Windows job-mode process control

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (`Spec.ProcessTree string`, parse/validate `direct|job`), `proc_control.go` (choose control by spec), `proc_windows.go` (job implementation; replace the KNOWN LIMITATION comment), `proc_unix.go` (job == today's group; no change in behaviour), `manager.go` (start path: create suspended → assign → resume when job; close job on reap)
- Create: `server-agent/internal/runtime/proc_job_windows.go`, `proc_job_windows_test.go` (Windows-only build tag; runs on Windows hosts), `proc_job_other.go`
- Test: `runtime/types_process_tree_test.go` (cross-platform parse), `proc_job_windows_test.go`

**Interfaces:**
- Produces:
  ```go
  type jobControl struct { cmd *exec.Cmd; job windows.Handle; ctrlBreak func(pid int) error }
  func startInJob(cmd *exec.Cmd) (*jobControl, error) // CREATE_NEW_PROCESS_GROUP|CREATE_SUSPENDED (+ hidden console if no agent console), assign, resume
  func (c *jobControl) terminate() error // ctrlBreak; on error → kill()
  func (c *jobControl) kill() error      // TerminateJobObject(job, 1)
  func (c *jobControl) close() error     // CloseHandle(job) on reap
  func (c *jobControl) pids() []int      // QueryInformationJobObject(JobObjectBasicProcessIdList)
  ```
- Consumes: `Spec.ProcessTree` from Task 7's DTO.

- [ ] **Step 1: Failing tests** — cross-platform: `ProcessTree` parses `""`→`direct`, `job`, rejects others; on non-Windows `job` behaves as today. Windows-tagged: a test child (`testdata/stubchild` extended with `-spawn-grandchild` and `-ignore-ctrlbreak`) started in job mode: `pids()` contains child and grandchild; `terminate()` ends both within `killGrace` (ctrlBreak path and injected-failing ctrlBreak path → kill); child exiting alone → grandchild gone after `close()`; stdout of child still reaches the log writer.
- [ ] **Step 2: Run** cross-platform tests (`go test ./internal/runtime/ -run ProcessTree`) → FAIL; `GOOS=windows go vet ./internal/runtime/` compiles the Windows tests.
- [ ] **Step 3: Implement.** Ctrl-Break delivery: when the agent has no console, give the child its own console (`CREATE_NEW_CONSOLE` + `SW_HIDE` via `SysProcAttr.HideWindow`, stdio still redirected) and deliver with `AttachConsole(childPID)` + `SetConsoleCtrlHandler(nil, true)` + `GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, childPID)` + `FreeConsole` + restore handler, serialized by a mutex; when the agent has a console, `GenerateConsoleCtrlEvent` on the group directly. Any failure → log and `kill()`. Never block the manager owner goroutine longer than 1 s on delivery.
- [ ] **Step 4: Run** cross-platform → PASS; `GOOS=windows go build ./... && GOOS=windows go vet ./...` clean. Windows-tagged tests run in Task 12 on the owner's host.
- [ ] **Step 5: Commit** `feat(agent): Windows job-object process tree with Ctrl-Break stop (#424)`.

### Task 9: Tree-wide RAM and per-GPU VRAM on Windows

**Files:**
- Create: `server-agent/internal/collector/process_tree.go` (registry), `process_tree_test.go`
- Modify: `collector/ram_measurer_windows.go` (sum members), `collector/nvidia_groups_other.go` → split `nvidia_groups_windows.go` (map member PID → root), PDH path in `nvidia_pdh.go` / `measurer_windows.go` (same attribution for PDH rows), `runtime/manager.go` (register on job start, unregister on reap)

**Interfaces:**
- Produces:
  ```go
  // collector/process_tree.go
  func RegisterProcessTree(root int, members func() []int) (unregister func())
  func treeRootOf(pid int) (root int, ok bool)      // via registered members snapshots
  func treeMembers(root int) []int                   // includes root
  ```
  Runtime registers `jobControl.pids` for job-mode children.

- [ ] **Step 1: Failing tests** (pure Go, all platforms, registry with fake members):
  - RAM: root 100 with members {100,101,102} sizes 10/2000/300 MB → measurer returns 2310 MB for 100; unregistered pid 200 → own value.
  - VRAM rows: PDH rows pid 101 GPU0 8 GB, pid 101 GPU2 20 GB, pid 102 GPU2 1 GB → root 100: GPU0 8 GB, GPU2 21 GB; rows of unregistered pids unchanged.
  - unregister → attribution reverts.
- [ ] **Step 2: Run** `go test ./internal/collector/ -run ProcessTree` → FAIL.
- [ ] **Step 3: Implement**; Linux keeps #404's pgid grouping untouched.
- [ ] **Step 4: Run** → PASS; Windows vet/build.
- [ ] **Step 5: Commit** `feat(agent): attribute Windows RAM and per-GPU VRAM to the whole job tree (#424)`.

### Task 10: Portal `process_tree` control and template wiring

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`processTree?: 'direct'|'job'`), `components/RuntimeAdminSection.tsx` (control for native specs, Windows-only label, warning label for `process_tree_agent_unknown`, error mapping for the two 400 codes), `components/shared/strataTemplate.ts` (`processTree: 'job'`), `i18n.ts`
- Test: `RuntimeAdminSection.test.tsx`, `strataTemplate` test, `i18n.test.ts`

- [ ] **Step 1: Failing tests** — control rendered for native specs, hidden for `wsl`; default `direct`; strata template sets `job`; both 400 codes render localized messages; warning label exists DE+EN; placeholder hint lists `${HOST_GPU_IDS_MAIN_LAST}` with a one-line explanation ("first GPU is main, moved last").
- [ ] **Step 2–4:** run → FAIL, implement, run → PASS; `npm run lint`, `format:check`, `build`.
- [ ] **Step 5: Commit** `feat(portal): process tree mode control for runtime specs (#424)`.

## Phase 4 — Docs, verification, acceptance

### Task 11: Architecture docs, ADR-111, README/AGENTS

**Files:** `docs/architecture/09-architecture-decisions.md` (ADR-111), `cross-cutting/agent-runtime-manager.md` (type section, placeholder table, §3.5 example, Windows process tree replacing the kill-only note, §5.6 table, §7 flags, §11.5 warnings), `cross-cutting/routing-and-model-selection.md`, `cross-cutting/compatibility-and-inference.md` (only if Task 3 added a guard), `reference/api-surface.md` (type lists, error codes), `reference/data-model.md` (migration 113, type list), `12-glossary.md`, `README.md`, `AGENTS.md` summary, `server-agent/README.md` (Windows Strata setup: allowlist `strata.cmd`, agent under the installing account, unsupported under LocalSystem).

- [ ] **Step 1:** Write; every claim cites code or the measured upstream behaviour; no host/user names.
- [ ] **Step 2:** `make lint-docs` → PASS.
- [ ] **Step 3: Commit** `docs: Strata support, main-last placeholder and Windows process tree (#424)`.

### Task 12: Full verification and Windows acceptance

- [ ] **Step 1:** Run every gate in Global Constraints; postgres leg; `make sonar-scan`, `make sonar-findings && make sonar-branch-findings` → no branch-attributed findings left unaddressed.
- [ ] **Step 2:** Build the Windows agent (`GOOS=windows GOARCH=amd64 go build`), hand it to the owner with steps; with the owner, run spec §6 acceptance 1–4 on the Windows host and the Windows-tagged agent tests (`go test ./internal/runtime/ -run Job` on that host). Record outcomes (no host names) in the PR body; fix anything found (strata.cmd vs python.exe fallback decided here, recorded in ADR-111).
- [ ] **Step 3:** Remove `docs/superpowers/` and `docs/implementation-status.md`; `git diff --name-only main...HEAD` shows neither.
- [ ] **Step 4:** Merge `origin/main`, re-check agent version/migration number collisions, re-run gates, push over SSH, open PR, enable Auto-fix.

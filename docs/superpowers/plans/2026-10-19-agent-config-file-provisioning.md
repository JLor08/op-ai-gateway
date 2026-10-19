# Agent-provisioned config files (#257) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator provide 0..N config files per agent-managed runtime spec in the portal; the agent writes them (placeholder-expanded, non-secret) into a configurable agent-owned directory under a per-spec subdir before launching the child, referenced from `Args` via `${CONFIG_DIR}`.

**Architecture:** Thread an opaque-JSON `config_files` field through the gateway request DTO → store (3 drivers + migration v93) → the agent runtime-config document, mirroring how `Args`/`Env` already travel. On the agent, add a `${CONFIG_DIR}` placeholder, a non-secret content expander, and a write step in `startProcess` between `expandSpec` and `cmd.Start`. Gate the new agent behavior behind the `runtime_config_files` feature flag (version 0.10.0) and redact content in file-mode reports.

**Tech Stack:** Go (gateway `op-ai-gateway`, agent `op-ai-server-agent`), React/TypeScript/Vite portal, memory/sqlite/postgres stores.

**Spec:** `docs/superpowers/specs/2026-10-19-agent-config-file-provisioning-design.md`

## Global Constraints

- Variant A only (one child per spec); no shared-server reconciler.
- Secrets stay out of written content: `${API_TOKEN}` and `${AGENT_ENV:NAME}` are **rejected** in config-file content (gateway validation) and never expanded by the content expander (agent).
- Content expands only the non-secret token set: `${PORT}`, `${MODEL}`, `${HOST_GPU_IDS}`, `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}`, `${CONFIG_DIR}`.
- Files written `0600` into `<ConfigFilesDir>/<spec.ID>/<name>`, never into WorkDir, via the atomic temp-create-in-dir + chmod + rename pattern (`config_client.go:78-115`).
- Filename limits (new validation — no existing pattern): a single path segment (reject empty, `/`, `\`, `..`, absolute, control chars); unique within a spec; ≤ 16 files/spec; ≤ 64 KiB content/file.
- Keep all three store drivers working; schema change is an append-only migration (next version **93**); wide values → wide Postgres columns (ADR-005). Verify store work with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Preserve stable error codes. New gateway sentinel(s) under the `runtime_spec.*` namespace.
- ServerAgent version bump is **once per branch**: 0.9.2 → 0.10.0 (MINOR, new feature flag). Do it in Task 8 and leave it alone.
- Portal strings: add DE **and** EN keys together (`i18n.ts`; `PortalMessages = typeof de` enforces parity at compile time).
- Repo-facing text (commits, ADR, docs, code comments) in English.
- Pre-push gates to run before the PR: `golangci-lint fmt --diff` + `run` per Go module; frontend `format:check` (prettier), `lint`, `test`, `build`; local `make sonar-gate` + branch-findings.

## Review Focus

- **Path traversal in a filename** (`../../etc/cron.d/x`, absolute, `a/b`): must be rejected at gateway validation **and** sanitized agent-side (file-mode bypasses the gateway). → Task 1 + Task 5 tests.
- **Two specs naming the same file** (`server.json` under two mappings): must not collide — per-spec `<base>/<spec.ID>/` dir keeps them separate. → Task 7 test.
- **A secret placeholder typed into content** (`${API_TOKEN}`, `${AGENT_ENV:X}`): must be rejected, never written literally, never expanded to the secret. → Task 1 + Task 7 tests.
- **Non-writable base dir / disk full at launch:** `StateStartFailed` + backoff with a message naming the path — never `StateNotPermitted`, never a crash. → Task 7 test.
- **File-mode spec carrying content:** content stripped from the agent's upward report exactly as `Env` values are. → Task 8 test.

---

### Task 1: Gateway request DTO + validation + column serialization

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (DTO ~496-560; sentinels 29-110; `validateRuntimeSpecRequest` 769-897; shape fn 903+; `runtimeSpecFields` 745-761; `runtimeSpecRow` 1038+)
- Test: `gateway/backend/internal/portal/service_runtime_test.go` (or a new `service_runtime_config_files_test.go`)

**Interfaces:**
- Produces: `type ConfigFileDTO struct { Name string \`json:"name"\`; Content string \`json:"content"\` }`; `PutRuntimeSpecRequest.ConfigFiles []ConfigFileDTO \`json:"config_files,omitempty"\``; sentinel `ErrRuntimeSpecConfigFilesInvalid = errors.New("runtime_spec.config_files_invalid")`; resolved JSON string on `runtimeSpecFields.configFiles string` carried into `routing.RuntimeSpec.ConfigFiles` (Task 2).
- Consumes: nothing (first task).

- [ ] **Step 1: Write the failing tests** `TestValidateRuntimeSpecConfigFiles` — table: valid 2-file set marshals; reject empty name; reject name with `/`, `\`, `..`, leading `/`, control char; reject duplicate names; reject 17 files; reject a 64KiB+1 content; reject content containing `${API_TOKEN}`; reject content containing `${AGENT_ENV:X}`; empty/nil list ⇒ `""` serialized.
- [ ] **Step 2: Run to verify it fails** — `cd gateway/backend && go test ./internal/portal/ -run TestValidateRuntimeSpecConfigFiles -v` → FAIL (undefined symbols).
- [ ] **Step 3: Implement** the `ConfigFileDTO` type and `PutRuntimeSpecRequest.ConfigFiles`; a `validateRuntimeSpecConfigFiles(files []ConfigFileDTO) (string, error)` that sanitizes (basename via a single-segment check: reject empty, `filepath.Base(n) != n`, `..`, absolute, control chars), enforces uniqueness + the two limits, rejects the two secret placeholders (substring check for `${API_TOKEN}` / `${AGENT_ENV:`), and returns `json.Marshal` of the list (or `""` when empty). Call it from `validateRuntimeSpecRequest`; store the result on `runtimeSpecFields.configFiles`; set it on the spec in `runtimeSpecRow`. Mirror the Env-key loop (924-928) and `validateRuntimeSpecGPUs` (1458) style; add the sentinel in the 29-110 block.
- [ ] **Step 4: Run to verify it passes** — same command → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(gateway): validate and serialize runtime-spec config_files (#257)"`

---

### Task 2: Persist `config_files` across the three stores (migration v93)

**Files:**
- Modify: `gateway/backend/internal/routing/store.go:2230-2316` (`RuntimeSpec`); `gateway/backend/internal/store/migrate.go` (registry 35-128; add `migration93Up`); `gateway/backend/internal/store/sqlite_runtime.go` (cols 79-97, upsert 20-77, scan 354-377); `gateway/backend/internal/routing/memory_store.go` (`UpsertRuntimeSpec` 2561, `RuntimeSpecByMapping` 2585, `copyRuntimeSpec` 2450)
- Test: `gateway/backend/internal/store/runtime_spec_column_parity_test.go` (`TestRoutingStoreRuntimeSpecs`); migration test alongside `migrate.go`'s suite

**Interfaces:**
- Consumes: `runtimeSpecFields.configFiles` (Task 1).
- Produces: `routing.RuntimeSpec.ConfigFiles string` (opaque JSON array; store never parses — same contract as `Args`/`Env`). Because it is a `string`, `copyRuntimeSpec` needs no deep copy (keep the "no slice/pointer fields" invariant true).

- [ ] **Step 1: Write the failing test** — extend `TestRoutingStoreRuntimeSpecs` to seed a non-empty `ConfigFiles` JSON (e.g. `[{"name":"server.json","content":"x"}]`) and assert it round-trips identically across memory, sqlite, and (DSN-gated) postgres.
- [ ] **Step 2: Run to verify it fails** — `cd gateway/backend && go test ./internal/store/ -run TestRoutingStoreRuntimeSpecs -v` → FAIL (column/field missing).
- [ ] **Step 3: Implement** — add `ConfigFiles string` to `RuntimeSpec`; `migration93Up` = `addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs", "config_files text not null default ''")` registered at version 93 (copy the `migration80Up` precedent); add `config_files` to `runtimeSpecCols`/`runtimeSpecColsPrefixed`, the insert binds, and `scanRuntimeSpec`; thread it through `memory_store` upsert/read.
- [ ] **Step 4: Run to verify it passes** — the parity test with and without `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set → PASS. Also run the full `./internal/store/...` migration suite.
- [ ] **Step 5: Commit** — `git commit -m "feat(store): persist agent_runtime_specs.config_files, migration 93 (#257)"`

---

### Task 3: Carry `config_files` onto both DTOs (agent wire + read-back + benchmark)

**Files:**
- Modify: `service_runtime.go` — `AgentRuntimeSpecDTO` 2142-2192; `agentRuntimeSpecDTO` 2468-2514; `RuntimeSpecDTO` 407-495; `runtimeSpecDTO` 1480; empty-GET seed in `GetRuntimeSpec` 587-601; `service_runtime_benchmark.go` `putRequestFromDTO` 33-70
- Test: `service_runtime_test.go`

**Interfaces:**
- Consumes: `routing.RuntimeSpec.ConfigFiles` (Task 2).
- Produces: `AgentRuntimeSpecDTO.ConfigFiles []ConfigFileDTO \`json:"config_files"\`` (what the agent reads); `RuntimeSpecDTO.ConfigFiles []ConfigFileDTO \`json:"config_files"\`` (portal read-back). Agent-side consumer: Task 5.

- [ ] **Step 1: Write the failing tests** — `TestAgentRuntimeSpecDTOCarriesConfigFiles` (a stored spec whose `ConfigFiles` JSON unmarshals onto `AgentRuntimeSpecDTO.ConfigFiles`); `TestRuntimeSpecReadBackConfigFiles` (PUT→GET round-trip, empty GET seeds `[]`); `TestPutRequestFromDTOConfigFiles` (benchmark restore keeps the field).
- [ ] **Step 2: Run to verify they fail** — `cd gateway/backend && go test ./internal/portal/ -run 'ConfigFiles' -v` → FAIL.
- [ ] **Step 3: Implement** — add the field to both DTOs; `json.Unmarshal(spec.ConfigFiles)` into `AgentRuntimeSpecDTO` in `agentRuntimeSpecDTO` (mirror Args 2469-2475) and into `RuntimeSpecDTO` in `runtimeSpecDTO`; seed `[]ConfigFileDTO{}` on the empty GET; copy it in `putRequestFromDTO`.
- [ ] **Step 4: Run to verify they pass** — same filter → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(gateway): expose config_files on the agent and read-back DTOs (#257)"`

---

### Task 4: Agent config knob `ConfigFilesDir` (+ generators + golden)

**Files:**
- Modify: `server-agent/internal/config/config.go` (mirror `RuntimeCachePath`: default-name const ~110, Config field 209-214, fileConfig json 310, flag 353, resolveStr 430, default guard 448-449, resolve-against-config 454, `defaultConfigFilesDir()` beside `defaultRuntimeCachePath()` 502-508); `server-agent/internal/runtime/manager.go` (`ManagerOptions` 125-165, owner field 790+, `NewManager` 219-228); `server-agent/main.go` (`NewManager(ManagerOptions{...})` 168-186); `gateway/backend/internal/gateway/agent_binaries.go:362` (`buildAgentConfigJSON`); `gateway/frontend/src/components/AgentTokenSection.tsx:67` (`buildServerAgentConfig`); `server-agent/README.md`; regenerate `server-agent/testdata/server-agent.config.jsonc`
- Test: `server-agent/internal/config/config_test.go` (resolution + `TestGeneratedAgentConfigCoversEverySettingTheAgentCanRead` 1268, `...DocumentsEverySettingInTheREADME` 1300)

**Interfaces:**
- Produces: `config.Config.ConfigFilesDir string` (flag `-config-files-dir`, env `OP_AGENT_CONFIG_FILES_DIR`, config key `config_files_dir`, default `filepath.Dir(exe)/server-agent-config-files`); `ManagerOptions.ConfigFilesDir` → `owner.configFilesDir`. Consumer: Tasks 6 & 7.

- [ ] **Step 1: Write the failing test** — `TestConfigFilesDirResolution` (flag beats env beats file beats default; relative resolved against the config file), plus let the two golden/README reflection tests drive the generators.
- [ ] **Step 2: Run to verify it fails** — `cd server-agent && go test ./internal/config/ -run 'ConfigFilesDir|GeneratedAgentConfig' -v` → FAIL.
- [ ] **Step 3: Implement** — add the knob to `config.go` mirroring `RuntimeCachePath` exactly; thread `ConfigFilesDir` through `ManagerOptions`/owner/`NewManager`/`main.go`; add `config_files_dir` to both generators and a README backtick mention; regenerate the golden `.jsonc`.
- [ ] **Step 4: Run to verify it passes** — the config suite → PASS; also `cd gateway/backend && go test ./internal/gateway/ -run AgentConfig` and `cd gateway/frontend && npx vitest run src/components/AgentTokenSection` if present.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): configurable config_files_dir knob (#257)"`

---

### Task 5: `runtime.Spec.ConfigFiles` + ParseConfig normalize & sanitize

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (`Spec` 39-92; `ParseConfig` 273-315, loop 302-312)
- Test: `server-agent/internal/runtime/types_test.go`

**Interfaces:**
- Consumes: the `config_files` wire field (Task 3).
- Produces: `type ConfigFile struct { Name string \`json:"name"\`; Content string \`json:"content"\` }` in package `runtime`; `Spec.ConfigFiles []ConfigFile \`json:"config_files"\``; `ParseConfig` nil-normalizes and sanitizes filenames (single segment, unique) — file-mode specs bypass gateway validation, so this is the only guard there. Consumer: Tasks 6, 7, 8.

- [ ] **Step 1: Write the failing tests** — `TestParseConfigNormalizesConfigFiles` (nil → `[]`); `TestParseConfigRejectsBadConfigFileName` (`/`, `..`, absolute, duplicate within a spec → error).
- [ ] **Step 2: Run to verify they fail** — `cd server-agent && go test ./internal/runtime/ -run TestParseConfig -v` → FAIL.
- [ ] **Step 3: Implement** — add `ConfigFile` + `Spec.ConfigFiles`; in the 302-312 loop nil-normalize the slice and validate each name with a shared `validConfigFileName(string) bool` (reuse in Task 1 conceptually, but keep an agent-local copy — no cross-module import per the arch test).
- [ ] **Step 4: Run to verify they pass** — same filter → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): parse and sanitize runtime spec config_files (#257)"`

---

### Task 6: `${CONFIG_DIR}` placeholder in Args/Env expansion

**Files:**
- Modify: `server-agent/internal/runtime/policy_local.go` (`ExpandPlaceholders` 791-797, `expandSpec` 860+, exact-match closure 877-921); callers in `manager.go` (dry run 1753, real call 1771)
- Test: `server-agent/internal/runtime/policy_local_test.go`

**Interfaces:**
- Consumes: `owner.configFilesDir` (Task 4).
- Produces: `ExpandPlaceholders(spec Spec, port int, vendor GPUVendor, getenv func(string) string, configFilesDir string)` and `expandSpec(..., configFilesDir string)`; `${CONFIG_DIR}` → `filepath.Join(configFilesDir, spec.ID)`. Consumer: Task 7 (shared resolver) + `startProcess`.

- [ ] **Step 1: Write the failing test** — `TestExpandPlaceholdersConfigDir` (`${CONFIG_DIR}` in an arg → `<base>/<id>`; present in env too).
- [ ] **Step 2: Run to verify it fails** — `cd server-agent && go test ./internal/runtime/ -run TestExpandPlaceholdersConfigDir -v` → FAIL.
- [ ] **Step 3: Implement** — thread `configFilesDir` through both signatures; add a non-secret exact-match `CONFIG_DIR` branch beside `HOST_GPU_IDS` (895); update both `manager.go` call sites. (Updating the signature will touch all existing callers/tests — fix the compile.)
- [ ] **Step 4: Run to verify it passes** — the whole `./internal/runtime/` package compiles and `-run TestExpandPlaceholders` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): \${CONFIG_DIR} placeholder for args/env (#257)"`

---

### Task 7: Non-secret content expander + write-before-launch in `startProcess`

**Files:**
- Create: the content expander + atomic writer helpers in `server-agent/internal/runtime/policy_local.go` (or a new `config_files.go` in package `runtime`)
- Modify: `server-agent/internal/runtime/manager.go` (`startProcess` 1740-1856 — dry run 1753, insert write between 1779 and 1793; failure blocks mirror 1762-1766)
- Test: `server-agent/internal/runtime/manager_test.go`, `policy_local_test.go`

**Interfaces:**
- Consumes: `expandSpec`/`${CONFIG_DIR}` (Task 6), `Spec.ConfigFiles` (Task 5), `owner.configFilesDir` (Task 4), the atomic-write pattern (`config_client.go:78-115`).
- Produces: `expandConfigFileContent(content string, spec Spec, port int, vendor GPUVendor, getenv func(string) string, configFilesDir string) (string, error)` (non-secret token set incl. `${CONFIG_DIR}`; hard-errors on `${API_TOKEN}`/`${AGENT_ENV:`); `writeSpecConfigFiles(spec Spec, port int, …) error` writing `<base>/<spec.ID>/<name>` atomically `0600`.

- [ ] **Step 1: Write the failing tests** — `TestWriteSpecConfigFilesExpandsAndWrites` (content `id:"${MODEL}" port:${PORT}` → file at `<base>/<id>/server.json` with substituted values); `TestConfigFileContentRejectsSecrets` (`${API_TOKEN}` → error); `TestTwoSpecsSameFilenameNoCollision` (two IDs → two paths); `TestStartProcessWriteFailureIsStartFailed` (unwritable base dir → `StateStartFailed` + backoff, not `StateNotPermitted`); `TestStartProcessContentExpansionErrorIsNotPermitted` (bad placeholder in content, caught in the pre-port dry run); `TestConfigFileContentChangeForcesRelaunch` (asserting `sameLaunchShape` returns false when only content differs — content is **not** on the strip list).
- [ ] **Step 2: Run to verify they fail** — `cd server-agent && go test ./internal/runtime/ -run 'ConfigFile|WriteSpecConfigFiles|StartProcess.*Config|RelaunchConfig' -v` → FAIL.
- [ ] **Step 3: Implement** — the content expander (shared non-secret branches, no secret spans); `writeSpecConfigFiles` reusing the atomic writer; add content expansion to the pre-port dry run (1753 → `setNotPermitted` on error); insert `writeSpecConfigFiles` after `expandSpec` (1779) and before `cmd` build (1793), routing a write error through the `StateStartFailed`+`recordFailure`+`enterBackoff` block (mirror 1762-1766). Do **not** add `ConfigFiles` to `sameLaunchShape`'s strip list (1035-1068).
- [ ] **Step 4: Run to verify they pass** — same filter → PASS; then the whole `./internal/runtime/...` package.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): write placeholder-expanded config files before launch (#257)"`

---

### Task 8: Feature flag, version bump, file-mode redaction

**Files:**
- Modify: `server-agent/internal/agent/features.go:197`; `server-agent/internal/agent/agent.go:268` (+ history block 240-267); `server-agent/internal/runtime/report.go` (`redactConfigEnv` 108-141, loop 117-138)
- Test: `server-agent/internal/agent/features_test.go` (`TestFeatureRegistry` 76, add `TestFeaturesDeclareRuntimeConfigFiles`); `server-agent/internal/runtime/report_test.go`

**Interfaces:**
- Produces: feature name string `"runtime_config_files"` (the gateway/portal negotiate on it — Task 9 consumes it); redaction that masks each config file's `Content` while keeping `Name`.

- [ ] **Step 1: Write the failing tests** — `TestFeaturesDeclareRuntimeConfigFiles` (name present, `Since == "0.10.0"`); `TestRedactConfigEnvMasksConfigFileContent` (file-mode report keeps names, masks contents).
- [ ] **Step 2: Run to verify they fail** — `cd server-agent && go test ./internal/agent/ ./internal/runtime/ -run 'RuntimeConfigFiles|RedactConfigEnv' -v` → FAIL (and `TestFeatureRegistry` fails until Version is bumped).
- [ ] **Step 3: Implement** — add `{Name: "runtime_config_files", Since: "0.10.0"}`; bump `Version` to `"0.10.0"` with a history paragraph; in `redactConfigEnv` nil-normalize + mask each `ConfigFiles[i].Content = envRedactedMask`.
- [ ] **Step 4: Run to verify they pass** — same filter + `TestFeatureRegistry` → PASS.
- [ ] **Step 5: Commit** — `git commit -m "feat(agent): runtime_config_files flag, v0.10.0, file-mode content redaction (#257)"`

---

### Task 9: Portal — config-files editor, API types, feature warning, i18n, e2e

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeSpec` 38-139, `PutRuntimeSpecRequest` 147-175); `gateway/frontend/src/components/RuntimeAdminSection.tsx` (default state 212, `buildSpecBody` 2885/2913, save sites); `gateway/frontend/src/i18n.ts` (DE ~828-1100, EN ~3985-4103); feature-warning plumbing mirroring an existing gated warning (e.g. `gpu_selection`)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx` (new/extended); `gateway/frontend/src/i18n.test.ts`; a local Playwright spec under `gateway/e2e/`

**Interfaces:**
- Consumes: `config_files` on the DTOs (Task 3); the `runtime_config_files` feature name (Task 8).
- Produces: `interface ConfigFile { name: string; content: string }`; `RuntimeSpec.config_files?: ConfigFile[]` (flows through `PutRuntimeSpecRequest`).

- [ ] **Step 1: Write the failing tests** — a component test: add/remove file rows, name+content bind, client-side validation message (bad basename / secret placeholder), `buildSpecBody` includes `config_files`; a warning shows when the connected agent lacks `runtime_config_files`; i18n parity test stays green with the new keys.
- [ ] **Step 2: Run to verify they fail** — `cd gateway/frontend && npx vitest run src/components/RuntimeAdminSection src/i18n.test.ts` → FAIL.
- [ ] **Step 3: Implement** — the TS types; a repeatable `{name, content}` editor in `RuntimeAdminSection.tsx`; include `config_files` in `buildSpecBody`; DE+EN keys; the feature-gated warning; add a local e2e spec exercising the editor save.
- [ ] **Step 4: Run to verify they pass** — vitest green; `npx prettier --check` on touched files; `npm run build` (type-check); run the new e2e spec locally.
- [ ] **Step 5: Commit** — `git commit -m "feat(portal): runtime-spec config-files editor with DE/EN and agent-feature warning (#257)"`

---

### Task 10: ADR-066 + architecture docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-066); `docs/architecture/cross-cutting/agent-runtime-manager.md`; `docs/architecture/reference/config-env.md`; `docs/architecture/reference/api-surface.md`

**Interfaces:** none (documentation deliverable; the repo rule requires the matching arch doc to change in the same unit of work).

- [ ] **Step 1: Write ADR-066** — record Variant A, the 0..N config-files facet, the agent-owned configurable write dir + per-spec subdir, `${CONFIG_DIR}` + non-secret content expansion, the `runtime_config_files` flag + 0.10.0 bump, and file-mode redaction; reference ADR-061/062 (native relay), ADR-025 (feature negotiation), #257.
- [ ] **Step 2: Update the cross-cutting + reference docs** — the facet and placeholder in `agent-runtime-manager.md`; the `config_files_dir` knob (flag/env/key/default) in `config-env.md`; the `config_files` DTO field in `api-surface.md`.
- [ ] **Step 3: Verify** — `grep` the ADR log for `ADR-066`; confirm no broken relative links; `make` doc checks if any.
- [ ] **Step 4: Commit** — `git commit -m "docs: ADR-066 and architecture docs for agent config-file provisioning (#257)"`

---

## Finishing (not a task — see superpowers:finishing-a-development-branch)

Before the PR: fold any remaining durable content into `docs/architecture/`, then **delete** `docs/superpowers/**` and `docs/implementation-status.md` from the branch; verify with `git diff --name-only main...HEAD` that neither path appears. Run the full gate set (both Go modules' lint, frontend `format:check`/`lint`/`test`/`build`, `make sonar-gate` + branch-findings, store postgres leg). Squash-merge with a substantive commit body (this repo pre-fills the squash description from the commit body).

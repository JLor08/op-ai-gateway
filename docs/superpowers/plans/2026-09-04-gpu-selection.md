# GPU Selection Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator order the GPUs assigned to a server_agent runtime spec (that order flows into the visible-devices env var / `${HOST_GPU_IDS}`), and choose whether `set_visible_devices` is enforced via the environment variable or via llama.cpp `--device` arguments using new `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}` placeholders.

**Architecture:** A new `position` column on `agent_runtime_spec_gpus` makes GPU order explicit (both stores read `ORDER BY position`; the portal captures request array order as `Position = i`); the agent stops sorting in `hostGPUIDs` (keeps dedup) and gains three device placeholders + an `args` mode that skips env injection. A new `visible_devices_mode` (`env`|`args`) rides the spec + agent wire. One `gpu_selection` agent capability flag (ServerAgent 0.4.0) drives portal hints. Entirely additive — no existing field is removed, so the module compiles throughout.

**Tech Stack:** Go 1.26 (`gateway/backend` module `op-ai-gateway`; `server-agent` module `op-ai-server-agent`), React/TypeScript/Vite (`gateway/frontend`), three store drivers (memory / sqlite / postgres via the `dialect` seam). Go tests: stdlib `testing`, table-driven, no testify. Frontend: Vitest + `@testing-library/react`.

## Global Constraints

- **Migration 73** — one function `migration73Up`, name `runtime_spec_gpu_position_and_visible_devices_mode`, adds BOTH `agent_runtime_spec_gpus.position integer not null default 0` (backfilled per spec to the ascending-`gpu_index` rank) AND `agent_runtime_specs.visible_devices_mode text not null default 'env'` (no backfill needed — the default covers upgrades). Append-only: never edit migration 65's create-table or the frozen baseline. Backfill uses the portable correlated-subquery form (like `migration72Up`), never `row_number()`/`UPDATE…FROM`.
- **Canonical Go names:** `routing.RuntimeSpecGPU.Position int`; `routing.VisibleDevicesMode` (`type VisibleDevicesMode string`, consts `VisibleDevicesModeEnv="env"`, `VisibleDevicesModeArgs="args"`, new file `internal/routing/visible_devices_mode.go`); `routing.RuntimeSpec.VisibleDevicesMode VisibleDevicesMode`. Agent-side wire type: `runtime.Spec.VisibleDevicesMode string` (plain string; consts `VisibleDevicesModeEnv="env"`/`VisibleDevicesModeArgs="args"` in `package runtime`); JSON key `visible_devices_mode` everywhere.
- **DTO/wire contract:** `visible_devices_mode` (string) on `RuntimeSpecDTO`, `PutRuntimeSpecRequest`, AND `AgentRuntimeSpecDTO` (the agent NEEDS it — unlike the endpoint modes). GPU order is carried by **array order** only — no `position` key on any DTO/wire type.
- **Device placeholders** (agent, in the `expand` closure): `${CUDA_DEVICES}`→prefix `CUDA`, `${VULKAN_DEVICES}`→prefix `Vulkan`, `${METAL_DEVICES}`→prefix `MTL` (note METAL→`MTL`, not `Metal`). Each emits `<prefix><host_index>,…` in operator (position) order, deduped (first occurrence wins); empty GPU list is the same hard error `${HOST_GPU_IDS}` raises. Shared helpers `gpuIndices(spec) []int` + `deviceList(spec, prefix) string` in `policy_local.go`; `hostGPUIDs` refactors onto `gpuIndices` (drop `sort.Ints`, keep the `sort` import — used elsewhere).
- **Agent capability + version:** append `{Name:"gpu_selection", Since:"0.4.0"}` to `agent.Features`; bump `const Version` (`server-agent/internal/agent/agent.go:77`) `0.3.0`→`0.4.0` (MINOR — one bump for the whole branch; `TestFeatureRegistry` enforces `Since ≤ Version`). The agent gates none of its own behavior on the flag (it always honors what it receives); the flag exists for the portal hint. No `gatewayAgentFeatures` entry needed.
- **New portal error codes (both HTTP 400):** `runtime_spec.visible_devices_mode_invalid`, `runtime_spec.visible_devices_args_no_placeholder`. Sentinels `ErrRuntimeSpecVisibleDevicesModeInvalid` / `ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder`.
- **Args-mode validation:** when `set_visible_devices` on and mode `args`, `req.Args` must contain at least one of the three device placeholder tokens, else `…ArgsNoPlaceholder` (400). The env-conflict trap (a hand-set `CUDA_VISIBLE_DEVICES` etc. in `env`) stays active in BOTH modes (agent + gateway). The mode value is validated always (a malformed enum is a malformed request); `""` is valid and defaults to `env`.
- **Agent host OS is already in the frontend** as `HardwareReport.os` (fetched at `RuntimeAdminSection.tsx:1394`; value is a gopsutil string like `"darwin 15.1"`). **Do NOT add `agent_os` to any backend DTO.** The Metal-on-non-macOS hint uses `hardware.data?.report?.os` with a case-insensitive substring test `/darwin|mac ?os/i`.
- **`hostGPUIDs` order change flips 4 existing agent tests** — they must be updated to the order-preserving contract in the SAME commit as the impl (listed in Task 6). Ascending-declaration specs elsewhere are unchanged.
- **Frontend:** `RuntimeSpec.visible_devices_mode: 'env' | 'args'` (`api/runtime.ts`); reorder via the house pattern (`components/shared/columnDrag.ts` `moveColumn`/`useColumnDrag('vertical')`/`columnDragSx` + the `OrderedMemberList` swap/arrow pattern), reusing i18n `modelGroupMoveUp`/`modelGroupMoveDown`; the mode is a `SelectField`. New i18n keys go in BOTH `de` and `en` (compile-enforced).
- **New source files** start with the header `// SPDX-License-Identifier: AGPL-3.0-only` / `// Copyright (C) 2026 OnPrem AI Gateway contributors`.
- **Additive, no build break:** every new field/column/flag is additive; the whole module builds after every task. Per-task gate is the package's own tests; the whole-module `make test-go` and `npm run build` stay green throughout.

## Detailed per-task code (task material)

Full current-code excerpts (with `file:line`) and complete failing-test + implementation code live alongside this plan in **`docs/superpowers/plan-material/`**, verified against the branch source:

| Bundle | Covers plan tasks |
|---|---|
| `01-backend-store-migration.md` | Task 1 |
| `02-backend-portal.md` | Tasks 2–5 |
| `03-agent.md` | Tasks 6–9 |
| `04-frontend.md` | Tasks 10–14 |

Where a step says "verbatim from `0X-*.md`", read that bundle section for the full code. These plan-material files are branch-local and are removed by the final cleanup task with the rest of `docs/superpowers/`. If a bundle conflicts with these Global Constraints, the plan wins — notably: **the AgentOS backend DTO in `02-backend-portal.md` Task B5 is DROPPED** (the frontend uses `HardwareReport.os` instead — see `04-frontend.md` §0 decision 1).

## File Structure

**Backend — created:** `internal/routing/visible_devices_mode.go`; `internal/store/migration73_gpu_selection_test.go` (optional focused migration test).
**Backend — modified:** `internal/routing/store.go` (RuntimeSpecGPU.Position, RuntimeSpec.VisibleDevicesMode, interface docs), `memory_store.go` (GPU sort by Position), `internal/store/migrate.go` (migration73Up), `sqlite_runtime.go` (GPU CRUD position + spec-row visible_devices_mode CRUD), `routing_store_conformance_test.go` + `conformance_test.go` (round-trips), `internal/portal/service_runtime.go` (type consumption, 3 DTOs, putRuntimeSpec, builders, validation, sentinels), `internal/gateway/portal_runtime_endpoints.go` (HTTP 400 rows).
**Agent — modified:** `server-agent/internal/runtime/policy_local.go` (gpuIndices/hostGPUIDs/deviceList, 3 placeholder cases, env-injection guard), `types.go` (Spec.VisibleDevicesMode + consts), `policy_local_test.go`/`command_test.go` (order-contract tests), `server-agent/internal/agent/features.go` (flag), `agent.go` (Version), `features_test.go` (pin).
**Frontend — modified:** `src/api/runtime.ts` (visible_devices_mode), `src/components/RuntimeAdminSection.tsx` (state, reorder, mode select, hints), `src/i18n.ts` (keys), `src/components/RuntimeAdminSection.test.tsx` (fixtures + tests).
**Docs — modified:** `docs/architecture/cross-cutting/agent-runtime-manager.md`, `docs/architecture/reference/api-surface.md` + data-model, ADR log.

---

## Task 1: GPU order persistence (position column, both drivers, migration 73)

**Files:** Modify `internal/routing/store.go` (RuntimeSpecGPU `:1300`, interface docs `:1403`), `internal/routing/memory_store.go` (`RuntimeSpecGPUs` `:2360`), `internal/store/migrate.go` (register `{version:73,…}` at `:105`, add `migration73Up`), `internal/store/sqlite_runtime.go` (GPU INSERT `:161`, SELECT `:179`), `internal/store/routing_store_conformance_test.go` (`:641-659`).

**Interfaces:**
- Produces: `routing.RuntimeSpecGPU.Position int`; DB column `agent_runtime_spec_gpus.position`; both stores return GPU rows `ORDER BY position` (sqlite `order by position, gpu_index`; memory `sort.Slice` Position→GPUIndex); `SetRuntimeSpecGPUs` persists caller `Position` verbatim (no renumber); `migration73Up` (also adds `agent_runtime_specs.visible_devices_mode text not null default 'env'` — the column Task 2 reads/writes).
- Consumes: nothing (additive).

- [ ] **Step 1:** Add `Position int` to `RuntimeSpecGPU` and refresh interface + memory doc comments (verbatim from `01-backend-store-migration.md` Task S1). Confirm `go build ./...` still builds.
- [ ] **Step 2 (RED):** Replace the conformance GPU block (`routing_store_conformance_test.go:641-659`) with the non-ascending-order round-trip from `01-…md` Task S2 (writes gpu_index 1 at position 0, asserts read order `[gpu1(pos0), gpu0(pos1)]` and the measured-write keys on gpu_index). Run `cd gateway/backend && go test ./internal/store/ -run TestRoutingStoreRuntimeSpecs -v` → FAIL (both stores still sort by gpu_index).
- [ ] **Step 3 (impl):** Add `migration73Up` (both columns + the correlated-subquery position backfill) verbatim from `01-…md` Task S3; update the sqlite GPU INSERT + SELECT (`order by position, gpu_index`, scan `&g.Position`) from Task S4; update the memory `RuntimeSpecGPUs` sort (Position then GPUIndex tiebreak) from Task S5.
- [ ] **Step 4 (GREEN):** `cd gateway/backend && go test ./internal/store/... ./internal/routing/... -count=1` → PASS. Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set → PASS (the migration runs on every `forEachDialect` DB).
- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/routing/memory_store.go gateway/backend/internal/store/migrate.go gateway/backend/internal/store/sqlite_runtime.go gateway/backend/internal/store/routing_store_conformance_test.go
git commit -m "feat(store): explicit GPU order (position column) + migration 73"
```

---

## Task 2: `VisibleDevicesMode` type, RuntimeSpec field, spec-row CRUD, DTOs, builders

**Files:** Create `internal/routing/visible_devices_mode.go`; modify `internal/routing/store.go` (RuntimeSpec `:1281`), `internal/store/sqlite_runtime.go` (`runtimeSpecCols` `:68`, `runtimeSpecColsPrefixed` `:78`, `UpsertRuntimeSpec` `:26-52`, `scanRuntimeSpec` `:343`), `internal/portal/service_runtime.go` (RuntimeSpecDTO `:348`, PutRuntimeSpecRequest `:381`, AgentRuntimeSpecDTO `:1316`, `GetRuntimeSpec` `:400`, `putRuntimeSpec` `:508`/`:591`, `runtimeSpecDTO` `:785`, `agentRuntimeSpecDTO` `:1585`), `internal/store/conformance_test.go` (`:7681-7704`).

**Interfaces:**
- Consumes: migration 73's `visible_devices_mode` column (Task 1).
- Produces: `routing.VisibleDevicesMode` + consts; `RuntimeSpec.VisibleDevicesMode`; the `visible_devices_mode` json key on all three DTOs; the field round-trips through the API + agent wire.

- [ ] **Step 1 (RED):** add `TestRuntimeSpecDTOCarriesVisibleDevicesMode` (verbatim from `02-backend-portal.md` Task B1) — args-mode round-trip via PutRuntimeSpec/GetRuntimeSpec, agent-wire carries the mode, omitted → `env`. Also extend the store conformance RuntimeSpec block (`conformance_test.go:7681-7704`) with `VisibleDevicesMode: routing.VisibleDevicesModeArgs` seed + assertion (Task B0). Run `cd gateway/backend && go test ./internal/portal/... ./internal/store/... -run 'VisibleDevicesMode|RuntimeSpec'` → FAIL (field/type absent).
- [ ] **Step 2 (impl):** create `visible_devices_mode.go` (type + consts, verbatim from `02-…md` Task B0); add `RuntimeSpec.VisibleDevicesMode` field; add the column to the sqlite spec CRUD (cols const, prefixed const, INSERT list + one `?`, on-conflict, exec arg `string(spec.VisibleDevicesMode)`, `scanRuntimeSpec` `&spec.VisibleDevicesMode` — all right after `messages_mode`/`&spec.MessagesMode`, from `02-…md` §1.14); add the field to the three DTOs + `GetRuntimeSpec` default (`string(routing.VisibleDevicesModeEnv)`); in `putRuntimeSpec` resolve the mode (default env) and set it in the spec literal; emit it in `runtimeSpecDTO` + `agentRuntimeSpecDTO` (all from Task B1).
- [ ] **Step 3 (GREEN):** `cd gateway/backend && go test ./internal/portal/... ./internal/store/... ./internal/routing/... -count=1` → PASS (memory + sqlite; postgres via DSN). Confirm `TestAgentRuntimeConfigOmitsFlavorsAndModes` still passes (`visible_devices_mode` doesn't collide with the guarded substrings).
- [ ] **Step 4: Commit**

```bash
git add gateway/backend/internal/routing/visible_devices_mode.go gateway/backend/internal/routing/store.go gateway/backend/internal/store/sqlite_runtime.go gateway/backend/internal/store/conformance_test.go gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): visible_devices_mode (env|args) on the spec + agent wire"
```

---

## Task 3: capture GPU array order in `putRuntimeSpec` (Position = i)

**Files:** Modify `internal/portal/service_runtime.go` (GPU loop `:605-613`), test `service_runtime_test.go`.

**Interfaces:** Consumes Task 1 (`Position` field + `ORDER BY position`) and Task 2. Produces: the request `gpus` array order becomes the stored/response/agent-wire order.

- [ ] **Step 1 (RED):** add `TestPutRuntimeSpecPreservesGPUArrayOrder` (verbatim from `02-…md` Task B4) — PUT gpus `[5,2,3]`, assert response + GetRuntimeSpec + agent-wire order all `[5,2,3]`. Run `cd gateway/backend && go test ./internal/portal/... -run TestPutRuntimeSpecPreservesGPUArrayOrder` → FAIL (order comes back `2,3,5`).
- [ ] **Step 2 (impl):** change the GPU loop to `for i, g := range req.GPUs` and set `Position: i` (from Task B4).
- [ ] **Step 3 (GREEN):** rerun → PASS.
- [ ] **Step 4: Commit**

```bash
git add gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): store GPU rows in the request array order (Position = i)"
```

---

## Task 4: mode + args-placeholder validation

**Files:** Modify `internal/portal/service_runtime.go` (sentinels `:75`, `runtimeSpecDevicePlaceholders`/`argsHaveDevicePlaceholder`/`validVisibleDevicesMode` near `:672`, `validateRuntimeSpecVisibleDevices` `:699-716`), test `service_runtime_test.go`.

**Interfaces:** Produces `ErrRuntimeSpecVisibleDevicesModeInvalid` (`runtime_spec.visible_devices_mode_invalid`), `ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder` (`runtime_spec.visible_devices_args_no_placeholder`), and the three `runtimeSpecDevicePlaceholders` tokens (must match the agent's exactly).

- [ ] **Step 1 (RED):** add `TestPutRuntimeSpecVisibleDevicesModeValidation` (verbatim from `02-…md` Task B2) — bad mode → mode_invalid; args mode with no placeholder → args_no_placeholder; each of the three tokens accepted; conflict trap holds in args mode; args-no-placeholder OK when the flag is off. Run → FAIL (sentinels undefined).
- [ ] **Step 2 (impl):** add the two sentinels, `runtimeSpecDevicePlaceholders`/`argsHaveDevicePlaceholder`/`validVisibleDevicesMode`, and extend `validateRuntimeSpecVisibleDevices` (validate the mode always; in args mode require a device placeholder; keep the conflict + no-gpus traps) — all from Task B2.
- [ ] **Step 3 (GREEN):** `cd gateway/backend && go test ./internal/portal/... -count=1` → PASS.
- [ ] **Step 4: Commit**

```bash
git add gateway/backend/internal/portal/service_runtime.go
git commit -m "feat(portal): validate visible_devices_mode + require a device placeholder in args mode"
```

---

## Task 5: HTTP 400 mapping for the two new sentinels

**Files:** Modify `internal/gateway/portal_runtime_endpoints.go` (`portalRuntimeSpecErrRows` `:68-86`), test `portal_runtime_endpoints_test.go`.

- [ ] **Step 1 (RED):** add `TestHandlePortalMappingRuntimeSpecPutBadVisibleDevicesModeReturns400` + `…ArgsModeNoPlaceholderReturns400` (verbatim from `02-…md` Task B3). Run `cd gateway/backend && go test ./internal/gateway/... -run TestHandlePortalMappingRuntimeSpecPut` → FAIL (falls through to 500).
- [ ] **Step 2 (impl):** add the two `errRow` entries (from Task B3).
- [ ] **Step 3 (GREEN):** rerun → PASS, then `make test-go` (whole backend) → PASS.
- [ ] **Step 4: Commit**

```bash
git add gateway/backend/internal/gateway/portal_runtime_endpoints.go
git commit -m "feat(gateway): map visible_devices_mode validation errors to HTTP 400"
```

---

## Task 6: agent honors GPU array order (hostGPUIDs) + flip the 4 old-contract tests

**Files:** Modify `server-agent/internal/runtime/policy_local.go` (`hostGPUIDs` `:285-304`, add `gpuIndices`), `policy_local_test.go` (`visibleDevicesSpec` `:1331`, `TestExpandPlaceholdersSetsVisibleDevicesPerVendor`, `…ConflictRefused`, `TestExpandPlaceholdersHostGPUIDs`), `command_test.go` (`TestResolvedCommandReportsPlaceholdersExpanded`).

**Interfaces:** Produces `gpuIndices(spec) []int` (ordered, deduped) and an order-preserving `hostGPUIDs`.

> The 4 existing tests encode the OLD ascending contract with non-ascending declarations and MUST flip in this commit (exact string edits `"2,5"→"5,2"`, `"level_zero:2,5"→"level_zero:5,2"`, `"0,4,7"→"7,0,4"`, `"2,3"→"3,2"`, `"level_zero:2,3"→"level_zero:3,2"`, and the "ascending" comments). `TestExpandPlaceholdersHostGPUIDsAreHostIndices` (`[4,6]`) and the ascending manager/command specs do NOT change.

- [ ] **Step 1 (RED):** add `TestHostGPUIDsPreservesOrderAndDedups` (verbatim from `03-agent.md` Task A1) AND make the 4 string edits above. Run `cd server-agent && go test ./internal/runtime/ -run 'TestHostGPUIDs|TestExpandPlaceholdersHostGPUIDs|TestExpandPlaceholdersSetsVisibleDevices|TestResolvedCommandReportsPlaceholdersExpanded' -count=1` → FAIL (impl still sorts).
- [ ] **Step 2 (impl):** add `gpuIndices`, refactor `hostGPUIDs` onto it (drop `sort.Ints`, KEEP the `sort` import), rewrite the doc comment (from Task A1).
- [ ] **Step 3 (GREEN):** `cd server-agent && go test ./internal/runtime/... -count=1` → PASS.
- [ ] **Step 4: Commit**

```bash
git add server-agent/internal/runtime/policy_local.go server-agent/internal/runtime/policy_local_test.go server-agent/internal/runtime/command_test.go
git commit -m "feat(agent): honor the operator GPU order in hostGPUIDs (keep dedup)"
```

---

## Task 7: three device placeholders `${CUDA_DEVICES}`/`${VULKAN_DEVICES}`/`${METAL_DEVICES}`

**Files:** Modify `server-agent/internal/runtime/policy_local.go` (add `deviceList`, three cases after `:864`), test `policy_local_test.go`.

**Interfaces:** Produces `deviceList(spec, prefix) string` and the three placeholder tokens (prefixes `CUDA`/`Vulkan`/`MTL`).

- [ ] **Step 1 (RED):** add `TestExpandPlaceholdersDeviceLists` (verbatim from `03-…md` Task A2) — each token → `<prefix><idx>` in operator order; no-gpus refused; dedup; near-miss passthrough. Run `cd server-agent && go test ./internal/runtime/ -run TestExpandPlaceholdersDeviceLists -count=1` → FAIL (tokens pass through literally).
- [ ] **Step 2 (impl):** add `deviceList` + the three `if inner == "…_DEVICES"` cases (reusing `gpuIDs == ""` for the refusal) from Task A2.
- [ ] **Step 3 (GREEN):** rerun → PASS.
- [ ] **Step 4: Commit**

```bash
git add server-agent/internal/runtime/policy_local.go server-agent/internal/runtime/policy_local_test.go
git commit -m "feat(agent): CUDA/Vulkan/Metal device placeholders for llama.cpp --device"
```

---

## Task 8: `Spec.VisibleDevicesMode` wire field + args-mode env-injection skip

**Files:** Modify `server-agent/internal/runtime/types.go` (Spec `:61`, consts), `policy_local.go` (env-injection block `:1006-1012`), test `policy_local_test.go`.

**Interfaces:** Produces `runtime.Spec.VisibleDevicesMode string` (json `visible_devices_mode`) + consts `VisibleDevicesModeEnv`/`VisibleDevicesModeArgs`; args mode injects no visibility env var; empty/unknown == env.

- [ ] **Step 1 (RED):** add `TestExpandPlaceholdersVisibleDevicesArgsModeSkipsEnvInjection` + `TestExpandPlaceholdersVisibleDevicesConflictHoldsInBothModes` (verbatim from `03-…md` Task A3). Run → compile FAIL (field/consts absent).
- [ ] **Step 2 (impl):** add the `Spec.VisibleDevicesMode` field + the two consts (types.go); add `&& spec.VisibleDevicesMode != VisibleDevicesModeArgs` to the env-injection guard (policy_local.go). The conflict trap at `:971-980` is unchanged (already guarded on `SetVisibleDevices` only). From Task A3.
- [ ] **Step 3 (GREEN):** `cd server-agent && go test ./internal/runtime/... -count=1` → PASS.
- [ ] **Step 4: Commit**

```bash
git add server-agent/internal/runtime/types.go server-agent/internal/runtime/policy_local.go server-agent/internal/runtime/policy_local_test.go
git commit -m "feat(agent): visible_devices_mode=args skips env injection (device placeholder in args)"
```

---

## Task 9: `gpu_selection` capability flag + ServerAgent 0.4.0

**Files:** Modify `server-agent/internal/agent/features.go` (Features `:89`), `agent.go` (Version `:77`), test `features_test.go`.

**Interfaces:** Produces feature name `gpu_selection` (Since `0.4.0`), `const Version = "0.4.0"`.

- [ ] **Step 1 (RED):** add `TestGPUSelectionFeatureIsDeclared` (verbatim from `03-…md` Task A4). Run `cd server-agent && go test ./internal/agent/ -run 'TestGPUSelectionFeatureIsDeclared|TestFeatureRegistry' -count=1` → FAIL (feature absent; and once added at Since 0.4.0 with Version 0.3.0, `TestFeatureRegistry` fails its `Since ≤ Version` guard).
- [ ] **Step 2 (impl):** append `{Name:"gpu_selection", Since:"0.4.0"}` to `Features`; bump `const Version` to `"0.4.0"` and extend its comment block (from Task A4).
- [ ] **Step 3 (GREEN):** `cd server-agent && go test ./internal/agent/... ./internal/runtime/... -count=1` → PASS. Then whole server-agent: `cd server-agent && go test ./... -count=1` → PASS.
- [ ] **Step 4: Commit**

```bash
git add server-agent/internal/agent/features.go server-agent/internal/agent/agent.go server-agent/internal/agent/features_test.go
git commit -m "feat(agent): declare gpu_selection capability, bump ServerAgent to 0.4.0"
```

---

## Task 10: frontend TS type + state wiring (`visible_devices_mode`, default env)

**Files:** Modify `src/api/runtime.ts` (RuntimeSpec `:57`), `src/components/RuntimeAdminSection.tsx` (emptySpec `:195`, state `:2046`, buildSpecBody `:2392`, hydrateSpecFields `:2143`, resetSpecFields `:2124`), test `RuntimeAdminSection.test.tsx` (`makeSpec` `:152`).

**Interfaces:** Produces `RuntimeSpec.visible_devices_mode: 'env' | 'args'` and its form state, default `'env'`.

- [ ] **Step 1 (RED):** add `visible_devices_mode: 'env'` to `makeSpec`; add the "defaults visible_devices_mode to env in the spec PUT body" test (verbatim from `04-frontend.md` Task 1). Run `cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx` → FAIL (field not emitted).
- [ ] **Step 2 (impl):** add the type field (runtime.ts); add `visibleDevicesMode` state + wire into `emptySpec`/`buildSpecBody`/`hydrateSpecFields`/`resetSpecFields` (from Task 1).
- [ ] **Step 3 (GREEN):** rerun the file → PASS; `npx tsc --noEmit` clean.
- [ ] **Step 4: Commit**

```bash
git add gateway/frontend/src/api/runtime.ts gateway/frontend/src/components/RuntimeAdminSection.tsx gateway/frontend/src/components/RuntimeAdminSection.test.tsx
git commit -m "feat(portal-ui): visible_devices_mode state (default env) round-trips"
```

---

## Task 11: GPU-row reorder (drag + up/down)

**Files:** Modify `src/components/RuntimeAdminSection.tsx` (icon + columnDrag imports, `reorderGpuRows`/`swapGpuRow` near `:2230`, `useColumnDrag` hook, GPU rows map `:3588`), test `RuntimeAdminSection.test.tsx`.

**Interfaces:** Consumes `columnDrag` helpers + `modelGroupMoveUp/Down` i18n. Produces the reordered `gpuRows` → PUT `gpus` order.

- [ ] **Step 1 (RED):** add the "reorders GPU rows (move down) and sends the new gpus order" test (verbatim from `04-…md` Task 2) — opens edit, clicks `${t.modelGroupMoveDown}: GPU 0`, asserts PUT `gpus` order `[1,0,2]`. Run → FAIL (no move button).
- [ ] **Step 2 (impl):** add the icon + `columnDrag` imports, `reorderGpuRows`/`swapGpuRow`, the `gpuDrag = useColumnDrag(reorderGpuRows, 'vertical')` hook, and the drag handle + up/down arrows on each GPU row `<Box>` (from Task 2). Keep `duplicateGpuIndex` submit check.
- [ ] **Step 3 (GREEN):** rerun → PASS; `npx tsc --noEmit` clean.
- [ ] **Step 4: Commit**

```bash
git add gateway/frontend/src/components/RuntimeAdminSection.tsx gateway/frontend/src/components/RuntimeAdminSection.test.tsx
git commit -m "feat(portal-ui): reorder GPU rows (drag + up/down arrows)"
```

---

## Task 12: i18n keys (de + en)

**Files:** Modify `src/i18n.ts` (de `:596`/`:627`, en `:2677`/`:2700`).

**Interfaces:** Produces `runtimeSpecVisibleDevicesMode`, `runtimeSpecVisibleDevicesModeEnv`, `runtimeSpecVisibleDevicesModeArgs`, `runtimeSpecVisibleDevicesModeArgsHint`, `runtimeSpecAgentTooOldArgs`, `runtimeSpecAgentTooOldOrder`, `runtimeSpecMetalNonMacos` (both locales).

- [ ] **Step 1 (impl):** add the seven keys to BOTH `de` and `en` (verbatim strings from `04-…md` Task 5).
- [ ] **Step 2 (GREEN):** `cd gateway/frontend && npm run build` → tsc clean (missing/excess key in either locale is a compile error). Run the i18n test if present.
- [ ] **Step 3: Commit**

```bash
git add gateway/frontend/src/i18n.ts
git commit -m "i18n(portal): visibility-mode + GPU-selection hint keys (de+en)"
```

---

## Task 13: `visible_devices_mode` SelectField (shown when the checkbox is on)

**Files:** Modify `src/components/RuntimeAdminSection.tsx` (below the set_visible_devices hint `:3558`), test `RuntimeAdminSection.test.tsx`.

**Interfaces:** Consumes the Task 12 i18n keys + Task 10 state.

- [ ] **Step 1 (RED):** add the two tests from `04-…md` Task 3 (control shows when checkbox on + round-trips `args`; hidden when off). Run → FAIL (combobox not found).
- [ ] **Step 2 (impl):** insert the `SelectField` (env/args options) + the args-mode hint caption, gated on `setVisibleDevices` (from Task 3). `SelectField` is already imported.
- [ ] **Step 3 (GREEN):** rerun → PASS; `npx tsc --noEmit` clean.
- [ ] **Step 4: Commit**

```bash
git add gateway/frontend/src/components/RuntimeAdminSection.tsx gateway/frontend/src/components/RuntimeAdminSection.test.tsx
git commit -m "feat(portal-ui): env/args visibility-mode select in the runtime-spec form"
```

---

## Task 14: portal hints (agent too old; Metal on non-macOS)

**Files:** Modify `src/components/RuntimeAdminSection.tsx` (derived values in the body, `<Alert>` blocks in the GPU section), test `RuntimeAdminSection.test.tsx` (add the `os` param to `makeHardware`).

**Interfaces:** Consumes agent feature `'gpu_selection'` (via `agent_features`), `HardwareReport.os` (already fetched), the Task 12 keys.

- [ ] **Step 1 (RED):** extend `makeHardware(gpus, os='linux')`; add the three tests from `04-…md` Task 4 (args-mode + old agent → prominent warning; `${METAL_DEVICES}` + non-macOS → hint; macOS agent → no Metal hint). Run → FAIL (hints absent).
- [ ] **Step 2 (impl):** add the derived values (`agentHasGpuSelection`, `gpuOrderIsCustom`, `argsHaveMetalDevices`, `agentOs`, `isMacOsAgent` via `/darwin|mac ?os/i`, `showAgentTooOldArgs`/`showAgentTooOldOrder`/`showMetalNonMacos`) and the three `<Alert>` blocks (from Task 4). Hints never block Save.
- [ ] **Step 3 (GREEN):** `cd gateway/frontend && npm test` (whole suite) → PASS; `npm run build` → tsc clean; `npm run lint` → 0 errors.
- [ ] **Step 4: Commit**

```bash
git add gateway/frontend/src/components/RuntimeAdminSection.tsx gateway/frontend/src/components/RuntimeAdminSection.test.tsx
git commit -m "feat(portal-ui): hints for an old agent and for ${METAL_DEVICES} on non-macOS"
```

---

## Task 15: Architecture docs + ADR

**Files:** `docs/architecture/cross-cutting/agent-runtime-manager.md`, `docs/architecture/reference/api-surface.md` + data-model, `docs/architecture/09-architecture-decisions.md`.

- [ ] **Step 1:** In `agent-runtime-manager.md`: update §3.2 placeholder catalog ("four" → seven, adding the three `${…_DEVICES}` with the operator-order + dedup + backend-numbering caveat: Vulkan/Metal enumerate independently, verify with `--list-devices`; Metal needs a multi-device build and only works on macOS). Update §3.3 `set_visible_devices` for the env/args mode + the args-placeholder requirement + the order-preserving change and the backfill safety property. Add the `gpu_selection` flag to the features/capability section.
- [ ] **Step 2:** `api-surface.md`: the `visible_devices_mode` DTO field + the two new 400 error codes; data-model: the `agent_runtime_spec_gpus.position` + `agent_runtime_specs.visible_devices_mode` columns + migration 73. Add a short ADR ("GPU order is explicit via a position column; set_visible_devices has an env/args mode; args mode uses CUDA/Vulkan/Metal device placeholders; one gpu_selection agent flag; ServerAgent 0.4.0").
- [ ] **Step 3:** `make lint-docs` (repo root) → OK.
- [ ] **Step 4: Commit**

```bash
git add docs/architecture/
git commit -m "docs(arch): GPU order + env/args visibility mode, device placeholders, gpu_selection flag"
```

---

## Task 16: Full verification + quality gate

- [ ] **Step 1:** `make test-go` (repo root — backend + server-agent). With Docker/Postgres available, run `OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./internal/store/... ./internal/routing/...` from `gateway/backend`. Expect PASS on all drivers.
- [ ] **Step 2:** `cd gateway/frontend && npm test && npm run build && npm run lint` → PASS.
- [ ] **Step 3:** `make lint` (Go + docs) → PASS.
- [ ] **Step 4:** Confirm the ServerAgent version rule: `server-agent/internal/agent/agent.go` `const Version` = `"0.4.0"`, changed exactly once; `git diff main...HEAD` shows the single bump.
- [ ] **Step 5:** If the environment allows, run the SonarQube local gate (`make sonar-up` once, then `make sonar-gate`, `make sonar-findings`, `make sonar-branch-findings`) and fix any finding on lines this branch changed; note in the PR if Docker/DSN was unavailable.

---

## Task 17: Branch cleanup + PR

- [ ] **Step 1:** `git rm -r docs/superpowers` (fold anything durable into `docs/architecture/` first — done in Task 15). Verify `git diff --name-only main...HEAD` shows no `docs/superpowers/**`.
- [ ] **Step 2:** Commit the removal, push `gpu-selection`, open a PR against `main` (squash-merge; human reviewer merges — never merge yourself).

```bash
git rm -r docs/superpowers
git commit -m "chore: remove branch-local working files before PR"
git push -u origin gpu-selection
```

---

## Sequencing summary

Backend **1 → 2 → 3 → 4 → 5** (Task 3 depends on Task 1's `Position` + Task 2; each keeps the module green — this feature is additive, no cross-package break). Agent **6 → 7 → 8 → 9** (independent of the gateway; Task 6 must flip the 4 old-contract tests in its own commit). Frontend **10 → 11 → 12 → 13 → 14** (Task 12 i18n before 13/14 so tsc stays green). Backend, agent, and frontend tracks are mutually independent and may interleave. **15** docs any time after the code; **16** full verification after all code + docs; **17** last.

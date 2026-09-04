# Runtime-Spec GPU Selection: Ordering + Visibility Mechanism — Design

Date: 2026-09-04
Branch: `gpu-selection`
Status: design approved (brainstorming), pending plan

## 1. Problem & Goal

Two related capabilities for a `server_agent` runtime spec's assigned GPUs:

- **A — Ordering.** Let an operator arrange the assigned GPUs into an explicit
  order (drag or move up/down), and have that order appear, in that sequence, in
  the GPU-visibility environment variable (and the shared `${HOST_GPU_IDS}`
  placeholder). Today the list is always sorted ascending by GPU index and any
  chosen order is discarded.
- **B — Visibility mechanism.** For "Enforce GPU visibility"
  (`set_visible_devices`), let the operator choose whether enforcement happens via
  the **environment variable** (today's mechanism) or via **command-line
  arguments** (llama.cpp `--device`). In args mode the operator writes a
  backend-specific device placeholder into the spec's `args`; the agent expands
  it, and the gateway checks that at least one such placeholder is actually
  present.

Both are gateway+agent changes (unlike the last feature). They ship together and
share one new agent capability flag and one ServerAgent version bump.

## 2. Terminology & Current State (as of `main` 2819099)

- **GPU rows.** `routing.RuntimeSpecGPU{SpecID, GPUIndex, VRAMEstimateMB,
  VRAMMeasuredMB}` (`internal/routing/store.go:1300`). Table
  `agent_runtime_spec_gpus(spec_id, gpu_index, vram_estimate_mb, vram_measured_mb,
  primary key (spec_id, gpu_index))` (`internal/store/migrate.go:2879`). **No
  order column** — every read is `ORDER BY gpu_index`
  (`internal/store/sqlite_runtime.go:182`; memory `sort.Slice … GPUIndex`
  `internal/routing/memory_store.go:2364`). `putRuntimeSpec` persists the request
  rows then re-reads sorted, so any request order is discarded
  (`internal/portal/service_runtime.go` row build ~605-613, `SetRuntimeSpecGPUs`
  ~614, re-read ~618).
- **`set_visible_devices`.** `bool` on `routing.RuntimeSpec`
  (`store.go:1268-1281`), the DTOs, and the agent wire spec
  (`server-agent/internal/runtime/types.go:54-61`). Persisted via migration 69.
  When on, the agent sets the vendor-appropriate visibility var; when off, `gpus`
  is a declaration only.
- **Env-var construction (agent).** `hostGPUIDs(spec)`
  (`server-agent/internal/runtime/policy_local.go:285-304`): dedup + `sort.Ints`
  → ascending, comma-joined host indices. `VisibleDevicesVar(vendor)`
  (`policy_local.go:212-223`): NVIDIA→`CUDA_VISIBLE_DEVICES`,
  AMD→`ROCR_VISIBLE_DEVICES`, Apple/None→`""`. The env var is appended in
  `expandSpec` at `policy_local.go:1006-1012`; a hand-set visibility var in `env`
  is refused (conflict trap, `policy_local.go:971-980`).
- **Placeholders (agent).** Exactly four, expanded by one `expand` closure over
  BOTH `args` and `env` (`policy_local.go`): `${PORT}`, `${MODEL}`,
  `${HOST_GPU_IDS}` (= `hostGPUIDs`, shares the sorted value), `${AGENT_ENV:NAME}`.
  Regex `\$\{[^}]*\}` (`policy_local.go:543`), manual index-walk substitution;
  unknown `${…}` tokens pass through literally; near-misses on `PORT`/`AGENT_ENV`
  prefixes error. Args are `[]string` run through the same closure
  (`policy_local.go:924-931`); env at `:1013-1029`.
- **Gateway validation.** `validateRuntimeSpecVisibleDevices(req)`
  (`internal/portal/service_runtime.go:699-716`): rejects a hand-set visibility
  var in `req.Env` (`ErrRuntimeSpecVisibleDevicesConflict`) and empty `req.GPUs`
  (`ErrRuntimeSpecVisibleDevicesNoGPUs`). `req.Args` is a parsed `[]string` here.
  Error sentinels near `service_runtime.go:34-50`; HTTP 400 mapping in
  `internal/gateway/portal_runtime_endpoints.go:76-77`. Args *content* is
  deliberately not validated (placeholders resolved at launch).
- **Agent capability negotiation.** `agent.Features` (`server-agent/internal/
  agent/features.go`, entries `{Name, Since}`, guard `TestFeatureRegistry`);
  `const Version` (`server-agent/internal/agent/agent.go:77`, currently `0.3.0`).
  Features ride on telemetry `capabilities.features` (auto-includes every registry
  name). Gateway ingests into `agentFeaturesRegistry` (`internal/gateway/
  runtime_registry.go`, `.Has(serverID, feature)`) and persists
  `ServerTelemetry.Capabilities`. Portal already exposes
  `ServerRuntimeReportViewDTO.{AgentVersion, AgentFeatures}`
  (`internal/portal/service_runtime.go:1640-1706`), the frontend `RuntimeReport`
  carries them (`gateway/frontend/src/api/runtime.ts:305-311`), and
  `RuntimeAdminSection.tsx` already derives `agentFeatures`/`agentVersion`
  (~:1703-1704) with a `featureMismatch` banner pattern (~:1723-1724, 3785-3793).
- **Frontend GPU rows.** `RuntimeAdminSection.tsx`: `GpuRow{rowKey,index,
  vramEstimateMb,vramMeasuredMb}` (~:150-155), `gpuRows` state (~:2047), stable
  `rowKey` via `makeRowKey` (~:144-148), `addGpuRow` appends (~:2208), no reorder
  control; JSX block ~:3572-3691; `buildSpecBody` sends `gpus` in array order
  (~:2393-2397); `hydrateSpecFields` loads `spec.gpus` positionally (~:2144-2151);
  `set_visible_devices` checkbox ~:3547-3558. TS `RuntimeSpecGPU`
  (`runtime.ts:21-25`).
- **House reorder pattern (reuse).** `components/shared/columnDrag.ts`
  (`moveColumn`, `useColumnDrag('vertical')`, `columnDragSx`) +
  `components/shared/OrderedMemberList.tsx` (drag **and** up/down `swap` +
  `ArrowUpward/DownwardIcon`, i18n `modelGroupMoveUp/Down`).
- **llama.cpp `--device` (research 2026-09-04).** `--device`/`-dev` takes a
  comma-separated list of backend-prefixed device names `<Backend><LocalIndex>`,
  0-based per backend, resolved by exact name. CUDA→`CUDA0,CUDA1,…` (with
  `CUDA_VISIBLE_DEVICES` unset, `CUDA2` = host GPU #2); Vulkan→`Vulkan0,…`;
  Metal→`MTL0,MTL1,…` (upstream hard-codes one device, but a custom multi-device
  Metal build exposes several `MTL<n>`). **Order in `--device` is honored**
  (device order drives layer assignment, `--tensor-split` position, `--main-gpu`
  index). Caveat: Vulkan and Metal enumerate independently of CUDA/host order, so
  `Vulkan2`/`MTL2` are not guaranteed to be the same physical card as host index 2
  — verify with `--list-devices`. `--device` since PR #10497 (2024-11-25).

## 3. Part A — GPU Ordering

### 3.1 Data model

Add an explicit order column `position integer not null default 0` to
`agent_runtime_spec_gpus`. `routing.RuntimeSpecGPU` gains `Position int`. Both
store drivers write it and read `ORDER BY position` (memory: `sort.Slice` by
`Position`). `putRuntimeSpec` sets `Position = i` (the index in the received
`req.GPUs` array), so the UI/request array order becomes the stored order.

The DTOs keep the **array order as the contract** (no explicit `position` field on
`RuntimeSpecGPUDTO` / `AgentRuntimeSpecGPUDTO`) — the store now guarantees the
array is in `position` order, exactly as it previously guaranteed `gpu_index`
order. `runtimeSpecDTO()` and `AgentRuntimeConfig` already iterate the store's
returned slice in order; no re-sort.

### 3.2 Agent — honor the array order

`hostGPUIDs` (`policy_local.go`): remove `sort.Ints(indices)`, iterate
`spec.GPUs` preserving the received array order, **keep the dedup** (`seen` map —
CUDA stops parsing at the first repeat). Result: the visibility env var value and
the shared `${HOST_GPU_IDS}` placeholder are in operator order, deduplicated.
Refresh the `hostGPUIDs` doc comment (it currently says "order never survives a
save").

### 3.3 Frontend — reorder UI

Reuse the house pattern in the `RuntimeAdminSection` GPU block: drag handles
(`useColumnDrag`/`moveColumn`, vertical) **and** up/down arrows (`swap`,
`IconButton` with `ArrowUpward/DownwardIcon`, disabled at the ends). Reorder the
`gpuRows` array (keys stay `row.rowKey`). `buildSpecBody` already sends the array
order → works end-to-end once the backend preserves it. i18n: reuse
`modelGroupMoveUp/Down` (or GPU-specific keys) in de+en.

### 3.4 Migration/backfill

New migration (next number, **73** on current `main`) adds the `position` column
and backfills existing rows to `position` = rank by `gpu_index` ascending, so **no
existing spec's env var / placeholder order changes on upgrade**; the order only
changes when an operator actively reorders. Cross-driver (memory/sqlite/postgres);
memory store deep-copies unaffected (GPU rows are separate from the spec struct).

## 4. Part B — Visibility Mechanism (env / args)

### 4.1 New field

`routing.RuntimeSpec` gains `VisibleDevicesMode` (a small enum,
`env` | `args`), plus the same on `RuntimeSpecDTO`, `PutRuntimeSpecRequest`, and
`AgentRuntimeSpecDTO`. It is only meaningful when `SetVisibleDevices` is on;
**default `env`** (today's behavior). Stored as `text not null default 'env'`
(new column on `agent_runtime_specs`, same migration 73). Represent it as a typed
string (mirroring the endpoint-modes `EndpointMode` style) with DTO-edge
validation.

- **`env` mode:** the agent injects `VisibleDevicesVar(vendor)=<indices>` exactly
  as today (now order-preserving via Part A). The child renumbers from 0.
- **`args` mode:** the agent does **not** inject the visibility env var. The
  device selection lives in `args` via a device placeholder (below); the child
  sees all cards, so host indices are used.

### 4.2 Device placeholders (agent)

Three new placeholders, siblings of `${HOST_GPU_IDS}`, added as `if inner == …`
cases in the same `expand` closure (so they work in `args` automatically). Each
emits the selected GPU rows' indices in operator (position) order, deduplicated,
as backend-prefixed llama.cpp `--device` names, comma-joined:

| Placeholder | Emits (example for rows 2,3 in that order) |
|---|---|
| `${CUDA_DEVICES}` | `CUDA2,CUDA3` |
| `${VULKAN_DEVICES}` | `Vulkan2,Vulkan3` |
| `${METAL_DEVICES}` | `MTL2,MTL3` |

All three are symmetric: `<Backend><gpu_index>` per row, in order, deduplicated
(empty GPU list ⇒ the same hard error `${HOST_GPU_IDS}` already raises). A shared
helper maps the ordered/deduped index list (from the same source as `hostGPUIDs`)
to `<prefix><idx>` names. Only CUDA/Vulkan/Metal (per the request); ROCm/MUSA are
out of scope.

Usage: the operator writes e.g. `--device ${CUDA_DEVICES}` (or `${VULKAN_DEVICES}`
/ `${METAL_DEVICES}` depending on their llama.cpp build) into `args`, with
`set_visible_devices` on and `visible_devices_mode = args`.

### 4.3 Agent env-injection branch + traps

In `expandSpec`:
- Env injection (`policy_local.go:1006-1012`): skip the append when
  `VisibleDevicesMode == args`.
- Conflict trap (`:971-980`): keep it in **both** modes — a hand-set
  `CUDA_VISIBLE_DEVICES`/etc. in `env` is still refused when `set_visible_devices`
  is on (in args mode it would remap the CUDA namespace and break `--device`
  numbering). The no-GPUs trap stays.

An older agent (pre-flag) does not know `VisibleDevicesMode`; it would inject the
env var (env-mode behavior) AND pass a `${…_DEVICES}` placeholder through
literally → a launch error. This is why args mode is a **hard** dependency on the
capability flag and gets a prominent portal hint (§5).

### 4.4 Gateway validation

Extend `validateRuntimeSpecVisibleDevices` (`service_runtime.go:699-716`): when
`SetVisibleDevices` is on **and** `VisibleDevicesMode == args`, scan `req.Args`
(already a parsed `[]string`) for at least one of the three device-placeholder
tokens; if none is present, return a new sentinel
`ErrRuntimeSpecVisibleDevicesArgsNoPlaceholder`
(`"runtime_spec.visible_devices_args_no_placeholder"`, HTTP 400, add the row to
`portalRuntimeSpecErrRows`). Validate the mode value itself against `env`/`args`
(new `ErrRuntimeSpecVisibleDevicesModeInvalid`). Keep the trap ordering
consistent with the agent.

## 5. Part C — Capability Flag, Version, Portal Hints

### 5.1 Agent

Append one flag `{Name: "gpu_selection", Since: "0.4.0"}` to `agent.Features`
(`features.go`) — it covers **both** new agent behaviors (order-honoring +
device-placeholders/args-mode, which ship together). Bump `const Version` to
`0.4.0` (`agent.go:77`) — MINOR, since `agent.Features` gains a flag
(`TestFeatureRegistry` enforces `Since ≤ Version`). No gateway
`gatewayAgentFeatures` entry is needed (the agent does not gate its own behavior on
the intersection; it always honors what it receives). Add a
`TestGPUSelectionFeatureIsDeclared`-style pin and an agent test that `hostGPUIDs`
preserves order + dedups and that the device placeholders expand correctly.

### 5.2 Portal hints (frontend)

`RuntimeAdminSection.tsx` already has `agentFeatures`, `agentVersion`, and the
`featureMismatch` banner pattern. Add two derived hints, mirroring it:

1. **Agent too old** — when the spec uses a non-default GPU order OR
   `visible_devices_mode = args`, and `!agentFeatures.includes('gpu_selection')`:
   a banner. For a custom order it is informational ("the order is ignored until
   the agent is ≥0.4.0"); for args mode it is prominent ("the connected agent
   (v…) cannot expand the device placeholder — the process would fail to start").
2. **Metal on non-macOS** — when `args` contain `${METAL_DEVICES}` and the
   connected agent's host OS is not macOS: a hint ("`${METAL_DEVICES}` only works
   on a macOS host"). Both remain **hints** — saving stays possible (the agent may
   change later).

The agent already reports `os` in telemetry; if it is not yet surfaced to the
frontend the way `agent_features` is, add `os` (agent host OS) to
`ServerRuntimeReportViewDTO` + `RuntimeReport` (small additive plumbing, mirroring
`agent_features`). The plan pins whether the OS is already available.

## 6. Frontend — visibility-mode control

Under the existing "Enforce GPU visibility" checkbox, when it is on, add a control
for `visible_devices_mode` (`env` / `args`) — a small `SelectField` or radio, with
i18n labels ("via environment variable" / "via arguments") and a short hint noting
that args mode requires a `${…_DEVICES}` placeholder in the args (and links the
three placeholder names). Wire it into `buildSpecBody`/`hydrateSpecFields` and the
`RuntimeSpec` TS type. Use a plain controlled `SelectField` (the app's standard
for such a small enum choice), disabled/hidden when the checkbox is off.

## 7. Docs

`docs/architecture/cross-cutting/agent-runtime-manager.md`:
- §3.2 placeholder catalog (currently "exactly four", ~:249-283) → seven, adding
  the three `${…_DEVICES}` placeholders with the ordering + dedup + numbering
  caveat (Vulkan/Metal independent enumeration; verify with `--list-devices`;
  Metal needs a multi-device build; Metal only on macOS).
- §3.3 `set_visible_devices` (~:408-509): the `env`/`args` mode, the args-mode
  placeholder requirement + validation error, the order-preserving change and the
  backfill safety property.
- The features table / capability section: the new `gpu_selection` flag.
- `docs/architecture/reference/api-surface.md` + data-model: the new
  `visible_devices_mode` field + `agent_runtime_spec_gpus.position` column + the
  new error codes; the (possibly new) `os` on the runtime-report DTO.
- Short ADR: "GPU order is explicit (position column); visibility enforcement has
  an env/args mode; args mode uses backend device placeholders; one `gpu_selection`
  agent flag; ServerAgent 0.4.0".

## 8. Testing (TDD)

- **Store/migration:** migration 73 adds `position` + `visible_devices_mode`,
  backfills `position` = ascending-`gpu_index` rank and `visible_devices_mode` =
  `env`; round-trip GPU rows in a custom order on all three drivers (memory/sqlite/
  postgres); parity where relevant.
- **Portal:** DTO carries `visible_devices_mode` + validates it; args-mode
  placeholder-required validation (missing → 400 with the new code; present →
  ok); GPU array order round-trips (request order == stored == response order);
  agent-wire DTO carries the mode + GPUs in position order.
- **Agent:** `hostGPUIDs` preserves order + dedups; the three device placeholders
  expand to `<prefix><idx>` in order; env injection skipped in args mode; conflict
  trap holds in both modes; `TestFeatureRegistry` covers the new flag.
- **Frontend:** reorder (drag + up/down) changes `gpuRows` order and the PUT
  `gpus` order; the mode `SelectField` round-trips; the two hints render under
  their conditions (agent lacks `gpu_selection`; `${METAL_DEVICES}` + non-macOS
  agent); tsc + i18n parity.
- **Gateway HTTP:** the new validation errors map to 400 (HTTP-level test).

## 9. Out of Scope

- ROCm/MUSA device placeholders (only CUDA/Vulkan/Metal, as requested).
- Reconciling the agent's GPU-index detection with each backend's independent
  enumeration (documented caveat instead; operator verifies with `--list-devices`).
- Per-backend `--main-gpu`/`--tensor-split` helpers (the operator writes those in
  `args` themselves).
- Any change to the endpoint-modes feature.

## 10. Open / To Confirm During Plan

- Exact migration number (73 expected on current `main`) and the cross-driver
  backfill SQL for `position` (per-spec rank by `gpu_index`).
- Whether the agent host `os` is already surfaced to the frontend, or needs the
  small DTO addition (§5.2).
- The device-placeholder helper's exact location and whether it shares code with
  `hostGPUIDs` (same ordered/deduped index list, different formatting).
- Whether the visibility-mode control is a `SelectField` or radio, and final i18n
  wording for the mode + the two hints (de+en).
- Confirm the conflict trap should stay active in args mode (design says yes).

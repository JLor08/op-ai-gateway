# Mixed NVIDIA + AMD hosts (#434) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One host may carry NVIDIA and AMD GPUs: every GPU is identified by (vendor, vendor-local index) end to end, both vendors are measured per process at once on Linux (CUDA, ROCm and Vulkan), visibility and `device_backend` are decided per spec, Vulkan ordinals are mapped through the PCI address, and RPC clusters may place members on the head's own server.

**Architecture:** The agent introduces a comparable GPU key in each of its packages that need one (`collector.GPUKey`, `runtime.GPUKey`; `main.go` adapts between them, so no new import edge), a composite Linux measurer (NVIDIA compute-apps + AMD DRM fdinfo, KFD only as fallback), a pure per-spec device planner (`planDevices`) that owns every visibility/backend/device-name decision, and two feature flags. The gateway adds `gpu_vendor` to four GPU tables (one rawUp migration, all three drivers), keys cluster members by position, backfills legacy empty-vendor rows once from the first vendor-reporting hardware report, gates everything on the agent flags, and the portal renders `NVIDIA 0` / `AMD 1`.

**Tech Stack:** Go 1.26 (`server-agent`, module `op-ai-server-agent`), Go (gateway `gateway/backend`, module `op-ai-gateway`), SQLite + Postgres + memory stores, React/TypeScript/Vite portal (`gateway/frontend`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-27-mixed-vendor-gpus-design.md` (the design; read it before any task). Verified facts the coordinator added after the design (2026-11-27, pinned llama.cpp `ggml/src/ggml-vulkan/ggml-vulkan.cpp` ~l. 5998, AMD test VM) and that this plan treats as part of the spec:

- **V1** `GGML_VK_VISIBLE_DEVICES` indices address the raw `vkEnumeratePhysicalDevices` order (the list `vulkaninfo` prints, CPU/llvmpipe devices included in the count) and the listed order is preserved: `"1,0"` → `Vulkan0` = enumeration index 1, `Vulkan1` = index 0.
- **V2** An index ≥ device count makes ggml throw "Invalid Vulkan device index" (child fails to start): the agent validates ordinals before injecting.
- **V3** `GGML_VK_VISIBLE_DEVICES=""` selects **no** device: the agent never injects an empty value.
- **V4** Without the variable, ggml numbers its own filtered list (supported discrete + integrated GPUs, de-duplicated by UUID across drivers), which is **not** the loader order.

## Global Constraints

- Work only in this worktree, branch `feat/434-mixed-vendor-gpus`; never commit to `main`; all phases land in ONE pull request.
- Go tests run per module: `cd server-agent && go test ./...` and `cd gateway/backend && go test ./...`. Focused runs use the same working directory.
- Pure parsing/decision code lives in files **without** a build tag (the `nvidia_pdh.go`/`wmi_map.go` precedent) so it is tested on macOS and in CI. Code behind `//go:build linux` (process scans, self-probe) is verified with `GOOS=linux go vet ./...` and with the linux-tagged tests in the local `golang:1.26` container (module cache mounted, `GOPROXY=off`); **ask the user before starting Docker Desktop** (it restarts their compose stack).
- Lint per module before every commit: `cd <module> && golangci-lint fmt --diff && golangci-lint run` (gofumpt, gocritic run here, not in `go vet`). Before the PR: `make lint` (Go both modules + `scripts/check-docs.sh`).
- Archtest: `cd server-agent && go test ./internal/archtest/` and `cd gateway/backend && go test ./internal/archtest/` after every task that adds a file or an import. No task adds a module-internal import edge; if one appears, the task is wrong, not the allowlist.
- Frontend gates: `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check` (the CI runs `prettier --check .`).
- i18n: every new key is added to the German **and** English tables of `gateway/frontend/src/i18n.ts` in the same commit (the type-checked build enforces parity).
- Store changes: every store task runs the conformance tests twice, once plain and once with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set to a disposable Postgres provisioned like CI (the postgres subtests skip silently without it). Schema changes are append-only (`migration119Up`, never edit 1–118); dialect differences go through `dialect`; ADR-005 wide types (no new numeric column here needs `bigint`).
- ServerAgent version: exactly one MINOR bump on the branch, `const Version` in `server-agent/internal/agent/agent.go` `"0.26.0"` → `"0.27.0"`, done in Task A1 (AGENTS.md: "bump once, early"); both new flags carry `Since: "0.27.0"`. If `main` has moved past 0.26.x at merge time, re-base the bump on main's version (next free MINOR) and update both `Since` values.
- Feature flags (ADR-025, names not versions): `gpu_vendor_identity` and `rpc_cluster_same_host`. The gateway never compares agent versions.
- Stable error codes. Agent codes follow the agent's `runtime.` prefix (`rpc_classify.go`: `runtime.device_backend_mismatch`); portal codes follow the portal's `runtime_spec.` prefix (`service_runtime.go`). New codes, exactly:
  - agent: `runtime.gpu_vendor_backend_mismatch`, `runtime.vulkan_mapping_unavailable`, `runtime.vulkan_visible_devices_required`
  - portal: `runtime_spec.gpu_vendor_backend_mismatch`, `runtime_spec.vulkan_visible_devices_required`, `runtime_spec.gpu_vendor_invalid`, `runtime_spec.gpu_vendor_unsupported`, `runtime_spec.cluster_member_ambiguous`
- GPU vendor wire values are exactly `nvidia`, `amd`, `apple`; `""` is the legacy "host's only vendor". JSON field names: `vendor` (telemetry, hardware report, runtime GPU samples, spec GPUs, budgets, every portal DTO), `pci_bus_id` (telemetry GPU), `vram_measurable` (hardware GPU, array of `cuda`/`rocm`/`vulkan`), `vulkan_ordinal` (hardware GPU, integer, absent when unknown), `vulkan_device_count` (hardware report). Database columns: `gpu_vendor text not null default ''`, `member_position integer not null default 0`, `ai_servers.gpu_legacy_vendor text not null default ''`, `ai_servers.gpu_vendor_note integer not null default 0`.
- Humans read a GPU as `NVIDIA 0`, `AMD 1`, `Apple 0`.
- Every new source file starts with `// SPDX-License-Identifier: AGPL-3.0-only` and `// Copyright (C) 2026 OnPrem AI Gateway contributors`.
- No LAN host names, IPs of the user's machines, or VM names in any repo text (code, tests, docs, commits, PR). The hosts are "the AMD test VM" and "the NVIDIA test VM".
- Repo-facing text (code comments, commits, docs, PR) in English. Commits are Conventional Commits with a substantive body (the squash merge uses commit bodies).
- `docs/superpowers/` is removed from the branch in the last task; nothing durable may live only there.

## Review Focus

1. **Upgrade without relaunch.** An existing single-vendor host (and a host that today reports NVIDIA plus a ROCm-visible AMD card) upgrades agent and gateway in either order: no running model or RPC member relaunches, no spec goes `not_permitted`. Pinned by `TestApplyConfigVendorBackfillDoesNotRelaunch` (A2), `TestAgentRuntimeConfigVendorStableAcrossBackfill` (F3) and `TestMemberAgentSpecIDStableForSingleMemberPerServer` (G2).
2. **A measurement that is really noise or a read failure never overrides an estimate.** A process that merely opened the other card (≤ 32 KiB VRAM), a pid whose fdinfo is unreadable (privileged-helper user split), or a kernel without `drm-memory-vram` yields "no measurement" for that card, never `0` and never a KFD+fdinfo sum. Pinned by `TestSumDRMClientsDropsStrayCardBelowFloor` (B2), `TestFdinfoMeasurerUnreadablePidIsNoMeasurement` (B3), `TestLinuxMeasurerNeverAddsKFDToFdinfo` (B3).
3. **A mixed host never guesses a Vulkan card.** No `vulkaninfo`, a card listed twice (two drivers), a stale ordinal ≥ device count, or a Vulkan spec without env-mode visibility: the spec is refused with its stable code, never pinned to the other vendor's card and never injected with an empty or out-of-range `GGML_VK_VISIBLE_DEVICES`. Pinned by the `TestPlanDevices*` table (C1) and `TestVulkanOrdinalsByPCIDuplicateIsUnavailable` (B4).
4. **A loopback member is not "held" by the agent's own probe.** The member's readiness probe connects from `127.0.0.1` exactly like a same-host head would. Pinned by `TestLoopbackMemberHoldNeedsTwoPolls` (C4).
5. **An agent without the flag never receives colliding GPU indices.** After a backfill on a mixed host, a downgraded agent (no `gpu_vendor_identity`) gets a document without `vendor` and only the legacy vendor's rows; the legacy vendor is persisted per server (`ai_servers.gpu_legacy_vendor`), so the vendor-less report of the downgraded agent does not change it. Pinned by `TestAgentRuntimeConfigUnflaggedAgentDropsForeignVendorRows` (F3).

---

## File Structure

server-agent (module `op-ai-server-agent`):

| File | Responsibility |
|---|---|
| `internal/sample/sample.go`, `internal/sample/system_report.go` | Wire fields `vendor` (GPU, GPUInfo, RuntimeGPUSample), `vram_measurable`, `vulkan_ordinal`, `vulkan_device_count`. |
| `internal/collector/gpu_key.go` (new) | `collector.GPUKey`. |
| `internal/collector/pci.go` (new, untagged) | `pciAddr` with domain, `parsePCIAddr`. |
| `internal/collector/kfd_measurer.go` | adds `kfdPCIRanks` (topology `properties`: `domain`, `location_id`). |
| `internal/collector/amd.go` | `applyKFDRanks`: AMD index = KFD rank via PCI join; `joinAMDIndex`/`currentAMDIndex` publish the PCI→index map of a complete join (the fdinfo measurer's only mapping). |
| `internal/collector/drm_fdinfo.go` (new, untagged) | fdinfo parse, (pdev, client-id) dedupe, KiB→MiB, noise floor. |
| `internal/collector/drm_fdinfo_linux.go` (new) | process-group scan of `/proc`, `/dev/dri` fd filter, self-probe. |
| `internal/collector/measurer.go` (new, untagged) | `VRAMMeasurer`, `composeMeasurers`. |
| `internal/collector/measurer_fallback_linux.go`, `measurer_fallback_other.go`, `measurer_other.go`, `measurer_windows.go` | per-OS `NewVRAMMeasurer() VRAMMeasurer`. |
| `internal/collector/vulkaninfo.go` (new, untagged) | `vulkaninfo` text parser, PCI→ordinal map. |
| `internal/collector/hwinfo.go` | fills `vendor`, `vram_measurable`, `vulkan_ordinal`, `vulkan_device_count`; stale comment fixed. |
| `internal/collector/nvidia.go`, `apple.go` | set `Vendor`. |
| `internal/runtime/gpu_key.go` (new) | `runtime.GPUKey`, `VulkanMap`, `GPUTopology`, legacy-vendor resolution. |
| `internal/runtime/device_plan.go` (new) | `planDevices`, `GPUPlanError`, the three refusal codes. |
| `internal/runtime/types.go`, `policy.go`, `policy_local.go`, `cluster_devices.go`, `manager.go`, `wsl_vram.go`, `config_files.go`, `manager_log_files.go`, `rpc_member.go`, `rpc_classify.go` | int GPU keys → `GPUKey`; expansion takes `GPUTopology`. |
| `internal/agent/agent.go` | Version, `gpuTotalsMB` keyed by `GPUKey`, Vulkan map into `HostInfo`, vendor on runtime GPU samples, `Config.VRAMMeasurable` into `HardwareOptions`. |
| `internal/agent/features.go` | two flags. |
| `internal/agent/rpc_observer.go` | loopback hold debounce. |
| `main.go` | measurer adapter `runtimeMeasurer`, `Config.VRAMMeasurable` (= `vm.Measurable`). |

gateway/backend (module `op-ai-gateway`):

| File | Responsibility |
|---|---|
| `internal/routing/gpu_key.go` (new) | `routing.GPUKey`, vendor constants, `LegacyGPUVendor`. |
| `internal/routing/store.go`, `memory_store.go` | struct fields, store interface changes, memory driver. |
| `internal/store/migrate.go` | `migration119RawUp` (`gpu_vendor_keys`). |
| `internal/store/sqlite_runtime.go`, `sqlite_rpc_cluster.go`, `sqlite_rpc_cluster_atomic.go`, `sqlite_routes.go` (AI server row) | SQL driver (sqlite + postgres share it via `dialect`). |
| `internal/store/migration119_gpu_vendor_keys_test.go` (new), `gpu_vendor_conformance_test.go` (new) | migration + conformance. |
| `internal/tracing/routingstore_gen.go`, `internal/portal/api_tracing_gen.go` | regenerated (`go generate ./internal/tracing`). |
| `internal/gateway/agent_ingest.go`, `runtime_registry.go`, `perf_endpoints.go`, `benchmark_vram*.go`, `cluster_usage.go`, `agent_features.go`, `portal_runtime_endpoints.go`, `portal_mapping_endpoints.go`, new `portal_server_gpu_note_endpoints.go` | ingest, write-back, VRAM benchmark, cluster usage, endpoints. |
| `internal/portal/gpu_vendor.go` (new) | `resolveSpecBackend`, vendor validation, pushed-row filter. |
| `internal/portal/service_runtime.go`, `service_cluster.go`, `service_cluster_config.go`, `service_cluster_usage.go`, `service_cluster_plan.go`, `service_rpc_members.go` | DTOs, validation, measurability, same-host clusters. |

gateway/frontend: `src/api/gpus.ts` (new: key/label/backend helpers), `src/api/runtime.ts`, `src/api/servers.ts`, `src/components/RuntimeAdminSection.tsx`, `ClusterHeadEditor.tsx`, `RpcMemberSection.tsx`, `HardwareSection.tsx`, `src/components/shared/format.ts`, `src/i18n.ts` (+ their `.test.tsx`/`.test.ts`).

docs/architecture: `09-architecture-decisions.md` (ADR-122, ADR-113 note), `cross-cutting/agent-runtime-manager.md`, `cross-cutting/telemetry-usage-observability.md`, `cross-cutting/persistence.md`, `reference/data-model.md`, `reference/api-surface.md`, `reference/openapi.yaml`, `11-risks-and-technical-debt.md`, `12-glossary.md`.

## Builder's calls made in this plan (design §10 and gaps)

- **Key types.** `runtime.GPUKey{Vendor GPUVendor; Index int}` (implements `encoding.TextMarshaler` as `"<vendor>:<index>"` so `map[GPUKey]int` still marshals), `collector.GPUKey{Vendor string; Index int}`, `routing.GPUKey{Vendor string; Index int}`. `runtime` may not import `collector`/`sample` (archtest), so `main.go` adapts the measurer (`runtimeMeasurer`).
- **Noise floor:** `drmNoiseFloorMiB = 8` per card per process group (stray entries are ≤ 32 KiB; the smallest real model allocation is hundreds of MiB). A card below it is dropped = "no measurement".
- **fdinfo index mapping:** the measurer maps `drm-pdev` only through the AMD collector's successful KFD/PCI join: `amdCollector.Collect` publishes the PCI→index map it used (`currentAMDIndex()`, B1) only when every rocm-smi row joined, so measurer and collector cannot disagree. When the join failed or the KFD topology is unreadable (the collector kept its `cardN` keys), the AMD measurer measures nothing and `Measurable()` lists no AMD backend (a guess is never measured against; the estimate stays).
- **AMD collector join is all-or-nothing:** if any rocm-smi row's `PCI Bus` is missing from the KFD ranks, every row keeps its `cardN` index (a mix of ranks and card keys could collide).
- **`vram_measurable`:** NVIDIA reports `["cuda","vulkan"]` wherever an NVIDIA measurer is installed (Linux compute-apps — verified for Vulkan in design §3 — and Windows PDH, which counts WDDM per-process dedicated memory independent of the API); AMD `["rocm","vulkan"]` with fdinfo, `["rocm"]` on the KFD fallback, both only while the AMD join holds (`Measurable` is a func evaluated at each hardware refresh). The gateway applies the list only to non-NVIDIA GPUs (the exemption in `checkClusterMemberGPUs` concerns non-NVIDIA cards only), so NVIDIA behaviour is unchanged.
- **Second flag needed:** a loopback member is mis-detected as held by the agent's own readiness probe (`rpc_observer.go` `classifyConns` treats any connection from a head IP as the head), so the agent changes and `rpc_cluster_same_host` ships (C4, D1). The gateway lifts the same-host refusals only for an agent declaring `rpc_cluster_same_host`.
- **Without injection, Vulkan tokens keep today's host index** and never emit loader ordinals: per V4 ggml's own numbering without the variable is the filtered list, so a raw loader ordinal would be wrong whenever llvmpipe or a second driver enumerates first. This is the design §5.3 rule as the spec now words it (loader ordinals are never emitted without injection; a single-vendor host keeps its host-index behaviour, a mixed host refuses); Task C1 implements it.
- **Vulkan RPC member on a mixed host:** the agent injects `GGML_VK_VISIBLE_DEVICES` (mapped ordinals) and passes `-d Vulkan0..k-1` (members otherwise never use a visibility variable); on a single-vendor host a Vulkan member keeps today's `-d Vulkan<host index>`.
- **Vendor-spanning specs:** `${HOST_GPU_IDS}`, `${HOST_GPU_IDS_MAIN_LAST}`, `${CUDA_DEVICES}`, `${CUDA_FIRST_DEVICE}`, `${METAL_DEVICES}`, `${METAL_FIRST_DEVICE}` are refused (plain `not_permitted` message, no code) for a spec whose GPUs span both vendors — they have no meaning there.
- **Automatic member id:** the first member (lowest position) of a head on a server keeps `rpca_<head>_<server>`; the k-th (k ≥ 2) gets `rpca_<head>_<server>_m<k>`. Server ids are `srv_<hex>`, so `_m<k>` cannot collide.
- **`rpc_sessions` stay keyed by member server**: a session records that a head used a server; two members on one server do not change that.
- **Usage reset:** `POST .../cluster-members/{serverID}/reset-usage` gains an optional `?position=<n>`; without it a server with several members of the head answers 409 `runtime_spec.cluster_member_ambiguous`. The portal always sends `position`.
- **Loopback members** always run `direct` (relay is pointless on loopback), `bind_ip` and the head's dial address `127.0.0.1`, `head_ips ["127.0.0.1"]`, and are excluded from NetBird policies, LAN-exposure acknowledgement and address readiness.
- **Legacy vendor and ambiguity note:** every backfill stores `ai_servers.gpu_legacy_vendor` (first of nvidia, amd, apple among the report's GPUs; persisted, so a later vendor-less report from a downgraded agent does not erase it). `ai_servers.gpu_vendor_note` (0/1) is set when the backfill rewrote at least one row and the report lists more than one vendor; the hardware section and the runtime admin server header show an informational note with a dismiss button (`POST /api/portal/servers/{id}/gpu-vendor-note/dismiss`, 204, clears the flag only). It never blocks a save.
- **Vendor label:** the portal always shows the vendor when the report carries it (`NVIDIA 0 · RTX 2080 Ti · 0000:01:00.0`), also on single-vendor hosts — one code path, no ambiguity.
- **Portal also refuses `vulkan_visible_devices_required`** at save when the server's stored report lists two vendors (the agent refuses anyway; this tells the operator at save time).
- **Unflagged agent document:** `vendor` is omitted; rows with an empty vendor or the server's stored legacy vendor (`ai_servers.gpu_legacy_vendor`) are pushed, every other row is dropped; with no stored legacy vendor only empty-vendor rows are pushed. A write-back frame without a vendor maps to the stored legacy vendor, else `""`.
- **One migration (119, rawUp)** carries every schema change of this branch.
- **`vulkan_device_count`** rides the hardware report so the agent validates ordinals (V2); the gateway stores it with the report and does not interpret it.

---

## Phase A — Agent GPU identity and reporting

### Task A1: Vendor on the agent's GPU wire types, collectors fill it, version bump

**Files:**
- Modify: `server-agent/internal/sample/sample.go:121-124` (RuntimeGPUSample), `:503-527` (GPU)
- Modify: `server-agent/internal/sample/system_report.go:69-79` (GPUInfo, SystemReport)
- Create: `server-agent/internal/collector/gpu_key.go`
- Modify: `server-agent/internal/collector/nvidia.go` (`parseNvidiaCSV`), `amd.go` (`parseRocmJSON`), `apple.go`, `hwinfo.go:98-108`
- Modify: `server-agent/internal/agent/agent.go:516` (Version)
- Test: `server-agent/internal/sample/system_report_test.go`, `server-agent/internal/collector/nvidia_test.go`, `amd_test.go`, `apple_test.go`, `hwinfo_test.go`

**Interfaces:**
- Produces:
  - `sample.GPU.Vendor string \`json:"vendor,omitempty"\``; `sample.GPUInfo.Vendor string \`json:"vendor,omitempty"\``; `sample.GPUInfo.VRAMMeasurable []string \`json:"vram_measurable,omitempty"\``; `sample.GPUInfo.VulkanOrdinal *int \`json:"vulkan_ordinal,omitempty"\``; `sample.SystemReport.VulkanDeviceCount int \`json:"vulkan_device_count,omitempty"\``; `sample.RuntimeGPUSample.Vendor string \`json:"vendor,omitempty"\``.
  - `collector.GPUKey struct { Vendor string; Index int }` and constants `collector.VendorNVIDIA = "nvidia"`, `VendorAMD = "amd"`, `VendorApple = "apple"` (the same strings as `GPUCollector.Name()`).
  - `agent.Version = "0.27.0"`.

- [ ] **Step 1: Write the failing tests**

```go
// collector/nvidia_test.go
func TestParseNvidiaCSVSetsVendor(t *testing.T) {
	gpus, _ := parseNvidiaCSV(readTestdata(t, "nvidia-smi.csv"))
	for _, g := range gpus { if g.Vendor != "nvidia" { t.Fatalf("vendor=%q", g.Vendor) } }
}
// collector/amd_test.go: TestParseRocmJSONSetsVendor -> every GPU Vendor == "amd"
// collector/apple_test.go: TestAppleCollectorSetsVendor -> Vendor == "apple"
// collector/hwinfo_test.go
func TestCollectHardwareCopiesVendor(t *testing.T) {
	r := CollectHardware(ctx, []GPUCollector{fakeGPU{name: "amd", gpus: []sample.GPU{{Index: 1, Vendor: "amd", PCIBusID: "0000:02:00.0"}}}}, "x", HardwareOptions{})
	if r.GPUs[0].Vendor != "amd" || r.GPUs[0].PCIBusID != "0000:02:00.0" { t.Fatalf("%+v", r.GPUs[0]) }
}
// sample/system_report_test.go
func TestGPUInfoOmitsUnknownVulkanOrdinal(t *testing.T) {
	b, _ := json.Marshal(GPUInfo{Index: 0, Vendor: "nvidia"})
	if strings.Contains(string(b), "vulkan_ordinal") || strings.Contains(string(b), "vram_measurable") { t.Fatal(string(b)) }
	zero := 0
	b, _ = json.Marshal(GPUInfo{VulkanOrdinal: &zero})
	if !strings.Contains(string(b), `"vulkan_ordinal":0`) { t.Fatal(string(b)) }
}
```
(`fakeGPU`/`readTestdata` exist in the package tests or are added as tiny helpers.)

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/sample/ ./internal/collector/ -run 'Vendor|VulkanOrdinal' -v` — Expected: FAIL (unknown field `Vendor`).
- [ ] **Step 3: Implement** the fields; each collector sets `Vendor` to its own `Name()`; `CollectHardware` copies `Vendor`; replace the comment "Empty for every collector but NVIDIA" with "NVIDIA and AMD fill it; Apple has none" and update the `sample.GPU.PCIBusID` doc comment (it is now the cross-API anchor that measurers and the Vulkan mapping join on — design §4). Bump `Version` to `"0.27.0"`.
- [ ] **Step 4: Run** `cd server-agent && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS (the feature guard test passes: no flag yet, Version only grew).
- [ ] **Step 5: Commit**

```bash
git add server-agent
git commit -m "feat(agent): report the GPU vendor on telemetry, hardware and runtime samples (#434)" -m "Every GPU the agent reports now carries vendor (nvidia, amd, apple), filled by the collector that found it, so GPU indices that collide across vendors can be told apart. The hardware GPU gains the optional vram_measurable, vulkan_ordinal and the report vulkan_device_count fields, filled by later commits. Bumps the agent to 0.27.0, the one MINOR bump of this branch (two feature flags follow)."
```

### Task A2: `runtime.GPUKey` replaces bare GPU indices in the runtime manager; legacy-vendor resolution

**Files:**
- Create: `server-agent/internal/runtime/gpu_key.go`, `server-agent/internal/runtime/gpu_key_test.go`
- Modify: `server-agent/internal/runtime/types.go:36-39` (SpecGPU), `:364-367` (GPUBudget), `:1065` (Status.MeasuredVRAM + its MarshalJSON at `:1157-1170`)
- Modify: `server-agent/internal/runtime/policy.go` (`RunningProc.GPUs :45`, `PolicySnapshot.Budgets :131`, `UnattributedVRAM :185`, `specGPUIndexes :547`, `firstSharedGPU`, `touchesAnyGPU`, every `snap.Budgets[g.Index]`)
- Modify: `server-agent/internal/runtime/manager.go` (`measurerFunc :233`, `SetMeasurer :842`, `HostInfo :918`, `SetHostInfo :940`, `raisedDemand :3228`, `realMeasurement :4060`, `fillDetectedBudgets :4733`, budgets build `:4550`, `applyConfig :1959`), `wsl_vram.go` (every `map[int]int`)
- Modify: `server-agent/internal/agent/agent.go:947-955` (field), `:2537-2606` (`publishHostInfo`, `seedHostInfo`, `noteGPUTotal`, `pushHostInfo`), `:2761-2766` (runtime GPU samples)
- Modify: `server-agent/main.go:267`
- Test: existing runtime tests updated mechanically; new tests below (`gpu_key_test.go`, `policy_test.go`, `manager_test.go`, `internal/agent/agent_test.go`).

**Interfaces:**
- Consumes: `GPUVendor`, `GPUVendors` (existing).
- Produces:
  - `type GPUKey struct { Vendor GPUVendor; Index int }`; `func (k GPUKey) String() string` → `"nvidia:0"`; `MarshalText`/`UnmarshalText` with the same form (so `Status.MeasuredVRAM` keeps marshalling as an object).
  - `SpecGPU.Vendor GPUVendor \`json:"vendor,omitempty"\``; `func (g SpecGPU) Key() GPUKey`; `GPUBudget.Vendor GPUVendor \`json:"vendor,omitempty"\``; `func (b GPUBudget) Key() GPUKey`.
  - `func resolveLegacyGPUVendors(cfg Config, legacy GPUVendor) Config` — returns a copy in which every `SpecGPU.Vendor == ""` and `GPUBudget.Vendor == ""` is `legacy`; called first in `owner.applyConfig`.
  - `HostInfo{ GPUTotalsMB map[GPUKey]int; MemAvailableMB int; Vulkan VulkanMap }`, `type VulkanMap struct { Ordinals map[GPUKey]int; DeviceCount int }` (copied deep in `SetHostInfo`).
  - `Status.MeasuredVRAM map[GPUKey]int`; `RunningProc.GPUs map[GPUKey]int`; `PolicySnapshot.Budgets map[GPUKey]int`; `PolicySnapshot.UnattributedVRAM map[GPUKey]int`.
  - `func (m *Manager) SetMeasurer(f func(pids []int) map[int]map[GPUKey]int)`; `type measurerFunc func(pids []int) map[int]map[GPUKey]int`.
  - `func specGPUKeys(spec Spec) []GPUKey` (declared order, first occurrence wins) replaces `specGPUIndexes`; `gpuIndices` (and `newDeviceContext`) stay only until Task C3.
  - `main.go`: `func runtimeMeasurer(f func(pids []int) map[int]map[int]int, vendor runtimectl.GPUVendor) func(pids []int) map[int]map[runtimectl.GPUKey]int` — temporary adapter keying the existing collector measurer by the legacy vendor (Task B3 replaces it).
  - `internal/agent`: `func gpuKeyOf(vendor string, index int) runtimectl.GPUKey` (vendor via `runtimectl.ParseGPUVendor`); `func (a *Agent) noteGPUTotal(key runtimectl.GPUKey, totalBytes int64)`; `a.gpuTotalsMB map[runtimectl.GPUKey]int`; runtime samples `sample.RuntimeGPUSample{Vendor: string(k.Vendor), Index: k.Index, VRAMMeasuredMB: mb}`, sorted by (vendor, index) for a stable wire order.

- [ ] **Step 1: Write the failing tests** (`gpu_key_test.go`, plus one each in `policy_test.go` and `manager_test.go`)

```go
func TestGPUKeyTextRoundTrip(t *testing.T) {
	b, _ := json.Marshal(map[GPUKey]int{{GPUVendorAMD, 1}: 3462})
	if string(b) != `{"amd:1":3462}` { t.Fatal(string(b)) }
	var back map[GPUKey]int
	if err := json.Unmarshal(b, &back); err != nil || back[GPUKey{GPUVendorAMD, 1}] != 3462 { t.Fatal(back, err) }
}
func TestResolveLegacyGPUVendors(t *testing.T) {
	cfg := Config{GPUBudgets: []GPUBudget{{Index: 0, BudgetMB: 1}}, Specs: []Spec{{ID: "a", GPUs: []SpecGPU{{Index: 0}, {Index: 1, Vendor: GPUVendorAMD}}}}}
	got := resolveLegacyGPUVendors(cfg, GPUVendorNVIDIA)
	// empty -> legacy, explicit kept, input untouched
	want := []SpecGPU{{Index: 0, Vendor: GPUVendorNVIDIA}, {Index: 1, Vendor: GPUVendorAMD}}
	if !reflect.DeepEqual(got.Specs[0].GPUs, want) || got.GPUBudgets[0].Vendor != GPUVendorNVIDIA || cfg.Specs[0].GPUs[0].Vendor != "" { t.Fatalf("%+v", got) }
}
// manager_test.go: the relaunch decision of the manager itself, not a self-comparison (Review Focus 1)
func TestApplyConfigVendorBackfillDoesNotRelaunch(t *testing.T) {
	m, fake := newManagerWithFakeProcs(t, legacyVendor(GPUVendorNVIDIA)) // the existing manager-test constructor and fake launcher; host legacy vendor nvidia
	m.ApplyConfig(Config{Specs: []Spec{{ID: "a", GPUs: []SpecGPU{{Index: 0}}}}}) // as pushed by a gateway before the upgrade (vendor-less)
	waitRunning(t, m, "a")
	starts := fake.Starts("a")
	m.ApplyConfig(Config{Specs: []Spec{{ID: "a", GPUs: []SpecGPU{{Index: 0, Vendor: GPUVendorNVIDIA}}}}}) // same spec after the gateway backfilled the vendor
	if got := fake.Starts("a"); got != starts || m.Status("a").State != StateRunning {
		t.Fatalf("an upgrade that only adds the legacy vendor relaunched: starts %d -> %d, state %v", starts, got, m.Status("a").State)
	}
}
// policy_test.go: TestAdmitKeysBudgetsByVendor — budgets {nvidia:0: 1000, amd:0: 20000};
// a running proc on {amd:0: 15000}; a candidate needing {nvidia:0: 900} is admitted
// (the AMD load must not count against NVIDIA 0) and one needing {amd:0: 6000} is refused.
// manager_test.go: TestFillDetectedBudgetsSkipsAppleKeysOnly — totals {nvidia:0: 11264, apple:0: 65536}
// -> budget for nvidia:0 = 11264-headroom, none for apple:0.

// internal/agent/agent_test.go
func TestPublishHostInfoKeepsBothVendorsTotals(t *testing.T) {
	// one sample with nvidia 0 (11 GiB) and amd 0 (16 GiB)
	// -> HostInfo.GPUTotalsMB == {nvidia:0: 11264, amd:0: 16384} (today the second overwrote the first)
}
func TestRuntimeGPUSamplesCarryVendor(t *testing.T) {
	// a Status with MeasuredVRAM {nvidia:0: 7874, amd:1: 3323}
	// -> RuntimeSample.GPUs == [{vendor:"amd",index:1,vram_measured_mb:3323},{vendor:"nvidia",index:0,vram_measured_mb:7874}]
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ ./internal/agent/ -run 'GPUKey|ResolveLegacy|VendorBackfillDoesNotRelaunch|KeysBudgetsByVendor|FillDetectedBudgetsSkipsApple|BothVendorsTotals|RuntimeGPUSamplesCarryVendor' -v` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement.** Mechanical retyping of every GPU-keyed map in the listed files (`g.Index` lookups become `g.Key()`); `fillDetectedBudgets` skips keys with `Vendor == GPUVendorApple` instead of returning on `o.gpuVendor == apple`; `resolveLegacyGPUVendors(cfg, o.gpuVendor)` as the first statement of `owner.applyConfig`; the WSL attribution keeps working on keys (WSL specs resolve to the legacy vendor). `main.go`: `mgr.SetMeasurer(runtimeMeasurer(collector.NewVRAMMeasurer(), runtimeGPUVendor(gpus)))`. Do not change any decision logic in this task.
- [ ] **Step 4: Run** `cd server-agent && go test ./... && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): key runtime GPU maps on (vendor, index) (#434)` — body: why bare indices collide on mixed hosts (#434 point 4: detected totals overwrote each other, fixed here), that an empty vendor resolves to the legacy vendor (first of NVIDIA, AMD, Apple) so no spec relaunches on upgrade, that runtime GPU samples now carry `vendor`, and that the measurer adapter in main.go is temporary.

---

## Phase B — Agent measurement

### Task B0 (controller, not a subagent): capture real fixtures from the test VMs

- [x] AMD test VM fdinfo of a HIP and a Vulkan `llama-server` -> `testdata/fdinfo-amdgpu-hip.txt`, `fdinfo-amdgpu-vulkan.txt`: captured in 1b05c92e.
- [x] AMD test VM KFD topology and per-process KFD stats -> `testdata/kfd-topology-2gpu.txt`, `kfd-proc-hip.txt`: captured in 1b05c92e.
- [x] `vulkaninfo` on both VMs -> `testdata/vulkaninfo-amd-2gpu.txt`, `vulkaninfo-nvidia-2gpu.txt`: captured in 1b05c92e (hostnames and serials scrubbed; the NVIDIA UUIDs and the KFD `unique_id` values are synthetic).

B1-B5 read these files (`server-agent/internal/collector/testdata/`); every expected value below is derived from them, none from design §3 numbers.

### Task B1: PCI addresses with domain; AMD index = KFD rank joined by PCI address

**Files:**
- Create: `server-agent/internal/collector/pci.go`, `pci_test.go`
- Modify: `server-agent/internal/collector/kfd_measurer.go` (add `kfdPCIRanks`; fix the comment at `:20-25` that claims rank == cardN by contract)
- Modify: `server-agent/internal/collector/amd.go` (`Collect`, new `applyKFDRanks`, `joinAMDIndex`, `currentAMDIndex`)
- Read: `server-agent/internal/collector/testdata/kfd-topology-2gpu.txt` (real capture, B0)
- Test: `server-agent/internal/collector/kfd_measurer_test.go`, `amd_test.go`

**Interfaces:**
- Produces:
  - `type pciAddr struct { Domain, Bus, Device, Function uint32 }`; `func (a pciAddr) String() string` → `"0000:02:00.0"`; `func parsePCIAddr(s string) (pciAddr, bool)` accepts `0000:02:00.0` (amdgpu `drm-pdev`, rocm-smi `PCI Bus`), `00000000:65:00.0` (nvidia-smi), case-insensitive hex; rejects `[N/A]`, empty, missing domain.
  - `func decodeKFDLocation(domain, locationID uint64) pciAddr` — `Bus = locationID >> 8`, `Device = (locationID >> 3) & 0x1f`, `Function = locationID & 0x7`.
  - `func kfdPCIRanks(root string) (map[pciAddr]int, error)` — GPU nodes (non-zero `gpu_id`) ordered by node number, rank = position; PCI from `properties` keys `domain` (absent → 0) and `location_id`; a node without `location_id` is skipped (no rank, no guess) but still counts toward the rank of later nodes (the rank is the ROCr ordinal, which counts every GPU node).
  - `func applyKFDRanks(gpus []sample.GPU, ranks map[pciAddr]int) ([]sample.GPU, bool)` — all-or-nothing: if every GPU's `PCIBusID` parses and is in `ranks`, `Index = rank` and `true`; otherwise the input unchanged and `false`.
  - `func joinAMDIndex(gpus []sample.GPU, ranks map[pciAddr]int) []sample.GPU` — `applyKFDRanks`, then publishes `ranks` (atomic, package-level) when the join was complete and `nil` otherwise (also for `ranks == nil`, an unreadable topology); `func currentAMDIndex() map[pciAddr]int` returns the published map (nil = no usable join). The fdinfo measurer and `Measurable` (B3) read only this.
  - `amdCollector.kfdRoot string` (zero value → `kfdRoot`), used by `Collect`: `parseRocmJSON`, then `joinAMDIndex(gpus, ranks)` (`ranks` nil when `kfdPCIRanks` fails).

Fixture `testdata/kfd-topology-2gpu.txt` (real capture from the AMD test VM, B0): one `== <node>` block per KFD node, first line `gpu_id`, then the node's `properties` lines. Node 0 is the CPU (`gpu_id 0`, `location_id 0`); node 1 (`gpu_id 59699`, `location_id 256`, `drm_render_minor 128`) is bus 1 and node 2 (`gpu_id 19306`, `location_id 512`, `drm_render_minor 129`) is bus 2, `domain 0` on all; so bus 1 → rank 0, bus 2 → rank 1 (the DRM `card0` is the virtual VGA and has no KFD node). `unique_id` values in the file are synthetic.

A tiny test helper `writeKFDTopology(t, text) (root string)` materialises the blocks under `t.TempDir()/topology/nodes/<n>/{gpu_id,properties}`.

- [ ] **Step 1: Write the failing tests**

```go
func TestParsePCIAddr(t *testing.T) {
	for in, want := range map[string]pciAddr{
		"0000:02:00.0": {0, 2, 0, 0}, "00000000:65:00.0": {0, 0x65, 0, 0}, "0001:C1:1f.7": {1, 0xc1, 0x1f, 7},
	} { if got, ok := parsePCIAddr(in); !ok || got != want { t.Errorf("%s: %+v %v", in, got, ok) } }
	for _, bad := range []string{"", "[N/A]", "02:00.0", "0000:02:00"} { if _, ok := parsePCIAddr(bad); ok { t.Errorf("%q accepted", bad) } }
}
func TestDecodeKFDLocation(t *testing.T) {
	if decodeKFDLocation(0, 256) != (pciAddr{0, 1, 0, 0}) || decodeKFDLocation(0, 512) != (pciAddr{0, 2, 0, 0}) || decodeKFDLocation(0, 0x0a0b) != (pciAddr{0, 0x0a, 1, 3}) { t.Fatal() }
}
func TestKFDPCIRanksFromTopology(t *testing.T) {
	ranks, err := kfdPCIRanks(writeKFDTopology(t, readTestdata(t, "kfd-topology-2gpu.txt")))
	if err != nil || ranks[pciAddr{0, 1, 0, 0}] != 0 || ranks[pciAddr{0, 2, 0, 0}] != 1 || len(ranks) != 2 { t.Fatal(ranks, err) }
}
func TestApplyKFDRanksReordersByPCI(t *testing.T) {
	// rocm-smi listed bus 2 as card0 and bus 1 as card1 (an enumeration that disagrees with KFD)
	in := []sample.GPU{{Index: 0, PCIBusID: "0000:02:00.0"}, {Index: 1, PCIBusID: "0000:01:00.0"}}
	out, ok := applyKFDRanks(in, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1})
	if !ok || out[0].Index != 1 || out[1].Index != 0 { t.Fatal(out, ok) }
}
func TestApplyKFDRanksAllOrNothing(t *testing.T) {
	in := []sample.GPU{{Index: 0, PCIBusID: "0000:02:00.0"}, {Index: 1, PCIBusID: ""}}
	if out, ok := applyKFDRanks(in, map[pciAddr]int{{0, 2, 0, 0}: 1}); ok || out[0].Index != 0 || out[1].Index != 1 { t.Fatal(out, ok) }
}
func TestAMDIndexPublishedOnlyOnCompleteJoin(t *testing.T) {
	ranks := map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}, {Index: 1, PCIBusID: "0000:02:00.0"}}, ranks)
	if !reflect.DeepEqual(currentAMDIndex(), ranks) { t.Fatal(currentAMDIndex()) }
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: ""}}, ranks) // incomplete join: the map is unpublished
	if currentAMDIndex() != nil { t.Fatal("an incomplete join must unpublish the map") }
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}}, ranks)
	joinAMDIndex([]sample.GPU{{Index: 0, PCIBusID: "0000:01:00.0"}}, nil) // topology unreadable
	if currentAMDIndex() != nil { t.Fatal("an unreadable topology must unpublish the map") }
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ -run 'PCIAddr|KFDLocation|KFDPCIRanks|ApplyKFDRanks|AMDIndex' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces; `amdCollector.Collect` = `parseRocmJSON` then `joinAMDIndex(gpus, ranks)` (ranks nil when `kfdPCIRanks` fails: indices unchanged, nothing published).
- [ ] **Step 4: Run** `cd server-agent && go test ./internal/collector/ && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): number AMD GPUs by their KFD rank joined on the PCI address (#434)` — body: the AMD index is the ROCr ordinal ROCR_VISIBLE_DEVICES addresses; rocm-smi's card key was only assumed equal (ADR-113); on the AMD test VM they agree, so existing hosts keep their numbering; without a readable topology the card key stays; a complete join is published for the fdinfo measurer.

### Task B2: DRM fdinfo parsing, client de-duplication and the noise floor (pure)

**Files:**
- Create: `server-agent/internal/collector/drm_fdinfo.go`, `drm_fdinfo_test.go`
- Read: `server-agent/internal/collector/testdata/fdinfo-amdgpu-hip.txt`, `fdinfo-amdgpu-vulkan.txt` (real captures, B0)
- Create: `server-agent/internal/collector/testdata/fdinfo-amdgpu-novram.txt` (the `renderD129` block of the HIP capture with every `drm-memory-*` and `amd-*vram*` line removed: an old kernel)

**Interfaces:**
- Consumes: `pciAddr`, `parsePCIAddr` (B1).
- Produces:
  - `type drmClient struct { PDev pciAddr; ClientID uint64; VRAMKiB uint64; HasVRAM bool }`
  - `func parseDRMFdinfo(data []byte) (drmClient, bool)` — ok only for `drm-driver: amdgpu` with a parseable `drm-pdev` and `drm-client-id`; `drm-memory-vram` value accepts units `KiB` (the amdgpu form), `MiB`, `GiB`, bare/`B`; missing key → `HasVRAM=false`.
  - `func sumDRMClients(clients []drmClient) (vramKiB map[pciAddr]uint64, anyVRAMKey bool)` — de-duplicates by `(PDev, ClientID)` (first wins) across every fd and pid given, sums per `PDev`.
  - `const drmNoiseFloorMiB = 8`
  - `func drmVRAMToMiB(sums map[pciAddr]uint64, toIndex map[pciAddr]int) map[int]int` — `MiB = KiB >> 10` (truncation, like `kfdSumsToMiB`: never over-reads); drops a card below `drmNoiseFloorMiB` and a `PDev` missing from `toIndex`; nil when empty (a present key would read as measured).

Fixtures (real captures from the AMD test VM, B0; blocks separated by `== <fd target>` lines):

- `fdinfo-amdgpu-hip.txt` — a HIP `llama-server`, **one fd per render node**: `renderD128` (`drm-client-id 28662`, `drm-pdev 0000:01:00.0`, `drm-memory-vram: 32 KiB`) and `renderD129` (client `28663`, `0000:02:00.0`, `drm-memory-vram: 3544696 KiB`). Expected: 3544696 KiB >> 10 = **3461** MiB on bus 2; bus 1 (32 KiB) dropped.
- `fdinfo-amdgpu-vulkan.txt` — a Vulkan `llama-server`, **two fds with the same `drm-client-id` per render node**: `renderD129` twice (client `28698`, `0000:02:00.0`, `3402924 KiB`) and `renderD128` twice (client `28699`, `0000:01:00.0`, `28 KiB`). Expected: 3402924 KiB >> 10 = **3323** MiB on bus 2 (not 6646); bus 1 (28 KiB) dropped.

A helper `splitFdinfoBlocks(data []byte) [][]byte` (test-only) splits on `== ` lines.

- [ ] **Step 1: Write the failing tests**

```go
var amdIdx = map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}

func clientsOf(t *testing.T, name string) []drmClient { /* parse every block of testdata/name */ }

func TestFdinfoHIPRunMeasuresTheModelCard(t *testing.T) {
	sums, any := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-hip.txt"))
	if !any { t.Fatal("vram key not seen") }
	if got := drmVRAMToMiB(sums, amdIdx); !reflect.DeepEqual(got, map[int]int{1: 3461}) { t.Fatal(got) } // 3544696 KiB >> 10
}
func TestFdinfoVulkanRunCountsDuplicatedClientOnce(t *testing.T) {
	sums, _ := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-vulkan.txt"))
	if sums[pciAddr{0, 2, 0, 0}] != 3402924 || sums[pciAddr{0, 1, 0, 0}] != 28 { t.Fatalf("one client, two fds per node: %v", sums) }
	if got := drmVRAMToMiB(sums, amdIdx); !reflect.DeepEqual(got, map[int]int{1: 3323}) { t.Fatalf("got %v, want 3323 (not 6646)", got) }
}
func TestSumDRMClientsDropsStrayCardBelowFloor(t *testing.T) {
	sums, _ := sumDRMClients(clientsOf(t, "fdinfo-amdgpu-vulkan.txt"))
	if _, ok := drmVRAMToMiB(sums, amdIdx)[0]; ok { t.Fatal("28 KiB on card 0 became a measurement") }
}
func TestDRMVRAMDropsUnmappedPDev(t *testing.T) {
	if got := drmVRAMToMiB(map[pciAddr]uint64{{0, 9, 0, 0}: 4 << 20}, amdIdx); got != nil { t.Fatal(got) }
}
func TestParseDRMFdinfoWithoutVRAMKey(t *testing.T) {
	c, ok := parseDRMFdinfo(splitFdinfoBlocks(readTestdata(t, "fdinfo-amdgpu-novram.txt"))[0])
	if !ok || c.HasVRAM { t.Fatal(c, ok) }
}
func TestParseDRMFdinfoIgnoresOtherDrivers(t *testing.T) {
	if _, ok := parseDRMFdinfo([]byte("drm-driver:\ti915\ndrm-client-id:\t3\ndrm-pdev:\t0000:00:02.0\n")); ok { t.Fatal() }
}
func TestParseDRMFdinfoUnits(t *testing.T) {
	c, _ := parseDRMFdinfo([]byte("drm-driver:\tamdgpu\ndrm-client-id:\t1\ndrm-pdev:\t0000:02:00.0\ndrm-memory-vram:\t3 MiB\n"))
	if c.VRAMKiB != 3072 { t.Fatal(c) }
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ -run 'Fdinfo|DRM' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces. Parsing is line-based `key:\tvalue`; unknown keys ignored; a malformed number makes `HasVRAM=false` for that block, never `0`.
- [ ] **Step 4: Run** the same command — Expected: PASS; then `go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run`.
- [ ] **Step 5: Commit** `feat(agent): parse amdgpu DRM fdinfo into per-card VRAM per DRM client (#434)` — body: fdinfo covers ROCm and Vulkan allocations (KFD only ROCm); clients are de-duplicated by (drm-pdev, drm-client-id) because a Vulkan process holds two fds per render node; cards below 8 MiB are noise from processes that merely opened them and stay unmeasured.

### Task B3: Linux fdinfo measurer, self-probe, KFD fallback and the composite measurer

**Files:**
- Create: `server-agent/internal/collector/measurer.go`, `measurer_compose_test.go`
- Create: `server-agent/internal/collector/drm_fdinfo_linux.go`, `drm_fdinfo_linux_test.go` (`//go:build linux`)
- Modify: `server-agent/internal/collector/measurer_fallback_linux.go` (replace `pickLinuxVRAMMeasurer`), `measurer_fallback_other.go`, `measurer_other.go` (keeps calling `fallbackVRAMMeasurer`: `measurer_test.go:72` pins that seam), `measurer_windows.go`, `kfd_measurer_linux.go` (`NewKFDMeasurer` unchanged in shape)
- Modify (the constructor returns a struct now): `server-agent/internal/collector/measurer_test.go:107` (`f != nil` → `f.Measure != nil`), `measurer_other_test.go:25` (`NewVRAMMeasurer() == nil` → `NewVRAMMeasurer().Measure == nil`; compile on macOS/CI)
- Modify: `server-agent/main.go` (`runtimeMeasurer` now adapts `collector.VRAMMeasurer`)
- Test: `server-agent/internal/collector/measurer_fallback_linux_test.go`, `server-agent/main_test.go` (if present; else `runtime_measurer_test.go` in package main)

**Interfaces:**
- Consumes: `currentAMDIndex` (B1: the AMD collector's published join), `getpgidOwner`, `NewNvidiaComputeApps`, `newNvidiaPDHMeasurer`, B2 functions.
- Produces:
  - `type VRAMMeasurer struct { Measure func(pids []int) map[int]map[GPUKey]int; Measurable func() map[string][]string }` (Measure nil = no measurer; `Measurable` is never nil, evaluated at each call, keyed by vendor, values from `cuda`, `rocm`, `vulkan`).
  - `type vendorMeasurer struct { vendor string; measure func(pids []int) map[int]map[int]int }`
  - `func composeMeasurers(parts ...vendorMeasurer) func(pids []int) map[int]map[GPUKey]int` — skips nil parts; merges per pid; nil when every part returned nil/empty; nil when no part.
  - `func NewVRAMMeasurer() VRAMMeasurer` on every OS. Linux: NVIDIA compute-apps (`nvidia` → `["cuda","vulkan"]`) **and** AMD: fdinfo when `probeDRMFdinfo()` (→ `["rocm","vulkan"]`), else KFD (→ `["rocm"]`), else none; every AMD part and the `amd` entry of `Measurable()` exist only while `currentAMDIndex() != nil` (the collector's join succeeded; otherwise no AMD measurement and no AMD measurability). Windows: PDH keyed `nvidia`, `["cuda","vulkan"]`. Other: compute-apps keyed `nvidia`.
  - `func linuxVRAMMeasurer(nvidia, fdinfo, kfd func([]int) map[int]map[int]int, amdJoined func() bool) VRAMMeasurer` — pure assembly, unit-tested on every OS (lives in `measurer.go`, untagged).
  - Linux only: `type fdinfoMeasurer struct { procRoot string; index func() map[pciAddr]int; groupOf func(int) (int, bool) }` (production `index` = `currentAMDIndex`), `func (m fdinfoMeasurer) measure(pids []int) map[int]map[int]int`; `func scanGroupMembers(procRoot string, wanted map[int]bool, groupOf func(int) (int, bool)) map[int][]int` (owner → member pids, every numeric `/proc` entry whose pgid is wanted); `func readDRMClients(procRoot string, pid int) []drmClient` (only fds whose `readlink` starts with `/dev/dri/`; a permission error or vanished pid yields no clients); `func probeDRMFdinfo() bool` (open the first `/dev/dri/renderD*` whose `/sys/class/drm/<node>/device/driver` basename is `amdgpu`, read `/proc/self/fdinfo/<fd>`, true iff a `drm-memory-vram` key appears; closes the fd).
  - `main.go`: `func runtimeMeasurer(m collector.VRAMMeasurer) func(pids []int) map[int]map[runtimectl.GPUKey]int` (nil when `m.Measure` is nil; converts `collector.GPUKey` → `runtimectl.GPUKey{Vendor: runtimectl.ParseGPUVendor(k.Vendor), Index: k.Index}`).

fdinfo measure algorithm (the order matters for the dedupe guarantee):

```go
func (m fdinfoMeasurer) measure(pids []int) map[int]map[int]int {
	if len(pids) == 0 { return nil }
	idx := m.index() // the AMD collector's published join (B1); nil = no usable join -> no measurement
	if len(idx) == 0 { return nil }
	wanted := setOf(pids)
	var out map[int]map[int]int
	for owner, members := range scanGroupMembers(m.procRoot, wanted, m.groupOf) {
		var clients []drmClient
		for _, pid := range members { clients = append(clients, readDRMClients(m.procRoot, pid)...) }
		sums, _ := sumDRMClients(clients) // dedupe across ALL fds and pids of the group
		if byIdx := drmVRAMToMiB(sums, idx); byIdx != nil { out = setOwner(out, owner, byIdx) }
	}
	return out
}
```

- [ ] **Step 1: Write the failing tests** (untagged `measurer_compose_test.go`; `joined := func() bool { return true }`)

```go
func TestComposeMeasurersMergesVendors(t *testing.T) {
	nv := func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 7874}} }
	amd := func([]int) map[int]map[int]int { return map[int]map[int]int{100: {0: 3323}, 200: {1: 3461}} }
	got := composeMeasurers(vendorMeasurer{"nvidia", nv}, vendorMeasurer{"amd", amd})([]int{100, 200})
	want := map[int]map[GPUKey]int{100: {{"nvidia", 0}: 7874, {"amd", 0}: 3323}, 200: {{"amd", 1}: 3461}}
	if !reflect.DeepEqual(got, want) { t.Fatal(got) }
}
func TestComposeMeasurersOneVendorFailingKeepsTheOther(t *testing.T) {
	// nvidia returns nil (nvidia-smi hiccup) -> only the amd keys; both nil -> nil
}
func TestLinuxMeasurerPrefersFdinfoOverKFD(t *testing.T) {
	m := linuxVRAMMeasurer(nil, fdinfoStub, kfdStub, joined)
	if !reflect.DeepEqual(m.Measurable(), map[string][]string{"amd": {"rocm", "vulkan"}}) { t.Fatal(m.Measurable()) }
}
func TestLinuxMeasurerNeverAddsKFDToFdinfo(t *testing.T) {
	// fdinfo stub reports {100:{1:3461}}, kfd stub {100:{1:3592}} (kfd-proc-hip.txt: vram_19306 3766919168 B = 3592 MiB) -> result {100:{amd:1:3461}} exactly
}
func TestLinuxMeasurerKFDFallbackMeasurability(t *testing.T) {
	m := linuxVRAMMeasurer(nvStub, nil, kfdStub, joined)
	// m.Measurable() == {"nvidia": {"cuda","vulkan"}, "amd": {"rocm"}}
}
func TestLinuxMeasurerNoAMDWithoutJoin(t *testing.T) {
	m := linuxVRAMMeasurer(nvStub, fdinfoStub, kfdStub, func() bool { return false })
	if _, ok := m.Measurable()["amd"]; ok { t.Fatal(m.Measurable()) }
	for _, byGPU := range m.Measure([]int{100}) { for k := range byGPU { if k.Vendor == "amd" { t.Fatal("amd measured without a join") } } }
}
func TestLinuxMeasurerNone(t *testing.T) { if m := linuxVRAMMeasurer(nil, nil, nil, joined); m.Measure != nil || len(m.Measurable()) != 0 { t.Fatal() } }
```

Linux-tagged (`drm_fdinfo_linux_test.go`), with a fake `procRoot` built in `t.TempDir()` (`<pid>/fd/<n>` symlinks to `/dev/dri/renderD129`, `<pid>/fdinfo/<n>` files cut from the real blocks of `fdinfo-amdgpu-vulkan.txt`: client `28698` on `0000:02:00.0`, 3402924 KiB), a fake `groupOf` and `index` returning `{bus1: 0, bus2: 1}`:

```go
func TestFdinfoMeasurerSumsAcrossGroupPidsOnce(t *testing.T) {
	// leader 100 and worker 101 (same group) both hold client 28698 on 0000:02:00.0 -> {100: {1: 3323}}
}
func TestFdinfoMeasurerUnreadablePidIsNoMeasurement(t *testing.T) {
	// fdinfo of 100 is mode 000 (or missing) -> result nil, never {100:{1:0}}
}
func TestFdinfoMeasurerSkipsNonDRMFds(t *testing.T) { /* an fd -> socket:[123] is never read */ }
func TestFdinfoMeasurerWithoutJoinMeasuresNothing(t *testing.T) { /* index returns nil -> measure(...) == nil, even with readable fdinfo */ }
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ -run 'ComposeMeasurers|LinuxMeasurer' -v` — Expected: FAIL. Linux-tagged: in the container (ask first) `go test ./internal/collector/ -run FdinfoMeasurer -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces and the algorithm. Update the KFD file comment: KFD is now the fallback for kernels whose amdgpu fdinfo lacks `drm-memory-vram`; it is never added to fdinfo values.
- [ ] **Step 4: Run** `cd server-agent && go test ./... && GOOS=linux go vet ./... && GOOS=windows go vet ./... && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run`, plus the linux-tagged tests in the container — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): measure NVIDIA and AMD per-process VRAM at once on Linux (#434)` — body: composite measurer (keys never collide, they carry the vendor); AMD via DRM fdinfo (ROCm and Vulkan) with a self-probe, KFD as fallback only; the AMD index mapping is the collector's published KFD-rank join (no join, no AMD measurement and no AMD measurability); an unreadable pid is no measurement, never 0.

### Task B4: Vulkan ordinal mapping from `vulkaninfo` (pure)

**Files:**
- Create: `server-agent/internal/collector/vulkaninfo.go`, `vulkaninfo_test.go`
- Read: `server-agent/internal/collector/testdata/vulkaninfo-amd-2gpu.txt`, `vulkaninfo-nvidia-2gpu.txt` (real captures, B0)

**Interfaces:**
- Consumes: `pciAddr` (B1).
- Produces:
  - `type VulkanDevice struct { Ordinal int; Type string; Name string; PCI pciAddr; HasPCI bool }`
  - `func parseVulkaninfo(out []byte) []VulkanDevice` — a device section starts at a line that is exactly `GPU<n>:` after trimming (lines like `GPU id : 0 (...)` in other sections are not headers); inside it the first `deviceType = …`, `deviceName = …`, `pciDomain/pciBus/pciDevice/pciFunction = …` win; numbers are decimal or `0x` hex (`strconv.ParseUint(s, 0, 32)`); `Ordinal = n`; output ordered by `n`.
  - `func vulkanOrdinalsByPCI(devs []VulkanDevice) map[pciAddr]int` — excludes `PHYSICAL_DEVICE_TYPE_CPU` and devices without PCI from the map (V1: they still count toward the device count); a PCI address listed twice (two drivers) is absent from the map.
  - `func collectVulkan(ctx context.Context) (devs []VulkanDevice, ok bool)` — runs `vulkaninfo` when on PATH with a 5 s timeout; `ok=false` when absent or failing.

Fixtures (real captures, B0):

- `vulkaninfo-amd-2gpu.txt` (Vulkan instance 1.3.275): `GPU0` AMD Radeon RX 6900 XT (RADV NAVI21), discrete, bus 1; `GPU1` the same card at bus 2; `GPU2` llvmpipe (LLVM 20.1.2), `PHYSICAL_DEVICE_TYPE_CPU`, no PCI bus info, listed last. Besides the `GPU<n>:` sections the file has `GPU id = N (...)` lines in surface sections (indented, not headers). 3 devices.
- `vulkaninfo-nvidia-2gpu.txt` (Vulkan instance 1.4.309): `GPU0`/`GPU1` NVIDIA GeForce RTX 2080 Ti, discrete, buses 1 and 2, `driverName = NVIDIA`; **no llvmpipe device** (2 devices). The UUIDs in the file are synthetic.

- [ ] **Step 1: Write the failing tests**

```go
func TestParseVulkaninfoAMD(t *testing.T) {
	devs := parseVulkaninfo(readTestdata(t, "vulkaninfo-amd-2gpu.txt"))
	if len(devs) != 3 || devs[2].Type != "PHYSICAL_DEVICE_TYPE_CPU" || devs[2].HasPCI || devs[1].PCI != (pciAddr{0, 2, 0, 0}) { t.Fatalf("%+v", devs) }
	m := vulkanOrdinalsByPCI(devs)
	if !reflect.DeepEqual(m, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}) { t.Fatal(m) }
}
func TestParseVulkaninfoLlvmpipeFirstShiftsOrdinals(t *testing.T) {
	// a synthetic output with llvmpipe as GPU0 and the RX 6900 XTs as GPU1/GPU2
	// -> map {bus1: 1, bus2: 2}: the CPU device still counts (V1)
}
func TestVulkanOrdinalsByPCIDuplicateIsUnavailable(t *testing.T) {
	// GPU0 RADV bus 1, GPU1 AMDVLK bus 1, GPU2 RADV bus 2 -> map {bus2: 2} only
}
func TestParseVulkaninfoNVIDIA(t *testing.T) {
	devs := parseVulkaninfo(readTestdata(t, "vulkaninfo-nvidia-2gpu.txt"))
	if len(devs) != 2 { t.Fatalf("%+v", devs) } // no llvmpipe on the NVIDIA test VM
	if m := vulkanOrdinalsByPCI(devs); !reflect.DeepEqual(m, map[pciAddr]int{{0, 1, 0, 0}: 0, {0, 2, 0, 0}: 1}) { t.Fatal(m) }
}
func TestParseVulkaninfoIgnoresGPUIdLines(t *testing.T) {
	if d := parseVulkaninfo([]byte("Presentable Surfaces:\n\t\tGPU id = 0 (AMD Radeon RX 6900 XT (RADV NAVI21))\n")); len(d) != 0 { t.Fatalf("%+v", d) }
}
func TestParseVulkaninfoHexPCI(t *testing.T) { /* "pciBus = 0x65" -> Bus 0x65 */ }
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ -run Vulkan -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces.
- [ ] **Step 4: Run** the same + `go test ./internal/archtest/` + lint — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): map PCI addresses to Vulkan enumeration ordinals via vulkaninfo (#434)` — body: GGML_VK_VISIBLE_DEVICES addresses the loader's enumeration (vulkaninfo's list, CPU devices included in the count); a card listed by two drivers gets no mapping rather than a guessed one.

### Task B5: Hardware report carries vendor measurability and Vulkan ordinals; the runtime receives the map

**Files:**
- Modify: `server-agent/internal/collector/hwinfo.go:29-33` (`HardwareOptions`), `:46-112` (`CollectHardware`)
- Modify: `server-agent/internal/agent/agent.go` (`Config` new field `VRAMMeasurable func() map[string][]string`; the `HardwareOptions` construction near `:1173`, extracted as `func (a *Agent) hardwareOptions() collector.HardwareOptions`; `seedHostInfo`, `pushHostInfo`, new field `vulkan runtimectl.VulkanMap`)
- Modify: `server-agent/main.go` (sets `Config.VRAMMeasurable = vm.Measurable` from the measurer built once)
- Test: `server-agent/internal/collector/hwinfo_test.go`, `server-agent/internal/agent/agent_test.go`

**Interfaces:**
- Consumes: `VRAMMeasurer.Measurable` (B3), `collectVulkan`, `vulkanOrdinalsByPCI` (B4), `runtimectl.VulkanMap` (A2).
- Produces:
  - `HardwareOptions.VRAMMeasurable func() map[string][]string` (nil-safe; evaluated on every `CollectHardware`, i.e. each hardware refresh, after the GPU collectors ran and the AMD join is known); `agent.Config.VRAMMeasurable` (same type) feeds it from `agent.hardwareOptions()`; `HardwareOptions.vulkan func(context.Context) ([]VulkanDevice, bool)` (unexported test seam, nil → `collectVulkan`).
  - `CollectHardware` sets `GPUInfo.VRAMMeasurable = opts.VRAMMeasurable()[gpu.Vendor]` (copy), `GPUInfo.VulkanOrdinal` from the PCI map when the GPU's `PCIBusID` parses and maps, `SystemReport.VulkanDeviceCount = len(devs)` when `vulkaninfo` ran.
  - `func vulkanMapFromReport(hw *sample.SystemReport) runtimectl.VulkanMap` in `agent.go`; `seedHostInfo` stores it, `pushHostInfo` always includes it.

- [ ] **Step 1: Write the failing tests**

```go
func TestCollectHardwareFillsMeasurabilityAndVulkanOrdinal(t *testing.T) {
	opts := HardwareOptions{VRAMMeasurable: func() map[string][]string { return map[string][]string{"amd": {"rocm", "vulkan"}} },
		vulkan: func(context.Context) ([]VulkanDevice, bool) { return parseVulkaninfo(readTestdata(t, "vulkaninfo-amd-2gpu.txt")), true }}
	r := CollectHardware(ctx, []GPUCollector{fakeGPU{name: "amd", gpus: []sample.GPU{{Index: 1, Vendor: "amd", PCIBusID: "0000:02:00.0"}}}}, "x", opts)
	g := r.GPUs[0]
	if g.VulkanOrdinal == nil || *g.VulkanOrdinal != 1 || !slices.Equal(g.VRAMMeasurable, []string{"rocm", "vulkan"}) || r.VulkanDeviceCount != 3 { t.Fatalf("%+v %d", g, r.VulkanDeviceCount) }
}
func TestCollectHardwareWithoutVulkaninfo(t *testing.T) { /* vulkan returns ok=false -> no ordinal, count 0 */ }
func TestCollectHardwareNVIDIAVulkanCount(t *testing.T) { /* vulkaninfo-nvidia-2gpu.txt: nvidia 0/1 get ordinals 0/1, VulkanDeviceCount == 2 (no llvmpipe) */ }
func TestHardwareOptionsUseConfigMeasurability(t *testing.T) {
	a := newTestAgent(t, Config{VRAMMeasurable: func() map[string][]string { return map[string][]string{"amd": {"rocm"}} }}) // the existing agent-test constructor
	if got := a.hardwareOptions().VRAMMeasurable()["amd"]; !slices.Equal(got, []string{"rocm"}) { t.Fatal(got) }
}
func TestSeedHostInfoCarriesVulkanMap(t *testing.T) {
	// report: amd 0 ordinal 0, amd 1 ordinal 1, count 3 -> HostInfo.Vulkan == {Ordinals: {amd:0:0, amd:1:1}, DeviceCount: 3}
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/collector/ ./internal/agent/ -run 'Measurability|Vulkan|HardwareOptions' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces. `main.go` builds `vm := collector.NewVRAMMeasurer()` once, sets `agent.Config.VRAMMeasurable = vm.Measurable` and passes `runtimeMeasurer(vm)` into `SetMeasurer`.
- [ ] **Step 4: Run** `cd server-agent && go test ./... && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): report per-GPU VRAM measurability and Vulkan ordinals (#434)` — body: measurability is reported, not assumed, so the gateway's estimate exemption reads the agent's real measurer; the Vulkan mapping rides the hardware report for the portal and HostInfo for launch decisions.

---

## Phase C — Agent per-spec visibility and device backend

### Task C1: `planDevices` — the per-spec decision table (pure)

**Files:**
- Create: `server-agent/internal/runtime/device_plan.go`, `device_plan_test.go`
- Modify: `server-agent/internal/runtime/gpu_key.go` (add `GPUTopology`)

**Interfaces:**
- Consumes: `GPUKey`, `VulkanMap`, `ResolveDeviceBackend` (host-level rule, unchanged, used for specs without GPUs), backend prefixes in `cluster_devices.go`.
- Produces:
  - `type GPUTopology struct { Legacy GPUVendor; Vendors GPUVendors; Vulkan VulkanMap }`; `func (t GPUTopology) Mixed() bool` (both `nvidia` and `amd` in `Vendors`).
  - `const codeGPUVendorBackendMismatch = "runtime.gpu_vendor_backend_mismatch"`, `codeVulkanMappingUnavailable = "runtime.vulkan_mapping_unavailable"`, `codeVulkanVisibleDevicesRequired = "runtime.vulkan_visible_devices_required"`.
  - `type GPUPlanError struct { Code, Message string }`; `func (e *GPUPlanError) Error() string`.
  - `type devicePlan struct { Prefix string; VisVar string; VisValue string; Names []string; Keys []GPUKey; SpansVendors bool }` — `Keys` = `specGPUKeys(spec)`, `Names` = local device names in declared order.
  - `func resolveDevicePrefix(spec Spec, topo GPUTopology) (prefix string, err error)` — rules 1–3 below (also used by C3).
  - `func planDevices(spec Spec, topo GPUTopology) (devicePlan, error)` — `resolveDevicePrefix`, then rules 4–7.

Decision rules (`inject` = `spec.SetVisibleDevices && spec.VisibleDevicesMode != "args"`):

1. Resolve `Prefix`: explicit `device_backend` as today; `auto`/`""` with GPUs from the spec's own vendors (only `nvidia` → CUDA, only `amd` → ROCm, only `apple` → MTL, `nvidia`+`amd` → Vulkan); without GPUs → `ResolveDeviceBackend(spec, topo.Vendors)` (today's host rule).
2. Unknown vendor value on a GPU → plain error `unknown gpu vendor "<v>"` (no code).
3. `CUDA` with any non-`nvidia` GPU, `ROCm` with any non-`amd`, `MTL` with any non-`apple` → `GPUPlanError{codeGPUVendorBackendMismatch}`. Vulkan and SYCL accept any vendor.
4. No GPUs → `VisVar=""`, no names (V3: never inject an empty list).
5. `inject` and CUDA → `CUDA_VISIBLE_DEVICES` = NVIDIA indices, names `CUDA0..k-1`; ROCm → `ROCR_VISIBLE_DEVICES`, `ROCm0..`; MTL/SYCL → today's `VisibleDevicesVar(vendor)` (apple: none) and host-index names.
6. `inject` and Vulkan: every key must be in `topo.Vulkan.Ordinals` with `0 ≤ ordinal < topo.Vulkan.DeviceCount` (V2); then `GGML_VK_VISIBLE_DEVICES` = ordinals in declared order, de-duplicated (V1), names `Vulkan0..k-1`. Otherwise: mixed host → `codeVulkanMappingUnavailable`; single-vendor host → today's behaviour (`VisibleDevicesVar(topo.Legacy)` with host indices, names `Vulkan<host index>`).
7. Not `inject` and Vulkan with GPUs on a mixed host → `codeVulkanVisibleDevicesRequired`. Otherwise not `inject` → no variable, names = prefix + **host index** (today's; never a loader ordinal, V4).

- [ ] **Step 1: Write the failing table test** `TestPlanDevices` with these rows (topologies: `nv2` = NVIDIA-only, legacy nvidia; `amd2` = AMD-only, Vulkan ordinals `{amd:0:0, amd:1:1}`, count 3; `mixed` = vendors nvidia+amd, ordinals `{nvidia:0:0, amd:0:1, amd:1:2}`, count 4; `mixedNoVK` = mixed without ordinals):

| name | topo | spec GPUs | backend | inject | want |
|---|---|---|---|---|---|
| nvidia auto | nv2 | nvidia 1, nvidia 0 | auto | yes | CUDA, `CUDA_VISIBLE_DEVICES=1,0`, names CUDA0,CUDA1 |
| amd auto | amd2 | amd 1 | auto | yes | ROCm, `ROCR_VISIBLE_DEVICES=1`, ROCm0 |
| mixed auto → vulkan | mixed | nvidia 0, amd 1 | auto | yes | Vulkan, `GGML_VK_VISIBLE_DEVICES=0,2`, Vulkan0,Vulkan1, SpansVendors |
| mixed single-vendor spec keeps native | mixed | amd 0 | auto | yes | ROCm, `ROCR_VISIBLE_DEVICES=0` |
| declared order preserved (V1) | amd2 | amd 1, amd 0 | vulkan | yes | `GGML_VK_VISIBLE_DEVICES=1,0`, Vulkan0,Vulkan1 |
| both vendors + cuda | mixed | nvidia 0, amd 0 | cuda | yes | `runtime.gpu_vendor_backend_mismatch` |
| nvidia + rocm | nv2 | nvidia 0 | rocm | no | `runtime.gpu_vendor_backend_mismatch` |
| amd + cuda | amd2 | amd 0 | cuda | no | `runtime.gpu_vendor_backend_mismatch` |
| vulkan unmapped, mixed | mixedNoVK | amd 0 | vulkan | yes | `runtime.vulkan_mapping_unavailable` |
| stale ordinal ≥ count (V2) | mixed with `{amd:0: 7}`, count 4 | amd 0 | vulkan | yes | `runtime.vulkan_mapping_unavailable` |
| vulkan unmapped, single-vendor | nv2 (no ordinals) | nvidia 2 | vulkan | yes | `CUDA_VISIBLE_DEVICES=2`, names Vulkan2 (today) |
| vulkan without inject, mixed | mixed | nvidia 0 | vulkan | no | `runtime.vulkan_visible_devices_required` |
| vulkan args mode, mixed | mixed | nvidia 0 | vulkan | args | `runtime.vulkan_visible_devices_required` |
| vulkan without inject, single-vendor, llvmpipe first (V4) | amd2 with ordinals `{amd:0:1, amd:1:2}` | amd 1 | vulkan | no | no var, names **Vulkan1** (host index, not ordinal 2) |
| no GPUs never injects (V3) | mixed | — | vulkan | yes | VisVar "", no names, no error |

(`planDevices` never sees an empty vendor: `resolveLegacyGPUVendors` (A2) ran in `applyConfig` before it. An empty vendor reaching it anyway is treated as `topo.Legacy`.)

Plus `TestResolveDevicePrefix` (`mixed`, `auto`, GPUs `nvidia 0, amd 0` → `Vulkan`; `cuda` with those GPUs → `GPUPlanError{codeGPUVendorBackendMismatch}`).

Each row asserts the full `devicePlan` (or `errors.As(err, &pe) && pe.Code == want`).

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ -run TestPlanDevices -v` — Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `planDevices` per the rules.
- [ ] **Step 4: Run** the same + `go test ./internal/archtest/` + lint — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): decide device backend and visibility per spec from its GPUs' vendors (#434)` — body: the decision table of the design (§5.3/§5.4) as one pure function with the three stable refusal codes; the facts verified against ggml-vulkan (declared order kept, out-of-range ordinal fatal, empty list selects nothing, no loader ordinals without injection).

### Task C2: Wire `planDevices` into placeholder expansion, env injection and start refusal

**Files:**
- Modify: `server-agent/internal/runtime/policy_local.go` (`injectedVisibleDevicesVar :231`, `visibleDevicesOwnedVars :267`, `gpuIndices :278`, `hostGPUIDs :312`, `deviceList :359`, `ExpandPlaceholders :1017`, `resolveNonSecretToken :1099`, `checkSpecEnv :1327`, `agentBaseEnv :1398`, `expandSpec :1428`)
- Modify: `server-agent/internal/runtime/cluster_devices.go` (`renumbers`, `resolveClusterDeviceToken`, `resolveGenericDevices`, `localDeviceNames` move onto the plan; `newDeviceContext`/`deviceContext` and `gpuIndices` stay as thin shims until C3, because `rpc_member.go:131,265-268` and `rpc_classify.go:171` still call them — C2 must compile and pass on its own)
- Modify: `server-agent/internal/runtime/config_files.go`, `manager_log_files.go`, `manager.go` (`startProcess :4763-4808`, the `prepareLogFiles` call at `:4860`, owner fields `gpuVendor/gpuVendors :1636-1643`, new `func (o *owner) topology() GPUTopology`)
- Test: `server-agent/internal/runtime/policy_local_test.go`, `manager_test.go`; mechanical signature updates (pass a `GPUTopology` built from the former `(vendor, vendors)` arguments) in `command_test.go`, `config_files_test.go`, `wsl_test.go`, `cluster_devices_test.go`, `secret_mask_test.go`, `rpc_member_test.go` (~70 call sites of `ExpandPlaceholders`/`expandSpec`/`planSpecConfigFiles`/`writeSpecConfigFiles`/`expandConfigFileContent`)

**Interfaces:**
- Consumes: `planDevices`, `GPUTopology`, `GPUPlanError` (C1); `HostInfo.Vulkan` (A2/B5).
- Produces:
  - `func ExpandPlaceholders(spec Spec, port int, topo GPUTopology, getenv func(string) string, configFilesDir string) (args []string, env []string, err error)`; `expandSpec`, `planSpecConfigFiles`, `writeSpecConfigFiles`, `prepareLogFiles` take `topo GPUTopology` instead of `(vendor, vendors)`.
  - `visibleDevicesOwnedVars` = `CUDA_VISIBLE_DEVICES`, `ROCR_VISIBLE_DEVICES`, `HIP_VISIBLE_DEVICES`, `GGML_VK_VISIBLE_DEVICES`.
  - `func (o *owner) topology() GPUTopology` — `Legacy: o.gpuVendor`, `Vendors: o.gpuVendors`, `Vulkan: o.hostInfo().Vulkan`.
  - `startProcess`: an expansion error that `errors.As` a `*GPUPlanError` goes to `o.setNotPermittedCode(st, err.Error(), pe.Code)`; other expansion errors stay `setNotPermitted`.
  - Tokens: `${DEVICES}`, `${FIRST_LOCAL_DEVICE}`, `${VULKAN_DEVICES}`, `${VULKAN_FIRST_DEVICE}` render `plan.Names` when the plan's prefix is Vulkan and it injected `GGML_VK_VISIBLE_DEVICES`; otherwise today's rendering. `${HOST_GPU_IDS}`, `${HOST_GPU_IDS_MAIN_LAST}`, `${CUDA_DEVICES}`, `${CUDA_FIRST_DEVICE}`, `${METAL_DEVICES}`, `${METAL_FIRST_DEVICE}` refuse a spec with `plan.SpansVendors` (plain error: `${X} has no meaning for a spec whose GPUs span NVIDIA and AMD`).

- [ ] **Step 1: Write the failing tests**

```go
func TestExpandInjectsGGMLVKVisibleDevicesOnVulkanSpec(t *testing.T) {
	spec := Spec{GPUs: []SpecGPU{{Index: 1, Vendor: GPUVendorAMD}, {Index: 0, Vendor: GPUVendorAMD}}, DeviceBackend: "vulkan", SetVisibleDevices: true,
		Args: []string{"--device", "${VULKAN_DEVICES}"}}
	args, env, err := ExpandPlaceholders(spec, 0, amd2Topo, os.Getenv, "")
	// env contains "GGML_VK_VISIBLE_DEVICES=1,0" and no ROCR_VISIBLE_DEVICES; args == [--device Vulkan0,Vulkan1]
}
func TestExpandRefusesHandSetGGMLVKVisibleDevices(t *testing.T) { /* SetVisibleDevices + Env{"GGML_VK_VISIBLE_DEVICES": "0"} -> error */ }
func TestExpandRefusesHostGPUIDsOnVendorSpanningSpec(t *testing.T) { /* mixed vulkan spec with ${HOST_GPU_IDS} -> error */ }
func TestStartRefusalCarriesGPUPlanCode(t *testing.T) {
	// manager with a mixed topology and a cuda spec on {nvidia 0, amd 0}
	// -> Status.State == not_permitted, LastError.Code == "runtime.gpu_vendor_backend_mismatch"
}
func TestExpandSingleVendorCUDASpecUnchanged(t *testing.T) {
	// regression: today's golden output for a CUDA_VISIBLE_DEVICES spec is byte-identical
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ -run 'GGMLVK|VendorSpanning|GPUPlanCode|SingleVendorCUDASpecUnchanged' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.** The spec's own expansion path uses the plan (`gpuIndices` callers there use `specGPUKeys`; the `deviceContext`/`gpuIndices` shims remain for C3's callers); host-index renderings (`hostGPUIDs`, `deviceList`) render `Key.Index`. Update the doc comments of `VisibleDevicesVar` (now per spec), `deviceList` (caveat disappears where the mapping exists) and `ResolveDeviceBackend` (host rule only for specs without GPUs).
- [ ] **Step 4: Run** `cd server-agent && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS. Existing expansion tests keep their expectations except two deliberate design §5.3 changes: (a) Vulkan + `set_visible_devices` now injects `GGML_VK_VISIBLE_DEVICES`; (b) on a two-vendor host a spec whose GPUs are vendor-less resolves them to the legacy vendor (NVIDIA) and so gets its native backend: `cluster_devices_test.go:66` (env mode) `Vulkan3,Vulkan2` → `CUDA0,CUDA1`, `:79` (no visibility) → `CUDA3,CUDA2` (a single-vendor spec now gets its native backend). Each such updated assertion names design §5.3 in a comment.
- [ ] **Step 5: Commit** `feat(agent): per-spec visibility, GGML_VK_VISIBLE_DEVICES and coded refusals (#434)` — body: a vulkan spec with set_visible_devices in env mode now gets GGML_VK_VISIBLE_DEVICES on every host (operator decision; a literal --device VulkanN must be adapted), single-vendor specs on a mixed host get their native backend, mismatches and unmapped Vulkan cards are refused with stable codes.

### Task C3: RPC members use the per-spec plan

**Files:**
- Modify: `server-agent/internal/runtime/rpc_member.go` (`memberArgs :122`, `memberEnv :170`, `prepareMember :219`, `memberDevicePrefix :264`)
- Modify: `server-agent/internal/runtime/rpc_classify.go` (`expectedMemberDevices :170`)
- Modify: `server-agent/internal/runtime/policy_local.go`, `cluster_devices.go` (delete the C2 shims `gpuIndices`, `newDeviceContext`, `deviceContext`: their last callers are in the two files above)
- Test: `server-agent/internal/runtime/rpc_member_test.go`, `rpc_classify_test.go`

**Interfaces:**
- Consumes: `planDevices`, `GPUTopology` (C1/C2).
- Produces:
  - `func (o *owner) memberDevicePlan(spec Spec) (devicePlan, error)` replaces `memberDevicePrefix`. A member never has `SetVisibleDevices` itself; the plan is computed as: `prefix, err := resolveDevicePrefix(spec, topo)`, then `planDevices` on a **copy** of the spec with `SetVisibleDevices = topo.Mixed() && prefix == "Vulkan"` and `VisibleDevicesMode = "env"` (the member rule of this plan; the member's own spec is never modified), so only a Vulkan member on a mixed host injects.
  - `func memberArgs(spec Spec, childPort int, names []string) ([]string, error)` — `-d` = `strings.Join(names, ",")`, `CPU` when empty.
  - `func memberEnv(spec Spec, cacheRoot string, plan devicePlan) []string` — appends `GGML_VK_VISIBLE_DEVICES=<ordinals>` when the plan injected.
  - `func expectedMemberDevices(spec Spec) int` — `len(specGPUKeys(spec))`, 1 when empty.

- [ ] **Step 1: Write the failing tests**

```go
func TestMemberArgsCUDAOnMixedHost(t *testing.T) {
	// spec GPUs {nvidia 1}, backend auto, mixed topo -> args contain "-d CUDA1", env has no visibility var
}
func TestMemberVulkanOnMixedHostInjects(t *testing.T) {
	// GPUs {nvidia 0, amd 1}, backend vulkan, mixed topo with ordinals -> "-d Vulkan0,Vulkan1", env "GGML_VK_VISIBLE_DEVICES=0,2"
}
func TestMemberVulkanOnSingleVendorHostUnchanged(t *testing.T) { /* -d Vulkan<host index>, no injection */ }
func TestMemberMismatchRefused(t *testing.T) { /* rocm member on {nvidia 0} -> *GPUPlanError code runtime.gpu_vendor_backend_mismatch */ }
func TestMemberDevicePlanDoesNotMutateSpec(t *testing.T) {
	spec := Spec{GPUs: []SpecGPU{{Index: 0, Vendor: GPUVendorNVIDIA}}, DeviceBackend: "vulkan"}
	if _, err := (&owner{}).memberDevicePlan(spec); err != nil && !errors.As(err, new(*GPUPlanError)) { t.Fatal(err) }
	if spec.SetVisibleDevices { t.Fatal("memberDevicePlan mutated the member spec") }
}
func TestExpectedMemberDevicesCountsKeys(t *testing.T) { /* {nvidia 0, amd 0} -> 2 (indices alone would dedupe to 1) */ }
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ -run 'Member' -v` — Expected: FAIL.
- [ ] **Step 3: Implement**; `prepareMember` returns the plan's `*GPUPlanError` unchanged so the start path records its code (same route as C2). Update the `memberArgs` doc comment ("A member never runs in visibility-variable mode" now has the mixed-host Vulkan exception).
- [ ] **Step 4: Run** `cd server-agent && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): vendor-aware device lists for RPC members (#434)` — body: member backends resolve from their own GPUs; a Vulkan member on a mixed host is pinned through GGML_VK_VISIBLE_DEVICES because without it ggml numbers its filtered list, not the loader order.

### Task C4: Loopback members — the agent's own probe is not a head hold

**Files:**
- Modify: `server-agent/internal/agent/rpc_observer.go` (`classifyConns :54`, `observedMember :81`, `poll :273`)
- Test: `server-agent/internal/agent/rpc_observer_test.go`

**Interfaces:**
- Produces: `func loopbackOnly(headIPs []netip.Addr) bool` (non-empty and every address `IsLoopback()`); in `poll`, for a loopback-only member a head connection counts toward `HeldBy` only when the same `connTuple` was already present in the previous poll (`headSeen[tuple]` older than this poll). Non-loopback members are unchanged.

- [ ] **Step 1: Write the failing tests**

```go
func TestLoopbackMemberHoldNeedsTwoPolls(t *testing.T) {
	// head_ips ["127.0.0.1"]; poll 1 sees 127.0.0.1:40000 -> no hold; poll 2 sees the same tuple -> held by the head
}
func TestLoopbackMemberProbeNeverHolds(t *testing.T) {
	// poll 1 sees 127.0.0.1:40001 (the readiness probe), poll 2 sees nothing -> never held
}
func TestRemoteMemberHoldUnchanged(t *testing.T) { /* head_ips ["100.64.0.2"]: one poll is enough, as today */ }
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/agent/ -run 'LoopbackMember|RemoteMemberHoldUnchanged' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces.
- [ ] **Step 4: Run** `cd server-agent && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(agent): debounce head detection for RPC members bound to loopback (#434)` — body: on a same-host cluster the head and the agent's own readiness probe both connect from 127.0.0.1; only a connection that survives two observer polls is the head.

---

## Phase D — Agent feature flags

### Task D1: Declare `gpu_vendor_identity` and `rpc_cluster_same_host`

**Files:**
- Modify: `server-agent/internal/agent/features.go` (registry after `runtime_log_files`, constants)
- Test: `server-agent/internal/agent/features_test.go`

**Interfaces:**
- Produces: `const FeatureGPUVendorIdentity = "gpu_vendor_identity"`, `const FeatureRPCClusterSameHost = "rpc_cluster_same_host"`; registry entries `{Name: FeatureGPUVendorIdentity, Since: "0.27.0"}`, `{Name: FeatureRPCClusterSameHost, Since: "0.27.0"}` with comments stating what each promises (vendor on every GPU report, `vendor` accepted on spec GPUs and budgets with empty = legacy vendor, per-vendor measurement, per-spec visibility, Vulkan mapping; loopback member hold detection) and that the gateway gates saves on them.

- [ ] **Step 1: Write the failing test**

```go
func TestMixedVendorFeaturesAreDeclared(t *testing.T) {
	for _, name := range []string{"gpu_vendor_identity", "rpc_cluster_same_host"} {
		if !slices.Contains(FeatureNames(), name) { t.Fatalf("%s missing", name) }
	}
}
```

- [ ] **Step 2: Run** `cd server-agent && go test ./internal/agent/ -run 'MixedVendorFeatures|FeatureRegistry' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd server-agent && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS (`TestFeatureRegistry` accepts `Since == Version`).
- [ ] **Step 5: Commit** `feat(agent): declare gpu_vendor_identity and rpc_cluster_same_host (#434)` — body: the two names the gateway negotiates; both Since 0.27.0, the branch's single MINOR bump.

---

## Phase E — Gateway store

### Task E1: Migration 119 `gpu_vendor_keys` (all dialects)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (ledger after `:153`; new `migration119RawUp`)
- Modify: `gateway/backend/internal/store/sqlite_rpc_cluster.go`, `sqlite_rpc_cluster_atomic.go` (insert `member_position` for GPU rows: the position of the member row with the same `server_id` in the slice being written — unique today)
- Create: `gateway/backend/internal/store/migration119_gpu_vendor_keys_test.go`
- Modify: `gateway/backend/internal/store/schema_column_coverage_test.go`, `ai_server_column_parity_test.go` (classification below)

**Interfaces:**
- Produces schema (both dialects):
  - `agent_runtime_spec_gpus`: `gpu_vendor text not null default ''`, PK `(spec_id, gpu_vendor, gpu_index)`.
  - `ai_server_gpu_budgets`: `gpu_vendor`, PK `(server_id, gpu_vendor, gpu_index)`.
  - `rpc_member_spec_gpus`: `gpu_vendor`, PK `(spec_id, gpu_vendor, gpu_index)`.
  - `rpc_cluster_members`: PK `(head_spec_id, position)`; the `(head_spec_id, server_id)` uniqueness is dropped; `idx_rpc_cluster_members_server`, `idx_rpc_cluster_members_member_spec` (partial unique) and `idx_rpc_cluster_members_server_port` (partial unique) are recreated.
  - `rpc_cluster_member_gpus`: `gpu_vendor`, `member_position integer not null default 0` (backfilled from the member row's `position` joined on `(head_spec_id, server_id)`), PK `(head_spec_id, member_position, gpu_vendor, gpu_index)`, FK `(head_spec_id, member_position)` → `rpc_cluster_members(head_spec_id, position)` on delete cascade; `server_id` kept as a plain column.
  - `ai_servers.gpu_legacy_vendor text not null default ''` (the server's persisted legacy vendor, set by every backfill) and `ai_servers.gpu_vendor_note integer not null default 0` (0/1: the legacy-mapping note, set when a backfill rewrote rows on a multi-vendor report, cleared by dismiss).
- `migration119RawUp(ctx, s *SQLStore, version int, name string) error`: SQLite — dedicated `*sql.Conn`, `pragma foreign_keys=off`, one transaction: for each table `create table <t>_new (...)`, `insert into <t>_new select ...`, `drop table <t>`, `alter table <t>_new rename to <t>`, recreate indexes, `pragma foreign_key_check` must return no rows, stamp `schema_migrations`, commit, `pragma foreign_keys=on` (deferred, also on error) — the migration-40 pattern. Postgres — one transaction: `addColumnIfMissing`, drop the old primary key / FK / unique constraints by name looked up in `pg_constraint` (no guessed names), add the new ones, backfill `member_position`, stamp.

- [ ] **Step 1: Write the failing test** (`forEachDialectMigratedTo(t, 118, ...)`, then `migrateTo(119)`)

```go
func TestMigration119GPUVendorKeys(t *testing.T) {
	forEachDialectMigratedTo(t, 118, func(t *testing.T, s *SQLStore) {
		// seed at v118: a server, a spec with gpu rows (0, 1), budgets (0), an rpc member spec with gpu 0,
		// a head with two members on two servers and their gpu rows
		// migrate to 119, then assert:
		//  - every seeded row survives with gpu_vendor = ''
		//  - rpc_cluster_member_gpus.member_position equals its member's position
		//  - ('nvidia', 0) and ('amd', 0) can now coexist on one spec / budget server / member spec / member
		//  - two members of one head on the same server insert fine (positions 0 and 1)
		//  - deleting a member row cascades its gpu rows; deleting the head cascades everything
		//  - ai_servers.gpu_legacy_vendor exists, default ''; ai_servers.gpu_vendor_note exists, default 0
		//  - re-running Migrate is a no-op
	})
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ -run TestMigration119 -v` — Expected: FAIL (migration missing). Repeat with `OP_AI_GATEWAY_TEST_POSTGRES_DSN=<dsn>`.
- [ ] **Step 3: Implement** per Interfaces. In `schema_column_coverage_test.go` and `ai_server_column_parity_test.go` classify `ai_servers.gpu_legacy_vendor` and `ai_servers.gpu_vendor_note` as **ignored** with the reason "struct fields land in E2" (E1 must not mark them `seeded`: `AIServer` has no such fields yet); E2 moves both to `seeded`. Extend the coverage test for the other new columns the same way if it enumerates these tables' columns.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/store/ ./internal/routing/ && OP_AI_GATEWAY_TEST_POSTGRES_DSN=<dsn> go test ./internal/store/` and `go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS on both dialects.
- [ ] **Step 5: Commit** `feat(store): add gpu_vendor to GPU keys and key cluster members by position (#434)` — body: migration 119 (rawUp: SQLite table rebuilds with foreign keys off on a dedicated connection; Postgres constraint swaps by looked-up name); existing rows keep vendor '' = the host's only vendor until the gateway backfills them; member GPU rows reference their member by (head, position) so one server may serve a head twice.

### Task E2: Routing structs, store interface and all three drivers carry the vendor and the member position

**Files:**
- Create: `gateway/backend/internal/routing/gpu_key.go`, `gpu_key_test.go`
- Modify: `gateway/backend/internal/routing/store.go` (`GPUSample :809`, `RuntimeSpecGPU :3265`, `ServerGPUBudget :3375`, `UpdateRuntimeSpecGPUMeasured :3499`, `RPCMemberSpecGPU :3804`, `ClusterMember :3817` doc, `ClusterMemberGPU :3838`, `EndClusterMemberRun :3929`, `AIServer`)
- Modify: `gateway/backend/internal/routing/memory_store.go`, `gateway/backend/internal/store/sqlite_runtime.go`, `sqlite_rpc_cluster.go`, `sqlite_rpc_cluster_atomic.go`, the AI-server row reader/writer
- Modify (compile only, no behaviour change): every caller in `internal/portal` and `internal/gateway` (pass `routing.GPUKey{Vendor: "", Index: i}` / position where the signature changed)
- Regenerate: `cd gateway/backend && go generate ./internal/tracing`
- Create: `gateway/backend/internal/store/gpu_vendor_conformance_test.go`
- Modify: `gateway/backend/internal/store/schema_column_coverage_test.go`, `ai_server_column_parity_test.go` (`ai_servers.gpu_legacy_vendor`, `ai_servers.gpu_vendor_note`: `ignored` → `seeded`, seeded with non-default values)

**Interfaces:**
- Produces:
  - `type GPUKey struct { Vendor string; Index int }`; `func (k GPUKey) String() string`; `const GPUVendorNVIDIA = "nvidia"`, `GPUVendorAMD = "amd"`, `GPUVendorApple = "apple"`; `func ValidGPUVendor(v string) bool` (`""` or one of the three); `func LegacyGPUVendor(vendors []string) string` (first present of nvidia, amd, apple; `""` when none).
  - `GPUSample.Vendor`, `GPUSample.PCIBusID` (JSON-only persistence, no tags; old rows decode empty).
  - `RuntimeSpecGPU.GPUVendor string`, `func (g RuntimeSpecGPU) Key() GPUKey`; `ServerGPUBudget.GPUVendor`; `RPCMemberSpecGPU.GPUVendor`; `ClusterMemberGPU.GPUVendor`, `ClusterMemberGPU.MemberPosition int` (`ServerID` kept and still written).
  - `AIServer.GPULegacyVendor string` (column `gpu_legacy_vendor`), `AIServer.GPUVendorNote bool` (column `gpu_vendor_note`), read and written by the AI-server row reader/writer in all three drivers.
  - `UpdateRuntimeSpecGPUMeasured(ctx context.Context, specID string, key GPUKey, measuredMB int) error` (exact `(spec_id, gpu_vendor, gpu_index)` match; ErrNotFound otherwise).
  - `EndClusterMemberRun(ctx context.Context, headSpecID string, position int, startedAt time.Time) (bool, error)`; `UpdateClusterMemberUsage` keeps its signature and matches the row by `m.Position` (and its GPU rows by `(MemberPosition, GPUVendor, GPUIndex)`).
  - Ordering: spec GPUs `order by position, gpu_vendor, gpu_index`; budgets `order by gpu_vendor, gpu_index`; member GPUs `order by member_position, position, gpu_vendor, gpu_index`.

- [ ] **Step 1: Write the failing tests** (`forEachRoutingStore`, memory + sqlite + postgres)

```go
func TestConformanceGPUVendorRoundTrip(t *testing.T) {
	// SetRuntimeSpecGPUs with {nvidia 0, pos 0}, {amd 0, pos 1} -> both read back in order with vendors
	// UpdateRuntimeSpecGPUMeasured(spec, {amd,0}, 3323) touches only the AMD row; {"",0} -> ErrNotFound
	// SetServerGPUBudgets with nvidia 0 and amd 0 -> both stored
	// ReplaceRPCMemberSpec with gpus nvidia 0 + amd 0 -> both stored
}
func TestConformanceTwoMembersOnOneServer(t *testing.T) {
	// SetClusterMembers(head, [{pos 0, srv_a}, {pos 1, srv_a}], gpus [{MemberPosition 0, nvidia 0}, {MemberPosition 1, amd 0}])
	// ClusterMembers returns both with their own gpu rows; UpdateClusterMemberUsage on position 1 leaves position 0 untouched;
	// EndClusterMemberRun(head, 1, run) ends only position 1
}
func TestLegacyGPUVendor(t *testing.T) {
	if LegacyGPUVendor([]string{"amd", "nvidia"}) != "nvidia" || LegacyGPUVendor([]string{"apple"}) != "apple" || LegacyGPUVendor(nil) != "" { t.Fatal() }
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ ./internal/routing/ -run 'GPUVendor|TwoMembersOnOneServer|LegacyGPUVendor' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces. In `sqlite_rpc_cluster.go` every membership/GPU `where head_spec_id = ? and server_id = ?` that addresses one member (run begin/end, usage update, peak reset, the GPU-row update) moves to `head_spec_id = ? and position = ?` / `member_position = ?`; `ClusterMembershipsByServer` and the session queries stay server-keyed. Update the existing rpc-cluster conformance tests that call `EndClusterMemberRun` with a server id.
- [ ] **Step 4: Run** `cd gateway/backend && go generate ./internal/tracing && go build ./... && go test ./... && OP_AI_GATEWAY_TEST_POSTGRES_DSN=<dsn> go test ./internal/store/ && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): GPU rows carry their vendor, member rows are addressed by position (#434)` — body: the routing structs, interface and all three drivers; callers compile with vendor '' and keep today's behaviour until the ingest and portal commits.

### Task E3: One-time backfill of empty vendors

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (interface), `memory_store.go`, `gateway/backend/internal/store/sqlite_runtime.go` (or a new `sqlite_gpu_vendor.go`)
- Test: `gateway/backend/internal/store/gpu_vendor_conformance_test.go`

**Interfaces:**
- Produces:
  - `BackfillServerGPUVendor(ctx context.Context, serverID, vendor string, noteMixed bool) (rewritten int, err error)` — in one transaction rewrites `gpu_vendor = ''` to `vendor` in: spec GPU rows of the server's specs (`agent_runtime_spec_gpus` → `agent_runtime_specs.mapping_id` → `model_mappings.application_id` → `applications.server_id`), `ai_server_gpu_budgets` of the server, `rpc_member_spec_gpus` of member specs on the server (`rpc_member_specs.server_id`), `rpc_cluster_member_gpus` with `server_id` = the server. Where an explicit row with the same key already exists, the empty-vendor row is deleted instead (explicit wins). On **every** call it sets `ai_servers.gpu_legacy_vendor = vendor` (also when `rewritten == 0`); when `rewritten > 0 && noteMixed` it also sets `ai_servers.gpu_vendor_note = 1`. `vendor` must be non-empty and valid (else error, nothing written). Idempotent: a second call rewrites 0 and leaves the note flag as it is.
  - `ClearServerGPUVendorNote(ctx context.Context, serverID string) error` — sets `gpu_vendor_note = 0`, keeps `gpu_legacy_vendor` (ErrNotFound for an unknown server).

- [ ] **Step 1: Write the failing tests** (`forEachRoutingStore`)

```go
func TestConformanceBackfillServerGPUVendor(t *testing.T) {
	// server A rows with '' in all four tables, server B rows with '' -> Backfill(A, "nvidia", true)
	// -> A's rows all 'nvidia', B's untouched, rewritten == number of A rows, A.GPULegacyVendor == "nvidia" && A.GPUVendorNote
	// second call -> rewritten 0, note flag unchanged (true)
	// a spec holding ('',0) and ('nvidia',0) -> after backfill one row ('nvidia',0) with the explicit row's estimate
	// noteMixed=false (server B) -> B.GPULegacyVendor == "nvidia" but B.GPUVendorNote == false
	// ClearServerGPUVendorNote(A) -> A.GPUVendorNote == false, A.GPULegacyVendor still "nvidia"; a further Backfill(A) rewrites 0 and does not raise the note again
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ -run Backfill -v` (and with the Postgres DSN) — Expected: FAIL.
- [ ] **Step 3: Implement**; regenerate tracing wrappers.
- [ ] **Step 4: Run** `cd gateway/backend && go generate ./internal/tracing && go test ./... && OP_AI_GATEWAY_TEST_POSTGRES_DSN=<dsn> go test ./internal/store/ && go test ./internal/archtest/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(store): backfill legacy empty GPU vendors per server (#434)` — body: rows written before vendors existed mean "the host's only vendor"; the first vendor-reporting hardware report makes that explicit once, and records when the host shows more than one vendor so the portal can ask for a review.

---

## Phase F — Gateway ingest, DTOs, feature gating, measurability

### Task F1: Ingest carries vendor, PCI bus id, measurability and Vulkan ordinals

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`agentRuntimeGPUSample :143`, `agentGPUReport :1994`, `agentGPUInfo :2055`, `agentSystemReport`, `sanitizeSystemReport :2527`, telemetry → `routing.GPUSample` mapping)
- Modify: `gateway/backend/internal/gateway/runtime_registry.go:112` (`RuntimeGPUStatusDTO`), `perf_endpoints.go:43` (`perfGPUDTO`)
- Test: `gateway/backend/internal/gateway/agent_ingest_test.go`, `perf_endpoints_test.go`

**Interfaces:**
- Produces: `agentGPUReport.Vendor \`json:"vendor"\``, `.PCIBusID \`json:"pci_bus_id"\``; `agentGPUInfo.Vendor \`json:"vendor,omitempty"\``, `.VRAMMeasurable []string \`json:"vram_measurable,omitempty"\``, `.VulkanOrdinal *int \`json:"vulkan_ordinal,omitempty"\``; `agentSystemReport.VulkanDeviceCount int \`json:"vulkan_device_count,omitempty"\``; `agentRuntimeGPUSample.Vendor`; `RuntimeGPUStatusDTO.Vendor \`json:"vendor,omitempty"\``; `perfGPUDTO.Vendor \`json:"vendor,omitempty"\``. Sanitizer: vendor lower-cased, kept only if `routing.ValidGPUVendor` and non-empty else `""`; `vram_measurable` filtered to `cuda`/`rocm`/`vulkan`, de-duplicated, sorted; negative `vulkan_ordinal` → nil; count clamped ≥ 0.

- [ ] **Step 1: Write the failing tests**

```go
func TestSanitizeSystemReportKeepsGPUVendorFields(t *testing.T) {
	// {vendor:"AMD", vram_measurable:["vulkan","rocm","bogus","rocm"], vulkan_ordinal:1} -> vendor "amd", ["rocm","vulkan"], ordinal 1
	// {vendor:"intel", vulkan_ordinal:-3} -> vendor "", ordinal absent
}
func TestTelemetryGPUSampleCarriesVendorAndPCI(t *testing.T) { /* stored GPUSample has Vendor and PCIBusID */ }
func TestRuntimeStatusGPUCarriesVendor(t *testing.T) { /* RuntimeGPUStatusDTO.Vendor round-trips */ }
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway/ -run 'GPUVendorFields|CarriesVendor' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/gateway/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(gateway): ingest GPU vendor, PCI address, measurability and Vulkan ordinals (#434)`.

### Task F2: Measured VRAM write-back, instance folding and the VRAM benchmark keyed by GPU key

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`foldInstanceRows :817` `MaxGPU`, `writeBackRuntimeVRAM :1001`, `storedMeasuredVRAM`)
- Modify: `gateway/backend/internal/gateway/benchmark_vram.go:269` (`VRAMGPUItem.Vendor`), `benchmark_vram_sampling.go` (`vramGPUByIndex` → `vramGPUByKey`, `vramCardSpan`, `vramWatchedIndexes` → `vramWatchedKeys`), `benchmark_vram_confidence.go`, `benchmark_vram_runner.go` (`vramFinalizeGPUs`, `vramReportItems`, `vramAwaitMeasured`, `vramMeasuredFromFrame`), `benchmark_vram_isolation.go`
- Test: the matching `_test.go` files

**Interfaces:**
- Consumes: `routing.GPUKey`, `UpdateRuntimeSpecGPUMeasured(ctx, specID, key, mb)` (E2), `RuntimeGPUStatusDTO.Vendor` (F1).
- Produces: `foldedSpec.MaxGPU map[routing.GPUKey]int`; `func vramGPUByKey(sample routing.TelemetrySample) map[routing.GPUKey]routing.GPUSample`; `func vramWatchedKeys(specGPUs []routing.RuntimeSpecGPU, sample routing.TelemetrySample) (keys []routing.GPUKey, attributable bool)`; `VRAMGPUItem.Vendor string \`json:"vendor,omitempty"\``; every `map[int]int` measured map → `map[routing.GPUKey]int`.

- [ ] **Step 1: Write the failing tests**

```go
func TestWriteBackRuntimeVRAMPerVendor(t *testing.T) {
	// a frame with {nvidia 0: 7874, amd 0: 3323} for one spec holding rows nvidia 0 and amd 0
	// -> each row gets its own value (today the second overwrote the first)
}
func TestWriteBackVendorlessFrameUsesLegacyVendor(t *testing.T) {
	// server GPULegacyVendor "nvidia" (backfilled), spec rows {nvidia 0}, {amd 0}; a downgraded agent's frame {Vendor "", Index 0, 7874}
	// -> the nvidia 0 row reads 7874 afterwards, the amd 0 row keeps its old value (exact-key write would be ErrNotFound for every row)
}
func TestVRAMGPUByKeyKeepsCollidingIndices(t *testing.T) { /* sample with nvidia 0 and amd 0 -> two entries */ }
func TestVRAMReportItemsCarryVendor(t *testing.T) { /* items for amd 1 carry vendor "amd" */ }
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway/ -run 'PerVendor|ByKey|ItemsCarryVendor' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.** A frame without a vendor (an agent that lacks `gpu_vendor_identity`) resolves `""` to the server's stored `AIServer.GPULegacyVendor` (E2/E3) before `foldInstanceRows` and `UpdateRuntimeSpecGPUMeasured` (the rows were backfilled to that vendor); with none stored the key stays `("", index)` (rows never backfilled).
- [ ] **Step 4: Run** `cd gateway/backend && go test ./internal/gateway/ && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(gateway): measured VRAM and the VRAM benchmark per (vendor, index) (#434)`.

### Task F3: Portal DTOs carry the vendor; only flagged agents receive it

**Files:**
- Create: `gateway/backend/internal/portal/gpu_vendor.go`, `gpu_vendor_test.go`
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`RuntimeSpecGPUDTO :506`, `validateRuntimeSpecGPUs :2543`, `GPUBudgetDTO :2881`, `SetServerGPUBudgets :2920`, `AgentRuntimeSpecGPUDTO :3527`, `AgentGPUBudgetDTO :3689`, `AgentRuntimeConfig :3801`, `specAsPushed :4352`, feature constants near `:4286`), `service_cluster.go` (`ClusterMemberGPUDTO :112`), `service_rpc_members.go` (member spec GPU DTOs, `:511-524`), `service_cluster_config.go` (`memberAgentSpec`)
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (`portalRuntimeSpecErrRows`), `portal_server_endpoints.go` (`portalServerErrRows`, used by the budget save), `agent_features.go` (comment only: the gateway declares nothing new)
- Test: `gateway/backend/internal/portal/service_runtime_gpu_vendor_test.go` (new), `gateway/backend/internal/gateway/portal_runtime_endpoints_test.go`

**Interfaces:**
- Produces:
  - `const AgentFeatureGPUVendorIdentity = "gpu_vendor_identity"`, `const AgentFeatureRPCClusterSameHost = "rpc_cluster_same_host"`.
  - Errors: `ErrRuntimeSpecGPUVendorInvalid = errors.New("runtime_spec.gpu_vendor_invalid")` (400), `ErrRuntimeSpecGPUVendorUnsupported = errors.New("runtime_spec.gpu_vendor_unsupported")` (400: a non-empty vendor for a server whose agent is known to lack `gpu_vendor_identity`).
  - `Vendor string \`json:"vendor"\`` on `RuntimeSpecGPUDTO`, `GPUBudgetDTO`, `ClusterMemberGPUDTO`, the RPC member spec GPU DTO; `Vendor string \`json:"vendor,omitempty"\`` on `AgentRuntimeSpecGPUDTO` and `AgentGPUBudgetDTO`.
  - `func (s *Service) agentHasGPUVendors(serverID string) (has, known bool)` (the `agentRunsWSL` pattern; nil reader → `(false, true)`).
  - `func (s *Service) serverLegacyGPUVendor(ctx context.Context, serverID string) (string, error)` — the persisted `AIServer.GPULegacyVendor` (E2/E3), **not** derived from the latest report: an old agent's vendor-less report replaces the stored report but must not change it. `""` when none was ever stored.
  - `func normalizeGPUVendor(vendor string, flagged, known bool, legacy string) (string, error)` — applied to every GPU of a spec, budget list, RPC member spec and cluster member on write: lower-cased; not `routing.ValidGPUVendor` → `ErrRuntimeSpecGPUVendorInvalid`; non-empty while `known && !flagged` → `ErrRuntimeSpecGPUVendorUnsupported`; empty while `flagged` and `legacy != ""` → `legacy`. Afterwards the existing duplicate check (`validateRuntimeSpecGPUs` and its member/budget siblings) compares `routing.GPUKey{vendor, index}` instead of the index.
  - `func pushedGPUVendor(vendor string, flagged bool, legacy string) (out string, keep bool)` — flagged: `(vendor, true)`; unflagged: `("", vendor == "" || (legacy != "" && vendor == legacy))` — with no stored legacy vendor only empty-vendor rows are kept. Applied to spec GPUs, budgets and member GPUs in `AgentRuntimeConfig`; a row whose index was already emitted for that spec/budget list is dropped, so an unflagged agent never sees two rows with one index.
  - `latestGPUSnapshotByIndex` → `latestGPUSnapshotByKey(ctx, serverID) map[routing.GPUKey]routing.GPUSample`; budget drift `expected_uuid`/`expected_name` snapshot and comparison by key.

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentRuntimeConfigSendsVendorOnlyToFlaggedAgents(t *testing.T) {
	// flagged: spec gpu {amd 1} -> AgentRuntimeSpecGPUDTO{Vendor:"amd", Index:1}; JSON has "vendor":"amd"
	// unflagged: JSON has no "vendor" key
}
func TestAgentRuntimeConfigUnflaggedAgentDropsForeignVendorRows(t *testing.T) {
	// server GPULegacyVendor "nvidia"; the stored report is vendor-less (an old agent replaced it) -- the result must not depend on it;
	// spec rows {nvidia 0, amd 0}; unflagged agent -> exactly one GPU entry, Index 0, no "vendor" key (the nvidia row)
}
func TestAgentRuntimeConfigUnflaggedAgentWithoutStoredLegacyVendor(t *testing.T) {
	// GPULegacyVendor ""; rows {nvidia 0, amd 0, "" 1} -> unflagged agent gets only index 1 (the empty-vendor row)
}
func TestAgentRuntimeConfigVendorStableAcrossBackfill(t *testing.T) {
	// flagged agent, rows '' then backfilled to 'nvidia': the pushed document's GPU entries differ only by "vendor":"nvidia",
	// which the agent resolves identically (A2) — assert the DTO pair and document the agent-side guarantee in a comment
}
func TestSaveRuntimeSpecVendorRules(t *testing.T) {
	// "AMD" -> stored "amd"; "intel" -> runtime_spec.gpu_vendor_invalid; "amd" on unflagged -> runtime_spec.gpu_vendor_unsupported;
	// "" on flagged with legacy nvidia -> stored "nvidia"; {nvidia 0, amd 0} accepted; {amd 0, amd 0} -> duplicate error
}
func TestVendorErrorsAreMappedInEveryTable(t *testing.T) {
	for _, tbl := range [][]errRow{portalRuntimeSpecErrRows, portalServerErrRows, portalRPCMemberErrors} { // the row type of writeMappedError's table
		for _, err := range []error{portal.ErrRuntimeSpecGPUVendorInvalid, portal.ErrRuntimeSpecGPUVendorUnsupported} {
			rec := httptest.NewRecorder()
			writeMappedError(rec, err, tbl, http.StatusInternalServerError, "x", "x")
			if rec.Code != http.StatusBadRequest { t.Fatalf("%v -> %d", err, rec.Code) }
		}
	}
}
func TestBudgetDriftComparesSameVendorCard(t *testing.T) {
	// live telemetry nvidia 0 (uuid A) and amd 0 (uuid B); budget {amd 0} snapshots expected_uuid B, not A
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal/ ./internal/gateway/ -run 'Vendor|Drift' -v` — Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces; add the two errors (`status 400`, messages: "unknown GPU vendor", "this server's agent does not support vendor-qualified GPUs yet") to `portalRuntimeSpecErrRows` and `portalServerErrRows` (member spec saves reach both through `portalRPCMemberErrors`'s `slices.Concat`; the budget save goes through `portalServerErrRows`), so no vendor refusal on those saves becomes a 500.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): vendor-qualified GPUs in specs, budgets and member GPUs (#434)` — body: the gateway sends vendor only to agents declaring gpu_vendor_identity; an older agent keeps today's single-vendor document; saves of a vendor for such an agent are refused with runtime_spec.gpu_vendor_unsupported.

### Task F4: Backfill trigger on the hardware report; the legacy-mapping note

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go:2597-2610` (after `UpsertServerHardware`)
- Create: `gateway/backend/internal/gateway/portal_server_gpu_note_endpoints.go` (+ test); register the route where the server endpoints are registered
- Modify: `gateway/backend/internal/portal/service.go` or `service_runtime.go` (server DTO fields `gpu_legacy_vendor` and `gpu_vendor_note`, `DismissServerGPUVendorNote`)
- Test: `gateway/backend/internal/gateway/agent_ingest_test.go`, `portal_server_gpu_note_endpoints_test.go`

**Interfaces:**
- Consumes: `BackfillServerGPUVendor`, `ClearServerGPUVendorNote` (E3), `routing.LegacyGPUVendor` (E2).
- Produces:
  - Ingest: when the sanitized report has at least one GPU with a non-empty vendor: `legacy := routing.LegacyGPUVendor(vendors)`, `noteMixed := len(distinct vendors) > 1`, `BackfillServerGPUVendor(ctx, serverID, legacy, noteMixed)` (which persists `gpu_legacy_vendor` on every call); a vendor-less report never calls it and never clears the stored legacy vendor; failures are logged, never fail the report; when `rewritten > 0` the server's runtime document is pushed (the same push the portal uses after a spec save).
  - Portal server DTO: `GPULegacyVendor string \`json:"gpu_legacy_vendor,omitempty"\``, `GPUVendorNote bool \`json:"gpu_vendor_note,omitempty"\``.
  - `func (s *Service) DismissServerGPUVendorNote(ctx context.Context, principal auth.Token, serverID string) error` (calls `ClearServerGPUVendorNote`; manage rights on the server, 404-no-leak like `authorizeServer`).
  - Route `POST /api/portal/servers/{id}/gpu-vendor-note/dismiss` → 204.

- [ ] **Step 1: Write the failing tests**

```go
func TestHardwareReportBackfillsLegacyVendorOnce(t *testing.T) {
	// rows with '' on the server; report with nvidia 0 + amd 0 -> rows 'nvidia', server GPULegacyVendor "nvidia" and GPUVendorNote true, one runtime push
	// the same report again -> nothing rewritten, no push
}
func TestHardwareReportFromOldAgentBackfillsNothing(t *testing.T) { /* GPUs without vendor -> rows stay '' */ }
func TestSingleVendorReportSetsNoNote(t *testing.T) { /* only nvidia -> rows 'nvidia', GPULegacyVendor "nvidia", GPUVendorNote false */ }
func TestVendorlessReportKeepsStoredLegacyVendor(t *testing.T) { /* after a vendor report, a vendor-less report of the same server -> GPULegacyVendor still "nvidia", rows untouched */ }
func TestDismissGPUVendorNote(t *testing.T) { /* POST -> 204, GPUVendorNote false and GPULegacyVendor still set; foreign user -> 404 */ }
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway/ -run 'Backfill|GPUVendorNote|NoNote' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go generate ./internal/tracing && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(gateway): backfill legacy GPU vendors from the first vendor-reporting hardware report (#434)`.

### Task F5: Host GPU model, measurability and save-time backend refusals

**Files:**
- Modify: `gateway/backend/internal/portal/service_cluster.go` (`hostGPU :1039`, `serverHostGPUs :1054`, `checkClusterMemberGPUs`, `unmeasuredNonNVIDIA`, `memberGPUMeasurable :1151`, `knownNonNVIDIA :1164`, `checkAppleMemberBudgets`), `service_rpc_members.go` (`gpuLooksAMD :375`, `gpuLooksNVIDIA :386`, the estimate check `:360-373`), `service_runtime.go` (spec save path), `service_cluster_config.go` (`serverMemoryCeilings :768`: stale-member ceilings and peak-above-budget), `service_cluster.go` (`clusterEntry` fields `gpus`/`estimate` moved here from G1, `budgetBelowDevice :2009`)
- Modify: `gateway/backend/internal/portal/gpu_vendor.go`
- Modify: `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (`portalRuntimeSpecErrRows`), `portal_server_endpoints.go` (`portalServerErrRows`): the two new refusals go into every table the touched saves use, so none becomes a 500
- Test: `gateway/backend/internal/portal/service_cluster_test.go`, `service_rpc_members_test.go`, `service_cluster_config_test.go`, `gpu_vendor_test.go`

**Interfaces:**
- Produces:
  - `hostGPU{ vendor string; nvidia, apple bool; kfdHost bool; totalMB int; measurable []string; hasMeasurable bool }`; `func (s *Service) serverHostGPUs(ctx, serverID string) (map[routing.GPUKey]hostGPU, error)` — key `(report vendor, index)`; vendor-less reports (old agents) keep today's name heuristic and key `("", index)`.
  - `func gpuLooksNVIDIA(vendor, name, pciBusID string) bool` / `func gpuLooksAMD(vendor, name string) bool` — vendor decides when non-empty; heuristic only for `""`.
  - `func resolveSpecBackend(backend string, vendors []string) (resolved string, mismatch bool)` — mirrors C1 rules 1 and 3 (`auto`/`""`: nvidia-only `cuda`, amd-only `rocm`, apple-only `metal`, nvidia+amd `vulkan`, none `auto`).
  - `func gpuMeasurable(g hostGPU, resolvedBackend string) bool` — `hasMeasurable` → `slices.Contains(g.measurable, resolvedBackend)`; else today's `kfdHost` rule. `unmeasuredNonNVIDIA` uses it with the entry's resolved backend.
  - Re-keying of everything that reads `serverHostGPUs` (design §6.2 "every `map[int]…` of GPU data", §6.3 budget checks), done here so F5 does not depend on G1: `clusterEntry.gpus []routing.GPUKey`, `clusterEntry.estimate map[routing.GPUKey]int`; `checkClusterMemberGPUs` looks the entry's keys up in the `map[routing.GPUKey]hostGPU` (vendor-less entries of old agents look up `("", index)`); `serverMemoryCeilings(...) map[routing.GPUKey]int`; `budgetBelowDevice` compares a member GPU with the budget and device of the **same** `(vendor, index)` key. `clusterEntries` keeps its current signature and its server-keyed refusals until G1.
  - `ErrRuntimeSpecGPUVendorBackendMismatch = errors.New("runtime_spec.gpu_vendor_backend_mismatch")` (400), `ErrRuntimeSpecVulkanVisibleDevicesRequired = errors.New("runtime_spec.vulkan_visible_devices_required")` (400): checked on spec save, RPC member spec save and cluster member save. The Vulkan rule applies only when the server's stored report lists both `nvidia` and `amd` and the resolved backend is `vulkan` with GPUs and not (`set_visible_devices` && mode `env`); RPC members are exempt from it (the agent pins them itself, C3).
  - `runtimeSpecVisibleDevicesVars` gains `GGML_VK_VISIBLE_DEVICES` (mirror of C2).

- [ ] **Step 1: Write the failing tests**

```go
func TestResolveSpecBackend(t *testing.T) {
	// ("auto",[nvidia])->cuda; ("auto",[amd])->rocm; ("auto",[nvidia,amd])->vulkan; ("cuda",[nvidia,amd])->mismatch; ("rocm",[nvidia])->mismatch; ("vulkan",[nvidia,amd])->vulkan
}
func TestServerHostGPUsKeysByVendor(t *testing.T) { /* report nvidia 0 + amd 0 -> two entries; amd entry not nvidia */ }
func TestGPULooksNVIDIAPrefersVendor(t *testing.T) { /* vendor "amd" with an NVIDIA-looking name -> false; vendor "" -> heuristic */ }
func TestMemberMeasurabilityFromReport(t *testing.T) {
	// amd GPU vram_measurable ["rocm","vulkan"]: vulkan member needs no estimate; ["rocm"]: vulkan member needs one;
	// report without the list on an all-AMD linux host: today's kfdHost rule
}
func TestServerMemoryCeilingsKeyedByVendor(t *testing.T) {
	// report nvidia 0 (11264 MB) + amd 0 (16384 MB), budgets {nvidia 0: 10000}
	got := svc.serverMemoryCeilings(ctx, "srv_a")
	if got[routing.GPUKey{Vendor: "nvidia", Index: 0}] != 10000 || got[routing.GPUKey{Vendor: "amd", Index: 0}] != 16384 { t.Fatal(got) }
}
func TestBudgetBelowDeviceComparesSameVendorCard(t *testing.T) {
	// member GPU {amd 0} with a 12000 MB estimate: budget {nvidia 0: 8000} does not flag it (other card), budget {amd 0: 8000} does
}
func TestCheckClusterMemberGPUsLooksUpByKey(t *testing.T) {
	// host nvidia 0 + amd 0, only the AMD card unmeasurable for vulkan (vram_measurable ["rocm"]): member {amd 0} needs an estimate, member {nvidia 0} does not
}
func TestSaveSpecRefusesVendorBackendMismatch(t *testing.T) { /* cuda spec with {nvidia 0, amd 0} -> runtime_spec.gpu_vendor_backend_mismatch */ }
func TestSaveSpecRefusesVulkanWithoutEnvVisibilityOnMixedHost(t *testing.T) { /* -> runtime_spec.vulkan_visible_devices_required; single-vendor host -> accepted */ }
func TestSaveSpecRefusesHandSetGGMLVKVisibleDevices(t *testing.T) { /* set_visible_devices + env GGML_VK_VISIBLE_DEVICES -> visible_devices_conflict */ }
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal/ -run 'ResolveSpecBackend|KeysByVendor|LooksNVIDIA|Measurability|Mismatch|VulkanWithoutEnv|GGMLVK' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): vendor-aware host GPUs, reported measurability and backend refusals (#434)`.

---

## Phase G — Gateway: clusters on one host

### Task G1: Same-host members allowed behind the flag; entries keyed by position and GPU key

**Files:**
- Modify: `gateway/backend/internal/portal/service_cluster.go` (`clusterEntries :521`, `newClusterEntry`, `clusterEntry.position`, `storedMemberDTO`, change detection `:617`; the GPU-key fields `gpus`/`estimate` were re-keyed in F5), `service_cluster_plan.go`
- Test: `gateway/backend/internal/portal/service_cluster_test.go`

**Interfaces:**
- Consumes: `AgentFeatureRPCClusterSameHost` (F3).
- Produces:
  - `func clusterEntries(members []ClusterMemberDTO, mode, headServerID string, sameHost func(serverID string) bool, prevMembers []routing.ClusterMember, prevGPUs []routing.ClusterMemberGPU) ([]clusterEntry, error)` — `ErrClusterMemberServerTwice` / `ErrClusterMemberOnHeadServer` only when `!sameHost(serverID)`; prior stored member matched by position.
  - `clusterEntry.position int` (`gpus []routing.GPUKey` and `estimate map[routing.GPUKey]int` already exist from F5).
  - `func (s *Service) agentRunsSameHostClusters(serverID string) bool` (known and declares `rpc_cluster_same_host`; nil reader → false).
  - The existing `cluster_member_gpu_overlap` warning compares GPU keys (head and member on one server sharing a card) and stays a warning.

- [ ] **Step 1: Write the failing tests**

```go
func TestClusterEntriesAllowHeadServerWithFlag(t *testing.T) { /* member on head server, flagged -> ok; unflagged -> ErrClusterMemberOnHeadServer */ }
func TestClusterEntriesAllowServerTwiceWithFlag(t *testing.T) { /* two members on srv_a, flagged -> two entries positions 0,1 */ }
func TestClusterOverlapWarningOnSameHostSharedCard(t *testing.T) { /* head gpus {nvidia 0}, member on head server {nvidia 0} -> warning cluster_member_gpu_overlap, save succeeds */ }
func TestClusterOverlapIgnoresOtherVendorSameIndex(t *testing.T) { /* head {nvidia 0}, member {amd 0} -> no warning */ }
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal/ -run 'ClusterEntries|ClusterOverlap' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.** Until G2 lands, two members on one server would share an automatic member id; G1 and G2 ship in the same PR and the branch is not pushed between them.
- [ ] **Step 4: Run** `cd gateway/backend && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): allow RPC members on the head's server and several per server (#434)`.

### Task G2: Loopback addressing, member ids, runtime documents

**Files:**
- Modify: `gateway/backend/internal/portal/service_cluster_config.go` (`clusterAddress :531`, `memberBindIP :558`, `clusterHeadIdentity`, `buildCluster`, `memberAgentSpec :700-735`, `memberGenerationPart`, readiness/NetBird/LAN checks), `service_cluster_usage.go` (`automaticMemberAgentSpecID :23`, `MemberAgentSpecID`), `service_netbird_rpc_policies.go` (exclude loopback members), `service_rpc_member_names.go:61` (a `MemberAgentSpecID(m)` caller without the member list: load `ClusterMembers(head)` there), `service_cluster.go` (skip the `ErrClusterLANAckRequired` check for same-host members)
- Test: `gateway/backend/internal/portal/service_cluster_config_test.go`, `service_cluster_usage_test.go`, `service_netbird_rpc_policies_test.go`

**Interfaces:**
- Produces:
  - `const loopbackAddr = "127.0.0.1"`; `func sameHostMember(headServerID string, m routing.ClusterMember) bool`.
  - For a same-host member: dial address `127.0.0.1`, `BindIP "127.0.0.1"`, `Transport "direct"`, `HeadIPs ["127.0.0.1"]`, `HeadDomain ""`; no mesh/LAN readiness reason, no LAN-ack requirement, no NetBird policy entry. Members on other servers unchanged.
  - `func automaticMemberAgentSpecID(headSpecID, serverID string, nth int) string` — `nth <= 1` → `rpca_<head>_<server>`; else `rpca_<head>_<server>_m<nth>`.
  - `func MemberAgentSpecID(m routing.ClusterMember, members []routing.ClusterMember) string` — `nth` = 1 + number of automatic members of the same head on the same server with a lower position.

- [ ] **Step 1: Write the failing tests**

```go
func TestMemberAgentSpecIDStableForSingleMemberPerServer(t *testing.T) {
	m := routing.ClusterMember{HeadSpecID: "rts_h", ServerID: "srv_a", Position: 3}
	if MemberAgentSpecID(m, []routing.ClusterMember{m}) != "rpca_rts_h_srv_a" { t.Fatal() }
}
func TestMemberAgentSpecIDSecondMemberOnServer(t *testing.T) { /* positions 0 and 2 on srv_a -> ids rpca_rts_h_srv_a and rpca_rts_h_srv_a_m2 */ }
func TestSameHostMemberUsesLoopback(t *testing.T) {
	// head on srv_a (mesh network), member on srv_a: endpoint "127.0.0.1:<port>", member DTO BindIP 127.0.0.1, Transport direct,
	// HeadIPs ["127.0.0.1"]; NetBird disabled on srv_a does not withhold it; a member on srv_b keeps its mesh address
}
func TestNetbirdPoliciesSkipLoopbackMembers(t *testing.T) {
	// head on srv_a with members: pos 0 on srv_a (loopback), pos 1 on srv_b; build the clusters like reconcileRPCPolicies does
	rules := desiredRPCRules(clusters) // keyed by member server
	if len(rules["srv_a"]) != 0 || len(rules["srv_b"]) != 1 { t.Fatalf("%+v", rules) }
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/portal/ -run 'MemberAgentSpecID|SameHostMember|Loopback' -v` — Expected: FAIL.
- [ ] **Step 3: Implement**; every caller of `MemberAgentSpecID` passes the head's member list (gateway `cluster_usage.go`, portal `service_rpc_member_names.go`).
- [ ] **Step 4: Run** `cd gateway/backend && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal): address same-host RPC members on loopback (#434)` — body: a member on the head's own server is dialed and bound on 127.0.0.1, always direct, outside NetBird and LAN exposure; ids of existing single-member-per-server clusters do not change, so no member relaunches.

### Task G3: Cluster usage, peaks and the usage reset keyed by position

**Files:**
- Modify: `gateway/backend/internal/gateway/cluster_usage.go` (`member :266`, `gpusOf :279`, `raise`, `measuredVRAM :325`, `raiseMeasured :338`, `runEnded :401`, `endMemberRun :434`, `reset :492`, `heldRow :241`)
- Modify: `gateway/backend/internal/portal/service_cluster_usage.go` (`AuthorizeClusterMemberUsageReset`), `service_cluster_plan.go` (`ClusterPeakAboveBudgetDTO.GPUVendor`), `gateway/backend/internal/gateway/portal_runtime_endpoints.go:101-115`, `portal_mapping_endpoints.go:94`
- Regenerate: `go generate ./internal/tracing` (portal API wrapper)
- Test: `gateway/backend/internal/gateway/cluster_usage_test.go`, `portal_runtime_endpoints_test.go`

**Interfaces:**
- Produces:
  - `func (u *clusterUsage) member(ctx context.Context, head string, position int) (routing.ClusterMember, []routing.ClusterMemberGPU, bool, error)`; `func gpusOf(gpus []routing.ClusterMemberGPU, position int) []routing.ClusterMemberGPU`; `func measuredVRAM(row RuntimeStatusDTO) map[routing.GPUKey]int`; `func (u *clusterUsage) reset(ctx context.Context, head string, position int) error`.
  - `AuthorizeClusterMemberUsageReset(ctx context.Context, principal auth.Token, mappingID, serverID string, position *int) (routing.ClusterMember, error)` — `position` nil and several members of the head on `serverID` → `ErrClusterMemberAmbiguous = errors.New("runtime_spec.cluster_member_ambiguous")` (409); a position whose member is not on `serverID` → `ErrClusterMemberNotFound`.
  - Endpoint reads optional query `position` (non-negative integer; malformed → **404** `runtime_spec.cluster_member_not_found`, the code's table status; no new code).
  - `ClusterPeakAboveBudgetDTO.GPUVendor string \`json:"gpu_vendor,omitempty"\``.
  - `rpc_sessions` and the hold/session sync stay keyed by server (builder's call).

- [ ] **Step 1: Write the failing tests**

```go
func TestClusterUsageTwoMembersOnOneServer(t *testing.T) {
	// head with members pos 0 (rpca_h_srv_a) and pos 1 (rpca_h_srv_a_m2) on srv_a; one frame of srv_a with both held rows
	// -> each membership row gets its own RAM/VRAM; ending the run ends both by position
}
func TestResetUsageAmbiguousWithoutPosition(t *testing.T) { /* 409 runtime_spec.cluster_member_ambiguous; with ?position=1 -> 204, only pos 1 reset */ }
func TestResetUsageSingleMemberWithoutPositionUnchanged(t *testing.T) {
	// head with one member on srv_a at position 3 with RAM/VRAM peaks > 0; POST .../cluster-members/srv_a/reset-usage without ?position
	if rec.Code != http.StatusNoContent { t.Fatal(rec.Code) }
	if m := memberAt(t, store, head, 3); m.RAMPeakMB != 0 || m.VRAMPeakMB != 0 { t.Fatalf("%+v", m) } // today's behaviour
}
func TestResetUsageMalformedPositionIs404(t *testing.T) {
	// ?position=abc -> 404 with code runtime_spec.cluster_member_not_found
}
```

- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/gateway/ -run 'ClusterUsageTwoMembers|ResetUsage' -v` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/backend && go generate ./internal/tracing && go test ./... && golangci-lint fmt --diff && golangci-lint run` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(gateway): key cluster member usage by position (#434)`.

---

## Phase H — Portal UI

### Task H1: API types, GPU key helpers, error labels and i18n keys

**Files:**
- Create: `gateway/frontend/src/api/gpus.ts`, `gpus.test.ts`
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeSpecGPU :22`, `ClusterMemberGPU :35`, `ClusterPeakAboveBudget :109`, `GPUBudget :561`, `RuntimeGPUStatus :613`), `src/api/servers.ts` (`PerfGPU :331`, `HardwareGPU :410`, `HardwareReport`, `VRAMGPUItemDTO :580`, `PortalServer.gpu_legacy_vendor?`/`gpu_vendor_note?`, `dismissGpuVendorNote`)
- Modify: `gateway/frontend/src/components/shared/format.ts` (`errorLabelByCode`), `src/i18n.ts` (DE + EN)
- Test: `gateway/frontend/src/api/gpus.test.ts`, `src/i18n.test.ts`

**Interfaces:**
- Produces (`gpus.ts`):
  - `export type GpuVendor = 'nvidia' | 'amd' | 'apple' | '';`
  - `export function gpuKey(vendor: GpuVendor | undefined, index: number): string` → `"amd:1"` (undefined → `''`).
  - `export function vendorLabel(vendor: GpuVendor): string` → `NVIDIA` / `AMD` / `Apple` / `''` (brand names, not translated).
  - `export function gpuDisplayName(vendor: GpuVendor | undefined, index: number): string` → `"AMD 1"`, `"GPU 1"` for `''`.
  - `export function reportVendors(gpus: HardwareGPU[]): GpuVendor[]` (distinct, order nvidia, amd, apple); `export function hostIsMixed(gpus: HardwareGPU[]): boolean`.
  - `export function resolveAutoBackend(backend: DeviceBackend | '', vendors: GpuVendor[]): { resolved: DeviceBackend | null; mismatch: boolean }` (mirror of `resolveSpecBackend`).
- Types: `vendor?: GpuVendor` (**optional** here so `tsc` stays green between H tasks; H3 makes it required) on `RuntimeSpecGPU`, `ClusterMemberGPU`, `GPUBudget`; `vendor?: GpuVendor` on `RuntimeGPUStatus`, `PerfGPU`, `HardwareGPU` (+ `vram_measurable?: ('cuda'|'rocm'|'vulkan')[]`, `vulkan_ordinal?: number`), `VRAMGPUItemDTO`; `gpu_vendor?: GpuVendor` on `ClusterPeakAboveBudget`.
- Error labels: `runtime.gpu_vendor_backend_mismatch` → `errorRuntimeGPUVendorBackendMismatch`, `runtime.vulkan_mapping_unavailable` → `errorRuntimeVulkanMappingUnavailable`, `runtime.vulkan_visible_devices_required` → `errorRuntimeVulkanVisibleDevicesRequired`, `runtime_spec.gpu_vendor_backend_mismatch` → `errorGPUVendorBackendMismatch`, `runtime_spec.vulkan_visible_devices_required` → `errorVulkanVisibleDevicesRequired`, `runtime_spec.gpu_vendor_invalid` → `errorGPUVendorInvalid`, `runtime_spec.gpu_vendor_unsupported` → `errorGPUVendorUnsupported`, `runtime_spec.cluster_member_ambiguous` → `errorClusterMemberAmbiguous`.
- i18n keys (DE + EN, both written by the implementer in the existing tone): the eight error keys above; `runtimeBackendResolvesTo: (backend: string) => string` ("auto → {backend}"); `runtimeBackendMismatchHint`; `hardwareGpuVendor`; `hardwareVulkanOrdinal`; `hardwareVramMeasurable`; `gpuLegacyMappedNote: (vendor: string) => string` (shown when `gpu_vendor_note` is set, with the vendor label of `gpu_legacy_vendor`); `gpuLegacyMappedDismiss`; `clusterMemberSameServerNote` (loopback addressing explained).

- [ ] **Step 1: Write the failing tests**

```ts
describe('gpus', () => {
  it('builds keys and display names', () => {
    expect(gpuKey('amd', 1)).toBe('amd:1');
    expect(gpuDisplayName('nvidia', 0)).toBe('NVIDIA 0');
    expect(gpuDisplayName('', 2)).toBe('GPU 2');
  });
  it('resolves auto like the agent', () => {
    expect(resolveAutoBackend('auto', ['nvidia'])).toEqual({ resolved: 'cuda', mismatch: false });
    expect(resolveAutoBackend('auto', ['nvidia', 'amd'])).toEqual({ resolved: 'vulkan', mismatch: false });
    expect(resolveAutoBackend('cuda', ['nvidia', 'amd'])).toEqual({ resolved: 'cuda', mismatch: true });
    expect(resolveAutoBackend('rocm', ['nvidia'])).toEqual({ resolved: 'rocm', mismatch: true });
  });
  it('detects a mixed host', () => {
    expect(hostIsMixed([{ index: 0, vendor: 'nvidia' } as HardwareGPU, { index: 0, vendor: 'amd' } as HardwareGPU])).toBe(true);
  });
});
```

- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/api/gpus.test.ts` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): GPU vendor types, helpers, error labels and strings (#434)`.

### Task H2: Runtime admin — GPU picker, budgets, drift, backend hint

**Files:**
- Create: `gateway/frontend/src/components/GpuVendorNote.tsx` (`GpuVendorNote({ vendor, onDismiss })`: the legacy-mapping note with its dismiss button; reused by H4)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (`liveGpuVram :895`, `effectiveGpuVram :890`, `formatGpus :934`, `duplicateGpuIndex :1316`, visibility vars list `:1230-1240`, `gpuOptionLabel :1348`, budget editor, drift display, `device_backend` select)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`

**Interfaces:**
- Consumes: H1 helpers.
- Produces: picker options keyed by `gpuKey(vendor, index)`, label `[gpuDisplayName(vendor, index), name, stableGpuHandle(gpu)].join(' · ')`; saving writes `{vendor, index, …}`; `duplicateGpuKey(keys: string[]): string | null` replaces `duplicateGpuIndex`; live VRAM lookup by key; the `device_backend` select shows `t.runtimeBackendResolvesTo(resolved)` under `auto` and blocks save with `t.runtimeBackendMismatchHint` when `mismatch`; `GGML_VK_VISIBLE_DEVICES` added to the client-side owned-vars list; the server header renders `<GpuVendorNote vendor={server.gpu_legacy_vendor} onDismiss={…dismissGpuVendorNote}/>` when `server.gpu_vendor_note` is true (text `t.gpuLegacyMappedNote(vendorLabel(v))`).

- [ ] **Step 1: Write the failing tests**

```tsx
it('lists NVIDIA 0 and AMD 0 as distinct choices and saves the vendor', async () => {
  // hardware report nvidia 0 + amd 0; pick "AMD 0 · AMD Radeon RX 6800"; PUT body gpus == [{vendor:'amd', index:0, ...}]
});
it('shows what auto resolves to and refuses a mismatch', async () => {
  // gpus nvidia 0 + amd 0, backend auto -> "auto → vulkan"; backend cuda -> save disabled with the mismatch hint
});
it('refuses a hand-set GGML_VK_VISIBLE_DEVICES with set_visible_devices', async () => {
  // spec editor with set_visible_devices on and an env row GGML_VK_VISIBLE_DEVICES=0 (render helper of this file)
  expect(screen.getByRole('button', { name: t.save })).toBeDisabled();
  expect(screen.getByText(/GGML_VK_VISIBLE_DEVICES/)).toBeInTheDocument();
});
it('shows and dismisses the legacy-mapping note', async () => {
  const dismiss = vi.fn(async () => undefined);
  // render with server { gpu_vendor_note: true, gpu_legacy_vendor: 'nvidia' } and dismissGpuVendorNote: dismiss
  expect(await screen.findByText(t.gpuLegacyMappedNote('NVIDIA'))).toBeInTheDocument();
  await userEvent.click(screen.getByRole('button', { name: t.gpuLegacyMappedDismiss }));
  expect(dismiss).toHaveBeenCalledWith('srv_1');
});
```

- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/RuntimeAdminSection.test.tsx` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): vendor-qualified GPU picker, budgets and backend hint (#434)`.

### Task H3: Cluster editor and RPC member editor

**Files:**
- Modify: `gateway/frontend/src/components/ClusterHeadEditor.tsx` (`gpuLabel :269`, `memberGpuLabel :277`, `useMemberHardware :294`, server options around `:687-706`, member rows keyed by position, reset-usage call with `position`), `RpcMemberSection.tsx` (`buildBody :141`, GPU picker, `effectiveVramMb :187`)
- Modify: `gateway/frontend/src/api/runtime.ts` (`resetClusterMemberUsage(mappingId, serverId, position)` sends `?position=`; also `vendor?` → `vendor` on `RuntimeSpecGPU`, `ClusterMemberGPU`, `GPUBudget`, fixing every literal `tsc` then reports, incl. `ClusterHeadEditor.tsx:1301`, `RpcMemberSection.tsx:163`, the budget bodies in `RuntimeAdminSection.tsx`)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (the usage-reset call at `:3734` and the `onResetUsage={(serverId) => …}` prop at `:6458` carry the member position)
- Test: `ClusterHeadEditor.test.tsx`, `RpcMemberSection.test.tsx`, `RuntimeAdminSection.test.tsx`, `src/api/runtime.test.ts` (`:385` pins the reset-usage URL: now `…/reset-usage?position=<n>`)

**Interfaces:**
- Produces: the member server select offers the head's own server and already-chosen servers; a member row on the head's server shows `t.clusterMemberSameServerNote`; member GPU options and stored values use `{vendor, index}`; React keys per member are the position; `vendor` is required on the three types from this task on.

- [ ] **Step 1: Write the failing tests**

```tsx
it('offers the head server and the same server twice as members', async () => {
  // head on srv_a, members [{server srv_a, position 0}, {server srv_a, position 1}] (render helper of this file)
  expect(screen.getAllByText(t.clusterMemberSameServerNote)).toHaveLength(2);
  const selects = screen.getAllByLabelText(t.clusterMemberServer);
  expect(within(selects[1]).getByRole('option', { name: 'srv_a' })).toBeInTheDocument();
});
it('stores member GPUs with their vendor', async () => {
  // pick "AMD 1 · …" for member 0 and save
  expect(save.mock.calls[0][0].members[0].gpus).toEqual([expect.objectContaining({ vendor: 'amd', index: 1 })]);
});
it('resets the usage of the second member on a server by position', async () => {
  await userEvent.click(screen.getAllByRole('button', { name: t.clusterResetUsage })[1]);
  expect(reset).toHaveBeenCalledWith('map_1', 'srv_a', 1); // api/runtime.test.ts: the URL ends with reset-usage?position=1
});
it('rpc member editor saves vendor-qualified GPUs', async () => {
  // pick "NVIDIA 0 · …" and save the member spec
  expect(save.mock.calls[0][0].gpus).toEqual([expect.objectContaining({ vendor: 'nvidia', index: 0 })]);
});
```

- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/ClusterHeadEditor.test.tsx src/components/RpcMemberSection.test.tsx` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): same-host cluster members and vendor-qualified member GPUs (#434)`.

### Task H4: Hardware table — vendor, Vulkan ordinal, measurability, note

**Files:**
- Modify: `gateway/frontend/src/components/HardwareSection.tsx`
- Test: `gateway/frontend/src/components/HardwareSection.test.tsx`

**Interfaces:**
- Produces: GPU rows show `gpuDisplayName(vendor, index)`, a Vendor column, `Vulkan #<n>` when `vulkan_ordinal` is present, the measurable backends (`vram_measurable.join(', ')`, `—` when absent); the legacy-mapping note with dismiss (`GpuVendorNote`, created in H2, reused).

- [ ] **Step 1: Write the failing tests**

```tsx
it('renders vendor and Vulkan ordinal', async () => {
  // report GPU { index: 1, vendor: 'amd', vulkan_ordinal: 2, vram_measurable: ['rocm', 'vulkan'] }
  expect(await screen.findByText(/AMD 1/)).toBeInTheDocument();
  expect(screen.getByText('Vulkan #2')).toBeInTheDocument();
  expect(screen.getByText('rocm, vulkan')).toBeInTheDocument();
});
it('renders without the new fields (old agent)', async () => {
  // report GPU { index: 0 } only
  expect(await screen.findByText(/GPU 0/)).toBeInTheDocument();
  expect(screen.queryByText(/Vulkan #/)).not.toBeInTheDocument();
});
it('shows the legacy note', async () => {
  // server { gpu_vendor_note: true, gpu_legacy_vendor: 'nvidia' }
  expect(await screen.findByText(t.gpuLegacyMappedNote('NVIDIA'))).toBeInTheDocument();
});
```
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/HardwareSection.test.tsx` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): show GPU vendor, Vulkan ordinal and measurability in the hardware table (#434)`.

---

## Phase I — Architecture documentation

### Task I1: Fold the design into `docs/architecture/`

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (new `## ADR-122 — Mixed-vendor GPU hosts: (vendor, index) identity, DRM-fdinfo measurement, per-spec visibility, Vulkan ordinal mapping, same-host RPC members` after ADR-121; ADR-113 gets a "Superseded in part by ADR-122" note on its NVIDIA-wins and rank==cardN statements)
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` §3.3 (per-spec table incl. `GGML_VK_VISIBLE_DEVICES`, the operator-visible change for `vulkan` specs and literal `--device VulkanN`), §3.6/§3.7 (vendor-aware members, loopback members, member ids, position keys), §5.1 (GPU key), §5.3 (composite measurer, fdinfo, noise floor, KFD fallback, `vram_measurable`), a mixed-host subsection (the three codes, legacy vendor, backfill, flags)
- Modify: `docs/architecture/reference/data-model.md` (four tables' columns/keys, `rpc_cluster_members` key, `ai_servers.gpu_legacy_vendor` and `ai_servers.gpu_vendor_note`, migration 119 row in the history table, count "119 migrations")
- Modify: `docs/architecture/reference/api-surface.md` + `reference/openapi.yaml` (DTO `vendor` fields, hardware fields, the five portal codes and three agent codes, `?position=` on reset-usage, `POST /api/portal/servers/{id}/gpu-vendor-note/dismiss`, server DTO `gpu_legacy_vendor` / `gpu_vendor_note`)
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` §8.2.2 (`vendor`, `pci_bus_id`), §8.2.5 (`vendor`, `vram_measurable`, `vulkan_ordinal`, `vulkan_device_count`)
- Modify: `docs/architecture/cross-cutting/persistence.md` (migration 119 as the second rawUp rebuild; constraint lookup by name on Postgres)
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (the "Backend device enumeration order" item: resolved for Vulkan where the mapping exists, CUDA_DEVICE_ORDER caveat remains, mixed host refuses instead of guessing; the "AMD per-process VRAM" item: rank via PCI join, Vulkan now measured via fdinfo, residual: ~130 MiB fdinfo/KFD gap, no mixed host verified yet)
- Modify: `docs/architecture/12-glossary.md` (`Device backend`: auto per spec from its GPUs' vendors; new entries `GPU key`, `Legacy GPU vendor`)

- [ ] **Step 1:** Write the ADR (context: #434 and design §3 facts incl. V1–V4; decision: the five parts; consequences: the deliberate behaviour changes, the second flag, what stays unverified until a mixed host exists).
- [ ] **Step 2:** Update the listed documents; remove statements made false (e.g. "auto on a mixed host is the host's vendor set", "rank equals the rocm-smi card index", "pci_bus_id is not an identity — nothing matches on it").
- [ ] **Step 3: Run** `make lint-docs` — Expected: PASS (links, anchors, index reachability, openapi `$ref`s).
- [ ] **Step 4: Commit** `docs: ADR-122 and architecture updates for mixed NVIDIA+AMD hosts (#434)`.

---

## Phase J — Verification on real hardware (controller, over SSH; design §9)

Manual steps the controller runs with the built agent binary (`cd server-agent && GOOS=linux GOARCH=amd64 go build -o /tmp/op-ai-server-agent .`). Record results in the PR description (numbers only, no host names). If a result contradicts a rule above, stop and fix the owning task before continuing.

### Task J1: Measurers against real processes

- [ ] On the AMD test VM: start a HIP `llama-server` on the card at `0000:02:00.0` and run a small harness (a `go test -run TestManualFdinfo -tags manual` file kept out of the repo, or the agent with debug logging of the measurer) → expect AMD 1 ≈ 3461 MiB, AMD 0 absent. Repeat with the Vulkan build → expect ≈ 3323 MiB (not ≈ 6646), AMD 0 absent. Compare with `rocm-smi` (3611 / 3340 MiB).
- [ ] Confirm the self-probe picks fdinfo (hardware report `vram_measurable: ["rocm","vulkan"]` on both AMD GPUs).
- [ ] With the privileged-helper setup (#438) on the AMD test VM: confirm the agent can read its children's `/proc/<pid>/fdinfo`; if not, confirm the measurement is absent (estimate stays), never 0, and record it as a known limitation in ADR-122.
- [ ] On the NVIDIA test VM: CUDA and Vulkan `llama-server` → NVIDIA measurement ≈ `memory.used` (Vulkan ≈ 7874 MiB); `vram_measurable: ["cuda","vulkan"]`.

### Task J2: Vulkan ordinal mapping

- [ ] On both VMs: the hardware report's `vulkan_ordinal` per GPU equals the `vulkaninfo` GPU number of the card's PCI address; `vulkan_device_count` counts llvmpipe (3 on the AMD test VM, 2 on the NVIDIA test VM, which has no llvmpipe device).
- [ ] On both VMs: a `vulkan` spec with `set_visible_devices` (env) on host GPU 1 then on GPUs `[1, 0]` → the child's `--list-devices` shows `Vulkan0` = the declared first card (V1 re-confirmed on NVIDIA).

### Task J3: Same-host cluster

- [ ] On one VM: head on card 0 (CUDA on the NVIDIA test VM, ROCm on the AMD test VM), automatic member on the same server with card 1 → member launched with `-H 127.0.0.1`, head `--rpc 127.0.0.1:<port>`, cluster forms, the member's hold appears only once the head connects (not during the readiness probe), member usage lands on the membership row, `rpc_sessions` records the server.
- [ ] Not possible until the operator's mixed host exists: a real mixed CUDA+ROCm+Vulkan run. The PR says so explicitly.

---

## Final task: Branch cleanup and full verification

- [ ] **Step 1:** Confirm every durable decision of the design and this plan is in `docs/architecture/` (ADR-122, the docs of Task I1).
- [ ] **Step 2:** `git rm -r docs/superpowers` (and `docs/implementation-status.md` if the branch created one); commit `docs: remove branch-local working files (#434)` with a body naming where their content now lives.
- [ ] **Step 3: Run** the full gates: `make lint`, `make test-go`, `cd server-agent && go test ./internal/archtest/`, `cd gateway/backend && go test ./internal/archtest/ && OP_AI_GATEWAY_TEST_POSTGRES_DSN=<dsn> go test ./internal/store/`, `cd gateway/frontend && npm test && npm run build && npm run lint && npm run format:check`, the Sonar branch findings (`make sonar-scan && make sonar-findings && make sonar-branch-findings`, advisory gate per AGENTS.md). Expected: all green, no new branch-attributed Sonar findings.
- [ ] **Step 4:** `git diff --name-only main...HEAD | grep -E '^docs/superpowers/|^docs/implementation-status.md'` — Expected: no output.

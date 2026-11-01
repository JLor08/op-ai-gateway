# RunSpec RAM Resource Accounting & Measured-Primary Display — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add host RAM as a second, co-equal placement axis alongside per-GPU VRAM — measured per process, persisted per spec, displayed, overridable, and fed into admission + eviction — turn resource accounting on by default with auto-detected budgets, show the *effective* value everywhere, and make measured/manual precedence `max()`.

**Architecture:** VRAM stays **per-GPU** (demand on `SpecGPU.VRAMMB`, budget in `PolicySnapshot.Budgets map[int]int`, occupancy in `RunningProc.GPUs map[int]int`). RAM is a **host scalar** (one value per spec): a new spec-level demand `Spec.RAMMB`, a new occupancy `RunningProc.RAMMB`, and a single host-RAM budget computed live from `MemAvailable − reserve`. Measurement mirrors the existing injected VRAM measurer (`SetMeasurer`) with a parallel RAM measurer. The gateway persists `ram_estimate_mb`/`ram_measured_mb` on `agent_runtime_specs` and pushes an effective `ram_mb` demand to the agent; the agent measures, reports, and admits on both axes. Budgets auto-detect from the hardware sample (per-GPU `MemTotalBytes`, host `MemAvailable`), with operator config as an override.

**Tech Stack:** Go 1.x (two modules: `server-agent`, `gateway/backend`), gopsutil v4 (`mem`, `process`), nvidia-smi / Windows PDH for VRAM, SQLite + Postgres (one `SQLStore` behind a `dialect` seam), TypeScript + React + MUI (`gateway/frontend`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-01-runspec-ram-resource-accounting-design.md` (binding design doc — the why, goals, non-goals, anti-goals, verified code map, decided approach, and the four-phase breakdown this plan expands). Executors read both.

---

## Global Constraints

- **Branching:** feature branch + PR only; never commit to or merge `main`. One PR per phase, each based off `main`, each independently shippable, Auto-fix enabled on each (`ccd_pr set_monitor auto_fix=true`). Do not merge — the user merges after green CI. Do not add attribution lines to commits or PR bodies.
- **Repo-facing text in English** (PRs, issues, commits, docs, code comments). Commit **bodies** are substantive (squash-merge pre-fills its description from the body).
- **No LAN host names** (privacy) in any repo text.
- **Push over SSH** (the `gh` token 403s on push; origin is already SSH — retry over SSH on a 403).
- **The `0` sentinel is load-bearing and stays so.** For VRAM: a GPU budget `<= 0` and an absent budget row both mean *unconstrained* — `Admit` filters on `budget <= 0`, and that check lives in `Admit`, never in `owner.buildSnapshot`. A spec GPU `vram_mb == 0` means *unknown demand* (routes to the unknown-VRAM rules, ADR-032), never a zero-cost claim. RAM follows the same discipline: `ram_mb == 0` = unknown, and `realMeasurement`-style zero-guards drop a measured `0` rather than letting it win. Auto-detected budgets mean a `0` *budget* now rarely occurs, but the interpretation must not change.
- **Agent wire-contract changes require a `Version` bump + a feature-registry entry.** `server-agent/internal/agent/agent.go` `Version` is currently `0.13.0`. Any phase that adds a telemetry or config field the agent produces/consumes bumps `Version` (MINOR) and adds an entry to `agent.Features` (`features.go`) with `Since` = the new `Version`, mirrored in the gateway's `gatewayAgentFeatures` (`gateway/backend/internal/gateway/agent_features.go`); `TestFeatureRegistry` enforces the pairing. **Because the agent `Version` is a frequent serial-PR collision point, merge `origin/main` before finalizing each phase and base the bump on main's then-current version.**
- **Per-module Go gates** before pushing (both `server-agent` and `gateway/backend`): `go build ./...`, `go vet ./...`, `golangci-lint fmt --diff`, `golangci-lint run` (gofumpt + gocritic — `gofmt`/`go vet`/`go test` do **not** cover these), and the full `go test ./...` including the **Postgres leg** (provision the DSN like CI; the postgres subtests skip silently without it). **Cross-compile the Windows measurer** (`GOOS=windows GOARCH=amd64 go vet ./...` in `server-agent`) — CI compiles nothing for Windows.
- **Frontend gates** before pushing: `vitest`, `tsc`, `eslint`, and **prettier `format:check`** (test+build+lint miss it).
- **Pre-PR verification:** run the repo's local `make sonar-gate` + branch-findings; judge by new-code / branch-attributed findings, not the legacy total.
- **One process per spec.** `specState.proc` is a single pointer — there is no multi-PID-per-spec model. RAM is measured from the spec's single managed PID's RSS; child processes of a model server are not individually attributed (documented caveat, same spirit as the measured-VRAM caveat).

## Review Focus

Input classes / failure modes the spec implies that no task's happy-path test exercises — each pinned by a test added to the owning task:

- **RAM-only / no-GPU host (nvidia-smi absent → VRAM measurer is `nil`).** Auto-detect must produce an empty GPU-budget set without panicking, and the host must still admit/evict on RAM alone. *Test in Task 1.3 (auto-detect with empty GPU sample) and Task 3.4 (RAM-only admission).*
- **Measured `0` / measurement unavailable for a live PID.** A measured `0` must never read as "needs no RAM" — it falls through to the estimate floor, mirroring `realMeasurement`. *Test in Task 1.5 (RAM zero-guard).*
- **Live `MemAvailable` already consumed by non-managed workloads (co-located docker/tig stack).** The RAM budget is `live MemAvailable − reserve`, not `total − reserve`, so a host already tight admits nothing new and self-corrects as neighbors grow. *Test in Task 3.3 (budget tracks live available, not total).*
- **Old agent without the RAM feature.** The gateway must not overwrite persisted `ram_measured_mb` from a sample that omits it (no flicker/clobber), and pushing `ram_mb` to an agent that ignores unknown fields is harmless. *Test in Task 1.8 (absent RAM in sample leaves the column alone).*
- **Bootstrap must not OOM a neighbor or the agent (hard anti-goal).** An unknown-RAM first start under pressure pre-evicts rather than starting blind; the interlock stops a second admission taking the freed/available RAM mid-window; memory-pressure failure escalates to a **single** bounded retry, never a C3 fork-exec storm. *Test in Task 4.2 (no blind optimistic start), 4.3 (interlock), 4.6 (single-retry bound).*

---

# Phase 1 — Auto-detect budgets (default-on) + measure/persist/display RAM + effective-VRAM display

**Ships:** per-GPU VRAM budgets auto-detected from device totals (so VRAM admission, previously effectively off, becomes enforced); per-process RAM measured, persisted (`ram_measured_mb`), and reported; an operator-settable `ram_estimate_mb`; the table shows effective VRAM (per-GPU + a new VRAM-total column) and a new RAM column. **No new admission *rules*** beyond budgets becoming non-zero; RAM is not yet an admission axis; `max()` precedence and `vram_locked` removal are Phase 2.

## Task 1.1: Host sample carries live available RAM

**Files:**
- Modify: `server-agent/internal/sample/sample.go:353` (the `Host` struct)
- Modify: `server-agent/internal/collector/host.go:55-58`
- Test: `server-agent/internal/collector/host_test.go` (add)

**Interfaces:**
- Produces: `sample.Host.MemAvailableBytes int64` (`json:"mem_available_bytes"`), populated from gopsutil `VirtualMemoryStat.Available`.

- [ ] **Step 1: Write the failing test** `TestCollectHostSetsMemAvailable` — collect a host sample on this machine and assert `h.MemAvailableBytes > 0` and `h.MemAvailableBytes <= h.MemTotalBytes`.
- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/collector/ -run TestCollectHostSetsMemAvailable` → FAIL (field absent / zero).
- [ ] **Step 3: Add the field + populate it.** Add `MemAvailableBytes int64 \`json:"mem_available_bytes"\`` to `sample.Host`. In `host.go:55-58`, inside the existing `if vm, err := mem.VirtualMemoryWithContext(ctx); ...` block, add `h.MemAvailableBytes = int64(vm.Available)` (same `vm`, no extra call).
- [ ] **Step 4: Run it, verify it passes.**
- [ ] **Step 5: Commit** — `feat(agent): sample host MemAvailable for live RAM budgeting`.

## Task 1.2: Latest hardware info reaches the runtime owner

**Files:**
- Modify: `server-agent/internal/runtime/manager.go` (near the measurer plumbing: `measurerFunc` `:120`, `SetMeasurer` `:478`, `buildSnapshot` `:1681`)
- Modify: the agent collect loop that builds each `sample.Sample` and already holds `mgr` (locate: `server-agent/main.go` around `:205` wiring, and the collector tick that produces samples)
- Test: `server-agent/internal/runtime/manager_test.go`

**Interfaces:**
- Produces: `type HostInfo struct { GPUTotalsMB map[int]int; MemAvailableMB int }` and `func (m *Manager) SetHostInfo(HostInfo)` (stores an `atomic.Pointer[HostInfo]`, mirroring the measurer). `owner.buildSnapshot` reads the latest via `m.hostInfo.Load()` (nil → treat as no detected budgets, `MemAvailableMB == 0`).
- Consumes (later tasks): Task 1.3 reads `HostInfo.GPUTotalsMB`; Phase 3 reads `HostInfo.MemAvailableMB`.

Rationale: `buildSnapshot` today consults only the per-process VRAM measurer, never the hardware sample. Auto-detect (1.3) and the RAM budget (Phase 3) need device totals + live available RAM at admission time. Push, don't poll: the collect loop already runs ~1s and produces `sample.GPU[].MemTotalBytes` + `sample.Host.MemAvailableBytes`; it calls `SetHostInfo` each tick (cheap atomic store), and `buildSnapshot` reads the cached value (never shells out on the admission path).

- [ ] **Step 1: Write the failing test** `TestSetHostInfoVisibleInSnapshot` — call `mgr.SetHostInfo(HostInfo{GPUTotalsMB: map[int]int{0: 24000}, MemAvailableMB: 32000})`, build a snapshot, assert the owner observed it (assert via the budget that 1.3 derives, or a test-only accessor). Keep the assertion on observable state, not internals.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** `HostInfo`, the `atomic.Pointer[HostInfo]` field, `SetHostInfo`, and the `buildSnapshot` load. Convert bytes→MB at the call site in the collect loop (`int(bytes / (1024*1024))`), keying `GPUTotalsMB` by `sample.GPU.Index`. Wire the `SetHostInfo` call into the collect loop beside the existing sample emit.
- [ ] **Step 4: Run it, verify it passes.**
- [ ] **Step 5: Commit** — `feat(agent): feed latest GPU totals and MemAvailable to the runtime owner`.

## Task 1.3: Auto-detect per-GPU VRAM budgets (default-on)

**Files:**
- Modify: `server-agent/internal/runtime/manager.go:1739-1747` (the `budgets := make(...)` block in `buildSnapshot`)
- Modify: `server-agent/internal/config/config.go` (new `OP_AGENT_VRAM_HEADROOM_MB` knob, following the `resolveXxx` idiom)
- Test: `server-agent/internal/runtime/manager_test.go`

**Interfaces:**
- Consumes: `HostInfo.GPUTotalsMB` (Task 1.2); `o.cfg.GPUBudgets` (operator override, `GPUBudget{Index, BudgetMB}`); the new headroom knob.
- Produces: a `PolicySnapshot.Budgets` map where each sampled GPU has a positive budget unless an explicit override says otherwise.

Decision (settled by the spec): **operator config override wins; otherwise detected total minus optional headroom.** Per GPU index `i`: if `o.cfg.GPUBudgets` has an entry with `BudgetMB > 0` for `i`, use it. Else if `HostInfo.GPUTotalsMB[i] > 0`, use `max(0, total − headroom)`. Else leave absent (unconstrained — the pre-feature behavior, reached on a no-GPU host or before the first sample). This replaces `0 == unconstrained` with detected values while keeping the `Admit`-side `budget <= 0` interpretation untouched. **Do not** add a `BudgetMB == 0` filter in `buildSnapshot` (the doc comment at `manager.go:1733-1738` and §5.2 forbid it — `Admit` owns that rule).

- [ ] **Step 1: Write the failing tests** in `manager_test.go`:
  - `TestBudgetsAutoDetectedFromGPUTotals` — `HostInfo{GPUTotalsMB:{0:24000}}`, no config budgets, headroom 0 → snapshot `Budgets[0] == 24000`.
  - `TestExplicitGPUBudgetOverridesDetected` — config `GPUBudget{0, 8000}` + detected `24000` → `Budgets[0] == 8000`.
  - `TestVRAMHeadroomSubtracted` — detected `24000`, headroom `1024` → `Budgets[0] == 22976`.
  - `TestNoGPUSampleLeavesBudgetsEmpty` (Review Focus: no-GPU host) — nil/empty `GPUTotalsMB`, no config → `len(Budgets) == 0`, no panic.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the budget-resolution in `buildSnapshot`; add `resolveVRAMHeadroomMB` + the `VRAMHeadroomMB` config field (default `0`, parse rule `int (allow 0)`), env `OP_AGENT_VRAM_HEADROOM_MB`.
- [ ] **Step 4: Run them, verify they pass.** Then run the full `runtime` package tests — existing admission tests that assumed `0`/absent budgets now see detected budgets; confirm none regress (they inject `HostInfo` or none, so default is still "no detected budget").
- [ ] **Step 5: Commit** — `feat(agent): auto-detect per-GPU VRAM budgets from device totals`.

## Task 1.4: Per-process RAM measurer (platform split)

**Files:**
- Create: `server-agent/internal/collector/ram_measurer_other.go` (`//go:build !windows`)
- Create: `server-agent/internal/collector/ram_measurer_windows.go` (`//go:build windows`)
- Test: `server-agent/internal/collector/ram_measurer_test.go`

**Interfaces:**
- Produces: `func NewRAMMeasurer() func(pids []int) map[int]int` — maps PID → resident RAM in **MB**; returns `nil` when unavailable (mirrors `NewVRAMMeasurer` at `measurer_other.go:22` / `measurer_windows.go:29`, but **drops the GPU inner axis** — RAM has no per-GPU dimension, confirmed by the measurement verification).

Decision (left to builder by the spec, settled here): **non-Windows** uses gopsutil `process.NewProcess(pid).MemoryInfo().RSS` → MB (RSS includes resident mmap'd weights, which is correct for host-RAM placement; page-cache reclaimability is the accepted caveat). **Windows** uses the **private working set** (mirroring the VRAM PDH split; private working set excludes shareable pages, the closer analogue to RSS for placement) via the existing Windows collector mechanics. Document the Windows metric choice in a file comment.

- [ ] **Step 1: Write the failing test** (non-Windows) `TestRAMMeasurerReportsSelf` — `m := NewRAMMeasurer(); got := m([]int{os.Getpid()})` → `got[os.Getpid()] > 0`, and roughly matches this process's RSS (assert `> 1`, i.e. at least ~1 MB).
- [ ] **Step 2: Run it, verify it fails** (files absent).
- [ ] **Step 3: Implement** both files. Non-Windows: iterate pids, skip a pid whose `MemoryInfo()` errors (dead/racing), convert `RSS` bytes→MB. Windows: private working set per pid. Each returns `nil` if the platform mechanism is unavailable.
- [ ] **Step 4: Run it, verify it passes.** Cross-compile check: `GOOS=windows GOARCH=amd64 go build ./internal/collector/`.
- [ ] **Step 5: Commit** — `feat(agent): per-process RAM measurer (RSS / Windows private working set)`.

## Task 1.5: RAM measurement plumbing in the manager

**Files:**
- Modify: `server-agent/internal/runtime/manager.go` — mirror `measurerFunc` `:120`, the atomic pointer `:197`, `SetMeasurer` `:478`, `dispatchMeasurement` `:1528`, `applyMeasurement` `:1654`, `buildSnapshot` measurement read `:1689`, `Status()` copy `:2615-2624`, `onProcExited` clear `:2142`
- Modify: `server-agent/main.go:205` (wire `mgr.SetRAMMeasurer(collector.NewRAMMeasurer())`)
- Test: `server-agent/internal/runtime/manager_test.go`

**Interfaces:**
- Produces: `func (m *Manager) SetRAMMeasurer(f func(pids []int) map[int]int)`; `specState.measuredRAM int` (0 = unknown); `Status().MeasuredRAMMB int` (spec-scalar). A `realRAM(v int) int` zero-guard (returns `0` for `v <= 0`) mirrors `realMeasurement`.
- Consumes: the RAM measurer func (Task 1.4).

Note: one PID per spec, so no summation. The RAM measurer is dispatched alongside the VRAM measurer; keep the existing generation guard (`st.proc != t.proc`). In Phase 1 the measured RAM flows to `Status`/telemetry only — it is **not** yet put on `RunningProc` or read by `Admit` (Phase 3).

- [ ] **Step 1: Write the failing tests:**
  - `TestRAMMeasurementReachesStatus` — install a fake RAM measurer returning `{pid: 1500}`, start a fake spec, drive a measurement beat, assert `Status()[specID].MeasuredRAMMB == 1500`.
  - `TestRAMMeasuredZeroIgnored` (Review Focus zero-guard) — fake returns `{pid: 0}` → `MeasuredRAMMB` stays at its prior value / `0`, never overwritten to a spurious measured-0-wins state.
  - `TestRAMMeasuredClearedOnExit` — after `onProcExited`, `MeasuredRAMMB == 0`.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the parallel plumbing: `ramMeasurerFunc` type, `ramMeasurer atomic.Pointer[ramMeasurerFunc]`, `SetRAMMeasurer`, dispatch RAM pids in `dispatchMeasurement` (extend `cmdMeasured` with `ramByPID map[int]int`), apply with `realRAM` into `st.measuredRAM`, copy into `Status`, clear on exit. Wire `SetRAMMeasurer` in `main.go` beside `SetMeasurer`.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(agent): measure and report per-spec RAM`.

## Task 1.6: RAM measured on the status wire + feature + version bump

**Files:**
- Modify: `server-agent/internal/runtime/*.go` (the `runtime.Status` → `RuntimeSample` status mirror — the runtime status struct that carries `MeasuredVRAM`)
- Modify: `server-agent/internal/agent/features.go` (append a feature), `server-agent/internal/agent/agent.go:351` (`Version` bump)
- Modify: `gateway/backend/internal/gateway/agent_features.go` (`gatewayAgentFeatures` mirror)
- Test: `server-agent/internal/agent/features_test.go`, and the status-mirror test alongside the runtime status type

**Interfaces:**
- Produces: a spec-level `ram_measured_mb` on the runtime-status wire frame (agent → gateway), beside the existing per-GPU `gpus[].vram_measured_mb`; a declared feature (e.g. `runtime_ram_measurement`) with `Since` = the bumped `Version`.

- [ ] **Step 1: Write the failing tests** — `TestRuntimeStatusCarriesMeasuredRAM` (the status→sample mirror includes `ram_measured_mb` from `MeasuredRAMMB`); `TestFeatureRegistry` will fail until the feature + `Since` are consistent.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — add the wire field; append `Feature{Name: "runtime_ram_measurement", Since: "<new Version>"}`; bump `Version` MINOR; mirror the name in `gatewayAgentFeatures`. **Merge `origin/main` first and base the bump on main's current `Version`.**
- [ ] **Step 4: Run them, verify they pass** (incl. `TestFeatureRegistry`).
- [ ] **Step 5: Commit** — `feat(agent): report measured RAM on the runtime status wire`.

## Task 1.7: Persistence — RAM columns + store method (migration 96)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append `{version: 96, ...}` after `:130`; add `migration96Up` at file end)
- Modify: `gateway/backend/internal/store/sqlite_runtime.go` (positional column lists `:30/:34/:45/:61`, `runtimeSpecCols` `:80-86`, `runtimeSpecColsPrefixed` `:92-98`, `scanRuntimeSpec` `:355-375`; new `UpdateRuntimeSpecRAMMeasured`, new upsert of `ram_estimate_mb`)
- Modify: `gateway/backend/internal/routing/store.go` (`RuntimeSpec` struct `:2586` region: add `RAMEstimateMB`, `RAMMeasuredMB`; `Store` interface `:2774` region: add `UpdateRuntimeSpecRAMMeasured`)
- Modify: `gateway/backend/internal/routing/memory_store.go` (`:2795` region: impl)
- Modify: `gateway/backend/internal/tracing/routingstore_gen.go` (`:1543` region: wrapper — regenerate if a generator exists, else hand-edit to match)
- Test: `gateway/backend/internal/store/conformance_test.go`, `routing_store_conformance_test.go`, `runtime_spec_column_parity_test.go`

**Interfaces:**
- Produces: `ram_estimate_mb` + `ram_measured_mb` columns on **`agent_runtime_specs`** (spec-level — RAM is host-scoped, so it lives on the spec table, **not** the per-GPU `agent_runtime_spec_gpus`; this is deliberate and mirrors the VRAM split, where per-GPU columns live on the GPU table); `RuntimeSpec.RAMEstimateMB int`, `RuntimeSpec.RAMMeasuredMB int`; `UpdateRuntimeSpecRAMMeasured(ctx, specID string, measuredMB int) error` (spec-level — no `gpuIndex`, unlike `UpdateRuntimeSpecGPUMeasured`).
- Consumes: the migration framework idiom (`addColumnIfMissing`, migration95Up precedent).

- [ ] **Step 1: Write the failing tests:**
  - Conformance: upsert a spec with `RAMEstimateMB: 4096`, read it back, assert both RAM fields round-trip; call `UpdateRuntimeSpecRAMMeasured(specID, 5000)`, read back `RAMMeasuredMB == 5000`.
  - `runtime_spec_column_parity_test.go` — add `ram_estimate_mb`, `ram_measured_mb` to the expected column set (`:17/:25/:37/:46/:144`).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — `migration96Up` adds the two columns via `addColumnIfMissing(ctx, tx, dl, "agent_runtime_specs", "ram_estimate_mb integer not null default 0")` (and `ram_measured_mb`); extend the positional INSERT/VALUES/on-conflict/bound-arg lists and `scanRuntimeSpec`; add the two struct fields; write `UpdateRuntimeSpecRAMMeasured` (SQLiteStore `update agent_runtime_specs set ram_measured_mb = ? where id = ?` + `requireAffected`), the interface decl, the memory-store impl, and the tracing wrapper.
- [ ] **Step 4: Run them, verify they pass** — run with the **Postgres DSN** set (both dialects) and the in-memory store.
- [ ] **Step 5: Commit** — `feat(store): persist per-spec RAM estimate and measurement (migration 96)`.

## Task 1.8: Ingest write-back of measured RAM

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (parse `ram_measured_mb` from the runtime status; add `writeBackRuntimeRAM` mirroring `writeBackRuntimeVRAM` `:785`, `storedMeasuredVRAM` `:846`; reuse the `resolveRuntimeSpecWritable` `:711` gate)
- Test: `gateway/backend/internal/gateway/agent_runtime_status_ingest_test.go`

**Interfaces:**
- Consumes: the status frame's spec-level `ram_measured_mb` (Task 1.6); `UpdateRuntimeSpecRAMMeasured` (Task 1.7).

Mirror the VRAM write-back exactly: `<= 0` skip, change-detection against the stored value (so an unchanged value writes nothing), writability/ownership gate. In Phase 1 the `resolveRuntimeSpecWritable` gate still contains the `VRAMLocked` skip (`:739`) — the RAM write-back passes through the **same** writability result but RAM is not gated by `vram_locked` (there is no RAM lock); the lock skip is removed entirely in Phase 2.

- [ ] **Step 1: Write the failing tests:**
  - `TestIngestWritesMeasuredRAM` — a sample with spec `ram_measured_mb: 6000` → `ram_measured_mb` persisted.
  - `TestIngestSkipsNonPositiveRAM` — `ram_measured_mb: 0` → no write.
  - `TestIngestSkipsUnchangedRAM` — same value twice → one write.
  - `TestIngestAbsentRAMLeavesColumnAlone` (Review Focus: old agent) — a sample with no `ram_measured_mb` key → the stored value is untouched.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** `writeBackRuntimeRAM` + `storedMeasuredRAM`; call it from the same ingest path as `writeBackRuntimeVRAM`.
- [ ] **Step 4: Run them, verify they pass** (Postgres leg included).
- [ ] **Step 5: Commit** — `feat(gateway): write back measured RAM from runtime telemetry`.

## Task 1.9: Portal DTO + API — RAM estimate/measured (persist + expose)

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` — `RuntimeSpecDTO` `:460`, `PutRuntimeSpecRequest` `:552`, `putRuntimeSpec` write `:1161`, read-back `:1684`
- Test: `gateway/backend/internal/portal/service_runtime_test.go`

**Interfaces:**
- Produces: `RuntimeSpecDTO.RAMEstimateMB` (`json:"ram_estimate_mb"`), `RuntimeSpecDTO.RAMMeasuredMB` (`json:"ram_measured_mb"`, read-only), `PutRuntimeSpecRequest.RAMEstimateMB` (`json:"ram_estimate_mb"`).
- Note: the **effective `ram_mb` demand pushed to the agent** (`AgentRuntimeSpecDTO`) is **Phase 3**. Phase 1 only persists + exposes the operator estimate and the measurement.

- [ ] **Step 1: Write the failing tests** `TestPutRuntimeSpecPersistsRAMEstimate` (PUT `ram_estimate_mb: 4096` → stored) and `TestGetRuntimeSpecReturnsRAMFields` (GET returns `ram_estimate_mb` + `ram_measured_mb`).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — add the DTO fields; map `req.RAMEstimateMB` → `routing.RuntimeSpec.RAMEstimateMB` in `putRuntimeSpec`; read both back in the DTO builder.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(portal): expose and persist per-spec RAM estimate`.

## Task 1.10: Frontend API types for RAM

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` — `RuntimeSpec` (`:67` region, add `ram_estimate_mb`, `ram_measured_mb`), `RuntimeStatus` (`:244-272`, add `ram_measured_mb?`)
- Test: covered indirectly (no `runtime.test.ts` exists) — type-checked by `tsc` and by the component tests in later tasks.

**Interfaces:**
- Produces: `RuntimeSpec.ram_estimate_mb: number`, `RuntimeSpec.ram_measured_mb: number`, `RuntimeStatus.ram_measured_mb?: number`. These flow into `PutRuntimeSpecRequest` automatically (it is `Omit<RuntimeSpec, ...>` and does not omit them).

- [ ] **Step 1: Add the fields** to the two interfaces, with doc comments matching the existing VRAM fields' operator-vs-agent ownership note.
- [ ] **Step 2: Verify** — `tsc` passes; `vitest` still green (fixtures without the field default via `??`/optional where read).
- [ ] **Step 3: Commit** — `feat(frontend): RAM fields on runtime spec and status types`.

## Task 1.11: Frontend table — effective VRAM column, VRAM-total column, RAM column

**Files:**
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` — `formatGpus` `:662-666`, the `gpus` column `:3497-3503`, add two columns in the `columns` array `:3474`
- Modify: `gateway/frontend/src/i18n.ts` (new column labels, de + en)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`

**Interfaces:**
- Consumes: `RuntimeSpecGPU.vram_estimate_mb` + `vram_measured_mb`, `RuntimeSpec.vram_locked` (still present in Phase 1), `RuntimeSpec.ram_estimate_mb` + `ram_measured_mb`.

Decision (phasing, settled here to honor the "table must not disagree with admission" anti-goal at the Phase 1 boundary): the per-GPU VRAM cell shows the **lock-aware effective** value — `vram_locked ? vram_estimate_mb : (vram_measured_mb > 0 ? vram_measured_mb : vram_estimate_mb)` — which matches exactly what admission uses in Phase 1. Phase 2 removes `vram_locked` and collapses this to `max(vram_measured_mb, vram_estimate_mb)`. The **VRAM-total** column is the sum of per-GPU effective values. The **RAM** column shows `max(ram_measured_mb, ram_estimate_mb)` (RAM has no lock), `—` when both are 0.

- [ ] **Step 1: Write the failing tests** in `RuntimeAdminSection.test.tsx`:
  - effective VRAM: a spec with `vram_estimate_mb: 8000, vram_measured_mb: 9000, vram_locked: false` renders the GPU cell with `9000`; with `vram_locked: true` renders `8000`.
  - VRAM total: two GPUs (effective 9000 + 3000) render a total cell of `12000` (or formatted MB).
  - RAM column: `ram_measured_mb: 6000, ram_estimate_mb: 4096` renders `6000`; both 0 renders `—`.
  (Reuse the existing `cellForColumn`/`gpus`-column scoping helpers at `:2680`.)
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — introduce `effectiveGpuVram(spec, g)` and a `formatGpus` that uses it; add the VRAM-total and RAM columns with i18n labels (`runtimeSpecVramTotal`, `runtimeSpecRam`). Keep the measured em-dash fallback.
- [ ] **Step 4: Run them, verify they pass** (`vitest`, then `tsc`, `eslint`, prettier `format:check`).
- [ ] **Step 5: Commit** — `feat(frontend): show effective VRAM, VRAM total, and RAM in the runtime table`.

## Task 1.12: Frontend form — spec-level RAM estimate field

**Files:**
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` — form state (`:2415` region), hydrate-on-edit (`:2601` region), `buildSpecBody` (`:2999`/`:3009-3013`), and the form layout (a new spec-level `<Field>` — **not** inside the per-GPU `gpuRows.map`, since RAM is host-scoped)
- Modify: `gateway/frontend/src/i18n.ts` (label + hint, de + en)
- Test: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx`

**Interfaces:**
- Produces: the PUT body carries `ram_estimate_mb` from a single form field. `GpuRow` (`:171-176`) is **unchanged** (RAM is not per-GPU).

- [ ] **Step 1: Write the failing test** `TestFormRoundTripsRAMEstimate` — open the edit form for a spec with `ram_estimate_mb: 4096`, assert the field shows `4096`; change it to `8192`, submit, assert the PUT body has `ram_estimate_mb: 8192`.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** — `const [ramEstimateMb, setRamEstimateMb] = useState(0)`, hydrate from `spec.ram_estimate_mb`, add a numbered `<Field label={t.runtimeSpecRam} type="number" min={0}>`, emit `ram_estimate_mb: ramEstimateMb` in `buildSpecBody`.
- [ ] **Step 4: Run it, verify it passes** (all four frontend gates).
- [ ] **Step 5: Commit** — `feat(frontend): operator RAM estimate field on the runtime spec form`.

## Task 1.13: Phase 1 docs

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§5.1 data model: note auto-detected per-GPU budgets + the new RAM measurement/persistence; §10 runtime status: the new `ram_measured_mb` frame field)
- Modify: `docs/architecture/reference/config-env.md` (new `OP_AGENT_VRAM_HEADROOM_MB` row)
- Modify: `docs/architecture/09-architecture-decisions.md` (new **ADR-083** — "Resource accounting is on by default with auto-detected budgets"; auto-populate per-GPU VRAM from device totals, host-RAM basis from live `MemAvailable`, operator config as override, the `0`-sentinel preserved, the default-on VRAM-enforcement behavioral change and its rollout note)

- [ ] **Step 1: Write ADR-083** and the §5.1/§10/config-env edits, in the house ADR style (Context / Decision / Rejected / Consequence), cross-linked.
- [ ] **Step 2: Verify** — `make sonar-gate` doc/link checks if any; manual link check.
- [ ] **Step 3: Commit** — `docs(adr): default-on resource accounting with auto-detected budgets (ADR-083)`.

**Phase 1 PR:** open against `main`, Auto-fix on. All per-module Go gates (incl. Windows cross-vet + Postgres leg) and all frontend gates green; sonar-gate branch-findings clean.

---

# Phase 2 — `max(measured, manual)` precedence everywhere + remove `vram_locked`

**Ships:** the effective value (VRAM push to agent, agent `buildSnapshot`, and table) becomes `max(measured, manual)` — the manual value is a floor, never under-reserving; `vram_locked` is removed end-to-end (DB column dropped via migration, DTOs/policy/UI/i18n/tests cleaned). RAM is still persisted/displayed from Phase 1 (its display is already `max`); RAM admission is Phase 3.

## Task 2.1: Gateway push uses `max()` (drop the lock from the demand)

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go:2659-2669` (the effective-VRAM block in `agentRuntimeSpecDTO`)
- Test: `gateway/backend/internal/portal/service_runtime_test.go` (rewrite `TestAgentRuntimeConfigVRAMLockedServesTheOperatorEstimate` `:1796-1877` into a `max()` test)

**Interfaces:**
- Produces: the pushed per-GPU demand `AgentRuntimeSpecGPUDTO.VRAMMB = max(g.VRAMMeasuredMB, g.VRAMEstimateMB)` (a measured `0` yields the estimate; both `0` yields `0` = unknown).

- [ ] **Step 1: Rewrite the test** `TestAgentRuntimeConfigPushesMaxOfMeasuredAndEstimate` — measured `9000`, estimate `8000` → pushed `9000`; measured `0`, estimate `8000` → `8000`; measured `7000`, estimate `8000` → `8000` (floor); both `0` → `0`.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** — collapse the block to `vram := g.VRAMMeasuredMB; if vram < g.VRAMEstimateMB { vram = g.VRAMEstimateMB }` (a `max` that still treats measured `0` as below any positive estimate; keep the existing `== 0 → estimate` semantics as a subset). Remove the `spec.VRAMLocked` term.
- [ ] **Step 4: Run it, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal): push max(measured, estimate) VRAM demand to the agent`.

## Task 2.2: Agent `buildSnapshot` effective VRAM uses `max()`

**Files:**
- Modify: `server-agent/internal/runtime/manager.go:1704-1714` (the effective-VRAM loop)
- Test: `server-agent/internal/runtime/manager_test.go`

**Interfaces:**
- Produces: `RunningProc.GPUs[i] = max(liveMeasured[i], spec.GPUs[i].VRAMMB)` — the live measurement raises the pushed demand (which already includes the estimate floor from Task 2.1) but never lowers it below it.

Rationale: today `buildSnapshot` uses the live measurement *instead of* the pushed demand when present, which can under-reserve when a transient live reading dips below the floor. `max()` makes the pushed demand a floor and the live reading the ceiling-raiser — the spec's "never under-reserve" rule.

- [ ] **Step 1: Write the failing test** `TestBuildSnapshotEffectiveVRAMIsMax` — spec demand `8000`, live measured `9000` → `RunningProc.GPUs[0] == 9000`; live measured `7000` → `8000` (floor); live absent → `8000`; live `0` (zero-guarded) → `8000`.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** — change the loop so a present measured value is `max`'d with `g.VRAMMB` rather than replacing it; preserve the `realMeasurement` zero-guard.
- [ ] **Step 4: Run it, verify it passes.**
- [ ] **Step 5: Commit** — `feat(agent): effective VRAM is max(measured, demand) at admission`.

## Task 2.3: Remove `vram_locked` from the gateway (DB + store + DTO + ingest)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append `{version: 97, name: "drop_vram_locked", up: migration97Up}`; `migration97Up` uses `dropColumnIfPresent(ctx, tx, dl, "agent_runtime_specs", "vram_locked")` — migration79Up precedent. **Do not edit the frozen `migration65Up` CREATE at `:3027`.**)
- Modify: `gateway/backend/internal/store/sqlite_runtime.go` (remove `vram_locked` from INSERT `:30`, VALUES tuple `:34`, on-conflict `:45`, bound arg `:61`, `runtimeSpecCols` `:83`, `runtimeSpecColsPrefixed` `:95`, `scanRuntimeSpec` `:357/:363/:375`)
- Modify: `gateway/backend/internal/routing/store.go` (remove `RuntimeSpec.VRAMLocked` `:2586`; fix the doc comments `:2753/:2773`)
- Modify: `gateway/backend/internal/portal/service_runtime.go` (remove `RuntimeSpecDTO.VRAMLocked` `:460`, `PutRuntimeSpecRequest.VRAMLocked` `:552`, the write `:1161`, read-back `:1684`, doc `:666`), `service_runtime_benchmark.go:49`
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (remove the `VRAMLocked` skip in `resolveRuntimeSpecWritable` `:739-741`; fix doc comments `:689-704/:733-735/:748/:869/:1943`)
- Test: update `runtime_spec_column_parity_test.go` (`:17/:25/:37/:46/:144`), `service_runtime_test.go`, `service_runtime_benchmark_test.go` (`:43/:159/:194/:327/:363-364`), `agent_runtime_status_ingest_test.go` (`:27/:57/:182/:199/:234/:377/:395`), `routing_store_conformance_test.go` (`:1307/:1358/:1419/:1434`), `conformance_test.go` (`:8344/:8364`)

**Interfaces:** removes `VRAMLocked` / `vram_locked` from every gateway surface. After this task, write-back and the effective-VRAM push no longer have any lock concept — the measurement always wins its `max()`.

- [ ] **Step 1: Update the column-parity test** to a column set without `vram_locked`; update/retire the lock-specific tests above (the write-back-skip tests become "measurement is always written"; the benchmark tests drop the lock-survives assertions). Run them → FAIL (field still present).
- [ ] **Step 2: Implement the removal** across the files above; add `migration97Up`.
- [ ] **Step 3: Run the full `gateway/backend` tests** (both dialects + memory) → green. Confirm a fresh DB (create at 65, drop at 97) and an upgraded DB both end without the column.
- [ ] **Step 4: Verify** `go build`, `go vet`, `golangci-lint fmt --diff`, `golangci-lint run`.
- [ ] **Step 5: Commit** — `refactor(gateway): remove vram_locked, superseded by max() precedence (migration 97)`.

## Task 2.4: Remove the VRAM-lock checkbox from the frontend + `max()` display

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (remove `RuntimeSpec.vram_locked` `:67`)
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` (remove the checkbox `:4628-4636` + its comment `:4618-4627`; the state `:2415`; default `:242`; hydrate `:2601`; `buildSpecBody` payload `:2999`; simplify `effectiveGpuVram` from Task 1.11 to `max(measured, estimate)` — no lock term)
- Modify: `gateway/frontend/src/i18n.ts` (remove `runtimeSpecVramLocked` + `runtimeSpecVramLockedHint`, de `:1209-1211` + en `:4549-4551`)
- Test: update `RuntimeAdminSection.test.tsx` (`:156/:2605/:2632`), `ApplicationSection.test.tsx:144`, `ServerList.test.tsx:249`, `i18n.test.ts:2483-2484`

**Interfaces:** removes the lock from every frontend surface. The effective-VRAM cell now computes `max(vram_measured_mb, vram_estimate_mb)`.

- [ ] **Step 1: Update the tests/fixtures** — drop `vram_locked` from the spec fixtures and `expectedBody`, remove the two i18n key assertions, update the effective-VRAM test so the previously-lock-aware case now reflects `max()`. Run → FAIL.
- [ ] **Step 2: Implement the removal** and the `effectiveGpuVram` simplification.
- [ ] **Step 3: Run all four frontend gates** (`vitest`, `tsc`, `eslint`, prettier `format:check`).
- [ ] **Step 4: Commit** — `refactor(frontend): remove the VRAM-lock control, display max(measured, estimate)`.

## Task 2.5: Phase 2 docs

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§5.1: effective value is `max(measured, manual)`, manual is a floor; the measurement now always informs the pushed demand — remove the `vram_locked` carve-outs)
- Modify: `docs/architecture/09-architecture-decisions.md` (new **ADR-084** — "`max(measured, manual)` precedence; `vram_locked` retired"; why max() instead of measured-wins + a lock, the floor semantics, the removal path)
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (§11.4 line ~47 references `unless the spec is vram_locked` — update that acceptance to the `max()` world)

- [ ] **Step 1: Write ADR-084** + the §5.1 and §11.4 edits; grep the docs tree for other `vram_locked` mentions and reconcile.
- [ ] **Step 2: Verify** link/doc checks.
- [ ] **Step 3: Commit** — `docs(adr): max(measured, manual) precedence, retire vram_locked (ADR-084)`.

**Phase 2 PR:** against `main`, Auto-fix on, all gates green.

---

# Phase 3 — RAM admission & eviction (host pool)

**Ships:** the gateway pushes an effective `ram_mb` demand; the agent carries it on `Spec.RAMMB` and `RunningProc.RAMMB`; `PolicySnapshot` gains a host-RAM budget from live `MemAvailable − reserve`; a new `Admit` rule evicts host-wide (idle, unpinned, non-starting, oldest-`LastUsed` first) or Waits when the host-RAM pool would overflow; a Starting proc reserves its RAM demand (the known-demand interlock). Bootstrap for unknown RAM (`ram_mb == 0`) is Phase 4.

## Task 3.1: RAM demand on the wire (gateway push + agent decode)

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`AgentRuntimeSpecDTO` `:2305-2364` gains `RAMMB int \`json:"ram_mb"\``; `agentRuntimeSpecDTO` sets `RAMMB = max(spec.RAMMeasuredMB, spec.RAMEstimateMB)`)
- Modify: `server-agent/internal/runtime/types.go` (`Spec` `:57-111` gains `RAMMB int \`json:"ram_mb"\`` — spec-level, beside `GPUs`)
- Modify: `server-agent/internal/agent/features.go` + `agent.go` `Version` + gateway `gatewayAgentFeatures` (feature `runtime_ram_accounting`, `Since` = new `Version`)
- Test: `service_runtime_test.go`, `server-agent/internal/runtime/*_test.go` (config decode), `features_test.go`

**Interfaces:**
- Produces: `AgentRuntimeSpecDTO.RAMMB` / `Spec.RAMMB` — the effective host-RAM demand (`max(measured, estimate)`), `0` = unknown.

- [ ] **Step 1: Write the failing tests** — `TestAgentConfigCarriesRAMDemand` (gateway builds `ram_mb = max(measured, estimate)`); `TestConfigDecodeReadsRAMMB` (agent decodes `ram_mb` onto `Spec.RAMMB`); `TestFeatureRegistry` for the new feature.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — add both fields; set `RAMMB` in the DTO builder; append the feature + bump `Version` (**merge `origin/main` first**).
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat: carry effective per-spec RAM demand to the agent`.

## Task 3.2: `RunningProc` RAM occupancy + host-RAM budget in the snapshot

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` (`RunningProc` `:17-28` gains `RAMMB int`; `PolicySnapshot` `:34-63` gains `HostRAMBudgetMB int` — `0` = unconstrained, same sentinel as per-GPU budgets)
- Modify: `server-agent/internal/runtime/manager.go` (`buildSnapshot`: set `RunningProc.RAMMB = max(liveMeasuredRAM, spec.RAMMB)`; set `PolicySnapshot.HostRAMBudgetMB` from `HostInfo.MemAvailableMB − reserve`)
- Modify: `server-agent/internal/config/config.go` (new `OP_AGENT_HOST_RAM_RESERVE_MB` knob)
- Test: `manager_test.go`

**Interfaces:**
- Produces: `RunningProc.RAMMB` (effective host-RAM occupancy, floor = demand, zero-guarded live measurement); `PolicySnapshot.HostRAMBudgetMB` (`live MemAvailable − reserve`, clamped `>= 0`, `0` when no sample yet → unconstrained).
- Consumes: `HostInfo.MemAvailableMB` (Task 1.2), `specState.measuredRAM` (Task 1.5), `Spec.RAMMB` (Task 3.1).

Decision (reserve knob, left to builder, settled here): `OP_AGENT_HOST_RAM_RESERVE_MB`, default a conservative built-in constant (e.g. `2048` MB — the plan's suggested default; confirm during review against the house `config-env` conventions). This single agent-side env knob *is* the per-host reserve (one agent per host); the built-in default is the global fallback. Live-available basis (`MemAvailable`, not `total`) is the design's self-correcting choice for shared hosts.

- [ ] **Step 1: Write the failing tests:**
  - `TestSnapshotHostRAMBudgetFromLiveAvailable` (Review Focus) — `HostInfo.MemAvailableMB == 30000`, reserve `2048` → `HostRAMBudgetMB == 27952`; and a change in `MemAvailable` changes the budget (tracks live, not total).
  - `TestRunningProcRAMEffectiveIsMax` — demand `4096`, live measured `5000` → `RAMMB == 5000`; live `3000` → `4096` (floor); live `0` → `4096`.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the two fields, the `buildSnapshot` wiring, and `resolveHostRAMReserveMB` + the config field.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(agent): host-RAM budget and per-proc RAM occupancy in the policy snapshot`.

## Task 3.3: RAM admission rule (host pool) in `Admit`

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` (new rule after the per-GPU rule 3 `:751-808`, within the same collect-then-decide structure; reuse `isEvictable` `:185`, `sortOldestFirst` `:464`, the `toEvict` map, and the `blocked`→`Wait` path `:835`)
- Test: `server-agent/internal/runtime/policy_test.go`

**Interfaces:**
- Consumes: `spec.RAMMB`, `RunningProc.RAMMB`, `PolicySnapshot.HostRAMBudgetMB`.
- Produces: contributes victims to `Decision.Evict` / sets `blocked` exactly like rule 3, but **host-scoped** (sum over *all* running procs, not per-GPU).

Decision (settled by the spec): host-wide pool. If `HostRAMBudgetMB <= 0`, skip (unconstrained). If `spec.RAMMB == 0` (unknown), **do not** run the arithmetic here — unknown RAM routes to Phase 4 bootstrap (Phase 3 treats unknown-RAM specs as contributing `0`, i.e. no RAM gate, matching today's behavior until bootstrap lands). Else: `sum = spec.RAMMB + Σ running.RAMMB` (skipping already-`toEvict`); if `sum <= budget`, fit; else evict idle (`isEvictable`), host-wide, oldest-`LastUsed` first (`sortOldestFirst`) until it fits; if still over, `blocked = true` → `Wait`. Never a partial eviction (the existing whole-decision `Wait` rule covers this). The self-filter (`:545-550`) already excludes the candidate.

Interlock (known-demand half): a Starting proc already contributes its `RAMMB` to the sum (it is in `Running` with `Starting: true`) and is never an eviction victim (`!r.Starting` in `isEvictable`), so a second RAM admission sees the reservation and cannot double-commit the same headroom. The unknown-demand (bootstrap) interlock is Phase 4.

- [ ] **Step 1: Write the failing tests** in `policy_test.go`:
  - `TestAdmitRAMFitsWithinHostBudget` — budget `30000`, one running `10000`, candidate `8000` → OK, no evict.
  - `TestAdmitRAMEvictsIdleOldestHostWide` — budget `16000`, two idle running `10000` (old) + `4000` (new), candidate `8000` → evicts the `10000` (oldest), fits.
  - `TestAdmitRAMWaitsWhenAllBusy` — same overflow but the idle candidate is `InFlight>0` → `Wait`, empty evict.
  - `TestAdmitRAMUnconstrainedWhenBudgetZero` — `HostRAMBudgetMB == 0` → OK regardless.
  - `TestAdmitRAMUnknownDemandNotGatedYet` — candidate `RAMMB == 0` → not refused by RAM arithmetic (Phase 4 owns it).
  - `TestAdmitStartingProcReservesRAM` (interlock) — a Starting proc's `RAMMB` counts toward the sum and is not an eviction victim.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the rule, slotted to compose with the existing `toEvict` chaining and the process-count limit that runs last (don't double-evict; mirror rule 3's structure).
- [ ] **Step 4: Run them, verify they pass**; run the whole `runtime` package to confirm VRAM/matrix tests still green.
- [ ] **Step 5: Commit** — `feat(agent): admit and evict on a host-wide RAM pool`.

## Task 3.4: End-to-end RAM-only placement test + docs

**Files:**
- Test: `server-agent/internal/runtime/manager_test.go` (an integration-style test driving a RAM-only spec, VRAM measurer nil)
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§5.1 gains the RAM axis; §5.2 "the three gates" gains the host-RAM arithmetic as a fourth computable veto — note the matrix's old "system RAM is not computable" comment is now partly superseded; §5.4 eviction gains the host-wide RAM victim selection), `docs/architecture/reference/config-env.md` (`OP_AGENT_HOST_RAM_RESERVE_MB`), `docs/architecture/09-architecture-decisions.md` (**ADR-085** — "RAM is a co-equal host-scoped admission axis on a live-available pool")

**Interfaces:** none new — verification + docs.

- [ ] **Step 1: Write** `TestRAMOnlyHostAdmission` (Review Focus: no-GPU host) — no GPU sample, a RAM-only spec admitted/evicted purely on the host-RAM pool.
- [ ] **Step 2: Run it, verify it passes** (after Task 3.3).
- [ ] **Step 3: Write ADR-085** + the §5.1/§5.2/§5.4/config-env edits.
- [ ] **Step 4: Verify** doc/link checks.
- [ ] **Step 5: Commit** — `feat(agent): RAM-only placement test; docs for the RAM admission axis (ADR-085)`.

**Phase 3 PR:** against `main`, Auto-fix on, all gates green.

## Review Focus for Phase 3

- Live-available (not total) basis — pinned by `TestSnapshotHostRAMBudgetFromLiveAvailable`.
- No-GPU host RAM-only placement — pinned by `TestRAMOnlyHostAdmission`.
- Known-demand interlock (Starting reserves) — pinned by `TestAdmitStartingProcReservesRAM`.

---

# Phase 4 — Cold-start bootstrap

**Ships:** a first start of a spec with unknown effective demand (`max == 0` on an axis) captures a first measurement safely. **RAM:** with live headroom, start + measure with no eviction; pre-evict idle procs only when the host is actually too tight; never a blind optimistic start. **VRAM:** keep ADR-032 in steady state (unknown waits, starts alone on a free card), but allow a **one-shot guarded** eviction solely to capture the first measurement, after which known-demand rules resume. **Failure handling:** a memory-pressure signature (OOM-kill / SIGKILL, or RSS near budget at death) escalates to evict-and-retry **bounded to a single retry**; a genuine crash/config failure goes to the existing `StateStartFailed` crash-loop backoff; no healthy+measured state within the deadline ⇒ `StateStartFailed`.

This is the riskiest phase (hard anti-goal: never OOM a neighbor or the agent). Tasks are kept small with explicit safety tests.

## Task 4.1: Detect the bootstrap condition + the RAM bootstrap interlock

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` and/or `manager.go` (classify "unknown axis" = effective `max == 0`; a bootstrap-in-progress flag on the owner)
- Test: `policy_test.go` / `manager_test.go`

**Interfaces:**
- Produces: a predicate for "spec S needs a RAM bootstrap" (`spec.RAMMB == 0` and no `measuredRAM` yet) and "needs a VRAM bootstrap" (reuses `specHasUnknownVRAM` `:224`); an owner-level interlock `o.ramBootstrapInFlight bool` (or a per-spec marker) that blocks other RAM admissions while an unknown-RAM spec is Starting+measuring.

Decision (interlock, spec's "my call"): while a RAM bootstrap is Starting and not yet measured, other RAM admissions `Wait` — the bootstrapping spec's true RAM cost is unknown, so a second spec taking the freed/available RAM mid-window is exactly how the bootstrap causes the OOM it exists to prevent. Clears when the spec reaches healthy+measured or fails.

- [ ] **Step 1: Write the failing tests** — `TestRAMBootstrapBlocksConcurrentRAMAdmission` (while spec A bootstraps RAM, spec B's RAM admission returns `Wait`); `TestBootstrapConditionDetected` (unit on the predicate for RAM and VRAM).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the predicate + the interlock flag and its `Wait` effect in `Admit`/owner; clear it on healthy-measured and on exit/fail.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(agent): detect cold-start bootstrap and interlock concurrent RAM admissions`.

## Task 4.2: RAM bootstrap — start-and-measure, pre-evict only under pressure

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` / `manager.go` (the unknown-RAM admission path)
- Test: `policy_test.go`

**Interfaces:**
- Consumes: `PolicySnapshot.HostRAMBudgetMB` (live), a conservative first-guess constant for the candidate's RAM (builder constant, e.g. a floor used only during bootstrap), the interlock from 4.1.
- Produces: for an unknown-RAM candidate — if `HostRAMBudgetMB` has headroom for the conservative guess, admit with **no eviction**; else pre-evict idle procs (host-wide, oldest-first) until the guess fits; if it cannot fit even after evicting all idle, `Wait`. **Never** start blind when the host is tight.

Decision (settled by the spec): default-on must not churn the host on every new spec's first start, so the common case (headroom available) is "just start and measure." Eviction happens only under genuine pressure.

- [ ] **Step 1: Write the failing tests** (Review Focus):
  - `TestRAMBootstrapStartsWithoutEvictionWhenHeadroom` — plenty of `MemAvailable` → unknown-RAM spec admits, `Evict` empty.
  - `TestRAMBootstrapPreEvictsUnderPressure` — tight host, an idle neighbor → the neighbor is evicted to make room for the conservative guess.
  - `TestRAMBootstrapWaitsWhenNoIdleVictimUnderPressure` — tight host, all busy → `Wait`, no blind start.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the unknown-RAM path with the conservative guess + pressure check.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(agent): RAM cold-start bootstrap (start-and-measure, pre-evict only under pressure)`.

## Task 4.3: VRAM bootstrap — one-shot guarded eviction preserving ADR-032

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` (the unknown-VRAM rules 4/5 region `:681-749`, ADR-032 comment `:616-660`) and the owner (one-shot marker)
- Test: `policy_test.go`

**Interfaces:**
- Produces: for an unknown-VRAM candidate that has **never** been measured, a single guarded eviction to get a free card and capture the first measurement; after the first measurement the spec has known demand and rules 4/5 resume unchanged. Steady-state ADR-032 (unknown waits rather than evicting known demand) is preserved for every spec that already has a measurement or has already used its one shot.

Decision (settled by the spec): the one-shot is strictly to capture a first measurement; it must not reintroduce the ADR-032 bidirectional thrash. Guard it per (spec, lifetime-first-measurement) so it cannot repeat.

- [ ] **Step 1: Write the failing tests:**
  - `TestVRAMBootstrapOneShotEvictsToMeasure` — a never-measured unknown-VRAM spec gets one eviction to claim a card.
  - `TestVRAMBootstrapDoesNotRepeat` — after the one shot (or a captured measurement), the spec falls back to ADR-032 `Wait` instead of evicting again.
  - `TestADR032SteadyStatePreserved` — an already-known/measured unknown-side pairing still obeys "known beats unknown" (re-run the existing ADR-032 tests + a new guard).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the one-shot marker + guarded eviction; leave rules 4/5 otherwise intact.
- [ ] **Step 4: Run them, verify they pass**; re-run the whole ADR-032 test set.
- [ ] **Step 5: Commit** — `feat(agent): one-shot guarded VRAM bootstrap eviction (preserves ADR-032)`.

## Task 4.4: Bootstrap failure classification + single-retry + termination

**Files:**
- Modify: `server-agent/internal/runtime/manager.go` (the start/exit-handling path: `onProcExited` `~:2142`, start-timeout path, `StateStartFailed` transition + crash-loop backoff)
- Test: `manager_test.go`

**Interfaces:**
- Produces: on a bootstrap process death — classify as **memory-pressure** (OOM-kill / SIGKILL exit signature, or last-measured RSS near the host budget at death) vs **genuine crash/config**; memory-pressure → evict-and-retry **once** (compose with existing backoff, never loop); crash → existing `StateStartFailed` crash-loop backoff unchanged; no healthy+measured within the start deadline / N measurement beats ⇒ `StateStartFailed` (bounded).

Decision (settled by the spec): escalation bounded to a single retry; must never become a C3 fork-exec storm. The classification reads the exit signature + the last measurement, not free text.

- [ ] **Step 1: Write the failing tests** (Review Focus: single-retry bound, no storm):
  - `TestBootstrapMemoryPressureRetriesOnce` — a SIGKILL/OOM exit during bootstrap triggers one evict-and-retry; a second failure goes to `StateStartFailed`, not a third start.
  - `TestBootstrapCrashUsesBackoffNotEscalation` — a plain non-zero exit (config error) goes straight to the existing crash-loop backoff, no eviction.
  - `TestBootstrapNoMeasurementWithinDeadlineFails` — healthy but no measurement within N beats ⇒ `StateStartFailed`.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the classifier + the single-retry counter (reset on a successful measurement) + the deadline/beat termination.
- [ ] **Step 4: Run them, verify they pass**; run the whole `runtime` package.
- [ ] **Step 5: Commit** — `feat(agent): classified, single-retry bootstrap failure handling`.

## Task 4.5: Phase 4 docs

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§5.3 unknown-VRAM: the one-shot bootstrap carve-out; a new subsection on RAM bootstrap + the interlock; §5.4/§6 process lifecycle: the classified single-retry)
- Modify: `docs/architecture/09-architecture-decisions.md` (**ADR-086** — "Cold-start bootstrap: measure-first safely; RAM pre-evict-under-pressure, VRAM one-shot guarded, classified single retry")
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (§11.4: record any accepted bootstrap edge, e.g. the conservative-guess caveat)

- [ ] **Step 1: Write ADR-086** + the §5.3/§5.4/§6/§11.4 edits, cross-linked to ADR-032.
- [ ] **Step 2: Verify** doc/link checks.
- [ ] **Step 3: Commit** — `docs(adr): cold-start bootstrap for RAM and VRAM (ADR-086)`.

**Phase 4 PR:** against `main`, Auto-fix on, all gates green.

---

## Self-review notes (author)

- **Spec coverage.** Goal 1 (auto-detect default-on) → 1.1–1.3; goal 2 (measure/persist RAM) → 1.4–1.9; goal 3 (`max()`) → 2.1–2.2 (VRAM), 3.1 (RAM demand); goal 4 (table shows effective + VRAM-total + RAM) → 1.11; goal 5 (RAM admission/eviction host pool, live basis) → 3.2–3.4; goal 6 (bootstrap) → Phase 4. `vram_locked` removal → 2.3–2.4. Non-goals respected: VRAM measurement mechanism unchanged (only precedence/budget/display); RAM host-scoped (no per-GPU RAM); single-host scope; no probe/capability change beyond RAM fields.
- **Code-map corrections folded in** (from verification): `RunningProc` is `policy.go:17-28` (not 34-63) and `GPUs` is effective occupancy, not demand; `isEvictable` is in `policy.go:185`, not manager.go; `beginDrain` def is `manager.go:2360` (eviction call site `:1451-1464`); the measurer is a bare `func`, so the RAM measurer gets its own signature (no GPU axis) and `SetRAMMeasurer`; one process per spec (no multi-PID aggregation); `sample.Host` lacks `MemAvailable` today (Task 1.1 adds it) and `buildSnapshot` cannot see the hardware sample today (Task 1.2 wires it); **RAM columns belong on `agent_runtime_specs` (spec-level), not the per-GPU `agent_runtime_spec_gpus`** — the one apparent code-map "mismatch" is deliberate; highest migration is 95 (next 96, then 97 for the drop); `vram_locked` is **not** on the agent wire DTO, so its removal is gateway + frontend only.
- **Type consistency.** `RAMMB`/`ram_mb` (agent `Spec` + gateway `AgentRuntimeSpecDTO`, spec-level demand), `RAMEstimateMB`/`RAMMeasuredMB` + `ram_estimate_mb`/`ram_measured_mb` (store + portal DTO + frontend), `MeasuredRAMMB` (agent status), `measuredRAM` (`specState`), `HostRAMBudgetMB`/`RunningProc.RAMMB` (policy), `HostInfo{GPUTotalsMB, MemAvailableMB}` — used consistently across tasks.
- **Proportion.** Code bodies are given only where an algorithm isn't fixed by the signature + tests (budget resolution, the `max()` collapses, the RAM admission rule); everything else is signature + test name + assertions + verified anchor.

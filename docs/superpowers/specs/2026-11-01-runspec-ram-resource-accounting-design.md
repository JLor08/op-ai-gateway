# RunSpec RAM Resource Accounting & Measured-Primary Resource Display — Design

**Date:** 2026-11-01
**Status:** Final draft for approval
**Intended repo path (feature branch):** `docs/superpowers/specs/2026-11-01-runspec-ram-resource-accounting-design.md`

## Intent & Why

The server-agent places and admits model RunSpecs using a **per-GPU VRAM budget
only**, and that VRAM budget is effectively off unless an operator sets it. That
blinds the scheduler to two real classes of workload:

1. **RAM-only models / binaries** (CPU-only): consume host RAM, zero VRAM —
   invisible to today's accounting, so the host silently over-commits RAM.
2. **VRAM-overspill models** (partial GPU offload): occupy some VRAM *and* the
   spilled remainder in host RAM — so a RAM-tight host can be chosen even though
   VRAM fits, causing host memory pressure / OOM.

Without per-spec RAM knowledge the system cannot make good placement decisions.
This feature adds **RAM as a second, co-equal placement axis** (measured,
displayed, overridable, fed into admission + eviction), turns resource accounting
**on by default with auto-detected budgets**, fixes the table so it shows the
*effective* value, and changes measured/manual precedence to `max()`.

The two axes compose: existing per-GPU VRAM measurement describes the GPU part; a
new per-spec RAM measurement describes the CPU/overspill part. A RAM-only model
is simply VRAM=0 / RAM>0.

## Goals

1. **Accounting ON by default, auto-detected budgets.** At startup the agent
   auto-populates budgets from the hardware — each *visible/used* GPU's total VRAM
   (already read via nvidia-smi `memory.total` → `sample.GPU.MemTotalBytes`,
   respecting `set_visible_devices`) and host RAM — replacing today's
   `0 == unconstrained` default. A manual system-variable / config **override
   takes precedence** (default = detected, override = set). Applies to VRAM
   (so its admission becomes actively enforced where it was effectively off) and
   RAM alike.
2. **Measure per-process RAM** of each managed RunSpec process, mirroring the
   per-process VRAM pipeline; persist a durable last-measured RAM per spec.
3. **Effective value = `max(measured, manual)`** for both VRAM and RAM. The
   manual value is a **floor** (reserve at least this much); measurement raises it
   when reality is higher. Never under-reserve. `max == 0` = "unknown" (triggers
   bootstrap).
4. **Table shows the effective value.** Keep the per-GPU VRAM column but bind it
   to effective; add a **VRAM total** column (sum of effective per-GPU VRAM) and a
   new **RAM** column.
5. **RAM feeds admission & eviction** as a single **host-wide pool**, budget based
   on **live available RAM (`MemAvailable`) − a per-host-configurable reserve**
   (not static `host-total − reserve`), because hosts are shared (co-located
   docker/tig stack); live-available self-corrects for non-managed workloads.
   Reuse the existing eviction machinery, host-scoped (any idle proc).
6. **Cold-start bootstrap** (new spec, effective `max == 0`):
   - Uses live-available RAM: with sufficient headroom, **just start and measure —
     no eviction** (so default-on does NOT churn the host on every first start);
     pre-evict idle procs **only** when the host is actually too tight.
   - **VRAM:** keep ADR-032's anti-thrash protection in steady state (unknown-VRAM
     waits rather than evicting known-demand occupants); allow a **one-shot,
     guarded** eviction solely to capture the first measurement — after which the
     spec has a known demand and normal rules resume (no repeated thrash).
   - Purpose: capture a first measurement so later admissions use real data.

## Non-goals

- No change to **how** VRAM is measured (per-process-per-GPU) — only precedence,
  default-on budgets, and display change.
- No per-GPU RAM (RAM is host-scoped: one value per spec).
- Not the multi-machine autoscaling/placement epic (#340); this feeds it but
  stays scoped to single-host admission as today.
- No change to probe/capability-discovery beyond adding RAM fields.

## Anti-goals (failure even if it technically works)

- A RAM bootstrap that OOM-kills *other* running models or the agent itself.
  Host-RAM safety is paramount: a failed model load is fine; a host OOM that takes
  down neighbors is not.
- The `max()` rule reserving *less* than today for a spec relying on its estimate.
- A table showing a number that disagrees with what admission actually used.
- Default-on accounting that quiesces the host on every new spec's first start, or
  that reintroduces the ADR-032 bidirectional thrash / C3 fork-exec storm.

## Constraints

- Repo norms: feature branch + PR (never commit/merge `main`); PR/issue/commit
  text in English; no LAN host names in repo text; push over SSH.
- **Default-on VRAM budgets is itself a behavioral change** where budgets were
  previously 0/unconstrained — treat as such (rollout, docs).
- Budget/measurement/admission is **per-GPU-index keyed** (`Budgets map[int]int`,
  `RunningProc.GPUs map[int]int`). RAM is a single host scalar — thread it as a
  new host-scoped dimension, do not shoehorn into per-GPU maps.
- The `0 == unknown/unbounded` sentinel is load-bearing (`policy.go` budgets,
  `realMeasurement`, `<=0` write-back guards). RAM fields follow the same
  discipline. (Note: auto-detected budgets mean `0` budget now rarely occurs.)
- Windows needs its own RAM reader (working set), mirroring VRAM's PDH split.

## Current-state code map (verified)

- **VRAM measurement:** `collector/measurer_other.go:22` vs `measurer_windows.go:29`;
  wired via `runtime/manager.go` `SetMeasurer` (`:478`), used by
  `dispatchMeasurement` (`:1528`) and `buildSnapshot` (`:1681-1716`).
- **Per-(spec,gpu) persistence:** `agent_ingest.go:785` →
  `UpdateRuntimeSpecGPUMeasured` → `agent_runtime_spec_gpus.vram_measured_mb`
  (`store/migrate.go:3034`). Operator estimate `vram_estimate_mb`; today's override
  = boolean `vram_locked` (`store.go:2586`).
- **Per-GPU device totals (for auto-budget):** nvidia-smi `--query-gpu=...memory.total`
  → `sample.GPU.MemTotalBytes` (`collector/nvidia.go:44,107`; `sample/sample.go:391-410`).
- **Effective VRAM / push-to-agent demand:** `portal/service_runtime.go:2664`
  (`vram := measured; if locked || vram==0 { vram = estimate }`) — this is the
  admission demand pushed to the agent, **not** just display; and agent-side
  `buildSnapshot:1704-1714`.
- **Admission/eviction:** `runtime.Admit` `policy.go:544`; per-GPU rule 3
  `:751-808`; unknown-VRAM rules 4/5 + ADR-032 `:681-749` (incl. `~616-660`);
  self-filter `:547`; `PolicySnapshot`/`RunningProc` `:34-63`; manager acts on
  `Decision.Evict` via `beginDrain` (`manager.go:1451-1464,2356`); `isEvictable`
  `Starting` C3 clause `:185`.
- **Host RAM telemetry (host-level only):** `collector/host.go:55-58`
  (gopsutil `mem.VirtualMemory`) → `sample.Host{MemUsedBytes,MemTotalBytes}`;
  `MemAvailable` obtainable from the same gopsutil call. **No per-process/per-spec
  RAM anywhere today.**
- **Table UI:** `RuntimeAdminSection.tsx` columns `:3474`; GPU column `formatGpus`
  `:662-665` (bound to `vram_estimate_mb` → 0 until typed). Client already has
  `vram_estimate_mb` + `vram_measured_mb` (`api/runtime.ts:23-24`, live status
  `:224-257`).

## The How (decided)

### Budgets — auto-detect, default-on
- At owner/startup init, auto-populate each visible GPU's VRAM budget from
  detected total VRAM (`sample.GPU.MemTotalBytes`), respecting `set_visible_devices`,
  and the host-RAM budget basis from live `MemAvailable`. Replace the
  `0 == unconstrained` default with detected values.
- A manual system-variable / config override, when set, takes precedence (per GPU;
  per host for the RAM reserve). Default = detected, override = set.
- Optional small VRAM headroom reserve on the auto GPU budget (default left to
  builder; may be 0).

### Measurement
- New per-process RAM reader beside the GPU measurer (same `SetMeasurer` /
  `measuredVRAM` / Status / telemetry shape). Non-Windows: gopsutil
  `Process.MemoryInfo().RSS`. Windows: working set via the build-tag split.
- Aggregate **per spec** (sum over the spec's PIDs), MB.
- RSS includes resident mmap'd weights — correct for placement (they compete for
  host RAM), documented caveat that page cache is partly reclaimable. Accept RSS
  (same spirit as measured VRAM).

### Persistence
- New **spec-level** columns `ram_estimate_mb` (operator) and `ram_measured_mb`
  (agent) on `agent_runtime_specs`. New migration.
- Write-back mirrors `writeBackRuntimeVRAM`: telemetry RAM →
  `UpdateRuntimeSpecRAMMeasured`, `<=0` skip + change detection.

### Effective value (`max`)
- Central helper `effective(measured, manual) = max(measured, manual)` applied at
  the push-to-agent demand (`service_runtime.go:2664`, **load-bearing — alters real
  admission, not just display**), agent `buildSnapshot`, and the new RAM demand.
- A new `ram_mb`-style field on `AgentRuntimeSpecDTO` / `runtime.Spec` (none today)
  carries the effective RAM demand to the agent, mirroring the per-GPU `vram_mb`.
- **Remove `vram_locked`** (column, DTO, UI checkbox + label, policy use) —
  superseded by `max()`.

### Admission & eviction (RAM pool)
- `PolicySnapshot` gains a host-RAM budget from live `MemAvailable` − per-host
  reserve; `RunningProc` gains a RAM demand (effective RAM MB).
- New rule parallel to VRAM rule 3: if admitting would exceed the host-RAM budget,
  pick idle, unpinned, non-starting victims (oldest `LastUsed` first) **host-wide**;
  else Wait. Reuse `Decision.Evict` + `beginDrain`. Self-filter (`:547`) applies.
- **Interlock (my call):** while a RAM bootstrap is starting+measuring, block other
  RAM admissions so a second spec can't take the freed/available RAM mid-window —
  otherwise the bootstrap can cause the OOM it exists to prevent.

### Cold-start bootstrap
- "Unknown" = effective `max == 0` on the axis.
- **RAM:** with enough live headroom, start + measure, no eviction; only pre-evict
  idle procs when the host is too tight for the candidate's conservative first
  guess. No blind optimistic RAM start.
- **VRAM:** steady state keeps ADR-032 (unknown waits, starts alone on a free
  card); a **one-shot guarded** eviction is allowed only to capture the first
  measurement, after which known-demand rules resume.
- **Failure classification (my call):** only a memory-pressure signature
  (OOM-kill / SIGKILL, or RSS near budget at death) escalates to evict-and-retry;
  a genuine crash/config failure goes to the existing `StateStartFailed`
  crash-loop backoff. Escalation **bounded to a single retry**, composes with
  existing backoff, never loops (no C3 storm).
- **Termination:** no healthy+measured state within the start deadline / N
  measurement beats ⇒ `StateStartFailed` (bounded).

### UI
- `RuntimeAdminSection.tsx`: per-GPU VRAM column shows effective
  `max(measured, estimate)` from existing client fields; new **VRAM total** column
  (sum effective per-GPU); new **RAM** column (`max(ram_measured_mb,
  ram_estimate_mb)`). Form: add a spec-level RAM estimate field; remove the VRAM
  lock checkbox.
- API (`api/runtime.ts`): add `ram_estimate_mb`, `ram_measured_mb` to spec DTO +
  live status; remove `vram_locked`.

## What's left to the builder

- Reserve default value + knob name (per-host + global fallback), per `config-env`
  conventions; optional VRAM headroom-reserve default.
- Exact Windows metric (private working set vs working set) — document the choice.
- Multi-PID RAM aggregation (sum is the assumption).
- Migration detail for retiring `vram_locked` (drop vs keep-ignored) — check other
  usages first.
- Phasing (plan concern). Suggested independently-shippable phases:
  1. Auto-detect budgets (default-on) + measure/persist/display RAM + effective
     VRAM display (no new admission *rules* yet beyond budgets becoming non-zero).
  2. `max()` precedence everywhere + drop `vram_locked`.
  3. RAM admission/eviction host pool (live-available basis, interlock).
  4. Cold-start bootstrap (RAM pre-evict-under-pressure; VRAM one-shot guarded;
     classified bounded retry).

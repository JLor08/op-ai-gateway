# AMD Linux VRAM telemetry (issue #432) — spec

Branch-local working document. Durable content moves to `docs/architecture/`
before the PR.

## Problem

On a native Linux host with AMD GPUs (verified: 2x RX 6900 XT, kernel 6.8,
ROCm 10.2, rocm-smi 4.0.0, agent 0.21.0):

1. `measured_vram` is never populated: `collector.NewVRAMMeasurer()` off
   Windows returns the nvidia-smi compute-apps measurer, which is nil without
   nvidia-smi, so the runtime manager has no VRAM measurer.
2. The card-level GPU entries carry no name, 0 used / 0 total VRAM, no uuid and
   no PCI bus id: `parseRocmJSON` reads rocm-smi 3.x keys, and the collector
   asks for `--showmemuse`, which in 4.0.0 emits only a percentage.

Consequences: the portal hardware report and the runtime-spec GPU picker show
`GPU 0` with no name/handle for AMD cards (NVIDIA shows
`GPU 0 · <name> · <pci bus id>`), the GPU budget drift check never arms for
AMD, the auto-detected VRAM budgets (ADR-083) are absent on AMD hosts because
the device total is 0, and specs on AMD stay in the unknown-demand class unless
the operator fills `vram_estimate_mb`.

## Verified host facts (test host, two llama.cpp ROCm instances)

- `rocm-smi --showid --showproductname --showuse --showmeminfo vram --showtemp
  --showpower --showdriverversion --showbus --showuniqueid --json` emits per
  card: `Device Name`, `Card Series` (capitalised; 3.x: `Card series`),
  `Card Model`, `GUID` (= KFD gpu_id), `Unique ID` (`0x12b1004de0189bb2`),
  `PCI Bus` (`0000:01:00.0`), `VRAM Total Memory (B)`,
  `VRAM Total Used Memory (B)`, `GPU use (%)`, `Temperature (Sensor edge) (C)`,
  `Average Graphics Package Power (W)`, `Node ID`; plus `system."Driver
  version"`. `--showmemuse` instead gives `GPU Memory Allocated (VRAM%)` only.
  `--showserial` gives `N/A` on these parts (not used).
- `/sys/class/kfd/kfd/proc/<pid>/vram_<gpu_id>` holds per-process, per-GPU VRAM
  in bytes, mode 0444. A 4B model on GPU 0: `vram_59699=3949375488`,
  `vram_19306=0`; a 12B model on GPU 1: the reverse with 9405710336. Card
  `mem_info_vram_used` matched (minus the idle baseline).
- `/sys/class/kfd/kfd/topology/nodes/<n>/gpu_id`: node 0 has gpu_id 0 (CPU),
  nodes 1 and 2 have 59699 and 19306. The rank among GPU nodes, ordered by node
  number, is the ROCr device ordinal that `ROCR_VISIBLE_DEVICES` (the agent's
  AMD GPU-assignment variable) addresses, and it equals rocm-smi's `cardN`
  index on the host (`Node ID` 1 and 2 for card0 and card1).
- The managed child (llama-server) was the KFD process itself; workers a
  runtime forks would appear under their own pid, in the root's process group.

## Design

### A. Collector (card level, `collector/amd.go`)

- `rocmArgs`: drop `--showmemuse`; add `--showmeminfo vram`, `--showbus`,
  `--showuniqueid`. One invocation, unchanged JSON shape.
- Name: first non-empty of `Device Name`, `Card Series`, `Card series`,
  `Card Model`, `Card model`.
- `UUID` from `Unique ID`; `PCIBusID` from `PCI Bus`.
- Memory keys unchanged (`VRAM Total Memory (B)` / `VRAM Total Used Memory
  (B)`), now actually emitted.
- rocm-smi string sentinels (`N/A`, `n/a`, `unknown`, empty, case-insensitive,
  trimmed) are "not reported" for every string field and leave it empty.
  Numeric parsing already maps them to 0.
- New fixture `testdata/rocm-smi-4.0.json` (two cards, real 4.0.0 shape incl.
  the extra keys); the 3.x fixture stays. Tests cover both, plus sentinel
  handling.

Effect: the portal shows `GPU 0 · AMD Radeon RX 6900 XT · 0000:01:00.0` in the
spec GPU picker and the hardware report has name, VRAM, uuid; the budget drift
check arms for new AMD budget rows; the ADR-083 auto-detected budgets work on
AMD hosts again (behaviour 3.x hosts already had). Existing budget rows with an
empty `expected_uuid` never report drift (the frontend skips them), so the new
uuid does not raise false alarms.

### B. KFD per-process VRAM measurer (`collector`, Linux)

- New build-tag-free file `kfd_measurer.go` with the pure logic, so it is
  testable everywhere:
  - `kfdGPUOrdinals(root string) (map[uint64]int, error)`: reads
    `<root>/topology/nodes/*/gpu_id`, skips gpu_id 0, orders by numeric node
    number, returns gpu_id → rank.
  - `kfdProcessVRAM(root string) (map[int]map[uint64]int64, error)`: reads
    `<root>/proc/<pid>/vram_<gpu_id>` for every numeric pid directory; files
    that vanish mid-read (process exit) are skipped, not an error.
  - `attributeKFD(perPID, ordinals, pids, groupOf func(int) (int, bool))
    map[int]map[int]int`: maps each KFD pid to its owner via `groupOf` (process
    group leader, the same boundary `groupNvidiaComputeApps` uses), keeps only
    owners in `pids`, converts gpu_id → ordinal (unknown gpu_id: skipped, never
    guessed), sums bytes per owner per ordinal, converts to MiB (floor), drops
    entries ≤ 0. nil when nothing is attributed.
  - `kfdMeasurer{root, groupOf}` with `measure(pids []int) map[int]map[int]int`:
    nil for no pids or any read error of the topology/proc root (a failed
    cycle must look like "nothing measured", never like a 0).
- Linux-only `kfd_measurer_linux.go`: `NewKFDMeasurer()` returns nil unless
  `/sys/class/kfd/kfd/topology/nodes` lists at least one GPU node (gpu_id ≠ 0)
  and `/sys/class/kfd/kfd/proc` exists; `groupOf` is `syscall.Getpgid`
  (error → not attributable).
- Selection: `measurer_other.go` (`!windows`) keeps one exported name and calls
  a per-OS `fallbackVRAMMeasurer()`: `NewNvidiaComputeApps()` if non-nil, else
  on Linux `NewKFDMeasurer()`, else nil. Linux file `measurer_fallback_linux.go`,
  other non-Windows `measurer_fallback_other.go` (returns nil). NVIDIA keeps
  priority on a host that has both (unchanged behaviour for every NVIDIA host).
- `manager.go` is not touched: the measurement flows through the existing
  `dispatchMeasurement` / `applyMeasurement` / `realMeasurement` path.

Limits (documented): the KFD sees ROCm/HIP clients only; a Vulkan build of a
runtime is invisible there (DRM fdinfo `drm-memory-vram` would be the source; a
later addition behind the same shape). The ordinal assumes rocm-smi's card
order equals KFD node order (verified on the test host; rocm-smi itself
enumerates via the KFD topology). A KFD entry that outlives its process (a
known driver bug after a CP fault) is never reported, because only live managed
pids are attributed and `getpgid` fails for a dead pid.

### C. Docs, comments, version

- `docs/architecture`: agent-runtime-manager (VRAM measurement scope paragraph
  and the limitations list: no longer NVIDIA-only), telemetry doc (AMD row in
  the collector table, `pci_bus_id` now NVIDIA and AMD), building-block view
  (collector row), new ADR (KFD sysfs over start-delta attribution and over
  amd-smi; rejected alternatives), risks entry (ROCm-only visibility, ordinal
  assumption).
- Comments: `sample.GPU.PCIBusID` / `GPUInfo.PCIBusID` (no longer NVIDIA only),
  frontend `stableGpuHandle` (AMD now reports both handles), `NewNvidiaComputeApps`
  doc mention of AMD, the `dispatchMeasurement` "NO MEASURER INSTALLED" note
  listing AMD.
- Agent `Version` 0.21.0 → 0.21.1 (PATCH: observable fix, no new feature flag).

## Acceptance

- On the AMD test host: hardware report shows name, VRAM total, uuid and PCI bus
  for both cards; the spec GPU picker labels read `GPU n · AMD Radeon RX 6900 XT ·
  0000:0n:00.0`; with a GPU spec running, LIVE-STATUS / portal show a measured
  VRAM close to the KFD figure (≈ 3766 MiB for the 4B model).
- Unit tests: 3.x and 4.0.0 fixtures, sentinels; KFD ordinal mapping,
  per-process read, attribution (group mapping, unwanted pid, unknown gpu_id,
  MiB conversion, zero dropped, vanished pid, missing root → nil); selection
  rule.
- No behaviour change for NVIDIA Linux, Windows (PDH), WSL2, macOS.

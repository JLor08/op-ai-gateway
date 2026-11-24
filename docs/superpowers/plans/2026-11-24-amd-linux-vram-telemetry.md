# AMD Linux VRAM telemetry (#432) — plan

Spec: `docs/superpowers/specs/2026-11-24-amd-linux-vram-telemetry.md` (binding).

Branch-local; removed before the PR.

## Global Constraints

- Go module `server-agent/` (own module `op-ai-server-agent`); build/test from
  `server-agent/`. Frontend in `gateway/frontend/`.
- Every new Go file starts with the SPDX header used by its neighbours:
  `// SPDX-License-Identifier: AGPL-3.0-only` then
  `// Copyright (C) 2026 OnPrem AI Gateway contributors`.
- CI runs `golangci-lint fmt --diff` and `golangci-lint run` per Go module
  (gofumpt, gocritic): run both in `server-agent/` before reporting done.
  Also `go vet ./...` and `go test ./...` for `server-agent/`.
  Cross-compile check for Windows: `GOOS=windows go build ./...` and for darwin:
  `GOOS=darwin go build ./...` (build tags must keep every platform building).
- `internal/runtime/manager.go` must NOT be modified by Tasks 1-2.
- The measurer shape is `func(pids []int) map[int]map[int]int`
  (pid → GPU ordinal → MiB). A failed read returns nil, never a zero map.
  Entries ≤ 0 are dropped.
- Do not commit `docs/superpowers/**` changes in Tasks 1-2 (the controller owns
  them). Commit your code with a Conventional Commit message
  (`fix(agent): …` / `test(agent): …` / `docs: …`), with a substantive body.
- Comments state constraints the code cannot show, in the repo's existing
  explanatory style; no "changed by" history notes.

## Task 1: rocm-smi 4.x collector (names, VRAM bytes, uuid, PCI bus)

Files: `server-agent/internal/collector/amd.go`,
`server-agent/internal/collector/amd_test.go`,
new `server-agent/internal/collector/testdata/rocm-smi-4.0.json`,
`server-agent/internal/sample/sample.go` (PCIBusID comment only),
`server-agent/internal/sample/system_report.go` (PCIBusID comment only).

1. `rocmArgs` becomes exactly:
   `--showid --showproductname --showuse --showmeminfo vram --showtemp
   --showpower --showdriverversion --showbus --showuniqueid --json`
   (`"--showmeminfo", "vram"` are two argv entries). Update the `rocmArgs`
   comment: `--showmemuse` is not used because rocm-smi 4.x reports only a
   percentage there; `--showmeminfo vram` carries the byte keys in every
   version.
2. In `parseRocmJSON`:
   - `Name` = first non-empty (after sentinel filtering) of `Device Name`,
     `Card Series`, `Card series`, `Card Model`, `Card model`.
   - `UUID` = `Unique ID`; `PCIBusID` = `PCI Bus`.
   - Keep the memory, util, temp, power, driver keys as they are.
   - Add `rocmString(s string) string`: trims; returns "" for "", "N/A",
     "n/a", "NA", "unknown" (case-insensitive); else the trimmed value. Use it
     for every string field (name candidates, uuid, PCI bus, driver version).
   - Update the function comment (keys of 3.x and 4.x both accepted; which
     key wins).
3. Fixture `testdata/rocm-smi-4.0.json`: two cards, verbatim shape below
   (this is real rocm-smi 4.0.0 output from the test host, reformatted):
   ```json
   {
     "card0": {"Device Name": "AMD Radeon RX 6900 XT", "Device ID": "0x73bf", "Device Rev": "0xc0", "Subsystem ID": "0x0e3a", "GUID": "59699", "Unique ID": "0x12b1004de0189bb2", "Temperature (Sensor edge) (C)": "61.0", "Temperature (Sensor junction) (C)": "66.0", "Temperature (Sensor memory) (C)": "68.0", "Average Graphics Package Power (W)": "24.0", "GPU use (%)": "3", "PCI Bus": "0000:01:00.0", "VRAM Total Memory (B)": "17163091968", "VRAM Total Used Memory (B)": "4002840576", "Card Series": "AMD Radeon RX 6900 XT", "Card Model": "0x73bf", "Card Vendor": "Advanced Micro Devices, Inc. [AMD/ATI]", "Card SKU": "D4120100", "Node ID": "1", "GFX Version": "gfx1030"},
     "card1": {"Device Name": "AMD Radeon RX 6900 XT", "Device ID": "0x73bf", "Device Rev": "0xc0", "Subsystem ID": "0x0e3a", "GUID": "19306", "Unique ID": "0xf427f0e640d6686b", "Temperature (Sensor edge) (C)": "42.0", "Temperature (Sensor junction) (C)": "45.0", "Temperature (Sensor memory) (C)": "42.0", "Average Graphics Package Power (W)": "8.0", "GPU use (%)": "0", "PCI Bus": "0000:02:00.0", "VRAM Total Memory (B)": "17163091968", "VRAM Total Used Memory (B)": "17174528", "Card Series": "AMD Radeon RX 6900 XT", "Card Model": "0x73bf", "Card Vendor": "Advanced Micro Devices, Inc. [AMD/ATI]", "Card SKU": "D4120100", "Node ID": "2", "GFX Version": "gfx1030"},
     "system": {"Driver version": "6.8.0-147-generic"}
   }
   ```
4. Tests (TDD, write first): parse the 4.0 fixture → 2 GPUs, index 0/1, name
   `AMD Radeon RX 6900 XT`, uuid `0x12b1004de0189bb2` / `0xf427f0e640d6686b`,
   PCI `0000:01:00.0` / `0000:02:00.0`, total 17163091968, used 4002840576 /
   17174528, util 3/0, temp 61/42, power 24/8, driver `6.8.0-147-generic`.
   Existing 3.x fixture test stays green and additionally asserts empty
   UUID/PCIBusID. A sentinel test: `"Device Name":"N/A","Card Series":"AMD X",
   "Unique ID":"N/A","PCI Bus":"n/a"` → name `AMD X`, empty uuid and PCI.
   A test asserting `rocmArgs` contains `--showmeminfo`,`vram` adjacent,
   `--showbus`, `--showuniqueid`, and not `--showmemuse`.
5. `sample.GPU.PCIBusID` and `sample.GPUInfo.PCIBusID` comments: NVIDIA
   (`nvidia-smi pci.bus_id`) and AMD (`rocm-smi --showbus`); Apple/ioreg
   leaves it empty. Keep the "display aid, not an identity" rule.

## Task 2: KFD sysfs per-process VRAM measurer and selection

Files: new `server-agent/internal/collector/kfd_measurer.go` (no build tag),
new `server-agent/internal/collector/kfd_measurer_test.go` (no build tag),
new `server-agent/internal/collector/kfd_measurer_linux.go` (`//go:build linux`),
new `server-agent/internal/collector/measurer_fallback_linux.go`
(`//go:build linux`), new `server-agent/internal/collector/measurer_fallback_other.go`
(`//go:build !windows && !linux`), edit `measurer_other.go`,
edit `measurer_other_test.go`.

1. `kfd_measurer.go` (pure, root-injected; `root` is the KFD sysfs dir,
   production `/sys/class/kfd/kfd`):
   - `const kfdRoot = "/sys/class/kfd/kfd"`.
   - `func kfdGPUOrdinals(root string) (map[uint64]int, error)`: list
     `<root>/topology/nodes`; entries whose name is a non-negative integer;
     read `<entry>/gpu_id` (trimmed decimal uint64); skip gpu_id 0 (CPU node)
     and unreadable/unparseable nodes; sort GPU nodes by numeric node number;
     ordinal = rank (0-based). Error if the nodes dir cannot be listed.
     Empty map (no GPU node) is not an error.
   - `func kfdProcessVRAM(root string) (map[int]map[uint64]int64, error)`:
     list `<root>/proc`; numeric dirs = pids; in each, files named
     `vram_<decimal gpu_id>`; content trimmed decimal int64 bytes. A pid dir
     or file that cannot be read (process exited mid-scan) is skipped. Error
     only if `<root>/proc` cannot be listed.
   - `func attributeKFD(perPID map[int]map[uint64]int64, ordinals map[uint64]int,
     pids []int, groupOf func(pid int) (int, bool)) map[int]map[int]int`:
     wanted = set(pids); for each KFD pid: owner via `groupOf`; skip if !ok or
     owner not wanted; for each gpu_id: ordinal lookup, skip unknown; sum bytes
     per owner per ordinal; after summing convert to MiB (`bytes >> 20`), drop
     ≤ 0; drop empty owners; nil if nothing remains.
   - `type kfdMeasurer struct { root string; groupOf func(int) (int, bool) }`
     and `func (m kfdMeasurer) measure(pids []int) map[int]map[int]int`: nil
     for empty pids; read ordinals then per-pid VRAM; any error → nil; return
     `attributeKFD(...)`. No caching (three small sysfs reads per GPU per pid).
   - File comment: what the source is (`/sys/class/kfd/kfd/proc/<pid>/vram_<gpu_id>`,
     bytes, mode 0444, what rocm-smi --showpids reads), the ordinal rule (rank
     among GPU topology nodes = ROCr ordinal ROCR_VISIBLE_DEVICES addresses =
     rocm-smi card index), the process-group attribution (same boundary as
     `groupNvidiaComputeApps`), ROCm/HIP-only visibility, and why a stale KFD
     entry is harmless.
2. `kfd_measurer_linux.go`: `func NewKFDMeasurer() func(pids []int) map[int]map[int]int`
   — nil unless `kfdGPUOrdinals(kfdRoot)` succeeds with ≥ 1 GPU and
   `<kfdRoot>/proc` is a directory; else returns `kfdMeasurer{root: kfdRoot,
   groupOf: getpgidOwner}.measure` where `getpgidOwner(pid)` rejects
   pid ≤ 0 or > math.MaxInt32 and returns `syscall.Getpgid(pid)`; error → false.
3. Selection: `measurer_other.go` (`!windows`) `NewVRAMMeasurer()` returns
   `fallbackVRAMMeasurer()`. `measurer_fallback_linux.go`:
   `func fallbackVRAMMeasurer() …` returns `NewNvidiaComputeApps()` if non-nil,
   else `NewKFDMeasurer()` (nil when absent). `measurer_fallback_other.go`:
   returns `NewNvidiaComputeApps()`. Update `measurer_other.go`'s comment
   (NVIDIA first; on Linux KFD next; nil otherwise; NVIDIA keeps priority on a
   mixed host so every NVIDIA host is unchanged).
   Make the choice testable: in `measurer_fallback_linux.go` express it as
   `pickLinuxVRAMMeasurer(nvidia, kfd func(pids []int) map[int]map[int]int)`
   returning the first non-nil, and call it with the two constructors.
4. `measurer_other_test.go`: the existing nil-equivalence test must stay
   correct: rewrite it so it asserts `(NewVRAMMeasurer()==nil) ==
   (NewNvidiaComputeApps()==nil && kfdAbsent)` on Linux — simplest: move the
   Linux-specific assertion into a new `measurer_fallback_linux_test.go`
   (`//go:build linux`) testing `pickLinuxVRAMMeasurer` with fakes (nvidia wins
   when both non-nil; kfd when nvidia nil; nil when both nil), and keep the
   non-Linux equivalence assertion behind `//go:build !windows && !linux` in
   `measurer_other_test.go` (or adapt it). Do not leave a test that would fail
   on an AMD Linux host.
5. `kfd_measurer_test.go` (TDD first, fake sysfs tree under `t.TempDir()`):
   - ordinals: nodes 0 (gpu_id 0), 1 (59699), 2 (19306), 10 (777) → {59699:0,
     19306:1, 777:2} (numeric not lexical order: 10 after 2); missing nodes
     dir → error; node with garbage gpu_id skipped.
   - process read: pids 12834 {vram_59699: 3949375488, vram_19306: 0}, 13286
     {vram_19306: 9405710336}; a non-numeric dir ignored; a non-vram file
     (`sdma_19306`, `pasid`) ignored; missing proc dir → error.
   - attribution: groupOf identity → {12834:{0:3766}, 13286:{1:8970}}
     (3949375488>>20 = 3766; 9405710336>>20 = 8970); unwanted pid excluded; a
     worker pid 20001 with groupOf → 13286 summed into 13286; groupOf false →
     skipped; unknown gpu_id skipped; all-zero → nil; sub-MiB → dropped.
   - measure: end-to-end on the fake tree; empty pids → nil; broken root → nil.
   Verify arithmetic in tests by computing, not hand-copying, where clearer.

## Task 3: Docs, comments, version bump

Files: `docs/architecture/cross-cutting/agent-runtime-manager.md`,
`docs/architecture/cross-cutting/telemetry-usage-observability.md`,
`docs/architecture/05-building-block-view.md`,
`docs/architecture/09-architecture-decisions.md` (new ADR at the end, next
free number),
`docs/architecture/11-risks-and-technical-debt.md`,
`server-agent/internal/agent/agent.go` (`const Version` 0.21.0 → 0.21.1),
`server-agent/internal/collector/nvidia.go` (NewNvidiaComputeApps comment),
`server-agent/internal/runtime/manager.go` (comment-only: the
"NO MEASURER INSTALLED" paragraph no longer lists AMD as measurer-less),
`gateway/frontend/src/components/RuntimeAdminSection.tsx` (comment-only in
`stableGpuHandle`: AMD reports both pci_bus_id and uuid now).

- agent-runtime-manager: the "VRAM measurement is NVIDIA-only" paragraph (§5.3
  area, around `collector.NewVRAMMeasurer()`) and the limitations bullet near
  the end ("VRAM measurement is NVIDIA-only"): now NVIDIA (compute-apps / PDH)
  and AMD on Linux (KFD sysfs); still none on Apple, GPU-less hosts, AMD on
  Windows, and AMD runtimes that are not ROCm/HIP (Vulkan). Keep anchors that
  other docs link to intact (check with `make lint-docs` from repo root).
- telemetry doc: AMD collector-table row (new args, 3.x/4.x keys, name
  fallback, uuid, PCI bus); the `pci_bus_id` paragraph (NVIDIA and AMD now).
- building-block view collector row: mention the KFD measurer.
- ADR: "Per-process VRAM on AMD Linux: KFD sysfs, not start-delta attribution
  or amd-smi" — context (#432, rocm-smi 4.x drift), decision, alternatives
  rejected (start-delta attribution like WSL: needless when the kernel reports
  per process; `amd-smi process`: a subprocess and a JSON format to pin when
  the same figure is a sysfs read; DRM fdinfo: needs root to read other users'
  fds and counts graphics clients; kept as the future Vulkan route),
  consequences (ordinal assumption, ROCm-only visibility, NVIDIA priority on
  mixed hosts, ADR-083 auto budgets now real on AMD).
- risks: entry for ROCm-only visibility + ordinal assumption (KFD node rank =
  rocm-smi card index; verified on the test host, not guaranteed by a
  contract).
- Run `make lint-docs` from the repo root and fix any issue it reports.
- Run frontend `npx prettier --check src/components/RuntimeAdminSection.tsx`
  in `gateway/frontend` (comment change only).

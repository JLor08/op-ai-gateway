# Mixed NVIDIA + AMD hosts — design (issue #434)

Status: draft for approval. Branch-local working document; its durable content
moves into `docs/architecture/` before the pull request.

## 1. Intent

The operator plans to build one inference host carrying both NVIDIA and AMD
GPUs, and wants to use it two ways, depending on the model:

- **one model per vendor** (a CUDA model on the NVIDIA cards, a ROCm model on
  the AMD cards, side by side), and
- **one model across both vendors**, either as a single llama.cpp process with
  the Vulkan backend, or as an RPC cluster on that same host (a CUDA head plus
  a ROCm rpc-server member, each on its native backend).

Today the agent and the gateway model a host as having GPUs of one vendor.
GPU indices collide across vendors (`nvidia 0` and `amd 0` are both
"GPU 0"), only one vendor is measured, only one vendor's visibility variable
is set, auto-detected VRAM totals overwrite each other, and RPC clusters
refuse a second member on the same host. All of that must change together:
the operator asked for the complete feature in one pull request, not phases
shipped separately.

## 2. Goals, non-goals, anti-goals

Goals

1. A GPU identity that is unambiguous across vendors, used end to end (agent
   telemetry, hardware report, specs, budgets, measurement, gateway store,
   portal).
2. Per-process VRAM measured for **both** vendors at the same time on Linux,
   for CUDA, ROCm **and Vulkan** workloads.
3. GPU visibility chosen **per spec** from the vendor(s) of its GPUs and its
   device backend; a Vulkan spec may span both vendors.
4. `device_backend: auto` resolved per spec: NVIDIA-only GPUs → CUDA,
   AMD-only → ROCm, both → Vulkan.
5. RPC clusters may place a member on the head's own server and several
   members on one server.
6. Existing single-vendor hosts and configurations keep working unchanged,
   with old agents and with new ones.

Non-goals

- A Windows AMD GPU collector. The agent has none today (`amd.go` needs
  `rocm-smi`); a mixed Windows host is therefore not detectable and stays out
  of scope. Windows NVIDIA keeps its PDH measurer as is. *(Builder's note:
  this was decided while writing the design — the earlier conversation
  mentioned extending PDH to AMD, which is pointless without an AMD
  collector.)*
- Mixed-vendor WSL execution. WSL specs keep today's behaviour.
- Apple: unchanged (one integrated GPU, vendor `apple`).
- Intel/SYCL GPUs as a third measured vendor.

Anti-goals (would be a failure even if everything "works")

- Any behaviour change on a single-vendor host that the operator did not ask
  for. Two changes there are deliberate: `auto` is decided per spec (identical
  result on a single-vendor host), and a `vulkan` spec with
  `set_visible_devices` in env mode now gets `GGML_VK_VISIBLE_DEVICES` on
  **every** host (operator decision, see §5.3).
- A measured `0` (or a too-small measured value) silently overriding an
  operator estimate — the same rule ADR-031/ADR-113 already follow: no
  measurement is better than a wrong one.
- A legacy configuration row silently applied to the wrong vendor's card.
- Double counting VRAM (KFD + fdinfo for the same allocation, or the same DRM
  client counted once per duplicated file descriptor).

## 3. Hardware facts this design rests on

Measured on two test VMs on 2026-11-27 with llama.cpp built from the same
commit, a 4.8 GB / 6.3 GB GGUF model, one card per run:

AMD VM — 2× Radeon RX 6800 (Navi 21), Ubuntu 24.04, kernel 6.8, ROCm with KFD,
Mesa RADV:

| Model on card at PCI 0000:02:00.0 | KFD `vram_<gpu_id>` | `/proc/<pid>/fdinfo` `drm-memory-vram` | rocm-smi used |
|---|---|---|---|
| HIP/ROCm build | 3593 MiB | 3462 MiB | 3611 MiB |
| Vulkan (RADV) build | **absent** | 3323 MiB | 3340 MiB |

- amdgpu DRM fdinfo carries `drm-driver: amdgpu`, `drm-pdev: <PCI address>`,
  `drm-client-id` and `drm-memory-vram` (KiB). It covers ROCm **and** Vulkan
  allocations. KFD covers ROCm only; `rocm-smi --showpids` shows no Vulkan
  process.
- The Vulkan process held **two file descriptors per render node with the same
  `drm-client-id`**; summing per fd would double count.
- Both runs also opened the other card's render node with a few KiB to ~50 MiB
  GTT and ≤ 32 KiB VRAM.
- KFD reported ~130 MiB more than fdinfo for the ROCm run.

NVIDIA VM — 2× RTX 2080 Ti, Debian 13, kernel 6.12, driver 620.70:

- `nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory` **does** list a
  Vulkan llama-server (process type `C+G` in `nvidia-smi -q -x`) with its real
  usage (7874 MiB, matching `memory.used`). The existing measurer therefore
  already covers Vulkan on NVIDIA.
- The proprietary driver exposes no DRM fdinfo memory keys.

Vulkan device numbering:

- `vulkaninfo` reports `pciDomain/pciBus/pciDevice/pciFunction` per device on
  both vendors (VK_EXT_pci_bus_info). On NVIDIA the Vulkan `deviceUUID` equals
  the nvidia-smi GPU UUID; on RADV it encodes the PCI address.
- `vulkaninfo` also lists non-GPU devices (llvmpipe); llama.cpp's
  `--list-devices` did not.
- `GGML_VK_VISIBLE_DEVICES=1` made the second card appear as `Vulkan0`, i.e.
  it renumbers like `CUDA_VISIBLE_DEVICES`.
- On both single-vendor VMs the Vulkan order followed the PCI bus order. This
  is **not** guaranteed in general (the loader and Mesa's device-select layer
  may reorder), which is why the mapping goes through the PCI address.

`GGML_VK_VISIBLE_DEVICES`, verified on the AMD test VM against the pinned
llama.cpp source (`ggml-vulkan.cpp`, device selection):

- Its indices address the raw `vkEnumeratePhysicalDevices` order (the list
  `vulkaninfo` prints, CPU devices such as llvmpipe included in the count),
  and the **listed order is preserved**: `1,0` makes `Vulkan0` the card at
  enumeration index 1.
- An index ≥ the device count makes ggml throw ("Invalid Vulkan device
  index"); the child fails to start.
- Set to an **empty** string it selects **no** device.
- Unset, ggml uses its own filtered list (supported discrete/integrated GPUs,
  de-duplicated by UUID across drivers), not the loader order.

Not measured (no mixed host exists yet): Vulkan enumeration order on a mixed
host. See §9.

## 4. GPU identity

**Decision: a vendor-qualified key — (`vendor`, vendor-local `index`) — plus
the PCI bus address as the cross-API anchor.**

- `vendor` ∈ `nvidia`, `amd`, `apple`. The vendor-local index is what each
  vendor's stack already uses, so mapping to `CUDA_VISIBLE_DEVICES` /
  `ROCR_VISIBLE_DEVICES` stays trivial:
  - NVIDIA: the nvidia-smi index (= CUDA ordinal), unchanged.
  - AMD: the **KFD topology GPU rank** (= ROCr ordinal, what
    `ROCR_VISIBLE_DEVICES` addresses), joined to the rocm-smi rows **by PCI
    address** (KFD node `properties`: `domain` + `location_id` =
    bus<<8 | device<<3 | function). Today the collector trusts the rocm-smi
    `cardN` key to equal that rank (ADR-113's assumption). On the AMD test VM
    they are equal (rocm-smi `card0`/`card1` = KFD ranks 0/1 = buses 1/2,
    while the DRM `card0` is the virtual VGA), so existing hosts are not
    renumbered; on a mixed host the PCI join removes the dependence on
    rocm-smi's enumeration. Without a readable KFD topology the collector
    falls back to the rocm-smi key. *(Design call after the builder check.)*
- The PCI bus address (`pci_bus_id`, already reported by NVIDIA and AMD
  collectors) is what DRM fdinfo, Vulkan and Windows PDH speak; measurers and
  the Vulkan mapping resolve through it to the (vendor, index) key.
- In agent Go code the key replaces bare `int` in every GPU-keyed map
  (a small comparable struct, e.g. `GPUKey{Vendor, Index}`); in JSON and in
  the database it is two fields, `vendor` and `index` / `gpu_vendor` and
  `gpu_index`. Rendered for humans as `NVIDIA 0`, `AMD 1`.

**Legacy meaning of an empty vendor.** `vendor: ""` means "the host's only
vendor". It is what old agents report, what old gateways send, and what
existing database rows hold after the migration. Resolution rules:

- The **legacy vendor** of a host is the vendor today's agent already treats
  as "the" vendor: the first detected one in the fixed order NVIDIA, AMD,
  Apple (`runtimeGPUVendor`, `pickLinuxVRAMMeasurer`). A host that already
  reports two vendors today (an NVIDIA card plus a ROCm-visible AMD card)
  therefore keeps exactly its current meaning: empty = NVIDIA.
- Agent: a spec GPU or budget with an empty vendor resolves to the legacy
  vendor. No refusal, no model goes offline on upgrade (Goal 6).
- Gateway: on the first hardware report of a vendor-reporting agent, the
  gateway rewrites that server's empty-vendor rows (spec GPUs, budgets, RPC
  member GPUs, cluster member GPUs) to the legacy vendor computed from the
  report with the same rule, once. Afterwards every stored row carries an
  explicit vendor.
- When that report shows more than one vendor, the portal shows an
  informational note on the server that legacy GPU assignments were mapped to
  the legacy vendor and should be reviewed. *(Builder's call how it is
  surfaced; it never blocks a save.)*
- *(Design call after the builder check: the earlier draft refused empty
  vendors on mixed hosts, which would have taken existing two-vendor hosts
  offline on upgrade.)*

## 5. Agent

### 5.1 Reporting

- `sample.GPU`, `sample.GPUInfo` and `sample.RuntimeGPUSample` gain
  `vendor` (JSON `vendor`). Collectors fill it (`nvidia`, `amd`, `apple`).
- Telemetry GPUs already carry `pci_bus_id`; the stale comment in
  `CollectHardware` ("empty for every collector but NVIDIA") is corrected.
- `Agent.noteGPUTotal`, `gpuTotalsMB`, `HostInfo.GPUTotalsMB`,
  `fillDetectedBudgets` and every budget/measurement map key on the GPU key.
  This fixes the latent overwrite bug (#434 point 4) on its own.

### 5.2 Measurement

The manager's measurer type becomes `pid → GPU key → MB`. On Linux the agent
installs a **composite** measurer that runs every applicable vendor measurer
and merges their results (keys never collide because they carry the vendor):

- **NVIDIA:** the existing compute-apps measurer, unchanged in what it reads
  (it already sees CUDA and Vulkan processes), now returning keys with vendor
  `nvidia`.
- **AMD:** a new **DRM fdinfo measurer** replaces KFD as the primary source:
  - for each managed process group (same group boundary as today's KFD and
    compute-apps attribution), read `/proc/<pid>/fdinfo/*`, keep entries with
    `drm-driver: amdgpu`,
  - de-duplicate by (`drm-pdev`, `drm-client-id`) across all fds and all pids
    of the group,
  - sum `drm-memory-vram` per `drm-pdev`, convert KiB → MiB,
  - map `drm-pdev` to the AMD index through the AMD collector's PCI bus ids;
    an unmapped PCI address is dropped, not guessed,
  - drop per-card values below a small noise floor so the stray ≤ 32 KiB
    entries on cards a process merely opened do not become a measured value.
    *(Builder's call on the exact floor; it must be well below any real model
    allocation and must not turn a real 0-MiB reading into an override —
    a dropped card means "no measurement", i.e. the estimate stays.)*
  - KFD stays only as a fallback for a kernel whose amdgpu fdinfo lacks
    `drm-memory-vram`; it is never added to fdinfo values.
  - **Measurability is reported, not assumed.** Each GPU in the hardware
    report carries the backends whose per-process VRAM the agent can measure
    on it (e.g. `vram_measurable: ["cuda","vulkan"]` for NVIDIA,
    `["rocm","vulkan"]` for AMD with fdinfo, `["rocm"]` for AMD on the KFD
    fallback, absent/empty when nothing). The agent determines fdinfo support
    by a self-probe (open an amdgpu render node, read its own fdinfo for a
    `drm-memory-vram` key). The gateway's estimate/`--tensor-split`
    exemption (`memberGPUMeasurable`, `kfdHost`) uses this list when present
    and keeps today's rule for reports without it. *(Design call after the
    builder check.)*
- **Windows:** unchanged (PDH, NVIDIA only).
- The ~130 MiB KFD/fdinfo difference on ROCm is accepted; fdinfo is the one
  source that covers Vulkan, and ADR-084's max(measured, estimate) plus the
  run-peak history absorb the small under-read. *(Design call.)*
- `/proc/<pid>/fdinfo` is readable for processes of the agent's own user. The
  builder verifies the privileged-helper setup (#438), where the agent runs
  unprivileged, still has its children under a user it can read; if not, the
  AMD measurer degrades to "no measurement" for those processes, never to 0.

### 5.3 Visibility and device backend per spec

- `SpecGPU` gains `vendor`. `GPUBudget` gains `vendor`. `Status.MeasuredVRAM`
  and the runtime GPU samples carry vendor + index.
- The single host-wide `GPUVendor` used for visibility is replaced by a
  per-spec decision:

  | Spec GPUs | `device_backend` | Visibility variable (env mode) | Device names |
  |---|---|---|---|
  | NVIDIA only | `auto`/`cuda` | `CUDA_VISIBLE_DEVICES` = NVIDIA indices | `CUDA0..` |
  | AMD only | `auto`/`rocm` | `ROCR_VISIBLE_DEVICES` = AMD indices | `ROCm0..` |
  | any vendors | `vulkan` (or `auto` with both vendors) | `GGML_VK_VISIBLE_DEVICES` = Vulkan ordinals | `Vulkan0..` |
  | both vendors | `cuda` or `rocm` | refused (`gpu_vendor_backend_mismatch`) | — |
  | NVIDIA only | `rocm` (or AMD only + `cuda`) | refused (same code) | — |

- `auto` is decided from the spec's own GPUs; a spec without GPUs keeps
  today's host-level rule. On a single-vendor host this is identical to today.
  On a mixed host a single-vendor spec now gets its native backend instead of
  Vulkan — a deliberate change.
- **The Vulkan row applies on every host, single-vendor ones included**
  (operator decision). Today a `vulkan` spec with `set_visible_devices` in env
  mode gets `CUDA_VISIBLE_DEVICES` / `ROCR_VISIBLE_DEVICES`, which the Vulkan
  driver ignores, so its pinning has no effect. After this change it gets
  `GGML_VK_VISIBLE_DEVICES` and its devices are `Vulkan0..` in declared order.
  A spec with a literal `--device VulkanN` in its args must be adapted; the
  architecture docs and the PR description say so. Without
  `set_visible_devices` (or in args mode) a single-vendor host keeps today's
  behaviour (host index = Vulkan index, documented caveat); a mixed host
  refuses it (§5.4). Loader ordinals are never emitted without injection,
  because ggml then numbers its own filtered list (§3).
- `GGML_VK_VISIBLE_DEVICES` joins the vendor-independent set of variables a
  `set_visible_devices` spec may not hand-set (`visibleDevicesOwnedVars`); the
  portal enforces the same rule.
- `${VULKAN_DEVICES}`, `${VULKAN_FIRST_DEVICE}`, `${DEVICES}` and
  `${FIRST_LOCAL_DEVICE}` emit `Vulkan0..k-1` in declared order when the agent
  injected `GGML_VK_VISIBLE_DEVICES`; without injection they keep today's host
  indices and the documented caveat "Vulkan2 is not necessarily host GPU 2"
  stays for that path only. `${HOST_GPU_IDS}` keeps emitting
  vendor-local indices and is refused for a spec spanning both vendors
  (it has no meaning there).

### 5.4 Vulkan ordinal mapping

- The agent runs `vulkaninfo` (when on PATH) at start and with each hardware
  refresh, parses each physical device's PCI address and device type, and
  builds `PCI address → Vulkan enumeration ordinal`. Non-GPU devices
  (`PHYSICAL_DEVICE_TYPE_CPU`, llvmpipe) are excluded from what the agent maps
  to cards but **not** from the ordinal count, because
  `GGML_VK_VISIBLE_DEVICES` addresses the loader's enumeration order.
  *(To verify — §9.)*
- The mapping is part of the hardware report (`vulkan_ordinal` per GPU, absent
  when unknown) so the portal can show it.
- A PCI address that appears more than once in `vulkaninfo` (two Vulkan
  drivers for one card, e.g. AMDVLK and RADV) makes that card's mapping
  **unavailable** rather than picking a driver. *(Design call.)*
- Without injection, ggml-vulkan numbers its own **filtered** list (discrete
  GPUs first, de-duplicated across drivers), which is not the loader order the
  mapping is built from. Therefore, on a **mixed** host a Vulkan spec that
  declares GPUs must run with `set_visible_devices` in env mode (the agent
  injects `GGML_VK_VISIBLE_DEVICES` and the child's devices are `Vulkan0..`
  in declared order); a Vulkan spec with GPUs but without that option is
  refused there (`vulkan_visible_devices_required`). This removes every
  "raw ordinal" path on mixed hosts. *(Design call after the builder check;
  the builder confirms ggml-vulkan's selection rules against the pinned
  llama.cpp source and the §9 VM checks.)*
- Without `vulkaninfo`, or when a spec GPU's PCI address is missing from its
  output: a Vulkan spec on a **single-vendor** host falls back to today's
  behaviour (host index = Vulkan index, the documented caveat applies); on a
  **mixed** host it is refused with a stable error
  (`vulkan_mapping_unavailable`), because guessing there pins the wrong
  vendor's card.

### 5.5 RPC members

- `rpc_member.go` (member `-d` list, `memberDevicePrefix`) and
  `rpc_classify.go` (`expectedMemberDevices`) use the per-spec backend and the
  vendor-aware GPU list. A member's GPUs must be single-vendor unless its
  backend is Vulkan (same table as §5.3).
- If the agent needs any change to accept a member bound to `127.0.0.1` or a
  head endpoint on `127.0.0.1` (§6.3), it ships under the same feature flag.

### 5.6 Wire contract and version

- New feature flag **`gpu_vendor_identity`** (name is the builder's call if a
  better one fits the registry's style): the agent reports `vendor`, accepts
  `vendor` on spec GPUs and budgets, measures per vendor, decides visibility
  per spec and maps Vulkan ordinals.
- If same-host clusters need agent changes, a second flag
  (`rpc_cluster_same_host`) with the same `Since`.
- One MINOR bump of `const Version` for the branch (current `0.26.0` →
  `0.27.0`, or the next free MINOR at merge time).
- A gateway talking to an agent **without** the flag sends no `vendor`
  fields, keeps today's single-vendor behaviour, and refuses saving a
  vendor-qualified GPU or a same-host cluster member on that server with the
  existing "agent lacks feature" pattern.

## 6. Gateway and portal

### 6.1 Store (append-only migrations, all three drivers)

- Add `gpu_vendor text not null default ''` to `agent_runtime_spec_gpus`,
  `ai_server_gpu_budgets`, `rpc_member_spec_gpus`, `rpc_cluster_member_gpus`,
  and include it in each primary key (`… , gpu_vendor, gpu_index`). SQLite
  needs table rebuilds for the key change; Postgres and memory follow the
  `dialect` seam (persistence.md). Verify with
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- `rpc_cluster_members`: the key moves from `(head_spec_id, server_id)` to
  `(head_spec_id, position)` (already unique), and `rpc_cluster_member_gpus`
  references the member by `(head_spec_id, position)`. The automatic member
  id `rpca_<head>_<server>` gains a disambiguator for a second member on the
  same server. *(Builder chooses the exact id scheme; it must stay stable for
  existing single-member-per-server clusters so running members are not
  relaunched by the upgrade.)*
- JSON-only GPU payloads (`server_telemetry_samples.gpus_json`,
  `server_availability_samples.gpus_json`, `server_hardware.report_json`)
  simply carry the new `vendor` (and `pci_bus_id` in telemetry) fields; no
  migration.

### 6.2 Ingest and runtime document

- `agentGPUReport`, `agentGPUInfo`, `agentRuntimeGPUSample`,
  `routing.GPUSample` gain `vendor` (+ `pci_bus_id` for telemetry).
- `latestGPUSnapshotByIndex`, measured-VRAM write-back
  (`UpdateRuntimeSpecGPUMeasured`), `cluster_usage.go` and every
  `map[int]…` of GPU data key on (vendor, index).
- `AgentRuntimeSpecGPUDTO`, `AgentGPUBudgetDTO`, the member spec DTOs carry
  `vendor` — only to agents with the flag.
- The empty-vendor backfill and ambiguity warning from §4.
- `gpuLooksNVIDIA` / `gpuLooksAMD` / `serverHostGPUs` use `vendor` when the
  report has it and keep the name heuristic only for vendor-less reports from
  old agents. Measurability comes from the per-GPU `vram_measurable` list
  (§5.2) for the spec's resolved backend; `kfdHost` ("every GPU is AMD")
  remains only for reports without that list.
- Budget drift check (`expected_uuid` / `expected_name`) compares the card at
  the same (vendor, index).

### 6.3 Clusters on one host

- `ErrClusterMemberOnHeadServer` and `ErrClusterMemberServerTwice` are no
  longer refusals when the server's agent has the flag (they remain for
  servers without it).
- A member on the head's server is addressed and bound on `127.0.0.1`
  (loopback), independent of the cluster's `mesh`/`lan` network; NetBird and
  LAN readiness are not required for that member. Members on other servers
  keep today's addressing.
- Several members on one server get distinct ports (the existing
  `(server_id, port)` uniqueness already guarantees it).
- Head and member (or two members) sharing a card stays the existing
  `cluster_member_gpu_overlap` warning, not a refusal.
- Cluster usage, peak and budget checks key members by position, not server.

### 6.4 Portal UI (DE + EN keys together)

- GPU pickers (runtime spec, budgets, cluster member, manual RPC member
  editor) list GPUs as `NVIDIA 0 · <name>`, `AMD 1 · <name>` and store
  (vendor, index). On single-vendor hosts the vendor label may be omitted
  *(builder's call)*, but the stored value always carries the vendor once the
  agent has the flag.
- `device_backend` selection shows what `auto` resolves to for the chosen
  GPUs and refuses the mismatch combinations of §5.3 at save time.
- The cluster editor allows choosing the head's own server as a member and the
  same server more than once.
- The hardware table shows vendor and, when known, the Vulkan ordinal.
- The ambiguity warning from §4.

## 7. Documentation

- New ADR (next free number, ADR-122 at time of writing): mixed-vendor GPU
  identity, fdinfo measurement, per-spec visibility, Vulkan mapping,
  same-host clusters. ADR-113's "NVIDIA wins on a mixed host" note is marked
  superseded by it.
- Update `agent-runtime-manager.md` (§3.3, §3.6, §3.7, §5.1, §5.3, mixed-host
  notes), `reference/data-model.md` (columns, keys, migration list),
  `reference/api-surface.md` (new error codes, DTO fields),
  `telemetry-usage-observability.md` (§8.2.2, §8.2.5),
  `11-risks-and-technical-debt.md` (backend enumeration order item),
  `12-glossary.md` (`device_backend`), `persistence.md` if the rebuild pattern
  needs a note.

## 8. Testing

- Unit tests for every new rule, with **fixtures copied from the real
  measurements in §3** (fdinfo dumps with duplicated client ids and stray
  cards, `vulkaninfo` output from both VMs including llvmpipe, KFD values).
- Store conformance tests for the migrations on memory, SQLite and Postgres,
  including legacy empty-vendor rows and the one-time backfill.
- Gateway tests for: flag/no-flag agents, ambiguity warning, same-host member
  addressing, the mismatch refusals.
- Portal tests for the pickers, the backend resolution hint, the cluster
  editor; `format:check`.
- The usual gates: golangci-lint per module, `make lint`, archtest, the
  frontend suite, Sonar branch findings.

## 9. Verification on real hardware

Available now (two single-vendor VMs, SSH):

1. Run the new AMD fdinfo measurer and NVIDIA compute-apps measurer (as a
   small test harness or the built agent's measurer entry point) against real
   HIP, Vulkan and CUDA llama-server processes; compare with the numbers in
   §3.
2. Verify the Vulkan ordinal mapping on both VMs, including whether
   `GGML_VK_VISIBLE_DEVICES=1,0` keeps the listed order (`Vulkan0` = ordinal
   1) or sorts it, and adjust the renumbering rule of §5.3 to the observed
   behaviour.
3. Verify a same-host cluster on one VM: head on card 0 (CUDA or ROCm),
   rpc-server member on card 1, loopback addressing.

Not possible until the operator's mixed host exists: a real mixed
CUDA+ROCm+Vulkan run. The pull request states this explicitly.

## 10. Left to the builder

The exact Go type names, the noise floor value, the automatic member id
scheme (stable for existing clusters), how the ambiguity warning is surfaced,
whether the portal hides the vendor label on single-vendor hosts, the flag
names if the registry suggests better ones, and the phase split of the
implementation plan (one pull request).

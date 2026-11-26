# Spec: exempt measurable ROCm members from the VRAM-estimate and tensor-split rules

Branch-local working document; removed before the pull request.

## Problem

Saving a cluster head whose automatic member runs on an AMD GPU is refused with
`400 runtime_spec.member_vram_estimate_required` unless every member GPU has a
`vram_estimate_mb`. The head is also refused `runtime_spec.cluster_tensor_split_required`
without `--tensor-split`. Manual member specs (`PUT /api/portal/servers/{id}/rpc-members`)
have the same estimate rule (`requireRPCMemberVRAMEstimates`).

Both rules exist because a non-NVIDIA GPU was not measured per process. Since
ADR-113 (#433, agent 0.21.1) the agent on **Linux** measures AMD per-process VRAM
from KFD sysfs, but only for ROCm/HIP processes, and only when no NVIDIA GPU is
present (NVIDIA compute-apps has priority, so the KFD measurer is not installed
on a mixed host). The portal rules still treat every non-NVIDIA GPU as
unmeasurable.

## Facts the design rests on (verified in code)

- Agent `ResolveDeviceBackend` (`server-agent/internal/runtime/cluster_devices.go`):
  explicit `rocm` → device prefix `ROCm`; `""`/`auto` with exactly one detected
  vendor AMD → `ROCm`; several vendors → `Vulkan`.
- A member is launched with `-d ROCm<n>,…` for the ROCm prefix. A binary that is not
  a ROCm/HIP build fails with `unknown device` / `No devices found`, classified
  `runtime.device_backend_mismatch` (`rpc_classify.go`). So a member that RUNS with
  the ROCm prefix is a HIP process, which is what the KFD measurer sees.
- `owner.measurementTargets` (`manager.go`) hands every live process, rpc_server
  members included, to the VRAM measurer.
- Linux measurer selection: nvidia-smi compute-apps if present, else KFD, else none.
  Windows: PDH (no AMD per-process figure). Apple: none.

## Rule (new)

A member GPU is **measurable** when all of the following hold, read from the member
server's stored hardware report and the member's device backend:

1. The report's `os` is `linux`.
2. The report lists at least one GPU, and **every** listed GPU looks AMD by name
   (case-insensitive `amd`, `radeon` or `instinct`, the same keywords `gpuLooksNVIDIA`
   already uses). This excludes mixed NVIDIA/AMD hosts (KFD not installed, and `auto`
   resolves to Vulkan) and Apple hosts.
3. The member's device backend is `rocm`, or `auto`/`""` (which on an AMD-only host
   resolves to ROCm on the agent). `vulkan`, `cuda`, `metal`, `sycl` are not measurable.

Where a GPU is measurable:

- `runtime_spec.member_vram_estimate_required` no longer applies to it: automatic
  members in `checkClusterMemberGPUs` and manual member specs in
  `requireRPCMemberVRAMEstimates` (which must now also take the spec's device backend).
- `runtime_spec.cluster_tensor_split_required` no longer counts it as a reason (a
  member — automatic or manual — all of whose known non-NVIDIA GPUs are measurable
  does not require `--tensor-split`).

Everything else is unchanged: Apple budgets, NVIDIA behaviour, unknown-vendor
GPUs (no report / index not listed: estimate stays optional, as today), Windows,
Vulkan, mixed hosts. Error codes stay stable; only the human-readable messages change.

## Messages

The current text "required for every GPU on a server without NVIDIA GPUs" is wrong
(the rule is per GPU). New texts (backend `msg` in `portal_rpc_member_endpoints.go`
and the matching `ErrRPCMemberVRAMEstimateRequired` mapping in the cluster endpoints if
any; frontend `i18n.ts` German + English):

- estimate required — EN: "A VRAM estimate is required for every GPU whose VRAM the
  agent cannot measure (non-NVIDIA, except AMD on Linux with the ROCm or auto backend)."
  DE: "Für jede GPU, deren VRAM der Agent nicht messen kann (nicht NVIDIA, außer AMD
  unter Linux mit dem Backend ROCm oder auto), ist eine VRAM-Schätzung nötig."
- tensor split — EN: "A head with a member on a GPU whose VRAM the agent cannot
  measure needs --tensor-split." DE accordingly.

## Accepted risk

There is no capability signal for "a VRAM measurer is installed" (ADR-025 forbids
version gates; measurement is deliberately not a feature flag). An agent older than
0.21.1 on such a host, or a host without `/sys/class/kfd/kfd/proc`, measures nothing;
the member then stays unknown-demand until measured or given an estimate (the same
state an NVIDIA member is in before its first measurement, §5.3). The KFD route was
verified with `llama-server`, not with `rpc-server`. Both go into the risks table.

## Docs

- `docs/architecture/cross-cutting/agent-runtime-manager.md` save-check paragraph (~2190).
- `docs/architecture/reference/api-surface.md` lines for rpc-members and save step 9.
- `docs/architecture/reference/openapi.yaml` if it describes the condition.
- ADR-113 consequences: one bullet that the portal exempts measurable ROCm members.
- `docs/architecture/11-risks-and-technical-debt.md`: extend the AMD row.

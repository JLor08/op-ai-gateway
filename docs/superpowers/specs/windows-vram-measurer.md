# Spec — per-process, per-GPU VRAM measurement on Windows

Branch-local working document (AGENTS.md). Durable content folds into
`docs/architecture/cross-cutting/agent-runtime-manager.md` §5 before the PR.

## Problem

`agent-runtime-manager.md` §5.3 says measurement works on "an NVIDIA host". On
**Windows that is false**: the WDDM driver model puts the OS, not the NVIDIA
driver, in charge of GPU memory, so
`nvidia-smi --query-compute-apps=pid,gpu_uuid,used_memory` reports `[N/A]` for
`used_memory`. Only the TCC driver model restores it, and TCC disables display
output and is unavailable on most GeForce — not an option for these hosts.

### The live bug this exposes

`naInt("[N/A]")` returns **0** (`collector/nvidia.go`), so on Windows
`attributeComputeApps` produces a **non-nil** `map[pid]map[gpuIndex]0`. In
`runtime/manager.go` `buildSnapshot`:

```go
if v, ok := byGPU[g.Index]; ok { gpus[g.Index] = v; continue } // measured 0 WINS
gpus[g.Index] = g.VRAMMB                                       // estimate never reached
```

There is no `<= 0` guard, so **every managed model process on Windows is charged
0 MB VRAM for admission**, the GPU budget looks entirely free, and co-residency
admission loses exactly the OOM protection it exists for.

The gateway's ingest **does** guard this (`gateway/agent_ingest.go:367`:
`if g.VRAMMeasuredMB <= 0 { continue }`), exactly as §5 documents. So the 0 is
never persisted — the portal shows `vram_measured_mb = 0`, indistinguishable
from "never measured", while the agent locally behaves as if the model needs no
VRAM. The missing local guard hides its own symptom.

## The chain, proven on the operator's hardware

Verified by a standalone probe (3x NVIDIA, driver 610.62), not from docs:

1. PDH counter `\GPU Process Memory(*)\Dedicated Usage`, instances named
   `pid_<PID>_luid_0x<HighPart>_0x<LowPart>_phys_<N>` → per-PID, per-LUID bytes.
2. `D3DKMTOpenAdapterFromLuid` + `D3DKMTQueryAdapterInfo(KMTQAITYPE_ADAPTERADDRESS)`
   (gdi32 exports, user-mode callable) → `{BusNumber, DeviceNumber, FunctionNumber}`.
3. `nvidia-smi --query-gpu=index,pci.bus_id` → the same PCI address → GPU index.

Measured facts that the implementation must honour (each cost a probe run):

| Fact | Consequence |
|---|---|
| Field order is `0x<HighPart>_0x<LowPart>` | parse High first |
| ONE `PdhCollectQueryData` suffices (raw gauge) | no second sample, no sleep |
| `PdhAddEnglishCounterW` worked directly | locale-proof; do not use localized names |
| One LUID (`0x0_0x16026`) fails with `STATUS_INVALID_PARAMETER`, retried 6x | **negative results must be cached**; unresolvable LUIDs are normal, skip them |
| Accuracy 0.04–0.8% vs `nvidia-smi memory.used` | good enough for admission; KB4490156 over-report did NOT appear |
| Attribution agreed with nvidia-smi for 15/15 PIDs | the bridge is cross-validated |
| The operator's model spans 3 GPUs | multi-GPU splitting is a real requirement; D3DKMT is load-bearing |
| `shared`/`non_local` read 4694 MiB **identically on all three GPUs** | they are NOT per-GPU; use `dedicated` only, claim no spillover detection |

## Design

**Same contract, no caller changes.** `func(pids []int) map[int]map[int]int`
(pid → gpuIndex → MB) via `runtime.Manager.SetMeasurer`. Fail-soft: nil means
"nothing measured this cycle", which the manager already handles.

**CI does not build or vet for Windows** (`ci.yml` is `ubuntu-latest`,
`go test ./...` only). So the split follows the house precedent of
`wmi_map.go` / `hwinfo_windows.go`:

- `nvidia_pdh.go` — **no build tag**: instance-name parsing, `pci.bus_id`
  parsing, the aggregation into the nested map. Unit-tested on Linux CI, where
  the logic most likely to be wrong actually gets exercised.
- `nvidia_pdh_windows.go` — `//go:build windows`: PDH and D3DKMT syscalls, the
  positive **and negative** LUID caches, plus compile-time struct-layout
  assertions (a wrong offset would silently yield a garbage PCI address and a
  false "unsupported", so it must fail the build instead).

**On Windows the compute-apps measurer must NOT be installed.** It is the
source of the zero-override bug. Selection is PDH-or-nothing there; nothing is
strictly safer than a measured 0, because nothing falls back to the estimate.

## Scope boundaries

- The zero-guard in `buildSnapshot` is a **separate commit**: it changes
  admission on every platform (a measured 0 stops overriding an estimate) and
  is separable from the Windows work. Applying the house `<= 0` rule to the one
  path that lacks it.
- ~~No admission-logic change beyond that guard.~~ **Widened deliberately by
  the operator ("es gehört einfach mit dazu"), so it lands on this branch
  rather than a follow-up.** Adding the zero-guard is what put the question in
  front of us: the guard makes an unmeasured process fall back to the
  operator's estimate, and that estimate is `0` — *unknown* — for exactly the
  specs this feature exists to serve. Following that value into `Admit` showed
  the unknown-demand class was only half enforced: both unknown-VRAM rules
  keyed on the CANDIDATE's spec, while an unknown-demand **occupant** was
  charged `0 MB` by the per-GPU arithmetic and ignored — even when busy, even
  when pinned. Verified by executing `Admit`, not inferred:

  | Snapshot | Before | After |
  |---|---|---|
  | occupant of unknown demand, idle | `OK`, `Evict=[]` | `Evict=[a]` |
  | occupant of unknown demand, busy | `OK`, `Evict=[]` | `Wait` |
  | occupant of unknown demand, pinned | `OK`, `Evict=[]` | `pending_vram_unknown` |
  | same occupant declaring 6000 MB | `Evict=[a]` | `Evict=[a]` (unchanged) |
  | mirror: unknown CANDIDATE, idle occupant | `Evict=[a]` | `Evict=[a]` (unchanged) |

  So the Windows measurer would have shipped its better numbers into an
  arithmetic that silently revoked the "may start only alone on that GPU"
  guarantee on the next admission — the promise this whole measurement chain
  is supposed to be the way OUT of. Shipping the two together is the point.
  The fix is a symmetric explicit rule (rule 5), not a larger charge: the
  arithmetic's eviction loop releases a victim with `sum -= r.GPUs[idx]`,
  which subtracts the same `0`, so a charge-based fix evicts every idle
  process on the card and then answers `Wait` anyway. Durable description:
  `agent-runtime-manager.md` §5.2 (evaluation order) and §5.3 (the symmetry,
  the rejected arithmetic fix, and why terminal is not permanent here).
- No admission-logic change beyond the guard and that rule. The measurer
  still only supplies better numbers to the existing arithmetic.
- No `shared`/`non_local` spillover feature: unproven, see the table.
- Agent `Version` bump: PATCH (`0.2.2` → `0.2.3`). No `agent.Features` entry —
  measurement is a hardware capability, not a negotiated flag (§5.3, ADR-025).

# Probe reachability + model runtime-status presentation — Design

**Status:** draft for review
**Date:** 2026-09-07
**Builds on:** #47 (RuntimeSpec type + per-model context size & live metrics via
server-agent probing). Part 2 of this discussion (richer per-model telemetry:
live throughput, cache stats, modality auto-detect) is **out of scope** and
tracked separately in issue #49.

## Goal

The server-agent already probes each managed child's `/metrics` and context
endpoint per cycle, but two things are unclear to the operator today:

1. **A forgotten probe endpoint is invisible.** If a child's metrics endpoint is
   unreachable (e.g. `--metrics` not passed to llama.cpp), the probe fails
   silently (a `slog.Debug`, values left at `0`). The operator has no signal.
2. **`0` is ambiguous.** In the Models views, active/queue/context show `0`
   whether the value is truly zero or simply not measured — misleading.

Plus two related presentation clean-ups the operator asked for.

## Scope (three parts)

- **A — Probe reachability**: the agent reports a three-state reachability per
  probe (metrics, context); the server-agent admin table shows it; the Models
  detail table stops showing a misleading `0`.
- **B — Consolidated model status**: merge the Models-detail table's separate
  "Geladen" (loaded) + "Live-Status" columns into one tri-state "Status" column.
- **C — Loading count**: add a "Lädt" column to the Models overview table.

---

## A. Probe reachability

### A.1 Agent — compute the three-state per probe

In `server-agent/internal/agent/agent.go` `probeRuntimeChild`, compute two
states and set them on the `RuntimeSample`:

- `metrics_probe` ∈ `ok` | `unreachable` | `na`
  - `na`   — `MetricsPath == ""` (this type has no metrics endpoint, e.g. Ollama,
             or a custom spec with no path). Not an error.
  - `unreachable` — `MetricsPath != ""` but the scrape could not produce a value:
             connection refused (the "forgot `--metrics`" case), non-2xx, a parse
             failure, **or** an unsafe path the SSRF guard refused to dial.
  - `ok`   — the scrape returned values.
- `context_probe` ∈ `ok` | `unreachable` | `na`
  - `na`   — `ContextProbePath == ""`.
  - `unreachable` — path set but the probe failed, was refused (unsafe), or
             returned a non-positive size (unknown).
  - `ok`   — the probe succeeded (size > 0) **or** a cached size is in effect
             (a cache hit means a prior probe succeeded for this child+config).

Only set for `StateRunning` children (`probeRuntimeChild` is only called for
those). A non-running child leaves both `""` → no indicator downstream.
Everything stays best-effort: computing these states never changes the
probe's non-fatal behaviour.

The value semantics ("" = not reported/not running) mean legacy agents and
non-probing agents are byte-neutral: they simply never populate the fields.

### A.2 Wire

Extend the existing per-runtime telemetry channel (no new array):

- `sample.RuntimeSample` gains `MetricsProbe` / `ContextProbe` (`string`, json
  `metrics_probe` / `context_probe`).
- `gateway/backend/internal/gateway/agent_ingest.go`: `agentRuntimeSample` gains
  the two fields; `runtimeStatusDTOsFromSamples` maps them onto
  `RuntimeStatusDTO` (`runtime_registry.go`), which also gains them.
- `injectRuntimeModelState` (`portal_model_endpoints.go`) injects them onto
  `ModelServerDTO` (alongside state/active/queue/context), gated on
  `runtime_model_probe` exactly like the active/queue injection is.

The frontend `RuntimeStatus` and `ModelServerRow` TS types gain
`metrics_probe` / `context_probe: string` (snake_case, no mapper).

### A.3 `RuntimeAdminSection` — "Probes" column

Add one compact **"Probes"** column to the mapping live-status table (the
existing `live_status` StatusChip column stays). It renders **two** small
StatusChips per row:

- **M** (metrics) and **C** (context), each: green (`ok`), amber/red
  (`unreachable`), neutral/"–" (`na`). A tooltip states the meaning
  ("metrics endpoint not reachable — check the server's `--metrics`/endpoint
  argument").

Shown only for running children with a reported state (`metrics_probe` /
`context_probe` non-empty).

### A.4 `ModelServersSection` — "—" instead of a misleading `0`

Drive the display off the reachability signal:

- **Aktiv** and **Warteschlange** render `—` when `metrics_probe != "ok"`;
  the real number (**including a real `0`** = truly idle) only when `ok`.
- **Kontext** renders `—` when `context_probe != "ok"`; the real size only when
  `ok`.

No new nullable numeric fields are needed — the single reachability signal is the
"is this value real" gate. A non-probing agent (fields empty) therefore also
correctly shows `—` rather than a fabricated `0`.

---

## B. `ModelServersSection` — consolidated "Status" column

Replace the two separate columns **"Geladen"** (the `loaded` boolean) and
**"Live-Status"** (`runtimeLiveStatus`, the runtime state chip) with **one
"Status" column**, tri-state:

- **Geladen** — runtime `state == "running"`.
- **Lädt** — runtime `state == "starting"`.
- **Nicht Geladen** — otherwise.

Fallback for rows with no runtime state (a non-`server_agent` model server, where
`state == ""`): use the existing `loaded` boolean — **Geladen** if loaded, else
**Nicht Geladen**. Reuse the shared `runtimeStateBadge` / `runtimeStateLabel`
(already extracted to `components/shared/runtimeState.ts`) for the chip
colour/label vocabulary so "Lädt" matches the loading indicator used elsewhere.

---

## C. `ModelList` — "Lädt" count column

Add a new column to the Models overview table (`ModelList.tsx`), positioned
**between "Angeboten" and "Geladen"**:

- **Lädt** — a StatusChip showing the **count of servers currently loading**
  (runtime `state == "starting"`) this model, styled **yellow** (`status="watch"`),
  rendered like the "Geladen" count chip (shown only when the count > 0).

Backend: `ModelOption` (`gateway/frontend/src/api/models.ts`, backed by the
models-list endpoint) gains `loading_on_count` — analogous to the existing
`offered_on_count` / `loaded_on`. It is computed from the volatile runtime-status
registry: the number of servers offering this model where the model's managed
spec is currently `starting`. (Implementation note: find where the model-list
DTO is built and where `loaded_on` / `offered_on_count` are derived, and add the
`starting`-count from the same `RuntimeStatus` registry the per-model injection
already reads. Gated on `runtime_model_probe`/registry availability; absent →
count 0, column hidden for that row.)

---

## Data flow (summary)

```
agent probeRuntimeChild
  -> RuntimeSample{ metrics_probe, context_probe }   (+ existing fields)
  -> agentRuntimeSample -> RuntimeStatusDTO           (gateway ingest)
       -> RuntimeAdminSection "Probes" column (A.3)
       -> ModelServerDTO (injectRuntimeModelState)    (gated on runtime_model_probe)
            -> ModelServersSection "—" gate (A.4) + "Status" column (B)
  RuntimeStatus registry (state == "starting")
       -> ModelOption.loading_on_count -> ModelList "Lädt" column (C)
```

## Error handling / edge cases

- Probing stays best-effort — states are advisory, never reject a sample.
- Non-running child → both states `""` → no indicator.
- Non-probing agent (no `runtime_model_probe`) → no probe fields → no indicators,
  and active/queue/context render `—` (correct: genuinely unknown).
- Ollama (`metrics_probe == "na"`): the "Probes" M-chip shows the neutral "–"
  (no metrics endpoint), NOT a red "unreachable" — so the correct absence of a
  metrics endpoint is not mistaken for a misconfiguration.

## Testing

- **Agent**: `probeRuntimeChild` sets the correct state for each case (na / scrape
  error / ok / unsafe path; context: na / err / size≤0 / cache-hit / ok).
- **Wire**: `RuntimeSample` round-trip carries the two fields.
- **Gateway**: `runtimeStatusDTOsFromSamples` maps them; `injectRuntimeModelState`
  injects them on `ModelServerDTO` (gated); `ModelOption.loading_on_count` counts
  `starting` servers.
- **Frontend**: `RuntimeAdminSection` renders M/C chips with the right
  status/`data-status`; `ModelServersSection` shows `—` vs the number per the
  reachability gate and the consolidated "Status" tri-state (incl. the non-agent
  `loaded` fallback); `ModelList` renders the yellow "Lädt" count between
  Angeboten and Geladen.

## Out of scope

Richer per-model telemetry harvested from the same endpoints — live tokens/sec,
prefix/KV-cache stats, and modality auto-detect (llama.cpp `modalities`, Ollama
`capabilities`) — is deliberately **not** in this feature. Tracked in issue #49.

# Spec — the VRAM benchmark: load one model alone, then measure what it costs

Branch-local working document (AGENTS.md). **A design decision document for
work that does NOT land on this branch.**

> **Read this before folding anything anywhere.** AGENTS.md says every
> branch-local spec is either folded into `docs/architecture/` or deleted
> before the pull request. Neither applies cleanly here: the feature is
> unimplemented, so folding it into the architecture docs would document
> behaviour the system does not have — the one thing those documents may never
> contain. So: **do not fold this into `docs/architecture/`**, and do not let
> the pre-PR cleanup delete it without first carrying it onto the follow-up
> branch (create that branch, or re-add the file there, *before* the cleanup
> commit — a squash merge drops this commit from `main`'s history entirely).
> Everything below is a proposal with named open questions, not settled
> behaviour.

## 0. Review corrections — three premises were FALSE, and four arguments were weak

The operator's design (load the model alone, then measure what it costs) is
unchanged. What follows is what a review of the first revision found, verified
in the tree rather than reasoned about, with the revision each finding forced.
Read this before D2/D3: **the paragraphs below supersede any older wording that
survives elsewhere in this file.**

| # | The premise as it was written | What is actually true | Where |
|---|---|---|---|
| P1 | D2 force-stops every other enabled spec and waits, bounded, for each to report a **transition** to `stopped`. | For a sibling that is **not running**, `admin_state: force_stopped` "does nothing at all — no state change, no frame". So the bounded wait **exhausts for every idle sibling**, and the run reports an isolation timeout on a server that is already isolated. | §11.2 of `agent-runtime-manager.md`, and `EnsureRunning`'s own guard: `manager.go:989` returns `ErrAdmissionBlocked` for a `force_stopped` spec, but a spec with no live process transitions nowhere. Every spec *is* present in each status frame (`snapshotStatus`, `manager.go:2316`), so its state is readable — there is simply no *edge* to wait for. |
| P2 | `runtime_max_processes = 1` was rejected **because it is unobservable** while `force_stopped` "is applied on push **and** produces a `stopped` frame per spec". | The observability asymmetry does not exist for a spec that is not running: no frame, **no `admin_state` field in `RuntimeStatusDTO` at all** (`runtime_registry.go:36-46`), and no applied-config version anywhere in telemetry (§3.5). So the stated ground is empty. The two levers must be re-weighed on **effect**, and there the difference is real and larger — see D2. | `runtime_registry.go:36-46`; `runtimeStatusDTOsFromSamples` (`agent_ingest.go:708`) drops the sample's `gpus` too. |
| P3 | D3's sequence drains, takes a baseline, then loads the target. | **The target is never drained.** D2 step 2 refuses if the *target* carries an override and step 3 writes only to every **other** spec — and the shared load core short-circuits on an already-resident model (`load_runner.go:35-38`: `modelResident` → `Loaded = true`, no load at all). So for the commonest trigger — an operator probing a model that is currently loaded — the baseline window **already contains the model**, both post-load windows are stable, and the run reports a *definitive* `delta_mb ≈ 0`. That is the first thing an operator would probe, and `0` in this house means *unknown*. | `load_runner.go:35-38`, D2 step 3. |

Four arguments that were weak rather than wrong:

- **W1 — strategy (a) has no freshness signal.** `RuntimeSpecGPU` carries
  `{SpecID, GPUIndex, VRAMEstimateMB, VRAMMeasuredMB}` and **no timestamp**
  (`routing/store.go:1274-1279`), and `writeBackRuntimeVRAM` deliberately
  **does not rewrite an unchanged value** (`agent_ingest.go`, "AN UNCHANGED
  MEASUREMENT IS NOT REWRITTEN"). Polling for "a positive value appears"
  therefore reads a number from last week as this run's result — and requiring
  it to *change* fails in the normal case where the true value is the same.
  Strategy (a) needs watermark discipline exactly like the `stopped` frame
  does; D3 says how.
- **W2 — the version-bump argument is not decisive.** D2 rejected the
  agent-side "measure now, isolated" frame partly because this branch has
  already spent its one bump on a PATCH. True, and irrelevant: **this feature
  does not land on this branch**, and a follow-up branch is free to spend its
  own single bump on a MINOR to `0.3.0`. The honest reasons are different ones
  (D2).
- **W3 — the deferred restore can revert a concurrent operator edit.**
  `PutRuntimeSpec` is a full-document replace (ADR-029) over a
  read-modify-write with no compare-and-set, and the same lost-update class is
  already recorded for `UpdateMapping` in
  [§11.1](../../architecture/11-risks-and-technical-debt.md#111-operational-risks).
  A restore that re-PUTs a document captured *before* the run silently reverts
  every edit an operator made during it. It must re-read immediately before
  writing; see D2.
- **W4 — the GPU-UUID drift safeguard is empty on exactly the hosts the delta
  strategy exists for.** `sample.GPU.UUID` is populated only by the NVIDIA
  collector; `collector/amd.go`'s ROCm parse and `collector/apple.go`'s ioreg
  parse never set it. So on AMD and Apple the "record the UUID next to the
  index" safeguard stores an empty string and warns about nothing. D3 replaces
  it with a fingerprint that degrades.

## 1. What was asked for

The operator's words:

> "wir sollten einen weiteren Benchmark einführen oder die Funktion in
> bestehende ergänzen. VRAM Benchmark. der dann auch wenn das Model mit anderen
> geladen werden dürfte, nur das Model auf dem Server lädt, dann die VRAM
> Benutzung misst. entweder direkt per Quelle. oder vorher alle verwendete GPUs
> VRAM merken und die Differenz zu wenn das Model geladen ist nehmen."

Three requirements, in the order they constrain the design:

1. **Isolation.** The run loads the target model **alone**, *even on a server
   where co-residency would permit company*. Isolation is the requirement, not
   a side effect of an idle host.
2. **Measurement, two strategies, either acceptable:**
   (a) "direkt per Quelle" — the per-process measurement the agent already
   takes; (b) "Differenz" — remember every used GPU's VRAM before the load and
   subtract once the model is resident.
3. It is a **benchmark**: an operator-triggered, reported measurement, in the
   same place the other benchmarks live.

Strategy (b) is the valuable half, and the reason is structural rather than
stylistic: it is the only one that produces a number on a host with **no
per-process measurer at all**. §5.3 states that case as a dead end in prose —
"On those hosts the operator's `vram_estimate_mb` is the only number there will
ever be […] Waiting for the measurement to arrive is waiting for something that
will not happen" — and §13 repeats it as an accepted limitation. AMD reports
per-GPU totals but no per-PID split (`collector/amd.go:87`), Apple reports
unified-memory totals (`collector/apple.go:75`), a GPU-less host reports
nothing. All three can still be *differenced*.

## 2. Why now: this closes a loop the branch beside it opens

The occupant fix landing on `windows-vram-measurer` (rule 5, see
`specs/windows-vram-measurer.md`) makes an **unknown-demand occupant
blocking**: idle → evicted, busy → `Wait`, pinned → `pending_vram_unknown`
(or `not_permitted` naming the closed cell, when the pair is not co-resident
either — a missing estimate is not what blocks that one).
That is correct, and it makes the cost of an unfilled `vram_estimate_mb`
visible and immediate instead of silent.

Which raises the operator's question directly: *how do I resolve an unknown
demand on purpose?* Today the only answers are "guess an estimate" or "wait for
a measurement", and the second one is unavailable on exactly the hosts §5.3
names. **This benchmark is the deliberate answer.** The two changes are halves
of one story: rule 5 makes unknown demand a real block, and the VRAM benchmark
is the operator's way through it.

## 3. What already exists (evidence)

Everything in this section was read, not assumed. **Line numbers are as of this
branch's tip**, re-derived after the admission-rule review fixes — not as of
`8318c0e`, which this section declared while shipping on top of later commits:
the rule-5 commit alone added ~40 lines inside `agent-runtime-manager.md`
§5.2/§5.3 and ~90 to `policy.go`, shifting every reference below them (§5.6's
levers table, once cited at `:1186-1204`, now starts at `:1299`). Where a number
can shift again, the symbol or section name beside it is the durable anchor —
re-derive, do not trust, an `NNNN` that no longer lands on what it names.

### 3.1 Run lifecycle: one run per server, reserved, cancellable

| Fact | Where |
|---|---|
| `BenchmarkRegistry` tracks **at most one in-flight run per server**, keyed by server id; volatile, nil-safe | `gateway/backend/internal/gateway/benchmark.go:59-95` |
| `TryStart(serverID, scope, mode, total, now, cancel)` is the reservation; `(nil,false)` when one is already in flight → HTTP 409 `benchmark.already_running` | `benchmark.go:83`, `benchmark_endpoints.go:64-69` |
| A reserved server is **excluded from new routing** (`ServerBusy` feeds the resolver's `ServerBusyChecker`), and a pre-run **idle gate** refuses (409 `benchmark.server_in_use`) when in-flight requests remain | `benchmark.go:98-106`, `benchmark_endpoints.go:70-80` |
| `Release` forgets a reservation whose idle gate failed; a run that *executed* keeps a terminal status so the poll can read results | `benchmark.go:108-119`, `benchmark_runner.go:486-492` |
| The run executes on `context.Background()` with a `cancel` stored on the run, so it **outlives the trigger request** | `benchmark_endpoints.go:63`, `:85-88` |
| Every streaming call is bounded by an **always-on idle watchdog** (2 min default) because a benchmark has no client to end it — a stall would leave the server reserved forever | `benchmark_runner.go:29-37`, `:61-76` |

### 3.2 Modes, and the two runs that are not modes

`parseBenchmarkMode` accepts exactly `"" → speed`, `speed`, `capacity`,
`both`, `vision`; anything else is HTTP 400 `benchmark.mode_invalid`
(`benchmark_endpoints.go:161-171`). It gates the three **fan-out** endpoints
(mapping / application / server scope), each of which loops
`runBenchmark`'s per-target `switch mode` over every authorized mapping
(`benchmark_runner.go:404-478`).

Two runs deliberately sit *outside* that mode set, each with its own endpoint
and its own single-target runner — and they are the precedent this feature
follows:

| Run | Endpoint | Runner | Persists? |
|---|---|---|---|
| Context probe | `POST /api/portal/mappings/{id}/probe-context` | `runContextProbe`, `benchmark_runner.go:484-523` | **No** — "REPORTS the size through the run status. It does NOT persist (the frontend fills the form field; the user saves manually)" |
| Load model | `POST /api/portal/mappings/{id}/load` | `runLoadModel`, `load_runner.go:19-45` | No |

`BenchmarkResult.Loaded` belongs to the second one — "true when a load action
confirmed the model resident (report-only; a load run)"
(`benchmark.go:36-37`), set at `load_runner.go:36,43`. **There is no "load"
mode**; `TryStart(server.ID, "load", "load", 1, …)`
(`benchmark_endpoints.go:136`) records that string on the status for the UI,
and `runLoadModel` is called directly. The load run short-circuits when the
model is already resident, then best-effort re-probes the app's loaded set so
the model-servers SSE flips immediately (`load_runner.go:35-38,44`).

**Scheduled runs are `"speed"` only** (`benchmark_scheduler.go:90`), so a new
run kind is not swept into the scheduler by accident.

### 3.3 Progress, status, persistence

- **SSE**: `Subscribe` returns (snapshot, channel, unsubscribe) per server;
  `publish` fans a **full status** out non-blockingly, so a slow reader drops
  frames and recovers on its next snapshot (`benchmark.go:196-252`). Frames go
  out after each measured target and once more after `finish`
  (`benchmark_runner.go:406-409`, `:476`). Wire: `GET
  /api/portal/servers/{id}/benchmark/events`, events `snapshot` + `progress`,
  both carrying the whole `BenchmarkStatus` (`perf_endpoints.go:241`,
  `frontend/src/api/servers.ts` `subscribeBenchmark`). The **poll**
  (`…/benchmark/status`) remains the completion authority.
- **Persistence**: `model_mapping_benchmarks`, one row per mapping per run,
  written best-effort (`InsertBenchmarkRun`, `store/sqlite_benchmarks.go:19-54`);
  a history-write failure never fails the run. Columns:
  `gen_tokens_per_second, prompt_tokens_per_second, load_time_ms,
  context_size, error, kind, capacity_curve, vision_capable`. `kind`
  discriminates (`speed` | `capacity` | `vision`; empty = legacy speed), and a
  **kind-specific payload gets its own column** — `capacity_curve` holds opaque
  JSON the store never parses (`routing/store.go:329-341`), decoded in the
  portal DTO only for `kind=="capacity"` (`portal/service_benchmark.go:106-111`).
  Latest migration is **70** (`store/migrate.go:103`), so a new column is 71.
- **Nullability precedent**: `VisionCapable *bool` — "nil = not run or
  inconclusive (nothing persisted); non-nil true/false = a definitive result"
  (`benchmark.go:42-44`), and the runner writes **nothing** on nil while still
  recording a history row (`benchmark_runner.go:437-454`). That is exactly the
  shape a VRAM result needs, and `CapacityReport` is the precedent for the
  nested-JSON half. **Both apply**: a nullable pointer to a nested per-GPU
  struct.

### 3.4 Where a per-GPU number can come from

| Source | Shape | Cadence / precision | Where |
|---|---|---|---|
| `routing.ServerTelemetry` | `VRAMUsedBytes`, `VRAMTotalBytes` — **host aggregate, not per GPU** | latest row only | `routing/store.go:231-251`; used by the capacity ramp at `benchmark_capacity.go:107`, `:406-425` |
| `routing.TelemetrySample.GPUs[]` = `GPUSample{Index, Name, UUID, UtilPct, MemUsedBytes, MemTotalBytes, …}` | **per GPU, host index space, with UUID** | agent pushes every `OP_AGENT_INTERVAL`, **default 1 s** (floor 250 ms) | `routing/store.go:264-292`, `:374-385`; cadence: `telemetry-usage-observability.md` §"`OP_AGENT_INTERVAL` \| 1s" |
| Live per-GPU samples, gateway side | `s.ServerPerf` ring, cap 1200 samples, **volatile**, plus SSE fan-out | as above | `server_perf.go:11-19,47`, field at `server.go:138` |
| Persisted per-GPU history | `server_telemetry_samples.gpus_json` | same samples, retained | `store/migrate.go:478`; read via `TelemetrySamples(serverID, from, to, limit)` |
| Per-process measurement (strategy a) | `agent_runtime_sample.gpus[].vram_measured_mb` per `spec_id` | dispatched on the child's **first health pass** and on the owner's **15 s** housekeeping beat | agent: `runtime/manager.go:82` (`idleTickInterval`), `:1327` (`dispatchMeasurement`), `:1853` (health-pass dispatch); gateway: `agent_ingest.go:102-110`, `:335` |

Precision of the underlying reads: NVIDIA `memory.used` is MiB, multiplied to
bytes (`collector/nvidia.go:126-127`); ROCm reports bytes
(`collector/amd.go:87-88`); Apple reads ioreg in-use/alloc
(`collector/apple.go:75-76`). **The noise floor of a delta is other processes,
not quantization.**

The ingest guard: `if g.VRAMMeasuredMB <= 0 { continue }`
(`agent_ingest.go:367`) — a measured 0 means *unknown* and is never persisted;
`writeBackRuntimeVRAM` additionally suppresses an unchanged value and only
writes for a spec `resolveRuntimeSpecWritable` confirms belongs to the
reporting server and is not `vram_locked` (`agent_ingest.go:261-329`).

### 3.5 What the gateway can and cannot tell the agent

- Control is **desired state, not commands** (ADR-026,
  `09-architecture-decisions.md:227-240`). The agent's WebSocket reader
  understands exactly four frame types — `cert_update`, `ca_update`,
  `runtime_config`, `runtime_log_config` — and discards everything else for
  forward compatibility (`server-agent/internal/client/ws.go:619-630`). Two of
  those are bare wakes; `runtime_log_config` carries the **full desired set**
  of watched spec ids. ADR-026's closing sentence is the rule for anything
  imperative: *"A genuinely imperative action would need its own frame type
  behind its own feature flag."*
- **There is no acknowledgement that a pushed config was applied.** In gateway
  mode the agent sends no applied-ETag anywhere: the ETag exists on the
  document the agent *fetches* (`agent_runtime.go:84-86`) and inside the
  file-mode upward report (`agent_runtime.go:316`), and the runtime report is
  fetched once per (api, server) with no polling refresh (§13). The only
  observable evidence that a config change took effect is its **effect** on the
  status stream.
- Desired-state levers the gateway *does* own, and how each applies (§5.6 "What
  a pushed config applies, and what it does not",
  `agent-runtime-manager.md:1299-1316`):

  | Lever | Applied on push? |
  |---|---|
  | spec removed from the document | immediately — the spec drains |
  | `admin_state: force_stopped` | **immediately** — a running child drains |
  | lowered per-GPU budget | **not retroactive** — binds at next start |
  | removed co-residency pair | **not retroactive** |
  | lowered `runtime_max_processes` | **not retroactive** |

- Push immediacy depends on transport: a WS-connected agent gets it at once
  (gated on the `runtime_manager` feature, `agent_runtime.go` `PushRuntimeConfig`);
  a POST-transport agent picks it up on its **60 s** runtime poll
  (`agent/agent.go:233`). §11.2's 120 s restart bound is derived from that 60 s
  plus the 60 s backoff cap, and its "Insurance" note says plainly that nothing
  in either codebase enforces the link.
- Enumeration: `RuntimeSpecsByApplication(appID)` lists a server's specs, and
  **at most one `server_agent` application exists per server** (portal
  enforcement plus the partial unique index, migration 68,
  `store/migrate.go:2953-2958`), so that one call covers the whole server.
  Live state per spec is in `s.RuntimeStatus` (`server.go:197`), whose only
  read today is `subscribe` (snapshot + channel, `runtime_registry.go:259`).

### 3.6 The ownership boundary this feature must not break

- `vram_estimate_mb` is **operator-owned**; `vram_measured_mb` is
  **agent-owned**. `PutRuntimeSpec` ignores what the request sends for the
  measured value and copies the stored one forward
  (`portal/service_runtime.go:368-379` for the rule, `:445-460,497` for the
  code). `UpdateRuntimeSpecGPUMeasured` is the only writer
  (`routing/store.go:1382`), reached only from the telemetry write-back.
- §5.1 states why: *"The split of VRAM ownership is the load-bearing rule of
  the whole budget feature: a future PUT handler that starts trusting
  `vram_measured_mb` from the request lets a UI round-trip erase real
  measurements, after which the agent's admission arithmetic uses estimates it
  has already disproved."* The long blockquote that follows explains
  `vram_locked` as the operator's **only** lever — *"the operator chooses
  whether to be governed by the measurement, never what it says"* — and records
  that a "reset the measured value to 0" button was **rejected**.
- The document the agent receives carries measured-over-estimate, unless
  locked (`portal/service_runtime.go:1441-1450`). So anything written into
  `vram_measured_mb` **feeds admission arithmetic directly**, and a wrong value
  there is the exact trap §5.1 describes: a measured breach of a budget is
  *terminal* `not_permitted`.
- The portal side of the same rule: §11.4 — a screen shows both numbers and
  edits only its own, and **"a form does not send a field it does not let you
  edit"**. The per-GPU rows in the launch-spec form already look exactly like
  this: an editable estimate input (`RuntimeAdminSection.tsx:3421`) beside a
  read-only measured line (`:3431`).

## 4. Decisions

### D1 — A new run kind with its own endpoint, sharing the load core

**Decision.** `POST /api/portal/mappings/{id}/probe-vram`, reserving the server
through `TryStart(serverID, "vram-probe", "vram", 1, …)`, with a single-target
runner `runVRAMProbe`. Not a fifth `?mode=` value, and not a change to the
existing load run.

**Why not a `mode`.** Every `mode` value is a **per-target** measurement inside
a fan-out loop over an application's or a server's mappings
(`benchmark_runner.go:410-477`). A VRAM run is not per-target: it drains the
whole server *once*, then loads exactly *one* model. Adding it as a mode would
either silently measure only the first target or drain-and-reload the server N
times inside one reservation — and on the server scope, a run an operator reads
as "measure my models" would stop every model on the box. The isolation is
destructive enough that it must be asked for one model at a time.

**Why not extend the load run.** `runLoadModel` is already 90% of the body
(reserve → resident-probe → one 1-token stream → confirm), and the portal's
"Load" button calls it. Adding isolation there would make that button stop
every other model on the server — a surprise with no affordance warning about
it. So: a **new runner that shares the load core**, extracted as a helper both
call (`ensureResidentForRun(ctx, tgt) (loaded bool, err error)`), leaving
`runLoadModel`'s behaviour byte-identical.

**Open question O1.** Whether an application/server-scoped **sweep** (drain
once, then measure each mapping in turn while keeping the rest stopped) is
worth adding later. It is strictly more useful and strictly more destructive;
defer until the single-model run is proven in the field.

### D2 — Isolation is `admin_state: force_stopped` on EVERY spec, target included, because it is the only lever that refuses a start

**Decision.** Before the baseline, the run:

1. Enumerates every spec of the server's one `server_agent` application
   (`RuntimeSpecsByApplication`), **the target included**.
2. **Refuses to start** (409, a new code `benchmark.vram_isolation_blocked`,
   naming the spec) if **any** of them — target included — already carries a
   non-empty `admin_state`, or if any spec **other than the target** is
   `pinned`. Rationale below.
3. Writes `admin_state: force_stopped` to every **enabled** spec of that
   application, **the target among them**. Writing it to the *running* ones only
   would leave a window in which a request through the agent's router starts one
   mid-measurement; writing it to the *others* only leaves the target itself
   resident, which is exactly how the baseline came to contain the model (P3).
4. **Partitions** what it wrote by the first status frame that resolves *after*
   the write, and waits only for what it can observe. A spec with a live process
   (`running`, `starting`, `draining`) is waited for, bounded, until a `stopped`
   transition **later than the run's own write** (§11.2's watermark discipline:
   a `stopped` frame that predates the write proves nothing). A spec in a
   no-process state (`stopped`, `pending_vram_unknown`, `not_permitted`,
   `crashed`, `start_failed`, `backoff`) is **already isolated** and is
   *confirmed*, never awaited — `force_stopped` with no live process produces no
   state change and no frame at all, so waiting for one is waiting out the
   timeout (P1). A spec still running when the bound expires is a genuine
   isolation timeout.
5. Baseline, then the measurement (D3) — which clears the **target's** override
   to start it, and nothing else's.
6. **Restores** every override it set back to `""`, in a `defer`, on a context
   that is *not* the run's own, **re-reading each spec immediately before
   writing it** (W3).

**Why this and not the alternatives:**

- **Rely on rule 4 (unknown demand starts alone).** Insufficient, twice over:
  it applies only to a spec whose demand is *already* unknown — the "verify a
  suspicious estimate" case is the other half of what was asked — and it forces
  solitude only on **that spec's own GPUs** (`specGPUIndexes`,
  `policy.go:196-202`; the toucher loop at `:573-587`), while a delta wants a
  quiet baseline on every GPU it reads. Rule 5, its occupant-side mirror, does
  not widen that: it blocks a *newcomer* from the unknown spec's cards, which is
  the same card set seen from the other end. **And rule 4 no longer clears a
  card at all**: under the precedence the branch beside this one settled
  (`known demand beats unknown demand`,
  [ADR-032](../../architecture/09-architecture-decisions.md#adr-032--known-vram-demand-outranks-unknown-the-unknown-side-blocks-never-evicts)),
  an unknown-demand candidate facing an occupant of *known* demand **blocks**
  instead of evicting it. So the rule an operator would most expect to produce
  isolation now produces a `Wait`, which is one more reason the run has to
  create the isolation itself.
- **Temporarily set `runtime_max_processes = 1`.** One write instead of N, and
  rule 2 would then let the target's *own* admission evict the others using the
  agent's machinery. The old ground for rejecting it — that it is unobservable
  while `force_stopped` is observable — **does not hold** (P2): for a spec that
  is not running, neither lever produces a frame, `RuntimeStatusDTO` has no
  `admin_state` field at all, and no applied-config version rides telemetry.
  **Rejected on effect instead, which is the larger difference:**
  - `force_stopped` **refuses a start.** `EnsureRunning` returns
    `ErrAdmissionBlocked` for a `force_stopped` spec before any admission
    arithmetic runs (`manager.go:989`), and the reconcile pass drains one that
    is up (`manager.go:923`) — *ahead* of the pinned/`force_running` start
    branch, so it outranks `pinned` as well.
  - `runtime_max_processes = 1` **licenses a displacement in the wrong
    direction.** A router request for a sibling asks the agent to admit *that
    sibling*, and rule 2 then looks for as many victims as the limit demands —
    with the target the only running process and idle between the run's own two
    probe requests, **the target is the victim**. The cheap lever hands any
    inbound request permission to kill the thing being measured, and the run
    would compute a delta across the target's own restart. It is also not
    retroactive (§5.6), so it does nothing about what is already up.

  So the conclusion survives its own rejected argument, on a ground that is
  true: `force_stopped` is the only lever that makes "alone" hold for the
  duration. The observability the old text claimed is real for a *running*
  spec — which is exactly why step 4 partitions instead of waiting on all of
  them.
- **A new imperative "measure now, isolated" agent frame.** Still the
  semantically best answer, and **not** excluded by the version rule the way the
  first revision claimed (W2): this feature is its own branch, and that branch
  may spend its single bump on a **MINOR** to `0.3.0`. The agent already owns
  the measurement, the drain, the eviction and the health probe, so it could
  isolate, measure and report a number it legitimately owns — no ownership
  crossing (D4), and **no crash window**: a gateway that dies between drain and
  restore leaves the whole fleet force-stopped (R1), while an agent that dies
  takes its own transient overrides with it. What argues for building the
  gateway-side drain **first** is fleet compatibility, not versioning: ADR-026
  requires the frame behind its own feature flag and ADR-025 forbids gating on a
  version number, so the run must cope with an agent that does not declare the
  flag — which means the gateway-side path has to exist anyway, as the fallback
  for every agent already in the field. Build the fallback, then treat the frame
  as the end state that retires R1. Two things to keep in view when it is
  written: the agent's number would be its **per-process** measurement, so a
  host with no measurer still needs the gateway-side delta; and the frame must
  carry its own bounded, self-restoring isolation, or it reintroduces R1 inside
  the agent.

**Why the refusals in step 2 are not timidity — and one of their stated reasons
was wrong.** The `admin_state` refusal is what makes the restore unambiguous:
the run only ever restores to `""`, so it never has to reconstruct what an
operator's override was — which, after a gateway restart, it could not know. The
**pinned** refusal was justified by "a pinned spec cannot be drained at all",
and that is **false**: `force_stopped` is tested before the pinned start branch
in the reconcile pass (`manager.go:923-930`) and `beginDrain` carries no pinned
guard, so a pinned *running* spec is stopped like any other. Keep the refusal,
on the honest ground: `pinned` is an operator's standing instruction that this
model stays up, and silently breaking it for a benchmark is a worse surprise
than refusing and naming it. The **target** may be pinned — stopping the target
is the point of the run, and the operator asked for this run on this model — so
the affordance must say plainly that the target will be stopped and restarted.
Every refusal names the blocking spec, the same discipline that made
`not_permitted`'s message load-bearing in §5.2.

**Traps this sequence must not fall into** (each already documented elsewhere,
for another feature, in this exact shape):

- **Full-document replace.** `PutRuntimeSpec` is a full replacement (ADR-029),
  so the `admin_state` write must be built by **spreading the loaded spec** and
  replacing one field — never by assembling a field list. The rule lives in
  **§11.1** ("An `admin_state` write builds its PUT body by rest-spreading the
  actual loaded spec"), not §11.4, and §11.1 also records what the
  assembled-body version costs: a body that has quietly reset the operator's
  binary path, args, timeouts and GPU rows, with a test asserting only that
  `admin_state` came out right passing anyway. (§11.4's neighbouring rule — "a
  form does not send a field it does not let you edit", whose cost was a spec
  form that re-enabled a disabled mapping on every save — is about the mapping
  PATCH's pointer fields, a different endpoint and a different mechanism.)
  A Go caller has no `...rest` spread, so this needs one
  named `putRequestFromDTO(RuntimeSpecDTO) PutRuntimeSpecRequest` helper with a
  test that fails when a field is added to the spec and not to the mapper.
  **Open question O2**: whether a narrow internal `SetRuntimeSpecAdminState`
  (one column) is preferable to the full replace. It would sidestep the mapper
  entirely, at the cost of a second writer of the spec row and a deliberate
  exception to ADR-029. My recommendation is the mapper plus its guard test,
  because ADR-029's rule is about *the runtime domain*, not about one endpoint.
- **The restore must not run on the run's context.** The run body's context is
  cancelled when the run finishes or is cancelled (`benchmark_endpoints.go:85-88`),
  which is precisely when the restore matters most. Use
  `context.WithoutCancel` (or a fresh background context) with its own timeout.
- **The restore must re-read, never replay** (W3). A full-document replace of a
  spec captured before the run reverts every field an operator edited *during*
  it — a launch spec is exactly what an operator opens while a model is
  stopped, and this endpoint is a read-modify-write with no compare-and-set,
  the same lost-update class §11.1 records for `UpdateMapping`. So: re-read the
  spec, spread *that* document, set `admin_state: ""`, PUT. And if the
  freshly-read `admin_state` is no longer the `force_stopped` this run wrote,
  **do not write at all** — someone else now owns the field; record the spec in
  `restore_failed` so the portal can say so.
- **Completion on a transition, not a state** — §11.2, verbatim reasoning: a
  `stopped` frame predating our write completes the wait immediately, and the
  run would then measure a server that is still draining. **But only for the
  specs that have a process to stop** (P1): for the others there is no
  transition to come, and a wait that demands one converts an
  already-isolated server into an isolation timeout. State and transition are
  both needed, for disjoint halves of the set.
- **A bounded wait, and no silent clearing on timeout.** §11.2 chose *not* to
  clear an override on timeout, because the portal cannot tell a wedged child
  from a slow one. A benchmark's calculus differs: it *created* the overrides,
  so leaving them is a strictly worse failure than clearing them. **Decision:**
  on timeout, abort the measurement and **still attempt the restore**, and
  report both facts (`error` = "isolation timed out", plus the list of specs
  whose restore failed). This is a deliberate divergence from §11.2 and must be
  written down as such wherever it lands.

**Named residual risk R1.** If the gateway process dies between the drain and
the restore, every other model on that server stays `force_stopped` until an
operator clears it by hand. Mitigations in scope: the run status carries the
drained set so the portal can say so; the refusal in step 2 means the state to
restore is always exactly `""`. A **persisted isolation lease** (reconciled on
gateway start) is the only real fix and is **out of scope** — it is also the
strongest argument for the agent-side capability, which has no such window
because the agent both owns and restores its own state. Record this in
[§11.1 Operational risks](../../architecture/11-risks-and-technical-debt.md#111-operational-risks)
when the feature lands.

### D3 — Both measurement strategies, reported side by side, with an honesty gate

**Decision.** One run produces, per GPU, up to two independent numbers, and
says which it got:

- **`measured_mb` (strategy a, "direkt per Quelle").** After the load, the
  agent measures its own child on the **first health pass** and every **15 s**
  beat. **This crosses no ownership boundary — it reads a number the agent
  produced.** On a host with no measurer it simply never appears, which is the
  case strategy (b) exists for.

  **It must be read with a watermark, and the stored row cannot supply one**
  (W1). `RuntimeSpecGPU` has no timestamp, and `writeBackRuntimeVRAM`
  deliberately does not rewrite an unchanged value, so "poll
  `RuntimeSpecGPUs(specID)` until a positive `vram_measured_mb` appears" reads
  a value from an arbitrarily old run as this run's result — and the obvious
  patch, requiring the value to *change*, fails in the normal case where this
  run measures exactly what the last one did. Nor can the run wait for the row
  to be *written*: the write is suppressed precisely when the answer is
  unchanged. Two honest ways out, in preference order:
  1. **Carry the measurement on the volatile status stream and time-stamp it
     there.** The per-spec measurement already reaches the gateway once per
     second inside `agentRuntimeSample.gpus[].vram_measured_mb`, and
     `runtimeStatusDTOsFromSamples` (`agent_ingest.go:708`) throws it away —
     `RuntimeStatusDTO` has no GPU field. Add one (plus the sample's arrival
     time, or a monotonic frame counter), and strategy (a) gets the same
     watermark discipline the `stopped` frame has: accept only a value carried
     by a frame that arrived **after the load completed**. Volatile RAM, no
     migration, no agent change, no new endpoint — the run is already
     subscribed to that stream for D2's isolation wait, so it is one
     subscription serving both purposes. The side benefit is a live measured-VRAM
     reading in the portal, which today only exists as a stored number of
     unknown age.
  2. **Drop strategy (a) from the run** and report the delta alone, leaving the
     measured column to be read where it already is. Cheaper, and it loses the
     cross-check that is half the value of running both.

  Whichever is chosen, the fallback rule is the same: **strategy (a) reports
  nothing rather than something stale.** `measured_mb` stays absent unless a
  post-load frame carried it.
- **`delta_mb` (strategy b, "die Differenz").** From `s.ServerPerf`'s per-GPU
  samples: `used_after − used_before`, per GPU index, in MB.

The sequence, and every step exists for a reason:

1. Drain (D2) and confirm — **the target included**. A baseline taken while the
   target is resident measures nothing at all (P3).
2. **Baseline**: require `K` consecutive samples (proposal: K=3, i.e. ~3 s at
   the 1 s cadence) in which each watched GPU's `mem_used_bytes` varies by no
   more than `tol` (proposal: max(64 MiB, 1%)). Not stable → **inconclusive**,
   report nothing.
3. **Clear the target's override only**, then load it through the shared load
   core. Its siblings stay `force_stopped`, so the target starts alone without
   any admission arithmetic having to be trusted: `force_stopped` refuses their
   starts outright (D2).
4. **The resident short-circuit is now a contamination signal, not a shortcut.**
   `ensureResidentForRun` reports whether it actually loaded anything
   (`load_runner.go:35-38` returns `Loaded = true` without loading when the
   model is already resident). After step 1 confirmed the target stopped, a
   model that reports resident is being served by something the gateway did not
   stop — a non-managed application on the same host, most likely. **Report
   inconclusive and say so**; never a delta.
5. Settle, then the **same stability gate** on the post-load samples →
   `delta_A`.
6. One tiny generation request (the load core already sends a 1-token stream) →
   settle → stability gate → `delta_B`. Report `max(delta_A, delta_B)` as the
   headline and keep both.
7. **Floor gate.** A confirmed-resident model whose headline delta is below a
   floor (proposal: `tol`) is **inconclusive**, not a measurement: no model
   costs ~0 MB, so such a number can only mean the window missed the
   allocation or something else absorbed it. `0` means *unknown* everywhere
   else in this feature and must mean it here too.
8. Restore (D2) — every override, target included.

Step 6 (the generation request) is not padding. llama.cpp preallocates its KV cache at load, so
`delta_A` is already the steady state there; a backend that allocates on first
use does not, and a number that omits the first KV allocation is an
*understated* demand — which §5.3 names as "exactly how a co-resident pair
reaches an OOM". Reporting the max of two observations is cheap and is the
conservative direction.

**Which GPUs are watched.** The spec's declared GPU rows
(`RuntimeSpecGPUs`) when it has any — that is the index set admission actually
uses. When it has none, watch every GPU and report each index whose delta
exceeds a floor, marked **unattributable** (there is no row to apply it to).

**Record a card fingerprint next to the index, and make it degrade** (W4). A
stored VRAM number attributed to index 1 after the cards were renumbered is
worse than no number, so the result carries what identified the card it was
measured on and the portal warns on a mismatch — the same mechanism the GPU
budget rows already use, where `ExpectedUUID`/`ExpectedName` are "a purely
descriptive drift detector […] compared against live telemetry to WARN that a
card was renumbered" (`routing/store.go:1296-1322`). **But `GPUSample.UUID` is
NVIDIA-only**: `collector/amd.go`'s ROCm parse and `collector/apple.go`'s ioreg
parse never populate it, so a UUID-only detector is empty on exactly the two
host classes the delta strategy exists to serve. So the fingerprint is
`{uuid, name, mem_total_bytes}` with the **strongest available** field
recorded and *named*:

| Host | Fingerprint | What drift it can catch |
|---|---|---|
| NVIDIA | `uuid` | any renumbering |
| AMD (ROCm) | `name` (`Card series`/`Card model`) + `mem_total_bytes` | a swap between *unlike* cards; two identical cards trading indices are indistinguishable, and the result must say so rather than imply a check it did not make |
| Apple | index 0 only, `name` | nothing to renumber — one integrated GPU. What matters here instead is that `mem_used`/`mem_total` are **unified system memory** read from ioreg (`collector/apple.go:75-76`), not dedicated VRAM, so the number must be labelled as such wherever it is shown |

The portal shows "verified by UUID" or "verified by name and total size only" —
never a bare "verified".

**What contaminates a delta, and what the design does about each:**

| Contaminant | Handling |
|---|---|
| Another **managed** spec starting/allocating mid-window | Closed by D2: `force_stopped` prevents a start, not merely an eviction |
| A **non-managed** application on the same server (llama-swap and the managed runtime coexisting is an explicit migration path, §13) | Not drained — out of the gateway's reach. A *static* neighbour cancels out of the delta; a *moving* one trips the stability gate and the run reports inconclusive |
| A client hitting the **agent's router port directly** — it authenticates nothing and its shipped default binds all interfaces (§4.6) | The benchmark reservation only excludes *gateway* routing. Trips the stability gate at best; must be named as a limitation |
| The **display/compositor** on a workstation GPU (the Windows case this whole chain came from) | A constant is absorbed by the delta; window activity during the window is drift, caught by the stability gate |
| **Driver reserve**, ECC overhead | Constant, absorbed by the delta. Note the consequence: a delta is *the model's marginal cost*, while the agent's per-process measurement is *that process's attributed usage* — they are not the same quantity and may legitimately differ. Report both; never average them |
| Sampling **quantization** (1 MiB on NVIDIA) | Below `tol`; irrelevant next to the above |
| **Shared/host-spillover** memory | Explicitly out of scope. The Windows probe found `shared`/`non_local` reading identically on all three GPUs, so it is not per-GPU (see `windows-vram-measurer.md`). Claim nothing about spillover |

**Open question O3.** `K` and `tol`, and the per-phase settle. The proposals
above are reasoned, not measured. They should be validated on the operator's
3-GPU Windows host — the same hardware that produced the PDH findings — before
the constants are frozen, and they must be `var`s so tests can shorten them
(the `coldLoadPollGap`/`coldLoadMaxWait` precedent, `benchmark_runner.go:245-253`).

### D4 — The result is REPORTED; the operator applies it. Nothing writes `vram_measured_mb`

This is the question the feature could most easily get wrong, so the answer is
stated as a rule:

> **The VRAM benchmark never writes `vram_measured_mb`, and never writes
> `vram_estimate_mb` either.** It reports a number and offers the operator a
> one-click way to put it into *their* field.

**Why not `vram_measured_mb`.** That field is agent-owned, and the ownership
split is "the load-bearing rule of the whole budget feature" (§5.1). Three
concrete consequences of crossing it, not one aesthetic one:

1. The value feeds admission arithmetic *as the spec's own declared demand*
   (`service_runtime.go:1441-1450`), and a breach of a budget by a **measured**
   value is **terminal** `not_permitted` (§5.2/§5.1). A gateway-computed delta
   that overshoots — a neighbour allocating inside the window — would refuse
   every future start of a model that had been working, with no operator action
   having occurred. That is §5.1's scenario verbatim, reached by a new route.
2. `vram_locked` exists precisely so the operator can opt out of being
   *governed* by the agent's measurement. A second, differently-sourced writer
   of the same field makes that lever mean something else, and §5.1 records
   that a *reset* button for this field was already considered and rejected.
3. `writeBackRuntimeVRAM` suppresses an unchanged value by comparing against
   what is stored (`agent_ingest.go:335-388`). A foreign writer of the same
   column makes that comparison lie, and the agent's next differing measurement
   would silently overwrite the benchmark's number anyway. **The benchmark
   cannot win that race, and should not try.**

**Why not `vram_estimate_mb` either.** It is operator-owned; `PutRuntimeSpec`
is its only writer, deliberately (`service_runtime.go:368-379`). A benchmark
that writes it takes a decision away from the operator and puts a machine
number in a field whose whole meaning is "what the operator declares".

**Why not hand the number to the agent to own.** No wire exists: gateway→agent
carries desired *state*, and the four frame types (§3.5) have no "here is your
measurement" shape. Inventing one means ADR-026's feature flag, hence the MINOR
bump this branch cannot take (D2). And it would be dishonest anyway: a
gateway-computed delta is *not* the agent's per-process measurement, and
laundering it through the agent would make it indistinguishable from one.

**So, exactly like the context probe** (`runContextProbe`, "does NOT persist …
the frontend fills the form field; the user saves manually"):

- The run status carries the result (`BenchmarkResult.VRAM`, below).
- The launch-spec form's per-GPU rows gain an **"apply"** affordance next to
  the existing editable estimate input (`RuntimeAdminSection.tsx:3421`) that
  fills the field; the operator saves. The measured line beside it
  (`:3431`) stays exactly as it is — three numbers with three distinct
  meanings, each with one owner, which is §11.4's rule rather than an exception
  to it.
- **The history row is the second half, and it is evidence rather than
  authority.** A `kind="vram"` row records what was measured, when, and under
  what isolation, so an operator can see that a spec was measured at 22 GB
  three times before they raised the estimate. Following `capacity_curve`'s
  precedent exactly, the per-GPU payload goes in **its own new column**
  (`vram_json`, migration 71), opaque to the store, decoded in the portal DTO
  only for `kind=="vram"`. Reusing `capacity_curve` for it would be a lie in a
  column name.

**Open question O4.** Whether the "apply" affordance should also be offered for
a *measured* value the agent already produced — i.e. "copy the measurement into
my estimate, then lock". That is arguably the missing lever §5.1's blockquote
circles around, and it is **out of scope here** because it changes the
`vram_locked` story rather than adding a benchmark. Name it; do not smuggle it
in.

### D5 — Wire and portal surface

```go
// BenchmarkResult (gateway/backend/internal/gateway/benchmark.go) gains:

// VRAM is the VRAM benchmark's result: nil = the run never reached the
// measurement phase (isolation refused, or a hard error — see Error).
// Non-nil with Inconclusive set = it ran and reached no number, and WHY
// is the operator's next action. The nested shape mirrors CapacityReport;
// what is deliberately NOT copied is VisionCapable's nil-means-both
// contract, because "no result" and "no result because the model was
// already being served by something we could not stop" send an operator
// to two different places.
VRAM *VRAMReport `json:"vram,omitempty"`

type VRAMReport struct {
    Isolated       bool          `json:"isolated"`                  // every spec confirmed stopped, target included
    DrainedSpecIDs []string      `json:"drained_spec_ids,omitempty"`// what the run force-stopped
    RestoreFailed  []string      `json:"restore_failed,omitempty"`  // specs left overridden, or taken over meanwhile (R1, W3)
    // Inconclusive is empty on a definitive result, else one of:
    // isolation_timeout | baseline_unstable | post_load_unstable |
    // already_resident (P3/step 4) | below_floor (step 7) | no_samples.
    Inconclusive   string        `json:"inconclusive,omitempty"`
    GPUs           []VRAMGPUItem `json:"gpus"`
}

type VRAMGPUItem struct {
    Index           int    `json:"index"`
    Fingerprint     string `json:"fingerprint,omitempty"`      // uuid, or name+total — see D3
    FingerprintKind string `json:"fingerprint_kind,omitempty"` // "uuid" | "name_total" | "" (none available)
    UnifiedMemory   bool   `json:"unified_memory,omitempty"`   // Apple: this is system memory, not VRAM
    BaselineUsedMB  int    `json:"baseline_used_mb"`
    DeltaMB         int    `json:"delta_mb,omitempty"`         // strategy (b); 0 = none
    MeasuredMB      int    `json:"measured_mb,omitempty"`      // strategy (a), post-load frame only; 0 = none/unknown
    Attributable    bool   `json:"attributable"`               // a spec GPU row exists for this index
}
```

`0` keeps its house meaning throughout: **unknown, never a real zero**
(`SpecGPU.VRAMMB`'s own doc, and `PolicySnapshot.Budgets`' list of every other
zero-value in the feature). i18n keys are added in German **and** English
together (AGENTS.md).

## 5. Non-goals

- No admission-logic change of any kind. This feature produces a number; rule 5
  on the branch beside it is what makes the number matter.
- No agent change: no new `agent.Features` entry, no new frame type, **no
  `Version` bump** — because the feature does not need one, *not* because a
  bump is unavailable (W2). If a future revision chooses the agent-side
  capability (D2's deferred alternative), that is its own branch, its own
  MINOR bump and its own feature flag, with this design as the fallback for
  agents that do not declare it.
- One gateway-side DTO addition is in scope and is not an agent change:
  `RuntimeStatusDTO` gaining a per-GPU measured-VRAM field plus frame
  freshness, which is what gives strategy (a) a watermark (D3, option 1). It
  is additive on the portal SSE, volatile, and needs no migration.
- No spillover / shared-memory accounting (D3).
- No scheduling: this run is manual, like `capacity` and `vision` before it
  (`benchmark_scheduler.go:90`).
- No new "reset the measurement" affordance (§5.1 rejected it; O4 names the
  adjacent question without answering it).

## 6. Test plan (TDD, red first)

1. `Admit`-free unit tests for the sampling core: stability gate accepts /
   rejects a synthetic sample window; delta arithmetic per index; `max(A,B)`.
2. Isolation sequence against a fake store + a driven `RuntimeStatus`: refuses
   on a pre-existing override (target's own included); refuses on a pinned
   **sibling** and proceeds on a pinned **target**; drains every **enabled**
   spec **including the target**; completes on a **transition** and not on a
   stale `stopped`; **completes without any transition for a sibling that was
   already in a no-process state** (P1 — the case the first revision would
   have timed out on, and the one to write first); reports an isolation
   timeout only for a spec still running at the bound.
3. Restore: runs in a `defer`; runs when the run context is already cancelled
   (asserted directly); **re-reads before writing, so a field an operator
   edited during the run survives** (W3); **skips the write and reports
   `restore_failed`** when the re-read shows the override is no longer the one
   this run wrote.
4. Result honesty: an already-resident target after a confirmed drain →
   `inconclusive: already_resident` and **no** `delta_mb` (P3); a headline
   delta below the floor → `inconclusive: below_floor`; an unstable window →
   `baseline_unstable` / `post_load_unstable`; every inconclusive result →
   **no** apply affordance. Definitive → per-GPU items, `attributable` false
   for an index with no spec row.
5. Strategy (a) freshness (W1): a stored `vram_measured_mb` written before the
   run is **never** reported as this run's `measured_mb`; a value carried by a
   post-load frame is; a value identical to the stored one is still accepted
   when it arrives on a post-load frame (the case a change-detection approach
   gets wrong).
6. Fingerprint (W4): NVIDIA samples yield `fingerprint_kind: "uuid"`; ROCm
   samples (no UUID) yield `"name_total"`; a sample with neither yields `""`
   and the portal renders no verification claim.
7. Ownership guard, the test that matters most: after a full VRAM run,
   `RuntimeSpecGPUs(specID)` shows `vram_measured_mb` **unchanged** and
   `vram_estimate_mb` **unchanged**. Name it after the rule it protects.
8. `putRequestFromDTO` field-completeness guard (D2/O2).
9. Frontend: the apply button fills the estimate input and does **not** submit;
   the trigger affordance states that the target itself will be stopped and
   restarted; i18n parity.

## 7. Open questions, collected

| # | Question | Where it bites |
|---|---|---|
| O1 | An application/server-scoped sweep? | D1 |
| O2 | Full-document `admin_state` replace + mapper guard, or a narrow one-column internal setter (an ADR-029 exception)? | D2 |
| O3 | `K`, `tol`, per-phase settle — validate on the 3-GPU Windows host before freezing | D3 |
| O4 | An "apply the agent's measurement to my estimate (and lock)" lever — adjacent, deliberately excluded | D4 |
| O5 | Does the run refuse outright on a server that also hosts **non-managed** active applications, or merely warn? Refusing is safer and may make the feature unusable on exactly the migration-path deployments §13 blesses | D3 |
| O6 | Bound for waiting on strategy (a): the health-pass dispatch makes the *measurement* prompt, but a POST-transport agent's telemetry is still the carrier, and the wait now ends on a **post-load frame** rather than on a store write. 30 s is still a guess | D3 |
| O7 | Where strategy (a)'s watermark comes from: a per-GPU field plus freshness on `RuntimeStatusDTO` (recommended — one stream already subscribed, and a live measured reading in the portal as a side benefit), or a separate volatile per-(spec, gpu) registry beside `RuntimeStatus`. Both are gateway-side and volatile; the choice is where the portal contract grows | D3/W1 |
| O8 | Whether the run should refuse when the **target** is `pinned`. D2 proceeds (the operator asked for this model), so a pinned model is stopped and restarted by an explicit benchmark. The alternative is refusing and making the operator unpin first, which is louder but costs the run | D2 |

## 8. Where the durable description goes, when it lands

`docs/architecture/cross-cutting/agent-runtime-manager.md` §5.3 (the paragraph
that currently ends "waiting for something that will not happen" gains its
answer) and a new subsection under §11 for the portal surface; the benchmark
plumbing itself is described in
`docs/architecture/cross-cutting/telemetry-usage-observability.md` and the
API surface in `docs/architecture/reference/api-surface.md`. R1 goes in
`docs/architecture/11-risks-and-technical-debt.md` §11.1. **None of that is
written yet, and none of it should be until the code exists.**

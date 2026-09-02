# VRAM benchmark: load one model alone on its server, then measure what it costs

## Why

A launch spec's `vram_estimate_mb` is operator-entered and defaults to `0`, which
the whole admission feature reads as **unknown demand**, not as "needs nothing".
Unknown demand is a real block, in both directions
(`docs/architecture/cross-cutting/agent-runtime-manager.md` §5.2/§5.3):
a spec of unknown demand may start only alone on its GPUs, and an occupant of
unknown demand blocks the cards it holds — evicted if idle, `Wait` if busy,
terminal `pending_vram_unknown` if pinned.

> **Version note — it matters for one sentence, and no argument here rests on
> it.** The *precedence* between the two directions — **known demand beats
> unknown demand**, so the unknown side blocks instead of evicting — is
> **ADR-032**, which lands with the `windows-vram-measurer` branch (together
> with ADR-031, the Windows PDH per-process measurer). Check
> `docs/architecture/09-architecture-decisions.md` before quoting it: if that
> file's last decision is **ADR-030**, the branch is not merged and ADR-032 is
> *proposed*, not settled. Everywhere below, ADR-032 is cited where it
> **sharpens** an argument and never where it carries one — D2's "Why not rely
> on the unknown-demand rules" holds either way, and the note there says which
> half survives without it.

That leaves the operator with an obvious question: **how do I resolve an unknown
demand on purpose?** Today there are two answers — guess an estimate, or wait
for the agent's per-process measurement — and the second one does not exist on
AMD or Apple-silicon hosts, where no measurer is installed at all (§5.3, §13).
This benchmark is the deliberate third answer.

## What was asked for

The operator's words:

> "wir sollten einen weiteren Benchmark einführen oder die Funktion in
> bestehende ergänzen. VRAM Benchmark. der dann auch wenn das Model mit anderen
> geladen werden dürfte, nur das Model auf dem Server lädt, dann die VRAM
> Benutzung misst. entweder direkt per Quelle. oder vorher alle verwendete GPUs
> VRAM merken und die Differenz zu wenn das Model geladen ist nehmen."

Three requirements, in the order they constrain the design:

1. **Isolation.** The run loads the target model **alone**, *even on a server
   where co-residency would permit company*. Isolation is the requirement, not a
   side effect of an idle host.
2. **Measurement, two strategies, either acceptable:** (a) *"direkt per
   Quelle"* — the per-process measurement the agent already takes; (b)
   *"Differenz"* — remember every used GPU's VRAM before the load and subtract
   once the model is resident.
3. It is a **benchmark**: operator-triggered, reported, in the same place the
   other benchmarks live.

Strategy (b) is the structurally valuable half: it is the only one that produces
a number where there is **no per-process measurer**. That measurer is
NVIDIA-only — `server-agent/internal/collector/measurer_other.go` selects the
`nvidia-smi --query-compute-apps` implementation and `measurer_windows.go` the
PDH one, and both return nothing on a host without NVIDIA. So:

| Host class | (a) per-process measurement | (b) a per-GPU delta |
|---|---|---|
| NVIDIA | yes | yes |
| AMD / ROCm | **no** — per-GPU totals, no per-PID split (`collector/amd.go`) | yes |
| Apple silicon | **no** — unified-memory totals (`collector/apple.go`) | yes, but the quantity is unified *system* memory, not dedicated VRAM, and must be labelled so |
| **No GPU at all** | no | **no** — see below |

**Correction to an earlier revision of this design: a GPU-less host cannot be
differenced either.** `collector.DetectGPUCollectors` returns only the
collectors whose `Available()` is true, so on a host with none it returns an
empty slice and the agent emits **no `GPUSample` at all**; correspondingly
`deriveRoutingSummary` (`gateway/backend/internal/gateway/agent_ingest.go`)
fills `VRAMUsedBytes` / `VRAMTotalBytes` **only inside `if len(req.GPUs) > 0`**.
There is nothing to subtract from nothing. "All three can still be differenced"
was false for the third class, and the design now refuses instead of pretending
(see D2's preconditions). A GPU-less host also has nothing this feature *could*
measure: a CPU-only model's cost is RAM, and this feature makes no claim about
RAM.

---

## What already exists (read the code, do not re-derive from this list)

### Run lifecycle — one run per server, reserved, cancellable

All in `gateway/backend/internal/gateway/`:

| Fact | Where |
|---|---|
| `BenchmarkRegistry` tracks **at most one in-flight run per server**, keyed by server id; volatile, nil-safe | `benchmark.go` |
| `TryStart(serverID, scope, mode, total, now, cancel)` is the reservation; `(nil, false)` when one is in flight → HTTP 409 `benchmark.already_running` | `benchmark.go`, `benchmark_endpoints.go` |
| A reserved server is **excluded from new routing** (`ServerBusy` feeds the resolver's `ServerBusyChecker`); a pre-run **idle gate** refuses with 409 `benchmark.server_in_use` while in-flight requests remain | `benchmark.go`, `benchmark_endpoints.go` |
| `Release` forgets a reservation whose idle gate failed; a run that *executed* keeps a terminal status so the poll can read the result | `benchmark.go`, `benchmark_runner.go` |
| The run executes on `context.Background()` with a `cancel` stored on the run, so it **outlives the trigger request** | `benchmark_endpoints.go` |
| Every streaming call is bounded by an **always-on idle watchdog** (2 min default), because a benchmark has no client to end it and a stall would reserve the server forever | `benchmark_runner.go` |

### Modes, and the two runs that are not modes

`parseBenchmarkMode` accepts exactly `"" → speed`, `speed`, `capacity`, `both`,
`vision`; anything else is 400 `benchmark.mode_invalid`. It gates the three
**fan-out** endpoints (mapping / application / server scope), each looping a
per-target `switch mode` over every authorized mapping.

Two runs deliberately sit *outside* that mode set, each with its own endpoint and
single-target runner — and they are the precedent this feature follows:

| Run | Endpoint | Runner | Persists? |
|---|---|---|---|
| Context probe | `POST /api/portal/mappings/{id}/probe-context` | `runContextProbe` (`benchmark_runner.go`) | **No** — it reports the size through the run status; the frontend fills the form field and the user saves |
| Load model | `POST /api/portal/mappings/{id}/load` | `runLoadModel` (`load_runner.go`) | No |

There is **no "load" mode**: the endpoint calls `TryStart(server.ID, "load",
"load", 1, …)` for the UI's benefit and invokes `runLoadModel` directly.
Scheduled runs are `"speed"` only (`benchmark_scheduler.go`), so a new run kind
is not swept into the scheduler by accident.

**The shared load core loads BY GENERATING.** `runLoadModel` takes
`benchmarkTargetReq`'s ordinary chat request (`benchmarkPrompts[0]`,
`Stream: true`), sets `req.MaxTokens = 1`, and calls `streamOnce` →
`provider.StreamingClient.CompleteStream`. There is no non-generating load path
anywhere in this code, and D3 depends on that fact.

### Progress, status, persistence

- **SSE**: `Subscribe` returns (snapshot, channel, unsubscribe) per server;
  `publish` fans a **full status** out non-blockingly, so a slow reader drops
  frames and recovers on its next snapshot. Wire: `GET
  /api/portal/servers/{id}/benchmark/events`, events `snapshot` + `progress`,
  both carrying the whole `BenchmarkStatus`. The **poll**
  (`…/benchmark/status`) remains the completion authority.
- **Persistence**: `model_mapping_benchmarks`, one row per mapping per run,
  written best-effort (`InsertBenchmarkRun`) — a history-write failure never
  fails the run. `kind` discriminates (`speed` | `capacity` | `vision`; empty =
  legacy speed) and **a kind-specific payload gets its own column**:
  `capacity_curve` holds opaque JSON the store never parses, decoded in the
  portal DTO only for `kind == "capacity"`. Latest migration is **70**, so a new
  column takes the next free version.
- **Nullability precedent**: `VisionCapable *bool` — nil = not run or
  inconclusive (nothing persisted), non-nil = a definitive result — and the
  runner writes nothing on nil while still recording a history row.
  `CapacityReport` is the precedent for the nested-JSON half. A VRAM result needs
  both shapes: a nullable pointer to a nested per-GPU struct.

### Where a per-GPU number can come from

| Source | Shape | Cadence | Where |
|---|---|---|---|
| `routing.ServerTelemetry` | `VRAMUsedBytes` / `VRAMTotalBytes` — **host aggregate, not per GPU**, and left at zero when the sample carries no GPUs | latest row only | `routing/store.go`, `agent_ingest.go`'s `deriveRoutingSummary`; used by the capacity ramp |
| `routing.TelemetrySample.GPUs[]` = `GPUSample{Index, Name, UUID, UtilPct, MemUsedBytes, MemTotalBytes, …}` | **per GPU, host index space**; **absent entirely on a host with no available GPU collector** | agent pushes every `OP_AGENT_INTERVAL`, default **1 s** (floor 250 ms) | `routing/store.go`, `collector.DetectGPUCollectors` |
| Live per-GPU samples, gateway side | `Server.ServerPerf` ring, cap 1200 samples, **volatile**, plus SSE fan-out | as above | `server_perf.go` |
| Persisted per-GPU history | `server_telemetry_samples.gpus_json` | same samples, retained | `store/migrate.go`, read via `TelemetrySamples(serverID, from, to, limit)` |
| Per-process measurement (strategy a) | `agent_runtime_sample.gpus[].vram_measured_mb` per `spec_id` | dispatched on a child's **first health pass** and on the agent owner's **15 s** housekeeping beat | agent `runtime/manager.go`; gateway `agent_ingest.go` |

Precision of the underlying reads: NVIDIA `memory.used` is MiB multiplied to
bytes, ROCm reports bytes, Apple reads ioreg in-use/alloc. **The noise floor of a
delta is other processes, not quantization.**

The ingest guard: `if g.VRAMMeasuredMB <= 0 { continue }` — a measured `0` means
*unknown* and is never persisted. `writeBackRuntimeVRAM` additionally suppresses
an **unchanged** value and writes only for a spec `resolveRuntimeSpecWritable`
confirms belongs to the reporting server and is not `vram_locked`.

### What the gateway can and cannot tell the agent

- Control is **desired state, not commands** (ADR-026). The agent's WebSocket
  reader understands exactly four frame types — `cert_update`, `ca_update`,
  `runtime_config`, `runtime_log_config` — and discards everything else for
  forward compatibility. ADR-026's closing sentence is the rule for anything
  imperative: *"A genuinely imperative action would need its own frame type
  behind its own feature flag."*
- **There is no acknowledgement that a pushed config was applied.** In gateway
  mode the agent sends no applied-ETag anywhere. The only observable evidence
  that a config change took effect is its **effect** on the status stream.
- Desired-state levers the gateway owns, and how each applies (§5.6):

  | Lever | Applied on push? |
  |---|---|
  | spec removed from the document | immediately — the spec drains |
  | `admin_state: force_stopped` | **immediately** — a running child drains |
  | lowered per-GPU budget | **not retroactive** — binds at next start |
  | removed co-residency pair | **not retroactive** |
  | lowered `runtime_max_processes` | **not retroactive** |

- Push immediacy depends on transport: a WS-connected agent gets it at once
  (gated on the `runtime_manager` feature); a POST-transport agent picks it up on
  its **60 s** runtime poll (`agent/agent.go`'s `runtimePollInterval`). The
  gateway can tell the two apart — `AgentStreamRegistry.hasConn(serverID)` — but
  it still gets **no ack either way**, so on the POST transport the only honest
  bound on "the override has landed" is that poll interval elapsing.
- **Two fail-closed gates decide whether a push happens at all**, and
  `Server.PushRuntimeConfig` (`agent_runtime.go`) checks both before it reads
  anything: the agent must have **declared `runtime_manager`**
  (`s.AgentFeatures.Has`) and the server must **not be in file mode**
  (`s.RuntimeStatus.IsFileMode`). Neither failing is an error there; it is
  "nothing to push". For this feature it is the difference between an isolation
  that happens and one that only appears to — see D2.
- **File mode makes every gateway-side runtime write inert** (§8.2). With
  `OP_AGENT_RUNTIME_CONFIG` set, the agent polls a local file by mtime, and
  `runtime.Driver.load` ignores a pushed payload for anything that is not a
  `*GatewaySource`: *"a pushed payload is simply not looked at; fall through to
  `Load` exactly as if pushed were nil."* The gateway also stops pushing once the
  upward report reveals the mode. §8.2 states the operator-visible consequence
  in the same terms this feature needs: start/stop buttons are **hidden**,
  because *"the admin override lives in the gateway document that file mode does
  not consume, and a dead button is worse than none."* Gateway-side specs that
  still exist are shown with an **"ineffective configuration" warning**. So in
  file mode a `PutRuntimeSpec` writing `admin_state: force_stopped` succeeds,
  returns 200, changes what the portal shows — and stops nothing.
- Enumeration: `RuntimeSpecsByApplication(appID)` lists a server's specs, and
  **at most one `server_agent` application exists per server** (portal
  enforcement plus a partial unique index), so that one call covers the whole
  server. It is a **`routing.Store` method, not a `portal.API` one** — the
  gateway package reaches the store only through `portal.API`, which is where
  D2's plumbing note starts. Live per-spec state is in `Server.RuntimeStatus`.
- **`AgentRuntimeConfig(ctx, serverID)` is the one principal-free runtime method
  already on `portal.API`.** It resolves the server's single `server_agent`
  application (returning an **empty document when there is none**), and returns
  one `AgentRuntimeSpecDTO` per **enabled** spec carrying `id`, `admin_state`,
  `pinned` and the per-GPU index rows. It is authorized by its caller, not by a
  portal principal — `PushRuntimeConfig` calls it with no token — and it is the
  precedent D2 leans on.

### The authorization shape of a benchmark run

- `AuthorizeBenchmarkScope(ctx, principal, scope, id)` is the whole gate. It runs
  **once, on the trigger request**, and every not-found collapses to a
  no-existence-leak sentinel.
- **The principal stops at the endpoint.** `startLoadModel` /
  `startContextProbe` use `token` for that one call and then build
  `benchmarkTarget{server, app, mapping}` — **no `auth.Token` field** — and hand
  it to a goroutine on `context.Background()`. `benchmarkTarget` and
  `benchmarkRun` (`benchmark.go`) carry no principal, and nothing in
  `benchmark_runner.go` has one to pass.
- `AuthorizeBenchmarkScope` has **no application-type gate**, unlike
  `PutRuntimeSpec` / `DeleteRuntimeSpec`, which reject a mapping whose owning
  application is not `routing.ProviderServerAgent` with
  `ErrRuntimeSpecNotServerAgent`. A `"mapping"`-scope benchmark authorizes any
  mapping on any application type, and benchmarks it "regardless of its status".

### The ownership boundary this feature must not break

- `vram_estimate_mb` is **operator-owned**; `vram_measured_mb` is
  **agent-owned**. `PutRuntimeSpec` ignores what the request sends for the
  measured value and copies the stored one forward
  (`portal/service_runtime.go`). `UpdateRuntimeSpecGPUMeasured` is the only
  writer, reached only from the telemetry write-back.
- §5.1 states why: a PUT handler that starts trusting `vram_measured_mb` from
  the request lets a UI round-trip erase real measurements, after which the
  agent's admission arithmetic uses estimates it has already disproved.
  `vram_locked` is the operator's **only** lever there — they choose whether to
  be governed by the measurement, never what it says — and a "reset the measured
  value to 0" button was explicitly **rejected**.
- The document the agent receives carries measured-over-estimate unless locked,
  so anything written into `vram_measured_mb` **feeds admission arithmetic
  directly**, and a measured breach of a budget is *terminal* `not_permitted`.
- The portal side of the same rule (§11.4): a screen shows both numbers and edits
  only its own, and **a form does not send a field it does not let you edit**.
  The per-GPU rows in the launch-spec form already look like this: an editable
  estimate input beside a read-only measured line.

### THE RULE that governs any new writer of the runtime document

`portal/service_runtime.go`'s `notifyRuntimeChanged` doc block states it once for
the whole service, and it is the single most important thing to read before
choosing how this feature writes `admin_state`:

> Any successful write that CAN change a server's runtime-config document
> notifies that server's agent — and what decides it is the write path's own
> SCOPE (which row it writes […]), never which field the request happened to
> change.

It then enumerates the **six kinds of row** the document derives from. Row 4 is
*"those mappings' RUNTIME SPECS and per-spec GPU rows → `specs[]`"* — and
`admin_state` is a field of `AgentRuntimeSpecDTO`, i.e. squarely inside the
document. The block closes with the asymmetry: over-notifying is *"cheap and
idempotent"*, and **"Under-notifying is the actual bug."**

Exactly **two** write paths are exempted, and both are exemptions of a whole
*path*, never of a field:

1. a writer whose signature confines it to columns **outside** the document
   (`persistApplicationSchemeSwitch`, `AgentProxyRoutes`' port assignment,
   `SetServerEnergyConfig`, the telemetry ingest's `LastSeenAt`/`UpdatedAt`);
2. **the agent's own write-back** — `writeBackRuntimeVRAM` →
   `UpdateRuntimeSpecGPUMeasured` — which does change the document but changes it
   *from* the agent, so a push would echo the agent its own measurement.

`notifyRuntimeChanged` is the **sole** trigger for
`gateway.Server.PushRuntimeConfig`. Nothing else pushes. Q2 below is decided by
this block, not by ADR-029 alone.

---

## Design

### D1 — A new run kind with its own endpoint, sharing the load core

`POST /api/portal/mappings/{id}/probe-vram`, reserving the server through
`TryStart(serverID, "vram-probe", "vram", 1, …)`, with a single-target runner
`runVRAMProbe`. **Not** a fifth `?mode=` value, and **not** a change to the
existing load run.

- **Why not a `mode`.** Every `mode` value is a **per-target** measurement inside
  a fan-out loop over an application's or a server's mappings. A VRAM run is not
  per-target: it drains the whole server *once*, then loads exactly *one* model.
  As a mode it would either silently measure only the first target, or
  drain-and-reload the server N times inside one reservation — and on the server
  scope, a run an operator reads as "measure my models" would stop every model on
  the box. The isolation is destructive enough that it must be asked for one
  model at a time.
- **Why not extend the load run.** `runLoadModel` is already most of the body
  (reserve → resident-probe → one 1-token stream → confirm), and the portal's
  "Load" button calls it. Adding isolation there would make that button stop
  every other model on the server, with no affordance warning about it. So: a
  **new runner that shares the load core**, extracted as a helper both call
  (`ensureResidentForRun(ctx, tgt) (loaded bool, err error)`), leaving
  `runLoadModel`'s behaviour byte-identical.

### D2 — Isolation is `admin_state: force_stopped` on **every** spec, target included

`force_stopped` is the only lever that **refuses a start**.

#### D2.0 — Preconditions: four refusals before anything is written

Each of these is a state in which the isolation the run promises **cannot be
achieved by any gateway-side write**, so the run refuses at the endpoint, with
HTTP 409 and a named code, before it touches a single spec. Refusing costs the
operator one error message; proceeding costs them every model on the server plus
a number that means nothing.

| # | Condition | Code | Why it is fatal, not degraded |
|---|---|---|---|
| P1 | The target mapping's owning application is not `routing.ProviderServerAgent` | `benchmark.vram_not_agent_managed` | `AuthorizeBenchmarkScope` has no type gate, so this reaches the runner. The target's process is then not agent-managed at all: it is not in the `server_agent` application's spec set, so D2.1's enumeration cannot include it, D2.3 cannot force-stop it, and `PutRuntimeSpec` would refuse the write with `ErrRuntimeSpecNotServerAgent` anyway — *after* the siblings were already drained. "The target among them" is simply false in that case. (This is a real configuration, not a test artefact: §13 blesses a non-managed llama-swap application coexisting on the same host, and its mappings are benchmarkable like any other.) |
| P2 | `s.RuntimeStatus.IsFileMode(serverID)` | `benchmark.vram_isolation_unavailable` | The agent re-reads its own local file and `Driver.load` never looks at a pushed document; `PushRuntimeConfig` does not even send one. Every write this run makes would return 200 and stop nothing, and the run would then report `Isolated: true` for a fleet it never touched — most starkly on a server whose gateway-side specs are all *already* in a no-process state, where D2.4 confirms every one of them without waiting for anything. **This is the defect this precondition exists for.** |
| P3 | `!s.AgentFeatures.Has(serverID, "runtime_manager")` | `benchmark.vram_isolation_unavailable` | The same gate `PushRuntimeConfig` fail-closes on. An agent that has not declared the feature has no runtime driver applying the document at all. |
| P4 | The server's latest telemetry carries **no GPU sample** | `benchmark.vram_no_gpu_samples` | Strategy (b) has no before-and-after to subtract (see *What was asked for*), and strategy (a) needs a measurer that does not exist without a GPU either. Nothing to measure, so nothing to drain for. |

Two honest notes on P2/P3, because they are discovered facts rather than
configured ones:

- `IsFileMode` is set from the agent's own upward report on ingest, so it is
  `false` for a file-mode server until the first report arrives after a gateway
  restart. The **durable** cross-check is the persisted report
  (`ServerRuntimeReportByServer` / `runtime_source`), and the run should consult
  it as well as the volatile flag. A stale `file` report on a server since
  switched back to `gateway` produces a false refusal — the safe direction, and
  the operator's fix is one report cycle away.
- Neither gate is a guarantee the document *arrived*: there is no ack (§3.5).
  They are guarantees the gateway **tried**. What turns "tried" into evidence is
  D2.4 plus the `Isolated` contract below.

#### D2.1–D2.6 — The sequence

1. **Enumerates** every **enabled** spec of the server's one `server_agent`
   application, **the target included**, via `AgentRuntimeConfig(ctx, serverID)`
   — already on `portal.API`, already principal-free, already filtered to
   enabled specs, and already carrying each spec's `id`, `admin_state`, `pinned`
   and GPU index rows. An **empty document means the server has no `server_agent`
   application**, which is P1 by another route and refuses the same way.
2. **Refuses to start** (409, `benchmark.vram_isolation_blocked`, naming the
   spec) if **any** of them — target included — already carries a non-empty
   `admin_state`, or if any spec **other than the target** is `pinned`.
3. Writes `admin_state: force_stopped` to every enumerated spec, **the target
   among them**. Writing it to the running ones only leaves a window in which a
   request through the agent's router starts one mid-measurement; writing it to
   the others only leaves the target itself resident, which destroys the
   measurement (see *Verified constraint 3*). **Every one of these writes must
   notify** — see D2's plumbing note and Q2.
4. **Partitions** what it wrote by the first status frame that resolves *after*
   the write, and waits only for what it can observe:
   - a spec with a live process (`running`, `starting`, `draining`) is waited
     for, bounded, until a `stopped` transition **later than the run's own
     write** — a `stopped` frame that predates the write proves nothing (§11.2);
     evidence recorded as `stopped_after_write`;
   - a spec in a no-process state (`stopped`, `pending_vram_unknown`,
     `not_permitted`, `crashed`, `start_failed`, `backoff`) is **already
     isolated** and is *confirmed*, never awaited (see *Verified constraint 1*);
     evidence recorded as `no_process_at_write`;
   - a spec still running at the bound is a genuine isolation timeout.

   **The bound is transport-derived, not guessed.** For a WS-connected agent
   (`AgentStreams.hasConn`) the push is immediate and the bound need only cover
   the drain. For a POST-transport agent the override binds up to
   `runtimePollInterval` (**60 s**) later, so the bound — and, for a
   `no_process_at_write` spec, the delay before the baseline may begin — must
   cover that interval. Otherwise `no_process_at_write` claims a refusal-to-start
   the agent has not yet been told about.
5. Baseline, then the measurement (D3) — which clears the **target's** override
   to start it, and nothing else's.
6. **Restores** every override it set back to `""`, in a `defer`, on a context
   that is *not* the run's own, **re-reading each spec immediately before writing
   it**.

#### The run carries no principal, and every writer this design names wants one

`PutRuntimeSpec(ctx, principal auth.Token, mappingID, req)` and
`GetRuntimeSpec(ctx, principal, mappingID)` both authorize through
`authorizeMapping`. A benchmark run has no principal to give them: the trigger's
`auth.Token` is consumed by `AuthorizeBenchmarkScope` and dropped, and
`benchmarkTarget` / `benchmarkRun` have no field for it. This has to be resolved
before D2 is implementable, and the three options are not equal:

- **Capture the trigger's token in the run.** `auth.Token` is a plain value
  struct, so this compiles, and it reuses a writer that already notifies and
  already gates the application type. **Rejected**, on the restore: every one of
  those calls re-derives authorization from *store rows*
  (`authorizeMapping → authorizeApplication → authorizeServer → ServerOwners`),
  so the deferred restore — minutes later, on a `WithoutCancel` context, when the
  fleet is force-stopped and the restore is the only thing standing between the
  operator and R1 — can be **refused** for reasons that have nothing to do with
  the run: the user removed from the server's owners, the mapping or application
  deleted mid-run (`ErrMappingNotFound`). A safety-critical restore must not have
  an authorization failure mode.
- **Synthesize a system principal** (`auth.Token{Scopes: ["system"]}`, which
  `isSystem` would wave through). **Rejected**: no production code in this tree
  fabricates a principal, and adding the first one for a benchmark creates a
  privilege surface far larger than the feature.
- **Recommended: principal-free `portal.API` methods, authorized once at the
  endpoint.** The trigger is already gated by `AuthorizeBenchmarkScope` plus
  D2.0; the run then uses methods shaped like `AgentRuntimeConfig(ctx, serverID)`
  — no principal, authorized by their caller, documented as such. Concretely:
  - reads: `AgentRuntimeConfig(ctx, serverID)` (exists) for enumeration, plus
    `GetRuntimeSpec`'s data by mapping id for the restore's re-read, in a
    principal-free form;
  - writes: one method that sets a spec's `admin_state` and **calls
    `notifyRuntimeChanged`** (Q2).

  Cost, stated rather than hidden: `portal.API` is a generated-mirror interface
  (`api_tracing_gen.go`), so each addition regenerates, and each method's doc
  block must say **who authorized it**, exactly as `AgentRuntimeConfig`'s does.
  A principal-free write path with no such sentence is how a future caller
  invents an unauthorized one.

#### Whichever writer is chosen, it must notify — this is not optional

`admin_state` is row 4 of the runtime document, and `notifyRuntimeChanged` is the
**only** trigger for `PushRuntimeConfig`. A write that does not notify reaches a
WS-connected agent no sooner than its 60 s poll, and D2.4's watermark wait then
times out on every running sibling — the drain silently degrades into an
isolation timeout, which is the *good* outcome; the bad one is a
`no_process_at_write` spec that starts mid-measurement because the refusal never
arrived.

The trap is that the obvious model for a "narrow one-column internal setter" is
`UpdateRuntimeSpecGPUMeasured`, which is precisely one of the block's **two
exempted paths** — exempted because it is *the agent's own write-back*. Copying
its shape for a gateway-originated `admin_state` write inherits the exemption
without the reason for it. `admin_state` fails the exemption test both ways: it
is not outside the document, and this writer is not the agent.

#### Why not `runtime_max_processes = 1`?

It looks cheaper — one write instead of N, letting the target's own admission
evict the others with the agent's own machinery — and it is *unsafe in the
opposite direction*:

- `force_stopped` **refuses a start**: `EnsureRunning` returns
  `ErrAdmissionBlocked` for a `force_stopped` spec before any admission
  arithmetic runs, and the reconcile pass drains one that is up — *ahead* of the
  `pinned`/`force_running` start branch, so it outranks `pinned` too
  (`server-agent/internal/runtime/manager.go`).
- `runtime_max_processes = 1` **licenses a displacement in the wrong
  direction**: a router request for a sibling asks the agent to admit *that
  sibling*, and the process-count rule then looks for as many victims as the
  limit demands — with the target the only running process and idle between the
  run's own probe requests, **the target is the victim**. The cheap lever
  hands any inbound request permission to kill the thing being measured. It is
  also not retroactive, so it does nothing about what is already up.

Note for anyone re-opening this: the *observability* argument sometimes offered
here is not the reason. For a spec that is not running, neither lever produces a
frame — `RuntimeStatusDTO` has no `admin_state` field, and no applied-config
version rides telemetry. The reason is effect.

#### Why not rely on the unknown-demand rules?

They apply only to a spec whose demand is *already* unknown — "verify a
suspicious estimate" is the other half of what was asked — and they force
solitude only on **that spec's own GPUs**, while a delta wants a quiet baseline
on every GPU it reads. **Both halves hold regardless of ADR-032.** What ADR-032
adds, once merged, is that the rule an operator would most expect to produce
isolation produces a `Wait` instead: under *known demand beats unknown*, an
unknown-demand candidate facing an occupant of *known* demand blocks rather than
evicting it. Without ADR-032 that candidate may evict — but only on its own
cards, which is still not the baseline a delta needs. The conclusion does not
move; only its sharpness does.

#### Why not a new imperative "measure now, isolated" agent frame?

It is the semantically best answer and should be treated as the end state: the
agent already owns the measurement, the drain, the eviction and the health probe,
so it could isolate, measure and report a number it legitimately owns — no
ownership crossing, and **no crash window** (a gateway that dies between drain
and restore leaves the fleet force-stopped; an agent that dies takes its own
transient overrides with it). It is also **the only design that works in file
mode at all**, since it needs nothing from the gateway document.

What argues for building the gateway-side drain **first** is fleet
compatibility, not versioning: ADR-026 requires the frame behind its own feature
flag and ADR-025 forbids gating on a version number, so the run must cope with an
agent that does not declare the flag — which means the gateway-side path has to
exist anyway as the fallback for every agent already in the field. **The
consequence is a real cost and is not being papered over**: choosing the frame
means an agent-side capability flag, which means a **MINOR `Version` bump** on
whatever branch builds it, its own `agent.Features` entry, and a fleet in which
both paths must coexist and be tested. That is a bigger commitment than this
issue, which is why it is deferred — not because it is worse.

(When the frame is written: the agent's number would be its **per-process**
measurement, so a host with no measurer still needs the gateway-side delta, and
the frame must carry its own bounded, self-restoring isolation or it reintroduces
the same crash window inside the agent.)

#### Why the refusals in D2.2 are not timidity

The `admin_state` refusal is what makes the restore unambiguous: the run only
ever restores to `""`, so it never has to reconstruct what an operator's override
was — which, after a gateway restart, it could not know. The **pinned** refusal
is *not* because a pinned spec cannot be drained (it can — `force_stopped` is
tested before the pinned start branch and the drain path has no pinned guard); it
is because `pinned` is an operator's standing instruction that this model stays
up, and silently breaking it for a benchmark is a worse surprise than refusing
and naming it. The **target** may be pinned — stopping the target is the point of
the run — so the affordance must say plainly that the target will be stopped and
restarted.

#### Traps this sequence must not fall into

Each is already documented elsewhere in the architecture docs, for another
feature, in this exact shape:

- **Full-document replace.** `PutRuntimeSpec` is a full replacement (ADR-029), so
  the `admin_state` write must be built by **spreading the loaded spec** and
  replacing one field — never by assembling a field list. §11.1 records what the
  assembled-body version cost: a body that had quietly reset the operator's
  binary path, args, timeouts and GPU rows, with a test asserting only that
  `admin_state` came out right passing anyway. A Go caller has no `...rest`
  spread, so this needs one named
  `putRequestFromDTO(RuntimeSpecDTO) PutRuntimeSpecRequest` helper with a test
  that fails when a field is added to the spec and not to the mapper.
- **The write must notify.** See above. Whichever writer Q2 picks.
- **The restore must not run on the run's context.** The run body's context is
  cancelled when the run finishes or is cancelled, which is precisely when the
  restore matters most. Use `context.WithoutCancel` (or a fresh background
  context) with its own timeout.
- **The restore must re-read, never replay.** A full-document replace of a spec
  captured *before* the run reverts every field an operator edited *during* it —
  a launch spec is exactly what an operator opens while a model is stopped — and
  this endpoint is a read-modify-write with no compare-and-set, the same
  lost-update class §11.1 records for `UpdateMapping`. So: re-read, spread *that*
  document, set `admin_state: ""`, PUT. If the freshly-read `admin_state` is no
  longer the `force_stopped` this run wrote, **do not write at all** — someone
  else owns the field now; record the spec in `restore_failed`.
- **Completion on a transition, not a state** (§11.2), *but only for the specs
  that have a process to stop.* Both facts are needed, for disjoint halves of the
  set.
- **A bounded wait, and no silent clearing on timeout.** §11.2 chose not to clear
  an override on timeout, because the portal cannot tell a wedged child from a
  slow one. A benchmark's calculus differs: it *created* the overrides, so
  leaving them is strictly worse than clearing them. **On timeout: abort the
  measurement, still attempt the restore, and report both facts** (`error =
  "isolation timed out"`, plus the specs whose restore failed). This is a
  deliberate divergence from §11.2 and must be written down as such wherever it
  lands.

#### `Isolated` is evidence, never an assumption

The single rule the file-mode defect produced, and the one to write a test for
first:

> **`Isolated` is true only when, for every enumerated spec, this run holds
> per-spec evidence it produced itself** — `stopped_after_write` or
> `no_process_at_write` (D2.4), the latter only after the transport's own binding
> delay has elapsed. A 200 from a write is **not** evidence: in file mode every
> write returns 200 and changes nothing. The report carries the per-spec evidence
> alongside the boolean (D5), so `Isolated` can be audited rather than believed.

### D3 — Both measurement strategies, reported side by side, with honesty gates

One run produces, per GPU, up to two independent numbers, and says which it got:

- **`measured_mb` (strategy a, "direkt per Quelle").** After the load the agent
  measures its own child on the first health pass and every 15 s beat. Reading it
  **crosses no ownership boundary** — it is a number the agent produced. On a
  host with no measurer it never appears, which is the case strategy (b) exists
  for. **It must be read with a watermark, and the stored row cannot supply one:**
  `RuntimeSpecGPU` is `{SpecID, GPUIndex, VRAMEstimateMB, VRAMMeasuredMB}` with
  **no timestamp**, and `writeBackRuntimeVRAM` deliberately does not rewrite an
  unchanged value — so polling for "a positive value appears" reads an
  arbitrarily old number as this run's result, and requiring the value to
  *change* fails in the normal case where this run measures what the last one
  did. Two honest ways out, in preference order:
  1. **Carry the measurement on the volatile status stream and time-stamp it
     there.** The per-spec measurement already reaches the gateway once per
     second inside `agent_runtime_sample.gpus[].vram_measured_mb`, and
     `runtimeStatusDTOsFromSamples` throws it away (`RuntimeStatusDTO` has no GPU
     field). Add one, plus the sample's arrival time or a monotonic frame
     counter, and strategy (a) gets the same watermark discipline the `stopped`
     frame has: accept only a value carried by a frame that arrived **after the
     load completed**. Volatile RAM, no migration, no agent change — and the run
     is already subscribed to that stream for D2's isolation wait. Side benefit:
     a live measured-VRAM reading in the portal, which today exists only as a
     stored number of unknown age.
  2. **Drop strategy (a) from the run** and report the delta alone. Cheaper, and
     it loses the cross-check that is half the value of running both.

  Either way: **strategy (a) reports nothing rather than something stale.**
- **`delta_mb` (strategy b, "die Differenz").** From the gateway's per-GPU
  sample ring: `used_after − used_before` per GPU index, in MB.

The sequence, and every step exists for a reason:

1. Drain (D2) and confirm — **the target included**. A baseline taken while the
   target is resident measures nothing at all.
2. **Baseline**: require `K` consecutive samples (proposal: K=3, ~3 s at the 1 s
   cadence) in which each watched GPU's `mem_used_bytes` varies by no more than
   `tol` (proposal: `max(64 MiB, 1 %)`). Not stable → **inconclusive**, report
   nothing.
3. **Clear the target's override only**, then load it through the shared load
   core. Its siblings stay `force_stopped`, so the target starts alone without
   any admission arithmetic having to be trusted. **This step generates.** The
   load core's only load mechanism is `streamOnce` with `MaxTokens = 1` over
   `benchmarkPrompts[0]`, so by the time this step returns, the model has both
   loaded *and* served a complete one-token generation.
4. **The resident short-circuit is a contamination signal, not a shortcut.**
   `ensureResidentForRun` must report whether it actually loaded anything — the
   existing load core returns "loaded" without loading when the model is already
   resident. After step 1 confirmed the target stopped, a model that still
   reports resident is being served by something the gateway did not stop (a
   non-managed application on the same host, most likely). **Report inconclusive
   and say so**; never a delta.
5. Settle, then the **same stability gate** on the post-load samples → the
   headline `delta_mb`.
6. **Floor gate.** A confirmed-resident model whose headline delta is below a
   floor (proposal: `tol`) is **inconclusive**, not a measurement: no model costs
   ~0 MB, so such a number can only mean the window missed the allocation or
   something else absorbed it. `0` means *unknown* everywhere else in this
   feature and must mean it here too.
7. Restore (D2) — every override, target included.

**Correction: there is no second "generation" step, because step 3 is already
one.** An earlier revision had a step 6 that sent "one tiny generation request"
after the load and reported `max(delta_A, delta_B)`, justified by *"llama.cpp
preallocates its KV cache at load, so `delta_A` is already the steady state
there, but a backend that allocates on first use does not."* That rationale is
**already satisfied by step 3**: the shared load core has no non-generating path,
so a first-use allocation has necessarily already happened before the post-load
window opens, and the second request — same short prompt, same `MaxTokens = 1` —
allocates nothing new. Two windows for one observation is measurement theatre:
it doubles the exposure to a drifting neighbour and to the reservation being held
open, in exchange for a number that cannot differ for the stated reason. The
genuine remaining question is different, and is recorded as Q9 rather than
smuggled back in as a step: whether a *larger* request (long prompt, many output
tokens) grows the footprint beyond what a 1-token generation reveals. If it does,
the answer is a *bigger* probe, not a repeated small one.

**Which GPUs are watched.** The spec's declared GPU rows (`RuntimeSpecGPUs`)
when it has any — that is the index set admission actually uses. When it has
none, watch every GPU and report each index whose delta exceeds the floor, marked
**unattributable** (there is no row to apply it to).

**Record a card fingerprint next to the index, and make it degrade.** A stored
VRAM number attributed to index 1 after the cards were renumbered is worse than
no number, so the result carries what identified the card and the portal warns on
a mismatch — the same mechanism the per-GPU budget rows already use, where
`ExpectedUUID`/`ExpectedName` are a purely descriptive drift detector compared
against live telemetry. **But `GPUSample.UUID` is NVIDIA-only**: the ROCm and
ioreg parses never populate it, so a UUID-only detector is empty on exactly the
two host classes the delta strategy exists to serve. So the fingerprint is
`{uuid, name, mem_total_bytes}`, with the strongest available field recorded
*and named*:

| Host | Fingerprint | What drift it can catch |
|---|---|---|
| NVIDIA | `uuid` | any renumbering |
| AMD (ROCm) | `name` (`Card series`/`Card model`) + `mem_total_bytes` | a swap between *unlike* cards; two identical cards trading indices are indistinguishable, and the result must say so rather than imply a check it did not make |
| Apple | index 0 only, `name` | nothing to renumber — one integrated GPU. What matters instead is that `mem_used`/`mem_total` are **unified system memory** read from ioreg, not dedicated VRAM, so the number must be labelled as such wherever it is shown |

The portal shows "verified by UUID" or "verified by name and total size only" —
never a bare "verified".

**What contaminates a delta, and what the design does about each:**

| Contaminant | Handling |
|---|---|
| Another **managed** spec starting or allocating mid-window | Closed by D2: `force_stopped` prevents a start, not merely an eviction — but only where D2.0's preconditions hold, which is why they are refusals rather than warnings |
| A **non-managed** application on the same server (llama-swap coexisting with the managed runtime is an explicit migration path, §13) | Not drained — out of the gateway's reach. A *static* neighbour cancels out of the delta; a *moving* one trips the stability gate and the run reports inconclusive; one serving the target model itself is caught by step 4 |
| A client hitting the **agent's router port directly** (it authenticates nothing and its shipped default binds all interfaces, §4.6) | The benchmark reservation excludes only *gateway* routing. Trips the stability gate at best; must be named as a limitation |
| The **display/compositor** on a workstation GPU | A constant is absorbed by the delta; window activity during the window is drift, caught by the stability gate |
| **Driver reserve**, ECC overhead | Constant, absorbed by the delta. Note the consequence: a delta is *the model's marginal cost*, while the agent's per-process measurement is *that process's attributed usage* — they are not the same quantity and may legitimately differ. Report both; never average them |
| Sampling **quantization** (1 MiB on NVIDIA) | Below `tol`; irrelevant next to the above |
| **Shared / host-spillover** memory | Explicitly out of scope. On Windows the `Shared Usage` and `Non Local Usage` performance counters were measured reading *identically* on all three GPUs of a 3-GPU host, so they are not per-adapter figures. Claim nothing about spillover |

`K`, `tol` and the per-phase settle are reasoned, not measured. Validate them on a
real multi-GPU host before freezing, and make them `var`s so tests can shorten
them (the `coldLoadPollGap`/`coldLoadMaxWait` precedent).

### D4 — The result is REPORTED; the operator applies it

> **The VRAM benchmark never writes `vram_measured_mb`, and never writes
> `vram_estimate_mb` either.** It reports a number and offers the operator a
> one-click way to put it into *their* field.

**Why not `vram_measured_mb`.** That field is agent-owned, and the ownership
split is the load-bearing rule of the whole budget feature (§5.1). This is the
one place where a gateway-orchestrated design brushes against a documented
ownership rule, and the answer is to stop at the boundary rather than to
negotiate it. Three concrete consequences of crossing it:

1. The value feeds admission arithmetic *as the spec's own declared demand*, and
   a breach of a budget by a **measured** value is **terminal** `not_permitted`.
   A gateway-computed delta that overshoots — a neighbour allocating inside the
   window — would refuse every future start of a model that had been working,
   with no operator action having occurred.
2. `vram_locked` exists precisely so the operator can opt out of being *governed*
   by the agent's measurement. A second, differently-sourced writer of the same
   field makes that lever mean something else, and a *reset* button for this field
   was already considered and rejected.
3. `writeBackRuntimeVRAM` suppresses an unchanged value by comparing against what
   is stored. A foreign writer of the same column makes that comparison lie, and
   the agent's next differing measurement would overwrite the benchmark's number
   anyway. **The benchmark cannot win that race, and should not try.**

**Why not `vram_estimate_mb` either.** It is operator-owned and `PutRuntimeSpec`
is deliberately its only writer. A benchmark that writes it takes a decision away
from the operator and puts a machine number in a field whose whole meaning is
"what the operator declares".

**Why not hand the number to the agent to own.** No wire exists: gateway→agent
carries desired *state*, and the four frame types have no "here is your
measurement" shape. Inventing one means ADR-026's feature flag and the MINOR
`Version` bump that comes with it — and it would be dishonest anyway, since a
gateway-computed delta is *not* the agent's per-process measurement and
laundering it through the agent would make it indistinguishable from one.

So, exactly like the context probe:

- The run status carries the result (`BenchmarkResult.VRAM`, below).
- The launch-spec form's per-GPU rows gain an **"apply"** affordance next to the
  existing editable estimate input, which fills the field; the operator saves.
  The read-only measured line beside it stays exactly as it is — three numbers
  with three distinct meanings, each with one owner.
- **The history row is evidence, not authority.** A `kind = "vram"` row records
  what was measured, when, and under what isolation, so an operator can see a
  spec measured at 22 GB three times before they raise the estimate. Following
  `capacity_curve`'s precedent exactly, the per-GPU payload goes in **its own new
  column** (`vram_json`, next free migration), opaque to the store, decoded in
  the portal DTO only for `kind == "vram"`. Reusing `capacity_curve` for it would
  be a lie in a column name.

### D5 — Wire and portal surface

```go
// BenchmarkResult (gateway/backend/internal/gateway/benchmark.go) gains:

// VRAM is the VRAM benchmark's result: nil = the run never reached the
// measurement phase (refused at D2.0, isolation refused, or a hard error --
// see Error). Non-nil with Inconclusive set = it ran and reached no number,
// and WHY is the operator's next action. The nested shape mirrors
// CapacityReport; what is deliberately NOT copied is VisionCapable's
// nil-means-both contract, because "no result" and "no result because the
// model was already being served by something we could not stop" send an
// operator to two different places.
VRAM *VRAMReport `json:"vram,omitempty"`

type VRAMReport struct {
    // Isolated is true ONLY when every enumerated spec, target included,
    // carries this run's OWN evidence in IsolationEvidence. A 200 from an
    // admin_state write is not evidence: in file mode every such write
    // returns 200 and stops nothing, which is why a file-mode server is
    // refused at D2.0 rather than reported as isolated.
    Isolated bool `json:"isolated"`
    // IsolationEvidence is spec id -> why this run believes that spec is not
    // running: "stopped_after_write" (a stopped transition on a frame later
    // than this run's write) or "no_process_at_write" (no live process when
    // the write landed, and force_stopped refuses its restart -- recorded
    // only after the transport's binding delay, D2.4). A missing entry, or
    // any other value, means NOT isolated.
    IsolationEvidence map[string]string `json:"isolation_evidence,omitempty"`
    DrainedSpecIDs    []string          `json:"drained_spec_ids,omitempty"` // what the run force-stopped
    RestoreFailed     []string          `json:"restore_failed,omitempty"`   // specs left overridden, or taken over meanwhile
    // Inconclusive is empty on a definitive result, else one of:
    // isolation_timeout | baseline_unstable | post_load_unstable |
    // already_resident | below_floor | no_samples.
    // no_samples is for samples that STOPPED arriving mid-run; a server with
    // no GPU samples at all is refused before the run starts (D2.0 P4).
    Inconclusive string        `json:"inconclusive,omitempty"`
    GPUs         []VRAMGPUItem `json:"gpus"`
}

type VRAMGPUItem struct {
    Index           int    `json:"index"`
    Fingerprint     string `json:"fingerprint,omitempty"`      // uuid, or name+total
    FingerprintKind string `json:"fingerprint_kind,omitempty"` // "uuid" | "name_total" | "" (none available)
    UnifiedMemory   bool   `json:"unified_memory,omitempty"`   // Apple: system memory, not VRAM
    BaselineUsedMB  int    `json:"baseline_used_mb"`
    DeltaMB         int    `json:"delta_mb,omitempty"`         // strategy (b); 0 = none
    MeasuredMB      int    `json:"measured_mb,omitempty"`      // strategy (a), post-load frame only; 0 = none/unknown
    Attributable    bool   `json:"attributable"`               // a spec GPU row exists for this index
}
```

New error codes, all 409 at the trigger:
`benchmark.vram_not_agent_managed`, `benchmark.vram_isolation_unavailable`,
`benchmark.vram_no_gpu_samples`, `benchmark.vram_isolation_blocked`. Each names
the blocking spec or condition; each gets a German **and** English i18n key
together.

`0` keeps its house meaning throughout: **unknown, never a real zero**.

---

## Verified constraints — things that look true and are not

Each was checked in the tree, and each one silently destroys the feature if the
implementation assumes otherwise.

1. **A `force_stopped` write against a spec with no live process does nothing at
   all — no state change, no frame.** §11.2 states it, and the agent's reconcile
   pass shows it: the `force_stopped` branch drains only `if st.proc != nil`. So
   a bounded wait for a `stopped` **transition** exhausts for every *idle*
   sibling, turning an already-isolated server into an isolation timeout. Every
   spec *is* present in every status frame (the agent's status snapshot walks all
   of them), so its **state** is readable — there is simply no *edge* to wait
   for. Hence D2.4's partition.
2. **`RuntimeStatusDTO` carries no `admin_state`**, and the runtime sample's
   `gpus` are dropped when the status DTOs are built. There is no
   applied-config acknowledgement anywhere either. So "did the agent adopt what I
   wrote?" is answerable only through the *effect* on the stream, and only for a
   spec that had a process to stop. Do not build a wait on any other signal.
3. **The load core short-circuits on an already-resident model** (`modelResident`
   → "loaded", without loading), and nothing else drains the *target*. Together
   those two make the commonest trigger — an operator probing a model that is
   currently loaded — produce a baseline that **already contains the model**, a
   stable post-load window, and a *definitive* `delta_mb ≈ 0`. Hence: drain the
   target too (D2.3), treat a still-resident model as contamination (D3 step 4),
   and gate a sub-floor delta as inconclusive (D3 step 6).
4. **In file mode, nothing the gateway writes reaches the agent.** The agent
   re-reads its local file; `Driver.load` looks at a pushed payload only for a
   `*GatewaySource`; and `PushRuntimeConfig` fail-closes on `IsFileMode` before
   it even assembles a document. A gateway-side isolation is therefore not
   "degraded" in file mode, it is **absent** — while every write still returns
   200. Hence D2.0 P2, and hence `Isolated` being evidence-backed rather than
   write-backed.
5. **`AuthorizeBenchmarkScope` gates ownership, not application type.** It has no
   equivalent of `PutRuntimeSpec`'s `ErrRuntimeSpecNotServerAgent`, so a
   `"mapping"`-scope run can target a model on a non-`server_agent` application,
   whose spec set is not the one D2 enumerates. Hence D2.0 P1.
6. **The run holds no principal.** The trigger's `auth.Token` does not survive
   into `benchmarkTarget`/`benchmarkRun`, and every `admin_state` writer this
   design would otherwise use takes one. Hence D2's plumbing note.
7. **`admin_state` is inside the runtime-config document**, so any writer of it
   owes a `notifyRuntimeChanged` — the sole trigger for `PushRuntimeConfig`.
   Hence Q2's real content.

---

## Test plan (TDD, red first)

1. Unit tests for the sampling core, with no `Admit` involvement: the stability
   gate accepts/rejects a synthetic sample window; delta arithmetic per index.
2. **Preconditions, one test each, all red first**: a file-mode server is
   refused and **no spec is written at all** (assert the write count, not just
   the status — this is the test that would have caught the original defect); an
   agent that has not declared `runtime_manager` is refused; a target on a
   non-`server_agent` application is refused *before* any sibling is drained; a
   server whose latest telemetry carries no GPU sample is refused.
3. **`Isolated` is never true without this run's own evidence.** Drive a
   status stream that reports every spec `stopped` from before the run began:
   the run must not report `Isolated: true` on that alone, and must record
   `no_process_at_write` only after the transport's binding delay.
4. Isolation sequence against a fake store plus a driven runtime status: refuses
   on a pre-existing override (the target's own included); refuses on a pinned
   **sibling** and proceeds on a pinned **target**; drains every **enabled** spec
   **including the target**; completes on a **transition** and not on a stale
   `stopped`; **completes without any transition for a sibling that was already
   in a no-process state** (write this one first — it is the case the naive
   sequence times out on); reports an isolation timeout only for a spec still
   running at the bound.
5. **Every `admin_state` write notifies.** Assert the runtime-config-changed
   hook fires once per spec written, on the drain *and* on the restore. Without
   this, Q2's cheaper option regresses the drain into a 60 s wait silently.
6. Restore: runs in a `defer`; runs when the run context is already cancelled
   (assert directly); **re-reads before writing, so a field an operator edited
   during the run survives**; **skips the write and reports `restore_failed`**
   when the re-read shows the override is no longer the one this run wrote; and
   **succeeds without any principal** — no authorization path can refuse it.
7. Result honesty: an already-resident target after a confirmed drain →
   `inconclusive: already_resident` and **no** `delta_mb`; a headline delta below
   the floor → `below_floor`; an unstable window → `baseline_unstable` /
   `post_load_unstable`; every inconclusive result → **no** apply affordance.
   Definitive → per-GPU items, `attributable` false for an index with no spec row.
8. **One load, one generation.** Assert the run issues exactly **one** streaming
   request to the upstream, and that the post-load window opens after it — the
   regression guard for the removed second-generation step.
9. Strategy (a) freshness: a stored `vram_measured_mb` written before the run is
   **never** reported as this run's `measured_mb`; a value carried by a post-load
   frame is; a value *identical* to the stored one is still accepted when it
   arrives on a post-load frame (the case a change-detection approach gets wrong).
10. Fingerprint: NVIDIA samples yield `fingerprint_kind: "uuid"`; ROCm samples
    yield `"name_total"`; a sample with neither yields `""` and the portal renders
    no verification claim.
11. Ownership guard, the test that matters most: after a full VRAM run,
    `RuntimeSpecGPUs(specID)` shows `vram_measured_mb` **unchanged** and
    `vram_estimate_mb` **unchanged**. Name the test after the rule it protects.
12. `putRequestFromDTO` field-completeness guard.
13. Frontend: the apply button fills the estimate input and does **not** submit;
    the trigger affordance states that the target itself will be stopped and
    restarted; the four refusal codes each render an actionable message; i18n
    parity (German and English together).

## Non-goals

- No admission-logic change of any kind. This feature produces a number.
- No agent change: no new `agent.Features` entry, no new frame type, no `Version`
  bump — because *this* design does not need one. If a future revision chooses
  the agent-side capability (the "measure now, isolated" frame, and the only path
  that works in file mode), that is its own branch, its own **MINOR** bump, and
  its own feature flag, with this design as the mandatory fallback for agents
  that do not declare it.
- One gateway-side DTO addition **is** in scope and is not an agent change:
  `RuntimeStatusDTO` gaining a per-GPU measured-VRAM field plus frame freshness
  (D3, option 1). Additive on the portal SSE, volatile, no migration.
- Additions to `portal.API` are in scope (D2's plumbing note) and regenerate its
  tracing mirror.
- **File mode is out of scope, by refusal rather than by omission** (D2.0 P2). It
  is not "unsupported until someone gets to it"; it is unreachable from the
  gateway side by design, and the agent-side frame is the only thing that would
  serve it.
- No spillover / shared-memory accounting.
- No scheduling: this run is manual, like `capacity` and `vision` before it.
- No new "reset the measurement" affordance.

## Open questions

| # | Question |
|---|---|
| Q1 | An application- or server-scoped **sweep** (drain once, then measure each mapping in turn while the rest stay stopped)? Strictly more useful and strictly more destructive; defer until the single-model run is proven in the field. |
| Q2 | Which writer sets `admin_state`: the full-document `PutRuntimeSpec` plus a `putRequestFromDTO` guard test, or a narrow one-column internal setter? **Not a free choice, and the constraint is not only ADR-029.** `admin_state` is row 4 of the runtime document and `notifyRuntimeChanged` is the sole trigger for `PushRuntimeConfig`, so *either* writer must notify; the narrow setter's obvious model, `UpdateRuntimeSpecGPUMeasured`, is one of the block's two exempted paths and would silently inherit a "do not notify" that belongs to the agent's own write-back. Recommendation: the full-document mapper plus its guard test — it already notifies, already gates the application type, and ADR-029's rule is about *the runtime domain*, not about one endpoint. A narrow setter is acceptable only with an explicit `notifyRuntimeChanged` call and a test asserting it. |
| Q3 | `K`, `tol`, per-phase settle — validate on a real multi-GPU host before freezing. |
| Q4 | An "apply the agent's measurement to my estimate (and lock)" lever — adjacent, deliberately excluded here because it changes the `vram_locked` story rather than adding a benchmark. |
| Q5 | Does the run refuse outright on a server that also hosts **non-managed** active applications, or merely warn? Refusing is safer and may make the feature unusable on exactly the migration-path deployments §13 blesses. |
| Q6 | Bound for waiting on strategy (a): the health-pass dispatch makes the *measurement* prompt, but a POST-transport agent's telemetry is still the carrier, and the wait ends on a post-load frame rather than on a store write. 30 s is a guess. |
| Q7 | Where strategy (a)'s watermark comes from: a per-GPU field plus freshness on `RuntimeStatusDTO` (recommended — one stream already subscribed, and a live measured reading in the portal as a side benefit), or a separate volatile per-(spec, gpu) registry. Both are gateway-side and volatile; the choice is where the portal contract grows. |
| Q8 | Whether the run should refuse when the **target** is `pinned`. This design proceeds (the operator asked for this model); the alternative is refusing and making them unpin first, which is louder but costs the run. |
| Q9 | Does a **larger** request (long prompt, many output tokens) grow the footprint beyond what step 3's one-token generation reveals? If yes, the answer is one bigger probe with a documented shape, not a second small one (see D3's correction). Needs measurement on a backend that allocates KV lazily before any step is added. |
| Q10 | Should the run refuse, or merely warn and extend its bound, on a **POST-transport** agent (no open WS)? Every override binds up to 60 s later there, which triples the reservation window for the same result. |

## Named risk

**If the gateway process dies between the drain and the restore, every model on
that server stays `force_stopped` until an operator clears it by hand.**
Mitigations in scope: the run status carries the drained set so the portal can say
so, and D2.2's refusal means the state to restore is always exactly `""`. A
**persisted isolation lease**, reconciled on gateway start, is the only real fix
and is out of scope — it is also the strongest argument for the agent-side
capability, which has no such window because the agent both owns and restores its
own state. Record this in the operational-risk table when the feature lands.

## Documentation to update when it lands

- `docs/architecture/cross-cutting/agent-runtime-manager.md` §5.3 — the paragraph
  that currently ends "waiting for something that will not happen" gains its
  answer — plus a subsection under §11 for the portal surface, and one sentence
  in **§8.2** recording that this run refuses in file mode and why.
- `docs/architecture/cross-cutting/telemetry-usage-observability.md` for the
  benchmark plumbing, and `docs/architecture/reference/api-surface.md` for the
  endpoint and its four refusal codes.
- `docs/architecture/11-risks-and-technical-debt.md` §11.1 for the named risk
  above.
- An ADR only if a decision here contradicts or extends an existing one (a
  narrow-setter choice in Q2 would be one).

# Demand-driven model-instance autoscaling (#340) — design

Status: draft for approval. Branch-local working document (removed before the
PR; everything durable is folded into `docs/architecture/`).

Epic: GitHub issue #340. First consumer: Voice Chat (#312).

## 1. Intent and why

Today a model the server-agent manages runs as exactly **one process per runtime
spec**. audio.cpp serves one request per process at a time, so one ASR and one
TTS process cap the gateway at **one concurrent voice chat**: a second user who
starts a voice chat while the first is talking gets `503 transcription.live_busy`
(the live slot is per route) or queues behind the first user's synthesis.

The goal is to run **several instances of the same model** — on one host and
across hosts — and to grow and shrink the instance count **with demand**, so N
concurrent voice chats can run on N instances, while idle capacity is given back.

North star (explicitly *not* a goal of this work): one day the gateway is only
handed hosts and decides on its own which model runs where and how often. The
design must not dead-end that. Therefore an instance is a **gateway-named,
individually addressable routing candidate** (the issue's "Decision A"): today an
operator sets per-spec bounds and the gateway fills them on demand; later an
automatic placement can fill the same slot without re-building routing, live
slots or resource accounting.

Success: with a replicable ASR and TTS spec, two (three, …) voice chats run
concurrently without `live_busy`, without stutter, each on its own instance; when
they end, the extra instances unload again.

## 2. Goals, non-goals, anti-goals

Goals
- Multiple instances per runtime spec under one agent, each with its own address,
  its own live slot, its own load figures, and correct per-GPU VRAM / host-RAM
  accounting (N instances = N × demand).
- Demand-driven scale-up (gateway-driven, agent-decided fit) and scale-down
  (agent-local idle unload), bounded by optional per-spec `min`/`max`.
- An audio performance benchmark: real-time throughput (seconds of audio per
  wall second), time to first audio / first text, and **how many concurrent
  streams a host keeps above real time**. Results steer routing (faster
  preferred), hard-gate voice chat (below real time = not used), and cap
  scale-up.
- Instance affinity: a voice chat's successive turns stay on the same warm
  instance when possible; token-pinned conversations pin an instance too.
- Everything ships in **one pull request** (user's explicit decision), built in
  four phases on one branch.

Non-goals
- audio.cpp's experimental in-process `--parallel-jobs` slots; continuous
  batching inside one instance.
- Autonomous placement of models onto hosts (the north star above).
- A cluster-wide `max` across hosts (bounds are per spec = per host).
- Cross-organisation / external scaling.

Anti-goals (would be a failure even if it "works")
- A spec that did not opt in behaves in any way differently from today.
- Scaling out one model evicts another model that is in use or was just used
  (the "only into free resources" rule, §5.4).
- A host is scaled past the point where its streams stutter (§8, §9).
- An OOM of a neighbour or the agent (inherited anti-goal of the RAM work).
- Start/stop thrash (an instance unloaded seconds before it is needed again).

## 3. Decisions agreed with the user

1. **Opt-in per spec** (`replicable`). Off = exactly one instance, as today.
2. **`min` / `max` are optional and per spec (= per host).** No `min` → the
   model can unload completely (min 0). No `max` → instances are added one by
   one for as long as resources allow (the agent's admission and the host's
   `max_processes` are then the only bound).
3. **All instances busy → the request waits** in the gateway's existing admission
   queue (CP4) while a new instance starts **in the background**; the waiting
   request takes whichever frees first — an existing instance or the new one.
4. **Extra instances only into free resources.** An instance beyond the first
   (and above `min`) never evicts another model; conversely such surplus
   instances are the **first** eviction victims when another model needs room.
5. **`min` instances are a promise**: started right after the config push, never
   idle-unloaded, never evicted — like `pinned`.
6. Issue defaults for later phases: the audio ranking feeds routing `Score()` by
   default; voice/live affinity reuses the existing affinity TTL; the automatic
   idle timeout is `max(manual, factor × measured startup)` with a floor (factor
   and floor are my call, §7.3).
7. **Audio benchmark measures real-time throughput** — seconds of audio produced
   (TTS) or transcribed (ASR) per wall-clock second — plus time to first audio /
   first text. Voice chat **hard-gates** on real time (below the threshold a
   host/model is not used for voice chat); faster is preferred everywhere.
8. **Concurrency is measured too** (user: "important"): how many simultaneous
   streams a host keeps above the real-time threshold. That number is an
   additional scale-up ceiling next to VRAM, RAM and `max`.
9. All four phases in **one PR**; after approval of this document the work runs
   autonomously to the finished PR.
10. **Audio benchmarks run like the text benchmarks** and their results —
    including the under-load figure — are **stored on the model mapping like the
    text results**, so reference values exist before a model is loaded (§8.1,
    §8.5).
11. **The ASR benchmark uses real, freely licensed speech that ships in the
    repository** (§8.3).

## 4. Vocabulary

- **Spec**: an agent runtime spec (`agent_runtime_specs`, 1:1 with a model
  mapping). Its id is `S`, its upstream model name (`mapping.app_model_name`) is
  `m`.
- **Instance k** of spec S: one process of S. k is a small integer ≥ 0.
- **Instance address** (what the gateway sends upstream as the model name):
  `m` for k = 0, `m#k` for k ≥ 1. Instance 0 is byte-for-byte today's address,
  which is what keeps non-replicable specs, older gateways and older agents
  unchanged.
- **Instance key** (agent-internal identity of one instance): `S` for k = 0,
  `S#k` for k ≥ 1. Used for the agent's state map, admission identities, config
  dirs and log buffers.
- **Gateway instance key**: `<mapping_id>#<k>` (k ≥ 0) — the per-instance key
  for gateway-side slots (in-flight count, live lock, affinity). `RouteID` stays
  the mapping id (§6.1).
- **Protected count** `P = max(min_instances, pinned || force_running ? 1 : 0)`.
  Instances `0 … P−1` are **protected** (the `min` promise).
- **Baseline instance**: instance 0 and every protected instance
  (`k < max(P, 1)`). A baseline instance is admitted by today's rules (it may
  evict idle neighbours, may bootstrap an unknown demand).
- **Burst instance**: every instance `k ≥ max(P, 1)`. Burst instances follow the
  "free resources only" rule (§5.4). Classification is **by index**, which is
  sound because of the ordering invariant below.
- **Surplus instance** (eviction view): a running burst instance.
- **Siblings**: instances of the same spec.
- **Ordering invariant — highest index leaves first.** An instance may be
  idle-unloaded or chosen as an eviction victim only if no sibling with a higher
  index is running. So instance 0 is always the last instance of a spec to go,
  and "instance 0 stopped while a sibling runs" only happens transiently (a crash
  or restart of instance 0) — a request routed to it then starts it the normal
  baseline way, exactly as a crashed spec restarts today. Gaps in the index range
  are allowed (a crashed burst instance is retired); the invariant is only about
  running higher-index siblings.

## 5. Part A — agent: multiple instances per spec (Phase 1)

### 5.1 Wire: new spec fields

`AgentRuntimeSpecDTO` (gateway) / `runtime.Spec` (agent) gain:

| field | type | meaning |
|---|---|---|
| `replicable` | bool | spec may run more than one instance |
| `min_instances` | int | instances kept warm (0 = none) |
| `max_instances` | int | cap (0 = unbounded) |

The per-instance parallelism (§6.3) is gateway-only and not pushed.
`idle_timeout_seconds` keeps its field but the gateway pushes the *effective*
value for replicable specs (§7.3). None of the new fields is part of the launch
shape (`sameLaunchShape` strips them, like `pinned`): changing them never
restarts a running child.

Agent-side validation in `ParseConfig`: a replicable spec with a fixed
`listen_port` (≠ 0) is treated as non-replicable with a Warn log (two processes
cannot share a fixed port; the gateway already refuses this combination, §11).
Negative values → 0. `min_instances > max_instances` (both > 0) → `min` clamped
to `max`, Warn.

An absolute safety cap on the instance index exists in the agent:
`k ≤ 63` (64 instances per spec). A request naming a larger k, or any `m#k` for
a non-replicable spec, or `k ≥ max_instances` when max > 0, is
`404 runtime.model_not_managed` — exactly like an unknown model.

### 5.2 State: one `specState` per instance

The manager's `specs` map becomes keyed by **instance key**. Each instance has
its own `specState` (state, process, in-flight, last-used, failures/backoff,
measured VRAM/RAM, bootstrap guards), so the existing lifecycle, health, drain,
backoff and measurement machinery is reused unchanged per instance. The
"generation identity" rule (one `*runningProc` pointer per generation) holds per
instance. Comments and docs stating "one process per spec" are updated to "one
process per instance".

- Instance 0 always exists for every spec, as today.
- Instances `1 … min−1` are created on Apply and started immediately (they are
  baseline and protected, §5.5).
- Other instances are created **on demand** when a request or `/ensure` names
  `m#k` (§5.3), and **retired** (state deleted) when their process exits and they
  are not `min`-protected. A burst instance that crashes is not restarted; it
  stays visible as `crashed` until it is next named or the next Apply, then is
  retired.
- Apply: a launch-shape change restarts every instance (each drains and restarts
  as today); spec removed → all instances drain and are removed; `replicable`
  turned off or `max_instances` lowered → instances that are no longer allowed
  drain and are removed.
- `force_stopped` applies to all instances (all drain, ensure refused).
  `force_running` and `pinned` keep instance 0 up as today; the protected count
  `P` (§4) folds them in.
- At an instance's **first successful health probe** the agent triggers an
  immediate VRAM/RAM measurement of that process (instead of waiting for the next
  15 s measurement beat), so a sibling's demand becomes known within seconds of
  the first instance being ready (§5.4 rule 5).

Per-instance resources:
- Port: always ephemeral (§5.1 guarantees `listen_port == 0` for replicable).
- `${MODEL}` expands to the **base** upstream name `m` (the child must never see
  `m#k`).
- `${CONFIG_DIR}` and the config-file directory are per instance key
  (`<configFilesDir>/<S>#<k>` for k ≥ 1) — config files may contain `${PORT}`.
- Log buffer per instance key; `LogStore.Retain` keeps the keys of instances that
  currently exist (so a retired instance's buffer is released at the next Apply).
- Work dir: shared (it is the operator's directory).

### 5.3 Router: addressing and model rewrite

- `byUpstream` keeps mapping base names to spec ids. A model value `m#k` is
  parsed (split on the **last** `#`, k a decimal without sign or leading zeros;
  anything else is the literal name) and resolved to (S, k), subject to §5.1's
  limits.
- **The child always sees the base name.** Before forwarding, the router rewrites
  the model:
  - JSON bodies: rewrite the top-level `model` field (number-preserving
    re-serialization, mirroring the gateway's `rewriteModelField`).
  - `multipart/form-data`: re-encode with the `model` part replaced (mirroring the
    gateway's transcription/image-edit model rewrite); other parts byte-identical.
  - `/live/{model}`: the child query's `model=` uses the base name.
  - `/upstream/{model}`: targets that instance; the child sees the base name.
  - `/ensure/{model}`: ensures that instance (creating it if allowed).
  Only instance addresses (k ≥ 1) are rewritten; for instance 0 the body passes
  untouched exactly as today.
- `/running` and `loaded_models` list running instance addresses (`m`, `m#2`, …).
  `/v1/models` keeps listing base names only.

### 5.4 Admission: siblings, burst, surplus

`RunningProc` gains `Spec` (base spec id; `SpecID` becomes the instance key) and
`Surplus bool`. `PolicySnapshot` gains a per-candidate `NoEvict bool` (same
pattern as `VRAMBootstrapEligible`).

1. **Self-filter** stays by `SpecID` (= instance key), so siblings are ordinary
   occupants: their VRAM/RAM is summed and each counts toward `max_processes`.
2. **Co-residency**: siblings are always allowed; any other pair is looked up as
   `PairKey(base(a), base(b))` (instances inherit their spec's verdicts). Same for
   `pinnedUnknownOccupantRefusal`.
3. **A sibling is never a victim** for a candidate of its own spec (excluded in
   `evictable`). Evicting a warm sibling to start a cold one is never useful.
   And per the ordering invariant (§4) an instance is not evictable while a
   higher-index sibling runs.
4. **Victim order**: surplus instances first (highest index first within a
   spec), then the existing order (candidate's role first, then
   least-recently-used, then id). The same order applies to the cold-start
   escalation's own pick (`evictOneIdleForBootstrap`).
5. **Demand of an instance k ≥ 1**: the spec's pushed demand, raised to the
   largest live measurement of any running sibling (per GPU for VRAM, host-wide
   for RAM). An instance k ≥ 1 is **never admitted with an unknown demand**: a
   baseline instance waits (its state shows the pending reason) until a sibling
   has been measured; a burst instance is refused fast (rule 6). Instance 0 keeps
   today's unknown-demand behaviour (bootstraps, one-shot eviction, …).
6. **Burst admission is all-or-nothing and immediate**: the candidate is admitted
   only if `Admit` answers OK with **no evictions** (`NoEvict`). Any other answer
   (Evict, Wait, unknown demand, bootstrap needed) refuses at once with
   `ErrInstanceNoCapacity` — no queueing, no eviction, no bootstrap shot spent.
   The router answers `409 runtime.instance_no_capacity` (unknown demand:
   `409 runtime.instance_demand_unknown`). The gateway then tries another host
   (§7.2). A request (not an ensure) that names a burst instance which is not
   running gets the same refusal; the gateway re-selects (§6.4).
7. **`min` protection** (by index, `k < P`): the protected instances are
   reported to admission as `Pinned = true` — never victims; `scanIdle` and the
   grace-expiry scan skip them; they restart after a crash with backoff like a
   pinned spec.

### 5.5 Scale-down: agent-local idle unload

`scanIdle` applies to replicable specs per instance, using the pushed effective
idle timeout (§7.3; for replicable specs always > 0). An instance may be
unloaded only if it is not protected (`k ≥ P`) and no higher-index sibling is
running (§4). With no `min`, instance 0 unloads too, last. Non-replicable specs:
unchanged.

### 5.6 Status and measured startup

`Status` (and the telemetry `RuntimeSample`) gain:
- `instance` (int; 0 for instance 0 and for every non-replicable spec),
- `startup_ms` (int): wall time from process start to the first successful
  health probe of the instance's most recent successful start; 0 = unknown.

`SpecID` in `Status` stays the **base** spec id, `Model` is the instance address.
So an older gateway (which never sends replicable specs) sees exactly today's
rows, and a newer gateway folds instance rows by spec id (§6.6). `startup_ms` is
measured for **every** spec (replicable or not) — it is the startup measurement
the scale-down needs (my call, §14).

### 5.7 Feature flag and version

New `agent.Features` entry `runtime_instances` (Since 0.16.0); agent `Version`
0.15.0 → 0.16.0 (one bump for the whole branch, made early). It declares: the
spec fields, instance addressing and rewrite, burst/surplus admission, and the
`instance` / `startup_ms` status fields.

## 6. Part B — gateway: instances as routing candidates (Phase 1)

### 6.1 Candidates and target

- `MappingCandidate` gains `Instance int`. `targetFrom` sets
  `ProviderModel = m` (k = 0) or `m#k` (k ≥ 1) and a new `Target.InstanceKey =
  <mapping_id>#<k>`. Everything that already uses `ProviderModel` upstream (body
  rewrite, multipart rewrite, `/live/{model}`, `/ensure/{model}` — `ExpandModelPath`
  already path-escapes `#`) then addresses the instance without further change.
- **`RouteID` stays the mapping id**: usage records, energy reconciler,
  opportunistic metrics, capability/streaming checks, output-byte-rate cache,
  voice gate lookups and `Score()`'s route id all keep keying by mapping.
- Usage records store the **base** provider model (the `#k` suffix is stripped)
  so analytics do not split by instance (my call).
- **Expansion** happens for a mapping whose spec is `replicable` *and* whose
  server's agent declares `runtime_instances`: candidates are instance 0 plus
  every instance k ≥ 1 the agent currently reports as `running` (from the runtime
  status registry). Instances that are `starting` are not candidates (a queued
  request should take "whichever frees first", §3.3). Everything else yields
  instance 0 only — today's behaviour.
- Expansion must be **re-evaluated on every turn of the admission loop**
  (`resolveStandard` computes candidates once today; the loop must re-read them so
  a parked request can see a freshly started instance). The group path already
  re-reads.
- Instance-aware name matches: `modelLoadedOn`, swap protection and
  `RuntimeModelState(server, name)` use the instance address, so each instance
  reads its own loaded state and active/queue overlay.

### 6.2 Per-instance in-flight count

`ActiveRequest` gains the gateway instance key; the active-request registry
counts in-flight requests per instance key (in addition to today's per-server
count, which keeps its existing uses). Release of a request wakes the admission
queue for its server as today.

### 6.3 Per-instance capacity and the queue

- New runtime-spec field (gateway-only) `instance_parallelism` (int, 0 = 1):
  concurrent requests one instance serves (audio.cpp = 1; a llama.cpp child with
  `--parallel 4` = 4).
- For instance candidates, the capacity filter compares the **instance's**
  in-flight count with `instance_parallelism` — not the server's activity against
  `MaxConcurrency`. The cap is therefore always defined for instance candidates,
  so "all instances at capacity" always reaches the CP4 queue (with
  `errAllAtCapacity`) instead of failing open. Non-instance candidates: unchanged.
- When the capped set is empty and at least one candidate is an instance
  candidate, the resolver emits a **demand signal** to the scaler (§7.1) before
  parking the request.
- The scaler wakes the queue for a server when a new instance reaches running.
  Because candidate expansion reads the telemetry-fed status registry, a parked
  request sees the new instance at most one telemetry interval later (the queue's
  250 ms recheck covers that window; acceptable lag, documented).

### 6.4 Live slots per instance and live failover

- `liveSessionRegistry` and the buffered-transcription `held` check key by the
  gateway **instance key** instead of `RouteID`: N instances = N live slots.
- Selection excludes instance candidates whose live slot is held.
- On a lost race (the slot was taken between selection and `acquire`), the
  resolver **re-selects** excluding that instance (bounded by the candidate
  count) instead of answering `503 transcription.live_busy`. If every instance of
  a replicable model is held, the live request parks in the queue like any other
  request (and signals demand). Non-replicable models keep today's immediate
  `503 transcription.live_busy`.
- A routed request that hits `409 runtime.instance_no_capacity` /
  `instance_demand_unknown` / `404 runtime.model_not_managed` on an instance
  address (k ≥ 1; the instance vanished between telemetry and dispatch) is
  re-selected once excluding that instance. The "INTERIM until #340" comments
  are resolved.

### 6.5 Voice gate and pre-warm

- **The gate never parks.** The voice-chat gate and the live-transcription ticket
  mint resolve in a **select-without-parking** mode: the per-instance cap is used
  only to *prefer* a free instance; when none is free the gate picks the least
  loaded instance anyway, emits a demand signal (a voice chat about to start is
  known upcoming demand) and returns — a ticket POST never blocks in the CP4
  queue. It then ensures the **instance it selected**. A `409
  runtime.instance_*` / `404` from that ensure on a k ≥ 1 instance (it vanished)
  re-selects once; otherwise the existing mapping applies (`admission_blocked` →
  co-load failure, anything else → leg unavailable). The per-turn requests are
  the ones that queue (§6.3).
- The TTS pre-warm targets the gate-time instance (and becomes the session's
  first preferred instance, §10.1).
- **One voice chat uses one TTS instance.** When the TTS leg targets an instance
  of a replicable spec, the run's speech-section concurrency is 1 (sections are
  synthesized one after another on the run's sticky instance) instead of
  `VoiceTTSConcurrencyDefault` (2). That is today's effective behaviour anyway
  (audio.cpp serves one request at a time, so the second section already waited
  inside the one process), the real-time gate (§9) guarantees one instance stays
  ahead of playback, and a run's own second section never looks like new demand.
  Non-replicable TTS targets keep the configured concurrency.

### 6.6 Ingest: folding instance rows

The agent reports one status row per instance. Ingest:
- VRAM write-back per (spec, gpu) and `ram_measured_mb`: take the **maximum**
  over the spec's instance rows in a sample, then write only when changed (no
  write churn from two instances alternating).
- `startup_ms` → persisted per spec as `startup_measured_ms` = the maximum
  non-zero value in the sample, written only when changed by more than 10 %
  (avoid re-pushing the document for jitter, §7.3).
- Context-size / capability write-back: from instance 0's row (or the first
  running instance).
- The volatile status registry keeps every row; portal lookups by spec id handle
  several rows (§11); the "starting" count counts a server once.
- Benchmark isolation checks (`benchmarkOthersStopped`, VRAM isolation) treat
  every instance row: for a cold/VRAM run of S, all other instances of S count as
  "others" and must be stopped.

## 7. Part C — scale-up and placement (Phase 2)

### 7.1 The scaler

A gateway component (`internal/gateway`), one per gateway process.

- **Inputs (demand signals)**: (a) the resolver found every instance candidate of
  a gateway model at capacity (§6.3); (b) live: every live slot of a replicable
  model held (§6.4); (c) voice gate found no free instance for a leg (§6.5).
- **Single-flight per gateway model**: concurrent signals for the same model
  coalesce into one scale-up; the next scale-up for that model can happen once
  the previous one finished (success or failure).
- **Host choice** among the model's mappings whose spec is replicable, on an
  agent with `runtime_instances`, whose server is selectable, reachable and not
  benchmark-busy, and which is **not at a ceiling**:
  - present instances (reported `starting`/`running`) < `max_instances` (if > 0),
  - present instances < the measured real-time concurrency (only when that
    number is a measured *performance* limit, §8.4),
  - no instance of that spec is currently `starting` (never scale next to a
    sibling whose demand is not yet measured),
  - not in refusal cooldown (§7.2).
  A demand signal from a pinned request (§10.2) restricts the choice to the
  pinned server.
  Order: highest routing `Score()` first (the same scorer the resolver uses), so
  faster / less loaded hosts are tried first.
- **Index**: the lowest k ≥ 1 not present in the agent's status for that spec and
  not currently being started by the scaler.
- **Action**: `EnsureRuntimeModel(m#k)` in the background, with a timeout of the
  spec's startup timeout plus a margin. On success: wake the admission queue for
  that server. `min` instances are not the scaler's business (the agent keeps
  them).

### 7.2 Refusal and placement negotiation

The agent has the final say (it holds the real measurements). On
`409 runtime.instance_no_capacity` the scaler puts that (server, spec) in a
**refusal cooldown** (default 60 s) and tries the next host in the same
scale-up. On `409 runtime.instance_demand_unknown` (the sibling's measurement has
not landed yet — normally seconds, §5.2) it only applies a short retry delay
(5 s) and tries the next host; the next demand signal after the delay retries
this one. Other errors: cooldown + Warn log. If no host
accepts, the scale-up ends; parked requests keep waiting for existing instances
(until the queue timeout). There is no retry loop beyond the next demand signal.

### 7.3 Effective idle timeout (scale-down input)

For replicable specs the gateway pushes

`idle_timeout_seconds = max(manual, ceil(factor × startup_measured_ms / 1000), floor)`

with gateway env `OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR` (default **5**) and
`OP_AI_GATEWAY_INSTANCE_IDLE_FLOOR_SECONDS` (default **120**). A manual value of
0 means "no manual override" for replicable specs (for non-replicable specs 0
keeps meaning "never idle-unload", unchanged). Non-replicable specs: pushed as
stored. Changing it never restarts a child.

## 8. Part D — audio performance benchmark (Phase 3)

### 8.1 Same modes, same buttons, same storage as the text benchmarks

User requirement: audio benchmarks are run **exactly like the text-model
benchmarks**, and their results are stored **on the model mapping like the text
results — including the under-load figure** — so reference values exist before
a model is ever loaded.

- **No new mode.** The existing modes apply to audio mappings: `speed` = the
  single-stream audio measurement (§8.2, §8.3), `capacity` = the under-load ramp
  (§8.4), `both` = both. `runBenchmark` dispatches per mapping: a mapping with the
  speech (TTS) or transcription (ASR) capability gets the audio measurement, a
  chat mapping the text one; a server/application run covers both kinds. The same
  portal buttons, endpoints (`/api/portal/{servers|applications|mappings}/{id}/benchmark?mode=`),
  SSE progress, history and reservation apply. `BenchType` gains nothing; the
  audio-only skip in the text paths becomes the audio dispatch.
- **Scheduler**: like text mappings, audio mappings take part in the per-app
  scheduled `speed` run (the audio-only skip there is removed for `speed`).
- **Automatic first run**: additionally, once, when an audio mapping on an idle
  server has no audio metrics yet, or is replicable and has no measured
  real-time concurrency yet (health-loop trigger, same gating style as the
  speech-rate trigger; mode `both`). Normally right after the operator set the
  model up, before real use.
- The existing `speech` mode (byte-rate cache, health-loop speech-rate trigger)
  stays as it is.
- Targets: mappings with the speech or transcription capability, on agent-managed
  or direct audio upstreams. Clone models need a preset voice (reuse
  `speechBenchmarkSkip`).
- The usual reservation applies (one run per server, idle gate, `ServerBusy`
  excludes the server from routing during the run).
- Each measurement goes **directly to a specific instance** (like the speech
  benchmark bypasses routing), after one short warm-up request (cold start is not
  part of the throughput figure).

### 8.2 TTS

- Byte rate: the cached `output_byte_rate`; if missing, the existing non-streamed
  speech measurement runs first to obtain it.
- Streaming models (`speech_streaming` = yes): one streamed request (SSE) with a
  fixed English passage of roughly 20 s of audio.
  `ttfa_ms = t(first speech.audio.delta) − t(send)`;
  `audio_s = decoded PCM bytes / byte rate`;
  `rate_x = audio_s / (t(done) − t(first delta))` — the production speed after
  the first chunk, which is what decides whether playback stutters.
  `rtf = (t(done) − t(send)) / audio_s` is recorded too (existing column).
- Non-streaming models: `rate_x = audio_s / wall`, `ttfa_ms = wall`.

### 8.3 ASR

- Source audio: a **bundled clip** of real speech, embedded in the gateway
  binary (`go:embed`): six sentences from the **Thorsten-Voice** dataset
  (German, single speaker, **CC0 1.0**; `Thorsten-Voice/TV-44kHz-Full`, subset
  `TV-2022.10-Neutral`), joined with short pauses into one ≈ 22.6 s clip,
  converted to 16 kHz mono s16le WAV (≈ 0.7 MB). The transcript ships next to it;
  the asset directory carries a source/licence note and
  `docs/architecture/cross-cutting/licensing.md` records it. The ASR request
  passes `language=de`.
- Buffered transcription: `rate_x = audio_s / wall`.
- Live transcription (`transcription_streaming` = yes): the PCM is sent unpaced
  over the live endpoint; `ttft_ms = t(first transcript.text.delta) − t(send)`;
  `rate_x = audio_s / (t(done) − t(send))`.
- A model with both: the live result is the stored one (voice chat uses live).

### 8.4 Concurrency ramp

k = 1, 2, 3, …: run k simultaneous measurements and record each stream's
`rate_x`. For a replicable spec on a `runtime_instances` agent each stream gets
its own instance (ensure instances 0 … k−1 first; burst rules apply — only free
resources, nothing is evicted). For any other audio mapping all k streams go to
its single process (the same "concurrency inside one process" a text capacity
run measures; audio.cpp serializes, so this normally yields 1). Level k is good if the **slowest** stream's `rate_x ≥
threshold`. Stop at the first bad level, a refused ensure, `max_instances`, or
`OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX` (default 8).
`realtime_concurrency` = the last good k (0 = not real-time even alone), stored
with **why the ramp stopped** — `performance` (a level fell below the
threshold), `resources` (an ensure was refused: neighbours held the room at that
moment) or `cap` (`max_instances` or the ramp cap reached). Only a
`performance` stop is a measured ceiling; `resources` / `cap` make k a lower
bound ("≥ k" in the portal) and the scaler does not treat it as a ceiling.
Instances started by the ramp are ordinary burst instances afterwards and
idle-unload normally.

Threshold: `OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR`, default **1.2** (20 %
safety margin over real time).

### 8.5 Storage

- History: a `kind = "audio"` row per run with `ttft_ms` (TTFA for TTS, TTFT for
  ASR), `wall_ms`, `rtf`, and the per-level curve (k → slowest/mean rate) as JSON
  in the existing `capacity_curve` column.
- Mapping (new columns): `audio_rate_x` (double precision), `audio_ttfa_ms`
  (bigint), `audio_realtime_concurrency` (integer),
  `audio_concurrency_limit` (text: `performance` / `resources` / `cap`),
  `audio_metrics_updated_at`.
  Written by the audio `speed` / `capacity` runs exactly like the text metrics:
  under the existing `metrics_locked` guard, stamping `metrics_source` /
  `metrics_updated_at` the same way. Like the text metrics they are also
  **editable in the mapping editor** (an operator may enter reference values by
  hand and lock them). 0 / NULL = unknown. Routing and the scaler read them from
  the mapping, so they apply whether or not the model is currently loaded.

## 9. Part E — routing uses the audio metrics (Phase 3)

- **Score**: for requests with the speech or transcription capability, a third
  bounded tiebreak term `clampBonus(audio_rate_x, 10, 50)` (+10 points per × of
  real time, at most 50) — together with the existing terms well below the
  error-rate penalty (200). Unmeasured = no bonus.
- **Voice hard gate**: the voice chat legs (ASR and TTS), at ticket mint *and* on
  every per-turn resolution, exclude candidates with a **measured** `audio_rate_x`
  below the threshold. Unmeasured candidates stay eligible (a new host must be
  usable before its first measurement). If no candidate is left, the ticket mint
  fails with a new stable error `voice_chat.no_realtime_model` naming the leg.
  The per-turn legs carry a "real-time required" marker on the loopback request
  so the resolver applies the gate.
- **Scale-up ceiling**: §7.1 (present instances < `audio_realtime_concurrency`).
- The portal warns (not refuses) when `min_instances` exceeds the measured
  real-time concurrency of that spec.

## 10. Part F — instance affinity (Phase 4)

### 10.1 Voice chat session stickiness

The voice chat session (server side) remembers, per leg (ASR, LLM, TTS), the
gateway instance key it used last (the gate-time instance for the first turn).
Each turn passes it as a **preferred instance** on the loopback request. The
resolver uses the preferred instance if it is still a candidate, under its cap,
and passes every gate (incl. real time); otherwise it selects normally (advisory
— never fails because of stickiness). No `route_affinity` row is involved (the
voice legs run under a session principal without a token id).

### 10.2 Token affinity carries the instance

`route_affinity` gains an `instance` column (default 0). Writes store the chosen
instance. A continuous `/live` socket is already pinned for its lifetime.

**Pinned requests and instances** — the same rule for a token-affinity hit and
for a server override (`resolveServerOverride`; it also governs the voice
per-turn hops of an overridden token), both of which today return before
`selectCandidate` and always land on instance 0:
1. Only the pinned server's instances of the mapping are considered.
2. Prefer the pinned instance (affinity) if it is a candidate and under its cap;
   else any instance on that server under its cap (lowest load).
3. None free: a **non-live** request overflows onto the preferred instance —
   today's "pinned requests never queue" semantics stay — and a **live** request
   (one slot per instance, overflow impossible) parks in the queue restricted to
   that server's instances. Both emit a demand signal restricted to the pinned
   server (§7.1).
The pin itself (server, TTL) is never deleted by instance changes.
Non-replicable mappings: unchanged.

## 11. Portal (all phases)

- Runtime spec editor (`RuntimeAdminSection`): checkbox "Multiple instances"
  (`replicable`); when on: `min` (optional, empty = 0), `max` (optional, empty =
  unlimited), "parallel requests per instance" (default 1); a read-only line
  with the measured startup and the resulting automatic idle timeout. `pinned`
  stays (it keeps instance 0 up).
- Validation (portal service, stable codes): negative values →
  `runtime_spec.tuning_invalid` (existing); `min > max` (both set) →
  `runtime_spec.instances_invalid`; `replicable` with a fixed `listen_port` →
  `runtime_spec.instances_need_ephemeral_port`. The full-replace PUT contract and
  `putRequestFromDTO` (benchmark writes) carry the new fields.
- Status tab: one row per instance (row key spec id + instance; an "Instance"
  column `#k`); the spec list's live-status column aggregates ("2 running · min 1
  · max ∞").
- Models catalog (`ModelServersSection`): per mapping the running instance count;
  audio columns (× real time, time to first audio/text, real-time concurrency).
- Benchmark panel: no new type — the existing speed / capacity / both buttons
  run the audio measurement on audio mappings (§8.1); an audio result line in
  the live panel and audio history rows.
- Mapping editor: the audio metrics are shown and editable next to the text
  metrics, under the same lock (§8.5).
- German and English keys together.

## 12. Data model and wire summary

Migrations (next free number is 100; the builder may merge them):
- `agent_runtime_specs`: `replicable` (bool convention of the table),
  `min_instances`, `max_instances`, `instance_parallelism` (integer, default 0),
  `startup_measured_ms` (bigint, default 0).
- `model_mappings`: `audio_rate_x` (double precision), `audio_ttfa_ms` (bigint),
  `audio_realtime_concurrency` (integer), `audio_concurrency_limit` (text),
  `audio_metrics_updated_at`.
- `route_affinity`: `instance` (integer, default 0).
All three store implementations (memory, SQL for sqlite + postgres) and the
conformance / column-parity tests; ADR-005 wide types.

Stable error codes (new): `runtime.instance_no_capacity` (409),
`runtime.instance_demand_unknown` (409), `runtime_spec.instances_invalid` (400),
`runtime_spec.instances_need_ephemeral_port` (400),
`voice_chat.no_realtime_model`.

Gateway env (new): `OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR` (5),
`OP_AI_GATEWAY_INSTANCE_IDLE_FLOOR_SECONDS` (120),
`OP_AI_GATEWAY_INSTANCE_SCALE_COOLDOWN_SECONDS` (60),
`OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR` (1.2),
`OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX` (8).

Compatibility: a gateway talking to an agent without `runtime_instances` sends
the new spec fields (ignored by the old agent's decoder), never expands
candidates, never scales — exactly today. An old gateway never sends
`replicable`, so a new agent runs one instance per spec — exactly today.

## 13. Phases (one branch, one PR)

1. **Agent multi-instance + gateway instance candidates**: §5, §6 (incl. live
   slots/failover and per-instance capacity), portal spec fields + per-instance
   status (§11 parts), migrations for the spec columns. Static use already
   works: `min` instances warm, plus instances started by `/ensure`.
2. **Scaler + scale-down**: §7 (demand signals, host choice, refusal cooldown,
   effective idle timeout from the agent-measured startup).
3. **Audio benchmark + routing use**: §8, §9 (benchmark mode, ramp, mapping
   columns, Score term, voice hard gate, scale-up ceiling, portal columns).
4. **Affinity**: §10.1, §10.2.
Then: architecture docs folded, working files removed, full verification, PR.

## 14. Calls I made (please check while reading)

- **Instance address = model name with a `#k` suffix** (k ≥ 1), instance 0 =
  bare name. Chosen over an instance header because every upstream path in the
  gateway already carries `ProviderModel`, and every name-keyed lookup (loaded
  models, per-model state, swap protection) then becomes per instance for free;
  the cost is one contained model rewrite in the agent router.
- **Baseline vs. burst by index, plus "highest index leaves first".** Instance 0
  and the `min` instances are admitted the normal way (may evict, like today);
  every further one only into free resources. Unload and eviction always take
  the highest-index running instance of a spec first, so instance 0 is the last
  to go and the index classification never meets "instance 0 is cold while a
  burst sibling runs" except transiently after a crash.
- **The voice gate and live ticket mint never park** (select-without-parking):
  they prefer a free instance, otherwise take the least loaded one and signal
  demand; only the per-turn requests queue.
- **One voice chat = one TTS instance**: speech sections of one run are
  synthesized one after another on its sticky instance (concurrency 1 instead of
  2 for instance targets) — today's effective behaviour on audio.cpp.
- **Pinned requests** (token affinity, server override) pick an instance on the
  pinned server; when none is free, non-live requests overflow onto the
  preferred instance (pins never queue, as today) and live requests park; both
  signal demand restricted to that server.
- **Audio benchmarks reuse the text modes** (speed = single stream, capacity =
  under load, both), dispatched per mapping; results land on the mapping like
  text metrics (lock, source, editable) — the user's requirement, the mode
  mapping is my call. The TTS passage stays English (like today's speech
  benchmark); the bundled ASR clip is German (Thorsten-Voice, CC0).
- **The concurrency ramp records why it stopped**; only a performance stop is a
  scale-up ceiling (a resource refusal or a cap makes the number a lower bound).
  The automatic first run includes the ramp, only on an idle server.
- **No scale-up next to a starting sibling**; an `instance_demand_unknown`
  refusal only delays 5 s (not the 60 s cooldown), and the agent measures a new
  instance immediately at its first healthy probe.
- **An instance k ≥ 1 never starts with an unknown demand** — it inherits its
  running sibling's measurement or waits/refuses. Avoids two unmeasured
  siblings bootstrapping on the same card.
- **Startup is measured by the agent** (process start → first healthy probe) on
  every start, not by a cold-minus-warm benchmark. It needs no disruptive
  stop-all and is refreshed on every real start. The issue suggested the
  benchmark technique; the agent measurement serves the same purpose (idle
  timeout) better.
- **Per-instance parallelism is a new spec field** (default 1) rather than the
  mapping's benchmark-filled `max_concurrency` (chat-only, 0 for audio).
- **Starting instances are not routing candidates**; a parked request takes
  whichever is free first.
- Idle factor 5, floor 120 s; refusal cooldown 60 s; real-time threshold 1.2×;
  ramp cap 8; instance-index cap 64.
- Audio Score bonus +10 per × real time, at most 50.
- `idle_timeout_seconds = 0` on a *replicable* spec means "no manual override"
  (the automatic value applies), while on a non-replicable spec it keeps meaning
  "never idle-unload".
- Usage records keep the base model name (no per-instance analytics split).
- Voice hard gate failing = ticket mint error `voice_chat.no_realtime_model`
  (no stuttering fallback).
- Real-time concurrency is measured per spec in isolation; contention between
  *different* models on one GPU is not modelled (documented acceptance).

## 15. Left to the builder

- Exact Go type/func names, file splits, and where the scaler lives inside
  `internal/gateway`.
- The fixed TTS benchmark passage (English, ~20 s of speech).
- How the voice session passes the preferred instance on the loopback request
  (header vs. internal request field).
- Migration grouping (one or several numbers ≥ 100).
- Portal layout details within `RuntimeAdminSection` / `ModelServersSection`.

## 16. Verification expectations

- TDD per unit. Admission: table tests for sibling co-residency, inherited pairs,
  sibling-never-victim, surplus-first victim order, burst all-or-nothing (Evict →
  refused, Wait → refused), unknown-demand instance handling, min protection,
  `max_processes` counting instances. Manager: on-demand creation, retirement,
  Apply transitions (shape change, replicable off, max lowered), idle unload
  respecting min, startup_ms measurement. Router: suffix parsing edge cases,
  JSON/multipart/live rewrite, 404 for disallowed k.
- Gateway: candidate expansion per admission turn, per-instance cap and queue
  engagement, demand signal, live failover, ingest folding, scaler (single-flight,
  host order, ceilings, cooldown, wake), effective idle timeout, audio benchmark
  measurement math against fake streams, Score term, voice gate, affinity with
  instance.
- Store: migrations on memory/sqlite/**postgres** (provisioned like CI).
- Both Go modules: `golangci-lint fmt --diff` + `run`, `go test ./...`
  (+ `-race` for `internal/runtime`), `GOOS=windows go vet` for the agent.
  Portal: test, build, lint, `format:check`. `make lint-docs`. Local SonarQube:
  branch-attributed findings = 0.

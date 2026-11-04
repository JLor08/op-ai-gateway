# Model-Instance Autoscaling (#340) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run several instances of one agent-managed model per host and across
hosts, scale them with demand, measure audio real-time capacity like the text
benchmarks, and keep voice chats on warm instances — all four phases of #340 in
one branch / one PR.

**Architecture:** Each instance is its own agent `specState` keyed `S` / `S#k`
and addressed upstream as `m` / `m#k`; the agent rewrites the model back to `m`
before the child sees it. The gateway treats every running instance as its own
routing candidate (per-instance in-flight cap, live slot, load), a gateway
scaler starts burst instances via `/ensure/m#k` on demand, the agent decides fit
("free resources only" for burst instances) and unloads idle instances
highest-index first. Audio benchmarks reuse the text modes and persist on the
mapping; routing reads them for a score term, a voice real-time gate and a
scale-up ceiling.

**Tech Stack:** Go 1.26 (gateway `gateway/backend`, agent `server-agent`),
SQL store (sqlite + postgres via dialect seam) + memory store, React/TypeScript
portal (`gateway/frontend`, vitest, MUI), ffmpeg only for the already-prepared
clip.

**Spec:** `docs/superpowers/specs/2026-11-02-model-instance-autoscaling-design.md`
(read it first; section numbers below — §n — refer to it).

## Global Constraints

- Never commit to `main`; work on branch `claude/autoscaling-340-1469d8`. Repo text in English; no attribution lines in commits.
- A spec that did not opt in (`replicable=false`) behaves exactly as today — every task keeps the existing tests green unchanged.
- Instance address: `m` for k=0, `m#k` for k≥1 (decimal, no sign, no leading zero, 1…63). Agent instance key `S` / `S#k`. Gateway instance key `<mapping_id>#<k>` for every k (incl. 0). `RouteID` stays the mapping id.
- Agent `Version` 0.15.0 → **0.16.0** (bump once, in Task 1); new feature `runtime_instances` Since `0.16.0`.
- Migrations: next free is **100**; append-only via `addColumnIfMissing`; ADR-005 wide types (`bigint`, `double precision`); all stores (memory + SQL) and conformance/parity tests; verify with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- New stable error codes (exact strings): `runtime.instance_no_capacity` (409), `runtime.instance_demand_unknown` (409), `runtime_spec.instances_invalid` (400), `runtime_spec.instances_need_ephemeral_port` (400), `voice_chat.no_realtime_model`.
- New gateway env (exact names/defaults): `OP_AI_GATEWAY_INSTANCE_IDLE_FACTOR`=5, `OP_AI_GATEWAY_INSTANCE_IDLE_FLOOR_SECONDS`=120, `OP_AI_GATEWAY_INSTANCE_SCALE_COOLDOWN_SECONDS`=60, `OP_AI_GATEWAY_AUDIO_REALTIME_MIN_FACTOR`=1.2, `OP_AI_GATEWAY_AUDIO_CONCURRENCY_MAX`=8.
- Fixed values: demand-unknown retry delay 5 s; ingest startup write threshold 10 %; audio score `clampBonus(rate, 10, 50)`; instance index cap 63.
- Portal: every new i18n key in German and English together (`i18n.ts`).
- Per task: TDD; `cd server-agent && go test ./internal/...` or `cd gateway/backend && go test ./internal/<pkg>/...`; frontend `npm test -- <file>`. Before each commit of Go code run `golangci-lint fmt --diff` and `golangci-lint run ./...` in the touched module (gofumpt, gocritic gate CI).
- Update `docs/implementation-status.md` (branch-local) at each task end with one line: task, status, notes.

## Review Focus

- A real model name that contains `#` (e.g. a spec whose upstream model literally is `foo#2`, or `m#2` naming a non-replicable spec) must keep routing literally: exact `byUpstream` match wins before instance parsing (Task 2, Task 5 tests).
- A config push that lowers `max_instances`, turns `replicable` off or removes the spec while a burst instance has requests in flight must drain (finish in-flight), never kill mid-request (Task 4 test).
- Telemetry lag: the gateway routes to `m#k` that the agent retired a moment ago → `404`/`409` → the gateway re-selects once and the client never sees it (Task 11 test).
- Agent restart / reconnect: instance rows vanish from the full status snapshot; candidates, scaler counts and affinity pins must drop them without errors (Task 9 and Task 20 tests).
- Mixed fleet: an agent without `runtime_instances` next to one with it — no expansion, no `m#k`, no scale-up on the old one (Task 9 and Task 13 tests).

---

## Phase 1 — Agent multi-instance

### Task 1: Agent wire fields, feature flag, version

**Files:**
- Modify: `server-agent/internal/runtime/types.go` (`Spec`, `Status`, `ParseConfig`)
- Modify: `server-agent/internal/runtime/manager.go` (`sameLaunchShape` strip list)
- Modify: `server-agent/internal/agent/features.go`, `server-agent/internal/agent/agent.go` (`Version`)
- Test: `server-agent/internal/runtime/types_test.go`, `server-agent/internal/agent/features_test.go` (existing registry guard)

**Interfaces:**
- Produces: `Spec.Replicable bool \`json:"replicable,omitempty"\``, `Spec.MinInstances int \`json:"min_instances,omitempty"\``, `Spec.MaxInstances int \`json:"max_instances,omitempty"\``; `Status.Instance int \`json:"instance"\``, `Status.StartupMS int \`json:"startup_ms,omitempty"\``; feature name constant `FeatureRuntimeInstances = "runtime_instances"`.

- [ ] **Step 1: Failing tests** in `types_test.go`:
  `TestParseConfigReplicableFields` — JSON `{"replicable":true,"min_instances":1,"max_instances":4}` decodes to those values;
  `TestParseConfigReplicableFixedPortDemoted` — `replicable:true, listen_port:9000` → `Replicable==false`;
  `TestParseConfigInstanceCountsSanitized` — `min_instances:-2,max_instances:-1` → both 0; `min_instances:5,max_instances:3` → `MinInstances==3`;
  `TestSameLaunchShapeIgnoresInstanceFields` — two specs differing only in the three fields → `sameLaunchShape==true`.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ -run 'TestParseConfig(Replicable|Instance)|TestSameLaunchShapeIgnoresInstanceFields'` → FAIL (unknown fields).
- [ ] **Step 3: Implement** the fields, the `ParseConfig` sanitation (Warn logs as for other demotions), the strip in `sameLaunchShape`; add the `Status` fields (populated in Task 4); register `runtime_instances` (Since `0.16.0`) and set `Version = "0.16.0"`.
- [ ] **Step 4: Run** `go test ./internal/runtime/ ./internal/agent/` → PASS (incl. `TestFeatureRegistry`).
- [ ] **Step 5: Commit** `feat(agent): replicable/min/max spec fields and runtime_instances feature (0.16.0)`.

### Task 2: Instance addressing helpers (pure)

**Files:**
- Create: `server-agent/internal/runtime/instance.go`
- Test: `server-agent/internal/runtime/instance_test.go`

**Interfaces:**
- Produces (package `runtime`):
  - `const MaxInstanceIndex = 63`
  - `func InstanceAddress(base string, k int) string` — `base` for k==0, `base+"#"+itoa(k)` otherwise
  - `func ParseInstanceAddress(model string) (base string, k int)` — split on the **last** `#`; suffix must be decimal 1…63 without sign/leading zero, else `(model, 0)`
  - `func InstanceKey(specID string, k int) string` — same scheme as `InstanceAddress`
  - `func ProtectedCount(spec Spec) int` — `max(spec.MinInstances, b)` where `b = 1` if `spec.Pinned || spec.AdminState=="force_running"` else 0; `MinInstances` ignored (treated 0) when `!spec.Replicable`
  - `func InstanceAllowed(spec Spec, k int) bool` — k==0 always; k≥1 needs `Replicable && k <= MaxInstanceIndex && (MaxInstances==0 || k < MaxInstances)`
  - `func IsBurst(spec Spec, k int) bool` — `k >= max(ProtectedCount(spec), 1)`

- [ ] **Step 1: Failing tests** — table tests:
  `ParseInstanceAddress`: `"m"→("m",0)`, `"m#2"→("m",2)`, `"m#63"→("m",63)`, `"m#64"→("m#64",0)`, `"m#0"→("m#0",0)`, `"m#02"→("m#02",0)`, `"m#-1"→("m#-1",0)`, `"a#b#3"→("a#b",3)`, `"m#"→("m#",0)`;
  `InstanceAllowed`: non-replicable k=1 false; replicable max=0 k=63 true, k=64 false; max=4 k=3 true, k=4 false;
  `ProtectedCount`: replicable min=2 → 2; pinned min=0 → 1; non-replicable min=3 pinned=false → 0;
  `IsBurst`: min=0 k=1 true, k=0 false; min=3 k=2 false, k=3 true.
- [ ] **Step 2: Run** `go test ./internal/runtime/ -run TestInstance` → FAIL.
- [ ] **Step 3: Implement** in `instance.go`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(agent): instance address/key helpers`.

### Task 3: Admission — siblings, burst (NoEvict), surplus, ordering invariant

**Files:**
- Modify: `server-agent/internal/runtime/policy.go` (`RunningProc`, `PolicySnapshot`, `Decision`, `newAdmission`, `ruleMatrix`, `pinnedUnknownOccupantRefusal`, `evictable`, `orderVictims`, `decide`)
- Test: create `server-agent/internal/runtime/policy_instances_test.go`

**Interfaces:**
- Consumes: Task 2 helpers.
- Produces: `RunningProc.Spec string` (base spec id; `SpecID` stays the unique instance key), `RunningProc.Instance int`, `RunningProc.Surplus bool`; `PolicySnapshot.NoEvict bool`; `Decision.NoCapacity bool` (set instead of Evict/Wait when `NoEvict`, `Evict` then empty, `OK=false`, `Message` names the cause). Helper `func baseOf(r RunningProc) string` (returns `r.Spec`, or `r.SpecID` when `Spec==""` so existing tests that never set it keep working). The candidate side reads the base id from a new `Spec`-level field: add `Spec.InstanceOf string \`json:"-"\`` (set by the manager for instance k≥1 copies, "" = itself) and `Spec.Instance int \`json:"-"\``.

- [ ] **Step 1: Failing tests** (`policy_instances_test.go`), each a snapshot + `Admit` call:
  - `TestAdmitSiblingsAreAlwaysCoResident` — candidate `S#1` (InstanceOf `S`), occupant `S` running, `Allowed` empty map → `OK`.
  - `TestAdmitInstanceInheritsSpecPairs` — candidate `S#1`, occupant `T`, `Allowed[PairKey("S","T")]` present → OK; absent (opt_in-style matrix) → occupant `T` idle is queued as victim exactly as for `S`.
  - `TestAdmitSiblingNeverVictim` — budget full only by idle sibling `S#2` → decision is `Wait` (not Evict `S#2`).
  - `TestAdmitLowerIndexNotEvictableWhileHigherRuns` — candidate `X` needs room; idle `S` (Instance 0) and busy `S#1` (InFlight 1) on the GPU → `S` is not proposed (Wait).
  - `TestAdmitSurplusVictimsFirst` — idle `S#1` (Surplus) last used recently, idle `T` used long ago, same role as candidate → Evict list starts with `S#1`.
  - `TestAdmitNoEvictRefusesInsteadOfEvicting` — `NoEvict=true`, fit only by evicting idle `T` → `NoCapacity==true`, `Evict` empty, `OK==false`.
  - `TestAdmitNoEvictRefusesInsteadOfWaiting` — `NoEvict=true`, busy occupant blocks → `NoCapacity==true`, `Wait==false`.
  - `TestAdmitNoEvictOKWhenFree` — `NoEvict=true`, room free → `OK`.
  - `TestAdmitInstancesCountTowardMaxProcesses` — `MaxProcesses=2`, running `S`,`S#1`, candidate `S#2` with `NoEvict` → `NoCapacity`.
  - `TestAdmitSiblingVRAMSummed` — budget 10000, `S` measured 4000, candidate `S#1` demand 4000 → OK; candidate `S#2` with `S#1` also running → NoCapacity under NoEvict.
- [ ] **Step 2: Run** `go test ./internal/runtime/ -run TestAdmit` → new tests FAIL, all existing `TestAdmit*` still PASS.
- [ ] **Step 3: Implement**: self-filter stays by `SpecID`; sibling check `baseOf(r)==candidateBase` short-circuits the pair rules (rule 1 and `pinnedUnknownOccupantRefusal`); pair lookup `PairKey(baseOf(a), baseOf(b))`; `evictable` additionally excludes siblings and any `r` with a running higher-index sibling in `a.running`; `orderVictims` sorts `Surplus` first (higher `Instance` first within a base), then the existing keys; `decide` maps any non-OK/Evict outcome to `NoCapacity` when `snap.NoEvict`. Keep the existing order of rules and the `toEvict` chaining untouched.
- [ ] **Step 4: Run** `go test -race ./internal/runtime/ -run TestAdmit` → PASS.
- [ ] **Step 5: Commit** `feat(agent): instance-aware admission (siblings, burst no-evict, surplus first)`.

### Task 4: Manager — one specState per instance

**Files:**
- Modify: `server-agent/internal/runtime/manager.go` (specs map keying, `applyConfig`, `rebuildUpstreamIndex`, `handleEnsure`, `admitAndStart`, `buildSnapshot`, `onProcExited`, `scanIdle`, `earliestGraceExpiry`, `evictOneIdleForBootstrap`, `handleStartResult`, `snapshotStatus`, `loadedModels`, sentinel errors), `server-agent/internal/runtime/policy_local.go` (`${MODEL}`, `${CONFIG_DIR}`), `server-agent/internal/runtime/config_files.go` (dir per instance key), `server-agent/internal/runtime/logs.go` (Retain keys)
- Test: create `server-agent/internal/runtime/manager_instances_test.go` (use the fake process/launcher fixtures `manager_test.go` already uses)

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: `ErrInstanceNoCapacity = errors.New("runtime.instance_no_capacity")`, `ErrInstanceDemandUnknown = errors.New("runtime.instance_demand_unknown")`; `EnsureRunning(ctx, model)` accepts instance addresses; `Status` rows carry base `SpecID`, `Instance`, `Model = InstanceAddress(m,k)`, `StartupMS`; `LoadedModels()` returns running instance addresses.
- Behaviour pinned by spec §5.2–§5.6: instance 0 + protected instances exist after Apply and the protected ones start; k≥1 created on demand if `InstanceAllowed`; burst (`IsBurst`) admission runs with `NoEvict` and maps `NoCapacity` → `ErrInstanceNoCapacity` (no waiter queued, no bootstrap shot); a k≥1 candidate whose demand is unknown even after inheriting the largest live sibling measurement (VRAM per GPU, RAM) → burst: `ErrInstanceDemandUnknown`; baseline: waits (pending state message); protected instances are `Pinned=true` in the snapshot; `Surplus=true` for running burst instances; idle unload only when `k >= ProtectedCount` and no higher-index sibling runs; burst instance that exits is retired (crash: kept as `crashed` row until named again or next Apply); `StartupMS` = start→first healthy probe of the last successful start; first healthy probe triggers an immediate measurement dispatch for that process; `${MODEL}` = base upstream; `${CONFIG_DIR}` and config files under `InstanceKey(S,k)`; `LogStore.Retain` gets current instance keys.

- [ ] **Step 1: Failing tests** (`manager_instances_test.go`):
  - `TestInstancesMinStartedOnApply` — replicable min=2 → `S` and `S#1` reach Running; `Status()` has 2 rows, both `SpecID=="S"`, instances 0 and 1, models `m` and `m#1`.
  - `TestEnsureCreatesBurstInstanceOnDemand` — `EnsureRunning(ctx,"m#2")` → Running row `Instance==2`; `LoadedModels()` contains `"m#2"`.
  - `TestEnsureDisallowedInstanceIsNotManaged` — non-replicable spec: `"m#1"` → `ErrModelNotManaged`; replicable max=2: `"m#2"` → `ErrModelNotManaged`.
  - `TestBurstInstanceRefusedWhenNoFreeRoom` — budget holds one instance; `S` running; `EnsureRunning("m#1")` → `ErrInstanceNoCapacity` immediately (no wait), and the idle neighbour `T` is still Running.
  - `TestBurstInstanceDemandUnknownRefused` — no declared VRAM/RAM and no sibling measurement → `ErrInstanceDemandUnknown`.
  - `TestBurstInheritsSiblingMeasurement` — `S` measured 3000 MB VRAM via the fake measurer; `m#1` admitted with charge 3000.
  - `TestIdleUnloadHighestIndexFirst` — instances 0,1,2 idle past timeout; first scan drains only `S#2`; `S` drains last.
  - `TestIdleUnloadKeepsProtected` — min=1: `S` never idle-unloaded.
  - `TestBurstExitRetiresInstance` — `S#1` drained by idle → its row disappears from `Status()`.
  - `TestApplyReplicableOffDrainsInstancesGracefully` — `S#1` with 1 request in flight; Apply with `replicable:false` → `S#1` Draining, the in-flight request's `release()` completes, then the row is gone; `S` untouched.
  - `TestApplyMaxLoweredDrainsDisallowed` — max 4→2 with `S#3` running → `S#3` drained+removed.
  - `TestInstanceUsesBaseModelAndOwnConfigDir` — launched args for `m#1` with `${MODEL}` and `${CONFIG_DIR}` → base `m` and a dir ending in `S#1`.
  - `TestStartupMSReported` — fake child healthy after ~N ms → `Status().StartupMS > 0`.
  - `TestNonReplicableUnchanged` — existing single-spec flow: one row, `Instance==0`, model `m`.
- [ ] **Step 2: Run** `go test ./internal/runtime/ -run 'TestInstances|TestEnsure(Creates|Disallowed)|TestBurst|TestIdleUnload|TestApply(Replicable|Max)|TestInstanceUses|TestStartupMS|TestNonReplicable'` → FAIL.
- [ ] **Step 3: Implement** per the Interfaces block. `specState` gains `instance int` and `startupMS int`; the per-instance `Spec` copy sets `InstanceOf`/`Instance`. Update the "one process per spec" comments/package doc to "per instance".
- [ ] **Step 4: Run** `go test -race ./internal/runtime/` → PASS (whole package, all pre-existing tests unchanged).
- [ ] **Step 5: Commit** `feat(agent): one specState per instance — on-demand burst instances, min, highest-index-first unload`.

### Task 5: Router addressing + model rewrite; telemetry fields

**Files:**
- Modify: `server-agent/internal/runtime/router.go` (`serveProxy`, multipart branch, `liveChildQuery`, `/upstream`, `/ensure`, `/running`, `sentinelCode`)
- Modify: `server-agent/internal/agent/agent.go` + `server-agent/internal/sample/sample.go` (`RuntimeSample.Instance`, `StartupMS` → JSON `instance`, `startup_ms`)
- Test: create `server-agent/internal/runtime/router_instances_test.go`; extend `server-agent/internal/agent` telemetry test that asserts the runtime sample fields

**Interfaces:**
- Consumes: Task 4 (`EnsureRunning` with instance addresses, sentinel errors).
- Produces: router resolves exact `byUpstream` match first, then `ParseInstanceAddress`; child always receives base `m` (JSON top-level `model` rewritten number-preservingly — mirror gateway `rewriteModelField`; multipart `model` part replaced, other parts byte-identical; live child query `model=m`); `sentinelCode`: `ErrInstanceNoCapacity`/`ErrInstanceDemandUnknown` → 409 with their codes; telemetry sample carries `instance`, `startup_ms`.

- [ ] **Step 1: Failing tests**:
  - `TestRouterRewritesInstanceModelJSON` — POST `{"model":"m#1","n":1.50}` → child sees `"model":"m"` and `1.50` unchanged.
  - `TestRouterRewritesInstanceModelMultipart` — form with `model=m#1` + file part → child form `model=m`, file bytes identical.
  - `TestRouterLiveInstanceQueryUsesBase` — `/live/m%231` → child query `model=m`.
  - `TestRouterInstanceZeroUntouched` — `{"model":"m"}` body forwarded byte-identical.
  - `TestRouterLiteralHashModelWins` — spec whose upstream is literally `foo#2` → request `foo#2` reaches that spec unchanged (Review Focus 1).
  - `TestRouterBurstNoCapacityIs409` — `/ensure/m%231` with no room → 409 `runtime.instance_no_capacity`.
  - Telemetry: `TestRuntimeSampleCarriesInstanceAndStartup` — Status `{Instance:2, StartupMS:900}` → sample JSON has `"instance":2,"startup_ms":900`.
- [ ] **Step 2: Run** `go test ./internal/runtime/ -run TestRouter ./internal/agent/` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test -race ./...` in `server-agent`; `GOOS=windows go vet ./...` → PASS.
- [ ] **Step 5: Commit** `feat(agent): route instance addresses and rewrite the model for the child`.

## Phase 1 — Gateway

### Task 6: Schema — spec, mapping and affinity columns (migrations 100–102)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (register 100, 101, 102), `gateway/backend/internal/store/sqlite_runtime.go`, `gateway/backend/internal/store/sqlite_applications.go` (mapping cols + `UpdateMappingAudioMetrics`), affinity store file (grep `route_affinity` in `internal/store`), `gateway/backend/internal/routing/store.go` (`RuntimeSpec`, `ModelMapping`, `RouteAffinity`, store interfaces), `gateway/backend/internal/routing/memory_store.go`, `gateway/backend/internal/tracing/routingstore_gen.go`
- Test: create `gateway/backend/internal/store/migration100_instances_test.go`; extend `store/conformance_test.go`, `store/routing_store_conformance_test.go`, `store/runtime_spec_column_parity_test.go`

**Interfaces:**
- Produces:
  - migration 100 `agent_runtime_specs`: `replicable integer not null default 0` (same bool convention as `pinned`), `min_instances integer not null default 0`, `max_instances integer not null default 0`, `instance_parallelism integer not null default 0`, `startup_measured_ms bigint not null default 0`
  - migration 101 `model_mappings`: `audio_rate_x double precision`, `audio_ttfa_ms bigint`, `audio_realtime_concurrency integer not null default 0`, `audio_concurrency_limit text not null default ''`, `audio_metrics_updated_at` (timestamp type the table already uses for `metrics_updated_at`)
  - migration 102 `route_affinity`: `instance integer not null default 0`
  - `routing.RuntimeSpec{Replicable bool; MinInstances, MaxInstances, InstanceParallelism int; StartupMeasuredMS int64}`
  - `routing.ModelMapping{AudioRateX float64; AudioTTFAMS int64; AudioRealtimeConcurrency int; AudioConcurrencyLimit string; AudioMetricsUpdatedAt *time.Time}`
  - `routing.RouteAffinity.Instance int`
  - `type AudioMetrics struct{ RateX float64; TTFAMS int64; RealtimeConcurrency int; ConcurrencyLimit string; LoadTimeMS int64 }`
  - store methods: `UpdateRuntimeSpecStartupMeasured(ctx, specID string, ms int64) error`; `UpdateMappingAudioMetrics(ctx, mappingID string, m AudioMetrics, source string) error` (guarded `metrics_locked = 0`, writes only positive/non-empty values, stamps `metrics_source`/`metrics_updated_at`/`audio_metrics_updated_at`; `LoadTimeMS>0` also writes `load_time_ms`)
- `UpsertRuntimeSpec` keeps the stored `StartupMeasuredMS` (agent-owned, like `RAMMeasuredMB`).

- [ ] **Step 1: Failing tests**: `TestMigration100AddsInstanceColumns`, `TestMigration101AddsAudioMetricColumns`, `TestMigration102AddsAffinityInstance` (column presence + defaults on a fresh and an upgraded DB); conformance: `TestRoutingStoreRuntimeSpecInstances` (round trip; upsert keeps `StartupMeasuredMS`), `TestRoutingStoreMappingAudioMetrics` (write; locked mapping unchanged; zero values do not overwrite), `TestRoutingStoreAffinityInstance` (round trip); parity test lists the five new spec columns.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ ./internal/routing/` → FAIL.
- [ ] **Step 3: Implement** across memory + SQL stores and the tracing wrapper.
- [ ] **Step 4: Run** with Postgres: start a disposable PG (as CI does) and `OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./internal/store/ ./internal/routing/` → PASS (no skipped postgres subtests).
- [ ] **Step 5: Commit** `feat(store): instance, audio-metric and affinity-instance columns (migrations 100-102)`.

### Task 7: Portal runtime-spec API + pushed document + effective idle timeout

**Files:**
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`RuntimeSpecDTO`, `PutRuntimeSpecRequest`, `validateRuntimeSpecRequest`/`validateRuntimeSpecShape`, `runtimeSpecRow`, `runtimeSpecDTO`, `AgentRuntimeSpecDTO`, `agentRuntimeSpecDTO`), `gateway/backend/internal/portal/service_runtime_benchmark.go` (`putRequestFromDTO`), `gateway/backend/internal/config/config.go` (+ the portal service deps that carry config)
- Test: `gateway/backend/internal/portal/service_runtime_test.go`, `service_runtime_benchmark_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: Task 6 fields.
- Produces: DTO/request JSON `replicable`, `min_instances`, `max_instances`, `instance_parallelism`, read-only `startup_measured_ms`; errors `ErrRuntimeSpecInstancesInvalid` = `runtime_spec.instances_invalid`, `ErrRuntimeSpecInstancesNeedEphemeralPort` = `runtime_spec.instances_need_ephemeral_port`; pushed `AgentRuntimeSpecDTO.Replicable/MinInstances/MaxInstances` (`omitempty`); `func effectiveIdleTimeoutSeconds(spec routing.RuntimeSpec, loadTimeMS int64, factor float64, floorSec int) int`; config `InstanceIdleFactor float64`, `InstanceIdleFloorSeconds int`, `InstanceScaleCooldownSeconds int`, `AudioRealtimeMinFactor float64`, `AudioConcurrencyMax int` with the env names/defaults from Global Constraints.

- [ ] **Step 1: Failing tests**:
  - `TestPutRuntimeSpecInstancesRoundTrip` — PUT with the four fields → GET echoes them.
  - `TestPutRuntimeSpecInstancesInvalid` — `min 3, max 2` → 400 `runtime_spec.instances_invalid`; negative min → `runtime_spec.tuning_invalid`.
  - `TestPutRuntimeSpecReplicableNeedsEphemeralPort` — `replicable, listen_port 9000` → 400 `runtime_spec.instances_need_ephemeral_port`.
  - `TestBenchmarkWritePreservesInstanceFields` — `putRequestFromDTO` round trip keeps all four.
  - `TestAgentRuntimeDocCarriesInstanceFields` — document spec has `replicable/min_instances/max_instances`; non-replicable spec omits them (JSON has no such keys).
  - `TestEffectiveIdleTimeout` — replicable, manual 0, startup 30000 ms, load_time 0 → 150; manual 600 → 600; startup 10000 → 120 (floor); startup 20000 & load_time 40000 → 200; non-replicable manual 0 → 0 (unchanged meaning).
  - `TestConfigInstanceEnvDefaults` — defaults 5 / 120 / 60 / 1.2 / 8 and env overrides.
- [ ] **Step 2: Run** `go test ./internal/portal/ ./internal/config/ -run '...'` → FAIL.
- [ ] **Step 3: Implement**; validation order: append the two new checks after the existing shape checks (the existing order is API).
- [ ] **Step 4: Run** `go test ./internal/portal/ ./internal/config/` → PASS.
- [ ] **Step 5: Commit** `feat(portal): runtime-spec instance settings, pushed instance fields, effective idle timeout`.

### Task 8: Ingest — instance rows, folding, startup write-back

**Files:**
- Modify: `gateway/backend/internal/gateway/runtime_registry.go` (`RuntimeStatusDTO.Instance int \`json:"instance"\``, `StartupMS int \`json:"startup_ms,omitempty"\``), `gateway/backend/internal/gateway/agent_ingest.go` (sample decode, VRAM/RAM fold, startup write, context/capability from instance 0), `gateway/backend/internal/gateway/portal_model_endpoints.go` (`injectRowRuntimeState`, `injectLoadingOnCounts` multi-row), `gateway/backend/internal/gateway/benchmark_runner.go` (`benchmarkOthersStopped`), `gateway/backend/internal/gateway/benchmark_vram_isolation.go`
- Test: `gateway/backend/internal/gateway/agent_ingest_instances_test.go` (create), extend the portal model endpoint and benchmark-isolation tests

**Interfaces:**
- Consumes: Tasks 5, 6.
- Produces: `func foldInstanceRows(rows []RuntimeStatusDTO) map[string]foldedSpec` with `foldedSpec{MaxGPU map[int]int; MaxRAM int; MaxStartupMS int; Primary *RuntimeStatusDTO}` (Primary = instance 0 row, else first running).

- [ ] **Step 1: Failing tests**:
  - `TestIngestFoldsInstanceVRAMToMax` — rows S/0 gpu0=3000, S/1 gpu0=3200 → one `UpdateRuntimeSpecGPUMeasured(S,0,3200)`; next identical sample writes nothing.
  - `TestIngestFoldsInstanceRAMToMax`.
  - `TestIngestStartupWrittenOnTenPercentChange` — stored 10000; sample 10500 → no write; 11500 → write 11500.
  - `TestIngestContextFromInstanceZero`.
  - `TestModelServersRowCountsRunningInstances` — mapping row exposes running instance count 3 (new DTO field `running_instances`).
  - `TestLoadingCountCountsServerOnce` — two starting instance rows on one server → count 1.
  - `TestBenchmarkOthersStoppedSeesInstances` — target S, row `S` instance 1 running → not isolated.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./internal/gateway/ -run 'Ingest|ModelServers|LoadingCount|BenchmarkOthers'` then the whole package → PASS.
- [ ] **Step 5: Commit** `feat(gateway): ingest per-instance runtime rows (fold measurements, startup write-back)`.

### Task 9: Routing — instance candidates

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`MappingCandidate`, `Target`, `targetFrom`, `resolveStandard` loop, `modelLoadedOn`, swap protection, runtime-state overlay), `gateway/backend/internal/routing/store.go` (candidate struct lives there)
- Create: `gateway/backend/internal/routing/instances.go` (expansion), `gateway/backend/internal/gateway/instance_source.go` (adapter)
- Modify: `gateway/backend/internal/gateway/inference_complete.go` (usage `ProviderModel` base), `gateway/backend/internal/gateway/server.go` (wire the source)
- Test: create `gateway/backend/internal/routing/instances_test.go`, `gateway/backend/internal/gateway/instance_source_test.go`

**Interfaces:**
- Produces (package `routing`):
  - `MappingCandidate.Instance int`; `Target.InstanceKey string`; `func InstanceKey(mappingID string, k int) string` → `mappingID+"#"+itoa(k)`; `func InstanceAddress(base string, k int) string` (same rule as the agent's).
  - `type InstanceView struct{ Replicable bool; Parallelism int; MaxInstances int; Running []int /* k>=1, state running */; Present []int /* k>=1, starting|running */ }`
  - `type InstanceSource interface{ InstanceView(serverID string, m ModelMapping) (InstanceView, bool) }` — ok=false ⇒ not instance-capable (non-replicable spec, no spec, or agent lacks `runtime_instances`). `Parallelism` = `InstanceParallelism` or 1.
  - `func (r *Resolver) SetInstanceSource(s InstanceSource)`
  - `func expandInstances(cands []MappingCandidate, src InstanceSource) []MappingCandidate` — for ok views: instance 0 + every `Running` k; else the candidate unchanged.
  - `targetFrom`: `ProviderModel = InstanceAddress(mapping.AppModelName, c.Instance)`, `InstanceKey = InstanceKey(mapping.ID, c.Instance)`; `RouteID` unchanged.
- Adapter (package `gateway`): `type instanceSource struct{ store; features *AgentFeatures; registry *runtimeStatusRegistry }`; reads the spec by mapping (cache allowed, ≤ 5 s), checks `features.Has(serverID, "runtime_instances")`, collects instance rows of that spec from the registry.

- [ ] **Step 1: Failing tests**:
  - `TestExpandInstancesAddsRunningInstances` — view `Running [2,3]` → candidates instances 0,2,3 with `ProviderModel` `m`,`m#2`,`m#3`, same `RouteID`.
  - `TestExpandInstancesNoViewUnchanged` — ok=false → one candidate, instance 0 (mixed fleet, Review Focus 5).
  - `TestResolveReExpandsEachAdmissionTurn` — first turn view has no running instance and all capped (Task 10 uses this); second turn view adds `m#1` → request resolves to `m#1`. (Write now with a fake admission controller that returns nil once.)
  - `TestModelLoadedOnUsesInstanceAddress` — loaded set `{m#2}` → candidate instance 2 loaded, instance 0 not.
  - `TestRuntimeStateOverlayPerInstance` — checker answers per `m#2`.
  - `TestInstanceSourceDropsVanishedRows` — registry snapshot without `S#2` → `Running` excludes 2 (Review Focus 4).
  - `TestInstanceSourceRequiresFeature` — agent without `runtime_instances` → ok=false.
  - `TestUsageRecordsBaseProviderModel` — usage for target `m#2` stores `m`.
- [ ] **Step 2: Run** `go test ./internal/routing/ ./internal/gateway/ -run '...'` → FAIL.
- [ ] **Step 3: Implement**; move candidate computation inside the admission loop of `resolveStandard` (group path already re-reads).
- [ ] **Step 4: Run** `go test ./internal/routing/ ./internal/gateway/` → PASS.
- [ ] **Step 5: Commit** `feat(routing): running instances are routing candidates`.

### Task 10: Per-instance capacity, queue engagement, demand signal

**Files:**
- Modify: `gateway/backend/internal/gateway/active_requests.go` (`ActiveRequest.InstanceKey`, per-instance counter), `gateway/backend/internal/routing/resolver.go` (capacity filter, demand signal), inference dispatch sites that build `ActiveRequest` (set `InstanceKey` from `Target`)
- Test: `gateway/backend/internal/routing/instances_capacity_test.go` (create), `gateway/backend/internal/gateway/active_requests_test.go`

**Interfaces:**
- Produces:
  - `routing.InstanceActivityChecker interface{ InstanceActivity(instanceKey string) int }`; `(*activeRegistry).InstanceActivity`; `func (r *Resolver) SetInstanceActivityChecker(c InstanceActivityChecker)`
  - `routing.DemandSignal struct{ Model, APIFlavor, ServerID string /* "" = any */; Live bool }`; `routing.DemandSink interface{ Signal(DemandSignal) }`; `func (r *Resolver) SetDemandSink(s DemandSink)` (nil = no-op)
  - capacity rule for instance candidates: under cap iff `InstanceActivity(key) < view.Parallelism`; if every candidate is at cap and ≥1 is an instance candidate → `Signal(...)` then `errAllAtCapacity` (queue engages even when the mapping's `MaxConcurrency` is 0). Non-instance candidates keep `ServerActivity` vs `MaxConcurrency`.

- [ ] **Step 1: Failing tests**:
  - `TestInstanceCapUsesInstanceActivity` — instance 0 busy (activity 1, parallelism 1), instance 2 free → picks `m#2`.
  - `TestAllInstancesBusyQueuesAndSignals` — both busy, `MaxConcurrency 0` → admission controller called, sink got one `DemandSignal{Model:...}`.
  - `TestNonInstanceCandidateCapUnchanged` — legacy mapping with `MaxConcurrency 0` still fails open (no queue).
  - `TestActiveRegistryCountsPerInstance` — add two requests on key `x#0`, one on `x#1` → 2 and 1; removal decrements and wakes the server's queue.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./internal/routing/ ./internal/gateway/` → PASS.
- [ ] **Step 5: Commit** `feat(routing): per-instance capacity; all-busy queues and signals demand`.

### Task 11: Live slots per instance + failover + instance-error re-select

**Files:**
- Modify: `gateway/backend/internal/gateway/live_session_registry.go` (key = instance key), `live_transcription_bearer.go`, `transcription_handler.go` (held check), `gateway/backend/internal/routing/resolver.go` (exclusion), `gateway/backend/internal/inference/types.go` (request fields), upstream-error handling where a target's request fails (re-select once)
- Test: create `gateway/backend/internal/gateway/live_instances_test.go`

**Interfaces:**
- Produces: `inference.Request.LiveSession bool \`json:"-"\`` (resolver excludes candidates whose live slot is held), `inference.Request.ExcludeInstanceKeys []string \`json:"-"\``; `routing.LiveSlotChecker interface{ LiveHeld(instanceKey string) bool }`, `(*Resolver).SetLiveSlotChecker`; `func isInstanceGoneError(err error) bool` (404 `runtime.model_not_managed`, 409 `runtime.instance_no_capacity`/`instance_demand_unknown`, only for targets with instance k≥1).
- Behaviour: acquire collision → re-select with that key excluded (bounded by candidate count); replicable model with all live slots held → park + `DemandSignal{Live:true}`; non-replicable keeps immediate `503 transcription.live_busy`. Remove the "INTERIM until #340" comments.

- [ ] **Step 1: Failing tests**:
  - `TestTwoLiveSessionsUseTwoInstances` — instances 0 and 1 running → second live session lands on `m#1`, no 503.
  - `TestLiveCollisionReselects` — slot of the selected instance taken between select and acquire (inject) → succeeds on the other instance.
  - `TestLiveAllHeldReplicableParks` — both held → parks; releasing one admits it.
  - `TestLiveNonReplicableStillBusy` — single non-replicable mapping held → 503 `transcription.live_busy`.
  - `TestVanishedInstanceReselectedOnce` — upstream for `m#2` answers 404 `runtime.model_not_managed` → request retried on instance 0 and succeeds (Review Focus 3).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./internal/gateway/ ./internal/routing/` → PASS.
- [ ] **Step 5: Commit** `feat(gateway): live slot per instance, live failover, re-select on vanished instance`.

### Task 12: Portal UI — instance settings and per-instance status

**Files:**
- Modify: `gateway/frontend/src/api/runtime.ts` (`RuntimeSpec`, `PutRuntimeSpecRequest`, `RuntimeStatus.instance`, `startup_ms`), `gateway/frontend/src/components/RuntimeAdminSection.tsx` (form state, `emptySpec`, `buildSpecBody`, status table row key/column, spec-list aggregation), `gateway/frontend/src/components/ModelServersSection.tsx` + `api/models.ts` (`running_instances`), `gateway/frontend/src/components/shared/format.ts` (error mapping), `gateway/frontend/src/i18n.ts`
- Test: `RuntimeAdminSection.test.tsx`, `api/runtime.test.ts`, `ModelServersSection.test.tsx`, `i18n.test.ts`; fixtures in `ApplicationSection.test.tsx`, `ServerList.test.tsx`

**Interfaces:**
- Consumes: Task 7 JSON, Task 8 DTO fields.
- Produces: i18n keys `runtimeSpecReplicable`, `runtimeSpecReplicableHint`, `runtimeSpecMinInstances`, `runtimeSpecMaxInstances`, `runtimeSpecMaxInstancesUnlimited`, `runtimeSpecInstanceParallelism`, `runtimeSpecAutoIdleTimeout` (function `(seconds: number, startupMs: number) => string`), `runtimeStatusInstance`, `runtimeInstancesSummary` (function `(running: number, min: number, max: number | null) => string`), `errorRuntimeSpecInstancesInvalid`, `errorRuntimeSpecInstancesNeedEphemeralPort`, `modelServersRunningInstances`.

- [ ] **Step 1: Failing tests**: checkbox "Multiple instances" reveals min/max/parallelism; empty max sends `max_instances: 0`; body contains the four fields; status table renders two rows for one spec with instances 0 and 1 (row key `spec_id#instance`); spec list shows `runtimeInstancesSummary(2,1,null)`; backend errors map to the two new messages; i18n parity test for the new keys.
- [ ] **Step 2: Run** `cd gateway/frontend && npm test -- RuntimeAdminSection ModelServersSection runtime i18n` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test`, `npm run build`, `npm run lint`, `npm run format:check` → PASS.
- [ ] **Step 5: Commit** `feat(portal): instance settings and per-instance runtime status`.

## Phase 2 — Scaling

### Task 13: The instance scaler

**Files:**
- Create: `gateway/backend/internal/gateway/instance_scaler.go`
- Modify: `gateway/backend/internal/gateway/server.go` (construct, `Resolver.SetDemandSink`), `gateway/backend/internal/gateway/admission_queue.go` (exported wake used by the scaler if `release` is not reachable)
- Test: create `gateway/backend/internal/gateway/instance_scaler_test.go`

**Interfaces:**
- Consumes: Task 9 `InstanceSource`/`InstanceView`, Task 10 `DemandSignal`, `provider.RuntimeEnsurer` (`EnsureRuntimeModel`), routing scorer, `ServerBusy`, reachability, mapping audio metrics (Task 6).
- Produces: `type instanceScaler struct{...}`; `func newInstanceScaler(deps instanceScalerDeps, cooldown time.Duration) *instanceScaler`; `func (s *instanceScaler) Signal(d routing.DemandSignal)` (non-blocking; single-flight per model); internal `func (s *instanceScaler) pickHost(ctx, d) []scaleCandidate` and `func nextInstanceIndex(present []int, inFlight map[int]bool) int`.
- Rules (spec §7.1–§7.2): candidates = mappings of `d.Model` (restricted to `d.ServerID` if set) with ok view; with `total = 1+len(Present)` (instance 0 counts): skip if `MaxInstances>0 && total >= MaxInstances`, if `AudioConcurrencyLimit=="performance" && total >= AudioRealtimeConcurrency`, if any instance is `starting`, if benchmark-busy/unreachable/unselectable, if in cooldown; order by routing `Score()` desc; ensure `m#k` with timeout = spec startup timeout + 10 s; success → wake queue for that server; `instance_no_capacity` → cooldown (config, default 60 s) + next host; `instance_demand_unknown` → 5 s delay + next host; other error → cooldown + Warn.

- [ ] **Step 1: Failing tests** (fake ensurer/clock):
  - `TestScalerSingleFlightPerModel` — 10 concurrent signals → 1 ensure.
  - `TestScalerPicksLowestFreeIndex` — present `[1,3]` → ensures `m#2`.
  - `TestScalerRespectsMax` — max 3, present `[1,2]` → no ensure.
  - `TestScalerRespectsPerformanceCeilingOnly` — concurrency 2 limit `performance`, present `[1]` → none; same with limit `resources` → ensures `m#2`.
  - `TestScalerSkipsStartingSibling` — a `starting` row → no ensure.
  - `TestScalerFallsBackToNextHostOnNoCapacity` — host A 409 no_capacity → host B ensured; A in cooldown for 60 s (second signal within 60 s skips A).
  - `TestScalerDemandUnknownShortDelay` — 409 demand_unknown → retried on next signal after 5 s, not before.
  - `TestScalerRestrictsToPinnedServer` — `ServerID` set → only that server.
  - `TestScalerSkipsBenchmarkBusyAndOldAgents` — busy server / no feature → never ensured (Review Focus 5).
  - `TestScalerWakesQueueOnSuccess`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestScaler` → FAIL.
- [ ] **Step 3: Implement**; wire in `server.go`.
- [ ] **Step 4: Run** `go test -race ./internal/gateway/ -run TestScaler` and the package → PASS.
- [ ] **Step 5: Commit** `feat(gateway): demand-driven instance scaler`.

### Task 14: Voice gate select-without-parking + one TTS instance per chat

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` (`NoPark bool \`json:"-"\``), `gateway/backend/internal/routing/resolver.go` (NoPark path), `gateway/backend/internal/gateway/voice_chat.go` (`voiceCoLoadGate`, `resolveVoiceASR`, `ensureVoiceLeg`), `gateway/backend/internal/gateway/live_transcription.go` (ticket mint), `gateway/backend/internal/gateway/chat_runs_voice.go` (synth gate limit)
- Test: `gateway/backend/internal/gateway/voice_chat_instances_test.go` (create)

**Interfaces:**
- Produces: `inference.Request.NoPark` — resolver never calls `WaitForSlot`; when nothing is under cap it picks the least-loaded instance candidate and emits a `DemandSignal`; `func voiceTTSConcurrency(target routing.Target, configured int) int` → 1 when `target.InstanceKey != ""` and the target's spec is replicable, else `configured`.

- [ ] **Step 1: Failing tests**: `TestVoiceGateNeverParks` (all instances busy → gate returns within 100 ms with a target + one demand signal); `TestVoiceGateEnsuresSelectedInstance` (ensure called with `m#1`); `TestVoiceGateReselectsVanishedInstance` (409 on `m#1` → re-select once); `TestLiveTicketMintNeverParks`; `TestVoiceTTSConcurrencyOneForInstances` (replicable instance target → 1; non-replicable → configured 2).
- [ ] **Step 2–4:** Run → FAIL; implement; `go test ./internal/gateway/ ./internal/routing/` → PASS.
- [ ] **Step 5: Commit** `feat(voice): gate selects without parking; one TTS instance per voice chat`.

## Phase 3 — Audio benchmark and routing use

### Task 15: Bundled ASR clip + audio measurement primitives

**Files:**
- Create: `gateway/backend/internal/benchaudio/benchaudio.go`, `gateway/backend/internal/benchaudio/asr-benchmark-de.wav`, `asr-benchmark-de.txt`, `SOURCE.md` (copy the three files from the session scratchpad `clip/` directory: `/private/tmp/claude-501/-Users-jlor08-Developer-codex-op-ai-gateway--claude-worktrees-autoscaling-340-1469d8/0e6fde61-db0d-4662-bbfe-f143999b1929/scratchpad/clip/`)
- Create: `gateway/backend/internal/gateway/benchmark_audio_measure.go`
- Modify: `docs/architecture/cross-cutting/licensing.md` (bundled data asset: Thorsten-Voice, CC0 1.0)
- Test: `gateway/backend/internal/benchaudio/benchaudio_test.go`, `gateway/backend/internal/gateway/benchmark_audio_measure_test.go`

**Interfaces:**
- Produces (package `benchaudio`): `func WAV() []byte`; `func Transcript() string`; `func PCM() (pcm []byte, sampleRate int, channels int)` (s16le, 16000, 1); `const Language = "de"`.
- Produces (package `gateway`): `type audioMeasurement struct{ TTFTMS int64 /* TTFA for TTS, first text for ASR */; WallMS int64; AudioSeconds float64; RateX float64; RTF float64 }`;
  `func measureTTSStream(ctx, client provider.NativeProxyClient, target routing.Target, text string, byteRate int) (audioMeasurement, error)` — `RateX = audio_s / (t_done − t_first_delta)`, `RTF = (t_done − t_send)/audio_s`;
  `func measureTTSBuffered(...)` — `RateX = audio_s / wall`, `TTFTMS = wall`;
  `func measureASRBuffered(ctx, client, target, wav []byte, lang string) (audioMeasurement, error)` — `RateX = audio_s / wall`;
  `func measureASRLive(ctx, client, target, pcm []byte, sampleRate int, lang string) (audioMeasurement, error)` — unpaced; `TTFTMS = first transcript.text.delta − send`, `RateX = audio_s/(done − send)`;
  `const audioBenchmarkPassage = "<English passage, ≈20 s spoken>"` (builder writes the text, ~55 words).

- [ ] **Step 1: Failing tests**: `TestBenchaudioClipIsPCM16kMono` (header: PCM, 1 ch, 16000 Hz, 16-bit; duration 22.85 s ± 0.05; transcript starts `"der schutz der menschen"`); `TestMeasureTTSStreamMath` (fake SSE: first delta at 300 ms, done at 2300 ms, PCM bytes = 10 s at byte rate 48000 → TTFT 300, RateX 5.0, RTF 0.23 ± 0.01); `TestMeasureTTSBufferedMath`; `TestMeasureASRBufferedMath` (22.85 s audio, 1000 ms wall → RateX 22.85); `TestMeasureASRLiveMath` (first delta 400 ms, done 2000 ms → TTFT 400, RateX ≈ 11.4).
- [ ] **Step 2–4:** Run `go test ./internal/benchaudio/ ./internal/gateway/ -run 'Benchaudio|TestMeasure'` → FAIL; implement; → PASS.
- [ ] **Step 5: Commit** `feat(benchmark): bundled CC0 ASR clip and audio measurement primitives`.

### Task 16: Audio benchmark orchestration (speed / capacity / both, cold start, scheduler, auto-run)

**Files:**
- Create: `gateway/backend/internal/gateway/benchmark_audio.go`
- Modify: `gateway/backend/internal/gateway/benchmark_runner.go` (dispatch per mapping kind in `runBenchmark`), `benchmark_stop.go` (`benchmarkMayPreStop` marks audio targets for speed/both), `benchmark_scheduler.go` (audio mappings take part in scheduled `speed`), `benchmark_speech_trigger.go` + `cmd/gateway/app_health.go` (one-time automatic `both` run), `benchmark.go` (`BenchmarkResult` audio fields for SSE)
- Test: create `gateway/backend/internal/gateway/benchmark_audio_test.go`; extend `benchmark_scheduler_test.go`

**Interfaces:**
- Consumes: Task 15 primitives as built — `measureTTSStream(ctx, client, target, text, voice string, byteRate int)`, `measureTTSBuffered(ctx, client, target, text, voice string)` (voice = the clone preset or ""), `measureASRBuffered(ctx, client, target, wav, lang)`, `measureASRLive(ctx, duplex provider.NativeDuplexProxyClient, target, pcm, sampleRate, lang)`, typed measurement errors and a first-byte field for buffered TTS (Task 15 fold-in); callers pass a ctx with a deadline wrapped by s.upstreamAuthCtx — Task 6 `UpdateMappingAudioMetrics(ctx, mappingID, m routing.AudioMetrics, source string, at time.Time)` (the ramp's count and limit are one pair: written together only when `ConcurrencyLimit != ""`; a measured 0 with limit `performance` means not real-time even alone; 0 with `resources` means nothing could be measured; use the exported limit constants from package routing), Task 4/5 agent instance ensure, existing `ensureColdLoad`/lease machinery.
- Produces: `func (s *Server) measureAudioTarget(ctx, run *benchmarkRun, view mappingView, mode string) (BenchmarkResult, error)`; `type audioCapacityResult struct{ Concurrency int; Limit string /* "performance"|"resources"|"cap" */; Curve []audioLevel }`; `type audioLevel struct{ K int; MinRateX, MeanRateX float64 }`; history `kind="audio"` (speed) and `kind="audio_capacity"` (ramp, curve JSON in `capacity_curve`); `BenchmarkResult.AudioRateX`, `AudioTTFTMS`, `AudioRealtimeConcurrency`, `AudioConcurrencyLimit`.
- Rules: speed = warm-up, single measurement (TTS stream if `speech_streaming`, else buffered; ASR live if `transcription_streaming`, else buffered; a mapping carrying both capabilities is measured for the one its derived role names — role `tts` → TTS, `asr` → ASR); cold start = cold pass then warm pass, `LoadTimeMS = cold − warm` only if cold confirmed and cold > warm; capacity ramp k=1…: replicable+feature → ensure `m#0…m#(k−1)` and one stream per instance, else k streams on the one target; good iff slowest `RateX >= cfg.AudioRealtimeMinFactor`; stop on first bad (limit `performance`), refused ensure (`resources`), `MaxInstances` or `cfg.AudioConcurrencyMax` (`cap`); TTS byte rate from cache, else run the existing non-streamed speech measure first.

- [ ] **Step 1: Failing tests** (fake upstreams):
  - `TestAudioSpeedRunStoresMappingMetrics` — speed on a TTS mapping → `UpdateMappingAudioMetrics` with RateX/TTFA and a `kind=audio` history row.
  - `TestAudioSpeedRunMeasuresColdStart` — agent target not resident → cold 5300 ms, warm 300 ms → `LoadTimeMS 5000` stored.
  - `TestAudioCapacityStopsOnPerformance` — rates per level k=1: 3.0, k=2: 1.5, k=3: 1.1 → Concurrency 2, limit `performance`.
  - `TestAudioCapacityStopsOnResources` — ensure of `m#2` → 409 → Concurrency 2, limit `resources`.
  - `TestAudioCapacityCap` — always fast, `AudioConcurrencyMax 3` → 3, `cap`.
  - `TestAudioCapacityNonReplicableSingleProcess` — k streams to one target.
  - `TestAudioRunMetricsLockedUntouched`.
  - `TestServerBothRunCoversTextAndAudio` — server run with a chat and a TTS mapping → both measured with their kinds.
  - `TestSchedulerIncludesAudioSpeed`.
  - `TestAutoAudioRunOnceWhenMissing` — idle server, mapping without audio metrics (no `audio_metrics_updated_at`), or a replicable spec whose `audio_concurrency_limit` is `""` → one `both` run; second health tick → none (a measured 0 with a non-empty limit never re-triggers).
- [ ] **Step 2–4:** Run → FAIL; implement; `go test ./internal/gateway/` → PASS.
- [ ] **Step 5: Commit** `feat(benchmark): audio speed/capacity/both with cold start, stored on the mapping`.

### Task 17: Routing use — audio score term, voice real-time gate, editable audio metrics

**Files:**
- Modify: `gateway/backend/internal/routing/scorer.go` (`Route.AudioRateX`, `Route.Audio bool`, `metricTiebreak`), `gateway/backend/internal/routing/resolver.go` (realtime filter), `gateway/backend/internal/inference/types.go` (`RealtimeRequired bool \`json:"-"\``), `gateway/backend/internal/gateway/voice_chat.go` + voice per-turn loopback requests (`voice_chat_ws.go`, `chat_runs_audio.go`) (mark legs), `gateway/backend/internal/portal/service_applications.go` (mapping editable audio metrics), `gateway/backend/internal/routing/score_servers.go` / `portal/service_model_servers.go` (ranking DTO audio fields)
- Test: `routing/scorer_test.go`, create `gateway/backend/internal/gateway/voice_realtime_gate_test.go`, `portal/service_applications_test.go`

**Interfaces:**
- Produces: `func audioTiebreak(rateX float64) float64 { return clampBonus(rateX, 10, 50) }` added for routes with `Audio` (request carries speech or transcription capability); resolver filter: when `req.RealtimeRequired`, drop candidates with `0 < AudioRateX < minFactor` (from a `SetAudioRealtimeMinFactor(float64)` setter); error `ErrNoRealtimeCandidate` → voice gate maps to `voice_chat.no_realtime_model` naming the leg; per-turn voice legs set `RealtimeRequired` via the loopback request (header `X-OP-Realtime-Required: 1` honoured only on the internal loopback pair); mapping update request accepts `audio_rate_x`, `audio_ttfa_ms`, `audio_realtime_concurrency`, `audio_concurrency_limit` like the text metrics (same `metrics_locked` semantics; add the audio columns to `MappingMetricsMask`/`UpdateMappingEditable`; count and limit are validated and saved as one pair — a count needs a limit from the exported constants, limit `""` clears both); portal warning field `instances_min_exceeds_realtime bool` on `RuntimeSpecDTO` when the limit is `performance` and `min_instances > audio_realtime_concurrency` (a measured 0 counts).

- [ ] **Step 1: Failing tests**: `TestScoreAudioTiebreak` (rate 3.0 → +30; 9.0 → +50 cap; chat route unaffected); `TestRealtimeGateDropsSlowCandidates` (rates 0.9 and 1.5, min 1.2 → only 1.5; unmeasured 0 kept); `TestVoiceGateNoRealtimeModel` (all measured below → code `voice_chat.no_realtime_model`); `TestVoicePerTurnLegCarriesRealtime`; `TestMappingAudioMetricsEditable` (PUT edits + lock respected by a later benchmark write); `TestRuntimeSpecMinExceedsRealtimeWarning`.
- [ ] **Step 2–4:** Run → FAIL; implement; `go test ./internal/routing/ ./internal/gateway/ ./internal/portal/` → PASS.
- [ ] **Step 5: Commit** `feat(routing): audio score term and voice real-time gate`.

### Task 18: Portal UI — audio benchmark results and metrics

**Files:**
- Modify: `gateway/frontend/src/components/BenchmarkSection.tsx` (audio result line + history for `audio` / `audio_capacity` kinds), `gateway/frontend/src/api/servers.ts` (result fields), `gateway/frontend/src/components/ModelServersSection.tsx` + `GroupServersSection.tsx` (columns ×RT, TTFA/TTFT, RT concurrency with "≥ k" for non-performance limits), mapping editor component (grep `max_concurrency` in `src/components` for the text-metric fields) + `api/models.ts`, `RuntimeAdminSection.tsx` (min > concurrency warning), `i18n.ts`
- Test: matching `*.test.tsx`, `i18n.test.ts`

**Interfaces:**
- Produces i18n keys: `benchmarkAudioResult` (function `(rate: number, ttftMs: number) => string`), `benchmarkAudioCapacityResult` (function `(k: number, limit: string) => string`), `modelServersAudioRate`, `modelServersAudioTtft`, `modelServersAudioConcurrency`, `mappingAudioRateX`, `mappingAudioTtfaMs`, `mappingAudioRealtimeConcurrency`, `runtimeSpecMinExceedsRealtime`.

- [ ] **Step 1: Failing tests**: audio result line renders `3.0×` and TTFA; history shows `kind=audio` rows; model servers show `≥ 2` when limit `resources` and `2` when `performance`; mapping editor sends the audio fields; warning visible when flag set; i18n parity.
- [ ] **Step 2–4:** `npm test -- …` → FAIL; implement; `npm test && npm run build && npm run lint && npm run format:check` → PASS.
- [ ] **Step 5: Commit** `feat(portal): audio benchmark results and editable audio metrics`.

## Phase 4 — Affinity

### Task 19: Voice session instance stickiness

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` (`PreferredInstanceKey string \`json:"-"\``), `gateway/backend/internal/routing/resolver.go` (preference), voice session state + loopback builders (`voice_chat_ws.go`, `chat_runs_voice.go`, `chat_runs_audio.go`, `voice_prewarm.go`)
- Test: create `gateway/backend/internal/gateway/voice_sticky_test.go`, `routing/instances_preference_test.go`

**Interfaces:**
- Produces: resolver rule — if `PreferredInstanceKey` names a current candidate that is under cap and passes all filters (incl. realtime, live slot) it is selected; otherwise normal selection (never an error because of the preference). Voice session stores per leg (`asr`,`llm`,`tts`) the last `Target.InstanceKey`; first turn's TTS preference = the pre-warmed gate target. Loopback carries it as header `X-OP-Preferred-Instance` (internal loopback pair only).

- [ ] **Step 1: Failing tests**: `TestPreferredInstanceChosenWhenFree`; `TestPreferredInstanceIgnoredWhenBusy`; `TestPreferredInstanceIgnoredWhenGone`; `TestVoiceTurnsStickToInstance` (three turns → same ASR/TTS instance keys); `TestVoiceFirstTurnUsesPrewarmedTTS`; `TestPreferredHeaderIgnoredOutsideLoopback`.
- [ ] **Step 2–4:** Run → FAIL; implement; `go test ./internal/gateway/ ./internal/routing/` → PASS.
- [ ] **Step 5: Commit** `feat(voice): keep a voice chat's turns on the same instances`.

### Task 20: Pinned requests and instances (token affinity + server override)

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`resolveAffinity`, `resolveServerOverride`, affinity upserts write `Instance`)
- Test: create `gateway/backend/internal/routing/affinity_instances_test.go`

**Interfaces:**
- Consumes: Task 6 `RouteAffinity.Instance`, Tasks 9–11.
- Produces (spec §10.2): pinned path considers only the pinned server's instance candidates; prefer the pinned instance if candidate and under cap, else the least-loaded instance under cap on that server; none free → non-live: overflow onto the preferred instance (no queue); live: park restricted to that server's instance slots; both emit `DemandSignal{ServerID: pinned}`; pin never deleted for instance reasons; non-replicable unchanged.

- [ ] **Step 1: Failing tests**: `TestAffinityWritesInstance`; `TestAffinityReusesPinnedInstance`; `TestAffinityPinnedInstanceBusyUsesSibling`; `TestAffinityAllBusyOverflowsNonLive` (no `WaitForSlot` call, target = pinned instance, one restricted demand signal); `TestOverrideLiveAllBusyParks`; `TestAffinityPinnedInstanceVanishedFallsBack` (Review Focus 4); `TestAffinityNonReplicableUnchanged`.
- [ ] **Step 2–4:** Run → FAIL; implement; `go test ./internal/routing/ ./internal/gateway/` → PASS.
- [ ] **Step 5: Commit** `feat(routing): pinned requests pick instances on the pinned server`.

## Finish

### Task 21: Architecture docs, cleanup, full verification

**Files:**
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (§4.2 instance addressing/rewrite, §5 instance admission/burst/surplus/ordering, §6 lifecycle per instance + startup measurement, §7 feature `runtime_instances` 0.16.0, §10 status rows, §11 portal), `docs/architecture/cross-cutting/routing-and-model-selection.md` (instance candidates, per-instance capacity/queue, scaler, live slots/failover, voice gate no-park, real-time gate, audio score term, affinity/override), `docs/architecture/cross-cutting/telemetry-usage-observability.md` (audio benchmark modes, storage, scheduler, auto-run), `docs/architecture/09-architecture-decisions.md` (ADR-090 instance addressing `m#k`; ADR-091 burst/no-evict + highest-index-first; ADR-092 gateway-driven scaler with agent-decided fit; ADR-093 audio benchmarks mirror the text modes + real-time gate/ceiling; ADR-094 instance affinity and pinned overflow), `docs/architecture/11-risks-and-technical-debt.md` §11.4 (per-spec real-time concurrency ignores other models on one GPU; telemetry-lag window for new instances), `docs/architecture/reference/data-model.md`, `api-surface.md`, configuration/env reference, `docs/architecture/06-runtime-view.md` if it shows the request flow
- Delete: `docs/superpowers/` and `docs/implementation-status.md`

- [ ] **Step 1:** Write the docs (behaviour as built; cite file paths); `make lint-docs` → OK.
- [ ] **Step 2:** Full verification: both Go modules `golangci-lint fmt --diff` + `golangci-lint run ./...`; `cd server-agent && go test -race ./... && GOOS=windows go vet ./...`; `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./...`; frontend `npm test && npm run build && npm run lint && npm run format:check`; `make sonar-scan` then `make sonar-findings && make sonar-branch-findings` → 0 branch findings.
- [ ] **Step 3:** Remove `docs/superpowers/` + `docs/implementation-status.md`; `git diff --name-only main...HEAD | grep -E 'docs/superpowers|implementation-status'` → empty.
- [ ] **Step 4: Commit** `docs: fold #340 instance autoscaling into the architecture docs`.

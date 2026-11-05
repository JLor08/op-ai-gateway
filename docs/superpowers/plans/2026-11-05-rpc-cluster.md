# llama.cpp RPC Cluster Mode (Stage 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run one llama.cpp model across several AI servers: a `llama_cpp` head offloads to `ggml-rpc-server` members on other servers, ordered, held, accounted and secured by the gateway, over `direct` or `relay` transport on a `mesh` (NetBird) or `lan` network.

**Architecture:** The agent learns a member spec kind (`rpc_server`), a head cluster block, new placeholders, TCP readiness, member holds (relay sessions or an observed connection), a head start guard and exit-code classification. The gateway stores heads, manual member specs and membership (with per-member usage), derives member specs into member servers' documents, orchestrates members-first starts (`ensureCluster`), gates routing on cluster state, manages the `op-gw-rpc-*` NetBird policies, and splits energy into per-host parts. The portal edits all of it.

**Tech Stack:** Go 1.26 (server-agent, own module), Go 1.25 gateway backend (SQLite/Postgres/memory stores), React 19 + MUI + vitest portal, llama.cpp `2bc46ba` for CI.

**Spec:** `docs/superpowers/specs/2026-11-05-rpc-cluster-design.md` (owner-approved), which adopts GitHub issue #365 (body + first comment) as its baseline. Executors read the spec, the issue (`gh issue view 365 --repo JLor08/op-ai-gateway --comments`) and this plan. Spec wins over the issue; this plan's Rulings win over both where they say so.

## Global Constraints

- Never commit to `main`; all work on `claude/rpc-cluster-365-6070e9` (lanes may use sibling branches merged back with plain merges). One PR at the end.
- Repo-facing text (code comments, commits, docs, PR) in English; no LAN host names; no attribution lines; no comment may cite branch-local files, task numbers or "Ruling Pn" — state the reasoning inline.
- ServerAgent `Version` goes `0.17.0` → `0.18.0` exactly once (Task A1), feature `rpc_cluster` `Since: "0.18.0"` declared in Task A9. Update the `Version == "0.17.0"` pin in `TestFeaturesDeclareRuntimeStop`.
- Migrations are append-only, next free number 103; every new column covered by the parity tests (`runtime_spec_column_parity_test.go`, `ai_server_column_parity_test.go`, `schema_column_coverage_test.go`); all three stores (memory, SQLite, Postgres) behave identically; store changes are verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Portal i18n: German and English together in `gateway/frontend/src/i18n.ts`; every new error code mapped in `components/shared/format.ts` `errorLabelByCode`.
- Stable error codes once introduced; new portal save codes are appended last in the validation order (`service_runtime.go:1034-1090` comment).
- Pinned llama.cpp: `2bc46baa9207d098d03741ac394e7faaf1a955b6`.
- Gates per task: per Go module `golangci-lint fmt --diff` and `golangci-lint run`, `go test` (agent runtime with `-race`); frontend `npm test`, `npm run build`, `npm run lint`, `npx prettier --check .` (`format:check`).
- `docs/superpowers/` and `docs/implementation-status.md` are removed in the last task; durable content goes to `docs/architecture/` (ADR-096).

## Rulings (decisions this plan makes on top of the spec)

- **P1 — manual member specs live in their own table** (`rpc_member_specs`, `rpc_member_spec_gpus`), not as `agent_runtime_specs` rows with a nullable `mapping_id` (issue D4). Making `mapping_id` nullable needs a SQLite table rebuild, breaks the `on conflict(mapping_id)` upsert, every `RuntimeSpecsByApplication` join through `model_mappings`, and the ingest ownership chain spec→mapping→application→server. A sibling table keeps all of that untouched; both member modes then derive the same agent spec kind.
- **P2 — automatic members have no spec row at all**; their settings, GPU selection, port and usage live on the membership row (`rpc_cluster_members` + `rpc_cluster_member_gpus`). Manual members keep settings on the spec and usage on the membership row (usage is per head, and a manual spec serves one head).
- **P3 — agent spec ids:** manual member = its `rpc_member_specs.id` (`rpcm_<hex16>`); automatic member = `rpca_<head spec id>_<member server id>`. The agent treats both identically.
- **P4 — auto device backend on the agent uses the host's detected vendor set** (from `collector.DetectGPUCollectors`): exactly one vendor → that vendor's backend (nvidia→`cuda`, amd→`rocm`, apple→`metal`), several vendors → `vulkan`, none → hard error for a token that needs a GPU prefix. The agent cannot map a declared index to a vendor on a mixed host (each collector numbers from 0), so "the vendor of the spec's GPUs" (spec §3.4) is approximated by the host's vendor set; documented.
- **P5 — error codes travel on `LastError.Code`** (new wire field `code`, omitempty) for lifecycle failures (`runtime.device_backend_mismatch`, `runtime.rpc_device_count_mismatch`, `runtime.rpc_unsupported_build`, `runtime.cluster_version_mismatch`, `runtime.cluster_member_unreachable`); router refusals use sentinels mapped in `sentinelCode`.
- **P6 — the gateway declares a one-sided feature `rpc_cluster`** (appended to `gatewayAgentFeatures`); the agent honours `kind`, `cluster` and `rpc` only while the gateway declares it (fail-closed, the `runtime_instances` precedent), so a file-mode document or an old gateway can never start a member as a model.
- **P7 — the gateway "Default policy last seen" state** is a system setting `netbird_default_policy_state` (`enabled`/`disabled`/`unknown` + `at`) written by every policy pass from its `ListPolicies` snapshot.
- **P8 — members-first start uses dedicated member routes** on the agent router (`/rpc/...`, wire contract below) rather than overloading `/ensure/{model}`, because members have no upstream model.

## Wire contract (agent ⇄ gateway; both lanes build against exactly this)

Runtime document spec additions (`server-agent/internal/runtime/types.go` `Spec`; gateway `portal.AgentRuntimeSpecDTO`):

```json
{
  "kind": "rpc_server",                       // "" (omitted) = model spec
  "device_backend": "auto",                   // "", "auto", "cuda", "rocm", "vulkan", "metal", "sycl"; "" == "auto"
  "cluster": {                                // head specs only
    "endpoints": [{"address": "100.64.0.5:47001", "device_count": 2}],
    "transport": "direct",                    // "direct" | "relay"
    "network": "mesh"                         // "mesh" | "lan"
  },
  "rpc": {                                    // rpc_server specs only
    "head_spec_id": "rspec_ab12",
    "transport": "relay",
    "bind_ip": "100.64.0.5",                  // exact IPv4, or "0.0.0.0" (lan without bind-only)
    "port": 47001,                            // RPC port (direct) or relay port (relay)
    "head_ips": ["100.64.0.2"],               // may be empty when head_domain is set
    "head_domain": "",                        // lan without head lan_address: agent resolves
    "cache_enabled": false
  }
}
```

- `rpc_server` specs carry `binary`, `args` (operator args, e.g. `-t 8`; never `-H`/`-p`/`-d`/`-c`), `env`, `gpus` (empty = CPU member; `vram_mb` = pushed demand), `ram_mb`, `startup_timeout_seconds`, `idle_timeout_seconds` (default 60, D2), `device_backend`; no `upstream_model`, `listen_port`, `health_path`, `metrics_path`, `context_probe_path`, replicable fields.
- Head `${RPC_SERVERS}` = comma-joined `endpoints[].address`; `${RPC_DEVICES}` = `RPC0..RPC(Σ device_count − 1)`.

Agent router (`server-agent/internal/runtime/router.go`):

| Route | Meaning | Answers |
|---|---|---|
| `POST /ensure/{model}` with header `X-OP-Cluster-Start: 1` | only way a cold cluster head starts | 200 running; 503 `runtime.cluster_member_unreachable` after the pre-exec probe gives up |
| `POST /ensure/{model}` / proxied request to a cold head without the header | refused | 503 `runtime.cluster_not_ready` |
| `POST /rpc/ensure/{specID}?reserve_seconds=N` | start member, arm reservation for N s | 200 `{"state":"running"}` once TCP-ready (and relay listening); 404 `runtime.model_not_managed`; 503 `runtime.admission_blocked`; 502 `runtime.not_permitted` / `runtime.start_failed`; 504 `runtime.start_timeout` |
| `POST /rpc/release/{specID}` | end reservation and hold (head not running) | 200 |
| `POST /rpc/stop/{specID}` | stop member | 200 `stopping`/`stopped`; 409 `runtime.cluster_member_in_use` while held, unless `?force=1` |
| `POST /stop/{model}?if_evictable=1` | conditional head stop (R10) | 200 as `/stop`; 409 `runtime.instance_busy` when the agent's eviction rule says no |

Telemetry (`sample.RuntimeSample` additions; gateway `RuntimeStatusDTO` mirrors them):

```json
{
  "kind": "rpc_server",
  "rpc": {
    "held_by": "rspec_ab12", "held_since": "…", "reserved_until": "…",
    "protocol": "8.0.1", "device_count": 2, "bind_mismatch": false,
    "breaches": [{"peer": "100.64.0.9", "at": "…"}],
    "cache_mb": 0, "stalled": false
  },
  "cluster": {"launched": ["100.64.0.5:47001"], "version": "b1234"},
  "admission_blocked_by": "rpcm_x",          // on any waiting spec: the held member blocking it
  "last_error": {"code": "runtime.device_backend_mismatch", "message": "…"}
}
```

## Review Focus

1. **A member flaps or a telemetry frame is late during formation** → the head must not relaunch (endpoints derive from configured membership, never liveness) and the watchdog must not fire inside formation or boot grace. Pinned by `TestEnsureClusterIgnoresStaleTelemetryDuringFormation` (Task D1) and `TestAgentRuntimeConfigEndpointsIgnoreMemberLiveness` (Task C3).
2. **A second head or an unexpected peer reaches a member** (direct mode queues and later inherits it, measured in Stage 0) → reported as breach, never accepted as `held_by`; in relay refused at once. Pinned by `TestObserverForeignPeerIsBreachNotHolder` (A8), `TestRelayRefusesSecondHead` (A7), `TestIngestForeignHeldByIsBreach` (D4).
3. **Old agents** (no `rpc_cluster`) on head or member → no head spec, no member spec, portal says `runtime.cluster_agent_too_old`; an old agent never sees `${RPC_SERVERS}` literally. Pinned by `TestAgentRuntimeConfigWithholdsClusterForOldAgents` (C3) and `TestParseConfigDropsRPCKindWithoutGatewayFeature` (A1).
4. **Membership edits while the cluster runs** (reorder, GPU change, mode switch, network switch) → head stopped first, values carried/marked stale per D1, `RPC<n>` never shifted silently. Pinned by `TestPutClusterReorderStopsHeadAndMarksStale` (C2) and `TestMemberDeviceCountMismatchFailsStart` (A4).
5. **Member host clock skew / telemetry gaps for energy** → part drops to Tier 2 with provenance, never a silent measured value outside the window; pricing order irrelevant. Pinned by `TestReconcileMemberPartGapFallsToTier2` and `TestReconcileClusterEnergyOrderIndependent` (D5).

---

## Lanes

The agent lane (Tasks A1–A10) and the gateway backend lane (B1–D5) build against the wire contract above and touch disjoint modules; the portal lane (E1–E5) starts after C2 has fixed the portal DTOs. Lanes run in sibling worktrees (`.worktrees/rpc-365-agent`, `.worktrees/rpc-365-fe`) and are merged into `claude/rpc-cluster-365-6070e9` with plain merges; only the controller edits `docs/implementation-status.md`. F1–F3 run on the integrated branch.

---

## Phase A — server-agent (`server-agent/`, run `go test ./internal/... -race` from `server-agent/`)

### Task A1: Wire types, version bump, gateway-feature gating

**Files:**
- Modify: `internal/runtime/types.go` (Spec ~66-165, Status ~697, LastError ~623, ParseConfig ~394, sanitize ~503)
- Modify: `internal/runtime/manager.go` (`sameLaunchShape` ~1944-1998)
- Modify: `internal/runtime/driver.go` (`featureActive` ~483, `Sync` ~355, `withoutReplicable` ~440 precedent)
- Modify: `internal/agent/agent.go` (`const Version` ~395 → `"0.18.0"`, Version doc paragraph)
- Modify: `internal/agent/features_test.go` (`TestFeaturesDeclareRuntimeStop` version pin)
- Test: `internal/runtime/types_test.go`, `internal/runtime/driver_test.go`

**Interfaces:**
- Produces:
  ```go
  const SpecKindModel = ""; const SpecKindRPCServer = "rpc_server"
  type ClusterEndpoint struct { Address string `json:"address"`; DeviceCount int `json:"device_count"` }
  type ClusterHead struct { Endpoints []ClusterEndpoint `json:"endpoints"`; Transport string `json:"transport"`; Network string `json:"network"` }
  type RPCMember struct { HeadSpecID string `json:"head_spec_id"`; Transport string `json:"transport"`; BindIP string `json:"bind_ip"`; Port int `json:"port"`; HeadIPs []string `json:"head_ips"`; HeadDomain string `json:"head_domain"`; CacheEnabled bool `json:"cache_enabled"` }
  // Spec gains: Kind string `json:"kind,omitempty"`; DeviceBackend string `json:"device_backend,omitempty"`; Cluster *ClusterHead `json:"cluster,omitempty"`; RPC *RPCMember `json:"rpc,omitempty"`
  // LastError gains: Code string `json:"code,omitempty"`; Status gains Kind, RPC *RPCStatus, Cluster *ClusterStatus, AdmissionBlockedBy string
  func (s Spec) IsClusterHead() bool   // Cluster != nil && len(Cluster.Endpoints) > 0
  func (s Spec) IsRPCServer() bool     // Kind == SpecKindRPCServer && RPC != nil
  const gatewayFeatureRPCCluster = "rpc_cluster"
  func withoutRPCCluster(cfg Config) (Config, bool) // drops rpc_server specs and head specs (whole spec), reports whether anything was dropped
  ```
- `sameLaunchShape`: `Kind`, `DeviceBackend`, `Cluster`, `RPC` stay launch-affecting except `RPC.HeadIPs`/`RPC.HeadDomain` (allowlist only, applied live — strip them).

- [ ] **Step 1: Write failing tests**
  - `TestParseConfigRPCServerSpecRoundTrips`: a document with one `rpc_server` spec and one head parses; `spec.IsRPCServer()`, `head.IsClusterHead()`, endpoints order preserved.
  - `TestParseConfigDemotesInvalidRPCServer`: `kind:"rpc_server"` without `rpc`, or with `port<=0`, or with `listen_port`/`upstream_model` set → spec dropped with a Warn, document still parses (the `sanitizeInstanceFields` precedent).
  - `TestParseConfigDropsRPCKindWithoutGatewayFeature` (driver): features server without `rpc_cluster` → `Sync` applies a config with neither the member nor the head; with it → both present; flipping the declaration bypasses the ETag short-circuit (like `sameReplicableOptIn`).
  - `TestSameLaunchShapeRPCAllowlistIsNotLaunchAffecting`: changing `RPC.HeadIPs` keeps the shape; changing `RPC.Port` or `Cluster.Endpoints` order does not.
- [ ] **Step 2:** run `go test ./internal/runtime -run 'TestParseConfigRPC|TestParseConfigDemotes|TestParseConfigDrops|TestSameLaunchShapeRPC' -race` → FAIL.
- [ ] **Step 3:** implement the types, sanitize rule, `withoutRPCCluster` + driver wiring, strip list; bump `Version` with a doc paragraph naming RPC cluster mode; fix the version pin test.
- [ ] **Step 4:** rerun → PASS; `go test ./internal/agent -run TestFeature` → PASS.
- [ ] **Step 5:** commit `feat(agent): wire types for RPC cluster heads and members`.

### Task A2: Device placeholders and device backend

**Files:**
- Modify: `internal/runtime/policy_local.go` (`deviceBackendByToken` ~870, `resolveNonSecretToken` ~889, `expandSpec` ~953, `ExpandPlaceholders` ~808, visibility injection ~1141)
- Modify: `internal/runtime/config_files.go` (`expandConfigFileContent` ~66)
- Modify: `cmd`/`main.go` vendor wiring (`runtimeGPUVendor` ~440): pass the detected vendor **set**
- Test: `internal/runtime/policy_local_test.go`

**Interfaces:**
- Produces:
  ```go
  type GPUVendors []GPUVendor          // detected set, precedence order
  func ResolveDeviceBackend(spec Spec, vendors GPUVendors) (prefix string, err error) // "CUDA","ROCm","Vulkan","MTL","SYCL"; P4
  func rpcDeviceNames(spec Spec) []string        // RPC0..RPC(n-1) from Cluster.Endpoints
  func localDeviceNames(spec Spec, prefix string, renumbered bool) []string
  ```
- Tokens (exact match): `RPC_SERVERS`, `RPC_DEVICES`, `DEVICES`, `FIRST_LOCAL_DEVICE`, `CUDA_FIRST_DEVICE`, `VULKAN_FIRST_DEVICE`, `METAL_FIRST_DEVICE`. Values per spec §3.1–3.4; `renumbered` = `SetVisibleDevices && mode != args && VisibleDevicesVar(vendor) != ""`.

- [ ] **Step 1: Write failing tests** (table-driven `TestExpandClusterPlaceholders`, each row asserting the expanded args/env or the error):
  - head, GPUs `[3,2]`, nvidia, endpoints `[{a:2},{b:1}]`: `${RPC_SERVERS}`→`a,b`; `${RPC_DEVICES}`→`RPC0,RPC1,RPC2`; `${DEVICES}`→`RPC0,RPC1,RPC2,CUDA3,CUDA2`; `${FIRST_LOCAL_DEVICE}`→`CUDA3`; `${CUDA_FIRST_DEVICE}`→`CUDA3`; `${VULKAN_FIRST_DEVICE}`→`Vulkan3`.
  - same with `set_visible_devices` env on nvidia: `${DEVICES}`→`RPC0,RPC1,RPC2,CUDA0,CUDA1`.
  - head without GPUs: `${DEVICES}`→`RPC0,RPC1,RPC2`; `${FIRST_LOCAL_DEVICE}`→`RPC0`; `${CUDA_FIRST_DEVICE}` → error.
  - non-head without GPUs: `${DEVICES}`, `${FIRST_LOCAL_DEVICE}` → error; `${RPC_SERVERS}`/`${RPC_DEVICES}` on a non-head → error.
  - backend: vendors `[nvidia]`→`CUDA`; `[amd]`→`ROCm`; `[apple]`→`MTL`; `[nvidia,amd]`→`Vulkan`; explicit `sycl`→`SYCL`; `[]` + `auto` + a GPU token → error.
  - embedded use `--device=${DEVICES}` and config-file content both expand.
  - `TestVisibleDevicesArgsModeAcceptsDevicesToken` (agent does not enforce the portal rule; just expands).
- [ ] **Step 2:** run → FAIL. **Step 3:** implement. **Step 4:** run → PASS. **Step 5:** commit `feat(agent): RPC and generic device placeholders with a device backend`.

### Task A3: Member command line, TCP readiness, exclusion from model paths

**Files:**
- Create: `internal/runtime/rpc_member.go` (member argv/env assembly, cache dir, readiness)
- Modify: `internal/runtime/manager.go` (`startProcess` ~3780, `pollHealth` ~3946, `rebuildUpstreamIndex` ~2005, `LoadedModels`, idle default)
- Modify: `internal/runtime/policy_local.go` (refuse reserved member flags)
- Modify: `internal/agent/agent.go` (collect probes ~2432: skip `Kind == rpc_server`)
- Test: `internal/runtime/rpc_member_test.go`, stub child gains `-tcp-listen` mode (`testdata/stubchild/main.go`) that only accepts TCP

**Interfaces:**
- Produces:
  ```go
  func memberArgs(spec Spec, childPort int, prefix string) ([]string, error) // operator args + agent -H/-p/-d (+ -c)
  func memberEnv(spec Spec, cacheRoot string) []string                      // GGML_RPC_NO_RDMA=1, LLAMA_CACHE=<cacheRoot>/rpc/<head spec id> when cache
  func probeTCP(addr string, timeout time.Duration) bool
  var errMemberFlagReserved = errors.New("runtime: -H, -p, -d and -c are set by the agent on rpc_server specs")
  ```
- Rules: relay → child `-H 127.0.0.1 -p <ephemeral>`; direct → `-H <bind_ip> -p <rpc.port>`; `-d` = local device names (A2) or `CPU` when no GPUs; `GGML_RPC_NO_COMM=1` on heads; `set_visible_devices` env mode on a member → `not_permitted`; operator `-H/--host/-p/--port/-d/--device/-c/--cache` (any `=` form) → `not_permitted`; readiness = TCP connect to bind addr:port (direct) or `127.0.0.1:child port` (relay; relay readiness added in A7); `rpc_server` specs never enter `byUpstream`, `LoadedModels`, `/v1/models`; default idle 60 s when the spec says 0.
- Free-disk floor for `-c`: refuse start when the cache volume has < 5 GiB free (`not_permitted`, message names the path) — constant `memberCacheMinFreeMB = 5120`.
- Cache purge: the agent deletes `<cacheRoot>/rpc/<head spec id>` when a member spec's `rpc.head_spec_id` changes or the spec leaves the document; `POST /rpc/purge-cache/{specID}` (added to the router in A5) purges on demand (refused 409 while held). Test `TestMemberCachePurgedOnHeadChange`, `TestPurgeCacheRefusedWhileHeld`.

- [ ] **Step 1: Write failing tests:** `TestMemberArgsDirectGPU` (`-d CUDA3,CUDA2 -H 100.64.0.5 -p 47001` appended after operator `-t 8`), `TestMemberArgsCPUMember` (`-d CPU`), `TestMemberArgsRelayBindsLoopback`, `TestMemberRefusesReservedFlags` (each of the eight spellings → `not_permitted`), `TestMemberEnvModeRefused`, `TestMemberCacheDirPerHead`, `TestHeadGetsNoComm`, `TestRPCServerReadyOnTCPConnect` (stub `-tcp-listen` → `StateRunning`, no HTTP probe), `TestRPCServerNotInUpstreamIndex`, `TestCollectSkipsRPCServerProbes` (agent: zero probe calls over 3 collect cycles).
- [ ] **Step 2–4:** fail → implement → pass. **Step 5:** commit `feat(agent): rpc_server member command line and TCP readiness`.

### Task A4: Exit and banner classification

**Files:**
- Create: `internal/runtime/rpc_classify.go`
- Modify: `internal/runtime/manager.go` (`onProcExited` ~4158, after `tail := proc.log.Tail(2048)` ~4344; `recordFailure`; `setNotPermitted`)
- Test: `internal/runtime/rpc_classify_test.go`, `manager_test.go` (stub child `-print` + `-exit-code`)

**Interfaces:**
- Produces:
  ```go
  type exitClass struct { Code string; Terminal bool }
  func classifyExit(spec Spec, tail string) (exitClass, bool)
  type memberBanner struct { Protocol string; Devices []string }
  func parseMemberBanner(lines []string) (memberBanner, bool) // "Starting RPC server v8.0.1", "Devices:" block until "  transport"
  ```
- Mapping (terminal = `not_permitted`-style, no backoff, until config changes): `invalid device: ` (head) or `unknown device: ` / `No devices found` (member) → `runtime.device_backend_mismatch`; `RPC not supported in this build` → `runtime.rpc_unsupported_build`; `RPC handshake failed` / `protocol version mismatch` → `runtime.cluster_version_mismatch`. Non-terminal (backoff as today, code set): `Failed to connect` → `runtime.cluster_member_unreachable`. Device count: banner devices ≠ expected (GPU rows, or 1 for CPU member) → kill, terminal `runtime.rpc_device_count_mismatch`.

- [ ] **Step 1:** `TestClassifyExitTable` (each message → code/terminal), `TestParseMemberBannerFromStage0Output` (the exact Stage 0 banner from the spec §12 → protocol `8.0.1`, 2 devices), `TestMemberDeviceCountMismatchFailsStart` (stub prints a 1-device banner for a 2-GPU spec → state `not_permitted`, `LastError.Code == "runtime.rpc_device_count_mismatch"`), `TestHeadInvalidDeviceIsTerminal`.
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): classify RPC exits and verify member device counts`.

### Task A5: Member routes, reservation, holds, stop refusal

**Files:**
- Create: `internal/runtime/rpc_hold.go` (hold/reservation state on `specState`)
- Modify: `internal/runtime/router.go` (dispatch ~356, `sentinelCode` ~1918), `manager.go` (`scanIdle` ~5108, `buildSnapshot` ~3558 `Pinned`, `evictOneIdleForBootstrap` ~4471, `stop.go` `stopInstance` ~142), `types.go` (`managerPort`/`stopPort` additions)
- Test: `internal/runtime/router_rpc_test.go`, `manager_rpc_hold_test.go`

**Interfaces:**
- Produces:
  ```go
  func (m *Manager) EnsureMember(ctx context.Context, specID string, reserve time.Duration) error
  func (m *Manager) ReleaseMember(specID string) error
  func (m *Manager) StopMember(ctx context.Context, specID string, force bool) (StopState, error)
  func (m *Manager) SetMemberHold(specID string, holder string, since time.Time) // called by relay/observer; holder "" clears
  var ErrMemberInUse = errors.New("runtime.cluster_member_in_use") // 409
  ```
- Held or reserved member: skipped by `scanIdle`, `RunningProc.Pinned = true` in snapshots (never an eviction victim), `/rpc/stop` → 409 unless `force`. Reservation expires at `now + reserve` or on `ReleaseMember`. A local admission that can only proceed by evicting a held member sets `Status.AdmissionBlockedBy = <member spec id>` on the waiting spec and waits (no failure).

- [ ] **Step 1:** `TestRPCEnsureStartsMemberAndReserves` (200 after TCP ready; `reserved_until` ≈ now+N), `TestReservedMemberSurvivesIdleScan`, `TestHeldMemberNeverEvicted` (admission that needs the room → `Wait` + `AdmissionBlockedBy`), `TestRPCStopRefusedWhileHeld` (409 code), `TestRPCStopForce`, `TestReservationExpires`, `TestRPCEnsureUnknownSpec404`, `TestRPCRoutesRejectModelSpecs` (a model spec id on `/rpc/ensure` → 404).
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): member ensure, reservation, holds and stop refusal`.

### Task A6: Head guard, pre-exec probe, conditional stop

**Files:**
- Modify: `internal/runtime/router.go` (`serveEnsure` ~720 header, `serveStop` ~780 `if_evictable`), `manager.go` (`handleEnsure` ~2057 guard before the Running branch; `applyConfig` start loop ~1733; `handleBackoffFire` ~4620; `scanIdle` head skip when pinned/force_running), `stop.go`
- Create: `internal/runtime/cluster_head.go`
- Test: `internal/runtime/manager_cluster_head_test.go`, `router_cluster_test.go`

**Interfaces:**
- Produces:
  ```go
  const ClusterStartHeader = "X-OP-Cluster-Start"
  func (m *Manager) EnsureClusterHead(ctx context.Context, model string) (endpoint string, release func(), err error)
  func probeEndpoints(ctx context.Context, eps []ClusterEndpoint, bound time.Duration) error // TCP connect each, retry every 500 ms until bound; never sends a byte
  var ErrClusterNotReady = errors.New("runtime.cluster_not_ready")               // 503
  var ErrClusterMemberUnreachable = errors.New("runtime.cluster_member_unreachable") // 503
  var ErrInstanceBusy = errors.New("runtime.instance_busy")                       // 409, conditional stop
  ```
- Guard: a cold head starts only through `EnsureClusterHead` (router, header present); proxied requests, `/live`, unmarked `/ensure`, `Apply` (even pinned/force_running) and backoff never start it → `ErrClusterNotReady`. A Running head serves normally. Pinned/force_running heads: idle scan skips them; `/stop` drains them (no `instance_protected`). The probe runs outside the owner goroutine before `EnsureRunning`, bounded by the head's startup timeout. Conditional stop applies `isEvictable` with force_running counted as pinned.

- [ ] **Step 1:** `TestColdHeadRefusesPlainRequest` (503 code), `TestApplyNeverStartsPinnedHead`, `TestBackoffNeverRestartsHead`, `TestClusterStartHeaderStartsHead` (stub members listening), `TestProbeRetriesUntilMemberListens` (listener opened after 1 s), `TestProbeGivesUpWithUnreachable`, `TestPinnedHeadStopDrains`, `TestConditionalStopRefusesBusyHead` (in-flight 1 → 409 `runtime.instance_busy`), `TestConditionalStopStopsIdleHead`.
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): cluster head guard, endpoint probe and conditional stop`.

### Task A7: Relay package

**Files:**
- Create: `internal/rpcrelay/relay.go`, `internal/rpcrelay/relay_test.go`, `internal/rpcrelay/fuzz_test.go`
- Modify: `internal/archtest/arch_test.go` (`allowedDeps`: `internal/rpcrelay` → `[]` leaf; `internal/agent` gains the edge), `internal/agent/agent.go` (wire relays from the applied config, `SetMemberHold` callbacks)
- Test as listed

**Interfaces:**
- Produces:
  ```go
  type Config struct { SpecID string; Bind string /* ip:port */; Target string /* 127.0.0.1:childPort */; AllowedIPs func() []net.IP; HeadSpecID string; StallAfter time.Duration }
  type Events interface { Held(specID, headSpecID string, since time.Time); Released(specID string); Refused(specID string, peer net.IP); Stalled(specID string) }
  type Manager struct{ /* one listener per member */ }
  func NewManager(ev Events, running func(specID string) bool) *Manager
  func (m *Manager) Apply(cfgs []Config) error  // add/replace/remove listeners; closes sessions on allowlist change
  func (m *Manager) Close()
  ```
- Behaviour: bind exactly `Bind` (not local → report `bind_mismatch`, retry every 5 s); check source IP before reading; refuse non-allowlisted (log + `Refused`); one head per member — a newer connection from an allowed IP replaces the old one; take the hold on the first client byte only while `running(specID)`, never start it; copy both ways unparsed; TCP keepalive 30 s; stall = client bytes unanswered for `StallAfter` (default 120 s) → `Stalled`; close on drain/stop/shutdown. Caps: max 4 concurrent accepted sockets per member, 10 s deadline to see the first byte.

- [ ] **Step 1:** `TestRelayForwardsBytesBothWays`, `TestRelayRefusesUnlistedSource` (no byte reaches the target), `TestRelayEmptyAllowlistRefusesAll`, `TestRelayRefusesSecondHead` (second allowed-IP connection replaces; a non-allowed one is refused), `TestRelayHoldsOnFirstByteOnlyWhenRunning`, `TestRelayReleasesOnClose`, `TestRelayStallSignal`, `TestRelayClosesOnAllowlistChange`, `TestRelayBindMismatchRetries`, `FuzzRelayFirstBytes` (random prefix bytes never panic or leak to an unlisted source); run all with `-race`; soak `TestRelaySoak` (`-short` skips) 10 k round trips.
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): byte relay for RPC members`.

### Task A8: Direct-mode connection observer and head identity

**Files:**
- Create: `internal/runtime/rpc_observer.go` (or `internal/agent/rpc_observer.go` if gopsutil import rules require; check archtest — gopsutil `net` is already used by `internal/collector/host.go`, so the observer lives in `internal/collector/conns.go` with a pure function and is driven from `internal/agent`)
- Create: `internal/agent/rpc_identity.go` (resolve `head_domain` every 60 s, keep last good set)
- Test: `internal/collector/conns_test.go`, `internal/agent/rpc_observer_test.go`

**Interfaces:**
- Produces:
  ```go
  // collector
  type Conn struct { Local, Remote netip.AddrPort }
  func EstablishedConns(pid int32) ([]Conn, error)
  // agent
  type observation struct { HeldBy string; Breaches []netip.Addr }
  func classifyConns(conns []Conn, bind netip.AddrPort, headIPs []netip.Addr, ownIPs []netip.Addr, headSpecID string) observation
  func (r *headResolver) IPs(specID string) []netip.Addr
  ```
- Behaviour: every 2 s on its own goroutine (never in the collect loop), for each Running direct-mode member: established conns on the bind port; a remote in `headIPs` → `SetMemberHold(spec, head)`; remote = own address → ignored; any other remote → breach (deduped per peer, kept 10 min, reported in telemetry) and **never** a holder; no head conn → `SetMemberHold(spec, "")` only after 2 consecutive empty polls. Domain resolution failure keeps the last good set; no good set ever → empty allowlist (relay refuses all, observer: every peer a breach).

- [ ] **Step 1:** `TestObserverHeadConnHolds`, `TestObserverForeignPeerIsBreachNotHolder`, `TestObserverOwnAddressIgnored`, `TestObserverReleaseNeedsTwoEmptyPolls`, `TestHeadResolverKeepsLastGoodSet`, `TestHeadResolverNoGoodSetIsEmpty`, `TestObserverRunsOffCollectLoop` (collect cadence unchanged while a fake `EstablishedConns` blocks 3 s).
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): direct-mode connection observer and head identity`.

### Task A9: Telemetry, feature declaration, agent docs

**Files:**
- Modify: `internal/sample/sample.go` (`RuntimeSample` ~133), `internal/agent/agent.go` (copy at ~2401-2418), `internal/agent/features.go` (`FeatureRPCCluster = "rpc_cluster"`, `Since: "0.18.0"`), `internal/agent/features_test.go` (`TestFeaturesDeclareRPCCluster`)
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` (placeholders §3.2 incl. Vulkan-order limitation, `set_visible_devices` args rule, member kind, relay, observer, router routes, error codes, loopback rule exception)
- Test: `internal/agent/agent_rpc_test.go`

- [ ] **Step 1:** `TestSampleCarriesRPCStatus` (kind, held_by, protocol, device_count, breaches, reserved_until, cache_mb, stalled), `TestSampleCarriesAdmissionBlockedBy`, `TestSampleCarriesLastErrorCode`, `TestFeaturesDeclareRPCCluster`.
- [ ] **Step 2–4.** **Step 5:** commit `feat(agent): RPC telemetry and the rpc_cluster feature`.

### Task A10: Binary inventory

**Files:**
- Create: `internal/agent/binaries.go`, `internal/agent/binaries_test.go`
- Modify: `internal/sample/sample.go` (top-level `Binaries []BinaryInfo \`json:"binaries,omitempty"\``), `internal/agent/agent.go` (attach on first sample and on change)

**Interfaces:**
```go
type BinaryInfo struct { Path, Kind, Version, Build, Commit, VersionSource string; Allowed bool } // Kind: llama_server | rpc_server | other; VersionSource: agent | path | ""
func scanBinaries(allowed []string, stat func(string) (fs.FileInfo, error), runVersion func(ctx context.Context, path string) (string, error), cache map[string]cachedVersion) []BinaryInfo
func parseLlamaVersion(out string) (version, build, commit string, ok bool) // "version: 0.9.0-dev (build 1, commit 2bc46ba)"
func versionFromPath(path string) (build, commit string, ok bool)          // b<digits> tag or 7-40 hex segment
```
Rules: rescan every 5 min; `--version` only for kind `llama_server`, 10 s timeout, cached by (path, size, mtime); `rpc_server` inherits from a `llama_server` in the same directory, else `versionFromPath`; inventory hash decides whether the sample carries `binaries`.

- [ ] Step 1: `TestParseLlamaVersionStage0Output`, `TestVersionFromPathTagAndCommit`, `TestRPCServerInheritsSiblingVersion`, `TestVersionRunCachedByStat`, `TestOnlyLlamaServerIsExecuted`, `TestSampleCarriesBinariesOnlyOnChange`.
- [ ] Step 2–4: fail → implement → pass. Step 5: commit `feat(agent): report the binary inventory with versions`.

---

## Phase B — gateway store (`gateway/backend`, `go test ./internal/store/... ./internal/routing/...` with the Postgres DSN set)

### Task B1: Migration 103 — head, spec and server columns

**Files:**
- Modify: `internal/store/migrate.go` (append `{version: 103, name: "rpc_cluster_columns"}`), `internal/routing/store.go` (`RuntimeSpec`, `AIServer`), `internal/store/sqlite_runtime.go` (`runtimeSpecCols`, prefixed list, `scanRuntimeSpec`, upsert set), `internal/store/sqlite_routes.go` (seven AIServer sites)
- Test: parity tests, `internal/store/migration103_rpc_test.go`

**Columns:** `agent_runtime_specs`: `cluster_enabled integer not null default 0`, `member_mode text not null default 'automatic'`, `cluster_network text not null default ''`, `rpc_transport text not null default ''`, `lan_exposure_ack integer not null default 0`, `device_backend text not null default 'auto'`. `ai_servers`: `netbird_ip text not null default ''`, `lan_address text not null default ''`, `lan_bind_only integer not null default 0`, `rpc_member_enabled integer not null default 0`, `rpc_allowed_head_groups text not null default '[]'`. New table `server_binaries(server_id → ai_servers cascade, path, kind, version, build, commit, version_source, source, allowed, reported_at, primary key (server_id, path))` with store methods `ServerBinaries(ctx, serverID) ([]ServerBinary, error)`, `ReplaceAgentServerBinaries(ctx, serverID string, rows []ServerBinary) error` (replaces `source='agent'` rows, keeps `manual` rows and manual version overrides), `UpsertManualServerBinary`, `DeleteManualServerBinary`.

- [ ] Steps: failing parity/migration tests (fresh + upgraded-from-102 legs, replay idempotent) → implement → pass on all three stores → commit `feat(store): RPC cluster columns on specs and servers`.

### Task B2: Migration 104 — member specs, membership, sessions

**Files:**
- Modify: `internal/store/migrate.go` (104 `rpc_cluster_tables`), `internal/routing/store.go` (new types + `RPCClusterStore` interface embedded in `Store`), `internal/routing/memory_store.go`
- Create: `internal/store/sqlite_rpc_cluster.go`
- Test: `internal/store/routing_store_conformance_test.go` (`TestRoutingStoreRPCCluster`), `internal/store/migration104_rpc_test.go`

**Interfaces:**
```go
type RPCMemberSpec struct { ID, ServerID string; Enabled bool; Binary, Args, Env string; CacheEnabled bool; DeviceBackend string; RAMEstimateMB int; AllowedHeadGroups string; CreatedAt, UpdatedAt time.Time }
type RPCMemberSpecGPU struct { SpecID string; GPUIndex, Position, VRAMEstimateMB int }
type ClusterMember struct { HeadSpecID string; Position int; ServerID string; MemberSpecID string /* "" = automatic */; Threads int; CacheEnabled bool; DeviceBackend string; RAMEstimateMB int; Port int; RAMMeasuredMB int; MeasuredAt time.Time; Stale bool; RunPeakRAMMB int; RunHeadInstance int; RunStartedAt time.Time }
type ClusterMemberGPU struct { HeadSpecID, ServerID string; GPUIndex, Position, VRAMEstimateMB, VRAMMeasuredMB, RunPeakVRAMMB int }
type RPCSession struct { HeadSpecID string; Instance int; MemberServerID string; Since time.Time; Until *time.Time }
type RPCClusterStore interface {
  UpsertRPCMemberSpec(ctx context.Context, s RPCMemberSpec) error
  RPCMemberSpecByID(ctx context.Context, id string) (RPCMemberSpec, bool, error)
  RPCMemberSpecsByServer(ctx context.Context, serverID string) ([]RPCMemberSpec, error)
  DeleteRPCMemberSpec(ctx context.Context, id string) error
  SetRPCMemberSpecGPUs(ctx context.Context, specID string, gpus []RPCMemberSpecGPU) error
  RPCMemberSpecGPUs(ctx context.Context, specID string) ([]RPCMemberSpecGPU, error)
  SetClusterMembers(ctx context.Context, headSpecID string, members []ClusterMember, gpus []ClusterMemberGPU) error // replace, carrying measured fields per (server, gpu_index) is the service's job
  ClusterMembers(ctx context.Context, headSpecID string) ([]ClusterMember, []ClusterMemberGPU, error)
  ClusterMembershipsByServer(ctx context.Context, serverID string) ([]ClusterMember, error) // reverse index
  ClusterMemberBySpec(ctx context.Context, memberSpecID string) (ClusterMember, bool, error)
  UpdateClusterMemberUsage(ctx context.Context, m ClusterMember, gpus []ClusterMemberGPU) error // measured/run-peak fields only
  OpenRPCSession(ctx context.Context, s RPCSession) error
  CloseRPCSessions(ctx context.Context, headSpecID string, instance int, memberServerID string, until time.Time) error
  RPCSessionsAt(ctx context.Context, headSpecID string, instance int, at time.Time) ([]RPCSession, error)
}
```
Tables: `rpc_member_specs` (FK `server_id` → `ai_servers` cascade), `rpc_member_spec_gpus` (cascade, PK `(spec_id, gpu_index)`), `rpc_cluster_members` (FK head → `agent_runtime_specs` cascade; unique `(head_spec_id, position)`, unique `(head_spec_id, server_id)`, partial unique `member_spec_id` where `<> ''`), `rpc_cluster_member_gpus` (cascade from the membership row via `(head_spec_id, server_id)`), `rpc_sessions` (index `(head_spec_id, instance)`, `(member_server_id, since)`). Bigint for ms/time per ADR-005.

- [ ] Steps: conformance tests first (upsert/read/delete, cascade on head delete and server delete, both unique rules → `ErrConflict`, reverse index, session open/close/at) → implement SQL + memory → pass on all three → commit `feat(store): RPC member specs, cluster membership and sessions`.

### Task B3: Migration 105 — usage hosts and energy parts

**Files:**
- Modify: `internal/store/migrate.go` (105 `usage_event_hosts`), `internal/usage/recorder.go` (`Store` interface + memory `Recorder`), `internal/store/sqlite_usage.go` (`UsageEventsForServerWindow` ~178 includes member rows)
- Test: `internal/store/usage_parts_test.go`, `internal/usage/recorder_test.go`

**Interfaces:**
```go
type EventHost struct { EventID, ServerID, Role string /* "head" | "member" */; CreatedAt time.Time }
type EnergyPart struct { EventID, ServerID, Role string; Wh, MarginalWh float64; Source string }
// usage.Store gains:
RecordEventHosts(ctx context.Context, hosts []EventHost) error
EventHosts(ctx context.Context, eventID string) ([]EventHost, error)
SetEnergyParts(ctx context.Context, eventID string, parts []EnergyPart) error
EnergyParts(ctx context.Context, eventID string) ([]EnergyPart, error)
```
Tables: `usage_event_hosts(event_id → usage_events cascade, server_id, role, created_at)`, index `(server_id, created_at)`; `usage_event_energy_parts(event_id → cascade, server_id, role, wh, marginal_wh, source)`. `UsageEventsForServerWindow(server)` returns events whose `host = server` **or** that list `server` in `usage_event_hosts`.

- [ ] Steps: failing tests (`TestUsageEventsForServerWindowIncludesMemberRows`, parts round trip, cascade) → implement → pass → commit `feat(store): usage event hosts and energy parts`.

### Task B4: Server fields end to end and mesh IP sync

**Files:**
- Modify: `cmd/gateway/netbird_sync.go` (~143-152 persist `peer.IP`; clear on disable/unlink), `internal/routing/store.go` (`UpdateServerNetbirdState` gains `ip string`), `internal/store/sqlite_routes.go` (~215), memory store, `internal/portal/service.go` (`ServerDTO` ~1527, `UpdateServerRequest` ~1683 pointer fields, validation ~3919: `lan_address` must parse as IPv4 or be empty → `server.lan_address_invalid`), `internal/gateway/portal_server_endpoints.go` (error rows ~438)
- Test: `cmd/gateway/netbird_sync_test.go` (`TestSyncPersistsPeerIP`), `internal/portal/service_server_rpc_test.go`

- [ ] Steps: failing tests (IP persisted/cleared; lan/rpc fields round-trip through Update; invalid values → codes; `UpdateServer` notifies runtime) → implement → pass → commit `feat(gateway): mesh IP, LAN address and RPC member settings on servers`.

---

## Phase C — gateway portal service (`internal/portal`, `internal/gateway`)

### Task C1: Manual member spec service and API

**Files:**
- Create: `internal/portal/service_rpc_members.go`, `internal/gateway/portal_rpc_member_endpoints.go`
- Modify: `internal/gateway/server.go` / `portal_server_endpoints.go` (sub-path `{id}/rpc-members[/{specID}]`)
- Test: `internal/portal/service_rpc_members_test.go`, `internal/gateway/portal_rpc_member_endpoints_test.go`

**Interfaces:**
```go
type RPCMemberSpecDTO struct { ID, ServerID string; Enabled bool; Binary string; Args []string; Env map[string]string; GPUs []RuntimeSpecGPUDTO; CacheEnabled bool; DeviceBackend string; RAMEstimateMB int; AllowedHeadGroups []string; UsedByHeadSpecID string /* only when visible to the caller */ }
func (s *Service) RPCMemberSpecs(ctx context.Context, p auth.Token, serverID string) ([]RPCMemberSpecDTO, error)
func (s *Service) PutRPCMemberSpec(ctx context.Context, p auth.Token, serverID, specID string, req PutRPCMemberSpecRequest) (RPCMemberSpecDTO, error) // specID "" = create
func (s *Service) DeleteRPCMemberSpec(ctx context.Context, p auth.Token, serverID, specID string) error
```
Errors: `runtime_spec.member_flag_reserved` (400), `runtime_spec.visible_devices_*` reuse, `runtime_spec.member_vram_estimate_required` (400: a GPU row on a server whose hardware report is non-NVIDIA without an estimate), `runtime.cluster_member_in_use` (409 delete/disable while referenced), `server.not_found` (404 via `authorizeServer`, no leak). Writes notify the member server and every head server referencing the spec (reverse index).

- [ ] Steps: failing tests (CRUD; reserved flags in every spelling; consent default = server admin groups; delete referenced → 409; foreign principal → 404 and no head name leak; notify fan-out) → implement → pass → commit `feat(portal): manual RPC member specs`.

### Task C2: Head cluster configuration and save checks

**Files:**
- Modify: `internal/portal/service_runtime.go` (`RuntimeSpecDTO`, `PutRuntimeSpecRequest`, `validateRuntimeSpecRequest`/`writeRuntimeSpec`, placeholder lists ~1548, `argsHaveDevicePlaceholder`)
- Create: `internal/portal/service_cluster.go` (cluster validation, port allocator, eligible-member listing, carry-over/stale rules), `internal/portal/cluster_args.go` (args/env scan for §3a)
- Modify: `internal/gateway/portal_runtime_endpoints.go` (error rows), `portal_mapping_endpoints.go` (`{id}/runtime/cluster-candidates`)
- Test: `internal/portal/service_cluster_test.go`, `cluster_args_test.go`

**Interfaces:**
```go
type ClusterMemberDTO struct { Position int; ServerID, ServerName string; MemberSpecID string; GPUs []ClusterMemberGPUDTO; Threads int; CacheEnabled bool; DeviceBackend string; RAMEstimateMB, RAMMeasuredMB int; MeasuredAt *time.Time; Stale bool; Port int; Visible bool /* false: only counted in totals */ }
type ClusterMemberGPUDTO struct { Index, VRAMEstimateMB, VRAMMeasuredMB int }
// RuntimeSpecDTO / PutRuntimeSpecRequest gain: ClusterEnabled bool; MemberMode, ClusterNetwork, RPCTransport string; LANExposureAck bool; DeviceBackend string; ClusterMembers []ClusterMemberDTO
// read-only echoes: ClusterVRAMTotalMB, ClusterRAMTotalMB int; ClusterTotalsComplete bool; ClusterNetworkDefault, ClusterTransportDefault string
func scanClusterArgs(args []string, env map[string]string) clusterArgScan // rpcInArgs, rpcInEnv, rpcArgIndex, deviceInArgs, deviceInEnv, deviceArgIndex, deviceHasRPC
func (s *Service) allocateRPCPort(ctx context.Context, serverID string) (int, error) // range 47000-47999, skips app Port, ProxyListenPort, router_listen, pinned listen_port, other RPC ports
func (s *Service) ClusterCandidates(ctx context.Context, p auth.Token, headMappingID, mode string) ([]ClusterCandidateDTO, error)
```
Save checks (appended last, in this order; all 400 unless noted): cluster on requires `type == llama_cpp` (`runtime_spec.cluster_type_invalid`); `${RPC_SERVERS}` present (`runtime_spec.cluster_rpc_placeholder_missing`) and exactly once across args/env (`runtime_spec.cluster_rpc_twice`); cluster off with `${RPC_SERVERS}`/`${RPC_DEVICES}` → `runtime_spec.cluster_rpc_placeholder_without_cluster`; device list lacks `${DEVICES}`/`${RPC_DEVICES}` → `runtime_spec.cluster_device_list_missing_rpc`; device in args before `--rpc` in args (and RPC not in env), or device in env while RPC in args → `runtime_spec.cluster_device_order`; ≥1 member; head not replicable (`runtime_spec.cluster_head_replicable`); explicit startup timeout > 0 (`runtime_spec.cluster_startup_timeout_required`, Q5); one entry per server (`runtime_spec.cluster_member_server_twice`); member server ≠ head server (`runtime_spec.cluster_member_on_head_server`); manual spec free (`runtime_spec.cluster_member_spec_taken`, 409); automatic server has `rpc_member_enabled` (`runtime_spec.cluster_member_rpc_disabled`) and an inventory `ggml-rpc-server` whose version matches the head binary's (`runtime_spec.cluster_member_no_matching_binary`); manual member with a known version different from the head's (`runtime_spec.cluster_version_mismatch`, unknown → warning); consent (`allowed_head_groups` / `rpc_allowed_head_groups` ∩ head server's admin groups, or system scope) else 404-no-leak on that server; authorization on every member server; network `mesh` requires NetBird + tracking group + known `netbird_ip` on all (`runtime_spec.cluster_mesh_unavailable`); `lan` requires a LAN address per server, `lan_address` on NetBird-enabled ones (`runtime_spec.cluster_lan_address_required`); direct + `mesh` requires manage-policies + deny-by-default + enforce + Default last seen disabled (`runtime_spec.cluster_direct_unavailable`); direct + `lan` requires `lan_exposure_ack` (`runtime.cluster_lan_ack_required`); a member with Apple GPUs needs an explicit GPU budget on its server (`runtime_spec.cluster_member_budget_required`); a member with non-NVIDIA GPUs needs a VRAM estimate and the head needs `--tensor-split` (`runtime_spec.cluster_tensor_split_required`) — issue §7. Defaults when the request leaves them empty: network `mesh` if all NetBird-ready else `lan`; transport per Q3. Warnings (returned in `RuntimeSpecDTO.ClusterWarnings []string` codes, not errors): RPC devices not first, missing `-mmdev`, CPU member without `--tensor-split`, member on shared host without estimate, budget below device total without `--tensor-split`, overlapping GPU selection, relay+mesh with deny-by-default off (Q7).
Carry-over (D1): re-save keeping a (server, gpu_index) keeps measured values; changes to members/order/GPU rows/model/args/env mark the kept values `stale`; removed members drop. A write affecting a running cluster stops the head (C3 push + D1 orchestration flag `restartAfterApply`).

- [ ] Steps: failing tests — one per save code (incl. all four env/argv device-order mixes from spec §2 and the additive double-`--rpc` case), `TestPutClusterDefaultsNetworkAndTransport`, `TestPutClusterReorderStopsHeadAndMarksStale`, `TestPutClusterCarriesMeasuredValues`, `TestAllocateRPCPortSkipsTakenPorts`, `TestClusterCandidatesFiltersByAuthAndConsent`, `TestClusterForeignMemberHiddenInDTO` → implement → pass → commit `feat(portal): cluster head configuration and save checks`.

### Task C3: Runtime document derivation

**Files:**
- Modify: `internal/portal/service_runtime.go` (`AgentRuntimeSpecDTO` ~2702, `AgentRuntimeConfig` ~2887, `agentRuntimeSpecDTO` ~3159, `notifyRuntimeChanged` fan-out), `internal/gateway/agent_features.go` (append `rpc_cluster` to `gatewayAgentFeatures`)
- Create: `internal/portal/service_cluster_config.go`
- Test: `internal/portal/service_cluster_config_test.go`

**Interfaces:**
```go
func (s *Service) clusterHeadBlock(ctx context.Context, head routing.RuntimeSpec, headServer routing.AIServer) (*AgentClusterHeadDTO, string /* withhold reason */, error)
func (s *Service) memberSpecsForServer(ctx context.Context, serverID string) ([]AgentRuntimeSpecDTO, error) // both modes, ids per P3
func (s *Service) notifyClusterServers(headSpecID string) // head server + every member server
```
Rules: endpoints from configured membership only (never liveness); `device_count` = deduped GPU rows/selection or 1; withhold the head (with a reason surfaced in the DTO and `runtime.cluster_*` status) when any member is missing/disabled/unauthorized, a mesh IP/LAN address/port is unknown, NetBird preconditions fail, the head or any member agent lacks `rpc_cluster` (`runtime.cluster_agent_too_old`) or its features are unknown (hold like `runtime_config.agent_features_pending`); never emit partial endpoints; member specs only to agents declaring `rpc_cluster`; member demand = `max(stored usage, estimate)` per GPU and RAM, stale values capped at the GPU budget / host-RAM ceiling; member `idle_timeout_seconds` default 60; head effective idle timeout uses the replicable rule; `rpc.head_ips` = head mesh IP (mesh) or head `lan_address` (lan) else `head_domain` = head `Domain`; bind: mesh → member mesh IP; lan → `lan_address` if `lan_bind_only` else `0.0.0.0`.

- [ ] Steps: failing tests — `TestAgentRuntimeConfigDerivesAutomaticMember`, `…ManualMember`, `…TwoHeadsOneServerTwoInstances` (distinct ids and ports), `TestAgentRuntimeConfigEndpointsIgnoreMemberLiveness`, `TestAgentRuntimeConfigWithholdsClusterForOldAgents`, `…WithholdsOnUnknownMeshIP`, `…NeverPartialEndpoints`, `…LanUsesLanAddressElseDomain`, `…MemberDemandUsesStoredPeak`, `…StaleDemandCappedAtBudget`, `TestMemberEditNotifiesHeadAndMemberServers` → implement → pass → commit `feat(portal): derive cluster heads and members into runtime documents`.

### Task C4: NetBird `op-gw-rpc-*` policy family

**Files:**
- Create: `internal/portal/service_netbird_rpc_policies.go`
- Modify: `service_netbird_policies.go` (`reconcileAllServerPolicies` ~938), `service_netbird_peers.go` (`ReconcileAllServerNetbird` ~262-307), `service_system_settings.go` (refusal before ~1985; `netbird_default_policy_state` write, P7), `internal/gateway/netbird.go` (status reads the stored state too)
- Test: `internal/portal/service_netbird_rpc_policies_test.go` (fake: `newFakeNetbird`, `enableNetbirdPolicies`, `seedManagedNetbirdServer`; extend `setPeer` with an IP)

**Interfaces:**
```go
const NetbirdRPCPolicyPrefix = "op-gw-rpc-"
func desiredRPCPolicyRequest(memberServer routing.AIServer, rules []rpcRule) netbird.PolicyRequest // one accept/TCP/unidirectional rule per member instance: head tracking group → member tracking group, that member's port
func rpcPolicyMatches(p netbird.Policy, rules []rpcRule) bool
func (s *Service) reconcileRPCPoliciesWith(ctx context.Context, ncfg netbird.Config, servers []routing.AIServer, policies []netbird.Policy) // under policyMu; prefix sweep removes orphans
```
Only `mesh` clusters produce rules; Q6 every cluster. Synchronous hook on cluster/membership/member-spec writes and deletes (`reconcileRPCPolicies(ctx)` taking `policyMu`). Turning off manage-policies/deny/enforce while a direct `mesh` cluster exists → `settings.netbird_direct_cluster_exists` (409). Drift (`Default` enabled again, seen by the loop) → C3 withholds direct-mesh heads; D3 stops their members head first.

- [ ] Steps: failing tests (create per member with exact port; update on port change; two members on one server → two rules; delete + sweep orphan; manage-off → untouched; relay rule uses relay port; lan cluster → no policy; switch-off refusal; Default state recorded) → implement → pass → commit `feat(portal): managed NetBird policies for RPC members`.

### Task C5: Binary inventory ingest, API and version matching

**Files:**
- Create: `internal/portal/service_binaries.go`, `internal/gateway/portal_binary_endpoints.go` (`/api/portal/servers/{id}/binaries`, GET/POST/PUT/DELETE for manual rows and version overrides)
- Modify: `internal/gateway/agent_ingest.go` (sample `binaries` → `ReplaceAgentServerBinaries`), `internal/portal/service_cluster.go` (`matchingRPCBinary(ctx, serverID, headVersion) (ServerBinary, bool)` used by C2 checks, C3 derivation and `ClusterCandidates`)
- Test: `internal/portal/service_binaries_test.go`, `internal/gateway/agent_ingest_binaries_test.go`

Rules: authorization via `authorizeServer` (404-no-leak); manual rows never overwritten by agent rows; a manual version override survives agent rescans; matching compares commit when both have one, else build; C3 sets the automatic member's `binary` from the match and withholds the head (`runtime.cluster_member_no_matching_binary`) when none exists at derivation time.

- [ ] Step 1: `TestIngestReplacesAgentBinariesKeepsManual`, `TestManualVersionOverrideSurvivesRescan`, `TestMatchingRPCBinaryByCommitThenBuild`, `TestClusterCandidatesHideServersWithoutMatchingBinary`, `TestBinariesForeignServer404`.
- [ ] Step 2–4. Step 5: commit `feat(gateway): server binary inventory and version matching`.

---

## Phase D — gateway runtime (`internal/gateway`, `internal/routing`)

### Task D1: `ensureCluster` orchestrator and triggers

**Files:**
- Create: `internal/gateway/cluster_orchestrator.go`, `internal/gateway/cluster_orchestrator_test.go`
- Modify: `internal/provider/runtime_ensure.go` (cluster-start header option), `internal/provider/runtime_rpc.go` (new: `/rpc/ensure`, `/rpc/release`, `/rpc/stop` client), `internal/gateway/runtime_actions.go` (Start/Stop for heads → orchestrator; `runtimeAgentCodes` gains the cluster codes), `internal/gateway/server.go` (wire; boot reconciler), `cmd/gateway/main.go` (start loop with `startCancellable`)

**Interfaces:**
```go
type clusterOrchestrator struct { /* one start per head: map[headSpecID]*clusterStart; cooldowns */ }
func (o *clusterOrchestrator) Ensure(ctx context.Context, headSpecID string) error  // joins an in-flight start
func (o *clusterOrchestrator) Stop(ctx context.Context, headSpecID string, stopMembers bool) error
func (o *clusterOrchestrator) State(headSpecID string) clusterGateState            // formed | forming | cold | coolingDown | invalid
func (o *clusterOrchestrator) Signal(d routing.DemandSignal)                        // DemandSink fan-out target
type RPCEnsurer interface { EnsureRPCMember(ctx context.Context, server routing.AIServer, specID string, reserve time.Duration) error; ReleaseRPCMember(...) error; StopRPCMember(ctx, server, specID string, force bool) (string, error) }
```
Steps of `Ensure` (spec/issue §6): (1) ETag wait on head + members (`appliedOurDocument` pattern), bounded 30 s; mesh: `reconcileRPCPolicies` synchronously; (2) members in parallel, each bounded by its startup timeout + 10 s, reservation = head startup bound + 60 s; direct mode: a member listing a connection while its head is not running is force-restarted first; (3) head `/ensure` with `X-OP-Cluster-Start: 1`, bounded by head startup + 30 s; (4) formed = every member reports `held_by == head` within head startup + 30 s, else `runtime.cluster_member_unused` naming the member, head stopped. Failures: started members keep only their reservation; cooldown 60 s with `runtime.cluster_start_failed` naming member/head/cause; terminal codes (`rpc_unsupported_build`, `cluster_version_mismatch`, `device_backend_mismatch`, `rpc_device_count_mismatch`) stop retries until the config ETag changes. Triggers: demand signal (D2), manual Start (202 like #364), boot + config push + head exit for pinned/force_running heads. Watchdog ignores stale telemetry during formation and a 60 s boot grace.

- [ ] Steps: failing tests with fake agents (`scaleEnsurer`-style recorder): `TestEnsureClusterMembersBeforeHead`, `…OneStartPerHead` (two concurrent calls → one sequence), `…MemberRefusedCoolsDown`, `…MemberTimeout`, `…FormationTimeoutStopsHead`, `…TerminalCodeStopsRetries`, `…WaitsForConfigAck`, `TestEnsureClusterIgnoresStaleTelemetryDuringFormation`, `TestManualStartClusterAnswers202`, `TestBootReconcilerStartsPinnedHead`, `TestStopClusterHeadFirst` → implement → pass → commit `feat(gateway): members-first cluster orchestration`.

### Task D2: Routing gate, park, retry, excluded paths

**Files:**
- Modify: `internal/routing/instance_scaling.go` (`CandidateServesNow`), `resolver.go` (`selectCandidate` ~2169, park bound ~937-959, `atCapacityError`), `instances.go` (`DemandSignal.ClusterHead string`), `internal/gateway/server.go` (demand fan-out scaler + orchestrator), `inference_complete.go` (~76 `cluster_not_ready` → orchestrate, retry once, instance 0 too), `model_warmer.go` (`textCandidates`), `benchmark_refusals.go` (`loadRefusal` + `runtime.cluster_head_unsupported`), `benchmark_audio.go`, `voice_chat.go` (`ensureVoiceTarget` → orchestrator for heads)
- Test: `internal/routing/resolver_cluster_test.go`, `internal/gateway/inference_cluster_test.go`, `model_warmer_test.go`, `benchmark_refusals_test.go`

Gate (via a `ClusterGate` interface the resolver reads): formed → serves; cold/forming → treated as at capacity with `DemandSignal.ClusterHead` set, park bound = head startup + slowest member startup (Q4), clipped by `timeout_ms`; cooling down / invalid / member server unselectable or benchmark-busy → excluded; if nothing else serves, the request fails with `runtime.cluster_start_failed` (cooldown) or `runtime.cluster_unavailable`, never `no_healthy_host`.

- [ ] Steps: failing tests `TestResolveParksColdClusterAndSignals`, `TestResolveServesFormedCluster`, `TestResolveCoolingDownFailsWithClusterCode`, `TestResolveParkBoundIsClusterBound`, `TestCompletionRetriesOnceOnClusterNotReady`, `TestWarmerSkipsClusterHeads`, `TestLoadBenchmarkRefusesClusterHead`, `TestVoiceGateUsesOrchestratorForHeads` → implement → pass → commit `feat(gateway): route through cluster gate states`.

### Task D3: Watchdog, R10 removal, breaches

**Files:**
- Create: `internal/gateway/cluster_watchdog.go`
- Modify: `internal/gateway/agent_ingest.go` (feed member/head status to the watchdog), `benchmark_stop.go` / VRAM fleet (`vramEnumerateFleet`) (member servers: refuse or stop cluster head first after confirmation flag), `cluster_orchestrator.go`
- Test: `internal/gateway/cluster_watchdog_test.go`

Rules: positive evidence (member exited, PID changed, session closed, relay `stalled`) → stop head, re-orchestrate after cooldown if the head was in use or desired up, record the failed member; stale telemetry only past 3× agent interval and outside formation/boot grace, after checking the head's own state. `admission_blocked_by` on a member host → map member → head → conditional stop (`/stop?if_evictable=1`); success → member released (`/rpc/release`), failure → nothing (start waits, Q12). Direct mode: head not running → `/rpc/release` on its members. Foreign `held_by` or breach → logged, surfaced in cluster status, never treated as formed. Mesh drift (P7 state `enabled`) → stop direct-mesh clusters head first.

- [ ] Steps: failing tests `TestWatchdogStopsHeadOnMemberExit`, `…OnPIDChange`, `…OnRelayStall`, `…IgnoresStaleInGrace`, `TestBlockedAdmissionTriggersConditionalHeadStop`, `TestBlockedAdmissionWaitsWhenHeadBusy`, `TestHeadStopReleasesDirectMembers`, `TestIngestForeignHeldByIsBreach`, `TestMeshDriftStopsDirectClusters`, `TestBenchmarkDrainOnMemberServerRefused` → implement → pass → commit `feat(gateway): cluster watchdog and member removal through the head`.

### Task D4: Member usage ingest and sessions

**Files:**
- Modify: `internal/gateway/agent_ingest.go` (new `writeBackClusterMemberUsage` beside `writeBackRuntimeVRAM` ~924; `RuntimeStatusDTO` ~58 gains kind/rpc/cluster fields), `internal/portal/service_runtime.go` (read-time cluster totals into the head DTO)
- Create: `internal/gateway/cluster_usage.go`
- Test: `internal/gateway/cluster_usage_test.go`

Rules: a sample counts only if the reporting server owns the member (manual spec's `server_id`, or the automatic membership's `server_id` with id matching P3) **and** `rpc.held_by` names that membership's head; during a run the stored value only rises (`run_peak_*` and stored), at a formed run's end the run peak replaces the stored value and the member server's document is pushed (echo rule otherwise); a run that never formed replaces nothing; sessions open/close in `rpc_sessions` from `held_by` transitions; sessions open at a gateway restart close at the first sample without them. Totals: `Σ max(measured, manual)`, unknown part → `ClusterTotalsComplete=false`. Reset: `POST /api/portal/mappings/{id}/runtime/cluster-members/{serverID}/reset-usage` clears that member's stored usage (authorized on head and member server); test `TestResetMemberUsage`. Cluster status names a stored peak above the member's GPU budget / host-RAM ceiling (`TestClusterStatusFlagsPeakAboveBudget`).

- [ ] Steps: failing tests `TestMemberUsageOnlyFromOwningServer`, `…OnlyWhileHeldByThatHead`, `…RisesDuringRun`, `…RunPeakReplacesAtFormedEnd`, `…UnformedRunKeepsRises`, `…PushesMemberServerAtRunEnd`, `TestRPCSessionsOpenAndClose`, `TestClusterTotalsLowerBound`, `TestCPUMemberRAMOnly` → implement → pass (incl. Postgres leg) → commit `feat(gateway): measure member usage per run and store it with the head`.

### Task D5: Energy parts and cost

**Files:**
- Modify: `internal/gateway/inference_complete.go` (`recordUsage` ~722 writes `usage_event_hosts` from `RPCSessionsAt(head, instance, now)`), `energy_reconciler.go` (~140 per-host parts), `energy_engine.go` (Tier 2 applies PUE ~260; add `ComputeEnergyPart` limited to Tier 1/2), `internal/store/sqlite_usage.go` (`EnergyByServer`, `UsageAggregateSince`, `UsageGroups` price parts by part host), `internal/portal/service.go` (`attachUsageCost` per part), `docs/architecture/cross-cutting/telemetry-usage-observability.md` (§8.4.1 `usage.Event` location fix, Tier 2 PUE, parts)
- Test: `internal/gateway/energy_reconciler_cluster_test.go`, `energy_engine_test.go`, store tests

Rules: per issue §8 + Q8–Q11: head part as today; each member part = that host's power split over its overlapping events (incl. cluster events listing it), Tier 1 or 2 only, missing both → 0 Wh flagged `member_unpriced`; `energy_wh` = sum, `energy_source` = weakest tier; EWMA calibration only when every part is measured; cost = Σ part Wh × that part host's `price_per_kwh`; member server views show aggregated Wh only; the server DTO gains `clock_offset_ms` (latest sample `reported_at` vs receive time, signed), shown in the portal's agent panel (E2) — test `TestServerDTOClockOffset`.

- [ ] Steps: failing tests `TestReconcileClusterEventSumsParts`, `TestReconcileMemberPartGapFallsToTier2`, `TestReconcileClusterEnergyOrderIndependent` (price member-side events before and after the cluster event → identical Wh), `TestTier2AppliesPUE`, `TestClusterCostUsesEachHostPrice`, `TestCalibrationSkipsPartiallyMeasured` → implement → pass (incl. Postgres) → commit `feat(gateway): per-host energy parts for cluster requests`.

---

## Phase E — portal (`gateway/frontend`; `npm test`, `npm run build`, `npm run lint`, `npx prettier --check .`)

### Task E1: API types, error codes, i18n

**Files:** `src/api/runtime.ts` (RuntimeSpec cluster fields, `ClusterMember`, echoes, `RuntimeStatus.kind/rpc/cluster/admission_blocked_by`, `clusterCandidates`, `startRuntime` unchanged path), `src/api/rpcMembers.ts` (new: list/put/delete manual member specs), `src/api/servers.ts` (`netbird_ip`, `lan_address`, `lan_bind_only`, `rpc_member_enabled`, `rpc_allowed_head_groups`), `src/api/binaries.ts` (new: inventory list, manual add/edit/delete), `src/api.ts` barrel, `components/shared/format.ts` (`errorLabelByCode` for every new code in C1–D5 and A4/A6), `src/i18n.ts` (de+en).
- [ ] Steps: failing `runtime.test.ts`/`rpcMembers.test.ts`/`format.test.ts` cases (request shapes, every new code labelled in both locales) → implement → pass → commit `feat(portal): API types and messages for RPC clusters`.

### Task E2: Server settings and manual member view

**Files:** `src/components/ServerList.tsx` (LAN address + bind-only fields), `src/components/RuntimeAdminSection.tsx` (new tab `'rpc'` rendered by a new component), create `src/components/RpcMemberSection.tsx` (+ `.test.tsx`): server RPC settings (enable, binary, allowed head groups), manual member spec list + editor (binary, args, env, GPU rows reusing the ordered-row pattern, cache, backend, estimates, consent), member status list (state, held by, effective VRAM/RAM, last stored value under the estimate as #362), "CPU member" label when no GPU rows.
- [ ] Steps: failing tests (fields round-trip; reserved flag lint mirrors the backend; CPU member label; held-by shows head only when visible; delete referenced → mapped error) → implement → pass → commit `feat(portal): RPC member settings and manual member specs`.

### Task E3: Head cluster editor and placeholder lint

**Files:** create `src/components/ClusterHeadEditor.tsx` (+ test) mounted in the spec form of `RuntimeAdminSection.tsx` for `type === 'llama_cpp'`; create `src/components/shared/clusterArgs.ts` (+ test) mirroring `scanClusterArgs`; extend `findPlaceholderViolation`/hint text with the new tokens; `buildSpecBody`/`hydrateSpecFields`/`emptySpec`/`specBodyWithAdminState` updated.
Editor: cluster toggle; member mode switch (confirm replaces list); network (default shown, LAN address hints); transport (default + reason; relay hint text includes the Stage 0 caveat once measured); LAN exposure confirmation dialog; ordered member list (drag/up/down like GPU rows) — manual: server picker from `clusterCandidates` (asks for the spec when a server has several); automatic: server + ordered GPU selection (from `serverHardware`), threads, cache, backend, estimates; warnings from `ClusterWarnings`; expected cold-start time hint; device-order and `-mmdev` hints.
- [ ] Steps: failing tests (toggle requires `${RPC_SERVERS}` hint; mode switch confirm; order → body positions; GPU selection order; ack dialog for direct+lan; each §3a client error; warnings render) → implement → pass → commit `feat(portal): cluster head editor`.

### Task E4: Table totals, status and actions

**Files:** `RuntimeAdminSection.tsx` (`vram_total`/`ram` cells show cluster total marked "cluster", own value beneath, ≥ for incomplete, per-member breakdown popover; status tab shows cluster progress members starting → ready → head starting → formed, held-by, breaches, protocol mismatch flag; Start/Stop on a head call the same endpoints, which the gateway routes to the orchestrator), `components/shared/runtimeActions.ts` (head rows: Stop drains even when pinned).
- [ ] Steps: failing tests (totals with an unknown member show ≥ and name it; CPU member adds RAM only; progress states from status frames; breach badge) → implement → pass → commit `feat(portal): cluster totals and status`.

### Task E5: Binaries tab and binary pickers

**Files:** create `src/components/BinariesSection.tsx` (+ test) as a new tab `'binaries'` in `RuntimeAdminSection.tsx` placed before `'limits'`; the spec form's binary field and the manual member editor (E2) become a picker over the server's inventory (path + version, "not allowed by the agent" marked), free text still accepted; the cluster editor (E3) shows each member's matched binary and version.
- [ ] Steps: failing tests (tab order; agent rows read-only except the version override; manual add/edit/delete; picker fills `binary`; free text still saves; unknown version label) → implement → pass → commit `feat(portal): binaries tab and binary pickers`.

---

## Phase F — integration, docs, finish

### Task F1: Real-cluster CI e2e

**Files:** `.github/workflows/ci.yml` (new job `e2e-rpc-cluster`, ubuntu, `actions/cache` keyed on the pinned commit for a CPU build `-DGGML_RPC=ON -DGGML_NATIVE=OFF`, targets `llama-server ggml-rpc-server`, `stories260K.gguf` cached from `huggingface.co/ggml-org/models/resolve/main/tinyllamas/stories260K.gguf`), `gateway/e2e/e2e-rpc-cluster/` (Go test harness or Playwright suite, following `e2e-runtime`): gateway (memory store) + two agents on one runner with distinct `OP_AGENT_*` config; `lan` network with `lan_address=127.0.0.1` on both servers; scenarios: (a) relay, head env mode; (b) direct, head args mode `--device ${DEVICES}` (CPU build → RPC devices only) with `-fa off`; each: request parks → cluster forms → completion returns text → member usage stored → stop cluster head first → members idle out; (c) member killed → head stopped by watchdog, request fails with a cluster code; (d) `-d` mismatch path not needed in CI.
- [ ] Steps: write the suite, run locally with the Stage 0 build (macOS) and in CI → green → commit `test(e2e): real llama.cpp RPC cluster in CI`.

### Task F2: Architecture docs and ADR-096

**Files:** `docs/architecture/09-architecture-decisions.md` (ADR-096: transport × network, defaults Q3, member modes, placeholder family + device-order rule, rulings P1/P4/P6/P8; ADR-084 table rule for cluster totals), `05-building-block-view.md` (rpcrelay, orchestrator, watchdog), `06-runtime-view.md` (members-first sequence), `07-deployment-view.md`, `11-risks-and-technical-debt.md` (lan exposure, DNS in the lan allowlist, Vulkan enumeration order follow-up, CPU member sizing, no per-process power), `12-glossary.md`, `cross-cutting/networking-mesh.md`, `security-auth-rbac.md` (consent, 404-no-leak, aggregated views), `persistence.md`, `routing-and-model-selection.md`, `telemetry-usage-observability.md`, `configuration.md`, `architecture-tests.md`, `reference/api-surface.md` (routes + error table), `reference/data-model.md`, `gateway/deploy/README-netbird.md` (manual relay rule, routing-peer warning), README feature claim.
- [ ] Steps: write; `make lint-docs` → OK → commit `docs: RPC cluster mode (ADR-096)`.

### Task F3: Whole-branch review, gates, cleanup, PR

- [ ] Whole-branch review (subagents, per area), fix loop.
- [ ] Full gates: both Go modules fmt/lint/vet/test (agent `-race`, `GOOS=windows go vet ./...`), backend Postgres leg, frontend test/build/lint/format, `make lint-docs`, `make sonar-scan` + `make sonar-findings && make sonar-branch-findings` (0 branch findings).
- [ ] Remove `docs/superpowers/` and `docs/implementation-status.md`; `git diff --name-only main...HEAD` shows neither.
- [ ] Merge `origin/main` if it moved (agent Version must stay one MINOR above main's); push over SSH; open the PR with a substantive body; enable Auto-fix.

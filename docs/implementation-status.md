# Implementation Status — agent-runtime-manager (branch-local, removed before PR)

## Current state

- 2026-08-25: Design brainstormed and approved section by section (architecture,
  feature negotiation/versioning, per-GPU data model, runtime/timeout/lifecycle
  behavior, portal UI + testing, WS realtime + config source).
- Spec written:
  `docs/superpowers/specs/2026-08-25-agent-runtime-manager-design.md`
  (self-reviewed; heartbeat mechanics, admission wait timeout, server_agent
  timeout default, and a test-table artifact fixed inline).
- Go test baseline in this worktree: green (`make test-go`, exit 0).
- Spec approved by the user (2026-08-25).
- Implementation plan written and self-reviewed:
  `docs/superpowers/plans/2026-08-25-agent-runtime-manager.md` — 24 tasks in
  five phases (store 1-4, gateway 5-10, agent 11-18, frontend 19-22,
  e2e+docs 23-24), grounded in five code-pattern briefs (exact signatures,
  registration points, test conventions).

## Context worth keeping on this branch

- A 125-finding verified cold-load/TTFT timeout inventory was produced with a
  multi-agent sweep (2026-08-25). Durable facts are folded into spec §8; the
  raw inventory lives outside the repo (session task output) — the durable
  home will be `docs/architecture/` when this branch documents §8.
- Two independent fixes were spun off as separate task suggestions (not on
  this branch): mislabeled `provider.unavailable` on native pre-header idle
  timeout; hardcoded 60 s `warmCallTimeout`.

## Execution progress (subagent-driven, per-task review)

Store phase COMPLETE (migrations 65/66/67 shipped, all verified on sqlite +
memory + a real PostgreSQL container):
- Task 1 — runtime spec + spec-GPU tables and repos (`c9ef6d6`)
- Task 2 — co-residency matrix repo (`f6ab0af`)
- Task 3 — per-GPU budgets + `ai_servers` runtime columns (`165a51c`)
- Task 4 — file-mode runtime reports (`f548d99`)

Gateway phase in progress:
- Task 5 — portal runtime-spec CRUD on mappings (`4bbd256`)
- Task 6 — co-residency matrix, GPU budgets, managed-runtime-only, warnings (`f9a89af`)
- Task 7 — agent endpoints: `GET /api/agent/v1/features` +
  `GET /api/agent/v1/runtime-config` (ETag), portal `AgentRuntimeConfig`
  assembly (`dc1c284`). Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-7-report.md`
  (carries a full sample JSON document for Tasks 16/17 to code against).
  One documented design call: a server with no `server_agent` application
  returns the FULLY empty document (router_listen 0, max_processes 0, empty
  gpu_budgets too) rather than a partially-populated one — revisit in Task 24
  if a later task needs GPU budgets visible before the application exists.
- Task 8 — WS `runtime_config` push + feature-gated delivery (`6f75feb`,
  reworked in `575d14a`, timeout/doc fix in `6c16caf`). Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-8-report.md`.
  `AgentStreamRegistry.NotifyRuntimeConfig` pushes the FULL runtime-config
  document (never a command/delta) to every open agent connection;
  `agentFeaturesRegistry`/`runtimeStatusRegistry` (new `runtime_registry.go`)
  gate delivery on the agent having declared `runtime_manager` and not being
  in file mode; `ingestTelemetrySample` now parses `capabilities` into the
  features registry; `Server.PushRuntimeConfig` bounds its
  `s.Portal.AgentRuntimeConfig` store read with a 5s timeout (never
  `context.Background()` unbounded — one goroutine is spawned per portal
  write, so an unbounded call could accumulate them under sustained write
  pressure).
  **main.go wiring, as actually shipped:** the brief's instruction
  (`portalService.SetRuntimeConfigChangedHook(...)` "in each of the three
  driver wirings") assumed a pre-CMP-1 main.go shape; this branch's actual
  `buildRuntime` consolidation leaves no in-scope `portalService` local at
  the one shared `gateway.New` call site. The setter is instead handed
  forward as a plain function value: `gateway.ServerDeps` carries a new
  `SetRuntimeConfigChangedHook func(func(serverID string))` field, set in
  `buildRuntime` (`deps.SetRuntimeConfigChangedHook =
  portalService.SetRuntimeConfigChangedHook`, right where `portalService` is
  already in scope building `ServerDeps.Portal`) and invoked in
  `buildGatewayServer` immediately after `gateway.New` returns
  (`deps.SetRuntimeConfigChangedHook(srv.PushRuntimeConfig)`). The `Server`
  itself never sees `*portal.Service`.
  **Tried and rejected: do not re-attempt.** An earlier version of this
  wiring added `portal.UnwrapService(api API) *Service`, which reached
  through the generated OTel tracing decorator (`api_tracing_gen.go`) to
  recover the concrete `*portal.Service` from `srv.Portal`. Review rejected
  it: it defeats the exact interface boundary `Portal portal.API` exists to
  enforce, breaks silently under a decorator change or a second layer of
  wrapping, and adds permanent public API surface whose only purpose was
  working around this one wiring-order gap. It has been deleted
  (`internal/portal/api_tracing.go` and its test no longer exist) — the
  `ServerDeps.SetRuntimeConfigChangedHook` field above is the sanctioned
  seam instead.
- Task 9 — runtime status ingest, VRAM write-back, file-mode report ingest
  (POST + WS), portal report view + live status SSE (`09d85f0`, `595affb`,
  `3942aa3`, `b5f33f7`, `9be204c`). Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-9-report.md`.
  `-race -timeout 20m` on `internal/gateway` reconfirmed clean at 803s,
  matching Task 8's baseline. New store method `RuntimeSpecByID`
  (conformance-tested on sqlite, memory, and a real local PostgreSQL
  container). Redaction of a file-mode report's `env` values is structural
  (re-parse into a fully-typed mirror of the runtime-config schema, mask,
  re-marshal — an unmodeled field is dropped for free), not a string scan.
  One deliberate scope expansion beyond the task's file list: wired
  `runtimeStatusRegistry.Retain` into `cmd/gateway/main.go` +
  `cmd/gateway/app_health.go`'s per-cycle pruning (an exported
  `gateway.NewRuntimeStatusRegistry()` + an anonymous-interface field on
  `agentRegistries`, since the concrete type is unexported) — the
  "OTHER HOUSE PATTERNS" note about per-server registry leaks turned out to
  have a small, worthwhile fix once traced through, see the task report's
  deviation #2 for the full rationale and the reviewer note.
  **Review round 1 fixes (separate commits):** a CRITICAL cross-server
  authorization gap in the VRAM write-back — `spec_id` is agent-supplied
  and nothing checked it belonged to the reporting server's own
  application, so server A's agent could overwrite server B's
  `vram_measured_mb` (which feeds B's OWN admission arithmetic via
  `agentRuntimeSpecDTO`). Fixed by resolving spec → mapping → application →
  `ServerID` and rejecting a mismatch; regression-tested by confirming the
  new test fails against the pre-fix code. Also fixed in the same round:
  the write-back loop is now length-capped
  (`maxRuntimeSamplesPerSample`/`maxRuntimeGPUsPerSample`) and memoizes
  EVERY resolution outcome including misses/errors (previously only a
  successful resolution was cached, so a repeated bad `spec_id` re-read
  every time — up to ~19k reads from one 1 MiB POST); and a file-mode
  report's `parse_error` is now redacted to its classification (text before
  the first `:`), not just length-clamped — a config-loader error routinely
  quotes the offending line, so an unparsed secret-bearing line could
  otherwise reach `server_runtime_reports` through this field instead of
  `env`. See the task report's "Review round 1" section for full detail,
  including two corrections agent-task authors should read before coding
  against the earlier JSON samples: the `runtime-report` example there
  omitted `work_dir`/`gpus`/`etag` (all three round-trip; a corrected full
  example is in that section), and — more importantly — **omitting
  `runtimes` from a telemetry sample is additive at the schema level but
  REPLACES the live status snapshot with empty at the behavior level**; an
  agent that only sends `runtimes` on a subset of its ~1s samples will make
  the portal's live runtime table visibly flicker empty between them.

Durable corrections discovered during execution (fold into docs/architecture in
Task 24):
- `binary` is a reserved word in PostgreSQL — the launch-spec column is
  `binary_path`; the Go field stays `RuntimeSpec.Binary`.
- Adding a column to `ai_servers` requires SEVEN sites in
  `internal/store/sqlite_routes.go`, not four: the insert, the update set-list,
  `AIServerByID`, `AIServers`, `ServersByOwner`, `ServersByAdminGroups`, and
  `scanAIServer`. `ServersByOwner`/`ServersByAdminGroups` carry their own inlined
  column lists.
- Store reads must return non-nil empty slices; a nil there becomes JSON `null`
  instead of `[]` for API clients. Two separate defects of this class were caught
  and fixed on this branch.
- A removal operation must never be gated by the same check that guards creation:
  gating `DeleteRuntimeSpec` on the application type stranded specs permanently
  once an application was retyped via the ordinary `UpdateApplication` path.
- `go test ./internal/gateway/ -race` needs `-timeout 20m` (not the 10m
  default) on this machine — the package is large and several tests seed a
  login user via bcrypt (deliberately expensive), which compounds under race
  instrumentation. A first attempt at the default timeout panics mid-run
  with no `DATA RACE` reported (a timeout, not a race/deadlock); the retry
  with `-timeout 20m` passes cleanly in ~800s. Pre-existing, unrelated to
  Task 8's changes — worth knowing before any later task re-runs this.

Gateway phase COMPLETE (Tasks 5-10):
- Task 5 — portal runtime-spec CRUD on mappings (`4bbd256`)
- Task 6 — co-residency matrix, GPU budgets, managed-runtime-only, warnings (`f9a89af`)
- Task 7 — agent `features` + `runtime-config` endpoints, ETag (`c3f0b08`)
- Task 8 — WS runtime_config push, feature-gated (`19f9633`)
- Task 9 — runtime status ingest, VRAM write-back, report ingest, portal SSE (`60d6e7c`)
- Task 10 — `server_agent` application type + cold-load-aware timeout (`618516c`)

Additional durable corrections from the gateway phase (fold into
docs/architecture in the documentation task):
- An agent endpoint's target server must come ONLY from the bearer token. The
  VRAM write-back initially took its target from the agent-supplied `spec_id`
  body field, which allowed an agent for server A to overwrite server B's spec —
  and because the pushed config prefers the measured value over the operator
  estimate, that changed the VRAM figure server B's agent did admission
  arithmetic against. Now resolved spec → mapping → application → server before
  every write.
- Ingest paths that fan out to the store need explicit bounds. The runtime
  status array is clamped, and the per-spec lookup memoizes every outcome
  (hit, miss, error, locked, cross-server), not just hits.
- Free-form error text from an agent must be redacted safe-by-default, not by a
  heuristic split: retain a leading classification token only when it looks like
  one (no whitespace, quotes or `=`, bounded), else emit a fixed constant.
- Runtime status, including any stderr tail, is volatile-only by policy — a
  model server's stderr can carry prompt fragments, which must not be persisted
  outside the opt-in payload capture.
- `Application.TimeoutMS` is a TOTAL request deadline, never reset by upstream
  activity, so the `server_agent` type defaults to 600000 ms; the stock 30000
  would fail every cold model load reproducibly.

Agent phase in progress:
- Task 11 — agent feature registry, `Version` 0.1.0 → 0.2.0, capabilities
  reporting (`42d420c`). (Not logged here at the time; backfilled during
  Task 12.)
- Task 12 — `server-agent/internal/runtime` package: wire-mirror types
  (`types.go`, `ParseConfig`), the visible-load-lifecycle state machine, and
  the pure `Admit` admission policy (`policy.go`), plus the `archtest`
  allowlist entry for the new package. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-12-report.md`.
  One documented judgment call: `archtest`'s `allowedDeps["internal/runtime"]`
  is an empty slice, not `{"internal/gwapi"}` as the task brief's file list
  suggested — `types.go`/`policy.go` are pure stdlib-only code and import
  nothing module-internal; the design doc's "imports only internal/gwapi"
  note describes the FULL future package (`config_client.go` et al., not yet
  written). The edge will be added in whichever later task actually adds the
  import.
- Task 12 review round 1 — coordinator review confirmed all 16 spec cases
  by hand-trace and found five fixable gaps, all addressed in the same
  commit: (1) the final victim sort was mutation-survivable (no case had 2+
  victims) -- added a multi-rule two-victim case; (2) `Admit` could
  self-evict a candidate already present in `Running` (a double-start
  race) -- fixed by filtering self out of the effective running set once,
  up front, ahead of all four rules (extended beyond the review's literal
  rule-1/rule-4-only scope after tracing the same bug through rules 2/3
  too); (3) a candidate whose own VRAM demand alone exceeds a GPU's budget
  used to return `Wait` forever instead of failing fast -- now returns a
  terminal `Decision{Reason: StateNotPermitted, Message: "..."}` (reused
  the existing state per the review's own preference so the portal badge
  mapping needs no new value; added `Decision.Message` to disambiguate
  from the OTHER not_permitted cause); (4) `sortOldestFirst` ties now break
  on SpecID for deterministic output; (5) new `Config.AllowedPairs()`
  canonicalizes `Coresident` pairs via `PairKey` so a consuming task cannot
  build a one-directional `Allowed` map by accident. Full detail, covering
  test names, and mutation-testing verification (including a correction to
  the original report's case-7 mutation description) appended to
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-12-report.md`.
- Task 12 review round 2 — round 1's Finding 5 came back NOT ADDRESSED,
  correctly: `Config.AllowedPairs()` existed and was tested, but nothing
  pointed a future implementer at it from `Coresident` or
  `PolicySnapshot.Allowed`'s own doc comments, so the one-directional-map
  trap it was meant to close stayed open in practice. Fixed with exactly
  the two doc comments asked for (no behavior change): `Coresident` now
  says explicitly not to hand-roll an `Allowed` map from it and names
  `AllowedPairs()`; `Allowed` now says to build it via
  `Config.AllowedPairs()`. Bundled minor: `assertDecision` (the shared
  `policy_test.go` helper) now also checks `Decision.Message`, verified
  discriminating by a mutation test (a leaked `Message` on the plain-OK
  path failed 7 table cases as expected, reverted after). Detail appended
  to the same task-12-report.md.

- Task 13 — `server-agent/internal/runtime/policy_local.go`: the
  agent-operator-controlled admission boundary. `LocalPolicy.Permit`
  refuses every spec when `AllowedBinaries` is empty (spec decision 2 — an
  unconfigured agent starts nothing), then matches `spec.Binary` exactly,
  then checks `spec.WorkDir` containment against `AllowedDirs` via
  `filepath.Clean` on both sides plus a separator-boundary comparison
  (rejects both `../` traversal and the sibling-prefix case, e.g.
  `/srv/models-evil` vs `/srv/models`). `ExpandPlaceholders` resolves
  `${PORT}` and `${AGENT_ENV:NAME}` in args/env (a missing `AGENT_ENV`
  variable is a hard error naming the variable, never an empty
  substitution) and builds the child's env from scratch: only the
  expanded spec env plus `PATH`/`HOME` from the agent's own environment
  (only if present) — never the agent's full environment, which holds its
  gateway bearer token and other models' secrets. `internal/config`
  gained the five `OP_AGENT_RUNTIME_*` settings (`RuntimeSource`,
  `RuntimeConfigPath`, `RuntimeAllowedBinaries`, `RuntimeAllowedDirs`,
  `RuntimeCachePath`) on the existing tri-source pattern. No `archtest`
  change needed — the new file is stdlib-only, and `internal/runtime`'s
  allowlist entry was already `{}`. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-13-report.md`.

- Task 14 — `server-agent/internal/runtime/manager.go`: the serialized-owner
  process supervisor (`NewManager`, `Apply`, `EnsureRunning`, `Status`,
  `LoadedModels`, `Transitions`, `SetMeasurer`, `Close`), consuming Tasks
  12-13's `Spec`/`Admit`/`LocalPolicy` verbatim. Core commit `9debc91`, three
  review-fix rounds `3cd107d`/`dd80c8e`/`28d4b00`. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-14-report.md`.
- Task 15 — `server-agent/internal/runtime/router.go`: `NewRouter(m
  *Manager) http.Handler`, the router port (health/`/running`/`/v1/models`
  plus the model-routed proxy with lazily-committed streaming heartbeats).
  Core commit `1b86c7e`, review fixes `dc4ed52`/`c23b82a`/`22f0e3a`. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-15-report.md`.
- Task 16 — `server-agent/internal/runtime/config_client.go`: `Source`
  interface, `GatewaySource` (ETag-conditional GET + disk cache +
  `ApplyPushed`), `FileSource` (mtime poll + `LastParseError`). Commit
  `a4f5f31`. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-16-report.md`.
- Task 17 — `server-agent/internal/runtime/features_client.go`/`report.go`,
  `internal/client/ws.go`'s `RuntimeUpdates()`/`PostRuntimeReport`: the
  gateway feature list client, the file-mode upward report builder
  (env-redacted), and the WS/POST sender plumbing. Commit `8cf4a4d`, atomic
  drain-then-send fix `68d46d6`. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-17-report.md`.

Agent phase COMPLETE (Tasks 11-18):
- Task 18 — `server-agent/internal/runtime/driver.go` (new): `Driver`, the
  top-level object wiring everything from Tasks 11-17 into the agent's main
  loop. `Sync` re-checks feature negotiation on every call (own local
  `runtimeManagerFeature` constant — `internal/runtime` cannot import
  `internal/agent`'s registry without an import cycle), loads the desired
  config (`GatewaySource.ApplyPushed` for a WS-pushed payload,
  `FileSource`/anything else via `Load`, `*FileSource` always ignoring a
  pushed payload per spec §10.2), and on change applies it to the manager,
  (re)binds the router listener (`StartRouter`, all-interfaces bind, torn
  down via `net/http.Server.Close`, idempotent on an unchanged port), and
  (file mode only) posts the redacted report. `internal/agent/agent.go`
  gained the symmetric `runtimeDriver`/`runtimeWaker`/
  `runtimeTransitionsWaker` seams (mirroring `certProxyDriver` exactly): a
  nil `Deps.RuntimeDriver` is a complete no-op (no ticker, no wake, no
  `runtimes` sample key) — pinned by
  `TestCollectOnceRuntimeNilOmitsRuntimesKey`, which asserts the marshaled
  JSON never contains the substring `"runtimes"`. `collectOnce` maps
  `driver.Status()` to the new `sample.RuntimeSample`/`RuntimeGPUSample`/
  `RuntimeErrorSample` types and sets `LoadedModels` authoritatively from
  the manager (`StateRunning` only), overriding the generic model-status
  lister. `collector/nvidia.go` gained `NewNvidiaComputeApps()` (nil
  without `nvidia-smi` on PATH — a hardware capability, not a negotiated
  feature), wired into the manager via `SetMeasurer` in `main.go`.
  `main.go` fetches gateway features once at startup (via the same
  `*FeaturesClient` the driver reuses), computes `runtimeActive` through
  `agent.ActiveFeatures`, and constructs the manager/source/driver only
  inside that branch. Two pre-existing debts paid off while in these files
  (both flagged by earlier reviews specifically for this task):
  `sample.EmptyCapabilities` is now a function (fresh `json.RawMessage`
  per call) instead of a shared package-level `var`; `runtime.Status`/
  `LastError` gained JSON tags plus a `Status.MarshalJSON` that normalizes
  a nil `MeasuredVRAM` to `{}` instead of Go's default `null`. `archtest`
  gained `internal/agent += internal/runtime` and `mainPkgKey +=
  internal/runtime`. Full test/verification detail, the exact `runtimes`
  sample JSON shape, and deviations:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-18-report.md`.

Agent phase COMPLETE (Tasks 11-18), server-agent module:
- Task 11 — feature registry, capabilities reporting, version 0.2.0 (`42d420c`)
- Task 12 — runtime config types + pure admission policy (`5adee5e`)
- Task 13 — local policy (allowlist, containment) + placeholder expansion (`cfcb98d`)
- Task 14 — process manager with a serialized admission owner (`28d4b00`)
- Task 15 — model router port with cold-load heartbeats (`22f0e3a`)
- Task 16 — config sources: gateway ETag + disk cache, and local file (`a4f5f31`)
- Task 17 — WS runtime_config push channel, features client, redacted report (`68d46d6`)
- Task 18 — driver, run-loop wiring, sample fields, VRAM measurer (`eefb222`)

Durable corrections from the agent phase (fold into docs/architecture in the
documentation task):
- `${AGENT_ENV:...}` must refuse the agent's own `OP_AGENT_*` namespace. Without
  that, a portal-authored launch spec could read the agent's gateway bearer token —
  which authenticates the certificate endpoint that issues a private key — and then
  act as that agent.
- A child process receives only its own expanded environment plus `PATH`/`HOME`; a
  spec env key of `PATH` or `HOME` is refused outright, because allowing an override
  would reopen the relative-binary resolution path the allowlist closes.
- "Config unchanged since last fetch" and "config already applied to the manager"
  are different facts. They diverge across a process restart once the ETag is
  persisted to disk, so the driver tracks applied-state separately. Conflating them
  meant the runtime worked only on a fresh agent's first-ever start.
- The router port authenticates nothing, so its bind host is operator-controlled
  (`OP_AGENT_RUNTIME_ROUTER_BIND`), defaulting to the mesh identity derived from the
  TLS leaf's SAN and only then to all interfaces, with a warning naming the setting.
- Feature negotiation must be continuous, not decided once at boot: a gateway that
  is down during agent startup must not disable the feature for the process
  lifetime.

Portal phase in progress:
- Task 19 — `gateway/frontend/src/api/runtime.ts` (new domain module): the
  mapping-scoped launch-spec CRUD, application-scoped co-residency +
  warnings, server-scoped GPU budgets + file-mode report view, and the live
  runtime-status SSE subscription. Three of the plan's original TypeScript
  interfaces were corrected against the Go DTOs while implementing. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-19-report.md`.
- Task 20 — `gateway/frontend/src/components/RuntimeAdminSection.tsx` (new):
  the operator screen for one `server_agent` application's runtime, wired
  into `ApplicationSection`'s drill-down (replaces `MappingSection` for that
  application type) and `ServerList`'s `managed_runtime_only` banner/
  auto-drill. Area 1 ("Launch specs") fully implemented; matrix/limits/status
  shipped as stubs for Tasks 21-22. Client-side placeholder-policy validation
  mirrors the agent's real `ExpandPlaceholders` rule exactly (round 2 fix).
  Commits `adf6865`, `8cf3729`. Reports:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-20-report.md`.
- Task 21 — `gateway/frontend/src/components/RuntimeMatrix.tsx` (new): the
  lower-triangle co-residency grid (canonical-order enforced on both read
  and write, advisory per-GPU-budget tooltip, never-blocking `disabled`
  prop). `RuntimeAdminSection`'s areas 2 ("Koresidenz-Matrix") and 3
  ("Runtime-Limits") replace their Task-20 placeholders: full-replace
  co-residency toggling with optimistic update, and per-GPU budget rows
  prefilled from the same live-telemetry hardware report `HardwareSection`
  already reads, plus a never-blocking UUID-drift warning and the
  `runtime_max_processes` field (saved via the general server PATCH).
  `PortalServer`/`UpdateServerRequest` gained an optional
  `runtime_max_processes` field. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-21-report.md`.

- Tasks 22 and 22b (live-status tab, then three review batches plus two extra
  items) are complete; their per-task detail lives in
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/progress.md` and the
  `task-22*` reports.

E2E phase:
- Task 23 — `gateway/e2e/playwright.runtime.config.ts`,
  `gateway/e2e/e2e-runtime/runtime.spec.ts`, and the new stdlib-only Go
  fixture module `gateway/e2e/e2e-runtime/fixtures/stubserver/` (`go.mod`,
  `main.go`, `main_test.go`), plus `"e2e:runtime"` in
  `gateway/e2e/package.json`. Six serial scenarios prove the cross-process
  circle with a REAL server-agent binary and REAL child processes: cold start
  on demand (with `starting` observed on the runtime SSE stream, not raced
  past), portal force-stop and restart-on-demand, and the admission
  arithmetic in both directions (a 1000 MB GPU-0 budget against two 700 MB
  specs evicts the idle one; raising it to 2000 MB and changing nothing else
  makes the same two co-resident). The stub model server is BUILT by the
  spec's `beforeAll` and never started by the harness — starting it is the
  agent's job, which is the property under test. Report:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-23-report.md`.

Durable facts found while writing Task 23 (for Task 24's docs):

- The runtime-config-changed hook obeys ONE rule, stated in full on
  `notifyRuntimeChanged` (`portal/service_runtime.go`) — **any successful write
  that CAN change a server's runtime-config document notifies that server's
  agent, and what decides it is the write path's own SCOPE (which row, and for
  an application-owned row whether that application is the server's
  `server_agent` one), never which field the request happened to change.**
  That doc comment enumerates the six kinds of row `AgentRuntimeConfig`
  derives the document from, maps each to its call sites, and lists the
  deliberate non-notifiers; check any new write path against it. Twelve call
  sites today:
  - the AI server row (`max_processes`): `UpdateServer` — unconditional for
    the server, gated on neither the field nor the server having a
    `server_agent` application (`PushRuntimeConfig` already fail-closes on "no
    `runtime_manager` agent connected" with a map lookup, before any read).
  - the `server_agent` application row (`router_listen`): `CreateApplication`,
    `UpdateApplication`, `DeleteApplication`, via
    `notifyRuntimeChangedForApplication(serverID, previousType, currentType)`,
    which fires when EITHER side of the write is `server_agent` (so retyping
    an application AWAY from it notifies too).
  - its mappings (a spec's `model`/`upstream_model`): `CreateMapping`,
    `UpdateMapping`, `DeleteMapping` and `reconcileApplicationModels` (the
    "Sync models" button + the background `model_sync` loop; one push per
    reconcile, and only when it wrote something), via
    `notifyRuntimeChangedForMapping(serverID, owningApplicationType)`. ONE
    type, not a pair: a mapping has no type and `UpdateMappingRequest` has no
    `application_id`, so a mapping cannot move between applications.
  - specs, co-residency, GPU budgets: `PutRuntimeSpec`, `DeleteRuntimeSpec`,
    `SetCoResidency`, `SetServerGPUBudgets` (the four original sites).

  Deliberate non-notifiers, all exemptions of a whole write PATH rather than
  of a field: writers whose signature confines them to columns outside the
  document (`persistApplicationSchemeSwitch`, `AgentProxyRoutes`' port
  assignment, `SetServerEnergyConfig`, the gateway's telemetry ingest), and
  the agent's own `UpdateRuntimeSpecGPUMeasured` writeback, which does change
  the document but changes it FROM the agent. Reports:
  `.superpowers/sdd/2026-08-25-agent-runtime-manager/task-23b-notify-report.md`
  (Task 23b closed the application row; its follow-up round closed the mapping
  rows and the server row, which had been the two known-open gaps — a mapping
  rename used to 404 at the agent's router under the new model name for up to
  a minute while the old name still routed, and `RuntimeMaxProcesses` reached
  the agent only on its 60 s `runtimePollInterval` backstop).
- With `OP_AGENT_RUNTIME_ALLOWED_DIRS` set, a spec with an EMPTY `work_dir` is
  refused outright (`LocalPolicy.Permit`). Configuring the agent's permitted
  directories therefore makes `work_dir` mandatory on every spec.
- No make target, CI job or Sonar source root enumerates the e2e fixture Go
  modules: `make lint`/`make test-go` cover `gateway/backend` and
  `server-agent` only, `.github/workflows/ci.yml` runs no e2e suite at all,
  and `sonar.sources` lists `gateway/backend,server-agent,gateway/frontend/src`.
  The new `stubserver` module needs no registration — the same posture as the
  existing `fakeacme` and `mailcatcher` fixtures.

## Next planned step

1. Task 24 (docs + Sonar gate + working-file cleanup before the PR). The e2e
   suite catalogue that needs `e2e:runtime` added is
   `docs/architecture/cross-cutting/development-and-quality.md` §"e2e" (the
   `e2e:agent` … `e2e:system-admin-mode` list, currently lines 127-132).

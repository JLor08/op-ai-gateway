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

## Next planned step

1. Continue the plan at Task 14.
2. Remaining: 14-18 (agent), 19-22 (portal UI),
   23 (e2e), 24 (docs + Sonar gate + working-file cleanup before the PR).

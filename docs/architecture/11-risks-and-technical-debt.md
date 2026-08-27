# 11. Risks & Technical Debt

Known risks and areas that warrant care. Documented so they are visible, not
hidden.

## 11.1 Operational risks

| Risk | Impact | Mitigation / status |
|---|---|---|
| **NetBird sidecar is a SPOF** when it shares the gateway's network namespace | A sidecar crash can 502 the whole public API (including login) | Prefer the no-netbird deployment where the mesh is not needed; monitor the sidecar; the wrapper enrolls via a key file to reduce misconfiguration. |
| **PostgreSQL narrow-type truncation** surfaces only on real PostgreSQL | Silent data truncation/precision loss if a column type is too narrow | Wide types are mandated (`bigint`/`double precision`); run the conformance suite against PostgreSQL before shipping store changes (see [ADR-005](09-architecture-decisions.md#adr-005--postgresql-needs-wide-column-types)). |
| **Payload capture holds plaintext in RAM** in the volatile mode | OS swap or a core dump could place prompt/response plaintext on disk | Documented as out of scope; the guarantee is "never written to disk by the application" (see [ADR-008](09-architecture-decisions.md#adr-008--payload-capture-opt-in-encrypted-or-volatile-redacted)). |
| **Privileged telemetry sources absent** when the agent runs unprivileged | Some power/temperature/per-DIMM data is unavailable | By design; the agent degrades gracefully and reports what it can. |
| **Public ACME / SMTP / OTLP unavailable** in air-gapped setups | Edge certs, e-mail invites, and trace export are unavailable | All are opt-in; the system runs without them. |
| **The agent is a SPOF for its server's managed models** ([agent-managed model runtime](cross-cutting/agent-runtime-manager.md)) | If the agent dies, every model it manages becomes unreachable — the router is the only way in | Accepted by construction; the mitigation is the migration path, since classic non-managed applications may coexist on the same server, so llama-swap and the managed runtime can run side by side. |
| **A portal admin with server-management rights effectively chooses what runs on the AI servers** | Portal write access selects binary, argv, env and working directory for a process on the AI server | Bounded **only** by the agent-local binary allowlist, which is operator-controlled on each host and starts nothing when empty ([ADR-024](09-architecture-decisions.md#adr-024--managed-runtime-the-gateway-specifies-the-launch-the-ai-server-permits-it)). Enabling the feature is an explicit act per host and the gateway cannot widen it. |
| **The agent's model-runtime router authenticates nothing, and its shipped default binds all interfaces** | Every managed model on that host is reachable from any interface that can reach the port | `runtime_router_bind` / `OP_AGENT_RUNTIME_ROUTER_BIND` is the operator's control (mesh IP, or `127.0.0.1`). The empty-value fallback derives a mesh address from the installed leaf certificate, but that requires `cert_mode != off` **and** an installed certificate, and the portal's generated agent config ships `cert_mode: "off"` — so **the shipped default always falls through to all interfaces**, logged at Warn. Set it explicitly. |
| **A secret placed in a launch spec's `args` reaches the gateway unmasked** | The upward file-mode report masks `env` values only, while placeholder expansion resolves `${AGENT_ENV:NAME}` in `args` too | Reviewed and upheld as spec-correct: the wire contract scopes redaction to `env`. **Operator guidance: put secrets in `env`, never in `args`.** |
| **`work_dir` containment for managed processes is lexical** — symlinks under an allowed directory can point outside it | A spec could run an allowlisted binary with its working directory effectively outside the permitted tree | Accepted. The security boundary is `runtime_allowed_binaries` (an exact absolute-path match against the agent's own local list), so a gateway-supplied spec cannot choose *what* executes; `work_dir` only decides where an already-allowlisted binary resolves relative paths, and an attacker able to plant symlinks there already has local write access. `filepath.EvalSymlinks` was **rejected deliberately, not overlooked**: it resolves at *check* time while `os/exec` applies the directory at *start* time, so anything able to rewrite the link between those moments defeats the check **while making it look enforced** — strictly worse than an honest lexical check, because it invites treating containment as a boundary. It would also break the legitimate case of an allowed directory that is itself a symlink to a mounted volume. If containment ever has to *be* a boundary, the non-TOCTOU direction is `openat`/`O_NOFOLLOW` resolution plus `fchdir` in the child, which `exec.Cmd.Dir` cannot express portably. |
| **Windows managed-process stop is kill-only** | On a Windows AI server a stop terminates the child rather than letting it finish in-flight work — there is no graceful-drain equivalent | Accepted for now; the code compiles and looks complete, so the gap is only discoverable by reading the platform file. A real fix needs `CREATE_NEW_PROCESS_GROUP` plus `GenerateConsoleCtrlEvent`. CI only checks that the Windows build compiles. |
| **Volatile runtime status loses `last_error` on an agent restart** | The last load failure for a managed model is gone after the agent restarts | Honest and documented, not a defect: gateway-side status is deliberately never persisted (a stderr tail can carry prompt fragments, which the capture policy forbids at rest), and a gateway restart self-heals within one ~1 s sample. Only an *agent* restart loses it permanently. |
| **The runtime report is fetched once per (api, server), with no polling refresh** | A file-mode agent's re-report after a config-file edit is only picked up on a remount or navigation | Deliberate split: the report is navigation-fresh, live status rides the SSE stream. Switching to a polled fetch is a small change if operators need it. |
| **A pairwise co-residency matrix plus per-GPU arithmetic cannot express every constraint** | Exotic interconnect or bandwidth contention is not modelled | Accepted: the matrix's role is operator *intent*, which covers the known non-VRAM cases (PCIe bandwidth, system RAM, CPU contention) that nobody can compute. |
| **File mode offers no remote operations, and gateway-side specs become invisible while it is active** | An operator cannot view or delete the now-ineffective gateway-side runtime specs from the portal | By design — whoever owns the configuration owns the operations, and the file-mode view is display-only. Operator guidance: switch the agent's source back to `gateway` to inspect or remove that dormant configuration. |
| **There is no one-shot "retry this model" action** | For a `stopped` row carrying a `last_error` — the headline failure case — the only affordance is `force_running`, a **persistent** override that must be cleared afterwards | A limitation of the state-shaped runtime API ([ADR-026](09-architecture-decisions.md#adr-026--gatewayagent-control-is-desired-state-not-commands)), not of the screen; restart is unavailable there because a `force_stopped` write with no live process does nothing at all. |
| **Two `server_agent` applications on one server make both un-editable from the portal** | Saving *any* field change sends `type: "server_agent"`, the uniqueness gate finds the other row, and the request is refused with a 409 naming a condition the operator did not try to create | Only reachable on a pre-invariant development database (the type is unwritable in every released version). The remediation is deleting one application, not editing around it. Related: the service-layer gate has a real TOCTOU — it reads, releases the lock, then writes with no transaction, so two concurrent creates can both pass it (reproduced 2 times in 200) — and the backstops that catch the race loser are migration 68's index on SQL and an explicit locked check in the memory store. **The service gate is not race-free and must not be described as such**, nor either backstop removed as "redundant with it". If that index ever does fire, the SQL store surfaces a bare conflict which maps to the misleading `application.port_conflict`; distinguishing which unique constraint failed would require parsing dialect-specific error text. |
| **`runtime_max_processes` on the runtime screen is never re-seeded from props** | A value another admin changed meanwhile is overwritten on Save | Left as-is deliberately: this is the ordinary controlled-form-draft-versus-prop tension the whole portal shares, not an instance of the load-gate class the same screen hardened against. |
| **The models list does not surface agent-managed runtime state** | "Currently loading" and "last load failed" are visible only on the runtime admin screen | Named rather than fixed: surfacing them would need new plumbing (either the model DTO or a new props path carrying per-mapping runtime state), so the gap is not a five-minute change. |

## 11.2 Design tensions to respect

- **CGO-free build vs. richer native libraries.** The static, CGO-free build
  forbids CGo-only dependencies. Server-side image transcoding (e.g. HEIC via
  libvips/libheif) would reintroduce CGo — HEIC is handled client-side instead.
- **Two enforcement surfaces.** The public and mesh listeners must be kept
  independent; per-listener behavior is threaded through request context rather
  than inferred, to avoid one surface's assumptions leaking into the other.
- **Reconcile vs. transient errors.** Reconcilers must distinguish a transient
  dependency error (keep current) from a definitive empty result (tear down);
  getting this wrong strands or destroys healthy resources
  ([ADR-022](09-architecture-decisions.md#adr-022--reconcile-loops-keep-healthy-resources-on-transient-errors)).
- **Conditional role-scopes.** Authorization flags must be read from an
  always-present role-scope; making a scope conditional (e.g. step-up) can break
  self-service paths that read it as a flag
  ([ADR-021](09-architecture-decisions.md#adr-021--layered-rbac-with-delegation-and-step-up)).

## 11.3 Testing blind spots to remember

- The memory/in-memory driver does not enforce foreign keys and has no usage-event
  store; FK-violation and usage-aggregation bugs are invisible there — test those
  paths against SQLite/PostgreSQL and via the store conformance suite.
- A nil Go slice in a DTO marshals to JSON `null` and can crash a TypeScript
  frontend; list DTO fields should be initialized to empty slices, and real
  backend↔frontend end-to-end coverage catches what unit tests and mocks mask.
  Two shapes produce nil where the author expected empty and need explicit
  guards — `append([]T(nil), src...)` when there is nothing to append, and
  `json.Unmarshal([]byte("null"), &m)` — and a conformance assertion must test
  `x == nil` explicitly, not merely `len(x) == 0`, or it cannot see either.
- **No Playwright scenario suite runs in CI**, so a pull request can pass without
  any of them; the e2e fixture Go modules are likewise outside `make lint`,
  `make test-go` and Sonar's sources. See
  [Development Tooling & Quality Gates §6](cross-cutting/development-and-quality.md).
- **A comment that names a collaborator or asserts a cross-layer guarantee is a
  claim to verify, not documentation** — several have shipped false, and a
  guarantee that differs per driver must be stated per driver. The instances and
  the corollary ("no test can fail on this line" is an invitation to delete a
  load-bearing guard) are in
  [Development Tooling & Quality Gates §9](cross-cutting/development-and-quality.md).
- **`agentFeaturesRegistry` is a `Retain`-pruned per-server registry.** It is in
  `cmd/gateway`'s prune bundle alongside `runtimeStatusRegistry`, and a
  mutation-proof wiring test pins that both sides hold the same instance — but the
  trap it was built to avoid is silent: an unexported constructor lets
  `gateway.New` default-construct an instance `cmd/gateway` never sees, so a
  correct `Retain` runs against the wrong object and prunes nothing, with
  everything compiling and the prune apparently running.

## 11.4 Deliberate design acceptances

These structures were reviewed against SOLID/SoC/DRY/KISS/coupling principles
and **deliberately kept as they are**. They are decisions, not oversights —
recorded here so they are not re-litigated (or "fixed") without new evidence.

| Structure | Decision & rationale |
|---|---|
| **`internal/store/migrate.go` as one append-only file** (68 migrations and counting) | A legitimate ledger: clean runner, documented `rawUp` escape hatch, well-commented entries. Splitting per-version would churn history for no changeability gain. New columns use `addColumnIfMissing`; the baseline is frozen (no back-porting — fresh installs replay the full chain). |
| **`gateway.Server` as one type** | Splitting the type would be high-risk for little gain (nil-safe-registry convention, in-package tests). Cohesion is managed instead via per-feature *files*, named sub-structs for field clusters, and the generic `settingCache[T]`/`warnThrottle` helpers. |
| **`portal.API` (generated) and `routing.Store` as wide facades** | `portal.API` is interfacer-generated from `portal.Service` and stays whole; `routing.Store` is a composition of role sub-interfaces but keeps the full embedding for existing consumers. Narrowing happens **consumer-side** (e.g. `resolverStore`, `PrincipalLimitStore`, `agentIngestPortal`/`certPortal`/`settingsPortal`). Note: the `interfacer` regeneration of `api.go` is broken (self-import bug) — signature changes are hand-edited there; only `api_tracing_gen.go` regenerates via `go generate` (gowrap). |
| **i18n as TypeScript modules, not JSON resources** | JSON would forfeit compile-time key parity and cannot carry function-valued messages. `PortalMessages = typeof de` keeps the compiler enforcing exact de/en parity. |
| **No generic CRUD/dialog hook in the portal frontend** | The views' mode/confirm shapes legitimately diverge (string-id vs object targets, 2–10 modes); one API would invalidate large test suites for little gain. Shared pieces exist where they are truly identical (`listTableLabels`, `ModelOverrideEditor`, `AdminGroupPicker`/`Editor`, `useColumnSettings`, `useLatestFetch`/`usePolledFetch`). |
| **`LineChart` and `UptimeTimeline` stay separate** | ~20 shared lines vs fundamentally different hover models; a shared chart scaffold would be a leaky abstraction. |
| **Two atomic-file writers in the server-agent** (`trust` vs `certinstall`) | Different semantics by design: single-file backup/rollback + `os.Root` validation vs staged multi-file batch. Cross-referenced in doc comments, not merged. |
| **`Activity`'s bespoke SSE guard machinery** | Its asymmetric bump-and-release protocol is unique; forcing it into the shared fetch hooks would complicate both. It lives isolated in `useActivityData`. |
| **The routing scorer** | The viability gate before the bounded tiebreak is structural and test-guarded; adding a scoring term is a local change (shared `scoringRoute` constructor feeds both real routing and the UI ranking). |
| **`agentDNS` cache separate from `settingCache[T]`** | It deliberately holds its mutex across the resolve to single-flight concurrent NetBird lookups; the generic cache releases the lock during load. |
| **Agent-listener state machine in `cmd/gateway/agent_listener.go` (package main)** | Deliberately not an `internal/` package: no second consumer exists, and its tests depend on a NetBird test fixture shared with the sync loop's tests. Promote only when a concrete reuse appears. |
| **Hand-rolled sub-routing in the portal item handlers** | Migrating to Go 1.22 ServeMux patterns is low-value and carries precedence/trailing-slash risk; the current branches are mutually exclusive. Revisit only when a sub-resource is next added. |
| **`portal.Service` keeps its methods; state is grouped** | Certificate and NetBird state live in `certState`/`netbirdState` sub-structs (lock scopes unchanged). The fuller extraction into standalone manager types is deliberately deferred until a concrete need (e.g. reuse outside `Service`) exists. |
| **`UpdateSystemSettings` keeps its sequential write path** | The drift hazard (a key missing from the `touches*` reconcile predicates) is guarded structurally instead: single-source per-domain field lists plus a test that forces every new request field to be classified. A full descriptor-registry rewrite of the write path was judged risk-heavy for the remaining gain. |
| **A trailing empty launch argument is unrepresentable in the runtime spec editor** | `parseArgsText` pops one trailing blank line so an accidental Enter is not treated as a new empty argument. Consequence: `args: ['--foo','','--bar','']` re-saved without editing PUTs `['--foo','','--bar']` — an **internal** blank survives, a **trailing** one silently disappears. No field hint was added, on the grounds that an operator typing a trailing blank line is overwhelmingly in the accidental-Enter case. It looks exactly like a bug to the next reader, who would "fix" it and reintroduce phantom empty arguments on every accidental Enter. |
| **Measured per-process VRAM is admission-driven, not polled** | It is refreshed only when an admission decision happens to run while a VRAM measurer is installed, so a long-idle host reports stale measurements. Polling it would put an external `nvidia-smi` invocation on a timer against the manager's single serialized owner goroutine, which is the exact cost the retry-interval cache elsewhere exists to bound. |
| **An ephemeral listen port for a managed child has a TOCTOU window** | When a spec does not pin `listen_port` the agent binds `127.0.0.1:0`, reads the port back and closes the listener, so another process could claim it before the child's own bind. Accepted: the alternative is passing an inherited socket, which no supported model server's CLI accepts. |
| **`resolveGroupOnce` keeps its admission-queue loop inline** | The group-resolution body was cut down when the selection settings landed (cognitive complexity 75 → 32: the pin check and the `climb_up` dance moved into `pinnedMember`/`climbTarget`, and the per-request inputs became one struct). The remaining 32 is dominated by the admission-queue loop itself (~24 points), which manages an absolute wall-clock deadline, per-member queue timeouts and liveness re-checks — restructuring it is a change to pre-existing routing behavior with no relation to the settings, so it was deliberately left intact rather than "fixed" in passing. Consequence: the local Sonar gate reports one S3776 finding for this function whenever the branch that touched it counts those lines as new code. Revisit as its own change, with its own tests and review, if the loop needs to grow. |

Dependency direction and layer boundaries are enforced by architecture tests
(see [Architecture Tests](cross-cutting/architecture-tests.md)); the
memory-vs-SQL parity of dual store implementations is enforced by conformance
harnesses (`internal/store/routing_store_conformance_test.go`,
`internal/portal/store_conformance_test.go`).

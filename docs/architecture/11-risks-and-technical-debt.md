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

## 11.4 Deliberate design acceptances

These structures were reviewed against SOLID/SoC/DRY/KISS/coupling principles
and **deliberately kept as they are**. They are decisions, not oversights —
recorded here so they are not re-litigated (or "fixed") without new evidence.

| Structure | Decision & rationale |
|---|---|
| **`internal/store/migrate.go` as one append-only file** (60+ migrations) | A legitimate ledger: clean runner, documented `rawUp` escape hatch, well-commented entries. Splitting per-version would churn history for no changeability gain. New columns use `addColumnIfMissing`; the baseline is frozen (no back-porting — fresh installs replay the full chain). |
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

Dependency direction and layer boundaries are enforced by architecture tests
(see [Architecture Tests](cross-cutting/architecture-tests.md)); the
memory-vs-SQL parity of dual store implementations is enforced by conformance
harnesses (`internal/store/routing_store_conformance_test.go`,
`internal/portal/store_conformance_test.go`).

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 OnPrem AI Gateway contributors -->

# Development Tooling & Quality Gates

How the codebase is built, formatted, linted, tested, and quality-gated.
Quality is enforced in layers: formatting/lint (pre-commit + CI) → unit and
integration tests (CI) → architecture tests (part of the normal test runs) →
end-to-end suites (on demand) → a local SonarQube gate (agent-runnable, on
demand). Everything below is runnable headlessly by a coding agent.

## 1. Toolchain baseline

| Tool | Version | Used for |
|---|---|---|
| Go | 1.26+ | `gateway/backend` (`op-ai-gateway`, declares go 1.25.0) and `server-agent` (`op-ai-server-agent`, declares go 1.26) |
| Node.js / npm | Node 22 | `gateway/frontend` (React/TS/Vite) and `gateway/e2e` (Playwright) |
| golangci-lint | v2 (pinned in CI, currently v2.13.1) | Go formatting (`fmt`, gofumpt-based) + linting, both modules; CI pins Go 1.26 |
| Docker | any recent | Deploy images, the Postgres test env, the local SonarQube server |

## 2. Branching & pull requests

`main` is protected by convention: **nobody commits to or merges into `main`
directly** — including automated coding agents. Every change reaches `main`
through a **feature branch and a pull request**, merged by a human reviewer
once CI is green. Squash merges are preferred so branch-local working files
never enter `main`'s history.

Branch-local working documents — design specs and plans under
`docs/superpowers/` and the handoff file `docs/implementation-status.md` —
may be created and used freely while a branch is in progress, but they are
**removed as the last step before the pull request is opened**: everything
durable from them (design decisions, behavior, constraints) is folded into
this architecture documentation first. `main` never contains these files;
a PR diff must not add or modify them.

A pull request that changes what the public `README.md` shows or describes —
UI visible in its screenshots (`docs/images/`), commands, endpoints, setup
steps, feature claims — updates the README and regenerates the affected
screenshots in the same branch.

## 3. Make targets (repo root)

| Target | What it does |
|---|---|
| `make test` | `test-go` + `test-ui` |
| `make test-go` | `go test ./...` in `gateway/backend` |
| `make test-ui` | `vitest run` in `gateway/frontend` |
| `make test-e2e` | The base Playwright suite in `gateway/e2e` |
| `make fmt` / `make lint` / `make lint-fix` | `golangci-lint fmt` / `run` / `run --fix` across both Go modules |
| `make lint-install` | Installs the pinned golangci-lint |
| `make hooks` | `git config core.hooksPath scripts/git-hooks` (enables the pre-commit hook) |
| `make dev` | Local full-stack dev run (gateway in memory mode + `vite dev`, health-gated) via `scripts/dev.sh` |
| `make run-gateway` | Runs the gateway alone |
| `make sonar-up` … `sonar-down` | The local SonarQube gate — see [§7](#7-sonarqube-quality-gate-local) |

`server-agent` is its own module: build/test with `go build ./...` /
`go test ./...` from `server-agent/`.

## 4. Formatting & linting

- **Go**: one shared `.golangci.yml` (v2 config) at the repo root, run **per
  module** (`cd gateway/backend && golangci-lint run`, likewise
  `server-agent`). Formatting is `golangci-lint fmt` (gofumpt-based);
  `golangci-lint fmt --diff` is the check form used by CI and the hook.
- **Frontend**: ESLint (flat config, `gateway/frontend/eslint.config.js`) and
  Prettier, via `npm run lint` / `lint:fix` / `format` / `format:check`. The
  frontend tooling ignores `.worktrees/` (ESLint ignores, Vitest excludes,
  `.prettierignore`) so linked git worktrees under the repo root never leak
  into lint/test runs.
- **Pre-commit hook** (`scripts/git-hooks/pre-commit`, enabled by
  `make hooks`): when Go files are staged, runs format-check + lint on both
  modules; skip a single commit with `--no-verify`.
- **Frontend bundle layout** (`gateway/frontend/vite.config.ts`): the
  production build splits the always-loaded vendor libraries (`mui`,
  `vendor`) from the app chunk via `manualChunks` for browser-cache
  stability; `heic-to` (~3 MB, inlined libheif WASM) is reached only via
  dynamic import and stays a lazy chunk. `chunkSizeWarningLimit` sits just
  above that known decoder chunk — if `vite build` warns about chunk size
  again, a chunk outgrew the decoder; treat it as a regression instead of
  raising the limit.

## 5. Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request:

- **Go job** (backend + server-agent, each in its own working directory):
  `golangci-lint fmt --diff`, `golangci-lint run`, `go test ./...`.
- **Frontend job**: `npm ci`, `npm run format:check`, `npm run lint`,
  `npm run build` (the type-checked build is the i18n de/en parity guard —
  `en: PortalMessages` fails to compile on a missing or excess key), and
  `npm test`.

## 6. Test suites

- **Go unit + integration tests** across all backend and server-agent
  packages. Provider adapter tests use `httptest.NewServer`, so sandboxed
  environments need permission to open loopback listeners. A
  `go test -race ./internal/gateway` run takes ~11 minutes — longer than Go's
  default 10-minute per-package timeout — so race runs there need
  `-timeout=25m` (a timeout failure at 600s is the deadline, not a hang). The
  cause is 151 `seedLoginUser` call sites each performing a real bcrypt hash,
  and bcrypt is several times slower under the race detector: the same package
  takes 86–103 s **without** `-race` and reports **zero** data races. CI does not
  pass `-race`, so this bites only when someone adds a race leg — and the
  goroutine dump points squarely at `bcrypt.GenerateFromPassword`, not at a lock
  cycle. The honest fix is a cheaper test-only bcrypt cost for those seeds.
- **Store conformance suite**
  (`gateway/backend/internal/store/conformance_test.go`): the store contract
  runs against the in-memory store and SQLite always, and against PostgreSQL
  when `OP_AI_GATEWAY_TEST_POSTGRES_DSN` is set. **There are two harnesses and
  they answer different questions** — `forEachDialect` (sqlite + postgres) can
  never see a memory-vs-SQL divergence, `forEachRoutingStore` (memory + sqlite)
  can never see a dialect one. When the property under test is "this constraint
  is enforced", assert it on the **driver** axis; use the dialect axis for
  SQL-generation and dialect-semantics questions. See
  [Persistence §5.1](persistence.md) for that rule, the test *shapes* that have
  each shipped in a form that could not fail, and the two operational rules for
  the PostgreSQL leg (it skips **silently** without a DSN, and the package's
  tests must not run concurrently against one database). CI sets it: the Go job runs a
  `postgres:17-alpine` service (matching the [deployment
  view](../07-deployment-view.md)) so the PostgreSQL leg runs on every push.
  It has not always — while that leg was skipped, a `real` column reached
  `main` and stayed there, which is what SQLite masks and ADR-005 warns about
  (see [Persistence](persistence.md)). Run it Postgres-backed locally too when
  you change the store, rather than discovering it in CI. Note that memory mode enforces
  no foreign keys and keeps no persistent usage store, so FK- and
  usage-aggregation behavior must be tested SQLite-backed or via the
  conformance suite, not via memory-mode e2e.
- **Frontend Vitest**: always run the **full** suite (`npx vitest run`) for
  component changes — `App.test.tsx` carries cross-cutting guards (derived
  de/en i18n key parity, per-row table cell counts) that component-scoped
  runs miss. `npm run test:coverage` produces the lcov report the Sonar gate
  imports.
- **Architecture tests**: dependency-rule tests in both Go modules
  (`internal/archtest`) and the frontend (`src/arch.test.ts`) run as part of
  the normal `go test` / `vitest` invocations — see
  [Architecture Tests](architecture-tests.md).
- **Playwright end-to-end**: the base suite (`make test-e2e`) drives the real
  built portal against the real gateway. Scenario-specific suites live in
  `gateway/e2e` as npm scripts, one Playwright config each:
  `e2e:agent` (real server-agent binary), `e2e:capture` / `e2e:capture-ram`,
  `e2e:certificates`, `e2e:active`, `e2e:health` / `e2e:health-modelsync`,
  `e2e:groups`, `e2e:limits`, `e2e:logs`, `e2e:perf`, `e2e:projects`,
  `e2e:resource-groups`,
  `e2e:runtime` (agent-managed model runtime end to end: the REAL server-agent
  binary starts REAL child processes from a stub model server module in
  `gateway/e2e/e2e-runtime/fixtures/stubserver`, which the suite builds but
  never starts), `e2e:services`, `e2e:servers`, `e2e:smtp` (local
  mail catcher module in `gateway/e2e/e2e-smtp/mailcatcher`), `e2e:totp`,
  and `e2e:system-admin-mode`. These suites run the gateway in **memory
  mode** — fast, but see the conformance-suite caveat above for what memory
  mode cannot catch. Changes to invite/auth UI or visibility models are
  expected to require matching spec updates; plan the e2e rewrite as part of
  such a change.

> **No Playwright suite runs in CI.** `.github/workflows/ci.yml` has only the
> Go job and the frontend job, `make test-e2e` runs the base suite alone, and
> nothing in the Makefile enumerates the scenario suites — so every one of them,
> `e2e:runtime` included, is a **local-only gate a pull request can pass without
> running**. Likewise the three e2e fixture Go modules
> (`e2e-certificates/fakeacme`, `e2e-smtp/mailcatcher`,
> `e2e-runtime/fixtures/stubserver`) are outside `make lint`/`make fmt`/`make
> test-go` (which enumerate `gateway/backend` and `server-agent` only) and
> outside `sonar.sources`, so they are formatted, vetted and linted only by hand
> in the module directory — which is also why a new fixture module needs no
> registration anywhere. A reader who sees a rich suite catalogue reasonably
> assumes CI enforces it; knowing it does not changes both review expectations
> and any decision to invest in these suites.

### 6.1 `e2e:runtime` — the one suite with real child processes

Config `gateway/e2e/playwright.runtime.config.ts`, spec
`gateway/e2e/e2e-runtime/runtime.spec.ts`. Six serial scenarios exercise the
whole cross-process circle with a real agent: cold start on demand with the
`starting` state actually **observed** on the runtime SSE stream rather than
raced past; portal force-stop and restart-on-demand; and the admission
arithmetic in both directions.

**The structural point, which a well-meant tidy-up would destroy.** The stub
model server is **built** by the spec's `beforeAll` and **never started** by the
harness: its absolute path goes on the agent's
`OP_AGENT_RUNTIME_ALLOWED_BINARIES`, and `playwright.runtime.config.ts`
deliberately has **no `webServer` entry** for it, unlike `e2e:certificates`'
`fakeacme`. Every stub process that runs during the suite was `exec`'d by the
real server-agent — visible in the run output as the agent's own
`runtime: process starting … binary=… pid=…` lines. Adding a `webServer` entry
"to match the other suites" would keep every test green while removing the only
end-to-end proof that the agent starts processes at all.

The suite also builds and spawns the real `server-agent` **from inside the test
body** rather than from the Playwright config, because the agent needs an agent
token minted for the AI server the suite itself creates through the portal — and
that server does not exist until the test body runs. It is started with
`OP_AGENT_RUNTIME_SOURCE=gateway`, both allowlists,
`OP_AGENT_RUNTIME_ROUTER_BIND=127.0.0.1` and a short `OP_AGENT_INTERVAL`;
`afterAll` **waits for the agent process's `exit` event** (bounded) rather than
assuming SIGTERM landed, so no stub child outlives the suite on a developer's
machine.

**The stub's three flags are load-bearing, not scaffolding.** `-port` (required,
loopback only) is what `${PORT}` in a spec's args resolves to. `-ready-after`
holds **both** `GET /health` **and** `POST /v1/chat/completions` at 503 for that
long after exec — gating the completions path too is deliberate, since a real
model server cannot answer before its weights load and a readiness gate covering
only `/health` would be decorative. `-tag` is echoed in the completion and
exposed with a served-completion counter on `GET /stats`; **that tag is the
suite's only means of proving which child answered**, which the eviction and
co-residency scenarios depend on entirely. A SIGTERM/SIGINT handler shuts the
listener down promptly, so eviction and force-stop steps measure the admission
machinery rather than the SIGKILL grace period.

**The two admission scenarios are one assertion, and neither may be deleted as
redundant.** With the co-residency pair permitted, an eviction produced by the
VRAM-sum arithmetic is observationally identical to one produced by the matrix
veto; the second scenario — raise the GPU-0 budget from 1000 MB to 2000 MB and
change *nothing else*, so the same two specs become co-resident — is what pins
the first on the arithmetic. Two mutation runs establish it: removing the
co-residency toggle leaves scenarios 1–5 green and fails only scenario 6;
widening the tight budget fails only scenario 5.

Three determinism rules the suite follows, each of which looks like a stylistic
quirk and is not:

- **Any polling loop over admission must drive an inference for every model
  involved on every retry round, and the scenario order must go tight-budget
  first, then roomy.** This is a property of the manager: admission is
  re-evaluated only when a request arrives for a spec that is *not currently
  running*, so a loop driving only one model cannot converge, and a "shrink"
  scenario run after both processes are already up can never converge at all.
- **No `sleep`, `waitForTimeout` or fixed delay anywhere** — every wait is
  `expect.poll` or an auto-retrying locator assertion.
- **The health/running helpers return transport failures as values instead of
  throwing**, because `expect.poll` aborts on a throwing callback rather than
  retrying it.

Two things the suite deliberately does *not* do:

- **No matrix-veto negative control.** Making it deterministic requires forcing
  both specs to `stopped` inside the retry loop — four API writes plus two state
  waits per round — because once both processes are running no further inference
  re-runs admission and a "must be exactly one running" poll can never settle.
  The matrix path is covered by the in-process policy tests; anyone adding the
  e2e control must add the quiesce rather than assume the poll will converge.
- **No assertion on a save toast.** The portal's save toasts stack and never
  disappear, so a `getByText` assertion on one is a strict-mode violation on the
  second save in a session, and narrowing with `.first()` matches a *stale* toast
  from an earlier save. Confirm a save by reading the value back through the API.

Finally, a memory-mode precondition that produces a confusing hard rejection at
the first `CreateServer` call: **a memory-mode e2e suite that creates an AI
server must create a system-tier group and an admin-tier group of its own
first.** Memory mode seeds no user groups, whereas the SQL drivers' migration
seeds a default "Standard" system-group + admin-group pair, and `CreateServer`
requires at least one admin-tier group for **every** caller including a system
admin. Reusing that "Standard" pair — the shortcut `e2e:certificates` uses — is a
**SQL-driver-only** shortcut that silently fails in memory mode. Creating the two
groups through the raw portal API rather than the UI is the established pattern:
they are precondition, not the feature under test.

## 7. SonarQube quality gate (local)

A self-hosted **SonarQube Community Build** provides static analysis and a
quality gate that a coding agent can run and act on headlessly. It is a
**local development tool, deliberately not part of CI**: the server runs via
`scripts/sonar/docker-compose.yml` bound to `127.0.0.1:9000` only, and its
generated credentials live in the gitignored `.sonar-local/` (0700/0600) —
never in the repository.

Lifecycle (`scripts/sonar/sonar.sh`, wrapped by make targets):

| Command | What it does |
|---|---|
| `make sonar-up` | Starts the server (first run: `sonar.sh bootstrap` sets credentials + token) |
| `make sonar-coverage` | Generates Go coverprofiles (both modules, `-covermode=atomic`) and the frontend lcov report |
| `make sonar-scan` | Runs the scanner alone (missing coverage reports are tolerated with a WARN, but the gate's coverage condition then reads 0) |
| `make sonar-gate` | Coverage + scan + waits for the computed **quality-gate verdict** — the meaningful pass/fail entry point |
| `make sonar-findings` | Exports open issues + hotspots as JSON into `.sonar-local/` for headless triage |
| `make sonar-branch-findings` | Filters that export down to the findings on lines **this branch** changed (see below); exits non-zero when the branch owns one |
| `make sonar-down` | Stops the server (data volume kept) |
| `sonar.sh purge` | Destroys the server **including** its database — resets credentials **and the new-code baseline** |

Semantics and policy:

- The gate is **new-code based**: pre-existing accepted issues never block;
  **new** violations and new-code coverage below the threshold do. The first
  scan after a purge establishes the baseline.
- The triage policy is **versioned in `sonar-project.properties`**, never in
  the server database, so it survives purges and is reviewable in diffs:
  cognitive-complexity (S3776) and too-many-parameters (S107) are ignored in
  **test files only** (table-driven tests legitimately grow long, flat
  bodies; production code stays fully gated), and the pseudorandom warning
  (S2245) is ignored for the purely decorative Matrix-rain theme animation.
  Deliberately accepted production findings are the structures documented in
  [§11.4 Deliberate design acceptances](../11-risks-and-technical-debt.md).
- Coverage import: `sonar.go.coverage.reportPaths` (both Go modules) +
  `sonar.javascript.lcov.reportPaths` (frontend lcov). The frontend report
  needs the `@vitest/coverage-v8` devDependency — run `npm ci` in any fresh
  checkout before `make sonar-gate`.

### 7.1 "Did my branch introduce this?" — the branch filter

Community Build cannot answer this with Sonar's own machinery: `sonar.branch.name`
is refused ("Developer Edition or above is required"), so the server only ever
knows a single branch, and a reference-branch new-code definition is unavailable.
The remaining server-side definitions each have a hole for this purpose:
`NUMBER_OF_DAYS` (max 90) stops counting a finding once the branch outlives the
window — a **false green** — and after a rebase it counts other people's recent
commits as yours; `PREVIOUS_VERSION` needs an analysis of the previous version in
the local database, which a freshly bootstrapped server does not have. Neither is
shareable either, because the reference lives in that server's database.

`scripts/sonar/branch-findings.sh` (`make sonar-branch-findings`) therefore
computes the comparison from **git**, which every developer has identically from
the repository:

- It diffs the **merge base** of `origin/main` and `HEAD` against the working
  tree, so the result is independent of how long the branch has lived and immune
  to a rebase (commits that landed on the base branch afterwards are not
  attributed), and it covers all three ways a line reaches the scanner —
  committed, uncommitted, and untracked (untracked files count whole).
- It defaults to `origin/main`, not the local `main`: in a worktree the local ref
  is often far behind, and a stale base makes the merge base ancient — which
  attributes half the repository to the branch and still looks like a working
  report.
- It is post-processing only and never contacts the server: run
  `make sonar-findings` first. Exit code 1 when the branch owns a finding, so it
  works as a pre-PR check.
- Its own cases (including the merge-base semantics and the stale-`main` default)
  are pinned by `scripts/sonar/branch-findings.test.sh`, which runs offline
  against a throwaway repository: `sh scripts/sonar/branch-findings.test.sh`.

Deliberate limits: attribution is by **line**, so a finding your change causes
elsewhere without touching that line is not attributed, and coverage is not
considered — for those, read the quality gate itself.

Operational notes (encoded in `sonar-project.properties` comments):

- **Linked git worktrees**: the scanner's JGit cannot resolve `.git`
  gitdir-pointer files; `sonar.sh` bind-mounts the main `.git` read-only and
  `sonar.scm.exclusions.disabled=true` keeps the analysis from silently
  reporting "0 non excluded files".
- **JS/TS bridge memory**: `sonar.javascript.node.maxspace=2048` caps the
  scanner's embedded Node analyzer. Do **not** raise it without raising
  Docker's memory limit — an unbounded bridge balloons into the VM's OOM
  killer and the scan dies with `EXECUTION FAILURE` (which is otherwise safe
  to retry).

## 8. License headers & generated code

- Every source file carries an `SPDX-License-Identifier: AGPL-3.0-only`
  header; `scripts/add-license-headers.sh` adds missing ones (run it after
  code generation). `scripts/gen-third-party-notices.sh` regenerates
  `THIRD-PARTY-NOTICES.md`. Dependency-license policy: see
  [Licensing](licensing.md).
- **Generated code** is excluded from analysis (`**/*_gen.go`): the OTel
  method-tracing decorators regenerate via
  `go generate ./internal/portal/...` (gowrap) followed by the license-header
  script. The `portal.API` interface itself is interfacer-generated in
  origin but the generator no longer works on the current codebase —
  signature changes are maintained by hand (see §11.4).
- **Concretely, when you add a method to `routing.RuntimeStore` or
  `portal.API`:** run `go generate ./internal/tracing/...`, which rewrites all
  three generated targets and **strips the SPDX headers** from
  `internal/account/api_tracing_gen.go` and `internal/portal/api_tracing_gen.go`;
  running `scripts/add-license-headers.sh` afterwards restores them to a zero net
  diff. Without that second step the generate leaves two files with spurious
  diffs. And `internal/portal/api.go` carries a "DO NOT EDIT" banner but **must**
  be hand-edited, so the edit is not the violation it looks like.

## 9. Test-authoring rules learned the hard way

Each of these was learned from a test that passed while covering nothing, or
from a green suite over a real defect. They are cheap to restate and expensive to
rediscover.

**Testing strategy by layer, and why each layer carries what it does.** For the
agent's model runtime the bulk of the coverage sits on the *pure* admission
policy as table-driven tests — matrix rules, per-GPU arithmetic, process limit,
unknown-VRAM-alone, victim selection, admin overrides — precisely *because* those
functions are pure. The process manager uses the **re-exec helper pattern** (the
test binary as its own child) for start/health/drain/crash/backoff and generation
discipline. The router uses `httptest` upstreams for model extraction, unbuffered
streaming, heartbeats, error envelopes and the `/running` shape. The store uses
the conformance suite across all three drivers plus migration tests. Note where
redaction is tested: **agent-side, where redaction happens** — a gateway-side
redaction test would assert on values that arrive already masked, looking like
coverage while proving nothing.

**A code comment that names a collaborator or asserts a cross-layer guarantee is
a claim to verify, not documentation.** This is the single highest-yield review
habit the codebase has produced, because several such comments have shipped
false — and in one case the false comment was the only evidence anyone had that
the work had been done at all. Real instances: a comment said a race loser "gets
its `ErrConflict` classified back into the honest sentinel by
`classifyApplicationWriteConflict`" — a function that appeared nowhere in the
repository and **had never been written**; another claimed an invariant was
"backed by the partial unique index from migration 68", which is false on the
memory driver and on any SQL database where that migration skipped the index; an
init-time panic was justified by a consequence that tracing disproved; a router
comment offered a leak as fixed while the leak remained; and a frontend comment
claimed a re-seed clears a dirty flag when it early-returns and leaves it set.
Two invariants follow: **a comment naming a collaborator or asserting a
cross-layer guarantee must be verified**, and **a guarantee that differs per
driver must be stated per driver.** Corollary: a comment saying "no reachable
behaviour depends on this line and no test can fail on it" is an *invitation* to
delete a load-bearing guard — one such line turned out to be reproducibly
load-bearing.

**A missing `errRow` in a gateway error map is invisible to the whole Go test
suite.** Deleting one row made a duplicate-create answer `500
application.request_failed` instead of 409, and `go test ./...` over the entire
backend module still passed, because the portal tests asserted only the Go
sentinel. **Any new sentinel needs at least one HTTP-level test asserting the
wire code string**, not just `errors.Is`. This is also why the sentinel/errRow
duplication matters: portal sentinels are declared as
`errors.New("<wire error code>")`, so the sentinel's message string *is* the
public API code, and the gateway's error table repeats the same literal —
renaming one is a breaking API change that must be applied in both places.

**Three rules about what an assertion actually proves.**

1. **Prove a property from the side that is not the thing under test.** The
   agent's single-start guarantee is counted from the *child process itself*
   (each real exec appends its PID to a file), never from the manager's own
   bookkeeping.
2. **An "exactly once" assertion that waits for a counter to reach 1 proves only
   *at least* once**, because it stops looking the instant it succeeds. Proving
   an absence needs a settle window after the count is reached.
3. **A source-scan wiring test passes while the wiring is broken.** Ask "is this
   registry actually wired?" through the `startAppHealthLoop` package-`var`
   seam — already stubbed in tests — so a test can drive the real production path,
   capture the actual bundle production hands the loop, **write** through the
   instance the `*gateway.Server` itself holds, **prune** through the captured
   bundle, and assert the write became invisible. That is only possible if both
   sides hold the same object. A `strings.Count` scan over `main.go` text is
   sensitive to gofmt alignment and blind to whether its matches even name the
   same variable; the seam-based shape is strictly stronger and is the pattern to
   follow, or convert to, for any future wiring assertion.

**Timing knobs are package-level `var`s so tests can shrink them — and the
restore must be registered *before* the component's close cleanup.** `t.Cleanup`
runs LIFO, so close (which joins every goroutine still reading those vars) must
finish before the values are restored, or the race detector fires. This is
invisible and fails only under `-race`.

**Testing a process manager's post-signal window needs a real barrier, and the
stub child must be able to ignore SIGTERM.** A test that triggers a drain "as
soon as the spec is `starting`" fires within microseconds of `cmd.Start()` —
before the child has finished Go runtime init and installed its SIGTERM
disposition — so SIGTERM kills it with the **default** action, the health poll
returns on the exit channel without ever posting a result, and the race the test
names cannot occur: **it passes against the unfixed manager.** The correct barrier
waits until the child answers its health port with *any* status, 503 included,
which is strictly after the handler is installed. Relatedly, an
`-ignore-sigterm` stub is not a contrivance: real model servers behave that way,
since a graceful shutdown that finishes in-flight generations easily outlasts
SIGTERM by seconds — which is what makes the SIGKILL escalation the path that
normally ends a child.

**Two frontend test traps, both of which produce false greens rather than
failures.**

- This project runs Vitest **without `globals`**, so Testing Library's automatic
  cleanup never registers: a test file that renders components must register
  `afterEach(cleanup)` itself. Skipping it leaves an open MUI Tooltip from one
  test satisfying the next test's `findByRole('tooltip')`. The cause is a Vitest
  config choice, not the test file, and is not discoverable from the assertion.
- **Never build a `RegExp` from a translation string.** Portal i18n strings
  legitimately contain regex metacharacters — `${PORT}`, `${AGENT_ENV:NAME}`,
  parentheses — so `new RegExp(t.someKey)` compiles into a pattern that can never
  match the rendered text (`$` becomes an end anchor mid-pattern, `{…}` and `(…)`
  become quantifier and group syntax). Use a plain substring match
  (`findByText(t.someKey, { exact: false })`). The same latent fragility exists in
  every such assertion, including ones whose current translation happens to
  contain no metacharacters.

**An agent's runtime-config document is derived for the server that owns the
agent token it authenticates with.** The development seed `dev-agent-secret`
(`OP_AI_GATEWAY_DEV_AGENT_TOKEN`) belongs to the seeded mock AI server
`mock-server`, so an agent started with that token manages *mock-server's*
runtime — not that of any server created afterwards, however the agent is
otherwise configured. Any dev or test setup that wants a specific server managed
must mint a fresh agent token for it. Otherwise the agent connects, negotiates
and reports healthy while managing a different server's (empty) document: a
silent no-op that is very hard to attribute to the token.

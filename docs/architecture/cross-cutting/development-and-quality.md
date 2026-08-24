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
  `-timeout=25m` (a timeout failure at 600s is the deadline, not a hang).
- **Store conformance suite**
  (`gateway/backend/internal/store/conformance_test.go`): the store contract
  runs against the in-memory store and SQLite always, and against PostgreSQL
  when `OP_AI_GATEWAY_TEST_POSTGRES_DSN` is set. Run it Postgres-backed
  before merging store changes — SQLite masks narrow-column truncation (see
  [Persistence](persistence.md) and ADR-005). Note that memory mode enforces
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
  `e2e:resource-groups`, `e2e:services`, `e2e:servers`, `e2e:smtp` (local
  mail catcher module in `gateway/e2e/e2e-smtp/mailcatcher`), `e2e:totp`,
  and `e2e:system-admin-mode`. These suites run the gateway in **memory
  mode** — fast, but see the conformance-suite caveat above for what memory
  mode cannot catch. Changes to invite/auth UI or visibility models are
  expected to require matching spec updates; plan the e2e rewrite as part of
  such a change.

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

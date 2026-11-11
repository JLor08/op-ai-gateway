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
| Node.js / npm | Node 26 (LTS) | `gateway/frontend` (React/TS/Vite) and `gateway/e2e` (Playwright) |
| golangci-lint | v2 (pinned in CI, currently v2.15.0) | Go formatting (`fmt`, gofumpt-based) + linting, both modules; CI pins Go 1.26 |
| Docker | any recent | Deploy images, the Postgres test env, the local SonarQube server |
| sonar-scanner | 8.0.1+ (optional) | The local SonarQube gate's native scanner, the default on arm64 hosts ([§7.3](#73-scanner-modes-docker-and-native)); `brew install sonar-scanner` |

**Go stays on 1.26 for now (CI `go-version: "1.26"`, both Go builder images pinned to
`golang:1.26.5`).** Go 1.27 (checked up to go1.27.4) carries a net/http server regression,
golang/go commit `f5c3217500dc` (CL 794640, "net/http: don't rely on server request body not
changing"): to decide whether to close the connection after the reply, `chunkWriter.writeHeader`
now calls `bodyRemains()`, which takes the request body's mutex, where Go 1.26 read a lock-free
flag. `body.Read` holds that mutex for the whole blocking network read. So for a request that sent
`Expect: 100-continue`, a full-duplex handler (`EnableFullDuplex`) cannot write its response
headers while another goroutine is blocked reading the request body: the header write waits until
the client sends more body bytes. The live-transcription relay hits exactly that. The loopback
dials `/v1/audio/transcriptions/live` with `Expect: 100-continue` (load-bearing, see ADR-073 (d)),
`liveBodyCopyLoop` reads the body during the upstream dial (#331), and a client that waits for the
SSE `200` before sending more audio deadlocks. On Go 1.27 every live-transcription and voice chat
WebSocket test times out. There is no GODEBUG for it, and dropping the `Expect` header instead
breaks the early-refusal paths. Move to Go 1.27+ once a Go release takes the body lock out of
that header-write path; the `TestVoice*` and `TestLiveTranscriptionLoopback*` tests in
`internal/gateway` show when it is safe.

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
| `make test-go` | `go test ./...` in `gateway/backend`, then in `server-agent` |
| `make test-ui` | `vitest run` in `gateway/frontend` |
| `make test-e2e` | The base Playwright suite in `gateway/e2e` |
| `make fmt` / `make lint-fix` | `golangci-lint fmt` / `run --fix` across both Go modules |
| `make lint` | `lint-go` + `lint-docs` — everything lintable |
| `make lint-go` | `golangci-lint run` across both Go modules |
| `make lint-docs` | The docs consistency check — see [§4.1](#41-documentation-consistency-make-lint-docs) |
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
- **Docs**: `scripts/check-docs.sh` (`make lint-docs`, part of `make lint`) —
  see [§4.1](#41-documentation-consistency-make-lint-docs).
- **Pre-commit hook** (`scripts/git-hooks/pre-commit`, enabled by
  `make hooks`): when markdown or a `docs/architecture/` YAML file is staged,
  runs the docs check; when Go files are staged, format-check + lint on both
  modules. Skip a single commit with `--no-verify`.
- **Frontend bundle layout** (`gateway/frontend/vite.config.ts`): the
  production build splits the always-loaded vendor libraries (`mui`,
  `vendor`) from the app chunk via `manualChunks` for browser-cache
  stability; `heic-to` (~3 MB, inlined libheif WASM) is reached only via
  dynamic import and stays a lazy chunk. `chunkSizeWarningLimit` sits just
  above that known decoder chunk — if `vite build` warns about chunk size
  again, a chunk outgrew the decoder; treat it as a regression instead of
  raising the limit.

### 4.1 Documentation consistency (`make lint-docs`)

`scripts/check-docs.sh` — bash + git + awk, no dependencies, ~0.2 s. Three
things this repository's conventions require and nothing else enforces:

1. **Every intra-repo markdown link resolves** — the file, and where the link
   carries a `#anchor`, the anchor too. Anchors are **derived** from the target
   document's own headings using GitHub's slug rules (lowercase, drop
   punctuation, each space becomes a hyphen — runs are *not* collapsed, so
   `## 12. Delegated & resource-scoped …` really does anchor as
   `#12-delegated--resource-scoped-…`, and an ADR heading's em dash leaves a
   double hyphen the same way). The generated ADR anchors are therefore
   checked, not trusted.
2. **Every file under `docs/architecture/` is reachable from
   `docs/architecture/README.md`**, directly or transitively. A document no
   index path reaches is invisible; the arc42 index rule is stated in
   `AGENTS.md` and this is what enforces it.
3. **`docs/architecture/reference/openapi.yaml` reads end to end and every
   `$ref` resolves.** The reader covers the YAML subset the spec is written in
   (block mappings and sequences, single-line flow collections, block scalars)
   and *fails* on anything outside it rather than skipping it, so a silent pass
   cannot mean "did not understand". It also catches tabs in indentation,
   indentation that lands between levels, and duplicate keys in one mapping.

Deliberately **out of scope**: `http(s)` links (they fail for reasons that have
nothing to do with the repository and would make the gate flaky); prose style,
spelling and line length (this is not a markdown linter); and
`docs/superpowers/` + `docs/implementation-status.md` as link *sources* for the
two checks above — they are branch-local working documents that never reach
`main`, so gating reachability/anchor checks on them would gate CI on scratch.
On a branch that carries them their headings are still collected, so a link
on *that* branch pointing *into* them is still verified there — but on `main`,
where it matters, neither path exists, so nothing is collected and a citation
into either is **not** verified by these two checks at all. The guard below is
what actually covers that case.

**A fourth, narrower guard**: `docs/superpowers` and `docs/implementation-status`
are forbidden **strings**, full stop, in every tracked (or about-to-be-tracked)
file in the repository — not just markdown, and not just as a link target.
Both name branch-local working documents (per `AGENTS.md`) that are real on a
feature branch and gone from every commit that reaches `main`; a Go doc
comment, a TypeScript comment or an operator-facing README that cites either
path is not stale someday, it is already dangling on every branch that is not
the one that happened to have the file open (issue #121 found dozens of these).
The check is deliberately a flat grep plus a five-file allowlist —
`AGENTS.md`, `CLAUDE.md`, this file, `scripts/check-docs.sh` and
`scripts/check-docs.test.sh`, the only files that describe or test the rule
itself — rather than a per-language parser: recognizing "this token denotes a
path" needs real parsing in each of Go, TypeScript and Markdown, and would
still trip over deliberately-constructed test fixtures (this file's own
`check-docs.test.sh` cases among them). The same reasoning is why the SDD
per-task report/brief files (`task-N-report.md` and similar, from a
`server-agent`-side, gitignored planning workspace — never committed on any
branch, so strictly worse than `docs/superpowers`) are **not** gated the same
way: unlike the two literal paths above, "task-" followed by a number collides
with ordinary prose already in this repository (review-round shorthand like
"fix round 1, I3", section banners like "Task 9: file-mode runtime report
ingest") in a way a flat grep cannot tell apart from a real citation without
false positives. Those were found and repointed by hand during the same
audit; nothing here re-checks that they stay gone.

`scripts/check-docs.test.sh` pins all of it: a miniature corpus in a throwaway
git repository, one case per failure mode (broken link, wrong anchor, wrong
duplicate-heading suffix, unindexed architecture file, severed reachability
path, dangling `$ref`, non-local `$ref`, duplicate key, bad indent, tab,
unparsable line, multi-line flow, a forbidden string outside the allowlist)
**and** one case per thing it must stay quiet about (links and headings inside
fenced code blocks, links inside inline code, external URLs, branch-local
scratch documents, the allowlisted files themselves saying the forbidden
strings). CI runs the pins before the check, because a docs checker that
reports success on a broken corpus is worse than no checker.

## 5. Continuous integration

`.github/workflows/ci.yml` runs on every push and pull request:

- **Go job** (backend + server-agent, each in its own working directory):
  `golangci-lint fmt --diff`, `golangci-lint run`, `go test ./...`.
- **Docs job**: `sh scripts/check-docs.test.sh` then `./scripts/check-docs.sh`
  ([§4.1](#41-documentation-consistency-make-lint-docs)). No toolchain setup —
  it needs only the checkout.
- **Sonar tooling job**: `sh scripts/sonar/branch-findings.test.sh` and
  `bash scripts/sonar/sonar.test.sh`, the offline tests of the local SonarQube
  gate's scripts ([§7](#7-sonarqube-quality-gate-local)). The gate itself stays
  out of CI; these need only the checkout, `git` and `jq`, and the job fails
  rather than skips when `jq` is missing.
- **opencode config generator job**: `sh scripts/gen-opencode-config.test.sh`,
  the offline test of `scripts/gen-opencode-config.sh` (it reads `context_length`
  from `/v1/models`; the test runs the real script against a stub `curl`). Needs
  only the checkout and `jq`, and fails rather than skips when `jq` is missing.
- **Frontend job**: `npm ci`, `npm run format:check`, `npm run lint`,
  `npm run build` (the type-checked build is the i18n de/en parity guard —
  `en: PortalMessages` fails to compile on a missing or excess key), and
  `npm test`.
- **E2E runtime job**: installs the frontend and e2e dependencies and
  Chromium, warms the Go build cache for the gateway, the agent and the
  suite's stub server, and runs `npm run e2e:runtime`
  ([§6.1](#61-e2eruntime--real-child-processes-from-a-stub-model-server)).
- **E2E RPC cluster job**: builds `llama-server` and `ggml-rpc-server`
  CPU-only at the pinned llama.cpp commit (cached by commit and a digest of the
  build flags), fetches the `stories260K.gguf` model (cached), and runs
  `npm run e2e:rpc-cluster`
  ([§6.2](#62-e2erpc-cluster--a-real-llamacpp-rpc-cluster)). These two are the
  Playwright suites CI runs.

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
  **CI now passes `-timeout=25m` for the backend module** for a second instance
  of the same "deadline, not a hang" confusion: `internal/store` normally takes
  ~151s and `internal/gateway` ~130s of the 600s default, which is ample until a
  runner degrades — one observed run had the disk-heavy packages ~4x slower
  (`internal/portal` 6.6s → 28.4s, `cmd/gateway` 24.7s → 90.9s) while CPU-bound
  ones were unaffected, and `internal/store` crossed 600s. Read such a red as
  the deadline unless the panic names a test whose printed elapsed time is
  large: Go names the test that is *running* and prints how long it has been
  running, so a genuinely stuck one shows the full elapsed (a deliberately
  blocked subtest under `-timeout 30s` is reported as `(30s)`), while that run
  named one `(1s)` old. `server-agent` stays on the default — its slowest
  package is ~47s.
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
  imports. **Every test is unmounted for you, from `src/vitest.setup.ts` — do
  not rely on Testing Library doing it.** RTL registers its own auto-cleanup
  only when a *global* `afterEach` exists, and `globals` is off in
  `vite.config.ts` (which is why the setup file imports `afterEach` from
  `vitest`), so RTL silently skips it and the setup file calls `cleanup()`
  instead. Leaving a tree mounted is not cosmetic: React defers a mounted
  root's passive-effect flush to a Node `setImmediate`, which Vitest's
  per-file environment teardown does not cancel, and the deferred callback
  reads `window` — so a surviving root can fail the whole job with an uncaught
  `ReferenceError` while every test passes, which `retry` cannot rescue
  because no test failed. `src/vitest.setup.test.tsx` pins this as an ordered
  pair of tests.
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
  `e2e:rpc-cluster` (a real llama.cpp RPC cluster: the gateway, two real
  server-agents, a real `llama-server` head and a real `ggml-rpc-server`
  member; [§6.2](#62-e2erpc-cluster--a-real-llamacpp-rpc-cluster)),
  `e2e:runtime` (agent-managed model runtime end to end: the REAL server-agent
  binary starts REAL child processes from a stub model server module in
  `gateway/e2e/e2e-runtime/fixtures/stubserver`, which the suite builds but
  never starts), `e2e:services`, `e2e:servers`,
  `e2e:speech` (covers `POST /v1/audio/speech` end to end: the real gateway
  against an in-process stub audio.cpp, through the portal's audio_cpp form,
  the capability probe, the relay and the Activity view),
  `e2e:chat-speech` (covers a text-to-speech turn in the portal chat end to
  end: the same in-process stub audio.cpp behind a form-created `audio_cpp`
  application, the probe's `speech` verdict deciding what the chat picker
  offers, the composer switching to speech mode, one relayed
  `/v1/audio/speech` call from the chat run executor, the playable and
  downloadable audio turn served from the chat blob store, its survival of a
  reload, the advanced passthrough reaching the upstream body, and the
  `audio_second` usage row; local-only),
  `e2e:chat-transcription` (covers a speech-to-text turn in the portal chat
  end to end: the same in-process stub audio.cpp behind a form-created
  `audio_cpp` application, the probe's `transcription` verdict deciding what
  the chat picker offers, the composer switching to the audio input, a clip
  attached from a file (a WebM and an MP3 fixture) or recorded with the real
  browser recorder being converted to WAV by the composer's real WebAudio path
  and arriving at the one multipart `/v1/audio/transcriptions` call of the chat
  run executor as a 16 kHz mono 16-bit PCM WAV, the same bytes the chat blob
  store holds, the transcript turn next to the user's own clip served from
  the chat blob store, their survival of a reload, the advanced passthrough
  reaching the form, and the `audio_second` usage row; plus streaming (ADR-065):
  against a model the stub lists with `mode=streaming` the run's `stream=true`
  is relayed and the transcript turn fills in delta by delta (the stub pauses
  after each delta and the spec reads the turn at every pause), the committed
  transcript is the done event's text and survives a reload, and the usage row
  is billed from the converted WAV's header; local-only),
  `e2e:smtp` (local
  mail catcher module in `gateway/e2e/e2e-smtp/mailcatcher`), `e2e:totp`,
  `e2e:transcription` (covers `POST /v1/audio/transcriptions` end to end, the
  multipart twin of `e2e:speech` over the same `audio_cpp` application type:
  the portal form, the `asr`/`tts` capability probe, the multipart relay with
  the model rewritten, the capability and `response_format` refusals, and the
  `audio_second` usage row metered from the response body's
  `timing.audio_duration_ms`; plus streaming (ADR-065): the probe writing the
  `transcription_streaming` verdict from the stub's `/v1/models` `mode`, a
  `stream=true` request relayed as the upstream's event stream frame by frame
  (the first frame reaches the client while the stub still holds the stream
  open) and billed from the uploaded WAV's header, a stale `yes` retried once
  buffered after the upstream's `500` "requires a model configured with
  `mode=streaming`", and an offline or verdict-less model answered buffered up
  front with no `stream` part sent; local-only like every suite but
  `e2e:runtime` and `e2e:rpc-cluster`),
  and `e2e:system-admin-mode`. These suites run the gateway in **memory
  mode** — fast, but see the conformance-suite caveat above for what memory
  mode cannot catch. Changes to invite/auth UI or visibility models are
  expected to require matching spec updates; plan the e2e rewrite as part of
  such a change.

> **Only `e2e:runtime` and `e2e:rpc-cluster` run in CI; the other 23 Playwright
> suites do not.** `.github/workflows/ci.yml` has seven jobs — `go`, `docs`,
> `sonar-scripts`, `opencode-config-script`, `frontend`, `e2e-runtime` and
> `e2e-rpc-cluster`. `e2e-runtime` runs `npm run e2e:runtime` alone (the
> agent-managed runtime, whose three-commit regression no gate once noticed —
> issue #24); `e2e-rpc-cluster` runs `npm run e2e:rpc-cluster` alone (the only
> proof of RPC cluster mode against real llama.cpp processes). Every **other**
> suite is still a **local-only gate a pull request can pass without
> running**: `make test-e2e` runs the base suite
> alone, and nothing in the Makefile enumerates the scenario suites. Likewise
> the three e2e fixture Go modules
> (`e2e-certificates/fakeacme`, `e2e-smtp/mailcatcher`,
> `e2e-runtime/fixtures/stubserver`) are outside `make lint`/`make fmt`/`make
> test-go` (which enumerate `gateway/backend` and `server-agent` only) and
> outside `sonar.sources`, so they are formatted, vetted and linted only by hand
> in the module directory — which is also why a new fixture module needs no
> registration anywhere. A reader who sees a rich suite catalogue should not
> assume CI enforces all of it: it enforces exactly two suites, which changes both
> review expectations and any decision to invest in the rest.

### 6.1 `e2e:runtime` — real child processes from a stub model server

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
redundant.** Scenario 5 shows an eviction: with a 1000 MB GPU-0 budget and two 700
MB specs, an inference for B evicts the idle A. On its own that is ambiguous, because
an eviction by the VRAM-sum arithmetic is observationally identical to one by any
other rule (the process limit, the pair rule). Scenario 6 raises the budget to 2000
MB and changes *nothing else*, and the same two specs become co-resident: that is what
pins scenario 5's eviction on the arithmetic, and it is the headline the feature
exists for, several models on one host under a real agent. Widening the tight budget
fails only scenario 5, which is the mutation that pins this half.

What scenario 6 **no longer** proves is delivery of the pair. Under the old
default-deny matrix an undelivered pair gave exactly scenario 5's outcome, so
scenario 6 doubled as the proof that the pair had reached the agent (removing the
co-residency toggle left scenarios 1–5 green and failed only scenario 6). Under the
`opt_out` default the pair is allowed with or without an explicit entry, so an
undelivered pair would now leave both scenarios green, and the pair rule is excluded
from scenario 5's explanation by construction and not by evidence. The scenario still
**explicitly allows** the pair in the co-residency editor, so that it does not lean on
the default and survives a change of it (the one Allowed control of the selected
model's single relationship, matched exactly because the German "Erlaubt" is also a
substring of the Default segment's "Standard (erlaubt)"), and it asserts the save is
the whole document (one explicit `allow`, plus a mode). That the effective list
actually reaches the agent is covered in process, by the portal's runtime-config
assembly tests and the agent's policy tests, and not by this suite.

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

- **No pair-rule-veto negative control.** Making it deterministic requires forcing
  both specs to `stopped` inside the retry loop — four API writes plus two state
  waits per round — because once both processes are running no further inference
  re-runs admission and a "must be exactly one running" poll can never settle.
  The pair-rule path is covered by the in-process policy tests; anyone adding the
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

### 6.2 `e2e:rpc-cluster` — a real llama.cpp RPC cluster

Config `gateway/e2e/playwright.rpc-cluster.config.ts`, spec
`gateway/e2e/e2e-rpc-cluster/rpc-cluster.spec.ts`. Pure API (no browser, no
frontend build): the gateway in memory mode and two real server-agents on one
machine, each registered as its own AI server; the head server's agent runs a
real `llama-server` cluster head, the member server's agent a real
`ggml-rpc-server`. The in-process tests fake every one of those processes, so
this suite is the only proof that the members-first start, the watchdog and
the member usage work against the real builds. It needs two inputs and fails
(never skips) without them: `OP_E2E_LLAMA_BIN_DIR`, a directory with
`llama-server` and `ggml-rpc-server` of one RPC-enabled build of the pinned
commit, and `OP_E2E_RPC_MODEL`, the path to `stories260K.gguf`.
`OP_E2E_KEEP_LOGS=1` keeps the agents' logs.

Three serial scenarios, both servers `lan` servers bound to loopback:

- **(a) relay transport, head in env mode** and **(b) direct transport, head
  in args mode**: a request for the cold head parks at the routing gate while
  the gateway starts the cluster, and is answered once it formed; the member
  reports the head's hold; the member's measured RAM is stored on the head and
  survives the stop (the formed run's peak). (a) stops with `stop_members`, so
  the member stops with the head; (b) stops without it, so the member is only
  released and idles out on its 60 s timeout.
- **(c) member process killed**: the gateway's watchdog stops the head without
  any request and records `runtime.cluster_member_lost`; a request during the
  cooldown is answered 503 `runtime.cluster_start_failed`, its message naming
  `runtime.cluster_member_lost at member 0`, without the head starting; once
  the cooldown ended a request forms the cluster again with a new member
  process.

A run takes about three minutes, most of it the two waits on timers (the
member idle-out and the cooldown).

## 7. SonarQube quality gate (local)

A self-hosted **SonarQube Community Build** provides static analysis and a
quality gate that a coding agent can run and act on headlessly. It is a
**local development tool, deliberately not part of CI**: the server runs via
`scripts/sonar/docker-compose.yml` bound to `127.0.0.1:9000` only, on a pinned
image ([§7.2](#72-pinned-images-and-deliberate-upgrades)), and its
generated credentials live in the gitignored `.sonar-local/` (0700/0600) —
never in the repository. The docker volumes are globally named, so they are
shared across every git worktree on the same docker daemon; `.sonar-local/`
therefore resolves to the **main worktree's** copy (via `git worktree list`,
overridable with `SONAR_LOCAL_DIR`) so a linked worktree picks up the same
admin credentials instead of hitting a stale, per-worktree copy.

Lifecycle (`scripts/sonar/sonar.sh`, wrapped by make targets):

| Command | What it does |
|---|---|
| `make sonar-up` | Starts the server (first run: `sonar.sh bootstrap` sets credentials + token) |
| `make sonar-coverage` | Generates Go coverprofiles (both modules, `-covermode=atomic`) and the frontend lcov report |
| `make sonar-scan` | Runs the scanner alone, in a container or natively ([§7.3](#73-scanner-modes-docker-and-native)); missing coverage reports are tolerated with a WARN, but the gate's coverage condition then reads 0 |
| `make sonar-gate` | Coverage + scan + waits for the computed quality-gate verdict. The verdict is **advisory**: it exits 0 even on an advisory FAIL (pass `sonar.sh gate --strict` to exit non-zero on it). Community Build cannot compare against main, so this is **not** the branch pass/fail gate — `sonar-branch-findings` is (see §7.1) |
| `make sonar-findings` | Exports open issues + hotspots as JSON into `.sonar-local/`, and records which analysis the export describes (`analysis-meta.json`: SCM revision + date) so a stale export can be detected |
| `make sonar-branch-findings` | Filters that export down to the findings on lines **this branch** changed (see below); the **authoritative pre-PR gate** — exits non-zero when the branch owns one, and refuses a stale export whose revision is not `HEAD` |
| `make sonar-down` | Stops the server (data volume kept) |
| `sonar.sh purge` | Destroys the server **including** its database — resets credentials **and the new-code baseline**, and drops the recorded server version ([§7.2](#72-pinned-images-and-deliberate-upgrades)) |

Semantics and policy:

- The gate is **new-code based**: pre-existing accepted issues never block;
  **new** violations and new-code coverage below the threshold do. But "new
  code" here is **not** "changed versus main". Community Build's new-code period
  is `PREVIOUS_VERSION`, so each scan is compared against the previous *analysis*
  on this shared, single-branch server — not against main. `sonar.sh scan` sets
  `sonar.projectVersion` to the short `HEAD` description per analysis so that
  period has a distinct previous version to advance against; without it every
  analysis reports version "not provided", the version never changes, and the
  period collapses to the project's **first-ever** analysis — making "new code"
  mean everything since the server was bootstrapped. Even with it, the server
  verdict is only advisory: for the real "did my branch introduce this?"
  question use `sonar-branch-findings` (§7.1), which computes it from git. The
  first scan after `sonar.sh purge` re-establishes the baseline.
- The triage policy is **versioned in `sonar-project.properties`**, never in
  the server database, so it survives purges and is reviewable in diffs.
  Every entry is justified individually in that file's own comment; the whole
  policy is: the Go rules for cognitive complexity (S3776) and too many
  parameters (S107) are ignored in **non-production Go** — `*_test.go` by
  name, plus anything under a `testdata/` directory for S3776, since a Go
  fixture compiled as its own program carries no `_test` in its name
  (table-driven tests legitimately grow long, flat bodies; production code
  stays fully gated, and the TypeScript S3776 rule is not excluded anywhere);
  the pseudorandom warning (S2245) is ignored for the purely decorative
  Matrix-rain theme animation; and empty-function-body (S1186) is ignored in
  the two agent files where an empty body **is** the implementation —
  `internal/runtime/manager.go`'s fourteen sealed-interface marker methods
  (`func (cmdX) isCommand() {}`) and `internal/runtime/proc_windows.go`'s
  `setProcGroup`, a documented Windows no-op. File scope is the finest
  granularity Sonar's exclusion mechanism offers, so those last two hide the
  rule for their whole file; why each is a false positive is written out next
  to the entry, to be re-checked rather than trusted. Deliberately accepted
  production findings are the structures documented in
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
- It **refuses a stale export**. `sonar.sh findings` records the analysed SCM
  revision beside the export (`analysis-meta.json`), and the filter exits 2 when
  that revision is not `HEAD`: a scan that failed server-side, or another
  worktree's scan, leaves the server's last *successful* analysis in place, and
  attributing findings against it would otherwise read exactly like a real pass.
  `SONAR_BRANCH_FINDINGS_ALLOW_STALE=1` overrides the check; a missing metadata
  file (an older export, or a hand-passed `--findings`) only warns.
- Its own cases (including the merge-base semantics, the stale-`main` default,
  and the stale-export guard) are pinned by
  `scripts/sonar/branch-findings.test.sh`, which runs offline against a
  throwaway repository: `sh scripts/sonar/branch-findings.test.sh`.

Deliberate limits: attribution is by **line**, so a finding your change causes
elsewhere without touching that line is not attributed, and coverage is not
considered — for those, read the quality gate itself. `go:S3776` (cognitive
complexity) anchors its finding on the function's declaration line, so a body
that grows past the threshold under an unchanged signature is not attributed
either: re-derive each changed function's score with `gocognit`, the
`origin/main` blob against the branch's.

Operational notes (encoded in `sonar-project.properties` comments):

- **Linked git worktrees have no SCM blame.** Run from a `.worktrees/<name>`
  checkout, the scanner's JGit *does* open the repository and read the HEAD
  revision — the `.git` gitdir-pointer resolves, and `sonar.sh` bind-mounts the
  common `.git` read-only so the objects are reachable — but its blame yields
  **nothing** (measured on SonarQube 26.8: 0 of 1027 files blamed). Without
  blame, Sonar dates every line by the analysis that first saw it and so treats
  the whole tree as new code. This inflates the **already-advisory** server gate
  only (see §7 and above); `sonar-branch-findings` computes its comparison from
  git on the host and is unaffected, which is why it — not the server gate — is
  the authoritative pre-PR check. The git CLI blames the same worktree correctly,
  but SonarQube's git SCM sensor uses JGit with no CLI fallback, so there is no
  clean in-scanner fix short of scanning a non-worktree checkout. That
  measurement is the **docker** scanner's; the native scanner
  ([§7.3](#73-scanner-modes-docker-and-native)) sees the worktree's real paths
  and does store per-line blame for a linked worktree (measured on SonarQube
  26.8 with SonarScanner CLI 8.4).
  `sonar.scm.exclusions.disabled=true` keeps the analysis from silently reporting
  "0 non excluded files".
- **JS/TS bridge memory**: `sonar.javascript.node.maxspace=2048` caps the
  scanner's embedded Node analyzer. Do **not** raise it without raising
  Docker's memory limit — an unbounded bridge balloons into the VM's OOM
  killer and the scan dies with `EXECUTION FAILURE` (which is otherwise safe
  to retry). A native scan is not bound by the VM, and can raise it for one
  run: `SONAR_SCANNER_EXTRA_ARGS=-Dsonar.javascript.node.maxspace=4608`.
- **Go analysis memory**: the scanner's engine JVM needs about **4 GB of
  heap** for this repository's Go code. sonar-go converts and deserializes one
  package (directory) at a time, and `gateway/backend/internal/gateway` alone
  holds 465 files: measured with SonarQube 26.8 (sonar-go 1.40), the engine
  runs out of heap in that package at `-Xmx3g` and passes at `-Xmx4g`. sonar-go
  has no property that limits how much it parses at once or caps its memory
  (its properties are `sonar.go.exclusions`, the report paths,
  `sonar.go.converter.validation` and `sonar.go.duration.statistics`), so the
  heap is the only lever. It is set with `SONAR_SCANNER_JAVA_OPTS`, **not**
  `SONAR_SCANNER_OPTS` (§7.3). The JVM's own default, a quarter of the memory
  it sees, is about 3 GB inside a 12 GB docker VM, which fails, and 4 GB on a
  16 GB host, which is borderline. `sonar.sh` therefore sets the engine heap
  itself when `SONAR_SCANNER_JAVA_OPTS` is unset: `-Xmx6g` in native mode and
  `-Xmx5g` in docker mode (in a 12 GB VM, docker mode passes with `-Xmx5g`,
  measured under Rosetta). Docker mode's default needs a **colima/Docker VM of
  at least about 8 GB**, room for the 5 GB engine next to the SonarQube
  server.
  `SONAR_SCANNER_EXTRA_ARGS=-Dsonar.go.duration.statistics=true` logs the Go
  sensor's used and peak memory.

### 7.2 Pinned images and deliberate upgrades

Both images are pinned to exact releases: the server in
`scripts/sonar/docker-compose.yml` (`sonarqube:26.8.0.126808-community`), the
scanner in `scripts/sonar/sonar.sh`
(`sonarsource/sonar-scanner-cli:12.1.0.3233_8.0.1`). A floating tag is not an
option for the server. It keeps everything — credentials, analyses, the
new-code baseline — in its **embedded H2 database** on the
`op-ai-gateway-sonar-data` volume, and SonarQube cannot move an embedded
database to another release, in either direction (measured with 26.8.0 and
26.9.0):

- **A newer server on an older volume** stays in status `DB_MIGRATION_NEEDED`,
  and `POST /api/system/migrate_db` answers `NOT_SUPPORTED` ("Upgrade is not
  supported on embedded database").
- **An older server on a newer volume** never answers — `/api/system/status`
  stays unreachable — and its container exits, with code 0, after logging
  "Database was upgraded to a more recent version of SonarQube".

With a floating tag such as `sonarqube:community`, the first case strikes the
day a newer image is pulled, and both look like a slow start: a plain wait
sits out its whole timeout before a bare "timed out waiting for SonarQube".
`sonar.sh up` — and `scan`, when it has to start the server — therefore fails
fast in both cases. It stops the container, leaves the volume untouched, and
names the two ways out:

1. **Keep the local data** by running the image that matches the volume:
   `export SONAR_IMAGE=sonarqube:<version>-community`. The override is not
   remembered, so it has to be set for every `sonar.sh` and `make sonar-*`
   call, until this worktree's branch pins that release or until you purge
   (unset it before the purge); `up`, a `scan` that starts the server, and
   `purge` print a note while it is set. Bootstrap — which every `up`, `scan`
   and `gate` runs, while `findings` does not — records the UP server's
   version in `.sonar-local/server-version`, so the message can name the
   image. A record that names the very release that just failed is stale — the
   volume was recreated by something that does not keep the record, such as
   an older `sonar.sh` — and is ignored whenever the failing release can be
   read (from the status endpoint, or from the image's `SONAR_VERSION`);
   without a usable record the message says how to read an image's release
   from its `SONAR_VERSION`.
2. **Start fresh** with `sonar.sh purge && sonar.sh up`, after
   `unset SONAR_IMAGE`: an override that is still set would create the fresh
   volume on its own release, and the message adds the `unset` whenever one
   is. The purge deletes every local analysis, the new-code baseline and the
   admin credentials, and drops the record; bootstrap mints new credentials,
   and the next scan sets a new baseline (§7).

`SONAR_SCANNER_IMAGE` overrides the scanner the same way (the JVM options are
in §7.3). The scanner keeps no state, but it is pinned too, so the gate is the
same known-good pair on every machine: a floating `latest` scanner can move
ahead of the pinned server it has to talk to. The native scanner is not pinned,
only held to a minimum release (§7.3).

**Upgrading deliberately.** Moving a pin is an ordinary change made through a
pull request, and the server pin resets every developer's local Sonar data:

1. Pick the release's full tag, `sonarqube:<version>-community` with the build
   number (for example `26.9.0.129388-community`) — never `community`,
   `lts-community` or another tag that moves.
2. Bump it in `docker-compose.yml`, and the scanner in `sonar.sh` when the new
   server needs a newer one. Check the server's release notes for the minimum
   scanner version.
3. Once the change is on `main`, each developer runs `unset SONAR_IMAGE;
   sonar.sh purge && make sonar-up` once. Until then, `SONAR_IMAGE` keeps the
   old server running against the old volume.
4. Re-check the version-specific statements in §7 against the new release, such
   as the SCM blame measurement above.

The volume is shared by every worktree on the docker daemon (§7), but each
worktree takes the pin from its own `docker-compose.yml`. After the purge, a
worktree whose branch predates the bump still pins the old release: starting
the server there recreates the one container on the old image, which then
fails as an older server on a newer volume. In such a worktree, take way 1
(export the new release) or rebase onto `main` — not way 2, which would move
the volume back to the old release and make every up-to-date worktree fail
the other way. While the author of the bump tests it before it lands, the
author's purge puts the volume ahead of every other worktree, `main`'s
included; they fail the same way, and until the bump lands only way 1 helps,
since `main` has nothing to rebase onto yet. Moving a pin back is the reverse
case and needs the same purge, because an older server cannot open a newer
volume either: a worktree that still pins the newer release then stops at
`DB_MIGRATION_NEEDED`, and way 1 there exports the older release.

The startup wait, the record and the override notes, both mismatch directions
included, are pinned by `scripts/sonar/sonar.test.sh`, which runs offline
against stubs of `curl`, `docker compose`, `docker` and `sleep`:
`bash scripts/sonar/sonar.test.sh`. It also checks that both images stay pinned
to full release tags, and covers the scanner modes of §7.3.

### 7.3 Scanner modes: docker and native

`sonar.sh scan` (so `make sonar-scan` and `make sonar-gate`) runs the scanner
one of two ways, chosen with `SONAR_SCANNER_MODE`:

| Mode | What runs |
|---|---|
| `docker` | The pinned scanner image on the compose network, with the repository mounted at `/usr/src` |
| `native` | A `sonar-scanner` on the host: the one `SONAR_SCANNER_BIN` names, else the one on `PATH`. Install it with `brew install sonar-scanner` |
| `auto` (default) | `native` when a native scanner is available **and** the docker host (`docker info`) or this machine (`uname -m`) is arm64; `docker` otherwise. The choice and its reason are printed |

**Why native exists.** The scanner image is published for linux/amd64 only, in
12.1 and still in 12.4, while the server image is multi-arch. On an arm64
docker host (Apple Silicon) the whole scanner therefore runs emulated: its JVM,
the Node.js JS/TS analyzer and the sonar-go converter binary. Under colima with
qemu the converter crashed on several Go packages, a different set on every
run (core dumps from `qemu-x86_64`), and the scanner silently never analyzed
those packages; runs were also slow. Rosetta stops the converter crashes, but
everything is still translated. A native scan runs nothing emulated: on an
Apple Silicon host the scanner takes about one minute for this repository,
against about three and a half minutes for the image under Rosetta (SonarQube
26.8, SonarScanner CLI 8.4 and 8.0.1, measured).

**Same analysis, host paths.** Both modes pass the same properties:
`sonar-project.properties`, the per-analysis `sonar.projectVersion`, the token
(in the environment, so it never shows in the host's process list) and
`SONAR_SCANNER_EXTRA_ARGS`. Native mode runs from the repository root with
`sonar.projectBaseDir` set to it, so the relative coverage report paths resolve
the same way they do under `/usr/src`; its working directory is the same
`.scannerwork/` the report task is read from; and it reaches the server at
`http://127.0.0.1:9000` instead of the compose network's `sonarqube:9000`. A
linked worktree needs no extra mount: the host sees the common `.git`.

**A floor, not a pin.** The scanner CLI is only a bootstrapper: it downloads the
scanner engine, the JRE that runs it (for the host's platform) and every
language analyzer from the server it talks to (observed with CLI 8.0.1 in the
image and 8.4 natively, against SonarQube 26.8: both ran engine 13.4.1 on a
server-provided JRE 21). With the server pinned, the analysis is the same
whichever CLI release starts it. Native mode
therefore checks a minimum only: `sonar-scanner -v` must report at least the
CLI release the pinned image bundles (the tag's part after `_`, today
`8.0.1`), and the scan stops before anything starts when it reports less, or
nothing readable. Bumping the image pin raises the floor with it.

**JVM options.** The scanner is two JVMs. `SONAR_SCANNER_OPTS` (default
`-Xmx2048m`) sets the options of the CLI's own **launcher** JVM, which only
bootstraps; the analysis runs in a separate **engine** JVM, whose options come
from `SONAR_SCANNER_JAVA_OPTS`. Heap sizes set in `SONAR_SCANNER_OPTS` never
reach the analysis. Because the Go analysis needs about 4 GB of engine heap
(§7.1, operational notes), `sonar.sh` gives the engine a heap of its own when
`SONAR_SCANNER_JAVA_OPTS` is unset:

| Mode | Engine default | Why |
|---|---|---|
| `native` | `-Xmx6g` | Room above the 4 GB the Go analysis needs; a quarter of a 16 GB host's memory falls short |
| `docker` | `-Xmx5g` | Passes in a 12 GB VM next to the server (measured); the colima/Docker VM needs **at least about 8 GB** for it |

A set `SONAR_SCANNER_JAVA_OPTS` always wins and is passed through unchanged, in
both modes. Every scan prints the effective engine options and where they came
from, and adds a note when `SONAR_SCANNER_OPTS` carries `-Xmx` while
`SONAR_SCANNER_JAVA_OPTS` is unset. For example:

```sh
SONAR_SCANNER_JAVA_OPTS="-Xmx8g -XX:+UseG1GC" make sonar-scan
```

`SONAR_SCANNER_EXTRA_ARGS` appends arguments, split on whitespace, to the
scanner's command line in either mode, for one-off properties such as
`-Dsonar.javascript.node.maxspace=4608`.

## 8. License headers & generated code

- Every source file carries an `SPDX-License-Identifier: AGPL-3.0-only`
  header; `scripts/add-license-headers.sh` adds missing ones (run it after
  code generation). `scripts/gen-third-party-notices.sh` regenerates
  `THIRD-PARTY-NOTICES.md`. Dependency-license policy: see
  [Licensing](licensing.md).
- **Generated code** is excluded from analysis (`**/*_gen.go`): the OTel
  method-tracing decorators are generated by gowrap. The `portal.API`
  interface itself is interfacer-generated in origin but the generator no
  longer works on the current codebase — signature changes are maintained by
  hand (see §11.4).
- **Concretely, when you add a method to `routing.Store` or `portal.API`:** run
  `go generate ./internal/tracing/...` (in `gateway/backend`). gowrap rewrites
  the three generated targets (`internal/tracing/routingstore_gen.go`,
  `internal/account/api_tracing_gen.go`, `internal/portal/api_tracing_gen.go`)
  and **does not preserve their SPDX headers**; the package's last
  `go:generate` step, in `internal/tracing/zz_license_headers.go` (its name sorts
  after every generated file, and `go generate` runs a package's files in name
  order), runs `scripts/add-license-headers.sh` on exactly those three files, so
  the command ends diff-clean. **Every regeneration must keep the headers:**
  running one generated file's embedded directive alone (for example
  `go generate ./internal/portal`) skips that step, and then
  `scripts/add-license-headers.sh <file>` (it takes file arguments since this
  step exists; without arguments it sweeps every in-scope file) must follow, or
  the file lands without its license header. A missing header is a licensing
  defect, not a cosmetic diff
  ([ADR-001](../09-architecture-decisions.md#adr-001--license-agpl-30-only)).
  And `internal/portal/api.go` carries a "DO NOT EDIT" banner but **must** be
  hand-edited, so the edit is not the violation it looks like.
- **Hand-maintained tables copied from llama.cpp follow its pin.**
  `internal/portal/llama_flags.go` (`llamaServerValueFlags`) lists every
  llama-server option that takes a value, with the number of argv elements the
  value occupies, at the pinned llama.cpp build
  (`2bc46baa9207d098d03741ac394e7faaf1a955b6`, the commit the CI RPC cluster
  job builds). The cluster head save checks use it to step over option values,
  so an argument such as `--alias -dev` is read as one option and its value,
  not as a device flag. There is no generator in the repository: the table was
  extracted from that commit's `common/arg.cpp` (every option registered for
  the server example or common to all examples whose definition carries a value
  hint, short and long spellings alike, 304 entries), and **it must be
  regenerated the same way whenever the pinned commit moves**. A stale table
  fails safe but wrong: an option missing from it is read as taking no value,
  so its value may be mistaken for an option (the scan's behaviour before the
  table existed), and a removed one is merely unused. `TestLlamaServerValueFlags`
  pins only a handful of entries, so it does not catch a stale table. The
  portal keeps a copy of the same table in
  `gateway/frontend/src/components/shared/clusterArgs.ts` for its pre-save
  check; regenerate it together with `llama_flags.go`, or the portal and the
  gateway read the same args differently.

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

**A stub server that discards `*http.Request` cannot fail on the request
being wrong — and that is how a probe that could never succeed shipped
green.** The agent's probe test server declared its handler as
`func(w, _ *http.Request)` (`server-agent/internal/collector/probe_test.go`),
recording only the canned body it served back. So no test in that package had
ever asserted a probe's method, path or body, and `ProbeContext` spent its
whole existence sending a **`GET`** to Ollama's POST-only `/api/show` — which
answers `405 text/plain` — while `TestProbeContext_Ollama` stayed green,
because the stub answered the wrong verb with the right JSON (issue #54). The
fix that prevents the CLASS rather than the instance is the helper: it now
records the request it received (method, path, body, `Content-Type`) and a
table test asserts the exact shape per spec type, including the assertion
that the four bodiless `GET` rows carry **no** content type while the POST
carries `application/json`. Two rules generalise from it. **A test double
must assert the request it is asked for whenever the request shape is part of
the contract** — an upstream's verb and body are contract, not plumbing. And
**a header set behind a condition needs a test on both sides of the
condition**: the `Content-Type` line was reachable from no test at all until
the first caller passed a non-nil body, so a regression there would have
passed the whole suite while breaking `ShouldBindJSON` dispatch against a
real server.

**An agent's runtime-config document is derived for the server that owns the
agent token it authenticates with.** The development seed `dev-agent-secret`
(`OP_AI_GATEWAY_DEV_AGENT_TOKEN`) belongs to the seeded mock AI server
`mock-server`, so an agent started with that token manages *mock-server's*
runtime — not that of any server created afterwards, however the agent is
otherwise configured. Any dev or test setup that wants a specific server managed
must mint a fresh agent token for it. Otherwise the agent connects, negotiates
and reports healthy while managing a different server's (empty) document: a
silent no-op that is very hard to attribute to the token.

**A struct assembled by hand at more than one site needs a reflective
completeness guard, because a Go literal has no `...rest` spread.** A field added
to such a struct and wired at only one construction site compiles, passes, and
carries a silent zero value everywhere else; nothing in the compiler or a
value-only test flags the omission. The pattern that removes the remembering is a
reflective test that walks the struct's fields and fails when the wiring falls
behind the shape — so *growing the struct* forces the author to touch every site.
Three of these guard the two shapes that have paid for it:

- `routing.Target` is built at `resolver.go`'s `targetFrom` (the canonical full
  builder) and again on the benchmark path.
  `TestTargetFromPopulatesEveryField` (`internal/routing`) resolves a Target from
  a fully-populated `server_agent` candidate and fails on any field left zero;
  `TestBenchmarkTargetReqSetsEveryFieldOrDocumentsOmission` (`internal/gateway`)
  does the same for `benchmarkTargetReq` with a named `benchmarkOmits` allow-list
  for the dispatch fields a benchmark stream legitimately never carries. `Target`
  has no json tags, so both walk field *names* + `reflect.Value.IsZero()`. The
  routing guard's failure message names the field *and* points at every other
  construction site the same new field must be taught -- the benchmark builder
  and the deliberately partial probe targets; the benchmark guard's names the
  field and its sibling builder. `ResponsesLiveTimingsEnabled` (#81) was the
  field that had to be threaded
  in by hand across these sites; the guards make the next one fail loudly instead.
- `ApplicationDTO` and its write request (`CreateApplicationRequest` /
  `UpdateApplicationRequest`) are two hand-maintained field lists with no compiler
  tie. `TestApplicationRequestCoversEveryWritableField` (`internal/portal`) asserts
  DTO↔request set-parity with the deliberate asymmetries named (identity/derived
  read-only DTO fields; the write-only token), and
  `TestCreateAndUpdateApplicationRequestsCarryTheSameFields` pins create and update
  to one writable surface. This mirrors the older
  `TestPutRequestFromDTOCoversEveryWritableField` (`RuntimeSpecDTO` ↔
  `PutRuntimeSpecRequest`), which additionally round-trips values through the pure
  `putRequestFromDTO` mapper — a half applications lack (a request is applied
  inline in `Create`/`UpdateApplication`, with no pure mapper to exercise).

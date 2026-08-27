# AGENTS.md

## Project Summary

This repository contains **OnPrem AI Gateway** (**OP AI Gateway**, OP = On
Premise / On Prem), a Linux-first, cross-platform AI gateway portal, licensed
AGPL-3.0-only.

The system routes AI client requests to AI servers running Ollama, llama.cpp,
or vLLM. It supports OpenAI-compatible clients, Anthropic-compatible clients,
coding agents such as Codex and Claude Code, and a built-in portal chat
experience. It ships with local users, invite-based provisioning, API tokens,
model-based routing with load-aware scoring from agent telemetry, usage
analytics, a multi-platform server-reporting agent, an optional NetBird mesh
with gateway-managed certificates, and a German/English portal UI.

## Documentation Sources Of Truth

Keep each kind of information in exactly one canonical place:

- **`docs/architecture/`** is the canonical description of the system:
  structure, behavior, constraints, decisions, and quality gates. Start at
  [`docs/architecture/README.md`](docs/architecture/README.md) (arc42-style
  index: chapters 01–12, cross-cutting concept docs, and reference docs).
  When behavior or structure changes, the matching architecture document
  MUST be updated in the same unit of work.
- `AGENTS.md` (this file) contains durable repository instructions, required
  workflows, and commands that every coding agent must follow. It points into
  `docs/architecture/` instead of duplicating it.
- `docs/superpowers/specs/` and `docs/superpowers/plans/` are **branch-local
  working documents**: create and use them freely on a feature branch (the
  brainstorming/planning workflow depends on them), but they are **never
  merged to `main`**. Before the branch is finalized for its pull request,
  fold everything durable into `docs/architecture/` and remove the folder
  from the branch. Never cite them as the source of current behavior — the
  architecture docs are.
- `docs/implementation-status.md` is the same kind of **branch-local working
  document**: keep it updated during branch work (handoff state between
  sessions/agents on that branch), fold durable outcomes into
  `docs/architecture/`, and remove it before the pull request is finalized.
  The pull request description carries the final summary; `main` never
  contains this file.
- `CLAUDE.md` is only a compatibility pointer to these files. Never place
  agent instructions, project history, verification logs, or current handoff
  state in it.

When information appears in more than one of these files, remove the
duplicate instead of maintaining parallel copies.

## Branching And Pull Requests

These rules are absolute:

- **Never commit to or merge into `main` yourself.** Every change reaches
  `main` exclusively through a **feature branch and a pull request**; the
  merge is performed by a human reviewer after CI is green. This applies to
  code, docs, and one-line fixes alike.
- Work on a feature branch, preferably in a worktree under
  `.worktrees/<feature-name>`.
- **`docs/superpowers/**` and `docs/implementation-status.md` must not land
  in `main`.** They may — and should — be created and used on the branch
  while working. As the **last step** before the branch is ready for its
  pull request: fold all durable information from them into
  `docs/architecture/`, then delete them from the branch. Verify with
  `git diff --name-only main...HEAD` that neither path appears as added or
  modified in the PR diff.
- Prefer **squash merges** for pull requests so branch-local working files
  never enter `main`'s history.

## Repository Layout

Monorepo. Top-level directories:

- `gateway/backend/`: the Go module `op-ai-gateway` (`cmd/`, `internal/*`).
  Import paths are unprefixed (`op-ai-gateway/internal/gateway`).
- `gateway/frontend/`: the React/TypeScript/Vite portal UI, served under the
  `/portal/` base path.
- `gateway/e2e/`: the Playwright end-to-end suites (own package).
- `gateway/deploy/`: Dockerfiles, `docker-compose.yml`, nginx config, and the
  operator-facing `themes/` directory (Docker build context is `gateway/`).
- `server-agent/`: the standalone server agent (own Go module
  `op-ai-server-agent`, imports nothing from the gateway). Build/test from
  `server-agent/`.
- Repo root: `Makefile`, `scripts/`, `docs/`, `.golangci.yml`,
  `sonar-project.properties`, and the gitignored `data/`, `.worktrees/`,
  `.sonar-local/`.

The authoritative component/package breakdown (both Go modules and the
frontend) is
[`docs/architecture/05-building-block-view.md`](docs/architecture/05-building-block-view.md).
Do not record live branch, worktree, or in-progress-feature state in this
file: inspect Git and `docs/implementation-status.md` at the start of a task.

## Architecture And Constraints — Where To Read

Read the matching document before changing an area. The most load-bearing
ones:

| Area | Canonical document |
|---|---|
| System goals, quality goals | `docs/architecture/01-introduction-and-goals.md` |
| Hard constraints (incl. product naming, licensing) | `docs/architecture/02-constraints.md` |
| External interfaces & context | `docs/architecture/03-context-and-scope.md` |
| Strategy & key mechanisms | `docs/architecture/04-solution-strategy.md` |
| Components/packages (backend, frontend, agent) | `docs/architecture/05-building-block-view.md` |
| Runtime scenarios (request flow, streaming, capture) | `docs/architecture/06-runtime-view.md` |
| Deployment (Docker, nginx path-split, env) | `docs/architecture/07-deployment-view.md` |
| ADR log (incl. Postgres wide-type rule ADR-005) | `docs/architecture/09-architecture-decisions.md` |
| Quality requirements & how they are verified | `docs/architecture/10-quality-requirements.md` |
| Risks, technical debt, deliberate design acceptances (§11.4) | `docs/architecture/11-risks-and-technical-debt.md` |
| Auth, sessions, CSRF, RBAC/roles, payload capture & redaction | `docs/architecture/cross-cutting/security-auth-rbac.md` |
| Persistence: three drivers, dialect seam, migrations, conformance | `docs/architecture/cross-cutting/persistence.md` |
| Routing, scoring, mappings, affinity | `docs/architecture/cross-cutting/routing-and-model-selection.md` |
| Agent-managed model runtime (launch specs, admission, router, feature flags) | `docs/architecture/cross-cutting/agent-runtime-manager.md` |
| Telemetry, usage analytics, SSE, observability | `docs/architecture/cross-cutting/telemetry-usage-observability.md` |
| NetBird mesh, gateway-managed PAT + rotation | `docs/architecture/cross-cutting/networking-mesh.md` |
| Certificates & TLS (edge + mesh, agent proxy) | `docs/architecture/cross-cutting/certificates-tls.md` |
| OpenAI/Anthropic compatibility, streaming timeouts, body-size policy | `docs/architecture/cross-cutting/compatibility-and-inference.md` |
| Theming (built-in + external themes), i18n | `docs/architecture/cross-cutting/theming-and-i18n.md` |
| Dependency-license policy (AGPL-3.0-only) | `docs/architecture/cross-cutting/licensing.md` |
| Configuration mechanics | `docs/architecture/cross-cutting/configuration.md` |
| Architecture tests (frozen dependency rules) | `docs/architecture/cross-cutting/architecture-tests.md` |
| Formatting, linting, CI, test suites, Sonar gate | `docs/architecture/cross-cutting/development-and-quality.md` |
| Env vars / HTTP API surface / data model | `docs/architecture/reference/` |

Always-on engineering rules (details in the documents above):

- Preserve stable error codes; clients and coding agents depend on
  predictable failures.
- Never store plaintext secrets (API tokens, passwords, session/reset
  tokens); new decryptable secrets follow the `enc:`/`plain:` at-rest scheme.
- Do not persist prompts or responses except via the opt-in payload capture
  (see the security doc: `capture_enabled` kill switch, per-token opt-in,
  `capture_override`, secret captures, header redaction, encrypted-at-rest
  vs volatile-RAM modes).
- Both auth modes stay supported side by side: session cookie + `X-OP-CSRF`
  for browsers, bearer tokens for programmatic clients. `/v1/chat/completions`
  additionally accepts the session (portal chat runs build on this via the
  internal loopback path); `/v1/responses` and `/v1/messages` are bearer-only.
- Keep compatibility mapping isolated in `internal/compat`; keep the internal
  inference model provider-neutral.
- Keep all three store drivers (memory/sqlite/postgres) working; dialect
  differences go through the `dialect` seam; schema changes are append-only
  versioned migrations; wide Go values need wide Postgres columns (ADR-005)
  — verify store changes with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Dependencies must be AGPL-3.0-compatible (permissive + AGPL/GPLv3+/LGPL/
  MPL-2.0); review the real, effective license including bundled code.
- Portal localization: add translation keys in German and English together
  (`i18n.ts`; the type-checked build enforces parity).
- Bump the ServerAgent version when you change the agent — see the rule below.
- Avoid broad refactors unrelated to the current task; deliberate
  kept-as-is structures are catalogued in §11.4 — do not "fix" them ad hoc.

## ServerAgent Version Rule

The gateway and the agent ship and upgrade independently, and the gateway
negotiates agent capabilities by **named feature flags, never by comparing
version numbers** (ADR-025). The reported version is therefore not a gate — it
is the diagnostic an operator reads in the portal's agent panel to know *which
build is in the field*. A stale version number makes that diagnosis a lie.

**The single place to edit is `const Version` in
`server-agent/internal/agent/agent.go`.** Nothing else declares the agent
version: it rides on every telemetry sample as the top-level `agent_version`
field, and that field is what the gateway persists and the portal renders. Do
not add a second copy — a nested `capabilities.agent_version` existed once and
was removed precisely because the gateway ignores it while reviewers assume it
is honoured.

**Bump it whenever a change alters the agent's behaviour or its wire contract.**
Concretely, a bump is required for:

- a new or removed entry in `agent.Features` (`internal/agent/features.go`);
- any change to what the agent sends or accepts — a telemetry sample field, a
  WebSocket frame type or payload, a request the agent makes to
  `/api/agent/v1/*`, or how it interprets a response;
- a change in observable runtime behaviour: lifecycle states, error codes,
  timings and cadences, retry or backoff discipline, redaction, the router's
  routes or its routing rule, the local security policy;
- a change to how the agent reads its own configuration (a new setting, a
  changed default or precedence);
- a bugfix that changes what an operator or the gateway observes.

No bump is needed for a change with no observable effect: refactors, comments,
tests, tooling, or a fix to code no shipped path reaches.

**Which digit.** MINOR when `agent.Features` gains a flag (that is a new
capability the fleet can negotiate); PATCH otherwise. MAJOR is reserved for
dropping support for a gateway the agent previously worked against.

**A bump is per shipped change, not per commit.** One feature branch is one
bump, however many commits it contains: bump once, early, and leave it alone for
the rest of the branch — a second bump inside the same branch is churn, and
bumping per commit makes the number meaningless. (The agent-runtime-manager
branch is the worked example: `Version` went to `0.2.0` with the first agent
commit and that one bump covers the whole feature.)

**What is and is not enforced.** A guard test asserts that every
`agent.Features` entry carries a valid SemVer `Since` no greater than `Version`,
unique and snake_case — so adding a flag without the MINOR bump fails a test. A
forgotten **PATCH** bump after a plain bugfix is not machine-detectable: the
test has no signal for what changed. That half is this rule plus the pull-request
checklist, and nothing else.

## Required Working Style

Use small, verifiable steps. Keep the repository in a state where another
coding agent can continue without reconstructing context from chat history.

Required sequence for substantial work:

1. Inspect the relevant `docs/architecture/` documents for the area (and
   `docs/implementation-status.md` if the current branch carries one).
2. Check `git status --short`.
3. Create a feature branch/worktree (`.worktrees/<name>`) — never work
   directly on `main`.
4. For new feature areas, write a spec (and, for multi-step work, a plan)
   under `docs/superpowers/` on the branch before implementation.
5. Use test-driven development for behavior changes.
6. Track handoff state in `docs/implementation-status.md` on the branch
   after each meaningful milestone.
7. Run focused tests after each task and full verification before
   completion.
8. Update the matching `docs/architecture/` documents with everything
   durable (design decisions, behavior, constraints) — in the same branch.
9. Run the local SonarQube quality gate whenever the environment allows it
   (`make sonar-up` once, then `make sonar-gate`), and act on the result
   before the cleanup below: the gate is new-code based, so what it reports
   is this branch's own work. `make sonar-branch-findings` narrows the
   export to the lines this branch changed. If Docker or the server is
   unavailable, say so explicitly in the pull request rather than staying
   silent about a skipped gate. See
   [`development-and-quality.md` §7](docs/architecture/cross-cutting/development-and-quality.md).
10. **Final step before the pull request:** remove `docs/superpowers/` and
    `docs/implementation-status.md` from the branch (their durable content
    must already live in `docs/architecture/`).
11. Push the branch and open a pull request. Do not merge it yourself.

Do not rely on chat history as the source of truth. Persist decisions in docs.

## Skills And Process Expectations

This project is developed with these process skills (or their manual
equivalents): `brainstorming` (design before code), `writing-plans`,
`using-git-worktrees` (prefer `.worktrees/<feature-name>`),
`test-driven-development`, `systematic-debugging`, `requesting-code-review`,
`receiving-code-review`, `verification-before-completion`,
`finishing-a-development-branch`. If a different agent environment does not
provide these exact skills, follow the same process manually.

When `finishing-a-development-branch` offers integration options, the only
allowed path is **push and create a pull request** — never "merge back
locally". Run the working-file cleanup (sequence step 9 above) before
pushing.

## Build And Verification Commands

Toolchain baseline: Go 1.26+ (backend declares 1.25.0, server-agent 1.26),
Node.js 22 + npm. The full tooling and
quality-gate reference (make targets, lint/format, CI, e2e suite catalog,
SonarQube gate) is
[`docs/architecture/cross-cutting/development-and-quality.md`](docs/architecture/cross-cutting/development-and-quality.md).

From the repository root:

```bash
make test-go
```

Provider adapter tests use `httptest.NewServer`; constrained sandboxes may
need permission to open local loopback listeners for `go test ./...` (run
from `gateway/backend`).

```bash
make test
```

For the portal:

```bash
cd gateway/frontend
npm test
```

```bash
cd gateway/frontend
npm run build
```

Go lint/format (both modules; pre-commit hook via `make hooks`):

```bash
make lint
```

Security/audit check for portal dependencies:

```bash
cd gateway/frontend
npm audit --audit-level=moderate
```

End-to-end tests (Playwright drives the real gateway plus the real built
portal UI through a proxy that mirrors the production nginx path-split):

```bash
make test-e2e
```

Scenario-specific e2e suites run via npm scripts in `gateway/e2e`
(catalog in the development-and-quality doc), e.g.:

```bash
cd gateway/e2e
npm run e2e:smtp
```

Local full-stack dev run (gateway in memory mode + `vite dev`, health-gated):

```bash
make dev
```

Docker test deployment (distroless backend + nginx-baked frontend +
PostgreSQL; requires `gateway/deploy/.env`, see `gateway/deploy/.env.example`):

```bash
cd gateway/deploy
docker compose up --build
```

Gateway smoke start:

```bash
OP_AI_GATEWAY_DEV_TOKEN=dev-secret make run-gateway
```

SQLite-backed local gateway start:

```bash
export OP_AI_GATEWAY_BOOTSTRAP_API_TOKEN="$(openssl rand -hex 32)"

OP_AI_GATEWAY_DB_DRIVER=sqlite \
OP_AI_GATEWAY_SQLITE_PATH=./data/op-ai-gateway.db \
OP_AI_GATEWAY_BOOTSTRAP_ADMIN_EMAIL=admin@example.test \
OP_AI_GATEWAY_BOOTSTRAP_ADMIN_NAME="Admin User" \
make run-gateway
```

Session login smoke request (memory-mode dev user, cookie + CSRF header):

```bash
curl -s -i http://127.0.0.1:8080/api/auth/login \
  -H 'Content-Type: application/json' -H 'X-OP-CSRF: 1' \
  -d '{"email":"dev@example.test","password":"dev-secret"}' \
  -c op-session.txt
```

```bash
curl -s http://127.0.0.1:8080/api/portal/me -H 'X-OP-CSRF: 1' -b op-session.txt
```

OpenAI-compatible smoke request:

```bash
curl -s http://127.0.0.1:8080/openai/v1/chat/completions \
  -H 'Authorization: Bearer dev-secret' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}'
```

Anthropic-compatible smoke request:

```bash
curl -s http://127.0.0.1:8080/anthropic/v1/messages \
  -H 'Authorization: Bearer dev-secret' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-coder","messages":[{"role":"user","content":"hello"}]}'
```

Portal API smoke requests:

```bash
curl -s http://127.0.0.1:8080/api/portal/me \
  -H 'Authorization: Bearer dev-secret'
```

```bash
curl -s http://127.0.0.1:8080/api/portal/servers \
  -H 'Authorization: Bearer dev-secret'
```

```bash
curl -s http://127.0.0.1:8080/api/portal/models \
  -H 'Authorization: Bearer dev-secret'
```

Agent telemetry smoke request (in loopback dev mode the mock server is seeded
with a reporting token whose secret is `dev-agent-secret`; override with
`OP_AI_GATEWAY_DEV_AGENT_TOKEN`):

```bash
curl -s http://127.0.0.1:8080/api/agent/v1/telemetry \
  -H 'Authorization: Bearer dev-agent-secret' \
  -H 'Content-Type: application/json' \
  -d '{"agent_version":"0.1.0","reported_at":"2026-07-10T12:00:00Z","os":"linux","arch":"amd64","cpu_load":0.2,"active_requests":1,"queue_depth":0,"latency_ms":100,"error_rate":0,"provider_health":{},"capabilities":{}}'
```

The full HTTP API surface (portal, agent, compatibility endpoints) is
documented in `docs/architecture/reference/api-surface.md`.

## Documentation Update Rule

Update `docs/architecture/` (the matching chapter/cross-cutting/reference
document) whenever structure, behavior, constraints, or decisions change —
in the same branch as the change itself. This is what survives on `main`;
everything durable must end up here.

While working on a branch, update `docs/implementation-status.md` (on the
branch) whenever:

- a task starts or finishes,
- review findings are received or resolved,
- verification status changes,
- the next planned step changes.

Remember: this file is removed again before the pull request (see Branching
And Pull Requests).

Update `README.md` — including regenerating the affected screenshots under
`docs/images/` — whenever a change alters what they show or describe:
user-visible UI that appears in a screenshot, commands, endpoints, setup
steps, or feature claims. The screenshots are captured from the `make dev`
stack via Playwright (English UI, dark mode, 1600×1000 viewport at
2× device scale, with a little seeded traffic so the tiles/charts show real
data); keep new captures consistent with that format.

Update this `AGENTS.md` whenever:

- repository structure changes,
- new required commands are introduced,
- workflow expectations change,
- a future agent would otherwise need chat history to continue correctly.

# Portal Chat Image Generation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A portal chat user picks a model that can generate images, types a
prompt, and gets the generated image back in the thread — persisted with the
conversation, visible on reload, and billed like any other turn.

**Architecture:** The existing chat run executor gains a second target
(`POST /v1/images/generations` instead of `/v1/chat/completions`), chosen from
a `kind` pinned to the thread on its first send. The generated image is stored
inline in the sealed chat transcript as an OpenAI-style `image_url` content
part, exactly as an uploaded vision image already is. `/v1/images/generations`
moves from bearer-only auth to a narrow loopback-or-bearer leg so the run
executor can reach it without opening the endpoint to browsers. Because that
endpoint emits zero incremental events, the composer leads with the one number
that is exact and known before the user commits — the remaining transcript
capacity — and shows an elapsed clock rather than any form of progress.

**Tech Stack:** Go 1.x (two modules: `gateway/backend`, `server-agent`),
React + TypeScript + MUI (`gateway/frontend`), Vitest, Playwright, three store
drivers (memory, sqlite, postgres) behind a dialect seam.

**Source spec:** `docs/superpowers/specs/2026-09-18-portal-chat-images-design.md`
(437 lines). Read it before starting: it records what was decided, what was
rejected and why, and eight pre-existing defects this feature makes reachable.

## Global Constraints

Every task's requirements implicitly include this section.

**Branching (absolute, from `AGENTS.md`).** Never commit to or merge into
`main`. All work happens on `feat/portal-chat-images` in the worktree
`.worktrees/portal-chat-images`. The merge is performed by a human after CI is
green.

**Branch-local docs.** `docs/superpowers/**` must never land in `main`. It is
created and used freely while working; as the **last** step before the PR,
fold everything durable into `docs/architecture/` and delete the folder from
the branch. Verify with `git diff --name-only main...HEAD` that no
`docs/superpowers/` path appears.

**No agent version bump.** The version rule in `AGENTS.md` governs
`const Version` in `server-agent/internal/agent/agent.go` — the standalone
agent. This feature touches the gateway backend and the portal frontend only,
so **do not bump it**, and do not add a second version constant anywhere.

**Verification gates — the exact commands CI runs.** `make lint` and
`make test` do **not** cover all of them; run these directly.

Go, for each of the two modules (`gateway/backend` and `server-agent`):

```bash
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

`golangci-lint fmt --diff` is a separate gate from `run` and is **not** part of
`make lint-go`; gofumpt and gocritic findings surface only there. The pinned
version is `v2.13.1` (`make lint-install`).

Frontend, in `gateway/frontend`:

```bash
npm run format:check && npm run lint && npm run build && npm test
```

`npm run format:check` is prettier and is the gate a local `test`+`build`+`lint`
run misses most often. `npm run build` is `tsc && vite build`, so it is also the
type gate.

Docs:

```bash
make lint-docs
```

**Running a single test.** Go:
`cd gateway/backend && go test ./internal/gateway/ -run 'TestName' -v`.
Frontend: `cd gateway/frontend && npx vitest run src/components/Chat.test.tsx -t 'test name'`.

**Postgres.** Store and migration changes must be verified with the postgres
leg, which **skips silently** when the DSN is absent — a green run without it
proves nothing about postgres:

```bash
docker start op-pg-test
export OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable'
```

**e2e.** `make test-e2e` runs a suite **no CI job covers** and is red on `main`
itself — CI runs `npm run e2e:runtime` only (19 other Playwright suites are
local-only, `ci.yml:112-113`). Do not treat its failures as branch-caused
without a baseline on the merge-base, and do not treat green CI as evidence
about it.

**Every new user-facing string needs both locales.** `gateway/frontend/src/i18n.ts`
declares German and English blocks; a key added to one and not the other is a
type error at `npm run build`.

**Licence header.** Every new file starts with the two-line SPDX header used
throughout the repo:

```
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors
```

**Commit discipline.** Commit at the end of each task's step list. This repo's
GitHub squash-merge pre-fills the squash description from the **commit message
body**, not the PR body, so write substantive bodies: what changed, why, and
what was verified.

**Repo-facing text is English.** Commits, PR text, code comments and issue text
are English.

---

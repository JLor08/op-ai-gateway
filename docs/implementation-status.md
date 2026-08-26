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

## Next planned step

1. Continue the plan at Task 7 (agent endpoints: features + runtime-config ETag).
2. Remaining: Tasks 7-10 (gateway), 11-18 (agent), 19-22 (portal UI),
   23 (e2e), 24 (docs + Sonar gate + working-file cleanup before the PR).

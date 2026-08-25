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
- Awaiting: user review of the written spec, then the implementation plan
  (writing-plans skill) under `docs/superpowers/plans/`.

## Context worth keeping on this branch

- A 125-finding verified cold-load/TTFT timeout inventory was produced with a
  multi-agent sweep (2026-08-25). Durable facts are folded into spec §8; the
  raw inventory lives outside the repo (session task output) — the durable
  home will be `docs/architecture/` when this branch documents §8.
- Two independent fixes were spun off as separate task suggestions (not on
  this branch): mislabeled `provider.unavailable` on native pre-header idle
  timeout; hardcoded 60 s `warmCallTimeout`.

## Next planned step

1. User reviews the spec file.
2. Write the implementation plan (writing-plans) on this branch.

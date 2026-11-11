# Implementation status: decision endpoints Phase 1 (#373)

Branch-local handoff file. It is removed before the PR.

- Branch: `feat/decisions-systemone-373` (from `main` @ 7e26f704).
- Spec: `docs/superpowers/specs/2026-11-11-decisions-systemone-phase1-design.md`
- Plan: `docs/superpowers/plans/2026-11-11-decisions-systemone-phase1.md` (Tasks 1–8)
- Flow: subagent-driven (one implementer per task, a task review after each, a final whole-branch review).

## State

- [ ] Task 1: routing vocabulary
- [ ] Task 2: decisions-only exclusions
- [ ] Task 3: body cap config
- [ ] Task 4: `POST /v1/systemone` relay
- [ ] Task 5: `GET /v1/systemone/models`
- [ ] Task 6: verdict probe
- [ ] Task 7: portal
- [ ] Task 8: docs + ADR-097
- [ ] Full verification (go test, golangci, frontend test/build/lint/format, lint-docs, sonar branch-findings)
- [ ] Live check (d3-mini on vLLM ≥ 0.33.0)

## Next step

The spec and plan wait for the owner's review. After the OK, start Task 1.

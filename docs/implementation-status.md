# Implementation status — live-timings rejection observable (#81 residuals)

Branch-local working file. Removed before the pull request (AGENTS.md).

## Goal

Close the five residuals an audit of issue #81 found after PRs #82 and #86
shipped the Responses live-timings opt-in:

1. A rejection learned on the translate path does not suppress the native
   injection — same `RouteID`, two endpoints, one memo the native path never
   reads or writes.
2. `live_progress` is still mintable as a **manual** capability verdict; a
   rank-3 `manual` "no" permanently and silently vetoes the operator's own
   opt-in. (#81's own third R1 ask, never implemented.)
3. The feature's purpose is untested end to end: every gate-ON test answers
   with a single `response.completed` frame carrying no `timings`.
4. The only diagnostic that names the injection rides `slog.Debug` while the
   default level is `info` — the "one grep" the docs promise returns nothing.
5. Two mode-conditional coverage gaps: no end-to-end negative for the
   capable-kind condition, and nothing pins that a translate-mode application
   carrying the opt-in gets no injection.

## Design (fixed before implementation, by the Understand pass)

- **The memo is already shared**; only a seam is missing. Production builds ONE
  `*provider.OpenAICompatibleClient` registered under five provider keys inside
  one `*provider.Multiplexer`, and `dispatchProxyNative`/`dispatchCompleteStream`
  key on the same `target.Provider`. So recording through the Multiplexer really
  does make both endpoints stop asking.
- **A new OPTIONAL capability interface**, not a method on
  `provider.NativeProxyClient` — that interface has eight implementations, six of
  them test fakes. Precedent: `s.Provider.(provider.NativeProxyClient)`.
  The methods take `routing.Target` so the Multiplexer can dispatch.
- **`wantsResponsesLiveTimings` stays PURE.** Its doc comment makes that a
  deliberate, table-tested property, and it already predicted this bug: "a
  fourth check, its rejection memo, is ANDed at its only call site that a caller
  from here would silently drop". The memo check is ANDed at the call site,
  exactly as `openai_compatible.go:467` does for `wantsLiveProgress`.
- **One rejection class, shared.** `schemaRejectionStatus` (400/422, never 503)
  becomes exported so both paths agree by construction rather than by comment.
- **The portal refusal is verdict-dependent, not name-dependent.** A SET
  ("yes"/"no") on a reserved name is refused; the RESET ("") still deletes the
  row. Refusing the reset would re-open a risk §11.1 records as CLOSED and make
  a stored bad row permanently uncorrectable — which is the very objection
  `normalizeCapabilityVerdicts`' own doc raises against name-whitelisting.
- **Reserved set: `live_progress` and `speculation_observed`; NOT `mtp`.** `mtp`
  has a deliberate operator control on the mapping form; the other two have no
  legitimate operator writer at all.
- **`slog.Warn` for the new line**, the house level for a refusal in this
  package (24 sites) and on at the default `info`. A separate conditional line,
  not a promotion of the per-request debug line, which would be per-request spam.

## Constraints honoured

- `internal/portal` may not import `internal/gateway` (forbidden edge,
  `archtest`): the portal gets its own reserved set keyed on
  `routing.Capability*` constants, imitating `agent_ingest.go`'s, not sharing it.
- No new package edge (`gateway→provider`, `portal→routing` both exist), so
  `allowedDeps` and the architecture-tests prose are untouched.
- No ServerAgent version bump: nothing in `server-agent/` changes.
- No frontend behaviour change: `MappingForm.tsx` sends `mtp`/`vision` only, so
  no UI can trigger the new 400; prose comments only.

## Progress

- [x] Worktree + Understand pass (6 parallel readers, change sites pinned)
- [x] Task 1: provider seam (exported interface + Multiplexer dispatch + exported rejection class)
- [x] Task 2: gateway wiring (call-site AND, Warn line, memo record)
- [x] Task 3: portal reserved-name refusal (SET refused, RESET kept)
- [x] Task 4: the missing end-to-end tests (residuals 3 + 5)
- [x] Task 5: docs (five condition-count restatements, three false claims, ADR-039, api-surface)
- [x] Committed as 8e64886
- [ ] Gates (in progress; see below)
- [ ] Adversarial review + fixes
- [x] Sonar: **0 findings attributed to lines this branch changed** (246 open project-wide, all pre-existing; analysis revision `743bf65` = HEAD, base merge-base `3b8aa88`)
- [ ] Remove this file, push, open PR

## Gate results

| Gate | Result |
|---|---|
| `go build ./...` (backend) | clean |
| `golangci-lint fmt --diff` | silent, both modules |
| `golangci-lint run` | **0 issues**, both modules |
| backend `go test ./...` | **23 packages ok**, 0 FAIL |
| PostgreSQL leg (`OP_AI_GATEWAY_TEST_POSTGRES_DSN` set) | **137 postgres-dialect subtests**; exactly 3 skips, all documented dialect skips, none missing-DSN |
| `-race` (gateway, provider, portal, routing, store) | **0 data races**, exit 0 (gateway 840s — hence `-timeout=25m`) |
| `scripts/check-docs.sh` | OK (44 md files, 582 anchors, 641 links, 29/29 reachable, 260 $refs) |
| frontend `format:check` | clean |
| frontend `lint` | 0 errors, 13 warnings — byte-identical to `main`, none on a changed file |
| frontend `build` | clean |
| frontend `npm test` | **113 files / 2976 tests** passed |

Note: the worktree needed its own `npm ci` — symlinking the main checkout's
`node_modules` passes prettier/eslint/build but breaks vitest's `/@fs` module
resolution (all 113 files fail to import `@testing-library/jest-dom`).

## Mutation proofs (each caught by exactly the test that claims it)

| Mutation | Caught by |
|---|---|
| `Multiplexer.liveProgressMemoFor` ignores the provider key (fallback only) | `TestMultiplexerDispatchesRejectionMemoToTheResolvedClient` |
| gate drops `routing.LiveTimingsCapableKind` | both new refusal-table rows |
| call site drops the memo conjunct | `TestRecordedRejectionSuppressesTheNextInjection` |
| rejection logged but not recorded | `…IsRecordedForBothEndpoints`, `TestNonSchemaRejectionIsNotRecorded`, `…SuppressesTheNextInjection` |
| the new line drops from Warn to Debug | `TestInjectedTimingsRejectionIsVisibleAtTheDefaultLogLevel` |
| reservation also blocks the RESET | `TestMappingCapabilityVerdictReservedNamesStayResettable` |
| reservation removed | `TestMappingCapabilityVerdictReservedNamesAreRefusedOnSet` |
| reservation moved BEFORE the value check | that test's ordering row |

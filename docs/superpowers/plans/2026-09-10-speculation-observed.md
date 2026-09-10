# Speculation Observed Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the flat +30 MTP scorer bonus and everything that fed only it, and record instead — as information, never as a routing input — that an endpoint was observed speculating.

**Architecture:** Two deletions and one addition. The bonus and its whole one-directional feed chain go, including a filtered LEFT JOIN on the request path, which makes the candidate query *cheaper*. In its place, a positive-only capability written from the drafted-token counter llama.cpp already puts on relayed traffic, at most once per mapping per gateway lifetime.

**Tech Stack:** Go (`gateway/backend`), the React portal, no new dependencies, no schema change.

## Global Constraints

- **The new capability is INFORMATION.** No scorer term, no routing filter, no candidate join, no `Target` field. If a task finds itself adding one, stop and report it.
- **Positive-only, and that is structural**: llama.cpp emits `draft_n` under `if (n_draft_tokens > 0)`, so the key is **absent, not zero**, when speculation is off. Never write a `no` verdict for it, and never read absence as a denial.
- **Absence of a row means unknown**; `verdict` is only ever `yes` or `no`; `''` is never stored.
- **The rank rule is untouched**: `manual` 3 > `vision_benchmark` 2 > probe sources 1 > no row 0, write iff `rank(incoming) >= rank(current)`. The new source ranks 1 through `capabilitySourceRank`'s **default** branch — add no case to the rank table.
- **`mtp` keeps its meaning** (an operator/heuristic claim about the model's architecture) and stays written by the name heuristic. Only its *justifications* change.
- **At most one database write per mapping per gateway lifetime** for the new verdict. A per-request read to compare is not acceptable.
- **The two `detectCapabilities`/`detectLiveProgressSupport` twins stay byte-identical** across the modules; this plan does not touch them.
- Every new/changed test must FAIL with its production change reverted. Revert **production files only** — reverting a test makes `go test -run X` report "ok" with zero tests, a fake pass. For a deletion, the discriminator is the reverse: state which test *would* fail if the deleted behaviour came back.
- **Per-module gates before every commit:** `~/go/bin/golangci-lint fmt --diff` (must print nothing) + `run` + `go test ./... -count=1`. Frontend → `npm run format:check` + `lint` + `build` + `test`. Docs → `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- **The PostgreSQL leg is mandatory for Task 2** — it removes a column from the candidate query in the shared implementation both dialects use, and it skips **silently** without `OP_AI_GATEWAY_TEST_POSTGRES_DSN`. Count with `^ +--- (PASS|FAIL|SKIP): .*/postgres`; baseline 125 PASS + 1 documented sqlite-only SKIP.
- `go test ./internal/gateway/ -race` has a pre-existing failure on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (#53) — use plain `go test` there.
- Branch `speculation-observed`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/speculation-observed`. Never commit to `main`. Never bare `git stash`. Never `cp -R` the worktree (its `.git` is a gitlink).
- `docs/superpowers/` is branch-local and removed before the PR.

---

## File Structure

- `gateway/backend/internal/routing/scorer.go` — the bonus and `Route.IsMTP` (Task 1).
- `gateway/backend/internal/routing/store.go` — `MappingCandidate.IsMTP` and `MTPFromVerdict` out (Task 2); the two new constants in (Task 5).
- `gateway/backend/internal/store/sqlite_applications.go`, `internal/routing/memory_store.go` — the join and its mirror (Task 2).
- `gateway/backend/internal/portal/service_applications.go` — the four evaporated justifications (Task 3).
- `gateway/backend/internal/inference/types.go`, `internal/provider/openai_compatible.go`, `internal/gateway/native_passthrough.go` — carry `draft_n` (Task 4).
- `gateway/backend/internal/gateway/inference_complete.go` — the write-once writer (Task 5).
- `gateway/frontend/src/…` — the chip label and i18n (Task 6).
- `docs/architecture/…` — the ADR and the passages (Task 7).

---

### Task 1: Delete the bonus

**Files:**
- Modify: `gateway/backend/internal/routing/scorer.go`
- Test: `gateway/backend/internal/routing/scorer_test.go`; delete `gateway/backend/internal/routing/mtp_capability_join_test.go`

**Interfaces:**
- Produces: `Route` without `IsMTP`; `metricTiebreak`'s bound becomes `genThroughputBonusCap + promptThroughputBonusCap`.

- [ ] **Step 1: Re-baseline the bound test first, and watch it fail**

In `TestScoreTiebreakBounded`, change `want` to `genThroughputBonusCap + promptThroughputBonusCap` and drop `maxed.IsMTP = true`.

Run: `cd gateway/backend && go test ./internal/routing/ -run TestScoreTiebreakBounded`
Expected: FAIL — the bonus is still applied, so the score is 30 above the new want. That failure is the bonus, observed.

- [ ] **Step 2: Delete the bonus**

In `metricTiebreak`, remove the `if route.IsMTP { bonus += mtpBonus }` block; delete the `mtpBonus` constant; correct the block comment above the tiebreak constants and `metricTiebreak`'s own doc, which both state the bound as a three-term sum. Delete `Route.IsMTP` and `scoringRoute`'s `IsMTP:` fill together with the six-line comment that explains how the verdict reached the scorer.

- [ ] **Step 3: Run it green, then handle the two pins**

`TestScoreTiebreakBounded` passes. `TestSelectRoutePrefersMTP` now FAILS — the two routes tie and `Select`'s strict `>` returns the plain one. It exists only to pin the bonus: delete it. Delete `mtp_capability_join_test.go` entirely (57 lines, one test, `scoreWith - scoreWithout == mtpBonus`).

- [ ] **Step 4: Fix the two incidental users**

`TestScoreTiebreakDoesNotRescueNonViable` and `TestScoreTiebreakBeatsSmallPriorityDelta` set `IsMTP` beside other values and mention it in their comments. Drop the field, re-baseline the comments (the second one's margin widens from 20 to 50), and keep both assertions — they prove the viability gate and the priority interaction, not the bonus.

- [ ] **Step 5: Add the replacement test**

```go
func TestSelectRouteNoLongerPrefersTheMTPVerdict(t *testing.T) {
	// Two candidates identical in everything the scorer reads. Before the
	// bonus was deleted the mtp verdict broke this tie; now nothing does,
	// so Select's strict > returns the first.
}
func TestSelectRoutePrefersTheMeasurablyFasterRoute(t *testing.T) {
	// The signal that replaces the bonus: measured throughput, in the same
	// tiebreak, still decides.
}
```

- [ ] **Step 6: Module gates, then commit**

Run: `~/go/bin/golangci-lint fmt --diff && ~/go/bin/golangci-lint run && go test ./... -count=1`

```bash
git commit -m "refactor(routing): delete the flat MTP bonus"
```

---

### Task 2: Delete the join that fed only the bonus

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`, `gateway/backend/internal/store/sqlite_applications.go`, `gateway/backend/internal/routing/memory_store.go`
- Test: `gateway/backend/internal/store/routing_store_conformance_test.go`

**Interfaces:**
- Consumes: Task 1's removal of the only reader.
- Produces: `MappingCandidate` without `IsMTP`; `MTPFromVerdict` gone; the candidate query with one filtered join instead of two.

- [ ] **Step 1: Prove the reader is gone before deleting the producer**

Run: `cd gateway/backend && grep -rn "IsMTP\|MTPFromVerdict" --include="*.go" . | grep -v _test`
Expected: only the declarations and the two store fills — no consumer. **Record that output in the report**; it is the deletion's premise, and a deletion's premise must be verified as hard as an addition's.

- [ ] **Step 2: Delete the SQL half**

In `ActiveMappingsForModel`: the `mtp` LEFT JOIN, its `routing.CapabilityMTP` bind argument, `mtp.verdict` from the select list, the `mtpVerdict sql.NullString` scan target and the `c.IsMTP = …` assignment. Keep the `live_progress` join exactly as it is. Update the comment that records the measured cost of *two* joins so it describes one — and keep the measurement, because Task 7 cites it.

- [ ] **Step 3: Delete the memory mirror and the type**

`MemoryStore`'s `IsMTP:` fill and its `mappingCapabilities[id][CapabilityMTP]` lookup; then `MappingCandidate.IsMTP` with its 16-line doc, and `MTPFromVerdict` (no callers remain — verify).

- [ ] **Step 4: Narrow the conformance test rather than deleting it**

`TestRoutingStoreActiveMappingsForModelReadsCapabilityVerdicts` asserts both verdicts across all three drivers. Remove the `wantIsMTP` column, the `mtp` fixture rows and the `IsMTP` assertion; keep the whole `live_progress` half, which is the test's surviving purpose.

- [ ] **Step 5: Run both legs**

Run plain, then:
```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:55432/op_test?sslmode=disable' go test ./internal/store/ -count=1
```
Count with `^ +--- (PASS|FAIL|SKIP): .*/postgres`. The baseline is 125 PASS + 1 SKIP; report the new number and account for any difference.

- [ ] **Step 6: Commit**

```bash
git commit -m "perf(routing): the candidate query drops the join that fed only the bonus"
```

---

### Task 3: Rewrite the four evaporated justifications, and the two half-tests

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go`, `gateway/backend/internal/routing/mtp.go`
- Test: `gateway/backend/internal/portal/service_applications_test.go`

- [ ] **Step 1: Find all four**

`CreateMapping`'s `case isMTP:` block and `reconcileApplicationModels`' heuristic block both justify writing the row by the bonus ("would silently lose the bonus migration 78 gave every mapping", "would never earn the scorer's ROW-based +30"). `IsMTPModelName`'s comment warns that "a false positive would later bias server selection". `legacyMTPCapabilityRow`'s doc forward-references "PR C's `/slots`-based probe" — which the spec proves impossible. Quote each in the report before rewriting.

- [ ] **Step 2: Rewrite them to the truth**

The row is now a **display and operator-seed** fact: a name-based guess about the model's architecture, `legacy`-sourced so its provenance is visible in the portal tooltip, and correctable by the operator's own three-state control. Say that, and say what a false positive costs now (a wrong chip, not a wrong route). Replace the `/slots` forward reference with the reason it is impossible — `speculative` is true for every technique, and the field that names `draft-mtp` needs a request to have run.

- [ ] **Step 3: Split the two half-tests**

`TestCreateMappingMTPHeuristicEarnsTheScorerBonus` and `TestReconcileApplicationModelsMTPHeuristicEarnsTheScorerBonus` each assert a 30.0 score delta **and then** that a rank-1 probe can replace the `legacy` row. Delete the delta half with its `mtpBonusPoints` constant, keep the row half, and rename each test for what it now proves (the heuristic writes a `legacy` row that a probe may replace).

- [ ] **Step 4: Gates and commit.**

---

### Task 4: Carry the drafted-token counter

**Files:**
- Modify: `gateway/backend/internal/inference/types.go`, `gateway/backend/internal/provider/openai_compatible.go`, `gateway/backend/internal/gateway/native_passthrough.go`

**Interfaces:**
- Produces: `inference.Usage.DraftTokens int` (`json:"draft_tokens,omitempty"`), used by Task 5.

- [ ] **Step 1: Write the failing tests**

One per shape that really carries it: the buffered OpenAI-compatible response, the buffered chat response, and the **final frame** of a chat stream (llama.cpp puts `timings` on `deltas.back()`, which is the same chunk as `usage`). Assert `DraftTokens` is 0 when the keys are absent — absence must not become a guess.

- [ ] **Step 2: Add the field and the three reads**

`Usage.DraftTokens`, then: the buffered decode's `timings` anon struct gains `DraftN int \`json:"draft_n"\`` and assigns it; `streamChunkTimings` gains the same field; `mergeChunkUsage` assigns it **inside** the existing `chunk.Usage != nil` guard, which already sees the terminal cumulative value. Also `mergeResponsesUsage`'s `Timings` struct in the native passthrough, with the same `takeMax` treatment as its siblings — that covers the Responses **stream** only.

- [ ] **Step 3: Document what carries it and what does not**

In the field's doc comment: present on the non-streaming OpenAI-compatible and chat shapes and on a chat stream's final frame; **absent entirely** from the non-streaming `/v1/responses` body, every Anthropic shape and ASR. Absent, not zero, when speculation is off — the upstream guard is `if (n_draft_tokens > 0)`. Do not carry `draft_n_accepted`: an acceptance rate is a performance measure, not a capability.

- [ ] **Step 4: Gates and commit.**

---

### Task 5: Write the verdict, at most once per mapping per lifetime

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`, `gateway/backend/internal/gateway/inference_complete.go`
- Test: `gateway/backend/internal/gateway/…`

- [ ] **Step 1: Add the two constants**

`CapabilitySpeculationObserved = "speculation_observed"` beside the other capability names, and `CapabilitySourceLlamaCppTimings = "llama_cpp_timings"` beside the other sources. **Add no case to `capabilitySourceRank`** — rank 1 via its documented default branch is the `ollama_api_show` precedent. Verify, do not assume, that `ValidateCapabilityRow` accepts both and that the ingest's agent-source allowlist does not apply to a gateway-side writer.

- [ ] **Step 2: Write the failing tests**

A completion whose usage carries `DraftTokens > 0` writes one `speculation_observed`/`yes`/`llama_cpp_timings` row for the target's mapping; a second completion on the same mapping writes **nothing** (assert the store call count, not just the row); a completion with no draft tokens never touches the store at all; and a `manual` row on that capability is not overwritten.

- [ ] **Step 3: Implement**

In `recordUsage`, beside the existing opportunistic-metrics update: if `resp.Usage.DraftTokens > 0`, consult an in-memory set keyed by `target.RouteID`, and on a first sighting write the row and record the key. Failure logs and continues — the completion is already delivered, and the verdict is information. Guard the set for concurrent requests.

- [ ] **Step 4: Prove the once-per-lifetime property**

The mutation is the interesting one: remove the in-memory guard and the second-completion test must fail on the store call count. Record it.

- [ ] **Step 5: Gates and commit.**

---

### Task 6: Show it

**Files:**
- Modify: the portal's capability chip list and its i18n files

- [ ] **Step 1:** Report first what the name does with **no** frontend change — the vocabulary is open, so it already renders verbatim with the neutral badge. Then add the label, its position in the known order, and the i18n key in **every** language (a missing key is a build failure).
- [ ] **Step 2:** Check no existing copy or tooltip claims a capability affects routing — after Task 1 none does.
- [ ] **Step 3:** Frontend gates (`format:check`, `lint`, `build`, `test`) and commit.

---

### Task 7: The ADR and the documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md`, `docs/architecture/cross-cutting/routing-and-model-selection.md`, `docs/architecture/cross-cutting/telemetry-usage-observability.md`, `docs/architecture/reference/data-model.md`, `docs/architecture/reference/api-surface.md`

- [ ] **Step 1: The ADR.** Record: the bonus is deleted because it duplicated the measured throughput term beside it; **the repo meant two different things by "MTP"** (architecture in every definition, deployment speed in every justification) and this writes that down for the first time; `mtp` keeps the architecture reading and is display-only; the new capability is an *observation*, positive-only because the upstream key is absent rather than zero; and `/slots` is rejected with its four reasons. Note explicitly that this deletion **speeds up the request path** by dropping one filtered join — the opposite of the usual trade, and worth recording.
- [ ] **Step 2:** Re-locate every affected passage by heading and wording, never by line number: the scorer's tiebreak description and its point budget, the MTP-bonus row in the routing table, the capability-source vocabulary, the DTO fields.
- [ ] **Step 3:** Docs gates and commit.

# Passthrough Live Progress Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show TTFT and — where a source honestly exists — live tokens/sec in the running-connections panel for native-passthrough streaming requests, which today show nothing.

**Architecture:** One bridge. The passthrough usage scanner already parses every frame with its arrival time, already stamps the first content frame per flavor, and already merges the upstream's reported usage as it arrives; the panel's `liveProgressDTO` already prefers a reported rate over a derived one and already reports which source it used. What is missing is a `requestProgress` on the passthrough `ActiveRequest` and a call into it from the scanner.

**Tech Stack:** Go (`gateway/backend`), the React portal, no new dependencies, no schema change, no store change.

## Global Constraints

- **Display only.** No write to `UpdateMappingOpportunisticMetrics`, the scorer, a candidate, or `Target` may originate from this path. The end-of-request rate already feeds that EWMA — which the scorer and a model group's `MinTokensPerSecond` gate read — and an in-flight sample would not self-correct there. Assert the absence on a **call count**, not a value.
- **Never inject `timings_per_token`** into a relayed body. Passthrough means the client's body reaches the upstream unchanged apart from `rewriteModelField`. Read the flag's effect when the client set it; never set it.
- **Never count deltas as tokens.** The repo's stated policy is that it has no tokenizer to count with, and a mid-stream token count for the Responses flavor has no other source. `—`, not a guess.
- **A not-applicable must never look like a zero**, which is the dual of the doctrine `formatLiveTps` already implements. Absent cells render as an em-dash.
- **Buffered passthrough keeps today's behaviour**: no frames, no first-content stamp, no progress, and no panic on the nil path.
- Every new/changed test must FAIL with its production change reverted. Revert **production files only** — reverting a test makes `go test -run X` print "ok" with zero tests, a fake pass.
- Per-module gates before every commit: `~/go/bin/golangci-lint fmt --diff` (must print nothing) + `run` + `go test ./... -count=1`; frontend `npm run format:check` + `lint` + `build` + `test`; docs `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- `go test ./internal/gateway/ -race` has a pre-existing failure on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (#53) — use plain `go test` there, but DO run the new concurrency-sensitive tests under `-race` explicitly.
- Branch `passthrough-progress`, worktree `/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/passthrough-progress`. Never commit to `main`, never bare `git stash`, never `cp -R` the worktree (its `.git` is a gitlink).
- `docs/superpowers/` is branch-local and removed before the PR.

---

## File Structure

- `gateway/backend/internal/gateway/native_passthrough.go` — allocate the progress on `Active.Add`; the non-injection comment.
- `gateway/backend/internal/gateway/passthrough_usage_scan.go` — feed the progress from `scan`.
- `gateway/backend/internal/gateway/request_progress.go` — an entry point that takes what the scanner has, if `observeDelta`'s current shape does not fit.
- `gateway/frontend/src/…` — the em-dash rule for absent cells, plus i18n if a new string is needed.
- `docs/architecture/…` — the endpoint-mode trade-off and the per-flavor table.

---

### Task 1: The bridge

**Files:**
- Modify: `internal/gateway/native_passthrough.go`, `internal/gateway/passthrough_usage_scan.go`, `internal/gateway/request_progress.go`
- Test: `internal/gateway/…`

**Interfaces:**
- Produces: a non-nil `ActiveRequest.Progress` for a **streaming** passthrough request, populated per flavor.

- [ ] **Step 1: Write the failing tests, one per row of the spec's table**

Per flavor, against a fake upstream emitting real frame shapes:
- `anthropic_messages`: after a `content_block_delta` and a `message_delta` carrying `usage.output_tokens`, the active row reports a TTFT and a rate, and the output-token count equals what the upstream reported.
- `openai_responses` **without** client `timings_per_token`: after a `response.output_text.delta`, the row reports a TTFT, and **no** token count and **no** rate.
- `openai_responses` **with** the client's `timings_per_token` (so partial frames carry `timings`): the row reports the upstream's rate, and `liveProgressDTO`'s `source` names it as upstream-reported rather than derived.
- Buffered passthrough: `Progress` stays nil, nothing panics, and the row reports nothing.

- [ ] **Step 2: Run them and watch them fail** — `Progress` is nil today, so the first three fail on a zero TTFT.

- [ ] **Step 3: Allocate the progress**

In `proxyNative`, allocate a `requestProgress` and attach it to the `ActiveRequest` **only for a streaming request** (`pfReq.Stream`), mirroring `stream_session.go`'s shape. A buffered passthrough keeps a nil `Progress` deliberately — say why in a comment.

- [ ] **Step 4: Feed it from the scanner**

`scan(payload, at)` already stamps `firstContentAt` and merges usage per frame. Give the scanner a way to report those to the progress — either by holding the pointer or by a callback set at construction; prefer whichever keeps `usageScanner` testable in isolation, and say which you chose and why. Feed: the first-content timestamp once, the upstream's cumulative output tokens when the frame carried one, and the upstream's rate when a partial frame carried `timings`.

**Do not** compute a token count where the upstream reported none, and do not derive a rate in the scanner — `liveProgressDTO` already derives one over the window when no upstream rate is present, and duplicating that logic would let the two drift.

- [ ] **Step 5: The non-injection comment**

At `rewriteModelField`'s call site, state that the relayed body is never augmented — specifically that `timings_per_token` is read when the client set it and never added — so the next reader does not "fix" the Responses gap by injecting it.

- [ ] **Step 6: Prove the absence of a routing write**

A test asserting that a streaming passthrough request performs **zero** `UpdateMappingOpportunisticMetrics` calls from the in-flight path (the end-of-request one is unchanged and still fires where it did before). Assert the call count.

- [ ] **Step 7: Mutation evidence, then gates and commit**

Mutations: remove the allocation (all three flavor tests fail); feed a derived rate from the scanner as well (the source-naming test fails); count deltas as tokens for Responses (that flavor's "no token count" test fails). Record which broke which. Then `fmt --diff` + `run` + `go test ./... -count=1`, plus the new tests under `-race`.

---

### Task 2: The display

**Files:**
- Modify: the running-connections panel component and its i18n files
- Test: the panel's component tests

- [ ] **Step 1:** Report first what the panel does **today** with a row whose progress carries a TTFT but no token count — before changing anything. If it already renders an em-dash for a zero, the rule may already hold and this task is a test plus a doc line.
- [ ] **Step 2:** Make an absent cell render an em-dash and a measured zero render `0`, following `formatLiveTps`' existing precedent rather than inventing a second convention.
- [ ] **Step 3:** Surface the `source` the DTO already returns, wherever the panel explains a figure, so a client-dependent difference in completeness is legible.
- [ ] **Step 4:** i18n keys in every language — a missing key is a **compile** failure here (`PortalMessages = typeof de`), so `npm run build` is the check.
- [ ] **Step 5:** Frontend gates and commit.

---

### Task 3: The documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/` — the endpoint-modes description and wherever the live panel is documented; `docs/architecture/reference/api-surface.md` if the DTO's fields are listed there.

- [ ] **Step 1:** Record the trade-off that motivated this, now partially closed: choosing `passthrough` used to cost the panel's live figures entirely; it now costs only what the shape cannot honestly provide.
- [ ] **Step 2:** Record the per-flavor table from the spec, and the reason for the asymmetry (llama.cpp attaches no `timings` to Anthropic frames at all; the Responses partials carry no usage).
- [ ] **Step 3:** Record the two rules that must survive: no `timings_per_token` injection, and no routing input from the in-flight figure.
- [ ] **Step 4:** Re-locate every passage by heading and wording, never by line number. Docs gates, then commit.

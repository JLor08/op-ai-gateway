# Responses Live Timings, Part 2 — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When the operator's switch is on for a llama.cpp upstream, the gateway
injects `"timings_per_token": true` into a streaming native passthrough
`/v1/responses` request, so the running-connections panel shows a live,
upstream-reported tokens/sec for the whole request — and the portal gains the
control to switch it.

**Architecture:** A pure gate predicate over `routing.Target` in package
`gateway`, a pure injection helper beside the existing model rewrite, and the
existing live-progress reader — which already lifts a rate off any Responses
frame — fed for the first time by a gateway-initiated key. No retry, no capture
change, no new wire field. The design document is
`docs/superpowers/specs/2026-09-12-responses-live-timings-injection-design.md`;
it records four first-hand measurements against the operator's deployment and
the ten decisions that follow from them.

**Tech Stack:** Go 1.x backend (`gateway/backend`), React + TypeScript portal
(`gateway/frontend`), SQLite and PostgreSQL behind one store interface.

## Global Constraints

- **Never commit to or merge into `main`.** All work happens on this branch and
  reaches `main` only through a pull request.
- **Repo-facing text is English** — code, comments, commit messages, docs. Only
  the operator chat is German.
- **Every new test must fail when its production change is reverted, and the
  revert must touch production files only.** Reverting a test file makes a
  filtered run print `ok` having run nothing, which is a fake pass.
- **No mutation used as proof may be compound.** One change at a time; a
  compound mutation proves only the compound.
- **No `file:line` citation inside a comment destined for the repository.** The
  edit that adds it is what falsifies it. Name the function instead. Line
  numbers addressed to *you*, saying where to edit, are fine.
- **A list presented as exhaustive must be exhaustive.** Count it yourself.
- **Go gates, per module, from `gateway/backend`:** `~/go/bin/golangci-lint fmt
  --diff` must print **nothing**; `~/go/bin/golangci-lint run` must be clean;
  `go test ./... -count=1` must pass. **Full packages — never a `-run` filter as
  a verdict.** A filter in a predecessor brief printed `ok` having skipped four
  of twelve tests. `internal/gateway` costs about 95 seconds; budget it.
- **If any file under `internal/store` changes, the PostgreSQL leg is
  mandatory** — those subtests skip **silently** without
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:55432/op_test?sslmode=disable'`.
  Reuse the running `op-test-pg` container, record the counts, and name every
  skip. No task in this plan is expected to touch `internal/store`.
- **Frontend gates, from `gateway/frontend`:** `npm run format:check`, `lint`,
  `build`, `test`. CI runs prettier's format check, which the other three do not
  cover. A fresh worktree needs `npm ci` first.
- **Docs gates, from the worktree root:** `./scripts/check-docs.sh` and `bash
  ./scripts/check-docs.test.sh`. They verify links, anchors and reachability;
  they cannot see a prose contradiction, and they cannot see a test comment.
- `docs/superpowers/` is branch-local and is removed before the pull request.

## File Structure

| Area | Files | Owned by |
|---|---|---|
| The capable-kind predicate and its tripwires | `internal/routing/live_timings*.go`, `internal/provider/live_progress_kind_parity_test.go` | Task 1 |
| The gate | `internal/gateway` (new predicate + table test) | Task 2 |
| The injection helper | `internal/gateway` (new pure helper + tests) | Task 3 |
| The passthrough wiring, the log field, the fixtures | `internal/gateway/native_passthrough.go`, `passthrough_progress_test.go`, `server_test.go` | Task 4 |
| The live count | `internal/gateway/passthrough_usage_scan.go`, `native_passthrough.go`'s timings struct | Task 5 |
| The panel column | `gateway/frontend/src/components/ActiveRequestsPanel*`, `i18n*` | Task 6 |
| The portal control | `gateway/frontend/src/components/ApplicationSection.tsx`, `RuntimeAdminSection.tsx`, `shared/`, `api/` | Task 7 |
| The standing claims and the new prose | `docs/architecture/**`, plus the test comments the docs check cannot see | Task 8 |

Ownership between tasks is stated in both directions wherever two tasks touch
one file, and was verified site-by-site: no site is owned twice, and none is
owned by nobody. `docs/architecture/cross-cutting/agent-runtime-manager.md`
§11.5 is the one deliberate two-writer site; Task 8's replacement supersedes
Task 1's clause and carries it forward verbatim, and is idempotent in either
order.

## Task weight, and where the risk sits

Eight tasks, 106 steps. They are not equal, and the two heaviest are the two
whose failure would be least visible:

| Task | Steps | Why it weighs what it does |
|---|---|---|
| 1 | 16 | Narrowing a shipped predicate; one of its companions is a live portal assertion, not prose |
| 2 | 11 | A pure predicate — the table is the whole defence, since a forgotten condition leaves every test green |
| 3 | 8 | A pure helper; the smallest task here |
| 4 | 14 | Where the feature becomes real, and where the positive path can ship untested |
| 5 | 15 | One merge function writes to two destinations, and only one of them may receive this value |
| 6 | 16 | Mostly assertion bookkeeping around a default-hidden column |
| 7 | 10 | **Dense.** Two forms, three traps, each of which otherwise blocks a save |
| 8 | 16 | **Dense.** Four correction sets across code, docs and test comments the docs check cannot see |

Tasks 7 and 8 carry the most surface per review gate. Their steps are long
because they quote exact replacement text, not because they bundle unrelated
actions — but they are the two to dispatch with the most care, and the two where
a fix round is most likely.

---

### Task 1: vLLM leaves the routing capable-kind set

Every path below is relative to the worktree root
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection`.
The Go module root is `gateway/backend` (module `op-ai-gateway`), so
`internal/routing` is `op-ai-gateway/internal/routing`.

**Files:**

- Modify: `gateway/backend/internal/routing/live_timings.go` — the `var
  liveTimingsCapableKinds` map literal **and its own doc comment**. Do **not**
  touch the doc comment on `func LiveTimingsCapableKind`: Task 2 owns it
  (confirmed — `plan-tasks-2.md`'s Files list has "Modify:
  `gateway/backend/internal/routing/live_timings.go` — the doc comment on
  `LiveTimingsCapableKind` (comment text only)", and its PROBLEM section lists
  that comment as member 3 of the "nothing reads the resolved flag" family).
- Test: `gateway/backend/internal/routing/live_timings_test.go` — all three
  tests: `TestLiveTimingsCapableKind`,
  `TestLiveTimingsCapableKindsSizeIsPinned`,
  `TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies`.
- Test: `gateway/backend/internal/provider/live_progress_kind_parity_test.go` —
  the whole file: `TestLiveProgressUpstreamsMatchesRoutingCapableKinds` is
  **renamed** to `TestLiveProgressUpstreamsCoverEveryRoutingCapableKind`, its
  doc comment rewritten, its first loop repurposed and its second loop relaxed.
- Test: `gateway/backend/internal/portal/service_applications_test.go` — the
  `routing.ProviderVLLM` row of `TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind`'s
  `cases` table, and two sentences of that test's doc comment.
- Test: `gateway/backend/internal/portal/service_runtime_test.go` — one sentence
  of the doc comment above
  `TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind`.
- Modify: `gateway/backend/internal/portal/service_applications.go` — comment
  text only, two sites: the doc comment on
  `CreateApplicationRequest.ResponsesLiveTimingsEnabled`, and the "nil with an
  incapable resulting type" bullet of the comment block above
  `updateApplication`'s live-timings `switch`.
- Modify: `gateway/backend/internal/portal/service_runtime.go` — comment text
  only, one site: the doc comment on
  `PutRuntimeSpecRequest.ResponsesLiveTimingsEnabled`.
- Modify: `docs/architecture/reference/api-surface.md` — the "absent is not the
  same as `false`" bullet of the `responses_live_timings_enabled` section.
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md` — the
  `responses_live_timings_enabled` paragraph of §11.5, the clause naming the
  refused effective type.

**Explicitly NOT modified** (P2 above — both stay true, both describe the
gate, which keeps vLLM): `gateway/backend/internal/provider/live_progress.go`
and `docs/architecture/cross-cutting/telemetry-usage-observability.md`.

**Interfaces:**

- Consumes: nothing. This is the first task of the plan.
- Produces:
  - `func routing.LiveTimingsCapableKind(kind string) bool` — signature
    unchanged, **behaviour changed**: it returns `true` for exactly one string,
    `routing.ProviderLlamaCPP` (`"llama_cpp"`, which is also
    `string(routing.RuntimeSpecTypeLlamaCpp)`), and `false` for every other
    input including `routing.ProviderVLLM` / `string(routing.RuntimeSpecTypeVLLM)`.
    Task 2's gate predicate is its first request-path caller.
  - `var liveTimingsCapableKinds map[string]struct{}` (unexported, package
    `routing`) — `len() == 1`.
  - `routing.ProviderVLLM` and `routing.RuntimeSpecTypeVLLM` keep sharing the
    string `"vllm"`; `internal/provider`'s `wantsLiveProgress` still depends on
    that, and `TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies`
    still pins it.
  - Renamed test symbol:
    `provider.TestLiveProgressUpstreamsCoverEveryRoutingCapableKind`.
- No new exported API. No signature anywhere changes.

**The assertions that change — counted, and this list is exhaustive.** Ten
assertions that change, plus one stated non-change (row 10), plus ten comment
blocks; nothing else in either Go module reads
`routing.LiveTimingsCapableKind` in a way whose verdict moves (verified by
`grep -rln "LiveTimingsCapableKind" --include=*.go gateway/backend server-agent`,
which names exactly **ten** files: the seven already in this task's Files list,
plus three that are unaffected —
`internal/store/application_column_parity_test.go` and
`internal/store/routing_store_conformance_test.go`, whose two guards both sit on
`ollama`, and `internal/gateway/portal_runtime_endpoints_test.go`, whose two
mentions are doc-comment prose about `LiveTimingsCapableKind("")` being false
and whose two live-timings tests are written against `ollama` and an
auto-detected `llama-server`; the `vllm` request bodies elsewhere in that file
never mention the flag).

| # | Where | Assertion |
|---|---|---|
| 1 | `routing/live_timings_test.go`, `TestLiveTimingsCapableKind` | table row `{ProviderVLLM, true}` → `false` |
| 2 | same | table row `{string(RuntimeSpecTypeVLLM), true}` → `false` |
| 3 | same | `if capableRows != len(liveTimingsCapableKinds)` — the arithmetic moves from 2 vs 2 to 1 vs 1, and its `t.Errorf` message is amended |
| 4 | `routing/live_timings_test.go`, `TestLiveTimingsCapableKindsSizeIsPinned` | `const pinnedSize = 2` → `1` |
| 5 | same | the `t.Fatalf` message, which currently instructs the reader to narrow the **wrong** list |
| 6 | `routing/live_timings_test.go`, `TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies` | the anonymous struct gains a `capable bool`; `if !LiveTimingsCapableKind(pair.provider)` → `if got := ...; got != pair.capable` |
| 7 | same | the `{ProviderVLLM, RuntimeSpecTypeVLLM}` row gains `capable: false`, the `llama_cpp` row `capable: true` (the row is **kept**, because `internal/provider` still needs the string equality) |
| 8 | `provider/live_progress_kind_parity_test.go` | loop 1 (`for kind := range liveProgressUpstreams`) becomes a one-recorded-divergence check instead of `!routing.LiveTimingsCapableKind(kind)` |
| 9 | same | the enumeration loop's `if inGate != want` → `if routing.LiveTimingsCapableKind(kind) && !inGate` |
| 10 | same | the staleness loop (loop 3) is **unchanged** — stated so nobody "tidies" it; it is now the only exact check over the real gate map besides #8 |
| 11 | `portal/service_applications_test.go`, `TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind` | table row `{routing.ProviderVLLM, 8201, true}` → `false` |

Comment blocks rewritten (ten, as 1 + 3 + 1 + 2 + 3): the
`liveTimingsCapableKinds` var doc; the three doc comments in
`routing/live_timings_test.go`; the parity test's doc comment; the two portal
test doc comments; and the three portal production doc comments (two blocks in
`service_applications.go` plus one in `service_runtime.go`).

---

- [ ] **Step 1: Flip `TestLiveTimingsCapableKind`'s two vllm rows and rewrite its doc**

In `gateway/backend/internal/routing/live_timings_test.go`, replace the doc
comment above `TestLiveTimingsCapableKind` and its two `vllm` rows.

Replace the sentence

```go
// five RuntimeSpecTypes -- because the predicate's whole job is to answer for a
// string drawn from EITHER of them (an Application.Type, or the string form of
// an EffectiveRuntimeSpecType), and only two values may answer true.
```

with

```go
// five RuntimeSpecTypes -- because the predicate's whole job is to answer for a
// string drawn from EITHER of them (an Application.Type, or the string form of
// an EffectiveRuntimeSpecType), and only ONE value may answer true.
```

Replace the last sentence of the `ProviderLlamaSwap`/`ProviderLiteLLM` bullet

```go
//     internal/provider/live_progress.go records the same reasoning for the
//     gate's own set, which is why the two sets agree here.
```

with

```go
//     internal/provider/live_progress.go records the same reasoning for the
//     gate's own set, and on these two kinds the two sets still agree -- but
//     they are no longer the same set. See the vllm bullet below.
//   - ProviderVLLM and RuntimeSpecTypeVLLM are FALSE, and they are the ONE
//     place this set and internal/provider's liveProgressUpstreams disagree.
//     vLLM's /v1/responses accepts timings_per_token and does nothing with it:
//     measured 2026-09-12 against a live vLLM upstream through the gateway,
//     a streamed request carrying the flag produced 48 data frames and NOT ONE
//     carrying a timings object -- not on the partials, not on the terminal
//     frame. (One deployment, one build, whose build identifier was not
//     recorded; the same scope caveat the repository already applies to #80's
//     measurement.) The opt-in this set decides defaults ON for a newly
//     created application, so keeping vllm here would default a switch on that
//     provably delivers nothing. The GATE keeps vllm, because the pair it
//     sends on /v1/chat/completions -- timings_per_token AND
//     stream_options.continuous_usage_stats -- includes a first-class vLLM
//     field that works there.
```

Then flip the two rows in `cases`:

```go
		{ProviderLlamaCPP, true},
		{ProviderVLLM, false},
		{string(RuntimeSpecTypeLlamaCpp), true},
		{string(RuntimeSpecTypeVLLM), false},
```

Both rows must flip in the same edit: `ProviderVLLM` and `RuntimeSpecTypeVLLM`
are the **same string**, and the `wantByKind` dedup check below them
`t.Fatalf`s with "the table answers true and false for the same kind
\"vllm\"" if only one is changed.

Finally amend the count check's message (the arithmetic itself is unchanged
code):

```go
	if capableRows != len(liveTimingsCapableKinds) {
		t.Errorf("the table expects %d capable kinds but liveTimingsCapableKinds has %d: fix the table to match the set. Do NOT reflexively re-level internal/provider's liveProgressUpstreams -- since 2026-09-12 the two are no longer one set, and the only rule left is that every kind HERE is also in the gate",
			capableRows, len(liveTimingsCapableKinds))
	}
```

---

- [ ] **Step 2: Re-pin the size and fix the failure message that points at the wrong list**

Still in `gateway/backend/internal/routing/live_timings_test.go`.

First the doc's OPENING sentence, which today promises the opposite of what the
new failure message says — "the set has to stay level with" the other list is
exactly the instruction this step retires. Replace

```go
// the size cannot happen without an edit right here -- at a site whose failure
// message names the OTHER hand-written list the set has to stay level with.
```

with

```go
// the size cannot happen without an edit right here -- at a site whose failure
// message names the OTHER hand-written list and says how far the two still have
// to agree: every kind HERE is in the gate, and the gate deliberately holds one
// kind this set does not.
```

Then replace this paragraph of `TestLiveTimingsCapableKindsSizeIsPinned`'s doc
comment

```go
// Why a size pin is needed on top of everything else. The two-test pair around
// internal/provider's TestLiveProgressUpstreamsMatchesRoutingCapableKinds is
// exact in the gate => capable direction only, because that direction ranges
// over the real liveProgressUpstreams map. The capable => gate direction runs
```

with

```go
// Why a size pin is needed on top of everything else. internal/provider's
// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind no longer checks
// equality in either direction: the contract since 2026-09-12 is that the
// capable set is a SUBSET of the gate, with exactly one recorded divergence
// (vllm, which the gate keeps and this set dropped). The only direction it can
// check exactly is "every member of the GATE is either capable or that one
// divergence", because that direction ranges over the real
// liveProgressUpstreams map. The capable => gate direction runs
```

Then the pin and its message:

```go
	// Changing this number is the deliberate act. Read the failure message
	// before you do.
	const pinnedSize = 1
	if len(liveTimingsCapableKinds) != pinnedSize {
		t.Fatalf("liveTimingsCapableKinds has %d kinds, pinned at %d: a kind was added or removed here. Do NOT go and make internal/provider's liveProgressUpstreams match -- that is the wrong half to follow: the two sets stopped being one set on 2026-09-12 and the gate deliberately holds vllm, which this set does not. The rule is only that every kind HERE is also in the gate; read the divergence note on liveTimingsCapableKinds first. And note that the provider-side parity test cannot see this set and enumerates kind strings by hand, so it stays SILENT about a kind neither list mentions yet",
			len(liveTimingsCapableKinds), pinnedSize)
	}
```

---

- [ ] **Step 3: Keep the vllm string-equality pin, split it from the capability pin**

Still in `gateway/backend/internal/routing/live_timings_test.go`. The vllm row
of `TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies` must NOT
be deleted: `internal/provider`'s `wantsLiveProgress` swaps `target.Provider`
for `target.LiveProgressSpecType` on a `server_agent` target and looks the
result up in `liveProgressUpstreams`, which still holds `"vllm"` — so the
equality `ProviderVLLM == string(RuntimeSpecTypeVLLM)` is still load-bearing,
just for the other package now. Split the two assertions instead.

The doc's FIRST paragraph rests the single-map design on "the two kinds that may
answer true", which this task makes false — after it exactly one kind answers
true, while the map is still looked up for both shared strings. Replace

```go
// LiveTimingsCapableKind serves both from ONE map only because the two kinds
// that may answer true spell themselves identically. internal/provider's
```

with

```go
// LiveTimingsCapableKind serves both from ONE map only because the kinds the
// two vocabularies have IN COMMON spell themselves identically -- llama_cpp,
// the one kind that may answer true, and vllm, which answers false here and is
// still looked up through that same single map. internal/provider's
```

Then replace the second paragraph of the doc comment

```go
// If a later rename broke either pair, the predicate would keep compiling and
// start answering false for one whole vocabulary -- a silent default-off for
// every runtime spec, with no type error anywhere. The pairs are compared
// through a slice so the check is a real runtime comparison and not a constant
// the compiler folds away.
```

with

```go
// If a later rename broke either pair, the predicate would keep compiling and
// start answering false for one whole vocabulary -- a silent default-off for
// every runtime spec, with no type error anywhere. The pairs are compared
// through a slice so the check is a real runtime comparison and not a constant
// the compiler folds away.
//
// The vllm pair is still here even though vllm is no longer capable, and the
// two halves of each row are now checked SEPARATELY, because the string
// equality outlived the membership: internal/provider's wantsLiveProgress
// swaps target.Provider for target.LiveProgressSpecType on a server_agent
// target and looks the result up in liveProgressUpstreams, which still lists
// vllm. Delete this row and that swap loses its only guard. The capable column
// is what says which vocabulary-crossing kind this set still opts in.
```

and replace the loop body:

```go
func TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies(t *testing.T) {
	for _, pair := range []struct {
		provider string
		specType RuntimeSpecType
		capable  bool
	}{
		{ProviderLlamaCPP, RuntimeSpecTypeLlamaCpp, true},
		{ProviderVLLM, RuntimeSpecTypeVLLM, false},
	} {
		if pair.provider != string(pair.specType) {
			t.Errorf("provider %q and runtime spec type %q no longer share one string: LiveTimingsCapableKind's single map cannot serve both vocabularies any more, and internal/provider's wantsLiveProgress loses the same equality",
				pair.provider, pair.specType)
			continue
		}
		if got := LiveTimingsCapableKind(pair.provider); got != pair.capable {
			t.Errorf("LiveTimingsCapableKind(%q) = %v, want %v: one string, one verdict -- both vocabularies have to get the same answer out of the single map",
				pair.provider, got, pair.capable)
		}
	}
}
```

---

- [ ] **Step 4: Run the routing package and watch it fail**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/routing/...
```

Expected: `FAIL op-ai-gateway/internal/routing`, with exactly these three tests
failing and no others. The production map still has two members, so:

- `--- FAIL: TestLiveTimingsCapableKind` — three messages:
  - `LiveTimingsCapableKind("vllm") = true, want false` (printed **twice**: the
    `ProviderVLLM` row and the `RuntimeSpecTypeVLLM` row are the same string),
  - `the table expects 1 capable kinds but liveTimingsCapableKinds has 2: fix the table to match the set. …`
- `--- FAIL: TestLiveTimingsCapableKindsSizeIsPinned` —
  `liveTimingsCapableKinds has 2 kinds, pinned at 1: a kind was added or removed here. Do NOT go and make internal/provider's liveProgressUpstreams match …`
- `--- FAIL: TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies` —
  `LiveTimingsCapableKind("vllm") = true, want false: one string, one verdict …`

If `TestLiveTimingsCapableKind` instead fails with *"the table answers false and
true for the same kind \"vllm\""* — the two booleans print in TABLE order, first
occurrence then second, so it reads *"true and false"* instead when it is the
later `RuntimeSpecTypeVLLM` row that was flipped — only one of the two vllm rows
was flipped in Step 1. Fix that row; do not touch production yet.

---

- [ ] **Step 5: Narrow the set (the one production change) and rewrite its doc comment**

In `gateway/backend/internal/routing/live_timings.go`, replace the entire doc
comment above `var liveTimingsCapableKinds` and the map literal itself. Leave
`func LiveTimingsCapableKind` and its doc comment untouched — Task 2 owns that
comment.

```go
// liveTimingsCapableKinds is the closed set of upstream kinds whose
// /v1/responses implementation is known to ANSWER a live-progress request
// parameter with per-token timings, and therefore the set for which a NEWLY
// CREATED application or runtime spec gets the responses live-timings opt-in
// switched on by default.
//
// ONE member. It is keyed on the string value BOTH vocabularies share --
// ProviderLlamaCPP == "llama_cpp" == RuntimeSpecTypeLlamaCpp -- so one lookup
// serves an ordinary application's Type and a server_agent child's
// EffectiveRuntimeSpecType alike. ProviderVLLM == "vllm" == RuntimeSpecTypeVLLM
// holds in exactly the same way and is still pinned by
// TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies, because
// internal/provider's gate still needs that equality -- it just no longer buys
// a membership here.
//
// THIS SET IS NO LONGER internal/provider's liveProgressUpstreams. The two
// were one set for one reason: both were read as "does this kind TOLERATE the
// parameter?". Measured on 2026-09-12 against a live vLLM upstream serving
// /v1/responses through this gateway, a streamed request carrying
// timings_per_token produced 48 data frames and NOT ONE of them carried a
// top-level timings object -- neither the partials nor the terminal frame.
// vLLM accepts the key (its request models allow unknown fields) and does
// nothing with it. Tolerance was never the question THIS set answers;
// DELIVERY is, because a switch that is offered, defaults on for new
// applications, and provably delivers nothing is worse than one that is not
// offered. (One deployment, one build, whose build identifier was not
// recorded -- the same scope caveat the repository already applies to #80's
// measurement. Record the build before revisiting this.)
//
// liveProgressUpstreams keeps vllm on purpose: it gates a different parameter
// PAIR (timings_per_token AND stream_options.continuous_usage_stats) on a
// different endpoint (/v1/chat/completions), and continuous_usage_stats is a
// first-class vLLM field that works there. So the relationship is now "every
// kind here is also in the gate", with exactly one recorded divergence in the
// other direction, pinned by internal/provider's
// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind. Do not re-level the
// two lists into one.
//
// Deliberately NOT listed: vllm (above), server_agent (not an inference server
// at all -- ask EffectiveRuntimeSpecType what actually serves), llama_swap and
// litellm (both resolve a model to an arbitrary downstream that can be
// api.openai.com, which answers 400 on an unrecognized body key), ollama, tgi,
// custom, mock. The default is OFF, so a kind added later never opts in
// silently.
var liveTimingsCapableKinds = map[string]struct{}{
	ProviderLlamaCPP: {},
}
```

---

- [ ] **Step 6: Run the routing package green**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/routing/...
```

Expected: `ok  	op-ai-gateway/internal/routing` (the whole package, no `-run`
filter). If `TestTargetResponsesLiveTimingsPrecedence` in
`resolver_live_timings_test.go` fails here, something other than the map was
edited — that test never calls `LiveTimingsCapableKind`.

---

- [ ] **Step 7: Run the two consumer packages and watch the companion failures**

The production change is in. Two packages are now red, and watching them fail
is the evidence that Steps 8 and 10 are required rather than optional.

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/provider/... ./internal/portal/...
```

Expected: both packages FAIL, with exactly two failing tests in total.

- `--- FAIL: TestLiveProgressUpstreamsMatchesRoutingCapableKinds`
  (`op-ai-gateway/internal/provider`), three messages:
  - `liveProgressUpstreams has "vllm" but routing.LiveTimingsCapableKind("vllm") is false -- the gate would send the parameters to a kind no create path ever opts in`
    — once, from the loop over the real gate map.
  - `"vllm": liveProgressUpstreams=true routing.LiveTimingsCapableKind=false -- the two kind lists have drifted`
    — **twice**. The enumeration loop's `kinds` slice lists both
    `routing.ProviderVLLM` and `string(routing.RuntimeSpecTypeVLLM)`, which are
    the same string `"vllm"`, so the per-kind loop reaches it in two iterations.
- `--- FAIL: TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind/vllm`
  (`op-ai-gateway/internal/portal`):
  - `vllm: ResponsesLiveTimingsEnabled = false, want true`

Nothing else fails. In particular `TestWantsLiveProgressAllowList`,
`TestWantsLiveProgressThreeLayerRule` and
`TestWantsLiveProgressReadsTheJoinedVerdict` stay green — they pin the GATE,
which did not change — and no `internal/portal` runtime-spec test fails,
because every live-timings spec case in that file is written against
`llama_cpp` or `ollama`.

---

- [ ] **Step 8: Relax and rename the cross-package parity test**

In `gateway/backend/internal/provider/live_progress_kind_parity_test.go`,
replace everything from the doc comment down to the end of the function.
Loop 3 (the staleness loop) is reproduced unchanged on purpose — do not edit
it.

```go
// TestLiveProgressUpstreamsCoverEveryRoutingCapableKind holds the two
// hand-written kind lists in the one relation that survives: liveProgressUpstreams
// (live_progress.go, the GATE's shape clause -- which upstreams may be SENT the
// two streaming parameters on /v1/chat/completions) must contain every kind
// routing.LiveTimingsCapableKind opts a newly created application or spec in
// for. Not the other way round, and not equality.
//
// They WERE one set, and until 2026-09-12 this test asserted exactly that.
// What broke the equality is a measurement, not a refactor: a streamed vLLM
// /v1/responses request carrying timings_per_token produced 48 data frames and
// not one timings object, so the key is inert there rather than merely
// tolerated. routing's set answers "which kind should the opt-in DEFAULT ON
// for", so vllm left it; this map answers "which kind may be SENT the
// parameters", and vllm stays, because the pair it sends includes
// stream_options.continuous_usage_stats, a first-class vLLM field that works.
// That is the ONE divergence, it is named in the first loop below, and a
// second one has to be argued for there.
//
// The relation that remains still matters: a capable kind missing from the
// gate would mean the portal defaults an opt-in ON for a kind the gate refuses
// to send anything to.
//
// It lives in package provider because liveProgressUpstreams is unexported.
//
// The two directions are NOT symmetric, because only one of the two sets is
// visible from here:
//   - over the GATE: the first loop ranges over the real map, so it catches any
//     kind added to the gate alone -- whichever kind that is, except the ONE
//     recorded divergence the loop names. Measured:
//     adding "ollama" to liveProgressUpstreams together with its four
//     expectations elsewhere in this package (TestWantsLiveProgressAllowList,
//     TestWantsLiveProgressThreeLayerRule twice, and
//     TestWantsLiveProgressReadsTheJoinedVerdict) leaves the entire backend
//     suite green except this loop. Nothing else in the tree can see that edit.
//   - over the CAPABLE set: the second loop has to go through the enumeration
//     below, since routing's set is unexported and another package cannot range
//     over it. The other half of that direction lives in routing's own
//     TestLiveTimingsCapableKind, which ranges over the set and fails for any
//     member it has no row for. The staleness check at the end is what keeps
//     this enumeration from quietly falling behind the vocabularies.
//
// Two things this test deliberately does NOT guard, named so they are not
// assumed:
//   - It does not pin that vllm is still in the gate. Removing
//     routing.ProviderVLLM from liveProgressUpstreams leaves every loop here
//     silent; what fails is three expectations in two OTHER tests of this
//     package -- TestWantsLiveProgressAllowList (wantsLiveProgress("vllm") =
//     false, want true) and TestWantsLiveProgressThreeLayerRule twice, on its
//     "undetermined + vllm shape implies tolerant" row and on its
//     "undetermined + server_agent whose effective spec type is vllm" row,
//     which reaches the same map entry through target.LiveProgressSpecType.
//   - It does not fire for a kind in NEITHER vocabulary. Add a future
//     routing.ProviderSGLang = "sglang" to routing's capable set with its row in
//     TestLiveTimingsCapableKind and nothing here notices -- the enumeration
//     never mentions it, and the first loop iterates the GATE's keys, where it
//     does not appear either. Closing that from this side needs an exported
//     accessor for routing's set, i.e. production API whose only caller is a
//     test. The mitigation lives next to the set instead: routing's
//     TestLiveTimingsCapableKindsSizeIsPinned fails on any change to its SIZE.
func TestLiveProgressUpstreamsCoverEveryRoutingCapableKind(t *testing.T) {
	// Over the real gate map: every kind the gate may send the parameters to
	// is either live-timings-capable or THE one recorded divergence.
	for kind := range liveProgressUpstreams {
		if kind == routing.ProviderVLLM || routing.LiveTimingsCapableKind(kind) {
			continue
		}
		t.Errorf("liveProgressUpstreams has %q, which routing.LiveTimingsCapableKind calls incapable and which is not the one recorded divergence (%q): either add it to routing's capable set, or record here why the gate may send the parameters to a kind no create path ever opts in",
			kind, routing.ProviderVLLM)
	}

	// Every value in BOTH kind vocabularies, including the three that spell
	// themselves the same in each ("vllm", "llama_cpp", "ollama"), plus the
	// empty spec type that means "auto-detect from the binary" and is a
	// legitimate stored value. Listed rather than derived: neither vocabulary
	// exposes an enumeration (portal.normalizeApplicationType and
	// portal.validRuntimeSpecType are unexported switches in a third package).
	kinds := []string{
		routing.ProviderMock, routing.ProviderOllama, routing.ProviderVLLM, routing.ProviderLlamaCPP,
		routing.ProviderLlamaSwap, routing.ProviderLiteLLM, routing.ProviderServerAgent,
		string(routing.RuntimeSpecTypeVLLM), string(routing.RuntimeSpecTypeLlamaCpp),
		string(routing.RuntimeSpecTypeTGI), string(routing.RuntimeSpecTypeOllama),
		string(routing.RuntimeSpecTypeCustom), "",
	}
	listed := make(map[string]struct{}, len(kinds))
	for _, kind := range kinds {
		listed[kind] = struct{}{}
		_, inGate := liveProgressUpstreams[kind]
		if routing.LiveTimingsCapableKind(kind) && !inGate {
			t.Errorf("%q: routing.LiveTimingsCapableKind is true but liveProgressUpstreams does not list it -- the portal would default the opt-in ON for a kind the gate refuses to send the parameters to", kind)
		}
	}

	// Not a drift claim, and deliberately phrased so it cannot be mistaken for
	// one: the two sets can stand in the right relation and still hold a kind
	// this enumeration has never heard of, and then the loop above is checking
	// thirteen strings that no longer describe the vocabularies. Checked on the
	// gate's keys only -- a size comparison would also fire whenever the
	// CAPABLE side changed, duplicating the errors above with a message that
	// points at the wrong file.
	for kind := range liveProgressUpstreams {
		if _, ok := listed[kind]; !ok {
			t.Errorf("liveProgressUpstreams has %q, which this test's kind list does not enumerate: add it there too, or the per-kind loop stops covering the whole vocabulary", kind)
		}
	}
}
```

**Why this test cannot be the pin for this task's production change, and what
is instead.** With the relaxation in place, restoring `ProviderVLLM: {}` to
`liveTimingsCapableKinds` leaves every loop here silent (vllm is skipped by the
explicit exception in loop 1, and loop 2 only fires for capable-but-not-gated).
That is correct and deliberate: this test now records a *relation*, not an
equality. The pin for this task's production change lives in
`routing/live_timings_test.go` (rows 1, 2, 3, 4, 6 of the table above) and in
`portal/service_applications_test.go` (row 11). Do not add a "vllm must be
incapable" assertion here to compensate — it would duplicate routing's own
table across a package boundary, which is exactly the hand-written drift this
file exists to prevent.

---

- [ ] **Step 9: Run the provider package green**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/provider/...
```

Expected: `ok  	op-ai-gateway/internal/provider`. Full package — this one also
runs the `httptest.NewServer`-backed adapter tests, so a sandbox may need
loopback-listener permission.

---

- [ ] **Step 10: Fix the portal's vllm expectation and the six portal sentences**

Six lettered edits, all in `gateway/backend/internal/portal`: one table row plus
six sentences across five comment blocks (edit (b) replaces two sentences of the
same doc comment).

**(a)** `service_applications_test.go`, `TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind`'s
`cases` table — flip the row and say why inline:

```go
		{routing.ProviderLlamaCPP, 8200, true},
		// vllm was true here until 2026-09-12, when a streamed vLLM
		// /v1/responses request carrying timings_per_token was measured to
		// produce 48 frames and zero timings objects: the key is accepted and
		// inert, so defaulting the opt-in on promised a number that never
		// arrives. internal/provider's GATE still lists vllm, for the pair it
		// sends on /v1/chat/completions.
		{routing.ProviderVLLM, 8201, false},
```

**(b)** same test, doc comment — replace

```go
// The rows deliberately DISAGREE: two want true, four want false. A table
```

with

```go
// The rows deliberately DISAGREE: one wants true, five want false. A table
```

and replace the opening sentence's "whose request schema tolerates the
live-progress parameter" with "whose /v1/responses implementation actually
ANSWERS the live-progress parameter" — tolerance is now precisely the wrong
word, since vLLM tolerates the key and delivers nothing.

**(c)** `service_runtime_test.go`, doc comment above
`TestPutRuntimeSpecResponsesLiveTimingsDefaultsFromTheSpecsOwnKind` — replace

```go
// implies -- on for llama_cpp/vllm, off for every other kind.
```

with

```go
// implies -- on for llama_cpp, off for every other kind (vllm included since
// 2026-09-12; see routing.liveTimingsCapableKinds).
```

**(d)** `service_applications.go`, doc comment on
`CreateApplicationRequest.ResponsesLiveTimingsEnabled` — replace

```go
	// false, because absent is what gets the kind-dependent default (ON for
	// llama_cpp and vllm) and false is a deliberate off; with a plain bool the
```

with

```go
	// false, because absent is what gets the kind-dependent default (ON for
	// llama_cpp) and false is a deliberate off; with a plain bool the
```

**(e)** `service_applications.go`, the "nil with an incapable resulting type"
bullet above `updateApplication`'s live-timings `switch` — replace

```go
	//     so a retype away from llama_cpp/vllm cannot leave a stale true
```

with

```go
	//     so a retype away from llama_cpp cannot leave a stale true
```

**(f)** `service_runtime.go`, doc comment on
`PutRuntimeSpecRequest.ResponsesLiveTimingsEnabled` — replace

```go
	// kind-dependent default (ON for a llama_cpp/vllm spec) while a later save
```

with

```go
	// kind-dependent default (ON for a llama_cpp spec) while a later save
```

Nothing else in `internal/portal` changes: the three production call sites of
`routing.LiveTimingsCapableKind` (`createApplication`'s `liveTimingsCapable`,
`updateApplication`'s refusal arm and its clear arm, `putRuntimeSpec`'s
`liveTimingsCapable`) all read the predicate and need no edit.

---

- [ ] **Step 11: Run the portal package green**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/portal/...
```

Expected: `ok  	op-ai-gateway/internal/portal`. Full package, no `-run`
filter — the live-timings tests are spread across
`service_applications_test.go`, `service_runtime_test.go` and
`service_runtime_benchmark_test.go`.

---

- [ ] **Step 12: The two architecture documents, then the docs check**

**(a)** `docs/architecture/reference/api-surface.md`, the "absent is not the
same as `false`" bullet of the `responses_live_timings_enabled` section —
replace

```markdown
    upsert means by "create" — takes the kind-dependent default: `true` for
    `llama_cpp`/`vllm`, `false` for every other kind. An explicit `false` is a
    deliberate off, honoured on any kind.
```

with

```markdown
    upsert means by "create" — takes the kind-dependent default: `true` for
    `llama_cpp`, `false` for every other kind. An explicit `false` is a
    deliberate off, honoured on any kind. `vllm` was on that list until
    2026-09-12, when a streamed vLLM `/v1/responses` request carrying
    `timings_per_token` was measured to produce 48 frames and **no** `timings`
    object at all: vLLM accepts the key and does nothing with it, so the
    default promised a figure that never arrives — and, for the same reason,
    an explicit `true` on a `vllm` application or on a spec whose effective
    type is `vllm` is now **refused** by the same three error codes below. The
    gateway's own shape clause for the `/v1/chat/completions` live-progress
    parameters still lists `vllm`; the two lists are no longer one list.
```

Deliberately **no cross-document link** in that paragraph: `check-docs.sh`
resolves every intra-repo link and anchor, and a guessed anchor into
`telemetry-usage-observability.md` fails the gate. If you want the link, grep
the target heading first and build the anchor from the real text.

**(b)** `docs/architecture/cross-cutting/agent-runtime-manager.md`, the
`responses_live_timings_enabled` paragraph of §11.5 — replace

```markdown
detected from `binary`) is not `llama_cpp`/`vllm` is refused with **400**,
```

with

```markdown
detected from `binary`) is not `llama_cpp` is refused with **400** (`vllm` left
that set on 2026-09-12, measured inert on `/v1/responses`),
```

Then:

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && ./scripts/check-docs.sh
```

Expected: `check-docs: OK` and exit 0. If it reports an unresolved anchor, that
is the link in (a) — fix or remove it.

**Do not edit** `docs/architecture/cross-cutting/telemetry-usage-observability.md`
in this task. Its "That map is keyed on exactly two values today, `llama_cpp`
and `vllm`" describes `liveProgressUpstreams`, which keeps vLLM, and is still
true.

---

- [ ] **Step 13: Format and lint both Go modules**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && golangci-lint fmt --diff && golangci-lint run
```

Expected: no diff printed by `fmt --diff`, and `golangci-lint run` exits 0.
CI runs `golangci-lint fmt --diff` plus `run` per Go module (gofumpt, gocritic);
`gofmt`/`go vet`/`go test` do not cover them. `server-agent` is untouched, so it
needs no run here. Note that `make lint-go` covers only the `run` half — it is
`golangci-lint run` in each of the two modules and never `fmt --diff`, and
`make fmt` reformats in place rather than reporting a diff — so run the command
above as written.

---

- [ ] **Step 14: Mutation check — revert the one production line and confirm what fails**

Production files only, one change at a time. Temporarily re-add
`ProviderVLLM: {}` to `liveTimingsCapableKinds` in
`gateway/backend/internal/routing/live_timings.go` (nothing else), then:

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/routing/... ./internal/provider/... ./internal/portal/...
```

Expected — `internal/routing` and `internal/portal` FAIL, `internal/provider`
stays green:

- `--- FAIL: TestLiveTimingsCapableKind`:
  `LiveTimingsCapableKind("vllm") = true, want false` (twice) and
  `the table expects 1 capable kinds but liveTimingsCapableKinds has 2: …`
- `--- FAIL: TestLiveTimingsCapableKindsSizeIsPinned`:
  `liveTimingsCapableKinds has 2 kinds, pinned at 1: …`
- `--- FAIL: TestLiveTimingsCapableKindsShareOneStringAcrossBothVocabularies`:
  `LiveTimingsCapableKind("vllm") = true, want false: one string, one verdict …`
- `--- FAIL: TestCreateApplicationResponsesLiveTimingsDefaultsByUpstreamKind/vllm`:
  `vllm: ResponsesLiveTimingsEnabled = true, want false`
- `ok  	op-ai-gateway/internal/provider` — expected and explained in Step 8.

Then **restore the line's removal** (undo the mutation) and re-run the same
command; all three must be `ok`.

Two further mutations, if you want the provider side's own guard confirmed
(each applied and undone alone, in `internal/provider/live_progress.go`):

- Add `routing.ProviderOllama: {}` to `liveProgressUpstreams` → the first loop
  of `TestLiveProgressUpstreamsCoverEveryRoutingCapableKind` fails with
  `liveProgressUpstreams has "ollama", which routing.LiveTimingsCapableKind calls incapable and which is not the one recorded divergence ("vllm") …`,
  alongside **four** expectations in three other tests of that package, all of
  which expect ollama to stay off the shape clause:
  `TestWantsLiveProgressAllowList` (`wantsLiveProgress("ollama") = true, want false`),
  `TestWantsLiveProgressThreeLayerRule` twice (`undetermined + ollama: not on
  the shape allow-list` and `undetermined + server_agent whose effective spec
  type is ollama`), and `TestWantsLiveProgressReadsTheJoinedVerdict` once
  (`absent row falls through to an intolerant shape clause, like an empty
  verdict`).
- Remove `routing.ProviderVLLM: {}` from `liveProgressUpstreams` → the parity
  test stays green by design; what fails is three expectations in two tests:
  `TestWantsLiveProgressAllowList` (`wantsLiveProgress("vllm") = false, want true`)
  and `TestWantsLiveProgressThreeLayerRule` twice, on `undetermined + vllm shape
  implies tolerant` and on `undetermined + server_agent whose effective spec
  type is vllm`. `TestWantsLiveProgressReadsTheJoinedVerdict` stays green — it
  names only `llama_cpp` and `ollama`.

---

- [ ] **Step 15: Run the whole backend suite**

The change crosses four packages, so the verdict is the whole module, not the
three packages touched. `internal/gateway` alone costs about 95 s; budget a few
minutes.

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./...
```

Expected: every package `ok` or `no test files`. In particular
`op-ai-gateway/internal/store` must be `ok`: its two live-timings guards
(`TestConformanceApplicationReadersAgreeOnEveryColumn`'s `capableOnly` check and
`TestRoutingStoreRuntimeSpecs`' effective-kind check) both require the seeding
type to be **incapable**, and both seed `ollama`, which this change does not
move. The PostgreSQL subtests skip silently without a DSN; that is acceptable
here because nothing in this task touches a store path or a migration.

---

- [ ] **Step 16: Commit**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && git add \
  gateway/backend/internal/routing/live_timings.go \
  gateway/backend/internal/routing/live_timings_test.go \
  gateway/backend/internal/provider/live_progress_kind_parity_test.go \
  gateway/backend/internal/portal/service_applications.go \
  gateway/backend/internal/portal/service_applications_test.go \
  gateway/backend/internal/portal/service_runtime.go \
  gateway/backend/internal/portal/service_runtime_test.go \
  docs/architecture/reference/api-surface.md \
  docs/architecture/cross-cutting/agent-runtime-manager.md && git status --short
```

Confirm exactly those nine paths are staged and nothing else, then:

```bash
git commit -F - <<'MSG'
refactor: vLLM leaves the live-timings capable-kind set

Measured on 2026-09-12 against a live vLLM upstream serving /v1/responses
through the gateway: a streamed request carrying "timings_per_token": true
produced 48 data frames and not one of them carried a top-level timings
object -- not the partials, not the terminal frame. vLLM accepts the key,
because its request models allow unknown fields, and does nothing with it.
timings_per_token is a llama.cpp parameter. (One deployment, one build,
whose build identifier was not recorded; the same scope caveat the
repository already applies to #80's measurement.)

routing.LiveTimingsCapableKind decides which kinds get the responses
live-timings opt-in defaulted ON for a newly created application or runtime
spec. A switch that is offered, defaults on, and provably delivers nothing
is worse than one that is not offered, so the set becomes llama_cpp alone.
The same predicate also gates the write rules, so an explicit true on a
vllm application or on a spec whose effective type is vllm is now refused
rather than stored.

internal/provider's liveProgressUpstreams KEEPS vllm. It gates a different
parameter pair -- timings_per_token together with
stream_options.continuous_usage_stats -- on a different endpoint,
/v1/chat/completions, where continuous_usage_stats is a first-class vLLM
field that works. So the two hand-written lists stop being one list: the
cross-package tripwire relaxes from equality to "every capable kind is in
the gate", keeps the direction that ranges over the real gate map as a
one-recorded-divergence check, and is renamed to
TestLiveProgressUpstreamsCoverEveryRoutingCapableKind so its name no longer
claims a parity it does not assert. The size pin's failure message told the
reader to go and make the provider-side gate agree -- the wrong half to
follow here -- and is amended in the same edit, as are the comments on both
sides that gave "the same reason" for the two sets being one.

The clear is prospective and there is no migration or one-time sweep: a
vLLM application or spec created between part 1 and this commit keeps its
stored true until its next save, where the portal's existing by-kind clear
fires on the post-mutation type. Issue #81 decision (e) puts that refusal
in the portal and never in SQL, and both store parity fixtures deliberately
seed their true on ollama, so neither moves.

Part of issue #81 part 2, design decision D6.
MSG
```

The body is substantive on purpose: this repository's GitHub squash-merge
pre-fills the PR description from the commit message body, not from the PR
body.

---

### Task 2: The gate predicate

Every path below is relative to the worktree root
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection`.
The Go module root is `gateway/backend` (module `op-ai-gateway`), so package
`gateway` is `op-ai-gateway/internal/gateway`.

**Files:**
- Create: `gateway/backend/internal/gateway/responses_live_timings.go`
- Test: `gateway/backend/internal/gateway/responses_live_timings_test.go`
- Modify: `gateway/backend/internal/routing/resolver.go` — the **doc comment** on the `Target.ResponsesLiveTimingsEnabled` field (comment text only; no code changes in this file)
- Modify: `gateway/backend/internal/routing/resolver_live_timings_test.go` — the last paragraph of the doc comment above `TestTargetResponsesLiveTimingsPrecedence`
- Modify: `gateway/backend/internal/routing/live_timings.go` — the doc comment on `LiveTimingsCapableKind` (comment text only)

**Interfaces:**
- Consumes (from Task 1): `func routing.LiveTimingsCapableKind(kind string) bool` — after Task 1 it is true for `routing.ProviderLlamaCPP` (`"llama_cpp"`, the same string as `routing.RuntimeSpecTypeLlamaCpp`) alone.
- Consumes (already on `main`): the `routing.Target` fields `ResponsesLiveTimingsEnabled bool`, `Provider string`, `LiveProgressSpecType string`, `LiveProgressSupport string`; the constants `routing.ProviderServerAgent`, `routing.ProviderLlamaCPP`, `routing.ProviderVLLM`, `routing.APIFlavorOpenAI`, `routing.CapabilityNo`, `routing.RuntimeSpecTypeLlamaCpp`, `routing.RuntimeSpecTypeVLLM`, `routing.RuntimeSpecTypeCustom`.
- Produces, in package `gateway`:
  - `func wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool`
  - `const liveTimingsVerdictUnsupported = "unsupported"`
- The injection task calls it inside `proxyNative` (file `native_passthrough.go`), beside `rewriteModelField`, as `wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream)` — both arguments are already in scope there as named parameters.

Nothing calls the predicate in production at the end of this task. That is
expected: `golangci-lint` runs with `run.tests` at its default `true`, so
`unused` counts the test as a use. (If you ever see `wantsResponsesLiveTimings
is unused`, the linter was run with tests excluded — not a defect to fix here.)

---

- [ ] **Step 1: Write the failing test**

Create `gateway/backend/internal/gateway/responses_live_timings_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"op-ai-gateway/internal/routing"
	"testing"
)

// liveTimingsPositiveTarget is the ONE target shape that must answer true: an
// ordinary llama.cpp application whose operator opt-in is on and whose
// live-progress verdict was never determined. Every row of the table below is
// this value with exactly ONE of the five conditions changed, which is what
// makes a row a test of one condition instead of a test of the conjunction.
// (The two server_agent rows move Provider and LiveProgressSpecType together,
// because a server_agent Provider with no spec type is a DIFFERENT condition-2
// row and is listed separately.)
//
// APIFlavor is deliberately left empty. That field carries the COARSE flavor --
// routing.Resolver.Resolve fills it from NormalizeAPIFlavor, so it is "openai"
// or "anthropic" and cannot tell /v1/responses from /v1/chat/completions -- so
// a gate that read it instead of the endpoint's own flavor would be answering a
// different question. Leaving it empty makes that shortcut fail THIS row, the
// first one, rather than hiding until someone adds a chat-completions caller.
func liveTimingsPositiveTarget() routing.Target {
	return routing.Target{
		RouteID:                     "map_live_timings",
		Provider:                    routing.ProviderLlamaCPP,
		ResponsesLiveTimingsEnabled: true,
	}
}

// TestWantsResponsesLiveTimings walks the gate's five conditions with a row per
// condition FAILING ALONE, because the whole hazard of this predicate is that a
// forgotten conjunct leaves every other test in the repository green: the two
// standing "the relayed body must not grow a timings_per_token flag"
// assertions on the passthrough path hold for a fixture whose opt-in is off,
// so they stay silent for a gate that forgets the flavor, the stream or the
// veto. This table is the only thing that does not.
//
// Which row isolates what:
//
//   - condition 1 (the operator's opt-in): "the operator opt-in is off".
//   - condition 2 (the effective kind is llama.cpp): the four rows naming a
//     kind. "vLLM is not a capable kind" also pins issue #81's D6 -- the key is
//     a llama.cpp parameter that a vLLM upstream was measured accepting with a
//     200 and answering with no timings object on any frame. The two positive
//     rows are the other half of the pair: "a server_agent child whose
//     effective kind is llama.cpp" fails a gate that asks about Provider alone,
//     and "baseline" fails a gate that asks about LiveProgressSpecType alone.
//   - condition 3 (the Responses flavor): the three flavor rows. "no flavor at
//     all" fails a gate written as "not anthropic_messages"; "the coarse flavor
//     is not the endpoint's own" fails one written over NormalizeAPIFlavor; and
//     "the Anthropic Messages flavor" carries a coarse "openai" on the TARGET,
//     so it fails a gate that reads target.APIFlavor instead of the parameter.
//   - condition 4 (streaming): "a buffered request".
//   - condition 5 (the stored verdict is not an explicit negative): the veto is
//     a veto, not a requirement, so BOTH "" (the baseline) and "supported"
//     inject. Only "unsupported" refuses. The "no" row is the vocabulary
//     boundary: routing.LiveProgressSupportFromVerdict is the single producer
//     of this field and translates the capability row's routing.CapabilityNo
//     INTO "unsupported", so a raw "no" cannot reach a real Target -- the row
//     exists to fail a veto written against routing.CapabilityNo, which would
//     never refuse anything.
func TestWantsResponsesLiveTimings(t *testing.T) {
	cases := []struct {
		// name says which condition the row isolates; it is also what the
		// failure prints, so it has to read as a claim.
		name string
		// mutate applies the one change that distinguishes this row from
		// liveTimingsPositiveTarget. nil means the untouched positive.
		mutate    func(*routing.Target)
		apiFlavor string
		stream    bool
		want      bool
	}{
		// ---- the positives ----
		{
			name:      "baseline: llama.cpp application, opt-in on, verdict never determined",
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name: "a server_agent child whose effective kind is llama.cpp",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeLlamaCpp)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "a recorded positive verdict changes nothing",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = "supported" },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "the capability row's own \"no\" is not this field's vocabulary",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = routing.CapabilityNo },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},
		{
			name:      "an ordinary application's spec type is never consulted",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeCustom) },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      true,
		},

		// ---- one condition failing alone, per condition ----
		{
			name:      "the operator opt-in is off",
			mutate:    func(tgt *routing.Target) { tgt.ResponsesLiveTimingsEnabled = false },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "vLLM is not a capable kind",
			mutate:    func(tgt *routing.Target) { tgt.Provider = routing.ProviderVLLM },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name: "a server_agent child whose effective kind is vLLM",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeVLLM)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name: "a server_agent child with no spec at all, which resolves to custom",
			mutate: func(tgt *routing.Target) {
				tgt.Provider = routing.ProviderServerAgent
				tgt.LiveProgressSpecType = string(routing.RuntimeSpecTypeCustom)
			},
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "a server_agent target whose spec type was never filled",
			mutate:    func(tgt *routing.Target) { tgt.Provider = routing.ProviderServerAgent },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
		{
			name:      "the Anthropic Messages flavor",
			mutate:    func(tgt *routing.Target) { tgt.APIFlavor = routing.APIFlavorOpenAI },
			apiFlavor: "anthropic_messages",
			stream:    true,
			want:      false,
		},
		{
			name:      "no flavor at all",
			apiFlavor: "",
			stream:    true,
			want:      false,
		},
		{
			name:      "the coarse flavor is not the endpoint's own",
			apiFlavor: routing.APIFlavorOpenAI,
			stream:    true,
			want:      false,
		},
		{
			name:      "a buffered request",
			apiFlavor: "openai_responses",
			stream:    false,
			want:      false,
		},
		{
			name:      "a recorded rejection vetoes the opt-in",
			mutate:    func(tgt *routing.Target) { tgt.LiveProgressSupport = "unsupported" },
			apiFlavor: "openai_responses",
			stream:    true,
			want:      false,
		},
	}
	for _, tc := range cases {
		target := liveTimingsPositiveTarget()
		if tc.mutate != nil {
			tc.mutate(&target)
		}
		if got := wantsResponsesLiveTimings(target, tc.apiFlavor, tc.stream); got != tc.want {
			t.Errorf("%s: wantsResponsesLiveTimings(target{provider=%q spec_type=%q enabled=%v verdict=%q coarse_flavor=%q}, %q, stream=%v) = %v, want %v",
				tc.name, target.Provider, target.LiveProgressSpecType, target.ResponsesLiveTimingsEnabled,
				target.LiveProgressSupport, target.APIFlavor, tc.apiFlavor, tc.stream, got, tc.want)
		}
	}
}
```

Note the two literals the test spells out itself rather than borrowing from
production: `"unsupported"` and `"openai_responses"`. Borrowing the constant
`liveTimingsVerdictUnsupported` here would make the test blind to a change in
its value (mutation M7 below), which is precisely the mistake this vocabulary
invites.

- [ ] **Step 2: Run it and watch it fail**

Run: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/`

Expected — a BUILD failure, not an assertion failure, because the function does
not exist yet (line:column will differ):

```
# op-ai-gateway/internal/gateway [op-ai-gateway/internal/gateway.test]
internal/gateway/responses_live_timings_test.go:214:6: undefined: wantsResponsesLiveTimings
FAIL	op-ai-gateway/internal/gateway [build failed]
FAIL
```

This test cannot fail "vacuously green" the way a test of an already-decoded
JSON field can: its whole subject is code that does not exist. The vacuity risk
here is a different one — a table that omits a condition's negative row — and
step 5 and step 6 are what rule that out.

(If the sandbox refuses loopback listeners, this package's other tests need
them — `httptest.NewServer`, see AGENTS.md "Build And Verification Commands".
Grant it; do not narrow the run.)

- [ ] **Step 3: Write the predicate**

Create `gateway/backend/internal/gateway/responses_live_timings.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import "op-ai-gateway/internal/routing"

// liveTimingsVerdictUnsupported is the ONE spelling of "this upstream was
// observed rejecting the live-progress parameters" that can ever reach
// routing.Target.LiveProgressSupport. That field's vocabulary is closed --
// "" (never determined), "supported", "unsupported" -- and has a single
// producer, routing.LiveProgressSupportFromVerdict, which every writer of the
// field goes through: the candidate join in both store drivers and the
// benchmark path's own keyed read. The capability ROW is spelled differently
// ("yes"/"no", routing.CapabilityYes/CapabilityNo) and is what that helper
// CONSUMES, never what it produces -- so a veto written against
// routing.CapabilityNo would compile, read plausibly, and never fire.
const liveTimingsVerdictUnsupported = "unsupported"

// wantsResponsesLiveTimings reports whether this request's outgoing body may
// carry llama.cpp's `timings_per_token`. It is a pure function of the resolved
// target, the CLIENT API flavor the dispatch layer routed on and the stream
// flag -- no clock, no store, no memo -- so the whole rule is table-testable
// (TestWantsResponsesLiveTimings), which matters because a forgotten conjunct
// here leaves the rest of the suite green.
//
// All five must hold:
//
//  1. target.ResponsesLiveTimingsEnabled -- the operator's per-endpoint opt-in,
//     resolved spec-over-application by routing.Resolver.targetFrom. It is the
//     only condition an operator controls, and the feature is off without it.
//  2. The EFFECTIVE upstream kind is one routing.LiveTimingsCapableKind
//     accepts: llama.cpp alone. `timings_per_token` is a llama.cpp parameter,
//     and a vLLM upstream was measured answering 200 to it and attaching no
//     `timings` object to any frame of the resulting stream -- offered,
//     accepted, inert. For a server_agent target the kind is
//     target.LiveProgressSpecType (the resolved spec's
//     EffectiveRuntimeSpecType), NEVER target.Provider, which is the literal
//     "server_agent" and says nothing about what actually serves; that is the
//     same substitution internal/provider's wantsLiveProgress makes for its own
//     shape clause. The kind is re-checked HERE rather than trusted from the
//     stored flag because the store is deliberately policy-free: the portal
//     refuses a true on an incapable kind, but every store path round-trips one
//     for any kind, so a restored dump or a direct write can present a true
//     this path must not act on.
//  3. The flavor is the Responses endpoint's own. proxyNative serves
//     /v1/responses and /v1/messages from one function, llama.cpp attaches no
//     `timings` to any Anthropic frame, and /v1/messages is out of scope for
//     issue #81. The FINE flavor is a parameter rather than target.APIFlavor
//     because that field carries the COARSE value (routing.Resolver.Resolve
//     fills it from NormalizeAPIFlavor), which cannot tell this endpoint from
//     /v1/chat/completions.
//  4. The request is streaming. The flag exists to put a rate on a PARTIAL
//     frame; a buffered response has none, and proxyNative allocates no
//     live-progress counter for one.
//  5. The stored live-progress verdict is not an explicit negative. This is a
//     VETO, not a requirement. Requiring a positive verdict would make the
//     operator's switch silently dead wherever the capability probe never ran
//     -- the worst possible failure for a control that is switched on -- and
//     within condition 2's population a positive verdict allows nothing the
//     veto has not allowed already. The verdict is a /props observation about
//     the COMPLETION endpoint's parameter schema, so it is trusted to refuse
//     and not to permit.
//
// Deliberately NOT internal/provider's wantsLiveProgress, whose shape this
// resembles: that predicate's verdict layer is a requirement rather than a
// veto, its own set still includes vLLM (correctly -- it gates a different
// parameter pair on a different endpoint, where vLLM's half is a first-class
// field), and a fourth check is ANDed at its only call site that a caller from
// here would silently drop.
func wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool {
	if !target.ResponsesLiveTimingsEnabled {
		return false
	}
	// The same literal endpointModeFor switches on for this endpoint. A rename
	// there fails this shut rather than open, which is the safe direction.
	if apiFlavor != "openai_responses" {
		return false
	}
	if !stream {
		return false
	}
	if target.LiveProgressSupport == liveTimingsVerdictUnsupported {
		return false
	}
	kind := target.Provider
	if target.Provider == routing.ProviderServerAgent {
		kind = target.LiveProgressSpecType
	}
	return routing.LiveTimingsCapableKind(kind)
}
```

- [ ] **Step 4: Run the full package and watch it pass**

Run: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/`

Expected (the package is large — budget about 95 s; the exact seconds differ):

```
ok  	op-ai-gateway/internal/gateway	95.412s
```

If `routing.LiveTimingsCapableKind` still accepts vLLM (Task 1 not landed, or
landed incompletely), the row `vLLM is not a capable kind` fails here with:

```
    responses_live_timings_test.go:NNN: vLLM is not a capable kind: wantsResponsesLiveTimings(target{provider="vllm" spec_type="" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
```

That is a real dependency failure, not a flake: stop and finish Task 1.

- [ ] **Step 5: Prove the table, mutations M1–M3 (conditions 1 and 2)**

Reverting the *test* file proves nothing (a package with no such test prints
`ok`). Reverting the *production* file deletes the only definition and the
package stops building — a stronger signal than a red assertion, but not one
that says the table covers each condition. So apply each mutation below to
`responses_live_timings.go` **one at a time**, run, confirm exactly the listed
rows go red, then restore the file before the next.

For these probes only, `-run TestWantsResponsesLiveTimings` is allowed: they
interrogate the test, not the change. The verdict runs are step 4 and step 10,
and both run whole packages.

Run each time: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/ -run TestWantsResponsesLiveTimings`

| # | Condition | The ONE edit | Rows that must go red |
|---|---|---|---|
| M1 | 1, the operator opt-in | delete the `if !target.ResponsesLiveTimingsEnabled` block | 1 row |
| M2 | 2a, the kind substitution | delete the `if target.Provider == routing.ProviderServerAgent { kind = ... }` block | 1 row |
| M3 | 2b, the capability lookup | replace the final `return routing.LiveTimingsCapableKind(kind)` with `return true` | 4 rows |

Exactly what each prints (the test-file line number will differ; nothing else
will):

```
M1: the operator opt-in is off: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=false verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false

M2: a server_agent child whose effective kind is llama.cpp: wantsResponsesLiveTimings(target{provider="server_agent" spec_type="llama_cpp" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = false, want true

M3: vLLM is not a capable kind: wantsResponsesLiveTimings(target{provider="vllm" spec_type="" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
M3: a server_agent child whose effective kind is vLLM: wantsResponsesLiveTimings(target{provider="server_agent" spec_type="vllm" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
M3: a server_agent child with no spec at all, which resolves to custom: wantsResponsesLiveTimings(target{provider="server_agent" spec_type="custom" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
M3: a server_agent target whose spec type was never filled: wantsResponsesLiveTimings(target{provider="server_agent" spec_type="" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
```

M2 must turn exactly one row red. If the three negative server_agent rows also
go red, the edit was compound — restore and redo it.

- [ ] **Step 6: Prove the table, mutations M4–M7 (conditions 3, 4, 5)**

Same procedure, same command, one at a time, restoring in between.

| # | Condition | The ONE edit | Rows that must go red |
|---|---|---|---|
| M4 | 3, the flavor | delete the `if apiFlavor != "openai_responses"` block | 3 rows |
| M5 | 4, the stream | delete the `if !stream` block | 1 row |
| M6 | 5, the veto | delete the `if target.LiveProgressSupport == liveTimingsVerdictUnsupported` block | 1 row |
| M7 | 5's spelling | change `liveTimingsVerdictUnsupported`'s value to `"no"` | 2 rows |

```
M4: the Anthropic Messages flavor: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="" coarse_flavor="openai"}, "anthropic_messages", stream=true) = true, want false
M4: no flavor at all: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="" coarse_flavor=""}, "", stream=true) = true, want false
M4: the coarse flavor is not the endpoint's own: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="" coarse_flavor=""}, "openai", stream=true) = true, want false

M5: a buffered request: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="" coarse_flavor=""}, "openai_responses", stream=false) = true, want false

M6: a recorded rejection vetoes the opt-in: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="unsupported" coarse_flavor=""}, "openai_responses", stream=true) = true, want false

M7: the capability row's own "no" is not this field's vocabulary: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="no" coarse_flavor=""}, "openai_responses", stream=true) = false, want true
M7: a recorded rejection vetoes the opt-in: wantsResponsesLiveTimings(target{provider="llama_cpp" spec_type="" enabled=true verdict="unsupported" coarse_flavor=""}, "openai_responses", stream=true) = true, want false
```

M7 is the one that proves the veto is spelled in the field's vocabulary and not
the capability row's. If it turns nothing red, the test borrowed the production
constant instead of the literal — fix the test.

After the last restore, confirm the file is back: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && git diff --stat gateway/backend/internal/gateway/responses_live_timings.go` prints nothing (the file is still untracked at this point, so `git status --short` showing `?? gateway/backend/internal/gateway/responses_live_timings.go` is the expected state).

- [ ] **Step 7: Correct the two "nothing reads the resolved flag" claims in `internal/routing`**

Both are comments this task makes false. Repo-facing text is English. Cite
**issue #81** only — never the branch-local design document's path, which is
deleted before the pull request.

In `gateway/backend/internal/routing/resolver.go`, in the doc comment on the
`Target.ResponsesLiveTimingsEnabled` field, replace:

```go
	// the spec for a server_agent child. Nothing reads it yet; the gate, the
	// injection and the retry are part 2 of issue #81.
```

with:

```go
	// the spec for a server_agent child. Its reader on the request path is
	// gateway.wantsResponsesLiveTimings, the part-2 gate of issue #81, which
	// ANDs this flag with the effective upstream kind, the Responses flavor
	// and the stream flag and lets a recorded live-progress rejection veto the
	// lot. No retry accompanies it: llama.cpp was measured accepting the
	// injected key on this endpoint, so a retry's trigger could not be
	// exercised against any upstream this repository can point at.
```

In `gateway/backend/internal/routing/resolver_live_timings_test.go`, in the doc
comment above `TestTargetResponsesLiveTimingsPrecedence`, replace:

```go
// No production code reads this field in part 1 of issue #81; the gate, the
// timings_per_token injection and the retry are part 2. This test is the whole
// of the field's current contract.
```

with:

```go
// This test is the field's PRECEDENCE contract -- which row's value wins -- and
// nothing else. What the request path then does with the resolved value is the
// part-2 gate's contract, pinned separately by TestWantsResponsesLiveTimings in
// internal/gateway. Neither test can fail for the other's regression, which is
// why both exist.
```

- [ ] **Step 8: Correct `LiveTimingsCapableKind`'s "nothing about it belongs to the request path"**

Open `gateway/backend/internal/routing/live_timings.go` and read the doc comment
on `LiveTimingsCapableKind` as it stands now — Task 1 edited this file and may
already have reworded the surrounding text. If its first sentences still say the
portal's create paths are the only callers and that nothing about the helper
belongs to the request path, replace:

```go
// Exported because the portal's create paths are its callers and they live in
// another package; it is a statement about a KIND, not a read of any stored
// flag, so nothing about it belongs to the request path.
```

with:

```go
// Exported because its callers live in other packages: the portal's create
// paths, which use it to pick the stored default, and the request path's own
// gate, gateway.wantsResponsesLiveTimings, which re-asks it about the
// EFFECTIVE kind of an already-resolved target. It is still a statement about a
// KIND and reads no stored flag -- the request-path caller supplies the kind
// and ANDs the answer with the stored opt-in itself, which is exactly what lets
// the store stay policy-free about which kinds may hold a true.
```

If Task 1 already replaced that sentence with wording that names a request-path
caller, leave it alone and say so in the commit body. Do not restate the capable
SET here in either case — Task 1 owns that sentence.

- [ ] **Step 9: Format and lint**

Run: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && golangci-lint fmt --diff && golangci-lint run`

Expected: `golangci-lint fmt --diff` prints nothing and exits 0 (gofumpt +
goimports, which CI gates on and `gofmt`/`go vet` do not cover); `golangci-lint
run` ends with `0 issues.`. If `fmt --diff` prints a diff, apply it with
`golangci-lint fmt` and re-run both.

- [ ] **Step 10: Full-package verdict for both touched packages**

Run: `cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/ ./internal/routing/`

Expected (seconds differ; `internal/gateway` is the ~95 s one):

```
ok  	op-ai-gateway/internal/gateway	95.412s
ok  	op-ai-gateway/internal/routing	12.884s
```

`internal/routing` is in the run because steps 7 and 8 edited two of its files;
they are comment-only edits, so a failure there means something else was
touched.

- [ ] **Final step: Commit**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && \
git add gateway/backend/internal/gateway/responses_live_timings.go \
        gateway/backend/internal/gateway/responses_live_timings_test.go \
        gateway/backend/internal/routing/resolver.go \
        gateway/backend/internal/routing/resolver_live_timings_test.go \
        gateway/backend/internal/routing/live_timings.go && \
git commit -F - <<'MSG'
feat: Answer "may this request carry timings_per_token?" in one pure predicate

Part 2 of issue #81 needs that question settled before any outgoing body is
touched. wantsResponsesLiveTimings answers it as a pure function of the
resolved routing.Target, the client API flavor and the stream flag -- no clock,
no store, no memo -- so the whole rule is table-testable. Nothing calls it yet;
the injection is the next commit.

Five conditions, all required. The operator's per-endpoint opt-in. An effective
upstream kind routing.LiveTimingsCapableKind accepts, read from
LiveProgressSpecType for a server_agent target and never from the literal
"server_agent" its Provider carries. The Responses flavor, taken as a parameter
rather than from Target.APIFlavor, which carries the coarse value and cannot
tell this endpoint from /v1/chat/completions. Streaming, because the flag exists
to put a rate on a partial frame. And a stored live-progress verdict that is not
the explicit "unsupported".

That last one is a VETO, not a requirement. Requiring a positive verdict would
make the operator's switch silently dead wherever the capability probe never
ran, which is the worst failure available to a control that is switched on. The
verdict is also spelled in Target.LiveProgressSupport's closed vocabulary, whose
single producer is routing.LiveProgressSupportFromVerdict -- a veto written
against the capability row's own routing.CapabilityNo would read plausibly and
never fire, so the table pins that too.

The kind is re-checked here rather than trusted from the stored flag: the store
is policy-free by design and round-trips a true for any kind, so a restored dump
or a direct write can present one this path must not act on.

The test carries a row per condition failing alone, because a forgotten conjunct
here leaves every other test in the repository green -- the two standing "the
relayed body must not grow a timings_per_token flag" assertions hold for a
fixture whose opt-in is off, so they stay silent for a gate that forgets the
flavor, the stream or the veto.

Three comments that recorded the resolved flag as unread, and the capable-kind
helper as having no request-path caller, are corrected here because this commit
is what makes them false. The four documentation sites that say nothing acts on
the value still hold -- no request behaves differently yet -- and belong to the
commit that lands the injection.
MSG
```

If this session's system-reminder prescribes attribution lines for commit
messages, append them at the end of the body. Do not commit to or merge into
`main`; this worktree is on branch `responses-timings-injection`, which is where
the commit belongs.

---

### Task 3: The `timings_per_token` injection helper

Everything below is relative to the worktree
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection`.
Go module root for every command: `<worktree>/gateway/backend` (module `op-ai-gateway`).

**Goal:** a pure function that takes the outgoing Responses body and returns a NEW
slice carrying `"timings_per_token": true`, plus a boolean saying whether it
injected. Nothing calls it yet — the gate and the call site are a later task.

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` — add one
  constant and one function, `injectTimingsPerToken`, immediately **after** the
  closing brace of `rewriteModelField` (that brace is line 527 today; line 528 is
  blank and line 529 starts the `parsePassthroughUsage` doc comment). Change
  nothing else in this file.
- Test: `gateway/backend/internal/gateway/native_passthrough_timings_inject_test.go` (new file)

**Interfaces:**
- Consumes: nothing from earlier tasks. Only `bytes` and `encoding/json`, both
  already in `native_passthrough.go`'s import block — do not add imports.
- Produces (package `gateway`, both unexported, both in `native_passthrough.go`):
  ```go
  const timingsPerTokenKey = "timings_per_token"

  // returns (body, injected); body is a fresh slice only when injected is true
  func injectTimingsPerToken(raw []byte) ([]byte, bool)
  ```

**Two things you must NOT do in this task:**
1. Do not call `injectTimingsPerToken` from `proxyNative` (or anywhere else in
   production code). Wiring it is the next task's job.
2. Do not edit the long comment block directly above
   `upstreamBody := rewriteModelField(raw, target.ProviderModel)` (lines 282-302
   today), which still states that the gateway does not add `timings_per_token`.
   It is one of nine coupled sites; THIS block belongs to Task 4 (steps 9a/9b),
   not to the documentation task -- see Task 8's own table, which marks both
   `proxyNative` comments "Task 4 -- do not touch". Editing
   it here would leave the other eight contradicting it.

**Why a separate function instead of extending `rewriteModelField`.**
`rewriteModelField` has **four** `return raw` statements: three no-op branches —
(a) `providerModel == ""`, (b) the `dec.Decode(&obj)` error, (c)
`cur == providerModel` — and (d) a `json.Marshal` error fallback. Branch (c) is an
ordinary configuration, not an edge case: the portal's application auto-sync writes
the gateway model name and the provider model name to the same string, so for those
mappings `rewriteModelField` returns early and an injection folded in below that
branch would silently never fire.

**The decoder detail.** `rewriteModelField` **does** call `dec.UseNumber()`, and the
new helper must too. Without it every number in the body round-trips through
`float64`: `9007199254740993` (2^53+1, not representable) comes back as
`9007199254740992`, a silent edit to a body this path otherwise forwards as the
client wrote it. `TestRewriteModelField` in `server_test.go` already pins exactly
that literal for the model rewrite; step 1 pins it for the injection.

---

- [ ] **Step 1: Write the failing test**

Create `gateway/backend/internal/gateway/native_passthrough_timings_inject_test.go`
with exactly this content:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"bytes"
	"encoding/json"
	"testing"
)

// decodeInjected decodes a body the way this package decodes one — UseNumber, so a
// large integer literal is compared as it was written rather than through float64.
func decodeInjected(t *testing.T, body []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// A body without the key gets it, every sibling field survives the round trip, and
// the client's own slice is not written to. The "a<b&c" value pins that
// re-serialization is VALUE-lossless: json.Marshal writes it as a<b&c,
// which any JSON parser reads back as the original string.
func TestInjectTimingsPerTokenAddsTheFlag(t *testing.T) {
	const body = `{"model":"gw","input":"a<b&c","max_output_tokens":9007199254740993}`
	in := []byte(body)

	out, injected := injectTimingsPerToken(in)

	if !injected {
		t.Fatalf("injected = false, want true for a body without the key: %s", out)
	}
	m := decodeInjected(t, out)
	if m[timingsPerTokenKey] != true {
		t.Fatalf("%s = %#v, want true", timingsPerTokenKey, m[timingsPerTokenKey])
	}
	if m["model"] != "gw" {
		t.Fatalf("model not preserved: %#v", m["model"])
	}
	if m["input"] != "a<b&c" {
		t.Fatalf("input not preserved through HTML escaping: %#v", m["input"])
	}
	if n, _ := m["max_output_tokens"].(json.Number); n.String() != "9007199254740993" {
		t.Fatalf("large integer literal reformatted: %#v (helper lost UseNumber?)", m["max_output_tokens"])
	}
	if string(in) != body {
		t.Fatalf("the input slice was modified: %s", in)
	}
}

// The caller keeps reading the client's own bytes (the payload capture) while the
// HTTP transport is sending the returned ones, so the two must not share a backing
// array. Scribbling over every byte of the result must leave the input intact.
func TestInjectTimingsPerTokenReturnsUnaliasedBytes(t *testing.T) {
	const body = `{"model":"gw","stream":true,"input":"hi"}`
	in := []byte(body)

	out, injected := injectTimingsPerToken(in)

	if !injected {
		t.Fatalf("injected = false, want true for a body without the key: %s", out)
	}
	for i := range out {
		out[i] = 'X'
	}
	if string(in) != body {
		t.Fatalf("the returned slice shares backing memory with the input: %s", in)
	}
}

// Presence, not value: a client that already sent the key keeps its body verbatim.
func TestInjectTimingsPerTokenLeavesAClientSetTrueAlone(t *testing.T) {
	const body = `{"model":"gw","stream":true,"timings_per_token":true,"input":"hi"}`

	out, injected := injectTimingsPerToken([]byte(body))

	if injected {
		t.Fatalf("injected = true for a body that already carries the key")
	}
	if string(out) != body {
		t.Fatalf("body was re-serialized: %s", out)
	}
}

// The same rule with the opposite value, which is the one that can regress on its
// own: llama.cpp honours an explicit false exactly as it honours an absent key, so
// overwriting it would silently reverse a client's choice. Asserting only that the
// key is still PRESENT would pass against a helper that overwrote false with true —
// hence the assertion on the VALUE.
func TestInjectTimingsPerTokenHonoursAnExplicitClientFalse(t *testing.T) {
	const body = `{"model":"gw","stream":true,"timings_per_token":false}`

	out, injected := injectTimingsPerToken([]byte(body))

	// The value is asserted FIRST, deliberately: it is the assertion that states
	// D5's rule, and a helper that tested truth instead of presence would trip it.
	m := decodeInjected(t, out)
	if m[timingsPerTokenKey] != false {
		t.Fatalf("%s = %#v, want the client's false to survive", timingsPerTokenKey, m[timingsPerTokenKey])
	}
	if injected {
		t.Fatalf("injected = true over a client's explicit false")
	}
	if string(out) != body {
		t.Fatalf("body was re-serialized: %s", out)
	}
}

// Anything that is not a JSON object is returned untouched.
func TestInjectTimingsPerTokenLeavesNonObjectBodiesAlone(t *testing.T) {
	for _, body := range []string{`["a","b"]`, `"str"`, `7`, ``} {
		out, injected := injectTimingsPerToken([]byte(body))
		if injected || string(out) != body {
			t.Fatalf("body %q: injected=%v out=%s, want the body unchanged", body, injected, out)
		}
	}
}

// A bare null decodes into a map WITHOUT an error and leaves that map nil;
// assigning into it panics with "assignment to entry in nil map". Nothing on the
// request path can reach the helper with such a body today — sniffRoutingModel
// yields an empty model for null and handleOpenAIResponses only calls
// tryProxyNative for a non-empty one — but that guard lives in
// inference_handlers.go, a different file, so the helper carries its own.
func TestInjectTimingsPerTokenDoesNotPanicOnANullBody(t *testing.T) {
	const body = `null`

	out, injected := injectTimingsPerToken([]byte(body))

	if injected || string(out) != body {
		t.Fatalf("null: injected=%v out=%s, want the body unchanged", injected, out)
	}
}
```

- [ ] **Step 2: Run it and watch it fail**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```
Expected: a **build** failure, in a few seconds — no test runs, because neither
identifier exists yet:
```
# op-ai-gateway/internal/gateway [op-ai-gateway/internal/gateway.test]
internal/gateway/native_passthrough_timings_inject_test.go:31:22: undefined: injectTimingsPerToken
internal/gateway/native_passthrough_timings_inject_test.go:37:7: undefined: timingsPerTokenKey
...
FAIL	op-ai-gateway/internal/gateway [build failed]
```
The `file:line:col` prefixes will differ from the ones above and the list is capped
at ten errors; what must appear is `undefined: injectTimingsPerToken` and
`undefined: timingsPerTokenKey`, and `[build failed]`.

- [ ] **Step 3: Add the constant and the helper**

In `gateway/backend/internal/gateway/native_passthrough.go`, immediately after
`rewriteModelField`'s closing brace (line 527 today — the blank line before the
`parsePassthroughUsage` doc comment), insert one blank line and then:

```go
// timingsPerTokenKey is llama.cpp's per-token timings request flag. With it set,
// the server attaches a top-level `timings` object to PARTIAL frames; without it
// only the terminal frame carries one. That object is the only thing that can put
// an upstream-reported tokens/sec on a running /v1/responses passthrough row.
const timingsPerTokenKey = "timings_per_token"

// injectTimingsPerToken returns the body with a top-level "timings_per_token": true
// added, and whether it actually added it. It is a pure function of its argument:
// the input slice is never written to, and an injected body is always a FRESH slice
// from json.Marshal. That matters at the call site — the returned bytes go to the
// HTTP transport, which is still reading them while the request is in flight, while
// the client's own bytes are read again afterwards to build the payload capture, so
// an in-place edit would race the send and corrupt the capture record.
//
// The body is returned unchanged, with false, in exactly three cases:
//
//   - It is not a JSON object, so the decode into map[string]any fails.
//   - It decodes to a nil map, which is what a bare `null` body does WITHOUT
//     reporting an error; assigning into that map would panic with "assignment to
//     entry in nil map". Nothing on this path can deliver such a body today —
//     sniffRoutingModel reads no model out of `null`, and handleOpenAIResponses
//     only reaches tryProxyNative for a non-empty model — but that guard lives in
//     inference_handlers.go, so this helper does not assume it. (rewriteModelField
//     has the same shape and relies on the same distant guard; changing it is not
//     part of this feature.)
//   - The key is ALREADY PRESENT at the top level, whatever its value. Presence,
//     not value: llama.cpp treats an explicit false exactly as it treats an absent
//     key, so a client that sent false has made a choice, and overwriting it would
//     be the silent rewriting of a client request that this path refuses to do.
//
// Deliberately NOT folded into rewriteModelField, which returns the original slice
// from three no-op branches — an empty providerModel, a body that is not a JSON
// object, and a provider model that already equals the body's model. The last is an
// ordinary configuration (the portal's auto-sync writes the gateway and provider
// model names to the same string), so an injection placed below it would never fire
// for those mappings.
//
// Re-serialization costs what rewriteModelField's doc already concedes: key order
// changes and <>& are HTML-escaped. That is value-lossless to any JSON parser, and
// UseNumber keeps it lossless for numbers too — without it a literal such as
// 9007199254740993 would come back as 9007199254740992.
func injectTimingsPerToken(raw []byte) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return raw, false
	}
	if obj == nil {
		return raw, false
	}
	if _, present := obj[timingsPerTokenKey]; present {
		return raw, false
	}
	obj[timingsPerTokenKey] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return raw, false
	}
	return out, true
}
```

- [ ] **Step 4: Run the whole package and watch it pass**

Run (full package, no `-run` filter; `internal/gateway` takes roughly 95 s):
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```
Expected:
```
ok  	op-ai-gateway/internal/gateway	95.xxxs
```
If instead the `unused` linter later complains that `injectTimingsPerToken` is
unused, that means tests were excluded from the analysis — do not delete it; the
next task adds the production caller.

- [ ] **Step 5: Prove tests 1-3 are load-bearing (one mutation at a time)**

Each row: apply **only** that edit to `native_passthrough.go`, run the full package
command from step 4, confirm the named failure, then revert the edit before the next
row. Never edit the test file to produce a failure.

| # | Production mutation (one edit) | Must fail | Failure text |
|---|---|---|---|
| 1 | In `injectTimingsPerToken`, change `obj[timingsPerTokenKey] = true` to `obj[timingsPerTokenKey] = false` | `TestInjectTimingsPerTokenAddsTheFlag` | `timings_per_token = false, want true` |
| 2 | Change the final `return out, true` to `return raw, true` | `TestInjectTimingsPerTokenReturnsUnaliasedBytes` | `the returned slice shares backing memory with the input: ` followed by 40 `X` characters. `TestInjectTimingsPerTokenAddsTheFlag` fails too, with `timings_per_token = <nil>, want true` — one edit, two consequences |
| 3 | Delete the three-line block `if _, present := obj[timingsPerTokenKey]; present { return raw, false }` | `TestInjectTimingsPerTokenLeavesAClientSetTrueAlone` | `injected = true for a body that already carries the key` |

- [ ] **Step 6: Prove tests 4-6 are load-bearing (one mutation at a time)**

Same procedure, same full-package command.

| # | Production mutation (one edit) | Must fail | Failure text |
|---|---|---|---|
| 4 | Replace the presence check with a value check: `if v, _ := obj[timingsPerTokenKey].(bool); v { return raw, false }` | `TestInjectTimingsPerTokenHonoursAnExplicitClientFalse`, and **only** that test | `timings_per_token = true, want the client's false to survive` |
| 5 | In the decode-error branch, replace `return raw, false` with `obj = map[string]any{}` | `TestInjectTimingsPerTokenLeavesNonObjectBodiesAlone` | `body "[\"a\",\"b\"]": injected=true out={"timings_per_token":true}, want the body unchanged` |
| 6 | Delete the three-line block `if obj == nil { return raw, false }` | `TestInjectTimingsPerTokenDoesNotPanicOnANullBody` | `panic: assignment to entry in nil map`, followed by a goroutine trace and `FAIL	op-ai-gateway/internal/gateway`. A panic aborts the test binary, so the package's remaining tests do not report — that is expected for this row only. |

Mutation 4 is the point of the D5 case: mutation 3 breaks tests 3 and 4 together,
mutation 4 breaks only test 4, which is what proves the helper checks presence
rather than truth. After the last row, re-run step 4's command once and confirm the
package is `ok` again before committing.

- [ ] **Step 7: Format and lint (what CI runs, and the repo's pre-commit hook)**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && golangci-lint fmt --diff && golangci-lint run
```
Expected: `golangci-lint fmt --diff` prints nothing (any output is a gofumpt/goimports
diff — apply it with `make fmt` from the repository root), and `golangci-lint run`
exits 0 with no finding naming `native_passthrough.go` or
`native_passthrough_timings_inject_test.go`. `go test` does not cover these two
gates; CI runs them per module.

- [ ] **Final step: Commit**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && \
git add gateway/backend/internal/gateway/native_passthrough.go \
        gateway/backend/internal/gateway/native_passthrough_timings_inject_test.go && \
git commit -F - <<'MSG'
feat: Add a pure timings_per_token injection helper for native passthrough

injectTimingsPerToken decodes the outgoing body, adds
"timings_per_token": true and re-marshals, returning a fresh slice plus
whether it injected. Nothing calls it yet; the gate and the call site are
the next step of issue #81 part 2.

It is deliberately separate from rewriteModelField rather than folded into
it. That helper returns the original slice from three no-op branches, and
one of them - a provider model that already equals the body's model - is an
ordinary configuration the portal's auto-sync produces, so an injection
placed below it would silently never fire for those mappings.

Presence, not value, is the test: a body that already carries the key is
forwarded unchanged, so a client's explicit false survives. A body that is
not a JSON object is returned unchanged, and so is a bare null, which
decodes into a nil map without an error and would panic on assignment -
the guard that keeps such a body off this path lives in another file.

UseNumber mirrors rewriteModelField, so a large integer literal is not
re-serialized through float64.
MSG
```

---

**For the author of the task that wires the call site:**

- `injectTimingsPerToken` returns **the input slice itself** in all three no-op
  branches. That is safe only because nothing downstream mutates the forwarded
  body; do not add an in-place edit anywhere below it.
- Chaining it after `rewriteModelField` means a flagged request whose model rewrite
  also fires decodes and re-marshals the body **twice**. `readRawJSONUnlimited`
  applies no size cap on this path (these bodies carry base64 image data), so that
  is a real peak-memory cost, not a rounding error. The design asks for two separate
  steps anyway; if you want one decode, that is a design change, not an
  implementation detail.
- The boolean is what the debug-log field (spec D9) should be stamped from. It is
  false when a client already sent the key, which is precisely the case the operator
  needs to see when the panel stays blank with the switch on.

---

### Task 4: Wire the gate and the injection into the passthrough path

Every path below is relative to the worktree root
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection`.
The Go module root is `gateway/backend` (module `op-ai-gateway`), so package
`gateway` is `op-ai-gateway/internal/gateway`.

**This task depends on Task 2 and Task 3 having landed.** Step 3 calls both of
their functions; nothing before step 3 does, so steps 1-2 will build and run
either way.

**Files:**

- Modify: `gateway/backend/internal/gateway/native_passthrough.go` — inside
  `proxyNative`: the two-line injection step added directly below the
  `upstreamBody := rewriteModelField(...)` statement (that statement is line 303
  today), and the `"timings_per_token_injected"` field on the
  `slog.Debug("inference request (native passthrough)", …)` call (line 343
  today). Also two comments in the same file: `proxyNative`'s own doc comment
  (its last sentence, "The only body edit is …") and the long comment block
  immediately above `rewriteModelField`'s call (lines 282-302 today).
- Create: `gateway/backend/internal/gateway/passthrough_timings_injection_test.go`
- Modify (test, comment + failure-message text only, assertions unchanged):
  `gateway/backend/internal/gateway/passthrough_progress_test.go` — the doc
  comment above `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly`
  and both `t.Fatalf("relayed body grew a timings_per_token flag the client
  never sent: …")` calls, one in that test and one at the end of the subtest body
  in `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves`.

**Interfaces:**

- Consumes (Task 2, package `gateway`, `responses_live_timings.go`):
  `func wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool`
- Consumes (Task 3, package `gateway`, `native_passthrough.go`):
  `func injectTimingsPerToken(raw []byte) ([]byte, bool)` and
  `const timingsPerTokenKey = "timings_per_token"`
- Consumes (already on the branch, package `gateway`, `agent_telemetry_log_test.go`):
  `func withCapturedSlog(t *testing.T) (*logbuffer.Buffer, func())` — swaps the
  process-global `slog` default for a Debug-level `logbuffer.Buffer` and returns
  a restore func. Its `Record` carries `Msg string` and `Attrs map[string]any`
  with values un-coerced, so a `bool` attr reads back as a `bool`.
- Consumes (already on the branch, package `gateway`, `server_test.go`):
  `type recordingProxyProvider struct { respBody string; proxyCalls int; gotPath string; gotBody []byte; gotModel string; onProxy func() }` — implements
  `provider.Client` + `StreamingClient` + `NativeProxyClient`, records the body
  it was handed.
- Produces, in package `gateway` (test-only; no exported production surface changes):
  ```go
  type liveTimingsSeed struct {
      appType             string
      appLiveTimings      bool
      spec                *routing.RuntimeSpec
      liveProgressVerdict string
  }
  func newLiveTimingsTestServer(t *testing.T, prov provider.Client, seed liveTimingsSeed) *Server
  func llamaCppOptedIn() liveTimingsSeed
  func serverAgentSpecOptedIn() liveTimingsSeed
  func postPassthrough(t *testing.T, srv *Server, prov *recordingProxyProvider, path, body string) string
  ```
  Later tasks that need a target with the opt-in resolved ON should build their
  fixture from `newLiveTimingsTestServer` rather than seeding a fourth one.
- Produces, on the wire to operators: the per-request debug line
  `"inference request (native passthrough)"` gains the attribute
  **`timings_per_token_injected`** (`bool`), emitted on **every** native
  passthrough request, `true` only when this request's outgoing body actually
  grew the key.

**Set 4 members 4-6 (the three canonical documents): LEFT TO TASK 8, the
documentation task.** Not to this one. The reason is not tidiness — it is that
each of those three sentences shares a paragraph with a sentence a *different*
task falsifies, so any split leaves one author editing around another's
half-edit:

- `api-surface.md`'s bullet ends "This is consistent with, and does not weaken,
  the standing rule that `timings_per_token` is read when the client set it and
  never injected" — that closing sentence is a **Set 1** member (the injection
  prohibition), Task 8's.
- `agent-runtime-manager.md`'s paragraph also carries "a PUT that sets it `true`
  on a spec whose effective type … is not `llama_cpp`/`vllm` is refused" (**D6**,
  Task 1's capable-set narrowing) **and** "a visible toggle that did nothing
  would be worse than the blank cell it promises to fix" (**D10**, the portal
  task's). Three owners, one paragraph.
- `data-model.md`'s three sites are two table rows and a migration row that cite
  each other ("for the same reason as the `applications` column above"), so they
  only stay coherent if rewritten together.

Task 8 therefore gets: the five-site enumeration in the PROBLEM section above,
the instruction to keep "nothing retries without it" (D3 keeps it true), and the
instruction to **retire** "the injection, the gate and the retry are part 2 of
issue #81" wherever it appears, because part 2 ships no retry and D3 records why
it never will.

**Set 1 members 1-2 (the two comments inside `proxyNative`): DONE HERE, in step
9.** Task 3 correctly deferred them — it added an uncalled helper, so nothing it
did contradicted them. This task adds the call three lines below them, and a
comment reading "the gateway does NOT add llama.cpp's `timings_per_token`"
sitting directly above the statement that adds it is not stale documentation but
a self-contradicting diff.

Task 8's Table A row 4 — the doc comment of
`TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly` — is this
task's too, in **step 10a**, for the same reason: step 10 re-scopes the very
assertion that comment describes, so the comment and the assertion have to move
together. Task 8 keeps the remaining **nine** Set 1 sites, which are exactly
rows 3 and 5-12 of its own Table A, and should skip these three.

---

- [ ] **Step 1: Write the test file — the two positive fixtures and the four negatives**

Create `gateway/backend/internal/gateway/passthrough_timings_injection_test.go`
with exactly this content:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"op-ai-gateway/internal/auth"
	"op-ai-gateway/internal/portal"
	"op-ai-gateway/internal/provider"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/store"
	"op-ai-gateway/internal/usage"
	"strings"
	"testing"
	"time"
)

// The tests in this file are the ONLY ones that exercise the opted-IN direction
// of the Responses live-timings switch (issue #81 part 2). That matters more
// than it sounds: the two standing "the relayed body must not grow a
// timings_per_token flag" assertions in passthrough_progress_test.go hold for
// TWO independent reasons, either of which alone would be enough --
// newNativeModeTestServerOn seeds no live-timings flag and no runtime spec, so
// its resolved target has the flag false AND an effective kind of "custom" (its
// application Type is server_agent, and an absent spec detects to "custom").
// The flavor, the stream and the veto conditions all PASS for those fixtures,
// so none of the three is a reason. A gated injection is therefore invisible to
// the entire pre-existing suite, and a positive path could ship completely
// untested with every test green. Those two assertions are kept, re-scoped to
// "flag off => no injection"; these are the other half.
//
// Everything here asserts on the body the provider fake was HANDED, because
// that is the only place the injection is observable: the payload capture
// deliberately keeps recording the CLIENT's bytes (part 2's D7), so the capture
// record cannot be used to see what was sent.

// liveTimingsSeed describes one seeded deployment. Every field is named at each
// call site rather than relying on the zero value, because the whole hazard this
// file exists for is a fixture that LOOKS opted in and resolves to a target that
// is not.
type liveTimingsSeed struct {
	// appType is the application row's kind (routing.Provider*).
	appType string
	// appLiveTimings is the APPLICATION row's own opt-in. For a server_agent
	// application it is deliberately not what the request path reads.
	appLiveTimings bool
	// spec, when non-nil, is upserted against the mapping. For a server_agent
	// application Resolver.targetFrom then reads the endpoint modes, the API
	// flavors AND the live-timings flag off it, and derives the effective
	// upstream kind from its Type.
	spec *routing.RuntimeSpec
	// liveProgressVerdict, when non-empty, is written as the mapping's
	// "live_progress" capability row (routing.CapabilityYes / CapabilityNo).
	// routing.LiveProgressSupportFromVerdict translates it into the Target's own
	// "" / "supported" / "unsupported" vocabulary on the way through, which is
	// why the seed speaks the ROW's vocabulary and the gate speaks the TARGET's.
	liveProgressVerdict string
}

// newLiveTimingsTestServer seeds one server + application + mapping (gateway
// model "gw-model" -> upstream "upstream-model"), optionally a runtime spec and
// a live_progress capability row, and returns a Server wired to prov.
//
// Both API flavors and both endpoint modes are on passthrough so that ONE seed
// serves the /v1/responses positive and the /v1/messages negative: the gate, not
// the route, has to be what refuses the Anthropic endpoint. A fixture that
// 404'd instead would make that negative pass for the wrong reason, which is
// what postPassthrough's proxyCalls check below exists to catch.
func newLiveTimingsTestServer(t *testing.T, prov provider.Client, seed liveTimingsSeed) *Server {
	t.Helper()
	tokens := auth.NewTokenStore()
	directory := portal.NewMemoryDirectory(tokens)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	directory.AddUser(store.User{ID: "usr_dev", Email: "dev@example.test", DisplayName: "Dev User", Role: "admin", Status: store.UserStatusActive, PreferredLanguage: "de", CreatedAt: now, UpdatedAt: now})
	if err := directory.CreatePlainToken(context.Background(), store.TokenRecord{ID: "tok_dev", UserID: "usr_dev", Name: "Dev Token", Status: store.TokenStatusActive, Scopes: `["gateway:use","admin"]`, CreatedAt: now, UpdatedAt: now}, "dev-secret"); err != nil {
		t.Fatalf("CreatePlainToken: %v", err)
	}
	recorder := usage.NewRecorder()
	routeStore := routing.NewMemoryStore()
	ctx := context.Background()
	if err := routeStore.CreateAIServer(ctx, routing.AIServer{ID: "srv-lt", Name: "Live Timings Upstream", Domain: "lt.example.test", Provider: routing.ProviderLlamaCPP, Endpoint: "http://lt.example.test:8000", Status: routing.ServerStatusActive, HealthStatus: routing.HealthHealthy, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateAIServer: %v", err)
	}
	if err := routeStore.CreateApplication(ctx, routing.Application{ID: "app-lt", ServerID: "srv-lt", Type: seed.appType, Port: 8000, Scheme: "http", APIFlavors: []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic}, Priority: 10, Weight: 50, TimeoutMS: 30000, Status: routing.ServerStatusActive, ResponsesMode: routing.EndpointModePassthrough, MessagesMode: routing.EndpointModePassthrough, ResponsesLiveTimingsEnabled: seed.appLiveTimings, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateApplication: %v", err)
	}
	if err := routeStore.CreateMapping(ctx, routing.ModelMapping{ID: "route-lt", ApplicationID: "app-lt", GatewayModelName: "gw-model", AppModelName: "upstream-model", Status: routing.ServerStatusActive, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("CreateMapping: %v", err)
	}
	if seed.spec != nil {
		spec := *seed.spec
		spec.ID, spec.MappingID, spec.CreatedAt, spec.UpdatedAt = "spec-lt", "route-lt", now, now
		if err := routeStore.UpsertRuntimeSpec(ctx, spec); err != nil {
			t.Fatalf("UpsertRuntimeSpec: %v", err)
		}
	}
	if seed.liveProgressVerdict != "" {
		if err := routeStore.UpsertMappingCapabilities(ctx, "route-lt", []routing.CapabilityRow{{Capability: routing.CapabilityLiveProgress, Verdict: seed.liveProgressVerdict, Source: routing.CapabilitySourceLlamaCppProps, CheckedAt: now}}); err != nil {
			t.Fatalf("UpsertMappingCapabilities: %v", err)
		}
	}
	if err := routeStore.UpsertTelemetry(ctx, routing.ServerTelemetry{ServerID: "srv-lt", ReportedAt: now, LatencyMS: 100, ProviderHealth: `{}`, Capabilities: `{}`, RawSummary: `{}`, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertTelemetry: %v", err)
	}
	return New(ServerDeps{
		Tokens:   tokens,
		Usage:    recorder,
		Provider: prov,
		Routes:   routeStore,
		Portal:   portal.NewService(portal.ServiceDeps{Users: directory, Tokens: directory, Usage: recorder, Routes: routeStore, Clock: func() time.Time { return now }, ModelLister: provider.NewMock()}),
	})
}

// llamaCppOptedIn is the ORDINARY-application positive: a plain llama_cpp
// application whose own row carries the opt-in and which has no runtime spec at
// all, so targetFrom never enters its server_agent branch and the application's
// own value is what reaches the Target.
func llamaCppOptedIn() liveTimingsSeed {
	return liveTimingsSeed{appType: routing.ProviderLlamaCPP, appLiveTimings: true}
}

// serverAgentSpecOptedIn is the second positive, and the design insists on it
// separately because BOTH halves of the spec are load-bearing and each fails
// silently on its own:
//
//   - Without spec.ResponsesLiveTimingsEnabled, targetFrom's spec-over-
//     application precedence resolves the flag back to false the moment a spec
//     row exists at all -- a spec is not a partial override.
//   - Without spec.Type, the effective kind is detected from the spec's empty
//     Binary and comes out "custom", because Target.Provider for this
//     application is the literal "server_agent" and says nothing about what
//     actually serves.
//
// appLiveTimings is false ON PURPOSE: with the application row saying no and the
// spec row saying yes, an injection that happens proves the request path read
// the SPEC.
func serverAgentSpecOptedIn() liveTimingsSeed {
	return liveTimingsSeed{
		appType:        routing.ProviderServerAgent,
		appLiveTimings: false,
		spec: &routing.RuntimeSpec{
			Enabled:                     true,
			Type:                        string(routing.RuntimeSpecTypeLlamaCpp),
			APIFlavors:                  []string{routing.APIFlavorOpenAI, routing.APIFlavorAnthropic},
			ResponsesMode:               routing.EndpointModePassthrough,
			MessagesMode:                routing.EndpointModePassthrough,
			ResponsesLiveTimingsEnabled: true,
		},
	}
}

// terminalOnlyResponsesStream is a one-frame upstream body: enough for the
// passthrough copy + usage scan to complete normally. These tests are about the
// REQUEST, so the response deliberately carries nothing interesting.
const terminalOnlyResponsesStream = "event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_x","usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}}}` + "\n\n"

// liveTimingsStreamBody is the plain streaming Responses request every positive
// uses: a client that set no timings_per_token of its own.
const liveTimingsStreamBody = `{"model":"gw-model","stream":true,"input":"hi"}`

// postPassthrough drives one request through the whole server and returns the
// body the provider fake was handed.
//
// The proxyCalls check is not decoration. Three of the four negatives assert the
// ABSENCE of a key, and a fixture that failed to route -- a 404 from a disabled
// endpoint, a resolve failure -- would satisfy that assertion with an empty
// body. Requiring exactly one ProxyNative call makes every negative prove the
// request actually reached proxyNative and the GATE is what refused.
func postPassthrough(t *testing.T, srv *Server, prov *recordingProxyProvider, path, body string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer dev-secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST %s: status = %d, body = %s", path, rec.Code, rec.Body.String())
	}
	if prov.proxyCalls != 1 {
		t.Fatalf("POST %s: ProxyNative calls = %d, want exactly 1 (the request must have reached the native passthrough path, or an absence assertion below proves nothing)", path, prov.proxyCalls)
	}
	return string(prov.gotBody)
}

// TestPassthroughResponsesInjectsTimingsForAnOptedInLlamaCppApplication is the
// ordinary case the feature exists for: an operator switched the per-application
// opt-in on for a llama.cpp upstream, and a streaming /v1/responses request
// therefore reaches that upstream carrying timings_per_token.
func TestPassthroughResponsesInjectsTimingsForAnOptedInLlamaCppApplication(t *testing.T) {
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, llamaCppOptedIn())

	got := postPassthrough(t, srv, prov, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(got, `"timings_per_token":true`) {
		t.Fatalf("relayed body carries no timings_per_token flag: %s", got)
	}
	// The two body edits COMPOSE, in that order: the injection runs over
	// rewriteModelField's OUTPUT. A wiring that built the outgoing body from the
	// client's raw bytes instead of chaining loses the mapped model name here,
	// loudly, rather than sending the gateway-side name upstream in silence.
	if !strings.Contains(got, `"model":"upstream-model"`) {
		t.Fatalf("relayed body lost the mapped provider model: %s", got)
	}
	if !strings.Contains(got, `"input":"hi"`) {
		t.Fatalf("relayed body lost a field the client sent: %s", got)
	}
}

// TestPassthroughResponsesInjectsTimingsForAnOptedInServerAgentSpec is the same
// request against a server_agent application whose OWN row has the opt-in off
// and whose runtime spec has it on, with an explicit llama_cpp spec type. It
// fails for a gate that asks Target.Provider about the kind (that field is the
// literal "server_agent") and for a resolution that reads the application's flag
// instead of the spec's.
func TestPassthroughResponsesInjectsTimingsForAnOptedInServerAgentSpec(t *testing.T) {
	prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
	srv := newLiveTimingsTestServer(t, prov, serverAgentSpecOptedIn())

	got := postPassthrough(t, srv, prov, "/v1/responses", liveTimingsStreamBody)

	if !strings.Contains(got, `"timings_per_token":true`) {
		t.Fatalf("relayed body carries no timings_per_token flag: %s", got)
	}
	if !strings.Contains(got, `"model":"upstream-model"`) {
		t.Fatalf("relayed body lost the mapped provider model: %s", got)
	}
}

// TestPassthroughResponsesDoesNotInjectTimingsWhenTheGateRefuses walks the four
// refusals that are invisible everywhere else in the suite. Each runs on a seed
// whose opt-in IS on, so "no injection" here is the gate's doing and not the
// fixture's -- which is exactly what separates these from the two standing
// assertions in passthrough_progress_test.go, whose fixture has the opt-in off.
//
// Two of the four are about the CALL SITE rather than the predicate, and are the
// only tests in the repository that can catch a wrong argument there: the
// predicate takes the FINE api flavor and the stream flag as parameters, and a
// proxyNative that passed a hardcoded "openai_responses", or the literal true
// for stream, leaves TestWantsResponsesLiveTimings entirely green.
func TestPassthroughResponsesDoesNotInjectTimingsWhenTheGateRefuses(t *testing.T) {
	recordedRejection := llamaCppOptedIn()
	recordedRejection.liveProgressVerdict = routing.CapabilityNo

	for _, tc := range []struct {
		name string
		seed liveTimingsSeed
		path string
		body string
		// wantRelayedFlag is what the relayed body must say about the key:
		// "" means the key must not appear AT ALL; a non-empty value is the
		// exact JSON fragment that must appear. The client-false row spells the
		// VALUE out on purpose -- asserting mere presence would pass against an
		// injection that overwrote the client's false with true, which is the
		// one way this rule regresses silently.
		wantRelayedFlag string
	}{
		{
			name: "the Anthropic Messages endpoint",
			seed: llamaCppOptedIn(),
			path: "/v1/messages",
			body: `{"model":"gw-model","stream":true,"max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "a non-streaming /v1/responses request",
			seed: llamaCppOptedIn(),
			path: "/v1/responses",
			body: `{"model":"gw-model","input":"hi"}`,
		},
		{
			name: "a recorded live-progress rejection vetoes the opt-in",
			seed: recordedRejection,
			path: "/v1/responses",
			body: liveTimingsStreamBody,
		},
		{
			name:            "a client's own explicit false survives",
			seed:            llamaCppOptedIn(),
			path:            "/v1/responses",
			body:            `{"model":"gw-model","stream":true,"input":"hi","timings_per_token":false}`,
			wantRelayedFlag: `"timings_per_token":false`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
			srv := newLiveTimingsTestServer(t, prov, tc.seed)

			got := postPassthrough(t, srv, prov, tc.path, tc.body)

			if tc.wantRelayedFlag == "" {
				if strings.Contains(got, "timings_per_token") {
					t.Fatalf("relayed body grew a timings_per_token flag the gate had to refuse: %s", got)
				}
				return
			}
			if !strings.Contains(got, tc.wantRelayedFlag) {
				t.Fatalf("relayed body does not carry %s: %s", tc.wantRelayedFlag, got)
			}
		})
	}
}
```

Note what is NOT asserted here: nothing about the running-connections row. The
chain from "the outgoing body carries the flag" to "the panel shows an
upstream-labelled rate" is identical whether the client set the key or the
gateway added it, and it is already pinned end to end by
`TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`
(`passthrough_progress_test.go`). Re-asserting it against a hand-written fake
that ignores the request body would add no production coverage.

- [ ] **Step 2: Run the full package and read the result carefully**

Run (full package, no `-run` filter; `internal/gateway` takes roughly 95 s):
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```

Expected: the package **builds and runs** — this is not a build failure, because
nothing in step 1 names `wantsResponsesLiveTimings` or `injectTimingsPerToken` —
and exactly **two** tests fail, with the relayed body printed as
`json.Marshal` writes a `map[string]any` (keys in sorted order):

```
--- FAIL: TestPassthroughResponsesInjectsTimingsForAnOptedInLlamaCppApplication (0.00s)
    passthrough_timings_injection_test.go:NNN: relayed body carries no timings_per_token flag: {"input":"hi","model":"upstream-model","stream":true}
--- FAIL: TestPassthroughResponsesInjectsTimingsForAnOptedInServerAgentSpec (0.00s)
    passthrough_timings_injection_test.go:NNN: relayed body carries no timings_per_token flag: {"input":"hi","model":"upstream-model","stream":true}
FAIL
FAIL	op-ai-gateway/internal/gateway	95.xxxs
```

**All four subtests of `TestPassthroughResponsesDoesNotInjectTimingsWhenTheGateRefuses`
PASS right now, and that is correct, not a defect in them.** They assert the
status quo — nothing injects yet — so they cannot go red before the injection
exists. What makes them load-bearing is step 11's mutation table: each has a
named production mutation that reds it and nothing else in this file. If any of
the four FAILS here, stop: it means the fixture is not routing (look at the
`ProxyNative calls = 0` message) rather than that the gate is working.

- [ ] **Step 3: Wire the gate and the injection into `proxyNative`**

In `gateway/backend/internal/gateway/native_passthrough.go`, find the statement

```go
	upstreamBody := rewriteModelField(raw, target.ProviderModel)
```

(line 303 today) and insert directly below it:

```go

	// The operator's opt-in, applied as a SECOND, separate edit rather than
	// folded into the rewrite above. rewriteModelField returns the client's own
	// slice unchanged from three no-op branches, and one of them -- a provider
	// model that already equals the body's model -- is an ordinary
	// configuration the portal's application auto-sync produces, so an
	// injection placed inside that helper would silently never fire for those
	// mappings.
	//
	// The argument order matters: the injection runs over the REWRITTEN body,
	// so a request that needs both edits gets both. Both helpers return a fresh
	// slice when they change anything and the original otherwise, so `raw` --
	// still read further down to build the payload capture, and handed to the
	// HTTP transport as `upstreamBody` while the request is in flight -- is
	// never written to.
	if wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream) {
		upstreamBody, _ = injectTimingsPerToken(upstreamBody)
	}
```

`pfReq.APIFlavor` is the **fine** flavor here (`"openai_responses"` /
`"anthropic_messages"`): `inferencePreflight` builds `pf.Req` with
`APIFlavor: shape.apiFlavor`. `proxyNative` itself normalizes that field a few lines
above to pick `nativeEndpoint`, which is what makes the fine value visible in
the first place. Do not substitute `target.APIFlavor`, which carries the coarse
value.

The `_` is temporary and becomes the D9 field in step 7; leave it for now so that
step is its own change rather than riding this one.

- [ ] **Step 4: Run the full package and watch both positives go green**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```
Expected:
```
ok  	op-ai-gateway/internal/gateway	95.xxxs
```
Every test in the package, including both standing "the relayed body must not
grow a timings_per_token flag" assertions, is green: their fixture's resolved
target has the opt-in off, so the gate refuses it.

- [ ] **Step 5: Write the failing test for the debug-log field (D9)**

Append to `gateway/backend/internal/gateway/passthrough_timings_injection_test.go`:

```go

// TestPassthroughNativeDebugLineRecordsTheInjection pins spec D9, whose whole
// purpose is to close a gap the other deferrals leave open together. The
// payload capture deliberately keeps recording the CLIENT's bytes, so an
// operator opening a 400 from a flagged request sees a body that would NOT have
// earned that 400, with nothing anywhere saying the gateway added a key. The
// per-request debug line is that "anywhere".
//
// The field is asserted in BOTH directions on purpose. A field emitted only when
// true is indistinguishable, to an operator grepping a log, from a build that
// never had the field -- and the false cases are the ones they are actually
// debugging: "the switch is on and the panel is still blank, did the gateway
// ask?". The client-already-sent-it row is the sharpest of those, because the
// gate said yes and the key still was not added.
func TestPassthroughNativeDebugLineRecordsTheInjection(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed liveTimingsSeed
		body string
		want bool
	}{
		{
			name: "the gate allowed and the key was added",
			seed: llamaCppOptedIn(),
			body: liveTimingsStreamBody,
			want: true,
		},
		{
			name: "the client had already sent the key, so nothing was added",
			seed: llamaCppOptedIn(),
			body: `{"model":"gw-model","stream":true,"input":"hi","timings_per_token":false}`,
			want: false,
		},
		{
			name: "the operator never switched it on",
			seed: liveTimingsSeed{appType: routing.ProviderLlamaCPP},
			body: liveTimingsStreamBody,
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf, restore := withCapturedSlog(t)
			defer restore()
			prov := &recordingProxyProvider{respBody: terminalOnlyResponsesStream}
			srv := newLiveTimingsTestServer(t, prov, tc.seed)

			postPassthrough(t, srv, prov, "/v1/responses", tc.body)

			found := false
			for _, rec := range buf.Snapshot() {
				if rec.Msg != "inference request (native passthrough)" {
					continue
				}
				found = true
				got, ok := rec.Attrs["timings_per_token_injected"]
				if !ok {
					t.Fatalf("the per-request debug line carries no timings_per_token_injected field: attrs = %+v", rec.Attrs)
				}
				if got != tc.want {
					t.Fatalf("timings_per_token_injected = %#v, want %v", got, tc.want)
				}
			}
			if !found {
				t.Fatalf("no %q debug record was emitted at all", "inference request (native passthrough)")
			}
		})
	}
}
```

`withCapturedSlog` lives in `agent_telemetry_log_test.go` in this package; it
swaps the process-global `slog` default for a Debug-level `logbuffer.Buffer` and
restores it. No new import is needed for it: `buf.Snapshot()`'s element type is
never named here.

- [ ] **Step 6: Run the full package and watch the log test fail — all three rows**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```
Expected — one test, three subtests, the same message in each, because the field
does not exist yet (the attrs map printed after it is the existing eight fields
— `path`, `api_flavor`, `model`, `stream`, `server`, `upstream_path`,
`token_id`, `user_id` — in Go's map-print order, so its text varies run to run):

```
--- FAIL: TestPassthroughNativeDebugLineRecordsTheInjection (0.01s)
    --- FAIL: TestPassthroughNativeDebugLineRecordsTheInjection/the_gate_allowed_and_the_key_was_added (0.00s)
        passthrough_timings_injection_test.go:NNN: the per-request debug line carries no timings_per_token_injected field: attrs = map[api_flavor:openai_responses model:gw-model path:/v1/responses ...]
    --- FAIL: TestPassthroughNativeDebugLineRecordsTheInjection/the_client_had_already_sent_the_key,_so_nothing_was_added (0.00s)
        passthrough_timings_injection_test.go:NNN: the per-request debug line carries no timings_per_token_injected field: attrs = map[...]
    --- FAIL: TestPassthroughNativeDebugLineRecordsTheInjection/the_operator_never_switched_it_on (0.00s)
        passthrough_timings_injection_test.go:NNN: the per-request debug line carries no timings_per_token_injected field: attrs = map[...]
FAIL
FAIL	op-ai-gateway/internal/gateway	95.xxxs
```

This test is NOT vacuous before the change: a naive "assert the value is false"
version would have read a missing key as `nil`, which compares unequal to both
`true` and `false`, so it would still have failed — but it would also have
passed forever against a field that was dropped from the log line later. The
comma-ok read is what pins the field's existence separately from its value.

- [ ] **Step 7: Record the injection on the debug line (D9)**

Two edits in `gateway/backend/internal/gateway/native_passthrough.go`, which
cannot be separated because the first does not compile without the second.

First, in the block added in step 3, replace

```go
	if wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream) {
		upstreamBody, _ = injectTimingsPerToken(upstreamBody)
	}
```

with

```go
	// injectedLiveTimings is false for a request the gate refused AND for one
	// whose client already sent the key -- two different reasons for a panel
	// cell that stays blank with the switch on, which is why it is recorded
	// from the helper's own answer rather than inferred from the gate.
	injectedLiveTimings := false
	if wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream) {
		upstreamBody, injectedLiveTimings = injectTimingsPerToken(upstreamBody)
	}
```

Second, find the `slog.Debug` call whose message is
`"inference request (native passthrough)"` (line 343 today) and append one
key/value pair at the end of its argument list, leaving every existing pair
untouched:

```go
	slog.Debug("inference request (native passthrough)", "path", r.URL.Path, "api_flavor", pfReq.APIFlavor, "model", pfReq.Model, "stream", pfReq.Stream, "server", serverName, "upstream_path", path, "token_id", token.ID, "user_id", token.UserID, "timings_per_token_injected", injectedLiveTimings)
```

Emitted on every native passthrough request, both values, deliberately: the
operator's one grep is `timings_per_token_injected=true`, and a field that is
absent when false cannot tell them whether the build has it at all.

- [ ] **Step 8: Run the full package and watch it go green**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/
```
Expected:
```
ok  	op-ai-gateway/internal/gateway	95.xxxs
```

- [ ] **Step 9: Correct the two comments inside `proxyNative` that this commit falsifies**

Repo-facing text is English. Cite **issue #81** only — never the branch-local
design document's path, which is deleted before the pull request. Refer to
functions by name; do not write a line number into the file.

**9a.** In `proxyNative`'s doc comment (lines 246-251 today), replace the last
sentence:

```go
// machinery. The only body edit is rewriting the `model` field to the upstream's
// mapped name (lossless; all other fields untouched).
```

with:

```go
// machinery. Exactly two body edits are possible, both value-lossless and both
// described at the body-building step below: the `model` field is rewritten to
// the upstream's mapped name, and -- only where the operator switched it on for
// a capable upstream -- llama.cpp's `timings_per_token` is added. Every other
// field reaches the upstream as the client wrote it.
```

**9b.** Replace the whole comment block directly above the
`upstreamBody := rewriteModelField(...)` statement (lines 282-302 today, from
`// The ONLY edit made to a relayed body, ever.` down to and including
`// request is not.`) with:

```go
	// The only two edits ever made to a relayed body. Passthrough still means
	// the client's bytes reach the upstream as the client wrote them, apart
	// from the mapped model name and -- under the operator's opt-in below --
	// llama.cpp's `timings_per_token`.
	//
	// That flag is what makes llama.cpp attach a top-level `timings` object to
	// PARTIAL frames, and such an object is the only thing that can put a live
	// tokens/sec figure on an /v1/responses passthrough row while generation is
	// still running. That its Responses implementation does attach one is
	// MEASURED rather than carried over from its chat streams: one flagged
	// 48-frame stream carried `timings` on 39 of its frames while the same
	// prompt replayed WITHOUT the flag carried exactly one, the terminal
	// `response.completed` (usageScanner's doc comment in
	// passthrough_usage_scan.go records the measurement and its scope caveat).
	// Note "while still running": that terminal frame carries its own `timings`
	// with or without the flag, so a rate does arrive at the END regardless --
	// the flag buys the mid-stream figure and nothing else (see the per-flavor
	// table under "Native passthrough is on this panel too" in
	// docs/architecture/cross-cutting/telemetry-usage-observability.md §8.4.3).
	//
	// Adding it is not the silent rewriting of a client request that this path
	// refuses to do, because every part of it is the operator's own decision and
	// none of it overrides the client's. wantsResponsesLiveTimings requires the
	// per-endpoint opt-in, a llama.cpp upstream, the Responses flavor and a
	// streaming request, and lets a recorded live-progress rejection veto the
	// lot; injectTimingsPerToken then tests for the key's PRESENCE rather than
	// its value, so a client that sent `timings_per_token: false` keeps it --
	// llama.cpp treats an explicit false exactly as it treats an absent key.
	// What the client does pay is fields it did not ask for on frames it has to
	// parse, and that cost is measured rather than guessed: in a separate
	// flagged/unflagged pair of the same prompt, both terminating at the same
	// `output_tokens`, the flagged run carried 2.49x the wire bytes. The
	// operator accepted that by switching this on, which is the whole reason
	// this is an operator's switch and not a default. No retry accompanies the
	// injection: the endpoint was measured accepting unknown top-level keys, so
	// a retry's trigger could not be exercised against any upstream this
	// repository can point at (issue #81).
	//
	// The payload capture keeps recording the CLIENT's bytes, so the debug line
	// below is where an operator debugging a 400 learns that the gateway added
	// a key at all.
```

- [ ] **Step 10: Re-scope the two standing tripwire assertions**

Both stay. Their condition is unchanged; only the comment and the failure text
change, so that a future reader knows what they now pin.

**10a.** In `gateway/backend/internal/gateway/passthrough_progress_test.go`, in
the doc comment above `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly`,
replace:

```go
// It also pins the non-injection rule from the other side: the relayed body must
// not have grown a `timings_per_token` flag the client never sent, which is the
// one edit that would turn this row's two em-dashes into numbers.
```

with:

```go
// It also pins the GATE from the other side. This fixture's resolved target has
// the operator's live-timings opt-in off — newNativeModeTestServerOn seeds no
// such flag and no runtime spec — so the relayed body must not have grown a
// `timings_per_token` flag: an UNGATED injection, one that dropped
// wantsResponsesLiveTimings from proxyNative's body-building step, is exactly
// the edit that would turn this row's two em-dashes into numbers, and this
// assertion is what catches it. The opted-IN direction is a different fixture
// and lives in passthrough_timings_injection_test.go.
```

**10b.** In the same file, in that test, replace the failure message:

```go
		t.Fatalf("relayed body grew a timings_per_token flag the client never sent: %s", prov.gotBody)
```

with:

```go
		t.Fatalf("relayed body grew a timings_per_token flag: the client sent none and this fixture's operator opt-in is off, so nothing may add one: %s", prov.gotBody)
```

**10c.** In the same file, in the subtest body of
`TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves`, the
second occurrence of that same `t.Fatalf` line: replace it with the identical
new text, and put one comment line above the `if strings.Contains(...)` that
guards it:

```go
			// The second copy of the same tripwire, on the terminal-frame shape:
			// this fixture's opt-in is off too, so an ungated injection reddens
			// both rather than only the partial-frames test above.
```

There are exactly **two** such assertions in the repository, and this grep is
what says so — it returns exactly these two lines and nothing else:

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && \
grep -rn 'strings.Contains(string(prov.gotBody), "timings_per_token")' gateway/backend/internal/gateway/
```

A bare `grep -rn "timings_per_token"` over the same directory returns many more
lines than that — production files and test files alike, and more again once
step 1's new file exists. None of them is another instance of the assertion
being re-scoped here: they are prose, request bodies, step 1's own opted-in
assertions, or the POSITIVE assertion in
`TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate` that a
client's own `"timings_per_token":true` survives the relay — which stays exactly
as it is. Use the targeted grep above, not the bare one, as the check.

- [ ] **Step 11: Prove the tests are load-bearing — mutations M1-M4**

Apply **one** edit, run, confirm exactly the listed tests go red, then restore
the file before the next row. Never edit a test file to produce a failure. For
these probes only, `-run` filters are allowed; the verdict runs are steps 4, 8
and 13, and all three run whole packages.

Run each time:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && go test ./internal/gateway/ -run 'TestPassthroughResponses|TestPassthroughNativeDebugLine'
```

| # | The ONE production edit | Tests that must go red | Failure text |
|---|---|---|---|
| M1 | In `native_passthrough.go`'s `proxyNative`, change `if wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream) {` to `if false {` | `…InjectsTimingsForAnOptedInLlamaCppApplication`, `…InjectsTimingsForAnOptedInServerAgentSpec`, and `…DebugLineRecordsTheInjection/the_gate_allowed_and_the_key_was_added` | `relayed body carries no timings_per_token flag: {"input":"hi","model":"upstream-model","stream":true}` (twice) and `timings_per_token_injected = false, want true` |
| M2 | Same call, replace the second argument: `wantsResponsesLiveTimings(target, "openai_responses", pfReq.Stream)` | `…DoesNotInjectTimingsWhenTheGateRefuses/the_Anthropic_Messages_endpoint`, and of the tests this filter runs, only it | `relayed body grew a timings_per_token flag the gate had to refuse: {"max_tokens":32,"messages":[{"content":"hi","role":"user"}],"model":"upstream-model","stream":true,"timings_per_token":true}` |
| M3 | Same call, replace the third argument: `wantsResponsesLiveTimings(target, pfReq.APIFlavor, true)` | `…DoesNotInjectTimingsWhenTheGateRefuses/a_non-streaming_/v1/responses_request`, and of the tests this filter runs, only it | `relayed body grew a timings_per_token flag the gate had to refuse: {"input":"hi","model":"upstream-model","timings_per_token":true}` |
| M4 | In `responses_live_timings.go`'s `wantsResponsesLiveTimings`, delete the block `if target.LiveProgressSupport == liveTimingsVerdictUnsupported { return false }` | `…DoesNotInjectTimingsWhenTheGateRefuses/a_recorded_live-progress_rejection_vetoes_the_opt-in` | `relayed body grew a timings_per_token flag the gate had to refuse: {"input":"hi","model":"upstream-model","stream":true,"timings_per_token":true}` |

M2 and M3 are the two that no other test in the repository can catch:
`TestWantsResponsesLiveTimings` builds its arguments itself, so a call site that
supplies the wrong ones leaves it green.

M4 under the filter above reds one subtest. Under the **whole** package it also
reds `TestWantsResponsesLiveTimings`'s row *a recorded rejection vetoes the
opt-in* (Task 2's own M6), which is expected: what this row adds over that one is
that the verdict really travels from a stored capability row through
`MemoryStore.ActiveMappingsForModel`'s join,
`routing.LiveProgressSupportFromVerdict` and `targetFrom` to
`Target.LiveProgressSupport`. The mutation that proves *that* half specifically
is deleting `LiveProgressSupport:  c.LiveProgressSupport,` from `targetFrom`'s
returned `Target` literal in `internal/routing/resolver.go`; it reds this same
subtest and, in `internal/routing`, `TestTargetCarriesLiveProgressVerdictFromCapabilityRow`
in `resolver_endpoint_mode_test.go` and the second assertion of
`resolver_affinity_live_progress_test.go`. Run it if you want the plumbing
pinned; it is not required to prove this file's tests.

- [ ] **Step 12: Prove the tests are load-bearing — mutations M5-M8**

Same procedure, one at a time, restoring in between. M8 uses the wider filter
given in its row.

| # | The ONE production edit | Tests that must go red | Failure text |
|---|---|---|---|
| M5 | In `native_passthrough.go`'s `injectTimingsPerToken`, replace the presence check `if _, present := obj[timingsPerTokenKey]; present { return raw, false }` with the value check `if v, _ := obj[timingsPerTokenKey].(bool); v { return raw, false }` | **two**: `…DoesNotInjectTimingsWhenTheGateRefuses/a_client's_own_explicit_false_survives` AND `…DebugLineRecordsTheInjection/the_client_had_already_sent_the_key,_so_nothing_was_added` — the mutated check lets an explicit `false` through, so the helper overwrites it and returns `true` | `relayed body does not carry "timings_per_token":false: {"input":"hi","model":"upstream-model","stream":true,"timings_per_token":true}` and `timings_per_token_injected = true, want false` |
| M6 | In `responses_live_timings.go`, delete the block `if target.Provider == routing.ProviderServerAgent { kind = target.LiveProgressSpecType }` | `…InjectsTimingsForAnOptedInServerAgentSpec`, and of the tests this filter runs, only it (over the whole package it also reds one row of Task 2's `TestWantsResponsesLiveTimings`) | `relayed body carries no timings_per_token flag: {"input":"hi","model":"upstream-model","stream":true}` |
| M7 | In `native_passthrough.go`, rename the attribute KEY on the `slog.Debug("inference request (native passthrough)", …)` call from `"timings_per_token_injected"` to `"timings_per_token_inject"`. Nothing else changes — the value argument, the local and the assignment stay exactly as step 7 wrote them | all three subtests of `…DebugLineRecordsTheInjection` | `the per-request debug line carries no timings_per_token_injected field: attrs = map[…]` |
| M8 | In `native_passthrough.go`, change `if wantsResponsesLiveTimings(target, pfReq.APIFlavor, pfReq.Stream) {` to `if true {` — an UNGATED injection, the regression class the two standing tripwires exist for. Filter: `-run 'TestPassthroughResponsesStreamWithoutClientTimings|TestPassthroughResponsesTerminalUsage'` (Go's `-run` takes an RE2 regexp, so the alternation is a bare `|`) | `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly` and BOTH subtests of `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves` | `relayed body grew a timings_per_token flag: the client sent none and this fixture's operator opt-in is off, so nothing may add one: {"input":"hi","model":"upstream-model","stream":true,"timings_per_token":true}` |

M7 renames the key and nothing else, because the obvious mutation — deleting the
field — would not compile: the log line is `injectedLiveTimings`'s only use, and
an unused local is a compile error in Go, so deleting the field would force the
declaration and the assignment out with it and stop being one edit. The rename
keeps the probe to a single token and reds the same three subtests, since the
test's comma-ok read is on the key's exact spelling. Everything else in
`proxyNative` is untouched.

M8 is the answer to "do the kept tripwires still catch anything?". They do, and
it is the most valuable regression class this feature has: the gate silently
disappearing while the injection stays.

After the last row, restore both files and confirm the tree is back to the
intended state:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && git diff --stat
```
Expected: **exactly two files** — `native_passthrough.go` and
`passthrough_progress_test.go`. `responses_live_timings.go` must NOT appear:
M4 and M6 only borrowed it, it is Task 2's file, and this task must leave it
exactly as Task 2 committed it. `passthrough_timings_injection_test.go` is new,
so it shows as untracked in `git status --short` rather than in this diff.

- [ ] **Step 13: Format, lint, and take the full-package verdict**

Run:
```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection/gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./internal/gateway/
```
Expected: `golangci-lint fmt --diff` prints nothing and exits 0 (gofumpt +
goimports, which CI gates on per Go module and `gofmt`/`go vet` do not cover);
`golangci-lint run` ends with `0 issues.`; then

```
ok  	op-ai-gateway/internal/gateway	95.xxxs
```

Only `internal/gateway` is in the test run because only its files were changed —
if `git diff --stat` from step 12 lists anything under `internal/routing`, a
mutation was not restored.

- [ ] **Final step: Commit**

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection && \
git add gateway/backend/internal/gateway/native_passthrough.go \
        gateway/backend/internal/gateway/passthrough_timings_injection_test.go \
        gateway/backend/internal/gateway/passthrough_progress_test.go && \
git commit -F - <<'MSG'
feat: Ask llama.cpp for mid-stream timings when the operator switched it on

proxyNative now adds "timings_per_token" to a streaming /v1/responses
passthrough body whenever wantsResponsesLiveTimings allows it. That is the
whole of issue #81 part 2's behaviour: the flag makes llama.cpp attach a
top-level timings object to PARTIAL frames, which the scanner already reads,
so the running-connections row shows an upstream-reported tokens/sec for the
request instead of nothing until it ends.

The injection sits beside rewriteModelField at the body-building step, not
inside it. That helper returns the client's own slice unchanged from three
no-op branches, and one of them - a provider model that already equals the
body's model - is an ordinary configuration the portal's auto-sync produces,
so an injection folded in would silently never fire for those mappings. It
runs over the rewrite's output, so a request needing both edits gets both,
and neither helper writes to the client's bytes, which are still read
afterwards to build the payload capture.

The per-request debug line gains timings_per_token_injected, on every native
passthrough request and in both directions. The capture deliberately keeps
recording the client's body, so an operator debugging a 400 from a flagged
request would otherwise see a body that could not have earned it, with
nothing anywhere saying the gateway added a key. The false answers are the
ones they are really debugging - the switch is on and the panel is still
blank - and one of them is a client that sent the key itself, which the
injection leaves alone.

The positive path could have shipped completely untested with every existing
test green: the two standing "the relayed body must not grow a
timings_per_token flag" assertions hold because their fixture's resolved
target has the opt-in OFF, not because of any gate. Both are kept and
re-scoped to that, with a mutation check that an ungated injection still
reddens them. Two new fixtures cover the other direction - an ordinary
llama_cpp application, and a server_agent mapping whose runtime spec carries
BOTH the type and the flag, since spec-over-application precedence resolves a
spec without the flag back to false and Target.Provider for such a target is
the literal "server_agent". Four negatives run on the opted-in fixture:
/v1/messages, a non-streaming request, a stored live-progress rejection, and
a client's own explicit false, that last asserting the VALUE survives. Two of
them are the only tests anywhere that can catch a call site passing the wrong
flavor or stream argument, because the predicate's own table builds its
arguments itself.

Two comments inside proxyNative said the gateway does not do this. They are
rewritten here because this commit is what makes them false and they sit
directly above the code that falsifies them; the remaining sites that state
the same rule belong to the documentation commit.
MSG
```

If this session's system-reminder prescribes attribution lines for commit
messages, append them at the end of the body. Do not commit to or merge into
`main`; this worktree is on branch `responses-timings-injection`, which is where
the commit belongs.

---

**For the authors of the later tasks:**

- The debug attribute is `timings_per_token_injected` (`bool`), on the record
  whose message is exactly `inference request (native passthrough)`. The
  operator-facing documentation that spec D9 asks for — "while this switch is on,
  the recorded request body is not the body that was sent" — is **not** written
  here; it belongs with the Set 2 capture-identity rewrite, which is already the
  documentation task's.
- The fixture helpers `newLiveTimingsTestServer` / `llamaCppOptedIn` /
  `serverAgentSpecOptedIn` / `postPassthrough` are in
  `passthrough_timings_injection_test.go`. Anything later that needs a target
  with the opt-in resolved ON should build on them rather than seed a fourth
  fixture; `newNativeModeTestServerOn`'s application type stays `server_agent`
  with no spec, which its other callers depend on.
- Design hazard, now live and not covered by any test in this commit: the
  operator's switch — not only a client — selects requests into the population
  whose partial frames carry rates. A flagged stream that ends without a
  terminal frame is recorded status "success", so its rate reaches the routing
  EWMA. `TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`
  pins that the recorded figure is the last rate reported and not the
  generation's peak, but only for a CLIENT-set flag. If anyone adds an
  operator-switch equivalent, build it on `newLiveTimingsTestServer` and a
  body-aware provider fake; do not "fix" the recorded rate, which #80 has just
  finished making the terminal frame's own figure.
- `injectTimingsPerToken`'s boolean is now consumed. Do not change it to report
  "the gate allowed" instead of "the key was added": the difference between the
  two is the client's-own-false case, which is the field's most useful answer.

---

### Task 5: Read `timings.predicted_n` for the live view only

**Goal:** llama.cpp's `timings.predicted_n` becomes the running-connections
panel's mid-stream output-token count for `openai_responses` native passthrough,
in a field of its own that can never reach `usage_events`, the Activity totals,
the usage timeseries or the rate limiter.

**Files:**
- Modify: `gateway/backend/internal/inference/types.go` — the `Usage` struct: add one field after `DraftTokens` (currently the last field, around line 117).
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` — function `mergeResponsesUsage` (around line 582): its anonymous `Timings` struct (around lines 588-592) and its `if m.Timings != nil` block (around lines 609-613).
- Modify: `gateway/backend/internal/gateway/passthrough_usage_scan.go` — method `(*usageScanner).publishProgress` (around line 394) and the second bullet of its doc comment (around lines 375-382).
- Modify: `gateway/backend/internal/gateway/active_requests.go` — the doc comment above `activeRequestDTO.OutputTokens` (around lines 200-205). Comment only.
- Test: `gateway/backend/internal/gateway/passthrough_usage_scan_test.go` — add one test (put it after `TestMergeResponsesUsageDraftTokensTakesRunningMax`, which ends around line 138).
- Test: `gateway/backend/internal/gateway/passthrough_progress_test.go` — modify `TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate` (doc from line 297, body from line 329) and add one new test after it.

**Interfaces:**

- **Consumes:** nothing at compile time. This task is fully implementable and
  verifiable with Tasks 1-4 absent: every test here sets `timings_per_token`
  **client-side**, which is the path the gateway already relays untouched. What
  Task 4's gated injection buys is that operator-opted-in traffic starts
  producing the partial `timings` objects this task reads; it changes no
  signature this task touches.
- **Produces:**
  - `inference.Usage.LiveOutputTokens int` with tag `json:"-"` — written ONLY by
    `mergeResponsesUsage`, read ONLY by `usageScanner.publishProgress`.
  - `func mergeResponsesUsage(dst *inference.Usage, payload []byte)` — unchanged
    signature; now also lifts `timings.predicted_n` into
    `dst.LiveOutputTokens` via `takeMax`.
  - `func (s *usageScanner) publishProgress(frame inference.Usage, authoritativeUsage bool)`
    — unchanged signature; now publishes `frame.OutputTokens` on an
    authoritative frame and `frame.LiveOutputTokens` on every other frame.
  - **No new DTO field, no new wire field, no frontend type change.** Task 6
    renders the existing `activeRequestDTO.OutputTokens` /
    `ActiveRequest.output_tokens`.

---

### FORBIDDEN in this task and in every later one

**No test may assert equality between the last partial's `predicted_n` and the
recorded total output tokens.** Such a test passes on cap-truncated data — where
both numbers are the cap, because the generation was cut off at
`max_output_tokens` — and FAILS on naturally-ending data, where the measured
run reached `predicted_n = 32` on its last partial while the terminal frame
reported 33. The partial series is monotone but not contiguous: llama.cpp
attaches `timings` to most partials and not all (the measured stream skipped 28
and 29 across the `output_item.added`/`content_part.added` pair, which carry no
`timings`). Anything that looks like `if lastPartialCount != events[0].OutputTokens`
is the trap; the author of this design walked into it twice.

**No test may put `predicted_n` into an assertion about `usage_events`, the
Activity totals, the timeseries or the limiter as a value that SHOULD appear
there.** The only permitted assertion in that direction is the negative one in
Step 1 and Step 3: it must be absent.

---

- [ ] **Step 1: Write the merge-level failing test**

Append to `gateway/backend/internal/gateway/passthrough_usage_scan_test.go`,
after `TestMergeResponsesUsageDraftTokensTakesRunningMax`:

```go
// TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly pins the carrier
// rule at the merge itself: llama.cpp's `timings.predicted_n` becomes
// LiveOutputTokens and touches neither OutputTokens nor TotalTokens.
//
// The rule needs a test of its own because this merge writes to TWO
// destinations. scan (passthrough_usage_scan.go) calls mergePassthroughUsage
// once into a per-frame scratch Usage -- what the live column reads -- and once
// into the scanner's ACCUMULATOR, which usage() hands recordUsage. A field added
// to the timings struct therefore reaches the accumulator BY CONSTRUCTION.
// Choosing a separate field is the only thing that keeps it out of the recorded
// row, and so out of usage_events, the Activity totals, the usage timeseries and
// the principal rate limiter's input. The negative assertion below is what turns
// that from a reviewed fact into a pinned one.
//
// The running max is asserted in both directions for the same reason
// DraftTokens asserts it above: mergeResponsesUsage runs once per SSE frame, and
// feed()'s documented tolerance for scanning a line more than once rests on
// every COUNT this merge writes being monotone. predicted_n is monotone over a
// generation, so a max is its final value.
//
// The last case is the shape nothing was ever observed emitting -- a `timings`
// object with no `predicted_n` key -- and it must report no count rather than
// infer one from the rate sitting beside it.
func TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly(t *testing.T) {
	var u inference.Usage
	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_per_second":38.25,"predicted_n":12}}`))
	if u.LiveOutputTokens != 12 {
		t.Fatalf("LiveOutputTokens = %d, want 12 (timings.predicted_n)", u.LiveOutputTokens)
	}
	if u.OutputTokens != 0 || u.TotalTokens != 0 {
		t.Fatalf("OutputTokens/TotalTokens = %d/%d, want 0/0 — predicted_n must land in NEITHER: those two are what usageScanner.usage hands recordUsage, and thence usage_events, the Activity totals, the timeseries and the rate limiter", u.OutputTokens, u.TotalTokens)
	}

	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_n":9}}`))
	if u.LiveOutputTokens != 12 {
		t.Fatalf("LiveOutputTokens = %d, want 12 (a later, smaller predicted_n must not overwrite the running max)", u.LiveOutputTokens)
	}
	mergeResponsesUsage(&u, []byte(`{"timings":{"predicted_n":20}}`))
	if u.LiveOutputTokens != 20 {
		t.Fatalf("LiveOutputTokens = %d, want 20 (a later, larger predicted_n must raise the running max)", u.LiveOutputTokens)
	}

	var absent inference.Usage
	mergeResponsesUsage(&absent, []byte(`{"timings":{"predicted_per_second":38.25}}`))
	if absent.LiveOutputTokens != 0 {
		t.Fatalf("LiveOutputTokens = %d, want 0 — a timings object carrying no predicted_n key reports no count, and none may be inferred from the rate beside it", absent.LiveOutputTokens)
	}
}
```

- [ ] **Step 2: Write the two server-level cases**

Append to `gateway/backend/internal/gateway/passthrough_progress_test.go`,
immediately after `TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`
(which ends around line 412, at the closing brace after the
`timings_per_token` survival assertion):

```go
// TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount pins the two
// halves of the live count's source, each of which an implementation can get
// wrong while the other stays green.
//
// FIRST CASE — a `timings` object is not itself a count. llama.cpp was never
// observed emitting one WITHOUT `predicted_n` on this endpoint (every measured
// partial that carried a `timings` object carried one), but "the object is
// present" and "the count is present" are different facts, and the reader must
// not turn the first into the second. The rate is still displayed; the count
// cell stays empty.
//
// SECOND CASE — `predicted_n` with no rate beside it, which is the shape that
// decides the row's LABEL, and it is reachable rather than contrived: the
// measured predicted_per_second series OPENS AT 0.0 (see the final* fields in
// passthrough_usage_scan.go for the recorded series), and observeDelta stores a
// rate only when it is positive. So the first timings-bearing partials of a real
// flagged stream hand the row an exact upstream count and no upstream rate, at
// which point liveProgressDTO's window derivation over that exact count fires
// and the cell is labelled "gateway". That state did not exist for this flavor
// mid-stream before a mid-stream count did. It is the feature's EXISTING rule,
// not a new one -- a gateway rate is only ever derived over an exact upstream
// count (liveProgressDTO, request_progress.go) -- and it is asserted here so
// that it is a decision on the record instead of an accident nobody noticed.
//
// Neither case asserts anything about the relayed body: this reader works on a
// CLIENT-set timings_per_token, which is what the fixture sends, and coupling
// these assertions to what the gateway may or may not inject would make them
// fail for a reason that is not theirs.
func TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount(t *testing.T) {
	for _, tc := range []struct {
		name string
		// timings is the object attached to BOTH partial frames, the two cases
		// differing only in which of its two keys is present.
		timings    string
		wantTokens int
		wantSource string
	}{
		{
			name:       "a timings object without predicted_n is a rate and nothing more",
			timings:    `{"predicted_per_second":38.25}`,
			wantTokens: 0,
			wantSource: "upstream",
		},
		{
			name:       "predicted_n with no rate beside it is an exact count, and the rate is gateway-derived",
			timings:    `{"predicted_n":12}`,
			wantTokens: 12,
			wantSource: "gateway",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got liveRow
			prov := &progressObservingProxyProvider{
				pieces: []string{
					"event: response.output_text.delta\n" +
						`data: {"type":"response.output_text.delta","delta":"hi","timings":` + tc.timings + `}` + "\n\n",
					"event: response.output_text.delta\n" +
						`data: {"type":"response.output_text.delta","delta":" there","timings":` + tc.timings + `}` + "\n\n",
				},
				gap: framePacing,
			}
			srv := newNativeProxyTestServer(prov, true, false)
			prov.observe = func() { got = snapshotLiveRow(t, srv) }

			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gw-model","stream":true,"input":"hi","timings_per_token":true}`))
			req.Header.Set("Authorization", "Bearer dev-secret")
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if got.ttftMS <= 0 {
				t.Fatalf("live ttft_ms = %d, want > 0 (content did arrive; only the count and the label are in question here)", got.ttftMS)
			}
			if got.outputTokens != tc.wantTokens {
				t.Fatalf("live output_tokens = %d, want %d — the mid-stream count is timings.predicted_n and nothing else: never inferred from a sibling timings field, never a count of delta frames", got.outputTokens, tc.wantTokens)
			}
			if got.source != tc.wantSource {
				t.Fatalf("live tokens_per_second_source = %q, want %q", got.source, tc.wantSource)
			}
			if got.tps <= 0 {
				t.Fatalf("live tokens_per_second = %v, want > 0 for a %q-labelled cell", got.tps, tc.wantSource)
			}
		})
	}
}
```

- [ ] **Step 3: Make the existing client-timings test restate the new rule**

`gateway/backend/internal/gateway/passthrough_progress_test.go`,
`TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`.

Its assertion `live output_tokens = %d, want 0` survives today only on FIXTURE
SHAPE: its partials carry a `timings` object with no `predicted_n`, a shape
llama.cpp was never observed to emit (90 of 90 measured partials carry one). Its
stated reason — *"the partials still report no usage"* — is what this task makes
false. Leaving the fixture alone would leave a green test whose message is a lie.

Three edits.

**3a.** Replace the doc sentence (around lines 303-304):

```go
// mysterious. The token count is the upstream's own `timings.predicted_n` off
// those same partial frames — a count of what the UPSTREAM has generated, which
// is a fact it reported, not a count of delta frames this gateway added up.
```

**3b.** Grow both fixture partials with a `predicted_n` (around lines 334 and
336) — monotone, because a generated-token count is:

```go
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":"hi","timings":{"predicted_per_second":42.5,"predicted_n":7}}` + "\n\n",
			"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","delta":" there","timings":{"predicted_per_second":38.25,"predicted_n":12}}` + "\n\n",
```

**3c.** Replace the count assertion (around lines 360-362) and add the
accumulator tripwire immediately after the existing recorded-rate assertion
(the block ending `…terminal frame or no terminal frame", events[0].TokensPerSecond)`):

```go
	if got.outputTokens != 12 {
		t.Fatalf("live output_tokens = %d, want 12 — the LATEST partial's timings.predicted_n, the upstream's own count of what it has generated so far", got.outputTokens)
	}
```

```go
	// The same request's RECORDED row is the other half of the rule: the live
	// count rides a field of its own, so no frame of this stream having reported
	// a usage object must still mean the recorded row reports none. This is the
	// one assertion that fires if predicted_n is routed into OutputTokens (or
	// TotalTokens) instead — the mutation that would silently rewrite
	// usage_events, the Activity totals, the timeseries and the limiter's input
	// for every flagged stream, with no other test in the suite noticing.
	if events[0].OutputTokens != 0 || events[0].TotalTokens != 0 {
		t.Fatalf("recorded OutputTokens/TotalTokens = %d/%d, want 0/0 — this stream reported no usage object on any frame, so the RECORDED row must report no tokens; 12 in either field means timings.predicted_n reached the accumulator's recorded counts", events[0].OutputTokens, events[0].TotalTokens)
	}
```

- [ ] **Step 4: Run it and watch it fail to build**

Run (from the worktree root):
`cd gateway/backend && go test ./internal/gateway/`

Expected — a BUILD failure, not a test failure, because Step 1 names a field
that does not exist yet. The compiler emits one error per mention of
`LiveOutputTokens` in the new merge test (there are eight), all with the same
message:

```
# op-ai-gateway/internal/gateway [op-ai-gateway/internal/gateway.test]
internal/gateway/passthrough_usage_scan_test.go:<line>:<col>: u.LiveOutputTokens undefined (type inference.Usage has no field or method LiveOutputTokens)
...
FAIL	op-ai-gateway/internal/gateway [build failed]
```

No test runs, so this proves nothing about behaviour yet. Step 5 adds the field
alone — deliberately WITHOUT the merge and without the publish — so that Step 6
produces the real behavioural red.

- [ ] **Step 5: Add the carrier field (and nothing else)**

`gateway/backend/internal/inference/types.go`, in `type Usage struct`, after
`DraftTokens` (the current last field, around line 117):

```go
	// LiveOutputTokens is llama.cpp's own `timings.predicted_n`: the number of
	// tokens the upstream reports having GENERATED so far. It exists for the
	// running-connections panel's live column and for nothing else.
	//
	// It is deliberately NOT OutputTokens and NOT TotalTokens. Those two are what
	// usageScanner.usage hands recordUsage, and thence usage_events, the Activity
	// totals, the usage timeseries and the principal rate limiter's input.
	// Measured on llama.cpp build b10448-ad1de39e0: on a NATURALLY ENDING
	// generation the TERMINAL predicted_n equals that response's
	// usage.output_tokens exactly, reasoning tokens included. So the live count
	// CONVERGES on the recorded total rather than competing with it -- which is
	// precisely why it must not also be written there: it would rewrite every one
	// of those surfaces with a number carrying no information they do not already
	// have.
	//
	// Mid-stream it is neither contiguous nor equal to that total. llama.cpp
	// attaches `timings` to most partials but not all -- the measured stream
	// skipped 28 and 29 across the output_item.added/content_part.added pair,
	// which carry none -- and the highest value any PARTIAL carried was short of
	// the terminal total. This is therefore an upstream count of what has been
	// generated, never a count of what the client has received, and nothing may
	// assert equality between the last partial's value and the recorded total:
	// such an assertion passes on a cap-truncated generation, where both numbers
	// are just the cap, and fails on a naturally ending one.
	//
	// Written only by mergeResponsesUsage (native_passthrough.go); read only by
	// usageScanner.publishProgress (passthrough_usage_scan.go). Anthropic's merge
	// writes it never, which is what keeps message_start's placeholder count off
	// the live row through this door. `json:"-"` because it belongs to no
	// recorded and no wire representation of usage: recordUsage assembles
	// usage.Event field by field and has no member for it, and the client-facing
	// bodies compat builds carry usage structs of their own.
	LiveOutputTokens int `json:"-"`
```

- [ ] **Step 6: Run it again and watch the three behavioural failures**

Run: `cd gateway/backend && go test ./internal/gateway/`

Expected — the package now builds and exactly three tests fail (the package
takes roughly 95 s):

```
--- FAIL: TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate (…)
    passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the LATEST partial's timings.predicted_n, the upstream's own count of what it has generated so far
--- FAIL: TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount (…)
    --- FAIL: TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount/predicted_n_with_no_rate_beside_it_is_an_exact_count,_and_the_rate_is_gateway-derived (…)
        passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the mid-stream count is timings.predicted_n and nothing else: never inferred from a sibling timings field, never a count of delta frames
--- FAIL: TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly (0.00s)
    passthrough_usage_scan_test.go:<line>: LiveOutputTokens = 0, want 12 (timings.predicted_n)
FAIL
FAIL	op-ai-gateway/internal/gateway	<~95>s
```

Everything else is green. In particular the first subtest of the new test
("a timings object without predicted_n…") PASSES already — it is a tolerance
guard, not a driver; see Step 12 for the mutation it does catch.

- [ ] **Step 7: Read `predicted_n` in the merge**

`gateway/backend/internal/gateway/native_passthrough.go`, function
`mergeResponsesUsage`. Add the field to the anonymous `Timings` struct (around
lines 588-592), keeping the wire order llama.cpp uses:

```go
		Timings *struct {
			PromptPerSecond    float64 `json:"prompt_per_second"`
			PredictedPerSecond float64 `json:"predicted_per_second"`
			PredictedN         int     `json:"predicted_n"`
			DraftN             int     `json:"draft_n"`
		} `json:"timings"`
```

and merge it in the `if m.Timings != nil` block (around lines 609-613):

```go
	if m.Timings != nil {
		takeMaxF(&dst.PromptPerSecond, m.Timings.PromptPerSecond)
		takeMaxF(&dst.TokensPerSecond, m.Timings.PredictedPerSecond)
		// predicted_n lands in its OWN field and never in OutputTokens or
		// TotalTokens. This function writes to TWO destinations -- scan's
		// per-frame scratch Usage, which the live column reads, and the
		// scanner's accumulator, which usage() hands recordUsage -- so whatever
		// is written here reaches the accumulator by construction. A separate
		// field is what makes that harmless: recordUsage assembles usage.Event
		// field by field and has no member for this one. See
		// inference.Usage.LiveOutputTokens for what would be rewritten
		// otherwise.
		takeMax(&dst.LiveOutputTokens, m.Timings.PredictedN)
		takeMax(&dst.DraftTokens, m.Timings.DraftN)
	}
```

- [ ] **Step 8: Publish it to the live row**

`gateway/backend/internal/gateway/passthrough_usage_scan.go`, method
`publishProgress`. Replace the body's count branch (around lines 398-401):

```go
	prog := inference.StreamProgress{TokensPerSecond: frame.TokensPerSecond}
	if authoritativeUsage {
		prog.OutputTokens = frame.OutputTokens
	} else {
		prog.OutputTokens = frame.LiveOutputTokens
	}
	s.progress.observeDelta(s.firstContentAt, &prog)
```

and replace the SECOND of the doc comment's "Three deliberate restrictions"
bullets (the one currently beginning *"The output-token count is published only
from an AUTHORITATIVE usage frame"*, around lines 375-382) with:

```go
//   - The output-token count comes from exactly two sources and never from a
//     frame that merely carries a usage object. An AUTHORITATIVE usage frame
//     (isTerminalUsageFrame) publishes its own OutputTokens; every other frame
//     publishes LiveOutputTokens, which only mergeResponsesUsage writes and only
//     out of llama.cpp's `timings.predicted_n`. The SEPARATE FIELD is what makes
//     that safe to open up. The old gate was "authoritative frames only", and its
//     reason was Anthropic's message_start: it carries `output_tokens: 1` as a
//     PLACEHOLDER that the merge cannot tell from a real total, and liveProgressDTO
//     would divide that 1 by the generation window and DISPLAY the result as a
//     measured rate for the rest of the stream. mergeAnthropicUsage writes no
//     LiveOutputTokens at all, so the placeholder cannot reach the row through the
//     new door either -- the gate is now "an authoritative frame, or a field that
//     only the Responses `timings` object fills".
//
//     On the terminal Responses frame BOTH are present and they report the same
//     quantity (measured: a naturally ending generation's terminal predicted_n
//     equals that response's usage.output_tokens). The authoritative count wins
//     by the explicit branch above rather than by observeDelta's last write, so
//     which of the two lands on the row is a rule and not an ordering accident.
```

- [ ] **Step 9: Tell the DTO's reader what the field now carries**

`gateway/backend/internal/gateway/active_requests.go`, the doc comment above
`activeRequestDTO.OutputTokens` (around lines 200-205). Comment only — the field
and its JSON name are unchanged, and that is the point: the mid-stream Responses
count reaches the panel through the field that already exists.

```go
	// Live per-request progress. output_tokens is the upstream's own cumulative
	// count of tokens GENERATED so far (0 = none reported) -- never a count of
	// what the client has received and never a gateway estimate. For
	// anthropic_messages it is message_delta's usage.output_tokens; for
	// openai_responses it is llama.cpp's `timings.predicted_n` off whatever
	// partial carries a `timings` object, and then the terminal frame's own
	// response.usage.output_tokens. The Responses figure therefore climbs in
	// jumps (partials without a `timings` object skip values) and can sit a token
	// or two below what the finished request records.
	// tokens_per_second is 0 when not measured; and
	// tokens_per_second_source says how the rate was obtained -- "upstream"
	// (reported by the inference server), "gateway" (computed here from the
	// upstream's exact count), or "" (not measured). Plain string, deliberately
	// NOT omitempty, so "not measured" is explicit on the wire.
```

- [ ] **Step 10: Run the package and watch it go green**

Run: `cd gateway/backend && go test ./internal/gateway/`

Expected:

```
ok  	op-ai-gateway/internal/gateway	<~95>s
```

If `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves` fails
with `live output_tokens = … want exactly 40`, the branch in Step 8 was written
the wrong way round: that test's terminal frame carries a `timings` object with
NO `predicted_n`, so the authoritative arm must supply the 40.

- [ ] **Step 11: Mutation 1 — delete the merge line**

In `mergeResponsesUsage`, delete the single line
`takeMax(&dst.LiveOutputTokens, m.Timings.PredictedN)`. Change nothing else.

Run: `cd gateway/backend && go test ./internal/gateway/`

Expected — the same three failures as Step 6:

```
--- FAIL: TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly (0.00s)
    passthrough_usage_scan_test.go:<line>: LiveOutputTokens = 0, want 12 (timings.predicted_n)
--- FAIL: TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate (…)
    passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the LATEST partial's timings.predicted_n, …
--- FAIL: TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount/predicted_n_with_no_rate… (…)
    passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the mid-stream count is timings.predicted_n and nothing else, …
```

Restore the line.

- [ ] **Step 12: Mutation 2 — send `predicted_n` to the recorded count instead**

In `mergeResponsesUsage`, change that one line's destination:
`takeMax(&dst.OutputTokens, m.Timings.PredictedN)`. Change nothing else. This is
the mutation the whole design of this task exists to prevent.

Run: `cd gateway/backend && go test ./internal/gateway/`

Expected:

```
--- FAIL: TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly (0.00s)
    passthrough_usage_scan_test.go:<line>: LiveOutputTokens = 0, want 12 (timings.predicted_n)
--- FAIL: TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate (…)
    passthrough_progress_test.go:<line>: recorded OutputTokens/TotalTokens = 12/12, want 0/0 — this stream reported no usage object on any frame, …
```

(The merge test aborts at its first assertion, so its `OutputTokens/TotalTokens`
message does not print; the progress test's recorded-row assertion is the one
that names the damage. `TotalTokens` is 12 as well because `finalizeTotalTokens`
fills it from input+output.)

Restore `&dst.LiveOutputTokens`.

- [ ] **Step 13: Mutation 3 — delete the publish branch**

In `publishProgress`, delete the `else { prog.OutputTokens = frame.LiveOutputTokens }`
arm, leaving the original `if authoritativeUsage { … }`. Change nothing else.

Run: `cd gateway/backend && go test ./internal/gateway/`

Expected — the two server-level tests fail and the merge test PASSES, which is
exactly the separation wanted (the merge reads it; the publish displays it):

```
--- FAIL: TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate (…)
    passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the LATEST partial's timings.predicted_n, …
--- FAIL: TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount/predicted_n_with_no_rate… (…)
    passthrough_progress_test.go:<line>: live output_tokens = 0, want 12 — the mid-stream count is timings.predicted_n and nothing else, …
```

Restore the `else` arm.

**On record, so nobody has to rediscover it:** the first subtest of
`TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount` and the
`absent` case of `TestMergeResponsesUsagePredictedNLandsInTheLiveFieldOnly` are
the only two assertions in this task that do NOT fail when the production change
is reverted whole. They are guards, not drivers, and the design asked for them
by name. The mutation each one does catch is a single edit inside the new code:
change `takeMax(&dst.LiveOutputTokens, m.Timings.PredictedN)` to
`takeMax(&dst.LiveOutputTokens, int(m.Timings.PredictedPerSecond))` — a
confusion of the two `predicted_*` keys that is easy to make and that makes both
guards report 38 where they want 0. Running that mutation is optional; the three
above are not.

- [ ] **Step 14: Format, lint, and run every package**

CI runs `golangci-lint fmt --diff` and `golangci-lint run` per Go module, and
neither `gofmt` nor `go vet` covers them.

Run:
```
cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

Expected: no diff from `fmt --diff`, `0 issues` from `run`, and `ok` for every
package (`internal/gateway` around 95 s; `internal/inference` is fast and has no
test that a new field disturbs — nothing in the backend compares an
`inference.Usage` whole).

- [ ] **Final step: Commit**

```
git add gateway/backend/internal/inference/types.go \
        gateway/backend/internal/gateway/native_passthrough.go \
        gateway/backend/internal/gateway/passthrough_usage_scan.go \
        gateway/backend/internal/gateway/active_requests.go \
        gateway/backend/internal/gateway/passthrough_usage_scan_test.go \
        gateway/backend/internal/gateway/passthrough_progress_test.go
```

```
git commit -F - <<'MSG'
feat: Read llama.cpp's timings.predicted_n as the Responses live token count

A streaming native-passthrough /v1/responses request whose partials carry a
`timings` object now shows an output-token count on the running-connections
panel while it is still in flight. The count is the upstream's own
`timings.predicted_n` -- a fact llama.cpp reported, not a count of delta frames
this gateway added up -- so the `openai_responses` column stops being empty
mid-stream wherever the flag is set, by the client or by the operator's switch.

The count rides inference.Usage.LiveOutputTokens, a field of its own, and
deliberately not OutputTokens or TotalTokens. mergeResponsesUsage writes to two
destinations at once -- scan's per-frame scratch, which the live column reads,
and the scanner's accumulator, which usage() hands recordUsage -- so anything
added to the timings struct reaches the accumulator by construction. A separate
field is what keeps it out of usage_events, the Activity totals, the usage
timeseries and the principal rate limiter: recordUsage assembles usage.Event
field by field and has no member for it. Measured on llama.cpp build
b10448-ad1de39e0, a naturally ending generation's terminal predicted_n equals
that response's usage.output_tokens exactly, so the live count converges on the
recorded total instead of competing with it -- which is the reason it must not
also be written there, not a reason it may be.

publishProgress's count gate widens from "an authoritative usage frame only" to
"an authoritative frame, or the field only the Responses timings object fills".
That stays safe for anthropic_messages because mergeAnthropicUsage writes the
new field never, so message_start's placeholder output_tokens: 1 -- the reason
the old gate existed -- still cannot reach the row.

No DTO field and no wire change: the mid-stream count reaches the panel through
active_requests' existing output_tokens, which already carried the Anthropic
equivalent.

One consequence worth naming: an openai_responses row can now show a
gateway-labelled mid-stream rate. The measured predicted_per_second series opens
at 0.0 and observeDelta stores only a positive rate, so the first
timings-bearing partials give the row an exact count and no upstream rate, and
liveProgressDTO's window derivation over that exact count fires. That is the
feature's existing rule -- a derived rate only ever over an exact upstream count
-- and it is pinned by a test rather than left to be discovered.
MSG
```

---

### Task 6: The live output-tokens column on the running-connections panel

**Goal:** the operator can put an upstream-reported, mid-stream output-token
count on the running-connections panel. The column is **default-hidden**, renders
the existing `output_tokens` DTO field, and renders a zero as the panel's shared
"never measured" em-dash rather than as a measured `0`.

**Files:**

- Modify: `gateway/frontend/src/i18n.ts` — two new entries `activityColLiveOutputTokens`, one in the `de` object and one in the `en` object, beside `activityColLiveTokenSpeed` / `activityColTTFT`; and the two `activityLiveTpsNone` strings.
- Modify: `gateway/frontend/src/components/ActiveRequestsPanel.tsx` — the `activeColumns` catalogue inside `ActiveRequestsPanel` (one new entry between the `live_tps` and `ttft` entries), and two clauses of `liveTpsTitle`'s doc comment.
- Modify: `gateway/frontend/src/api/usage.ts` — the doc comment above `ActiveRequest.output_tokens`. Comment only.
- Test: `gateway/frontend/src/i18n.test.ts` — one key added to the `keys` array in `it('defines the active-panel keys in de and en')`, and one new `it(...)` in `describe('live tokens/sec provenance strings claim only what the row can know')`; plus three comment corrections, in three other cases of that same `describe`.
- Test: `gateway/frontend/src/components/ActiveRequestsPanel.test.tsx` — one new `describe` block with two cases, appended at the end; two comment corrections; `fireEvent` added to the existing `@testing-library/react` import.

**Interfaces:**

- **Consumes:** nothing at compile time. The field this column renders
  (`ActiveRequest.output_tokens`) has shipped since the passthrough bridge, so
  every step here builds and passes with Tasks 1–5 absent. Run it **after Task
  5** all the same: the comments it writes describe `timings.predicted_n` as the
  mid-stream source, which is Task 5's behaviour, and Step 10's string names the
  operator switch, whose effect is Task 4's.
- **Produces:**
  - `messages.de.activityColLiveOutputTokens` / `messages.en.activityColLiveOutputTokens`
    — `string`, reachable as `t.activityColLiveOutputTokens` through
    `Translation = typeof messages.de`.
  - A `ListColumn<ActiveRequest>` with `id: 'output_tokens'` in
    `ActiveRequestsPanel`'s `activeColumns`, `defaultHidden: true`,
    `numeric: true`, `searchable: false`, `value: (a) => formatMetric(a.output_tokens, 0)`.
    The catalogue goes from 15 columns to 16, and the default-hidden subset from
    5 to 6. `'output_tokens'` becomes a persisted column id under the
    `op.activeRequests` storage key; it must not be renamed afterwards without a
    migration of the stored order.
  - **No DTO field, no TypeScript type member, no API change.**

---

- [ ] **Step 0: Install the frontend dependencies**

`gateway/frontend/node_modules` is gitignored (the root `.gitignore` lists
`gateway/frontend/node_modules/`), so a fresh worktree does not have it, and every
`npx vitest`, `npm run format:check`, `npm run lint`, `npm run build` and
`npm run test` below dies at startup without it — before any assertion runs. From
the repository root:

```
cd gateway/frontend && npm ci
```

Expected: a clean install from `package-lock.json`, exit 0, printing a line of
the shape `added <n> packages, and audited <n+1> packages in <t>`. If
`gateway/frontend/node_modules` is already present, skip this step — `npm ci`
deletes and reinstalls the whole tree, which costs minutes and changes nothing —
but come back and run it if any command below reports a missing module or a
missing binary.

- [ ] **Step 1: Write the failing i18n key test**

`gateway/frontend/src/i18n.test.ts`, in
`describe('running-connections (active requests) i18n keys')` →
`it('defines the active-panel keys in de and en')`. Add one entry to the `keys`
array, so it reads:

```ts
    const keys = [
      'activityActiveTitle',
      'activityActiveEmpty',
      'activityActiveElapsed',
      'activityActiveSession',
      'activityColLiveOutputTokens',
    ] as const;
```

- [ ] **Step 2: Run it and watch it fail**

From `gateway/frontend`:

```
npx vitest run src/i18n.test.ts
```

A single-file run is a **probe**, never this task's verdict — the verdict is
`npm run test` in Step 14. Expected: one failing case (vitest's `retry: 2`
re-runs it twice first, so the same failure prints three times):

```
 FAIL  src/i18n.test.ts > running-connections (active requests) i18n keys > defines the active-panel keys in de and en
AssertionError: expected 'undefined' to be 'string' // Object.is equality
```

(The quotes around `undefined` are not a typo: the assertion is
`expect(typeof messages.de[k]).toBe('string')`, so what is compared is the
`typeof` **string** `'undefined'`, not the value.)

Do **not** run `npm run build` here: `tsconfig.json` has `include: ["src"]`, so
`tsc` type-checks test files and would fail on the unknown key with
`Property 'activityColLiveOutputTokens' does not exist on type ...`. That is
expected until Step 3 and is not information.

- [ ] **Step 3: Add the two strings**

`gateway/frontend/src/i18n.ts`. In the `de` object, immediately after
`activityColLiveTokenSpeed`:

```ts
  activityColLiveOutputTokens: 'Generiert (live)',
```

and in the `en` object, in the same position:

```ts
  activityColLiveOutputTokens: 'Generated (live)',
```

Both must be added: `en` is declared as `const en: PortalMessages = {` with
`PortalMessages = typeof de`, so adding only the German entry breaks `tsc`.
The label deliberately mirrors the completed-requests table's `activityColOutput`
(`Generiert` / `Generated`) plus the `(live)` suffix `activityColLiveTokenSpeed`
already uses, so the panel and the usage list name one quantity one way. Neither
string is a substring of any other column header on this panel, which keeps the
header-index lookups in this file's tests unambiguous.

Run `npx vitest run src/i18n.test.ts` again. Expected: `Test Files 1 passed`.

- [ ] **Step 4: Write the two failing column tests**

Append to `gateway/frontend/src/components/ActiveRequestsPanel.test.tsx`, after
the closing `});` of `describe('ActiveRequestsPanel native-passthrough row shapes')`:

```tsx
// The live output-tokens column. It renders the DTO field the panel has carried
// since the passthrough bridge (`output_tokens`, already populated for
// anthropic_messages) -- there is no new wire field and no new type member here,
// only a column that shows one.
describe('ActiveRequestsPanel live output-tokens column', () => {
  it('stays hidden by default and is offered in the column menu', async () => {
    render(
      <ActiveRequestsPanel
        t={t}
        active={[makeActive({ output_tokens: 12 })]}
        effectiveScope="own"
      />,
    );

    // Hidden by default, and deliberately so. This panel renders a
    // never-measured metric as the shared em-dash, and output_tokens is 0 -- "the
    // upstream reported none" -- on most rows, so a visible count column would put
    // a SECOND em-dash cell on those rows and make five of this file's existing
    // cases ambiguous: getByRole('cell', { name: '—' }) and getByText('—') both
    // throw on more than one match, and the failure ("Found multiple elements")
    // invites the wrong repair -- loosening the query, which would retire the
    // invariant that a measured zero never renders as "0".
    expect(screen.queryByRole('columnheader', { name: t.activityColLiveOutputTokens })).toBeNull();

    // Hidden is not the same as absent: the operator who wants the count must be
    // able to switch it on, so the column menu has to offer it, unticked.
    fireEvent.click(screen.getByRole('button', { name: t.listColumns }));
    const entry = await screen.findByRole('checkbox', {
      name: t.activityColLiveOutputTokens,
    });
    expect(entry).not.toBeChecked();
  });

  it('shows the upstream count when switched on, and a zero as the shared em-dash', () => {
    // ListTable persists column visibility at `table.<storageKey>.hidden`,
    // mirrored to localStorage at `op.pref.` + key; an EMPTY hidden set is the
    // state of an operator who has switched every optional column on. Seeded
    // rather than clicked because an open MUI column menu is a Modal that
    // aria-hides the rest of the page, and getByRole skips an aria-hidden
    // subtree -- the row queries below would find nothing. vitest.setup.ts
    // clears localStorage after every test, so this leaks into none of them.
    window.localStorage.setItem('op.pref.table.op.activeRequests.hidden', JSON.stringify([]));
    render(
      <ActiveRequestsPanel
        t={t}
        active={[
          // A mid-stream openai_responses passthrough row: the count is
          // llama.cpp's own timings.predicted_n off a partial frame, and the rate
          // is derived over that exact count because the same partial reported no
          // rate of its own (the measured predicted_per_second series opens at
          // 0.0). Pinned backend-side by
          // TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount
          // (passthrough_progress_test.go); asserted here only as a row shape.
          makeActive({
            id: 'act_live_count',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 12,
            tokens_per_second: 24.0,
            tokens_per_second_source: 'gateway',
          }),
          // The same flavor before any timings-bearing partial arrived. 0 means
          // "the upstream reported none", which is not a measurement of zero
          // tokens, so the cell must read as the shared em-dash.
          makeActive({
            id: 'act_no_count',
            model: 'live-model-2',
            api_flavor: 'openai_responses',
            req_path: '/v1/responses',
            provider_path: '/v1/responses',
            ttft_ms: 640,
            output_tokens: 0,
          }),
        ]}
        effectiveScope="own"
      />,
    );

    // Addressed by column index rather than by cell text: with every optional
    // column on, several cells in these rows are em-dashes, so a text query could
    // be satisfied by the wrong one. ListTable renders header and body cells from
    // the same visible-column list, so the index is shared.
    const headers = screen.getAllByRole('columnheader').map((h) => h.textContent ?? '');
    const countIndex = headers.findIndex((h) => h.includes(t.activityColLiveOutputTokens));
    expect(countIndex).toBeGreaterThanOrEqual(0);

    const countRow = screen.getByRole('cell', { name: 'live-model' }).closest('tr')!;
    expect(within(countRow).getAllByRole('cell')[countIndex].textContent).toBe('12');

    const noneRow = screen.getByRole('cell', { name: 'live-model-2' }).closest('tr')!;
    expect(within(noneRow).getAllByRole('cell')[countIndex].textContent).toBe('—');
  });
});
```

and widen the file's first import to bring in `fireEvent`:

```tsx
import { cleanup, fireEvent, render, screen, within } from '@testing-library/react';
```

- [ ] **Step 5: Run them and watch both fail**

```
npx vitest run src/components/ActiveRequestsPanel.test.tsx
```

Expected — the two new cases fail and every existing case in the file passes
(each failure prints three times because of `retry: 2`):

```
 FAIL  src/components/ActiveRequestsPanel.test.tsx > ActiveRequestsPanel live output-tokens column > stays hidden by default and is offered in the column menu
TestingLibraryElementError: Unable to find an accessible element with the role "checkbox" and name "Generiert (live)"

 FAIL  src/components/ActiveRequestsPanel.test.tsx > ActiveRequestsPanel live output-tokens column > shows the upstream count when switched on, and a zero as the shared em-dash
AssertionError: expected -1 to be greater than or equal to 0
```

Note which assertion does **not** fail in the first case: `queryByRole(...)` for
a column that does not exist returns `null`, so the default-hidden assertion is
green before the column ships. It is not the driver — the column-menu assertion
is — and Step 8 is where it earns its place.

- [ ] **Step 6: Add the column**

`gateway/frontend/src/components/ActiveRequestsPanel.tsx`, in the `activeColumns`
array inside `ActiveRequestsPanel`, between the `live_tps` entry and the `ttft`
entry (around line 218, immediately after `live_tps`'s closing `},`):

```tsx
    // Optional (hidden by default): the upstream's own cumulative count of tokens
    // GENERATED so far. Hidden because a 0 here means "the upstream reported
    // none", which this panel renders as the shared never-measured em-dash -- and
    // a second em-dash column on every unmeasured row makes the row-scoped
    // em-dash queries in this panel's tests ambiguous rather than wrong, which is
    // the kind of failure that gets "fixed" by loosening the query.
    //
    // formatMetric is what supplies that em-dash, and it doubles as the sort
    // accessor: '—' is not a number, so ListTable's `numeric` sort sinks an
    // unreported count in BOTH directions instead of ranking it as the smallest
    // real value.
    {
      id: 'output_tokens',
      label: t.activityColLiveOutputTokens,
      value: (a) => formatMetric(a.output_tokens, 0),
      searchable: false,
      numeric: true,
      defaultHidden: true,
    },
```

Placed before `ttft` on purpose: it keeps the three live-metric columns adjacent
and leaves `elapsed` last, which `Activity.active.test.tsx` reads positionally
(`cells[cells.length - 1]`).

Run `npx vitest run src/components/ActiveRequestsPanel.test.tsx`. Expected: the
whole file passes.

- [ ] **Step 7: Correct the two stale comments in this panel's test file**

Both are false the moment Task 5 reads `timings.predicted_n`, and both are the
"a test can stay green while its message goes false" hazard the design's §5
names. No assertion moves; the file must stay green.

**7a.** In `it('shows a TTFT beside an em-dash rate when the stream has a
first-content stamp and nothing else')`, replace the opening comment:

```tsx
    // openai_responses passthrough, mid-generation, with timings_per_token set by
    // NOBODY -- neither the client nor the operator's Responses live-timings
    // switch: the first content frame stamped a TTFT and the upstream attached no
    // `timings` to any partial, so there is neither a rate to report nor a
    // predicted_n to count. So: a real TTFT, no count, no rate -- and the rate
    // cell must read as not-applicable, never as a zero (which would read as
    // "stalled").
```

**7b.** In `it('shows an upstream-reported rate on a row whose token count is
still zero, and claims no count')`, replace the opening comment. The old one
explained the zero with *"(the Responses partials carry no usage)"*; that
parenthetical is exactly what Task 5 retires, and the row shape survives for a
different and narrower reason:

```tsx
    // Same stream WITH timings_per_token set -- by the client, or by the
    // operator's Responses live-timings switch: llama.cpp attaches its own
    // `timings` to the partial frames, so a real rate arrives. The count can
    // still be 0, because the mid-stream count is `timings.predicted_n` and
    // nothing else: a partial whose `timings` object carries a rate and no
    // predicted_n reports a rate and no count, the shape pinned by the first case
    // of TestPassthroughResponsesPartialPredictedNDecidesTheMidStreamCount
    // (passthrough_progress_test.go). ("The Responses partials carry no usage"
    // was the reason until predicted_n was read; it is no longer one.) "rate
    // present, tokens absent" is therefore still a legitimate row here, so the
    // tooltip for an upstream-reported rate must make no claim about a count -- a
    // "computed from 0 tokens" reading would be false in exactly the case that
    // produces this row.
```

Run `npx vitest run src/components/ActiveRequestsPanel.test.tsx`. Expected: still
green — comments only.

- [ ] **Step 8: Mutation 1 — delete `defaultHidden: true`**

In the new column entry in `ActiveRequestsPanel.tsx`, delete the single line
`defaultHidden: true,`. Change nothing else. Run the **whole** suite, because the
point of this mutation is which OTHER tests it takes down:

```
npm run test
```

Expected — six failing cases, all in `src/components/ActiveRequestsPanel.test.tsx`,
and no failure anywhere else in the suite. Five are the existing cases the design
predicted; the sixth is the new one:

```
 FAIL  ... > shows the em-dash for a request with no measurement and the rate with one decimal otherwise
 FAIL  ... > renders the TTFT cell as an em-dash when unmeasured and "<n> ms" otherwise
 FAIL  ... > distinguishes a measured rate that rounds to 0.0 from a measured zero
   TestingLibraryElementError: Found multiple elements with the role "cell" and name `—`
 FAIL  ... > names the never-measured provenance in the tooltip
   TestingLibraryElementError: Found multiple elements with the text: —
 FAIL  ... > shows a TTFT beside an em-dash rate when the stream has a first-content stamp and nothing else
   TestingLibraryElementError: Found multiple elements with the role "cell" and name `—`
 FAIL  ... > ActiveRequestsPanel live output-tokens column > stays hidden by default and is offered in the column menu
   AssertionError: expected <th …>Generiert (live)</th> to be null
```

(The exact punctuation of testing-library's name hint is its own; `Found multiple
elements` is the part that matters. `names the never-measured provenance` fails
on the text query, the other three on the role query.) Note that the fifth case
carries **two** ambiguity-prone assertions but aborts at the first, which is why
six assertions produce five existing failures — the count the design states.

Restore `defaultHidden: true,`.

- [ ] **Step 9: Mutation 2 — render the raw number instead of the metric**

In the same entry, change `value: (a) => formatMetric(a.output_tokens, 0)` to
`value: (a) => String(a.output_tokens)`. Change nothing else. Run:

```
npx vitest run src/components/ActiveRequestsPanel.test.tsx
```

Expected — one failing case, the second of the new ones:

```
 FAIL  ... > shows the upstream count when switched on, and a zero as the shared em-dash
AssertionError: expected '0' to be '—' // Object.is equality
```

That is the whole point of the column: an unreported count is not a measured
zero. Restore `formatMetric(a.output_tokens, 0)`.

- [ ] **Step 10: The not-measured tooltip stops presenting two axes as the whole list**

First the failing assertion. `gateway/frontend/src/i18n.test.ts`, appended inside
`describe('live tokens/sec provenance strings claim only what the row can know')`,
after `it('leaves the upstream-reported string free of any token claim')`:

```ts
  it('names the operator switch beside the client as a third dependency axis', () => {
    // The string ends by naming what the absence depends on, and it named two
    // things: the upstream, and what the client requested. Part 2 of issue #81
    // adds a third -- the operator's switch, which makes the gateway ask
    // llama.cpp for timings_per_token on a streaming /v1/responses passthrough.
    // So a row can be empty because the SWITCH is off while the client asked for
    // nothing, and can carry a rate no client ever asked for. A two-item list
    // presented as the whole list is the same class of defect as the wrong cause
    // the first case in this describe bans: the sentence has to be complete, not
    // merely free of false blame.
    expect(messages.en.activityLiveTpsNone).toMatch(/switch/i);
    expect(messages.de.activityLiveTpsNone).toMatch(/Schalter/i);
  });
```

Run `npx vitest run src/i18n.test.ts`. Expected:

```
 FAIL  src/i18n.test.ts > live tokens/sec provenance strings claim only what the row can know > names the operator switch beside the client as a third dependency axis
AssertionError: expected 'Not measured — the inference server re…' to match /switch/i
```

Then amend both strings in `gateway/frontend/src/i18n.ts` — `activityLiveTpsNone`
in the `de` object and in the `en` object:

```ts
  activityLiveTpsNone:
    'Nicht gemessen — der Inferenzserver hat keine Rate gemeldet, und es konnte noch keine berechnet werden; beides hängt vom Inferenzserver, von der Anfrage des Clients und vom Responses-Live-Timings-Schalter ab',
```

```ts
  activityLiveTpsNone:
    'Not measured — the inference server reported no rate, and none could be derived yet; both depend on the upstream, on what the client requested, and on the Responses live-timings switch',
```

Both are checked against every assertion that was already guarding this string
before the case above was added — `grep -n activityLiveTpsNone src/i18n.test.ts`
returned **nine**, five on `de` and four on `en` (eleven once the new case is in
place), and no other file asserts on its content:

- `de` still matches `/nicht gemessen/i` and `/client/i`, and still avoids
  `/dieser Upstream/i`, `/dies\w* Inferenzserver/i` (it says *"der"* and *"vom"*
  Inferenzserver, never *"diesem"*) and `/keine exakte Tokenzahl/i` (the added
  clause names the switch, never a count).
- `en` still matches `/not measured/i` and `/client/i`, and still avoids
  `/this upstream/i` and `/no exact token count/i`.

`ActiveRequestsPanel.test.tsx` reads the string four times as well, but always
through `t.activityLiveTpsNone` itself (`getByTitle` / `toHaveAttribute`), so
those follow any rewording. The English sentence deliberately carries no
apostrophe, so prettier keeps it in single quotes.

Run `npx vitest run src/i18n.test.ts`. Expected: `Test Files 1 passed`.

- [ ] **Step 11: Correct the remaining stale comments**

**11a.** `gateway/frontend/src/i18n.test.ts`, the opening comment of
`it('leaves the upstream-reported string free of any token claim')` — the third
site of the claim Task 5 retires, which the map does not list:

```ts
    // A native-passthrough openai_responses stream WITH timings_per_token can
    // produce the row shape "rate present, token count 0": the mid-stream count
    // is `timings.predicted_n` and nothing else, so a partial whose `timings`
    // object carries a rate and no predicted_n puts a rate on the row and no
    // count. This is the string shown there, so interpolating a count into it (as
    // activityLiveTpsGateway legitimately does) would make it claim "computed
    // from 0 tokens".
```

**11b.** `gateway/frontend/src/components/ActiveRequestsPanel.tsx`, in
`liveTpsTitle`'s doc comment: **two** clauses, both in the same comment block
above the function. First, the clause listing the causes of an empty source names
only the client as the party that can ask for timings. Replace that one clause,
leaving the lines around it untouched:

```tsx
// same empty source arises from a translated stream whose provider reports neither
// an exact count nor a rate; from a native-passthrough openai_responses stream that
// NOBODY asked for timings on — neither the CLIENT nor the operator's Responses
// live-timings switch — where the very same llama.cpp upstream would have attached
// its own `timings` to the partial frames had it been asked (the per-flavor
```

Second, the comment's closing paragraph — about fifteen lines further down,
immediately above `function liveTpsTitle` — still describes the string Step 10
just rewrote as naming *"both dependency axes"*, which is now a two-item summary
of a three-item sentence. Replace that paragraph:

```tsx
// So activityLiveTpsNone claims only what holds across all of them — no rate
// reported, none derivable yet — and names all three dependency axes (the upstream,
// what the client requested, and the operator's Responses live-timings switch)
// without asserting which applies. Note the second clause is about the DERIVATION,
// not about the count: on that last row an exact count exists, so a sentence denying
// one would be false there.
```

The enumeration higher up in the same comment still holds: the first replacement
rewords one of the four CAUSES, it does not add or remove one, so *"Those four
are CAUSES"* and the four-states cross-reference below it stay correct.

**11c.** `gateway/frontend/src/api/usage.ts`, the doc comment above
`ActiveRequest.output_tokens`, so the TypeScript side says what
`activeRequestDTO`'s Go comment says after Task 5:

```ts
  // Live per-request progress. `output_tokens` is the upstream's own cumulative
  // count of tokens GENERATED so far (0 = none reported) — never a count of what
  // this client has received and never a gateway estimate. For anthropic_messages
  // it is message_delta's usage.output_tokens; for openai_responses it is
  // llama.cpp's `timings.predicted_n` off whatever partial carries a `timings`
  // object, and then the terminal frame's own response.usage.output_tokens — so
  // the Responses figure climbs in jumps (partials without a `timings` object
  // skip values) and can sit a token or two below what the finished request
  // records. `tokens_per_second` is 0 when not measured, and
  // `tokens_per_second_source` says how it was obtained: 'upstream' (reported by
  // the inference server), 'gateway' (computed here from the upstream's exact
  // count), or '' (not measured). `ttft_ms` is 0 when not measured.
```

**11d.** `gateway/frontend/src/i18n.test.ts` again (same file as 11a, different
case): in `it('does not pin the missing rate on the upstream, because the absence
is client-dependent')`, the second paragraph explains the ban by calling the
server *"one of two dependency axes"* — the same two-item summary Step 10
retired. Replace the paragraph's tail, from *"false cause and still pass."* to the
end of the comment; the three assertions below it do not move:

```ts
    // false cause and still pass. The demonstrative is what does the blaming, so it is
    // what is banned — the honest string names the server as one of THREE dependency
    // axes, beside what the client requested and the operator's Responses live-timings
    // switch ("the upstream" in en, "vom Inferenzserver" in de, which uses that one
    // term throughout the sentence rather than mixing it with the anglicism), never
    // "this upstream" / "dieser Inferenzserver" as the reason. The German term is
    // banned in any declension, and the anglicism's ban stays so the guard does not go
    // quiet if the wording is ever swapped back to "Upstream".
```

**11e.** `gateway/frontend/src/i18n.test.ts`, in `it('keeps the not-measured
framing and names the client as a factor, in both locales')`: the comment's last
sentence carries the same two-axis claim about the client. Replace those two
lines:

```ts
    // from" would be false there. The client is named as one of the three dependency
    // axes — beside the upstream and the operator's Responses live-timings switch —
    // which is what stops the sentence reading as an upstream limitation.
```

That is every in-repo comment describing this string as a two-item list. Check it:
`grep -rn "two dependency\|both dependency" gateway/frontend/src` returns exactly
three hits before this step — `i18n.test.ts` twice (11d, 11e) and
`ActiveRequestsPanel.tsx` once (11b's second clause) — and nothing after it. (A
grep for the whole phrase `dependency axes` finds only one of the three: in
`i18n.test.ts` the phrase is split across a line break both times.)

Comments only, in three files; nothing runs differently.

- [ ] **Step 12: Format the five touched files**

Prettier owns formatting and CI runs its check, which `lint`, `build` and `test`
do not cover. Normalise the files this task touched rather than the whole tree:

```
cd gateway/frontend && npx prettier --write \
  src/i18n.ts \
  src/i18n.test.ts \
  src/api/usage.ts \
  src/components/ActiveRequestsPanel.tsx \
  src/components/ActiveRequestsPanel.test.tsx
```

Expected: five paths, each printed as `<path> <n>ms (unchanged)`. Every block
prescribed above was written against this repo's `.prettierrc` (printWidth 100,
single quotes, trailing commas) and prettier rewrites none of it — the multi-line
`<ActiveRequestsPanel …/>` calls and the two-line `activityLiveTpsNone` properties
are already its output. So a path printed WITHOUT `(unchanged)` means something
drifted while it was pasted; re-read the diff either way, so the committed code is
what you meant to write.

- [ ] **Step 13: Run the three non-test gates**

```
cd gateway/frontend && npm run format:check && npm run lint && npm run build
```

Expected: `All matched files use Prettier code style!`; eslint silent (exit 0);
`tsc` silent, then vite's build summary with no error. `build` is the gate that
would have caught a key added to `de` and not to `en`, because `en` is declared
as `PortalMessages` — neither `lint` nor `test` type-checks.

- [ ] **Step 14: Run the whole suite — the verdict**

```
cd gateway/frontend && npm run test
```

Expected: every test file passes. The single-file runs above were probes; this is
the only run that counts, and it is what proves the new column disturbs neither
`Activity.active.test.tsx` (which renders this panel inside the Activity view and
reads the elapsed column positionally as the last cell) nor the five existing
em-dash cases.

- [ ] **Final step: Commit**

```
git add gateway/frontend/src/i18n.ts \
        gateway/frontend/src/i18n.test.ts \
        gateway/frontend/src/api/usage.ts \
        gateway/frontend/src/components/ActiveRequestsPanel.tsx \
        gateway/frontend/src/components/ActiveRequestsPanel.test.tsx
```

```
git commit -F - <<'MSG'
feat: A live output-tokens column on the running-connections panel

The running-connections panel gains an optional column showing the upstream's
own cumulative count of tokens generated so far. Nothing new travels on the
wire: activeRequestDTO has carried output_tokens since the passthrough bridge
and the TypeScript ActiveRequest has always declared it, populated for
anthropic_messages from message_delta and, since timings.predicted_n is read,
mid-stream for openai_responses as well. This is a column that renders that
field, not a new field.

It is hidden by default. This panel renders a never-measured metric as a shared
em-dash, and an output_tokens of 0 means "the upstream reported none" rather
than "zero tokens", so a visible count column would put a second em-dash cell on
every unmeasured row -- which makes six singular em-dash assertions across five
existing cases ambiguous rather than wrong. That failure reads as "Found
multiple elements" and invites the wrong repair: loosening the query, which
would retire the invariant that a measured zero never renders as "0". The
operator who wants the count ticks it on in the column menu, which is asserted
here so that hidden never quietly becomes absent.

The cell goes through formatMetric, which supplies that same em-dash and doubles
as the sort accessor, so an unreported count sinks in both sort directions
instead of ranking as the smallest real value.

Four standing claims are corrected in the same change, all of them comments that
no check can see. Three go false the moment timings.predicted_n is read: two in
this panel's test file (one of them the parenthetical "the Responses partials
carry no usage", which was the stated reason a rate can arrive with no count --
the row shape survives, but now because a timings object carrying a rate and no
predicted_n reports no count), and one in i18n.test.ts repeating the same
sentence. The fourth is a clause of liveTpsTitle's own doc comment, which named
only the client as the party that can ask an upstream for timings; the
operator's switch can ask too.

For that same reason the not-measured tooltip stops presenting two dependency
axes as the whole list. It said the absence depends on the upstream and on what
the client requested; with the operator's Responses live-timings switch a row
can be empty because the switch is off while the client asked for nothing, and
can carry a rate no client ever asked for. Three more comments summarised that
sentence as a two-item list -- one closing liveTpsTitle's doc comment and two in
i18n.test.ts, each explaining an assertion in terms of "the two dependency axes"
-- and they are corrected with it.

No change was needed for the gateway-labelled mid-stream rate this feature makes
reachable on openai_responses: the tooltip already says the rate was computed
from a count the server reported, and timings.predicted_n is exactly that.
MSG
```

---

### Task 7: The portal control on both surfaces (spec D10)

**Files:**

- Modify: `gateway/frontend/src/i18n.ts` — four control strings and three
  error labels, in **both** the `de` and the `en` object.
- Modify: `gateway/frontend/src/i18n.test.ts` — one new `describe` block at
  the end of the file.
- Modify: `gateway/frontend/src/components/shared/format.ts` — three entries
  in `errorLabelByCode`.
- Modify: `gateway/frontend/src/components/shared/format.test.ts` — one new
  literal wire-code block inside the existing
  `describe('errorLabelByCode (whole-map invariants)')`.
- Create: `gateway/frontend/src/components/shared/liveTimings.ts`
- Create: `gateway/frontend/src/components/shared/liveTimings.test.ts`
- Modify: `gateway/frontend/src/components/shared/ApiVariantControls.tsx` —
  three new required props and the control, inside `ApiVariantControls`.
- Modify: `gateway/frontend/src/components/shared/ApiVariantControls.test.tsx`
  — `Harness` and the one direct `render(<ApiVariantControls …>)` in
  `'reports the toggled flavor list'`, plus three new cases.
- Modify: `gateway/frontend/src/api/models.ts` — `PortalApplication`,
  `CreateApplicationRequest`, `UpdateApplicationRequest`.
- Modify: `gateway/frontend/src/api/runtime.ts` — `RuntimeSpec` and
  `PutRuntimeSpecRequest`.
- Modify: `gateway/frontend/src/components/ApplicationSection.tsx` — the
  `liveTimings` state, `openCreate`, `openEdit`, `buildBody`, and the
  `ApiVariantControls` call.
- Modify: `gateway/frontend/src/components/ApplicationSection.test.tsx` —
  `makeApp`, `makeRuntimeSpec`, plus a new `describe`.
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.tsx` —
  `emptySpec`, `specBodyWithAdminState`, the `specLiveTimings` state,
  `resetSpecFields`, `hydrateSpecFields`, `buildSpecBody`, and the
  `ApiVariantControls` call.
- Modify: `gateway/frontend/src/components/RuntimeAdminSection.test.tsx` —
  the module-level `application` literal, `makeSpec`, plus a new `describe`.
- Modify (fixtures only, one field each):
  `gateway/frontend/src/App.test.tsx` (the `const application: PortalApplication`
  inside the fake create handler, **and** the five row literals in the four
  `apiState.applicationRows = [ … ]` assignments),
  `gateway/frontend/src/components/ServerList.test.tsx` (`defaultApplication`,
  `defaultRuntimeSpec`),
  `gateway/frontend/src/components/MappingSection.test.tsx` (the module-level
  `application` literal),
  `gateway/frontend/src/components/BenchmarkSection.test.tsx` (`makeApp`).

That fixture list is exhaustive and was counted: **six** files declare a whole
`PortalApplication` (`App.test.tsx` ×6 literals, `ApplicationSection.test.tsx`
`makeApp`, `RuntimeAdminSection.test.tsx` `application`,
`ServerList.test.tsx` `defaultApplication`, `MappingSection.test.tsx`
`application`, `BenchmarkSection.test.tsx` `makeApp`) and **four** places
declare a whole `RuntimeSpec` (`RuntimeAdminSection.tsx` `emptySpec` —
production; `ApplicationSection.test.tsx` `makeRuntimeSpec`;
`RuntimeAdminSection.test.tsx` `makeSpec`; `ServerList.test.tsx`
`defaultRuntimeSpec`). `RuntimeAdminSection.test.tsx`'s `fullSpec` delegates
to `makeSpec` and needs no edit.

**Do NOT edit anything under `docs/` in this task.** Two canonical sentences
expire with this cut and belong to the documentation task, which owns the
whole set: the *"No operator control for it ships in this cut either … a
visible toggle that did nothing would be worse than the blank cell it promises
to fix"* paragraph in
`docs/architecture/cross-cutting/agent-runtime-manager.md`, and the
`responses_live_timings_enabled` bullet list in
`docs/architecture/reference/api-surface.md`.

**Interfaces:**

Consumes (from Task 1, the routing/D6 change): `routing.LiveTimingsCapableKind`
is true for `"llama_cpp"` **alone** — `"vllm"` has left the set. The
TypeScript mirror written in Step 3 must match that, not the pre-Task-1 set.
Nothing else from Tasks 2–6 is consumed: this task touches no Go file.

Consumes (already on `main`, the part-1 wire contract, verified against the Go
source, not guessed):

- `portal.ApplicationDTO.ResponsesLiveTimingsEnabled bool` →
  `json:"responses_live_timings_enabled"`
- `portal.CreateApplicationRequest.ResponsesLiveTimingsEnabled *bool` →
  `json:"responses_live_timings_enabled,omitempty"`
- `portal.UpdateApplicationRequest.ResponsesLiveTimingsEnabled *bool` →
  same tag
- `portal.RuntimeSpecDTO.ResponsesLiveTimingsEnabled bool` → same key
- `portal.PutRuntimeSpecRequest.ResponsesLiveTimingsEnabled *bool` →
  `json:"responses_live_timings_enabled,omitempty"`
- the three wire codes, read verbatim from the `errRow` tables in
  `gateway/backend/internal/gateway/portal_application_endpoints.go` and
  `portal_runtime_endpoints.go`:
  `application.responses_live_timings_unsupported` (400),
  `application.responses_live_timings_conflict` (409),
  `runtime_spec.responses_live_timings_unsupported` (400).

Produces (no later task in this plan consumes them; listed so a reviewer can
check the seams):

```ts
// gateway/frontend/src/components/shared/liveTimings.ts
export type LiveTimingsKind = 'capable' | 'incapable' | 'unknown';
export function applicationLiveTimingsKind(type: ApplicationType): LiveTimingsKind;
export function runtimeSpecLiveTimingsKind(specType: RuntimeSpec['type']): LiveTimingsKind;

// gateway/frontend/src/components/shared/ApiVariantControls.tsx — three added props
liveTimings: boolean | undefined;
liveTimingsKind: LiveTimingsKind;
onLiveTimingsChange: (enabled: boolean) => void;

// gateway/frontend/src/api/models.ts
PortalApplication.responses_live_timings_enabled: boolean;
CreateApplicationRequest.responses_live_timings_enabled?: boolean;
UpdateApplicationRequest.responses_live_timings_enabled?: boolean;

// gateway/frontend/src/api/runtime.ts
RuntimeSpec.responses_live_timings_enabled: boolean;
PutRuntimeSpecRequest.responses_live_timings_enabled?: boolean;  // Omit-ed out, re-declared
```

**The rule the whole task implements, stated once.** Each form sends
`responses_live_timings_enabled` **only when it would not contradict the
document it is sending**:

| form state | what the body carries | why |
|---|---|---|
| kind `incapable` | key **omitted** | an explicit `true` here is the 400; omitting is what the backend normalises (create → per-kind default `false`; update → the clear arm drops a stale `true`) |
| kind `capable` | the control's value | both values are accepted for a capable kind |
| kind `unknown` (spec form, Type = Auto) and value `undefined` | key **omitted** | no opinion; the backend's first-write default or the stored value decides |
| kind `unknown` and value definite | the control's value | the operator asserted; a `true` against a non-llama.cpp binary earns the documented 400, now legible in German |

The application form never reaches `unknown` (it always knows `type`), and its
state is never `undefined`.

**Precondition: the frontend toolchain must be installed in THIS worktree.**
Every command in this task — `npm test`, `npm run build`, `npm run lint`,
`npm run format` — runs `vitest` / `tsc` / `vite` / `eslint` / `prettier` out of
`gateway/frontend/node_modules`, which is gitignored (`.gitignore` line
`gateway/frontend/node_modules/`) and therefore exists per worktree, not per
repository. Check once, before Step 1:

```
cd gateway/frontend && ls node_modules/.bin/vitest
```

If that path is missing, run `npm ci` in `gateway/frontend` once and then
proceed. If it is there, install nothing: this task adds no dependency, so a
re-install can only cost time.

**Two notes before you start.**

1. `vitest` runs through esbuild and **does not typecheck**. A type error
   surfaces only under `npm run build` (`tsc && vite build`). Several steps
   below leave the tree type-incomplete between the red and the green; that is
   expected. Run `npm run build` **only** where a step tells you to.
2. Let `prettier` settle all quoting and line wrapping: write the strings
   however is readable, then run `npm run format` before the final gate. Do
   not hand-wrap long message strings.

---

- [ ] **Step 1: the seven i18n strings, in both locales**

Write the failing test. Append this `describe` at the **end** of
`gateway/frontend/src/i18n.test.ts` (after the last closing `});`):

```ts
// Responses live-timings (issue #81 part 2, design D10): the shared
// API-variant block's own checkbox strings plus the three backend refusals
// part 1 introduced. The codes are read verbatim from the Go errRow tables in
// gateway/portal_application_endpoints.go and portal_runtime_endpoints.go;
// see format.test.ts for the map side.
describe('responses live-timings i18n keys', () => {
  it('defines the control + error-code keys in de and en', () => {
    const keys = [
      'applicationLiveTimings',
      'applicationLiveTimingsNote',
      'applicationLiveTimingsUnsupportedNote',
      'applicationLiveTimingsAutoNote',
      'errorApplicationResponsesLiveTimingsUnsupported',
      'errorApplicationResponsesLiveTimingsConflict',
      'errorRuntimeSpecResponsesLiveTimingsUnsupported',
    ] as const;
    for (const k of keys) {
      expect(typeof messages.de[k]).toBe('string');
      expect(typeof messages.en[k]).toBe('string');
      expect(messages.de[k].length).toBeGreaterThan(0);
      expect(messages.en[k].length).toBeGreaterThan(0);
    }
  });

  // The two application refusals answer DIFFERENT questions -- the 400 says
  // the body you just sent is contradictory, the 409 says this application's
  // STORED type cannot honour it -- so one shared sentence would tell half
  // the operators the wrong remedy.
  it('gives the 400 and the 409 application refusals different sentences', () => {
    for (const locale of ['de', 'en'] as const) {
      expect(messages[locale].errorApplicationResponsesLiveTimingsUnsupported).not.toBe(
        messages[locale].errorApplicationResponsesLiveTimingsConflict,
      );
    }
  });
});
```

Run it:

```
cd gateway/frontend && npm test -- src/i18n.test.ts
```

Expected: the first case fails on the first key —
`expected 'undefined' to be 'string'` — at the
`expect(typeof messages.de[k]).toBe('string')` line. (The second case fails
too, `expected undefined not to be undefined`.)

Implement. In `src/i18n.ts`, in the **`de`** object, insert the four control
strings immediately after the `applicationNativeNote` entry (the shared
block's existing caption) and before `applicationResponsesMode`:

```ts
  // Der Live-Timings-Schalter im gemeinsamen API-Varianten-Block. Beide
  // Formulare rendern denselben Block, deshalb tragen alle vier Texte --
  // wie applicationFlavors/applicationNativeNote daneben -- das
  // application*-Präfix, auch wenn der Auto-Hinweis nur auf der
  // Startvorgaben-Seite erscheint.
  applicationLiveTimings: 'Live-Timings vom Upstream anfordern',
  applicationLiveTimingsNote:
    'Setzt bei gestreamten Codex-Anfragen (/v1/responses) den llama.cpp-Parameter „timings_per_token“, damit die Laufenden Verbindungen eine vom Upstream gemeldete Tokens/s für die gesamte Anfrage zeigen statt einer hier abgeleiteten. Solange dies aktiv ist, ist der aufgezeichnete Anfragetext nicht mehr exakt der gesendete.',
  applicationLiveTimingsUnsupportedNote:
    'Live-Timings sind nur für llama.cpp verfügbar. Für diesen Typ wird der Parameter nicht gesetzt, und ein noch gespeichertes Ja wird beim nächsten Speichern gelöscht.',
  applicationLiveTimingsAutoNote:
    'Der Typ steht auf „Automatisch“: welche Art Server der Programmpfad startet, erkennt erst das Gateway. Ohne Häkchen entscheidet es selbst — eine neu angelegte llama.cpp-Startvorgabe bekommt Live-Timings, jede andere nicht, und eine bestehende Startvorgabe behält ihren gespeicherten Wert. Ein Häkchen auf einer Startvorgabe, die nicht llama.cpp ist, wird beim Speichern abgelehnt.',
```

…and the three error labels immediately after
`errorRuntimeSpecApplicationNotServerAgent` in the same object:

```ts
  errorApplicationResponsesLiveTimingsUnsupported:
    'Live-Timings sind für diesen Anwendungstyp nicht verfügbar; nur llama.cpp kann sie liefern. Die Meldung nennt den gesendeten Typ.',
  errorApplicationResponsesLiveTimingsConflict:
    'Diese Anwendung hat einen Typ, der keine Live-Timings liefern kann. Ändern Sie zuerst den Typ, oder lassen Sie die Einstellung aus.',
  errorRuntimeSpecResponsesLiveTimingsUnsupported:
    'Live-Timings sind für die Art dieses Servers nicht verfügbar; nur llama.cpp kann sie liefern. Die Meldung nennt die erkannte Art.',
```

Then the mirrors in the **`en`** object, at the matching positions (after
`applicationNativeNote`, and after `errorRuntimeSpecApplicationNotServerAgent`):

```ts
  // The live-timings switch on the shared API-variant block. Both forms
  // render that block, so all four strings carry the application* prefix its
  // neighbours already use, even though the Auto hint only ever shows on the
  // launch-spec side.
  applicationLiveTimings: 'Ask the upstream for live timings',
  applicationLiveTimingsNote:
    'Sets llama.cpp\'s "timings_per_token" on streamed Codex requests (/v1/responses), so Running connections show a tokens/sec the upstream reports for the whole request instead of one derived here. While this is on, the recorded request body is no longer exactly the body that was sent.',
  applicationLiveTimingsUnsupportedNote:
    'Live timings are available for llama.cpp only. For this type the parameter is not set, and a stored yes is cleared on the next save.',
  applicationLiveTimingsAutoNote:
    'Type is set to "Auto", so which kind of server the binary launches is only detected by the gateway. Left unticked, the gateway decides — a newly created llama.cpp launch spec gets live timings, any other kind does not, and an existing spec keeps its stored value. Ticking it on a spec that is not llama.cpp is refused on save.',
```

```ts
  errorApplicationResponsesLiveTimingsUnsupported:
    'Live timings are not available for this application type; only llama.cpp can report them. The message names the type that was sent.',
  errorApplicationResponsesLiveTimingsConflict:
    'This application has a type that cannot report live timings. Change the type first, or leave the setting off.',
  errorRuntimeSpecResponsesLiveTimingsUnsupported:
    'Live timings are not available for this runtime kind; only llama.cpp can report them. The message names the detected kind.',
```

Re-run `npm test -- src/i18n.test.ts`: green.

Revert-proof (production file only): delete the
`errorApplicationResponsesLiveTimingsConflict` line from the `de` object in
`src/i18n.ts`. `npm run build` fails first (`en` is typed `PortalMessages`),
so instead delete it from **both** objects — the test then fails with
`expected 'undefined' to be 'string'`.

Commit: `src/i18n.ts`, `src/i18n.test.ts` —
`feat(portal): add the responses live-timings control and refusal strings in de and en`

---

- [ ] **Step 2: map the three refusal codes**

Write the failing test. Inside the existing
`describe('errorLabelByCode (whole-map invariants)')` in
`gateway/frontend/src/components/shared/format.test.ts`, immediately **after**
the `it('carries every VRAM-benchmark refusal code, by its exact wire string', …)`
case, add:

```ts
  /**
   * Part 1's three live-timings refusals, pinned as LITERALS for the same
   * reason the VRAM codes above are: the whole-map invariants cannot catch a
   * code STRING drifting from the backend's, and an unmapped code is not an
   * error reported badly -- `formatPortalError` falls back to the raw English
   * the server sent, in a portal that is otherwise fully localized.
   *
   * Declared in Go as the sentinels
   * `portal.ErrApplicationResponsesLiveTimingsUnsupported` /
   * `...Conflict` / `portal.ErrRuntimeSpecResponsesLiveTimingsUnsupported`,
   * wired to these exact codes in the `errRow` tables in
   * `internal/gateway/portal_application_endpoints.go` and
   * `portal_runtime_endpoints.go`. Two application codes, not one: the same
   * refusal answers 400 when the request supplied the incapable type and 409
   * when the type came from the stored row.
   */
  const liveTimingsWireCodes = [
    'application.responses_live_timings_unsupported',
    'application.responses_live_timings_conflict',
    'runtime_spec.responses_live_timings_unsupported',
  ] as const;

  it('carries every live-timings refusal code, by its exact wire string', () => {
    for (const code of liveTimingsWireCodes) {
      expect(
        errorLabelByCode[code],
        `${code} is not mapped: the operator sees raw English`,
      ).toBeDefined();
    }
    // Both directions, like the VRAM list above: a fourth
    // `*.responses_live_timings_*` code added to the map without being named
    // here fails too.
    expect(
      entries
        .filter(([code]) => code.includes('responses_live_timings'))
        .map(([code]) => code)
        .sort(),
    ).toEqual(liveTimingsWireCodes.slice().sort());
  });
```

Run it:

```
cd gateway/frontend && npm test -- src/components/shared/format.test.ts
```

Expected: fails on the first loop iteration with
`application.responses_live_timings_unsupported is not mapped: the operator sees raw English`
(`expected undefined to be defined`).

Implement. In `gateway/frontend/src/components/shared/format.ts`, inside
`errorLabelByCode`, insert after the
`'runtime_spec.application_not_server_agent': 'errorRuntimeSpecApplicationNotServerAgent',`
entry:

```ts
  // Issue #81 part 2 (design D10): the three refusals part 1 introduced. THREE
  // distinct labels on purpose -- the whole-map "reuses a label for two codes
  // only where that is deliberate" invariant below fails on a shared one, and
  // it would be right to: the 400 tells the operator their own body is
  // contradictory, the 409 tells them this application's stored type is, and
  // the runtime_spec one names a kind that may have been DETECTED rather than
  // typed. Read verbatim from the Go errRow tables, not guessed.
  'application.responses_live_timings_unsupported':
    'errorApplicationResponsesLiveTimingsUnsupported',
  'application.responses_live_timings_conflict': 'errorApplicationResponsesLiveTimingsConflict',
  'runtime_spec.responses_live_timings_unsupported':
    'errorRuntimeSpecResponsesLiveTimingsUnsupported',
```

Re-run: green. Note the existing
`it('reuses a label for two codes only where that is deliberate')` case must
stay green — it does, because the three labels are distinct.

Revert-proof (production file only): delete the
`'runtime_spec.responses_live_timings_unsupported'` entry from
`errorLabelByCode`. Fails with
`runtime_spec.responses_live_timings_unsupported is not mapped: the operator sees raw English`,
and the both-directions assertion fails too.

Commit: `src/components/shared/format.ts`,
`src/components/shared/format.test.ts` —
`feat(portal): map the three responses live-timings refusal codes to localized labels`

---

- [ ] **Step 3: the shared capability predicate**

Write the failing test. Create
`gateway/frontend/src/components/shared/liveTimings.test.ts`:

```ts
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import { describe, expect, it } from 'vitest';
import { applicationLiveTimingsKind, runtimeSpecLiveTimingsKind } from './liveTimings';
import type { ApplicationType, RuntimeSpec } from '../../api';

// Both lists are stated EXHAUSTIVELY and are the whole point of the test: the
// Go set (routing.liveTimingsCapableKinds, read by
// routing.LiveTimingsCapableKind) is a map this module hand-copies, and
// nothing compiles the two together. A kind added to the Go set without a
// matching edit here is a silent portal that never offers the switch; a kind
// left here after Go drops it is a portal that offers a switch every save
// refuses. Naming every member in both directions is what makes either
// direction fail loudly.
const allApplicationTypes: ApplicationType[] = [
  'ollama',
  'vllm',
  'llama_cpp',
  'llama_swap',
  'litellm',
  'server_agent',
];

const allSpecTypes: RuntimeSpec['type'][] = ['', 'vllm', 'llama_cpp', 'tgi', 'ollama', 'custom'];

describe('applicationLiveTimingsKind', () => {
  it('calls llama_cpp capable and every other application type incapable', () => {
    expect(applicationLiveTimingsKind('llama_cpp')).toBe('capable');
    for (const type of allApplicationTypes.filter((t) => t !== 'llama_cpp')) {
      expect(applicationLiveTimingsKind(type), type).toBe('incapable');
    }
  });

  // vLLM was in the capable set in part 1 and left it in part 2 (design D6):
  // the key is a llama.cpp parameter and was measured completely inert on
  // vLLM's /v1/responses -- 48 frames, zero carrying `timings`. A switch that
  // is offered and provably delivers nothing is worse than one that is not.
  it('does NOT call vllm capable', () => {
    expect(applicationLiveTimingsKind('vllm')).toBe('incapable');
  });

  // The application form always knows its own `type`, so it must never see
  // the third state; `unknown` belongs to the launch-spec form alone.
  it('never answers unknown, for any application type', () => {
    for (const type of allApplicationTypes) {
      expect(applicationLiveTimingsKind(type), type).not.toBe('unknown');
    }
  });
});

describe('runtimeSpecLiveTimingsKind', () => {
  it('answers unknown for Auto: the kind is detected from the binary, and that detection is Go-only', () => {
    expect(runtimeSpecLiveTimingsKind('')).toBe('unknown');
  });

  it('answers capable for an explicit llama_cpp and incapable for every other explicit kind', () => {
    expect(runtimeSpecLiveTimingsKind('llama_cpp')).toBe('capable');
    for (const specType of allSpecTypes.filter((s) => s !== '' && s !== 'llama_cpp')) {
      expect(runtimeSpecLiveTimingsKind(specType), specType).toBe('incapable');
    }
  });
});
```

Run it:

```
cd gateway/frontend && npm test -- src/components/shared/liveTimings.test.ts
```

Expected: the file fails to load —
`Error: Failed to resolve import "./liveTimings" from "src/components/shared/liveTimings.test.ts". Does the file exist?`

Implement. Create
`gateway/frontend/src/components/shared/liveTimings.ts`:

```ts
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

import type { ApplicationType, RuntimeSpec } from '../../api';

/**
 * What a form knows about whether its upstream can honour
 * `responses_live_timings_enabled`.
 *
 * Three states, not two, because the launch-spec form genuinely has a third:
 * with Type on "Auto" the effective kind is detected from the binary's
 * basename, and that detection (routing.DetectRuntimeSpecType) is Go-only.
 * Mirroring it here would be a second, uncompiled copy of a matching rule --
 * exactly the drift this file's own list is already at risk of -- so Auto
 * stays UNKNOWN and the form sends no opinion rather than a guess.
 *
 *   capable   -- the form may send either value
 *   incapable -- the form must send NO key at all. An explicit true here is
 *                refused (application.responses_live_timings_unsupported /
 *                runtime_spec.responses_live_timings_unsupported), and since
 *                both forms restate `type` on every save, that refusal blocks
 *                the WHOLE save rather than just the flag. Omitting instead is
 *                what the backend normalises: a create takes the kind's own
 *                default and an update clears a stale true.
 *   unknown   -- only the launch-spec form, only under Auto.
 */
export type LiveTimingsKind = 'capable' | 'incapable' | 'unknown';

/**
 * The kinds whose request schema is known to tolerate `timings_per_token`.
 *
 * A HAND-COPY of `liveTimingsCapableKinds` in
 * `gateway/backend/internal/routing/live_timings.go`, which is the authority.
 * Nothing compiles the two together, so the exhaustive both-directions test in
 * liveTimings.test.ts is the only guard -- edit the two together.
 *
 * The Go map is keyed on the string BOTH vocabularies share
 * (ProviderLlamaCPP == "llama_cpp" == RuntimeSpecTypeLlamaCpp), which is why
 * one list serves an application `type` and a spec `type` alike.
 */
const liveTimingsCapableKinds: readonly string[] = ['llama_cpp'];

/** The application form's gate: it always knows its own `type`. */
export function applicationLiveTimingsKind(type: ApplicationType): LiveTimingsKind {
  return liveTimingsCapableKinds.includes(type) ? 'capable' : 'incapable';
}

/**
 * The launch-spec form's gate, from the WRITABLE Type select alone -- not from
 * the read-only `effective_type` echo, which is undefined on create and stale
 * the moment `binary` is edited. An explicit Type is the first branch of
 * routing.EffectiveRuntimeSpecType, so when it is set the form knows the
 * effective kind exactly; only "" (Auto) falls through to the Go-only
 * detection, and that is the one case answered `unknown`.
 */
export function runtimeSpecLiveTimingsKind(specType: RuntimeSpec['type']): LiveTimingsKind {
  if (specType === '') return 'unknown';
  return liveTimingsCapableKinds.includes(specType) ? 'capable' : 'incapable';
}
```

Re-run: green.

Revert-proof (production file only): change
`const liveTimingsCapableKinds: readonly string[] = ['llama_cpp'];` to
`['vllm']`. **Three** cases go red — both exported functions read the same
list, so a swap of its one member reds the spec side as well as the
application side:

- `expected 'incapable' to be 'capable'` in
  `'calls llama_cpp capable and every other application type incapable'`
  (first assertion);
- `expected 'capable' to be 'incapable'` in `'does NOT call vllm capable'`;
- `expected 'incapable' to be 'capable'` in
  `'answers capable for an explicit llama_cpp and incapable for every other explicit kind'`
  (first assertion — its loop over the other explicit kinds is never reached).

The remaining two cases stay green, and correctly so: `'never answers unknown,
for any application type'` and `'answers unknown for Auto…'` pin the shape of
the third state, not the membership of the list.

Commit: `src/components/shared/liveTimings.ts`,
`src/components/shared/liveTimings.test.ts` —
`feat(portal): add the live-timings capability predicate for both portal forms`

---

- [ ] **Step 4: the control on the shared API-variant block**

Write the failing tests. In
`gateway/frontend/src/components/shared/ApiVariantControls.test.tsx`:

first extend `Harness` so it owns the new state (the component is controlled):

```tsx
function Harness({
  flavors = ['openai', 'anthropic'],
  responses = 'passthrough',
  msgs = 'passthrough',
  // NO default value here, deliberately: a default would fire for an
  // EXPLICIT `timings={undefined}` too, and `undefined` is the value one of
  // the cases below is about.
  timings,
  timingsKind = 'capable',
}: {
  flavors?: string[];
  responses?: EndpointMode;
  msgs?: EndpointMode;
  timings?: boolean;
  timingsKind?: LiveTimingsKind;
}) {
  const [apiFlavors, setApiFlavors] = useState<string[]>(flavors);
  const [responsesMode, setResponsesMode] = useState<EndpointMode>(responses);
  const [messagesMode, setMessagesMode] = useState<EndpointMode>(msgs);
  const [liveTimings, setLiveTimings] = useState<boolean | undefined>(timings);
  return (
    <ApiVariantControls
      t={t}
      apiFlavors={apiFlavors}
      responsesMode={responsesMode}
      messagesMode={messagesMode}
      liveTimings={liveTimings}
      liveTimingsKind={timingsKind}
      onFlavorsChange={setApiFlavors}
      onResponsesModeChange={setResponsesMode}
      onMessagesModeChange={setMessagesMode}
      onLiveTimingsChange={setLiveTimings}
    />
  );
}
```

add the import `import type { LiveTimingsKind } from './liveTimings';` beside
the existing `EndpointMode` import, add the three props to the one direct
`render(<ApiVariantControls …>)` inside
`it('reports the toggled flavor list', …)`:

```tsx
        liveTimings={false}
        liveTimingsKind="capable"
        onLiveTimingsChange={() => {}}
```

and append these three cases inside the existing `describe('ApiVariantControls')`:

```tsx
  const liveTimingsBox = () =>
    screen.getByRole('checkbox', { name: t.applicationLiveTimings });

  it('renders the live-timings checkbox for a capable kind and reports a toggle', () => {
    render(<Harness timings={false} timingsKind="capable" />);
    expect(liveTimingsBox()).not.toBeChecked();
    expect(screen.getByText(t.applicationLiveTimingsNote)).toBeInTheDocument();
    fireEvent.click(liveTimingsBox());
    expect(liveTimingsBox()).toBeChecked();
  });

  it('offers no checkbox at all for an incapable kind, and says why instead', () => {
    render(<Harness timings={false} timingsKind="incapable" />);
    expect(
      screen.queryByRole('checkbox', { name: t.applicationLiveTimings }),
    ).not.toBeInTheDocument();
    expect(screen.getByText(t.applicationLiveTimingsUnsupportedNote)).toBeInTheDocument();
  });

  // "No opinion" (undefined) is not a third rendering -- it is the value the
  // BACKEND will pick, shown honestly. On a kind the form knows is capable
  // that value is ON (the documented create default), so the box shows
  // ticked; on an unknown kind nothing can be promised, so the box shows
  // unticked and the Auto caption is what states the rule.
  it('shows no-opinion as the default the backend will apply, per kind', () => {
    render(<Harness timings={undefined} timingsKind="capable" />);
    expect(liveTimingsBox()).toBeChecked();
    cleanup();
    render(<Harness timings={undefined} timingsKind="unknown" />);
    expect(liveTimingsBox()).not.toBeChecked();
    expect(screen.getByText(t.applicationLiveTimingsAutoNote)).toBeInTheDocument();
  });
```

Run it:

```
cd gateway/frontend && npm test -- src/components/shared/ApiVariantControls.test.tsx
```

Expected: all three new cases fail, and the five pre-existing cases stay green
(the added props are inert until the component reads them).

- Case one, first assertion:
  `TestingLibraryElementError: Unable to find an accessible element with the role "checkbox" and name "Live-Timings vom Upstream anfordern"`.
- Case two, **second** assertion:
  `Unable to find an element with the text: Live-Timings sind nur für llama.cpp verfügbar…`.
  Its first assertion passes vacuously right now — there is no checkbox
  anywhere yet — and becomes real once the component renders one; the
  revert-proof below is what exercises it.
- Case three, first assertion: the same "role checkbox" error as case one.

Implement. In
`gateway/frontend/src/components/shared/ApiVariantControls.tsx`:

extend the import to
`import { Checkbox, FormControlLabel, Typography } from '@mui/material';`, add
`import type { LiveTimingsKind } from './liveTimings';` beside the
`EndpointMode` type import, add the three props to the signature and the
`Readonly<{…}>` type:

```tsx
  liveTimings,
  liveTimingsKind,
  …
  onLiveTimingsChange,
```

```tsx
  // undefined = "no opinion": the FORM omits the key entirely and the backend
  // decides -- a first write takes the kind's own default, a later save keeps
  // the stored value. The launch-spec form needs that third state because
  // under Type "Auto" it cannot know the kind; the application form always
  // knows its `type` and never passes it.
  liveTimings: boolean | undefined;
  liveTimingsKind: LiveTimingsKind;
  onLiveTimingsChange: (enabled: boolean) => void;
```

and render this **after** the existing `applicationNativeNote` `Typography`,
still inside the fragment:

```tsx
      {liveTimingsKind === 'incapable' ? (
        // Never a blank: the slot the checkbox would occupy still explains why
        // there is nothing to set. Rendering a DISABLED checkbox instead would
        // be worse -- it would show a value (ticked or not) that this form
        // will not send, since an incapable kind omits the key altogether.
        <Typography variant="caption" sx={{ color: 'text.secondary' }}>
          {t.applicationLiveTimingsUnsupportedNote}
        </Typography>
      ) : (
        <>
          <FormControlLabel
            control={
              <Checkbox
                // `?? liveTimingsKind === 'capable'` is the honest rendering of
                // "no opinion", not a default: what the backend will apply for
                // a capable kind on a first write is ON, so the box says ON.
                // For an unknown kind nothing can be promised and the caption
                // below is what states the rule. Clicking either way leaves a
                // DEFINITE value, and there is deliberately no way back to "no
                // opinion" once the operator has said something.
                checked={liveTimings ?? liveTimingsKind === 'capable'}
                onChange={(e) => onLiveTimingsChange(e.target.checked)}
              />
            }
            label={t.applicationLiveTimings}
          />
          <Typography variant="caption" sx={{ color: 'text.secondary' }}>
            {liveTimingsKind === 'unknown'
              ? t.applicationLiveTimingsAutoNote
              : t.applicationLiveTimingsNote}
          </Typography>
        </>
      )}
```

Re-run: green (all eight cases).

Revert-proof (production file only), two independent mutations:

1. `checked={liveTimings ?? liveTimingsKind === 'capable'}` →
   `checked={liveTimings === true}`. The third new case fails with
   `expected element to be checked` (received unchecked) on the capable /
   undefined half.
2. Replace the `liveTimingsKind === 'incapable'` condition with `false`. The
   second case fails on its **first** assertion —
   `expected <input … /> not to be in the document` — because the else branch
   now renders the checkbox and `queryByRole` returns it. Its second
   assertion, the one about the caption, is never reached; that assertion is
   what was red before the implementation, this one is what proves the gate.

Commit: `src/components/shared/ApiVariantControls.tsx`,
`src/components/shared/ApiVariantControls.test.tsx` —
`feat(portal): render the responses live-timings checkbox on the shared API-variant block`

---

- [ ] **Step 5: the API types, the fixtures, and the override-PUT omission**

This step has no new test: it is pinned by six **existing** assertions.

First, the types.

In `gateway/frontend/src/api/models.ts`, add to `PortalApplication`
immediately after the `messages_mode: EndpointMode;` line:

```ts
  // The operator's opt-in to asking a capable upstream (llama.cpp only) for
  // live per-token timings on streamed /v1/responses. Always present on read;
  // the two request shapes below make it optional, because absent there means
  // something a `false` cannot say.
  responses_live_timings_enabled: boolean;
```

add to **both** `CreateApplicationRequest` and `UpdateApplicationRequest`,
after their own `messages_mode?: EndpointMode;` lines:

```ts
  // Absent is NOT the same as false. Absent on a create takes the type's own
  // default (true for llama_cpp, false otherwise); absent on an update keeps
  // the stored value, except that the backend CLEARS a stored true whenever
  // the resulting type cannot honour it. An explicit true against an
  // incapable type is refused outright -- 400 when the body also carries
  // `type` (which this form's buildBody always does), 409 when it does not --
  // and that refusal fails the WHOLE save, so the form omits the key rather
  // than ever sending an impossible pair. See ApplicationSection's buildBody.
  responses_live_timings_enabled?: boolean;
```

In `gateway/frontend/src/api/runtime.ts`, add to `RuntimeSpec` immediately
after `messages_mode: EndpointMode;`:

```ts
  // This spec's OWN live-timings opt-in (not inherited from the parent
  // application, and it is the spec's copy that wins for a server_agent
  // model). Always present on read; on the request shape below it is optional
  // and that is load-bearing -- see PutRuntimeSpecRequest.
  responses_live_timings_enabled: boolean;
```

and change `PutRuntimeSpecRequest` to exclude it and re-declare it optional:

```ts
export type PutRuntimeSpecRequest = Omit<
  RuntimeSpec,
  | 'configured'
  | 'id'
  | 'mapping_id'
  | 'api_token_set'
  | 'app_api_token_set'
  | 'app_api_token_header'
  | 'effective_type'
  | 'resolved_metrics_path'
  | 'resolved_context_probe_path'
  | 'responses_live_timings_enabled'
> & {
  // Write-only: undefined/absent or null = keep the stored token; '' = clear;
  // a value = replace-and-seal (set mode).
  api_token?: string | null;
  // Force regeneration of the random-mode token on save.
  api_token_rotate?: boolean;
  // OPTIONAL, and excluded from the Omit above for exactly that reason rather
  // than as a tidy-up. The Go field is a *bool with omitempty, and nil there
  // means "no opinion": a FIRST write then takes the effective kind's own
  // default and a later save keeps the stored value. Inherited through Omit
  // it would arrive here as RuntimeSpec's REQUIRED boolean, every caller
  // would have to state an opinion it may not have, and the third state part
  // 1 built would be gone. An explicit true whose effective kind is not
  // llama_cpp is refused with 400 runtime_spec.responses_live_timings_unsupported
  // and fails the whole upsert.
  responses_live_timings_enabled?: boolean;
};
```

Now the fixtures. Add `responses_live_timings_enabled: false,` to every
`PortalApplication` literal — `src/App.test.tsx` (the
`const application: PortalApplication` in the fake create handler, where it
reads `responses_live_timings_enabled: body.responses_live_timings_enabled ?? false,`
and the request-shape type alias just above it gains
`responses_live_timings_enabled?: boolean;`; plus the five row literals in the
four `apiState.applicationRows = [ … ]` assignments),
`src/components/ApplicationSection.test.tsx` (`makeApp`),
`src/components/RuntimeAdminSection.test.tsx` (the module-level `application`),
`src/components/ServerList.test.tsx` (`defaultApplication`),
`src/components/MappingSection.test.tsx` (the module-level `application`),
`src/components/BenchmarkSection.test.tsx` (`makeApp`).

Add `responses_live_timings_enabled: false,` to every `RuntimeSpec` literal —
`src/components/RuntimeAdminSection.tsx`'s `emptySpec` (a never-configured
spec is every field at its zero value, and `false` is that zero value),
`src/components/ApplicationSection.test.tsx`'s `makeRuntimeSpec`,
`src/components/RuntimeAdminSection.test.tsx`'s `makeSpec`, and
`src/components/ServerList.test.tsx`'s `defaultRuntimeSpec`.

Now run the tests and watch existing assertions fail:

```
cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx
```

Expected: **five failing cases** carrying six `expectedBody` assertions
between them (one case has two) — `'force-stop PUTs the loaded spec verbatim
with ONLY admin_state changed'`, `'clear-override PUTs the loaded spec
verbatim with admin_state emptied'`, `'runs force_stopped -> await stopped ->
clear override, preserving every other spec field'`, `'does not abort on a
transient empty frame: the row coming back resumes the sequence'`, and
`'does not complete off a \`stopped\` frame that predates its own
force_stopped write'`. Each reports
`expected { …, responses_live_timings_enabled: false } to deeply equal { … }`
from `expect(putSpecs[n].body).toEqual(expectedBody(spec, …))`. Cause:
`specBodyWithAdminState` builds its body by rest-spreading the loaded
`RuntimeSpec`, so the new field now rides along, while the test's
`expectedBody` helper enumerates the PUT fields explicitly and does not carry
it.

Implement. In `src/components/RuntimeAdminSection.tsx`, add
`responses_live_timings_enabled` to `specBodyWithAdminState`'s destructuring
so it is dropped from `rest`, and extend that function's comment:

```tsx
  // …existing comment about the six read-only echoes…
  //
  // responses_live_timings_enabled is dropped for a DIFFERENT reason: it is
  // writable, but on the request shape it is OPTIONAL, and absent means "keep
  // the stored value". Dropping it is therefore exactly what an override
  // click wants -- and it is also the only safe thing to send, because the
  // store is policy-free by design and a stored true can outlive a retype to
  // an incapable kind; restating such a pair would earn a 400 and turn a
  // Start/Stop/Clear click into a failed write.
  const {
    configured,
    id,
    mapping_id,
    api_token_set,
    app_api_token_set,
    app_api_token_header,
    effective_type,
    resolved_metrics_path,
    resolved_context_probe_path,
    responses_live_timings_enabled,
    ...rest
  } = spec;
  return { ...rest, admin_state: adminState };
```

(`@typescript-eslint/no-unused-vars` is configured with
`ignoreRestSiblings: true`, so the unused binding is fine — the nine beside it
already rely on that.)

Re-run `npm test -- src/components/RuntimeAdminSection.test.tsx`: green (all
five cases back). Then run the type gate — **not** for a clean exit, which is
impossible at this point, but for the one signal it can give here, the
fixtures:

```
cd gateway/frontend && npm run build
```

Expected: `tsc` fails with **exactly two** errors, so `vite build` never runs
(`build` is `tsc && vite build`). Both are `TS2739` on an
`<ApiVariantControls` element — one in `src/components/ApplicationSection.tsx`,
one in `src/components/RuntimeAdminSection.tsx` — each ending:

```
… is missing the following properties from type 'Readonly<{ … }>': liveTimings, liveTimingsKind, onLiveTimingsChange
```

That is expected and is not a defect to chase: Step 4 made the three props
required, those two files are the only production call sites, and they are
filled in by Steps 6 and 8 respectively. `tsconfig.json` has
`include: ["src"]`, so both files are typechecked from here on. Step 6 clears
the first error, Step 8 the second, and Step 8 is where the clean-build verdict
lives — do not run `npm run build` again in between.

What this run IS for: no line reading
`Property 'responses_live_timings_enabled' is missing in type … but required in type 'PortalApplication'`
(or `… required in type 'RuntimeSpec'`) may appear. One that does names a
fixture site that was missed — the list above is exhaustive; re-read it.

Revert-proof (production file only): remove
`responses_live_timings_enabled,` from `specBodyWithAdminState`'s
destructuring. The six `expectedBody` assertions fail again with the deep-equal
message above.

Commit: `src/api/models.ts`, `src/api/runtime.ts`,
`src/components/RuntimeAdminSection.tsx`, and the six fixture files —
`feat(portal): carry responses_live_timings_enabled on both portal API surfaces`

---

- [ ] **Step 6: the application form's state, seeds and rendering**

Write the failing test. Append to
`gateway/frontend/src/components/ApplicationSection.test.tsx`:

```tsx
// Responses live timings (issue #81 part 2, design D10). The control is
// rendered by the shared ApiVariantControls block; what is pinned HERE is the
// part this form owns -- which kind it reports, what it seeds, and (next
// describe) what it puts in the body.
describe('ApplicationSection responses live timings', () => {
  it('hides the control for an incapable type and shows it TICKED once a capable type is chosen', async () => {
    renderSection();
    openCreate();
    // The create form opens on ollama.
    expect(
      screen.queryByRole('checkbox', { name: t.applicationLiveTimings }),
    ).not.toBeInTheDocument();
    expect(screen.getByText(t.applicationLiveTimingsUnsupportedNote)).toBeInTheDocument();

    await selectType('llama_cpp');
    // Ticked without the operator touching anything: the API's create default
    // for a capable type is ON, and a control that opened unticked would turn
    // the feature off for every application created through this portal.
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).toBeChecked();
  });

  // BOTH directions in one case, deliberately: asserting only the stored
  // `false` would still pass if openEdit stopped seeding at all, because the
  // create default would be left standing -- for `false` that happens to look
  // right. The stored `true` half is what fails then.
  it('seeds the control from the loaded application on edit, not from the create default', async () => {
    renderSection({
      apps: [makeApp({ id: 'app_1', type: 'llama_cpp', responses_live_timings_enabled: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).toBeChecked();
    cleanup();

    renderSection({
      apps: [makeApp({ id: 'app_1', type: 'llama_cpp', responses_live_timings_enabled: false })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).not.toBeChecked();
  });
});
```

Run it:

```
cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx
```

Expected, and read this carefully because the naive reading is wrong. The
shared block from Step 4 renders **whatever it is handed**, and this form
hands it nothing yet, so `liveTimingsKind` arrives `undefined`, the
`=== 'incapable'` branch is false, and a checkbox DOES render — unticked, with
the ordinary note.

- Case one fails on its **first** assertion:
  `expected <input … /> not to be in the document`. (Its second and third
  assertions are never reached.)
- Case two fails on its **first** assertion: `expected element to be checked`.

Commit nothing until both pass.

Implement. In
`gateway/frontend/src/components/ApplicationSection.tsx`:

add the import
`import { applicationLiveTimingsKind } from './shared/liveTimings';` beside
the existing `applicationTypeDefaults` import;

declare the state next to `proxyExcluded`:

```tsx
  // The operator's opt-in to asking a capable upstream for live per-token
  // timings on /v1/responses. Seeded TRUE on create -- deliberately, and not
  // a guess: this value is only ever SENT for a capable type (buildBody), and
  // the API's create default is ON for every capable type, so an untouched
  // portal create now agrees with the documented default instead of silently
  // switching the feature off. On edit it is seeded from the stored row, so a
  // retype TO a capable kind does NOT switch it on -- matching the API, whose
  // kind-dependent default is scoped to create.
  const [liveTimings, setLiveTimings] = useState(true);
```

derive the kind next to `showProxyControls` (it must be recomputed on every
render, because `type` is form state that `handleTypeChange` moves):

```tsx
  const liveTimingsKind = applicationLiveTimingsKind(type);
```

add `setLiveTimings(true);` to `openCreate` (beside `setProxyExcluded(false);`)
and `setLiveTimings(app.responses_live_timings_enabled);` to `openEdit`
(beside `setProxyExcluded(app.proxy_excluded);`);

and pass the three props in the `ApiVariantControls` call:

```tsx
            <ApiVariantControls
              t={t}
              apiFlavors={flavors}
              responsesMode={responsesMode}
              messagesMode={messagesMode}
              liveTimings={liveTimings}
              liveTimingsKind={liveTimingsKind}
              onFlavorsChange={setFlavors}
              onResponsesModeChange={setResponsesMode}
              onMessagesModeChange={setMessagesMode}
              onLiveTimingsChange={setLiveTimings}
            />
```

Do **not** touch `buildBody` yet — that is the next step.

Re-run: green.

Revert-proof (production file only), three independent mutations:

1. `setLiveTimings(true);` **inside `openCreate`** → `setLiveTimings(false);`:
   case one fails on its last assertion with `expected element to be checked`.
   Mutate that call, **not** the `useState(true)` initializer: `mode` starts
   `'list'`, the form renders only under `mode !== 'list'`, and the only two
   ways in are `openCreate` and `openEdit` — both of which seed this state
   before the form can render. The initializer's value is therefore never
   observable and mutating it reds nothing; it is written `true` only so the
   declaration reads as what the create path means.
2. Delete `setLiveTimings(app.responses_live_timings_enabled);` from
   `openEdit`: case two fails on its **second** half with
   `expected element not to be checked` — the create default survives into
   the edit form.
3. Replace `liveTimingsKind` in the `ApiVariantControls` call with the literal
   `"capable"`: case one fails on its first assertion with
   `expected <input … /> not to be in the document`.

Commit: `src/components/ApplicationSection.tsx`,
`src/components/ApplicationSection.test.tsx` —
`feat(portal): seed and render the live-timings control on the application form`

---

- [ ] **Step 7: what the application form's body carries**

Write the failing tests. Append a second `describe` to
`gateway/frontend/src/components/ApplicationSection.test.tsx`:

```tsx
describe('ApplicationSection responses live timings body', () => {
  it('sends the create default for a capable type without the operator touching the box', async () => {
    const { created } = renderSection();
    openCreate();
    await selectType('llama_cpp');
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].responses_live_timings_enabled).toBe(true);
  });

  it('sends an explicit false when the operator unticks it', async () => {
    const { created } = renderSection();
    openCreate();
    await selectType('llama_cpp');
    fireEvent.click(screen.getByRole('checkbox', { name: t.applicationLiveTimings }));
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect(created[0].responses_live_timings_enabled).toBe(false);
  });

  it('restates the stored value on an unrelated save of a capable application', async () => {
    const { updated } = renderSection({
      apps: [makeApp({ id: 'app_1', type: 'llama_cpp', responses_live_timings_enabled: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    fireEvent.change(screen.getByLabelText(t.applicationWeight), { target: { value: '7' } });
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    // Safe to restate, unlike proxy_excluded: a capable type accepts BOTH
    // values, so a save made for an unrelated reason cannot change anything.
    expect(updated[0].body.responses_live_timings_enabled).toBe(true);
    expect(updated[0].body.weight).toBe(7);
  });

  // THE BLOCKER THIS FORM MUST NOT BE ABLE TO BUILD. buildBody restates
  // `type` on every save, and the backend picks the 400 arm over the 409
  // precisely when the request carries a type -- so an unconditional true on
  // an incapable type does not merely fail to apply, it refuses the whole
  // save. Omitting is also what the backend WANTS: the create takes the
  // type's default and the update clears a stale true.
  it('omits the key entirely for an incapable type, on create and on save', async () => {
    const { created } = renderSection();
    openCreate();
    // ollama, the form's own create default, is incapable.
    fireEvent.click(screen.getByRole('button', { name: t.applicationCreate }));
    await waitFor(() => expect(created).toHaveLength(1));
    expect('responses_live_timings_enabled' in created[0]).toBe(false);
    cleanup();

    const { updated } = renderSection({
      apps: [makeApp({ id: 'app_1', type: 'vllm', responses_live_timings_enabled: true })],
    });
    await screen.findByText('https://s1.example.test:8000');
    fireEvent.click(screen.getByRole('button', { name: t.applicationEdit }));
    fireEvent.click(screen.getByRole('button', { name: t.applicationSave }));
    await waitFor(() => expect(updated).toHaveLength(1));
    // A vLLM row can still hold a stale true from before design D6 dropped
    // vLLM from the capable set. Omitting is what clears it, prospectively,
    // on the row's next save -- the clear D6 promises and refuses to do in SQL.
    expect('responses_live_timings_enabled' in updated[0].body).toBe(false);
  });
});
```

`cleanup` is already imported in this file; `waitFor` too.

Run it:

```
cd gateway/frontend && npm test -- src/components/ApplicationSection.test.tsx
```

Expected: the first three cases fail with `expected undefined to be true` /
`… to be false` / `expected undefined to be true` — `buildBody` sends no such
key yet. **The fourth case PASSES right now, and that is not evidence.** It is
vacuously true while nothing sends the key at all; it is kept because it is
the permanent guard on the blocker, and its revert-proof below is what makes
it real. Do not take its green as a signal in this step.

Implement. In `buildBody` in
`gateway/frontend/src/components/ApplicationSection.tsx`, insert immediately
after the `messages_mode: messagesMode,` line:

```tsx
      // Sent ONLY for a type that can honour it. buildBody restates `type` on
      // every save, and the backend selects the 400 arm
      // (application.responses_live_timings_unsupported) over the 409
      // precisely when the request carries a type -- so an unconditional true
      // on an incapable type would not merely fail to apply, it would REFUSE
      // THE WHOLE SAVE, and on an unrelated edit at that. Omitting is what
      // the API asks for instead: absent on a create takes the type's own
      // default, and absent on an update is what CLEARS a stored true whose
      // resulting type cannot honour it.
      //
      // Unlike proxy_excluded next door, no seed-diff is needed: this key is
      // only ever sent for a capable type, which accepts both values, so
      // restating it on an unrelated save changes nothing.
      ...(liveTimingsKind === 'capable'
        ? { responses_live_timings_enabled: liveTimings }
        : {}),
```

Re-run: green (all four).

Revert-proof (production file only): replace that conditional spread with the
unconditional `responses_live_timings_enabled: liveTimings,`. The fourth case
fails on its first assertion with `expected true to be false` (the `in` check),
which is exactly the save-blocking pair reaching the wire. Separately,
replacing it with `{}` fails the first case with `expected undefined to be true`.

Commit: `src/components/ApplicationSection.tsx`,
`src/components/ApplicationSection.test.tsx` —
`feat(portal): send responses_live_timings_enabled only for a type that can honour it`

---

- [ ] **Step 8: the launch-spec form's state, hydration and rendering**

Write the failing test. Append to
`gateway/frontend/src/components/RuntimeAdminSection.test.tsx`:

```tsx
// Responses live timings on the launch-spec surface (issue #81 part 2, design
// D10). This form's capability signal is the WRITABLE Type select, which is
// the first branch of routing.EffectiveRuntimeSpecType; only "Auto" falls
// through to the binary-basename detection, which is Go-only and is the one
// case this form answers "unknown".
describe('RuntimeAdminSection responses live timings', () => {
  it('shows the Auto caption on create and no tick, because the kind is not known yet', async () => {
    renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).not.toBeChecked();
    expect(screen.getByText(t.applicationLiveTimingsAutoNote)).toBeInTheDocument();
  });

  it('ticks the box once Type is set to llama_cpp, and hides it for an incapable Type', async () => {
    renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecType }));
    fireEvent.click(await screen.findByRole('option', { name: t.runtimeSpecTypeLlamaCpp }));
    // Nothing was ticked by hand: with a known-capable kind and no opinion,
    // what the backend will apply on a first write is ON, so the box says so.
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).toBeChecked();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecType }));
    fireEvent.click(await screen.findByRole('option', { name: t.runtimeSpecTypeOllama }));
    expect(
      screen.queryByRole('checkbox', { name: t.applicationLiveTimings }),
    ).not.toBeInTheDocument();
    expect(screen.getByText(t.applicationLiveTimingsUnsupportedNote)).toBeInTheDocument();
  });

  it('hydrates the box from a CONFIGURED spec, and treats an unconfigured one as no opinion', async () => {
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true,
          mapping_id: 'map_1',
          type: 'llama_cpp',
          binary: '/usr/bin/llama-server',
          responses_live_timings_enabled: false,
        }),
      },
    });
    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).not.toBeChecked();
    cleanup();

    // Edit is deliberately ungated and is reachable on a mapping with NO spec
    // row. That document's `false` is a ZERO VALUE, not an operator decision:
    // hydrating it would make this form's FIRST write send an explicit false
    // and lose the llama.cpp create default.
    renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({ mapping_id: 'map_1', type: 'llama_cpp' }),
      },
    });
    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).toBeChecked();
  });
});
```

Run it:

```
cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx
```

Expected, and again the naive reading is wrong for the same reason as Step 6:
the spec form does not pass the props yet, so `liveTimingsKind` arrives
`undefined`, a checkbox renders unticked, and the ordinary note renders.

- Case one fails on its **second** assertion:
  `Unable to find an element with the text: Der Typ steht auf „Automatisch“…`
  (its first assertion passes vacuously — it is kept because it becomes real
  the moment a kind is reported, and its revert-proof below exercises it).
- Case two fails on its **first** assertion after the llama_cpp selection:
  `expected element to be checked`.
- Case three fails on its **last** assertion: `expected element to be checked`
  (its first half passes vacuously, for the same reason as case one's first).

Also verify the *existing* suite is still green — in particular the five
`expectedBody` cases from Step 5.

Implement. In
`gateway/frontend/src/components/RuntimeAdminSection.tsx`:

add `import { runtimeSpecLiveTimingsKind } from './shared/liveTimings';` beside
the existing `ApiVariantControls` import;

declare the state immediately after
`const [contextProbePath, setContextProbePath] = useState('');`:

```tsx
  // undefined = "no opinion", and the request field is optional so this form
  // can say it. It genuinely has to: with Type on "Auto" the effective kind
  // is detected from the binary's basename, and that detection is Go-only --
  // so a `true` here might be refused and a `false` would silently disagree
  // with the llama.cpp create default. The only correct thing to send is
  // nothing, and the backend then applies the kind's own default on a first
  // write or keeps the stored value on a later one.
  const [specLiveTimings, setSpecLiveTimings] = useState<boolean | undefined>(undefined);
  const specLiveTimingsKind = runtimeSpecLiveTimingsKind(specType);
```

add `setSpecLiveTimings(undefined);` to `resetSpecFields` (beside
`setSpecType('');`);

add to `hydrateSpecFields`, beside `setSpecType(spec.type);`:

```tsx
    // `configured: false` means the mapping has no spec row and every other
    // field is a zero value -- so that `false` is not an operator decision
    // and must not become one. Edit is ungated and reaches exactly that
    // document, and the write it leads to is a FIRST write.
    setSpecLiveTimings(spec.configured ? spec.responses_live_timings_enabled : undefined);
```

and pass the three props in the spec form's `ApiVariantControls` call:

```tsx
            <ApiVariantControls
              t={t}
              apiFlavors={specApiFlavors}
              responsesMode={specResponsesMode}
              messagesMode={specMessagesMode}
              liveTimings={specLiveTimings}
              liveTimingsKind={specLiveTimingsKind}
              onFlavorsChange={setSpecApiFlavors}
              onResponsesModeChange={setSpecResponsesMode}
              onMessagesModeChange={setSpecMessagesMode}
              onLiveTimingsChange={setSpecLiveTimings}
            />
```

Do **not** touch `buildSpecBody` yet.

(The block renders above the Type select it is governed by. That is where the
shared block already sits and moving it is out of scope; the caption is what
carries the explanation, so keep the strings free of any "above"/"below".)

Re-run: green.

Then the type gate, which **can** be clean for the first time in this task —
this step fills in the second and last production call site of the props Step 4
made required:

```
cd gateway/frontend && npm run build
```

Expected: `tsc` clean, `vite build` succeeds. The two `TS2739` diagnostics
Step 5 predicted are gone. A remaining one names the file whose
`<ApiVariantControls` call was left unfilled — this step's, or Step 6's.

Revert-proof (production file only), three independent mutations. Two of them
mutate the same expression, the `liveTimingsKind` prop, to two different
literals: that is deliberate, because each literal reds a different case and
together they pin the wiring in both directions.

1. `hydrateSpecFields`'s line → `setSpecLiveTimings(spec.responses_live_timings_enabled);`
   (drop the `configured` guard): case three fails on its LAST assertion with
   `expected element to be checked` — the unconfigured document's zero-value
   `false` has become an opinion.
2. `liveTimingsKind={specLiveTimingsKind}` → `liveTimingsKind="unknown"`:
   case two fails on its first assertion with `expected element to be checked`
   — with the kind pinned to `unknown`, choosing llama_cpp no longer ticks the
   box. Case three fails on its last assertion with the same message, for the
   same reason (no opinion on a kind reported `unknown` renders unticked); both
   reds are this one mutation. Do **not** mutate the
   `useState<boolean | undefined>(undefined)` initializer instead:
   `resetSpecFields` sets `undefined` and `hydrateSpecFields` sets the stored
   value, both before `specMode` leaves `'list'` and the form can render, so
   that initializer is unobservable and reds nothing.
3. `liveTimingsKind={specLiveTimingsKind}` → `liveTimingsKind="capable"`:
   case one fails on its **first** assertion with
   `expected element not to be checked` — `specLiveTimings` is still
   `undefined` on create, so `checked={liveTimings ?? liveTimingsKind === 'capable'}`
   is now `true`. (Its second assertion, about the Auto caption, would fail too
   but is never reached.) Case two fails on its **second-to-last** assertion
   with `expected <input … /> not to be in the document`, the ollama half.

Commit: `src/components/RuntimeAdminSection.tsx`,
`src/components/RuntimeAdminSection.test.tsx` —
`feat(portal): seed and render the live-timings control on the launch-spec form`

---

- [ ] **Step 9: what the launch-spec form's PUT body carries**

Write the failing tests. Append a second `describe` to
`gateway/frontend/src/components/RuntimeAdminSection.test.tsx`:

```tsx
describe('RuntimeAdminSection responses live timings body', () => {
  it('omits the key on an untouched Auto create, so the backend applies the detected kind default', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    // Sending `false` here would be the defect: the backend's first-write
    // default for a llama-server binary is ON, and this form cannot detect
    // that basename itself.
    expect('responses_live_timings_enabled' in putSpecs[0].body).toBe(false);
  });

  it('sends an explicit value once the operator states one under Auto', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.click(screen.getByRole('checkbox', { name: t.applicationLiveTimings }));
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.responses_live_timings_enabled).toBe(true);
  });

  it('sends the value for an explicit llama_cpp Type', async () => {
    const { putSpecs } = renderSection();
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecCreate }));
    fireEvent.change(screen.getByLabelText(t.mappingAppName), { target: { value: 'app-new' } });
    fireEvent.change(screen.getByLabelText(t.runtimeSpecBinary), {
      target: { value: '/usr/bin/llama-server' },
    });
    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecType }));
    fireEvent.click(await screen.findByRole('option', { name: t.runtimeSpecTypeLlamaCpp }));
    fireEvent.click(screen.getByRole('checkbox', { name: t.applicationLiveTimings })); // untick
    fireEvent.click(screen.getByRole('button', { name: t.runtimeSpecCreate }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.responses_live_timings_enabled).toBe(false);
  });

  // THE BLOCKER THIS FORM MUST NOT BE ABLE TO BUILD, and it is the same one
  // the application form has: buildSpecBody is a FULL-DOCUMENT upsert that
  // restates `type` on every save, and putRuntimeSpec refuses an explicit
  // true against an incapable effective kind with 400
  // runtime_spec.responses_live_timings_unsupported BEFORE any store write --
  // so the whole save fails. Omitting is what the backend normalises: it
  // clears a stored true whose document can no longer honour it.
  it('omits the key when the operator retypes a live-timings spec to an incapable kind', async () => {
    const { putSpecs } = renderSection({
      mappings: [makeMapping({ id: 'map_1' })],
      specsByMappingId: {
        map_1: makeSpec({
          configured: true,
          mapping_id: 'map_1',
          type: 'llama_cpp',
          binary: '/usr/bin/llama-server',
          responses_live_timings_enabled: true,
        }),
      },
    });
    await screen.findByText('gw-model');
    fireEvent.click(await screen.findByRole('button', { name: t.runtimeSpecEditAction }));
    await screen.findByLabelText(t.runtimeSpecBinary);
    expect(screen.getByRole('checkbox', { name: t.applicationLiveTimings })).toBeChecked();

    fireEvent.mouseDown(screen.getByRole('combobox', { name: t.runtimeSpecType }));
    fireEvent.click(await screen.findByRole('option', { name: t.runtimeSpecTypeOllama }));
    fireEvent.click(screen.getByRole('button', { name: t.save }));
    await waitFor(() => expect(putSpecs).toHaveLength(1));
    expect(putSpecs[0].body.type).toBe('ollama');
    expect('responses_live_timings_enabled' in putSpecs[0].body).toBe(false);
  });
});
```

Run it:

```
cd gateway/frontend && npm test -- src/components/RuntimeAdminSection.test.tsx
```

Expected: cases 2 and 3 fail with `expected undefined to be true` and
`expected undefined to be false`. **Cases 1 and 4 PASS right now and that is
not evidence** — nothing sends the key at all yet, so both `in` checks are
vacuously false. They are kept as the permanent guards (case 1 on the create
default, case 4 on the save blocker) and their revert-proofs below are what
make them real. Do not read their green as a signal in this step.

Implement. In `buildSpecBody` in
`gateway/frontend/src/components/RuntimeAdminSection.tsx`, insert immediately
after the `messages_mode: specMessagesMode,` line:

```tsx
      // Two independent reasons to omit, and both must hold before the key is
      // sent.
      //
      // Kind: this is a FULL-DOCUMENT upsert that restates `type` on every
      // save, and putRuntimeSpec refuses an explicit true against an
      // incapable effective kind with 400 BEFORE any store write -- so the
      // whole save would fail, on a retype the operator made for another
      // reason entirely. Omitting is what the backend normalises: it clears a
      // stored true whose document can no longer honour it, deliberately and
      // silently, because the caller said nothing about the flag.
      //
      // Value: undefined is "no opinion", which IS the absent key. Under Type
      // "Auto" that is the only honest thing to send, since the kind is
      // detected from the binary's basename by Go and not by this form.
      ...(specLiveTimingsKind !== 'incapable' && specLiveTimings !== undefined
        ? { responses_live_timings_enabled: specLiveTimings }
        : {}),
```

Re-run: green (all four).

Revert-proof (production file only), two independent mutations:

1. Drop the `specLiveTimingsKind !== 'incapable' &&` conjunct. Case 4 fails on
   its last assertion with `expected true to be false` — the save-blocking
   `{type: 'ollama', responses_live_timings_enabled: true}` pair reaches the
   wire.
2. Replace the whole conditional spread with the unconditional
   `responses_live_timings_enabled: specLiveTimings ?? false,`. Case 1 fails
   with `expected true to be false`, and the six `expectedBody` assertions
   from Step 5 stay green (they exercise the override path, which does not go
   through `buildSpecBody`) — which is why case 1 has to exist.

Commit: `src/components/RuntimeAdminSection.tsx`,
`src/components/RuntimeAdminSection.test.tsx` —
`feat(portal): send responses_live_timings_enabled from the launch-spec form only when it is meant`

---

- [ ] **Step 10: the four frontend gates**

Run them in CI's own order, from `gateway/frontend`, and read every one:

```
cd gateway/frontend
npm run format
npm run format:check
npm run lint
npm run build
npm test
```

- `npm run format` first, then `format:check` — CI runs only the check, and it
  is the gate a local `test + build + lint` does not cover. Expected:
  `Checking formatting...` followed by `All matched files use Prettier code style!`
- `npm run lint` — expected: no output, exit 0. The ten-way destructuring in
  `specBodyWithAdminState` (nine bindings before this task, plus the one Step 5
  adds) relies on `ignoreRestSiblings: true`, which is set in
  `eslint.config.js`; if `@typescript-eslint/no-unused-vars` fires on
  `responses_live_timings_enabled` there, you edited the wrong place.
- `npm run build` — `tsc` then `vite build`. Expected: no diagnostics.
- `npm test` — the **full** suite, no `-t`/file filter. This is the verdict;
  the per-file runs above were for speed only. Expected: every file passing,
  including `src/App.test.tsx`, `ServerList.test.tsx`,
  `MappingSection.test.tsx` and `BenchmarkSection.test.tsx`, whose fixtures
  Step 5 touched.

If `npm run format` rewrote files, commit the reformat:
`style(portal): apply prettier to the live-timings control changes`

Then confirm nothing outside `gateway/frontend` changed:

```
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection
git status --porcelain
```

Expected: clean. No Go file, no file under `docs/`, and no
`docs/implementation-status.md` entry belongs to this task.

---

### Task 8: Rewrite the standing claims and document the feature

Every path is relative to the worktree root
`/Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection`.
Run every command from there. This task ships **no production behaviour**: it is
comments and canonical documents only.

**Run this task LAST.** Several steps document things Tasks 1–7 build, and each
one verifies the real tree before writing prose about it.

**Files:**

- **Do NOT modify** `gateway/backend/internal/gateway/native_passthrough.go`.
  Its two comments at the injection point are Task 4's, step 9 (PROBLEM 1). This
  task only *reads* that file, in the step-8 pre-read.
- Modify: `gateway/backend/internal/gateway/passthrough_usage_scan.go` — the doc
  comment on the `usageScanner` struct's `finalPromptPerSecond` /
  `finalTokensPerSecond` field pair. Comment only.
- Modify: `gateway/backend/internal/gateway/capture.go` — the doc comment on
  `captureInput`'s `Translated*` field group, and the doc comment above
  `func attachTranslatedCapture`. Comments only.
- Test (comment only): `gateway/backend/internal/gateway/passthrough_progress_test.go`
  — the doc comment above
  `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves`, and
  nothing else in the file. **Do not touch that test's body** (Task 4's step 10c
  edits one failure message and adds one comment inside it), **do not touch
  `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly`**, whose doc
  and failure message are Task 4's steps 10a/10b, **and do not touch
  `TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate`**, which
  is Task 5's.
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md`
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md`
- Modify: `docs/architecture/cross-cutting/security-auth-rbac.md`
- Modify: `docs/architecture/cross-cutting/agent-runtime-manager.md`
- Modify: `docs/architecture/reference/api-surface.md`
- Modify: `docs/architecture/reference/data-model.md`
- Modify: `docs/architecture/11-risks-and-technical-debt.md`

**Interfaces:**

- **Consumes** (names only — this task calls nothing and compiles nothing new):
  - From Task 1: `routing.LiveTimingsCapableKind(kind string) bool`, true for
    `routing.ProviderLlamaCPP` (`"llama_cpp"`) alone; `internal/provider`'s
    `liveProgressUpstreams` keeps vLLM, and the cross-package parity test relaxes
    from equality to `capable ⊆ gate`.
  - From Task 2: `gateway.wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool`,
    in `gateway/backend/internal/gateway/responses_live_timings.go`; the constant
    `liveTimingsVerdictUnsupported = "unsupported"`.
  - From Task 3: `gateway.injectTimingsPerToken(raw []byte) ([]byte, bool)` and
    `timingsPerTokenKey`, both in `native_passthrough.go`.
  - From Task 4: the call site inside `proxyNative`, and **D9's log field** on the
    `slog.Debug("inference request (native passthrough)", …)` line. **Its name is
    not fixed by any plan file this task could read** — step 8 reads it out of the
    tree and writes the real one.
  - From Task 5: `inference.Usage.LiveOutputTokens`, written only by
    `mergeResponsesUsage`, read only by `usageScanner.publishProgress`, carried to
    the existing `output_tokens` DTO field. No new wire field.
  - From Task 6: a default-hidden live output-tokens column on the
    running-connections panel, rendering that existing field.
  - From Task 7: the portal checkbox on both surfaces.
- **Produces:** nothing any later task consumes. This is the last task.

**No new automated test, and why that is not a cop-out.** Every edit here is a
comment or a sentence in a markdown document. Hard rule 3 ("every new test must
fail when its production change is reverted") has no purchase: reverting a
comment breaks no compiler and no assertion, and a Go test that greps its own
repository for a forbidden English sentence would pin *wording*, break on every
legitimate rewording, and is a shape this repository has nowhere. The executable
check each step carries instead is a `grep` that must print the stale sentence
**before** the edit and print nothing **after** it — a real red/green with an
exact command and exact output, run on the real corpus. Where a step documents
something another task built, it also greps the tree for that thing first, so no
step writes prose about code that is not there.

**The two gates, and exactly what they do and do not catch.**

`./scripts/check-docs.sh` (also `make lint-docs`) runs five checks, and its own
header enumerates them:

1. every intra-repo markdown link resolves — the file, and the `#anchor` too,
   slugged from the target document's own headings by GitHub's rules;
2. every file under `docs/architecture/` is reachable from
   `docs/architecture/README.md`, directly or transitively;
3. `docs/architecture/reference/openapi.yaml` parses as the YAML subset it is
   written in, and every `$ref` resolves;
4. `config-env.md`'s agent table agrees, in both directions, with the flags
   `server-agent` actually registers;
5. no document anywhere names a `-a-b`-shaped CLI flag that no Go binary in this
   repository registers (fenced code blocks **included**, deliberately).

Baseline on this worktree, verified: `check-docs: OK`, exit 0, *"markdown files
read: 44, anchors: 598, intra-repo links: 633"*, *"docs/architecture files: 29,
reachable from the index: 29"*. **Do not treat those counts as the verdict** —
`docs/superpowers/` is branch-local and is removed before the pull request, so
the file count moves. `check-docs: OK` and exit 0 are the verdict.

What it does **not** catch, which is the whole hazard of this task: a prose
contradiction, a stale fact, a document that disagrees with the code, anything
inside a Go or TypeScript comment, spelling, or prose style. Its own header says
so: *"This is a consistency check, not a markdown linter."* **Four** of this
task's own edits are Go comments, entirely invisible to it: the `usageScanner`
field doc in `passthrough_usage_scan.go` (step 3), the terminal-usage test's doc
comment (step 4), and **both** `capture.go` comments (step 5) — Set 2's two code
sites are both comments, not one.

`bash ./scripts/check-docs.test.sh` pins **the checker**, not this corpus. It
builds a throwaway git repository holding a miniature fixture and runs a copy of
`check-docs.sh` over it, 28 `expect` cases. Counted: **five** of them pin the
checker staying quiet where it must (a clean corpus, transitive reachability,
the clean agent table, the skip when the agent module is absent, and a document
naming only real flags); the other 23 pin a failure it must produce. It never
reads `docs/architecture/` at all, so
it catches **nothing** about these edits. It is in the gate list because editing
documents is precisely when someone is tempted to loosen the checker instead of
fixing the document. Expected tail: `all check-docs cases passed`.

---

### Table A — Set 1, the injection prohibition: twelve sites, three of them already owned

Named by function/region, never by line number. "Owner" is this task unless the
Step column says otherwise.

| # | File | Region (by name) | The sentence that becomes false | Step |
|---|---|---|---|---|
| 1 | `internal/gateway/native_passthrough.go` | the doc comment above `func (s *Server) proxyNative` | "The only body edit is rewriting the `model` field to the upstream's mapped name (lossless; all other fields untouched)." | **Task 4**, step 9a — do not touch |
| 2 | `internal/gateway/native_passthrough.go` | the block comment above `upstreamBody := rewriteModelField(...)` inside `proxyNative` | "The ONLY edit made to a relayed body, ever." / "the gateway does NOT add llama.cpp's `timings_per_token`" / "The flag is READ when the client set it and never set here." — **plus the already-stale** "Whether its Responses implementation does the same on partials is not something this repo has captured" | **Task 4**, step 9b — do not touch |
| 3 | `internal/gateway/passthrough_usage_scan.go` | the `finalPromptPerSecond` / `finalTokensPerSecond` field doc on `usageScanner` | "The precondition is a client's own `timings_per_token`, RELAYED UNTOUCHED" / "this capture is a measured no-op for traffic that does not" | 3 |
| 4 | `internal/gateway/passthrough_progress_test.go` | doc comment of `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly` | "the relayed body must not have grown a `timings_per_token` flag the client never sent, which is the one edit that would turn this row's two em-dashes into numbers" | **Task 4**, step 10a — do not touch |
| 5 | `telemetry-usage-observability.md` §8.4.3 | the `openai_responses` cell of the per-flavor table's **`rate`** row | "mid-stream only when the **client** set `timings_per_token`" | 6 |
| 6 | `telemetry-usage-observability.md` §8.4.3 | the paragraph beginning "**The Responses column's terminal frame is a different answer**" | "The honest one-line reading of the column is therefore \"nothing to show mid-stream unless the client asked for it\"" | 6 |
| 7 | `telemetry-usage-observability.md` §8.4.3 | the first bullet under "**Two rules on this path must survive any later change.**" | "**`timings_per_token` is READ when the client set it, and never injected.**" (whole bullet) | 7 |
| 8 | `telemetry-usage-observability.md` §8.4.3 | the paragraph beginning "**Native passthrough gets neither parameter**" | "`proxyNative` forwards the client's own body unmodified (only the `model` field is ever rewritten, and losslessly), so neither parameter is ever added on this path; `timings_per_token` is read when the client set it and never injected" | 10 |
| 9 | `telemetry-usage-observability.md` §8.4.3 | the bullet "**The precondition needed no gateway change.**" | "this substitution is a measured no-op for traffic that does not" | 10 |
| 10 | `reference/api-surface.md` | the `responses_live_timings_enabled` bullet's first sub-bullet | "the standing rule that `timings_per_token` is read when the client set it and never injected" | 12 |
| 11 | `cross-cutting/compatibility-and-inference.md` | the paragraph "**The panel's live figures are no longer part of the trade-off**" | "a mid-stream **rate** only when the client itself asked llama.cpp for `timings_per_token`" and "the relayed body is never augmented to improve a display column" | 15 |
| 12 | `cross-cutting/agent-runtime-manager.md` | the `responses_live_timings_enabled` paragraph in §11.5 | "no upstream parameter is injected" | 14 |

**Twelve rows, counted.** Eight are the distinct sites the surface map's P3 table
names; four (rows 1, 4, 5 and 6) are additions counted by hand. **Nine are this
task's**; rows 1, 2 and 4 are Task 4's and must be left alone (PROBLEM 1).

**Four rows appear in a second table as well**, because one sentence or one
region carries two claims — counted, not estimated:

- row 5 is also Table C row 2 (the `rate` cell, two false clauses, one cell);
- row 10 is also Table D member 4 — the *same* `api-surface.md` sub-bullet, so
  edit 12a retires both claims in one replacement;
- row 11 is also Table C row 4 (one `compatibility-and-inference.md` paragraph,
  both claims, one edit in step 15);
- row 12 is also Table D member 6 (the §11.5 paragraph).

### Table B — Set 2, the capture-identity claim: three sites (already false on `main`)

| # | File | Region | The sentence | Step |
|---|---|---|---|---|
| 1 | `internal/gateway/capture.go` | the doc comment on `captureInput`'s `Translated*` field group | "Empty on native passthrough (its client bytes already equal the upstream bytes)" | 5 |
| 2 | `internal/gateway/capture.go` | the doc comment above `func attachTranslatedCapture` | "Native passthrough passes no sink because the bytes it already captures ARE the upstream bytes." | 5 |
| 3 | `cross-cutting/security-auth-rbac.md` | §14, the header-redaction paragraph | "native passthrough has no separate translated headers to redact because the client bytes already equal the upstream bytes" | 5 |

### Table C — Set 3, the "no mid-stream source" claim: seven sites, two of them already owned

| # | File | Region | Owner | Step |
|---|---|---|---|---|
| 1 | `telemetry-usage-observability.md` §8.4.3 | the `openai_responses` cell of the **`output tokens`** row | this task | 6 |
| 2 | `telemetry-usage-observability.md` §8.4.3 | the `openai_responses` cell of the **`rate`** row, second clause ("There is no mid-stream count on this flavor, so the gateway derives nothing here") | this task | 6 |
| 3 | `telemetry-usage-observability.md` §8.4.3 | the paragraph "**The same gate governs both columns.**" | this task (unowned; see PROBLEM 3) | 11 |
| 4 | `cross-cutting/compatibility-and-inference.md` | the paragraph "**The panel's live figures are no longer part of the trade-off**", clause "on the Responses shape **no** mid-stream token count at all" | this task (unowned; see PROBLEM 3) | 15 |
| 5 | `passthrough_progress_test.go` | doc comment of `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves`: "the flavor has no MID-STREAM source for a count" | this task (unowned; see PROBLEM 3) | 4 |
| 6 | `passthrough_progress_test.go` | `…WithClientTimingsShowsTheUpstreamRate`'s doc + its "the partials still report no usage" failure message | **Task 5**, step 3 — do not touch | — |
| 7 | `ActiveRequestsPanel.test.tsx` / `ActiveRequestsPanel.tsx` / `i18n.test.ts` | the three frontend comments | **Task 6**, steps 7 and 11 — do not touch | — |

**Seven rows, counted.** Rows 1+2 (one table, two cells), 6 and 7 are the three
sites the design names. Rows **3, 4 and 5** are the three it names nowhere
(PROBLEM 3). Row 4 is also Table A row 11 — one paragraph, one edit; rows 3 and
5 carry the Set 3 claim only and appear in no other table.

### Table D — Set 4, "nothing reads the resolved flag": six members, three already owned

| # | File | Region | Owner | Step |
|---|---|---|---|---|
| 1 | `internal/routing/resolver.go` | `Target.ResponsesLiveTimingsEnabled`'s doc comment | **Task 2** — do not touch | — |
| 2 | `internal/routing/resolver_live_timings_test.go` | `TestTargetResponsesLiveTimingsPrecedence`'s doc comment | **Task 2** — do not touch | — |
| 3 | `internal/routing/live_timings.go` | `LiveTimingsCapableKind`'s doc comment | **Task 2** — do not touch | — |
| 4 | `reference/api-surface.md` | the "Nothing acts on the value in this cut" sub-bullet | this task | 12 |
| 5 | `reference/data-model.md` | the `applications` table row, the `agent_runtime_specs` table row **and** the migration-80 row (**three** sentences, one file — the design and the surface map both describe two) | this task | 13 |
| 6 | `cross-cutting/agent-runtime-manager.md` | "**Nothing acts on it yet**" in the §11.5 paragraph | this task | 14 |

Every one of members 4–6 also carries the no-retry claim, which D3 keeps
**true**. Those clauses are **edited, never deleted** — and the wording differs
per site, which matters for step 17's sweep: `api-surface.md`'s sub-bullet and
`agent-runtime-manager.md`'s paragraph both say *"nothing retries without it"*
(each **wrapped across a line break**, so the literal string matches neither),
while in `data-model.md` it is the `applications` row's *"gates on it or retries
without it"*. The `agent_runtime_specs` row carries no retry clause of its own,
and the migration row's *"the injection, the gate and the retry are part 2 of
issue #81"* is **retired**, not kept — step 13b replaces it with a clause that
keeps the no-retry fact.

### Table E — also expiring with this cut

| # | File | Region | What expires | Step |
|---|---|---|---|---|
| 1 | `cross-cutting/agent-runtime-manager.md` | the §11.5 paragraph's closing sentences | part 1's reason for shipping no control: "a visible toggle that did nothing would be worse than the blank cell it promises to fix". The only site — verified by grep across `docs/`, `gateway/` | 14 |
| 2 | `reference/api-surface.md` | the "absent is not the same as `false`" sub-bullet | the capable set: "`true` for `llama_cpp`/`vllm`" | **Task 1**, step 12(a) — do not touch |
| 3 | `cross-cutting/agent-runtime-manager.md` | the §11.5 paragraph's write-rule sentence | the capable set: "is not `llama_cpp`/`vllm` is refused with **400**" | **Task 1**, step 12(b) — superseded by step 14, which carries its parenthetical forward |

Table E rows 2 and 3 are the "two documents that record the capable set" the
brief names as possibly Task 1's. They **are** Task 1's: `plan-tasks-1.md` step
12 replaces both, and this task must not re-narrow either. Row 2 needs nothing
from this task at all. Row 3 is the one place the two tasks overlap on the page:
step 14 replaces the whole run that contains Task 1's edited clause, so it
**supersedes** rather than duplicates it, and its replacement text below carries
Task 1's `vllm` parenthetical verbatim.

---

- [ ] **Step 1: The ownership sweep — read the tree, decide nothing yet**

No edit, no commit. This is the 3-minute measurement that makes every later step
safe. Run it and **write the output down**. In the blocks labelled *mine*, each
line that prints is a site still owned by you and each that prints nothing was
already handled by someone else. The block labelled **NOT mine** is the reverse:
it must print nothing, because Tasks 1 and 4 retire those sentences before this
task runs.

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection

echo "=== Set 1 (mine) ==="
grep -n -F "The precondition is a client's own" gateway/backend/internal/gateway/passthrough_usage_scan.go
grep -n -F "mid-stream only when the **client** set" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "nothing to show mid-stream unless the client asked for it" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "is READ when the client set it, and never injected" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "Native passthrough gets neither parameter" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "The precondition needed no gateway change" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "measured no-op for" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "is read when the client set it and never injected" docs/architecture/reference/api-surface.md
grep -n -F "only when the client itself asked llama.cpp" docs/architecture/cross-cutting/compatibility-and-inference.md

echo "=== Set 2 ==="
grep -n -F "already equal the upstream bytes" gateway/backend/internal/gateway/capture.go
grep -n -F "ARE the upstream bytes" gateway/backend/internal/gateway/capture.go
grep -n -F "already equal the upstream bytes" docs/architecture/cross-cutting/security-auth-rbac.md

echo "=== Set 3 (mine only) ==="
grep -n -F "**no mid-stream source**" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "There is no mid-stream count on this flavor" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "The same gate governs both columns" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "flavor has no MID-STREAM source for a count" gateway/backend/internal/gateway/passthrough_progress_test.go

echo "=== Set 4 members 4-6 ==="
grep -n -F "Nothing acts on the value in this cut" docs/architecture/reference/api-surface.md
grep -n -F "inert in this cut" docs/architecture/reference/data-model.md   # TWO hits: lines 39 and 52
grep -n -F "Nothing ACTS on the column anywhere in part 1" docs/architecture/reference/data-model.md
grep -n -F "Nothing acts on it" docs/architecture/cross-cutting/agent-runtime-manager.md

echo "=== Table E row 1 (mine) ==="
grep -rn -F "worse than the blank" docs/architecture/

echo "=== NOT mine: these must ALREADY print nothing (Tasks 1 and 4) ==="
grep -n -F "The only body edit is rewriting" gateway/backend/internal/gateway/native_passthrough.go
grep -n -F "the gateway does NOT add llama.cpp" gateway/backend/internal/gateway/native_passthrough.go
grep -n "is not something this" gateway/backend/internal/gateway/native_passthrough.go
grep -n -F "flag the client never sent, which is the" gateway/backend/internal/gateway/passthrough_progress_test.go
grep -n -F "llama_cpp\`/\`vllm" docs/architecture/reference/api-surface.md docs/architecture/cross-cutting/agent-runtime-manager.md

echo "=== ambiguity clauses (step 9) ==="
grep -n -F "with \`timings_per_token\` unset" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "records is whether the **client** asked" docs/architecture/cross-cutting/telemetry-usage-observability.md

echo "=== what Tasks 1-7 actually shipped ==="
grep -rn "wantsResponsesLiveTimings" gateway/backend/internal/gateway/ | grep -v _test.go
grep -rn "injectTimingsPerToken" gateway/backend/internal/gateway/ | grep -v _test.go
grep -n "inference request (native passthrough)" gateway/backend/internal/gateway/native_passthrough.go
grep -n "LiveOutputTokens" gateway/backend/internal/inference/types.go
grep -n "ProviderVLLM\|ProviderLlamaCPP" gateway/backend/internal/routing/live_timings.go
grep -rn "responses_live_timings\|responsesLiveTimings" gateway/frontend/src/api/ | head
```

Expected when this task runs, i.e. **after** Tasks 1–7 have landed:

- Every `grep` in the *mine* blocks — Set 1, Set 2, Set 3, Set 4 members 4–6,
  Table E row 1, and the two ambiguity clauses — prints **exactly one line**,
  with the single exception of `inert in this cut`, which prints **two** (the
  `applications` row and the `agent_runtime_specs` row). Counted on `03e2f9c`:
  **23** greps in those blocks — 22 single hits and that one double, 24 lines in
  all.
- Every `grep` in the **NOT mine** block prints **nothing**, and its exit status
  is 1. Task 4's steps 9a/9b/10a retired the three `native_passthrough.go` and
  `passthrough_progress_test.go` sentences; Task 1's step 12 narrowed both
  `` `llama_cpp`/`vllm` `` sentences.
- The last block prints the real names Tasks 1–7 shipped — which is what steps
  7, 8, 11 and 14 quote. On an untouched `03e2f9c` it would instead print
  nothing for `wantsResponsesLiveTimings`, `injectTimingsPerToken`,
  `LiveOutputTokens` and the frontend, one line for the `slog.Debug` call, and
  `ProviderVLLM` present in `live_timings.go`; if that is what you see, Tasks
  1–7 have not landed and this task is being run out of order.

**If a grep in a *mine* block prints nothing:** somebody else edited that site.
Read what they wrote, skip that part of the step, and say so in the final
summary. Do not re-edit it.

**If a grep in the NOT mine block prints a line:** Task 1 or Task 4 has not
landed, or landed differently. Do **not** fix it here — that site is theirs, and
a second author editing it is the collision this table exists to prevent. Record
it in the final summary.

---

### Step 2 — REMOVED. `native_passthrough.go`'s two comments are **Task 4's**.

No step here, and no checkbox: there is nothing for this task to do. Task 4's
step 9a rewrites `proxyNative`'s doc-comment sentence and its step 9b rewrites
the whole block comment above `upstreamBody := rewriteModelField(...)` — the
same two ranges this section used to claim (PROBLEM 1, Table A rows 1 and 2).
**Do not edit `gateway/backend/internal/gateway/native_passthrough.go` in this
task at all.** Step 8 below reads one line of it and writes nothing.

If the step-1 sweep's **NOT mine** block printed either of those sentences, Task
4 did not land as planned. Say so in the final summary; do not edit them here.

---

- [ ] **Step 3: `passthrough_usage_scan.go` — the peak-bias precondition**

Set 1 row 3. The comment says the peak-bias population is "whenever a client
asks", and that the capture is "a measured no-op for traffic that does not". The
operator's switch now selects traffic into that population.

**Red:**

```bash
grep -n -F "The precondition is a client's own" gateway/backend/internal/gateway/passthrough_usage_scan.go
```

Expected: one line (around 93).

**Edit.** In the doc comment on `usageScanner`'s `finalPromptPerSecond` /
`finalTokensPerSecond` field pair, replace:

**The last line is shared.** In the file, `for traffic that does not.` is followed
on the SAME line by ` A recorded rate is a ROUTING input — recordUsage`, which
stays. The em dashes below are the file's own; this package uses `—`, not `--`.

Replace:

```go
	// The precondition is a client's own `timings_per_token`, RELAYED UNTOUCHED
	// (a rule this path pins deliberately — see
	// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate). The
	// flag is what puts `timings` on the partials at all: the SAME PROMPT
	// REPLAYED WITHOUT the flag produced exactly ONE timings-bearing frame, the
	// terminal response.completed. A replay, not the same request — a request
	// either carried the flag or it did not. So the peak is reachable whenever a
	// client asks for mid-stream timings, and this capture is a measured no-op
	// for traffic that does not. A recorded rate is a ROUTING input — recordUsage
```

with:

```go
	// The precondition is `timings_per_token` on the OUTGOING body, and there
	// are now two ways it gets there: a client set it and proxyNative relayed it
	// untouched (pinned by
	// TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate), or
	// the operator's per-endpoint opt-in made proxyNative add it. The flag is
	// what puts `timings` on the partials at all: the SAME PROMPT REPLAYED
	// WITHOUT the flag produced exactly ONE timings-bearing frame, the terminal
	// response.completed. A replay, not the same request — a request either
	// carried the flag or it did not. So the peak is reachable on any request
	// that carries the flag, whoever put it there, and this capture is a
	// measured no-op only for traffic that carries none. Note what that widening
	// means downstream: an OPERATOR's switch, not only a client's own body, now
	// selects requests into the population whose per-frame rates exist at all —
	// including the truncated flagged stream that ends cleanly with no terminal
	// frame, whose last reported rate reaches the routing EWMA.
	// A recorded rate is a ROUTING input — recordUsage
```

**Green:**

```bash
grep -n -F "The precondition is a client's own" gateway/backend/internal/gateway/passthrough_usage_scan.go
grep -n -F "whoever put it there" gateway/backend/internal/gateway/passthrough_usage_scan.go
git diff gateway/backend/internal/gateway/passthrough_usage_scan.go | grep -E '^[-+]' | grep -v '^[-+][[:space:]]*//' | grep -v '^[-+][-+][-+]'
```

Expected: first prints nothing, second prints one line, third prints nothing.

**Commit:**

```bash
git add gateway/backend/internal/gateway/passthrough_usage_scan.go
git commit -F - <<'MSG'
docs: the peak-bias precondition is the flag on the wire, not the client who set it

usageScanner's rate-capture comment said the precondition for a timings-bearing
partial was "a client's own timings_per_token", and that the capture was a
measured no-op for traffic that does not ask. The operator's per-endpoint opt-in
now puts the same key on the same requests, so the population the comment
describes is wider than the comment says.

The consequence is not cosmetic and is spelled out rather than left implied: a
flagged stream that ends cleanly at 200 with no response.completed has per-frame
rates where it previously had none, nativeTerminalStatus records it as success,
and a success's rate reaches the mapping's throughput EWMA. PR #80 made that
figure the rate the stream reported rather than the generation's peak; what
changes here is which requests reach it at all.
MSG
```

---

- [ ] **Step 4: the terminal-usage test's doc comment**

Table C row 5 — unowned by any other task, and false the moment Task 5 lands
(PROBLEM 3). `TestPassthroughResponsesTerminalUsageBecomesVisibleBeforeTheRowLeaves`
opens by saying the `openai_responses` flavor *"has no MID-STREAM source for a
count"*. Task 5 gives it one: `timings.predicted_n` off a flagged partial. What
stays true is the **fixture's** emptiness — its partials carry no `timings`
object, because its client sends no `timings_per_token` and its resolved target
has the operator's opt-in off.

**Touch nothing else in this file.** Task 4's step 10c edits one `t.Fatalf`
inside this test's body and adds one comment above the `if strings.Contains(...)`
that guards it — a region disjoint from this doc comment. Task 4's steps 10a/10b
own `TestPassthroughResponsesStreamWithoutClientTimingsShowsTTFTOnly` (Table A
row 4), and Task 5's step 3 owns
`TestPassthroughResponsesStreamWithClientTimingsShowsTheUpstreamRate` (Table C
row 6). **No test body, no assertion, no fixture changes here.**

**Pre-read — do not write prose about code that is not there:**

```bash
grep -n "LiveOutputTokens" gateway/backend/internal/inference/types.go
```

Expected: the field declaration Task 5 added. If it prints nothing, Task 5 did
not ship, the sentence is still TRUE, and this step must be skipped and recorded
in the final summary.

**Red:**

```bash
grep -n -F "flavor has no MID-STREAM source for a count" gateway/backend/internal/gateway/passthrough_progress_test.go
```

Expected: exactly one line. **No line number**: it is 525 on `03e2f9c`, but Task
4's step 10a and Task 5's new test both add lines to this file above the target,
so the number will have moved by the time this task runs. Check the count.

**Edit.** Replace the three lines that open the doc comment's first sentence:

```go
// `openai_responses` cell the other two Responses tests leave uncovered: the
// flavor has no MID-STREAM source for a count, but the TERMINAL
// response.completed frame carries the upstream's own final
```

with:

```go
// `openai_responses` cell the other two Responses tests leave uncovered: this
// fixture's partials carry no `timings` object — its client sends no
// `timings_per_token` and its resolved target has the operator's opt-in OFF,
// so nothing injects one, and without one this fixture has no mid-stream
// source for a count — while the TERMINAL response.completed frame carries
// the upstream's own final
```

The next existing line, `` // `response.usage.output_tokens`, isTerminalUsageFrame accepts it, and ``,
continues the sentence and is unchanged. **Leave the rest of the comment alone**
— in particular *"Only the mid-stream cells are empty here"* further down stays
TRUE of this fixture and must stay.

**Green:**

```bash
grep -n -F "flavor has no MID-STREAM source for a count" gateway/backend/internal/gateway/passthrough_progress_test.go
grep -n -F "without one this fixture has no mid-stream" gateway/backend/internal/gateway/passthrough_progress_test.go
grep -n -F "Only the mid-stream cells are empty here" gateway/backend/internal/gateway/passthrough_progress_test.go
```

Expected: the first prints nothing, the second one line, the third one line
(that sentence survives deliberately).

Confirm you changed no code:

```bash
git diff gateway/backend/internal/gateway/passthrough_progress_test.go | grep -E '^[-+]' | grep -v '^[-+][[:space:]]*//' | grep -v '^[-+][-+][-+]'
```

Expected: **nothing**. (Plain `git diff` here shows only your own working-tree
change; Task 4's and Task 5's edits to this file are already committed.)

**Commit:**

```bash
git add gateway/backend/internal/gateway/passthrough_progress_test.go
git commit -F - <<'MSG'
docs: the terminal-usage test's doc names its fixture, not the whole flavor

The doc comment opened by saying the openai_responses flavor has no mid-stream
source for an output-token count. Reading llama.cpp's timings.predicted_n off a
flagged partial makes that false as a statement about the flavor, while leaving
it true as a statement about this fixture -- which sends no timings_per_token
and whose resolved target has the operator's opt-in off, so no partial here
carries a timings object at all.

The comment now says which of the two it means. Nothing else in it changes: the
sentence that only the mid-stream cells are empty is still right about this
fixture, and the paragraph explaining why the terminal frame's figure is not
suppressed for arriving late is untouched.

Comment only. No assertion, fixture or helper changed, and no other test in this
file is touched -- the two tripwire assertions and their messages belong to
other commits on this branch.
MSG
```

---

- [ ] **Step 5: Set 2 — the capture-identity claim, all three sites**

Already false on `main` because `rewriteModelField` re-serializes the object;
this cut widens the divergence from cosmetic to semantic. Three sites, one
commit — this is a single claim in three places.

**Red:**

```bash
grep -n -F "already equal the upstream bytes" gateway/backend/internal/gateway/capture.go
grep -n -F "ARE the upstream bytes" gateway/backend/internal/gateway/capture.go
grep -n -F "already equal the upstream bytes" docs/architecture/cross-cutting/security-auth-rbac.md
```

Expected: three lines total (capture.go ~40 and ~51, security doc ~737).

**Edit 5a** — `capture.go`, the `Translated*` field group's doc comment. Replace:

```go
	// Translated* hold the TRANSLATED upstream exchange (the request the gateway
	// actually sent to the Chat-Completions upstream + the raw upstream response),
	// populated only on the translate path when capturing. Empty on native
	// passthrough (its client bytes already equal the upstream bytes) and on plain
	// same-protocol requests. Request headers are redacted like the client ones.
```

with:

```go
	// Translated* hold the TRANSLATED upstream exchange (the request the gateway
	// actually sent to the Chat-Completions upstream + the raw upstream response),
	// populated only on the translate path when capturing. Empty on native
	// passthrough and on plain same-protocol requests. Request headers are
	// redacted like the client ones.
	//
	// Empty on native passthrough because that path attaches no sink, NOT because
	// its captured bytes are the upstream's. They are not, and have not been since
	// the model rewrite: rewriteModelField re-serializes the whole object whenever
	// a provider-model override applies, which reorders keys and HTML-escapes
	// <>&, and proxyNative may additionally add `timings_per_token` when the
	// operator's Responses live-timings opt-in is on. So a capture on this path
	// shows what the CLIENT sent, which is the right thing to show a tenant and
	// the wrong thing to debug an upstream 400 with — the per-request debug log
	// is what covers the second case.
```

**Edit 5b** — `capture.go`, above `func attachTranslatedCapture`. Replace:

```go
// attachTranslatedCapture copies the sink's collected upstream (translated)
// request+response onto ci, for the translate path. Nil-safe: a nil ci or a nil
// sink leaves ci unchanged. Native passthrough passes no sink because the bytes it
// already captures ARE the upstream bytes.
```

with:

```go
// attachTranslatedCapture copies the sink's collected upstream (translated)
// request+response onto ci, for the translate path. Nil-safe: a nil ci or a nil
// sink leaves ci unchanged. Native passthrough passes no sink at all, so nothing
// upstream-side is captured on that path — see the Translated* field doc for why
// that is not the same as the two byte streams being equal.
```

**Edit 5c** — `security-auth-rbac.md` §14. Replace:

```
translated *upstream* request headers are redacted with the exact same list,
so an upstream API key injected by the gateway itself never leaks into a
capture either; native passthrough has no separate translated headers to
redact because the client bytes already equal the upstream bytes.
```

with:

```
translated *upstream* request headers are redacted with the exact same list,
so an upstream API key injected by the gateway itself never leaks into a
capture either; native passthrough has no separate translated headers to
redact because **nothing upstream-side is captured on that path at all** —
neither the headers `applyUpstreamAuth` attaches nor the upstream request body.
The operative guarantee is therefore stronger than a redaction rule, and it is
worth being precise about what it is not: a native-passthrough capture holds the
**client's** bytes, which are not the upstream's. They differ whenever the model
rewrite fires (it re-serializes the object, reordering keys and HTML-escaping
`<>&`) and, since the Responses live-timings opt-in, whenever the gateway adds
`timings_per_token` to a streaming `/v1/responses` body. That divergence is
recorded in the request log rather than closed in the capture
([Telemetry, Usage & Observability
§8.4.3](telemetry-usage-observability.md#843-running-connections-active-requests)).
```

**Green:**

```bash
grep -n -F "already equal the upstream bytes" gateway/backend/internal/gateway/capture.go docs/architecture/cross-cutting/security-auth-rbac.md
grep -n -F "ARE the upstream bytes" gateway/backend/internal/gateway/capture.go
./scripts/check-docs.sh
git diff gateway/backend/internal/gateway/capture.go | grep -E '^[-+]' | grep -v '^[-+][[:space:]]*//' | grep -v '^[-+][-+][-+]'
```

Expected: the two greps print nothing; `check-docs.sh` prints `check-docs: OK`
and exits 0 (it is the gate that proves the new anchor link resolves); the last
command prints nothing.

**Commit:**

```bash
git add gateway/backend/internal/gateway/capture.go docs/architecture/cross-cutting/security-auth-rbac.md
git commit -F - <<'MSG'
docs: a native-passthrough capture holds the client's bytes, not the upstream's

Three sites -- two comments in capture.go and one paragraph in the security
document -- said the passthrough capture needs no separate upstream record
because its client bytes already equal the upstream bytes. That was false before
this branch touched anything: rewriteModelField re-serializes the whole object
whenever a provider-model override applies, reordering keys and HTML-escaping
<>&, and its own doc concedes the result is not byte-identical. The Responses
live-timings injection widens the divergence from cosmetic to semantic.

The operative security claim survives and is stated more strongly than before:
no upstream credential can leak through a passthrough capture, not because the
headers match but because nothing upstream-side is captured on that path at all
-- not the Authorization header applyUpstreamAuth attaches, not the sent body.

What is captured stays the client's bytes. Changing that is a behaviour change
for every passthrough request and would put the internal provider model name in
front of the tenant; it belongs in its own change. The gap an operator actually
feels -- debugging an upstream 400 against a body that was not sent -- is closed
by the per-request debug log instead.
MSG
```

---

- [ ] **Step 6: the per-flavor table, both `openai_responses` cells, and the reading under it**

Set 1 rows 5 and 6, Set 3 rows 1 and 2. All in
`docs/architecture/cross-cutting/telemetry-usage-observability.md` §8.4.3, within
20 lines of each other.

**Red:**

```bash
grep -n -F "**no mid-stream source**" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "mid-stream only when the **client** set" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "There is no mid-stream count on this flavor" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "nothing to show mid-stream unless the client asked for it" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: four lines (718, 719, 719, 729 today).

**Edit 6a** — the `output tokens` row's `openai_responses` cell. That row is one
physical line. Replace the third cell — everything after the second `|` — so the
row reads:

```
| output tokens | yes, from the first `message_delta` on: it carries the message's cumulative `usage.output_tokens` | **yes, once the outgoing body carries `timings_per_token`** — those partials then carry llama.cpp's own `timings.predicted_n`, and that is the count. Without the flag there is no mid-stream source at all: the `*.delta` partials carry no usage object, and counting deltas as tokens is the option this feature already rejected above. **It is an UPSTREAM count, not a count of what the client has received.** It advances only on the partials that happen to carry a `timings` object, so the series is monotone but **not contiguous** (a measured run ran 1…27 on the reasoning deltas, then 30, 31, 32 — two tokens generated across frames that carry no `timings`), and mid-stream it can trail the terminal `response.usage.output_tokens` by a token or two. That is why it travels in a field of its own (`inference.Usage.LiveOutputTokens`, never `OutputTokens`) and reaches the live counter only: the recorded row, `usage_events`, the Activity totals, the usage timeseries and the rate limiter are all built from the accumulator and never see it. **No test may assert equality between the last partial count and the recorded total** — such a test passes on cap-truncated data, where both numbers are the cap, and fails on a naturally ending generation. |
```

**Edit 6b** — the `rate` row's `openai_responses` cell, first two sentences only.
Replace:

```
mid-stream only when the **client** set `timings_per_token`, and then labelled `upstream`. There is no mid-stream count on this flavor, so the gateway derives nothing here: a `timings` object attached by the upstream to a PARTIAL frame is the only possible source, and the gateway reads one off whatever partial carries it.
```

with:

```
mid-stream when the outgoing body carries `timings_per_token` — the **client's own**, or the one the **operator's** per-endpoint opt-in injects — and then labelled `upstream`: a `timings` object attached by the upstream to a PARTIAL frame is the only upstream source, and the gateway reads one off whatever partial carries it. A `gateway`-labelled rate is reachable here too since this flavor gained a mid-stream count, in the narrow window where a timings-bearing partial carries a `predicted_n` but a `predicted_per_second` of `0.0` — the measured series opens at exactly that, and only a positive rate is stored, so the earliest flagged partials give the row an exact count and no rate, and the feature's one derivation runs over that count. That is accepted and pinned rather than suppressed: the structural invariant is that a gateway-derived rate is only ever computed over an **exact upstream count**, and `predicted_n` is exactly that, so suppressing it would mean inventing a per-flavor exception inside the single derivation this feature has.
```

Leave the rest of that cell — the MEASURED sentence, the build identifier, the
replay sentence — **unchanged**, except for edit 6c.

**Edit 6c** — in the same cell, the closing clause. Replace:

```
Either way the flag is the only thing that can put a rate on this cell mid-stream:
```

with:

```
Either way the flag is the only thing that can put an UPSTREAM-reported rate on this cell mid-stream:
```

**Edit 6d** — the paragraph under the table. **The last line is shared.** In the
file, `"nothing to show".` is followed on the SAME line by
` **Which label that rate carries is the upstream's choice,`, which stays — put
it back after the replacement's final `it there.`

Replace:

```
deferred `Active.Remove`. The honest one-line reading of the column is
therefore "nothing to show mid-stream unless the client asked for it", not
"nothing to show".
```

with:

```
deferred `Active.Remove`. The honest one-line reading of the column is
therefore "nothing to show mid-stream unless the body carried
`timings_per_token`", not "nothing to show" — and since the Responses
live-timings opt-in, the operator is the second of the two parties who can put
it there.
```

**Green:**

```bash
grep -n -F "**no mid-stream source**" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "mid-stream only when the **client** set" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "There is no mid-stream count on this flavor" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "nothing to show mid-stream unless the client asked for it" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: all four greps print nothing; `check-docs: OK`, exit 0.

Sanity-check the table still has three columns on every row:

```bash
awk 'NR>=715 && NR<=719 {n=gsub(/\|/,"|"); printf "%d: pipes=%d\n", NR, n}' docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: `pipes=4` on every one of the five lines (leading, two separators,
trailing).

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: the Responses column has a mid-stream count, and both of its cells said it does not

The per-flavor table's openai_responses column carried two claims this cut
retires. The output-tokens cell said there is no mid-stream source; the rate
cell said a mid-stream rate exists only when the CLIENT asked, and -- a second
false clause in the same cell -- that with no mid-stream count on this flavor
the gateway derives nothing here. Both cells are rewritten, and so is the
one-line reading of the column in the paragraph below them, which said there is
nothing to show unless the client asked.

The count cell states the property that matters more than its presence: it is an
UPSTREAM count, not a count of what the client received. It advances only on
partials that carry a timings object, so it is monotone but not contiguous, and
mid-stream it can trail the recorded total. That is why it rides its own field
and never the accumulator, and why no test may assert it equals the recorded
total -- an equality test passes on cap-truncated data, where both numbers are
the cap, and fails on a naturally ending generation.

The rate cell records the accepted consequence rather than hiding it: a
gateway-labelled mid-stream rate is now reachable on this flavor, because the
measured predicted_per_second series opens at 0.0 and only a positive rate is
stored, so the earliest flagged partials carry a count and no rate. Suppressing
it would mean a per-flavor exception inside the one derivation this feature has.
MSG
```

---

- [ ] **Step 7: the "Two rules" bullets — the gate's five conditions, and what stays true**

Set 1 row 7, plus the one clause the "in-flight figure is display only" bullet
needs (PROBLEM 4). Same file, §8.4.3.

**Pre-read.** Confirm what Task 2 shipped, so the prose names real things:

```bash
grep -n "func wantsResponsesLiveTimings" gateway/backend/internal/gateway/responses_live_timings.go
grep -n "liveTimingsVerdictUnsupported" gateway/backend/internal/gateway/responses_live_timings.go
```

Expected: the function signature `func wantsResponsesLiveTimings(target routing.Target, apiFlavor string, stream bool) bool`
and the constant. **If the file or the names differ, use what is actually there**
— this document names code, and a doc naming a function that does not exist is a
worse defect than the one being fixed.

**Red:**

```bash
grep -n -F "is READ when the client set it, and never injected" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: exactly one line. **No line number is quoted from here on**, in this
step or in steps 8–11: edits 6a–6d above already move everything below the
per-flavor table, and step 7's own replacement moves it much further. Check the
*count* of lines a grep prints, never the number it prints them with.

**Edit 7a** — replace the whole first bullet (from
``- **`timings_per_token` is READ when the client set it, and never injected.**``
down to and including `rewritten client request is not.`) with:

```
- **`timings_per_token` is added to a relayed body only where the operator asked
  for it, and never over a value the client set itself.** It is the one request
  parameter the gateway ever adds on this path, and the second of the only two
  body edits `proxyNative` makes — the other being the model rewrite.
  `wantsResponsesLiveTimings` (`responses_live_timings.go`) is the whole of the
  decision, and it answers true only when **all five** of these hold:
  1. `Target.ResponsesLiveTimingsEnabled` — the operator's per-endpoint opt-in,
     resolved spec-over-application, the same precedence `responses_mode` uses
     ([API Surface](../reference/api-surface.md#api-variant-endpoint-modes-responses_mode--messages_mode));
  2. the effective upstream kind is `llama_cpp`. For a `server_agent` target that
     is `Target.LiveProgressSpecType`; `Target.Provider` holds the literal
     `server_agent` there and is the wrong field to read;
  3. the request is the **Responses** flavor — the *fine* flavor, taken as a
     parameter, because `Target.APIFlavor` is the coarse `openai`/`anthropic` one
     and cannot tell `/v1/responses` from `/v1/chat/completions`;
  4. the request is **streaming** — a buffered body has no partial frames to
     time, and gets no live counter either;
  5. the stored live-progress verdict is not an explicit `"unsupported"`.

  Point 5 is a **veto, not a requirement.** Requiring a *positive* verdict would
  make the switch silently dead wherever the capability probe never ran, which is
  the worst failure available to a control an operator has deliberately switched
  ON; and within llama.cpp a positive verdict allows nothing the veto has not
  already allowed. The verdict's vocabulary is load-bearing:
  `Target.LiveProgressSupport` speaks `""` / `"supported"` / `"unsupported"`, so
  a veto written against the capability row's own `"no"` would never fire. Point
  2 re-checks the **kind** rather than trusting the portal's write rule, because
  the store is policy-free by design — its parity fixture deliberately seeds a
  `true` on an incapable row — so a restored dump or a direct write can produce
  one the request path must not act on.

  A client that sent `timings_per_token` **itself** — `true` or `false` — has its
  body forwarded unchanged: the injection tests for the key's *presence*, not its
  value. Overwriting an explicit client `false` would be the silently rewritten
  client request this path refuses to be; the accepted cost is that such a client
  makes the operator's switch ineffective for its own requests, with nothing on
  the panel explaining why.

  **Nothing retries without it.** An upstream that rejects the key answers the
  client's request with its own 4xx. That residual is measured-small rather than
  overlooked: on the build this was measured against, `/v1/responses` answered a
  request carrying an entirely fabricated top-level key with an ordinary
  completion, and llama.cpp's request schema is pull-based, so a key nobody asks
  for is never inspected. The blast radius is one application, the operator can
  switch it off through either portal surface, and the failure is immediate and
  visible rather than silent. What makes it *diagnosable* is the log field in the
  next paragraph, not the capture.
```

**Edit 7b** — append to the second bullet, after `…would otherwise hide.`:

```
  What the opt-in does change is **which traffic** reaches the end-of-request
  feed at all. A flagged stream carries per-frame rates where an unflagged one
  carries none, including the truncated stream that ends cleanly at 200 with no
  `response.completed` — recorded `status = "success"`, so its last reported rate
  is a routing input like any complete stream's (see "A response with no
  authoritative frame records the LAST rate its own frames reported" further
  down). The operator's switch, not only a client's own body, now selects
  requests into that population. The recorded figure is still the rate the stream
  reported rather than the generation's peak, and must stay that way.
```

**Green:**

```bash
grep -n -F "is READ when the client set it, and never injected" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "The in-flight figure is display only" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: the first prints nothing; the second still prints one line (**that rule
stays**); `check-docs: OK`, exit 0 — which is what proves the new
`api-surface.md#…` anchor resolves. If check 1 reports
`anchor not found in docs/architecture/reference/api-surface.md`, the heading
slug differs from the guess: re-derive it from the heading
`#### API-variant endpoint modes (...)` in that file and fix the link.

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: the canonical rule becomes the gate, with its five conditions written out

Section 8.4.3's first standing rule said timings_per_token is read when the
client set it and never injected. It is replaced by the rule that now holds: the
key is added only where the operator asked, never over a value the client set,
and never with a retry.

All five gate conditions are stated, because four of them have no negative test
anywhere else and a gate that forgets one leaves the whole suite green: the
operator opt-in, the llama.cpp kind read from LiveProgressSpecType rather than
Provider, the FINE Responses flavor, the stream flag, and the recorded-rejection
veto. The two that get argued rather than listed are the ones a later reader
would otherwise "tighten": the verdict is a veto and not a requirement, because
requiring a positive verdict makes the switch dead wherever the probe never ran;
and the gate re-checks the kind rather than trusting the portal, because the
store is policy-free by design and its parity fixture seeds exactly that state.

The second rule -- the in-flight figure is display only -- stays true and stays.
What it gains is one clause naming the consequence that is easy to miss: the
switch changes which traffic reaches the end-of-request EWMA feed, because a
flagged stream truncated without a terminal frame now has per-frame rates where
it had none, and is recorded as a success.
MSG
```

---

- [ ] **Step 8: D9 — the log field, and the operator-facing sentence about the capture**

The brief's two remaining pieces of new prose. Same file, §8.4.3, as a new
paragraph immediately after the "Two rules" bullet list and before
`**What this surface does and does not distinguish**`.

**Pre-read — this step must not invent the field name:**

```bash
grep -n "inference request (native passthrough)" gateway/backend/internal/gateway/native_passthrough.go
```

Expected: one `slog.Debug` line. Read its attribute list and take the **exact**
key Task 4 added (something of the shape `"timings_per_token_injected", …`).
Substitute it for `<FIELD>` below, and quote it with backticks.

If the line carries **no** such attribute, Task 4 did not ship D9. **Stop and say
so in the summary** rather than documenting a field that does not exist: D9 is
what makes D2's accepted residual diagnosable, and a document promising a grep
that returns nothing is worse than one that promises nothing.

**Red:**

```bash
grep -n -F "the recorded request body is not the body that was sent" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: **nothing** — this paragraph is new. (This step's red/green is
inverted: empty before, one line after.)

**Edit.** Insert:

```
**While the switch is on, the recorded request body is not the body that was
sent.** The payload capture on this path records the **client's** bytes —
`buildCaptureInput` is handed the raw client body, never the body `proxyNative`
built — and that has already diverged from the upstream bytes since long before
this feature, because `rewriteModelField` re-serializes the whole object whenever
a provider-model override applies. The injected `timings_per_token` widens the
divergence from cosmetic to semantic. Changing what is captured is deliberately
out of scope here: it is a behaviour change for **every** passthrough request and
would put the internal provider model name in front of the tenant. What closes
the operator's gap instead is the **request log** — `proxyNative`'s per-request
`inference request (native passthrough)` debug line carries `<FIELD>`, so an
operator holding an unexplained upstream 4xx has one grep that says whether the
gateway added a key the capture does not show. No wire change, no DTO field,
nothing tenant-visible. The panel gains no "we asked" state either: when the key
is injected, the request succeeds and no partial carries `timings`, the row is
byte-identical to one where nothing was injected — and the panel's job is to
report the number and where it came from, not who asked for it. Revisit that if
the case is ever observed.
```

**Green:**

```bash
grep -n -F "the recorded request body is not the body that was sent" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: one line; `check-docs: OK`, exit 0.

Then prove the documented field really exists — the whole point of the pre-read:

```bash
FIELD=$(grep -A 12 'inference request (native passthrough)' gateway/backend/internal/gateway/native_passthrough.go \
  | grep -o '"[a-z_]*timings[a-z_]*"' | tr -d '"' | head -1)
echo "FIELD=[$FIELD]"
[ -n "$FIELD" ] || { echo "no timings attribute on the debug line - see the pre-read"; false; }
grep -c -- "$FIELD" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: `FIELD=[…]` naming the attribute Task 4 really added — the same string
you wrote into the document — and then `1` or more. If it prints `0`, the name
in the document is not the name in the code.

**The `[ -n "$FIELD" ]` guard is load-bearing, not decoration.** Without it, a
Task 4 that named the field something this pattern does not match leaves `FIELD`
empty, `grep -c -- ""` matches **every** line of the document and prints its
whole line count, and a check whose expectation is "1 or more" passes on
exactly the failure it exists to catch. If the guard fires, go back to the
pre-read: either the attribute is named differently (adjust the pattern to the
real name and re-run) or Task 4 shipped no D9 field at all, in which case this
whole step is skipped and reported.

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: say plainly that a captured passthrough body is not the body that was sent

Three deferrals in this cut are each defensible alone and are not defensible
together: no retry, no capture change, and no panel state saying the gateway
asked. Their sum is an operator who opens a capture after an upstream 400, sees
the client's body -- a body that would not have earned that 400 -- and has
nothing anywhere telling them a key was added.

The cheapest honest close was neither a capture change nor a DTO field: the
per-request debug line records the injection. This paragraph documents that,
together with the operator-facing sentence the whole switch needs -- while it is
on, the recorded request body is not the body that was sent -- and the reason
the capture is left alone: changing it is a behaviour change for every
passthrough request and would put the internal provider model name in front of
the tenant.

It also records why the panel gains no "we asked" state: with the key injected,
a 200, and no timings on any partial, the row is byte-identical to one where
nothing was injected, and who asked for a number is not a question the panel
answers.
MSG
```

---

- [ ] **Step 9: the two clauses that become ambiguous rather than false**

PROBLEM 4. Two one-clause edits, same file.

**Red:**

```bash
grep -n -F "shapes that carry a \`timings\` object with \`timings_per_token\` unset" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "records is whether the **client** asked for mid-stream timings" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: exactly two lines, one from each grep. No line numbers are quoted:
steps 6–8 have already inserted well over sixty lines above both targets.

**Edit 9a** — in the `speculation_observed` paragraph, replace:

```
shapes that carry a `timings` object with `timings_per_token` unset** — measured
```

with:

```
shapes that carry a `timings` object with `timings_per_token` unset — unset by the
client AND not injected by the operator's Responses live-timings opt-in** — measured
```

**Edit 9b** — in "What this surface does and does not distinguish". **The last
line is shared.** In the file, `turns on.` is followed on the SAME line by
` So the tooltip is not claiming that passthrough and translation are`, which
stays — put it back after the replacement's final `turns on.`

Replace:

```
on the DTO records is whether the **client** asked for mid-stream timings — and
on the passthrough Responses path that is precisely the axis an absent rate
turns on.
```

with:

```
on the DTO records is whether **anyone** asked for mid-stream timings — neither
the client's own `timings_per_token` nor the operator's opt-in that injects it —
and on the passthrough Responses path that is precisely the axis an absent rate
turns on.
```

**Green:**

```bash
grep -n -F "unset by the client AND not injected" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "records is whether **anyone** asked" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "records is whether the **client** asked" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: first two print one line each, third prints nothing, `check-docs: OK`.

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: two "the client asked" clauses now have a second party to name

Neither sentence became false, which is why neither is rewritten: the three
shapes that carry a timings object with the flag unset are still those three,
and nothing on the ActiveRequest DTO still records who asked for mid-stream
timings. Both became ambiguous, because "unset" and "the client asked" each had
exactly one possible subject when they were written and now have two.

Named here so a later reader does not read them as oversights and correct a
sentence that is right.
MSG
```

---

- [ ] **Step 10: the "neither parameter" paragraph and the measurement bullet**

Set 1 rows 8 and 9. Same file, further down §8.4.3.

**Red:**

```bash
grep -n -F "Native passthrough gets neither parameter" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "measured no-op for" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: exactly two lines, one from each grep — no line numbers, for the
reason step 7 gives. Note the second pattern is deliberately
short: the full sentence is **wrapped across two source lines**
(`… is a measured no-op for` / `traffic that does not.`), so a `grep -F` for the
whole sentence matches nothing and would read as a false green.

**Edit 10a. The last line is shared.** In the file,
`path must survive any later change" above).` is followed on the SAME line by
` A **buffered** passthrough request`, which stays — put it back after the
replacement's final `nothing else touched.`

Replace:

```
**Native passthrough gets neither parameter — and gets the live figures its own
relayed frames can support, which is not the same statement.** `proxyNative`
forwards the client's own body unmodified (only the `model` field is ever
rewritten, and losslessly), so neither parameter is ever added on this path;
`timings_per_token` is read when the client set it and never injected, which is
a decision with its own reasons rather than an omission (see "Two rules on this
path must survive any later change" above).
```

with:

```
**Native passthrough gets neither of `CompleteStream`'s parameters — and gets the
live figures its own relayed frames can support, which is not the same
statement.** `stream_options.continuous_usage_stats` is never added on this path
at all, and `timings_per_token` is added only under the operator's per-endpoint
opt-in and the four other conditions the gate applies (see "Two rules on this
path must survive any later change" above) — never through `wantsLiveProgress`,
whose verdict describes the *completion* endpoint's parameter schema and which is
deliberately not reused here. Everything else `proxyNative` forwards is the
client's own body, with the `model` field losslessly rewritten and nothing else
touched.
```

**Edit 10b** — replace the tail of the "**The precondition needed no gateway
change.**" bullet. The bullet's opening claim is historically true and stays; its
closing sentence is not. **The last line of this first block is shared:** in the
file, `rule above).` is followed on the SAME line by
` Measured **direct to the runtime router**, with the flag set:`, which stays —
put it back after the replacement's final `under the gate above.`

Replace:

```
- **The precondition needed no gateway change.** A client that sets
  `timings_per_token` itself has the flag relayed untouched (the non-injection
  rule above).
```

with:

```
- **The precondition needed no gateway change at the time this was measured.** A
  client that sets `timings_per_token` itself has the flag relayed untouched, and
  that is still true — but it is no longer the only way the flag reaches the
  upstream, since the operator's opt-in injects it under the gate above.
```

and then, further down in the **same** bullet — after the five measurement
sentences (`Measured **direct to the runtime router**…` through
`(A replay, not the same request — a request either`) — replace the bullet's
closing sentence. Unlike the block above, all three of its lines are **whole**
source lines — nothing is shared. The source wraps exactly like this:

```
  carried the flag or it did not.) So the peak is reachable for any client that
  asks for mid-stream timings, and this substitution is a measured no-op for
  traffic that does not.
```

with:

```
  carried the flag or it did not.) So the peak is reachable on any request that
  carries the flag, whoever put it there, and this substitution is a measured
  no-op only for traffic that carries none.
```

**Green:**

```bash
grep -n -F "measured no-op for" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "so neither parameter is ever added on this path" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "no-op only for traffic that carries none" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: the first two print nothing (the replacement wraps as `… is a
measured` / `no-op only for traffic …`, so the old three-word run is gone), the
third prints one line, `check-docs: OK`.

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: separate the two live-progress parameters, since only one of them is still never added

The paragraph said native passthrough gets neither of the translate path's two
extra parameters and that timings_per_token is read but never injected. Half of
that survives: stream_options.continuous_usage_stats is still never added here,
and vLLM's own live-rate parameter stays out of scope. The other half does not.

It now also says what still does NOT decide the question, because reusing it is
the obvious wrong move: wantsLiveProgress is not consulted on this path. Its
verdict describes the completion endpoint's parameter schema, its weakest layer
is a shape guess whose safety on the translate path comes from a retry this path
does not have, and a fourth check is ANDed at its only call site.

The #80 measurement bullet keeps its historical claim -- the precondition needed
no gateway change at the time it was measured -- and loses the clause that made
it a statement about the present: a client is no longer the only party who can
put the flag on the wire.
MSG
```

---

- [ ] **Step 11: the authoritative-frame gate paragraph**

Set 3 row 3 — unowned, and false as soon as Task 5 lands.

**Pre-read:**

```bash
grep -n "LiveOutputTokens" gateway/backend/internal/inference/types.go gateway/backend/internal/gateway/passthrough_usage_scan.go
```

Expected: the field declaration and its read inside `publishProgress`. If the
field is named differently, use the real name.

**Red:**

```bash
grep -n -F "The same gate governs both columns" docs/architecture/cross-cutting/telemetry-usage-observability.md
```

Expected: exactly one line — no line number, for the reason step 7 gives.

**Edit.** Replace:

```
**The same gate governs both columns.** `usageScanner.publishProgress`
publishes an output-token count to the live counter only from an authoritative
frame, for a sharper version of the identical reason: the recorded row can
tolerate a placeholder because it is presented as a *count*, whereas
`liveProgressDTO` would divide that `1` by the generation window and DISPLAY the
quotient as a measured *rate* for the rest of the stream.
```

with:

```
**The same gate still governs the frame's own `OutputTokens`, and no longer
governs the live column alone.** `usageScanner.publishProgress` publishes **this
frame's** `OutputTokens` to the live counter only from an authoritative frame,
for a sharper version of the identical reason: the recorded row can tolerate a
placeholder because it is presented as a *count*, whereas `liveProgressDTO` would
divide that `1` by the generation window and DISPLAY the quotient as a measured
*rate* for the rest of the stream. What a **non**-authoritative frame may publish
is the separate `LiveOutputTokens` — llama.cpp's `timings.predicted_n`, a number
the upstream reported for **itself** rather than a placeholder the merge cannot
distinguish. That is the whole reason the two travel in different fields: the
gate exists to keep a placeholder off the live column, and an upstream's own
per-frame count is not one.
```

Leave the rest of the paragraph — "One predicate, one definition per flavor,
three consumers…" — unchanged.

**Green:**

```bash
grep -n -F "The same gate governs both columns" docs/architecture/cross-cutting/telemetry-usage-observability.md
grep -n -F "no longer governs the live column alone" docs/architecture/cross-cutting/telemetry-usage-observability.md
./scripts/check-docs.sh
```

Expected: first prints nothing, second one line, `check-docs: OK`.

**Commit:**

```bash
git add docs/architecture/cross-cutting/telemetry-usage-observability.md
git commit -F - <<'MSG'
docs: the authoritative-frame gate governs the frame's own count, not the live column

The paragraph said publishProgress puts an output-token count on the live
counter only from an authoritative frame. Reading timings.predicted_n makes that
false: a non-authoritative frame now publishes LiveOutputTokens.

The gate's REASON is what decides which half survives, and the rewrite leads
with it. The gate exists because mergePassthroughUsage max-merges every usage
object it sees and so cannot tell Anthropic's message_start placeholder of 1
from a real total, and liveProgressDTO would divide that 1 by the generation
window and display the quotient as a measured rate. An upstream's own per-frame
predicted_n is not a placeholder -- it is a fact the upstream reported about
itself -- which is precisely why it rides a field of its own rather than being
merged into OutputTokens and gated alongside it.
MSG
```

---

- [ ] **Step 12: `api-surface.md` — what the value does**

Set 4 member 4 and Set 1 row 10, which are the **same** sub-bullet, so one
replacement retires both claims. **Table E row 2 — the kind-dependent create
default — is Task 1's**, step 12(a), in the *next* sub-bullet down. Do not touch
it, and do not re-narrow it here.

**Red:**

```bash
grep -n -F "Nothing acts on the value in this cut" docs/architecture/reference/api-surface.md
grep -n -F "\`llama_cpp\`/\`vllm\`" docs/architecture/reference/api-surface.md
```

Expected: the first prints **one** line (536 — nothing in this task or Task 1
moves it) and the second prints **nothing**, because Task 1's step 12(a) already
narrowed that default. If the second prints a line, Task 1 has not landed:
record it and still do not edit it — it is theirs.

**Edit 12a** — replace the whole first sub-bullet (from
`  - **Nothing acts on the value in this cut,` down to the closing `§8.4.3](...)).`)
with:

```
  - **What the value does.** On a **streaming** `/v1/responses` request that this
    application — or, for a `server_agent` model, its runtime spec — serves in
    `passthrough` mode to a `llama_cpp` upstream, the gateway adds
    `"timings_per_token": true` to the body it forwards. llama.cpp then attaches
    a `timings` object to the partial frames, and the running-connections panel
    shows an upstream-reported tokens/sec and an upstream-reported output-token
    count for the whole request instead of a blank cell. The stored value is
    resolved onto the request's routing target spec-over-application, the same
    precedence `responses_mode` uses. **Five conditions gate the injection and
    nothing retries without it** — an upstream that rejects the key answers the
    client's request with its own 4xx ([Telemetry, Usage & Observability
    §8.4.3](../cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)).
    A body that already carries `timings_per_token`, `true` **or** `false`, is
    forwarded unchanged, so a client that sends `false` makes the flag
    ineffective for its own requests. Nothing else about the request changes,
    and no response field the client sees is removed or rewritten — the client
    receives llama.cpp's frames as llama.cpp emits them, extra `timings` objects
    included. **While the flag is on, an encrypted payload capture for such a
    request shows the body the CLIENT sent, not the body that was sent
    upstream**; the injection is recorded on the gateway's per-request debug log
    line instead.
```

**There is no edit 12b.** The capable-set default in the sub-bullet below is
Task 1's, per Table E row 2.

**Green:**

```bash
grep -n -F "Nothing acts on the value in this cut" docs/architecture/reference/api-surface.md
grep -n -F "nothing retries without it" docs/architecture/reference/api-surface.md
./scripts/check-docs.sh
```

Expected: the first prints nothing, the second **one** line — the replacement
keeps that clause on a single source line on purpose, so the sweep in step 17
can find it — and `check-docs: OK`, exit 0.

**Commit:**

```bash
git add docs/architecture/reference/api-surface.md
git commit -F - <<'MSG'
docs: the API reference says what responses_live_timings_enabled does

Part 1's bullet opened by saying nothing acts on the value, that the upstream
parameter is not injected, that nothing gates on it and that nothing retries
without it. Three of those four clauses expire with this cut; the fourth stays
true and is kept rather than dropped, because "no retry" is a decision an API
client has to know about: an upstream that rejects the key answers the client's
own request with a 4xx.

The bullet now leads with what a caller actually gets for setting the flag, and
carries the two consequences a caller cannot discover by reading the schema: a
body that already carries timings_per_token -- true or false -- is forwarded
unchanged, so a client that sends false silently defeats the operator's switch;
and while the flag is on, an encrypted payload capture shows the client's body
rather than the one that was sent.

The kind-dependent create default in the next sub-bullet is not touched here. It
narrows from llama_cpp/vllm to llama_cpp in its own commit on this branch, with
the vLLM measurement that justifies it.
MSG
```

---

- [ ] **Step 13: `data-model.md` — three rows**

Set 4 member 5. **Three** sentences in one file, one commit. The design's §4 and
the surface map both describe this member as two; counted by hand it is three —
the `agent_runtime_specs` table row carries the same "inert in this cut" clause
by reference to the `applications` one, and leaving it is exactly the silent
contradiction this task exists to prevent.

**Red:**

```bash
grep -n -F "inert in this cut" docs/architecture/reference/data-model.md
grep -n -F "Nothing ACTS on the column anywhere in part 1" docs/architecture/reference/data-model.md
```

Expected: **three** lines — `inert in this cut` on line 39 (`applications`) and
on line 52 (`agent_runtime_specs`), plus the migration row on 467.

**Edit 13a** — in the `applications` table row (one long physical line), replace:

```
orthogonal to `responses_mode`, default off, and **inert in this cut**: the write rules are enforced and the value reaches the request's routing target, but nothing injects the upstream parameter, gates on it or retries without it, so setting it changes no request's behaviour — part 2 of issue #81)
```

with:

```
orthogonal to `responses_mode`, default off; when it is on, a streaming `passthrough` `/v1/responses` request to a `llama_cpp` upstream has `timings_per_token` added to the body the gateway forwards, which is what puts a live tokens/sec and a live token count on the running-connections row. Five conditions gate the injection and **nothing retries without it**)
```

**Edit 13b** — in the migration-80 row, replace:

```
Nothing ACTS on the column anywhere in part 1 of this feature — the portal surfaces do read it, to enforce the write rules and to echo it back, and the value is resolved onto the request's routing target, but no inference request behaves differently for it: the injection, the gate and the retry are part 2 of issue #81.
```

with:

```
The request path reads the resolved value: on a streaming `passthrough` `/v1/responses` request to a `llama_cpp` upstream the gateway adds `timings_per_token` to the body it forwards, under a five-condition gate and with **nothing retrying without it** ([Telemetry, Usage & Observability §8.4.3](../cross-cutting/telemetry-usage-observability.md#843-running-connections-active-requests)). The column itself stays policy-free: an incapable kind may hold a `true` — the portal refuses to write one, never SQL — so the request path re-checks the kind rather than trusting the stored row.
```

**Edit 13c** — in the `agent_runtime_specs` table row, replace:

```
orthogonal to `responses_mode`, default off, and **inert in this cut** for the same reason as the `applications` column above; for a `server_agent` mapping the resolved spec, not the parent application, is the authority)
```

with:

```
orthogonal to `responses_mode`, default off, and read by the request path for the same reason as the `applications` column above; for a `server_agent` mapping the resolved spec, not the parent application, is the authority — and spec-over-application precedence means a spec row without the flag resolves it back to `false` whatever the parent says)
```

**Green:**

```bash
grep -n -F "inert in this cut" docs/architecture/reference/data-model.md
grep -n -F "Nothing ACTS on the column anywhere in part 1" docs/architecture/reference/data-model.md
./scripts/check-docs.sh
awk 'NR==39 || NR==52 || NR==467 {n=gsub(/\|/,"|"); printf "%d: pipes=%d\n", NR, n}' docs/architecture/reference/data-model.md
```

Expected: the two greps print nothing; `check-docs: OK`; the `awk` prints
`pipes=3` for lines 39 and 52 (two-column table) and `pipes=4` for the migration
row (three-column table). **Compare against what those rows printed before the
edit** — if a count changed you introduced a stray `|` into a cell.

**Commit:**

```bash
git add docs/architecture/reference/data-model.md
git commit -F - <<'MSG'
docs: the data model stops calling responses_live_timings_enabled inert

Three rows said nothing acts on the column -- the applications table row,
migration 80's own row, and the agent_runtime_specs row, which carries the claim
by reference to the applications one and is the easiest of the three to miss.
The write rules are enforced, the value reaches the routing target, and now the
request path reads it.

The spec row also states the precedence consequence an operator trips over: a
spec that exists without the flag resolves it back to false whatever the parent
application says, because for a server_agent model the spec is the authority for
the endpoint behaviour the flag qualifies.

The migration row also records the property a schema reader most needs and
cannot see in the DDL: the column is policy-free on purpose. The "an incapable
kind may not store true" refusal lives in the portal as a 400 naming the type,
never in SQL, and the store's parity fixture deliberately round-trips a true for
any kind -- which is exactly why the request-path gate re-checks the kind
instead of trusting the stored row.

The clause that says nothing retries without it is kept in both rows. That
decision did not expire with this cut.
MSG
```

---

- [ ] **Step 14: `agent-runtime-manager.md` — the spec-side paragraph**

Set 4 member 6, Set 1 row 12, Table E row 1 — three expiring claims this task
owns, in one paragraph, one commit. Table E row 3, the capable set, is **Task
1's** and is superseded rather than re-edited: the run replaced below spans it.

**Pre-read — what the portal actually shipped:**

```bash
grep -rn "responses_live_timings\|responsesLiveTimings" gateway/frontend/src/api/ gateway/frontend/src/components/shared/ | head -20
```

Expected after Task 7: hits in `api/models.ts`, `api/runtime.ts` and
`components/shared/ApiVariantControls.tsx`. **Write the sentence about the
control from what you find, not from this plan.** If the frontend has no hits,
Task 7 did not ship and you must keep — not delete — the "no operator control"
sentence, and say so in the summary.

**Red:**

```bash
grep -n -F "Nothing acts on it" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -n -F "worse than the blank" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -n -F "\`llama_cpp\`/\`vllm\`" docs/architecture/cross-cutting/agent-runtime-manager.md
```

Expected: the first two print **one line each** (3751 and 3762 on an untouched
tree; Task 1's step 12(b) rewraps one clause inside this same run, so check the
count, not the number). The third prints **nothing** — Task 1's step 12(b) has
already narrowed that clause.

**There is no "skip that part" here, and no duplicate edit either.** The run this
step replaces *contains* Task 1's edited clause, so it cannot be edited around;
the replacement below **supersedes** that run and carries Task 1's parenthetical
forward verbatim. That makes this step idempotent with respect to Task 1: if the
third grep does print a line, Task 1 has not landed, and the replacement below
still leaves the paragraph in exactly the state it should be in. Record it and
apply the step unchanged.

**Edit.** Replace the run from `made. **Will, not does:**` through
`cell it promises to fix.` with:

```
made. **And the request path reads it now:** on a **streaming** `/v1/responses`
request this spec serves in `passthrough` mode to a `llama_cpp` upstream, the
gateway adds `"timings_per_token": true` to the body it forwards, which is what
makes llama.cpp attach a `timings` object to the partial frames and fills the
running-connections row's live tokens/sec and live output-token count. Five
conditions gate it — the opt-in itself, the effective kind, the Responses
flavor, the stream flag, and a recorded live-progress rejection as a veto — and
**nothing retries without it**, so an upstream that rejects the key answers the
client's request with its own 4xx ([Telemetry, Usage & Observability
§8.4.3](telemetry-usage-observability.md#843-running-connections-active-requests)).
Its write rule is enforced on the spec's own kind rather than the operator's
optimism — a PUT that sets it `true` on a spec whose **effective** type (the
explicit `type`, else detected from `binary`) is not `llama_cpp` is refused with
**400** (`vllm` left that set on 2026-09-12, measured inert on `/v1/responses`),
never 409, because the document always carries the type it is judged against; a
PUT that omits it on such a type clears any stored `true`. **The
checkbox ships on this form too**, on the same shared `ApiVariantControls` block
the flavor and mode dropdowns live on, so the value is no longer reachable
through the API alone. It is deliberately **permissive here in a way it is not on
the application form**: that form knows `type` directly, while this one's only
capability signal is the read-only echo of the *loaded* spec's effective type —
`undefined` on create, and stale the moment `binary` is edited, since the
detection itself is Go-only from a binary basename. So this form can offer a
combination the backend refuses, and the documented 400 is what answers it.
```

**Green:**

```bash
grep -n -F "Nothing acts on it" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -rn -F "worse than the blank" docs/architecture/
grep -n -F "\`llama_cpp\`/\`vllm\`" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -n -F "The checkbox ships on this form too" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -n -F "nothing retries without it" docs/architecture/cross-cutting/agent-runtime-manager.md
grep -n -F "measured inert on" docs/architecture/cross-cutting/agent-runtime-manager.md
./scripts/check-docs.sh
```

Expected: the first three print nothing, the last three one line each,
`check-docs: OK`, exit 0.

The fifth grep is not decoration. In the text this step replaces, that clause
**wraps across a line break** (`… and nothing` / `retries without it …`), which
is why a literal `grep -rn -F "nothing retries without it" docs/architecture/`
finds nothing at all on `03e2f9c`. The replacement above deliberately puts the
whole clause on one source line so the step-17 sweep can see it; if your
re-wrapping breaks it across lines again, this grep is what catches you. The
sixth proves Task 1's parenthetical survived the supersession.

**Commit:**

```bash
git add docs/architecture/cross-cutting/agent-runtime-manager.md
git commit -F - <<'MSG'
docs: the runtime-spec paragraph stops promising a flag that does nothing

One paragraph in §11.5 carried three claims that expire with this cut: that the
request path will read the value rather than does; that nothing acts on it; and
part 1's own reason for shipping no operator control -- a visible toggle that
did nothing would be worse than the blank cell it promises to fix. The
capable-set clause in the same paragraph was narrowed to llama_cpp in its own
commit on this branch; this rewrite spans it and carries that narrowing, and its
vLLM parenthetical, forward unchanged. That reason was good and is spent: the
toggle does something now, and until it shipped an operator with a pre-existing
llama.cpp application had to PATCH the API by hand, because migration 80's
default is off and the kind-dependent true applies only on create.

The clause that says nothing retries without it survives, because that decision
did not expire.

The replacement records the asymmetry between the two forms rather than hiding
it. The application form knows its type directly and can gate the checkbox; this
one's only capability signal is a read-only echo of the LOADED spec's effective
type, which is undefined on create and stale the moment the binary field is
edited -- the detection is Go-only, from a binary basename. So the spec form is
permissive and the documented 400 answers an impossible pair.
MSG
```

---

- [ ] **Step 15: `compatibility-and-inference.md` — the passthrough trade-off**

Set 1 row 11 and Table C row 4 — one paragraph carrying both claims, so one
replacement retires both (PROBLEM 3).

**Red:**

```bash
grep -n -F "only when the client itself asked llama.cpp" docs/architecture/cross-cutting/compatibility-and-inference.md
```

Expected: one line (380).

**Edit.** Replace:

```
wire cannot supply: on the Responses shape **no** mid-stream token count at all
(the `*.delta` partials carry no usage object, and the terminal
`response.completed` frame is what finally lands one), a mid-stream **rate**
only when the client itself asked llama.cpp for `timings_per_token`, and
nothing at all on a **buffered** response, which has no frames to time. The
per-flavor detail, and the two rules that keep it honest — the relayed body is
never augmented to improve a display column, and no in-flight figure ever
becomes a routing input — are in [Telemetry, Usage Analytics & Observability
§8.4.3](telemetry-usage-observability.md#843-running-connections-active-requests).
```

with:

```
wire cannot supply — and on the Responses shape what the wire supplies now
depends on one operator opt-in. Without `timings_per_token` on the outgoing body
there is **no** mid-stream token count and **no** mid-stream rate: the `*.delta`
partials carry no usage object, and the terminal `response.completed` frame is
what finally lands one. With it, those same partials carry llama.cpp's own
`timings`, and the row gets both an upstream-reported rate and an
upstream-reported count while the request is still running. The flag reaches the
body either because the **client** set it, in which case it is relayed untouched,
or because the application's (or runtime spec's)
`responses_live_timings_enabled` opt-in is on and the request is a streaming
`/v1/responses` passthrough to a `llama_cpp` upstream. A **buffered** response
still gets nothing at all, having no frames to time. The per-flavor detail, the
five conditions that gate the injection, and the two rules that keep it honest —
a relayed body is augmented only where the operator asked and never over a value
the client set itself, and no in-flight figure ever becomes a routing input — are
in [Telemetry, Usage Analytics & Observability
§8.4.3](telemetry-usage-observability.md#843-running-connections-active-requests).
```

**Green:**

```bash
grep -n -F "only when the client itself asked llama.cpp" docs/architecture/cross-cutting/compatibility-and-inference.md
grep -n -F "never over a value the client set itself" docs/architecture/cross-cutting/compatibility-and-inference.md
./scripts/check-docs.sh
```

Expected: first prints nothing, second one line, `check-docs: OK`.

**Commit:**

```bash
git add docs/architecture/cross-cutting/compatibility-and-inference.md
git commit -F - <<'MSG'
docs: what choosing passthrough costs on the Responses shape now depends on an opt-in

The endpoint-modes chapter's account of the trade-off said a Responses
passthrough row has no mid-stream token count at all and a mid-stream rate only
when the client itself asked. Both halves change, and it also restated the
non-augmentation rule in the form that is now wrong.

It is rewritten as a conditional rather than a flat absence, because that is
what an operator choosing between translate and passthrough needs: without
timings_per_token on the outgoing body the two cells are empty, with it they
both fill, and the flag gets there either from the client or from the
application's own responses_live_timings_enabled opt-in on a streaming
Responses passthrough to llama.cpp. A buffered response still gets nothing,
which has not changed and is not a gap to close.
MSG
```

---

- [ ] **Step 16: the accepted residual goes on the risk register**

D2 calls the upstream-rejection case an *"Accepted residual"*, and
`11-risks-and-technical-debt.md` §11.1 is this repository's canonical home for
exactly that — part 1 already edited this file. Adding it here is the one piece
of scope in this task the brief does not name explicitly; drop the step if a
reviewer disagrees, but do not leave the residual recorded nowhere.

**Red:**

```bash
grep -n -F "responses_live_timings" docs/architecture/11-risks-and-technical-debt.md
```

Expected: **nothing**. (Inverted red/green again.)

**Edit.** §11.1 is **not one table.** It is a headed table, then a **blank
line**, then a two-row header-less fragment, then `## 11.2`. A row appended
"after the last existing row and before the `## 11.2` heading" lands inside that
fragment, where GitHub renders it as literal text full of pipes rather than as a
table row — measured on `03e2f9c`, where the blank line sits between the
`Windows per-process VRAM` row and the `If the gateway dies between the VRAM
benchmark's drain and its restore` row.

Append the row to the **headed** table instead: immediately after the row
beginning `| **Windows per-process VRAM rests on a display-driver DDI`, and
**before the blank line** that follows it. One physical line:

```
| **A llama.cpp build that rejects unknown top-level keys on `/v1/responses` turns the live-timings opt-in into a 4xx for every request on that application** | With `responses_live_timings_enabled` on, the gateway adds `timings_per_token` to a streaming Responses passthrough body and does **not** retry without it, so a rejecting upstream fails the client's request rather than merely omitting a display figure | Accepted and bounded. The build this was measured against answered an entirely fabricated top-level key with an ordinary completion, and llama.cpp's request schema is pull-based, so a key nobody asks for is never inspected. The blast radius is one application, the operator can switch the flag off through either portal API surface, and the failure is immediate and visible rather than silent. `proxyNative`'s per-request debug line records that the key was injected, which is what makes it diagnosable — the payload capture shows the client's bytes, not the body that was sent. A retry is a self-contained follow-up if a real rejection is ever observed: the status is known before the first client byte and the untouched client bytes are still in hand. |
```

**Green:**

```bash
grep -n -F "responses_live_timings" docs/architecture/11-risks-and-technical-debt.md
awk '/^\| \*\*A llama.cpp build that rejects/ {n=gsub(/\|/,"|"); printf "pipes=%d\n", n}' docs/architecture/11-risks-and-technical-debt.md
./scripts/check-docs.sh
```

Expected: one line; `pipes=4`; `check-docs: OK`.

**Commit:**

```bash
git add docs/architecture/11-risks-and-technical-debt.md
git commit -F - <<'MSG'
docs: record the live-timings injection's accepted residual on the risk register

The design accepts one residual by name: an older llama.cpp build that rejects
unknown top-level keys on /v1/responses would answer 400 to a flagged request,
and this cut ships no retry, so that 400 reaches the client. Everything else
about the residual is recorded in the telemetry chapter's rule; the risk
register is where this repository keeps accepted operational exposures, and it
was the only canonical place this one was not written down.

The row carries the three facts that make it acceptable rather than merely
tolerated: the measured build accepts a fabricated top-level key because
llama.cpp's request schema is pull-based; the blast radius is one application
and the operator can switch it off from either portal surface; and the failure
is immediate and visible rather than a silently missing number. It also names
what makes it diagnosable, since the obvious first move fails -- the payload
capture shows the client's bytes, not the body that was sent, so the per-request
debug line is the grep that explains the 400.
MSG
```

---

- [ ] **Step 17: the final sweep and both gates**

No edit. This is the step that catches a member you skipped by accident.

```bash
cd /Users/jlor08/Developer/codex/op-ai-gateway/.worktrees/responses-timings-injection

# Re-run the ENTIRE step-1 sweep. Every Set 1/2/3/4 and Table E grep must now
# print NOTHING, except the two that are supposed to survive.
# (paste the step-1 script here and read its output)

echo "=== these two MUST still print ==="
grep -n -F "The in-flight figure is display only" docs/architecture/cross-cutting/telemetry-usage-observability.md
# NOT -F "nothing retries without it": that literal matches NOTHING on 03e2f9c,
# because the clause wraps across a line break in all three documents. A literal
# grep here reads as a clean sweep while proving nothing.
grep -rn "retr[a-z]* without it" docs/architecture/

echo "=== gates ==="
./scripts/check-docs.sh; echo "check-docs exit=$?"
bash ./scripts/check-docs.test.sh 2>&1 | tail -3

echo "=== this task changed comments only ==="
git diff 03e2f9c --stat -- gateway/backend/internal/gateway/passthrough_usage_scan.go \
  gateway/backend/internal/gateway/capture.go \
  gateway/backend/internal/gateway/passthrough_progress_test.go
git diff 03e2f9c -- gateway/backend/internal/gateway/capture.go \
  | grep -E '^[-+]' | grep -v '^[-+][[:space:]]*//' | grep -v '^[-+][-+][-+]'
git diff 03e2f9c --stat -- gateway/backend/internal/gateway/native_passthrough.go
```

Expected:
- `check-docs: OK`, `check-docs exit=0`.
- `all check-docs cases passed`.
- The `capture.go` filter prints **nothing**: this task is the only author of
  that file on this branch, and it changed comments only. (`passthrough_usage_scan.go`
  and `passthrough_progress_test.go` will also show Tasks 4's and 5's real code
  changes against `03e2f9c`, which is expected — use `git diff HEAD~N` scoped to
  your own commits if you want to isolate yours.)
- `native_passthrough.go` shows Tasks 3's and 4's changes and **none of yours**.
  This task never edits that file.
- The retry-clause regex prints **four** lines in **three** files, and the count
  matters: `reference/api-surface.md` once (step 12a), `reference/data-model.md`
  twice — the `applications` row's *"nothing retries without it"* (13a) and the
  migration row's *"nothing retrying without it"* (13b), which is why the pattern
  is `retr[a-z]*` and not the literal — and
  `cross-cutting/agent-runtime-manager.md` once (step 14). D3 keeps that clause
  TRUE; it must not have been deleted anywhere, and it must not have been
  re-wrapped across a line break, which would hide it from this sweep.

**Do NOT run `go build`, `go test`, `gofmt` or `golangci-lint` as part of this
task.** Nothing here changes a token of Go code, so a Go failure at this point
belongs to another task and reporting it as this one's would mislead. The
repository's own gates for a comment-and-docs change are the two above.

If anything in the sweep still prints, fix it in its own step with its own
commit rather than folding it into an existing one.

---

### What a reviewer should check that no gate can

1. **All nine Set 1 sites this task owns are edited, not seven of them.** Table A
   has **twelve** rows; rows 1, 2 and 4 are Task 4's. The sweep script in step 1
   is the enumeration: its *mine* blocks must come back empty at the end, and its
   **NOT mine** block must have been empty from the start.
2. **Every retry clause was edited, never deleted — and none was re-wrapped out
   of the sweep's reach.** D3 keeps it true. Three documents carry it, four lines
   in all, and `grep -rn "retr[a-z]* without it" docs/architecture/` is the
   check. The literal `nothing retries without it` is **not** the check: it
   matches nothing even on `03e2f9c`, because the clause wraps across a line
   break in all three documents, so a reviewer using it would read an empty
   result as a clean sweep.
3. **No comment written in this task contains a line number.** `grep -nE
   '\.go:[0-9]+|line [0-9]+' ` over the diff should find none.
4. **The log field named in step 8 is the field Task 4 actually shipped.** A
   document promising a grep that returns nothing is worse than one promising
   nothing.
5. **Nothing another task owns was touched.** Table A rows 1, 2 and 4 (Task 4),
   Table C rows 6 and 7 (Tasks 5 and 6), Table D members 1–3 (Task 2), and Table
   E row 2 (Task 1). A duplicate edit is a merge conflict at best. Table E row 3
   is the single deliberate exception and is not a duplicate: step 14 replaces a
   run that *contains* Task 1's narrowed clause and carries its parenthetical
   forward verbatim, which `grep -n -F "measured inert on"` confirms.

---

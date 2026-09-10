# Delete the flat MTP bonus, and observe speculation instead

**Status:** approved (operator, 2026-09-10)

## 1. Why

**The bonus duplicates a measurement, and does it worse.** `mtpBonus = 30.0` is added
inside `metricTiebreak` (`internal/routing/scorer.go`), the same function whose other two
terms are the *measured* generation and prompt throughput. Its own justification is a
throughput claim — the comment warns that "a false positive would later bias server
selection toward a model that is **not actually faster**" — so it is a guess about speed
sitting next to a measurement of speed. If a route is faster because the server
speculates, the measured tokens-per-second term already says so, from the actual effect
rather than from a substring of a model name.

**The repo means two different things by "MTP", in the same comment block.** Every
*definition* is about the model's architecture: `routing/mtp.go` says
"deepseek-v3 … ship an MTP head", "glm-4.5 / 4.6 expose MTP" — statements about weights.
Every *justification* is about deployment speed, and the *placement* agrees with the
justification: the bonus is a peer of measured metrics. The two readings come apart in
practice — a GLM-4.6 GGUF with MTP heads served without `--spec-type` is not speculating
at all — and the repo has no vocabulary for the second reading: the words "speculative
decoding", "draft model" and "speculation" appear nowhere in it.

**And the property that actually matters is observable.** llama.cpp reports drafted-token
counters on the traffic this gateway already relays, so "this endpoint is speculating" can
be *observed* instead of guessed — as information, not as a thumb on the scale.

## 2. Decisions

1. **Delete the flat bonus.** Operator's call: tokens per second is the more important
   signal, and it is already in the same tiebreak.
2. **Add a purely informational capability** recording that speculation was observed on
   this mapping. Display only — no scorer, no routing filter, no candidate join.
3. **`mtp` stays what its definitions say it is**: an operator- and heuristic-owned claim
   about the model's architecture, now display-only, with the ambiguity written down for
   the first time.
4. **`/slots` is not used.** It cannot answer either reading: its `speculative` bool is
   true for *every* technique (`draft-simple`, `ngram-*`, `draft-eagle3`, `draft-mtp`, …),
   so it is not an MTP signal; the only field that names `draft-mtp` lives under
   `params`, which exists only after a request has run and is therefore useless against a
   server idle since boot. It also needs the child's API key (only `/health` is public),
   answers **501** when the endpoint is disabled, and its upstream README is stale.

## 3. What gets deleted

The chain is short and it is entirely one-directional — `scoringRoute`'s `IsMTP: c.IsMTP`
is **the only reader of `MappingCandidate.IsMTP` in the repo**:

- `mtpBonus` and the three lines that apply it in `metricTiebreak`; `metricTiebreak`'s
  documented bound drops from `50 + 20 + 30` to `50 + 20`.
- `Route.IsMTP` and `scoringRoute`'s fill of it.
- `MappingCandidate.IsMTP`, `MTPFromVerdict` (its only two callers are the two store
  fills), the **filtered `mtp` LEFT JOIN** in `ActiveMappingsForModel` with its bind
  argument, selected column and scan target, and the `MemoryStore` mirror.
- **This makes the request path cheaper**, not more expensive: the candidate query loses
  one of its two filtered joins, worth about 6 µs against a ~17 µs base by the measurement
  recorded in that comment. Say so in the ADR; a deletion that speeds up the hot path is
  the opposite of the usual trade and worth recording.

Tests, each judged rather than swept: `mtp_capability_join_test.go` exists only to pin the
bonus — delete the file. `TestSelectRoutePrefersMTP` likewise, and note it would *fail*
rather than pass vacuously, because the two routes then tie and `Select`'s strict `>`
returns the plain one. `TestScoreTiebreakBounded` re-baselines to 70 and keeps proving the
cap. `TestScoreTiebreakDoesNotRescueNonViable` and
`TestScoreTiebreakBeatsSmallPriorityDelta` use the flag incidentally: drop the field,
re-baseline the comment, keep the assertion. The two portal tests
(`…MTPHeuristicEarnsTheScorerBonus`) are **half bonus pin and half something else** — their
second half proves a `legacy` row is replaceable by a rank-1 probe, which must survive:
rewrite them to assert the ROW rather than a score delta, and rename them accordingly. The
cross-driver conformance test keeps its `live_progress` half and loses its `mtp` half.

## 4. What `mtp` becomes

Display-only, and still written by the name heuristic at both mapping-creation sites — it
is a useful seed an operator can now correct, and its `legacy` source names its provenance
in the tooltip. But **every justification for writing it evaporates** and must be
rewritten, not left: both call sites justify themselves by the bonus ("would silently lose
the bonus migration 78 gave every mapping"), `IsMTPModelName`'s comment warns about
biasing server selection, and `legacyMTPCapabilityRow`'s doc contains a forward reference
to "PR C's `/slots`-based probe" that this spec proves impossible. Fix all four.

## 5. The new capability

**Name:** `speculation_observed`. Not `mtp` (a different proposition), and not
`speculative` (which would read as a capability of the model rather than an observation of
a deployment). **Source:** `llama_cpp_timings`, named for the document it read, exactly as
`llama_cpp_props` and `ollama_api_show` are. It ranks 1 through `capabilitySourceRank`'s
documented `default` branch — **add no case to the rank table**, which is the
`ollama_api_show` precedent: a named constant so provenance is a fact rather than a
plausible lie, ranked so it can never talk over `manual` or a benchmark but can always
repair its own drift.

**Positive-only, structurally.** llama.cpp emits the counters under
`if (n_draft_tokens > 0)`, so when speculation is off the keys are **absent, not zero** —
there is no `"draft_n": 0` state on the wire. A `no` verdict is therefore never available,
and absence of the key never proves the server cannot speculate: a stream without the
usage chunk, a cache hit, an error, a short completion, or any non-llama.cpp upstream all
produce none. The field names are `draft_n` and `draft_n_accepted`, verified stable across
llama.cpp's server refactor (checked at master and at tag `b6000`).

**Where it can be read, and where it cannot.** Present on the non-streaming
OpenAI-compatible and chat shapes, and on the **final frame** of a chat stream — where
llama.cpp puts `timings` on `deltas.back()`, which is the same chunk as `usage`. Absent
entirely from the non-streaming `/v1/responses` body, every Anthropic shape, and ASR, so
those paths contribute nothing and need no code. Only `draft_n` is carried;
`draft_n_accepted` is not, because an acceptance *rate* is a performance measure that
belongs with metrics, not a capability verdict.

**The write happens at most once per mapping per gateway lifetime.** The rank rule already
drops a write whose verdict and rank match, but comparing needs a read, so the naive shape
would query the database on every completion. Instead: an in-memory set keyed by
`Target.RouteID` (the serving mapping id, already on the target for usage attribution),
consulted before anything else, so a mapping already recorded costs nothing and a
non-speculating mapping is never touched at all. Follow the existing opportunistic-metrics
shape in the same function — including its failure posture, a log line rather than a
failed request, because the verdict is information and the completion has already been
delivered.

## 6. Out of scope

- `/slots`, for the four reasons in §2.4.
- vLLM, Ollama and TGI. vLLM's `vllm:spec_decode_*` metrics would be a separate source
  with its own scrape path; Ollama and TGI expose nothing.
- A `no` verdict for speculation, in any form (§5).
- `draft_n_accepted` and any acceptance-rate figure.
- Re-defining `mtp` (decision 3) and removing the name heuristic.

## 7. Verification

- Per touched Go module: `golangci-lint fmt --diff` (must print nothing) + `run` +
  `go test ./... -count=1`. `go test ./internal/gateway/ -race` has a pre-existing failure
  on `TestPushRuntimeConfigNeverPushesTheEmptyDocumentOnAStoreFailure` (#53) — use plain
  `go test` there.
- The **PostgreSQL leg** is mandatory: this removes a column from the candidate query's
  SQL in the shared implementation both dialects use. It skips **silently** without
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN`, so a green SQLite-only run proves nothing. Count with
  the indented grep `^ +--- (PASS|FAIL|SKIP): .*/postgres`; baseline 125 PASS + 1
  documented sqlite-only SKIP.
- Frontend gates if the portal changes: `format:check` + `lint` + `build` + `test`.
- Docs: `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- The two `detectCapabilities` / `detectLiveProgressSupport` twins stay byte-identical
  across the modules, checked **with a non-vacuity guard and a positive control**: assert
  both extracts are non-empty, then verify that a one-character change to one makes the
  same comparison disagree.
- A routing test proving two otherwise-identical candidates no longer differ on the `mtp`
  verdict, and that a measurably faster route still wins.

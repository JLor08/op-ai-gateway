# Ollama capabilities, and the context probe that could never succeed

**Status:** approved (operator, 2026-09-10)

## 1. Why

Three things, one code path.

**(a) The context probe for an `ollama`-typed spec can never succeed (#54).** Ollama's
`/api/show` is a **POST**; the agent probes it with a **GET**
(`server-agent/internal/collector/probe.go`, `fetchProbeBody` hardcodes
`http.MethodGet`). Since Ollama v0.7.0 the server sets `HandleMethodNotAllowed`, so the
GET returns **405 text/plain**; before that it returned a 404. Either way it is never
JSON, so the probe reports `context_probe = "unreachable"` permanently — indistinguishable
in the portal from a genuinely misconfigured endpoint.

**(b) The reason it shipped is a test that cannot fail.** `newProbeServer`
(`probe_test.go`) declares its handler as `func(w, _ *http.Request)`. Method, path and
body are never asserted, so `TestProbeContext_Ollama` is green while production 405s on
every cycle. No fix to (a) prevents recurrence unless that `_` goes.

**(c) Ollama's capabilities are never read.** `POST /api/show` returns a `capabilities`
array; nothing in either module asks for it. `collector.Capabilities.Extra` — the field
added for exactly this — has no producer anywhere.

## 2. What Ollama actually reports (verified in upstream Go source, 2026-09-09/10)

- `POST /api/show`, body `{"model": "<name>"}`. `ShowRequest.Model` is the field
  (`name` is a deprecated alias); an empty body is 400 `missing request body`.
- `ShowResponse.Capabilities []model.Capability` with `json:"capabilities,omitempty"`.
  The eight declared constants are `completion`, `tools`, `insert`, `vision`,
  `embedding`, `thinking`, `image`, `audio` — and the set is **not closed**:
  `configCapabilities` casts every string of `ConfigV2.Capabilities` ("used for remotes")
  straight to a capability, so cloud models return whatever the publisher wrote.
- **Three names collide with ours by spelling: `vision`, `tools`, `audio`.**
- **`image` means image GENERATION, not vision.** It was born as
  `CapabilityImageGeneration`, its error string is `"image generation"`, and the only code
  that ever required it backed `/v1/images/generations`. Upstream's own test calls
  `["image","vision"]` "image editing". Never map it onto `vision`.

**The list is NOT exhaustive: absence means unknown, not "no".** Five independent pieces
of upstream source say so:
1. `Capabilities()` logs `slog.Warn("unknown capabilities for model")` for an empty result
   — upstream's own word for it is *unknown*.
2. The field is `omitempty`, so an undeterminable model emits no key at all — byte-identical
   to a pre-v0.7.0 server that has no such field.
3. `ggufCapabilities` returns early with none when the model file cannot be opened
   (`slog.Error("couldn't open model file")`) — a transient read failure silently shortens
   the list and nothing marks the response as degraded.
4. Detection is substring heuristics over the chat template: a tool-capable model whose
   template lacks the literal `tools`/`tool_call` reports no tools. Non-GGUF and remote
   models get capabilities only from the manifest array.
5. `filterUnsupportedCapabilities` deliberately strips real vision/audio for some builds —
   the omission then describes the runner, not the model.

**Therefore: only `yes` rows.** Deriving a `no` from a missing name would encode read
failures, template heuristics and runner quirks as operator-visible denials.

## 3. Decisions

1. **Source: `POST /api/show`.** Authoritative and available since v0.7.0.
   Rejected: `GET /api/tags`, which would cover a whole server in one request but needs
   v0.30.0 and under-reports `tools`/`thinking` for models whose template lives only in
   the GGUF.
2. **Only `yes` verdicts** (§2).
3. **Foreign names are stored verbatim, except `completion`.** Not because it is noise but
   because it carries no evidence: upstream *assumes* it whenever a model has no
   `pooling_type`. Everything else — `insert`, `thinking`, `embedding`, `image`, and any
   unknown publisher string — is a real assertion and is kept.
4. **Agent-side only.** The supervised path needs no change to the agent router's
   deliberate GET-only `/upstream/{model}/props` allowlist (#62).
   *Deferred with a reason:* a **directly configured** Ollama application (no agent) still
   gets no capability detection. Closing that needs a POST-capable `fetchModelInfo` on the
   gateway **and** a decision about fan-out — a direct Ollama endpoint serves many models,
   so it is one `/api/show` per model per probe cycle. That is its own change, not a
   footnote to this one.
5. **The context probe keeps its meaning.** `/api/show`'s `model_info.<arch>.context_length`
   is the model maximum and stays the reported number. `/api/ps`' `context_length` is the
   loaded runner's effective `num_ctx` (and 0 when nothing is loaded) — a different
   quantity, so switching to it is an operator-visible change and is **out of scope**.

## 4. The detector

`detectOllamaCapabilities(body []byte) Capabilities` in
`server-agent/internal/collector/probe.go`, beside `detectCapabilities`:

- reads exactly one thing, `ShowResponse.capabilities`;
- maps `vision`, `tools` and `audio` onto the **structured** fields, as `"yes"`;
- appends every other name to `Extra`, except `completion`;
- emits nothing for a name it did not see (no `"no"`, ever);
- an absent or empty array yields the zero `Capabilities{}` — which the wire already
  distinguishes from "detection never ran" via the `*Capabilities` pointer.

It is a **sibling** of `detectCapabilities`, not a branch inside it: `/props` and
`/api/show` share no field, and `ProbePropsVerdicts`' own doc already records that keeping
a differently-shaped probe as its own function is the deliberate choice.

**Twinning:** `detectCapabilities` and `detectLiveProgressSupport` exist byte-identically
in both Go modules because both modules probe `/props`. Only the agent probes `/api/show`,
so `detectOllamaCapabilities` lands in the agent module **alone** — shipping an
uncalled copy in the gateway would be dead code. Its doc comment must say that the day the
gateway gains its own Ollama probe (decision 4's deferred half), this function becomes a
twin and the drift discipline applies.

## 5. The probe plumbing

**Capabilities.** A sibling of `ProbePropsVerdicts`:
`ProbeOllamaVerdicts(ctx, client, baseURL, model) (PropsVerdicts, bool)`. It POSTs
`{"model": model}` to `/api/show`, runs `detectOllamaCapabilities`, and returns
`LiveProgress: ""` — Ollama has no `timings_per_token` surface, and an unknown must never
become a denial. It reuses the existing conclusive-status set (404/401/403/405 are cacheable
answers; 0 and 5xx are retried), because those are properties of the binary's routing table
and its credential, fixed at exec time.

`probeRuntimeChildProps` (`server-agent/internal/agent/agent.go`) gains **one** branch on
`st.Type == "ollama"`, choosing which sibling to call. Everything else is unchanged: the
cache stays keyed `(SpecID, PID)` — one probe per child PID generation, so this costs one
extra request per child lifetime, not per collect cycle — and both siblings return the same
`PropsVerdicts`, so the cache, `capabilitiesSample` and the wire need no change at all.

The existing comment on that function says `/props` is probed "regardless of `st.Type`"
because `custom` is the type-detection fallback and gets no derived path. That reasoning
survives: the branch is for `ollama` **only**, and every other type — `custom` included —
keeps trying `/props`. Update the comment so it says that.

**Context (#54).** `fetchProbeBody` gains a method and an optional body (or a small
`probeRequest{method, path, body}` sibling; the `/props` callers keep passing GET/nil).
`ProbeContext` gains a `model` parameter, which `probeRuntimeChildContext` already holds as
`st.Model`. The `ollama` branch POSTs `{"model": st.Model}`; every other type keeps GET.
`DeriveProbePaths`' `ollama` context path stays `/api/show` and `extractOllamaContext` stays
byte-for-byte — only the verb was ever wrong.

`runtimeCtxEntry` gains a `model` field and the cache-hit condition compares it, for the
same reason the existing comment gives about config-only edits: a spec whose model changed
must not serve a context size measured for the previous one.

## 6. Where the verdicts land

Unchanged path: `capabilitiesSample` → the telemetry wire → `runtimeSampleCapabilityRows`
→ `WritableCapabilityRows` → `UpsertMappingCapabilities`. No new writer, no schema change,
no store change.

Two existing invariants now become load-bearing for real, where before they were
precautionary:

- **One row per capability name, first occurrence wins.** Ollama's names collide with the
  structured fields by spelling, and `capabilitiesSample` emits the four structured fields
  before `Extra`. That ordering is already documented as load-bearing; this change is the
  first one that can actually exercise it.
- **The rank rule.** A new source constant `CapabilitySourceOllamaAPIShow = "ollama_api_show"`
  ranks 1 through `capabilitySourceRank`'s default branch — no rank-table edit — so it can
  never overwrite `manual` (3) or `vision_benchmark` (2), and it can repair its own drift
  (1 vs 1). Name it as a constant anyway, so every place that maps a source to a display
  string knows it.

## 7. Test integrity

`newProbeServer` must stop discarding `*http.Request`. Add a table test over
`{vllm, llama_cpp, tgi, ollama, custom}` asserting the **method and path** each type
actually issues, and for `ollama` the **request body**. This is the assertion whose absence
let #54 ship, and it is the only part of this spec that prevents the class rather than the
instance.

Every new or changed test must fail with its production change reverted — production files
only. Reverting a test makes `go test -run X` print "ok" with zero tests, a fake pass.

## 8. Out of scope

- `/api/ps` for the context size (decision 5).
- Gateway-side detection for directly configured Ollama applications (decision 4).
- Any widening of the agent router's GET-only upstream allowlist.
- `GET /api/tags` as a bulk source.

## 9. Verification

- Per touched Go module: `golangci-lint fmt --diff` (must print nothing) + `run` +
  `go test ./... -count=1`.
- Docs: `./scripts/check-docs.sh` + `bash ./scripts/check-docs.test.sh`.
- The PostgreSQL leg only if anything under `internal/store` or `internal/routing` changes
  — it skips **silently** without `OP_AI_GATEWAY_TEST_POSTGRES_DSN`, so a green SQLite-only
  run would prove nothing. Count subtests with the indented grep
  `^ +--- (PASS|FAIL|SKIP): .*/postgres`; the baseline is 125 PASS + 1 documented
  sqlite-only SKIP.
- The twin drift check for the two existing pairs, **with a non-vacuity guard**: assert both
  `sed` extracts are non-empty before believing `diff`, or two wrong paths produce a
  confident "identical" that checked nothing.
- An `ollama`-typed spec with a loaded model reports `context_probe = "ok"` and a plausible
  context size, and its mapping shows the capabilities Ollama declared, sourced
  `ollama_api_show`.

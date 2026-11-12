# Decisions Phase 2 — `/v1/decisions` (OpenAI Decisions API), translate and passthrough (#373)

Branch-local design. Authority: issue #373 ("Phase 2" acceptance criteria, translation matrix A, error table, owner decisions 1–10), ADR-097 (Phase 1 as shipped). Wire facts: `2026-11-12-decisions-wire-schemas.md` (openai-python v3.49.0 models, vLLM v0.33.0 sources). Plumbing pointers: `2026-11-12-decisions-phase2-inventory.md`.

## 1. Scope

In:
- `POST /v1/decisions` and `POST /openai/v1/decisions` (OpenAI Decisions API), bearer-only.
- Endpoint modes `decisions_mode` (`auto` | `translate` | `passthrough` | `disabled`) and `systemone_mode` (`passthrough` | `disabled`) on applications and runtime specs (the column ADR-097 deferred).
- Capability verdict `decision_openai` and its probe.
- Translator OpenAI Decisions → System One (matrix A), pure functions in `internal/compat/decisions.go`.
- Per-token opt-in `decisions_confidence_normalize`, both endpoints, every mode.
- Usage: translated usage on translate; cached-token details on passthrough.
- Portal: both mode selects, the `decision_openai` verdict and chip, the effective decisions route per mapping, the token checkbox.
- Docs: compat §3.9/§3.10/§6/§13, api-surface, data-model, routing, openapi.yaml, ADR-098, glossary.

Out (later phases): vllm-sr types (Phase 3); System One → OpenAI (matrix B) and vendor accounts (Phase 4); a decisions listing beyond `/v1/systemone/models`; portal playground.

## 2. Wire facts the design rests on

- OpenAI request (`decision_create_params.py`): `model`, `input` (string | items), `questions` (1–400), `prompt_cache_key`, `prompt_cache_options`, `safety_identifier`. No `stream`, no top-level `instructions`. Question: `{type: predicate|choice|score, instructions: string | parts[], name?}`; choice `choices[{value: string|bool, description?}]`; score `levels[{label, description?}]`. Parts: `input_text{text}`, `input_image{image_url, detail?}` (or `file_id`), `input_json{data}`.
- OpenAI response (`decision.py`): `{answers[], model, usage}` — no id/object/created. Answers in question order, each with `name` (null when unnamed): predicate `{probability}`, choice `{choice, confidence, probabilities[{value, probability}]}`, score `{score, confidence, probabilities[{value:int, label, probability}]}`, refusal `{type:"refusal", name}`. Usage `{input_tokens, input_tokens_details{cached_tokens, cache_write_tokens}, output_tokens, output_tokens_details{reasoning_tokens}, total_tokens}` (all required by the SDK).
- System One (vLLM `/v1/systemone`, TypeSafe): request `{model, state, questions{id:{type, instructions, criteria}}, images?}`, unknown fields → 400 on vLLM. Types `noul` (criteria null or `{true,false}`), `choice` (criteria `{name: description|null}`), `score` (criteria ordered list of level names). Response `{answers{id:…}, usage{input_tokens, output_tokens}}` (+ vLLM `id/object/created/diagnostics/joint`). Answers: noul `{noul: P(yes), probabilities{yes,no}?, confidence?}`; choice `{choice, probabilities{name:p}, confidence}`; score `{score: expected zero-based index (float), legend{"0":name…}, probabilities{"0":p…}, confidence}`. All three sources agree `score` is an expected zero-based index.
- vLLM `/v1/decisions`: generate-task models only; d3 (classify) → 501. `name: null` → 400, `prompt_cache_*` → 400, text only. Both vLLM endpoints answer 501 for a model without a read strategy. The request-level `instructions` field (not the per-question one) is what fixed-prompt models such as d3 refuse with 400.

## 3. Routing

### 3.1 Flavors

- The coarse flavor stays `decisions` (`routing.APIFlavorDecisions`). A new request flavor `openai_decisions` (`routing.APIFlavorOpenAIDecisions`) normalizes to `decisions`, as `openai_responses` normalizes to `openai`. It is request-side only: never stored, never accepted by the portal flavor whitelist. `/v1/systemone` keeps using the coarse `decisions` as its request flavor (Phase 1 usage rows stay comparable).
- `FlavorsAreDecisionsOnly`, the warmer/benchmark/chat exclusions and `/v1/systemone/models` are unchanged.

### 3.2 Modes

- `routing.EndpointModeAuto EndpointMode = "auto"` is added. It is valid only for `decisions_mode`.
- `routing.Application` and `routing.RuntimeSpec` gain `DecisionsMode` and `SystemOneMode` (`EndpointMode`). `EffectiveSpecFields` carries both (a server_agent spec row wins, as for the other modes); `Target` carries both (copied in `targetFrom`). Vendor targets: never decision candidates (the capability list keeps them off), so no forcing is added.
- Defaults: `decisions_mode = auto`, `systemone_mode = passthrough`, on create (absent or "") and for every existing row (migration backfill).
- Allowed values: `decisions_mode ∈ {auto, translate, passthrough, disabled}`; `systemone_mode ∈ {passthrough, disabled}` (System One has no translate path until Phase 4). Invalid → 400 `application.endpoint_mode_invalid` / `runtime_spec.endpoint_mode_invalid` (existing codes).
- Type guard: `decisions_mode = passthrough` is refused for application type `llama_cpp` and runtime spec type `llama_cpp` (llama-server has no `/v1/decisions` route): 400 `application.decisions_passthrough_unsupported` / `runtime_spec.decisions_passthrough_unsupported`; on an application update that keeps the stored `llama_cpp` type, 409 `application.decisions_passthrough_conflict` (the live-timings pattern). A type change to `llama_cpp` with a stored `passthrough` resets it to `auto` (the live-timings "clear when the type cannot carry it" pattern). Phase 3 extends the guard to `vllm_sr`.
- Candidacy (`applicationServesEndpoint`): for an ordinary application, request flavor `decisions` (System One) is served iff the flavor is listed and `SystemOneMode != disabled`; `openai_decisions` iff listed and `DecisionsMode != disabled`. A server_agent application is not removed here; the spec is enforced at dispatch (as for the other modes).

### 3.3 Capability `decision_openai`

- `routing.CapabilityDecisionOpenAI = "decision_openai"`: the model answers OpenAI Decisions (`/v1/decisions`) natively. Source `routing.CapabilitySourceDecisionsProbe = "decisions_probe"`, rank 1 (same as `systemone_probe`). `(decision_openai, no)` is a reserved manual verdict; `decision_openai` is a reserved agent capability name; the portal fold exposes it as `ModelDTO.DecisionOpenAI` (`json:"decision_openai"`, always present) and the verdict select accepts it like `decision`.
- Derived requirement `routing.CapabilityAnyDecision = "any_decision"`: never stored, never accepted as a row name (manual or agent); `capabilityRowsSatisfy` treats it as satisfied when the rows carry `decision: yes` OR `decision_openai: yes`. It is the pre-resolve gate of `/v1/decisions` (so a mapping with neither verdict is `404 routing.model_not_capable`, and the list keeps the request off vendor accounts).

### 3.4 Effective decisions route (post-resolve)

After resolve, the handler reads the resolved mapping's verdicts (one `MappingCapabilities` read by `target.MappingID`) and computes the concrete route from the target's effective `DecisionsMode`:

| effective mode | route | required verdict | without it |
|---|---|---|---|
| `disabled` | — | — | 404 `decisions.endpoint_disabled` |
| `auto` | `passthrough` if `decision_openai: yes`, else `translate` | `decision` for translate | 404 `routing.model_not_capable` (not reachable after the gate, except a race) |
| `translate` | `translate` | `decision: yes` | 404 `routing.model_not_capable` |
| `passthrough` | `passthrough` | `decision_openai: yes` | 404 `routing.model_not_capable` |

The handler stores the concrete route on its copy of the target (`DecisionsMode = translate|passthrough`), so `upstreamPath(target, openai_decisions, endpointDecisions)` answers `/v1/systemone` for translate and `/v1/decisions` for passthrough, and the usage row's `ProviderPath` records the route taken (no new usage column: `req_path` + `provider_path` identify the mode).

For `/v1/systemone`, the effective `SystemOneMode = disabled` on the resolved target answers 404 `systemone.endpoint_disabled` (a server_agent spec can disable it per model; an ordinary app is already filtered at candidacy).

## 4. `POST /v1/decisions`

Handler `handleOpenAIDecisions` (new file `internal/gateway/decisions_handler.go`), routes `/v1/decisions` and `/openai/v1/decisions`. Template: `handleSystemOne`.

1. `requireMethod POST`; `requireAnyScope(gateway:use, llm:invoke)` (bearer only); `liftInferenceDeadlines`; `readRawJSONLimit(…, decisionsBodyLimit())` (shared cap, 413 `request.body_too_large`); `sniffRoutingModel`.
2. Shallow validator `validateDecisionsRequest(raw) *inference.Error`: `model` non-blank string → else `decisions.model_required`; `questions` non-empty JSON array → else `decisions.questions_required`; `stream` absent, `null` or `false` → else `decisions.stream_unsupported`. All 400.
3. `runAsPrincipal`; `inferencePreflight` with shape `{apiFlavor: openai_decisions, endpoint: endpointDecisions, model, requiredCapabilities: [any_decision]}`.
4. Resolve (`resolveTarget`), `targetServesFlavor(target, openai_decisions)` (spec flavors), then §3.4. Refusals answered and recorded like `recordSystemOneRoutingFailure`.
5. Passthrough: `proxyNative` to `/v1/decisions`, body relayed value-for-value (model rewritten). Without normalization the response streams through the copier verbatim; usage is read by `mergePassthroughUsage` (`openai_decisions` → the Responses-shaped usage reader, extended to `cache_write_tokens`), so `cached_tokens` and `cache_write_tokens` reach the usage row. Non-2xx relayed verbatim.
6. Translate: `compat.TranslateDecisionsRequest(raw, providerModel)` → System One body + answer table (§5). A translator refusal is a 400 with its code, recorded as a failed request (no upstream call). The System One body goes to `/v1/systemone` via `proxyNative` with `upstreamBody` set; the response takes the buffered decisions path (§7).
7. `relayAgain` reselect as on System One.

## 5. Translator (`internal/compat/decisions.go`)

Pure functions, no I/O; one table-driven test per matrix-A row.

- `TranslateDecisionsRequest(raw []byte, upstreamModel string) (body []byte, table DecisionsAnswerTable, err *DecisionsError)`.
- `TranslateSystemOneResponse(body []byte, table DecisionsAnswerTable) (out []byte, usage DecisionsUsage, err error)`.
- `NormalizeDecisionsConfidence(body []byte, dialect DecisionsDialect) ([]byte, bool)` (§6).

Request mapping (matrix A; 🟡 = emulated, documented; ❌ = 400 `decisions.input_unsupported` unless stated):

| OpenAI | System One |
|---|---|
| `model` | `model` = upstreamModel |
| `input` string | `state` string |
| `input` items: only `type:"message"` (or no `type`) with `role:"user"`; content string or parts | `state`: text parts and string contents joined with `"\n\n"` in order (🟡 when more than one); `input_json` parts kept as JSON values: a lone `input_json` → `state` = that value; JSON mixed with text → `state` = array of the pieces in order (text pieces as strings) 🟡 |
| roles `developer`/`system`/`assistant`, `function_call*`/`custom_tool_call*`/any other item type | ❌ |
| `input_image` with `image_url` starting `data:` | `images[]` (in order), `detail` dropped |
| `input_image` with any other URL scheme or with `file_id` | ❌ (no gateway-side fetch) |
| `prompt_cache_key`, `prompt_cache_options`, `prompt_cache_breakpoint`, `safety_identifier` | dropped 🟡 |
| any other unknown top-level field | dropped (OpenAI has none; tolerated, not relayed) |
| `questions[i]` order | `questions` JSON object written in request order (hand-built JSON, never a Go map) |
| `questions[i].name` | the key; missing/null/empty → `q<i>` (i zero-based); a duplicate key or a name colliding with a synthetic key gets `-<n>` (n from 2) until unique |
| `predicate` | `noul`, `criteria` omitted |
| `choice.choices[{value, description}]` | `criteria {key: description|null}` — key = string value, or `"true"`/`"false"` for a bool; empty or absent description → `null`; two choices mapping to the same key (`"true"` and `true`) → 400 `decisions.question_invalid` |
| `score.levels[{label, description}]` | `criteria [label…]`; a level with a non-empty description becomes `"label: description"` 🟡 |
| `instructions` string | `instructions` string |
| `instructions` parts: only `input_text` | joined with `"\n\n"` |
| `instructions` parts with `input_image`/`input_json` | ❌ |
| unknown question `type`, choice without ≥1 choice, score without ≥1 level, a choice/level that is not an object | 400 `decisions.question_invalid` |
| limits (400 questions, 255 choices, 10 levels) | not pre-validated; the upstream's 400 is normalized and relayed |

`DecisionsAnswerTable` records per question index: the System One key, the original `name` (string or null), the type, the original choice values in order (typed: string or bool) with their keys, and the level labels in order.

Response mapping:

| System One | OpenAI |
|---|---|
| `answers{}` | `answers[]` in request order, `name` restored (null if the question had none) |
| missing answer for a question, or answer whose `type` differs from the asked type | the whole translation fails → 502 `decisions.upstream_error` ("the upstream's answer is incomplete") |
| `noul` | `{type:"predicate", name, probability: noul}` |
| `choice`, `probabilities` | `{type:"choice", name, choice: original typed value of the chosen key, confidence: upstream number, probabilities: [{value: original typed value, probability}]}` in the request's choice order; `probabilities` accepted as a map (by key) or an array of `{value|name, probability}`; a key missing from the upstream map → probability 0 |
| `score`, `legend`, `probabilities` | `{type:"score", name, score: upstream number, confidence, probabilities: [{value: i, label: original label i, probability}]}` for i in level order; `probabilities` accepted as a map keyed by index string or an array (positional, or objects with `value`) |
| `confidence` absent | `0` (the SDK requires the field) — documented 🟡 |
| `usage{input_tokens, output_tokens}` | `{input_tokens, input_tokens_details{cached_tokens:0, cache_write_tokens:0}, output_tokens, output_tokens_details{reasoning_tokens:0}, total_tokens: in+out}` |
| `model` | `model` = the client's requested model name (as the other translate paths do) |
| `id`, `object`, `created`, `diagnostics`, `joint`, `meta` | dropped 🟡 |
| refusal | never produced by translation (System One has none; an upstream error stays an error) |

Output field order: answers keep the order `type, name, <value fields>`; top level `answers, model, usage` — byte order is not a contract, but tests compare JSON values.

## 6. Confidence normalization (`decisions_confidence_normalize`)

- Per-token boolean, default false, column `api_tokens.decisions_confidence_normalize integer not null default 0`; `auth.Token.DecisionsConfidenceNormalize`; portal `TokenDTO`/create/update (`*bool`, nil keeps) and service tokens; ungated (like `claude_model_aliases`: a response-format preference, not a privilege).
- Rule, for every `choice` and `score` answer that carries a parseable `probabilities` (map or array) with a positive sum: `confidence = max(p_i) / Σ p_i`. Predicate/noul answers and answers without usable probabilities are left unchanged. All other fields and answers are preserved value-for-value.
- Where it applies: `/v1/decisions` translate (on the translated OpenAI body), `/v1/decisions` passthrough (OpenAI dialect), `/v1/systemone` (System One dialect). It turns those relays into buffered parse-and-rewrite for opted-in tokens only; for every other token the Phase 1 / §4.5 streaming relay is unchanged.
- An upstream 2xx body that is not JSON while normalization is on: relayed unchanged (normalization is best-effort, never turns a success into an error).

## 7. Buffered decisions response path

A new `nativeRelay` field (`decisionsRewrite`, nil on every other relay) makes `proxyNative` hand the response to `relayDecisionsResponse(w, r, ex, resp, rw)` (template: `relayTranscriptionResponse`): read the body bounded by `decisionsBodyLimit()` (over the bound or a read error → 502 `decisions.upstream_error` / `systemone.upstream_error`, nothing written, recorded as an error), then:

- 2xx: apply the rewrite (translate and/or normalize); a translate failure → 502 `decisions.upstream_error`; write with Content-Type `application/json` and Content-Length.
- non-2xx, translate route: normalize into the gateway envelope `{"error":{"code":"decisions.upstream_error","message":…,"type":"upstream_error"}}` with the upstream's status; the message carries the upstream's own message when one of the known shapes yields it (`{error:{message}}`, `{detail: string}`, `{detail:[{msg}]}`), otherwise a generic sentence. A 501 adds guidance: "the model does not answer System One decisions on this upstream".
- non-2xx, passthrough route (either endpoint): relayed verbatim (status, Content-Type, body).
- Usage on 2xx: tokens from the upstream body's `usage` (System One: input/output; OpenAI: also `cached_tokens`, `cache_write_tokens`), recorded once; capture gets the client-facing body.

## 8. `decision_openai` probe

- `provider.DecisionsProber` (`ProbeOpenAIDecisions(ctx, target, model) (verdict string, err error)`), same rules as `ProbeSystemOne`: POST `/v1/decisions` body `{"model":"<app model>","input":"ok","questions":[{"type":"predicate","name":"probe","instructions":"Is the input non-empty?"}]}` (`name` must be a string: vLLM rejects null). 2xx whose `answers` is a non-empty array with an object first element → yes; 404/501 → no except the agent router's `runtime.*` 404 → no verdict; 400/405/422 → refusal sentinel (escalating back-off); anything else → no verdict.
- Scope: a mapping the System One pass already considers (effective flavors include `decisions`) AND (external application type `vllm`, OR server_agent with runtime spec type `vllm` or `custom`). Other types never get the request.
- It rides the existing detached pass: per candidate, the System One probe and (in scope) the OpenAI probe, each with its own settled/back-off key (`systemone:<mapping>:<identity>` stays; new `decisions:<mapping>:<identity>`), its own row re-probe check (`decision_openai` row on file), loaded-only for agent mappings, 30 s timeout, unmetered, `writeCapabilityRows` precedence (an operator's verdict is never overwritten).

## 9. Errors (new codes)

| Code | Status |
|---|---|
| `decisions.model_required`, `decisions.questions_required`, `decisions.stream_unsupported`, `decisions.input_unsupported`, `decisions.question_invalid` | 400 |
| `decisions.endpoint_disabled`, `systemone.endpoint_disabled` | 404 |
| `decisions.upstream_error` (translate route), `systemone.upstream_error` (buffered System One over-bound/read failure only) | upstream status, or 502 for the gateway's own read/translate failure |
| `application.decisions_passthrough_unsupported`, `runtime_spec.decisions_passthrough_unsupported` | 400 |
| `application.decisions_passthrough_conflict` | 409 |

## 10. Acceptance (from #373 Phase 2) and what this branch can prove

- Table tests: every ✅/🟡 row of §5 and every ❌ row's code; order, name restore (incl. null), bool round trip, score index/label mapping, map- and array-shaped probabilities.
- Handler tests (httptest upstreams): translate end to end; passthrough byte relay + cached tokens on the usage row; auto picks passthrough with `decision_openai: yes` and translate otherwise; forced modes require their verdict; disabled → 404; normalization on/off on both endpoints and every mode; upstream error normalization; body cap 413; stream 400; bearer-only.
- Save guards: passthrough refused for llama_cpp app and spec (400/409), reset on type change.
- Probe tests mirroring Phase 1's.
- Not provable here (handed to the owner, as in Phase 1): live d3 translate (score index base against a real d3), a vLLM Qwen3 passthrough, the official OpenAI Python SDK (`client.decisions.create`) against both. The PR lists the commands.

## 11. Decisions taken for this phase (to record in ADR-098)

1. A request flavor `openai_decisions` beside the coarse `decisions`, instead of a second coarse flavor (one opt-in per application, as decided with the owner).
2. `systemone_mode` arrives now with only `passthrough|disabled`.
3. `auto` and the forced modes are resolved after the target is chosen, behind a derived pre-resolve requirement `any_decision`; forced modes still need their verdict (refuse on unknown, ADR-042).
4. The route taken is recorded through `provider_path`; no new usage column.
5. Normalization is ungated per token and buffers only opted-in requests.
6. The `decision_openai` probe is limited to `vllm` applications and `vllm`/`custom` specs; llama.cpp passthrough is refused on save.
7. Missing `confidence` becomes 0 on translate; a missing answer fails the request (502), never a refusal.

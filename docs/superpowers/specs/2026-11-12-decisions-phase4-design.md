# Decisions Phase 4: System One → OpenAI translation and OpenAI vendor accounts (#373)

This is a branch-local design. Its authority is issue #373: the Phase 4 acceptance criteria, translation matrix B, the error table and owner decisions 1–10, together with ADR-097/098/099 as shipped. Wire facts come from `2026-11-12-decisions-wire-schemas.md` (openai-python v3.49.0, vLLM v0.33.0) and `2026-11-12-openai-decisions-vendor.md` (the OpenAI reference, guide and pricing; the latest SDK is still v3.49.0).

## 1. Scope

In scope:
- **Vendor passthrough:** `POST /v1/decisions` (and `/openai/v1/decisions`) passes through to an **OpenAI api-key vendor account** (`api.openai.com`, model `gpt-6-luna`), under the per-token vendor access rules of external-vendor-accounts §11.
- **Reverse translation (matrix B):** System One → OpenAI Decisions, for `POST /v1/systemone` against an upstream that answers OpenAI Decisions only. That means the vendor account, and any self-hosted mapping whose only verdict is `decision_openai: yes`.
- **`systemone_mode` gains `auto` and `translate`**, plus a post-resolve System One route that mirrors the decisions route.

Out of scope:
- Anthropic, Gemini and OpenAI-compatible preset vendors, and OpenAI subscription (Codex) accounts. The ChatGPT backend has no Decisions endpoint.
- Per-question billing. Billing stays tokens only.
- A portal playground.

## 2. Facts the design rests on

- OpenAI Decisions is GA (2026-10-22). The only model is `gpt-6-luna`.
- The request takes `input`, `model`, `questions` (1–400), `prompt_cache_key`, `prompt_cache_options` and `safety_identifier`. It has no `stream`, `store`, `metadata` or `service_tier`, and no beta header.
- A question's `name` is a string; it is optional but not nullable.
- `instructions` is a string or parts (`input_text`, `input_image`, `input_json`).
- `predicate` has **no** field for what counts as yes or no. The criterion lives only in the instructions.
- `input_image` takes a data URL or a public http(s) URL, plus `detail`.
- The response is `{answers[], model, usage}`. Each answer carries `name` (null when unnamed), and answers may be `refusal`, which has no reason.
- Usage carries `input_tokens_details{cached_tokens, cache_write_tokens}`.
- The error envelope is not documented. The gateway assumes the standard OpenAI `{error:{message,type,code}}` and also reads `detail`.
- The gateway has no per-model token pricing: vendor rows cost 0 (energy-only cost), so `gpt-6-luna` needs no price entry.

## 3. Routing

### 3.1 System One modes and route

- `systemone_mode ∈ {auto, passthrough, translate, disabled}`.
  - New application and runtime spec rows default to **`auto`**.
  - Existing rows keep the stored `passthrough` that migration 107 wrote, so their Phase 1–3 behavior is unchanged. No migration is needed: the column is plain text.
- `translate` is refused on save for the passthrough-incapable kinds `llama_cpp` and `vllm_sr`, because they have no OpenAI `/v1/decisions`:
  - 400 `application.systemone_translate_unsupported` and `runtime_spec.systemone_translate_unsupported`;
  - 409 `application.systemone_translate_conflict` when the stored type is kept;
  - a retype to such a kind resets a stored `translate` to `auto`.
- `routing.EffectiveSystemOneRoute(kind string, mode EndpointMode, decision, decisionOpenAI bool) (route EndpointMode, ok bool)` is the twin of `EffectiveDecisionsRoute`:
  - `disabled` → (disabled, true).
  - `passthrough` → (passthrough, decision).
  - `translate` → (translate, decisionOpenAI && DecisionsPassthroughCapableKind(kind)).
  - `auto` (or blank) → passthrough if decision; otherwise translate if decisionOpenAI && capable; otherwise ("", false).
- `/v1/systemone` pre-resolve: the gate changes from `[decision]` to `[any_decision]`, which is kind-aware since Phase 3. After resolve, the handler computes the route exactly as `/v1/decisions` does:
  - `disabled` → 404 `systemone.endpoint_disabled`.
  - `!ok` → 404 `routing.model_not_capable`.
  - The concrete route is stored on the target copy, so `upstreamPath(target, decisions, endpointSystemOne)` answers `/v1/systemone` (passthrough) or `/v1/decisions` (translate).
- Every surface that judges `/v1/systemone` by mode alone now uses the route, so a model is listed and callable for System One exactly when the route is ok:
  - candidacy (`applicationServesEndpoint` for the coarse `decisions` flavor still filters `disabled`);
  - `MappingServesDecisionEndpoint`;
  - the `ModelDTO.Decision` fold;
  - `/v1/systemone/models`;
  - the unknown-model redirect.
- The portal mapping DTO gains `systemone_route` (`passthrough` | `translate` | `disabled` | `""`), beside `decisions_route`.

### 3.2 OpenAI vendor accounts

- **Decision models:** a vendor model is a decision model when its account is `vendor == openai` with `auth_type == api_key`, and its **upstream** model name is in `routing.OpenAIDecisionModels = {"gpt-6-luna"}`. The constant names the GA date. This is the only source of a vendor decision verdict; there is no portal flag and no migration.
- **Eligibility:** `vendorAccountRoutingEligible` admits a request whose `RequiredCapabilities` is exactly `[any_decision]` (both decision endpoints now carry it). For such a request, `resolveVendorAccount` considers only OpenAI api-key accounts and only decision models. Every other capability list stays off the vendor path, as today. The server-override, images and subscription guards are unchanged.
- **The target** (`vendorAccountTarget` for a decision request):
  - `APIFlavors` gains `decisions`.
  - `DecisionsMode = passthrough` and `SystemOneMode = translate`.
  - The kind stays `vendor_openai`, which is passthrough-capable.
- **Verdicts:** `decisionVerdicts` answers `decision = false` and `decision_openai = true` for a vendor target whose model is a decision model, with no store read; vendor targets have no mapping rows. Consequences:
  - `/v1/decisions` with `auto` passes through to `https://api.openai.com/v1/decisions`.
  - `/v1/systemone` with `auto` translates to `/v1/decisions`.
- **Access rules:** §11 is unchanged. Strict opt-in, own and shared accounts, and prefixes all apply, and `vendor_first` / `fallback_only` order applies as for chat.
- **Listings:** a decision model on an eligible account is listed with flavor `decisions`, beside its chat flavors if the catalog row has them. For `gpt-6-luna` it has decisions only.
  - It appears in `GET /v1/systemone/models`, because the route is ok through translate.
  - It counts as callable for both decision endpoints in the offering and redirect.
- **Catalog:** the static OpenAI api-key seed gains `gpt-6-luna`, and the discovery heuristic keeps it.
- **Usage:** the vendor row carries `AccountID` and tokens, including `cached_tokens` and `cache_write_tokens` from passthrough or translated answers. There is no energy and no cost.

## 4. Reverse translator (`internal/compat/decisions_reverse.go`)

These are pure functions with one table-driven test per matrix-B row.

- `TranslateSystemOneRequest(raw []byte, upstreamModel string) ([]byte, SystemOneAnswerTable, *DecisionsError)`. Error codes are `systemone.question_type_unsupported` and `systemone.field_unsupported`, both 400 at the handler.
- `TranslateOpenAIDecisionsResponse(body []byte, table SystemOneAnswerTable, clientModel string) ([]byte, DecisionsUsage, error)`. It returns `ErrSystemOneRefused` when any answer is a `refusal`.

Request mapping (matrix B):

| System One | OpenAI | |
|---|---|---|
| `model` | `model` = upstreamModel | ✅ |
| `state` string | `input` string | ✅ |
| `state` object/array | `input` = one user message with one `input_json` part `{data: state}` | ✅ |
| `images[]` (data or https URLs) | `input_image` parts in that user message, after the state part, in order | ✅ |
| `questions{}` in **object order** (an order-preserving decode, never a Go map) | `questions[]`, `name` = the key | ✅ |
| `noul` with no criteria | `predicate` | ✅ |
| `noul.criteria {true, false}` with descriptions | `predicate`, instructions + `"\n\nAnswer yes if: <true>"` / `"\n\nAnswer no if: <false>"` for the present non-null descriptions | 🟡 folded (OpenAI has no field) |
| `choice.criteria {key: desc|null}` | `choices[{value: key, description?}]` in key order | ✅ |
| `score.criteria [names]` | `levels[{label: name}]` | ✅ |
| `instructions` string (or absent → `""`) | `instructions` | ✅ |
| top-level `instructions` | ❌ `systemone.field_unsupported` (OpenAI has no request-level instructions) | |
| `box`, `set`, `span`, `track`, any unknown type | ❌ `systemone.question_type_unsupported` | |
| `assets`, `audio`, `videos`, `roles`, `options`, `preset`, `states`, `joint`, `chat_template_kwargs`, `cache_salt`, `priority`, `request_id` | ❌ `systemone.field_unsupported` | |
| `stream` (already refused by the validator) | — | |

Response mapping:

| OpenAI | System One | |
|---|---|---|
| `answers[]` (matched by `name`, else by position) | `answers{key: …}` in question order | ✅ |
| `predicate.probability` p | `{type:"noul", noul: p, probabilities:{yes: p, no: 1-p}, confidence: max(p, 1-p)}` | 🟡 confidence computed (OpenAI predicate has none) |
| `choice` | `{type:"choice", choice: key, probabilities:{key: p…}, confidence}` | ✅ |
| `score` | `{type:"score", score, legend:{"0": label…}, probabilities:{"0": p…}, confidence}` | ✅ |
| any `refusal` | whole request 422 `systemone.refused`, naming the refused question keys | 🟡 (System One semantics: one failed question fails the request) |
| `usage` with details | `{input_tokens, output_tokens}`; the details are **recorded** on the usage row first | 🟡 |
| `model` | the client's requested model | ✅ |

## 5. `/v1/systemone` translate path (gateway)

- **Gate:** `systemOneInferenceShape` requires `[any_decision]`.
- **Route:** after resolve and the flavor check, `systemOneRoute` (§3.1) runs. Passthrough is the Phase 1 relay, unchanged.
- **Translate:** `TranslateSystemOneRequest(raw, effectiveProviderModel)`. A refusal is a 400 with its code, with no upstream call and an error row.
  - The body goes to `/v1/decisions` through `proxyNative` with `upstreamBody`.
  - The response goes through the buffered decisions path with a reverse rewrite (dialect OpenAI → System One); this generalizes `decisionsRewrite`.
  - Normalization (`decisions_confidence_normalize`) applies to the System One output.
- **Errors on translate:**
  - An upstream non-2xx becomes `{"error":{"code":"systemone.upstream_error","message":…,"type":"upstream_error"}}` with the upstream status. The message comes from `error.message` / `detail`.
  - A missing answer or an unparseable body gives 502 `systemone.upstream_error`.
  - A refusal gives 422 `systemone.refused`.
- **Usage:** from the OpenAI body (`ParseDecisionsUsage(DialectOpenAI)`), so cached and cache-write tokens are recorded.

## 6. Errors (new codes)

| Code | Status |
|---|---|
| `systemone.question_type_unsupported`, `systemone.field_unsupported` | 400 |
| `systemone.refused` | 422 |
| `application.systemone_translate_unsupported`, `runtime_spec.systemone_translate_unsupported` | 400 |
| `application.systemone_translate_conflict` | 409 |

## 7. Acceptance (from #373 Phase 4) and what this branch can prove

**Proven here:**
- Table tests for every matrix-B row and every ❌ code.
- Order preservation, with ≥ 12 keys in object order.
- The predicate's computed fields and the refusal.
- Handler tests against httptest upstreams standing in for `api.openai.com`:
  - vendor passthrough of `/v1/decisions` (body value-equal, usage cached tokens, row `AccountID`);
  - `/v1/systemone` translated to a vendor and to a self-hosted mapping with only `decision_openai: yes`;
  - `auto` / `translate` / `passthrough` / `disabled` on `/v1/systemone`;
  - §11 access (no opt-in → not routable; prefix override);
  - a subscription or Anthropic account is never chosen;
  - listings and `/v1/systemone/models` include the vendor model.
- Save guards for `translate`.

**Not provable here** (handed to the owner):
- A real OpenAI api-key account with `gpt-6-luna` on both endpoints.
- The official SDK against the vendor route.
- The real error envelope and the treatment of `name`.
- Whether the api-key `/v1/models` lists `gpt-6-luna` (the seed covers it either way).

## 8. Decisions to record in ADR-100

1. Vendor decision models come from a fixed allowlist `{"gpt-6-luna"}` on OpenAI api-key accounts, not from an operator flag.
2. A `[any_decision]` request is the one capability list that opts into the vendor path. Every other capability-gated endpoint stays self-hosted.
3. `systemone_mode` gains `auto` (the new default for new rows) and `translate`. Existing rows keep `passthrough`, so there is no migration.
4. `/v1/systemone`'s gate becomes `any_decision` plus a post-resolve route, mirroring `/v1/decisions`.
5. `noul` criteria are folded into the predicate instructions. A refusal fails the whole System One request with `systemone.refused`. The predicate's `probabilities` and `confidence` are computed.
6. No pricing table: vendor rows stay cost-free (energy-only cost), as for chat.

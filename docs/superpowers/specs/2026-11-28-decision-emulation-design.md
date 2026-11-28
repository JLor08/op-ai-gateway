# Opt-in decision emulation on chat models (issue #380) — design

Branch-local working document. Durable content moves to `docs/architecture/` before the PR.

The user skipped the design questions ("skip question"), so every decision below is the
implementer's call unless the issue text fixes it. The issue fixes: opt-in only, both the
target and the caller must opt in, every emulated answer is marked with a response header
and a usage-row flag, default behaviour unchanged.

## 1. Intent

OpenAI's Decisions endpoint needs an API key. A ChatGPT subscription (Codex backend,
`chatgpt.com/backend-api/codex`) lists `gpt-6-luna` only as a Responses/chat model. An
operator who has only a subscription should still be able to send `/v1/decisions` and
`/v1/systemone` requests for `gpt-6-luna`, as long as both the account owner and the token
explicitly accept that the answers are **emulated**: a chat completion that asks the model to
estimate probabilities, not a native decision read.

## 2. Goals, non-goals, anti-goals

Goals:
- `POST /v1/decisions` and `POST /v1/systemone` answer, for an opted-in OpenAI subscription
  account's decision model (`routing.IsOpenAIDecisionModel`, today `gpt-6-luna`), through one
  structured-output Responses call per request.
- Opt-in on two sides: a per-vendor-account switch `decision_emulation` (owner-set, default
  off) **and** a per-token switch `decisions_emulation` (default off). Either off means the
  request behaves exactly as today (the subscription account is no decision candidate).
- Every emulated answer is marked: response header `X-OP-Decision-Emulated: chat`, and a
  usage-row column `decision_emulated` (true), shown in Activity.
- The answer has the exact client shape of each endpoint, so OpenAI SDK and System One clients
  keep working.

Non-goals (this PR):
- Other vendors, other subscription models, self-hosted chat models without a read strategy.
  The design keeps the emulator vendor-neutral (it drives the provider-neutral
  `inference.Request`), so widening is a routing change, not an emulator change.
- Token logprobs. Whether the Codex backend returns logprobs is unverified; the
  structured-output route works without them.
- Vendor-account-group failover on the emulated route (a limit error is relayed like any
  upstream error).
- A body field in the answer. The header and the usage row are the marks; adding a field
  would break strict SDK response models.

Anti-goals:
- Any change for a request without both opt-ins, including the api-key path of ADR-100.
- Presenting emulated probabilities as calibrated. They are the model's own estimates.

## 3. Decisions on the issue's open questions

1. **Reading probabilities: structured output (option b), with verbalized probabilities.**
   The model gets a strict JSON schema built per request and returns, per question, a
   probability for each option (predicate: one probability of "yes"). The gateway derives the
   verdict from those numbers, never from a separate "chosen" field, so verdict and
   probabilities cannot disagree.
2. **Calibration and honesty.** The numbers are uncalibrated self-estimates. A predicate
   probability is clamped to [0, 1]; choice and score values are floored at 0 and
   renormalized to sum 1, with no upper clamp (they act as relative weights; a sum of 0 is an
   unusable answer, 502). `confidence` is the largest probability (predicate:
   `max(p, 1-p)`). A confidence threshold still works mechanically, but the docs state that a
   threshold tuned on a native decision model does not carry over. The header and usage flag
   let a client tell the two apart.
3. **Cost.** One Responses call per request, all questions batched in one prompt and one schema.
   Reasoning effort `low` (enough for classification, the cheapest the backend accepts).
4. **Targets.** OpenAI subscription accounts only, decision-model allowlist only.
5. **Prompt format.** One fixed instruction template in `internal/compat`, plus one user message:
   the state (text, or JSON pretty-printed as text, plus images as image parts), then a numbered
   list of the questions with their key, type, instructions and options/levels with
   descriptions. Per type: predicate = "probability that the answer is yes"; choice = "a
   probability for each option"; score = "a probability for each level". The schema enforces
   the keys.
6. **Terms.** A new paragraph in External Vendor Accounts §10: classification traffic through a
   consumer subscription adds to the existing ToS exposure; the account switch is owner-only and
   its portal help text says so.

## 4. Behaviour

### 4.1 Routing

- `vendorAccountUsable` today admits only OpenAI api-key accounts for a decision request
  (`isVendorDecisionAccount`). It additionally admits an OpenAI **subscription** account when
  `acc.DecisionEmulation && token.DecisionsEmulation` (and the existing subscription switches
  hold).
- `vendorAccountModelTarget`: for a decision request on such an account, a row whose upstream
  model is `IsOpenAIDecisionModel` builds `withVendorDecisionEmulation(vendorSubscriptionOpenAITarget(...))`:
  `decisions` added to `APIFlavors`, `DecisionsMode = SystemOneMode = EndpointModeEmulate`,
  `DecisionEmulated = true`. Any other row fails closed as today.
- `EndpointModeEmulate = "emulate"` is a route value only, never stored and never accepted by
  `validEndpointMode` / `validDecisionsMode` / the System One mode validator.
- No preference between an api-key account and an emulating subscription account serving the
  same name: the resolver's existing account order applies (documented).
- `GET /v1/systemone/models` and the model listings show the emulated model for a token that
  would be routed to it (both opt-ins, §11 access), and only then.

### 4.2 The emulated route (gateway)

`decisionsRoute` / `systemOneRoute`: a target with `DecisionEmulated` takes route `emulate`
without reading verdicts. Then:

- `/v1/decisions`: `compat.BuildDecisionEmulation(raw, upstreamModel)` gives the neutral
  request and a table, or a `*DecisionsError` (400, recorded like other translator
  refusals). Its request-side rules are matrix A's input rules where they apply (refuse what
  cannot be expressed), except that an `input_image` with an `http(s)` URL is accepted (the
  vendor fetches it, the gateway does not).
- `/v1/systemone`: `compat.TranslateSystemOneRequest` first (matrix B, its refusals unchanged),
  then the same builder on the OpenAI body, then `compat.TranslateOpenAIDecisionsResponse` on
  the emulated OpenAI answer.
- The call goes through the provider client's buffered `Complete` (the Responses translate
  client), with the subscription auth context, the payload capture and the request timeout
  of a normal chat call.
- `compat.ParseDecisionEmulation(text, table, clientModel, usage)` turns the model's text into
  the OpenAI Decisions answer. Text that is not the schema's JSON (a code fence around it is
  tolerated) is `502 decisions.upstream_error` / `systemone.upstream_error` with tokens
  recorded (the vendor answered and is paid).
- A provider error (auth, unavailable, limit) is answered as the chat path answers it for the
  same error, with the endpoint's code family where the chat path's code is generic: the
  implementer maps provider sentinels through the existing helper, no new codes beyond
  `*.upstream_error`.
- Confidence normalization: harmless and still applied for an opted-in token.
- Every response written after the route settles on `emulate` (success or error) carries
  `X-OP-Decision-Emulated: chat`.
- Usage: token-metered from the Responses usage (cached tokens included), `AccountID` the
  serving account, `provider_path` `/responses`, `decision_emulated = true`, cost 0.

### 4.3 Structured output in the neutral model

New internal field on `inference.Request`: `OutputSchema *OutputSchema` (`json:"-"`) with
`Name string`, `Schema map[string]any`, `Strict bool`. Only the emulator sets it.
`OpenAIResponsesClient` renders it as `text: {format: {type: "json_schema", name, schema,
strict}}` (the shape Codex CLI's `--output-schema` sends; VERIFY-LIVE against the subscription
backend). Other provider clients ignore it (only the subscription target reaches the emulator).

### 4.4 Schema and answer mapping

Schema (strict: every object `additionalProperties: false`, every property required):
`{answers: {<qkey>: <per-type object>}}` where `qkey` is `q<i>` (zero-based request index;
names can be anything, so they are not used as keys).
- predicate: `{yes: number}`
- choice: `{probabilities: {<o<j>>: number ...}}` one property per choice in request order;
  the prompt shows `o<j>` next to the option's value and description.
- score: `{probabilities: {<l<k>>: number ...}}` one per level, the prompt shows the label.

Answer (OpenAI Decisions shape, request order, original `name`):
- predicate `{type:"predicate", name, probability: p}`
- choice `{type:"choice", name, choice: <original typed value of argmax>, confidence,
  probabilities:[{value, probability}]}`; ties go to the first option in request order.
- score `{type:"score", name, score: <argmax index>, confidence, probabilities:[{value:i,
  label, probability}]}`.
- `usage` in the Responses spelling (`input_tokens`, `input_tokens_details.cached_tokens`,
  `output_tokens`, `output_tokens_details.reasoning_tokens`, `total_tokens`), `model` the
  client's name.

### 4.5 Storage (migration 120, all three drivers)

- `vendor_accounts.decision_emulation integer not null default 0`
- `api_tokens.decisions_emulation integer not null default 0`
- `usage_events.decision_emulated integer not null default 0`

### 4.6 Portal

- `PUT /api/portal/vendor-accounts/{id}/decision-emulation` `{enabled}`: owner only, OpenAI
  subscription accounts only (`409 vendor_account.decision_emulation_unsupported` for others),
  mirroring the share-usage endpoint. DTO field `decision_emulation`.
- Token editor (portal tokens create/update, service tokens create): `decisions_emulation`,
  ungated like `decisions_confidence_normalize`.
- Activity: the usage DTO carries `decision_emulated`; the detail view shows an "emulated"
  badge.
- i18n keys in German and English.

## 5. Constraints

- Translators stay pure and inside `internal/compat`; `internal/provider` must not import
  `internal/compat` (archtest).
- Stable error codes; the one new code is `vendor_account.decision_emulation_unsupported`.
- ServerAgent untouched (no version bump).
- ADR-123 records the decision; architecture docs updated in the same branch.

## 6. Not verified live

The Codex backend accepting `text.format` json_schema for `gpt-6-luna`, the shape of the
answer text under it, and the token accounting. Marked VERIFY-LIVE in docs and constants.

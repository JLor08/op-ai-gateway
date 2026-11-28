# Decision emulation on chat models (#380) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `/v1/decisions` and `/v1/systemone` for an opted-in OpenAI subscription account's decision model (`gpt-6-luna`) by one structured-output Responses call, marked as emulated.

**Architecture:** The resolver admits an OpenAI subscription account for a decision request only when the account and the token both opted in, and marks the target `DecisionEmulated` with route `emulate`. The gateway handlers build a neutral `inference.Request` with a strict JSON schema (pure builder in `internal/compat`), call the provider's buffered `Complete`, and turn the model's JSON into the endpoint's answer shape. Three new boolean columns (migration 120) carry the two opt-ins and the usage mark.

**Tech Stack:** Go (gateway/backend), React/TypeScript (gateway/frontend), SQLite/Postgres/memory stores.

**Spec:** `docs/superpowers/specs/2026-11-28-decision-emulation-design.md` (read it first; section numbers below refer to it).

## Global Constraints

- Migration number 120; columns exactly `vendor_accounts.decision_emulation`, `api_tokens.decisions_emulation`, `usage_events.decision_emulated`, each `integer not null default 0`, via `addColumnIfMissing`.
- Response header exactly `X-OP-Decision-Emulated: chat`.
- Route value `routing.EndpointModeEmulate = "emulate"`; never accepted by any mode validator.
- New error code exactly `vendor_account.decision_emulation_unsupported` (409).
- Reasoning effort on the emulated call: `low`.
- `internal/provider` must not import `internal/compat`; translators are pure.
- Every store change verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- i18n: German and English keys together.
- No ServerAgent change, no agent version bump.
- Repo text in English.

## Review Focus

- A token with the opt-in but an account without it (and vice versa): the subscription account must stay no decision candidate; the request answers as today (`404 routing.no_model_route` / `model_not_capable` as before). Test in Task 4.
- Model output wrapped in a ```json fence or with leading prose before the JSON: fence tolerated, anything else 502 with tokens recorded. Test in Task 3.
- All probabilities 0 for a choice question, or a negative/ >1 value: clamp, then sum 0 → 502 naming the question. Test in Task 3.
- Duplicate or missing `name` on OpenAI questions: the answer keeps the original `name` (absent stays absent/null as matrix A restores it). Test in Task 3.
- A provider auth failure on the emulated route still carries `X-OP-Decision-Emulated` and records a row with `decision_emulated = true`. Test in Task 5.

---

### Task 1: Storage and model fields

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (register migration 120 after 119; one function adding the three columns)
- Modify: `store/models.go`, `store/sqlite_token.go`, `store/sqlite_vendor_accounts.go`, `store/sqlite_usage.go`, `auth/token_store.go`, `routing/store.go`, `routing/memory_store.go`, `usage/recorder.go`, `tracing/routingstore_gen.go`, `portal/memory_directory.go`
- Test: `store/migration120_decision_emulation_test.go`, `store/conformance_test.go`

Mirror `decisions_confidence_normalize` (migration 108) for the token column and `share_usage_with_members` (migration 110) for the account column, file by file as they did.

**Interfaces:**
- Produces: `auth.Token.DecisionsEmulation bool`; `store.TokenRecord.DecisionsEmulation bool`; `routing.VendorAccount.DecisionEmulation bool`; `routing.Store.UpdateVendorAccountDecisionEmulation(ctx context.Context, id string, enabled bool) error`; `usage.Event.DecisionEmulated bool` (`json:"decision_emulated"`), persisted and read back by every driver.

- [ ] Step 1: Write failing tests: migration 120 adds the three columns with default 0 on an existing DB; conformance round-trips each flag (token create/update/resolveBearer, vendor account update + read, usage record + list) on memory/sqlite/postgres.
- [ ] Step 2: Run `cd gateway/backend && go test ./internal/store/ -run 'Migration120|Conformance'` — FAIL.
- [ ] Step 3: Implement.
- [ ] Step 4: Run the same with and without `OP_AI_GATEWAY_TEST_POSTGRES_DSN` — PASS; `go test ./internal/... ` compiles.
- [ ] Step 5: Commit `feat(store): decision emulation opt-in columns and usage mark (#380)`.

### Task 2: Structured output in the neutral request

**Files:**
- Modify: `internal/inference/types.go`, `internal/provider/openai_responses.go`
- Test: `internal/provider/openai_responses_test.go`

**Interfaces:**
- Produces: `inference.OutputSchema{Name string; Schema map[string]any; Strict bool}`; `inference.Request.OutputSchema *OutputSchema` (`json:"-"`).

- [ ] Step 1: Failing test `TestOpenAIResponsesRequestBodyOutputSchema`: a request with `OutputSchema{Name:"decisions", Schema:{"type":"object"}, Strict:true}` renders `"text":{"format":{"type":"json_schema","name":"decisions","schema":{"type":"object"},"strict":true}}`; a request without it renders no `text` key.
- [ ] Step 2: Run — FAIL.
- [ ] Step 3: Add an `omitempty` `Text` field to `openaiResponsesRequest`; mark the shape VERIFY-LIVE in a comment.
- [ ] Step 4: Run `go test ./internal/provider/` — PASS.
- [ ] Step 5: Commit.

### Task 3: The pure emulator (`internal/compat/decisions_emulation.go`)

**Files:**
- Create: `internal/compat/decisions_emulation.go`, `internal/compat/decisions_emulation_test.go`

**Interfaces:**
- Consumes: `inference.Request`, `inference.OutputSchema`, `compat.DecisionsError`, `compat.DecisionsUsage`.
- Produces:
  - `BuildDecisionEmulation(raw []byte, upstreamModel string) (inference.Request, DecisionEmulationTable, *DecisionsError)` — input is an OpenAI Decisions body. Output request: `Model` = upstreamModel, `ReasoningEffort: "low"`, `Stream: false`, one system message (the fixed instruction template), one user message (state text / pretty-printed JSON / image parts in order, then the question list), `OutputSchema` per spec §4.4.
  - `ParseDecisionEmulation(text string, table DecisionEmulationTable, clientModel string, usage inference.Usage) ([]byte, error)` — returns the OpenAI Decisions answer body (spec §4.4).
  - `DecisionEmulationTable` (unexported fields: per question the original raw `name`, type, typed choice values, level labels).

Input rules: reuse matrix A's input parsing (`TranslateDecisionsRequest` helpers in `decisions.go`) for questions, names, instructions and input items, with one difference: `input_image` with an `http(s)` URL is accepted as an image part. Refusal codes are matrix A's (`decisions.input_unsupported`, `decisions.question_invalid`).

- [ ] Step 1: Failing tests:
  - `TestBuildDecisionEmulationSchema`: predicate + choice (3 options, one boolean value) + score (2 levels) → schema keys `q0`,`q1`,`q2`; `q1.probabilities` has exactly `o0,o1,o2`; every object `additionalProperties:false` and all keys required; `ReasoningEffort=="low"`.
  - `TestBuildDecisionEmulationPrompt`: the user text contains each question's instructions, each option value and description next to its `o<j>`, each level label next to `l<k>`; a JSON input appears pretty-printed; an http image becomes an image part.
  - `TestBuildDecisionEmulationRefuses`: unknown type → `decisions.question_invalid`; role `system` input → `decisions.input_unsupported`.
  - `TestParseDecisionEmulation`: `{"answers":{"q0":{"yes":0.8},"q1":{"probabilities":{"o0":1,"o1":3,"o2":0}},"q2":{"probabilities":{"l0":0.2,"l1":0.6}}}}` → predicate probability 0.8; choice = value of o1 (typed), probabilities 0.25/0.75/0, confidence 0.75; score 1, probabilities 0.25/0.75, labels restored; names restored (absent stays absent, `""` stays `""`); usage spelled as Responses with cached tokens.
  - `TestParseDecisionEmulationFenceAndErrors`: a ```json fenced body parses; prose before the JSON errors; clamping of -1 and 2; all-zero choice errors naming the question; missing question key errors; tie picks the first option.
- [ ] Step 2: Run `go test ./internal/compat/ -run DecisionEmulation` — FAIL.
- [ ] Step 3: Implement; round all derived numbers to 12 decimals like matrix B.
- [ ] Step 4: Run — PASS.
- [ ] Step 5: Commit.

### Task 4: Routing admission and the emulated target

**Files:**
- Modify: `internal/routing/endpoint_mode.go`, `internal/routing/vendor_decisions.go`, `internal/routing/resolver.go` (`vendorAccountUsable`, `vendorAccountModelTarget`, `Target`)
- Test: `internal/routing/resolver_vendor_decision_emulation_test.go`

**Interfaces:**
- Consumes: `VendorAccount.DecisionEmulation`, `auth.Token.DecisionsEmulation` (Task 1).
- Produces: `EndpointModeEmulate EndpointMode = "emulate"`; `Target.DecisionEmulated bool`; `withVendorDecisionEmulation(t Target) Target`; `isVendorDecisionEmulationAccount(acc VendorAccount, token auth.Token) bool`.

- [ ] Step 1: Failing tests (table): subscription account + `gpt-6-luna` row, decision request on both flavors: (account on, token on) → target with `DecisionEmulated`, both modes `emulate`, `Subscription` true, flavors include `decisions`; (on, off), (off, on), (off, off) → no vendor target (existing error); account on + token on but row `gpt-5` → not served; a chat request for `gpt-6-luna` on the same account → unchanged chat target (no `DecisionEmulated`); the api-key path of ADR-100 unchanged; the group path (`resolveVendorGroup`) honours the same rule.
- [ ] Step 2: Run `go test ./internal/routing/ -run DecisionEmulation` — FAIL.
- [ ] Step 3: Implement; also make `portal.validEndpointMode` / `validDecisionsMode` / the System One validator reject `emulate` (add a test in `portal` for that).
- [ ] Step 4: Run `go test ./internal/routing/ ./internal/portal/` — PASS.
- [ ] Step 5: Commit.

### Task 5: The gateway's emulated route

**Files:**
- Create: `internal/gateway/decisions_emulation.go`, `internal/gateway/decisions_emulation_test.go`
- Modify: `internal/gateway/decisions_handler.go` (`decisionsRoute`, `relayDecisions`), `internal/gateway/systemone_handler.go` (`systemOneRoute`, `relaySystemOne`), `internal/gateway/inference_complete.go` (`usageMeta` / `recordUsage` carries `DecisionEmulated`), `internal/gateway/auth.go` (header constant)
- Modify: the model-listing fold for `/v1/systemone/models` and the vendor decision listing so an emulated model is listed only for a token that would route to it.

**Interfaces:**
- Consumes: Tasks 2–4.
- Produces: `headerDecisionEmulated = "X-OP-Decision-Emulated"`; `(s *Server) relayDecisionEmulation(w, r, c decisionsCall, target routing.Target, openAIBody []byte, systemOne *compat.SystemOneAnswerTable)` — one function for both endpoints (`systemOne` nil on `/v1/decisions`).

Flow per spec §4.2: build → `s.upstreamAuthCtx` + capture context as `complete` uses → `completeProvider` → parse → (System One: reverse-translate) → optional confidence normalization → write with `Content-Type: application/json` and the header → `recordUsage` with `DecisionEmulated`, `provider_path` `/responses`. Provider errors go through the same status/code mapping `complete` uses.

- [ ] Step 1: Failing tests with a fake provider client and a seeded subscription vendor account:
  - `/v1/decisions` happy path: 200, header set, OpenAI shape, the provider saw `OutputSchema` and `ReasoningEffort "low"`, usage row `decision_emulated` true with tokens and account id.
  - `/v1/systemone` happy path: System One shape keyed by the client's keys.
  - unparseable model text → 502 `decisions.upstream_error` / `systemone.upstream_error`, header set, tokens recorded.
  - provider `ErrAuthRejected` → the chat path's status, header set, row recorded with `decision_emulated`.
  - translator refusal (unknown question type) → 400, header set, no provider call.
  - token without opt-in → no provider call, today's 404.
  - `GET /v1/systemone/models`: listed for the opted-in token only.
- [ ] Step 2: Run `go test ./internal/gateway/ -run DecisionEmulation` — FAIL.
- [ ] Step 3: Implement.
- [ ] Step 4: Run `go test ./internal/gateway/` — PASS.
- [ ] Step 5: Commit.

### Task 6: Portal API

**Files:**
- Modify: `internal/portal/service_vendor_accounts.go` (DTO `DecisionEmulation`), `internal/portal/service_vendor_sharing.go` (`SetVendorAccountDecisionEmulation`), `internal/portal/api.go`, `internal/portal/api_tracing_gen.go`, `internal/portal/service.go` (token DTO/create/update/run-as), `internal/portal/service_services.go` (service token), usage DTO in the activity service, `internal/gateway/portal_vendor_account_endpoints.go` (`PUT /api/portal/vendor-accounts/{id}/decision-emulation`)
- Docs later (Task 8)

**Interfaces:**
- Produces: JSON fields `decision_emulation` (vendor account DTO), `decisions_emulation` (token DTOs and create/update requests), `decision_emulated` (usage DTO); `SetVendorAccountDecisionEmulationRequest{Enabled *bool}`.

- [ ] Step 1: Failing tests: owner sets/clears on an OpenAI subscription account (200, DTO reflects); a CM or non-owner → the share-usage endpoint's forbidden answer; an api-key or Anthropic account → 409 `vendor_account.decision_emulation_unsupported`; missing `enabled` → 400 like share-usage; token create/update round-trip `decisions_emulation`; service token create; usage DTO carries `decision_emulated`.
- [ ] Step 2: Run `go test ./internal/portal/ ./internal/gateway/ -run 'DecisionEmulation|TokenDecisionsEmulation'` — FAIL.
- [ ] Step 3: Implement.
- [ ] Step 4: PASS.
- [ ] Step 5: Commit.

### Task 7: Frontend

**Files:**
- Modify: `frontend/src/api/vendorAccounts.ts`, `frontend/src/components/VendorAccountSharePanel.tsx` or the account detail panel (a toggle visible to the owner of an OpenAI subscription account only, with a help text naming emulation and the ToS caveat), `frontend/src/api/tokens.ts`, `frontend/src/api/services.ts`, `frontend/src/components/TokenList.tsx`, `frontend/src/components/ServiceTokensSection.tsx`, the activity detail component (an "Emulated" badge when `decision_emulated`), `frontend/src/i18n.ts`
- Test: the matching `*.test.tsx`, `frontend/src/i18n.test.ts`

- [ ] Step 1: Failing tests: toggle shown only for an owned OpenAI subscription account and calls the endpoint; token checkbox sends `decisions_emulation` on create and update; service token create sends it; activity badge renders.
- [ ] Step 2: `cd gateway/frontend && npx vitest run <files>` — FAIL.
- [ ] Step 3: Implement.
- [ ] Step 4: `npm test`, `npm run build`, `npm run lint`, `npm run format:check` — PASS.
- [ ] Step 5: Commit.

### Task 8: Architecture docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-123), `cross-cutting/compatibility-and-inference.md` (§3.9 and §3.10: new item "Emulated route"), `cross-cutting/external-vendor-accounts.md` (§4.2 new *Subscription decision emulation* subsection replacing the "separate follow-up, issue #380" sentence; §10 ToS paragraph and VERIFY-LIVE list), `cross-cutting/telemetry-usage-observability.md` (usage column), `reference/data-model.md`, `reference/api-surface.md`, `reference/openapi.yaml`, `cross-cutting/security-auth-rbac.md` if tokens' flags are listed there, `05-building-block-view.md` (new compat file)

- [ ] Step 1: Write the docs from the spec (decisions as shipped, uncalibrated-probability caveat, not verified live).
- [ ] Step 2: `make lint-docs` — PASS.
- [ ] Step 3: Commit.

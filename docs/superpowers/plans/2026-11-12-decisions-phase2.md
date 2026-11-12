# Decisions Phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve OpenAI's Decisions API (`POST /v1/decisions`, `/openai/v1/decisions`) through the gateway, translated to System One or passed through per mapping, with endpoint modes, a `decision_openai` verdict and probe, and per-token confidence normalization.

**Architecture:** A request flavor `openai_decisions` (normalizing to the coarse `decisions`) is gated pre-resolve on a derived `any_decision` requirement; after resolve the handler turns the target's `decisions_mode` plus the mapping's verdicts into a concrete route. Translate uses pure functions in `internal/compat/decisions.go` and a buffered response path in `proxyNative`; passthrough reuses the native relay. Modes live on applications and runtime specs (migration 107), the token flag on `api_tokens` (migration 108).

**Tech Stack:** Go (gateway/backend), React + TypeScript + Vitest (gateway/frontend), sqlite/postgres migrations in Go.

**Spec:** `docs/superpowers/specs/2026-11-12-decisions-phase2-design.md` (authority). Pointers: `docs/superpowers/specs/2026-11-12-decisions-phase2-inventory.md`. Wire facts: `docs/superpowers/specs/2026-11-12-decisions-wire-schemas.md`.

## Global Constraints

- Branch `feat/decisions-api-373-phase2`; never commit to `main`. Commit messages in English, conventional style, `(#373)` suffix, no attribution lines.
- No LAN host names in any repo text.
- Code and docs never cite `docs/superpowers/` (check-docs forbids it); cite ADR-098 / the architecture docs instead.
- Migrations: append-only in `internal/store/migrate.go`; next versions **107** (`application_decision_modes`) and **108** (`token_decisions_confidence_normalize`); same code path for sqlite and postgres.
- No server-agent change, no agent version bump.
- Error envelope and codes exactly as spec §9.
- Go: `gofumpt`/`golangci-lint` v2.15.0 clean (`golangci-lint fmt --diff` and `run ./...` per module). Frontend: `npm test`, `npm run build`, `npm run lint`, `npm run format:check`.
- Postgres leg: any store/migration change is tested with the postgres DSN as CI does (the subtests skip silently without it).
- Known local-only failures, not to chase: `TestVoice*` / `TestLiveTranscriptionLoopback*` (fail on main locally); `make lint-docs` agent-extractor line on macOS (#377).

## Review Focus

1. A JSON `questions` order that a Go map would scramble — the System One body must keep request order (test with ≥ 12 questions whose names sort differently from their order).
2. A choice list with both `"true"` (string) and `true` (bool), and bool-valued choices round-tripping as bools, not strings.
3. An upstream System One answer whose `probabilities` is an array instead of a map, or lacks a key — translation must not panic or drop answers.
4. A `server_agent` spec with `decisions_mode: disabled` under an app with `auto` — the spec wins at dispatch (404 `decisions.endpoint_disabled`), and the same for `systemone_mode`.
5. A token with normalization off must keep the exact Phase 1 streaming relay on `/v1/systemone` (no buffering, byte-identical body).

---

### Task 1: Routing vocabulary, modes on applications/specs, migration 107

**Files:**
- Modify: `gateway/backend/internal/routing/endpoint_mode.go`, `internal/routing/store.go` (constants, `Application`, `RuntimeSpec`, `applicationServesEndpoint`), `internal/routing/served_flavors.go` (`EffectiveSpecFields`, `EffectiveFields`, `NormalizeAPIFlavor`), `internal/routing/resolver.go` (`Target` fields, `targetFrom` copy, `capabilityRowsSatisfy`)
- Modify: `internal/store/migrate.go`, `internal/store/sqlite_applications.go`, `internal/store/sqlite_runtime.go`
- Test: `internal/routing/served_flavors_test.go`, `internal/routing/resolver_endpoint_mode_test.go`, `internal/routing/target_completeness_test.go`, a new `internal/routing/capability_any_decision_test.go`, `internal/store/migration107_decision_modes_test.go`, the parity tests named in the inventory, `internal/store/sqlite_migration_test.go`

**Interfaces:**
- Produces: `routing.EndpointModeAuto EndpointMode = "auto"`; `routing.APIFlavorOpenAIDecisions = "openai_decisions"` (`NormalizeAPIFlavor` → `"decisions"`); `routing.CapabilityDecisionOpenAI = "decision_openai"`; `routing.CapabilitySourceDecisionsProbe = "decisions_probe"` (rank 1, like `systemone_probe`); `routing.CapabilityAnyDecision = "any_decision"`; fields `DecisionsMode`, `SystemOneMode EndpointMode` on `Application`, `RuntimeSpec`, `EffectiveSpecFields`, `Target`; store columns `decisions_mode`, `systemone_mode` (`text not null default ''`) on `applications` and `agent_runtime_specs`.

- [ ] **Step 1: Write failing tests**
  - `TestNormalizeAPIFlavorOpenAIDecisions`: `NormalizeAPIFlavor("openai_decisions") == "decisions"`; the portal flavor whitelist is NOT touched (stored flavors still reject it — assert in Task 2).
  - `TestCapabilityAnyDecisionSatisfied`: rows `{decision:yes}` → true; `{decision_openai:yes}` → true; `{decision:no, decision_openai:no}` → false; `{}` → false; `[any_decision, image]` with only `decision:yes` → false.
  - `TestApplicationServesEndpointDecisionModes`: ordinary app with flavor `decisions`: `SystemOneMode=disabled` → not served for `"decisions"`, served for `"openai_decisions"`; `DecisionsMode=disabled` → the reverse; server_agent app with both disabled → still served (dispatch decides).
  - `TestEffectiveFieldsDecisionModes`: server_agent spec row's modes win over the app's; without a spec row the app's.
  - `TestTargetFromCopiesDecisionModes` (or extend `target_completeness_test.go` so the new fields are asserted).
  - `TestMigration107DecisionModes`: after migrating, existing application and spec rows read `decisions_mode="auto"`, `systemone_mode="passthrough"`; a re-run is idempotent; both dialects.
  - Extend column-parity allow-lists and the migration count.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/routing/ ./internal/store/ -run 'Decision|Migration107|Parity|ServesEndpoint|EffectiveFields|TargetFrom|Normalize'` — expect FAIL.
- [ ] **Step 3: Implement** the constants and fields; `capabilityRowsSatisfy` gains the `CapabilityAnyDecision` case beside `CapabilityImageEdit`; `applicationServesEndpoint` gains the two cases; migration 107 adds the four columns and backfills `'' → 'auto'` / `'' → 'passthrough'` on both tables; store read/write of the columns (an empty value read back is normalized to the default in the scan, so rows written by an older binary stay valid).
- [ ] **Step 4: Run** the same tests, then `go test ./internal/routing/ ./internal/store/` and the postgres leg — expect PASS.
- [ ] **Step 5: Commit** `feat(routing): decisions and System One endpoint modes, the decision_openai capability and migration 107 (#373)`

### Task 2: Portal — mode validation, type guard, verdict, effective route

**Files:**
- Modify: `internal/portal/service_applications.go` (DTOs, create/update, `validDecisionsMode`, `validSystemOneMode`, guard + conflict sentinels, `reservedManualVerdicts`), `internal/portal/service_runtime.go` (spec DTO/request/validation/row/DTO), `internal/portal/service_runtime_benchmark.go` (`putRequestFromDTO`), `internal/portal/service_model_offering.go` (capability-name switch), `internal/portal/service.go` (`ModelDTO.DecisionOpenAI` fold; `ModelMappingDTO.DecisionsRoute`), `internal/gateway/agent_ingest.go` (`reservedAgentCapabilityNames` + `decision_openai`, and `any_decision` refused as a row name everywhere), `internal/gateway/portal_application_endpoints.go`, `internal/gateway/portal_runtime_endpoints.go` (HTTP mapping)
- Test: `internal/portal/service_applications_test.go` (or a new `service_decision_modes_test.go`), `internal/portal/service_runtime_test.go`, `internal/gateway/portal_application_endpoint_mode_test.go`, `internal/gateway/portal_runtime_endpoints_test.go`, portal capability tests

**Interfaces:**
- Consumes: Task 1 constants and fields.
- Produces: JSON fields `decisions_mode`, `systemone_mode` on application and runtime-spec DTOs/requests (update requests `*string`, nil keeps); `ModelDTO.DecisionOpenAI bool` (`json:"decision_openai"`, always present); `ModelMappingDTO.DecisionsRoute string` (`json:"decisions_route"`: `"translate"`, `"passthrough"`, `"disabled"` or `""` when the mapping does not list `decisions` or has neither verdict); sentinels `ErrApplicationDecisionsPassthroughUnsupported` (`application.decisions_passthrough_unsupported`, 400), `ErrApplicationDecisionsPassthroughConflict` (`application.decisions_passthrough_conflict`, 409), `ErrRuntimeSpecDecisionsPassthroughUnsupported` (`runtime_spec.decisions_passthrough_unsupported`, 400). Exported helper `routing.EffectiveDecisionsRoute(mode EndpointMode, decision, decisionOpenAI bool) (route EndpointMode, ok bool)` (put it in `internal/routing`, it is reused by Task 6) implementing the spec §3.4 table.

- [ ] **Step 1: Write failing tests**
  - Create without modes → `auto`/`passthrough`; `decisions_mode: "auto"` accepted, `responses_mode: "auto"` → `application.endpoint_mode_invalid`; `systemone_mode: "translate"` → invalid.
  - `llama_cpp` app create with `decisions_mode: passthrough` → 400 `application.decisions_passthrough_unsupported`; update of a stored llama_cpp app to passthrough without `type` → 409 conflict; update changing type to llama_cpp with stored passthrough → stored becomes `auto`.
  - Spec of type `llama_cpp` with passthrough → 400 `runtime_spec.decisions_passthrough_unsupported`; spec modes round-trip through PUT/GET and `putRequestFromDTO`.
  - Manual `decision_openai: yes` accepted, `decision_openai: no` → reserved; `any_decision` as a manual or agent row name refused; agent ingest drops `decision_openai`.
  - `ModelDTO.decision_openai` present (false) on every model; true with a yes row.
  - `TestEffectiveDecisionsRoute`: every row of spec §3.4.
  - `decisions_route` on mapping DTOs: auto+decision_openai yes → passthrough; auto+decision only → translate; disabled → disabled; no `decisions` flavor → "".
- [ ] **Step 2: Run** `go test ./internal/portal/ ./internal/gateway/ -run 'Decision|EndpointMode|RuntimeSpec|Capabilit'` — FAIL.
- [ ] **Step 3: Implement** following the live-timings and endpoint-mode templates (inventory).
- [ ] **Step 4: Run** the packages — PASS.
- [ ] **Step 5: Commit** `feat(portal): decisions_mode and systemone_mode, the decision_openai verdict and the effective decisions route (#373)`

### Task 3: Per-token `decisions_confidence_normalize`, migration 108

**Files:**
- Modify: `internal/store/migrate.go`, `internal/store/models.go`, `internal/store/sqlite_token.go`, `internal/auth/token_store.go`, `internal/portal/service.go` (token DTOs, create/update, run-as copy, `tokenDTO`), `internal/portal/service_services.go`, `internal/portal/memory_directory.go`
- Test: `internal/store/migration108_token_decisions_confidence_normalize_test.go`, `internal/store/conformance_test.go`, `internal/portal/service_token_test.go`, `internal/portal/memory_directory_test.go`, service-token tests

**Interfaces:**
- Produces: column `api_tokens.decisions_confidence_normalize integer not null default 0`; `store.TokenRecord.DecisionsConfidenceNormalize bool`; `auth.Token.DecisionsConfidenceNormalize bool` (set by `LookupBearer`, copied by `AuthorizeRunAsToken`); JSON `decisions_confidence_normalize` on `TokenDTO` (always present), create (bool) and update (`*bool`) requests, service-token equivalents. Ungated (no `principalMayManageTokenSetting`).

- [ ] **Step 1: Failing tests:** migration adds the column (both dialects, idempotent); create/update/lookup round-trip; nil update keeps; run-as principal carries it; a non-admin owner may set it on their own token.
- [ ] **Step 2: Run** `go test ./internal/store/ ./internal/portal/ ./internal/auth/ -run 'Decisions|Migration108|Token'` — FAIL.
- [ ] **Step 3: Implement** mirroring `claude_model_aliases` (ungated sibling of `claude_effort_compatibility`).
- [ ] **Step 4: Run** with the postgres leg — PASS.
- [ ] **Step 5: Commit** `feat(tokens): per-token decisions_confidence_normalize (#373)`

### Task 4: Translator `internal/compat/decisions.go`

**Files:**
- Create: `gateway/backend/internal/compat/decisions.go`, `gateway/backend/internal/compat/decisions_test.go`

**Interfaces:**
- Produces:
  - `type DecisionsError struct{ Code, Message string }` with codes `decisions.input_unsupported`, `decisions.question_invalid` (the handler maps it to `*inference.Error` 400).
  - `type DecisionsAnswerTable struct{ … }` (opaque to callers; fields per spec §5).
  - `func TranslateDecisionsRequest(raw []byte, upstreamModel string) ([]byte, DecisionsAnswerTable, *DecisionsError)`
  - `func TranslateSystemOneResponse(body []byte, table DecisionsAnswerTable, clientModel string) ([]byte, DecisionsUsage, error)` — `DecisionsUsage{InputTokens, OutputTokens, CachedTokens, CacheWriteTokens int}`.
  - `type DecisionsDialect int` with `DialectOpenAI`, `DialectSystemOne`; `func NormalizeDecisionsConfidence(body []byte, d DecisionsDialect) ([]byte, bool)`.
  - `func ParseDecisionsUsage(body []byte, d DecisionsDialect) DecisionsUsage`.

- [ ] **Step 1: Write the table-driven tests**, one case per spec §5 row (request and response tables) and §6:
  - request: string input; single user message string; two user messages joined `"\n\n"`; text parts joined; lone `input_json` → state object; JSON + text → state array `["text", {…}]`; `data:` image → `images`, `detail` absent; `https:` image → `decisions.input_unsupported`; `file_id` image → input_unsupported; system/developer/assistant role and a `function_call` item → input_unsupported; `prompt_cache_key`/`safety_identifier` absent from output; 12 questions named `z, y, …` keep request order in the raw output bytes; missing/null name → `q<i>`; duplicate names `a`,`a` → `a`,`a-2`; predicate → `noul` without criteria; choice string/bool values → criteria keys with `null` for empty description; `"true"` + `true` → question_invalid; score levels with/without description → `"label: description"` / `"label"`; instructions parts text-only joined; image part in instructions → input_unsupported; unknown type → question_invalid.
  - response: answers ordered with names restored incl. JSON `null`; noul→predicate probability; choice bool value restored in `choice` and `probabilities[].value`; choice probabilities as array; missing probability key → 0; score probabilities as map `{"0":…}` and as positional array → `[{value:0,label:…},…]` with original labels (not the folded ones); missing confidence → 0; missing answer → error; type mismatch → error; usage details zero-filled, `total_tokens` = in+out; `id/object/created/diagnostics` absent; `model` = clientModel.
  - normalize: OpenAI choice `[0.2,0.6]` → confidence 0.75; System One score map `{"0":0.1,"1":0.3}` → 0.75; predicate untouched; zero-sum probabilities untouched; non-JSON → (body, false); every other field byte-value preserved (compare decoded JSON).
- [ ] **Step 2: Run** `go test ./internal/compat/ -run Decisions` — FAIL.
- [ ] **Step 3: Implement.** Build the System One `questions` object with `bytes.Buffer` + `json.Marshal` per key/value (never a Go map). Decode inputs with `json.RawMessage` so typed values (bool vs string) survive. Write OpenAI answers as ordered structs.
- [ ] **Step 4: Run** — PASS; `go vet` clean.
- [ ] **Step 5: Commit** `feat(compat): translate OpenAI Decisions to System One and normalize decision confidence (#373)`

### Task 5: `decision_openai` probe

**Files:**
- Create: `internal/provider/decisions_probe.go`, `internal/provider/decisions_probe_test.go`
- Modify: `internal/provider/` multiplexer route for the new interface (as `SystemOneProber`), `cmd/gateway/app_health.go` (pass extension), `cmd/gateway/app_health_systemone_test.go` (or a new `app_health_decisions_probe_test.go`)

**Interfaces:**
- Consumes: Task 1 `CapabilityDecisionOpenAI`, `CapabilitySourceDecisionsProbe`.
- Produces: `provider.DecisionsProber` interface `ProbeOpenAIDecisions(ctx context.Context, target routing.Target, model string) (string, error)`; `ErrDecisionsProbeRefused` (wraps `ErrUnavailable`); the body in spec §8.

- [ ] **Step 1: Failing tests**
  - provider: exact request body bytes (`{"model":"m","input":"ok","questions":[{"type":"predicate","name":"probe","instructions":"Is the input non-empty?"}]}`); 200 with `answers:[{…}]` → yes; 200 with `answers:[]` or non-JSON → "" + error; 501 → no; plain 404 → no; router `runtime.model_not_managed` 404 → "" ; 400/405/422 → refusal sentinel; 503 → "".
  - pass: an external `vllm` app mapping with `decisions` gets both probes and both rows written with their sources; an external `llama_cpp` app gets only the System One probe; a server_agent mapping with spec type `custom` gets both, but only while loaded; spec type `llama_cpp` only System One; settled keys are separate (a settled System One verdict does not stop the OpenAI probe and vice versa); a cleared `decision_openai` row is re-probed; a manual `decision_openai: yes` is not overwritten by a probe `no`.
- [ ] **Step 2: Run** `go test ./internal/provider/ ./cmd/gateway/ -run 'Decisions|SystemOne'` — FAIL.
- [ ] **Step 3: Implement** by generalizing the Phase 1 pass (one candidate loop, two probe kinds; keep `systemOneProbeKey` and add `decisionsProbeKey`), keeping the cognitive-complexity of each function ≤ 15 (Sonar go:S3776).
- [ ] **Step 4: Run** the packages, plus `-race` on `./cmd/gateway/ -run 'Decisions|SystemOne'` — PASS.
- [ ] **Step 5: Commit** `feat(gateway): probe whether a decision model answers OpenAI Decisions natively (#373)`

### Task 6: `/v1/decisions` handler, buffered decisions path, System One mode and normalization

**Files:**
- Create: `internal/gateway/decisions_handler.go`, `internal/gateway/decisions_handler_test.go`, `internal/gateway/decisions_response.go` (buffered path), `internal/gateway/decisions_response_test.go`
- Modify: `internal/gateway/systemone_handler.go` (mode check, normalization opt-in), `internal/gateway/native_passthrough.go` (`nativeRelay.decisionsRewrite`, branch in `proxyNative`, `upstreamPath` for `openai_decisions`, `mergePassthroughUsage` case, `responsesUsage` + `cache_write_tokens`), `internal/gateway/session_extract.go` (`endpointDecisions`), `internal/gateway/server.go` (routes `/v1/decisions`, `/openai/v1/decisions`), any flavor switch that must know `openai_decisions` (`billingUnitFor`, usage labels — find with `git grep -n apiFlavorSystemOne`)
- Test: also `cmd/gateway/native_passthrough_test.go` if it pins `upstreamPath`

**Interfaces:**
- Consumes: Tasks 1–4 (`routing.EffectiveDecisionsRoute`, `auth.Token.DecisionsConfidenceNormalize`, compat functions).
- Produces: `handleOpenAIDecisions`, `validateDecisionsRequest`, `decisionsInferenceShape(model)` (pinned like `systemOneInferenceShape`), codes per spec §9, `relayDecisionsResponse`.

- [ ] **Step 1: Failing tests** (httptest upstream recording path + body):
  - validator: model/questions/stream codes; `questions: {}` (object) → questions_required; `stream: null` accepted.
  - auth: session cookie and loopback refused; `/openai/v1/decisions` alias works.
  - `decisionsInferenceShape` pins flavor `openai_decisions`, endpoint, `[any_decision]`.
  - auto + `decision_openai: yes` → upstream `/v1/decisions`, body value-equal with model rewritten, response bytes identical, usage row `cached_tokens` from `input_tokens_details`, `provider_path=/v1/decisions`.
  - auto + only `decision: yes` → upstream `/v1/systemone` with the translated body; client gets the OpenAI shape; usage row input/output tokens; `provider_path=/v1/systemone`.
  - forced `translate` without `decision` and forced `passthrough` without `decision_openai` → 404 `routing.model_not_capable`; `disabled` (app, and server_agent spec overriding an `auto` app) → 404 `decisions.endpoint_disabled`.
  - translator refusal (https image) → 400 `decisions.input_unsupported`, no upstream call, error usage row.
  - translate upstream 501 / 400 with `{error:{message}}` / 422 `{detail:[…]}` → envelope `decisions.upstream_error` with the upstream status and message; passthrough upstream 400 → verbatim.
  - translate upstream answer missing a question → 502 `decisions.upstream_error`; oversize upstream body → 502.
  - normalization: token on → confidence rewritten on translate, on passthrough, and on `/v1/systemone`; token off → `/v1/systemone` body byte-identical to upstream and served through the streaming copier (assert no Content-Length set by the buffered path, or a spy on the branch).
  - `/v1/systemone` with a server_agent spec `systemone_mode: disabled` → 404 `systemone.endpoint_disabled`.
  - body cap 413 on `/v1/decisions`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run 'Decisions|SystemOne'` — FAIL.
- [ ] **Step 3: Implement** per spec §4, §6, §7.
- [ ] **Step 4: Run** `go test ./internal/gateway/ ./cmd/gateway/` (skip the known local voice families) and `-race` on the new tests — PASS; golangci-lint clean.
- [ ] **Step 5: Commit** `feat(gateway): POST /v1/decisions with translate and passthrough, System One mode and confidence normalization (#373)`

### Task 7: Portal frontend

**Files:**
- Modify: `gateway/frontend/src/api/models.ts`, `api/runtime.ts`, `api/tokens.ts`, `api/services.ts`, `components/shared/ApiVariantControls.tsx`, `components/ApplicationSection.tsx`, `components/RuntimeAdminSection.tsx`, `components/shared/applicationTypeDefaults.ts`, `shared/runtimeSpecTypeFlavors.ts`, `shared/runtimeSpecTemplate.ts`, the mapping verdict form (Phase 1's decision select in `MappingForm`), `ModelServersSection` chip, `TokenList.tsx`, `ServiceTokensSection.tsx`, `i18n.ts`
- Test: the matching `*.test.tsx`/`*.test.ts` named in the inventory

**Interfaces:**
- Consumes: Task 2/3 JSON fields (`decisions_mode`, `systemone_mode`, `decision_openai`, `decisions_route`, `decisions_confidence_normalize`).
- Produces: types `DecisionsMode = 'auto' | EndpointMode`, `SystemOneMode = 'passthrough' | 'disabled'`; selects `application-decisions-mode` and `application-systemone-mode` (disabled and showing the default while `decisions` is unticked); the passthrough option absent for `llama_cpp` app/spec types; a `decision_openai` verdict select (reserved `no` shown only when stored, as Phase 1); chip `decision_openai`; the mapping's `decisions_route` shown next to the verdict chips; token checkbox; i18n DE+EN.

- [ ] **Step 1: Failing tests:** default values in create bodies (`auto`/`passthrough`); selects disabled without the flavor; llama_cpp hides passthrough and a stored passthrough is reset to auto on type change; spec template does not inherit modes beyond defaults; verdict select and chip; route label; token checkbox sends `decisions_confidence_normalize` on create and update; i18n keys exist in both languages.
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run <the touched test files>` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `npm test`, `npm run build`, `npm run lint`, `npm run format:check` — PASS.
- [ ] **Step 5: Commit** `feat(portal): decisions and System One modes, the decision_openai verdict and the confidence opt-in (#373)`

### Task 8: Documentation and ADR-098

**Files:**
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (endpoint table; §3.9 mode + normalization; new §3.10 "OpenAI Decisions" with matrix A as shipped; §6 endpoint modes; §13 error codes), `docs/architecture/reference/api-surface.md` (endpoints, mode section, token field, error table), `docs/architecture/reference/data-model.md` (columns, migrations 107/108), `docs/architecture/cross-cutting/routing-and-model-selection.md` (flavor, `decision_openai`, `any_decision`, type defaults), `docs/architecture/reference/openapi.yaml` (`/v1/decisions`, `/openai/v1/decisions`, schemas, token field), `docs/architecture/09-architecture-decisions.md` (ADR-098 per spec §11, with Rejected/Consequence like ADR-097), `docs/architecture/12-glossary.md`, `docs/architecture/cross-cutting/persistence.md`, `security-auth-rbac.md` (token field), `README.md`/`AGENTS.md` where Phase 1 mentioned decisions
- Test: `make lint-docs` (only the #377 macOS line may remain), `sh scripts/check-docs.test.sh` (same caveat)

- [ ] **Step 1:** Write the docs from the code as merged in Tasks 1–7 (read the code, not this plan, for every claimed behavior).
- [ ] **Step 2: Run** `make lint-docs` — only the known #377 line.
- [ ] **Step 3: Commit** `docs(architecture): OpenAI Decisions, decision modes and ADR-098 (#373)`

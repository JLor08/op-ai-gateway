# Decisions Phase 4 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve OpenAI's `gpt-6-luna` through OpenAI api-key vendor accounts on both decision endpoints, and translate System One to OpenAI Decisions for any upstream that answers only OpenAI Decisions.

**Architecture:** A pure reverse translator in `internal/compat`. `systemone_mode` gains `auto` and `translate`, with a post-resolve System One route mirroring the Phase 2 decisions route. The vendor resolver admits `[any_decision]` requests on OpenAI api-key accounts for an allowlisted decision model, builds a target with `decisions` and fixed modes, and answers its verdicts synthetically.

**Tech Stack:** Go (gateway/backend), React + TypeScript + Vitest (gateway/frontend).

**Spec:** `docs/superpowers/specs/2026-11-12-decisions-phase4-design.md` (the authority). Touch points: `docs/superpowers/specs/2026-11-12-decisions-phase4-inventory.md`. Wire facts: `2026-11-12-decisions-wire-schemas.md`, `2026-11-12-openai-decisions-vendor.md`.

## Global Constraints

- Work on branch `feat/decisions-vendor-373-phase4`; never commit to `main`. Commit messages are English, conventional, with a `(#373)` suffix and no attribution lines.
- Keep LAN host names out of repo text. Code and docs never cite `docs/superpowers/`; cite ADR-100 instead.
- No migration and no server-agent change.
- `routing.OpenAIDecisionModels = {"gpt-6-luna"}` applies on OpenAI (`vendor == openai`) api-key accounts only.
- Error codes are exactly the ones in spec §6.
- Go: golangci-lint v2.15.0 must be clean, and each new function's cognitive complexity must be ≤ 15. Frontend: `npm test`, `npm run build`, `npm run lint` and `npm run format:check` must pass.
- Known local-only failures, not to chase: `TestVoice*`, `TestLiveTranscriptionLoopback*`, the load flake `TestVRAMRunUnstableWindowsAreInconclusive`, and the `make lint-docs` agent-extractor line on macOS (#377).

## Review Focus

1. A System One `questions` object with ≥ 12 keys whose names sort differently from their order: the OpenAI `questions[]` must keep object order. Decode with a token stream, never through a Go map.
2. A token without vendor access (§11) asking `/v1/decisions` for `gpt-6-luna` must not reach the vendor. Also check that an Anthropic, OpenAI-compatible preset or Codex subscription account carrying a model named `gpt-6-luna` is never chosen for a decision request.
3. A `chat` request for `gpt-6-luna` on the vendor account: the chat path must be unchanged by the decision eligibility (only `[any_decision]` opts in).
4. An OpenAI answer whose `answers` lack `name` (positional), or carry a `refusal` among valid answers: map them by position, and turn the refusal into 422 `systemone.refused` naming the key.
5. An existing Phase 1 application with stored `systemone_mode: passthrough` and only a `decision: yes` verdict must behave exactly as before. A new application defaults to `auto`.

---

### Task 1: Reverse translator `internal/compat/decisions_reverse.go`

**Files:**
- Create: `gateway/backend/internal/compat/decisions_reverse.go`, `gateway/backend/internal/compat/decisions_reverse_test.go`

**Interfaces:**
- Produces:
  - `func TranslateSystemOneRequest(raw []byte, upstreamModel string) ([]byte, SystemOneAnswerTable, *DecisionsError)`
  - `func TranslateOpenAIDecisionsResponse(body []byte, table SystemOneAnswerTable, clientModel string) ([]byte, DecisionsUsage, error)`
  - `var ErrSystemOneRefused`. The returned error wraps it and names the refused keys, readable via `errors.As` into `*SystemOneRefusedError{Keys []string}`.
  - Codes `systemone.question_type_unsupported` and `systemone.field_unsupported` (constants exported for the handler).

- [ ] **Step 1: Write table tests, one case per spec §4 row.**
  - **Request side:**
    - string state;
    - object state → `input_json`;
    - array state → `input_json`;
    - images (data and https) → `input_image` parts after the state part;
    - 12 keys kept in object order (assert on the raw bytes);
    - noul without criteria → predicate;
    - noul with `{true:"A", false:"B"}` → instructions get both folded sentences;
    - noul with `{true:"A", false:null}` → only the yes sentence;
    - choice criteria → choices in key order, descriptions kept and nulls omitted;
    - score criteria → levels;
    - missing instructions → `""`;
    - each ❌ row → its code: top-level instructions, box/set/span/track/unknown type, assets/audio/videos/roles/options/preset/states/joint/chat_template_kwargs/cache_salt/priority/request_id.
  - **Response side:**
    - named and positional answers;
    - predicate → noul with probabilities and confidence computed;
    - choice → map probabilities;
    - score → legend + probabilities keyed by index string;
    - refusal → `ErrSystemOneRefused` with keys;
    - a missing answer → error;
    - usage → System One usage; `DecisionsUsage` carries cached and cache-write tokens;
    - `model` = clientModel;
    - malformed bodies → error, never a panic.
- [ ] **Step 2: Run** `go test ./internal/compat/ -run 'SystemOneRequest|OpenAIDecisionsResponse'`. Expect FAIL.
- [ ] **Step 3: Implement.** Reuse Phase 2 helpers where they fit (`decodeProbabilities`, `ParseDecisionsUsage`). Keep each function's complexity ≤ 15.
- [ ] **Step 4: Run** `go test ./internal/compat/` and lint. Expect PASS.
- [ ] **Step 5: Commit** `feat(compat): translate System One to OpenAI Decisions (#373)`

### Task 2: System One modes and route (routing + portal)

**Files:**
- Modify: `internal/routing/decisions_route.go` (add `EffectiveSystemOneRoute`), `internal/routing/served_flavors.go` (`MappingServesDecisionEndpoint` for System One uses the route), `internal/routing/store.go` (candidacy still filters only `disabled`), `internal/portal/service_applications.go` (validation, `auto` default for new rows, `translate` guard/conflict/reset, `systemone_route` on the mapping DTO, `decisionFlagsByName` judged by the route), `internal/portal/service_runtime.go` (spec validation and default), `internal/gateway/portal_application_endpoints.go`, `internal/gateway/portal_runtime_endpoints.go` (HTTP mappings).
- Test: routing and portal tests next to the Phase 2 decision-mode tests.

**Interfaces:**
- Produces:
  - `routing.EffectiveSystemOneRoute(kind string, mode routing.EndpointMode, decision, decisionOpenAI bool) (routing.EndpointMode, bool)`, as in spec §3.1;
  - DTO field `systemone_route`;
  - sentinels for the three new error codes.

- [ ] **Step 1: Write failing tests.**
  - The route table, every row in spec §3.1, including passthrough-incapable kinds.
  - New app and spec rows default to `auto`; a stored `passthrough` is kept on update.
  - `translate` refused for `llama_cpp`/`vllm_sr` with 400, and 409 on a kept stored type; a retype resets to `auto`.
  - Listing:
    - a mapping with only `decision_openai: yes` under `auto` is listed for System One (`ModelDTO.decision` true) on a `vllm` app;
    - the same mapping is not listed on `llama_cpp`;
    - a Phase 1 row (`passthrough`, `decision: yes`) behaves unchanged.
  - The `systemone_route` DTO values.
- [ ] **Step 2: Run** the focused tests. Expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/routing/ ./internal/portal/ ./internal/gateway/ -skip '…'` and lint. Expect PASS.
- [ ] **Step 5: Commit** `feat(routing): systemone_mode auto and translate with a post-resolve System One route (#373)`

### Task 3: `/v1/systemone` translate path (gateway)

**Files:**
- Modify: `internal/gateway/systemone_handler.go` (shape `[any_decision]`, post-resolve route, translate relay), `internal/gateway/decisions_response.go` (generalize `decisionsRewrite` to both directions; error-body wording per endpoint), `internal/gateway/native_passthrough.go` (`upstreamPath`: System One under translate → `/v1/decisions`; usage for the translated response).
- Test: `internal/gateway/systemone_handler_test.go` and the decisions response tests.

**Interfaces:**
- Consumes: Task 1 and Task 2.

- [ ] **Step 1: Write failing tests**, using an httptest upstream.
  - `auto` with only `decision_openai: yes` → upstream `/v1/decisions` receives the translated body; the client gets the System One shape; the usage row has `provider_path` `/v1/decisions` and cached tokens.
  - `auto` with `decision: yes` → the Phase 1 passthrough, byte-identical.
  - Forced `translate` without `decision_openai` → 404 `routing.model_not_capable`.
  - `disabled` → 404 `systemone.endpoint_disabled`.
  - Translator refusal → 400 with no upstream call.
  - Upstream refusal → 422 `systemone.refused`.
  - Upstream 400 `{error:{message}}` → `systemone.upstream_error` with the status and the message.
  - Normalization on → confidence rewritten in the System One output.
  - `TestSystemOneInferenceShape` pins `[any_decision]`.
- [ ] **Step 2: Run.** Expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the focused tests, the gateway package with `-skip`, and lint. Expect PASS.
- [ ] **Step 5: Commit** `feat(gateway): translate /v1/systemone to OpenAI Decisions (#373)`

### Task 4: OpenAI vendor accounts for both decision endpoints

**Files:**
- Modify: `internal/routing/resolver.go` (eligibility; account and model filter for decision requests; target flavors and modes), a small `internal/routing/vendor_decisions.go` (`OpenAIDecisionModels`, `IsVendorDecisionModel(acc, m)`), `internal/gateway/decisions_handler.go` (`decisionVerdicts` synthesized for vendor decision targets, used by both endpoints), `internal/portal/vendor_catalog.go` (seed `gpt-6-luna`), `internal/portal/service_vendor_discovery.go` (keep it through the heuristic), `internal/portal/service_vendor_listing.go` (flavor `decisions` for decision models), `internal/portal/service_model_offering.go` (callable for both decision endpoints), and comments that state the exclusion.
- Test: `internal/routing/resolver_vendor_account_test.go`, `internal/gateway/decisions_handler_test.go`, `systemone_handler_test.go`, portal listing tests. Update `TestSystemOneIsNeverVendorRoutable` to the new rule: only the OpenAI api-key decision model.

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: `routing.OpenAIDecisionModels`, `routing.IsVendorDecisionModel`.

- [ ] **Step 1: Write failing tests.**
  - `/v1/decisions` for `gpt-6-luna` with a token that has §11 access → a passthrough to the vendor httptest upstream at `/v1/decisions`. The body is value-equal with the upstream slug. The usage row has `AccountID`, cached tokens and no cost.
  - `/v1/systemone` for it → translated to `/v1/decisions`.
  - No vendor access → not routed (404 or standard path).
  - The prefix override is respected.
  - An Anthropic, OpenAI-compatible preset or subscription account with a `gpt-6-luna` row → never chosen for a decision request.
  - A chat request for `gpt-6-luna` → unchanged chat behavior.
  - Speech and image capability lists → still never vendor.
  - Listing: `/api/portal/models` shows `gpt-6-luna` with flavor `decisions` for an entitled token; `GET /v1/systemone/models` lists it.
  - The seed catalog contains it.
- [ ] **Step 2: Run.** Expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/routing/ ./internal/portal/ ./cmd/gateway/`, the gateway package with `-skip`, and lint. Expect PASS.
- [ ] **Step 5: Commit** `feat(routing): route decision requests to OpenAI api-key vendor accounts (#373)`

### Task 5: Portal frontend

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (`SystemOneMode` adds `auto` and `translate`; mapping `systemone_route`), `components/shared/ApiVariantControls.tsx` (`systemOneModeOptions`), `components/shared/decisionsModes.ts` (default `auto`; `translate` not offered for `llama_cpp`/`vllm_sr`; a retype resets to `auto`), `components/shared/applicationTypeDefaults.ts` / `runtimeSpecTypeFlavors.ts` (the vllm_sr default stays `passthrough`; other types default to `auto`), `MappingForm.tsx` (show `systemone_route` beside `decisions_route`), `utils/format.ts` (new error codes), `i18n.ts` (DE+EN: option labels, the updated `applicationDecisionsModeNote`, route label, errors, and a note that `gpt-6-luna` on an OpenAI api-key account serves both decision endpoints).
- Test: the matching component and unit tests.

- [ ] **Step 1: Write failing tests:** the options; the defaults (`auto` for new apps and specs, `passthrough` for vllm_sr); `translate` hidden for llama_cpp/vllm_sr; the retype reset; route display; error messages; i18n keys.
- [ ] **Step 2: Run.** Expect FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** all four frontend gates. Expect PASS.
- [ ] **Step 5: Commit** `feat(portal): systemone_mode auto and translate, the System One route (#373)`

### Task 6: Documentation and ADR-100

**Files:** the docs listed in the inventory section "Docs stating the vendor exclusion / systemone_mode values":
- ADR-100, with "Amended by" notes on ADR-097/098 and wherever the exclusion was stated.
- compat §3.9/§3.10 (matrix B as shipped; the route table for System One), §6, §13.
- external-vendor-accounts §4/§4.2/§6 (Decisions on api-key accounts; the decision-model allowlist; listing).
- routing §2.3, api-surface (modes, `systemone_route`, error codes), glossary.
- Fix the stale `resolveSystemOneTarget` name in compat §3.9.

- [ ] **Step 1:** Write every claim from the code as merged in Tasks 1–5. Include the "not verified live" list from spec §7.
- [ ] **Step 2: Run** `make lint-docs`. Only the #377 line and the branch-local plan reference may remain.
- [ ] **Step 3: Commit** `docs(architecture): OpenAI vendor decisions, System One translation and ADR-100 (#373)`

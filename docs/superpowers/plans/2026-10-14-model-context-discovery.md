# Model context-size discovery (opencode) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a current opencode (and standard LM-Studio clients) discover each gateway model's context window — serve `GET /api/v1/models` (LM Studio v2 shape), add `context_length` to `/v1/models`, remove the dead `/api/v0/models` listing, and correct the four claims that opencode reads v0.

**Architecture:** All model listings are built from the same `openAIModelDTOs(Portal.Models(token).Data)` rows (`ModelDTO` already carries `ContextSize`, `Vision`, `Loaded`). `/v1/models` switches from the ids-only `ModelsForFlavor` to that richer source to gain `context_length`; a new handler serves the LM Studio v2 `{"models":[…]}` shape at `/api/v1/models`; the v0 handler/helper/route are deleted. No `ModelDTO` change (the tools verdict is a constant `true`).

**Tech Stack:** Go (`internal/gateway`, `internal/portal`), a bash config-generator script, the architecture docs + `openapi.yaml`.

**Spec:** docs/superpowers/specs/2026-10-14-model-context-discovery-design.md

## Global Constraints

- Repo-facing text (code, tests, docs, commits, ADR) in English.
- `docs/superpowers/**` is branch-local; removed before the PR.
- Decisions: `trained_for_tool_use: true` for every chat model (constant, no DTO change); `/api/v0/models` is REMOVED, not deprecated; no portal UI change; the Ollama tools-drop is #219 (out of scope); no per-model `max_output_tokens`.
- `/api/v1/models` entries must send `max_context_length` ALWAYS (even `0`); `/v1/models` entries OMIT `context_length` when `0`.
- `models` and every `loaded_instances` array must encode as `[]`, never `null` (build with `make([]T, 0, n)`).
- Auth for both `/v1/models` and `/api/v1/models`: `requireAnyScope(scopeGatewayUse, scopeLLMInvoke)` (bearer-only; a service token must reach them).
- Pre-PR gates (mirror CI): both Go modules `golangci-lint fmt --diff` + `run` + `go test` with the Postgres DSN (`OP_AI_GATEWAY_TEST_POSTGRES_DSN`); frontend `format:check`+`lint`+`build`+`test` (no frontend change expected, run anyway); `make lint-docs`; local Sonar branch-findings (judge by new-code/line-attributed findings). No local host names.

## Review Focus

- **opencode's strict decode fails the WHOLE response on a `null` array or a missing required key.** `/api/v1/models` must emit `models` and every `loaded_instances` as `[]` (not `null`) and include `type`, `key`, `display_name`, `max_context_length`, `loaded_instances` on every entry. Tested by asserting on the raw JSON bytes (a `map`/struct that marshals a nil slice to `null` must be caught).
- **`context_length`/`max_context_length` at `0`.** `/v1/models` must OMIT `context_length` at `0` (opencode discovery PRs read `context_length ?? fallback`; a literal `0` blocks the fallback); `/api/v1/models` must INCLUDE `max_context_length: 0`. Both the omit case and the include-at-0 case get a test.
- **Service-token discovery.** `/api/v1/models` (and the unchanged `/v1/models`) must answer a service token (sole `llm:invoke`) 200, not 403. A session cookie alone must 401. Tested.
- **`/v1/models` id-equivalence after the data-source change.** Switching `handleOpenAIModels` from `ModelsForFlavor` to `openAIModelDTOs(Portal.Models())` must keep the exact same id set. Keep/extend the equivalence test.
- **No dangling `/api/v0/models`.** Route, `handleLMStudioModels`, `lmStudioModelsFromDTOs`, its tests, and every doc/comment claim must be gone; the path now 404s.

---

## Task 1: `/v1/models` carries `context_length`

**Files:**
- Modify: `gateway/backend/internal/gateway/server.go` (`handleOpenAIModels`, ~1510)
- Test: `gateway/backend/internal/gateway/` (the existing models-listing test file; e.g. `models_test.go` / `lmstudio_models_test.go` sibling — add an openai-models context test)

**Interfaces:**
- Consumes: `openAIModelDTOs(s.Portal.Models(ctx, token).Data) []portal.ModelDTO` (exists, `server.go:1568`); `portal.ModelDTO.{ID,ContextSize}`.
- Produces: `/v1/models` + `/openai/v1/models` entries now `{id, object:"model", owned_by:"op-ai-gateway", context_length?:int}` (`context_length` omitted at 0).

- [ ] **Step 1 — failing test:** a listing over two stub models, one with `ContextSize: 8192` and one with `ContextSize: 0`, asserts the first entry's JSON has `"context_length":8192` and the SECOND entry has **no** `context_length` key; and that the id set equals `ModelsForFlavor(token, openai)` (keep the existing equivalence assertion). Assert on decoded JSON for the ids and on the raw body (or a `map[string]json.RawMessage`) for the omit case.
- [ ] **Step 2:** run it; it fails (no `context_length` today; and the handler currently sources `ModelsForFlavor`).
- [ ] **Step 3 — implement:** change `handleOpenAIModels` to build `data` from `openAIModelDTOs(s.Portal.Models(r.Context(), token).Data)`; per row emit `map[string]any{"id": m.ID, "object": "model", "owned_by": "op-ai-gateway"}` and, only when `m.ContextSize > 0`, set `["context_length"] = m.ContextSize`. Keep the `{object:"list", data}` envelope and the `requireAnyScope(scopeGatewayUse, scopeLLMInvoke)` auth unchanged.
- [ ] **Step 4:** run the test (pass) + the existing models/auth tests (`go test ./internal/gateway -run Models`).
- [ ] **Step 5:** commit `feat(gateway): expose context_length on /v1/models`.

## Task 2: replace `/api/v0/models` with `/api/v1/models` (LM Studio v2)

**Files:**
- Modify: `gateway/backend/internal/gateway/server.go` (remove the `/api/v0/models` route ~1297, `handleLMStudioModels` ~1578, `lmStudioModelsFromDTOs` ~1536; add the `/api/v1/models` + `/openai/api/v1/models` routes and a new handler + helper; fix the `openAIModelDTOs` doc comment to stop referencing `/api/v0/models`)
- Modify/replace: `gateway/backend/internal/gateway/lmstudio_models_test.go` (drop the v0 tests; add the v1 + 404 tests)

**Interfaces:**
- Consumes: `openAIModelDTOs(Portal.Models(token).Data)`; `ModelDTO.{ID,DisplayName,ContextSize,Vision,Loaded}`; `requireAnyScope`, `scopeGatewayUse`, `scopeLLMInvoke`.
- Produces: `GET /api/v1/models` + `/openai/api/v1/models` → `{"models":[ {type:"llm", key, display_name, max_context_length, loaded_instances, capabilities:{vision, trained_for_tool_use}} ]}`.

- [ ] **Step 1 — failing tests:**
  - A v1-shape test over stub models (one loaded with `ContextSize: 8192, Vision: true`, one not-loaded with `ContextSize: 0, Vision: false`) asserts: top-level key is `models`; each entry has `type:"llm"`, `key == ID`, `display_name == DisplayName`, `max_context_length` PRESENT on both (incl. the `0` one), `capabilities.trained_for_tool_use == true`, `capabilities.vision` matching the fold; the loaded entry's `loaded_instances == [{config:{context_length:8192}}]`, the not-loaded entry's `loaded_instances == []`.
  - A raw-bytes test: with ZERO models the body contains `"models":[]` (not `"models":null`); a not-loaded model's body contains `"loaded_instances":[]` (not `null`).
  - An auth test: a `gateway:use` bearer → 200; a service token (sole `llm:invoke`) → 200; a session cookie alone → 401. Both `/api/v1/models` and `/openai/api/v1/models` route to the handler.
  - A removal test: `GET /api/v0/models` with a valid bearer → 404 `request.not_found`.
- [ ] **Step 2:** run them; they fail (no v1 handler; v0 still 200s).
- [ ] **Step 3 — implement:**
  - Delete the `/api/v0/models` route line, `handleLMStudioModels`, and `lmStudioModelsFromDTOs`.
  - Add `lmStudioV1ModelsFromDTOs(models []portal.ModelDTO) []map[string]any`: `out := make([]map[string]any, 0, len(models))`; per `m`, build `loaded := make([]map[string]any, 0, 1)` and, when `m.Loaded`, append `{"config": {"context_length": m.ContextSize}}`; append `{"type":"llm", "key":m.ID, "display_name":m.DisplayName, "max_context_length":m.ContextSize, "loaded_instances":loaded, "capabilities": map[string]any{"vision": m.Vision, "trained_for_tool_use": true}}`.
  - Add `handleLMStudioV1Models`: `requireMethod GET`; `token := requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)`; `rows := make([]map[string]any, 0, …)`; `writeJSON(w, 200, map[string]any{"models": lmStudioV1ModelsFromDTOs(openAIModelDTOs(s.Portal.Models(r.Context(), token).Data))})` — and ensure the top-level `models` is a non-nil slice (the helper returns one).
  - Register `s.mux.HandleFunc("/api/v1/models", s.handleLMStudioV1Models)` and `.../openai/api/v1/models` next to the model routes.
  - Fix the `openAIModelDTOs` comment (and any other `server.go` comment) that references `/api/v0/models` or opencode auto-detection.
- [ ] **Step 4:** `go test ./internal/gateway` (incl. `-run LMStudio|Models|Auth`), scoped `-race` on the new tests, `golangci-lint fmt --diff` + `run`.
- [ ] **Step 5:** commit `feat(gateway): serve /api/v1/models (LM Studio v2) and drop /api/v0/models`.

## Task 3: port `gen-opencode-config.sh` to `/v1/models`

**Files:**
- Modify: `scripts/gen-opencode-config.sh`
- Test: its existing shape/jq test if present (`scripts/`), else add a minimal one.

**Interfaces:**
- Consumes: `GET <base>/v1/models` with a bearer, now carrying `context_length`.
- Produces: the same v1 `opencode.json` (`limit.context`, `limit.output`).

- [ ] **Step 1 — failing test:** feed a sample `/v1/models` body (`{object:"list", data:[{id:"m1", context_length:8192}, {id:"m2"}]}`) through the script's jq transform; assert `m1` → `limit.context=8192`, `limit.output=min(floor(8192/4),8192)=2048`, and `m2` (no `context_length`) is SKIPPED.
- [ ] **Step 2:** run/fail (script reads `/api/v0/models` and `max_context_length` today).
- [ ] **Step 3 — implement:** point the fetch at `<base>/v1/models`; read `.data[]`, use `.context_length` (skip entries without it); keep `limit.context = context_length`, `limit.output = min(floor(context/4), 8192)`, and `options.apiKey` = the token (optionally support `{env:OP_GATEWAY_TOKEN}`). Remove the `/api/v0/models` and `max_context_length` references.
- [ ] **Step 4:** run the script test.
- [ ] **Step 5:** commit `chore(scripts): gen-opencode-config reads /v1/models context_length`.

## Task 4: correct the claims + ADR

**Files:**
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md`, `docs/architecture/reference/api-surface.md`, `docs/architecture/reference/openapi.yaml`, `docs/architecture/09-architecture-decisions.md` (ADR-045), `docs/architecture/11-risks-and-technical-debt.md` (the v0-emulation entry). Any remaining `server.go`/`service.go` comment that claims opencode reads `/api/v0/models`.

- [ ] **Step 1:** amend **ADR-045** (the LM-Studio-v0 emulation decision): record that v0 is retired, `/api/v1/models` serves the LM Studio v2 shape, `/v1/models` gains `context_length`, and the `trained_for_tool_use: true` policy; keep ADR-045's number and mark the old decision amended inline (lighter than a superseding ADR).
- [ ] **Step 2:** sweep the docs:
  - `compatibility-and-inference.md`: replace the "auto-detects … via the LM-Studio-shaped `GET /api/v0/models`" passage with `/api/v1/models` + `context_length` on `/v1/models`.
  - `api-surface.md`: route table — drop `/api/v0/models`, add `/api/v1/models` + `/openai/api/v1/models`, add `context_length` to the `/v1/models` row; drop the "LM-Studio-aware clients such as opencode" claim; fix the wrong auth cells ("Session-or-bearer"/v0 → bearer-only).
  - `openapi.yaml`: drop the v0 path; add the v1 path(s) and the `/v1/models` `context_length` field; remove `sessionCookie` from the model listings.
  - `11-risks`: update the v0-emulation entry.
  - Remove any remaining code comment claiming opencode reads v0.
- [ ] **Step 3:** `make lint-docs` (links/anchors/index/openapi green; only the branch-local plan/spec files are the expected "forbidden branch-local doc" exemption until they are removed before the PR).
- [ ] **Step 4:** commit `docs: /api/v1/models + context_length, retire /api/v0/models (ADR-045)`.

---

## Self-Review

- **Spec coverage:** A = Task 2; B = Task 1; C-remove = Task 2; C-script = Task 3; C-docs+ADR = Task 4. Every spec part maps to a task. ✓
- **Type consistency:** `openAIModelDTOs(Portal.Models(token).Data)` is the one source for Tasks 1+2; `ModelDTO.{ID,DisplayName,ContextSize,Vision,Loaded}` are the fields read; `requireAnyScope(scopeGatewayUse, scopeLLMInvoke)` is the one auth for both listings; `lmStudioV1ModelsFromDTOs` replaces `lmStudioModelsFromDTOs`. ✓
- **Review Focus:** the `[]`-not-`null` decode trap (Task 2 raw-bytes test), the `0`-context omit/include split (Tasks 1+2), service-token discovery (Tasks 1+2 auth tests), the `/v1/models` id-equivalence after the source change (Task 1), and the no-dangling-v0 removal (Task 2) each have an owning task + test. ✓
- **Placeholder scan:** shapes, the field map, the auth, the `make([]…,0,n)` discipline, and the test inputs/expected values are concrete. ✓
- **Ordering:** 1 (`/v1/models` context_length) → 2 (v0→v1 migration) → 3 (script, needs Task 1's field) → 4 (docs, describe the final state).

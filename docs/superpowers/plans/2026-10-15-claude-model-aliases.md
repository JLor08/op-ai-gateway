# Claude-compatible model aliases — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an opt-in per-token setting (`claude_model_aliases`) that auto-exposes eligible local models/groups as stable `anthropic-local-<hash>` aliases in discovery and inference, with `original_model_id` + honest `display_name` discovery metadata.

**Architecture:** Reuse the existing alias machinery. One shared derivation function computes `aliasID → {originalGatewayID, displayName}` from the token's currently-offered, Anthropic-Messages-eligible real models/groups; discovery overlays those rows (usage path only) and inference resolves them once to the real target before auth/routing. No generated rows are persisted. Metadata rides as top-level siblings of `capabilities` on alias rows only.

**Tech Stack:** Go 1.26 backend (`gateway/backend`, module `op-ai-gateway`); React/TS/Vite portal (`gateway/frontend`); stores memory/sqlite/postgres via the `dialect` seam; append-only versioned migrations.

**Spec:** `docs/superpowers/specs/2026-10-15-claude-model-aliases-design.md` (read it alongside this plan — the plan argues from it).

## Global Constraints

- TDD for every behavior change: write the failing test, watch it fail for the right reason, then implement. Repo-facing text (comments, tests, docs, commit messages) in English.
- Keep all three stores working (memory/sqlite/postgres); schema changes are **append-only versioned migrations**; verify store changes with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set. A boolean column is `integer not null default 0` on both dialects (ADR-005 wide-column rule does not apply to bools).
- Preserve stable error codes (`ErrInvalidResponse`/routing codes/`auth.*`) — clients depend on them.
- Alias IDs are **lowercase hexadecimal** throughout: `anthropic-local-` + 12 hex of SHA-256(UTF-8 gateway ID), with a **sliding-window** hex fallback when a window contains a denylisted family substring. Never switch to decimal.
- Metadata is `original_model_id` + honest `display_name` as **top-level siblings of `capabilities`** (not nested, no `op_ai_gw` namespace), on generated alias rows only.
- Hash the **gateway model/group ID** (`GatewayModelName` / group name), never the upstream `AppModelName`, display label, position, loaded state, or member.
- **No alias-of-alias**: derive only from alias-free real-model views; resolve in a single lookup.
- The flag defaults **off**; off ⇒ zero behavior change. Service tokens get the flag on **create + read** only (no field-PATCH exists for them).
- Portal i18n: add German + English keys together in `i18n.ts` (compile-time parity via `en: PortalMessages`); add an `i18n.test.ts` spec for the new keys.
- No ServerAgent version bump (this change does not touch `server-agent/`).
- Keep the admin `ManageModels` view showing real models — never generated aliases.

## Review Focus

Inputs the spec implies but that a naive task could leave unexercised (each gets a test in the owning task):

1. **Gateway ID whose first 12 hex contains a denylisted substring** (e.g. forced `abab`) must slide to a safe hex window, deterministically and stably — Task 4.
2. **Two distinct gateway IDs hashing to the same alias** must skip **both** auto aliases with a diagnostic, never route one alias to two targets — Task 4.
3. **An auto alias colliding with an existing manual `model_override_map` key or a real model/group name** → manual/real wins, auto alias skipped, no auto metadata — Task 4 + Task 5.
4. **Disabling the flag (or a model going away) mid-life**: a previously-served alias must stop resolving and disappear from discovery — it must not silently fall back to another model — Task 7 + Task 5.
5. **Hidden/locked/images-only/Messages-disabled models and hidden group members** must never receive an alias nor be revealed through an alias row or its metadata — Task 5.

---

## File Structure & Shared Interfaces

Later tasks depend on these exact names. Define them in the earliest owning task; reference verbatim elsewhere.

**New files**
- `gateway/backend/internal/portal/claude_aliases.go` — derivation (Task 4)
- `gateway/backend/internal/portal/claude_aliases_test.go` — derivation tests (Task 4)

**Shared Go identifiers**
- `store.TokenRecord.ClaudeModelAliases bool` (`internal/store/models.go`); `auth.Token.ClaudeModelAliases bool` (`internal/auth/token_store.go`).
- DTO JSON key `claude_model_aliases`: `TokenDTO.ClaudeModelAliases bool json:"claude_model_aliases"` (and `ServiceTokenDTO`); `CreateTokenRequest.ClaudeModelAliases bool`; `UpdateTokenRequest.ClaudeModelAliases *bool`.
- `portal.ModelDTO.OriginalModelID string json:"original_model_id,omitempty"` (`internal/portal/service.go`).
- Derivation (Task 4, package `portal`):
  - `type claudeAlias struct { OriginalID string; DisplayName string }`
  - `func claudeAliasID(gatewayID string) (id string, ok bool)` — returns `"anthropic-local-"+hash`, `ok=false` when no filter-safe hex window exists.
  - `var claudeBlockedSubstrings = []string{…}` — documented denylist constant (lowercased; checked against the whole generated id).
  - `func (s *Service) claudeAliasMap(real map[string]flavorSet, displayName func(string) string) (map[string]claudeAlias, []string)` — returns `aliasID → claudeAlias` plus a slice of skipped diagnostics; `real` is the alias-free pre-suppression eligible set, keyed by gateway model/group id.
- Frontend: `claude_model_aliases?: boolean` on `PortalToken`, `CreateTokenRequest`, `UpdateTokenRequest` (`src/api/tokens.ts`); i18n keys `tokenClaudeAliases` (+ helper text) in `de`/`en`.

---

## Phase A — Token setting (`claude_model_aliases`)

### Task 1: Persist the flag across all three stores

**Files:**
- Modify: `internal/store/models.go` (`TokenRecord`), `internal/auth/token_store.go` (`Token` + memory LookupBearer), `internal/store/sqlite_token.go` (insert/columns/update/scan/LookupBearer), `internal/portal/memory_directory.go` (create/update/mirror), `internal/store/migrate.go` (register v88 + `migration88Up`)
- Test: `internal/store/sqlite_token_test.go` (or the existing token store test file), `internal/store/conformance_test.go` / `internal/portal/store_conformance_test.go`, `internal/store/schema_column_coverage_test.go` fixture

**Interfaces:**
- Produces: `store.TokenRecord.ClaudeModelAliases bool`, `auth.Token.ClaudeModelAliases bool`; migration version **88**; column `api_tokens.claude_model_aliases integer not null default 0`.

- [ ] **Step 1 — Failing conformance test.** In the token store conformance test, create a token with `ClaudeModelAliases: true`, read it back across memory + sqlite (+ postgres when `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set), assert it round-trips; create one without the field and assert it reads `false`.

```go
rec := store.TokenRecord{ID: "tok_cma", /* …required fields… */ ClaudeModelAliases: true}
// CreatePlainToken(rec); got := TokenByID("tok_cma")
if !got.ClaudeModelAliases { t.Fatalf("claude_model_aliases did not round-trip: %+v", got) }
```

- [ ] **Step 2 — Run, verify it fails** (`go test ./internal/store/ -run Conformance` — field unknown / always false). Provision Postgres like CI.
- [ ] **Step 3 — Add the field + migration + store plumbing.** Add `ClaudeModelAliases bool` to `TokenRecord` and `auth.Token`; register `{version: 88, name: "token_claude_model_aliases", up: migration88Up}` after v87, with `migration88Up` calling `addColumnIfMissing(ctx, tx, dl, "api_tokens", "claude_model_aliases integer not null default 0")` (model on `migration63Up`); add the column to `sqlite_token.go` insert list, `tokenColumns`, `UpdateTokenMetadata` SET, `scanToken` (`int64 != 0`), and the `LookupBearer`→`auth.Token` assembly; mirror in `memory_directory.go` (record store + `auth.Token`).
- [ ] **Step 4 — Run, verify pass** (`go test ./internal/store/ ./internal/portal/ -run 'Conformance|Token'`).
- [ ] **Step 5 — Satisfy the column-coverage guard.** Seed `claude_model_aliases` TRUE in the parity fixture `schema_column_coverage_test.go` expects; run `go test ./internal/store/ -run ColumnCoverage` to green.
- [ ] **Step 6 — Commit** (`feat(store): persist per-token claude_model_aliases flag (#225)`).

### Task 2: Token API — user (create/update/read) + service (create/read)

**Files:**
- Modify: `internal/portal/service.go` (`TokenDTO`, `CreateTokenRequest`, `UpdateTokenRequest`, `CreateToken`, `UpdateToken`, `tokenDTO`), `internal/portal/service_services.go` (`ServiceTokenDTO`, create path, `serviceTokenDTO`)
- Test: `internal/portal/service_test.go` (or the token-focused test file)

**Interfaces:**
- Consumes: `store.TokenRecord.ClaudeModelAliases` (Task 1).
- Produces: DTO field `claude_model_aliases` on `TokenDTO`/`ServiceTokenDTO`/`CreateTokenRequest` (bool) and `UpdateTokenRequest` (`*bool`).

- [ ] **Step 1 — Failing test (create + read).** Create a user token with `claude_model_aliases: true`; assert `TokenDTO.ClaudeModelAliases == true`. Create one omitting it; assert `false`.
- [ ] **Step 2 — Failing test (PATCH-preserve).** Update a token with `ClaudeModelAliases == nil` (omitted) and assert the stored value is unchanged; update with `*true`/`*false` and assert it changes.

```go
_, err := svc.UpdateToken(ctx, ownerToken(), id, UpdateTokenRequest{ /* ClaudeModelAliases nil */ LogCommunication: ptr(true)})
// reload → ClaudeModelAliases unchanged from its pre-update value
```

- [ ] **Step 3 — Failing test (service token create + read).** Create a service token with the flag true; assert `ServiceTokenDTO.ClaudeModelAliases == true`.
- [ ] **Step 4 — Run, verify all three fail** (`go test ./internal/portal/ -run Token`).
- [ ] **Step 5 — Implement.** Add the DTO fields; set the record field in `CreateToken`/service create; add the `if req.ClaudeModelAliases != nil { record.ClaudeModelAliases = *req.ClaudeModelAliases }` guard in `UpdateToken`; map it back in `tokenDTO`/`serviceTokenDTO`. No validator needed.
- [ ] **Step 6 — Run, verify pass.**
- [ ] **Step 7 — Commit** (`feat(portal): expose claude_model_aliases on token create/update/read (#225)`).

### Task 3: Portal UI + i18n

**Files:**
- Modify: `gateway/frontend/src/api/tokens.ts` (`PortalToken`, `CreateTokenRequest`, `UpdateTokenRequest`), `gateway/frontend/src/components/TokenList.tsx` (checkbox state/reset/hydrate/submit, both forms), service-token create form, `gateway/frontend/src/i18n.ts` (`de` + `en` keys)
- Test: `gateway/frontend/src/i18n.test.ts`

**Interfaces:**
- Consumes: the `claude_model_aliases` JSON field (Task 2).

- [ ] **Step 1 — Failing i18n test.** Add a spec asserting `tokenClaudeAliases` (+ helper text key) is non-empty in both `de` and `en`.
- [ ] **Step 2 — Run, verify it fails** (`cd gateway/frontend && npm test -- i18n`).
- [ ] **Step 3 — Add keys + types + UI.** Add DE/EN keys (`de` canonical; `en: PortalMessages` keeps parity at compile time); add `claude_model_aliases?: boolean` to the three TS types; add a `<Checkbox>`/`<FormControlLabel>` in `TokenList.tsx` following the `unknownRedirect` pattern (state `:164`, reset `:266`, hydrate `:299`, render `:670`, submit `:373`/`:405`) and in the service-token create form; label "Claude Desktop model aliases".
- [ ] **Step 4 — Run, verify pass + build** (`npm test` and `npm run build` for type-checked parity; `npm run format:check`).
- [ ] **Step 5 — Commit** (`feat(portal-ui): Claude Desktop model aliases token toggle + i18n (#225)`).

---

## Phase B — Alias derivation

### Task 4: Shared derivation (hash, fallback, denylist, eligibility, collisions)

**Files:**
- Create: `internal/portal/claude_aliases.go`, `internal/portal/claude_aliases_test.go`

**Interfaces:**
- Produces: `claudeAliasID`, `claudeBlockedSubstrings`, `claudeAlias`, `(*Service).claudeAliasMap` (signatures under Shared Interfaces).

- [ ] **Step 1 — Failing test: canonical hash.** `claudeAliasID("qwen3.8-fast-loaded")` returns `"anthropic-local-556f772a70b1", true`.

```go
id, ok := claudeAliasID("qwen3.8-fast-loaded")
if !ok || id != "anthropic-local-556f772a70b1" { t.Fatalf("got %q %v", id, ok) }
```

- [ ] **Step 2 — Failing test: hex sliding-window fallback.** With a denylist containing `"abab"`, a gateway ID whose first 12 hex window contains `abab` yields an id whose hash window is the first later 12-hex window with no denylisted substring; the result is still all-hex and is **stable** across repeated calls. (Pick the fixture ID by computing SHA-256 offline, or assert the invariants: `strings.HasPrefix(id,"anthropic-local-")`, the 12 hash chars are `[0-9a-f]`, contain no denylisted substring, and two calls agree.)
- [ ] **Step 3 — Failing test: eligibility + collisions.** `claudeAliasMap` over a fixture set: (a) an eligible local model gets `aliasID → {OriginalID, DisplayName}`; (b) a genuine Anthropic / vendor / images-only / Messages-disabled / hidden entry gets **no** alias; (c) two gateway IDs forced to the same hash → **both** skipped, each in the diagnostics slice; (d) an alias id equal to an existing real model name or manual-rule key → skipped with a diagnostic.
- [ ] **Step 4 — Run, verify all fail.**
- [ ] **Step 5 — Implement.** `claudeAliasID`: `sha256.Sum256([]byte(gatewayID))`, hex-encode, scan 12-char windows from offset 0; return the first window with no `claudeBlockedSubstrings` entry (checked against `"anthropic-local-"+window`); `ok=false` if none. `claudeAliasMap`: iterate the alias-free eligible `real` set, compute ids, drop ineligible, detect id↔id and id↔real-name/manual-key collisions deterministically (skip + append diagnostic), return the map + diagnostics. Honest `DisplayName` via the `displayName` callback (target label if present, else the gateway id).
- [ ] **Step 6 — Run, verify pass; `gofmt`/`go vet`/`golangci-lint`.**
- [ ] **Step 7 — Commit** (`feat(portal): shared Claude alias derivation (hash + eligibility + collisions) (#225)`).

---

## Phase C — Discovery

### Task 5: Generated-alias discovery overlay + metadata DTO field

**Files:**
- Modify: `internal/portal/service.go` (`ModelDTO.OriginalModelID`; wire `claudeAliasMap` into `modelsResponse` usage path and `flavorSetsFromViews`), `internal/portal/service_model_offering.go` (overlay next to `applyOverrideAliases`)
- Test: `internal/portal/service_model_offering_test.go` / `service_test.go`

**Interfaces:**
- Consumes: `claudeAliasMap` (Task 4), `auth.Token.ClaudeModelAliases` (Task 1).
- Produces: `ModelDTO.OriginalModelID`; generated alias rows on the usage path.

- [ ] **Step 1 — Failing test: alias row appears, flag-gated.** With the flag on, an eligible local model yields an extra `ModelDTO{ID: "anthropic-local-…", OriginalModelID: "<gatewayID>", DisplayName: <honest>}` inheriting the target's flavors/loaded/context/vision/image/group payload; with the flag off, no such row. The original model row still appears (offer in addition to).
- [ ] **Step 2 — Failing test: exclusions + admin.** Hidden/locked/images-only/Messages-disabled/hidden-group-member → no alias row and no leak; `ManageModels()` (admin) shows real models only, never aliases; a manual rule for the same id wins (no duplicate row).
- [ ] **Step 3 — Run, verify fail.**
- [ ] **Step 4 — Implement.** Add `OriginalModelID` to `ModelDTO`; when `token.ClaudeModelAliases`, compute `claudeAliasMap` from the alias-free `preSuppress` eligible set and overlay rows (like `Offer:true, HideTarget:false`) on the suppressed/usage path in `modelsResponse` and `flavorSetsFromViews` — after group/suppression, respecting manual-alias precedence; copy the target's row payload onto the alias row and set `OriginalModelID` + honest `DisplayName`. Never run on `ManageModels()`.
- [ ] **Step 5 — Run, verify pass.**
- [ ] **Step 6 — Commit** (`feat(portal): generated Claude alias discovery rows (#225)`).

### Task 6: Metadata serialization in the three listings

**Files:**
- Modify: `internal/gateway/server.go` (`handleOpenAIModels` item map, `lmStudioV1ModelsFromDTOs`, `handleAnthropicModels`)
- Test: `internal/gateway/server_test.go` (or the model-endpoint test file)

**Interfaces:**
- Consumes: `ModelDTO.OriginalModelID`, `ModelDTO.DisplayName` (Task 5).

- [ ] **Step 1 — Failing test: `/v1/models` + `/openai/v1/models`.** An alias row serializes `original_model_id` and `display_name` as **top-level siblings of `capabilities`**; a non-alias row has neither; flavor filter still excludes anthropic-only/images-only rows from `/v1/models`.
- [ ] **Step 2 — Failing test: `/api/v1/models` + `/anthropic/v1/models`.** Both carry the alias row's `original_model_id` + honest `display_name` (Anthropic listing keeps its required shape).
- [ ] **Step 3 — Run, verify fail.**
- [ ] **Step 4 — Implement.** In each serializer, when `m.OriginalModelID != ""`, add `item["original_model_id"] = m.OriginalModelID` and `item["display_name"] = m.DisplayName` next to `capabilities`; route the DTO (not just IDs) into `handleAnthropicModels`.
- [ ] **Step 5 — Run, verify pass.**
- [ ] **Step 6 — Commit** (`feat(gateway): expose original_model_id + display_name on alias rows (#225)`).

---

## Phase D — Inference resolution

### Task 7: Resolve generated aliases (precedence, single-resolution, access, disable)

**Files:**
- Modify: `internal/gateway/inference_handlers.go` (`resolveModelOverride` / preflight), a portal accessor `(*Service).ClaudeAliasTarget(ctx, token, requested) (original string, ok bool)` wrapping `claudeAliasMap` so discovery + resolution share it
- Test: `internal/gateway/inference_handlers_test.go` (+ a portal test for the accessor)

**Interfaces:**
- Consumes: `claudeAliasMap` (Task 4) via `ClaudeAliasTarget`; `auth.Token.ClaudeModelAliases` (Task 1).

- [ ] **Step 1 — Failing test: precedence + single resolution.** For a token with the flag on: an explicit `model_override_map` entry wins over a generated alias; absent that, a generated `anthropic-local-…` resolves **once** to its real gateway id (no second override applied); absent both, catch-all then unknown-redirect apply as before.
- [ ] **Step 2 — Failing test: disable + isolation + access.** Flag off ⇒ the alias id is not resolved (follows unknown-model behavior, no silent fallback); another token without the flag cannot resolve the alias; access checks (allowlist/locked/etc.) run on the **effective target**; `RequestedModel` keeps the alias, `Model` the target.
- [ ] **Step 3 — Run, verify fail.**
- [ ] **Step 4 — Implement.** Add `ClaudeAliasTarget` to the portal service; in `resolveModelOverride`, after the explicit map and before catch-all, if `token.ClaudeModelAliases` and `ClaudeAliasTarget(token, requested)` matches, return the real target (single lookup, no recursion). Keep `RequestedModel`/`Model` semantics; all downstream gates already run on `Model`.
- [ ] **Step 5 — Run, verify pass** (native + translate paths; cover streaming + a tool call in the test matrix).
- [ ] **Step 6 — Commit** (`feat(gateway): resolve generated Claude aliases in shared preflight (#225)`).

---

## Phase E — Documentation

### Task 8: Canonical docs + OpenAPI + data-model

**Files:**
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (alias contract + discovery metadata), `routing-and-model-selection.md` (precedence + eligibility), `persistence.md` + `docs/architecture/reference/data-model.md` (migration v88 + column), `security-auth-rbac.md` (token setting + discovery-vs-invocation), `docs/architecture/reference/api-surface.md` + the OpenAPI yaml (`claude_model_aliases` token field; `original_model_id`/`display_name` discovery fields)
- Test: `make lint-docs` (links/anchors/openapi refs)

- [ ] **Step 1 — Write the doc updates** describing the flag, alias stability + hex fallback, precedence, metadata placement (sibling of `capabilities`), eligibility/exclusions, and limitations; add the v88 migrations-table row.
- [ ] **Step 2 — Run `make lint-docs`; verify `check-docs: OK`.**
- [ ] **Step 3 — Commit** (`docs: document the Claude model alias feature (#225)`).

---

## Final (handled by subagent-driven-development)

Whole-branch review on the most capable model; full verification (`make test-go`, `make lint-go`, `make lint-docs`, Postgres leg, Sonar `sonar-scan` → `sonar-branch-findings`, frontend `npm test`/`build`/`format:check`); fold durable notes into `docs/architecture/`; remove `docs/superpowers/` + `docs/implementation-status.md`; push + PR (Auto-fix on).

# Per-token opt-in for usage-limit signals — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Usage-limit headers reach a client only when the token opts in with `send_usage_limits`. A second switch, `codex_model_switch_prompt`, lets the Codex CLI show its gpt-5.4-mini prompt.

**Architecture:**
- **Persistence:** two new `api_tokens` booleans travel from the store into `auth.Token`. They follow the `decisions_confidence_normalize` pattern end to end.
- **Request-time gate:** every site that writes usage headers checks `token.SendUsageLimits` first. The block writers get explicit flags.
- **Credits flag:** the gateway's credits flag is written only when the popup switch is off.
- **Portal and docs:** the portal gains the checkboxes; the docs gain ADR-110.

**Tech Stack:** Go 1.26 (`gateway/backend`, module `op-ai-gateway`), memory/sqlite/postgres stores, React/TS/Vite/MUI portal (Vitest).

**Spec:** the approved design `2026-11-22-usage-limits-token-opt-in-design.md` in the sibling `specs` folder. Issue JLor08/op-ai-gateway#420. Read §5 before your task.

## Global Constraints

**Names and values**
- Column, JSON and TS names: `send_usage_limits` and `codex_model_switch_prompt`. Go fields: `SendUsageLimits` and `CodexModelSwitchPrompt`, on `store.TokenRecord` and `auth.Token`.
- Both are `integer not null default 0`. One append-only migration, **112** `token_usage_limit_switches`, adds them via `addColumnIfMissing` and runs `update api_tokens set send_usage_limits = 1 where claude_code_usage_display = 1`.

**Token behaviour**
- User tokens: create, PATCH (present value replaces, absent keeps) and read. Ungated, no rotation.
- Service tokens: create and read only. Issue #421 adds editing later.

**Header behaviour**
- `send_usage_limits` off means: no `anthropic-ratelimit-unified-*` and no `x-codex-*` header toward the client, on any path. The only exception is `x-codex-promo-message` on the Codex block.
- What always stays: status codes, `retry-after`, `x-should-retry`, the `billing_error` and `usage_limit_reached` bodies (sentence and `resets_at` included), and the verbatim relay of vendor error bodies.
- Unchanged: capture redaction, which stays entitlement-based, and `writeLimitDenied`.
- `codex_model_switch_prompt` on, together with `send_usage_limits` on: the gateway never writes its own `X-Codex-Credits-Has-Credits` / `X-Codex-Credits-Unlimited`. The vendor's relayed credits headers are untouched.

**Repo rules**
- Stable error codes.
- i18n keys in German and English together.
- Repo text in English; no attribution lines; no LAN host names.
- No `server-agent/` change.
- Gates:
  - Go: `golangci-lint fmt --diff` and `golangci-lint run` from `gateway/backend`, and `go test`.
  - Frontend: `npm test`, `build`, `lint`, `format:check` from `gateway/frontend`.
  - `make lint-docs`.

## Review Focus

1. **A token switched off while the window cache still holds its windows.** The next response must carry nothing, so the gate sits at attach, not only at observe. Test: Task 2.
2. **A PATCH that turns the switch on or off** takes effect on the very next request, on sqlite and memory alike. The bearer path must reread the flag. Test: Task 1.
3. **The popup switch on while `send_usage_limits` is off.** No credits headers and no `x-codex-*` except the promo on a block. Test: Task 3.
4. **A gateway block with the switch off** still stops retries: Messages 429 + `X-Should-Retry: false` + `billing_error`, Codex 429 + `usage_limit_reached` + promo. Tests: Tasks 2 and 3.
5. **A service token created with both switches.** Its bearer resolves to an `auth.Token` carrying both flags. Test: Task 1.

---

### Task 1: Columns, store, `auth.Token`, portal token API

**Files:**
- Modify (store):
  - `gateway/backend/internal/store/migrate.go`: migration 112.
  - `gateway/backend/internal/store/models.go`: `TokenRecord`.
  - `gateway/backend/internal/store/sqlite_token.go`: insert, `tokenColumns`, scan, the metadata update, and the copy into `auth.Token` in `resolveBearer`.
- Modify (auth): `gateway/backend/internal/auth/token_store.go` (`auth.Token`).
- Modify (memory driver): `gateway/backend/internal/portal/memory_directory.go`: create, the update copy list, and the `auth.Token` builders.
- Modify (portal service):
  - `gateway/backend/internal/portal/service.go`:
    - `TokenDTO` (always emitted), `CreateTokenRequest`;
    - `UpdateTokenRequest` (`*bool`), `CreateToken`, `UpdateToken`;
    - `tokenDTO`, `AuthorizeRunAsToken`.
  - `gateway/backend/internal/portal/service_services.go`: `CreateServiceTokenRequest`, `ServiceTokenDTO`, `CreateServiceToken`, `serviceTokenDTO`.
- Modify (docs):
  - `docs/architecture/reference/data-model.md`: the `api_tokens` row, a migration-112 row, and the heading bump to 112. Move every `#4-migration-history-111-migrations` link across `docs/` to `-112-`; grep the whole tree.
  - `docs/architecture/cross-cutting/persistence.md`: the token opt-in paragraph, and the migration counts if any.
- Test:
  - `gateway/backend/internal/store/conformance_test.go`: a round-trip, plus the migration-40 rebuild re-add list.
  - New `gateway/backend/internal/store/migration112_token_usage_limit_switches_test.go`, copied from the migration-111 test.
  - Portal token tests: `service_token_test.go` or a new `service_token_usage_switches_test.go`.
  - The portal memory or store conformance tests that check `auth.Token` builders.

**Interfaces:**
- Produces:
  - `auth.Token.SendUsageLimits bool` and `auth.Token.CodexModelSwitchPrompt bool`, populated on every bearer path (sqlite `resolveBearer`, memory directory, run-as);
  - `TokenDTO` / `ServiceTokenDTO` JSON fields `send_usage_limits` and `codex_model_switch_prompt`;
  - PATCH `/api/portal/tokens/{id}` accepting both.

- [ ] **Step 1: Write the failing tests.**
  - `TestMigration112TokenUsageLimitSwitches`:
    - migrate to 111;
    - seed one token with `claude_code_usage_display=1` and one with 0;
    - migrate to 112;
    - the first reads `send_usage_limits=1`, the second 0, and both read `codex_model_switch_prompt=0`.
  - `TestConformanceTokenUsageLimitSwitchesRoundTrip` (all drivers): create with both true and read back true; `UpdateTokenMetadata` with false, false reads back false.
  - `TestTokenUsageSwitchesReachAuthToken`: for sqlite and memory, a token created with both true resolves through the bearer lookup to an `auth.Token` with both true. After PATCH to false, the next lookup reports false.
  - Service test:
    - `CreateToken` with both true gives a DTO with both true;
    - `UpdateToken` with `SendUsageLimits: ptr(false)` turns it off, and an absent field keeps the value;
    - an inference-only bearer may set both (ungated).
  - `TestCreateServiceTokenUsageSwitches`: a service token created with both true has a DTO with both true, and its bearer resolves to an `auth.Token` with both true.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/store/ ./internal/portal/ ./internal/auth/ -run 'Migration112|UsageLimitSwitches|UsageSwitches' -count=1`. Expected: FAIL (undefined fields).
- [ ] **Step 3: Implement** along the `decisions_confidence_normalize` pattern. Regenerate `portal/api_tracing_gen.go` only if the `portal.API` interface changes; gowrap drops the SPDX header, so re-add it.
- [ ] **Step 4: Run** the same command, then the full `./internal/store/ ./internal/portal/ ./internal/auth/`, then the postgres leg. For the postgres leg, Docker is running; start `postgres:17-alpine` like CI (`POSTGRES_PASSWORD=postgres`, `POSTGRES_DB=op_test`, port 5432) and set `OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:5432/op_test?sslmode=disable`. Then run `make lint-docs`. Expected: all pass, `check-docs: OK`.
- [ ] **Step 5: Commit.** `feat(tokens): send_usage_limits and codex_model_switch_prompt token switches (#420)`, with a body on the columns, the backfill, the bearer wiring and service tokens being create-only (#421).

### Task 2: Gate the Claude Code (Messages) signals

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go`, the Messages block (~l.1146-1165).
- Modify: `gateway/backend/internal/gateway/ratelimit_window_cache.go` (`observeRateLimitWindows`, `attachRateLimitWindows`).
- Modify: `gateway/backend/internal/gateway/principal_limit_headers.go` (`writeLimitBlocked`, `attachTranslatedUsageLimits`).
- Modify: `gateway/backend/internal/gateway/inference_handlers.go`: the `writeLimitBlocked` caller in `admitPrincipal` passes the flag.
- Test: new `gateway/backend/internal/gateway/usage_limit_optin_messages_test.go`. Update existing tests that rely on today's ungated behaviour by setting `SendUsageLimits: true` on their tokens, not by weakening assertions.

**Interfaces:**
- Consumes: `auth.Token.SendUsageLimits` (Task 1).
- Produces: `writeLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time, sendUsage bool)`.
  - With `sendUsage` false it writes `Retry-After`, `X-Should-Retry: false` and the unchanged `billing_error` body, and no unified header.

- [ ] **Step 1: Write the failing tests** (switch off unless stated). Positive header checks read `rec.Result().Header`.
  - `TestMessagesOptOutRelaysNoUnifiedHeaders`: a native vendor Messages response with the unified family, entitled owner, switch off. The client gets `Retry-After` and `X-Should-Retry` when present, and no `anthropic-ratelimit-unified-*`.
  - `TestMessagesOptOutAttachesNoCachedWindows` (Review Focus 1):
    - the owner observes windows with the switch on;
    - then flips it off (token value);
    - a local Messages response carries no unified header;
    - the translate buffered and SSE paths likewise.
  - `TestMessagesOptOutObservesNothing`: a vendor response with the switch off leaves the cache empty for the principal (a later switch-on local response attaches nothing).
  - `TestMessagesOptOutNoPrincipalStanding`: a principal with a quota and the switch off gets no unified headers on native or translate success.
  - `TestMessagesBlockOptOut` (Review Focus 4): an exhausted quota with the switch off answers 429 with `Retry-After`, `X-Should-Retry: false` and a body of type `billing_error` with the unchanged message, and no `anthropic-ratelimit-unified-*` header.
  - `TestMessagesOptInUnchanged`: the same scenarios with the switch on produce today's headers (relay, cache attach, standing, block headers).
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/gateway/ -run 'TestMessagesOpt|TestMessagesBlockOptOut' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
  - In the native Messages block, the gate becomes `hasUnifiedRateLimitHeader(resp.Header) && token.SendUsageLimits && entitled`. Keep the entitlement lookup lazy: check the switch before the lookup. The cache attach and the standing run only when `token.SendUsageLimits`.
  - `observeRateLimitWindows`, `attachRateLimitWindows` and `attachTranslatedUsageLimits` return early when `!token.SendUsageLimits`.
  - `admitPrincipal` passes `token.SendUsageLimits` to `writeLimitBlocked`.
- [ ] **Step 4: Run** the same command, then `go test ./internal/gateway/ -count=1 -timeout 20m` (foreground, wait). Expected: PASS.
- [ ] **Step 5: Commit.** `feat(gateway): send Claude Code usage-limit headers only for tokens that opt in (#420)`.

### Task 3: Gate the Codex (Responses) signals and the popup switch

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go`, the Responses block (~l.1166-1187).
- Modify: `gateway/backend/internal/gateway/codex_window_cache.go` (`observeCodexRateLimitWindows`, `attachCodexRateLimitWindows`).
- Modify: `gateway/backend/internal/gateway/codex_principal_limit_headers.go`:
  - `writeCodexPrimaryWindow`
  - `writeCodexLimitStanding`
  - `attachCodexPrincipalLimitStanding`
  - `attachTranslatedCodexUsageLimits`
  - `writeCodexLimitBlocked`
- Modify: `gateway/backend/internal/gateway/inference_handlers.go`: the `writeCodexLimitBlocked` caller.
- Test: new `gateway/backend/internal/gateway/usage_limit_optin_codex_test.go`. Update existing Codex tests by setting the switch, as in Task 2.

**Interfaces:**
- Consumes: `auth.Token.SendUsageLimits`, `auth.Token.CodexModelSwitchPrompt` (Task 1).
- Produces:
  - `writeCodexPrimaryWindow(h http.Header, r LimitReading, usedPercent int, offerModelSwitch bool)`: it writes the two credits headers only when `!offerModelSwitch`.
  - `attachCodexPrincipalLimitStanding(ctx context.Context, dst http.Header, now time.Time, offerModelSwitch bool)`.
  - `writeCodexLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time, sendUsage, offerModelSwitch bool)`. With `sendUsage` false it writes `Retry-After`, `X-Should-Retry: false`, `X-Codex-Promo-Message` and the unchanged `usage_limit_reached` body, and no other `x-codex-*` header.

- [ ] **Step 1: Write the failing tests.** Positive checks read `rec.Result().Header`.
  - `TestCodexOptOutRelaysNoFamily`: a native Responses vendor response with `x-codex-*` (including credits) and the switch off. No `x-codex-*` reaches the client.
  - `TestCodexOptOutAttachesNoCachedWindows`: the same structure as Task 2's cache test, for native local responses and translate buffered and SSE.
  - `TestCodexOptOutObservesNothing`.
  - `TestCodexOptOutNoPrincipalStanding`.
  - `TestCodexBlockOptOut` (Review Focus 4): 429, `Retry-After`, `X-Should-Retry: false`, `X-Codex-Promo-Message` present, a body of type `usage_limit_reached` with `resets_at`, and no other `x-codex-*` header (no primary window, no credits).
  - `TestCodexModelSwitchPromptDropsCreditsFlag`:
    - With both switches on, the success standing and the block carry the primary window but no `X-Codex-Credits-Has-Credits` / `-Unlimited`.
    - With only `send_usage_limits` on, both carry `true` / `false` as today.
  - `TestCodexModelSwitchPromptWithoutSendIsMoot` (Review Focus 3): popup on and send off gives no credits header and no `x-codex-*` except the promo on the block.
  - `TestCodexVendorCreditsUntouchedByPromptSwitch`: with the entitled owner and both switches on, a vendor response's own `x-codex-credits-*` is relayed unchanged.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/gateway/ -run 'TestCodexOpt|TestCodexBlockOptOut|TestCodexModelSwitch|TestCodexVendorCredits' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement** as Task 2, plus the `offerModelSwitch` threading:
  - `!token.SendUsageLimits` short-circuits every success-path site;
  - the native standing call passes `token.CodexModelSwitchPrompt`;
  - `admitPrincipal` passes both flags.
- [ ] **Step 4: Run** the same command, then the full gateway package (foreground, `-timeout 20m`). Expected: PASS.
- [ ] **Step 5: Commit.** `feat(gateway): Codex usage-limit headers only on opt-in, and a token switch for the gpt-5.4-mini prompt (#420)`.

### Task 4: Portal checkboxes

**Files:**
- Modify:
  - `gateway/frontend/src/api/tokens.ts`: `PortalToken`, `CreateTokenRequest`, `UpdateTokenRequest`.
  - `gateway/frontend/src/api/services.ts`: the service token types and create request.
  - `gateway/frontend/src/components/TokenList.tsx`: create and edit checkboxes, the dependency, the create-form coupling with the Claude Code token format, a list badge.
  - `gateway/frontend/src/components/ServiceTokensSection.tsx`: create checkboxes.
  - `gateway/frontend/src/i18n.ts`: `de` and `en`.
- Test: `TokenList.test.tsx`, `ServicesView.test.tsx` (or the service-token test file), `api.test.ts`. Add the new required field to fixtures where the build needs it.

**Interfaces:**
- Consumes: the JSON fields from Task 1.

- [ ] **Step 1: Write the failing tests.**
  - Create sends `send_usage_limits` and `codex_model_switch_prompt`.
  - The Codex checkbox is disabled until "send usage limits" is checked; unchecking the latter unchecks and disables the former.
  - Ticking the Claude Code token format in the create form ticks "send usage limits", and the user can untick it again.
  - Edit:
    - PATCH carries both changed values;
    - toggling them does not call rotate (unlike the token format);
    - a row with `send_usage_limits` shows the badge.
  - Service token create sends both.
  - Help texts:
    - "send usage limits": clients (Claude Code, Codex CLI) then show the remaining usage, and Claude Code also needs the Claude Code token format;
    - Codex: Codex then offers gpt-5.4-mini at 90 % of its main window, also when the gateway's own limit is shown.
- [ ] **Step 2: Run.** `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx src/api.test.ts <service test file>`. Expected: FAIL.
- [ ] **Step 3: Implement**, following the existing opt-in checkbox blocks.
- [ ] **Step 4: Run** the same tests, then `npm test`, `npm run build`, `npm run lint`, `npm run format:check`. Expected: all pass.
- [ ] **Step 5: Commit.** `feat(portal-ui): token switches for sending usage limits and the Codex gpt-5.4-mini prompt (#420)`.

### Task 5: Architecture docs and ADR-110

**Files:**
- Modify `docs/architecture/cross-cutting/compatibility-and-inference.md`: §6 (the header paragraphs), §9.3, §9.4, §9.5, §9.6, and the error-table rows.
  - "The gateway does not look at the shape", "with no token option" and the always-suppress credits text become conditional on the switches.
  - State what survives without the opt-in, including the promo exception.
- Modify `docs/architecture/09-architecture-decisions.md`: ADR-110 (next free), recording:
  - the opt-in;
  - what survives without it and why (the verified Claude Code and Codex retry behaviour);
  - the promo exception;
  - the popup switch and the credits carry-over;
  - two switches instead of one (the token format stays rotation-only);
  - the backfill;
  - service tokens being create-only until #421.

  ADR-108 rejected "a token opt-in": mark ADR-106–109 as amended, following the file's convention for amendments if one exists, otherwise a one-line note under each.
- Modify:
  - `docs/architecture/cross-cutting/security-auth-rbac.md` (token section);
  - `docs/architecture/reference/api-surface.md` (token field table, tokens route, service tokens);
  - `docs/architecture/reference/openapi.yaml` (token and service-token schemas, PATCH, `/v1/responses` and `/v1/messages` notes);
  - `docs/architecture/12-glossary.md`;
  - `docs/architecture/cross-cutting/external-vendor-accounts.md` (where it says figures go to entitled callers: add the token opt-in);
  - `docs/architecture/11-risks-and-technical-debt.md` (if a row describes usage display as unconditional);
  - `README.md` (the coding-agents bullet).

- [ ] **Step 1: Write the doc changes.** Each claim must match the code from Tasks 1–3.
- [ ] **Step 2: Run** `make lint-docs`, and grep the docs tree for statements that still say usage figures go to every caller or that the credits flag is always sent. Expected: `check-docs: OK`, and no stale statement.
- [ ] **Step 3: Commit.** `docs(architecture): ADR-110 per-token opt-in for usage-limit signals (#420)`.

The controller runs these after Task 5, outside the subagent tasks:
- End to end, Claude Code 2.1.328: a JWT token with the switch off, then on, against a stub upstream with unified headers. Use the #407 script pattern.
- End to end, Codex 0.137.0: `codex exec` against a sqlite gateway with a quota, switch off and on. Check the rollout's `token_count.rate_limits`, and the promo text on a block. Scripts are in `/tmp/codex-opaigw`.
- The full gates, including the postgres leg and Sonar branch findings.
- The final whole-branch review and the cleanup of branch-local docs.
- Push, the PR, Auto-fix.

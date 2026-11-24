# Portal Chat Vendor Access Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking. (This run: workflow-orchestrated implementer+reviewer agents per phase, controller in the loop between phases.)

**Goal:** A per-user provider-access policy for the token-less portal chat (edited like a token's), and a chat model picker that lists exactly the selected run-as token's models under its names, with "offered (original)" labels for generated Claude aliases.

**Architecture:** One new `users` column carries the chat-session policy; `sessionPrincipal` decodes it (empty = today's all+groups). The save rides the existing `PUT /api/portal/chat-settings` with portal-side validation. `GET /api/portal/models` honours the existing `X-OP-Run-As-Token` header (capture-redacted) through the existing `AuthorizeRunAsToken` gate. The frontend extracts the token form's provider-access glue into a shared editor and gives `ChatStore` a token-scoped model list.

**Tech Stack:** Go 1.26 (`gateway/backend`), SQLite/PostgreSQL/memory stores, React+TS+Vite+MUI (`gateway/frontend`), vitest.

**Spec:** `docs/superpowers/specs/2026-11-24-portal-chat-vendor-access-design.md` (read first; §3 is the verified code map, §4 the design, §5 the designer calls).

## Global Constraints

- Branch `feat/portal-chat-vendor-access` in `.worktrees/portal-chat-vendor-access`, based on `main` (#431 merged as 4e7a6c4a). Never commit to `main`; the PR targets `main`.
- All three store drivers; append-only migration **115** (`user_chat_vendor_access`); run store tests with `OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable'` and confirm the `/postgres` subtests ran (`grep -E "^ +--- (PASS|FAIL|SKIP): .*/postgres"`). Never two store-suite runs at once. If 115 is taken at rebase time, renumber.
- Wire names: the policy field is `vendor_access` (same `VendorAccessDTO` as tokens: `all`, `include_groups`, `accounts[]`, `groups[]`); the column is `users.chat_vendor_provider_access text not null default ''`; the models run-as transport is the `X-OP-Run-As-Token` request header (never a query param).
- Stored-value semantics (spec §4.1): `''` = unset → `VendorAccess{All: true, IncludeGroups: true}`; non-empty JSON = explicit policy; an explicit save whose encoding would be `""` stores `{"all":false}`.
- Existing error codes stay stable; policy validation reuses `portal.token_vendor_access_invalid` / `portal.token_vendor_access_conflict`; chat-settings save keeps `portal.chat_settings_update_failed`. New answers: bearer caller + run-as header on the models listing, or bearer caller sending `vendor_access` on chat-settings → 403 `portal.token_forbidden` (via `writePortalTokenError(portal.ErrTokenForbidden)`); `?manage=1` + run-as header → 400 `portal.models_run_as_conflict`.
- `portal.API` is hand-edited; regenerate `api_tracing_gen.go` with gowrap and KEEP its SPDX header.
- i18n keys in German AND English together (`gateway/frontend/src/i18n.ts`).
- Go gates per touched module: `golangci-lint fmt --diff ./...`, `golangci-lint run ./...`. Frontend gates: `npm run format:check`, `npm run lint`, `npx tsc --noEmit -p .`, touched vitest files, `npm run build`.
- Repo-facing text in English; no LAN host names in repo text. No ServerAgent change (no version bump).

## Review Focus

1. **An old client PUTs only the two booleans** → the stored policy must be untouched (absent `vendor_access` field ≠ clearing). Test in Task 3.
2. **Explicit "no providers" save** must round-trip as the zero policy, never as "unset → all". Tests in Tasks 1 and 3.
3. **A stored policy naming a deleted account/group** must not break `sessionPrincipal` or any listing — tolerant decode, stale ids match nothing. Test in Task 2.
4. **Switching the chat token must not flash the session's models** while the scoped list loads (and a failed scoped fetch must not silently fall back to the session list). Test in Task 7.
5. **The run-as header naming another user's (or an unknown/disabled) token** answers with the gate's own 403 `portal.token_forbidden`, identical body for all three (no existence leak); a bearer caller is refused with the same 403 before any lookup. Tests in Task 4.

---

## Phase A — Storage and session principal

### Task 1: Migration 115 and the user column

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append `{version: 115, name: "user_chat_vendor_access", up: migration115Up}`; `addColumnIfMissing(ctx, tx, dl, "users", "chat_vendor_provider_access text not null default ''")`)
- Modify: `gateway/backend/internal/store/models.go` (`User.ChatVendorAccess string` next to the two chat flags; doc comment: encoded vendor-access JSON, `''` = unset)
- Modify: `gateway/backend/internal/store/sqlite_user.go` (every raw column list + `scanUser`: CreateUser, UserByID, UserByEmail, UpdateUser, ListUsers)
- Modify: `gateway/backend/internal/account/service.go` (`UserUpdate.ChatVendorAccess *string`; `UpdateOwnProfile` and admin `UpdateUser` apply it)
- Check: `gateway/backend/internal/portal/memory_directory.go` copies `store.User` by value (then nothing to do; otherwise carry the field)
- Tests: `gateway/backend/internal/store/migration115_user_chat_vendor_access_test.go` (`forEachDialectMigratedTo`), extend the user round-trip test that covers `chat_log_communication` with the new column. The users table exists only on SQLStore: sqlite + postgres via `forEachDialect`; there is no memory users driver in `internal/store` (portal's `memory_directory.go` copies `store.User` by value).
- Doc contract on `UserUpdate.ChatVendorAccess`: pre-validated encoded JSON, produced only by `portal.EncodeChatSessionVendorAccess`; the admin user PATCH handler (`internal/gateway/admin_users.go`) must NOT expose it.

**Interfaces:**
- Produces: `store.User.ChatVendorAccess`, `account.UserUpdate.ChatVendorAccess *string`.

- [ ] Step 1: failing tests — `TestMigration115AddsUserChatVendorAccess` (migrate 114→115 with an existing user row reads `''`; replay is a no-op) and a round-trip case: create/update a user with `ChatVendorAccess: '{"all":false}'`, read back verbatim on sqlite+postgres.
- [ ] Step 2: run `go test ./internal/store -run 'Migration115|UserChatVendorAccess|UserRoundTrip' -count=1` with and without the DSN — FAIL.
- [ ] Step 3: implement.
- [ ] Step 4: same command + `go test ./internal/store ./internal/account -count=1` (DSN set) — PASS, `/postgres` legs confirmed.
- [ ] Step 5: commit `feat(store): per-user chat-session vendor access column (migration 115)`.

### Task 2: sessionPrincipal applies the stored policy

**Files:**
- Modify: `gateway/backend/internal/gateway/auth.go` (`sessionPrincipal` ~:310-339; new helper `func chatSessionVendorAccess(user store.User) auth.VendorAccess` — `''` → `{All: true, IncludeGroups: true}`, else `store.DecodeVendorAccess(user.ChatVendorAccess)`)
- Modify: the comment block on `sessionPrincipal` (auth.go ~:325-337) and the `TokenVendorPrefix` doc comment in `gateway/backend/internal/routing/vendor_access.go` (~:32-34: "a portal session, which is always all:true" becomes false)
- Tests: the two existing tests pinning `VendorAccess{All:true, IncludeGroups:true}` (`TestSessionPrincipalGrantsAllVendorAccess` auth_test.go:120, `TestSessionPrincipalVendorAccessIncludesGroups` :1086 — adjust to "unset user"), new `gateway/backend/internal/gateway/auth_chat_vendor_access_test.go`

**Interfaces:**
- Consumes: Task 1 field. Produces: `chatSessionVendorAccess` (used by both sessionPrincipal callers automatically).

- [ ] Step 1: failing table test `TestSessionPrincipalChatVendorAccess`: unset → All+IncludeGroups; stored `{"all":false}` → zero policy; stored policy with accounts/groups entries (incl. a stale `vag_gone` id) → decoded verbatim, no error; malformed JSON → zero policy (tolerant decode, documented trade: a corrupt row fails closed, matching token decode). Also assert the loopback path (`loopbackPrincipal`) carries the same policy.
- [ ] Step 2: run `go test ./internal/gateway -run 'SessionPrincipal|ChatVendorAccess' -count=1` — FAIL.
- [ ] Step 3: implement.
- [ ] Step 4: run it plus `go test ./internal/gateway -run 'Vendor' -count=1` — PASS.
- [ ] Step 5: commit `feat(gateway): the chat session's vendor access follows the stored per-user policy`.

## Phase B — Save path and token-scoped listing

### Task 3: Chat-settings save carries the policy; synthetic row exposes it

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` or new `service_chat_vendor_access.go`: exported `func (s *Service) EncodeChatSessionVendorAccess(ctx context.Context, userID string, d *VendorAccessDTO) (string, error)` — `userTokenPrincipal(auth.Token{UserID: userID})`, `validateVendorAccess`, encode via `store.EncodeVendorAccess`, `""` → `{"all":false}` (the `encodeChatVendorAccess` wrapper of spec §4.1)
- Modify: `gateway/backend/internal/portal/api.go` + regenerate `api_tracing_gen.go` (keep SPDX header); classify the method in `TestEveryVendorAccountServiceMethodIsClassified` if it bites
- Modify: `gateway/backend/internal/gateway/auth.go` `handlePortalChatSettings` (~:659): request gains `VendorAccess *portal.VendorAccessDTO` (json `vendor_access`); nil → untouched; non-nil from a bearer caller (`token.ID != ""`) → 403 `portal.token_forbidden` and nothing stored (the booleans keep today's session-or-bearer rule); non-nil from a session → portal encode (error mapping: `portal.token_vendor_access_invalid`/`_conflict` → 400, else `portal.chat_settings_update_failed` 500) → `account.UserUpdate.ChatVendorAccess`
- Modify: `tokensForUser` (portal): the synthetic chat-session row's `VendorAccess` is ALWAYS present when the user lookup succeeded: unset → `{all:true, include_groups:true}`; explicit zero → `{all:false}` with an empty `accounts` (so the existing `decodeVendorAccessDTO`, which renders the zero policy as nil, cannot be reused as-is — add a chat-row variant); any other stored policy → decoded. When the `UserByID` lookup fails (today's zero-value fallback), OMIT `vendor_access`. ONE `''`-default rule shared with Task 2 (export it from a spot both packages can import, e.g. `store`). `GET /api/admin/users/{id}/tokens` reuses `tokensForUser` and so carries the field by design.
- Tests: `gateway/backend/internal/gateway/portal_chat_settings_vendor_test.go`, portal tests for the new method

- [ ] Step 1: failing tests — PUT with `vendor_access` listing an own account persists (read back via `GET /api/portal/tokens` synthetic row); PUT without the field leaves it (Review Focus 1); explicit empty policy stores `{"all":false}` and the synthetic row shows `{"all":false,"accounts":[]}` (present, not omitted — Review Focus 2); invalid id → `portal.token_vendor_access_invalid` 400 and nothing stored; name conflict → `_conflict` 400; unset user's synthetic row shows `{all:true, include_groups:true}`; bearer caller sending `vendor_access` → 403 `portal.token_forbidden`, nothing stored, while a bearer PUT of only the booleans still works; failed user lookup → synthetic row without `vendor_access`.
- [ ] Step 2: run `go test ./internal/gateway -run 'ChatSettings' -count=1` and the portal method tests — FAIL.
- [ ] Step 3: implement.
- [ ] Step 4: run Step 2 + `go test ./internal/portal -run 'TokenVendor|ChatSession' -count=1` — PASS.
- [ ] Step 5: commit `feat(portal): chat-session provider access is saved and validated like a token policy`.

### Task 4: Run-as header on the models listing

**Files:**
- Modify: `gateway/backend/internal/gateway/portal_model_endpoints.go` (`handlePortalModels` :21): read the `X-OP-Run-As-Token` header (constant from `run_as.go`); when present: `?manage=1` too → 400 `portal.models_run_as_conflict`; bearer caller (`token.ID != ""`) → 403 `portal.token_forbidden` via `writePortalTokenError(portal.ErrTokenForbidden)` BEFORE any lookup (note: `runAsPrincipal` silently ignores the header for bearers on inference endpoints — this endpoint refuses instead, deliberately); else `s.Portal.AuthorizeRunAsToken(ctx, token, id)` (its errors through `writePortalTokenError`); then `Models(ctx, runAs)` AND `injectLoadingOnCounts(ctx, runAs, …)` with the run-as principal (alias keys come from its `ModelOverrideRules`)
- Tests: `gateway/backend/internal/gateway/portal_models_run_as_test.go`

- [ ] Step 1: failing tests — (a) token with a vendor prefix override sees `work/<model>` while the session sees the account prefix; (b) token with `claude_model_aliases` gets alias rows with `original_model_id`, session principal gets none; (c) another user's token id, an unknown id and a disabled own token → identical 403 `portal.token_forbidden` bodies (Review Focus 5); (d) bearer caller with the header → 403 `portal.token_forbidden` without a token lookup; (e) `?manage=1` + header → 400 `portal.models_run_as_conflict`; (f) a manual alias row's loading count is keyed through the run-as token's override rules.
- [ ] Step 2: run `go test ./internal/gateway -run 'PortalModelsRunAs' -count=1` — FAIL.
- [ ] Step 3: implement.
- [ ] Step 4: run it plus `go test ./internal/gateway -run 'PortalModels|RunAs' -count=1` — PASS.
- [ ] Step 5: commit `feat(gateway): the portal models listing can be read as a run-as token`.

## Phase C — Frontend

### Task 5: Extract the provider-access editor

**Files:**
- Create: `gateway/frontend/src/components/shared/VendorAccessEditor.tsx` (+ test)
- Modify: `gateway/frontend/src/components/TokenList.tsx` (the token form uses the editor; behavior-neutral — `buildVendorAccess` output unchanged)

**Interfaces:**
- Produces: `<VendorAccessEditor value={VendorAccessDraft} onChange={...} accounts={...} groups={...} labels={...}/>` — a controlled component holding NO fetching; the draft type mirrors what `buildVendorAccess` consumes today (all, includeGroups, account entries + overrides, group entries + overrides). Exact shape is the implementer's from the current glue; the editor must render identically (same i18n keys, same checkbox nesting incl. "incl. Gruppen" disabled-until-all).

- [ ] Step 1: failing test — `VendorAccessEditor.test.tsx` renders accounts/groups/overrides and reports changes; plus the existing `TokenList` + `TokenList.vendorGroups` suites as the refactor net.
- [ ] Step 2: `npx vitest run src/components/shared/VendorAccessEditor.test.tsx` — FAIL.
- [ ] Step 3: implement (move JSX + handlers; keep `buildVendorAccess` in TokenList or move alongside — one source).
- [ ] Step 4: `npx vitest run src/components/TokenList src/components/shared/VendorAccessEditor.test.tsx` — PASS (all pre-existing assertions untouched).
- [ ] Step 5: commit `refactor(portal-ui): extract the provider-access editor`.

### Task 6: Chat-session card edits the policy

**Files:**
- Modify: `gateway/frontend/src/api/auth.ts` (`updateChatSettings` body gains optional `vendor_access`), `gateway/frontend/src/api/tokens.ts` (no change needed — `PortalToken.vendor_access` exists; verify)
- Modify: `gateway/frontend/src/components/TokenList.tsx` (chat card ~:1104-1166: render `VendorAccessEditor` pre-filled from the synthetic row's `vendor_access`; save sends booleans + `vendor_access`; error mapping via existing `format.ts` keys), `gateway/frontend/src/i18n.ts` (chat note de+en)
- Tests: extend `TokenList.test.tsx` (chat card)

- [ ] Step 1: failing tests — card pre-fills "all + incl. groups" for an unset user; an explicit-zero row (`{all:false, accounts:[]}`) pre-fills with nothing ticked; unticking all and ticking one account sends `vendor_access: {all:false, accounts:[…]}`; the card sends its draft `vendor_access` on every save when the row carried the field; when the row has NO `vendor_access` (backend lookup failure) the editor is disabled with a hint and the save sends only the booleans; a `_conflict` error renders translated.
- [ ] Step 2: `npx vitest run src/components/TokenList.test.tsx` — FAIL (new cases).
- [ ] Step 3: implement.
- [ ] Step 4: Step 2 + `npm run format:check && npx tsc --noEmit -p .` — PASS.
- [ ] Step 5: commit `feat(portal-ui): provider access for the token-less chat session`.

### Task 7: Token-scoped chat model picker with "(original)" labels

**Files:**
- Modify: the api module that defines `models()`: `models(runAsTokenId?: string)` sends the `X-OP-Run-As-Token` header when given (never a query param); `ModelOption` gains `original_model_id?: string`
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx` (scoped list state: fetch on `selectedTokenId` change via a new provider prop `fetchModels(runAsTokenId) => Promise<ModelOption[]>` wired in `App.tsx`; `effectiveModels` = scoped list when a token is selected, else the `models` prop; loading flag; refresh after each run alongside the existing `onRefresh`, and inside the 15 s unavailable poll; fetch failure keeps the last scoped list and surfaces the existing error path)
- Modify: `gateway/frontend/src/components/Chat.tsx` (option label: when `original_model_id` is present and differs from the row `id` → label `${id} (${original_model_id})`, value stays `id`; do NOT use `display_name` there — on alias rows it is the target's label; loading state for the picker)
- Rules (binding): picking a manual alias that exists in the scoped list skips the client-side `model_override_map` remap, and availability + capability gates look up the picked row; a run-as token with a `server_override` keeps today's server-locked picker (`api.serverModels`) — accepted exception; the FIRST scoped fetch failing after a token switch leaves the picker empty with the error surfaced (no session-list fallback)
- Modify: `gateway/frontend/src/App.tsx` (pass `fetchModels`)
- Tests: `ChatStore` tests (scoped fetch, fallback to the session prop only when no token is selected, no-flash while loading — Review Focus 4, first-fetch failure shows empty + error, poll uses the scoped fetch, manual-alias pick is sendable with gates from the alias row and the hidden target never shown, server-override token keeps the server list), `Chat` label test (`anthropic-local-3f9a (qwen-coder)`), both locales where labels differ

- [ ] Step 1: failing tests first (list above).
- [ ] Step 2: `npx vitest run src/components/chat src/components/Chat.test.tsx` — FAIL (new cases).
- [ ] Step 3: implement.
- [ ] Step 4: Step 2 + full frontend gates (`format:check`, `lint`, `tsc`, `npm run build`) — PASS.
- [ ] Step 5: commit `feat(portal-ui): the chat model picker follows the selected token`.

## Phase D — Docs

### Task 8: Architecture docs

**Files:**
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md` §1 (session framing), §6.7 ("the owner's interactive portal session (all: true)"), §11.4 (session-principal row, member-reach sentence, and the closing "synthetic chat session row carries no vendor_access" sentence), §12.3 ("the portal session is treated as all + include_groups")
- Modify: `docs/architecture/09-architecture-decisions.md`: "Amended by ADR-113" notes on ADR-050 (decision (e) and its ADR-060 amendment sentence) and on ADR-060 (e)
- Modify: `docs/architecture/cross-cutting/security-auth-rbac.md` §5 (run-as consumers: the models listing as a new consumer; the §7 flowchart edge) and the §12 parenthetical about portal sessions
- Modify: `docs/architecture/11-risks-and-technical-debt.md` §11.4 row "Per-token vendor access is strict…" (session no longer always all; a shared account IS reachable from the token-less chat when named)
- Modify: `docs/architecture/12-glossary.md` entries "Vendor access (API token)" and "Run-as token"
- Verify (and only touch if now false): `docs/architecture/cross-cutting/routing-and-model-selection.md` §2.4 ("a session principal gets no aliases" stays true)
- Modify: `docs/architecture/reference/api-surface.md`: `/api/portal/chat-settings` body (incl. the stale wording "Own default chat run settings"; `vendor_access` session-only, 403 for bearers), `/api/portal/models` run-as header (403/400 answers, mutual exclusion with `manage`), the tokens synthetic row's `vendor_access` (always present / omitted on lookup failure; admin variant carries it), and three now-false sentences: the "Token vendor access" closing paragraph (session behaves as all: true), "The portal session and ?manage=1 never list an alias" (a session call with the run-as header does), and the session-sees-all sentence; `docs/architecture/reference/openapi.yaml` (all of it)
- ADR-113 records the accepted designer calls incl. the server-override picker exception and admin visibility
- Modify: `docs/architecture/reference/data-model.md` (users table columns; migration-history heading 114→115 with every inbound anchor rename)
- Add: ADR at the next free number (113 unless taken on this stack): per-user chat-session vendor policy on the session principal; `''`-vs-`{"all":false}`; run-as listing via query param; rejected alternatives (separate table, chat-only scoping, new endpoint, inference header)
- Run: `make lint-docs` — the only allowed hits are the branch-local `docs/superpowers/` files (removed before the PR)

- [ ] Step 1: write; Step 2: `make lint-docs`; Step 3: commit `docs(architecture): chat-session vendor access and the run-as models listing`.

## Phase E — Verification and finish (controller-led)

- [ ] Full backend: `go test ./... -count=1` with the Postgres DSN (no FAIL lines; known flakes re-run in isolation).
- [ ] Frontend: `format:check`, `lint`, `tsc`, `npm test`, `build`.
- [ ] Sonar: `make sonar-scan && make sonar-findings && make sonar-branch-findings` — 0 branch-attributed findings (fix otherwise).
- [ ] Live smoke on `make dev`: set a restrictive chat policy → token-less chat picker narrows; select a token with an override prefix + `claude_model_aliases` → picker shows the token's names with "(original)"; old-style save (booleans only) leaves the policy.
- [ ] Remove `docs/superpowers/` (and `docs/implementation-status.md` if present); `git diff --name-only origin/main...HEAD` shows neither.
- [ ] Push; PR against `main`; bind PR, enable Auto-fix.

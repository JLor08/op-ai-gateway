# Claude Code usage-limit display — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code (CLI and Claude Desktop) shows the Anthropic subscription's 5h/7d usage when it talks to OP AI Gateway. Three pieces make that happen: the gateway forwards the rate-limit headers, issues an opt-in token shape that makes Claude Code read them, and keeps the windows stable across providers. A sharer can opt members of a shared subscription into seeing the figures.

**Architecture:**
- **Relay:** a small header relay in `internal/gateway` copies an allowlist on the native Messages passthrough. An entitlement predicate gates the `anthropic-ratelimit-unified-*` part for vendor accounts.
- **Window cache:** a per-principal in-memory cache attaches the last subscription windows to other `/v1/messages` responses.
- **Token shape:** a user token can carry a JWT-shaped secret. The gateway still treats it as an opaque, hashed bearer.
- **Share opt-in:** a vendor-account opt-in lets members see the figures in Claude Code and in the portal.

**Tech Stack:** Go 1.26 (`gateway/backend`, module `op-ai-gateway`), three store drivers (memory / sqlite / postgres through the shared `SQLStore`), React + TypeScript + Vite + MUI portal (`gateway/frontend`), Vitest.

**Spec:** `docs/superpowers/specs/2026-11-21-claude-code-usage-limits-design.md` (approved). Issue: JLor08/op-ai-gateway#407. Read the spec before your task. It holds the why and the exact rules this plan cites by section (§5.1–§5.5).

## Global Constraints

**Header rules**
- Header allowlist (§5.1): names whose lowercased form starts with `anthropic-ratelimit-unified-`, plus `retry-after` and `x-should-retry`. Copy every value verbatim, never anything else: no `request-id`, no `anthropic-organization-id`, no `anthropic-ratelimit-requests-*` / `-tokens-*`.
- The relay and attach apply only to the `anthropic_messages` API flavor (`inference.APIFlavorAnthropicMessages`).

**Token shape**
- Claude Code token secret (§5.3): `eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0` + `.` + `eyJhdWQiOiJjbGF1ZGUtZ2F0ZXdheSJ9` + `.` + `<today's generated secret, opaigw_ + 64 hex>`. These are the base64url, unpadded encodings of `{"alg":"none","typ":"JWT"}` and `{"aud":"claude-gateway"}`. No other claims.
- Token setting name: `claude_code_usage_display` (JSON, DB column, TS field). Go field: `ClaudeCodeUsageDisplay`.

**Vendor-account opt-in**
- Name: `share_usage_with_members` (JSON, DB column, TS field). Go field: `ShareUsageWithMembers`.
- Only the account owner may set it.

**Persistence**
- New booleans are `integer not null default 0` columns added with `addColumnIfMissing`, in append-only migrations. Take the next free version numbers on the branch: 110 for the vendor-account column and 111 for the token column at the time of writing. Re-check against `main` before merging.

**Fixed behaviours**
- Window attach (§5.4) never emits a top-level `anthropic-ratelimit-unified-status: rejected`.
- Entitlement (§5.5) fails closed. A non-entitled principal gets only `retry-after` and `x-should-retry` from a vendor-account response.

**Repo rules**
- Error codes stay stable. The new 400 code is `portal.token_claude_code_usage_display_requires_rotation`.
- i18n keys go in German and English together (`gateway/frontend/src/i18n.ts`).
- Repo text is English. No attribution lines. No LAN host names.
- No change under `server-agent/`, so no agent version bump.
- Go formatting and lint gates: `golangci-lint fmt --diff` and `golangci-lint run` from `gateway/backend`. Frontend gates: `npm test`, `npm run build`, `npm run lint`, `npm run format:check` from `gateway/frontend`.
- Every task updates the architecture docs its behaviour touches, in the same commit. `make lint-docs` must pass.

## Review Focus

1. **Canonicalized and repeated headers.** Go canonicalizes `anthropic-ratelimit-unified-5h-utilization` to `Anthropic-Ratelimit-Unified-5h-Utilization`, and an upstream may send a header twice. The relay must match case-insensitively and copy all values. Test: Task 3.
2. **The instance-gone re-selection retry.** Headers from the discarded first attempt must not reach the client. Test: Task 3.
3. **A failed rotate after a successful PATCH in the portal edit flow.** The user must see the error, and the token must still show the old setting. Test: Task 9.
4. **Two JWT-shaped tokens of one user.** Both share the `secret_prefix` `eyJhbGci`, and both must create, list, authenticate and rotate independently. Test: Task 7.
5. **Stale or expired windows.** A principal whose cached windows all reset while idle must get nothing attached, and the entry must be pruned. A principal that later uses a second subscription account sees that account's windows only. Test: Task 10.

---

## Phase A — relay and entitlement

### Task 1: Vendor-account opt-in column and target stamping

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`
  - `VendorAccount`: add `ShareUsageWithMembers bool`.
  - `routing.Store`: add `UpdateVendorAccountShareUsage(ctx context.Context, id string, enabled bool) error` next to `UpdateVendorAccountShareLevel`.
- Modify: `gateway/backend/internal/routing/resolver.go`
  - `Target`: add `VendorAccountOwnerUserID string` and `VendorShareUsageWithMembers bool`, documented next to `VendorAccountID`.
  - Set both from `acc` in every vendor target builder (the three `VendorAccountID: acc.ID` sites, ~l.1416/1504/1569).
- Modify: `gateway/backend/internal/store/migrate.go` (migration 110 `vendor_account_share_usage`), `gateway/backend/internal/store/sqlite_vendor_accounts.go` (select/scan and the new update), and the memory vendor-account store (find the implementer of `UpdateVendorAccountShareLevel` for the memory driver).
- Test: `gateway/backend/internal/store/conformance_test.go`, a new `gateway/backend/internal/store/migration110_vendor_account_share_usage_test.go` (copy the shape of `migration108_token_decisions_confidence_normalize_test.go`), and `gateway/backend/internal/routing/resolver_test.go` (or the existing vendor-target test file).

**Interfaces:**
- Produces:
  - `routing.VendorAccount.ShareUsageWithMembers bool`
  - `routing.Store.UpdateVendorAccountShareUsage(ctx, id string, enabled bool) error`, which returns the store's not-found error for an unknown id, like `UpdateVendorAccountShareLevel`
  - `routing.Target.VendorAccountOwnerUserID string`
  - `routing.Target.VendorShareUsageWithMembers bool`

- [ ] **Step 1: Write the failing tests.**
  - `TestConformanceVendorAccountShareUsageRoundTrip`, per driver:
    - a new account reads back `ShareUsageWithMembers == false`;
    - `UpdateVendorAccountShareUsage(ctx, id, true)` then read gives `true`; `false` gives `false`;
    - an unknown id returns the not-found error.
  - `TestMigration110VendorAccountShareUsage`: after migrating to 109 and seeding an account, migrating to 110 reads it back with `false`.
  - A resolver test: the vendor target for an account with `OwnerUserID: "u-owner"`, `ShareUsageWithMembers: true` has `VendorAccountOwnerUserID == "u-owner"` and `VendorShareUsageWithMembers == true`, for an api-key account and for a subscription account.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/store/ ./internal/routing/ -run 'ShareUsage|Migration110|VendorTarget' -count=1`. Expected: FAIL, because the field, method and migration are missing.
- [ ] **Step 3: Implement** the column (migration 110), store read/write for all drivers, and the two `Target` fields.
- [ ] **Step 4: Run** the same command. Expected: PASS. Then run the postgres leg: the same command with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (provision it as CI does). If you cannot provision it, say so in your report.
- [ ] **Step 5: Docs.**
  - `docs/architecture/reference/data-model.md`: the `vendor_accounts` row, and a migration-110 row after 109 in the migration table.
- [ ] **Step 6: Commit.** `git commit -m "feat(store): vendor account share_usage_with_members opt-in column (#407)"`, with a body describing the column, the migration and the target stamping.

### Task 2: Portal predicate for vendor-account usage managers

**Files:**
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go`. Add `VendorAccountUsageManager`, built on `isSystem` and `isManagerOfSharedVendorAccount`, the same checks `authorizeVendorAccountRead` uses.
- Modify: the `portal.API` interface (find its declaration). Then regenerate `gateway/backend/internal/portal/api_tracing_gen.go` with `cd gateway/backend/internal/portal && go generate ./...`. `gowrap` is installed at `~/go/bin/gowrap`.
- Test: `gateway/backend/internal/portal/service_vendor_accounts_test.go` (or the sharing test file).

**Interfaces:**
- Produces: `func (s *Service) VendorAccountUsageManager(ctx context.Context, principal auth.Token, accountID string) (bool, error)`. It is on `portal.API`.
  - Returns true for system scope.
  - Returns true for a CM of the account while the account is `shared` or `full` by its effective level.
  - Returns false (nil error) for the owner, because the gateway checks the owner itself from the target. It also returns false for a member, a stranger and an unknown id.
  - A store error is returned as is, never read as true.

- [ ] **Step 1: Write the failing test.**
  - `TestVendorAccountUsageManager` with sub-tests:
    - `system_scope` → true;
    - `cm_of_shared_account` → true;
    - `cm_after_downgrade_to_private` → false;
    - `member` → false;
    - `stranger` → false;
    - `unknown_id` → false, nil error.
  - Reuse the fixtures of the existing sharing tests.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/portal/ -run TestVendorAccountUsageManager -count=1`. Expected: FAIL (undefined).
- [ ] **Step 3: Implement** the method, add it to `portal.API`, and regenerate the tracing wrapper.
- [ ] **Step 4: Run** the same test plus `go build ./...`. Expected: PASS and build OK.
- [ ] **Step 5: Commit.** `git commit -m "feat(portal): VendorAccountUsageManager predicate for rate-limit figures (#407)"`.

### Task 3: Rate-limit header relay with entitlement (G1)

**Files:**
- Create: `gateway/backend/internal/gateway/ratelimit_headers.go`
- Modify: `gateway/backend/internal/gateway/native_passthrough.go`. Call the relay directly before `w.WriteHeader(resp.StatusCode)` (~l.1146), only for `rel.pfReq.APIFlavor == inference.APIFlavorAnthropicMessages`.
- Modify: `gateway/backend/internal/gateway/server.go`. Add the entitlement cache field to `Server` and initialize it where the server is constructed.
- Test: `gateway/backend/internal/gateway/ratelimit_headers_test.go`, plus a passthrough test next to the existing native passthrough tests (reuse their `httptest` upstream and server helpers).

**Interfaces:**
- Consumes:
  - `routing.Target.VendorAccountID`, `.VendorAccountOwnerUserID` and `.VendorShareUsageWithMembers` (Task 1)
  - `portal.API.VendorAccountUsageManager` (Task 2)
- Produces:
  - `func isUnifiedRateLimitHeader(name string) bool`: case-insensitive prefix test.
  - `func forwardRateLimitHeaders(dst, src http.Header, includeUnified bool)`: copies `retry-after` and `x-should-retry` always. It copies the unified family only when `includeUnified` is true. It copies all values of each header.
  - `func (s *Server) rateLimitFiguresEntitled(ctx context.Context, token auth.Token, target routing.Target) bool`, with this rule, evaluated in order:
    1. true when `target.VendorAccountID == ""`;
    2. true when `token.UserID != "" && token.UserID == target.VendorAccountOwnerUserID`;
    3. true when `target.VendorShareUsageWithMembers`;
    4. otherwise the cached `s.Portal.VendorAccountUsageManager`. The cache key is principal type + id + account id, with a TTL of 60 s. An error or a nil `Portal` gives false.

- [ ] **Step 1: Write the failing unit tests** in `ratelimit_headers_test.go`.
  - `TestForwardRateLimitHeadersAllowlist`:
    - src has `Anthropic-Ratelimit-Unified-5h-Utilization: 0.82`, two values of `Anthropic-Ratelimit-Unified-Status`, `Retry-After: 37`, `X-Should-Retry: false`, `Request-Id: req_1`, `Anthropic-Organization-Id: org`, `Anthropic-Ratelimit-Requests-Limit: 50`.
    - With `includeUnified=true`, dst has exactly the unified ones (both status values), `Retry-After` and `X-Should-Retry`.
    - With `false`, dst has only `Retry-After` and `X-Should-Retry`.
  - `TestRateLimitFiguresEntitled`, table-driven:
    - self-hosted → true;
    - owner → true;
    - opt-in → true;
    - portal says true → true;
    - portal says false → false;
    - portal error → false;
    - nil portal → false;
    - a second call within the TTL does not call the portal again (count the calls with a fake).
- [ ] **Step 2: Write the failing passthrough tests.**
  - `TestNativeMessagesForwardsRateLimitHeaders`: an `httptest` upstream behind an `anthropic_messages` passthrough answers with the src headers above.
    - Cases: a 200 SSE stream, a 200 JSON body and a 429 JSON body.
    - The client response carries the allowlisted headers and none of `Request-Id`, `Anthropic-Organization-Id`, `Anthropic-Ratelimit-Requests-Limit`.
  - `TestNativeMessagesRateLimitHeadersNonEntitled`: a vendor target with another owner, opt-in off, and a fake portal returning false. The client gets `Retry-After` and `X-Should-Retry` only.
  - `TestNativeResponsesDoesNotForwardRateLimitHeaders`: the same upstream on the `openai_responses` flavor; the client gets none of them.
  - `TestNativeMessagesReselectionDropsFirstAttemptHeaders`: the first attempt answers with a status that triggers the instance-gone re-selection and carries `Retry-After: 99`. The second answers 200 with `Retry-After: 1`. The client sees only `1`.
- [ ] **Step 3: Run.** `cd gateway/backend && go test ./internal/gateway/ -run 'RateLimit' -count=1`. Expected: FAIL.
- [ ] **Step 4: Implement** `ratelimit_headers.go` and the call site, which is `forwardRateLimitHeaders(w.Header(), resp.Header, s.rateLimitFiguresEntitled(r.Context(), *rel.token, rel.target))`. Adapt it to how `nativeRelay` exposes the token.
- [ ] **Step 5: Run** the same command and `go test ./internal/gateway/ -count=1`. Expected: PASS.
- [ ] **Step 6: Docs.**
  - `docs/architecture/cross-cutting/external-vendor-accounts.md`: in both places that say "No other inbound header is forwarded. The response and its SSE are relayed unchanged" (~l.786 and ~l.1038), state the response-header allowlist and the entitlement gate.
  - `docs/architecture/cross-cutting/compatibility-and-inference.md` §6: the same allowlist for the Messages passthrough.
- [ ] **Step 7: Commit.** `git commit -m "fix(gateway): forward rate-limit headers on the Messages passthrough (#407)"`, with a body naming the allowlist, the entitlement rule and the dropped headers.

### Task 4: Scrape per-window resets (G4)

**Files:**
- Modify: `gateway/backend/internal/gateway/vendor_usage_scrape.go` (`scrapeAnthropicUsage` and its constants).
- Modify: `gateway/backend/internal/provider/anthropic_messages.go` (`AnthropicClient.post`). Record the response headers into the capture sink before the status check, as `OpenAIResponsesClient.post` does, so a translate-path 429 is scraped too.
- Test: `gateway/backend/internal/gateway/vendor_usage_scrape_test.go` and `gateway/backend/internal/provider/anthropic_messages_test.go`.

**Interfaces:** none new.

- [ ] **Step 1: Write the failing tests.**
  - `TestScrapeAnthropicPerWindowResets`: `-5h-reset: 1795190222` and `-7d-reset: 1795445822` set `FiveHourResetAt` and `WeeklyResetAt` to those instants, even when `-reset` names another time.
  - `TestScrapeAnthropicTopLevelResetFallback`:
    - only `-reset` with `-representative-claim: five_hour` → `FiveHourResetAt` set;
    - `-reset` with `seven_day` and no `-7d-reset` → `WeeklyResetAt` set. That is the weekly fallback, my call.
    - `-reset` with no claim → neither set.
  - `TestAnthropicClientRecordsHeadersOn429`: on a 429, the capture sink holds `Anthropic-Ratelimit-Unified-Status`.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/gateway/ ./internal/provider/ -run 'ScrapeAnthropic|RecordsHeadersOn429' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command, plus the packages' full tests. Expected: PASS.
- [ ] **Step 5: Docs.** In `external-vendor-accounts.md` §5.1, update the table row for the Anthropic subscription to the per-window resets and the fallback rule, and drop the VERIFY-LIVE note on the reset.
- [ ] **Step 6: Commit.** `git commit -m "fix(gateway): scrape Anthropic per-window resets for the usage snapshot (#407)"`.

## Phase B — shared accounts in the portal (G5)

### Task 5: Share-usage opt-in service, endpoint and member DTO

**Files:**
- Modify: `gateway/backend/internal/portal/service_vendor_sharing.go`
  - Add `SetVendorAccountShareUsageRequest{Enabled bool \`json:"enabled"\`}`.
  - Add `SetVendorAccountShareUsage`.
  - Owner rule: as `authorizeVendorAccountRaise`'s owner check. A CM or a member gets `ErrVendorAccountForbidden`; a stranger and an unknown id get `ErrVendorAccountNotFound`; system scope is a stranger, as for every write.
  - Not gated on the sharing flags: the opt-in is allowed at any share level.
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go`
  - `VendorAccountDTO` gets `ShareUsageWithMembers bool \`json:"share_usage_with_members"\``, set in the management view.
  - `memberVendorAccountDTO` reads and sets `Usage` exactly when `acc.ShareUsageWithMembers`. It is the single builder for the read and for the list's member rows.
- Modify: the `portal.API` interface, then regenerate the tracing wrapper.
- Modify: `gateway/backend/internal/gateway/portal_vendor_account_endpoints.go`. Route `PUT /api/portal/vendor-accounts/{id}/share-usage` next to `share-level` (~l.211) to a new `handlePortalVendorAccountShareUsage`. Map the errors as the share-level handler does.
- Test: the portal sharing tests and the endpoint tests next to the share-level ones.

**Interfaces:**
- Consumes: `routing.Store.UpdateVendorAccountShareUsage` (Task 1).
- Produces:
  - `func (s *Service) SetVendorAccountShareUsage(ctx context.Context, principal auth.Token, id string, req SetVendorAccountShareUsageRequest) (VendorAccountDTO, error)`, which returns the management DTO.
  - The JSON field `share_usage_with_members` on vendor-account reads (management view only).
  - `PUT .../share-usage` with body `{"enabled": bool}` → 200 and the DTO.

- [ ] **Step 1: Write the failing tests.**
  - `TestSetVendorAccountShareUsage`:
    - the owner turns it on → the DTO and the stored value are true;
    - the owner turns it off → false;
    - a CM → `ErrVendorAccountForbidden`;
    - a member → `ErrVendorAccountForbidden`;
    - a stranger → `ErrVendorAccountNotFound`;
    - system scope → `ErrVendorAccountNotFound`.
  - `TestMemberVendorAccountDTOUsageFollowsOptIn`: with a stored snapshot, a member's single read and the list's member row carry `usage` when the opt-in is on and omit it when off.
  - An endpoint test: `PUT` by the owner → 200 with `share_usage_with_members: true`; by a member → 403; a malformed body → 400.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/portal/ ./internal/gateway/ -run 'ShareUsage|MemberVendorAccountDTOUsage' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command, plus the full `./internal/portal/` and `./internal/gateway/` packages. Expected: PASS.
- [ ] **Step 5: Docs.**
  - `external-vendor-accounts.md` §8.2: the member view carries `usage` when the owner opts in, and the opt-in also unlocks Claude Code figures (§5.5). Also the sharing section.
  - `docs/architecture/reference/api-surface.md`: the new route, the field and the errors.
  - `docs/architecture/reference/openapi.yaml`: the route and the field.
- [ ] **Step 6: Commit.** `git commit -m "feat(portal): owner opt-in to share a vendor account's usage figures with members (#407)"`.

### Task 6: Portal UI for the share-usage opt-in

**Files:**
- Modify:
  - `gateway/frontend/src/api/vendorAccounts.ts`: add the field, and `setVendorAccountShareUsage(id: string, enabled: boolean): Promise<VendorAccount>`.
  - `gateway/frontend/src/components/VendorAccountsView.tsx`: add the checkbox in the sharing section, rendered only for the owner (`is_owner`). Below it goes a help text naming both effects, Claude Code and the dashboard.
  - `gateway/frontend/src/i18n.ts`: add keys in `de` and `en`.
- Test:
  - `gateway/frontend/src/components/VendorAccountsView.test.tsx`
  - `gateway/frontend/src/components/DashboardVendorUsage.test.tsx`
  - `gateway/frontend/src/i18n.test.ts` if it pins key sets

**Interfaces:**
- Consumes: `PUT /api/portal/vendor-accounts/{id}/share-usage` and `share_usage_with_members` (Task 5).

- [ ] **Step 1: Write the failing tests.**
  - The owner sees the checkbox reflecting `share_usage_with_members`. Clicking it calls `setVendorAccountShareUsage(id, true)` and shows the updated state.
  - A non-owner (`is_owner: false`) does not see the checkbox.
  - `DashboardVendorUsage` renders usage rows for a member-only account row that carries `usage`. That confirms `canReportUsage` accepts a member row; if it does not, fix `canReportUsage`.
- [ ] **Step 2: Run.** `cd gateway/frontend && npx vitest run src/components/VendorAccountsView.test.tsx src/components/DashboardVendorUsage.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command, then `npm test`, `npm run build`, `npm run lint` and `npm run format:check`. Expected: all pass.
- [ ] **Step 5: Commit.** `git commit -m "feat(portal-ui): share-usage opt-in on shared vendor accounts (#407)"`.

## Phase C — Claude Code token format (G2)

### Task 7: Token column, secret shape, create and rotate

**Files:**
- Modify (store):
  - `gateway/backend/internal/store/models.go`: add `ClaudeCodeUsageDisplay bool` to `TokenRecord`.
  - `gateway/backend/internal/store/migrate.go`: migration 111 `token_claude_code_usage_display`.
  - `gateway/backend/internal/store/sqlite_token.go`: the INSERT, `tokenColumns`, `scanToken`, and the secret-rotation write, which must set the column together with the new hash and prefix. Extend the rotation store method's signature, or add a sibling.
  - The memory driver in `gateway/backend/internal/portal/memory_directory.go`, including its rotation path.
- Modify (service):
  - `gateway/backend/internal/portal/service.go`:
    - Add `TokenDTO.ClaudeCodeUsageDisplay bool \`json:"claude_code_usage_display"\``, always emitted.
    - Add `CreateTokenRequest.ClaudeCodeUsageDisplay bool`.
    - `UpdateTokenRequest.ClaudeCodeUsageDisplay *bool`: equal to the stored value means a no-op; different means the new sentinel `ErrTokenClaudeCodeUsageDisplayRequiresRotation`.
    - Add a `RotateTokenRequest{ClaudeCodeUsageDisplay *bool}` parameter to `RotateToken`.
    - Wrap the secret in `CreateToken` and `RotateToken`.
- Modify (gateway):
  - `gateway/backend/internal/gateway/portal_token_endpoints.go`: the rotate handler (~l.177) decodes an optional JSON body. An empty body means today's behaviour; malformed JSON → 400. Map the sentinel to 400 `portal.token_claude_code_usage_display_requires_rotation`.
- Unchanged: service tokens (`service_services.go`), agent tokens (`service_agent_tokens.go`) and `service_runtime.go`.
- Test:
  - `gateway/backend/internal/store/conformance_test.go`: the round-trip, plus the column in the migration-40 rebuild re-add list (~l.6519–6530).
  - A new `gateway/backend/internal/store/migration111_token_claude_code_usage_display_test.go`.
  - `gateway/backend/internal/portal/service_token_test.go`.
  - The token endpoint tests.

**Interfaces:**
- Produces:
  - `func claudeCodeTokenSecret(secret string) string`, in `portal/service.go` near `generateSecret`. It returns `claudeCodeTokenSecretPrefix + secret`, where `const claudeCodeTokenSecretPrefix = "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJhdWQiOiJjbGF1ZGUtZ2F0ZXdheSJ9."`.
  - `func (s *Service) RotateToken(ctx context.Context, owner auth.Token, id string, req RotateTokenRequest) (CreateTokenResponse, error)`. Update all callers.
  - `portal.ErrTokenClaudeCodeUsageDisplayRequiresRotation`.

- [ ] **Step 1: Write the failing tests.**
  - `TestCreateTokenClaudeCodeUsageDisplaySecretShape`. With the flag on, the secret:
    - splits into three dot-separated parts;
    - its part[1] base64url-decodes to exactly `{"aud":"claude-gateway"}`;
    - its part[0] decodes to `{"alg":"none","typ":"JWT"}`;
    - its part[2] matches `^opaigw_[0-9a-f]{64}$`;
    - `auth.HashSecret(secret)` authenticates through the bearer store;
    - the DTO reads `claude_code_usage_display: true`.

    With the flag off, the secret matches `^opaigw_[0-9a-f]{64}$`.
  - `TestRotateTokenKeepsAndSwitchesShape`:
    - rotate without a request keeps the shape both ways;
    - rotate with `true` on an opaque token → JWT shape and stored `true`;
    - rotate with `false` → opaque and stored `false`;
    - the old secret stops authenticating each time.
  - `TestUpdateTokenClaudeCodeUsageDisplayRequiresRotation`: same value → OK with nothing changed; different value → the sentinel, and nothing written.
  - `TestTwoClaudeCodeTokensOneUser`: two JWT-shaped tokens of one user both create, list, authenticate and rotate independently.
  - Store: `TestConformanceTokenClaudeCodeUsageDisplayRoundTrip` (create and rotate-with-change on all drivers) and `TestMigration111TokenClaudeCodeUsageDisplay`.
  - Endpoint:
    - `POST /api/portal/tokens/{id}/rotate` with `{"claude_code_usage_display": true}` → 200, a JWT-shaped secret;
    - with an empty body → 200, the same shape as before;
    - malformed → 400;
    - `PATCH` with a different value → 400 `portal.token_claude_code_usage_display_requires_rotation`.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/store/ ./internal/portal/ ./internal/gateway/ -run 'ClaudeCodeUsageDisplay|ClaudeCodeToken|RotateToken|Migration111' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command, then the full packages, then the postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set. Expected: PASS.
- [ ] **Step 5: Docs.**
  - `reference/data-model.md`: the `api_tokens` row and migration row 111.
  - `reference/api-surface.md`: the tokens route, the field table, the rotate body and the new error row.
  - `reference/openapi.yaml`: token list, create, PATCH (the equality rule), rotate body and the error.
  - `cross-cutting/persistence.md`: the token opt-in paragraph.
  - `cross-cutting/security-auth-rbac.md`: token secret formats; the gateway never parses the JWT shape; `alg: none` is harmless because nothing verifies it; the bearer secret is the third segment.
- [ ] **Step 6: Commit.** `git commit -m "feat(tokens): opt-in Claude Code token secret shape (#407)"`, with a body explaining the shape and why (Claude Code reads the rate-limit headers only for a JWT whose `aud` is `claude-gateway`).

### Task 8: Token create form and list badge

**Files:**
- Modify:
  - `gateway/frontend/src/api/tokens.ts`: add the field to `PortalToken` and `CreateTokenRequest`.
  - `gateway/frontend/src/components/TokenList.tsx`: the create state, reset, body, a checkbox in the opt-in block (~l.767–794) with a help text, and a "Claude Code" chip in the name column for tokens with the flag.
  - `gateway/frontend/src/i18n.ts`: `de` and `en`.
- Test: `gateway/frontend/src/components/TokenList.test.tsx`, `gateway/frontend/src/api.test.ts`.

**Interfaces:**
- Consumes: `claude_code_usage_display` on create and read (Task 7).

- [ ] **Step 1: Write the failing tests.**
  - Creating with the checkbox on sends `claude_code_usage_display: true`.
  - The one-time secret dialog shows the returned secret unchanged.
  - A listed token with the flag shows the chip; one without does not.
- [ ] **Step 2: Run.** `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx src/api.test.ts`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command and the four frontend gates. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(portal-ui): Claude Code token option on create, badge in the list (#407)"`.

### Task 9: Token edit flow with rotation

**Files:**
- Modify:
  - `gateway/frontend/src/api/tokens.ts`: `rotateToken(id: string, body?: { claude_code_usage_display: boolean })`.
  - `gateway/frontend/src/components/TokenList.tsx`: `openEdit` and `submitEdit` (~l.310–437).
  - `gateway/frontend/src/i18n.ts`.
- Test: `gateway/frontend/src/components/TokenList.test.tsx`.

**Interfaces:**
- Consumes: the rotate body and the PATCH equality rule (Task 7).

- [ ] **Step 1: Write the failing tests.**
  - In edit, the checkbox shows the stored value. Changing it shows the warning (saving rotates the token; the old secret stops working).
  - On save:
    - PATCH is called without `claude_code_usage_display`, or with the stored value;
    - then `rotateToken(id, {claude_code_usage_display: <new>})`;
    - then the one-time secret dialog opens with the new secret.
  - Saving without changing the checkbox does not call rotate.
  - A rotate failure after a successful PATCH shows the error, and the token row still shows the old setting.
- [ ] **Step 2: Run.** `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx`. Expected: FAIL.
- [ ] **Step 3: Implement.** Builder's call: whether to add a confirm step in the style of the existing rotate `ConfirmDialog`.
- [ ] **Step 4: Run** the same command and the four frontend gates. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(portal-ui): switch the Claude Code token option through rotation (#407)"`.

## Phase D — stable display across providers (G3)

### Task 10: Per-principal window cache

**Files:**
- Create: `gateway/backend/internal/gateway/ratelimit_windows.go`
- Test: `gateway/backend/internal/gateway/ratelimit_windows_test.go`

**Interfaces:**
- Produces:
  - `type rateLimitWindowCache struct`: a mutex-guarded map with a hard cap.
  - `func newRateLimitWindowCache(maxEntries int) *rateLimitWindowCache`. The server uses `maxEntries = 10000`.
  - `func (c *rateLimitWindowCache) Observe(principalKey string, h http.Header, now time.Time)`. It stores the unified headers when `h` has `anthropic-ratelimit-unified-status`, and ignores the call otherwise. It prunes expired entries on write. At the cap, it evicts the entry with the oldest observation.
  - `func (c *rateLimitWindowCache) Attach(principalKey string, dst http.Header, now time.Time) bool`. It writes the windows whose `-<w>-reset` > now, using `-<w>-utilization`, `-<w>-reset`, `-<w>-status` and `-<w>-surpassed-threshold` for `w` in `5h`, `7d`, `7d_oi`. It then writes the top-level fields:
    - `-status`: `allowed_warning` if any attached window status is `allowed_warning` or `rejected`, else `allowed`;
    - `-representative-claim` and `-reset`: only if the claim (`five_hour`→`5h`, `seven_day`/`seven_day_opus`/`seven_day_sonnet`→`7d`) maps to an attached window.

    It writes nothing else. It returns false and deletes the entry when no window is in the future.
  - `func principalKey(token auth.Token) (string, bool)`, built on `principalFor`, as `type + ":" + id`.

- [ ] **Step 1: Write the failing tests.**
  - `TestWindowCacheAttachCopiesFutureWindows`: both windows attached with exact values.
  - `TestWindowCacheNeverRejected`: the stored top-level status is `rejected` and the 5h status is `rejected`; the attached top level is `allowed_warning`.
  - `TestWindowCacheDropsExpiredWindows`: 5h expired, 7d future → only 7d attached, and the claim `five_hour` is not copied.
  - `TestWindowCacheAllExpiredPrunes`: `Attach` returns false, and a second `Attach` finds no entry.
  - `TestWindowCacheIgnoresHeaderlessObserve`: no status header → the earlier entry is kept.
  - `TestWindowCacheLastAccountWins`: two observations from different accounts' headers → the second one is attached.
  - `TestWindowCacheNeverAttachesOverageOrRetry`: `-overage-*`, `-fallback-percentage`, `retry-after` and `x-should-retry` in the observed headers are absent from the attach.
  - `TestWindowCacheCap`: cap 2, three principals → the oldest is evicted.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/gateway/ -run TestWindowCache -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the same command. Expected: PASS.
- [ ] **Step 5: Commit.** `git commit -m "feat(gateway): per-principal cache of subscription rate-limit windows (#407)"`.

### Task 11: Observe and attach in the request paths

**Files:**
- Modify:
  - `gateway/backend/internal/gateway/server.go`: a `rateLimitWindows *rateLimitWindowCache` field, initialized with `newRateLimitWindowCache(10000)`.
  - `gateway/backend/internal/gateway/inference_complete.go`:
    - In `recordUsage`, next to the `scrapeVendorAccountUsage` call, add `Observe` when `target.Provider == routing.ProviderVendorAnthropic` and `s.rateLimitFiguresEntitled(...)`.
    - In `complete`, add `Attach` before the buffered success write, when `req.APIFlavor == inference.APIFlavorAnthropicMessages`.
  - `gateway/backend/internal/gateway/stream_session.go`: in `beginStream`, call `Attach` before the SSE `WriteHeader` for the Anthropic flavor.
  - `gateway/backend/internal/gateway/native_passthrough.go`: call `Attach` after the relay, before `WriteHeader`, for the Anthropic flavor, a 2xx status, and only when the upstream response has no `anthropic-ratelimit-unified-status`.
- Test: `gateway/backend/internal/gateway/ratelimit_windows_paths_test.go`.

**Interfaces:**
- Consumes: Task 3 (`rateLimitFiguresEntitled`, the relay) and Task 10 (the cache, `principalKey`).

- [ ] **Step 1: Write the failing tests** (in-process; a vendor `routing.Target` with `Provider: routing.ProviderVendorAnthropic` pointed at an `httptest` server, as existing vendor tests do).
  - `TestWindowsObservedFromVendorThenAttachedOnLocalPassthrough`: principal P gets a vendor response with windows. A later P request to a self-hosted Messages passthrough whose upstream sends no headers returns P's windows. A different principal Q gets none.
  - `TestWindowsAttachedOnTranslateStreamAndBuffered`: the same, with the second request on the translate path, streaming and buffered.
  - `TestWindowsNotAttachedOnError`: a second request answered 500 gets nothing attached.
  - `TestWindowsNotAttachedOverFreshHeaders`: the second upstream sends its own status → its own headers win, unmodified.
  - `TestWindowsNotObservedForNonEntitled`: a member, with the opt-in off, of another owner's account gets nothing recorded, so nothing is attached later.
  - `TestChatCompletionsGetNoWindows`: an OpenAI chat request of P gets nothing.
- [ ] **Step 2: Run.** `cd gateway/backend && go test ./internal/gateway/ -run 'TestWindows|TestChatCompletionsGetNoWindows' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement** the four call sites.
- [ ] **Step 4: Run** the same command and `go test ./internal/gateway/ -count=1`. Expected: PASS.
- [ ] **Step 5: Docs.** In `compatibility-and-inference.md`, add the new §9.3 "Claude Code usage-limit display". It covers:
  - the client condition (a JWT whose `aud` is `claude-gateway`);
  - the token option;
  - the relay allowlist and entitlement;
  - the window cache (per process, best effort, never `rejected`);
  - what Claude Code shows and does not show (no Desktop usage card, no `/usage` plan panel);
  - the dependency on undocumented client behaviour.

  Link it from §6 and from the token docs.
- [ ] **Step 6: Commit.** `git commit -m "feat(gateway): keep subscription windows on other providers' Messages responses (#407)"`.

## Phase E — decision record and finish

### Task 12: ADR, risk entry, glossary

**Files:**
- Modify:
  - `docs/architecture/09-architecture-decisions.md`: ADR-106 (next free). It records:
    - forward an allowlist rather than everything;
    - a JWT-shaped opt-in token rather than `CLAUDE_CODE_USE_GATEWAY` (Desktop-incompatible, and it loses betas) or the sign-in protocol;
    - entitlement follows the portal's management view, plus the owner opt-in;
    - the window cache.
  - `docs/architecture/11-risks-and-technical-debt.md`: Claude Code's `aud: claude-gateway` detection is undocumented; if it changes, only the display disappears.
  - `docs/architecture/12-glossary.md`: entries for "Claude Code token format" and "share usage with members".
- [ ] **Step 1: Write** the three entries, linking §9.3 and §8.2.
- [ ] **Step 2: Run** `make lint-docs`. Expected: pass.
- [ ] **Step 3: Commit.** `git commit -m "docs(architecture): ADR-106 Claude Code usage-limit display (#407)"`.

Controller-run after Task 12, not a subagent task:
- the end-to-end check from the spec §7 with Claude Code 2.1.328 against a locally started memory-mode gateway and a loopback stub;
- the full gates (both Go modules' `golangci-lint fmt --diff` / `run` / `go test`, the postgres leg, the four frontend gates, `make lint-docs`, `make sonar-scan` with branch-attributed findings);
- the final whole-branch review;
- removing `docs/superpowers/` and confirming with `git diff --name-only main...HEAD`;
- push over SSH, the PR, and enabling Auto-fix.

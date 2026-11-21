# Codex CLI usage-limit display Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Codex CLI users routed through the gateway to an OpenAI subscription account see that subscription's usage windows
(relay plus a window cache), and payload captures never show them to non-entitled principals.

**Architecture:** This applies ADR-106 (#408) to the `openai_responses` flavor. It relays an allowlisted `x-codex-*`
rate-limit family in `proxyNative`, gated by the existing `rateLimitFiguresEntitled`. A second instance of the existing
`rateLimitWindowCache` holds Codex windows per principal, observed in `recordUsage` and attached to header-less
`openai_responses` successes. Capture redaction extends to the family.

**Tech Stack:** Go (backend module `op-ai-gateway`, package `internal/gateway`), Markdown architecture docs.

**Spec:** `docs/superpowers/specs/2026-11-21-codex-usage-limits-design.md` (read it first; §4 is the behavior contract).

## Global Constraints

- **Relay scope:** the Codex family (spec §4.1) on `inference.APIFlavorOpenAIResponses` only.
- **Messages unchanged:** the `/v1/messages` behavior is byte-identical, and the existing `ratelimit_*_test.go` and
  `principal_limit*_test.go` tests pass unchanged. The single exception is `TestNativeResponsesDoesNotForwardRateLimitHeaders`,
  which Task 1 reworks.
- **Never relayed:** `x-codex-turn-state`, request ids, organization ids, `x-ratelimit-*`, and anything outside the family plus
  `retry-after` and `x-should-retry`.
- **Fail closed:** entitlement fails closed; the cache and entitlement code never faults or blocks a request beyond the existing
  2 s lookup bound.
- **Cache bounds:** 10 000 principals, in memory, per process.
- **Logging:** log no header values or tokens.
- **Gates:**
  - from `gateway/backend`: `golangci-lint fmt --diff` and `golangci-lint run`;
  - `go test ./...` from `gateway/backend`;
  - `make lint-docs` from the repo root.
- **Repo text:** in English; source files carry the SPDX/copyright header used by every file in `internal/gateway`.
- **Commits:** frequent, with Conventional-Commit subjects and substantive bodies; never on `main`.

## Review Focus

1. A header in non-canonical case (`X-CODEX-PRIMARY-USED-PERCENT`) or with several values. Relay: every value, under its
   canonical key. Cache: the first value. Pinned in Task 1 and Task 3.
2. A `reset-at` that is fractional, negative or garbage. That window is not cached, and the other windows still are. Pinned in
   Task 3.
3. One principal using Claude Code and Codex in turn. Neither family's cache entry evicts or overwrites the other. Pinned in
   Task 3 and Task 4.
4. A token with no principal (no user id, no service id). Nothing is cached. The relay still goes through entitlement, which
   denies unless the target is self-hosted or shared. Pinned in Task 4.
5. A vendor 429 carrying the family:
   - it is relayed to an entitled caller and observed into the cache;
   - an entitled caller's following local-model success shows the 429's windows;
   - the 429 itself never gets cached windows attached over its own.

   Pinned in Task 1 and Task 4.

---

### Task 1: Codex family predicate and the Responses relay

**Files:**
- Modify: `gateway/backend/internal/gateway/ratelimit_headers.go`
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (relay block before `w.WriteHeader(resp.StatusCode)`,
  currently around line 1146)
- Test: `gateway/backend/internal/gateway/ratelimit_headers_test.go`

**Interfaces:**
- Produces:
  - `func isCodexRateLimitHeader(name string) bool`: the spec §4.1 predicate, case-insensitive.
  - `func hasCodexRateLimitHeader(h http.Header) bool`.
- Changes: `forwardRateLimitHeaders(dst, src http.Header, gated func(name string) bool)`. It always copies `Retry-After`
  and `X-Should-Retry`, plus every header for which `gated != nil && gated(canon)`, with every value. The Messages call site
  passes `isUnifiedRateLimitHeader` when entitled, else `nil`.

- [ ] **Step 1: Write the failing tests**
  - `TestIsCodexRateLimitHeader`, table-driven.
    - True for: `x-codex-primary-used-percent`, `X-Codex-Secondary-Reset-At`, `x-codex-primary-window-minutes`,
      `x-codex-limit-name`, `x-codex-other-primary-used-percent`, `x-codex-other-limit-name`, `x-codex-credits-has-credits`,
      `x-codex-credits-unlimited`, `x-codex-credits-balance`, `x-codex-active-limit`, `x-codex-rate-limit-reached-type`,
      `x-codex-promo-message`.
    - False for: `x-codex-turn-state`, `x-codex-beta-features`, `x-request-id`, `x-ratelimit-remaining-requests`,
      `x-other-primary-used-percent`, `anthropic-ratelimit-unified-status`, `retry-after`.
  - `codexRateLimitSrcHeaders()` fixture with:
    - `X-Codex-Primary-Used-Percent: 42.5`, `X-Codex-Primary-Window-Minutes: 300`, `X-Codex-Primary-Reset-At: 1795290566`;
    - `X-Codex-Secondary-Used-Percent: 17`, `X-Codex-Credits-Balance: 0`;
    - `X-Codex-Turn-State: ts_1`, `X-Request-Id: req_1`, `X-Ratelimit-Remaining-Requests: 50`;
    - `Retry-After: 37`, `X-Should-Retry: false`;
    - `Anthropic-Ratelimit-Unified-Status: allowed`.
  - `TestNativeResponsesForwardsCodexRateLimitHeaders`: cases sse/json/429 as in `TestNativeMessagesForwardsRateLimitHeaders`,
    self-hosted target, flavor `inference.APIFlavorOpenAIResponses`, endpoint `endpointResponses`.
    - Assert the family values (42.5/300/1795290566/17/0) and `Retry-After` 37 / `X-Should-Retry` false are relayed.
    - Assert `X-Codex-Turn-State`, `X-Request-Id`, `X-Ratelimit-Remaining-Requests` and
      `Anthropic-Ratelimit-Unified-Status` are absent.
  - `TestNativeResponsesCodexHeadersNonEntitled`: vendor target `{Provider: routing.ProviderVendorOpenAISubscription,
    VendorAccountID: "acc_x", VendorAccountOwnerUserID: "usr_other"}`, portal `usageManagerPortal{ok: false}`, status 429.
    Assert the family is absent and `Retry-After` / `X-Should-Retry` are present.
  - `TestNativeResponsesCodexHeadersEntitledOwner`: same target with `VendorAccountOwnerUserID: "usr_dev"`; the family is
    present.
  - `TestNativeResponsesWithoutCodexHeadersNeverConsultsPortal`: the analog of the Messages test; portal calls == 0.
  - `TestNativeResponsesCodexHeaderValuesAndCase` (Review Focus 1): the upstream sets `x-codex-primary-used-percent` via
    direct map assignment in lower case, and `X-Codex-Limit-Name` with two values. The client sees
    `X-Codex-Primary-Used-Percent` and both limit-name values.
  - Rework `TestNativeResponsesDoesNotForwardRateLimitHeaders` → rename to `TestNativeResponsesDoesNotForwardUnifiedHeaders`.
    With `rateLimitSrcHeaders()`, `Anthropic-Ratelimit-Unified-*` stays absent on responses, and `Retry-After` 37 is now
    present.
- [ ] **Step 2:** Run `go test ./internal/gateway -run 'Codex|NativeResponses|ForwardRateLimit|NativeMessages' -count=1`
  from `gateway/backend`. Expected: FAIL (undefined functions / missing headers).
- [ ] **Step 3: Implement**
  - Implement the predicates in `ratelimit_headers.go`: lowercase once, then a prefix test plus a suffix or exact-name set.
    Keep the name set in package-level constants or variables next to `unifiedRateLimitPrefix`.
  - Generalize `forwardRateLimitHeaders`.
  - In `proxyNative`, add an `else if rel.pfReq.APIFlavor == inference.APIFlavorOpenAIResponses` branch:
    `forwardRateLimitHeaders(w.Header(), resp.Header, gated)`, where `gated` is `isCodexRateLimitHeader` only when
    `hasCodexRateLimitHeader(resp.Header) && s.rateLimitFiguresEntitled(r.Context(), rel.token, rel.target)`.
- [ ] **Step 4:** Run `go test ./internal/gateway -count=1` from `gateway/backend`. Expected: PASS.
- [ ] **Step 5:** Run `golangci-lint fmt --diff` and `golangci-lint run ./internal/gateway/...` from `gateway/backend`, then
  commit: `feat(gateway): relay the Codex rate-limit headers on the Responses passthrough (#416)`.

### Task 2: Capture redaction covers the Codex family

**Files:**
- Modify: `gateway/backend/internal/gateway/ratelimit_headers.go` (`redactCapturedAccountHeaders`)
- Test: `gateway/backend/internal/gateway/ratelimit_window_paths_test.go` (next to
  `TestCapturedAccountHeadersRedactedForNonEntitled`)

**Interfaces:**
- Consumes: `isCodexRateLimitHeader` (Task 1).
- Produces: the same signature. Its sensitive predicate becomes `isUnifiedRateLimitHeader || isCodexRateLimitHeader ||
  anthropic-organization-id`.

- [ ] **Step 1: Write the failing test** `TestCapturedCodexHeadersRedactedForNonEntitled`. It calls
  `srv.redactCapturedAccountHeaders` directly, as the existing test does, with a vendor OpenAI-subscription target and
  `X-Codex-Primary-Used-Percent`, `X-Codex-Credits-Balance`, `X-Codex-Turn-State`, `Content-Type`.
  - Not entitled: the two family headers are gone; `X-Codex-Turn-State` and `Content-Type` are kept; the input header set is
    unmodified (copy semantics).
  - Entitled (owner): everything is kept.
  - Self-hosted target (no account): returned as is.
- [ ] **Step 2:** Run `go test ./internal/gateway -run CapturedCodex -count=1`. Expected: FAIL.
- [ ] **Step 3:** Implement: factor the sensitive test into `func isAccountSensitiveHeader(name string) bool` and use it in
  both loops.
- [ ] **Step 4:** Run `go test ./internal/gateway -run 'Captured|Redact' -count=1`. Expected: PASS.
- [ ] **Step 5:** Commit: `feat(gateway): redact the Codex rate-limit family from captures of non-entitled principals (#416)`.

### Task 3: Codex windows in the window cache

**Files:**
- Modify: `gateway/backend/internal/gateway/ratelimit_window_cache.go`. Extract the store-and-prune step of `Observe` into
  `func (c *rateLimitWindowCache) put(key string, e cachedRateLimitEntry, now time.Time)`. `Observe` calls it, and nothing
  else in its behavior changes. Add `names map[string]string` to `cachedRateLimitEntry` (nil for Anthropic entries).
- Create: `gateway/backend/internal/gateway/codex_window_cache.go`
- Modify: `gateway/backend/internal/gateway/server.go`: field `codexRateLimitWindows *rateLimitWindowCache` next to
  `rateLimitWindows`, initialized `newRateLimitWindowCache(10000)` where `rateLimitWindows` is.
- Test: `gateway/backend/internal/gateway/codex_window_cache_test.go`

**Interfaces:**
- Consumes: `principalKey`, `Forget`, `liveEntry`, `hasFutureWindow` (existing).
- Produces:
  - `func parseCodexWindows(h http.Header) (windows map[string]cachedRateLimitWindow, names map[string]string, fresh bool)`.
    - Families come from headers matching `x-codex[-<id>]-(primary|secondary)-used-percent`.
    - The window key is `<family>-primary` / `<family>-secondary`, with fields `used-percent`, `window-minutes` and
      `reset-at` (only those present).
    - `reset` comes from `reset-at` via `strconv.ParseInt` and must be > 0. A window without such a reset is skipped.
    - `names[family]` is `<family>-limit-name` when present.
    - `fresh` is true when any `…-used-percent` family header is present.
  - `func (c *rateLimitWindowCache) ObserveCodex(key, accountID string, h http.Header, now time.Time)`. It does nothing
    unless `fresh`; otherwise it `put`s an entry with the parsed windows and names (an empty window set is allowed and is
    pruned like any expired entry).
  - `func (c *rateLimitWindowCache) AttachCodex(key string, dst http.Header, now time.Time) bool`. It writes
    `<window>-<field>` for every window whose reset is after `now`, and `<family>-limit-name` for each family with at least
    one attached window. It writes nothing else: no credits, no 429-only headers, no retry headers. It returns whether
    anything was written.
  - `func (s *Server) observeCodexRateLimitWindows(token auth.Token, target routing.Target, h http.Header)`. It runs only
    for `routing.IsOpenAIVendorProvider(target.Provider)` with a fresh family and a principal key:
    - entitled (`rateLimitFiguresEntitled(context.Background(), …)`) → `ObserveCodex`;
    - not entitled → `Forget(key, target.VendorAccountID)`.
  - `func (s *Server) attachCodexRateLimitWindows(token auth.Token, dst http.Header)`: nil-safe, principal-keyed `AttachCodex`.

- [ ] **Step 1: Write the failing tests** in `codex_window_cache_test.go`, with a fixture
  `codexWindowHeaders(now, primaryOffset, secondaryOffset)` (primary 42.5/300, secondary 17/10080, `x-codex-limit-name: codex`,
  plus `x-codex-credits-balance: 5`, `x-codex-active-limit: codex`, `retry-after: 30`).
  - `TestCodexCacheAttachCopiesFutureWindows`: exact values written, limit-name written.
  - `TestCodexCacheNeverAttachesCreditsOr429Headers`: no credits, active-limit or retry-after on attach.
  - `TestCodexCacheDropsExpiredWindows`: primary past, secondary future → only secondary plus limit-name.
  - `TestCodexCacheAllExpiredPrunes`: attach returns false and the entry is gone.
  - `TestCodexCacheIgnoresObserveWithoutUsedPercent`: a header set with only credits leaves the previous entry intact.
  - `TestCodexCacheSkipsUnparseableReset` (Review Focus 2): `reset-at` `"1795290566.5"`, `"-5"` and `"soon"` on the primary each
    → primary not attached, secondary attached.
  - `TestCodexCacheExtraFamilies`: `x-codex-other-primary-*` plus `x-codex-other-limit-name` round-trip next to the default
    family.
  - `TestCodexCacheFirstValueOfMultiValuedHeader` (Review Focus 1).
  - `TestCodexAndAnthropicEntriesCoexist` (Review Focus 3): Anthropic `Observe` on `s.rateLimitWindows` and `ObserveCodex` on
    `s.codexRateLimitWindows` for the same key → both attach their own family, and `Attach` writes no `x-codex-*`.
  - `TestCodexCacheCapAndForget`: the cap evicts the oldest; `Forget` drops only an entry from that account.
  - Existing `ratelimit_window_cache_test.go` runs unchanged.
- [ ] **Step 2:** Run `go test ./internal/gateway -run 'CodexCache|CodexAndAnthropic|WindowCache' -count=1`. Expected: FAIL
  (undefined).
- [ ] **Step 3:** Implement as in the Interfaces block.
- [ ] **Step 4:** Run `go test ./internal/gateway -count=1`. Expected: PASS.
- [ ] **Step 5:** Lint as in Task 1, then commit: `feat(gateway): cache a principal's Codex usage windows (#416)`.

### Task 4: Observe and attach on the request paths

**Files:**
- Modify: `gateway/backend/internal/gateway/inference_complete.go`:
  - `recordUsage`: call `s.observeCodexRateLimitWindows(token, target, upstreamHeaders)` right after
    `s.observeRateLimitWindows(...)`.
  - The buffered success: `if req.APIFlavor == inference.APIFlavorOpenAIResponses { s.attachCodexRateLimitWindows(token,
    w.Header()) }`, next to the Messages branch.
- Modify: `gateway/backend/internal/gateway/stream_session.go`: the same branch in the SSE header block, before
  `WriteHeader`.
- Modify: `gateway/backend/internal/gateway/native_passthrough.go`: in the Task 1 Responses branch, on a 2xx with
  `!hasCodexRateLimitHeader(resp.Header)`, call `s.attachCodexRateLimitWindows(rel.token, w.Header())` after the relay
  call.
- Test: create `gateway/backend/internal/gateway/codex_window_paths_test.go`

**Interfaces:**
- Consumes: Task 1 and Task 3 functions.
- Test fixture:
  - `seedSubscriptionOpenAIRoutableAccount(t, routes, cipher, accountID, ownerID string, ts vendorauth.TokenSet)`, the analog
    of `seedSubscriptionAnthropicRoutableAccount`, with `routing.VendorOpenAI`, `AuthType: routing.VendorAuthSubscription`,
    model `oai/gpt-5.5` → upstream `gpt-5.5`, `APIFlavor: routing.APIFlavorOpenAI`.
  - Reuse `newVendorAccountSettingsTestServer`, `seedWindowsLocalModel`, `vaOwnerSecret`/`vaOtherSecret`.
  - A provider whose `ProxyNative` answers `routing.IsOpenAIVendorProvider(target.Provider)` targets with the Codex fixture
    headers, and other targets with `localStatus`/`localHdr`.

- [ ] **Step 1: Write the failing tests**
  - `TestCodexWindowsObservedFromVendorThenAttachedOnLocalPassthrough`: the owner calls `/v1/responses` on the vendor model,
    then on the local model with `ResponsesMode` passthrough. The local response carries the family windows (`42.5`, reset,
    limit-name) and no credits. `vaOtherSecret` gets none.
  - `TestCodexWindowsAttachedOnTranslateStreamAndBuffered`: the local app in translate mode for responses (the
    `seedWindowsLocalModel` default), buffered and stream.
  - `TestCodexWindowsNotAttachedOnErrorOrOverFresh`:
    - local 500 → none;
    - local 2xx carrying its own `x-codex-primary-used-percent: 5` → client sees `5`, not `42.5`.
  - `TestCodexWindowsNotObservedForNonEntitledAndForgottenOnRevocation`: a member's request to the owner's account
    (`share_usage_with_members` off) observes nothing. After the owner turns sharing on then off again, the next vendor
    response drops the member's entry. Mirror `TestWindowsForgottenWhenEntitlementRevoked`.
  - `TestCodexWindowsObservedFrom429` (Review Focus 5): the vendor answers 429 with the family and primary `100`. The client
    gets the 429 with the family (owner). The next local success shows `100`.
  - `TestCodexWindowsNeedAPrincipal` (Review Focus 4): call `observeCodexRateLimitWindows` with `auth.Token{ID: "t"}` → the
    cache stays empty.
  - `TestMessagesGetNoCodexWindowsAndResponsesGetNoUnified`: after both families are observed, a local `/v1/messages` carries
    no `x-codex-*`, and a local `/v1/responses` carries no `anthropic-ratelimit-unified-*`.
  - `TestTranslatedCaptureRedactsCodexHeadersThroughRequestPath`: the analog of
    `TestTranslatedCaptureRedactsAccountHeadersThroughRequestPath`, for portal-chat-style `/v1/chat/completions` translate to
    the OpenAI subscription account, if the existing analog's harness allows. Otherwise the Task 2 unit test stands; note
    that in the commit body.
- [ ] **Step 2:** Run `go test ./internal/gateway -run 'CodexWindows|TranslatedCaptureRedactsCodex|MessagesGetNoCodex' -count=1`.
  Expected: FAIL.
- [ ] **Step 3:** Implement the three attach sites and the observe call.
- [ ] **Step 4:** Run `go test ./... -count=1` from `gateway/backend`. Expected: PASS. Known unrelated flakes:
  `TestResolveEmitsSpan` at `-count>=2`, `TestVRAMRunUnstable…` under load.
- [ ] **Step 5:** Lint, then commit: `feat(gateway): keep Codex usage windows across local-model answers (#416)`.

### Task 5: Architecture docs

**Files (all under `docs/architecture/`):**
- `09-architecture-decisions.md`:
  - Add a new `## ADR-108 — Codex shows an OpenAI subscription's usage limits through an allowlisted x-codex relay and a
    per-principal window cache` after ADR-107, covering Context (verified Codex 0.137.0 behavior, #416), Decision (spec §4),
    Rejected (whole `x-codex-` prefix, a token opt-in, relaying `x-codex-turn-state`) and Consequences.
  - In ADR-106's Rejected list, point the Codex bullet to ADR-108.
- `cross-cutting/compatibility-and-inference.md`:
  - In §6, after *Response headers on the Messages passthrough*, add the *Response headers on the Responses passthrough*
    paragraph.
  - Add a new `### 9.5 Codex usage-limit display`: client behavior, relay, window cache, captures, what Codex shows, the model
    nudge and `notices.hide_rate_limit_model_nudge`, and the dependency.
  - Keep the §9 intro/TOC consistent if it lists subsections.
- `cross-cutting/security-auth-rbac.md`: in the capture response-header exception sentence (around line 1268), add the Codex
  family.
- `cross-cutting/external-vendor-accounts.md`: in the OpenAI subscription passthrough description, the response relay
  allowlist (find the M5b passthrough paragraph that says the response is relayed unchanged).
- `11-risks-and-technical-debt.md`: in §11.1, add a row "Codex's rate-limit header reading is undocumented".
- `reference/api-surface.md`: in the `/v1/responses` row, if it lists passthrough response behavior, mirror the Messages row's
  wording.

- [ ] **Step 1:** Write the edits; every claim must match the code from Tasks 1–4 (names, conditions, bounds).
- [ ] **Step 2:** Run `make lint-docs` from the repo root. Expected: `docs OK` (exit 0).
- [ ] **Step 3:** Commit: `docs(architecture): ADR-108 and §9.5 for the Codex usage-limit display (#416)`.

### Task 6 (controller): End-to-end with the Codex CLI

Not a subagent task. Build the backend and run a memory-mode gateway on loopback. Register the stub
(`/tmp/codex-research/scripts/stub.py`) as a self-hosted `vllm` app serving model `stub-codex` with `ResponsesMode`
passthrough, and create a user token. Run `codex` 0.137.0 (isolated `CODEX_HOME`, `env -i`, custom provider
`base_url = http://127.0.0.1:<gw>/v1`) in tmux. Expected:

- the status line shows `5h 57% left · weekly 83% left`;
- `/status` shows both bars.

Record the results in the notes folder and the PR body.

### Task 7 (controller): Gates, cleanup, PR

- Run the full gates (Global Constraints), and Sonar if Docker is up (ask before starting Docker Desktop).
- Fold durable content into `docs/architecture/` (Task 5), then delete `docs/superpowers/` and
  `docs/implementation-status.md`; check with `git diff --name-only main...HEAD`.
- Push over SSH, open the PR with `Closes #416`, and enable Auto-fix.

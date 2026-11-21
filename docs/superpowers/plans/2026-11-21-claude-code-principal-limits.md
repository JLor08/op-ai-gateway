# Claude Code Principal Limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Report OP AI Gateway's own request quota, token quota and cost budget to Claude Code on `/v1/messages` as the
documented `overage` usage-limit headers and a `429 billing_error`. Every other endpoint gets `x-should-retry: false` on its
quota and budget denials.

**Architecture:**

- **Readings at admission:** the limiter's admission returns one reading per configured quota or budget (its `Admission.Standing`).
- **Block:** a `/v1/messages` quota or budget denial writes the contract 429 from those readings.
- **Success:** the readings travel in the request context to the three #407 attach points, which overlay the success headers
  on top of the subscription windows. The response path does no store I/O.

**Tech Stack:** Go 1.26, `net/http`, the existing `internal/gateway` and `internal/routing` packages, standard `testing` + `httptest`.

**Spec:** `docs/superpowers/specs/2026-11-21-claude-code-principal-limits-design.md` (read it; this plan argues from it).

## Global Constraints

- Header family prefix `anthropic-ratelimit-unified-` (constant `unifiedRateLimitPrefix`, `ratelimit_headers.go`).
- **No headers for a principal without a configured quota or budget.** For such a principal, `/v1/messages` is byte-identical to today.
- **No store I/O on the response path.** Readings come only from admission.
- **Stable codes stay:** `limit.request_quota_exceeded`, `limit.token_quota_exceeded`, `limit.cost_budget_exceeded`,
  `limit.rate_limited` (constants in `inference_handlers.go`).
- **The rate-limit denial is unchanged everywhere** (429 + `Retry-After`, no `X-Should-Retry`).
- **Other endpoints keep status and body;** quota and budget denials only gain `X-Should-Retry: false`.
- **Block message, exact copy:** `<label> reached · resets <Jan 2, 15:04> UTC · ask your gateway administrator to raise it`.
  Labels are `request quota`, `token quota` and `cost budget`. `·` is U+00B7.
- **Untouched:** the server agent (no version bump), the frontend and the store schema.
- **Per Go module before each commit:** `golangci-lint fmt --diff` and `golangci-lint run` (gofumpt, gocritic); CI runs both.
- Every new Go file starts with the SPDX header used in the package (`// SPDX-License-Identifier: AGPL-3.0-only` + copyright line).

## Review Focus

1. **Float formatting:** 29 of 100 must read `0.29` and 82 of 100 `0.82`, not `0.28` / `0.81` from `floor(0.29*100)`. Task 3,
   `TestOverageUtilizationValueFloatEdges`.
2. **A capped principal's subscription success** whose upstream already carries `-overage-status: rejected`,
   `-overage-disabled-reason`, `-representative-claim: five_hour` and `-fallback-percentage`: all replaced or removed, while the
   5h/7d windows stay. Task 3, `TestStandingOverlayReplacesSubscriptionTopLevel`.
3. **An hour-only quota block:** no `-overage-period` header, and `Retry-After` under 3600. Task 2,
   `TestWriteLimitBlockedHourPeriod`.
4. **A week limit read in the last days of December:** the reset is the following Monday, from a Sunday and across the year.
   Task 1, `TestPeriodEnd` cases.
5. **A capped principal on `/v1/chat/completions` and `/v1/responses`:** no unified headers. The flavor gate holds. Task 3,
   `TestStandingNotAttachedToOtherFlavors`.

---

### Task 1: Admission readings

**Files:**
- Modify: `gateway/backend/internal/routing/limit_period.go` (add `PeriodEnd`)
- Modify: `gateway/backend/internal/routing/limit_period_test.go`
- Modify: `gateway/backend/internal/gateway/principal_limits.go` (`aggregate` gains `ok`; add `Evaluate`; `Admit` wraps it)
- Create: `gateway/backend/internal/gateway/principal_limit_standing.go` (types + `Fullest` / `Blocking`)
- Create: `gateway/backend/internal/gateway/principal_limit_standing_test.go`
- Modify: `gateway/backend/internal/gateway/principal_limits_test.go` (Evaluate tests next to the Admit ones)

**Interfaces:**
- Produces:
  - `routing.PeriodEnd(period string, now time.Time) time.Time`
  - `type LimitKind string` with constants `LimitKindRequestQuota = "request_quota"`, `LimitKindTokenQuota = "token_quota"`,
    `LimitKindCostBudget = "cost_budget"` (deliberately equal to `Admit`'s reason strings)
  - `type LimitReading struct { Kind LimitKind; Used, Cap float64; Period string; Reset time.Time; Failed bool }`
  - `func (r LimitReading) Utilization() float64` (`Used / Cap`)
  - `type LimitStanding []LimitReading` (configured readings in the fixed order request, token, cost; nil or empty = none
    configured, or not readable)
  - `func (s LimitStanding) Fullest() (LimitReading, bool)`
  - `func (s LimitStanding) Blocking(fallback LimitKind) (LimitReading, bool)`
  - `type Admission struct { Allow bool; Reason string; RetryAfter time.Duration; At time.Time; Standing LimitStanding }`
  - `func (l *PrincipalLimiter) Evaluate(ctx context.Context, p Principal) Admission`

- [ ] **Step 1: Write failing `TestPeriodEnd` in `routing/limit_period_test.go`** — table test, the start of the next period
  (UTC):
  - `hour`, 2026-03-15 13:47:22 → 2026-03-15 14:00:00
  - `day`, 2025-12-31 23:59:59 → 2026-01-01 00:00:00
  - `week`, Sunday 2026-12-27 10:00 → Monday 2026-12-28 00:00. 2026-12-27 is a Sunday; verify it with `date -u -d 2026-12-27 +%A`
    before committing the case.
  - `week`, Thursday 2026-12-31 10:00 → Monday 2027-01-04 00:00
  - `month`, 2026-12-31 23:59:59 → 2027-01-01 00:00
  - `month`, 2026-01-31 12:00 → 2026-02-01 00:00
  - `""` and `"fortnight"` → zero `time.Time`
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/routing -run TestPeriodEnd` — expected FAIL (undefined: PeriodEnd).
- [ ] **Step 3: Implement `PeriodEnd`.** Take `PeriodStart` and add 1h, 1 day (`AddDate(0,0,1)`), 7 days or 1 month
  (`AddDate(0,1,0)`). Return zero for an unknown period.
- [ ] **Step 4: Run** the same test — PASS.
- [ ] **Step 5: Write failing tests in `principal_limit_standing_test.go`:**
  - `TestStandingFullestPicksHighestUtilization`: readings request 50/100, token 900/1000, cost 0.2/1 → token.
  - `TestStandingFullestTieBreaks`: equal utilization 0.5; one week reset, one day reset → the week reading. With equal reset
    too → the earlier slice position.
  - `TestStandingFullestRefusesFailedOrEmpty`: nil → `ok=false`; any reading `Failed` → `ok=false`.
  - `TestStandingBlockingLatestReset`: request breached (day reset), cost breached (month reset), token not breached →
    cost. `Failed` readings are never "breached".
  - `TestStandingBlockingFallback`: no reading breached, fallback `LimitKindTokenQuota` present → that reading (even when
    `Failed`). Fallback kind absent → `ok=false`.
- [ ] **Step 6: Run** `go test ./internal/gateway -run TestStanding` — FAIL (undefined types).
- [ ] **Step 7: Implement the types, `Utilization`, `Fullest` and `Blocking`** in `principal_limit_standing.go`, following
  spec §5 and §6.
- [ ] **Step 8: Run** the standing tests — PASS.
- [ ] **Step 9: Write failing Evaluate tests in `principal_limits_test.go`** (use `fakePrincipalStore`, `clockAt`, `utc`):
  - `TestPrincipalLimiterEvaluateStandingOnAllow`:
    - config RequestQuota 100/day, TokenQuota 1000/week, CostBudget 5/month;
    - aggregate 10 requests, 300 tokens, 1.25 cost;
    - now Wednesday 2026-03-18 09:00.
    - Expect `Allow`, and `Standing` of 3 readings in order request, token, cost, with `Used` 10 / 300 / 1.25, `Cap`
      100 / 1000 / 5, `Reset` 2026-03-19 00:00 / 2026-03-23 00:00 / 2026-04-01 00:00, none `Failed`; and `At == now`.
  - `TestPrincipalLimiterEvaluateStandingOnQuotaDeny`: RequestQuota 10/day breached (10 requests) and CostBudget 1/month
    breached. Expect `Allow=false`, `Reason="request_quota"`, and `Standing` with both readings (all configured limits read).
  - `TestPrincipalLimiterEvaluateNoStandingWithoutConfig`: no row → `Allow`, `Standing == nil`. A rate-only config also gives
    `Standing == nil`.
  - `TestPrincipalLimiterEvaluateRateDenyHasNoStanding`: `Reason="rate"`, `RetryAfter > 0`, `Standing == nil`.
  - `TestPrincipalLimiterEvaluateAggregateErrorMarksFailed`: `aggErr` set → `Allow` (fail open, as today), every reading
    `Failed`.
  - `TestPrincipalLimiterEvaluateConfigErrorNoStanding`: `cfgErr` set → `Allow`, `Standing == nil`.
- [ ] **Step 10: Run** `go test ./internal/gateway -run TestPrincipalLimiterEvaluate` — FAIL.
- [ ] **Step 11: Implement `Evaluate` and make `Admit` its wrapper** (`a := l.Evaluate(ctx, p); return a.Allow, a.Reason, a.RetryAfter`):
  - Change `aggregate` to return `(requests, tokens int64, cost float64, ok bool)`, with `ok=false` on a store error. Callers
    that keep today's fail-open semantics (`spendBreach`, so `Check`) ignore it.
  - In `Evaluate`: after the rate check, read every configured limit (threshold > 0 and period != "") into a reading. Then
    check breaches in today's order (request, token, cost). A `Failed` reading is never breached. The reason is the first
    breach.
  - `At` is `l.now()` taken once.
  - Keep the nil-safety, the zero-config short-circuit and the config fail-open exactly as `Admit` has them.
- [ ] **Step 12: Run** `go test ./internal/gateway -run 'TestPrincipalLimiter|TestStanding|TestPeriod' && go test ./internal/routing` —
  all PASS, including every pre-existing Admit/Check test unchanged.
- [ ] **Step 13: Lint and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff ./... && golangci-lint run ./internal/gateway/... ./internal/routing/...
git add internal/routing/limit_period.go internal/routing/limit_period_test.go internal/gateway/principal_limits.go internal/gateway/principal_limits_test.go internal/gateway/principal_limit_standing.go internal/gateway/principal_limit_standing_test.go
git commit -m "feat(gateway): principal limiter returns its quota and budget readings at admission (#413)"
```

### Task 2: The block and `x-should-retry`

**Files:**
- Create: `gateway/backend/internal/gateway/principal_limit_headers.go` (`writeLimitBlocked`, `limitBlockedMessage`, period mapping)
- Create: `gateway/backend/internal/gateway/principal_limit_headers_test.go`
- Modify: `gateway/backend/internal/gateway/inference_handlers.go`:
  - `admitPrincipal` becomes flavor-aware and returns the standing;
  - `inferencePreflight` passes `shape.apiFlavor` and stores the standing in `preflight.Standing`;
  - `writeLimitDenied` adds `X-Should-Retry`.
- Modify: `gateway/backend/internal/gateway/principal_limits_wiring_test.go`

**Interfaces:**
- Consumes (Task 1): `Admission`, `LimitStanding.Blocking`, `LimitReading`, `LimitKind*`, `(*PrincipalLimiter).Evaluate`.
- Produces:
  - `func writeLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time)`
  - `func limitBlockedMessage(r LimitReading) string`
  - `func (s *Server) admitPrincipal(w http.ResponseWriter, r *http.Request, token auth.Token, apiFlavor string) (standing LimitStanding, handled bool)`
  - `preflight.Standing LimitStanding` (new field)

- [ ] **Step 1: Write failing unit tests in `principal_limit_headers_test.go`:**
  - `TestWriteLimitBlockedDailyTokenQuota`:
    - **Input:** reading token 1000/1000, period `day`, reset 2026-11-22 00:00 UTC; now 2026-11-21 13:30:00 UTC.
    - **Status and body:** status 429; body JSON `{"type":"error","error":{"type":"billing_error","code":"limit.token_quota_exceeded","message":"token quota reached · resets Nov 22, 00:00 UTC · ask your gateway administrator to raise it"}}`.
    - **Headers present:** `Retry-After` `37800`; `X-Should-Retry` `false`; `-status` `rejected`; `-reset` and `-overage-reset`
      the reset's unix seconds; `-overage-utilization` `1`; `-overage-surpassed-threshold` `1`; `-overage-period` `daily`;
      `-overage-disabled-reason` `org_spend_cap_reached`.
    - **Headers absent:** `-representative-claim` and `-overage-status`.
  - `TestWriteLimitBlockedPeriods`: `week` → `weekly`, `month` → `monthly`; cost reading → code
    `limit.cost_budget_exceeded`, label `cost budget`; request reading → `limit.request_quota_exceeded`, label `request quota`.
  - `TestWriteLimitBlockedHourPeriod`: period `hour`, reset 14:00, now 13:59:59.4 → no `-overage-period` header,
    `Retry-After` `1` (rounded up, at least 1).
- [ ] **Step 2: Run** `go test ./internal/gateway -run TestWriteLimitBlocked` — FAIL.
- [ ] **Step 3: Implement `writeLimitBlocked` and `limitBlockedMessage`** to spec §6:
  - The body is a small local struct with top-level `type`, so it is not `apierror.Body`.
  - `Retry-After` = `ceil(r.Reset.Sub(now).Seconds())`, at least 1.
  - The message time uses `r.Reset.UTC().Format("Jan 2, 15:04")`.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Extend the `writeLimitDenied` tests:**
  - `TestWriteLimitDeniedRequestQuota`, `TestWriteLimitDeniedTokenQuota` and `TestWriteLimitDeniedCostBudget` additionally
    assert `X-Should-Retry` == `false`; status, code and absent `Retry-After` are unchanged.
  - `TestWriteLimitDeniedRate` additionally asserts `X-Should-Retry` is absent.
- [ ] **Step 6: Write failing wiring tests in `principal_limits_wiring_test.go`.** Follow
  `TestPrincipalLimiterCostBudgetDenies`: `newServiceScopeTestServer`, service token `svc-secret`, `fakePrincipalStore`, fixed
  clock.
  - `TestMessagesQuotaDenialIsBillingError`: TokenQuota 100/day with 100 tokens used. POST `/v1/messages` (model `qwen-coder`)
    → 429, body `error.type` `billing_error`, `error.code` `limit.token_quota_exceeded`, `X-Should-Retry` `false`, `-status`
    `rejected`, `Retry-After` > 0. No usage event recorded.
  - `TestMessagesCostBudgetDenialIs429`: CostBudget 1/month with 1.0 cost → 429 (not 402), code `limit.cost_budget_exceeded`,
    `-overage-period` `monthly`.
  - `TestMessagesBlockDescribesLatestReset`: TokenQuota 100/day and CostBudget 1/month, both breached → the 429 describes the
    cost budget (`monthly`, cost code).
  - `TestChatQuotaDenialKeepsShapeAddsNoRetry`: same TokenQuota breach on `/v1/chat/completions` → 429, `apierror` body (no
    top-level `type`), code `limit.token_quota_exceeded`, `X-Should-Retry` `false`, no `-status`. The cost-budget twin → 402.
  - `TestMessagesRateDenialUnchanged`: RateRequests 1/60 s, second `/v1/messages` → 429 `limit.rate_limited` with
    `Retry-After`, no `X-Should-Retry`, no unified header.
- [ ] **Step 7: Run** `go test ./internal/gateway -run 'TestMessages|TestChatQuota|TestWriteLimitDenied'` — FAIL.
- [ ] **Step 8: Wire it in `inference_handlers.go`:**
  - **`admitPrincipal`** calls `s.Limiter.Evaluate`. On allow it returns `(a.Standing, false)`.
  - **On a denial with apiFlavor `inference.APIFlavorAnthropicMessages` and a reason other than `rate`:** if
    `a.Standing.Blocking(LimitKind(a.Reason))` is ok, call `writeLimitBlocked(w, reading, a.At)`. Otherwise fall back to
    `writeLimitDenied`.
  - **`writeLimitDenied`** sets `X-Should-Retry: false` for every reason except `rate`.
  - **`inferencePreflight`** passes `shape.apiFlavor` and returns `pf.Standing`.
  - **Comments:** update the doc comments on `admitPrincipal` and `writeLimitDenied`, which name the status mapping.
- [ ] **Step 9: Run** `go test ./internal/gateway` — all PASS.
- [ ] **Step 10: Lint and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff ./... && golangci-lint run ./internal/gateway/...
git add internal/gateway/principal_limit_headers.go internal/gateway/principal_limit_headers_test.go internal/gateway/inference_handlers.go internal/gateway/principal_limits_wiring_test.go
git commit -m "feat(gateway): /v1/messages answers a quota or budget block with 429 billing_error; x-should-retry false elsewhere (#413)"
```

### Task 3: Success headers on `/v1/messages`

**Files:**
- Modify: `gateway/backend/internal/gateway/principal_limit_headers.go`:
  - add `overageUtilizationValue`, `writeOverageStanding`, the context helpers and `attachPrincipalLimitStanding`;
  - add `(*Server).attachTranslatedUsageLimits`, which bundles #407's `attachRateLimitWindows` with the standing overlay for the
    two translate points.
- Modify: `gateway/backend/internal/gateway/inference_handlers.go` (`handleAnthropicMessages` puts `pf.Standing` into the
  request context after each successful `inferencePreflight`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go:1146-1157` (overlay on a 2xx, after the #407 block)
- Modify: `gateway/backend/internal/gateway/inference_complete.go:111-113` and `gateway/backend/internal/gateway/stream_session.go:142-144`
  (call `attachTranslatedUsageLimits` in place of `attachRateLimitWindows`)
- Modify: `gateway/backend/internal/gateway/principal_limit_headers_test.go`
- Create: `gateway/backend/internal/gateway/principal_limit_paths_test.go` (end-to-end attach tests; reuse `newWindowsFixture`,
  `windowHeaders`, `rlp`, `vaOwnerSecret` from `ratelimit_window_paths_test.go` / `ratelimit_window_cache_test.go`)

**Interfaces:**
- Consumes (Task 1/2): `LimitStanding.Fullest`, `LimitReading`, `preflight.Standing`.
- Produces:
  - `func overageUtilizationValue(u float64) string`
  - `func writeOverageStanding(dst http.Header, r LimitReading)`
  - `func withLimitStanding(ctx context.Context, s LimitStanding) context.Context`
  - `func limitStandingFrom(ctx context.Context) LimitStanding`
  - `func attachPrincipalLimitStanding(ctx context.Context, dst http.Header)` (no-op unless `Fullest` is ok)
  - `func (s *Server) attachTranslatedUsageLimits(r *http.Request, token auth.Token, dst http.Header)`

- [ ] **Step 1: Write failing unit tests in `principal_limit_headers_test.go`:**
  - `TestOverageUtilizationValueFloatEdges`:
    - 82/100 → `0.82`; 29/100 → `0.29`; 57/100 → `0.57`;
    - 0.749 → `0.74`; 0.75 → `0.75`; 0.999 → `0.99`; 1.2 → `0.99`; 0 → `0.00`.
    - Floor with a small epsilon (`math.Floor(u*100 + 1e-9)`) so binary fractions such as 0.29 do not drop a cent.
  - `TestWriteOverageStandingBelowThreshold` (dst empty, reading 50/100, reset R):
    - present: `-overage-utilization` `0.50`, `-overage-status` `allowed`, `-status` `allowed`, `-representative-claim`
      `overage`, and `-reset` = `-overage-reset` = R unix;
    - absent: `-overage-surpassed-threshold` and `-overage-period`.
  - `TestWriteOverageStandingThresholds`: 0.80 → threshold `0.75`, both statuses `allowed_warning`; 0.96 → `0.95`.
  - `TestStandingOverlayReplacesSubscriptionTopLevel`:
    - **Input:** `windowHeaders(now, 1h, 24h)` plus `-overage-status: rejected`, `-overage-disabled-reason: org_level_disabled`,
      `-fallback-percentage: 0.5`, `-upgrade-paths: x`; then the overlay with reading 10/100.
    - **Replaced:** `-representative-claim` `overage`; `-overage-status` `allowed`; `-reset` = the reading's reset.
    - **Removed:** `-overage-disabled-reason`, `-fallback-percentage` and `-upgrade-paths`.
    - **Kept unchanged:** every `-5h-*` / `-7d-*` field, including `7d-surpassed-threshold`.
  - `TestStandingStatusMergesSubscriptionWarning`: dst with `-5h-status: allowed_warning`, reading 10/100 → `-status`
    `allowed_warning`, `-overage-status` `allowed`.
  - `TestAttachPrincipalLimitStandingNoop`: a context without standing, with an empty standing, or with a `Failed` reading
    → dst unchanged (compare against a clone).
- [ ] **Step 2: Run** `go test ./internal/gateway -run 'TestOverage|TestWriteOverage|TestStanding|TestAttachPrincipal'` — FAIL.
- [ ] **Step 3: Implement `overageUtilizationValue`, `writeOverageStanding`, the context helpers and
  `attachPrincipalLimitStanding`** to spec §5 and §8.
  - **Keep set:** the per-window fields of `5h`, `7d` and `7d_oi` (`utilization`, `reset`, `status`, `surpassed-threshold`).
    Match names case-insensitively, as `isUnifiedRateLimitHeader` does.
  - **Remove** every other unified header, then write the §5 set.
  - **The threshold comes from the formatted value**, so `0.75` written always carries threshold `0.75`.
- [ ] **Step 4: Run** — PASS.
- [ ] **Step 5: Write failing path tests in `principal_limit_paths_test.go`.** Each test builds `newWindowsFixture` and
  replaces `f.srv.Limiter` with a `fakePrincipalStore` limiter. Config for `Principal{Type: routing.PrincipalTypeUser, ID: "usr_va_a"}`:
  TokenQuota 1000/day, with 800 tokens used.
  - `TestStandingAttachedOnLocalPassthrough` (`EndpointModePassthrough`, POST `/v1/messages` `windowsLocalModel`): 200,
    `-overage-utilization` `0.80`, `-overage-surpassed-threshold` `0.75`, `-status` `allowed_warning`.
  - `TestStandingAttachedOnTranslateStreamAndBuffered` (`EndpointModeTranslate`, stream false and true): the same three values.
  - `TestStandingWithSubscriptionRelay`:
    - **Setup:** `f.prov.vendorHdr = subscriptionWindowHeaders()` with `-overage-status: rejected` added; POST `/v1/messages`
      `subMessagesModel` as `vaOwnerSecret`.
    - **Expected:** the 5h/7d windows are present, the gateway's top level is present, and the upstream `-overage-status` is gone.
    - **Unchanged #407 behavior:** a later local request still gets the cached windows; the cache observed the upstream set,
      not the overlay.
  - `TestStandingWithCachedWindows`: `observeViaVendor`, then a local request → windows from the cache plus the gateway's
    top level.
  - `TestStandingNotAttachedToOtherFlavors`: `/v1/chat/completions` (and `/v1/responses` if the fixture routes it) for the
    capped principal → `requireNoUnified`.
  - `TestStandingAbsentWithoutLimits`: no config → the local passthrough response matches #407 exactly, via
    `requireNoUnified` when nothing is cached.
  - `TestStandingNotAttachedOnError`: `f.prov.localStatus = 500` on passthrough → no gateway headers.
- [ ] **Step 6: Run** `go test ./internal/gateway -run 'TestStanding'` — FAIL.
- [ ] **Step 7: Wire the attach points:**
  - **Context:** in `handleAnthropicMessages`, after each successful `inferencePreflight`,
    `r = r.WithContext(withLimitStanding(r.Context(), pf.Standing))`.
  - **Native passthrough:** after the #407 block, inside the Anthropic-flavor branch, on a 2xx,
    `attachPrincipalLimitStanding(r.Context(), w.Header())`.
  - **Translate buffered JSON and SSE header block:** `s.attachTranslatedUsageLimits(r, token, w.Header())`.
  - **Overlay targets `w.Header()` only**, never `resp.Header` or the capture sink's set.
- [ ] **Step 8: Run** `go test ./internal/gateway` — all PASS, including every `TestWindows*` from #407 unchanged.
- [ ] **Step 9: Lint and commit**

```bash
cd gateway/backend && golangci-lint fmt --diff ./... && golangci-lint run ./internal/gateway/...
git add internal/gateway/principal_limit_headers.go internal/gateway/principal_limit_headers_test.go internal/gateway/principal_limit_paths_test.go internal/gateway/inference_handlers.go internal/gateway/native_passthrough.go internal/gateway/inference_complete.go internal/gateway/stream_session.go
git commit -m "feat(gateway): /v1/messages reports the principal's fullest quota or budget as the overage window (#413)"
```

### Task 4: Architecture documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md`:
  - add **ADR-107** after ADR-106, in the same structure as its neighbors (context, decision, consequences);
  - link it from ADR-106.
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md`:
  - new **§9.4** "Claude Code display of the gateway's own limits", after §9.3, covering spec §2 and §5–§8;
  - §9.3's opening sentence links §9.4;
  - the error-code table row for `limit.cost_budget_exceeded` (402) and the quota rows gain the `/v1/messages` 429
    `billing_error` variant and the `X-Should-Retry: false` note.
- Modify: `docs/architecture/reference/api-surface.md`:
  - the `/v1/messages` row's refusals;
  - the limit-code rows (`X-Should-Retry`; `/v1/messages` 429 `billing_error` for both quotas and the budget).
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md`. Next to the `PrincipalLimiter.Check`
  paragraph, describe `Evaluate`:
  - readings at admission;
  - `Failed` readings, which mean no display, while admission still fails open;
  - no store I/O on the response path.
- Modify: `docs/architecture/11-risks-and-technical-debt.md`:
  - the client's fixed window order (a warning subscription window hides the overage warning);
  - the memory driver reporting 0 %;
  - the undocumented client dependency, next to #407's existing entry if there is one.
- Modify: `docs/architecture/cross-cutting/security-auth-rbac.md` only if §12's limit text names the 402 or the
  `Retry-After` rule; check it, and change nothing else.

- [ ] **Step 1: Write the docs.**
  - **Source:** the spec, with English prose in the documents' existing style.
  - **Record verified facts as facts:** Claude Code 2.1.328, JWT vs gateway sign-in vs opaque, and the retry loop on the old 429.
  - **Name every code identifier exactly as implemented:** check `git diff main...HEAD -- gateway/backend` for the final names.
- [ ] **Step 2: Run** `make lint-docs` from the repo root — PASS (fix links and anchors it reports).
- [ ] **Step 3: Commit**

```bash
git add docs/architecture
git commit -m "docs(architecture): gateway's own limits in Claude Code, ADR-107 (#413)"
```

## After the tasks (controller, not a subagent task)

- **Full gates:**
  - `cd gateway/backend && go test ./...` and `golangci-lint fmt --diff` / `run`;
  - `make lint-docs`;
  - the frontend gates only if a frontend file changed (none planned).
- **End to end against real Claude Code 2.1.328** (spec §10), sqlite-mode gateway built from the branch:
  - a quota near its cap → `rate_limit_event` with `rateLimitType: overage`;
  - a breached quota → the verbatim sentence after one request.
- **Sonar branch findings** when the environment allows (it needs Docker Desktop; ask the owner first).
- **Cleanup:** fold any remaining durable content into `docs/architecture/`, delete `docs/superpowers/`, and check
  `git diff --name-only main...HEAD`.
- **Ship:** push over SSH, open the PR (English text, substantive commit bodies for the squash), and turn Auto-fix on.

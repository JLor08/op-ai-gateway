# Gateway's own principal limits in the Codex CLI — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On `/v1/responses`, report a principal's fullest gateway limit in the Codex CLI's default limit family and answer a quota or budget block with a 429 `usage_limit_reached` that Codex prints as one sentence after one request.

**Architecture:** ADR-107's readings (`Admission.Standing`, carried on the request context) feed two new writers in `codex_principal_limit_headers.go`: a success writer laid over the client header set at the three `/v1/responses` attach points (after #417's relay and cache, yielding to any Codex figure already there), and a block writer called from `admitPrincipal` for the `openai_responses` flavor. No store I/O on the response path; nothing else changes.

**Tech Stack:** Go (`gateway/backend`, package `internal/gateway`), `net/http`, `httptest`; docs in `docs/architecture/`.

**Spec:** `docs/superpowers/specs/2026-11-21-codex-principal-limits-design.md` (read it first; §5 and §6 hold every header value).

## Global Constraints

- Header values (spec §5/§6) verbatim: limit names `OP AI Gateway request quota` / `OP AI Gateway token quota` / `OP AI Gateway cost budget`; promo `OP AI Gateway <request quota|token quota|cost budget> reached; ask your gateway administrator to raise it` (ASCII only); window minutes hour `60`, day `1440`, week `10080`, month `43200`; credits `x-codex-credits-has-credits: true`, `x-codex-credits-unlimited: false`, never a balance.
- Success: used percent = `overageUtilizationCents(r.Utilization())` (floor, clamped 0..99), integer string. Block: `100`.
- Block: never `x-codex-limit-name`, `x-codex-active-limit`, `x-codex-rate-limit-reached-type` or a secondary window.
- Write only the client header set (`w.Header()` / `dst`), never `resp.Header` or a sink's header set.
- ADR-108 (#417) behavior stays byte for byte on every response that carries Codex figures; ADR-107 (`/v1/messages`) and every other flavor unchanged; the rate denial unchanged everywhere.
- Every new Go file starts with the two-line SPDX/copyright header used in the package. CI runs `golangci-lint fmt --diff` (gofumpt) and `golangci-lint run` per module.
- Repo text (code, comments, docs, commits) in English.

## Review Focus

- A concurrent `Record` bump pushes utilization to 1.0 or more on an admitted request → the success header reads `99`, never `100` (Task 1 test).
- The reading's reset has already passed when the response headers go out (late response after a period boundary) → no gateway header at all (Task 1 test).
- Relayed vendor figures arrive under a non-canonical header key (raw lower-case map key, or only a credits header) → the gateway still yields and writes nothing (Task 1 test).
- A quota block for a model whose application uses native passthrough → the same Codex 429, and the upstream is never called (Task 3 test).
- A vendor response to a caller NOT entitled to the account's figures (relay drops them) → the gateway's own family is written, since the client header set carries no Codex figure (Task 2 test).

---

## Phase 1 — Writers

### Task 1: Codex limit writers

**Files:**
- Create: `gateway/backend/internal/gateway/codex_principal_limit_headers.go`
- Create: `gateway/backend/internal/gateway/codex_principal_limit_headers_test.go`
- Modify: `gateway/backend/internal/gateway/principal_limit_headers.go` (extract the Retry-After computation of `writeLimitBlocked` into a shared helper)

**Interfaces:**
- Consumes (existing): `LimitReading`, `LimitStanding.Fullest`, `limitStandingFrom(ctx)`, `overageUtilizationCents(float64) int`, `limitBlockedLabel(LimitKind) string`, `limitBlockedCode(LimitKind) string`, `limitBlockedMessage(LimitReading) string`, `hasCodexRateLimitHeader(http.Header) bool`, `writeJSON`.
- Produces:
  - `func limitRetryAfterSeconds(reset, now time.Time) int` (in `principal_limit_headers.go`; ceil of seconds to reset, at least 1; `writeLimitBlocked` uses it)
  - `func codexWindowMinutes(period string) string` (`"60"`, `"1440"`, `"10080"`, `"43200"`, `""` for anything else; an empty value means the header is omitted)
  - `func codexLimitName(kind LimitKind) string`
  - `func codexPromoMessage(kind LimitKind) string`
  - `func writeCodexLimitStanding(dst http.Header, r LimitReading)`
  - `func attachCodexPrincipalLimitStanding(ctx context.Context, dst http.Header, now time.Time)`
  - `func writeCodexLimitBlocked(w http.ResponseWriter, r LimitReading, now time.Time)`
  - body types `codexLimitBlockedBody{Error codexLimitBlockedError}` / `codexLimitBlockedError{Type, Code, Message string; ResetsAt int64 "resets_at"}`

- [ ] **Step 1: Write the failing tests** in `codex_principal_limit_headers_test.go` (fixed `now := time.Date(2026, 11, 21, 18, 0, 0, 0, time.UTC)`, `reset := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)`):
  - `TestCodexLimitStandingHeaders`: token quota, day, used 620 / cap 1000 → `X-Codex-Primary-Used-Percent` `62`, `-Window-Minutes` `1440`, `-Reset-At` `strconv.FormatInt(reset.Unix(),10)`, `X-Codex-Limit-Name` `OP AI Gateway token quota`, `X-Codex-Credits-Has-Credits` `true`, `X-Codex-Credits-Unlimited` `false`; absent: `X-Codex-Secondary-Used-Percent`, `X-Codex-Credits-Balance`, `X-Codex-Active-Limit`, `X-Codex-Promo-Message`.
  - `TestCodexWindowMinutesPerPeriod`: hour `60`, day `1440`, week `10080`, month `43200`, `""` and `"year"` → `""`; and a reading with an unknown period writes no `-Window-Minutes` header.
  - `TestCodexLimitStandingClampsBelow100` (Review Focus): used/cap 1000/1000 → `99`; 1500/1000 → `99`; 0/1000 → `0`; 29/100 → `29`.
  - `TestCodexLimitNamesAndPromo`: the three names and the three promo sentences verbatim; every byte of each promo is `< 0x80`.
  - `TestAttachCodexPrincipalLimitStanding` (table): writes for a ctx with a standing; no-op for a ctx without one; no-op when any reading is `Failed`; no-op when dst already holds a raw lower-case key `x-codex-primary-used-percent` (set via `dst["x-codex-primary-used-percent"] = []string{"42.5"}`); no-op when dst holds only `X-Codex-Credits-Balance`; no-op when `reset` equals `now` and when it is before `now` (Review Focus). Every no-op case asserts dst is unchanged.
  - `TestWriteCodexLimitBlocked`: token quota day reading, `now` as above → status 429, `Content-Type` `application/json`; body decodes to `error.type` `usage_limit_reached`, `error.code` `limit.token_quota_exceeded`, `error.message` `== limitBlockedMessage(r)`, `error.resets_at` `== reset.Unix()`; headers `Retry-After` `21600`, `X-Should-Retry` `false`, `X-Codex-Promo-Message` `OP AI Gateway token quota reached; ask your gateway administrator to raise it`, `X-Codex-Primary-Used-Percent` `100`, `-Window-Minutes` `1440`, `-Reset-At` reset unix, credits `true`/`false`; absent: `X-Codex-Limit-Name`, `X-Codex-Active-Limit`, `X-Codex-Rate-Limit-Reached-Type`, `X-Codex-Secondary-Used-Percent`. A cost-budget month reading gives code `limit.cost_budget_exceeded` and window `43200`; a reading whose reset is before `now` gives `Retry-After` `1`.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexLimit|TestCodexWindowMinutes|TestAttachCodexPrincipal|TestWriteCodexLimitBlocked' -count=1`
Expected: FAIL (undefined functions).

- [ ] **Step 3: Implement the writers** in `codex_principal_limit_headers.go` per the Interfaces block. `writeCodexLimitStanding` and `writeCodexLimitBlocked` share one helper that writes the primary window (used percent passed in) and the two credits headers. `attachCodexPrincipalLimitStanding` returns without writing when `hasCodexRateLimitHeader(dst)`, when `limitStandingFrom(ctx).Fullest()` reports none, or when `!r.Reset.After(now)`. Doc comments say why each header is there (spec §2: credits flag suppresses the gpt-5.4-mini popup; no limit-name on the block keeps the promo sentence). Extract `limitRetryAfterSeconds` and use it in `writeLimitBlocked` too.

- [ ] **Step 4: Run the new tests and the existing header tests**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexLimit|TestCodexWindowMinutes|TestAttachCodexPrincipal|TestWriteCodexLimitBlocked|TestWriteLimitBlocked|TestWriteOverageStanding' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/codex_principal_limit_headers.go gateway/backend/internal/gateway/codex_principal_limit_headers_test.go gateway/backend/internal/gateway/principal_limit_headers.go
git commit -m "feat(gateway): Codex writers for the gateway's own limits (#418)"
```

## Phase 2 — Wiring

### Task 2: Success display at the three `/v1/responses` attach points

**Files:**
- Modify: `gateway/backend/internal/gateway/inference_handlers.go` (`handleOpenAIResponses`, after both `inferencePreflight` calls)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the `openai_responses` branch, after the cache attach, ~line 1176)
- Modify: `gateway/backend/internal/gateway/inference_complete.go` (~line 115) and `gateway/backend/internal/gateway/stream_session.go` (~line 146)
- Modify: `gateway/backend/internal/gateway/codex_principal_limit_headers.go` (add the Server method below)
- Create: `gateway/backend/internal/gateway/codex_principal_limit_paths_test.go`

**Interfaces:**
- Consumes: `attachCodexPrincipalLimitStanding` (Task 1), `withLimitStanding`, `s.attachCodexRateLimitWindows`.
- Produces: `func (s *Server) attachTranslatedCodexUsageLimits(r *http.Request, token auth.Token, dst http.Header)` — `s.attachCodexRateLimitWindows(token, dst)` then `attachCodexPrincipalLimitStanding(r.Context(), dst, time.Now())`; it replaces the bare `attachCodexRateLimitWindows` call at the two translate points. The native passthrough calls `attachCodexPrincipalLimitStanding(r.Context(), w.Header(), time.Now())` on a 2xx after its existing cache attach.

- [ ] **Step 1: Write the failing path tests** in `codex_principal_limit_paths_test.go`, on `newCodexWindowsFixture` (`codex_window_paths_test.go`) plus the promoted `capPrincipal()` (`principal_limit_paths_test.go`: 1000 tokens/day, 800 used, real-clock limiter). Helper `requireCodexStandingAttached(t, rec)`: `X-Codex-Primary-Used-Percent` `80`, `-Window-Minutes` `1440`, `X-Codex-Limit-Name` `OP AI Gateway token quota`, `X-Codex-Credits-Has-Credits` `true`.
  - `TestCodexStandingAttachedOnLocalPassthrough`: passthrough mode, `/v1/responses` with `windowsLocalModel` → 200 + attached.
  - `TestCodexStandingAttachedOnTranslateStreamAndBuffered`: translate mode, stream and buffered → attached.
  - `TestCodexStandingYieldsToVendorFigures`: capped owner (`vaOwnerSecret`), vendor model `codexWindowsModel` with `codexWindowHeaders` → `X-Codex-Primary-Used-Percent` `42.5`, `X-Codex-Limit-Name` `codex`, no `X-Codex-Credits-Has-Credits`.
  - `TestCodexStandingYieldsToCachedWindows`: `observeViaVendor`, then a local request → `requireCodexWindowsAttached(t, rec, "42.5")` and `X-Codex-Limit-Name` is `codex`.
  - `TestCodexStandingForNonEntitledVendorCaller` (Review Focus): set up the non-entitled member exactly as `TestCodexWindowsNotObservedForNonEntitledAndForgottenOnRevocation` does (`vaMemberFixture` with share level `shared`, member token for `usr_va_b`), give `usr_va_b` the `capPrincipal` limits (1000 tokens/day, 800 used, real-clock limiter), post to the vendor model → the vendor's `42.5` is absent and the gateway's own family is present (`Limit-Name` `OP AI Gateway token quota`, used `80`).
  - `TestCodexStandingNotAttachedOnError`: local upstream status 500 → `requireNoCodex`.
  - `TestCodexStandingAbsentWithoutLimits`: no `capPrincipal` → `requireNoCodex` on a local 200.
  - `TestCodexStandingNotOnMessages`: capped principal, `/v1/messages` → no `X-Codex-*` header.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexStanding' -count=1`
Expected: FAIL (the attached cases; the absent cases may already pass).

- [ ] **Step 3: Wire it**: the context in `handleOpenAIResponses` (mirror `handleAnthropicMessages`: `if len(pf.Standing) > 0 { r = r.WithContext(withLimitStanding(r.Context(), pf.Standing)) }` after each preflight, before `tryProxyNative` for the first), the native passthrough call, and `attachTranslatedCodexUsageLimits` at the two translate points. Update the comments at the touched call sites.

- [ ] **Step 4: Run the package's limit and window tests**

Run: `cd gateway/backend && go test ./internal/gateway -run 'Codex|Standing|Windows|PrincipalLimiter' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/
git commit -m "feat(gateway): show the gateway's own limit in Codex on /v1/responses (#418)"
```

### Task 3: The block on `/v1/responses`

**Files:**
- Modify: `gateway/backend/internal/gateway/inference_handlers.go` (`admitPrincipal` and its doc comment; the `inferencePreflight` step-5 comment; the `writeLimitDenied` doc comment)
- Modify: `gateway/backend/internal/gateway/principal_limits_wiring_test.go`

**Interfaces:**
- Consumes: `writeCodexLimitBlocked` (Task 1), `LimitStanding.Blocking`.
- Produces: `admitPrincipal` answers a non-rate denial whose `Blocking` reading exists with `writeLimitBlocked` for `inference.APIFlavorAnthropicMessages` and `writeCodexLimitBlocked(w, reading, a.At)` for `inference.APIFlavorOpenAIResponses`; everything else (rate, other flavors, no reading) still goes to `writeLimitDenied`.

- [ ] **Step 1: Write the failing tests** next to the Messages block tests, with `responsesBlockBody = {"model":"qwen-coder","input":"hi"}` and `blockTestServer`:
  - `TestResponsesQuotaDenialIsUsageLimitReached`: token quota day 100, used 100 → 429; `error.type` `usage_limit_reached`, `error.code` `limit.token_quota_exceeded`, `error.resets_at` > 0; `X-Should-Retry` `false`; `Retry-After` > 0; `X-Codex-Primary-Used-Percent` `100`; promo as in the spec; zero usage events.
  - `TestResponsesCostBudgetDenialIs429`: cost budget 1/month at 1.0 → 429, code `limit.cost_budget_exceeded`, `X-Codex-Primary-Window-Minutes` `43200`.
  - `TestResponsesBlockDescribesLatestReset`: token quota day + cost budget month, both breached → code `limit.cost_budget_exceeded`.
  - `TestResponsesBlockOnNativeModel` (Review Focus): same quota on `native-model` (the passthrough app used by `TestPrincipalLimiterRateLimitDeniesNativePassthrough`) → the same 429 body type, and the upstream received no request (assert as that test does).
  - `TestResponsesRateDenialUnchanged`: rate 1/60 s, second request → 429 `limit.rate_limited`, `Retry-After` set, top-level and `error.type` empty (apierror shape), no `X-Codex-*` header.
  - Existing `TestChatQuotaDenialKeepsShapeAddsNoRetry` and the Messages block tests stay green unchanged.

- [ ] **Step 2: Run them to verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestResponses(Quota|CostBudget|Block|Rate)' -count=1`
Expected: FAIL on the block cases (apierror 429/402 today).

- [ ] **Step 3: Implement the flavor switch in `admitPrincipal`**, keeping a single `Blocking` call; update the three doc comments (admitPrincipal, inferencePreflight step 5, writeLimitDenied) to name both flavors.

- [ ] **Step 4: Run the wiring tests**

Run: `cd gateway/backend && go test ./internal/gateway -run 'PrincipalLimiter|Messages|Responses|Chat' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/inference_handlers.go gateway/backend/internal/gateway/principal_limits_wiring_test.go
git commit -m "feat(gateway): answer a /v1/responses quota or budget block as Codex usage_limit_reached (#418)"
```

## Phase 3 — Architecture docs

### Task 4: ADR-109 and the compatibility section

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (append ADR-109 after ADR-108, same layout: Context, Decision, Rejected, Consequences and limits)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (new §9.6 "Codex display of the gateway's own limits" after §9.5; §9.4 "Other endpoints" names `/v1/responses` as the second exception; §9.5 gets one sentence on precedence with a link to §9.6; §13 error table rows for the three limit codes name the `/v1/responses` 429 `usage_limit_reached`)
- Modify: `docs/architecture/reference/api-surface.md` (the `/v1/responses` row), `docs/architecture/reference/openapi.yaml` (`/v1/responses` and its alias: a `'429'` response described in words; keep every `$ref` resolvable)
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (§11.1 row: the Codex display rests on undocumented behavior — last family wins, credits flag suppresses the popup, promo precedence; extend the memory-driver row to Codex)
- Modify: `docs/architecture/cross-cutting/telemetry-usage-observability.md` (~line 3215) and `docs/architecture/cross-cutting/security-auth-rbac.md` (~line 1048): add the §9.6 cross-reference beside §9.4

Content source: spec §2 (verified behavior, with issue #418 as the evidence link), §5, §6, §7, §10 (the credits carry-over consequence), and the end-to-end result from Task 5 (add it to §9.6 once Task 5 has run).

- [ ] **Step 1: Write ADR-109 and §9.6**, then the cross-references and table rows listed above.
- [ ] **Step 2: Run the docs check**

Run: `make lint-docs`
Expected: exit 0 (links, anchors, reachability, openapi `$ref`).

- [ ] **Step 3: Commit**

```bash
git add docs/architecture
git commit -m "docs(architecture): ADR-109 and compat §9.6 for the gateway's own limits in Codex (#418)"
```

## Phase 4 — Verification and finish

### Task 5: Gates, end to end, pull request (controller)

- [ ] **Step 1: Backend gates**: `cd gateway/backend && golangci-lint fmt --diff && golangci-lint run ./... && go test ./... -count=1` → all clean (known flakes per the owner's notes: re-run, don't chase).
- [ ] **Step 2: End to end** with Codex CLI 0.137.0 (isolated `CODEX_HOME`, `env -i`, fake key) → sqlite-mode gateway on this branch (bootstrap admin + API token) → a self-hosted stub answering `/v1/responses`, token quota per day on the user: a turn at < 90 % shows the status line `daily …% left` and `/status` `OP AI Gateway token quota Daily limit`; a turn at ≥ 90 % shows Codex's warning and no `gpt-5.4-mini` popup; past the cap Codex prints `You've hit your usage limit. OP AI Gateway token quota reached; ask your gateway administrator to raise it, or try again at …` with one request and zero upstream calls. Save the log to the session notes folder; put the result in §9.6.
- [ ] **Step 3: Local Sonar**: `make sonar-scan`, then `make sonar-findings && make sonar-branch-findings` → 0 branch-attributed findings.
- [ ] **Step 4: Remove** `docs/superpowers/`, verify `git diff --name-only origin/main...HEAD` lists neither it nor `docs/implementation-status.md`, commit.
- [ ] **Step 5: Push over SSH and open the PR** (English, substantive commit body for the squash), `Closes #418`; enable Auto-fix.

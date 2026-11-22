# Gateway limit over cached Codex windows from 75 % — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On a `/v1/responses` success whose Codex figures would come only from the per-principal window cache, write the gateway's own limit instead once its written percent is ≥ 75.

**Architecture:** One reading selector and one predicate in `codex_principal_limit_headers.go`; the three Codex attach points (native passthrough, translate JSON, translate SSE header block) skip the cache attach when the predicate holds, and the unchanged standing writer then fills the empty header set. Docs: ADR-109 amendment, compat §9.5/§9.6, risk row, api-surface.

**Tech Stack:** Go (`gateway/backend`, package `internal/gateway`), Markdown architecture docs.

**Spec:** `docs/superpowers/specs/2026-11-22-codex-gateway-limit-over-cached-windows-design.md`

## Global Constraints

- Threshold: `75`, compared with `overageUtilizationCents(r.Utilization())` (floor, clamped 0..99), inclusive (`>= 75`).
- Fresh upstream Codex figures are never replaced; the cache is skipped only where the gateway's window is written on the same reply.
- A takeover reply carries no cached header at all (no default-family window, no `x-codex-<id>-*` family, no cached limit name).
- Byte-identical behavior below 75 %, without a quota/budget, with a failed reading, with a reset not after now, and for tokens without `send_usage_limits`.
- One `now` per reply for the takeover decision and the standing writer.
- `codex_window_cache.go` (observe/store/attach) is not modified.
- Repo text in English; no LAN host names; commit messages with substantive bodies (squash merge uses them); Conventional Commit subjects ending in `(#418)`.
- CI gates per Go module: `golangci-lint fmt --diff` and `golangci-lint run` (gofumpt, gocritic), plus `go test ./...`.

## Review Focus

- Period boundary with a cached entry (reset not after now): the reply must keep the cached windows, never end up headerless → Task 1 unit test `reset equals now` / `reset before now` on the predicate.
- Failed reading with a cached entry: cache stays → Task 1 unit test `failed reading`.
- A cached extra family (`x-codex-other-*`) on a takeover reply would hide the gateway's window → Task 2 `TestCodexTakeoverDropsCachedExtraFamily`.
- The SSE header block is written before the upstream answers; the takeover must be decided before `WriteHeader` → Task 2 covers `stream=true` in the translate test.
- A token with `codex_model_switch_prompt` on a takeover reply: gateway window without credits flags → Task 2 `TestCodexTakeoverWithModelSwitchPrompt`.

---

### Task 1: Reading selector and takeover predicate

**Files:**
- Modify: `gateway/backend/internal/gateway/codex_principal_limit_headers.go` (`attachCodexPrincipalLimitStanding` and new declarations next to it)
- Test: `gateway/backend/internal/gateway/codex_principal_limit_headers_test.go`

**Interfaces:**
- Produces:
  - `const codexGatewayTakeoverPercent = 75`
  - `func codexPrincipalLimitReading(ctx context.Context, now time.Time) (LimitReading, bool)` — `limitStandingFrom(ctx).Fullest()`, ok only if `r.Reset.After(now)`.
  - `func codexGatewayLimitTakesOver(ctx context.Context, now time.Time) bool` — `codexPrincipalLimitReading` ok and `overageUtilizationCents(r.Utilization()) >= codexGatewayTakeoverPercent`.
  - `attachCodexPrincipalLimitStanding(ctx, dst, now, offerModelSwitch)` keeps its signature and behavior; it now gets its reading from `codexPrincipalLimitReading`.

- [ ] **Step 1: Write the failing test** `TestCodexGatewayLimitTakesOver` (table, uses existing `codexTokenReading`, `codexTestNow`, `withLimitStanding`):

| case | standing | want |
|---|---|---|
| `74 percent` | `codexTokenReading(749, 1000)` | false |
| `75 percent` | `codexTokenReading(750, 1000)` | true |
| `clamped 99` | `codexTokenReading(1000, 1000)` | true |
| `fullest decides` | `codexTokenReading(100, 1000)` + a request-quota reading at 80/100 | true |
| `no standing` | `context.Background()` | false |
| `failed reading` | `codexTokenReading(900, 1000)` + a reading with `Failed: true` | false |
| `reset equals now` | 900/1000 with `Reset = codexTestNow` | false |
| `reset before now` | 900/1000 with `Reset = codexTestNow.Add(-time.Minute)` | false |

Call: `codexGatewayLimitTakesOver(ctx, codexTestNow)`.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexGatewayLimitTakesOver' -count=1`
Expected: build failure, `undefined: codexGatewayLimitTakesOver`.

- [ ] **Step 3: Implement** the constant, `codexPrincipalLimitReading` and `codexGatewayLimitTakesOver`, and make `attachCodexPrincipalLimitStanding` use `codexPrincipalLimitReading`. Doc comments: the predicate says it decides whether cached subscription windows yield on a reply (ADR-109 amendment), and why 75 (Codex's first warning step).

- [ ] **Step 4: Run tests**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexGatewayLimitTakesOver|TestAttachCodexPrincipalLimitStanding|TestCodexLimitStanding' -count=1`
Expected: PASS.

- [ ] **Step 5: Commit** — `feat(gateway): decide when the gateway's own limit takes over from cached Codex windows (#418)` with a body naming the threshold and the shared reading.

---

### Task 2: Skip the cache attach on takeover at the three attach points

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the `openai_responses` branch of `proxyNative`, the `sendUsage && 2xx && !hasCodexRateLimitHeader(resp.Header)` cache attach and the standing attach after it)
- Modify: `gateway/backend/internal/gateway/codex_principal_limit_headers.go` (`attachTranslatedCodexUsageLimits`)
- Modify: `gateway/backend/internal/gateway/principal_limit_paths_test.go` (fixture helper)
- Test: `gateway/backend/internal/gateway/codex_principal_limit_paths_test.go`

**Interfaces:**
- Consumes: `codexGatewayLimitTakesOver(ctx, now)`, `attachCodexPrincipalLimitStanding(ctx, dst, now, offerModelSwitch)` from Task 1.
- Produces: test helper `func (f *windowsFixture) capPrincipalAt(tokens int64)` (token quota 1000/day for `usr_va_a`, aggregate `tokens`); `capPrincipal()` becomes `capPrincipalAt(800)`.

- [ ] **Step 1: Write the failing tests** in `codex_principal_limit_paths_test.go`:
  - Rename `TestCodexStandingYieldsToCachedWindows` → `TestCodexStandingTakesOverCachedWindows` (passthrough) and `TestCodexStandingYieldsToCachedWindowsOnTranslate` → `TestCodexStandingTakesOverCachedWindowsOnTranslate` (`stream` false and true). Setup unchanged (`capPrincipal()` = 80 %, `observeViaVendor`). Assert `requireCodexStandingAttached(t, rec)` and `X-Codex-Secondary-Used-Percent == ""`, `X-Codex-Secondary-Reset-At == ""`.
  - `TestCodexStandingYieldsToCachedWindowsBelowThreshold` (passthrough) and `...BelowThresholdOnTranslate` (both stream modes): `capPrincipalAt(740)`; keep the old assertions (`requireCodexWindowsAttached(t, rec, "42.5")`, limit name `codex`, `X-Codex-Credits-Has-Credits == ""`).
  - `TestCodexTakeoverDropsCachedExtraFamily` (passthrough): before `observeViaVendor`-style call, set `f.cprov.vendorHdr` to `codexWindowHeaders(now, time.Hour, 24*time.Hour)` plus `x-codex-other-primary-used-percent: 12`, `x-codex-other-primary-window-minutes: 60`, `x-codex-other-primary-reset-at: <now+1h>`, `x-codex-other-limit-name: other`; post the vendor model once (200), then `capPrincipal()` and post the local model. Assert `requireCodexStandingAttached` and no header whose lower-case name starts with `x-codex-other-`. Control (same setup at `capPrincipalAt(740)`): `X-Codex-Other-Primary-Used-Percent == "12"`.
  - `TestCodexTakeoverWithModelSwitchPrompt` (passthrough): `capPrincipal()`, `observeViaVendor`, `f.setCodexModelSwitchPrompt(t, true)`; assert used percent `80`, limit name `OP AI Gateway token quota`, `X-Codex-Credits-Has-Credits == ""`, `X-Codex-Credits-Unlimited == ""`, no secondary.
  - `TestCodexCachedWindowsWithoutLimits` (passthrough): no `capPrincipal`, `observeViaVendor`, local post → `requireCodexWindowsAttached(t, rec, "42.5")`. (Skip if an existing test in `codex_window_paths_test.go` already asserts exactly this; name it in the commit body.)

- [ ] **Step 2: Run them to verify the takeover tests fail**

Run: `cd gateway/backend && go test ./internal/gateway -run 'TestCodexStanding|TestCodexTakeover|TestCodexCachedWindowsWithoutLimits' -count=1`
Expected: FAIL in the two `TakesOver` tests and `TestCodexTakeoverDropsCachedExtraFamily` / `TestCodexTakeoverWithModelSwitchPrompt` (cached `42.5` found instead of `80`); below-threshold and no-limit tests PASS.

- [ ] **Step 3: Implement**
  - `proxyNative`, `openai_responses` branch: take `now := time.Now()` once; attach cached windows only when additionally `!codexGatewayLimitTakesOver(r.Context(), now)`; pass the same `now` to `attachCodexPrincipalLimitStanding`. Update the two comments there.
  - `attachTranslatedCodexUsageLimits`: `now := time.Now()`; skip `s.attachCodexRateLimitWindows` when `codexGatewayLimitTakesOver(r.Context(), now)`; pass `now` to the standing writer. Update its doc comment.

- [ ] **Step 4: Run the package**

Run: `cd gateway/backend && go test ./internal/gateway -count=1`
Expected: PASS (known load flakes per the repo notes may need a re-run in isolation; note any in the report).

- [ ] **Step 5: Lint**

Run: `cd gateway/backend && golangci-lint fmt --diff && golangci-lint run ./internal/gateway/...`
Expected: no diff, no findings.

- [ ] **Step 6: Commit** — `feat(gateway): the gateway's own limit takes over from cached Codex windows from 75 % (#418)` with a body: which replies, what is skipped, what stays.

---

### Task 3: Architecture docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-109: new amendment paragraph directly after the ADR-110 amendment note; first consequence bullet; a new consequence bullet)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (§9.6 *Precedence with §9.5*; §9.5 *Window cache* "Attached only…" bullet and the closing "What Codex shows" sentence "so the subscription always wins")
- Modify: `docs/architecture/11-risks-and-technical-debt.md` (row "The gateway's own-limit display in Codex rests on undocumented client behavior")
- Modify: `docs/architecture/reference/api-surface.md` (the `/v1/responses` row's "on a success that carries no subscription figures")
- Any further hit of `grep -rn "relayed or cached\|carries no subscription figure\|no gateway warning" docs/architecture`

**Interfaces:**
- Consumes: the behavior of Tasks 1–2 and spec §5–§6.

- [ ] **Step 1: ADR-109 amendment** — title unchanged (anchors). Paragraph starting `**Amended (cached windows yield from 75 %, issue #418 follow-up).**`: on a `/v1/responses` success whose Codex figures would come only from ADR-108's window cache, the gateway's own fullest limit replaces them, the whole cached set included, once its written percent is 75 or more (Codex's first warning step); fresh relayed vendor figures still win whole; below 75 %, without a quota or budget, with a failed reading or a reset not after now nothing changes. Why: a local-model request does not consume the subscription, and with cached windows up to 7 days a capped user's first notice was the block. The PR link is added once the PR exists.
- [ ] **Step 2: ADR-109 consequences** — rewrite the first bullet (gateway shown on cached-only replies from 75 %; below that the subscription, no gateway warning); add a bullet: above 75 % the display alternates between subscription (vendor replies) and gateway (local replies); the subscription's weekly (secondary) bar disappears on local replies; Codex keeps one warning step counter per slot for the whole session, shared by whatever figure fills the default family's primary slot, so a 75 % warning already taken by the subscription's 5h window means the gateway's first warning comes at 90 %, and the alternation repeats no warning. Credits bullet: also reached via takeover replies.
- [ ] **Step 3: §9.6 *Precedence with §9.5*** — "Fresh figures win whole; cached ones yield from 75 %", the rule as in spec §5 (names `codexGatewayLimitTakesOver`, `codexGatewayTakeoverPercent`), the result table of spec §5, consequences as in Step 2, and that a native vendor 2xx without Codex headers counts like a local reply.
- [ ] **Step 4: §9.5, risk row, api-surface** — adjust the sentences listed under **Files** to the new rule; risk row: replace "While cached subscription windows are in the future, a subscription user sees no gateway warning." with the 75 % rule and the shared warning steps.
- [ ] **Step 5: Lint docs**

Run: `make lint-docs` (repo root)
Expected: OK.

- [ ] **Step 6: Commit** — `docs(architecture): cached Codex windows yield to the gateway's own limit from 75 % (#418)` with a body listing the touched sections.

---

### Phase after the tasks (controller, not a subagent)

- Codex CLI 0.137.0 stub run per spec §9 (four turns), log to the shared notes folder; fold any contradiction into the docs per spec §10.
- Whole-branch review (Opus), full gates (`go test ./...`, golangci fmt/run, `make lint-docs`, local Sonar branch findings), remove `docs/superpowers/`, verify `git diff --name-only origin/main...HEAD`, push over SSH, PR, Auto-fix, add the PR link to the ADR-109 amendment.

# The gateway's own limit takes over from cached subscription windows in Codex from 75 % (design)

Branch-local working file (removed before the pull request). Follow-up of issue #418 / PR #419, on main `e356bfa9`
(#419 and #422 merged). Owner decision recorded in chat on 2026-11-22.

## 1. Intent and why

Since PR #419 (ADR-109, compat §9.6) the Codex CLI shows the gateway's own principal limit (request quota, token
quota, cost budget) in its default `x-codex` family on a `/v1/responses` success, but only when the client header set
carries no Codex rate-limit header at all: "the subscription wins, whole". That rule covers both kinds of subscription
figures:

- **fresh** ones, relayed from the vendor response of this very request (ADR-108 relay), and
- **cached** ones, the caller's last observed subscription windows that ADR-108's per-principal window cache attaches to
  every `/v1/responses` success that has none of its own, i.e. every local-model reply, until their reset (a weekly
  window up to 7 days).

The cached case hurts: a capped principal who made one vendor call and then works on local models never sees the
gateway's limit and never gets Codex's 75/90/95 % warnings for it, because every local reply carries the cached
subscription windows. The first notice is the block. Yet a local-model request does not consume the subscription at
all; the only limit that can stop it is the gateway's.

Codex shows only one family per response (verified in #418), so the two cannot both appear on one reply. The owner
decided: **on a reply that would carry only cached subscription windows, the gateway's own limit takes over once it
reaches 75 %.**

## 2. Goals

- A principal with a quota or budget at 75 % or more sees the gateway's limit, and gets Codex's warnings for it, on
  every `/v1/responses` success whose Codex figures would otherwise come only from the window cache.
- Nothing changes below 75 %, for a principal without a quota or budget, or when the gateway cannot describe the
  standing (failed reading, empty standing, reset not after now): those replies are byte-for-byte what they are today.

## 3. Non-goals

- A configurable threshold, a per-token or per-principal switch for this rule. The threshold is a fixed constant.
- Showing both the subscription and the gateway on one reply (Codex cannot).
- Any change to Claude Code / `/v1/messages`. There the gateway's overage figures sit beside the subscription windows and
  Claude Code shows both, so the problem does not exist (`attachPrincipalLimitStanding`, ADR-107).
- Any change to the block (429 `usage_limit_reached`), the rate denial, the relay, the entitlement rule, the cache's
  observe/store side, capture redaction, or #422's two token switches.
- Working around Codex's own warning de-duplication (§6).

## 4. Anti-goals (a failure even if it "works")

- Fresh vendor figures ever being replaced or mixed with the gateway's window. A reply that relays the vendor's
  `x-codex-*` figures stays exactly as ADR-108 writes it, whatever the gateway's utilization.
- A reply ending up with **no** Codex figures where it has cached ones today (Codex would clear its display). The cache
  may be skipped only when the gateway's window is certainly written on the same reply.
- A cached extra family (`x-codex-<id>-*`) left on a reply that carries the gateway's window: Codex parses extra families
  after the default one and keeps only the last, so it would hide the gateway's window.
- Byte changes for tokens without `send_usage_limits` (they get no Codex headers at all, #422), or for principals below
  the threshold.

## 5. The rule (decided)

On a `/v1/responses` 2xx (the `openai_responses` flavor: native passthrough, translate JSON, translate SSE header
block), for a token with `send_usage_limits`, at the point where today the cached windows are attached (i.e. the
upstream sent no Codex rate-limit header, `!hasCodexRateLimitHeader(resp.Header)`, or the path is translate):

1. Determine the gateway reading exactly as `attachCodexPrincipalLimitStanding` does today:
   `limitStandingFrom(ctx).Fullest()`, ok, and `Reset.After(now)`.
2. If such a reading exists **and** its written percent `overageUtilizationCents(r.Utilization())` is **≥ 75**, do
   **not** attach the cached windows at all (no default family, no extra family, no limit names). The gateway's
   standing is then written by the existing `attachCodexPrincipalLimitStanding`, unchanged (primary window, window
   minutes, reset, `x-codex-limit-name`, credits flags unless `codex_model_switch_prompt`).
3. Otherwise attach the cached windows as today; the standing then yields to them as today.

Decided details:

- **Threshold 75, on the integer percent the gateway writes** (floor, at most 99). 74.99 % writes 74 and keeps the
  cache; 75.0 % writes 75. This is Codex's first warning step (`used_percent >= 75.0`), so the moment of the takeover
  coincides with the first possible gateway warning. *(owner: 75 %; on the written integer: designer's call)*
- **The gateway takes over regardless of how full the cached subscription windows are.** A local request does not
  consume the subscription; the next vendor reply shows the subscription's fresh figures again. *(designer's call,
  shown to the owner, not objected to)*
- **The whole cache attach is skipped** on a takeover reply, extra families included. *(designer's call, see §4)*
- **One `now` per reply** for the takeover decision and the standing writer, so they cannot disagree about whether the
  reset is still in the future.
- **Fresh vendor figures always win**, as today: when the upstream response carries any Codex rate-limit header the cache
  is never attached anyway, and the takeover question does not arise. A vendor family withheld from a non-entitled
  caller still leaves room for the gateway's window, as today, at any utilization.
- **Without a quota or budget** (empty standing), with a failed reading, or with a reset not after now, there is no
  takeover: the cache attaches as today. With the memory driver a configured principal reads 0 %, so no takeover there.
- **A native 2xx without Codex headers from a vendor account** (normal for an API-key account, rare for a subscription)
  is treated like a local reply: the takeover applies. Nothing from the vendor is replaced, so no anti-goal is touched.
  *(designer's call, raised by the builder check)*
- **Translate replies are never subscription replies:** an OpenAI subscription target on `openai_responses` is always
  native passthrough (`vendorSubscriptionOpenAITarget`, `routing/resolver.go`), so every translate reply is a local one.
- **Observation is unchanged.** Vendor responses keep feeding the cache; a takeover reply only skips the attach.
- **Credits.** Unchanged rules: the gateway's window carries `x-codex-credits-has-credits: true` and
  `x-codex-credits-unlimited: false` unless the token has `codex_model_switch_prompt`. With that switch Codex may now show
  its `gpt-5.4-mini` popup on local replies from 90 % of the gateway's limit (the user chose that switch).

### Resulting behavior (token with `send_usage_limits`, principal with a cached subscription entry)

| reply | gateway < 75 % (or no limit / failed reading) | gateway ≥ 75 % |
|---|---|---|
| vendor reply with fresh figures, entitled caller | vendor figures | vendor figures (unchanged) |
| vendor reply, figures withheld (not entitled) | gateway window (the upstream sent figures, so no cache attach) | gateway window |
| local reply (native passthrough without Codex headers, translate JSON, translate SSE) | cached windows | **gateway window, no cached family** |
| any non-2xx | as today | as today |
| block (quota/budget denial) | as today | as today |

## 6. Codex client behavior this relies on (Codex CLI 0.137.0)

Verified earlier (#416, #418): one family per response, last one wins; only the default family drives status line and
warnings; a response's snapshot replaces primary and secondary; credits carry over.

Read in code for this design (tag `rust-v0.137.0`, `codex-rs/tui/src/chatwidget/rate_limits.rs`), to be confirmed in the
stub run (§9):

- `RateLimitWarningState { primary_index, secondary_index }` exists once per chat session and is never reset. Warnings
  at [75, 90, 95] are taken per slot (primary, secondary), each step at most once, for whatever figure fills the default
  family's slot. So the gateway's window (always the primary slot) shares its steps with the subscription's primary
  (5h) window: if the subscription's 5h window already warned at 75 % in this session, the gateway's first warning comes
  at 90 %, and vice versa. The subscription's weekly window uses the secondary slot and is independent.
- A snapshot with a window at 100 % yields no warnings at all.

Consequences to document (not to fix):

- Above the threshold, the display alternates between the subscription (on vendor replies) and the gateway (on local
  replies). Codex's step counters do not reset, so this does not repeat warnings.
- A takeover reply has no secondary window, so the subscription's weekly bar disappears from the status line and
  `/status` on local replies and comes back on the next vendor reply.
- Credits carry-over (ADR-109's existing consequence) now also follows from takeover replies: after one, a vendor reply
  that sends no credits headers of its own keeps the gateway's `has-credits: true`, suppressing the subscription's popup.

## 7. Code shape (constraints; the rest is the builder's)

Package `gateway/backend/internal/gateway`.

- `codex_principal_limit_headers.go`: factor the reading selection out of `attachCodexPrincipalLimitStanding` so the
  takeover decision and the writer use the same reading and the same `now`; add the threshold as a named constant
  (75) and a predicate (name is the builder's) that is true when a describable reading's written percent is ≥ 75.
- The three attach points skip `attachCodexRateLimitWindows` when the predicate holds:
  - native passthrough, `proxyNative`'s `openai_responses` branch (`native_passthrough.go`, the
    `sendUsage && 2xx && !hasCodexRateLimitHeader(resp.Header)` cache attach);
  - `attachTranslatedCodexUsageLimits` (used by `inference_complete.go` for the buffered JSON answer and by
    `stream_session.go` for the SSE header block).
- `attachCodexPrincipalLimitStanding` keeps its contract (writes nothing if `dst` already has a Codex header), so a
  fresh vendor family still wins. No other function changes behavior. `codex_window_cache.go` stays unchanged.

## 8. Tests (TDD)

Unit (`codex_principal_limit_headers_test.go`): the predicate at 74 (false), 75 (true), 99/clamped (true), empty
standing, failed reading, reset not after now (all false).

Path tests (`codex_principal_limit_paths_test.go`, fixtures `newCodexWindowsFixture`, `observeViaVendor`,
`capPrincipal` = token quota 1000/day at 800 → 80 %):

- `TestCodexStandingYieldsToCachedWindows` and `...OnTranslate` today assert that the cache wins at 80 %. They become the
  takeover tests: at 80 % on native passthrough, translate JSON and translate SSE the reply carries the gateway's window
  (`80`, `1440`, `OP AI Gateway token quota`, credits flag) and **no** cached header (no `x-codex-secondary-*`, limit
  name not `codex`).
- New below-threshold variants (e.g. 740 of 1000 → 74 %) keep today's assertions: cached windows, limit name `codex`,
  no gateway credits flag. A helper to cap at a given aggregate is the builder's.
- A takeover reply with a cached **extra** family (`x-codex-<id>-*` in the observed vendor headers) carries none of it.
- Fresh vendor figures at ≥ 75 % stay untouched (`TestCodexStandingYieldsToVendorFigures` already covers 80 %; keep it).
- No limit configured, with a cached entry: cached windows as today (existing tests in `codex_window_paths_test.go`
  cover this; confirm one runs with a cache entry and no limiter config).
- `codex_model_switch_prompt` on a takeover reply: the gateway's window without credits flags.
- A token without `send_usage_limits`: no Codex header at all (existing #422 tests; keep green).

## 9. Verification

- `go test ./...` in `gateway/backend`, `golangci-lint fmt --diff` and `golangci-lint run` (CI gates), `make lint-docs`,
  local Sonar gate (new-code / branch-attributed findings).
- **Codex CLI 0.137.0 against a loopback stub** (isolated `CODEX_HOME`, custom provider, TUI in tmux, `/status` after
  each turn), feeding the exact header sequences the gateway produces: (1) subscription windows with 5h at 80 %
  (warning at 75 expected), (2) the gateway's takeover window at 80 % (expected: display switches to the gateway, no
  second 75 warning, weekly bar gone), (3) subscription again (display back), (4) the gateway at 91 % (90 warning, no
  popup with the credits flag). This confirms §6. The gateway's cached path itself cannot run end to end: the cache is
  filled only from an OpenAI vendor provider, whose ChatGPT backend URL is fixed (§9.5); handler tests cover it, as in
  ADR-108. Log goes to the shared notes folder.

## 10. Docs to update

- ADR-109 (`docs/architecture/09-architecture-decisions.md`): an amendment note after ADR-110's ("cached windows yield to
  the gateway's own limit from 75 %", why, link to this PR), the first consequence bullet rewritten, a consequence on
  Codex's shared warning steps and the alternating display. The ADR title stays (anchors). *(designer's call: amend
  ADR-109 rather than a new ADR, as the task brief asks)*
- compat §9.6 *Precedence with §9.5*: fresh figures win whole, cached ones yield from 75 %; the consequences list; the
  shared warning steps. §9.5's closing sentence ("so the subscription always wins") and its *Window cache* "Attached
  only…" bullet.
- Risk row "The gateway's own-limit display in Codex rests on undocumented client behavior" (`11-risks-and-technical-debt.md`):
  the "no gateway warning while cached windows are in the future" sentence, plus the shared warning steps.
- `docs/architecture/reference/api-surface.md` (the `/v1/responses` row: "on a success that carries no subscription
  figures").
- The ADR-109 amendment links the pull request; its number is added in a commit once the PR exists.
- If the stub run (§9) contradicts §6, the docs describe what was observed; the decision goes back to the owner only if
  the gateway's window or its warnings do not appear on takeover replies.
- Any other mention found by grepping for the old rule (e.g. "only on responses that carry no subscription figure",
  "relayed or cached").

## 11. Left to the builder

Names of the constant, predicate and helpers; how the shared reading/`now` is passed; test helper shape; exact doc
wording within §10.

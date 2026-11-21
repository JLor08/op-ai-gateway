# Design: the gateway's own principal limits in the Codex CLI (issue #418)

Status: approved in chat by the owner, 2026-11-21. Branch `feat/418-codex-principal-limits`.

## 1. Intent

A principal (user or service) can carry a request quota, a token quota and a cost budget (`routing.LimitConfig`,
enforced by `PrincipalLimiter`). #413/ADR-107 made them visible in Claude Code and turned a `/v1/messages` block into one
readable sentence. Codex users get neither today:

- Codex shows no gateway limit, so a developer learns about a quota only when it bites.
- When a quota bites, Codex prints `exceeded retry limit, last status: 429 Too Many Requests`, with no limit and no reset
  time. A cost-budget 402 is retried five times in about 7 s and then printed as
  `unexpected status 402 Payment Required: cost budget exceeded, url: …`.

After this change a Codex user sees the fullest gateway limit in the status line and `/status`, gets Codex's own warnings
at 75, 90 and 95 %, and on a block reads one sentence with the reset time after exactly one request.

## 2. Verified client behavior (Codex CLI 0.137.0)

Full evidence: issue #418 (stub runs with an isolated `CODEX_HOME` plus a code read at tag `rust-v0.137.0`). The facts
the design rests on:

1. **One family per response.** Codex parses every header family (`x-codex-*` first, then each `x-<id>-*` family in sorted
   id order) but keeps one snapshot slot and emits it once per response, so only the response's **last** family reaches
   the TUI. An extra family on a response that also carries the subscription's `x-codex-*` windows hides the subscription.
2. **Only the default family `x-codex` drives the status line and warnings.** An extra family is a `/status` row only.
3. Default family with `x-codex-limit-name: <name>` and one window: the status line shows `daily 38% left`, `/status`
   shows `<name> Daily limit: […] 38% left (resets …)`, and warnings read
   `Heads up, you have less than 10% of your daily limit left. Run /status for a breakdown.`
4. Window labels come from `-window-minutes` (±5 %): 1440 `daily`, 10080 `weekly`, 43200 `monthly`. Anything else,
   including 60, falls back to `usage`.
5. At 90 % or more of the default family Codex opens the popup `Switch to gpt-5.4-mini for lower credit usage?` with the
   switch preselected; one Enter switched a test session's model. `x-codex-credits-has-credits: true` plus
   `x-codex-credits-unlimited: false` and no balance suppresses the popup and adds no `/status` row. In Codex's code that
   flag affects only the popup and the credits row.
6. A 429 with body `error.type = usage_limit_reached` is never retried. With `x-codex-promo-message` and no
   `-limit-name` on the active family it prints
   `You've hit your usage limit. <promo>, or try again at <reset in local time>.`, the time taken from `error.resets_at`.
   A `-limit-name` on the active family wins over the promo and yields
   `You've hit your usage limit for <name>. Switch to another model now, or try again at …`.
7. The 429's family windows (default family when `x-codex-active-limit` is absent) replace the display. Without windows the
   display is cleared.
8. Codex reads neither `retry-after` nor `x-should-retry`. Any 429 is unretried (`retry_429 = false`); a 402 is retried
   at turn level.

## 3. Goals, non-goals, anti-goals

Goals:

- Show a principal's fullest gateway limit in Codex's status line, `/status` and warnings on `/v1/responses` successes.
- Answer a quota or budget block on `/v1/responses` with one request and one sentence that names the limit, its reset
  time and whom to ask.
- Same data source as ADR-107: readings taken at admission, no store I/O on the response path.

Non-goals:

- The short rate limit (429 `limit.rate_limited` + `Retry-After`) stays unchanged everywhere. Codex keeps printing
  `exceeded retry limit, last status: 429 Too Many Requests` for it, after one request.
- No extra limit family (`x-opaigw-*` or `x-codex-<id>-*`), and no display of more than one gateway limit.
- No change to other endpoints, to `/v1/messages` (ADR-107), to the store, the server agent or the portal.

Anti-goals (a failure even if it works):

- Changing or hiding #417's subscription figures on any response that carries them. ADR-108 stays byte for byte.
- Leading a user into a model switch that does not help with a gateway limit.
- Holding a response on the store.

## 4. Decisions made with the owner

- **Display in the default family** (status line + warnings), not an extra `/status` row.
- **Suppress the model-switch popup** with the credits flag on every response that carries the gateway's family.
- **Block shape on `/v1/responses`** as in §6, including the 100 % window and a 429 for the cost budget too.

## 5. Success: the fullest limit in the default family

**Readings.** `handleOpenAIResponses` puts `pf.Standing` on the request context with `withLimitStanding`, after either of
its two `inferencePreflight` calls, exactly as `handleAnthropicMessages` does. `LimitStanding.Fullest` picks the reading
(highest utilization, ties to the later reset, then the fixed order; none when the standing is empty or any reading
failed).

**When.** On every 2xx of the `openai_responses` flavor, at the three attach points that already carry #417's cache:

- native passthrough: the `openai_responses` branch in `native_passthrough.go`, after the relay and the cache attach,
  before the status line is written;
- translate, buffered JSON: `inference_complete.go`, after `attachCodexRateLimitWindows`;
- translate, SSE: `stream_session.go`, the header block written with the 200, after `attachCodexRateLimitWindows`.

Each writes the client header set only, never the upstream set that `recordUsage`, the window caches and the vendor usage
scrape read.

**Precedence.** The gateway writes nothing when, after #417's steps, the client header set already carries any Codex
rate-limit header (`isCodexRateLimitHeader`, i.e. relayed vendor figures or the cached windows). The subscription wins,
whole. It also writes nothing when the standing has no describable fullest reading, or when the reading's reset is not
after the moment of writing (a response that outlives the period boundary; the response stays headerless and the next one
corrects it).

**Headers written** (lower-case names, as the rest of the family):

| Header | Value |
|---|---|
| `x-codex-primary-used-percent` | `floor(utilization × 100)` as an integer, clamped to 0..99 (the cents rule of ADR-107: an admitted request stays below 100) |
| `x-codex-primary-window-minutes` | nominal period length: hour `60`, day `1440`, week `10080`, month `43200` (nominal so a 28-day February still reads `monthly`) |
| `x-codex-primary-reset-at` | the reading's reset, unix seconds |
| `x-codex-limit-name` | `OP AI Gateway request quota`, `OP AI Gateway token quota` or `OP AI Gateway cost budget` |
| `x-codex-credits-has-credits` | `true` (suppresses the model-switch popup, §2.5) |
| `x-codex-credits-unlimited` | `false` |

No secondary window, no `x-codex-credits-balance`, no `x-codex-active-limit`.

What Codex then shows: status line `daily 62% left` (an hourly limit reads `usage 60% left`), `/status`
`OP AI Gateway token quota Daily limit: […] 62% left (resets …)`, warnings at 75/90/95 %, no popup.

## 6. Block: a 429 `usage_limit_reached` on `/v1/responses`

`admitPrincipal` already branches on the flavor for `/v1/messages`. A quota or budget denial (`a.Reason` other than
`rate`) on the `openai_responses` flavor goes to a new writer; `LimitStanding.Blocking(LimitKind(a.Reason))` picks the
reading as in ADR-107 (latest reset among the breached readings, ties to the fixed order, fallback to the reading Admit
named). When it reports none, the request falls back to `writeLimitDenied` as Messages does.

**Status:** 429 for all three kinds; the cost budget is a 429 on this endpoint instead of its 402 (as on `/v1/messages`).

**Body** (`Content-Type: application/json`):

```json
{"error":{"type":"usage_limit_reached","code":"limit.token_quota_exceeded","message":"token quota reached · resets Nov 22, 00:00 UTC · ask your gateway administrator to raise it","resets_at":1795305600}}
```

- `code`: the limit's stable code (`limit.request_quota_exceeded`, `limit.token_quota_exceeded`,
  `limit.cost_budget_exceeded`), unchanged.
- `message`: ADR-107's sentence (`limitBlockedMessage`), for SDK clients that print `error.message`.
- `resets_at`: the reading's reset, unix seconds; Codex formats it in local time.

**Headers:**

| Header | Value |
|---|---|
| `Retry-After` | seconds to the reset, rounded up, at least 1 |
| `X-Should-Retry` | `false` |
| `x-codex-promo-message` | `OP AI Gateway <request quota\|token quota\|cost budget> reached; ask your gateway administrator to raise it` (ASCII only) |
| `x-codex-primary-used-percent` | `100` |
| `x-codex-primary-window-minutes` | nominal period length (§5) |
| `x-codex-primary-reset-at` | the reading's reset, unix seconds |
| `x-codex-credits-has-credits` | `true` |
| `x-codex-credits-unlimited` | `false` |

Deliberately absent: `x-codex-limit-name` (it would replace the sentence with "Switch to another model now"),
`x-codex-active-limit` (the default family is the one Codex reads), `x-codex-rate-limit-reached-type`, any secondary
window. During a block `/status` therefore labels the row `Daily limit` without the `OP AI Gateway` prefix, and the
status line shows `daily 0% left`. A subscription user sees the gateway's 0 % until the next successful vendor response.

What Codex prints: `You've hit your usage limit. OP AI Gateway token quota reached; ask your gateway administrator to raise
it, or try again at 1:00 AM.` (for a client in UTC+1). A denied request is still not recorded as a usage event and never reaches `Resolve`.

## 7. Unchanged

- Every other flavor: `/v1/messages` (ADR-107), and chat completions, images, speech, transcription, live transcription,
  the realtime handshake and the portal loopback paths keep `writeLimitDenied` (429 quota, 402 budget, `apierror` body,
  `X-Should-Retry: false`).
- The rate denial on every flavor.
- ADR-108's relay, cache, entitlement and capture redaction.
- A principal with no quota or budget, or with a failed read: no gateway header, byte for byte as today.
- The memory driver has no `usage_events`, so a configured principal reads 0 % (as ADR-107 documents).

## 8. Verification

- Unit tests for the two writers (values, clamping, window minutes per period, absent headers, ASCII promo).
- Path tests for the three attach points: written for a principal with a standing; not written with fresh vendor figures,
  with cached windows, on a non-2xx, without a standing, with a failed reading, or with a reset already past.
- Admission tests: a quota and a budget block on `openai_responses` give the 429 above; `/v1/messages` and another flavor
  are unchanged; the rate denial is unchanged.
- End to end: Codex CLI 0.137.0 against a sqlite-mode gateway with a stub upstream answering `/v1/responses`: the status
  line and `/status` show the gateway limit, no popup at 90 % or more, and a block prints the sentence after one request.
- All repo gates (Go fmt/lint/test, `make lint-docs`, local Sonar branch findings).

## 9. Documentation

- New ADR-109 next to ADR-107/108 (decision, rejected alternatives: extra family, documenting the popup only, the
  limit-name 429; consequences: subscription wins, undocumented client behavior).
- `compatibility-and-inference.md`: a new §9.6 "Codex display of the gateway's own limits"; §9.4 "Other endpoints" and
  §9.5 cross-references updated; §13 errors if it lists the limit statuses.
- Reference docs: API surface and `openapi.yaml` where they describe `/v1/responses` limit responses.
- Risks entry (undocumented Codex behavior: last-family-wins, credits flag, promo precedence), and the telemetry/limiter
  section if it lists the per-flavor denial shapes.

## 10. Calls made by the designer (not discussed with the owner)

- Fullest reading only, one window, as ADR-107; integer percent, floored, at most 99 on a success.
- Nominal window minutes (month 43200) so every month reads `monthly`; an hourly limit takes Codex's fallback label `usage`.
- The limit name carries the kind (`OP AI Gateway token quota`), because one window cannot say which limit it is.
- Precedence keyed on *any* Codex rate-limit header in the client header set, so the subscription wins whole.
- No header when the reading's reset has already passed at write time (ADR-107 writes a stale reset; Codex would show a
  past reset time).
- Consequence to record in ADR-109: Codex carries credits over between responses, so once a gateway response has set
  `has-credits: true`, a later local-model reply that carries only cached subscription windows also suppresses the
  subscription's popup, until the next vendor response sends its own credits headers.
- The end-to-end Codex run is a documented manual run (the #416 precedent), not a committed test.

## 11. Left to the builder

- Function and file names, and whether the success writer shares helpers with ADR-107's (`overageUtilizationCents`,
  `limitBlockedLabel`, `limitBlockedCode`, `limitBlockedMessage`).
- Test layout, as long as §8 is covered.

# Codex CLI usage-limit display through the gateway — design

Issue: #416. Branch: `feat/416-codex-usage-limits`. Pattern: ADR-106 (#407/#408), applied to `/v1/responses`.
Owner decision (2026-11-21): no question round, follow the #408 pattern.

## 1. Intent

A Codex CLI user who routes through OP AI Gateway to an OpenAI (ChatGPT) subscription vendor account should see that
subscription's plan usage in Codex, as they would when signed in directly. That covers the status-line items, the `/status` limit
bars with reset times, the 75/90/95 % warnings and the usage-limit message on a 429. The display should survive a session that
also uses local models.

## 2. Verified client behavior (Codex CLI 0.137.0; evidence in #416)

- Codex reads the rate-limit headers of every HTTP `/responses` stream (`parse_all_rate_limits`). It does this behind a custom
  provider with an API key too: no login type and no token shape is required (unlike Claude Code). Custom providers use HTTP,
  not WebSocket.
- Headers it reads (all lowercased on the wire):
  - The default family `x-codex-{primary,secondary}-{used-percent,window-minutes,reset-at}` and `x-codex-limit-name`.
  - Further families `x-<id>-{primary,secondary}-…` and `x-<id>-limit-name`, discovered through `x-<id>-primary-used-percent`.
    The observed ids all start with `codex` (`x-codex-other-*`, `x-codex-bengalfox-*`).
  - Credits: `x-codex-credits-has-credits`, `x-codex-credits-unlimited` (both required) and `x-codex-credits-balance`.
  - On a 429 with body `error.type = usage_limit_reached`: `x-codex-active-limit` (which family's windows to show),
    `x-codex-promo-message` and `x-codex-rate-limit-reached-type`.
- Each response's snapshot replaces the primary/secondary windows; only credits and plan type carry over. A response without the
  headers therefore clears the display (verified: `Limits: not available for this account`).
- Codex does not read `retry-after`. Other OpenAI SDK clients of `/v1/responses` do.

## 3. Goals, non-goals, anti-goals

Goals:

1. Relay the Codex rate-limit family on the `openai_responses` flavor of the native passthrough, to entitled callers only.
2. Keep the display stable in mixed sessions: a per-principal window cache attaches the last subscription windows to
   `openai_responses` successes of other targets.
3. Capture redaction: a payload capture never shows the family to someone the relay would withhold it from.
4. Architecture docs updated (new ADR-108, compatibility-and-inference §6 and new §9.5, security, external vendor accounts,
   risks, glossary).

Non-goals:

- The gateway's own principal limits in Codex (counterpart of #413/ADR-107). This is a possible follow-up, as an extra limit
  family.
- `x-codex-turn-state` (sticky-routing token) and other non-limit `x-codex-*` headers. These are not relayed; a possible
  effect is unverified.
- Changes to the portal scrape (`scrapeCodexUsage`) or the active usage fetch.
- Any token option or secret shape: Codex needs none.
- The WebSocket transport and the `codex.rate_limits` event.

Anti-goals (failure even if it "works"):

- Leaking a shared account's figures to a member the portal would not show them to.
- Relaying arbitrary upstream headers: request ids, organization ids, an API-key organization's `x-ratelimit-*`, or
  `x-codex-turn-state`.
- Any change to ADR-106/107 behavior on `/v1/messages`. The existing #408 tests stay green unchanged.
- Faulting or slowing an inference request: the cache and entitlement code is best effort, bounded and fail-closed.

## 4. Design

### 4.1 The Codex rate-limit family (one predicate)

`isCodexRateLimitHeader(name)` is a case-insensitive test. It is true when the name starts with `x-codex-` and either:

- ends with `-used-percent`, `-window-minutes`, `-reset-at` or `-limit-name`; or
- is one of `x-codex-credits-has-credits`, `x-codex-credits-unlimited`, `x-codex-credits-balance`, `x-codex-active-limit`,
  `x-codex-rate-limit-reached-type` or `x-codex-promo-message`.

Everything else under `x-codex-` is not in the family, `x-codex-turn-state` included. Ids outside the `x-codex` prefix are
not relayed (builder's call; no such family has been observed). This one predicate drives the relay, the entitlement trigger and
the capture redaction.

### 4.2 Relay (native passthrough)

In `proxyNative`, at the same point as the Messages relay (after the final upstream response, before `WriteHeader`), for
`rel.pfReq.APIFlavor == openai_responses`:

- Copy `retry-after` and `x-should-retry` always. This mirrors #408 exactly: they steer OpenAI SDK clients' retries, and they
  disclose nothing account-specific.
- Copy the Codex family (every value) only when the upstream sent at least one family header and
  `rateLimitFiguresEntitled(ctx, token, target)` is true. The lookup runs only when a family header is present.
- This applies on any status (2xx, 429, …). The headers come from the final upstream response only, so a discarded re-selection
  attempt contributes none, as on Messages.
- There is no provider check, as on Messages: a self-hosted target is always entitled, and a vendor account goes through the
  entitlement rule.

Implementation latitude: generalize `forwardRateLimitHeaders` (for example, pass the gated predicate) rather than duplicate it.
The Messages call keeps its exact behavior.

### 4.3 Entitlement

Reuse `rateLimitFiguresEntitled` unchanged. The figures reach the owner, a community manager of a `shared`/`full` account, or
everyone when the owner opted in (`share_usage_with_members`). The answer is cached per principal and account and fails closed.
The cache key is per account, not per header family, so one answer serves both families.

### 4.4 Window cache (mixed sessions)

Codex clears its windows on a headerless response, so the gateway remembers the caller's last observed Codex windows and attaches
them to an `openai_responses` success that carries no family header of its own.

- **Observe:** in `recordUsage`, next to `observeRateLimitWindows`, on the upstream headers. This covers native passthrough
  (`resp.Header`) and translate (the sink headers), on any status. It applies only to a target with
  `routing.IsOpenAIVendorProvider(target.Provider)` whose response carries at least one
  `x-codex[-<id>]-primary-used-percent` or `-secondary-used-percent`. It requires an entitled caller; for a caller who is not
  entitled, `Forget(key, account)` runs, as on Messages.
- **What is stored:** per family (`x-codex`, `x-codex-other`, …), the primary and secondary windows, each with `used-percent`,
  `window-minutes` and `reset-at`, plus the family's `limit-name`. A window without a parseable integer `reset-at` is not
  stored, because it could never expire. Not stored:
  - credits: Codex itself carries them over when a snapshot lacks them;
  - the 429-only headers (`active-limit`, `rate-limit-reached-type`, `promo-message`);
  - `retry-after` and `x-should-retry`.
- **Attach:** on `openai_responses` 2xx responses only, when the response carries no Codex family header. Three places:
  - the native passthrough, before `WriteHeader`;
  - the translate path's buffered JSON success;
  - the translate path's SSE header block, which, as on Messages, is written before the upstream answers.
- **Which windows are written:** only windows whose reset is still in the future. A family's `limit-name` is written only with at
  least one of its windows. Nothing is written on errors, and nothing on any other flavor.
- **Bounds:** per process, in memory, capped at 10 000 principals (oldest observation evicted) and pruned as windows expire. It
  is lost on restart, which matches ADR-106.
- **Keying:** the principal key, as on Messages. The Codex entries live apart from the Anthropic entries: either a second instance
  of a shared store, or a family dimension in the key. A principal using both Claude Code and Codex must not have one family
  overwrite the other.
- **No duplicated eviction logic.** Refactor the shared part (entries, cap, prune, evict, forget, live lookup) of
  `rateLimitWindowCache` into something both families use. The Anthropic behavior must stay byte-identical, so the existing
  `ratelimit_window_*_test.go` tests pass unchanged.

### 4.5 Capture redaction

`redactCapturedAccountHeaders` also treats the Codex family as sensitive for a vendor-account target. It drops the family from
the captured translate-path upstream headers unless the principal is entitled. The translate leg of an OpenAI subscription
account (portal chat via the ChatGPT backend) stores upstream headers verbatim today. Native passthrough captures hold the
client header set, which the relay already gates (unchanged reasoning from ADR-106).

### 4.6 Client-side consequences to document

- The ≥ 90 % warning comes with Codex's own model-switch nudge to `gpt-5.4-mini`, which the gateway may not map. Codex can hide
  it with `notices.hide_rate_limit_model_nudge = true`.
- Before the first subscription response of a session (and after a gateway restart), `/status` says
  `Limits: not available for this account`. Codex's startup prefetch exists only for a ChatGPT login.
- The dependency is on undocumented client behavior: the header names and the clear-on-missing rule. Add a risk-table row.

## 5. Testing

- Unit:
  - the family predicate (members, non-members, case-insensitivity, `x-codex-turn-state` excluded);
  - relay allowlist on responses: family only when entitled; retry headers always; unified family and other headers never;
  - the Messages relay unchanged;
  - cache observe/attach/expiry/forget/cap for the Codex family, including that both families coexist for one principal.
- Handler (`proxyNative` with a fake native proxy client and a vendor OpenAI subscription target, as the #408 tests do):
  - entitled, non-entitled and self-hosted cases;
  - a 429 carrying the family;
  - re-selection that drops first-attempt headers;
  - a translate `openai_responses` success with cached windows attached (JSON and SSE);
  - capture redaction for a non-entitled vs. an entitled principal.
- Rework `TestNativeResponsesDoesNotForwardRateLimitHeaders`: the unified family must still not be forwarded on responses, and
  the Codex family now is.
- End to end: the Codex CLI 0.137.0 against a memory-mode gateway, with the stub registered as a self-hosted app (always
  entitled) answering `/v1/responses` with the family. Expect the status line and `/status` to show the windows. The vendor-only
  parts (entitlement, cache observe) are covered by the handler tests, because the ChatGPT backend URL is hard-coded in the
  resolver and cannot be pointed at a stub.
- Gates: per-module `golangci-lint fmt --diff` and `run`, `go test ./...` (backend), `make lint-docs`, and the portal gates only
  if the portal is touched (not planned). The local Sonar gate runs if Docker is available; ask before starting Docker Desktop.

## 6. Builder's calls (made by the session, not by the owner)

- The family predicate of §4.1: explicit suffixes and names, no whole-prefix relay, no non-`x-codex` ids.
- `x-should-retry` relayed on responses too (mirrors #408), besides `retry-after`.
- Credits are not cached: Codex carries them over itself.
- Observe applies only to OpenAI vendor providers, attach only to `openai_responses` 2xx without own family headers.
- `x-codex-promo-message` belongs to the gated family. It is account-specific upsell text and part of Codex's usage-limit
  message.

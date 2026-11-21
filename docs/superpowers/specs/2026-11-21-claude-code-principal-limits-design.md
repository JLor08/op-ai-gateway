# Claude Code shows the gateway's own principal limits — design

Issue: #413 (follow-up to #407 / ADR-106). Branch: `feat/413-claude-code-principal-limits`.
Branch-local working document; its durable content moves to `docs/architecture/` before the PR.

## 1. Intent

A developer using Claude Code through OP AI Gateway should hear about the gateway's own limits before they bite, the way a
claude.ai subscriber does. When a limit does bite, Claude Code should show one clear sentence: which limit, when it resets,
what to do. It should not hammer the gateway with retries for minutes.

The limits are the principal's request quota, token quota and cost budget, per user or per service (`routing.LimitConfig`,
enforced by `PrincipalLimiter` in `gateway/backend/internal/gateway/principal_limits.go`). Claude Code reads them only in the
documented gateway shape: the `anthropic-ratelimit-unified-*` headers with the window-agnostic `overage` claim, and a
`429 billing_error` for a block.

Why now: #407 made the subscription windows visible, and the owner agreed the gateway's own limits are the follow-up. Today
a quota denial makes Claude Code retry with backoff for minutes. Verified: 8 retries in 90 s, still going.

## 2. Verified facts this design rests on

All against Claude Code 2.1.328 (details in issue #413):

- **Contract:** the `/protocol` text in the binary, section "Usage-limit headers — optional", quoted in §5 and §6.
  Anthropic's own gateway, with several caps, "describes the fullest one, or once blocked the one that resets last".
- **JWT token (`aud: claude-gateway`, #407's opt-in `claude_code_usage_display`):**
  - the overage window is read: `rate_limit_event` with `rateLimitType: overage`, utilization and threshold;
  - the contract 429 is printed verbatim, after exactly one request.
- **Opaque token:** the 429 is printed as `API Error: Request rejected (429) · <message>`, also after one request.
- **Gateway sign-in only:** the status line `rate_limits.spend_limit` and the `/usage` "Spend limit" bar appear only in a
  gateway sign-in session. That sign-in is not available to Claude Desktop.
- **Client order:** the client picks the warning from the first window with a surpassed threshold, in the order `5h`, `7d`,
  `7d_oi`, `overage`.

## 3. Goals

1. On every successful `/v1/messages` response for a principal with at least one quota or budget configured, send the
   usage-limit headers describing that principal's fullest limit (§5).
2. On `/v1/messages`, answer a quota or budget denial with the contract's `429 billing_error` (§6). This includes the cost
   budget, which is `402` today.
3. On every other endpoint, keep each quota or budget denial's status and body. Add `x-should-retry: false`, so Anthropic
   and OpenAI SDKs stop retrying into a block (§7).
4. Coexist with #407's subscription windows (§8).

## 4. Non-goals and anti-goals

**Non-goals:**

- `GET /api/oauth/usage` (dollar amounts). It is read only in gateway sign-in sessions, it is USD-only, and the cost budget
  here is energy-based.
- Anything extra for the gateway sign-in displays. A terminal client in gateway sign-in mode gets the `/usage` bar and the
  status line field from the same headers, with no further work.
- The short rate limit (`RateRequests` / `RateWindowSeconds`). It stays a throttle (429 + `Retry-After`) everywhere, is not
  part of the overage window, and its body is unchanged.
- Changing how limits are configured or enforced: periods, thresholds, caching, fail-open.
- The contract's `group_pool` and fail-closed variants. There are no shared pools, and the limiter fails open.
- Codex `x-codex-*` headers, and any portal or frontend change.

**Anti-goals (a failure even if it works):**

- **Headers for a principal with no quota or budget.** None may be sent; for such a principal, `/v1/messages` stays
  byte-identical to today.
- **Responses held up by a store problem.** The headers come from the limiter's existing caches. A failed read means no
  gateway headers, never a delay and never a block.
- **Changed behavior on other endpoints.** Nothing beyond the one added header.
- **A broken #407 display.** A principal without gateway limits keeps exactly today's subscription headers.

## 5. Success headers (`/v1/messages`, 2xx)

**Which limits take part.** The configured ones among request quota, token quota and cost budget. A limit is configured when
its threshold is > 0 and its period is set, the same rule `Admit` uses.

**Reading of one limit:**

| Field | Meaning |
|---|---|
| `used` | The period aggregate the limiter already keeps: request count, token sum or cost, from `UsageAggregateSince` through the 10 s cache plus `Record`'s bumps |
| `cap` | The configured threshold |
| `period` | `hour`, `day`, `week` or `month` |
| `reset` | The end of the current UTC calendar period, i.e. the next period start after `routing.PeriodStart(period, now)` |

`utilization = used / cap`.

**When the readings are taken: at admission, then carried to the response (my call, after the builder check).**

- **Why:** the response headers go out when the upstream answers. That is often more than the 10 s cache TTL after `Admit`:
  the buffered translate path waits for the whole completion, and a long prompt on a local model delays the first byte. A
  fresh read at that point would hold the status line on the store.
- **What:** `Admit` reads every configured limit's aggregate on an allowed request. It hands those readings back as the
  request's standing, and the handlers carry them to the attach point.
- **Result:** no store I/O happens on the response path. The figures are exactly the ones admission judged; `Record`'s bumps
  from concurrent requests during the upstream call are not reflected, which is acceptable for a notice at 75 % and 95 %.

**Which limit is described: the fullest.** Highest utilization wins. Ties go to the later reset, then to the fixed order
request quota, token quota, cost budget. The standing is the one admission judged: the current request is not yet counted.
That is what keeps utilization below 1 on an allowed request.

**Headers written:**

| Header | Value |
|---|---|
| `anthropic-ratelimit-unified-overage-utilization` | `floor(utilization × 100) / 100`, two decimals (`0.82`), clamped to `0.99` at most. Concurrent bumps can push the cached figure to 1 or above on an admitted request; the contract wants it below 1 while requests are allowed |
| `anthropic-ratelimit-unified-overage-surpassed-threshold` | `0.95` when the written utilization is ≥ 0.95, `0.75` when ≥ 0.75, otherwise absent |
| `anthropic-ratelimit-unified-overage-status` | `allowed_warning` when a threshold is written, otherwise `allowed` |
| `anthropic-ratelimit-unified-overage-reset` and `anthropic-ratelimit-unified-reset` | `reset` as unix seconds |
| `anthropic-ratelimit-unified-representative-claim` | `overage` |
| `anthropic-ratelimit-unified-status` | `allowed_warning` when the overage window warns, or when a subscription window kept on the response (§8) has a `-<w>-status` of `allowed_warning` or `rejected`; otherwise `allowed` |

No `-overage-period` on a success: the contract does not list it there.

**Where it is attached.** At the three points where #407 attaches cached windows, as the last header step before the status
line is written. The callers already know the flavor is Anthropic Messages and the response is a 2xx.

| Path | Point |
|---|---|
| Native passthrough (`native_passthrough.go`) | After `forwardRateLimitHeaders` / `attachRateLimitWindows`, before `WriteHeader`, on a 2xx; whether or not the upstream sent unified headers |
| Translate, buffered JSON (`inference_complete.go`) | On success, before the body is written |
| Translate, SSE (`stream_session.go`) | In the 200 header block. As with #407, a stream whose upstream then fails carries the headers together with its SSE error event |

The overlay changes only the client response header set, never the upstream header set that `recordUsage`, the #407 window
cache (`observeRateLimitWindows`) and the vendor usage scrape read.

**Fail-open.** No gateway headers are sent when:

- there is no limiter;
- the config read at admission failed;
- any configured limit's aggregate read at admission failed.

A partial picture could name the wrong "fullest" limit. Today's `aggregate` turns a read error into zeros, which would read as
0 %. The admission readings must therefore be able to tell "zero" from "failed". Admission itself still fails open exactly as
today. The #407 headers are left as they were in all three cases.

**Memory driver.** It has no `usage_events`, so every aggregate is zero and a configured principal is reported at 0 %. That
matches what enforcement sees: quotas and budgets are a persistent-store feature by design. This is documented, not special-cased.

**Token shape.** The headers go out whatever the token's shape, like #407's relay. Only a JWT-shaped
(`claude_code_usage_display`) or gateway sign-in client reads them.

## 6. Block (`/v1/messages`, quota or budget denial)

**Trigger.** `Admit` denies with `request_quota`, `token_quota` or `cost_budget`, and the request's flavor is Anthropic
Messages. That covers `/v1/messages` and its aliases; `/v1/messages/count_tokens` never admits and is never blocked.
`admitPrincipal` / `writeLimitDenied` must learn the flavor. `inferencePreflight` has it as `shape.apiFlavor`.

**Which limit is described.** Among the configured quota and budget limits whose `used >= cap`, the one with the latest
`reset`; ties follow the fixed order. That is when the user can work again.

- **Reads:** `Admit` stops at the first breach. The deny path therefore reads the remaining configured aggregates through the
  normal cached path. This happens only on a denial, which never reaches an upstream.
- **A failed read** counts as not breached (my call): the worst case is a reset and `Retry-After` that come too early.
- **None breached:** if no breached limit is left after that (for example, the cache moved), the limit `Admit` named is
  described, with its own period and reset.

**Response:**

- **Status:** `429`.
- **Body:** `{"type":"error","error":{"type":"billing_error","code":"<code>","message":"<sentence>"}}`. The code is the described
  limit's stable code: `limit.request_quota_exceeded`, `limit.token_quota_exceeded` or `limit.cost_budget_exceeded`. The
  top-level `"type":"error"` is new for this one body. Every other gateway error keeps the `apierror` shape.
- **Sentence** (fixed English, lowercase start like Anthropic's own gateway; the owner approved this form):
  `<request quota|token quota|cost budget> reached · resets <Mon D, HH:MM> UTC · ask your gateway administrator to raise it`.
  Example: `token quota reached · resets Nov 22, 00:00 UTC · ask your gateway administrator to raise it`. The time is the
  reset in UTC, Go layout `Jan 2, 15:04`.
- **Headers:**

| Header | Value |
|---|---|
| `Retry-After` | Seconds until `reset`, rounded up, at least 1 |
| `X-Should-Retry` | `false` |
| `anthropic-ratelimit-unified-status` | `rejected` |
| `anthropic-ratelimit-unified-reset` and `-overage-reset` | `reset` as unix seconds |
| `anthropic-ratelimit-unified-overage-utilization` | `1` |
| `anthropic-ratelimit-unified-overage-surpassed-threshold` | `1` |
| `anthropic-ratelimit-unified-overage-period` | `daily` / `weekly` / `monthly` for `day` / `week` / `month`; absent for `hour` (the client accepts a missing period) |
| `anthropic-ratelimit-unified-overage-disabled-reason` | `org_spend_cap_reached` |

- **Absent:** `representative-claim` and `overage-status` (with them, Claude Code composes its own line and drops ours), and
  no #407 subscription windows.
- **Unchanged:** the denied request is still not recorded as a usage event and never reaches `Resolve`.

## 7. Other endpoints

**Quota and budget denials.** Status and body stay exactly as today: quota `429`, budget `402`, `apierror` body, no
`Retry-After`. They gain `X-Should-Retry: false`. This covers every caller of the shared admission:

- `/v1/chat/completions`, `/v1/responses`, images, speech and transcription;
- live transcription, and the `/v1/realtime` handshake;
- the portal loopback paths.

**The rate-limit denial** is unchanged everywhere.

## 8. Coexistence with #407 (ADR-106)

**For a principal with at least one quota or budget configured** (and readings available), on a 2xx `/v1/messages` response:

1. **Starting point:** the #407 steps run as today, relaying the upstream's unified headers when the caller is entitled, or
   attaching the cached windows to another provider's success.
2. **Remove:** every `anthropic-ratelimit-unified-*` header except the per-window fields of `5h`, `7d` and `7d_oi`
   (`-<w>-utilization`, `-<w>-reset`, `-<w>-status`, `-<w>-surpassed-threshold`). That removes the subscription's own
   top-level fields and its `-overage-*`, which describe Anthropic's extra usage, not this gateway's cap. It also removes
   `-fallback*` and `-upgrade-paths`.
3. **Write** the §5 headers.

Result: the gateway's cap owns the top-level fields, as the contract wants ("the headers always describe the developer's own
cap"). The subscription windows stay visible beside it.

**Principals without gateway limits** keep #407's behavior byte for byte.

**Known client limitation (document, do not work around).** Claude Code takes the first window with a surpassed threshold in
the order `5h`, `7d`, `7d_oi`, `overage`. While a subscription window warns, the gateway's overage warning is not the one
shown. Both are still in the `rate_limit_event`.

**The 429** carries only §6's headers. Claude Code's per-window display then waits for the next subscription response or
cache attach, as after any error today.

## 9. Constraints

- **Stable error codes stay.** The new body carries the existing `limit.*` code next to the new `type`.
- **`internal/compat` holds compatibility mapping, but these headers are a gateway concern.** They describe the gateway's
  own admission, not a provider translation, so they live with #407's code in `internal/gateway`.
- **Untouched:** the server agent (no version bump), the store schema and the frontend.
- **Undocumented dependency.** The display relies on undocumented Claude Code behavior: the `aud` gate, the header names and
  the window order. A client update can stop it without breaking the gateway. The 429 shape and `x-should-retry` are
  documented in the contract and help every client.

## 10. Verification

- **Unit tests:**
  - period end for each period;
  - reading selection (fullest, tie-breaks, the blocking limit, the fallback when none breached);
  - header formatting (floor, clamp, thresholds, status merge);
  - the fail-open cases.
- **Handler tests:**
  - each of the three attach points with a capped principal;
  - no headers for an unconfigured principal;
  - an entitled subscription relay plus gateway cap (top-level replaced, windows kept, upstream header set untouched);
  - a cached-window attach plus gateway cap;
  - the `/v1/messages` 429 (status, body, every header, hour vs day period);
  - another flavor's denial (unchanged body and status, plus `X-Should-Retry`);
  - the rate denial unchanged.
- **End to end, controller-run, not in the repo:** Claude Code 2.1.328 against a sqlite-mode gateway built from the branch,
  with a stub upstream and a user token with `claude_code_usage_display`:
  - a configured quota near its cap yields a `rate_limit_event` with `rateLimitType: overage`;
  - a breached quota yields the verbatim sentence after one request.
- **Repo gates:**
  - per module, `golangci-lint fmt --diff` and `run`, and `go test`;
  - `make lint-docs`;
  - the Sonar branch findings when the environment allows (the gate needs Docker Desktop; ask the owner first).

## 11. Durable docs to update (before the PR)

- **New ADR-107**, the decisions in §5, §6, §8.
- **`compatibility-and-inference.md`:** §9.3 gets a sibling §9.4 for the gateway's own limits, and the error-code table
  gets the `/v1/messages` 429 row and the `X-Should-Retry` note.
- **`reference/api-surface.md`:** the `/v1/messages` refusals and the code table.
- **`telemetry-usage-observability.md`:** the limiter section, for the standing reading and its fail-open rule.
- **`11-risks-and-technical-debt.md`:** the client-order limitation, the memory driver's 0 %, the undocumented client
  dependency.

## 12. Left to the builder

- Names, file layout and the limiter API shape: for example `Admit` returning its readings (each with a failure flag)
  alongside its verdict, and pure functions for "fullest" and "blocking".
- How the flavor reaches `writeLimitDenied`.
- How the admission standing travels to the attach points. Candidates are the `preflight` value, the `inference.Request`
  copy the handlers pass on, or a request-scoped value. The constraint: no store I/O on the response path.
- Test organization.
- Small refactors needed to keep the three attach points to one call each.

# Per-token opt-in for usage-limit signals — design

Issue: JLor08/op-ai-gateway#420. Branch: `feat/420-usage-limits-token-opt-in` (from `main` 263ece32).
Status: draft for owner approval (builder check done; owner decisions on the promo header and service tokens folded in).

## 1. Intent

Since #407/#413/#416/#418 the gateway tells coding clients about usage limits:
- the vendor subscription windows: Anthropic `anthropic-ratelimit-unified-*` and OpenAI/Codex `x-codex-*`;
- cached copies of those windows;
- the gateway's own principal limits.

It does so for every principal, with no token setting. The owner wants this to be a deliberate per-token choice. A token sends nothing about usage unless its owner switched it on, except what stops a client from retrying uselessly once nothing is left.

Second, the gateway currently always suppresses the Codex CLI's "switch to gpt-5.4-mini" popup. Someone who only works with a Codex subscription may want it, so it becomes a per-token choice too.

## 2. Verified facts this design relies on

- **Claude Code:**
  - stops retrying on `x-should-retry: false` (and on `retry-after` > 60 s);
  - reads unified headers only for a JWT-shaped bearer with `aud: claude-gateway` (#407).
- **Codex 0.137.0:**
  - never retries a 429 whose body has error type `usage_limit_reached`, ignores `retry-after` / `x-should-retry`, and retries a 402 five times;
  - at ≥ 90 % of its default window it shows the gpt-5.4-mini popup unless `x-codex-credits-has-credits: true` + `-unlimited: false` arrived;
  - carries the credits state over between responses (#418 stub runs).
- **`claude_code_usage_display`** only shapes the secret today. It is in `store.TokenRecord` but not in `auth.Token`.
- **Signal sites:** the complete list of where usage signals leave the gateway is in the issue (#420, "Current state").

## 3. Goals

1. **G1:** a per-token boolean `send_usage_limits`, default off, on user and service tokens. When off, no usage-limit header reaches the client on any path.
2. **G2:** what prevents retries survives with G1 off. That means status codes, `retry-after`, `x-should-retry`, and the block error bodies.
3. **G3:** a per-token boolean `codex_model_switch_prompt`, default off, effective only together with `send_usage_limits`. When on, the gateway never writes its own credits flag.
4. **G4:** existing tokens with the Claude Code token format keep their display after the upgrade.

## 4. Non-goals and anti-goals

- **Non-goal:** merging the switch with the Claude Code token format. The owner decided on two switches; the format stays rotation-only and separate.
- **Non-goal:** changing capture redaction, which stays entitlement-based, `writeLimitDenied`, or the verbatim relay of vendor error bodies.
- **Non-goal:** the open question of which figures Codex shows on local responses when the gateway's limit nears exhaustion (cached subscription windows hiding gateway warnings). It is discussed separately; whichever change lands second rebases.
- **Anti-goal:** leaving a path that still sends a usage header with the switch off. Every site in §5.2 is covered by a test.
- **Anti-goal:** breaking retry suppression. With the switch off, a gateway block must still stop Claude Code and Codex from retrying.

## 5. Design

### 5.1 Settings and persistence

**Columns:** two new `api_tokens` columns, `send_usage_limits` and `codex_model_switch_prompt`, both `integer not null default 0`. They are added by one append-only migration 112 (`token_usage_limit_switches`) via `addColumnIfMissing`. The same migration runs `update api_tokens set send_usage_limits = 1 where claude_code_usage_display = 1` (G4).

**Naming:**

| Layer | `send_usage_limits` | `codex_model_switch_prompt` |
|---|---|---|
| Go fields (`store.TokenRecord`, `auth.Token`) | `SendUsageLimits` | `CodexModelSwitchPrompt` |
| JSON field | `send_usage_limits` | `codex_model_switch_prompt` |

**Template:** follow `decisions_confidence_normalize` end to end.
- The column, the record field, insert, `tokenColumns`, scan and the metadata update, and the copy into `auth.Token` in every bearer→token builder:
  - `resolveBearer` in `store/sqlite_token.go`;
  - the memory directory builders;
  - `AuthorizeRunAsToken`.
- The portal DTO plus the create and update requests:
  - Changeable through PATCH. Unlike the token format, no rotation is needed.
  - Ungated: any principal that may edit the token.
  - Always emitted on reads.
- Service tokens: settable on create and read back, as the template does (owner's decision).
  - Service tokens have no field-level update today, and adding one is follow-up issue #421.
  - Consequence: every existing service token is off for both switches after the upgrade (the backfill reaches none, since service tokens cannot carry the token format). Turning a switch on means recreating the token until #421 lands.
  - The docs state this.

### 5.2 Request-time gate

A helper `usageSignalsEnabled(token auth.Token) bool` returns `token.SendUsageLimits`. When it is false, every site below sends no usage header. When it is true, they behave exactly as today; the vendor-account entitlement still applies on top for vendor figures.

| Site | Today | With `send_usage_limits` off |
|---|---|---|
| `forwardRateLimitHeaders` (native, both flavors) | `retry-after` / `x-should-retry` always; vendor family if entitled | `retry-after` / `x-should-retry` only (`includeUnified=false`) |
| Window cache attach: `attachRateLimitWindows`, `attachCodexRateLimitWindows`, and the translate wrappers | attaches cached windows | nothing |
| Window cache observe: `observeRateLimitWindows`, `observeCodexRateLimitWindows` | observes for entitled principals | nothing observed. Builder's call: whether to also `Forget` an existing entry, which keeps memory clean after a toggle-off |
| Gateway standing on success: `attachPrincipalLimitStanding`, `attachCodexPrincipalLimitStanding`, and the translate wrappers | writes the standing | nothing |
| Messages block (`writeLimitBlocked`) | 429, `retry-after`, `x-should-retry: false`, `billing_error` body, unified `rejected` / `overage-*` headers | 429, `retry-after`, `x-should-retry: false`, the same `billing_error` body; no unified headers |
| Responses block (`writeCodexLimitBlocked`) | 429, `retry-after`, `x-should-retry: false`, `usage_limit_reached` body with `resets_at`, the window at 100, `x-codex-promo-message`, credits flags | 429, `retry-after`, `x-should-retry: false`, the same `usage_limit_reached` body, and `x-codex-promo-message`; no other `x-codex-*` header |

**The promo-message exception (owner's decision).** Codex 0.137.0 does not print the body's message; it prints its own "You've hit your usage limit…" line and appends only `x-codex-promo-message`. Without that header a gateway block looks like a ChatGPT plan limit (verified in the #418 stub runs). The header carries no figures, only the text that it is the gateway's quota and whom to ask. It therefore stays on the Codex block with the switch off, and it is the one named exception to "no `x-codex-*` header". Tests and docs state the exception explicitly.

**Blocks:** the block writers currently receive no token, so they get the flags they need as parameters. Their callers in `admitPrincipal` have the token.

**Request context:** the standing is attached to the request context in `inference_handlers.go`. Builder's call: skip computing it when the switch is off, or gate only at write time. Either way nothing is written.

**Body text (owner-approved):** the `billing_error` and `usage_limit_reached` bodies keep their sentence and `resets_at` with the switch off. They are the error itself, and `retry-after` carries the same reset.

### 5.3 Codex popup switch (G3)

When `codex_model_switch_prompt` and `send_usage_limits` are both on, the gateway omits its own `X-Codex-Credits-Has-Credits` / `X-Codex-Credits-Unlimited` on the success standing and on the block. When `send_usage_limits` is off, the flag is moot, because no `x-codex-*` header is written at all.

The vendor's own credits headers, relayed for entitled principals, stay untouched in every case.

The consequence goes in the docs and the portal hint: Codex then shows the popup at 90 % of its default window, including when that window shows the gateway's own limit.

### 5.4 Portal

**User tokens** (`TokenList.tsx`, create and edit):
- A checkbox "Send usage limits to the client". The help text says that clients (Claude Code, Codex CLI) then show the remaining usage. It also says that Claude Code additionally needs the Claude Code token format.
- A checkbox "Codex: offer switching to gpt-5.4-mini", disabled unless the first one is checked. Its help text says that Codex then offers the switch at 90 % of its main window, also when the gateway's own limit is the one shown.
- In the create form, ticking the Claude Code token format also ticks "send usage limits"; the user can untick it.
- A small badge in the list for tokens with `send_usage_limits`. Builder's call: one chip, or reuse the existing "Claude Code" chip area.

**Service tokens** (`ServiceTokensSection.tsx`, create): the same two checkboxes.

**i18n:** German and English together.

### 5.5 Docs

- **`compatibility-and-inference.md`:** update §6 (the header paragraphs) and §9.3–9.6.
  - The statements "The gateway does not look at the shape", "with no token option" and the always-suppress credits text become conditional on the switches.
  - The relevant rows of the error table change too.
- **New ADR**, the next free number (110 at the time of writing), recording:
  - the opt-in;
  - what survives without it;
  - the popup switch and its carry-over consequence;
  - the two-switch decision.

  Mark ADR-106–109 as amended where they state the opposite. Check whether the file has a convention for amendment notes; follow it, or add a one-line "Amended by ADR-110" under each.
- **References:** `security-auth-rbac.md` (token secret section), `api-surface.md` (token field table, tokens route), `openapi.yaml` (token schemas, `/v1/responses` and `/v1/messages` notes), `data-model.md` (`api_tokens` row plus the migration-112 row and its heading count), `persistence.md`, `12-glossary.md`, `external-vendor-accounts.md` (where it says figures go to entitled callers).
- **README:** the coding-agents bullet, if it claims usage display without mentioning the opt-in.

## 6. Constraints

- Stable error codes; status codes and error types of blocks unchanged.
- All three store drivers.
- An append-only migration, with the heading bump and the anchor move for `#4-migration-history-111-migrations`.
- Bump the ServerAgent version only if the agent changes (it should not).
- i18n parity. Repo text in English. Feature branch `feat/420-…`, push over SSH.

## 7. Verification

- **Unit tests per §5.2 row, both flavors, native and translate, buffered and SSE:**
  - With the switch off, no `anthropic-ratelimit-unified-*` and no `x-codex-*` header in the client response, except `x-codex-promo-message` on the Codex block. Positive checks read `rec.Result().Header`.
  - With it on, unchanged behaviour (existing tests keep passing with the switch set).
  - The blocks keep 429, `retry-after`, `x-should-retry: false` and the body with the switch off.
  - The credits flag is absent with the popup switch on and present with it off.
  - The window caches do not observe with the switch off.
- **Store and portal:**
  - The migration-112 test, including the backfill;
  - conformance round-trips;
  - DTO, PATCH and service-token create;
  - `auth.Token` carries both flags on every builder, sqlite and memory.
- **Portal tests:** the checkboxes, the dependency between them, the create-form coupling with the token format, and the badge.
- **End to end (controller):**
  - Claude Code 2.1.328 with a JWT-shaped token: switch off → `rate_limit_event` carries no windows; switch on → it carries them.
  - Codex 0.137.0 `codex exec` against a gateway with a quota: switch off → the rollout carries no rate limits; on → it carries them. With the switch off, an exhausted quota still prints the gateway's promo text.
  - If feasible, the TUI shows the popup only with the popup switch.
- **Gates:** Go fmt / lint / test with the postgres leg (Docker is available now), the four frontend gates, `make lint-docs`, Sonar branch findings.

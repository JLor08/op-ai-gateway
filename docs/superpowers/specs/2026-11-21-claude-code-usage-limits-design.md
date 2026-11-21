# Claude Code usage-limit display through the gateway — design

Issue: JLor08/op-ai-gateway#407. Branch: `agentic/claude-code-limits-7ab4ff`.
Status: draft for owner approval (builder check done; owner decisions on rotation and shared accounts folded in).

## 1. Intent

The owner uses Claude Code mostly in the Claude Desktop Code tab, and also in the terminal, with OP AI Gateway as its Anthropic endpoint. Claude reaches the gateway through an Anthropic subscription vendor account (OAuth). The owner wants Claude Code to show how much of the subscription's **5-hour and 7-day windows** is used, the way it does for a claude.ai subscriber.

Later, a session will mix providers: subagents on local models, and a free choice of which model does which task. The display must survive that: a reply from a local model must not wipe the subscription's numbers.

## 2. What Claude Code can and cannot show

These findings were verified against Claude Code 2.1.328. The issue carries the evidence.

- Claude Code reads `anthropic-ratelimit-unified-*` response headers on `/v1/messages`. The windows `5h`, `7d`, `7d_oi` and `overage` each carry `-<w>-utilization` (a 0..1 fraction), `-<w>-reset` (unix seconds), `-<w>-status` and `-<w>-surpassed-threshold`. The top level carries `-status`, `-reset`, `-representative-claim` and `-overage-*`.
- It reads them only in some sessions. Relevant here: when `ANTHROPIC_BASE_URL` is a non-Anthropic host and `ANTHROPIC_AUTH_TOKEN` is a JWT whose `aud` is or contains `claude-gateway`. The client decodes the payload without verifying it. An opaque token, which is what OP AI Gateway issues today, means the headers are ignored. That is true in Claude Desktop (3P, static key) and in the terminal alike.
- Once Claude Code reads them, the values reach:
  - the terminal status line (`rate_limits.five_hour`, `rate_limits.seven_day`);
  - the near-limit warnings and the SDK `rate_limit_event`, which Claude Desktop renders as limit banners;
  - the 429 handling, where it can tell a plan limit from a throttle.
- Unreachable for any gateway: the Desktop usage card (always `not_applicable` in 3P mode) and the `/usage` plan panel (claude.ai login only).
- A `/v1/messages` response **without** these headers clears the per-window values in the client. This was observed: the top-level status is kept, `unifiedWindows` goes.

## 3. Goals

1. **G1 – forward.** A `/v1/messages` response relayed natively from an upstream carries the upstream's rate-limit headers to the client. This applies to success and error responses, streaming and non-streaming.
2. **G2 – readable token.** A user can create an API token whose secret Claude Code recognises as a gateway token, so that Claude Code reads the headers. The token stays an ordinary opaque bearer for the gateway.
3. **G3 – stable display.** A principal's last-seen subscription windows are attached to that principal's `/v1/messages` responses from other targets until the windows reset.
4. **G4 – scrape fix.** The portal's per-account usage snapshot uses the per-window reset headers.
5. **G5 – shared accounts.** A subscription account's rate-limit figures reach only principals entitled to see them. Those are the principals who get the account's management view today, and the account's route-only members only if the owner opted in (owner's decision).

## 4. Non-goals and anti-goals

- **Non-goal:** the Desktop usage card and the `/usage` plan panel, which are unreachable.
- **Non-goal:** Anthropic's Claude apps gateway sign-in protocol (device flow, `/login`, `/managed/settings`). It adds no display beyond G2 and costs features: a gateway-credential session sends fewer betas.
- **Non-goal, follow-up issue:** reporting the gateway's own principal limits (request/token quotas, cost budget) in the documented `overage` / spend-limit shape, and the matching `429` shape (`retry-after`, `x-should-retry: false`).
- **Non-goal:** forwarding Codex `x-codex-*` headers on `/v1/responses`.
- **Non-goal:** service tokens. They serve unattended pipelines where nobody sees a display.
- **Anti-goal:** forwarding anything beyond the allowlist. In particular no `request-id`, no `anthropic-organization-id`, and no classic `anthropic-ratelimit-requests-*` / `-tokens-*` (API-key organization quotas).
- **Anti-goal:** changing how the gateway authenticates or stores tokens. The secret is still only hashed, never parsed.
- **Anti-goal:** a display that claims a local response was refused. Attached data never says top-level `rejected`.

## 5. Design

### 5.1 G1 – forward the rate-limit headers on native passthrough

In `proxyNative` (`gateway/backend/internal/gateway/native_passthrough.go`), directly before `w.WriteHeader(resp.StatusCode)`, copy the allowlisted headers from the upstream response to the client response.

- **When:** only when the request's API flavor is `anthropic_messages`. The target does not matter: a self-hosted upstream simply sends none. Images, speech and transcription return earlier through their own relays and are untouched.
- **Allowlist:** every header whose lowercased name starts with `anthropic-ratelimit-unified-`, plus `retry-after` and `x-should-retry`. All values are copied verbatim. Nothing else is copied. For a vendor-account target, the `anthropic-ratelimit-unified-*` part is forwarded only to an entitled principal (§5.5).
- **Which response:** on the instance-gone re-selection retry, only the final upstream response's headers count.
- **Capture:** forwarded headers are response headers and so enter a payload capture like the others. The builder confirms that capture header redaction needs no change, since none of these headers are secrets.

Update the architecture text that today says "No other inbound header is forwarded. The response and its SSE are relayed unchanged" so it states the response-header allowlist. The text lives in `external-vendor-accounts.md` (two places) and in `compatibility-and-inference.md` §6.

### 5.2 G4 – scrape per-window resets

In `scrapeAnthropicUsage` (`gateway/backend/internal/gateway/vendor_usage_scrape.go`):

- `-5h-reset` sets `FiveHourResetAt` and `-7d-reset` sets `WeeklyResetAt`.
- The top-level `-reset` is used only as the five-hour fallback, and only when `-5h-reset` is absent and `-representative-claim` is `five_hour`. Builder's call: whether `-reset` is also used as the weekly fallback for `seven_day`.
- Update the table in `external-vendor-accounts.md` §5.1 and drop the "VERIFY-LIVE" note on resets. Claude Code itself parses `-5h-reset` and `-7d-reset`.

### 5.3 G2 – opt-in Claude Code token format

A new boolean token setting on **user tokens**, working name `claude_code_usage_display`, off by default.

**Secret shape when on:**

```
<base64url(`{"alg":"none","typ":"JWT"}`)>.<base64url(`{"aud":"claude-gateway"}`)>.opaigw_<64 lowercase hex>
```

- base64url without padding; the JSON is exactly as shown, so the first two segments are constant.
- The third segment is today's secret, `opaigw_` plus 32 random bytes in hex. Every character of it is in the base64url alphabet.
- No `exp`, `sub` or other claims. Claude Code reads `exp`, and an expiry claim on a static key would invite client-side refresh or expiry logic.
- Verified: exactly this payload, with an `opaigw_…` third segment, makes Claude Code 2.1.328 emit `rate_limit_event`.

**Gateway handling:**

- Unchanged: the whole string is hashed with SHA-256 and looked up; nothing parses, decodes or verifies the JWT shape.
- `alg: none` is harmless because no party verifies a signature. The bearer secret is the random third segment. The security doc says so explicitly.

**Lifecycle:**

- **Invariant:** the stored setting always matches the shape of the token's current secret.
- **Create:** the setting can be set at create time.
- **Rotate:** `RotateToken` regenerates the secret in the shape the stored setting names.
- **Change on an existing token (owner's decision):** allowed in both directions, but only together with a rotation, because the shape of a secret cannot change in place.
  - The rotate endpoint `POST /api/portal/tokens/{id}/rotate` takes an optional JSON body `{"claude_code_usage_display": bool}`. With it, the rotation stores the new setting and issues the new secret in the matching shape in one store write. Without a body, rotation behaves as today and keeps the shape.
  - `PATCH /api/portal/tokens/{id}` accepts the field only with the value already stored, as a no-op. That way a client that reads a token and writes it back keeps working, since the flag is always emitted like the other opt-ins. A different value is refused with a clear 400 that points to rotation, so a flag can never disagree with the secret. This is my call.
- **Portal edit form:** the checkbox is editable. When its value differs from the stored one, a warning says that saving rotates the token and the old secret stops working. On save:
  1. The portal PATCHes the other changed fields, as today.
  2. It then calls rotate with the new value.
  3. It shows the new secret in the existing one-time dialog.

  Builder's call: whether the portal asks for an extra confirmation before rotating, in the style of the existing rotate `ConfirmDialog`.
- **Storage:** the setting is persisted in a new `api_tokens` column `claude_code_usage_display integer not null default 0` through an append-only migration for all three drivers.

**Permissions:** ungated, like `decisions_confidence_normalize` and `claude_model_aliases`. Any principal that may create the token may set it, because the setting grants no capability: it changes only the secret's shape.

**Display:**

- The portal shows the setting in the token create form, with a short help text saying it makes Claude Code show plan usage limits, and as a badge in the token list.
- `secret_prefix` stays `secret[:8]` (here constant `eyJhbGci`). The portal does not render a user token's prefix today (`TokenList.tsx` columns), so nothing changes visibly.

**Implementation touchpoints.** Template: the newest ungated per-token bool, `decisions_confidence_normalize` (commit `84f8f949`, migration 108). Paths are under `gateway/backend/internal/`.

- **Store:**
  - `store/models.go` `TokenRecord`.
  - `store/migrate.go`: the next free migration version, 110 at the time of writing. Re-check main before merging, because serial PRs collide on numbers. Use `addColumnIfMissing` as in `migration108Up`.
  - `store/sqlite_token.go`, which is shared by sqlite and postgres: the INSERT, `tokenColumns` and `scanToken`. Also the secret-rotation write, which must be able to set the new column together with the new hash. Not the metadata UPDATE: PATCH does not carry the field.
  - The memory driver in `portal/memory_directory.go` keeps the record whole.
- **Store tests:**
  - A conformance round-trip.
  - A new migration test copied from `migration108_token_decisions_confidence_normalize_test.go`.
  - Add the column to the migration-40 rebuild test's re-add list in `store/conformance_test.go`.
- **Service:** in `portal/service.go`:
  - `TokenDTO`, `CreateTokenRequest` and `tokenDTO`.
  - `CreateToken`: wrap the output of `s.secretGenerator()` in the JWT shape, so tests that inject a generator keep working and the third segment stays today's secret.
  - `RotateToken`: pick the shape from the stored record.
- **Must stay unchanged:** service tokens (`service_services.go` create and rotate), agent tokens (`service_agent_tokens.go`), the runtime-spec random key (`service_runtime.go`, which calls `generateSecret()` directly), and every service-token DTO and UI.
- **Inbound auth:** the gateway accepts only `Authorization: Bearer` on inference, not `x-api-key`. Claude Code therefore uses `ANTHROPIC_AUTH_TOKEN`, which Claude Desktop's static gateway key also maps to.
- **Portal:**
  - `src/api/tokens.ts`: `PortalToken`, `CreateTokenRequest`, and an optional body on `rotateToken`.
  - `src/components/TokenList.tsx`: create state and body, the checkbox block next to the other opt-ins, the editable checkbox in edit with the rotation warning and the PATCH-then-rotate save, and a badge column or chip.
  - `src/i18n.ts`, German and English. Type-level parity is enforced.
  - Tests in `TokenList.test.tsx`, `i18n.test.ts` and `api.test.ts`.

**Docs:**

- `compatibility-and-inference.md` §9.3 "Claude Code usage-limit display": what it does, the client condition, the token shape, what Claude Code shows and does not show, and the dependency on undocumented client behaviour.
- The setting in `security-auth-rbac.md` (token secret formats, and the `alg: none` note), `reference/api-surface.md` (tokens route and field table), `reference/openapi.yaml` (token list, create and rotate), and `reference/data-model.md` (`api_tokens` row and migration row).
- `cross-cutting/persistence.md` (the sibling paragraph of the other token opt-ins) and `12-glossary.md`.
- An ADR in `09-architecture-decisions.md`, using the next free number (106 at the time of writing).
- A risk entry in `11-risks-and-technical-debt.md`: the client heuristic is undocumented; if it breaks, only the display disappears.

### 5.4 G3 – keep the subscription windows on other responses

The gateway keeps an in-memory, per-process, bounded map from **principal** (`principalFor`: user or service) to that principal's last subscription observation: the `anthropic-ratelimit-unified-*` headers of one response plus the time they were seen.

- **Update:**
  - **Which responses:** whenever a response from a target with `Provider == routing.ProviderVendorAnthropic` carries `anthropic-ratelimit-unified-status`, and the principal is entitled to the account's figures (§5.5), it replaces the principal's entry. That holds for 2xx and for 429, on native passthrough and on the translate path.
  - **Where:** the natural choke point is where the vendor scrape already receives the response headers (`recordUsage` → `scrapeVendorAccountUsage`). For the current response, G1 already forwards the headers. The update only needs to be in place before the principal's next request.
- **Attach:**
  - **When:** on a `/v1/messages` response for a principal that has an entry, when the upstream response itself carries no `anthropic-ratelimit-unified-status`. This covers native passthrough to non-Anthropic targets and the translate path. It applies to streaming and non-streaming responses, on 2xx only.
  - **What:** for each window `w` whose `-<w>-reset` lies in the future, its `-<w>-utilization`, `-<w>-reset`, `-<w>-status` and `-<w>-surpassed-threshold` as observed.
  - **Top-level status:**
    - `-status` is `allowed_warning` if any attached window's status is `allowed_warning` or `rejected`, else `allowed`. It is never `rejected`.
    - `-representative-claim` and `-reset` are copied only if they refer to an attached window.
    - Never attached: `-overage-*`, `-fallback-percentage`, the grace or slow families, `retry-after`, `x-should-retry`.
  - **Nothing attached:** when no window is still in the future, attach nothing and drop the entry.
  - **Where it is set:** the attach must happen before the response headers are written, in three places:
    - before `WriteHeader` in `proxyNative`;
    - before the SSE header block in `stream_session.go`, which `completeStreamAnthropic` reaches;
    - before the buffered JSON write of the translate path's `complete` when it renders an Anthropic Messages response.

    Only the `anthropic_messages` flavor gets attached headers. Chat completions and Responses are untouched.
- **Bounding:** entries whose windows have all reset are pruned. Builder's call: the exact bound (for example prune on write plus a hard cap). It must not grow without limit.
- **Scope note:** multiple gateway replicas each keep their own map. That is best effort and acceptable, because the next subscription response refreshes it. The architecture doc says so.

### 5.5 G5 – who may see a shared account's figures

Today `external-vendor-accounts.md` §8.2 withholds `usage` ("the owner's rate-limit and spend figures") from a shared account's **member view**. G1 and G3 must not reopen that through response headers.

- **Entitled principal:** for the serving vendor account, a principal is entitled when either holds:
  - it gets the management view today. That is the owner, a CM of a `shared` or `full` account by its effective level, or system scope. This is the same decision as `authorizeVendorAccountRead` in `portal/service_vendor_accounts.go`, and it must not be re-derived differently.
  - the account's new opt-in, `share_usage_with_members` (boolean, default off), is on.
- **G1 for a non-entitled principal:** only `retry-after` and `x-should-retry` are forwarded, so Claude Code still waits correctly on a 429. All `anthropic-ratelimit-unified-*` headers are dropped.
- **G3 for a non-entitled principal:** no observation is recorded for that principal, so nothing is attached later either.
- **The opt-in:**
  - Only the account's **owner**, the sharer, may set it. It follows the share-level write's owner rule in `portal/service_vendor_sharing.go`: a CM or a member is forbidden, a stranger gets not-found.
  - It can be set at any share level, and has an effect only while the account is shared.
  - It is persisted in a new `vendor_accounts` column through an append-only migration.
- **Portal member view (owner's decision):** when the opt-in is on, everyone allowed to use the account sees its figures in the portal too, including the dashboard.
  - `memberVendorAccountDTO` carries `usage` exactly when the opt-in is on, which means reading the snapshot it skips today.
  - That builder renders both the single read and the member-only rows of the list. The dashboard widget `gateway/frontend/src/components/DashboardVendorUsage.tsx` reads the list, so it shows the figures without its own change. The builder confirms that `canReportUsage` passes for a member row.
  - One setting has one meaning across Claude Code, the account page and the dashboard.
- **UI:**
  - The account's sharing section offers the owner a checkbox, "share rate-limit figures with members", with a help text naming both effects.
  - German and English keys.
- **Request path, builder's call:** how the gateway learns entitlement cheaply. For example, the resolver could stamp the target, or a short-lived cache keyed by principal and account could hold the decision. It must not add an uncached store round trip to every `/v1/messages` request, and it must fail closed: when entitlement cannot be determined, treat the principal as not entitled.
- **Non-vendor targets** (self-hosted) are unaffected. G1 forwards their allowlisted headers to everyone, because they are not an account's figures.
- **Docs:**
  - `external-vendor-accounts.md` §8.2 (member view and the opt-in) and the sharing section.
  - `api-surface.md`, `openapi.yaml` and `data-model.md`.
  - The ADR covers this rule as well.

### 5.6 What the owner sees afterwards

With a token created with the new setting and pasted into Claude Desktop (3P gateway, static key) or used as `ANTHROPIC_AUTH_TOKEN` in the terminal:

- Desktop: limit banners as the subscription approaches its limits.
- Terminal: `rate_limits.five_hour` and `seven_day` in the status line script input, plus the warnings.
- Both: on an upstream 429, Claude Code receives the reset time and `retry-after`.
- Not shown: the Desktop usage card and the `/usage` plan panel.

## 6. Constraints

- Keep error codes stable; G1 adds headers only.
- All three store drivers; the migration is append-only, verified with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set.
- Portal i18n keys in German and English together.
- No change to the server agent, so no agent version bump.
- `internal/compat` stays the only place for dialect mapping. The header allowlist is relay plumbing, not translation, so it lives in the gateway package.
- The architecture docs are updated in the same branch. `docs/superpowers/` is removed before the PR.

## 7. Verification

- Unit tests:
  - G1 allowlist: stream and non-stream, 200 and 429; non-allowlisted headers do not leak; the flavor gate holds.
  - G4 resets.
  - G2: the secret shape (decode the payload, regex the third segment), bearer authentication of a JWT-shaped token, rotation keeps the shape, rotation with a body switches the shape in both directions and stores the setting, a PATCH carrying a different value gets a 400 (the same value is a no-op), the store round-trip of the new column on all drivers.
  - G3: update, attach, expiry, never `rejected`, bounded size.
  - G5: the owner, a CM and system scope get the figures; a member gets only `retry-after`/`x-should-retry` with the opt-in off and the figures with it on; G3 records nothing for a non-entitled member; only the owner may set the opt-in; the member DTO carries `usage` exactly when the opt-in is on; the migration round-trips on all drivers.
- Portal: unit tests for the form option and the badge; `npm test`, `build`, `lint` and `format:check`.
- End to end, run by the controller and not in CI:
  - Setup: a locally started gateway (memory mode), a loopback stub upstream that answers `/v1/messages` with rate-limit headers, registered as a self-hosted application with Messages passthrough, and a user token created with the new setting.
  - Check: Claude Code 2.1.328 `-p --output-format stream-json --verbose` must emit `rate_limit_event` with the stub's windows. With an opaque token it must emit none.
  - This covers G1 and G2. G3 records only from `vendor_anthropic` targets, whose endpoint is fixed to `https://api.anthropic.com`, so a stub cannot stand in for it end to end. G3 is verified by in-process tests that build a vendor `Target` against an `httptest` server. My call: no test-only base-URL override is added, since that would be a new production config surface.

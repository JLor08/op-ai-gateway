# Design — External Vendor Accounts ("Anbieter")

Status: draft for review · Branch: `vendor-accounts` · Date: 2026-10-07

## 1. Summary

Add a new portal area — menu label **"Anbieter"** (de) / **"Providers"** (en) —
through which a portal user connects an **external AI vendor account** and routes
their own requests through it, alongside the existing self-hosted **AI Server**
backends.

Two vendors at launch — **OpenAI** and **Anthropic** — each reachable through
either of two auth types:

- **`api_key`** — the user pastes a platform API key (`sk-…` / `sk-ant-…`). Cheap,
  documented, stable, legal (the user's own metered key).
- **`subscription`** — the user connects a consumer subscription (ChatGPT
  Plus/Pro, Claude Pro/Max) via an OAuth login, so inference is served against
  the vendor's subscription backend. Reverse-engineered, undocumented, and
  restricted by both vendors' consumer Terms (see §3).

A connected account is **personal**: owned by the connecting user and usable only
for that user's own requests (portal chat and the user's own API tokens). Sharing
an account with multiple users via **resource groups** is deliberately deferred to
a later phase (§2).

The feature reuses the gateway's existing credential-sealing, routing, dispatch,
and usage machinery wherever possible; the genuinely new parts are a first-class
account entity, an OAuth subsystem (connect + token refresh), a small native
Anthropic Messages client, two static dispatch extensions (extra headers + a
system-prompt masquerade), and a usage/limits panel fed by scraping the vendors'
rate-limit response headers.

## 2. Scope

### In scope (this spec — "Spec 1 + 2" combined)

- The `vendor_account` entity across all three store drivers, with migration and
  conformance tests.
- A new portal menu item and CRUD/connect UI, visible to every authenticated
  portal user, each managing only their **own** accounts.
- The **API-key** serving path for both vendors.
- The **subscription-OAuth** serving path for both vendors, including the connect
  flows (OpenAI device-code, Anthropic code-paste, token-import as a quick start)
  and background token refresh.
- Routing integration so a user's connected account becomes a routable candidate
  **scoped to that user**, reachable from both portal chat and the user's own API
  tokens.
- A curated per-vendor model catalog.
- A **Usage & Limits** panel: the cheap subscription rate-limit windows (5-hour +
  weekly) scraped from vendor response headers.

### Out of scope (later phases)

- **Admin-managed accounts shared via resource groups** (Phase 3). The entity and
  ownership model are designed so this is additive (a sharing link table + a
  resolver filter extension), not a rewrite.
- **Per-model price table and € / $ cost accounting** for the API-key path. For
  now the API-key usage panel shows throughput headroom only (from rate-limit
  headers); real spend accounting is a later step.
- Any change to `server-agent` (it is not involved).

## 3. Explicit design assumptions

These are recorded deliberately; the implementation is built around them, not in
denial of them.

1. **ToS / enforcement.** The subscription path reuses consumer-subscription
   OAuth tokens for inference through a third-party gateway. This is **against
   both vendors' consumer Terms** and Anthropic enforces it server-side (the
   "credential is only authorized for use with Claude Code" gate; the forced
   `You are Claude Code` system block). The feature is built **for internal
   testing with the operator's own account**, as an explicitly experimental,
   **feature-flagged, disable-able** capability. It must degrade gracefully when
   a vendor blocks or changes behavior, and must never be presented as a
   supported, multi-tenant production capability.
2. **Undocumented surface.** Every subscription endpoint, header, client-id, beta
   header, and the masquerade requirement is reverse-engineered and can change
   without notice. All parsing is tolerant (prefix-matched, fail-open); all
   vendor-specific constants live in one place, easy to update.
3. **Cipher key required.** Subscription tokens (access + refresh) are decryptable
   secrets at rest, so the gateway **must** have `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY`
   configured; a keyless disk-backed store rejects the write (`capture.ErrKeyRequired`),
   consistent with the existing secret-at-rest rule.
4. **Privacy.** The operator's real gateway hostname is never written into repo
   text (spec, code, docs, commits, PR). Use the placeholder `<gateway-host>`.

## 4. Terminology and naming

The word **"provider" is already heavily overloaded** in this codebase — it names
the backend client-adapter layer (`internal/provider`), the `Application.Type`
discriminator (`routing.ProviderOllama` etc.), `Target.Provider`,
`usage.Event.Provider`, and the agent-reported `provider_health`. The new concept
(an external vendor *account/subscription*) is different and must not collide.

Decision:

- Code / wire / schema name: **`vendor_account`** (type `VendorAccount`, id prefix
  `va_`). Enums: **`vendor` ∈ {`openai`, `anthropic`}**, **`auth_type` ∈
  {`api_key`, `subscription`}**.
- UI label: **"Anbieter"** (de) / **"Providers"** (en).
- The existing `internal/provider` adapter package is **left untouched** (renaming
  it would be a broad, unrelated refactor, which the repo rules forbid). The new
  entity lives beside it and *reuses* its clients to talk to the vendor clouds.

## 5. Data model

New tables (append-only migration, next free version in
`internal/store/migrate.go`; `<ts>` = `dl.timestampType()`; floats
`double precision`; sealed secrets `text not null default ''`).

### 5.1 `vendor_accounts`

| Column | Type | Notes |
|---|---|---|
| `id` | text pk | `va_` + compact random hex |
| `owner_user_id` | text not null | FK → `users(id)` `on delete cascade` |
| `vendor` | text not null | `openai` \| `anthropic` |
| `auth_type` | text not null | `api_key` \| `subscription` |
| `name` | text not null | user-facing label |
| `status` | text not null | `active` \| `disabled` |
| `api_key` | text not null default '' | sealed (`enc:`/`plain:`); used when `auth_type=api_key` |
| `oauth_tokens` | text not null default '' | sealed JSON `{access, refresh, expires_at, account_id, plan_type, scope}`; used when `auth_type=subscription` |
| `created_at` / `updated_at` | `<ts>` not null | |

Exactly one of `api_key` / `oauth_tokens` is populated per row (enforced in the
service, not the schema).

### 5.2 `vendor_account_models`

Join table: which catalog models the account offers.

| Column | Type | Notes |
|---|---|---|
| `account_id` | text not null | FK → `vendor_accounts(id)` `on delete cascade` |
| `gateway_model` | text not null | public model name routed to this account |
| `upstream_model` | text not null | the vendor's real model id |
| `api_flavor` | text not null | `openai` \| `anthropic` |
| pk | (`account_id`, `gateway_model`) | |

Default rows = the full curated catalog for the account's vendor (see §8.3);
rows are removable so a user can narrow what the account exposes.

### 5.3 `vendor_account_usage`

Latest usage snapshot per account (written by the header scraper, §9.4).

| Column | Type | Notes |
|---|---|---|
| `account_id` | text pk | FK → `vendor_accounts(id)` `on delete cascade` |
| `five_hour_pct` | double precision | 0–100, −1 = unknown |
| `five_hour_reset_at` | `<ts>` nullable | |
| `weekly_pct` | double precision | 0–100, −1 = unknown |
| `weekly_reset_at` | `<ts>` nullable | |
| `credit_balance` | text not null default '' | opaque (OpenAI overage); '' = none |
| `updated_at` | `<ts>` not null | |

### 5.4 `usage_events.account_id` (additive column)

Add nullable/`''`-default `account_id` to `usage_events`, set in
`recordUsage` (`internal/gateway/inference_complete.go:650`) from the resolved
target when the target is a vendor account. Needed so two accounts of the same
vendor are distinguishable in analytics (`Provider` + `Host` alone cannot).

### 5.5 Store + test obligations

- Domain type `routing.VendorAccount` (+ a `VendorAccountStore` interface) added to
  the composite `routing.Store`.
- Implement in `routing/memory_store.go` (map + deep-copy) and as SQL methods on
  the single `store.SQLStore` (both dialects; bool→int handled by `sanitizeArgs`).
- New tests mirroring the `ai_server_*` ones: column-parity, schema-coverage,
  store conformance (memory + sqlite + **postgres**, provisioned like CI via
  `OP_AI_GATEWAY_TEST_POSTGRES_DSN`), and delete-cascade (deleting an account and
  deleting its owner cascade the child rows).

## 6. Credentials and sealing

Reuse the repo-wide secret envelope verbatim — no new key, no new mechanism:

- Seal on write with `capture.SealSecret(s.cipher, s.settingsVolatile, plain)`
  **before** the store write (so a keyless-disk rejection leaves nothing
  half-written). Open with `capture.OpenSecret` only at the cipher-holding
  dispatch edge.
- Read-back DTO exposes presence only: `api_key_set bool` /
  `subscription_connected bool` — never the secret.
- PATCH uses the keep/clear/replace sentinel (`*string`: nil = keep, "" = clear,
  value = replace + reseal), exactly as `applications.api_token`
  (`service_applications.go`) and `runtimeSpecAPIToken` (`service_runtime.go:1124`)
  do today.
- Subscription `oauth_tokens` is the same envelope wrapping a JSON blob; refresh
  (§7.4) re-seals in place.

## 7. OAuth / connect flows

All vendor OAuth constants (client-ids, authorize/token URLs, scopes, beta
headers, loopback/redirect values, device-code endpoints) live in one file,
clearly marked reverse-engineered.

### 7.1 Anthropic — authorization-code + PKCE, manual code paste

Suited to a remote gateway (no callback reaches the server).

1. Backend generates PKCE verifier + `state`, stores them server-side keyed to the
   user (short-lived; a `vendor_account_oauth_pending` row or in-memory map).
2. Portal shows the authorize URL: `https://claude.ai/oauth/authorize` with
   `code=true`, PKCE `S256`, Claude Code client-id, scope
   `org:create_api_key user:profile user:inference`.
3. User authorizes; the Anthropic console shows the code; user pastes `code#state`
   into the portal (split on `#`).
4. Backend exchanges at `https://console.anthropic.com/v1/oauth/token` (JSON body),
   seals `{access (sk-ant-oat01-…), refresh (sk-ant-ort01-…), expires_at, scope}`.

### 7.2 OpenAI — device-code (primary), manual paste (fallback)

Device-code needs no callback to the gateway, so it fits a remote server best.

1. Backend requests a device/user code from the OpenAI auth host; portal shows the
   verification URL + user code.
2. Backend polls the token endpoint until authorized or timeout.
3. On success, seal `{access, refresh, expires_at, account_id, plan_type}`; read
   `chatgpt_account_id` and `chatgpt_plan_type` from the `id_token` JWT (claims
   namespace `https://api.openai.com/auth`).

Fallback if the device flow is unavailable for the client-id: the loopback-redirect
authorize URL, with the user copying the redirected `code`/`state` back into the
portal (same paste UX as Anthropic).

### 7.3 Token import (quick start, both vendors)

User pastes an already-obtained OAuth access (+ optional refresh) token; backend
seals it and runs a probe call to validate. Fastest path to a testable connection
without implementing the full browser dance first; also the natural first
implementation milestone.

### 7.4 Refresh

- **Lazy refresh before dispatch**: if the sealed access token is within a small
  buffer of `expires_at`, refresh via `grant_type=refresh_token` (same client-id),
  re-seal, persist — under a per-account lock to avoid concurrent refreshes.
- Optional periodic sweep for idle accounts.
- A refresh that fails (revoked / expired refresh token) flips the account to a
  `needs_reconnect` state and surfaces in the UI; dispatch returns the existing
  `provider.ErrAuthRejected`.

## 8. Routing integration

Goal: a connected account becomes a routable candidate **without touching the hot
scoring path, the multiplexer, or the `Target` core** beyond the small dispatch
extension in §9.1.

### 8.1 New candidate source

The resolver (`internal/routing/resolver.go`) gains a vendor-account candidate
source consulted during fresh selection. For a resolve carrying a known principal:

1. Enumerate the principal's active vendor accounts (Phase 1: owned by the
   principal; Phase 3 will add resource-group-shared accounts).
2. For each account, match the requested gateway model against its
   `vendor_account_models` rows for the requested API flavor.
3. Synthesize an **in-memory** candidate in the existing `MappingCandidate` shape
   (not persisted), carrying: endpoint (vendor host), upstream model id, the
   account's credential, the api flavor + mode, and the new dispatch extras
   (static headers + masquerade flag, §9.1).

Scoring, admission, and the multiplexer see an ordinary candidate.

### 8.2 Ownership filter

A vendor-account candidate is eligible **only for its owner** (Phase 1). This
extends the existing provisioning filter (`filterProvisioned`) with an
account-ownership check. Principals are known on both serving paths: portal chat
(session user) and API tokens (token's user). Requests with no user principal
(e.g. bootstrap tokens) simply see no personal accounts.

### 8.3 Curated model catalog

A static per-vendor catalog in code (vendor-native names, e.g. current
`claude-…` / `gpt-…` ids) seeds `vendor_account_models` on connect. Vendor-native
names avoid collisions with self-hosted model mappings. Dynamic discovery
(`GET /v1/models`) is reserved for the API-key path in a later step; subscription
backends do not expose a reliable model list.

## 9. Dispatch / vendor specifics

### 9.1 The one real core extension

Extend the dispatch credential carrier with two static pieces:

- **Extra static headers** — a small ordered set attached to the outgoing upstream
  request (today `applyUpstreamAuth` sets exactly one header; `doNativeProxy`
  sets only Content-Type + that one). Threaded via `Target` →
  `provider.WithUpstreamAuth`-adjacent context, applied in
  `internal/provider/upstream_auth.go` and the native proxy.
- **System-prompt masquerade flag** — when set (Anthropic subscription), the
  request's system content is prefixed with the required
  `You are Claude Code, Anthropic's official CLI for Claude.` block.

### 9.2 Anthropic

- Always `anthropic-version: 2023-06-01`.
- **api_key**: credential under `x-api-key` (custom header, already supported).
- **subscription**: `Authorization: Bearer <oat>` + `anthropic-beta:
  oauth-2025-04-20,…` + the masquerade block.
- Portal chat builds requests itself and Anthropic serves only `/v1/messages`
  (not `/v1/chat/completions`), so a **small native Anthropic Messages client** is
  added in `internal/provider` (emit `/v1/messages`, parse the response/SSE into
  the neutral `inference` model, reusing `internal/compat/anthropic.go` as the
  shaping reference). Claude-Code-flavored inbound traffic continues to use native
  passthrough. This client is the only substantial new dispatch code.

### 9.3 OpenAI

- **api_key**: `api.openai.com` via the existing `OpenAICompatibleClient`
  (`/v1/responses` or `/v1/chat/completions`), `Authorization: Bearer`.
- **subscription**: `https://chatgpt.com/backend-api/codex/responses` (Responses
  protocol only) with `chatgpt-account-id`, `OpenAI-Beta: responses=experimental`,
  `originator: codex_cli_rs`.

### 9.4 Usage header scraping

At the single `recordUsage` choke point
(`internal/gateway/inference_complete.go:650`), when the target is a vendor
account, parse the upstream response headers already in scope (native path:
`ProxyResponse.Header`; translate path: the capture-sink headers or an explicit
plumb) and upsert `vendor_account_usage`:

- Anthropic subscription: `anthropic-ratelimit-unified-{5h,7d}-utilization`
  (fraction 0–1 → ×100), `-reset` (epoch), `-representative-claim`.
- OpenAI subscription: `x-codex-primary-used-percent` / `-reset-at` (5h),
  `x-codex-secondary-used-percent` / `-reset-at` (weekly),
  `x-codex-credits-balance`. Also the `codex.rate_limits` SSE event.

Parsers are tolerant (prefix match; missing → leave `−1`/unknown, never treat as
0). No extra request is needed — this piggybacks on traffic the gateway already
proxies.

## 10. Usage & Limits (this spec)

- **Subscription accounts**: two progress bars — **5-hour** and **weekly** — each
  "% used + resets in …", from `vendor_account_usage` (header-scraped). These are
  percentages only; absolute caps are not exposed by either vendor. Optional
  credit balance line (OpenAI overage).
- **API-key accounts**: throughput headroom (from the standard rate-limit headers)
  only in this spec; € / $ spend accounting is deferred.
- Reuse the existing used/threshold display component
  (`gateway/frontend/src/components/shared/LimitsEditor.tsx`) pattern where it
  fits; the 5-hour rolling window is shown from the scraped snapshot (the internal
  limit subsystem has no 5-hour calendar period).

## 11. Portal UI

- New `View` union member (`shared/types.ts`) + one `viewRegistry` entry
  (`components/views.tsx`) + i18n keys in **both** `de` and `en` (`i18n.ts`;
  compile-time parity enforced).
- **Visibility: every authenticated portal user** (not admin-only); each user sees
  and manages only their own accounts. (Admin-managed/shared accounts are Phase 3.)
  Behind an experiment feature flag so the whole area can be disabled.
- Page modeled on `ResourceGroupsView.tsx` (self-fetching, `useResource`, list +
  create + detail): list my accounts → "Anbieter verbinden" → choose vendor +
  auth type → **adaptive form**:
  - `api_key`: a single write-only key field (set/replace/clear sentinel, masked
    `•••• set`), like `ApplicationSection`.
  - `subscription`: a "Verbinden" button that drives the OAuth flow (device-code
    display for OpenAI; authorize-URL + `code#state` paste for Anthropic; or token
    import).
  - "Verbindung testen" (probe) before use.
- Per-account detail: status, offered models, and the Usage & Limits panel.
- New `api/vendorAccounts.ts` client module + barrel wiring, per the frontend
  layering rule (`arch.test.ts`).

## 12. Security & RBAC

- HTTP handlers gate on the standard web scope, then per-object authz in the
  service: a user may read/manage only accounts where `owner_user_id` == principal
  (404-no-leak for others), mirroring `authorizeServer`.
- Secrets never leave the backend (presence booleans only).
- Routing enforces the ownership filter (§8.2) so one user's account can never
  serve another user's request in Phase 1.

## 13. Configuration

- Requires `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY` for the subscription path
  (documented in `reference/config-env.md`).
- An experiment feature flag (env + system setting) to enable/disable the whole
  "Anbieter" area and the subscription path independently.
- All reverse-engineered vendor constants in one override-friendly location.

## 14. Testing strategy (TDD)

- **Store**: column-parity, schema-coverage, conformance (memory/sqlite/postgres),
  delete-cascade for all new tables + the `usage_events.account_id` column.
- **Credentials**: seal/mask/keep-clear-replace; keyless-disk rejection.
- **OAuth**: PKCE generation, code-paste exchange, device-code poll, token import,
  refresh + re-seal, refresh-failure → `needs_reconnect` — all against `httptest`
  servers.
- **Dispatch**: extra-header injection, Anthropic masquerade block, `x-api-key` vs
  bearer selection, the native Anthropic Messages client (request shaping +
  response/SSE parse) against `httptest`.
- **Usage scraping**: tolerant parsing of both vendors' header families and the
  `codex.rate_limits` SSE event; unknown → −1 not 0.
- **Routing**: ownership filter (owner sees candidate, non-owner does not);
  catalog model match; in-memory candidate construction.
- **Frontend**: i18n de/en parity, `arch.test.ts` layering, view render, the
  adaptive connect form, secret masking.

## 15. Documentation updates (fold into `docs/architecture/` before PR)

- New **ADR**: the `vendor_account` entity + the ToS design-acceptance (also
  catalogued under §11.4 risks/technical-debt).
- `reference/data-model.md`: the new tables + the `usage_events.account_id` column.
- `05-building-block-view.md`: the new portal menu area.
- A new cross-cutting doc **"External Vendor Accounts"** (entity, OAuth, dispatch
  extensions, usage scraping, ToS/experimental status) — or a clearly-scoped
  section folded into the routing/compatibility docs.
- `reference/api-surface.md` + `openapi.yaml`: the new `/api/portal/vendor-accounts`
  surface.
- `reference/config-env.md`: the feature flag + the cipher-key requirement.
- README/screenshots only if the menu appears in a documented screenshot.

## 16. Risks & open questions

- **ToS / server-side blocking** (§3.1) — the dominant risk; mitigated by the
  experimental framing, graceful degradation, and tolerant parsing, not solved.
- **OpenAI device-code availability** for the Codex client-id is unconfirmed;
  the manual-paste fallback (§7.2) covers the case where it is not offered.
- **Anthropic plan/tier and absolute caps** are not retrievable — the panel shows
  percentages + reset only.
- **Model catalog drift** — vendor model ids change; the catalog is a
  single-file, easily-updated constant.
- Open: exact precedence when a catalog model name also matches a self-hosted
  mapping (default plan: vendor-native names avoid overlap; revisit if a conflict
  is desired for failover).

## 17. Milestones (for the implementation plan)

1. Entity + store (3 drivers) + migration + tests; `usage_events.account_id`.
2. Portal CRUD + menu + i18n + API-key connect (no routing yet) + tests.
3. API-key serving path end-to-end (OpenAI via existing client; Anthropic native
   Messages client) + routing candidate source + ownership filter + portal-chat
   and API-token tests.
4. Subscription connect: token-import first, then Anthropic code-paste, then
   OpenAI device-code; sealing + refresh.
5. Subscription serving: dispatch extras (headers + masquerade) + endpoints.
6. Usage & Limits: header scraping + snapshot + UI panel.
7. Docs fold-in + working-file cleanup + PR.

## 18. Key existing touchpoints (reference)

- Entity/store template: `routing.AIServer` + `ServerStore`
  (`internal/routing/store.go`), `internal/routing/memory_store.go`,
  `internal/store/` (SQL), `internal/store/migrate.go`.
- Secret pattern: `internal/capture/secret.go`, `service_applications.go`
  (api_token), `service_runtime.go:1124` (`runtimeSpecAPIToken`).
- Dispatch/auth: `internal/provider/upstream_auth.go`,
  `internal/provider/openai_compatible.go`, `internal/provider/proxy.go`,
  `internal/routing/resolver.go` (`Target`, `targetFrom`),
  `internal/gateway/server.go:1717` (`upstreamAuthCtx`).
- Usage choke point: `internal/gateway/inference_complete.go:650` (`recordUsage`);
  `internal/usage/`.
- Frontend: `components/views.tsx`, `components/shared/types.ts`, `App.tsx`,
  `components/ResourceGroupsView.tsx`, `components/shared/LimitsEditor.tsx`,
  `api.ts` + `api/*`, `i18n.ts`.

<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 OnPrem AI Gateway contributors -->

# External Vendor Accounts ("Anbieter")

A portal user can connect an **external AI vendor account** — a plain platform
API key, or a consumer **subscription** reached through the vendor's own OAuth
login — and route their own requests through it, alongside the self-hosted AI
servers. The portal menu label is **"Anbieter"** (de) / **"Providers"** (en); in
code, the wire and the schema the entity is a `vendor_account`, deliberately
named apart from the heavily overloaded **"provider"** adapter term
([ADR-049](../09-architecture-decisions.md#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted),
[Glossary](../12-glossary.md)).

Two vendors ship: **OpenAI** and **Anthropic**. Each is reachable through either
of two auth types — a documented, metered **API key**, or an **experimental,
ToS-restricted subscription** path. The subscription path reuses consumer
OAuth tokens through a third-party gateway, which is against both vendors' consumer
Terms and is reverse-engineered throughout; it is **off by default**, behind a
master feature flag, and every reverse-engineered constant is marked **VERIFY-LIVE**
(§9).

The feature reuses the gateway's existing credential sealing, routing `Target`,
dispatch and usage machinery wherever possible. The genuinely new parts are a
first-class account entity (§1), an OAuth subsystem — connect + token refresh —
(`internal/vendorauth`, §3), a small native Anthropic Messages client and an
OpenAI Responses translate client (`internal/provider`, §4), two static dispatch
extensions (extra headers + a system-prompt masquerade, §4), and a usage/limits
snapshot scraped from vendor rate-limit response headers (§5).

## 1. The entity and its ownership

A `vendor_account` is **personal**: owned by the connecting user and usable only
for that user's own requests (portal chat and the user's own API tokens). Sharing
an account across users via resource groups is deliberately deferred to a later
phase; the entity and ownership model are shaped so that becomes additive (a
sharing link table plus a resolver filter extension), not a rewrite.

| Field | Meaning |
|---|---|
| `id` | `va_`-prefixed, random hex. |
| `owner_user_id` | FK → `users(id)` `on delete cascade`. The one principal the account serves. |
| `vendor` | `openai` \| `anthropic` (`routing.VendorOpenAI` / `VendorAnthropic`). |
| `auth_type` | `api_key` \| `subscription` (`routing.VendorAuthAPIKey` / `VendorAuthSubscription`). Immutable after creation. |
| `name` | User-facing label. |
| `status` | `active` \| `disabled` \| `needs_reconnect`. The last is system-managed (§3.4): a refresh rejection flips an account to it; the operator cannot set it directly. |
| `api_key` | **Sealed** (`enc:`/`plain:`), populated only when `auth_type = api_key`. |
| `oauth_tokens` | **Sealed** JSON token set, populated only when `auth_type = subscription`. |

Exactly one of `api_key` / `oauth_tokens` is populated per row (enforced in the
service, not the schema). A per-account curated model catalog
(`vendor_account_models`) and a rate-limit usage snapshot
(`vendor_account_usage`) hang off the account, both `on delete cascade`. The three
tables and the `usage_events.account_id` attribution column are migration 82; see
[Data Model](../reference/data-model.md#external-vendor-accounts-anbieter).

The domain type is `routing.VendorAccount`, stored across all three drivers
(memory / SQLite / PostgreSQL) through the usual `routing.Store` composition; the
portal view is `portal.VendorAccountDTO`, which carries **no** credential material
— the sealed key and token set are reduced to the `api_key_set` /
`subscription_connected` booleans ([Secrets at rest](#8-secrets-at-rest)).

## 2. The two auth types

**`api_key`.** The user pastes a platform API key (`sk-…` / `sk-ant-…`). Cheap,
documented, stable, and legal — it is the user's own metered key. The gateway
seals it at rest and sends it as the vendor's own API-key header at dispatch
(§4). This is the recommended path.

**`subscription`.** The user connects a consumer subscription (Claude Pro/Max,
ChatGPT/Codex Plus/Pro/Team) through the vendor's OAuth login, and inference is
served against the vendor's **subscription backend** rather than its metered API.
Reverse-engineered, undocumented, and restricted by both vendors' consumer Terms
(§9). A subscription account is created **unconnected**; a connect flow (§3) fills
its sealed OAuth token set afterwards.

## 3. Connect flows (`internal/vendorauth`)

All vendor OAuth constants — client ids, authorize/token URLs, redirect URIs,
scopes, beta headers, device-code endpoints, token-claim names — live in **one
file**, `internal/vendorauth/constants.go`, every value marked
reverse-engineered and VERIFY-LIVE. The flow functions read them through an
overridable `Endpoints` struct, so a live operator (or a unit test against an
`httptest` server) can point them at a corrected endpoint without touching the
flow code. `vendorauth` may import `capture` (for sealing) and nothing from
`provider`, `portal` or `routing`.

Three connect methods exist. All are owner-only and gated by the
`vendor_accounts_enabled` master flag in `portal.Service`; the HTTP endpoints are
under `POST /api/portal/vendor-accounts/{id}/connect/*`
([API Surface](../reference/api-surface.md#vendor-accounts-anbieter)).

### 3.1 Token import (both vendors) — the quick start

The user pastes OAuth tokens they already hold (`POST .../connect/import` with
`{access_token, refresh_token?, expires_at?}`). The backend seals them into
`oauth_tokens` and marks the account connected; there is deliberately **no live
probe** — the first real request validates the tokens. For an OpenAI account the
ChatGPT account id and plan type are read, best-effort, from the access token's
JWT claims (the `https://api.openai.com/auth` namespace), so an import yields the
same token set the code-paste flow does. The portal ships a guide for finding the
tokens: Claude Code's `~/.claude/.credentials.json` (or the macOS Keychain entry,
or `claude setup-token`) and Codex's `~/.codex/auth.json`.

### 3.2 Authorization-code paste (both vendors)

Suited to a remote gateway, since no OAuth callback reaches the server.
`POST .../connect/begin` generates a PKCE verifier + `state`, stores them in an
in-memory pending map keyed to the account, and returns the vendor authorize URL
(`{authorize_url}`). The portal opens it; the user signs in and approves, the
vendor shows a `code#state` string (Anthropic) or lands the browser on a dead
loopback URL carrying the code (OpenAI), and the user pastes it back to
`POST .../connect/complete` with `{code}` (a bare code, `code#state`, or a whole
callback URL is accepted). The backend verifies the state, exchanges the code
with the PKCE verifier at the vendor token endpoint, and seals the resulting
token set.

Anthropic exchanges at its console token URL with a JSON body;
OpenAI uses form-encoded bodies at its OAuth token URL. The `state` is verified
when the pasted value carries it; PKCE binds the exchange either way.

### 3.3 Device-code (OpenAI only) — optional

OpenAI additionally offers the Codex CLI's **bespoke `deviceauth` protocol**
(not RFC 8628), which works for both local and remote gateways because it is
poll-based and needs no callback. `POST .../connect/device/begin` requests a user
code and returns `{user_code, verification_url}` for the UI to display;
`POST .../connect/device/poll` is called on an interval and answers
`{connected: bool}` — `false` while the user has not approved yet, `true` once the
account is connected (no token is ever returned). A poll that meets a transient
upstream error (5xx/429/network) keeps the pending entry alive and keeps polling;
only a 4xx rejection or the pending-state expiry ends it. Anthropic has no device
flow. Device-code is the primary OpenAI connect method, with code-paste and token
import as the alternatives.

### 3.4 Token refresh

The subscription bearer is resolved and, when stale, refreshed **lazily at
dispatch** (`subscriptionAuthCtx` / `resolveSubscriptionBearer`,
`internal/gateway/server.go`). If the sealed access token is within a small buffer
(`vendorTokenRefreshBuffer`, 2 minutes) of its expiry, the gateway refreshes with
`grant_type=refresh_token`, re-seals the token set in place through a **narrow
writer** (`SetVendorAccountOAuthTokens`, so a full-row rewrite cannot clobber a
concurrently rotated token), and persists it. The whole sequence runs under a
**per-account lock** (`lockVendorAccount`) so concurrent dispatches for one
account single-flight the refresh instead of each burning the refresh token. The
refresh is **vendor-aware**: an Anthropic token goes only to the Anthropic token
endpoint and an OpenAI token only to the OpenAI one; the `resolveSubscriptionBearer`
switch is fail-closed, serving no bearer for an unrecognized vendor.

The whole path is **fail-open**: on any failure the request proceeds **without**
a bearer (and the upstream answers 401/403) rather than faulting. A refresh
**rejection** (a revoked/expired refresh token) additionally flips the account to
`needs_reconnect`, which the portal surfaces. A 401 on an unexpired-but-revoked
access token does **not** flip the status (it is bounded by the token TTL) — a
known limitation recorded in §9.

## 4. Serving

A connected account becomes a routable candidate **without touching the hot
scoring path, the multiplexer, or the `Target` core** beyond a small, additive
dispatch extension. The resolver (`internal/routing/resolver.go`) gains a
vendor-account candidate source consulted for a resolve carrying a known
principal: it enumerates the principal's **own** active accounts, matches the
requested gateway model + API flavor against the account's `vendor_account_models`
rows, and builds a `Target` directly. Owner-scope is intrinsic — only the
principal's accounts are enumerated — so one user's account can never serve
another user's request. Precedence against self-hosted/shared routes is
configurable (§6).

`Target` carries four vendor fields, all empty/false for an ordinary AI-server
target:

| Field | Meaning |
|---|---|
| `VendorAccountID` | Names the serving account, for usage attribution (§5) and for the dispatch layer to resolve the subscription bearer. Set on **every** vendor target, api-key included. |
| `Subscription` | `true` only on a subscription (OAuth) target. **This**, not `VendorAccountID`, is the subscription-bearer trigger, so an api-key target that also carries a `VendorAccountID` keeps using its sealed `APIToken`. |
| `ExtraHeaders` | A small static header set attached to the upstream request (§4.1/§4.2). |
| `Masquerade` | `""` (none) or `claude_code` (`routing.MasqueradeClaudeCode`), which makes the Anthropic client prepend the required Claude-Code system block (§4.1). |

Four provider kinds select the client at dispatch: `vendor_openai`
(`ProviderVendorOpenAI`, the api-key OpenAI path via the existing
OpenAI-compatible client), `vendor_anthropic` (`ProviderVendorAnthropic`, the
native Anthropic Messages client), and `vendor_openai_subscription`
(`ProviderVendorOpenAISubscription`, the ChatGPT backend).

### 4.1 Anthropic

- **`anthropic-version: 2023-06-01`** on every call (set by the client).
- **api_key**: the credential rides the `x-api-key` header.
- **subscription**: `Authorization: Bearer <oauth-access-token>` plus
  `anthropic-beta: oauth-2025-04-20`, plus the **Claude-Code masquerade** — the
  request's system content is prefixed with the exact block
  `You are Claude Code, Anthropic's official CLI for Claude.`, which the OAuth
  Messages path requires. Endpoint `https://api.anthropic.com`.

Portal chat builds requests itself and Anthropic serves only `/v1/messages`
(not `/v1/chat/completions`), so a **native Anthropic Messages client**
(`internal/provider/anthropic_messages.go`) renders the neutral `inference`
request to a `/v1/messages` body and parses the response/SSE back. It renders and
parses itself rather than importing `internal/compat` (an architecture-test
boundary). Claude-Code-flavored inbound traffic continues to use native
passthrough.

### 4.2 OpenAI

- **api_key**: `https://api.openai.com` via the existing OpenAI-compatible client
  (`/v1/responses` or `/v1/chat/completions`), `Authorization: Bearer`.
- **subscription**: the ChatGPT backend,
  `https://chatgpt.com/backend-api/codex`, which speaks the **Responses protocol
  only**. Static headers `OpenAI-Beta: responses=experimental` and
  `originator: codex_cli_rs`; the per-account `chatgpt-account-id` header is added
  at dispatch from the sealed token set (not baked into the shared `Target`, which
  is copied before the per-request header is added). No masquerade.

The subscription path splits by the inbound request shape:

- An inbound **Codex `/v1/responses`** request is relayed **verbatim** to the
  ChatGPT backend's `/responses` (native passthrough — lossless). The endpoint
  mode keys on `Target.Subscription`, so an api-key OpenAI `/responses` target is
  unaffected.
- Any other OpenAI flavor (chat completions, **portal chat**) is **translated** by
  a new outbound **OpenAI Responses translate client**
  (`internal/provider/openai_responses.go`): it renders the neutral request to a
  Responses body, POSTs the same bare `/responses` path, and parses the Responses
  SSE back to the neutral model.

### 4.3 Credential resolution at the edge

For a subscription target, `subscriptionAuthCtx` resolves (and refreshes, §3.4)
the OAuth bearer, attaches it plus the target's static `ExtraHeaders`, and — for
OpenAI — the per-account `chatgpt-account-id`. The static headers are attached
even when no bearer is available, so an auth failure reads as an auth error
upstream rather than a missing API version. The api-key path is unchanged: its
sealed `APIToken` rides the vendor's own API-key header. The custom `x-api-key`
header is redacted in payload capture (a latent leak the feature closed).

## 5. Usage & limits (header scraping)

Neither vendor exposes an absolute subscription cap, so the panel shows
**percentages and reset times only**. At the single `recordUsage` choke point
(`internal/gateway/inference_complete.go`), when the served target is a vendor
account, the gateway scrapes the vendor's **rate-limit response headers** — which
it already has in hand, so no extra request is made — and upserts a per-account
snapshot (`vendor_account_usage`). The scrape is **entirely best-effort**: it
never faults the inference request, logs only the account id (never a header value
or a token), and never overwrites a good snapshot with an all-unknown one.

| Vendor | Headers (lowercased; VERIFY-LIVE) | Normalization |
|---|---|---|
| Anthropic subscription | `anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization`, `-reset` | A 0..1 utilization **fraction** → ×100 percent; the single unified reset is attributed to the five-hour window. |
| OpenAI subscription | `x-codex-primary-used-percent` + `x-codex-primary-reset-at` (five-hour), `x-codex-secondary-used-percent` + `x-codex-secondary-reset-at` (weekly), `x-codex-credits-balance` | A 0..100 **percent** used as-is; the credit balance is an opaque string. |

Parsing is **tolerant**: a missing/non-numeric value leaves the window at its
unknown sentinel (`-1` percent, nil reset), never a fabricated `0`; a scaled
percent is clamped to `≤ 100`. The snapshot is exposed only on the **detail**
read (`GET /api/portal/vendor-accounts/{id}` → `VendorAccountDTO.Usage`,
`omitempty`), never on the list (one snapshot read per row would be an N+1), and a
`-1` window is hidden in the UI rather than shown as a real 0 %. API-key accounts
carry no subscription window; absolute €/$ spend accounting is deferred.

## 6. Feature flag and routing mode

Two system settings govern the feature (both read from the `system_settings`
store through a cached accessor invalidated on a settings write):

| Setting | Values | Default | Effect |
|---|---|---|---|
| `vendor_accounts_enabled` | bool | **off** | The **master** flag. When off: the "Anbieter" nav item is hidden, the CRUD/connect endpoints answer `409 vendor_accounts.module_disabled`, and the resolver's vendor branch and the model-listing overlay are no-ops. |
| `vendor_account_routing_mode` | `vendor_first` \| `fallback_only` | `vendor_first` | Precedence between a caller's own vendor accounts and the self-hosted/shared routes. `vendor_first`: an owned account wins when it serves the requested model. `fallback_only`: an owned account is used only when no self-hosted/shared route exists. |

The frontend reads the master flag through a portal-scoped
`GET /api/portal/vendor-accounts/enabled` (`{module_enabled}`, readable by any
user even while the module is off — it exists to let the shell show or hide the
nav item without granting the system-scoped settings read), mirroring the
NetBird/certificates `/enabled` endpoints. The exact-path route wins over the
`{id}` subtree because account ids are always `va_`-prefixed.

A connected account also carries a **model-listing owner overlay**: the owner's
vendor-account catalog models appear in their own `/v1/models` listing and the
chat picker (served-flavors parity with dispatch holds). The overlay is gated on
the same master flag.

## 7. Owner-scope RBAC

The HTTP handlers gate on the standard web scope (`gateway:use`), then per-object
authorization happens **in `portal.Service`**: a user may read/manage only
accounts where `owner_user_id` equals the principal, mirroring `authorizeServer`.
A non-owner gets the **same `404 vendor_account.not_found`** as for a
non-existent account (the no-existence-leak rule). The `system` scope may **read**
any account (`GetVendorAccount`), but `ListVendorAccounts` and every **write**
(create/update/delete/connect) are owner-only — a deliberate read/write
asymmetry. Routing enforces the same owner scope by enumerating only the
principal's own accounts, so one user's account can never serve another's request.

## 8. Secrets at rest

Credentials reuse the repo-wide secret envelope verbatim — no new key, no new
mechanism ([ADR-007](../09-architecture-decisions.md#adr-007--secrets-at-rest-the-encplain-scheme)):

- The `api_key` and `oauth_tokens` columns are **sealed** with
  `capture.SealSecret` **before** the store write, and opened with
  `capture.OpenSecret` only at the cipher-holding dispatch edge. A refresh
  re-seals in place.
- The read-back DTO exposes **presence only** (`api_key_set`,
  `subscription_connected`), never the value. PATCH uses the keep/clear/replace
  `*string` sentinel (nil = keep, `""` = clear, value = replace + reseal), exactly
  as `applications.api_token` does.
- The **subscription path requires `OP_AI_GATEWAY_CAPTURE_ENCRYPTION_KEY`**: the
  OAuth token set is a decryptable secret at rest, so a keyless disk-backed store
  rejects the write (`capture.ErrKeyRequired`, surfaced as
  `vendor_account.connect_key_required` on connect and
  `vendor_account.api_key_key_required` on an API key), consistent with the
  secret-at-rest rule. A memory/volatile store needs no key.

## 9. ToS, experimental status, and VERIFY-LIVE

The subscription path is built **for internal testing with the operator's own
account**, as an explicitly experimental, flag-gated, disable-able capability. It
must never be presented as a supported, multi-tenant production capability. The
reasons are recorded deliberately, not in denial of them
([ADR-049](../09-architecture-decisions.md#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted),
[Risks §11.4](../11-risks-and-technical-debt.md#114-deliberate-design-acceptances)):

- **It is against both vendors' consumer Terms.** Reusing consumer-subscription
  OAuth tokens for inference through a third-party gateway violates the consumer
  Terms, and **Anthropic enforces it server-side** (the credential is authorized
  only for use with Claude Code; the forced `You are Claude Code` system block).
  The feature must degrade gracefully when a vendor blocks or changes behavior.
- **Every subscription constant is reverse-engineered and VERIFY-LIVE.** Each
  endpoint, client id, redirect URI, scope, beta header, device-code path,
  token-claim name, masquerade requirement, serving host, request/refresh body
  encoding, and the rate-limit response-header names can change without notice.
  They are confined to `internal/vendorauth/constants.go` and the resolver's
  dispatch literals (routing cannot import `vendorauth`), all marked VERIFY-LIVE,
  and all parsing is tolerant (prefix-matched, fail-open).
- **The seeded OpenAI subscription model ids are a best guess.** The curated
  catalog seeds vendor-native ids (e.g. `gpt-5`, `claude-sonnet-5-5`); the ids a
  given ChatGPT plan actually serves (e.g. `gpt-5-codex`) must be verified live,
  and a subscription account will not serve until its model rows match what the
  plan offers.
- **Known behavioral gaps.** A 401 on an unexpired-but-revoked access token does
  not flip the account to `needs_reconnect` (only a refresh rejection does); a
  persist-failure after a refresh may keep a single-use refresh token, forcing a
  reconnect on the next request. Both are bounded by the token TTL and accepted.

Keep the operator's real gateway hostname out of all repo text; use
`<gateway-host>` where a host is needed.

## Related chapters

- [Routing & Model Selection](routing-and-model-selection.md) — the resolver and
  candidate scoring the vendor-account source plugs into.
- [API Compatibility & Inference](compatibility-and-inference.md) — endpoint
  modes, native passthrough vs. translate, the neutral inference model.
- [Security, Authentication & Authorization](security-auth-rbac.md) — scopes and
  object-level authorization.
- [Data Model](../reference/data-model.md#external-vendor-accounts-anbieter) — the
  three tables and the `usage_events.account_id` column (migration 82).
- [HTTP API Surface](../reference/api-surface.md#vendor-accounts-anbieter) — the
  `/api/portal/vendor-accounts*` routes and their error codes.
- [Configuration & Environment Variables](../reference/config-env.md) — the cipher
  key and the two system settings.
- [ADR-049](../09-architecture-decisions.md#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted)
  — the first-class-entity and experimental-subscription decisions.

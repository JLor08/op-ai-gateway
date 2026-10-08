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
(`internal/vendorauth`, §3), model-independent credential validation that
tells a rejected login from a wrong model (§3.5), a small native Anthropic
Messages client and an OpenAI Responses translate client (`internal/provider`,
§4), two static dispatch extensions (extra headers + a system-prompt masquerade,
§4), and a usage/limits snapshot scraped from vendor rate-limit response headers
(§5).

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
| `model_prefix` | Optional per-account namespace for the account's gateway model ids (migration 83; `''` = none). Validated by the service — trimmed, printable ASCII without spaces, at most 64 bytes — and stored verbatim; the DTO reports it (`model_prefix`) and the create/update requests accept it. |

At most one of `api_key` / `oauth_tokens` is populated per row — a subscription
account created but not yet connected, and an api-key account with no key set, have
neither (the exclusivity is enforced in the
service, not the schema). A per-account curated model catalog
(`vendor_account_models`, seeded at creation from a static set keyed by vendor
**and** auth type, §9; each row also carries the vendor's human-readable
`display_name`, `''` when none, since migration 83) and a rate-limit usage snapshot
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
`vendor_accounts_enabled` master flag in `portal.Service`, and a successful
connect (by any method) sets the account's status to `active`, clearing a prior
`needs_reconnect`. The HTTP endpoints are
under `POST /api/portal/vendor-accounts/{id}/connect/*`
([API Surface](../reference/api-surface.md#vendor-accounts-anbieter)).

### 3.1 Token import (both vendors) — the quick start

The user supplies OAuth tokens they already hold (`POST .../connect/import` with
`{access_token, refresh_token?, expires_at?}`). Before anything is stored, the
access token is checked once against the vendor (§3.5): a token the vendor
definitively rejects is refused with `400 vendor_account.connect_invalid_credentials`
and nothing is persisted, while an unreachable vendor or an inconclusive answer
still lets the import through. The backend then seals the tokens into
`oauth_tokens` and marks the account connected.

Two details are filled in so that an import yields the same token set the
code-paste flow does:

- **Identity (OpenAI).** The ChatGPT account id and plan type are read,
  best-effort, from the access token's JWT claims (the `https://api.openai.com/auth`
  namespace); when the token carries no account id, it is backfilled from the
  validation answer (§3.5).
- **Expiry.** A pasted token has no `expires_in`. When the request carries no
  `expires_at`, the expiry is read from the access token's JWT `exp` claim (an
  OpenAI token is a JWT; an Anthropic one is opaque and yields none, which is why
  the file-assisted import below sends the explicit `expiresAt` that a Claude Code
  credential file carries). Without a known expiry the lazy refresh (§3.4) never
  fires for the token, so an import with no known expiry would keep presenting a
  lapsed access token for good, even when a refresh token was supplied that could
  have renewed it.

The portal ships a guide for finding the tokens: Claude Code's
`~/.claude/.credentials.json` (or the macOS Keychain entry, or `claude setup-token`)
and Codex's `~/.codex/auth.json`.

**File-assisted import.** Picking the right field out of those files by hand is
error-prone; the usual slip is pasting Codex's `id_token`, which is an identity
token the gateway cannot use, where the `access_token` belongs. The import panel
therefore also accepts the file itself. It is read and parsed **in the browser**
(`parseCredentialFile`, a pure module with no network access): the size is capped
at 1 MiB before the file is read, the vendor is recognized from the file's content
(the file name only breaks a tie), and a file for the other vendor than the
account is refused locally. Only `{access_token, refresh_token, expires_at}` are
then submitted through the *same* import endpoint as a manual paste, so both
ways get the same validation and no server code path accepts a credential file.
This is data minimization: the raw file, its `id_token`, a sibling
`OPENAI_API_KEY` and every other field **never leave the browser**. The parser's
failures are fixed strings keyed by a code that the UI localizes; none quotes the
file, because a JSON syntax-error message would echo a fragment of a credential.
An `id_token`-only file is rejected with an explanation instead of being sent and
left to surface later as a 401.

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

### 3.3 Device-code (OpenAI only)

OpenAI additionally offers the Codex CLI's **bespoke `deviceauth` protocol**
(not RFC 8628), which works for both local and remote gateways because it is
poll-based and needs no callback. `POST .../connect/device/begin` requests a user
code and returns `{user_code, verification_url}` for the UI to display;
`POST .../connect/device/poll` is called on an interval and answers
`{connected: bool}` — `false` while the user has not approved yet, `true` once the
account is connected (no token is ever returned). A poll that meets a transient
upstream error (5xx/429/network) keeps the pending entry alive and keeps polling;
only a 4xx rejection or the pending-state expiry ends it. Anthropic has no device
flow. Device-code is one of OpenAI's three connect methods — the portal offers
code-paste, device-code and token import — not a required or primary one.

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

### 3.5 Credential validation

A failed chat conflates two unrelated problems — a login the vendor rejects and a
model the backend does not serve — and the seeded model ids are a best guess
(§9). Validation separates them. Four probes in `internal/vendorauth/validate.go`
each make **one cheap GET that names no model**, so an authentication verdict can
never be mistaken for a wrong-model error. The URLs live in `constants.go` beside
the OAuth constants.

| Credential | Probe | Auth headers | Provenance |
|---|---|---|---|
| OpenAI **subscription** access token | `GET https://chatgpt.com/backend-api/wham/accounts/check` | `Authorization: Bearer` | reverse-engineered, **VERIFY-LIVE** |
| Anthropic **subscription** access token | `GET https://api.anthropic.com/api/oauth/profile` | `Authorization: Bearer`, `anthropic-beta: oauth-2025-04-20`, `anthropic-version` | reverse-engineered, **VERIFY-LIVE** |
| OpenAI **`api_key`** | `GET https://api.openai.com/v1/models` | `Authorization: Bearer` | documented public API |
| Anthropic **`api_key`** | `GET https://api.anthropic.com/v1/models` | `x-api-key`, `anthropic-version` | documented public API |

**Classification** is one rule for all four:

- HTTP **2xx** → `valid`.
- HTTP **401** → `invalid`. This is the only answer that ever counts as a bad
  credential.
- **Everything else** — 403, 404, 429, 5xx, a redirect, a timeout, any transport
  failure → `unverifiable`: the vendor gave no clean answer, which says nothing
  about the credential. Redirects are not followed, so a credential header can
  never be carried to another host.
- **One exception:** for the Anthropic *subscription* probe, **403 is `valid`**. A
  token minted by `claude setup-token` has the inference scope but not
  `user:profile`, so the profile endpoint legitimately refuses it with a 403
  although the token serves inference.

**Fail-soft.** Validation never reduces availability: `unverifiable` neither
blocks an import nor is ever reported as an invalid credential. A probe runs only
at import and on the explicit test action — never on a request path — and is
bounded to 10 seconds. The probes never log or return the credential; the
`detail` is a fixed phrase plus the HTTP status and, only when the response body
carries a short identifier-like error code, that code. The vendor's free-text
message (which may quote the key) and transport error text are never echoed, and
the service scrubs the credential from any detail once more before it leaves.

**At import.** `ConnectVendorAccountImport` runs the matching *subscription* probe
once before it persists (§3.1). The code-paste and device-code connects are not
probed, since the vendor itself has just issued those tokens, and an API key is
probed only by the explicit test, not when it is saved. At import:

- a definitive `invalid` rejects the import with `400
  vendor_account.connect_invalid_credentials` (a 400, never a 401: the portal
  treats a 401 from this API as an expired session). `valid` and `unverifiable`
  persist;
- an access token that is **already expired while a refresh token came with it**
  is not probed at all and reads `unverifiable`: the vendor would answer 401 for
  an account that heals on its first request through the lazy refresh (§3.4), so an
  expired-but-refreshable token is never blocked (a token that has not expired
  and that the vendor answers with 401 is still refused). Without a refresh
  token, an expired token can never heal and is probed like any other;
- **account-id backfill (OpenAI).** If the token's claims carried no
  `chatgpt_account_id`, a `valid` answer from `accounts/check` supplies it
  (`default_account_id`, falling back to the first listed account) together with
  the matching plan type. The probe body is untrusted vendor input and the id is
  later sent back as the `chatgpt-account-id` request header, so it is stored only
  if it is non-empty printable ASCII of at most 128 bytes; an id derived from the
  token wins, and the plan is taken only with the id it belongs to;
- **never-refresh fix.** The expiry derived from the JWT `exp` claim (§3.1) is
  what lets a pasted access token be refreshed at all.

**Test connection.** `POST /api/portal/vendor-accounts/{id}/check` opens the
account's stored credential (an API key, or the access token of the sealed token
set), runs the same probe and returns `VendorConnectionCheck`:
`{status: "valid" | "invalid" | "unverifiable", detail, checked_at}`
([API Surface](../reference/api-surface.md#vendor-accounts-anbieter)). It has no
request body and changes nothing — it does not even refresh a token. It is
**strictly owner-only, system scope included**, because it sends the owner's
sealed credential to the vendor from the gateway: letting anyone else trigger it
would turn that credential into a live-or-dead oracle. A non-owner gets the same
`404 vendor_account.not_found` as for an unknown id; a disabled module answers
`409`. A missing credential (no API key set, subscription not connected) and an
expired-but-refreshable token report `unverifiable` with an explanatory `detail`;
a stored credential that cannot be opened (a lost or never-configured cipher key,
a corrupt blob, a blob sealed under another key) is an error,
`409 vendor_account.credential_unreadable`, not a verdict — a state of the
account that reconnecting fixes, worded about *reading* the credential rather
than the write path's "an encryption key is required to store ...", which would
be wrong for a read-only check and nonsensical on a subscription. The portal's
detail view has a "Test connection" button that renders the verdict inline — success, error, or a
neutral notice for `unverifiable` — and says in so many words that it checks the
credential only, never a particular model, so a chat that fails while the test
reads `valid` points at the model rather than the login. A thrown error (the
check could not run) surfaces as a toast, never as a verdict.

The two subscription endpoints are reverse-engineered and join the other
VERIFY-LIVE constants (§9). A moved or removed endpoint typically answers 404, a
redirect or a 5xx and so degrades to `unverifiable`. Two residual exposures
remain, in opposite directions. An endpoint that starts answering 401 for a good
token would read as a false `invalid` and refuse an import. Conversely, the
Anthropic subscription probe counts **any** 403 as `valid`, so a profile endpoint
that is moved or blocked behind a 403 would pass a bad token; and the reason given
for that exception (a `setup-token` token lacks `user:profile`) is itself inferred
from the scope names, not confirmed against a live token. The 403 rule is
therefore part of the VERIFY-LIVE assumption, like the URLs. The probe URLs are
plain constants, not part of the overridable OAuth `Endpoints`, so correcting
either is a one-line change in `constants.go`.

## 4. Serving

A connected account becomes a routable candidate **without touching the hot
scoring path, the multiplexer, or the `Target` core** beyond a small, additive
dispatch extension. The resolver (`internal/routing/resolver.go`) gains a
vendor-account candidate source consulted for a resolve carrying a known **user**
principal: it enumerates the principal's **own** active accounts and builds a
`Target` directly for the **first** active account that has a
`vendor_account_models` row whose `gateway_model` equals the requested model.
Owner-scope is intrinsic — only the principal's accounts are enumerated — so one
user's account can never serve another user's request. Precedence against
self-hosted/shared routes is configurable (§6).

The match is on the **model name only** (`gateway_model == req.Model`); the
catalog row's `api_flavor` is stored metadata and is **not** read by the resolver
or the listing overlay. What flavor each account serves is decided instead by the
target the resolver builds, giving this served-flavor matrix:

| Account | Serves | How |
|---|---|---|
| **api-key** (OpenAI or Anthropic) | the `openai` **and** `anthropic` dialects | `Target.APIFlavors = [openai, anthropic]`, endpoint modes left zero (**translate**): whichever dialect the caller used is translated through the neutral model to the vendor's native wire (OpenAI → `/v1/chat/completions`; Anthropic → `/v1/messages`). |
| **Anthropic subscription** | the `openai` **and** `anthropic` dialects | same `[openai, anthropic]` translate, into `/v1/messages` with the masquerade + beta headers. |
| **OpenAI subscription** | the `openai` dialect **only** | `Target.APIFlavors = [openai]`. The resolver's flavor guard **skips** an `anthropic`-dialect request to such an account, which then falls through to the standard path and ends `routing.no_model_route`. |

The vendor branch is **skipped entirely** — the request falls through to the
self-hosted/shared path — when the module flag is off, the principal has no user
id (a **service token**), a **server-override** is set, the request is
**capability-gated** (`RequiredCapabilities` non-empty, e.g. vision/image), or the
flavor is **images** (`openai_images`). That the resolver and the listing overlay
agree on what each account serves is what makes §6's "served-flavors parity"
between dispatch and the model listing meaningful.

`Target` carries four vendor fields, all empty/false for an ordinary AI-server
target:

| Field | Meaning |
|---|---|
| `VendorAccountID` | Names the serving account, for usage attribution (§5) and for the dispatch layer to resolve the subscription bearer. Set on **every** vendor target, api-key included. |
| `Subscription` | `true` only on a subscription (OAuth) target. **This**, not `VendorAccountID`, is the subscription-bearer trigger, so an api-key target that also carries a `VendorAccountID` keeps using its sealed `APIToken`. |
| `ExtraHeaders` | A small static header set attached to the upstream request (§4.1/§4.2). |
| `Masquerade` | `""` (none) or `claude_code` (`routing.MasqueradeClaudeCode`), which makes the Anthropic client prepend the required Claude-Code system block (§4.1). |

Three provider kinds select the client at dispatch: `vendor_openai`
(`ProviderVendorOpenAI`, the api-key OpenAI path via the existing
OpenAI-compatible client), `vendor_anthropic` (`ProviderVendorAnthropic`, the
native Anthropic Messages client, used for an Anthropic account whether api-key or
subscription), and `vendor_openai_subscription`
(`ProviderVendorOpenAISubscription`, the ChatGPT backend).

### 4.1 Anthropic

- **`anthropic-version: 2023-06-01`** on every call (set by the client).
- **api_key**: the credential rides the `x-api-key` header.
- **subscription**: `Authorization: Bearer <oauth-access-token>` plus
  `anthropic-beta: oauth-2025-04-20`, plus the **Claude-Code masquerade** — the
  request's system content is prefixed with the exact block
  `You are Claude Code, Anthropic's official CLI for Claude.`, which the OAuth
  Messages path requires. Endpoint `https://api.anthropic.com`.

Anthropic serves only `/v1/messages` (not `/v1/chat/completions`), and the vendor
targets leave `MessagesMode` zero (**translate**), so **both** inbound dialects —
OpenAI and Anthropic — are translated through the neutral model into `/v1/messages`
by a **native Anthropic Messages client** (`internal/provider/anthropic_messages.go`),
which renders the neutral `inference` request to a `/v1/messages` body and parses
the response/SSE back. It renders and parses itself rather than importing
`internal/compat` (an architecture-test boundary), and it has **no** native
passthrough path — the only passthrough on any vendor path is the OpenAI-subscription
Responses path (§4.2).

### 4.2 OpenAI

- **api_key**: `https://api.openai.com` via the existing OpenAI-compatible client.
  The target's endpoint modes are zero, so every inbound dialect is **translated to
  `/v1/chat/completions`** (there is no api-key `/responses` passthrough);
  `Authorization: Bearer`.
- **subscription**: the ChatGPT backend,
  `https://chatgpt.com/backend-api/codex`, which speaks the **Responses protocol
  only**. Static headers `OpenAI-Beta: responses=experimental` and
  `originator: codex_cli_rs`; the per-account `chatgpt-account-id` header is added
  at dispatch from the sealed token set (not baked into the shared `Target`, which
  is copied before the per-request header is added). No masquerade.

The subscription path splits by the inbound request shape:

- An inbound **Codex `/v1/responses`** request has `ResponsesMode = passthrough`
  set by the resolver (from the **fine** `openai_responses` flavor), so it is
  relayed **verbatim** to the ChatGPT backend's `/responses` (lossless). It is
  `Target.Subscription` that makes `endpointModeFor` select the **bare** `/responses`
  path (not `/v1/responses`); an api-key OpenAI target, whose `Subscription` is
  false, is unaffected.
- Any other OpenAI flavor (chat completions, **portal chat**) is **translated** by
  a new outbound **OpenAI Responses translate client**
  (`internal/provider/openai_responses.go`): it renders the neutral request to a
  Responses body, POSTs the same bare `/responses` path, and parses the Responses
  SSE back to the neutral model.

The ChatGPT backend is far stricter than the public Responses API, so the
translate client always sends what the Codex CLI always sends, whatever the
inbound request carried (REVERSE-ENGINEERED / VERIFY-LIVE): `store: false` (a
body without `store` defaults to `true`, which the subscription backend rejects
with a 400), `include: ["reasoning.encrypted_content"]` (so reasoning round-trips
while nothing is stored), and a `reasoning` object — the request's effort, else
`medium` (a portal chat carries none, and the subscription catalog is the
reasoning-only gpt-5 family). `instructions` and `max_output_tokens` stay omitted
when empty. A non-2xx answer keeps the usual status → sentinel mapping
(401/403 → `ErrAuthRejected`, 503 → `ErrUpstreamStarting`, else `ErrUnavailable`)
and now also carries a bounded (4 KiB), single-line snippet of the vendor's error
body in the returned error and in the payload capture, because the backend states
why it refused a request only there.

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

Three system settings govern the feature (all read from the `system_settings`
store). The **resolver** reads the first two through a cached accessor that is
**invalidated on the settings PUT** whenever it carries either key
(`invalidateVendorSettingsCache`), so a portal toggle takes effect on the next
resolve; the accessor's short TTL (~5 s, `vendorSettingsCacheTTL`) only bounds an
**out-of-band** change, such as a direct database edit. `portal.Service` (the CRUD
gate, the model-listing overlay and the discovery client version) reads them
uncached, so a PUT is visible on its very next call:

| Setting | Values | Default | Effect |
|---|---|---|---|
| `vendor_accounts_enabled` | bool | **off** | The **master** flag. When off: the "Anbieter" nav item is hidden, the CRUD/connect/test-connection endpoints answer `409 vendor_accounts.module_disabled`, and the resolver's vendor branch and the model-listing overlay are no-ops. |
| `vendor_account_routing_mode` | `vendor_first` \| `fallback_only` | `vendor_first` | Precedence between a caller's own vendor accounts and the self-hosted/shared routes. `vendor_first`: an owned account wins when it serves the requested model. `fallback_only`: an owned account is used only when no self-hosted/shared route exists. |
| `vendor_openai_codex_client_version` | version string | `26.930.61225` | The Codex `client_version` the OpenAI **subscription** model discovery sends. The ChatGPT backend **hides every model whose `minimal_client_version` exceeds it**, so a stale value silently hides new models: **raise it when OpenAI ships a newer Codex app** (System settings, no redeploy). Blank resets to the built-in default (`vendorauth.CodexModelsClientVersionDefault`); a malformed value is a `400 system.vendor_openai_codex_client_version_invalid`. Read only by `portal.Service` (never the resolver), so it has no cache. VERIFY-LIVE. |

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
asymmetry. The **test-connection** action (§3.5) is not a write but is owner-only
for the `system` scope too, because it uses the stored credential: it is
authorized like a write, not like a metadata read. Routing enforces the same owner scope by enumerating only the
principal's own accounts, so one user's account can never serve another's request.

## 8. Secrets at rest

Credentials reuse the repo-wide secret envelope verbatim — no new key, no new
mechanism ([ADR-007](../09-architecture-decisions.md#adr-007--secrets-at-rest-the-encplain-scheme)):

- The `api_key` and `oauth_tokens` columns are **sealed** with
  `capture.SealSecret` **before** the store write, and opened with
  `capture.OpenSecret` at the cipher-holding dispatch edge **and, for the owner's
  explicit test-connection (§3.5), inside `portal.Service`** (`checkVendorAccount`).
  On that second path the opened value goes only to the vendor's own validation
  URL: it is never returned, logged or echoed in a verdict's `detail`. Likewise a
  token import probes the plaintext access token the user just submitted, before
  it is sealed. A refresh re-seals in place.
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
  endpoint (including the two subscription credential-validation probes, §3.5),
  client id, redirect URI, scope, beta header, device-code path, token-claim name,
  masquerade requirement, serving host, request/refresh body encoding, and the
  rate-limit response-header names can change without notice.
  They are confined to `internal/vendorauth/constants.go` and the resolver's
  dispatch literals (routing cannot import `vendorauth`), all marked VERIFY-LIVE,
  and all parsing is tolerant (prefix-matched, fail-open).
- **The seeded model ids are a best guess, and the OpenAI set depends on the auth
  type.** The curated catalog (`internal/portal/vendor_catalog.go`) seeds
  vendor-native ids when an account is created, keyed by vendor **and** auth type.
  An OpenAI **`api_key`** account talks to `api.openai.com` and gets the full set
  (`gpt-5`, `gpt-5-mini`, `gpt-4.1`, `o3`). An OpenAI **subscription** account is
  served by the Codex ChatGPT backend, which, as far as is known, does not serve
  `gpt-4.1` or `o3`; it is therefore seeded with only `gpt-5` and `gpt-5-mini`, so
  a working credential is never paired with a model its backend is known to
  refuse — the auth-versus-model confusion that §3.5 exists to take apart.
  Anthropic's OAuth Messages path and its API key serve the same ids, so its set
  does not vary. Even the narrowed subscription set is unverified: a plan may
  serve further ids (e.g. `gpt-5-codex`) that are not seeded until confirmed live,
  and a subscription account serves nothing its rows do not name. **There is no
  backfill:** an account keeps the rows it was seeded with, so a subscription
  account created before the split keeps its `gpt-4.1` and `o3` rows (which that
  backend rejects) until the account is recreated.
- **Known behavioral gaps.** A 401 on an unexpired-but-revoked access token does
  not flip the account to `needs_reconnect` (only a refresh rejection does); a
  persist-failure after a refresh may keep a single-use refresh token, forcing a
  reconnect on the next request. Both are bounded by the token TTL and accepted.
- **Single-process assumptions.** The pending-connect state (PKCE verifier/state
  and the device-code pending entry) and the per-account refresh lock are both
  **in-process**: a gateway restart loses any connect in progress (the user starts
  it again), and a multi-replica deployment (the PostgreSQL driver) would break the
  refresh single-flight — two replicas could refresh one account concurrently and
  burn its single-use refresh token. The feature is an operator-only experiment, so
  one gateway process is assumed.

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

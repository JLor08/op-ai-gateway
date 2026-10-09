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
(§10).

The feature reuses the gateway's existing credential sealing, routing `Target`,
dispatch and usage machinery wherever possible. The genuinely new parts are a
first-class account entity (§1), an OAuth subsystem — connect + token refresh —
(`internal/vendorauth`, §3), model-independent credential validation that
tells a rejected login from a wrong model (§3.5), a small native Anthropic
Messages client and an OpenAI Responses translate client (`internal/provider`,
§4), two static dispatch extensions (extra headers + a system-prompt masquerade,
§4), a usage/limits snapshot fed by the vendor's rate-limit response headers and,
for an OpenAI subscription, an active usage pull (§5), and a discovery of each account's real model catalog from the vendor,
served under an optional per-account prefix (§6).

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
| `model_prefix` | Optional per-account namespace for the account's model ids (migration 83; `''` = none). The service trims it and accepts at most 64 bytes from `A-Z a-z 0-9 - _ . ~ : / @ +` (the URL-path-safe characters of a model id) with no `..`, anything else being `400 vendor_account.model_prefix_invalid`. It is **applied**, not merely stored: every model the account serves is advertised and requested as the prefix plus the vendor's own id, while the vendor is still sent the bare id (§6.5). The DTO reports it and the create/update requests accept it. |

At most one of `api_key` / `oauth_tokens` is populated per row — a subscription
account created but not yet connected, and an api-key account with no key set, have
neither (the exclusivity is enforced in the
service, not the schema). A per-account model catalog
(`vendor_account_models`) and a rate-limit usage snapshot (`vendor_account_usage`)
hang off the account, both `on delete cascade`. Each catalog row pairs the id a
client requests (`gateway_model`, the prefix plus the vendor's id) with the id
the vendor is sent (`upstream_model`), the API flavor, and the vendor's
human-readable `display_name` (`''` when it gave none; since migration 83). The
catalog starts as a small **static guess** keyed by vendor **and** auth type
(§10), written when the account is created, and is **replaced by the vendor's
real list** once discovery has run (§6). The three tables and the
`usage_events.account_id` attribution column are migration 82; see
[Data Model](../reference/data-model.md#external-vendor-accounts-anbieter).

The domain type is `routing.VendorAccount`, stored across all three drivers
(memory / SQLite / PostgreSQL) through the usual `routing.Store` composition; the
portal view is `portal.VendorAccountDTO`, which carries **no** credential material
— the sealed key and token set are reduced to the `api_key_set` /
`subscription_connected` booleans ([Secrets at rest](#9-secrets-at-rest)).

## 2. The two auth types

**`api_key`.** The user pastes a platform API key (`sk-…` / `sk-ant-…`). Cheap,
documented, stable, and legal — it is the user's own metered key. The gateway
seals it at rest and sends it as the vendor's own API-key header at dispatch
(§4). This is the recommended path.

**`subscription`.** The user connects a consumer subscription (Claude Pro/Max,
ChatGPT/Codex Plus/Pro/Team) through the vendor's OAuth login, and inference is
served against the vendor's **subscription backend** rather than its metered API.
Reverse-engineered, undocumented, and restricted by both vendors' consumer Terms
(§10). A subscription account is created **unconnected**; a connect flow (§3) fills
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
`needs_reconnect`, and then runs a best-effort **model discovery** (§6.2) so the
account serves the models its vendor really offers rather than the static guess;
that discovery can never fail a connect. The HTTP endpoints are
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
writer** (`SetVendorAccountOAuthTokens`, which writes only `oauth_tokens` and
`updated_at`, so a refresh cannot clobber a concurrent rename or status change),
and persists it. The whole sequence runs under a
**per-account lock** (`lockVendorAccount`) so concurrent dispatches for one
account single-flight the refresh instead of each burning the refresh token. The
explicit model refresh (§6.3) asks for a refresh through this same path and lock
rather than refreshing by itself. The protection runs in one direction only: the
PATCH writer (`UpdateVendorAccount`) still rewrites the whole row, `oauth_tokens`
included, from a copy it loaded earlier and outside both locks, so a PATCH that
races a refresh can write the superseded single-use refresh token back — a known,
tracked limitation (§10). The
refresh is **vendor-aware**: an Anthropic token goes only to the Anthropic token
endpoint and an OpenAI token only to the OpenAI one; the `resolveSubscriptionBearer`
switch is fail-closed, serving no bearer for an unrecognized vendor.

The whole path is **fail-open**: on any failure the request proceeds **without**
a bearer (and the upstream answers 401/403) rather than faulting. A refresh
**rejection** (a revoked/expired refresh token) additionally flips the account to
`needs_reconnect`, which the portal surfaces. A 401 on an unexpired-but-revoked
access token does **not** flip the status (it is bounded by the token TTL) — a
known limitation recorded in §10.

### 3.5 Credential validation

A failed chat conflates two unrelated problems — a login the vendor rejects and a
model the backend does not serve — and the seeded model ids are a best guess
(§10). Validation separates them. Four probes in `internal/vendorauth/validate.go`
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
VERIFY-LIVE constants (§10). A moved or removed endpoint typically answers 404, a
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
self-hosted/shared routes is configurable (§7).

The match is on the **model name only** (`gateway_model == req.Model`). That name
is the account's `model_prefix` plus the vendor's id (§6.5), so a prefixed account
does not answer to the bare vendor id, and two accounts serving the same vendor
model under different prefixes each route to their own account. The resolver
sends the vendor the row's `upstream_model`, the bare id, as the target's
`ProviderModel`. The catalog row's `api_flavor` is stored metadata and is **not**
read by the resolver or the listing overlay. What flavor each account serves is
decided instead by the target the resolver builds, giving this served-flavor
matrix:

| Account | Serves | How |
|---|---|---|
| **api-key** (OpenAI or Anthropic) | the `openai` **and** `anthropic` dialects | `Target.APIFlavors = [openai, anthropic]`, endpoint modes left zero (**translate**): whichever dialect the caller used is translated through the neutral model to the vendor's native wire (OpenAI → `/v1/chat/completions`; Anthropic → `/v1/messages`). **One exception:** an inbound `openai_responses` request (`POST /v1/responses`) to an **OpenAI** api-key account has `ResponsesMode = passthrough` and is relayed verbatim to `https://api.openai.com/v1/responses` instead (§4.2). Chat completions, every other `openai` flavor, the `anthropic` dialect and the **Anthropic** api-key account stay translate. |
| **Anthropic subscription** | the `openai` **and** `anthropic` dialects | same `[openai, anthropic]` translate, into `/v1/messages` with the masquerade + beta headers. |
| **OpenAI subscription** | the `openai` dialect **only** | `Target.APIFlavors = [openai]`. The resolver's flavor guard **skips** an `anthropic`-dialect request to such an account, which then falls through to the standard path and ends `routing.no_model_route`. |

The vendor branch is **skipped entirely** — the request falls through to the
self-hosted/shared path — when the module flag is off, the principal has no user
id (a **service token**), a **server-override** is set, the request is
**capability-gated** (`RequiredCapabilities` non-empty, e.g. vision/image), or the
flavor is **images** (`openai_images`). That the resolver and the listing overlay
agree on what each account serves is what makes the "served-flavors parity"
between dispatch and the model listing (§6.7) meaningful.

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
passthrough path — the only passthroughs on any vendor path are the two OpenAI
Responses ones, the subscription's and the api-key account's (§4.2).

### 4.2 OpenAI

- **api_key**: `https://api.openai.com` via the existing OpenAI-compatible client,
  `Authorization: Bearer`. The target's endpoint modes are zero (translate), so
  chat completions, every other `openai` flavor and the `anthropic` dialect are
  **translated to `/v1/chat/completions`** — with one exception, the lossless
  Responses passthrough described below.
- **subscription**: the ChatGPT backend,
  `https://chatgpt.com/backend-api/codex`, which speaks the **Responses protocol
  only**. Static headers `OpenAI-Beta: responses=experimental` and
  `originator: codex_cli_rs`; the per-account `chatgpt-account-id` header is added
  at dispatch from the sealed token set (not baked into the shared `Target`, which
  is copied before the per-request header is added). No masquerade.

The subscription path splits by the inbound request shape:

- An inbound **Codex `/v1/responses`** request has `ResponsesMode = passthrough`
  set by the resolver (from the **fine** `openai_responses` flavor), so it is
  relayed **verbatim** to the ChatGPT backend's `/responses` (lossless; the one
  edit is the request's `model` field, set to the bare vendor id when the account
  has a prefix, and the response is not rewritten back, §6.5). It is
  `Target.Subscription` that makes `endpointModeFor` select the **bare** `/responses`
  path (not `/v1/responses`); an api-key OpenAI target, whose `Subscription` is
  false, is unaffected and keeps the platform `/v1/responses` (*Api-key Responses
  passthrough*, below).
- Any other OpenAI flavor (chat completions, **portal chat**) is **translated** by
  a new outbound **OpenAI Responses translate client**
  (`internal/provider/openai_responses.go`): it renders the neutral request to a
  Responses body, POSTs the same bare `/responses` path, and parses the Responses
  SSE back to the neutral model.

The ChatGPT backend is far stricter than the public Responses API, so the
subscription translate client always sends what the Codex CLI always sends,
whatever the inbound request carried (REVERSE-ENGINEERED / VERIFY-LIVE):
`store: false` (a body without `store` defaults to `true`, which the
subscription backend rejects
with a 400), `include: ["reasoning.encrypted_content"]` (so reasoning round-trips
while nothing is stored), and a `reasoning` object — the request's effort, else
`medium` (a portal chat carries none, and the subscription catalog is the
reasoning-only gpt-5 family). `instructions` and `max_output_tokens` stay omitted
when empty. A non-2xx answer keeps the usual status → sentinel mapping
(401/403 → `ErrAuthRejected`, 503 → `ErrUpstreamStarting`, else `ErrUnavailable`)
and now also carries a bounded (4 KiB), single-line snippet of the vendor's error
body in the returned error and in the payload capture, because the backend states
why it refused a request only there.

#### Api-key Responses passthrough

An inbound **OpenAI Responses** request (`POST /v1/responses`, the **fine**
`openai_responses` flavor) to an OpenAI **api-key** account is relayed
**verbatim** to the platform's `https://api.openai.com/v1/responses` rather than
translated to `/v1/chat/completions`, which cannot carry what a Responses client
sends (tools, reasoning, `previous_response_id`, `store`, `include`, ...). The
resolver sets `ResponsesMode = passthrough` for exactly this one case
(`vendorAccountTarget`, keyed on the fine flavor exactly as the subscription
target is), and everything downstream is the existing native-passthrough layer:
the OpenAI-compatible client's native relay, `endpointModeFor` choosing the
standard `/v1/responses` (a non-subscription OpenAI vendor target), the account's
opened sealed key as the `Authorization: Bearer`, and the usual usage attribution
to the serving account (§5).

- **Only `model` is rewritten.** It is set to the account's bare upstream id,
  which equals the requested name when the account has no prefix. The rest of the
  body reaches OpenAI as the client wrote it, **including `store`, `include` and
  `previous_response_id`**: `store: false` is **not** forced here, because that
  is a requirement of the subscription backend, imposed by the subscription
  *translate* client only (above). The relay is value-lossless rather than
  byte-identical when a rewrite happens (key order may change). The response and
  its SSE are relayed unchanged and are not rewritten back (§6.5).
- **Unchanged:** chat completions and every other `openai` flavor to an api-key
  account, the `anthropic` dialect to any account, the **Anthropic** api-key
  account (an inbound Responses request there still translates to `/v1/messages`;
  there is no Responses surface to pass through to) and the whole subscription
  path.
- **Behaviour change (VERIFY-LIVE).** A model that the platform serves only over
  Chat Completions — for example `gpt-4o-search-preview` or
  `gpt-4o-audio-preview` — worked for a Responses client through the old
  translate path and would now **fail** on `/v1/responses`, where OpenAI rejects
  it. This is the expected cost of forwarding to the real Responses endpoint,
  not a regression to chase in the relay. Such ids are not excluded by the
  discovery heuristic (§6.1), so they can appear in an account's catalog. Which
  models the platform's `/v1/responses` accepts is OpenAI's to change and has
  not been checked against a live key; chat completions to the same model are
  unaffected.
- **Create-only.** Only `POST /v1/responses` (and its `/openai/v1/responses`
  alias) is registered. The follow-up routes `GET`/`DELETE /v1/responses/{id}`,
  `POST /v1/responses/{id}/cancel` and `GET /v1/responses/{id}/input_items` are
  not registered on the gateway and answer 404, so a client that stores a
  response and later fetches, cancels or lists its input items through the
  gateway cannot. The translate path never supported them either; proxying them
  is a documented follow-up, not part of this passthrough.
  `previous_response_id` itself rides the create body and so reaches OpenAI
  intact.

### 4.3 Credential resolution at the edge

For a subscription target, `subscriptionAuthCtx` resolves (and refreshes, §3.4)
the OAuth bearer, attaches it plus the target's static `ExtraHeaders`, and — for
OpenAI — the per-account `chatgpt-account-id`. The static headers are attached
even when no bearer is available, so an auth failure reads as an auth error
upstream rather than a missing API version. The api-key path is unchanged: its
sealed `APIToken` rides the vendor's own API-key header. The custom `x-api-key`
header is redacted in payload capture (a latent leak the feature closed).

## 5. Usage & limits

Neither vendor exposes an absolute subscription cap, so the panel shows
**percentages and reset times only**. They live in one per-account snapshot
(`vendor_account_usage`) that two independent, best-effort writers fill: a
**passive header scrape** on every served request (§5.1) and, for an OpenAI
subscription only, an **active pull** from the vendor's usage endpoint whenever
the account's models are refreshed (§5.2). Both write through the same **merge
rule** (§5.3), so neither can blank what the other learned.

### 5.1 Passive header scraping

At the single `recordUsage` choke point (`internal/gateway/inference_complete.go`),
when the served target is a vendor account, the gateway scrapes the vendor's
**rate-limit response headers** — which it already has in hand, so no extra
request is made — and writes the per-account snapshot. The scrape is **entirely
best-effort**: it never faults the inference request, logs the account id (and,
on a store read or write failure, that store error) at Debug — never a header
value or a token — and never writes an all-unknown snapshot (a
response that carries none of the recognized headers leaves the stored snapshot
alone). What it does parse is merged over the stored row (§5.3).

| Vendor | Headers (lowercased; VERIFY-LIVE) | Normalization |
|---|---|---|
| Anthropic subscription | `anthropic-ratelimit-unified-5h-utilization`, `-7d-utilization`, `-reset` | A 0..1 utilization **fraction** → ×100 percent; the single unified reset is attributed to the five-hour window. |
| OpenAI subscription | `x-codex-primary-used-percent` + `x-codex-primary-reset-at` (five-hour), `x-codex-secondary-used-percent` + `x-codex-secondary-reset-at` (weekly), `x-codex-credits-balance` | A 0..100 **percent** used as-is; the credit balance is an opaque string. |

Parsing is **tolerant**: a missing/non-numeric value leaves the window at its
unknown sentinel (`-1` percent, nil reset), never a fabricated `0`; a scaled
percent is clamped to `≤ 100`.

### 5.2 Active pull (OpenAI subscription only)

The headers arrive only with a served request, so a ChatGPT subscription that has
not been used lately shows no windows, or old ones. For that one account kind the
gateway can also **ask** the vendor. An OpenAI `api_key` account (a platform
account has no subscription window to report) and every Anthropic account have no
such endpoint; for Anthropic the header scrape stays the only source.

| Credential | Request | Provenance |
|---|---|---|
| OpenAI **subscription** | `GET https://chatgpt.com/backend-api/wham/usage` with `Authorization: Bearer`, `ChatGPT-Account-Id` (left out when the account id is unknown) and `User-Agent: codex-cli`; no query, no body | reverse-engineered from the open-source Codex client, **VERIFY-LIVE** — not yet confirmed against a live account |

The fetcher is `vendorauth.FetchOpenAISubscriptionUsage` (`internal/vendorauth/usage.go`),
built like the discovery fetchers (§6): one GET, **no error return**, only a
snapshot and a status, `ok` or `unverifiable`. The URL and the `User-Agent` live in
`constants.go` beside the other VERIFY-LIVE constants. The body is mapped as
`rate_limit.primary_window` → the five-hour window and `rate_limit.secondary_window`
→ the weekly one (each `used_percent` and `reset_at`, an absolute unix time in
seconds), and `credits.balance` → the credit balance. The other fields of the
`credits` object, `has_credits` and `unlimited`, are **not read** (follow-up, below).

Parsing is as tolerant as the scrape's, and for the same reason: a percent is
clamped to `≤ 100` and a negative one is unknown; an absent, null or `0` reset is
unknown; a balance is kept as the string the vendor wrote (a JSON number is
coerced to its literal, one longer than 64 bytes is unknown); a field of the wrong
type is skipped without sinking its siblings. The verdict is `ok` only when **at
least one** field could be read. Every other answer — any non-2xx (a 401 included:
it is no verdict on the credential, and only the dispatch moves an account to
`needs_reconnect`), a redirect (never followed), a timeout, a transport failure, a
body that is not JSON, or JSON that carries none of the fields — is `unverifiable`,
and an empty answer is deliberately not "OK, nothing known", for the reason given
for discovery (§6.3): it must not be able to wipe a stored snapshot. The response
is capped at the same 8 MiB as a discovery fetch (§6.4).

**When it runs.** The pull is the **last, best-effort step of
`RefreshVendorAccountModels`** (§6), so it runs wherever that does: on the explicit
`POST .../models/refresh` ("Modelle aktualisieren") and, because the connect flows
call the same function, at the end of every OpenAI-subscription connect (§6.2). There is
no separate usage endpoint, trigger, background job or startup step. Specifically:

- It reuses the **token set the model discovery already opened** and, if it had
  expired, renewed through the gateway's locked refresher (§6.3). The usage code
  never opens, renews or refreshes a token itself, and a token that could not be
  renewed means no pull.
- It runs only when the refresh itself did not fail with an error, and **also when
  the model list was unusable** (the refresh then reads `unverifiable` and keeps
  the models): the usage endpoint is a separate request that may well succeed.
- It runs **after** the model rows are written, so a slow usage endpoint can never
  starve the model write of the connect-time bound.
- It is purely additive: it cannot change the refresh's result, its account view or
  its error, the pull itself never changes the account's status, and it logs only
  the account id (and, on a store read or write failure, that store error) at
  Debug — never the token, the ChatGPT account id or any vendor text.

**Time budget.** The explicit refresh now makes up to **two** vendor requests, each
bounded by the discovery client's 10 seconds (the model list, then usage), so it can
take about 20 seconds against the server's 30 second write timeout. The connect
flows stay inside their single 5 second budget (§6.2), which the usage pull
**shares** with the token renewal, the model list and the model write: behind a
slow model list the budget is spent and the pull degrades to `unverifiable`,
leaving the snapshot as it was. That is acceptable for an advisory number, and the
next refresh or served request fills it in.

### 5.3 The merge rule

`UpsertVendorAccountUsage` replaces the whole row, and each writer sees only part of
the picture: the OpenAI scrape carries the credit balance only when the response
does, and a pull may know the windows but not the balance. If a write simply
replaced the row, a partial reading would blank known values. So **every usage
write keeps each known field and never blanks a stored one**, not merely "never
replaces a good snapshot with an all-unknown one".
Both writers read the stored row and combine it with their reading through
`routing.MergeVendorAccountUsage(existing, incoming)`, a pure function. Per field the
result takes `incoming` when it **knows** the field and keeps `existing` otherwise:
a percent is known when it is `≥ 0` (a real `0` is known, `-1` is not), a reset time
when non-nil, the credit balance when non-empty. A field no writer has ever seen
stays unknown (`-1` / nil / `""`) and is never turned into a fabricated `0`: with no
stored row, the reading is written as it is. A stored row that cannot be read is
handled per writer, both best-effort: the scrape writes its own reading as it is,
the pull writes nothing.

Two consequences to know:

- `UpdatedAt` is the time of the **last write**, not a per-field freshness. A credit
  balance carried over a scrape that only reported the windows still shows the
  fresh `UpdatedAt`, so the panel's "updated" time says when the snapshot was last
  touched, not how old each number is.
- The read-merge-write takes **no per-account lock**, in either writer. Two that
  overlap (a scrape during a refresh) can each merge against a read that misses the
  other's newer write, and the later write wins. The worst case is one writer's
  fresh reading being lost to the other's slightly older one, which the next write
  repairs; a field that was already stored when both read is never blanked. This is
  accepted for a best-effort snapshot; closing it would need a store-level merge
  upsert.

### 5.4 Reading it, and what is deferred

The snapshot is exposed only on the **detail** read (`GET /api/portal/vendor-accounts/{id}`
→ `VendorAccountDTO.Usage`, `omitempty`), never on the list (one snapshot read per
row would be an N+1), and a `-1` window is hidden in the UI rather than shown as a
real 0 %. The detail view's usage panel **re-reads** the account after a successful
models refresh — whatever its `ok` / `unverifiable` answer, since the pull may have
changed the snapshot either way — so a fresh pull shows without reloading the page;
a refresh that failed with an error does not trigger it. API-key accounts carry no
subscription window; absolute €/$ spend accounting is deferred.

Deliberately **not** in this change, and listed as follow-ups: the credit object's
`has_credits` and `unlimited` flags (so a plan with no credit balance cannot yet be
told apart from a balance the vendor did not send), and a **dedicated
`POST .../usage/refresh` endpoint**, so the usage can be refreshed without also
re-listing the models.

## 6. Dynamic model discovery and the model prefix

The catalog an account serves is seeded at creation from a small static set, and
that set is only a guess (§10): a ChatGPT subscription's Codex backend serves
newer model generations than any list kept in this repository, and a vendor
renames or retires models without notice. So the gateway asks the vendor which
models the account's **own credential** can really use and **replaces** the
guess with the answer. The static set stays as the create-time fallback and as
what an account keeps whenever discovery yields nothing.

Discovery is split like the validation probes (§3.5). One fetcher per credential kind lives
in `internal/vendorauth/discover.go` and never returns an error — only a list
and a status, `ok` or `unverifiable`. The service around it
(`portal.Service.RefreshVendorAccountModels`, `service_vendor_discovery.go`)
opens the sealed credential, picks the fetcher, validates and caps what the
vendor sent, applies the account's prefix and replaces the rows in one
transaction.

### 6.1 Where the list comes from

| Credential | Request | Kept | Provenance |
|---|---|---|---|
| OpenAI **subscription** | `GET https://chatgpt.com/backend-api/codex/models?client_version=<V>` with `Authorization: Bearer`, `ChatGPT-Account-Id` (left out when the account id is unknown) and `originator: codex_cli_rs` | entries whose `visibility` is `list` **and** whose `supported_in_api` is `true`; slug and display name | reverse-engineered, **live-confirmed** |
| OpenAI **`api_key`** | `GET https://api.openai.com/v1/models` | ids that look chat-capable (below); the listing has no display name, so the id doubles as one | documented public API |
| Anthropic **`api_key`** | `GET https://api.anthropic.com/v1/models?limit=1000` with `x-api-key`, `anthropic-version` | every `data[].id` with its `display_name` | documented public API |
| Anthropic **subscription** | the same URL with `Authorization: Bearer`, `anthropic-beta: oauth-2025-04-20`, `anthropic-version` | as above | reverse-engineered, **VERIFY-LIVE** |

An OpenAI api-key listing names every model the key reaches, with no capability
data, so it is narrowed by a small heuristic: the `gpt-*`, `chatgpt-*` and
o-series families (and fine-tunes of them), minus ids that mark an embedding,
speech, image, moderation, realtime or completion-only model. A model the rule
does not recognize is left out — a missing model is added by a newer rule, while
a non-chat model offered wrongly fails every request routed to it.

**What "live-confirmed" covers, and what it does not.** The operator ran the
Codex catalog request against a real subscription and got the real catalog: the
endpoint, the three headers and the response shape are known to work, and the
answer listed model ids no static list here held (for example `gpt-5.6-luna`,
`gpt-6-luna`, `gpt-6.1-sol`). That settles the request, not the backend: it is
still undocumented, reached under the subscription path's Terms caveats (§10),
and free to change without notice. The two api-key rows are the vendors'
documented public listings. Whether Anthropic serves a consumer OAuth bearer a
model list at all is **unconfirmed** (VERIFY-LIVE): whenever that endpoint
answers with anything but a usable list the result is `unverifiable` and the
account keeps its static seed.

### 6.2 When it runs

- **At connect.** After a subscription's tokens are stored — by token import,
  code paste or device code alike — a discovery runs best-effort under a bound
  of 5 seconds for the whole of it (token refresh, fetch and write, and for an
  OpenAI subscription the usage pull that follows, §5.2). A vendor
  that hangs adds at most that to the connect; whatever goes wrong is logged
  without a credential and the connect still succeeds with the models it had.
  The import and complete responses carry the account as it then serves; the
  device poll only reports `connected`, and the portal re-reads the account.
- **On demand.** `POST /api/portal/vendor-accounts/{id}/models/refresh` — the
  "Modelle aktualisieren" button of the account's Models panel in the detail view
  — runs the same discovery and may wait for the vendor client's own 10 second
  timeout, twice for an OpenAI subscription (the model list, then the usage pull
  of §5.2; [API Surface](../reference/api-surface.md#vendor-accounts-anbieter)).
  An `api_key` account has no connect step, so this is how it leaves the static
  seed.
- **Never otherwise.** No request path, background job or startup step runs a
  discovery, and migration 83 rewrites no catalog: an account that predates it
  keeps its rows until its owner refreshes or reconnects.

### 6.3 Fail-soft

Discovery never reduces what an account already serves and never blocks a
connect. When the vendor cannot be asked, answers with any non-2xx status (a 401
included — that is not a verdict on the credential; the test-connection action
is, §3.5), redirects, sends a body that is not a catalog, or lists nothing
usable, the existing rows stay exactly as they were and the answer is
`status: unverifiable` with a one-line `detail`. An **empty** list is
deliberately `unverifiable` and not "OK, no models": a changed vendor schema or
a filtered-out catalog is far likelier than a credential with no models, and an
empty OK would let a schema change wipe a working set. A missing credential (no
key set, subscription not connected) reads `unverifiable` too. Only a stored
credential that cannot be opened (lost key, corrupt blob) is an error,
`409 vendor_account.credential_unreadable`; after a connect it is logged and
ignored.

**An expired token is renewed first.** The vendor answers an expired access token
with a 401, so a subscription access token that is past its expiry and has a
refresh token is renewed through the gateway's own locked refresher before the
vendor is asked: `Server.RefreshVendorSubscriptionTokens`, the refresh the
dispatch itself runs under its per-account lock (§3.4), handed to the portal at
start-up so the two cannot race for the single-use refresh token. The portal never refreshes by
itself. The renewed set is read back from the store, so the fresh token is the
one sent. If the refresh fails the result is `unverifiable` with a retry-or-
reconnect note. If the vendor **rejected the refresh token**, the refresher has
already moved the account to `needs_reconnect`; the response says to reconnect
and its `account` reports the new status, not the one loaded before. A token with
no refresh token can never heal and is simply tried. Because a rotated refresh
token is single-use, the renewal is not cancelled when the request ends — a
client that disconnects, or the 5 second connect bound, only stops the wait (the
discovery reports `unverifiable` on time) while the refresh runs to completion
under a bound of its own and persists its result.

### 6.4 What is stored, and how it is bounded

What the vendor sends becomes model ids, header values and rendered text, so it
is untrusted and capped before it is stored:

- **At most 500 models** per discovery.
- **Slug:** 1 to 128 bytes, from `A-Z a-z 0-9 . _ ~ : / @ + -`, starting with a
  letter or digit and containing no `..` — the URL-path-safe subset a model id
  needs to travel in `/v1/models/{id}`, JSON and logs. It is used exactly as
  sent (no trimming, no case change), since it is the id the vendor expects back.
  An entry whose slug fails the rule is dropped, and a repeated slug is stored once.
- **Display name:** trimmed, printable ASCII only (`0x20`–`0x7E`) and at most 128
  bytes, cut if longer. A name with anything else is stored as `''` and the UI
  falls back to the model id, because a hostile one would be rendered.
- **Response body:** the discovery fetch has its own **8 MiB** limit, separate
  from the 1 MiB the credential probes and token endpoints share, because the
  live Codex catalog already weighs about 0.6 MiB (every entry carries its base
  instructions) and grows with each model; the shared cap would soon cut it into
  an unparseable body, a silent `unverifiable` for a working credential.

`refresh.detail` says how many models were stored and, when entries were
dropped, how many. It carries no credential and no vendor text. The write is a
whole-set replace under a per-account lock (`accountLocks`), the same lock the
prefix re-label (§6.5) takes, so a discovery and a prefix change that overlap
cannot undo each other; the account is re-read inside the lock, so a prefix
changed while the vendor was being asked is the one applied.

### 6.5 The model prefix

An account's optional `model_prefix` (§1) is the namespace its models are
requested under. A row carries both names: `gateway_model` is `model_prefix` +
the vendor's slug, what clients list and request, and `upstream_model` is the
bare slug, what the vendor is sent. With the prefix `chatgpt/`, the vendor's
`gpt-6-luna` is advertised and requested as `chatgpt/gpt-6-luna`, and the
resolver (§4) matches on the first and sets the target's `ProviderModel` to the
second. The prefix is applied wherever rows are written: to the creation seed, to
every discovery, and on a PATCH that carries `model_prefix`, which **re-labels the
existing rows** without asking the vendor again. Re-sending the stored prefix
repairs rows a failed re-label left behind. Two rows whose labelled ids collide
keep only the first.

Two behaviours to know:

- With a prefix set, the bare vendor id is **not** served by that account.
  Different prefixes are also how a user keeps two accounts that offer the same
  model both reachable.
- On the native-passthrough path (an OpenAI subscription's or an OpenAI api-key
  account's `/v1/responses`, §4.2) the gateway rewrites only the request's `model`
  field to the bare slug. The vendor's response is relayed verbatim and there is
  no response-side rewrite, so its `model` field echoes the **bare** slug, not
  the prefixed name the client asked for. A recorded usage event keeps the
  requested (prefixed) name as its model and the bare slug as the provider
  model.

### 6.6 The `client_version` knob

The Codex catalog request names a Codex app version, and the ChatGPT backend
**hides every model whose `minimal_client_version` exceeds it**. The version
sent is the system setting `vendor_openai_codex_client_version`, default
`26.930.61225` (the version the confirmed request used). A stale value does not
fail — it quietly leaves new models out of the catalog, so a model OpenAI has
shipped never shows up in the portal. **The setting must be raised whenever
OpenAI ships a newer Codex app** (System settings, no redeploy; blank resets to
the default; the next refresh uses it). The default will age, and the gateway
cannot tell that a model is missing, because the backend simply omits it from the
answer; the remedy is to raise the setting to the current Codex app version and
refresh again. Both the hiding rule and the default are VERIFY-LIVE, and the upkeep is
tracked in [Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks).
The setting is read by `portal.Service` at discovery time, never by the
resolver, so it needs no cache (§7).

### 6.7 Where an account's models show up

Every listing reads **one** source, `ownVendorAccountModels`: the principal's
own **active** vendor accounts and their rows, behind the master flag, for a user
principal only (a service token owns no account) and fail-open per account. What
the listings advertise and what the dashboard shows therefore cannot drift, and a
vendor model is never subject to the gateway-wide hidden/locked suppression,
which applies to self-hosted models.

| Surface | Vendor models of the caller's own accounts |
|---|---|
| `GET /v1/models`, `/openai/v1/models`, `/anthropic/v1/models`, `/api/v0/models` | Listed under the prefixed name, per dialect the account serves (the served-flavor matrix of §4: the OpenAI subscription only under `openai`). |
| Portal chat picker and a regular user's **Models** page | Listed. |
| **Dashboard "Live Model Routes"** (`GET /api/portal/dashboard`) | Listed, one row per model: the prefixed name, the vendor as provider, the account's name as host, status `active`. A vendor model has no server, so its row id is the account id plus the model; the portal keys rows by that id because two accounts may share a name and a model. |
| **Admin Models management** (`ManageModels`, the admin view of the Models page) | **Not listed — deliberately.** It shows the system's real models, and a vendor model is one principal's own, not something an operator manages for the system. |
| Another user's account, or a disabled / `needs_reconnect` account | Never. |

The asymmetry is intended and worth stating, because it can read as a bug: an
admin who connects an account sees its models on their own Dashboard, in the
chat picker and in `/v1/models`, but not on the Models management page. The
account's own detail view lists all its rows with the vendor's display name and
the id clients request, and carries the refresh button.

## 7. Feature flag and routing mode

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
| `vendor_openai_codex_client_version` | version string | `26.930.61225` | The Codex `client_version` the OpenAI **subscription** model discovery sends (§6.6). The ChatGPT backend **hides every model whose `minimal_client_version` exceeds it**, so a stale value silently hides new models: **raise it when OpenAI ships a newer Codex app** (System settings, no redeploy). Blank resets to the built-in default (`vendorauth.CodexModelsClientVersionDefault`); a malformed value is a `400 system.vendor_openai_codex_client_version_invalid`. Read only by `portal.Service` (never the resolver), so it has no cache. VERIFY-LIVE. |

The frontend reads the master flag through a portal-scoped
`GET /api/portal/vendor-accounts/enabled` (`{module_enabled}`, readable by any
user even while the module is off — it exists to let the shell show or hide the
nav item without granting the system-scoped settings read), mirroring the
NetBird/certificates `/enabled` endpoints. The exact-path route wins over the
`{id}` subtree because account ids are always `va_`-prefixed.

The same master flag gates the **model-listing owner overlay** and the dashboard
rows that carry an account's models (§6.7).

## 8. Owner-scope RBAC

The HTTP handlers gate on the standard web scope (`gateway:use`), then per-object
authorization happens **in `portal.Service`**: a user may read/manage only
accounts where `owner_user_id` equals the principal, mirroring `authorizeServer`.
A non-owner gets the **same `404 vendor_account.not_found`** as for a
non-existent account (the no-existence-leak rule). The `system` scope may **read**
any account (`GetVendorAccount`), but `ListVendorAccounts` and every **write**
(create/update/delete/connect) are owner-only — a deliberate read/write
asymmetry. The **test-connection** action (§3.5) and the **model refresh** (§6.3)
are not writes of the account's credential but are owner-only for the `system`
scope too, because each sends the stored credential to the vendor: they are
authorized like a write, not like a metadata read. Routing and the model
listings (§6.7) enforce the same owner scope by enumerating only the principal's
own accounts, so one user's account can never serve, or be listed to, another.

## 9. Secrets at rest

Credentials reuse the repo-wide secret envelope verbatim — no new key, no new
mechanism ([ADR-007](../09-architecture-decisions.md#adr-007--secrets-at-rest-the-encplain-scheme)):

- The `api_key` and `oauth_tokens` columns are **sealed** with
  `capture.SealSecret` **before** the store write, and opened with
  `capture.OpenSecret` at the cipher-holding dispatch edge **and, for the owner's
  explicit test-connection (§3.5) and model refresh (§6.3), inside
  `portal.Service`** (`checkVendorAccount`, `RefreshVendorAccountModels`).
  On those paths the opened value goes only to the vendor's own validation,
  model-listing or (OpenAI subscription) usage URL: it is never returned, logged or echoed in a verdict's or a
  refresh's `detail`. Likewise a token import probes the plaintext access token
  the user just submitted, before it is sealed. A refresh re-seals in place.
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

## 10. ToS, experimental status, and VERIFY-LIVE

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
  endpoint (including the two subscription credential-validation probes, §3.5, the
  two subscription model-list endpoints, §6.1, and the ChatGPT usage endpoint,
  §5.2),
  client id, redirect URI, scope, beta header, device-code path, token-claim name,
  masquerade requirement, serving host, request/refresh body encoding, and the
  rate-limit response-header names can change without notice.
  They are confined to `internal/vendorauth/constants.go` and the resolver's
  dispatch literals (routing cannot import `vendorauth`), all marked VERIFY-LIVE,
  and all parsing is tolerant (prefix-matched, fail-open). One value is further
  along than the rest: the Codex model-catalog request (URL, headers, response
  shape) is **live-confirmed**, an operator having run it and received the real
  catalog. That confirms the request as sent, not the backend's stability, and
  the `client_version` hiding rule and its default, as well as whether Anthropic
  serves a consumer bearer a model list, stay VERIFY-LIVE.
- **The static model seed is a fallback guess, and the OpenAI set in it depends
  on the auth type.** The curated set (`internal/portal/vendor_catalog.go`) is
  written when an account is created, keyed by vendor **and** auth type, and is
  what the account serves until discovery (§6) replaces it: an `api_key` account
  until its first refresh, a subscription account when no discovery could be had
  at connect, and an Anthropic subscription account for as long as that vendor's
  consumer-bearer listing stays unconfirmed. An OpenAI **`api_key`** account talks
  to `api.openai.com` and gets the full set (`gpt-5`, `gpt-5-mini`, `gpt-4.1`,
  `o3`). An OpenAI **subscription** account is served by the Codex ChatGPT
  backend, which, as far as is known, does not serve `gpt-4.1` or `o3`; it is
  therefore seeded with only `gpt-5` and `gpt-5-mini`, so a working credential is
  never paired with a model its backend is known to refuse — the auth-versus-model
  confusion that §3.5 exists to take apart. Anthropic's OAuth Messages path and
  its API key serve the same ids, so its set does not vary. Even the narrowed
  subscription set is unverified, and an account serves nothing its rows do not
  name — which is why the real catalog is discovered rather than kept here.
  **There is no backfill:** nothing rewrites existing catalogs at upgrade or in
  the background. An account created before discovery keeps its seeded rows until
  its owner refreshes the models or reconnects, so a subscription account created
  before the auth-type split still carries `gpt-4.1` and `o3` (which that backend
  rejects) until a successful discovery replaces them.
- **Discovery needs a live credential and a high enough `client_version`.** An
  access token that is expired and cannot be renewed — no refresh token came with
  it, the vendor rejects the refresh token (the account then reads
  `needs_reconnect`), or the vendor is unreachable — cannot be asked, so the
  account keeps its old rows until its owner retries or reconnects. Separately,
  the Codex catalog silently omits models newer than the configured
  `client_version` (§6.6). Both degrade to "fewer models than the vendor offers",
  never to a wrong or emptied catalog.
- **Known behavioral gaps.** A 401 on an unexpired-but-revoked access token does
  not flip the account to `needs_reconnect` (only a refresh rejection does); a
  persist-failure after a refresh may keep a single-use refresh token, forcing a
  reconnect on the next request. Both are bounded by the token TTL and accepted.
  A third is **not** accepted but tracked: a PATCH that races a refresh can write
  a stale `oauth_tokens` back, and its fix is a column-subset PATCH writer in
  every store driver (§3.4; [Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks)).
- **Single-process assumptions.** The pending-connect state (PKCE verifier/state
  and the device-code pending entry), the per-account refresh lock and the
  per-account model-write lock (§6.4) are all **in-process**: a gateway restart
  loses any connect in progress (the user starts it again), and a multi-replica
  deployment (the PostgreSQL driver) would break the refresh single-flight — two
  replicas could refresh one account concurrently and burn its single-use refresh
  token — and let a discovery and a prefix re-label undo each other. The feature is an operator-only experiment, so
  one gateway process is assumed.

## Related chapters

- [Routing & Model Selection](routing-and-model-selection.md) — the resolver and
  candidate scoring the vendor-account source plugs into.
- [API Compatibility & Inference](compatibility-and-inference.md) — endpoint
  modes, native passthrough vs. translate, the neutral inference model.
- [Security, Authentication & Authorization](security-auth-rbac.md) — scopes and
  object-level authorization.
- [Data Model](../reference/data-model.md#external-vendor-accounts-anbieter) — the
  three tables and the `usage_events.account_id` column (migration 82), and the
  prefix and display-name columns (migration 83).
- [HTTP API Surface](../reference/api-surface.md#vendor-accounts-anbieter) — the
  `/api/portal/vendor-accounts*` routes (the model refresh included) and their
  error codes.
- [Configuration & Environment Variables](../reference/config-env.md) — the cipher
  key and the three system settings.
- [ADR-049](../09-architecture-decisions.md#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted)
  — the first-class-entity and experimental-subscription decisions.

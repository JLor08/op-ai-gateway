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

Two bespoke vendors ship: **OpenAI** and **Anthropic**. Each is reachable through
either of two auth types — a documented, metered **API key**, or an
**experimental, ToS-restricted subscription** path. The subscription path reuses
consumer OAuth tokens through a third-party gateway, which is against both vendors'
consumer Terms and is reverse-engineered throughout; it is **off by default**,
behind a master feature flag, and every reverse-engineered constant is marked
**VERIFY-LIVE** (§10).

Five further vendors are **OpenAI-compatible hosted providers**: **x.ai** (Grok),
**OpenRouter**, the **Kilo Gateway**, **Google Gemini** (through Google's
OpenAI-compatible shim) and a **Custom** endpoint the user addresses themselves.
They speak OpenAI `chat/completions` with a Bearer key, so they are **api-key
only** presets over the one OpenAI client, driven by a data registry and a stored
`base_url` rather than by bespoke code (§4.4,
[ADR-052](../09-architecture-decisions.md#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)).

One more vendor reaches Google Gemini through its **own** wire instead of the
OpenAI shim: **Google Gemini (native)**, vendor id `google_native`. It is a preset
too (api-key only, a stored root, the same registry), but its `Dialect` selects a
dedicated provider kind and client that speak `generateContent` and authenticate
with `x-goog-api-key` (§4.5,
[ADR-054](../09-architecture-decisions.md#adr-054--native-google-gemini-dialect-its-own-vendor-id-and-provider-kind-selected-by-the-preset-dialect-field-over-a-leaf-translation-package)).
It is a separate account from the shim `google` one, so both can exist side by side.

The feature reuses the gateway's existing credential sealing, routing `Target`,
dispatch and usage machinery wherever possible. The genuinely new parts are a
first-class account entity (§1), an OAuth subsystem — connect + token refresh —
(`internal/vendorauth`, §3), model-independent credential validation that
tells a rejected login from a wrong model (§3.5), a small native Anthropic
Messages client, an OpenAI Responses translate client and a native Gemini
`generateContent` client (`internal/provider`, §4), two static dispatch extensions (extra headers + a system-prompt masquerade,
§4), a usage/limits snapshot fed by the vendor's rate-limit response headers and,
for an OpenAI subscription and for an OpenRouter or Kilo api key, an active usage
pull with its own refresh endpoint (§5),
and a discovery of each account's real model catalog from the vendor, served under
an optional per-account prefix (§6). An account is **personal** by default, and its
owner, if an admin, may **share** it through the management-group and
resource-group machinery AI servers use, at a tiered share level (§1.1), with a
routing eligibility that is a **pure grant** and deliberately not the server rule
(§4.6); who may do what to a shared account is authorization by level (§8). Which of
the accounts a principal can reach each API token may use is the token's own,
strictly opt-in decision, per account and always by name for a shared one, with an
optional per-token prefix override (§11,
[ADR-060](../09-architecture-decisions.md#adr-060--shared-vendor-accounts-a-tiered-share-level-a-pure-routing-grant-authorization-by-level-and-a-per-account-opt-in)).

## 1. The entity and its ownership

A `vendor_account` is owned by exactly one user and is **personal** by default
(`share_level = none`): usable only for that user's own requests, the owner's
interactive portal session and the owner's own API tokens **that have been opted in
to it** (§11; a token reaches no account until the owner grants it). The owner, if an
admin, may **share** it (§1.1): it is then also usable by the principals, users and
services, provisioned into a resource group that contains it, and manageable by the
co-managers of the management groups it is assigned to, at the share level's reach.
Sharing is the second phase of ADR-049, built as that ADR shaped it (a link table plus
a resolver filter extension, not a rewrite), and sits behind its own default-off
master switch (§7).

| Field | Meaning |
|---|---|
| `id` | `va_`-prefixed, random hex. |
| `owner_user_id` | FK → `users(id)` `on delete cascade`. The single owner, by design (§1.3): the one principal a personal account serves, and the only one who can raise it. |
| `vendor` | `openai` \| `anthropic` (`routing.VendorOpenAI` / `VendorAnthropic`) \| `xai` \| `openrouter` \| `kilo` \| `google` \| `google_native` \| `openai_compatible` (`routing.VendorXAI` / `VendorOpenRouter` / `VendorKilo` / `VendorGoogle` / `VendorGoogleNative` / `VendorOpenAICompatible`; the last is the Custom endpoint). The six newer ids are the **preset** vendors (`routing.IsPresetVendor`): five are the OpenAI-compatible vendors (§4.4; `routing.IsOpenAICompatibleVendor`, a preset whose `Dialect` is empty), and `google_native` is the native Gemini one (§4.5; `routing.IsNativeGeminiVendor`, `Dialect == "gemini"`), distinct from the shim `google`. Immutable after creation. |
| `auth_type` | `api_key` \| `subscription` (`routing.VendorAuthAPIKey` / `VendorAuthSubscription`). Immutable after creation. |
| `name` | User-facing label. |
| `status` | `active` \| `disabled` \| `needs_reconnect`. The last is system-managed (§3.4): a refresh rejection flips an account to it; the operator cannot set it directly. |
| `api_key` | **Sealed** (`enc:`/`plain:`), populated only when `auth_type = api_key`. |
| `oauth_tokens` | **Sealed** JSON token set, populated only when `auth_type = subscription`. |
| `model_prefix` | Optional per-account namespace for the account's model ids (migration 83; `''` = none). The service trims it and accepts at most 64 bytes from `A-Z a-z 0-9 - _ . ~ : / @ +` (the URL-path-safe characters of a model id) with no `..`, anything else being `400 vendor_account.model_prefix_invalid`. It is **applied**, not merely stored: every model the account serves is advertised and requested as the prefix plus the vendor's own id, while the vendor is still sent the bare id (§6.5). The DTO reports it and the create/update requests accept it. |
| `share_level` | `none` \| `shared` \| `full` (`routing.VendorShareLevelNone` / `VendorShareLevelShared` / `VendorShareLevelFull`; migration 91, `text not null default 'none'`, so every account that predates sharing is personal with no data step). Changed only by the share-level lifecycle (§1.3), never by the full-row update or the create, which both leave it at its column default. A level the service does not recognise reads as `none`, fail closed. See §1.1. |
| `system_group_id` | The **containment root** of a shared account (migration 91; opaque, no foreign key like `ai_servers.system_group_id`; `''` until the account is first raised): the one system group its management groups and its linked resource groups must sit under (§1.2). |
| `base_url` | The upstream **root** URL of a preset account, OpenAI-compatible or native Gemini (migration 86; `''` for the bespoke `openai` / `anthropic` accounts, whose hosts are fixed). Stored as a root **without** `/v1` or any other path prefix: the client appends the preset's prefix and the resource (`{prefix}/chat/completions`, `{prefix}/models`; §4.4), or, for native Gemini, its own `/v1beta/models/{model}:generateContent` (§4.5). Defaulted from the preset at create (required for `openai_compatible`), https only with no userinfo, query or fragment, and **immutable** after creation: it is absent from the update request. The DTO always carries it. |

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
(§10), written when the account is created (empty for the preset vendors, which
have no seed and are filled by discovery alone), and is **replaced
by the vendor's real list** once discovery has run (§6). The three tables and the
`usage_events.account_id` attribution column are migration 82; migration 84 later
adds the spend-control columns to the usage snapshot (§5.2), and migration 86 the
immutable `base_url` of a preset account (OpenAI-compatible or native Gemini);
migrations 90 to 92 add the share model (§1.1): the sixth co-manager flag, the
`share_level` and `system_group_id` columns with the `vendor_account_admin_groups`
table, and the `resource_group_vendor_accounts` link table; see
[Data Model](../reference/data-model.md#external-vendor-accounts-anbieter).

The domain type is `routing.VendorAccount`, stored across all three drivers
(memory / SQLite / PostgreSQL) through the usual `routing.Store` composition; the
portal view is `portal.VendorAccountDTO`, which carries **no** credential material
— the sealed key and token set are reduced to the `api_key_set` /
`subscription_connected` booleans ([Secrets at rest](#9-secrets-at-rest)), whoever
the viewer is. It is rendered **for the viewer** and gains the share fields
(`share_level`, `admin_groups`, `system_group_id` / `system_group_name`, `is_owner`,
`shared_by`); a **member** of a shared account gets a separate, minimal allowlist
view (§8.2).

### 1.1 Share levels

`vendor_accounts.share_level` is an enum, not a boolean: three named states read
better than two flags, and the level decides how much a co-manager (CM) may do
(§8). It is `text`, like the table's other free-text enums (`auth_type`, `status`).

| Level | Meaning |
|---|---|
| `none` | A **personal** account: the default, and what every account is until its owner raises it. Never in anyone else's pool. A management-group or resource-group link that somehow survives at `none` grants nothing, because every gate and listing reads the level live. |
| `shared` | The account enters the server-like model: it is assigned to management groups and linked into resource groups. A CM may **operate** it (status, model prefix, rename, test connection, model refresh, usage refresh); the credential, the management-group and resource-group assignment, the delete and the lowering stay owner-only. A principal provisioned into a linked resource group may route to it once its token opts in (§4.6, §11). |
| `full` | **Complete owner-equivalent delegation**: a CM may do everything the owner can, the credential, the group and resource-group assignment, the delete and the lowering, except **raise** the level. The owner's control is structural (they own the record and chose the level, and can always lower it) and not an operation reserved at `full`. A subscription account can never be `full` (§10). |

The full authorization matrix is §8.1. With the sharing master switch off (§7) the
**effective** level of every account is `none`: a pause, not an un-share.

### 1.2 Management groups, the containment root and resource-group links

Management and assignment **mirror AI servers**, with the vendor-account twin of each
piece, and routing eligibility does **not** (§4.6):

- **Management groups** (`vendor_account_admin_groups`, at least one while the account
  is shared): the admin-tier groups whose owners and co-managers manage the account. A
  CM is an owner or co-manager of one of them who holds the sixth co-manager
  permission `can_manage_vendor_accounts` (migration 90, a one-line wrapper
  `vendorAccountManageGroupIDs` over the predicate its five siblings use; default
  `true`, so no existing co-manager is narrowed).
- **The containment root** (`system_group_id`): like a server, a shared account's
  management groups share one parent system group, which becomes its root. It is set at
  the first raise from `none` and is **immutable while the account stays shared or
  full**; only a downgrade to `none` clears it. An account and a resource group link
  only when their roots match (`vendorAccountRootMatches` fails closed on an empty
  root). The edge: an FK cascade may delete the last `vendor_account_admin_groups` row
  while the account stays shared with a non-empty root. The account then has zero
  management groups, remains owner-manageable and lowerable, and re-adding a
  management group must match the retained root (or a downgrade to `none` resets it).
- **Resource-group links** (`resource_group_vendor_accounts`): the groups whose
  provisioned principals can route to the account. There are two paths that write the
  same single row, mirroring `resource_group_servers`: the **owner's self-service**
  (join and leave a group the owner is a member of an admin group of, at `shared` or
  `full`, on `.../vendor-accounts/{id}/resource-groups`) and the **resource-group
  manager's side** (a manager with `can_manage_resources` adds an account to the group
  on `PUT /resource-groups/{id}/vendor-accounts`, additionally needing to operate the
  account, the account to be `full`, and the roots to match; removal needs only the
  group). A link makes an account **offerable**; it never makes it **usable** by
  itself (§4.6).

### 1.3 Raise, lower and the owner-delete consequence

- **Raise** (`none` to `shared` to `full`): the **owner**, who must be an admin right
  now (the token carries the admin scope **and** the user's current role is `admin` or
  `system_admin`), on an owned account, carrying **at least one** management group
  (which establishes the root). A subscription account may be raised to `shared` only
  and only while `vendor_subscription_sharing_enabled` is on (§10). The writes are
  ordered **root, then groups, then the level last**, so every failure leaves the
  account private or unchanged and never half-shared: until the level moves, every gate
  and listing reads the account as `none`, so a link or a root left behind by a failed
  raise grants nothing and is overwritten by the retry.
- **Lower**: the **credential gate's** shape: the owner at every level (ownership alone,
  so an owner can un-share even after losing the admin role) and a CM at `full` only. It
  never looks at the subscription flag: an account can always be un-shared. Lowering to
  `none` is `ClearVendorAccountSharing`, **one transaction** that sets the level to
  `none` **first**, then deletes every management-group link and every resource-group
  link, then clears the root, so even a torn write leaves the account owner-only. It
  always runs, even for an account that already reads `none`, so it is idempotent and
  also scrubs a leftover link. `full` to `shared` changes only the level (the groups,
  the root and the links stay). A downgrade takes effect on the next resolve, because
  the grant re-reads the live level (§4.6).
- **Memory-store cascades.** Deleting an account drops its management-group links by a
  single-key delete and its resource-group links by a **reverse-direction loop over
  every group** (the map is keyed group to account, like `resource_group_servers`); a
  resource-group delete drops its own entry; the conformance suite asserts both on all
  three drivers. As with `server_admin_groups`, a user-group delete cascades
  `vendor_account_admin_groups` on the SQL drivers only and leaves a dangling entry in
  memory mode, which is harmless because routing is keyed on the resource-group link and
  the live level, never on management-group membership.
- **The owner-delete consequence.** `vendor_accounts.owner_user_id` stays
  `not null … on delete cascade`, a single owner, so deleting the owning user's row
  hard-deletes every account they own, including a `full`, org-shared one, and revokes
  every borrower, with **no ownership-transfer path** (servers use the many-to-many
  `server_owners` to survive one owner's removal; a credential deliberately has exactly
  one owner). The portal has no user-delete operation today (a user is disabled, not
  deleted), so this is a property of the schema that a direct database delete or a
  future feature would trigger, and the owner's own account delete does the same on
  purpose. An ownership-transfer operation is a named follow-up
  ([Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks)).

## 2. The two auth types

The two types below apply to the bespoke `openai` and `anthropic` vendors. **The
five OpenAI-compatible vendors (x.ai, OpenRouter, Kilo, Google, Custom) are
`api_key` only**, and so is native Gemini (`google_native`): they have no OAuth,
no connect flow and no subscription backend, and creating one with
`auth_type = subscription` is refused with `400 vendor_account.auth_type_invalid`
(§4.4, §4.5).

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

The `subscription` type is also **independently switchable**. The
`vendor_subscriptions_enabled` system setting (default **on**, §7), distinct from the
`vendor_accounts_enabled` module flag, gates this auth type and nothing else: while it
is off a subscription account cannot be created or connected, and an existing one is
kept but neither routed nor listed, whereas an `api_key` account is never affected. An
operator can therefore keep the documented API-key path and shut the experimental,
ToS-restricted one off without turning the whole module off. (It is an
operator-wide switch and has nothing to do with account sharing, which has its own
switches, §7.)

## 3. Connect flows (`internal/vendorauth`)

All vendor OAuth constants — client ids, authorize/token URLs, redirect URIs,
scopes, beta headers, device-code endpoints, token-claim names — live in **one
file**, `internal/vendorauth/constants.go`, every value marked
reverse-engineered and VERIFY-LIVE. The flow functions read them through an
overridable `Endpoints` struct, so a live operator (or a unit test against an
`httptest` server) can point them at a corrected endpoint without touching the
flow code. `vendorauth` may import `capture` (for sealing) and nothing from
`provider`, `portal` or `routing`.

Three connect methods exist. All run the **credential gate** (§8: the owner, or a CM
of a `full` account; a subscription account is shared at most at `shared`, so in
practice the owner) and are gated in `portal.Service` by the
`vendor_accounts_enabled` master flag and, as the last check, the
`vendor_subscriptions_enabled` switch (a refused connect is
`409 vendor_accounts.subscriptions_disabled`, §7), and a successful
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
rather than refreshing by itself. The portal PATCH does not clobber it either: an
operate-only PATCH (`name`, `status`, `model_prefix`) writes only those columns and
`updated_at` through `UpdateVendorAccountOperateFields`, never `oauth_tokens`. Only a
PATCH that carries the api key (the credential gate) still rewrites the whole row from
a copy it loaded earlier and outside both locks, so a subscription account's
`api_key: ""` clear can write the superseded single-use refresh token back — a known,
narrowed, tracked limitation (§10). The
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
(§10). Validation separates them. Four fixed-URL probes in
`internal/vendorauth/validate.go`, plus two that take their URL as an argument
(the OpenAI-compatible vendors and native Gemini, below), each make **one cheap GET that names no
model**, so an authentication verdict can never be mistaken for a wrong-model
error. The fixed URLs live in `constants.go` beside the OAuth constants. The
native Gemini probe (`ValidateGeminiAPIKey`, below) takes its URL as an argument
too, and sends the key in `x-goog-api-key` instead of a Bearer header.

| Credential | Probe | Auth headers | Provenance |
|---|---|---|---|
| OpenAI **subscription** access token | `GET https://chatgpt.com/backend-api/wham/accounts/check` | `Authorization: Bearer` | reverse-engineered, **VERIFY-LIVE** |
| Anthropic **subscription** access token | `GET https://api.anthropic.com/api/oauth/profile` | `Authorization: Bearer`, `anthropic-beta: oauth-2025-04-20`, `anthropic-version` | reverse-engineered, **VERIFY-LIVE** |
| OpenAI **`api_key`** | `GET https://api.openai.com/v1/models` | `Authorization: Bearer` | documented public API |
| Anthropic **`api_key`** | `GET https://api.anthropic.com/v1/models` | `x-api-key`, `anthropic-version` | documented public API |
| x.ai, Google, Custom **`api_key`** | `GET {base}{prefix}/models` (`https://api.x.ai/v1/models`, `https://generativelanguage.googleapis.com/v1beta/openai/models`, the user's root plus `/v1/models`) | `Authorization: Bearer` | preset registry (§4.4); the listing needs a key |
| Google (native) **`api_key`** | `GET {base}/v1beta/models` (`https://generativelanguage.googleapis.com/v1beta/models`) | `x-goog-api-key` | preset registry (§4.4, §4.5); the listing needs a key |
| OpenRouter **`api_key`** | `GET {base}/v1/key` (`https://openrouter.ai/api/v1/key`): its `/v1/models` is public, so a listing would call every key valid | `Authorization: Bearer` | preset registry (§4.4) |
| Kilo **`api_key`** | **none**: its `/gateway/models` is public and it has no key endpoint, so no honest offline check exists and the answer is `unverifiable` ("this provider offers no offline key check") | — | preset registry (§4.4) |

The three OpenAI-compatible rows are one function,
`vendorauth.ValidateOpenAICompatibleAPIKey(probeURL, key)`, and the native Gemini
row is its counterpart `vendorauth.ValidateGeminiAPIKey(probeURL, key)`; in both the
URL is composed by the portal from the account's `base_url` and its preset:
`vendorauth` knows no vendor ids or roots. The rows' provenance is the providers' own API, confirmed live
on 2026-10-10 and still VERIFY-LIVE (§10); the native Gemini row was designed from
Google's documentation and is VERIFY-LIVE in full (§10). As for every api key, they
are probed only by the explicit Test connection, never when the key is saved.

**Classification** is one rule for all of them, the OpenAI-compatible probes
included:

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
- **Consequence for the OpenAI-compatible providers.** x.ai and Gemini (the shim)
  answer a wrong key with **400**, not 401, so a bad key there reads `unverifiable`
  ("could not verify"), never `invalid`. The one rule is deliberately not bent
  per provider: the price is that a wrong key is not caught on those two, the gain
  that a non-401 answer never reads as `invalid`. A moved endpoint that begins
  answering 401 would still show a false `invalid` on Test connection, which
  blocks nothing, since an api key is not probed when it is saved ([ADR-052](../09-architecture-decisions.md#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix) (d)).
- **Native Gemini's wrong-key status is unconfirmed.** Whether the native
  `/v1beta/models` answers a bad key with 400 (`INVALID_ARGUMENT`) like the shim or
  with 401 is VERIFY-LIVE (§10). Under the uniform rule either is safe: a 400 reads
  `unverifiable`, a 401 `invalid`, and nothing else is ever a verdict.

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
request body and changes nothing — it does not even refresh a token. It is an
**operate** call (§8): the owner, or a CM of a `shared` or `full` account, and
**never system scope alone**, because it sends the owner's sealed credential to the
vendor from the gateway: letting an arbitrary caller trigger it would turn that
credential into a live-or-dead oracle, and a CM is the one non-owner the owner has
delegated that to. On a **subscription** account it is **owner-only even at
`shared`**, because it opens the single-use OAuth token set (§8.1). A stranger gets the
same `404 vendor_account.not_found` as for an unknown id, a CM of a subscription
account `403 vendor_account.forbidden`; a disabled module answers `409`. The verdict's
`detail` is scrubbed of the credential for every caller. A missing credential (no API key set, subscription not connected) and an
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
vendor-account candidate source consulted for a resolve carrying a known **principal**,
a user or a service token with a service id: it enumerates the accounts the principal
may route to, its **own** accounts first and then the accounts another user **shared**
into a resource group the principal is provisioned into (the grant, §4.6), keeps only
those the principal's **token** may use (§11), and builds a `Target` directly for the
**first** such active account that has a `vendor_account_models` row whose public name
under the token's effective prefix equals the requested model. Reach is intrinsic —
only accounts the grant lets the principal reach are enumerated — so a personal account
can never serve another principal's request, and a shared one only a principal
provisioned into one of its groups whose token named it. Precedence against
self-hosted routes is configurable (§7).

The match is on the **model name only**: the requested name must equal the
**effective prefix plus the row's `upstream_model`**. For an all-access token or an
opted-in account without an override that prefix is the account's own
`model_prefix` (§6.5), so the match is exactly the stored `gateway_model`, a
prefixed account does not answer to the bare vendor id, and two accounts serving
the same vendor model under different prefixes each route to their own account. A
per-token prefix override changes the prefix, and so the name the token matches
on, without touching the stored rows (§11.2). Whatever the prefix, the resolver
sends the vendor the row's `upstream_model`, the bare id, as the target's
`ProviderModel`. The catalog row's `api_flavor` is stored metadata and is **not**
read by the resolver or the listing overlay. What flavor each account serves is
decided instead by the target the resolver builds, giving this served-flavor
matrix:

| Account | Serves | How |
|---|---|---|
| **OpenAI api-key** | the `openai` **and** `anthropic` dialects | `Target.APIFlavors = [openai, anthropic]`, endpoint modes left zero (**translate**): whichever dialect the caller used is translated through the neutral model to OpenAI's `/v1/chat/completions`. **One exception:** an inbound `openai_responses` request (`POST /v1/responses`) has `ResponsesMode = passthrough` and is relayed verbatim to `https://api.openai.com/v1/responses` instead (§4.2). Chat completions, every other `openai` flavor and the `anthropic` dialect stay translate. |
| **Anthropic api-key** | the `openai` **and** `anthropic` dialects | `Target.APIFlavors = [openai, anthropic]`, endpoint modes left zero (**translate**): whichever dialect the caller used is translated through the neutral model to Anthropic's `/v1/messages`. **One exception, the mirror image of the OpenAI one:** an inbound `anthropic_messages` request (`POST /v1/messages`) has `MessagesMode = passthrough` and is relayed verbatim to `https://api.anthropic.com/v1/messages` instead (§4.1). Chat completions and every `openai` flavor, including `openai_responses`, stay translate. The client's `anthropic-beta` is forwarded verbatim on that relay (§4.1). |
| **Anthropic subscription** | the `openai` **and** `anthropic` dialects | `Target.APIFlavors = [openai, anthropic]`, always with the Claude-Code masquerade (`Masquerade = claude_code`) and the OAuth headers. **An inbound `anthropic_messages` request (`POST /v1/messages`) has `MessagesMode = passthrough`**: it is relayed to `https://api.anthropic.com/v1/messages` with the Claude-Code system block injected as the first `system` block, `Authorization: Bearer <oauth access token>`, and `anthropic-beta` = `oauth-2025-04-20` plus the client's own betas (§4.1). Every `openai_*` flavor stays translate (`MessagesMode` zero): the neutral request is rendered into `/v1/messages` with the masquerade block prepended. |
| **OpenAI subscription** | the `openai` dialect **only** | `Target.APIFlavors = [openai]`. The resolver's flavor guard **skips** an `anthropic`-dialect request to such an account, which then falls through to the standard path and ends `routing.no_model_route`. |
| **OpenAI-compatible api-key** (x.ai, OpenRouter, Kilo, Google, Custom) | the `openai` **and** `anthropic` dialects, **translate only** | `Target.APIFlavors = [openai, anthropic]`, both endpoint modes left zero: whichever dialect the caller used is translated through the neutral model to the provider's `{prefix}/chat/completions` on the account's own root (§4.4). **No exception**: Responses and Messages passthrough exist only for the native OpenAI and Anthropic accounts, so an inbound `openai_responses` or `anthropic_messages` request is translated like any other. |
| **Google (native) api-key** (`google_native`) | the `openai` **and** `anthropic` dialects, **translate only** | `Target.Provider = vendor_gemini`, `APIFlavors = [openai, anthropic]`, both endpoint modes left zero: whichever dialect the caller used is translated through the neutral model to Gemini's `POST {root}/v1beta/models/{model}:generateContent` (`:streamGenerateContent?alt=sse` when streaming), authenticated with `x-goog-api-key` (§4.5). **No exception**, as for the OpenAI-compatible accounts: there is no passthrough yet (the inbound Gemini endpoint is #204). |

The vendor branch is **skipped entirely** — the request falls through to the
self-hosted path — when the module flag is off, the principal has **no identity the
grant can be asked about** (neither a user id nor, for a service token, a service id),
a **server-override** is set, the request is
**capability-gated** (`RequiredCapabilities` non-empty, e.g. vision/image), or the
flavor is **images** (`openai_images`). That the resolver and the listing overlay
agree on what each account serves is what makes the "served-flavors parity"
between dispatch and the model listing (§6.7) meaningful.

One further skip is per account, not per request: an **active `subscription`
account does not serve while `vendor_subscriptions_enabled` is off** (§7). It falls
through to the standard path exactly like a `needs_reconnect` account (a same-named
self-hosted mapping, or `routing.no_model_route`), and the listing overlay applies the
identical skip (§6.7), so the two move together and parity holds in both states. An
`api_key` account is untouched.

A token that has **not** been opted in does not skip the branch: it runs, enumerates
the accounts the principal can reach, and `TokenVendorPrefix` denies every one of them
(§11.2), so it matches nothing and the request falls through to the self-hosted path
all the same.

`Target` carries four vendor fields, all empty/false for an ordinary AI-server
target:

| Field | Meaning |
|---|---|
| `VendorAccountID` | Names the serving account, for usage attribution (§5) and for the dispatch layer to resolve the subscription bearer. Set on **every** vendor target, api-key included. |
| `Subscription` | `true` only on a subscription (OAuth) target. **This**, not `VendorAccountID`, is the subscription-bearer trigger, so an api-key target that also carries a `VendorAccountID` keeps using its sealed `APIToken`. |
| `ExtraHeaders` | A small static header set attached to the upstream request (§4.1/§4.2). |
| `Masquerade` | `""` (none) or `claude_code` (`routing.MasqueradeClaudeCode`), which makes the Anthropic client put the required Claude-Code system block first: prepended to the rendered request on the translate path, injected into the relayed body on the passthrough path (§4.1). |

Four provider kinds select the client at dispatch: `vendor_openai`
(`ProviderVendorOpenAI`, the api-key OpenAI path via the existing
OpenAI-compatible client, which the five OpenAI-compatible vendors reuse as well,
§4.4), `vendor_anthropic` (`ProviderVendorAnthropic`, the
native Anthropic Messages client, used for an Anthropic account whether api-key or
subscription), `vendor_openai_subscription`
(`ProviderVendorOpenAISubscription`, the ChatGPT backend) and `vendor_gemini`
(`ProviderVendorGemini`, the native Gemini `generateContent` client of a
`google_native` account, §4.5). The Gemini kind has two paths. The one built is
**translate**: the neutral request is rendered into `generateContent` and the answer
parsed back. The other, a lossless **passthrough** of Gemini's own wire (the inbound
Gemini endpoint where a Gemini-shaped client such as Antigravity connects), is
deferred to #204: the client already implements `NativeProxyClient` for it, and the
translation package it will reuse is the one the translate path uses.

### 4.1 Anthropic

- **`anthropic-version: 2023-06-01`** on every call. The native client sets it
  itself, on the translate path and on the native relay alike, rather than relying
  on the resolver's target to carry it; a target `ExtraHeaders` value still wins
  (the subscription target repeats it).
- **api_key**: the credential rides the `x-api-key` header.
- **subscription**: `Authorization: Bearer <oauth-access-token>` plus
  `anthropic-beta: oauth-2025-04-20` (on the passthrough, merged with the client's
  own betas), plus the **Claude-Code masquerade** — the request's system content
  is prefixed with the exact block
  `You are Claude Code, Anthropic's official CLI for Claude.`, which the OAuth
  Messages path requires (rendered into the request on the translate path,
  injected into the relayed body on the passthrough path). Endpoint
  `https://api.anthropic.com`.

Anthropic serves only `/v1/messages` (not `/v1/chat/completions`). Two paths reach
it, both through the **native Anthropic Messages client**
(`internal/provider/anthropic_messages.go`):

- **Translate (the default).** A vendor target leaves `MessagesMode` zero, bar the
  `anthropic_messages` case below, so the OpenAI dialect is translated through the
  neutral model into `/v1/messages`: the client renders the neutral `inference`
  request to a `/v1/messages` body and parses the response/SSE back. It renders and
  parses itself rather than importing `internal/compat` (an architecture-test
  boundary). This serves chat completions and every `openai` flavor to any
  Anthropic account, api-key or subscription (a subscription's rendered request
  carries the masquerade block). Prompt caching on this path is opt-in and gated
  by its own system setting (*Translate prompt caching*, below).
- **Native passthrough, api-key and subscription.** The client also implements the
  native-proxy interface (`ProxyNative`), and the resolver selects it for exactly
  one case: an inbound `anthropic_messages` request to an Anthropic account,
  **api-key** (*Api-key Messages passthrough*, below) or **subscription**
  (*Anthropic subscription Messages passthrough*, below). The vendor's
  passthroughs are therefore four: the OpenAI subscription's and the OpenAI
  api-key account's `/v1/responses` (§4.2), and the Anthropic api-key and
  subscription accounts' `/v1/messages`.

#### Api-key Messages passthrough

An inbound **Anthropic Messages** request (`POST /v1/messages`, also served as
`/anthropic/v1/messages`; the **fine** `anthropic_messages` flavor) to an Anthropic
**api-key** account is relayed **verbatim** to the platform's
`https://api.anthropic.com/v1/messages` rather than translated. The translate path
parses the request into the provider-neutral model and renders it back, and that
model carries only what the chat-style dialects share, so everything else is lost
on the way: the structured `system` array collapses to a single string and its
`cache_control` markers are dropped (so prompt caching never engages), the
request's `thinking` setting, `metadata` and `top_p`/`top_k` are never read,
tools are reduced to name, description and input schema (a typed server tool loses
its `type`), `document` blocks and images inside a tool result are skipped, and a
replayed `thinking` block is reduced to its text without its signature. A Claude
Code or SDK client that sends those fields would silently get a different request
upstream. The resolver sets `MessagesMode = passthrough` for exactly this one case
(`vendorAccountTarget`, keyed on the vendor being Anthropic and the fine flavor
being `anthropic_messages`, as the OpenAI target is keyed on the Responses flavor),
and everything downstream is the existing native-passthrough layer: `tryProxyNative`
reading the target's mode through `endpointModeFor`, the Anthropic client's
`ProxyNative`, the account's opened sealed key sent as `x-api-key`
(`upstreamAuthCtx`), and the usual usage attribution to the serving account (§5).

- **Only `model` is rewritten.** It is set to the account's bare upstream id, which
  equals the requested name when the account has no prefix. `system`, `tools`,
  `thinking`, `cache_control`, `metadata` and everything else reach Anthropic as the
  client wrote it. The relay is value-lossless rather than byte-identical when a
  rewrite happens (key order may change). The response and its SSE are relayed
  unchanged and are not rewritten back (§6.5).
- **The headers are the gateway's, bar one.** Besides `Content-Type`, the
  upstream request carries `x-api-key` (the opened credential) and
  `anthropic-version: 2023-06-01`, which `ProxyNative` guarantees itself, so a
  target that forgot it cannot make Anthropic answer 400. There is **no**
  `Authorization` bearer and **no** Claude-Code masquerade block, both being
  subscription-only, and the resolver's target sets no `ExtraHeaders` for this
  path. The one value taken from the inbound request is the client's
  `anthropic-beta` (*Client `anthropic-beta` on the Messages passthrough*, below);
  an api-key target has no beta of its own, so that is the whole header, and a
  request without one sends none. No other inbound header is forwarded, the
  client's own `anthropic-version` included. Redirects from the upstream are not
  followed, because net/http strips `Authorization` but not `x-api-key` when a
  redirect leaves the host.
- **Verbatim in body and in beta opt-in, pinned in version.** A beta-gated body
  feature (for example a `context_management` field) reaches Anthropic together
  with the opt-in the client sent for it. The one header the relay does not take
  from the client is `anthropic-version`, which stays the gateway's pin.
- **Unchanged:** chat completions and every `openai` flavor to an Anthropic api-key
  account (still translated to `/v1/messages`; an inbound `openai_responses` request
  there has no Responses surface to pass through to), an `anthropic_messages` request
  to an **OpenAI** account (still translated to `/v1/chat/completions`) and the
  `count_tokens` utility route (a local estimate that never reaches an upstream).
- **Subscription account.** An Anthropic *subscription* account takes the same
  passthrough for an inbound Messages request, with the one body edit this api-key
  relay never makes: the Claude-Code system block (*Anthropic subscription
  Messages passthrough*, below).
- **Behaviour change.** A request the translate path tolerated or reshaped now
  meets Anthropic's own validation, because Anthropic receives the body as the
  client wrote it. The relay was exercised against a stub upstream, not a live
  Anthropic key.
- **Create-only.** Only `POST /v1/messages` (and its `/anthropic/v1/messages`
  alias) is relayed to the vendor. The gateway registers no other Anthropic route
  that relays to one, so a client cannot call, say, the Message Batches API through
  an account.

#### Client `anthropic-beta` on the Messages passthrough

On **either** Anthropic passthrough, api-key or subscription, the client's
`anthropic-beta` header reaches Anthropic instead of being dropped. A Claude Code
or SDK client uses it to opt into beta features (interleaved thinking, a larger
context window, `context_management`, ...), and a beta-gated field that arrives
without its opt-in is rejected or ignored upstream. `proxyNative` builds the
upstream call's context through `upstreamAuthCtxWithHeaders`, fed by
`anthropicPassthroughHeaderOverrides`:

- **Gate.** Only a target whose provider is the Anthropic vendor client **and**
  whose `MessagesMode` is `passthrough`, and only when the client's header carries
  at least one non-blank token. An OpenAI or self-hosted target, and the translate
  path (a translated request is the gateway's own rendering, which has no use for
  the client's beta), forward nothing, and a request without a client beta goes
  out exactly as it would have before.
- **Merge.** The value sent is the target's own static `anthropic-beta` (none for
  an api-key target, `oauth-2025-04-20` for a subscription one) followed by the
  client's tokens. Each header value (a client may repeat the header) is split on
  commas, each token is trimmed, empty tokens are dropped, and a token is kept
  only the first time it appears (an exact, case-sensitive match); the result is
  comma-joined with no spaces. A subscription's OAuth opt-in is therefore always
  present, and present once, even when the client names it too.
- **Per-request copy.** The merged value is overlaid on a per-request copy of the
  target's `ExtraHeaders` (a differently-cased spelling of the header is replaced,
  not sent beside it). The shared resolved `Target` is never mutated, and the
  api-key and subscription credential paths (§4.3) are used unchanged.
- **Verbatim, not allow-listed.** Issue #188 proposed forwarding an *allow-listed*
  `anthropic-beta`; the implementation forwards the client's tokens **verbatim**
  instead (an open list). That matches the other lossless passthroughs (the
  gateway does not curate what a client's body may carry) and Anthropic's own
  guidance for a gateway in front of Claude Code, which is to pass the client's
  `anthropic-beta` through. An allow-list would also go stale silently: a beta the
  gateway has not heard of would be dropped without a trace, the very loss the
  passthrough exists to remove, and keeping the list current would be upkeep for
  no protection the vendor does not already apply.
- **Deliberate consequence: any client of the account can opt it into any beta.**
  Because the tokens are forwarded verbatim, a gateway client (anything holding a
  token that routes to the connected account) can request **any** Anthropic beta
  for it, **including billing-affecting ones**: a beta that changes how a request
  is billed, or that the account's plan does not include, is sent exactly as the
  client names it, and whether it is accepted is Anthropic's to decide, its answer
  being relayed as-is. This is the intended behavior of an open list, not a
  defect. The exposure is bounded by who can reach the account: while it is personal
  (§1) the clients are its owner's own tokens and portal chat, and once it is shared
  (§1.1) they also include every borrowing token that named it (§11), so a share is a
  grant to spend the owner's account on any beta the vendor will accept.
- **`anthropic-version` stays pinned.** The client's own `anthropic-version` is
  **not** forwarded; `ProxyNative` sends the gateway's `2023-06-01` (the
  subscription target repeats the same value in its `ExtraHeaders`). The version
  is the one the body format is tied to, whereas a feature opt-in is
  `anthropic-beta`'s job.

#### Anthropic subscription Messages passthrough

An inbound **Anthropic Messages** request (`POST /v1/messages`, also served as
`/anthropic/v1/messages`; the **fine** `anthropic_messages` flavor) to an Anthropic
**subscription** account is relayed to `https://api.anthropic.com/v1/messages`
rather than translated, for the same reason as the api-key relay above: the
translate path parses the request into the neutral model and renders it back, and
loses everything that model cannot carry (the structured `system` array and its
`cache_control`, `thinking`, `metadata`, server tools, ...). The resolver sets
`MessagesMode = passthrough` for exactly this one case
(`vendorSubscriptionAnthropicTarget`, keyed on the **fine** request flavor, since
the coarse `apiFlavor` folds every `openai_*` flavor together, exactly as
`vendorSubscriptionOpenAITarget` is keyed). Every `openai_*` flavor to the same
account is still translated. The target keeps everything a subscription target
carries (`Subscription`, `Masquerade = claude_code`, the `anthropic-version` and
`anthropic-beta: oauth-2025-04-20` `ExtraHeaders`), and the rest is the existing
native-passthrough layer: `tryProxyNative` reading `MessagesMode` through
`endpointModeFor`, `subscriptionAuthCtx` resolving the OAuth bearer (§4.3), the
Anthropic client's `ProxyNative`, and the usual usage attribution to the serving
account (§5).

- **What reaches Anthropic.** `Authorization: Bearer <oauth access token>` (no
  `x-api-key`), `anthropic-version: 2023-06-01` once, `Content-Type`, and
  `anthropic-beta` = `oauth-2025-04-20` followed by the client's own tokens,
  deduplicated (*Client `anthropic-beta`*, above). No other inbound header is
  forwarded. The response and its SSE are relayed unchanged and are not rewritten
  back (§6.5); redirects are not followed, as for the api-key relay.
- **The body is verbatim except two edits.** The gateway layer rewrites `model` to
  the account's bare upstream id (equal to the requested name when the account has
  no prefix). `AnthropicClient.ProxyNative` then **injects the Claude-Code block**
  the OAuth Messages path requires: the exact
  `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}`
  becomes the **first** `system` block (the same block the translate path renders,
  so the two paths cannot drift). It happens only when the target's `Masquerade`
  is `claude_code`; an api-key target's body is never touched, byte for byte. How
  the client's `system` is handled:

  | Client `system` | Sent upstream |
  |---|---|
  | absent, `null` or `""` | `[block]` |
  | a non-empty string `s` | `[block, {"type":"text","text":s}]` |
  | an array not starting with the block (including `[]`) | the block prepended; every existing block kept as written, `cache_control` included |
  | an array whose first element is already a `text` block with exactly the Claude-Code text (any other field on it, such as `cache_control`, is ignored) | **unchanged**: no block is added, so the injection is idempotent and a real Claude Code client is not doubled |
  | anything else (the body is not a single JSON object, or `system` is an object, number or bool) | **unchanged** (fail-open): Anthropic's own 400 is the answer, not a gateway-made one |

- **Value-lossless when edited.** An edited body is decoded with `UseNumber` and
  re-marshalled, so every value (a large integer, a float's exact spelling such
  as `1.50`, every client field, `cache_control` and all) arrives as the client
  wrote it. Only object key order (nested objects included) and insignificant
  whitespace and escaping may change, and a body that needs no injection (the
  idempotent and fail-open rows above) keeps the exact bytes the gateway layer
  handed to the provider client, which are the client's own unless `model` needed
  its rewrite. The payload capture records the body as the client sent it, before
  either edit.
- **Behaviour change.** A request the translate path tolerated or reshaped now
  meets Anthropic's own validation, because Anthropic receives the body as the
  client wrote it. The relay was exercised end to end against a stub upstream
  (bearer, merged beta, block first with the client's `system` and `cache_control`
  intact, body otherwise verbatim, SSE byte for byte, usage attributed to the
  account), not against a live Anthropic OAuth session.

**Scope decision: no CLI-fingerprint headers.** The passthrough sends **none** of
the headers a real Claude Code client sends to identify itself: no `x-app: cli`,
no `claude-cli` `User-Agent`, no `X-Stainless-*` SDK headers and no outbound
`X-Claude-Code-Session-Id`, and the client's own copies of them are not forwarded
(only `anthropic-beta` is taken from the inbound request). It therefore keeps
exactly the **same impersonation depth as the existing translate masquerade**
(the system block, the OAuth beta and the bearer), just lossless. Whether
Anthropic's OAuth path checks anything beyond that is unknown, and a deeper
fingerprint set is a **VERIFY-LIVE** follow-up that would sit behind its own flag,
not something this passthrough does by default.

**VERIFY-LIVE.** The subscription OAuth path is reverse-engineered and has not been
confirmed against a live account (§10): that Anthropic accepts the injected block
together with the merged `anthropic-beta` is assumed, on the same footing as the
translate path it replaces. The idempotency assumes that a real Claude Code client
sends the exact Claude-Code block as `system[0]`. If a client version puts some
other block ahead of its own Claude-Code block, the gateway cannot tell and
**prepends a second copy** of the block, so Anthropic would see the Claude-Code
line twice; whether it objects is likewise unverified.

#### Translate prompt caching

Anthropic caches a prompt prefix only where the request marks it with a
`cache_control` breakpoint. The **native passthrough** relays the client's own
`cache_control` untouched (above), so a client that already marks its prompts, such
as Claude Code, caches without any help from the gateway. The **translate** path
renders the request itself from the neutral model and used to emit no breakpoint at
all, so every translated call (an OpenAI-dialect client, the portal chat) paid full
price for the same system prompt, tool list and earlier turns on every round. The
translate path can now place the breakpoints itself, behind the
**`anthropic_prompt_caching_enabled`** system setting: a bool, **off by default**,
**experimental**, edited as the "Anthropic prompt caching" toggle (de: "Anthropic
Prompt-Caching") on the System settings page. The decision and its rejected
alternatives are [ADR-051](../09-architecture-decisions.md#adr-051--anthropic-translate-prompt-caching-is-flag-gated-decided-at-the-gateway-by-a-hybrid-auto-switch-and-placed-by-the-provider).

**The decision is made at the gateway, the placement at the provider.**
`Server.applyAnthropicCachePolicy` (`internal/gateway/anthropic_cache_policy.go`) is
called from both dispatch hooks, the non-streaming `complete` and the streaming
`beginStream`, after the target's `ProviderModel` is applied. It writes the verdict
onto the **upstream** copy of the request only (the client-facing request that is
echoed back and recorded in usage is never touched) as
`inference.Request.PromptCache`, a `*PromptCacheDirective{Enabled, TTL}` tagged
`json:"-"`: internal, never parsed from or serialized to a client body. The Anthropic
client's `anthropicRequestBody` only **honors** the directive and holds no policy of
its own. A request with no directive (or `Enabled=false`) renders byte-identically
to one built before this feature.

**The hybrid auto-switch.** The directive is set only when **all four** hold, so
caching turns on exactly where the prefix is both big enough to be cached and likely
to be re-sent:

| # | Condition | Why |
|---|---|---|
| 1 | The target is an **Anthropic translate** target: `routing.ProviderVendorAnthropic`, which covers **both** the api-key and the subscription account (the subscription differs only in `Subscription`/`Masquerade`, which the builder already handles). | Only this path renders the body from the neutral request, so only it can carry a directive. A native `/v1/messages` passthrough never reaches the dispatch hooks. |
| 2 | The conversation is **likely to be reused**: it already carries an **assistant turn** (at least the second round of an exchange), **or** it is a **portal-chat session** (`SessionSource == "chat"`, whose next message re-sends this one). | A cache write is billed at a premium over plain input, so a prefix nothing re-reads costs more cached than not. A large **one-shot** is therefore never cached. |
| 3 | The **`anthropic_prompt_caching_enabled`** flag is on. | The master switch. |
| 4 | The **estimated prefix** reaches the model's cacheable minimum. | Below it Anthropic ignores `cache_control` for free, so marking such a request gains nothing. |

The cheap structural checks (1, 2) run first, then the flag, and the O(prompt)
estimate last, so a non-Anthropic or one-shot request pays almost nothing.

- **The flag read is cached at the gateway.** The system-settings store read is an
  uncached full-table `SELECT` and the policy runs on every Anthropic translate
  dispatch, so it goes through `anthropicPromptCachingEnabledCached`: a **5 s TTL**
  cache (`anthropicPromptCachingFlagTTL`) that `handleSystemSettings`
  **invalidates on a PUT that carries the key**, so a portal toggle applies on the
  next request and the TTL only bounds an out-of-band change (a direct database
  edit). The same trade-off, and the same value, as the vendor flags' cache (§7). A
  gateway with no portal, or an unreadable setting, reports **off**: the feature is
  opt-in, so it fails closed. (`portal.Service.AnthropicPromptCachingEnabled` itself
  reads the store on every call; the hot path does not use it directly.)
- **The model minimum** (`anthropicCacheMinTokens`, matched on the upstream model
  id): **4096** tokens for the Opus 4.6 / Opus 4.5 / Haiku 4.5 families, **2048** for
  Opus 4.7, **512** for the Opus 5 / Sonnet 5.5 / Haiku 5.5 / Fable 5 families, and
  **1024** for everything else, including an **unknown** model (the conservative
  default).
- **The estimate** (`estPrefixTokens`) is a cheap `chars / 4` over the message text
  parts, the tool-call names and arguments, and the tool names and descriptions. It
  deliberately does **not** measure tool parameter schemas (that would need a
  marshal per request), so it errs **low, never high**; a low estimate merely skips
  the markers, which is today's behavior.

**Placement** (`anthropicRequestBody`, `internal/provider/anthropic_messages.go`).
With an enabled directive the builder places **at most two** breakpoints:

1. **The last system block.** `system` is rendered as a **block array**
   (`anthropicSystemFieldCached`) with `cache_control` on its last block. On the
   plain path the joined system/developer text becomes one text block; for the
   subscription masquerade the array already exists, its first block (the
   Claude-Code line) is left untouched and the **last** block carries the marker
   (when there is no caller system text, that single Claude-Code block is the last
   block and carries the marker; its text is unchanged).
   Anthropic's cache covers `tools`, then `system`, then `messages`, in that order,
   so this one marker caches **the tool list and the system prompt**, the stable
   prefix. A request with no system text and no masquerade has no system block, and
   only the turn marker is placed.
2. **The last content block of the latest turn** (the final message after
   consecutive same-role turns are merged). It caches the whole conversation up to
   it, so the next round, which re-sends that conversation and appends a turn, reads
   everything before its new turn from the cache. The marker moves forward each
   round.

The marker is `{"type":"ephemeral"}`: the **5-minute** TTL **only** in v1. The
directive's `TTL` field exists and is reserved for `"1h"`, but nothing emits a
`ttl` today; the 1-hour TTL needs Anthropic's extended-cache-TTL beta header on the
translate request and is a deferred follow-up (ADR-051).

**Prefix stability is a correctness requirement.** A cache read lands only when the
prefix is **byte-identical** from one round to the next: any change ahead of a
breakpoint (the system text, the tool order or a tool's schema, an earlier turn)
invalidates everything after it, and the request then pays the write premium again
for nothing. The render is therefore deterministic (struct-ordered fields, tool
schemas marshalled from maps in sorted key order, no per-request value such as a
timestamp or id ahead of a marker), and
`TestAnthropicCachedPrefixIsStableAcrossTurns` pins it: the same request renders to
the same bytes repeatedly, and the `system` and `tools` fields and the leading
messages of a longer follow-up are identical to the earlier round's, for both the
api-key and the masquerade target. One known limit: Anthropic looks back a bounded
number of content blocks from a breakpoint for an earlier cache entry, so a **single
turn that adds a long run of blocks** can fall outside that window and miss the
previous write; extra intermediate breakpoints for that case are a deferred
follow-up.

**Observability, and no Activity change.** The Anthropic client already parses
`cache_read_input_tokens` and `cache_creation_input_tokens` from the response usage,
and the gateway stores them as `usage_events.cached_tokens` (read) and
`cache_write_tokens` (write), disjoint from the fresh `input_tokens`
([Telemetry](telemetry-usage-observability.md)). Turning the flag on therefore
needs no display work: the **cache-read** stat tile and the grouped-view column are
already visible by default and now simply populate, while the **cache-write** tile
and column stay hidden by default (a manual drill-down in the column and tile
pickers; the per-request table's cache columns are likewise opt-in there).

**Scope.** Translate only. The native passthrough is untouched, as above. The
setting has no effect on an OpenAI account or on a self-hosted route, and because an
Anthropic target exists only through a vendor account, it matters only in a
deployment that has the vendor-accounts module on (§7).

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
the OAuth bearer, attaches it plus the target's static `ExtraHeaders` (on an
Anthropic Messages passthrough, with the client's `anthropic-beta` merged into
them per request, §4.1), and — for OpenAI — the per-account `chatgpt-account-id`. The static headers are attached
even when no bearer is available, so an auth failure reads as an auth error
upstream rather than a missing API version. The api-key path is unchanged: its
sealed `APIToken` rides the vendor's own API-key header. The custom `x-api-key`
header (Anthropic) and `x-goog-api-key` (native Gemini, §4.5) are redacted in payload
capture: the provider's capture sink records the **outbound** request headers, so an
unlisted credential header would be stored in clear (a latent leak the feature
closed).

### 4.4 OpenAI-compatible vendors

Five vendor ids reach hosted providers that speak the OpenAI chat dialect with a
Bearer key: `xai` (x.ai, Grok), `openrouter`, `kilo` (the Kilo Gateway), `google`
(Gemini through Google's OpenAI-compatible shim) and `openai_compatible` (Custom:
any OpenAI-compatible endpoint the user addresses). They are ordinary per-user
vendor accounts, so ownership, the per-token opt-in (§11), the model prefix (§6.5),
the master flag (§7) and the authorization by share level (§8) apply unchanged, but they are
**presets over the one OpenAI client** rather than clients of their own
([ADR-052](../09-architecture-decisions.md#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)).

**The preset registry.** `internal/routing/vendor_presets.go` is a pure-data table
keyed by vendor id (`VendorPresetFor`, `IsPresetVendor`, `IsOpenAICompatibleVendor`,
`IsNativeGeminiVendor`, `OpenAIPathPrefixFor`). It lives in `routing` because the resolver needs the path
prefix and routing cannot import the portal; the portal reads the rest to compose
URLs, and `vendorauth` never sees a preset: it receives plain URL strings. A preset
carries its default root, its path prefix, how its key is validated, how a
discovered model id is rewritten, and its usage strategy (`UsageVia` with the
root-relative `UsagePath`, below; §5.2), and its **`Dialect`**: `""` for the
OpenAI wire served by the shared OpenAI-compatible client, `"gemini"` for the native
`generateContent` client (§4.5). The values (live-confirmed 2026-10-10,
VERIFY-LIVE, §10; the usage endpoints, checked on 2026-10-11, are
docs-derived or undocumented, §5.2):

| Vendor id | Default root (stored as `base_url`) | Path prefix | Key probe (§3.5) | Discovered id | Usage (`UsageVia` + `UsagePath`, §5.2) |
|---|---|---|---|---|---|
| `xai` | `https://api.x.ai` | `/v1` | `GET {base}/v1/models` | verbatim | none |
| `openrouter` | `https://openrouter.ai/api` | `/v1` | `GET {base}/v1/key` | verbatim | `openrouter_key`, `GET {base}/v1/key` |
| `kilo` | `https://api.kilo.ai/api` | `/gateway` | none (`unverifiable`) | verbatim | `kilo_balance`, `GET {base}/profile/balance` (best effort) |
| `google` | `https://generativelanguage.googleapis.com` | `/v1beta/openai` | `GET {base}/v1beta/openai/models` | a leading `models/` is stripped | none |
| `google_native` (`Dialect` `gemini`; §4.5) | `https://generativelanguage.googleapis.com` | `/v1beta` (discovery and the key probe only: the client owns its request paths) | `GET {base}/v1beta/models` with `x-goog-api-key` | a leading `models/` is stripped; only `generateContent` models are kept | none |
| `openai_compatible` | none: the user supplies it | `/v1` | `GET {base}/v1/models` | verbatim | none |

**One registry, three predicates.** The registry holds both preset families, so its
membership test is split by what a caller means. `IsPresetVendor` is true for every
preset account and carries the **account** semantics shared by both families: api-key
only, a configurable immutable root, fail-closed on an empty root, a best-effort
discovery at create and an empty static seed. `IsOpenAICompatibleVendor` is
`IsPresetVendor` with an empty `Dialect` and selects the OpenAI-wire resolver branch
and the OpenAI-compatible discovery and key probe. `IsNativeGeminiVendor`
(`Dialect == "gemini"`) is tested **first** in the resolver, discovery and
validation, so a native account never takes an OpenAI-wire branch. The two Google
presets share a default root but are separate accounts (§4.5).

So x.ai serves chat at `https://api.x.ai/v1/chat/completions`, OpenRouter at
`https://openrouter.ai/api/v1/chat/completions`, Kilo at
`https://api.kilo.ai/api/gateway/chat/completions` and Gemini at
`https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`.

**The usage strategy.** `UsageVia` names how the stored api key can be used to ask
the provider for usage or credits (`openrouter_key`, `kilo_balance`; empty = the
provider offers no such query with an inference key: x.ai keeps its balance behind
a separate Management API with its own key, Gemini has no usage API at all, and
Custom has no standard endpoint). `UsagePath` is the **root-relative** suffix of
that request, composed as `TrimRight(base_url, "/") + UsagePath` by the portal, so
`vendorauth` receives a finished URL and stays free of any vendor root. It is
root-relative, and not relative to the path prefix, because Kilo's balance sits
under the host root and **outside** its `/gateway` prefix, while OpenRouter's
`/v1/key` is also expressible from the root and is the very URL its Test
connection probe asks. The registry is what says which api-key accounts have an
active usage pull (§5.2): the gate reads `UsageVia != ""`, it keeps no vendor
list of its own.

**The stored root.** `base_url` is the root without the prefix. At create the
service defaults it from the preset when it is empty (`openai_compatible` has no
default, so empty is `400 vendor_account.base_url_required`) and otherwise accepts
only an **https** URL with a host and no userinfo, query, fragment or space
(`400 vendor_account.base_url_invalid`); a trailing `/` is trimmed. So the stored
root is either the preset's default or an **owner-supplied** one: required for
Custom, an optional override for every named preset vendor (x.ai, OpenRouter, Kilo,
Google and `google_native`). Beyond the shape the host
is unrestricted, which is the owner-supplied base-URL trust model of
[Risks §11.4](../11-risks-and-technical-debt.md#114-deliberate-design-acceptances).
It is immutable: a different root means a new account. For `openai` and `anthropic`
a supplied `base_url` is ignored and the column stays `''`.

**Serving.** `vendorAccountTarget` has a branch for these vendors that builds a
`Target` with `Provider = vendor_openai` (the existing OpenAI-compatible client, so
no dispatch wiring changes), Bearer auth from the sealed key (`APITokenHeader`
empty), `Endpoint` = the stored root and `OpenAIPathPrefix` = the registry's prefix.
`Target.OpenAIPathPrefix` is the one new `Target` field: the client composes
`{Endpoint}{prefix}/chat/completions` and `{prefix}/models`, and the gateway's
usage-label path reads the same normalised value
(`Target.OpenAIPathPrefixOrDefault`, the single place the `/v1` default lives).
**Empty means `/v1`**, so every pre-existing caller (self-hosted AI servers, probe
targets, the OpenAI vendor target) is byte-identical to before. The served flavors
are `[openai, anthropic]` with zero endpoint modes, **translate only**: no
Responses or Messages passthrough, which are native to OpenAI and Anthropic. An
account with no stored root cannot arise through the API, but if one is read it
fails closed (no target; the request falls through) rather than routing to a URL
with no host.

**Credential check and models.** Test connection is per preset (§3.5) and model
discovery is `GET {base}{prefix}/models` (§6.1), both with the uniform fail-soft
classification. A new account has an empty catalog until discovery fills it
(§6.2).

**Deliberately out of scope.** Usage and credits are reported only for OpenRouter
(its key's limit, per key) and, best effort, Kilo (a balance); x.ai, Google, Google (native) and
Custom report none (§5.2), and the passive header scrape finds nothing for any of
them (§5.1); Gemini-only features do not exist on the shim, which carries only what
OpenAI's chat completions do (the native dialect, §4.5, is the separate
`google_native` account); and the subscription/OAuth path does not exist for them
(§2).

### 4.5 Native Google Gemini

The vendor id `google_native` reaches Gemini through Google's own `generateContent`
wire rather than the OpenAI shim of §4.4
([ADR-054](../09-architecture-decisions.md#adr-054--native-google-gemini-dialect-its-own-vendor-id-and-provider-kind-selected-by-the-preset-dialect-field-over-a-leaf-translation-package)).
It is an ordinary per-user vendor account (ownership, the per-token opt-in §11, the
model prefix §6.5, the master flag §7, authorization by share level §8 all apply) and a preset
of the same registry, so it is api-key only and carries an immutable stored root
(default `https://generativelanguage.googleapis.com`, https only, an optional owner
override under the same trust model as §4.4). It differs from the shim `google`
account in three ways: its `Dialect` is `gemini`, its provider kind is
`vendor_gemini`, and its client is a native one.

**Selection, with no migration.** The two Google accounts are told apart by vendor
id alone. `vendor_accounts.vendor` is free text with no check constraint, so adding
`google_native` changes no schema: an owner may hold a shim and a native account at
once, and rolling back is deleting the native account. Both register the same model
names when they discover, so the name-collision rule of §11.3 applies (the first
account by id wins at resolve time), and the advice is a distinct `model_prefix` on
one of them (§6.5).

**Serving.** `vendorAccountTarget` builds the target in an `IsNativeGeminiVendor`
branch that comes **before** the generic OpenAI-compatible one: `Provider =
vendor_gemini`, `Endpoint` = the stored root, `APITokenHeader = x-goog-api-key` and no
`OpenAIPathPrefix` (the client composes its own paths, and `OpenAIPathPrefixFor`
answers empty for a native preset). The served flavors are `[openai, anthropic]` with
both endpoint modes zero, **translate only** (the matrix in §4). A stored account
with no root fails closed through `IsPresetVendor`, like an OpenAI-compatible one.

**The client** (`internal/provider/gemini.go`, `GeminiClient`) implements `Client`,
`StreamingClient` and `NativeProxyClient`; like the Anthropic client it implements no
`ModelLister` or `Prober`, because vendor discovery lives in the portal (§6).

- **Paths.** `POST {root}/v1beta/models/{model}:generateContent`, and
  `:streamGenerateContent?alt=sse` for a stream. The model is taken from the
  target's `ProviderModel` (a leading `models/`, the listing's form, is stripped) and
  path-escaped; the request body carries no model and no stream flag.
- **Auth.** The sealed key is sent only as the `x-goog-api-key` header, never as the
  `?key=` query parameter (which would put it in URLs and logs).
- **No redirects.** On both the translate and the passthrough path the client does not
  follow a redirect: `net/http` drops `Authorization` but not a custom credential header
  when a redirect leaves the host, and the root is owner-supplied. A 3xx is returned as
  the upstream's answer.
- **Timeouts and errors.** `Complete` applies the target timeout; `CompleteStream`
  applies none (the gateway's idle watchdog governs a stalled stream, as for every
  client). A context deadline is `provider.timeout`, a transport failure
  `provider.unavailable`, and a non-2xx status the usual status mapping **with a bounded
  snippet of the upstream body**, so a 400 `INVALID_ARGUMENT` explains itself.
- **Streaming.** Gemini's SSE carries only `data:` frames (no `[DONE]`), each a whole
  response chunk. The stream is complete only when a chunk reports a finish reason;
  the last (cumulative) `usageMetadata` is the answer's usage; whole function calls are
  buffered and emitted before the terminal event; and a mid-stream `{"error":...}` frame
  or an end of stream with no finish reason is `provider.unavailable`, so a cut-off
  stream is never reported as a completed one.
- **Capture.** `x-goog-api-key` is in the capture-redaction allowlist (§4.3), and the
  usage-row path label of a `vendor_gemini` target is the fixed string
  `/v1beta/models/*:generateContent`, because the label function takes no model.

**Translation** is `internal/geminiwire`, a leaf package that imports only
`internal/inference` (pinned by the architecture tests,
[Architecture Tests](architecture-tests.md)). It holds the Gemini wire structs and the
converters, outbound (`RenderRequest`, `ParseResponse`, `ParseStreamChunk`) now and
inbound (`ParseRequest`, `RenderResponse`, `RenderStreamChunk`) reserved for #204, so
both directions share one package. The rules, at the neutral model's expressiveness:

| Neutral | Gemini |
|---|---|
| `system` / `developer` messages | joined with a blank line into `systemInstruction` (text only) |
| `user` / `assistant` turns | `user` / `model` contents; consecutive same-role turns merge, empty ones drop |
| `tool` message | a `user` turn of `functionResponse` parts (parallel results merge); a non-object result is wrapped as `{"result": text}` |
| image data URI / http(s) URL | `inlineData` / `fileData` (an external URL is VERIFY-LIVE); other media is dropped |
| `Tool` | `functionDeclarations[].parametersJsonSchema` (a bare object schema when there are none) |
| `tool_choice` | `functionCallingConfig`: auto is `AUTO`, none `NONE`, required `ANY`, a named function `ANY` with `allowedFunctionNames` |
| `temperature`, `max_tokens`, `stop` | `generationConfig` `temperature`, `maxOutputTokens`, `stopSequences` |
| `reasoning_effort` | `thinkingConfig` with thoughts included: a `thinkingBudget` for the 2.5 family (low 1024, medium 8192, high 24576), a `thinkingLevel` for `gemini-3`, the family sniffed from the model name |
| response `thought` parts / text parts / `functionCall` parts | reasoning / text / tool calls (arguments as compact JSON, `{}` when absent) |
| `finishReason` | `MAX_TOKENS` is `length`; safety-class reasons (`SAFETY`, `RECITATION`, `BLOCKLIST`, `PROHIBITED_CONTENT`, `SPII`, `LANGUAGE`, `IMAGE_*`) are `content_filter`; anything else is `stop`, or `tool_calls` when a call is present |
| a prompt-blocked reply (HTTP 200, no candidates, a `promptFeedback.blockReason`) | an empty answer with `content_filter`; the parser never indexes `candidates[0]` unconditionally |
| `usageMetadata` | input = `promptTokenCount` (includes the cache), cached = `cachedContentTokenCount`, output = candidates plus `thoughtsTokenCount`, total as reported |

**Function name and `thoughtSignature` via the tool-call id.** The neutral model has no
slot for either, yet Gemini keys a `functionResponse` by function **name** and, on
Gemini 3, rejects a function-calling turn whose `thoughtSignature` is not echoed back.
Every dialect echoes a tool-call id verbatim, so the parser puts both into
`ToolCall.ID` as `gcw1:` plus the base64url of a small JSON object (`n` the name, `s` the
signature, `x` a per-response call index — omitted when 0 — that keeps parallel same-name
calls distinct for a strict downstream client; it is ignored on decode and never sent to
Gemini), and the renderer decodes it on the next turn to restore the model turn's
`thoughtSignature` and the `functionResponse` name. A malformed or `null` payload does
not decode. An id minted elsewhere (history from another provider) falls back to the
name of an **earlier** assistant call with that id and carries no signature (what
Gemini 3 does with a signature-less history is VERIFY-LIVE, §10). Only a function-call
part's signature is carried.

**Discovery and validation** are the portal's, through `vendorauth` (§6.1, §3.5):
`DiscoverGeminiModels` pages `GET {base}/v1beta/models` and `ValidateGeminiAPIKey`
probes the same URL, both with `x-goog-api-key`. The preset has no usage strategy:
Gemini has no usage API, so a `google_native` account reports no usage (§5.2).

**Scope.** Translate-only. Lossless passthrough of Gemini's own wire, the inbound
Gemini endpoint where a Gemini-shaped client connects, is #204 and reuses
`geminiwire` and the client's `ProxyNative`. Broad translate fidelity, meaning the
features the neutral model cannot carry (structured output, safety settings, non-image
media, fine thinking control), is #208.

### 4.6 Routing to a shared account: the pure grant

A shared account reaches other principals through **one grant**,
`VendorAccountsForRouting(principal)`
(`internal/portal/service_vendor_sharing_grant.go`), which the resolver, the model
listings and the save-time check of a token's opt-in all use, and whose shared arm the
member read and the list's member rows reuse (`isMemberOfSharedVendorAccount` is its
single-account form), so what is advertised is what is routable. `internal/routing` cannot import `portal`, so the
resolver reaches it through the `VendorAccountEnumerator` seam
(`Resolver.SetVendorAccountEnumerator`), the way it reaches `ProvisioningGate`, with
the **opposite** nil meaning (below).

**Two enumerators, two purposes.**

| Enumerator | Set | Used for |
|---|---|---|
| `VendorAccountsForRouting` (the routing enumerator) | the principal's **own** accounts (every level, id order) **then** the accounts that are live `shared` or `full` **and** linked to a resource group the principal is provisioned into (id order) | routing (`resolveVendorAccount`), the model listings and dashboard rows and the opt-in's save-time check: **parity-bound** (the member read applies the same shared-arm predicate to one account) |
| the **management union** (`ListVendorAccounts`) | own ∪ the accounts the principal **manages** (`VendorAccountsByAdminGroups` of the groups it manages vendor accounts through, live `shared`/`full`) ∪ the accounts it can **route to as a member**, each listed once in the first arm that holds it | the Anbieter page only, **never routing**. A CM sees an account it manages without being provisioned into it, and a member every account it can route to, a non-active one included, so it sees why it cannot use it |

**The grant is pure, not the server rule.** An account is available to a principal iff
it is **owned**, or it is (a) `shared` or `full` by its *effective* level (the stored
level while the sharing master switch is on, `none` while it is off), **read live from
the row in the same call**, AND (b) linked to a resource group the principal is
**provisioned into** (`provisionedRGsForPrincipal`: a user matches its user, user-group
and admin-group provisions, only `member` state counting so an invitation never grants;
a service matches only a **direct** `ProvisionKindService` provision, since a service is
in no group). Link existence is necessary, never sufficient: a `none` account with a
surviving link, a paused account and an account lowered a moment ago grant nothing. A
subscription account additionally needs `vendor_subscription_sharing_enabled` (link
creation does not look at that switch, so this is the one place it bites on an
existing link). A `disabled` **resource group** still grants its linked accounts,
exactly as it does for `AllowedServerIDs`: the grant reads links and provisions, not
the group's status.

> **This deliberately diverges from AI servers.** The server gate (`AllowedServerIDs`)
> is opt-in by default: a server in no provisioned resource group is available to
> **everyone** (`allowed[id] || !restricted[id]`), and in that default mode it **fails
> open** on a store error (it leans deny only under `resource_provisioning_enforce`).
> Copying that for a credential would hand every user the owner's key. The
> vendor grant reads neither `resource_provisioning_enforce` nor an open-pool branch: a
> shared account in no group, or in a group nobody is provisioned into, is usable **only
> by its owner**.

**One fail direction.** The owner's own accounts are **always** returned; only the
**shared arm** fails closed. A store error while computing the shared candidates denies
them and still returns the own accounts with no error (logged at Warn), so routing and
the listing agree even in the error window, unlike `AllowedServerIDs` in its default mode; only a failure to
read the principal's *own* accounts is reported, and the resolver then fails the request
as it always has. With the sharing master switch off the shared arm is empty and is not
even computed.

**An unwired seam is owner-only.** A nil enumerator falls back to
`VendorAccountsByOwner(token.UserID)`, the principal's own accounts only, **never
allow-all** (contrast `ProvisioningGate`, whose nil is allow-all): an unwired grant must
fail closed. The gateway installs the adapter only when a real `portal.API` exists, as a
genuinely nil seam otherwise, never a non-nil adapter around a nil api.

**Eligibility.** The vendor branch asks the grant only for a principal with an
identity: `token.UserID != "" || (token.IsService() && token.ServiceID != "")`, which
the resolver (`vendorPrincipalEligible`) and the listing (`vendorListingPrincipal`)
keep identical. A principal with neither is denied before any enumerator is consulted,
and so is a service token whose id is blank. A service owns no account and reaches a
shared one only through the grant.

**Precedence and parity.** The own block comes first, so the owner's own account wins a
same-name or same-prefix collision; among shared accounts the first active one by id
wins. The resolver chooses per **(name, dialect)**, not per name: an account that does
not serve the requested dialect (an OpenAI subscription for the anthropic dialect) is
skipped and the request falls through to the next account serving the name. So the
listing advertises a name under the **union** of the dialects its eligible accounts
serve it on, each backed by its precedence winner (§6.7): a name is advertised on
exactly the dialects it is routable on.

**Dispatch is unchanged and owner-agnostic.** It trusts `Target.VendorAccountID`; the
sealed credential opens only at the dispatch edge, so a borrower never sees it, and the
resolver is the single routing-time gate.

**Reach is not use, and management is not usage.** The grant decides which accounts a
principal can *reach*; a token still *uses* only the accounts its policy names (§11),
and for a shared account always by name. A CM who is not provisioned into a containing
group can read and operate the account but cannot route through it: to *use* it a CM
must be provisioned and opted in, like anyone.

**Cost.** The enumerator runs on every routed request that reaches the vendor branch,
before the token's policy is consulted. The sharing switches it asks for are **not** a
per-request settings read: `portal.Service` serves them from a 5 s invalidatable cache,
one settings read per window for both (§7), so with sharing off the enumerator costs the
principal's own-accounts read and nothing else. With sharing on the shared arm adds its
structural reads (provisioned groups, links, one account read per candidate), uncached on
purpose so the grant stays live; see [Risks
§11.1](../11-risks-and-technical-debt.md#111-operational-risks).

## 5. Usage & limits

Neither OpenAI nor Anthropic exposes an absolute cap on its **rate-limit windows**,
so those are shown as **percentages and reset times only**. The absolute figures
are the **spend-control credit allowance** of a Codex *Business* (workspace) plan,
whose quota is a number of credits rather than a window, and an **OpenRouter key's
USD limit**: the panel shows each as "used / limit" with its unit (§5.2, §5.4). Everything
lives in one per-account snapshot (`vendor_account_usage`) filled by best-effort
writers: a **passive header scrape** on every served request (§5.1) and an
**active pull** from the vendor's usage endpoint for an OpenAI subscription or for
an OpenRouter or Kilo api key: on its own when the account's usage view is opened
or refreshed, and as the last step of a models refresh, a connect or an
OpenAI-compatible create (§5.2). The two writers of an OpenAI or Anthropic
subscription write through the same **merge rule** (§5.3), so neither can blank
what the other learned; an OpenRouter or Kilo account has the pull as its only
writer and stores what it fetched, without a merge (§5.2).

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
percent is clamped to `≤ 100`. The scrape knows **nothing about spend control or
the credit state** (the response headers never carry them): it builds its snapshot
with `SpendUsedPct = -1` and no spend strings, so the merge (§5.3) keeps whatever
the active pull stored.

**OpenAI-compatible accounts (§4.4) are never scraped.** They are `vendor_openai`
targets, so the scrape runs on their responses, but it looks only for the Codex
`x-codex-*` headers, which these providers do not send: it finds none and writes
nothing. A native Gemini account (§4.5) is a `vendor_gemini` target: the scrape runs
for it too (it runs for any vendor target), but it has no recognized rate-limit
headers for that provider kind and writes nothing, the same empty outcome. Their
snapshot is therefore written by the active pull alone (§5.2) for OpenRouter and
Kilo, and stays empty (unknown, never a fabricated `0`) for x.ai, Google,
`google_native` and Custom, which have no usage query with an inference key.

### 5.2 Active pull (OpenAI subscription, OpenRouter and Kilo api keys)

The headers arrive only with a served request, so a ChatGPT subscription that has
not been used lately shows no windows, or old ones. For that account kind the
gateway can also **ask** the vendor, and for an OpenRouter or Kilo `api_key`
account, whose responses never carry usage headers at all, the pull is the only way
to learn anything (the api-key pull, below). An OpenAI `api_key` account (a
platform account has no subscription window to report), every Anthropic account
and the x.ai, Google, Google (native) and Custom accounts have no such endpoint; for Anthropic the
header scrape stays the only source.

| Credential | Request | Provenance |
|---|---|---|
| OpenAI **subscription** | `GET https://chatgpt.com/backend-api/wham/usage` with `Authorization: Bearer`, `ChatGPT-Account-Id` (left out when the account id is unknown) and `User-Agent: codex-cli`; no query, no body | reverse-engineered from the open-source Codex client, **VERIFY-LIVE** — the **Business** body below is the first sample confirmed against a live account; the `rate_limit` window shape of the other plans is still unconfirmed |

The fetcher is `vendorauth.FetchOpenAISubscriptionUsage` (`internal/vendorauth/usage.go`),
built like the discovery fetchers (§6): one GET, **no error return**, only a
snapshot and a status, `ok` or `unverifiable`. The URL and the `User-Agent` live in
`constants.go` beside the other VERIFY-LIVE constants.

**Three quota shapes.** The body carries a plan's quota in one of three shapes, and
the fetcher reads each independently (a body may carry more than one, and a plan
reports `null` for the shapes it does not use):

| Shape | Body fields | Snapshot fields | Plans |
|---|---|---|---|
| Rate-limit windows | `rate_limit.primary_window` → the five-hour window and `rate_limit.secondary_window` → the weekly one; each `used_percent` and `reset_at` (an absolute unix time in seconds) | `five_hour_pct` / `five_hour_reset_at`, `weekly_pct` / `weekly_reset_at` | Plus / Pro style plans (reverse-engineered, not live-confirmed) |
| Spend-control credits | `spend_control.individual_limit`: `unit`, `limit`, `used`, `remaining` (JSON **strings**), `used_percent` (number), `reset_at` (unix seconds) | `spend_unit`, `spend_limit`, `spend_used`, `spend_remaining`, `spend_used_pct`, `spend_reset_at` | Business / workspace plans (**live-confirmed**, one sample) |
| Credit balance | `credits.balance` (a nullable string) | `credit_balance` | plans that report one |

The `credits.unlimited` and `credits.has_credits` flags are read on top of these
and collapse into one `credit_status` (below).

The first live sample, from a **Business** plan, reads as follows (PII and the exact
usage figures replaced by placeholders; the types and the shape are the real
ones). `rate_limit` and `credits.balance` are both `null`, so a fetcher that read
only those two shapes found nothing to store and the panel stayed empty (#195):

```json
{
  "plan_type": "business",
  "rate_limit": null,
  "credits": {
    "has_credits": true, "unlimited": false, "overage_limit_reached": false,
    "balance": null
  },
  "spend_control": {
    "reached": false,
    "individual_limit": {
      "source": "group_based_spend_controls", "unit": "credit",
      "limit": "<limit>", "used": "<used>", "remaining": "<remaining>",
      "used_percent": 1, "remaining_percent": 99,
      "reset_after_seconds": 1938953, "reset_at": 1793491200
    }
  }
}
```

(Further keys of the real body, among them `user_id`, `account_id`, `email`,
`model_usage`, `code_review_rate_limit` and `promo`, are ignored and omitted here.)

**The credit status.** The two flags collapse into one store-friendly, unknown-aware
column, `credit_status`, by priority: `"unlimited"` when `credits.unlimited` is
`true`; else `"has_credits"` when `credits.has_credits` is `true`; else `"none"` when
`has_credits` is `false`; else `""` (unknown: no `credits` object, or both flags
absent, `null` or not booleans). Each flag is read on its own, so a malformed one
never hides the other. `overage_limit_reached`, `spend_control.reached` and the
`source`, `remaining_percent` and `reset_after_seconds` fields are **not read**.

Parsing is as tolerant as the scrape's, and for the same reason: a percent
(window or spend) is clamped to `≤ 100` and a negative one is unknown; an absent,
null or `0` reset is unknown; a balance is kept as the string the vendor wrote (a
JSON number is coerced to its literal, one longer than 64 bytes is unknown); the
spend-control strings (`unit`, `limit`, `used`, `remaining`) are trimmed and kept
**verbatim, never parsed to a number**, so the unit and the exact digits survive
(longer than 64 bytes, blank, or a JSON number where a string is expected is
unknown); a field of the wrong type is skipped without sinking its siblings. The
verdict is `ok` only when **at least one** field could be read: a window percent
or reset, a balance, a spend percent, reset, limit or used amount, or a credit
status (a spend `unit` or `remaining` alone does not count). Every other answer —
any non-2xx (a 401 included: it is no verdict on the credential, and only the
dispatch moves an account to `needs_reconnect`), a redirect (never followed), a
timeout, a transport failure, a body that is not JSON, or JSON that carries none of
the fields — is `unverifiable`,
and an empty answer is deliberately not "OK, nothing known", for the reason given
for discovery (§6.3): it must not be able to wipe a stored snapshot. The response
is capped at the same 8 MiB as a discovery fetch (§6.4).

**When it runs.** There are **two pulls**, one per credential kind, sharing one
store tail: `fetchVendorUsage` for an OpenAI subscription (the fetch with its
token set, then the merge over the stored row and the store write) and
`fetchCompatibleVendorUsage` for an OpenRouter or Kilo api key (the fetch with the
opened key, then the store write of the snapshot as fetched, no merge; see the
api-key pull below). Both are fail-soft and both have the same two callers.
Nothing else triggers them: there is no background job and no startup step (a
background refresher is a deferred follow-up, §5.4).

1. **As the last, best-effort step of `RefreshVendorAccountModels`** (§6), so it
   runs wherever that does: on the explicit `POST .../models/refresh` ("Modelle
   aktualisieren"), at the end of every OpenAI-subscription connect and at the
   create of an OpenAI-compatible account with its key (§6.2), both of which call
   the same function. The create-time pull therefore only exists for an OpenRouter
   or Kilo account created **with** a key.
2. **On its own, through the dedicated usage refresh** (§5.5), which is what the
   usage panel calls when its account's detail view is opened and when its refresh
   button is pressed. It is the only caller that does not also run the model
   discovery.

Both callers record the attempt as the account's **last active pull** (§5.5) when
the vendor was reached, so the on-view refresh does not ask the vendor again right
after a models refresh or a connect. The models-refresh step itself ignores the
lazy TTL: it pulls on every models refresh, the way the OpenAI subscription's
always has. The passive scrape (§5.1) is **not** a pull and records nothing.

As the models-refresh step the pull behaves as follows:

- It reuses the **token set the model discovery already opened** and, if it had
  expired, renewed through the gateway's locked refresher (§6.3). `fetchVendorUsage`
  never opens, renews or refreshes a token itself, and a token that could not be
  renewed means no pull. (The dedicated refresh opens and renews the token set
  itself, through the same helper the model discovery uses, before it calls
  `fetchVendorUsage`.)
- It runs only when the refresh itself did not fail with an error, and **also when
  the model list was unusable** (the refresh then reads `unverifiable` and keeps
  the models): the usage endpoint is a separate request that may well succeed.
- It runs **after** the model rows are written, so a slow usage endpoint can never
  starve the model write of the connect-time bound.
- It is purely additive: it cannot change the refresh's result, its account view or
  its error, the pull itself never changes the account's status, and it logs only
  the account id (and, on a store read or write failure, that store error) at
  Debug — never the token, the ChatGPT account id or any vendor text.

**Time budget.** The explicit models refresh makes up to **two** vendor requests,
each bounded by the discovery client's 10 seconds (the model list, then usage), so
it can take about 20 seconds against the server's 30 second write timeout, for an
OpenRouter or Kilo api key as for an OpenAI subscription. The dedicated usage
refresh makes at most one (plus a token renewal when the token had expired). The
connect flows and the OpenAI-compatible create stay inside their single 5 second
budget (§6.2), which the usage pull **shares** with the token renewal, the model
list and the model write: behind a slow model list the budget is spent and the
pull degrades to `unverifiable`, leaving the snapshot as it was. That is
acceptable for an advisory number, and the next refresh or view fills it in.

#### The api-key pull: OpenRouter and Kilo

An OpenRouter or Kilo `api_key` account is asked with the key the account already
stores; no second credential is involved
([ADR-053](../09-architecture-decisions.md#adr-053--usagecredits-for-api-key-vendor-accounts-a-per-preset-usage-strategy-over-the-existing-oauth-only-pull-path)).
The portal composes the URL from the account's `base_url` and the preset's
`UsagePath` (§4.4), `routing.VendorPresetFor` supplies the strategy, and the
fetcher in `internal/vendorauth/compatible_usage.go` (one per strategy, with the
same no-error-return, `ok` / `unverifiable` shape as the ChatGPT fetch) takes the
finished URL and the key as a `Bearer` token. The key is never logged or returned
and a transport error's text is dropped.

| Provider | Request | Maps to | Provenance |
|---|---|---|---|
| OpenRouter (`openrouter_key`) | `GET {base}/v1/key` (`https://openrouter.ai/api/v1/key`) with `Authorization: Bearer`; the same URL its Test connection probe asks | a **capped** key (`data.limit` a number): `spend_unit` `USD`, `spend_limit`, `spend_remaining`, `spend_used` (limit minus remaining, never below 0), `spend_used_pct` (0..100, only for a positive limit) and `credit_status` (`has_credits` while the remaining amount is above 0, else `none`; unknown when `limit_remaining` is absent). An **unlimited** key (`data.limit` an explicit `null`): `credit_status = unlimited` and every spend field left empty, so the panel reads "Unlimited" | **REVERSE-ENGINEERED / VERIFY-LIVE**: the shape is derived from OpenRouter's documentation, no live key was available |
| Kilo (`kilo_balance`) | `GET {base}/profile/balance` (`https://api.kilo.ai/api/profile/balance`, under the **root**, not under `/gateway`) with `Authorization: Bearer` | `credit_balance` (the balance as a plain decimal) and `credit_status` (`has_credits` while above 0, else `none`); `spend_unit` stays empty because the unit is not documented and is not asserted | **Best effort, undocumented**: the endpoint is read off Kilo's own client, which calls it with an OAuth access token, and it is **unconfirmed** that the api-key type the gateway stores is accepted at all |

Three things are worth knowing about what these figures mean.

- **OpenRouter reports the KEY, not the account.** `/v1/key` answers the limit and
  usage of the one key the gateway holds, in USD. It is not an account balance
  (that needs a management key, which the gateway never asks for), so
  `credit_balance` stays empty. A key without a cap is shown as "Unlimited"; the
  all-time `usage` figure the body also carries is deliberately not shown beside
  it, since it would read as a period figure. The richer OpenRouter fields
  (`usage_daily`, `usage_weekly`, `usage_monthly`, the free-tier counters, BYOK)
  have no home in the snapshot and are not stored (deferred, they would need new
  columns).
- **Kilo degrades quietly.** A 401, 403 or 404, a redirect, a body without a
  numeric `balance`, or a balance that is not a plausible number is
  `unverifiable`: the panel keeps its empty-state line and nothing is raised. That
  is the expected outcome while the key type is unconfirmed, not a fault.
- **Money is a plain decimal string.** Amounts are rendered without exponent or
  float noise (rounded to six places), so the panel can reformat the used amount
  and show the limit verbatim.

**Stored as fetched, not merged.** Unlike a subscription's snapshot (§5.3), an
api-key account's usage row is **replaced** by what the pull learned. Its pull is
the row's only writer (the scrape writes nothing for these accounts, §5.1), so
the merge's "an unknown incoming field is no news" rule has nothing to protect,
and it would keep showing the old limit and spend of a key whose cap the owner has
since removed: a merge cannot express "now absent". A failed pull, on the other
hand, writes nothing: the last good snapshot stays, as for every other pull.

**What the gate and the request path do.** `hasActiveVendorUsagePull` is true for
an OpenAI subscription and for an `api_key` account whose preset names a
`UsageVia` (OpenRouter, Kilo); the native OpenAI and Anthropic api-key accounts are
not in the preset registry and stay false. The dedicated refresh (§5.5) branches
**before** the OAuth token step, which would answer "not connected" for an account
that has no token set. An api-key account with **no key set** answers
`unverifiable` ("no API key is set"); one whose stored key **cannot be opened**
answers the documented `409 vendor_account.credential_unreadable`, not
`unverifiable`, exactly as for a subscription's token set. In the trailing pull of
a models refresh an unreadable or missing key is simply skipped, the refresh
having already said so.

### 5.3 The merge rule

This rule governs the two writers of an **OpenAI or Anthropic subscription's**
snapshot, the scrape and the pull. An OpenRouter or Kilo api-key account's snapshot
has a single writer and does not use it: its pull stores what it fetched (§5.2).

`UpsertVendorAccountUsage` replaces the whole row, and each writer sees only part of
the picture: the OpenAI scrape carries the credit balance only when the response
does, and a pull may know the windows but not the balance. If a write simply
replaced the row, a partial reading would blank known values. So **every usage
write keeps each known field and never blanks a stored one**, not merely "never
replaces a good snapshot with an all-unknown one".
Both writers read the stored row and combine it with their reading through
`routing.MergeVendorAccountUsage(existing, incoming)`, a pure function. Per field the
result takes `incoming` when it **knows** the field and keeps `existing` otherwise:
a percent (`five_hour_pct`, `weekly_pct`, `spend_used_pct`) is known when it is
`≥ 0` (a real `0` is known, `-1` is not), a reset time (`five_hour_reset_at`,
`weekly_reset_at`, `spend_reset_at`) when non-nil, and each string (`credit_balance`,
`spend_unit`, `spend_limit`, `spend_used`, `spend_remaining`, `credit_status`) when
non-empty. A field no writer has ever seen stays unknown (`-1` / nil / `""`) and is
never turned into a fabricated `0`: with no stored row, the reading is written as
it is. A writer that has no spend data must therefore set `spend_used_pct` to `-1`,
not leave the zero value: `0` is a *known* 0 % and would overwrite a stored percent
(the scrape does so, §5.1). A stored row that cannot be read is
handled per writer, both best-effort: the scrape writes its own reading as it is,
the pull writes nothing.

Two consequences to know:

- `UpdatedAt` is the time of the **last write**, not a per-field freshness. A credit
  balance carried over a scrape that only reported the windows still shows the
  fresh `UpdatedAt`, so the panel's "updated" time says when the snapshot was last
  touched, not how old each number is.
- A field the vendor **stops reporting** is not cleared: the merge reads an unknown
  incoming field as "no news", never as "gone", so it keeps its last value. A
  Business account whose spend control disappears from the body keeps showing its
  last stored credits, with the fresh `UpdatedAt`.
- The read-merge-write takes **no per-account lock**, in either writer. Two that
  overlap (a scrape during a refresh) can each merge against a read that misses the
  other's newer write, and the later write wins. The worst case is one writer's
  fresh reading being lost to the other's slightly older one, which the next write
  repairs; a field that was already stored when both read is never blanked. This is
  accepted for a best-effort snapshot; closing it would need a store-level merge
  upsert.

### 5.4 Reading it, and what is deferred

The snapshot is exposed on both **read** endpoints, as `VendorAccountDTO.Usage`
(`omitempty`, so an account the gateway has no snapshot for omits it): the **detail**
read (`GET /api/portal/vendor-accounts/{id}`), which the account's usage panel reads,
and the **list** (`GET /api/portal/vendor-accounts`), whose `data[]` rows carry it so
the dashboard's usage section (below) gets every account's figures in one call. The
create, update and connect responses and the models refresh's `account` carry none.
The list reads the snapshot **once per row** and is **fail-soft** about it: a
snapshot that cannot be read leaves that row's `usage` out (a Debug log with the
account id) instead of failing the whole list, whereas the detail read still returns
the error. That is not a new N+1 shape: the list already reads each account's model
rows per row, so each account costs one more small primary-key read.
`VendorAccountUsageDTO` (and the frontend's `VendorAccountUsage` type) is the same on
both and carries, always on the wire, the three windows' `five_hour_pct`,
`five_hour_reset_at`, `weekly_pct`, `weekly_reset_at`, the raw `credit_balance`, the
spend-control and credit-state fields `spend_unit`, `spend_limit`, `spend_used`,
`spend_remaining` (the vendor's raw strings), `spend_used_pct` (`0..100`),
`spend_reset_at` and `credit_status` (`"unlimited"` | `"has_credits"` | `"none"`),
and `updated_at`. The unknown sentinels travel as they are stored: `-1` for a
percent, `null` for a reset, `""` for a string, so an account with no spend control
reads `""` / `-1` / `null` for all of the spend fields. They are stored in the
seven columns migration 84 adds to `vendor_account_usage`
([Data Model](../reference/data-model.md#external-vendor-accounts-anbieter)).

The detail view's usage panel **re-reads** the account after anything that may have
changed the snapshot behind its back, so a fresh pull shows without reloading the
page: a successful models refresh — whatever its `ok` / `unverifiable` answer, since
the pull may have changed the snapshot either way; a refresh that failed with an
error does not trigger it — and a usage refresh the gateway answered (§5.5), the
panel's own button or, when it really pulled (`ok`), its on-view call. The
spend-control figures change only through an active pull: the passive scrape never
carries them (§5.1), so a Business account's credits are as fresh as its last pull —
the on-view refresh (at most every five minutes while its view is opened), the
refresh button, a models refresh or a connect.

**What the panel shows.** The panel (`VendorAccountUsage` /
`VendorAccountUsagePanel`) is **availability-aware**: it shows only the limits the
vendor provides for the account, and it is **always there** for an account that can
have usage — an `api_key` account or a connected subscription (a never-connected
subscription has served nothing and gets no panel). The titled frame (the "Nutzung &
Limits" panel; English "Usage & limits") stays even when no row would be drawn, and
then holds one empty-state line (English: "No limits from the provider are known for
this account yet.") instead of vanishing, so the refresh button (below) stays
reachable; the line is held back only while the first read of the snapshot is still
in flight, and a failed read of the snapshot also leaves the frame with that line
rather than a toast. An
`api_key` account has no snapshot from the scrape, since its responses carry none of
the headers of §5.1: an OpenRouter or Kilo account gets one from the active pull
(§5.2), and the others (x.ai, Google, Google (native), Custom, a native OpenAI or Anthropic key)
show the empty-state line. A window the vendor did not
report (`-1`, never observed) has **no row at all** — it is hidden, not drawn as
"no data" and never as a real 0 % (a real `0` is a known value and is shown). The
rows are therefore a subset of: the five-hour window, the weekly window and one
credit row. The credit side is **one row, chosen by priority**:

| Snapshot | Row |
|---|---|
| spend control known (`spend_used_pct ≥ 0`, or a non-empty `spend_limit` or `spend_used`): a Business plan's credits, or an OpenRouter key's USD limit | "Credit-Kontingent" (English: "Credit allowance"): `<used> / <limit> <unit> (<pct> %)` with a progress bar and the reset line (`spend_reset_at`); the panel's intro text switches to its pulled-figures variant, which also applies to the two rows below |
| else `credit_status = "unlimited"` (an OpenRouter key without a cap, or an unlimited plan) | the same "Credit-Kontingent" label with the value "Unbegrenzt" (English: "Unlimited") |
| else a non-empty `credit_balance` (Kilo's balance, or a plan that reports one) | "Guthaben" (English: "Credit balance") with the vendor's raw string |
| else `credit_status = "none"` | the same label with the value "Keine Credits" (English: "No credits") |
| else (`has_credits`, which has nothing more to say, or unknown) | no credit row |

Display details of the spend row: the used amount is reformatted (at most one
decimal, the portal locale's decimal separator, no digit grouping) when it is a plain
decimal, and shown exactly as the vendor sent it otherwise; the limit is always
shown as the vendor sent it. A unit of `credit` / `credits` (any case) or none reads
"Credits", any other unit (OpenRouter's `USD`) is shown as the vendor named it. The
at-most-one-decimal rounding is made for credits: a USD amount under a dollar
loses its cents in the used figure (0.42 reads "0,4"), while the limit stays exact.
Showing currency to the cent is a deferred follow-up
([Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks)). The percent is rounded and
capped at 100; the bar turns amber from 75 % and red from 90 %, and is left off
(with the text kept) when the percent is unknown. A missing half degrades to
"`<used>` Credits used" or "Limit: `<limit>` Credits", and an unknown percent leaves
off the parenthesis. For 42.5 of 6000 credits used (1 %) the German row reads
"42,5 / 6000 Credits (1 %)" and the English one "42.5 / 6000 Credits (1%)".

A Business account whose snapshot carries only the spend control therefore shows just
its credit row, with no empty window placeholders. Under the rows the panel prints
"Zuletzt aktualisiert: …" ("Last updated: …") from `updated_at`, which is the time of
the last write, not a per-field freshness (§5.3).

**Refreshing it from the panel.** For an account the gateway can actively ask — a
connected **OpenAI subscription**, or an **OpenRouter or Kilo `api_key` account with
a key stored** — the panel's frame also carries an "Aktualisieren"
button ("Refresh") that calls the dedicated refresh with `?force=1` (§5.5), shows the
answer inline by its `status` (an answer the gateway delivered is not an error:
`ok` and `fresh` read as a success, "Nutzung aktualisiert"; `unverifiable` reads as a
neutral, informational note that the usage could not be refreshed and the existing
figures are unchanged; `unsupported` says the account's usage cannot be queried from
the provider), and re-reads the snapshot; a call that itself failed is a toast. The
same account also gets **one on-view-lazy refresh** (the plain call, no `force`) when
its detail view opens, so figures that went stale while nobody looked are renewed
without a click; it is best effort, its failure is silent and it never loops or
polls. Every other account (an x.ai, Google, Google (native), Custom or native `api_key`, an Anthropic
subscription) gets neither the button nor the call: it learns its usage only from
the requests it serves. The frontend names the two api-key vendors itself
(`hasActiveUsagePull`), mirroring the registry's `UsageVia`; the backend registry
is the authority, so a drift between the two only shows or hides a button, and the
backend still answers `unsupported` for an account it cannot ask.

**On the dashboard.** The portal dashboard has a flag-gated section "Anbieter —
Nutzung & Limits" (English "Providers — usage & limits") that shows the user's own
vendor accounts and their limits side by side, one compact card per account (name,
vendor, a status chip when the account is not active, and the same rows in a tighter
layout). It reads the **stored snapshots through the list endpoint**
(`GET /api/portal/vendor-accounts`) and **never calls the vendor**: it makes no
refresh request, so it is safe to poll, which it does about once a minute so the
figures and the countdowns do not go stale on a dashboard left open. A card appears
only for an account that can report usage (an `api_key` account or a connected
subscription) **and** has at least one visible row; an account with no snapshot yet
is left out rather than drawn empty. When no account qualifies, before the first read,
and when the read fails, the section renders nothing, and the dashboard keeps its
tiles and routes. It is mounted only while the `vendor_accounts_enabled` master flag
is on, so a deployment that leaves the flag off never makes the request. Because it
shows only what the gateway has already learned, its figures move with the served
requests and the active pulls (§5.5), not with the vendor's own changes.

**Deferred.** API-key accounts carry no subscription window; absolute €/$ spend
accounting is deferred, and so are OpenRouter's period and free-tier figures and a
Kilo organization's balance. A **background refresher** — a periodic active pull
that would keep every OpenAI-subscription or OpenRouter/Kilo account's snapshot
current without anyone opening its view — is deliberately **not** built, and would be an opt-in, default-off follow-up. The
usage endpoint is reverse-engineered and ToS-restricted like the rest of the
subscription path (§10), so the gateway asks it only when a person looks (§5.5:
human-correlated and capped by the TTL), never on an idle timer: the usage is
refreshed when somebody opens or refreshes it, or when the account is refreshed or
connected.

### 5.5 The dedicated usage refresh and its lazy TTL

`POST /api/portal/vendor-accounts/{id}/usage/refresh` (no body;
[API Surface](../reference/api-surface.md#vendor-accounts-anbieter)) pulls an OpenAI
subscription's or an OpenRouter or Kilo api key's usage **without re-listing the
models**, so the usage panel can keep
its figures current on its own (`RefreshVendorAccountUsage`,
`internal/portal/service_vendor_usage_refresh.go`). It is an **operate** call (§8): the
owner, or a CM of a `shared` or `full` account, never system scope alone (it opens the
owner's sealed credential and sends it to the vendor, so it is authorized like a
write), and **owner-only even at `shared` for a subscription account**, because the
pull renews an expired token first, the lazy on-view pull included (a CM gets
`403 vendor_account.forbidden` and nothing is renewed or written). It is gated by the
master flag.

| Call | `maxAge` | Behavior |
|---|---|---|
| plain `POST .../usage/refresh` — the **on-view-lazy** call | the lazy TTL, **5 minutes** (`portal.VendorUsageLazyTTL`) | When the account's last active pull is strictly younger than the TTL the vendor is **not asked**: the stored snapshot is answered with `refresh.status: fresh`. Otherwise it pulls. |
| `POST .../usage/refresh?force=1` — the **manual** button | `0` | Always asks the vendor. |

**The TTL is enforced on the server**, so a client that opens the view in a loop
cannot hammer the vendor. It is keyed off an **in-memory, per-account** "last active
pull" time held by the portal service (a mutex-guarded map, no column and no
migration); a restart forgets it, which costs at most one extra pull per account,
and deleting the account drops its entry. The time is **recorded only when the
vendor was actually reached**: a vendor that answered with an error, or an unusable
body, counts (a failing vendor is not asked again on every view), but a refresh that
never got that far does not. That covers an account with no usable token (not
connected, or an expired token that could not be renewed) and a context that had
already ended when the fetch returned (the connect flows' shared 5 second budget
spent on a slow model list, a cancelled request). Without that rule a pull that
never happened would make the next lazy call answer `fresh` with no snapshot for the
whole TTL. A clock stepped backward never keeps an entry fresh (a negative age is
not "fresh"), and two concurrent calls for one account may both ask the vendor (the
merged write is idempotent).

**The answer** is `200 {"usage": <the stored snapshot, as GET .usage, or null>,
"refresh": {"status", "detail"}}`, the snapshot read **after** the pull:

| `refresh.status` | Meaning |
|---|---|
| `ok` | The vendor was asked, answered usably and the snapshot was stored (merged, §5.3, for an OpenAI subscription; as fetched for an api-key account, §5.2). |
| `fresh` | The lazy TTL skipped the pull; the vendor was **not** called. It says the pull was skipped, not that the data is current: the last attempt may have failed, so the stored snapshot can be older than the TTL, or `usage` can be `null`. |
| `unverifiable` | Nothing usable could be had: no usable token, or no API key set, the vendor unreachable or answering with an error or an unusable body (Kilo's undocumented balance endpoint is expected to do so while the stored key type is unconfirmed), or a snapshot that could not be stored. The stored snapshot is kept. It says nothing about the credential (only the dispatch moves an account to `needs_reconnect`, §3.4). |
| `unsupported` | Neither an OpenAI subscription nor an OpenRouter or Kilo api key: a native OpenAI or Anthropic `api_key` account, an x.ai, Google, Google (native) or Custom account and an Anthropic subscription have no active pull; their snapshot, when they have one, only comes from the scrape (§5.1). No credential is opened. |

`refresh.detail` is a short English phrase for diagnostics; the portal keys off
`status` only. The endpoint is **fail-soft** like the models refresh: an
unreachable vendor is a `200` with `unverifiable`, never an error. The only errors
are a stranger's or unknown id (`404`, no existence leak), the master flag off
(`409`), a stored credential that cannot be opened (`409
vendor_account.credential_unreadable`: a subscription's token set or an api-key
account's key; an api-key account with **no** key set is `unverifiable`, not this)
and a store failure (`500
vendor_account.usage_refresh_failed`).

**What the portal does with it.** The usage panel (§5.4) calls the plain form once
when the detail view of an account it can ask (an OpenAI subscription, an OpenRouter
or Kilo api key) opens — a cheap stored read while the TTL holds — and the `?force=1`
form from its refresh button. Nothing polls it.

## 6. Dynamic model discovery and the model prefix

The catalog of an OpenAI or Anthropic account is seeded at creation from a small
static set, and that set is only a guess (§10): a ChatGPT subscription's Codex
backend serves newer model generations than any list kept in this repository, and
a vendor renames or retires models without notice. So the gateway asks the vendor
which models the account's **own credential** can really use and **replaces** the
guess with the answer. The static set stays as the create-time fallback and as
what an account keeps whenever discovery yields nothing. The preset vendors, the
OpenAI-compatible ones (§4.4) and native Gemini (§4.5), seed **nothing**: their
catalog starts empty and is filled by a best-effort discovery at create (when the
account is created with its key, §6.2) and by the refresh.

Discovery is split like the validation probes (§3.5). One fetcher per credential kind lives
in `internal/vendorauth` (`discover.go`, `openai_compatible.go` for the
OpenAI-compatible vendors and `gemini.go` for native Gemini) and never returns an error — only a list
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
| **OpenAI-compatible `api_key`** (x.ai, OpenRouter, Kilo, Google, Custom) | `GET {base}{prefix}/models` with `Authorization: Bearer`, the URL composed by the portal from the account's `base_url` and its preset (§4.4) and fetched by `vendorauth.DiscoverOpenAICompatibleModels(modelsURL, key)`, **not** through the provider client | every `data[].id`; display name from `display_name`, else `name` (OpenRouter sends `name`), else the id. For Google a leading `models/` is stripped from the id (and from a display name that was only the id standing in) | the providers' own endpoints, confirmed live 2026-10-10, **VERIFY-LIVE** |
| **Google (native) `api_key`** (`google_native`) | `GET {base}/v1beta/models?pageSize=1000` with `x-goog-api-key`, the URL composed by the portal from the account's `base_url` and its preset (§4.5), following `nextPageToken` for at most 20 pages, fetched by `vendorauth.DiscoverGeminiModels(modelsURL, key)`, **not** through the provider client | only entries whose `supportedGenerationMethods` contain `generateContent` (the listing also carries embedding and other models); the `models/` resource prefix is stripped from the id (and from a display name that was only the id standing in). A listing still paging after the cap is `unverifiable`, never a truncated catalog | Google's documented endpoint, shape and method vocabulary **VERIFY-LIVE** (§10) |

An OpenAI api-key listing names every model the key reaches, with no capability
data, so it is narrowed by a small heuristic: the `gpt-*`, `chatgpt-*` and
o-series families (and fine-tunes of them), minus ids that mark an embedding,
speech, image, moderation, realtime or completion-only model. A model the rule
does not recognize is left out — a missing model is added by a newer rule, while
a non-chat model offered wrongly fails every request routed to it. **That heuristic
stays OpenAI-only**: the OpenAI-compatible vendors keep every listed id exactly as
the provider sends it (an aggregator's catalog is the point), and the per-token
vendor access and the prefix (§11, §6.5) are what scope which of them a token sees.

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
- **At create (preset accounts).** Creating an OpenAI-compatible or native Gemini
  account **with its key** runs the same discovery once, best-effort and under the same 5-second
  bound, because these accounts have no static seed and would otherwise serve
  nothing until the first refresh. For OpenRouter and Kilo the trailing usage pull
  of §5.2 rides along inside the same bound, so a new account can show its limit or
  balance at once. It never fails the create: whatever goes wrong
  is logged without the key and the account keeps its empty catalog. An account
  created without a key, or whose key is set or replaced later through the update
  request, does not discover by itself; the refresh below is how it fills.
- **On demand.** `POST /api/portal/vendor-accounts/{id}/models/refresh` — the
  "Modelle aktualisieren" button of the account's Models panel in the detail view
  — runs the same discovery and may wait for the vendor client's own 10 second
  timeout, twice for an OpenAI subscription or an OpenRouter or Kilo api key (the
  model list, then the usage pull of §5.2; [API Surface](../reference/api-surface.md#vendor-accounts-anbieter)).
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

- **At most 1000 models** per discovery (raised from 500 for the aggregators:
  OpenRouter lists about 460 models and Kilo about 390 at the time of writing, and
  both are growing). Entries beyond the cap are counted as dropped in
  `refresh.detail`.
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
resolver (§4) matches on the effective public name (equal to the stored
`gateway_model` for the native prefix, §11.2) and sets the target's
`ProviderModel` to the second. The prefix is applied wherever rows are written: to the creation seed, to
every discovery, and on a PATCH that carries `model_prefix`, which **re-labels the
existing rows** without asking the vendor again. Re-sending the stored prefix
repairs rows a failed re-label left behind. Two rows whose labelled ids collide
keep only the first.

Two behaviours to know:

- With a prefix set, the bare vendor id is **not** served by that account.
  Different prefixes are also how a user keeps two accounts that offer the same
  model both reachable. The prefix above is the account's **native** one; an
  API token can be given a different one, or none, per account (§11.1).
- On the native-passthrough path (an OpenAI subscription's or an OpenAI api-key
  account's `/v1/responses`, §4.2, or an Anthropic api-key or subscription
  account's `/v1/messages`, §4.1) the gateway rewrites only the request's `model`
  field to the bare slug (the Anthropic subscription's relay additionally
  injects the Claude-Code system block, which does not touch `model`). The vendor's response is relayed verbatim and there is
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

Every listing reads **one** source, `ownVendorAccountModels` (the name
predates sharing; the set is no longer owner-only): the **active** vendor accounts the
principal can **route to**, its own and then the ones shared with it
(`VendorAccountsForRouting`, §4.6, the very enumerator the resolver routes by), and
their rows that the principal's **token may use** (§11) — each named under the token's
effective prefix — behind the master flag (and, for a `subscription` account, the
`vendor_subscriptions_enabled` switch, §7), for a principal with an identity (a user,
or a service token with a service id) and fail-open per account. (That fail-open is the listing's: for the **generated Claude alias
derivation** an unreadable account, like a failed read of the principal's own accounts, makes the vendor
set incomplete and yields no generated aliases at all, fail-closed, because its
vendor-shadow exclusion needs the whole set; see
[Routing and Model Selection §2.4](routing-and-model-selection.md#24-generated-claude-model-aliases).) The owner's interactive portal session (`all: true`) may use all of its **own**
accounts, so the table below describes what a session sees of them (never a shared
account, §11); an API token sees the subset, and the names, its policy grants. A
shared account the grant lets the principal reach is advertised only to a token whose
policy names it. A failure of the **shared** arm alone is not an incomplete set: it is
the routable set, the one the resolver uses in that same window. What
the listings advertise and what the dashboard shows therefore cannot drift, and a
vendor model is never subject to the gateway-wide hidden/locked suppression,
which applies to self-hosted models.

| Surface | Vendor models of the accounts the caller can route to |
|---|---|
| `GET /v1/models`, `/openai/v1/models`, `/anthropic/v1/models`, `/api/v1/models`, `/openai/api/v1/models` | Listed under the prefixed name, per dialect the account serves (the served-flavor matrix of §4: the OpenAI subscription only under `openai`). A vendor-only model has no `context_size`, so `/v1/models` lists it without `context_length` and `/api/v1/models` with `max_context_length: 0`. |
| Portal chat picker and a regular user's **Models** page | Listed. |
| **Dashboard "Live Model Routes"** (`GET /api/portal/dashboard`) | Listed, one row per model: the prefixed name, the vendor as provider, the account's name as host, status `active`. A vendor model has no server, so its row id is the account id plus the model; the portal keys rows by that id because two accounts may share a name and a model. |
| **Admin Models management** (`ManageModels`, the admin view of the Models page) | **Not listed — deliberately.** It shows the system's real models, and a vendor model is one principal's own, not something an operator manages for the system. |
| A **personal** account of another user, a shared account the caller is not provisioned into (or one that is paused, lowered or in no resource group), a shared account the caller's token does not name, or a disabled / `needs_reconnect` account | Never. |
| A `subscription` account while `vendor_subscriptions_enabled` is off | Never on any row above (the account is kept, and listed again once the switch is back on); its `api_key` siblings are unaffected. The listing reads the same switch as the resolver's skip (§4), from one settings read together with the master flag, and the skip also drops the account's models from the generated-alias vendor-shadow set. |

A name several accounts serve is advertised under the **union** of the dialects they
serve it on (the resolver picks an account per name **and** dialect, §4.6), so a name
whose first account is openai-only stays advertised, and routable, on the anthropic
dialect through a later both-dialect account.

The asymmetry is intended and worth stating, because it can read as a bug: an
admin who connects an account sees its models on their own Dashboard, in the
chat picker and in `/v1/models`, but not on the Models management page. The
account's own detail view lists all its rows with the vendor's display name and
the id clients request, and carries the refresh button.

## 7. Feature flag and routing mode

Six system settings govern the feature (all read from the `system_settings`
store). The **resolver** reads the first three (the master flag, the subscriptions
switch and the routing mode) each through its own cached accessor
(`vendorEnabledCache`, `vendorSubscriptionsEnabledCache`, `vendorRoutingModeCache`),
**invalidated on the settings PUT** whenever it carries any of those keys
(`invalidateVendorSettingsCache`), so a portal toggle takes effect on the next
resolve; the accessors' short TTL (~5 s, `vendorSettingsCacheTTL`) only bounds an
**out-of-band** change, such as a direct database edit. `portal.Service` (the CRUD
and connect gates, the model-listing overlay and the discovery client version)
reads them uncached, so a PUT is visible on its very next call (the two sharing
switches' per-request decision reads are the exception, cached and dropped on every
settings write; see below):

| Setting | Values | Default | Effect |
|---|---|---|---|
| `vendor_accounts_enabled` | bool | **off** | The **master** flag. When off: the "Anbieter" nav item is hidden, the CRUD/connect/test-connection endpoints answer `409 vendor_accounts.module_disabled`, and the resolver's vendor branch and the model-listing overlay are no-ops. |
| `vendor_subscriptions_enabled` | bool | **on** (opt-out) | Gates **only** `auth_type = subscription` accounts, strictly narrower than the master flag (it only matters while the module is on, and it never affects an `api_key` account). When off: creating a subscription account and every connect flow answer `409 vendor_accounts.subscriptions_disabled`; existing subscription accounts are **kept** but stop routing and are dropped from every listing (the resolver and the listing move in lock-step, §4, §6.7); the portal Anbieter page flags each one as currently unusable. Default on, and an absent, blank or unparseable value, or an unreadable settings store, reads as **on**. Detail below. |
| `vendor_account_routing_mode` | `vendor_first` \| `fallback_only` | `vendor_first` | Precedence between the vendor accounts a caller can route to (its own and the ones shared with it) and the self-hosted routes. `vendor_first`: a vendor account wins when it serves the requested model. `fallback_only`: a vendor account is used only when no self-hosted route exists. |
| `vendor_openai_codex_client_version` | version string | `26.930.61225` | The Codex `client_version` the OpenAI **subscription** model discovery sends (§6.6). The ChatGPT backend **hides every model whose `minimal_client_version` exceeds it**, so a stale value silently hides new models: **raise it when OpenAI ships a newer Codex app** (System settings, no redeploy). Blank resets to the built-in default (`vendorauth.CodexModelsClientVersionDefault`); a malformed value is a `400 system.vendor_openai_codex_client_version_invalid`. Read only by `portal.Service` (never the resolver), so it has no cache. VERIFY-LIVE. |
| `vendor_account_sharing_enabled` | bool | **off** | The **sharing master** switch (opt-in, **fails closed**: an absent, blank or unparseable value, or an unreadable settings store, reads as off). Off: `share_level` is **ignored** by routing, the listings and every gate, so every account behaves personal (its *effective* level is `none`); the share-level, management-group and resource-group endpoints answer `409 vendor_accounts.sharing_disabled`; the Groups UI hides the `can_manage_vendor_accounts` column and the Anbieter page the sharing controls. A **pause, not an un-share**: nothing stored (level, root, links) is touched, so switching it back on honours the stored level again. Meaningful only while `vendor_accounts_enabled` is on (the module flag answers first). |
| `vendor_subscription_sharing_enabled` | bool | **off** | Whether **subscription** accounts may be shared at all (§10); subordinate to the master switch (the effective condition is the AND of both). Off: *raising* a subscription account answers `409 vendor_accounts.subscription_sharing_disabled`, and an already-shared subscription account stops granting (the grant's live predicate asks it, the one place it bites on an existing link). Lowering is never refused. Fails closed like the master switch. |

**`vendor_subscriptions_enabled` in detail.** It is the one **opt-out** setting of the
set, the narrowest, and it can only restrict further inside an enabled module. Only an
explicit `false` turns it off. While it is off:

- **Create and connect are refused.** `CreateVendorAccount` with
  `auth_type = subscription` and every connect method (token import, code-paste
  begin/complete, device begin/poll, all of which run through one gate) answer
  `409 vendor_accounts.subscriptions_disabled`, and nothing is stored. The check is
  deliberately late so that it masks no other answer: after the master flag, the
  owner authorization and the subscription-only check on a connect (a stranger or an
  unknown id is still `404`, an `api_key` account still `400 not_subscription`), and
  after request validation on a create (a preset vendor asked for a `subscription` is
  still `400 auth_type_invalid`).
- **Existing accounts are kept but inert.** Nothing is deleted or changed, and there
  is no schema change or migration (the setting is one more `system_settings` key), so
  switching it back on restores every account as it was. The resolver skips them (§4)
  and the listing overlay leaves them out (§6.7), reading the same switch, so no model
  is advertised that cannot be routed.
- **The portal says so.** The Anbieter page marks each subscription account with a
  notice that it cannot currently be used. The notice is render-only and reads
  `subscriptions_enabled` from the probe below; viewing, renaming and deleting an
  account stay available (§8). The page also stops inviting the actions the
  backend would refuse: the create form disables the Subscription auth-type option
  (with an inline reason) and never submits a subscription create, and a blocked
  account's connect/reconnect panel is hidden — so a models-refresh outcome also
  drops its "reconnect via that panel" hint, which would otherwise point at a
  control that is not on the page.

**Fail direction.** The master flag fails **closed** (an unreadable store reads as
off). This switch fails **open**: an unreadable settings store, or an absent, blank or
unparseable value, reads as on, in the portal, in the resolver's accessor (a resolver
built without one allows subscriptions) and in the settings read-back, so a transient
read error can never silently take an operator's subscription accounts out of service.
The price is the mirror image of the master flag's cache tail: because the resolver
reads the switch through a 5 s cache (`vendorSettingsCacheTTL`) and a read that fails
is cached as on, a failed read right after an operator turned subscriptions off can
keep the resolver routing them for up to ~5 s, until the next successful read
reflects off. Create, connect and the listing read it uncached through
`portal.Service`, so they see an off on their very next call (a failed read there is
likewise treated as on).

**What "off" does not stop.** Turning subscriptions off blocks inference routing,
model advertising, create and connect, and nothing else. The **test connection**,
**model refresh** and **usage refresh** on an *existing* subscription account (§8)
are not gated by it: each still opens the stored OAuth credential and sends it to
the vendor, and a refresh may renew and so rotate the stored token set (§3.4). That
includes the owner's explicit clicks and also the automatic, TTL-throttled usage
pull that fires, with no click, whenever an OpenAI subscription account's detail
page is opened (§5.5). "Off" is therefore not "the gateway never contacts the
vendor's subscription endpoints". An operator who needs that turns the master flag
off (which blocks those actions too) or deletes the account.

**The two sharing switches in detail.** Both follow the settings plumbing of the
others (schemaless `system_settings`, a pointer field on the PUT where an omitted or
`null` value keeps the stored one, an always-present boolean on the GET, invalidation
on a PUT that carries either key) and both fail **closed**, the opposite of
`vendor_subscriptions_enabled`: a transient read error never turns sharing on. They are
read in three places, through two kinds of read. The **guard** of every share,
management-group and resource-group method (`requireVendorAccountSharingEnabled`, module
flag first, so a disabled module answers `vendor_accounts.module_disabled` and not the
sharing code) uses the `portal.Service` accessors (`VendorAccountSharingEnabled`,
`VendorSubscriptionSharingEnabled`), which hit the settings store on every call,
**uncached** (a full-table `SELECT`): it runs once per request on a rare write path, and
an admin acting right after a flip is told the truth. The two **decisions** that run per
request or per account, the *effective* level every gate and listing reads
(`effectiveVendorAccountShareLevel`) and the routing grant (§4.6, which also reads the
subscription switch), read the switches through **one 5 s invalidatable cache** in
`portal.Service` (`vendorAccountSharingEnabledCached`,
`vendorSubscriptionSharingEnabledCached`; one settings read per window serves both
switches). It is the same mechanism as the gateway's other vendor-flag caches (the
shared `ttlcache.Cache`, the generic behind `settingCache`), it is dropped after **every**
settings write (`persistSystemSettings`, so a portal toggle in either direction is seen on
the very next decision), and the TTL only bounds an **out-of-band** change such as a
direct database edit. It fails **closed** exactly like the accessors: an unreadable or
missing settings store reads as both switches off for that call, and a failed read is
**not retained** (the next call reads again), so an outage is never pinned for a TTL
window. The raw accessors stay uncached on purpose: the gateway defines its own cached
wrappers around them (`vendorAccountSharingEnabledCached`,
`vendorSubscriptionSharingEnabledCached` on `Server`, dropped by
`invalidateVendorSettingsCache` on a PUT that carries either key, though no production
path consults them today), and a cache inside the accessors would stack two TTL windows
under that one. The grant's remaining per-request cost when sharing is on is its
structural reads, not the switches ([Risks
§11.1](../11-risks-and-technical-debt.md#111-operational-risks)).

The frontend reads the switches through a portal-scoped
`GET /api/portal/vendor-accounts/enabled` (`{module_enabled, subscriptions_enabled,
sharing_enabled}`, readable by any user even while the module is off — it exists to let
the shell show or hide the nav item, the Anbieter page flag inert subscription accounts,
and the Anbieter page and the resource-group manager hide the sharing controls that
would answer `409`, without granting the system-scoped settings read; it returns only
those **three** booleans, so `vendor_subscription_sharing_enabled` is never exposed
there), mirroring the NetBird/certificates `/enabled` endpoints. The exact-path route wins
over the `{id}` subtree because account ids are always `va_`-prefixed.

The same master flag gates the **model-listing owner overlay** and the dashboard
rows that carry an account's models (§6.7); for a subscription account the
subscriptions switch gates them too.

A further system setting, `anthropic_prompt_caching_enabled` (bool, **off**,
experimental), is not part of the vendor-account policy but applies to its Anthropic
targets: it lets the translate path place prompt-cache breakpoints. Like the first
three it is read on the hot path through a TTL cache that is invalidated on a PUT
that carries the key, but through its **own** gateway-side cache
(`anthropicPromptCachingEnabledCached`, 5 s), separate from the resolver's
vendor-settings cache (see *Translate prompt caching* in §4.1).

## 8. Authorization by share level

The HTTP handlers gate on the standard web scope (`gateway:use`); per-object
authorization happens **in `portal.Service`**, by the account's share level (§1.1).
Three kinds of principal can touch an account:

- the **owner**: `owner_user_id` equals the principal, at every level;
- a **co-manager (CM)**: an owner or co-manager holding `can_manage_vendor_accounts` of
  an admin group the account is assigned to (`vendor_account_admin_groups`), while the
  account is `shared` or `full`. It is the **intersection** of the account's management
  groups with `vendorAccountManageGroupIDs(principal)`, both read live, so
  un-assigning the group or demoting the co-manager revokes the reach on the very next
  call;
- a **member**: a principal provisioned into a resource group that contains the account.
  A member may *read* a minimal view (§8.2) and *use* the account once a token opts in
  (§11); it never operates it.

**System scope stays read-only and never bypasses a write.** It may read any account
(`GetVendorAccount`, the management view), but it lists only its own, and on an account that is
not its own, every write and every call that opens or acts on the sealed credential (the
connection test, the model refresh, the usage refresh) treats it as a stranger: those calls send the owner's
credential upstream, so they are authorized like a write, not like a metadata read.

### 8.1 The matrix

| Operation | `none` | `shared` | `full` |
|---|---|---|---|
| **Read** (key-free DTO) | owner, system scope | owner, CM, member (minimal view), system scope | owner, CM, member (minimal view), system scope |
| **Use** via a resource group plus a token opt-in | owner | owner, member | owner, member (a CM too, once provisioned and opted in) |
| **Operate**: status, model prefix, rename, connection test, model refresh, usage refresh | owner | owner, CM (¹) | owner, CM (¹) |
| **Credential**: connect, reconnect, device connect, import, set or change the api key | owner | owner | owner, CM |
| **Management-group assignment**; resource-group link | not available | owner | owner, CM |
| **Delete** | owner | owner | owner, CM |
| **Raise** (`none` to `shared` to `full`) | owner and admin | owner and admin | nobody (already the top) |
| **Lower** | not available | owner | owner, CM |

(¹) **Subscription operate-op caveat.** For a **subscription** account the operate
calls that open the sealed OAuth token set, the **connection test, the model refresh
and the usage refresh**, are **owner-only even at `shared`**: the two refreshes renew an
expired access token through the gateway's refresher, which persists the rotated
**single-use** refresh token (and can trip the account-wide `needs_reconnect`), and a CM
may not do that to the owner's token (the test only opens the set and renews nothing,
but it is listed with the other two and the rule covers it). A CM keeps status, model
prefix and rename on a subscription account. The rule is
`requireOwnerForSubscriptionOperate`, run **after** the operate gate by every call that
opens a subscription's token set (a further token-rotating operation joins the rule by
calling it too); it tests the account's **auth type, not its level**, so it fails
closed whatever level a stored row claims. An api-key account has no token to rotate:
the owner and a CM both pass.

So **`full` is complete owner-equivalent delegation**: a CM may do everything the owner
can, the credential, the group and resource-group assignment, the delete and the
lowering, except **raise**, which happens at `none` or `shared`, where a CM has no
management reach. The owner's control is structural: they own the record, chose the
level and can always lower it.

Notes on how the matrix is enforced:

- **Two denials, on purpose.** An operation the caller may not perform is answered
  `404 vendor_account.not_found` when the caller has no business knowing the account
  exists (a stranger, system scope on a write, a co-manager without the flag, a principal
  with no user identity, an account whose level was lowered or whose sharing is paused:
  indistinguishable from an unknown id, the no-existence-leak rule), and
  `403 vendor_account.forbidden` when the caller **can already see** the account, a CM of
  a `shared` or `full` one, and the operation is simply not theirs (a credential call, the
  delete or an assignment at `shared`, a raise, a token-opening call on a subscription):
  "not found" would be a lie to them. This is a deliberate departure from the servers'
  never-a-403 posture.
- **The gates.** There is a read gate (`authorizeVendorAccountRead`, which returns the
  viewer's tier, §8.2) and three write gates: **operate**
  (`authorizeVendorAccountOperate`: the owner, or a CM at `shared`/`full`), **credential**
  (`authorizeVendorAccountCredential`: the owner, or a CM at `full`; also the gate of
  the connect calls, the delete, the lowering and the management-group assignment) and
  **raise** (`authorizeVendorAccountRaise`: the owner who is a current admin). The two
  resource-group link paths differ: the owner's self-service is owner-only at `shared`
  and `full` (a CM links through the other path), and the resource-group manager's side
  is the operate gate plus the `full`-only restriction plus matching roots, so an owner
  who also manages the group still links a `shared` account through their own
  self-service, not through the group.
- **PATCH is field-level and deny by default.** `name`, `status` and `model_prefix`
  take the operate gate; **every other field**, `api_key` and any credential- or
  endpoint-sensitive one, takes the credential gate, and a body that mixes both takes the
  stricter. The allowlist is one function (`isVendorAccountPatchOperateField`) and there
  is deliberately no list of "credential fields", because a list of what is sensitive is
  what an overlooked field falls out of; the body is walked by reflection, so a field
  added to the request later is credential-gated until someone edits the allowlist, and
  a test restates it literally. The decision depends only on which fields are *present*,
  never on their values (an `api_key` is not knowable without opening the sealed one),
  so a client that round-trips `base_url` or `auth_type` takes the credential gate even
  though they are create-only: a change to either is `400 vendor_account.field_immutable`,
  answered after the gate so it confirms nothing to a stranger. The credential-gated
  methods are enumerated, not illustrative: import, begin and complete connect, device
  begin and poll, `api_key` set, change and clear, delete, lowering and the
  management-group assignment.
- **An operate-only PATCH never writes a credential column.** It writes only the columns
  it names plus `updated_at` (`UpdateVendorAccountOperateFields`, in every store
  driver), because the load-modify-write is not atomic and a full-row write would carry
  the credential columns of a stale copy back over the owner's concurrent key rotation,
  a dispatch-time refresh-token rotation or the `needs_reconnect` flip, writing a
  credential without passing the credential gate. A PATCH that takes the credential gate
  still rewrites the whole row ([Risks
  §11.1](../11-risks-and-technical-debt.md#111-operational-risks)).
- **Raise and the first groups are owner and admin; lowering needs only ownership.**
  Every management-group set must keep **at least one** group while the account is
  shared (an empty set is rejected). A lowering needs only ownership at `shared` (an
  owner can un-share even after losing the admin role) or owner-or-CM at `full`.
- **The effective level and the master pause.** Every decision that lets a **non-owner**
  through on the strength of the share level reads the *effective* level, `none` while
  `vendor_account_sharing_enabled` is off (§7): a CM then reads, operates and deletes
  nothing, the management arm of the list is empty, and only the owner can act. The DTO
  keeps reporting the **stored** level, because the pause is a global switch and not a
  property of the account. A delete is not a sharing endpoint, so the sharing switch does
  not *refuse* it, but the credential gate reads the effective level.
- **Response hygiene.** Every CM-reachable operate endpoint (test, refresh) answers with
  fixed English phrases and scrubs its result and error payloads of any api key, access
  token or refresh token (`scrubCredential` on the test's `detail`), so a CM never sees
  the raw secret even in an echoed upstream error. A regression test plants secrets on
  every channel (the stored credential, a raw upstream error, vendor-listed model names,
  an unreadable stored value) and drives each endpoint as a CM and as the owner.
- **Management is not usage** (§4.6): a CM who is not provisioned into a containing
  resource group can read and operate but cannot route through the account.

### 8.2 What each viewer reads

The account DTO is rendered **for the viewer**, widest tier first (so a CM who is also
provisioned into the account's group reads the management view):

- **Management view**: the owner, a CM of a `shared` or `full` account (by its effective
  level) and system scope. Every field, the share fields filled, `is_owner` the viewer's
  own and `shared_by` the owner's display name for a non-owner, **and still no secret**:
  the DTO has no secret field to hand out by mistake, and a test pins its field set to an
  explicit credential-free allowlist.
- **Member view**: a principal that can only **route** to the account through a resource
  group. Built **from an allowlist, deny by default** (`memberVendorAccountDTO`, from
  nothing, never by blanking the management DTO): `id`, `vendor`, `auth_type`, `name`,
  `status`, `model_prefix`, `models` (each only the gateway name, the dialect and the
  display name), the two credential booleans, `is_owner: false` and `shared_by`.
  **Withheld**: `share_level`, `admin_groups`, `system_group_id` and `system_group_name`
  (who manages the account and where it is rooted), `base_url` (where the owner's key
  is sent), `usage` (the owner's rate-limit and spend figures), each model's
  `upstream_model` and the timestamps. One builder renders it for both the single read
  and the member-only rows of the list, so the two cannot differ.
- **Nothing**: everyone else, the same `404`.

A member's read needs **reach**, not the opt-in (`isMemberOfSharedVendorAccount`): the
account is `shared` or `full` by its effective level, a subscription account needs
subscription sharing, and the principal is provisioned into a resource group containing
it, the same predicate the list's member rows use. The opt-in gates *using* an account;
reading is how a member finds an account to opt into. A principal without a user
identity is never a member of the management page, even a service provisioned into the
group; a store error while deciding reach fails the read, never grants it.

### 8.3 Related rules

The `vendor_subscriptions_enabled` switch (§7) is an operator-wide gate, not an
authorization rule: it narrows what a subscription account may do (create, connect,
serve, be listed) but never who may reach it, and it leaves an owner's view, rename and
delete of an existing account, and the three vendor-contacting actions above, as they
are. A token's vendor-access policy (§11) narrows reach further, never widens it: it can
name only accounts the token's principal can reach, and a stale id matches nothing. The
Activity list follows the account's read rule for the account's **name**: a usage row
carries its `account_id` for every viewer, but the transient `account_name` is resolved
only for a viewer who may read the account (the owner, a CM of a shared or full account,
a member that can route to it, or a system admin) and stays empty for anyone else
([Telemetry §8.4.1](telemetry-usage-observability.md#841-the-usage-event)).

## 9. Secrets at rest

Credentials reuse the repo-wide secret envelope verbatim — no new key, no new
mechanism ([ADR-007](../09-architecture-decisions.md#adr-007--secrets-at-rest-the-encplain-scheme)):

- The `api_key` and `oauth_tokens` columns are **sealed** with
  `capture.SealSecret` **before** the store write, and opened with
  `capture.OpenSecret` at the cipher-holding dispatch edge **and, for the explicit
  test-connection (§3.5), model refresh (§6.3) and usage refresh (§5.5) run by the owner
  or, on an api-key account, a CM, inside `portal.Service`**
  (`checkVendorAccount`, `RefreshVendorAccountModels`).
  On those paths the opened value goes only to the vendor's own validation,
  model-listing or (OpenAI subscription) usage URL: it is never returned, logged or echoed in a verdict's or a
  refresh's `detail`. Likewise a token import probes the plaintext access token
  the user just submitted, before it is sealed. A refresh re-seals in place.
- The read-back DTO exposes **presence only** (`api_key_set`,
  `subscription_connected`), never the value, for **every** viewer: the owner, a CM and
  a member alike. A borrower never sees or opens the credential: dispatch is
  owner-agnostic and opens it only at the dispatch edge (§4.6). PATCH uses the keep/clear/replace
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
- **Sharing a subscription is the most exposed thing the feature can do, so it is
  capped and separately gated.** A subscription account may be raised to `shared`
  **but never to `full`**: its OAuth credential stays strictly owner-only (connect,
  reconnect, device connect and the token set never reach a CM). Raising one needs
  `vendor_subscription_sharing_enabled` (default **off**, subordinate to the sharing
  master switch, §7), and the same switch gates an existing shared subscription's
  grant. Even at `shared`, the operate calls that open the token set (the connection
  test, the model refresh, the usage refresh) are owner-only (§8.1), so no CM can
  rotate the owner's single-use refresh token or trip an account-wide
  `needs_reconnect`. Borrowers spend the **owner's** rate-limit windows and credits, and
  the usage snapshot (§5) stays one per account, mixing borrowers. None of this makes
  the subscription path a supported multi-tenant capability: it stays built for internal
  testing with the operator's own account, and the whole of sharing is off by default
  ([ADR-060](../09-architecture-decisions.md#adr-060--shared-vendor-accounts-a-tiered-share-level-a-pure-routing-grant-authorization-by-level-and-a-per-account-opt-in)).
- **Every subscription constant is reverse-engineered and VERIFY-LIVE.** Each
  endpoint (including the two subscription credential-validation probes, §3.5, the
  two subscription model-list endpoints, §6.1, and the ChatGPT usage endpoint,
  §5.2),
  client id, redirect URI, scope, beta header, device-code path, token-claim name,
  masquerade requirement (including the passthrough's injected block and merged
  `anthropic-beta`, §4.1), serving host, request/refresh body encoding, and the
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
  its API key serve the same ids, so its set does not vary. The preset vendors, the
  OpenAI-compatible ones (§4.4) and native Gemini (§4.5), seed **empty**: discovery
  is their only source of models. Even the narrowed
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
  A third is **not** accepted but tracked, and narrowed: an operate-only PATCH now
  writes only its own columns (`UpdateVendorAccountOperateFields`), but a PATCH that
  carries the api key still rewrites the whole row, so an `api_key: ""` clear sent to
  a subscription account can write a stale `oauth_tokens` back; its fix is an api-key
  column writer (§3.4; [Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks)).
- **The OpenAI-compatible presets are the providers' own endpoints, but still
  VERIFY-LIVE data (§4.4).** Each preset's root, path prefix, key-probe endpoint and model-list
  shape was confirmed live on 2026-10-10: x.ai at `https://api.x.ai/v1`;
  OpenRouter at `https://openrouter.ai/api/v1` with its key endpoint `/v1/key`;
  the Kilo Gateway at `https://api.kilo.ai/api/gateway` with a public listing and
  no key endpoint; Gemini at `https://generativelanguage.googleapis.com/v1beta/openai`
  with `models/`-prefixed ids and a 400 on a wrong key (as for x.ai). The
  providers can still move any of it without notice. It all sits as data in
  `internal/routing/vendor_presets.go`, so a correction is an edit there, and a
  moved endpoint degrades to `unverifiable` or an unchanged catalog, never to a
  wrong one. The **owner-supplied base-URL** trust model is the one deliberate
  acceptance here: it applies to any preset account (OpenAI-compatible or native
  Gemini) whose owner supplies a root, which is required for Custom and an optional
  override for the named presets (x.ai, OpenRouter, Kilo, Google and `google_native`),
  and the root is not otherwise restricted beyond https, so the on-prem
  gateway can be pointed at any https host
  (server-side request forgery is accepted, with its limits,
  [Risks §11.4](../11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
- **Native Gemini was designed from documentation, without a live key, so its wire
  details are VERIFY-LIVE (§4.5).** The conservative choice in each case is what the
  code does today, and a wrong guess degrades to an upstream error or an unverifiable
  probe, not to a wrong answer:
  - whether the native endpoint accepts an API key sent as `Authorization: Bearer`
    (assumed not, so the key is sent as `x-goog-api-key` only, never as a Bearer
    token);
  - a wrong key may be answered with 400 `INVALID_ARGUMENT` or with 401
    (`ACCESS_TOKEN_TYPE_UNSUPPORTED`), which the uniform classification turns into
    `unverifiable` or `invalid` respectively (§3.5);
  - the exact shape of `/v1beta/models`, its default page size and the vocabulary of
    `supportedGenerationMethods` (the discovery filters on `generateContent`);
  - whether every model accepts `parametersJsonSchema` for tool schemas (the fallback
    would be the OpenAPI-subset `parameters` field), and whether an external image URL
    is accepted as `fileData` (else it is dropped);
  - streaming details: that `usageMetadata` is cumulative so the last one is kept,
    that function-call parts arrive whole, the shape of a mid-stream error frame and
    that `finishReason` is absent on partial chunks;
  - what Gemini 3 does with a history that carries no `thoughtSignature` (a
    cross-provider turn), a documented dummy-signature fallback versus failing, and
    which parallel function-call parts carry signatures;
  - per-model quirks that drive the conservative rendering: sampling parameters the
    newer models ignore or reject, a trailing `model` turn answered with a 400, and
    `thinkingLevel` versus `thinkingBudget` per model family (the family is sniffed
    from the model name).
- **The OpenRouter and Kilo usage endpoints are the least certain data in the
  registry (§5.2).** OpenRouter's `GET {base}/v1/key` is a documented endpoint, but
  its response shape was derived from the documentation and has not been confirmed
  against a live key, so the parser is tolerant (a field of the wrong type is
  skipped, a body with nothing usable is `unverifiable`) and the shape is marked
  REVERSE-ENGINEERED / VERIFY-LIVE. It reports the **key's** limit and usage, not
  the account's credit balance. Kilo's `GET {base}/profile/balance` is
  **undocumented**, read off Kilo's own client (which sends an OAuth access token),
  and it is not known whether the api-key type the gateway stores is accepted; the
  fetch is best effort and every failure degrades to the panel's empty-state line.
  Both sit as data in the registry (`UsageVia`, `UsagePath`), so a correction is an
  edit of the table and its test
  ([Risks §11.1](../11-risks-and-technical-debt.md#111-operational-risks)).
- **Single-process assumptions.** The pending-connect state (PKCE verifier/state
  and the device-code pending entry), the per-account refresh lock and the
  per-account model-write lock (§6.4) are all **in-process**: a gateway restart
  loses any connect in progress (the user starts it again), and a multi-replica
  deployment (the PostgreSQL driver) would break the refresh single-flight — two
  replicas could refresh one account concurrently and burn its single-use refresh
  token — and let a discovery and a prefix re-label undo each other. The feature is an operator-only experiment, so
  one gateway process is assumed.

## 11. Per-token vendor access

An account is personal to its owner (§1) unless it is shared (§1.1), but in either case
**which API tokens may use it is the token's own decision**, made in the token editor:
by the owner for a user token, by whoever creates it for a service token. The
default is strict opt-in: a token reaches **no** vendor account until it is
granted one, so connecting an account never silently exposes it to every key the
owner has issued, a leaked or shared key cannot spend a subscription the
owner never meant it to reach, and **borrowing another owner's credential is always a
deliberate, named choice**, because `all: true` never covers a shared account (§11.1)
([ADR-050](../09-architecture-decisions.md#adr-050--vendor-account-access-is-a-per-token-opt-in-enforced-in-listing-and-routing-through-one-prefix-helper)).

### 11.1 The policy

The policy is the token's `vendor_access` (wire) / `vendor_provider_access`
(column, a JSON string on `api_tokens`, migration 85;
[Data Model](../reference/data-model.md#4-migration-history-92-migrations)):

```json
{ "all": false,
  "accounts": [
    { "account_id": "va_…" },
    { "account_id": "va_…", "prefix_override": { "enabled": true, "value": "work/" } }
  ] }
```

| Setting | Effect |
|---|---|
| **No policy** / `all: false` with an empty list | **No vendor access.** The default for every new token, and — because migration 85 adds the column with an empty default and backfills nothing — for every token that existed at upgrade. `""` is how it is stored. |
| `all: true` | Every **active** account the token's principal **owns**, **including accounts connected later**, each under its **own** `model_prefix`. The account list is ignored **for the principal's own accounts**. It **never covers a shared account**: an account shared with the principal is used only when an entry names it. |
| `all: false` + a list | Only the listed accounts, the principal's own or ones shared with it. Each entry may carry a **prefix override**. |
| `all: true` + an entry naming a **non-own** account | The entry is a live **opt-in to that shared account** and is validated like an `all: false` entry (an id the principal cannot reach is refused as an unknown one, so it is no longer stored inert). An entry naming an own account is ignored, as before sharing: all wins. |
| `prefix_override` `{enabled: true, value}` | The token sees that account's models under `value` **instead of** the account's `model_prefix`. |
| `prefix_override` with **`enabled: true` and an empty `value`** | The account's models are served under their **original vendor names, with no prefix** (`gpt-5` rather than `chatgpt/gpt-5`). This is a real, supported setting, not an unset field. |
| No `prefix_override`, or `enabled: false` | The account's own `model_prefix`. A stored override only counts while it is enabled, so an object with `enabled: false` is a switched-off override. |

An override value follows the account-prefix rule (§1: at most 64 bytes from
`A-Z a-z 0-9 - _ . ~ : / @ +`, no `..`); an enabled one that breaks it is refused at
save. Only **active** accounts are ever reachable — a `disabled` or
`needs_reconnect` account is neither listed nor routed whatever the policy says —
and the master flag (§7) still gates the whole feature, the subscriptions switch (§7)
the `subscription` accounts among them. The stored policy decodes
tolerantly: a blank or malformed value reads as the strict default and an entry
without an account id is dropped, so a bad row can never break token resolution or
open access. An account deleted after a grant leaves a stale id behind, which
simply matches nothing.

The token editor (portal, **Provider access** / "Anbieter-Zugriff") exposes it as
an "All providers (incl. future)" switch, a checkbox per connected account and,
for each ticked account, an "Override prefix" toggle with a prefix field whose
hint states that empty means no prefix. Accounts shared with the creator are offered
too, each labelled with its owner's name; the list is the creator's own, managed and
member accounts, so it can offer an account the token's principal cannot actually reach,
which is why the save-time check and the grant, not the form, are the gates. The
service-token create form offers the same list, on create only (§11.6). The whole section is hidden while the
master flag is off, and the field is then left out of the request so an edit
cannot disturb a stored policy ([API surface: token vendor
access](../reference/api-surface.md#token-vendor-access)).

### 11.2 One helper, so a listed name is a routable name

The effective public name of a vendor model, for a given token, is
**`prefix + upstream_model`**. Both places that need it ask one function,
`routing.TokenVendorPrefix(token, account) → (prefix, allowed)`, which takes the whole
token because the own-versus-shared line depends on the token's principal
(`routing.TokenOwnsVendorAccount`: the account's owner is the token's user; a service
owns nothing), the one place that line is drawn, for routing, the listing and the
save-time check alike:

| Policy | Result for an account |
|---|---|
| `all: true`, the account is **owned** by the token's principal | allowed, the account's own `model_prefix` |
| `all: true`, a **shared** account the policy does not name | **denied** |
| listed (own or shared), no (or disabled) override | allowed, the account's own `model_prefix` |
| listed (own or shared), override enabled | allowed, the override value (possibly `""`) |
| not listed (and not covered by `all`) | **denied** |

For an **own** account `all` wins over a listed entry (its override is not applied),
exactly as before sharing.

- **Listing.** `ownVendorAccountModels` (§6.7) skips a denied account and
  re-labels each remaining account's rows to `prefix + upstream_model`, so
  `/v1/models`, `/openai/v1/models`, `/anthropic/v1/models`, `/api/v1/models`,
  `/openai/api/v1/models` and every consumer of that one source advertise
  exactly the token's names.
- **Routing.** `resolveVendorAccount` (§4) skips a denied account and matches the
  request model against `prefix + upstream_model`. **The resolver reverse-maps the
  token's public name back to the raw vendor slug**: the target keeps the public
  name as the request model (usage rows record what the client asked for) and
  carries the row's `upstream_model` as its `ProviderModel`, which is what the
  vendor is sent. Matching the stored `gateway_model` instead would pin every token
  to the account's native prefix.

Sharing the helper is the parity contract: an advertised name cannot be
unroutable, and a routable name cannot be unadvertised. Reach and use are separate
questions asked in order: the grant (§4.6) decides which accounts the principal can
**reach**, this helper which of those the token **uses**. An override is purely a
**naming layer per token** — the stored catalog is the account's, written once by
discovery (§6) under its native prefix, and nothing is copied per token.

An empty override puts a vendor's own names into the token's namespace, so it can
share a name with a self-hosted model. The existing routing mode decides (§7): under
`vendor_first` the token's vendor account answers that name, under `fallback_only`
the self-hosted route does. That is the point of choosing bare names, but it is also
the reason to prefer a distinct prefix when both exist.

### 11.3 Name collisions

Two accounts can offer the same effective name: two accounts of one vendor with no
prefix, or an override that lands on another account's prefix. The two Google
accounts are a concrete case: a shim `google` account and a native `google_native`
account discover the same model names, so without a prefix on one of them they
collide (give one a distinct `model_prefix`, §6.5). Two layers handle it:

- **Rejected at save** (every entry with `all: false`; under `all: true` only the
  entries naming a non-own account). Creating or updating a token checks the
  effective names across the checked accounts' **current** models and refuses a
  duplicate with `400 portal.token_vendor_access_conflict`; the owner resolves it by
  giving one account a distinct override. The same save also refuses an account id the
  token's principal **cannot reach** (unknown, or someone else's and not shared with it:
  the same answer, so nothing leaks), a repeated one and a malformed enabled override
  value with `400 portal.token_vendor_access_invalid`. A transient failure to read the
  principal's own accounts is a `500`, and a failure to read one account's models is
  logged and skipped rather than failing the save (the backstop below still applies). An
  `all: true` policy that names no non-own account has no save-time check, because it
  names nothing to check.
- **A deterministic backstop at routing.** A collision that only emerges later —
  the vendor ships a model after the save, or an account's own prefix is changed —
  cannot be caught at save. The resolver enumerates accounts in the grant's **stable
  order** (the principal's own accounts first, then the shared ones, each in
  account-id order) and the **first** match wins, so an own account beats a shared one
  of the same name, the same rule that already applies to colliding native
  prefixes, while the model listings show a name once. Routing is therefore
  never ambiguous; the cost is that a shadowed account's model is unreachable under
  that name until the owner separates the prefixes.

### 11.4 Who is restricted: API tokens, not the owner's session

The opt-in binds **API tokens**, and only those:

| Principal | Vendor access |
|---|---|
| **API token** (user-owned) | Its stored policy; strict default none. Its own accounts are covered by `all`, a shared account only by a named entry. |
| **The owner's interactive portal session** (`sessionPrincipal`, with the trusted loopback chat acting for it) | **Always all** of the owner's **own** active accounts under their native prefixes (`VendorAccess{All: true}`), and, because `all` never covers a shared account, **never a shared one**. It has no `api_tokens` row to opt in on, and the chat model picker, the Models page and the dashboard's live routes (§6.7) depend on seeing every account the user owns; an account shared with the user is reached through a token that names it. |
| **A run-as token** (a chat run that acts through one of the user's tokens) | That token's own stored policy, like any API call with it. |
| **Service token** (a service id, no user id) | Its stored policy, **create-only** (§11.6). A service owns no account, so `all: true` covers nothing for it; it reaches a shared account only when its policy **names** it **and** the service is **provisioned directly** into a resource group that contains it. A service token with no policy has no vendor access, as before. |

So a user always sees and can chat with every account they own, while a key
they hand to a script reaches only what they ticked for it, and no principal borrows an
account without a named choice. The token list's
synthetic "chat session" row carries no `vendor_access` for the same reason.

### 11.5 Upgrade behavior

The strict default is a **deliberate behavior change** for any token that was
already calling a vendor model: after migration 85 its requests for those names fall
through to the self-hosted path and, finding no such model, end as
`routing.no_model_route`, and the model listings no longer advertise them, until the
owner opens the token and opts it in. The trade is accepted over the alternative of
defaulting existing tokens to `all: true`, which would leave every previously issued
key with access to accounts it was never meant for
([ADR-050](../09-architecture-decisions.md#adr-050--vendor-account-access-is-a-per-token-opt-in-enforced-in-listing-and-routing-through-one-prefix-helper),
[Risks §11.4](../11-risks-and-technical-debt.md#114-deliberate-design-acceptances)).
The feature is behind the default-off master flag (§7), so only a deployment that
enabled vendor accounts is affected.

### 11.6 Service tokens and the save-time check

A service token carries the same opt-in as a user token, on **new plumbing**:

- `vendor_access` is a field of `CreateServiceTokenRequest` and `ServiceTokenDTO`;
  `CreateServiceToken` writes the token's `VendorProviderAccess` column and
  `RotateServiceToken` **preserves** it (it writes only the secret columns). It is
  **create-only**: a service token has no field-level update, so changing the opt-in
  means creating a new token, matching every other service-token setting. The bearer
  lookup builds the policy onto the request principal for a service token exactly as for
  a user token.
- A service reaches a shared account only through a **direct service provision**
  (`ProvisionKindService`) into a containing resource group, the way a service reaches
  servers, because a service is in no management or user group, so there is no group
  path. The service-side resource-group self-service (`GET`/`PUT`/`DELETE
  /api/portal/services/{id}/resource-groups[/{rgId}]`, writing the same
  `resource_group_provisions` rows the group's provisions editor writes, for a caller who
  manages the service **and** is a member of an admin group the group is linked to) is the
  place a service manager sees and edits that provisioning.
- The save-time check validates against the **token's own principal**: the owning user
  for a user token (`userTokenPrincipal`), the service for a service token
  (`serviceTokenPrincipal`), **never the admin or delegate saving it**, because a token
  does not inherit its creator's reach.
- The check is **advisory; routing is the sole security gate.** Naming an account the
  principal cannot reach is harmless (routing re-evaluates the grant on every request and
  denies it, and a stale id matches nothing), so the check only keeps unreachable ids out
  of a new policy the way an unknown own id always was. The shared arm of the grant fails
  **closed** on a store error, which reads as "not reachable", the same answer routing
  gives in that window.

## Related chapters

- [Routing & Model Selection](routing-and-model-selection.md) — the resolver and
  candidate scoring the vendor-account source plugs into.
- [API Compatibility & Inference](compatibility-and-inference.md) — endpoint
  modes, native passthrough vs. translate, the neutral inference model.
- [Security, Authentication & Authorization](security-auth-rbac.md) — scopes and
  object-level authorization.
- [Data Model](../reference/data-model.md#external-vendor-accounts-anbieter) — the
  three tables and the `usage_events.account_id` column (migration 82), the
  prefix and display-name columns (migration 83), the usage snapshot's
  spend-control columns (migration 84), the per-token
  `api_tokens.vendor_provider_access` column (migration 85), the
  preset accounts' (OpenAI-compatible and native Gemini) `vendor_accounts.base_url`
  column (migration 86) and the share model (migrations 90 to 92: the
  `can_manage_vendor_accounts` flag, `share_level` / `system_group_id`,
  `vendor_account_admin_groups` and `resource_group_vendor_accounts`).
- [HTTP API Surface](../reference/api-surface.md#vendor-accounts-anbieter) — the
  `/api/portal/vendor-accounts*` routes (the model and usage refreshes and the
  share-level, management-group and resource-group endpoints included) and
  their error codes; [token vendor access](../reference/api-surface.md#token-vendor-access)
  for the per-token `vendor_access` field and its two error codes.
- [Security, Authentication & Authorization §12](security-auth-rbac.md#12-delegated--resource-scoped-authorization)
  — the vendor-account row of the delegated-authorization table and the
  `can_manage_vendor_accounts` co-manager permission.
- [Configuration & Environment Variables](../reference/config-env.md) — the cipher
  key and the six system settings.
- [ADR-049](../09-architecture-decisions.md#adr-049--vendor-accounts-are-a-first-class-entity-the-subscription-oauth-path-is-experimental-and-tos-restricted)
  — the first-class-entity and experimental-subscription decisions.
- [ADR-050](../09-architecture-decisions.md#adr-050--vendor-account-access-is-a-per-token-opt-in-enforced-in-listing-and-routing-through-one-prefix-helper)
  — the per-token opt-in, the shared prefix helper and the collision policy.
- [ADR-051](../09-architecture-decisions.md#adr-051--anthropic-translate-prompt-caching-is-flag-gated-decided-at-the-gateway-by-a-hybrid-auto-switch-and-placed-by-the-provider)
  — translate prompt caching: the flag, the hybrid auto-switch and where the
  breakpoints go.
- [ADR-052](../09-architecture-decisions.md#adr-052--openai-compatible-vendors-are-presets-over-one-openai-client-a-stored-root-url-plus-a-registry-derived-path-prefix)
  — OpenAI-compatible vendors as presets over one OpenAI client: the registry, the
  stored root, the path prefix and the owner-supplied base-URL trust model.
- [ADR-053](../09-architecture-decisions.md#adr-053--usagecredits-for-api-key-vendor-accounts-a-per-preset-usage-strategy-over-the-existing-oauth-only-pull-path)
  — usage/credits for api-key vendor accounts: the per-preset usage strategy, its
  root-relative path and the single-writer store of the api-key pull.
- [ADR-054](../09-architecture-decisions.md#adr-054--native-google-gemini-dialect-its-own-vendor-id-and-provider-kind-selected-by-the-preset-dialect-field-over-a-leaf-translation-package)
  — the native Gemini dialect: its own vendor id and provider kind, the preset
  `Dialect` selector, the shared `geminiwire` translation package and the
  translate-only scope.
- [ADR-060](../09-architecture-decisions.md#adr-060--shared-vendor-accounts-a-tiered-share-level-a-pure-routing-grant-authorization-by-level-and-a-per-account-opt-in)
  — shared vendor accounts: the tiered share level, management that mirrors servers
  with a routing grant that does not, authorization by level, the subscription cap and
  the per-account opt-in across user and service tokens.

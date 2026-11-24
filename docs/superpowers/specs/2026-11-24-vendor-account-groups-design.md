# Vendor Account Groups ("Anbieter-Gruppen") — Design

Status: draft for review · Branch: `feat/vendor-account-groups` · Date: 2026-11-24

This is a branch-local working document (see `AGENTS.md`). Everything durable
from it moves into `docs/architecture/` before the pull request.

## 1. Intent

The user wants to **keep working without interruption**. Today a token reaches
a vendor model through exactly one vendor account; when that account's
subscription window is used up, Claude Code or Codex stops until the window
resets. A **vendor account group** lets the user put several accounts of the
same vendor into an ordered list: the gateway serves each request from the
first account in that order that still has room, and moves on to the next one
when an account is almost used up. The typical group is "my Claude
subscription first, a Claude subscription shared with me second, a paid
Anthropic API key as the reserve at the end".

## 2. Goals, non-goals, anti-goals

**Goals**

- A user can create groups of vendor accounts of the **same vendor**, mixing
  auth types (subscription and API key) and mixing their **own** accounts with
  accounts **shared with them** that they may route to.
- Members are ordered by **position**; the order is a strict priority.
- Each group has **per-category thresholds** (five-hour window, weekly window,
  credits/spend), default **99 %** each. A member at or over any threshold of a
  category it reports is skipped.
- A token opts in to a group exactly like it opts in to an account, with the
  same prefix override, in the token editor's provider-access section. The
  "all providers (incl. future)" switch gets a second checkbox **"incl. groups"**.
- When **all** members are over a threshold, the group behaves as if it held
  only the member that becomes available **soonest**.
- A vendor **limit error** (429/402 and the vendors' quota error codes) on a
  member, before any byte reached the client, transparently retries the request
  on the next eligible member.
- The **dashboard** shows, per group, which member is active and why the ones
  before it are skipped.
- Groups are managed under the existing **"Anbieter" (Providers)** menu item.

**Non-goals (this project)**

- Sharing a group itself with other users or services. Groups are personal.
- Groups across vendors (e.g. an Anthropic subscription plus OpenRouter's
  Claude models). Model ids differ across vendors, so such a group could not
  fail over by model name.
- Load balancing or round robin between members. Order is strict priority.
- Per-model weekly windows (Anthropic's `seven_day_opus` / `seven_day_sonnet`,
  the `7d_oi` header triple) as thresholds. A member that is out of one of
  these is caught by the limit-error failover (§6).
- The portal-chat work agreed as the **second project** (§13).
- Any change to the admin-managed self-hosted **model groups**
  (routing doc §5); the two concepts stay separate.

**Anti-goals (failure even if it "works")**

- A group must **never widen reach**: through a group a token can only reach an
  account its principal could reach anyway (owned, or shared with it by the
  live grant). Losing access to a shared account silently removes it from the
  group's candidates.
- **No mid-stream switch.** Once any response byte has been relayed to the
  client, the request stays on its member; a failover after that point would
  corrupt the client's stream.
- **No fabricated usage values.** An unknown category is unknown, never `0`
  and never "over threshold".
- **No hammering of vendor usage endpoints.** Anthropic's subscription usage
  endpoint rate-limits aggressively; pulls must be rare and backed off (§7).

## 3. Background (current system)

- The entity is `routing.VendorAccount` ("Anbieter"), owned by one user,
  optionally shared (`share_level`) — `docs/architecture/cross-cutting/external-vendor-accounts.md`
  §1. Reach to a shared account is decided by one grant,
  `VendorAccountsForRouting(principal)` (§4.6).
- A token's `vendor_access` policy decides which reachable accounts it
  **uses**, each under the account's `model_prefix` or a per-token prefix
  override (§11). One helper, `routing.TokenVendorPrefix`, feeds both the model
  listing and the resolver, so advertised names equal routable names (§11.2).
- Usage lives in one snapshot per account (`vendor_account_usage`, §5):
  five-hour and weekly percent with reset times, credits/spend. Writers: a
  passive header scrape on every served request (§5.1) and an active pull for
  OpenAI subscriptions and OpenRouter/Kilo keys (§5.2).
- Since #407 the Anthropic scrape reads `anthropic-ratelimit-unified-5h-utilization`
  / `-5h-reset`, `-7d-utilization` / `-7d-reset` and the representative claim,
  so both Anthropic windows carry their own reset time already
  (`gateway/backend/internal/gateway/vendor_usage_scrape.go`).

### 3.1 Code map (verified on `a5a4eacd`; paths under `gateway/backend/` unless noted)

- **Resolver vendor branch:** `resolveVendorAccount`
  (`internal/routing/resolver.go:1175`); eligibility `vendorAccountRoutingEligible`
  (:1287); enumeration `routableVendorAccounts` (:1312) through the
  `VendorAccountEnumerator` seam (:470, set at :746, wired at
  `internal/gateway/server.go:1190`; real implementation
  `portal.Service.VendorAccountsForRouting`,
  `internal/portal/service_vendor_sharing_grant.go:50`); per-account gate
  `vendorAccountServes` (:1206); name match `vendorAccountModelMatch` (:1271,
  `prefix + UpstreamModel == req.Model`, first account and first row win);
  target builders `vendorAccountTarget` (:1381), `vendorSubscriptionAnthropicTarget`
  (:1503), `vendorSubscriptionOpenAITarget` (:1570). The routing mode is applied
  in `Resolve` (:851-866). **The resolver never reads the usage snapshot today.**
- **Token policy:** `auth.VendorAccess{All, Accounts}` /
  `auth.VendorAccessEntry` (`internal/auth/vendor_access.go`); wire codec
  `internal/store/token_vendor_access.go`; `routing.TokenOwnsVendorAccount` and
  `routing.TokenVendorPrefix` (`internal/routing/vendor_access.go`); save-time
  check `validateVendorAccess` (`internal/portal/service.go:6100`, with
  `checkVendorAccessEntry` :6168 and `claimVendorAccessNames` :6208); the session
  principal gets `VendorAccess{All: true}` in `sessionPrincipal`
  (`internal/gateway/auth.go:337`).
- **Listing:** `ownVendorAccountModels` → `advertisedVendorAccountModels` →
  `vendorModelFlavorSets` / `vendorDashboardRoutes`
  (`internal/portal/service_vendor_listing.go`), consumed by `modelsResponse`,
  `dashboardRouteData`, `modelFlavorSets`, `ModelOfferingFor` and
  `claudeAliasesForToken`.
- **Usage snapshot:** `routing.VendorAccountUsage` and `MergeVendorAccountUsage`
  (`internal/routing/store.go:419/447`); passive scrape
  `internal/gateway/vendor_usage_scrape.go`, called from `recordUsage`
  (`inference_complete.go:848`, after the response); active pulls in
  `internal/vendorauth/usage.go` (OpenAI `wham/usage`) and `compatible_usage.go`
  (OpenRouter, Kilo); gate `hasActiveVendorUsagePull` and the in-memory 5-minute
  lazy TTL (`internal/portal/service_vendor_usage_refresh.go`). **No periodic
  pull exists.** No Go code calls Anthropic's `api/oauth/usage`; ADR-107 lists it
  as a rejected option, but for a different purpose (the gateway's *own*
  principal limits shown to Claude Code), not for reading a vendor account.
- **Dispatch and errors:**
  - *Passthrough* (`/v1/messages`, `/v1/responses`, the paths Claude Code and
    Codex use): `tryProxyNative` → `relayPassthroughTarget` → `proxyNative`
    (`internal/gateway/native_passthrough.go`). The upstream status is
    **peeked before anything is written**, and a lost instance is already
    re-selected there (`rel.reselect` → `reselectPassthrough`); the upstream
    429/402 body and rate-limit headers are otherwise relayed as they came.
  - *Translate* (`complete`, `completeStream*` in `inference_complete.go`,
    `streamSession` in `stream_session.go`): provider clients collapse every
    non-2xx other than 503/401/403 into `ErrUnavailable`
    (`internal/provider/client.go:48-58`), which surfaces as
    **502 `provider.unavailable`**, so a vendor 429/402 is not distinguishable
    there today.
  - *Existing pre-first-byte re-selection to mirror:* instance re-selection
    (`instance_reselect.go`, `inference.Request.ExcludeInstanceKeys`), used in
    `complete`, `streamSession.reselect` (guarded by `delivered`) and
    `proxyNative`.
  - **No vendor-account failover exists.**
- **Usage events:** `usage.Event.AccountID` written from `target.VendorAccountID`
  (`inference_complete.go:778`); no group column.
- **Client-facing limit relays (#413/#416/#418/#420):** passthrough forwards the
  served target's **live** response headers; the per-principal window caches
  (`ratelimit_window_cache.go`, `codex_window_cache.go`) are keyed by principal
  and record the account of the last observation. None of them read the stored
  snapshot.
- **Frontend:** dashboard `frontend/src/components/Dashboard.tsx` →
  `DashboardVendorUsage.tsx` → `VendorAccountUsageRows.tsx`; Anbieter page
  `VendorAccountsView.tsx` (nav item `providers`, `/providers`), usage panel
  `VendorAccountUsagePanel.tsx`; token editor `TokenList.tsx:905-960` with the
  **checkbox** `tokenVendorAccessAll` = "All own providers (incl. future)",
  helpers in `shared/VendorAccessEntryGroups.tsx`, `shared/vendorAccessAccounts.ts`.
- **Persistence:** migrations in `internal/store/migrate.go` (highest on main:
  **113**); memory store `internal/routing/memory_store.go`; vendor conformance
  tests `internal/store/vendor_account_conformance_test.go`,
  `routing_store_conformance_test.go` (`forEachRoutingStore`).

## 4. The group entity

| Field | Meaning |
|---|---|
| `id` | `vag_`-prefixed random hex. |
| `owner_user_id` | FK → `users(id)` `on delete cascade`. Groups are personal. |
| `vendor` | The vendor id all members share (`anthropic`, `openai`, `openrouter`, …). Fixed at create. "Same vendor" means equal `vendor_accounts.vendor`, so `google` and `google_native` cannot be mixed. |
| `name` | User-facing label. |
| `model_prefix` | The group's own namespace, same syntax rule as an account's `model_prefix` (§1 of the vendor doc). May be empty. |
| `status` | `active` \| `disabled`. A disabled group is neither listed nor routed. |
| `threshold_five_hour_pct` | 1..100, default 99. |
| `threshold_weekly_pct` | 1..100, default 99. |
| `threshold_credits_pct` | 1..100, default 99. Applies to the spend/credit categories that report a percentage (Codex Business spend control, OpenRouter/Kilo USD limit). |
| `created_at`, `updated_at` | As elsewhere. |

**Members** (`vendor_account_group_members`): `(group_id, account_id, position)`,
primary key `(group_id, account_id)`, `position` unique within the group,
`on delete cascade` from both the group and the account. An account may sit in
several groups.

**Adding a member** requires: same `vendor` as the group, and the account is
**reachable** by the group owner at save time (owned, or in the owner's
routing grant). This is a save-time check only; routing re-checks reach live
(§5), so a later loss of access never needs a data step.

**Ownership and sharing interplay.** If a shared member's owner lowers its
share level or removes the user's provisioning, the member row stays and is
skipped at routing; the management view marks it "not reachable". The user can
remove it. If the account is deleted, the row cascades away.

## 5. Routing

### 5.1 Token policy

`vendor_access` (JSON on `api_tokens`, no migration needed) gains:

```json
{ "all": false,
  "include_groups": false,
  "accounts": [ … unchanged … ],
  "groups": [
    { "group_id": "vag_…" },
    { "group_id": "vag_…", "prefix_override": { "enabled": true, "value": "" } }
  ] }
```

| Setting | Effect |
|---|---|
| `groups` entry | The token uses that group under the group's `model_prefix`, or the override. |
| `all: true` + `include_groups: true` | Every **active** group the token's principal **owns**, including groups created later, each under its own prefix. |
| `include_groups` without `all` | Ignored (the UI only offers it next to the "all" switch). |

A token opted into a group may use the group's members **through the group**
even when they are not listed individually under `accounts`. This is the
deliberate exception to "a shared account is used only when an entry names it"
(ADR-050): putting a shared account into a group is itself the user's named,
deliberate choice, and the group never widens reach (the grant still applies).

A service owns no group, so groups are for user tokens only. The portal session
principal is treated as `all + include_groups` over its own groups (it already
sees all own accounts); configurable chat-session access is project 2 (§13).

**Save-time checks** extend the existing ones (`portal.token_vendor_access_conflict`,
`portal.token_vendor_access_invalid`): a group id the principal does not own is
invalid, and the effective names of opted-in groups and accounts must not
collide. Names are claimed **per opted-in source**: a group claims
`group prefix + model` for the union of its members' models, an account claims
its own names. Members of one group naturally expose identical vendor model
ids; that is not a conflict, because the token opts into the group, not into
the members. A name shared by a group and **one of its own members** is not a
conflict either: the group shadows the member (§5.3). Any other shared
effective name is a conflict (another group, or an account outside the group).

Group save (create/update of the prefix or the members) refuses a prefix that
makes the group's names collide with the owner's own accounts **outside the
group** or with another own group's names (`vendor_account_group.prefix_conflict`),
so the common `all + include_groups` token never trips the token check by
accident.

### 5.2 Names and listing

A group advertises the **union** of the models its currently usable members
serve, as `prefix + upstream_model` (`upstream_model` is the vendor's own id,
which is identical across accounts of one vendor). The listing reuses the
account listing path (the prefix helper gains a group form), so a listed group
model is routable and vice versa. A model is listed under the union of the
dialects its members serve it on, as for accounts.

### 5.3 Member selection

For a request whose model matches a group's effective name, walk the members
by position and pick the first that is **eligible**:

1. reachable by the token's principal **now** (owned, or in the live grant);
2. account `status = active`, and for a subscription account
   `vendor_subscriptions_enabled` is on;
3. serves the requested `(upstream_model, dialect)`;
4. **not over a threshold**: for each category the member reports
   (percent `>= 0`), it is over when `pct >= threshold` **and** its reset time
   is unknown or in the future. A category whose reset time has passed counts
   as renewed (the stale snapshot is not trusted past its own reset);
5. **not in limit cooldown** for the requested upstream model (§6).

If no member is eligible but at least one passes rules 1–3, choose the member
that becomes **available again soonest**: for each such member, its "free at"
time is the **latest** reset among the categories that block it (and its
cooldown end); the member with the earliest "free at" is chosen; a member with
no known reset sorts last; ties keep position order. The request goes there and
the vendor's own answer (success or its limit error) is relayed under the
**existing** header rules (the rate-limit header entitlement and the
`send_usage_limits` token switch stay as they are), so Claude Code shows the
vendor's limit message and wait time where it is entitled to.

If no member passes rules 1–3, the group does not serve this model and
resolution continues as if the name were unknown to the group.

**Precedence.** Groups take part in the existing vendor branch and in
`vendor_account_routing_mode` (`vendor_first` / `fallback_only`) exactly like
accounts. **A group shadows its own members:** when a token uses both a group
and one of its member accounts under the same effective name (the typical
case: own Claude subscription with an empty prefix, a group with an empty
prefix that starts with that subscription, a token with
"all + incl. groups"), the group answers that name. This is safe because the
group serves that member first anyway, and it is what makes plain names like
`claude-opus-4-5` fail over without extra setup. Every other collision is
refused at save (§5.1); the routing backstop order is own groups (id order),
then own accounts, then shared accounts.

## 6. Failover on vendor limit errors

When the chosen member answers with a **limit error** before any response byte
was relayed to the client, the gateway:

1. scrapes the response headers into the member's snapshot (as for any served
   response);
2. puts the member into **cooldown for the requested upstream model** until the
   earliest of: `Retry-After`, the scraped reset of the rejected window, or a
   default of 60 s when neither is known. The cooldown is keyed by
   `(account id, upstream model)`, not by account: a per-model weekly window
   (e.g. Opus) can be days away, and an account-wide block would push the
   other models to the paid reserve while the subscription still has room.
   Account-wide exhaustion is already covered by the thresholds of §5.3, which
   apply to every model;
3. re-runs member selection without that member and retries the same request
   on the next eligible member;
4. stops when no eligible member is left, and relays the **last** vendor error
   unchanged.

Limit errors are HTTP 429, HTTP 402, and the vendors' quota error codes in a
non-streamed error body (e.g. OpenAI `insufficient_quota`, Codex
`usage_limit_reached`; VERIFY-LIVE). 5xx and 529 (`overloaded`) are **not**
limit errors in this project.

Cooldown is in-memory per gateway process (it only bridges the time until the
next scrape or pull tells the truth), keyed by `(account id, upstream model)`.

Retries are bounded by the number of members. Each attempt is a separate
upstream call; only the served attempt produces the request's usage event.
The request body must be replayable (buffered before dispatch).

**Where it hooks in.**

- *Passthrough* (Claude Code `/v1/messages`, Codex `/v1/responses`): in
  `proxyNative`, where the upstream status is already peeked before anything is
  written and a lost instance is already re-selected. A limit error on a group
  target excludes the member (a new `ExcludeVendorAccountIDs` on
  `inference.Request`, mirroring `ExcludeInstanceKeys`) and re-resolves.
- *Translate* (`/v1/chat/completions` and the translate modes): provider clients
  today collapse a vendor 429/402 into `ErrUnavailable`. They gain a classified
  **limit error** (status, `Retry-After`, the response headers for the scrape) so
  `complete` and `streamSession` (before `delivered`) can fail over the same
  way. The **final** error the client sees on the translate path stays as today
  (502 `provider.unavailable`); changing that mapping is out of scope.
- Failover applies only to targets that came **from a group**. A token that uses
  a single account behaves exactly as today.

## 7. Usage data

**Anthropic passive scrape:** no change needed; it already reads both windows
with their own reset times (#407, §3).

**Anthropic subscription active pull** (new, VERIFY-LIVE, ToS-restricted like
the whole subscription path): `GET https://api.anthropic.com/api/oauth/usage`
with the subscription's OAuth bearer and `anthropic-beta: oauth-2025-04-20`,
returning `five_hour` / `seven_day` objects with `utilization` (0..100) and
`resets_at` (ISO 8601). `five_hour` may be `null` (weekly-only plans). It is
written through the existing merge rule (§5.3 of the vendor doc).

**When pulls run**, for any account with an active pull (OpenAI subscription,
OpenRouter, Kilo, now Anthropic subscription):

- on the existing triggers (usage view open/refresh, models refresh, connect);
- for a **group member** that is skipped by a threshold whose reset time is
  unknown, or whose snapshot is older than its reset suggests, at most once per
  **15 minutes** per account, asynchronously (never on the request path), with
  exponential backoff after a 429 from the usage endpoint.

## 8. Usage attribution

Each usage event keeps `account_id` = the member that actually served the
request, and gains `vendor_group_id` (empty when not routed through a group).
The analytics views may filter by group later; this project only records it.

The client-facing limit relays (#413 Claude Code, #416/#418 Codex, opt-in via
#420): passthrough forwards the **serving member's** live headers. One change
is needed in the per-principal window caches (`ratelimit_window_cache.go`,
`codex_window_cache.go`): today `Attach` adds the principal's last cached
subscription windows to any response that carries none. For a response served
by a **group member that reports no windows** (an API-key reserve), nothing is
attached and the principal's cached entry is dropped, so the client never
shows, and never blocks itself on, the windows of a member that was skipped.
A member that reports windows overwrites the entry as today. No aggregation
across members.

## 9. Dashboard

The group cards sit next to the existing vendor usage cards
(`DashboardVendorUsage.tsx`). Per group the dashboard shows the group name, the **active member** (the member
the selection of §5.3 would pick, ignoring rule 3 so it is model-independent),
and one compact line per member:

- active;
- over threshold — which category, percent, "free at" time;
- cooldown — for which model, until when (after a limit error);
- not usable — disabled, needs reconnect, subscriptions switched off, or no
  longer reachable.

## 10. Management (portal "Anbieter" page)

Groups are managed under the existing **"Anbieter" / "Providers"** menu item,
next to the accounts:

- create, rename, enable/disable, delete a group; set its prefix; vendor fixed
  after create;
- three threshold fields (five-hour, weekly, credits), default 99 %, each with
  a hint that it applies only where the account reports that category;
- the member list with position (reorder), add from the accounts of the same
  vendor the user can reach (own and shared, shared ones labelled with their
  owner), remove;
- each member shows its current usage rows (reuse of the existing usage rows).

Token editor ("Provider access", `TokenList.tsx`): groups are listed alongside
accounts with the same checkbox + prefix override; the existing checkbox
"All own providers (incl. future)" gets a nested **"incl. groups"** checkbox
(enabled only while the first is ticked).

Everything is behind the existing `vendor_accounts_enabled` master flag; no new
feature flag.

## 11. Persistence and API

- New tables `vendor_account_groups` and `vendor_account_group_members`, new
  column `usage_events.vendor_group_id`, all three drivers, append-only
  migrations (next free number on main at merge time; 113 is the highest
  today and parallel branches keep claiming numbers), conformance tests
  (cascades on account delete, group delete, owner delete).
- The token policy extension (`groups`, `include_groups`) lives in the
  existing JSON column `api_tokens.vendor_provider_access`; the codec decodes
  tolerantly, so old rows read as "no groups".
- Portal endpoints under `/api/portal/vendor-account-groups` (CRUD, member list
  replace with order, read with member status). Error codes in a new
  `vendor_account_group.*` family. OpenAPI and API-surface docs updated.
- i18n keys de + en together.

## 12. Decisions taken by the designer (to be checked by the user)

- Groups are personal and cannot be shared (§2).
- A token opted into a group may use shared members through the group without
  naming them individually (§5.1).
- A group shadows its own members' names, so plain model names fail over
  without extra setup (§5.3).
- Limit cooldown is per `(account, upstream model)`, not per account (§6).
- After a switch to a member without windows (API key), the client is sent no
  cached windows of the skipped member (§8).
- The portal session uses all own groups until project 2 makes it configurable.
- "Active member" on the dashboard is model-independent (§9).
- Cooldown default 60 s; pull throttle 15 min; both in-memory.
- No new feature flag.
- Per-model weekly windows are left to the limit-error failover.

**Open — the user's decision:** shared members whose owner has **not** switched
on `share_usage_with_members` (vendor doc §1.3: the owner's opt-in that shows
the account's usage figures to the people allowed to use it).

## 13. Project 2 (follows this one; recorded, not designed here)

- The token list's synthetic **"Chat-Sitzung" / chat session** row gets the same
  provider-access editor as a token (own and shared accounts, groups, prefix
  override). Without a stored policy it stays at "all own" so nothing changes.
- When a token is selected in the portal chat, the model picker shows exactly
  the models that token may use, under the names it sees them by.
- When a renaming is active for that token (e.g. `claude_model_aliases`
  → `anthropic-local-<hash>`), the picker shows **"offered name (original name)"**.

# Portal Chat Vendor Access & Token-Scoped Model Picker — Design

Status: approved direction (user: "Erledige diese Aufgabe hier") · Branch:
`feat/portal-chat-vendor-access` (on `main`; #431 is merged) · Date: 2026-11-24

Branch-local working document (see `AGENTS.md`); durable content moves to
`docs/architecture/` before the pull request.

## 1. Intent

Two gaps remain after vendor account groups (#431):

1. The **token-less portal chat** always uses *all* of the user's own vendor
   accounts and groups. The user wants to configure, in the token settings,
   **which providers the portal chat may use** — exactly like a token, with
   prefix overrides.
2. When a **token is selected in the portal chat** ("Run as token"), the model
   picker still shows the session's list: models the token may not use, under
   names the token does not see. The picker must show **exactly the selected
   token's models under the token's own names**, and when a renaming is active
   for that token (generated Claude aliases, `anthropic-local-<hash>`), show
   **"offered name (original name)"**.

## 2. Requirements (user's words, binding)

- R-A: The synthetic "Chat-Sitzung" row in the token settings gets the same
  provider-access editor as a token: own and shared accounts, groups, prefix
  override, "All own providers (incl. future)" with "incl. groups". Without a
  stored policy everything behaves as today (all own providers and groups).
- R-B: With a token selected in the portal chat, the model picker lists only
  that token's models, under that token's effective names.
- R-C: When Claude model renaming is active for that token
  (`claude_model_aliases`), the picker shows `offered name (original name)`.

**Non-goals:** sharing the chat policy; service tokens (no chat); changing
run-time authorization (chat runs already act through `AuthorizeRunAsToken`);
revealing the target of **manual** override aliases (`model_override_map`
rows deliberately hide their target — they get no "(original)"); generating
Claude aliases for the session principal (stays "no aliases", routing doc
§2.4); the Models page / dashboards gaining per-token views.

**Anti-goals:** a chat policy must never widen reach (the grant and the
save-time checks still gate shared accounts); an old frontend saving only the
two capture booleans must never clear a stored policy; the token-scoped list
must never leak another user's token (ownership enforced server-side).

## 3. Code map (verified on the #431 feature head, content-identical to `main` after the squash merge)

- **Chat-session settings today**: two columns on `users`
  (`chat_log_communication`, `chat_secret`; `internal/store/models.go:109`,
  `sqlite_user.go:71`), saved by `PUT /api/portal/chat-settings`
  (`internal/gateway/auth.go:659` → `account.Service.UpdateOwnProfile`,
  `internal/account/service.go:701`, `UserUpdate` at :528). Read back by
  `sessionPrincipal` and by the synthetic token row.
- **Synthetic row**: `portal.Service.tokensForUser` puts a `TokenDTO` with
  `id "chat-session"`, `is_chat_session: true` first in
  `GET /api/portal/tokens`; it carries no `vendor_access` today. Frontend
  card: `TokenList.tsx` (~:1104-1166), saved via `api.updateChatSettings`
  (`api/auth.ts:64`), response `CurrentUser`.
- **Session principal**: `sessionPrincipal(user store.User, elevated bool)`
  (`internal/gateway/auth.go:310`), hard-codes
  `VendorAccess{All: true, IncludeGroups: true}`. Exactly two callers, both
  already holding the full user row: the cookie path
  (`authenticateWebSession`, :225) and the trusted loopback chat
  (`loopbackPrincipal`, :149). No caching; a policy column on `users` costs
  no extra query.
- **Run-as**: `AuthorizeRunAsToken` (`internal/portal/service.go:5982`)
  builds the run principal from the `api_tokens` row field by field,
  including `VendorAccess` and `ClaudeModelAliases`; gated on
  `token.ID == ""` + `X-OP-Run-As-Token` (`run_as.go:26-42`), used by the
  inference handlers only. Chat runs send the token id as
  `settings.run_as_token_id`; the backend sets the header on its loopback
  request.
- **Model listing**: the chat fetches `GET /api/portal/models`
  (`server.go:1827` → `portal_model_endpoints.go:21` → `Service.Models`
  :2996 → `modelsResponse(ctx, token, suppress=true)` :3024). Only query
  param today: `?manage=1`. Generated Claude alias rows appear in this
  listing with `ModelDTO.OriginalModelID` (json `original_model_id`) and
  `display_name`; manual alias rows never reveal their target; vendor rows
  carry no account/group metadata. No run-as mechanism exists on any portal
  listing.
- **Frontend**: `App.tsx` fetches `api.models()` and passes `models` into
  `ChatStoreProvider` as a prop; refetch happens on view changes, after every
  chat stream (`onRefresh`), a 5 s poll while the Models view is open, and a
  15 s `refreshModels()` poll only while a picked model is unavailable.
  `usableTokens` = active tokens with `gateway:use`. Changing
  `selectedTokenId` never refetches models. `SearchableSelect` options are
  `{value, label, loaded?, loadedTitle?}`; `ModelOption` has no
  `original_model_id` field yet.
- **Provider-access editor**: pure helpers (`shared/vendorAccessAccounts.ts`)
  and `shared/VendorAccessEntryGroups.tsx` are reusable; the glue (eight
  state hooks, `buildVendorAccess`, lazy account/group loads) is inlined in
  the token form (`TokenList.tsx` ~:197-436, JSX ~:969-1063);
  `ServiceTokensSection` duplicates a smaller variant.
- **Validation**: `validateVendorAccess(ctx, principal auth.Token, d
  *VendorAccessDTO)` (unexported, `service.go`), principal built by
  `userTokenPrincipal`; codecs `store.EncodeVendorAccess` /
  `DecodeVendorAccess` (`store/token_vendor_access.go`; the **zero policy
  encodes to ""** and "" decodes to the zero policy).
- **Migrations/tests**: highest migration on this branch is **114**; a users
  column is added via `addColumnIfMissing`; no guard test covers `users`
  columns — the raw column lists in `sqlite_user.go` and the user round-trip
  tests must be extended by hand. Four backend tests pin
  `sessionPrincipal`'s `VendorAccess{All:true, IncludeGroups:true}`.

## 4. Design

### 4.1 Chat-session vendor policy (R-A)

**Storage**: new column `users.chat_vendor_provider_access text not null
default ''` (migration **115**), the exact sibling of
`api_tokens.vendor_provider_access`, written through the existing profile
path (`account.UserUpdate` gains `ChatVendorAccess *string`, already-encoded
JSON; `UpdateOwnProfile` applies it) and read wherever the user row already
is.

**Semantics** — the column distinguishes three states:

| Stored value | Meaning | Session principal gets |
|---|---|---|
| `''` (default; every existing row) | never configured | `VendorAccess{All: true, IncludeGroups: true}` — today's behavior |
| any non-empty JSON | explicit policy | the decoded policy, verbatim |
| `{"all":false}` (written when an explicit save encodes to the zero policy) | explicitly "no vendor access" | the zero policy |

The save path therefore uses a wrapper (`encodeChatVendorAccess`): encode the
DTO; when the encoding is `""` (zero policy), store `{"all":false}` instead,
so an explicit "nothing" never collapses into "unset".

**Application**: `sessionPrincipal` decodes the column (tolerant decode, as
for tokens) and sets the principal's `VendorAccess` accordingly. Both callers
(cookie session, trusted loopback chat) inherit it, so the token-less chat,
the session's model listings and session-cookie inference all follow one
rule: **the chat session behaves exactly like a token whose policy is the
stored one, defaulting to "all own incl. groups"** (designer's call: one
rule, no special-casing per page; the vendor doc §11.4 sentence is updated).
Run-as requests are untouched — `AuthorizeRunAsToken` replaces the whole
policy with the token's, as today.

**Save**: `PUT /api/portal/chat-settings` gains an optional field
`vendor_access` (same wire DTO as tokens). Absent field → stored policy
untouched (backward compatible). Present → validated like a token policy
(`validateVendorAccess` with `userTokenPrincipal(auth.Token{UserID})`, same
error codes `portal.token_vendor_access_invalid` / `_conflict`), encoded,
stored. A new exported portal method wraps validate+encode so the gateway
handler keeps calling `account.UpdateOwnProfile` for persistence.

**Read-back**: the synthetic chat-session `TokenDTO` carries `vendor_access`
= the **effective** policy (`{all:true, include_groups:true}` when unset), so
the editor pre-fills truthfully with no extra "unset" state in the UI.

**UI**: the chat-session card gets the same provider-access editor as the
token form. To avoid a third copy of the glue, the token form's section is
extracted into a shared `VendorAccessEditor` component (own + shared
accounts, groups, all/incl.-groups, prefix overrides); the token form and the
chat card both use it (`ServiceTokensSection` stays as-is). i18n de+en,
including a chat-specific note ("gilt für den Chat ohne Token" /
"applies to the chat without a token").

### 4.2 Token-scoped model listing (R-B)

`GET /api/portal/models` gains the query parameter **`run_as_token=<token
id>`**:

- Only a **session** caller (`token.ID == ""`) may use it; it runs the same
  gate as chat runs (`AuthorizeRunAsToken`), so ownership, status and scope
  checks — and their error answers — are identical to starting a chat run
  with that token. A bearer caller sending the param is refused the same way
  the run-as header is refused elsewhere.
- On success the listing is computed for the run-as principal:
  `Models(ctx, runAs)` — vendor prefixes, visibility overlays, manual
  override rules and generated Claude aliases (with `original_model_id`) all
  follow from the existing code with **no listing change**.
- `?manage=1` and `run_as_token` are mutually exclusive (400 on both).

### 4.3 Chat picker follows the token (R-B, R-C)

- `api.models` gains an optional `runAsTokenId`.
- `ChatStore` keeps receiving the session `models` prop (now already
  policy-filtered by 4.1 for the token-less case). When `selectedTokenId` is
  non-empty, `ChatStore` fetches and holds a **token-scoped list** and every
  derived consumer (chatModels, capability gates, voice-chat candidates,
  synthetic saved-model entry, the 15 s unavailable poll, sticky picks) reads
  the effective list; when empty, it falls back to the prop. Refresh points
  mirror today's: on token change, after every run (`onRefresh` already
  refetches the prop; the store refetches its scoped list then too), and in
  the 15 s unavailable poll. Fetch failures keep the last list and surface
  the existing global error path; while the first scoped fetch is in flight
  the picker shows a loading state, not the session list (no flash of models
  the token may not use).
- **Labels**: `ModelOption` gains `original_model_id?`. The picker label
  becomes `display_name (original_model_id)` whenever a row carries an
  `original_model_id` different from its offered name — for any principal
  (this is exactly the generated-alias case; manual aliases never carry it).

## 5. Decisions taken by the designer

- One policy for the whole session principal (chat, session listings,
  session-cookie inference), not a chat-only special case (§4.1).
- Storage as a `users` column with the `''`-vs-`{"all":false}` distinction
  (§4.1), reusing the token codec.
- Run-as on the existing `/api/portal/models` via query param, not a new
  endpoint and not the inference header (§4.2).
- The whole chat list swaps per token (voice pickers included), matching what
  a run with that token can actually use (§4.3).
- `VendorAccessEditor` extraction instead of a third glue copy (§4.1).
- Migration number **115** (first free above this stack; renumber at rebase
  if taken — same rule as #431).

## 6. Risks

- Narrowing the session policy narrows the session's own `/v1/models` and
  dashboard live routes (intended: "like a token", default unchanged).
- `portal.API` is hand-edited with a regenerated tracing wrapper — keep the
  SPDX header (regression in #431 history).

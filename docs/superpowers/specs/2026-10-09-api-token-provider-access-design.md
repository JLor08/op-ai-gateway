# Design — Per-API-token provider (vendor-account) access + prefix override

Date: 2026-10-09
Branch: `feat/api-token-provider-access` (off `main` 9ecaf25)
Status: design, pending user review

## 1. Goal

Let each **personal API token** opt in to which of its owner's **vendor accounts**
("Anbieter") it may use, and give each selected account an optional model-name
**prefix override**. Enabling the override with an empty value exposes that account's
models under their original names with **no prefix**.

This is strict opt-in: a token sees/routes to a vendor account only if it (a) has the
"all providers" switch on, or (b) explicitly lists that account.

## 2. Terminology (as used in this codebase)

- **Anbieter / provider** = `routing.VendorAccount` — a user-owned external OpenAI /
  Anthropic account (api-key or subscription). The portal nav item "Anbieter" (`i18n`
  key `providers`) renders `VendorAccountsView`. ADR-049 keeps this deliberately distinct
  from the overloaded "provider" adapter kind. This feature concerns **only** vendor
  accounts.
- **model_prefix** = `VendorAccount.ModelPrefix` (`routing/store.go:240-246`). The
  account-level namespace: `gateway_model = model_prefix + upstream_model` (direct
  concatenation; the separator, if any, is part of the prefix string). Empty prefix =
  bare upstream slug. This feature adds a **per-token override** of this prefix.
- **gateway_model** = public, client-facing model id a caller requests and a listing
  advertises. **upstream_model** = the raw slug sent to the vendor
  (`VendorAccountModel.GatewayModel` / `.UpstreamModel`, `routing/store.go:248-263`).
- **api token / auth.Token** = the client bearer; persisted `store.TokenRecord`
  (`store/models.go:117-169`), resolved at request time to `auth.Token`
  (`auth/token_store.go:27-73`).

## 3. Scope

In scope: personal API tokens (`TokenList`). Everything behind the existing
`vendor_accounts` experimental flag; flag off ⇒ feature invisible and inert.

Out of scope:
- Service tokens / chat-session pseudo-tokens — they own no vendor accounts
  (`token.UserID == ""` ⇒ `ownVendorAccountModels` returns nil), so the feature is moot.
- Self-hosted servers/adapters — they have no prefix concept; a token-level prefix there
  would be a new namespacing mechanism (explicitly deferred, "variant B").
- An "all except X" mode.

Self-hosted models remain fully visible/usable regardless of a token's vendor access.

## 4. Decisions (locked in brainstorming)

1. Granularity: **per vendor account** (not per vendor openai/anthropic).
2. Default/migration: **strict**. Existing tokens migrate to `{ all: false, accounts: [] }`
   — **no** automatic vendor-account access. Same default as new tokens. (Breaking for any
   token currently using vendor accounts; acceptable under the experimental flag. The
   operator reconfigures their own tokens.)
3. **"All providers (incl. future)" switch** per token (`all`), default off — grants access
   to all of the owner's active accounts under each account's own `model_prefix` (no
   per-token override, future accounts auto-included).
4. Storage: **single JSON column** on `api_tokens` (not a junction table).
5. Collision policy: with `all=false`, **reject at save time** any config whose effective
   public names collide across the selected accounts (clear 400). With `all=true`, keep
   today's behavior (first-wins + dedup; future accounts can't be pre-checked).

## 5. Data model

New JSON column `vendor_provider_access` on `api_tokens`. Shape:

```json
{
  "all": false,
  "accounts": [
    { "account_id": "<vendor account id>",
      "prefix_override": { "enabled": true, "value": "" } }
  ]
}
```

- `all` (bool) — true ⇒ every active owner account, each under its own `model_prefix`;
  `accounts` ignored.
- `accounts[]` — only meaningful when `all=false`. Each entry:
  - `account_id` — must be an account the token owner owns.
  - `prefix_override.enabled=false` ⇒ use the account's own `model_prefix`.
  - `enabled=true, value="<p>"` ⇒ use `<p>` as the prefix (replaces the account prefix).
  - `enabled=true, value=""` ⇒ **no prefix** (bare `upstream_model`).
- `all=false, accounts=[]` ⇒ no vendor-account models (new-token default).

Carried on `auth.Token` as a decoded struct. Encode/Decode/`Auth*` trio mirrors
`store/token_override.go` (the `model_override_map` precedent). Nullable/absent column
decodes to the strict default `{ all:false, accounts:[] }`.

### Token-effective prefix (shared helper — the parity contract)

One function used by **both** listing and routing so advertised names == routable names:

```
tokenEffectivePrefix(token, account) -> (prefix string, allowed bool)
  all == true            -> allowed=true,  prefix = account.ModelPrefix
  entry for account.ID   -> allowed=true,  prefix = entry.enabled ? entry.value
                                                                   : account.ModelPrefix
  otherwise              -> allowed=false
```

For `all=true` this yields `prefix = account.ModelPrefix`, i.e. identical to today — no
regression on that path.

## 6. Backend

Enforcement MUST be in both the listing and the routing path (the codebase's recurring
"listing ≠ access control" invariant).

### 6.1 Persistence
- `store/models.go` `TokenRecord`: add the decoded field.
- `store/migrate.go`: new migration (next number, currently 85) via `addColumnIfMissing`;
  baseline `CREATE TABLE` gains the column. All rows default to the strict JSON (`all:false`).
- New `store/token_vendor_access.go`: `Decode`/`Encode`/`AuthVendorAccess` trio
  (rollback-safe JSON) — pattern from `store/token_override.go:34/96/132`.
- `store/sqlite_token.go`: `tokenColumns:110`, insert list `50-52`, update stmt `221-222`,
  `scanToken:377`, and the `auth.Token` build in `LookupBearer:280` (around `:343`) read
  the new column into the new field (no extra query).
- `auth/token_store.go`: add the field to `auth.Token:27-73`; clone it in the
  `cloneStrings`-style helpers (`:86-105`) and in `AddPlainToken`/`UpdateToken`.
- `portal/memory_directory.go`: mirror in the three build sites (`:129/:191/:300`) and
  `routing/memory_store.go`, so the memory driver (tests/dev) matches SQLite.

### 6.2 Listing (display)
- `portal/service_vendor_listing.go`: `ownVendorAccountModels:25` filters the owner's
  active accounts to those `allowed` for the token; `vendorModelFlavorSets:70` /
  `overlayVendorModels:135` build each public name with `tokenEffectivePrefix` instead of
  the stored `m.GatewayModel`. `vendorDashboardRoutes:103` gets the same relabel for
  dashboard parity.
- Reuse the isolated relabel rule `prefix + upstream_model`
  (`service_vendor_discovery.go:760-764`) with the token-effective prefix.
- `overlayVendorModels` is the single choke point feeding both `/v1/models`
  (`ModelsForFlavor`) and the `Models()` DTO path, so one change covers OpenAI, Anthropic
  (`service.go:2562`), and LM Studio listings.
- Ordering note for the plan: the vendor overlay/relabel must run **before**
  `applyOverrideAliases` (`service_model_offering.go:117`, wired at `service.go:4210`) so a
  token's `model_override` `Offer`/`HideTarget` aliases operate on the token-relabeled
  vendor names.

### 6.3 Routing (the access boundary) + reverse map
- `routing/resolver.go`: `resolveVendorAccount:823` skips non-`allowed` accounts (extend
  `vendorAccountRoutingEligible:884`). `vendorAccountModelMatch:870` changes from
  `m.GatewayModel == req.Model` to: compute `tokenEffectivePrefix` for the account, and
  match `effectivePrefix + m.UpstreamModel == req.Model`; on hit, `vendorAccountTarget:932`
  sets `Target.Model = req.Model` (public) and `Target.ProviderModel = m.UpstreamModel`
  (upstream) as today. This is the **reverse map** (client sends the token-prefixed name →
  strip → dispatch the raw slug). Keep it inside the resolver's vendor path, independent of
  the generic `resolveModelOverride`.
- Interaction with `model_override`: `resolveModelOverride` (`inference_handlers.go:223`)
  runs before `Resolve`, rewriting `req.Model`; the vendor reverse-map then applies to the
  already-rewritten name. An alias whose target isn't exposed by the token's vendor access
  simply won't resolve — acceptable, documented.

### 6.4 Validation (config time)
Alongside `validateModelOverrideRules` (`portal/service.go:1829`), in `CreateToken:1489`
and `UpdateToken:1564`:
- every `account_id` must belong to the token owner (reject otherwise);
- each override `value` validated with the existing
  `normalizeVendorAccountModelPrefix` (`service_vendor_accounts.go:234`): ≤64 bytes,
  charset `A-Za-z0-9-_.~:/@+`, no `..`;
- **collision check** (`all=false`): build the set of effective public names across the
  selected accounts' current models; a duplicate ⇒ 400 naming the colliding name.
  New error code (e.g. `vendor_account.token_access_conflict`), mapped in
  `portal_token_endpoints.go:182`.

### 6.5 DTO / API
`TokenDTO:988`, `CreateTokenRequest:1046`, `UpdateTokenRequest:1079`, `tokenDTO:4527`
gain the field (pointer-optional on update, matching the existing partial-update
semantics). `AuthorizeRunAsToken:4503` carries it where relevant.

## 7. Frontend (`gateway/frontend`, flag-gated)

- `components/TokenList.tsx`: new form section between the model-override editor (~`:548`)
  and the project picker (~`:608`):
  - Checkbox **"Alle Anbieter (auch künftige)"** (`all`).
  - When off: a list of the owner's connected accounts (`shared/CheckboxGroup.tsx` pattern);
    per checked account a **"Prefix überschreiben"** toggle + text field (empty = no prefix,
    with help text). Controlled-editor pattern like `shared/ModelOverrideEditor.tsx`.
  - Lazy-load the account list on form open via `api.vendorAccounts()` with a latest-wins
    ref guard (mirror the `myProjects()` effect, `TokenList.tsx:163-174`). Hide the whole
    section unless `vendorAccountsEnabled`.
- Plumb `vendorAccountsEnabled` through `components/views.tsx:141-159` (it already exists in
  the render ctx).
- `api/tokens.ts`: add the field to `PortalToken:17`, `CreateTokenRequest:66`,
  `UpdateTokenRequest:94`. Reset/hydrate in `openCreate`/`openEdit`; include in
  `submitCreate`/`submitEdit` bodies.
- `i18n.ts`: new keys in both `de` and `en` (type parity enforced).
- `ServiceTokensSection.tsx`: not touched (service tokens own no accounts).

## 8. Docs

- `docs/architecture/cross-cutting/external-vendor-accounts.md`: new section — per-token
  provider access + prefix override, the `tokenEffectivePrefix` parity contract, the
  strict-opt-in default.
- `docs/architecture/.../api-surface.md` + `openapi.yaml`: the new token DTO field + error
  code.
- New **ADR-050**: per-token provider access & prefix override (strict opt-in, JSON column,
  enforce in listing + routing, collision = reject).
- Token/data-model architecture docs touched where the `api_tokens` shape is described.

## 9. Testing & verification

TDD RED→GREEN. `routing/resolver_vendor_prefix_test.go` is the template for the
reverse-map tests.

Backend:
- Decode/Encode roundtrip incl. the three override states (off / on+value / on+empty) and
  absent-column → strict default.
- Migration: existing rows → `{all:false}`; new rows default strict.
- `LookupBearer` populates the field (SQLite + memory driver parity).
- Listing: `all=true` (all accounts, native prefix), explicit list, empty (none);
  prefix relabel incl. empty = bare names.
- Routing: account filter; reverse map for all three override states; `all=true` unchanged
  vs today; a de-opted account's model 404s even if requested directly.
- Validation: foreign `account_id` rejected; bad prefix rejected; collision (`all=false`)
  rejected with the new code.
- Service token unaffected; flag-off no-op.

Frontend: vitest for the new section (toggle, account list, per-account override,
empty = no prefix), create/edit round-trip, flag-gated hidden.

Gates before PR: Go build/vet/`go test ./...` + golangci-lint (gofumpt/gocritic);
frontend vitest + build + lint + **prettier format:check**; `lint-docs`;
**Postgres leg** (provision the DSN — this is a store/migration change);
**SonarQube gate** (judge by branch-attributed findings).

## 10. Open edge cases (resolved)

- **Dangling `account_id`** after an account is deleted: harmless — listing/routing filter
  to active owner accounts, so it matches nothing. No FK/cascade needed (JSON). Optional:
  prune on account delete (not required for correctness).
- **De-prefixed vendor name colliding with a self-hosted model name**: existing
  `vendorMode` ordering (vendor_first / fallback_only) already decides precedence at
  resolve; listing already unions same-named flavors. No new rule needed; documented.
- **`all=true` collisions** (two accounts, same native prefix + slug): unchanged — today's
  first-wins + dedup.

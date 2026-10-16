# Design: shared vendor accounts via resource groups (#183 Phase 2)

Status: draft for review. Branch-local (removed before the PR; durable content folds
into `docs/architecture/`). This revision folds in a four-dimension adversarial review.

## 1. Goal

Make a vendor account (today strictly personal, owner-only) shareable with other users
and services, modelled on how AI servers are managed and provisioned. An **admin owner**
raises their own account's **share level**, assigns it to **management groups** and
**resource groups** (like an AI server), and lowers it again. A principal provisioned
into a resource group that contains a shared account, who also **opts in**, may use it; a
management-group co-manager may operate it (and, at the top level, fully manage it);
personal accounts are unchanged. Phase 2 of ADR-049 ("a sharing link table plus a
resolver filter extension"). It is built in ordered phases and delivered as ONE PR (§17), under `vendor_accounts_enabled`
and a new `vendor_account_sharing_enabled` master flag, with subscription sharing behind
its own default-off flag.

## 2. The decisive decisions

1. **Share level is an enum**: `vendor_accounts.share_level ∈ {none, shared, full}`. `none`
   = today's personal account, never in the pool. `shared`/`full` enter a server-like
   model. The level sets how much a co-manager may do (§6).
2. **Management/assignment mirror AI servers; routing eligibility does NOT.** A shared
   account is assigned to management groups and resource groups like a server (§5, §9),
   with the `system_group_id` containment root and a new `can_manage_vendor_accounts`
   co-manager permission mirroring `can_manage_servers`. But **routing/usage eligibility is
   a PURE GRANT, not the server open-pool rule** (§8): usable iff owner OR the principal is
   provisioned into a resource group that contains the account — **fail-closed**. A shared
   account in no resource group is usable **only by its owner** (the operator's intent).
   This deliberately diverges from servers: `AllowedServerIDs` makes a server in no
   provisioned RG available to *everyone* in opt-in mode (`allowed[id] || !restricted[id]`,
   verified); copying that for a credential would leak it.
3. **The per-token opt-in still gates actual use, and is required for shared accounts.**
   Availability (owner or provisioned via RG) is necessary, not sufficient: a token (user
   or service) must **explicitly opt into the account by name** in its
   `vendor_provider_access` policy, with an optional prefix override (ADR-050). For
   **shared** accounts the opt-in is **always per-account**: `all:true` (and a portal
   session, which is always `all:true`) covers only the principal's **own** accounts, never
   borrowed shared ones (§8). So borrowing another owner's credential is always a deliberate,
   named choice.

## 3. Scope

**In:** `share_level` + lifecycle (raise/lower); `can_manage_vendor_accounts` end to end;
`vendor_account_admin_groups` + `vendor_accounts.system_group_id`;
`resource_group_vendor_accounts`; the share-level authorization model (§6); the pure-grant
routing filter + the routing enumerator (own ∪ shared-via-RG) feeding routing and the
model listings in lock-step, distinct from the wider management listing (§8); the
vendor-access opt-in extended to name reachable shared accounts for user tokens and
**newly added** to service tokens; the management + RG-assignment endpoints (owner
self-service and RG-manager paths, mirroring servers); the member key-free read + a
`shared_by`/`is_owner` indicator; `vendor_account_sharing_enabled` and
`vendor_subscription_sharing_enabled`; portal UI; docs/OpenAPI.

**Out:** per-borrower usage snapshots/limits (keep one `vendor_account_usage` snapshot per
account; per-event attribution is enough); any change to personal-account behavior or to
how servers work; sharing to principals outside the resource-group mechanism; an
ownership-transfer operation (noted as a follow-up in §4).

## 4. Data model

Next free migration is **90** (branch and origin/main tail = v89). Three migrations,
shaped on the server/RG ones; booleans are `integer not null default 1`, FKs cascade, FK
enforcement is on (verified) on both drivers.

- **Migration 90 — co-manager permission.**
  `user_group_managers.can_manage_vendor_accounts integer not null default 1` (copy
  `migration49Up`). The default grandfathers every existing co-manager, like the other five.
- **Migration 91 — the share model.** `vendor_accounts.share_level text not null default
  'none'` and `vendor_accounts.system_group_id text not null default ''` (the containment
  root, opaque, no FK, as on `ai_servers`), plus `vendor_account_admin_groups(vendor_account_id
  FK→vendor_accounts on delete cascade, group_id FK→user_groups on delete cascade, created_at,
  unique(vendor_account_id, group_id))` (copy `migration50Up`), index on `group_id`.
  `share_level` is `text` by the precedent of the table's existing free-text enums
  `auth_type`/`status` (migrate.go ~3885) — three named states read better than two bools.
  (ADR-005 is irrelevant here: it is only about `integer`/`real` truncating wide Go values.)
  Default `'none'` backfills every account to personal — no behavior change.
- **Migration 92 — the RG link.** `resource_group_vendor_accounts(resource_group_id
  FK→resource_groups on delete cascade, vendor_account_id FK→vendor_accounts on delete
  cascade, created_at, unique(resource_group_id, vendor_account_id))` (copy `migration54Up`),
  index on `vendor_account_id`.

Store work mirrors `ServerStore`/`ResourceGroupStore`: on `VendorAccountStore`
(`internal/routing/store.go`) `UpdateVendorAccountShareLevel`, `UpdateVendorAccountSystemGroup`,
`SetVendorAccountAdminGroup`, `RemoveVendorAccountAdminGroup`, `VendorAccountAdminGroups`,
`VendorAccountsByAdminGroups`, **and a single-transaction `ClearVendorAccountSharing(id)`**
(§7); on `ResourceGroupStore` `SetResourceGroupVendorAccount`, `RemoveResourceGroupVendorAccount`,
`ResourceGroupVendorAccounts`, `ResourceGroupsByVendorAccount`. SQL, the
`UserGroupManagerPerm` flag, conformance tests on all three drivers, regenerated
`routingstore_gen.go`.

**Memory-store cascades (keyed `rgID → accountID`, like `resource_group_servers`):**
`DeleteVendorAccount` cascades `vendorAccountAdminGroups` by single-key delete **and**
`resourceGroupVendorAccounts` by a **reverse-direction loop over every RG** (`for _, accts
:= range m.resourceGroupVendorAccounts { delete(accts, id) }`, the `deleteServer`
pattern at memory_store.go:422). `DeleteResourceGroup` must additionally
`delete(m.resourceGroupVendorAccounts, id)`, mirroring its existing
`delete(m.resourceGroupServers, id)`. Conformance asserts both on all three drivers. As
with `server_admin_groups`, a **user-group delete** cascades `vendor_account_admin_groups`
on SQL only; memory mode leaves a dangling admin-group entry — harmless, because the
routing grant is keyed on `resource_group_vendor_accounts` + live `share_level`, never on
admin-group membership.

`vendor_accounts.owner_user_id` stays `not null … on delete cascade` (single owner). The
**consequence**, sharpened by sharing: deleting one user hard-deletes every vendor account
they own — including a `full`, org-shared account — and revokes every borrower, with no
ownership-transfer path (servers use many-to-many `server_owners` to survive one owner's
deletion; a credential deliberately has exactly one owner). Documented in §12; an
ownership-transfer operation is a named follow-up, out of Phase 2.

## 5. The `can_manage_vendor_accounts` permission (mirror of `can_manage_servers`)

A sixth per-co-manager boolean on `user_group_managers`, threaded exactly like
`can_manage_servers`: predicate `vendorAccountManageGroupIDs` (a one-line wrapper over
`manageGroupIDs`); `UserGroupManagerPerm.CanManageVendorAccounts`; the SQL read/write +
memory mirror (default-true new row); DTO flags on `UserGroupDTO`/`UserGroupMemberDTO`
filled in all four `groupDTO` branches and `GroupMembers`; the extra positional bool on
`PromoteManager`/`SetManagerPermissions`; `portal.API` + `groupManagerRequest` + both
handlers; regenerated `api_tracing_gen.go`; the Groups UI column + DE/EN i18n.

**Containment root.** Like a server, a shared account's management groups must share one
parent system group, which becomes its `system_group_id`. It is **immutable while the
account remains shared/full**: set at the first raise-from-`none`, and cleared only on a
full downgrade to `none` (§7). A shared account and an RG link only when their roots match.
Edge: FK cascade may delete the last `vendor_account_admin_groups` row while the account
stays `shared` with a non-empty root — the account then has zero management groups, remains
owner-manageable and downgradable, and re-adding a management group must match the retained
root (or a downgrade-to-`none` resets it).

## 6. Authorization — by share level

"CM" = a co-manager holding `can_manage_vendor_accounts` in a management group the account
is assigned to. "Member" = a principal provisioned into a resource group that contains the
account (and opted in). **System scope stays read-only and never bypasses a write** (§8 of
the vendor doc: credential-using operations send the owner's sealed key upstream).

| Operation | `none` | `shared` | `full` |
|---|---|---|---|
| Read (key-free DTO); use via RG+opt-in | owner (+ system read; member) | owner + CM + member | owner + CM + member |
| Operate: status, model_prefix, model refresh, connection test, usage/limits refresh, rename | owner | **owner + CM** (¹) | owner + CM |
| Credential: connect / reconnect / device-connect / set+change api_key / sealed key | owner | **owner-only** | **owner + CM** |
| Management-group assignment; resource-group link (both mirror servers) | — | **owner-only** | **owner + CM** |
| Delete | owner | **owner-only** | **owner + CM** |
| Raise level (none→shared→full) | owner+admin | owner+admin | — |
| Lower level / downgrade | — | **owner-only** | **owner + CM** |

(¹) **Subscription exception (§11):** for a *subscription* account, the operate ops that
can rotate the single-use OAuth token — connection test, model refresh, usage refresh — are
**owner-only even at `shared`** (a CM may still read and change status/prefix/rename).

So `full` = complete owner-equivalent delegation: a CM may do everything the owner can,
including credential, group/RG assignment, delete, and downgrade; only **raising** the
level (none→shared→full) is owner+admin, and it happens at `shared`/`none` where a CM has
no management reach. The owner's standing lever is that they can always downgrade (and, by
owning the record, cannot be removed).

Service-layer shape:

- Two write gates (plus read): `authorizeVendorAccountOperate` (owner OR CM — the operate +
  rename + management + RG + lower-at-full rows) and `authorizeVendorAccountCredential`
  (owner-only at `shared`; owner OR CM at `full` — the credential rows + delete + downgrade).
  Management-group and RG assignment use the **operate** gate but are additionally restricted
  to `full` (owner-only at `shared`).
- **PATCH is field-level, deny-by-default:** `name`/`status`/`model_prefix` → operate gate;
  **every other field, including `api_key` and any credential- or endpoint-sensitive field,
  → credential gate** (never let an overlooked field fall through to the operate gate).
  `base_url`/`auth_type` stay immutable-or-create-only, stated as an invariant.
- Credential-gated methods are enumerated exhaustively (connect, reconnect, device-connect,
  import, begin/complete connect, api_key set/change), not an illustrative list.
- Raising the level and the **first** management-group assignment are owner+admin. Every
  management-group PUT must keep **≥1 group** (reject empty) while the account is
  shared/full. Downgrade requires only **ownership** (not current admin role) at `shared`,
  or owner-or-CM at `full` — an owner must be able to un-share even after losing admin role.
- Non-owners who are not CM or member keep the **404-no-leak** (`ErrVendorAccountNotFound`).
- **Management ≠ usage:** a CM who is not RG-provisioned can read/operate but cannot route
  through the account; to USE it a CM must still be RG-provisioned and opted in, like anyone.
- **Response hygiene:** every CM-reachable operate endpoint (test/refresh) scrubs its result
  and error payloads of any api_key/access/refresh token material — a CM must never see the
  raw secret even in an echoed upstream error. A test asserts it.

## 7. Lifecycle (raise / lower)

- **Raise to `shared`/`full`:** owner+admin, on an owned account, carrying ≥1 management
  group (establishing `system_group_id`; a non-system caller may name only groups in
  `vendorAccountManageGroupIDs`; all share one parent). A **subscription** account may be
  raised only to `shared`, never `full` (§11), gated by `vendor_subscription_sharing_enabled`.
- **Resource-group links:** owner self-service (join/leave an RG the owner is a member of,
  like `AddServerToResourceGroup`) at `shared`+; RG-manager side (an RG manager with
  `can_manage_resources` adds the account, additionally gated by `authorizeVendorAccountOperate`
  + the `full`-only restriction + matching root) — both mirror `resource_group_servers`.
- **Lower / downgrade:** owner-only at `shared`, owner-or-CM at `full`. Lowering to `none`
  calls the single-transaction **`ClearVendorAccountSharing(id)`**: in one tx it sets
  `share_level='none'`, deletes all `vendor_account_admin_groups` and
  `resource_group_vendor_accounts` rows, and clears `system_group_id`. The ordering within
  the tx sets `share_level='none'` first, so even a torn write leaves the account
  owner-only. A downgrade takes effect on the next resolve; the routing grant re-reads live
  `share_level` per resolve (§8), so a dangling link never grants access.

## 8. Routing and usage eligibility (the pure grant)

Today `resolveVendorAccount` enumerates only `VendorAccountsByOwner(token.UserID)` and
requires `token.UserID != ""`. Phase 2 introduces **two** enumerators and the grant.

- **Routing/model-listing enumerator (parity-bound, one source):**
  `VendorAccountsForRouting(principal)` = the principal's **own** accounts (emitted **first**,
  id-sorted) **then** the accounts that are (a) `share_level ∈ {shared, full}` **read live in
  the same pass** AND (b) linked to a resource group the principal is **provisioned into**
  (id-sorted). It replaces the owner-only read in `resolveVendorAccount` **and** in
  `ownVendorAccountModels` / `TokenVendorPrefix` validation, so advertised == routable.
  **Live share_level is part of the predicate** — link existence is necessary, not sufficient;
  a `none` account with a surviving link grants nothing.
- **Management listing (wider, not parity-bound):** non-system `ListVendorAccounts` returns
  own ∪ `VendorAccountsByAdminGroups(managed)` ∪ the member (routing) set — the full union, so
  a CM sees accounts they manage even without RG provisioning, and a member sees every account
  they can route. Member-only rows get the key-free DTO (§10). This is a **different** set from
  the routing enumerator and is explicitly not used for routing.
- **Grant (pure, fail-closed on the shared arm):** available iff owned OR (live
  shared/full AND provisioned via a containing RG). Does **not** read
  `resource_provisioning_enforce` and does **not** use the server open-pool branch. The
  **owner's own accounts are always returned**; only the **shared arm** fails closed on a
  store error (deny the shared candidates, keep own) — one consistent fail direction across
  routing and the model listing, so parity holds even in the error window (unlike
  `AllowedServerIDs`, which fails open).
- **Opt-in required, per-account for shared (ADR-050, §2.3):** the token's
  `vendor_provider_access` must name the account. `all:true` (and portal sessions) include
  only **own** accounts; a **shared** account is reached only by an explicit per-account entry
  (with optional prefix override). `validateVendorAccess`/`ownerVendorAccountsByID` are
  extended so a policy may name a **reachable** shared account, where reachability is computed
  against the **token's own principal** (the owning user for a user token, the service identity
  for a service token) — not the editing admin. Naming a non-reachable shared account is
  harmless (routing re-checks and denies), so the save-time check is advisory; routing is the
  sole security gate. The own-versus-shared line is `TokenOwnsVendorAccount` (the account's
  owner is the token's user; a service owns nothing), used by routing, the listing and the
  save-time check alike. An `all:true` policy may also carry explicit entries: an entry naming
  an own account is ignored (all wins, as before sharing), an entry naming any other account is
  a live opt-in to a shared one and is validated like an `all:false` entry (an unreachable id
  is refused as an unknown one, so it is no longer stored inert).
- **Service tokens (A = option a):** a service reaches a shared account only when the
  **service is directly provisioned into a containing resource group** (`ProvisionKindService`)
  — exactly how a service reaches servers; a service is in no management/user group, so there
  is no group path. The eligibility guard becomes `token.UserID != "" OR (token.IsService()
  && token.ServiceID != "")` — never merely drop the `UserID != ""` check. Service-token
  vendor access is **new plumbing** (§3): add `VendorAccess` to `CreateServiceTokenRequest`
  and `ServiceTokenDTO`, write `VendorProviderAccess` in `CreateServiceToken`, preserve it in
  `RotateServiceToken`; the opt-in is **create-only** for a service token (changing it recreates
  the token), matching the existing "a service token has no field-level update" model — stated
  explicitly.
- **Enumerator seam (routing cannot import portal):** exposed like `ProvisioningGate`, but its
  nil/unwired behavior is **fail-closed to owner-only** — a nil seam falls back to
  `VendorAccountsByOwner(token.UserID)` (own accounts only), **never allow-all** (contrast
  `ProvisioningGate`, whose nil = allow-all).
- **Precedence & parity:** the own-block-first ordering makes the owner's own account win a
  same-name/prefix collision; among shared accounts the first active by id wins. The resolver
  chooses per **(name, dialect)**, not per name: an account that does not serve the requested
  dialect (an OpenAI subscription for the anthropic dialect) is skipped and the request falls
  through to the next account serving the name. So the per-(name, dialect) winner is the first
  account, in that order, that serves the name **on that dialect**, and the listing's per-name
  flavor set is the **union** of the flavors served by the eligible accounts for that name:
  each dialect in it is backed by its winner, so an advertised name is routable on exactly the
  dialects advertised, and no dialect that is routable is dropped. (An earlier draft of this
  bullet said the set resolves to the first account's flavors, "not a union across own and
  shared". Review found that a misstatement: it assumed one account per name regardless of
  dialect, which under-advertises a name whose first account is openai-only and regresses an
  owner-only principal with two same-name accounts.)
- **Dispatch unchanged:** owner-agnostic, trusts `Target.VendorAccountID`; the sealed
  credential opens only at the dispatch edge, so a borrower never sees it. The resolver is the
  single routing-time gate.

## 9. Management & resource-group endpoints (mirror servers)

Under `/api/portal/vendor-accounts`: a **dedicated** `PUT .../{id}/share-level` (raise:
owner+admin; lower: owner at `shared`, owner-or-CM at `full`; lowering-to-none runs
`ClearVendorAccountSharing`); `PUT .../{id}/admin-groups` (owner-only at `shared`, owner+CM
at `full`; `validateAdminGroupIDs`; ≥1 group; root immutable) +
`GET /api/portal/vendor-account-admin-group-candidates`; owner self-service RG link
`GET/PUT/DELETE .../{id}/resource-groups[/{rgId}]` (mirroring the server-owner endpoints); and
the RG-manager side — a vendor-accounts panel on the resource group
(`PUT /api/portal/resource-groups/{id}/vendor-accounts`, `GET .../vendor-account-candidates`,
gated by `authorizeResourceGroup` + `authorizeVendorAccountOperate` + the `full` restriction +
matching root). Error rows: `ErrVendorAccountForbidden`→403, admin-group validation→400, a new
root-mismatch sentinel→400, `ErrVendorAccountNotFound`→404.

### 9a. Service ↔ resource-group visibility & self-service (UX gap closed)

A separate, pre-existing gap this feature depends on: a service is provisioned into a
resource group only from the **RG side** (`resource_group_provisions`, `ProvisionKindService`,
via the RG "provisioned-for" editor), and there is **no service-side view** of which RGs a
service is in — so there is nowhere on a service to see or manage the provisioning that lets
its tokens reach a shared account (A = option a, §8). Add a service-side self-service,
mirroring the server-owner RG section (`service_resource_group_owner.go` is the analogue to
build): `GET /api/portal/services/{id}/resource-groups` (the RGs the service is provisioned
into, plus the candidates the caller can reach) and `PUT`/`DELETE
.../services/{id}/resource-groups/{rgId}` (add/remove the `service` provision row). It writes
the same `resource_group_provisions` rows the RG-side editor does — a second, symmetric edit
path. **Authorization:** the caller must manage the service (`serviceManageGroupIDs` /
`can_manage_services`) **and** be a member of an admin group the RG is linked to (the
server-owner rule's analogue), so a service manager self-serves only RGs they can reach; the
RG-side provisions editor (gated by `can_manage_resources`) is unchanged. This is independent
of vendor accounts and may ship early (§17).

## 10. Member read & DTO

`VendorAccountDTO` gains `share_level`, `admin_groups` (`{id,name}[]`), `system_group_id`,
`system_group_name`, and `is_owner`/`shared_by`. A **member** (reached only via RG) gets a
**key-free** DTO (`api_key_set`/`subscription_connected` booleans only) with `is_owner:false`
and the owner's name as `shared_by`; a **CM** gets the management fields but still never the
raw secret. `visibleVendorAccountName` is extended so a member/CM sees the shared account's
name. The DTO field additions **and the CM-facing management read land in phase 2** (the authz +
management work); only the **member** (RG-reachable) surfacing is phase 3.

**The member view is minimal, built from an allowlist (deny by default).** A member gets what a
borrower needs to find, understand and request the account: `id`, `vendor`, `auth_type`, `name`,
`status` (it sees that it cannot use a disabled account), `model_prefix`, `models` (each by
`gateway_model`, `api_flavor` and `display_name`), the two credential booleans, `is_owner:false` and
`shared_by`. Everything else is withheld (zero value on the wire, `usage` omitted): `share_level`,
`admin_groups`, `system_group_id`/`system_group_name` (who manages the account and where it is
rooted), `base_url` (where the owner's key is sent), `usage` (the owner's rate-limit and spend
figures), each model's `upstream_model` (the vendor's own id: the member requests the gateway
name) and the timestamps. One builder (`memberVendorAccountDTO`) renders it for **both** the
single read (`GetVendorAccount`) and the member-only rows of `ListVendorAccounts`, so the two
cannot differ; the owner's and the CM's views are unchanged. The viewer tiers are tried widest
first (owner / system, then CM, then member), so a CM who is also provisioned into the account's
group reads the management view.

**The member read needs REACH, not the opt-in.** The read gate admits a principal iff it can
route to the account through the shared arm of the grant (`isMemberOfSharedVendorAccount`: the
account is `shared`/`full` by its effective level, a subscription account needs subscription
sharing, and the principal is provisioned into a resource group containing it): the same
predicate the list's member rows use, so the read and the list agree. The per-token opt-in gates
*using* an account (resolver, model listing), and reading is how a member finds an account to
opt into. A principal without a user identity is never a member of the management page, even a
service provisioned into the group. A stranger, a member of nothing, a paused or lowered account
and an unlinked account are all the same 404-no-leak; a store error while deciding reach fails
the read (never a grant).

## 11. Subscription sharing

Gated by `vendor_subscription_sharing_enabled` (default **off**), meaningful only with
`vendor_account_sharing_enabled` on. The subscription path is ToS-restricted and documented as
"built for internal testing … never a supported multi-tenant capability" (§10 of the vendor
doc). Therefore a subscription account may be raised to **`shared`** but **not `full`** — the
OAuth credential (connect/reconnect/device/the token set) stays strictly owner-only. **And,
because the operate ops open the token set and can persist a rotated single-use refresh token
(`currentSubscriptionTokens → runVendorTokenRefresh`), connection test / model refresh / usage
refresh are owner-only even at `shared` for a subscription account** (§6 note ¹) — a CM may not
trigger a refresh that rotates the owner's single-use token or trips an account-wide
`needs_reconnect`. Sharing a subscription makes borrowers spend the owner's rate-limit windows
and credits (documented), and the whole capability is off by default. The subscription cap and
its flag land **in phase 2** (the raise endpoint), never leaving a window where a subscription can
be raised ungated.

## 12. Usage attribution

`usage_events` already records the caller (`user_id`) and `account_id`, so per-request
attribution is correct for borrowers. `vendor_account_usage` stays **one snapshot per account**
(rate-limit windows/credits are per account), mixing borrowers — a documented limitation;
per-borrower views/limits are a later extra. Documented alongside the owner-delete consequence
(§4).

## 13. System flags

- `vendor_account_sharing_enabled` (bool, default **off**) — master switch. Off ⇒ `share_level`
  is ignored by routing/listings (every account behaves personal), the share/management/RG
  endpoints answer 409, the Groups UI hides the new permission column. Read on the resolver hot
  path through the 5s invalidatable `settingCache[bool]`, like the other vendor flags; fail-closed.
- `vendor_subscription_sharing_enabled` (bool, default **off**, §11).

Both follow the PR1 settings plumbing (schemaless `system_settings`, the `nonReconcileFields`
test, DTO/PUT/cache/invalidation).

## 14. Frontend

Mirror the server UI with the shared building blocks: the `can_manage_vendor_accounts` column
in `GroupsView`; an Anbieter "share" area (admin-only) with a share-level control
(none/shared/full; raise owner+admin, lower per level), an `AdminGroupsEditor` panel
(reusing `adminGroupLinkage`, shown owner-only at `shared`, owner+CM at `full`), and a
`VendorAccountResourceGroupsSection` cloned from `ServerResourceGroupsSection`; a
`ServiceResourceGroupsSection` (also cloned from `ServerResourceGroupsSection`) in the
**ServicesView detail, placed between the Verwaltungsgruppen (`AdminGroupsEditor`, titleId
`service-admin-groups-heading`) and the `ServiceTokensSection`** — the switch table a service
manager uses to join/leave the service's resource groups (§9a); a "Vendor accounts" panel on
`ResourceGroupsView`'s detail; the token `vendor_provider_access`
opt-in widened to list reachable **shared** accounts (per-account, with prefix) on TokenList
**and** ServiceTokensSection (service create carries it, create-only). Thread `ctx.role`/
`isAdmin`/`userId` into the Anbieter view. DE (canonical) + EN parity; update the full-object
`SystemSettings` and related fixtures.

## 15. Docs

`external-vendor-accounts.md` (§1 the share model, §3.5/§8 the by-level RBAC + the subscription
operate-op caveat, §4 the two enumerators + pure grant + the not-the-server-rule note + the
owner-delete consequence, §6.7 listings, §7 the two flags, §11 the opt-in extension + service-token
plumbing + the per-account-for-shared rule), `security-auth-rbac.md`, `data-model.md` (tables +
migrations 90–92 + the count header), `api-surface.md` + `openapi.yaml`, the risk register,
`12-glossary.md` ("shared"/"full shared"), and a new ADR (verify number; likely ADR-060) capturing
the tiered level, management-mirrors-servers-but-routing-is-a-pure-grant, the by-level authorization
(incl. full = owner-equivalent), subscription capped at shared + operate-op caveat, and the
per-account opt-in for shared across user and service tokens.

## 16. Key design decisions

- **Tiered `share_level` enum**; `none` is the untouched personal account.
- **Mirror servers for management/assignment, pure grant for routing** — servers default open,
  credentials default owner-only; the `share_level` flag plus the live-share_level predicate keep
  personal accounts (and any stale link) out of the pool.
- **`full` is complete owner-equivalent delegation** (incl. downgrade/delete); the owner's control
  is structural (they own the record and chose the level), not an operation reserved at `full`.
- **Two enumerators**: the narrow routing one (own ∪ shared-via-RG, live-level, fail-closed on the
  shared arm) is parity-bound; the wide management listing (own ∪ managed ∪ member) is not.
- **Opt-in stays explicit, and per-account for shared** — borrowing another owner's credential is
  always a deliberate, named choice; `all:true` never auto-borrows.

## 17. Decomposition into phases (delivered as ONE pull request)

The feature is built in the ordered phases below but delivered as a **single PR at the end**,
not seven. Each phase is still an independently-reasoned, self-contained unit; at **every phase
boundary** the full quality gate runs green before the next phase starts — `go test ./...` +
`golangci-lint run`/`fmt --diff` per Go module, the **Postgres store leg** for the migrations
and accessors, frontend `npm test`/`build`/`format:check`, `make lint-docs`, and a **Sonar**
scan with **branch-findings = 0** — so the branch stays shippable throughout and the work can
run start-to-finish without stopping for per-phase PRs. The phase order still matters: each
builds on the last, and the per-phase gate is where a regression is caught early rather than at
the end of a 6-phase branch.

0. **Service ↔ resource-group self-service (§9a).** The `GET/PUT/DELETE
   /api/portal/services/{id}/resource-groups` endpoints + `ServiceResourceGroupsSection` in
   ServicesView. Independent of vendor accounts (pure service-provisioning UX over the existing
   `resource_group_provisions`); it removes the visibility gap that makes the service-token
   shared-account flow (§8) usable.
1. **Store + migrations + permission plumbing.** Migrations 90–92, the store methods (incl.
   `ClearVendorAccountSharing`) + memory mirror/cascades + conformance + regen, the
   `can_manage_vendor_accounts` flag through store + group-management service/endpoints/DTOs +
   Groups UI. No routing behavior change yet.
2. **Share lifecycle + authorization + management groups + subscription cap.** The
   `PUT .../share-level` raise/lower (with `ClearVendorAccountSharing` removing admin-group links +
   clearing root), the `authorizeVendorAccountOperate`/`Credential` split, field-level deny-by-default
   PATCH, management-group assignment endpoints + candidates + the containment root, the
   `vendor_account_sharing_enabled` master flag, the DTO additions + the CM-facing key-free read,
   **and the subscription cap + `vendor_subscription_sharing_enabled`** (so a subscription can never
   be raised ungated). No cross-user routing yet (no RG links, not resolvable by others).
3. **RG assignment (both paths) + routing enumerator + pure grant + opt-in + member read.** The
   `resource_group_vendor_accounts` endpoints, `VendorAccountsForRouting` + the live-level grant +
   the listing changes in lock-step, the vendor-access opt-in extension (user + service tokens,
   per-account for shared), the member key-free surfacing, and extending `ClearVendorAccountSharing`'s
   downgrade to also delete RG links. This is the phase that lets a borrower use a shared account.
4. **Subscription sharing finish.** The operate-op owner-only caveat for subscriptions (§11) and its
   docs — if not already folded into phase 2/phase 3 where those endpoints land.
5. **Frontend.** The Anbieter share area, the RG-side panel, the token opt-in widening, i18n + fixtures.
6. **Docs.** Architecture/OpenAPI/ADR — folded into each phase where practical; this sweeps the rest.

## 18. Integration points (paths relative to `gateway/backend/`; verify exact lines at plan time)

- Store: `internal/store/{migrate.go (90–92), sqlite_vendor_accounts.go, sqlite_resource_groups.go,
  sqlite_user_groups.go, models.go}`, `internal/routing/{store.go, memory_store.go}`,
  `internal/portal/memory_directory.go`, `internal/tracing/routingstore_gen.go`.
- Permission: `internal/portal/{service_user_groups.go, group_store.go, api.go, api_tracing_gen.go}`,
  `internal/gateway/group_endpoints.go`.
- Authz + lifecycle: `internal/portal/service_vendor_accounts.go` (gate split, field-level PATCH,
  create/list), new `internal/portal/service_vendor_sharing.go` (share level + `ClearVendorAccountSharing`
  + admin-group assignment + RG link), `service_vendor_connect.go`/`service_vendor_validation.go`/
  `service_vendor_discovery.go` (operate ops reachable by CM except the subscription token-rotating
  ones), `service.go` (`validateVendorAccess`/`ownerVendorAccountsByID` → per-principal reachable set,
  `visibleVendorAccountName`), service-token create/DTO/rotate for `VendorAccess`.
- Routing: `internal/routing/resolver.go` (`resolveVendorAccount`, eligibility predicate,
  `VendorAccountsForRouting` seam with nil=owner-only), `internal/routing/vendor_access.go`.
- Listings: `internal/portal/service_vendor_listing.go` + call sites.
- Endpoints/routes: `internal/gateway/{portal_vendor_account_endpoints.go, resource_group_endpoints.go,
  portal_service_endpoints.go, server.go}`; a new `internal/portal/service_service_resource_groups.go`
  (the service-side RG self-service, §9a, analogue of `service_resource_group_owner.go`).
- Settings: the two flags via `service_vendor_settings.go` + `service_system_settings.go` +
  `internal/gateway/{vendor_account_settings.go, system_settings_endpoints.go}`.
- Frontend: `src/components/{GroupsView, VendorAccountsView, VendorAccountModels, ResourceGroupsView,
  ServicesView, ServerResourceGroupsSection (cloned for vendor accounts + services), TokenList,
  ServiceTokensSection}.tsx`,
  `src/components/shared/{AdminGroupsEditor, AdminGroupPicker, adminGroupLinkage}`,
  `src/api/{vendorAccounts, resourceGroups, groups, services}.ts`, `src/components/views.tsx`,
  `src/i18n.ts` + fixtures.
- Quality gates: golangci-lint per module, prettier `format:check`, the Postgres store leg for the
  three migrations + accessors, `make lint-docs`, Sonar branch-findings = 0.

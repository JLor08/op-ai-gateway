# Shared Vendor Accounts via Resource Groups — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a personal vendor account shareable with other users and services via resource groups, modelled on AI servers for management/assignment but using a pure, fail-closed grant for routing so personal accounts never leak.

**Architecture:** A `share_level` enum (none/shared/full) on the account; a new `can_manage_vendor_accounts` co-manager permission mirroring `can_manage_servers`; `vendor_account_admin_groups` + `system_group_id` containment root + `resource_group_vendor_accounts`, all mirroring the server/RG tables; a by-level authorization split (operate vs credential gates); a routing enumerator (own ∪ shared-via-provisioned-RG, live `share_level`, fail-closed on the shared arm) distinct from a wider management listing; per-account opt-in for shared accounts (user + new service-token plumbing). Built in ordered phases, each ending at the full quality gate, delivered as ONE PR.

**Tech Stack:** Go backend (`gateway/backend`, module `op-ai-gateway`); React/TS/Vite portal (`gateway/frontend`); memory/sqlite/postgres stores behind a dialect seam; append-only migrations; OTel; MUI; state-based routing.

**Spec:** `docs/superpowers/specs/2026-10-15-vendor-account-sharing-design.md` (read it; the plan argues from it).

## Global Constraints

- Branching: never commit to/merge `main`; all work on this feature branch; ONE PR at the end (spec §17).
- Migrations are append-only; next free number is **90** (verify tail of `internal/store/migrate.go` before adding); booleans are `integer not null default …`; FKs cascade; FK enforcement is on both dialects.
- Repo-facing text (code, comments, commits, docs, PR) in **English**; chat in German.
- Every public `portal.API` method change: hand-edit `internal/portal/api.go`, then `go generate ./internal/tracing/...`, then `scripts/add-license-headers.sh`; a `routing.Store` method change regenerates `internal/tracing/routingstore_gen.go` likewise. Guard `add-license-headers.sh` from touching unrelated files (it over-reached onto `geminiwire` once — revert any spillover).
- i18n: DE is canonical, EN parity is a compile error; add both. New required `SystemSettings`/DTO fields break full-object test fixtures — update them.
- TDD throughout: failing test first, watch it fail, minimal code, green.
- **Phase gate (run at every phase boundary, spec §17):** `cd gateway/backend && go test ./...` + `golangci-lint run ./...` + `golangci-lint fmt --diff ./...`; the **Postgres store leg** (`OP_AI_GATEWAY_TEST_POSTGRES_DSN` set, throwaway `postgres:16`, run `./internal/store/... ./internal/auth/...`) for any phase touching the store; `cd gateway/frontend && npm ci && npm test && npm run build && npm run format:check`; `make lint-docs`; and **Sonar** (`make sonar-scan && make sonar-findings && make sonar-branch-findings`, branch-findings = 0). Do not proceed past a red gate.
- Security posture (spec §6/§8): system scope is read-only on vendor-account writes; the resolver is the single routing-time gate; the enumerator predicate re-reads **live** `share_level` (a stale link grants nothing); the routing enumerator fails **closed** on the shared arm (own accounts always returned); borrowing a shared account always needs an explicit per-account opt-in.

## Review Focus

Five input/failure classes the spec implies but no single task's happy-path test exercises; each gets a pinning test in the owning task:

- **Stale-link grant (phase 3).** A `none`/downgraded account with a surviving `resource_group_vendor_accounts` row must grant nothing — the enumerator re-reads live `share_level`. Test: link an account, set `share_level='none'` directly in the store, assert a provisioned non-owner cannot resolve/list it.
- **Field-level PATCH fall-through (phase 2).** A PATCH naming a credential-sensitive field (`api_key`, and any non-allowlisted field) by a CM at `shared` must be refused (deny-by-default), not fall through to the operate gate. Test: CM PATCHes `api_key` at `shared` → 403, nothing written.
- **Subscription token rotation by a CM (phase 2/4).** A CM running connection-test / model-refresh / usage-refresh on a *subscription* account at `shared` must be refused (owner-only), so the single-use OAuth token can't rotate under a non-owner. Test: CM calls refresh on a shared subscription → 403.
- **all:true does not auto-borrow (phase 3).** An `all:true` token / portal session must reach only *own* accounts, never a reachable shared one without an explicit per-account opt-in. Test: all:true token, provisioned into a shared account's RG, no per-account entry → resolver/listing excludes it.
- **Service eligibility precision (phase 3).** The relaxed eligibility guard admits only `IsService() && ServiceID != ""` (and users with `UserID != ""`); a principal with both empty must stay denied. Test: a both-empty principal → vendor branch not entered.

---

## Phase 0 — Service ↔ resource-group self-service (independent; spec §9a)

Closes the pre-existing gap: no service-side view of a service's resource groups. Mirror the server-owner self-service (`internal/portal/service_resource_group_owner.go` + `ServerResourceGroupsSection.tsx`) for services, over the existing `resource_group_provisions` (`ProvisionKindService`). No vendor-account dependency.

### Task 0.1 — Backend: service RG self-service service + endpoints

**Files:** Create `internal/portal/service_service_resource_groups.go`; modify `internal/gateway/portal_service_endpoints.go` (+ routes in `internal/gateway/server.go`); test `internal/portal/service_service_resource_groups_test.go`, `internal/gateway/portal_service_resource_groups_test.go`.

**Interfaces (produces):**
- `(*Service).ServiceResourceGroups(ctx, principal, serviceID) ([]ServiceResourceGroupDTO, error)` — the RGs the service is provisioned into + whether each is a member, over `ResourceGroupIDsByProvisionTargets(ProvisionKindService, [serviceID])`, filtered to RGs linked to an admin group the caller is a member of (mirror `ServerOwnerResourceGroups`).
- `(*Service).AddServiceToResourceGroup(ctx, principal, serviceID, rgID) error` / `RemoveServiceFromResourceGroup(...)` — add/remove the `service` provision row via `SetResourceGroupProvisions`-style write. Authorize with a new `authorizeServiceRGSelfService(principal, serviceID, rgID)` = caller manages the service (`serviceManageGroupIDs`/`can_manage_services`) AND is a member of an admin group the RG is linked to.
- `ServiceResourceGroupDTO{ID, Name string; Member bool}` (mirror `ServerResourceGroup`).

- [ ] **Step 1:** Write failing `service_service_resource_groups_test.go`: a service manager sees/joins/leaves an RG they reach; a non-manager gets empty/forbidden; a stranger gets no leak. Run, watch fail.
- [ ] **Step 2:** Implement the service + `authorizeServiceRGSelfService`, mirroring `service_resource_group_owner.go` (swap `authorizeServerOwner`→the service-manager check, `ResourceGroupServers`→the `service`-provision read/write). Green.
- [ ] **Step 3:** Write failing HTTP test (`GET/PUT/DELETE /api/portal/services/{id}/resource-groups[/{rgId}]`, scope `gateway:use`), wire the endpoints + routes mirroring the server-owner endpoints (`portal_server_endpoints.go:254-286`). Green.
- [ ] **Step 4:** Full `go test ./internal/portal/ ./internal/gateway/`; gofmt/vet/golangci. Commit.

### Task 0.2 — Frontend: `ServiceResourceGroupsSection`

**Files:** Create `src/components/ServiceResourceGroupsSection.tsx` (clone `ServerResourceGroupsSection.tsx`); modify `src/components/ServicesView.tsx` (insert between the `service-admin-groups-heading` Panel and `ServiceTokensSection`, ~line 656), `src/api/services.ts` (+ `serviceResourceGroups`/`joinServiceResourceGroup`/`leaveServiceResourceGroup`, mirror `api/servers.ts:651-664`); tests `src/components/ServiceResourceGroupsSection.test.tsx` (clone the server one), extend `ServicesView.test.tsx`.

- [ ] **Step 1:** Failing api test for the three calls; implement in `services.ts`. Green.
- [ ] **Step 2:** Failing component test (switch table join/leave, empty state); clone `ServerResourceGroupsSection` + i18n keys (`serviceResourceGroups*`, DE+EN). Green.
- [ ] **Step 3:** Mount it in `ServicesView` detail between the two sections; test it renders there. `npm test`/`build`/`format:check`. Commit.

### ⟐ Phase 0 gate — run the full quality gate (Global Constraints). Fix red before Phase 1.

---

## Phase 1 — Store, migrations, and the `can_manage_vendor_accounts` permission (spec §4, §5)

No routing behavior change: the columns/tables/flag exist, nothing reads them for routing yet.

### Task 1.1 — Migration 90: `can_manage_vendor_accounts` column

**Files:** `internal/store/migrate.go` (new `migration90Up`, registered after v89); test `internal/store/migrate_test.go` (mirror `TestMigration53AddsCanManageResourcesColumn` at :424).

- [ ] Verify the tail is v89. Write failing migration test. Add `migration90Up` copying `migration49Up` (`ALTER TABLE user_group_managers ADD COLUMN can_manage_vendor_accounts integer not null default 1`, duplicate-tolerant both dialects). Green. Commit.

### Task 1.2 — Migration 91: share model columns + `vendor_account_admin_groups`

**Files:** `internal/store/migrate.go` (`migration91Up`); `migrate_test.go`.

- [ ] Failing test. Add `vendor_accounts.share_level text not null default 'none'`, `vendor_accounts.system_group_id text not null default ''`, and `vendor_account_admin_groups(vendor_account_id FK→vendor_accounts cascade, group_id FK→user_groups cascade, created_at, unique(vendor_account_id,group_id))` + index on `group_id`, copying `migration50Up`. Green. Commit.

### Task 1.3 — Migration 92: `resource_group_vendor_accounts`

**Files:** `internal/store/migrate.go` (`migration92Up`); `migrate_test.go`.

- [ ] Failing test. Add the link table copying `migration54Up`'s `resource_group_servers` + index on `vendor_account_id`. Green. Commit.

### Task 1.4 — Store: the `UserGroupManagerPerm` flag

**Files:** `internal/store/models.go` (`UserGroupManagerPerm.CanManageVendorAccounts`), `internal/store/sqlite_user_groups.go` (select/scan :246-268, update :278 — add the column), `internal/portal/memory_directory.go` (entry struct :786, default-true new row :806, read :843, write :882), `internal/portal/group_store.go` (if the interface names the fields); conformance in `internal/store/conformance_test.go`.

- [ ] Failing conformance test (persist+read the flag on all drivers). Implement each site. Green. Commit.

### Task 1.5 — Routing store: vendor-account sharing methods

**Files:** `internal/routing/store.go` (`VendorAccountStore` + `ResourceGroupStore` interfaces), `internal/store/sqlite_vendor_accounts.go`, `internal/store/sqlite_resource_groups.go`, `internal/routing/memory_store.go`, regenerate `internal/tracing/routingstore_gen.go`; conformance tests.

**Interfaces (produces):** on `VendorAccountStore`: `UpdateVendorAccountShareLevel(ctx,id,level) error`, `UpdateVendorAccountSystemGroup(ctx,id,sgid) error`, `SetVendorAccountAdminGroup(ctx,id,gid) error`, `RemoveVendorAccountAdminGroup(ctx,id,gid) error`, `VendorAccountAdminGroups(ctx,id) ([]string,error)`, `VendorAccountsByAdminGroups(ctx,gids) ([]VendorAccount,error)`, `ClearVendorAccountSharing(ctx,id) error` (one tx: set level='none', delete all admin-group + RG links, clear system_group_id — **set level first**). On `ResourceGroupStore`: `SetResourceGroupVendorAccount`, `RemoveResourceGroupVendorAccount`, `ResourceGroupVendorAccounts(ctx,rgID)`, `ResourceGroupsByVendorAccount(ctx,accID)`.

- [ ] **Step 1:** Failing conformance tests mirroring the `ServerStore`/`ResourceGroupStore` server tests, PLUS: `ClearVendorAccountSharing` empties both link sets + resets level/root in one go; `DeleteResourceGroup` empties the account link set; `DeleteVendorAccount` empties admin-group (single-key) AND the RG link set (**reverse loop over all RGs**, pattern `deleteServer` memory_store.go:422).
- [ ] **Step 2:** Implement SQL + memory (watch the memory map key direction: `resourceGroupVendorAccounts` keyed `rgID→accID`). Add the memory cascades to `DeleteVendorAccount` and `DeleteResourceGroup`. Green.
- [ ] **Step 3:** Regenerate `routingstore_gen.go`; confirm only the intended additions. Full `go test ./internal/store/ ./internal/routing/`. Commit.

### Task 1.6 — Permission through the group-management service + API + endpoints

**Files:** `internal/portal/service_user_groups.go` (predicate `vendorAccountManageGroupIDs` wrapper :~181; `UserGroupDTO`/`UserGroupMemberDTO` flags; `groupDTO` four branches; `GroupMembers`; `PromoteManager`/`SetManagerPermissions` extra positional bool), `internal/portal/api.go` (+ regen `api_tracing_gen.go`), `internal/gateway/group_endpoints.go` (`groupManagerRequest` + both handlers); tests `service_user_groups_test.go`.

- [ ] **Step 1:** Failing test: promote a manager with `can_manage_vendor_accounts`, read it back in the DTO + members; `vendorAccountManageGroupIDs` returns the right groups.
- [ ] **Step 2:** Implement all sites (mirror `can_manage_servers`). Regenerate tracing. Green. Commit.

### Task 1.7 — Frontend: the permission column in GroupsView

**Files:** `src/api/groups.ts` (`UserGroup`/`GroupMember` types, `ManagerPerms`, `DEFAULT_MANAGER_PERMS`, `promoteManager`/`setManagerPermissions` bodies), `src/components/GroupsView.tsx` (`ManagePermFlag`/`ManagePermPatch`, switch helpers, `setManagerPermission` carry-over, the `perm-vendor-accounts` column), `src/i18n.ts` (`groupsPermVendorAccounts` DE+EN); update fixtures in `GroupsView.test.tsx`/`UsersView.test.tsx`/`ProjectsView.test.tsx`.

- [ ] Failing test (the new column renders + toggles send the flag). Implement mirroring `groupsPermServers`. `npm test`/`build`/`format:check`. Commit.

### ⟐ Phase 1 gate — full quality gate **including the Postgres store leg** (migrations). Fix red before Phase 2.

---

## Phase 2 — Share lifecycle, authorization, management-group assignment, subscription cap (spec §6, §7, §9, §10, §11, §13)

No cross-user routing yet: accounts can be shared + assigned to management groups, but not to RGs and not resolvable by others.

### Task 2.1 — `vendor_account_sharing_enabled` master flag

**Files:** mirror the PR1 `vendor_subscriptions_enabled` plumbing exactly — `internal/portal/service_vendor_settings.go` (key + pure reader + service method), `service_system_settings.go` (DTO + PUT + write + `nonReconcileFields` test), `internal/gateway/vendor_account_settings.go` (cache + accessor, fail-closed default **off**), `system_settings_endpoints.go` (invalidation), `portal/api.go` (+regen). Default **off**.

- [ ] Failing settings tests (persist/read, default off, cache invalidation). Implement. Green. Commit.

### Task 2.2 — The authorization split

**Files:** `internal/portal/service_vendor_accounts.go` (`authorizeVendorAccount` → `authorizeVendorAccountOperate` [owner OR CM via `vendorAccountManageGroupIDs`] + `authorizeVendorAccountCredential` [owner-only at `shared`; owner OR CM at `full`]; the 404-no-leak `ErrVendorAccountNotFound`), `internal/portal/service.go` (sentinels `ErrVendorAccountForbidden`); tests `service_vendor_sharing_authz_test.go`.

**Interfaces (produces):** the two gates; `share_level` read on the account record.

- [ ] **Step 1:** Failing table test for the matrix (spec §6): operate vs credential vs system-read, at none/shared/full, owner/CM/member/stranger — asserting allow/deny AND no-write-on-deny. Include the Review-Focus field-level-PATCH case.
- [ ] **Step 2:** Implement the two gates. Green. Commit.

### Task 2.3 — Field-level, deny-by-default PATCH

**Files:** `internal/portal/service_vendor_accounts.go` (`UpdateVendorAccount`: `name`/`status`/`model_prefix` → operate gate; **every other field incl. `api_key` → credential gate**; `base_url`/`auth_type` stay immutable/create-only); enumerate the credential-gated connect/reconnect/device/import methods in `service_vendor_connect.go`/`service_vendor_validation.go` through the credential gate; tests.

- [ ] Failing test: CM at `shared` PATCHes `api_key` → 403, nothing written; CM PATCHes `status`/`prefix` → ok; an unknown/new field → credential gate. Implement deny-by-default. Green. Commit.

### Task 2.4 — Share-level lifecycle endpoint

**Files:** new `internal/portal/service_vendor_sharing.go` (`RaiseVendorAccountShareLevel`/`LowerVendorAccountShareLevel`; raise = owner+admin + ≥1 management group establishing `system_group_id` via `validateAdminGroupScope`; lower = owner at `shared`, owner-or-CM at `full`; lower-to-none calls `ClearVendorAccountSharing`; **subscription accounts raise only to `shared`, gated by `vendor_subscription_sharing_enabled`** — spec §11), `internal/gateway/portal_vendor_account_endpoints.go` (`PUT .../{id}/share-level` + error rows), `server.go` route; tests.

- [ ] **Step 1:** Failing tests: raise none→shared→full (owner+admin), lower (per level), downgrade-to-none clears links + root (assert via store), subscription capped at shared + flag-gated, ≥1 group required, raise needs admin.
- [ ] **Step 2:** Implement. Green. Commit.

### Task 2.5 — Management-group assignment endpoint

**Files:** `service_vendor_sharing.go` (`SetVendorAccountAdminGroups` — owner-only at `shared`, owner+CM at `full`; `validateAdminGroupIDs`; ≥1 group; root immutable), `ServiceVendorAccountAdminGroupCandidates`; endpoints `PUT .../{id}/admin-groups` + `GET /api/portal/vendor-account-admin-group-candidates` (mirror the server ones); tests.

- [ ] Failing tests (shared=owner-only, full=owner+CM, root immutable, candidates scoping, no-empty). Implement mirroring `SetServerAdminGroups`/`ServerAdminGroupCandidates`. Green. Commit.

### Task 2.6 — DTO additions + CM key-free management read; ListVendorAccounts management union

**Files:** `service_vendor_accounts.go` (`VendorAccountDTO` + `share_level`/`admin_groups`/`system_group_id`/`system_group_name`/`is_owner`/`shared_by`; `vendorAccountDTO` fills them; CM gets management fields but never the raw key; `ListVendorAccounts` non-system = own ∪ `VendorAccountsByAdminGroups(managed)` — the member/RG set is added in phase 3), `visibleVendorAccountName` (member/CM see the name); frontend `src/api/vendorAccounts.ts` types; tests.

- [ ] Failing tests: a CM reads a managed account key-free with `is_owner:false`; owner sees full; stranger 404. Implement. Green. Commit.

### ⟐ Phase 2 gate — full quality gate (incl. Postgres leg). Fix red before Phase 3.

---

## Phase 3 — Resource-group assignment, routing enumerator, pure grant, opt-in, member read (spec §8, §9, §10)

The phase that lets a borrower use a shared account.

### Task 3.1 — RG assignment: owner self-service + RG-manager side

**Files:** `service_vendor_sharing.go` (owner self-service `VendorAccountResourceGroups`/`AddVendorAccountToResourceGroup`/`RemoveVendorAccountFromResourceGroup`, mirror `service_resource_group_owner.go`, at `shared`+; RG-manager `SetResourceGroupVendorAccounts` in `service_resource_groups.go` gated `authorizeResourceGroup` + `authorizeVendorAccountOperate` + `full`-only + matching root), endpoints under `/api/portal/vendor-accounts/{id}/resource-groups` + `/api/portal/resource-groups/{id}/vendor-accounts` + candidates; extend `ClearVendorAccountSharing` call in downgrade to also drop RG links (already in the store method — confirm the lifecycle wires it); tests.

- [ ] **Step 1:** Failing tests for both paths (owner self-service join/leave; RG-manager add with the dual gate + root match + full-only), and that downgrade removes RG links.
- [ ] **Step 2:** Implement mirroring the server RG paths. Green. Commit.

### Task 3.2 — The routing enumerator + pure grant (resolver)

**Files:** `internal/portal/service.go` or a new `service_vendor_sharing_grant.go` (`VendorAccountsForRouting(principal)` = own [emitted first, id-sorted] ∪ live-`shared`/`full` accounts in RGs the principal is provisioned into [id-sorted], reusing `provisionedRGsForPrincipal` + `ResourceGroupVendorAccounts`; **fail-closed on the shared arm**, own always returned); the routing seam on `Resolver` (nil = owner-only, NOT allow-all); `internal/routing/resolver.go` (`resolveVendorAccount` uses the seam; eligibility guard `UserID != "" OR (IsService && ServiceID != "")`); `internal/gateway/server.go` wiring; tests `resolver_vendor_sharing_test.go`.

- [ ] **Step 1:** Failing tests: owner resolves own; a provisioned member resolves a shared account; a non-provisioned principal does not; a `none`/stale-link account never resolves for a non-owner (**Review-Focus stale-link**); own-before-shared precedence; service token via service-provision; the both-empty principal denied (**Review-Focus eligibility**); nil seam = owner-only.
- [ ] **Step 2:** Implement the enumerator (live share_level in the predicate) + the seam + wiring. Green. Commit.

### Task 3.3 — Listing parity (ownVendorAccountModels) + management listing

**Files:** `internal/portal/service_vendor_listing.go` (`ownVendorAccountModels` reads `VendorAccountsForRouting`, same fail direction as routing); `ListVendorAccounts` non-system = own ∪ managed ∪ member (the full management union, spec §8/§9); the per-name flavor set resolves to the precedence winner (not a union across own+shared); tests.

- [ ] Failing tests: advertised == routable for a shared account across on/off; the management list includes a member-only account (key-free) and a CM-managed-not-provisioned account; flavor set = winner's. Implement. Green. Commit.

### Task 3.4 — Per-token opt-in for shared accounts (user + service)

**Files:** `internal/portal/service.go` (`validateVendorAccess`/`ownerVendorAccountsByID` → reachable set computed against the **token's principal**: owning user for a user token, service identity for a service token; `all:true`/portal session covers only own — shared needs an explicit per-account entry), `internal/routing/vendor_access.go` (`TokenVendorPrefix` honors a named shared account); service-token plumbing: `CreateServiceTokenRequest`/`ServiceTokenDTO` gain `VendorAccess`, `CreateServiceToken` writes `VendorProviderAccess`, `RotateServiceToken` preserves it (create-only); tests.

- [ ] **Step 1:** Failing tests: a user token with an explicit per-account entry reaches a reachable shared account (prefix honored); `all:true` does NOT auto-borrow a shared account (**Review-Focus all:true**); a service token created with a vendor-access entry reaches its provisioned shared account; reachability uses the token's principal.
- [ ] **Step 2:** Implement. Green. Commit.

### Task 3.5 — Member key-free read surfacing

**Files:** `service_vendor_accounts.go` (`GetVendorAccount` returns the key-free member DTO for a member-reached account; `is_owner`/`shared_by`); tests.

- [ ] Failing test: a provisioned member GETs a shared account key-free with `shared_by`; stranger 404. Implement. Green. Commit.

### ⟐ Phase 3 gate — full quality gate (incl. Postgres leg). Fix red before Phase 4.

---

## Phase 4 — Subscription sharing hardening (spec §11)

### Task 4.1 — Subscription operate-ops owner-only at `shared`

**Files:** `service_vendor_validation.go` (`TestVendorAccountConnection`), `service_vendor_discovery.go` (model refresh), `service_vendor_usage_refresh.go` (usage refresh): when `auth_type == subscription`, require the owner even at `shared` (not CM), because these can rotate the single-use token; tests.

- [ ] Failing test (**Review-Focus subscription-rotation**): a CM calls refresh/test on a shared *subscription* account → 403; on a shared *api-key* account → ok; owner always ok. Implement the subscription guard. Green. Commit.

### ⟐ Phase 4 gate — full quality gate.

---

## Phase 5 — Frontend (spec §14)

### Task 5.1 — Anbieter share area

**Files:** `src/components/VendorAccountsView.tsx` (thread `ctx.role`/`isAdmin`/`userId` via `views.tsx`; a share-level control none/shared/full [raise owner+admin, lower per level]; an `AdminGroupsEditor` panel [owner-only at shared, owner+CM at full] reusing `adminGroupLinkage`; credential panels shown per tier), `src/api/vendorAccounts.ts` (share-level, admin-groups, candidates calls); i18n; tests + fixtures.

- [ ] Failing wiring tests (share-level control, admin-groups editor gated by level, credential panel hidden for CM at shared). Implement. `npm test`/`build`/`format:check`. Commit.

### Task 5.2 — Vendor-account RG self-service + RG-side panel

**Files:** `src/components/VendorAccountResourceGroupsSection.tsx` (clone `ServerResourceGroupsSection`) in the Anbieter detail; a "Vendor accounts" MultiSelect panel in `ResourceGroupsView.tsx` detail (beside Servers); `src/api/{vendorAccounts,resourceGroups}.ts`; i18n; tests.

- [ ] Failing tests; implement mirroring the server RG UI. Green. Commit.

### Task 5.3 — Token opt-in widening (user + service)

**Files:** `src/components/TokenList.tsx` + `ServiceTokensSection.tsx` (the `vendor_provider_access` opt-in lists reachable **shared** accounts per-account with prefix override; service create carries it, create-only), `src/api/{tokens,services}.ts`; i18n; tests.

- [ ] Failing tests (a shared account is opt-in-able with a prefix; service-create sends vendor access). Implement. Green. Commit.

### ⟐ Phase 5 gate — full quality gate (frontend-heavy; `format:check` is CI-gating).

---

## Phase 6 — Docs (spec §15)

### Task 6.1 — Architecture + reference + ADR

**Files:** `docs/architecture/cross-cutting/external-vendor-accounts.md` (the share model §1, by-level RBAC §3.5/§8 + the subscription operate-op caveat, the two enumerators + pure grant + not-the-server-rule + owner-delete consequence §4, listings §6.7, the two flags §7, the opt-in + service-token plumbing + per-account-for-shared §11), `security-auth-rbac.md`, `reference/data-model.md` (tables + migrations 90–92 + the count header), `api-surface.md` + `openapi.yaml` (the new endpoints + DTO fields + 403/404/409), `11-risks-and-technical-debt.md`, `12-glossary.md` ("shared"/"full shared"), a new ADR (verify the next free number) capturing the five key decisions (spec §16).

- [ ] Write the doc updates. `make lint-docs` OK; grep for stale migration-count / cross-links. Commit.

### ⟐ Phase 6 gate — full quality gate (final): all of the above green, then branch-local cleanup (`docs/superpowers/`) and ONE PR + Auto-fix (await the standing authorization / user OK).

---

## Self-review notes (plan author)

- Spec coverage: every spec section maps to a phase/task (§4→P1, §5→P1.6, §6→P2.2/2.3, §7→P2.4, §8→P3.2–3.4, §9→P2.5/P3.1, §9a→P0, §10→P2.6/P3.5, §11→P2.4/P4, §12→P6, §13→P2.1, §14→P5, §15→P6).
- Type consistency: `VendorAccountsForRouting` (P3.2) and the store methods (P1.5) are named once and reused; `ClearVendorAccountSharing` is the store method, called by the lifecycle (P2.4 for admin-group+root, extended for RG links in P3.1).
- Review Focus: the five classes each have a named pinning test in the owning task.
- No placeholders: mechanical tasks name the exact server/RG precedent to mirror (concrete content); novel logic (the gates, the enumerator, `ClearVendorAccountSharing`, the field-level PATCH) is specified in prose + signatures; per-task code is composed at dispatch time by the subagent-driven implementer from the brief + the named precedent.

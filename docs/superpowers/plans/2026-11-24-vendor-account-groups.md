# Vendor Account Groups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user order several of their vendor accounts of one vendor into a group that a token can use like an account, so requests move to the next account when one is near its limit or rejects with a limit error.

**Architecture:** A new personal entity `vendor_account_group` with ordered members lives beside `vendor_account` in all three store drivers. The resolver's vendor branch gains a group step (before the account loop) that picks the first eligible member from a pure selection function fed by the usage snapshot and an in-memory limit-cooldown registry. Limit errors (429/402) on a group target fail over before the first byte on both the passthrough and the translate path. The portal gets group CRUD, token-policy support and dashboard cards.

**Tech Stack:** Go 1.26 (`gateway/backend`, module `op-ai-gateway`), SQLite/PostgreSQL/memory stores, React + TypeScript + Vite + MUI (`gateway/frontend`), vitest, Playwright not required.

**Spec:** `docs/superpowers/specs/2026-11-24-vendor-account-groups-design.md` (read it first; section numbers below refer to it).

## Global Constraints

- Branch `feat/vendor-account-groups` in `.worktrees/vendor-account-groups`; never commit to `main`.
- All three store drivers (memory / sqlite / postgres); schema changes are append-only migrations; run store tests with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (provision Postgres like CI).
- One new migration, **version 114** (`vendor_account_groups`). Before the PR, rebase on `origin/main`; if 114 is taken, renumber to the next free version.
- Threshold fields are integers 1..100, default **99** (`routing.DefaultVendorGroupThresholdPct`).
- Limit-cooldown default **60 s** (`routing.DefaultVendorLimitCooldown`); group-triggered usage pull at most once per **15 min** per account (`vendorGroupUsagePullInterval`), exponential backoff after a 429 from the usage endpoint, capped at **4 h**.
- Limit errors are HTTP **429** and **402** only (status-based; OpenAI `insufficient_quota` and Codex `usage_limit_reached` arrive as 429).
- Group ids: `"vag_" + compactRandomHex(16)`.
- New error codes live in the `vendor_account_group.*` family; existing codes are unchanged.
- Everything sits behind `vendor_accounts_enabled`; no new feature flag.
- i18n keys are added in German **and** English together in `gateway/frontend/src/i18n.ts`.
- Reverse-engineered constants (Anthropic `api/oauth/usage`) carry a `VERIFY-LIVE` comment.
- No ServerAgent change, so no agent version bump.
- Go gates: `gofumpt`/`golangci-lint fmt --diff` and `golangci-lint run` per module (CI runs them; `go vet`/`go test` do not cover them). Frontend gate: `npm run format:check`, `npm run lint`, `npm test`, `npm run build`.
- Repo-facing text (commits, docs, code comments) in English.

## Review Focus

1. **A limit error after bytes reached the client** (streamed translate response, `delivered == true`) must not fail over; the client keeps the stream it has. Test in Task 11.
2. **A group whose every member became unreachable** (shares revoked, accounts disabled, subscriptions switched off) must resolve as "not served by this group" and fall through to the accounts/standard path, never 500. Test in Task 7.
3. **A stale snapshot past its reset** (five-hour 99.5 % with a reset in the past) must count as renewed, so position 1 is used again. Test in Task 6.
4. **A deleted group or member still named in a token policy** must match nothing and never break token decode, listing or routing. Tests in Tasks 5 and 7.
5. **Every member answers 429**: each member is tried exactly once per request (exclusion grows), and the client gets the last vendor answer. Test in Task 12.

---

## Phase A — Data layer

### Task 1: Group types, store interface and memory store

**Files:**
- Create: `gateway/backend/internal/routing/vendor_groups.go`
- Create: `gateway/backend/internal/routing/memory_store_vendor_groups.go`
- Modify: `gateway/backend/internal/routing/store.go` (add `VendorAccountGroupStore` to `Store`, near :3680)
- Modify: `gateway/backend/internal/routing/memory_store.go` (maps in the struct + constructor; cascade in `DeleteVendorAccount`)
- Create: `gateway/backend/internal/store/vendor_account_group_conformance_test.go` (runs through `forEachRoutingStore`, so memory now and SQL after Task 2)

**Interfaces:**
- Produces (package `routing`):
  - `type VendorAccountGroup struct { ID, OwnerUserID, Vendor, Name, ModelPrefix, Status string; ThresholdFiveHourPct, ThresholdWeeklyPct, ThresholdCreditsPct int; CreatedAt, UpdatedAt time.Time }`
  - `const VendorAccountGroupStatusActive = "active"`, `VendorAccountGroupStatusDisabled = "disabled"`, `DefaultVendorGroupThresholdPct = 99`
  - `type VendorAccountGroupMember struct { GroupID, AccountID string; Position int }`
  - `type VendorAccountGroupStore interface`:
    - `CreateVendorAccountGroup(ctx context.Context, g VendorAccountGroup) error`
    - `UpdateVendorAccountGroup(ctx context.Context, g VendorAccountGroup) error` — writes name, model_prefix, status, the three thresholds, updated_at; never owner_user_id or vendor; unknown id `ErrNotFound`
    - `DeleteVendorAccountGroup(ctx context.Context, id string) error` — unknown id `ErrNotFound`; cascades members
    - `VendorAccountGroupByID(ctx context.Context, id string) (VendorAccountGroup, error)` — `ErrNotFound`
    - `VendorAccountGroupsByOwner(ctx context.Context, userID string) ([]VendorAccountGroup, error)` — id order, never nil
    - `VendorAccountGroupMembers(ctx context.Context, groupID string) ([]VendorAccountGroupMember, error)` — position order, never nil
    - `SetVendorAccountGroupMembers(ctx context.Context, groupID string, accountIDs []string) error` — atomic replace, positions 1..n in slice order; unknown group or account `ErrNotFound`; duplicate id `ErrConflict`; a rejected set leaves the old one

- [ ] **Step 1: Write the failing conformance tests** in `vendor_account_group_conformance_test.go`, each inside `forEachRoutingStore`:
  - `TestVendorAccountGroupCRUD`: create → `VendorAccountGroupByID` equals input (times compared with `Equal`); `UpdateVendorAccountGroup` with changed `Vendor`/`OwnerUserID` leaves both unchanged and changes name/prefix/status/thresholds; `VendorAccountGroupsByOwner` returns id order and `[]` (non-nil) for a stranger; delete → `ErrNotFound` on read and on a second delete.
  - `TestVendorAccountGroupMembersReplaceAndOrder`: set `[b, a]` → positions `b=1, a=2`; set `[a]` → only `a`, position 1; `[a, a]` → `errors.Is(err, routing.ErrConflict)` and the previous set is intact; unknown account → `ErrNotFound`.
  - `TestVendorAccountGroupCascades`: deleting a member account removes its member row; deleting the group removes all member rows; deleting the owner user removes the group (SQL drivers; memory: the store has no user delete, skip with `t.Skip` when the driver is memory, matching existing cascade tests).
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store -run 'TestVendorAccountGroup' -count=1` — expect FAIL (undefined types).
- [ ] **Step 3: Implement** the types in `vendor_groups.go`, the interface in `store.go`, and the memory driver in `memory_store_vendor_groups.go` (maps `vendorGroups map[string]VendorAccountGroup`, `vendorGroupMembers map[string][]string` under the store's existing mutex; `DeleteVendorAccount` also removes the account from every member list).
- [ ] **Step 4: Run** the same command — memory subtests PASS; SQL subtests fail to compile only if `SQLStore` lacks the methods: add stubs returning `errors.New("not implemented")` so the package builds, SQL subtests FAIL.
- [ ] **Step 5: Commit** `feat(routing): vendor account group entity and memory store`.

### Task 2: Migration 114 and SQL store

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append `{version: 114, name: "vendor_account_groups", up: migration114Up}`; template `migration92Up` at :4164)
- Create: `gateway/backend/internal/store/sqlite_vendor_account_groups.go` (replace the Task 1 stubs)
- Create: `gateway/backend/internal/store/migration114_vendor_account_groups_test.go`

**Interfaces:**
- Consumes: Task 1 interface.
- Produces: tables `vendor_account_groups(id text primary key, owner_user_id text not null references users(id) on delete cascade, vendor text not null, name text not null, model_prefix text not null default '', status text not null default 'active', threshold_five_hour_pct integer not null default 99, threshold_weekly_pct integer not null default 99, threshold_credits_pct integer not null default 99, created_at ts not null, updated_at ts not null)` with index on `owner_user_id`; `vendor_account_group_members(group_id text not null references vendor_account_groups(id) on delete cascade, vendor_account_id text not null references vendor_accounts(id) on delete cascade, position integer not null, unique(group_id, vendor_account_id), unique(group_id, position))` with index on `vendor_account_id`; column `usage_events.vendor_group_id text not null default ''` via `addColumnIfMissing`.

- [ ] **Step 1: Write the failing migration test** `TestMigration114CreatesVendorAccountGroupTables`: on a store migrated to 113 with one usage row, migrate to 114; assert both tables exist, `usage_events.vendor_group_id` reads `''` for the old row, and re-running the migration is a no-op (pattern: `migration110_vendor_account_share_usage_test.go`).
- [ ] **Step 2: Run** `go test ./internal/store -run 'TestMigration114|TestVendorAccountGroup' -count=1` with and without `OP_AI_GATEWAY_TEST_POSTGRES_DSN` — expect FAIL.
- [ ] **Step 3: Implement** `migration114Up(ctx, tx, dl)` with `dl.timestampType()`, and the seven SQL methods (`SetVendorAccountGroupMembers` in one transaction: delete then insert; map FK violation to `ErrNotFound`, unique violation to `ErrConflict` using the helpers `sqlite_vendor_accounts.go` already uses).
- [ ] **Step 4: Run** the Step 2 command on sqlite and postgres — PASS (all Task 1 conformance subtests too). Also run `go test ./internal/store -run 'ColumnParity|Conformance' -count=1`.
- [ ] **Step 5: Commit** `feat(store): migration 114 vendor account groups`.

### Task 3: Usage attribution of the group

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`Target`, field docs near :292-316: add `VendorGroupID string`)
- Modify: `gateway/backend/internal/usage/recorder.go:42` (`Event.VendorGroupID string`)
- Modify: `gateway/backend/internal/store/sqlite_usage.go` (insert/select `vendor_group_id`)
- Modify: memory usage store if it keeps events (follow where `AccountID` is copied)
- Modify: `gateway/backend/internal/gateway/inference_complete.go:778` (set `VendorGroupID: target.VendorGroupID`)
- Test: `gateway/backend/internal/gateway/vendor_account_attribution_test.go`

**Interfaces:**
- Produces: `routing.Target.VendorGroupID`, `usage.Event.VendorGroupID`.

- [ ] **Step 1: Write the failing test** `TestRecordUsageStampsVendorGroupID`: record a usage event for a target with `VendorAccountID: "va_1", VendorGroupID: "vag_1"`; the stored event has `AccountID == "va_1"` and `VendorGroupID == "vag_1"`; a target without a group stores `""`.
- [ ] **Step 2: Run** `go test ./internal/gateway -run TestRecordUsageStampsVendorGroupID -count=1` — FAIL.
- [ ] **Step 3: Implement** the field plumbing.
- [ ] **Step 4: Run** the test and `go test ./internal/store -run Usage -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(usage): attribute vendor-account usage to its group`.

### Task 4: "Nutzung teilen" defaults to on for new accounts (spec §10.1)

**Files:**
- Modify: `gateway/backend/internal/store/sqlite_vendor_accounts.go` (`CreateVendorAccount` inserts `share_usage_with_members` from the struct)
- Modify: `gateway/backend/internal/routing/memory_store.go` (`CreateVendorAccount` keeps `ShareUsageWithMembers`)
- Modify: `gateway/backend/internal/routing/store.go` (`VendorAccount.ShareUsageWithMembers` doc: now also written by create)
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go` (`CreateVendorAccountRequest.ShareUsageWithMembers *bool`, json `share_usage_with_members,omitempty`; create sets `true` when nil)
- Modify: `gateway/frontend/src/api/vendorAccounts.ts` (create request field), the create form in `gateway/frontend/src/components/VendorAccountsView.tsx` (toggle, default ticked), `i18n.ts` (label + hint, de/en)
- Tests: `gateway/backend/internal/store/vendor_account_share_usage_test.go`, `gateway/backend/internal/gateway/portal_vendor_account_share_usage_test.go`, `gateway/frontend/src/components/VendorAccountsView.test.tsx`

- [ ] **Step 1: Write failing tests:**
  - store (all drivers): `TestCreateVendorAccountPersistsShareUsage` — create with `ShareUsageWithMembers: true` reads back `true`; with `false` reads back `false`.
  - portal: `TestCreateVendorAccountSharesUsageByDefault` — POST without the field → DTO `share_usage_with_members == true`; POST with `false` → `false`; an existing account (created directly in the store with `false`) stays `false` after a GET/PATCH round trip.
  - frontend: the create form renders the share-usage checkbox checked and sends `share_usage_with_members: true`; unticking sends `false`.
- [ ] **Step 2: Run** `go test ./internal/store ./internal/gateway -run 'ShareUsage' -count=1` and `cd gateway/frontend && npx vitest run src/components/VendorAccountsView.test.tsx` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** both commands — PASS (sqlite + postgres).
- [ ] **Step 5: Commit** `feat(portal): share vendor account usage with members by default for new accounts`.

---

## Phase B — Policy and selection core

### Task 5: Token policy carries groups

**Files:**
- Modify: `gateway/backend/internal/auth/vendor_access.go`
- Modify: `gateway/backend/internal/store/token_vendor_access.go` (wire codec)
- Modify: `gateway/backend/internal/routing/vendor_access.go`
- Modify: `gateway/backend/internal/gateway/auth.go:337` (`sessionPrincipal`: `VendorAccess{All: true, IncludeGroups: true}`)
- Tests: `gateway/backend/internal/store/token_vendor_access_test.go`, `gateway/backend/internal/routing/vendor_access_test.go` (create if absent), `gateway/backend/internal/gateway/auth_session_vendor_test.go` (or the existing session-principal test file)

**Interfaces:**
- Produces:
  - `auth.VendorAccessGroupEntry{ GroupID string; OverrideEnabled bool; OverridePrefix string }`
  - `auth.VendorAccess{ All bool; IncludeGroups bool; Accounts []VendorAccessEntry; Groups []VendorAccessGroupEntry }` (`cloneVendorAccess` deep-copies both slices)
  - Wire: `{"all":bool,"include_groups":bool,"accounts":[…],"groups":[{"group_id":"vag_…","prefix_override":{"enabled":bool,"value":string}}]}`; `include_groups` and `groups` omitted when zero; decode tolerant (blank/malformed → zero policy, entries without `group_id` dropped).
  - `routing.TokenOwnsVendorGroup(token auth.Token, g VendorAccountGroup) bool` — `token.UserID != "" && g.OwnerUserID == token.UserID`
  - `routing.TokenVendorGroupPrefix(token auth.Token, g VendorAccountGroup) (prefix string, allowed bool)` — not owned → denied; `All && IncludeGroups` → `g.ModelPrefix` (a listed entry's override is ignored, mirroring "all wins" for own accounts); listed → override value if enabled else `g.ModelPrefix`; otherwise denied.

- [ ] **Step 1: Write failing tests:**
  - codec: round trip of a policy with two group entries (one with an enabled empty override) is lossless; an old row `{"all":true}` decodes with `IncludeGroups == false` and `Groups == nil`; `{"groups":[{"prefix_override":{"enabled":true}}]}` drops the id-less entry; encoding the zero policy still yields `""`.
  - prefix table test `TestTokenVendorGroupPrefix`: owned + `All && IncludeGroups` → (`g.ModelPrefix`, true); owned + `All` only → (`""`, false); listed with override `""` enabled → (`""`, true); listed without override → (`g.ModelPrefix`, true); group of another user even when listed → (`""`, false); service token (no `UserID`) → denied.
  - session principal: `sessionPrincipal(...)` has `VendorAccess.All && VendorAccess.IncludeGroups`.
- [ ] **Step 2: Run** `go test ./internal/store ./internal/routing ./internal/gateway -run 'VendorAccess|VendorGroupPrefix|SessionPrincipal' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Step 2 command plus `go test ./internal/auth ./internal/portal -count=1` (DTO round trips must still pass) — PASS.
- [ ] **Step 5: Commit** `feat(auth): vendor access policy carries groups and include_groups`.

### Task 6: Pure member selection and the limit-cooldown registry

**Files:**
- Create: `gateway/backend/internal/routing/vendor_group_select.go`
- Create: `gateway/backend/internal/routing/vendor_cooldowns.go`
- Tests: `gateway/backend/internal/routing/vendor_group_select_test.go`, `gateway/backend/internal/routing/vendor_cooldowns_test.go`

**Interfaces:**
- Produces:
  - `type VendorGroupThresholds struct { FiveHourPct, WeeklyPct, CreditsPct int }` and `func (g VendorAccountGroup) Thresholds() VendorGroupThresholds`
  - `type VendorGroupCandidate struct { AccountID string; Usable bool; Usage VendorAccountUsage; HasUsage bool; ThresholdsApply bool; CooldownUntil time.Time }` — `Usable` = spec §5.3 rules 1–3; `ThresholdsApply` = member owner is the group owner **or** `acc.ShareUsageWithMembers`; zero `CooldownUntil` = none
  - `const VendorBlockFiveHour = "five_hour"`, `VendorBlockWeekly = "weekly"`, `VendorBlockCredits = "credits"`, `VendorBlockCooldown = "cooldown"`
  - `type VendorMemberBlock struct { Category string; Pct float64; FreeAt *time.Time }`
  - `func VendorGroupMemberBlocks(c VendorGroupCandidate, t VendorGroupThresholds, now time.Time) []VendorMemberBlock` — threshold blocks only when `ThresholdsApply && HasUsage`; a category blocks when `pct >= 0 && pct >= threshold` and its reset is nil or after `now` (five-hour: `FiveHourPct`/`FiveHourResetAt`; weekly: `WeeklyPct`/`WeeklyResetAt`; credits: `SpendUsedPct`/`SpendResetAt`); a cooldown block when `CooldownUntil.After(now)` (`FreeAt` = cooldown end); result ordered five_hour, weekly, credits, cooldown
  - `func VendorGroupMemberFreeAt(blocks []VendorMemberBlock) (time.Time, bool)` — the **latest** `FreeAt`; false when any block has nil `FreeAt`
  - `func SelectVendorGroupMember(cands []VendorGroupCandidate, t VendorGroupThresholds, now time.Time, allowFallback bool) (index int, ok bool)` — first usable candidate with no blocks; else, when `allowFallback`, the usable candidate with the earliest `VendorGroupMemberFreeAt` (unknown sorts last, ties by index); else `(-1, false)`; no usable candidate → `(-1, false)`
  - `const DefaultVendorLimitCooldown = 60 * time.Second`
  - `type VendorCooldown struct { UpstreamModel string; Until time.Time }`
  - `type VendorLimitCooldowns struct` (mutex + map keyed by account id and upstream model), `func NewVendorLimitCooldowns() *VendorLimitCooldowns`, `func (c *VendorLimitCooldowns) Set(accountID, upstreamModel string, until time.Time)`, `func (c *VendorLimitCooldowns) Until(accountID, upstreamModel string, now time.Time) (time.Time, bool)` (expired entries read as absent and are deleted), `func (c *VendorLimitCooldowns) Active(accountID string, now time.Time) []VendorCooldown` (sorted by model). A nil `*VendorLimitCooldowns` is safe: `Set` no-op, `Until` absent, `Active` nil.

- [ ] **Step 1: Write failing table tests** in `vendor_group_select_test.go` (now = fixed instant):
  - five-hour 99 with reset now+1h, threshold 99 → index 1 chosen over index 0.
  - **stale:** five-hour 99.5 with reset now−1m → index 0 chosen (Review Focus 3).
  - weekly 100 with nil reset → blocked (`FreeAt == nil`).
  - credits `SpendUsedPct` 99 with threshold 99 → blocked; with threshold 100 → not blocked.
  - unknown values (`-1`) never block; `HasUsage == false` never blocks.
  - `ThresholdsApply == false` with five-hour 100 → not blocked (shared member without usage opt-in).
  - cooldown until now+30s → blocked with `FreeAt` = now+30s; a cooldown in the past → not blocked.
  - all blocked, `allowFallback`: member A five-hour reset now+2h, member B five-hour reset now+30m **and** weekly reset now+3d → A chosen (B's free-at is the later of its two); a member with an unknown reset sorts last; ties keep index order.
  - all blocked, `allowFallback == false` → `(-1, false)`.
  - no usable candidate → `(-1, false)` regardless of fallback.
- [ ] **Step 2: Write failing tests** in `vendor_cooldowns_test.go`: `Set`/`Until` per `(account, model)` (a cooldown on `claude-opus` does not block `claude-sonnet`); expiry deletes; `Active` sorted; nil receiver safe; `-race` clean under concurrent `Set`/`Until`.
- [ ] **Step 3: Run** `go test ./internal/routing -run 'VendorGroup|VendorLimitCooldown' -race -count=1` — FAIL.
- [ ] **Step 4: Implement** both files.
- [ ] **Step 5: Run** the Step 3 command — PASS.
- [ ] **Step 6: Commit** `feat(routing): vendor group member selection and limit cooldowns`.

---

## Phase C — Resolver and listing

### Task 7: Resolver group step

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`resolveVendorAccount` :1175, `vendorAccountServes` :1206, resolver store interface, setters near :746/:776)
- Modify: `gateway/backend/internal/inference/types.go:320` (`Request.ExcludeVendorAccountIDs []string`)
- Create: `gateway/backend/internal/routing/resolver_vendor_group.go`
- Modify: `gateway/backend/internal/gateway/server.go` (create one `*routing.VendorLimitCooldowns` on `Server` as `vendorCooldowns`; pass it to the resolver; wire the usage puller in Task 14)
- Test: `gateway/backend/internal/routing/resolver_vendor_group_test.go` (pattern: `resolver_vendor_sharing_test.go`)

**Interfaces:**
- Consumes: Tasks 1, 3, 5, 6.
- Produces:
  - `func (r *Resolver) SetVendorLimitCooldowns(c *VendorLimitCooldowns)`
  - `type VendorUsagePuller interface { RequestVendorUsagePull(accountID string) }` and `func (r *Resolver) SetVendorUsagePuller(p VendorUsagePuller)` (nil = no pulls)
  - `func (r *Resolver) vendorAccountUsable(acc VendorAccount, apiFlavor string, decision bool) bool` — today's `vendorAccountServes` minus the `TokenVendorPrefix` check; the account loop calls `TokenVendorPrefix` itself afterwards
  - `func (r *Resolver) resolveVendorGroup(ctx context.Context, token auth.Token, req inference.Request, apiFlavor string, reachable []VendorAccount) (Target, bool, error)` — called first inside `resolveVendorAccount` with the accounts from `routableVendorAccounts`; for each own active group (`VendorAccountGroupsByOwner(token.UserID)`, id order) the token uses (`TokenVendorGroupPrefix`) whose prefix is a prefix of `req.Model`: upstream = `strings.TrimPrefix(req.Model, prefix)`; build one `VendorGroupCandidate` per member (`Usable` = member account is in `reachable`, has the group's vendor, `vendorAccountUsable`, not in `req.ExcludeVendorAccountIDs`, and serves a model row with `UpstreamModel == upstream`; usage from `VendorAccountUsageByID`; cooldown from the registry); `SelectVendorGroupMember(..., allowFallback: len(req.ExcludeVendorAccountIDs) == 0)`; target = `vendorAccountModelTarget(acc, row, req, apiFlavor)` with `VendorGroupID = g.ID`. A group with no usable candidate yields no match and the next group / the account loop runs.
  - Pull trigger: for each skipped member whose blocks contain a threshold block with nil `FreeAt`, call `RequestVendorUsagePull(accountID)` (the puller throttles).

- [ ] **Step 1: Write failing tests** (memory store, stub enumerator as in `resolver_vendor_sharing_test.go`):
  - `TestResolveVendorGroupPicksFirstBelowThreshold`: group [subA (five-hour 99, reset +1h), keyB] with an `all + include_groups` token → target account keyB, `VendorGroupID` set, `ProviderModel == "claude-opus-4-5"`.
  - `TestResolveVendorGroupShadowsOwnMember`: subA has prefix `""`, group has prefix `""` and starts with subA; request `claude-opus-4-5` → target carries the group id (group step runs before the account loop).
  - `TestResolveVendorGroupFallsBackToSoonestFree`: both members blocked → the earlier free-at member, group id set.
  - `TestResolveVendorGroupExclusionDisablesFallback`: `ExcludeVendorAccountIDs: [subA]`, keyB blocked → no group match (resolution continues; with nothing else → `ErrNoModelRoute`).
  - `TestResolveVendorGroupAllMembersUnreachable` (Review Focus 2): shared member no longer in the enumerator, own member disabled → no group match, falls through to the account loop / standard path; no error.
  - `TestResolveVendorGroupSkipsCooldownForThatModelOnly`: cooldown on (subA, opus) → opus goes to keyB, sonnet stays on subA.
  - `TestResolveVendorGroupSharedMemberWithoutUsageOptIn`: shared member at five-hour 100 with `ShareUsageWithMembers == false` → still chosen.
  - `TestResolveVendorGroupIgnoresForeignAndStaleIDs` (Review Focus 4): token lists a deleted group id and another user's group → no panic, no match.
  - `TestResolveVendorGroupRequestsPullForUnknownReset`: blocked member with weekly 100 and nil reset → the stub puller recorded that account id.
  - Existing `resolver_vendor_*_test.go` stay green (refactor safety).
- [ ] **Step 2: Run** `go test ./internal/routing -run 'ResolveVendor|Resolver.*Vendor' -count=1` — new tests FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/routing -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(routing): resolve vendor account groups before single accounts`.

### Task 8: Group models in the listings

**Files:**
- Modify: `gateway/backend/internal/portal/service_vendor_listing.go` (`ownVendorAccountModels` :55, `advertisedVendorAccountModels`, `vendorAccountModels` :135, `vendorDashboardRoutes` :220)
- Test: `gateway/backend/internal/portal/service_vendor_listing_groups_test.go`

**Interfaces:**
- Consumes: Tasks 1, 5; `routing.TokenVendorGroupPrefix`.
- Produces: `vendorAccountModels` gains `GroupID, GroupName string`. `advertisedVendorAccountModels` first emits, for each own active group the token uses, one entry **per usable member** (active, subscriptions switch, reachable, same vendor) with that member's model rows relabeled to the group prefix (`relabelVendorModels(models, groupPrefix)`) and `GroupID/GroupName` set; then the account entries as today. Thresholds and cooldowns do **not** affect listing. `vendorDashboardRoutes` labels a group row's host as `"<group name> · <account name>"`.

- [ ] **Step 1: Write failing tests:**
  - `TestVendorListingAdvertisesGroupModels`: group "claude/" over two accounts with overlapping models → `/v1/models` (via `modelsResponse`) lists each `claude/<model>` once; flavors are the union of the members' flavors.
  - `TestVendorListingSkipsGroupWithoutTokenOptIn`: token with `All` but not `IncludeGroups` → no `claude/…` names.
  - `TestVendorListingGroupSkipsUnusableMembers`: a disabled member's exclusive model is not listed under the group.
  - parity check: every listed group name resolves through the resolver of Task 7 for the same token (reuse the existing listing/resolver parity helper if present; otherwise resolve each name with a `Resolver` over the same memory store).
- [ ] **Step 2: Run** `go test ./internal/portal -run 'VendorListing' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/portal -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(portal): advertise vendor group models under the group prefix`.

---

## Phase D — Portal service and API

### Task 9: Group CRUD, member states and endpoints

**Files:**
- Create: `gateway/backend/internal/portal/service_vendor_groups.go`
- Create: `gateway/backend/internal/gateway/portal_vendor_group_endpoints.go`
- Modify: `gateway/backend/internal/gateway/server.go:1825` (routes; also hand `s.vendorCooldowns` to the portal service through the new setter), `gateway/backend/internal/portal/service.go` (error vars near :62; `func (s *Service) SetVendorLimitCooldowns(c *routing.VendorLimitCooldowns)`)
- Tests: `gateway/backend/internal/portal/service_vendor_groups_test.go`, `gateway/backend/internal/gateway/portal_vendor_group_endpoints_test.go`

**Interfaces:**
- Consumes: Tasks 1, 6.
- Produces:
  - Requests: `CreateVendorAccountGroupRequest{ Name, Vendor, ModelPrefix string; Status string; ThresholdFiveHourPct, ThresholdWeeklyPct, ThresholdCreditsPct *int; AccountIDs []string }` (json snake_case; nil thresholds → 99), `UpdateVendorAccountGroupRequest{ Name, ModelPrefix, Status *string; ThresholdFiveHourPct, ThresholdWeeklyPct, ThresholdCreditsPct *int }`, members body `{ "account_ids": [...] }`.
  - DTOs: `VendorAccountGroupDTO{ ID, Vendor, Name, ModelPrefix, Status string; Thresholds VendorGroupThresholdsDTO; ActiveMemberID string; Members []VendorAccountGroupMemberDTO; CreatedAt, UpdatedAt time.Time }`, `VendorGroupThresholdsDTO{ FiveHourPct, WeeklyPct, CreditsPct int }` (json `five_hour_pct`, `weekly_pct`, `credits_pct`), `VendorAccountGroupMemberDTO{ AccountID, AccountName, AuthType string; OwnerName string (non-empty only for a shared member); Position int; State string; Reason string; Pct *float64; FreeAt *time.Time; Cooldowns []VendorCooldownDTO; Usage *VendorAccountUsageDTO }` with `State` ∈ `active | available | over_threshold | cooldown | not_usable`, `Reason` ∈ `five_hour | weekly | credits | cooldown | disabled | needs_reconnect | subscriptions_disabled | not_reachable | ""`; `VendorCooldownDTO{ Model string; Until time.Time }`; `VendorAccountGroupListResponse{ Groups []VendorAccountGroupDTO }`.
  - Methods on `*Service`: `ListVendorAccountGroups(ctx, principal) (VendorAccountGroupListResponse, error)`, `GetVendorAccountGroup(ctx, principal, id) (VendorAccountGroupDTO, error)`, `CreateVendorAccountGroup(ctx, principal, req) (VendorAccountGroupDTO, error)`, `UpdateVendorAccountGroup(ctx, principal, id, req) (VendorAccountGroupDTO, error)`, `DeleteVendorAccountGroup(ctx, principal, id) error`, `SetVendorAccountGroupMembers(ctx, principal, id string, accountIDs []string) (VendorAccountGroupDTO, error)`.
  - Rules: module flag first (`vendor_accounts.module_disabled`); owner-only (anyone else, unknown id → `vendor_account_group.not_found` 404); vendor via `normalizeVendorAccountVendor`, immutable; prefix via `normalizeVendorAccountModelPrefix`; status `active|disabled`; thresholds 1..100; members: same vendor (`vendor_account_group.member_vendor_mismatch`), reachable by the owner now via `VendorAccountsForRouting` (`vendor_account_group.member_unreachable`), no duplicates (`vendor_account_group.member_duplicate`); prefix conflict check (spec §5.1) against own accounts **outside** the group and other own groups (`vendor_account_group.prefix_conflict`), run on create, prefix update and member change.
  - Member state: `active` = the index `SelectVendorGroupMember(..., allowFallback: true)` returns, computed with **rule 3 ignored** (`Usable` = rules 1–2 only, spec §9); usable members after it with no blocks → `available`; blocked → `over_threshold` (first threshold block: `Reason`, `Pct`, `FreeAt` = `VendorGroupMemberFreeAt`) or `cooldown` (`FreeAt` = latest cooldown end, `Cooldowns` from `Active`); unusable → `not_usable` with reason. For a member whose `ThresholdsApply` is false, `Pct`, `Usage` and threshold reasons are never set (spec §9); its cooldown `FreeAt` comes from the cooldown only.
  - Endpoints: `GET, POST /api/portal/vendor-account-groups`; `GET, PATCH, DELETE /api/portal/vendor-account-groups/{id}`; `PUT /api/portal/vendor-account-groups/{id}/members`. Same auth/CSRF/scope as `/api/portal/vendor-accounts` (copy its handler preamble). HTTP mapping: validation codes 400, not found 404, module disabled 409; uncategorized `vendor_account_group.failed` 500.

- [ ] **Step 1: Write failing service tests:** create with defaults (thresholds 99, status active, members positioned); stranger read/update/delete → not found; vendor mismatch / unreachable shared account / duplicate → the codes above; prefix `""` colliding with an own non-member account with prefix `""` → `prefix_conflict`, but colliding only with a **member** → allowed; member states for: active position 1; position 1 over five-hour with free-at → `over_threshold`/`five_hour` and position 2 `active`; shared member without usage opt-in at 100 % → `active`, no `Pct`/`Usage`; disabled member → `not_usable`/`disabled`; a revoked share → `not_usable`/`not_reachable`; cooldown entry → `cooldown` with `Cooldowns`.
- [ ] **Step 2: Write failing endpoint tests:** each route's status codes, CSRF required for session writes, module flag off → 409 `vendor_accounts.module_disabled`, JSON field names as in the DTOs.
- [ ] **Step 3: Run** `go test ./internal/portal ./internal/gateway -run 'VendorAccountGroup|VendorGroup' -count=1` — FAIL.
- [ ] **Step 4: Implement.**
- [ ] **Step 5: Run** the Step 3 command — PASS.
- [ ] **Step 6: Commit** `feat(portal): vendor account group CRUD, member states and endpoints`.

### Task 10: Token save-time checks and DTO for groups

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (`VendorAccessDTO` :1290-1309, `validateVendorAccess` :6100, `checkVendorAccessEntry` :6168, `claimVendorAccessNames` :6208)
- Modify: `gateway/backend/internal/portal/service_services.go:791` (service tokens)
- Tests: `gateway/backend/internal/portal/service_token_vendor_groups_test.go`

**Interfaces:**
- Consumes: Tasks 1, 5, 8.
- Produces: `VendorAccessDTO` gains `IncludeGroups bool` (`include_groups,omitempty`) and `Groups []VendorAccessGroupDTO` (`groups,omitempty`) with `VendorAccessGroupDTO{ GroupID string; PrefixOverride <the same override type the account entries of VendorAccessDTO use> }` (json `group_id`, `prefix_override`).
- Rules: a group entry must name a group the token's principal owns (else `portal.token_vendor_access_invalid`); duplicate group entry → invalid; service token with any group entry or `include_groups` → invalid (a service owns no group); name claiming per opted-in source — a group claims `prefix + upstream` over its usable members' models; a clash between a group and **one of its own members** is not a conflict (the group shadows it); any other clash → `portal.token_vendor_access_conflict`. `all + include_groups` without listed groups claims the owner's active groups too, so a clash with an own non-member account is reported (group save already prevents it; this is the backstop).

- [ ] **Step 1: Write failing tests:** owned group entry accepted and round-trips in the DTO; another user's group → invalid; repeated group → invalid; service token with a group → invalid; group "" + own member account "" under `all + include_groups` → accepted; group "" + own **non-member** account "" (inserted directly in the store, bypassing group save) → conflict; old policy without groups still validates unchanged.
- [ ] **Step 2: Run** `go test ./internal/portal -run 'TokenVendor' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/portal ./internal/gateway -run 'Token' -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(portal): token vendor access validates groups`.

---

## Phase E — Failover and usage data

### Task 11: Limit errors on the translate path

**Files:**
- Modify: `gateway/backend/internal/provider/client.go` (:48-58)
- Modify: the vendor clients' non-2xx branches: `anthropic_messages.go` (Complete :198 and its stream), `openai_responses.go` (Complete :95 and stream), `openai_compatible.go` (Complete :45 and stream), `gemini.go` (Complete :70 and stream)
- Create: `gateway/backend/internal/gateway/vendor_group_failover.go`
- Modify: `gateway/backend/internal/gateway/inference_complete.go` (`complete` after the instance re-select block :87-99), `gateway/backend/internal/gateway/stream_session.go` (`reselect` path guarded by `delivered`, :226-290)
- Tests: `gateway/backend/internal/provider/limit_error_test.go`, `gateway/backend/internal/gateway/vendor_group_failover_translate_test.go`

**Interfaces:**
- Produces:
  - `type LimitError struct { Status int; RetryAfter time.Duration; Header http.Header }` in package `provider`, `Error() string`, `Unwrap() error { return ErrUnavailable }` (so the final client mapping stays 502 `provider.unavailable`)
  - `func vendorStatusError(resp *http.Response) error` — `*LimitError` for 429/402 (parse `Retry-After` seconds or HTTP date; clone headers), else `unavailableStatus(resp.StatusCode)`
  - In `gateway`: `func vendorLimitCooldownUntil(h http.Header, retryAfter time.Duration, now time.Time) time.Time` — earliest of: `now+retryAfter` (if > 0); `anthropic-ratelimit-unified-reset` (epoch seconds); the earliest `x-codex-primary-reset-at` / `x-codex-secondary-reset-at` whose used-percent is ≥ 100; default `now + routing.DefaultVendorLimitCooldown`. Reuse `parseEpochSeconds` / header constants from `vendor_usage_scrape.go`.
  - `func withExcludedVendorAccount(req inference.Request, accountID string) inference.Request` (copies the slice, like `withLostInstance`)
  - `func (s *Server) vendorGroupFailover(ctx context.Context, token auth.Token, req inference.Request, target routing.Target, h http.Header, retryAfter time.Duration) (inference.Request, bool)` — only when `target.VendorGroupID != ""`: scrape `h` into the snapshot (`scrapeVendorAccountUsage`), set the cooldown `(target.VendorAccountID, target.ProviderModel)`, build the excluded request, and return ok only if `s.resolveTarget` for it yields a target with the same `VendorGroupID` (another member); otherwise false and the caller answers with the original error.

- [ ] **Step 1: Write failing tests:**
  - provider: a 429 with `Retry-After: 120` → `errors.As(err, &le)` with `Status 429`, `RetryAfter 120s`, and `errors.Is(err, ErrUnavailable)`; 402 likewise; 500 → not a `LimitError`.
  - `vendorLimitCooldownUntil`: Retry-After 30 s vs unified reset in 1 h → now+30s; no headers → now+60s.
  - `TestTranslateFailsOverOnVendorLimit`: two httptest vendors in one group (member 1 answers 429, member 2 200) → client gets 200 from member 2; one usage row with member 2's account id and the group id; cooldown set for (member 1, model).
  - `TestTranslateStreamDoesNotFailOverAfterDelivery` (Review Focus 1): member 1 streams one chunk then the stream fails with a limit error → no second upstream call; client sees the partial stream and the existing error handling.
  - `TestTranslateLastMemberLimitAnswersOriginalError`: both members 429 → 502 `provider.unavailable`, exactly two upstream calls.
  - single-account (no group) 429 → behaviour unchanged (one call, 502).
- [ ] **Step 2: Run** `go test ./internal/provider ./internal/gateway -run 'LimitError|VendorLimit|Translate.*Vendor' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/provider ./internal/gateway -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(gateway): fail over vendor groups on limit errors in the translate path`.

### Task 12: Limit errors on the passthrough path

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`nativeRelay` :1396, `relayPassthroughTarget` :438-480, `proxyNative` after the instance-gone block :858-878)
- Test: `gateway/backend/internal/gateway/vendor_group_failover_passthrough_test.go` (pattern: `vendor_apikey_messages_dispatch_test.go`)

**Interfaces:**
- Consumes: Task 11 helpers.
- Produces: `nativeRelay.vendorFailover func(lost routing.Target, h http.Header, retryAfter time.Duration) bool` — set by `relayPassthroughTarget` for `/v1/messages` and `/v1/responses`; it calls `vendorGroupFailover` and, when ok, relays the excluded request to the new target through `relayPassthroughTarget` and returns true. In `proxyNative`: when `rel.target.VendorGroupID != ""`, `rel.vendorFailover != nil` and the status is 429/402, call it **before** writing anything; if it returns true, stop the watchdog, close the body, `cancel()`, `s.Active.Remove(id)`, return; if false, relay the upstream answer as today (header allowlist and entitlement unchanged).

- [ ] **Step 1: Write failing tests:**
  - `TestPassthroughMessagesFailsOverOnVendor429`: group [anthropic api-key A → 429 with `retry-after: 5` and unified headers, B → 200 stream] → client gets B's bytes unchanged; A got exactly one call; usage row = B + group id.
  - `TestPassthroughAllMembers429RelaysLast` (Review Focus 5): three members all 429 → three upstream calls, one each; the client gets the third 429 body with `retry-after`; one usage row.
  - `TestPassthroughSingleAccount429Unchanged`: no group → relayed as today, one call.
  - `/v1/responses` variant of the first test with OpenAI api-key accounts.
- [ ] **Step 2: Run** `go test ./internal/gateway -run 'Passthrough.*Vendor|Passthrough.*Members' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/gateway -count=1` — PASS (watch the known load flakes listed in memory; re-run a flaky one in isolation before chasing it).
- [ ] **Step 5: Commit** `feat(gateway): fail over vendor groups on limit errors in the passthrough path`.

### Task 13: No stale windows after a switch to a member without windows

**Files:**
- Modify: `gateway/backend/internal/gateway/ratelimit_window_cache.go` (`Attach` :188, `attachRateLimitWindows` :274), `gateway/backend/internal/gateway/codex_window_cache.go` (`attachCodexRateLimitWindows` :243)
- Test: `gateway/backend/internal/gateway/ratelimit_window_cache_group_test.go`

**Interfaces:**
- Produces: `func (c *rateLimitWindowCache) Forget(key string)` and the Codex twin; `attachRateLimitWindows(token, dst)` / `attachCodexRateLimitWindows` gain the served `routing.Target`: when `target.VendorGroupID != ""` and the served response carried no windows, they call `Forget(principalKey)` and attach nothing. A group member that does carry windows overwrites the entry as today.

- [ ] **Step 1: Write failing tests:** principal has a cached subscription window; a group-served response from an api-key member without unified headers → no `anthropic-ratelimit-unified-*` header attached and the cache entry is gone; a non-group local-model response still gets the cached windows (unchanged #407 behaviour); Codex twin.
- [ ] **Step 2: Run** `go test ./internal/gateway -run 'RateLimitWindow|CodexWindow' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** the Step 2 command — PASS.
- [ ] **Step 5: Commit** `fix(gateway): drop cached limit windows when a group switches to a member without windows`.

### Task 14: Anthropic subscription usage pull and the throttled group pull

**Files:**
- Create: `gateway/backend/internal/vendorauth/anthropic_usage.go`
- Modify: `gateway/backend/internal/vendorauth/constants.go` (URL constant, `VERIFY-LIVE`)
- Modify: `gateway/backend/internal/portal/service_vendor_usage_refresh.go` (`hasActiveVendorUsagePull` :136, the pull dispatch)
- Create: `gateway/backend/internal/portal/service_vendor_group_pull.go`
- Modify: `gateway/backend/internal/gateway/server.go` (wire `Service` as the resolver's `VendorUsagePuller`)
- Tests: `gateway/backend/internal/vendorauth/anthropic_usage_test.go`, `gateway/backend/internal/portal/service_vendor_group_pull_test.go`

**Interfaces:**
- Produces:
  - `const AnthropicOAuthUsageURL = "https://api.anthropic.com/api/oauth/usage"` (VERIFY-LIVE)
  - `func FetchAnthropicSubscriptionUsage(ctx context.Context, client *http.Client, accessToken string) (Usage, error)` — GET with `Authorization: Bearer`, `anthropic-beta: AnthropicBeta`, the Claude Code user agent the subscription path already sends; parses `five_hour` / `seven_day` `{utilization (0..100), resets_at (RFC 3339)}`; a `null` window stays unknown (`-1`, nil); a 429 returns a typed `ErrUsageRateLimited`
  - `hasActiveVendorUsagePull` true for an Anthropic subscription; the existing refresh path dispatches to the new fetcher and writes through `MergeVendorAccountUsage`
  - `const vendorGroupUsagePullInterval = 15 * time.Minute`, `const vendorGroupUsagePullMaxBackoff = 4 * time.Hour`
  - `func (s *Service) RequestVendorUsagePull(accountID string)` — non-blocking; skips accounts without an active pull; at most one pull per account per interval (in-memory last-attempt map); after `ErrUsageRateLimited` the next allowed attempt doubles the wait up to the cap; runs on a detached context with the existing discovery timeout; never on the request path.

- [ ] **Step 1: Write failing tests:** fetcher against httptest — both windows parsed, `five_hour: null` → unknown five-hour, 429 → `ErrUsageRateLimited`, headers sent; pull throttle — two requests within 15 min trigger one fetch (fake clock), a rate-limited fetch delays the next to 30 min then 60 min, capped at 4 h; an OpenAI api-key account is never pulled.
- [ ] **Step 2: Run** `go test ./internal/vendorauth ./internal/portal -run 'AnthropicSubscriptionUsage|VendorGroupPull|ActiveVendorUsagePull' -count=1` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./internal/vendorauth ./internal/portal -count=1` — PASS.
- [ ] **Step 5: Commit** `feat(vendor): pull Claude subscription usage and refresh blocked group members`.

---

## Phase F — Portal UI

### Task 15: API client types

**Files:**
- Create: `gateway/frontend/src/api/vendorAccountGroups.ts` (+ `vendorAccountGroups.test.ts`)
- Modify: `gateway/frontend/src/api/tokens.ts` (`VendorAccessDTO`: `include_groups?: boolean; groups?: VendorAccessGroupDTO[]`; `VendorAccessGroupDTO = { group_id: string; prefix_override?: { enabled: boolean; value: string } }`)
- Modify: the place that composes the API object (where `vendorAccountsApi(fetcher)` is spread in)

**Interfaces:**
- Produces: types mirroring Task 9 DTOs (`VendorAccountGroup`, `VendorAccountGroupMember`, `VendorGroupMemberState`, `VendorGroupThresholds`) and `vendorAccountGroupsApi(fetcher)` with `vendorAccountGroups()`, `vendorAccountGroup(id)`, `createVendorAccountGroup(req)`, `updateVendorAccountGroup(id, req)`, `deleteVendorAccountGroup(id)`, `setVendorAccountGroupMembers(id, accountIds)`.

- [ ] **Step 1: Write failing tests** (pattern `vendorAccounts.test.ts`): each function hits the right method/path/body; responses typed through.
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/api/vendorAccountGroups.test.ts` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** it — PASS; `npx tsc --noEmit -p .` clean.
- [ ] **Step 5: Commit** `feat(portal-ui): vendor account group API client`.

### Task 16: Groups on the Anbieter page

**Files:**
- Create: `gateway/frontend/src/components/VendorAccountGroupsSection.tsx` (+ `.test.tsx`)
- Modify: `gateway/frontend/src/components/VendorAccountsView.tsx` (render the section below the accounts), `gateway/frontend/src/i18n.ts`

**Interfaces:**
- Consumes: Task 15.
- Produces: a section titled "Gruppen" / "Groups" listing the user's groups; create/edit dialog with name, vendor (select, disabled after create), prefix, status, three threshold number fields (1..100, default 99, hint "applies only where the account reports this category"), member list with position, up/down buttons, add (from the user's accounts of the group's vendor: own, managed and member accounts as `api.vendorAccounts()` returns them; shared ones labelled with their owner) and remove; each member row shows its state chip and, where present, `VendorAccountUsageRows` for its usage; delete with confirmation. Server errors map to inline messages for the `vendor_account_group.*` codes.

- [ ] **Step 1: Write failing tests:** renders groups and member states from a mocked API; creating sends thresholds 99 by default; moving a member down sends the reordered `account_ids`; accounts of another vendor are not offered; a `prefix_conflict` response shows the translated message; German and English labels exist.
- [ ] **Step 2: Run** `npx vitest run src/components/VendorAccountGroupsSection.test.tsx` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** it plus `npx vitest run src/components/VendorAccountsView` — PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): manage vendor account groups on the providers page`.

### Task 17: Token editor offers groups

**Files:**
- Modify: `gateway/frontend/src/components/TokenList.tsx` (:905-960), `gateway/frontend/src/components/shared/VendorAccessEntryGroups.tsx`, `gateway/frontend/src/components/shared/vendorAccessAccounts.ts`, `gateway/frontend/src/i18n.ts`
- Test: `gateway/frontend/src/components/TokenList.vendorGroups.test.tsx`

**Interfaces:**
- Consumes: Task 15.
- Produces: under "Provider access" a group checkbox list with the same prefix-override control as accounts; a nested checkbox "inkl. Gruppen" / "incl. groups" under "All own providers (incl. future)", enabled only while that is ticked (unticking the parent clears it); the request carries `include_groups` and `groups`; service-token create does **not** show groups.

- [ ] **Step 1: Write failing tests:** nested checkbox disabled until "all" is ticked; ticking a group and enabling an empty override sends `groups: [{group_id, prefix_override: {enabled: true, value: ''}}]`; editing a token with stored groups pre-ticks them; service token form shows no group list.
- [ ] **Step 2: Run** `npx vitest run src/components/TokenList.vendorGroups.test.tsx` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** it plus the existing `TokenList` tests — PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): token provider access offers vendor groups`.

### Task 18: Dashboard group cards

**Files:**
- Create: `gateway/frontend/src/components/DashboardVendorGroups.tsx` (+ `.test.tsx`)
- Modify: `gateway/frontend/src/components/Dashboard.tsx:117` (render next to `DashboardVendorUsage`), `gateway/frontend/src/i18n.ts`

**Interfaces:**
- Consumes: Task 15 (`vendorAccountGroups()`), polled on the same cadence as `DashboardVendorUsage`.
- Produces: one card per active group: group name, active member name, one line per member — "aktiv" / "bereit" / "über Schwelle: 5h 99 %, frei ab 14:20" / "gesperrt bis 14:05 (claude-opus-4-5)" / "nicht nutzbar: …" (en equivalents); a member without figures shows no percent. Nothing renders when the user has no groups.

- [ ] **Step 1: Write failing tests:** card shows the active member; an over-threshold member shows category, percent and local free-at time; a cooldown line names the model; a member without `pct` shows the state only; no groups → no card.
- [ ] **Step 2: Run** `npx vitest run src/components/DashboardVendorGroups.test.tsx` — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** it plus `npx vitest run src/components/Dashboard` — PASS.
- [ ] **Step 5: Commit** `feat(portal-ui): show the active member of each vendor group on the dashboard`.

---

## Phase G — Documentation and verification

### Task 19: Architecture docs

**Files:**
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md` (new §12 "Vendor account groups" with entity, selection, failover, usage pull, dashboard; edits in §1.3 usage opt-in default, §4.6 precedence, §5.2 Anthropic pull, §5.5 group pull, §6.7 listing, §11.1–§11.4 policy/`include_groups`/session)
- Modify: `docs/architecture/09-architecture-decisions.md` (new ADR at the next free number: groups personal, shadowing, per-model cooldown, status-only limit errors, thresholds skipped without usage opt-in, share-usage default on for new accounts; amend ADR-106's entitlement bullet wording "opted in" → "has it on, the default for new accounts")
- Modify: `docs/architecture/reference/data-model.md` (tables, `usage_events.vendor_group_id`, migration 114, ER diagram), `docs/architecture/reference/api-surface.md` + `openapi.yaml` (group endpoints, `vendor_access` fields, `share_usage_with_members` on create, error codes), `docs/architecture/12-glossary.md` ("Anbieter-Gruppe")
- Modify: `README.md` only if a feature list mentions provider accounts (add one line); no screenshot change is required unless the dashboard screenshot shows vendor cards

- [ ] **Step 1: Write the docs.**
- [ ] **Step 2: Run** `make lint-docs` — PASS (links, anchors, index reachability, openapi refs).
- [ ] **Step 3: Commit** `docs(architecture): vendor account groups`.

### Task 20: Full verification

- [ ] **Step 1:** `make lint` (Go both modules + docs) — PASS.
- [ ] **Step 2:** `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=… go test ./... -count=1` — PASS (known load flakes per memory: re-run in isolation before chasing).
- [ ] **Step 3:** `cd gateway/frontend && npm run format:check && npm run lint && npm test && npm run build` — PASS.
- [ ] **Step 4:** Sonar: `make sonar-scan`, then `make sonar-findings && make sonar-branch-findings`; fix branch-attributed findings.
- [ ] **Step 5:** Manual smoke on `make dev`: create two vendor accounts of one vendor (api-key accounts against a local stub), a group, a token with "all + incl. groups"; verify listing, a failover with the stub answering 429, and the dashboard card.
- [ ] **Step 6:** Final branch hygiene per `AGENTS.md`: fold durable content into `docs/architecture/`, delete `docs/superpowers/` and `docs/implementation-status.md`, verify with `git diff --name-only main...HEAD`, then push and open the PR (English text, substantive squash commit body).

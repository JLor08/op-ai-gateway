# External Vendor Accounts ("Anbieter") Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a portal user connect an external AI vendor account (OpenAI, Anthropic) — via a plain API key or a consumer-subscription OAuth login — and route their own requests through it, under a new "Anbieter" menu item, with a per-account usage/limits panel.

**Architecture:** A new first-class `vendor_account` entity (per-user owned, distinct from the overloaded "provider" term) stored across all three drivers. Serving reuses the existing routing `Target`/dispatch: the resolver gains an in-memory candidate source that turns a user's account + curated model catalog into an ordinary scored candidate, gated to its owner. Dispatch is extended with static extra headers and an optional Claude-Code system-prompt masquerade; a small native Anthropic Messages client is added for the translate path. A usage snapshot per account is scraped from vendor rate-limit response headers at the existing `recordUsage` choke point.

**Tech Stack:** Go 1.26 backend (`op-ai-gateway`, `internal/*`), React + TypeScript + Vite frontend (served under `/portal/`), SQLite/Postgres via one `SQLStore` + a `dialect` seam, in-memory `routing.MemoryStore`. AES-256-GCM secret envelope (`internal/capture`). Design spec: `docs/superpowers/specs/2026-10-07-vendor-accounts-design.md`.

## Global Constraints

Every task's requirements implicitly include this section.

- **Branching:** never commit to or merge into `main`. Work on branch `vendor-accounts` (worktree `.worktrees/vendor-accounts`). Squash-merge pre-fills its description from the commit body → write substantive commit bodies.
- **Repo text in English** (code, commits, PR, docs). Keep the real gateway hostname out of all repo text — use the placeholder `<gateway-host>`.
- **Branch-local files:** `docs/superpowers/**` and `docs/implementation-status.md` must never land on `main`. Fold durable content into `docs/architecture/` and delete them before the PR (Milestone 7).
- **Secrets at rest:** seal with `capture.SealSecret(s.cipher, s.settingsVolatile, plain)` BEFORE persisting; open with `capture.OpenSecret(s.Cipher, stored)` only at the dispatch edge; DTOs expose only `*_set` / `*_connected` booleans, never the value. A disk store with no cipher returns `capture.ErrKeyRequired` — never persist plaintext.
- **Store changes:** keep all three drivers working (`routing.MemoryStore`, sqlite, postgres — the latter two share `store.SQLStore` via the `dialect` seam). Schema changes are append-only migrations (next free version = **82**); never edit/reorder a shipped migration. Wide Go values need wide Postgres columns — floats are `double precision`, bools are `integer not null default 0` (the `sanitizeArgs` seam converts Go bool→int64), timestamps use `dl.timestampType()`. **Verify every store change with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set** (the postgres conformance leg skips silently otherwise).
- **`portal/api.go` is interfacer-generated but its regeneration is BROKEN (self-import bug):** hand-edit `internal/portal/api.go` to add new method signatures. After interface changes, regenerate the tracing wrapper with `go generate ./internal/tracing/...` then run `scripts/add-license-headers.sh` (re-applies SPDX headers to `*_gen.go`).
- **i18n:** add every key to BOTH the `de` and the `en` object in `gateway/frontend/src/i18n.ts` (the `en: PortalMessages` typing enforces parity at build time).
- **Lint/format gates (run before pushing):** backend `make lint-go` (golangci-lint `fmt --diff` + `run`: gofumpt + gocritic); frontend `cd gateway/frontend && npm run format:check && npm run lint && npm test && npm run build`.
- **Tests:** backend from `gateway/backend` (`make test-go`); provider adapter tests use `httptest` loopback listeners. Full: `make test`.
- **No `server-agent` change** → no agent `Version` bump.
- **Feature flag:** the whole area and the subscription path are behind an experimental, disable-able flag.
- **Naming:** entity `VendorAccount`, id prefix `va_`, table `vendor_accounts`; enums `vendor ∈ {openai, anthropic}`, `auth_type ∈ {api_key, subscription}`. Do NOT rename the existing `internal/provider` package.

## File Structure

**Backend (`gateway/backend/`):**
- `internal/routing/store.go` — MODIFY: add `VendorAccount` struct, vendor/auth_type/account-status constants, `VendorAccountStore` interface; embed it in the composite `Store`.
- `internal/routing/memory_store.go` — MODIFY: `vendorAccounts` map + CRUD + cascade in `DeleteUser` path; `copyVendorAccount`.
- `internal/store/migrate.go` — MODIFY: append `migration82Up` (create 3 tables + `usage_events.account_id`).
- `internal/store/sqlite_vendor_accounts.go` — CREATE: SQL CRUD + `scanVendorAccount` (+ models/usage/usage snapshot rows).
- `internal/store/vendor_account_conformance_test.go`, `vendor_account_column_parity_test.go`, `vendor_account_cascade_test.go` — CREATE.
- `internal/portal/service_vendor_accounts.go` — CREATE: DTOs, Create/Get/List/Update/Delete, `authorizeVendorAccount`, seal/mask, model-catalog seeding.
- `internal/portal/vendor_catalog.go` — CREATE: the static per-vendor model catalog + vendor OAuth constants are referenced from here but defined in `internal/vendorauth` (below).
- `internal/portal/api.go` — MODIFY (hand-edit): add the new `Service` methods to the `API` interface.
- `internal/portal/service.go` — MODIFY: error sentinels; (reuse existing `cipher`/`settingsVolatile`).
- `internal/gateway/portal_vendor_account_endpoints.go` — CREATE: HTTP handlers + `writePortalVendorAccountError`.
- `internal/gateway/server.go` — MODIFY: register routes.
- `internal/vendorauth/` — CREATE: the OAuth subsystem (`constants.go`, `anthropic.go`, `openai.go`, `tokens.go`, `refresh.go`) — vendor OAuth connect + refresh, kept out of `provider` and `portal`.
- `internal/provider/anthropic_messages.go` — CREATE: native Anthropic `/v1/messages` client (translate path).
- `internal/provider/upstream_auth.go` — MODIFY: carry extra static headers + masquerade.
- `internal/routing/resolver.go` — MODIFY: `Target` gains `ExtraHeaders`/`Masquerade`; new vendor-account candidate source + ownership filter.
- `internal/gateway/inference_complete.go` — MODIFY: scrape rate-limit headers → account usage snapshot; set `usage.Event.AccountID`.
- `internal/usage/recorder.go` — MODIFY: add `AccountID` field + column write.

**Frontend (`gateway/frontend/src/`):**
- `components/shared/types.ts` — MODIFY: add `'providers'` to `View`.
- `components/views.tsx` — MODIFY: import icon + `VendorAccountsView`; add registry entry.
- `components/VendorAccountsView.tsx` — CREATE: the CRUD + connect UI.
- `api/vendorAccounts.ts` — CREATE: DTOs + factory.
- `api.ts` — MODIFY: barrel wiring.
- `i18n.ts` — MODIFY: de + en keys.

**Docs:** `docs/architecture/reference/data-model.md`, `05-building-block-view.md`, `09-architecture-decisions.md` (new ADR), `11-risks-and-technical-debt.md`, `reference/api-surface.md`, `reference/openapi.yaml`, `reference/config-env.md`, a new `cross-cutting/external-vendor-accounts.md`.

---

## Milestone 1 — Entity, store (3 drivers), migration, tests

Deliverable: `routing.VendorAccount` with full CRUD across memory/sqlite/postgres, a migration creating the three tables plus the `usage_events.account_id` column, and conformance/parity/cascade tests green on all drivers.

### Task 1.1: Failing CRUD conformance test

**Files:**
- Create: `gateway/backend/internal/store/vendor_account_conformance_test.go`

**Interfaces:**
- Produces (consumed by every later task): `routing.VendorAccount{ID, OwnerUserID, Vendor, AuthType, Name, Status, APIKey, OAuthTokens string; CreatedAt, UpdatedAt time.Time}`; store methods `CreateVendorAccount(ctx, VendorAccount) error`, `VendorAccountByID(ctx, id) (VendorAccount, error)`, `VendorAccounts(ctx) ([]VendorAccount, error)`, `VendorAccountsByOwner(ctx, userID) ([]VendorAccount, error)`, `UpdateVendorAccount(ctx, VendorAccount) error`, `DeleteVendorAccount(ctx, id) error`.

- [ ] **Step 1: Write the failing test.** Mirror `forEachRoutingStoreSeeded` (seed a user for the FK). Assert create → `VendorAccountByID` round-trips every field; `VendorAccounts` and `VendorAccountsByOwner` return it; update replaces fields; delete makes `VendorAccountByID` return `store.ErrNotFound`; duplicate id → `storeerr.ErrConflict`.

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"op-ai-gateway/internal/routing"
	"op-ai-gateway/internal/storeerr"
	"testing"
	"time"
)

func TestRoutingStoreVendorAccountCRUD(t *testing.T) {
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	seedSQL := func(t *testing.T, s *SQLStore) {
		if err := s.CreateUser(context.Background(), newTestUser("u_va", "va@example.test", now)); err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	forEachRoutingStoreSeeded(t, seedSQL, func(t *testing.T, s routing.Store) {
		ctx := context.Background()
		// memory store has no FK, but seed a user there too so ownership reads are realistic.
		_ = s // memory: user seeding is a no-op path; SQL seeded above.
		acc := routing.VendorAccount{
			ID: "va_one", OwnerUserID: "u_va", Vendor: routing.VendorOpenAI,
			AuthType: routing.VendorAuthAPIKey, Name: "My OpenAI", Status: routing.VendorAccountStatusActive,
			APIKey: "enc:seeded", OAuthTokens: "", CreatedAt: now, UpdatedAt: now,
		}
		if err := s.CreateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("create: %v", err)
		}
		if err := s.CreateVendorAccount(ctx, acc); !errors.Is(err, storeerr.ErrConflict) {
			t.Fatalf("duplicate create err = %v, want ErrConflict", err)
		}
		got, err := s.VendorAccountByID(ctx, "va_one")
		if err != nil {
			t.Fatalf("by id: %v", err)
		}
		if got != acc {
			t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, acc)
		}
		byOwner, err := s.VendorAccountsByOwner(ctx, "u_va")
		if err != nil || len(byOwner) != 1 {
			t.Fatalf("by owner = %v, %v", byOwner, err)
		}
		acc.Name = "Renamed"
		acc.UpdatedAt = now.Add(time.Minute)
		if err := s.UpdateVendorAccount(ctx, acc); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got, _ := s.VendorAccountByID(ctx, "va_one"); got.Name != "Renamed" {
			t.Fatalf("update not applied: %+v", got)
		}
		if err := s.DeleteVendorAccount(ctx, "va_one"); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := s.VendorAccountByID(ctx, "va_one"); !errors.Is(err, storeerr.ErrNotFound) {
			t.Fatalf("after delete err = %v, want ErrNotFound", err)
		}
	})
}
```

- [ ] **Step 2: Run, verify it fails to compile.** `cd gateway/backend && go test ./internal/store/ -run TestRoutingStoreVendorAccountCRUD` → FAIL (undefined `routing.VendorAccount`, methods).
- [ ] **Step 3: Commit the test.** `git add internal/store/vendor_account_conformance_test.go && git commit -m "test: add failing vendor_account store conformance test"` (expected red; commit keeps the step visible).

### Task 1.2: Domain type, constants, interface, memory driver

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`
- Modify: `gateway/backend/internal/routing/memory_store.go`

**Interfaces:**
- Produces: the type + `VendorAccountStore` interface named in 1.1; constants `VendorOpenAI="openai"`, `VendorAnthropic="anthropic"`, `VendorAuthAPIKey="api_key"`, `VendorAuthSubscription="subscription"`, `VendorAccountStatusActive="active"`, `VendorAccountStatusDisabled="disabled"`, `VendorAccountStatusNeedsReconnect="needs_reconnect"`.

- [ ] **Step 1: Add the type + constants** to `store.go` (next to the `AIServer` block):

```go
const (
	VendorOpenAI    = "openai"
	VendorAnthropic = "anthropic"

	VendorAuthAPIKey       = "api_key"
	VendorAuthSubscription = "subscription"

	VendorAccountStatusActive         = "active"
	VendorAccountStatusDisabled       = "disabled"
	VendorAccountStatusNeedsReconnect = "needs_reconnect"
)

// VendorAccount is a per-user external AI vendor account ("Anbieter"): either a
// plain API key or a consumer-subscription OAuth connection. Credentials are
// SEALED (enc:/plain:); routing never decrypts them. Distinct from the
// provider-adapter "Provider*" constants and from AIServer.
type VendorAccount struct {
	ID          string
	OwnerUserID string
	Vendor      string // VendorOpenAI | VendorAnthropic
	AuthType    string // VendorAuthAPIKey | VendorAuthSubscription
	Name        string
	Status      string // VendorAccountStatus*
	// APIKey holds the sealed API key when AuthType == api_key, else "".
	APIKey string
	// OAuthTokens holds the sealed JSON token blob when AuthType == subscription,
	// else "". Shape (plaintext, before sealing): {access, refresh, expires_at,
	// account_id, plan_type, scope}. See internal/vendorauth.TokenSet.
	OAuthTokens string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
```

- [ ] **Step 2: Add the `VendorAccountStore` interface** and embed it in the composite `Store` (add `VendorAccountStore` to the interface list at `store.go:~2115`):

```go
// VendorAccountStore is per-user external-vendor-account CRUD. Credentials ride
// sealed; the cipher-holding layers open them at dispatch.
type VendorAccountStore interface {
	CreateVendorAccount(ctx context.Context, acc VendorAccount) error
	UpdateVendorAccount(ctx context.Context, acc VendorAccount) error
	VendorAccountByID(ctx context.Context, id string) (VendorAccount, error)
	VendorAccounts(ctx context.Context) ([]VendorAccount, error)
	VendorAccountsByOwner(ctx context.Context, userID string) ([]VendorAccount, error)
	DeleteVendorAccount(ctx context.Context, id string) error
}
```

- [ ] **Step 3: Implement the memory driver.** Add `vendorAccounts map[string]VendorAccount` to the `MemoryStore` struct, init it in `NewMemoryStore`, add `copyVendorAccount` (plain value copy — no pointers), and the six methods mirroring the server CRUD (lock discipline, `storeerr.Err*`, sorted list). Add a cascade line in `DeleteUser` if the memory store enforces user deletion (grep `func (m *MemoryStore) DeleteUser`); otherwise note it.

```go
func copyVendorAccount(a VendorAccount) VendorAccount { return a }

func (m *MemoryStore) CreateVendorAccount(_ context.Context, a VendorAccount) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.vendorAccounts[a.ID]; ok {
		return storeerr.ErrConflict
	}
	m.vendorAccounts[a.ID] = copyVendorAccount(a)
	return nil
}
// UpdateVendorAccount (ErrNotFound if absent), VendorAccountByID, VendorAccounts
// (sorted by id), VendorAccountsByOwner (filter OwnerUserID), DeleteVendorAccount
// — all mirror the server CRUD shape verbatim.
```

- [ ] **Step 4: Run the memory leg.** `go test ./internal/routing/` builds; `go test ./internal/store/ -run TestRoutingStoreVendorAccountCRUD/memory` → PASS. sqlite/postgres still fail (no table/SQL).
- [ ] **Step 5: Commit.** `git commit -am "feat: add VendorAccount domain type + memory store"`.

### Task 1.3: Migration 82 (three tables + usage_events.account_id)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go`

- [ ] **Step 1: Append the registry entry** `{version: 82, name: "vendor_accounts", up: migration82Up}` to the `migrations` slice (end).
- [ ] **Step 2: Write `migration82Up`** (create tables as a `[]string` loop + add the usage column via `addColumnIfMissing`):

```go
func migration82Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	ts := dl.timestampType()
	stmts := []string{
		`create table if not exists vendor_accounts (
			id text primary key,
			owner_user_id text not null references users(id) on delete cascade,
			vendor text not null,
			auth_type text not null,
			name text not null,
			status text not null,
			api_key text not null default '',
			oauth_tokens text not null default '',
			created_at ` + ts + ` not null,
			updated_at ` + ts + ` not null
		)`,
		`create index if not exists idx_vendor_accounts_owner on vendor_accounts(owner_user_id)`,
		`create table if not exists vendor_account_models (
			account_id text not null references vendor_accounts(id) on delete cascade,
			gateway_model text not null,
			upstream_model text not null,
			api_flavor text not null,
			primary key (account_id, gateway_model)
		)`,
		`create table if not exists vendor_account_usage (
			account_id text primary key references vendor_accounts(id) on delete cascade,
			five_hour_pct double precision not null default -1,
			five_hour_reset_at ` + ts + `,
			weekly_pct double precision not null default -1,
			weekly_reset_at ` + ts + `,
			credit_balance text not null default '',
			updated_at ` + ts + ` not null
		)`,
	}
	for _, stmt := range stmts {
		if err := execTx(ctx, tx, dl, stmt); err != nil {
			return err
		}
	}
	return addColumnIfMissing(ctx, tx, dl, "usage_events", "account_id text not null default ''")
}
```

- [ ] **Step 3: Run** `go test ./internal/store/ -run TestRoutingStoreVendorAccountCRUD/sqlite` → still fails (SQL methods missing) but the migration applies (no migration error). Commit: `git commit -am "feat: migration 82 — vendor account tables + usage_events.account_id"`.

### Task 1.4: SQL driver

**Files:**
- Create: `gateway/backend/internal/store/sqlite_vendor_accounts.go`

- [ ] **Step 1: Implement CRUD** on `*SQLiteStore` (receiver name; it is `= SQLStore`), through `s.exec`/`s.query`/`s.queryRow` (which apply `rebind` + `sanitizeArgs`). `CreateVendorAccount` maps unique-violation → `ErrConflict`; `scanVendorAccount` uses `rowScanner`; update/delete use `requireAffected`.

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"errors"
	"database/sql"
	"fmt"
	"op-ai-gateway/internal/routing"
)

const vendorAccountColumns = `id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, created_at, updated_at`

func (s *SQLiteStore) CreateVendorAccount(ctx context.Context, a routing.VendorAccount) error {
	_, err := s.exec(ctx, `insert into vendor_accounts (`+vendorAccountColumns+`)
		values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerUserID, a.Vendor, a.AuthType, a.Name, a.Status, a.APIKey, a.OAuthTokens, a.CreatedAt, a.UpdatedAt)
	if err != nil {
		if s.dl.isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("create vendor account: %w", err)
	}
	return nil
}

func scanVendorAccount(row rowScanner) (routing.VendorAccount, error) {
	var a routing.VendorAccount
	err := row.Scan(&a.ID, &a.OwnerUserID, &a.Vendor, &a.AuthType, &a.Name, &a.Status, &a.APIKey, &a.OAuthTokens, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return routing.VendorAccount{}, ErrNotFound
	}
	if err != nil {
		return routing.VendorAccount{}, fmt.Errorf("scan vendor account: %w", err)
	}
	return a, nil
}
// VendorAccountByID (queryRow + scanVendorAccount), VendorAccounts (query, order by id),
// VendorAccountsByOwner (where owner_user_id = ? order by id), UpdateVendorAccount
// (update ... where id = ?, requireAffected), DeleteVendorAccount (delete, requireAffected)
// — all mirror the AIServer SQL methods verbatim.
```

- [ ] **Step 2: Run** `OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./internal/store/ -run TestRoutingStoreVendorAccountCRUD` → all three legs PASS.
- [ ] **Step 3: Commit.** `git commit -am "feat: SQL vendor account CRUD"`.

### Task 1.5: Column-parity + schema-coverage tests

**Files:**
- Create: `gateway/backend/internal/store/vendor_account_column_parity_test.go`
- Modify: the `usage_events` schema-coverage fixture (the new `account_id` column will break it)

- [ ] **Step 1:** Write `TestConformanceVendorAccountReadersAgreeOnEveryColumn` (seed a user + 3 accounts with distinct values across every column; assert `VendorAccountByID`, `VendorAccounts`, `VendorAccountsByOwner` agree field-by-field using a `normalize...ForCompare` that UTCs the timestamps) and `TestVendorAccountsSchemaColumnsAllCovered` using `assertColumnCoverage` (no bools in this table → `bools: nil`, all 10 columns `seeded`). Model on `ai_server_column_parity_test.go`.
- [ ] **Step 2:** Find the `usage_events` coverage test (`grep -rn "assertColumnCoverage(.*usage_events" internal/store`) and add `"account_id"` to its `seeded` list so `TestUsageEvents...SchemaColumnsAllCovered` stays green.
- [ ] **Step 3: Run** all store tests with the DSN set → green. Commit.

### Task 1.6: Cascade test

**Files:**
- Create: `gateway/backend/internal/store/vendor_account_cascade_test.go`

- [ ] **Step 1:** Write `TestRoutingStoreDeleteVendorAccountCascades` (create account + a model row + a usage row via the store writers from later tasks; assert all present; `DeleteVendorAccount`; assert model+usage rows gone) and `TestRoutingStoreDeleteUserCascadesVendorAccounts` (create user+account; `DeleteUser`; account gone on SQL; memory mirrors via the cascade line in 1.2 step 3). Model on `delete_ai_server_cascade_test.go`.

   > NOTE: the `vendor_account_models` / `vendor_account_usage` store writers land in Milestones 3/6. Until then, insert those child rows in the test via raw `s.exec` against the table, or sequence this task after the writers exist. Prefer sequencing: implement the child-row writers (3.x / 6.x) then this cascade assertion. Mark this task blocked-on-3.x in `implementation-status.md`.

- [ ] **Step 2:** Add the memory-store cascade: in `DeleteVendorAccount` (memory) also delete child maps once they exist (M3/M6); in `DeleteUser` (memory) drop the owner's accounts. Run → green. Commit.

**Milestone 1 gate:** `make test-go` green; `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set for the store run; `make lint-go` clean.

---

## Milestone 2 — Portal CRUD, API interface, menu, i18n, API-key connect form

Deliverable: a user can create/list/rename/delete an **API-key** vendor account in the new "Anbieter" portal page; secrets are write-only; no routing yet.

### Task 2.1: Service layer — DTOs, CRUD, authorization, sealing

**Files:**
- Create: `gateway/backend/internal/portal/service_vendor_accounts.go`
- Modify: `gateway/backend/internal/portal/service.go` (error sentinels)

**Interfaces:**
- Produces: `VendorAccountDTO{ID, Vendor, AuthType, Name, Status string; APIKeySet, SubscriptionConnected bool; Models []VendorAccountModelDTO; CreatedAt, UpdatedAt time.Time}`; `CreateVendorAccountRequest{Vendor, AuthType, Name, Status string; APIKey string}`; `UpdateVendorAccountRequest{Name, Status *string; APIKey *string}`; service methods `CreateVendorAccount/ListVendorAccounts/GetVendorAccount/UpdateVendorAccount/DeleteVendorAccount(ctx, auth.Token, ...)`.

- [ ] **Step 1: Failing service test** (`service_vendor_accounts_test.go`): a non-system principal creates an `api_key` openai account → DTO has `APIKeySet=true`, no key echoed; another user's `GetVendorAccount` returns `ErrVendorAccountNotFound` (404-no-leak); update with `APIKey=nil` keeps the key, `""` clears it; delete removes it. Use a memory-store-backed `Service` with a nil cipher + `settingsVolatile=true` (so seal uses the `plain:` path).
- [ ] **Step 2: Add error sentinels** to the `var (...)` block in `service.go`:

```go
	ErrVendorAccountNotFound   = errors.New(CodeVendorAccountNotFound) // CodeVendorAccountNotFound = "vendor_account.not_found"
	ErrVendorAccountNameRequired = errors.New("vendor_account.name_required")
	ErrVendorAccountVendorInvalid   = errors.New("vendor_account.vendor_invalid")
	ErrVendorAccountAuthTypeInvalid = errors.New("vendor_account.auth_type_invalid")
	ErrVendorAccountStatusInvalid   = errors.New("vendor_account.status_invalid")
	ErrVendorAccountForbidden       = errors.New("vendor_account.forbidden")
```

- [ ] **Step 3: Implement** `service_vendor_accounts.go`: validate+normalize (trim name → `ErrVendorAccountNameRequired`; vendor ∈ {openai,anthropic}; auth_type ∈ {api_key,subscription}; status via a `normalizeVendorAccountStatus`), seal the key with `capture.SealSecret(s.cipher, s.settingsVolatile, req.APIKey)` BEFORE the store write, assign `id := "va_" + compactRandomHex(16)`, set `OwnerUserID = principal.UserID`. `authorizeVendorAccount(ctx, principal, id)` mirrors `authorizeServer` but owner-only (system scope may read all; a non-owner gets `ErrVendorAccountNotFound`). DTO sets `APIKeySet: acc.APIKey != ""`, `SubscriptionConnected: acc.OAuthTokens != ""`, never the values. Update uses the `*string` nil/""/value sentinel.
- [ ] **Step 4:** Run the service test → PASS. Commit.

### Task 2.2: API interface + HTTP handlers + routes

**Files:**
- Modify (hand-edit): `gateway/backend/internal/portal/api.go`
- Create: `gateway/backend/internal/gateway/portal_vendor_account_endpoints.go`
- Modify: `gateway/backend/internal/gateway/server.go`

- [ ] **Step 1:** Hand-add the five methods to the `API` interface in `api.go` (interfacer regen is broken — edit by hand, keep alphabetical grouping):

```go
	CreateVendorAccount(context.Context, auth.Token, CreateVendorAccountRequest) (VendorAccountDTO, error)
	DeleteVendorAccount(context.Context, auth.Token, string) (bool, error)
	GetVendorAccount(context.Context, auth.Token, string) (VendorAccountDTO, error)
	ListVendorAccounts(context.Context, auth.Token) (VendorAccountListResponse, error)
	UpdateVendorAccount(context.Context, auth.Token, string, UpdateVendorAccountRequest) (VendorAccountDTO, error)
```

- [ ] **Step 2:** `go generate ./internal/tracing/...` then `scripts/add-license-headers.sh` → regenerates `api_tracing_gen.go`. Verify `go build ./...`.
- [ ] **Step 3:** Create the handler file mirroring `portal_server_endpoints.go`: `handlePortalVendorAccounts` (GET list / POST create) and `handlePortalVendorAccountItem` (GET/PATCH/DELETE on `/{id}`), each starting with `s.requireWebScope(w, r, scopeGatewayUse)`; a `portalVendorAccountErrRows []errRow` + `writePortalVendorAccountError`.
- [ ] **Step 4:** Register routes in `server.go` next to the servers routes:

```go
	s.mux.HandleFunc("/api/portal/vendor-accounts", s.handlePortalVendorAccounts)
	s.mux.HandleFunc("/api/portal/vendor-accounts/", s.handlePortalVendorAccountItem)
```

- [ ] **Step 5:** Handler test (httptest against the gateway mux, memory store, logged-in session): create → 201 with `api_key_set:true`; list → contains it; cross-user get → 404. Run → PASS. Commit.

### Task 2.3: Frontend — API module, menu entry, i18n

**Files:**
- Create: `gateway/frontend/src/api/vendorAccounts.ts`
- Modify: `gateway/frontend/src/api.ts`
- Modify: `gateway/frontend/src/components/shared/types.ts`
- Modify: `gateway/frontend/src/i18n.ts`

- [ ] **Step 1:** `api/vendorAccounts.ts` — DTO types mirroring the Go DTOs (snake_case) + `vendorAccountsApi(fetcher)` with `vendorAccounts()`, `createVendorAccount`, `updateVendorAccount`, `deleteVendorAccount` (model on `api/resourceGroups.ts`; IDs `encodeURIComponent`).
- [ ] **Step 2:** `api.ts` — add `import { vendorAccountsApi }`, the `...vendorAccountsApi(fetcher)` spread, and `export * from './api/vendorAccounts'`.
- [ ] **Step 3:** `shared/types.ts` — add `| 'providers'` to the `View` union.
- [ ] **Step 4:** `i18n.ts` — add to BOTH `de` and `en`: `providers` (de `'Anbieter'` / en `'Providers'`), `providersIntro`, `vendorAccountCreate`, `vendorAccountNameLabel`, `vendorAccountVendorLabel`, `vendorAccountAuthTypeLabel`, `vendorAccountApiKeyLabel`/`...SetPlaceholder`/`...Clear`/`...Note`, vendor labels (`vendorOpenAI`/`vendorAnthropic`), auth labels (`vendorAuthApiKey`/`vendorAuthSubscription`), table column labels, `vendorAccountDeleteConfirm`, status labels (reuse `statusActive`/`statusDisabled`).
- [ ] **Step 5:** `npm test` (the `App.test.tsx` parity guard + `arch.test.ts` layering) + `npm run format:check` → green. Commit.

### Task 2.4: Frontend — the VendorAccountsView page + registry entry

**Files:**
- Create: `gateway/frontend/src/components/VendorAccountsView.tsx`
- Modify: `gateway/frontend/src/components/views.tsx`

- [ ] **Step 1:** Copy the `ResourceGroupsView` skeleton: props `{ t, api, role }` with `api: Pick<PortalApi, 'vendorAccounts'|'createVendorAccount'|'updateVendorAccount'|'deleteVendorAccount'>`; `useResource(() => api.vendorAccounts().then(r => r.data), ...)`; `Mode = 'list' | 'create' | { kind:'detail'; account }`; list (PageTitle + Panel + ListTable with columns vendor/auth/name/status) + create form (vendor `SelectField`, auth-type `SelectField`, API-key field using the ApplicationSection write-only secret pattern — `tokenInput`/`tokenCleared`, set-placeholder, clear button) + detail (rename/status + delete via `ConfirmDialog`). The subscription auth-type renders a "Verbinden" placeholder button wired in M4 (for now it is disabled with a "coming soon" tooltip or simply hidden behind the feature flag).
- [ ] **Step 2:** Add the registry entry in `views.tsx` after `servers`, importing an icon (e.g. `Cloud`) into the `lucide-react` import block and the component:

```tsx
  providers: {
    id: 'providers',
    labelKey: 'providers',
    href: '/providers',
    icon: Cloud,
    gate: alwaysVisible,
    render: (ctx) => <VendorAccountsView t={ctx.t} api={ctx.api} role={ctx.role} />,
  },
```

   (Visible to every authenticated user — each manages only their own accounts. Gate behind the feature flag once M5 wires it; `alwaysVisible` is fine until then.)
- [ ] **Step 3:** `npm test && npm run lint && npm run build` green. Optional `VendorAccountsView.test.tsx` (render list, open create, submit mocked). Commit.

**Milestone 2 gate:** `make dev`, log in, open "Anbieter", create an API-key account, see it listed, rename, delete — all in the browser. Backend + frontend test suites + lint/format green.

---

## Milestone 3 — API-key serving path (routing candidate + dispatch) end-to-end

Deliverable: a request (portal chat or the user's own API token) for a catalog model routes to the user's connected **API-key** account; OpenAI via the existing client, Anthropic via a new native Messages client. Owner-scoped.

### Task 3.1: Curated model catalog + model rows

**Files:**
- Create: `gateway/backend/internal/portal/vendor_catalog.go`
- Modify: `internal/store/sqlite_vendor_accounts.go` + memory store (model-row writers/readers)

- [ ] Static catalog: `func VendorCatalog(vendor string) []routing.VendorAccountModel` returning `{GatewayModel, UpstreamModel, APIFlavor}` rows (OpenAI → `openai` flavor models; Anthropic → `anthropic` flavor models — current vendor-native ids; keep in ONE file for easy update). On `CreateVendorAccount`, seed `vendor_account_models` with the full catalog. Add `VendorAccountModels(ctx, accountID)` + `SetVendorAccountModels(...)` store methods (both drivers) + the `routing.VendorAccountModel` type. TDD: a store test for the model rows round-trip + cascade (completes Task 1.6).

### Task 3.2: `Target` extensions + resolver candidate source

**Files:**
- Modify: `internal/routing/resolver.go`

**Interfaces:**
- Produces: `Target.ExtraHeaders map[string]string`, `Target.Masquerade string` (empty = none); a candidate-source that, given the resolve principal, yields vendor-account candidates.

- [ ] Add `ExtraHeaders` + `Masquerade` to `Target` (default empty; existing dispatch unaffected). Add a resolver step: enumerate `store.VendorAccountsByOwner(principal.UserID)` filtered to `status==active`, match the requested gateway model + flavor against `VendorAccountModels`, and build a `Target` directly (Endpoint = vendor host from catalog/constants, `ProviderModel` = upstream id, `APIToken` = the sealed credential, `APITokenHeader` = `x-api-key` for Anthropic api_key else ""). Owner-scope is intrinsic (only the principal's accounts are enumerated). TDD: resolver test — owner's request resolves to the account target; a different principal's identical request does not.
- [ ] The principal must be threaded into the resolve. Confirm `inferencePreflight`/`Resolve` already carry the `auth.Token`/user; if not, thread `principal` through (grep the resolve call sites in `inference_handlers.go`). Portal chat uses the internal loopback with the session user as principal — verify the user id is present there.

### Task 3.3: Native Anthropic Messages client

**Files:**
- Create: `gateway/backend/internal/provider/anthropic_messages.go`

- [ ] A `Client`/`StreamingClient` that emits `POST {endpoint}/v1/messages` from the neutral `inference.Request` (reuse `internal/compat/anthropic.go` rendering), sets `anthropic-version: 2023-06-01`, parses the response + SSE back to `inference.*`. Register it in `cmd/gateway/main.go:providerClients` under a new provider key used by Anthropic vendor-account targets. TDD: `httptest` server asserting the request shape + a parsed non-stream and stream response.
- [ ] OpenAI api_key target reuses the existing `OpenAICompatibleClient` pointed at `api.openai.com` — no new client.

### Task 3.4: Dispatch applies extra headers

**Files:**
- Modify: `internal/provider/upstream_auth.go` (+ the native proxy + openai client request builders)

- [ ] Extend the upstream-auth context carrier to also hold `ExtraHeaders`; apply them in `applyUpstreamAuth` / the request builders (set each header verbatim). `Target.ExtraHeaders` is opened/attached in `server.go:upstreamAuthCtx` alongside the token. For api_key Anthropic, `ExtraHeaders` carries `anthropic-version`. TDD: unit test that a target with `ExtraHeaders` produces those headers on the outgoing request.

**Milestone 3 gate:** with a real API key pasted (or an `httptest` stub vendor), a portal-chat and a bearer-token request for a catalog model reach the stubbed vendor with the right auth + headers; owner-scoping enforced. (The operator has no key, so e2e uses a stub; real-key validation happens opportunistically.)

---

## Milestone 4 — Subscription connect (token-import, Anthropic code-paste, OpenAI device-code) + refresh

Deliverable: a user can connect a subscription account and the gateway stores+refreshes the OAuth tokens. Build in the order token-import → Anthropic code-paste → OpenAI device-code (each independently testable).

### Task 4.0: `internal/vendorauth` package + constants (spike first)

**Files:**
- Create: `internal/vendorauth/constants.go`, `tokens.go`

- [ ] **Spike step (concrete, time-boxed):** with the operator's own account, capture the exact live values into `constants.go` (all REVERSE-ENGINEERED; one file, commented): Anthropic authorize/token URLs, client-id, scopes, `anthropic-beta`; OpenAI device-code + token endpoints, client-id, `originator`, the `chatgpt.com/backend-api/codex/responses` host. Record a redacted sample token response as a test fixture. Acceptance: a `TokenSet` round-trips through seal/open; constants compile.
- [ ] `TokenSet{Access, Refresh string; ExpiresAt time.Time; AccountID, PlanType, Scope string}` with JSON (de)serialization; `MarshalSealed`/`UnmarshalSealed` wrapping `capture.SealSecret`/`OpenSecret` over the JSON.

### Task 4.1: Token-import connect (both vendors)

**Files:**
- Modify: `service_vendor_accounts.go`, the handler, `api/vendorAccounts.ts`, `VendorAccountsView.tsx`, i18n

- [ ] Backend `ConnectVendorAccountImport(ctx, principal, accountID, {access, refresh?, expires_at?})`: build a `TokenSet`, seal into `OAuthTokens`, set `AuthType=subscription`, run a probe call (a cheap models/usage request) to validate → on success `status=active`, else `ErrVendorAccountTokenInvalid`. Route `POST /api/portal/vendor-accounts/{id}/connect/import`. Frontend: the subscription form shows a token textarea + "Importieren" button. TDD: service test with a stub probe.

### Task 4.2: Anthropic code-paste connect

- [ ] `BeginVendorAccountOAuth(ctx, principal, accountID)` → generate PKCE verifier+state, store in an in-memory pending map keyed by (accountID), return the authorize URL. `CompleteVendorAccountOAuth(ctx, principal, accountID, codeAndState)` → split `code#state`, verify state, exchange at the Anthropic token URL (JSON body) with the verifier, seal the `TokenSet`. Routes `.../connect/begin` and `.../connect/complete`. Frontend: "Verbinden" → shows the URL (open in new tab) + a `code#state` paste field → submit. TDD: `httptest` token endpoint; state-mismatch rejected.

### Task 4.3: OpenAI device-code connect

- [ ] `BeginVendorAccountDeviceCode` → request a device/user code, return `{verification_url, user_code, interval}`; `PollVendorAccountDeviceCode` → poll the token endpoint until authorized/expired; on success read `chatgpt_account_id`/`chatgpt_plan_type` from the id_token JWT, seal. Frontend shows the user code + URL and polls. Fallback: if device-code is unavailable for the client-id, reuse the 4.2 code-paste flow with the loopback-redirect authorize URL. TDD: `httptest` device + token endpoints (authorization_pending → success).

### Task 4.4: Token refresh

**Files:**
- Create: `internal/vendorauth/refresh.go`

- [ ] `EnsureFresh(ctx, cipher, acc) (TokenSet, bool, error)`: open the sealed tokens; if `ExpiresAt` within a buffer (e.g. 2 min), `grant_type=refresh_token` → new `TokenSet`, return `changed=true` so the caller re-seals + persists via `UpdateVendorAccount`. Per-account mutex to avoid concurrent refreshes. A failed refresh → caller sets `status=needs_reconnect`. Called lazily at dispatch (M5). TDD: expired token triggers refresh + re-seal; refresh failure surfaces.

**Milestone 4 gate:** the operator connects their Anthropic (code-paste) and OpenAI (device-code) subscriptions on `<gateway-host>`; tokens are stored sealed; a forced-expiry test shows refresh works. Account shows `subscription_connected:true`.

---

## Milestone 5 — Subscription serving (dispatch extras + vendor endpoints)

Deliverable: a subscription account serves inference, with the vendor-specific headers and the Claude-Code masquerade.

### Task 5.1: Resolver fills subscription targets

- [ ] Extend the M3 candidate source: for `auth_type==subscription`, set the subscription endpoint (Anthropic `api.anthropic.com`; OpenAI `chatgpt.com/backend-api/codex`), `ExtraHeaders` (Anthropic: `anthropic-version` + `anthropic-beta: oauth-2025-04-20,...`; OpenAI: `chatgpt-account-id`, `OpenAI-Beta: responses=experimental`, `originator: codex_cli_rs`), `Masquerade="claude_code"` for Anthropic, and resolve the bearer via `vendorauth.EnsureFresh` (open sealed tokens, refresh if near expiry, persist if changed). OpenAI subscription forces the Responses protocol only.

### Task 5.2: Masquerade injection

**Files:**
- Modify: the Anthropic Messages client / the request-building path

- [ ] When `Target.Masquerade=="claude_code"`, prepend the required system block `You are Claude Code, Anthropic's official CLI for Claude.` as the first system content block before the user's system prompt. TDD: a request with masquerade starts with that exact block; without it, unchanged.

### Task 5.3: Credential resolution for subscription at the edge

**Files:**
- Modify: `internal/gateway/server.go:upstreamAuthCtx` (or a sibling)

- [ ] For a subscription target, the bearer is the (possibly refreshed) OAuth access token, not `APIToken`. Thread the resolved access token + `ExtraHeaders` + masquerade from the candidate build through to dispatch. Keep the api_key path unchanged. TDD: integration test (stub vendors) — subscription request carries bearer + beta headers + masquerade; api_key request carries `x-api-key`.

**Milestone 5 gate:** against stub vendors, both subscription vendors serve a chat round-trip with correct headers/masquerade; against the operator's real connected accounts, a portal-chat message returns a completion. Feature flag gates the whole path.

---

## Milestone 6 — Usage & limits (header scraping + snapshot + UI)

Deliverable: per-account 5h + weekly usage bars (subscription), fed by scraping vendor rate-limit headers.

### Task 6.1: `usage.Event.AccountID` + snapshot store

**Files:**
- Modify: `internal/usage/recorder.go` (+ the usage SQL writer/reader), `internal/gateway/inference_complete.go`
- Modify: `internal/store/sqlite_vendor_accounts.go` + memory (usage snapshot upsert/read)

- [ ] Add `AccountID` to `usage.Event` + the `usage_events` insert/scan; set it in `recordUsage` from the resolved target (empty for non-vendor targets). Add `UpsertVendorAccountUsage(ctx, VendorAccountUsage)` + `VendorAccountUsageByID` store methods + the `routing.VendorAccountUsage` type (both drivers). TDD: round-trip + the `usage_events` column parity already covered in 1.5.

### Task 6.2: Header scraper

**Files:**
- Create: `internal/vendorauth/usage_headers.go` (tolerant parsers)
- Modify: `inference_complete.go` (call at `recordUsage` when target is a vendor account)

- [ ] `ParseAnthropicUnified(h http.Header) (VendorAccountUsage, bool)` — prefix-match `anthropic-ratelimit-unified-`, read `5h-utilization`/`7d-utilization` (×100), `reset` (epoch). `ParseCodex(h http.Header) (...)` — `x-codex-primary-used-percent`/`-reset-at`, `-secondary-*`, `-credits-balance`. Also parse the `codex.rate_limits` SSE event. Missing → leave `-1`/zero (never 0% silently). On a vendor-account request, upsert the snapshot. TDD: table tests with captured header fixtures from the 4.0 spike; unknown headers → unknown, not 0.

### Task 6.3: Usage panel API + UI

**Files:**
- Modify: service (`GetVendorAccount` includes the usage snapshot), `api/vendorAccounts.ts`, `VendorAccountsView.tsx`, i18n

- [ ] DTO adds `usage?: { five_hour_pct, five_hour_reset_at, weekly_pct, weekly_reset_at, credit_balance }`. The detail view renders two progress bars (5h, weekly) with reset countdowns (subscription) — reuse the used/threshold rendering idiom from `LimitsEditor`. API-key accounts show nothing here yet (deferred). TDD: frontend render test with a mocked usage snapshot.

**Milestone 6 gate:** after a few real subscription requests, the account detail shows live 5h/weekly bars updating from scraped headers.

---

## Milestone 7 — Docs fold-in, cleanup, PR

- [ ] **Docs (English):** new ADR in `09-architecture-decisions.md` (the `vendor_account` entity + ToS design-acceptance); add the ToS/experimental acceptance to `11-risks-and-technical-debt.md` §11.4; add the three tables + `usage_events.account_id` to `reference/data-model.md`; add the menu area to `05-building-block-view.md`; new `cross-cutting/external-vendor-accounts.md` (entity, OAuth flows, dispatch extras, header scraping, feature flag, ToS); `reference/api-surface.md` + `openapi.yaml` for `/api/portal/vendor-accounts*`; `reference/config-env.md` for the feature flag + the cipher-key requirement. Run `make lint-docs`.
- [ ] **README/screenshots** only if the menu appears in a documented screenshot (regenerate per the capture format if so).
- [ ] **Working-file cleanup:** fold durable content from this plan + the spec into `docs/architecture/`, then `git rm -r docs/superpowers docs/implementation-status.md`. Verify `git diff --name-only main...HEAD` shows neither path.
- [ ] **Full verification:** `make test` (+ `OP_AI_GATEWAY_TEST_POSTGRES_DSN`), `make lint`, frontend `format:check`+`lint`+`test`+`build`, `make test-e2e` if feasible, and the local Sonar branch-findings gate (`make sonar-gate` advisory; `make sonar-findings && make sonar-branch-findings` authoritative) — act on branch-attributed findings.
- [ ] **Push over SSH** and open a PR (English body; substantive commit body for the squash). Do not merge.

---

## Self-review

**Spec coverage:** §4 naming → M1.2 + Global Constraints. §5 data model → M1.2–1.6 (+ account_id M1.3/6.1). §6 secrets → M2.1 (seal/mask), M4.0 (TokenSet). §7 OAuth → M4. §8 routing → M3.2/5.1. §9 dispatch → M3.3/3.4/5.1/5.2/5.3. §10 usage/limits → M6. §11 UI → M2.3/2.4 + M4 forms + M6.3. §12 RBAC → M2.1 (`authorizeVendorAccount`) + M3.2 owner-scope. §13 config/flag → M2.4/M5 flag, M7 docs. §14 tests → each task's TDD step. §15 docs → M7. §16 risks → M4.0 spike, feature flag.

**Placeholder scan:** the two deliberate forward-references (M1.6 child-row writers land in M3/M6; M2.4 subscription button wired in M4) are sequencing notes with explicit resolution, not vague TODOs. The reverse-engineered vendor constants (M4.0) are an explicit spike with acceptance criteria — the honest way to plan an undocumented integration, not a placeholder.

**Type consistency:** `VendorAccount`, `VendorAccountStore`, `VendorAccountDTO`, `vendor`/`auth_type` enums, `Target.ExtraHeaders`/`Target.Masquerade`, `vendorauth.TokenSet`, `usage.Event.AccountID` are used consistently across tasks. Route base `/api/portal/vendor-accounts`; View id `providers`; i18n label key `providers`.

**Calibration note:** M1–M2 are written at full bite-sized-step granularity (foundation, fully determinable now). M3–M7 are executable task blocks with real code shapes + per-task TDD steps; their step-level test/impl code is finalized at execution time against the landed earlier milestones and (for M4–M6) the wire behavior captured in the M4.0 spike — because the vendor subscription endpoints are undocumented and must be observed, not guessed.

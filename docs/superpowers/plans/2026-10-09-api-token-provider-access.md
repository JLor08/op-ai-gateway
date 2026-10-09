# Per-API-token provider access + prefix override — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let each personal API token opt in to which of its owner's vendor accounts ("Anbieter") it may use, each with an optional model-name prefix override (override enabled + empty value = original names, no prefix).

**Architecture:** A new JSON column `vendor_provider_access` on `api_tokens` carries `{all, accounts[]}`, decoded onto `auth.Token.VendorAccess`. A single shared helper `routing.TokenVendorPrefix(access, account) → (prefix, allowed)` drives BOTH the listing overlay (display) and the resolver (access + reverse map), so advertised names always equal routable names. Strict opt-in: default `{all:false, accounts:[]}` = no vendor access; existing tokens migrate to that default.

**Tech Stack:** Go 1.x backend (`gateway/backend`, module `op-ai-gateway`), SQLite + Postgres stores, React/TS + MUI portal (`gateway/frontend`), vitest, SonarQube.

## Global Constraints

- Repo-facing text (code, comments, commits, PRs, docs) in **English**; chat may be German.
- **No attribution lines** in commits or the PR.
- Push over **SSH** (the `gh` token 403s on push).
- Everything new is gated by the existing `vendor_accounts` experimental flag (`VendorAccountsEnabled(ctx)` / frontend `vendorAccountsEnabled`); flag off ⇒ the feature is invisible and inert.
- Scope: **personal API tokens only** (user tokens). Service tokens (`UserID==""`) own no vendor accounts — the field stays empty/ignored for them; `ServiceTokensSection` is NOT touched.
- Strict opt-in default `{all:false, accounts:[]}`; **no automatic access** for existing tokens (migration sets the uniform default).
- Vendor-account reference stored by **stable `account_id`** (primary key), never by display name.
- Prefix validation reuses `normalizeVendorAccountModelPrefix` rules: ≤64 bytes, charset `A-Za-z0-9-_.~:/@+`, no `..`, empty allowed.
- Collision policy **b1**: with `all=false`, reject at save time any config whose effective public names collide across the selected accounts' current models; the resolver keeps a deterministic first-wins backstop (stable account id order) for collisions that emerge later.
- Go migrations are **append-only**; `baselineCreateStatements` is frozen — add columns only via a new `addColumnIfMissing` migration.
- Any new `api_tokens` column requires lock-step edits: `tokenColumns`, `scanToken`, `CreatePlainToken`, `UpdateTokenMetadata` (if editable), the `auth.Token` literal in `SQLiteStore.LookupBearer`, `store.TokenRecord`, plus the three `portal/memory_directory.go` build sites and the memory copy-in.
- Store/migration change ⇒ run the **Postgres leg** (provision the DSN like CI) and the store-conformance parity tests.
- Pre-PR gates: Go build/vet/`go test ./...` + golangci-lint (gofumpt/gocritic) per module; frontend vitest + build + lint + **prettier format:check**; lint-docs; **SonarQube branch-findings = 0**.

## File Structure

New files:
- `gateway/backend/internal/auth/vendor_access.go` — `VendorAccess`, `VendorAccessEntry`, `cloneVendorAccess`.
- `gateway/backend/internal/store/token_vendor_access.go` — `DecodeVendorAccess`, `EncodeVendorAccess` (+ its test).
- `gateway/backend/internal/routing/vendor_access.go` — `TokenVendorPrefix` (+ its test).

Modified (by task):
- Store/auth layer: `store/models.go`, `store/migrate.go`, `store/sqlite_token.go`, `auth/token_store.go`, `portal/memory_directory.go`.
- Service layer: `portal/service.go`, `gateway/portal_token_endpoints.go`.
- Enforcement: `portal/service_vendor_listing.go`, `routing/resolver.go`.
- Frontend: `frontend/src/api/tokens.ts`, `frontend/src/components/TokenList.tsx`, `frontend/src/components/views.tsx`, `frontend/src/i18n.ts`.
- Docs: `docs/architecture/cross-cutting/external-vendor-accounts.md`, `api-surface.md`, `openapi.yaml`, a new ADR, data-model doc.

## Shared type definitions (used across tasks — exact names)

```go
// auth package
type VendorAccessEntry struct {
    AccountID       string // stable vendor-account id
    OverrideEnabled bool   // true => OverridePrefix replaces the account's ModelPrefix
    OverridePrefix  string // the override value; "" with OverrideEnabled => no prefix
}
type VendorAccess struct {
    All      bool                // true => every active owner account, native prefix; Accounts ignored
    Accounts []VendorAccessEntry // meaningful only when All==false
}
```

```go
// routing package — the ONE parity helper
// Returns the public-name prefix this token serves `acc` under, and whether `acc`
// is allowed for the token at all. Native prefix for All==true and for an entry
// with OverrideEnabled==false; the override value when OverrideEnabled==true.
func TokenVendorPrefix(access auth.VendorAccess, acc VendorAccount) (prefix string, allowed bool)
```

```go
// portal service DTO (wire)
type VendorAccessDTO struct {
    All      bool                   `json:"all"`
    Accounts []VendorAccessEntryDTO `json:"accounts,omitempty"`
}
type VendorAccessEntryDTO struct {
    AccountID      string             `json:"account_id"`
    PrefixOverride *PrefixOverrideDTO `json:"prefix_override,omitempty"`
}
type PrefixOverrideDTO struct {
    Enabled bool   `json:"enabled"`
    Value   string `json:"value"`
}
```

```ts
// frontend api/tokens.ts
export interface VendorAccessDTO {
  all: boolean
  accounts?: { account_id: string; prefix_override?: { enabled: boolean; value: string } }[]
}
```

Effective public name of a model `m` for an allowed account with prefix `p` is always `p + m.UpstreamModel` (both in listing and routing). For the native case `p == acc.ModelPrefix`, so this equals the stored `m.GatewayModel` — no regression.

---

## Task 1a: Vendor-access codec (pure functions)

**Files:**
- Create: `gateway/backend/internal/auth/vendor_access.go`
- Create: `gateway/backend/internal/store/token_vendor_access.go`
- Test: `gateway/backend/internal/store/token_vendor_access_test.go`

**Interfaces:**
- Produces: `auth.VendorAccess`, `auth.VendorAccessEntry`, `auth.cloneVendorAccess`; `store.DecodeVendorAccess(string) auth.VendorAccess`, `store.EncodeVendorAccess(auth.VendorAccess) string`.
- Consumes: nothing (pure).

- [ ] **Step 1: Write the codec types + clone (auth/vendor_access.go)**

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package auth

// VendorAccessEntry is one opted-in vendor account on a token, with its optional
// per-token model-name prefix override. OverrideEnabled==true means OverridePrefix
// REPLACES the account's own ModelPrefix for this token (an empty OverridePrefix
// then serves the account's models under their bare upstream names).
type VendorAccessEntry struct {
	AccountID       string
	OverrideEnabled bool
	OverridePrefix  string
}

// VendorAccess is a token's vendor-account (Anbieter) access policy, carried on
// Token and persisted as api_tokens.vendor_provider_access JSON.
//   - All==true: every active owner account under its own ModelPrefix (Accounts
//     ignored; future accounts auto-included).
//   - All==false: only Accounts; an empty slice means NO vendor access (strict
//     opt-in default).
type VendorAccess struct {
	All      bool
	Accounts []VendorAccessEntry
}

// cloneVendorAccess deep-copies the policy so a stored or returned Token never
// aliases the caller's slice. Entries are value types (scalars only), so a slice
// copy is a full copy.
func cloneVendorAccess(v VendorAccess) VendorAccess {
	if len(v.Accounts) == 0 {
		return VendorAccess{All: v.All}
	}
	return VendorAccess{All: v.All, Accounts: append([]VendorAccessEntry(nil), v.Accounts...)}
}
```

- [ ] **Step 2: Write the failing codec test (store/token_vendor_access_test.go)**

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"op-ai-gateway/internal/auth"
	"reflect"
	"testing"
)

func TestVendorAccessRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   auth.VendorAccess
		want string // canonical encoded form
	}{
		{"strict default", auth.VendorAccess{}, ""},
		{"all", auth.VendorAccess{All: true}, `{"all":true}`},
		{"explicit native", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1"}}}, `{"accounts":[{"account_id":"acc_1"}]}`},
		{"explicit override value", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "foo/"}}}, `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":"foo/"}}]}`},
		{"explicit override empty", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}, `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":""}}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EncodeVendorAccess(tc.in)
			if got != tc.want {
				t.Fatalf("Encode = %q, want %q", got, tc.want)
			}
			back := DecodeVendorAccess(got)
			if !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("Decode(Encode) = %#v, want %#v", back, tc.in)
			}
		})
	}
}

func TestDecodeVendorAccessTolerant(t *testing.T) {
	for _, s := range []string{"", "   ", "not json", "{", `{"accounts":[{"account_id":""}]}`} {
		got := DecodeVendorAccess(s)
		if got.All || len(got.Accounts) != 0 {
			t.Fatalf("DecodeVendorAccess(%q) = %#v, want strict default", s, got)
		}
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd gateway/backend && go test ./internal/store/ -run TestVendorAccess -v`
Expected: FAIL — `EncodeVendorAccess`/`DecodeVendorAccess` undefined.

- [ ] **Step 4: Implement the codec (store/token_vendor_access.go)**

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"encoding/json"
	"op-ai-gateway/internal/auth"
	"strings"
)

// vendor_provider_access wire shape. Kept local; auth.VendorAccess is the
// in-memory type (store imports auth, auth cannot import store).
type vendorAccessWire struct {
	All      bool                    `json:"all,omitempty"`
	Accounts []vendorAccessEntryWire `json:"accounts,omitempty"`
}
type vendorAccessEntryWire struct {
	AccountID      string              `json:"account_id"`
	PrefixOverride *prefixOverrideWire `json:"prefix_override,omitempty"`
}
type prefixOverrideWire struct {
	Enabled bool   `json:"enabled"`
	Value   string `json:"value"`
}

// DecodeVendorAccess parses api_tokens.vendor_provider_access. Blank or malformed
// yields the strict default (All=false, no accounts) — a bad row never breaks
// token resolution. Entries with a blank account_id are dropped.
func DecodeVendorAccess(s string) auth.VendorAccess {
	if strings.TrimSpace(s) == "" {
		return auth.VendorAccess{}
	}
	var w vendorAccessWire
	if err := json.Unmarshal([]byte(s), &w); err != nil {
		return auth.VendorAccess{}
	}
	out := auth.VendorAccess{All: w.All}
	for _, e := range w.Accounts {
		id := strings.TrimSpace(e.AccountID)
		if id == "" {
			continue
		}
		entry := auth.VendorAccessEntry{AccountID: id}
		if e.PrefixOverride != nil {
			entry.OverrideEnabled = true
			entry.OverridePrefix = e.PrefixOverride.Value
		}
		out.Accounts = append(out.Accounts, entry)
	}
	return out
}

// EncodeVendorAccess serializes the policy. The strict default encodes to "" (the
// column default) so "no access" round-trips as the empty string, not "{}".
func EncodeVendorAccess(v auth.VendorAccess) string {
	if !v.All && len(v.Accounts) == 0 {
		return ""
	}
	w := vendorAccessWire{All: v.All}
	for _, e := range v.Accounts {
		ew := vendorAccessEntryWire{AccountID: e.AccountID}
		if e.OverrideEnabled {
			ew.PrefixOverride = &prefixOverrideWire{Enabled: true, Value: e.OverridePrefix}
		}
		w.Accounts = append(w.Accounts, ew)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return ""
	}
	return string(b)
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd gateway/backend && go test ./internal/store/ -run TestVendorAccess -v` and `cd gateway/backend && go test ./internal/auth/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/auth/vendor_access.go gateway/backend/internal/store/token_vendor_access.go gateway/backend/internal/store/token_vendor_access_test.go
git commit -m "feat: Vendor-access codec for per-token provider access

auth.VendorAccess/{Entry} + cloneVendorAccess, and store
Decode/EncodeVendorAccess for the api_tokens.vendor_provider_access
JSON column. Strict default round-trips as the empty string; decode is
tolerant (blank/malformed -> default)."
```

---

## Task 1b: Persist vendor access on the token (migration + store + auth.Token)

**Files:**
- Modify: `gateway/backend/internal/store/models.go` (`TokenRecord`, +`VendorProviderAccess string`)
- Modify: `gateway/backend/internal/store/migrate.go` (append `migration85Up` + slice entry)
- Modify: `gateway/backend/internal/store/sqlite_token.go` (`tokenColumns`, `scanToken`, `CreatePlainToken`, `UpdateTokenMetadata`, `LookupBearer` literal)
- Modify: `gateway/backend/internal/auth/token_store.go` (`Token` field + 3 clone sites)
- Modify: `gateway/backend/internal/portal/memory_directory.go` (3 build sites + copy-in)
- Test: `gateway/backend/internal/store/sqlite_token_vendor_access_test.go` (+ extend the store-conformance harness)

**Interfaces:**
- Consumes: `store.Decode/EncodeVendorAccess`, `auth.VendorAccess` (Task 1a).
- Produces: a persisted, editable `TokenRecord.VendorProviderAccess` and a populated `auth.Token.VendorAccess` on every bearer lookup.

- [ ] **Step 1: Write the failing persistence test (store/sqlite_token_vendor_access_test.go)**

Model it on the existing token store tests (same package `store`). It creates a token with a non-default vendor access, looks the bearer up, and asserts the field survives create + update. Use the existing test helpers/fixtures in the package (find them with `grep -n "func newTestStore\|CreatePlainToken(" gateway/backend/internal/store/*_test.go`).

```go
func TestTokenVendorProviderAccessPersists(t *testing.T) {
	st := newTestSQLiteStore(t) // existing helper in the store test package
	ctx := context.Background()
	rec := sampleUserTokenRecord(t)     // existing helper that builds a minimal valid user TokenRecord + secret
	rec.VendorProviderAccess = `{"accounts":[{"account_id":"acc_1","prefix_override":{"enabled":true,"value":""}}]}`
	secret := mustCreateToken(t, st, ctx, rec) // existing helper; returns the plaintext secret

	tok, err := st.LookupBearer(ctx, "Bearer "+secret)
	if err != nil {
		t.Fatalf("LookupBearer: %v", err)
	}
	want := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}
	if !reflect.DeepEqual(tok.VendorAccess, want) {
		t.Fatalf("VendorAccess = %#v, want %#v", tok.VendorAccess, want)
	}

	rec.VendorProviderAccess = `{"all":true}`
	if err := st.UpdateTokenMetadata(ctx, rec); err != nil {
		t.Fatalf("UpdateTokenMetadata: %v", err)
	}
	got, err := st.TokenByID(ctx, rec.ID)
	if err != nil {
		t.Fatalf("TokenByID: %v", err)
	}
	if got.VendorProviderAccess != `{"all":true}` {
		t.Fatalf("VendorProviderAccess = %q, want {\"all\":true}", got.VendorProviderAccess)
	}
}
```

(If the package has no `sampleUserTokenRecord`/`mustCreateToken` helpers, write the minimal `TokenRecord` inline following an existing `CreatePlainToken` test.)

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/store/ -run TestTokenVendorProviderAccessPersists -v`
Expected: FAIL — `TokenRecord` has no `VendorProviderAccess`; `auth.Token` has no `VendorAccess`.

- [ ] **Step 3: Add the record field (store/models.go)**

In `TokenRecord` (after `ModelOverrideMap`, grouping with the other JSON-in-text columns):

```go
	// VendorProviderAccess is the per-token vendor-account (Anbieter) access policy,
	// stored as a JSON object string in api_tokens.vendor_provider_access.
	// "" = the strict default (no vendor access). Decoded/encoded at the edges by
	// DecodeVendorAccess / EncodeVendorAccess.
	VendorProviderAccess string
```

- [ ] **Step 4: Add the migration (store/migrate.go)**

Append to the `migrations` slice (after the version-84 entry, slice around line 119):

```go
	{version: 85, name: "api_token_vendor_provider_access", up: migration85Up},
```

Append the function at the end of the file (after `migration84Up`):

```go
// migration85Up adds api_tokens.vendor_provider_access: the per-token
// vendor-account access policy (JSON object string; "" = strict default, no
// vendor access). See store.DecodeVendorAccess / EncodeVendorAccess.
func migration85Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	return addColumnIfMissing(ctx, tx, dl, "api_tokens", "vendor_provider_access text not null default ''")
}
```

- [ ] **Step 5: Thread the column through sqlite_token.go**

1. `tokenColumns` const: append `vendor_provider_access` as the LAST selected column (keep the `coalesce` style only if the column were nullable — it is `not null default ''`, so a plain column name).
2. `scanToken`: add `&record.VendorProviderAccess` as the LAST Scan arg, in the SAME position as the column.
3. `CreatePlainToken`: add `vendor_provider_access` to the INSERT column list, one more `?` placeholder, and `record.VendorProviderAccess` to the args (it is NOT a nullable FK, so no `nullableTokenRef`).
4. `UpdateTokenMetadata`: add `vendor_provider_access = ?` to the SET clause and `record.VendorProviderAccess` to the args (it IS user-editable, unlike `last_used_model`).
5. `SQLiteStore.LookupBearer` auth.Token literal: add `VendorAccess: DecodeVendorAccess(record.VendorProviderAccess),` (resolve it in the same block as the other decoded fields, before the `last_used_at` touch).

- [ ] **Step 6: Add the auth.Token field + clone (auth/token_store.go)**

In `Token` struct (near `ModelOverrideRules`/`AllowedModels`):

```go
	// VendorAccess is the token's vendor-account (Anbieter) access policy, mirrored
	// from store.TokenRecord.VendorProviderAccess. Zero value (All=false, no
	// accounts) = no vendor access.
	VendorAccess VendorAccess
```

In ALL THREE clone blocks (`AddPlainToken` ~132-134, `UpdateToken` ~145-147, `LookupBearer` copy-out ~215-217), add:

```go
	stored.VendorAccess = cloneVendorAccess(token.VendorAccess)
```

(use the local variable name each block already uses for the copy — match the existing `.ModelOverrideRules = cloneOverrideMap(...)` line in the same block).

- [ ] **Step 7: Thread through the memory driver (portal/memory_directory.go)**

In each of the three `auth.Token{...}` build sites — `CreatePlainToken` (~90-159), `SetServiceTokensState` (~176-216), `UpdateTokenMetadata` (~270-328) — add to the literal:

```go
		VendorAccess: store.DecodeVendorAccess(<record>.VendorProviderAccess),
```

where `<record>` is the local the site already reads from (`token`, `token`, `existing` respectively). In the `UpdateTokenMetadata` copy-in block (~277-298, before `m.tokens[token.ID] = existing`), add `existing.VendorProviderAccess = token.VendorProviderAccess` (it is editable). (Service tokens keep whatever default they were created with; vendor access is user-token-only but the column is uniform.)

- [ ] **Step 8: Run tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/store/ ./internal/auth/ ./internal/portal/ -run 'Token' -v`
Expected: PASS.

- [ ] **Step 9: Extend the store-conformance harness**

Find the token conformance case (`grep -n "VendorProviderAccess\|ModelOverrideMap\|TestTokenRepository" gateway/backend/internal/store/*conformance*`). Add `VendorProviderAccess` to the round-trip case so memory-vs-SQL parity is pinned (both drivers must agree). Run with the Postgres DSN provisioned (the postgres subtests skip silently without it).

- [ ] **Step 10: Commit**

```bash
git add gateway/backend/internal/store/ gateway/backend/internal/auth/token_store.go gateway/backend/internal/portal/memory_directory.go
git commit -m "feat: Persist per-token vendor-access on api_tokens

Add api_tokens.vendor_provider_access (migration 85) and thread it
through TokenRecord, the SQLite column/scan/insert/update lock-step,
auth.Token (+clone at all three sites), the three memory-driver build
sites, and the conformance harness. LookupBearer now populates
auth.Token.VendorAccess."
```

---

## Task 2: Service DTO, validation, collision check, error mapping

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (DTO types, `TokenDTO`, `CreateTokenRequest`, `UpdateTokenRequest`, `CreateToken`, `UpdateToken`, `tokenDTO`, `AuthorizeRunAsToken`, new validator + errors + converters)
- Modify: `gateway/backend/internal/gateway/portal_token_endpoints.go` (both error ladders)
- Test: `gateway/backend/internal/portal/service_token_vendor_access_test.go`

**Interfaces:**
- Consumes: `store.Decode/EncodeVendorAccess`, `auth.VendorAccess`, `normalizeVendorAccountModelPrefix`, `s.routes.VendorAccountsByOwner`, `s.routes.VendorAccountModels`.
- Produces: `VendorAccessDTO`/`VendorAccessEntryDTO`/`PrefixOverrideDTO` on the token DTOs; `ErrTokenVendorAccessInvalid`, `ErrTokenVendorAccessConflict`; converters `encodeVendorAccessDTO`, `decodeVendorAccessDTO`, validator `validateVendorAccess`.

- [ ] **Step 1: Write the failing validation test (service_token_vendor_access_test.go)**

Build a `Service` with a memory directory + a routing memory store seeded with two owner accounts, each serving slug `gpt-4o` under a prefix. Use the existing service test harness (`grep -n "func newTestService\|newServiceForTest" gateway/backend/internal/portal/*_test.go`). Assert:

```go
func TestCreateTokenVendorAccessValidation(t *testing.T) {
	svc, owner := newServiceWithVendorAccounts(t, // existing-style harness; seeds acc_a (prefix "a/") + acc_b (prefix "b/"), both serving gpt-4o
	)
	ctx := context.Background()

	// foreign account id -> ErrTokenVendorAccessInvalid
	_, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "t1", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_not_mine"}}}})
	if !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("foreign id: err = %v, want ErrTokenVendorAccessInvalid", err)
	}

	// bad prefix -> ErrTokenVendorAccessInvalid
	_, err = svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "t2", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: "bad prefix with spaces"}}}}})
	if !errors.Is(err, ErrTokenVendorAccessInvalid) {
		t.Fatalf("bad prefix: err = %v, want ErrTokenVendorAccessInvalid", err)
	}

	// collision: both accounts overridden to empty prefix -> both serve "gpt-4o" -> ErrTokenVendorAccessConflict
	_, err = svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "t3", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			{AccountID: "acc_b", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
		}}})
	if !errors.Is(err, ErrTokenVendorAccessConflict) {
		t.Fatalf("collision: err = %v, want ErrTokenVendorAccessConflict", err)
	}

	// happy: distinct prefixes round-trip through the DTO
	resp, err := svc.CreateToken(ctx, owner, CreateTokenRequest{Name: "t4", Scopes: []string{"gateway:use"},
		VendorAccess: &VendorAccessDTO{Accounts: []VendorAccessEntryDTO{
			{AccountID: "acc_a", PrefixOverride: &PrefixOverrideDTO{Enabled: true, Value: ""}},
			{AccountID: "acc_b"},
		}}})
	if err != nil {
		t.Fatalf("happy: %v", err)
	}
	if resp.Token.VendorAccess == nil || len(resp.Token.VendorAccess.Accounts) != 2 {
		t.Fatalf("DTO did not round-trip: %+v", resp.Token.VendorAccess)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/portal/ -run TestCreateTokenVendorAccessValidation -v`
Expected: FAIL — types/fields/errors undefined.

- [ ] **Step 3: Add DTO types + errors + converters (service.go)**

Add the three DTO structs (see Shared type definitions). Add sentinel errors near the other `ErrToken*` vars:

```go
var (
	ErrTokenVendorAccessInvalid  = errors.New("token vendor access invalid")
	ErrTokenVendorAccessConflict = errors.New("token vendor access conflict")
)
```

Add converters + validator:

```go
func encodeVendorAccessDTO(d *VendorAccessDTO) string {
	if d == nil {
		return ""
	}
	v := auth.VendorAccess{All: d.All}
	for _, e := range d.Accounts {
		entry := auth.VendorAccessEntry{AccountID: strings.TrimSpace(e.AccountID)}
		if e.PrefixOverride != nil {
			entry.OverrideEnabled = true
			entry.OverridePrefix = e.PrefixOverride.Value
		}
		v.Accounts = append(v.Accounts, entry)
	}
	return store.EncodeVendorAccess(v)
}

func decodeVendorAccessDTO(s string) *VendorAccessDTO {
	v := store.DecodeVendorAccess(s)
	if !v.All && len(v.Accounts) == 0 {
		return nil
	}
	d := &VendorAccessDTO{All: v.All}
	for _, e := range v.Accounts {
		entry := VendorAccessEntryDTO{AccountID: e.AccountID}
		if e.OverrideEnabled {
			entry.PrefixOverride = &PrefixOverrideDTO{Enabled: true, Value: e.OverridePrefix}
		}
		d.Accounts = append(d.Accounts, entry)
	}
	return d
}

// validateVendorAccess checks a token's requested vendor-access policy against the
// owner's own accounts: every account_id must be owned, every override prefix must
// pass normalizeVendorAccountModelPrefix, and (All==false) the effective public
// names must not collide across the selected accounts' current models.
func (s *Service) validateVendorAccess(ctx context.Context, owner Principal, d *VendorAccessDTO) error {
	if d == nil || d.All || len(d.Accounts) == 0 {
		return nil // All / empty need no per-account checks
	}
	if s.routes == nil {
		return fmt.Errorf("%w: vendor accounts unavailable", ErrTokenVendorAccessInvalid)
	}
	accounts, err := s.routes.VendorAccountsByOwner(ctx, owner.UserID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrTokenVendorAccessInvalid, err)
	}
	byID := make(map[string]routing.VendorAccount, len(accounts))
	for _, a := range accounts {
		byID[a.ID] = a
	}
	names := make(map[string]string) // effective public name -> account id that first produced it
	for _, e := range d.Accounts {
		acc, ok := byID[e.AccountID]
		if !ok {
			return fmt.Errorf("%w: unknown account %q", ErrTokenVendorAccessInvalid, e.AccountID)
		}
		prefix := acc.ModelPrefix
		if e.PrefixOverride != nil {
			norm, err := normalizeVendorAccountModelPrefix(e.PrefixOverride.Value)
			if err != nil {
				return fmt.Errorf("%w: %v", ErrTokenVendorAccessInvalid, err)
			}
			prefix = norm
		}
		models, err := s.routes.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			continue // fail-open on read; routing backstop still protects at dispatch
		}
		for _, m := range models {
			name := prefix + m.UpstreamModel
			if prior, dup := names[name]; dup && prior != e.AccountID {
				return fmt.Errorf("%w: %q served by both %q and %q", ErrTokenVendorAccessConflict, name, prior, e.AccountID)
			}
			names[name] = e.AccountID
		}
	}
	return nil
}
```

(Confirm `normalizeVendorAccountModelPrefix` returns `(string, error)`; if it returns only `error` with in-place normalization, adapt — read `gateway/backend/internal/portal/service_vendor_accounts.go:225`.)

- [ ] **Step 4: Thread the field through DTO + Create/Update + mapping**

- `TokenDTO`: add `VendorAccess *VendorAccessDTO `json:"vendor_access,omitempty"``.
- `CreateTokenRequest`: add `VendorAccess *VendorAccessDTO `json:"vendor_access,omitempty"`` (plain pointer; nil = default).
- `UpdateTokenRequest`: add `VendorAccess *VendorAccessDTO `json:"vendor_access,omitempty"`` (nil = keep stored).
- `CreateToken`: after the existing model-override validation, `if err := s.validateVendorAccess(ctx, owner, req.VendorAccess); err != nil { return CreateTokenResponse{}, err }`, and set `record.VendorProviderAccess = encodeVendorAccessDTO(req.VendorAccess)` on the `store.TokenRecord` literal.
- `UpdateToken`: add a block mirroring the pointer-optional pattern:

```go
	if req.VendorAccess != nil {
		if err := s.validateVendorAccess(ctx, owner, req.VendorAccess); err != nil {
			return TokenDTO{}, err
		}
		record.VendorProviderAccess = encodeVendorAccessDTO(req.VendorAccess)
	}
```

- `tokenDTO`: set `dto.VendorAccess = decodeVendorAccessDTO(record.VendorProviderAccess)`.
- `AuthorizeRunAsToken` (~4485-4522): add `VendorAccess: store.DecodeVendorAccess(record.VendorProviderAccess),` to the built `auth.Token` so run-as honors the policy.

- [ ] **Step 5: Map the new errors (portal_token_endpoints.go)**

- In the inlined create ladder (`handlePortalTokens`), add before the generic 500 fallback:
  `case errors.Is(err, ErrTokenVendorAccessInvalid): ... 400 "portal.token_vendor_access_invalid"`
  `case errors.Is(err, ErrTokenVendorAccessConflict): ... 400 "portal.token_vendor_access_conflict"`
- In `portalTokenErrRows` (used by PATCH/DELETE), add two rows:
  `{err: ErrTokenVendorAccessInvalid, status: 400, code: "portal.token_vendor_access_invalid", msg: "token vendor access invalid"}`
  `{err: ErrTokenVendorAccessConflict, status: 400, code: "portal.token_vendor_access_conflict", msg: "token vendor access conflict"}`

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd gateway/backend && go test ./internal/portal/ ./internal/gateway/ -run 'Token' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add gateway/backend/internal/portal/service.go gateway/backend/internal/portal/service_token_vendor_access_test.go gateway/backend/internal/gateway/portal_token_endpoints.go
git commit -m "feat: Token API carries vendor-access with validation

Add vendor_access to TokenDTO/Create/UpdateTokenRequest, validate the
selected accounts (owned, prefix normalized) and reject save-time name
collisions (all=false), map ErrTokenVendorAccess{Invalid,Conflict} in
both token error ladders, and honor the policy in run-as."
```

---

## Task 3: Listing enforcement (filter + prefix relabel)

**Files:**
- Create: `gateway/backend/internal/routing/vendor_access.go` (`TokenVendorPrefix`)
- Test: `gateway/backend/internal/routing/vendor_access_test.go`
- Modify: `gateway/backend/internal/portal/service_vendor_listing.go` (`ownVendorAccountModels`)
- Test: `gateway/backend/internal/portal/service_vendor_listing_access_test.go`

**Interfaces:**
- Consumes: `auth.VendorAccess`, `routing.VendorAccount`, portal `relabelVendorModels`.
- Produces: `routing.TokenVendorPrefix(access, acc) (prefix string, allowed bool)`; a filtered + token-relabeled `ownVendorAccountModels`.

- [ ] **Step 1: Write the failing helper test (routing/vendor_access_test.go)**

```go
func TestTokenVendorPrefix(t *testing.T) {
	acc := VendorAccount{ID: "acc_1", ModelPrefix: "native/"}
	cases := []struct {
		name        string
		access      auth.VendorAccess
		wantPrefix  string
		wantAllowed bool
	}{
		{"all uses native", auth.VendorAccess{All: true}, "native/", true},
		{"not listed -> denied", auth.VendorAccess{}, "", false},
		{"listed, no override -> native", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1"}}}, "native/", true},
		{"listed, override value", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "x/"}}}, "x/", true},
		{"listed, override empty -> no prefix", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}, "", true},
		{"other account listed -> denied", auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_2"}}}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := TokenVendorPrefix(tc.access, acc)
			if p != tc.wantPrefix || ok != tc.wantAllowed {
				t.Fatalf("= (%q,%v), want (%q,%v)", p, ok, tc.wantPrefix, tc.wantAllowed)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/routing/ -run TestTokenVendorPrefix -v`
Expected: FAIL — `TokenVendorPrefix` undefined.

- [ ] **Step 3: Implement the helper (routing/vendor_access.go)**

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "op-ai-gateway/internal/auth"

// TokenVendorPrefix reports the public model-name prefix a token serves `acc`
// under, and whether `acc` is allowed for the token at all. It is the single
// source both the listing overlay and the resolver use, so advertised names
// always equal routable names. The effective public name of a model `m` is
// prefix + m.UpstreamModel.
//
//   - All==true: every account allowed, under its own ModelPrefix (native).
//   - An entry with OverrideEnabled==false: allowed, native ModelPrefix.
//   - An entry with OverrideEnabled==true: allowed, the override prefix (which may
//     be "" => bare upstream names).
//   - Not All and not listed: denied.
func TokenVendorPrefix(access auth.VendorAccess, acc VendorAccount) (prefix string, allowed bool) {
	if access.All {
		return acc.ModelPrefix, true
	}
	for _, e := range access.Accounts {
		if e.AccountID != acc.ID {
			continue
		}
		if e.OverrideEnabled {
			return e.OverridePrefix, true
		}
		return acc.ModelPrefix, true
	}
	return "", false
}
```

- [ ] **Step 4: Write the failing listing test (service_vendor_listing_access_test.go)**

Using the portal service harness with a routing store seeded with two owner accounts (acc_a prefix `a/`, acc_b prefix `b/`, each serving `gpt-4o`), assert the flavor-set keys that `vendorModelFlavorSets(ctx, token)` returns for different `token.VendorAccess`:

```go
func TestVendorListingHonorsTokenAccess(t *testing.T) {
	svc, token := newListingHarness(t) // token is a user auth.Token; svc.routes seeded with acc_a("a/")+acc_b("b/") each serving gpt-4o

	names := func(acc auth.VendorAccess) []string {
		token.VendorAccess = acc
		sets := svc.vendorModelFlavorSets(context.Background(), token)
		return sortedKeys(sets) // test helper
	}

	if got := names(auth.VendorAccess{}); len(got) != 0 {
		t.Fatalf("strict default should list nothing, got %v", got)
	}
	if got := names(auth.VendorAccess{All: true}); !reflect.DeepEqual(got, []string{"a/gpt-4o", "b/gpt-4o"}) {
		t.Fatalf("all: got %v", got)
	}
	if got := names(auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_a", OverrideEnabled: true, OverridePrefix: ""}}}); !reflect.DeepEqual(got, []string{"gpt-4o"}) {
		t.Fatalf("explicit acc_a empty-override: got %v, want [gpt-4o]", got)
	}
}
```

- [ ] **Step 5: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/portal/ -run TestVendorListingHonorsTokenAccess -v`
Expected: FAIL — listing ignores `VendorAccess` today (lists everything).

- [ ] **Step 6: Filter + relabel in ownVendorAccountModels (service_vendor_listing.go)**

Replace the account loop body so each account is gated and relabeled with the token-effective prefix:

```go
	var out []vendorAccountModels
	for _, acc := range accounts {
		if acc.Status != routing.VendorAccountStatusActive {
			continue
		}
		prefix, allowed := routing.TokenVendorPrefix(token.VendorAccess, acc)
		if !allowed {
			continue
		}
		models, err := s.routes.VendorAccountModels(ctx, acc.ID)
		if err != nil {
			continue
		}
		models, _ = relabelVendorModels(models, prefix) // token-effective public names (native when no override)
		out = append(out, vendorAccountModels{Account: acc, Models: models})
	}
	return out
```

`relabelVendorModels` already produces `prefix + UpstreamModel` and dedups. Downstream (`vendorModelFlavorSets`, `vendorDashboardRoutes`) keep keying on `m.GatewayModel` unchanged — they now see the token-effective names.

- [ ] **Step 7: Run to verify it passes**

Run: `cd gateway/backend && go test ./internal/portal/ ./internal/routing/ -run 'VendorListing|TokenVendorPrefix' -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add gateway/backend/internal/routing/vendor_access.go gateway/backend/internal/routing/vendor_access_test.go gateway/backend/internal/portal/service_vendor_listing.go gateway/backend/internal/portal/service_vendor_listing_access_test.go
git commit -m "feat: Vendor model listing honors per-token access + prefix

Add routing.TokenVendorPrefix (the shared parity helper) and apply it in
ownVendorAccountModels: filter to the token's allowed accounts and
relabel each model to the token-effective prefix (empty override = bare
names). All listings (/v1/models, Anthropic, chat picker, dashboard)
flow through this one choke point."
```

---

## Task 4: Routing enforcement + reverse map (the access boundary)

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`resolveVendorAccount`, `vendorAccountModelMatch`)
- Test: `gateway/backend/internal/routing/resolver_token_access_test.go`

**Interfaces:**
- Consumes: `routing.TokenVendorPrefix` (Task 3), the existing test helpers `vendorResolver`, `ownerToken`, `seedPrefixedVendorAccount`, `vendorOwner`, `vendorKey`, `must` (in `resolver_vendor_*_test.go`).
- Produces: a resolver that filters vendor accounts by `token.VendorAccess` and reverse-maps the token prefix.

- [ ] **Step 1: Write the failing routing test (resolver_token_access_test.go)**

Reuse the Task-6 template's `seedPrefixedVendorAccount` + `ownerToken()` (both in package `routing`). Note `ownerToken()` currently returns a token with no `VendorAccess`; after this task a no-access token must resolve NOTHING, so build an explicit token here.

```go
func tokenWithAccess(a auth.VendorAccess) auth.Token {
	tok := ownerToken()
	tok.VendorAccess = a
	return tok
}

func TestResolveHonorsTokenVendorAccess(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	store := NewMemoryStore()
	// Account stored with native prefix "native/" serving slug gpt-4o.
	seedPrefixedVendorAccount(t, store, now, "acc_1", VendorOpenAI, VendorAuthAPIKey, "native/", "gpt-4o", APIFlavorOpenAI)
	resolver := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	// (a) strict default (no access): even the native name must NOT resolve.
	if _, err := resolver.Resolve(ctx, tokenWithAccess(auth.VendorAccess{}), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("no-access token resolved a vendor model; want ErrNoModelRoute")
	}

	// (b) All: native name resolves, raw slug sent upstream.
	target, err := resolver.Resolve(ctx, tokenWithAccess(auth.VendorAccess{All: true}), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "native/gpt-4o" {
		t.Fatalf("all: target=%+v err=%v", target, err)
	}

	// (c) Override to a new prefix: the NEW name resolves (reverse map), native name does not.
	acc := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: "x/"}}}
	target, err = resolver.Resolve(ctx, tokenWithAccess(acc), inference.Request{Model: "x/gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "x/gpt-4o" {
		t.Fatalf("override: target=%+v err=%v", target, err)
	}
	if _, err := resolver.Resolve(ctx, tokenWithAccess(acc), inference.Request{Model: "native/gpt-4o", APIFlavor: "openai_chat"}); !errors.Is(err, ErrNoModelRoute) {
		t.Fatalf("override: native name should no longer resolve")
	}

	// (d) Override to empty: the bare slug resolves.
	accEmpty := auth.VendorAccess{Accounts: []auth.VendorAccessEntry{{AccountID: "acc_1", OverrideEnabled: true, OverridePrefix: ""}}}
	target, err = resolver.Resolve(ctx, tokenWithAccess(accEmpty), inference.Request{Model: "gpt-4o", APIFlavor: "openai_chat"})
	if err != nil || target.ProviderModel != "gpt-4o" || target.Model != "gpt-4o" {
		t.Fatalf("empty-override: target=%+v err=%v", target, err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `cd gateway/backend && go test ./internal/routing/ -run TestResolveHonorsTokenVendorAccess -v`
Expected: FAIL — resolver ignores `VendorAccess`; (a) resolves instead of 404, (c)/(d) 404 instead of resolving.

- [ ] **Step 3: Filter + reverse-map in the resolver (resolver.go)**

In `resolveVendorAccount`, inside the account loop, after the `Status` check and before the subscription-flavor guard, add the access gate and capture the prefix:

```go
		prefix, allowed := TokenVendorPrefix(token.VendorAccess, acc)
		if !allowed {
			continue
		}
```

Change the match call to pass `prefix`:

```go
		if t, ok := vendorAccountModelMatch(acc, models, prefix, req, apiFlavor); ok {
			return t, true, nil
		}
```

Change `vendorAccountModelMatch` to reverse-map via the prefix instead of matching the stored `GatewayModel`:

```go
func vendorAccountModelMatch(acc VendorAccount, models []VendorAccountModel, prefix string, req inference.Request, apiFlavor string) (Target, bool) {
	for _, m := range models {
		if prefix+m.UpstreamModel != req.Model {
			continue
		}
		return vendorAccountModelTarget(acc, m, req, apiFlavor)
	}
	return Target{}, false
}
```

(For `All==true` / no-override, `prefix == acc.ModelPrefix`, so `prefix + m.UpstreamModel == m.GatewayModel` — identical to today. The account iteration stays in stable id order, so later-emerging collisions resolve first-wins — the b1 backstop.) Update the doc comment on `vendorAccountModelMatch` to describe the prefix reverse-map.

- [ ] **Step 4: Run to verify it passes (and no regression)**

Run: `cd gateway/backend && go test ./internal/routing/ -v`
Expected: PASS — the new test and the existing `resolver_vendor_prefix_test.go` (its `ownerToken()` must now carry access; if that test's `ownerToken()` has no `VendorAccess`, it will newly fail → update `ownerToken()` to return `VendorAccess{All: true}` so the existing prefix tests keep asserting today's behavior, OR adjust those tests to set `All: true`). Decide by reading `resolver_vendor_account_test.go:48`; prefer making `ownerToken()` return `All: true` (it represents "the owner can use their accounts").

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/resolver.go gateway/backend/internal/routing/resolver_token_access_test.go gateway/backend/internal/routing/resolver_vendor_account_test.go
git commit -m "feat: Resolver enforces per-token vendor access + prefix reverse map

resolveVendorAccount skips accounts the token has not opted into and
matches the request against the token-effective prefix + upstream model
(empty override => bare slug), translating back to the raw upstream slug
at dispatch. Stable id order keeps the first-wins collision backstop."
```

---

## Task 5: Frontend — token form section + wiring

**Files:**
- Modify: `gateway/frontend/src/api/tokens.ts` (`VendorAccessDTO` + field on the three types)
- Modify: `gateway/frontend/src/components/TokenList.tsx` (api Pick, prop, state, lazy-load, JSX, submit)
- Modify: `gateway/frontend/src/components/views.tsx` (pass `vendorAccountsEnabled`)
- Modify: `gateway/frontend/src/i18n.ts` (de + en keys)
- Test: `gateway/frontend/src/components/TokenList.test.tsx` (extend), `gateway/frontend/src/i18n.test.ts` (extend)

**Interfaces:**
- Consumes: the `vendor_access` wire shape (Task 2), `api.vendorAccounts()` (`{data: VendorAccount[]}`), `VendorAccount.model_prefix`, `CheckboxGroup`, `ctx.vendorAccountsEnabled`.
- Produces: a flag-gated vendor-access section that round-trips create/edit.

- [ ] **Step 1: Add the wire type (api/tokens.ts)**

Add `VendorAccessDTO` (see Shared type definitions) and `vendor_access?: VendorAccessDTO` to `PortalToken`, `CreateTokenRequest`, `UpdateTokenRequest`.

- [ ] **Step 2: Add i18n keys (i18n.ts) + the failing i18n test (i18n.test.ts)**

Add, adjacent to the `tokenProject*` group in BOTH `de` (≈1982) and `en` (≈4613) blocks:

```ts
tokenVendorAccessLabel: 'Anbieter-Zugriff', // en: 'Provider access'
tokenVendorAccessNote: 'Wähle, welche Anbieter dieser Token nutzen darf.', // en: '…which providers this token may use.'
tokenVendorAccessAll: 'Alle Anbieter (auch künftige)', // en: 'All providers (incl. future)'
tokenVendorAccessAllHint: 'Nutzt alle verbundenen Konten unter ihrem eigenen Prefix.', // en: 'Uses every connected account under its own prefix.'
tokenVendorAccessAccountsLabel: 'Verfügbare Anbieter', // en: 'Available providers'
tokenVendorAccessPrefixToggle: 'Prefix überschreiben', // en: 'Override prefix'
tokenVendorAccessPrefixLabel: 'Prefix', // en: 'Prefix'
tokenVendorAccessPrefixEmptyHint: 'Leer = Modelle unter Originalnamen ohne Prefix.', // en: 'Empty = models under their original names, no prefix.'
tokenVendorAccessNone: 'Keine Anbieter verbunden.', // en: 'No providers connected.'
errorTokenVendorAccessInvalid: 'Anbieter-Zugriff ungültig.', // en: 'Provider access is invalid.'
errorTokenVendorAccessConflict: 'Zwei Anbieter liefern denselben Modellnamen — vergib einen eigenen Prefix.', // en: 'Two providers serve the same model name — give one a distinct prefix.'
```

In `i18n.test.ts`, add a `describe('token vendor access i18n keys')` block listing these keys `as const` and looping the `expect(typeof messages.de[k]).toBe('string')` parity assertion (copy the existing 'server override i18n keys' block).

Run: `cd gateway/frontend && npx vitest run src/i18n.test.ts` → initially FAIL (keys missing), then PASS after adding them. Also map the two backend error codes in `formatPortalError` (find it: `grep -rn "errorTokenProjectNotMember" src/`) so `portal.token_vendor_access_invalid`/`_conflict` → the `errorTokenVendorAccess*` keys.

- [ ] **Step 3: Plumb the prop (views.tsx + TokenList signature)**

- `views.tsx` `viewRegistry.tokens` mount: add `vendorAccountsEnabled={ctx.vendorAccountsEnabled}`.
- `TokenList` props: add `vendorAccountsEnabled?: boolean` (default `false`, like `servers = []`), and add `'vendorAccounts'` to the `api` `Pick<PortalApi, …>`.

- [ ] **Step 4: Write the failing component test (TokenList.test.tsx)**

Extend `renderTokenList`'s `fakeApi` with `vendorAccounts: vi.fn(async () => ({ data: [{ id: 'acc_a', vendor: 'openai', auth_type: 'api_key', name: 'Work OpenAI', status: 'active', model_prefix: 'work/' }] }))` (shape per `VendorAccount`), and pass `vendorAccountsEnabled`. Add a test (looping `de`/`en`) that, with the flag on, opening the create form shows the "all" checkbox and the account `Work OpenAI`; checking the account then toggling its override and typing a prefix, submitting, calls `createToken` with `vendor_access.accounts[0] == { account_id: 'acc_a', prefix_override: { enabled: true, value: 'x/' } }`; and with the flag off the section is absent.

- [ ] **Step 5: Run to verify it fails**

Run: `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx`
Expected: FAIL — no section rendered.

- [ ] **Step 6: Implement the form section (TokenList.tsx)**

- State: `const [vaAll, setVaAll] = useState(false)` and `const [vaAccounts, setVaAccounts] = useState<VendorAccessEntryState[]>([])` where `VendorAccessEntryState = { accountId: string; overrideEnabled: boolean; prefix: string }`; plus `const [vendorAccounts, setVendorAccounts] = useState<VendorAccount[]>([])`.
- Lazy-load: a `useEffect` on form open (`mode !== 'list'`) gated by `vendorAccountsEnabled`, calling `api.vendorAccounts().then(r => setVendorAccounts(r.data)).catch(() => {})`, with a latest-wins `reqId` ref (copy the `myProjects` effect at `TokenList.tsx:163-174`).
- Reset in `openCreate` (all false, empty list); hydrate in `openEdit` from `row.vendor_access` (All → `vaAll=true`; else map entries to state, `overrideEnabled = !!e.prefix_override`, `prefix = e.prefix_override?.value ?? ''`).
- JSX: insert a grid-child section (gated by `vendorAccountsEnabled`) between the `ModelOverrideEditor`/unknown-redirect `Box` and the project `SearchableSelect`. Render: a Checkbox bound to `vaAll` (label `t.tokenVendorAccessAll`); when `!vaAll`, a `CheckboxGroup` over `vendorAccounts` (value = `acc.id`, label = `acc.name`, `selected` = the checked account ids, `onToggle` toggles an entry in `vaAccounts`); for each checked account, a "Prefix überschreiben" Checkbox (`overrideEnabled`) and, when enabled, a `Field` for `prefix` (empty hint `t.tokenVendorAccessPrefixEmptyHint`). Show `t.tokenVendorAccessNone` when the list is empty.
- Submit: a helper `buildVendorAccess(): VendorAccessDTO | undefined` → `vaAll ? { all: true } : vaAccounts.length ? { all: false, accounts: vaAccounts.map(e => ({ account_id: e.accountId, ...(e.overrideEnabled ? { prefix_override: { enabled: true, value: e.prefix } } : {}) })) } : { all: false, accounts: [] }`. Include `vendor_access: buildVendorAccess()` in `submitCreate`'s object literal and assign `body.vendor_access = buildVendorAccess()` in `submitEdit`.

- [ ] **Step 7: Run to verify it passes + full frontend gate**

Run: `cd gateway/frontend && npx vitest run src/components/TokenList.test.tsx src/i18n.test.ts && npx tsc --noEmit && npm run lint && npm run format:check && npm run build`
Expected: PASS / clean.

- [ ] **Step 8: Commit**

```bash
git add gateway/frontend/src/api/tokens.ts gateway/frontend/src/components/TokenList.tsx gateway/frontend/src/components/views.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/components/TokenList.test.tsx gateway/frontend/src/i18n.test.ts
git commit -m "feat: Token editor — vendor-account access + prefix override

Flag-gated section in TokenList: an 'all providers' switch, else a
per-account opt-in list with an optional prefix override (empty = no
prefix). Lazy-loads the owner's vendor accounts; round-trips
vendor_access on create/edit; de/en strings + error-code mapping."
```

---

## Task 6: Documentation

**Files:**
- Modify: `docs/architecture/cross-cutting/external-vendor-accounts.md`
- Modify: `docs/architecture/.../api-surface.md` and `openapi.yaml` (token DTO field + two error codes)
- Create: `docs/architecture/decisions/ADR-0050-per-token-vendor-access.md` (confirm the number/format from existing ADRs)
- Modify: the token data-model doc where `api_tokens` columns are listed

**Interfaces:** none (docs).

- [ ] **Step 1: Document the feature**
  - external-vendor-accounts.md: a new section — per-token vendor access (`all` + explicit accounts), the prefix override (empty = bare names), the `TokenVendorPrefix` parity contract (listing == routing), strict opt-in default, and the save-time collision rejection + resolver backstop.
  - api-surface.md + openapi.yaml: add `vendor_access` to the token DTO + create/update request schema; add the `portal.token_vendor_access_invalid` / `portal.token_vendor_access_conflict` error rows.
  - ADR: record the decision (per-token provider access, JSON column, enforce in both listing and routing, b1 collision policy, strict migration).
  - data-model doc: add the `vendor_provider_access` column (migration 85).

- [ ] **Step 2: Verify docs**

Run: `make lint-docs` (and `grep` for stale "every token sees all vendor accounts"-type phrasing to reword). Confirm `openapi.yaml` parses.

- [ ] **Step 3: Commit**

```bash
git add docs/
git commit -m "docs: Document per-token vendor access + prefix override

external-vendor-accounts.md section, api-surface + openapi token DTO
field and error codes, ADR for the decision, and the new api_tokens
column in the data-model doc."
```

---

## Verification (before PR)

- Backend: `cd gateway/backend && go build ./... && go vet ./... && go test ./...` + `golangci-lint run` (and `golangci-lint fmt --diff`), **with the Postgres DSN provisioned** so the store-conformance postgres subtests run.
- Frontend: `cd gateway/frontend && npm ci && npx vitest run --coverage --retry=2 && npm run build && npm run lint && npm run format:check`.
- Docs: `make lint-docs`.
- **SonarQube gate**: `make sonar-up`, scan, `make sonar-findings && make sonar-branch-findings` → 0 branch findings; note the gate in the PR.
- Remove branch-local files (`docs/superpowers/`, `docs/implementation-status.md` if any, coverage artifacts, `.superpowers/sdd`), push over SSH, open the PR (`Closes <issue>`), bind + get_status, `make sonar-down`.

## Self-review notes (coverage check)

- Spec §5 data model → Task 1a (codec) + 1b (column/record/auth). §6.1 persistence → 1b. §6.2 listing → Task 3. §6.3 routing + reverse map → Task 4. §6.4 validation + collision → Task 2. §6.5 DTO/API → Task 2. §7 frontend → Task 5. §8 docs → Task 6. §9 testing → each task's tests + the Verification block. Decisions 1–5 (per-account, strict migration, all-switch, JSON column, b1) all realized.
- Type names are consistent across tasks: `auth.VendorAccess`/`VendorAccessEntry`, `routing.TokenVendorPrefix`, `store.Decode/EncodeVendorAccess`, `VendorAccessDTO`/`VendorAccessEntryDTO`/`PrefixOverrideDTO`, `ErrTokenVendorAccess{Invalid,Conflict}`, frontend `VendorAccessDTO` + `vendor_access`.

# OpenAI-compatible Vendor Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user connect hosted OpenAI-compatible AI providers (x.ai/Grok, OpenRouter, Kilo, Google Gemini via its OpenAI shim) plus a generic "Custom (OpenAI-compatible)" option as per-user vendor accounts, routed through the existing `OpenAICompatibleClient`.

**Architecture:** A data-driven provider-preset registry in package `routing` + a new immutable `BaseURL` (root) column on `routing.VendorAccount` replace the resolver's hardcoded per-vendor endpoint switch. A default-`/v1` `OpenAIPathPrefix` field on `routing.Target` (normalised by one shared method) lets the client compose `{Endpoint}{prefix}/chat/completions` and `{prefix}/models`, so Kilo's `/gateway` and the Gemini shim's `/v1beta/openai` fit with no bespoke client. All new providers reuse `ProviderVendorOpenAI` (no `main.go` change), are api-key/Bearer only, and serve `[openai, anthropic]` inbound flavors translate-only.

**Tech Stack:** Go (gateway/backend), React + TypeScript + vitest (gateway/frontend), SQLite + Postgres stores, SonarQube + golangci-lint + prettier gates.

**Spec:** `../specs/2026-10-10-openai-compatible-vendors-design.md` (approved; §11 = Variante 1, Gemini in A). (Referenced relatively on purpose — `scripts/check-docs.sh` check 6 forbids the literal branch-local directory string in any non-allowlisted file.)

## Global Constraints

- Repo-facing text (code comments, commits, PR, docs, openapi) in **English**; no attribution lines in commits/PR; substantive commit bodies (squash description is pre-filled from the body). Push over SSH.
- New vendor id string values are stable wire constants, in this canonical order: `xai`, `openrouter`, `kilo`, `google`, `openai_compatible`. Never rename once stored.
- `BaseURL` is stored as a **root without `/v1`**; the client appends `{prefix}/chat/completions`. It is **immutable after create** (set on create, never in `UpdateVendorAccountRequest`).
- `OpenAIPathPrefix` empty ⇒ `/v1` (every existing caller — self-hosted vLLM/ollama-openai, probe targets, api.openai.com vendor targets — stays byte-identical). Non-empty is a path with a leading `/` and no trailing `/`.
- "Prefix" = the path segment before `/chat/completions` and `/models`, not strictly a version: `/v1` (x.ai, OpenRouter, Custom), `/gateway` with root `https://api.kilo.ai/api` (Kilo), `/v1beta/openai` (Gemini).
- These providers are **api-key (Bearer) only** — reject `auth_type=subscription` for them (reuse `vendor_account.auth_type_invalid`, no new code).
- They serve `APIFlavors = [openai, anthropic]` **translate-only** — both endpoint modes stay zero; no Responses/Messages passthrough.
- No new system setting / env var — they reuse the existing `vendor_accounts_enabled` flag, per-token `vendor_access`, and `ModelPrefix`.
- Credential-validation classification is the existing **uniform** rule (`vendorauth.classify`): 2xx = valid, 401 = invalid, everything else (incl. 400/403/404/429/5xx/redirect) = unverifiable. Do **not** fork it per preset; some providers answer a bad key with 400 → shown fail-soft as "could not verify".
- Registry lives in **`internal/routing`** (routing cannot import portal); `vendorauth` stays `capture`-only and receives plain string URLs the portal composes. **Zero** archtest allowlist edits (archtest doc count 63 edges / 26 packages unchanged).
- Store change ⇒ the **Postgres leg** runs (provision the DSN like CI); the migration is additive (`addColumnIfMissing`, `text not null default ''`), does not touch `baselineCreateStatements` (frozen at v60).
- Gates before PR: `golangci-lint fmt --diff` + `golangci-lint run` + `go test -timeout=25m ./...` (gateway/backend, with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set) and the same in server-agent (untouched here); `sh scripts/check-docs.test.sh && ./scripts/check-docs.sh`; frontend `npm ci && npm run format:check && npm run lint && npm run build && npm test`; `make sonar-gate` + `make sonar-findings` + `make sonar-branch-findings` = 0 branch findings. Frontend `node_modules` is absent in this worktree → `npm ci` first. The Sonar coverage leg flakes on `VendorAccountsView.test.tsx` → regenerate lcov with `npx vitest run --coverage --retry=2`.
- Never write the literal branch-local working-docs directory string (the one `check-docs.sh` check 6 forbids) in any committed file. Remove the branch-local working docs (this plan + the design spec) as the LAST step before the PR (AGENTS.md step 10 names the exact directory); verify `git diff --name-only main...HEAD` shows neither.

## Review Focus

- **Existing OpenAI-compatible targets must not regress.** Self-hosted vLLM/ollama-openai + existing vendor-OpenAI flow through the same client; with `OpenAIPathPrefix == ""` the composed chat + discovery URLs must be byte-identical. (Task 3 — table test over {default, xai, openrouter, kilo, google} asserting the exact composed URL, plus the five reflective Target-completeness guards.)
- **Bad / missing base URL for Custom.** `openai_compatible` with empty/non-https/malformed `BaseURL` must be rejected at create with a clear code, not fail opaquely at route time; the resolver also fails closed on an empty `BaseURL`. (Tasks 4 + 5.)
- **Bad API key at Test connection.** A wrong key maps to the uniform classification; OpenRouter validates against `/v1/key` (its `/v1/models` is public), Kilo has no offline validation (validate-kind `none` → "could not verify"). (Task 6.)
- **Aggregator-sized catalogs.** OpenRouter (~458) / Kilo (~390) exceed the old 500 cap's headroom; discovery + `relabelVendorModels` + per-token `vendor_access` must not truncate (raise cap to 1000; keep the dropped-counter log). (Task 7.)
- **Inbound Anthropic → OpenAI-compatible translate.** An inbound `anthropic_messages` (and `openai_responses`) request to one of these accounts must translate, never passthrough (`MessagesMode`/`ResponsesMode` stay zero). (Task 4 — resolver test over all flavors.)

---

## Task 1: Routing vendor ids + provider-preset registry

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (add the five vendor id constants to the vendor const block at :211-221; update the `VendorAccount.Vendor` comment at :230)
- Create: `gateway/backend/internal/routing/vendor_presets.go`
- Test: `gateway/backend/internal/routing/vendor_presets_test.go`

**Interfaces:**
- Produces: `routing.VendorXAI/VendorOpenRouter/VendorKilo/VendorGoogle/VendorOpenAICompatible` string consts; `routing.IsOpenAICompatibleVendor(vendor string) bool`; `routing.VendorPreset` struct; `routing.VendorPresetFor(vendor string) (VendorPreset, bool)`; `routing.OpenAIPathPrefixFor(vendor string) string` (used by the resolver, Task 4); fields the portal (Tasks 5-7) reads: `DefaultBaseURL`, `PathPrefix`, `ValidateVia`, `ValidatePath`, `StripModelsPrefix`, `UsageVia`.

- [ ] **Step 1: Write the failing registry test**

Create `gateway/backend/internal/routing/vendor_presets_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

import "testing"

func TestIsOpenAICompatibleVendor(t *testing.T) {
	compat := []string{VendorXAI, VendorOpenRouter, VendorKilo, VendorGoogle, VendorOpenAICompatible}
	for _, v := range compat {
		if !IsOpenAICompatibleVendor(v) {
			t.Errorf("IsOpenAICompatibleVendor(%q) = false, want true", v)
		}
	}
	for _, v := range []string{VendorOpenAI, VendorAnthropic, "", "unknown"} {
		if IsOpenAICompatibleVendor(v) {
			t.Errorf("IsOpenAICompatibleVendor(%q) = true, want false", v)
		}
	}
}

func TestVendorPresetForComposesURLs(t *testing.T) {
	cases := []struct {
		vendor, wantChat, wantModels string
	}{
		{VendorXAI, "https://api.x.ai/v1/chat/completions", "https://api.x.ai/v1/models"},
		{VendorOpenRouter, "https://openrouter.ai/api/v1/chat/completions", "https://openrouter.ai/api/v1/models"},
		{VendorKilo, "https://api.kilo.ai/api/gateway/chat/completions", "https://api.kilo.ai/api/gateway/models"},
		{VendorGoogle, "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions", "https://generativelanguage.googleapis.com/v1beta/openai/models"},
	}
	for _, c := range cases {
		p, ok := VendorPresetFor(c.vendor)
		if !ok {
			t.Fatalf("VendorPresetFor(%q) ok=false", c.vendor)
		}
		gotChat := p.DefaultBaseURL + OpenAIPathPrefixFor(c.vendor) + "/chat/completions"
		if gotChat != c.wantChat {
			t.Errorf("%s chat = %q, want %q", c.vendor, gotChat, c.wantChat)
		}
		gotModels := p.DefaultBaseURL + OpenAIPathPrefixFor(c.vendor) + "/models"
		if gotModels != c.wantModels {
			t.Errorf("%s models = %q, want %q", c.vendor, gotModels, c.wantModels)
		}
	}
}

func TestVendorPresetCustomHasNoDefaultBaseURL(t *testing.T) {
	p, ok := VendorPresetFor(VendorOpenAICompatible)
	if !ok {
		t.Fatal("custom preset missing")
	}
	if p.DefaultBaseURL != "" {
		t.Errorf("custom DefaultBaseURL = %q, want empty", p.DefaultBaseURL)
	}
	if OpenAIPathPrefixFor(VendorOpenAICompatible) != "/v1" {
		t.Errorf("custom prefix = %q, want /v1", OpenAIPathPrefixFor(VendorOpenAICompatible))
	}
}

func TestOpenAIPathPrefixForBespokeVendorsIsEmpty(t *testing.T) {
	for _, v := range []string{VendorOpenAI, VendorAnthropic, "unknown"} {
		if got := OpenAIPathPrefixFor(v); got != "" {
			t.Errorf("OpenAIPathPrefixFor(%q) = %q, want empty (⇒ /v1 default)", v, got)
		}
	}
}

func TestVendorPresetValidateVia(t *testing.T) {
	cases := map[string]string{
		VendorXAI:              "models",
		VendorGoogle:           "models",
		VendorOpenAICompatible: "models",
		VendorOpenRouter:       "key",
		VendorKilo:             "none",
	}
	for vendor, want := range cases {
		p, _ := VendorPresetFor(vendor)
		if p.ValidateVia != want {
			t.Errorf("%s ValidateVia = %q, want %q", vendor, p.ValidateVia, want)
		}
	}
	if p, _ := VendorPresetFor(VendorOpenRouter); p.ValidatePath != "/key" {
		t.Errorf("openrouter ValidatePath = %q, want /key", p.ValidatePath)
	}
	if p, _ := VendorPresetFor(VendorGoogle); p.StripModelsPrefix != "models/" {
		t.Errorf("google StripModelsPrefix = %q, want models/", p.StripModelsPrefix)
	}
}
```

- [ ] **Step 2: Run the test to confirm it fails to compile**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'VendorPreset|OpenAICompatible|OpenAIPathPrefixFor' -count=1`
Expected: FAIL — undefined `VendorXAI`, `IsOpenAICompatibleVendor`, `VendorPresetFor`, etc.

- [ ] **Step 3: Add the vendor id constants**

In `gateway/backend/internal/routing/store.go`, extend the vendor const block (currently at :211-221). After `VendorAnthropic = "anthropic"` (:213) add:

```go
	// OpenAI-compatible hosted providers (api-key only; reached through the
	// shared OpenAICompatibleClient against the account's own BaseURL). Their
	// endpoints and path prefixes are data in vendor_presets.go.
	VendorXAI              = "xai"
	VendorOpenRouter       = "openrouter"
	VendorKilo             = "kilo"
	VendorGoogle           = "google"
	VendorOpenAICompatible = "openai_compatible"
```

Update the `VendorAccount.Vendor` field comment (:230) from `// VendorOpenAI | VendorAnthropic` to `// VendorOpenAI | VendorAnthropic | an OpenAI-compatible id (see vendor_presets.go)`.

- [ ] **Step 4: Create the preset registry**

Create `gateway/backend/internal/routing/vendor_presets.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package routing

// VendorPreset is the fixed, per-vendor data for an OpenAI-compatible hosted
// provider. It lives in package routing because the resolver needs the path
// prefix (routing cannot import portal); the portal reads the rest to validate
// a key, discover models and (sub-project D) pull usage. vendorauth never sees
// it — the portal composes plain URL strings from these fields.
type VendorPreset struct {
	// DefaultBaseURL is the upstream ROOT (no /v1 or other path prefix); "" for
	// VendorOpenAICompatible, where the user must supply BaseURL.
	DefaultBaseURL string
	// PathPrefix is the segment the client puts between BaseURL and
	// "/chat/completions" (and "/models"): "/v1", Kilo's "/gateway", Gemini's
	// "/v1beta/openai". It is a path with a leading "/" and no trailing "/".
	PathPrefix string
	// ValidateVia selects how the portal checks an api key at Test connection:
	//   "models" — GET {BaseURL}{PathPrefix}/models (listing needs a key)
	//   "key"    — GET {BaseURL}{PathPrefix}{ValidatePath} (a key-introspection endpoint)
	//   "none"   — no offline validation (the models listing is public and there
	//              is no key endpoint); Test connection reports "could not verify".
	ValidateVia string
	// ValidatePath is the suffix for ValidateVia=="key" (e.g. "/key"); "" otherwise.
	ValidatePath string
	// StripModelsPrefix, when non-empty, is stripped from the front of each
	// discovered model id (Gemini's "models/"); "" = keep ids verbatim.
	StripModelsPrefix string
	// UsageVia is reserved for sub-project D (per-provider usage/credits). Empty
	// in sub-project A: no provider reports usage yet.
	UsageVia string
}

// vendorPresets is the registry of OpenAI-compatible providers. The keys are the
// VendorOpenAICompatible-family ids; openai/anthropic are NOT here (they keep
// their bespoke resolver branch). Roots/prefixes are confirmed live (2026-10-10):
// see ADR-052 and external-vendor-accounts.md §10.
var vendorPresets = map[string]VendorPreset{
	VendorXAI: {
		DefaultBaseURL: "https://api.x.ai",
		PathPrefix:     "/v1",
		ValidateVia:    "models", // GET /v1/models needs a key (bad key -> 400 -> unverifiable)
	},
	VendorOpenRouter: {
		DefaultBaseURL: "https://openrouter.ai/api",
		PathPrefix:     "/v1",
		ValidateVia:    "key", // /v1/models is public; validate against /v1/key
		ValidatePath:   "/key",
	},
	VendorKilo: {
		DefaultBaseURL: "https://api.kilo.ai/api",
		PathPrefix:     "/gateway",
		ValidateVia:    "none", // /gateway/models is public and there is no key endpoint
	},
	VendorGoogle: {
		DefaultBaseURL:    "https://generativelanguage.googleapis.com",
		PathPrefix:        "/v1beta/openai",
		ValidateVia:       "models", // GET .../models needs a key (bad key -> 400 -> unverifiable)
		StripModelsPrefix: "models/",
	},
	VendorOpenAICompatible: {
		DefaultBaseURL: "", // user supplies the root
		PathPrefix:     "/v1",
		ValidateVia:    "models",
	},
}

// IsOpenAICompatibleVendor reports whether vendor is one of the OpenAI-compatible
// hosted providers (routed through OpenAICompatibleClient against its own BaseURL).
func IsOpenAICompatibleVendor(vendor string) bool {
	_, ok := vendorPresets[vendor]
	return ok
}

// VendorPresetFor returns the preset for an OpenAI-compatible vendor.
func VendorPresetFor(vendor string) (VendorPreset, bool) {
	p, ok := vendorPresets[vendor]
	return p, ok
}

// OpenAIPathPrefixFor is the Target.OpenAIPathPrefix value for an OpenAI-compatible
// vendor; "" for every other vendor (so the Target keeps the default-/v1 behaviour).
func OpenAIPathPrefixFor(vendor string) string {
	if p, ok := vendorPresets[vendor]; ok {
		return p.PathPrefix
	}
	return ""
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'VendorPreset|OpenAICompatible|OpenAIPathPrefixFor' -count=1`
Expected: PASS.

- [ ] **Step 6: Format and commit**

Run: `cd gateway/backend && golangci-lint fmt ./internal/routing/... && golangci-lint run ./internal/routing/...`

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/routing/vendor_presets.go gateway/backend/internal/routing/vendor_presets_test.go
git commit -m "feat(routing): vendor ids + preset registry for OpenAI-compatible providers

Add the xai/openrouter/kilo/google/openai_compatible vendor ids and a
pure-data preset registry (default root, path prefix, validate strategy,
model-id strip, reserved usage slot) in package routing, plus
IsOpenAICompatibleVendor / VendorPresetFor / OpenAIPathPrefixFor. The
registry lives in routing because the resolver needs the path prefix and
routing cannot import portal; the portal composes URL strings from it."
```

---

## Task 2: `BaseURL` column on `VendorAccount` (migration 86 + store round-trip)

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (add `BaseURL string` to `VendorAccount` after `ModelPrefix`, :243)
- Modify: `gateway/backend/internal/store/migrate.go` (migration 86)
- Modify: `gateway/backend/internal/store/sqlite_vendor_accounts.go` (columns const, insert, scan — **not** the update set; BaseURL is immutable)
- Modify: `gateway/backend/internal/routing/memory_store.go` (no change to `UpdateVendorAccount`; `CreateVendorAccount` copies the whole struct — confirm)
- Modify: `gateway/backend/internal/store/vendor_account_column_parity_test.go` (seed BaseURL + add to `seeded` list)
- Modify: `gateway/backend/internal/store/vendor_account_conformance_test.go` (BaseURL in the CRUD literal)
- Create: `gateway/backend/internal/store/migration86_vendor_account_base_url_test.go`

**Interfaces:**
- Produces: `routing.VendorAccount.BaseURL string` (immutable; set on create, round-trips through every reader on sqlite + postgres + memory).

- [ ] **Step 1: Write the failing migration test**

Create `gateway/backend/internal/store/migration86_vendor_account_base_url_test.go`, mirroring `migration84_vendor_spend_control_test.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package store

import (
	"context"
	"strings"
	"testing"

	"op-ai-gateway/internal/routing"
)

func TestMigration86AddsVendorAccountBaseURL(t *testing.T) {
	forEachDialectMigratedTo(t, 85, func(t *testing.T, s *SQLStore) {
		ctx := context.Background()
		if cols := tableColumns(ctx, t, s, "vendor_accounts"); cols["base_url"] {
			t.Fatal("base_url present before migration 86")
		}
		// Seed a pre-upgrade row without base_url.
		if _, err := s.exec(ctx, s.dl.rebind(`insert into vendor_accounts
			(id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, created_at, updated_at)
			values (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			"va_pre", "usr_1", routing.VendorOpenAI, routing.VendorAuthAPIKey, "Pre", routing.VendorAccountStatusActive,
			"enc:k", "", "", nowUTC(), nowUTC()); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		decls := tableColumnDecls(ctx, t, s, "vendor_accounts")
		if d := strings.ToLower(decls["base_url"]); !strings.Contains(d, "not null") {
			t.Errorf("base_url decl = %q, want not null default ''", decls["base_url"])
		}
		acc, err := s.VendorAccountByID(ctx, "va_pre")
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if acc.BaseURL != "" {
			t.Errorf("pre-upgrade BaseURL = %q, want empty", acc.BaseURL)
		}
		// Idempotent replay.
		if err := s.Migrate(ctx); err != nil {
			t.Fatalf("re-migrate: %v", err)
		}
	})
}
```

(If a `nowUTC()` test helper does not exist in the package, use `time.Now().UTC()` with a `time` import; confirm the exact helper name used by `migration84_*_test.go` and match it.)

- [ ] **Step 2: Run to confirm it fails**

Run: `cd gateway/backend && go test ./internal/store/ -run TestMigration86 -count=1`
Expected: FAIL — `acc.BaseURL` undefined / migration 86 absent.

- [ ] **Step 3: Add the struct field**

In `gateway/backend/internal/routing/store.go`, insert into `VendorAccount` (after `ModelPrefix string` at :243, before `CreatedAt`):

```go
	// BaseURL is the upstream ROOT url of an OpenAI-compatible account (no /v1 or
	// other path prefix — the client adds the preset's prefix). "" for the bespoke
	// openai/anthropic accounts. Set at create and immutable thereafter.
	BaseURL string
```

Run `golangci-lint fmt ./internal/routing/...` to settle gofumpt alignment.

- [ ] **Step 4: Add migration 86**

In `gateway/backend/internal/store/migrate.go`, after the version-85 entry (`{version: 85, name: "api_token_vendor_provider_access", up: migration85Up},` at :120) and before the closing `}` (:121):

```go
	{version: 86, name: "vendor_account_base_url", up: migration86Up},
```

Append the up-function next to `migration85Up` (mirror its doc style):

```go
// migration86Up adds vendor_accounts.base_url: the upstream ROOT url of an
// OpenAI-compatible account, stored without the /v1 (or /v1beta/openai) prefix
// the gateway adds from its provider-preset registry; "" for openai/anthropic.
// Schema only, no backfill; baselineCreateStatements stays frozen at v60.
func migration86Up(ctx context.Context, tx *sql.Tx, dl dialect) error {
	return addColumnIfMissing(ctx, tx, dl, "vendor_accounts", "base_url text not null default ''")
}
```

- [ ] **Step 5: Wire the SQL readers/writer (immutable column)**

In `gateway/backend/internal/store/sqlite_vendor_accounts.go`:
- Extend `vendorAccountColumns` (:17) to append `, base_url` after `model_prefix`:
  ```go
  const vendorAccountColumns = `id, owner_user_id, vendor, auth_type, name, status, api_key, oauth_tokens, model_prefix, base_url, created_at, updated_at`
  ```
- `CreateVendorAccount` insert (:22-26): add one `?` (now 12) and `a.BaseURL` in the matching position (after `a.ModelPrefix`, before `a.CreatedAt`).
- `scanVendorAccount` (:274): add `&a.BaseURL` after `&a.ModelPrefix`, before `&a.CreatedAt`.
- **Do NOT** change the `UpdateVendorAccount` set-list (:42-44): `base_url` is immutable, so it is never rewritten.

In `gateway/backend/internal/routing/memory_store.go`: `CreateVendorAccount` stores the whole struct via `copyVendorAccount` (:2326) — no change. **Do NOT** add `cur.BaseURL = a.BaseURL` to `UpdateVendorAccount` (:2794-2798): immutability means the stored value is kept untouched.

- [ ] **Step 6: Run the migration test (sqlite)**

Run: `cd gateway/backend && go test ./internal/store/ -run TestMigration86 -count=1`
Expected: PASS (sqlite; postgres skips silently without the DSN — covered in Step 9).

- [ ] **Step 7: Update the column-parity + conformance tests**

In `gateway/backend/internal/store/vendor_account_column_parity_test.go`:
- In the seed literal (:61-66) add `BaseURL: "https://base-" + idx + ".example.test",` after the `ModelPrefix:` line, so a reader that drops/reorders the column is caught.
- In the `seeded` list (:146-151, `TestVendorAccountsSchemaColumnsAllCovered`) add `"base_url"` in alphabetical position (after `"auth_type"`/`"api_key"`: the list is `api_key, auth_type, base_url, created_at, id, model_prefix, name, oauth_tokens, owner_user_id, status, updated_at, vendor`).
- Update the doc-comment counts (:33-34) from eleven/seven to twelve/eight if it enumerates columns.

In `gateway/backend/internal/store/vendor_account_conformance_test.go` (`TestRoutingStoreVendorAccountCRUD`, :28-86): add `BaseURL: "https://api.x.ai",` to the account literal (:47-51) so the memory+sqlite CRUD round-trip asserts it.

- [ ] **Step 8: Run the store package (sqlite)**

Run: `cd gateway/backend && go test ./internal/store/ ./internal/routing/ -count=1`
Expected: PASS.

- [ ] **Step 9: Run the Postgres leg**

Provision Postgres like CI (Docker is running; use port 55432 to avoid colliding with the user's stacks):

```bash
docker run -d --name op-ai-gw-test-pg -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=op_test -p 55432:5432 postgres:17-alpine
docker exec op-ai-gw-test-pg pg_isready -U postgres -d op_test
```

Then (count the indented `/postgres` PASS lines — postgres subtests skip silently without the DSN):

```bash
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:55432/op_test?sslmode=disable' go test ./internal/store/ -count=1 -v -run 'VendorAccount|Migration86|Migrate' 2>&1 | grep -E '^ +--- (PASS|FAIL|SKIP): .*/postgres'
```

Expected: `/postgres` PASS lines for `TestMigration86…`, `TestVendorAccountsSchemaColumnsAllCovered`, `TestConformanceVendorAccountReadersAgreeOnEveryColumn`. Leave the container running for later tasks; tear down at the end (`docker rm -f op-ai-gw-test-pg`).

- [ ] **Step 10: Commit**

```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/store/migrate.go gateway/backend/internal/store/sqlite_vendor_accounts.go gateway/backend/internal/store/vendor_account_column_parity_test.go gateway/backend/internal/store/vendor_account_conformance_test.go gateway/backend/internal/store/migration86_vendor_account_base_url_test.go
git commit -m "feat(store): add immutable vendor_accounts.base_url (migration 86)

Add VendorAccount.BaseURL (the upstream root of an OpenAI-compatible
account) with migration 86 (addColumnIfMissing, text not null default '').
Wired through the shared SQL column list, insert and scan (sqlite +
postgres) and the memory driver's whole-struct create; deliberately NOT in
the update set, so it is immutable after create. Column-parity, conformance
and a dedicated migration test cover the round-trip on both dialects."
```

---

## Task 3: `OpenAIPathPrefix` on `Target` + prefix-aware client + usage label

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (add `OpenAIPathPrefix string` to `Target` after `Subscription bool` at :151; add method `OpenAIPathPrefixOrDefault()`)
- Modify: `gateway/backend/internal/provider/openai_compatible.go` (chat POST ×2 at :60/:662; discovery `modelDiscoveryFor`/`ListModels` at :189-194/:244-245)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (usage-label fallthrough `upstreamPath` at :192)
- Modify (reflective guards): `gateway/backend/internal/routing/target_completeness_test.go`; `gateway/backend/internal/gateway/benchmark_target_completeness_test.go`; `gateway/backend/internal/routing/resolver_vendor_account_test.go` (two maps); `gateway/backend/internal/routing/resolver_vendor_subscription_test.go` (two maps)
- Test: `gateway/backend/internal/provider/openai_compatible_test.go`; `gateway/backend/internal/gateway/native_passthrough_test.go`

**Interfaces:**
- Produces: `routing.Target.OpenAIPathPrefix string`; `func (t Target) OpenAIPathPrefixOrDefault() string` (trimmed prefix with a leading `/` and no trailing `/`, or `/v1` when blank). Consumed by the provider client (Task 3) and the resolver branch (Task 4).

- [ ] **Step 1: Write the failing provider table test**

In `gateway/backend/internal/provider/openai_compatible_test.go` add:

```go
func TestOpenAICompatibleClientUsesOpenAIPathPrefix(t *testing.T) {
	cases := []struct {
		name, prefix, wantChat, wantModels string
	}{
		{"default empty => /v1", "", "/v1/chat/completions", "/v1/models"},
		{"gemini shim", "/v1beta/openai", "/v1beta/openai/chat/completions", "/v1beta/openai/models"},
		{"kilo gateway", "/gateway", "/gateway/chat/completions", "/gateway/models"},
		{"trailing slash normalised", "/v1/", "/v1/chat/completions", "/v1/models"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var gotChat, gotModels string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/chat/completions"):
					gotChat = r.URL.Path
					w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
				case strings.HasSuffix(r.URL.Path, "/models"):
					gotModels = r.URL.Path
					w.Write([]byte(`{"data":[{"id":"m"}]}`))
				}
			}))
			defer upstream.Close()
			client := NewOpenAICompatibleClient(http.DefaultClient)
			target := routing.Target{Endpoint: upstream.URL, ProviderModel: "m", Timeout: 5 * time.Second, OpenAIPathPrefix: c.prefix}
			_, _ = client.Complete(context.Background(), target, inference.Request{Model: "m"})
			_, _ = client.ListModels(context.Background(), target)
			if gotChat != c.wantChat {
				t.Errorf("chat path = %q, want %q", gotChat, c.wantChat)
			}
			if gotModels != c.wantModels {
				t.Errorf("models path = %q, want %q", gotModels, c.wantModels)
			}
		})
	}
}
```

(Confirm the exact `Complete`/`ListModels`/`Request` signatures against the existing tests at :25-60 and :187-209 and adjust the call/response shapes to match; the point is the asserted paths.)

- [ ] **Step 2: Run to confirm it fails**

Run: `cd gateway/backend && go test ./internal/provider/ -run TestOpenAICompatibleClientUsesOpenAIPathPrefix -count=1`
Expected: FAIL — `OpenAIPathPrefix` undefined on `routing.Target`.

- [ ] **Step 3: Add the field + the shared normaliser method**

In `gateway/backend/internal/routing/resolver.go`, insert into `Target` after `Subscription bool` (:151, before the closing brace):

```go
	// OpenAIPathPrefix is the URL path segment OpenAICompatibleClient puts between
	// Endpoint and the OpenAI-dialect resource ("/chat/completions", "/models"):
	// the composed URLs are {Endpoint}{prefix}/chat/completions and {prefix}/models.
	// EMPTY means "/v1" — the value every self-hosted target, probe target and
	// api.openai.com vendor target has always used — so leaving it unset is
	// byte-identical to the pre-field behaviour. A non-empty value is a path with a
	// leading "/" and no trailing "/" (Kilo "/gateway", Gemini "/v1beta/openai").
	// Only an OpenAI-compatible vendor-account target sets it.
	OpenAIPathPrefix string
```

Add the method (near the `Target` type; `strings` is already imported in resolver.go — confirm, else add it):

```go
// OpenAIPathPrefixOrDefault returns the normalised OpenAI-dialect path prefix:
// the trimmed OpenAIPathPrefix with a leading "/" and no trailing "/", or "/v1"
// when it is blank. It is the one place the "/v1" default lives, shared by the
// provider client and the gateway usage-label path.
func (t Target) OpenAIPathPrefixOrDefault() string {
	prefix := strings.TrimRight(strings.TrimSpace(t.OpenAIPathPrefix), "/")
	if prefix == "" {
		return "/v1"
	}
	if !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return prefix
}
```

- [ ] **Step 4: Make the provider client prefix-aware**

In `gateway/backend/internal/provider/openai_compatible.go`:
- Chat POST site #1 (:60): replace `endpointURL(target.Endpoint, "/v1/chat/completions")` with `endpointURL(target.Endpoint, target.OpenAIPathPrefixOrDefault()+"/chat/completions")`.
- Chat POST site #2 (:662): same replacement.
- `modelDiscoveryFor` (:189-194): add a `pathPrefix string` parameter; keep the sdcpp branch unprefixed, and change the default return to `return pathPrefix + "/models", decodeOpenAIModelList`:
  ```go
  func modelDiscoveryFor(providerType, pathPrefix string) (path string, decode func(io.Reader) ([]string, error)) {
  	if strings.TrimSpace(providerType) == routing.ProviderStableDiffusionCpp {
  		return sdcppModelsPath, decodeSdcppModelList
  	}
  	return pathPrefix + "/models", decodeOpenAIModelList
  }
  ```
- `ListModels` call site (:244): `discoveryPath, decode := modelDiscoveryFor(target.Provider, target.OpenAIPathPrefixOrDefault())`.

- [ ] **Step 5: Make the usage label prefix-aware**

In `gateway/backend/internal/gateway/native_passthrough.go`, `upstreamPath` fallthrough (:192): replace `return "/v1/chat/completions"` with `return target.OpenAIPathPrefixOrDefault() + "/chat/completions"`. (The ollama / vendor_anthropic / vendor_openai_subscription cases above are unchanged.)

- [ ] **Step 6: Run the provider + gateway tests**

Run: `cd gateway/backend && go test ./internal/provider/ -run 'TestOpenAICompatibleClientUsesOpenAIPathPrefix|TestOpenAICompatibleClientCompletesChat|TestOpenAICompatibleClientListModels|TestListModelsKeepsV1ModelsForEveryOtherType|TestListModelsDiscoversStableDiffusion' -count=1`
Expected: the new test PASSES; the existing path-guard tests still PASS (empty prefix ⇒ `/v1`).

- [ ] **Step 7: Add the usage-label regression case + update the five reflective guards**

Add to `gateway/backend/internal/gateway/native_passthrough_test.go` (next to `TestUpstreamPathLabelsVendorProvidersByTheirTranslatePath` at :704-722) a case with `routing.Target{Provider: routing.ProviderVendorOpenAI, OpenAIPathPrefix: "/v1beta/openai"}` expecting `/v1beta/openai/chat/completions`, and keep the existing prefix-less row expecting `/v1/chat/completions`.

Add `OpenAIPathPrefix` to each may-be-zero map (the field is zero wherever the prefix is the default):
- `gateway/backend/internal/routing/target_completeness_test.go` `targetFromMayLeaveZero` (:21-32): `"OpenAIPathPrefix": true, // empty ⇒ /v1; only an OpenAI-compatible vendor-account target sets it`.
- `gateway/backend/internal/gateway/benchmark_target_completeness_test.go` `benchmarkOmits` (:45-53): `"OpenAIPathPrefix": true, // a benchmark measures self-hosted servers on the default /v1 path`.
- `gateway/backend/internal/routing/resolver_vendor_account_test.go`: add the key to BOTH `vendorTargetMayBeZero` (:491-502) and `vendorAnthropicMessagesTargetMayBeZero` (:511-521) — the existing OpenAI and Anthropic vendor targets leave it empty.
- `gateway/backend/internal/routing/resolver_vendor_subscription_test.go`: add the key to BOTH `vendorSubscriptionTargetMayBeZero` and `vendorSubscriptionOpenAITargetMayBeZero` (:~342-364).

Keep gofumpt column alignment in each map (run `golangci-lint fmt`).

- [ ] **Step 8: Run the full affected set**

Run: `cd gateway/backend && golangci-lint fmt ./... && golangci-lint run ./internal/routing/... ./internal/provider/... ./internal/gateway/... && go test ./internal/routing/ ./internal/provider/ ./internal/gateway/ -count=1`
Expected: PASS (all completeness guards satisfied; no regression).

- [ ] **Step 9: Commit**

```bash
git add gateway/backend/internal/routing/resolver.go gateway/backend/internal/provider/openai_compatible.go gateway/backend/internal/gateway/native_passthrough.go gateway/backend/internal/routing/target_completeness_test.go gateway/backend/internal/gateway/benchmark_target_completeness_test.go gateway/backend/internal/routing/resolver_vendor_account_test.go gateway/backend/internal/routing/resolver_vendor_subscription_test.go gateway/backend/internal/provider/openai_compatible_test.go gateway/backend/internal/gateway/native_passthrough_test.go
git commit -m "feat(routing,provider): configurable OpenAI path prefix (default /v1)

Add Target.OpenAIPathPrefix + Target.OpenAIPathPrefixOrDefault() (empty ⇒
/v1) and make OpenAICompatibleClient compose {Endpoint}{prefix}/chat/
completions and {prefix}/models, plus the gateway usage-label path. Empty
is byte-identical for self-hosted vLLM/ollama-openai and api.openai.com
vendor targets; /v1beta/openai and /gateway now compose correctly. Updates
the five reflective Target-completeness guards for the new field."
```

---

## Task 4: Resolver OpenAI-compatible branch

**Files:**
- Modify: `gateway/backend/internal/routing/resolver.go` (`vendorAccountTarget` :950-1012; fail-closed in `vendorAccountModelTarget` :915-918)
- Test: `gateway/backend/internal/routing/resolver_vendor_account_test.go`

**Interfaces:**
- Consumes: `IsOpenAICompatibleVendor`, `OpenAIPathPrefixFor` (Task 1); `Target.OpenAIPathPrefix` (Task 3); `VendorAccount.BaseURL` (Task 2).
- Produces: an OpenAI-compatible vendor `Target` — `Provider = ProviderVendorOpenAI`, `Endpoint = acc.BaseURL`, `OpenAIPathPrefix = OpenAIPathPrefixFor(acc.Vendor)`, Bearer (`APITokenHeader == ""`), `APIFlavors = [openai, anthropic]`, both endpoint modes zero.

- [ ] **Step 1: Write the failing resolver test**

In `gateway/backend/internal/routing/resolver_vendor_account_test.go` add (reusing the `seedVendorAccount`/`vendorResolver`/`ownerToken` helpers at :23-58, but creating the account directly so `BaseURL` is set):

```go
func TestOpenAICompatibleVendorResolvesToTranslateTarget(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	acc := VendorAccount{
		ID: "acc_xai", OwnerUserID: vendorOwner, Vendor: VendorXAI, AuthType: VendorAuthAPIKey,
		Status: VendorAccountStatusActive, APIKey: vendorKey, BaseURL: "https://api.x.ai", CreatedAt: now, UpdatedAt: now,
	}
	if err := store.CreateVendorAccount(context.Background(), acc); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := store.SetVendorAccountModels(context.Background(), acc.ID, []VendorAccountModel{
		{AccountID: acc.ID, GatewayModel: "grok-4", UpstreamModel: "grok-4", APIFlavor: APIFlavorOpenAI},
	}); err != nil {
		t.Fatalf("models: %v", err)
	}
	r := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)

	// Both inbound dialects translate (no passthrough) for every flavor.
	for _, flavor := range []string{"openai_chat", "openai_responses", "anthropic_messages"} {
		target, _, err := r.Resolve(context.Background(), ownerToken(), inference.Request{Model: "grok-4", APIFlavor: flavor})
		if err != nil {
			t.Fatalf("resolve %s: %v", flavor, err)
		}
		if target.Provider != ProviderVendorOpenAI {
			t.Errorf("%s Provider = %q, want %q", flavor, target.Provider, ProviderVendorOpenAI)
		}
		if target.Endpoint != "https://api.x.ai" {
			t.Errorf("%s Endpoint = %q, want https://api.x.ai", flavor, target.Endpoint)
		}
		if target.OpenAIPathPrefix != "/v1" {
			t.Errorf("%s OpenAIPathPrefix = %q, want /v1", flavor, target.OpenAIPathPrefix)
		}
		if target.APITokenHeader != "" {
			t.Errorf("%s APITokenHeader = %q, want empty (Bearer)", flavor, target.APITokenHeader)
		}
		if target.ResponsesMode != "" || target.MessagesMode != "" {
			t.Errorf("%s modes = (%q,%q), want translate (empty,empty)", flavor, target.ResponsesMode, target.MessagesMode)
		}
		if got := strings.Join(target.APIFlavors, ","); got != APIFlavorOpenAI+","+APIFlavorAnthropic {
			t.Errorf("%s APIFlavors = %q, want openai,anthropic", flavor, got)
		}
		if target.VendorAccountID != acc.ID {
			t.Errorf("%s VendorAccountID = %q, want %q", flavor, target.VendorAccountID, acc.ID)
		}
	}
}

func TestOpenAICompatibleVendorEmptyBaseURLFailsClosed(t *testing.T) {
	now := time.Now().UTC()
	store := NewMemoryStore()
	acc := VendorAccount{
		ID: "acc_custom", OwnerUserID: vendorOwner, Vendor: VendorOpenAICompatible, AuthType: VendorAuthAPIKey,
		Status: VendorAccountStatusActive, APIKey: vendorKey, BaseURL: "", CreatedAt: now, UpdatedAt: now,
	}
	_ = store.CreateVendorAccount(context.Background(), acc)
	_ = store.SetVendorAccountModels(context.Background(), acc.ID, []VendorAccountModel{
		{AccountID: acc.ID, GatewayModel: "m", UpstreamModel: "m", APIFlavor: APIFlavorOpenAI},
	})
	r := vendorResolver(store, now, true, vendorRoutingModeVendorFirst)
	_, _, err := r.Resolve(context.Background(), ownerToken(), inference.Request{Model: "m", APIFlavor: "openai_chat"})
	if err == nil {
		t.Fatal("resolve with empty BaseURL succeeded, want no-route error")
	}
}
```

Also add `OpenAIPathPrefix` to a new third may-be-zero map used by a compat case in `TestVendorAccountTargetCompleteness` if you extend that test; for the compat shape `Endpoint` and `OpenAIPathPrefix` must be **non-zero**, while `ResponsesMode`, `MessagesMode`, `APITokenHeader`, `ExtraHeaders`, `Masquerade`, `Subscription`, `ServerID`, `OpportunisticMetrics`, `ResponsesLiveTimingsEnabled`, `LiveProgressSupport`, `LiveProgressSpecType` are allowed zero.

- [ ] **Step 2: Run to confirm it fails**

Run: `cd gateway/backend && go test ./internal/routing/ -run 'TestOpenAICompatibleVendor' -count=1`
Expected: FAIL — the branch does not exist; Endpoint is `https://api.openai.com`, prefix empty.

- [ ] **Step 3: Add the branch**

In `vendorAccountTarget` (resolver.go :951-960), extend the default block and add a `pathPrefix`:

```go
	provider := ProviderVendorOpenAI
	endpoint := "https://api.openai.com"
	tokenHeader := ""
	pathPrefix := "" // empty ⇒ /v1 (Target.OpenAIPathPrefix)
	if acc.Vendor == VendorAnthropic {
		provider = ProviderVendorAnthropic
		endpoint = "https://api.anthropic.com"
		// The native Anthropic client authenticates with x-api-key, not the
		// Authorization: Bearer default the OpenAI-compatible client uses.
		tokenHeader = "x-api-key"
	} else if IsOpenAICompatibleVendor(acc.Vendor) {
		// x.ai / OpenRouter / Kilo / Google Gemini shim / custom: the shared
		// OpenAI-compatible client against the account's own root URL, Bearer
		// auth, translate-only (both endpoint modes stay zero below).
		endpoint = acc.BaseURL
		pathPrefix = OpenAIPathPrefixFor(acc.Vendor)
	}
```

In the `Target` literal (after `APITokenHeader: tokenHeader,` at :971) add:

```go
		OpenAIPathPrefix: pathPrefix,
```

The two passthrough branches (:991-993, :1008-1010) are keyed on `VendorOpenAI`/`VendorAnthropic`, so the compat vendors never enter them — no change there.

- [ ] **Step 4: Fail closed on an empty BaseURL**

In `vendorAccountModelTarget` (resolver.go :915-918), before returning the api-key target, reject a compat account with no BaseURL:

```go
func vendorAccountModelTarget(acc VendorAccount, m VendorAccountModel, req inference.Request, apiFlavor string) (Target, bool) {
	if acc.AuthType != VendorAuthSubscription {
		if IsOpenAICompatibleVendor(acc.Vendor) && strings.TrimSpace(acc.BaseURL) == "" {
			// A misconfigured account (no root url) must not route to a broken
			// "/v1/chat/completions" with no host. Create-time validation
			// normally prevents this; fail closed so the request falls through.
			return Target{}, false
		}
		return vendorAccountTarget(acc, m, req.Model, apiFlavor, req.APIFlavor), true
	}
	// ... existing subscription switch unchanged ...
```

- [ ] **Step 5: Run the resolver tests**

Run: `cd gateway/backend && go test ./internal/routing/ -count=1`
Expected: PASS (new tests + all existing resolver/vendor tests).

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/routing/resolver.go gateway/backend/internal/routing/resolver_vendor_account_test.go
git commit -m "feat(routing): route OpenAI-compatible vendor accounts

vendorAccountTarget gains a branch for the OpenAI-compatible vendors:
Provider stays vendor_openai, Endpoint = the account's stored root,
OpenAIPathPrefix from the preset, Bearer auth, [openai, anthropic]
translate-only (no Responses/Messages passthrough). vendorAccountModelTarget
fails closed on an empty BaseURL so a misconfigured account cannot route to
a hostless URL."
```

---

## Task 5: Portal create/update accepts the new vendors (+ base_url, errors, DTO, catalog, fail-closed connect)

**Files:**
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go` (`normalizeVendorAccountVendor` :195-202; `CreateVendorAccount` :352-429; DTO structs :90-166; a `normalizeVendorAccountBaseURL` validator)
- Modify: `gateway/backend/internal/portal/service.go` (new error sentinels)
- Modify: `gateway/backend/internal/gateway/portal_vendor_account_endpoints.go` (`portalVendorAccountErrRows` :419+)
- Modify: `gateway/backend/internal/portal/vendor_catalog.go` (`vendorAPIFlavor` :84-92 for the 5 ids; `VendorCatalog` default stays empty — confirm)
- Test: `gateway/backend/internal/portal/service_vendor_accounts_test.go` (or the existing create test file)

**Interfaces:**
- Consumes: `routing.IsOpenAICompatibleVendor`, `routing.VendorPresetFor` (Task 1); `routing.VendorAccount.BaseURL` (Task 2).
- Produces: `CreateVendorAccountRequest.BaseURL string json:"base_url"`; `VendorAccountDTO.BaseURL string json:"base_url"`; error sentinels `ErrVendorAccountBaseURLRequired` (`vendor_account.base_url_required`) and `ErrVendorAccountBaseURLInvalid` (`vendor_account.base_url_invalid`); `vendorAPIFlavor` returns `(APIFlavorOpenAI, true)` for the 5 ids.

- [ ] **Step 1: Write failing service tests**

In the portal create test file add (match the existing test harness/fixtures in `service_vendor_accounts_test.go`):

```go
func TestCreateVendorAccountAcceptsOpenAICompatibleVendors(t *testing.T) {
	// For each preset vendor: create with api_key succeeds; the stored account
	// carries base_url (the typed one, or the preset default when omitted).
	// Assert normalizeVendorAccountVendor accepts the id and the DTO round-trips base_url.
}

func TestCreateVendorAccountRejectsSubscriptionForCompatVendor(t *testing.T) {
	// vendor=xai, auth_type=subscription -> ErrVendorAccountAuthTypeInvalid (reuse existing code).
}

func TestCreateCustomRequiresBaseURL(t *testing.T) {
	// vendor=openai_compatible, base_url="" -> ErrVendorAccountBaseURLRequired.
}

func TestCreateRejectsMalformedOrNonHTTPSBaseURL(t *testing.T) {
	// vendor=openai_compatible, base_url in {"not-a-url","http://x.test","https://u:p@x.test","https://x.test/?a=1"} -> ErrVendorAccountBaseURLInvalid.
}

func TestVendorAPIFlavorKnowsCompatVendors(t *testing.T) {
	for _, v := range []string{routing.VendorXAI, routing.VendorOpenRouter, routing.VendorKilo, routing.VendorGoogle, routing.VendorOpenAICompatible} {
		flavor, ok := vendorAPIFlavor(v)
		if !ok || flavor != routing.APIFlavorOpenAI {
			t.Errorf("vendorAPIFlavor(%q) = (%q,%v), want (openai,true)", v, flavor, ok)
		}
	}
}
```

Fill each body from the existing create-test patterns (the service fixture, the error-assertion helper). Keep exact wording minimal; assert the sentinel errors and the stored `BaseURL`.

- [ ] **Step 2: Run to confirm failure**

Run: `cd gateway/backend && go test ./internal/portal/ -run 'VendorAccountAcceptsOpenAICompatible|RejectsSubscriptionForCompat|CustomRequiresBaseURL|MalformedOrNonHTTPSBaseURL|VendorAPIFlavorKnowsCompat' -count=1`
Expected: FAIL.

- [ ] **Step 3: Accept the new vendor ids**

In `normalizeVendorAccountVendor` (:195-202) extend the accepted set:

```go
	switch vendor := strings.TrimSpace(raw); vendor {
	case routing.VendorOpenAI, routing.VendorAnthropic,
		routing.VendorXAI, routing.VendorOpenRouter, routing.VendorKilo,
		routing.VendorGoogle, routing.VendorOpenAICompatible:
		return vendor, nil
	default:
		return "", ErrVendorAccountVendorInvalid
	}
```

- [ ] **Step 4: Add the base_url DTO fields + error sentinels**

In `service_vendor_accounts.go`:
- `CreateVendorAccountRequest` (:115-122): add `BaseURL string `json:"base_url"`` after `ModelPrefix`.
- `VendorAccountDTO` (:90-103): add `BaseURL string `json:"base_url"`` after `ModelPrefix` (always on the wire, `""` for openai/anthropic).
- `vendorAccountDTO` mapper (:139-166): set `BaseURL: acc.BaseURL`.
- Do **not** add base_url to `UpdateVendorAccountRequest` (immutable).

In `service.go` (next to `ErrVendorAccountModelPrefixInvalid`):

```go
	ErrVendorAccountBaseURLRequired = errors.New("vendor_account.base_url_required")
	ErrVendorAccountBaseURLInvalid  = errors.New("vendor_account.base_url_invalid")
```

- [ ] **Step 5: Validate + default the base URL on create**

Add a validator (net/url only; mirrors the frontend `isValidBaseUrl`):

```go
// normalizeVendorAccountBaseURL validates and defaults an OpenAI-compatible
// account's root url. For a preset vendor an empty value means "use the preset
// default root". For VendorOpenAICompatible (Custom) a value is required. A
// non-empty value must be an https URL with a host and no userinfo/query/fragment
// (the key rides on every request, so plaintext/credential-in-URL is refused;
// the host is otherwise unrestricted — this is an on-prem gateway, see ADR-052).
func normalizeVendorAccountBaseURL(vendor, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		preset, ok := routing.VendorPresetFor(vendor)
		if !ok || preset.DefaultBaseURL == "" {
			return "", ErrVendorAccountBaseURLRequired
		}
		return preset.DefaultBaseURL, nil
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", ErrVendorAccountBaseURLInvalid
	}
	return strings.TrimRight(value, "/"), nil
}
```

In `CreateVendorAccount` (:352-429), in the validation order after the vendor/authType checks:
- If `routing.IsOpenAICompatibleVendor(vendor)`: force api-key only — if `authType != routing.VendorAuthAPIKey` return `ErrVendorAccountAuthTypeInvalid`; then `baseURL, err := normalizeVendorAccountBaseURL(vendor, req.BaseURL)` (return err); set `BaseURL: baseURL` in the built `routing.VendorAccount` (:391-402).
- Else (openai/anthropic): `BaseURL` stays `""` (ignore any supplied `req.BaseURL`).

- [ ] **Step 6: Teach `vendorAPIFlavor` the new ids + confirm catalog stays empty**

In `vendor_catalog.go` `vendorAPIFlavor` (:84-92):

```go
func vendorAPIFlavor(vendor string) (flavor string, ok bool) {
	switch vendor {
	case routing.VendorOpenAI:
		return routing.APIFlavorOpenAI, true
	case routing.VendorAnthropic:
		return routing.APIFlavorAnthropic, true
	}
	if routing.IsOpenAICompatibleVendor(vendor) {
		return routing.APIFlavorOpenAI, true
	}
	return "", false
}
```

`VendorCatalog` (:55-77) keeps its `default: return []routing.VendorAccountModel{}` — the new vendors seed no static models (discovery fills them; Task 7). No edit needed; add a one-line comment that the OpenAI-compatible vendors intentionally seed empty and rely on discovery.

- [ ] **Step 7: Map the new error codes at the HTTP edge**

In `gateway/backend/internal/gateway/portal_vendor_account_endpoints.go` `portalVendorAccountErrRows` (:419+), add rows (mirroring the `ErrVendorAccountModelPrefixInvalid` row):

```go
	{err: portal.ErrVendorAccountBaseURLRequired, status: http.StatusBadRequest, code: "vendor_account.base_url_required", msg: "a base URL is required for a custom OpenAI-compatible account"},
	{err: portal.ErrVendorAccountBaseURLInvalid, status: http.StatusBadRequest, code: "vendor_account.base_url_invalid", msg: "the base URL must be an https URL without credentials, query or fragment"},
```

- [ ] **Step 8: Confirm the subscription-connect paths stay fail-closed**

No code change expected: `service_vendor_connect.go` / `service_vendor_device_connect.go` / `gateway/server.go:2002` switch on the two subscription vendors and already `default` to an unknown-vendor error; the new ids are api-key only. Add/extend a test asserting a connect attempt on a compat vendor fails closed (reuse the existing connect test's unknown-vendor assertion).

- [ ] **Step 9: Run the portal package**

Run: `cd gateway/backend && golangci-lint fmt ./internal/portal/... ./internal/gateway/... && golangci-lint run ./internal/portal/... ./internal/gateway/... && go test ./internal/portal/ ./internal/gateway/ -count=1`
Expected: PASS.

- [ ] **Step 10: Commit**

```bash
git add gateway/backend/internal/portal/service_vendor_accounts.go gateway/backend/internal/portal/service.go gateway/backend/internal/portal/vendor_catalog.go gateway/backend/internal/gateway/portal_vendor_account_endpoints.go gateway/backend/internal/portal/service_vendor_accounts_test.go
git commit -m "feat(portal): create OpenAI-compatible vendor accounts

Accept the five OpenAI-compatible vendor ids at create, api-key only, with
an immutable base_url (validated https, no userinfo/query/fragment; defaulted
from the preset for the named providers, required for Custom). Surface
base_url on the DTO, add base_url_required/base_url_invalid error codes, and
teach vendorAPIFlavor the new ids; the static catalog stays empty (discovery
fills it). Subscription/connect paths remain fail-closed for these vendors."
```

---

## Task 6: Credential validation (Test connection) for the new vendors

**Files:**
- Create: `gateway/backend/internal/vendorauth/openai_compatible.go` (`ValidateOpenAICompatibleAPIKey`)
- Modify: `gateway/backend/internal/portal/service_vendor_validation.go` (new seam type + field + `withDefaults` + the `validateAPIKey` dispatch)
- Test: `gateway/backend/internal/vendorauth/validate_test.go`; `gateway/backend/internal/portal/service_vendor_validation_test.go`

**Interfaces:**
- Consumes: `routing.VendorPresetFor` (Task 1); `routing.VendorAccount.BaseURL` (Task 2); `Target.OpenAIPathPrefixFor`/preset `PathPrefix`.
- Produces: `vendorauth.ValidateOpenAICompatibleAPIKey(ctx, httpClient, modelsURL, apiKey) CredentialCheck`; portal composes the probe URL from the preset and calls it.

- [ ] **Step 1: Write failing vendorauth + portal tests**

In `gateway/backend/internal/vendorauth/validate_test.go` extend `probeCases()` with a case for `ValidateOpenAICompatibleAPIKey` (Authorization `Bearer <cred>` present; `X-Api-Key`/`Anthropic-Version` absent; `forbidden = StatusUnverifiable`; never leaks the credential; no redirect following), passing a `modelsURL` that the test's `rewriteTransport` captures.

In `gateway/backend/internal/portal/service_vendor_validation_test.go` add a fake-validator case (`kindOpenAICompatibleAPIKey`) that asserts the composed URL: for `xai` → `https://api.x.ai/v1/models`; for `openrouter` → `https://openrouter.ai/api/v1/key`; for `kilo` (validate-kind `none`) → `validateAPIKey` returns an unverifiable check without calling any probe.

- [ ] **Step 2: Run to confirm failure**

Run: `cd gateway/backend && go test ./internal/vendorauth/ ./internal/portal/ -run 'OpenAICompatible' -count=1`
Expected: FAIL — function/seam undefined.

- [ ] **Step 3: Add the vendorauth validator**

Create `gateway/backend/internal/vendorauth/openai_compatible.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"context"
	"net/http"
)

// ValidateOpenAICompatibleAPIKey checks an api key against an OpenAI-compatible
// provider's probe URL (GET probeURL, Authorization: Bearer). probeURL is composed
// by the caller (the portal) from the provider-preset registry — this package does
// not know vendor ids or roots. Classification is the shared uniform rule
// (runProbe/classify): 2xx valid, 401 invalid, everything else unverifiable.
func ValidateOpenAICompatibleAPIKey(ctx context.Context, httpClient *http.Client, probeURL, apiKey string) CredentialCheck {
	check, _ := runProbe(ctx, httpClient, probeSpec{
		url:     probeURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
		secret:  apiKey,
	})
	return check
}
```

- [ ] **Step 4: Wire the portal dispatch**

In `service_vendor_validation.go`:
- Add `type VendorOpenAICompatibleValidator func(ctx context.Context, httpClient *http.Client, probeURL, apiKey string) vendorauth.CredentialCheck`.
- Add field `OpenAICompatibleAPIKey VendorOpenAICompatibleValidator` to `VendorCredentialValidators` and default it in `withDefaults` to `vendorauth.ValidateOpenAICompatibleAPIKey`.
- Change `validateAPIKey` to receive the account (so `BaseURL` is available) and branch on the preset. The single caller `checkVendorAccount` (:321) already holds `acc`; pass it:

```go
func (s *Service) validateAPIKey(ctx context.Context, acc routing.VendorAccount, apiKey string) vendorauth.CredentialCheck {
	if routing.IsOpenAICompatibleVendor(acc.Vendor) {
		preset, _ := routing.VendorPresetFor(acc.Vendor)
		switch preset.ValidateVia {
		case "none":
			return unverifiableVendorCheck("this provider offers no offline key check")
		case "key":
			probeURL := strings.TrimRight(acc.BaseURL, "/") + preset.PathPrefix + preset.ValidatePath
			return s.vendorValidation.validators.OpenAICompatibleAPIKey(ctx, s.vendorValidation.client, probeURL, apiKey)
		default: // "models"
			probeURL := strings.TrimRight(acc.BaseURL, "/") + preset.PathPrefix + "/models"
			return s.vendorValidation.validators.OpenAICompatibleAPIKey(ctx, s.vendorValidation.client, probeURL, apiKey)
		}
	}
	var probe VendorCredentialValidator
	switch acc.Vendor {
	case routing.VendorOpenAI:
		probe = s.vendorValidation.validators.OpenAIAPIKey
	case routing.VendorAnthropic:
		probe = s.vendorValidation.validators.AnthropicAPIKey
	}
	if probe == nil {
		return unverifiableVendorCheck("no credential check exists for this vendor")
	}
	return probe(ctx, s.vendorValidation.client, apiKey)
}
```

Update the `checkVendorAccount` call site (:321) to `s.validateAPIKey(ctx, acc, apiKey)`.

- [ ] **Step 5: Run the tests**

Run: `cd gateway/backend && go test ./internal/vendorauth/ ./internal/portal/ -count=1`
Expected: PASS (new + existing, incl. `TestValidatorsNeverLeakCredential`, `TestDiscoverDoesNotFollowRedirects`).

- [ ] **Step 6: Commit**

```bash
git add gateway/backend/internal/vendorauth/openai_compatible.go gateway/backend/internal/vendorauth/validate_test.go gateway/backend/internal/portal/service_vendor_validation.go gateway/backend/internal/portal/service_vendor_validation_test.go
git commit -m "feat(portal,vendorauth): Test-connection for OpenAI-compatible vendors

Add ValidateOpenAICompatibleAPIKey (a Bearer probe against a caller-composed
URL, uniform classification) and route Test connection per preset: GET
{base}{prefix}/models for x.ai/Gemini/Custom, GET {base}{prefix}/key for
OpenRouter (its /models is public), and no offline check for Kilo
(reported as 'could not verify')."
```

---

## Task 7: Model discovery (Refresh + best-effort on create) for the new vendors

**Files:**
- Modify: `gateway/backend/internal/vendorauth/openai_compatible.go` (`DiscoverOpenAICompatibleModels`)
- Modify: `gateway/backend/internal/portal/service_vendor_discovery.go` (new seam + the `discoverAPIKeyModels` arm; `maxDiscoveredModels` → 1000; Gemini `models/` strip; OpenRouter `name` display fallback)
- Modify: `gateway/backend/internal/portal/service_vendor_accounts.go` (`CreateVendorAccount`: best-effort discovery for a compat account)
- Modify: `gateway/backend/internal/vendorauth/discover.go` (optional `name` fallback in `parseDataModels`)
- Test: `gateway/backend/internal/vendorauth/discover_test.go`; `gateway/backend/internal/portal/service_vendor_discovery_test.go`

**Interfaces:**
- Consumes: `routing.VendorPresetFor`, `routing.VendorAccount.BaseURL`.
- Produces: `vendorauth.DiscoverOpenAICompatibleModels(ctx, httpClient, modelsURL, apiKey) ([]DiscoveredModel, DiscoveryStatus)`; a compat account has its models discovered on create (best-effort) and on Refresh.

- [ ] **Step 1: Write failing discovery tests**

In `discover_test.go` add a `DiscoverOpenAICompatibleModels` case (parses `{"data":[{"id":...}]}`, honours the 8 MiB cap, empty list ⇒ Unverifiable, no redirect, never returns the credential).

In `service_vendor_discovery_test.go` add cases: (a) a compat account discovers via the composed `{base}{prefix}/models` URL (fake seam asserts the URL); (b) a Gemini account strips a leading `models/` from each id; (c) discovery of >500 ids stores up to the new cap without truncating at 500.

- [ ] **Step 2: Run to confirm failure**

Run: `cd gateway/backend && go test ./internal/vendorauth/ ./internal/portal/ -run 'DiscoverOpenAICompatible|CompatDiscovery|GeminiStrip|AggregatorCap' -count=1`
Expected: FAIL.

- [ ] **Step 3: Add the vendorauth discoverer**

Append to `gateway/backend/internal/vendorauth/openai_compatible.go`:

```go
// DiscoverOpenAICompatibleModels lists the models an api key can use with GET
// modelsURL (Authorization: Bearer). modelsURL is composed by the caller.
func DiscoverOpenAICompatibleModels(ctx context.Context, httpClient *http.Client, modelsURL, apiKey string) ([]DiscoveredModel, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     modelsURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return nil, DiscoveryUnverifiable
	}
	return discoveryResult(parseDataModels(body, true))
}
```

(Call `parseDataModels(body, true)` so a `display_name`/`name` is used when present — see Step 6.)

- [ ] **Step 4: Wire the portal discovery arm**

In `service_vendor_discovery.go`:
- Add `type VendorOpenAICompatibleDiscoverer func(ctx context.Context, httpClient *http.Client, modelsURL, apiKey string) ([]vendorauth.DiscoveredModel, vendorauth.DiscoveryStatus)`; add field `OpenAICompatibleAPIKey` to `VendorModelDiscoverers`; default it in `withDefaults` to `vendorauth.DiscoverOpenAICompatibleModels`.
- In `discoverAPIKeyModels` (:360-374), before the existing `apiKeyDiscoverer` dispatch, handle the compat vendors:

```go
	if routing.IsOpenAICompatibleVendor(acc.Vendor) {
		preset, _ := routing.VendorPresetFor(acc.Vendor)
		modelsURL := strings.TrimRight(acc.BaseURL, "/") + preset.PathPrefix + "/models"
		models, status := s.vendorDiscovery.discoverers.OpenAICompatibleAPIKey(ctx, s.vendorDiscovery.client, modelsURL, apiKey)
		if preset.StripModelsPrefix != "" {
			for i := range models {
				models[i].Slug = strings.TrimPrefix(models[i].Slug, preset.StripModelsPrefix)
			}
		}
		return models, status, "", nil
	}
```

- Raise `maxDiscoveredModels` (:81) from `500` to `1000` (OpenRouter ~458, Kilo ~390 today; keep the dropped-count reporting). Update the comment.

- [ ] **Step 5: Best-effort discovery on create**

In `CreateVendorAccount` (service_vendor_accounts.go), after the account is persisted, for a compat account run a bounded best-effort discovery (reuse the `discoverAfterConnect` pattern / `vendorConnectDiscoveryTimeout`): discover, store the rows via the existing `SetVendorAccountModels` path, and ignore errors (the account is still created; the user can press Refresh). Pin this with a test asserting a freshly created compat account whose discovery fake returns two models has them stored.

- [ ] **Step 6: Optional display-name fallback**

In `parseDataModels` (discover.go :272): when `display_name` is absent, fall back to a `name` field if present (OpenRouter sends `name`). This is additive and harmless for OpenAI/Anthropic (no `name`). Pin with a small unit test.

- [ ] **Step 7: Run the tests**

Run: `cd gateway/backend && golangci-lint fmt ./internal/vendorauth/... ./internal/portal/... && golangci-lint run ./internal/vendorauth/... ./internal/portal/... && go test ./internal/vendorauth/ ./internal/portal/ -count=1`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add gateway/backend/internal/vendorauth/openai_compatible.go gateway/backend/internal/vendorauth/discover.go gateway/backend/internal/vendorauth/discover_test.go gateway/backend/internal/portal/service_vendor_discovery.go gateway/backend/internal/portal/service_vendor_accounts.go gateway/backend/internal/portal/service_vendor_discovery_test.go
git commit -m "feat(portal,vendorauth): discover models for OpenAI-compatible vendors

Add DiscoverOpenAICompatibleModels and a portal arm that composes
{base}{prefix}/models, strips Gemini's models/ id prefix, and runs a bounded
best-effort discovery on create so a new account is usable immediately
(Refresh re-runs it). Raise the per-discovery cap to 1000 for aggregator
catalogs (OpenRouter/Kilo) and add a name display-name fallback."
```

---

## Task 8: Frontend — vendor dropdown, base URL field, types, i18n

**Files:**
- Modify: `gateway/frontend/src/api/vendorAccounts.ts` (union + `base_url` on `VendorAccount` and `CreateVendorAccountRequest`)
- Modify: `gateway/frontend/src/components/shared/vendorLabel.ts` (5 cases)
- Create: `gateway/frontend/src/components/shared/vendorPresets.ts` (id list + predicate + default-root placeholders)
- Modify: `gateway/frontend/src/components/shared/vendorInputs.ts` (`isValidBaseUrl`)
- Modify: `gateway/frontend/src/components/VendorAccountsView.tsx` (VENDORS; create-form base-URL field + auth-type hiding; detail read-only base URL)
- Modify: `gateway/frontend/src/i18n.ts` (de + en keys)
- Modify: `gateway/frontend/src/components/shared/format.ts` (base-url error-code mapping)
- Modify test factories: `gateway/frontend/src/components/VendorAccountsView.test.tsx`, `components/Dashboard.vendorUsage.test.tsx`, `components/TokenList.test.tsx` (`base_url: ''`)
- Create/Modify tests: `components/shared/vendorLabel.test.ts`, `components/shared/vendorInputs.test.ts`, `VendorAccountsView.test.tsx` (new describe), `api/vendorAccounts.test.ts`, `i18n.test.ts`, `components/shared/format.test.ts`

**Interfaces:**
- Consumes: the backend contract from Tasks 5-7 (`base_url` on the DTO + create request; error codes `vendor_account.base_url_required`/`_invalid`).

- [ ] **Step 1: Install deps (node_modules absent in this worktree)**

Run: `cd gateway/frontend && npm ci`

- [ ] **Step 2: Write failing tests first**

- `components/shared/vendorLabel.test.ts` (new): per-locale table asserting the five new ids map to `t.vendorXAI`/… and an unknown id is shown verbatim.
- `components/shared/vendorInputs.test.ts`: add cases for `isValidBaseUrl` (blank ⇒ true; `https://api.x.ai` ⇒ true; `http://x.test`, `https://u:p@x.test`, `https://x.test/?a=1`, `https://x.test/#f`, `not-a-url` ⇒ false).
- `api/vendorAccounts.test.ts`: a create case with `base_url` asserting it is sent; a preset case asserting `base_url` omitted when the user leaves it blank.
- `i18n.test.ts`: a describe asserting the new keys exist non-empty in de and en.
- `VendorAccountsView.test.tsx`: the new describe (dropdown lists 7 providers in order; base-URL field hidden for openai/anthropic, shown for xai with the default as placeholder; required + trimmed for Custom; omitted from the body for a preset left default; auth select hidden + api_key forced for compat; invalid URL disables Create and shows the error; typed base URL not carried into the next form; detail shows base URL read-only for a compat account).
- `components/shared/format.test.ts`: add `vendor_account.base_url_invalid` (and `_required`) to `vendorAccountWireCodes` and an `it.each` row.

- [ ] **Step 3: Run to confirm failures**

Run: `cd gateway/frontend && npm test -- --run vendorLabel vendorInputs i18n vendorAccounts format VendorAccountsView`
Expected: FAIL (+ `npm run build` type errors once the type changes land).

- [ ] **Step 4: Types + presets + validator**

- `api/vendorAccounts.ts`: widen the union to `'openai' | 'anthropic' | 'xai' | 'openrouter' | 'kilo' | 'google' | 'openai_compatible'`; add `base_url: string` to `VendorAccount` (after `model_prefix`) and `base_url?: string` to `CreateVendorAccountRequest`.
- Create `components/shared/vendorPresets.ts` with `OPENAI_COMPATIBLE_VENDORS`, `isOpenAICompatibleVendor`, and `defaultBaseUrl(vendor)` placeholders (`xai → https://api.x.ai`, `openrouter → https://openrouter.ai/api`, `kilo → https://api.kilo.ai/api`, `google → https://generativelanguage.googleapis.com`, `openai_compatible → https://llm.example.com`). These are display-only placeholders; the backend registry is authoritative.
- `components/shared/vendorInputs.ts`: add `isValidBaseUrl` (blank ⇒ true; else `new URL` parses, `protocol === 'https:'`, non-empty `hostname`, empty `username`/`password`/`search`/`hash`).

- [ ] **Step 5: Labels + i18n**

- `vendorLabel.ts`: add the five `case` branches before `default`.
- `i18n.ts`: add to **both** de and en (tsc enforces parity): `vendorXAI`, `vendorOpenRouter`, `vendorKilo`, `vendorGoogle`, `vendorOpenAICompatible` (after `vendorAnthropic`); `vendorAccountBaseUrlLabel`, `vendorAccountBaseUrlNote`, `vendorAccountBaseUrlCustomNote`, `vendorAccountApiKeyOnlyNote` (after `vendorAccountModelPrefixNote`); `errorVendorAccountBaseUrlInvalid` (after `errorVendorAccountModelPrefixInvalid`). German uses „…“, English straight quotes; write any apostrophe-bearing English string in double quotes to satisfy prettier (then run `npm run format`).
- `components/shared/format.ts`: map `'vendor_account.base_url_invalid' → 'errorVendorAccountBaseUrlInvalid'` (and `base_url_required` to a suitable key, or reuse the invalid key's text) next to the `model_prefix_invalid` row.

- [ ] **Step 6: Create form + detail**

In `VendorAccountsView.tsx`: set `VENDORS = ['openai', 'anthropic', ...OPENAI_COMPATIBLE_VENDORS]`; add `baseUrl` state + `baseUrlValid`/`compatVendor`/`baseUrlMissing`; reset `baseUrl` in `openCreate`; on vendor change clear `baseUrl` and force `api_key` for compat; guard `submitCreate` with `!baseUrlValid || baseUrlMissing` and append `base_url` to the body only when `compatVendor && base !== ''`; hide the auth-type select for compat vendors (render `vendorAccountApiKeyOnlyNote` instead); add the conditional base-URL `Field` after the name field (required only for `openai_compatible`, placeholder = `defaultBaseUrl(vendor)`); add the read-only base-URL `Field` on the detail view for a compat account. Do **not** send `base_url` on PATCH (immutable). (Use the exact JSX from the frontend anchor digest.)

- [ ] **Step 7: Fix the three test factories**

Add `base_url: ''` to `makeVendorAccount` in `VendorAccountsView.test.tsx` (:23-37), `Dashboard.vendorUsage.test.tsx` (:69-83), `TokenList.test.tsx` (:69-83), since `npm run build` type-checks tests against the now-required `base_url`.

- [ ] **Step 8: Run the frontend gates**

Run: `cd gateway/frontend && npm run format && npm run format:check && npm run lint && npm run build && npm test`
Expected: PASS (format:check clean, tsc incl. i18n de/en parity, vitest incl. `src/arch.test.ts`).

- [ ] **Step 9: Commit**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): add OpenAI-compatible providers to the Anbieter UI

Extend the vendor dropdown and labels with x.ai/OpenRouter/Kilo/Gemini and a
Custom option, add an immutable base-URL field (shown for OpenAI-compatible
vendors, required for Custom, validated https) with the preset default as a
placeholder, force api-key auth for them, and show the base URL read-only on
the detail. Types, i18n (de+en), error mapping and tests included."
```

---

## Task 9: Documentation + ADR-052

**Files (all under `docs/architecture/`):**
- `cross-cutting/external-vendor-accounts.md` (§1 entity/base_url row; §2 api-key-only note; §3.5 probe rows; new §4.4; §5.1 scrape note; §6.1 discovery row + §6.4 cap note; §10 VERIFY-LIVE)
- `reference/api-surface.md` (vendor-accounts section: create body + base_url + error rows)
- `reference/openapi.yaml` (vendor enum ×2, `base_url` property + required, create-request base_url)
- `reference/data-model.md` (migration 86 ledger row; ER `base_url` + widened `vendor`; `routing.VendorAccount` row; heading "(86 migrations)")
- `09-architecture-decisions.md` (ADR-052; the 8 in-file `#4-migration-history-85-migrations` anchors)
- `cross-cutting/telemetry-usage-observability.md`, `cross-cutting/persistence.md`, `11-risks-and-technical-debt.md` (anchor + count ripple)
- `05-building-block-view.md`, `12-glossary.md`, `03-context-and-scope.md`, `cross-cutting/compatibility-and-inference.md` (prose touch-ups)

**Interfaces:** none (docs only). Must keep `./scripts/check-docs.sh` green (anchors, forbidden strings, YAML subset).

- [ ] **Step 1: ADR-052**

Append ADR-052 to `09-architecture-decisions.md` after L3292 (copy the ADR-051 shape). Title: "OpenAI-compatible vendors are presets over one OpenAI client: a stored root URL plus a registry-derived path prefix". Record choices (a) registry = data in `internal/routing` (routing ↛ portal); (b) `base_url` stored as a root, prefix on `Target.OpenAIPathPrefix`, default empty ⇒ `/v1` (byte-identical for existing callers); (c) translate-only, api-key-only; (d) per-preset validate/discovery with the existing uniform fail-soft classification (bad key = 400 reads "unverifiable" for x.ai/Gemini); (e) Gemini via its OpenAI shim now, native dialect deferred to sub-project C, usage/credits deferred to sub-project D (registry usage slot); (f) the Custom base-URL trust model (https, no userinfo/query/fragment, host otherwise unrestricted on an on-prem gateway — SSRF accepted, documented in Risks §11.4). Rejected: bespoke per-vendor branches; free-form base_url without a preset identity; a new leaf package (extra allowlist edges); storing the prefix in a column. Close with the `→ [..]` link line. Add a `(extended by ADR-052)` pointer at ADR-049 choice (a) (:2988) — do not rewrite history.

- [ ] **Step 2: external-vendor-accounts.md**

Edits: L15 intro add the five vendors; §1 table — widen the `vendor` row to the 7 ids (with `routing.Vendor*` names) and insert a `base_url` row after `model_prefix` (root, no /v1, '' for bespoke, migration 86); L71-73 add migration 86; §2 add "the five OpenAI-compatible vendors are api_key only (no OAuth/connect)"; §3.5 add a probe row per preset (`GET {base}{prefix}/models` Bearer; OpenRouter `GET {base}/v1/key`; Kilo none) and keep the "probed only at Test connection, never at save" rule; add **§4.4 OpenAI-compatible vendors** (after L842) describing the translate-only matrix row, `Provider=vendor_openai`, Bearer, `Endpoint`=stored root, `OpenAIPathPrefix` from the registry, no passthrough; add the matrix row (L356) too; §5.1 note the vendor_openai scrape is a no-op for them (empty usage state; real usage = sub-project D); §6.1 add a discovery row (via vendorauth with a composed URL, not the provider client), note the chat-only heuristic stays OpenAI-only and the cap is now 1000; §10 add a VERIFY-LIVE bullet for the five presets' roots/prefixes/validate endpoints and the Custom trust model.

- [ ] **Step 3: api-surface.md + openapi.yaml**

- `api-surface.md`: create body `{…, base_url?}`; DTO "always carries base_url"; add error rows `vendor_account.base_url_required` / `vendor_account.base_url_invalid` (400) next to `model_prefix_invalid`; note compat vendors accept only `auth_type=api_key`. PATCH row unchanged (base_url immutable).
- `openapi.yaml`: set the vendor enum to `[openai, anthropic, xai, openrouter, kilo, google, openai_compatible]` at **both** L2237 (VendorAccount) and L2317 (VendorAccountCreateRequest) — do **not** touch the `api_flavor` enum at L2222; add `base_url` to the VendorAccount `required` list and as a property after `model_prefix`; add optional `base_url` to VendorAccountCreateRequest with a description (defaulted from the preset, required for `openai_compatible`, https only). Keep the repo's YAML subset (single-line flow, no conditionals).

- [ ] **Step 4: data-model.md + the migration-count anchor ripple**

- Add the migration-86 ledger row after L539 (mirror the 85 row): adds `vendor_accounts.base_url` via `addColumnIfMissing`, schema only, no backfill, baseline frozen at v60.
- ER block (:218): widen `vendor` and add `string base_url "migration 86, '' = bespoke vendor"`.
- `routing.VendorAccount` domain-type row (:297): add `BaseURL`.
- Rename the heading (:302) to `## 4. Migration history (86 migrations)`.
- Fix every inbound anchor to the renamed heading in the same commit:

```bash
git ls-files -z docs | xargs -0 grep -l '4-migration-history-85-migrations' | xargs sed -i '' 's/4-migration-history-85-migrations/4-migration-history-86-migrations/g'
```

Then bump the two plain-text counts by hand: `cross-cutting/persistence.md:176` ("all 85 migrations") and `11-risks-and-technical-debt.md:193` ("85 migrations and counting"). Do **not** touch legitimate "migration 85" references (they name the vendor_provider_access migration). Verify: `grep -rn 'migration-history-85' docs` returns nothing.

- [ ] **Step 5: Prose touch-ups + Risks §11.4**

- `05-building-block-view.md` (routing/portal/vendorauth rows), `12-glossary.md` (Vendor account entry), `03-context-and-scope.md` (external vendor clouds row: add api.x.ai, openrouter.ai, the Kilo gateway, generativelanguage.googleapis.com, "any https base URL a user supplies"), `cross-cutting/compatibility-and-inference.md` (the `OpenAICompatibleClient` node label → `{prefix}/chat/completions (default /v1)`).
- `11-risks-and-technical-debt.md`: §11.1 add a VERIFY-LIVE row (preset endpoints can change); §11.4 add a deliberate-acceptance row for the Custom base URL (owner-authenticated, on-prem; https + no userinfo/query/fragment; host otherwise unrestricted; redirects refused on validate/discovery; no DNS-rebinding dial guard in v1).

- [ ] **Step 6: Run the docs gate**

Run: `sh scripts/check-docs.test.sh && ./scripts/check-docs.sh`
Expected: PASS (exit 0 — all anchors resolve, no forbidden strings, YAML parses).

- [ ] **Step 7: Commit**

```bash
git add docs/architecture
git commit -m "docs: OpenAI-compatible vendor accounts (ADR-052, migration 86)

Document the OpenAI-compatible vendors end to end: ADR-052, the §4.4 served
matrix row, the base_url column + migration 86 (with the migration-history
anchor/count ripple), the per-preset validate/discovery probes, api-surface
+ openapi base_url and error codes, and the Custom base-URL trust model in
Risks §11.4."
```

---

## Final verification (before finishing the branch)

- [ ] **Backend, exactly as CI runs it (with the Postgres leg):**

```bash
export PATH="$HOME/go/bin:$PATH"
cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN='postgres://postgres:postgres@127.0.0.1:55432/op_test?sslmode=disable' golangci-lint fmt --diff && golangci-lint run && go test -timeout=25m ./...
cd ../../server-agent && golangci-lint fmt --diff && golangci-lint run && go test ./...
```

- [ ] **Docs + frontend:**

```bash
sh scripts/check-docs.test.sh && ./scripts/check-docs.sh
cd gateway/frontend && npm ci && npm run format:check && npm run lint && npm run build && npm test
```

- [ ] **Sonar gate (0 branch findings):** `make sonar-up`; regenerate coverage with the frontend-flake workaround (`npx vitest run --coverage --retry=2`, Go coverprofiles), `make sonar-scan && make sonar-findings && make sonar-branch-findings`. Judge by branch-attributed/new-code findings; `branch-findings` attributes by line, so a touched core function (e.g. `vendorAccountTarget`) may surface pre-existing findings — git-check before acting. If Docker/Sonar is unavailable, say so in the PR.

- [ ] **Tear down the test Postgres:** `docker rm -f op-ai-gw-test-pg`.

- [ ] **Remove the branch-local working docs (AGENTS.md step 10, LAST):** delete this plan and the design spec (AGENTS.md step 10 names the exact working-docs directory); verify `git diff --name-only main...HEAD` lists neither, then open the PR over SSH with a substantive body.

# Vendor Usage/Credits Display Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show usage/credits/limits for OpenAI-compatible vendor accounts using only the stored api key — OpenRouter (`GET /v1/key`, solid) and Kilo (`GET /profile/balance`, best-effort) — reusing the existing usage snapshot store, DTO, refresh endpoint, lazy-TTL tracker, and usage panel.

**Architecture:** A per-preset usage strategy (`UsageVia` + `UsagePath` in the routing registry) + two URL-free `vendorauth` fetchers; the OAuth-only usage-pull path is extended to api-key accounts (widened gates, a split per-kind fetch + shared tail, upsert-as-fetched). No migration — reuses the existing `vendor_account_usage` table, `VendorAccountUsageDTO`, and frontend `VendorAccountUsage` type.

**Tech Stack:** Go (gateway/backend), React + TypeScript + vitest (gateway/frontend), SonarQube + golangci-lint + prettier gates.

**Spec:** `../specs/2026-10-11-vendor-usage-display-design.md` (approved; referenced relatively — `check-docs.sh` forbids the literal branch-local directory string).

## Global Constraints

- Repo-facing text (comments, commits, PR, docs) in **English**; no attribution lines; substantive commit bodies; push over SSH.
- **No DB migration, no new columns, no new wire fields** — reuse `routing.VendorAccountUsage`, `portal.VendorAccountUsageDTO`, and the frontend `VendorAccountUsage` type unchanged. (So **no Postgres leg** — say so in the PR.)
- `vendorauth` imports only `internal/capture`; the new fetchers take a **portal-composed URL + api key** (no `routing` import). Registry lives in `internal/routing`.
- Reuse the existing tolerant-parse contract: unknown sentinels are `-1` (pct), `nil` (reset time), `""` (strings); a writer with no data for a field MUST leave the sentinel (never a fabricated `0`). 401/non-2xx/redirect/parse-failure → `DiscoveryUnverifiable`; **never log or return the api key**; drop transport error text.
- Keep `RefreshVendorAccountUsage`'s signature unchanged (so `portal/api.go`, `api_tracing_gen.go`, and the HTTP handler need no change/regen).
- Native `openai`/`anthropic` api-key accounts and Anthropic subscriptions stay **"unsupported"** (existing tests pin this); only `openrouter`/`kilo` api-key accounts gain an active pull.
- Gates before PR: `golangci-lint fmt --diff` + `run` + `go test ./...` (gateway/backend); frontend `npm ci && format:check && lint && build && test`; `check-docs`; `make sonar-gate`/`sonar-branch-findings` = 0. Frontend `node_modules` is absent in a fresh worktree → `npm ci` first; the Sonar coverage leg uses `npx vitest run --coverage --retry=2`.
- Remove the branch-local working docs (this plan + the design spec) as the LAST step before the PR (AGENTS.md step 10 names the exact directory); never write that forbidden branch-local directory literal in any committed file (`check-docs.sh` check 6).

## Review Focus

- **Native api-key accounts must stay "unsupported".** Widening the pull gate must not turn `openai`/`anthropic` api-key accounts (not in the preset registry) or Anthropic subscriptions into active pulls. (Task 3 — gate test over every account kind.)
- **Kilo degrades gracefully.** The undocumented endpoint, or our api-key type being rejected, must surface as a 200 `{status:"unverifiable"}` → the neutral empty/"unchanged" state, never a 5xx or error. (Tasks 2 + 3.)
- **Upsert-as-fetched must not fabricate zeros or blank a field.** The api-key path writes the fetched snapshot as-is (sentinels preserved); a failed pull keeps the last good row (no write). (Task 3 — test an unlimited→capped and a failed-pull sequence.)
- **OpenRouter unlimited vs capped rendering.** `limit:null` → `credit_status=unlimited` with spend fields empty (panel shows "Unlimited"); a capped key → spend row. The explicit-`null`-vs-absent distinction must be correct. (Task 2 — both cases + the null/absent test.)
- **Frontend gate mirrors the backend set exactly.** The Refresh button + on-view lazy pull appear for `openrouter`/`kilo` api-key accounts and NOT for `xai`/`google`/`openai_compatible`. (Task 4 — per-vendor button-visibility test.)

---

## Task 1: Registry — `UsageVia` + `UsagePath`

**Files:**
- Modify: `gateway/backend/internal/routing/vendor_presets.go` (`VendorPreset` struct: fill `UsageVia` doc, add `UsagePath`; set both on the OpenRouter + Kilo presets)
- Test: `gateway/backend/internal/routing/vendor_presets_test.go`

**Interfaces:**
- Produces: `VendorPreset.UsageVia` (values `"openrouter_key"`, `"kilo_balance"`, `""`) and `VendorPreset.UsagePath string` (root-relative usage URL suffix). Consumed by the portal (Task 3) and mirrored by the frontend vendor set (Task 4).

- [ ] **Step 1: Write the failing test**

Add to `gateway/backend/internal/routing/vendor_presets_test.go`:

```go
func TestVendorPresetUsage(t *testing.T) {
	cases := map[string]struct {
		wantVia  string
		wantURL  string // composed: TrimRight(DefaultBaseURL,"/") + UsagePath; "" when no usage
	}{
		VendorOpenRouter:       {"openrouter_key", "https://openrouter.ai/api/v1/key"},
		VendorKilo:             {"kilo_balance", "https://api.kilo.ai/api/profile/balance"},
		VendorXAI:              {"", ""},
		VendorGoogle:           {"", ""},
		VendorOpenAICompatible: {"", ""},
	}
	for vendor, c := range cases {
		p, ok := VendorPresetFor(vendor)
		if !ok {
			t.Fatalf("VendorPresetFor(%q) ok=false", vendor)
		}
		if p.UsageVia != c.wantVia {
			t.Errorf("%s UsageVia = %q, want %q", vendor, p.UsageVia, c.wantVia)
		}
		got := ""
		if p.UsagePath != "" {
			got = strings.TrimRight(p.DefaultBaseURL, "/") + p.UsagePath
		}
		if got != c.wantURL {
			t.Errorf("%s usage URL = %q, want %q", vendor, got, c.wantURL)
		}
	}
}
```

(Add `"strings"` to the test imports if not present.)

- [ ] **Step 2: Run — expect FAIL** (`UsagePath` undefined / `UsageVia` empty)

Run: `cd gateway/backend && go test ./internal/routing/ -run TestVendorPresetUsage -count=1`

- [ ] **Step 3: Add `UsagePath` + fill the presets**

In `vendor_presets.go`, add after the `UsageVia` field:

```go
	// UsagePath is the ROOT-relative URL suffix for the usage GET (composed as
	// TrimRight(BaseURL,"/")+UsagePath), used when UsageVia != "". It is NOT under
	// PathPrefix: OpenRouter's key endpoint is "/v1/key" and Kilo's undocumented
	// balance is "/profile/balance" (outside its "/gateway" prefix). "" when the
	// provider reports no usage.
	UsagePath string
```

Update the `UsageVia` doc (it is no longer "reserved/empty") to: `UsageVia selects the per-provider usage strategy: "openrouter_key" (GET {BaseURL}{UsagePath} → key limit/usage), "kilo_balance" (best-effort balance), "" = the provider reports no usage with the stored api key.`

Set on the presets:
```go
	VendorOpenRouter: {
		DefaultBaseURL: "https://openrouter.ai/api",
		PathPrefix:     "/v1",
		ValidateVia:    "key",
		ValidatePath:   "/key",
		UsageVia:       "openrouter_key",
		UsagePath:      "/v1/key",
	},
	VendorKilo: {
		DefaultBaseURL: "https://api.kilo.ai/api",
		PathPrefix:     "/gateway",
		ValidateVia:    "none",
		UsageVia:       "kilo_balance",
		UsagePath:      "/profile/balance",
	},
```
(xai/google/openai_compatible leave `UsageVia`/`UsagePath` empty.)

- [ ] **Step 4: Run — expect PASS**; `golangci-lint fmt ./internal/routing/... && golangci-lint run ./internal/routing/...`

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/routing/vendor_presets.go gateway/backend/internal/routing/vendor_presets_test.go
git commit -m "feat(routing): preset usage strategy (UsageVia + UsagePath)

Fill UsageVia for OpenRouter (openrouter_key) and Kilo (kilo_balance) and
add a root-relative UsagePath (OpenRouter /v1/key, Kilo /profile/balance;
Kilo's balance is outside its /gateway prefix). Other presets report no usage."
```

---

## Task 2: vendorauth usage fetchers (OpenRouter + Kilo)

**Files:**
- Create: `gateway/backend/internal/vendorauth/compatible_usage.go` (`FetchOpenRouterKeyUsage`, `FetchKiloBalanceUsage`, a money formatter + a JSON-number reader if needed)
- Test: `gateway/backend/internal/vendorauth/compatible_usage_test.go`

**Interfaces:**
- Consumes: the existing `OpenAISubscriptionUsage` result struct, `unknownOpenAISubscriptionUsage()`, `fetchModelList`/`modelListSpec`, `authBearerPrefix`, `DiscoveryOK`/`DiscoveryUnverifiable`, the `creditStatus*` consts, `readJSONNumber`/`readCreditBalance` (in `usage.go`).
- Produces: `FetchOpenRouterKeyUsage(ctx, httpClient, keyURL, apiKey) (OpenAISubscriptionUsage, DiscoveryStatus)` and `FetchKiloBalanceUsage(ctx, httpClient, balanceURL, apiKey) (OpenAISubscriptionUsage, DiscoveryStatus)`. Consumed by the portal seam (Task 3).

- [ ] **Step 1: Write failing tests**

Create `compatible_usage_test.go` mirroring `usage_test.go`'s harness (`newProbeClient`/`rewriteTransport` serving a fixed body; assert the parsed snapshot + status; assert no credential leak + no redirect). Cases:
- OpenRouter **capped**: body `{"data":{"label":"k","limit":100,"limit_remaining":74.5,"limit_reset":"monthly","usage":25.5,"is_free_tier":false}}` → `SpendUnit=="USD"`, `SpendLimit=="100"`, `SpendRemaining=="74.5"`, `SpendUsed=="25.5"`, `SpendUsedPct≈25.5`, `CreditStatus=="has_credits"`, `CreditBalance==""`, `FiveHourPct==-1`, status `DiscoveryOK`.
- OpenRouter **unlimited**: `{"data":{"limit":null,"usage":12.3}}` → `CreditStatus=="unlimited"`, `SpendLimit==""`, `SpendUsed==""`, `SpendRemaining==""`, `SpendUsedPct==-1`, status OK.
- OpenRouter **nothing usable**: `{"data":{"label":"k"}}` (no limit/usage) → `DiscoveryUnverifiable`.
- OpenRouter **remaining=0** (capped): `{"data":{"limit":10,"limit_remaining":0}}` → `CreditStatus=="none"`, `SpendUsedPct≈100`.
- OpenRouter **wrong-typed** `{"data":{"limit":"oops"}}` → degrades to Unverifiable (no usable field).
- Kilo: `{"balance":12.34}` → `CreditBalance=="12.34"`, `CreditStatus=="has_credits"`, `SpendUnit==""`, status OK; `{"balance":0}` → `CreditStatus=="none"`; `{}` → Unverifiable; non-JSON → Unverifiable.
- Both: a 401 response → Unverifiable (via `fetchModelList` returning `!ok`); the probe recorder shows the credential never appears in any returned field.

- [ ] **Step 2: Run — expect FAIL** (functions undefined)

Run: `cd gateway/backend && go test ./internal/vendorauth/ -run 'OpenRouter|Kilo' -count=1`

- [ ] **Step 3: Implement the fetchers**

Create `compatible_usage.go`:

```go
// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 OnPrem AI Gateway contributors

package vendorauth

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
)

// FetchOpenRouterKeyUsage reads OpenRouter's per-key limit/usage from GET keyURL
// (the /api/v1/key endpoint; composed by the portal), Authorization: Bearer.
// REVERSE-ENGINEERED / VERIFY-LIVE: the response shape is OpenRouter-docs-derived.
// All amounts are USD credits and are PER-KEY, not an account balance.
func FetchOpenRouterKeyUsage(ctx context.Context, httpClient *http.Client, keyURL, apiKey string) (OpenAISubscriptionUsage, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     keyURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	usage, known := parseOpenRouterKeyUsage(body)
	if !known {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	return usage, DiscoveryOK
}

func parseOpenRouterKeyUsage(body []byte) (OpenAISubscriptionUsage, bool) {
	var env struct {
		Data struct {
			Limit          json.RawMessage `json:"limit"`
			LimitRemaining json.RawMessage `json:"limit_remaining"`
			Usage          json.RawMessage `json:"usage"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &env)
	u := unknownOpenAISubscriptionUsage()
	limit, limitOK := readJSONNumber(env.Data.Limit)
	remaining, remOK := readJSONNumber(env.Data.LimitRemaining)
	usage, usageOK := readJSONNumber(env.Data.Usage)
	limitIsNull := bytes.Equal(bytes.TrimSpace(env.Data.Limit), []byte("null"))
	switch {
	case limitOK: // capped key
		u.SpendUnit = "USD"
		u.SpendLimit = formatMoney(limit)
		if remOK {
			u.SpendRemaining = formatMoney(remaining)
			used := limit - remaining
			if used < 0 {
				used = 0
			}
			u.SpendUsed = formatMoney(used)
			if limit > 0 {
				u.SpendUsedPct = math.Min(math.Max(used/limit*100, 0), maxUsagePct)
			}
			if remaining > 0 {
				u.CreditStatus = creditStatusHasCredits
			} else {
				u.CreditStatus = creditStatusNone
			}
		}
	case limitIsNull: // explicit null => no per-key cap
		u.CreditStatus = creditStatusUnlimited
		// Deliberately leave spend fields empty so the panel shows "Unlimited"
		// rather than an all-time "N USD used"; usage is all-time, not a period.
		_ = usage
		_ = usageOK
	}
	known := u.SpendLimit != "" || u.CreditStatus != ""
	return u, known
}

// FetchKiloBalanceUsage reads Kilo's balance from GET balanceURL (the undocumented
// /api/profile/balance; composed by the portal), Authorization: Bearer.
// BEST-EFFORT / UNDOCUMENTED: shape beyond {"balance":n} is unknown and the stored
// api-key type may not be accepted; any failure degrades to Unverifiable.
func FetchKiloBalanceUsage(ctx context.Context, httpClient *http.Client, balanceURL, apiKey string) (OpenAISubscriptionUsage, DiscoveryStatus) {
	body, ok := fetchModelList(ctx, httpClient, modelListSpec{
		url:     balanceURL,
		headers: map[string]string{"Authorization": authBearerPrefix + apiKey},
	})
	if !ok {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	var env struct {
		Balance json.RawMessage `json:"balance"`
	}
	_ = json.Unmarshal(body, &env)
	u := unknownOpenAISubscriptionUsage()
	balance, balOK := readJSONNumber(env.Balance)
	if !balOK {
		return unknownOpenAISubscriptionUsage(), DiscoveryUnverifiable
	}
	u.CreditBalance = formatMoney(balance)
	if balance > 0 {
		u.CreditStatus = creditStatusHasCredits
	} else {
		u.CreditStatus = creditStatusNone
	}
	return u, DiscoveryOK
}

// formatMoney renders a monetary amount without exponent or float noise, within
// the stored-string length cap. (The frontend only reformats a plain-decimal
// spend_used; spend_limit/credit_balance are shown as sent.)
func formatMoney(v float64) string {
	return strconv.FormatFloat(math.Round(v*1e6)/1e6, 'f', -1, 64)
}
```

**Confirm before coding** (read `usage.go`): the exact names `readJSONNumber`, `maxUsagePct`, `creditStatusHasCredits`/`creditStatusNone`/`creditStatusUnlimited`, `unknownOpenAISubscriptionUsage`, `fetchModelList`/`modelListSpec`, `authBearerPrefix`. If `readJSONNumber` cannot be reused for `balance`/`limit` as written, adapt (it returns `(float64, bool)` and treats null/absent/non-number as `!ok`).

- [ ] **Step 4: Run — expect PASS**; `golangci-lint fmt ./internal/vendorauth/... && golangci-lint run ./internal/vendorauth/...`

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/vendorauth/compatible_usage.go gateway/backend/internal/vendorauth/compatible_usage_test.go
git commit -m "feat(vendorauth): OpenRouter key-usage + Kilo balance fetchers

Add URL-free FetchOpenRouterKeyUsage (GET /v1/key: capped key -> spend
limit/used/remaining/pct USD, unlimited -> credit_status unlimited) and
FetchKiloBalanceUsage (undocumented /profile/balance -> credit_balance,
best-effort). Both reuse the tolerant-parse contract and fail soft to
Unverifiable; neither logs or returns the api key."
```

---

## Task 3: Portal — extend the usage pull to api-key accounts

**Files:**
- Modify: `gateway/backend/internal/portal/service_vendor_usage_refresh.go` (`hasActiveVendorUsagePull`; `RefreshVendorAccountUsage` api-key branch; reword the unsupported detail + doc comments)
- Modify: `gateway/backend/internal/portal/service_vendor_discovery.go` (new seam `VendorOpenAICompatibleUsageFetcher` + field + `withDefaults`; split `fetchVendorUsage` into a per-kind fetch + shared tail; new `fetchCompatibleVendorUsage`; `refreshVendorUsage` dispatch by auth type)
- Test: `gateway/backend/internal/portal/service_vendor_usage_refresh_test.go`, `service_vendor_discovery_test.go`

**Interfaces:**
- Consumes: `routing.VendorPresetFor(...).UsageVia`/`UsagePath` (Task 1); `vendorauth.FetchOpenRouterKeyUsage`/`FetchKiloBalanceUsage` (Task 2); existing `openVendorAPIKey`, `MergeVendorAccountUsage`, `UpsertVendorAccountUsage`, the `vendorUsagePulls` tracker.
- Produces: api-key usage pull for `openrouter`/`kilo`; `hasActiveVendorUsagePull` true for them.

- [ ] **Step 1: Write failing tests** (extend the existing suites; use the `fakeVendorDiscoverers` seam so no network)
- `hasActiveVendorUsagePull`: table over account kinds — `openrouter` api_key → true, `kilo` api_key → true, `openai`/`anthropic` api_key → false, `openai` subscription → true, `anthropic` subscription → false, `xai`/`google`/`openai_compatible` api_key → false.
- `RefreshVendorAccountUsage` for an `openrouter` api-key account: calls the (faked) compatible usage fetcher with the composed `https://openrouter.ai/api/v1/key` URL + the opened key, upserts the snapshot, returns `ok`; a second immediate call within the 5-min TTL returns `fresh`; `?force` bypasses.
- api-key account with **no key stored** → `unverifiable` with "no API key is set" (no fetcher call).
- Kilo fetcher returns `DiscoveryUnverifiable` (undocumented miss) → `RefreshVendorAccountUsage` returns `unverifiable`, no write, the last snapshot kept.
- **upsert-as-fetched**: an `openrouter` account whose first pull wrote a capped snapshot, then a pull reporting unlimited (`SpendLimit=""`, `CreditStatus=unlimited`) → the stored row becomes unlimited with spend fields cleared (NOT the merged stale limit). (This asserts the api-key path does not call `MergeVendorAccountUsage`.)
- native `openai`/`anthropic` api-key `RefreshVendorAccountUsage` still `unsupported` (pin the existing cases).
- the model-refresh trailing usage pull runs for an `openrouter` api-key account (its snapshot is populated after `RefreshVendorAccountModels`).

- [ ] **Step 2: Run — expect FAIL**

Run: `cd gateway/backend && go test ./internal/portal/ -run 'VendorUsage|FetchVendorUsage|RefreshVendorAccountUsage' -count=1`

- [ ] **Step 3: Widen the gate (registry-driven)**

`service_vendor_usage_refresh.go` `hasActiveVendorUsagePull`:
```go
func hasActiveVendorUsagePull(acc routing.VendorAccount) bool {
	if acc.AuthType == routing.VendorAuthSubscription {
		return acc.Vendor == routing.VendorOpenAI
	}
	if acc.AuthType == routing.VendorAuthAPIKey {
		if preset, ok := routing.VendorPresetFor(acc.Vendor); ok {
			return preset.UsageVia != ""
		}
	}
	return false
}
```
(openai/anthropic api-key are not in the preset registry → `ok` false → stays false.) Update the doc comment.

- [ ] **Step 4: Add the compat-usage seam (one dispatcher seam)**

`service_vendor_discovery.go`: add ONE injectable seam that carries the strategy so a single field dispatches to the right `vendorauth` fetcher (and tests can intercept it):
```go
type VendorOpenAICompatibleUsageFetcher func(ctx context.Context, httpClient *http.Client, usageVia, usageURL, apiKey string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus)
```
Add `OpenAICompatibleUsage VendorOpenAICompatibleUsageFetcher` to `VendorModelDiscoverers`, and in `withDefaults` default it to `defaultCompatibleUsageFetcher`:
```go
func defaultCompatibleUsageFetcher(ctx context.Context, httpClient *http.Client, usageVia, usageURL, apiKey string) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus) {
	switch usageVia {
	case "openrouter_key":
		return vendorauth.FetchOpenRouterKeyUsage(ctx, httpClient, usageURL, apiKey)
	case "kilo_balance":
		return vendorauth.FetchKiloBalanceUsage(ctx, httpClient, usageURL, apiKey)
	default:
		return vendorauth.OpenAISubscriptionUsage{FiveHourPct: -1, WeeklyPct: -1, SpendUsedPct: -1}, vendorauth.DiscoveryUnverifiable
	}
}
```
Add the new field to the `withDefaults` all-fields-non-nil completeness test (`service_vendor_discovery_test.go` ~:1627/1642/1668-1677).

- [ ] **Step 5: Split `fetchVendorUsage` into per-kind fetch + shared tail**

Refactor `service_vendor_discovery.go:499-542` so the record→status→build-snapshot→store tail is shared. Keep the subscription path (`ts.AccessToken`) exactly as-is (it still calls `MergeVendorAccountUsage`). Extract:
- `storeVendorUsageSnapshot(ctx, acc, snapshot, merge bool) string` — does the `record` + read-existing + (merge? `MergeVendorAccountUsage` : as-is) + `UpsertVendorAccountUsage`, returning the status string. (The `record` call stays where it is relative to the fetch.)
- Subscription path: `merge=true`. api-key path: `merge=false` (upsert-as-fetched; single writer).

- [ ] **Step 6: Add `fetchCompatibleVendorUsage` + dispatch**

```go
func (s *Service) fetchCompatibleVendorUsage(ctx context.Context, acc routing.VendorAccount) string {
	preset, ok := routing.VendorPresetFor(acc.Vendor)
	if !ok || preset.UsageVia == "" {
		return VendorUsageRefreshUnsupported
	}
	apiKey, err := s.openVendorAPIKey(acc)
	if err != nil {
		return VendorUsageRefreshUnverifiable // credential unreadable; keep stored
	}
	if apiKey == "" {
		return VendorUsageRefreshUnverifiable // no key set
	}
	usageURL := strings.TrimRight(acc.BaseURL, "/") + preset.UsagePath
	usage, status := s.vendorDiscovery.discoverers.OpenAICompatibleUsage(ctx, s.vendorDiscovery.client, preset.UsageVia, usageURL, apiKey)
	if ctx.Err() == nil {
		s.vendorUsagePulls.record(acc.ID, s.clock())
	}
	if status != vendorauth.DiscoveryOK {
		return VendorUsageRefreshUnverifiable
	}
	snapshot := buildVendorUsageSnapshot(acc.ID, usage, s.clock().UTC()) // shared field-copy helper (Step 5)
	return s.storeVendorUsageSnapshot(ctx, snapshot, false /* upsert as-fetched */)
}
```
(Introduce `buildVendorUsageSnapshot` as the extracted 14-field copy so both paths share it; `storeVendorUsageSnapshot` takes the built snapshot.)

In `RefreshVendorAccountUsage` (before `currentSubscriptionTokens`, after the `within`-TTL check): branch on auth type — for `VendorAuthAPIKey`, `return s.vendorUsageRefreshAnswer(ctx, acc.ID, s.fetchCompatibleVendorUsage(ctx, acc), "")`. (The gate already guaranteed only usage-capable api-key accounts reach here.)

In `refreshVendorUsage` (the model-refresh trailing pull, `:466-473`): dispatch by auth type — `VendorAuthAPIKey && hasActiveVendorUsagePull(acc)` → `_ = s.fetchCompatibleVendorUsage(ctx, acc)`; else the existing `_ = s.fetchVendorUsage(ctx, acc, ts)`.

- [ ] **Step 7: Reword the stale "OpenAI subscription only" strings/comments**

`service_vendor_usage_refresh.go:64` detail string → e.g. `"this account has no usage endpoint the gateway can ask"`. Update the doc comments at `service_vendor_usage_refresh.go:14-18,113-116,121-125`, `service_vendor_discovery.go:475-491`, and `gateway/portal_vendor_account_endpoints.go:279-293` to say "an OpenAI subscription or an OpenRouter/Kilo api-key account".

- [ ] **Step 8: Run — expect PASS**; fmt + run

Run: `cd gateway/backend && golangci-lint fmt ./... && golangci-lint run ./internal/portal/... ./internal/gateway/... && go test ./internal/portal/ ./internal/gateway/ -count=1`

- [ ] **Step 9: Commit**

```bash
git add gateway/backend/internal/portal/service_vendor_usage_refresh.go gateway/backend/internal/portal/service_vendor_discovery.go gateway/backend/internal/portal/service_vendor_usage_refresh_test.go gateway/backend/internal/portal/service_vendor_discovery_test.go
git commit -m "feat(portal): usage pull for OpenRouter/Kilo api-key accounts

Widen the usage-pull gate (registry UsageVia) to OpenRouter/Kilo api-key
accounts, branch the refresh + model-refresh paths to open the stored key and
fetch via the preset's UsageVia/UsagePath, and upsert as-fetched (single
writer, no merge). Native openai/anthropic api-key accounts stay unsupported.
Keeps RefreshVendorAccountUsage's signature; reuses the 5-min lazy TTL."
```

---

## Task 4: Frontend — Refresh button + lazy pull for OpenRouter/Kilo

**Files:**
- Modify: `gateway/frontend/src/components/VendorAccountUsagePanel.tsx` (widen `hasActiveUsagePull`; the subtitle for a balance-only snapshot; doc comments)
- Modify: `gateway/frontend/src/i18n.ts` (a balance-pull intro string, de + en) — only if the subtitle change needs a new key
- Test: `gateway/frontend/src/components/VendorAccountsView.test.tsx` (extend the button-visibility + lazy-refresh it.each)

**Interfaces:**
- Consumes: the backend contract from Task 3 (OpenRouter/Kilo api-key accounts now answer a non-"unsupported" refresh). Mirrors the backend usage-vendor set `{openrouter, kilo}`.

- [ ] **Step 1: `npm ci`** (node_modules absent in a fresh worktree)

Run: `cd gateway/frontend && npm ci`

- [ ] **Step 2: Write/extend failing tests**

In `VendorAccountsView.test.tsx`, extend the existing it.each blocks (`~:2477-2494` "hides the button for %s", `~:2646-2662` "does not fire the lazy refresh for %s") and add: an `openrouter` api-key account and a `kilo` api-key account **show** the Refresh button and fire **one** `refreshUsage(id)` lazy on-view call (no options); `xai`/`google`/`openai_compatible` api-key accounts still hide it. Use `makeVendorAccount({ vendor: 'openrouter', auth_type: 'api_key', api_key_set: true })`.

- [ ] **Step 3: Run — expect FAIL**

Run: `cd gateway/frontend && npm test -- --run VendorAccountsView`

- [ ] **Step 4: Widen `hasActiveUsagePull`**

In `VendorAccountUsagePanel.tsx`:
```ts
// Vendors whose stored api key the gateway can use to ask for usage/credits
// (mirrors the routing preset's UsageVia != ""): OpenRouter and, best-effort, Kilo.
const API_KEY_USAGE_VENDORS: ReadonlySet<string> = new Set(['openrouter', 'kilo']);

export function hasActiveUsagePull(
  account: Pick<VendorAccount, 'vendor' | 'auth_type' | 'subscription_connected' | 'api_key_set'>,
): boolean {
  if (account.auth_type === 'subscription') {
    return account.vendor === 'openai' && account.subscription_connected;
  }
  return account.api_key_set && API_KEY_USAGE_VENDORS.has(account.vendor);
}
```
Update its doc comment. (`VendorAccountUsageSection`'s `usageCapable` already admits api-key accounts; only this gate changes.)

- [ ] **Step 5: Fix the balance-only subtitle**

The panel subtitle (`VendorAccountUsagePanel.tsx:129-131`) uses `hasSpendControl(usage) ? vendorUsageIntroSpend : vendorUsageIntro`; a Kilo balance-only snapshot has `hasSpendControl === false` and would show `vendorUsageIntro` ("percentage … refreshed with every request the gateway serves"), which is wrong for a pulled balance. Change the condition to treat a pulled snapshot as spend-like: `hasSpendControl(usage) || usage.credit_balance !== '' || usage.credit_status === 'unlimited' ? vendorUsageIntroSpend : vendorUsageIntro`. (`vendorUsageIntroSpend` already describes refresh-on-view/button/models-refresh; the "reconnected" clause is harmless for api keys.) No new i18n key needed. If the implementer judges a dedicated string clearer, add `vendorUsageIntroBalance` to **both** `de` and `en` (i18n.test.ts enforces parity) and use it for the `credit_balance` case.

- [ ] **Step 6: Run the gates**

Run: `cd gateway/frontend && npm run format && npm run format:check && npm run lint && npm run build && npm test`
Expected: PASS (incl. `src/arch.test.ts`, i18n de/en parity).

- [ ] **Step 7: Commit**

```bash
git add gateway/frontend/src
git commit -m "feat(frontend): usage refresh for OpenRouter/Kilo api-key accounts

Widen hasActiveUsagePull so OpenRouter/Kilo api-key accounts get the Refresh
button + the one-shot on-view lazy pull (mirroring the backend UsageVia set);
xai/google/custom stay button-less. Route a balance-only snapshot to the
pull-based subtitle. No TS type or wire change."
```

---

## Task 5: Documentation + ADR-053

**Files (under `docs/architecture/`):**
- `cross-cutting/external-vendor-accounts.md` (§4.4 UsageVia now filled; §5.1/§5.2 OpenRouter/Kilo usage + the per-key and best-effort caveats; the "api_key accounts have no active pull" wording)
- `reference/api-surface.md` (the usage-refresh endpoint now supports openrouter/kilo)
- `09-architecture-decisions.md` (ADR-053)
- `11-risks-and-technical-debt.md` (Kilo undocumented endpoint; OpenRouter per-key-not-account; our-key-type-on-Kilo unconfirmed; the 1-decimal spend display)
- the stale `VendorAccountUsageDTO` doc comment (`gateway/backend/internal/portal/service_vendor_accounts.go:43-66` "percentages only / no N of M") — reword in the same change, or note it's covered by the §5 docs

**Interfaces:** none (docs). Gate: `check-docs` exit 0.

- [ ] **Step 1: ADR-053**

Append ADR-053 to `09-architecture-decisions.md` (copy the ADR-052 shape; next free number — verify ADR-052 is last). Title: "Usage/credits for api-key vendor accounts: a per-preset usage strategy over the existing OAuth-only pull path". Record: (a) extend the OAuth-only usage pull to api-key accounts, gated by the preset's `UsageVia` (not a hardcode); (b) `UsagePath` is root-relative (Kilo's balance is outside its `/gateway` prefix; OpenRouter's `/v1/key` equals its validate URL); (c) reuse the existing usage snapshot/DTO/panel — no migration, OpenRouter maps to spend_*, Kilo to credit_balance; richer fields (daily/weekly/monthly, free-tier) deferred; (d) **upsert-as-fetched** for the compat path (single writer — no passive scrape for these — so merge's "can't blank" rule is unneeded and would keep a stale limit); (e) OpenRouter is per-**key** not account balance, and unlimited renders "Unlimited"; (f) Kilo best-effort (undocumented endpoint; our api-key type may be rejected) → graceful `unverifiable`. Rejected: new DTO columns for the richer OpenRouter data (v1); an OpenRouter-only hardcode; x.ai Management-API usage (second credential). Close with the `→` links line.

- [ ] **Step 2: external-vendor-accounts.md + api-surface.md + the DTO comment**

Fill §4.4's UsageVia note; add a §5 subsection for OpenRouter (`/v1/key`, USD, per-key) + Kilo (best-effort, undocumented) usage; reword the "api_key accounts have no active pull / only an OpenAI subscription" statements to include OpenRouter/Kilo; update `api-surface.md`'s usage-refresh endpoint description; reword the `VendorAccountUsageDTO` "percentages only, no N of M" comment (OpenRouter brings USD amounts).

- [ ] **Step 3: Risks §11**

Add deliberate-acceptance/VERIFY-LIVE rows: Kilo `/api/profile/balance` undocumented + our api-key type unconfirmed (degrades to empty state); OpenRouter `/v1/key` is per-key, not account balance (account credits need a management key we don't use); OpenRouter response shape is docs-derived (REVERSE-ENGINEERED, tolerant parser); the shared spend display rounds to 1 decimal (USD cents lose precision) — follow-up.

- [ ] **Step 4: Run the docs gate**

Run: `sh scripts/check-docs.test.sh && ./scripts/check-docs.sh` → exit 0.

- [ ] **Step 5: Commit**

```bash
git add docs/architecture gateway/backend/internal/portal/service_vendor_accounts.go
git commit -m "docs: vendor usage for OpenRouter/Kilo (ADR-053)

Document the api-key usage pull (ADR-053), OpenRouter /v1/key (per-key, USD)
and Kilo best-effort balance in external-vendor-accounts + api-surface, the
Risks entries (Kilo undocumented, OpenRouter per-key, 1-decimal display), and
reword the stale 'percentages only' DTO comment."
```

---

## Final verification (before finishing the branch)

- [ ] **Backend:** `export PATH="$HOME/go/bin:$PATH"; cd gateway/backend && golangci-lint fmt --diff && golangci-lint run && go test ./... ` (no Postgres leg needed — no schema change; state this in the PR) + `cd ../../server-agent && golangci-lint fmt --diff && golangci-lint run && go test ./...`
- [ ] **Docs + frontend:** `sh scripts/check-docs.test.sh && ./scripts/check-docs.sh`; `cd gateway/frontend && npm ci && npm run format:check && npm run lint && npm run build && npm test`
- [ ] **Sonar:** `make sonar-up`; regenerate coverage (`npx vitest run --coverage --retry=2` + Go coverprofiles); `make sonar-scan && make sonar-findings && make sonar-branch-findings` = 0 branch findings. If Docker/Sonar unavailable, say so in the PR.
- [ ] **Remove the branch-local working docs (LAST):** delete this plan + the design spec (AGENTS.md step 10 names the directory); verify `git diff --name-only main...HEAD` lists neither; then push over SSH and open the PR with a substantive body.

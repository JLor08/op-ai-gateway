# Design — Usage / credits display for OpenAI-compatible vendor accounts (sub-project D)

Date: 2026-10-11
Branch: `feat/vendor-usage-display` (off `main` 1520d59)
Status: approved (design conversation), pending written-spec review
Part of: "support more providers" decomposition. Sub-project **A** (OpenAI-compatible vendor accounts) shipped in #205; this is **D** (usage/credits/limits for those accounts). Sub-project **C** (native Gemini dialect) is issue #206.

## 1. Goal

Show usage / credits / limits for the OpenAI-compatible vendor accounts added in A, using **only the api key already stored** for the account — reusing the existing vendor-usage snapshot store, DTO, refresh endpoint, lazy-TTL tracker, and the availability-aware usage panel (built in #182/#195/#199 for OpenAI-subscription accounts).

## 2. Feasibility reality (verified 2026-10-11) — drives the scope

Usage is only reachable with the **stored inference key** for some providers:

| Provider | With the stored api key? | Endpoint | Maps to |
|---|---|---|---|
| **OpenRouter** | **Yes** | `GET {base}/v1/key` (same URL as its validate probe) | spend_limit/used/remaining/pct, credit_status (USD, per-key) |
| **Kilo** | **Best-effort** | `GET {base}/profile/balance` (undocumented; under the **root**, not `/gateway`) | credit_balance + credit_status |
| **x.ai** | **No** | balance/usage only on the separate Management API (distinct key + `team_id`) | — |
| **Google Gemini** | **No** | there is no usage/balance/quota-remaining API (per-project, AI Studio only) | — |
| **Custom** | **No** standard endpoint | — | — |

So D = **OpenRouter** (solid) + **Kilo** (best-effort). x.ai/Gemini/Custom fetch nothing and keep the existing "no limits from the provider are known" empty state (zero work).

**Kilo caveat:** `/api/profile/balance` is reverse-engineered from Kilo's own client, which calls it with an *OAuth* access token; it is unconfirmed that the *api-key* type we store is accepted. D treats Kilo as strictly best-effort: any 401/403/404/parse failure degrades to the empty state, never an error.

## 3. Decisions (from the approved design)

1. **Registry-driven**, like A: the preset carries the usage strategy; the portal composes the URL; `vendorauth` stays URL-free (takes a composed URL + api key). No bespoke per-vendor branches in the resolver/portal beyond a strategy switch.
2. **No migration, no new columns, no new wire fields.** Reuse the existing `vendor_account_usage` table, `routing.VendorAccountUsage`, `portal.VendorAccountUsageDTO`, and the frontend `VendorAccountUsage` type (all already carry credit/spend/pct fields). **⇒ no Postgres leg for D.**
3. OpenRouter's richer fields with **no DTO home** (`usage_daily/weekly/monthly`, `is_free_tier`, `free_model_daily_requests`, BYOK) are **out of v1** (they would need new columns). Listed as follow-up.
4. **Upsert-as-fetched** for the api-key path (no merge): these accounts have a single usage writer (the passive scrape writes nothing for them — it only reads Codex/Anthropic headers), so `MergeVendorAccountUsage` (which can never express "now absent") is not needed and would keep a stale limit after a cap is removed. Still **skip the write on a failed pull** (keep the last good snapshot).
5. Keep `RefreshVendorAccountUsage`'s signature unchanged (so `portal/api.go`, the generated `api_tracing_gen.go`, and the HTTP handler are untouched).

## 4. Registry additions (`internal/routing/vendor_presets.go`)

`VendorPreset` gains one field (`UsageVia` already exists, reserved):
- Fill **`UsageVia`** (strategy key): `VendorOpenRouter → "openrouter_key"`, `VendorKilo → "kilo_balance"`, all others `""` (none).
- Add **`UsagePath string`** — the **root-relative** URL suffix for the usage GET: OpenRouter `"/v1/key"`, Kilo `"/profile/balance"`, others `""`. (Root-relative because Kilo's balance is under the host root, not under `PathPrefix` `/gateway`; OpenRouter's `/v1/key` is also expressible root-relative, so one uniform rule: `usageURL = TrimRight(acc.BaseURL,"/") + preset.UsagePath`.)

Update the `UsageVia` doc comment (no longer "reserved/empty"). Optional typed constants for the strategy keys (mirrors the `ValidateVia` string-literal style — keep as strings for consistency with A).

## 5. vendorauth fetchers (`internal/vendorauth/`, capture-only, URL-free)

Two new functions modeled on `FetchOpenAISubscriptionUsage` / `DiscoverOpenAICompatibleModels` (take a caller-composed URL + api key; reuse `fetchModelList` → tolerant parse → `(snapshot, DiscoveryStatus)`; 401/non-2xx/redirect → `DiscoveryUnverifiable`; never log/return the key):

- `FetchOpenRouterKeyUsage(ctx, httpClient, keyURL, apiKey) (OpenAISubscriptionUsage, DiscoveryStatus)`
- `FetchKiloBalanceUsage(ctx, httpClient, balanceURL, apiKey) (OpenAISubscriptionUsage, DiscoveryStatus)`

Both fill the existing `OpenAISubscriptionUsage` result struct (reused as-is — its field set is already generic; renaming it to a neutral name is deferred naming-debt, noted). Parsing follows the established tolerant pattern (unmarshal into `json.RawMessage` fields, ignore the decode error, start from the all-unknown constructor, mark `known = any field set`, return Unverifiable when nothing usable).

### 5.1 OpenRouter mapping (`GET {base}/v1/key` → `{"data":{...}}`)
Fields are **docs-derived (VERIFY-LIVE)** — no real key was available. Decode `data` defensively (wrapped object; tolerate missing newer fields `budgets`, `effective_budget`, `workspace_id`, …). `limit`/`limit_remaining` are `number|null` (null = unlimited). Units: USD.
- **Capped key** (`limit` is a number): `SpendUnit="USD"`, `SpendLimit=fmt(limit)`, `SpendRemaining=fmt(limit_remaining)`, `SpendUsed=fmt(limit − limit_remaining)` (consistent with the bar, whatever `limit_reset` is), `SpendUsedPct = clamp((limit−remaining)/limit*100, 0..100)` when `limit>0`, `CreditStatus = has_credits` if `remaining>0` else `none`. `SpendResetAt`: from a `budgets[]` entry with `scope=="key_limit"` `resets_at` (RFC3339) when present, else nil (do not guess from `limit_reset` unless the value set is confirmed live).
- **Unlimited key** (`limit` is explicit `null` — distinguish null from absent by literal check, since the number reader cannot): `CreditStatus="unlimited"`, spend fields left empty (so the panel shows "Unlimited", not an all-time number — `creditKind` priority would otherwise hide "unlimited" behind a spend row).
- `CreditBalance` stays `""` (the endpoint is per-**key**, not account balance; account balance needs a management key we don't have). Five-hour/weekly pct stay `-1`.
- Money formatting: `strconv.FormatFloat(round(v,1e-6), 'f', -1, 64)` — no exponent/float-noise (the frontend only reformats a plain-decimal `spend_used`, and `spend_limit` is shown verbatim), ≤ 64 chars.

### 5.2 Kilo mapping (`GET {base}/profile/balance` → `{"balance": <number>}`, undocumented)
Fully tolerant (shape beyond `balance` unknown): `CreditBalance = fmt(balance)`, `CreditStatus = has_credits` if `balance>0` else `none`. Leave `SpendUnit=""` (unit inferred only from the Kilo CLI's `$` formatting, not an API contract — don't assert USD). `known = balance present`. Any failure → `DiscoveryUnverifiable` → empty state. Optional `x-kilocode-organizationid` header is out of v1 (personal balance only).

## 6. Portal wiring (`internal/portal/`)

- **`hasActiveVendorUsagePull`** (`service_vendor_usage_refresh.go:117-119`): widen to also return true when `acc.AuthType==VendorAuthAPIKey && preset.UsageVia != ""` (via `routing.VendorPresetFor`). Native openai/anthropic api-key accounts are not in the preset registry → stay false (existing tests pin this).
- **`RefreshVendorAccountUsage`** (`:156-178`): insert an api-key branch **before** `currentSubscriptionTokens` (which is OAuth-only and would wrongly answer "not connected" for an api-key account). For api-key: `openVendorAPIKey(acc)` (empty key → Unverifiable with "no API key is set"), then the api-key fetch. Keep the signature.
- **Split `fetchVendorUsage`** (`service_vendor_discovery.go:499-542`): factor the shared tail (record attempt → status check → build `routing.VendorAccountUsage` → **upsert-as-fetched for the api-key path / merge for the subscription path** → return status) from the per-kind fetch. Subscription path unchanged (TokenSet). New `fetchCompatibleVendorUsage(ctx, acc)` opens the key, composes `usageURL = TrimRight(acc.BaseURL,"/") + preset.UsagePath`, dispatches on `preset.UsageVia` to the right vendorauth fetcher via a new injectable seam.
- **Injectable seam**: add a `VendorCompatibleUsageFetcher func(ctx, httpClient, usageURL, apiKey) (vendorauth.OpenAISubscriptionUsage, vendorauth.DiscoveryStatus)` field(s) to `VendorModelDiscoverers` + `withDefaults` defaults (so service tests never hit the network), mirroring the `OpenAICompatibleAPIKey` discoverer seam.
- **`RefreshVendorAccountModels`** trailing `refreshVendorUsage(ctx, acc, tokens)` (`:262`): dispatch by auth type so an api-key account runs the compat usage path (it opens its own key; the zero `TokenSet` is unused).
- **`VendorUsageRefreshUnsupported` detail string** (`:64`) + the "only an OpenAI subscription" doc comments: reword (now also OpenRouter/Kilo).

## 7. Frontend (`gateway/frontend/src/`)

- **`hasActiveUsagePull`** (`VendorAccountUsagePanel.tsx:27-35`): widen to also return true for `auth_type==='api_key' && (vendor==='openrouter' || vendor==='kilo')`, so those accounts get the Refresh button + the one-shot on-view lazy refresh. The panel already renders for api-key accounts (`usageCapable`); only this gate widens. The two ids are hardcoded in the frontend (the backend registry is authoritative for the actual fetch; a drift only mis-shows a button). **No TS type change, no new wire field.**
- The existing `VendorAccountUsageRows` renders the mapped fields as-is (spend row for OpenRouter; balance/unlimited row otherwise). Optional: a short i18n note that OpenRouter figures are per-key — decide during implementation (may be purely docs).

## 8. Scope boundaries

In: OpenRouter + Kilo usage via refresh + on-view lazy refresh; registry `UsageVia`/`UsagePath`; two vendorauth fetchers; portal split + widened gates; frontend gate.

Out (follow-up / other sub-projects): OpenRouter `usage_daily/weekly/monthly` + free-tier counters (need new columns); x.ai Management-API usage (second credential); Gemini (no API); native Gemini dialect (C/#206); usage for Custom.

## 9. Testing & verification

- **vendorauth** (`usage_test.go` style): OpenRouter parse — capped key (limit/remaining/used/pct/status), unlimited (`null` → status unlimited, spend empty), explicit-null-vs-absent, wrong-typed fields degrade, 401 → Unverifiable, no credential leak, body-cap; Kilo parse — `{balance}` → credit_balance/status, missing/garbled → Unverifiable.
- **portal**: via the injectable seam — OpenRouter capped/unlimited produce the right `VendorAccountUsage` + upsert; Kilo best-effort + graceful degrade; api-key refresh branch (before OAuth tokens); `hasActiveVendorUsagePull` true for openrouter/kilo api-key and **false** for native openai/anthropic api-key and all subscription-but-OpenAI unchanged; upsert-as-fetched (no stale-limit merge); the model-refresh trailing usage pull runs for api-key.
- **frontend**: the Refresh button + panel appear for an openrouter/kilo api-key account and not for xai/google/custom; the usage rows render a mapped OpenRouter snapshot (spend) and a Kilo snapshot (balance); vitest + build + lint + **format:check**.
- **Gates**: Go build/vet/`go test ./...` + golangci; **no Postgres leg** (no schema change — note it explicitly in the PR); frontend gates; `check-docs`; SonarQube branch-findings = 0.

## 10. Docs
`external-vendor-accounts.md` §4.4 (UsageVia now filled) + §5.1/§5.2 (OpenRouter/Kilo usage, the per-key and best-effort caveats), `api-surface.md` (refresh endpoint now supports openrouter/kilo), the stale `VendorAccountUsageDTO` "percentages only / no N of M" comment, Risks (Kilo undocumented endpoint; OpenRouter per-key-not-account; our-key-type-on-Kilo unconfirmed), and **ADR-053** (api-key usage pull extending the OAuth-only path; per-preset `UsageVia`/`UsagePath` strategy; upsert-as-fetched for the single-writer compat path; Kilo best-effort acceptance).

## 11. VERIFY-LIVE at implementation
- OpenRouter `/v1/key` exact JSON (field names/nullability, whether `budgets[]`/`effective_budget` are present, the `limit_reset` value set) — docs-derived; confirm against a real response if a key is available, else keep the tolerant parser + mark REVERSE-ENGINEERED like the existing usage parser.
- Kilo `/api/profile/balance`: whether the stored api-key type authenticates it at all, and the exact response shape — keep it best-effort regardless.

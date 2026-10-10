# Design — OpenAI-compatible vendor providers (registry + generic base_url)

Date: 2026-10-10
Branch: `feat/openai-compatible-vendors` (off `main` 585384f)
Status: approved (2026-10-10). §11 resolved — **Variante 1**: Gemini is included in A via the default-`/v1` `OpenAIPathPrefix` change (§4.2a).
Part of: "support more providers" decomposition — this is **sub-project A** (the enabler). Usage/credits display is **sub-project D** (separate spec); native Gemini dialect is **C**; the Gemini *inbound* endpoint is issue #204. Antigravity and Cursor are **excluded** (no usable/ToS-compliant third-party inference API — verified).

## 1. Goal

Let a user connect additional **hosted, OpenAI-compatible** AI providers as vendor accounts ("Anbieter") and route to them — specifically **x.ai (Grok)**, **OpenRouter**, **Kilo Gateway**, and **Google Gemini via its OpenAI-compatible shim** — plus an open-ended **"Custom (OpenAI-compatible)"** option for anything else. All four speak OpenAI `/v1/chat/completions` with a Bearer key, so the existing `OpenAICompatibleClient` already handles the wire; the only gap is that a vendor's endpoint is hardcoded per-vendor in the resolver with no stored base URL.

## 2. Why the vendor-account system (not servers/adapters)

These are hosted, per-user, credentialed services exactly like the existing api-key OpenAI/Anthropic vendor accounts, so they belong in the **vendor-account** feature (`routing.VendorAccount`): per-user ownership, per-token `vendor_access` opt-in, `ModelPrefix`, the usage machinery, and the `vendor_accounts` experimental flag all apply for free. The self-hosted `AIServer`/`Application` path is admin-wide and host:port-shaped (wrong UX); the `Provider*` adapter kinds are a fixed dispatch enum, not user entities.

## 3. Decisions (from brainstorming; user approved "take the recommendation")

1. **Scope:** x.ai, OpenRouter, Kilo, Gemini-via-OpenAI-shim + a Custom option. Exclude Antigravity, Cursor.
2. **Per-user** vendor accounts (consistent with the existing "Anbieter").
3. **api-key auth only** (Bearer). No OAuth/subscription for these (the subscription path is reverse-engineered/ToS-restricted — not a model for new providers).
4. **Provider-preset registry** (data-driven, the OmniRoute idea ported to Go) + a stored `base_url` — not 4 bespoke hardcoded branches, and not a pure free-form base_url (provider **identity** is needed so sub-project D can fetch provider-specific credits/usage).
5. **Gemini via the OpenAI-compat shim** now; native Gemini dialect is sub-project C later.
6. **Usage/credits (D) is out of this spec** but the design leaves the seam: the registry has a slot for a per-provider usage fetcher, and new providers simply show no usage until D fills it in (the display is already availability-aware — it shows only what a vendor reports).

## 4. Data model

### 4.1 New vendor identities (`routing` constants)
Add OpenAI-compatible vendor ids alongside `VendorOpenAI`/`VendorAnthropic`:
`VendorXAI = "xai"`, `VendorOpenRouter = "openrouter"`, `VendorKilo = "kilo"`, `VendorGoogle = "google"` (Gemini OpenAI-shim), and `VendorOpenAICompatible = "openai_compatible"` (the Custom option). A predicate `IsOpenAICompatibleVendor(v)` returns true for these five (NOT for `openai`/`anthropic`, which keep their bespoke dialect paths).

### 4.2 New `VendorAccount.BaseURL` column
Add `BaseURL string` to `routing.VendorAccount` + a new `api_tokens`-style migration (`addColumnIfMissing`, `text not null default ''`) on the vendor-accounts table. Semantics: the upstream **root** URL for an OpenAI-compatible account (`""` for the bespoke `openai`/`anthropic` accounts). **Stored as a root, NOT including `/v1`** — the provider client composes the full URL as `endpointURL(BaseURL, prefix+"/chat/completions")` (`provider/client.go:101` is `TrimRight(endpoint,"/")+path`; the chat path is the literal `/v1/chat/completions` at `openai_compatible.go:60,662` and discovery defaults to `/v1/models` at `:193`). So e.g. x.ai stores `https://api.x.ai`, OpenRouter stores `https://openrouter.ai/api`. This is a **store change → the Postgres leg runs** for this work; memory/SQLite/Postgres conformance must include the column.

### 4.2a Configurable OpenAI path prefix (for the Gemini shim + odd-path Custom)
The Gemini OpenAI-compat shim serves chat at `…/v1beta/openai/chat/completions` and models at `…/v1beta/openai/models` — it does **not** fit the hardcoded `/v1` prefix. To include it (and to make "Custom" genuinely general), add an optional `OpenAIPathPrefix string` to `routing.Target` (default empty ⇒ `/v1`, so **every existing caller is byte-identical**) and have `OpenAICompatibleClient` build chat/discovery as `endpointURL(Endpoint, prefix+"/chat/completions")` / `prefix+"/models"`. The prefix is **not a new column** — it is looked up from the preset registry by `acc.Vendor` at resolve time (`/v1` for all presets except `google` = `/v1beta/openai`; Custom defaults to `/v1`). This touches the shared client + `Target` (the self-hosted vLLM/ollama-openai and existing vendor-OpenAI paths run through the same client), so the default-`/v1` guard and a no-regression test on the existing targets are load-bearing. (Alternative if this shared-client change is unwanted: drop `google` from A and deliver Gemini only in sub-project C as a native dialect — see §11.)

### 4.3 Provider-preset registry (Go data)
A table (e.g. `internal/portal/vendor_presets.go`) keyed by vendor id:
```
preset{ id, defaultBaseURL (root, no /v1), pathPrefix (default /v1), apiFlavor (=openai),
        validatePath (how to verify a key), usage (reserved for D) }
```
`defaultBaseURL` is the **root** (client appends `pathPrefix+"/chat/completions"`); discovery is `GET {root}{pathPrefix}/models` unless noted. Entries (roots + paths VERIFY-LIVE at implementation):
- `xai` → root `https://api.x.ai`, prefix `/v1`, validate `GET {root}/v1/models`.
- `openrouter` → root `https://openrouter.ai/api`, prefix `/v1`, validate `GET {root}/v1/auth/key` (a public `/v1/models` 200s on a bad key — validate against the key endpoint).
- `kilo` → root + prefix VERIFY (likely `https://api.kilo.ai…` + `/v1`), validate `GET {root}/v1/models` or the Kilo-documented endpoint.
- `google` → root `https://generativelanguage.googleapis.com`, prefix `/v1beta/openai`, validate `GET {root}/v1beta/openai/models`, Bearer key.
- `openai_compatible` (Custom) → no default root (user supplies, no `/v1`); prefix `/v1`; validate `GET {root}/v1/models`; no provider-specific usage.
The preset is a **create-time convenience + a behavior key**: it defaults the root, carries the path prefix, and selects the validate/discovery/usage behavior. The stored account keeps `Vendor` (the id) + `BaseURL` (resolved root, user-overridable, required for Custom). The prefix is registry-derived at resolve time, not stored.

## 5. Routing (resolver)

In `vendorAccountTarget` (`routing/resolver.go`), add a branch before the openai/anthropic switch: when `IsOpenAICompatibleVendor(acc.Vendor)`, build
`Provider = ProviderVendorOpenAI` (reuses the existing `OpenAICompatibleClient`, no `main.go` change), `Endpoint = acc.BaseURL` (the stored root), `OpenAIPathPrefix = preset(acc.Vendor).pathPrefix` (§4.2a; empty ⇒ `/v1`), Bearer auth (`APITokenHeader = ""`, `APIToken = acc.APIKey`), `APIFlavors = [openai, anthropic]` with **translate** (zero endpoint modes — no Responses/Messages passthrough; those are OpenAI/Anthropic-native only). The existing `openai`/`anthropic` branches are unchanged (they leave `OpenAIPathPrefix` empty → byte-identical). `VendorAccountID` is set for usage attribution as today.

Result: an inbound OpenAI-chat (or translated Anthropic-messages) request to one of these accounts is dispatched to `{BaseURL}{prefix}/chat/completions` with the account's key (`https://api.x.ai/v1/chat/completions`, `https://generativelanguage.googleapis.com/v1beta/openai/chat/completions`, …).

## 6. Create / validate / discover (service)

- **Create/update** (`service_vendor_accounts.go`): extend the accepted-vendor validation switch to the five new ids; accept + validate `BaseURL` (required for `openai_compatible`; defaulted from the preset otherwise; must be a well-formed https URL — reuse/add a small validator). api-key only (reject subscription for these).
- **Credential validation** (`service_vendor_validation.go` + `vendorauth`): per preset — probe the validate endpoint with the Bearer key; map 200 → valid, 401/403 → bad key, else → unverifiable (mirror the existing openai/anthropic probe behavior).
- **Model discovery** (`vendorauth/discover.go` + `service_vendor_discovery.go`): `GET {root}{prefix}/models` (prefix from the preset; `/v1/models` for most, `/v1beta/openai/models` for Gemini), Bearer, parse the OpenAI `{data:[{id}]}` list into `VendorAccountModel` rows; apply the account's `ModelPrefix` as today (`relabelVendorModels`). Aggregators (OpenRouter/Kilo) return hundreds — store them all; the per-token `vendor_access` + prefix already scope what each token sees.
- **Static seed catalog** (`vendor_catalog.go`): minimal or empty for aggregators (dynamic discovery is the source of truth); optional small seed for x.ai/Gemini so a freshly-created account has something before the first discovery.

## 7. Frontend

`VendorAccountsView.tsx`: the `VENDORS` enum + create form gain the new providers as a **dropdown** (x.ai, OpenRouter, Kilo, Gemini, Custom), with a **base-URL field** shown for Custom (and editable/prefilled for the presets). `VendorAccount['vendor']` type in `api.ts` + i18n labels (de/en). The rest of the view (model list, prefix, per-token access, status) works unchanged.

## 8. Scope boundaries

In: routing to the four providers + Custom via vendor accounts; `base_url` column + preset registry; create/validate/discovery; frontend; model catalog; the `vendor_accounts` flag + per-token `vendor_access` + `ModelPrefix` apply automatically.

Out (separate work):
- **Usage/credits/limits display** — sub-project D (the registry's usage slot is the seam).
- **Native Gemini dialect** (generateContent, Gemini-only features) — sub-project C.
- **Gemini inbound endpoint** — issue #204.
- **Antigravity, Cursor** — excluded (no usable/compliant API).
- OAuth/subscription for these providers.

## 9. Testing & verification

- Routing: an OpenAI-compatible account resolves to `ProviderVendorOpenAI` with `Endpoint == acc.BaseURL`, the preset's `OpenAIPathPrefix`, Bearer auth, translate modes; existing openai/anthropic targets byte-identical (no regression in `vendorAccountTarget`).
- Client/path (§4.2a): with `OpenAIPathPrefix == ""` the composed chat+discovery URLs are byte-identical to today (self-hosted vLLM/ollama-openai + existing vendor OpenAI unchanged); with `/v1beta/openai` the Gemini-shim URLs come out right. A table test over {default, xai, openrouter, google, custom} asserts the exact composed URL.
- Store: `BaseURL` round-trips (memory + SQLite + **Postgres leg** conformance); migration additive.
- Service: create/validate for each preset (valid key, bad key, bad/missing base URL for Custom); discovery parses `/v1/models`; `ModelPrefix` applied.
- Frontend: the dropdown + Custom base-URL field; create round-trip; vitest + build + lint + **format:check**.
- Docs: `external-vendor-accounts.md` (new providers + base_url + the preset registry + the "translate-only, api-key-only" matrix), `api-surface.md`/`openapi.yaml` (the `base_url` field + new vendor values), an ADR, `config-env` if any.
- Gates: Go build/vet/`go test ./...` + golangci; **Postgres leg** (store change); SonarQube branch-findings = 0.

## 10. Confirm at implementation (VERIFY-LIVE)
- Exact **root** URLs + path prefixes + discovery/validate endpoints for x.ai, OpenRouter, Kilo, Gemini-shim (and the OpenRouter `/v1/auth/key` validate quirk). Kilo's base + prefix especially.
- The vendor-accounts table name + the latest migration number for the `base_url` column.
- Whether `ModelPrefix`/relabel + the per-token `vendor_access` collision checks need any adjustment for aggregator-sized catalogs (hundreds of models).

## 11. Resolved decision — Gemini in A (Variante 1)
**Decided (2026-10-10): Variante 1.** Gemini is delivered in A via the §4.2a default-`/v1` `OpenAIPathPrefix` change to `Target` + `OpenAICompatibleClient`. This ships all four named providers (x.ai, OpenRouter, Kilo, Gemini) + Custom in A and makes "Custom" accept any path prefix. The cost — a small, default-guarded change to the shared client that the self-hosted (vLLM/ollama-openai) and existing vendor-OpenAI paths also traverse — is accepted; the no-regression test over the existing targets (§9) is load-bearing.

Antigravity and Cursor stay excluded; sub-project C remains available later for native-only Gemini features (`generateContent`, Gemini-specific params).

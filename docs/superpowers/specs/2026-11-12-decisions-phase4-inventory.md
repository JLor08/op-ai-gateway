# Phase 4 touch-point inventory (main cc2410ca)

This file is branch-local. BE stands for gateway/backend/internal. Line numbers are hints; confirm each one before editing.

## Vendor routing
- `Resolve` BE/routing/resolver.go:800. Under `vendor_first`, `resolveVendorAccount` runs before standard routing (:833-840). Under `fallback_only` it runs only on ErrNoModelRoute (:841-848).
- `resolveVendorAccount` :1150 (doc :1105-1149; the RequiredCapabilities doc bullet is at :1126-1128). Per account, in order:
  - skip inactive (:1161);
  - skip subscriptions when off (:1170);
  - apply the `TokenVendorPrefix` deny (:1180);
  - skip OpenAI subscriptions when the coarse flavor ≠ openai (:1198);
  - call `vendorAccountModelMatch` (:1224), which compares `prefix + UpstreamModel == req.Model` (name only; first row wins).
- Guard `vendorAccountRoutingEligible` resolver.go:1234-1244: `vendorOn && vendorPrincipalEligible && ServerOverrideID == "" && len(RequiredCapabilities) == 0 && apiFlavor != images`.
- `vendorAccountModelTarget` :1280. `vendorAccountTarget(acc, m, model, apiFlavor, fineFlavor) Target` :1322:
  - Provider `vendor_openai`, endpoint `https://api.openai.com` (presets use acc.BaseURL plus a path prefix).
  - APIToken is sealed. VendorAccountID is set. `APIFlavors [openai, anthropic]`.
  - ResponsesMode passthrough only for OpenAI + openai_responses (:1381).
  - DecisionsMode and SystemOneMode are "".
- Subscription targets: :1442 / :1507. Constants: `vendorAccountDefaultTimeout = 120s` (:670), `vendorRoutePrefix = "vendor:"` (:675).
- Providers: BE/routing/store.go (:60 ProviderVendorOpenAI, :61 Anthropic, :73 OpenAISubscription, :77 Gemini). `IsOpenAIVendorProvider` :149. Vendor ids :278-291. Auth types :293-294. Preset helpers: BE/routing/vendor_presets.go.
- §11 access: `TokenVendorPrefix` BE/routing/vendor_access.go:42; `TokenOwnsVendorAccount` :14; listing BE/portal/service_vendor_listing.go:114 (`advertisedVendorAccountModels`) and :67 (`ownVendorAccountModels`); grant BE/portal/service_vendor_sharing_grant.go:50; save check BE/portal/service.go:5961/6029; policy type BE/auth/vendor_access.go:10-25. Docs: external-vendor-accounts.md:2582-2776.
- Upstream call:
  - `proxyNative` works with vendor targets: auth comes through `upstreamAuthCtxWithHeaders` (native_passthrough.go:747, server.go:2371/2401).
  - The URL is `endpointURL(Endpoint, path)` (BE/provider/client.go:101) with no OpenAIPathPrefix.
  - For an OpenAI api-key target, `/v1/decisions` resolves to `https://api.openai.com/v1/decisions`.
  - Clients are set in cmd/gateway/main.go:1371 (vendor_openai → OpenAICompatibleClient).
- Catalog:
  - `VendorAccountModel{AccountID, GatewayModel, UpstreamModel, APIFlavor, DisplayName}` (store.go:376-382; `api_flavor` is unread). There are no capability rows for vendor models.
  - The static seed BE/portal/vendor_catalog.go:32-33 has no gpt-6-luna.
  - Discovery is in BE/vendorauth/discover.go:135 (api-key GET /v1/models), filtered by `isChatCapableOpenAIModel` (BE/portal/service_vendor_discovery.go:920, markers :908).
  - Listing flavors come from `vendorAccountServedFlavors` (service_vendor_listing.go:234; never `decisions`). `ModelOffering` (BE/portal/service_model_offering.go:316-323) adds vendor names only for an asked flavor in the set, and `capableNames` reads mapping views only.
- Exclusion tests:
  - `TestSystemOneIsNeverVendorRoutable` (BE/gateway/systemone_handler_test.go:404)
  - `TestVendorAccountSkippedForSpeech` (BE/routing/resolver_capability_gate_test.go:830)
  - `TestVendorAccountSkippedForImagesCapabilityAndOverride` (BE/routing/resolver_vendor_account_test.go:241)
  - `TestDecisionsInferenceShape` (decisions_handler_test.go:264)

## Decisions and System One handlers
- `/v1/decisions`:
  - Shape at BE/gateway/decisions_handler.go:54 (`[any_decision]`).
  - `relayDecisions` :129 → `resolveDecisionsTarget` (systemone_handler.go:199: `targetServesFlavor`, then `decisionsRoute`).
  - `decisionsRoute` :172 → `decisionVerdicts` :196 (`s.Routes.MappingCapabilities(ctx, target.RouteID)`; empty for `vendor:` route ids) → `routing.EffectiveDecisionsRoute(target.EffectiveKind(), …)` (BE/routing/decisions_route.go:36).
  - The translate branch uses `decisionsRewrite` (:147-161).
- `Target.EffectiveKind()` resolver.go:322 → Provider for vendor targets (`vendor_openai`), which counts as capable.
- `/v1/systemone`:
  - Shape at systemone_handler.go:74 (`[decision]`).
  - `relaySystemOne` refuses disabled at :232; normalization at :246-248.
  - `handleSystemOneModels` :259 filters `m.Decision`.
- `upstreamPath` native_passthrough.go:237-253: System One always maps to /v1/systemone (:239-241). openai_decisions maps to /v1/systemone under translate, otherwise /v1/decisions.
- `systemone_mode`:
  - Validator `validSystemOneMode` BE/portal/service_applications.go:2050; `resolveDecisionModes` :2063 (default passthrough :2072); create :2086; update :1070; runtime spec service_runtime.go:1515.
  - Reads: `applicationServesEndpoint` BE/routing/store.go:3637-3641; `MappingServesDecisionEndpoint` BE/routing/served_flavors.go:187; target fill resolver.go:2943.
  - Folds: `decisionFlagsByName` service_model_offering.go:438; `ModelDTO.Decision` service.go:1481/1485/3370.
  - The passthrough guard pattern is `ErrApplicationDecisionsPassthroughUnsupported` / conflict / retype reset.
- compat/decisions.go:
  - `TranslateDecisionsRequest` :130 and its helpers (:168–:516).
  - `TranslateSystemOneResponse` :618 and its helpers (:655–:803).
  - `NormalizeDecisionsConfidence` :867; `ParseDecisionsUsage` :980.
  - Types: `DecisionsError` codes :22-25, `DecisionsUsage` :58, `DecisionsDialect` :66-73, `DecisionsAnswerTable` :79; output structs :557-611.
- decisions_response.go:
  - `decisionsRewrite{table, clientModel, normalize, dialect, failCode}` and `apply` (:45/:64) hardcode `TranslateSystemOneResponse` and `DialectOpenAI`.
  - `relayDecisionsResponse` :107; `decisionsUpstreamErrorBody` :178 (its message says "System One upstream"); `decisionsInferenceUsage` :87.

## Usage
- There is no per-model pricing; cost is energy × the host price (BE/portal/service.go:2587), so vendor rows cost 0.
- `Event.CachedTokens` and `CacheWriteTokens` live in BE/usage/recorder.go:51-52; the split is at inference_complete.go:747.
- `mergePassthroughUsage` reads `openai_decisions` through `mergeResponsesUsage` (native_passthrough.go:1646-1660, cache-write at :1695-1698).
- `scrapeVendorAccountUsage` is a no-op for platform api-key responses.

## Docs stating the vendor exclusion / systemone_mode values
- external-vendor-accounts.md: :533-557 (§4 matrix and skip paragraph), :950-1042 (§4.2 OpenAI; add a Decisions passthrough sub-section), :1880-1888, :2016-2024, :2066-2078.
- compatibility-and-inference.md: §3.9 :1586-1589 (exclusion; also names a non-existent `resolveSystemOneTarget`, the real one is `resolveDecisionsTarget`); :1727; §3.10 :1794-1795, :1804-1810; the modes table :2351-2399; the error table :4613.
- routing-and-model-selection.md: :386, :1129, :1135, :1203, :1233.
- ADRs: ADR-097 (b) :11899 and :11859; ADR-098 (b) :12024 and (c) :12027-12028; ADR-042 :1899; ADR-061 :5196; ADR-062 :5559. The next ADR is **ADR-100**.
- 11-risks-and-technical-debt.md:58; 12-glossary.md:42; reference/api-surface.md:71-72 and the decision-endpoint-modes section; persistence.md:236.
- Frontend: gateway/frontend/src/components/shared/ApiVariantControls.tsx:53 (`systemOneModeOptions`), api/models.ts:418 (`SystemOneMode`), components/shared/decisionsModes.ts, i18n.ts:1034/5052 (`applicationDecisionsModeNote`).
- Code comments that state the exclusion: systemone_handler.go:60-68, decisions_handler.go:42-48, resolver.go:1126-1128 and :1234-1237.

## Versions
- The latest migration is 108; none is needed. The agent is at 0.18.2; no agent change is needed.

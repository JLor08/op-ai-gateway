# Phase 2 plumbing inventory (as of main f34ec050)

Branch-local working file. Paths relative to `gateway/backend/` (Go), `gateway/frontend/src/` (TS), `docs/architecture/` (docs). Line numbers are hints from f34ec050; confirm before editing.

## Endpoint modes (`responses_mode` / `messages_mode`) — the template for `decisions_mode` / `systemone_mode`

- Type: `internal/routing/endpoint_mode.go:17-23` `type EndpointMode string` (`disabled`, `translate`, `passthrough`). No parser; validation lives in portal.
- `routing.Application` fields: `internal/routing/store.go:904-913`; live-timings sibling flag :950-960.
- Candidacy: `applicationServesEndpoint` store.go:3572-3586 (ordinary app with disabled mode removed; server_agent never removed here, spec enforced at dispatch). Callers: `filterServesEndpoint` resolver.go:1737, `affinityApplicationStale` resolver.go:1776-1780, `MappingServesAPIFlavor` served_flavors.go:150-156.
- Validator: `internal/portal/service_applications.go:1946-1957` `validEndpointMode`. Sentinel `ErrApplicationEndpointModeInvalid` :39-42 → HTTP 400 at `internal/gateway/portal_application_endpoints.go:148`.
- Create `CreateApplication` :665 (absent/"" → passthrough at :689-707, assigned :793-794). Update `UpdateApplication` :915 (validate :1005-1023, nil keeps, "" rejected; apply :1106-1111).
- Per-type refusal template (live timings): `routing.LiveTimingsCapableKind` routing/live_timings.go:102; `ErrApplicationResponsesLiveTimingsUnsupported` (400) service_applications.go:79-98 (create :871-880, update :1351-1358); `...Conflict` (409) :99-113 (400 when request sent `type`, 409 when the stored type); update clears flag when the type cannot carry it :1208-1211; HTTP mapping portal_application_endpoints.go:200-215.
- Runtime specs (`internal/portal/service_runtime.go`): `ErrRuntimeSpecEndpointModeInvalid` :89-91 → 400 at `internal/gateway/portal_runtime_endpoints.go:143`; `runtimeSpecEndpointModes(req)` :1467-1488; called from `validateRuntimeSpecRequest` :1209; `runtimeSpecFields` :1169-1190.
- Migrations: Go code in `internal/store/migrate.go`; registry slice :33-142 (`{version, name, up}`), append only; `addColumnIfMissing` :285; same code for sqlite/postgres via `dl`. Migration 72 `application_endpoint_modes` (func :3277-3357) is the template (columns on `applications` and `agent_runtime_specs`). Latest is **106**; next free **107**.
- Store I/O: `internal/store/sqlite_applications.go` (CreateApplication :17, UpdateApplication :79, ApplicationByID :145, ApplicationsByServer :160, ActiveMappingsForModel :666/:674, scanMappingCandidate :728/:756, scanApplication :822/:846); `internal/store/sqlite_runtime.go` (UpsertRuntimeSpec :26/:36/:55/:80, `runtimeSpecCols` :102-120, scanRuntimeSpec :469/:478).
- Column-pinning tests: store/application_column_parity_test.go (:351-372 allow-list, :50-70, :196), store/runtime_spec_column_parity_test.go (:40-50, :180, :269-280), store/sqlite_migration_test.go:247 (migration count), conformance_test.go, routing_store_conformance_test.go.
- RuntimeSpec fields: routing/store.go:3066-3085. `EffectiveFields(app, spec, hasSpec) EffectiveSpecFields` routing/served_flavors.go:8-48 (struct :13-18). `Resolver.targetFrom` resolver.go:2833 → copies onto `Target` :2873-2879 (Target fields :226-235). Vendor targets force passthrough: resolver.go:1307/1367/1384/1452/1516. Other EffectiveFields callers: portal/service.go:3775, portal/service_mapping_images_only.go:78-95, gateway/benchmark_refusals.go:207-235, gateway/benchmark_speech.go:243, cmd/gateway/app_health.go:1981.
- Portal spec I/O: `RuntimeSpecDTO` service_runtime.go:530-537, `PutRuntimeSpecRequest` :692-699, `runtimeSpecRow` :1545 (modes :1621-1623), `runtimeSpecDTO` :2132 (:2189-2191), `putRequestFromDTO` service_runtime_benchmark.go:46 (:71-73).
- Dispatch: native_passthrough.go constants :37-45 (`codeResponsesEndpointDisabled`, `msgEndpointDisabled`, `statusEndpointDisabled` 404), `endpointDisabledError` :47-54, `endpointModeFor` :56-96, `targetServesFlavor` :98-127, `upstreamPath` :204 (System One branch :236-241), `relayPassthroughTarget` :375-441 (disabled → 404 + failed usage row :412-429).
- App DTOs: service_applications.go `ApplicationDTO` :471-475, `CreateApplicationRequest` :549-550, `UpdateApplicationRequest` :603-604 (`*string`), `applicationDTO()` :1453/:1474-1475.
- Frontend: api/models.ts `EndpointMode` :377, `PortalApplication` :61-62, create :109-110, update :154-155; api/runtime.ts:289-291; components/shared/ApiVariantControls.tsx (flavors :22-39, `endpointModeOptions` :40-49, selects :121-146); ApplicationSection.tsx (:271-272, :359-360, :393-394, :425-426, :471-472, :953-954); RuntimeAdminSection.tsx (:307-308, :2751-2752, :2936-2937, :2985-2986, :3040-3041, :3343-3344, :3442-3451, :4888-4889); shared/applicationTypeDefaults.ts (:58-59, :86-159, sd/audio 'disabled' :183-184, :209-210); shared/runtimeSpecTypeFlavors.ts (:24-25, :35-36, :152-153); shared/runtimeSpecTemplate.ts (:12-13, :45-46); i18n.ts DE :999-1028, EN :4985-5013.
- Docs: ADR-033 (09-architecture-decisions.md:580); api-surface.md :1218-1290 (mode section), :1351-1354 (error codes); data-model.md :46, :60, migration table (row 106 at :645); routing-and-model-selection.md :300, :336; compatibility-and-inference.md §3.9 :1535, endpoint table :151-152; openapi.yaml `/v1/systemone` :837-884, schemas :5138-5168.

## Per-token boolean (`claude_effort_compatibility` / `claude_model_aliases`) — the template for `decisions_confidence_normalize`

- Migration 89 `token_claude_effort_compatibility` (migrate.go:124, func :4051-4059: `addColumnIfMissing(..., "api_tokens", "claude_effort_compatibility integer not null default 0")`); test store/migration89_token_claude_effort_compatibility_test.go.
- `store.TokenRecord` models.go:184-190; sqlite_token.go (CreatePlainToken :18/:54/:82, `tokenColumns` ~:118, UpdateTokenMetadata :211/:229/:234, LookupBearer :287 → auth.Token :375, scanToken :387/:397/:425/:445).
- `auth.Token` internal/auth/token_store.go:80-83 (in-memory store copies the struct).
- Portal: service.go `TokenDTO` :1245-1251, `CreateTokenRequest` :1320-1322, `UpdateTokenRequest` :1362-1364 (`*bool`), `CreateToken` :1888-1898/:1970, `UpdateToken` :1979 (guard :1991-2001, apply :2074-2076), `AuthorizeRunAsToken` :5712/:5744, `tokenDTO` :5755/:5781. Management-only gate `principalMayManageTokenSetting` authz.go:58-80 (NOT used for the new flag; `claude_model_aliases` is the ungated sibling). Service tokens service_services.go :159-162, :193-195, :810, :1124. memory_directory.go :159, :218, :307, :337.
- Runtime read: straight from the principal after `requireAnyScope` + `runAsPrincipal` (see `wantsChatTemplateEffort` native_effort_injection.go:57-61).
- Frontend: api/tokens.ts :85-90, :125, :160; api/services.ts :115, :140; TokenList.tsx (:172, :296, :331, :382, :416, checkbox :764-771); ServiceTokensSection.tsx (:159, :209, :241, checkbox :520-527); i18n `tokenClaudeEffort*` DE :3357-3358, EN :7185-7186; tests TokenList.test.tsx:1055-1095, ServicesView.test.tsx:1143-1165.
- Docs: api-surface.md :107, :182, :217, :247, :1704; data-model.md :27, :515; persistence.md:214-215; security-auth-rbac.md:218; openapi.yaml :110, :1160-1270, :3015.

## Capability gate

- `filterCapable` routing/resolver.go:1661 → `capabilityRowsSatisfy(rows, required)` :1690 — the ONE funnel (also used by `affinityCapabilityVerdict` :1844). `image_edit` is the precedent for a derived requirement.
- `reservedManualVerdicts` portal/service_applications.go:295 (decision:no at :391); capability-name switch portal/service_model_offering.go:390/:416; fold `decisionFlagsByName` portal/service.go:2947.

## Probe pass (Phase 1)

- `provider.SystemOneProber` internal/provider/systemone_probe.go (body, verdict rules, router-404 transient, refusal sentinel).
- cmd/gateway/app_health.go: `startSystemOnePasses` :1895, `systemOneView` :1937, `systemOneCandidateFor` :1976, `probeSystemOne` :1995, `probeSystemOneMapping`, `systemOneDue`, `decisionRowOnFile`, `settleSystemOne`, `systemOneCapabilityRows` :1858, `systemOneProbeKey` :1871; runner fields :334-349; constants :51-69.

## Buffered response template

- `relayTranscriptionResponse` internal/gateway/transcription_handler.go:1329 (read bounded, decide, write; usage + capture) and `failTranscriptionRelay`; branch point in `proxyNative` native_passthrough.go:~843.
- Usage: `mergePassthroughUsage` native_passthrough.go:1618, `mergeResponsesUsage` :1636 (reads `input_tokens_details.cached_tokens`), `inference.Usage.CachedTokens`/`CacheWriteTokens` inference/types.go:86-94; `usageMeta` inference_complete.go:677; recordUsage recomputes `ProviderPath` from `upstreamPath(target, req.APIFlavor, meta.Endpoint)` inference_complete.go:780.

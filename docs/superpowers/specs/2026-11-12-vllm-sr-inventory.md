# Phase 3 touch-point inventory (main 84f8f949)

Branch-local. GB = gateway/backend, FE = gateway/frontend/src, SA = server-agent. Line numbers are hints; confirm before editing. Precedent: the `audio_cpp` app + spec type (commit fc35beb0, ADR-061) — it touched no server-agent file and added no migration.

## Application type
- Constant block GB/internal/routing/store.go:19-44 (`ProviderAudioCpp = "audio_cpp"` :44 with a namespace doc comment :36-43).
- `normalizeApplicationType` GB/internal/portal/service_applications.go:1699-1720 (one case per type); `CreateApplication` :690/:698.
- Timeout default: `applicationTimeoutDefaultFor` :2127-2138 (`defaultAudioCppTimeoutMS = 600000`, constants ~:424-460). Health path default is type-agnostic `/v1/health` (:424, :1670) — per-type health paths are frontend defaults.
- Model discovery: `modelDiscoveryFor(providerType, pathPrefix)` GB/internal/provider/openai_compatible.go:194-199 (only sd has a case), `ListModels` :268/:274, `decodeOpenAIModelList` :227 fits `{"object":"list","data":[{"id",...}]}`.
- Provider clients map GB/cmd/gateway/main.go:1345-1365 (`routing.ProviderAudioCpp: openAICompatible` :1364). Multiplexer dispatch by target.Provider (GB/internal/provider/multiplexer.go 217/401/432).
- `viewRuntimeType` GB/internal/portal/service.go:3670-3686 (app type → spec type; audio :3682).
- `DeriveRole` GB/internal/routing/model_role.go:95-121 (vllm_sr → RoleOther without a case; fine).
- Decisions passthrough guard: `decisionsPassthroughCapableKind(kind)` service_applications.go:2053-2058 (`kind != ProviderLlamaCPP`), used for app types AND spec kinds (same strings); call sites :2092, :1171, :1422, service_runtime.go:1320. Tests: portal/service_decision_modes_test.go, gateway/portal_application_endpoint_mode_test.go, gateway/portal_runtime_endpoints_test.go.
- decision_openai probe scope `openAIDecisionsInScope` GB/cmd/gateway/app_health.go:1998-2010 (vllm app; vllm/custom spec) — excludes new types automatically IF detection resolves `vllm-srun` to the new type. Test app_health_decisions_probe_test.go:61.
- System One pass needs the app's APIFlavors to include `decisions` (app_health.go:1917/:1925); backend flavor default is [openai, anthropic] for every type (portal normalizeFlavors :1992-2012) — per-type flavor defaults are frontend.
- Type-enumerating Go tests: portal/service_applications_test.go:625; provider/live_progress_kind_parity_test.go:97-104; routing/runtime_spec_type_test.go (Detect :8, DeriveProbePaths :181, per-type tests :121, :168). Live-timings/live-progress allowlist doc comments count the type strings (routing/live_timings.go:69-71, provider/live_progress.go:70-73).

## Runtime spec type
- GB/internal/routing/runtime_spec_type.go: constants :20-28; `DetectRuntimeSpecType` :41-60 (order vllm, llama, tgi, sd, audiocpp, ollama, custom — `vllm-srun` currently detects as vllm); `EffectiveRuntimeSpecType` :65-70; `DeriveProbePaths` :75-130 (vllm → /metrics, /v1/models; audio → "", "").
- `validRuntimeSpecType` GB/internal/portal/service_runtime.go:1914-1921 ("seven values" doc); error text listing types GB/internal/gateway/portal_runtime_endpoints.go:145.
- Spec health default `/health` already (service_runtime.go:239, :253-258).
- RuntimeWarnings service_runtime.go:2627-2662 (advisory precedent `api_flavors_text_on_stable_diffusion` :2622, `anyStableDiffusionSpecNotImagesOnly` :2750-2760); frontend mapping FE/components/RuntimeAdminSection.tsx ~:255-265.
- Agent wire DTO `AgentRuntimeSpecDTO` service_runtime.go:3019 (`type` :3062, metrics/context paths :3078-3079 from DeriveProbePaths :3375-3376).
- Launch args are operator-entered; agent placeholders `${PORT}`, `${MODEL}`, `${CUDA_DEVICES}`, … (SA/internal/runtime/policy_local.go:584). With set_visible_devices the child sees device 0.

## Server agent
- Learns the type from the wire (`type`, SA/internal/runtime/types.go:115) plus `metrics_path`/`context_probe_path`/`health_path`.
- Readiness: `pollHealth` SA/internal/runtime/manager.go:4206-4250 (GET health_path until 2xx or StartupTimeoutSeconds).
- Context: `extractContext(specType, v)` SA/internal/collector/probe.go:914-927 (vllm data[].max_model_len; default best-effort n_ctx/max_model_len/context_length).
- Metrics: SA/internal/collector/scrape.go:30-37 (running/waiting names, auto-detected by name via firstPresent :112-113; runs only when metrics_path != "").
- Capability probe `probeRuntimeChildProps` SA/internal/agent/agent.go:2057 (default GET /props → 404 → empty set: harmless).
- Version `const Version = "0.18.1"` SA/internal/agent/agent.go:411 (history in the comment above). Agent code change = PATCH bump; a new Features entry = MINOR (TestFeatureRegistry). Gating is by feature name (SA/internal/agent/features.go; GB/internal/gateway/agent_features.go:87).
- Sample channels (SA/internal/sample/sample.go ~:172-256): `metrics_probe`/`context_probe` state strings (`ok`/`na`/`unreachable`/`router`), `last_error`, `capabilities`. Gateway ingest GB/internal/gateway/agent_ingest.go:398-399, GB/internal/gateway/runtime_registry.go:139-140; portal echo GB/internal/portal/service_model_servers.go:60-61. No generic warnings field.

## Frontend
- FE/api/models.ts:6-27 `ApplicationType`; FE/components/ApplicationSection.tsx (options :55-64, labels are raw strings :899, type note :799-801, decision-mode coercion :418/:495/:993); FE/components/shared/applicationTypeDefaults.ts (TypeDefaults :55-76; audio :206-218).
- FE/components/shared/decisionsModes.ts:32-34 `decisionsPassthroughOffered(kind) => kind !== 'llama_cpp'`, `decisionsModeForKind` :41-43.
- FE/api/runtime.ts:304-309 spec type union; FE/components/RuntimeAdminSection.tsx (health default :743-745, type note :750-754, type select :4992-5000, type change :~4985); FE/components/shared/runtimeSpecTypeFlavors.ts (audio defaults :33-37, map :44-47).
- i18n FE/i18n.ts: `applicationTypeAudioCppNote` (987/4995), `runtimeSpecTypeAudioCppNote` (1598/5564), `runtimeSpecTypeAudioCpp` (1253/5247).
- Exhaustive type tests: FE/components/shared/liveTimings.test.ts:54-75 (tsc fails on a missing type), applicationTypeDefaults.test.ts, runtimeSpecTypeFlavors.test.ts, ApplicationSection.test.tsx, RuntimeAdminSection.test.tsx, i18n.test.ts.

## Docs
- routing-and-model-selection.md (:27 type list, :191 defaults tables, :288 provider client table, :342-366 audio_cpp defaults, :1177-1185 decision_openai probe scope); agent-runtime-manager.md (§3.4 :836-880 types/detection/DeriveProbePaths table, :3933 roles, :5895/:5928 capability probe, :6211 benchmark table, :9240 timeouts); compatibility-and-inference.md (§3.9 :1609, :1718-1726, §3.10 :1924-1930, :2320-2340 passthrough refusal, :2532/:2670-2674 shared clients); api-surface.md (:541-556 app types, :1115-1117 spec types, :1216); data-model.md:577; glossary :59; ADR-061 (:5148), ADR-097/098 (next is ADR-099); README.md:134/:221; AGENTS.md:143.

## Migrations
- None needed: `applications.type` and `agent_runtime_specs.type` are plain text, no CHECK/enum. Latest is 108.

# Decision endpoints Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `POST /v1/systemone` (System One passthrough) and `GET /v1/systemone/models` through the gateway, behind a new opt-in coarse flavor `decisions` and a new capability `decision`, written by a gateway-side probe.

**Architecture:** A new buffered JSON relay cloned from the speech relay. It routes through `inferencePreflight` and `proxyNative`, and its target is gated by the flavor and by `RequiredCapabilities=["decision"]`. A decisions-only predicate keeps decision models out of every chat path, in the same style as images-only and audio-only. A new health-loop pass sends one tiny System One request per mapping identity, only to loaded agent models, and writes the verdict under source `systemone_probe`.

**Tech Stack:** Go (gateway/backend, module `op-ai-gateway`), React + TypeScript + Vite (gateway/frontend), Markdown docs.

**Spec:** `docs/superpowers/specs/2026-11-11-decisions-systemone-phase1-design.md` (read it first; issue JLor08/op-ai-gateway#373 is the wider design).

## Global Constraints

- Flavor string `decisions`. Capability string `decision`. Capability source `systemone_probe`. Fine flavor used by the handler: `decisions` (the coarse value itself, as speech uses `openai_audio`).
- Error codes, exact: `systemone.model_required`, `systemone.questions_required`, `systemone.stream_unsupported` (all 400); `benchmark.decisions_only` (409). Existing codes are reused unchanged: `routing.model_not_capable`, `routing.no_model_route`, `request.body_too_large`.
- Env var `OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES`, default `32 << 20`.
- Upstream path `/v1/systemone`. Listing path `/v1/systemone/models`. Both are bearer-only (`requireAnyScope(w, r, scopeGatewayUse, scopeLLMInvoke)`) and never accept the session cookie or the internal loopback.
- Probe body, exact: `{"model":"<app_model_name>","state":"ok","questions":{"probe":{"type":"noul","instructions":"Is the state non-empty?"}}}`.
- **No server-agent change. No `const Version` bump.** No store migration in this phase.
- Repo text is English. i18n keys are added to `de` and `en` together. Run `npm run format:check` before pushing frontend changes. golangci-lint (gofumpt, gocritic) must pass per module.
- Keep compat logic in `internal/compat` (none is needed in Phase 1: the relay is passthrough).

## Review Focus

1. **Mixed flavor lists.** `[openai, decisions]` must behave as chat **and** decisions. `FlavorsAreDecisionsOnly` is false, so the model is listed as chat, warmed and benchmarked, and also answers `/v1/systemone` and is probed. Pin it in Task 1 and Task 2.
2. **`[openai_audio, decisions]` (no text flavor).** Both audio-only and decisions-only are true. The benchmark refusal must be deterministic: the existing images/audio checks run first, and decisions-only comes after them. Pin the code in Task 2.
3. **Probe on an agent model that is not loaded, or unloads between ticks.** There must be no POST, and so no cold start and no idle-timer refresh. A model that is loaded later is probed then. Pin it in Task 6.
4. **Upstream 2xx without `usage`, or with a body larger than the capture cap.** The relay still succeeds. The row records zero tokens, or the tail-recovered usage, and never fails the request. Pin it in Task 4.
5. **Validator edge values.** `model` as a number or `"  "`, `questions` as an array or `{}`, `stream: "false"` (a string): each is a 400 with the right code. `stream: false` and an absent `stream` are accepted. Pin it in Task 4.

---

### Task 1: Routing vocabulary, decisions-only predicate, capability plumbing

**Files:**
- Modify: `gateway/backend/internal/routing/store.go`
  - the flavor const block around :91-111: `APIFlavorDecisions = "decisions"`;
  - the capability block around :1696-1870: `CapabilityDecision = "decision"`;
  - the sources around :1987-1995: `CapabilitySourceSystemOneProbe = "systemone_probe"`.
- Modify: `gateway/backend/internal/routing/served_flavors.go`
  - `FlavorsAreDecisionsOnly`;
  - a `listedEndpoint` case;
  - the `effectiveHasAPIFlavor` openai row, which adds `&& !FlavorsAreDecisionsOnly(...)`.
- Modify: `gateway/backend/internal/routing/score_servers.go:36` (add `decisions` to the flavor list).
- Modify: `gateway/backend/internal/portal/service_applications.go`
  - `normalizeFlavors` whitelist :1922;
  - `reservedManualVerdicts`: add `decision: {no}`.
- Modify: `gateway/backend/internal/portal/service.go:5186` `knownAPIFlavors`. Keep it sorted.
- Modify: `gateway/backend/internal/portal/service_model_offering.go`: `hasCapabilityFold`/`capabilityFlagsByName` learn `decision`. The `ModelDTO` gains `Decision bool` (`json:"decision,omitempty"`), folded like `Speech`.
- Modify: `gateway/backend/internal/gateway/agent_ingest.go:1531` `reservedAgentCapabilityNames` (add `decision`).
- Test: `gateway/backend/internal/routing/served_flavors_test.go`, `score_servers_test.go`, `resolver_test.go` (NormalizeAPIFlavor), `gateway/backend/internal/portal/service_models_audio_test.go` (or a new `service_models_decisions_test.go`), `service_applications_test.go`, `gateway/backend/internal/gateway/agent_runtime_status_ingest_test.go`.

**Interfaces:**
- Produces:
  - `routing.APIFlavorDecisions`, `routing.CapabilityDecision`, `routing.CapabilitySourceSystemOneProbe`;
  - `func FlavorsAreDecisionsOnly(flavors []string) bool`;
  - `portal.ModelDTO.Decision bool`.

- [ ] **Step 1: Write failing tests.**
  - `TestFlavorsAreDecisionsOnly`, table:
    - `[decisions]` → true;
    - `[decisions, openai]` → false;
    - `[decisions, anthropic]` → false;
    - `[openai_audio, decisions]` → true;
    - `[]` → false;
    - `[openai]` → false.
  - `TestNormalizeAPIFlavorKeepsDecisions`: `NormalizeAPIFlavor("decisions") == "decisions"` and `NormalizeAPIFlavor(" Decisions ") == "decisions"`.
  - `TestMappingServesAPIFlavorDecisionsOnlyIsNotOpenAI`: a decisions-only mapping is false for `openai`/`anthropic` and true for `decisions`. A `[openai, decisions]` mapping is true for all three.
  - `TestKnownAPIFlavorsListDecisions`: contains `decisions` and stays sorted.
  - `TestNormalizeFlavorsAcceptsDecisionsAndNeverDefaultsIt`:
    - `[decisions]` is kept;
    - `[]` → `[openai, anthropic]`;
    - `[bogus]` → `ErrApplicationFlavorInvalid`.
  - `TestReservedManualVerdictRefusesDecisionNo`: a manual `(decision, no)` → the reserved error; a manual `(decision, yes)` is accepted.
  - `TestAgentIngestDropsADecisionCapabilityRow`: an agent sample claiming `decision` is not written.
  - `TestScoreModelServersRanksADecisionsOnlyMapping`.
  - `TestCapabilityFoldCarriesDecision`: a mapping with a `(decision, yes)` row yields `ModelDTO.Decision == true`.
- [ ] **Step 2: Run them and see them fail.** Run `cd gateway/backend && go test ./internal/routing/ ./internal/portal/ ./internal/gateway/ -run 'Decision|Decisions' -count=1`. Expected: compile errors or FAIL.
- [ ] **Step 3: Implement the constants, the predicate (same shape as `FlavorsAreAudioOnly`), the switch and list entries, the reservations and the fold.**
- [ ] **Step 4: Run the tests again; they should pass.** Also run `go test ./internal/routing/ ./internal/portal/ -count=1` and expect PASS (no regressions in the enumerating tests).
- [ ] **Step 5: Commit** `feat(routing): add the decisions flavor, the decision capability and the decisions-only rule (#373)`.

### Task 2: Keep decisions-only mappings out of every chat path

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go:148-158`: add `targetIsDecisionsOnly`, beside `targetIsImagesOnly`/`targetIsAudioOnly`.
- Modify: `gateway/backend/internal/gateway/inference_resolve.go:102` (translate dispatch refusal).
- Modify: `gateway/backend/internal/gateway/model_warmer.go:175` (`textCandidates`).
- Modify: `gateway/backend/internal/gateway/benchmark_refusals.go`:
  - `codeBenchmarkDecisionsOnly = "benchmark.decisions_only"` with its message;
  - the sentinel;
  - the `loadRefusalErrs` entry;
  - `benchmarkSkippedDecisionsOnly`;
  - `mappingSpecIsDecisionsOnly`;
  - the `loadRefusal` and `loadTargetFor` checks;
  - the `partitionBenchmarkViews` counter and case;
  - `benchmarkViewTarget`;
  - the `refuseNothingRunnable` case.
- Modify: `gateway/backend/internal/gateway/benchmark_endpoints.go`:
  - `benchmarkErrRows` (409);
  - `startContextProbe` :172-179;
  - the Load check :228.
- Modify: `gateway/backend/internal/gateway/portal_model_endpoints.go:483`.
- Modify: `gateway/backend/internal/gateway/benchmark_runner.go:205,219,828,831` (swap sibling).
- Modify: `gateway/backend/internal/gateway/benchmark_vram_isolation.go:477`, `benchmark_vram_runner.go:268`, `load_runner.go:138`.
- Modify: `gateway/backend/internal/gateway/benchmark_speech_trigger.go:225` (skip switch, if it enumerates the reasons).
- Modify: `gateway/backend/internal/portal/service_mapping_images_only.go`: add a `DecisionsOnly` marker. `service_applications.go`: `ModelMappingDTO.DecisionsOnly` (`json:"decisions_only"`), set in `mappingDTO`.
- Test: `benchmark_refusals_test.go`, `model_warmer_test.go`, `native_passthrough_test.go` (or `inference_resolve_test.go`), `benchmark_vram_ensure_test.go`, `portal/service_mapping_decisions_only_test.go` (new, copy of the audio-only test).

**Interfaces:**
- Consumes: `routing.FlavorsAreDecisionsOnly`, `routing.APIFlavorDecisions` (Task 1).
- Produces: `codeBenchmarkDecisionsOnly`, `benchmarkSkippedDecisionsOnly` (string `decisions_only`), `ModelMappingDTO.DecisionsOnly`.

- [ ] **Step 1: Write failing tests**, each a copy of its audio-only twin with a `[decisions]` mapping:
  - `TestTranslateDispatchRefusesADecisionsOnlyTarget` → 404 `routing.no_model_route`;
  - `TestWarmerSkipsADecisionsOnlyMapping`;
  - `TestBenchmarkRefusesADecisionsOnlyMapping` → 409 `benchmark.decisions_only`;
  - `TestBenchmarkPartitionSkipsDecisionsOnly` (skipped reason `decisions_only`);
  - `TestContextProbeRefusesDecisionsOnly`;
  - `TestLoadRefusesDecisionsOnly`;
  - `TestVRAMProbeSkipsDecisionsOnly`;
  - `TestMappingDTOMarksDecisionsOnly`.
  - Review Focus 1: `TestMixedChatAndDecisionsMappingIsStillWarmedAndBenchmarked` (`[openai, decisions]` is not refused).
  - Review Focus 2: `TestAudioAndDecisionsOnlyMappingRefusesWithTheAudioCode` (`[openai_audio, decisions]` → `benchmark.audio_only`, deterministic).
- [ ] **Step 2: Run them and see them fail.** Run `cd gateway/backend && go test ./internal/gateway/ ./internal/portal/ -run 'DecisionsOnly|MixedChatAndDecisions|AudioAndDecisions' -count=1`. Expected: FAIL.
- [ ] **Step 3: Implement each twin next to its audio-only original.** The decisions check always comes after the images and audio checks.
- [ ] **Step 4: Run them again; PASS.** Then run `go test ./internal/gateway/ ./internal/portal/ -count=1`; PASS.
- [ ] **Step 5: Commit** `feat(gateway): keep decisions-only models out of every chat path (#373)`.

### Task 3: Decisions body cap configuration

**Files:**
- Modify: `gateway/backend/internal/config/config.go`: the field `MaxDecisionsBodyBytes int64` next to :164, and the loader next to :327 (`l.integer("OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES", 32<<20)`).
- Modify: `gateway/backend/cmd/gateway/main.go:1164`: pass the value into `ServerDeps`.
- Modify: `gateway/backend/internal/gateway/server.go`: the `ServerDeps.MaxDecisionsBodyBytes` field, the defaulting in `New`, the Server field and the assignment, mirroring the transcription chain at :158-162, :469-472, :940-943, :1198.
- Create: `gateway/backend/internal/gateway/systemone_handler.go`, holding for now only `const defaultDecisionsBodyBytes = 32 << 20` and `func (s *Server) decisionsBodyLimit() int64`, which defaults on a bare `&Server{}`.
- Test: `config/config_test.go` (`TestLoadMaxDecisionsBodyBytesDefault` = 33554432, `TestLoadMaxDecisionsBodyBytesOverride`), `cmd/gateway/decisions_body_cap_wiring_test.go` (copy of `audio_transcription_cap_wiring_test.go`), `gateway/systemone_body_cap_test.go` (`TestDecisionsBodyLimitDefaultsOnABareServer`).
- Docs: add a `docs/architecture/reference/config-env.md` row after :144.

**Interfaces:** Produces `(*Server).decisionsBodyLimit() int64`.

- [ ] **Step 1: Write the failing tests above.**
- [ ] **Step 2: Run `go test ./internal/config/ ./cmd/gateway/ ./internal/gateway/ -run 'DecisionsBody' -count=1`; FAIL.**
- [ ] **Step 3: Implement the chain.**
- [ ] **Step 4: Run them again; PASS.**
- [ ] **Step 5: Commit** `feat(config): add OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES (#373)`.

### Task 4: `POST /v1/systemone` relay

**Files:**
- Modify: `gateway/backend/internal/gateway/systemone_handler.go`. Add:
  - `const apiFlavorSystemOne = routing.APIFlavorDecisions`;
  - `func (s *Server) handleSystemOne(w http.ResponseWriter, r *http.Request)`;
  - `func validateSystemOneRequest(raw []byte) *inference.Error`;
  - `func systemOneInferenceShape(model string) inferenceShape` (apiFlavor `decisions`, endpoint `endpointSystemOne`, requiredCapabilities `[routing.CapabilityDecision]`);
  - `func (s *Server) resolveSystemOneTarget(...)`, modelled on `resolveSpeechTarget` (speech_handler.go:356), including the `targetServesFlavor` refusal;
  - `func (s *Server) recordSystemOneRoutingFailure(...)`, modelled on `recordSpeechRoutingFailure` (set `Endpoint`);
  - `func (s *Server) relaySystemOne(...)`, modelled on `relaySpeech`, through `proxyNative` with `relayAgain`.
- Modify: `gateway/backend/internal/gateway/session_extract.go:24-35`: append `endpointSystemOne`, and add "no session signal" cases in `extractClientSession` and `sessionFromBody`.
- Modify: `gateway/backend/internal/gateway/native_passthrough.go`:
  - `upstreamPath`: before the mode lookup (before :227), `if apiFlavor == apiFlavorSystemOne { return "/v1/systemone" }`;
  - `mergePassthroughUsage` (:1603): `case apiFlavorSystemOne: mergeResponsesUsage(dst, payload)`.
- Modify: `gateway/backend/internal/gateway/server.go` `routes()`: `/v1/systemone` → `s.handleSystemOne`. No `/openai` alias.
- Test: `gateway/backend/internal/gateway/systemone_handler_test.go` (new; fixtures copied from `speech_handler_test.go`: `newSpeechUpstream`, `newSpeechCapableTestServer` → `newSystemOneCapableTestServer`, `newServerAgentSpeechSpecTestServer` → `newServerAgentSystemOneSpecTestServer`), plus `native_passthrough_test.go` (`upstreamPath` table row), `session_extract_test.go`, `inference_handlers_test.go` (shape pin), `instance_reselect_paths_test.go` (row), `passthrough_usage_scan_test.go`.

**Interfaces:**
- Consumes: Task 1 constants, Task 3 `decisionsBodyLimit()`.
- Produces: `apiFlavorSystemOne`, `endpointSystemOne`, `systemOneInferenceShape`.

- [ ] **Step 1: Write failing tests** in `systemone_handler_test.go`:
  - `TestSystemOneRelaysTheRawBodyAndRewritesTheModel`: the upstream sees path `/v1/systemone`, `model` = the mapping's `AppModelName`, and every other byte of `state`/`questions` unchanged. The client gets the upstream's 200 body and status.
  - `TestSystemOneRecordsTokenUsage`: upstream `usage {input_tokens: 213, output_tokens: 0}` → the last usage event has input 213, output 0, `BillingUnit ""`, `ProviderPath "/v1/systemone"`.
  - Review Focus 4: `TestSystemOneWithoutUsageRecordsZeroTokens` (200 body without `usage` → 200 to the client, a row with 0 tokens).
  - `TestSystemOneRelaysAnUpstreamErrorVerbatim`: upstream 422 `{"detail":[...]}` → the client gets 422 with the identical bytes; the usage row error code is `upstream.422`.
  - Review Focus 5, `TestValidateSystemOneRequest` table:
    - no model → `systemone.model_required`;
    - `"model": 3` → model_required;
    - `"model": "  "` → model_required;
    - no questions → `systemone.questions_required`;
    - `"questions": []` → questions_required;
    - `"questions": {}` → questions_required;
    - `"stream": true` → `systemone.stream_unsupported`;
    - `"stream": "false"` → stream_unsupported;
    - `"stream": false` → nil;
    - valid → nil.
  - `TestSystemOneRefusesAMappingWithoutADecisionVerdict` → 404 `routing.model_not_capable`, and no upstream call.
  - `TestSystemOneRefusesAServerAgentChildWhoseSpecExcludesDecisions` → 404 `routing.no_model_route`; `…IncludesDecisions` → relayed.
  - `TestSystemOneIsNeverVendorRoutable`: a vendor model of the same name is not chosen.
  - `TestSystemOneBodyOverTheCapIs413` → `request.body_too_large`.
  - Auth:
    - `TestSystemOneRequiresABearer` (no auth → 401);
    - `TestSystemOneRefusesASessionCookie`;
    - `TestSystemOneRefusesTheInternalLoopback`;
    - `TestSystemOneRejectsAGet` → 405.
  - `TestSystemOneInferenceShapeIsTheEndpointsIdentity` (in `inference_handlers_test.go`).
  - The `upstreamPath` row (`decisions`, `endpointSystemOne`) → `/v1/systemone`.
  - The reselect row `jsonRequest("/v1/systemone", body)`.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run 'SystemOne|UpstreamPath|Reselect' -count=1`; FAIL.**
- [ ] **Step 3: Implement the handler, the relay, the session endpoint, the `upstreamPath` branch, the usage merge case and the route.**
- [ ] **Step 4: Run them again; PASS.** Then `go test ./internal/gateway/ -count=1`; PASS.
- [ ] **Step 5: Commit** `feat(gateway): relay POST /v1/systemone to decision models (#373)`.

### Task 5: `GET /v1/systemone/models`

**Files:**
- Modify: `gateway/backend/internal/gateway/systemone_handler.go`: `func (s *Server) handleSystemOneModels(w http.ResponseWriter, r *http.Request)`. It uses the same portal model source and caller visibility as `handleOpenAIModels` (server.go:2049-2073, `modelDTOsOnFlavor`), filtered to the flavor `decisions` and `ModelDTO.Decision == true`.
- Modify: `gateway/backend/internal/gateway/server.go` `routes()`: `/v1/systemone/models`.
- Test: `systemone_handler_test.go`.

**Interfaces:** Consumes `ModelDTO.Decision` (Task 1). Produces the JSON shape `{"object":"list","data":[{"id","object":"model","api":"systemone"}]}`.

- [ ] **Step 1: Write failing tests:**
  - `TestSystemOneModelsListsOnlyCallableDecisionModels`: of decisions+yes, decisions+no-row, openai-only and a model not allowed for the token, only the first is listed.
  - `TestSystemOneModelsIsAnEmptyArrayNotNull`: the body contains `"data":[]`.
  - `TestSystemOneModelsIsBearerOnly`.
- [ ] **Step 2: Run `go test ./internal/gateway/ -run SystemOneModels -count=1`; FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run it again; PASS.**
- [ ] **Step 5: Commit** `feat(gateway): list decision models on GET /v1/systemone/models (#373)`.

### Task 6: The `decision` verdict probe

**Files:**
- Create: `gateway/backend/internal/provider/systemone_probe.go`:
  - `type SystemOneProber interface { ProbeSystemOne(ctx context.Context, target routing.Target, model string) (verdict string, err error) }`;
  - the `OpenAICompatibleClient` implementation, which POSTs the exact probe body to `target.Endpoint + "/v1/systemone"` with the upstream auth from ctx;
  - `const SystemOneProbeYes = "yes"`, `SystemOneProbeNo = "no"`.
  - `""` with a nil error means no verdict (transient). Classification:
    - 2xx whose JSON `answers.probe` is an object → yes;
    - 404 or 501 → no;
    - anything else → `""`.
- Modify: `gateway/backend/internal/provider/multiplexer.go` (around :384-416): route `ProbeSystemOne` like the sd.cpp prober.
- Modify: `gateway/backend/cmd/gateway/app_health.go`. Add a pass `probeSystemOne` beside the sd.cpp/audio.cpp passes (around :2196-2273):
  - for each mapping whose effective flavors include `decisions`;
  - external apps: `ApplicationEndpoint(server, app)` + app auth;
  - `server_agent`: only names in `r.loaded.LoadedAppModels(app.ID, server.ID)`, `SpecUpstreamAuth(spec, app)`, model = `AppModelName`;
  - settled memory keyed `"systemone:"+mappingID+":"+identity`, where identity is the app model name for external apps and `agentSpecIdentity(spec)` for agent apps;
  - backoff via `mappingBackoff` on `""`;
  - the write is a `CapabilityRow{Capability: decision, Verdict, Source: systemone_probe, CheckedAt}` through `writeCapabilityRows`.
- Test: `provider/systemone_probe_test.go`, `cmd/gateway/app_health_systemone_test.go` (fixtures from the existing sd.cpp/audio.cpp pass tests).

**Interfaces:** Consumes Task 1 constants. Produces `provider.SystemOneProber`.

- [ ] **Step 1: Write failing tests:**
  - `TestProbeSystemOneClassifies`, table:
    - 200 `{"answers":{"probe":{"type":"noul","noul":0.9}}}` → yes;
    - 200 `{"answers":{}}` → `""`;
    - 200 non-JSON → `""`;
    - 404 → no;
    - 501 → no;
    - 400 → `""`;
    - 503 → `""`;
    - a timeout → `""`.
  - `TestProbeSystemOneSendsTheExactBody` (path, method, JSON equal to the Global Constraints body, auth header).
  - `TestSystemOnePassWritesYesForAnExternalApp`.
  - `TestSystemOnePassSkipsAMappingWithoutTheFlavor`.
  - Review Focus 3: `TestSystemOnePassNeverProbesAnUnloadedAgentModel` (no request reaches the fake router) and `…ProbesItOnceLoaded`.
  - `TestSystemOnePassProbesAnIdentityOnce` (two passes → one request).
  - `TestSystemOnePassBacksOffOnATransientResult`.
  - `TestSystemOnePassDoesNotOverwriteAManualYes`.
- [ ] **Step 2: Run `go test ./internal/provider/ ./cmd/gateway/ -run 'SystemOne' -count=1`; FAIL.**
- [ ] **Step 3: Implement the prober, the multiplexer route and the pass.**
- [ ] **Step 4: Run them again; PASS.** Then `go test ./cmd/gateway/ ./internal/provider/ -count=1`; PASS.
- [ ] **Step 5: Commit** `feat(gateway): probe decision models for the decision capability (#373)`.

### Task 7: Portal

**Files:**
- Modify:
  - `gateway/frontend/src/components/shared/ApiVariantControls.tsx`: `apiVariantFlavorOptions` :19 and `flavorLabelKey` :24-30;
  - `components/shared/runtimeSpecTemplate.ts:8` (`optInFlavors`);
  - `components/shared/runtimeSpecTypeFlavors.ts` (a `flavorsAreDecisionsOnly` twin, only if a check there needs it);
  - `api/models.ts`: `decisions_only` on `PortalModelMapping` :225-231, the load-refusal union :704-719, `decision` on ModelOption if surfaced;
  - `api/servers.ts:412-418` (skipped union);
  - `components/shared/format.ts:100-101` (`benchmark.decisions_only` → i18n);
  - `components/BenchmarkSection.tsx`: the refused modes, the refusal and the skipped label;
  - `components/MappingForm.tsx`: a `decision` verdict select, with the reserved `no` shown only when stored;
  - `components/ModelServersSection.tsx`: `KNOWN_CAPABILITY_ORDER` gets `decision`;
  - `i18n.ts` (de + en): `applicationFlavorDecisions`, `mappingDecisionCapable`, `mappingDecisionCapableUnknownHint`, `capabilityDecision`, `benchmarkDecisionsOnlyHint`, `benchmarkResultSkippedDecisionsOnly`, `errorBenchmarkDecisionsOnly`, `mappingProbeContextDecisionsOnly`.
- Test:
  - `ApiVariantControls.test.tsx`: the `decisions` option renders, unticked by default;
  - `runtimeSpecTemplate.test.ts`: `decisions` is not inherited;
  - `BenchmarkSection.test.tsx`: a decisions-only mapping shows the hint and no run button;
  - `MappingForm.test.tsx`: the decision select round-trips yes/unknown and hides `no` unless stored;
  - `ModelServersSection.test.tsx`: the chip label;
  - `format.test.ts`;
  - `i18n.test.ts`: parity.

- [ ] **Step 1: Write the failing tests above.**
- [ ] **Step 2: Run `cd gateway/frontend && npx vitest run <files>`; FAIL.**
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run `npm test && npm run build && npm run lint && npm run format:check`; all pass.**
- [ ] **Step 5: Commit** `feat(portal): offer the decisions flavor and the decision verdict (#373)`.

### Task 8: Architecture docs and ADR

**Files:**
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md`:
  - the §2 diagram and the §3 endpoint table;
  - a new **§3.9 System One decisions**, covering the relay, the validator, the cap, the usage, the verbatim errors, the probe, the listing and the two Phase 1 deviations;
  - §13 rows for the three `systemone.*` codes.
- Modify: `docs/architecture/reference/api-surface.md`:
  - the §1 rows for both paths;
  - the `api_flavors` vocabulary (`decisions`);
  - `decisions_only` on the mapping DTO;
  - `benchmark.decisions_only`.
- Modify: `docs/architecture/reference/openapi.yaml` (both paths, request and response schemas, 400/404/413).
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md` (§1 flavor list, §2.3 capability gate: `decision` and source `systemone_probe`, the decisions-only rule).
- Modify: `docs/architecture/09-architecture-decisions.md`: **ADR-097**, "System One decisions: a third non-text flavor, a probe-written `decision` capability, and a verbatim relay (Phase 1 of #373)", covering context, the decision, the rejected alternatives (a `systemone_mode` column in Phase 1; reading llama.cpp `output_modalities`; an agent-side probe) and the consequences.
- Modify: `docs/architecture/12-glossary.md` ("System One", "decision model").
- Modify: `AGENTS.md` (the auth paragraph: `/v1/systemone` is bearer-only) and `docs/architecture/02-constraints.md:60`.
- Modify: `docs/architecture/reference/config-env.md` (if not done in Task 3).

- [ ] **Step 1: Write the docs.**
- [ ] **Step 2: Run `make lint-docs`; PASS** (links, anchors, openapi `$ref`).
- [ ] **Step 3: Commit** `docs(architecture): document the System One relay and ADR-097 (#373)`.

# Audio Speech Endpoint (`POST /v1/audio/speech`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `POST /v1/audio/speech` behind audio.cpp — a JSON-in request relayed as a binary `audio/wav` response through the existing `proxyNative` streaming path, gated by a new `speech` capability, billed in `audio_second`, and fully configurable through the portal.

**Architecture:** Mirror the #71 images/`sdcpp` template. A dedicated handler validates the JSON body, runs the single `inferencePreflight` gate declaring `RequiredCapabilities:["speech"]`, and relays through `proxyNative` (which already streams and relays the upstream `Content-Type` verbatim). A new `audio_cpp` runtime kind and an `AudioCppModelsProber` write the `speech` verdict from audio.cpp's `/v1/models` `task` field. The non-token billing unit `audio_second` (shipped by #70) is emitted from the `X-AudioCPP-Audio-Duration-Ms` response header.

**Tech Stack:** Go 1.26 (`op-ai-gateway`, `op-ai-server-agent` modules), React + TypeScript + Vite portal.

**Spec:** `docs/superpowers/specs/2026-10-16-audio-speech-design.md` (read it; this plan argues from it).

## Global Constraints

- Go 1.26+ (backend declares 1.25.0, server-agent 1.26); Node 22 + npm. Verbatim from AGENTS.md.
- **wav-only in v1**: accept `response_format` ∈ {`wav`, absent}; refuse all others (mp3/opus/flac/aac, json, b64_json) with `speech.response_format_unsupported`. Refuse `stream`/`stream_format` with `speech.stream_unsupported`.
- **Capability name is `speech`** (`routing.CapabilitySpeech = "speech"`), distinct from `CapabilityAudio` (audio INPUT). **API flavor is `openai_audio`** (`routing.APIFlavorOpenAIAudio = "openai_audio"`). **Runtime kind is `audio_cpp`** (`routing.RuntimeSpecTypeAudioCpp = "audio_cpp"`).
- **No schema/migration change** — `billing_unit`/`billing_quantity` columns and `usage.BillingUnitAudioSecond = "audio_second"` already exist (#70).
- A speech usage row is non-token: `billing_unit="audio_second"`, `billing_quantity` = seconds, and **zero in all token columns and both per-second rates** (`ValidateBillingXOR`).
- Both auth modes on the endpoint: bearer or internal loopback pair, **never the session cookie** (same posture as images; AGENTS.md).
- Preserve stable error codes. Portal i18n: add German + English keys together. No local host names in any repo-facing text. PR/commit/issue text in English.
- ServerAgent `Version` bump rule: only if the agent's wire/behaviour changes (the agent-side capability source is deferred per the spec §8.3, so **no agent bump in this PR**).

## Review Focus

Inputs/conditions the spec implies that no single happy-path task exercises, most likely to bite first:

1. **A request to a model that is present but not speech-capable** (`task` ∈ {asr, music}) must be refused with `routing.model_not_capable` (404), not routed — pinned in Task 4 (verdict `(speech,no)`) and Task 6 (gate refusal).
2. **A speech request whose model's verdict is still unknown** (not yet probed / model not loaded) must refuse, not fail open — pinned in Task 6.
3. **An upstream non-2xx** (audio.cpp returns 500 even for client errors) must be normalised to OpenAI shape and not be mistaken for a successful audio body — pinned in Task 9.
4. **A successful speech response must never write a token count or a t/s value** onto the usage row (would violate `ValidateBillingXOR` and pollute stats) — pinned in Task 8.
5. **The affinity routing branch** must apply the speech gate too (a pinned non-speech mapping must not be reused) — pinned in Task 6.

---

## Phase A — Routing vocabulary & runtime kind

### Task 1: `speech` capability, `openai_audio` flavor, served-flavor plumbing

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (Capability* block ~1297–1356; APIFlavor* block ~89; CapabilitySource* ~1448)
- Modify: `gateway/backend/internal/routing/resolver.go` (`NormalizeAPIFlavor` ~1383–1398)
- Modify: `gateway/backend/internal/routing/served_flavors.go` (the images-only helper)
- Modify: `gateway/backend/internal/routing/score_servers.go` (flavor list ~32)
- Test: `gateway/backend/internal/routing/normalize_api_flavor_test.go`, `served_flavors_parity_test.go` (extend)

**Interfaces:**
- Produces: `routing.CapabilitySpeech = "speech"`; `routing.APIFlavorOpenAIAudio = "openai_audio"`; `routing.CapabilitySourceAudioCppModels = "audiocpp_models"`; a `FlavorsAreAudioOnly(flavors []string) bool` helper (mirror the images-only one).

- [ ] **Step 1: Write failing tests** — `TestNormalizeAPIFlavorFoldsOpenAIAudio` (asserts `NormalizeAPIFlavor("openai_audio") == APIFlavorOpenAIAudio` and that an `openai_audio`-prefixed value folds **before** the generic `openai` prefix); extend the served-flavors parity test to assert `openai_audio` is a known coarse flavor and `FlavorsAreAudioOnly([]string{"openai_audio"})` is true, `FlavorsAreAudioOnly([]string{"openai"})` false.
- [ ] **Step 2: Run tests, verify they fail** — `cd gateway/backend && go test ./internal/routing/ -run 'OpenAIAudio|AudioOnly' -v` → FAIL (undefined constants).
- [ ] **Step 3: Add the constants and the `NormalizeAPIFlavor` case** next to the image analogs (`CapabilityImage`, `APIFlavorOpenAIImages`, `CapabilitySourceSdcppCapabilities`). Fold `openai_audio*` before the generic `openai` prefix exactly as `openai_images*` is folded. Add `FlavorsAreAudioOnly` mirroring the images-only helper, and add `openai_audio` to the `score_servers` flavor list.
- [ ] **Step 4: Run tests, verify pass.**
- [ ] **Step 5: Commit** — `feat(routing): add speech capability and openai_audio flavor vocabulary`

### Task 2: `audio_cpp` runtime kind (gateway + server-agent twin)

**Files:**
- Modify: `gateway/backend/internal/routing/runtime_spec_type.go` (constants ~20–27; `DetectRuntimeSpecType` ~39–56; `DeriveProbePaths` ~71–120)
- Modify: `server-agent/internal/collector/probe.go` (the `DeriveProbePaths`/detection twin)
- Test: `gateway/backend/internal/routing/runtime_spec_type_test.go`; `server-agent/internal/collector/probe_test.go`

**Interfaces:**
- Produces: `routing.RuntimeSpecTypeAudioCpp = "audio_cpp"`.

- [ ] **Step 1: Write failing tests** — `TestDetectRuntimeSpecTypeAudioCpp` (binary basename `audiocpp_server` → `RuntimeSpecTypeAudioCpp`; confirm whether a `minitts` alias is needed — see spec §8.1, decide from the shipped binary name, default to matching `audiocpp_server` and `audio.cpp`); `TestDeriveProbePathsAudioCpp` asserts both returned paths are `""` (no `/metrics`, no context axis — same as `stable_diffusion_cpp`).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Add** the constant, a `DetectRuntimeSpecType` case (basename substrings), and a `DeriveProbePaths` case returning `"", ""`. Mirror all three in the `server-agent` twin.
- [ ] **Step 4: Run, verify pass** — both modules.
- [ ] **Step 5: Commit** — `feat(routing): add audio_cpp runtime kind`

## Phase B — Capability verdict writer (gateway-direct probe)

> Scope note: audio.cpp here is a standalone server, so the verdict is written by the **gateway health loop** probing `/v1/models`. The agent-side source (`agent_ingest` + feature flag + ServerAgent bump) is a deferred follow-up (spec §8.3); do not add it in this PR.

### Task 3: `AudioCppModelsProber` — read `task` from `/v1/models`

**Files:**
- Create: `gateway/backend/internal/provider/audiocpp_models.go`
- Modify: `gateway/backend/internal/provider/multiplexer.go` (add `ProbeAudioCppModels` + `dispatchProbeAudioCppModels`, mirror `ProbeSdcppCapabilities` ~314–346)
- Test: `gateway/backend/internal/provider/audiocpp_models_test.go`

(Gateway-only, per the 2026-10-16 ruling: the server-agent twin `collector/audiocpp.go` and the cross-module parity test belong to the agent-side capability source, deferred to a follow-up (spec §8.3). The gateway health loop (Task 4) probes the running server directly.)

**Interfaces:**
- Produces: interface `AudioCppModelsProber interface { ProbeAudioCppModels(ctx, target, modelID string) (AudioCppVerdicts, error) }`; `AudioCppVerdicts{ Speech string }` (verdict `"yes"`/`"no"`/`""`); `audiocppModelsPath = "/v1/models"`; `parseAudioCppModels(body []byte, modelID string) AudioCppVerdicts`.
- Consumes: the probe HTTP client from the provider layer (as `SdcppCapabilitiesProber` does).

- [ ] **Step 1: Write failing tests** for `parseAudioCppModels` with table cases against a captured `/v1/models` body:
  - model present with `"task":"tts"` → `Speech:"yes"`; `"task":"clone"` → `"yes"`.
  - model present with `"task":"asr"` (or any non-speech task) → `Speech:"no"` (bidirectional: the loaded model's task is authoritative).
  - model id absent from the list → `Speech:""` (unknown).
  - empty `data:[]` → `""`.
  Assert `Multiplexer.ProbeAudioCppModels` returns `ErrUnavailable` when the resolved client does not implement `AudioCppModelsProber` (mirror the sdcpp dispatcher).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** `parseAudioCppModels` (unmarshal the OpenAI list, find the entry whose `id == modelID`, map `task`), `ProbeAudioCppModels` (GET `/v1/models`, call the parser), and the `Multiplexer` dispatcher. Duplicate the parser verbatim into the `server-agent` twin.
- [ ] **Step 4: Run, verify pass** — both modules + the parity test.
- [ ] **Step 5: Commit** — `feat(provider): probe audio.cpp /v1/models for the speech verdict`

### Task 4: Health-loop projector + reserved manual verdict

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (add `audioCppCapabilityRows(v, now)` ~ mirror `sdcppCapabilityRows` ~581–589; call a `probeAudioCppModels` in the health loop ~ mirror `probeSdcppCapabilities` ~631; persist via `store.UpsertMappingCapabilities`)
- Modify: `gateway/backend/internal/portal/service_applications.go` (`reservedManualVerdicts` ~294–321)
- Test: `gateway/backend/cmd/gateway/app_health_test.go`; `gateway/backend/internal/portal/service_applications_test.go`

**Interfaces:**
- Consumes: `AudioCppModelsProber` (Task 3), `routing.CapabilitySpeech`, `routing.CapabilitySourceAudioCppModels` (Task 1).

- [ ] **Step 1: Write failing tests** — `audioCppCapabilityRows` builds one `CapabilityRow{Capability: CapabilitySpeech, Verdict, Source: CapabilitySourceAudioCppModels, CheckedAt}` from an `AudioCppVerdicts`, omitting the row when the verdict is `""`; the health loop calls the probe only for `audio_cpp` mappings and upserts the row. Add a `reservedManualVerdicts` test asserting `(speech, no)` is reserved (a manual rank-3 "no" cannot be written).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** the projector, the health-loop call (gated on `RuntimeSpecTypeAudioCpp`), and the `reservedManualVerdicts` entry `routing.CapabilitySpeech: {routing.CapabilityNo: true}`.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(health): write the speech capability verdict from audio.cpp probes`

### Task 2b: `ProviderAudioCpp` application/provider type

(Added 2026-10-16 by ruling — Task 4 revealed that the health-loop verdict writer
only reaches an app whose `app.Type` triggers the probe. sd-server uses a dedicated
`ProviderStableDiffusionCpp` app type; audio needs the mirror, or a static
operator-run audio.cpp server is never probed and every speech request 404s. This
task runs after Task 4 and before Task 6/7; it supersedes Task 4's stopgap probe
gate and subsumes Task 10's timeout.)

**Files (mirror `ProviderStableDiffusionCpp` — grep it for the full set):**
- Modify: `gateway/backend/internal/routing/store.go` (add `ProviderAudioCpp = "audio_cpp"` + doc, next to `ProviderStableDiffusionCpp` ~31)
- Modify: `gateway/backend/cmd/gateway/main.go` (register `routing.ProviderAudioCpp: openAICompatible` in the client map ~1296 — `OpenAICompatibleClient` already implements `AudioCppModelsProber`)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (REWRITE the audio probe gate from `app.Type == ProviderServerAgent` + spec-kind to `app.Type == ProviderAudioCpp`, probing all active mappings like the sdcpp pass; keep the per-mapping `AppModelName` probe loop, since the audio verdict is per-model)
- Modify: `gateway/backend/internal/portal/service_applications.go` (`normalizeProviderType`/validation ~1518; `applicationTimeoutDefaultFor` ~1863 → `600000`, add `defaultAudioCppTimeoutMS` — this subsumes Task 10)
- Check & mirror if needed: `gateway/backend/internal/gateway/portal_model_endpoints.go:572` (provider-type branch); `modelDiscoveryFor` (`provider/openai_compatible.go:195`) needs NO audio branch (audio.cpp uses the default `/v1/models` discovery).
- Test: `app_health_test.go` (gate now fires for a `ProviderAudioCpp` app with no spec; update the Task 4 gate tests that used `ProviderServerAgent`), `service_applications_test.go` (timeout default; provider-type validation).

**Interfaces:**
- Produces: `routing.ProviderAudioCpp = "audio_cpp"`.
- Consumes: `provider.AudioCppModelsProber`/`Multiplexer.ProbeAudioCppModels` (Task 3), the Task 4 projector.

- [ ] **Step 1: Write failing tests** — a `ProviderAudioCpp` app (static, NO runtime spec) with active mappings is probed and its `speech` verdict written (update/replace the Task 4 gate tests that asserted the `ProviderServerAgent`+spec gate); `applicationTimeoutDefaultFor(ProviderAudioCpp) == 600000`; the provider type validates/normalizes.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — mirror `ProviderStableDiffusionCpp` at every site above; rewrite the probe gate to `app.Type == ProviderAudioCpp`.
- [ ] **Step 4: Run, verify pass** (`cmd/gateway`, `internal/portal`, and the provider package).
- [ ] **Step 5: Commit** — `feat(provider): add the audio_cpp application type and probe static audio.cpp servers`

## Phase C — The endpoint

### Task 5: Endpoint plumbing (`endpointSpeech`, session switches, `upstreamPath`, route)

**Files:**
- Modify: `gateway/backend/internal/gateway/session_extract.go` (iota ~26–32; `extractClientSession` switch ~73–88; `sessionFromBody` switch ~101–141)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`upstreamPath` ~178–213)
- Test: `gateway/backend/internal/gateway/session_extract_test.go`; `native_passthrough_test.go` (extend the `upstreamPath` table)

**Interfaces:**
- Produces: `endpointSpeech` (new `sessionEndpoint` iota member); `apiFlavorSpeech = "openai_audio"` local alias (add next to `apiFlavorImages`).

- [ ] **Step 1: Write failing tests** — extend the `upstreamPath` table: `apiFlavorSpeech` + `endpointSpeech` → `/v1/audio/speech`. Add `session_extract` cases asserting `endpointSpeech` yields empty `ClientSession`/`Source` (speech has no session signal) through both switches.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — add `endpointSpeech` to the iota; add an **explicit, commented** `case endpointSpeech:` to **both** switches stating "no session signal" (house style, never the absent default); add the `apiFlavorSpeech` branch to `upstreamPath` → `/v1/audio/speech`. (Route registration is done in Task 6, co-located with the handler it references — ruling 2026-10-16.)
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(gateway): wire the /v1/audio/speech endpoint path and session plumbing`

### Task 5b: Audio-only served-flavor gating

(Added 2026-10-16 by ruling — Task 1 surfaced that `FlavorsAreAudioOnly` has no
consumers; the images-only helper is wired into several sites that the audio flavor
must mirror, else an `openai_audio`-only server counts as serving `openai` (chat) and
chat-prompt background jobs would ping audio models.)

**Files (mirror every site that consumes the images-only helper — confirm the current set by grep):**
- Modify: `gateway/backend/internal/routing/` — `effectiveHasAPIFlavor` (the `openai` half) and any `targetServes*`/served-flavor matching that special-cases images.
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` — the `targetIsImagesOnly` analog used on the relay path.
- Modify: the chat-prompt background jobs that skip images-only mappings: `benchmark_refusals`, `benchmark_scheduler`, and `portal/service_mapping_images_only` (grep for the images-only helper to find them all).
- Test: extend the existing images-only tests with the audio-only cases, in their current locations.

**Interfaces:**
- Consumes: `routing.FlavorsAreAudioOnly`, `routing.APIFlavorOpenAIAudio` (Task 1).

- [ ] **Step 1: Write failing tests** — for each site, the audio-only case mirroring the images-only case: an `[openai_audio]` mapping is NOT offered for `openai`/chat, IS offered for `openai_audio`, and is skipped by the chat-prompt background jobs. First grep for every current consumer of the images-only helper and enumerate them in the report, so no site is missed.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — mirror the images-only gating for audio at every enumerated site, consuming `FlavorsAreAudioOnly`. Prefer a minimal parallel of the existing pattern; only generalise an images-only helper to "non-chat-only" if the existing code shape makes that the natural, smaller change (do not undertake a broad refactor — AGENTS.md).
- [ ] **Step 4: Run, verify pass** — the affected packages.
- [ ] **Step 5: Commit** — `feat(routing): gate audio-only servers out of chat flavors and chat-prompt jobs`

### Task 6: Handler, validation, and the capability gate

**Files:**
- Create: `gateway/backend/internal/gateway/speech_handler.go`
- Modify: `gateway/backend/internal/gateway/server.go` (register `/v1/audio/speech` + `/openai/v1/audio/speech` → `s.handleOpenAISpeech`, next to the images routes ~1291–1294 — moved here from Task 5 so the route and its handler land in one compiling commit)
- Test: `gateway/backend/internal/gateway/speech_handler_test.go`; `inference_handlers_test.go` (preflight shape); `gateway/backend/internal/routing/resolver_capability_gate_test.go` (extend for speech incl. the affinity branch)

**Interfaces:**
- Produces: `handleOpenAISpeech(w,r)`; `validateSpeechRequest(raw []byte, model string) *inference.Error`; `resolveSpeechTarget(...)`; `recordSpeechRoutingFailure(...)`; error-code consts `speechResponseFormatUnsupported = "speech.response_format_unsupported"`, `speechStreamUnsupported = "speech.stream_unsupported"`.
- Consumes: `inferencePreflight` + `inferenceShape` (existing); `routing.CapabilitySpeech`, `apiFlavorSpeech`, `endpointSpeech`; `ErrModelNotCapable` (existing → 404 `routing.model_not_capable`); `requireInternalOrBearerAnyScope`, `scopeGatewayUse`, `scopeLLMInvoke`.

- [ ] **Step 1: Write failing tests** — `TestValidateSpeechRequest`: missing `model`/`input` → 400 with distinct codes; `response_format:"mp3"` and `response_format:"json"` and `response_format:"b64_json"` → `speech.response_format_unsupported`; `response_format:"wav"` and absent → OK; `stream:true`/`stream_format` → `speech.stream_unsupported`. `TestHandleOpenAISpeechRefusesSessionCookie` (bearer/internal only). Extend the capability-gate test: a request with `RequiredCapabilities:["speech"]` to a model without a `(speech,yes)` verdict → `ErrModelNotCapable` on the fresh, group, **affinity**, and server-override branches; an unknown (unprobed) verdict also refuses (no fail-open).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** `handleOpenAISpeech` mirroring `handleOpenAIImages` (requireMethod POST → auth → `liftInferenceDeadlines` → `readRawJSONUnlimited` → `sniffRoutingModel` → `validateSpeechRequest` → `inferencePreflight(inferenceShape{apiFlavor: apiFlavorSpeech, endpoint: endpointSpeech, model, requiredCapabilities: []string{routing.CapabilitySpeech}})` → `relaySpeech`), plus `validateSpeechRequest`, `resolveSpeechTarget`, `recordSpeechRoutingFailure`, and **register the routes** in `server.go`. The gate itself is reused (`filterCapable`); only the new verdict/flavor flow through it.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(gateway): add the /v1/audio/speech handler with capability gate`

### Task 7: Binary relay + upstream `Content-Length` passthrough

**Files:**
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`proxyNative` ~388; the response-header write ~655–659; `newUsageScanner` ~694)
- Modify: `gateway/backend/internal/gateway/speech_handler.go` (`relaySpeech` → `proxyNative` with the speech `nativeRelay`)
- Test: `gateway/backend/internal/gateway/native_passthrough_test.go`; `speech_handler_test.go`

**Interfaces:**
- Consumes: `proxyNative`, `nativeRelay`, `upstreamPath(target, apiFlavorSpeech, endpointSpeech)`.

- [ ] **Step 1: Write failing tests** — against an `httptest` audio.cpp stub returning `audio/wav` + `Content-Length`: assert the client response has `Content-Type: audio/wav`, the **upstream `Content-Length` is forwarded**, the body is byte-identical, and the whole body is not buffered (assert the stub is streamed / handler returns before full read, as the images copier test does). Assert `newUsageScanner` returns `nil` for `apiFlavorSpeech` (no chat-usage scan of the audio body).
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — in `proxyNative`, when the upstream response carries a `Content-Length` and the body is relayed verbatim, set it on the client response and copy in increments without forcing chunked (scoped enhancement; confirm images/chat keep their current chunked behaviour via the existing tests). Gate `newUsageScanner` to return `nil` for `apiFlavorSpeech` (mirror images). Wire `relaySpeech`.
- [ ] **Step 4: Run, verify pass** (incl. the existing images/chat passthrough tests, unchanged).
- [ ] **Step 5: Commit** — `feat(gateway): relay audio/wav with upstream Content-Length, no buffering`

### Task 8: Billing — `audio_second` from the duration header

**Files:**
- Modify: `gateway/backend/internal/gateway/images_handler.go` (`billingUnitFor` ~34–56 — it is the shared chokepoint; extend, don't fork)
- Modify: `gateway/backend/internal/gateway/speech_handler.go` (`setSpeechBillingQuantity`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the quantity call site, ~706 mirror `setImagesBillingQuantity`)
- Test: `gateway/backend/internal/gateway/usage_billing_unit_test.go` (extend)

**Interfaces:**
- Produces: `setSpeechBillingQuantity(meta *usageMeta, ex *nativeExchange, status string)`.
- Consumes: `usage.BillingUnitAudioSecond`; the upstream `resp.Header` on the exchange (confirm it is exposed at the call site — spec §8.4).

- [ ] **Step 1: Write failing tests** — a successful speech relay records `billing_unit="audio_second"`, `billing_quantity == X-AudioCPP-Audio-Duration-Ms/1000` (e.g. header `2000.0` → `2.0`), **zero in all token columns and both per-second rates** (`ValidateBillingXOR` holds); a missing/zero header → `billing_quantity == 0` and a single Error log (mirror the images "counted 0" log); a routing failure still carries `billing_unit="audio_second"`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — `billingUnitFor` returns `BillingUnitAudioSecond` for `apiFlavorSpeech`; `setSpeechBillingQuantity` reads `X-AudioCPP-Audio-Duration-Ms` from the upstream response header and sets `meta.BillingQuantity = ms/1000` on success; wire it at the shared recordUsage site next to `setImagesBillingQuantity`.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(gateway): bill speech in audio_second from the duration header`

### Task 9: Capture skip + upstream error normalisation

**Files:**
- Modify: `gateway/backend/internal/gateway/capture.go` (add a response-side body skip, ~187–199)
- Create/Modify: `gateway/backend/internal/gateway/speech_handler.go` (`needsSpeechErrorRelay`, `normalizeSpeechUpstreamError`, `relaySpeechUpstreamError` — mirror the images analogs ~585–720)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (invoke `needsSpeechErrorRelay` on non-2xx, mirror ~598–601)
- Test: `gateway/backend/internal/gateway/capture_*_test.go`; `speech_handler_test.go`

**Interfaces:**
- Produces: `needsSpeechErrorRelay(apiFlavor string, status int) bool`; `normalizeSpeechUpstreamError(body []byte, status int) ([]byte, bool)`; `speechUpstreamErrorType = "upstream_error"`, `speechUpstreamErrorCode = "speech.upstream_error"`.

- [ ] **Step 1: Write failing tests** — capture of a successful speech response stores metadata only (no WAV bytes through `json.Marshal`), gated on the **response** `Content-Type` being `audio/*`; an error response (`application/json`) is still captured. `normalizeSpeechUpstreamError`: audio.cpp's `{"error":{"message":…,"type":"server_error"}}` on a 500 is relayed as OpenAI shape (object-shaped passthrough, gateway sets its own `type`/`code` only when upstream's is not OpenAI-attested); a 2xx `audio/wav` is **not** treated as an error.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** the response-side capture skip (new, small — the cited `capture.go` precedent is request-side) and the error-relay trio mirroring images; `needsSpeechErrorRelay` narrow on `apiFlavorSpeech` + non-2xx.
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(gateway): skip binary capture and normalise upstream speech errors`

### Task 10: Long-synthesis timeout default — SUBSUMED into Task 2b

(Resolved 2026-10-16: audio.cpp is served by the new `ProviderAudioCpp` app type
added in Task 2b, so its `applicationTimeoutDefaultFor` → `600000` default lands
there alongside the rest of the provider-type wiring. No separate task. Task 2b's
tests cover the timeout default; the whole-branch review confirms it.)

## Phase D — Operator configuration (portal backend)

### Task 11: Portal flavor validation + model-offering gating

**Files:**
- Modify: `gateway/backend/internal/portal/service_applications.go` (`normalizeFlavors` ~1790–1794; also used for `ErrRuntimeSpecFlavorInvalid` in `service_runtime.go`)
- Modify: `gateway/backend/internal/portal/service.go` (`knownAPIFlavors`/`callableModelNames` ~2036–2044; model-offering gating ~3008–3022) and `service_model_offering.go`
- Modify: `gateway/backend/internal/portal/service_runtime.go` (`validRuntimeSpecType` closed switch ~1340–1347 + its "six kinds" doc comment ~1335–1339) and `gateway/backend/internal/gateway/portal_runtime_endpoints.go` (the `runtime_spec.type_invalid` message listing the kinds ~87) — add `audio_cpp` so an operator can configure an `audio_cpp` runtime spec explicitly. (Added per Task 2 ruling. Note: `runtimeSpecHealthPathDefault` ~214 already defaults to `/health` for an unknown kind, which is correct for audio.cpp — no change.)
- Test: `service_applications_test.go`, `service_runtime_test.go` (incl. `TestPutRuntimeSpecTypeValidation` — add the `audio_cpp` accepted case), `service_model_offering_test.go`

- [ ] **Step 1: Write failing tests** — an application and a runtime spec can be saved with flavor `openai_audio` (no `ErrApplicationFlavorInvalid`/`ErrRuntimeSpecFlavorInvalid`); `knownAPIFlavors` includes it; the `openai_audio` flavor is offered **only** behind a `(speech,yes)` verdict (the speech analog of the image-verdict gate) — a model without the verdict does not offer `openai_audio`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — add `openai_audio` to `normalizeFlavors` and `knownAPIFlavors`; add the model-offering gate mirroring the image one (`ModelOffering.Capable` keyed on `CapabilitySpeech`).
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(portal): accept and gate the openai_audio flavor`

## Phase E — Live panel + frontend

### Task 12: Live Active-Requests billing unit (backend)

**Files:**
- Modify: `gateway/backend/internal/gateway/active_requests.go` (`ActiveRequest` ~19–58; `activeRequestDTO` ~180–220; the registration site that builds the in-flight record)
- Test: `gateway/backend/internal/gateway/active_requests_test.go`

**Interfaces:**
- Produces: `ActiveRequest.BillingUnit string` + `activeRequestDTO` json `billing_unit`.

- [ ] **Step 1: Write failing test** — an in-flight speech request's `ActiveRequest`/DTO carries `billing_unit="audio_second"`; a chat request carries `""`.
- [ ] **Step 2: Run, verify fail.**
- [ ] **Step 3: Implement** — add the field, mirror it in the DTO, populate it from `billingUnitFor(apiFlavor)` where the in-flight record is registered (so this also fixes the existing images "0" gap).
- [ ] **Step 4: Run, verify pass.**
- [ ] **Step 5: Commit** — `feat(gateway): carry the billing unit on the in-flight active request`

### Task 13: Frontend — flavor UI, i18n, live-panel em dash, runtime template

**Files:**
- Modify: `gateway/frontend/src/api/usage.ts` (`ActiveRequest` type ~147–189)
- Modify: `gateway/frontend/src/components/ActiveRequestsPanel.tsx` (output-tokens column ~237–244)
- Modify: `gateway/frontend/src/i18n.ts` (add `applicationFlavorOpenaiAudio` DE+EN next to `applicationFlavorOpenaiImages`; `billingUnitHelpAudioSecond` already exists)
- Modify: the flavor-checkbox component and `RuntimeAdminSection.tsx` (audio-only default for `audio_cpp`); the runtime-kind lists `gateway/frontend/src/api/runtime.ts` and `runtimeSpecTypeFlavors.ts` must include `audio_cpp` (added per Task 2 ruling)
- Test: `gateway/frontend/src/components/*.test.tsx` (ActiveRequestsPanel; flavor UI)

**Interfaces:**
- Consumes: `isTokenMetered` from `components/billingUnit.ts`.

- [ ] **Step 1: Write failing tests** — `ActiveRequestsPanel` renders "—" (with the `activityNotTokenMetered` tooltip) for a row whose `billing_unit` is non-empty, and the number otherwise; the application form offers an `openai_audio` flavor checkbox with its label; `RuntimeAdminSection` defaults `audio_cpp` flavors to audio-only.
- [ ] **Step 2: Run, verify fail** — `cd gateway/frontend && npm test`.
- [ ] **Step 3: Implement** — add `billing_unit?` to the `ActiveRequest` type; wrap the output-tokens column in `isTokenMetered`; add the flavor checkbox + i18n keys (DE+EN together); add the runtime template default.
- [ ] **Step 4: Run, verify pass**; then `npm run build` and `npx prettier --check` (format:check — frontend CI runs it).
- [ ] **Step 5: Commit** — `feat(portal-ui): openai_audio flavor, audio-only runtime template, live em dash`

## Phase F — Docs & end-to-end

### Task 14: Architecture docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR for `speech` capability + `audio_cpp` runtime kind, in the ADR-042 family)
- Modify: `docs/architecture/reference/api-surface.md`, `reference/openapi.yaml` (the new endpoint), `reference/data-model.md` (if capability source listed)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (speech request/response, wav-only policy), `routing-and-model-selection.md` (speech verdict), `telemetry-usage-observability.md` (audio_second on speech), `12-glossary.md`
- Modify: `docs/architecture/03-context-and-scope.md`, `06-runtime-view.md` (speech relay scenario) as #71 did for images

- [ ] **Step 1:** Write the ADR and update each reference/cross-cutting doc so behaviour and structure are recorded (the durable home; `docs/superpowers/**` is removed before the PR).
- [ ] **Step 2: Run** `make lint-docs` (link + reachability + openapi `$ref` check) → PASS.
- [ ] **Step 3: Commit** — `docs(architecture): record the speech capability, audio_cpp kind, and /v1/audio/speech`

### Task 15: End-to-end suite + full verification

**Files:**
- Create: `gateway/e2e/` speech suite (mirror the images e2e), with a mock (or real) audio.cpp returning `audio/wav` + metering headers.
- Modify: `gateway/e2e/package.json` scripts if a named suite is added.

- [ ] **Step 1: Write the e2e** — configure an `openai_audio` application against a stub audio.cpp; assert: a `speech`-capable model returns `audio/wav` with the upstream `Content-Length`; a non-capable model → 404 `routing.model_not_capable`; `response_format:"mp3"` → `speech.response_format_unsupported`; the usage row shows `audio_second` + a non-zero quantity and renders "—" for tokens.
- [ ] **Step 2: Run** the suite → PASS.
- [ ] **Step 3: Full verification** — `make lint`, `make test-go`, `cd gateway/frontend && npm test && npm run build && npm audit --audit-level=moderate`, the Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` (no schema change expected, but the store/capability paths are touched), and the local Sonar branch-findings gate (`make sonar-findings && make sonar-branch-findings`). Report any skipped gate in the PR.
- [ ] **Step 4: Commit** — `test(e2e): cover /v1/audio/speech end to end`

### Task 16: Branch finalisation (pre-PR)

- [ ] **Step 1:** Fold any durable content from this plan/spec into `docs/architecture/` (Task 14 should already cover it).
- [ ] **Step 2:** Remove `docs/superpowers/**` and `docs/implementation-status.md` from the branch; verify with `git diff --name-only main...HEAD` that neither path is added/modified.
- [ ] **Step 3:** Push the branch over SSH; open the PR (English; substantive commit body for the squash description; no local host names; no attribution lines). Enable Auto-fix on the PR. Do not merge.

---

## Self-Review notes

- **Spec coverage:** every §5 piece maps to a task (vocabulary→T1, runtime kind→T2, prober→T3, projector/reserved→T4, endpoint plumbing→T5, handler/validation/gate→T6, relay+Content-Length→T7, billing→T8, capture+errors→T9, timeout→T10, portal config→T11, live panel→T12/T13, docs→T14, e2e→T15, hygiene→T16). §6 capability considerations are exercised by T3/T4/T6. §9 follow-ups (streaming, json/b64_json, transcriptions, agent-side) are explicitly out.
- **Review Focus:** each of the five lines is pinned — (1)(2)(5) in T6, (3) in T9, (4) in T8.
- **Type consistency:** `CapabilitySpeech`, `APIFlavorOpenAIAudio`/`apiFlavorSpeech`, `RuntimeSpecTypeAudioCpp`, `CapabilitySourceAudioCppModels`, `AudioCppVerdicts.Speech`, `endpointSpeech`, `billingUnitFor`, `setSpeechBillingQuantity`, `ActiveRequest.BillingUnit` are used consistently across tasks.
- **Open implementation confirmations** (spec §8, settle in the owning task, none changes the shape): binary basename (T2), provider/appType for the timeout (T10), `resp.Header` exposure at the quantity site (T8), capability-source rank tier (T3/T4).

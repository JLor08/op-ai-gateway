# Audio Transcriptions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve `POST /v1/audio/transcriptions` (multipart audio in, JSON out) backed by audio.cpp, gated by a new `transcription` capability and billed in `audio_second`.

**Architecture:** A twin of the speech endpoint (#68) on the gate/flavor/billing/health axes and a twin of the images-edit endpoint (#218) on the multipart request axis. It reuses the `openai_audio` flavor, the `audio_cpp` application type/runtime kind, and the `audio_second` unit; it adds a `transcription` capability, a multipart handler, an endpoint-keyed relay that parses the JSON response body for the billable audio duration, and the operator-portal/frontend/docs/e2e surface.

**Tech Stack:** Go 1.26 (gateway backend `op-ai-gateway`, server-agent `op-ai-server-agent`), React/TypeScript/Vite (portal), Playwright (e2e).

**Spec:** `docs/superpowers/specs/2026-10-16-audio-transcriptions-design.md`

## Global Constraints

- Base tree is the worktree on `origin/main` @ ae41f81 (includes #241 speech, #242 run-as). All file:line references below are approximate origin/main numbers — **re-verify the symbol in the worktree before editing**; TDD will catch drift.
- Repo-facing text (code, docs, commits, PR) in **English**; no local host names; no attribution lines in commits/PRs.
- Never commit to `main`; feature branch only. Frequent commits, one per task.
- Preserve stable error codes. Keep both auth modes. No schema/migration change (`audio_second`, `billing_unit`/`billing_quantity`, capability vocabulary already exist).
- **No ServerAgent version bump** — the generic `multipart_proxy` feature (agent 0.9.0) already routes any multipart body by its `model` form part.
- Portal i18n keys are added in **German and English together** (`i18n.ts`; the type-checked build enforces parity).
- The flavor string for transcription **is** `openai_audio` (same as speech), so every behavior that differs between speech and transcription is keyed on `endpoint == endpointTranscription`, never on the flavor string.
- Capability naming: `transcription` (audio→text), distinct from `speech` (text→audio) and `audio` (model accepts audio input). Never reuse `CapabilityAudio`.
- Billing XOR (`ValidateBillingXOR`): a non-token row carries `billing_unit`/`billing_quantity` and **zero** in all seven token columns and both rates.

## Review Focus

Input classes the spec implies but a happy-path test may miss — each is pinned to the task that owns the code (test added in that task's steps), most-likely-to-bite first:

1. **A success response whose `timing.audio_duration_ms` is absent, non-numeric, ≤0, or NaN/Inf** → must record `billing_quantity = 0` and log once at Error, never a poisoned sum or a crash (Task 7).
2. **A response that exceeds the buffer cap** → must be an upstream error, never a silently-truncated transcript billed at 0 (Task 7).
3. **A repeated `model` form field, or a `file` part before `model`, or an empty `file`** → the model must be read identically to the agent router's `peekMultipartModel` (whole body walked, repeat refused), and an empty/missing file refused before admission (Task 5).
4. **A request whose model resolves to a non-transcription-capable model, including via an affinity pin** → refused `routing.model_not_capable` (404) on every resolve branch, affinity included; the pin is not honored (Task 6).
5. **An agent-managed target whose agent has not declared `multipart_proxy`** → refused with a distinct code that reads "agent too old / not reported", not an unknown-model 404 (Task 6).

Additional audited-not-tested concern carried to the whole-branch review: no `apiFlavor == apiFlavorSpeech` guard in `proxyNative`/billing/usage-scan may misfire for transcription (the shared-flavor hazard).

---

### Task 1: `transcription` capability + reserved manual verdict

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (capability constants, near `CapabilitySpeech`)
- Modify: `gateway/backend/internal/portal/service_applications.go` (`reservedManualVerdicts`, ~:294)
- Test: `gateway/backend/internal/routing/*_test.go` (constant + `capabilityRowsSatisfy`), `gateway/backend/internal/portal/service_mapping_capability_reserved_test.go` (or the nearest reserved-verdict test)

**Interfaces:**
- Produces: `routing.CapabilityTranscription = "transcription"` (string constant). `reservedManualVerdicts[routing.CapabilityTranscription] = {routing.CapabilityNo: true}`.

- [ ] **Step 1: Write the failing tests** — (a) a routing test asserting `capabilityRowsSatisfy([{transcription, yes}], ["transcription"])` is true and an absent/`no` row refuses; (b) a portal test asserting `reservedManualVerdict(CapabilityTranscription, CapabilityNo)` is reserved (refused) while `(CapabilityTranscription, CapabilityYes)` and a reset are writable. Mirror the existing `CapabilitySpeech` reserved test.
- [ ] **Step 2: Run the tests; expected FAIL** (constant undefined / verdict not reserved).
- [ ] **Step 3: Add `CapabilityTranscription = "transcription"`** with a doc line (model transcribes audio→text; orthogonal to `CapabilityAudio` and `CapabilitySpeech`). Add the `reservedManualVerdicts` entry mirroring `CapabilitySpeech: {CapabilityNo: true}`.
- [ ] **Step 4: Run the tests; expected PASS.** Then `make test-go` for the two packages.
- [ ] **Step 5: Commit** `feat(routing): add the transcription capability and reserve its manual no verdict`.

### Task 2: capability prober + health projector write the transcription verdict

**Files:**
- Modify: `gateway/backend/internal/provider/audiocpp_models.go` (`AudioCppVerdicts`, `parseAudioCppModels`, ~:43/:128)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`audioCppCapabilityRows`, ~:609)
- Test: `gateway/backend/internal/provider/audiocpp_models_test.go`, `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: `routing.CapabilityTranscription` (Task 1).
- Produces: `AudioCppVerdicts{ Speech string; Transcription string }`. `parseAudioCppModels(body, modelID)` sets, by `task`: `asr → {Speech:no, Transcription:yes}`; `tts`/`clone → {Speech:yes, Transcription:no}`; any other non-empty task → both `no`; empty/absent/no-match → `{}` (both empty). `audioCppCapabilityRows(v, at)` emits a `transcription` row (source `audiocpp_models`) when `v.Transcription != ""`, alongside the existing `speech` row.

- [ ] **Step 1: Write the failing tests** — extend the `parseAudioCppModels` table: an `asr` entry → `Transcription == yes`, `Speech == no`; a `tts` entry → `Transcription == no`, `Speech == yes`; an unknown task → both `no`; absent task / no match → both `""`. Extend `audioCppCapabilityRows` test: a `{Speech:yes, Transcription:no}` verdict emits two rows with the right capabilities/verdicts/source; an empty verdict emits none.
- [ ] **Step 2: Run; expected FAIL** (`Transcription` field missing).
- [ ] **Step 3: Implement** — add the `Transcription` field; rewrite the `switch` in `parseAudioCppModels` so `asr` is an explicit `yes`-transcription case (today it falls to the `no` default) and `tts`/`clone` set `Transcription:no`; add the second row in `audioCppCapabilityRows` guarded on non-empty verdict. The decode struct still reads only `id`+`task` (do not add `loaded`; see the flagged review item).
- [ ] **Step 4: Run the two package tests; expected PASS.**
- [ ] **Step 5: Commit** `feat(provider): derive a transcription verdict from audio.cpp task=="asr"`.

### Task 3: endpoint enum, `upstreamPath`, session no-signal

**Files:**
- Modify: `gateway/backend/internal/gateway/session_extract.go` (`sessionEndpoint` enum + both switches, ~:26/:74/:105)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (`upstreamPath`, ~:191)
- Test: `gateway/backend/internal/gateway/session_extract_test.go`, `native_passthrough_test.go`

**Interfaces:**
- Produces: `endpointTranscription sessionEndpoint` (new iota after `endpointSpeech`). `upstreamPath(target, apiFlavor, endpointTranscription) == "/v1/audio/transcriptions"`.

- [ ] **Step 1: Write the failing tests** — `upstreamPath` returns `/v1/audio/transcriptions` for `endpointTranscription` (keyed on the endpoint, regardless of the `openai_audio` flavor it shares with speech); both `session_extract` switches return `("","")` for `endpointTranscription` (a transcription request carries no session/affinity signal). Extend the existing table tests.
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** — add `endpointTranscription` to the enum; add a no-signal `case endpointTranscription:` in `extractClientSession` and `sessionFromBody`; add the `upstreamPath` branch (answered before the EndpointMode lookup, keyed on `endpoint == endpointTranscription`).
- [ ] **Step 4: Run; expected PASS.**
- [ ] **Step 5: Commit** `feat(gateway): add the transcription endpoint to session extraction and upstreamPath`.

### Task 4: upload body-cap config and wiring

**Files:**
- Modify: `gateway/backend/internal/config/config.go` (field + env parse, ~:112/:254)
- Modify: `gateway/backend/cmd/gateway/main.go` (ServerDeps wiring, ~:1105)
- Modify: `gateway/backend/internal/gateway/server.go` (`ServerDeps` field, `Server` field, `New` default, a `bodyLimit` accessor, ~:123/:389/:731)
- Test: `gateway/backend/internal/config/config_test.go` (env parse), `gateway/backend/cmd/gateway/audio_transcription_cap_wiring_test.go` (new, mirror `image_edit_cap_wiring_test.go`)

**Interfaces:**
- Produces: `config.Config.MaxAudioTranscriptionBodyBytes int` (env `OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES`, default `32<<20`). `Server` honors it via a `transcriptionBodyLimit() int64` accessor returning the configured value or `defaultMaxAudioTranscriptionBodyBytes` when ≤0.

- [ ] **Step 1: Write the failing tests** — env-parse test (unset/blank/non-numeric/≤0 → 32 MiB default, a valid value parsed); a wiring test driving `buildGatewayServer(config.Config{MaxAudioTranscriptionBodyBytes: 2048})` asserting a body over the cap → 413 `request.body_too_large` and one under → not 413 (mirror `TestBuildGatewayServerHonoursTheConfiguredImageEditBodyCap`; it will need the Task 5/6 handler to exist, so if authored before them, assert only config→Server field propagation and move the 413 assertion into Task 6).
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** the full chain mirroring `MaxImageEditBodyBytes`: config field + `loader.integer` default; `main.go` ServerDeps pass-through; `ServerDeps` field; `Server` private field; `New` default; `transcriptionBodyLimit()` accessor; `const defaultMaxAudioTranscriptionBodyBytes = 32 << 20` and `const maxTranscriptionFieldBytes = 1 << 20`.
- [ ] **Step 4: Run; expected PASS.**
- [ ] **Step 5: Commit** `feat(config): add the audio-transcription upload body cap and wire it through`.

### Task 5: multipart form parser + request validator

**Files:**
- Create: `gateway/backend/internal/gateway/transcription_handler.go` (form struct, `parseTranscriptionForm`, `validateTranscriptionRequest`, `readTranscriptionBody`, error-code constants)
- Test: `gateway/backend/internal/gateway/transcription_handler_test.go`

**Interfaces:**
- Consumes: `transcriptionBodyLimit()` (Task 4), `maxTranscriptionFieldBytes` (Task 4).
- Produces: `transcriptionForm{ boundary, model, language, prompt, responseFormat, stream string; files int }` with `routingModel() string`; `parseTranscriptionForm(contentType string, raw []byte) (transcriptionForm, error)`; `validateTranscriptionRequest(form transcriptionForm) *inference.Error`; `readTranscriptionBody(w, r, limit int64) ([]byte, bool)`. Error codes: `transcription.model_required`, `transcription.file_required`, `transcription.response_format_unsupported`, `transcription.stream_unsupported`, `transcription.invalid` (malformed multipart).

- [ ] **Step 1: Write the failing tests** — parse: a well-formed body yields model + one file; a repeated `model` part → invalid; a `file` part before `model` still finds the model (whole body walked); an empty `file` → `files == 0`; a non-multipart content type → invalid. validate: missing model → `model_required`; zero non-empty files → `file_required`; `response_format` ∈ {`json`, absent} accepted, every other value (`verbose_json`,`text`,`srt`,`vtt`,`mp3`) → `response_format_unsupported`; `stream=true` or any `stream_format` → `stream_unsupported`. Mirror `parseImageEditForm`/`validateImageEditRequest` test shapes. Build the multipart bodies the way the images-edit tests do.
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** by mirroring `parseImageEditForm`/`readPart`/`validateImageEditRequest` from `images_edit_handler.go`: `multipart.NewReader` (never `ParseMultipartForm`); single-use text fields (`model`/`language`/`prompt`/`response_format`/`stream`); `file`/`file[]` counted via a non-empty-part count; every other part discarded to its end; the model read identically to the agent router's `peekMultipartModel`. `response_format` policy A; stream refusal as `validateSpeechRequest` does it.
- [ ] **Step 4: Run; expected PASS.**
- [ ] **Step 5: Commit** `feat(gateway): parse and validate the transcription multipart request`.

### Task 6: handler, routing, capability + agent gate, upstream relay with model rewrite

**Files:**
- Modify: `gateway/backend/internal/gateway/transcription_handler.go` (handler, inference shape, resolve, relay, rebuild, agent-served gate)
- Modify: `gateway/backend/internal/gateway/server.go` (route registration, ~:1321; a `codeTranscriptionUnavailable` sentinel; `completionHTTPStatus`/`completionErrorCode`/`completionErrorResponse` mappings in `inference_complete.go`)
- Test: `gateway/backend/internal/gateway/transcription_handler_test.go` (recording upstream), `inference_handlers_test.go` / `resolver` capability-gate test

**Interfaces:**
- Consumes: Task 5 (form+validate+body read), Task 3 (`endpointTranscription`, `upstreamPath`), Task 1 (`CapabilityTranscription`), `runAsPrincipal` (run_as.go), `requireInternalOrBearerAnyScope`, `imageEditTargetServed` pattern, `multipartProxyFeature`.
- Produces: `handleOpenAITranscription(w, r)`; `transcriptionInferenceShape(model) inferenceShape` with `apiFlavor: routing.APIFlavorOpenAIAudio, endpoint: endpointTranscription, requiredCapabilities: []string{routing.CapabilityTranscription}`; `relayTranscription(...)`; `transcriptionTargetServed(target) bool` (fail-closed on `multipart_proxy`); sentinel `errTranscriptionUnavailable` → code `transcription.unavailable` (404) and `errTranscriptionTooLarge` → `request.body_too_large` (413).

- [ ] **Step 1: Write the failing tests** — a recording upstream (never discard `*http.Request`, per #54) asserts the gateway sends one POST to `/v1/audio/transcriptions` with the verbatim `multipart/form-data` Content-Type incl. boundary and the model rewritten to `target.ProviderModel` when it differs; a non-capable model (and an **affinity pin** to a non-transcription mapping) → 404 `routing.model_not_capable`, no upstream call; an agent target without `multipart_proxy` → `transcription.unavailable` (reads "agent too old/not reported"), not an unknown-model 404; a body over the agent 32 MiB cap → 413. (Review Focus 4 & 5 live here.)
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** the handler flow (mirror `handleOpenAISpeech` + `handleOpenAIImagesEdit`): method → `requireInternalOrBearerAnyScope(scopeGatewayUse, scopeLLMInvoke)` → `liftInferenceDeadlines` → `readTranscriptionBody(transcriptionBodyLimit())` → `parseTranscriptionForm` → `validateTranscriptionRequest` (before admission) → `runAsPrincipal` → single `inferencePreflight(transcriptionInferenceShape(model))` → `relayTranscription`. `relayTranscription`: resolve+served conjunct; `transcriptionTargetServed` fail-closed; rebuild the multipart body (model field only, mirror `rebuildImageEditBody`, no mask) when `ProviderModel != "" && != form model`; agent 32 MiB re-check after rebuild; `proxyNative(nativeRelay{ path: upstreamPath(...), raw, upstreamBody, contentType, endpoint: endpointTranscription })`. Register both routes; add the three completion mappings for the two sentinels.
- [ ] **Step 4: Run; expected PASS;** then the wiring 413 assertion deferred from Task 4 if applicable.
- [ ] **Step 5: Commit** `feat(gateway): serve POST /v1/audio/transcriptions through audio.cpp`.

### Task 7: response-body duration billing + error normalisation

**Files:**
- Modify: `gateway/backend/internal/gateway/transcription_handler.go` (duration parse, `setTranscriptionBillingQuantity`, `needsTranscriptionErrorRelay`, `normalizeTranscriptionUpstreamError`, recordUsage sites)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (the response-body hook keyed on `endpointTranscription`)
- Test: `gateway/backend/internal/gateway/transcription_handler_test.go`, `usage_billing_unit_test.go`

**Interfaces:**
- Consumes: Task 6 relay; `usage.BillingUnitAudioSecond`; `billingUnitFor` (already returns `audio_second` for `openai_audio`).
- Produces: `transcriptionDurationSeconds(body []byte) (float64, bool)` (parses `timing.audio_duration_ms`/1000, rejects absent/NaN/Inf/≤0); `setTranscriptionBillingQuantity(meta, ex, body, status)` (only on `status=="success"`); `needsTranscriptionErrorRelay(endpoint, status)`, `normalizeTranscriptionUpstreamError(body, status)` → code `transcription.upstream_error`, type `upstream_error`.

- [ ] **Step 1: Write the failing tests** — a success usage row has `billing_unit == "audio_second"`, `billing_quantity` = `audio_duration_ms/1000`, all token cols 0, `provider_path == "/v1/audio/transcriptions"`; a success with no/invalid `timing.audio_duration_ms` (absent, NaN, ≤0) → quantity 0 (Review Focus 1); a response over the buffer cap → `transcription.upstream_error`, quantity 0, no truncated body served to the client (Review Focus 2); an upstream 500 with an OpenAI error object relays it byte-for-byte under 500; an upstream `{"error":"str"}` is wrapped as `transcription.upstream_error`; every refusal/failure records an `audio_second` row with quantity 0.
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** mirroring `setSpeechBillingQuantity`/`normalizeSpeechUpstreamError`, but: read the duration from the **buffered response body** (not a header), bounded by `transcriptionBodyLimit()` — over the bound is an error, never a truncated success; pass the body (incl. the `timing` block) through verbatim. Gate the hooks on `endpoint == endpointTranscription`. Set `usageMeta.Endpoint = endpointTranscription` and `BillingUnit = audio_second` at every recordUsage site (routing failure, upstream error, terminal).
- [ ] **Step 4: Run; expected PASS.**
- [ ] **Step 5: Commit** `feat(gateway): bill transcription audio_second from the response body duration and normalise upstream errors`.

### Task 8: mock provider audio branch + capture test

**Files:**
- Modify: `gateway/backend/internal/provider/mock.go` (`Mock.ProxyNative`, ~:136)
- Test: `gateway/backend/internal/provider/*_test.go` (mock), `gateway/backend/internal/gateway/capture_multipart_test.go` or `capture_binary_test.go` (audio capture)

**Interfaces:**
- Produces: `Mock.ProxyNative` returns `{"text":"…","timing":{"audio_duration_ms":2000}}` (`application/json`) for a `/v1/audio/transcriptions` path.

- [ ] **Step 1: Write the failing tests** — the mock returns the transcription JSON (not a Responses SSE stream) for the transcription path; a capture test asserts a transcription multipart request stores the text fields (`model`, `language`) and marks the `file` part redacted, and the JSON response is captured normally.
- [ ] **Step 2: Run; expected FAIL.**
- [ ] **Step 3: Implement** the mock path case (mirror the `/v1/images/` prefix case). The capture path needs no code change (keyed on Content-Type) — the test pins existing behavior; add a rename if `captureTextPartMaxBytes` must be endpoint-neutral.
- [ ] **Step 4: Run; expected PASS.**
- [ ] **Step 5: Commit** `test(provider): mock the transcription path and pin audio capture redaction`.

### Task 9: server-agent router multipart routing test (no code change)

**Files:**
- Test: `server-agent/internal/runtime/router_multipart_test.go`

**Interfaces:**
- Consumes: the existing `serveProxy` multipart branch + `peekMultipartModel` (flavor-agnostic).

- [ ] **Step 1: Write the failing/▸passing test** — a `postMultipart` to `/v1/audio/transcriptions` routes to the child keyed on the form `model`, asserting the child sees the same path, Content-Type (incl. boundary) and body; a repeated/empty/missing model → 404 `runtime.model_not_managed`. Reuse the `editBody`/`postMultipart` helpers with an audio field set.
- [ ] **Step 2: Run** — it should PASS without code change (the branch is flavor-agnostic); if it fails, the branch needs the fix (surface in the task's review, do not silently change the router contract).
- [ ] **Step 3: Commit** `test(server-agent): pin that a transcription multipart body routes by its form model`.

### Task 10: portal frontend — transcription capability chip + i18n

**Files:**
- Modify: `gateway/frontend/src/components/ModelServersSection.tsx` (`KNOWN_CAPABILITY_ORDER`, ~:175)
- Modify: `gateway/frontend/src/i18n.ts` (DE + EN `capabilityTranscription`)
- Test: `gateway/frontend/src/components/ModelServersSection.test.tsx` (or the nearest capability-chip test)

**Interfaces:**
- Produces: a `transcription` entry in `KNOWN_CAPABILITY_ORDER` with `label: (t) => t.capabilityTranscription`; i18n keys `capabilityTranscription` (DE `'Transkription'`, EN `'Transcription'`).

- [ ] **Step 1: Write the failing test** — a model row with a `transcription: yes` verdict renders the translated success chip (not a raw standby string).
- [ ] **Step 2: Run** `npm test -- ModelServersSection`; expected FAIL.
- [ ] **Step 3: Implement** the `KNOWN_CAPABILITY_ORDER` entry and the two i18n keys (parity enforced by the build).
- [ ] **Step 4: Run** `npm test -- ModelServersSection`, then `npm run build` and prettier `format:check`; expected PASS.
- [ ] **Step 5: Commit** `feat(portal): render a transcription capability chip`.

### Task 11: docs — ADR-062 and the doc sweep

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-062, after ADR-061)
- Modify: `docs/architecture/cross-cutting/compatibility-and-inference.md` (new §3.7; body-size table; the "all six / sixth endpoint" statements)
- Modify: `docs/architecture/reference/api-surface.md` (route table; the internal-or-bearer sentence now names audio too), `reference/openapi.yaml`
- Modify: `cross-cutting/routing-and-model-selection.md`, `cross-cutting/telemetry-usage-observability.md`, `cross-cutting/agent-runtime-manager.md`, `cross-cutting/security-auth-rbac.md` (§14 capture: transcript output-sensitivity), `12-glossary.md`, `11-risks-and-technical-debt.md` (§11.4 acceptances)
- Modify: the stale comments the issue names (`usage/recorder.go` "no producer writes a non-empty unit", `inference_handlers.go` "only images carries a required capability", the agent-side `multipart_proxy` image-edit-only comments)
- Modify: `README.md` if the endpoint count/table is user-visible

**Interfaces:** none (documentation). Verified by `make lint-docs`.

- [ ] **Step 1: Write ADR-062** mirroring ADR-061's structure, recording the **differences**: multipart-IN (rides #218), JSON-OUT, billing from the response body `timing.audio_duration_ms` (not a header), response-format policy A, reuse of `openai_audio`/`audio_cpp`, endpoint-keyed (not flavor-keyed) divergence, the response-buffer-bound and loud-zero, the output-sensitivity of the captured transcript. Cross-link as ADR-061 does.
- [ ] **Step 2: Sweep** the listed docs (new §3.7, route/body-size tables, the "sixth endpoint" phrasings, glossary `transcription`, risks §11.4, the stale comments).
- [ ] **Step 3: Run** `make lint-docs`; expected PASS (links/anchors/index/openapi resolve).
- [ ] **Step 4: Commit** `docs: record ADR-062 and sweep the transcription endpoint into the architecture docs`.

### Task 12: local e2e suite `e2e:transcription`

**Files:**
- Create: `gateway/e2e/e2e-transcription/transcription.spec.ts`, `gateway/e2e/playwright.transcription.config.ts`
- Modify: `gateway/e2e/package.json` (`e2e:transcription` script), `docs/architecture/cross-cutting/development-and-quality.md` (suite catalog, ~:267)

**Interfaces:** Consumes the running gateway + portal (copy `playwright.speech.config.ts`; ports 8091/4173; reuse the `audio_cpp` type).

- [ ] **Step 1: Write the suite** mirroring `e2e-speech/speech.spec.ts`: an in-process Node http stub with `GET /health`, `GET /v1/models` carrying a `task:"asr"` model (and a `tts` one), and a multipart `POST /v1/audio/transcriptions` → `{"text":"…","timing":{"audio_duration_ms":2000}}`. Scenarios: (1) the portal `audio_cpp` form + probe writes `(transcription, yes)` for the asr model / `(transcription, no)` for the tts model via `audiocpp_models`; (2) the relay wire contract (one multipart POST, model rewritten); (3) the gate refuses a non-transcription model with 404 `routing.model_not_capable` and refuses `response_format=text` with 400 before upstream; (4) the usage row is `audio_second`, quantity 2, zero tokens; (5) Activity renders em dashes + the `audio_second` chip + quantity 2.
- [ ] **Step 2: Add** the `e2e:transcription` package.json script and the catalog line (noting CI runs only `e2e:runtime`).
- [ ] **Step 3: Run** `cd gateway/e2e && npm run e2e:transcription`; expected PASS.
- [ ] **Step 4: Commit** `test(e2e): add a local transcription suite driving the real gateway against a stub audio.cpp`.

---

## Notes for the executor

- Tasks 1–4 are independent and small; 5→6→7 build the handler incrementally (each a reviewable layer); 8–12 are independent of each other once 6/7 land.
- Re-verify every file:line in the worktree before editing (origin/main numbers drift).
- The whole-branch review must audit the shared-flavor hazard (every `apiFlavor == apiFlavorSpeech`/`apiFlavorImages` guard) and the `loaded:false` prober decision.
- Do NOT bump the ServerAgent version. Do NOT touch `main`.

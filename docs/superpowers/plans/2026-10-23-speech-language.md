# Speech Language Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a portal speech run synthesize the intended language on a TTS model whose audio.cpp spec default is not auto-detecting (e.g. German on `confucius4_tts`), via a per-thread composer language control backed by a cached per-mapping "does this model accept `language`, and its default" fact.

**Architecture:** audio.cpp accepts a top-level `language` on `/v1/audio/speech` and declares each model's `language` param (and default) via `GET /v1/models?include_params=true`. The gateway caches that fact per mapping in the existing `model_mapping_cache` table (new `speech_language` type), written by the health loop while the model is loaded — mirroring the voices cache — for both static and agent-managed audio.cpp. A new `ChatAudioSettings.Language` is injected into the speech body, and a composer gate endpoint surfaces the control. The agent gains one allowlisted passthrough route plus a feature flag.

**Tech Stack:** Go 1.26 (`server-agent`), Go 1.25 (`gateway/backend`), React + TypeScript + Vite (`gateway/frontend`).

**Spec:** `docs/superpowers/specs/2026-10-23-speech-language-design.md`

## Global Constraints

- ServerAgent `const Version` (`server-agent/internal/agent/agent.go`): **0.12.0 → 0.13.0**, MINOR, **one** bump for the whole branch (a new `agent.Features` entry). Base on main's current; merge `origin/main` before finalizing.
- Agent/gateway negotiate by **named feature flags, never version numbers** (ADR-025). The new flag string is `runtime_model_params`, spelled identically in both modules (the agent registry and the gateway's agent-only mirror constant).
- **No schema migration** (cache row and chat-settings field ride schema-free documents / the existing `model_mapping_cache` table).
- Repo-facing text (code, commits, PR, issue, docs) in **English**; no LAN host names in repo-facing text.
- Portal i18n keys added in **German and English together** (`gateway/frontend/src/i18n.ts`; the type-checked build enforces parity).
- Keep all three store drivers working; verify store changes with `OP_AI_GATEWAY_TEST_POSTGRES_DSN` set (the postgres leg).
- Do **not** change `parseAudioCppModels` (the shared verdict rule) or its `audioCppModelCases` table — the language parse is a separate axis.
- Gates before pushing: per-module `golangci-lint fmt --diff` + `run` (gofumpt/gocritic), `go test ./...` (both modules; gateway with the postgres leg), frontend `npm run build` + `format:check` + vitest, `make sonar-gate` + `make sonar-findings` + `make sonar-branch-findings`, `./scripts/check-docs.sh` (after moving `docs/superpowers/**` aside).
- Push over SSH.

## Review Focus

- **Clone model gets no language row.** A `params`-carrying `clon` model must not get a `speech_language` row (plain-tts gate), or the composer would surface a control for a model a text chat never routes to. → Task 4 / Task 5 tests.
- **First-class `language` vs a passthrough `language`.** A thread that sets both `ChatAudioSettings.Language` and `Passthrough["language"]` must send the first-class value once, not twice (the `hasKeyFold` wins-rule). → Task 6 tests.
- **Older agent without `runtime_model_params`.** The gateway must not GET `/upstream/{model}/v1/models` on an agent that never declared the flag (fail-closed), or every health tick hits a doomed 404. → Task 5 tests.
- **Composer gate must not leak model existence.** An unknown, hidden, non-speech or not-yet-probed model answers the same `{"accepts":false}` 200 as a known "no"; only a store error is a 500. → Task 7 tests.
- **`include_params=true` against a server that ignores it / a non-audio.cpp body.** No `params` → `accepts:false`, `default:""`, and the existing speech/transcription verdicts are unchanged. → Task 2 tests.

---

### Task 1: ServerAgent — `/v1/models` passthrough route, `runtime_model_params` flag, version bump

**Files:**
- Modify: `server-agent/internal/runtime/router.go` (`upstreamEndpoints`, near the `/v1/audio/voices` entry)
- Modify: `server-agent/internal/agent/features.go` (append to `Features`)
- Modify: `server-agent/internal/agent/agent.go` (`const Version`)
- Test: `server-agent/internal/runtime/router_test.go`, `server-agent/internal/agent/features_test.go`

**Interfaces:**
- Produces: feature name string `"runtime_model_params"` (Since `"0.13.0"`); agent route `GET /upstream/{model}/v1/models` relaying the inbound query to the running child.

- [ ] **Step 1: Write the failing router test** in `router_test.go`: `TestServeUpstreamModelsRelaysIncludeParams` — a running child registered in a fake manager; `GET /upstream/<model>/v1/models?include_params=true` is relayed to the child at path `/v1/models` with raw query `include_params=true`, and the child's body is returned verbatim. Add a sibling assertion that a cold/unmanaged model still answers `runtime.model_not_running` / `runtime.model_not_managed` (Status-only, never-start, unchanged).
- [ ] **Step 2: Run it to verify it fails.** Run: `cd server-agent && go test ./internal/runtime/ -run TestServeUpstreamModelsRelaysIncludeParams -v` → FAIL (route not allowlisted → `runtime.upstream_endpoint_not_allowed`).
- [ ] **Step 3: Add the allowlist entry.** In `upstreamEndpoints` add `{path: "/v1/models", forwardQuery: true}` (so `?include_params=true` reaches the child; audio.cpp reads it). No other router change — `serveUpstream`'s `CutSuffix` already dispatches by suffix and there is no collision with `/props` or `/v1/audio/voices`.
- [ ] **Step 4: Run the router test to verify it passes.** Run: `cd server-agent && go test ./internal/runtime/ -run TestServeUpstream -v` → PASS.
- [ ] **Step 5: Add the feature flag + version bump.** Append `{Name: "runtime_model_params", Since: "0.13.0"}` to `Features` with a doc comment in the style of `fullduplex_proxy` (declared for the GATEWAY's benefit; gates nothing on the agent; the gateway negotiates on the name before it pulls `/v1/models` through the router, fail-closed). Set `const Version = "0.13.0"` in `agent.go`.
- [ ] **Step 6: Run the feature-registry guard.** Run: `cd server-agent && go test ./internal/agent/ -run TestFeature -v` → PASS (every `Since` ≤ `Version`, unique, snake_case).
- [ ] **Step 7: Commit.**
```bash
git add server-agent/internal/runtime/router.go server-agent/internal/runtime/router_test.go server-agent/internal/agent/features.go server-agent/internal/agent/agent.go
git commit -m "feat(server-agent): add /v1/models upstream passthrough and runtime_model_params flag"
```

---

### Task 2: Gateway provider — parse the audio.cpp `language` request param

**Files:**
- Modify: `gateway/backend/internal/provider/audiocpp_models.go`
- Test: `gateway/backend/internal/provider/audiocpp_models_test.go`

**Interfaces:**
- Consumes: audio.cpp `GET /v1/models?include_params=true`, each `data[]` entry carrying `params: [{name,type,default,required}, ...]`.
- Produces: `AudioCppVerdicts.AcceptsLanguage bool`, `AudioCppVerdicts.LanguageDefault string`, filled by `ProbeAudioCppModels` beside the existing verdicts/`Loaded`/`Path`.

- [ ] **Step 1: Write the failing test** `TestParseAudioCppLanguageParam` with cases: (a) tts entry whose `params` has `{"name":"language","default":"zh"}` → `accepts true, default "zh"`; (b) `params` has a `language` entry with no `default` → `accepts true, default ""`; (c) `params` present but no `language` entry → `accepts false, default ""`; (d) no `params` field at all → `accepts false, default ""`; (e) model not named / empty body / non-JSON → `accepts false, default ""`; (f) `name` matched case/space-insensitively (`" Language "`). Also extend an existing `ProbeAudioCppModels` test (httptest server) to assert the two new fields are populated from an `include_params` body AND that the five existing verdicts are unchanged by the new query.
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/backend && go test ./internal/provider/ -run 'TestParseAudioCppLanguageParam|TestProbeAudioCppModels' -v` → FAIL.
- [ ] **Step 3: Implement.** Add `AcceptsLanguage bool` and `LanguageDefault string` to `AudioCppVerdicts`. Change `audiocppModelsPath` to carry `?include_params=true` (verify `endpointURL` concatenates the query cleanly; if not, build the URL so the query survives). Add `parseAudioCppLanguageParam(body []byte, modelID string) (accepts bool, def string)`: decode a widened `audioCppModelsDoc` whose entry adds `Params json.RawMessage` (kept raw so an odd shape never fails the verdict decode), find `modelID`'s entry, scan its params for the first whose `name` is `language` (case/space-tolerant), read its `default` as a string (tolerant: a non-string default → `def ""`). Call it in `fetchAudioCppModels` beside `parseAudioCppLoaded`/`parseAudioCppPath` and set the two fields. Do **not** touch `parseAudioCppModels`.
- [ ] **Step 4: Run to verify it passes, plus the shared-table guard.** Run: `cd gateway/backend && go test ./internal/provider/ -run 'AudioCpp' -v` → PASS (including `TestAudioCppModelsCasesMatchTheAgentsTable`, untouched).
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/internal/provider/audiocpp_models.go gateway/backend/internal/provider/audiocpp_models_test.go
git commit -m "feat(provider): read the audio.cpp language request param from /v1/models"
```

---

### Task 3: Gateway routing store — `speech_language` cache type + payload helpers

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (near `ModelMappingCacheOutputByteRate` / `ParseOutputByteRate`)
- Test: `gateway/backend/internal/routing/speech_language_test.go` (new)

**Interfaces:**
- Produces: `const ModelMappingCacheSpeechLanguage = "speech_language"`; `func SpeechLanguagePayload(accepts bool, def string) string`; `func ParseSpeechLanguage(payload string) (accepts bool, def string, ok bool)`.

- [ ] **Step 1: Write the failing test** `TestSpeechLanguagePayloadRoundTrip`: `SpeechLanguagePayload(true,"zh")` → a JSON string that `ParseSpeechLanguage` reads back as `(true,"zh",true)`; `SpeechLanguagePayload(false,"")` → `(false,"",true)`; `ParseSpeechLanguage("")` and `ParseSpeechLanguage("{bad")` → `ok==false`; whitespace tolerated.
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/backend && go test ./internal/routing/ -run TestSpeechLanguage -v` → FAIL.
- [ ] **Step 3: Implement** the const + the two helpers (`SpeechLanguagePayload` marshals `{"accepts":...,"default":...}`; `ParseSpeechLanguage` is the one definition of "a usable cached language fact", `ok==false` for empty/unparseable), with doc comments in the `ParseOutputByteRate` style and the two-reader-state contract from the spec.
- [ ] **Step 4: Run to verify it passes.** Run: `cd gateway/backend && go test ./internal/routing/ -run TestSpeechLanguage -v` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/internal/routing/store.go gateway/backend/internal/routing/speech_language_test.go
git commit -m "feat(routing): add speech_language mapping-cache type and payload helpers"
```

---

### Task 4: Gateway health loop (static) — write the `speech_language` cache

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (add `cacheAudioCppSpeechParams`, `writeSpeechLanguageCache`, `speechParamsBackoff`; call from `probeAudioCppModels`)
- Modify: `appHealthRunner` struct (add `speechParamsBackoff mappingBackoff`)
- Test: the audio app-health test file (e.g. `gateway/backend/cmd/gateway/app_health_audio_test.go` — place beside the existing audio health tests)

**Interfaces:**
- Consumes: `provider.AudioCppVerdicts.AcceptsLanguage/.LanguageDefault/.Loaded/.Speech/.SpeechClone` (Task 2); `routing.ModelMappingCacheSpeechLanguage` / `routing.SpeechLanguagePayload` (Task 3); `audioCppVoicesWanted` (existing).
- Produces: `func (r *appHealthRunner) writeSpeechLanguageCache(ctx, mappingID, accepts bool, def string) (changed bool, err error)` — reused by Task 5.

- [ ] **Step 1: Write the failing test** `TestCacheAudioCppSpeechParams` over a fake store + prober: (a) a **loaded plain-tts** model whose verdict carries `AcceptsLanguage=true, LanguageDefault="zh"` → a `speech_language` row with payload `{accepts:true,default:"zh"}`, change-only (a second identical tick writes nothing); (b) an **unloaded** model → no write (loaded-gate, like voices); (c) a **clone** model (`SpeechClone=yes`) → no write; (d) `accepts:false` on a loaded plain-tts model → a row `{accepts:false,default:""}` is written (a real "no"); (e) a failed probe never reaches this step (verified by `probeAudioCppModels` leaving the last-good row intact + opening the backoff).
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/backend && go test ./cmd/gateway/ -run TestCacheAudioCppSpeechParams -v` → FAIL.
- [ ] **Step 3: Implement.** Add `speechParamsBackoff mappingBackoff` to `appHealthRunner`. `cacheAudioCppSpeechParams(ctx, mp, v)`: gate on `audioCppVoicesWanted(v)` (loaded + plain tts); on a cleared gate `speechParamsBackoff.clear(mp.ID)` and return; else `writeSpeechLanguageCache(ctx, mp.ID, v.AcceptsLanguage, v.LanguageDefault)` with the change-only policy (`writeVoicesCache` shape: read the row, compare payload, upsert only on change). Call `cacheAudioCppSpeechParams` from `probeAudioCppModels` right after `cacheAudioCppVoices`. (No separate fetch — it rides the verdict read the static pass already does.)
- [ ] **Step 4: Run to verify it passes, with the postgres leg.** Run: `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable go test ./cmd/gateway/ -run 'TestCacheAudioCppSpeechParams|TestProbeAudioCppModels' -v` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/cmd/gateway/app_health.go gateway/backend/cmd/gateway/app_health_audio_test.go
git commit -m "feat(gateway): cache the static audio.cpp speech-language fact in the health loop"
```

---

### Task 5: Gateway health loop (agent) — pull + cache the fact through the router, gated on the flag

**Files:**
- Modify: `gateway/backend/internal/gateway/agent_features.go` (add the mirror constant)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (add `probeAgentSpeechParams`; call it from the agent audio pass alongside `probeAgentVoices`)
- Test: `gateway/backend/internal/gateway/agent_features_test.go`, the agent audio app-health test file

**Interfaces:**
- Consumes: `provider.AudioCppModelsProber.ProbeAudioCppModels` against a `/upstream/{model}` endpoint (Task 2); `writeSpeechLanguageCache` (Task 4); `routing.SpecUpstreamAuth`, `routing.AgentRouterUpstreamPath`, `agentVoiceProbeSpec` (existing); `hasAgentFeature` (existing).
- Produces: `const RuntimeModelParamsFeature = "runtime_model_params"` (exported, agent-only mirror, **not** in `gatewayAgentFeatures`).

- [ ] **Step 1: Write the failing tests.** (a) `agent_features_test.go`: `TestRuntimeModelParamsFeatureName` pins `RuntimeModelParamsFeature == "runtime_model_params"` and asserts it is NOT in `gatewayAgentFeatures` (the `RuntimeVoicesFeature` precedent). (b) app-health: `TestProbeAgentSpeechParams` — for a running audio.cpp plain-tts mapping on a server whose agent **declares** `runtime_model_params`, the pass GETs `/upstream/<model>/v1/models?include_params=true` through the agent (spec credential attached) and writes the `speech_language` row; for a server whose agent does **not** declare it, no request is made and no row written (fail-closed).
- [ ] **Step 2: Run to verify they fail.** Run: `cd gateway/backend && go test ./internal/gateway/ -run TestRuntimeModelParamsFeatureName -v && go test ./cmd/gateway/ -run TestProbeAgentSpeechParams -v` → FAIL.
- [ ] **Step 3: Implement.** Add `RuntimeModelParamsFeature` to `agent_features.go` with a doc comment in the `RuntimeVoicesFeature` style. Add `probeAgentSpeechParams(ctx, server, app, prober)` modeled on `probeAgentVoices`: require `hasAgentFeature(server, RuntimeModelParamsFeature)` (fail-closed) at the caller; reuse `agentVoiceProbeSpec` for the eligible set (running + audio.cpp spec + plain-tts rows); build the target endpoint `ApplicationEndpoint(server, app) + ExpandModelPath(AgentRouterUpstreamPath, mp.AppModelName)` with the per-mapping `SpecUpstreamAuth`; call `ProbeAudioCppModels` against it, use **only** `AcceptsLanguage`/`LanguageDefault` (verdicts here come from telemetry — do not re-write them), and `writeSpeechLanguageCache`, sharing the `speechParamsBackoff` + leave-last-good policy. Wire the call into the agent audio pass next to `probeAgentVoices`.
- [ ] **Step 4: Run to verify they pass, with the postgres leg.** Run: `cd gateway/backend && OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable go test ./cmd/gateway/ -run 'ProbeAgentSpeechParams|ProbeAgentVoices' -v && go test ./internal/gateway/ -run RuntimeModelParams -v` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/internal/gateway/agent_features.go gateway/backend/internal/gateway/agent_features_test.go gateway/backend/cmd/gateway/app_health.go gateway/backend/cmd/gateway/app_health_audio_test.go
git commit -m "feat(gateway): pull the agent-managed audio.cpp speech-language fact fail-closed"
```

---

### Task 6: Gateway run — `ChatAudioSettings.Language` + inject into the speech body

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (`ChatAudioSettings`)
- Modify: `gateway/backend/internal/gateway/chat_runs_audio.go` (`speechBodyFields`)
- Test: `gateway/backend/internal/gateway/` speech body test (e.g. extend the existing `buildSpeechBody` / speech dispatch tests)

**Interfaces:**
- Produces: `ChatAudioSettings.Language string` (`json:"language,omitempty"`, declared **last**, after `KeepLastAudio`).

- [ ] **Step 1: Write the failing test** `TestBuildSpeechBodyLanguage`: (a) `Audio.Language="de"` → the body carries top-level `"language":"de"`; (b) `Language=""` → no `language` key; (c) `Language="de"` AND `Passthrough["language"]="en"` → body has `language":"de"` exactly once (first-class wins via `hasKeyFold`, passthrough skipped + logged); (d) a round-trip test in `service_chats` that `ChatAudioSettings` with a trailing `Language` preserves existing stored bytes for a doc without it.
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/backend && go test ./internal/gateway/ -run TestBuildSpeechBodyLanguage -v` → FAIL.
- [ ] **Step 3: Implement.** Add the trailing `Language` field to `ChatAudioSettings` with a doc comment twinned on `ChatTranscriptionSettings.Language` and the "declare last" note. In `speechBodyFields`, after `voice`/`speed` and before the passthrough merge, `if audio.Language != "" { body["language"] = audio.Language }`. `language` stays out of `speechReservedFields`.
- [ ] **Step 4: Run to verify it passes.** Run: `cd gateway/backend && go test ./internal/gateway/ ./internal/portal/ -run 'TestBuildSpeechBodyLanguage|ChatAudioSettings' -v` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/internal/portal/service_chats.go gateway/backend/internal/gateway/chat_runs_audio.go gateway/backend/internal/gateway/chat_runs_audio_test.go
git commit -m "feat(gateway): inject a per-thread speech language into /v1/audio/speech"
```

---

### Task 7: Gateway composer gate — `GET /api/portal/model-speech-language`

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_audio_stream.go` (or a focused `portal_model_speech_language.go`) — `handlePortalModelSpeechLanguage` + `portalSpeechLanguage` reader
- Modify: `gateway/backend/internal/gateway/server.go` (register the route)
- Test: the portal model-endpoints test file (beside `handlePortalModelSpeechStreaming`'s tests)

**Interfaces:**
- Consumes: `routing.ParseSpeechLanguage` + `routing.ModelMappingCacheSpeechLanguage` (Task 3); the model→plain-tts-mapping resolution the voice/speech-streaming gates already use (`ModelVoiceMappingIDs` / `MappingCapabilitiesForMappings` + `ModelMappingCacheForMappings`).
- Produces: `GET /api/portal/model-speech-language?name=<model>` → `{"accepts":true,"default":"zh"}` or `{"accepts":false}`.

- [ ] **Step 1: Write the failing test** `TestHandlePortalModelSpeechLanguage`: (a) a model with a plain-tts mapping carrying a cached `{accepts:true,default:"zh"}` → `200 {"accepts":true,"default":"zh"}`; (b) a model with no cached row / unknown / hidden / non-speech → `200 {"accepts":false}` (no-leak: all the same); (c) blank `name` → `400 model.name_required`; (d) store error → `500`; (e) nothing loads a model or contacts upstream.
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/backend && go test ./internal/gateway/ -run TestHandlePortalModelSpeechLanguage -v` → FAIL.
- [ ] **Step 3: Implement.** `portalSpeechLanguage(ctx, principal, model) (accepts bool, def string, err error)` resolving the model's portal-visible plain-tts speech mappings (the voice-gate resolution) and reading their `speech_language` cache; reconcile multiple mappings fail-safe (accepts only if a candidate's row says accepts; representative default). `handlePortalModelSpeechLanguage` mirrors `handlePortalModelSpeechStreaming` exactly (`requireWebScope(gateway:use)`, blank-name 400, store-error 500 only, every "no" the same 200). Register `/api/portal/model-speech-language` in `server.go`.
- [ ] **Step 4: Run to verify it passes.** Run: `cd gateway/backend && go test ./internal/gateway/ -run 'TestHandlePortalModelSpeechLanguage' -v` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/backend/internal/gateway/portal_model_speech_language.go gateway/backend/internal/gateway/server.go gateway/backend/internal/gateway/portal_model_speech_language_test.go
git commit -m "feat(gateway): add the model-speech-language composer gate endpoint"
```

---

### Task 8: Frontend — speech composer language control + i18n

**Files:**
- Modify: the speech composer component + its audio-settings hook/client (follow the `voice` combobox + `model-speech-streaming` gate call already there)
- Modify: `gateway/frontend/src/i18n.ts` (de + en keys together)
- Test: the composer's vitest spec

**Interfaces:**
- Consumes: `GET /api/portal/model-speech-language` (Task 7); writes `settings.audio.language` (Task 6's field).

- [ ] **Step 1: Write the failing vitest** for the control: it renders when the gate returns `accepts:true`; it is hidden/absent when `accepts:false`; selecting/typing a code sets `settings.audio.language`; when `accepts:true` and the returned `default` is not `auto`, it pre-selects the portal UI language (de/en); clearing it removes the field.
- [ ] **Step 2: Run to verify it fails.** Run: `cd gateway/frontend && npx vitest run <spec>` → FAIL.
- [ ] **Step 3: Implement** the control (combobox/free-text, like the voice picker), the gate fetch (mirroring the speech-streaming fetch), the pre-fill rule, and de+en i18n keys.
- [ ] **Step 4: Run to verify it passes + format.** Run: `cd gateway/frontend && npx vitest run <spec> && npm run build && npm run format:check` → PASS.
- [ ] **Step 5: Commit.**
```bash
git add gateway/frontend/src
git commit -m "feat(portal): add a speech language control to the chat composer"
```

---

### Task 9: Architecture docs

**Files:**
- Modify: `docs/architecture/cross-cutting/routing-and-model-selection.md` (the `model_mapping_cache` types + the speech-language gate), `docs/architecture/cross-cutting/agent-runtime-manager.md` (the `/v1/models` passthrough + `runtime_model_params` feature + version), `docs/architecture/reference/api-surface.md` + `docs/architecture/reference/openapi.yaml` (the new portal endpoint + the `ChatAudioSettings.language` field), and the ADR log / feature-flag list as the other audio PRs did.

- [ ] **Step 1: Update the docs** to describe the shipped behavior (new cache type, new agent route + flag at version 0.13.0, new portal endpoint, the composer control, the injected `language`). English; no LAN host names.
- [ ] **Step 2: Verify the docs gate.** Run: `./scripts/check-docs.sh` (with `docs/superpowers/**` moved aside) → OK; intra-repo links + anchors resolve, openapi `$ref`s resolve.
- [ ] **Step 3: Commit.**
```bash
git add docs/architecture
git commit -m "docs: document the speech-language cache, agent passthrough, and composer gate"
```

---

## Branch finalization (not a task — the PR gate)

Before the PR: merge `origin/main` and re-base the version bump on main's current `const Version` (serial audio PRs collide on it); run both Go modules' `golangci-lint fmt --diff` + `run` and `go test ./...` (gateway with the postgres leg), the frontend `build` + `format:check` + vitest, `make sonar-gate` + `make sonar-findings` + `make sonar-branch-findings`, and `./scripts/check-docs.sh`. Then **remove `docs/superpowers/**` and any `docs/implementation-status.md` from the branch** (fold durable content into `docs/architecture/` first) and verify with `git diff --name-only main...HEAD` that neither path appears. Push over SSH; open the PR with a substantive commit body (the squash description comes from it); enable Auto-fix. Verify end-to-end against a reachable audio.cpp if available (confucius4_tts + `language=de` → German; a no-`language` model unaffected); otherwise state what could not be verified.

# Streaming Speech Synthesis & Streamed-Response Metering — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Lift the `/v1/audio/speech` streaming refusal for a streaming-capable model, relay audio.cpp's PCM stream, and meter every streamed call with a real `(unit, quantity)` — `audio_second` when the model's output byte-rate is known, an honest `audio_bytes` fallback otherwise, never a silent zero.

**Architecture:** Mirror the shipped ADR-065 transcription-streaming machinery on the speech path. A new `speech_streaming` capability verdict (read from audio.cpp's per-model `mode`) decides stream-vs-buffered; the relay tees the SSE/raw PCM to count bytes and bills bytes ÷ a per-mapping cached byte-rate; a new auto-triggered speech benchmark learns and caches that byte-rate (and audio perf metrics); the portal TTS composer gains a streaming run. The output byte-rate is never guessed (ADR-041).

**Tech Stack:** Go (two modules: `gateway/backend` = `op-ai-gateway`, `server-agent` = `op-ai-server-agent`), SQLite/Postgres store, React+TS portal (Vite/vitest). Reuses `wavConsistentByteRate` (`wavduration.go`), the ADR-067 `model_mapping_cache`, the benchmark subsystem, and the ADR-063 loopback speech run.

**Spec:** `docs/superpowers/specs/2026-10-21-streamed-speech-metering-design.md` (read it alongside this plan — the plan argues from it).

## Global Constraints

- All repo-facing text (code, comments, commits, ADRs, docs) in **English**; chat with the author in German. **No LAN host names** anywhere in repo text — "an operator LAN host".
- Never commit/merge to `main`; this branch → **one PR**; enable Auto-fix on it.
- **Never guess the output byte-rate** (ADR-041): a wrong rate is a wrong bill, worse than a loud zero. It comes only from a real non-stream WAV `fmt` observation.
- **All failure/refusal/error rows stay `billing_unit = audio_second`, quantity 0** (endpoint identity, ADR-041(c)); only the success path may carry `audio_bytes`.
- audio.cpp streaming is **`mode=streaming`-gated** and **`response_format=pcm`-only** (wav→500); the terminal `speech.audio.done` has only `ttft_ms`.
- ServerAgent `const Version = "0.11.0"` → **`0.11.1`** (PATCH; no new `agent.Features`). Next ADR **068**. One migration **v95** (perf-metric column). **Re-base** the version, ADR number and migration number on `main` before finalizing (baseline high-water: migration v94, ADR-067).
- Pre-PR gates (all green): `golangci-lint fmt --diff` + `run ./...` for **both** Go modules; frontend `prettier --check` + `vitest run` + build; the **Postgres store leg** (`postgres:17-alpine`, DB `op_test`, `OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:postgres@127.0.0.1:5432/op_test?sslmode=disable`); `make sonar-gate` then `make sonar-branch-findings` (0 branch-attributed); `./scripts/check-docs.sh`; remove `docs/superpowers/**` + `docs/implementation-status.md` before the PR.

## Review Focus

Input classes the spec implies that each get a pinned test in the owning task:

1. **Streamed success, byte-rate not cached** → bills `(audio_bytes, pcmBytes)`, never `0`, never `audio_second` with a guessed rate. *(Task 4)*
2. **Stale `speech_streaming: yes`** (model switched to offline) → the stream 500s, the gateway retries buffered once and bills the buffered WAV correctly. *(Task 4)*
3. **Stream aborted / upstream error mid-stream** → a loud-zero `(audio_second, 0)` error row, never a partial over-bill; a clean-but-incomplete SSE (no `[DONE]`) is also loud-zero. *(Task 4)*
4. **Clone (`clon`) streaming model** → the benchmark skips it (a plain synth 500s without a reference), so it bills `audio_bytes`; streaming still works. *(Task 6)*
5. **Loaded-model identity changes under one mapping** → the cached byte-rate is re-derived (not left stale), so no wrong `audio_second` bill. *(Task 7)*

---

## File Structure

- `gateway/backend/internal/usage/billing.go` — the `audio_bytes` unit (Task 1).
- `gateway/backend/internal/gateway/wavduration.go` — a full-WAV byte-rate extractor (Task 2).
- `gateway/backend/internal/routing/store.go` — `CapabilitySpeechStreaming`, `ModelMappingCacheOutputByteRate` consts (Tasks 2, 3).
- `gateway/backend/internal/provider/audiocpp_models.go` + `server-agent/internal/collector/audiocpp.go` — the `speech_streaming` parser (Task 3).
- `gateway/backend/cmd/gateway/app_health.go` — verdict rows + the benchmark auto-trigger (Tasks 3, 7).
- `gateway/backend/internal/gateway/{speech_handler.go,speech_stream.go,native_passthrough.go}` — the relay + tee + billing (Tasks 4, 5).
- `gateway/backend/internal/gateway/benchmark_*.go` + `internal/store/{migrate.go,sqlite_benchmarks.go}` — the speech benchmark + v95 migration (Task 6).
- `gateway/backend/internal/portal/*` + `gateway/frontend/**` — the portal streaming run (Task 8).
- `docs/architecture/**` — ADR-068 + amendments (Task 9).

---

### Task 1: The `audio_bytes` billing unit

**Files:**
- Modify: `gateway/backend/internal/usage/billing.go` (const block near `:24`, `ValidBillingUnit` `:42`)
- Test: `gateway/backend/internal/usage/billing_test.go`

**Interfaces:**
- Produces: `usage.BillingUnitAudioBytes = "audio_bytes"`; `ValidBillingUnit("audio_bytes") == true`; `ValidateBillingXOR` passes for a row with `BillingUnit=audio_bytes`, `BillingQuantity>0`, all seven token columns zero.

- [ ] **Step 1: Write failing tests** in `billing_test.go`: `TestValidBillingUnitAcceptsAudioBytes` (asserts `ValidBillingUnit(BillingUnitAudioBytes)`), and `TestValidateBillingXORAudioBytesRow` (an `audio_bytes` row with quantity 2048 and zero token counts/rates returns nil; the same with a non-zero `OutputTokens` returns an error).
- [ ] **Step 2: Run — expect FAIL** (`cd gateway/backend && go test ./internal/usage/ -run 'AudioBytes' -v`), undefined `BillingUnitAudioBytes`.
- [ ] **Step 3: Implement** — add `BillingUnitAudioBytes BillingUnit = "audio_bytes"` to the const block and the `ValidBillingUnit` switch/set. No change to `ValidateBillingXOR` (it is unit-agnostic for non-token rows).
- [ ] **Step 4: Run — expect PASS** (same command).
- [ ] **Step 5: Commit** — `feat(usage): add the audio_bytes billing unit for streamed audio payloads`.

---

### Task 2: Output byte-rate — WAV extractor + per-mapping cache type

**Files:**
- Modify: `gateway/backend/internal/gateway/wavduration.go` (add extractor beside `wavConsistentByteRate` `:172`, `wavReadFmtByteRate` `:102`)
- Modify: `gateway/backend/internal/routing/store.go` (`ModelMappingCacheOutputByteRate` beside `ModelMappingCacheVoices` `:1384`)
- Modify: `gateway/backend/internal/gateway/` (a small read helper `cachedOutputByteRate`)
- Test: `gateway/backend/internal/gateway/wavduration_test.go`, a cache read/write test

**Interfaces:**
- Produces: `wavByteRateFromReader(r io.Reader) (uint32, bool)` in `wavduration.go` — RIFF/WAVE → `fmt` → byte-rate, `(0,false)` for a non-WAV or inconsistent `fmt` (reuses `wavConsistentByteRate`). `routing.ModelMappingCacheOutputByteRate = "output_byte_rate"`. `cachedOutputByteRate(ctx, routeID) (uint32, bool)` reading `ModelMappingCache(..., ModelMappingCacheOutputByteRate)` and parsing `payload`.

- [ ] **Step 1: Write failing tests** — `TestWavByteRateFromReader` (a `buildPCMWav(t,44100,1,16,dataBytes)` fixture → `(88200,true)`; a 24000/1/16 fixture → `(48000,true)`; a truncated/`fmt`-inconsistent fixture → `(0,false)`); `TestCachedOutputByteRate` (upsert `output_byte_rate` payload "88200" for a mapping, read back `(88200,true)`; absent → `(0,false)`).
- [ ] **Step 2: Run — expect FAIL** (`go test ./internal/gateway/ -run 'ByteRate' -v`).
- [ ] **Step 3: Implement** — `wavByteRateFromReader` walks RIFF→`fmt` (reuse `wavReadFmtByteRate`/`wavConsistentByteRate`, no full-body read). Add the cache const. `cachedOutputByteRate` reads via the existing `ModelMappingCache`/`ModelMappingCacheForMappings` store methods and `strconv.ParseUint` on `payload`.
- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Commit** — `feat(gateway): extract WAV byte-rate and add the output_byte_rate per-mapping cache type`.

---

### Task 3: The `speech_streaming` verdict (gateway + agent)

**Files:**
- Modify: `gateway/backend/internal/routing/store.go` (`CapabilitySpeechStreaming` beside `:1498`)
- Modify: `gateway/backend/internal/provider/audiocpp_models.go` (`AudioCppVerdicts.SpeechStreaming`, `audioCppStreamingVerdict` `:338`, `audioCppModelCases`)
- Modify: `server-agent/internal/collector/audiocpp.go` (duplicate parser `:116/:151`, shared `audioCppModelCases`), `server-agent/internal/collector/probe.go` (`Capabilities.SpeechStreaming` `:447/:470`)
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`audioCppCapabilityRows` `:658`)
- Modify: `gateway/backend/internal/gateway/agent_ingest.go` (`audioCppProbeCapabilities` `:1280`)
- Modify: `gateway/backend/internal/portal/service_applications.go` (`reservedManualVerdicts` `:357`)
- Modify: `server-agent/internal/agent/agent.go` (`capabilitiesSample` `:1955`, `Version` `:318`, version-history comment `:257`)
- Test: `audiocpp_models_test.go` (both modules), `agent_ingest` test, `TestFeatureRegistry`, `TestAudioCppModelsCasesMatchTheAgentsTable`

**Interfaces:**
- Consumes: nothing from prior tasks — this task defines the verdict and its const.
- Produces: `routing.CapabilitySpeechStreaming = "speech_streaming"`; a `speech_streaming` verdict (`yes` for a `tts`/`clon` model loaded `mode=streaming`, `no` for offline, nothing for an unnamed/unknown model), written on the `audiocpp_models` source; readable via `MappingCapabilities` + `CapabilityRowsByName`. Agent reports it on the existing `capability_source_audiocpp` path. `agent.Version == "0.11.1"`.

- [ ] **Step 1: Write failing tests** — gateway `TestParseAudioCppModelsSpeechStreaming` (a `tts` entry `mode:"streaming"` → `SpeechStreaming==yes`; `mode:"offline"` → `no`; a `clon` streaming entry → `yes`; an `asr` entry leaves `SpeechStreaming` unset); the parity test `TestAudioCppModelsCasesMatchTheAgentsTable` extended for the new column; `TestAudioCppProbeCapabilitiesAllowsSpeechStreaming` (ingest allowlist); `TestReservedManualVerdictSpeechStreamingNo`; `TestFeatureRegistry` (Version 0.11.1, no new `agent.Features`).
- [ ] **Step 2: Run — expect FAIL** (`cd gateway/backend && go test ./internal/provider/ ./internal/gateway/ ./internal/portal/ -run 'SpeechStreaming|Feature' -v`; `cd server-agent && go test ./... -run 'AudioCpp|Feature' -v`).
- [ ] **Step 3: Implement** — mirror every `transcription_streaming` point: add the const; add `SpeechStreaming` to `AudioCppVerdicts`/`Capabilities` and derive it in `audioCppStreamingVerdict` (read `mode` for `tts`/`clon`, not only `asr`); update the shared `audioCppModelCases` table in both modules; emit the row in `audioCppCapabilityRows` for a speech model; add the name to `audioCppProbeCapabilities` (NOT to `reservedAgentCapabilityNames`); add `{No:true}` to `reservedManualVerdicts`; emit it in `capabilitiesSample` beside the speech verdict; bump `Version` to `"0.11.1"` with a version-history comment entry.
- [ ] **Step 4: Run — expect PASS** (both modules).
- [ ] **Step 5: Commit** — `feat(audio): add the speech_streaming capability verdict from audio.cpp mode (agent 0.11.1)`.

---

### Task 4: Streamed speech relay + `audio_bytes` billing

**Files:**
- Modify: `gateway/backend/internal/gateway/speech_handler.go` (`validateSpeechRequest` `:191`, `relaySpeech` `:308`; add `mappingStreamsSpeech`)
- Create: `gateway/backend/internal/gateway/speech_stream.go` (`speechStreamScan`, `setStreamedSpeechBillingQuantity`)
- Modify: `gateway/backend/internal/gateway/native_passthrough.go` (speech stream dispatch `:839/:904`, Content-Length skip, retry-buffered-on-500)
- Test: `gateway/backend/internal/gateway/speech_stream_test.go`, `speech_handler_test.go`

**Interfaces:**
- Consumes: Task 1 (`BillingUnitAudioBytes`), Task 3 (`speech_streaming` verdict read).
- Produces: `mappingStreamsSpeech(ctx, routeID) bool`; `speechStreamScan` with `feed`/`finish`/`complete()`/`pcmBytes()` (sums decoded SSE `speech.audio.delta` payloads, or raw body bytes; `complete()` = `speech.audio.done`+`[DONE]` for SSE, clean HTTP close for raw); `setStreamedSpeechBillingQuantity(meta *usageMeta, ex nativeExchange, scan *speechStreamScan, status string)` — on `success`+`complete`, `meta.BillingUnit=BillingUnitAudioBytes`, `meta.BillingQuantity=pcmBytes` (Task 5 upgrades to seconds); else loud-zero `(audio_second,0)` + one `Error` log.

- [ ] **Step 1: Write failing tests** — `TestSpeechStreamsOnlyWhenVerdictYes` (verdict yes + `stream=true` → SSE relayed; verdict absent → buffered WAV, `stream` dropped); `TestSpeechStreamBillsAudioBytesOnCleanCompletion` (SSE of N delta bytes + done + `[DONE]` → `(audio_bytes, N)`, no Error log); `TestSpeechStreamLoudZeroOnIncompleteStream` (done without `[DONE]` → `(audio_second,0)` + Error); `TestSpeechStreamRetriesBufferedOn500` (a `yes` that draws the `mode=streaming` 500 → one buffered retry, `audio_second` from the header); `TestSpeechRejectsStreamWithWavFormat` (`stream=true,response_format=wav` → 400 up front); `TestSpeechStreamRawBytesBilled` (`stream_format=audio` → `(audio_bytes, rawBytes)`); Review-Focus #3 abort case → loud-zero.
- [ ] **Step 2: Run — expect FAIL** (`go test ./internal/gateway/ -run 'SpeechStream|SpeechReject' -v`).
- [ ] **Step 3: Implement** — relax `validateSpeechRequest` to admit `stream=true`+pcm/absent, reject `stream`+`wav` and a non-bool `stream`; add `mappingStreamsSpeech`; in `relaySpeech`/`native_passthrough` take the ADR-065 stream branch (set `pfReq.Stream`, skip Content-Length, tee through `speechStreamScan`, retry-buffered-on-500); wire `setStreamedSpeechBillingQuantity` at the terminal speech `recordUsage` beside `setSpeechBillingQuantity` (`native_passthrough.go:904`); keep the capture backstop redacting the stream.
- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Commit** — `feat(speech): relay streamed speech and bill it in audio_bytes on clean completion`.

---

### Task 5: Upgrade streamed billing to `audio_second` when the rate is cached

**Files:**
- Modify: `gateway/backend/internal/gateway/speech_stream.go` (`setStreamedSpeechBillingQuantity`)
- Test: `gateway/backend/internal/gateway/speech_stream_test.go`

**Interfaces:**
- Consumes: Task 2 (`cachedOutputByteRate`), Task 4 (`speechStreamScan.pcmBytes()`).
- Produces: `setStreamedSpeechBillingQuantity` now, on clean completion, reads `cachedOutputByteRate`; if present → `(audio_second, pcmBytes / byteRate)`; else → `(audio_bytes, pcmBytes)` (Task 4 behavior).

- [ ] **Step 1: Write failing test** — `TestSpeechStreamBillsAudioSecondWhenRateCached` (seed `output_byte_rate=88200`, stream 176400 PCM bytes cleanly → `(audio_second, 2.0)`, no Error); and `TestSpeechStreamFallsBackToAudioBytesWithoutRate` (no cached rate → `(audio_bytes, 176400)`).
- [ ] **Step 2: Run — expect FAIL** (the second test already passes from Task 4; the first fails — still billed audio_bytes).
- [ ] **Step 3: Implement** — in `setStreamedSpeechBillingQuantity`, look up `cachedOutputByteRate(ctx, ex.route)`; on hit set `BillingUnit=audio_second`, `BillingQuantity=float64(pcmBytes)/float64(byteRate)`; on miss keep the `audio_bytes` branch.
- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Commit** — `feat(speech): bill streamed speech in audio_second when the output byte-rate is cached`.

---

### Task 6: The output-byte-rate benchmark (+ v95 migration + perf metrics)

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (v95: nullable audio perf-metric columns on `model_mapping_benchmarks`), `internal/routing/store.go` (`BenchmarkRun` fields), `internal/store/sqlite_benchmarks.go`
- Modify: `gateway/backend/internal/gateway/benchmark_runner.go` (a speech-measurement path modeled on `runVisionTarget` `:937`), `benchmark_refusals.go` (`partitionBenchmarkViews` `:249` — admit speech for the speech benchmark), `benchmark_endpoints.go` (a speech `?mode=`)
- Test: `benchmark_*_test.go`, `migration95_*_test.go`

**Interfaces:**
- Consumes: Task 2 (`wavByteRateFromReader`, the cache const), Task 3 (verdict, to target streaming speech mappings).
- Produces: a benchmark path that, for a speech-capable mapping, issues a non-stream `POST /v1/audio/speech` over the **internal loopback pair** (ADR-063 mechanism, system run-as), reads `wavByteRateFromReader` from the response, writes `output_byte_rate` to `model_mapping_cache`, and records a `Kind:"speech"` history row with ttft/RTF/wall; `partitionBenchmarkViews` no longer refuses a speech mapping for this benchmark; a `clon` mapping is **skipped** (no reference).

- [ ] **Step 1: Write failing tests** — `TestSpeechBenchmarkCachesByteRate` (against a WAV-returning test upstream → `output_byte_rate` cached, a `speech` benchmark history row with perf metrics); `TestSpeechBenchmarkRunsOnSpeechMapping` (the audio-only refusal is lifted for this path; the chat benchmark still refuses); `TestSpeechBenchmarkSkipsCloneModel` (Review-Focus #4); the v95 migration test (`migration95_..._test.go`, columns exist, nullable, backfill sane).
- [ ] **Step 2: Run — expect FAIL** (`go test ./internal/gateway/ ./internal/store/ -run 'SpeechBenchmark|migration95' -v`).
- [ ] **Step 3: Implement** — add the v95 migration (re-base number on `main`); add the `BenchmarkRun` perf fields + `InsertBenchmarkRun` Kind; add the speech-measurement function (loopback non-stream synth, `wavByteRateFromReader`, `UpsertModelMappingCache(output_byte_rate)`, history row); invert the audio-only split in `partitionBenchmarkViews` for the speech benchmark; skip `clon`.
- [ ] **Step 4: Run — expect PASS**, then run the Postgres leg for the migration (`OP_AI_GATEWAY_TEST_POSTGRES_DSN=…`).
- [ ] **Step 5: Commit** — `feat(benchmark): add a speech benchmark that caches the output byte-rate and records audio perf metrics`.

---

### Task 7: Auto-trigger the rate benchmark from the health loop

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (the audio probe pass, beside the voices cache fill)
- Test: `gateway/backend/cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: Task 3 (`speech_streaming` verdict), Task 6 (the speech benchmark + `TryStart`), Task 2 (`cachedOutputByteRate`).
- Produces: on a health tick, a `speech_streaming: yes` mapping with **no cached byte-rate** (or a changed loaded-model identity) **triggers the speech benchmark once** via the registry's `TryStart` (async, non-blocking); it does not re-trigger while a rate is cached for the current model identity.

- [ ] **Step 1: Write failing tests** — `TestHealthTriggersSpeechRateBenchmarkOnce` (a streaming speech mapping with no cached rate → one `TryStart`; a second tick with the rate now cached → no trigger); `TestHealthReBenchmarksOnModelIdentityChange` (Review-Focus #5: the loaded model path/identity changes → the rate is re-derived).
- [ ] **Step 2: Run — expect FAIL** (`go test ./cmd/gateway/ -run 'SpeechRateBenchmark|ModelIdentity' -v`).
- [ ] **Step 3: Implement** — in the audio probe pass add the gate (verdict yes + no cached rate / changed identity) → `registry.TryStart` the speech benchmark; record the model identity alongside the cached rate so a change re-triggers. The health loop itself never synthesizes.
- [ ] **Step 4: Run — expect PASS.**
- [ ] **Step 5: Commit** — `feat(gateway): auto-trigger the speech rate benchmark from the health loop`.

---

### Task 8: Portal speech-streaming run

**Files:**
- Modify: the portal speech run (ADR-063 `executeSpeechRun` in `internal/portal/`), a model-rate read endpoint if needed
- Modify: `gateway/frontend/**` (the TTS composer: progressive PCM playback, a stream affordance, the cached-rate gate)
- Test: backend run test; `gateway/frontend/**` vitest

**Interfaces:**
- Consumes: Task 4/5 (the streamed relay + billing), Task 2 (the cached rate, surfaced to the client for playback).
- Produces: a streaming branch of the portal speech run that POSTs `stream=true` over loopback, relays `speech.audio.delta` to the client, and finalizes on `done`+`[DONE]` storing the assembled WAV blob; the composer plays PCM progressively **only** when the model is `speech_streaming: yes` and the rate is known, else falls back to the buffered WAV run.

- [ ] **Step 1: Write failing tests** — backend `TestSpeechRunStreamsWhenCapableAndRateKnown` / `TestSpeechRunFallsBackToBufferedWithoutRate`; frontend vitest for the composer's stream affordance gated on the capability+rate and the progressive-playback reducer.
- [ ] **Step 2: Run — expect FAIL** (backend `go test`; `cd gateway/frontend && npx vitest run <paths>`).
- [ ] **Step 3: Implement** — the backend streaming branch (reuse the run reservation/delta/checkpoint plumbing, ADR-065(e) shape); surface the sample rate to the client; the composer's AudioWorklet/Web-Audio progressive playback + the gate.
- [ ] **Step 4: Run — expect PASS**; `prettier --check` + build.
- [ ] **Step 5: Commit** — `feat(portal): stream synthesized speech progressively in the TTS composer`.

---

### Task 9: ADR-068 + documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-068; amend ADR-041, ADR-061), `cross-cutting/compatibility-and-inference.md` (§3.6), `reference/api-surface.md` + `openapi.yaml`, `11-risks-and-technical-debt.md` (§11.4), `reference/data-model.md` (v95 + the cache type), `12-glossary.md`
- Test: `./scripts/check-docs.sh`

**Interfaces:**
- Consumes: the behavior landed in Tasks 1–8 (documents it; adds nothing new).

- [ ] **Step 1: Write ADR-068** — "Streaming speech synthesis": the `speech_streaming` verdict, the SSE/raw relay, the byte-count metering with the `audio_bytes` fallback and the cached byte-rate, the auto-triggered speech benchmark, the portal run; **amend ADR-041** (the `audio_bytes` unit + the success-path measurement-unit refinement) and **ADR-061** (streaming lifted). Ground every claim in the live contract (spec §2). Re-base the ADR number on `main`.
- [ ] **Step 2: Update** compat §3.6, api-surface + openapi, risks §11.4 (named residuals: the streamed-vs-buffered rate assumption, the raw-path completion residual, clone→audio_bytes), data-model (v95 + `output_byte_rate` cache type), glossary (`audio_bytes`).
- [ ] **Step 3: Run — expect PASS** `./scripts/check-docs.sh`.
- [ ] **Step 4: Commit** — `docs(adr): ADR-068 streaming speech synthesis and streamed-response metering`.

---

## Notes for the executor

- Tasks are ordered by dependency: 1→2 are foundational; 3 (verdict) gates 4; 4→5 build the relay+billing incrementally; 6 (benchmark) feeds the cache 5 reads; 7 auto-triggers 6; 8 is the portal UX; 9 documents it all.
- Before finalizing: `git merge origin/main`, then re-base the agent version, the ADR number and the v95 migration number on `main`'s then-current values; re-run the live contract check (spec §2) against the operator audio.cpp host; run the full gate list (Global Constraints) including the Postgres leg and `make sonar-branch-findings` (must be 0).

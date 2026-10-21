# Design — Streaming speech synthesis and streamed-response metering

**Issues:** #263 (metering a streamed audio response) folded into #253 part 2 (speech
streaming). Branch-local working spec; removed before the PR. The permanent record is a new
ADR-068 plus amendments to ADR-041 and ADR-061.

**Status:** awaiting approval (nothing is built until this is approved).

---

## 1. Intent and why

`POST /v1/audio/speech` relays a single complete WAV and bills `audio_second` from the
`X-AudioCPP-Audio-Duration-Ms` response header (ADR-061(g)). Streaming is refused
(`speech.stream_unsupported`, ADR-061(e)) because the relay's "one complete response"
contract carries the duration. #253 wants streaming; #263 is the metering prerequisite,
because a streamed response carries **no** single-response duration, so a naive streaming
relay would bill `audio_second` quantity `0` on every call.

This design lifts the streaming refusal for a streaming-capable model, relays audio.cpp's
PCM stream, and meters it correctly — producing a real, honest `(unit, quantity)` on every
call, never a silent zero. The transcription half of #263 already shipped (ADR-065, issue
#262, closed): a streamed transcription bills from the **uploaded** WAV header. Speech is
the mirror: the billable duration is the **output** audio, which the stream does not
describe, so speech needs its own answer — recorded here.

## 2. Verified live contract (the grounding)

Probed live 2026-10-21 against the operator audio.cpp host (v0.9.1, model `audio8-tts`,
family `audio8_tts`, 44100 Hz / mono / 16-bit). Model restored to `mode:offline` after.

1. **Streaming requires `mode=streaming`.** With an `offline` model every streamed speech
   request → `500 {"error":{"message":"speech streaming requires a model configured with
   mode=streaming"}}`. Same gate audio.cpp uses for transcription (ADR-065).
2. **A `mode=streaming` model is a superset.** It serves a streamed request **and**, for
   `stream=false`, still returns `200 audio/wav` with `X-AudioCPP-Audio-Duration-Ms`
   (2925.7 ms observed). Only `offline` is restrictive.
3. **Streamed output is headerless PCM, with no duration and no format on the wire.**
   - `stream=true` (default) and `stream_format=sse` → `Content-Type: text/event-stream`,
     chunked SSE: `speech.audio.delta` (base64 PCM) ×N → `speech.audio.done`
     (**only `timing.ttft_ms`, no `audio_duration_ms`**, two probes) → `[DONE]`.
   - `stream_format=audio` → `application/octet-stream`, chunked, **headerless raw PCM**
     (first bytes `e7 ff ea ff…`, not `RIFF`).
   - Streaming accepts **`response_format=pcm`/absent only**; `response_format=wav` while
     streaming → `500 "streaming speech currently supports response_format=pcm"`. So there
     is never a WAV container (hence no `fmt` chunk) on the stream.
   - No `X-AudioCPP-Audio-Duration-Ms` header, and no API endpoint exposes the output
     sample rate (`GET /v1/models/{id}` → 404; `package-sizes` carries no rate). The rate
     is the model's native rate — stable per model, unexposed.
4. **The duration header is exactly `data_bytes ÷ byteRate`.** Baseline WAV: 331776 data
   bytes ÷ 88200 = 3.7616 s = the header's 3761.633 ms. So counting the streamed PCM bytes
   and dividing by the byteRate is the **identical** number audio.cpp reports — not an
   approximation. Verified: streamed raw PCM (319488 B) and SSE delta sums all divide by
   88200 to plausible durations; only 88200 fits (24 kHz would give impossible lengths), so
   the **streamed PCM byteRate equals the non-stream WAV byteRate** for the same model.

**Deployment context (operator-confirmed):** audio.cpp is normally off and started per
**RunSpec** (agent-managed); there are no manual changes outside the RunSpec. So
streaming-capable models are launched directly as `mode=streaming` (the superset), and the
response type (streamed vs buffered) is chosen per request. The gateway follows `mode`; it
never flips it.

## 3. Scope

**Goals**
- Lift the speech streaming refusal for a streaming-capable model and relay audio.cpp's SSE
  / raw-PCM stream unbuffered.
- Meter a streamed speech call correctly: a real `(unit, quantity)` on every call, no
  silent zero.
- A `speech_streaming` capability verdict driving the stream-vs-buffered decision, mirroring
  ADR-065's `transcription_streaming`.
- A new `audio_bytes` billing unit as the honest fallback measure when the output byte-rate
  is not yet known.
- A streaming-speech **benchmark** that determines the output byte-rate automatically and
  persists it per mapping, so streamed calls reach `audio_second` without depending on
  organic non-stream traffic; it also records audio **perf metrics** (ttft/RTF/wall).
- A **portal speech-streaming run** — the TTS composer plays the synthesized audio as it
  streams, mirroring ADR-065(e)'s transcription portal run (§4.8).

**Non-goals (explicitly out)**
- **Non-WAV output formats** (mp3/opus/flac) — #253 part 1, independent, not here.
- **Changing non-streaming speech or any transcription metering** — ADR-061(g) /
  ADR-062(e) / ADR-065 are unchanged.
- **Live transcription (#266)** — "transcribe while speaking" (live mic *input* over a
  WebSocket to audio.cpp `/v1/audio/transcriptions/live`) is a separate feature; this PR's
  portal work is the speech *output* composer (§4.8), not #266.
- **The gateway setting a model's `mode`** — mode is a RunSpec/config property; the gateway
  only reads and follows it.

**Anti-goals (would make this a failure even if it "works")**
- Guessing the output byte-rate / sample rate. ADR-041: a wrong rate is a wrong bill, worse
  than a loud zero. The rate must come from a real observation (the benchmark's non-stream
  `fmt` header), never a constant or a per-model assumption.
- A second audio code path. Reuse the speech relay and the ADR-065 streaming machinery.
- Billing a partial/aborted stream as if complete.

## 4. Decisions

### 4.1 `speech_streaming` verdict (mirror ADR-065(b),(f))
A per-mapping capability `speech_streaming`: **yes** = the mapped model is `mode=streaming`,
**no** = offline, **absent** = unknown. Written **only for a speech (tts/clon) model** —
the axis on which streaming is a question — from the same per-model `mode` field of
audio.cpp's `/v1/models` that ADR-065 reads for `transcription_streaming`, on the same
`audiocpp_models` probe source. The gateway health loop writes it for a direct `audio_cpp`
app; the ServerAgent decodes `mode` and reports it for an agent-launched child, riding the
existing `capability_source_audiocpp` one-sided flag and the `audiocpp_models` source as
**one new capability name** → a **PATCH** agent bump, **no new `agent.Features`**, **no
migration** (the capability vocabulary is open, ADR-062(c)). `(speech_streaming, no)` is
reserved against a manual write and against a probe being outranked by a stale manual
override, as the other audio verdicts are.

The handler reads the verdict keyed on the resolved route (`MappingCapabilities`), like
ADR-065: **yes** + client asked `stream=true` → stream; **no / absent / read error** →
buffered one-shot (drop `stream`). An unprobed or offline model simply buffers; a client
that asked `stream=true` against one gets a valid buffered WAV, never an error.

### 4.2 Streamed speech relay (mirror ADR-065(a),(b))
- `validateSpeechRequest` stops refusing `stream=true`. When the gateway streams it sends
  `stream=true` upstream and relays the event stream / raw PCM **chunked, byte for byte**.
- **Still refused** (400 `speech.stream_unsupported`): a non-boolean `stream`. **Still
  refused** while streaming: `response_format=wav` (audio.cpp 500s it) — streaming requires
  `response_format=pcm`/absent, validated up front so the gateway never forwards a request
  audio.cpp will 500. `stream_format` `sse` and `audio` are **relayed** (audio.cpp emits its
  own shapes; the gateway translates nothing).
- **Retry-buffered-on-500 safety net:** if the gateway streamed on a stale `yes` and the
  upstream answers `500 "speech streaming requires a model configured with mode=streaming"`,
  re-send once with `stream` dropped and return the buffered WAV. Nothing of the failed
  stream has been written yet. Any other upstream error surfaces as ADR-061(i) relays it.
- **Content-Length:** the speech relay forwards the upstream Content-Length
  (`native_passthrough.go:839`); a chunked stream has none, so this is **skipped** for the
  streamed case.
- **Capture:** a streamed speech response stays a **redacted binary summary** (it is audio
  output), mirroring ADR-061(h) — **not** assembled like a transcription (ADR-065(d)). The
  SSE body is `text/event-stream`, so the capture must be stamped so the binary-speech
  backstop still redacts it rather than storing the event text.

### 4.3 Metering — the billing hierarchy
On the streamed success path the gateway counts the relayed PCM bytes: for SSE, the sum of
the decoded base64 `speech.audio.delta` payloads; for raw audio, the raw body bytes. Clean
completion = SSE `speech.audio.done` + `[DONE]` both seen, or a raw body the upstream closed
without an error status mid-stream.

| Case | Billed |
|---|---|
| Clean completion **and** output byte-rate known for the mapping | `(audio_second, pcmBytes ÷ byteRate)` — exact, consistent with the non-stream header |
| Clean completion, byte-rate **not yet known** | `(audio_bytes, pcmBytes)` — the fallback, exact byte count, never guessed |
| Refusal / upstream error / stream aborted before clean completion | `(audio_second, 0)` — uniform with every other audio error row (ADR-061/062/065) |

`audio_bytes` replaces the loud-zero that would otherwise fire on the **common** case (clean
success before the rate is known). The loud zero remains only where nothing measurable
arrived.

### 4.4 The new `audio_bytes` billing unit (amends ADR-041)
Add `BillingUnitAudioBytes = "audio_bytes"` to the code-open vocabulary in
`internal/usage/billing.go` (`ValidBillingUnit` accepts it). Meaning: relayed PCM payload
bytes. `ValidateBillingXOR` is already generic (a non-token row must zero the seven
token-denominated columns; audio has no usage scanner, so they are zero) and needs no change.
No migration (`billing_unit` is an open text column, `billing_quantity` is `double`).

**Interaction with ADR-041(c) ("a unit may only come from endpoint identity, never from the
response").** Preserved: the unit stays audio-derived, **all failure rows remain
`audio_second`** (the endpoint-identity unit, so a failed streamed request is never
mislabelled), and only the **success** path chooses between `audio_second` and `audio_bytes`
from **mapping/cache state** (is the byte-rate known?), not from the response body. ADR-068
records this as a deliberate ADR-041 amendment: a non-token endpoint may carry a success-path
*measurement unit* refinement, with failure rows uniform.

**Pricing/energy:** `audio_bytes` is `unpriceable` in Tier 3 unless a coefficient with the
declared unit `audio_bytes` exists (ADR-041's rule for any new unit) — honestly unpriced
until an operator adds one or converts the rows to seconds post-hoc. `NonTokenRequests`
already counts any `billing_unit <> ''` row, so disclosure works unchanged. A portal unit
label is optional and not required for correctness.

### 4.5 The output byte-rate: what it is, source, persistence
The **byte-rate** here is the PCM **format** rate — `sampleRate × channels × bytesPerSample`
as one integer (88200 B/s for the probed model), the constant that converts PCM bytes to
seconds. It is **not** throughput or `bytes ÷ wall-time` (a performance figure that varies per
run); billing from that would be a wrong bill. It is read from the **`fmt` chunk of a
non-stream WAV response** via the existing `wavConsistentByteRate` (`wavduration.go`, which
self-consistency-checks the chunk); cross-checked by `data_bytes ÷ (X-AudioCPP-Audio-Duration-Ms
÷ 1000)`, which equals it exactly. It is a **stable per-mapping property**.

**Persistence: the generic per-mapping cache, no migration.** The ADR-067 cache
(`model_mapping_cache`, keyed by `(mapping_id, cache_type, scope_key)`) is generic — voices is
just the first `cache_type`. The byte-rate is the second consumer: a new const
`ModelMappingCacheOutputByteRate = "output_byte_rate"`, the integer in `payload`, `scope_key`
`""`. It inherits FK-cascade and app-model-name rename-invalidation; **no new table or
migration**. Read on the billing hot path via `ModelMappingCacheForMappings`.

**Source: the streaming-speech benchmark (§4.6)** writes it — a synthesis is needed, so the
health loop stays cheap and does not synthesize. Until a mapping has been benchmarked, streamed
calls bill `audio_bytes` — honest, never zero.

### 4.6 The streaming-speech benchmark (new)
Audio models are excluded from benchmarks today (`benchmark.audio_only` 409, ADR-061(b)) and
the benchmark engine measures via **streaming chat completions** (`provider.StreamingClient`,
`benchmarkTargetReq`/`streamOnce`/`measureMapping`), which a native speech relay cannot reuse.
So add a **new speech-measurement path** modeled on `runVisionTarget` (the existing template
that runs a measurement, then writes a capability row at source rank and a kind-tagged history
row). It:
- **inverts** the audio-only gate — it runs exactly on the speech-capable mappings that
  `partitionBenchmarkViews` currently refuses; speech stays excluded from the **chat**
  benchmark;
- sends a short **non-stream** `POST /v1/audio/speech` through the normal relay (direct or
  agent-launched over loopback), reads the byte-rate from the WAV `fmt` header, and writes it
  to `model_mapping_cache` (§4.5);
- **records** audio perf metrics (wall, RTF, ttft) as a new benchmark `Kind` (a nullable
  column on `model_mapping_benchmarks` → the one migration, §7) — audio models gain the
  benchmark coverage they lack today.
Derived gateway-side, so **no agent change** beyond the §4.1 verdict.

### 4.7 Resolved design points (from the builder-check)
1. **The benchmark trigger is automatic.** The health loop already visits each audio mapping
   while the model is loaded (the ADR-067 voices gate). When it sees a `speech_streaming: yes`
   mapping with **no cached byte-rate** (or a changed loaded-model identity), it **triggers**
   the speech-rate benchmark once via the benchmark registry's `TryStart` — async; the health
   loop never synthesizes inline — so a streaming mapping has its rate before any client
   streams. This lifts the `benchmark.audio_only` refusal **for the speech-rate benchmark path
   only**; the chat benchmark still refuses audio. The benchmark is also manually triggerable
   like the others. (Without this, a mapping bills `audio_bytes` until an operator runs a
   manual benchmark — which contradicts "automatic".) *Scope note: this adds a new auto-trigger
   path (health-loop gate + lifting the audio-only refusal on that path), beyond a bare
   `runVisionTarget` clone.*
2. **Rate-source mechanism.** The benchmark issues a non-stream `POST /v1/audio/speech` over
   the **internal loopback pair** (the ADR-063 portal-speech-run mechanism, with a system
   run-as principal), so it reaches **both** a direct `audio_cpp` app and an agent-launched
   child uniformly through the gateway's own route resolution, then reads the byte-rate from
   the response WAV `fmt` header. It does **not** reuse the chat `StreamingClient` engine.
3. **Streamed-vs-buffered rate equality (the correctness assumption).** A streamed call bills
   PCM-bytes ÷ a rate read from a *non-stream* WAV, so it assumes audio.cpp streams at the
   model's **native rate = the buffered WAV's rate**. Verified equal for the probed model (both
   44100), and it is **also required for the client**, who receives headerless PCM and can only
   play it at a single known rate consistent with the buffered WAV — a divergent streaming rate
   would break playback, not just billing. Named risk: a future model/engine that resampled
   only the stream would mis-bill; mitigated by the operator's trusted server, a re-check on
   audio.cpp upgrade, and **re-benchmark on a loaded-model-identity change** (not only on
   app-model-name rename, which is all ADR-067's cache invalidation covers) to close the
   hot-swap staleness window.
4. **Clone (`clon`) models bill `audio_bytes`.** A clone model synthesizes only in a supplied
   reference voice (a server-side reference WAV path + mandatory `reference_text`, verified
   #272), so a plain `{model, input}` benchmark synthesis 500s. The speech-rate benchmark
   **skips clone models**; clone streaming still works (the `speech_streaming` verdict is
   written for `clon` too) and bills the honest `audio_bytes` fallback. Clone metering with a
   real duration is deferred with the rest of the #272 clone epic.
5. **Raw `stream_format=audio` billing.** SSE proves clean completion with `speech.audio.done`
   + `[DONE]`. The raw `application/octet-stream` path has no terminal marker; it is billed
   from the **relayed PCM byte count on a clean HTTP completion** (a mid-stream copy error →
   loud-zero error row), which bills **exactly what the client received** — honest for a partial
   delivery, not a full-amount over-bill. The residual (a cleanly-FIN'd-but-truncated raw body
   from the trusted upstream) is named and accepted; SSE is the default `stream=true` shape and
   the primary path.

### 4.8 Portal speech-streaming run (the TTS composer)
The portal chat's speech run (ADR-063) synthesizes a complete WAV and plays it. Add a
**streaming branch**, mirroring ADR-065(e)'s transcription portal run: the run POSTs
`stream=true` over the loopback, consumes the `speech.audio.delta` PCM and **plays it
progressively** (Web Audio / AudioWorklet), finalizes on `speech.audio.done` + `[DONE]`, and
stores the assembled WAV as the chat blob as today. It reuses the run's
reservation/deadline/delta/checkpoint plumbing (not a new audio path).

- **It needs the output sample rate to play headerless PCM**, read from the same cached
  byte-rate (§4.5). **Gate:** the portal streams only when the mapping is `speech_streaming:
  yes` **and** the rate is cached; otherwise it falls back to the buffered WAV run of ADR-063
  (today's behavior). So the portal never plays un-playable PCM and never needs the
  `audio_bytes` path — portal streaming and `audio_second` billing become available together
  (both gated on the cached rate).
- A "stream" affordance appears in the composer only for a streaming-capable model.
- This is the portal half of **#253**, distinct from **#266** (live *input* transcription).

## 5. Constraints
- English in all repo text; no LAN host names (refer to "an operator LAN host").
- Never commit/merge to `main`; one branch → one PR; Auto-fix on.
- Re-base version/migration/ADR/shared-enum on `main` before finalizing. Current baseline:
  ServerAgent `Version = 0.11.0` → bump to `0.11.1` (PATCH, §4.1); next ADR **068**; one
  migration **v95** (the perf-metric column, §7) — re-base its number on `main`'s then-highest
  (baseline high-water v94).
- Full pre-PR gates: `golangci-lint fmt --diff` + `run ./...` for both Go modules; frontend
  `prettier --check` + `vitest run` + build (**the portal changes** — the speech-streaming
  run, §4.8); the **Postgres store leg** (the store changes — the v95 perf-metric migration);
  `make sonar-gate` then `make sonar-branch-findings` (0 branch-attributed);
  `./scripts/check-docs.sh`; remove `docs/superpowers/**` and `docs/implementation-status.md`
  before the PR.

## 6. What's left to the builder
- Exact wiring of the `speech_streaming` verdict (gateway prober `probeAudioCppModels`,
  server-agent `audiocpp.go` collector, ingest allowlist, reserved-verdict lists) by mirror
  of `transcription_streaming`.
- The streamed-speech SSE/raw tee + billing setter (a `speechStreamScan` analogous to
  `transcriptionStreamScan`; a new `setStreamedSpeechBillingQuantity` beside
  `setSpeechBillingQuantity`).
- The byte-rate read on the billing path (`ModelMappingCacheForMappings`,
  `cache_type=output_byte_rate`) and the benchmark write; the audio perf-metric `Kind` on
  `model_mapping_benchmarks` (the v95 column).
- The portal speech-streaming run (§4.8): the streaming branch of the ADR-063 speech run, the
  client-side progressive PCM playback, and the cached-rate gate / buffered fallback.
- Tests mirroring `transcription_stream_test.go` / `speech_handler_test.go` /
  `wavduration_test.go` and the billing XOR tests, plus live verification against the
  operator audio.cpp host.

See the Implementation anchors appendix (§11) for the exact wiring points.

## 7. Version / migration / ADR accounting
- ServerAgent: `0.11.0` → `0.11.1` (PATCH; new verdict name, no `agent.Features`).
- ADR-068 "Streaming speech synthesis": the relay + `speech_streaming` verdict +
  byte-rate/`audio_bytes` metering + the audio benchmark; **amends ADR-041** (new unit;
  success-path measurement-unit refinement) and **ADR-061** (streaming lifted).
- Migration: **one, v95** — a nullable audio perf-metric column on `model_mapping_benchmarks`
  (ttft/RTF/wall) for the benchmark's `Kind` history. The verdict (open-vocabulary capability
  name), the byte-rate (generic `model_mapping_cache` `cache_type`) and `audio_bytes` (open
  `billing_unit` text) need no migration. Re-base v95 on `main`'s then-highest before
  finalizing (baseline high-water v94); provision the Postgres leg like CI.

## 8. Test strategy
- Unit: byte-rate parse from a `fmt` header (reuse `wavConsistentByteRate`); the billing
  hierarchy (seconds when rate known, `audio_bytes` when not, loud-zero on incomplete/error);
  `ValidateBillingXOR` for an `audio_bytes` row.
- Handler/stream: stream-only-on-verdict-yes; retry-buffered-on-500; SSE tee byte-count;
  raw-audio byte-count; capture stays redacted.
- Benchmark: derives + persists the rate; lifts the audio-only exclusion for the audio path.
- Live: re-verify the contract (§2) on the operator host, including that a benchmarked
  mapping then bills `audio_second`.

## 9. Scope decisions (resolved with the author)
1. **Portal speech-streaming run — included** (§4.8). It is the portal half of #253 and is
   distinct from #266 (live *input* transcription), which stays a separate feature.
2. **One PR**, phased.
3. **Audio perf metrics — included** (§4.6), carrying the one v95 migration (§7).

## 10. References
ADR-041 (billable unit pair, loud-zero), ADR-061 (speech WAV-only relay), ADR-062(e)
(transcription body metering), ADR-065 (streaming transcription + WAV-header billing +
`transcription_streaming` verdict), ADR-067 (persistent per-mapping cache). Issues #253,
#262 (closed), #263, #68, #69, #70. Live contract notes:
`side-session-notes/.../263-streamed-speech-contract.md`.

## 11. Implementation anchors (from the code map; for planning)

**`speech_streaming` verdict — mirror every `transcription_streaming` point:**
- `routing/store.go`: add `CapabilitySpeechStreaming` const beside `CapabilityTranscriptionStreaming`
  (`store.go:1498`); source `CapabilitySourceAudioCppModels` (`store.go:1652`) unchanged.
- `provider/audiocpp_models.go`: `AudioCppVerdicts` field + a parse rule in
  `audioCppStreamingVerdict` (`:338`) — today `mode` drives streaming only for `asr`; read it
  for `tts`/`clon` for speech_streaming. Mode consts `:46-47`.
- `cmd/gateway/app_health.go`: `audioCppCapabilityRows` (`:658`) emit the speech_streaming row
  when the model is a speech model (parallels the `v.Transcription==yes` guard at `:663`).
- `server-agent/internal/collector/audiocpp.go`: duplicate parser `parseAudioCppModels` (`:116`),
  `audioCppStreamingVerdict` (`:151`), and the shared `audioCppModelCases` table (pinned by
  `TestAudioCppModelsCasesMatchTheAgentsTable`); `collector/probe.go` `Capabilities` field
  (`:447/:470`); `agent/agent.go` `capabilitiesSample` emission (`:1955`, guard parallel to
  `:1984`).
- `internal/gateway/agent_ingest.go`: add the name to `audioCppProbeCapabilities` (`:1280`);
  do **not** add to `reservedAgentCapabilityNames` (`:1257`).
- `internal/portal/service_applications.go`: `reservedManualVerdicts` `{No:true}` entry
  (`:357` sibling).
- Handler read: a `mappingStreamsSpeech` beside `mappingStreamsTranscription`
  (`transcription_handler.go:881`) → `MappingCapabilities`/`CapabilityRowsByName`.
- ServerAgent `Version` `0.11.0` → `0.11.1` (`agent/agent.go:318`), version-history entry
  beside the `0.9.1->0.9.2` transcription_streaming note (`:257`); **no** `agent.Features` entry.

**Relay + billing — mirror ADR-065 on the speech path:**
- `speech_handler.go`: relax `validateSpeechRequest` (`:191`, refusals `:214-225`) to admit
  `stream=true`+pcm; `relaySpeech` (`:308`); billing setter beside `setSpeechBillingQuantity`
  (`:382`); the SSE/raw tee a `speechStreamScan` analogous to `transcriptionStreamScan`
  (`transcription_stream.go`); Content-Length skip for the streamed case
  (`native_passthrough.go:839`).
- `internal/usage/billing.go`: `BillingUnitAudioBytes = "audio_bytes"` (`:24` area),
  `ValidBillingUnit` (`:42`); `ValidateBillingXOR` (`:63`) unchanged.

**Byte-rate cache:** `routing/store.go` const `ModelMappingCacheOutputByteRate` beside
`ModelMappingCacheVoices` (`:1384`); write/read via `store/sqlite_model_cache.go`
(`UpsertModelMappingCache` / `ModelMappingCacheForMappings`). No migration.

**Benchmark:** new speech-measurement path modeled on `runVisionTarget`
(`benchmark_runner.go:937`); invert the audio-only split in `partitionBenchmarkViews`
(`benchmark_refusals.go:249`); the engine's chat path (`benchmarkTargetReq`/`streamOnce`/
`measureMapping`) is **not** reusable for a native speech relay.

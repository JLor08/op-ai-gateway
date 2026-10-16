# Design — Serve `POST /v1/audio/speech` behind audio.cpp (issue #68)

Status: draft for approval · Date: 2026-10-16 · Branch: `worktree-audio-speech`

This document is the design for issue #68. It supersedes the stale code
references in the issue body (verified against current `main`, see §2) and
records the decisions taken with the operator on 2026-10-16. It is written so a
builder can produce a phased plan without going back to the issue.

---

## 1. Intent & why

Text-to-speech is the endpoint that forces the gateway to relay a **binary,
non-JSON response body** (`audio/wav`). It is the third multi-modal endpoint in
the direction started by #70 (non-token billable unit) and #71 (capability gate,
error normalisation, the `stable_diffusion_cpp` runtime kind, `/v1/images/
generations`). #68 adds `POST /v1/audio/speech`, backed by audio.cpp, by
**mirroring the #71 images template** — a dedicated handler that relays through
the existing `proxyNative` streaming path, gated by a new `speech` capability.

Scope decisions already taken by the operator (unchanged from the issue): only
natively OpenAI-shaped backends qualify, and the gateway **proxies** request
shapes rather than translating them.

## 2. Verified reality (replaces the issue's stale anchors)

Four points from the issue no longer hold on current `main`; the design below
depends on the corrected picture:

1. **`proxyNative` no longer parses a chat-shaped body.** It routes off a generic
   `{model,stream}` probe (`sniffRoutingModel`), relays the upstream
   `Content-Type` **verbatim**, and streams the body through `http.Flusher`
   (`nativeCopier`, 32 KiB loop, ~1 MiB capped capture tee). This is the correct
   primitive for `audio/wav`; `complete()` (which re-marshals into memory and
   hardcodes `application/json`) is the wrong one and is not used here.
2. **The capability gate already covers every routing branch, including
   affinity** (`resolver.go` `filterCapable` + `resolveAffinity`'s own gate,
   from #71/ADR-042/044/045). The issue's "affinity path trap" is already closed;
   the pin-write guards even name #68/#69. #68 only has to *declare* a new
   required capability and *write its verdict*.
3. **`CapabilityAudio = "audio"` means audio INPUT** (a llama.cpp `/props`
   modality) and now has a writer (`app_health.go`). It is **not** TTS. #68 adds
   a distinct `CapabilitySpeech = "speech"`.
4. **The non-token billing unit already ships end-to-end.** `usage.
   BillingUnitAudioSecond = "audio_second"` exists (from #70), is accepted by
   `ValidBillingUnit`/`ValidateBillingXOR`, and the **completed-row frontend
   already renders it** (chip, em-dash on token cells, i18n
   `billingUnitHelpAudioSecond`). `ComputeHistogram` already drops the zero t/s
   such rows carry. #68 only has to **emit** the unit.

The runtime-kind constant from #71 is `RuntimeSpecTypeStableDiffusionCpp =
"stable_diffusion_cpp"` (there is no literal `sd_cpp`); the images API flavor is
`routing.APIFlavorOpenAIImages = "openai_images"`; the capability source string
is `"sdcpp_capabilities"`. #68 adds parallel constants (§5).

## 3. Verified audio.cpp behaviour (probed live against v0.9.1, CUDA, 2026-10-16)

Probed a running `audiocpp_server` with `pocket_tts_english_bf16` loaded:

- **Capability signal lives in `/v1/models`** — cleaner than sd-server, no extra
  document. Each entry carries `family`, **`task`**, **`mode`**, `loaded`, `path`:
  ```json
  {"id":"pocket_tts_english_bf16","object":"model","owned_by":"engine",
   "family":"pocket_tts","task":"tts","mode":"offline","loaded":true,"path":"…"}
  ```
  - `task` ∈ {`tts`, `clone`} ⇒ **speech**. `task == "asr"` ⇒ transcription (#69).
  - `mode` ∈ {`offline`, `streaming`} ⇒ the streaming signal.
  - `/v1/models` lists **loaded** models only (health reports `models: N`).
- **Non-streaming response:** `Content-Type: audio/wav`, real RIFF/WAVE body,
  `Content-Length` set. Metering headers present and honest:
  `X-AudioCPP-Audio-Duration-Ms` (→ billing quantity, seconds),
  `X-AudioCPP-Wall-Ms`, `X-AudioCPP-RTF`.
- **`response_format`:** `wav`/absent → `audio/wav` (binary); `json`/`b64_json`
  → `application/json` with base64 WAV (`{"audio":"UklGRi…"}`); **`mp3` (and any
  other unsupported value) → silently raw WAV** labelled `audio/wav`.
  **Metering headers are present only on the `audio/wav` response — `json` and
  `b64_json` carry none** (probed 2026-10-16: only `Content-Type: application/json`
  + `Content-Length`). This is decisive for the wav-only decision in §4/§5.
- Upstream always sends `Content-Length` (no `Transfer-Encoding: chunked`), so the
  length is available to forward (see §5 relay).
- **Errors:** already object-shaped `{"error":{"message":…,"type":"server_error"}}`,
  but audio.cpp returns **HTTP 500 even for client errors** (missing `input`,
  unknown model id).
- **Streaming boundary:** a stream request against an `offline` model → 500
  `"speech streaming requires a model configured with mode=streaming"`.
- **No auth of any kind** (all probes succeeded with no `Authorization`).
- **Health:** `GET /health` → `{"status":"ok","backend":…,"models":N,"ui":…}`.

## 4. Scope

### Goals
- Serve `POST /v1/audio/speech` (and `/openai/v1/audio/speech`), JSON-in, relaying
  the binary `audio/wav` response through `proxyNative` without buffering the whole
  body, and forwarding the upstream `Content-Length` (§5).
- A new `speech` capability: detected from audio.cpp `/v1/models` `task`, written
  as a verdict, and used to **gate** routing (a non-speech model is refused with
  `routing.model_not_capable` → 404, not routed or mislabelled).
- A new `audio_cpp` runtime kind (shared with #69's transcriptions later).
- **The full #71-style operator-facing configuration** for the new `openai_audio`
  flavor, through the portal: backend flavor validation, model-offering gating
  behind the speech verdict, the frontend flavor UI, i18n, and the runtime-admin
  template. (Decided 2026-10-16: full mirror in this PR, not a frontend fast-follow.
  Rationale: without the backend validation the relay is unreachable dead code;
  without the frontend the feature is a half-configurable trap.) Delivered in phases
  within the PR (see the plan).
- Record a usage row with `billing_unit="audio_second"` and
  `billing_quantity=<X-AudioCPP-Audio-Duration-Ms ÷ 1000>`, zero tokens, zero t/s,
  labelled as `/v1/audio/speech` in Activity (not `/v1/chat/completions`).
- Close the **live Active-Requests panel** gap so a non-token in-flight request
  shows "—" not "0" (also fixes the existing images gap — folded in).
- Docs (`docs/architecture/`) and the full test mirror.

### Non-goals (this PR)
- **Streaming** (`stream_format: sse` and `stream_format: audio`). Deferred to a
  fast-follow. v1 **refuses** `stream`/`stream_format` up front with
  `speech.stream_unsupported` (mirrors `images.stream_unsupported`), to be lifted
  when streaming lands.
- `GET /v1/audio/voices` proxying (an audio.cpp/llama-swap convention, not an
  OpenAI endpoint). Clients learn voice names out of band. Documented consequence.
- `POST /v1/audio/transcriptions` (that is #69; it shares the `audio_cpp` runtime
  kind this PR introduces, and will add `task == "asr"` detection).
- Launching/managing audio.cpp models (agent-runtime-manager concern). The gateway
  proxies to a configured, running server.

### Anti-goals (would make this a failure even if it "works")
- Routing a speech request to a non-speech model, or silently handing a strict
  `mp3` client a WAV labelled `audio/wav`.
- Buffering the whole audio body in memory (must stream).
- Poisoning usage stats: recording token counts or a t/s value on a speech row
  (must satisfy `ValidateBillingXOR`).
- Corrupting or storing the binary WAV through `json.Marshal` in capture.
- A second `admitPrincipal`/preflight call site (the one gate is
  `inferencePreflight`).

## 5. How it is built — the pieces (mirroring the #71 images/`sdcpp` template)

Names are proposals; the plan may refine them. "Add" = new; everything else is
reuse. Anchors are by symbol (line numbers drift).

**New vocabulary (routing/store.go):**
- `CapabilitySpeech = "speech"` (next to `CapabilityImage`). Note: unlike `image`
  it does **not** coincide with the billing unit (`audio_second`).
- `CapabilitySourceAudioCppModels = "audiocpp_models"` (a probe-rank source, same
  rank tier as `sdcpp_capabilities`; it may write `(speech, no)` because the
  `/v1/models` `task` is authoritative for a loaded model).
- Coarse API flavor `APIFlavorOpenAIAudio = "openai_audio"` + `NormalizeAPIFlavor`
  case folding `openai_audio*` **before** the generic `openai` prefix; add to the
  `score_servers` flavor list and the `served_flavors` "images-only" analog
  (a `FlavorsAreAudioOnly`-style helper so an audio-only server isn't offered for
  chat and vice-versa).

**New runtime kind (routing/runtime_spec_type.go + server-agent twin):**
- `RuntimeSpecTypeAudioCpp = "audio_cpp"`; `DetectRuntimeSpecType` case on the
  binary basename (`audiocpp_server` / `audio.cpp`; **confirm the exact basename
  and any `minitts` alias** against the shipped binary); `DeriveProbePaths` case
  → `"", ""` (no `/metrics`, no context axis — same as `stable_diffusion_cpp`).
  Mirror in `server-agent/internal/collector/probe.go`.

**Capability verdict writer (provider + cmd/gateway/app_health.go):**
- Provider optional-capability interface `AudioCppModelsProber` + a
  `Multiplexer.ProbeAudioCppModels`/`dispatchProbeAudioCppModels` returning
  `ErrUnavailable` on a non-implementing client (mirror `SdcppCapabilitiesProber`).
- A prober that `GET`s `/v1/models`, finds the entry whose `id` == the mapping's
  `ProviderModel`, and yields: `task` ∈ {tts,clone} → `(speech, yes)`; present
  with another task → `(speech, no)`; absent → `""` (unknown). Duplicated verbatim
  across the two Go modules (gateway + server-agent) and pinned identical by a
  table test (mirror `TestSdcppImageCasesMatchTheAgentsTable`).
- `app_health.go`: an `audioCppCapabilityRows(v, now)` projector + a
  `probeAudioCppModels` call in the health loop → `UpsertMappingCapabilities`.
- `reservedManualVerdicts` (portal): add `(speech, no)` as reserved (a manual
  rank-3 "no" must not veto the real probe writer).
- **Agent side** (if agent-managed audio.cpp is in scope): an `agent.Features`
  flag + `agent_ingest.go` acceptance of the new source for `CapabilitySpeech`
  only, plus the ServerAgent `Version` bump (MINOR, once, per the version rule).
  *Builder to confirm whether the agent path is in this PR or a follow-up.*

**Endpoint plumbing:**
- `sessionEndpoint` iota (`session_extract.go`): add `endpointSpeech`; add an
  **explicit, commented** case to **both** switches (`extractClientSession`,
  `sessionFromBody`) — speech has no session signal, stated as a case, matching
  the images house style (never left to the absent default).
- `upstreamPath` (`native_passthrough.go`): add the `apiFlavorSpeech` branch →
  `/v1/audio/speech`.
- Route registration (`server.go`): `/v1/audio/speech` + `/openai/v1/audio/speech`
  → `s.handleOpenAISpeech`, next to the images routes.

**Handler (new `speech_handler.go`, modelled on `images_handler.go`):**
- `handleOpenAISpeech`: `requireMethod` POST; auth
  `requireInternalOrBearerAnyScope(scopeGatewayUse, scopeLLMInvoke)` — **bearer or
  internal loopback, never the session cookie** (same posture as images; matches
  AGENTS.md); `liftInferenceDeadlines`; `readRawJSONUnlimited`; `sniffRoutingModel`.
- `validateSpeechRequest(raw, model)`: require non-empty `model` and `input`
  (proper 400 before relay, so we don't relay audio.cpp's sloppy 500s for client
  errors); reject `stream`/`stream_format` with `speech.stream_unsupported`;
  **accept only `response_format` ∈ {`wav`, absent}; refuse every other value**
  (mp3/opus/flac/aac **and** json/b64_json) with `speech.response_format_unsupported`.
  Rationale (decided 2026-10-16): v1 is purely binary (the issue's point); `mp3`
  would otherwise be silently handed WAV-as-mp3; and `json`/`b64_json` are non-OpenAI
  for speech **and** carry no `X-AudioCPP-Audio-Duration-Ms` header, so they would
  record `billing_quantity=0` and log an Error on every success. Supporting them
  later means adding a RIFF-header duration decode (noted as a follow-up in §9).
- Preflight (the one gate): `inferencePreflight(..., inferenceShape{apiFlavor:
  apiFlavorSpeech, endpoint: endpointSpeech, model, requiredCapabilities:
  []string{routing.CapabilitySpeech}})`.
- `relaySpeech`/`resolveSpeechTarget` → `proxyNative` with
  `nativeRelay{path: upstreamPath(target, APIFlavor, endpointSpeech), endpoint:
  endpointSpeech}`. `rewriteModelField` already applies (content-type-gated, JSON
  body) and needs no change.
- **Forward the upstream `Content-Length`** on the audio relay. `proxyNative` today
  forwards only `Content-Type` and then streams chunked (per-chunk `Flush` ⇒ no
  `Content-Length` on the wire). For audio that matters: the body is relayed
  byte-for-byte, audio.cpp always sends a `Content-Length` (§3), and the OpenAI
  speech contract returns a sized body (clients use it for progress/seeking; some
  reject a chunked `audio/wav` stream). Scoped `proxyNative` enhancement: when the
  upstream response carries a `Content-Length` and the body is relayed verbatim,
  set it on the client response and copy in increments without forcing chunked.
  (Plan item — this goes beyond the images template; confirm it does not regress the
  images/chat flavors, which keep their current chunked behaviour.)
- `recordSpeechRoutingFailure` analog for terminal resolve failures (carries the
  billing unit so even failures are labelled honestly).

**Operator-facing portal configuration (full #71 mirror — Goal in §4):**
- Backend flavor validation: add `openai_audio` to `normalizeFlavors`
  (`portal/service_applications.go`, reused for `ErrRuntimeSpecFlavorInvalid` in
  `service_runtime.go`) and to `knownAPIFlavors`/`callableModelNames`
  (`portal/service.go`), so applications and runtime specs can be saved with it.
- Model-offering gating: offer the `openai_audio` flavor only behind a `speech`
  verdict (the speech analog of the image-verdict gate in
  `service.go`/`service_model_offering.go`).
- Frontend: add the `openai_audio` flavor checkbox + i18n label
  (`applicationFlavorOpenaiAudio`, next to `applicationFlavorOpenaiImages`), and the
  `RuntimeAdminSection.tsx` flavor template for `audio_cpp` (default flavors =
  audio-only, mirroring the stable_diffusion_cpp images-only default).

**Billing (usage row):**
- `billingUnitFor(apiFlavor)` → `BillingUnitAudioSecond` for `apiFlavorSpeech`
  (it is the shared chokepoint feeding the three `proxyNative` `recordUsage` sites).
- `setSpeechBillingQuantity(&meta, ex, status)`: on success, read
  `X-AudioCPP-Audio-Duration-Ms` from the upstream response header and set
  `meta.BillingQuantity = ms/1000`; missing/zero → 0 + log at Error (mirror
  `setImagesBillingQuantity`'s "counted 0" logging). **Builder to confirm** the
  exchange struct exposes `resp.Header` at that call site.
- Usage scanner: `newUsageScanner` must return nil for `apiFlavorSpeech` (binary/
  JSON audio has no chat usage), as it does for images — so no token counts and no
  t/s leak onto the row and `ValidateBillingXOR` holds. No audio body counter is
  needed (quantity comes from the header, not the body).
- Endpoint label: `usageMeta{ReqPath: r.URL.Path, Endpoint: endpointSpeech}` →
  Activity shows `/v1/audio/speech`.

**Capture policy:**
- Request JSON captured normally (it carries the `input` text — exactly what the
  opt-in payload capture is for).
- **Do not** store the binary WAV response body through `json.Marshal`
  (U+FFFD-sanitises + clips). Add a **response-side** body skip, gated on the
  **response `Content-Type`** being binary audio (`audio/*`), storing metadata only
  (content-type, byte size, metering headers). Note: the precedent cited in
  `capture.go` is a *request-side* multipart skip; there is no existing response-side
  skip, so this is new (small) code. With the wav-only decision every successful
  speech response is `audio/wav`, so the response body is always skipped — but gate
  on the actual response content-type (not the request flavor) so an error response
  (`application/json`) is still captured.

**Error normalisation:**
- `needsSpeechErrorRelay(apiFlavor, statusCode)` on non-2xx for `apiFlavorSpeech`
  → `relaySpeechUpstreamError` → `normalizeSpeechUpstreamError`. audio.cpp errors
  are already `{"error":{message,type}}`; the normaliser passes object-shaped
  OpenAI errors through, and sets the gateway's own
  `type="upstream_error"`/`code="speech.upstream_error"` only when upstream's shape
  is not already OpenAI-attested (reuse `apierror.ErrorBody.Type`). Preserve/relay
  upstream status.
- Reuse `ErrModelNotCapable` (→ 404, `routing.model_not_capable`) for the gate.

**Timeout:**
- A speech synthesis can be long. `applicationTimeoutDefaultFor(appType)` must give
  the audio provider/appType a long default (600000, like `server_agent` and
  `stable_diffusion_cpp`) instead of the 30000 default. **Builder to confirm which
  provider/appType serves audio.cpp** (a new `ProviderAudioCpp`, or agent-served)
  and add the case there.

**Live Active-Requests panel (fold-in):**
- Add `BillingUnit` to `ActiveRequest` (`active_requests.go`) and `activeRequestDTO`
  (json `billing_unit`); populate it where the in-flight record is registered.
- Mirror on the frontend `ActiveRequest` type (`api/usage.ts`).
- `ActiveRequestsPanel.tsx`: wrap the output-tokens column in `isTokenMetered`
  (from `components/billingUnit.ts`) → "—" with the `activityNotTokenMetered`
  tooltip for a non-token unit (the per-row two-state rule). This also fixes the
  pre-existing images live-panel "0" bug.

## 6. Capability detection — considerations

- The verdict is **per mapping**, keyed on the mapping's `ProviderModel` id found
  in `/v1/models`. Because `/v1/models` lists only *loaded* models, a mapping whose
  model is not currently loaded reads `""` (unknown ⇒ refuse) until the health loop
  sees it loaded. For a static operator-run server the mapped model is loaded; for
  an agent-managed runtime the agent ensures load. Documented, acceptable, same
  bootstrapping shape as sd_cpp.
- `(speech, no)` is a legitimate bidirectional verdict: a loaded model present with
  a non-speech `task` is authoritatively not-speech.

## 7. Verification (acceptance)

- A speech request to a model without the `speech` capability is refused with
  `routing.model_not_capable` (404), not routed — on the fresh, group, affinity and
  server-override branches alike.
- The relay does not buffer the whole audio body (assert streaming/flush; assert
  upstream `Content-Type: audio/wav` reaches the client intact).
- Only `response_format` ∈ {`wav`, absent} passes; every other value
  (mp3/opus/flac/aac, json, b64_json) is refused with
  `speech.response_format_unsupported`.
- `stream`/`stream_format` is refused with `speech.stream_unsupported`.
- The relay forwards the upstream `Content-Length` and still does not buffer the
  whole body.
- An operator can configure an `openai_audio` application and runtime spec through
  the portal, and the `openai_audio` flavor is offered only behind a `speech`
  verdict.
- Client errors (missing `model`/`input`) return a gateway 400 with a distinct
  code; upstream non-2xx is relayed/normalised to OpenAI shape.
- The usage row records `billing_unit="audio_second"`,
  `billing_quantity` = audio-duration seconds, **zero tokens, zero t/s**
  (`ValidateBillingXOR` holds), and `ReqPath == /v1/audio/speech`.
- The chat usage scanner does not attempt to parse the audio/JSON body.
- Capture never passes WAV bytes through `json.Marshal`.
- The live Active-Requests panel shows "—" (not "0") for a speech request.
- `make lint`, `make test-go`, frontend `npm test` + `npm run build` +
  `format:check`, and the Sonar branch-findings gate are clean; Postgres leg run
  with a DSN for the store/migration-adjacent changes (none expected — no schema
  change, see §4). e2e: an audio/speech suite against a real/mock audio.cpp.

## 8. What is left to the builder (minor calls + confirmations)

Settle these during planning/implementation; none changes the shape above:
1. Exact audio.cpp binary basename(s) for `DetectRuntimeSpecType` (and whether the
   `minitts` rename alias must be matched).
2. Which provider/appType serves audio.cpp, and therefore where the 600000 timeout
   default case goes (new `ProviderAudioCpp` vs agent-served).
3. Whether the **agent-side** capability source (feature flag + `agent_ingest` +
   ServerAgent `Version` bump) is in this PR or a follow-up (gateway health-loop
   probe is the core and is in-scope regardless).
4. Confirm the exchange struct exposes the upstream `resp.Header` at the
   `setSpeechBillingQuantity` call site (for the duration header read).
5. Final capability-source string and its rank tier.

Settled since the first draft (no longer open): `response_format` is wav-only
(§5, resolves the json/b64_json billing-header gap); `Content-Length` is forwarded
(§5); capture skip is gated on the response content-type (§5); the operator-facing
portal config is in scope (§4/§5).

## 9. Out of scope / follow-ups (own issues/PRs)
- Streaming (`stream_format: sse` / `audio`, `mode=streaming`, PCM, the
  `speech.audio.delta`/`done` SSE events the chat scanner must ignore).
- `response_format` `json`/`b64_json` (base64 WAV in JSON): refused in v1; adding
  them later requires a RIFF-header duration decode for billing (they carry no
  `X-AudioCPP-Audio-Duration-Ms`).
- `/v1/audio/transcriptions` (#69) reusing the `audio_cpp` runtime kind.
- Agent-managed audio.cpp launch specs (agent-runtime-manager), if deferred per §8.3.

## 10. Branch hygiene (AGENTS.md)
- Feature branch only; never commit to/merge `main`; squash merge.
- Before the PR: fold durable content into `docs/architecture/`, remove
  `docs/superpowers/**` and `docs/implementation-status.md` from the branch; verify
  with `git diff --name-only main...HEAD`.
- No local host names in any repo-facing text (PR/commits/docs).
- ADR for the `speech` capability + `audio_cpp` runtime kind (alongside ADR-042's
  family); update `api-surface.md`, `openapi.yaml`, the compatibility, routing,
  telemetry/usage and glossary docs.

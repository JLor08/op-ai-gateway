# 6. Runtime View

Key runtime scenarios as sequence diagrams. Details of each subsystem are in the
[cross-cutting concepts](README.md#8-cross-cutting-concepts).

## 6.1 Inference request (routing, dispatch, streaming)

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway (edge)
    participant CO as compat
    participant R as routing (resolver+scorer)
    participant P as provider client
    participant AI as AI server
    participant U as usage

    C->>GW: POST /v1/chat/completions (Bearer token)
    GW->>GW: authenticate (bearer or session+CSRF)
    GW->>CO: translate request → internal model
    GW->>R: resolve(model, flavor) → candidates
    R->>R: score by telemetry, priority/weight, capacity
    R-->>GW: chosen (server, application, mapping)
    GW->>P: dispatch to scheme://domain:port (app model name)
    P->>AI: upstream request
    alt streaming
        AI-->>P: SSE chunks
        P-->>GW: chunks
        GW-->>C: SSE chunks (idle watchdog, deadlines lifted)
    else non-streaming
        AI-->>P: response
        P-->>GW: response
        GW-->>C: response
    end
    GW->>U: record usage event (tokens, cost, energy, status)
```

Notes: the six JSON inference endpoints read the request body with no size cap
(large multimodal payloads); the other two, the multipart image edit
(`POST /v1/images/edits`) and the multipart audio transcription
(`POST /v1/audio/transcriptions`), are the capped bodies, each at a setting of its
own (`OP_AI_GATEWAY_MAX_IMAGE_EDIT_BODY_BYTES`,
`OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES`, default 32 MiB each). All eight
lift the server read/write deadlines; SSE streams are bounded by an idle watchdog and
client disconnect, not a total cap. See
[Compatibility & Inference](cross-cutting/compatibility-and-inference.md).

### 6.1.1 Speech relay (`POST /v1/audio/speech`)

Text-to-speech is the one inference scenario whose response is **binary and not
JSON**, so it has its own runtime shape: no translate path, a capability gate
before routing, a validation that refuses what the relay cannot serve, a sized
and unbuffered audio body, and a usage row in seconds rather than tokens.

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway (edge)
    participant R as routing (resolver)
    participant AC as audio.cpp server
    participant U as usage

    C->>GW: POST /v1/audio/speech {model, input, voice} (Bearer token)
    GW->>GW: authenticate (bearer or loopback, never a session)
    GW->>GW: validate: model + input; response_format wav (pcm when streaming); stream boolean
    Note over GW: 400 speech.* here, before any admission, not recorded
    GW->>R: resolve(model, openai_audio, required capability speech)
    R-->>GW: candidate with speech: yes, or 404 no_model_route / model_not_capable
    GW->>AC: POST /v1/audio/speech (model rewritten to the app's own name)
    alt 2xx
        AC-->>GW: audio/wav + Content-Length + X-AudioCPP-* headers
        GW-->>C: audio/wav + Content-Length (streamed, never buffered)
        GW->>U: audio_second = Audio-Duration-Ms / 1000, zero tokens
    else non-2xx
        AC-->>GW: error body (an OpenAI object, or a plain string)
        GW-->>C: OpenAI-shaped error at the upstream's status
        GW->>U: audio_second error row, quantity 0
    end
```

The gate is `RequiredCapabilities = ["speech"]`, whose verdict the health loop wrote
from the server's `/v1/models` `task`; the flavor is the application's own
`openai_audio` declaration. A payload capture keeps the request JSON and a metadata
summary of the audio, never the WAV bytes. See [Compatibility & Inference
§3.6](cross-cutting/compatibility-and-inference.md#36-openai-audio-speech) and
[ADR-061](09-architecture-decisions.md#adr-061--speech-is-the-second-endpoint-on-the-capability-gate-a-distinct-speech-capability-a-coarse-openai_audio-flavor-the-audio_cpp-kind-and-a-binary-relay-that-serves-wav-only).

**Streaming variant** ([ADR-068](09-architecture-decisions.md#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives), issues #253 and #263). When the client sends
`stream=true` (with `response_format` `pcm` or absent) and the mapped model's
`speech_streaming` verdict is `yes` (the model is `mode=streaming`), the relay sends
`stream=true` upstream and **relays audio.cpp's stream unbuffered**: the SSE
`speech.audio.delta` (base64 PCM) → `speech.audio.done` → `[DONE]`, or raw PCM for
`stream_format=audio`, with **no `Content-Length`** (a stream has none). The stream states no
duration and no sample rate, so the relay **counts the PCM it relays** after each chunk has
been written, and on a clean completion bills `audio_second` = bytes ÷ the mapping's cached
output byte rate, or `audio_bytes` = the byte count while no rate is cached; anything else
(an error, a cut or a disconnected stream) is an `audio_second` row of quantity 0. The capture
stays the redacted summary, with the endpoint stamped speech. A verdict that is `no`, absent or
unreadable serves the buffered one-shot of the diagram (the body is sent without the stream
fields, so the client gets `audio/wav` whatever it asked), and a stale `yes` that draws a `500
"speech streaming requires a model configured with mode=streaming"` is **retried buffered
once**, nothing of the stream having reached the client. The byte rate is learned by the speech
benchmark (§6.5), which the health loop starts for a loaded streaming-capable mapping.

### 6.1.2 Transcription relay (`POST /v1/audio/transcriptions`)

Speech-to-text is speech's mirror image: its request is a **multipart audio upload** and
its response is **JSON**, and the quantity it bills is in that JSON, so its relay is the
one that reads its response before it writes. It shares speech's flavor, so what differs
is keyed on the endpoint.

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway (edge)
    participant R as routing (resolver)
    participant AC as audio.cpp server
    participant U as usage

    C->>GW: POST /v1/audio/transcriptions multipart {model, file} (Bearer token)
    GW->>GW: authenticate (bearer or loopback, never a session)
    GW->>GW: read the body (capped), peek the model from the form, validate
    Note over GW: 400 transcription.* here (exactly one non-empty file, response_format json only, no stream), 413 over the cap, before any admission, not recorded
    GW->>R: resolve(model, openai_audio, required capability transcription)
    R-->>GW: candidate with transcription: yes, or 404 no_model_route / model_not_capable
    GW->>GW: agent target needs multipart_proxy, else 404 transcription.unavailable
    GW->>AC: POST /v1/audio/transcriptions (form verbatim, or rebuilt once: model renamed, empty file dropped)
    alt 2xx
        AC-->>GW: application/json {text, timing{audio_duration_ms}}
        GW->>GW: buffer the whole body up to the cap, then write it verbatim
        GW-->>C: the same JSON, timing kept
        GW->>U: audio_second = timing.audio_duration_ms / 1000, zero tokens
    else 2xx over the cap, or broken off
        GW-->>C: 502 transcription.upstream_error, nothing of the body written
        GW->>U: audio_second error row, quantity 0
    else non-2xx
        AC-->>GW: error body (an OpenAI object, or a plain string)
        GW-->>C: OpenAI-shaped error at the upstream's status
        GW->>U: audio_second error row, quantity 0
    end
```

The gate is `RequiredCapabilities = ["transcription"]`, whose verdict the health loop wrote
from the same `/v1/models` `task` as speech's (`asr` is a transcription `yes`). A payload
capture keeps the form's text fields (the audio `file` part is redacted) **and the
transcript**, which is output-sensitive. See [Compatibility & Inference
§3.7](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions) and
[ADR-062](09-architecture-decisions.md#adr-062--transcription-is-multipart-in-and-json-out-on-the-shared-openai_audio-flavor-a-distinct-transcription-capability-billing-read-from-the-response-body-and-every-divergence-from-speech-keyed-on-the-endpoint).

**Streaming variant.** When the client sends `stream=true` and the mapped model's
`transcription_streaming` verdict is `yes` (the model is `mode=streaming`), the relay takes
a different shape: it sends `stream=true` upstream and **relays audio.cpp's SSE unbuffered**
(`transcript.text.delta` → `transcript.text.done` → `[DONE]`) instead of the read-decide-write
of the diagram above, bills `audio_second` from the **uploaded WAV header** on a clean
completion (there is no response `audio_duration_ms` to read), and stamps the transcription
endpoint on the capture so the assembled transcript is stored as text. A verdict that is
`no`, absent or unreadable serves the buffered one-shot of the diagram, and a stale `yes`
that draws a `500 "requires a model configured with mode=streaming"` is **retried buffered
once** — the upload is already in memory and nothing of the stream has reached the client.
A cold `mode=streaming` model is given a first-byte deadline grace before the idle watchdog
takes over ([ADR-065](09-architecture-decisions.md#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript),
[Compatibility & Inference §3.7 item 11](cross-cutting/compatibility-and-inference.md#37-openai-audio-transcriptions)).

### 6.1.3 Portal speech chat turn (through loopback to a chat blob)

A speech turn in the portal chat is the one place the speech relay of §6.1.1 is called
by the gateway itself. The browser never calls `/v1/audio/speech`: it starts a **run**
like any chat turn, and the run executor calls the endpoint over loopback, stores the
WAV it gets back as a chat blob and lets the browser fetch that blob for playback.
A **buffered** speech turn, like an image turn, emits no incremental event between dispatch
and the finished reply, so the browser shows a wait state fed by a server-measured clock; a
**streaming** turn (below the diagram) plays the audio as it arrives.

```mermaid
sequenceDiagram
    participant B as Portal (browser)
    participant GW as Gateway (run executor)
    participant SP as Gateway (/v1/audio/speech)
    participant AC as audio.cpp server
    participant S as chat store (chat_blobs)
    participant U as usage

    B->>GW: POST /api/portal/chats/{id}/runs {user_message, settings: kind speech, model, audio} (session + CSRF)
    GW->>GW: PrepareChatRun pins the kind, clears non-speech settings, stores the user turn
    GW-->>B: 201 {run id, kind: speech}
    B->>GW: GET .../runs/{runId}/events (snapshot with elapsed_ms, no deltas)
    GW->>GW: executeAudioRun under speechRunDeadline (10 minutes)
    GW->>GW: buildSpeechBody {model, input = last user text, voice, speed, passthrough}
    Note over GW: a passthrough key named model, input, stream, stream_format or response_format ends the run gateway.chat_run_speech_reserved_field and sends nothing
    GW->>SP: POST /v1/audio/speech over loopback (internal pair, chat-id, run-as)
    SP->>AC: resolve (openai_audio, capability speech), relay as in 6.1.1
    alt 2xx WAV
        AC-->>SP: audio/wav + Content-Length + X-AudioCPP-* headers
        SP->>U: audio_second under the run-as token
        SP-->>GW: audio/wav
        GW->>GW: read the whole body, require a WAV Content-Type, build ONE audio_url part (data:audio/wav)
        GW->>S: commit the turn, extractChatBlobs moves the WAV into chat_blobs and rewrites the URL
        GW-->>B: SSE done with content_parts (the part as produced)
        B->>GW: GET /api/portal/chat-blobs/{id} (once, session cookie, owner-scoped)
        GW-->>B: audio/wav (no Range support)
        B->>B: AudioTurn plays an object URL in audio controls, revoked on unmount
    else error
        SP-->>GW: the endpoint's own error, or an empty, unreadable or non-WAV 2xx
        GW-->>B: SSE done, status error with gateway.chat_run_* or speech.* code
    end
```

**Streaming branch** ([ADR-068](09-architecture-decisions.md#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives), issues #253 and #263). When the gateway can stream the
model (every mapping the model could route to is `speech_streaming: yes` with a cached output
byte rate, all agreeing, and the thread has not opted out; the composer asks
`GET /api/portal/model-speech-streaming` and the run re-evaluates the same gate), the executor
sends `stream=true` with `stream_format=sse` instead, arming `speechRunDeadline` only until the
first chunk of audio. Each `speech.audio.delta` reaches the browser as an **`audio`** run event
(`{seq, rate, data}`: base64 PCM, a sequence number and the sample rate) that the browser
schedules gaplessly on one shared `AudioContext`, and is buffered nowhere on the server. The
executor keeps the PCM, and on a stream that ended with `speech.audio.done` and `[DONE]` it wraps
it in a canonical WAV header at the cached rate and commits the **same one `audio_url` part** as
the buffered turn, so the rest of the diagram (the blob, the SSE `done`, `AudioTurn`) is
unchanged; a cut stream commits nothing. Live playback is abandoned for the run at the first
gap in the sequence, which is harmless because the committed WAV is complete.

What differs from the speech relay of §6.1.1: the caller is the executor and the
credential the internal loopback pair (never a session cookie, [Security
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance)), so
billing, capture and routing run under the **run-as token**; the response is read
whole and checked before anything is stored (empty, unreadable or not labelled WAV is
`gateway.chat_run_no_audio` or `gateway.chat_run_speech_response_unreadable`); and the
WAV leaves the sealed transcript for `chat_blobs`, so the document carries a short gateway
URL. A failed commit (a WAV over `MaxChatBlobBytes`) ends the run
`gateway.chat_run_commit_failed`. Playback is the browser's own concern: the blob
endpoint answers no `Range` request, so `AudioTurn` fetches the blob once and plays an
object URL. See [Compatibility & Inference
§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground) and
[ADR-063](09-architecture-decisions.md#adr-063--speech-in-the-portal-chat-is-a-third-run-kind-the-model-is-the-affordance-the-run-reuses-the-speech-endpoint-over-loopback-and-the-wav-lives-in-chat_blobs).

### 6.1.4 Portal transcription chat turn (through loopback from a chat blob)

A transcription turn **inverts** the speech turn of §6.1.3: the audio is the run's
**input**, not its output, and the reply is the ordinary text turn. The browser attaches a
file or records a clip in the composer and **converts it to a 16 kHz mono 16-bit PCM WAV**
(audio.cpp reads WAV only, so the upload is always a WAV, whatever was attached or
recorded); the clip is stored as a chat blob on the user turn, and the run executor
resolves it, relays it multipart to `/v1/audio/transcriptions` over loopback, and commits
the transcript as a text turn.

```mermaid
sequenceDiagram
    participant B as Portal (browser)
    participant GW as Gateway (run executor)
    participant TR as Gateway (/v1/audio/transcriptions)
    participant AC as audio.cpp server
    participant S as chat store (chat_blobs)
    participant U as usage

    B->>B: attach a file or record: WebAudio decodes the clip, an OfflineAudioContext downmixes and resamples it to 16 kHz mono, a 16-bit PCM WAV is written (data:audio/wav)
    B->>GW: POST /api/portal/chats/{id}/runs {user_message with audio_url clip (the WAV), settings: kind transcription, model, transcription} (session + CSRF)
    GW->>GW: PrepareChatRun pins the kind, clears non-transcription settings, extractChatBlobs moves the clip into chat_blobs, resolveTranscriptionRunSource resolves the last user clip (or refuses: not owned)
    GW-->>B: 201 {run id, kind: transcription}
    B->>GW: GET .../runs/{runId}/events (snapshot with elapsed_ms, no deltas)
    GW->>GW: executeTranscriptionRun under transcriptionRunDeadline (10 minutes)
    GW->>GW: buildTranscriptionBody multipart {file = clip, model, language?, prompt?, temperature?, passthrough}, response_format NOT sent
    Note over GW: a passthrough key named model, file, stream or response_format ends the run gateway.chat_run_transcription_reserved_field and sends nothing
    GW->>TR: POST /v1/audio/transcriptions over loopback (internal pair, chat-id, run-as), Content-Type overridden to multipart
    TR->>AC: resolve (openai_audio, capability transcription), relay as in 6.1.2
    alt 2xx JSON
        AC-->>TR: application/json {text, timing{audio_duration_ms}}
        TR->>U: audio_second from the body under the run-as token
        TR-->>GW: the JSON transcript
        GW->>GW: read the body, take text, build ONE text part (no new part type, no output blob)
        GW->>S: commit the turn (the input clip is already a chat blob)
        GW-->>B: SSE done with the transcript as a text turn
        B->>B: title an untitled chat from the transcript, and play the input clip via a fetched object URL (reused AudioTurn)
    else error
        TR-->>GW: the endpoint's own error, or an empty or unreadable 2xx
        GW-->>B: SSE done, status error with gateway.chat_run_transcription_* or transcription.* code
    end
```

What differs from the transcription relay of §6.1.2: the caller is the executor and the
credential the internal loopback pair (never a session cookie, [Security
§1](cross-cutting/security-auth-rbac.md#1-overview-authentication-surfaces-at-a-glance)), so
billing, capture and routing run under the **run-as token**; the clip is a user-turn
**input resolved before the run** (like an image-edit source,
[ADR-057](09-architecture-decisions.md#adr-057--image-edit-a-derived-image_edit-capability-a-multipart-passthrough-and-the-attached-image-as-the-edit-affordance)),
not a client-supplied multipart body; and the transcript is committed as the **ordinary
text turn**, with no output part and no output blob — the inverse of the speech turn, whose
WAV becomes an `audio_url` blob. **Against a `mode=streaming` model the run streams**: the
executor POSTs `stream=true` over the loopback, renders the transcript **incrementally**
from `transcript.text.delta` events (reusing the text run's delta/checkpoint plumbing), and
`TranscriptionPendingTurn` shows only until the first delta, the run keeping its full
deadline until then; against an offline or unprobed model the gateway serves it buffered and
the run is the one zero-delta call ADR-064 described
([ADR-065](09-architecture-decisions.md#adr-065--streaming-transcription-the-gateway-relays-the-sse-for-a-streaming-model-gated-by-a-transcription_streaming-verdict-with-a-retry-buffered-on-500-safety-net-bills-the-streamed-call-from-the-wav-header-and-the-portal-chat-run-streams-the-transcript),
issue #262). See [Compatibility & Inference
§12](cross-cutting/compatibility-and-inference.md#12-in-portal-chat-playground) and
[ADR-064](09-architecture-decisions.md#adr-064--transcription-in-the-portal-chat-is-the-fourth-run-kind-the-model-is-the-affordance-the-audio-input-lives-in-chat_blobs-and-is-relayed-multipart-over-loopback-and-the-reply-is-the-ordinary-text-turn).

## 6.2 Portal login (session + CSRF) with optional TOTP

```mermaid
sequenceDiagram
    participant B as Browser (SPA)
    participant GW as Gateway
    participant AC as account
    participant T as totp
    participant S as store

    B->>GW: POST /api/auth/login (email, password, X-OP-CSRF)
    GW->>AC: verify credentials (bcrypt)
    AC->>S: load user
    alt TOTP enabled/required
        AC-->>B: 2FA required
        B->>GW: POST /api/auth/login (+ TOTP code)
        GW->>T: verify code
    end
    AC->>S: create server-side session
    GW-->>B: Set-Cookie (session), principal + scopes
    B->>GW: GET /api/portal/me (cookie + X-OP-CSRF)
    GW-->>B: current user, role/scopes, session elevation
```

## 6.3 Server-Agent telemetry ingest

```mermaid
sequenceDiagram
    participant AG as Server-Agent
    participant GW as Gateway (agent mux)
    participant R as routing/telemetry store
    participant SSE as portal SSE

    loop every interval
        AG->>AG: collect host/GPU/power/temp
        AG->>GW: POST /api/agent/v1/telemetry (Bearer agent token)
        GW->>GW: derive server from token, whitelist fields
        GW->>R: store sample, update availability
        R-->>SSE: fan out live sample (per-server /events)
    end
    Note over AG,GW: WebSocket transport (/api/agent/v1/stream) is an<br/>alternative to per-interval POSTs — liveness via active Ping
    AG->>GW: POST /api/agent/v1/system-report (hardware inventory)
```

## 6.4 Model synchronization

```mermaid
sequenceDiagram
    participant A as Admin (portal)
    participant GW as Gateway
    participant PR as provider (ModelLister)
    participant AI as AI server
    participant S as store

    A->>GW: POST /api/portal/applications/{id}/sync-models
    GW->>PR: list upstream models
    PR->>AI: GET /v1/models, /sdapi/v1/sd-models or /api/tags
    AI-->>PR: model list
    PR-->>GW: models
    GW->>S: upsert model mappings for the application
    GW-->>A: updated mappings
```

The listing endpoint is derived from the application's **type**, never from its
`loaded_models_path`: `/api/tags` for `ollama`, `/sdapi/v1/sd-models` for
`stable_diffusion_cpp` (whose `/v1/models` names only a placeholder), and
`/v1/models` for every other OpenAI-compatible type; `mock` answers fixed
names without a request. A body that is not JSON is an error that leaves every
mapping unchanged; which JSON bodies also count as errors differs by decoder
([API Compatibility & Inference §8](cross-cutting/compatibility-and-inference.md#8-provider-clients)).

## 6.5 Model-selection benchmark

```mermaid
sequenceDiagram
    participant A as Admin/scheduler
    participant GW as Gateway
    participant R as routing/benchmark
    participant AI as AI server
    participant SSE as portal SSE

    A->>GW: trigger benchmark (manual / scheduled / opportunistic)
    GW->>R: run capacity + speed probes
    R->>AI: probe requests (context size, throughput, concurrency)
    AI-->>R: measurements
    R->>R: persist metrics, update selection inputs
    R-->>SSE: live progress
    Note over R: metrics feed candidate scoring —<br/>hybrid swap-protection avoids thrashing
```

**Speech benchmark** ([ADR-068](09-architecture-decisions.md#adr-068--streaming-speech-synthesis-the-gateway-relays-the-pcm-stream-of-a-streaming-model-meters-it-from-the-bytes-it-relayed-audio_bytes-or-audio_second-over-a-benchmark-learned-byte-rate-and-the-portal-composer-plays-it-as-it-arrives), issue #263). `POST …/benchmark?mode=speech`, and the
app-health loop on its own for a loaded, streaming-capable, plain text-to-speech mapping that
has no cached **output byte rate** (or one measured under another loaded model), is the one
benchmark that runs on audio mappings and sends no chat prompt. After taking the server's
reservation, and giving it back if the server has live requests, it synthesizes one short
**non-stream** clip **directly** to the mapping's upstream through the provider's native relay
(not through the gateway's own loopback, which the reservation would exclude and which has no
principal for a scheduled run), reads the byte rate from the WAV `fmt` chunk, caches it (only
from a mono 16-bit PCM WAV) in `model_mapping_cache` beside the model identity it was measured
under, and appends a `kind=speech` history row with `ttft_ms` (roughly synthesis plus any model
cold start, since the request does not stream), `wall_ms` and `rtf`. A clone model is skipped.
The health loop retires a rate and re-runs the benchmark when the loaded model behind the
mapping changes, and leaves a mapping whose run produced no rate alone for five minutes.

## 6.6 Mesh certificate issuance & HTTPS auto-switch

```mermaid
sequenceDiagram
    participant AG as Server-Agent
    participant GW as Gateway
    participant CA as certissue (internal CA)
    participant APP as AI server (behind agent proxy)

    AG->>GW: GET /api/agent/v1/ca
    GW-->>AG: CA bundle (trust store)
    AG->>GW: request leaf certificate
    GW->>CA: issue mesh leaf (SANs)
    CA-->>AG: leaf + key
    AG->>AG: start TLS-terminating proxy (cert_mode=proxy)
    AG->>GW: telemetry proxy_routes[] (tls_active:true)
    GW->>GW: ReconcileHTTPSSwitch → set application scheme=https
    Note over GW,APP: gateway now dispatches over HTTPS to the<br/>agent proxy — a scope-exit reverts to http
```

## 6.7 Agent-managed model runtime: on-demand start

```mermaid
sequenceDiagram
    participant C as AI client
    participant GW as Gateway
    participant RT as agent router
    participant MG as runtime manager (owner goroutine)
    participant CH as model process

    C->>GW: inference (gateway model name)
    GW->>RT: proxied request (app_model_name in JSON body)
    RT->>RT: buffer body, read `model` + `stream`
    RT->>MG: EnsureRunning(upstream model)
    MG->>MG: Permit (local policy) → Admit (matrix · limit · per-GPU VRAM)
    Note over MG: stopped → starting
    MG->>CH: exec (argv, scratch env, loopback port)
    MG->>CH: poll health until green (or startup_timeout_seconds)
    Note over MG: starting → running
    MG-->>RT: admitted + release()
    RT->>CH: forward, splice response unbuffered
    CH-->>C: tokens
    Note over MG,GW: every transition rings the doorbell →<br/>immediate coalesced telemetry sample →<br/>portal SSE shows stopped → starting → running
```

**The externally observable contract**, verified end to end with a real agent and
real child processes: an inference for a mapped model whose spec is not running
**blocks in the gateway** while the agent admits and starts the child, and the
request that triggered the start is the one that receives the answer — the first
request pays the start latency rather than failing fast. The state sequence on
the portal's runtime event stream is exactly `stopped → starting → running`, and
the status frame for the running spec reports the child's real `pid` and the
ephemeral loopback `port`. Nobody should redesign the router around a fail-fast
assumption.

**The degradation ladder is layered on purpose, and the WebSocket push is an
optimisation, not a dependency.** If a doorbell or a config frame is lost, the
agent's 60 s runtime poll catches up. If the gateway is entirely unreachable, the
agent keeps running from the last good runtime-config document it persisted
atomically to its own disk cache. In the reverse direction, every state
transition triggers an immediate coalesced out-of-band telemetry sample, so a
portal click feels like network latency rather than tick cadence. The
`RuntimeUpdates()` channel is buffered(1) with a non-blocking, coalescing send —
exactly like `certUpdates`. A reader who treats the push as *the* delivery
mechanism will "fix" a missed-frame bug with acks and retries instead of relying
on the poll and disk-cache fallbacks already designed in.

See [Agent-Managed Model Runtime](cross-cutting/agent-runtime-manager.md).

## 6.8 Theme resolution (built-in vs external)

```mermaid
sequenceDiagram
    participant B as Browser (ThemeRoot)
    participant GW as Gateway
    participant TH as theme (external loader)

    B->>GW: GET /api/system/theme (public, pre-auth)
    GW->>TH: is active id an external theme?
    alt external
        TH-->>GW: theme data (tokens, brand, product name)
        GW-->>B: {theme, source: external, data}
        B->>B: apply data as CSS variables, favicon/logo via /api/system/themes/{id}/…
    else built-in
        GW-->>B: {theme, source: builtin, data: null}
        B->>B: apply compiled built-in theme
    end
```

# Plan: bill streamed sglang-omni speech and transcription

Branch `claude/sglang-omni-stream-billing`, cut from `origin/main` 2e018c0d (#396 merged).
Gateway-only change; no agent version bump. Repo text in English.

## Verified upstream facts (sglang-omni v0.2.0, `e8c635ec`)

Source copies:
`/private/tmp/claude-503/-Users-jlor08-Developer-codex-op-ai-gateway--claude-worktrees-gracious-mclean-fd9741/7581e996-7dda-470f-bdf1-15d45e358ac5/scratchpad/up/`

- **Speech stream** (`sglang_omni_serve_openai_api.py`, `speech_audio_response` ~:2122-2316):
  - SSE body (`_sse_body`): one `data: {"type":"speech.audio.delta","audio":"<b64 pcm>"}\n\n` per chunk. On success, a final
    `data: {"type":"speech.audio.done","usage":{...tokens...}|null,"finish_reason":...}\n\n`, and the body then ENDS. There
    is no `[DONE]`. On failure it emits `data: {"type":"error",...}\n\n` instead of the done event, then ends.
  - Raw shape: `audio/pcm`, the body is PCM.
  - BOTH shapes carry the response headers `X-Sample-Rate: <int>`, `X-Channels: 1` and `X-Bit-Depth: 16`.
- **Speech request** (`sglang_omni_serve_protocol.py` ~:417-420, `sglang_omni_serve_speech_service.py` ~:323-326):
  - `response_format` defaults to `"wav"`, and `stream_format` defaults to `"audio"`.
  - `stream = request.stream or stream_format == "sse"`. If `stream and response_format != "pcm"`, the request is
    rejected with a 400, "stream=true requires response_format='pcm'".
  - The gateway's streamed bodies omit `response_format` (the portal's `buildSpeechStreamBody`, the benchmark's
    `streamSynthesis`, and any client that relies on the documented default). So **every streamed speech call to
    sglang-omni is refused today**.
- **Transcription stream** (`sglang_omni_serve_speech_to_text.py` `speech_to_text_stream` ~:560-619):
  - deltas `transcript.text.delta`;
  - then `data: {"type":"transcript.text.done","text":...,"usage":{"type":"duration","seconds":<int ceil>}}` (usage
    omitted when the duration is unknown);
  - then `data: [DONE]`. The error path emits `{"type":"error",...}` and returns without done or `[DONE]`.

## Design decisions

1. **The gateway terminates sglang-omni's speech event stream.** For a target whose effective kind
   (`routing.Target.EffectiveKind()`) is `sglang_omni`, the relay writes the sentinel frame itself when all of these hold:
   - the upstream closed the SSE body cleanly (io.EOF);
   - a `speech.audio.done` event was seen;
   - no `[DONE]` arrived;
   - nothing was lost.

   The sentinel goes out through the normal `writeChunk` path, so it is flushed and fed to the scan. Effects:
   - The relay's billing rule stays unchanged (done event, then `[DONE]`).
   - The portal voice-chat TTS reader (`chat_runs_audio_stream.go` `speechEventReader`, which reads the relayed
     stream) works without any change.
   - API clients see one contract for every speech event stream the gateway relays.

   A write error is the client going away, so the call's status turns into the disconnect, which is a zero.

   **Frame bytes:** `\n` + `data: [DONE]\n\n` when the last relayed byte was `\n`, and `\n\n` + `data: [DONE]\n\n` otherwise.
   This way the sentinel is always its own SSE event and never fuses into the data of an upstream frame that was left
   unterminated. An extra blank line is a no-op in SSE.

   audio.cpp targets never get the sentinel written, so their behaviour is byte-identical.
2. **Streamed speech to sglang-omni states `response_format: "pcm"`.** In `relaySpeech` the upstream body gets
   `"response_format":"pcm"` when all of these hold:
   - the request is relayed as a stream (`rel.pfReq.Stream`);
   - the target's effective kind is `sglang_omni`;
   - the client's body has no `response_format`, or a null one.

   `validateSpeechRequest` already admits only `pcm` or absent on a stream, so nothing else can be there. The buffered
   retry (`speechBufferedBody`) is unchanged and still drops it. Other targets are unchanged.

   The audio benchmark's `streamSynthesis` does the same for an `sglang_omni` target.
3. **The header byte rate is a fallback after the cached rate.** For an `sglang_omni` target, `newSpeechStreamScan` reads
   `X-Sample-Rate`, `X-Channels` and `X-Bit-Depth` into a byte rate. All three must be plain decimal positive integers,
   and the bit depth must be a multiple of 8. The resulting `rate*channels*depth/8` must satisfy the one existing
   definition of a usable rate (`routing.ParseOutputByteRate`'s rule: positive and fits a uint32). Any unusable or
   missing header means no header rate; nothing is guessed.

   `setStreamedSpeechBillingQuantity` order:
   1. the cached rate (unchanged): `audio_second`;
   2. else the header rate: `audio_second`;
   3. else `audio_bytes` (unchanged).

   The fallback applies to both shapes. Other targets never get a header rate.
4. **The streamed transcription bills from the stream's own usage statement after the WAV measure.**
   `transcriptionStreamScan` keeps the `usage` of the `transcript.text.done` event (the last done event wins).
   `setStreamedTranscriptionBillingQuantity`, on a completed stream:
   1. the uploaded WAV (unchanged);
   2. else the done event's usage `{type:"duration", seconds:N}`, under exactly the one-shot rule
      `transcriptionUsageSeconds` applies: type exactly `duration`, and N a positive finite number. Share the
      validation; don't duplicate it.
   3. else the logged zero (unchanged).

   This is not keyed on the target type, mirroring the one-shot fallback. audio.cpp's done event states no usage, so
   its behaviour is unchanged.
5. **The benchmark accepts sglang-omni's stream end.** `readSynthesisStream`/`complete` (`benchmark_audio_measure.go`)
   count a clean EOF after `speech.audio.done` as the end of the stream for an `sglang_omni` target, and only for that
   kind. audio.cpp still needs `[DONE]`.

ADR-041 holds throughout: a present-but-unusable signal is never billed, and every gap stays a logged zero.

## Tasks

### Task 1: speech relay (decisions 1-3)

**Files:**
- `gateway/backend/internal/gateway/speech_stream.go`: scan fields; constructor gets the target kind; header rate;
  terminal sentinel helper; billing order.
- `native_passthrough.go`:
  - pass `rel.target` or its kind to `newSpeechStreamScan`;
  - at clean EOF in `nativeCopier.run`, finish the carry and write the sentinel when the scan asks for it.
- `speech_handler.go`: `relaySpeech` pcm upstream body for an sglang_omni stream.

**Tests** in `speech_stream_test.go`, `speech_handler_test.go` and the relevant relay tests (see the existing
`X-Sample-Rate` test near `speech_stream_test.go:1702`):
- an sglang_omni SSE stream with done + EOF:
  - the client receives the sentinel;
  - with a cached rate it bills `audio_second`;
  - without one, with headers, it bills `audio_second` from the header rate;
  - without either it bills `audio_bytes`;
- an sglang_omni SSE stream with an `error` event and no done: no sentinel, logged zero;
- an sglang_omni stream cut before done: no sentinel, zero;
- an sglang_omni stream that already sent `[DONE]`: no second sentinel;
- an unterminated last frame: the sentinel frame bytes start with `\n\n`;
- an audio.cpp SSE stream with done + EOF and no `[DONE]`: unchanged (no sentinel, logged zero);
- an audio.cpp stream with the X- headers: no header rate;
- invalid headers (0, negative, non-decimal, bit depth 12, overflow): no header rate;
- the raw `audio/pcm` sglang_omni shape: billed via the header rate without a cache;
- the client disconnects while the sentinel is written: the status is a disconnect, zero;
- relaySpeech: a stream to sglang_omni without `response_format` sends `"pcm"` upstream; with `"pcm"` the body is
  unchanged; to audio.cpp the body is unchanged; the buffered retry body drops it.

### Task 2: streamed transcription usage (decision 4) and benchmark (decisions 2b and 5)

**Files:**
- `transcription_stream.go`: event usage, scan field, billing fallback; share validation with
  `transcriptionUsageSeconds` in `transcription_handler.go`.
- `benchmark_audio_measure.go`: `streamSynthesis` body, `readSynthesisStream` and `complete`.

**Tests:**
- a streamed non-WAV upload with done usage bills N;
- a WAV upload still bills the WAV even when usage disagrees;
- usage of type other than `duration`, `seconds` 0/negative/string/NaN-like: logged zero;
- done usage without `[DONE]`: still incomplete, zero;
- benchmark:
  - sglang_omni done + EOF is complete; audio.cpp done + EOF is still "without [DONE]";
  - the sglang_omni stream body carries `response_format: "pcm"`, the audio.cpp body doesn't.

### Task 3: docs

- **ADR-103** in `docs/architecture/09-architecture-decisions.md`: amend the decision (the gateway-written sentinel,
  the pcm `response_format`, the header rate fallback, the streamed transcription usage, the benchmark end rule).
  Remove the resolved "Streaming residuals for sglang-omni" bullet; anything still open stays.
- **compatibility-and-inference.md §3.6** (~:888-904) and **§3.7** (~:1258-1269): describe the new rules and drop the
  residual sentences.
- Check **api-surface.md / openapi.yaml** and **telemetry-usage-observability.md** for statements about the speech
  stream's end, streamed billing or `[DONE]`, and update them where they now say something false.
- `make lint-docs` must pass.

## Gates

- `cd gateway/backend`:
  - `golangci-lint fmt --diff`
  - `GOTOOLCHAIN=go1.26.5 golangci-lint run ./...`
  - `GOTOOLCHAIN=go1.26.5 go test ./... -skip 'TestVoice|TestLiveTranscriptionLoopback'`
  - `go test -count=1 ./internal/archtest/`
- `make lint-docs`
- local Sonar: `GOTOOLCHAIN=go1.26.5 make sonar-gate`, then `make sonar-findings` and `make sonar-branch-findings`.
  Judge only findings attributed to this branch.

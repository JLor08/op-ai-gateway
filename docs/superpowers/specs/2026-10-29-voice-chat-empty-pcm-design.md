# Voice Chat: keepalive silence over gaps — design (#339)

## Intent & why

In Voice Chat the ordered speech relay streams each `<speak>` section's audio to
the browser as `audio_delta` frames in document order. The relay is a blocking
range over the per-leg audio channels — no timer, no way to fill a gap. When a
gap outlasts the client's ~1200 ms playing-gap window the player underruns,
`playing` flips false, and the **half-duplex gate opens the microphone
mid-turn** (and a long retry after the answer finished can drain the turn
early). Two gaps cause this: a transient **pre-stream retry backoff** (#337) and
an **inter-section synthesis gap** (section N done, N+1 still synthesizing).

#337 deferred "empty-PCM during a retry" as a follow-up. This is that follow-up,
widened to **any in-run gap where more section audio is still coming**: fill it
with gateway-generated silence so the client's pipeline stays fed, `playing`
stays true, and the mic stays gated, until real audio resumes or the turn ends.

## Goals

- During any in-run gap where more section audio is still coming (a pre-stream
  retry backoff **or** an inter-section synthesis gap), keep the client's audio
  pipeline fed so `playing` stays true and the half-duplex mic gate stays closed.
- Real audio resumes seamlessly after silence; the only quiet the user hears is
  the unavoidable gap itself.
- Backend-only: no frontend change, no wire-protocol change, no schema/migration,
  no `ServerAgent` bump.

## Non-goals / anti-goals

- **Not** a new keepalive frame type (the accounting-neutral alternative was
  weighed and rejected — `audio_delta` reuse needs no frontend/wire change).
- **Not** filling the pre-first-audio "thinking" gap when no output rate is yet
  known: fail-open (no silence), exactly today's behavior.
- **No** change to document order, the single monotonic `seq` stream, the commit,
  the billing of real audio, or the LLM/ASR legs.
- **Anti-goal:** silence must never be stored, never double-bill, and never
  prevent the turn from ending.

## Constraints (from the code; anchors for the builder)

- **Wire:** one audio frame type, `audio_delta`, JSON text, `{seq, section?,
  rate, audio=base64 s16le PCM}`; `seq` is one monotonic stream across all
  sections, `section` is omitempty and transcript-attribution-only
  (`voice_chat_ws.go:378-411`). Built and sent in `relayAudio`
  (`voice_chat_ws.go:1643-1658`, send at `:1657`).
- **Relay:** `relayVoiceSections` is a blocking nested range
  (`chat_runs_voice.go:373-383`) — no select, no timer; it reads only the
  channels and knows nothing of leg state (synthesizing / retry-backoff / done /
  failed all look identical as "audioCh open, no value"). Terminal signal is
  `audioCh` close → advance; `legsCh` close → exit. Seq/Section assigned here;
  chunks minted in `readSpeechEvents.consumeDelta` (`chat_runs_audio_stream.go:684`).
- **Drain floor (server):** `accountPlayback` (`voice_chat_ws.go:1665-1675`) adds
  relayed audio's real-time duration to the floor (`byteRate = rate * 2`), sets
  `firstAudioAt`, and sets `playbackRateUnknown` on a `rate<=0` chunk;
  `relayedAudioChunks++` in `relayAudio` (`:1645`); the client's `played_through`
  is validated in `onPlaybackDone` (`:1818-1826`, a signal below
  `relayedAudioChunks` is ignored). Floor/ceiling model at `:865-918`; ceiling
  `VoicePlaybackFallbackMs = 30000`.
- **Client:** `PLAYING_GAP_MS = 1200` (`voiceSession.ts:430`); any pushed
  `audio_delta` calls `clearPlayingGap()` + `setPlaying(true)` (`:729-730`), so a
  received frame resets the timer; `player.onEmpty` re-arms it in `speaking`
  (`:711-719`). The gateway-facing `playback_done` is **not** debounced — it is
  sent on every queue drain with `played_through: receivedChunks` (`:712`).
- **Rate known pre-stream:** `speechTurnAttempt` resolves
  `sampleRate, streaming := s.speechRunStreamRate(...)` before the request
  (`chat_runs_audio.go:232`), reading the cached per-mapping `output_byte_rate`
  (`chat_runs_audio_stream.go:271-293`, `portalSpeechStreamRate:140-151`;
  `sample = byteRate / 2`; cache `routing/store.go:1410-1453`, ADR-068). The
  **stream gate** is the `streaming` bool: a mapping with no agreed cached rate,
  `Stream=false`, or a non-event-stream 2xx response returns one **buffered WAV**
  for that leg (`streamed=false`, `chat_runs_audio.go:269/281/298`).
- **Commit independence:** the relay sink (`audioSink`) and the storage sink
  (`keep`) split at `consumeDelta` (`chat_runs_audio_stream.go:684` relay / `:686`
  store); stored `parts` never read the relay output, and the commit
  (`commitVoiceTurn`, `chat_runs_voice.go:830-889`) assembles only the per-leg
  `parts`. **Silence injected at the relay can never reach stored audio** (verified).
- **#335 silence helper:** `voiceSilenceFrame` (`voice_chat_ws.go:2190-2200`)
  makes zeroed bytes but sized to the **mic** ticket shape (16 kHz/mono/s16le) —
  NOT reusable as-is; the client-direction silence must be sized to the **TTS
  output rate**.

## The design (decided)

1. **Silence = ordinary `audio_delta`.** A silence chunk is a
   `runAudio{Seq, Rate, Data=zeros}` relayed through the same `publishAudio` →
   `relayAudio` path as real audio. The client plays zeros inaudibly and resets
   its playing-gap timer on it. **No frontend/wire change.**
2. **Restructure `relayVoiceSections` into a select loop.** It tracks:
   - `lastRate` — the `Rate` of the last **real** chunk relayed (same run = same
     rate);
   - `fallbackRate` — the run's cached output sample-rate, resolved **once** at
     run start (see 3), used before the first real chunk.
   The silence rate is `lastRate` if known, else `fallbackRate`, else **none**
   (no silence). While the relay is blocked waiting for the next real chunk
   (within a leg, `leg.audioCh`) or for the next leg (`legsCh`) **and the turn is
   in progress**, a cadence ticker fires; on each tick, if still waiting and a
   rate is known, it relays one silence frame. A real chunk is relayed, updates
   `lastRate`, and resets the wait. `audioCh` close → advance; `legsCh` close →
   exit (**no trailing silence**). Honors `ctx` cancel.
3. **Resolve the run's fallback rate once.** At run start (after the speech
   target is resolved) read the cached `output_byte_rate` for the resolved
   mapping and pass its sample-rate (`byteRate/2`) into `relayVoiceSections` as
   `fallbackRate`; absent/unbenchmarked → `0` (no fallback). This gives even a
   first-section pre-stream retry gap a rate when the mapping is benchmarked.
4. **Silence sizing & cadence.** Frame bytes = `rate * silenceFrameMs/1000 * 2`
   (mono s16le), zeroed, via a small pure helper (e.g. `voiceSilencePCM(rate,
   ms)`). `silenceFrameMs` and the cadence are package consts ≈ one frame
   duration (~160 ms) so the player stays continuously fed, well under
   `PLAYING_GAP_MS = 1200`. **No operator knob** (out of scope).
5. **Accounting is consistent, not special-cased.** Silence flows through
   `accountPlayback` (floor += its real duration) and `relayedAudioChunks++`,
   which is correct because the client really plays it: `played_through` catches
   up and the floor reflects the true playback timeline. Silence emission happens
   only **during the run** (while the relay is active); by the time
   `awaitingPlayback` is set all legs are joined, so no silence races the drain
   and there is no trailing silence to delay turn end. Silence is never a usage
   event (it never passes through `keep`/the usage writer), so it does not bill.

## Edge cases to handle

- **No rate known** (unbenchmarked mapping, first gap with no `fallbackRate`,
  buffered-only run) → **no silence** (today's behavior); must not crash or busy-spin.
- **Buffered (non-streaming) leg** → one WAV, no mid-leg gap, but a gap
  before/after it; `lastRate`/`fallbackRate` apply, else skip.
- **Permanently-failed leg** → `audioCh` closes with no audio → relay advances;
  silence relayed during its backoff is bounded by the retry budget and correctly
  counted.
- **Turn end / cancel** → `legsCh` close or `ctx.Done` stops silence immediately.
- **`seq`/`section`** → silence consumes a `seq` like any `audio_delta`
  (monotonic, inaudible) and **omits `section`** (it is not a section's content).

## What's left to the builder

- Exact constant values (`silenceFrameMs`, cadence ≈ 160 ms) — sane package
  consts; a knob is explicitly out of scope.
- The precise select-loop shape and the exact run-start rate-resolution call.
- The tests below.

## Verification

- Mock-backed tests (like #337): the relay emits silence during a simulated gap
  (slow leg / retry backoff), stops on the next real chunk, emits none after the
  turn ends; no known rate → no silence; a silence chunk never appears in stored
  `parts`; the drain floor stays consistent and the turn still ends; `ctx` cancel
  stops silence.
- Gates: golangci-lint fmt+run (gateway **and** server-agent); `go test ./...`
  both modules; the voice suite under `-race` scoped; `make lint-docs`.
- Not live-reproducible (no mic → no real turn → no `<speak>` sections), as with
  #337; covered by tests.

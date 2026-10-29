# Voice Chat — mute-aware ASR loopback (design)

**Issue:** #335
**Status:** approved design (brainstormed 2026-10-29)

## Intent & why

Muting the microphone during a Voice Chat turn stalls the ASR loopback: the
client stops sending PCM (`voiceSession.ts` `deliverAudio` drops every batch while
`micMuted`) and tells the gateway nothing, so an already-open loopback starves and
audio.cpp aborts the body with `live request body stalled: no data within the idle
timeout`. (#331/PR #332 fixed this for the dial window; muting is a sustained
send-pause the gateway must handle explicitly.)

## Chosen design

Make the gateway **mute-aware**, keep the connection alive long enough to avoid a
stall, let the operator tune how long a muted mic holds the ASR open, and never
let silence produce phantom transcripts.

- **New `mic_mute` / `mic_unmute` up-control** on the portal Voice Chat WebSocket
  (browser↔gateway only; no `ServerAgent`/schema change). The client sends it from
  `setMicMuted`. The client still sends **no audio** while muted (unchanged
  `deliverAudio` drop) — only the control.
- **Filter ASR output while muted (always):** the state machine discards ASR
  partial/final events while `micMuted`, so a model that hallucinates on silence
  cannot inject phantom text into the turn.
- **Gateway-generated keepalive silence:** while muted, the state machine feeds
  periodic silence frames (zeroed PCM of the ticket's fixed shape) into the open
  loopback body, so audio.cpp's own idle timeout never fires before the gateway's
  own decision. Generated server-side so nothing audio-shaped leaves the browser.
- **Operator-tunable mute teardown timer:** a new `OP_AI_GATEWAY_VOICE_*` knob in
  `voiceTiming`. After it elapses while still muted, the machine tears the loopback
  down gracefully (clean EOF, no error frame), freeing its admission, per-route
  slot and model pin and ending its metering, and stops the keepalive. **`0` means
  keepalive** — never tear down while muted (the keepalive silence keeps it up).
- **On `mic_unmute`:** stop filtering and the keepalive; if the loopback was torn
  down it reopens lazily on the next real PCM frame; if kept alive it continues.

## Decisions taken (resolving the issue's open points)

- **Default teardown (knob unset): proposed 10 s** — a sustained 10 s mute frees
  the ASR; quick mute/unmute within 10 s keeps it. `0` = keepalive is an explicit
  opt-in. (Confirm the exact default at plan review.)
- **Encoding unset-vs-0:** unlike the other voice knobs (where non-positive means
  "use the default"), `0` is a meaningful value (keepalive) here. The config layer
  distinguishes an **absent** knob (→ the default) from an explicit **0**
  (keepalive); the resolved `voiceTiming` carries the teardown as a value plus a
  "keepalive" sense (e.g. a sentinel or a bool), never conflating absent with 0.

## Invariants & constraints

- **Single-owner preserved:** only the run loop reads/writes machine fields; the
  keepalive and teardown are run-loop timers (like the existing voice timers), and
  the loopback teardown reuses the #331 async-pump teardown. Clean under `-race`.
- **Behavior-preserving** when NOT muted: listening, turns, the continuation
  window, the #331 concurrent feed and teardown are unchanged. The standalone
  live-transcription path is untouched.
- Backend + frontend only. No `ServerAgent` version bump, no wire change beyond the
  two new portal controls, no schema/migration.
- A new config knob must be registered and documented (the `lint-docs`
  registered-flags + config-env.md checks must stay green).

## Non-goals

- No change to audio.cpp's idle timeout (not in this repo) or to how the mic is
  captured. No server-side reflection of mute into state frames (the client already
  shows the muted indicator) unless it falls out naturally.
- Not changing the per-chunk loopback lifecycle beyond the mute case.

## Left to the builder

- The keepalive-silence cadence (any interval comfortably under audio.cpp's idle
  timeout) and the exact silence-frame size from the ticket's PCM shape.
- The precise config encoding of unset-vs-0 (sentinel vs bool), consistent with the
  repo's config patterns.
- The exact timer wiring into the run loop's select (mirroring the existing
  silence/grace/tail timers).

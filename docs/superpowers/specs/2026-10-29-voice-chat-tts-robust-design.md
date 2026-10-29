# Voice Chat — robust speech generation (design)

**Issue:** #337
**Status:** approved design (brainstormed 2026-10-29)

## Intent & why

Two brittleness points in Voice Chat speech generation (the per-section TTS legs of
the voice run, `chat_runs_voice.go` `launchVoiceSpeech` → `generateSpeechTurn` →
`/v1/audio/speech`):

1. A briefly busy / momentarily unavailable TTS model fails the leg immediately and
   degrades the turn — no second chance.
2. A very fast LLM closes many `<speak>` regions in quick succession, each firing a
   concurrent speech leg; concurrency is bounded today only by `voiceSectionQueue =
   64` (the ordered-relay queue), so up to ~64 `/v1/audio/speech` syntheses can run
   at once and overwhelm the TTS.

## Chosen design (backend only)

### 1. Bounded TTS synthesis concurrency (backpressure)

A **per-run semaphore** caps how many section legs **synthesize at once** to a small
**K**, decoupled from the ordered-relay queue (`voiceSectionQueue` stays deep for
document ordering). Each leg goroutine (`launchVoiceSpeech`) acquires the semaphore
before `generateSpeechTurn` and releases it after; the acquire blocks (honoring the
run's context) when K legs are already synthesizing. A fast LLM may queue many legs
but only K synthesize concurrently — one relaying while the next synthesizes ahead.

- **Operator-tunable**, default **K = 2**; absent/≤0 → the default. (The voice run's
  legs are already concurrent goroutines coordinated by channels — not the
  state-machine single-owner — so the semaphore is a plain run-scoped channel.)

### 2. Retry a speech leg on a transient, pre-stream failure

Before degrading, a leg that fails **before emitting any audio** waits a short
backoff and retries, up to a tunable attempt count.

- **Billing- and ordering-safe:** retry only when NO audio chunk has been handed to
  the ordered relay yet (a pre-stream failure bills nothing and duplicates nothing).
  A mid-stream failure (after a chunk was relayed) is NOT retried.
- **Transient (retry):** gateway busy (503), admission (429), loopback-dispatch
  error; and a **5xx classified by its upstream message** — audio.cpp returns 500
  both for a transient hiccup and for a bad voice, so a 5xx whose message matches a
  recognized **permanent** pattern (voice/reference not found) is NOT retried; any
  other 5xx is.
- **Permanent (no retry):** 4xx request errors.
- **Tunable** attempt count, default **2 retries**; absent → default, explicit
  **0 → retry disabled**. Backoff is a fixed, growing const (~250ms base), capped so
  a turn never hangs.

A retry leaves a brief gap in that section's playback; the server's drain floor and
the client's ~1200ms playback-done debounce cover a short retry budget, so the turn
is not ended prematurely.

## Constraints

- Backend only (`chat_runs_voice.go`, `chat_runs_audio.go`, `internal/config`,
  `cmd/gateway`). No `ServerAgent` version bump, no schema/migration. The LLM/ASR
  legs, the per-chunk loopback, and the standalone speech/live paths are untouched.
- Document ordering and the single ordered relay are unchanged; the semaphore gates
  only synthesis start, not relay order.
- Two new config knobs must be registered + documented (lint-docs stays green).
- The voice suite stays green under `-race` (the semaphore + retry live in the leg
  goroutines, coordinated by channels/context, no new shared-state race).

## Non-goals (deferred follow-up)

- **Empty-PCM during a retry** (stream silence to keep the client player fed across a
  long retry gap) — only needed if the retry budget is tuned beyond the client
  debounce; deferred.
- No retry on the LLM/ASR legs; no change to `voiceSectionQueue`'s ordering role.

## Left to the builder

- The exact transient/permanent classification seam (where the HTTP status / stable
  code / upstream 5xx message is inspected — thread it out of `generateSpeechTurn` or
  classify its returned code/message), and the recognized permanent 5xx message
  pattern(s).
- The "no audio emitted yet" guard (e.g. a flag set by the leg's audio sink) that
  makes the retry pre-stream-only.
- The exact backoff schedule and the config encoding for the two int knobs
  (concurrency: absent/≤0 → default; retry count: absent → default, explicit 0 →
  disabled), consistent with the existing `OP_AI_GATEWAY_VOICE_*` knobs.

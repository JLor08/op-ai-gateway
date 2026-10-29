# Voice Chat robust speech generation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make Voice Chat speech generation robust: cap concurrent TTS synthesis to a small tunable K (backpressure for fast LLMs), and retry a speech leg on a transient pre-stream failure (tunable count) instead of degrading the turn.

**Architecture:** A per-run semaphore gates how many section legs synthesize at once (decoupled from the ordered-relay queue). Each leg's synth is wrapped in a retry that fires only on a transient, pre-stream (no-audio-emitted) failure, classified by HTTP status / stable code / a recognized-permanent 5xx upstream message. Two new operator-tunable `OP_AI_GATEWAY_VOICE_*` knobs.

**Tech Stack:** Go (`gateway/backend/internal/gateway`, `internal/config`, `cmd/gateway`), the voice test harness, `go test -race`.

**Spec:** docs/superpowers/specs/2026-10-29-voice-chat-tts-robust-design.md

## Global Constraints

- Backend only. No `ServerAgent` version bump, no schema/migration. Do NOT change the LLM/ASR legs, `voiceSectionQueue`'s ordering role, the per-chunk loopback, or the standalone speech/live paths.
- Billing- and ordering-safe retry: retry ONLY a pre-stream failure (no audio chunk handed to the ordered relay yet) — never duplicate audio, never re-bill.
- Two new config knobs registered + documented so `make lint-docs` stays green.
- The voice suite stays green under `-race` (semaphore + retry live in the leg goroutines, coordinated by channels/context).
- Gates before PR: golangci-lint fmt+run (gateway **and** server-agent); `go test ./...` both modules; `go test -race -run '(Voice|Speak|Speech|Stream|Transcription|WAV)' ./internal/gateway/... -timeout 15m`; `make lint-docs`.

## Review Focus

- **Retry is pre-stream-only:** a failure after a chunk was relayed must NOT retry (would duplicate audio / re-bill). (Task 3 test.)
- **5xx classification:** a bad-voice 500 (recognized permanent message) is NOT retried; a transient 5xx is. 4xx never retried; 503/429/dispatch retried. (Task 3 test.)
- **Retry-count encoding:** absent → default 2; explicit 0 → retry disabled (one real synth, no retry). (Task 1 test.)
- **Concurrency cap holds:** with many sections queued, at most K synthesize at once; document order and the single ordered relay are unchanged. (Task 2 test.)
- **No leak / -race:** the semaphore is released on every path (success, failure, cancel); a retrying leg holds its slot but is bounded. (Task 2/3 tests, -race.)

---

### Task 1: Two config knobs (TTS concurrency K + retry attempts)

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go` (VoiceTiming/voiceTiming structs, defaults, resolveVoiceTiming, consts) OR a voice-run config holder if cleaner — follow the `OP_AI_GATEWAY_VOICE_MUTE_TEARDOWN` pattern added in #335.
- Modify: `gateway/backend/internal/config/config.go`, and `cmd/gateway/voice_timing.go` (`voiceTimingFromConfig`).
- Test: machine/config tests.

**Interfaces (produced, consumed by Tasks 2/3):**
- `OP_AI_GATEWAY_VOICE_TTS_CONCURRENCY` → int; **absent/≤0 → default 2**; positive → that cap. Exposed on the resolved voiceTiming as e.g. `ttsConcurrency int` (always ≥1).
- `OP_AI_GATEWAY_VOICE_TTS_RETRIES` → int; **absent → default 2; explicit 0 → disabled**; positive → that many retries. Needs the unset-vs-0 distinction (like the mute-teardown knob: config `durationAllowZero` twin for an int, or an equivalent). Exposed as e.g. `ttsRetries int` (0 = no retry).

- [ ] **Step 1: Write failing tests** for both knobs' resolution: concurrency absent/≤0 → 2, positive → value; retries absent → 2, explicit 0 → 0 (disabled), positive → value. (Mirror the #335 mute-teardown resolve + config tests, incl. absent-vs-0 for retries.)
- [ ] **Step 2: Run them, confirm they fail.**
- [ ] **Step 3: Add the consts + VoiceTiming fields + resolved fields + resolveVoiceTiming logic** (concurrency clamps to ≥1 default 2; retries preserves explicit 0). Decide the int unset-vs-0 encoding consistent with the repo.
- [ ] **Step 4: Wire both env knobs** in config.go + voiceTimingFromConfig (distinguish absent from 0 for the retry count).
- [ ] **Step 5: Run the tests to green.**
- [ ] **Step 6: Commit.** `feat(voice-chat): add tunable TTS concurrency + retry knobs (#337)`

---

### Task 2: Per-run TTS synthesis concurrency semaphore (backpressure)

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`executeVoiceRun` ~127, `launchVoiceSpeech` ~333)
- Test: `voice_chat_turn_test.go`

**Interfaces:** consumes Task 1's `ttsConcurrency`.

- [ ] **Step 1: Write the failing test** — drive a voice run whose LLM closes many `<speak>` regions quickly (more than K) against a fake TTS that records concurrent in-flight syntheses; assert the peak concurrency never exceeds K, every section still synthesizes, and the relayed audio stays in document order. (Use a small K via the timing seam.)
- [ ] **Step 2: Run it, confirm it fails** (today up to `voiceSectionQueue` run at once).
- [ ] **Step 3: Add a per-run semaphore** (a buffered channel of size `ttsConcurrency`, created in `executeVoiceRun`) and have each `launchVoiceSpeech` goroutine acquire it (honoring the run ctx) before `generateSpeechTurn` and release it after (on every exit path). Keep `legsCh`/`voiceSectionQueue` and the ordered relay unchanged.
- [ ] **Step 4: Run the test + the voice suite to green.**
- [ ] **Step 5: Commit.** `feat(voice-chat): cap concurrent TTS synthesis with a per-run semaphore (#337)`

---

### Task 3: Retry a speech leg on a transient pre-stream failure

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`launchVoiceSpeech`) and/or `chat_runs_audio.go` (`generateSpeechTurn` ~205, to expose the status/code for classification)
- Test: `voice_chat_turn_test.go`

**Interfaces:** consumes Task 1's `ttsRetries`; runs inside the Task 2 semaphore slot.

- [ ] **Step 1: Write the failing tests** (fake TTS child programmable per attempt): (a) a transient pre-stream failure (503) then success → the leg retries and succeeds, audio relayed once (no dup); (b) a recognized-permanent 500 (voice/reference not found) → NO retry, fails fast; (c) a transient 500 (other message) → retried; (d) a 4xx → no retry; (e) `ttsRetries=0` → no retry; (f) a mid-stream failure (fail after one chunk) → NOT retried and the already-relayed chunk is not duplicated.
- [ ] **Step 2: Run them, confirm they fail.**
- [ ] **Step 3: Implement the retry** around the leg's synth: classify the outcome (transient: 503/429/dispatch, or a 5xx whose upstream message is NOT a recognized permanent pattern; permanent: 4xx or a recognized voice-not-found 5xx), and retry ONLY when the failure is transient AND no audio was emitted to the relay yet (guard via a sink-wrote flag). Up to `ttsRetries` attempts with the fixed growing backoff (honoring the run ctx). Thread the HTTP status / code out of `generateSpeechTurn` as needed for classification.
- [ ] **Step 4: Run the tests + the voice suite to green.**
- [ ] **Step 5: Commit.** `feat(voice-chat): retry a speech leg on a transient pre-stream TTS failure (#337)`

---

### Task 4: Docs — register the knobs; ADR amendment

**Files:**
- Modify: `docs/architecture/reference/config-env.md` (the two new knobs)
- Modify: `docs/architecture/09-architecture-decisions.md` (amend the voice-run / speech-leg ADR — ADR-080 phase 3, the ordered speech legs — with the concurrency cap and the transient-retry, incl. the pre-stream/billing-safe + 5xx-by-message rules and the deferred empty-PCM follow-up)
- Modify any other doc that states the speech-leg concurrency/failure behavior (e.g. 06-runtime-view §6.1.6 if it describes it)

- [ ] **Step 1: Document both knobs** in config-env.md with exact names + semantics (concurrency default 2, absent/≤0 → default; retries default 2, absent → default, 0 → disabled).
- [ ] **Step 2: Amend the ADR** accurately (concurrency cap, pre-stream transient retry, 5xx-by-message, the deferred empty-PCM follow-up as a residual). No overclaim.
- [ ] **Step 3: Run `make lint-docs` to green** (except the known branch-local citation).
- [ ] **Step 4: Commit.** `docs(voice-chat): document the TTS concurrency + retry knobs (#337)`

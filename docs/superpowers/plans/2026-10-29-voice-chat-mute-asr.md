# Voice Chat mute-aware ASR loopback — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the gateway mute-aware so muting the mic no longer stalls the ASR loopback: filter ASR output while muted, keep the loopback fed with gateway-generated silence, and tear it down after an operator-tunable timer (0 = keepalive), reopening on unmute.

**Architecture:** New `mic_mute`/`mic_unmute` portal WS controls drive a `micMuted` flag on the single-owner voice state machine. While muted it discards ASR events, runs a keepalive-silence ticker into the loopback body, and arms a teardown timer (a new `voiceTiming` knob; 0 = keepalive) that gracefully closes the loopback. Unmute resumes (lazy reopen on next PCM, or continue if kept alive).

**Tech Stack:** Go (`gateway/backend/internal/gateway`, `internal/config`), React/TS (`gateway/frontend`), the voice test harness, Vitest, `go test -race`.

**Spec:** docs/superpowers/specs/2026-10-29-voice-chat-mute-asr-design.md

## Global Constraints

- Backend + frontend only. No `ServerAgent` version bump, no schema/migration; the only wire change is the two new portal-WS controls. The standalone live-transcription path and the #331 async pump/teardown are reused, not rebuilt.
- **Single-owner invariant** (voice_chat_ws.go ~555-560): only the run loop touches machine fields; the keepalive and teardown are run-loop timers (like the existing silence/grace/tail timers). Clean under `-race`.
- Behavior-preserving when NOT muted.
- A new config knob must be **registered and documented** so `make lint-docs` (registered-flags + config-env.md checks) stays green.
- Gates before PR: golangci-lint fmt+run (gateway **and** server-agent); `go test ./...` both modules; voice suite `go test -race -run '(Voice|Speak|Speech|Stream|Transcription|WAV)' ./internal/gateway/... -timeout 15m`; frontend `npm run build` + `prettier --check .` + `vitest run --retry=2`; `make lint-docs`.

## Review Focus

- **Config unset-vs-0:** an absent knob must resolve to the default; an explicit `0` must mean keepalive (never torn down). A wrong encoding silently breaks one of the two. (Task 1 test.)
- **Keepalive cadence + stop:** silence must be fed often enough to beat audio.cpp's idle timeout, and must stop on teardown and on unmute — no silence after the loopback is gone, no ticker leak. (Task 2 test.)
- **ASR filter boundary:** while `micMuted`, ASR partial/final events are discarded and never reach `pendingText` or a down-frame; unmute resumes cleanly. (Task 2 test.)
- **Teardown-at-T frees resources:** after the timer, the loopback is gone (slot/model freed, metering ended) with no error frame; `-race` clean alongside the keepalive ticker. (Task 2 test.)
- **Mute during the continuation window** (thinking, loopback open): handled the same as in listening. (Task 2 test.)

---

### Task 1: The mute-teardown config knob (voiceTiming + env, unset-vs-0 semantics)

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go` (the `VoiceTiming`/`voiceTiming` structs ~140-160, `defaultVoiceTiming`, `resolveVoiceTiming`, and a `Voice*Ms` const)
- Modify: `gateway/backend/internal/config/config.go` (parse the new `OP_AI_GATEWAY_VOICE_*` env; distinguish absent from `0`)
- Modify wherever `ServerDeps`/config maps env → `VoiceTiming` (follow the existing `OP_AI_GATEWAY_VOICE_*` wiring)
- Test: `gateway/backend/internal/gateway/voice_chat_machine_test.go` (resolve semantics) and/or `internal/config` test

**Interfaces:**
- Produces: `voiceTiming.muteTeardown time.Duration` and a keepalive sense (a bool like `muteKeepalive`, or a sentinel), resolved so **absent → default (proposed 10s)**, **explicit 0 → keepalive**, **>0 → that duration**. Consumed by Task 2.

- [ ] **Step 1: Write failing tests** for the resolve semantics: absent → default; explicit 0 → keepalive sense; explicit >0 → that value. (Mirror existing `resolveVoiceTiming` tests.)
- [ ] **Step 2: Run them, confirm they fail.**
- [ ] **Step 3: Add the const + `VoiceTiming` field + resolved `voiceTiming` field(s)** and the `resolveVoiceTiming` logic that preserves 0 as keepalive (do NOT fold 0 into the default). Decide and implement the encoding (sentinel or bool) consistent with the repo.
- [ ] **Step 4: Wire the env knob** in `config.go` and the ServerDeps→VoiceTiming mapping, distinguishing an absent env var from `0`.
- [ ] **Step 5: Run the tests to green.**
- [ ] **Step 6: Commit.** `feat(voice-chat): add the operator-tunable mute-teardown timing knob (#335)`

---

### Task 2: Backend mute-awareness (control, ASR filter, keepalive silence, teardown)

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go`
- Modify: `gateway/backend/internal/gateway/voice_chat_turn_test.go` / `voice_chat_machine_test.go`

**Interfaces:**
- Consumes: Task 1's `voiceTiming.muteTeardown` (+ keepalive sense); the #331 loopback opener/pump/teardown; `onPCM` (~884), `onControl` (~1071), `onASREvent` (~1126), `parseVoiceUpControl` (~204), the control consts (~50-53), the run-loop select (~746-785).

- [ ] **Step 1: Write failing tests** (harness, reusing the #331 `fakeVoiceASRChild`): (a) with a loopback open, a `mic_mute` control keeps it alive via gateway silence and the child receives data (no stall) past audio.cpp's idle window; (b) after the teardown timer the loopback is gone (child connection closed cleanly, no error frame to the browser); (c) `muteTeardown=0` keeps it open indefinitely; (d) ASR deltas/finals arriving while muted produce NO `asr_partial`/`asr_final` down-frames and no `pendingText`; (e) `mic_unmute` then a real PCM frame reopens the loopback and transcription resumes.
- [ ] **Step 2: Run them, confirm they fail.**
- [ ] **Step 3: Add the controls** `mic_mute`/`mic_unmute` (consts + `parseVoiceUpControl`) and handle them in `onControl`: set `m.micMuted`; on mute arm the keepalive ticker + (when not keepalive) the teardown timer; on unmute disarm both and resume.
- [ ] **Step 4: Filter ASR while muted** in `onASREvent`: discard events while `m.micMuted` (no `pendingText`, no down-frame).
- [ ] **Step 5: Keepalive silence + teardown** as run-loop timers in the select: the keepalive ticker enqueues a zeroed PCM frame (the ticket's shape) into the loopback feed on its interval while muted; the teardown timer, on fire, gracefully tears the loopback down (reuse the #331 teardown) and stops the keepalive. Ensure both stop on unmute and on turn teardown (no leak).
- [ ] **Step 6: Run the tests + the full voice suite to green.**
- [ ] **Step 7: Commit.** `feat(voice-chat): make the ASR loopback mute-aware — keepalive, filter, tunable teardown (#335)`

---

### Task 3: Frontend — send the mute control

**Files:**
- Modify: `gateway/frontend/src/components/chat/voiceSession.ts` (`setMicMuted` sends `mic_mute`/`mic_unmute` via `sendControl`)
- Modify: the relevant frontend test (`voiceSession`/`ChatStore` tests)

- [ ] **Step 1: Write the failing test** that toggling mic-mute sends a `mic_mute` control up, and unmuting sends `mic_unmute` (the client still sends no audio while muted).
- [ ] **Step 2: Run it, confirm it fails.**
- [ ] **Step 3: Extend `sendControl`'s type union and `setMicMuted`** to emit the control; keep the existing `applyMicMute`/`deliverAudio` drop (no audio while muted).
- [ ] **Step 4: Run the test + the frontend suite to green.**
- [ ] **Step 5: Commit.** `feat(portal): signal mic mute/unmute to the voice-chat backend (#335)`

---

### Task 4: Docs — register and document the knob; ADR-078 amendment

**Files:**
- Modify: `docs/architecture/reference/config-env.md` (the new `OP_AI_GATEWAY_VOICE_*` knob, with `0 = keepalive`)
- Modify: `docs/architecture/09-architecture-decisions.md` (an ADR-078 amendment: mute-aware loopback, keepalive silence, ASR filter, tunable teardown, `0`=keepalive)
- Any registered-flags source the `lint-docs` check reads, so the knob is "registered"

- [ ] **Step 1: Document the knob** in config-env.md matching Task 1's exact env name and semantics (absent → default, `0` → keepalive).
- [ ] **Step 2: Amend ADR-078** with the mute-aware behavior and the residual/tradeoffs (keepalive holds the slot and meters; teardown frees them).
- [ ] **Step 3: Run `make lint-docs` to green** (registered-flags + config-env.md + links/anchors).
- [ ] **Step 4: Commit.** `docs(voice-chat): document the mute-teardown knob and amend ADR-078 (#335)`

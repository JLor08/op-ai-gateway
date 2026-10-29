# Voice Chat ASR loopback — concurrent PCM feed — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Feed the Voice Chat per-turn ASR loopback's request body with PCM concurrently with the dial, so audio.cpp never idle-times-out the body during a cold-model dial — mirroring the standalone path's dedicated pump.

**Architecture:** The state-machine run loop stops calling the blocking loopback dial. On the first gated PCM frame of a turn it creates the pipe + a buffered per-turn PCM channel, starts a dedicated pump goroutine (channel → pipe writer) and the dial in its own goroutine, and forwards every frame to the channel without blocking. The dial reports success/failure to the run loop as a select event; the run loop enables the ASR relay on success and surfaces the gateway code+message as the error frame on failure.

**Tech Stack:** Go (`gateway/backend/internal/gateway`), the existing voice test harness (`voiceTurnHarness`, `fakeVoiceASRChild`) with `httptest` + `go test -race`.

**Spec:** docs/superpowers/specs/2026-10-29-voice-chat-loopback-pump-design.md

## Global Constraints

- Backend only: `gateway/backend/internal/gateway`. No wire-protocol change, **no `ServerAgent` version bump**, no schema/migration. Do NOT touch the standalone `liveReadLoop` / `handleLiveTranscription` / `handleBearerLiveTranscription` / `openLiveLoopback` / `proxyLiveDuplex`.
- **Single-owner invariant (voice_chat_ws.go:555-560):** only the `run` goroutine reads/writes `voiceMachine` fields. The pump and dial goroutines use locals + channels; the run loop sets `asrCh`/`respBody`/loopback fields only in response to the dial-outcome event. Must be clean under `-race`.
- Behavior-preserving for: a successful turn, a non-2xx loopback refusal (forward its code+message), a dial error, cancel/resume/finalize, and full teardown (no leaked goroutine; pipe + dial cancelled; pump + relay joined), including teardown racing an in-flight dial.
- Go gates before PR: `golangci-lint fmt --diff` + `golangci-lint run` (gofumpt/gocritic) for BOTH `gateway/backend` and `server-agent`; `go test ./...`; and the voice suite under `-race` scoped to avoid the unrelated 10m-timeout E2E tests: `go test -race -run '(Voice|Speak|Speech|Stream|Transcription|WAV)' ./internal/gateway/... -timeout 15m`.

## Review Focus

- **Teardown racing an in-flight dial:** a turn cancelled (resume/cancel/bye/ctx-done/finalize) while `Do` is still blocked and the pump is blocked on `pw.Write` must cancel the dial, unblock the pipe, join both goroutines, and leak nothing. (Task 2 test, under -race.)
- **Non-2xx refusal still surfaces its code+message** as the error frame, now via the event path (not the old synchronous return). (Task 1 test.)
- **Drop-oldest-on-full never blocks the run loop** and never splits a frame. (Task 2 test.)
- **No goroutine leak across per-chunk re-dials:** the pump + relay from a finalized chunk are joined before/with the next chunk's loopback. (Task 2 leak assertion.)
- **-race cleanliness:** no `voiceMachine` field is read/written off the run loop. (-race run in both tasks.)

---

### Task 1: Async loopback setup — pump goroutine + dial-outcome event

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go`
- Modify: `gateway/backend/internal/gateway/voice_chat_turn_test.go` (harness extension + the regression test)

**Interfaces:**
- Consumes: `openLiveLoopback` (unchanged), `voiceASRRelay`, `liveRefusalFrame`, the run loop select (voice_chat_ws.go:746-785), `micOpen` (:805).
- Produces: a per-turn buffered PCM channel field and a dial-outcome result type + channel on `voiceMachine`, consumed only by the run loop (Task 2 adds edge tests against them).

- [ ] **Step 1: Write the failing regression test** in `voice_chat_turn_test.go`. Extend `fakeVoiceASRChild` / `voiceTurnOpts` with a hook that DELAYS the fake ASR child's acceptance (it holds before reading/responding to its request body for a set delay) and RECORDS, with ordering, the request-body (PCM) bytes it receives. The test: start a turn, send several PCM frames, and assert the child receives PCM **before** the accept delay elapses (i.e. PCM is fed concurrently with the dial). On today's serial code this FAILS (no PCM until the dial completes).

- [ ] **Step 2: Run it to confirm it fails.**
  Run: `go test -run TestVoice...ConcurrentLoopbackFeed ./internal/gateway/ -count 1`
  Expected: FAIL (no PCM received during the accept delay).

- [ ] **Step 3: Add the async loopback fields to `voiceMachine`** (near the existing `pw/turnCancel/respBody/asrCh/relayWG`, voice_chat_ws.go:586-592): a buffered per-turn PCM channel (run loop → pump), and a dial-outcome result type (`{asrCh, respBody, turnCancel}` on success; `{code, message}` / error on failure) plus its channel (nil when no dial is pending, so its select case is disabled). Keep the single-owner comment accurate.

- [ ] **Step 4: Replace `openTurnLoopback` with an async opener** called from `onPCM` (voice_chat_ws.go:824-839). The opener (run loop): if `selfBaseURL == ""` return false as today; else create `turnCtx`/`turnCancel`, the pipe, the per-turn PCM channel, and the dial-outcome channel; set `m.pw`; start the pump goroutine (drains the PCM channel → `pw.Write`, returns on channel close or ctx done) and the dial goroutine (`openLiveLoopback` → on 2xx send a success result; on non-2xx build `liveRefusalFrame` and send a refusal result; on error send an error result). `onPCM` then: if no loopback open or opening, call the opener; forward the frame to the per-turn channel with **drop-oldest-on-full** (never block); keep the existing `micOpen` gate.

- [ ] **Step 5: Add the dial-outcome select case + handler** to `run` (voice_chat_ws.go:746-785): on success, start the ASR relay (`relayWG.Add(1)` + `go voiceASRRelay(turnCtx, respBody, asrCh)`) and set `m.asrCh`/`m.respBody`; on a refusal, `emitError(code, message)` + `setState(idle)` + tear the turn down; on a dial error, `failTurn("transcription.unavailable", ...)` — mirroring the current synchronous failure handling (voice_chat_ws.go:859-877). Clear the dial-outcome channel once consumed.

- [ ] **Step 6: Update teardown to stop the pump and join it** — `finalizeChunk` (voice_chat_ws.go:~1013-1026), `teardownTurn` (~1684), `detachRun` (~1644), and the `ctx.Done` branch (:750-754): close the per-turn PCM channel (stops the pump), cancel `turnCancel`, close the pipe, clear the new fields, and `relayWG.Wait()` to join the pump + relay. Safe to call when no loopback is open (idempotent), as today.

- [ ] **Step 7: Run the regression test + the full voice suite to green.**
  Run: `go test -run '(Voice|Speak|Speech|Stream|Transcription|WAV)' ./internal/gateway/ -count 1`
  Expected: PASS (including the new concurrent-feed test and every existing voice test unchanged).

- [ ] **Step 8: Commit.**
  ```bash
  git add gateway/backend/internal/gateway/voice_chat_ws.go gateway/backend/internal/gateway/voice_chat_turn_test.go
  git commit -m "fix(voice-chat): feed the ASR loopback body concurrently with the dial (#331)"
  ```

---

### Task 2: Teardown-mid-dial, drop-on-full, and -race hardening

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_turn_test.go` (and/or `voice_chat_ws_test.go`)

**Interfaces:**
- Consumes: the async loopback fields + opener from Task 1; the `voiceTurnHarness` / `fakeVoiceASRChild` accept-delay hook.

- [ ] **Step 1: Write a teardown-mid-dial test** — start a turn so the dial is in flight (child accept held), then cancel the turn (e.g. `cancel`/`bye`/handler ctx end) and assert the session tears down promptly with no goroutine left running (the pump and dial return; the pipe is closed; `relayWG` joins). Prefer an explicit join/goroutine-count assertion over a sleep.

- [ ] **Step 2: Write a drop-oldest-on-full test** — drive more PCM frames than the per-turn channel's capacity while the pump is held (child accept delayed) and assert the run loop never blocks (the session still processes a control/timer meanwhile) and the oldest frames are dropped, newest retained, with no split frame.

- [ ] **Step 3: Run the voice suite under -race.**
  Run: `go test -race -run '(Voice|Speak|Speech|Stream|Transcription|WAV)' ./internal/gateway/... -timeout 15m`
  Expected: PASS, no race, no leak.

- [ ] **Step 4: Commit.**
  ```bash
  git add gateway/backend/internal/gateway/voice_chat_turn_test.go
  git commit -m "test(voice-chat): pin teardown-mid-dial, drop-on-full and -race for the loopback pump (#331)"
  ```

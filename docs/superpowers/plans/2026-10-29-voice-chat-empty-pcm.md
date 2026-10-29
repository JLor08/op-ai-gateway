# Voice Chat: keepalive silence over gaps — Implementation Plan (#339)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fill any in-run gap in the Voice Chat speech relay (a pre-stream retry backoff or an inter-section synthesis gap) with gateway-generated silence, so the client's audio pipeline stays fed, `playing` stays true and the half-duplex mic stays gated until real audio resumes or the turn ends.

**Architecture:** The ordered relay (`relayVoiceSections`) becomes a single select loop that, while blocked waiting for the next real chunk (within a leg) or the next leg, emits small zeroed-PCM `audio_delta` frames at the output rate. Silence reuses the existing `audio_delta` wire frame and the `publishAudio` path — no frontend, wire, schema or agent change. The relay learns the rate from the last real chunk, falling back to the run's cached output byte-rate (resolved once at run start) for a gap before any audio.

**Tech Stack:** Go (gateway `internal/gateway`), existing voice run machinery; mock-backed Go tests.

**Spec:** docs/superpowers/specs/2026-10-29-voice-chat-empty-pcm-design.md

## Global Constraints

- Backend only (`internal/gateway`). **No** frontend, wire-protocol, schema/migration or `ServerAgent` change. **No** new operator knob (always-on).
- Silence must **never** be stored (never reach the `keep`/commit path), **never** bill, and **never** prevent the turn from ending.
- Document order, the single monotonic `seq` stream, the commit path and the LLM/ASR legs are **unchanged**.
- Fail-open: when no output rate is known, emit no silence (exactly today's behavior); never crash, never busy-spin.
- The relay must keep its **full-drain invariant**: it never returns early while a leg could still send to its buffered `audioCh` (an early return would block the leg goroutine and deadlock `joinVoiceLegs`). Termination is by channel close only — do **not** add a `ctx` early-return to the relay.
- Silence must **not** interleave with flowing real audio: a silence frame is emitted only after a genuine quiet gap (the cadence timer is reset on every real chunk), never between two real chunks that are arriving steadily.
- Repo-facing text (code, docs, commits) in English. Gates: golangci-lint fmt+run (gofumpt/gocritic) for gateway **and** server-agent; `go test ./...` both modules; the voice suite under `-race`; `make lint-docs`.

## Review Focus

- **Permanent leg failure after silence.** A leg whose `audioCh` closes with **no** audio (retries spent) after the relay already emitted silence → the relay advances, does not hang, and the emitted silence stays correctly counted. (Task 2.)
- **First section retrying before any audio, rate known.** `lastRate == 0` but `fallbackRate > 0` → the inner wait emits silence at `fallbackRate` (not skipped). (Task 2.)
- **No rate at all.** Unbenchmarked mapping: `lastRate == 0` and `fallbackRate == 0` → zero silence, no busy-spin, no crash; the turn behaves exactly as today. (Task 2.)
- **Steady real audio, no gap.** A leg streaming chunks faster than the cadence → **no** silence interleaved (timer reset on each real chunk). (Task 2.)
- **Turn end races the timer / no trailing silence.** After the last real chunk and `legsCh` close, no silence is emitted; the turn drains and ends, and the committed sections contain only real audio. (Task 3.)

---

### Task 1: Silence PCM helper + run output-rate resolver

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (add helper + consts)
- Modify: `gateway/backend/internal/gateway/chat_runs_audio.go` *or* `chat_runs_voice.go` (add `voiceRunOutputRate`)
- Test: `gateway/backend/internal/gateway/chat_runs_voice_silence_test.go` (new)

**Interfaces:**
- Produces:
  - `voiceSilencePCM(sampleRate, ms int) []byte` — zeroed mono s16le PCM of `sampleRate*ms/1000*2` bytes; returns `nil` when `sampleRate <= 0` or `ms <= 0`.
  - `(s *Server) voiceRunOutputRate(ctx context.Context, owner auth.Token, model string) int` — the run's fallback output **sample** rate in Hz, or `0` when unknown. Reuse the existing resolution the speech path uses: `portalSpeechStreamRate(ctx, owner, model)` → `(byteRate, ok, _)`; `if !ok { return 0 }`; return `speechStreamSampleRate(byteRate)` (the existing `byteRate/2` helper, `chat_runs_audio_stream.go`). Swallow the error as `0` (fail-open).
  - Consts (package `gateway`, near the existing `voiceTTSRetryBackoff*`): `voiceSilenceFrameMs = 200` (silence frame duration) and `voiceSilenceCadence = 150 * time.Millisecond` (frame > cadence so the player stays continuously fed; both well under the client's `PLAYING_GAP_MS = 1200`).
- Consumes: the existing `portalSpeechStreamRate` and `speechStreamSampleRate`.

- [ ] **Step 1: Write the failing tests** in `chat_runs_voice_silence_test.go`:
  - `TestVoiceSilencePCMSizeAndZeroed`: `voiceSilencePCM(24000, 200)` returns `24000*200/1000*2 = 9600` bytes, all zero.
  - `TestVoiceSilencePCMNonPositive`: `voiceSilencePCM(0, 200)` and `voiceSilencePCM(24000, 0)` and `voiceSilencePCM(-1, 200)` each return `nil`.
  - `TestVoiceRunOutputRateFromCache`: with a `Server` whose route cache returns an agreed output byte-rate for `model`, `voiceRunOutputRate` returns that byte-rate / 2; when the cache has no agreed rate (or errors), it returns `0`. (Use the same mock/store seam the existing speech-rate tests use; if that seam is heavy, assert `voiceRunOutputRate` returns `0` for an unbenchmarked model and leave the positive case to the existing `portalSpeechStreamRate` tests, noting so.)

- [ ] **Step 2: Run the tests to confirm they fail** (`go test ./internal/gateway/ -run 'VoiceSilencePCM|VoiceRunOutputRate' -v`). Expected: FAIL (undefined symbols).

- [ ] **Step 3: Implement** `voiceSilencePCM`, `voiceRunOutputRate` and the consts as specified in Interfaces.

- [ ] **Step 4: Run the tests to confirm they pass.** Expected: PASS.

- [ ] **Step 5: Commit** (`feat(voice-chat): add silence-PCM helper and run output-rate resolver (#339)`).

---

### Task 2: Restructure the ordered relay to emit keepalive silence

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`relayVoiceSections` signature + body; the call site in `executeVoiceRun`)
- Test: `gateway/backend/internal/gateway/chat_runs_voice_silence_test.go` (extend)

**Interfaces:**
- Consumes: `voiceSilencePCM`, `voiceRunOutputRate`, the consts (Task 1); `run.publishAudio(runAudio)`; `voiceSpeechLeg{section, audioCh}`.
- Produces (changed signature): `relayVoiceSections(run *ChatRun, legsCh <-chan *voiceSpeechLeg, fallbackRate int)`.
- Call site (`executeVoiceRun`, ~`chat_runs_voice.go:154-158`): compute `fallbackRate := s.voiceRunOutputRate(ctx, owner, vp.tts.Model)` before launching the relay goroutine, and pass it: `go func() { defer relayWG.Done(); relayVoiceSections(run, legsCh, fallbackRate) }()`.

- [ ] **Step 1: Write the failing relay tests** (drive `relayVoiceSections` directly with hand-built `legsCh`/`voiceSpeechLeg`s and a fake/recording `run.publishAudio`; a silence frame is an `audio_delta` whose `Data` is all-zero). Cover the Review Focus items:
  - `TestRelaySilenceDuringInnerGap`: one leg whose `audioCh` delivers its first real chunk only after > cadence; assert at least one all-zero frame is published before the real chunk, at `fallbackRate` (leg’s first chunk sets `lastRate` afterwards), in `seq` order.
  - `TestRelaySilenceBetweenSections`: leg0 streams one real chunk (rate R), leg1 delays its first chunk; assert silence at rate `R` (last-seen) fills the gap before leg1’s real chunk.
  - `TestRelayNoRateNoSilence`: `fallbackRate == 0` and the leg never streams before closing (immediate close) → **no** silence frames, relay returns.
  - `TestRelayNoSilenceWhenAudioSteady`: a leg streaming chunks faster than the cadence → **no** all-zero frame interleaved among the real chunks.
  - `TestRelayNoTrailingSilence`: after the last leg’s `audioCh` closes and `legsCh` closes, no silence is published after the final real chunk; relay returns.
  - `TestRelayPermanentFailureAfterSilence`: a leg that emits silence during a delay then closes `audioCh` with no real audio → relay advances to the next leg and ultimately returns; no deadlock.
  - `TestRelayNoPreFirstAudioSilenceOnOuterWait`: `legsCh` delivers its first leg only after > cadence, `lastRate == 0` → **no** silence during the outer (pre-first-leg) wait (thinking gap unchanged).

- [ ] **Step 2: Run the tests to confirm they fail** (`go test ./internal/gateway/ -run 'Relay(Silence|NoRate|NoSilence|NoTrailing|Permanent|NoPreFirst)' -v`). Expected: FAIL.

- [ ] **Step 3: Implement the select-loop relay.** Replace the nested range with a single loop that tracks a `cur *voiceSpeechLeg` (nil between legs) and a reset-on-real-audio timer. Algorithm (the tests do not fully determine it):

```go
func relayVoiceSections(run *ChatRun, legsCh <-chan *voiceSpeechLeg, fallbackRate int) {
    seq, lastRate := 0, 0
    var cur *voiceSpeechLeg
    timer := time.NewTimer(voiceSilenceCadence)
    defer func() { if !timer.Stop() { select { case <-timer.C: default: } } }()
    reset := func() { if !timer.Stop() { select { case <-timer.C: default: } }; timer.Reset(voiceSilenceCadence) }
    emit := func(rate, section int) {
        if rate <= 0 { return }
        data := voiceSilencePCM(rate, voiceSilenceFrameMs)
        if data == nil { return }
        run.publishAudio(runAudio{Seq: seq, Section: section, Rate: rate, Data: data})
        seq++
    }
    for {
        if cur == nil { // between legs: silence only after audio has started (no pre-first-audio fill)
            select {
            case leg, ok := <-legsCh:
                if !ok { return }      // all legs done — no trailing silence
                cur = leg; reset()
            case <-timer.C:
                emit(lastRate, 0); timer.Reset(voiceSilenceCadence) // lastRate==0 => no-op
            }
        } else { // draining cur: silence at lastRate, else fallbackRate
            select {
            case a, ok := <-cur.audioCh:
                if !ok { cur = nil; reset(); continue } // leg done — advance
                a.Seq = seq; a.Section = cur.section; seq++
                lastRate = a.Rate
                run.publishAudio(a); reset()
            case <-timer.C:
                rate := lastRate; if rate == 0 { rate = fallbackRate }
                emit(rate, cur.section); timer.Reset(voiceSilenceCadence)
            }
        }
    }
}
```
  Keep the existing doc comment’s intent (one monotonic `seq`, document order) and extend it to describe the silence fill. Do **not** add a `ctx` parameter (see Global Constraints: full-drain invariant).

- [ ] **Step 4: Run the tests to confirm they pass**; also run the touched file’s existing relay tests to confirm no regression. Expected: PASS.

- [ ] **Step 5: Commit** (`feat(voice-chat): fill relay gaps with keepalive silence (#339)`).

---

### Task 3: End-to-end voice-run tests (not stored, drain floor, turn ends)

**Files:**
- Test: `gateway/backend/internal/gateway/chat_runs_voice_silence_test.go` (extend) or the existing voice-run integration test file (match where `executeVoiceRun` is already exercised).

**Interfaces:**
- Consumes: the full `executeVoiceRun` path with mock chat-completions + mock `/v1/audio/speech` (reuse the harness the existing voice-run tests use).

- [ ] **Step 1: Write the failing integration tests:**
  - `TestVoiceRunGapSilenceNotStored`: a run where one section’s speech leg delays its first chunk (forcing silence) then streams real audio; assert the committed assistant turn’s `spoken_sections` / audio parts contain **only** the real audio (no all-zero silence part) and the turn commits `completed`.
  - `TestVoiceRunGapTurnStillEnds`: the same run reaches its single terminal commit (does not hang); the relay goroutine joins (`relayWG.Wait()` returns).
  - `TestVoiceRunSilenceCountedButNoDoubleBill`: assert the speech usage/billing for the section is unchanged by the injected silence (silence is not a usage event). (Assert via the existing billing/usage seam these tests already use; if none is reachable here, assert no extra `spoken_sections` audio and note billing is covered by the commit-independence in Task 2.)

- [ ] **Step 2: Run to confirm they fail** (symbols/behaviour). Expected: FAIL (or RED on the assertion).

- [ ] **Step 3: No production code expected** — these pin behavior already implemented in Task 2. If a test reveals a real defect, fix it in `chat_runs_voice.go` (not here) and note it in the ledger.

- [ ] **Step 4: Run to confirm they pass.** Expected: PASS. Then run the **voice suite under `-race`** scoped (`go test -race -run '(Voice|Speak|Speech|Stream|Transcription|WAV|Relay)' ./internal/gateway/... -timeout 15m`).

- [ ] **Step 5: Commit** (`test(voice-chat): pin gap-silence is unstored and turn still ends (#339)`).

---

### Task 4: Documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-080: the empty-PCM residual is now **resolved**; describe the relay’s silence fill — reuse `audio_delta`, rate = last-seen else cached run rate else none, no pre-first-audio fill, always-on, accounting-consistent, never stored).
- Modify: `docs/architecture/06-runtime-view.md` (the §6.1.6 "Bounded and retried speech legs" passage that currently says "the retry gap is not filled with silence" → it now **is**, for both the retry gap and the inter-section gap).
- Modify: `docs/architecture/11-risks-and-technical-debt.md` **only if** a current row claims the gap is unfilled (update it); otherwise skip.
- No `config-env.md` change (no new knob). Confirm there is no stale "empty-PCM deferred" claim left anywhere (grep).

**Interfaces:** none (docs).

- [ ] **Step 1: Amend ADR-080** — move the "no empty PCM during a retry" residual from the deferred list to resolved (#339), with the design summary above; keep the two genuinely-still-deferred residuals (first-chunk-deadline timeout; 2xx stream dying before a delta).

- [ ] **Step 2: Update 06-runtime-view** §6.1.6 to state the gap is filled with keepalive silence (both gap kinds), reusing `audio_delta`, with the no-rate fail-open.

- [ ] **Step 3: Grep for stale claims** (`grep -rniE "empty.?pcm|not filled with silence|retry gap" docs/`) and reconcile any remaining "not filled" / "deferred" wording.

- [ ] **Step 4: Run `make lint-docs`.** Expected: OK (branch-local allowlist aside).

- [ ] **Step 5: Commit** (`docs(voice-chat): empty-PCM gap silence resolved (#339)`).

---

## Self-Review

- **Spec coverage:** Goals 1-3 → Task 2 (fill both gaps, reuse `audio_delta`, backend-only) + Task 1 (rate helpers); "never stored / never bill / turn ends" → Task 3; docs → Task 4. Non-goals honored: no new frame type, no pre-first-audio fill (outer wait uses `lastRate` only), no knob, no change to seq/commit/legs.
- **Type consistency:** `voiceSilencePCM(sampleRate, ms int) []byte`, `voiceRunOutputRate(ctx, owner, model) int`, `relayVoiceSections(run, legsCh, fallbackRate int)` used identically across Tasks 1-2. `runAudio{Seq, Section, Rate, Data}` matches the existing struct.
- **Review Focus → tests:** each of the five lines has an owning test in Task 2 (permanent-failure, first-section-fallback, no-rate, steady-audio-no-interleave) or Task 3 (turn-end/no-trailing/not-stored).
- **Proportion:** one new helper + a relay rewrite + tests + a docs pass; the one code block is the relay algorithm the signature/tests do not fully pin. No `ServerAgent` bump, no schema, no frontend.
- **Full-drain invariant:** called out in Global Constraints and enforced by "no `ctx` early-return"; termination is channel-close only, preserving the no-leg-deadlock property.

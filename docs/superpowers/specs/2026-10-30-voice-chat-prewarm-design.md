# Voice Chat: pre-warm the TTS on session open — design (#342)

## Intent & why

Opening a Voice Chat session pays a one-time cold-start on the first turn. Measured
live (2026-10-30): the session's co-load gate already loads all three models
(ASR/LLM/TTS) at ticket-mint, but "loaded" ≠ "warm" — the first **TTS** synth still
pays a graph build (~3–3.5 s for the upcoming default Qwen3-TTS) **plus a per-voice
reference encoding (~8.4 s for a cold preset voice)**. ASR (~0.1–0.5 s) and LLM
(~0.5 s) first-inference-after-load are **not** perceptible, and the co-load already
loads them (the LLM `/load` even generates a token). So the perceptible first-turn
lag is the **TTS first-synth warmup**, dominated by the chosen voice's reference
encoding.

Pre-Warm pays that TTS warmup **before** the user's first turn by firing a priming
synth for the session's chosen voice as soon as the models are confirmed resident.

## Goals

- On session open, pay the TTS graph build + **chosen-voice** reference encoding up
  front, so the first spoken reply is in the warm (~40 ms) regime, not the cold
  (~3–11 s) one.
- **Best-effort and non-blocking:** never delay or fail the ticket or the session.
- **Never bill, never store.**
- Backend only.

## Non-goals / anti-goals

- **No ASR / LLM priming** — their first-inference-after-load is not perceptible and
  the co-load already loads them.
- **No priming of all preset voices upfront** — rejected: ~8.4 s × N serial for
  voices that may never be used, only one TTS loaded at a time.
- **No mid-session voice-switch priming** — the architecture has no voice switch:
  the voice is sealed in the ticket, and a voice change is a *new* ticket (a new
  session), which re-primes at mint. (Refinement from recon vs. the initial scope —
  the "prime on switch" idea is moot.)
- **Not** the LLM slow-load (~10 s warm-cache / ≥2 min cold-disk + the 2-min
  stream-idle timeout) — a separate follow-up issue.
- **Anti-goal:** priming must never bill, store, block, delay or fail the
  ticket/session.

## Constraints (from the code; anchors)

- **Co-load runs at TICKET-MINT (HTTP), not WS open.** `voiceCoLoadGate`
  (`voice_chat.go:155`) is called from `handlePortalVoiceChatTicket`
  (`voice_chat_ticket_endpoint.go:197`); it ensures-loaded + verifies co-residency
  and returns the resolved `asr/llm/tts` targets. It does **not** synth-warm the TTS.
  By WS open the models are already resident (the ticket's existence is the
  ready signal).
- **The seam:** right after the gate succeeds and the voice is resolved
  (`withCloneDefaultVoice`, `voice_chat_ticket_endpoint.go:203`), before/around
  `Mint` (`:201-205`). The TTS target is the gate's `tts`; the voice is `audio.Voice`.
- **The loopback `/v1/audio/speech` path BILLS.** `speechTurnAttempt`
  (`chat_runs_audio.go:231`) POSTs the loopback endpoint, which records a usage row
  — **not usable for priming**.
- **The no-bill / no-store pattern is the speech BENCHMARK.** `benchmark_speech.go`
  dispatches a synth **directly** to the upstream via
  `provider.NativeProxyClient.ProxyNative` (`benchmark_speech.go:413-427`), body
  `{Model, Input, Voice}` — **no usage row, no storage**. The resolved `tts` target
  carries the sealed upstream endpoint/credential. A configured preset voice
  synthesizes with just `Voice: <preset>` (the server resolves the reference).
- **Storage is only at commit** (`finalizeVoiceCommit`/`commitVoiceTurn`); a synth
  that never commits stores nothing.
- **Voice is sealed at ticket time;** the WS up-controls (`parseVoiceUpControl`,
  `voice_chat_ws.go:342`) carry no voice field → no mid-session switch.

## The design (decided)

1. **Fire the priming synth at ticket-mint, after the gate, asynchronously.** In
   `handlePortalVoiceChatTicket`, once `voiceCoLoadGate` returns success and the
   voice is resolved, fire a priming TTS synth in a **detached goroutine** with a
   **background context + timeout** (NOT the request ctx — that ends when the ticket
   response returns). The ticket is minted and returned immediately; priming runs
   concurrently with the WS open and the user getting ready, so it maximally
   overlaps the dead time before the first turn.
2. **Use the benchmark's direct dispatch, not the loopback.** The priming synth goes
   through `provider.NativeProxyClient.ProxyNative` with `{Model: tts.Model, Input:
   <short text>, Voice: audio.Voice}` — so it **neither bills nor stores**. Extract a
   small shared helper from `benchmark_speech.go` if it is clean to do so (e.g.
   `directSpeechSynth` / `primeSpeechVoice`); otherwise mirror its dispatch.
3. **Best-effort.** A priming failure (upstream error, timeout, provider type
   mismatch) is **logged and ignored** — it never blocks or fails the ticket or the
   session. The first turn then simply pays the cold start as it does today.
4. **Scope: the chosen voice only.** ASR and LLM get no priming. No prime-on-switch.
5. **Operator knob + consts.** A new enable/disable knob (default **ON**), e.g.
   `OP_AI_GATEWAY_VOICE_PREWARM`. A short fixed priming text and a priming timeout
   (~30 s, covering a cold Qwen-TTS voice ~8–11 s + margin) as package consts.

## Edge cases

- **Ticket minted, WS never opened** → priming ran for nothing (unbilled, acceptable).
- **Priming races the first turn** (user speaks before priming finishes) → partial
  warmup still helps (shared model/voice process state); best-effort by design.
- **Plain/non-clone TTS, or a voice with no reference** → priming still warms the
  graph; harmless.
- **Priming failure / timeout** → logged, ignored, session unaffected.
- **Knob off** → no priming (today's behavior).

## What's left to the builder

- The exact helper extraction (reuse the benchmark's `ProxyNative` dispatch) and the
  background-ctx + timeout.
- The knob name + consts (priming text, timeout).
- The tests below.

## Verification

- Mock-backed tests: priming fires after the gate for the chosen voice via the
  **direct** (no-bill / no-store) path; it is **async + best-effort** (a priming
  failure does not block or fail the ticket); **knob-off → no priming**; a priming
  run produces **no usage row and no stored audio**.
- Gates: golangci-lint fmt+run (gateway **and** server-agent); `go test ./...` both
  modules; the voice suite under `-race`; `make lint-docs`.
- Not fully live-reproducible without a session; covered by tests plus the live
  measurement that motivated it.

# Voice Chat: pre-warm the TTS on session open — Implementation Plan (#342)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** On Voice Chat session open, fire a best-effort priming TTS synth for the session's chosen voice (as soon as the co-load gate confirms the models resident), so the first spoken reply is warm (~40 ms) instead of cold (~3–11 s). It must never bill, store, block, delay or fail the ticket/session.

**Architecture:** The co-load gate runs at the HTTP ticket-mint endpoint (not WS open), so right after it succeeds the resolved TTS target + chosen voice are known and the models are freshly resident. A detached goroutine (background ctx + timeout) then fires one short synth through the speech **benchmark's direct upstream dispatch** (`provider.NativeProxyClient.ProxyNative`), which neither bills nor stores — unlike the loopback `/v1/audio/speech` path. ASR/LLM are not primed (their first-inference-after-load is not perceptible and the co-load already loads them).

**Tech Stack:** Go (gateway `internal/gateway`, `internal/config`, `cmd/gateway`); mock-backed Go tests.

**Spec:** docs/superpowers/specs/2026-10-30-voice-chat-prewarm-design.md

## Global Constraints

- Backend only (`internal/gateway`, `internal/config`, `cmd/gateway`). **No** frontend, wire-protocol, schema/migration or `ServerAgent` change.
- Priming must **never bill, never store, and never block, delay or fail the ticket or the session.**
- Priming uses the **benchmark's direct dispatch** (`provider.NativeProxyClient.ProxyNative`, see `benchmark_speech.go:413-427`), **NOT** the loopback `/v1/audio/speech` (`speechTurnAttempt`), because the loopback records a usage row.
- Priming runs in a **detached goroutine with a background context + timeout** — never the HTTP request ctx (which is cancelled when the ticket response returns).
- Scope: **the chosen voice only** (`claims`/gate `audio.Voice`). No ASR/LLM priming, no all-presets priming, no mid-session switch (none exists).
- Repo text in English. Gates: golangci-lint fmt+run (gofumpt/gocritic) gateway **and** server-agent; `go test ./...` both modules; the voice suite under `-race`; `make lint-docs`.

## Review Focus

- **Priming slow/hung → the ticket response still returns immediately** (firing is async; the handler never awaits it). (Task 2.)
- **Request-ctx misuse** — priming must use a background ctx + timeout, so it is not cancelled the instant the ticket response returns. (Task 1/2.)
- **No bill / no store** — priming goes through the direct dispatch and never commits; a priming run writes no usage row and no stored audio. (Task 1 + Task 2.)
- **Priming failure (upstream error / timeout) → the ticket and session are unaffected** (best-effort, logged). (Task 2.)
- **Knob off → no priming fired.** (Task 2.)

---

### Task 1: Priming-synth helper + config knob + consts

**Files:**
- Create or modify: `gateway/backend/internal/gateway/` — the priming helper (extract from or mirror `benchmark_speech.go`'s direct dispatch).
- Modify: `gateway/backend/internal/config/config.go` (the knob).
- Modify: `gateway/backend/cmd/gateway/` (wire the knob onto the Server, next to the other voice config).
- Test: `gateway/backend/internal/gateway/*_test.go` (new/extended).

**Interfaces:**
- Produces: `(s *Server) primeSpeechVoice(ctx context.Context, tts routing.Target, voice string) error` — fires ONE short synth for `tts` + `voice` **directly to the upstream** via `provider.NativeProxyClient.ProxyNative` (body `{Model: tts.Model, Input: voicePrimingText, Voice: voice}`, like `speechBenchmarkRequest` in `benchmark_speech.go`), drains/discards the response, returns any error. It must NOT hit the loopback `/v1/audio/speech` and must NOT record usage or store anything. Prefer extracting a small shared helper used by both the benchmark and this (if clean); else mirror the benchmark's dispatch.
- Produces: `Config.VoicePrewarmEnabled bool` (default **true**) from env `OP_AI_GATEWAY_VOICE_PREWARM`, read following the existing config pattern, and threaded onto the `Server` (next to `voiceTiming`) so the ticket endpoint can read it (e.g. an accessor like `voicePrewarmEnabled() bool`).
- Produces: consts `voicePrimingText` (a short fixed string) and `voicePrimingTimeout` (~`30 * time.Second`).
- Consumes: the provider resolution for a `routing.Target` + `ProxyNative` (from `benchmark_speech.go`); `routing.Target`.

- [ ] **Step 1: Write the failing tests.**
  - `primeSpeechVoice` dispatches via the DIRECT path (a mock/fake `NativeProxyClient`/provider) with body `{Model: tts.Model, Input: voicePrimingText, Voice: <voice>}`, and does NOT call the loopback speech path nor the usage-recording path. Assert the request body + that no usage row is written.
  - `primeSpeechVoice` returns the upstream error when the direct dispatch fails.
  - The knob: `OP_AI_GATEWAY_VOICE_PREWARM` unset → `VoicePrewarmEnabled` true; `"false"`/`"0"` → false (match the repo's bool-env convention).

- [ ] **Step 2: Run the tests to confirm they fail** (`go test ./internal/gateway/ -run 'PrimeSpeechVoice|VoicePrewarm' -v`; config test in `internal/config`). Expected: FAIL (undefined).

- [ ] **Step 3: Implement** the helper, the knob (config.go + cmd/gateway wiring + the Server accessor), and the consts, per Interfaces. Read `benchmark_speech.go` for the exact `ProxyNative` dispatch (provider resolve, `upstreamPath`, content type) and reuse/mirror it.

- [ ] **Step 4: Run the tests to confirm they pass.** Also `golangci-lint fmt --diff` + `run ./...` (gateway) and `go test ./internal/config/...`.

- [ ] **Step 5: Commit** (`feat(voice-chat): add a no-bill TTS voice-priming helper + prewarm knob (#342)`).

---

### Task 2: Fire priming at ticket-mint (async, best-effort, gated)

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ticket_endpoint.go` (`handlePortalVoiceChatTicket`).
- Test: the ticket-endpoint test file (extend).

**Interfaces:**
- Consumes: `primeSpeechVoice`, `voicePrewarmEnabled()`, `voicePrimingTimeout` (Task 1); the gate result `tts` and the resolved `audio` (incl. `audio.Voice`) at `voice_chat_ticket_endpoint.go:201-205`.
- Behavior: after `voiceCoLoadGate` returns success and the voice is resolved (`withCloneDefaultVoice`), and the `code != ""` refusal check has passed, **if** `voicePrewarmEnabled()` and `audio.Voice != ""` and the target streams, launch a **detached goroutine**:
  ```go
  go func() {
      ctx, cancel := context.WithTimeout(context.Background(), voicePrimingTimeout)
      defer cancel()
      if err := s.primeSpeechVoice(ctx, tts, audio.Voice); err != nil {
          log.Printf("voice prewarm: priming synth failed (model %q voice %q): %v", tts.Model, audio.Voice, err)
      }
  }()
  ```
  The handler then mints and returns the ticket **without awaiting** the goroutine. Priming never affects the ticket's success/latency.

- [ ] **Step 1: Write the failing tests.**
  - After a successful gate, priming is invoked for the resolved `tts` + chosen voice (assert via a seam — e.g. a test hook/spy on `primeSpeechVoice`, or a fake provider that records the dispatch).
  - **Async / non-blocking:** make the priming path block on a test-controlled signal; assert the ticket handler returns its 200 + ticket **before** priming completes (then release).
  - **Best-effort:** a priming failure does NOT change the ticket response (still 200 + valid ticket).
  - **Knob off** (`VoicePrewarmEnabled=false`) → priming is NOT invoked.
  - **No bill / no store:** a priming run writes no usage row and no stored audio (assert against the usage/commit seams).

- [ ] **Step 2: Run the tests to confirm they fail.**

- [ ] **Step 3: Implement** the async, gated, best-effort firing at the seam, per Interfaces.

- [ ] **Step 4: Run the tests to confirm they pass**; then the voice suite under `-race` scoped (`go test -race -run '(Voice|Speak|Speech|Stream|Prewarm|Prime|Ticket)' ./internal/gateway/... -timeout 15m`) and golangci-lint.

- [ ] **Step 5: Commit** (`feat(voice-chat): prime the TTS voice at ticket-mint, async and best-effort (#342)`).

---

### Task 3: Documentation

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (a new ADR for session-open TTS pre-warm).
- Modify: `docs/architecture/06-runtime-view.md` (the voice-chat session-open / ticket section).
- Modify: `docs/architecture/reference/config-env.md` (the new `OP_AI_GATEWAY_VOICE_PREWARM` knob).

- [ ] **Step 1: New ADR** — session-open TTS pre-warm: the measured motivation (TTS first-synth warmup ~3–3.5 s + ~8.4 s per cold voice; ASR/LLM not perceptible), the design (async best-effort priming synth at ticket-mint, chosen voice only, via the benchmark's direct no-bill/no-store dispatch), the knob, and the residuals/non-goals (no ASR/LLM priming; no all-presets; no mid-session switch — none exists; the LLM slow-load is a separate follow-up).

- [ ] **Step 2: 06-runtime-view** — note that after the co-load gate at ticket-mint the gateway fires a best-effort priming synth for the chosen voice (async, no bill/store, knob-gated).

- [ ] **Step 3: config-env.md** — document `OP_AI_GATEWAY_VOICE_PREWARM` (bool, default on).

- [ ] **Step 4: `make lint-docs`** clean (branch-local allowlist aside).

- [ ] **Step 5: Commit** (`docs(voice-chat): document session-open TTS pre-warm (#342)`).

---

## Self-Review

- **Spec coverage:** priming helper + knob + consts → Task 1; async best-effort firing at the ticket-mint seam → Task 2; docs → Task 3. Non-goals honored: no ASR/LLM priming, no all-presets, no mid-session switch, no loopback (no bill), no store.
- **Type consistency:** `primeSpeechVoice(ctx, tts routing.Target, voice string) error`, `voicePrewarmEnabled() bool`, `Config.VoicePrewarmEnabled`, consts `voicePrimingText`/`voicePrimingTimeout` used identically across Tasks 1–2.
- **Review Focus → tests:** async-non-blocking, best-effort-failure, knob-off, no-bill/no-store, request-ctx-not-used each have an owning test in Task 1 or 2.
- **Proportion:** one small helper + one async call site + a knob + docs; the one code block is the async firing the tests pin. No ServerAgent bump, no schema, no frontend.
- **Billing/storage safety:** called out in Global Constraints and enforced by using the benchmark's direct `ProxyNative` dispatch (never the billing loopback) and never committing (no storage).

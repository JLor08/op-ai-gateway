# Voice Chat Mode — Phase 1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use
> superpowers:subagent-driven-development (recommended) or
> superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the Voice Chat orchestration MVP — a backend WebSocket that runs
a hands-free ASR → LLM → TTS loop and speaks the whole answer, plus the minimal
portal UI to drive it.

**Architecture:** One new portal WebSocket (`/api/portal/voice-chat`), minted by
a CSRF-protected ticket endpoint that runs a co-load admission gate. The WS runs
a server-side state machine (idle → listening → thinking → speaking → draining)
that composes three existing `/v1` loopback cores: a **per-turn** live-ASR relay,
a new **voice run kind** (text LLM → streamed TTS, persisted as one assistant
turn carrying text + audio), and the streaming-speech relay. Half-duplex mic
gating is enforced server-side (the loop guarantee). The frontend reuses the
live-ASR capture worklet and the streaming PCM player.

**Tech Stack:** Go (`gateway/backend`, `internal/gateway` + `internal/portal`),
coder/websocket, AES-256-GCM tickets; React/TS/Vite portal (`gateway/frontend`).

**Spec:** `docs/superpowers/specs/2026-10-24-voice-chat-design.md` (read it; this
plan argues from it).

## Global Constraints

- Feature branch + PR only; never commit/merge to `main`. Branch:
  `claude/voice-chat-mode-9cf8da` (already checked out).
- Repo-facing text (code, comments, commits, PR, docs) in **English**; the LAN
  host name of the test audio.cpp server must never appear in repo text.
- Serial-roadmap slots (verified on `main` 2026-10-24): **ADR-078**; ServerAgent
  **0.13.0** — **no bump expected** (phase 1 changes no agent wire contract; if
  one sneaks in, bump once off `0.13.0`); highest migration **v95** — **no
  migration expected** (new state rides the schema-free chat document).
- Auth: the ticket-mint POST uses `requireWebScope(scopeGatewayUse)` (session
  cookie + `X-OP-CSRF`); the WS upgrade is authenticated by the single-use
  encrypted ticket (redeemed before `websocket.Accept`) + the coder/websocket
  same-origin check. Loopback calls use the trusted-loopback header pair
  (`internalAuthHeaderName` + `internalUserHeaderName`, `X-OP-CSRF: 1`,
  `s.selfBaseURL`), exactly as the live-ASR relay and chat runs do.
- Compose existing `/v1` cores — do not re-implement admission, billing, or the
  streaming relays. Each core already spends admission once, holds its per-route
  guard, writes its billing row, and applies its verdict gates.
- i18n: every new key added to **both** `de` and `en` in
  `gateway/frontend/src/i18n.ts` (type-checked parity).
- Gates before pushing: per-module `golangci-lint` fmt+run; frontend prettier
  `format:check` + `npm run build` + vitest; `make sonar-gate` +
  `make sonar-findings` + `make sonar-branch-findings` (judge by branch
  findings); Postgres leg (`OP_AI_GATEWAY_TEST_POSTGRES_DSN`) for any store
  touch. No store schema change is planned; if one appears, add the Postgres leg
  and ADR-005 (wide types) check.
- Remove `docs/superpowers/**` and `docs/implementation-status.md` from the
  branch as the last step before the PR (their durable content must already be
  in `docs/architecture/`).

## Review Focus

Input classes/failure modes the spec implies but that a task's happy-path tests
may not exercise — each gets its pinning test added to the owning task:

- **Mic frames during `speaking`/`draining` are dropped before reaching ASR** —
  the loop guarantee. If a client keeps streaming PCM while the answer plays, not
  one byte reaches the ASR loopback. *(Task 6.)*
- **A leg failing mid-conversation** (ASR drop, LLM 5xx, TTS error, or a
  mid-turn `runtime.admission_blocked`) → one typed `error` frame with the
  core's stable code, then back to `idle` with the mic re-enabled; never a silent
  retry. *(Task 7.)*
- **Co-load gate refuses honestly** — if the three cannot be resident together
  (a leg unavailable, or ensuring the third evicted the first), the ticket is
  refused with a leg-specific code; the mode is never released into a
  first-turn failure. *(Task 2.)*
- **Ticket misuse** — an expired, replayed, tampered, or wrong-origin ticket
  never upgrades the WS. *(Task 1 for expiry/replay/tamper; Task 5 for the
  pre-Accept redeem + origin.)*
- **Turn finalization under both triggers** — a silence gap *and* an explicit
  `push_end` each finalize exactly one chunk; a sub-`MinUtteranceMs` blip does
  not. *(Task 6.)*
- **Pause-tolerant turns** — resumed speech during the continuation window
  cancels the speculative LLM run (emitting `answer_reset`), persists **nothing**,
  extends `pendingText`, and the combined text is (re-)sent on the next end; the
  window closing with no resume commits exactly one turn. *(Tasks 4 + 7.)*

---

## Task 1: Voice ticket store + claims

**Files:**
- Create: `gateway/backend/internal/gateway/voice_chat_ticket.go`
- Test: `gateway/backend/internal/gateway/voice_chat_ticket_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type voiceTicketClaims struct { ASR, LLM, TTS routing.Target; ChatID, UserID, TokenID, ServiceID, ProjectID string; Audio portal.ChatAudioSettings; SampleRate int; Channels int; SampleFormat string; Exp time.Time; Nonce string }`
  - `type voiceTicketStore struct { ... }`; `func newVoiceTicketStore() *voiceTicketStore`
  - `func (s *voiceTicketStore) Mint(c voiceTicketClaims) (string, error)`
  - `func (s *voiceTicketStore) Redeem(token string) (voiceTicketClaims, error)`
  - const `voiceTicketTTL = 30 * time.Second`

Model on `live_transcription_ticket.go` verbatim (AES-256-GCM, per-process random
key, URL-safe no-pad base64 so the token is a valid `Sec-WebSocket-Protocol`
value, `Mint` stamps fresh Nonce + Exp, `Redeem` checks Exp and enforces
single-use via an in-process redeemed-nonce set). The one difference is three
targets instead of one. Parallel typed store is deliberate (the crypto is tiny
and isolated; a shared generic would refactor the live path — out of scope).

- [ ] **Step 1: Write the failing tests** — `TestVoiceTicketMintRedeemRoundTrips`
  (mint then redeem returns the same three targets + ChatID + Audio),
  `TestVoiceTicketRedeemRejectsExpired` (redeem after `voiceTicketTTL` → error),
  `TestVoiceTicketRedeemIsSingleUse` (second redeem of one token → error),
  `TestVoiceTicketRedeemRejectsTampered` (flip a byte → error).
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestVoiceTicket -v` → FAIL (undefined).
- [ ] **Step 3: Implement** `voice_chat_ticket.go` per the Interfaces block.
- [ ] **Step 4: Run** the same → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add encrypted voice-chat ticket store`.

## Task 2: Co-load admission gate

**Files:**
- Create: `gateway/backend/internal/gateway/voice_chat.go` (gate + shared
  resolve helpers + error-code consts)
- Test: `gateway/backend/internal/gateway/voice_chat_test.go`

**Interfaces:**
- Consumes: `resolveTarget`/`inferenceGate` (inference_resolve.go,
  inference_handlers.go), `RuntimeEnsurer.EnsureRuntimeModel`
  (provider/runtime_ensure.go), the agent runtime report read used by the
  runtime panel (locate the existing "which specs are resident" accessor on
  `s.Routes`/telemetry; the residency check reuses it — do **not** re-implement
  the agent's admission arithmetic).
- Produces:
  - `func (s *Server) voiceCoLoadGate(ctx, token *auth.Token, asrModel, llmModel, ttsModel string) (asr, llm, tts routing.Target, code string, err error)` — returns an empty `code` on success, else one of the consts below.
  - consts: `voiceChatASRUnavailable = "portal.voice_chat_asr_unavailable"`, `voiceChatLLMUnavailable = "portal.voice_chat_llm_unavailable"`, `voiceChatTTSUnavailable = "portal.voice_chat_tts_unavailable"`, `voiceChatCoLoadFailed = "portal.voice_chat_coload_failed"`, `voiceChatTicketFailed = "portal.voice_chat_ticket_failed"`.

Gate algorithm (spec §6.4): (1) resolve+gate each model for its shape (ASR →
`["transcription"]` + streaming verdict, LLM → chat shape, TTS → `["speech"]` +
speech-streaming/offerable verdict); a failure returns the leg's `*Unavailable`
code. (2) For agent-managed legs, `EnsureRuntimeModel`; `runtime.admission_blocked`
→ `voiceChatCoLoadFailed`. (3) Read the runtime report; if not all agent-managed
legs are resident together → `voiceChatCoLoadFailed`. Non-agent legs skip 2–3.

- [ ] **Step 1: Write the failing tests** (mock the resolver + a fake
  `RuntimeEnsurer` + a fake residency reader): `TestVoiceCoLoadGatePassesWhenAllResident`,
  `TestVoiceCoLoadGateRefusesUnroutableLeg` (one model unresolvable → its
  `*Unavailable` code), `TestVoiceCoLoadGateRefusesWhenEnsureBlocked`
  (`admission_blocked` → `coload_failed`), `TestVoiceCoLoadGateRefusesWhenNotCoResident`
  (ensure "succeeds" for all but residency read shows a leg missing →
  `coload_failed`).
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestVoiceCoLoadGate -v` → FAIL.
- [ ] **Step 3: Implement** `voiceCoLoadGate` + the consts.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add voice-chat co-load admission gate`.

## Task 3: Ticket endpoint

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat.go` (add the handler)
- Modify: `gateway/backend/internal/gateway/server.go` (register
  `/api/portal/voice-chat-ticket`; add `s.voiceTickets *voiceTicketStore` field +
  `newVoiceTicketStore()` in the constructor, beside `s.liveTickets`)
- Test: `gateway/backend/internal/gateway/voice_chat_test.go`

**Interfaces:**
- Consumes: Task 1 (`voiceTicketStore`), Task 2 (`voiceCoLoadGate`).
- Produces: `func (s *Server) handlePortalVoiceChatTicket(w, r)` — POST,
  `requireWebScope(scopeGatewayUse)`. Body: `{chat_id, asr_model, llm_model,
  tts_model}`. On gate pass → `s.voiceTickets.Mint(...)` → `200 {"ticket":"..."}`;
  on gate refusal → `apierror.Response(code, msg, "")` with the gate's code (400
  for `*Unavailable`, 409 for `coload_failed`). Blank model → 400.

- [ ] **Step 1: Write the failing tests** (httptest against the mux):
  `TestVoiceTicketEndpointMintsOnGatePass`, `TestVoiceTicketEndpointRefusesLeg`
  (gate returns a leg code → that HTTP status + body code),
  `TestVoiceTicketEndpointRequiresCSRF` (cookie session without `X-OP-CSRF` →
  403), `TestVoiceTicketEndpointRejectsServiceToken`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestVoiceTicketEndpoint -v` → FAIL.
- [ ] **Step 3: Implement** the handler + wire the route + the store field.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add voice-chat ticket endpoint`.

## Task 4: Voice run kind (speculative text LLM → streamed TTS → one deferred-commit assistant turn)

**Files:**
- Create: `gateway/backend/internal/gateway/chat_runs_voice.go`
- Modify: `gateway/backend/internal/gateway/chat_runs.go` (add the `voice` kind
  branch in `executeRun`; a constructor the orchestrator calls with three
  resolved targets + the `pendingText` as the user message)
- Test: `gateway/backend/internal/gateway/chat_runs_voice_test.go`

**Interfaces:**
- Consumes: `reserveRun`/`launchRun` (chat_runs.go), `consumeRunStream` (text
  deltas), the streaming-speech consumer (`consumeSpeechStream`/`publishAudio`,
  chat_runs_audio_stream.go), `CommitAssistant` → `SaveChatWithBlobs` and the
  user-turn write helper used by `PrepareChatRun`, `speechBodyFields` (#282
  language injection), `ChatRun.subscribe`.
- Produces:
  - `func (s *Server) startVoiceRun(ctx, owner auth.Token, chatID, pendingText string, asr, llm, tts routing.Target, audio portal.ChatAudioSettings) (*ChatRun, error)` — reserves a run and launches `executeVoiceRun`. **Does not commit the user turn up front** (persistence is deferred so a cancelled speculative run leaves nothing to undo — spec §6.1).
  - `func (s *Server) executeVoiceRun(ctx, owner, run, prep)` — POST the text body to `/v1/chat/completions` (`setRunLoopbackHeaders`), stream deltas (published to subscribers). The run is **cancellable** via its context (the orchestrator cancels it on resumed speech); a cancelled run commits **nothing**. On an un-cancelled finish, POST the assembled answer to `/v1/audio/speech` streaming (`buildSpeechStreamBody` + `speechBodyFields`, voice/language from `audio` + the `tts` target), relay `speech.audio.delta` as published audio, then persist in one commit: the user turn (`pendingText`) **and** the assistant turn (answer text + `AudioStreamed=true`, audio blobs) via `CommitAssistant`/`SaveChatWithBlobs`.

Notes: (spec §9) phase-1 TTS may be a buffered one-shot WAV when the TTS model's
byte-rate isn't cached (`speechRunStreamRate` falls back silently) — acceptable;
low time-to-first-audio is phase 3. The deferred-commit shape is the deliberate
divergence from the standard `PrepareChatRun`-commits-user-first path, required
by the continuation window (spec §5.4, §6.1); LLM-token billing still occurs on
the `/v1/chat/completions` leg even for a cancelled run (tokens were produced) —
only *chat-document persistence* is deferred, not metering.

- [ ] **Step 1: Write the failing tests** (mock `/v1/chat/completions` and
  `/v1/audio/speech` loopback handlers on the test mux):
  `TestVoiceRunCommitsUserAndAssistantOnFinish` (on an un-cancelled finish the
  chat doc gains a user turn with `pendingText` and an assistant turn with the
  answer), `TestVoiceRunCancelledPersistsNothing` (cancel the run context mid-LLM
  → the chat doc is unchanged, no assistant turn, no user turn),
  `TestVoiceRunAttachesAudioBlobToAssistantTurn` (the committed assistant turn
  carries an `audio_url` blob; `SaveChatWithBlobs` got the blob),
  `TestVoiceRunBillsLLMAndTTS` (an LLM-token row and a TTS
  `audio_second`/`audio_bytes` row are written on finish),
  `TestVoiceRunPublishesTextThenAudio` (a subscriber sees text deltas then audio
  deltas).
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestVoiceRun -v` → FAIL.
- [ ] **Step 3: Implement** `startVoiceRun` + `executeVoiceRun` (deferred commit, cancellable) + the kind branch.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add speculative voice run kind with deferred commit`.

## Task 5: Voice WS adapter — upgrade, ticket redeem, single writer, scaffolding

**Files:**
- Create: `gateway/backend/internal/gateway/voice_chat_ws.go`
- Modify: `gateway/backend/internal/gateway/server.go` (register
  `/api/portal/voice-chat`)
- Test: `gateway/backend/internal/gateway/voice_chat_ws_test.go`

**Interfaces:**
- Consumes: Task 1 (`voiceTicketStore.Redeem`), the `liveWriter` discipline +
  `agentStreamPing*` constants (live_transcription_ws.go) — reuse the single
  writer pattern (extract a shared `wsWriter` only if trivial; otherwise mirror).
- Produces:
  - `func (s *Server) handleVoiceChat(w, r)` — extract ticket from
    `Sec-WebSocket-Protocol` → `s.voiceTickets.Redeem` (miss → 401, no upgrade) →
    `websocket.Accept` (default same-origin policy, subprotocol
    `voiceWSSubprotocol = "op-voice-chat"`) → `conn.SetReadLimit(maxLiveFrameBytes)`
    → `relayVoiceChat`.
  - Up/down frame types: `type voiceUpControl struct { Type string }`
    (`push_start|push_end|cancel|bye`); `type voiceDownMsg struct { Type, State,
    Text, MessageID, Audio string; Seq int; Rate int; Code string }` with the
    `type` values from spec §5.2 (`state|asr_partial|asr_final|answer_delta|
    answer_done|audio_delta|audio_done|error`).
  - `func parseVoiceUpControl(data []byte) (voiceUpControl, bool)`.

This task delivers the upgrade + redeem + a writer that can emit a `state` frame
and close cleanly. The state machine body is Task 6.

- [ ] **Step 1: Write the failing tests**: `TestVoiceWSRejectsMissingTicket` (no
  ticket → 401, no upgrade), `TestVoiceWSRejectsRedeemedTicket` (reused ticket →
  401), `TestVoiceWSAcceptsAndSendsInitialState` (valid ticket → upgrade →
  first down-frame is `{type:"state",state:"listening"|"idle"}`),
  `TestParseVoiceUpControl` (valid/invalid control frames).
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run TestVoiceWS -v` → FAIL.
- [ ] **Step 3: Implement** the handler + frame types + writer + route.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add voice-chat WebSocket upgrade + framing`.

## Task 6: State machine — ASR loopback (per turn), turn detection, half-duplex

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go` (add
  `relayVoiceChat` + the state machine)
- Modify: `gateway/backend/internal/gateway/voice_chat.go` (config constants)
- Modify: `gateway/backend/internal/config/*` (the `OP_AI_GATEWAY_VOICE_*` env
  keys; follow the existing config pattern) and
  `docs/architecture/reference/config-env.md` (fold in during Task 11)
- Test: `gateway/backend/internal/gateway/voice_chat_ws_test.go`

**Interfaces:**
- Consumes: `openLiveLoopback` pattern (live_transcription_ws.go) for the
  per-turn ASR relay; Task 5 writer/frames.
- Produces:
  - `relayVoiceChat(r, conn, claims)` — the state machine with a `pendingText`
    buffer: `idle`/`listening` opens a fresh ASR loopback and maps SSE
    `delta`/`partial` → `asr_partial` (continuous, low latency); a silence timer
    or a `push_end` ends the **chunk** (`{"type":"end"}` upstream → the chunk's
    final text, emitted as `asr_final` and **appended** to `pendingText`);
    `speaking`/`draining` **discard inbound binary PCM** (the mic gate) + a
    `MicResumeTailMs` tail; `cancel` → back to `listening`; `bye`/drop → teardown
    once. The continuation window (speculative start + cancel-on-resume) is wired
    in Task 7; this task delivers the chunking, `pendingText` accumulation, mic
    gate, and timers.
  - config constants with defaults: `VoiceSilenceTimeoutMs = 900`,
    `VoiceMinUtteranceMs = 300`, `VoiceMaxUtteranceMs = 30000`,
    `VoiceMicResumeTailMs = 300`, `VoiceContinuationGraceMs = 1500`.

- [ ] **Step 1: Write the failing tests** (drive the machine with a fake ASR
  loopback + a fake WS peer): `TestVoiceChunkEndsOnSilence` (no new partial for
  `SilenceTimeoutMs` → `asr_final`, text appended to `pendingText`),
  `TestVoiceChunkEndsOnPushEnd` (`push_end` ends the chunk immediately),
  `TestVoicePendingTextAccumulatesAcrossChunks` (two chunks → `pendingText` is
  chunk1 + chunk2), `TestVoiceSubMinUtteranceIgnored` (a blip shorter than
  `MinUtteranceMs` does not end a chunk), `TestVoiceMicDroppedWhileSpeaking`
  (binary frames sent while state is `speaking`/`draining` never reach the ASR
  loopback — assert the loopback pipe received zero bytes),
  `TestVoiceByeTearsDownOnce`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run 'TestVoiceChunk|TestVoicePending|TestVoiceSub|TestVoiceMic|TestVoiceBye' -v` → FAIL.
- [ ] **Step 3: Implement** `relayVoiceChat` + `pendingText` + the mic gate + the silence/chunk timers + the config constants.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): add voice-chat state machine with per-turn ASR, chunking and half-duplex gating`.

## Task 7: Wire the voice run into the machine — relay + degradation

**Files:**
- Modify: `gateway/backend/internal/gateway/voice_chat_ws.go`
- Test: `gateway/backend/internal/gateway/voice_chat_ws_test.go`

**Interfaces:**
- Consumes: Task 4 (`startVoiceRun` + `ChatRun.subscribe` + run cancel), Task 6
  (the machine + `pendingText` + `VoiceContinuationGraceMs`).
- Produces: the **continuation window**. On a chunk end the machine starts a
  **speculative** `startVoiceRun` on `pendingText` (ticket's three targets +
  `ChatID` + `Audio`), subscribes, and relays text deltas →
  `answer_delta`/`answer_done`, audio deltas → `audio_delta`/`audio_done`, driving
  `thinking`. While in the window (`thinking`, before TTS) the **mic stays live**:
  - **resumed voiced speech** → cancel the speculative run, emit `answer_reset`,
    append the next chunk to `pendingText`, return to `listening` (re-run on the
    next chunk end);
  - **window closes** (LLM finished **and** `VoiceContinuationGraceMs` elapsed
    with no resume) → the run commits (user + assistant turn), TTS plays
    (`speaking` → `draining` → `listening`), `pendingText` cleared.
  Any leg error (run error, speech error, mid-turn `runtime.admission_blocked`) →
  one `error` down-frame carrying the core's stable code → `idle` with the mic
  re-enabled; no retry.

- [ ] **Step 1: Write the failing tests** (mock loopback cores on the test mux):
  `TestVoiceHappyPathRoundTrips` (binary PCM → `asr_final` → `answer_delta*` →
  `answer_done` → `audio_delta*` → `audio_done` → back to `listening`; the chat
  doc has both turns), `TestVoiceResumeCancelsAndExtends` (resumed speech during
  the window → `answer_reset`, the speculative run is cancelled and persists
  nothing, `pendingText` extends, and the next end re-runs on the combined text,
  which is what finally commits), `TestVoiceCommitsAfterGraceNoResume` (no resume
  → one committed turn after the grace), `TestVoiceLLMErrorDegradesToIdle` (LLM
  5xx → one `error` frame with the stable code → `state:idle`, no retry),
  `TestVoiceTTSErrorDegradesToIdle`, `TestVoiceMidTurnAdmissionBlockedDegrades`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run 'TestVoiceHappyPath|TestVoiceResume|TestVoiceCommits|TestVoiceLLMError|TestVoiceTTSError|TestVoiceMidTurn' -v` → FAIL.
- [ ] **Step 3: Implement** the continuation window (speculative start,
  cancel-on-resume + `answer_reset`, commit-on-grace-close) + the relay +
  degradation.
- [ ] **Step 4: Run** the full `go test ./internal/gateway/ -run TestVoice -v` → PASS; then `make test-go` with the Postgres leg.
- [ ] **Step 5: Commit** `feat(gateway): orchestrate voice turns with a pause-tolerant continuation window and clean degradation`.

## Task 8: Frontend voice session client

**Files:**
- Create: `gateway/frontend/src/components/chat/voiceSession.ts`
- Create: `gateway/frontend/src/api/voiceChat.ts` (ticket fetch)
- Test: `gateway/frontend/src/components/chat/voiceSession.test.ts`

**Interfaces:**
- Consumes: `captureWorklet`/`livePcm` (capture, verbatim), `audioToWav` consts,
  `speechPlayback`/`StreamingPcmPlayer` (playback), the live-ASR client
  (`createLiveSession`, liveTranscription.ts) as the structural template incl.
  the connect-time `pending[]` audio buffer.
- Produces:
  - `voiceChat.ts`: `createVoiceChatTicket({chatId, asrModel, llmModel, ttsModel}): Promise<{ticket}>` → POST `/api/portal/voice-chat-ticket` (CSRF), mirroring `createLiveTranscriptionTicket`.
  - `voiceSession.ts`: `createVoiceSession(args): { pushStart(), pushEnd(), cancel(), stop() }` — builds `AudioContext`+`getUserMedia` inside the gesture, mints the ticket, opens the WS (subprotocol `op-voice-chat` + ticket), streams capture PCM up, routes down-frames: `asr_partial`/`asr_final` → `onTranscript` (the live, low-latency display), `answer_delta`/`answer_done` → `onAnswer`, `answer_reset` → `onAnswerReset` (drop the answer preview), `audio_delta` → `StreamingPcmPlayer.acceptChunk`/`push`, `state` → `onState`, `error` → `onError`. Const `VOICE_WS_SUBPROTOCOL='op-voice-chat'`, `VOICE_WS_PATH='/api/portal/voice-chat'`.

- [ ] **Step 1: Write the failing tests** (fake WebSocket + fake player):
  `parses down-frames and routes asr/answer/audio/state`, `answer_reset fires
  onAnswerReset so the preview clears`, `feeds audio_delta to the player in seq
  order`, `pushEnd sends {type:"push_end"}`, `stop sends {type:"bye"} and tears
  down`.
- [ ] **Step 2: Run** `npx vitest run src/components/chat/voiceSession.test.ts` → FAIL.
- [ ] **Step 3: Implement** `voiceChat.ts` + `voiceSession.ts`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(portal): add voice-chat WebSocket session client`.

## Task 9: Frontend voice-chat hook

**Files:**
- Create: `gateway/frontend/src/components/chat/useVoiceChat.ts`
- Test: `gateway/frontend/src/components/chat/useVoiceChat.test.ts`

**Interfaces:**
- Consumes: Task 8 (`createVoiceSession`), `speechPlayback` (prime in the
  gesture).
- Produces: `useVoiceChat(deps): { state, liveTranscript, answerPreview, start(),
  stop(), pushStart(), pushEnd(), cancel() }` — owns the session lifetime across
  the conversation and surfaces the `state` down-frames. **Display is provisional;
  the backend owns persistence** (it commits the user + assistant turn on the real
  finalize, §6.1): `asr_partial`/`asr_final` feed a live `liveTranscript`
  (pendingText + current partial) shown as a provisional composer/indicator line,
  `answer_delta` feeds a provisional `answerPreview`, `answer_reset` clears it, and
  on turn commit (`audio_done`) the hook **refreshes the chat thread** from the
  backend (reuse the existing chat-doc reload) so the persisted user + assistant
  (text + audio) turns replace the provisional display. It does **not** commit
  turns locally (that would double-persist). Classifies errors; tears down on
  unmount.

- [ ] **Step 1: Write the failing tests**: `start opens a session and reflects
  state`, `asr_partial/asr_final update liveTranscript provisionally without
  committing a message`, `answer_reset clears answerPreview`, `audio_done
  triggers a chat-thread refresh (backend-persisted turns appear)`, `stop tears
  down`, `an error frame surfaces and returns to idle`.
- [ ] **Step 2: Run** `npx vitest run src/components/chat/useVoiceChat.test.ts` → FAIL.
- [ ] **Step 3: Implement** the hook.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(portal): add useVoiceChat hook`.

## Task 10: Portal UI — mode toggle, indicator, capability gate

**Files:**
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx` (voice-mode state;
  the three-model selection; the capability predicate that a thread is
  voice-capable = an LLM-capable model + a transcription-streaming model + an
  offerable streaming TTS; wire `useVoiceChat`)
- Modify: `gateway/frontend/src/components/Chat.tsx` (the Voice Chat toggle + the
  listening indicator + the live transcript line; turns render through the normal
  `ChatMessage` path)
- Create: `gateway/frontend/src/components/VoiceChatControl.tsx` (the toggle +
  listening indicator + live transcript + push-to-talk button)
- Test: `gateway/frontend/src/components/VoiceChatControl.test.tsx`

**Interfaces:**
- Consumes: Task 9 (`useVoiceChat` → `state`, `liveTranscript`, `answerPreview`),
  the existing capability flags (`modelTranscriptionCapable`,
  `modelSpeechCapable`/`modelPresetClone`, LLM availability),
  `modelTranscriptionStreaming`/`modelSpeechStreaming` gates.
- Produces: `VoiceChatControl` component with a **prominent listening indicator**
  that clearly distinguishes **listening** (`state==="listening"`: active mic icon
  + "listening…" + the live transcript) from **not listening** (`thinking` /
  `speaking` / `draining` / `idle`: muted/inactive mic icon + the matching
  status), so the user always sees whether the system hears them (spec §7); a
  `voiceChatCapable` derived flag in ChatStore; the toggle is disabled with the
  gate's reason when the triple is unsatisfiable or the co-load ticket is refused.

- [ ] **Step 1: Write the failing tests**: `toggle is disabled when the model
  triple is not satisfiable`, `enabling calls start`, `the indicator shows a
  listening state when state==="listening" and a not-listening state during
  speaking/thinking/idle`, `the live transcript shows while listening`,
  `push-to-talk button calls pushStart/pushEnd`.
- [ ] **Step 2: Run** `npx vitest run src/components/VoiceChatControl.test.tsx` → FAIL.
- [ ] **Step 3: Implement** the control + ChatStore wiring.
- [ ] **Step 4: Run** → PASS; then `npm run build`.
- [ ] **Step 5: Commit** `feat(portal): add Voice Chat mode toggle and indicator`.

## Task 11: i18n + architecture docs + ADR-078

**Files:**
- Modify: `gateway/frontend/src/i18n.ts` (a `chatVoice*` key cluster in **both**
  `de` and `en`: toggle label, the four states, push-to-talk, and the gate
  refusal messages mirroring the `portal.voice_chat_*` codes)
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-078 — the voice
  orchestration WS, per-turn ASR, the live transcript + speculative LLM start +
  pause-tolerant continuation window with deferred-commit, half-duplex guarantee,
  ensure-then-verify co-load gate, inherited billing, degradation; note no agent
  bump / no migration), `docs/architecture/06-runtime-view.md` (a voice-turn
  sequence incl. the cancel-and-extend path),
  `docs/architecture/reference/api-surface.md` + `reference/openapi.yaml` (the
  two new portal routes + the WS frames), `docs/architecture/reference/data-model.md`
  (any voice-settings field on the chat document), `docs/architecture/reference/config-env.md`
  (the `OP_AI_GATEWAY_VOICE_*` keys), `docs/architecture/11-risks-and-technical-debt.md`
  §11.4 (the honest co-load limit + non-agent-mapping caveat),
  `docs/architecture/cross-cutting/telemetry-usage-observability.md` (the three
  inherited billing rows per voice turn).

**Interfaces:** Consumes: all prior tasks (final names/codes/keys).

- [ ] **Step 1:** Add the `chatVoice*` keys to `de` and `en`; run `npm run build`
  → PASS (parity enforced by tsc). Add any i18n spot-check in `i18n.test.ts`.
- [ ] **Step 2:** Write ADR-078 + fold the behavior into the docs above.
- [ ] **Step 3: Run** `./scripts/check-docs.sh` (with `docs/superpowers/**` moved
  aside, per the known check-6 behavior) → OK; `make lint-docs` → OK.
- [ ] **Step 4: Commit** `docs: ADR-078 voice chat orchestration + i18n`.

## Task 12: Deploy plumbing + whole-branch verification

**Files:**
- Modify: `gateway/deploy/nginx/*` (a WS-upgrade location for
  `/api/portal/voice-chat`, mirroring the live-transcription WS location) and the
  Vite dev proxy (`gateway/frontend/vite.config.*`) for the WS path; k8s
  configmap if the live-transcription route has one.

**Interfaces:** Consumes: Task 5 (the WS path).

- [ ] **Step 1:** Add the nginx WS-upgrade location + Vite ws proxy entry
  mirroring the existing live-transcription entries.
- [ ] **Step 2: Run the full gate suite:** `make test-go` (Postgres leg);
  `cd gateway/frontend && npm run build && npx prettier --check . && npx vitest run`;
  per-module `golangci-lint fmt --diff` + `run`; `make sonar-gate` →
  `make sonar-findings` → `make sonar-branch-findings` (branch findings = 0);
  `./scripts/check-docs.sh`.
- [ ] **Step 3: Live verification** against the test audio.cpp (via the
  logged-in gateway in the in-app browser; host name out of repo text): load a
  streaming ASR + streaming TTS + an LLM, enable the mode, confirm the co-load
  gate's pass/refuse, one spoken round-trip, the mic gated during playback, and a
  forced leg failure degrading cleanly. Record the outcome in the PR body (and
  what the mock tests already prove, so a skipped live step is stated, not
  silent).
- [ ] **Step 4:** Remove `docs/superpowers/**` and `docs/implementation-status.md`
  from the branch; verify `git diff --name-only main...HEAD` shows neither.
- [ ] **Step 5: Commit** `chore(deploy): nginx + vite ws route for voice chat` and open the PR.

---

## Self-review notes

- **Spec coverage:** WS + ticket (T1,T3,T5), co-load gate (T2), state machine +
  turn detection + half-duplex + chunking/`pendingText` (T6), three-core
  composition + speculative deferred-commit voice run (T4), continuation window /
  live transcript / pause-tolerant turns (T6+T7, frontend T8+T9), billing
  inherited (T4 tests), degradation (T7), frontend reuse + UI (T8–T10), i18n +
  docs + ADR-078 (T11), deploy + verification (T12). Phases 2–4 are explicitly
  out of scope.
- **Review Focus** lines are each pinned to a task's test (T6 mic-drop + chunk
  triggers, T4+T7 pause-tolerance/cancel-and-extend, T7 degradation, T2 honest
  refusal, T1/T5 ticket misuse).
- **Agent/migration:** none expected; if either becomes necessary, bump
  `Version` once off `0.13.0` and/or add migration v96 with the Postgres leg +
  ADR-005 check, and say so in the PR.
- **Type consistency:** `voiceTicketClaims`/`voiceCoLoadGate`/`startVoiceRun`/
  `relayVoiceChat` signatures and the `voiceDownMsg`/`voiceUpControl` frame types
  are used consistently across T1–T7; the `portal.voice_chat_*` codes defined in
  T2 are the same ones surfaced in T7 and translated in T11.

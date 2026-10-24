# Voice Chat Mode — Design (issue #312)

**Status:** design for review (branch-local working document; folds into
`docs/architecture/` before the PR).
**Issue:** #312 — Voice Chat mode: continuous spoken conversation
(live-ASR → LLM → TTS) orchestrated in the backend.
**Serial-roadmap slot (verified against `main` on 2026-10-24):** next free
**ADR-078**; agent **0.13.0** (expected **no bump** — composes existing agent
routes without changing the wire contract); highest migration **v95**
(expected **no migration** — new state rides the schema-free chat document).

---

## 1. Intent and the why

Today the portal already has every leg of a spoken conversation, but a human
has to drive each one by hand: start live transcription, read the committed
text, press send, wait for the answer, press "speak", listen. Voice Chat makes
that loop **hands-free and continuous**: you pick a live-ASR model, an LLM, and
a TTS model, enable the mode, and then you just talk — the gateway hears you,
sends what you said to the LLM, and speaks the answer back, while the chat
transcript fills in exactly as if you had typed and sent each turn.

The point is a *verbal* back-and-forth where the whole linking happens **in the
backend**, so the guarantee that matters most — *the microphone can never feed
the model's own voice back into itself* — is a property of the server, not of a
cooperative browser.

This is an epic. Phase 1 (this branch) ships the orchestration MVP that speaks
the whole answer; phases 2–4 are sketched here and deferred to their own
branches.

## 2. Goals, non-goals, anti-goals

### Goals (the whole feature)
- A portal Voice Chat mode: pick live-ASR + LLM + TTS, enable, converse
  hands-free; the transcript fills in as normal chat turns.
- The ASR→LLM→TTS linking is orchestrated **entirely in the backend**.
- The user's recognized speech appears as a normal **sent user message**; the
  LLM answer appears as **text plus streamed audio**.
- Turn detection by server-side silence/VAD **and** an explicit push-to-talk
  control (both in phase 1).
- A **live, low-latency transcript** of what the user is saying (exactly the text
  that will become the LLM request), streamed as it is recognized.
- A **clear listening indicator** so the user can always see whether the system is
  currently listening or not (it is gated while the answer plays — §6.2).
- **Pause-tolerant turns:** a pause that triggers a tentative send does not lose a
  user who keeps talking — the in-flight LLM request is cancelled, the new speech
  extends the previous text, and the combined text is (re-)sent on the next end
  (spec §6.1). This is phase 1.
- A **co-load admission gate**: the mode is only released when all three models
  can be admitted together; a clear message when they cannot.
- **The loop can never happen** — half-duplex gating enforced server-side.
- Billing is inherited from the three existing `/v1` cores (ASR `audio_second`,
  LLM tokens, TTS `audio_second`/`audio_bytes`); no new metering.
- (Phase 2) a TTS-optimized system prompt plus an optional
  `<speak lang="…">…</speak>` tag the backend extracts (text + per-answer
  language).
- (Phase 3) multiple spoken sections per answer, synthesized sequentially,
  played back-to-back, each a collapsed, replayable block that reuses the
  stored audio blob.
- (Phase 4, optional) barge-in.

### Non-goals
- Acoustic echo cancellation (AEC). Loop prevention is half-duplex gating, not
  signal-level echo removal. (This is what makes "the loop can never happen"
  tractable.)
- A public bearer API for the orchestration surface. Voice Chat is a **portal**
  feature; the three underlying `/v1` cores already have their own public APIs.
- New billing units or a new metering model.
- Multi-user / shared voice sessions, wake-word, or always-on listening across
  page reloads.
- Changing any of the five merged building blocks' wire contracts.

### Anti-goals (these would be failures even if the feature "worked")
- Any design where loop prevention depends on the client behaving. If a
  misbehaving or malicious browser can make the model transcribe its own TTS
  output, the design has failed.
- Spreading the orchestration across the client so that "the backend
  orchestrates" is only nominally true.
- Re-implementing admission, billing, or the streaming relays instead of
  composing the existing `/v1` cores (drift between two copies of the same
  relay is exactly the failure #266/ADR-073 avoided by looping back through
  `/v1`).
- A co-load gate that silently lets a conversation start with a model that
  isn't actually loadable, then fails on the first turn.

## 3. Constraints

- **Repo rules (AGENTS.md):** feature branch + PR, never commit/merge to
  `main`; English repo-facing text; keep LAN host names out of all repo text;
  push over SSH; run the local gates before pushing (per-module
  `golangci-lint` fmt+run, frontend prettier `format:check` + build + vitest,
  `make sonar-gate` + `sonar-branch-findings`, Postgres leg for any store
  change); substantive commit body (squash description comes from it).
- **Auth seam:** the WS upgrade cannot carry `X-OP-CSRF`, so the orchestration
  WS authenticates by a **single-use, short-TTL, AES-256-GCM-encrypted ticket**
  minted by a CSRF-protected POST — exactly the #266 pattern. The internal
  loopback calls use the trusted-loopback header pair
  (`internalAuthHeaderName` + `internalUserHeaderName`, `X-OP-CSRF: 1`,
  target `s.selfBaseURL`).
- **Compose, don't fork:** each `/v1` core already owns admission (spent once),
  its per-route guard, its billing row, and its verdict gates. The orchestrator
  sequences them and surfaces their stable error codes; it does not duplicate
  them.
- **ServerAgent version rule:** bump only if the agent's behaviour or wire
  contract changes. Phase 1 is expected to need **no bump**; if any agent
  change sneaks in, bump once off `main`'s current `0.13.0` per the collision
  rule.
- **Persistence:** reuse the schema-free chat document and the existing
  `chat_blobs` audio-part storage; a new per-chat "voice settings" field rides
  `ChatRunSettings`/`ChatAudioSettings` the way #281/#282 added fields (no
  migration). Any genuine store change must be verified with the Postgres leg
  and ADR-005 (wide types).

## 4. The decisions already made (with the user)

These were settled in the brainstorm and are **not** open:

1. **Session shape = one new backend orchestration WebSocket** that is a
   server-side state machine composing the three existing `/v1` loopback cores.
   The client holds one connection; the server is authoritative over the whole
   loop. *(Confirmed.)*
2. **Phase-1 turn detection ships both** server-side silence/VAD **and**
   push-to-talk. *(Confirmed.)*
3. **LLM + TTS are kept warm for the whole session** (best-effort; see §6.5 for
   the honest limits). *(Confirmed.)*
4. **Transcript display:** the full answer text renders normally; `<speak>`
   regions *also* become collapsed, replayable audio blocks (phase 3).
   *(Confirmed.)*
5. **Barge-in is deferred to phase 4.** *(Epic + confirmed.)* Note the
   distinction from the phase-1 continuation window (§6.1): continuation is
   resumed speech during `thinking` **before any TTS plays** (no echo, cheap to
   cancel); barge-in is interrupting the TTS during `speaking` (needs echo
   handling), which is why only the latter is deferred.
7. **Live transcript + pause-tolerant turns** (speculative LLM start, cancel and
   extend on resume, commit on the real end) are **phase 1**. *(Confirmed.)*
6. **Live verification** uses the operator's test audio.cpp server (reachable
   via the logged-in gateway in the in-app browser; host name kept out of repo
   text).

## 5. Architecture — the orchestration WebSocket

### 5.1 Surfaces (new)

- `POST /api/portal/voice-chat-ticket` — CSRF-protected (session cookie +
  `X-OP-CSRF`, `requireWebScope(scopeGatewayUse)`). Validates the three chosen
  models, runs the **co-load gate** (§6.4), mints a single-use encrypted ticket
  carrying the resolved targets + PCM params + principal. Modeled on
  `handlePortalLiveTranscriptionTicket` and the live ticket store.
- `GET /api/portal/voice-chat` — ticket-authenticated WS upgrade
  (ticket as the 2nd `Sec-WebSocket-Protocol`, redeemed before `Accept`,
  coder/websocket same-origin check). Modeled on `handleLiveTranscription` +
  `relayLiveTranscription`, with the single-writer/ping-pong/airtight-teardown
  discipline (`liveWriter`, `agentStreamPing*`) carried over.

Both registered on `s.mux` in `server.go` beside the live-transcription routes.

### 5.2 The client ↔ orchestrator protocol

**Up (client → server):**
- Binary frames: s16le PCM mic audio, 16 kHz mono, ~100 ms batches — the exact
  format the live-ASR capture already produces (`livePcm`/`captureWorklet`,
  `audioToWav` constants). Reused verbatim.
- Text control frames:
  - `{"type":"push_start"}` / `{"type":"push_end"}` — push-to-talk boundaries
    (optional; when present they override the silence timer for that turn).
  - `{"type":"cancel"}` — stop the current turn's TTS / abandon the in-flight
    turn, return to listening.
  - `{"type":"playback_done"}` — the client's audio player has finished playing
    out the answer (its `StreamingPcmPlayer` drained — the #304 `onDrained`
    signal). This is what lets the server end `draining` at true playout-done
    rather than relay-done (§6.2); the server still owns the gate and applies its
    own fallback, so a missing or late signal cannot wedge the session.
  - `{"type":"bye"}` — end the session cleanly.

**Down (server → client):**
- `{"type":"state","state":"idle|listening|thinking|speaking|draining"}` — the
  state machine (drives the UI and the mic gate).
- `{"type":"asr_partial","text":<provisional>}` — the **current chunk's**
  provisional text, replace-not-append (mirrors the live-ASR `partial` contract).
  It is NOT the whole turn: across a pause-tolerant multi-chunk turn the client
  shows `pendingText` (the finalized chunks so far) **plus** this partial.
- `{"type":"asr_final","text":<chunk final>}` — the finalized text of **one
  chunk**, appended to `pendingText`. No `message_id`: nothing is persisted at a
  chunk boundary. The user turn is persisted only at commit (§5.4/§6.1).
- `{"type":"answer_delta","text":<increment>}` — LLM token stream (so the
  answer fills in live), then `{"type":"answer_done"}`. `answer_done` is
  **provisional** — it fires when the answer text is complete (before commit), and
  a later `answer_reset` can still void it. No `message_id`.
- The persisted turns are not delivered field-by-field: after `audio_done` the
  client **refreshes the chat thread** to pick up the committed user + assistant
  turns (text + stored audio). So no down-frame carries a `message_id`.
- `{"type":"answer_reset"}` — the speculative LLM run was cancelled because the
  user resumed speaking (§6.1); the client discards the answer preview shown so
  far and returns to showing the growing transcript.
- `{"type":"audio_delta","seq":<n from 0>,"rate":<hz>,"audio":"<base64 PCM>"}`
  → feeds `StreamingPcmPlayer.acceptChunk` unchanged, then
  `{"type":"audio_done"}`.
- `{"type":"error","code":<stable code>,"message":<text>}` — surfaces each
  core's own stable error code (e.g. `transcription.live_busy`,
  `runtime.admission_blocked`, a chat-run code, a speech code).

### 5.3 The server-side state machine

```
            push_start / voiced input
   idle ───────────────────────────────▶ listening ◀───────────────┐
     ▲                                        │                     │
     │                       chunk end (silence timer or push_end)  │
     │                       → append chunk to pendingText          │
     │                                        ▼                     │
     │                              thinking + CONTINUATION WINDOW   │
     │                         (speculative LLM runs on pendingText; │
     │                          mic STILL live — no TTS yet;         │
     │                          answer_delta streams as a preview)   │
     │                                        │                     │
     │                  resumed speech in the window:               │
     │                  cancel LLM run, emit answer_reset ──────────▶┘
     │                  (append next chunk, keep accumulating)
     │                                        │
     │                   window closes with no resume
     │                 (LLM done AND ContinuationGraceMs elapsed)
     │                   → COMMIT user+assistant turn
     │                                        ▼
     │                                     speaking ──(TTS streams PCM;
     │                                        │          mic input DROPPED)
     │                                   audio done
     │                                        ▼
     │                                     draining ──(tail plays out;
     └────────────────────────────────────  mic input STILL dropped)
         tail drained → listening (next turn); pendingText cleared

   any leg error → emit {error} → idle (mic re-enabled; recoverable)
   cancel        → stop LLM/TTS → listening
   bye / drop    → teardown (release ASR session, cancel loopbacks)
```

### 5.4 How each leg composes an existing core

- **Mic → ASR (per turn):** on entering `listening`, the orchestrator opens a
  **fresh** internal full-duplex loopback to `POST /v1/audio/transcriptions/live`
  (the `openLiveLoopback` pattern: `s.selfBaseURL`, `Expect: 100-continue`,
  trusted-loopback headers, body = `io.Pipe`). Client PCM frames are piped up;
  the SSE `delta`/`partial` lines are mapped to the `asr_*` down-frames. On turn
  finalize the orchestrator closes the pipe (`{"type":"end"}` upstream), which
  makes the bearer core emit the **clean final transcript** and end the relay,
  releasing that turn's admission and per-route slot. This uses the live-ASR
  contract exactly as it is tested (final on end-of-input); it does **not**
  depend on any per-utterance `transcript.text.done` mid-stream (ADR-072 left
  that open), and it does **not** pin the ASR child for the whole session (see
  §6.5 for why that matters on a shared GPU). The ASR child stays *warm* between
  turns via the co-load gate (§6.4) and idle-unload windows; only the relay is
  per-turn. Billing is therefore one honest `audio_second` row per turn.
- **Turn → LLM (speculative):** on a chunk end the orchestrator starts a text
  completion for the chosen LLM on `pendingText` — POST `/v1/chat/completions`
  with the trusted-loopback headers — and relays the answer deltas as
  `answer_delta`. This run is **speculative and cancellable** (§6.1): if the user
  resumes within the continuation window it is cancelled with **nothing
  persisted**. Persistence is therefore **deferred to the commit** (below), not
  done up front — so the standard `PrepareChatRun`-commits-the-user-turn-first
  flow is adapted: the user turn is written only when the turn truly finalizes.
- **Answer → TTS, then commit:** when the continuation window closes with no
  resume, the orchestrator forwards the answer text to `POST /v1/audio/speech`
  (streaming) for the chosen TTS model and relays `speech.audio.delta` as
  `audio_delta`, cut into stored WAV parts via the existing blob path. It then
  commits **one assistant turn** carrying the answer text + its audio
  (`CommitAssistant` with `AudioStreamed`, `SaveChatWithBlobs`) alongside the
  user turn (`pendingText`) — a normal stored, replayable audio answer.

**The one genuinely new chat-run behaviour:** today a *speech-kind* thread
synthesizes TTS from the *user's* prompt, and a *text-kind* thread runs the LLM
with no audio. Voice Chat needs **text-LLM-then-TTS on one assistant turn**, run
**speculatively** and with persistence **deferred to a true finalize** so a
cancelled run leaves nothing to undo. Phase 1 introduces this as a new **voice
run kind** (`executeVoiceRun`): it runs the completion and, on commit, synthesizes
the answer and persists the user turn + the assistant turn (text + audio blobs)
together. The exact split between the orchestrator (which owns the continuation
window and the cancel) and `executeVoiceRun` (which owns the loopback calls and
the deferred commit) is a plan-level decision; §9 states the recommendation.

## 6. The open questions, resolved

### 6.1 Live transcript and turn detection (server-authoritative)

**Live transcript (low latency).** While the user speaks, the ASR `delta`/
`partial` lines are streamed to the client **continuously** as `asr_partial`
down-frames (replace-not-append), so the user sees exactly the text accumulating
toward the LLM request, with no extra buffering on the orchestrator. The
orchestrator keeps a `pendingText` buffer = the finalized chunks so far; the live
display is `pendingText` + the current `asr_partial`.

**Turn finalization.** The **server** decides a chunk boundary: a **silence
timer** — after a voiced partial, if no new partial arrives for
`SilenceTimeoutMs`, the current chunk ends; or an explicit `push_end`. Guards:
`MinUtteranceMs` (ignore sub-threshold blips) and `MaxUtteranceMs` (hard cap). A
chunk end **appends** the chunk's final text to `pendingText`.

**Speculative start + continuation window (the "keep talking after a pause"
rule).** A pause is ambiguous: the user may be done, or just drawing breath. So
on a chunk end the orchestrator does **both** — it starts the LLM
**speculatively** on `pendingText` (the answer begins streaming as `answer_delta`,
low latency) **and** it keeps the mic live and opens a **continuation window**.
The mic is safe to keep live here because no TTS is playing yet (the half-duplex
gate applies only to `speaking`/`draining`).

- If the user **resumes speaking** within the window: the orchestrator **cancels
  the in-flight LLM run**, emits `answer_reset` (the client drops the answer
  preview), captures the new speech as the next chunk, appends it to
  `pendingText`, and returns to `listening`. On the next chunk end it starts the
  LLM again on the now-longer `pendingText`. This can repeat across several
  pauses.
- If the window **closes with no resume** — the LLM has finished **and**
  `ContinuationGraceMs` has elapsed since the last chunk end with no new voiced
  input — the turn is **committed**: the user turn (`pendingText`) and the
  assistant turn (the answer) are persisted, TTS synthesis begins, and the
  machine moves to `speaking`. `pendingText` is cleared.

Because the LLM runs *during* the grace, the grace usually adds no latency (it is
hidden behind generation that takes longer than the grace); it only waits when
the LLM is faster than the grace. Persistence is therefore **deferred to the
commit** (§5.4): nothing is written for a speculative run that gets cancelled, so
cancel-and-extend needs no undo.

Proposed defaults (config-tunable): silence ≈ 800–1000 ms, min ≈ 300 ms, max
≈ 30 s, continuation grace ≈ 1500 ms.

Note: ADR-072 left open whether the live ASR emits a per-utterance
`transcript.text.done` mid-stream; **the design does not depend on it** — the
silence timer plus the per-chunk relay close are the source of truth.

### 6.2 Half-duplex loop prevention (the guarantee)
While the state machine is in `speaking` **or** `draining`, inbound binary PCM
frames are **discarded in the orchestrator** before they ever reach the ASR
loopback. This is enforced on the server, so no client behaviour can route
TTS-era audio into the ASR. That is the whole "loop can never happen" guarantee;
it needs no AEC. (Barge-in in phase 4 will *deliberately* relax this under an
explicit, bounded rule.)

**`draining` must track playout, not relay.** The orchestrator relays the TTS PCM
to the client **faster than real time** (it streams as fast as the TTS
synthesizes), so "all audio relayed" (`audio_done`) is *not* "the user has
stopped hearing it." If the gate ended at relay-done, the mic would re-open on a
speaker setup while the answer is still audible — and feed straight back into the
ASR. So `draining` ends at **true playout-done**: the client sends
`{"type":"playback_done"}` when its `StreamingPcmPlayer` drains (the #304
`onDrained` signal), and only then does the server apply the short
`MicResumeTailMs` tail and return to `listening`. The gate stays
**server-authoritative**: the server keeps discarding mic PCM for the whole
`draining` period and decides when it ends — the client signal only tells it
*when playout finished*. A **server-side fallback** bounds the wait so a missing
or late signal cannot wedge the session: the server computes the relayed audio's
duration (`pcmBytes ÷ byteRate` when the TTS byte-rate is cached, else a generous
fixed cap) and ends `draining` at the later of that bound and the signal. This
reuses the exact lesson from #304 ("gate *still playing* on the Web-Audio queue
drain, not on stream completion").

**A consequence that shapes admission:** because the loop is half-duplex, the
three legs run *sequentially* within a turn — ASR (listen), then LLM (think),
then TTS (speak) — never at the same instant. So strict three-way co-residency
is **not required for correctness**: a VRAM-tight host could time-share one slot
and reload between phases, and the conversation would still be correct (just
sluggish). Co-residency is what keeps the conversation *fast*. That is exactly
what the co-load gate (§6.4) and the "kept warm" decision protect: they make the
low-latency promise honest, and they are not load-bearing for the loop
guarantee.

### 6.3 Billing (inherited)
No new metering. Each leg bills through its own `/v1` core exactly as today:
ASR `audio_second` (bytes ÷ byteRate, loud-zero on unclean end) at
`/v1/audio/transcriptions/live`; LLM tokens via `/v1/chat/completions`; TTS
`audio_second` (or `audio_bytes` when the byte-rate is uncached) via
`/v1/audio/speech`. Each row is attributed to the loopback principal, like a
normal chat run. The orchestrator spends **no** admission of its own.

### 6.4 The co-load admission gate
The agent enforces admission by **three gates** (agent-runtime-manager §5.2):
(1) the operator's pairwise **co-residency matrix** — "not co-resident" is the
structural default, so a pair must be explicitly opened; (2) a process-count
limit; (3) per-GPU **VRAM arithmetic** (summed demand ≤ budget per touched GPU).
Three sequential `/ensure` calls do **not** verify this — each can "succeed" by
*evicting* the previous, so a naive gate passes while the three are not actually
co-resident, and the first turn reloads/evicts (the §2 anti-goal).

The honest gate, at **mode enable** (ticket mint):

1. Resolve + gate all three mappings (fast, explicit error for an unroutable or
   ungated model, naming which leg).
2. For the **agent-managed** legs, `EnsureRuntimeModel` (`/ensure/{model}`) each
   — this warms them; a `runtime.admission_blocked` (503) on any → refuse,
   naming the leg.
3. **Verify residency**: read the agent runtime report and confirm **all three
   are resident together**. If ensuring the third evicted the first, only two
   show resident → **refuse**, with a message pointing at co-residency/VRAM
   configuration. This is the step that makes the gate honest, and it reuses the
   live agent state rather than re-implementing the agent's admission arithmetic
   (the arithmetic has subtle, test-pinned ordering and a direction-sensitive
   pair map — re-deriving it gateway-side invites drift; agent-runtime-manager
   §5.2 warns about exactly this).

This is **topology-agnostic**: if the LLM lives on a different server it is
trivially resident there while ASR + TTS are resident on the audio.cpp host; if
all three share one GPU, the residency check catches a configuration that cannot
hold them together. A model with no `/ensure` (non-agent direct mapping) skips
steps 2–3 and relies on step 1 plus the degradation path (§6.6).

*Refinement (follow-up, not phase 1):* a pre-flight that computes the three
gates gateway-side from the co-residency matrix (`CoResidencyRulesByApplication`,
via the canonical `Config.AllowedPairs()`/`PairKey`) + GPU budgets, to refuse
*before* any eviction churn and give a more specific reason. Phase 1's
ensure-then-verify is sound without it.

### 6.5 Keeping the three warm (honest limits)
All three are warmed by the co-load gate (§6.4) and kept warm by per-turn use;
within an active conversation each leg is touched every turn, inside the agent's
idle-unload window.

The honest limit: there is **no gateway-side primitive that pins a child for a
whole session**. The live `/live` relay *can* pin its ASR child for the relay's
duration — but we deliberately **do not** use that here, because a session-long
ASR pin would make the ASR child un-evictable and, on a VRAM-tight single-GPU
host, would block the TTS child from ever loading (every turn's TTS →
`admission_blocked`). The per-turn ASR relay (§5.4) avoids that: between turns
ASR holds no admission, so the agent can time-share the slot if it must. The
cost of keeping all three genuinely resident across the conversation is paid by
the co-load gate verifying they fit; a long pause that idle-unloads a leg, or a
competing external load that evicts one, surfaces as a re-admit failure on the
next turn and is handled as degradation (§6.6), never a crash or a loop. This
limit is documented in ADR-078 and §11.4.

### 6.6 Degradation and errors
Any leg failing mid-conversation — ASR drop, LLM error, TTS error, or a
mid-conversation `runtime.admission_blocked` — causes the orchestrator to emit
a typed `{"type":"error","code":…}` down-frame (the core's own stable code),
**stop the current turn**, and return to `idle` with the mic re-enabled. The
session stays open and recoverable; there is **no silent retry** that could spin
into a loop. A clean `bye` or a dropped socket tears everything down (release
the ASR session, cancel the loopback contexts) exactly once.

## 7. Portal UI (phase 1, mostly reuse)

- A Voice Chat **mode toggle** on a thread whose three capabilities are
  satisfiable (an LLM-capable model + a `transcription`-streaming model + an
  offerable streaming TTS). The mode is only enabled when the co-load gate
  passes; otherwise the toggle shows the refusal reason.
- One new WS client, modeled on `createLiveSession`/`useLiveTranscription`,
  reusing `captureWorklet`/`livePcm` for capture and `StreamingPcmPlayer`
  (`speechPlayback`) for playback.
- A **prominent listening indicator** driven by the `state` down-frames that
  clearly distinguishes **listening** (mic open, capturing — e.g. an active mic
  icon + "listening…" and the live transcript) from **not listening** (`thinking`
  / `speaking` / `draining` / `idle` — e.g. a muted mic icon + "speaking…"). This
  is not cosmetic: during `speaking`/`draining` the server gates the mic
  (half-duplex, §6.2), so the user must *see* that the system is not hearing them,
  or they will talk into a void. The indicator is the user-visible mirror of the
  state machine, with the live transcript (§6.1) shown while listening.
- Turns render through the normal chat message path: the ASR final as a sent
  user message, the answer as streaming text (`answer_delta`) plus a streamed,
  stored audio answer (the existing `AudioTurn` + tail controls).
- i18n: a new `chatVoice*` key cluster added to **both** `de` and `en` in
  `i18n.ts` (the type-checked parity).

Phase-1 UI is intentionally minimal; the collapsed replayable spoken-section
blocks (mirroring the reasoning `<details>` block) are **phase 3**.

## 8. Phasing

1. **Orchestration MVP (this branch):** the WS + ticket, the server state
   machine, the three-core composition, server-side half-duplex, the co-load
   gate, turn detection (silence + push-to-talk), minimal UI; **the whole answer
   is spoken** (no tag). Billing inherited. ADR-078.
2. **`<speak lang>` + dynamic language:** a TTS-optimized system prompt; a
   streaming scanner that extracts `<speak lang="…">…</speak>` from the token
   stream; text + per-answer language → TTS via #282's top-level `language`;
   no tag → speak the whole answer. Transcript shows the full answer text.
3. **Multiple spoken sections + replayable blocks:** several `<speak>` regions
   per answer, synthesized sequentially as each completes, played back-to-back
   for low time-to-first-audio; each region is a collapsed, play-button block
   (mirroring the reasoning `<details>` block) that replays the **stored** blob
   (#279/#280/#283) and re-synthesizes only if the blob is gone.
4. **(Optional) barge-in:** the user speaking interrupts/stops the TTS — a
   deliberate, bounded relaxation of the half-duplex rule. The per-turn ASR
   relay (§5.4) is the intended foundation: barge-in replaces the mic *discard*
   during `speaking`/`draining` with a cheap **voice-activity detector** (not
   full ASR — running ASR during TTS would be the loop). On a VAD trigger the
   orchestrator stops the TTS, leaves `speaking`, and opens a fresh per-turn ASR
   relay into `listening` — the barge-in utterance then flows through the exact
   normal turn path (the client's connect-time audio buffer covers the re-open
   latency, so the utterance's start is not lost). Phase 4 thus adds only a
   detector plus one transition edge; it does not change the per-turn model. The
   residual cost — the mic listening while the TTS tail fades — is the explicit,
   bounded relaxation, which is why barge-in is the hard, optional phase (trivial
   on a headset; a speaker setup wants an immediate TTS cut or echo robustness).

## 9. What is left to the builder (plan-level)

- **Voice run kind vs. orchestrator composition.** *Recommendation:* a new
  `executeVoiceRun` run kind that runs the text completion and then drives the
  speech relay, committing one assistant turn carrying both the answer text and
  its audio blobs — this keeps persistence/retention/billing on the established
  run path rather than inventing a parallel commit. The plan validates this
  against `finishRunWithParts`/`CommitAssistant`/`SaveChatWithBlobs`.
- **Where the silence timer lives** (orchestrator goroutine vs. the ASR read
  loop) and the exact default constants (config keys under
  `OP_AI_GATEWAY_VOICE_*`).
- **Ticket claims shape** for three targets (vs. the live ticket's one). The
  co-load gate (resolve+gate → ensure → verify-resident, §6.4) runs at **mint**
  so the mode is refused before release; the exact runtime-report read used for
  the residency check is a builder choice.
- **The new portal error codes** (`portal.voice_chat_*`) and their HTTP/status
  rows, co-located with the handler per the repo's two idioms.
- **Exactly which capability gates** a model must satisfy to appear in each of
  the three pickers, reusing `CapabilityRows*`, `model-transcription-streaming`,
  `model-speech-streaming`, and the offerable-TTS set.
- **TDD coverage:** the half-duplex invariant (mic frames during
  speaking/draining are dropped), the state transitions, turn finalization,
  co-load refusal, and degradation on each leg's failure — provable with mock
  loopback cores, as #266 did.
- **Architecture docs to fold in** (ADR-078 + runtime-view scenario +
  api-surface/openapi + telemetry + risks §11.4 + the voice-settings data
  model), and the branch-local `docs/superpowers/**` +
  `docs/implementation-status.md` removal before the PR.

## 10. Live verification plan

End-to-end against the operator's test audio.cpp (via the logged-in gateway in
the in-app browser): load a streaming ASR model + a streaming TTS (preset
clone) + an LLM, enable the mode, confirm (a) the co-load gate passes/refuses
honestly, (b) a spoken turn round-trips ASR→LLM→TTS, (c) the mic is gated while
the answer plays (no self-transcription), and (d) a forced leg failure degrades
cleanly. Unit/mock tests remain the authoritative gate for CI; the live run
confirms the real contracts the mock can't (host name kept out of repo text).

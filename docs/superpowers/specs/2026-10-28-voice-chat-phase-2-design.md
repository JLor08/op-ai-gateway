<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 OnPrem AI Gateway contributors -->

# Voice Chat — Phase 2 design: speakable summaries, editable voice system prompt, per-answer language

Branch-local working doc (removed before the PR, per `AGENTS.md`). Issue
**#314**, phase 2 of the Voice Chat epic **#312**. Builds on phase 1 (#313),
the model picker (#319), and the TTS voice picker (#320). The canonical
system description lives in `docs/architecture/` + **ADR-078**; this spec is
the design to plan from.

## 1. Intent and why

Phase 1 speaks the **whole** answer. In a real spoken conversation nobody
wants to sit through a minutes-long monologue of a long, detailed answer read
out verbatim. Phase 2 makes the **spoken** part a short, self-contained
**summary that carries the essence**, while the **fuller detail stays in the
transcript as supplementary text that is shown but not spoken**. You listen to
the gist and only read on if you want to.

The mechanism is a backend-extracted `<speak lang="…">…</speak>` region: the
LLM is instructed (via an editable, pre-filled voice system prompt) to lead
with one short speakable summary wrapped in the tag, in the conversation's
language, and to put the rest outside it. The backend stream-parses the token
stream, extracts the `<speak>` inner text + its language, and synthesizes
**only that** — reusing #282's per-request top-level `language` so the spoken
language follows the answer. In the transcript the spoken summary renders as a
collapsed, replayable block (like the reasoning / "Denken" block — play to
re-listen, expand to read) and the supplementary detail as the normal answer
body; the `<speak>` tags are stripped from the stored and displayed text.

## 2. Goals / non-goals / anti-goals

**Goals**

- The spoken output is a short summary (the essence), not the whole answer;
  the unspoken remainder is supplementary text in the transcript.
- The LLM chooses the spoken language **per answer** (e.g. you speak German →
  it summarizes and speaks German; switch to English → it follows), delivered
  to the TTS via #282's `language`.
- A **voice system prompt** that drives this behaviour: pre-filled with a good
  default, **visible and editable** by the user, stored **per thread** in
  `settings.voice_chat`, applied **only to Voice Chat answers**.
- No regression to phase 1: no tag → speak the whole answer (unchanged
  behaviour); typed turns in the same thread are completely unaffected.

**Non-goals (deferred to later phases)**

- **Multiple** `<speak>` regions per answer (several spoken blocks,
  synthesized sequentially) — **phase 3** (#315). Phase 2 handles exactly one
  spoken region, rendered as **one** collapsed, replayable block (§5.4).
- Barge-in / interrupting the TTS by speaking — **phase 4** (#316).
- Re-synthesizing or editing the spoken region after the fact.

**Anti-goals (a failure even if it "works")**

- The voice prompt changing **typed** (non-voice) answers in the thread.
- `<speak>` markup leaking into the **persisted/displayed** transcript text or
  into later LLM context / strict upstreams.
- Speaking the whole long answer by default (the phase-1 monologue) once the
  summary behaviour is in — the spoken part must be the summary.
- A hard failure (silence or an error turn) when the tag is missing,
  malformed, or names a language the chosen voice can't do. Phase 2 degrades,
  it does not break the loop.

## 3. Constraints

- **No ServerAgent bump, no migration** — ride the schema-free chat document
  (`settings.voice_chat`), exactly as the pickers do. New sub-field(s) only.
- **Voice-run-only**: the voice system prompt and the `<speak>` extraction live
  on the phase-1 voice run kind; nothing touches the normal text run path.
- **Reuse, don't reinvent**: the spoken language rides #282's existing
  top-level `language` on the speech request (`service_chats.go` speech body;
  `chat_runs_audio.go` already sends `body["language"] = audio.Language`).
  Model language support is already discoverable per model
  (`audiocpp_models.go` detects whether an entry advertises a `language`
  param), so capability is a lookup, not a guess.
- **Server-authoritative loop**: the half-duplex guarantee, the playout-aware
  drain, and the continuation window are **preserved**. Phase 2 changes *what
  text is spoken* and overlaps the LLM and speech legs (§5.5), but **not** *when
  the mic is live* — mic governance stays the phase-1 drain, and the
  continuation/cancel window still closes before any TTS plays.

## 4. The experience

1. User enables Voice Chat (phase 1 UI) and talks.
2. The voice run starts; the LLM, under the voice system prompt, emits an
   answer that **opens with** `<speak lang="de">…short summary…</speak>` and
   then continues with the fuller detail (outside the tag).
3. The backend streams the inner `<speak>` text into the TTS leg with
   `language="de"` (you **hear the summary**), and renders the turn so that the
   **spoken summary is a collapsed, replayable block** (like the reasoning /
   "Denken" block) and the **supplementary detail is the normal answer body**
   below it (§5.4). You hear the gist; you can read the detail; you can expand
   the block to read the summary or press play to hear it again.
4. If the model emits no tag, the whole answer is spoken and rendered like
   phase 1 (full text + one audio player, no collapsed block).
5. Loop continues exactly as phase 1 (drain → mic re-opens).

The editable voice system prompt sits with the other Voice Chat settings (the
model/voice pickers): a multi-line text field, pre-filled with the default,
overridable, persisted per thread.

## 5. Design

### 5.1 Editable voice system prompt (per-thread, pre-filled)

- New sub-field under `settings.voice_chat` (schema-free, no migration), e.g.
  `voice_chat.system_prompt` (string). Absent / empty → the **built-in
  default** applies. The frontend pre-fills the field with the default so the
  user sees it; saving an unchanged value simply stores the default, saving a
  blank clears back to the default.
- Applied **only on the voice run**, as the voice run's **sole system
  message**. (Builder-check correction: phase-1 voice runs apply **no** system
  prompt today — `VoiceRunHistory` builds history with `buildAPIHistory("", …)`,
  and the thread's typed-chat system prompt never reaches a voice run. So there
  is nothing to "augment"; phase 2 introduces the voice system prompt as the
  system message for voice answers. The thread's typed-chat persona not applying
  to voice is the existing behaviour and stays — bringing it into voice runs is
  out of scope for phase 2.)
- **Delivery = server-side read at run time, not the ticket.** The voice run
  reads `settings.voice_chat.system_prompt` from the **stored chat document**
  when it builds the LLM history (in `startVoiceRun` / `VoiceRunHistory`).
  (Builder-check correction: the encrypted ticket caps the settings blob at
  1024 B and the whole ticket at ~7 KB — an nginx 8 KB header-line limit — so a
  multi-line user prompt could fail the mint. Reading from the stored doc avoids
  the cap **and** gives the stated security property for free: the prompt is the
  stored value, never a per-request POST/ticket field, so a client can't plant
  one.) The `PrepareChatRun` lift of the whole `voice_chat` blob already
  preserves the field across **text** sends (phase-1/#319, `liftedVoiceChat`) —
  that needs no change; the voice run itself does **not** go through
  `PrepareChatRun`, which is exactly why it reads the stored doc directly.

### 5.2 Streaming `<speak lang>` extraction

- As the voice run streams the completion, a small **stream scanner** watches
  the token text for the first `<speak …>` open tag and its matching
  `</speak>` close.
- The scanner must tolerate the tag and attribute **spanning token
  boundaries**. The speech endpoint takes the **complete** input text in one
  request body (`buildSpeechBody`, `chat_runs_audio.go`) — there is no
  streaming-*input* path — so the one speech request carries the **complete
  summary** (the text captured between `<speak …>` and `</speak>`), not a
  token-by-token feed.
- **The speech request fires as soon as `</speak>` is seen** (decided:
  concurrent, §9.0), while the LLM keeps streaming the detail in the background.
  Since the prompt puts the summary first, `</speak>` arrives early — that is the
  time-to-first-audio win. The overlapping two-leg flow is specified in §5.5.
- **One region (phase 2)**: only the **first** `<speak>` region is spoken; any
  later `<speak>` regions in the same answer are treated as plain text for
  phase 2 (their multi-region handling is phase 3). This keeps the phase-2
  contract simple and forward-compatible.
- **Language**: the `lang` attribute of that region → the speech request's
  `language`. Absent `lang` → omit `language` (TTS model / voice default, =
  phase-1 behaviour).

### 5.3 Per-answer language → TTS (#282)

- The extracted `lang` flows to the voice run's speech leg as #282's top-level
  `language`. No new transport — it is the field `chat_runs_audio.go` already
  sends.
- **Capability-aware, best-effort (my call, see §9):** `audiocpp_models.go`
  already knows whether the chosen TTS model accepts a `language` param. If it
  does, pass the requested language through. If it does **not**, omit the
  param (the model speaks its native language) rather than failing — the
  default prompt also steers the model to summarize in the voice's language
  when unsure, so mismatch is rare and never breaks the loop.

### 5.4 Transcript rendering and persistence

The turn renders as two parts, mirroring the existing reasoning / "Denken"
block in `ChatMessage.tsx`:

- **The spoken summary → a collapsed, replayable block.** The inner `<speak>`
  text becomes a collapsed `<details>`-style block (same pattern as the
  reasoning block, `ChatMessage.tsx` ~lines 236–302) whose summary line carries
  a **play button**. Pressing play **replays the stored audio blob** of the
  spoken region (reuse the `AudioTurn` player / `ReplayIcon`); expanding the
  block shows the **summary text**. If the blob is gone (retention), the block
  still expands to the text and shows the not-stored state (reuse
  `AudioNotStoredTurn`); re-synthesis-on-replay is a later nicety (phase 3).
- **The supplementary detail → the normal answer body**, rendered as usual
  below the block.
- **Default open/closed state:** the block is **collapsed by default when there
  is a detail body** (you read the detail, expand/play the summary on demand),
  and **expanded by default for a short answer with no detail body** (so the
  summary text is visible without a click — you heard it, you can also read it).
- **Tag stripping / persistence.** The `<speak …>` / `</speak>` tags are
  **stripped** from both the displayed and the **persisted** text. The stored
  assistant message carries the clean full text (summary + detail, no markup)
  plus the spoken region's audio blob and the metadata needed to render the
  block (which span of the text was the spoken summary) — one assistant turn,
  as in phase 1's one-turn commit. **No `<speak>` markup is ever persisted or
  fed back into later LLM context or a strict upstream.**
- **Live stream, no tag flash (my call).** Tags are stripped from the
  **live-streamed deltas** too, not only the committed text. (Builder-check
  correction: `observeDelta` → `run.publish` currently emits the **raw** delta,
  so without this the user would briefly see `<speak lang="…">` in the live
  transcript.) The scanner therefore also sits on the **publish path** and holds
  back a delta only across an open tag it is mid-parsing (a tag is short, so the
  added display latency is negligible), emitting clean text otherwise. The block
  itself appears on commit; during streaming the live text is simply clean.
- **No tag** → no collapsed block: render like phase 1 (full text + one audio
  player for the whole-answer audio).

Phase 3 generalizes this to **several** such blocks per answer (one per
`<speak>` region), synthesized and played back-to-back.

### 5.5 Concurrent two-leg flow (the chosen architecture)

Phase 1's voice run is strictly ordered: `runVoiceLLM` returns the whole answer,
then `generateSpeechTurn(answer)` synthesizes it (`chat_runs_voice.go`). Phase 2
overlaps the two legs:

- The LLM completion streams as usual. The scanner (§5.2) watches the stream.
- **At `</speak>`**, the orchestrator fires the single speech request with the
  **complete captured summary** + its language, and transitions to `speaking`
  (mic stays discarded — half-duplex, unchanged). The LLM keeps streaming the
  **detail** in the background.
- **Commit when both legs are done**: the one assistant turn is committed once
  the LLM completion has finished (full text: summary + detail, tags stripped)
  **and** the speech audio has been captured/stored — reusing phase 1's one-turn
  commit (text + audio blob) and the deferred-commit / CAS arbitration. The
  continuation/cancel window lives in `thinking`, **before** `</speak>`, so it is
  unaffected; once speech has fired the turn is past the cancel window.
- **Mic re-open is governed by the speech drain** (phase-1 playout-aware drain),
  **not** by the LLM leg. The detail text is pure text and normally finishes
  before the summary audio drains; the commit waits for the LLM leg regardless.
- **Error paths (degrade, one typed error → idle, mic re-enabled — phase 1):**
  - LLM fails **before** `</speak>` (no summary yet) → no speech fired; same as a
    phase-1 LLM failure.
  - LLM fails **after** `</speak>` (summary spoken, detail cut off) → commit the
    turn with what exists (the summary text + its audio, detail truncated) and
    surface the degrade; do not leave a half-turn.
  - Speech leg fails → the text turn still commits (no audio block / not-stored
    state), degrade surfaced, as in phase 1.
- **No tag** → no early fire: the whole answer is spoken after it completes
  (phase-1 ordered flow, the fallback path).

The exact goroutine/channel orchestration and where the two completions join is
plan-level (validate against `executeVoiceRun`, the CAS `claimCommit`/cancel
seam, and `finishRunWithParts`).

### 5.6 Fallbacks and robustness

- **No tag** → speak the whole answer (phase-1 behaviour).
- **Unclosed `<speak>` at stream end** → speak what was captured inside it (the
  summary was cut off but is still spoken); the full text still renders.
- **Malformed / unparseable tag** → treat the answer as untagged (speak the
  whole answer). Never emit an error turn for a tag problem.
- **Empty `<speak></speak>`** → nothing to speak; fall through to speaking the
  whole answer (avoid a silent turn).

## 6. The default voice system prompt (draft — my call, for review)

A first draft the builder refines; the user can see and override it:

> You are in a spoken conversation. Begin every answer with ONE short, natural
> spoken summary of the essential point, wrapped in a single
> `<speak lang="…">…</speak>` tag, using the language the user is speaking (set
> `lang` to its two-letter code). Keep what is inside `<speak>` brief enough to
> listen to — a sentence or two, the gist only. After the closing `</speak>`,
> write the fuller answer (detail, lists, code, caveats) as normal text; that
> part is shown but not spoken. Do not put anything before the `<speak>` tag.

(Wording is a builder/UX detail; the behaviour it encodes is the spec.)

## 7. What is left to the builder (plan-level)

- **Where the scanner lives**: inside the voice run's streaming path
  (`chat_runs_voice.go` `executeVoiceRun` / `voiceSpeechPrep`). It feeds two
  consumers — the **speech leg** (the complete first `<speak>` region, or the
  whole answer if none) and the **publish/display path** (`observeDelta` →
  `run.publish`), which must emit tag-stripped deltas (§5.4 live-stream call).
  Validate against how phase 1 feeds the whole answer to the speech relay and
  publishes raw deltas today.
- **Prompt injection point**: the voice system prompt becomes the voice run's
  **system message** where `VoiceRunHistory` today passes `""` to
  `buildAPIHistory` (`service_chats.go` ~1244). No composition with a thread
  prompt (there is none on voice runs).
- **Settings plumbing**: the backend **reads** `voice_chat.system_prompt` from
  the stored chat doc at run time (§5.1 — not via the ticket, not via
  `PrepareChatRun`, which the voice run bypasses); the existing `PrepareChatRun`
  `voice_chat`-blob lift already carries the field across text sends. Frontend:
  the field + save + i18n, mirroring the #319/#320 picker persistence.
- **Transcript block (frontend)**: build the collapsed spoken-summary block by
  composing the existing reasoning `<details>` pattern (`ChatMessage.tsx`
  ~236–302) with the `AudioTurn` player / `ReplayIcon` and the
  `AudioNotStoredTurn` fallback. Decide the exact summary-line label + play
  affordance; validate it collapses/expands like the reasoning block and
  replays the stored blob.
- **Persistence shape**: how the one committed assistant turn records the split
  so the block re-renders on reload — the simplest being the spoken summary is
  the leading span of the clean text (the prompt forbids text before `<speak>`),
  so a stored summary length/offset (or the summary text as its own field)
  plus the existing per-turn audio blob suffices. No new agent field; rides the
  chat document. The plan validates against `finishRunWithParts` /
  `CommitAssistant` / `SaveChatWithBlobs`.
- **ADR / docs**: extend ADR-078 (or a short ADR-079) + the runtime view, the
  config/data-model reference (`settings.voice_chat.system_prompt`), and
  risks, for the tag-extraction + per-answer-language + spoken-block behaviour.
- **Tests**: stream-scanner unit tests (tag across token boundaries, no tag,
  unclosed, malformed, empty, multiple regions → first only); the
  language→speech-request wiring + the capability-aware omit; the settings
  lift (stored wins over body; survives send/switch); frontend field.

## 8. Phase boundary

| | Phase 2 (this) | Phase 3 (#315) | Phase 4 (#316) |
|---|---|---|---|
| Spoken regions | exactly one | multiple, sequential | — |
| Transcript | one collapsed, replayable block (spoken summary) + detail body | several such blocks, one per region | — |
| Language | per-answer via `lang` | (inherited) | — |
| Interrupt TTS | no (phase-1 cancel only) | no | barge-in |

## 9. Decisions

### 9.0 Decided: concurrent (fire speech at `</speak>`)

**Chosen (user, concurrent).** The speech request fires the moment `</speak>`
is seen, while the LLM keeps generating the detail in the background — you hear
the gist early, the whole point of a short spoken summary, and it matches the
phase-1 low-latency goal. The two legs overlap (§5.5); the ordered phase-1 flow
and its error path are restructured for the overlap, but the continuation/cancel
window closes before `</speak>`, so the CAS commit/cancel arbitration is largely
untouched. (The rejected alternative was sequential — finish the whole answer,
then speak — simpler but first-audio waits for the full generation.)

### 9.1 My calls (check these while reading)

1. **Prompt is per-thread** (in `settings.voice_chat`), pre-filled default,
   read server-side at run time (§5.1) — consistent with the pickers. (You can
   still ask for a global default instead; that is a different data model.)
2. **Prompt is the voice run's sole system message**, voice runs only — the
   thread's typed-chat system prompt does not reach voice runs today and stays
   that way (builder-check corrected my earlier "augment" wording).
3. **One `<speak>` region in phase 2**; later regions are plain text until
   phase 3.
4. **The one speech request carries the complete summary** captured at
   `</speak>` (the endpoint takes whole input, not a token feed); it fires
   **concurrently** at `</speak>` (decided, §9.0 / §5.5).
5. **Language is best-effort + capability-aware**: pass it through when the TTS
   model accepts `language`, otherwise omit (native voice language); never
   fail the turn on a language the voice can't do.
6. **Tags stripped from persisted, displayed, AND live-streamed text** — the
   scanner also sits on the delta publish path so no `<speak>` markup flashes in
   the live transcript (builder-check); none is ever stored or sent upstream.
7. **No-tag / malformed / empty-tag → speak the whole answer** (safe
   phase-1 fallback), unclosed → speak the captured part.
8. **The spoken region renders as one collapsed, replayable block** in phase 2
   (play replays the stored blob; expand reads the summary), mirroring the
   reasoning "Denken" block — pulled forward from the original phase-3 sketch at
   your request. Only the detail renders as normal body text. Several blocks
   per answer stays phase 3.
9. **Blob-gone on replay** (retention deleted it): the block still expands to
   the text and shows the not-stored state (`AudioNotStoredTurn`); no
   re-synthesis in phase 2.
10. **Answer with no detail** (everything was inside `<speak>`): render just the
    block, **expanded by default** so the short answer's text is visible (play
    still replays it); with a detail body present the block is collapsed by
    default (§5.4, at your request).

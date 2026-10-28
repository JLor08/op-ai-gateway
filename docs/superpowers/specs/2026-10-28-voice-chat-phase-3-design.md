<!-- SPDX-License-Identifier: AGPL-3.0-only -->
<!-- Copyright (C) 2026 OnPrem AI Gateway contributors -->

# Voice Chat — Phase 3 design: multiple spoken sections, playback queue + controls, loaded-model UX

Branch-local working doc (removed before the PR, per `AGENTS.md`). Issue
**#315**, phase 3 of the Voice Chat epic **#312**. Builds on phase 1 (#313),
the model/voice pickers (#319/#320), phase 2 (#314/#321: the `<speak lang>`
scanner, the editable voice system prompt, per-answer language, the single
collapsed replayable block, the concurrent two-leg flow + `played_through`
drain), and the phase-2 follow-ups (#322). Canonical description: `docs/architecture/`
+ ADR-078/079; this spec is the design to plan from.

## 1. Intent and why

Phase 2 speaks **one** short summary and renders it as a collapsed replayable
block. For a **long** answer, cramming the essence into one block loses
nuance. Phase 3 lets the model emit **several** `<speak>` sections (roughly one
per major point/paragraph), each a short spoken summary of its part. The spoken
output stays **summarizing** — not the whole answer — but a long answer gets a
**fuller, slightly more detailed, and therefore slightly longer** spoken
summary, delivered as several sections that **play back-to-back** and start
early (section 1 speaks while the LLM is still writing section 2). Each section
is its own collapsed, replayable block in the transcript.

Phase 3 also adds the playback and mic controls a longer spoken answer needs,
and makes the model pickers show what is loaded.

## 2. Goals / non-goals / anti-goals

**Goals**

- **Multiple `<speak>` sections per answer.** Still summarizing (the essence,
  not the full text); a long answer may carry several short section-summaries,
  a short answer stays one section (phase-2 behaviour). Driven by the
  (editable) default voice system prompt.
- **Sequential synthesis + gapless back-to-back playback**, low
  time-to-first-audio: a section is synthesized as its `</speak>` closes while
  the LLM keeps generating; a section that finishes early **waits its turn** in
  an ordered playout queue. The mic stays closed across the whole queue.
- **Per-section transcript blocks**: each spoken section renders as a collapsed,
  replayable block (the phase-2 block pattern) **at its position** in the
  answer; non-spoken text around them is the body. Each block replays its
  stored audio blob; when the blob is gone (retention) it shows **text-only**
  (phase-2 behaviour). Re-synthesis-on-replay is **deferred to #324** (it needs
  a new non-committing portal synth endpoint — see §5.7 Q4).
- **Playback controls** for the spoken answer: pause/resume, mute, stop
  (→ reopen the mic / back to listening).
- **Mic-mute toggle**: the user can mute their own microphone (the session
  stays on but does not listen) independently of the half-duplex auto-mute.
- **Combined system prompt (b)**: a voice run's system message is the thread's
  normal system prompt **plus** the voice system prompt (persona/context first,
  voice `<speak>`/summary formatting rules on top).
- **Loaded-model UX**: the ASR and TTS pickers show a green "loaded" dot (as the
  main model picker does), and the Automatic pick **prefers loaded models**.
- **Collapse** the voice system-prompt field (collapsed by default, like the
  settings) so it doesn't eat vertical space.

**Non-goals (later / out of scope)**

- Voice **barge-in** (interrupting TTS by *speaking*) stays phase 4 (#316). The
  phase-3 **stop button** is a deliberate, user-initiated, button interruption —
  not voice-activated — so it needs no echo/VAD handling.
- Changing the ASR/live-transcript path, billing, or the co-load gate's model
  set (phase 3 only *prefers* loaded ones in the auto-pick and *shows* loaded
  state).

**Anti-goals (a failure even if it "works")**

- Speaking the **whole** answer section-by-section (it must stay a summary, just
  possibly several short sections).
- Sections playing **out of order** or **overlapping**, or a gap/echo exposure:
  the mic must never reopen while any queued section is still playing (the
  half-duplex guarantee holds across the whole multi-section playout).
- `<speak>` markup leaking into displayed/persisted text or upstream (phase-2
  invariant, preserved).
- A control that strands the loop (e.g. pause that never resumes and never
  reopens the mic; stop that leaves the mic shut).

## 3. Constraints

- **No ServerAgent bump, no migration.** The multi-section turn rides the
  schema-free chat document (an array of sections, generalizing phase 2's
  single `spoken_summary`/`spoken_summary_start`/audio blob).
- **Preserve the half-duplex loop guarantee, the playout-aware drain
  (floor/ceiling + `played_through` counter), and the CAS commit/cancel
  arbitration** — generalized from one audio segment to an ordered queue of N.
- **Reuse** phase-2's scanner (extend first-region-only → all regions), the
  collapsed-block UI, the audio player/queue, the co-load/loaded-status signal
  the main picker uses, and #282 per-answer language (now per section).
- Repo-facing text in **English**; no LAN host names; no attribution lines.

## 4. The experience

A long answer streams in. The model emits, e.g.:
`…intro… <speak lang="de">S1</speak> …detail A… <speak lang="de">S2</speak> …detail B…`.
- You **hear S1** almost immediately (it fires at its `</speak>` while the LLM
  writes detail A + S2), then **S2 back-to-back** once S1 finishes playing
  (S2 waits in the queue if it was ready early). The mic is closed throughout.
- The **transcript** shows the full answer; S1 and S2 are collapsed replayable
  blocks at their spots, the rest is normal body text.
- While it speaks you can **pause/resume**, **mute**, or **stop** (stop → it
  reopens the mic so you can talk). A separate **mic-mute** toggle lets you
  silence your own mic any time.
- The ASR/TTS pickers show which models are **loaded** (green dot); Automatic
  prefers a loaded one.

## 5. Design

### 5.1 Multiple-section scanner + synthesis pipeline

- **Scanner (extend phase 2).** The phase-2 scanner captures only the first
  `<speak>` region. Phase 3: capture **every** region, each with its text,
  `lang`, and rune offset in the clean text. The clean text (tags stripped) and
  the live-delta stripping are unchanged; it now yields a **list** of sections
  and signals **each** region's close (not just the first).
- **Pipeline.** On each region close, start that section's speech leg (reuse the
  phase-2 concurrent fire-at-`</speak>`), **in order**. Sections are synthesized
  as they close (overlapping the LLM), but their audio is relayed/played **in
  document order**; a later section that finishes first **waits** (see §5.2). The
  LLM keeps streaming; the turn commits when the LLM completes **and** all
  section speech legs are done.
- **No tag** → phase-1/2 fallback (speak the whole answer; one implicit
  section). One tag → exactly phase 2.
- **Per-section language** rides #282 as in phase 2 (capability-aware omit).

### 5.2 Ordered gapless playout queue (client) + controls

- **Queue.** The client enqueues each section's audio keyed by section index and
  plays them **strictly in order, gapless**; a section whose audio arrived early
  waits until the previous section finishes. (Generalize the phase-1
  `StreamingPcmPlayer`: today it's one continuous stream; phase 3 needs ordered
  per-section segments so each is individually replayable AND they play
  back-to-back live.)
- **Server relay order.** The server relays section audio in index order (and
  tags each `audio_delta` with its section index) so the client can both play
  in order and attribute audio to the right block. The `played_through` drain
  counter (phase-2 follow-up) generalizes to "all sections relayed and played".
- **Mic across the whole queue.** The half-duplex drain ends (mic reopens) only
  after the **last** section's audio has played out (floor = sum of sections'
  real-time durations; ceiling as today; `played_through` honored per §5.2).
- **Controls** (shown while speaking):
  - **Pause/Resume** — halt/continue audio AND the drain timer (paused time
    excluded from floor/ceiling); mic stays closed while paused.
  - **Mute** — silence output; the timeline continues and the turn completes
    normally (the drain still advances).
  - **Stop** — stop the spoken answer, drop the rest of the queue, and **reopen
    the mic → listening** (a user-initiated, button interruption; server
    transitions `speaking`→`listening`, distinct from phase-4 voice barge-in).
  - **Mic-mute toggle** — mute the user's own mic: the session stays on but no
    turns are detected (capture paused / ASR not fed) until unmuted;
    independent of the auto-mute during `speaking`.

### 5.3 Transcript: multiple blocks + re-synth

- **Multiple blocks.** Each spoken section renders as a collapsed replayable
  block (phase-2 pattern) **at its position** in the answer text; the body is the
  full text with each section's span rendered as its block (generalize phase-2's
  single-span body derivation to N spans by offset). Open/collapsed default per
  phase-2 rule (collapsed when there is body, open for a bare one).
- **Blob gone → text-only (re-synth deferred to #324).** When a block's stored
  blob has been deleted by retention, the block shows its **text only** (the
  phase-2 not-stored behaviour). Re-synthesis-on-replay is **out of scope for
  phase 3** — it needs a new non-committing session-cookie portal synth endpoint
  (the only portal synth path today commits a chat turn; `/v1/audio/speech` is
  bearer-auth), filed as **#324**.

### 5.4 Combined system prompt (decision b)

- A voice run's system message = **thread system prompt + voice system prompt**
  (thread persona/context first, then the voice `<speak>`-sections/summary
  formatting rules). The voice rules govern *form*; the thread prompt governs
  *persona/context*.
- **Sourcing (must verify — plan/builder):** the voice run reads server-side
  from the stored chat doc (`VoiceRunHistory`). The thread's normal system
  prompt is today a per-*request* client setting (`req.Settings.SystemPrompt`),
  not necessarily in the stored doc. The plan must confirm the thread prompt is
  available to the voice run — either reliably present in `doc.Settings`, or
  carried on the voice-chat connection/ticket — before combining. Keep it
  voice-run-only; typed runs are unchanged.
- The default voice prompt (editable, phase 2) is updated to describe the
  multiple-sections behaviour.

### 5.5 Persistence (multiple sections per turn)

- One assistant turn stores an **ordered list of spoken sections**, each with
  its text span (rune offset + length, generalizing `spoken_summary_start`) and
  its audio blob reference; plus the full clean answer text. Schema-free, no
  migration, no ServerAgent bump. The phase-2 single-section shape is the N=1
  case (keep back-compat reading of old turns).

### 5.6 Loaded-model UX + collapse

- **Green "loaded" dot** in the ASR and TTS pickers, reusing the loaded-status
  signal the main model picker uses. **Automatic** prefers a loaded candidate
  (falls back to the current auto-pick when none is loaded).
- **Collapse** the voice system-prompt field into a collapsed-by-default
  section (like the settings), so it doesn't take vertical space.

### 5.7 Builder-check resolutions

The design builder-check grounded these against the phase-1/2 code; recording
the resolved decisions (details in §9):

- **Q1 — Playout = Model A (server-serialized single stream).** The server
  relays sections **in index order as one continuous seq stream** (holding
  section k+1's relay until section k's speech leg finishes = "early-ready
  waits", server-side), tagging each `audio_delta` with a **section index used
  only to attribute live audio to the right transcript block**. This reuses the
  existing `StreamingPcmPlayer`, the drain floor (Σ section durations, already
  summed by `accountPlayback`) and the `played_through` counter nearly
  unchanged. (Model B — a client-side per-section queue — was rejected: a
  frontend rewrite with a redefined counter for no UX gain.) **Per-block REPLAY
  is a separate path** from the live player: it plays the stored per-section WAV
  blobs via `AudioTurn`, independent of the live stream. Live playout and
  stored-blob replay are treated as distinct.
- **Q3 — Combined prompt source (premise corrected).** The thread's normal
  system prompt **is** in the stored doc at `doc.Settings.system_prompt`
  (`PrepareChatRun` marshals `req.Settings` into `doc.Settings` on every typed
  run, and the frontend autosave persists it). So combining is done entirely in
  `voiceSystemPrompt`/`VoiceRunHistory` (which already reads `doc.Settings`) —
  **no ticket/endpoint change**. Decision (a): read the stored doc and have the
  client **flush its debounced save before minting the ticket** (the
  `flushSave`-in-`begin()` added in phase-2 follow-up E already does this), so an
  edit made just before starting is not stale. A never-saved thread has an empty
  thread prompt (→ voice prompt alone, as today). Update the thread
  system-prompt field's "unused by a spoken conversation" copy.
- **Q2 — Control mechanisms (semantics were already decided §9.2).** New WS
  up-frames: **pause/resume are server-tracked** (the server shifts
  `firstAudioAt` / extends the floor+ceiling by the paused interval, so the mic
  cannot reopen over paused audio — a client-only pause could not keep the
  server floor honest); **stop** is a new up-frame → server `speaking`→
  `listening`, committing the audio produced so far (the committed blobs are the
  full synthesized sections regardless of playback position); **mic-mute is
  client-side** (pause mic capture / stop sending PCM — the server then simply
  detects no turns), with the indicator driven client-side. The plan resolves
  the **stop-vs-commit race** (stop reopening the mic while the speculative run
  is still committing under `WithoutCancel` and holding its per-chat
  `reserveRun` slot): the recommended approach is to let the commit finish and
  have `reserveRun`/the next turn tolerate the in-flight commit, rather than
  force-detach — validated against `chat_runs.go`.
- **Q4 — Re-synth-on-replay: DEFERRED (decision b), filed as #324.** The only
  portal synth path today commits a chat turn, and `/v1/audio/speech` is
  bearer-auth (not the portal session cookie), so re-synthesizing a single
  section's text without creating a turn requires a **new non-committing,
  session-cookie portal synth endpoint** — net-new surface. Decision: phase 3
  shows **text-only** when a blob is gone (phase-2 behaviour); the re-synth
  endpoint is a separate follow-up (**#324**).

## 6. Default voice system prompt (draft update — my call, for review)

Extend the phase-2 default so a long answer gets several sections:

> You are in a spoken conversation. Summarize the essentials as short spoken
> sections, each wrapped in its own `<speak lang="…">…</speak>` tag in the
> language the user is speaking. For a short answer use ONE section; for a long
> answer use several — roughly one per key point — each a brief spoken summary
> of that part, so the spoken sections together are a fuller but still concise
> summary. Keep each `<speak>` short enough to listen to. Write the fuller
> detail as normal text outside the tags; that text is shown but not spoken.

(Wording is a builder/UX detail; the behaviour it encodes is the spec. If (b)
combining is on, this is appended after the thread prompt.)

## 7. What is left to the builder (plan-level)

- Scanner: all-regions capture + per-region close signal + per-region offset
  (extend phase-2's `speakScanner`); keep chunk-invariance + tag stripping.
- Orchestration: N concurrent-but-ordered speech legs; commit when LLM + all
  legs done; generalize the CAS/commit, the drain (floor = Σ durations, the
  `played_through` counter), and the error paths from 1 to N sections.
- Client player: an ordered per-section queue (gapless, early-ready waits) that
  is also the source for per-block replay; the 4 controls (pause/resume, mute,
  stop) + their server signals (stop → `speaking`→`listening`); the mic-mute
  toggle (capture pause).
- Persistence shape for N sections (array on the turn), back-compat read of
  phase-2 single-section turns.
- Combined-prompt sourcing (verify the thread prompt reaches the voice run,
  §5.4) and compose it; update the default prompt.
- Re-synth-on-replay path for a single section (reuse the speech core, current
  voice settings).
- Loaded-status signal in the ASR/TTS pickers + auto-pick preference; the
  collapsed prompt-field UI.
- ADR update + `docs/architecture/` (runtime view, data model, api-surface,
  risks), + tests (scanner N-regions, ordered queue, each control + stop→mic,
  mic-mute, N-section persistence/commit under `-race`, combined prompt,
  re-synth, loaded-preference auto-pick).

## 8. Phase boundary

| | Phase 2 (shipped) | Phase 3 (this) | Phase 4 (#316) |
|---|---|---|---|
| Spoken regions | exactly one | several, ordered, gapless | — |
| Transcript | one block + body | N blocks at their spots + body | — |
| Playback controls | mute/abort tail | pause/resume, mute, stop→listen | — |
| Mic control | auto half-duplex | + user mic-mute toggle | voice barge-in |
| System prompt | voice-only (sole) | thread + voice (combined) | — |
| Re-synth gone blob | not-stored text | text-only (re-synth → #324) | — |

## 9. Decisions (my calls — check these while reading)

1. **Still summarizing**, several short sections for long answers → fuller but
   still concise, slightly longer spoken total (your call).
2. **Stop → reopen mic/listening**; pause halts audio+timer (mic closed); mute
   silences but continues; mic-mute is a separate self-mute toggle (your calls).
3. **Combine (b)**: thread prompt + voice prompt, voice-run-only; sourcing to be
   verified in the plan (§5.4).
4. **Blob gone → text-only** (phase-2 behaviour); re-synth-on-replay **deferred
   to #324** (needs a new non-committing portal synth endpoint).
5. **Sections play strictly in document order, gapless**; a server tag carries
   the section index; the drain floor is the sum of section durations.
6. **Auto-pick prefers loaded**; green dot reuses the main picker's loaded
   signal; prompt field collapsed by default.
7. **N=1 is exactly phase 2**; old single-section turns still render (back-compat).

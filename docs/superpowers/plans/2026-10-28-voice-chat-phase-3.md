# Voice Chat Phase 3 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a Voice Chat answer carry several spoken `<speak>` sections (still summarizing; more for long answers), synthesized in order and played back-to-back via an ordered gapless queue, each a collapsed replayable block; add playback controls (pause/resume, mute, stop→listen) + a mic-mute toggle; combine the thread + voice system prompts; show loaded models in the pickers and prefer loaded in auto-pick; collapse the prompt field.

**Architecture:** Extend the phase-2 single-region pipeline to N ordered regions. The scanner captures all regions; the voice run fires a speech leg per region (in document order) and commits one turn when the LLM and all legs finish. The server relays section audio in order as **one monotonic seq stream** (Model A), tagging each `audio_delta` with a section index for live block attribution; the existing player, drain floor (Σ section durations) and `played_through` counter generalize. Per-block replay stays a separate path (stored WAV blobs via `AudioTurn`). New WS up-frames carry pause/resume/stop; mic-mute is client-side.

**Tech Stack:** Go (gateway backend), React/TS/Vite (portal), existing `/v1` cores, SonarQube local gate.

**Spec:** `docs/superpowers/specs/2026-10-28-voice-chat-phase-3-design.md`

## Global Constraints

- **No ServerAgent bump, no migration.** Multi-section turns + combined prompt ride the schema-free chat document.
- **Preserve the half-duplex loop guarantee**, the playout-aware drain (floor/ceiling + `played_through`), and the single commit/cancel CAS (`claimCommit`) — generalized from 1 to N ordered audio segments. The mic must never reopen while any queued/paused section is still playing.
- **Reuse, don't reinvent:** the phase-2 scanner (extend), `StreamingPcmPlayer`, `AudioTurn` (replay), `generateSpeechTurn`/`launchVoiceSpeech` (per leg), the `ModelOption.loaded` signal + `SearchableSelect` dot, #282 per-answer language (now per section).
- **Re-synth-on-replay is OUT of scope** (deferred to #324): a gone blob shows text-only.
- `<speak>` markup never persisted/displayed(live or committed)/sent upstream (phase-2 invariant).
- Repo-facing text English; no LAN host names; no attribution lines. `DEFAULT_VOICE_SYSTEM_PROMPT` (frontend) must stay byte-identical to backend `defaultVoiceSystemPrompt` (a FE test reads the Go file).

## Review Focus

- **Section ordering / gaplessness:** a later section whose synthesis finishes first must NOT play or relay before the earlier one; the client's `seq===0 ⇒ new answer` reset must fire only once per turn (the server's single monotonic seq across legs guarantees this). → Tasks 1, 4, 7.
- **Mic never opens over playing/paused audio:** the drain floor = Σ section durations; pause must extend the server floor/ceiling by the paused interval; stop reopens the mic only after dropping the queue. → Tasks 4, 5.
- **Stop-vs-commit race:** a mid-answer stop while the speculative run is committing under `WithoutCancel` must not double-commit or strand the `reserveRun` slot. → Task 5.
- **N=1 / old turns still work:** a single-section answer renders and persists exactly as phase 2; turns committed before phase 3 still render. → Tasks 2, 6.
- **Combined prompt isolation:** the thread prompt reaches voice runs via the stored doc only; typed runs are unchanged; a never-saved thread → voice prompt alone. → Task 3.

---

### Task 1: Scanner — capture all `<speak>` regions

**Files:** Modify `gateway/backend/internal/gateway/voice_speak_scanner.go` (+ `_test.go`).

**Interfaces (produces, consumed by Tasks 4/6):**
- `type speakRegion struct { Text string; Lang string; RuneOffset int }`
- `func (s *speakScanner) Write(delta string) (clean string, closedIdx int, closed bool)` — `closedIdx` is the index of the region that closed on this delta (valid when `closed`).
- `func (s *speakScanner) Regions() []speakRegion` — ordered; valid at any point (in-progress region excluded until closed), final at `Close`.
- Keep `Close() (clean string)`, `Found() bool`. Keep back-compat `Summary()/SummaryStart()` returning the first region (N=1 path) OR drop them if no caller remains (Task 4 migrates callers).

**Behaviour:** drop the first-only gate in `openTag`; maintain a running clean-text **rune counter** so each region stamps its `RuneOffset`; `emit` appends to the current open region's builder; `closeTag` returns the closed region's index. Tag stripping, chunk-invariance, self-closing, `Close()` fragment handling (phase-2 follow-up) unchanged. `lang` verbatim per region.

- [ ] **Step 1: failing tests** — `TestSpeakScanner_MultipleRegions` (two/three regions: each text, lang, rune offset correct, in order; `closed`/`closedIdx` fire per region on the right delta); regions split across deltas; a region with multibyte text before/between (rune offsets); no-tag → zero regions; one-tag → one region (phase-2 equivalence). Keep all existing `TestSpeakScanner_*` green.
- [ ] **Step 2: run, verify fail.**
- [ ] **Step 3: implement** the ordered-regions state machine.
- [ ] **Step 4: run, verify pass** (`cd gateway/backend && go test ./internal/gateway/ -run TestSpeakScanner -v`).
- [ ] **Step 5: commit** `feat(voice): scanner captures all <speak> regions (#315)`.

---

### Task 2: Persist an ordered list of spoken sections

**Files:** Modify `gateway/backend/internal/portal/service_chats.go` (`AssistantTurn`, `buildAssistantMessage`); `gateway/backend/internal/gateway/chat_runs_voice.go` (`voiceContentParts`); test in `internal/portal`.

**Interfaces (produces, consumed by Tasks 4/6):**
- `AssistantTurn` gains `SpokenSections []SpokenSection` where `type SpokenSection struct { Text string; Start int; Len int }` (rune offset+length into the clean answer text; audio rides `ContentParts` in section order).
- `buildAssistantMessage` **always** writes a top-level `spoken_sections` array (each `{text?,start,len}`) for a voice turn with ≥1 section, and **stops** writing the scalar `spoken_summary`/`spoken_summary_start` going forward. The renderer (Task 6) reads `spoken_sections` when present and falls back to the scalar keys only for turns committed before phase 3.
- `voiceContentParts(answer string, audioBySection []json.RawMessage)` — each element is one section's audio array (a `json.RawMessage`, as `generateSpeechTurn` returns it); orders audio as `[text, sec0 audio…, sec1 audio…]`; nil/`null`/`[]` entries skipped, a non-array entry errors. (Implemented in Task 2; Task 4 passes per-section `audioParts` straight through.)

- [ ] **Step 1: failing tests** — commit a voice turn with 2 sections → stored message has `spoken_sections` with correct spans + audio parts in order; a 1-section turn still carries the phase-2 shape (back-compat); empty → no section keys. Reload via `parseChatDoc`.
- [ ] **Step 2-4: TDD** (`go test ./internal/portal/`).
- [ ] **Step 5: commit** `feat(voice): persist ordered spoken sections on the turn (#315)`.

---

### Task 3: Combine the thread + voice system prompts

**Files:** Modify `gateway/backend/internal/portal/service_chats.go` (`voiceSystemPrompt` ~L1285, its doc comment ~L1307); `defaultVoiceSystemPrompt` ~L1271 + `gateway/frontend/src/components/chat/chatDoc.ts` `DEFAULT_VOICE_SYSTEM_PROMPT` ~L963 (byte-identical); the thread system-prompt field's "unused by a spoken conversation" copy (frontend). Tests in `internal/portal` + the FE parity test.

**Interfaces/behaviour:**
- `voiceSystemPrompt(settings json.RawMessage) string` also decodes the top-level `system_prompt` member and composes **thread prompt (persona/context) + "\n\n" + voice prompt (form)**; voice prompt alone when the thread prompt is blank. Voice-run-only (`VoiceRunHistory` call site unchanged otherwise). Typed runs unchanged.
- Update `defaultVoiceSystemPrompt` to the multi-section wording (spec §6) and mirror it byte-for-byte in `DEFAULT_VOICE_SYSTEM_PROMPT`.

- [ ] **Step 1: failing tests** — a doc with top-level `system_prompt`="Persona." + `voice_chat.system_prompt`="Voice." → the voice run's sole system message is "Persona.\n\nVoice."; thread-only → "Persona.\n\n<default>"; neither → default; voice-only → voice. The thread prompt still does NOT leak as a *separate* message. The FE/BE default-parity test passes with the new wording.
- [ ] **Step 2-4: TDD.**
- [ ] **Step 5: commit** `feat(voice): combine thread + voice system prompts on voice runs (#315)`.

---

### Task 4: N ordered speech legs, serialized seq stream, section index, commit-all

**Files:** Modify `gateway/backend/internal/gateway/chat_runs_voice.go` (`executeVoiceRun`, `onSummaryClosed`, `launchVoiceSpeech`, `voiceTurnOutcome`, `finalizeVoiceCommit`, `voiceContentParts` wiring), `chat_runs_audio_stream.go` + `voice_chat_ws.go` `relayAudio`/`voiceDownMsg` (monotonic seq + `section` index). Tests in `internal/gateway` (mock-backed, `-race`).

**Interfaces/behaviour (consumes Task 1 scanner regions, Task 2 persist):**
- `onSummaryClosed` fires **per region** (with the closed index); launch one `launchVoiceSpeech` per region **in document order**, each into an ordered result set (slice of buffered channels or a fan-in keyed by index).
- `voiceTurnOutcome` carries an **ordered `[]sectionOutcome{ text string; start,len int; audioParts json.RawMessage; streamed bool }`** instead of the scalar summary fields.
- `executeVoiceRun` joins **all** legs before `finalizeVoiceCommit`; the `claimCommit` CAS stays the single arbitration point (fires after LLM + all legs). No-tag → one implicit whole-answer section (phase-1/2 fallback).
- **Serialize relay (Model A):** sections relay strictly in order, holding section k+1 until section k's leg finishes; the server renumbers audio into **one monotonic seq** across legs (a running base at the relay boundary in the voiceMachine, OR a start-seq on each `speechEventReader`); add a `section` field to the down audio frame (`voiceDownMsg`/`runAudio`) for live block attribution only. `accountPlayback` (floor) and `relayedAudioChunks`/`played_through` generalize unchanged.

- [ ] **Step 1: failing tests** — a 2-region answer: both legs fire in order, audio relays in order with one monotonic seq (client-visible seq never restarts at 0 mid-turn) + correct `section` tags; commit happens only after the LLM AND both legs finish; the committed turn has both sections' audio in order + the section spans (Task 2); an early-finishing section 2 still relays after section 1; no-tag fallback = whole answer; cancel mid-run persists nothing. Keep phase-2 single-section tests green. `-race`.
- [ ] **Step 2-4: TDD** (`go test ./internal/gateway/ -run TestVoice -race`).
- [ ] **Step 5: commit** `feat(voice): N ordered speech legs, serialized audio stream, commit all (#315)`.

---

### Task 5: Playback control up-frames — pause/resume, stop (server)

**Files:** Modify `gateway/backend/internal/gateway/voice_chat_ws.go` (`voiceUp*` consts + `parseVoiceUpControl` switch, `onControl` branches, the drain floor/ceiling math, `micOpen`/state transitions); validate against `chat_runs.go` (`claimCommit`/`reserveRun`). Tests in `internal/gateway` (`-race`).

**Interfaces/behaviour:**
- New up-frames `voiceUpPause="pause"`, `voiceUpResume="resume"`, `voiceUpStop="stop"` (add to the const block AND `parseVoiceUpControl`).
- **pause/resume (server-tracked):** on pause, stop the playback timer and record the pause instant; on resume, **shift `firstAudioAt` / extend the floor+ceiling by the paused interval** and re-arm, so the mic cannot reopen over paused audio. Mic stays closed while paused.
- **stop:** transition `speaking`/`draining`→`listening`, drop the remaining queue, reopen the mic. Commit the audio produced so far (committed blobs are the full synthesized sections regardless of playback position). Handle the **stop-vs-commit race**: do not force-detach a run that already won `claimCommit`; let it finish under `WithoutCancel` and have the next `beginAnswer`/`reserveRun` tolerate the in-flight commit (per spec §5.7 Q2; validate `retire`/`remove`/`reserveRun` in `chat_runs.go`).

- [ ] **Step 1: failing tests** — pause during speaking halts the drain (mic stays closed past the un-paused floor; after resume + paused-shifted floor, the mic reopens, not before); stop during speaking → state listening, mic reopens, the turn is committed with the sections synthesized so far, and a concurrent commit does not double-commit or strand the slot (seam-driven, `-race`); unknown/old frames still parse.
- [ ] **Step 2-4: TDD.**
- [ ] **Step 5: commit** `feat(voice): pause/resume + stop playback control frames (#315)`.

---

### Task 6: Transcript — render N collapsed blocks

**Files:** Modify `gateway/frontend/src/components/ChatMessage.tsx` (`bodyAt` → N spans; the block render; props), `gateway/frontend/src/components/chat/chatDoc.ts` (`ChatUiMessage` `spoken_sections`), the message→prop mapping (`Chat.tsx`). Tests `ChatMessage.test.tsx`.

**Interfaces/behaviour (consumes Task 2 shape):**
- `ChatUiMessage` gains `spoken_sections?: Array<{ text?: string; start: number; len: number }>` (audio per section via content parts); surface it to `ChatMessage` as `spokenSections?`.
- Split the full clean text by successive section spans into `[body0][block0][body1][block1]…`; render each section as its own collapsed `<details>` block (reuse the phase-2 pattern) with its own `AudioTurn` (that section's blob urls) for replay; blob gone → text-only (phase-2 `AudioNotStoredTurn`). Read `spoken_sections` when present; fall back to the scalar `spokenSummary`/`spokenSummaryStart` only for pre-phase-3 turns.

- [ ] **Step 1: failing tests** — a 2-section message renders two blocks at their spans with the body between/around; each block's player uses that section's audio; N=1 and old (scalar) turns render as phase 2; blob-gone section shows text-only; copy copies the full answer. Per locale.
- [ ] **Step 2-4: TDD** (`vitest run src/components/ChatMessage.test.tsx --retry=2`).
- [ ] **Step 5: commit** `feat(portal): render N collapsed spoken-section blocks (#315)`.

---

### Task 7: Client player — pause/resume

**Files:** Modify `gateway/frontend/src/components/chat/speechPlayback.ts` (`StreamingPcmPlayer`). Tests `speechPlayback.test.ts`.

**Interfaces/behaviour:**
- Add `pause(): void` and `resume(): void` to `StreamingPcmPlayer` that **halt and resume the playout timeline** (not just gain): stop the scheduled sources and remember the remaining queue + offset, then reschedule from the paused point on resume. Must not disturb other chats (do NOT suspend the shared `AudioContext`; manage this player's own scheduled nodes). `stop`/`setMuted`/`onEmpty`/`push` unchanged; `onEmpty` must not fire spuriously while paused.

- [ ] **Step 1: failing tests** — pause stops advancing playout and fires no `onEmpty`; resume continues from where it paused; pause/resume doesn't drop or reorder queued chunks; a second player (other chat) is unaffected.
- [ ] **Step 2-4: TDD.**
- [ ] **Step 5: commit** `feat(portal): pause/resume on the streaming PCM player (#315)`.

---

### Task 8: Client session — section index, control sends, mic-mute

**Files:** Modify `gateway/frontend/src/components/chat/voiceSession.ts` (`sendControl` union + senders; `audio_delta` `section`; mic-mute capture pause), `useVoiceChat.ts` (expose controls + state). Tests `voiceSession.test.ts`, `useVoiceChat.test.ts`.

**Interfaces/behaviour (consumes Task 4 section index, Task 5 frames, Task 7 player):**
- `sendControl` union gains `'pause'|'resume'|'stop'`; add `pause()/resume()/stop()` senders on `VoiceSession` (stop also stops local playback). `audio_delta` parsing reads the new `section` field (for live block attribution). mic-mute: a `setMicMuted(b)` that pauses mic capture / stops sending PCM (the server detects no turns); keep the single-monotonic-seq assumption (seq===0 resets once per turn — unchanged by Model A).
- `useVoiceChat` `VoiceChatApi` gains `pause/resume/stop/toggleMicMute` + state (`paused`, `micMuted`); wire pause/resume to the player (Task 7) + the server frames; stop → server stop frame + local stop.

- [ ] **Step 1: failing tests** — pause/resume/stop send the right frames (and stop stops local playback); mic-mute pauses capture (no PCM sent) and unmute resumes; `section` is parsed and surfaced; existing playback_done/counter behaviour unchanged.
- [ ] **Step 2-4: TDD.**
- [ ] **Step 5: commit** `feat(portal): voice session pause/resume/stop + mic-mute + section index (#315)`.

---

### Task 9: Controls UI + mic-mute toggle

**Files:** Modify `gateway/frontend/src/components/VoiceChatControl.tsx` (a playback-controls row while `speaking`/`draining`, wired to `voice.pause/resume/mute/stop`; a mic-mute toggle; extend `detailText`), `i18n.ts` (labels de+en). Tests `VoiceChatControl.test.tsx`.

**Interfaces/behaviour (consumes Task 8):**
- A controls row shown while the answer plays: Pause/Resume toggle, Mute toggle (reuse the `AudioTailControls` mute pattern / `setMuted`), Stop (→ `voice.stop`, which reopens the mic). A **mic-mute** toggle available whenever the session is on. i18n keys for each (both locales).

- [ ] **Step 1: failing tests** — the controls appear while speaking and call the right `voice.*`; pause shows resume; stop calls `voice.stop`; mic-mute toggles `voice.toggleMicMute` and reflects state; hidden/disabled appropriately when idle/off. Per locale.
- [ ] **Step 2-4: TDD** (build + prettier + `vitest run src/components/VoiceChatControl.test.tsx --retry=2`).
- [ ] **Step 5: commit** `feat(portal): Voice Chat playback controls + mic-mute toggle (#315)`.

---

### Task 10: Loaded-model dot + auto-pick prefers loaded + collapse prompt field

**Files:** Modify `gateway/frontend/src/components/VoiceChatControl.tsx` (`VoiceModelPicker` options carry `loaded`/`loadedTitle`; collapse the system-prompt field), `gateway/frontend/src/components/chat/voiceModels.ts` (`autoVoiceModel` prefers a loaded candidate), `i18n.ts` if new copy. Tests `VoiceChatControl.test.tsx`, `voiceModels.test.ts`.

**Interfaces/behaviour:**
- Map each ASR/TTS option to `{ value, label, loaded: o.loaded, loadedTitle: o.loaded_on?.length ? … : t.chatModelLoaded }` (mirror `Chat.tsx`), so `SearchableSelect` shows the green dot.
- `autoVoiceModel(streaming, models?)` prefers the first candidate whose `ModelOption.loaded` is true, else `streaming[0]`; thread the loaded lookup through `effectiveVoiceModel`/`voiceLegsOf`.
- Wrap the voice system-prompt field + reset in a collapsed-by-default section (like the settings).

- [ ] **Step 1: failing tests** — the picker renders a loaded dot for a loaded model; auto-pick returns a loaded candidate over an earlier unloaded one (and falls back when none loaded); the prompt field is collapsed by default and expands.
- [ ] **Step 2-4: TDD.**
- [ ] **Step 5: commit** `feat(portal): loaded-model dot + loaded-preferring auto-pick + collapsed voice prompt (#315)`.

---

### Task 11: Documentation

**Files:** Modify `docs/architecture/` — ADR (extend ADR-079 or add ADR-080), `06-runtime-view.md` (the N-section pipeline + ordered queue + controls), `reference/data-model.md` (`spoken_sections`; combined prompt), `reference/api-surface.md` + `reference/openapi.yaml` (the `pause`/`resume`/`stop` up-frames + the `section` on `audio_delta`), `11-risks-and-technical-debt.md` (pause/stop drain interactions; re-synth deferred → #324). Test `make lint-docs`.

- [ ] **Step 1: update docs** (behaviour, not line-by-line); cross-link.
- [ ] **Step 2: `make lint-docs`** → OK.
- [ ] **Step 3: commit** `docs(voice-chat): phase 3 — multiple spoken sections, controls, combined prompt (#315)`.

---

## Final verification (before the PR)

- `golangci-lint` fmt+run (both modules) 0; `make test-go` with the Postgres leg (`internal/portal` serial) + the voice suite under `-race`; frontend `npm run build` + `prettier --check` + `vitest --retry=2`; `make lint-docs`.
- `make sonar-gate && make sonar-findings && make sonar-branch-findings` → **0**.
- Remove the branch-local `docs/superpowers/` working files before opening the PR (per `AGENTS.md`).
- PR note: not run as a live spoken round-trip against a real audio.cpp; re-synth-on-replay deferred (#324).

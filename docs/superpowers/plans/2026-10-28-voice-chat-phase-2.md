# Voice Chat Phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the spoken part of a Voice Chat answer a short, self-contained summary carried in a backend-extracted `<speak lang>` region (spoken early, concurrently with the detail), shown as a collapsed replayable block, driven by an editable per-thread voice system prompt, in the model's per-answer language.

**Architecture:** The phase-1 voice run (strictly ordered LLM→TTS) gains a streaming `<speak lang="…">…</speak>` scanner inside its SSE loop. The scanner strips tags from the published/buffered text and captures the first spoken region; the speech leg fires at `</speak>` while the LLM keeps streaming the detail (concurrent two-leg flow); the one assistant turn commits when both legs finish, recording the spoken-summary text. An editable voice system prompt (read server-side from the stored chat doc) becomes the voice run's sole system message. The transcript renders the spoken summary as a collapsed, replayable block (the reasoning-block pattern + the audio player) and the detail as the body.

**Tech Stack:** Go (gateway backend), React/TypeScript/Vite (portal frontend), existing `/v1` loopback cores, SonarQube local gate.

**Spec:** `docs/superpowers/specs/2026-10-28-voice-chat-phase-2-design.md`

## Global Constraints

- **No ServerAgent bump, no migration.** Ride the schema-free chat document; `settings.voice_chat` and the assistant turn's new field are plain chat-doc JSON.
- **Voice-run-only.** The system prompt and `<speak>` extraction live on the `chatRunKindVoice` path; the normal text run path is untouched.
- **Reuse, don't reinvent.** Per-answer language rides #282's existing conditional `body["language"]` (`chat_runs_audio.go:446`) via `ChatAudioSettings.Language` (`service_chats.go:577`); capability is the cached `portalSpeechLanguage` lookup (`portal_model_speech_language.go:45`), never a per-request probe. The spoken block reuses the reasoning `<details>` pattern (`ChatMessage.tsx:236-305`) + `AudioTurn` + `AudioNotStoredTurn`.
- **No `<speak>` markup is ever persisted, displayed (live or committed), or sent to an LLM upstream.**
- **Repo-facing text (code comments, commits, docs) in English.** No LAN host names anywhere in repo text.
- **No attribution lines** in commits.

## Review Focus

Input classes the spec implies that no task's happy-path test exercises; each gets a test in the named task.

- **Tag attribute robustness** — `lang='de'` (single quotes), surplus/unknown attributes, inner whitespace `<speak   lang="de" >`, mixed-case `<SPEAK Lang="DE">`. The scanner must still extract text + lang. → Task 1.
- **`lang` value pass-through** — `"DE"`, `"de-DE"`, `"deu"`, `lang=""`: whatever the tag carries reaches the speech `language` unchanged (the TTS model / cache decides), and `lang=""`/absent omits the param. → Task 3.
- **Tag not at the start** — the model emits text before `<speak>` despite the prompt: that leading text is detail, the summary is still the region, and the rendered body must exclude the summary wherever it sits. → Task 1 (capture), Task 7 (body excludes it).
- **Resume/cancel during the overlap** — the user speaks again (or cancels) while the summary audio plays and the LLM detail is still streaming: no double-commit, a cancelled run persists nothing. → Task 5.
- **Multiple `<speak>` regions** — the model emits two: only the first is spoken; the later one is plain text in the detail (no second block in phase 2). → Task 1 (first only), Task 7 (renders once).

---

### Task 1: `<speak lang>` stream scanner (pure, backend)

The streaming tag extractor, with no gateway wiring — easy to TDD exhaustively.

**Files:**
- Create: `gateway/backend/internal/gateway/voice_speak_scanner.go`
- Test: `gateway/backend/internal/gateway/voice_speak_scanner_test.go`

**Interfaces:**
- Produces (consumed by Task 5):
  - `type speakScanner struct { … }`
  - `func newSpeakScanner() *speakScanner`
  - `func (s *speakScanner) Write(delta string) (clean string, closed bool)` — feed a raw content delta; returns the **tag-stripped** text safe to publish (holding back only the bytes of a tag it is mid-parsing) and whether the **first** `<speak>` region just closed on this delta.
  - `func (s *speakScanner) Close() (clean string)` — flush at stream end; any bytes held back for an unterminated tag are emitted as plain text.
  - `func (s *speakScanner) Summary() (text string, lang string)` — the first region's captured text + its `lang` (the full captured text if the region was never closed).
  - `func (s *speakScanner) Found() bool` — whether any `<speak>` open tag was seen.

Behaviour: strips every `<speak …>` and `</speak>` from the clean stream (both summary and detail text pass through clean); captures only the **first** region; later `<speak>` tags are stripped but their text stays in the clean stream as plain detail; `lang` is read verbatim (not normalized); an unclosed first region's captured text is still the summary; an empty first region yields `text == ""`.

- [ ] **Step 1: Write the failing tests**

```go
// voice_speak_scanner_test.go
func TestSpeakScanner_WholeRegionOneWrite(t *testing.T) {
    s := newSpeakScanner()
    clean, closed := s.Write(`<speak lang="de">Kurz.</speak> Detail.`)
    // clean text keeps summary + detail, tags gone:
    assertEqual(t, clean, "Kurz. Detail.")
    assertTrue(t, closed)
    text, lang := s.Summary()
    assertEqual(t, text, "Kurz.")
    assertEqual(t, lang, "de")
    assertTrue(t, s.Found())
}

func TestSpeakScanner_TagSplitAcrossWrites(t *testing.T) {
    s := newSpeakScanner()
    var clean string
    for _, d := range []string{"<spea", `k lang="d`, `e">Hi`, "</spe", "ak> rest"} {
        c, _ := s.Write(d); clean += c
    }
    clean += s.Close()
    assertEqual(t, clean, "Hi rest")
    text, lang := s.Summary()
    assertEqual(t, text, "Hi"); assertEqual(t, lang, "de")
}

func TestSpeakScanner_NoTag(t *testing.T)            // Found()==false; clean == input; Summary text ""
func TestSpeakScanner_Unclosed(t *testing.T)         // "<speak lang=\"de\">partial" → Close(); Summary text "partial", Found true
func TestSpeakScanner_EmptyRegion(t *testing.T)      // "<speak lang=\"de\"></speak>x" → Summary text "", clean "x", closed true
func TestSpeakScanner_FirstRegionOnly(t *testing.T)  // two regions → Summary is the first; second region's text stays in clean
func TestSpeakScanner_AttributeRobustness(t *testing.T) // single quotes, extra attrs, inner spaces, mixed case → text+lang extracted  [Review Focus]
func TestSpeakScanner_LeadingTextBeforeTag(t *testing.T) // "pre <speak lang=\"de\">sum</speak>" → clean "pre sum", Summary "sum"  [Review Focus]
func TestSpeakScanner_NoLangAttr(t *testing.T)       // "<speak>sum</speak>" → Summary lang ""
```

- [ ] **Step 2: Run the tests, verify they fail**

Run: `cd gateway/backend && go test ./internal/gateway/ -run TestSpeakScanner -v`
Expected: FAIL (undefined: `newSpeakScanner`).

- [ ] **Step 3: Implement the scanner** in `voice_speak_scanner.go`

A small byte/rune state machine (outside / in-open-tag / in-summary / in-close-tag / done-first-region). Hold back only bytes while mid-tag; on an ambiguous partial (`<spea`) buffer until resolved or Close. Parse the open tag's `lang` attribute tolerantly (either quote, case-insensitive name, surplus attributes ignored). Implementer chooses the exact buffering structure.

- [ ] **Step 4: Run the tests, verify they pass**

Run: `cd gateway/backend && go test ./internal/gateway/ -run TestSpeakScanner -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/voice_speak_scanner.go gateway/backend/internal/gateway/voice_speak_scanner_test.go
git commit -m "feat(voice): streaming <speak lang> tag scanner (#314)"
```

---

### Task 2: Voice system prompt — default + server-side read

Make the voice run's history carry the voice system prompt (stored override or built-in default) as its sole system message.

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (`VoiceRunHistory` at :1244-1257; add a helper + the default const)
- Test: `gateway/backend/internal/portal/service_chats_voice_test.go` (or the existing voice test file)

**Interfaces:**
- Produces:
  - `const defaultVoiceSystemPrompt = "…"` — the spec §6 draft wording (language-agnostic; "use the language the user is speaking").
  - `func voiceSystemPrompt(settings json.RawMessage) string` — unmarshals `{ "voice_chat": { "system_prompt": string } }` from the chat doc settings; returns the trimmed override, or `defaultVoiceSystemPrompt` when absent/blank.
- Consumes: the existing `doc.Settings` already in hand at `service_chats.go:1256`, and `buildAPIHistory(systemPrompt, …)` at `:881`.

- [ ] **Step 1: Write the failing tests**

```go
func TestVoiceSystemPrompt_DefaultWhenAbsent(t *testing.T) {
    assertEqual(t, voiceSystemPrompt(nil), defaultVoiceSystemPrompt)
    assertEqual(t, voiceSystemPrompt(json.RawMessage(`{"voice_chat":{}}`)), defaultVoiceSystemPrompt)
    assertEqual(t, voiceSystemPrompt(json.RawMessage(`{"voice_chat":{"system_prompt":"  "}}`)), defaultVoiceSystemPrompt)
}
func TestVoiceSystemPrompt_Override(t *testing.T) {
    assertEqual(t, voiceSystemPrompt(json.RawMessage(`{"voice_chat":{"system_prompt":"Be terse."}}`)), "Be terse.")
}
func TestVoiceRunHistory_InjectsSystemMessage(t *testing.T) {
    // seed a chat doc with settings.voice_chat.system_prompt = "Be terse."
    // call VoiceRunHistory; assert history[0].Role == "system" and its content == "Be terse."
}
func TestVoiceRunHistory_DefaultSystemMessage(t *testing.T) {
    // doc with no voice_chat.system_prompt → history[0] is the default prompt (system role)
}
```

- [ ] **Step 2: Run, verify fail** — `cd gateway/backend && go test ./internal/portal/ -run 'VoiceSystemPrompt|VoiceRunHistory' -v` → FAIL.

- [ ] **Step 3: Implement** — add `defaultVoiceSystemPrompt` + `voiceSystemPrompt(...)`; at `service_chats.go:1257` change `buildAPIHistory("", doc.Messages)` to `buildAPIHistory(voiceSystemPrompt(doc.Settings), doc.Messages)`.

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal/service_chats.go gateway/backend/internal/portal/service_chats_voice_test.go
git commit -m "feat(voice): voice run system prompt — default + stored override (#314)"
```

---

### Task 3: Per-answer language plumbing + capability-aware omit

Carry the extracted `lang` to the speech leg, omitting it when the TTS model can't accept `language`.

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`voiceSpeechPrep` at :262 — add a language parameter)
- Create helper + test: in `gateway/backend/internal/gateway/chat_runs_voice.go` + `..._test.go`

**Interfaces:**
- Produces (consumed by Task 5):
  - `func effectiveSpeechLanguage(requested string, modelAccepts bool) string` — returns `requested` when it is non-empty **and** `modelAccepts`, else `""`.
  - `func voiceSpeechPrep(answer string, tts routing.Target, audio portal.ChatAudioSettings, language string) PrepareRunResult` — the existing function gains a `language` arg that sets `settingsAudio.Language` (empty leaves it unset, so `speechBodyFields` omits `body["language"]`, `chat_runs_audio.go:446`).
- Consumes: `s.portalSpeechLanguage(ctx, owner, ttsModel) (accepts bool, def string, err error)` (`portal_model_speech_language.go:45`) — the cached lookup Task 5 calls to get `modelAccepts`.

- [ ] **Step 1: Write the failing tests**

```go
func TestEffectiveSpeechLanguage(t *testing.T) {
    assertEqual(t, effectiveSpeechLanguage("de", true), "de")
    assertEqual(t, effectiveSpeechLanguage("de", false), "")  // model can't → omit  [Review Focus: pass-through]
    assertEqual(t, effectiveSpeechLanguage("", true), "")
    assertEqual(t, effectiveSpeechLanguage("de-DE", true), "de-DE") // verbatim, not normalized
}
func TestVoiceSpeechPrep_SetsLanguage(t *testing.T) {
    prep := voiceSpeechPrep("hi", someTTSTarget, portal.ChatAudioSettings{}, "de")
    assertEqual(t, prep.Settings.Audio.Language, "de")
}
func TestVoiceSpeechPrep_EmptyLanguageUnset(t *testing.T) {
    prep := voiceSpeechPrep("hi", someTTSTarget, portal.ChatAudioSettings{}, "")
    assertEqual(t, prep.Settings.Audio.Language, "")
}
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** — add `effectiveSpeechLanguage`; give `voiceSpeechPrep` the `language` parameter and set `settingsAudio.Language = language`. (Update the existing call at `chat_runs_voice.go:145` to pass `""` for now; Task 5 replaces it with the effective language.)

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/chat_runs_voice.go gateway/backend/internal/gateway/chat_runs_voice_test.go
git commit -m "feat(voice): per-answer speech language, capability-aware omit (#314)"
```

---

### Task 4: Persist the spoken-summary text on the assistant turn

Record which text was the spoken summary, so the block re-renders on reload. Phase-1 turns keep an empty value (no behaviour change).

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (`AssistantTurn` struct; `buildAssistantMessage` at :1152)
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`commitVoiceTurn` at :287 — add a `summary` param)
- Test: `gateway/backend/internal/portal/service_chats_voice_test.go`

**Interfaces:**
- Produces (consumed by Task 5 + Task 7):
  - `AssistantTurn.SpokenSummary string` (new field; omitempty on the persisted message, e.g. under a `spoken_summary` key).
  - `func (s *Server) commitVoiceTurn(ctx context.Context, owner auth.Token, run *ChatRun, pendingText, answer, summary string, audioParts json.RawMessage, streamed bool)` — the existing signature gains `summary`, set on `AssistantTurn.SpokenSummary`.
- Consumes: `CommitVoiceTurn` (`service_chats.go:1432`) / `buildAssistantMessage` (`:1152`) persist the field.

- [ ] **Step 1: Write the failing tests**

```go
func TestCommitVoiceTurn_PersistsSpokenSummary(t *testing.T) {
    // commit a voice turn with answer "Sum. Detail." and summary "Sum."
    // reload the chat doc; assert the assistant message carries spoken_summary == "Sum."
    // and its text content is the full "Sum. Detail."
}
func TestCommitVoiceTurn_EmptySummaryOmitted(t *testing.T) {
    // summary "" (no-tag fallback) → no spoken_summary key on the message (phase-1 shape)
}
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** — add the field; thread `summary` through `commitVoiceTurn` → `portal.AssistantTurn{… SpokenSummary: summary}` → `buildAssistantMessage`. Update `commitVoiceTurn`'s only caller (`chat_runs_voice.go:177`) to pass `""` for now (Task 5 passes the real summary).

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/portal/service_chats.go gateway/backend/internal/gateway/chat_runs_voice.go gateway/backend/internal/portal/service_chats_voice_test.go
git commit -m "feat(voice): persist the spoken-summary text on the assistant turn (#314)"
```

---

### Task 5: Concurrent two-leg flow + live-delta stripping

The integration task: run the scanner in the LLM SSE loop, publish tag-stripped deltas, fire the speech leg at `</speak>` while the LLM streams the detail, and commit both legs as one turn with the summary + effective language.

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_voice.go` (`executeVoiceRun` :116, `runVoiceLLM` :191)
- Test: `gateway/backend/internal/gateway/chat_runs_voice_test.go` (mock-backed, driven through the real `/v1` loopback cores, as phase 1's voice tests are)

**Interfaces:**
- Consumes: `newSpeakScanner` (Task 1); `effectiveSpeechLanguage` + `voiceSpeechPrep(…, language)` (Task 3); `commitVoiceTurn(…, summary, …)` (Task 4); `s.portalSpeechLanguage` (`portal_model_speech_language.go:45`); the CAS seam `run.claimCommit()` (`chat_runs.go:276`) and `s.voiceBeforeCommitClaim` (`server.go:485`); `run.publish` / `run.buffered` (`chat_runs.go:413/466`).
- Behaviour:
  - In `runVoiceLLM`'s SSE loop, feed each content delta through the scanner; publish the scanner's **clean** text (so `run.content` buffers the clean full text and no `<speak>` markup is fanned out). Reasoning deltas are unchanged.
  - When `Write` reports the first region **closed**, compute `lang := effectiveSpeechLanguage(scannerLang, accepts)` (accepts from `portalSpeechLanguage` for the TTS model) and launch `generateSpeechTurn(voiceSpeechPrep(summary, tts, audio, lang))` **concurrently**; the LLM loop keeps running.
  - On LLM completion, `answer = run.buffered()` (clean full text); join the speech goroutine.
  - Commit: `run.claimCommit()` then `commitVoiceTurn(…, answer, summary, audioParts, streamed)`.
  - **No tag** (`Found()==false` at loop end): fall back to phase-1 — synthesize the whole `answer`, commit with `summary == ""`.
  - Error paths per spec §5.5 (LLM before/after `</speak>`, speech leg failure).

- [ ] **Step 1: Write the failing tests**

```go
func TestVoiceRun_FiresSpeechAtCloseTag(t *testing.T)   // speech request observed BEFORE the LLM stream ends (ordering via a recording fake)
func TestVoiceRun_CommitsFullTextAndSummary(t *testing.T) // committed turn: content == full clean text; spoken_summary == region; audio present
func TestVoiceRun_NoTagSpeaksWholeAnswer(t *testing.T)   // no <speak> → whole answer synthesized, spoken_summary ""
func TestVoiceRun_LiveDeltasAreTagStripped(t *testing.T) // no published delta contains "<speak"  [anti-goal]
func TestVoiceRun_LanguageOmittedWhenModelCannot(t *testing.T) // model without language capability → speech body has no "language"
func TestVoiceRun_CancelDuringOverlapPersistsNothing(t *testing.T) // claimCancel wins in the overlap window → no commit  [Review Focus]
func TestVoiceRun_LLMFailsAfterCloseCommitsSummary(t *testing.T)   // detail cut off → turn committed with summary+audio, degrade surfaced
func TestVoiceRun_SpeechFailsCommitsTextOnly(t *testing.T)         // speech error → text committed, no audio, degrade surfaced
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement the concurrent flow.** Split `runVoiceLLM` so the scanner taps the SSE loop and signals close; restructure `executeVoiceRun` to launch the speech leg at close and join both before the single `claimCommit`+`commitVoiceTurn`. Keep the CAS/cancel seam intact (the cancel window closes before `</speak>`). Implementer chooses the goroutine/channel shape.

- [ ] **Step 4: Run, verify pass** — `cd gateway/backend && go test ./internal/gateway/ -run TestVoiceRun -v` and the voice `-race` subset.

- [ ] **Step 5: Commit**

```bash
git add gateway/backend/internal/gateway/chat_runs_voice.go gateway/backend/internal/gateway/chat_runs_voice_test.go
git commit -m "feat(voice): concurrent summary speech at </speak>, live tag stripping (#314)"
```

---

### Task 6: Frontend — editable voice system prompt

A pre-filled, per-thread system-prompt field beside the Voice Chat pickers; persisted under `settings.voice_chat.system_prompt`.

**Files:**
- Modify: `gateway/frontend/src/components/chat/chatDoc.ts` (`VoiceChatSettings` :957, `DEFAULT_VOICE_CHAT_SETTINGS` :963, `voiceChatSetting` :982, `voiceChatSettingsFrom` :1000)
- Modify: `gateway/frontend/src/api/chat.ts` (`ChatVoiceChatSettings` :136)
- Modify: `gateway/frontend/src/components/chat/voiceModels.ts` (add the prompt to `VoiceChatModels` :163)
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx` (state :769/seed :1371, `setVoiceSystemPrompt` near :2416, wire into `voiceChatModels` :2423)
- Modify: `gateway/frontend/src/components/VoiceChatControl.tsx` (`VoiceModelPickers` :364 — add a multiline field)
- Modify: `gateway/frontend/src/i18n.ts` (`chatVoiceSystemPrompt` + help, in `de` and `en`)
- Test: `gateway/frontend/src/components/chat/chatDoc.test.ts`, `VoiceChatControl.test.tsx`

**Interfaces:**
- `VoiceChatSettings` gains `systemPrompt: string`; the wire type `ChatVoiceChatSettings` gains `system_prompt?: string`.
- `const DEFAULT_VOICE_SYSTEM_PROMPT` (frontend copy of the backend `defaultVoiceSystemPrompt`, Task 2 — cross-referenced in a comment). The field pre-fills with it; `voiceChatSetting` persists `system_prompt` **only when non-empty and ≠ the default** (so the default stays code-owned; backend empty → default, Task 2).

- [ ] **Step 1: Write the failing tests**

```ts
// chatDoc.test.ts
it('persists a non-default system prompt under voice_chat.system_prompt', () => {
  const s = voiceChatSetting('', { asrModel:'', ttsModel:'', voice:'', systemPrompt:'Be terse.' })
  expect(s.voice_chat?.system_prompt).toBe('Be terse.')
})
it('omits the system prompt when it equals the default', () => {
  const s = voiceChatSetting('', { asrModel:'', ttsModel:'', voice:'', systemPrompt: DEFAULT_VOICE_SYSTEM_PROMPT })
  expect(s.voice_chat?.system_prompt).toBeUndefined()
})
it('seeds systemPrompt from the stored doc', () => {
  expect(voiceChatSettingsFrom({ system_prompt:'X' }).systemPrompt).toBe('X')
})
// VoiceChatControl.test.tsx
it('shows the prompt field pre-filled and calls set on edit', () => { /* … */ })
it('disables the prompt field while the conversation is live (locked)', () => { /* … */ })
```

- [ ] **Step 2: Run, verify fail** — `cd gateway/frontend && npx vitest run src/components/chat/chatDoc.test.ts src/components/VoiceChatControl.test.tsx`.

- [ ] **Step 3: Implement** — add the field end-to-end (store state + setter, wire type, chatDoc fragment with the default-omit rule, the multiline `TextField` in `VoiceModelPickers`, i18n keys in both `de` and `en`).

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components/chat/chatDoc.ts gateway/frontend/src/api/chat.ts gateway/frontend/src/components/chat/voiceModels.ts gateway/frontend/src/components/chat/ChatStore.tsx gateway/frontend/src/components/VoiceChatControl.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/components/chat/chatDoc.test.ts gateway/frontend/src/components/VoiceChatControl.test.tsx
git commit -m "feat(portal): editable per-thread Voice Chat system prompt (#314)"
```

---

### Task 7: Frontend — spoken-summary collapsed replayable block

Render the spoken summary as a collapsed, replayable block (expand to read, play to re-listen); the detail is the body. No-tag turns render as phase 1.

**Files:**
- Modify: `gateway/frontend/src/components/ChatMessage.tsx` (reasoning block pattern :236-305; audio usages :408-411; props :78-146)
- Modify: the message→`ChatMessage` prop mapping (surface `spokenSummary` from the stored message — locate in `ChatStore.tsx` / the chat renderer; validate during the task)
- Modify: `gateway/frontend/src/i18n.ts` (`chatVoiceSpokenSummary` label for the block summary line)
- Test: `gateway/frontend/src/components/ChatMessage.test.tsx` (or `Chat.test.tsx`)

**Interfaces:**
- Consumes: the persisted `spoken_summary` (Task 4), surfaced as `ChatMessage` prop `spokenSummary?: string`.
- Behaviour: when `spokenSummary` is non-empty, render a `<Box component="details">` (mirroring the reasoning block) whose `<summary>` line shows `t.chatVoiceSpokenSummary` + the `AudioTurn` player (the turn's audio IS the summary audio), expanding to the summary text; set `open` when the body is empty (short answer, no detail). The **body** = the message text with the summary removed. `AudioNotStoredTurn` still covers a retention-removed blob. When `spokenSummary` is empty → unchanged phase-1 rendering.

- [ ] **Step 1: Write the failing tests**

```tsx
it('renders a collapsed spoken-summary block with a player when spokenSummary is set', () => {})
it('omits the summary text from the body (shown only in the block)', () => {})
it('renders the block open when there is no detail body', () => {})          // short answer
it('excludes the summary even when it was not the leading text', () => {})   // [Review Focus: tag not at start]
it('renders exactly one block when the answer had two <speak> regions', () => {}) // [Review Focus: first only]
it('falls back to the phase-1 layout when spokenSummary is empty', () => {})
it('shows the not-stored state when the summary blob was retention-removed', () => {})
```

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** — add the prop + the block; derive the body by removing the summary from the text; wire `spokenSummary` through the message mapping; add the i18n label (de + en).

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit**

```bash
git add gateway/frontend/src/components/ChatMessage.tsx gateway/frontend/src/components/chat/ChatStore.tsx gateway/frontend/src/i18n.ts gateway/frontend/src/components/ChatMessage.test.tsx
git commit -m "feat(portal): collapsed replayable spoken-summary block (#314)"
```

---

### Task 8: Documentation

Record the phase-2 behaviour in the canonical docs; keep `check-docs` green.

**Files:**
- Modify: `docs/architecture/decisions/ADR-078-*.md` (extend; or add a short ADR-079 for the `<speak>`/system-prompt/per-answer-language behaviour — follow the repo's ADR convention)
- Modify: `docs/architecture/06-runtime-view.md` (the concurrent two-leg voice flow)
- Modify: `docs/architecture/reference/data-model.md` (`settings.voice_chat.system_prompt`; the assistant turn's `spoken_summary`)
- Modify: `docs/architecture/11-risks.md` (§11.4 — per-answer language best-effort; concurrent-leg commit)
- Test: `./scripts/check-docs.sh`

**Interfaces:** docs only; no code.

- [ ] **Step 1: Update the docs** listed above (behaviour, not implementation detail); cross-link the ADR from the runtime view and the data-model reference per the repo's link conventions.

- [ ] **Step 2: Verify docs consistency**

Run: `make lint-docs`
Expected: PASS (intra-repo links/anchors, architecture index, OpenAPI refs all resolve).

- [ ] **Step 3: Commit**

```bash
git add docs/architecture/
git commit -m "docs(voice-chat): phase 2 — speak-tag summaries, voice prompt, per-answer language (#314)"
```

---

## Final verification (before the PR)

Not a task — the pre-PR gate, run after all tasks (and the subagent-driven whole-branch review):

- `cd gateway/backend && golangci-lint run` and `cd server-agent && golangci-lint run` → 0.
- `make test-go` with the Postgres leg provisioned (store/commit tests need it); run serially if the shared container flakes under load.
- `cd gateway/frontend && npm run build && npx prettier --check . && npx vitest run --retry=2`.
- `make lint-docs`.
- `make sonar-gate && make sonar-findings && make sonar-branch-findings` → **branch-findings 0**.
- Remove the branch-local `docs/superpowers/` working files before opening the PR (per `AGENTS.md`).
- Note in the PR body: not run as a live spoken round-trip against a real audio.cpp (the automated tests can't inject live mic audio); an operator should confirm the real-model behaviour.

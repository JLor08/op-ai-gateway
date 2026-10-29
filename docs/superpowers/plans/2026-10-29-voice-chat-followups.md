# Voice Chat follow-ups (#326) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the Voice Chat automatic model pick from drifting as models load in the background, reconcile the saved voice against the model it was chosen for (in-session and across reloads), and two UI-clarity fixes (a "customised" marker on the collapsed voice system-prompt field, a persistent output mute with no layout jump).

**Architecture:** The pure selection rules stay in `voiceModels.ts` (unit-tested on their own); the runtime wiring is in `ChatStore.tsx`; persistence is the schema-free `voice_chat` thread settings in `chatDoc.ts`; the per-model voice list comes from `useVoiceChatVoices.ts`; the UI is `VoiceChatControl.tsx` (with the session mute already in `voiceSession.ts`). Option B: one additive persisted field, `voiceModel`, records the TTS model a saved voice was chosen for, so reconciliation has a reference across reloads.

**Tech Stack:** React + TypeScript (Vite), vitest.

**Spec:** `docs/superpowers/specs/2026-10-29-voice-chat-followups-design.md`

## Global Constraints
- Frontend only (`gateway/frontend`). No ServerAgent version bump, no backend change, no schema migration — `voiceModel` is an additive field on the schema-free `voice_chat` settings.
- Keep the pure rules in `voiceModels.ts` unit-testable in isolation, as today.
- `voiceChatSetting` must stay byte-identical for a thread that chose nothing (write a field only when it is set and non-default); never emit `voice_chat: {}`.
- English for all repo-facing text (code, comments, docs, commits, PR). Chat is German.
- Gates before PR: `npm run build` + `npx prettier --check .` + `npx vitest run --retry=2`; `make lint-docs`; SonarQube branch-findings = 0.
- No attribution lines in commits.

## Review Focus
- A thread loaded with a saved `voice` but **no** `voice_model` (pre-feature / old thread): reconciliation must not crash and must follow the rule (Task 1 + Task 4 tests).
- The effective TTS model's voice list still **loading or failed** when the model differs from `voiceModel`: the voice must **not** be cleared then (defer) — only a confirmed `ready` list drives the membership decision (Task 1 + Task 3).
- Rapid chat switching: the sticky automatic pick is per `chatId` and resets on chat change — it must not leak a previous chat's pick (Task 4).
- An explicit pick equal to the current automatic model must not be read as a drift that clears the voice (Task 1 `withTtsModel` fix + Task 4).
- The "customised" marker must compare the **trimmed** prompt to `DEFAULT_VOICE_SYSTEM_PROMPT` (Task 5).

---

### Task 1: Pure selection rules in `voiceModels.ts`

**Files:**
- Modify: `gateway/frontend/src/components/chat/voiceModels.ts`
- Test: `gateway/frontend/src/components/chat/voiceModels.test.ts`

**Interfaces:**
- Consumes: `ModelOption`, the existing `autoVoiceModel`, `effectiveVoiceModel`.
- Produces:
  - `stickyAutoVoiceModel(previous: string, streaming: readonly string[], models?: readonly ModelOption[]): string` — returns `previous` when it is non-empty and still in `streaming`; otherwise `autoVoiceModel(streaming, models)` (prefer-loaded resolution).
  - `type VoiceList = { status: 'loading' } | { status: 'failed' } | { status: 'ready'; voices: readonly string[] }` (exported; Task 3's hook returns it, Task 4 consumes it).
  - `reconcileVoice(args: { voice: string; voiceModel: string; effectiveModel: string; voices: VoiceList }): { voice: string; voiceModel: string } | null` — the reconciliation; `null` means "no change".
  - A corrected `withTtsModel(current: VoiceChatSettings, ttsModel: string, currentEffectiveTts: string, streaming: readonly string[], models?: readonly ModelOption[]): VoiceChatSettings` — it judges "same model would speak" as `currentEffectiveTts === effectiveVoiceModel(ttsModel, streaming, models)` (the caller passes the current effective/sticky TTS model), and on a real change clears both `voice` and `voiceModel` (see Step 5).

- [ ] **Step 1: Write failing tests for `stickyAutoVoiceModel`**

```ts
// keeps a still-streaming previous pick even when another model is now loaded
expect(stickyAutoVoiceModel('m1', ['m1', 'm2'], [{ id: 'm2', loaded: true } as ModelOption])).toBe('m1');
// re-resolves (prefer-loaded) when the previous pick no longer streams
expect(stickyAutoVoiceModel('m1', ['m2', 'm3'], [{ id: 'm3', loaded: true } as ModelOption])).toBe('m3');
// empty previous -> fresh resolution
expect(stickyAutoVoiceModel('', ['m2', 'm3'], [])).toBe('m2');
```

- [ ] **Step 2: Write failing tests for `reconcileVoice`**

```ts
const ready = (voices: string[]) => ({ status: 'ready', voices } as const);
// same attributed model -> no change (typed-not-in-list voice is kept)
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm1', voices: ready([]) })).toBeNull();
// model differs, voice in the new list -> keep + re-attribute
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm2', voices: ready(['alloy']) }))
  .toEqual({ voice: 'alloy', voiceModel: 'm2' });
// model differs, voice NOT in the new list -> clear to default
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm2', voices: ready(['nova']) }))
  .toEqual({ voice: '', voiceModel: '' });
// model differs but list not ready -> defer (no change)
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm2', voices: { status: 'loading' } })).toBeNull();
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: 'm2', voices: { status: 'failed' } })).toBeNull();
// no voice -> no change (even if voiceModel is stale, nothing to send)
expect(reconcileVoice({ voice: '', voiceModel: 'm1', effectiveModel: 'm2', voices: ready(['nova']) })).toBeNull();
// no effective model -> no change
expect(reconcileVoice({ voice: 'alloy', voiceModel: 'm1', effectiveModel: '', voices: ready(['nova']) })).toBeNull();
// pre-feature thread (voiceModel ''): treated as a differing model -> membership check
expect(reconcileVoice({ voice: 'alloy', voiceModel: '', effectiveModel: 'm2', voices: ready(['alloy']) }))
  .toEqual({ voice: 'alloy', voiceModel: 'm2' });
```

- [ ] **Step 3: Write failing tests for the `withTtsModel` fix**

```ts
// choosing the model that already speaks (the effective one) keeps the voice;
// choosing a different streaming model clears it. The "effective" model is passed in,
// not recomputed, so a sticky pick is honoured.
```
Pin: `withTtsModel` keeps `voice` when the chosen model resolves to the same **effective** model it is given, and clears it otherwise; add a case that would fail if it recomputed the automatic pock from scratch.

- [ ] **Step 4: Run the new tests, verify they fail**

Run: `npx vitest run src/components/chat/voiceModels.test.ts`
Expected: FAIL (new functions undefined / old `withTtsModel` behaviour).

- [ ] **Step 5: Implement**
  - `stickyAutoVoiceModel` as specified.
  - `VoiceList` union + `reconcileVoice` per the rules: no change when `voice===''`, `effectiveModel===''`, or `voiceModel===effectiveModel`; when the model differs, `ready` drives the membership check (in list → keep + re-attribute; not in list → clear both), and `loading`/`failed` defer (`null`).
  - Change `withTtsModel` to the signature above: it takes `currentEffectiveTts` and compares it to `effectiveVoiceModel(ttsModel, streaming, models)` instead of recomputing the current side with a fresh `autoVoiceModel`. Keep its "clear voice on a real model change" behaviour, and clear `voiceModel` together with `voice`.

- [ ] **Step 6: Run tests, verify pass**

Run: `npx vitest run src/components/chat/voiceModels.test.ts`
Expected: PASS.

- [ ] **Step 7: Commit** `feat(portal): sticky auto voice-model pick + voice reconciliation rules (#326)`

---

### Task 2: `voiceModel` field + persistence in `chatDoc.ts`

**Files:**
- Modify: `gateway/frontend/src/components/chat/chatDoc.ts` (the `VoiceChatSettings` type, `DEFAULT_VOICE_CHAT_SETTINGS`, `voiceChatSetting`, `voiceChatSettingsFrom`) and the wire type `ChatVoiceChatSettings` (add optional `voice_model?: string`).
- Test: `gateway/frontend/src/components/chat/chatDoc.test.ts` (or the existing chatDoc test file).

**Interfaces:**
- Produces: `VoiceChatSettings` gains `voiceModel: string` (default `''`); the `voice_chat` wire gains `voice_model?`.
- Consumes: `DEFAULT_VOICE_SYSTEM_PROMPT` (unchanged).

- [ ] **Step 1: Write failing tests**
  - `voiceChatSetting` writes `voice_model` only when BOTH `voice` and `voiceModel` are non-empty (trimmed); a voice with no model, or a model with no voice, writes no `voice_model`.
  - A thread that chose nothing still yields `{}` (byte-identical, no `voice_chat`).
  - `voiceChatSettingsFrom` reads `voice_model` (string → kept; missing/mistyped → `''`).
  - Round-trip: `voiceChatSettingsFrom(voiceChatSetting('', s).voice_chat)` preserves `voiceModel` when a voice+model are set.

- [ ] **Step 2: Run, verify fail** — `npx vitest run src/components/chat/chatDoc.test.ts`

- [ ] **Step 3: Implement** the field, default, `voiceChatSetting` (emit `voice_model` only alongside a `voice`), `voiceChatSettingsFrom` (read it), and the wire `voice_model?: string`.

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit** `feat(portal): persist voiceModel attribution for the voice-chat voice (#326)`

---

### Task 3: tri-state in `useVoiceChatVoices`

**Files:**
- Modify: `gateway/frontend/src/components/chat/useVoiceChatVoices.ts`
- Test: its test file (create `useVoiceChatVoices.test.ts` if none exists).

**Interfaces:**
- Produces: the hook returns `VoiceList` (from Task 1): `{ status: 'loading' }` while no model is resolved yet or a lookup for the asked model is in flight; `{ status: 'failed' }` on a rejected lookup; `{ status: 'ready'; voices }` on success (`voices` may be empty). `model === ''` resolves to `{ status: 'ready'; voices: [] }` (nothing to load).
- Consumes: `VoiceList` type from `voiceModels.ts`.

- [ ] **Step 1: Write failing tests** (fake `modelVoices`):
  - before resolve → `loading`; after resolve → `ready` with the list; a model with an empty list → `ready` with `[]`; a rejected lookup → `failed`; a result for a model no longer asked is ignored (stays `loading`/does not leak); `model===''` → `ready` with `[]`.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** the tri-state (hold `{ model, result: VoiceList }`; return the held result only when `held.model === model`, else `loading`).

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit** `refactor(portal): useVoiceChatVoices reports loading/ready/failed (#326)`

---

### Task 4: Wire the rules into `ChatStore.tsx`

**Files:**
- Modify: `gateway/frontend/src/components/chat/ChatStore.tsx`
- Test: the ChatStore test file(s).

**Interfaces:**
- Consumes: `stickyAutoVoiceModel`, `reconcileVoice`, `VoiceList`, the fixed `withTtsModel` (Task 1); `voiceModel` on `VoiceChatSettings` (Task 2); the `VoiceList` hook return (Task 3).

- [ ] **Step 1: Write failing tests** (ChatStore / hook tests) pinning the observable behaviour:
  - The effective TTS/ASR model does **not** change when a different model merely becomes loaded (sticky pick); it **does** change when the current one stops streaming.
  - On a forced TTS switch where the new model lists the saved voice → voice kept, `voiceModel` re-attributed; where it does not → voice cleared (and the ticket/`begin()` sends the default).
  - While the new model's voices are still `loading`/`failed`, the saved voice is **not** cleared and the default is sent meanwhile.
  - Switching the active chat resets the sticky pick (no leak between chats).
  - Picking a voice records the current effective TTS model as `voiceModel`; choosing a different speech model clears voice+`voiceModel` (via the fixed `withTtsModel` with the effective model).

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement the wiring**
  - Hold the sticky automatic pick per leg, keyed by `chatId` (a ref/map), seeded on first resolution and reset on chat change; feed it so the effective model is `explicit pick (if streaming) else stickyAutoVoiceModel(previous, streaming, chatModels)`, updating the stored "previous".
  - Compute the effective voice via `reconcileVoice` using `voiceSettings.voice`, `voiceSettings.voiceModel`, the effective TTS model, and the `VoiceList` from the hook; when it returns a change, apply it with `setVoiceSettings`, and send the reconciled voice in `begin()`.
  - `setVoiceVoice` records `voiceModel = effective TTS model`; `setVoiceTtsModel` calls the fixed `withTtsModel` with the effective model.
  - Adapt the existing `effectiveVoice` usage to the `VoiceList` (ready→voices, else none).

- [ ] **Step 4: Run, verify pass; run the broader frontend tests you touched.**

- [ ] **Step 5: Commit** `feat(portal): stable voice-chat model pick + voice reconciliation wiring (#326)`

---

### Task 5: "customised" marker on the collapsed voice prompt

**Files:**
- Modify: `gateway/frontend/src/components/VoiceChatControl.tsx`, `gateway/frontend/src/i18n.ts` (marker label/aria, de + en).
- Test: `gateway/frontend/src/components/VoiceChatControl.test.tsx`.

**Interfaces:** Consumes `DEFAULT_VOICE_SYSTEM_PROMPT` (chatDoc) for the comparison.

- [ ] **Step 1: Write failing tests**
  - The collapsed voice-prompt toggle shows a marker when `systemPrompt.trim() !== DEFAULT_VOICE_SYSTEM_PROMPT`, and shows none when it equals the default (including when it differs only by surrounding whitespace). Both locales.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** the marker (a small dot / badge on the collapsed section's toggle) with trimmed comparison; i18n keys in de + en.

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit** `feat(portal): mark a customised voice system prompt while collapsed (#326)`

---

### Task 6: Persistent output mute + no layout jump

**Files:**
- Modify: `gateway/frontend/src/components/VoiceChatControl.tsx` (+ `gateway/frontend/src/components/chat/voiceSession.ts` / `useVoiceChat.ts` only if the mute state needs surfacing differently than today), `i18n.ts` if copy changes.
- Test: `VoiceChatControl.test.tsx`.

**Interfaces:** Consumes the existing session-level mute (`voiceSession.ts` `setMuted` / `muted`, surfaced via `useVoiceChat`).

- [ ] **Step 1: Write failing tests**
  - The output-mute toggle is shown whenever the conversation is on (not only while audio plays), reflects the session `muted` state, and toggles it; pause/resume and "stop speaking" remain shown only while audio is actually playing.
  - The playing-only controls appearing/disappearing does not change the surrounding layout (space reserved) — assert the container/placeholder is present when idle.

- [ ] **Step 2: Run, verify fail.**

- [ ] **Step 3: Implement** — move the output-mute out of the playing-only group into the always-on row (beside the mic-mute); reserve the space the transient controls occupy. Rewrite the existing playback-control tests affected by the move.

- [ ] **Step 4: Run, verify pass.**

- [ ] **Step 5: Commit** `feat(portal): persistent voice-chat output mute without layout jump (#326)`

---

### Task 7: Documentation

**Files:** Modify `docs/architecture/`: `11-risks-and-technical-debt.md` (around :264 and :271 — remove the auto-pick-drift debt row, since option B closes it; keep any accurate residue), `06-runtime-view.md` (around :634 — the stable pick + `voiceModel` attribution + reconciliation, the persistent mute), `05-building-block-view.md` (around :247 — the auto-pick wording), `09-architecture-decisions.md` ADR-080 point (g) (update the auto-pick statement). Test: `make lint-docs`.

- [ ] **Step 1: Update the docs** to the shipped behaviour (stable pick, `voiceModel` attribution, uniform reconciliation, persistent output mute, customised-prompt marker); cross-link.
- [ ] **Step 2: `make lint-docs`** → OK (run with `docs/superpowers` still present will fail only on the branch-local check; the controller removes it before the PR).
- [ ] **Step 3: Commit** `docs(voice-chat): #326 follow-ups — stable pick, voice attribution, controls`

---

## Final verification (before the PR)
- `npm run build` + `npx prettier --check .` + `npx vitest run --retry=2` — all green.
- `make lint-docs` — OK (after removing the branch-local `docs/superpowers/`).
- `make sonar-gate && make sonar-findings && make sonar-branch-findings` → **0** on this branch's lines.
- Remove the branch-local `docs/superpowers/` working files before opening the PR (per `AGENTS.md`).
- PR note: frontend-only, no ServerAgent bump / migration; not run as a live spoken round-trip.

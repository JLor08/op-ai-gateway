# Voice Chat follow-ups (#326) — Design

**Intent:** Three non-blocking follow-ups surfaced by the Voice Chat phase 3
reviews. Make the automatic speech/transcription model pick **stable** within a
conversation (no more drift as models load in the background), **reconcile** the
saved voice when a forced model switch happens, and two small UI-clarity fixes
(a "customised" marker on the collapsed voice system-prompt field, and a
persistent, always-visible output mute with no layout jump).

**Why:** Phase 3's "auto-pick prefers loaded" made the auto-picked model
recompute on every models refresh (page load, view change), so a voice chosen
for one auto-picked model could silently be sent to a different one. Two UI
affordances also hide state: a customised prompt looks default while collapsed,
and the output mute is only reachable while audio plays — so it can't be
pre-set, its state is invisible between answers, and the controls jump the
layout as they appear and disappear.

## Items

### 1. Stable automatic model pick + voice reconciliation

**Current behavior (the drift).** `autoVoiceModel(streaming, models)`
(`gateway/frontend/src/components/chat/voiceModels.ts`) returns the first
streaming **and loaded** model, else the first streaming. It is called — via
`effectiveVoiceModel` ← `voiceLegsOf` in `ChatStore.tsx` — on every render with
the live `chatModels` catalogue, whose `loaded` flags change on any models
refresh. So the automatic pick (used when a leg is left on "automatic") can
change whenever load state changes, not only when the current model becomes
unavailable. `effectiveVoice` then sends the saved `voice` to whatever model is
effective (the voice list is "an offer, not a constraint"), so a voice belonging
to the old model can be sent to the newly-picked one.

**Desired behavior.**

*Stable automatic pick (runtime).* The automatic pick of each leg is resolved
once per conversation (per thread) and held **stable**: it changes only when the
currently-effective model is no longer available (no longer in the leg's
`streaming` set). "Prefer loaded" applies when the pick is first resolved or
re-resolved — a model merely *becoming* loaded in the background never switches a
working pick. This applies to the **automatic** pick only; an explicit user pick
wins while it streams and falls back otherwise. While an explicit pick is in
force for a leg, that leg holds no sticky automatic pick (it re-seeds if the user
returns the leg to automatic). The stable pick is **runtime state**, re-seeded
once per page load.

*Voice attribution (persisted) — closes the reload gap (option B, chosen).* The
thread persists, alongside the saved `voice`, the TTS model the voice was chosen
for (`voiceModel`). When the user picks a voice, `voiceModel` is set to the then
effective TTS model; when the voice is cleared, `voiceModel` is cleared too. It
is an **additive** field on the schema-free thread settings — no migration; an
old thread simply has none (treated as "unattributed").

*Reconciliation (uniform — in-session and across reloads).* Whenever the
effective TTS model differs from the voice's attributed `voiceModel`:
- voices of the effective model known and the saved `voice` is among them → keep
  the voice, and re-attribute `voiceModel` to the effective model;
- known and the voice is **not** among them → clear `voice` (and `voiceModel`):
  fall back to the model's default voice;
- not yet known (voices are fetched async per model, `useVoiceChatVoices`) →
  send nothing (the model default) meanwhile, and reconcile once the list arrives.

When the effective model equals `voiceModel`, the voice is left as is — so a
voice the user **typed** that is not in that model's cached list is still honored
for its own model (the "offer, not a constraint" rule stands); the membership
check runs only when the model actually differs from the one the voice was chosen
for. Because the reference is the persisted `voiceModel` (not an in-memory
"previous model"), the one rule closes the drift both during a session (any
forced switch) and across a reload.

**Rulings / notes:**
- Chosen **option B**: one small persisted field (`voiceModel`), not the heavier
  general per-voice attribution; it fully closes the cross-reload gap that
  stable-pick-alone would leave open. The auto-pick-drift debt row is then
  **removed**, not just narrowed.
- `withTtsModel` (the clean-up on an **explicit** model pick) must be reconciled
  with this: today it judges "same model speaks anyway" with a fresh
  `autoVoiceModel`, which is wrong under a sticky pick (it would keep/clear the
  wrong voice). Its caller (`ChatStore.tsx`) must pass the current **effective**
  TTS model, and the voice / `voiceModel` clean-up should go through the same
  attribution rule above. The builder picks the exact factoring; the observable
  rule is what binds.
- `useVoiceChatVoices` must distinguish **not-yet-known / empty / failed** (a
  tri-state) so the reconciliation does not clear a voice just because the new
  model's list has not loaded yet.

### 2. "Customised" marker on the collapsed voice system-prompt field

The voice system-prompt field is collapsed by default (phase 3). When the stored
prompt differs from the built-in default, show a small marker on the collapsed
section's toggle (a dot / "edited" badge) so a customised prompt is visible
without expanding. No marker when the prompt equals the default. Lives in
`gateway/frontend/src/components/VoiceChatControl.tsx`.

### 3. Persistent output mute + no layout jump

- Make the **output mute** a persistent toggle shown whenever the conversation
  is on (alongside the mic-mute from phase 3), so it can be pre-set and its state
  is always visible between answers. Pause/resume and "stop speaking" remain the
  transient controls shown only while audio is actually playing.
- Reserve the space the playing-only controls occupy (or otherwise stabilize the
  layout) so them appearing/disappearing does not shift the surrounding layout or
  drop focus.
- The session-level mute already exists (`voiceSession.ts` `setMuted`, applied
  per answer); this is about surfacing it persistently and the layout.

## Non-goals / out of scope
- General per-voice attribution UI or a per-model voice-memory across all models:
  option B adds exactly one field (`voiceModel`, the model the current voice was
  chosen for) to close the drift — nothing broader.
- Re-synth-on-replay (tracked separately in #324).
- Changing "prefer loaded" for the first/forced resolution — it stays; only the
  *continuous* re-evaluation is removed.
- Backend / ServerAgent / schema changes: frontend-only.

## Constraints (project-wide)
- Frontend only (`gateway/frontend`). No ServerAgent bump, no migration.
- TypeScript/React; reuse existing patterns (SearchableSelect, the thread-settings
  collapse pattern, the phase-3 mic-mute toggle).
- Keep the pure rules in `voiceModels.ts` unit-testable on their own, as today;
  UI/runtime wiring in `ChatStore.tsx` / `VoiceChatControl.tsx` /
  `voiceSession.ts`.
- English for all repo-facing text (chat is German).
- Gates before PR: `npm run build` + `prettier --check` + `vitest --retry=2`;
  `make lint-docs`; SonarQube branch-findings = 0.

## What's left to the builder
- Exactly where the stable auto-pick state lives (a per-`chatId` ref/map in
  `ChatStore`, or a small hook) and how it is seeded/re-seeded — pick the cleanest
  fit, as long as the observable rule holds: stable; switches only on
  unavailability; prefer-loaded at resolution.
- The precise membership-check wiring and the "voices now known" timing hook.
- The exact visual for the "customised" marker and the persistent mute toggle
  (match existing control styling).
- Docs: update `docs/architecture/` where the auto-pick behavior is described —
  at least `11-risks-and-technical-debt.md:264` and `:271` (the drift debt row is
  removed, since option B closes it), `06-runtime-view.md:634`,
  `05-building-block-view.md:247`, and ADR-080 point (g). State the new rule
  (stable pick + `voiceModel` attribution + reconciliation) and the persistent
  mute / customised-prompt marker.
- The "customised" marker compares the **trimmed** stored prompt against the
  built-in default (same trimming as `voiceChatSetting`).
- Moving the output mute out of the playing-only controls into the mic-mute row
  is straightforward, but the existing playback-control tests need rewriting.
- Any in-memory resolution state (the sticky auto-pick) resets on chat change;
  forced-switch detection ignores the `'' → model` first resolution.

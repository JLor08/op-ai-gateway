# Voice Chat error-reason disclosure — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Show the gateway's real failure reason for a Voice Chat error behind an expandable "Details" disclosure, instead of only the generic sentence.

**Architecture:** A pure helper turns a `VoiceChatError` into the raw diagnostic string to show (code + message, de-duplicated, suppressed when there is nothing genuine). `VoiceChatControl` renders that string inside an accessible disclosure under the existing error `Alert`, and stops appending the code inline.

**Tech Stack:** React + TypeScript (Vite), MUI, the repo's `i18n.ts` message catalogue, Vitest + Testing Library.

**Spec:** docs/superpowers/specs/2026-10-29-voice-chat-error-detail-design.md

## Global Constraints

- Frontend only. No backend, server-agent, or schema change; **no `ServerAgent` version bump**. The diagnostic `message` is already on the wire and already parsed into `VoiceChatError`.
- Repo-facing text (code comments, commits, PR) in English. New i18n keys exist in **every** locale and must satisfy the existing `i18n.test.ts` invariants.
- The disclosure detail is the raw English diagnostic (operator-facing, not translated); only the toggle label is localized.
- Behavior-preserving for every non-error path and for the friendly sentences, the Dismiss button, and the reserved-height playback room.
- Accessibility: the disclosure is a real control — a button with `aria-expanded` and `aria-controls` referencing the detail region. The error `Alert` keeps `role="alert"`.
- Verification before PR: `npm run build`, `npx prettier --check .`, `npx vitest run --retry=2` (full frontend suite), `make lint-docs`, and `make sonar-scan` + branch-findings (must be 0).

## Review Focus

- A `turn` error with **neither** code nor message (defensive): no disclosure is rendered, only the sentence — never an empty/clickable-to-nothing disclosure. (Task 1 tests.)
- A `message` equal to the `code`: the value is shown **once**, not twice. (Task 1 tests.)
- A `message` equal to the synthesized generic fallback ("the voice turn could not be completed"): suppressed — if that is all there is, no disclosure. (Task 1 tests.)
- A very long single-token `message`: wraps inside the Alert, no horizontal overflow/scroll. (Task 2.)
- Each new error starts with the disclosure **collapsed** (a stale "open" from a previous error must not persist). (Task 2 tests.)

---

### Task 1: Pure `errorDetailText` helper + fallback constant + i18n label

**Files:**
- Create: `gateway/frontend/src/components/chat/voiceErrorDetail.ts`
- Create: `gateway/frontend/src/components/chat/voiceErrorDetail.test.ts`
- Modify: `gateway/frontend/src/components/chat/voiceSession.ts` (export the fallback literal as a named constant; use it in the existing `case 'error'` — no behavior change)
- Modify: `gateway/frontend/src/i18n.ts` (add `chatVoiceErrorDetails`)

**Interfaces:**
- Consumes: `VoiceChatError` (`gateway/frontend/src/components/chat/useVoiceChat.ts`), `VOICE_TURN_FALLBACK_MESSAGE` (new, from `voiceSession.ts`).
- Produces: `export function errorDetailText(error: VoiceChatError): string | null` and `export const VOICE_TURN_FALLBACK_MESSAGE: string` — both consumed by Task 2.

- [ ] **Step 1: Export the fallback constant in `voiceSession.ts`.** Introduce `export const VOICE_TURN_FALLBACK_MESSAGE = 'the voice turn could not be completed'` and reference it in the existing `case 'error'` `args.onError(frame.message || frame.code || VOICE_TURN_FALLBACK_MESSAGE, …)` — the string value is unchanged, so no behavior changes.

- [ ] **Step 2: Write the failing unit tests** in `voiceErrorDetail.test.ts` for `errorDetailText(error)`:
  - `kind:'turn'` + `message:'live request body stalled: no data within the idle timeout'`, no code → returns that message.
  - `kind:'refused'` + `code:'portal.voice_chat_asr_unavailable'` + `message:'asr leg failed'` → contains both the code and the message.
  - `message` equals `code` (`code:'x'`, `message:'x'`) → the value appears exactly once.
  - `message` equals `VOICE_TURN_FALLBACK_MESSAGE`, no code → returns `null`.
  - empty: no code, empty/whitespace message → `null`.
  - `kind:'denied'`, `kind:'unsupported'`, `kind:'capture'` (even with a code/message) → `null`.
  - `kind:'dropped'` + `message:'voice chat connection closed (1006)'` → returns that message.

- [ ] **Step 3: Run the tests to verify they fail** (`errorDetailText` not defined).
  Run: `npx vitest run src/components/chat/voiceErrorDetail.test.ts`
  Expected: FAIL.

- [ ] **Step 4: Implement `errorDetailText(error: VoiceChatError): string | null`** in `voiceErrorDetail.ts`:
  - Return `null` unless `error.kind` is one of `turn`, `refused`, `dropped`.
  - `code = error.code?.trim() ?? ''`; `msg = error.message?.trim() ?? ''`; if `msg === VOICE_TURN_FALLBACK_MESSAGE` set `msg = ''`.
  - If `code` and `msg` and `code !== msg` → return both clearly (e.g. ``${code}\n${msg}``). If only one is non-empty → return it. If `msg === code` → return the single value. If neither → return `null`.

- [ ] **Step 5: Add the i18n label** `chatVoiceErrorDetails` to `i18n.ts`: de `'Details'`, en `'Details'` (and any other locale present), placed next to the other `chatVoiceError*` keys so stored ordering is unaffected.

- [ ] **Step 6: Run the unit tests + the i18n test to verify they pass.**
  Run: `npx vitest run src/components/chat/voiceErrorDetail.test.ts src/i18n.test.ts`
  Expected: PASS.

- [ ] **Step 7: Commit.**
  ```bash
  git add gateway/frontend/src/components/chat/voiceErrorDetail.ts gateway/frontend/src/components/chat/voiceErrorDetail.test.ts gateway/frontend/src/components/chat/voiceSession.ts gateway/frontend/src/i18n.ts
  git commit -m "feat(voice-chat): compute a failure-detail string from the error frame (#329)"
  ```

---

### Task 2: Expandable "Details" disclosure in the error Alert

**Files:**
- Modify: `gateway/frontend/src/components/VoiceChatControl.tsx` (drop the inline `(code)`; render the disclosure)
- Modify: `gateway/frontend/src/components/VoiceChatControl.test.tsx`

**Interfaces:**
- Consumes: `errorDetailText` from Task 1, `t.chatVoiceErrorDetails`.

- [ ] **Step 1: Write the failing component tests** in `VoiceChatControl.test.tsx`:
  - Given a `turn` error whose `message` is a diagnostic and a `code` is set: the alert shows `t.chatVoiceErrorTurn`; the code is **not** in the top sentence; a "Details" toggle (`t.chatVoiceErrorDetails`) is present with `aria-expanded="false"`; after clicking it, `aria-expanded="true"` and the diagnostic text is visible.
  - Given a `denied` error: no "Details" toggle is rendered.
  - Given a `turn` error with only the generic fallback message and no code: no "Details" toggle.

- [ ] **Step 2: Run the tests to verify they fail.**
  Run: `npx vitest run src/components/VoiceChatControl.test.tsx`
  Expected: FAIL.

- [ ] **Step 3: Remove the inline code suffix in `errorText`.** Delete the `withCode` wrapper so `turn` → `t.chatVoiceErrorTurn` and `refused` → `sentenceForCode(error.code, t) ?? t.chatVoiceErrorRefused`. The code now surfaces only through the disclosure.

- [ ] **Step 4: Render the disclosure** inside the error `Alert` (`VoiceChatControl.tsx` ~417), below `errorText(error, t)`: compute `const detail = errorDetailText(error)`; when non-null, render a disclosure — a toggle button labelled `t.chatVoiceErrorDetails` with `aria-expanded`/`aria-controls`, and an MUI `Collapse` (or `Accordion`) holding the detail in a muted, wrapping block (`whiteSpace: 'pre-wrap'`, `wordBreak: 'break-word'`). Keep it **collapsed by default for each error** (local `useState`, reset when `error` changes — e.g. a `key` on the error identity).

- [ ] **Step 5: Run the component tests + full suite to verify they pass.**
  Run: `npx vitest run src/components/VoiceChatControl.test.tsx`
  Expected: PASS.

- [ ] **Step 6: Commit.**
  ```bash
  git add gateway/frontend/src/components/VoiceChatControl.tsx gateway/frontend/src/components/VoiceChatControl.test.tsx
  git commit -m "feat(voice-chat): reveal the failure reason behind a Details disclosure (#329)"
  ```

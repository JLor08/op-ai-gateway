# Voice Chat — surface the failure reason (design)

**Issue:** #329
**Status:** approved design (brainstormed 2026-10-29)

## Intent & why

When a Voice Chat turn fails, the control shows only the generic localized
sentence `chatVoiceErrorTurn` ("Diese Antwort ist fehlgeschlagen. Halten Sie die
Sprechtaste, um es erneut zu versuchen."). The gateway actually sends the reason
— the `error` down-frame carries a stable `code` and a diagnostic `message` — but
the UI throws the `message` away and only appends `code` in parentheses *when one
is present*. Many real failures arrive with **no code** (the gateway relays an
upstream server-agent / audio.cpp error verbatim as `message`, no gateway code),
so the user, who is the **operator** of a self-hosted gateway, sees nothing
actionable.

Reproduced against the live deployment, the real frame was:

```
{"type":"error","message":"live request body stalled: no data within the idle timeout"}
```

No `code` → the UI shows just the generic line. This change makes the reason the
gateway already sends **visible**, so every failure becomes self-diagnosing.

## What the user sees

- The friendly localized sentence stays as the top line of the error `Alert`
  (the mapped sentence for a known code, the generic one otherwise).
- Beneath it, an **expandable "Details" disclosure**. Collapsed by default; when
  opened it shows the raw diagnostic the gateway sent: the stable `code` (if any)
  and the English `message`.
- The English diagnostic is intentional and operator-facing — like a log line or
  an error code. It is not translated.

## Decisions (made during brainstorming)

1. **Disclosure, not always-visible.** The detail sits behind a "Details" toggle
   (chosen over an always-visible muted line).
2. **Which errors get a disclosure:** only the server/upstream-carried kinds —
   `turn`, `refused`, `dropped`. The purely client-side kinds — `denied`,
   `unsupported`, `capture` — keep their complete friendly sentence and get **no**
   disclosure (there is no gateway diagnostic to show, and the sentence already
   says everything).
3. **The `code` moves into the disclosure.** The inline `(code)` suffix
   (`withCode` in `errorText`) is removed; the code now appears in the disclosure
   together with the message, so it is never shown twice.
4. **No redundant or empty detail.** The disclosure is not shown when there is no
   genuine diagnostic: when both `code` and `message` are empty, when the
   `message` is only the synthesized generic fallback
   ("the voice turn could not be completed"), or when `message` duplicates the
   `code` (show the value once).
5. **Frontend only.** The `message` is already sent on the wire and already
   parsed into the client's `VoiceChatError`; no backend, server-agent, or schema
   change, and no `ServerAgent` version bump.

## Constraints

- English diagnostic text is allowed in the (German) UI for the detail only; all
  chrome the change adds (the "Details" toggle label) is localized in every
  locale and must satisfy the existing `i18n.test.ts` invariants.
- The disclosure must be accessible: a real disclosure control (a button with
  `aria-expanded` and `aria-controls` to the detail region). The error `Alert`
  keeps its `role="alert"`.
- Behavior-preserving elsewhere: the friendly sentences, the Dismiss button, the
  reserved-height playback room, and every non-error path are unchanged.

## Non-goals / anti-goals

- Not fixing any specific upstream failure (e.g. whether the idle-timeout stall is
  itself a bug). This change only makes the reason visible.
- Not translating the diagnostic `message`, and not building a code→sentence
  catalogue for every upstream code (the informative cases are codeless anyway).
- Not changing the wire protocol or persisting anything.

## Left to the builder

- The exact visual form of the disclosure (MUI `Accordion`, or a toggle button +
  `Collapse`), and the exact layout of code + message inside it, as long as it is
  accessible, wraps long text, and reads as a muted secondary detail.
- Where the pure detail-text helper lives (a new exported function in
  `VoiceChatControl.tsx` or a small sibling module) and how the generic-fallback
  string is shared (export the existing literal from `voiceSession.ts` as a named
  constant rather than duplicating it).

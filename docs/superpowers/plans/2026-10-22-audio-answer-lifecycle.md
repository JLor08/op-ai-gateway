# Audio answer lifecycle Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give portal speech chats a per-chat audio-retention policy (keep all / keep last N / do not store), manual per-answer delete, a live playing/finished status bubble with mute and abort, and a not-stored placeholder that can be regenerated.

**Architecture:** A single `*int` retention knob rides the existing schema-free `ChatAudioSettings` JSON (no migration). One extended, atomic store method (`SaveChatWithBlobs` + delete ids) rewrites the chat document and deletes `chat_blobs` rows in one transaction; three callers use it — lazy prune at commit, retroactive prune, and manual delete. A not-stored answer is marked by a new `audio_removed` content part so one render rule covers prune / delete / N=0 / abort. The frontend adds the composer control, the not-stored bubble, the delete control, and mute/abort wired into the ADR-068 Web-Audio playback.

**Tech Stack:** Go (`gateway/backend`: `internal/store`, `internal/portal`, `internal/gateway`), React/TypeScript + MUI (`gateway/frontend`), SQLite + PostgreSQL store legs.

**Spec:** `docs/superpowers/specs/2026-10-22-audio-answer-lifecycle-design.md`

## Global Constraints

- No DB migration (the retention setting and the marker ride the schema-free `chats.blob` JSON; `chat_blobs` exists since migration 87). Highest migration stays 95.
- No server-agent version bump (portal + gateway only; no new agent capability).
- Add **ADR-071** (next free on origin/main, which is at ADR-070). If another doc-touching audio PR merges first, re-base the number.
- All repo-facing text in English; every new i18n string added to **both** `de` and `en` in `gateway/frontend/src/i18n.ts` (`en` is typed from `de`, so a missing key is a compile error).
- No LAN host names in any repo-facing text.
- Retention field semantics (single `*int`, declared **last** on `ChatAudioSettings`): `nil`/omitted = keep all (default, backward compatible); `0` = do not store; `N ≥ 1` = keep last N answers.
- `N` counts assistant messages that currently hold ≥1 `audio_url` part; a marker (not-stored) answer occupies no slot; a mixed text+audio answer counts as one.
- Verification (pre-PR): `golangci-lint fmt --diff` + `run ./...` for both Go modules; frontend `prettier --check` + `vitest run` + build; the **Postgres store leg is required** (`OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable`, standing `op-pg-test` container, Docker already running — do not start it); `make sonar-gate` → `make sonar-findings` → `make sonar-branch-findings` (branch-attributed must be 0); `./scripts/check-docs.sh`; remove `docs/superpowers/**` and `docs/implementation-status.md` before the PR.

## Review Focus

- **Legacy chat with no retention setting** (`Audio == nil` or `KeepLastAudio == nil`): must behave exactly as today — no prune, no marker, audio kept. Pinned in Task 4.
- **Prune/delete racing an active run**: the new mutations rewrite the whole document and hard-delete blobs; they must be refused with the existing active-run 409, never run concurrently with a commit. Pinned in Task 6.
- **N=0 on a run that silently fell back to buffered**: the single answer must be stored (not lost) and must not accumulate across repeats. Pinned in Task 5.
- **Deleting a multi-blob (ADR-070) answer**: every blob row of that answer must be deleted, not just the first, and the document rewritten so no live `audio_url` points at a deleted blob. Pinned in Tasks 2 and 6.
- **Regenerating from a not-stored bubble**: a marker answer must remain a real message with its id and prior history so `regenerate(id)` replays correctly. Pinned in Task 9.

---

## Task 1: Retention field on `ChatAudioSettings` (backend)

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (`ChatAudioSettings` struct ~511-537; `PrepareChatRun` audio-clear block ~744)
- Test: `gateway/backend/internal/portal/service_chats_test.go` (or the nearest existing `*_test.go` covering `ChatAudioSettings`/`PrepareChatRun`)

**Interfaces:**
- Produces: `ChatAudioSettings.KeepLastAudio *int` (json `keep_last_audio,omitempty`), declared last on the struct. Semantics per Global Constraints.

- [ ] **Step 1: Write the failing test** — `TestPrepareChatRunClampsKeepLastAudio`: a speech request with `KeepLastAudio = -3` round-trips with `KeepLastAudio == nil` (negative = unset/unlimited); `0` and `2` survive unchanged; a non-speech request has the whole `Audio` cleared (existing behavior still holds with the new field present).
- [ ] **Step 2: Run it, verify it fails** — `go test ./internal/portal/ -run TestPrepareChatRunClampsKeepLastAudio` → FAIL (field undefined).
- [ ] **Step 3: Add the field + clamp** — add `KeepLastAudio *int` as the last field of `ChatAudioSettings` with a doc comment matching the pointer-vs-unset convention of `Speed`/`Stream`; in `PrepareChatRun`, after the speech-kind audio handling, clamp `*KeepLastAudio < 0` to `nil`.
- [ ] **Step 4: Run it, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal): add keep_last_audio retention knob to ChatAudioSettings (#281)`.

---

## Task 2: Atomic store delete — extend `SaveChatWithBlobs` with delete ids

**Files:**
- Modify: `gateway/backend/internal/store/sqlite_chats.go` (`SaveChatWithBlobs` ~138-164), `gateway/backend/internal/store/memory_chats.go` (`SaveChatWithBlobs` ~127-155; add a narrowed `dropChatBlobIDs`), `gateway/backend/internal/portal/service.go` (`ChatStore` interface ~360), `gateway/backend/internal/portal/service_chats.go` (`commitChatRun` ~280, the one caller)
- Test: `gateway/backend/internal/store/sqlite_chat_blobs_test.go`, `gateway/backend/internal/store/memory_chat_blobs_test.go`

**Interfaces:**
- Produces: `SaveChatWithBlobs(ctx, chat store.Chat, blobs []store.ChatBlob, deleteBlobIDs []string) error` — inserts `blobs` and deletes `deleteBlobIDs` (scoped `chat_id = ? and id in (...)`) and updates the chat row, all in one transaction. `nil`/empty `deleteBlobIDs` behaves exactly as today.
- Consumes: `ChatBlob` (`sqlite_chats.go:39`), the `execer`/`execOn` tx seam, `rebind` (both legs).

- [ ] **Step 1: Write the failing tests** — `TestSaveChatWithBlobsDeletesIDsAtomically` (SQLite + Postgres via the existing dual-leg harness in this file): save a chat with 3 blobs; then call again with 1 new blob and `deleteBlobIDs` of 2 existing ids → those 2 rows gone, the new one present, the third untouched, chat doc updated; a forced failure mid-transaction leaves rows and doc unchanged (mirror the existing cancelled-context atomicity test ~227). `TestMemorySaveChatWithBlobsDeletesIDs` in the memory file, asserting the byte-budget is decremented for deleted ids.
- [ ] **Step 2: Run them, verify they fail** — signature mismatch / rows still present.
- [ ] **Step 3: Implement** — SQLite: inside the existing tx, after inserts, run `delete from chat_blobs where chat_id = ? and id in (<placeholders>)` via `execOn` (skip when empty); build placeholders like `ChatBlobsByIDs`. Memory: add `dropChatBlobIDs(chatID string, ids []string)` reusing the byte accounting of `dropChatBlobs` narrowed to the id set, applied in the same locked apply pass. Update the `ChatStore` interface and the sole caller `commitChatRun` to pass `nil` for now.
- [ ] **Step 4: Run them (memory + sqlite + postgres), verify they pass** — `OP_AI_GATEWAY_TEST_POSTGRES_DSN=... go test ./internal/store/ -run 'SaveChatWithBlobs'`.
- [ ] **Step 5: Commit** — `feat(store): atomic blob deletes in SaveChatWithBlobs (#281)`.

---

## Task 3: Not-stored marker part + message rewrite helpers (backend)

**Files:**
- Create: `gateway/backend/internal/portal/chat_audio_retention.go`
- Test: `gateway/backend/internal/portal/chat_audio_retention_test.go`
- Reference: `gateway/backend/internal/gateway/chat_runs_audio.go` (`speechAudioPart` ~54, `audio_url` shape), `gateway/backend/internal/portal/service_chats_blobs.go` (blob-url ↔ id)

**Interfaces:**
- Produces:
  - `const removedAudioPartType = "audio_removed"` and a one-field part struct marshalling to `{"type":"audio_removed"}`.
  - `markMessageAudioRemoved(raw json.RawMessage) (rewritten json.RawMessage, blobIDs []string, changed bool, err error)` — on an assistant message, strips every `audio_url` part (collecting each part's `chat_blobs` id from its `/api/portal/chat-blobs/<id>` url), appends one `audio_removed` marker if any audio was present, and returns the rewritten message, the collected ids, and whether anything changed. A message with no stored audio returns `changed == false` and no ids.
  - `messageHasStoredAudio(raw json.RawMessage) (bool, error)` — true when the message holds ≥1 `audio_url` part (the counting predicate for N).
  - `chatBlobIDFromURL(url string) (id string, ok bool)` — reuse an existing parser if one exists; otherwise add it here.

- [ ] **Step 1: Write the failing tests** — `TestMarkMessageAudioRemoved`: a multi-part (ADR-070) assistant audio message → all part ids collected, content becomes a single `audio_removed` marker, `changed == true`; a mixed text+audio message → text preserved, audio parts replaced by the marker; a text-only message → `changed == false`, no ids; an already-marked message → `changed == false`. `TestMessageHasStoredAudio` for the three shapes.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the helpers and the marker type.
- [ ] **Step 4: Run them, verify they pass.**
- [ ] **Step 5: Commit** — `feat(portal): audio_removed marker and message rewrite helpers (#281)`.

---

## Task 4: Lazy going-forward prune at commit (backend)

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (`writeAssistant` ~1026-1098; `commitChatRun` ~280)
- Create helper in `gateway/backend/internal/portal/chat_audio_retention.go`
- Test: `gateway/backend/internal/portal/service_chats_audio_blobs_test.go` (or the nearest audio-commit service test)

**Interfaces:**
- Consumes: Task 2 `SaveChatWithBlobs(..., deleteBlobIDs)`, Task 3 helpers, `ChatAudioSettings.KeepLastAudio` (read from `doc.Settings`).
- Produces: `pruneAudioBeyond(doc *chatDoc, keepLast int) (deleteBlobIDs []string, err error)` — walks messages newest→oldest, counting assistant messages with stored audio; beyond `keepLast` it rewrites each via `markMessageAudioRemoved` (in place in `doc.Messages`) and appends the ids. `commitChatRun(ctx, row, title, doc, blobs, deleteBlobIDs []string) error`.

- [ ] **Step 1: Write the failing tests** — `TestCommitPrunesBeyondKeepLast`: commit 3 audio answers with `keep_last_audio = 1` → after the 3rd commit, answers 1 and 2 are markers and their blob rows are gone, answer 3 is intact. `TestCommitKeepAllRetainsEverything` (Review Focus — legacy/nil): with `KeepLastAudio == nil`, three commits leave all three answers stored, no markers, no deletes. `TestCommitKeepLastCountsAnswersNotBlobs`: a multi-blob answer counts as one toward N.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — add `pruneAudioBeyond`; in `writeAssistant`, after `extractChatBlobs`, read `KeepLastAudio`; when it is a non-nil `N ≥ 1`, compute the prune set and pass it into the extended `commitChatRun`; `nil` (keep all) and `0` (handled in Task 5) do not prune here. Thread `deleteBlobIDs` through `commitChatRun` into `SaveChatWithBlobs`.
- [ ] **Step 4: Run them (incl. postgres leg), verify they pass.**
- [ ] **Step 5: Commit** — `feat(portal): lazy keep-last-N audio prune at commit (#281)`.

---

## Task 5: N=0 commit behavior + stream-vs-buffered threading + abort marker (backend)

**Files:**
- Modify: `gateway/backend/internal/gateway/chat_runs_audio.go` (`generateSpeechTurn` ~188-228), `gateway/backend/internal/gateway/chat_runs.go` (`finishRunWithParts` ~1245), `gateway/backend/internal/portal/service_chats.go` (`CommitAssistant`/`writeAssistant`; the speech cancel-commit)
- Test: `gateway/backend/internal/gateway/chat_runs_audio_test.go` (or nearest), `gateway/backend/internal/portal/service_chats_audio_blobs_test.go`

**Interfaces:**
- Consumes: Task 3 marker, Task 2/4 commit path.
- Produces: a `streamed bool` outcome threaded from `generateSpeechTurn` through `finishRunWithParts` into the commit (add a return value / field on the committed turn — the implementer picks the least-invasive carrier). The commit applies: `KeepLastAudio == 0` **and** `streamed` → commit the answer as a marker, no blobs stored; `KeepLastAudio == 0` **and** not `streamed` (buffered fallback) → store the one answer and prune every *older* stored answer in the chat (keep-last-1); a canceled/aborted speech run → commit a marker turn instead of empty.

- [ ] **Step 1: Write the failing tests** — `TestN0StreamedCommitsMarker`: retention 0 + a streamed run → committed answer is a marker, no blob rows. `TestN0BufferedFallbackKeepsOne` (Review Focus): retention 0 + a buffered run → the answer is stored; a second buffered-fallback answer stores and the first becomes a marker (no accumulation). `TestAbortedSpeechRunCommitsMarker`: a canceled speech run → the assistant turn is a marker (not empty), nothing billed/stored beyond that, and it is regenerate-able (has an id).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** — return/thread `streamed` from `generateSpeechTurn` (it already computes `streaming` and branches at `consumeSpeechStream` vs `bufferedSpeechTurn`); apply the N=0 rules and the abort marker in the commit. Reuse `markMessageAudioRemoved`/`pruneAudioBeyond`.
- [ ] **Step 4: Run them (incl. postgres leg), verify they pass.**
- [ ] **Step 5: Commit** — `feat(gateway): N=0 do-not-store with buffered-fallback keep-one, abort marker (#281)`.

---

## Task 6: Backend mutations — per-answer delete + retroactive prune (guarded)

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` / a new `gateway/backend/internal/portal/service_chats_audio_delete.go`; the portal chat endpoints file that registers chat routes and the active-run 409 guard used by `PUT /chats` (portal chat endpoints; 409 `portal.chat_run_active`)
- Test: `gateway/backend/internal/portal/service_chats_audio_delete_test.go`, an endpoint test beside the existing chat endpoint tests

**Interfaces:**
- Consumes: Task 2 `SaveChatWithBlobs(..., deleteBlobIDs)`, Task 3 helpers, Task 4 `pruneAudioBeyond`.
- Produces:
  - `DeleteMessageAudio(ctx, owner auth.Token, chatID, messageID string) error` — rewrites that one message via `markMessageAudioRemoved` and deletes its blob rows in one `SaveChatWithBlobs`; `ErrNotFound` for an unknown chat/message; no-op (success) if the message has no stored audio.
  - `PruneChatAudio(ctx, owner auth.Token, chatID string, keepLast int) error` — prunes the chat's stored audio down to `keepLast` (0 removes all), server-computed from the stored doc.
  - Routes: `DELETE /api/portal/chats/{id}/messages/{messageID}/audio` and `POST /api/portal/chats/{id}/audio/prune` (body `{ "keep_last": N }`), both **refused with the active-run 409** and owner-scoped.

- [ ] **Step 1: Write the failing tests** — `TestDeleteMessageAudioRemovesAllBlobs` (Review Focus, multi-blob): deleting one multi-blob answer removes every blob row and leaves a marker; a concurrent blob read then 404s cleanly. `TestPruneChatAudioToKeepLast` and `TestPruneChatAudioZeroRemovesAll`. `TestAudioMutationsRefusedWhileRunActive` (Review Focus): both routes return 409 `portal.chat_run_active` while a run is active. `TestDeleteMessageAudioUnknownIsNotFound`, ownership-scoping test (another owner's chat → not found).
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the two service methods, the two routes, and the active-run guard (reuse the same guard `PUT /chats` uses).
- [ ] **Step 4: Run them (incl. postgres leg), verify they pass.**
- [ ] **Step 5: Commit** — `feat(portal): per-answer audio delete and retroactive prune routes (#281)`.

---

## Task 7: Frontend wire type + settings round-trip

**Files:**
- Modify: `gateway/frontend/src/api/chat.ts` (`ChatAudioSettings` ~61-80), `gateway/frontend/src/components/chat/chatDoc.ts` (`AudioSettings` ~498, `DEFAULT_AUDIO_SETTINGS` ~521, `audioSetting` ~648, `audioSettingsFrom` ~670)
- Test: `gateway/frontend/src/components/chat/chatDoc.test.ts` (or nearest)

**Interfaces:**
- Produces: `ChatAudioSettings.keep_last_audio?: number`; `AudioSettings.keepAudio: boolean` (default `true`) and `AudioSettings.keepLast: string` (blank-able, default `''` = keep all), mirroring the `speed` string pattern; `audioSetting`/`audioSettingsFrom` mapping: `keepAudio === false` → `keep_last_audio: 0`; `keepAudio && keepLast` parses to `N ≥ 1` → `keep_last_audio: N`; else omit. Inverse reads `0 → {keepAudio:false, keepLast:''}`, `N → {keepAudio:true, keepLast:String(N)}`, `undefined → {keepAudio:true, keepLast:''}`.

- [ ] **Step 1: Write the failing test** — `audioSetting`/`audioSettingsFrom` round-trip for all three states and the blank/invalid `keepLast` (omitted); a non-speech thread still emits no `audio` fragment.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** the type + default + both round-trip directions.
- [ ] **Step 4: Run `vitest run` for the file, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): keep_last_audio retention wire + round-trip (#281)`.

---

## Task 8: Composer retention control

**Files:**
- Modify: `gateway/frontend/src/components/Chat.tsx` (speech block ~666-765, model on the Stream checkbox ~704-720), `gateway/frontend/src/i18n.ts` (de + en)
- Test: `gateway/frontend/src/components/Chat.test.tsx` (or a focused composer test)

**Interfaces:**
- Consumes: Task 7 `AudioSettings.keepAudio/keepLast`, the store `audioSettings`/`setAudioSettings`, the `speechStreaming` flag (gates the Stream toggle today).

- [ ] **Step 1: Write the failing test** — the speech composer shows a "keep audio" checkbox (default checked), a "keep all" / "keep last N" radio with an `N ≥ 1` number field active only for "keep last N"; unchecking is disabled unless `speechStreaming`; loading a thread with a stored value reflects it without re-persisting (no save fired on mount).
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** the control and the new i18n keys in both tables (e.g. `chatAudioKeep`, `chatAudioKeepAll`, `chatAudioKeepLast`, `chatAudioKeepHint` — exact spelling the builder's call).
- [ ] **Step 4: Run `vitest run`, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): audio retention composer control (#281)`.

---

## Task 9: `audio_removed` content part + not-stored bubble rendering

**Files:**
- Modify: `gateway/frontend/src/components/shared/chatContent.ts` (`ChatContentPart` ~24-27, `audioUrlsOf` ~78), `gateway/frontend/src/components/ChatMessage.tsx` (assistant speech branch ~158-167 / ~352), `gateway/frontend/src/i18n.ts`
- Create: `gateway/frontend/src/components/AudioNotStoredTurn.tsx`
- Test: `gateway/frontend/src/components/AudioNotStoredTurn.test.tsx`, `gateway/frontend/src/components/ChatMessage.test.tsx`

**Interfaces:**
- Produces: `ChatContentPart` gains `| { type: 'audio_removed' }`; a predicate `hasRemovedAudio(content)`; `AudioNotStoredTurn({ t, onRegenerate })` rendering the placeholder line + a regenerate button (reusing `chatRegenerate`).
- Consumes: the existing `regenerate` handler wiring (`onRegenerate` threaded to `ChatMessage`).

- [ ] **Step 1: Write the failing tests** (Review Focus — regenerate from marker): a terminal speech assistant message with an `audio_removed` marker renders `AudioNotStoredTurn` (no player, no download) and its regenerate button calls `onRegenerate(id)`; a message with `audio_url` parts still renders `AudioTurn`; a streaming/pending turn still renders `AudioPendingTurn`; `audioUrlsOf` ignores the marker.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the union member, the predicate, the component, the render branch, and the `chatAudioNotStored` key (de + en).
- [ ] **Step 4: Run `vitest run`, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): not-stored audio bubble with regenerate (#281)`.

---

## Task 10: API client + store wiring for delete and retroactive prune

**Files:**
- Modify: `gateway/frontend/src/api/chat.ts` (`chatApi` ~239-292), `gateway/frontend/src/components/chat/ChatStore.tsx` (api `Pick` list ~448-463; new handlers near `regenerate`/`onRefine` ~1540-1619; retroactive detection in the audio-settings setter ~498)
- Test: `gateway/frontend/src/api/chat.test.ts`, `gateway/frontend/src/components/chat/ChatStore.test.tsx`

**Interfaces:**
- Produces: `deleteMessageAudio(chatId, messageId)` → `DELETE .../messages/{id}/audio`; `pruneChatAudio(chatId, keepLast)` → `POST .../audio/prune`; a store `deleteAudio(messageId)` (threaded like `onRefine`) that calls the API then replaces the message's audio with the marker locally; retroactive flow: when `setAudioSettings` lowers retention (lower N or unchecks), hold the stricter value pending a confirm, and on confirm persist it + call `pruneChatAudio` + update the transcript; on decline revert.
- Consumes: Task 6 routes, Task 9 marker.

- [ ] **Step 1: Write the failing tests** — the two api methods hit the right URLs/methods; `deleteAudio` calls `deleteMessageAudio` and marks the message locally; a stricter retention change is NOT auto-persisted before confirm, persists + prunes on confirm, and reverts on decline (Review Focus — no unconfirmed bulk delete); raising N / keep-all persists with no prune call.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the api methods (+ `Pick` list), the store handlers, and the retroactive confirm gating.
- [ ] **Step 4: Run `vitest run`, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): audio delete/prune API + retroactive-confirm wiring (#281)`.

---

## Task 11: Per-answer delete control + inline confirm

**Files:**
- Modify: `gateway/frontend/src/components/ChatMessage.tsx` (assistant action stack ~353-404; thread a new `onDeleteAudio`), `gateway/frontend/src/components/Chat.tsx` (pass the handler ~992-1003), `gateway/frontend/src/i18n.ts`
- Test: `gateway/frontend/src/components/ChatMessage.test.tsx`

**Interfaces:**
- Consumes: Task 10 store `deleteAudio`, Task 9 marker (post-delete the bubble becomes `AudioNotStoredTurn`).

- [ ] **Step 1: Write the failing test** — a stored audio answer shows a delete (trash) icon; clicking it reveals an inline confirm (Delete / Cancel, local state, no modal); confirming calls `onDeleteAudio(id)`; canceling restores the icon; the control is absent on a not-stored bubble.
- [ ] **Step 2: Run it, verify it fails.**
- [ ] **Step 3: Implement** the control, inline confirm, handler threading, and the `chatAudioDelete` / `chatAudioDeleteConfirm` keys (de + en).
- [ ] **Step 4: Run `vitest run`, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): per-answer audio delete control with inline confirm (#281)`.

---

## Task 12: Live stop — mute (GainNode) and abort

**Files:**
- Modify: `gateway/frontend/src/components/chat/speechPlayback.ts` (`StreamingPcmPlayer` connect ~194; `SpeechPlayback` interface ~224-237; the per-chat `Session`), `gateway/frontend/src/components/chat/ChatStore.tsx` (expose `muteAudio(chatId, muted)` + a `runAudioMuted` render mirror), `gateway/frontend/src/components/Chat.tsx` (pass `onMute/onAbort/muted` ~991), `gateway/frontend/src/components/AudioPendingTurn.tsx` (render the two controls), `gateway/frontend/src/i18n.ts`
- Test: `gateway/frontend/src/components/chat/speechPlayback.test.ts`, `gateway/frontend/src/components/AudioPendingTurn.test.tsx`

**Interfaces:**
- Produces: `SpeechPlayback.setMuted(chatId: string, muted: boolean): void` (a persistent `GainNode` between each source and `ctx.destination`, `gain.value` 0/1, reversible); store `muteAudio` + `runAudioMuted`; `AudioPendingTurn` gains `muted`, `onMute`, `onAbort`.
- Consumes: the existing `stop()` (cancel run) for abort; the existing `runAudioLive` threading as the model for `runAudioMuted`.

- [ ] **Step 1: Write the failing tests** — `setMuted` toggles the gain to 0 and back (live playback silenced, run still live); the live bubble renders Mute and Abort; Mute calls `muteAudio` (toggles label), Abort calls `stop`.
- [ ] **Step 2: Run them, verify they fail.**
- [ ] **Step 3: Implement** the GainNode + `setMuted`, the store wrapper/mirror, the bubble controls, and the `chatAudioMute`/`chatAudioUnmute`/`chatAudioAbort` keys (de + en).
- [ ] **Step 4: Run `vitest run`, verify it passes.**
- [ ] **Step 5: Commit** — `feat(portal-ui): mute and abort controls on the live speech bubble (#281)`.

---

## Task 13: ADR-071 + architecture docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (add ADR-071, amend ADR-063/068/070 cross-refs), `docs/architecture/reference/api-surface.md` (the two new routes), `docs/architecture/reference/data-model.md` (`keep_last_audio`, the `audio_removed` marker), `docs/architecture/cross-cutting/compatibility-and-inference.md` (the retention knob) as applicable
- Verify: `./scripts/check-docs.sh`

**Interfaces:** none (documentation).

- [ ] **Step 1: Write ADR-071** — "Audio answer lifecycle: per-chat retention (keep all / last N / do not store), atomic blob delete + document rewrite with an `audio_removed` marker, lazy + retroactive-with-confirm prune, manual per-answer delete, live mute/abort — no migration, no agent bump." Record the decisions, the N=0 buffered-fallback keep-one rule, the active-run guard, and the amendments to ADR-063/068/070.
- [ ] **Step 2: Update the reference docs** for the new routes, the wire field, and the marker.
- [ ] **Step 3: Run `./scripts/check-docs.sh`, verify it passes.**
- [ ] **Step 4: Commit** — `docs(adr): ADR-071 audio answer lifecycle (#281)`.

---

## Notes for the executor

- Order is dependency-first: Tasks 1–6 (backend) before 7–12 (frontend), docs last. Within backend, 2 and 3 are the primitives 4/5/6 build on.
- Run the Postgres leg for every store/service task (Tasks 2, 4, 5, 6), not just SQLite.
- Keep each commit's body substantive (squash-merge pre-fills from it).
- The design spec travels with this plan — read both.

# Design — Audio answer lifecycle: retention, manual delete, live stop (mute/abort), and the not-stored bubble

- Issue: #281 (follow-up to #279, shipped as PR #280 / ADR-070)
- Date: 2026-10-22
- Status: design, awaiting approval (brainstormed with the user 2026-10-22)
- Scope: portal frontend + gateway backend only. No server-agent change, no DB migration.

## Intent and why

Portal speech threads synthesize assistant audio answers and (since ADR-063/ADR-070)
store each answer as one or more ordered WAV blobs so it can be replayed and downloaded
later. Today an answer, once stored, lives until the whole chat is deleted: there is no
way to keep only recent audio, to drop a single answer, or to opt out of storing at all.

The user wants control over the lifecycle of synthesized audio, for two reasons they named:

1. **Data minimization** — a synthesized answer need not be kept once it has been heard.
2. **Bounding per-chat storage growth** — long TTS clips are large; a chat should not grow
   without limit.

These are the same two motivations behind #281. A third, separate feature (issue #264,
out of scope here) will later *shrink* a kept clip by re-encoding it to a smaller format
and replacing the stored blob in place; #281 deliberately does not do transcoding. What
#281 must provide for that future is a clean, well-tested **delete / rewrite building
block** at the store, which #264 can sit beside later.

During brainstorming the user added a fourth concern beyond the issue text: while a
streaming answer is playing, the user wants to be able to **stop the audio** — and
specifically wants *two* distinct controls: **mute** (silence it but let it finish and be
saved) and **abort** (cancel the run entirely).

## Goals

- A **per-chat retention setting** for assistant audio answers: keep all (default), keep
  only the last *N* answers, or do not store at all.
- **Lazy, going-forward prune**: when a new assistant audio answer commits successfully,
  older answers beyond the limit are removed in the same atomic commit.
- **Retroactive prune with confirmation**: lowering *N* or turning retention off removes
  the now-excess stored audio immediately, after a confirm (it is an irreversible bulk
  delete).
- **Manual per-answer delete**: a control on a single audio answer that removes just that
  answer's audio (blob rows + the audio parts in the document), with inline confirm.
- A **not-stored bubble state** for any answer whose audio is gone (pruned, deleted, or
  never stored because retention was off): a clear placeholder plus a **regenerate**
  action, and nothing else (no player, no download).
- A **live playing/finished status** on a streaming answer, with two controls while it
  plays: **mute** (reversible, silences local playback only; the run finishes and is saved
  per retention) and **abort** (cancels the run; nothing is stored; the bubble ends in the
  not-stored state).

## Non-goals

- Server-side transcoding / compressed storage (issue #264).
- The #279 chunking / honest-error work itself (shipped, ADR-070).
- A global or operator-level retention default. #281 is **per-chat only**; a global default
  is a separate surface (operator settings, precedence rules) and a later follow-up.
- Counting or pruning user transcription inputs. *N* counts assistant audio answers only.
- Any DB schema change (see "No migration" below).

## Anti-goals (things that would make this a failure even if it "works")

- A retention setting that silently discards a **paid, un-heard** answer. The system must
  never bill for a synthesis the user could never listen to. This drives the "keep the one
  if it buffered" fallback and the "N=0 only when streaming" gate.
- A destructive bulk delete without an explicit confirm.
- A pruned/deleted answer that leaves a dangling `audio_url` serving a 404, or a half-written
  document where the blob rows and the document disagree. Delete and document-rewrite must
  be one atomic transaction.
- Losing an answer's place in the transcript, or its ability to be regenerated, just because
  its audio is gone.

## The decided design

### Retention setting (the composer control)

A per-chat setting on the speech thread, edited in the composer, round-tripped through the
stored chat document exactly like the existing audio knobs (voice/speed/stream).

UI shape (brainstormed and confirmed, see the "not stored" and composer mockups):

- A checkbox **"Keep audio answers"**, default **on** — so existing chats and new chats
  behave exactly as today (unlimited retention) with no migration and no behavior change.
- When on, a radio pair: **"Keep all"** (default) or **"Keep last N"** with a number field,
  *N ≥ 1*. The number field is only active for the "keep last N" choice, so there is no
  `1 → 0 → ∞` stepper confusion and no infinity sentinel in the UI.
- Unchecking the checkbox means **do not store** (the ephemeral option). It is **only
  selectable when the thread actually streams** — i.e. when the portal's `speechStreaming`
  gate holds (the same flag that shows the existing Stream toggle). On a buffered-only model
  the checkbox is forced on, because a buffered answer is only ever heard from storage.
- On load the composer reflects the stored value and never silently re-persists it: a thread
  that stored "do not store" (0) but whose model is now buffered-only shows the stored value
  as-is (the buffered-fallback below keeps it correct); only *new* selection of "do not store"
  is disabled on a non-streaming thread.

Three-valued data model — a single optional integer field on `ChatAudioSettings` (wire
`keep_last_audio`, Go `*int` following the existing pointer-vs-unset discipline of `Speed`
and `Stream`, declared **last** on the struct so existing chats' serialized bytes do not
move):

| State                       | Wire value        | Meaning                               |
|-----------------------------|-------------------|---------------------------------------|
| Keep all (default)          | omitted (nil)     | unlimited; backward compatible        |
| Keep last N                 | `N` (≥ 1)         | keep the newest N assistant answers   |
| Do not store                | `0`               | do not store (streaming only)         |

The frontend represents this with the checkbox + radio and maps to/from the single wire
field in the existing `audioSetting` / `audioSettingsFrom` round-trip; exact frontend
state naming is the builder's call.

### N counts whole answers, not blobs

*N* counts assistant audio **answers** (messages), not blobs. Since ADR-070 stores one long
answer as several ordered blobs, pruning an answer removes **all** of that answer's blobs
together and rewrites that one message. User transcription inputs are a different thread
kind and are never counted or pruned.

The prune/count walks by *message currently holding at least one `audio_url` part*: a
not-stored answer (a marker) does not occupy a slot, so it never pushes a still-stored older
answer out of the last *N*; a mixed text+audio answer counts as one audio answer.

### Prune semantics: lazy going-forward + retroactive-with-confirm

- **Lazy, going-forward** — on every successful assistant audio commit, after the new answer
  is appended, older assistant answers beyond the limit are pruned in the *same* atomic
  commit. The newest answer is the one just appended, so it is never the one pruned.
- **Retroactive, with confirmation** — when the user lowers *N* or turns retention off, the
  excess stored audio is removed immediately. Because this is an irreversible bulk delete, the
  stricter setting is **held pending an explicit confirm** — it is not auto-saved first. On
  confirm, the new setting is persisted and the excess is pruned to it (one flow, one
  server-side prune routine shared with the lazy path). On decline, the control reverts and
  nothing is persisted or deleted. Raising *N*, choosing "keep all", or re-enabling needs no
  confirm and deletes nothing. Thereafter the going-forward lazy prune runs silently: it is the
  chosen setting operating as intended (one newly-excess answer per send), not a surprise bulk
  delete, so it is not re-confirmed.

### Manual per-answer delete

A delete control on a single stored audio answer removes just that answer's audio: its blob
rows and its `audio_url` parts in the document, in one atomic transaction, leaving the message
in the not-stored state. Inline confirm (not a modal dialog), matching the mockup. This is the
one-at-a-time version of the same delete path the prune uses.

### The not-stored bubble state

Any answer whose audio is gone — pruned, manually deleted, or never stored (retention off) —
renders a single placeholder: an "audio no longer stored" line plus a **regenerate** action,
and nothing else. For a pure-speech answer (no text) that is the whole bubble; for a mixed
text+audio answer the text stays and the placeholder sits where the player was.

To make this deterministic (and to keep the message a real, non-empty, regenerate-able turn),
when audio is removed the answer's `audio_url` parts are replaced by a **single marker part**
(a new content-part type, e.g. `audio_pruned`, carrying no URL). The marker:

- is written by every path that leaves an answer without audio — the three removal paths (lazy
  prune, retroactive prune, manual delete), the "do not store" commit path, and the
  **abort/cancel commit** of a streaming run — so one rendering rule covers every not-stored case;
- is the signal the not-stored rendering keys on (not merely "zero audio"), so a genuinely failed
  run (status error, no marker) still renders its error rather than the not-stored placeholder;
- is ignored by the audio-URL extractor (so the message has zero playable audio) and drives the
  frontend to render the not-stored state;
- requires no migration (it rides the schema-free JSON document) but must be tolerated by both
  the backend content-part model and the frontend content discriminated union.

### Regenerate

The not-stored bubble's regenerate reuses the existing `regenerate(id)` → `startRunWithHistory`
path (slice history before the answer, replay). This is why a not-stored answer must remain a
real message with its id rather than being deleted from the transcript.

### Live stop: mute and abort

While a streaming answer plays (ADR-068 progressive Web-Audio playback), the live status bubble
shows "playing…" and two controls:

- **Mute** — reversible; silences local playback only (a gain node set to 0, re-enabled by the
  same control). The run keeps generating, completes normally, and is **stored per the
  retention setting**. It then becomes the normal saved player. This is a "listen-no-more"
  control, not a cancel.
- **Abort** — cancels the run (reusing the existing stop path: cancel the chat run, which closes
  the live stream). Billing is unchanged (an aborted stream bills a loud-zero incomplete) and no
  partial/paid audio is committed. The one change from today: the speech cancel-commit writes the
  not-stored **marker** instead of an empty canceled turn, so the bubble ends in the not-stored
  state with regenerate. The partial audio is discarded.

The user explicitly wants both, as distinct controls.

### N=0 ("do not store") and the streaming fallback

"Do not store" only makes sense when the answer is heard live, i.e. when the run streams.

- The UI only offers it for a streaming thread (gate above).
- At commit, if retention is 0 but the run did **not** actually stream (a stale streaming
  verdict fell back to a buffered one-shot WAV), the single answer is **stored anyway** — it
  was never heard live, and a paid answer must not vanish. If the run did stream, the just-
  produced parts are dropped and the answer commits in the not-stored state (marker, no blobs).
- This fallback also makes a raw-API client setting `0` on a non-streaming model safe without a
  hard rejection: it simply keeps the one buffered answer.
- `0` is an explicit exception to the generic "keep newest *N*" prune (which read literally as
  keep-newest-0 would delete the answer just stored): under `0`, a streamed answer stores
  nothing, and a buffered-fallback answer is stored but **supersedes any previously stored audio
  in the chat** (effectively keep-last-1 for the stored fallback), so a `0` thread never
  accumulates even on a model that only sometimes falls back to buffered.

This requires threading "did this run actually stream vs fall back to buffered" from the speech
generation into the commit, which does not know that today.

## Architecture and touch-points (where it lands)

Verified against current `main` (origin/main merged 2026-10-22) by two code-exploration passes.

### No migration

The chat document is schema-free JSON (`settings` + `messages`) sealed in one opaque
`chats.blob` column; `chat_blobs` already exists (migration 87) with
`chat_id ... on delete cascade`. Therefore: the retention setting rides `doc.Settings`
(inside `ChatAudioSettings`), the not-stored marker rides a message's content array, and
removing audio is a document rewrite + `DELETE FROM chat_blobs ...` — all inside the existing
sealed-JSON model. **No schema change, no new migration.** (Next free migration remains 96 if
one were ever needed; none is.)

### Backend (gateway/backend)

Packages: the chat *service* and document live in `internal/portal` (`service_chats.go`,
`service_chats_blobs.go`); the run *executor* lives in `internal/gateway`
(`chat_runs.go`, `chat_runs_audio.go`, `chat_runs_audio_stream.go`); the store is
`internal/store`.

- **Setting** — add the `*int` retention field to `ChatAudioSettings`
  (`internal/portal/service_chats.go`, last field); validate/clamp near the existing Audio
  clearing in `PrepareChatRun`.
- **Store delete primitive** — `internal/store/sqlite_chat_blobs.go` is insert/read-only today.
  Add atomic delete: either extend `SaveChatWithBlobs` to take a `deleteIDs []string` (preferred:
  one transaction for doc-rewrite + blob-insert + blob-delete) or add a sibling method, plus the
  memory-store mirror (`memory_chats.go`, reusing its byte-budget accounting) and the `ChatStore`
  interface (`service.go`). One `SQLStore` serves both SQLite and Postgres via the dialect
  rebind, so a single `?`-placeholder implementation covers both legs.
- **Lazy prune hook** — in `writeAssistant` (`service_chats.go`), after `extractChatBlobs` moves
  the new turn's inline WAV data into blob rows, compute the prune set from older assistant
  answers, rewrite those messages to the marker, and fold the blob deletes into the same
  `commitChatRun` / `SaveChatWithBlobs`.
- **Retroactive prune + manual delete** — a portal mutation (route + API method) that prunes a
  chat's stored audio to the current setting (server-computed set, robust to concurrent edits)
  and a per-message delete; both reuse the store delete primitive and the marker rewrite.
- **Stream-vs-buffered threading** — thread the actual-streamed outcome from `generateSpeechTurn`
  (`internal/gateway/chat_runs_audio.go`, the `consumeSpeechStream` vs `bufferedSpeechTurn`
  branch) to the commit so the N=0 fallback can decide.
- **Abort commit** — the executor's cancel path already commits nothing paid and bills the
  incomplete stream (loud-zero); the only change is that a canceled *speech* run commits the
  not-stored marker rather than an empty turn, so the bubble renders not-stored + regenerate.
- **Concurrency** — the new retroactive-prune and per-answer-delete mutations rewrite the whole
  document and hard-delete blob rows, so they are refused with the existing active-run guard
  (409 `portal.chat_run_active`, as `PUT /chats` is), to avoid racing a run's read-modify-write
  (which could resurrect a dangling `audio_url` or clobber a just-committed turn). Consequence:
  per-answer delete and retention changes are unavailable while a run is active for that chat
  (the composer is already disabled during a run).
- **Read endpoint** — unchanged: a deleted blob already returns a clean 404
  (`portal.chat_blob_not_found`); the atomic rewrite ensures no live message points at a deleted
  blob.

### Frontend (gateway/frontend/src)

- **Wire + state** — add the retention field to the `ChatAudioSettings` wire type (`api/chat.ts`)
  and to the store `AudioSettings` + defaults (`chatDoc.ts`); round-trip through `audioSetting`
  / `audioSettingsFrom` (full-replace PUT, so both directions are required).
- **Composer** — add the checkbox + radio + N field in the speech block (`components/Chat.tsx`),
  modeled on the existing Stream checkbox; disable "do not store" unless `speechStreaming` holds.
- **Live bubble** — add Mute + Abort to the live status turn (`AudioPendingTurn.tsx`, threaded
  like the existing `audioLive` flag). Mute needs a persistent `GainNode` inserted in
  `speechPlayback.ts`'s `StreamingPcmPlayer` (there is none today) plus a `setMuted` on the
  playback interface and a store wrapper. Abort calls the existing `stop()` (cancel run).
- **Not-stored bubble** — render the marker/zero-audio complete speech turn as the placeholder +
  regenerate (`ChatMessage.tsx`), reusing the existing `regenerate` handler wiring.
- **Delete control** — a new per-turn callback threaded like `onRefine`, an icon on the existing
  assistant action stack, inline confirm via local state; a new `api/chat.ts` mutation.
- **Retroactive confirm** — detect a stricter setting change in the composer, confirm, then call
  the prune mutation.
- **i18n** — add all new labels to both `de` and `en` tables (`i18n.ts`); `en` is typed from
  `de`, so a missing key is a compile error. Reuse the existing `chatRegenerate` label for
  "regenerate".

## UX (confirmed via mockups)

Five states, all approved by the user:

- **Composer**: "Keep audio answers" checkbox (default on) → "Keep all" / "Keep last N" radio
  (N ≥ 1); unchecking disabled unless streaming.
- **Streaming**: "playing…" with a **Mute** and an **Abort** button.
- **Finished & stored**: the normal player + download + a delete control.
- **Delete confirm**: inline (no modal).
- **Not stored** (pruned / deleted / N=0): one placeholder line + **regenerate**; for a pure-
  speech answer that is the whole bubble.

## Constraints

- **Branch/PR discipline**: never commit to or merge into `main`; one branch → one PR against
  `main`; substantive commit bodies (squash-merge pre-fills from the commit body).
- **ADR**: add **ADR-071** (next free on origin/main; `main` is at ADR-070) documenting the
  retention/delete/not-stored decision and amending the audio-storage ADRs (063/068/070). If
  another doc-touching audio PR merges first, re-base the ADR number.
- **No migration** (established above); **no server-agent version bump** (portal + gateway only,
  no new agent capability).
- **i18n**: every new string in both `de` and `en`.
- **Privacy**: no LAN host names in any repo-facing text (PR, commits, issues, docs, ADRs).
- **Language**: repo-facing text in English; the user is addressed in German in chat only.
- **Verification (hard gates, pre-PR)**: `golangci-lint fmt --diff` + `run ./...` for both Go
  modules; frontend `prettier --check` + `vitest run` + build; the **Postgres store leg is
  required** (this touches the store) via the standing `op-pg-test` container
  (`OP_AI_GATEWAY_TEST_POSTGRES_DSN=postgres://postgres:test@127.0.0.1:55432/optest?sslmode=disable`);
  `make sonar-gate` → `make sonar-findings` → `make sonar-branch-findings` (branch-attributed
  must be 0); `./scripts/check-docs.sh`. Remove branch-local working files
  (`docs/superpowers/**`, `docs/implementation-status.md`) before the PR. Push over SSH; enable
  Auto-fix on the PR.
- **Testing discipline**: TDD per task; the delete primitive is exercised on memory + SQLite +
  Postgres; the commit-time prune, the N=0 streaming fallback, and the marker round-trip get
  store- and service-level tests; the frontend gets vitest coverage for the composer control,
  the not-stored rendering, mute/abort, and the delete confirm.

## Left to the builder

- Exact names: the retention field/wire key, the marker part type string, the new route(s) and
  API method names, and the frontend state field names (subject to the semantics above).
- Whether retroactive prune and manual delete are one route with two modes or two routes
  (recommendation: server-computed "apply current retention" for the bulk case, a per-message
  delete for the single case, both over the one store primitive).
- Whether to extend `SaveChatWithBlobs` with `deleteIDs` or add a sibling atomic method
  (recommendation: extend, to keep one transaction).
- The precise wording of the new user-facing labels (English source strings) and the ADR-071
  prose, within the decisions above.
- The phased task breakdown (the plan, written next via the planning workflow).

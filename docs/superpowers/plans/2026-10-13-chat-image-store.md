# Chat binary payload store (#124) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move chat image (and future binary) payloads out of the sealed chat
transcript into a generic owner-scoped `chat_blobs` store, referenced from the
transcript by a gateway URL, so a thread is no longer limited to ~8–12 images by
the 4 MiB text cap; resolve references back to bytes for inference; replace the
removed cap with per-blob and per-run bounds.

**Architecture:** A new `chat_blobs` table (migration v87), sealed with the same
capture cipher, holds decoded payload bytes keyed by a random id; a content part's
`image_url.url` becomes `/api/portal/chat-blobs/{id}`. An extract hook runs at the
two run-commit paths (`PrepareChatRun`, `writeAssistant`) — not on create or bare
autosave — storing inline `data:` payloads and rewriting the url; `PrepareChatRun`
resolves gateway urls back to `data:` urls (batched, kind-aware, remote-safe) when
building the run's upstream history; an owner-scoped, XSS-hardened serving endpoint
feeds the browser `<img>`.

**Tech Stack:** Go (gateway/backend: `internal/portal`, `internal/gateway`,
`internal/store`, `internal/capture`), React/TypeScript (gateway/frontend),
SQLite + Postgres behind a dialect seam + an in-RAM memory driver.

**Spec:** `docs/superpowers/specs/2026-10-13-chat-image-store-design.md`

## Global Constraints

- **Branching:** feature branch + PR only; never commit to `main`. All repo-facing
  text (commits, PR, issues) in **English**. **No attribution/trailer lines** in
  commits or the PR. Push over **SSH**.
- **Store migrations:** append-only; next free number is **v87**; never edit or
  reorder a shipped migration; do **not** back-port the new table into the v1
  baseline DDL (`migrate.go` frozen baseline). Byte/size columns are **`bigint`**
  (ADR-005), valid on both dialects; blob columns go through `dl.blobType()`
  (`bytea` on postgres, `blob` on sqlite); timestamps through the dialect helper.
- **Three store drivers stay in parity:** `MemoryChatStore`, sqlite, postgres. Any
  new table/method needs a dialect-aware SQL impl, a memory impl, and a conformance
  test that runs on **all three** — provision the Postgres DSN for the store tests
  (the postgres subtests **skip silently** without it).
- **At-rest:** reuse the existing capture cipher (`capture/cipher.go`,
  AES-256-GCM, `KeyVersion 1`); **no new decryptable secret**; payload bytes are
  sealed on the SQL path, plain+volatile on the RAM-fallback path — never plaintext
  on disk.
- **Serving safety:** stored `content_type` is **allow-listed** (`image/png`,
  `image/jpeg`, `image/webp`, `image/gif`) at store time; the endpoint sends
  `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'`,
  `Content-Disposition: inline`, `Cache-Control: private, no-store`; never serves an
  un-allow-listed type.
- **Bounds:** `MaxChatBlobBytes` (default 20 MiB) at store time; `MaxRunHistoryBytes`
  on the resolved run history. The existing `MaxChatContentBytes` (4 MiB text cap)
  and `MaxChatRequestBytes` (5 MiB) are **unchanged**.
- **Codegen:** `portal.API` is interfacer-generated and wrapped by a gowrap OTel
  wrapper; after changing the interface, run `go generate` for
  `portal/api_tracing_gen.go` and **restore its SPDX/Copyright header** by hand
  (go generate strips lines 1-2).
- **CI gates:** `golangci-lint fmt` + `run` per Go module (gofumpt single sorted
  import group; gocritic); frontend `prettier format:check` + `tsc` + `vitest` +
  `vite build`; `make lint-docs`; the frontend arch boundary (`Chat.tsx` may import
  `chat/` constants only via `ChatStore`, pinned by `arch.test.ts`).

## Review Focus

- **Stored XSS:** a `data:text/html` or `image/svg+xml` upload is rejected **at
  store time**, and the endpoint never serves an un-allow-listed type inline (Task 2,
  Task 7).
- **Cross-tenant leak:** `GET /api/portal/chat-blobs/{id}` for another user's blob
  404s with no existence leak; a forged `/chat-blobs/{id}` url grafted into
  `edited_history` cannot resolve or serve another chat's/user's bytes (Task 6, Task 7).
- **Create-path FK:** a `CreateChat` carrying an inline `data:` image does **not**
  violate the `chat_blobs.chat_id` FK (no extraction on create) and still succeeds
  (Task 6).
- **Idempotence / no orphan re-upload:** a legacy chat migrated by a run stores each
  image **exactly once**; a subsequent autosave stores nothing new (Task 6).
- **Remote provider bytes:** a vision turn over a migrated thread sends real decoded
  bytes to a **remote** provider (Anthropic/OpenAI), not an unreachable gateway url
  (Task 8).
- **Cascade + eviction:** deleting a chat drops its `chat_blobs` on all three
  drivers; the memory `evict()` drops a chat's blobs and frees its budget bytes
  (Task 3).
- **Replacement bounds:** a payload over `MaxChatBlobBytes` is refused; a run whose
  resolved history exceeds `MaxRunHistoryBytes` is refused (Task 2, Task 8).
- **Back-compat:** an un-migrated chat with inline `data:` images renders and runs
  unchanged (Task 6, Task 8).

---

## File Structure

- Create: `gateway/backend/internal/store/sqlite_chat_blobs.go` — the SQL
  `chat_blobs` CRUD behind the dialect seam.
- Create: `gateway/backend/internal/store/sqlite_chat_blobs_test.go`,
  `gateway/backend/internal/store/memory_chat_blobs_test.go` /
  conformance additions.
- Modify: `gateway/backend/internal/store/migrate.go` (add v87),
  `.../store/sqlite_chats.go` (DTO sibling, `SaveChatWithBlobs`),
  `.../store/memory_chats.go` (blob map, cascade, evict, budget).
- Modify: `gateway/backend/internal/portal/service.go` (ChatStore interface +
  cipher use), `.../portal/api.go` + regen `.../portal/api_tracing_gen.go`.
- Modify: `gateway/backend/internal/portal/service_chats.go` (the `extractChatBlobs`
  hook, resolution, bounds), `gateway/backend/internal/gateway/chat_runs.go` /
  `chat_runs_images.go` (wire the hook at the commit; kind-aware resolution call).
- Create: `gateway/backend/internal/gateway/portal_chat_blob_endpoint.go` — the
  serving handler; Modify `.../gateway/server.go` (route), `error_map.go` (codes),
  `config/config.go` (the two bounds).
- Modify frontend: `gateway/frontend/src/components/shared/chatCapacity.ts` (delete),
  `components/chat/ChatStore.tsx` (remove the send refusal), `ImageTurn.tsx` +
  `shared/downloadBinary.ts` (download fix), `i18n.ts`.
- Modify docs: `09-architecture-decisions.md` (new ADR + amendments),
  `02-constraints.md`, `11-risks-and-technical-debt.md`,
  `reference/data-model.md`, `reference/api-surface.md`, `reference/openapi.yaml`,
  `cross-cutting/security-auth-rbac.md`, `cross-cutting/compatibility-and-inference.md`.

---

## Task 1: Migration v87 — `chat_blobs` table

**Files:**
- Modify: `gateway/backend/internal/store/migrate.go` (append v87 after v86)
- Test: `gateway/backend/internal/store/sqlite_migration_test.go` (table-exists assertion)

**Interfaces:**
- Produces: a `chat_blobs` table on both dialects, created by migration v87.

- [ ] **Step 1: Write the failing test** — assert the table exists after migration on a fresh sqlite DB (mirror the existing `chats`-exists assertion at `sqlite_migration_test.go:27`), and that `chat_id` has an `ON DELETE CASCADE` FK to `chats`.

- [ ] **Step 2: Run it, confirm it fails** (no such table).

- [ ] **Step 3: Append migration v87.** After the v86 entry, add a migration whose `up` runs this DDL through the dialect helpers (follow the `chats` baseline DDL at `migrate.go:540-548` for the substitution style — `dl.blobType()`, the timestamp type, and how an `on delete cascade` FK is written):

```sql
CREATE TABLE chat_blobs (
  id           text    PRIMARY KEY,
  chat_id      text    NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  key_version  integer NOT NULL,
  blob         <dl.blobType()> NOT NULL,
  content_type text    NOT NULL,
  byte_len     bigint  NOT NULL,
  created_at   <ts>    NOT NULL
);
CREATE INDEX idx_chat_blobs_chat ON chat_blobs(chat_id);
```

  Do **not** add this table to the frozen v1 baseline DDL. Register v87 in the
  migration list and, if `migrate.go:405-409` enumerates the substituted blob
  columns, add `chat_blobs.blob`.

- [ ] **Step 4: Run the migration test, confirm it passes** on sqlite; run the conformance migration test on postgres if the DSN is set.

- [ ] **Step 5: Commit** `feat(store): add chat_blobs table (migration v87)`.

## Task 2: `ChatBlob` DTO + sqlite store methods + allow-list + per-blob cap

**Files:**
- Create: `gateway/backend/internal/store/sqlite_chat_blobs.go`
- Modify: `gateway/backend/internal/store/sqlite_chats.go` (the DTO sibling, near `Chat`)
- Modify: `gateway/backend/internal/config/config.go` (`MaxChatBlobBytes`, default `20<<20`)
- Create/modify test: `gateway/backend/internal/store/sqlite_chat_blobs_test.go`

**Interfaces:**
- Produces: `store.ChatBlob{ID, ChatID string; KeyVersion int; Blob []byte; ContentType string; ByteLen int64; CreatedAt time.Time}`; and on `*SQLStore`:
  `PutChatBlob(ctx, ChatBlob) error`, `ChatBlobByID(ctx, id string) (ChatBlob, error)`, `ChatBlobOwner(ctx, id string) (chatID string, err error)`, `ChatBlobsByIDs(ctx, chatID string, ids []string) (map[string]ChatBlob, error)`.
- Consumes: `store.ErrNotFound` for a missing id (the store is a dumb byte store — **sealing/allow-listing/caps live in the service**, not here; this task adds only the raw CRUD + the DTO). Keep the allow-list + cap enforcement decision in the service layer (Task 6), but add the content_type column round-trip here.

- [ ] **Step 1: Write failing tests** in `sqlite_chat_blobs_test.go`: a put/get round trip (bytes, content_type, byte_len preserved); `ChatBlobByID` of a missing id returns `store.ErrNotFound`; `ChatBlobOwner` returns the `chat_id`; `ChatBlobsByIDs(chatID, [a,b])` returns only rows whose `chat_id == chatID` (a row from another chat with a matching id is excluded — ownership by construction). Use `openMigratedTestSQLite` as the other store tests do.

- [ ] **Step 2: Run, confirm they fail** (methods undefined).

- [ ] **Step 3: Implement** `sqlite_chat_blobs.go` with the four methods, using `dialect.rebind` for `?`→`$n` and the same error mapping (`sql.ErrNoRows → store.ErrNotFound`) as `sqlite_chats.go:89-102`. `ChatBlobsByIDs` issues one `SELECT … WHERE chat_id = ? AND id IN (?, …)`. Add `MaxChatBlobBytes = 20 << 20` to config with its env override following the `CaptureMemoryMaxBytes` precedent (`config.go:248`).

- [ ] **Step 4: Run the tests, confirm pass**; run `golangci-lint fmt --diff` and `run` in `gateway/backend`.

- [ ] **Step 5: Commit** `feat(store): chat_blobs DTO and sqlite CRUD (put/get/owner/batch)`.

## Task 3: `MemoryChatStore` blobs — cascade + evict + budget

**Files:**
- Modify: `gateway/backend/internal/store/memory_chats.go`
- Test: `gateway/backend/internal/store/memory_chat_blobs_test.go`

**Interfaces:**
- Produces: the same four methods on `*MemoryChatStore`, plus cascade + budget behavior.

- [ ] **Step 1: Write failing tests:** put/get round trip in memory; `DeleteChat(id)` drops that chat's blobs (a later `ChatBlobByID` → `ErrNotFound`); `evict()` of a chat (drive it by exceeding a small budget) drops its blobs **and** subtracts their bytes from `m.bytes`; blob bytes count toward the budget (putting blobs past the budget triggers eviction). Match the `memory_chats.go` test style.

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement:** add a `blobs map[string]ChatBlob` (and a `chatID→ids` index) guarded by the existing mutex; `PutChatBlob` adds `len(Blob)` to `m.bytes`; `DeleteChat` and `evict()` remove the chat's blobs and subtract their bytes (extend the eviction accounting at `memory_chats.go:147-155`); `ChatBlobsByIDs` filters by `chatID`. There is **no** users→chats cascade to emulate (no production DeleteUser), so only `DeleteChat`/`evict` matter.

- [ ] **Step 4: Run tests, confirm pass**; vet + golangci.

- [ ] **Step 5: Commit** `feat(store): memory chat_blobs with cascade, evict and budget accounting`.

## Task 4: `SaveChatWithBlobs` transactional primitive + three-way parity

**Files:**
- Modify: `gateway/backend/internal/store/sqlite_chats.go` (add `SaveChatWithBlobs`), `memory_chats.go`
- Test: `gateway/backend/internal/store/chats_test.go` / a conformance test added to `conformance_test.go`

**Interfaces:**
- Produces: `SaveChatWithBlobs(ctx, chat Chat, blobs []ChatBlob) error` on both drivers — writes all blobs and the chat row atomically (SQL `BeginTx` per `sqlite_capabilities.go:151` precedent; memory under the single mutex). Consumed by the service in Task 6.

- [ ] **Step 1: Write a failing conformance test** that runs on **all three** drivers (extend the `forEachDialect` pattern at `conformance_test.go:225-250`, and the sqlite-vs-memory parity): `SaveChatWithBlobs` with two blobs persists the chat and both blobs; a forced failure (e.g. a too-large blob rejected, or a cancelled ctx) persists **neither** the chat update nor the blobs (all-or-nothing).

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement** on `SQLStore` with a transaction (insert/update the blobs, then `UPDATE chats`), and on `MemoryChatStore` under the mutex (stage then apply). Add the postgres leg to the store test harness; document that the DSN must be set.

- [ ] **Step 4: Run on all three** (provision the Postgres DSN as CI does); golangci.

- [ ] **Step 5: Commit** `feat(store): SaveChatWithBlobs atomic primitive with three-way parity`.

## Task 5: `portal.ChatStore` interface + codegen

**Files:**
- Modify: `gateway/backend/internal/portal/service.go` (the `ChatStore` interface, ~:289-295)
- Modify: `gateway/backend/internal/portal/api.go`; regen `gateway/backend/internal/portal/api_tracing_gen.go`

**Interfaces:**
- Produces: the new store methods on the `portal.ChatStore` interface the service sees, so Task 6-8 can call them; the regenerated OTel wrapper.

- [ ] **Step 1: Write a failing compile/test** — reference `s.chats.PutChatBlob`/`ChatBlobsByIDs`/`ChatBlobOwner`/`SaveChatWithBlobs` from a small service test so the build fails until the interface carries them.

- [ ] **Step 2: Run, confirm fail** (interface does not declare them).

- [ ] **Step 3: Add** the four signatures to the `portal.ChatStore` interface; run `go generate ./...` for the interfacer/gowrap wrapper and **restore the SPDX header** (lines 1-2) of `api_tracing_gen.go` by hand; verify `MemoryChatStore` and `SQLStore` both still satisfy the interface.

- [ ] **Step 4: Build + the service test pass**; golangci (single sorted import group).

- [ ] **Step 5: Commit** `feat(portal): extend ChatStore with chat_blobs methods (regen tracing wrapper)`.

## Task 6: Extract-on-run-commit hook (the heart)

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (the `extractChatBlobs` helper; wire into `PrepareChatRun` and `writeAssistant`), `error_map.go` (new codes)
- Test: `gateway/backend/internal/portal/service_chats_blobs_test.go`

**Interfaces:**
- Consumes: `SaveChatWithBlobs`, `PutChatBlob`, `ChatBlobOwner`, the allow-list, `MaxChatBlobBytes`.
- Produces: a document whose inline `data:` image parts are stored and rewritten to `/api/portal/chat-blobs/{id}` before the seal; the returned run history still holds the just-uploaded bytes inline (no round trip) for the immediate run.

- [ ] **Step 1: Write failing tests:**
  - extract of a user turn with one inline `data:image/png` part: a `chat_blobs` row is written, the sealed doc's part url is `/api/portal/chat-blobs/blob_…`, and the pre-seal size is measured **after** rewrite (a 3.9 MiB inline image + text does not trip `ErrChatTooLarge`).
  - idempotence: a doc already carrying a gateway url stores **no** new blob.
  - allow-list: an inline `data:text/html` (or `image/svg+xml`) part is **rejected** (`images.blob_type_unsupported`, mapped 400) and nothing is stored.
  - per-blob cap: an inline image over `MaxChatBlobBytes` is rejected (`images.blob_too_large`).
  - `edited_history` ownership: a part already a `/chat-blobs/{id}` url whose id the owner does **not** hold in this chat is rejected (`images.blob_not_owned`); one they do hold is kept untouched.
  - create path: `CreateChat` with an inline `data:` image does **not** call extract (no FK failure) and succeeds; it is migrated by the first run.

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement** `extractChatBlobs(ctx, chatID string, doc *chatDoc) ([]store.ChatBlob, error)`: walk `doc.Messages` (parse each `RawMessage` to inspect its `content` parts; a part is `{type, image_url:{url}}`), and for each part whose `url` has the `data:` scheme: parse `data:<mediatype>;base64,<b64>`, **allow-list** the mediatype, `base64.StdEncoding.DecodeString`, enforce `len(bytes) <= MaxChatBlobBytes`, mint `blob_<hex>`, append a `ChatBlob{ID, ChatID: chatID, ContentType, ByteLen, Blob: s.seal(bytes)}`, and rewrite the part url to `"/api/portal/chat-blobs/"+id`. For a part already a `/chat-blobs/` url, call `ChatBlobOwner(id)` and require it `== chatID`. Return the blobs to persist. Then the caller seals the rewritten doc and calls `SaveChatWithBlobs(chat, blobs)`. Wire it:
  - in `PrepareChatRun`, after the messages are assembled (user turn or `edited_history`) and **before** `SaveChat` (switch that save to `SaveChatWithBlobs`); keep the returned `history` built from the pre-rewrite messages so the immediate run has the bytes.
  - in `writeAssistant`, before its save (same switch).
  - Do **not** wire it into `CreateChat` or the bare PUT `SaveChat`.
  - Map an FK error on commit (chat deleted mid-run) through the existing `commitFailureIsChatGone` path.
  - Reject non-standard base64/charset data urls (normalize to std padded on store).

- [ ] **Step 4: Run tests, confirm pass**; `-race` on the package; golangci.

- [ ] **Step 5: Commit** `feat(portal): extract inline chat images into chat_blobs at run commit`.

## Task 7: Owner-scoped, hardened serving endpoint

**Files:**
- Create: `gateway/backend/internal/gateway/portal_chat_blob_endpoint.go`
- Modify: `gateway/backend/internal/gateway/server.go` (route), `error_map.go`
- Test: `gateway/backend/internal/gateway/portal_chat_blob_endpoint_test.go`

**Interfaces:**
- Consumes: the portal session auth (as other `/api/portal/chats*` reads), `ChatBlobByID`, `ChatBlobOwner`, `openChat`-style unseal.
- Produces: `GET /api/portal/chat-blobs/{id}` → the raw bytes.

- [ ] **Step 1: Write failing tests:** owner GET returns 200, the bytes, `Content-Type: image/png`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'none'`, `Content-Disposition: inline`, `Cache-Control: private, no-store`; a non-owner GET returns **404** with a `chat-blob`-specific not-found code (not `portal.chat_not_found`) and no existence leak; a non-GET returns 405; a never-stored id returns 404.

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement** the handler mirroring `handlePortalChat*` auth; look up `ChatBlobOwner(id)` → the chat → `chats.user_id`, require `== session user` else 404 (distinct code via `error_map.go`); unseal the blob with the service cipher; write the hardening headers + the bytes. Register the route in `server.go`'s mux (a path-param pattern like the other `/api/portal/chats/{id}` routes). Use a lightweight session check that does not `TouchSession` on every hit (or add a short in-process positive cache) so a many-image chat open does not stampede the pool — pin a test that N parallel GETs issue no `TouchSession` write-storm.

- [ ] **Step 4: Run tests, confirm pass**; golangci; add a doc-parity note for the route (Task 11 covers openapi/api-surface).

- [ ] **Step 5: Commit** `feat(gateway): owner-scoped hardened GET /api/portal/chat-blobs/{id}`.

## Task 8: Resolution in `PrepareChatRun` (kind-aware, batched, remote-safe) + run bound

**Files:**
- Modify: `gateway/backend/internal/portal/service_chats.go` (resolution in `PrepareChatRun`), `config/config.go` (`MaxRunHistoryBytes`)
- Test: `gateway/backend/internal/portal/service_chats_resolve_test.go`

**Interfaces:**
- Consumes: `ChatBlobsByIDs(chatID, ids)`.
- Produces: a run `history` whose `/chat-blobs/{id}` urls are resolved to `data:` urls, for a vision/text run; the image run's history is left untouched (it uses only text).

- [ ] **Step 1: Write failing tests:** a later vision run over a thread whose stored history has `/chat-blobs/{id}` urls returns a history with those parts as `data:` urls (bytes round-trip byte-exact); the resolution issues **one** batched store call for N refs (not N); an image-kind run does **not** resolve (its history keeps the text-only path); a run whose total resolved history bytes exceed `MaxRunHistoryBytes` is refused with a clear code; a part whose id the chat does not own is dropped/refused (no cross-chat resolve).

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement:** in `PrepareChatRun`, when building the returned `history` for a **non-image** kind, collect all `/chat-blobs/{id}` ids across the history, `ChatBlobsByIDs(chatID, ids)` (ownership by construction), re-encode each as a standard-padded `data:<content_type>;base64,…` url, substitute, and sum `ByteLen`; refuse if `> MaxRunHistoryBytes`. Skip entirely for the image kind (the executor reads only `lastUserPrompt` text). For the **immediate** run, the just-uploaded part is still inline (Task 6 kept pre-rewrite bytes in the returned history), so no round trip is needed for it.

- [ ] **Step 4: Run tests (incl. a stub remote-provider path asserting real bytes reach it), confirm pass**; `-race`; golangci.

- [ ] **Step 5: Commit** `feat(portal): resolve chat_blobs refs to bytes for inference, bounded and kind-aware`.

## Task 9: Frontend — remove the image capacity estimate

**Files:**
- Modify/Delete: `gateway/frontend/src/components/shared/chatCapacity.ts`, its test
- Modify: `components/chat/ChatStore.tsx` (the `send()` image refusal ~:455-458, :1137-1141), `Chat.tsx` (the capacity line), `i18n.ts` (de+en strings)

**Interfaces:**
- Consumes: nothing new.
- Produces: no image-count capacity UI; text overflow stays the server `400 portal.chat_too_large`.

- [ ] **Step 1: Write/adjust failing tests:** the composer no longer renders the "room for N images" line; `send()` no longer refuses on image count; the existing capacity tests are deleted or rewritten to assert absence. (`vitest`.)

- [ ] **Step 2: Run, confirm the old tests fail / new assertions fail.**

- [ ] **Step 3: Remove** `imageCostBytes`/`imagesLeft`/`ESTIMATED_IMAGE_BYTES` and the UI that consumes them, the `send()` image refusal, and the `max_content_bytes`-for-images reliance; drop the now-unused i18n keys (both locales). Keep the server-side text 400 path untouched. Respect the arch boundary (no new `chatDoc` import in `Chat.tsx`).

- [ ] **Step 4: `vitest` + `tsc` + `prettier format:check` + `vite build` pass.**

- [ ] **Step 5: Commit** `refactor(frontend): drop the image-count capacity estimate (payloads now out of the transcript)`.

## Task 10: Frontend — download a `/chat-blobs/` image

**Files:**
- Modify: `gateway/frontend/src/components/shared/downloadBinary.ts`,
  `components/chat/ImageTurn.tsx` (the Save button ~:109)
- Test: `downloadBinary.test.ts`, `ImageTurn.test.tsx`

**Interfaces:**
- Consumes: the serving endpoint.
- Produces: a working Save button for a gateway-url image.

- [ ] **Step 1: Write failing tests:** `downloadBinary` for a `/api/portal/chat-blobs/{id}` url fetches the blob and triggers a download with an extension derived from the response `Content-Type`; a `data:` url still works (back-compat).

- [ ] **Step 2: Run, confirm fail.**

- [ ] **Step 3: Implement:** add a branch to `downloadBinary`/`extensionFor` that, for a non-`data:` url, `fetch(url)` → `blob()` → object URL → download, with `extensionFor` reading the blob's `type`. Keep the `data:` branch for old chats.

- [ ] **Step 4: `vitest` + `tsc` + `format:check` + `build` pass.**

- [ ] **Step 5: Commit** `fix(frontend): download chat images from the serving endpoint, not only data urls`.

## Task 11: ADRs + docs

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (new ADR + amend 043(c), 043(d)),
  `02-constraints.md`, `11-risks-and-technical-debt.md`,
  `reference/data-model.md`, `reference/api-surface.md`, `reference/openapi.yaml`,
  `cross-cutting/security-auth-rbac.md`, `cross-cutting/compatibility-and-inference.md`

- [ ] **Step 1:** Write the new ADR (next free number after ADR-055) recording the
  generic `chat_blobs` store, the derived owner, extract-on-run-commit, resolution
  in `PrepareChatRun`, the content-type allow-list + serving hardening, the
  replacement bounds, reuse-`image_url` and no-gzip; images the only v1 consumer.
  Inline-amend ADR-043(c) ("Rejected: blob store for v1 … #124" → resolved) and
  ADR-043(d) (the served-cap/`send()`-refuses mechanism removed), and
  `11-risks-and-technical-debt.md:224` (risk retired). Update `02-constraints.md`
  (image no longer rides inside the blob). Add the `chat_blobs` row + v87 to
  `data-model.md`; the serving route to `api-surface.md` + `openapi.yaml`; the
  sealed store + content-type hardening + ownership to `security-auth-rbac.md`; the
  `PrepareChatRun` resolution to `compatibility-and-inference.md`.

- [ ] **Step 2:** Run `make lint-docs` (checks 1-5 must pass; the branch-local
  `docs/superpowers/*` reference is removed before the PR).

- [ ] **Step 3: Commit** `docs: chat_blobs store, serving endpoint, resolution, and ADR`.

---

## Self-Review

- **Spec coverage:** store (T1-5), write/serve/resolve (T6-8), frontend (T9-10),
  docs/ADR (T11) — every spec Part maps to a task. ✓
- **Type consistency:** `store.ChatBlob` (T2) is the single shape; the interface
  (T5), the hook (T6), the serving handler (T7) and resolution (T8) all use it;
  `SaveChatWithBlobs` (T4) is the one atomic write the hook uses. ✓
- **Review Focus:** XSS (T2 store allow-list + T6 reject + T7 serve), leak (T6
  edited_history + T7 ownership), create-FK (T6), idempotence (T6), remote bytes
  (T8), cascade/evict (T3), bounds (T2/T8), back-compat (T6/T8) — each owned by a
  task with a test. ✓
- **Placeholder scan:** signatures, DDL, the hook/resolution logic, and the test
  cases are concrete; mechanical mirrors cite their precedent file:line. ✓
- **Ordering:** T1→T2/T3→T4→T5 (store) before T6-8 (service) before T9-10
  (frontend); T11 last. T2 and T3 are independent (sqlite vs memory) and may be
  reviewed in parallel but dispatched sequentially.

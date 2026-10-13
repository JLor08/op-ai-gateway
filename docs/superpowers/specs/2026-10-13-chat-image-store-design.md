# Chat binary payloads get their own store (issue #124) — design

**Goal:** move chat binary payloads (generated **and** uploaded) out of the sealed
chat-transcript blob into their own owner-scoped store, referenced from the
transcript by URL, so a thread is no longer limited to ~8–12 images by the
`maxChatContentBytes` cap — which bounds the *text* document and stays. New, sane
bounds replace the one shared 4 MiB budget (Part 8); "no limit" means "not that
tiny shared one," not "unbounded."

Resolves issue #124 (ADR-043(c) called it "the right end state"). **Pulled forward
before image-edit**: once payloads leave the transcript, image-edit's
refine/capacity workarounds disappear.

**Generic at the user's request:** the store is a **generic binary-payload** store
(`chat_blobs`, any content type), not image-specific. **Images are the only v1
consumer**, but the store, serving endpoint, extract hook and resolution are
**payload-type-agnostic**, so a future audio/PDF/file an operator hands a model
needs no new storage layer.

**Decided with the user:** scope = **both generated and uploaded** images;
retention = **chat lifetime** (cascade, no prune); **random id** per payload (no
dedup; refine references the existing id); **additive, backward-compatible**
migration (old inline `data:` parts still render); same cipher/KeyVersion seal.

> Verified against the code by six adversarial readers; the placements below
> reflect their corrections (the write-path site, the resolution site, the XSS
> hardening, the replacement bounds, the download path). Line anchors are
> approximate; the plan pins them.

## Grounding (verified)

- A transcript is **one `chats` row**, one opaque `blob` =
  `nonce||AEAD(gzip(JSON{settings, messages[]}))` (sealed, `KeyVersion 1`) or plain
  gzip (RAM fallback). No message rows; **full-replace** writes. `sealChat` takes
  the **raw doc bytes**, checks the cap, gzips, seals; it has **no ctx/owner/chatID**
  and returns `(keyVersion, blob)` — so the extract hook is a *separate* helper, not
  inside `sealChat`.
- `sealChat` has exactly two callers: `CreateChat` (`service_chats.go:127`) and
  `SaveChat` (`:192`). `PrepareChatRun` (`:547`), `writeAssistant` (`:728`) and
  `Checkpoint` all funnel through `SaveChat`.
- A payload is a base64 `data:` URL inside an `image_url` part
  (`{type:"image_url", image_url:{url}}`); `content` is a string or an array of
  parts. Generated parts come from `imagePartsFrom` (`chat_runs_images.go` ~:460),
  uploaded from `ChatStore.send` (~:1126); both are opaque `RawMessage` the server
  never interprets today.
- `MaxChatContentBytes = 4<<20` is checked **inside** `sealChat`
  (`service_chats.go:228`); `MaxChatRequestBytes = content + 1<<20` (5 MiB) bounds
  create/save/run bodies. **The text cap stays** (#124).
- Three drivers behind a dialect seam: `MemoryChatStore` (RAM, **no FK**, 64 MiB
  FIFO `evict()` over transcript-blob bytes only), `SQLStore` (sqlite + postgres,
  `foreign_keys(1)` forced on sqlite). Migrations append-only; next free **v87**;
  `bigint` for byte columns (ADR-005); blob via `dl.blobType()`; `chats` cascades
  from `users`. There is **no production DeleteUser** path, so no users→chats
  cascade is emulated in memory today.
- Upstream history: `buildAPIHistory` (`service_chats.go:556`) is a **pure** function
  (no store) with **one** caller, `PrepareChatRun` (`:551`), whose output feeds both
  the vision `/v1/chat/completions` body (`chat_runs.go:1136` `buildChatCompletionsBody`,
  gateway package, **no store handle**) and the image run (which reads only
  `lastUserPrompt` text, `chat_runs_images.go:301`).
- Remote providers need real bytes: `anthropicImageSourceFor`
  (`provider/anthropic_messages.go:590-611`) returns nil for a non-`data:`,
  non-`http(s)` url — a gateway url would **silently drop** the image.
- Adoption already exists on two of three write paths: `activateChat(created)`
  seeds from `created.content` (create); `adoptCanonicalTranscript`
  (`useChatRuns.ts:334-377`) refetches the chat after every terminal run. The run
  start returns `{run_id,…}` (no content echo); the SSE `done` carries the
  pre-rewrite parts.
- Capacity is **image-thread-only** frontend code: `imageCostBytes` = length of the
  largest `image_url.url` (`chatCapacity.ts:53-63`), `imagesLeft`, and the
  `send()` refusal (`ChatStore.tsx:455-458`, gated on `chatKind==='image'`). There
  is **no** frontend text-document guard. A text overflow is the server
  `400 portal.chat_too_large`.
- Ownership: every chat access checks `row.UserID == owner`, 404 no leak.
- The download button decodes client-side: `ImageTurn.tsx:109` →
  `downloadBinary` (`shared/downloadBinary.ts:21`) only accepts
  `^data:<mime>;base64,…`.

## The core idea

An image stays an `image_url` part, but its `url` becomes an **owner-scoped
gateway path** once the bytes live in `chat_blobs`:

```
 before:  image_url.url = "data:image/png;base64,iVBORw0K…"   (inside the sealed blob)
 after:   image_url.url = "/api/portal/chat-blobs/blob_<hex>"  (bytes in chat_blobs, sealed)
```

Reusing `image_url` keeps the **renderer** unchanged (`<img src=url>`) and keeps
**one wire format**. The reference mechanism is **generic** (a gateway url in any
part that can carry a `data:` url).

## Part 1 — The payload store (generic)

- **New table `chat_blobs`** (migration **v87**; dual-dialect; not back-ported to
  the frozen baseline):
  `id text primary key`,
  `chat_id text not null references chats(id) on delete cascade`,
  `key_version integer not null`,
  `blob <dl.blobType()> not null`  — `nonce||AEAD(bytes)` sealed, else plain,
  `content_type text not null`,
  `byte_len bigint not null`,
  `created_at <ts> not null`.
  Index on `chat_id`. **Owner is derived through `chat_id → chats.user_id`** (no
  denormalized `user_id` column — it could disagree with the chat's owner and the
  `users` cascade already reaches blobs transitively). Delete is cascade-only
  (chat lifetime).
- **Seal reuses the capture cipher** (`KeyVersion 1`), stores **decoded bytes**,
  **no gzip** (already-compressed media). No new decryptable secret → at-rest
  guarantee unchanged. RAM path: plain bytes, `KeyVersion 0`, volatile.
- **Store interface** (`portal.ChatStore` gains; `portal.API` is interfacer- and
  gowrap-generated → regenerate `api_tracing_gen.go`, restoring the SPDX header):
  `PutChatBlob(ctx, ChatBlob)`, a **metadata-only** `ChatBlobOwner(ctx, id)
  (chatID string, err error)` for cheap ownership checks, and a **batched**
  `ChatBlobsByIDs(ctx, chatID, ids)` returning bytes (ownership by construction,
  avoids N+1 on the resolution path). `ChatBlobByID` for the single-serve case.
  SQL in `store/sqlite_chat_blobs.go`; `MemoryChatStore` gets a parallel RAM map
  that **drops a chat's blobs on both `DeleteChat` and `evict()`**, and whose blob
  bytes **count against the 64 MiB memory budget** (else the RAM path is
  unbounded). Three-way parity tests (extend the existing sqlite+memory chat
  conformance; add the postgres leg like other store tests).

## Part 2 — Write path: extract on the run commits (not create, not bare save)

**The hook is `extractChatBlobs(ctx, chatID, content) (rewritten []byte, err)`:**
it walks the doc's messages, and for every content part carrying an inline `data:`
url it validates the media type (Part 3 allow-list), decodes, `PutChatBlob`
(`blob_<32 hex>`, the chat id, content-type), and rewrites the url to
`/api/portal/chat-blobs/{id}`. It runs **after** the chat row exists and **before**
the `sealChat` cap check, at exactly the two run-commit paths:

- `PrepareChatRun` — after it assembles the final `doc.Messages` (append user turn,
  or the wholesale `edited_history` replacement) and **before** its `SaveChat`.
  This covers an **uploaded** source and any `edited_history` image.
- `writeAssistant` — before its `SaveChat`. This covers a **generated** image.

It does **not** run on `CreateChat` (the chat row isn't minted yet → the
`chat_id` FK would fail; and a created/imported chat is ≤ the old cap, so it needs
no extraction) **nor on the bare autosave PUT** (the composer never persists a new
large payload until it is sent through a run; a bare title/text edit carries only
already-stored urls). This placement **dodges both the create-FK ordering bug and
the autosave re-extract/orphan problem** the first draft had.

- **Adoption is then free:** the post-run `adoptCanonicalTranscript` refetch
  delivers the rewritten gateway urls to the client; `activateChat` covers create.
  No autosave-merge, no new adoption code. (The SSE `done` still shows the
  pre-rewrite `data:` image briefly; the refetch swaps it — acceptable, or the
  commit can carry rewritten parts later as a polish, which would touch
  `CommitAssistant`'s signature.)
- **Idempotent:** an already-gateway url is skipped.
- **Extract before the cap check**, so a legacy ~3.9 MiB thread can still take its
  next turn (lazy migration on first run).
- **edited_history with gateway urls:** a part already a `/chat-blobs/{id}` url
  must reference a blob the **owner holds in this chat** (`ChatBlobOwner` check);
  else rejected — a client cannot graft another chat's/user's id.
- **Base64 fidelity:** store decoded bytes + exact media type; re-encode as
  standard padded base64 on resolution. Reject/normalize non-standard
  (url-safe/unpadded/charset-param) data urls so round-trips are byte-exact.
- **Delete-mid-run race:** a `PutChatBlob` whose chat was deleted mid-run hits an
  FK error; map it like the existing `commitFailureIsChatGone` so the run ends
  cleanly, not as a 500.
- **Transactionality:** prefer a `SaveChatWithBlobs(ctx, chat, blobs)` that writes
  the blobs and the chat in one store transaction (SQL `BeginTx` precedent;
  Memory's single mutex makes it trivial), so a failed seal leaves no orphan.

## Part 3 — Serving endpoint (owner-scoped, hardened)

- **`GET /api/portal/chat-blobs/{id}`**, portal session-authed. Resolve
  `chat_id → chats.user_id`, require `== session user` (404, no existence leak,
  with a `chat-blob`-specific not-found code, not `portal.chat_not_found`), unseal,
  write raw bytes.
- **XSS hardening (blocking):** the content-type is attacker-influenced (it came
  from a client `data:` url), and this is a same-origin, cookie-authed, navigable
  URL. So: **allow-list** stored media types to a safe set (`image/png`,
  `image/jpeg`, `image/webp`, `image/gif`; extend deliberately per future
  consumer) and **reject** `text/html`, `image/svg+xml`, and anything not
  allow-listed at **store** time. Serve with `X-Content-Type-Options: nosniff`, a
  restrictive `Content-Security-Policy: default-src 'none'`, and
  `Content-Disposition: inline` (an `<img>` is fine; a navigation cannot execute).
  Never echo an un-validated type.
- **Caching:** the id is immutable, but the bytes are decrypted and private — use
  `Cache-Control: private, no-store` (or a short `max-age`) rather than a
  year-long `immutable`, to avoid long-lived decrypted payloads in the browser
  disk cache. Accept the re-fetch cost; see session-ladder note.
- **Session-ladder cost:** each uncached `<img>` runs
  `SessionBySecret + UserByID + TouchSession` (a write). A many-image thread opens
  N in parallel against a 20-conn pool. Mitigate with a lightweight read path for
  this endpoint (skip `TouchSession`, or a short in-process cache) — pin a test
  that opening a 50-image chat does not stampede the pool.
- `requireMethod(GET)` (405 on others, fine for `<img>`); it is **not** an
  inference endpoint (no bearer/internal path).

## Part 4 — Upstream resolution (one place, kind-aware, remote-safe)

Resolution lives in **`PrepareChatRun`** (it has the ctx, the owner, and the store;
`buildAPIHistory` is pure and `buildChatCompletionsBody` has no store handle). When
`PrepareChatRun` builds the `history` it returns for the run, it **resolves every
`/chat-blobs/{id}` url in that history back to a `data:` url** via the batched
`ChatBlobsByIDs(chatID, …)` (one query, ownership by construction), so the upstream
request — **including a remote provider** (Anthropic/OpenAI) that cannot reach the
gateway — gets real bytes, exactly as today.

- **Kind-aware:** only vision/text runs carry history images; the image run uses
  only `lastUserPrompt` text, so it needs no resolution (skip it there). For the
  immediate run, `PrepareChatRun` already holds the just-uploaded bytes and can
  keep them inline in the returned history without a round-trip; a **later** run
  that re-reads the chat resolves from the store.

## Part 5 — Frontend

- **Capacity: delete it.** `imageCostBytes`/`imagesLeft`/`ESTIMATED_IMAGE_BYTES`
  and the image-count UI do **not** shrink harmlessly (a ~47-char url makes the
  divisor ~47 → nonsense "millions left"). Remove the image capacity estimate,
  the served `max_content_bytes` reliance for it, the `send()` image refusal, and
  their tests. Nothing frontend-side replaces it (text overflow stays the server
  400). The new **storage/upstream bounds live server-side** (Part 8).
- **Download button (required fix):** `downloadBinary`/`extensionFor` only handle
  `data:` urls. Teach the generated-image Save path to `fetch(url)` → blob →
  download for a `/chat-blobs/` url, deriving the extension from the response
  content-type.
- **Render:** unchanged (`<img>` hits the endpoint). The keepalive size branch
  (`useChatPersistence`) is a pure byte test and simply starts firing once
  transcripts are small — correct, no change.
- **Adoption:** free via the existing refetch + `activateChat` (Part 2).
- **i18n:** remove the capacity strings.

## Part 6 — Migration & backward compatibility

- **Additive.** Old chats keep inline `data:` parts and **render** unchanged. No
  forced migration. A legacy chat migrates **on its next run** (the run-path hook);
  a never-run legacy chat stays inline and works. Resolution (Part 4) handles both
  forms (a `data:` url passes through; a gateway url resolves).

## Part 7 — ADRs and docs

- **New ADR** (next free; last is ADR-055): the generic owner-scoped
  same-cipher-sealed `chat_blobs` store, referenced by a gateway url in the
  carrying part; extract-on-run-commit; derived owner; the content-type allow-list
  + serving hardening; the replacement bounds; images the only v1 consumer. Records
  reuse-`image_url` and no-gzip.
- **Amend** ADR-043(c) (the "Rejected: blob store for v1 … #124" is now resolved),
  ADR-043(d) (`~:1893-1910`, the served-cap / `send()`-refuses mechanism being
  removed), `02-constraints.md` (image "rides inside that blob"),
  `11-risks-and-technical-debt.md:224` (the accepted-inline-image risk is retired).
  Reference ADR-005.
- **Docs:** `reference/data-model.md` (`chat_blobs` + v87),
  `reference/api-surface.md` + `openapi.yaml` (`GET /api/portal/chat-blobs/{id}`),
  `cross-cutting/security-auth-rbac.md` (the sealed store + the content-type
  hardening + ownership), `cross-cutting/compatibility-and-inference.md`
  (resolution in `PrepareChatRun`). `make lint-docs` passes.

## Part 8 — Replacement bounds (the 4 MiB cap was the only bound)

Removing images from the document removes the only limit on stored and in-flight
image bytes. Replace it, explicitly:

- **Per-blob cap** (`MaxChatBlobBytes`, configurable, default e.g. 20 MiB): one
  payload cannot be arbitrarily large. Enforced at `PutChatBlob`/extract.
- **Per-run upstream-history cap** (`MaxRunHistoryBytes`, configurable): bound the
  total resolved bytes a single inference request carries, so a 50-image vision
  thread cannot assemble a 100 MB `/v1/chat/completions` body. On overflow, refuse
  the run with a clear code (or trim oldest, decided in the plan).
- **Per-chat growth** is bounded by chat lifetime + cascade; a soft per-chat blob
  count/bytes ceiling is **deferred** unless the plan finds it needed.
- The **5 MiB request cap** stays (one upload still rides inline in the run body);
  a dedicated upload endpoint (larger single uploads) is deferred.

These are the numbers to confirm at review — they define what "no 4 MiB limit"
means in practice.

## Review focus (tests must pin)

- **Round trip:** a generated image is stored, referenced, served with the right
  Content-Type, renders; the transcript blob is tiny.
- **XSS:** a `data:text/html` / `image/svg+xml` upload is **rejected at store**;
  the endpoint sends `nosniff` + restrictive CSP and never serves an un-allow-listed
  type.
- **Ownership:** another user's blob 404s; a forged `/chat-blobs/{id}` in
  `edited_history` cannot resolve/serve another chat's bytes.
- **Create-FK:** a `CreateChat` with an inline `data:` image does **not** FK-fail
  (no extraction on create); it migrates on first run.
- **No re-upload / orphans:** a legacy chat migrated by a run stores each image
  **once**; the autosave path stores nothing new.
- **Remote provider:** a vision turn over a migrated thread sends real bytes to
  Anthropic/OpenAI (resolved), not a gateway url.
- **Cascade:** deleting a chat drops its `chat_blobs` on all three drivers;
  memory `evict()` drops them too and frees the budget.
- **Bounds:** a payload over `MaxChatBlobBytes` is refused; a run whose resolved
  history exceeds `MaxRunHistoryBytes` is refused/trimmed.
- **Download:** the Save button downloads a `/chat-blobs/` image.
- **Store parity:** memory == sqlite == postgres.
- **Back-compat:** an un-migrated inline chat renders and runs unchanged.

## Delivery phasing

- **A — Store:** v87 `chat_blobs`, DTO + `PutChatBlob`/`ChatBlobOwner`/
  `ChatBlobsByIDs`/`ChatBlobByID` in SQL + memory (cascade + evict + budget),
  seal, `SaveChatWithBlobs`, `api_tracing_gen` regen, three-way parity, the
  content-type allow-list, `MaxChatBlobBytes`.
- **B — Write + serve + resolve:** the extract hook at the two run commits, the
  hardened serving endpoint, resolution in `PrepareChatRun` (kind-aware, batched,
  remote-safe), `MaxRunHistoryBytes`, the `edited_history` ownership check,
  delete-mid-run mapping.
- **C — Frontend:** delete the capacity estimate, fix the download path, adoption
  verification, i18n.
- **D — Migration/back-compat + docs + ADR.**

## Out of scope / deferred

- **Non-image payload types** (audio/PDF/file parts): the store/plumbing are
  generic and ready; v1 ships no new part shape, renderer, or composer affordance.
- A dedicated **upload endpoint** and **content-hash dedup**.
- A **forced** one-time migration; a **soft per-chat** blob ceiling; the
  commit-carries-rewritten-parts polish.
- **Raising/removing** `maxChatContentBytes`, the image-generation endpoint, or the
  capability gate (out per #124).
- Thumbnails/transforms/CDN at the serving endpoint.

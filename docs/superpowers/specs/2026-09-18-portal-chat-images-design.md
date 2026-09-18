# Design: image generation in the portal chat

Status: draft for review
Date: 2026-09-18
Branch: `feat/portal-chat-images`
Follows: issue #71 (`POST /v1/images/generations`, merged as `b2bbb01`)
Follow-up filed: issue #124 (a store of their own for chat image payloads)

## 1. Goal

A portal-chat user picks a model that can generate images, types a prompt, and
gets an image back in the thread — persisted with the conversation, visible on
reload, and billed like any other turn.

Issue #71 built the endpoint. Nothing reaches it from the portal. This closes
that gap and adds the operator UI without which the endpoint cannot be turned
on at all.

## 2. What exists today

Verified against `b2bbb01`.

**The endpoint works and is closed to the portal.** `handleOpenAIImages`
authenticates with `requireAnyScope`
(`gateway/backend/internal/gateway/images_handler.go:284`), which is
**bearer-only** — it calls `authenticate`, which reads `LookupBearer` and
nothing else (`server.go:1341-1352`, and the doc on `requireAnyScope` at
`:1366-1380` says so). The portal-chat run executor authenticates over the
internal loopback pair as a token-less session principal, so it cannot reach
this endpoint at all.

**The run executor speaks one URL.** `executeRun` posts to
`s.selfBaseURL+"/v1/chat/completions"` — a literal, not a variable
(`chat_runs.go:487`) — and `buildChatCompletionsBody` emits exactly
`{model, messages, stream, stream_options, temperature?, max_tokens?}`
(`:681-699`). There is no second target and no request shape but that one.

**The transcript already carries images.** A chat is one sealed blob per row
(`store/migrate.go:535-542`), and a message's content is
`json.RawMessage` — already structured and opaque to the persistence layer
(`portal/service_chats.go:274-277`). `buildAPIHistory` passes that content
through verbatim (`:418-434`). The frontend already has the renderer for OpenAI-style
`image_url` parts: `contentImages` filters `part.type === 'image_url'` and
maps `part.image_url.url` (`frontend/src/components/ChatMessage.tsx:24-31`).

**But not on the assistant path.** `contentImages` is called at
`ChatMessage.tsx:298`, and the `role === 'assistant'` branch returns at `:89`
— so an assistant message never reaches it. Today that is unobservable
(assistants only ever produced text); for this feature it means the call has
to be hoisted above the assistant branch, not merely reused.

**The size cap is on the assistant write path.** `writeAssistant` ends in
`SaveChat` (`service_chats.go:571`), which seals through `sealChat`
(`:172`), which is where `ErrChatTooLarge` comes from. So an over-cap image
turn fails exactly at `CommitAssistant` — the error `finishRun` swallows.

**The capability has no writer.** `routing.CapabilityImage` exists
(`routing/store.go:1121`) and gates the endpoint, but the admin capability
write loop covers only `mtp` and `vision`
(`frontend/src/components/MappingForm.tsx:322-323`), and the display list in
`ModelServersSection.tsx:176-180` shows vision, video, audio, tools and
speculation_observed. So an operator can neither set nor see the verdict that
decides whether this feature does anything. Issue #71 left
`(image, yes)` writable on purpose for exactly this reason — see the comment
on `reservedManualVerdicts` at `portal/service_applications.go:278-294`.

**A failed persist is silent.** `finishRun` logs a failed `CommitAssistant`
with `log.Printf` under an explicit `// control flow is unchanged` comment
(`chat_runs.go:657-679`), leaving the trailing assistant message stuck at
`pending` — which a later restart reads as a false `interrupted`.

## 3. Design

### 3.1 The picked model is the affordance

No toggle, no "generate an image" button, no sniffing the prompt for intent.
The chat's model selector already decides what the turn can do; a model whose
`image` verdict is `yes` produces images, and the composer's behaviour follows
from the pick.

This mirrors how `vision` already works: `portal.ModelDTO.Vision`
(`portal/service.go:998`) is AND-folded across a mapping's servers in
`modelsResponse` (`:2023`, the fold at `:2105`), and the attach button is enabled or not from
that one flag. Adding a second, independent axis of user intent would mean the
UI can ask for an image from a model that cannot make one — a state the
capability gate would then have to reject at the bottom of the stack, after
the user already committed a prompt.

**Rejected: a mode toggle.** It makes the failure case reachable for no gain,
and it forces a second question ("what do I do when the toggle is on and the
model says no?") whose only good answer is to disable the toggle — which is
the design above, with extra steps.

### 3.2 Routing the run: a second target, not a second executor

`executeRun` learns to choose its URL and body shape from the resolved model's
image verdict, instead of hardcoding chat completions. Everything else about a
run — the SSE fan-out to subscribers, the run registry, eviction, the
terminal commit — is shape-independent and stays exactly as it is.

The images request the executor builds sends `response_format: "b64_json"`
**explicitly**. `validateImagesRequest` accepts absent-or-`b64_json` and
rejects anything else (`images_handler.go:378` and its doc), and sd-server
returns that shape regardless; sending it explicitly states the contract the
executor depends on rather than inheriting it from a default.

The images endpoint is **not** streamed — `stream` is refused outright with
`images.stream_unsupported`, and the gateway has pinned itself to the buffered
path there. So the run emits its terminal event when the relay returns, with
no incremental deltas. The run's existing event envelope carries that without
change; what it must not do is fake token deltas for a response that has none.

### 3.3 Auth: a narrow loopback-or-bearer leg

`handleOpenAIImages` moves from `requireAnyScope` to a new helper that accepts
the **internal loopback pair or a bearer token** — `authenticateWeb`
(`auth.go:77-113`) minus its cookie leg.

The browser never calls this endpoint. Only the run executor does, over the
loopback pair (`chat_runs.go:494-495`). So the cookie leg is not needed, and
granting it would make `/v1/images/generations` directly reachable from a
browser session — which would falsify `docs/architecture/02-constraints.md:38`
("/v1/chat/completions also accepts the session; the other inference endpoints
are bearer-only") for browsers, as a side effect of a feature that never
wanted it.

**Rejected: switch to `requireWebAnyScope`.** One-line change, and it grants
strictly more than the feature needs. The narrow helper keeps the documented
boundary literally true and still admits the executor.

Session attribution needs no work: the executor already sets
`sessionHeaderName` to the chat id (`chat_runs.go:496`), and the explicit
session-override header is read **before** the per-endpoint switch, tagging
the request `source = "chat"` when the internal auth header is present
(`session_extract.go:61-68`). The endpoint-specific gap documented at
`session_extract.go:83` and `:130` is about clients that send no header at
all, which the executor is not.

Run-as attribution is copied from the chat path: the block at
`inference_handlers.go:42-51`, gated on `token.ID == ""`, applies
`X-OP-Run-As-Token`. Without it an image turn bills to a different principal
than the text turn beside it in the same thread.

### 3.4 Persistence: inline in the sealed transcript, and loud when it does not fit

The generated image is stored as an OpenAI-style `image_url` content part
carrying a data URL, inside the sealed chat blob — the same shape an uploaded
vision image already uses, which means `buildAPIHistory` feeds it back as a
vision input on the next turn for free, and the existing `contentImages`
renderer applies once it is hoisted past the assistant branch (§2).

This is policy-compliant, not a hole in the no-persist rule:
`docs/architecture/cross-cutting/security-auth-rbac.md:687` names chat
transcripts among what the capture encryption key seals, so an inline image
inherits the same guarantee as the prompts already in that blob.

`AssistantTurn.Content` is a `string` and `writeAssistant` sets
`"content": turn.Content` unconditionally (`service_chats.go:487-489`, `:537-540`), so
the turn type widens to carry structured content. The widening must keep the
plain-string case byte-identical on the wire — every existing chat is that
shape.

**The 4 MiB ceiling is accepted for v1, and made loud.** `sealChat` rejects a
pre-seal document over `maxChatContentBytes = 4 << 20` (`:42-43`, enforced at
`:206-209` with `ErrChatTooLarge`), measured against the **whole** document,
so images share one budget for the thread's life. With the silent-commit
behaviour above, an over-cap turn renders and then vanishes on reload with
only a log line — so `finishRun`'s failed terminal commit must surface to the
user as a failed turn instead of being swallowed. Lifting the ceiling itself
is issue #124.

**Rejected for v1: a separate blob store.** It is the right end state and it
is a schema change across three drivers, an owner-scoped serving endpoint, a
delete-cascade and a re-litigated sealing decision — none of which this
feature needs to work. Filed as #124.

**Rejected: server-side re-encode to fit the cap.** The client already
downscales its **own uploads** above 1568px, keeping the original data URL
below it (`frontend/src/components/Chat.test.tsx:208-212`). Re-encoding what the *model produced* is
different: it silently degrades the artifact the user asked for. Upstream
bytes are stored as-is, with a download button so the user can keep the
original.

### 3.5 Operator UI

Without this the feature ships dead: the gate defaults to refusing, and no
screen can change the verdict.

- `MappingForm.tsx:322-323` — add `image` to the capability write loop, so an
  operator can set `(image, yes)`.
- `ModelServersSection.tsx:176-180` — add `image` to the displayed capability
  list, so the verdict is visible where the others are.

`(image, no)` stays reserved (`service_applications.go:278-294`); the UI
writes the enabling verdict, not the refusing one.

### 3.6 The composer: lead with the number that is exact

A design panel of four independent designs plus a completeness critic produced
one finding that inverts the obvious approach. All four designs spent their
effort on the one quantity **nobody can bound** — progress — and all four
withheld the one that is **exact and known before the user commits**: the
remaining transcript budget.

So the composer leads with capacity, not with a clock.

**Before Send.** The prompt field's label becomes "Bildprompt" — the only
affordance signal, and a true statement about what the field now does. Beside
it, a capacity line: the room left in this chat, computed from the current
`buildDoc()` size against the cap. The document size is client-side and exact,
and the cap is checked pre-gzip (`service_chats.go:208`), so base64 pays full
price and the arithmetic is honest without modelling compression.

**The cap itself is not available to the client.** `maxChatContentBytes` is
package-private to `portal` (`service_chats.go:43`) and no DTO carries it. It
must be **served**, not duplicated: a second 4 MiB literal in TypeScript would
drift from the Go constant with nothing to catch it, and the failure mode is a
capacity line that confidently states the wrong number. Expose it once and
read it.

When the budget cannot hold another image, **Send refuses up front**. This is
the repo's own rule applied one layer up: `validateImagesRequest` 400s a
`response_format` it cannot honor rather than relaying and mis-measuring, and
the same logic forbids spending minutes of sd_cpp CPU on an artifact we can
already prove we cannot store. Every panel design surfaced the cap only
*afterwards* — toasts, chips, download rescues — which is four recovery
mechanisms for a failure that can simply be declined.

**During generation.** Three elements, each backed by a value that exists:

- A liveness dot, from the run's server-reported `running` status — the same
  claim the sidebar's existing indicator already makes, and no more.
- An elapsed clock from the run's server-reported age, labelled as the
  **wait**, not the work: "Warte auf Bild · 1:47". **No such age exists
  today** — the only `time.Time` on a `ChatRun` is `endedAt`
  (`chat_runs.go:168`), so a start instant has to be added and surfaced on the
  snapshot, the active-runs DTO and the start response. Deriving it client-side
  from the moment of Send is not equivalent: a reopened tab never saw that
  moment. The `m:ss` formatter the design wants already exists as
  `formatElapsed` (`ActiveRequestsPanel.tsx:13-19`) but is **not exported**, so
  it needs lifting into a shared module rather than copying. Not "Bild wird erzeugt":
  between dispatch and terminal the request may still be queued for admission
  or waiting on a model load, during which "is being generated" is false.
  "Warte auf Bild" is true for the whole span and makes the clock's referent
  unambiguous. The clock is `aria-hidden`, because the transcript box is
  `aria-live="polite"` (`Chat.tsx:266-269`) and a once-per-second announcement
  would make the thread unusable with a screen reader.
- One static sentence stating the structural truth: there are no intermediate
  messages; the image arrives finished or not at all.

**The character counter is absent, not zero.** That distinction is not
invented here — it is the same one `proxyNative` already makes one layer down,
where a buffered relay gets `progress = nil` rather than `&requestProgress{}`
precisely because "measured 0" asserts something different from "nothing to
measure". Rendering `0 Zeichen` for a three-minute wait would be the exact
class of silently-wrong measurement this codebase rejects everywhere else.

**A bound, because one exists.** `reserveRun` creates the run's context itself
with `context.WithCancel` (`chat_runs.go:436-444`). Making that a
`context.WithTimeout` gives the run a deadline it owns outright — no plumbing
through `proxyNative`, and self-consistent because the deadline is what ends
the run. "This finishes or fails within N minutes" then becomes a
configuration value rather than an estimate, and it is the one thing that
turns an open-ended wait into a bounded one. Without it the most likely real
failure is a user cancelling a run that would have succeeded.

Two traps make this more than a one-line change, both verified:

- **A timeout would be reported as a user cancel.** `executeRun` turns any
  context error into `finishRun(..., "canceled", "")` with an empty message
  (`chat_runs.go:520`). A deadline firing would therefore be indistinguishable
  from the user pressing Stop — the exact silent-conflation this design
  rejects elsewhere. The timeout needs its own terminal status or message.
- **The deadline could abort its own commit.** `finishRun` is called with the
  run's own context on three paths (`:484`, `:489`, `:529`). Once that context
  carries a deadline, the terminal `CommitAssistant` inside `finishRun` can be
  cancelled by the very timeout that ended the run — losing the turn instead
  of recording it. The commit must run on a context that does not carry the
  run's deadline.

**Cancel says what it discards.** For a buffered relay there is no partial
result: at the moment of cancel nothing has been received, so Stop loses the
entire generation and the upstream may keep burning CPU regardless. That is
not true of the text path, where the received deltas are kept, so the image
path must say it rather than inherit the text path's silence.

**Every terminal state gets a distinguishable ending.** Today all three end
the same way — silently — because `finishRun` prunes an assistant bubble it
considers empty, and a zero-event turn is *always* empty until the terminal
moment. So cancel, an interrupted run, and a failed commit all currently
leave the user's prompt sitting alone with no trace a run happened. See
§7.1: this is not polish, it is the same defect that deletes a successful
image.

## 4. Out of scope

- Lifting or working around the 4 MiB transcript cap (issue #124).
- Image **editing** or variations endpoints; only `images/generations`.
- Any change to the capability gate's routing logic, the endpoint's
  validation, or its billing — #71 settled those.
- Streaming or partial images.

## 5. Decided: an image model makes the thread image-only

A model whose `image` verdict is `yes` sends every turn to
`/v1/images/generations`. There is no branch in which the portal sends a chat
completion to such a model.

This follows from §3.1 — the model is the affordance — and it is the reason
that principle is worth anything: one axis, so the composer can never offer
something the gate will refuse.

Three consequences the implementation must carry:

- **The attach button stays vision-gated, and that gate is a different
  capability.** It is disabled unless `c.modelVisionCapable`
  (`Chat.tsx:391`), and `image` is not `vision`. So in an image-only thread the
  attach button is disabled unless the same mapping also declares vision. That
  is correct — an image *generator* has no reason to accept an image *input* —
  but it must be deliberate rather than incidental, because the two capability
  names are one word apart — and the existing UI already blurs them: the attach
  button's disabled tooltip uses the i18n key `chatImageModelUnsupported`
  (`Chat.tsx:384`), which today means "this model cannot accept an image as
  *input*". Its text is worse than its name: `'Dieses Modell unterstützt keine
  Bilder.'` / `'This model does not support images.'` (`i18n.ts:191`, `:2539`).
  On an image-generating model without vision, the portal would say that model
  does not support images — about a model whose entire job is images. The
  string has to say what it actually gates (image *input*), not "images".
- **History still renders both shapes.** The thread's model can change between
  turns, so a thread containing text turns and image turns is reachable and
  normal. Image-only constrains what the composer *offers now*, not what the
  transcript *contains*.
- **A hypothetically multimodal mapping loses its text path in the portal.**
  If one mapping ever declared `image = yes` while also serving chat, this rule
  makes it image-only there. Accepted for v1: the capability table is per
  mapping, and today an `image = yes` mapping is an sd-server process, which
  serves one model and does not do chat at all. Revisit when a mapping that is
  genuinely both exists — not before, because the alternative is the toggle
  §3.1 rejects.

## 6. Decided: the kind is pinned to the thread at first send

§5 says an image model makes the thread image-only. As written that is a
property of the **thread**; implemented naively it is a property of the
**currently-picked model**, which the user can change between every turn. The
gap is not cosmetic, and it is worse in the direction nobody expects:

- Switching an image thread to a **text** model hits §7.2's role-blind
  `historyHasImage` guard and is refused. Accidentally correct, and
  unexplained.
- Switching an image thread to a **vision** model *lifts* that guard — and the
  entire multi-megabyte image history is then POSTed to
  `/v1/chat/completions` as vision input. A real cost and privacy surprise, on
  a thread the rule called image-only.

So the kind is **pinned to the thread on its first send** and persisted.

`chatDoc` already carries `Settings json.RawMessage` beside `Messages`
(`service_chats.go:316-318`), typed as `ChatRunSettings` (`:280-301`), inside
the same sealed blob. The pin is a new field there — a `kind` string rather
than an `image_only` boolean, because a string is the same value that selects
the request URL (§3.2) and extends to a future audio or speech kind without a
second flag. It carries `omitempty`, so every existing chat stays
byte-identical on the wire and an absent kind reads as text.

Once pinned, the model picker filters to models of that kind. The thread's
kind, not the picker's current value, decides what the composer offers.

**The pin must be enforced server-side, and the obvious place does not work.**
`PrepareChatRun` does not read the persisted settings blob at all — it
**replaces** it wholesale with the settings the client submitted on this
request:

```go
settingsRaw, err := json.Marshal(req.Settings)   // req, i.e. the CLIENT's settings
...
doc.Settings = settingsRaw                        // service_chats.go:399
```

So a `kind` written into `doc.Settings` and left there is overwritten by the
next run's submitted settings. Worse, `startRunRequest.Settings` is
`portal.ChatRunSettings` **verbatim** (`chat_run_endpoints.go:23`), so every
field added to that struct becomes client-settable over the API the moment it
exists. A naive pin is therefore not a pin: the client sets it.

The pin must be read from the stored document **before** the overwrite and
re-imposed on the submitted settings — a first send establishes it, and every
later send has it forced back regardless of what was submitted.

This also corrects a misreading in an earlier draft of this spec: the
`ServerOverride` self-heal is **not** an example of distrusting storage,
because nothing here reads storage. It re-validates the value the *client
submitted in this request* (`req.Settings.ServerOverride`, `:390`). It is
still the right precedent, but for a different reason than stated — it shows
that this function is where a submitted setting gets overridden by a
server-side truth, which is exactly the hook the pin needs.

**And the pin is still a UI constraint, not an authorization.** A thread
pinned to `image` whose model later loses its `image` verdict — operator
revoked, mapping changed — must fail the capability gate exactly as it would
unpinned. The pin decides what the composer offers; `filterCapable` decides
what the gateway serves and stays the only authority. Given that the field is
client-settable, this is a requirement rather than a nicety.

## 7. Pre-existing defects this feature walks into

Found by the design panel, each verified by reading the code rather than taken
on report. None of them are caused by this feature; all of them are reachable
*because* of it, and several are silent data loss. They are in scope.

### 7.1 The happy path deletes the image

`useChatRuns.ts:177-178`:

```ts
const empty =
  (typeof last.content === 'string' ? last.content.length === 0 : true) && !last.reasoning;
if (empty) return prev.slice(0, -1);
```

Any **non-string** content counts as empty, and the bubble is then sliced off.

**There are two such prunes and only this one is wrong.**
`pruneEmptyAssistantTail` (`chatDoc.ts:264-267`), which `buildDoc` runs on
every save, tests `last.content.length === 0` under the comment "`.length`
covers both the string and (defensively) the array shape" — correct for an
array, because a one-part image array has length 1. Only the `useChatRuns`
copy takes the `typeof` branch. Fix that one; do not "align" them.
An image written as an array of content parts is therefore deleted after a
successful generation *and* a successful persist — it survives only via the
best-effort canonical refetch (`:196-204`, whose own comment says a failed
refetch leaves the buffer as-is). The fix is on the content-shape axis; a
status-based narrowing does not work, because `completed` is exactly the
status an arriving image carries.

### 7.2 Edit and Regenerate die from the second image turn on

`ChatStore.tsx:720` guards on `!modelVisionCapableRef.current &&
historyHasImage(history)`, and `historyHasImage` (`chatDoc.ts:273-277`) is
role-blind — it matches the assistant's own generated images. An image
generator is not vision-capable, so from the second turn onward editing or
regenerating is refused, with the message from §5: "Dieses Modell unterstützt
keine Bilder", on a model whose only purpose is images.

### 7.3 `PUT /chats/{id}` has no live-run guard, and `DELETE` has one

`portal_chat_endpoints.go:104-119` goes straight to `SaveChat`.
`:120-123` — the very next case in the same switch — calls
`s.ChatRuns.cancelChat(...)` under the comment "tear down an active run before
deleting". The save path never asks the registry. `flushSave` skips only
`if (isRunning(id)) return` (`useChatPersistence.ts:164`), and `runsRef` is
**per-tab** with no cross-tab signal, so a second already-open view never
learns a run started and can PUT its stale document over the transcript
mid-run. The exposure window is the run duration, which this feature
multiplies by roughly fifty, and what it clobbers is a just-committed image.

### 7.4 No chat error code is mapped

`errorLabelByCode` (`shared/format.ts`) contains no chat-related code at all —
checked across the whole file, not a prefix. The backend can return three:
`portal.chat_too_large` (`error_map.go:45`), `portal.chat_run_active` and
`portal.chat_run_limit` (`chat_run_endpoints.go:49-50`, with `maxPerUser`
defaulting to 5 — hardcoded in two places, `chat_runs.go:268-270` and
`cmd/gateway/main.go:1082`, and configurable in neither). A fourth is
reachable from the operator form this feature adds: `mapping.capability_reserved`
(`portal_mapping_endpoints.go:138`), returned when someone tries to write the
refused `(image, no)`. All four toast raw English today. The run-contention pair
becomes ordinary rather than exotic precisely because this feature's answer to
a multi-minute wait is "go work in another chat".

### 7.5 The download rescue cannot download an image

`shared/download.ts` exports only `downloadText(filename, content, mime)`,
which wraps a **string** in a Blob — its own doc comment says it was written
for PEM/text. Handing it a data URL saves a text file containing the data URL.
A binary path is needed, plus a real extension.

### 7.6 The generated image is misnamed and cropped

`ChatMessage.tsx:361-366` hardcodes `alt={t.chatAttachedImage}`
("Angehängtes Bild") and renders at `72×72` with `objectFit: 'cover'`.
Correct for an upload thumbnail, wrong for the artifact the user asked for: a
false accessible name and a cropped stamp. The right alt text is one message
above — the prompt.

### 7.7 Two real upstream signals go unused

The response body carries `output_format` at the top level and may carry a
per-item `revised_prompt`. The MIME prefix and the download extension must
come from `output_format`; hardcoding `image/png` would be a fabricated
measurement of the same class as the `0 Zeichen` counter. `revised_prompt` is
the only substantive news this endpoint ever reports about a generation.

### 7.8 `data[]` is plural by design

`imagesDataCounter`'s own doc comment (`images_handler.go:537-539`): "The
quantity comes from the RESPONSE, not the request: n states what was asked
for, data[] states what was produced, and a partial failure makes those
differ." So the backend already bills plural, a plural response is a real
case, and the UI must render and offer download per item and size the
capacity arithmetic on the sum.

### 7.9 The pagehide keepalive is dead for any image thread

`useChatPersistence.ts:227-229` refuses to send its unload-time save when
`JSON.stringify(payload).length > 60000`. A single inline base64 image is far
past that, so the keepalive silently returns and a last-moment change is lost
on navigate-away — in exactly the threads this feature creates. The size guard
exists for a real reason (a `sendBeacon`-class payload limit), so the answer is
not to raise it blindly.

### 7.10 A non-200 from the gateway's own loopback call loses its body

`executeRun` turns any non-200 into `errMsg = "upstream status " + resp.Status`
without reading `resp.Body` (`chat_runs.go:528-531`). Every error code the
images endpoint was built to return — `images.prompt_required`,
`images.stream_unsupported`, `images.response_format_unsupported`,
`images.upstream_error` — is therefore discarded before the user can see it,
and the chat reports a bare status line instead. The endpoint's careful error
vocabulary is only useful if the executor relays it.

### 7.11 `npm test` does not type-check

`npm test` is `vitest run`; only `npm run build` runs `tsc`
(`package.json:8-9`). A `ChatStore` field missing from
`ChatSidebar.test.tsx`'s `makeStore` — which enumerates all 47 fields and is
typed `ChatStore` with no cast — passes the test suite and fails the build. Any
task touching that type must run `npm run build`, not just `npm test`.

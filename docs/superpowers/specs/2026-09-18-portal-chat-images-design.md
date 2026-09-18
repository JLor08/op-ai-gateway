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

## 6. Open questions

What the composer shows while an unstreamed image is generating — see §3.2:
there are zero incremental events between Send and the finished image, and
generation on sd_cpp takes tens of seconds to minutes, so the existing pending
state is both unusable and misleading. The existing text pending state is a
live character counter (`ChatMessage.tsx:82-83`), which would read `0 Zeichen`
for the entire wait.

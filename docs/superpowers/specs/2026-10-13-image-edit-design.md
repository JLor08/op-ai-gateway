# Image edit in the portal chat (and as an OpenAI-compatible endpoint) — design

**Goal:** let a user edit an image in the portal chat — upload a source image (or
refine a just-generated one) with a prompt, optionally masked — served by
sd-server's `/v1/images/edits`, and expose the same capability as the
OpenAI-compatible `POST /v1/images/edits` endpoint for bearer API clients.

This is the image-**edit** part of #69, the larger half left after Phase 1a/1b
(image settings + per-model options, shipped in #214/#215/#216). It is the
gateway's **first multipart request path**.

**Decided with the user:** full scope ("alles mitnehmen") — img2img whole-image
edit, masked inpainting, iterative refine, the public bearer endpoint, the
multipart passthrough path, and an `image_edit` capability. Upstream surface is
**multipart passthrough** to sd-server's `/v1/images/edits` (not a JSON translate
to `/sdapi/v1/img2img`). The capability is **derived** (auto-on wherever
`image: yes`) and **operator-removable**.

> This spec was verified against the code by six adversarial readers; the
> mechanism details below reflect their corrections (notably: the two
> reserved-capability lists, the agent model-name rewrite, the
> single-sourced derivation, and the `capableNames` redirect). **It was then
> rebased after #124 (the `chat_blobs` store, PR #217) shipped — Part 6's
> refine-budget and capacity workarounds are now obsolete (see Part 6).** Exact
> line anchors are approximate and will be pinned in the implementation plan.

## Grounding (verified facts that shape the design)

1. **sd-server has no per-model edit signal.** It serves `/v1/images/edits`
   whenever the loaded model supports `img_gen` (`routes_openai.cpp`,
   `runtime.cpp`); `features_by_mode.img_gen` flags are constant-true. So "which
   models may edit" is a gateway **policy**, not a probe result.
2. **Two gateway-side reconciliations:** `strength` defaults to **0.75** (a weak
   edit) unless overridden via the `<sd_cpp_extra_args>{"strength":…}` prompt
   block Phase 1b-i already plumbs; and the **mask is luminance (white =
   regenerate)** while sd **drops alpha**, so an OpenAI alpha-transparent mask
   must be converted. Edit responses share the generations shape
   (`{created, output_format, data:[{b64_json}]}`); the edit route is png/jpeg.
3. **The portal needs no multipart on the browser hop.** The chat already sends
   attachments as data-URL `image_url` parts over JSON to
   `POST /api/portal/chats/{id}/runs`; the **server executor**
   (`chat_runs_images.go`) makes the inference call. The browser hands the source
   in as a data-URL `image_url` part; **post-#124 the server stores it as a
   `chat_blobs` payload and the transcript carries a `/chat-blobs/{id}` reference**,
   which the executor resolves back to bytes when it builds the **server-side**
   multipart body for the loopback call. Multipart exists only on the
   gateway→sd-server hop and the public bearer endpoint.
4. **A vision-attach affordance exists to mirror** (`ModelOption.vision` → a
   store boolean → a ref → the attach enable/tooltip → the `send()` guard → the
   clear-on-switch effect), but it keys on the **thread kind + picked model**,
   not the model alone — the new `image_edit` wiring must follow that same
   kind-aware shape.
5. **ADR separability.** The public bearer endpoint is governed by ADR-042 (gate)
   and ADR-044 (flavor). ADR-043 (the portal-turn "the model is the only
   affordance, no mode toggle") is reconciled **only** to put edit into the
   composer. The "browser never calls this endpoint" rule is an **auth**
   constraint (loopback-or-bearer, no cookie), which the edits route must keep;
   the portal reaches it over the loopback pair.

## Architecture overview

```
 PUBLIC (bearer API client)                     PORTAL (browser)
   multipart/form-data                            JSON run start; source as an
   POST /v1/images/edits                          image_url data: URL part
          │                                              │
          ▼                                              ▼
   gateway edits handler  ◄──────────────── server executor base64-decodes the
   (peek model from form; image_edit gate;   source + builds the multipart body,
   inferencePreflight; relay)                calls the gateway over loopback
          │
          ▼  proxyNative carrying the multipart Content-Type (+ boundary);
          │  model-field rewrite for agent targets; mask converted; strength set
   ┌──────┴───────────────────────────────┐
   external stable_diffusion_cpp app       agent-launched stable_diffusion_cpp
   (model ignored → verbatim relay ok)     child (router peeks `model` from the
          │                                  form → rebuild needed; feature-gated)
          ▼                                              ▼
   sd-server POST /v1/images/edits  ◄──────────────── sd-server (child)
```

One gateway handler serves both entry points.

## Part 1 — The `image_edit` capability (derived, operator-removable)

Upstream can't tell us which models edit, so the gateway owns the policy:

- **Effective `image_edit` = (`image: yes` on the mapping) AND NOT (an operator
  `image_edit: no` verdict row).** Derived at the gate from the existing `image`
  verdict, with one operator opt-out row. No probe produces it; "no row" means
  "derive from `image`," **not** "unknown" — a deliberate, documented exception
  to ADR-042's unknown-refuses rule.
- **Single-sourced derivation (ADR-045 rule — "one function in routing is what
  everyone reads").** Add one predicate in `internal/routing` (an
  implied-capability helper) and consume it from **all four** readers so the gate
  and the composer can never disagree in direction:
  1. `capabilityRowsSatisfy` (`resolver.go` ~:1318) — the routing gate; every
     satisfaction decision funnels through it (four callers, all pass the
     mapping's full row set), so one special case covers routing.
  2. `capableNames` (`portal/service_model_offering.go` ~:277-313) — **today it
     returns an empty set for any required capability other than `image`**, which
     makes the unknown-model redirect in `inferencePreflight` **dead on arrival
     for edits**. It must derive `image_edit` too, or an unknown edit model 404s
     instead of redirecting.
  3. The `ModelDTO`/`ModelOption` fold (`service.go` `imageFlagsByName`/
     `groupCapabilityFlags` ~:2687-2717, threaded through the per-name fold, the
     group fold, the override-alias copy, and the DTO assembly) — gains an
     `image_edit` boolean read from a **second** per-mapping row.
  4. The operator opt-out UI (below).
- **Gate vs fold asymmetry (correct invariant).** The gate is per-candidate
  any-of (`filterCapable`); the fold ANDs across a model's mappings. A model on
  server A (`image_edit:no`) and B (yes) is **served** by the gate but
  **advertised false** — the safe direction, the same asymmetry `image` has
  today. The invariant is "**the composer never offers what the gate will
  refuse**," not "they agree exactly."
- **Reserved lists (both were inverted in the first draft — corrected):**
  - `reservedManualVerdicts` (`service_applications.go` ~:278-314) is a **deny**
    list of pairs an operator may **not** state. The capability vocabulary is
    **open** (`ValidateCapabilityRow`, `normalizeCapabilityVerdicts` accept any
    non-empty name with yes/no/''), so `(image_edit, no)` is **already writable
    with no code change** — do **not** add it (adding it would forbid the opt-out
    the design needs). `(image, no)` stays reserved, which is exactly why a
    separate `image_edit:no` opt-out row is needed. Extend the list's doc comment
    to explain this.
  - `reservedAgentCapabilityNames` (`agent_ingest.go` ~:1149-1236) is a **deny**
    list of names an agent's open verdict list may **not** carry (its criterion:
    "names this codebase reasons about that no agent probe can observe"). Because
    `image_edit` has **no legitimate agent writer**, it **must be added** here —
    otherwise a buggy or hostile agent could send `{image_edit, no}` (rank 1) and
    silently disable edit, re-overwriting the operator's reset each sample. (This
    is the opposite of ADR-042 (f)'s rule for `image`, which is **kept off** the
    list precisely because a legitimate agent writer exists for it.)
- **Operator opt-out UI is new, not reuse.** `MappingForm.tsx`'s image control is
  a fixed yes/no/unknown set whose image select deliberately has **no "no"**
  option (because `(image,no)` is reserved); `ModelServersSection.tsx` only
  displays yes-chips. Add a **new** `MappingForm` control for `image_edit` where
  `unknown` = "derived from `image`", `no` = the opt-out, and `yes` is inert (the
  gate derives); and surface the opt-out state. This is a real UI addition.
- **Type-blindness (stated, accepted).** The derivation keys on the
  `openai_images` flavor + an `image:yes` verdict, so a non-sd upstream that
  declares `openai_images` with a manual `image:yes` (e.g. a custom/LiteLLM
  backend with no `/v1/images/edits`) also becomes edit-capable. This is
  consistent with the user's choice (auto-on, operator removes); the per-mapping
  `image_edit:no` row is the remedy. The new ADR records it.
- **Rejected alternative:** an auto-written `image_edit:yes` row (rank 1/2) with a
  manual `no` (rank 3) outranking it. Rejected to avoid a new producer in both the
  health loop and the agent twin; derive-at-gate needs neither (same spirit as
  ADR-055's "no new durable store for a derivable value").

## Part 2 — The multipart request path (gateway)

- **Route + endpoint.** Register `POST /v1/images/edits` (+ `/openai` alias). Add
  a `sessionEndpoint` value `endpointImagesEdit` (no-op for session extraction,
  like `endpointImages`). Auth is `requireInternalOrBearerAnyScope` (browser
  never calls it directly, per ADR-043); copy the run-as block as the images
  handler does by convention.
- **Body read + model-before-preflight.** `readRawJSONLimit` rejects multipart.
  The edits handler reads the raw body (bounded; Part 7) and parses it with
  `mime/multipart` and the request boundary — never `r.ParseMultipartForm` (temp
  spill + stream consumption). Peek the `model` **form field** (accepting both
  `image`/`image[]` conventions for the image parts) before `inferencePreflight`,
  which needs only the model string (its `raw`/`extractClientSession` is a no-op
  for the images endpoint; `admitPrincipal` keeps its single call site). Missing
  `model`/`prompt`/image → `400 images.*` before preflight (no admission spent).
- **The proxy carries the content type.** `provider.ProxyNative(ctx,target,path,
  body)` hard-codes POST + `application/json` (`provider/proxy.go`). Generalize it
  to carry the inbound Content-Type (and, where needed, a distinct upstream body)
  — e.g. a small request descriptor — and update **all six** production
  implementers (`multiplexer`, `openai_compatible`, `anthropic_messages`,
  `openai_responses`, `gemini`, `mock`) **and the ~19 test call sites across 9
  files**. JSON callers pass `application/json` unchanged. **`mock.ProxyNative`
  currently ignores the path and returns a canned Responses SSE stream** — it must
  grow a generations-shaped image response so an edits e2e against the mock
  provider works.
- **Model rewrite is required for agent targets (not verbatim).** The client/
  gateway sends `Target.Model = GatewayModelName`, but the agent router keys the
  child on `ProviderModel = AppModelName` (`resolver.go` ~:1895-1897) — **two
  distinct names.** `rewriteModelField` only rewrites JSON. So:
  - **External sd-server target:** model is ignored upstream → relay the client's
    multipart bytes **verbatim** (unless a mask forces a rebuild, Part 4).
  - **Agent-launched target:** if `GatewayModelName != AppModelName`, the gateway
    must **rebuild** the multipart body with the `model` field set to
    `AppModelName` (via `multipart.Writer`, emitting the new Content-Type through
    the generalized `ProxyNative`); verbatim only when the two names are equal.
  - The verbatim-vs-rebuilt split is therefore "**no model-rewrite needed AND no
    mask → verbatim; else rebuild**," pinned by tests for each axis.
- **Upstream path + usage.** `upstreamPath` returns a flavor-keyed constant; key
  it on the **endpoint** so it yields `/v1/images/edits`. **`recordUsage`
  recomputes `ProviderPath = upstreamPath(target, req.APIFlavor)`**
  (`inference_complete.go` ~:725) from the flavor, not `rel.path`, so the endpoint
  must be threaded into that computation too, or the usage row records the
  generations path for an edit.
- **Served-flavor conjunct.** Replicate the generations relay's ADR-044(b)
  second-stage `targetServesFlavor` check (`images_handler.go` ~:517) so an
  agent child whose spec lists only text cannot be picked to serve an edit
  (`404 routing.no_model...`).
- **Validation twin.** An edits `validateImagesRequest`: require `prompt`,
  `model`, ≥1 image part; reject `stream`; same `response_format` (b64_json)
  rule; new codes `images.image_required`, `images.mask_invalid`,
  `images.edit_unsupported_format`.

## Part 3 — The agent relay (server-agent)

External sd-server apps need no agent change. The one blocker is the router's
JSON-only model peek.

- **Router multipart branch.** `router.go` ~:580-584 `json.Unmarshal`s the body to
  peek `model`/`stream` and 404s `runtime.model_not_managed` otherwise. Add a
  Content-Type branch: on `multipart/form-data`, read the boundary and scan the
  already-buffered body for the `model` part (bounded). It must **always take
  `servePlainProxy`** (never `serveStreamProxy` — multipart has no stream notion).
  JSON stays the default so existing tests are byte-identical; malformed/missing
  `model` yields the same 404 (no child started). (Note: the router **buffers**
  the request under `maxBodyBytes` 32 MiB and streams only the response; the
  "verbatim" relay is byte-faithful, not streamed.)
- **Version + feature.** Bump `const Version` 0.8.2 → **0.9.0**. The rule
  (AGENTS.md) picks MINOR **because `agent.Features` gains a flag**
  (`multipart_proxy`), not because the request shape changed — add that flag so
  the gateway can tell which agents serve edits. Update the pin at
  `features_test.go` ~:285 (currently asserts `Version == "0.8.2"`).
- **The feature gate is a Phase-A artifact, not Phase-B.** The gateway must refuse
  an edit to a `ProviderServerAgent` target whose agent lacks `multipart_proxy`
  (an `AgentFeatures.Has(serverID, multipart_proxy)` pre-relay check), reporting
  "edit not available on this server" rather than sending multipart to an old
  router (a 404). This check ships in Phase A so there is no A→B window where an
  agent target silently breaks; Phase B makes agents advertise the flag.

## Part 4 — Upstream edit semantics (strength + mask)

- **Strength.** An advanced **edit** knob delivered through the existing
  `<sd_cpp_extra_args>{"strength":…}` prompt block (the Go extra-args struct in
  `chat_runs_images.go` gains `strength`; the frontend `ImageSettings`/
  `DEFAULT_IMAGE_SETTINGS`/`imageSetting`/`imageSettingsFrom` + `useChatPersistence`
  three-edit lockstep + i18n gain it). Blank = sd-server's default (0.75); a
  tooltip notes reference-edit models may want higher (upstream #1779).
- **Mask polarity + rebuild.** The gateway converts an OpenAI-style RGBA mask
  (alpha-transparent = edit) to an sd luminance mask (white = regenerate), so the
  public endpoint is genuinely OpenAI-compatible; the portal mask UI emits the
  same OpenAI-style mask through the same conversion. Converting a mask means the
  gateway **rebuilds the multipart body** (and the rebuild must preserve the
  `model` field for agent routing — ties to Part 2). The gateway enforces
  init/mask equal dimensions (`images.mask_invalid` otherwise). Note a converted
  mask can **inflate** bytes past the 32 MiB agent cap — Part 7.
- **Output size semantics.** The composer size dropdown defaults to '' (model
  default); for an edit, sd derives the output from the init-image unless `size`
  is set, and the mask must match the init dimensions. The spec's size/mask
  dimension rule must be stated in the handler and the docs.

## Part 5 — The portal composer (ADR-043 reconciliation R1)

An image attached to an image-model thread makes that turn an **edit**. No mode
toggle, no "edit" button.

- **Attach enablement is kind-aware.** Composer affordances render off the
  **thread kind** (`ChatStore.tsx` ~:138-158), while the attach ban/clear are
  keyed on picked-model flags (`Chat.tsx` ~:749, the ~:877-889 effect). Introduce
  **one predicate** — e.g. `attachMode = chatKind==='image' ? (modelImageEdit ?
  'edit' : 'none') : (modelVision && !modelImage ? 'vision' : 'none')` — and drive
  the attach button, the clear-on-switch, and the `send()` path from it. Do **not**
  AND `vision` (sd models are `vision:false`). The non-edit-capable image model
  keeps the current generator wording (`chatImageGeneratorNoInput`); the
  `chatImageOnlyHint` string needs an edit-capable variant.
- **Turn routing.** A turn whose last user message carries an `image_url` (source)
  part posts to `/v1/images/edits`; else `/v1/images/generations`. Thread `kind`
  stays `image`; URL = `f(kind, attachment)`.
- **Replay/edit guards.** `ChatStore.tsx` ~:1005-1012 exempts image threads from
  the history guard on the premise "the image request carries no history" — **false
  for an edit turn** (it carries the source). And `editUserMessage` (~:1057-1061)
  rebuilds content as `[text, ...image_url]`, which **drops** any new mask/reference
  part type. Both must be re-specified for edit turns.
- **Server executor.** `chat_runs_images.go` locates the source data-URL in the
  last user message, base64-decodes it, builds the multipart body (`image`,
  `prompt`, `n`, `size`, optional `mask`, the `<sd_cpp_extra_args>` prompt) and
  posts to `/v1/images/edits` over loopback. No `setRunLoopbackHeaders` variant is
  needed — the call site simply `req.Header.Set("Content-Type", mw.FormDataContentType())`
  after the shared helper (Content-Type is the only header it sets).
- **Deadline + commit.** The edit stays `kind:image`, so it is already covered by
  the image-run deadline (no new unbounded `kind`); one terminal commit via
  `finishRunWithParts`, no streaming executor reuse.

## Part 6 — Masks and iterative refine (simplified by #124)

> **#124 (the `chat_blobs` store) has since SHIPPED (PR #217, now on `main`).**
> Chat images (generated and uploaded) now live OUT of the transcript as
> `/api/portal/chat-blobs/{id}` references — bounded by a per-blob cap
> (`MaxChatBlobBytes`, 20 MiB) and a per-run history bound (`MaxRunHistoryBytes`),
> with a server-side resolution of a gateway url back to bytes for inference. That
> dissolves the refine-budget and capacity workarounds this section originally
> needed; the simplifications below replace them. The plan will build on #124.

- **Mask upload (v1).** A second composer slot tagged as a mask. A mask is stored
  the same way every chat image now is — a `chat_blobs` payload referenced by a
  gateway url — but it needs a mask **tag/convention** on the content part
  (distinct from the source image) that survives `editUserMessage` and replay. An
  in-browser mask-**drawing** canvas is deferred (frontend-only, same backend).
- **Iterative refine — just reference the stored image.** Post-#124 a generated
  image is ALREADY a `/api/portal/chat-blobs/{id}` reference in the transcript, so
  a "refine this image" action simply makes that existing reference the edit
  turn's source. **No copy, no re-embed, no reference-part-type to invent, no
  budget problem** — the earlier "re-embed blows the 4 MiB cap" and the
  reference-shaped-part design are moot. The server executor resolves the source
  chat-blob url to bytes (the same `chat_blobs` resolution #124 added for the
  inference history) when it builds the multipart `/v1/images/edits` call.
- **Uploaded sources — no special budget handling.** A user-uploaded source is
  stored as a `chat_blob` by #124's extract-on-run-commit (bounded by
  `MaxChatBlobBytes`), not inline in the transcript, so the old budget-driven
  "re-encode to a bounded size" is no longer needed. EXIF-orientation
  normalization is optional (a UX nicety, not a budget requirement) — decide in
  the plan. The executor resolves the uploaded source's chat-blob url to bytes
  for the edit call, the same as refine.
- **Capacity accounting — nothing to do.** #124 REMOVED the image-count capacity
  estimate (`imageCostBytes`/`imagesLeft`/`chatCapacity.ts` are gone); images no
  longer count against the transcript budget. The role-awareness workaround this
  section originally specified is obsolete.

## Part 7 — Cross-cutting: size, capture, billing, response

- **Size.** Edge nginx is uncapped for `/v1/`. Apply one configurable gateway cap
  for the multipart read (default coordinated with the agent's 32 MiB), and
  account for the fact that a mask-rebuild can grow the body past the client's
  original — the gateway cap and the agent cap must leave headroom or the rebuild
  must be checked. Amend ADR-009 to note this one cap and why (memory exposure).
- **Capture.** `buildCaptureEnvelope` stores `string(raw)` clipped to
  `captureMaxBytes`; `json.Marshal` of raw binary corrupts bytes to U+FFFD. Branch
  it on Content-Type: for multipart, store the text fields + part metadata only,
  redacting the binary image/mask parts.
- **Billing.** `setImagesBillingQuantity` counts produced **output** images only;
  input source/mask images are **unmetered** (stated decision; a 2-input edit bills
  like a 0-input generation).
- **Response.** Generations-shaped, so the b64_json counter, error normalization,
  and billing quantity carry over unchanged.
- **Vendor accounts (ADR-050).** Edits inherit generations' vendor-account
  handling via the shared preflight→resolve path; the vendor branch is skipped for
  any non-empty `RequiredCapabilities` (so `image_edit` doesn't perturb it) — state
  it, no change expected.

## Part 8 — ADRs and docs

- **New ADR** (next free number at implementation time): edit is served by image
  models via a **derived, operator-removable `image_edit`** capability; the portal
  makes an **attached image the edit affordance** (R1); the request is **multipart
  passthrough** to `/v1/images/edits` (vs the rejected JSON `/sdapi/v1/img2img`
  translate and the auto-written-row alternative); plus the strength + mask
  reconciliations and the agent `multipart_proxy` feature gate. Re-argue the
  ADR-043 rejected-toggle honestly (the attachment is the user's **operand**, not
  a mode; the control is one model-derived flag).
- **Amend, with the repo's precedent form:**
  - ADR-043 uses **inline parenthetical** `(Amended by ADR-NNN …)` markers (no
    block precedent for 043). Mark the superseded sentences precisely: (a) :1776
    "the picked model is the ONLY affordance" and :1779-1783 "one axis"; the
    Consequence :1797-1810; (b) :1822-1823 "the same value selects the request
    URL"; and touch (c) :1885 and (d) :1891 (the capacity/exactness and
    history-feedback premises an edit turn changes).
  - ADR-042: the derived exception changes more than "unknown refuses" (:1591-1600)
    — also the Decision sentence (:1571-1573). Use the existing block form.
  - ADR-009: the one multipart body cap.
- **Docs:** `compatibility-and-inference.md` (route table; a new §3.x edits
  section; §10 reader table — this route is not JSON-only; §11 vision-vs-image
  axes gains `image_edit`; §12; fix the stale "every accepted image is downscaled"
  sentence), `api-surface.md` (the route + the `image_edit` capability column),
  `openapi.yaml` (a `multipart/form-data` requestBody + the edits path),
  `routing-and-model-selection.md` (the derived capability + the multipart gate it
  already earmarks for #68/#69), `telemetry-usage-observability.md`
  (`multipart_proxy`), `agent-runtime-manager.md` (the router multipart contract +
  the feature/version history), `02-constraints.md`/`03`/`04`/`12-glossary.md`
  (the loopback-invariant + required-capability-row wording),
  `components/shared/format.ts` `errorLabelByCode` + i18n (de+en) for the new
  `images.*` run-error codes. `make lint-docs` must pass.

## Review focus (failure modes the tasks' tests must pin)

- A **non-edit-capable** image model (operator `image_edit:no`): no attach
  control, the gate 404s an edit, generation still works.
- A **hostile/buggy agent** sending `image_edit:no`: dropped at ingest (the name
  is in `reservedAgentCapabilityNames`), the operator's state untouched.
- An **unknown edit model**: the preflight redirect works (`capableNames` derives
  `image_edit`), not a bare 404.
- A model the token **can't route to**: the edit endpoint leaks nothing.
- **Agent model-name rewrite**: an agent target with `GatewayModelName !=
  AppModelName` gets the form `model` rewritten to `AppModelName`; the child is
  found. External sd-server with no mask is relayed **verbatim**.
- A **multipart body with no `model`** / malformed boundary: same 400/404, no
  child started.
- An **old agent** (no `multipart_proxy`): edit reported unavailable, not a 500,
  and not multipart-to-an-old-router.
- A **text-only agent spec**: the served-flavor conjunct refuses the edit.
- A **mask**: converted alpha→luminance, dimensions enforced; a **no-mask** edit
  to an external server is relayed verbatim (not needlessly rebuilt).
- **Refine/source via #124 reference**: a generated-image **refine** uses the
  existing `/chat-blobs/{id}` reference as the source (no copy/re-embed), and the
  executor resolves it (and an uploaded source's chat-blob) to bytes for the edit
  call. (No capacity-estimate test — #124 removed that estimate.)
- **Capture** of a binary multipart request stores no corrupted/truncated binary.
- **Usage** records the `/v1/images/edits` path, not generations.
- Every **JSON endpoint** is byte-for-byte unchanged after the `ProxyNative`
  signature change (the 6 impls + ~19 test sites).

## Delivery phasing (one coherent feature, layered; the plan details it)

- **A — Backend multipart foundation + public endpoint + capability + feature
  gate:** `ProxyNative` content type (6 impls + mock image response + test sites),
  `upstreamPath`/`recordUsage` endpoint-aware, multipart model peek + preflight +
  served-flavor conjunct, the `/v1/images/edits` route/handler, the derived
  `image_edit` (routing predicate + `capabilityRowsSatisfy` + `capableNames` +
  DTO fold + the two reserved-list changes + the MappingForm opt-out), and the
  `multipart_proxy` pre-relay gate for agent targets. Delivers the
  OpenAI-compatible endpoint for external sd-server apps, API-testable.
- **B — Agent relay:** router multipart branch + Version 0.9.0/`multipart_proxy`
  feature + `features_test` update, so agent-launched sd children serve edits.
- **C — Upstream reconciliations:** strength knob (Go + frontend + persistence +
  i18n) + gateway mask-polarity conversion/dimension check + the mask-rebuild path.
- **D — Portal img2img:** the `attachMode` predicate through the composer, the
  `image_edit` flag fold, the server-side multipart built from the source
  **chat-blob reference resolved to bytes** (#124), the ADR-043 amendment. (No
  capacity work — #124 removed the estimate.)
- **E — Masks in the portal:** the mask slot + its tag/convention
  (editUserMessage/replay) + the mask chat-blob end to end.
- **F — Iterative refine:** a "refine this image" action that makes the existing
  `/chat-blobs/{id}` reference the edit source + the executor resolving it to
  bytes + edit/replay handling. (Much smaller post-#124 — no new part type.)
- Docs + ADRs land with the parts they describe.

## Out of scope / deferred

- The JSON `/sdapi/v1/img2img` translate and the async `/sdcpp/v1/img_gen`
  surface (passthrough chosen).
- An in-browser mask-**drawing** canvas (upload-only in v1, same backend).
- A durable store for anything new (the capability reuses the verdict table + one
  opt-out row).
- A per-model **probe** for edit capability (impossible upstream).
- Metering input images.

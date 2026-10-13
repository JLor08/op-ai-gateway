# Image edit in the portal chat (#69) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a user edit an image in the portal chat (upload a source or refine a
stored one, with a prompt, optionally masked), served by sd-server's
`/v1/images/edits`, and expose the same `POST /v1/images/edits` as an
OpenAI-compatible endpoint for bearer API clients.

**Architecture:** The gateway's **first multipart request path**. One
`/v1/images/edits` handler serves both the public bearer endpoint and the portal
(reached server-side over loopback). The `ProxyNative` seam grows a content type
so a multipart body reaches upstream; the model is peeked from the form before
preflight; an agent-launched target gets the form `model` rewritten to its app
name. Edit routes to a model via a **derived, operator-removable `image_edit`**
capability (auto-on wherever `image:yes`). The composer makes an attached image
the edit affordance (ADR-043 R1). Sources are `chat_blobs` references (#124
shipped); the executor resolves them to bytes for the multipart call.

**Tech Stack:** Go (gateway/backend: `internal/{gateway,provider,routing,portal}`;
server-agent `internal/runtime`), React/TS (gateway/frontend). Builds on #124
(`chat_blobs`), Phase 1a/1b (#214/#215/#216).

**Spec:** `docs/superpowers/specs/2026-10-13-image-edit-design.md` (6-reader
verified; rebased after #124).

## Global Constraints

- **Branching:** feature branch + PR only; never commit to `main`; repo-facing
  text (commits/PR/issues) **English**; **no attribution/trailer lines**; push
  over **SSH**.
- **ServerAgent Version Rule (AGENTS.md):** the agent change (multipart proxy)
  adds a new `agent.Features` entry `multipart_proxy` → bump `const Version`
  0.8.2 → **0.9.0** (MINOR, because Features gains a flag — not because the shape
  changed); update the `features_test.go` version assertion + add the history
  paragraph.
- **Capability gate invariants (ADR-042/043/044/045):** `RequiredCapabilities` is
  set from the **handler identity**, never the body; `unknown` refuses (fail-
  closed) — the `image_edit` derivation is a *documented exception* (no row ⇒
  derive from `image`); the derivation lives in **one** routing function that the
  gate, the `capableNames` redirect, and the DTO fold all read (ADR-045 "one
  function both read"); keep the strict verdict decode separate from the lenient
  options decode.
- **Reserved lists (both verified in the spec — do not invert):**
  `reservedManualVerdicts` gets **no change** (the vocabulary is open, so
  `(image_edit,no)` is already operator-writable); `reservedAgentCapabilityNames`
  **gains `image_edit`** (no legitimate agent writer; keeps a hostile agent from
  asserting it).
- **Store/ADR:** reuse the existing verdict table + one operator opt-out row (no
  new durable table); `#124` `chat_blobs` is the source store (do not re-inline
  images).
- **Codegen:** `portal.API` is interfacer+gowrap-generated; if an interface it
  wraps changes, run `go generate` and **restore the SPDX header** of
  `api_tracing_gen.go` by hand.
- **CI gates:** `golangci-lint fmt`+`run` per Go module (gofumpt single sorted
  import group, gocritic); frontend `format:check`+`tsc`+`vitest`+`vite build`;
  `make lint-docs`; the frontend arch boundary (`Chat.tsx` imports `chat/`
  constants only via `ChatStore`, pinned by `arch.test.ts`); provision the
  Postgres DSN for any store-touching test.
- **No-translate ethos:** the gateway→sd-server hop is **multipart passthrough**
  to `/v1/images/edits` (not a JSON translate); the gateway owns only the
  `strength` default and the mask alpha→luminance polarity conversion.

## Review Focus

- A model the token **can't route to**, or a **non-edit-capable** image model
  (operator `image_edit:no`): the edit endpoint leaks nothing / 404s the edit,
  generation still works (Task A4/A3).
- A **hostile agent** sending `image_edit:no`: dropped at ingest (it is in
  `reservedAgentCapabilityNames`) (Task A4).
- An **unknown edit model**: the preflight redirect works — `capableNames`
  derives `image_edit`, not a bare 404 (Task A4).
- **Agent model-name rewrite:** a `server_agent` target with `GatewayModelName !=
  AppModelName` gets the form `model` rewritten to `AppModelName`; an external
  sd-server with no mask is relayed **verbatim** (Task A3/C).
- An **old agent** (no `multipart_proxy`): the edit is reported unavailable, not a
  500 and not multipart-to-an-old-router (Task A5); a **text-only agent spec**:
  the served-flavor conjunct refuses the edit (Task A3).
- A **mask**: alpha→luminance converted, dimensions enforced; a **no-mask**
  external edit is relayed verbatim, not needlessly rebuilt (Task C).
- **Capture** of a binary multipart request stores no corrupted/truncated binary;
  **usage** records the `/v1/images/edits` path, not generations (Task A2/A3).
- Every **JSON endpoint** is byte-for-byte unchanged after the `ProxyNative`
  signature change (Task A1).
- **Refine/source via #124:** refine uses the existing `/chat-blobs/{id}` as the
  source and the executor resolves it (and an uploaded source's chat-blob) to
  bytes (Task D/F).

---

## File Structure

- `internal/provider/proxy.go` + the 6 `ProxyNative` implementers (`multiplexer.go`,
  `openai_compatible.go`, `anthropic_messages.go`, `openai_responses.go`,
  `gemini.go`, `mock.go`) + ~19 test call sites — the content-type seam (A1).
- `internal/gateway/native_passthrough.go` (`upstreamPath`, model rewrite),
  `inference_complete.go` (`recordUsage` ProviderPath) — endpoint-awareness (A2).
- `internal/gateway/images_edit_handler.go` (new), `server.go` (route),
  `session_extract.go` (`endpointImagesEdit`), `error_map.go` (codes) — the
  handler (A3).
- `internal/routing/resolver.go` (`capabilityRowsSatisfy` + an `impliedImageEdit`
  helper), `internal/portal/service_model_offering.go` (`capableNames`),
  `internal/portal/service.go` (`ModelDTO` fold), `internal/gateway/agent_ingest.go`
  (`reservedAgentCapabilityNames`), frontend `MappingForm.tsx`/`ModelServersSection.tsx`
  — the capability (A4).
- `internal/gateway/agent_features.go` (the pre-relay gate), `server-agent/internal/runtime/router.go`,
  `server-agent/internal/agent/agent.go` (Version + `multipart_proxy`) — the agent (A5/B).
- `internal/gateway/chat_runs_images.go` (strength extra-arg; the edit executor),
  mask conversion helper — reconciliations (C).
- frontend `components/Chat.tsx`, `chat/ChatStore.tsx`, `chat/chatDoc.ts`,
  `ImageTurn.tsx`, `i18n.ts` — composer/masks/refine (D/E/F).
- `docs/architecture/**` + `openapi.yaml` + a new ADR — docs.

---

## Task A1: `ProxyNative` carries a content type

**Files:** Modify `internal/provider/proxy.go` (the `NativeProxyClient` interface +
`doNativeProxy`), the 6 implementers, and the ~19 `.ProxyNative(` test call sites.

**Interfaces:**
- Produces: `ProxyNative(ctx, target, path string, body []byte, contentType string)`
  (or a small `nativeRequest{path, body, contentType}` descriptor — pick one and
  use it everywhere). `doNativeProxy` sets `Content-Type: contentType` instead of
  the hard-coded `application/json`, and keeps POST.

- [ ] **Step 1 — failing test:** a provider test calls `ProxyNative` with
  `contentType = "multipart/form-data; boundary=X"` and asserts the upstream
  request carries exactly that Content-Type (not `application/json`); an existing
  JSON caller test still asserts `application/json`.
- [ ] **Step 2** run/FAIL (signature mismatch).
- [ ] **Step 3** — change the interface + `doNativeProxy`; update all 6 production
  implementers to thread `contentType` (JSON callers pass `"application/json"`);
  update the ~19 test call sites; **`mock.ProxyNative`** must, for an images/edits
  path, return a generations-shaped `{data:[{b64_json}]}` body (today it returns a
  Responses SSE and would count 0 images).
- [ ] **Step 4** run/PASS; `golangci-lint fmt`+`run`; `go test ./internal/...`.
- [ ] **Step 5** commit `refactor(provider): ProxyNative carries a content type`.

## Task A2: `upstreamPath` + `recordUsage` are endpoint-aware

**Files:** Modify `internal/gateway/native_passthrough.go` (`upstreamPath`),
`inference_complete.go` (`recordUsage`).

**Interfaces:** Consumes the inference `endpoint` (a new `endpointImagesEdit`,
Task A3 adds the constant). Produces `/v1/images/edits` as the upstream path for an
edit, and the same path in the recorded usage row.

- [ ] **Step 1 — failing test:** `upstreamPath` for an edit endpoint returns
  `/v1/images/edits`; `recordUsage` for an edit records `ProviderPath ==
  /v1/images/edits` (today `recordUsage` recomputes `upstreamPath(target,
  req.APIFlavor)` from the flavor — thread the endpoint so it does not record the
  generations path).
- [ ] **Step 2** run/FAIL. **Step 3** key `upstreamPath` on the endpoint (carried
  in `nativeRelay.endpoint`), and pass the endpoint into `recordUsage`'s
  ProviderPath computation. **Step 4** run/PASS + gates. **Step 5** commit
  `feat(gateway): edits upstream path + usage recording`.

## Task A3: the `/v1/images/edits` handler (multipart, model-before-preflight)

**Files:** Create `internal/gateway/images_edit_handler.go`; modify `server.go`
(route `/v1/images/edits` + `/openai` alias), `session_extract.go`
(`endpointImagesEdit`, a no-op extraction case), `error_map.go` (`images.image_required`,
`images.mask_invalid`, `images.edit_unsupported_format`).

**Interfaces:**
- Consumes: `requireInternalOrBearerAnyScope(scopeGatewayUse, scopeLLMInvoke)`,
  `inferencePreflight` (needs only the model string), `relayImages`' flavor-stage
  `targetServesFlavor` conjunct.
- Produces: `handleOpenAIImagesEdit`. Sets `RequiredCapabilities =
  []string{routing.CapabilityImageEdit}` from handler identity.

- [ ] **Step 1 — failing tests:** POST multipart with `model`,`prompt`,`image`
  → relays to `/v1/images/edits` and returns the b64_json body; a missing `model`
  part → 400 `images.model_required` **before** preflight (no admission spent); no
  image part → 400 `images.image_required`; `stream` present → refused; a non-GET/
  non-POST → 405; a text-only agent spec target → `404 routing.no_model...`
  (served-flavor conjunct).
- [ ] **Step 2** run/FAIL. **Step 3:**
  - read the raw body (bounded; Task C/size), parse with `mime/multipart` using the
    request boundary (NOT `r.ParseMultipartForm`); peek the `model` form field
    (accept `image` and `image[]`) **before** `inferencePreflight`;
  - a validation twin of `validateImagesRequest` (require prompt/model/≥1 image;
    b64_json-only; reject stream);
  - copy the run-as block + auth exactly as `handleOpenAIImages`;
  - replicate the `targetServesFlavor` conjunct;
  - relay via `proxyNative` with the multipart Content-Type (Task A1); **model
    rewrite:** for a `ProviderServerAgent` target whose `GatewayModelName !=
    AppModelName`, rebuild the multipart body with the `model` field set to
    `AppModelName` (via `multipart.Writer`); an external sd-server target with no
    mask is relayed verbatim.
- [ ] **Step 4** run/PASS + `-race` + gates. **Step 5** commit
  `feat(gateway): POST /v1/images/edits handler`.

## Task A4: the derived `image_edit` capability (single-sourced)

**Files:** Modify `internal/routing/resolver.go` (`capabilityRowsSatisfy` +
`impliedImageEdit` helper + `CapabilityImageEdit` const), `internal/portal/service_model_offering.go`
(`capableNames`), `internal/portal/service.go` (`ModelDTO`/`imageFlagsByName`/
`groupCapabilityFlags` fold), `internal/gateway/agent_ingest.go`
(`reservedAgentCapabilityNames` gains `image_edit`); frontend `api/models.ts`
(`ModelOption.image_edit`), `MappingForm.tsx` (new opt-out control),
`ModelServersSection.tsx` (display).

**Interfaces:** Produces `routing.CapabilityImageEdit = "image_edit"` and one
derivation predicate `impliedImageEdit(rows) bool = has(image:yes) && !has(image_edit:no)`,
consumed by the gate, `capableNames`, and the DTO fold.

- [ ] **Step 1 — failing tests:** a mapping with `image:yes` and no `image_edit`
  row satisfies `image_edit` (derived); adding a manual `image_edit:no` makes it
  NOT satisfy (operator opt-out); `capableNames` for required `[image_edit]`
  returns the derived set (so the unknown-model redirect works, not empty); the
  `ModelDTO.image_edit` boolean is the AND-fold across a model's mappings; a
  `{image_edit,no}` row from an agent is DROPPED at ingest.
- [ ] **Step 2** run/FAIL. **Step 3:** add the const + `impliedImageEdit`; special-
  case it in `capabilityRowsSatisfy` and `capableNames` and the DTO fold (same
  predicate, so the composer never offers what the gate refuses); add `image_edit`
  to `reservedAgentCapabilityNames` (make **no** change to `reservedManualVerdicts`
  — `(image_edit,no)` is already writable); add the `ModelOption.image_edit` TS
  field; add a `MappingForm` control (options unknown/no; `yes` inert) + show the
  opt-out. **Step 4** run/PASS + gates (incl. `go generate` if the ModelDTO change
  touches a generated surface — restore SPDX). **Step 5** commit
  `feat(routing): derived operator-removable image_edit capability`.

## Task A5: the `multipart_proxy` pre-relay gate (agent targets)

**Files:** Modify `internal/gateway/agent_features.go` (or wherever `AgentFeatures.Has`
lives) + the edit handler's relay step.

- [ ] **Step 1 — failing test:** an edit to a `ProviderServerAgent` target whose
  agent lacks `multipart_proxy` → the edit is reported **unavailable** (a clear
  code), NOT sent as multipart to the router (which would 404). An agent WITH the
  flag relays normally. **Step 2** run/FAIL. **Step 3** add
  `AgentFeatures.Has(serverID, "multipart_proxy")` before the agent-target relay;
  reuse the "edit not available on this server" path. This ships in Phase A so
  there is no A→B window. **Step 4** run/PASS. **Step 5** commit
  `feat(gateway): gate edits to agents advertising multipart_proxy`.

## Task B: the agent multipart relay + Version bump

**Files:** Modify `server-agent/internal/runtime/router.go` (multipart model peek),
`server-agent/internal/agent/agent.go` (`const Version` + `multipart_proxy` feature),
`server-agent/internal/agent/features_test.go`.

- [ ] **Step 1 — failing tests:** the router, on a `multipart/form-data` body,
  peeks the `model` from the form part and routes to the child (today
  `json.Unmarshal` at ~`router.go:580-584` 404s a non-JSON body); it ALWAYS takes
  `servePlainProxy` (never stream); a malformed boundary / missing `model` part →
  the same `404 runtime.model_not_managed` (no child started); `agent.Features`
  contains `multipart_proxy`; `const Version == "0.9.0"`.
- [ ] **Step 2** run/FAIL. **Step 3** add the Content-Type branch (bounded scan of
  the already-buffered body with `multipart.NewReader`), the feature flag, the
  version bump + the history paragraph + the `features_test.go` assertion. **Step 4**
  run/PASS (+ cross-module build). **Step 5** commit
  `feat(agent): serve multipart image edits (multipart_proxy, v0.9.0)`.

## Task C: upstream reconciliations — strength + mask

**Files:** Modify `internal/gateway/chat_runs_images.go` (strength extra-arg; the
edit-request build), a mask-conversion helper; frontend `chatDoc.ts`
(`ImageSettings.strength`), `useChatPersistence.ts`, `i18n.ts`.

- [ ] **Step 1 — failing tests:** a `strength` advanced knob rides the
  `<sd_cpp_extra_args>{"strength":…}` block (blank = sd-server default 0.75); an
  OpenAI alpha-transparent mask is converted to an sd luminance mask (transparent→
  white); init/mask dimension mismatch → `images.mask_invalid`; a **no-mask**
  external edit is relayed verbatim (not rebuilt), a **mask** edit rebuilds the
  multipart body (preserving the `model` field for agent routing).
- [ ] **Step 2** run/FAIL. **Step 3** add the strength knob (Go struct + frontend
  persistence lockstep + i18n); the gateway mask conversion before the relay; the
  "rebuild only when mask present (or model-rewrite needed)" split. **Step 4**
  run/PASS + gates. **Step 5** commit `feat(gateway): edit strength knob + mask polarity`.

## Task D: the portal composer (ADR-043 R1) — img2img

**Files:** Modify `components/Chat.tsx`, `chat/ChatStore.tsx`,
`internal/gateway/chat_runs_images.go` (the edit executor), `chat_runs.go`
(loopback Content-Type), `docs/architecture/09-architecture-decisions.md` (amend
ADR-043 (a):1776-1783, the Consequence :1797-1810, (b):1822-1823, touch (c):1885/(d):1891,
with inline `(Amended by ADR-NNN …)` markers).

- [ ] **Step 1 — failing tests:** an `attachMode` predicate (`chatKind==='image' ?
  (modelImageEdit ? 'edit' : 'none') : (modelVision && !modelImage ? 'vision' :
  'none')`) drives the attach control + clear-on-switch + `send()`; a turn whose
  last user message carries a source `image_url` posts to `/v1/images/edits`, else
  generations; the thread `kind` stays `image`; the replay guard
  (`ChatStore.tsx:1005-1012` image-thread exemption) is re-specified for an edit
  turn (it DOES carry a source); `editUserMessage` keeps the source part.
- [ ] **Step 2** run/FAIL. **Step 3** the predicate + wiring; the executor locates
  the source (a `/chat-blobs/{id}` ref post-#124), resolves it to bytes (the #124
  resolution), and builds the multipart `/v1/images/edits` loopback call
  (`req.Header.Set("Content-Type", mw.FormDataContentType())` after the shared
  helper); the ADR-043 amendment. **Step 4** `vitest`/`tsc`/`format:check`/`build`
  + Go gates + `lint-docs`. **Step 5** commit `feat(portal): attach-an-image-edits R1 img2img`.

## Task E: masks in the portal

**Files:** Modify `chat/chatDoc.ts` (a mask tag/convention on the content part),
`ChatStore.tsx`/`Chat.tsx` (a mask upload slot), `ImageTurn.tsx`/replay handling,
`i18n.ts`.

- [ ] **Step 1 — failing tests:** a mask upload slot stores the mask as a tagged
  `chat_blob`-referenced part distinct from the source; `editUserMessage` and
  replay preserve the mask part; the mask reaches the edit executor and the
  multipart call. **Step 2** run/FAIL. **Step 3** the mask tag/convention + the
  slot + the end-to-end plumbing (drawing canvas deferred). **Step 4** gates.
  **Step 5** commit `feat(portal): masked inpainting upload`.

## Task F: iterative refine

**Files:** Modify `ChatStore.tsx` (a per-message "edit this image" action via the
`handlersFor` cache), the edit executor (resolve the referenced chat-blob).

- [ ] **Step 1 — failing tests:** a "refine this image" action on a generated
  image starts an edit turn whose source is that image's existing
  `/chat-blobs/{id}` reference (no copy, no re-embed); the executor resolves it to
  bytes; edit/regenerate/replay of the refine turn behaves. **Step 2** run/FAIL.
  **Step 3** the per-message action + the executor reference-resolution (reuses
  Task D's resolution path). **Step 4** `vitest`/gates. **Step 5** commit
  `feat(portal): refine a generated image in place`.

## Task G: ADRs + docs

**Files:** `docs/architecture/09-architecture-decisions.md` (a new ADR for the
derived capability + R1 + multipart passthrough + strength/mask + the agent
feature gate; amend ADR-042 for the derived exception, ADR-043 per Task D, ADR-009
for the multipart body cap), `cross-cutting/compatibility-and-inference.md`,
`reference/api-surface.md` (+ the `image_edit` column), `reference/openapi.yaml`
(a `multipart/form-data` requestBody + the edits path),
`routing-and-model-selection.md`, `telemetry-usage-observability.md`
(`multipart_proxy`), `agent-runtime-manager.md`, `components/shared/format.ts` +
i18n (de+en) for the new `images.*` codes.

- [ ] **Step 1** write the ADR + amendments + the route + the docs. **Step 2**
  `make lint-docs` (checks 1-5 pass). **Step 3** commit `docs: image edit endpoint, capability, and ADR`.

---

## Self-Review

- **Spec coverage:** multipart seam (A1), endpoint-awareness (A2), handler (A3),
  capability (A4), agent gate+relay (A5/B), reconciliations (C), composer (D),
  masks (E), refine (F), docs (G) — every spec Part maps to a task. ✓
- **Type consistency:** the `ProxyNative` content-type signature (A1) is the one
  seam A3/C relay through; `routing.CapabilityImageEdit` + `impliedImageEdit` (A4)
  are the single derivation the gate/`capableNames`/DTO read; `multipart_proxy`
  (A5/B) is one feature string; `endpointImagesEdit` (A2/A3) is one constant. ✓
- **Review Focus:** leak/non-edit-capable (A3/A4), hostile agent (A4),
  unknown-model redirect (A4), model rewrite (A3), old agent / text-only spec
  (A5/A3), mask (C), capture/usage (A2/A3), JSON-unchanged (A1), refine/source
  (D/F) — each owned by a task with a test. ✓
- **Placeholder scan:** signatures, the novel logic (model-peek, model-rewrite, the
  derivation, the mask conversion, the attachMode predicate), and test cases are
  concrete; the 6 `ProxyNative` impls + the ADR clause anchors cite their place. ✓
- **Ordering:** A1→A2→A3→A4→A5 (backend+public endpoint) → B (agent) → C
  (reconciliations) → D (composer) → E (masks) → F (refine) → G (docs). A5 ships
  the agent feature-gate so there is no A→B gap.

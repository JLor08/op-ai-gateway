# `POST /v1/images/generations`: the capability gate, error normalisation, and the sd_cpp launch shape

Design for issue #71. Branch `images-generations`, cut from `main` @ `3dcf494`
(the merge of #70).

## 1. Goal and scope

Serve `POST /v1/images/generations` against leejet/stable-diffusion.cpp's
`sd-server`, and with it build the one piece of machinery every further non-chat
endpoint needs: **a candidate filter that actually excludes a model from
routing**. Nothing in this gateway refuses a model for lacking a capability
today — the scorer ranks, `wantsLiveProgress` annotates, the models-list fold
advertises. None of them says no.

In scope: the endpoint, the gate, normalisation of `sd-server`'s error shape,
the `usage_events` billable unit (on #70's rails), the launch shape for an
`sd-server` process, and the docs.

**Out of scope, decided:** the automatic capability writer (§9), a per-application
`images_mode` toggle, a new `sd_cpp` runtime kind (§7), deterministic image
stickiness (§6.4), and making `/v1/models` stop advertising an images-only model
to chat clients (§10).

## 2. The decision that shapes everything: gate on the capability, not the flavor

The issue proposes a new fine API flavor `openai_images` plus a per-capability
column in the candidate join. **That is the more expensive of the two available
mechanisms, and it does not do the refusing.**

`applicationServesEndpoint`'s default branch is
`applicationHasAPIFlavor(app, NormalizeAPIFlavor(fineFlavor))`
(`internal/routing/store.go:2095`), and `NormalizeAPIFlavor` folds any `openai*`
flavor to the coarse `openai` (`resolver.go:675`). So a fine flavor
`openai_images` passes for **every** OpenAI-flavored application in the fleet and
refuses nothing. Every byte of actual refusal would come from the new capability
column beside it. The flavor buys only the **trigger** — and it buys it for a
second filtered `LEFT JOIN`, a select column, a bind arg, a `sql.NullString` scan
target, a `MappingCandidate` field, the `MemoryStore` mirror, a verdict-conversion
twin, and assertions in two cross-driver conformance suites. Then again for #68,
and again for #69.

**The capability axis buys the same trigger for one additive struct field.**

- Carrier: `RequiredCapabilities []string` on `inference.Request`, beside
  `APIFlavor` (`internal/inference/types.go:175`). `Resolve` already takes the
  whole `req` (`resolver.go:419`) and there is exactly one call site
  (`internal/gateway/inference_resolve.go:51`), so no signature changes.
  `nil` for chat, responses and messages.
- The store read the filter needs **already exists, everywhere**:
  `MappingCapabilitiesForMappings` is declared on `routing.Store`, implemented
  chunked in SQLite, implemented in `MemoryStore`, present in the generated
  tracing decorator (`internal/tracing/routingstore_gen.go:475`), and already
  carries an N+1 guard test in the portal
  (`internal/portal/service_model_servers_test.go:518`). **No store work at all.**
- #68 and #69 then cost one constant and one handler line each.

The capability name is the string **`image`**, not `image_generation`. That is
what `cmd/gateway/app_health.go` already writes verbatim as a yes-row for every
`Extra` name, what migration 78 backfilled, and what the agent's own probe
already pins to image *generation* rather than vision. It also equals
`usage.BillingUnitImage` (`internal/usage/billing.go:23`), so #70's billable unit
and the capability speak one vocabulary.

### 2.1 Three claims in the issue that are wrong against the current tree

- "a **third** filtered LEFT JOIN … already carrying **two**": there is exactly
  **one** filtered `LEFT JOIN` today (`model_mapping_capabilities` on
  `live_progress`, `internal/store/sqlite_applications.go:559`). Under this
  design there is no new join at all.
- "the affinity path sees **none** of the candidate filters":
  `affinityApplicationStale` (`resolver.go:692`) already calls
  `applicationServesEndpoint(app, fineFlavor)` at `:696`. What the affinity path
  genuinely lacks is join-sourced *capability* data on its synthetic candidate —
  which is why the live-progress precedent needed its own keyed read.
- "only `applicationServesEndpoint`'s fine switch and the new filter get a case":
  its default branch already performs exactly the flavor-membership check a
  dedicated case would, because this design keeps the issue's own decision
  against a per-application `images_mode`. It needs **no** change.

Line numbers in the issue have drifted (`filterServesEndpoint` is at `:662`, not
`:631`; `applicationServesEndpoint` at `:2082`, not `:1973`). Not from #70 —
those two files were not in its diff.

## 3. The gate

One filter, `(*Resolver).filterCapable(ctx, cands, required []string)`, placed
beside `filterProvisioned` (`resolver.go:632`) and copying its contract exactly:
`len(required) == 0 || len(cands) == 0` returns `cands` untouched, and it builds
its result with the same `cands[:0:0]` idiom.

`Resolve` has **four** branches that reach `targetFrom` without passing through
one another. The gate goes on all of them:

| Branch | Where | How |
|---|---|---|
| Default fresh candidates | `resolver.go:501` | beside `filterServesEndpoint` |
| Server override | `resolver.go:601` | beside `filterServesEndpoint` |
| Model-group dispatch | `resolver.go:1230` (`eligibleCandidates`) | beside `filterServesEndpoint` |
| Affinity hit | inside `resolveAffinity` | off the `caps` it already fetches |

The server-override site needs no new error plumbing: its empty case already
returns `ErrServerOverrideModelUnavailable`, which already has its 404.

### 3.1 The affinity branch, and why it is not `affinityApplicationStale`

`resolveAffinity` already fetches `MappingCapabilities` for the live-progress
verdict (`resolver.go:768`). The image gate reads the **same local** — zero extra
store call. This is the one place the capability axis is strictly cheaper than a
join.

Two rules there, both deliberate:

- **A read error is NOT-SATISFIED**, unlike live-progress's advisory `""`
  degradation at `:756-766`. Live-progress failing open costs one annotation; an
  image gate failing open holds for the whole `AffinityTTLSeconds`.
- **The refusal is non-destructive**: return `(Target{}, false, nil)` and fall
  through to the fresh-candidate path. This is a new shape in that function and
  must be commented as such, because all seven existing rejections there delete
  the affinity row.

**Do not put the gate in `affinityApplicationStale`.** Every stale verdict there
leads to `DeleteAffinity`, and `AffinityKey.APIFlavor` is *coarse*
(`NormalizeAPIFlavor` at `resolver.go:430`; the key is four fields at
`store.go:489`), so an image request declaring the pin stale would delete **the
chat client's pin**. Recorded here as a rejected alternative so a later reviewer
does not "simplify" the non-destructive fall-through into the stale check.

### 3.2 The write-side leak the issue does not mention

A read-side gate does not fix this. Because the affinity key is coarse, an image
resolve on the main path would write its pin under **the same key a chat client
uses**, repointing that client at an image server. There are three
`UpsertAffinity` call sites in `resolver.go`: `:560` (main pin), `:750` (the
refresh inside `resolveAffinity`), and `:1449` (`upsertGroupPin`, whose own doc
says it mirrors the main pin).

Guard `:560` and `:1449` on `len(req.RequiredCapabilities) == 0` — keyed on the
capability list, **not** on a flavor string, so #68 and #69 inherit the guard.
Guarding the affinity read makes `:750` unreachable, so those two writes plus the
read guard are exhaustive.

### 3.3 Refusal, legibly

- A new sentinel `routing.ErrModelNotCapable`, with its own case in
  `completionErrorCode` (`internal/gateway/inference_complete.go:937`) and its
  own message. "No capable model" must not surface as "unknown model".
- HTTP status 404, following `ErrServerOverrideModelUnavailable`'s precedent.
- **Fold in a related defect:** `completionHTTPStatus` (`:921-935`) has no case
  for `ErrNoModelRoute` or `ErrNoHealthyHost`; both fall through to
  `http.StatusBadGateway`. So a group-path refusal would be a 502,
  indistinguishable from an upstream outage. Both get explicit cases in this
  change.
- Group-path semantics: an all-chat model group receiving an image request is an
  **unknown model** (`ErrNoModelRoute`), gating before `live` is taken in
  `eligibleCandidates`. That follows the existing precedent — endpoint
  non-service already runs before `live`.

### 3.4 The chat path must be bit-identical

The filter early-returns on a nil required list and this design changes no SQL,
so the cost on the chat path is one length check. The load-bearing test asserts
that an `openai_chat_completions` resolve returns the **same target** for a
mapping with an `image: no` row, an `image: yes` row, and no row at all.

## 4. The unknown direction: refuse

An absent capability row means *unknown*, not *no*. This design **refuses**, and
the reason is in the code rather than in taste: `Extra`-sourced rows are written
`yes`-only (`cmd/gateway/app_health.go`), and the Ollama detector can
structurally never emit `no` (its own comment says absence means "Ollama did not
tell us"). A lenient gate would therefore refuse **nothing** in the real fleet —
it reproduces the exact defect this issue is filed against.

**The cost, stated plainly rather than discovered later:** on ship day no mapping
carries an `image` row and every image request returns 404 until an operator
enables one. That belongs in the release notes.

## 5. Day-one enablement, and why the automatic writer is a separate issue

The manual path **already works today, with no schema or API change**:
`{"capability_verdicts":{"image":"yes"}}` on the mapping-update endpoint writes an
`image` row with source `manual` (`internal/portal/service_applications.go:2874`
accepts any non-empty capability name verbatim). One API call per mapping enables
an `sd-server`.

The automatic writer the issue's decision 2 implies — reading
`GET /sdcpp/v1/capabilities` — needs an agent-side probe, a wire-contract
addition, an `agent.Features` flag and therefore a MINOR ServerAgent version
bump. Pulling that into an issue already estimated at ~8 tasks buys nothing that
the manual call does not, so it is filed separately.

**One thing this design must NOT do:** add `image` to
`reservedAgentCapabilityNames` (`internal/gateway/agent_ingest.go:1038`). That
list is keyed on names *no probe can observe*; an `sd-server` probe will observe
exactly this one, and listing it would block the follow-up writer before it is
written.

### 5.1 `(image, no)` joins `reservedManualVerdicts`

`reservedManualVerdicts` (`internal/portal/service_applications.go:276`) is keyed
on the **pair**, and `(live_progress, no)` is there because a `manual` row is
rank 3, outranks every automated source, and nothing re-derives it.

`(image, no)` gets the same treatment, and the argument is asymmetric:

- It costs the operator **nothing**. Under §4 an absent row already refuses, so
  "this model cannot generate images" is fully expressed by not writing a `yes`.
- It prevents a permanent veto. The moment the §5 writer lands, any `no` written
  in the interim outranks it forever, against a genuinely capable model.

Reserving a pair whose writer does not exist yet is unusual, so the ADR records
that it is forward protection bought at zero cost — and that the reset (`""`) is
never refused, so there is a way back either way.

## 6. The request path

- **Route.** `routes()` (`internal/gateway/server.go:1165`) is a flat
  `ServeMux`; each logical endpoint is registered twice, bare and `/openai/`-
  prefixed. Four lines.
- **Preflight is non-negotiable.** Call the existing `inferencePreflight`
  (`internal/gateway/inference_handlers.go`), which carries model-override
  resolution, the unknown-model redirect, `applyServerOverride` re-auth, the
  allowlist and `admitPrincipal`. Add **no** second `admitPrincipal`; there is
  exactly one and its comment records what broke when there were four.
- **`sessionEndpoint`.** Adding an iota member alone is not enough: both switches
  in `session_extract.go` have no `default` branch, so the new endpoint's rows
  would carry an empty `session_id`/`session_source` and session affinity would
  be silently off. Handle the member explicitly in both — and since an images
  request has no natural session signal, the explicit handling is *"this endpoint
  has none"*, written down rather than left to fall through. (Recorded on the
  issue already, from #70's reconnaissance.)
- **Model resolution.** `sd-server` **ignores** the request's `model` field: one
  process serves one model. The routing model therefore comes from our own
  mapping, via the same `sniffRoutingModel` JSON probe every native-passthrough
  endpoint uses — the request is JSON, so this works unchanged.
- **Relay.** Use `proxyNative`/`nativeCopier`
  (`internal/gateway/native_passthrough.go`): a 32 KiB read/write/flush loop with
  a bounded tee for capture, already relaying the upstream `Content-Type`
  verbatim. Do **not** use `complete()`, which `json.Marshal`s the whole client
  body a second time — wrong for multi-megabyte base64.
- **Labelling.** `upstreamPath`'s default branch returns
  `/v1/chat/completions` (`native_passthrough.go:109`). Without its own case
  every image request is mislabelled on the usage row and in Activity.
- **Timeout.** `defaultApplicationTimeoutMS` is 30 s
  (`internal/portal/service_applications.go`) and will abort most real
  generations; `server_agent` applications already default to 600 s. The spec
  must not leave an `sd-server` on the 30 s default.
- **Validation.** `inference.Request.Validate()` requires `len(Messages) > 0`.
  An images request has none, so the handler validates its own shape (prompt
  required) and does not call the chat validator.

### 6.1 Capture

`captureMaxBytes` is 1 MiB and bodies are stored as JSON strings clipped at it. A
base64 image exceeds that by design. This change does not widen the cap: it
records that an images capture is clipped, and that the clip is the honest
outcome rather than a bug — the same reasoning #70 applied to a measured zero.

### 6.2 Error normalisation

`sd-server` returns `{"error": "<plain string>"}`, not OpenAI's
`{"error":{message,type,code}}`. The relay normalises it. The code must record
that `type` and `code` are then **ours**, not the backend's, so nobody later
reads them as an upstream statement.

### 6.3 Usage, on #70's rails

`usageMeta` gained `BillingUnit`/`BillingQuantity` in #70; the images call site
sets `usage.BillingUnitImage` and the count. The XOR requires all seven
token-denominated columns to be 0 — for a relayed image response nothing writes
any of them, so that holds by construction, and `ValidateBillingXOR` logs a
violation if a future change breaks it.

**The quantity comes from the response, not the request.** `n` states what was
asked for; `data[]` states what was produced, and a partial failure makes those
differ. Metering what was asked for would be the same class of error as counting
a measured zero as a measurement.

Energy needs nothing: Tiers 1 and 2 are time-based and already price a
zero-token request, and #70's `unpriceable` stamp covers a Tier-3-only host.

### 6.4 Stickiness: no

Image requests do not pin deterministically. Making them pin needs a
`route_affinity` discriminator column, because the key is coarse and
`affinityID` hashes it — which would change `affinityID` for existing
`openai_responses` traffic and orphan every stored row. That is its own pull
request.

Under this design images keeps stickiness wherever the pinned mapping is
genuinely image-capable, which is strictly better than refusing to pin at all.

## 7. The launch shape: `custom`, not a new runtime kind

`RuntimeSpec` is Binary + an opaque Args array + Env + WorkDir + ListenPort +
timeouts, so an `sd-server` launch — including a multi-file model whose UNet, VAE
and text encoders are separate flags — is expressible with **no schema change**.

Use `Type = custom` with explicit paths rather than adding an `sd_cpp` kind. A
new kind would need a case in `runtime_spec_type.go` (constant,
`DeriveProbePaths`, `DetectRuntimeSpecType`), in `validRuntimeSpecType`, and a
review of `live_timings.go` and the benchmark/ingest paths that assume an
LLM-shaped provider — eight-plus files — and `DeriveProbePaths`' `custom` branch
already returns exactly the right thing: empty strings, because `sd-server` has
neither a metrics nor a context axis. A dedicated kind's only real payoff is
auto-detection from the binary name and a friendlier dropdown label.

Two specifics the spec must carry:

- `pollHealth` defaults `HealthPath` to `/health`, which `sd-server` does not
  serve, so an `sd-server` spec **must** set it explicitly.
- The `MODEL` placeholder expands to the spec's upstream model **name**;
  `sd-server`'s `-m` takes a filesystem **path**. So the weights path is written
  literally in `Args` and the placeholder is not used. The `PORT` placeholder
  works.

**Open item, honestly flagged:** the reconnaissance could not verify
`sd-server`'s actual liveness route from upstream source — it relied on the
issue's reading. The concrete `HealthPath` value must be confirmed against the
real binary before the launch documentation claims one, and if `sd-server` has no
2xx-returning GET at all, the health-poll model itself needs revisiting rather
than a path guess.

## 8. Testing

- The gate refuses on **all four** `Resolve` branches, each asserted separately.
- The affinity refusal is non-destructive: the pin still exists afterwards.
- The affinity and group **writes** do not create or overwrite a pin for a
  capability-carrying request.
- Chat routing is bit-identical across `image: yes` / `image: no` / no row.
- A capability read error on the affinity path refuses rather than failing open.
- The relay does not re-marshal: a multi-megabyte base64 payload streams.
- An `sd-server` plain-string error reaches the client as a valid OpenAI error
  object, with the mapping pinned.
- The usage row carries the `image` unit, a quantity taken from the response, zero
  tokens, and the correct `ReqPath`; Activity labels it as images.
- A refusal is a 404 with its own code, and a group-path refusal is no longer a
  502.
- **The request method and path per runtime kind are pinned**, because
  whisper.cpp's own test helper discarded `*http.Request` and that is exactly why
  #54's GET-vs-POST bug shipped.

## 9. Documentation

`routing-and-model-selection.md` (the first excluding filter, the four branches,
the affinity read and write rules), `agent-runtime-manager.md` (the `custom`
launch shape and the `HealthPath` obligation), `compatibility-and-inference.md`
(the endpoint, the error normalisation and whose `type`/`code` those are),
`api-surface.md` plus `openapi.yaml`, `telemetry-usage-observability.md` (the
`image` unit and the response-sourced quantity), and **ADR-042** recording: the
gate keys on a required capability rather than an API flavor and why; an absent
row refuses and what that costs on day one; `(image, no)` is reserved before its
writer exists; and `/v1/models` keeps advertising what the router refuses (§10).

## 10. Recorded, not fixed: the list lies while the router refuses

`ModelOfferingFor` is called with the **coarse** flavor
(`internal/gateway/inference_handlers.go:503`), so `/v1/models` will keep
advertising an images-only model to chat clients. This is axis-independent — it
would be true under the flavor design too. It is recorded as a known gap in the
ADR rather than fixed here, because fixing it means teaching the offering path a
capability dimension, which is its own change.

## 11. Task breakdown

1. The carrier and the filter: `RequiredCapabilities`, `CapabilityImage`,
   `filterCapable`, the three `filterServesEndpoint`-adjacent sites, and the
   bit-identity test.
2. The affinity branch: the read-side gate off the existing `caps` local,
   non-destructive, read-error-refuses; plus the two write guards.
3. Refusal legibility: `ErrModelNotCapable`, its code and 404, and the folded-in
   `ErrNoModelRoute`/`ErrNoHealthyHost` status cases.
4. `(image, no)` in `reservedManualVerdicts`, with its test.
5. The endpoint: route, preflight, `sessionEndpoint` handled in both switches,
   own-shape validation, relay via `proxyNative`, `upstreamPath` case.
6. Error normalisation and the capture-clipping record.
7. Usage: the billable unit and the response-sourced quantity.
8. The `sd-server` launch shape under `custom`, with the confirmed `HealthPath`.
9. Docs and ADR-042.

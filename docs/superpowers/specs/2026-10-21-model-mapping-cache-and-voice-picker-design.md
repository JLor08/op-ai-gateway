# Design — Per-mapping model cache + discoverable TTS voice picker (#251)

- **Date:** 2026-10-21
- **Status:** Draft design — not yet approved, nothing built.
- **Scope note:** Branch-local working document under `docs/superpowers/specs/`. Its
  durable outcomes fold into `docs/architecture/` (+ a new **ADR-067**) and this file
  is deleted before the pull request. It never reaches `main`.
- **Issue:** #251 (discoverable voice picker). Builds on #248 (TTS chat composer, shipped),
  #252/#273 (the `speech`/`speech_clone` distinction, shipped). Related: #272 (clone
  reference-voice epic, deferred), ADR-061(j), ADR-063(f), ADR-055.
- **Base:** `origin/main` @ `0ab7c6a` (agent Version 0.10.2, latest migration v93).

## 1. Intent and why

The portal chat TTS composer takes **`voice` as free text** today (`Chat.tsx`, a
`TextField`). A name the model does not know fails upstream. #251 replaces it with a
**discoverable picker**. The obstacle: voices are only queryable from a **loaded**
audio.cpp model (`GET /v1/audio/voices`), and the gateway does **not** proxy that
listing (ADR-061(j)); so when the model is unloaded there is nothing to populate a
dropdown from.

The solution has two halves that are worth building together:

1. A **general, persistent per-ModelMapping cache** (new infrastructure). Information
   discovered from a loaded model is stored per mapping and **survives unload/restart**.
   Voices are its first consumer; the facet is deliberately general (other per-mapping
   discovered facts can use it later).
2. #251 itself: make voices **reachable** (gateway proxy for static servers **and** an
   agent-router voices route for agent-launched children — the "agent part"), **fill the
   cache** on each model load, expose the cache as a **picker** (and a public
   `GET /v1/audio/voices` endpoint), and make a **chosen voice route to a server that has
   it**.

Prior art (llama-swap) validates the shape: it names the model on a GET via a `?model=`
query param (`GET /v1/audio/voices?model=tts` → `{"model":"tts","voices":[...]}`),
answers voices from a **persisted** source without loading the model (discovered
capabilities persisted via its `store.path`; a declared `capabilities.voices` list), and
keeps voices **out of** `/v1/models`. Our design mirrors all three.

## 2. The three pieces

- **A. The cache** — a general `model_mapping_cache` table + store methods (memory /
  sqlite / postgres), migration **v94**.
- **B. Voices reachability + population** — a gateway provider "voices lister", an agent
  router voices route behind a new feature flag (agent **0.10.2 → 0.11.0**), and a
  change-detected fetch-on-load hook in the health loop that writes the cache.
- **C. Consumption** — a public `GET /v1/audio/voices?model=` endpoint, the portal
  combobox picker, and a **fail-open voice→server routing filter** (covering single
  models **and** groups).

## 3. Decisions

### 3.1 Cache keying = per **mapping** (the "per-server" case is already per-mapping)

In this gateway a `ModelMapping` is 1 mapping = 1 `(AIServer, Application, AppModelName)`.
"The same model on different servers" is therefore **several mappings sharing one
`GatewayModelName`** (resolved by `ActiveMappingsForModel`), not a second dimension. So
the cache is keyed by **`mapping_id`**, and the "combined set" a user sees is the
**union across the active mappings for that gateway model name** (and, for a group, across
the group's member mappings — §3.7). *(My call: one row per mapping for voices; `scope_key`
stays in the schema for future sub-mapping granularity but is `""` for voices.)*

### 3.2 The general cache table (v94)

`model_mapping_cache(mapping_id, cache_type, scope_key, payload, updated_at)`:
- `mapping_id` — FK to `model_mappings(id)` **on delete cascade** (and a manual cascade in
  `MemoryStore.deleteMappingLocked`).
- `cache_type` — e.g. `"voices"`.
- `scope_key` — sub-key within a type; `""` for voices.
- `payload` — `text` holding JSON (the voice list for voices).
- `updated_at` — `dl.timestampType()`.
- PK `(mapping_id, cache_type, scope_key)`; index `idx_model_mapping_cache_mapping`.

Mirror the `model_mapping_capabilities` store code (single/bulk read, tx upsert
`on conflict do update`, delete; memory twin with an FK existence check). Append-only
`migration94Up` (create-table pattern of `migration78Up`/`migration87Up`). Regenerate the
tracing decorator (`go generate ./internal/tracing/...` + `scripts/add-license-headers.sh`).
Conformance (`forEachRoutingStore`), a per-migration test, and a parity fixture if the
column-coverage test requires one.

### 3.3 Cache population = change-detected, on each health tick while loaded

There is **no** clean "just became loaded" event in the gateway today. The robust,
restart-surviving approach (the `writeCapabilityRows` precedent) is: on each health pass,
when the mapping's model is observed **loaded**, fetch its voices and **write only on
change** (compare to the stored payload). The observable outcome equals "refresh on every
load". Gate the fetch on **tts only**: `(speech, yes)` AND **not** `(speech_clone, yes)`
(reuse #252's verdict; never fetch for clone or non-speech mappings).

- **Static `audio_cpp` servers:** extend the existing `probeAudioCppModels` pass to decode
  the `loaded` flag for the mapping's `AppModelName` (the listing already carries it; the
  current `audioCppModelsDoc` drops it) and, when loaded, call the new voices lister. The
  gateway reaches `{ApplicationEndpoint}/v1/audio/voices` directly. The gateway cannot
  load a static model (operators load out of band), so "fetch while loaded" is all that is
  possible here — correct by design.
- **Agent-launched children (`server_agent`, spec type `audio_cpp`):** a new health-loop
  pass modelled on the `{model}` context-probe branch — gated on
  `LoadedAppModels(app, server)`, on the new agent feature flag
  (`hasAgentFeature(...)`), and on `EffectiveRuntimeSpecType == audio_cpp`; builds per-mapping
  credentials via `SpecUpstreamAuth` + `capture.OpenSecret`; fetches through the agent
  **router** voices route (never starts a cold child — a cold child answers 404
  `runtime.model_not_running`). *(My call: fail-closed on a missing agent flag — an old
  router without the route 404s like an unmanaged model; so an old-agent server simply has
  no cached voices, which the picker handles fail-open.)*

Keep the writer **change-detected and memoised** (the hot path runs ~1×/s per agent plus
every probe tick; GOTCHA 9). `healthStore` gains the cache write method; `fakeHealthStore`
in `app_health_test.go` must gain it too.

### 3.4 Voices reachability (the #251 "agent part")

- **Gateway provider "voices lister":** a new optional provider capability — interface in
  `internal/provider`, method on `*OpenAICompatibleClient`, `Multiplexer` dispatch with a
  tracing span (mirror `AudioCppModelsProber` / `RuntimeEnsurer`; `var _ I = (*Multiplexer)(nil)`).
  Auth via `applyUpstreamAuth`, 1 MiB body bound, `target.Timeout`. Parse the audio.cpp
  response (shape **verified live** — §7). If a static server and an agent child return
  different shapes, the parser is twin-tested like `AudioCppModelsProber`.
- **Agent router voices route:** a new route next to `/props` in
  `server-agent/internal/runtime/router.go` — **Status-only, never-start**, outbound path
  `/v1/audio/voices`, model taken from the request path/query, byte-verbatim relay,
  per-spec token forwarded; a cold child → 404 `runtime.model_not_running`.
- **Agent feature flag + version bump (the single bump for this branch):** a new
  `agent.Features` entry (name e.g. `runtime_voices`), `Since: "0.11.0"`; `Version`
  **0.10.2 → 0.11.0** (MINOR, because `Features` gains an entry — `features_test.go`
  enforces it); a `Since` test mirroring `runtime_config_files`. Gateway-side: a const in
  `internal/gateway/agent_features.go`, checked **fail-closed** via `AgentFeatures.Has` /
  `hasAgentFeature`. **Merge `origin/main` before finalizing and base the bump on main's
  then-current version** (serial roadmap PRs collide on the agent version).

### 3.5 The public endpoint

`GET /v1/audio/voices?model=<gateway-model-name>` (+ the `/openai/v1/audio/voices` alias),
auth like the other `/v1/audio/*` endpoints: **bearer token OR the internal loopback
pair, never the session cookie**. It **reads the cache** (the union across the model
name's active mappings) and **never loads a model**. Response shape mirrors the convention:
`{"model": "<name>", "voices": [ ... ]}` (the exact per-voice fields set once the live
audio.cpp shape is verified — §7). Voices stay **out of** `/v1/models`. Update
`reference/api-surface.md` (the voices row) and `reference/openapi.yaml`.

**Gate + returns (resolved after the builder-check).**
- **Visibility gate = routing-consistent** (not the `/v1/models` listing gate, not the
  portal hidden/locked gate): the public endpoint returns voices for exactly the mappings a
  dispatch for this token could reach — `filterProvisioned` + the group `DirectAllowed`
  check + the **current** speech/`speech_clone` (tts) verdict. So it never exposes a model
  the token cannot route to, and never hides one it can. (The portal route in §3.6 instead
  uses the portal hidden/locked + provisioning gate, over the same shared union core.)
- **Group names are accepted** and expanded (`Group(name)`); the union spans the group's
  member mappings (§3.7).
- **Read-side verdict gate:** filter by the **current** verdict at read time, not only by
  what was written — a mapping that has since flipped to `speech_clone` or `speech:no`
  returns no voices even if a stale row lingers (ties to §3.8 invalidation).
- **Status codes (no-leak):** missing `model` → `400 model.name_required`. Every "nothing
  to show" case — unknown name, not-visible, non-speech, clone, or simply nothing cached —
  returns **`200` with an empty `voices` list** (do not distinguish them; the picker's
  free-text fallback and the no-leak posture both want a uniform empty 200).

### 3.6 The portal picker (combobox)

Replace the free-text voice `TextField` (`Chat.tsx:672-685`) with a **combobox**: a
dropdown of the cached voices that **also allows free text** (a freeSolo-style control, so
the user can pick from the list **or** type an arbitrary name). *(My call: SearchableSelect
is select-only; use a free-text-capable combobox so the current free-text behavior is
preserved.)*

**The browser uses a session-cookie portal route, not the public endpoint.** *(Correction
after the builder-check: the picker is browser code; it cannot use the internal loopback
pair — nginx blanks it, and `/v1/audio/voices` refuses the session cookie. "Dogfood over
loopback" was wrong.)* Add a **new session-cookie route**
`GET /api/portal/model-voices?name=<gateway model name>` (`requireWebScope(scopeGatewayUse)`,
`?name=` required else `400 model.name_required`), exactly mirroring
`handlePortalModelImageOptions` / `/api/portal/model-image-options`. It applies the
**portal visibility** gate (`modelServersSuppressed` hidden/locked suppression +
`filterVisibleMappingViews` provisioning), then returns the **union** of the model's cached
voices. The public `/v1/audio/voices` (bearer/loopback, §3.5) and this portal route **share
one "voices-for-model-X-as-principal-P" core** and differ only in their auth leg and
visibility gate. Mirror the image-sampler fetch effect (`ChatStore.tsx:1228-1246`):
clear-first, latest-wins guard, and on failure/empty **fall back to free text** (fail-open).
Keep a saved pick visible even if the list omits it (the `imageChoices` pattern). DE + EN
i18n keys together.

### 3.7 Voice → server routing (fail-open filter, single models **and** groups)

- The speech handler **sniffs `voice`** from the raw body (as it already sniffs `model`).
- A new **`inference.Request.RequiredVoice`** field carries it. *(My call: a dedicated
  field, not a dynamic `voice:<name>` capability token — the voice filter is **fail-open**,
  the opposite of the fail-closed capability semantics, and a field avoids the
  `ModelOfferingFor` folding carve-out GOTCHA 5 would force.)*
- **The filter is fail-open:** when `RequiredVoice` is set **and ≥1 candidate mapping's
  cached voices contain it**, restrict candidates to those; **otherwise do not filter**
  (route as today). This protects existing bearer clients that pass a free-text `voice`
  (unchanged behavior), and matches "a free/unknown voice just risks an upstream error".
  The dropdown always offers cached values, so the chat path with a picked voice is the
  strict (filtered) case.
- **Standard (single-model) path:** the filter runs where `filterCapable` runs
  (`resolver.go`), reading the voices cache via a new `resolverStore` method.
- **Groups need a group-level PRE-PASS, not a per-member filter.** *(Correction after the
  builder-check: `eligibleCandidates`/`selectMember` see one member's candidates at a time,
  so a per-member "restrict only if ≥1 of THIS member's candidates has the voice" rule
  leaves a voice-less member fully eligible, and speed/priority order can then pick it ahead
  of the member that has the voice — the group is never narrowed.)* So: before member
  iteration, compute over the **flattened group** both "does ≥1 candidate anywhere in the
  group have the voice?" (the fail-open switch) and the **allowed mapping set**, reusing the
  same provisioning/endpoint/capability filters; thread that result through `resolveGroupOnce`
  into `eligibleCandidates`, `orderMembersBySpeed`, `firstAvailable` and `climbTarget` (which
  today take `required []string`, not `req`). A group's offered voices are the **union across
  its member mappings**; the chosen voice narrows the allowed members to those that have it,
  then normal group dispatch/scoring applies over that set. **The voice restriction is NOT
  relaxed by the `resolveGroup` relaxation ladder** (loaded-only and the speed floor are
  relaxed; the voice set is not).
- **After the filter, the existing load-on-demand + admission is unchanged:** a viable
  server is loaded and serves; if none is viable the request is **rejected**. *(Factual note
  from the builder-check: `selectWithSwapProtection` and the capacity filter **fail open**,
  so rejection to `ErrNoHealthyHost` arises only for **unreachable/unhealthy/ServerBusy**
  candidates — not for "server is holding another model", which swap-protection permits. The
  dedicated "voice only on a currently-unavailable server" sentinel (open call A) therefore
  fires for fewer cases than a naive reading suggests; the generic 404/503 covers the rest.)*

### 3.8 Cache write + invalidation semantics (resolved after the builder-check)

- **Only a successful fetch writes.** A transport error, a cold-child `404
  runtime.model_not_running`, an old agent without the voices route/flag, or a timeout
  **never overwrites or clears** a non-empty cached row — last-known-good stands (the
  `config_client`/RoutesClient discipline). A **successful** fetch that legitimately returns
  an empty set **does** write empty (that is the truth for a model with no built-in voices);
  so the writer distinguishes "fetch failed" (leave) from "fetch succeeded, empty" (write).
- **Back off, don't hammer.** Gate the agent-child fetch on the `runtime_voices` feature
  flag (an old agent is never probed) and memoise per `(mapping, child generation)` like the
  context-probe cache; a static server that does not support the listing is backed off too,
  so a missing route does not cost a GET per loaded tts mapping every tick forever.
- **Store a canonical form; change-detect on it.** Persist a **normalized** payload (voices
  deduped and sorted by id/name, with whatever fields audio.cpp returns) and compare the
  normalized form, so cosmetic upstream reordering does not flap a write. The normalized form
  still retains the per-voice fields, so richer picker display (open call B) stays additive.
- **Invalidation.** Delete a mapping's cache rows when its `AppModelName` changes (the cached
  voices are model-specific and the next load repopulates them). A **verdict flip**
  (tts→clone, or speech→no) needs no delete because the **read-side verdict gate** (§3.5/§3.6)
  already suppresses a stale row; the next load's tts-gated write stops refreshing it. The FK
  cascade removes rows when the mapping is deleted (manual cascade in the memory store).

## 4. Constraints

- Never commit to `main`; feature branch + PR; remove `docs/superpowers/**` before the PR.
- Keep all three store drivers working; append-only migration **v94**; wide values → wide
  Postgres columns (ADR-005); verify the Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN`.
- Arch tests are frozen both ways: put the cache types in `routing`, the lister in
  `provider`, the health write in `cmd/gateway`; the agent voices code in `runtime`
  (imports only `gwapi`). No new import edges.
- One ServerAgent version bump for the whole branch; **merge `origin/main` and re-base the
  bump on main's current version before finalizing** (version-collision memory). The same
  re-base applies to the three other monotonic identifiers a serial roadmap PR also claims:
  the **migration number** (v94 here — re-check it is still the next free one), the **ADR
  number** (ADR-067 here), and any shared enum/const; confirm all four at finalize.
- Preserve stable error codes; `voice` stays free text for bearer clients (fail-open).
- DE + EN i18n together; frontend prettier `format:check`; golangci-lint fmt+run per Go
  module; local `make sonar-gate` + branch-findings as pre-PR verification.
- `check-docs.sh` gates links/anchors/openapi `$ref`s/forbidden strings.

## 5. Security

- The public endpoint is bearer/loopback (never session cookie) and read-only on the cache;
  it never triggers a load.
- The agent voices route is Status-only and never starts a child; the per-spec token is
  forwarded only to the matching child, as `/props` does.
- The cache payload is operator-/server-derived voice names, not secrets; stored `text`.
- Fetch credentials: static = `app.APIToken`; agent child = per-mapping `SpecUpstreamAuth`.

## 6. Open calls for the human (the single round after the builder-check)

- **(A) Dedicated unavailable-voice error.** Confirm we add a specific message/sentinel for
  "the chosen voice is only on a currently-unavailable server" (vs. the generic routing
  404/503). *Default if unanswered: **yes**, add it.*
- **(B) Per-voice metadata in the picker.** If audio.cpp returns more than voice names
  (language, gender, sample), does the picker show any of it, or just names for v1?
  *Default: **names only** for v1; store the raw payload so richer display is additive later.*
- *(The exact wire shape of `GET /v1/audio/voices` is a build-time live-verification, §7,
  not a product decision.)*

## 7. What is left to the builder

- **Live-verify first (prerequisite).** Probe a real audio.cpp **v0.9.1** server's
  `GET /v1/audio/voices`: does it take a `model` query param? what does it return for an
  **unloaded** model? what fields per voice? Is the set per-model or per-server? (The
  team's habit — memory `audiocpp-server-ops`.) The parser and the endpoint's model-param
  handling follow from this; do not write the parser from assumption.
- **Also live-verify WHICH speech-request field consumes a listed voice name** (critical —
  it decides the whole sniff/picker/filter key). audio.cpp's `/v1/audio/speech` body has
  `voice`, **`voice_id`** ("a cached voice by id", memory `audiocpp-server-ops`), `voice_ref`
  and `voice_samples`, and OpenAI's `voice` may even be an object. If a listed voice's name/id
  belongs in **`voice_id`**, then a picker/filter/sniff keyed on `voice` does nothing: the
  portal `ChatAudioSettings` (which has only `voice` today), the speech-handler body sniff,
  and the `RequiredVoice` filter must all key on the **field the probe identifies**. Also pin
  the **matching rule** (exact / case-sensitivity / trim) and how an object-valued `voice` is
  treated (as unset). The builder keys the sniff, picker output, cache entries and filter on
  that one verified field.
- **A. Cache:** `routing.ModelMappingCache*` types + `Store` methods (memory + SQL), migration
  v94, tracing-decorator regen, conformance + migration tests, `deleteMappingLocked` cascade.
- **B. Reachability + population:** the provider voices lister (+ Multiplexer dispatch);
  the agent router voices route + `runtime_voices` flag + Version 0.11.0; the health-loop
  fetch-on-load passes (static + agent), change-detected, tts-gated; `healthStore` +
  `fakeHealthStore` growth.
- **C. Consumption:** the `GET /v1/audio/voices` route + handler + alias; the portal
  combobox + loopback fetch + DE/EN i18n; the `RequiredVoice` plumbing (speech-handler
  sniff → `inference.Request` → the fail-open filter in the standard and group paths +
  `resolverStore` read); the dedicated unavailable-voice sentinel (pending (A)).
- **Docs:** **ADR-067** (first persistent per-mapping cache — amend ADR-055 "never stored",
  ADR-061(j) "voices not proxied", ADR-063(f) "voices dropdown deferred"); `api-surface.md`,
  `openapi.yaml`, `data-model.md`, `persistence.md`, `agent-runtime-manager.md` (flag +
  version history), glossary, compat §3.6/§12.
- **Tests:** cache store round-trip (3 drivers, Postgres leg); the voices lister parser
  (twin-tested if parsed in both modules); the fetch-on-load change-detection; the fail-open
  filter (voice known on ≥1 → restrict; unknown/empty → no filter) in single and group
  paths; the public endpoint (union, empty-when-uncached, auth); the picker (combobox,
  free-text fallback, i18n parity); a local e2e if the harness runs.

## 8. Goals / non-goals / anti-goals

**Goals.** A persistent per-mapping cache; voices reachable for static **and** agent-launched
audio.cpp (the #251 agent part); voices available for selection (picker + public endpoint)
**even when the model is unloaded**; a chosen voice routed to a server that has it, loading
on demand or rejecting clearly; works for single models **and** groups; tts-only.

**Non-goals.** The clone reference-voice path (`voice_ref` + reference text) — that is #272.
Transcoding; streaming; non-audio cache facets (the store is general but only voices lands
now). Showing voices in `/v1/models`.

**Anti-goals (a failure even if it "works").** A fail-**closed** voice filter that breaks
existing free-text bearer clients or an empty/fresh cache; fetching/offering voices for a
`speech_clone` or non-speech model; starting a cold child just to list voices; a version
bump that is not re-based on main's current version at finalize; voices cached without
change-detection on the ~1/s hot path.

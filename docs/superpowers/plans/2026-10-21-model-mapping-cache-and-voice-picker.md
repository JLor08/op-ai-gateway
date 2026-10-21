# Per-mapping model cache + TTS voice picker (#251) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give audio.cpp TTS models a discoverable voice picker that works even when the model is unloaded, backed by a general persistent per-ModelMapping cache, with a chosen voice routed to a server that has it.

**Architecture:** A new general `model_mapping_cache` table (keyed by mapping_id + type + scope_key, JSON payload) mirrors `model_mapping_capabilities`. The health loop fetches voices from a loaded audio.cpp model (static: directly; agent-launched: via a new agent-router voices route behind a feature flag) and writes them change-detected. A public `GET /v1/audio/voices` endpoint and a session-cookie portal route read the union from the cache; the portal composer becomes a free-text-capable combobox; a fail-open `RequiredVoice` filter narrows candidate servers (single models and groups) before the existing load-on-demand/admission.

**Tech Stack:** Go (`op-ai-gateway`, `op-ai-server-agent`), React/TS portal, memory/sqlite/postgres stores.

**Spec:** `docs/superpowers/specs/2026-10-21-model-mapping-cache-and-voice-picker-design.md`

## Global Constraints

- Base `origin/main` @ `0ab7c6a`: latest migration **v93** → new is **v94**; agent `Version` **0.10.2** → **0.11.0** (one MINOR bump for the whole branch, because `agent.Features` gains one entry). **At finalize, merge `origin/main` and re-base all four monotonic identifiers on main's then-current values: the agent Version, the migration number, the ADR number (ADR-067), and any shared enum/const.**
- **`VOICE_FIELD`** = the audio.cpp `/v1/audio/speech` request field that consumes a listed voice name (`voice` vs `voice_id`), **determined by Task 1's live-verify**. Every sniff/picker/filter/`ChatAudioSettings` edit keys on `VOICE_FIELD`; do not assume `voice`.
- tts-only: fetch/offer/filter voices only for a mapping whose current verdict is `(speech, yes)` AND **not** `(speech_clone, yes)` (reuse #252/#273: `routing.CapabilitySpeechClone`, task spelling `"clon"`).
- Never commit to `main`; feature branch + PR; remove `docs/superpowers/**` and `docs/implementation-status.md` before the PR.
- All three store drivers stay working; append-only migration; wide values → wide Postgres columns (ADR-005); verify the Postgres leg with `OP_AI_GATEWAY_TEST_POSTGRES_DSN`.
- Arch tests frozen both ways: cache types in `routing`, lister in `provider`, health write in `cmd/gateway`, agent voices code in `runtime` (imports only `gwapi`). No new import edges.
- Fail-open voice filter: `voice` stays free text for bearer clients (unchanged). Public endpoint bearer/loopback, never session cookie. Agent voices route never starts a cold child.
- DE+EN i18n together; frontend prettier `format:check` + full `vitest run`; golangci-lint fmt+run per Go module; local `make sonar-gate` + branch-findings pre-PR; `check-docs.sh`.

## Review Focus

- **Fail-open filter regression:** a free-text/unknown voice, or an empty/fresh cache, must NOT refuse a request that works today (existing bearer clients). → Task 10 tests.
- **Group never narrows:** a per-member filter leaves a voice-less member eligible and pickable; the group-level pre-pass must actually restrict to members that have the voice, and survive the relaxation ladder. → Task 10 tests.
- **Secret / visibility leak on the endpoints:** the public endpoint must not expose a model the token cannot route to nor hide one it can; the portal route must honor hidden/locked suppression; neither distinguishes unknown/non-speech/clone (200-empty). → Task 7/8 tests.
- **Stale cache drives wrong behavior:** a verdict flip (tts→clon) or `AppModelName` edit must not keep serving old voices to the picker or the filter. → Task 2 (invalidation) + Task 7 (read-side verdict gate) tests.
- **Cold child / old agent / failed fetch:** must never clear a good cache row, never start a child to list voices, and never hammer a 404 route every tick. → Task 6 tests.

---

### Task 1: Live-verify audio.cpp `/v1/audio/voices` (prerequisite — controller/human, not a code subagent)

**Deliverable:** a recorded findings block in `docs/implementation-status.md` (branch-local) that Tasks 3, 7, 9, 10 consume. No product code.

- [ ] **Step 1:** Against a live audio.cpp **v0.9.1** server, capture: (a) the exact JSON of `GET /v1/audio/voices` for a **loaded** model and whether it accepts `?model=`; (b) what it returns for an **unloaded** model; (c) whether the voice set is per-model or per-server; (d) the per-voice fields; (e) **which `/v1/audio/speech` request field consumes a listed voice name** (`voice` vs `voice_id` vs object) — this fixes `VOICE_FIELD`; (f) the matching rule (exact / case / trim). If no server is reachable, the controller asks the human to run the two curls and paste the output.
- [ ] **Step 2:** Record a, b, c, d, e (`VOICE_FIELD`), f verbatim in `docs/implementation-status.md` under "Task 1 — audio.cpp voices contract". Mark it the source of truth for the parser (Task 3), the endpoints (Task 7/8), the picker (Task 9) and the sniff/filter (Task 10).

---

### Task 2: The `model_mapping_cache` store (routing types + Store + memory + sql + migration v94)

**Files:**
- Create: `gateway/backend/internal/store/sqlite_model_cache.go`
- Modify: `gateway/backend/internal/routing/store.go` (types + `MappingStore` ~1890-2001), `routing/memory_store.go` (map ~136, methods ~1129, `deleteMappingLocked` ~997), `store/migrate.go` (registry ~35-129), `internal/tracing/routingstore_gen.go` (regen)
- Test: `store/routing_store_conformance_test.go`, a new `store/migration94_model_mapping_cache_test.go`

**Interfaces:**
- Produces: `routing.ModelMappingCacheRow{MappingID, CacheType, ScopeKey, Payload string, UpdatedAt time.Time}`; on `MappingStore`: `UpsertModelMappingCache(ctx, row) error`, `ModelMappingCache(ctx, mappingID, cacheType string) ([]ModelMappingCacheRow, error)`, `ModelMappingCacheForMappings(ctx, mappingIDs []string, cacheType string) (map[string][]ModelMappingCacheRow, error)` (bulk, chunked like `capabilityBatchChunk`), `DeleteModelMappingCache(ctx, mappingID string) error`. Consumed by Tasks 5/6 (write), 7/8/10 (read), 2-invalidation.

- [ ] **Step 1: Write the failing conformance test** in `forEachRoutingStore`: upsert two rows for one mapping (`cache_type="voices"`, `scope_key=""`, distinct payloads under two mappings), read single + bulk, overwrite-changes-payload, delete-by-mapping, and FK cascade on mapping delete. Memory + sqlite + (DSN-gated) postgres. Plus an **invalidation** test: persisting a mapping whose `AppModelName` changed deletes that mapping's cache rows (a same-`AppModelName` update does not).
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/store/ -run ModelMappingCache -v` → FAIL (undefined).
- [ ] **Step 3: Implement** the type + `MappingStore` methods; `migration94Up` = `execTx(... "create table if not exists model_mapping_cache(mapping_id ... references model_mappings(id) on delete cascade, cache_type text not null, scope_key text not null default '', payload text not null default '', updated_at <timestampType>, primary key(mapping_id, cache_type, scope_key))")` + `idx_model_mapping_cache_mapping`, registered v94 (copy `migration78Up`); sqlite impl (single/bulk/upsert `on conflict do update`/delete, mirror `sqlite_capabilities.go`); memory impl + `deleteMappingLocked` cascade; regen tracing (`go generate ./internal/tracing/...` + `scripts/add-license-headers.sh`). Wire the **invalidation hook** into the mapping-update path (where `AppModelName` is persisted): when it changes, call `DeleteModelMappingCache(mappingID)` (verdict flips need no delete — the read-side verdict gate in Task 7 handles them).
- [ ] **Step 4: Run** the conformance + a new `migration94` upgrade test (real v93 DB → v94, both dialects) with/without the DSN → PASS.
- [ ] **Step 5: Commit** `feat(store): model_mapping_cache table + store, migration 94 (#251)`

---

### Task 3: Gateway provider voices lister

**Files:**
- Create: `gateway/backend/internal/provider/audiocpp_voices.go`
- Modify: `provider/multiplexer.go` (dispatch ~348-380, `var _ I = (*Multiplexer)(nil)`)
- Test: `provider/audiocpp_voices_test.go`

**Interfaces:**
- Consumes: Task 1's shape.
- Produces: `provider.AudioCppVoicesLister` interface with `ListVoices(ctx, target Target, model string) ([]Voice, error)`; `provider.Voice{ID/Name string, …fields per Task 1}`; `(*OpenAICompatibleClient).ListVoices` reaching `{Endpoint}/v1/audio/voices` (query `?model=` per Task 1), `applyUpstreamAuth`, 1 MiB bound, `target.Timeout`. Consumed by Tasks 5/6.

- [ ] **Step 1: Write the failing test** `TestParseAudioCppVoices` (parse the Task-1 shape into `[]Voice`; empty/absent → empty slice, not nil) + a Multiplexer-dispatch test (routes to the client, span emitted).
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/provider/ -run Voices -v` → FAIL.
- [ ] **Step 3: Implement** the interface, the client method + parser (keyed on Task 1's fields), and Multiplexer dispatch mirroring `AudioCppModelsProber`.
- [ ] **Step 4: Run** → PASS; `golangci-lint run ./internal/provider/...`.
- [ ] **Step 5: Commit** `feat(provider): audio.cpp voices lister (#251)`

---

### Task 4: Agent router voices route + `runtime_voices` flag + Version 0.11.0 + gateway feature const

**Files:**
- Modify: `server-agent/internal/runtime/router.go` (`ServeHTTP` switch ~312-333, new route beside `serveUpstreamProps` ~423-487), `server-agent/internal/agent/features.go` (`Features` ~212), `agent/agent.go` (`Version` ~305 + history), `agent/features_test.go` (~321,~352); `gateway/backend/internal/gateway/agent_features.go` (new const ~90-style)
- Test: `server-agent/internal/runtime/router_test.go`, `agent/features_test.go`

**Interfaces:**
- Produces: agent route `GET /v1/audio/voices` (model from path/query, **Status-only, never EnsureRunning**, outbound `http://127.0.0.1:<port>/v1/audio/voices`, byte-verbatim, per-spec token forwarded; cold child → `404 runtime.model_not_running`); feature name `"runtime_voices"` (Since `0.11.0`); gateway const `runtimeVoicesFeature` + `AgentFeatures.Has`/`hasAgentFeature` gate (fail-closed). Consumed by Task 6.

- [ ] **Step 1: Write the failing tests** `TestServeVoicesRelaysWhenRunning` (running child → relay) + `TestServeVoicesColdChild404` (not running → 404 `runtime.model_not_running`, never starts) + `TestFeaturesDeclareRuntimeVoices` (name present, Since `0.11.0`) — and `TestFeatureRegistry` goes red until `Version` is bumped.
- [ ] **Step 2: Run** `cd server-agent && go test ./internal/runtime/ ./internal/agent/ -run 'Voices|FeatureRegistry' -v` → FAIL.
- [ ] **Step 3: Implement** the route (mirror `serveUpstreamProps`'s Status-only shape), the `Features` entry, `Version` → `0.11.0` + a history paragraph, the `Since` test, and the gateway-side const.
- [ ] **Step 4: Run** → PASS; `go build ./...` both modules; golangci per module.
- [ ] **Step 5: Commit** `feat(agent): runtime_voices router route + flag, v0.11.0 (#251)`

---

### Task 5: Health-loop population — static audio_cpp servers

**Files:**
- Modify: `gateway/backend/cmd/gateway/app_health.go` (`probeAudioCppModels` ~789-822 / its pass ~1348-1372; a shared `writeVoicesCache` helper; `healthStore` ~35), `provider/audiocpp_models.go` (`audioCppModelsDoc` ~167 — decode `loaded`)
- Test: `cmd/gateway/app_health_test.go` (`audioCppFakeProber` ~3955, `fakeHealthStore` ~35)

**Interfaces:**
- Consumes: Task 2 store write, Task 3 lister, Task 1 shape, `#252` verdict.
- Produces: `writeVoicesCache(ctx, mappingID string, voices []provider.Voice) (changed bool, err error)` — normalizes (dedupe+sort), compares to the stored row, writes only on change; used by Task 6 too.

- [ ] **Step 1: Write the failing tests** (via the fakes): a loaded tts mapping → voices fetched + written; a second tick with identical voices → **no write** (change-detected); a `(speech_clone,yes)` or non-speech mapping → **never fetched**; a fetch error → **row unchanged** (last-known-good); a successful **empty** result → writes empty.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./cmd/gateway/ -run Voices -v` → FAIL.
- [ ] **Step 3: Implement** `loaded` decoding in `audioCppModelsDoc`; in the audio.cpp pass, for a loaded tts mapping call the lister and `writeVoicesCache`; add the write method to `healthStore` + `fakeHealthStore`. Back off a server that does not support the listing.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): cache static audio.cpp voices on load (#251)`

---

### Task 6: Health-loop population — agent-launched audio_cpp children

**Files:**
- Modify: `cmd/gateway/app_health.go` (a new pass modelled on the context-probe branch ~1101-1197)
- Test: `cmd/gateway/app_health_test.go`

**Interfaces:**
- Consumes: Task 4 (route + `runtimeVoicesFeature`), Task 5 (`writeVoicesCache`), `LoadedAppModels`, `SpecUpstreamAuth`.

- [ ] **Step 1: Write the failing tests**: an agent child that is `running`, tts, and whose server declares `runtime_voices` → voices fetched via the router + written; a child whose server **lacks the flag** → **never fetched** (fail-closed, no write); a **not-loaded** child → never fetched (never started); credentials come from `SpecUpstreamAuth`.
- [ ] **Step 2: Run** `go test ./cmd/gateway/ -run VoicesAgent -v` → FAIL.
- [ ] **Step 3: Implement** the pass: gate on `LoadedAppModels` + `hasAgentFeature(runtimeVoicesFeature)` + `EffectiveRuntimeSpecType==audio_cpp` + tts verdict; fetch through the router; `writeVoicesCache`; memoise per `(mapping, child generation)`.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): cache agent-launched audio.cpp voices on load (#251)`

---

### Task 7: Public `GET /v1/audio/voices` endpoint + the shared union/visibility core

**Files:**
- Create: `gateway/backend/internal/gateway/voices_handler.go`
- Modify: `internal/gateway/server.go` (route table ~1337-1340 + `/openai/v1/...` alias)
- Test: `internal/gateway/voices_handler_test.go`

**Interfaces:**
- Consumes: Task 2 read, `#252` verdict, group expansion.
- Produces: `handleAudioVoices` (`requireInternalOrBearerAnyScope`); and the shared core `voicesForModel(ctx, principal, modelName string) ([]provider.Voice, error)` returning the union across the model's active mappings (+ group members), filtered by `filterProvisioned` + `DirectAllowed` + the **current** tts verdict. Consumed by Task 8.

- [ ] **Step 1: Write the failing tests**: union across two same-name mappings; a group name expands to its members' union; missing `model` → `400 model.name_required`; unknown / non-speech / clone / nothing-cached → **`200` empty**; a model the token cannot route to → `200` empty (no leak); a model it can route to → its voices; a mapping flipped to clone after caching → empty (read-side verdict gate); response shape `{"model":...,"voices":[...]}`.
- [ ] **Step 2: Run** `go test ./internal/gateway/ -run AudioVoices -v` → FAIL.
- [ ] **Step 3: Implement** `voicesForModel` (routing-consistent gate) + the handler + routes + alias.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(gateway): GET /v1/audio/voices reads the cache (#251)`

---

### Task 8: Portal route `/api/portal/model-voices`

**Files:**
- Modify: `internal/gateway/server.go` (route ~1379-style), `internal/gateway/portal_model_endpoints.go` (handler mirroring `handlePortalModelImageOptions` ~536), `internal/portal/service_model_voices.go` (new, mirroring `service_model_image_options.go`)
- Test: `internal/gateway/portal_model_endpoints_test.go`, `internal/portal/service_model_voices_test.go`

**Interfaces:**
- Consumes: the Task 7 union core (shared), Task 2 read.
- Produces: `GET /api/portal/model-voices?name=` (`requireWebScope(scopeGatewayUse)`); `Portal.ModelVoiceViews(ctx, token, name) ([]provider.Voice, error)` applying the **portal** visibility gate (`modelServersSuppressed` hidden/locked + `filterVisibleMappingViews`) over the shared union.

- [ ] **Step 1: Write the failing tests**: `?name=` missing → `400`; a hidden/locked model for a non-admin → `200` empty; a visible model → its union; aggregation is a **union** (not an intersection).
- [ ] **Step 2: Run** `go test ./internal/gateway/ ./internal/portal/ -run ModelVoices -v` → FAIL.
- [ ] **Step 3: Implement** the portal service method + handler, reusing the Task 7 core with the portal gate.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** `feat(portal): /api/portal/model-voices for the picker (#251)`

---

### Task 9: Portal voice picker (combobox)

**Files:**
- Modify: `gateway/frontend/src/api/models.ts` (a `modelVoices(name)` fetch), `components/Chat.tsx` (voice field ~672-685 → combobox), `components/chat/ChatStore.tsx` (fetch effect ~1228-1246), `components/chat/chatDoc.ts` (`AudioSettings` ~498-514), `api/chat.ts` (`ChatAudioSettings` ~62-64), `internal/portal/service_chats.go` (`ChatAudioSettings` ~511-513) + `internal/gateway/chat_runs_audio.go` (`buildSpeechBody` ~282-315) **iff `VOICE_FIELD != "voice"`**, `src/i18n.ts` (de+en)
- Test: `components/Chat.test.tsx` (or the chat test file), `src/i18n.test.ts`

**Interfaces:**
- Consumes: Task 8 endpoint, Task 1 `VOICE_FIELD`.

- [ ] **Step 1: Write the failing tests**: the picker lists fetched voices and **still accepts free text** (combobox); on fetch failure/empty → free-text fallback; a saved pick absent from the list stays visible; `buildSpeechBody` sends the chosen value in `VOICE_FIELD`; i18n de/en parity for the new keys.
- [ ] **Step 2: Run** `cd gateway/frontend && npx vitest run src/components/Chat src/i18n.test.ts` → FAIL.
- [ ] **Step 3: Implement** the combobox + `modelVoices` fetch + the effect (clear-first, latest-wins, fail-open) + i18n; if `VOICE_FIELD != "voice"`, thread the new field through `ChatAudioSettings`/`buildSpeechBody`.
- [ ] **Step 4: Run** full `npx vitest run`; `npm run build`; `npx prettier --check` on touched files → PASS.
- [ ] **Step 5: Commit** `feat(portal): discoverable TTS voice picker (#251)`

---

### Task 10: Voice → server routing filter (fail-open; single models + group pre-pass)

**Files:**
- Modify: `internal/inference/types.go` (`Request.RequiredVoice` ~211), `internal/gateway/speech_handler.go` (sniff `VOICE_FIELD` ~165; `recordSpeechRoutingFailure` ~219 + a new sentinel), `internal/gateway/inference_handlers.go` (`inferencePreflight` ~535), `internal/routing/resolver.go` (`filterCapable`/`resolveStandard` ~745,1387; group pre-pass through `resolveGroupOnce`/`eligibleCandidates`/`orderMembersBySpeed`/`firstAvailable`/`climbTarget`; `resolverStore` ~394-429)
- Test: `internal/routing/resolver_test.go`, `internal/gateway/speech_handler_test.go`

**Interfaces:**
- Consumes: Task 2 read (`resolverStore` voices read), Task 1 `VOICE_FIELD`.
- Produces: `inference.Request.RequiredVoice string`; `routing.<sentinel> ErrVoiceUnavailable` → code e.g. `routing.voice_unavailable`.

- [ ] **Step 1: Write the failing tests**: voice known on ≥1 candidate → restrict to those; voice unknown/empty-cache → **no filter** (today's behavior); a **group** where only member B has the voice → routes to B, never A (group pre-pass), and the restriction is **not** relaxed by the relaxation ladder; the voice's only server down/busy → the new `voice_unavailable` surfaces (where `ErrNoHealthyHost` would); a non-chat bearer client with free-text `voice` is unaffected.
- [ ] **Step 2: Run** `cd gateway/backend && go test ./internal/routing/ ./internal/gateway/ -run 'Voice' -v` → FAIL.
- [ ] **Step 3: Implement** the sniff → `RequiredVoice` plumbing; the fail-open filter in the standard path; the group-level pre-pass threaded through the group functions; the `resolverStore` voices read; the dedicated sentinel + its code mapping.
- [ ] **Step 4: Run** → PASS; golangci.
- [ ] **Step 5: Commit** `feat(routing): fail-open voice->server filter incl. groups (#251)`

---

### Task 11: Docs — ADR-067 + architecture sweep

**Files:**
- Modify: `docs/architecture/09-architecture-decisions.md` (ADR-067; amend ADR-055/061(j)/063(f)), `reference/api-surface.md` (voices rows ~55), `reference/openapi.yaml`, `reference/data-model.md`, `cross-cutting/persistence.md`, `cross-cutting/agent-runtime-manager.md` (flag + version history), `cross-cutting/compatibility-and-inference.md` (§3.6/§12), `12-glossary.md`

- [ ] **Step 1:** Write **ADR-067** (first persistent per-mapping cache; the voices-dropdown/#251 decision): the cache, the reachability (gateway lister + agent route + flag), change-detected fetch-on-load, the two read endpoints + fail-open filter + group pre-pass, the tts gate; reference ADR-055/061(j)/063(f) (amend each) and #251.
- [ ] **Step 2:** Sweep the reference/cross-cutting docs (the `/v1/audio/voices` + `/api/portal/model-voices` rows, migration v94, agent `runtime_voices` + 0.11.0, the `model_mapping_cache` data-model row).
- [ ] **Step 3: Verify** `./scripts/check-docs.sh` passes (links/anchors/openapi/flags); `grep` for stale "voices not proxied"/"never stored" claims.
- [ ] **Step 4: Commit** `docs: ADR-067 + architecture for the voice cache (#251)`

---

## Finishing (not a task — superpowers:finishing-a-development-branch)

Merge `origin/main`; re-base the agent Version, migration v94, ADR-067 and any shared enum on main's current values. Fold durable content into `docs/architecture/`; delete `docs/superpowers/**` + `docs/implementation-status.md`; verify `git diff --name-only main...HEAD`. Run the full gates (both Go modules' lint, frontend format:check/test/build, `make sonar-gate` + branch-findings, the Postgres store leg). Squash-merge with a substantive commit body; enable Auto-fix on the PR.

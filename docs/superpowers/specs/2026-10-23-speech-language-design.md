# Design — Portal speech language (issue #282)

Status: draft for review (brainstorming output). Branch-local working doc;
fold durable parts into `docs/architecture/` and delete before the PR.

## Intent and why

A portal speech run never sends a `language` to audio.cpp, so a TTS model
whose spec default language is a fixed code renders text with the **wrong**
text frontend: German input synthesized by `confucius4_tts` (default `zh`)
comes out with a Chinese frontend ("sounds wrong"). A model whose default is
`auto` (`qwen3_tts`) is unaffected because it detects the language from the
text. The gateway has no way today to know a model accepts/needs the field,
nor to supply it. This blocks otherwise strong models — notably
`confucius4_tts`, the only installed German TTS that streams and emits
44.1 kHz.

The root cause and the audio.cpp behaviour are verified in issue #282:

- audio.cpp accepts `language` as a **top-level** field on
  `POST /v1/audio/speech`; it sets the transcript language (the authoritative
  language for the text frontend), is applied per model (`accepts_language`),
  and is **silently dropped** for models that don't accept it, so a client
  may send it unconditionally.
- The default differs per model and is stated by audio.cpp:
  `GET /v1/models?include_params=true` returns a `params` array per model
  (`name`, `type`, `default`, `required`). Live-verified: `qwen3_tts` →
  `language` default `auto`; `confucius4_tts` → `language` default `zh`. The
  `params` array does **not** enumerate the accepted values, only the default.
- The gateway's speech dispatch (`buildSpeechBody`/`buildSpeechStreamBody`)
  sends no `language`. `language` is **not** in `speechReservedFields`, so a
  passthrough already survives — the gap is supplying it and knowing whether
  the model accepts it.

## Goals

1. A portal speech run can synthesize the intended language on a model whose
   default is not auto-detecting (e.g. German on `confucius4_tts`), without
   the user having to hand-write advanced JSON.
2. The gateway knows, per mapping, whether the model accepts a `language`
   request field and the default it declares — cached as a durable runtime
   fact, for both static (operator-run) and agent-managed audio.cpp.
3. A model that does not accept `language` is unaffected; a model with
   default `auto` keeps working as it does today.

## Non-goals / anti-goals

- **Not** an operator per-mapping language setting. The config surface is a
  per-thread composer control (decision below). An operator default could be a
  later convenience; it is out of scope here.
- **Not** validating the chosen language against a model's accepted set:
  audio.cpp does not publish that set, and it silently drops an unsupported
  value. We offer a free-text/combobox control, not a closed enum.
- **No** schema migration, and **no** change to the yes/no capability verdict
  wire (the language default is a string; it does not belong there).
- **Not** a language control for transcription — that composer already has a
  `Language` field (`ChatTranscriptionSettings.Language`); this adds the twin
  on the speech side only.

## Constraints

- Never commit to/merge into `main`; feature branch + PR; English repo-facing
  text; no LAN host names; push over SSH; local gates before pushing
  (per-module `golangci-lint` fmt+run, frontend prettier `format:check`,
  `make sonar-gate` + branch-findings).
- Agent/gateway negotiate by **named feature flags, never version numbers**
  (ADR-025). A new `agent.Features` entry requires a MINOR bump to
  `const Version` in `server-agent/internal/agent/agent.go`, **once** per
  branch. Base the bump on main's current version (0.12.0 → **0.13.0**) and
  merge `origin/main` before finalizing — serial audio PRs collide on this
  version and on the shared feature-name list.
- Keep the gateway's and the agent's duplicated audio.cpp model parsers
  byte-identical where they share a rule (the `audioCppModelCases` table and
  `TestAudioCppModelsCasesMatchTheAgentsTable` pin them). This change touches
  only the gateway's parser (see below), so the shared verdict table is
  unaffected — but the new language-param parse must keep the existing verdict
  parse unchanged.
- Portal i18n: add German + English keys together (`i18n.ts`, type-checked).

## The design

Two layers: a **fact cache** (so the gateway knows whether a model accepts
`language` and its default) and a **composer control + injection** (so the
user picks the language and the run sends it).

### Decision: config surface — per-thread composer control (option A)

The portal's speech knobs (`voice`, `speed`, `stream`) are per-thread
`ChatAudioSettings`, read exactly where the speech body is built
(`buildSpeechBody` reads `prep.Settings.Audio`), and the **transcription**
composer already has a per-thread `Language` field. So the speech language is
a per-thread `ChatAudioSettings.Language`, the twin of two existing patterns,
sitting right at the injection point. An operator per-mapping default was
considered and rejected for this change: it is heavier (new mapping setting +
admin UI + threading mapping config into the run, which `buildSpeechBody`
cannot see today) and it is the operator, not the synthesizing user, choosing
the language. (Confirmed with the requester.)

### Layer 1 — the fact cache (mirror the voices cache)

The gateway already caches per-mapping audio.cpp runtime facts in the
schema-free `model_mapping_cache` table (`voices`, `output_byte_rate`),
written by the health loop while the model is observed loaded and read at
request/compose time. The language param is the same kind of fact and uses the
same table with a new cache type.

**New cache type.** `ModelMappingCacheSpeechLanguage = "speech_language"`,
ScopeKey `""`, payload JSON `{"accepts":<bool>,"default":"<code>"}` where
`accepts` is "the model's `params` array declares a `language` entry" and
`default` is that entry's declared default (`""` if none). A reader helper
`ParseSpeechLanguage(payload) (accepts bool, def string, ok bool)` is the one
definition of "a usable cached language fact" (`ok==false` for an empty or
unparseable payload), shared by the composer gate and any future reader — the
`ParseOutputByteRate` precedent. No migration (the table exists since
migration 94).

**Cache contract (two reader states, voices-like — not three).** The reader
sees exactly: **no row** = unknown (never probed while loaded; the composer
treats it as "model default, no control"), or **a row** = a known fact
`{accepts, default}`, where `accepts:false` is a real "this model declares no
`language` param". There is no separate "retired/empty-payload" state: a fact
is simply overwritten by the next successful loaded read, and dropped wholesale
when the mapping's `AppModelName` changes (the existing cache-row invalidation
every `model_mapping_cache` row already gets). This mirrors the voices cache,
not the byte-rate cache (which needs empty-payload retirement only because a
rate is tied to a loaded-model identity that can change under the same name; a
`language` param is re-read on the next loaded tick, so it needs no such
mechanism).

**Gateway parser (static path + shared).** Extend
`provider/audiocpp_models.go`:

- Read the listing with `include_params=true` (the static path already GETs
  `/v1/models` every tick for verdicts; add the query to that same request).
- Add `AcceptsLanguage bool` and `LanguageDefault string` to
  `AudioCppVerdicts`, parsed by a new `parseAudioCppLanguageParam(body,
  modelID)` that finds the model's entry, scans its `params` array for the
  entry whose `name` is `language` (case/space-tolerant like the task/mode
  parse), and reads its `default`. This is read beside `Loaded` and `Path`
  from the same body — a new axis, independent of the verdict rule, so the
  pinned `audioCppModelCases` verdict table is untouched. It has its own test
  cases.

**Static path write.** In `cmd/gateway/app_health.go`'s `probeAudioCppModels`,
add a per-mapping step `cacheAudioCppSpeechParams` (beside
`cacheAudioCppVoices`/`cacheAudioCppSpeechRate`), gated by the **same rule as
voices** (`audioCppVoicesWanted`: the model is observed **loaded** and is a
**plain tts** model — `speech:yes` and not `speech_clone:yes`). On a loaded
plain-tts model, write the `speech_language` row from the just-read verdict
with a change-only write (the `writeVoicesCache` discipline); a failed read
leaves the last-known-good row and opens a back-off (a new `speechParamsBackoff`,
the `voicesBackoff` precedent), and a model that leaves the loaded/plain-tts set
clears its back-off. Loaded-gating matters for two reasons: audio.cpp may only
populate a model's `params` while it is loaded (unverified — see "What's left
to the builder"), so an ungated write could flap `accepts`; and it keeps the
static path symmetric with the agent path, which can read the fact **only**
while the child runs.

**Agent path.** Agent-managed audio.cpp children report verdicts via telemetry
(the agent reads the child's `/v1/models`), but the gateway cannot see their
`params` there — the language default is a string and the capability wire is
yes/no by design. Mirror the **voices** pass instead (`probeAgentVoices`): the
gateway pulls the fact through the agent router's Status-only `/upstream/
{model}/…` passthrough.

- **Agent change (minimal):** add `{path: "/v1/models", forwardQuery: true}`
  to `upstreamEndpoints` in `server-agent/internal/runtime/router.go`
  (`forwardQuery: true` so `?include_params=true` reaches the child), and a new
  feature flag `runtime_model_params` (Since **0.13.0**) in
  `internal/agent/features.go`, with the MINOR bump to `const Version`. The
  agent's collector, telemetry sample and ingest are **unchanged** — the agent
  only gains a route.
- **Gateway change:** add a `probeAgentSpeechParams` step (twin of
  `probeAgentVoices`), run over the same eligible set that pass uses
  (`agentVoiceProbeSpec`: running, audio.cpp runtime spec, stored rows are
  **plain tts** via `CapabilityRowsPlainTTS`) and only for a server whose agent
  declares `runtime_model_params` (fail-closed, the `runtime_voices`
  precedent). It GETs `/upstream/{model}/v1/models?include_params=true` through
  the agent (per-mapping spec credential via `SpecUpstreamAuth`), parses the
  language param with the **same** gateway parser, and writes the same
  `speech_language` cache row with the same change-only + leave-last-good +
  back-off policy as the static path. The gateway declares an **agent-only**
  mirror constant `RuntimeModelParamsFeature = "runtime_model_params"` in
  `agent_features.go` (NOT added to `gatewayAgentFeatures`, like
  `RuntimeVoicesFeature`), exported for the health loop's `hasAgentFeature`
  gate; a name-equality test pins it against the agent's spelling. This mirror
  constant is the "shared enum" the collision rule warns about.

Both paths share one gateway parser and one cache writer, so static and agent
cannot disagree, exactly as voices do.

### Layer 2 — composer control + injection

**Run injection.** Add `Language string \`json:"language,omitempty"\`` to
`ChatAudioSettings`, declared **last** (after `KeepLastAudio`) so existing
stored chat documents' bytes don't move. In `speechBodyFields`
(`chat_runs_audio.go`), after `voice`/`speed` and before the passthrough
merge, set `body["language"] = audio.Language` when non-empty. `language`
stays non-reserved, so an advanced passthrough `language` still works and the
existing first-class-wins collision handling (`hasKeyFold`) covers a thread
that sets both. Injection does not gate on `accepts` — audio.cpp drops an
unsupported value; the gate is a composer concern.

**Composer gate endpoint.** `GET /api/portal/model-speech-language?name=<model>`
→ `{"accepts":true,"default":"zh"}` or `{"accepts":false}`, reading the
`speech_language` cache for the model's portal-visible speech mappings. Same
no-leak discipline as `handlePortalModelSpeechStreaming` (every "no" is the
same 200; a store error is the only 500; nothing loads a model). The twin of
`GET /api/portal/model-voices` and `…/model-speech-streaming`.

**Frontend.** The speech composer gains a `Language` control
(combobox/free-text, any code accepted), shown when the gate says `accepts`.
De + en i18n, added together.

**Default behaviour (my call — minor, settle-able by the builder):** when the
gate says the model accepts `language` and its declared default is **not**
`auto`, the control pre-selects the portal UI language (de/en); otherwise it
starts blank ("model default"). The user can change or clear it. Rationale:
this directly fixes the reported "German → Chinese" case for `confucius4_tts`
while staying overridable, and we do not assume the model accepts the literal
`auto` value (the `params` array does not list accepted values). If the
builder finds a cleaner default while wiring the real data, that is a minor
call to settle in-place.

### Version / migrations

- ServerAgent `Version`: 0.12.0 → **0.13.0** (MINOR — one new feature flag),
  one bump for the branch. Base on main's current; merge `origin/main` first.
- Migrations: **none** (both the cache and the chat-settings field ride
  schema-free documents / existing tables).

## Verification

- Unit: gateway language-param parser (present/absent/blank default,
  non-speech model, malformed body); the `speech_language` cache write
  (change-only, clear-on-stale); `speechBodyFields` injection + precedence;
  the composer gate endpoint (no-leak, accepts/default, store-error 500);
  agent router allowlist (`/v1/models` relayed with query; 404 without the
  flag); the feature-registry guard (Since ≤ Version).
- Store: run the postgres leg (`OP_AI_GATEWAY_TEST_POSTGRES_DSN`) for the
  cache read/write, like the voices cache.
- Frontend: vitest for the control + gate wiring; prettier `format:check`.
- End-to-end (if an audio.cpp server is reachable): a `confucius4_tts` mapping
  with `language=de` yields correct German; a model without the field is
  unaffected; `qwen3_tts` unchanged. If unreachable, state so in the PR.

## What's left to the builder

- **Confirm against a live audio.cpp** (if reachable) whether a model's
  `params`/`language` default is reported for a listed-but-unloaded model or
  only while loaded. The design is correct either way because the write is
  loaded-gated, but the answer tells us whether the loaded-gate is strictly
  necessary or merely conservative; note the finding in the PR.
- Exact method/URL plumbing for the agent-path read (reuse
  `ProbeAudioCppModels` against the `/upstream/{model}` endpoint vs a small
  sibling; `endpointURL` query handling for `include_params=true`).
- Multi-mapping reconciliation in the composer gate (accepts/default when a
  gateway model has several speech mappings) — follow the fail-safe posture of
  the existing speech-streaming gate.
- The precise composer UX (where the control sits, combobox vs input) within
  the default-behaviour decision above.
- Architecture-doc updates (routing/agent-runtime cross-cutting + api-surface
  + openapi) folded in on the branch before the PR.

## Calls settled during the builder-check (for the reviewer)

A fresh builder-check read the design and raised three points; all were
refinements to match the established voices-cache pattern, none a product
decision, so they were settled here rather than reopened:

1. **Write is loaded-gated like voices** (not an ungated "any named speech
   model" write). Avoids an `accepts` flap if audio.cpp only reports `params`
   while loaded, and keeps the static and agent paths symmetric.
2. **Two reader states, not three** (no row = unknown; a row = a known
   `{accepts, default}`). Dropped the empty-payload "retire" wording; a fact is
   overwritten on the next loaded read and dropped on `AppModelName` rename.
3. **Plain-tts only** (loaded + `speech:yes` + not `speech_clone:yes`), the
   same restriction every sibling cache step and composer gate uses — a text
   speech chat is never routed to a clone model, so a clone row would be cached
   but never read.

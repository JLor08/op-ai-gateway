# Capability auto-detect from probe endpoints — design (issue #49, sub-project 2)

**Goal:** stop treating a model's capabilities as operator trivia. Where the
upstream *tells* us what a model can do — vision, video, audio, tools, and
speculative/MTP decoding — read it from the probe endpoint we already fetch,
persist it per mapping with three-state honesty, show it, and let an operator
override still win.

**Approved scope decisions (operator, 2026-09-08):** tri-state capability
columns outside the `metrics_locked` group, with a definitive vision verdict
additionally synced onto the existing `vision_capable` bool through its
existing lock-guarded writer; issue #54 (Ollama `/api/show` is POST-only) is
fixed as part of this work; delivered as three PRs (A: llama.cpp, B: Ollama +
#54, C: MTP via `/slots`). No routing change.

---

## 1. What upstreams actually expose (verified against source, 2026-09-08)

Every claim below was read out of the upstream's own code, not documentation
and not memory. Version boundaries matter: fielded deployments span years.

| Capability | llama.cpp | vLLM | Ollama | TGI |
| --- | --- | --- | --- | --- |
| vision | `GET /props` → `modalities.vision` (since 2025-05-09, PR #13393) | none | `POST /api/show` → `capabilities[]` (since v0.6.4) | none |
| video | `modalities.video` (since 2026-06-08, PR #24269) | none | none | none |
| audio | `modalities.audio` (since 2025-05-23, PR #13714) | none | `capabilities[]` (since v0.20.0) | none |
| tools | `chat_template_caps.supports_tools` (since 2026-01-22, PR #18994) | none | `capabilities[]` | none |
| other | — | — | `completion`, `embedding`, `thinking`, `insert`, `image` | — |
| MTP / speculative | `GET /slots` → per-slot `speculative` bool (default-on since 2025-08-31) | `/metrics` → `vllm:spec_decode_*` presence | none | none |

Three upstream facts shape the design and must survive into the code comments,
because each one is a trap:

1. **`modalities.video` is not a model fact.** `mtmd_helper_support_video()`
   returns `mtmd_support_vision()` under `#ifdef MTMD_VIDEO`, so `video: true`
   means "this binary was built with video support *and* the model has a vision
   encoder" — not "this model understands video". We persist it as what it is
   (the server accepts video input) and the portal tooltip says so.
2. **`/props` is useless for MTP.** `get_res_props` builds its probe task
   params as `tparams.sampling = params.sampling` and never copies
   `params.speculative`, so `default_generation_settings.params["speculative.types"]`
   reads `"none"` on every server regardless of configuration. Detecting MTP
   from `/props` would silently always answer "no". `/slots` carries the real
   per-slot `speculative` bool (`can_speculate()`, true for draft-model, MTP and
   ngram alike).
3. **`supports_tools: false` does not mean tool calls fail.** With
   `--jinja` (default on since 2025-11-27) llama.cpp accepts tools for every
   model via a generic handler. The flag is a *native-template quality* signal.
   The portal label and the docs must say "native tool template", never "tools
   unsupported".

### 1.1 vLLM: the gap, stated plainly

The operator asked specifically that every avenue for vLLM be checked. There
is no capability surface:

- `GET /v1/models`'s `ModelCard` carries `id`, `object`, `created`, `owned_by`,
  `root`, `parent`, `max_model_len`, `permission[]` — nothing about modality or
  tools.
- The tool-calling flags (`--enable-auto-tool-choice`, `--tool-call-parser`) are
  frontend argparse fields, absent from `VllmConfig`; the 400 that used to
  betray them (`"auto" tool choice requires ...`) was removed in v0.18.0, so
  even error-shape probing no longer works.
- `GET /server_info?config_format=json` *would* dump `multimodal_config` and
  `speculative_config.method`, but it exists only under
  `VLLM_SERVER_DEV_MODE=1` — not something a gateway may require of an
  operator's production server.
- `GET /openapi.json` reveals the *task class* (transcription ⇒ ASR,
  `/v1/embeddings` ⇒ embedding, `/classify` ⇒ classifier) because task-gated
  routers are only attached for tasks the model supports — but it cannot
  distinguish a vision LLM from a text LLM: both are just `generate`.
- `/metrics` `vllm:mm_cache_queries`/`_hits` are registered unconditionally
  with eagerly-created zero-valued series, so their *presence* proves nothing;
  only a non-zero value after real multimodal traffic implies anything.

**Consequence for this design:** vLLM (and TGI) get **no** capability probe.
Their coverage stays what it already is — the existing vision *benchmark*
(an active image request, accept/verify modes), which works against any
OpenAI-compatible upstream. This is written into the docs as a known gap with
the reason, so the next reader does not re-litigate it. vLLM's
`vllm:spec_decode_*` presence *is* a real MTP signal, but it lives in a metrics
scrape and therefore belongs to sub-project 3, not here.

---

## 2. Schema: tri-state, outside the metrics group

### 2.1 The decision

The repo carries two contradictory precedents. `vision_capable` is a **bool
inside** the `metrics_locked` group, written with `metrics_source = 'vision'`.
`live_progress_support` is a **tri-state string outside** it, with its own
`checked_at` column, no lock guard, and a doc comment arguing at length that a
build capability is not a metric an operator pins numbers against — the
operator's own ruling on #52 ("außerhalb von metrics_locked. Da es eine
Funktion ist und keine Metrik").

New capability columns follow the **`live_progress_support` precedent**, for two
reasons beyond consistency:

- A bool cannot express "not probed yet". `vision_capable = false` today
  conflates *unknown* with *no*, and the models list AND-aggregates it
  fail-closed — so one unprobed mapping silently disables image attachment for
  a whole model. The repo's own "unknown must never overwrite a stored verdict"
  discipline is only expressible with three states.
- `metrics_source` is one mapping-wide string. A vision write stamps `'vision'`
  over a prior `'benchmark'`, so per-capability provenance ("an operator
  override wins *for this capability*") cannot be expressed there at all.
  Adding four more auto-detected flags to that shared column amplifies the
  clobbering.

### 2.2 Migration 77 — `model_mappings_capabilities`

Six columns, each `text not null default ''`, plus one nullable timestamp:

| Column | Values | Meaning |
| --- | --- | --- |
| `cap_vision` | `''` \| `yes` \| `no` | the server accepts image input for this model |
| `cap_video` | `''` \| `yes` \| `no` | the server accepts video input (build + vision fact, see §1) |
| `cap_audio` | `''` \| `yes` \| `no` | the server accepts audio input |
| `cap_tools` | `''` \| `yes` \| `no` | the model's chat template natively supports tool calls |
| `cap_extra` | `''` \| JSON array of strings | capabilities the upstream named that we have no column for (Ollama's open vocabulary: `thinking`, `insert`, `embedding`, `image`, …) |
| `capabilities_source` | `''` \| `llama_cpp_props` \| `ollama_show` | which probe produced the current verdicts (per-capability-group provenance, deliberately NOT `metrics_source`) |
| `capabilities_checked_at` | nullable `dl.timestampType()` | diagnostics and tooltip only — **no decision logic reads it** (the `live_progress_checked_at` precedent) |

`''` is unknown and **never overwrites** a stored verdict, in either
direction. `cap_extra` exists because Ollama's capability vocabulary is
open-ended (manifest-declared capabilities pass through verbatim upstream, which
is how `image` arrived) — binding an enum would silently drop future values, so
unknown strings are preserved verbatim for display instead.

Follows the established migration shape: forward-only entry appended in
`migrate.go` (next free version is 77), columns added via `addColumnIfMissing`,
timestamp via `dl.timestampType()`. No backfill.

### 2.3 The `vision_capable` sync

A **definitive** `cap_vision` verdict additionally writes the existing
`vision_capable` bool through the existing `UpdateMappingVisionCapable`
writer — the one whose SQL carries `and metrics_locked = 0`:

- `cap_vision = 'yes'` → `UpdateMappingVisionCapable(id, true, now)`
- `cap_vision = 'no'` → `UpdateMappingVisionCapable(id, false, now)`
- `cap_vision = ''` → no call at all

That writer stamps `metrics_source = 'vision'`, which is correct and desirable
here: it is the *existing* provenance value for "something probed vision", and
an operator lock atomically no-ops it. The point of the sync is that the
existing consumers — the models-list `vision` flag and the portal chat's image
gate — start working from probe evidence with **zero changes to those
consumers**. The new tri-state column is the honest record; the bool is the
compatibility surface.

Both writes obey compare-to-stored: the bool is only written when it differs
from the mapping's stored `vision_capable`.

---

## 3. PR A — llama.cpp modalities and tools via `/props`

### 3.1 The detector (duplicated on purpose, like its siblings)

```go
// Capabilities is the parsed capability verdict set from one probe document.
// Zero value = nothing determined; every field is ""/"yes"/"no".
type Capabilities struct {
    Vision string
    Video  string
    Audio  string
    Tools  string
    Extra  []string
}
```

`detectCapabilities(body []byte) Capabilities`, byte-identical in both
modules (`server-agent/internal/collector/probe.go` and
`gateway/backend/internal/provider/model_info.go`), carrying the same
"DUPLICATED locally on purpose / must never drift" doc comment the
`detectLiveProgressSupport` twins carry, and the same **`"role":"router"`
gate**: a router-mode `/props` stub describes the router's own build, and under
the never-rewrite discipline reading it as the child's evidence would be a
permanent, self-reinforcing wrong verdict (#55).

Evidence rule, deliberately narrow:

- Only a llama.cpp `/props` document is evidence. `role == "router"` → zero
  value, untouched.
- `modalities` object present: each of `vision`/`video`/`audio` present as a
  bool → `yes`/`no`; a key *absent* from a present `modalities` object → `''`
  (an older server that predates that key must not be read as "no").
- `chat_template_caps.supports_tools` present as a bool → `Tools` `yes`/`no`;
  absent (server older than 2026-01-22) → `''`.
- No `modalities` and no `chat_template_caps` → zero value.

`Extra` stays empty on this path (llama.cpp has no open vocabulary).

### 3.2 One `/props` fetch, not three

Today `probeRuntimeChild` issues **separate** `fetchProbeBody` GETs for the
context probe and the live-progress probe — two `/props` requests per uncached
cycle for a llama_cpp child. Adding a third naive probe would make it three.

Instead, `ProbeLiveProgressSupport` becomes `ProbePropsVerdicts`:

```go
// ProbePropsVerdicts fetches /props ONCE and returns every verdict the
// document carries.
func ProbePropsVerdicts(ctx context.Context, client *http.Client, baseURL string) (PropsVerdicts, bool)

type PropsVerdicts struct {
    LiveProgress string       // "", "supported", "unsupported" — unchanged semantics
    Caps         Capabilities
}
```

The `(verdict, stable bool)` contract, the conclusive-status set
`{404, 401, 403, 405}`, the transient/retry semantics, and the invalid-JSON
handling are **carried over unchanged** — only the payload widens. The
`runtimeCapabilityCache` entry widens from `{pid int; verdict string}` to
`{pid int; verdicts PropsVerdicts}`, keeping its keying (by `SpecID`, with the
PID checked on lookup — note: *not* a composite key) and its rule that a stable
outcome is cached including an all-empty one.

This is a behavioural-contract change to a documented regression anchor, so the
existing live-progress tests must keep passing unchanged in substance, and the
task carries an explicit instruction that their assertions may not be weakened.

### 3.3 Wire, ingest, write-back

- `sample.RuntimeSample` grows `Capabilities *SampleCapabilities json:"capabilities,omitempty"`
  (a pointer/omitempty so an older agent's absent field is unambiguous), with
  `SampleCapabilities{Vision, Video, Audio, Tools string; Extra []string}`.
  The gateway mirror `agentRuntimeSample` grows the identical shape.
- `writeBackRuntimeCapabilities` in `agent_ingest.go` mirrors
  `writeBackRuntimeLiveProgress` exactly: gated on this sample's own
  `runtime_model_probe` capability set (never a re-read of the shared
  registry), `maxRuntimeSamplesPerSample` cap, ownership memoized per distinct
  `spec_id` with the cross-server `slog.Warn` rejection, compare-to-stored,
  `''` skips, best-effort (never rejects the sample), **no `metrics_locked`
  guard** on the new columns — and the `vision_capable` sync of §2.3 layered on
  top through its own lock-guarded writer.
- Gateway pass: `provider.ModelInfo` grows `Caps Capabilities`; `parseModelInfo`
  fills it via the detector; a new `PickModelCapabilities(infos, model)` mirrors
  `PickModelLiveProgressSupport` (exact name match wins, else first non-empty).
  The `{model}` branch of `app_health.go` grows one capabilities write beside
  its existing context and live-progress writes; the single-probe branch grows
  the equivalent (nameless document ⇒ every mapping of the app, the existing
  rule). Both reuse the #58 machinery unchanged: implicit
  `/upstream/{model}/props` path, `runtime_upstream_props` feature gate,
  per-mapping `SpecUpstreamAuth`.
- New store writer `UpdateMappingCapabilities(ctx, id string, caps CapabilityVerdicts, at time.Time) error`,
  writing only the six new columns; no lock guard; never touches
  `metrics_source`/`metrics_updated_at`. It writes each column
  **only when the incoming verdict is non-empty**, so a partial verdict set
  (an older llama.cpp that has `modalities` but no `chat_template_caps`) cannot
  clear a capability another probe already established.

### 3.4 Portal

The Models views get one **Fähigkeiten** column (i18n `capabilitiesColumn`;
en `Capabilities`), rendering one chip per `yes` capability
(`Vision`/`Video`/`Audio`/`Tools`, plus `cap_extra` values verbatim), an
em-dash when every verdict is `''`, and nothing for `no` (a chip per negative
would be noise). Chips are keyed by `data-status`, never by colour — the house
rule. The tooltip carries the provenance (`capabilities_source`), the
`capabilities_checked_at` timestamp, and the two upstream caveats from §1
(video = build + vision; tools = native template quality, not availability).
Column-scoped test queries via the `cellForColumn` helper — a positional index
provably false-passes here because neighbouring columns render the same
em-dash.

---

## 4. PR B — Ollama capabilities, and the #54 fix

### 4.1 #54's fix must be the POST fix

Issue #54 records that the agent's Ollama context probe GETs `/api/show`,
which upstream registers **POST-only** (with gin's
`HandleMethodNotAllowed = true`, so a GET is a 405 on every version) — the
probe has never once succeeded. #54 listed switching to `GET /api/ps` as an
alternative; **that option is now foreclosed**: `/api/ps` does not carry the
`capabilities` array. Fixing #54 as a POST is a precondition for Ollama
capability detection, which is why the two land together.

This introduces the first **POST** probe in `fetchProbeBody`, which is
`http.MethodGet`-hardcoded today. The change is a new sibling
`fetchProbeBodyPOST(ctx, client, baseURL, path string, body []byte)` rather
than a method parameter on the existing function, so every existing caller's
shape is untouched and the GET-only guarantee of the live-progress and metrics
probes stays visible in the code. Body is `{"model": "<upstream model>"}`.

Verified safe: `ShowHandler` → `GetModelInfo` reads the manifest and decodes
GGUF metadata from disk (`os.Open` + `ggml.Decode`) and **never** calls the
scheduler — so it cannot start a model, satisfying the never-start discipline
#58 established. Current Ollama additionally caches show responses per manifest
digest when the request carries no `System`/`Options` overrides (ours carries
neither).

### 4.2 The detector

`detectOllamaCapabilities(body []byte) Capabilities` (again duplicated
byte-identically across the two modules, same doc discipline):

- `capabilities[]` present → map the known names onto columns
  (`vision`→Vision, `audio`→Audio, `tools`→Tools) as `yes`; every **other**
  string goes into `Extra` verbatim, unknown values included, because the
  vocabulary is open-ended upstream.
- A present-but-absent name is `no` **only** for the three mapped columns, and
  only because the array is exhaustive by construction upstream (it is derived,
  not declared). `Video` stays `''` — Ollama has no video capability at all,
  and `no` would falsely imply it was assessed.
- `capabilities[]` absent (server older than v0.6.4) → fall back to the
  `model_info` map: keys are architecture-prefixed, so match by **suffix**
  (`.vision.block_count` → Vision `yes`, `.audio.block_count` → Audio `yes`);
  absent keys stay `''`, never `no`, because a suffix miss is not evidence.
- Not an `/api/show` document → zero value.

### 4.3 Scope of the Ollama path

Agent-side only, on the existing per-type context-probe dispatch (the gateway's
`parseModelInfo` is llama.cpp-`/props`-shaped and has no runtime-type
plumbing — teaching it per-type dispatch is a structural change this
sub-project does not need, and `server_agent`-managed Ollama children are
covered by the agent's own probe). The same one-fetch discipline as §3.2
applies: the Ollama context probe and the capability parse share one POST.

---

## 5. PR C — MTP via `/slots`

### 5.1 Widening the `/upstream` allowlist

The #58 route's allowlist is exactly `/props`, and both its file header and
ADR-037 name this issue as the owner of any widening. PR C adds `/slots` as a
**second allowed suffix**:

- The suffix check becomes a small allowlist (`upstreamPropsSuffix`,
  `upstreamSlotsSuffix`) and the outbound path becomes the matched suffix
  rather than the hardcoded `/props` — everything else about
  `serveUpstreamProps` is unchanged, including `Status()`-only resolution, the
  never-start guarantee, the verbatim relay, and the four error outcomes. The
  handler and the refusal message are renamed accordingly
  (`serveUpstreamEndpoint`, "only /props and /slots may be requested through
  /upstream/{model}").
- **Privacy gate, load-bearing:** `/slots` can carry prompt and generated text
  — upstream only omits them unless `LLAMA_SERVER_SLOTS_DEBUG` is set, and the
  relay is byte-verbatim. The gateway's parse therefore reads **only** the
  `speculative` bool per slot and never stores or logs any other slot field;
  the same reasoning `maxRuntimeStderrTail` documents for stderr prompt
  fragments applies verbatim and is cited in the code.

### 5.2 The detector and the write

`detectSpeculative(body []byte) string` → `yes` if **any** slot object has
`speculative: true`, `no` if the document is a well-formed slot array where
none does, `''` otherwise (not a slots document, empty array, `/slots`
disabled → 404 which is a probe error, never a verdict).

The verdict writes `is_mtp` — and unlike the §2 columns, `is_mtp` is
**routing-affecting** (a flat `+30` in the scorer's bounded tiebreak). It
therefore goes through a **lock-guarded** writer
(`UpdateMappingIsMTP`, new, with `and metrics_locked = 0` and
`metrics_source = 'probe'`), matching how every other routing-affecting probed
value behaves. Rationale in one line, in the code: an operator who pins metrics
is pinning the numbers routing acts on; a capability that only *displays*
(§2) is not.

The name heuristic `IsMTPModelName` stays as the creation-time seed. Probe
evidence beats the name heuristic; an operator lock beats both.

`/slots` is a per-process fact, not per-model — for a single-model
`llama-server` that is the same thing, and for a router-mode parent the
`role: "router"` gate on the `/props` sibling already keeps us from attributing
a parent's document to a child. A `/slots` document from a router parent yields
`''` because it carries no slot array.

---

## 6. Explicitly out of scope

- **No routing change.** Modality never enters candidate filtering or scoring
  in this work. Today a vision request to a non-vision model routes normally and
  fails at the upstream (a generic 502 `provider.unavailable` on the translate
  path, the upstream's own body on native passthrough). Making modality a hard
  filter would be the first hard capability filter in a chain whose every
  existing soft filter fails *open* — a real design decision, deliberately left
  to its own issue.
- **No vLLM/TGI capability probe** (§1.1) — documented as a known gap with the
  reason, so it is not re-litigated.
- **vLLM MTP via `/metrics`** — real signal, belongs to sub-project 3.
- `EnergyWhPerToken`, benchmark fields, and the vision *benchmark* are all
  untouched.

## 7. Regression anchors

Unchanged in substance, and each named in the plan as an anchor:

- `ProbeLiveProgressSupport`'s conclusive set `{404, 401, 403, 405}`, its
  transient/retry rule, and its stable-`''` caching — carried into
  `ProbePropsVerdicts` with only the payload widened.
- `runtimeCapabilityCache` keying (by `SpecID`, PID checked on lookup) and
  PID-change re-arm.
- `writeBackRuntimeLiveProgress` and `writeBackRuntimeContext`: `''` skip,
  compare-to-stored, per-sample capability gate, cross-server `Warn`, and the
  deliberate absence of `metrics_locked` on the live-progress column.
- `UpdateMappingLiveProgressSupport`'s SQL; `UpdateMappingVisionCapable`'s
  lock guard.
- The `/upstream` route's `Status()`-only resolution, never-start guarantee,
  verbatim relay and error codes.
- The detector duplication discipline: every new detector exists twice,
  byte-identically, or not at all.
- The `"role":"router"` gate, on every `/props`-derived detector.

## 8. Verification themes

Beyond per-task tests, three properties get explicit adversarial coverage
because they are where this design can silently rot:

1. **`''` never clears.** For every new column and every detector: a probe
   that determines nothing must leave a stored verdict byte-identical. Includes
   the partial-verdict case (a server with `modalities` but no
   `chat_template_caps` must not clear `cap_tools`).
2. **One fetch per document.** Numeric hit-count assertions on the child's
   `/props` (and Ollama's `/api/show`) proving the per-cycle request count did
   not grow — the whole point of §3.2.
3. **Upstream traps encoded as tests.** A router-mode `/props` yields nothing
   (all detectors); a `/props` whose `speculative.types` says `"none"` while
   `/slots` says `speculative: true` yields `is_mtp = yes` (the §1.2 trap,
   which a `/props`-based implementation would fail); an Ollama document with
   an unknown capability string preserves it in `cap_extra` rather than
   dropping it.

# Design — `POST /v1/audio/transcriptions` backed by audio.cpp (issue #69)

Status: approved rulings, design for implementation.
Branch: `worktree-issue-69-transcriptions` (base `origin/main` @ ae41f81,
includes #241 speech and #242 run-as).

## 1. Intent and why

The gateway serves five OpenAI-compatible inference endpoints plus
`/v1/images/edits` (multipart-IN) and `/v1/audio/speech` (JSON-in, binary-out).
Issue #69 asks for the **last** of the originally-paired multipart endpoints:
`POST /v1/audio/transcriptions` (speech-to-text), backed by audio.cpp. The
images-edit half of #69 already shipped (#218); only transcriptions remains.

Transcription is the mirror image of speech on two axes at once:

- **Like speech** (#68/ADR-061): it routes through the capability gate, uses the
  coarse `openai_audio` flavor and the static `audio_cpp` application type, bills
  in the non-token `audio_second` unit, and takes no affinity pin.
- **Like images-edit** (#218): its request is `multipart/form-data` (an audio
  file upload), so it rides the multipart request path — a model peeked from the
  form, a bounded-buffered body, a model rewrite when the provider model differs.

It is **unlike both** in one decisive way settled by probing the live server:
the billable quantity (input audio duration) is in the **JSON response body**,
not a header. So transcription is the first endpoint whose relay must parse a
(small) JSON response to meter the request.

The "why now": #69's prerequisites (#70 billing-unit pair, #71 capability gate,
#218 multipart path, #68 audio_cpp foundation) are all merged. Transcription is
unblocked and reuses almost all of them.

## 2. Live-server facts (probed 2026-10-16 against audio.cpp v0.9.1 CUDA)

Loaded `vibevoice_asr_f16` (task `asr`), transcribed a ~6 s WAV, then unloaded.

1. `GET /v1/models` lists each model with `task` (`asr` for transcription),
   `family`, `mode`, `loaded`, `path`. An **unloaded** asr model still appears
   with `loaded:false`.
2. A success returns `application/json`:
   `{"text":"…","timing":{"wall_ms":…,"audio_duration_ms":5961.94,"rtf":…}}`.
   `audio_duration_ms` is the input audio duration. **There is no `X-AudioCPP-*`
   header** on this endpoint (unlike speech).
3. audio.cpp **ignores `response_format`**: `json`, `verbose_json`, and `text`
   all return the identical `{"text","timing"}` JSON. It does not produce
   OpenAI's `verbose_json` (no `segments`/`language`) or plain text.
4. Errors are OpenAI error **objects** but with **HTTP 500** and
   `type:"server_error"` even for client errors (missing file, unknown model).
5. Warm RTF ~0.15–0.2; the first call after load pays model warmup (~7.8 s).

## 3. Goals / non-goals / anti-goals

**Goals**
- Serve `POST /v1/audio/transcriptions` and `/openai/v1/audio/transcriptions`,
  multipart audio in, JSON out, through audio.cpp.
- Gate on a new `transcription` capability, written by the gateway health loop
  from audio.cpp's `/v1/models` `task=="asr"`, refusing a non-capable model on
  every resolve branch (affinity included).
- Bill in `audio_second`, quantity from the response body's
  `timing.audio_duration_ms`, on every usage row including failures.
- Mirror #68's operator-portal + frontend + ADR + doc surface + a local e2e
  suite, every task through the review loop, all gates green, PR with Auto-fix.

**Non-goals (v1, deferred with a stated way back)**
- `stream=true` / streamed transcription.
- `response_format` other than `json`/absent (verbose_json, text, srt, vtt).
- JSON-bodied transcription requests (audio.cpp accepts them; OpenAI's contract
  is multipart-only, so v1 is multipart-only).
- An agent-managed automatic transcription verdict (the agent router's
  `/v1/models` carries no `task`; operator `transcription: yes` serves it
  meanwhile — same deferral speech took).
- A portal audio composer / transcription UI (API-only).
- Hosted/vendor-account transcription (vendor accounts reject any
  capability-carrying request).
- A streaming (non-buffering) upload relay — bounded buffering is accepted, as
  ADR-057(j) accepted it for images-edit.

**Anti-goals (would make this a failure even if it "works")**
- Translating request or response shapes (the project proxies natively-shaped
  backends; it does not translate). The `timing` block is passed through.
- A second `admitPrincipal` call site, or any path that skips `inferencePreflight`.
- A usage row that mislabels a transcription as a chat completion, or that bills
  tokens for a non-token request.
- Reusing `CapabilityAudio` (input modality) for transcription.
- A capability gate that misses the affinity / server-override branch.

## 4. Constraints

- AGENTS.md: feature branch + PR, squash merge, repo text in English, no local
  host names, no attribution lines; `docs/superpowers/**` and
  `docs/implementation-status.md` removed before the PR; architecture docs
  updated in the same branch.
- Preserve stable error codes; both auth modes stay supported.
- Keep all three store drivers working (no schema change is expected here —
  `audio_second`, `billing_unit`/`billing_quantity`, and the capability
  vocabulary already exist).
- Keep compat mapping isolated; the native inference path stays provider-neutral.
- No ServerAgent version bump (the generic `multipart_proxy` feature, agent
  0.9.0, already routes any multipart body by its `model` form part).

## 5. Decisions (the "how", as decided)

Numbered to the issue's nine open questions; all confirmed with the user.

**D1 — Upstream: the static `audio_cpp` application type, reused as-is.**
audio.cpp is natively OpenAI-shaped; `ProviderAudioCpp` already dispatches to the
shared OpenAI-compatible client, health via `/health`, 600 s total timeout. No
new application type, no transport change. The agent-managed `audio_cpp` child
works through the existing `multipart_proxy` router with a manual verdict; its
automatic verdict stays deferred.

**D2 — Flavor: reuse `openai_audio`.** Transcription is audio too; it needs no
new flavor. The audio-only served-flavor gating (`FlavorsAreAudioOnly`) already
keeps an audio server off chat/warmer/benchmark traffic. Consequence: the flavor
string is identical for speech and transcription, so **behavior that differs
between the two is keyed on the `endpoint`, never on the flavor string** (see D-arch).

**D3 — Billing quantity: the response body's `timing.audio_duration_ms` / 1000.**
Set only on a clean success (full delivery, `status == "success"`, mirroring
speech and ADR-041); a mid-write client disconnect records `0` as speech does. A
success with no usable duration (absent, non-numeric, ≤0, NaN/Inf) records `0`
and logs once at Error (ADR-041: the pair has no "unknown", so the gap is made
loud, not repaired). Unit is endpoint identity → `audio_second` on every
recordUsage site including refusals/failures; tokens and both rates stay 0
(`ValidateBillingXOR`).

**D3a — Response buffering bound (settled from the builder-check).** The metered
`timing` block is **trailing** in the JSON (`{"text":…,"timing":{…}}`), so the
whole response must be buffered to reach it, and a transcript must never be
truncated. The relay buffers the full response up to a **generous** bound — the
transcription body cap (`OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES`,
default 32 MiB, orders of magnitude above any real transcript: a 32 MiB audio
upload transcribes to at most a few MB of text) — parses `timing`, then writes
the bytes through verbatim. A response that **exceeds** the bound is treated as an
upstream error (`transcription.upstream_error`), **never a silently-truncated
success billed at 0**. Do **not** reuse the 1 MiB `captureMaxBytes` on the
success path (speech's error path uses it for error bodies only; a transcript is
not an error body). This is the one place transcription cannot stream the way
speech streams its binary output — it is forced to hold the (small) JSON output to
meter it; the memory profile under concurrency is bounded by this cap.

**D4 — Upload cap: a dedicated `OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES`
(default 32 MiB).** Full wiring mirroring `MaxImageEditBodyBytes`, with a wiring
test. A dedicated var (not sharing the image cap) lets operators size audio
independently; 32 MiB matches the agent-router cap. The upload is bounded-buffered
(not streamed); the small JSON response is fully buffered to read the duration.

**D5 — Streaming: refuse `stream=true` (and any `stream_format`) → 400
`transcription.stream_unsupported`.** The relay's buffering and billing contract
is one complete response; the router's multipart branch is plain-relay with no
heartbeat.

**D6 — Auth: `requireInternalOrBearerAnyScope(scopeGatewayUse, scopeLLMInvoke)`**
— a bearer token or the internal loopback pair, never the session cookie, with
the shared `runAsPrincipal` block. Mirrors images-edit and speech.

**D7 — Error normalisation: yes, gated on the transcription endpoint + non-2xx.**
An OpenAI error object passes through byte-for-byte (keep audio.cpp's attested
`type`); a plain `{"error":"str"}` is wrapped as `type:"upstream_error"`,
`code:"transcription.upstream_error"`; anything else relays unchanged. Upstream
status relays as it came; a failed relay records an `audio_second` row, quantity
0. (The live server's 500-for-client-errors makes this worth having.)

**D8 — Verdict writer: the gateway health loop, from `task=="asr"`.** Extend
`AudioCppVerdicts` with a `Transcription` field and `parseAudioCppModels`:
`asr → {Transcription: yes, Speech: no}`; `tts`/`clone → {Transcription: no,
Speech: yes}`; other non-empty task → both `no`; empty/absent/no-match → write
nothing. `audioCppCapabilityRows` emits the extra `transcription` row (source
`audiocpp_models`). Reserve `(transcription, no)` in `reservedManualVerdicts`
(so a manual `no` can't permanently veto the probe); leave it OFF
`reservedAgentCapabilityNames` (an agent may report it later). The prober decodes
`task` only (not `loaded`), matching speech — loadedness is the health axis, not
the capability axis; an operator loads the mapped model.

**D9 — Response-format policy (A): accept `response_format` ∈ {`json`, absent}
only; every other value → 400 `transcription.response_format_unsupported`.**
Rationale: audio.cpp only ever produces the `{"text","timing"}` JSON shape, so
accepting `text`/`srt`/`vtt`/`verbose_json` would promise a shape we do not
serve and, for the non-JSON ones, would bill zero. This is the tight mirror of
speech's WAV-only surface. Deferred formats have a way back (a real verbose_json
shape, or honoring text/srt/vtt, needs a translation this project does not do
yet). The `timing` extension block is **passed through** unchanged (proxy, don't
translate; OpenAI clients ignore unknown fields).

**D-portal — API-only, mirror #68.** No new application type/flavor/timeout.
Add a `transcription` capability chip (frontend `KNOWN_CAPABILITY_ORDER` +
`capabilityTranscription` i18n in DE/EN), so a `transcription` verdict renders a
proper chip instead of a raw standby string. The live Active-Requests billing
unit is already generic (closed by #68); confirm, don't redo.

**D-arch — endpoint-keyed, not flavor-keyed.** Because speech and transcription
share the `openai_audio` flavor string, the per-behavior hooks that speech keyed
on `apiFlavor == apiFlavorSpeech` must, for transcription, be keyed on
`endpoint == endpointTranscription`: `upstreamPath` (returns
`/v1/audio/transcriptions`), the billing-quantity source (body vs header), and
the error-relay gate. `billingUnitFor`/`newUsageScanner` already behave correctly
for any `openai_audio` request (audio_second; nil scanner), so they need no
transcription-specific case beyond what the shared flavor already gives — to be
confirmed in implementation and pinned by a test.

## 6. Shape of the implementation (left to the builder, with the seams named)

Base tree is the worktree on `origin/main` @ ae41f81. All file:line references
are origin/main numbers — re-verify in the worktree. Full seam-by-seam map:
`side-session-notes/.../issue-69-transcriptions-implementation-map.md`.

**Request path (twin of images-edit, `images_edit_handler.go`)**: a new
`transcription_handler.go` with `readTranscriptionBody` (bounded, mirror
`readImageEditBody`), `parseTranscriptionForm` (mime/multipart reader; `model`
single-use; exactly one non-empty `file`; optional `language`/`prompt`/
`response_format`/`temperature`; whole body walked; model read identically to the
agent router's `peekMultipartModel` so the allowlist and the child agree),
`validateTranscriptionRequest` (before preflight), a model-rewrite rebuild
(mirror `rebuildImageEditBody`, `model` field only), and the agent-served gate
(mirror `imageEditTargetServed` on `multipart_proxy`, fail-closed).

**Gate/flavor/billing/health (twin of speech, `speech_handler.go`)**:
`CapabilityTranscription`, `endpointTranscription` (session_extract enum + both
switches, no-signal cases), `upstreamPath` branch, the response-body duration
parser + `setTranscriptionBillingQuantity`, the error-relay pair, the extended
prober + health projector, the reserved manual verdict.

**Relay**: multipart-IN means the JSON model-rewrite in `proxyNative` is skipped
(non-JSON content type); the handler rebuilds the body itself. JSON-OUT means the
relay must read the small response body to meter it — add a response-body hook
keyed on `endpointTranscription` that buffers (bounded) the body, parses
`timing.audio_duration_ms`, and still writes the bytes through verbatim.

**Mock provider**: add a `/v1/audio/transcriptions` case returning
`{"text":…,"timing":{"audio_duration_ms":N}}` so dev/e2e don't get an SSE stream.

**Server-agent router**: no code change; add a `router_multipart_test` case that
POSTs multipart to `/v1/audio/transcriptions` and asserts same path/Content-Type/
body routed by the form `model`.

**Tests (per the #54 lesson — never discard `*http.Request`)**: a recording
upstream pinning method + verbatim Content-Type (incl. boundary); not-capable
refusal (distinct code); affinity-pin-not-honored; agent-without-multipart_proxy
refusal; usage-row (unit `audio_second`, zero token cols, path, quantity from the
body); validation-before-admission; capture (file redacted, text fields kept);
body-cap wiring; the duration-parse edge cases (absent/NaN/≤0 → loud zero).

**Docs**: ADR-062 (mirror ADR-061, record the differences); new
compatibility §3.7; api-surface route table + the internal-or-bearer sentence;
openapi.yaml; routing/telemetry/agent-runtime/glossary/risks §11.4 sweeps; fix
the stale "only images carries a required capability" / "no producer writes a
non-empty unit" comments; README if user-visible.

**e2e**: local-only `e2e:transcription` mirroring `e2e-speech` (Node http stub
with a `task:"asr"` model and a multipart transcription route), catalog line.

## 7. Verification gates (all must pass, stated in the PR if skipped)

`make lint`, `make lint-docs`, `make test-go`, frontend `npm test` / `npm run
build` / prettier `format:check`, `make test-e2e` + the new `e2e:transcription`,
the Postgres leg (`OP_AI_GATEWAY_TEST_POSTGRES_DSN`), and the Sonar
branch-findings gate (judge by branch-attributed/new-code findings). `npm audit`
is red on main already (pre-existing); state it, don't fix it here.

## 8. Settled calls and flags for the builder

Calls made by the author (settle in the ledger, confirm at whole-branch review):

- **Response buffer bound** (D3a): buffer fully to the 32 MiB transcription cap,
  error over it, never truncate. Settled.
- **Billing gate** (D3): on full successful delivery, mirroring speech; a
  mid-write disconnect bills 0. Settled.
- **Capture** (#3): the transcript response is output-sensitive but captured like
  any response body under the opt-in; no transcription-specific redaction; the
  security doc records the output-sensitivity. Settled.
- **No distinct flavor string**: the handler uses `routing.APIFlavorOpenAIAudio`
  and keys every divergence (upstreamPath, billing-quantity source, error relay)
  on `endpoint == endpointTranscription`, since the flavor string equals speech's.
  Settled; pinned by a test that speech and transcription do not collide.

Flagged for the whole-branch review (verify, don't assume):

- The prober treats `loaded:false` as not-unknown (verdict from `task` alone,
  mirroring speech). The live server lists unloaded asr models with `task:"asr"`
  and `loaded:false`; confirm the review agrees an operator-loaded model is the
  intended contract and a stale `(transcription, yes)` for an unloaded model is
  acceptable (the health axis, not the capability axis, governs loadedness).
- That no `apiFlavor == apiFlavorSpeech` guard anywhere in `proxyNative` /
  billing / usage-scan silently also fires for transcription in a way that is
  wrong (the shared-flavor hazard). Every such guard must be audited.

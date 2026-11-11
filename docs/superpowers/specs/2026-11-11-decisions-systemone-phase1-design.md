# Decision endpoints, Phase 1: `/v1/systemone` passthrough, `decisions` flavor, `decision` capability

Branch-local working document (removed before the PR). Source issue: JLor08/op-ai-gateway#373. That issue is the
researched design for all four phases; this spec fixes Phase 1 only and records where Phase 1 deviates from it.

## Goal

A client can send a System One request (`POST /v1/systemone`) to the gateway, with a bearer token, and have it relayed
**verbatim** to a decision model served by llama.cpp (`llama-server` ≥ b11526), vLLM (≥ 0.33.0, e.g. `vllm-sr/d3-mini`
under the existing `vllm` runtime spec), or any other upstream that serves `/v1/systemone`. The relay is admitted,
quota-checked, token-metered, captured and listed like every other relay. Decision models never receive a chat prompt
from the gateway, and they never appear as chat models.

## Scope

In scope:

1. **The coarse flavor `decisions`** (`routing.APIFlavorDecisions`). It is opt-in like `openai_images`/`openai_audio`:
   never part of the empty-list default, accepted by application and runtime-spec flavor validation, and listed in
   `knownAPIFlavors`. The `/v1/systemone` handler uses it as its flavor (as speech uses `openai_audio`). Phase 2's
   `/v1/decisions` shares it, distinguished by its `sessionEndpoint`, as the images edits endpoint is today.
2. **The decisions-only rule.** `routing.FlavorsAreDecisionsOnly(flavors)` is true when the list names `decisions` and
   neither text flavor (`openai`, `anthropic`). It is the third twin of `FlavorsAreImagesOnly`/`FlavorsAreAudioOnly`,
   and every caller of those gets its decisions twin:
   - the `openai` row of `effectiveHasAPIFlavor`, which keeps the model out of `/v1/models`, `/api/v1/models` and the
     chat listing;
   - the text translate dispatch (404 `routing.no_model_route`);
   - the model warmer;
   - manual and scheduled benchmarks (new code `benchmark.decisions_only`, 409), with the benchmark view partition and
     the "skipped" reason;
   - the context probe, Load, the VRAM probe and the swap sibling;
   - the portal mapping DTO marker `decisions_only`.
3. **The capability `decision`** (`routing.CapabilityDecision`): "the model answers System One questions". Rules:
   - The handler sets `RequiredCapabilities = ["decision"]`. `filterCapable` refuses a mapping without a `yes` row on
     every resolve branch (404 `routing.model_not_capable`).
   - The capability list is also what keeps the request off the vendor-account path (`vendorAccountRoutingEligible`
     declines capability-carrying requests). A test pins it, as for speech.
   - `(decision, no)` joins `reservedManualVerdicts`, so a manual `no` cannot veto the probe that writes it.
   - `decision` joins `reservedAgentCapabilityNames`, since only the gateway's probe writes it.
   - The portal capability fold (`hasCapabilityFold`/`capabilityFlagsByName`) learns `decision`, so the unknown-model
     redirect and the listing can see it.
4. **The verdict probe** (new capability source `systemone_probe`, rank 1 through the default branch). It runs as a
   gateway health-loop pass, beside the sd.cpp and audio.cpp passes, for every mapping whose **effective** flavors list
   `decisions`:
   - **External application** (any type but `server_agent`): `POST {ApplicationEndpoint}/v1/systemone` with the
     application's upstream auth.
   - **`server_agent` mapping:** the same request through the agent router, **only while the model is loaded**
     (`LoadedAppModels`), with per-spec auth (`SpecUpstreamAuth`). The probe never cold-starts a model.
   - **Request:** `{"model": <app_model_name>, "state": "ok", "questions": {"probe": {"type": "noul", "instructions": "Is the state non-empty?"}}}`.
   - **Result:**
     - 2xx with a JSON object whose `answers.probe` is an object: `decision: yes`.
     - HTTP 404 or 501: `decision: no` (no route, or a model the server cannot read).
     - Anything else (timeouts, 5xx other than 501, 400, unparseable 2xx): no row, retry on the next pass.
   - **Settled memory:**
     - an external mapping is probed once per (mapping id, app model name);
     - a `server_agent` mapping once per (mapping id, `agentSpecIdentity`);
     - a transient outcome backs off (`mappingBackoff`);
     - a gateway restart re-probes once.
   - **Unmetered:** no usage row, no capture.
5. **`POST /v1/systemone`.** Bearer-only (`requireAnyScope`, as `/v1/responses`), plus `runAsPrincipal`. The flow
   follows `handleOpenAISpeech`:
   - `requireMethod`;
   - auth;
   - `liftInferenceDeadlines`;
   - `readRawJSONLimit` with the new cap;
   - `sniffRoutingModel`;
   - the validator `validateSystemOneRequest`;
   - `runAsPrincipal`;
   - `inferencePreflight` with `systemOneInferenceShape()`;
   - `relaySystemOne`, which resolves, applies the `targetServesFlavor` conjunct (404 `routing.no_model_route` for a
     `server_agent` child whose spec excludes `decisions`), then calls `proxyNative` to `upstreamPath(...)` =
     `/v1/systemone`.

   The validator owns only what the gateway needs; everything else reaches the upstream unexamined:
   - `systemone.model_required` (400): `model` absent, not a string, or blank;
   - `systemone.questions_required` (400): `questions` absent, not a JSON object, or `{}`;
   - `systemone.stream_unsupported` (400): `stream` present and not `false`. System One defines no streaming; this
     refuses a value nobody sends today rather than relaying it.

   Body and response handling:
   - **Body cap:** `OP_AI_GATEWAY_MAX_DECISIONS_BODY_BYTES`, default 32 MiB (= the agent router's buffer). Over the cap
     is 413 `request.body_too_large`. The value is wired like `OP_AI_GATEWAY_MAX_AUDIO_TRANSCRIPTION_BODY_BYTES`.
   - **Model rewrite:** `model` is rewritten to the mapping's `app_model_name` (the existing JSON rewrite in
     `proxyNative`).
   - **Usage:** token-metered (`billing_unit = ""`). `mergePassthroughUsage` gains a `decisions` case reading
     `usage.input_tokens`/`output_tokens` (the Responses shape helper), so the row carries real token counts.
   - **Upstream non-2xx:** relayed verbatim through the generic copier (error code `upstream.<status>`). The System One
     error shape differs per implementation, so the gateway does not normalise it in passthrough.
6. **`GET /v1/systemone/models`.** Bearer-only. It lists the model names the caller may use whose effective flavors
   include `decisions` and whose `decision` verdict is `yes`, as
   `{"object":"list","data":[{"id":<name>,"object":"model","api":"systemone"}]}`. The array is never `null`.
7. **Portal:**
   - the application and runtime-spec flavor controls offer `decisions`, opt-in and unticked by default;
   - `runtimeSpecTemplate`'s `optInFlavors` includes it;
   - mapping DTO `decisions_only`;
   - benchmark refusal and skipped handling (`benchmark.decisions_only`);
   - a manual `decision` verdict select (the reserved `no` is shown only when already stored, as for speech);
   - a `decision` capability chip;
   - i18n in German and English.
8. **Docs** (durable, in `docs/architecture/`):
   - compatibility-and-inference: §2 diagram and §3 table, a new §3.9 "System One decisions", and §13 error rows;
   - api-surface: §1 table, the flavor vocabulary and the mapping DTO;
   - routing-and-model-selection: the flavor, the capability and the decisions-only rule;
   - config-env: the new variable;
   - openapi.yaml: both paths;
   - a new ADR-097;
   - glossary: "System One";
   - AGENTS.md and 02-constraints: the bearer-only list.

## Deviations from issue #373 (Phase 1)

- **No `systemone_mode` column in Phase 1.** With one coarse flavor, "may this application serve decisions" is the
  flavor. "May this model serve System One" is the `decision` verdict. The only other value of a System One mode would
  be `disabled`, which unticking the flavor already gives. The per-application/per-spec mode columns (and their
  migration) arrive in Phase 2 together with `decisions_mode`, which genuinely needs four values. Phase 2 adds
  `systemone_mode` alongside if a use for a separate switch appears.
- **One uniform probe instead of reading llama.cpp's `output_modalities`.** A tiny `POST /v1/systemone` answers the
  real question ("does this model answer System One?") for llama.cpp, vLLM and vllm-sr alike. It needs no second
  `/v1/models` parser, no new agent capability source, no agent wire change and no agent version bump.

## Out of scope (Phase 1)

- `/v1/decisions` and translation (Phase 2).
- vllm-sr types (Phase 3).
- Reverse translation and vendor accounts (Phase 4).
- `confidence` normalization (Phase 2).
- A `decision` model role for co-residency (a decision model on llama.cpp/vLLM still derives `chat`; follow-up if
  eviction grouping needs it).
- A portal playground.

## Acceptance (Phase 1)

- **Flavor:**
  - `decisions` is accepted on applications and specs, never defaulted;
  - a decisions-only mapping is absent from all three chat listings, refused by the translate dispatch, and never sent a
    warmer, benchmark, context-probe, Load, VRAM-probe or swap chat prompt (tests mirror the audio-only tests);
  - `score_servers` ranks it.
- **Gate:** a mapping without `decision: yes` gets 404 `routing.model_not_capable`, including under the affinity pin and
  the server override. A request is never routed to a vendor account.
- **Relay:**
  - the raw body reaches `/v1/systemone` with `model` rewritten;
  - the raw 2xx body and status reach the client;
  - one token-metered usage row records the upstream's `input_tokens`/`output_tokens`;
  - a non-2xx upstream gives an error row and a verbatim body;
  - the cap answers 413, and the three validator codes answer 400;
  - a `server_agent` child whose spec excludes `decisions` answers 404 `routing.no_model_route`.
- **Probe:** it writes `yes`/`no`/nothing per the table above, external and agent-launched. It never probes an unloaded
  agent model, a mapping without the flavor, or the same identity twice. It writes nothing that outranks a manual
  `yes`.
- **Listing:** `GET /v1/systemone/models` lists exactly the callable decision models.
- **Live check:**
  - `vllm-sr/d3-mini` (or `d3-nano`) on vLLM ≥ 0.33.0 under the `vllm` spec, through the gateway: text, JSON and image
    `state`;
  - a llama.cpp decision model if one is at hand;
  - vLLM's OpenAI `/v1/decisions` with d3 confirmed 501 (input for Phase 2).

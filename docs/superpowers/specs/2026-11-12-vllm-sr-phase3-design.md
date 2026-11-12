# Decisions Phase 3 — vLLM Semantic Router as an application type and a runtime spec type (#373)

Branch-local design. Authority: issue #373 ("vllm-sr as an application type", "as a runtime spec type", Phase 3 acceptance criteria, owner decisions 9 and 10), ADR-097/098 as shipped. Contract facts: `2026-11-12-vllm-sr-contract.md` (semantic-router @ `4ec449f358ce7805228fb341bdc55ea5211707fd`, 2026-11-12). Touch points: `2026-11-12-vllm-sr-inventory.md`.

## 1. Pinned contract

- Pinned commit: `vllm-project/semantic-router@4ec449f358ce7805228fb341bdc55ea5211707fd` (2026-11-12). Runtime package `vllm-srun` 0.2.0 (not on PyPI; installed from that checkout, `pip install ./src/model-runtime`, or used from the router images). Runtime API `api_version` **5.0.0**; the supported major is **5**.
- The contract moved 3 → 4 → 5 between 2026-11-09 and 2026-11-11; Router mode refuses another major. We warn, never refuse (owner decision 9).

## 2. Deviations from the issue (forced by the contract)

1. **Engine mode health:** `/health` lives on the management port (127.0.0.1:8080 inside the container), not on the 8899 listener. The `vllm_sr` application type's default health path is therefore `GET /v1/systemone/models` on the listener (authenticated with the application's API key like discovery). The issue said `/health`.
2. **Worker discovery:** the worker has no `GET /v1/systemone/models` (that is a listener path). A runtime spec needs no discovery (the spec names its model), so nothing changes for specs; only the application type uses `/v1/systemone/models`.
3. **Metrics:** the worker exposes only a waiting gauge, `vllm_srun_queue_depth`; there is no in-flight gauge. The agent maps it to the waiting count and reports no running count.
4. **No 501:** an unsupported question type is 422 (`question_failed` on `/v1/systemone`), never 501. Nothing in the gateway depends on a 501 from vllm-sr: the System One probe asks a `noul` question, which every d3 size answers; a model not published on an Engine listener answers 404 `systemone_not_published`, which the probe records as `no`, as for any 404.
5. **5.0 error envelope:** `/v1/systemone` errors carry `detail` (string, or a list on 422) plus flat `code`/`question`/`reason` fields and no `error` key; overload is HTTP 529. The Phase 2 translate error normalizer already reads `detail` (string and `[{msg}]` list); a passthrough relays it verbatim. A test pins a 5.0-shaped 422 and a 529 through translate.
6. **Engine listener deadline:** the listener applies a 30 s default request deadline (504 `systemone_deadline_exceeded`) unless the client sends `options.deadline_ms`; documented as an operator note, not changed by the gateway.

## 3. Application type `vllm_sr` (operator-run Engine mode)

- `routing.ProviderVllmSR = "vllm_sr"`; accepted by `normalizeApplicationType`; served by the shared OpenAI-compatible client (`providerClients`); `viewRuntimeType` maps it to `RuntimeSpecTypeVllmSR`.
- Model discovery: `modelDiscoveryFor("vllm_sr", prefix)` → `/v1/systemone/models`, decoded with the existing OpenAI list decoder (`data[].id`).
- Backend timeout default 600000 ms (like audio_cpp).
- Frontend type defaults: port 8899, flavors `["decisions"]`, `responses_mode`/`messages_mode` `disabled`, `decisions_mode` `auto` (passthrough not offered), `systemone_mode` `passthrough`, health path `/v1/systemone/models`, loaded-models path empty, timeout 600000.
- `decisions_mode: passthrough` refused for `vllm_sr` applications and specs: `decisionsPassthroughCapableKind` excludes it (its `/v1/decisions` is the System One alias). `auto` therefore always resolves to translate (no `decision_openai` probe is ever sent: `openAIDecisionsInScope` admits only `vllm` apps and `vllm`/`custom` specs).
- A type note in the portal: Engine mode only, Router mode is not supported; models must be granted on the listener (`listeners[].apis.systemone.models`); set the listener `api_keys` to the application's API key; the 30 s default deadline.

## 4. Runtime spec type `vllm_sr` (agent-launched worker `vllm-srun`)

- `routing.RuntimeSpecTypeVllmSR RuntimeSpecType = "vllm_sr"`; accepted by `validRuntimeSpecType` (error text lists it).
- Detection: `DetectRuntimeSpecType` matches `vllm-srun`, `vllm_srun` and `vllm-sr` **before** the `vllm` case; a test pins that `vllm-srun` (and a path ending in it) detects as `vllm_sr` and plain `vllm` still as `vllm`.
- `DeriveProbePaths(vllm_sr)` → metrics `/metrics`, context `/v1/models`. Health path default stays `/health` (503 until the golden check passed; the agent's readiness poll already waits for 2xx).
- Frontend spec defaults: flavors `["decisions"]`, both coding-agent modes `disabled`, `decisions_mode` `auto` (passthrough not offered), a type note with the launch template `serve ${MODEL} --device cuda:0 --host 127.0.0.1 --port ${PORT}` (device 0 because `set_visible_devices` remaps the GPU), the install hint (checkout of the pinned commit, `runtime_allow_binary_dirs` for its venv), the first-start download note (raise the startup timeout), and that the worker has no authentication and must stay on loopback.
- A RuntimeWarning `api_flavors_text_on_vllm_sr` when a `vllm_sr` spec lists `openai` or `anthropic` (the precedent `api_flavors_text_on_stable_diffusion`).

## 5. Server agent (PATCH bump 0.18.1 → 0.18.2; no new feature, no gate)

- Context: `extractContext("vllm_sr", v)` reads `data[].limits.max_input_tokens` (first entry that carries it); falls back to the best-effort scan.
- Metrics: `vllm_srun_queue_depth` is added as a waiting metric; no running metric exists for vllm-sr (running stays absent, as for a server without one).
- API version: the context probe's `/v1/models` body carries a top-level `api_version`; for a `vllm_sr` child the agent records it in a new additive runtime-sample field `runtime_api_version` (string, omitted when unknown). No warning logic in the agent: the gateway decides.
- Capability probe: a `vllm_sr` child is skipped (no `/props` request) — it has no props endpoint.
- Mixed fleet: an older agent ignores the type string (best-effort context, no queue gauge, no api_version) — harmless; the gateway treats an absent `runtime_api_version` as unknown and warns nothing.

## 6. Gateway: API version warning

- `routing.VllmSRSupportedAPIMajor = 5` (one constant, with the pinned commit in its doc comment).
- Agent ingest stores `runtime_api_version` in the runtime registry; the portal model-servers row carries `runtime_api_version` and `runtime_api_version_unsupported` (true when the type is `vllm_sr` and the major parsed from it differs from 5; false when unknown). The gateway logs one `slog.Warn` per (server, model, version) change into an unsupported major.
- The portal shows a warning chip with the reported version on that row. Nothing is refused (owner decision 9).
- External `vllm_sr` applications (Engine mode) report no api_version (the listener does not expose one): no warning there; documented.

## 7. Acceptance (from #373 Phase 3) and what this branch can prove

- Unit/handler tests: type accepted (API + portal form), defaults, discovery path, passthrough refused (400/409, spec 400), detection order pinned, probe paths, context extractor, queue gauge, api_version recorded and warned, warning chip, RuntimeWarning.
- Not provable here (handed to the owner, GPU host): an Engine-mode instance registered as `vllm_sr` (discovery, health, `/v1/systemone` passthrough, `/v1/decisions` translate); `vllm-srun serve vllm-sr/d3-mini` launched by the agent and reached through both endpoints; the warning with a worker of another major; the time-to-first-answer and answers/s comparison of the same d3 checkpoint under the `vllm` and `vllm_sr` spec types.

## 8. Decisions to record in ADR-099

1. Engine-mode health on `/v1/systemone/models` (management port unreachable).
2. Agent-launched worker, not the Docker-orchestrating `vllm-sr serve` CLI (the agent supervises one child process).
3. api_version read from the context probe's `/v1/models` (no extra request), warned by the gateway, never refused.
4. Waiting gauge only; no running count.
5. Passthrough refused (its `/v1/decisions` is the System One alias); `auto` = translate.
6. No migration; the agent change is additive (PATCH).
